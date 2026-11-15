// Package config 读取 billgo 节点的 JSON 配置。
//
// 配置项尽量少：v1 只有节点自述、监听地址、数据目录和少量对接参数。
// 找不到配置文件时用默认值，写错了（未知字段、非法 kind）直接报错，
// 不静默忽略——新工具里拼错的字段名比缺省值更可怕。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"strings"
)

// Kind 节点的两种形态。
const (
	KindResident = "resident"
	KindWakeable = "wakeable"
)

// Wake 唤醒信息，写在 peer 记录里。
//
//   - mode=ssh：ssh 到 target，由 host 的 authorized_keys 强制命令启动容器。
//     权限边界在 sshd，billgod 不需要 docker 权限。
//   - mode=local：直接执行 docker start container。
//     适合 billgod 跑在宿主机上，或容器里挂载了 docker socket 的部署。
//     后者的实际权限等于宿主机 root，配置者需自行接受。
type Wake struct {
	Mode      string `json:"mode"`      // ssh | local
	Target    string `json:"target"`    // 仅 ssh：user@host
	Container string `json:"container"` // 容器名；local 下会校验字符集
	HostFP    string `json:"host_fp"`   // 仅 ssh：SSH 主机指纹，连的时候固定
}

// Config 一个节点的全部运行配置。
type Config struct {
	Name      string   `json:"name"`       // 别名，只作展示，不唯一
	Desc      string   `json:"desc"`       // 自由描述，发起者按它选节点
	Kind      string   `json:"kind"`       // resident | wakeable
	Network   string   `json:"network"`    // 域标识，默认 default
	Listen    string   `json:"listen"`     // HTTP 监听地址
	Endpoint  string   `json:"endpoint"`   // 对外地址，如 https://10.0.0.2:8787
	DataDir   string   `json:"data_dir"`   // 数据目录（bbolt + blobs + 私钥）
	Bootstrap []string `json:"bootstrap"`  // 启动时拉名录的已知节点（https://h:p#peer_id）
	AllowFrom []string `json:"allow_from"` // 允许派单给我的 peer_id；空 = 全拒
	Wake      *Wake    `json:"wake,omitempty"`
	AgentCmd  []string `json:"agent_cmd"` // 接活时执行的命令，空 = 只接受不执行

	AcceptTimeoutMin  int `json:"accept_timeout_min"`  // 派单后多久未受理算超时
	DeliverTimeoutMin int `json:"deliver_timeout_min"` // 受理后多久未交付算超时
	JobTimeoutMin     int `json:"job_timeout_min"`     // Agent 进程最长运行时间
	AntiEntropySec    int `json:"anti_entropy_sec"`    // 反熵周期
}

// Default 返回默认配置。
func Default() *Config {
	return &Config{
		Name:              "agent",
		Kind:              KindResident,
		Network:           "default",
		Listen:            "0.0.0.0:8787",
		DataDir:           "data",
		AcceptTimeoutMin:  30,
		DeliverTimeoutMin: 120,
		JobTimeoutMin:     60,
		AntiEntropySec:    60,
	}
}

// Load 读配置文件；文件不存在时返回默认值。
func Load(path string) (*Config, error) {
	c := Default()
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("打开配置 %s: %w", path, err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	c.fillDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("配置 %s: %w", path, err)
	}
	return c, nil
}

// fillDefaults 补齐文件里省略的可选项。
func (c *Config) fillDefaults() {
	d := Default()
	if c.Kind == "" {
		c.Kind = d.Kind
	}
	if c.Network == "" {
		c.Network = d.Network
	}
	if c.Listen == "" {
		c.Listen = d.Listen
	}
	if c.DataDir == "" {
		c.DataDir = d.DataDir
	}
	if c.AcceptTimeoutMin <= 0 {
		c.AcceptTimeoutMin = d.AcceptTimeoutMin
	}
	if c.DeliverTimeoutMin <= 0 {
		c.DeliverTimeoutMin = d.DeliverTimeoutMin
	}
	if c.JobTimeoutMin <= 0 {
		c.JobTimeoutMin = d.JobTimeoutMin
	}
	if c.AntiEntropySec <= 0 {
		c.AntiEntropySec = d.AntiEntropySec
	}
}

// Bootstrap 一个引导节点：地址 + 预期 peer_id。指纹必须写进配置，
// 否则第一次连接就没有可核对的身份。
type Bootstrap struct {
	Endpoint string
	PeerID   string
}

// ParseBootstrap 解析 "https://host:port#peer_id"。
func ParseBootstrap(s string) (Bootstrap, error) {
	u, err := url.Parse(s)
	if err != nil {
		return Bootstrap{}, fmt.Errorf("bootstrap %q: %w", s, err)
	}
	if u.Scheme != "https" || u.Host == "" {
		return Bootstrap{}, fmt.Errorf("bootstrap %q: 需要 https://host:port#peer_id", s)
	}
	pid := u.Fragment
	if len(pid) != 16 {
		return Bootstrap{}, fmt.Errorf("bootstrap %q: 片段必须是 16 位 peer_id", s)
	}
	u.Fragment = ""
	return Bootstrap{Endpoint: strings.TrimRight(u.String(), "/"), PeerID: strings.ToLower(pid)}, nil
}

// ParsedBootstrap 解析配置里的全部引导节点。
func (c *Config) ParsedBootstrap() ([]Bootstrap, error) {
	out := make([]Bootstrap, 0, len(c.Bootstrap))
	for _, s := range c.Bootstrap {
		b, err := ParseBootstrap(s)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// AdvertisedEndpoint 返回别人连我用的地址。
func (c *Config) AdvertisedEndpoint() string {
	if c.Endpoint != "" {
		return strings.TrimRight(c.Endpoint, "/")
	}
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return "https://" + c.Listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "https://" + net.JoinHostPort(host, port)
}

// Validate 校验必填项和枚举值。
func (c *Config) Validate() error {
	if c.Name == "" {
		return errors.New("name 不能为空")
	}
	switch c.Kind {
	case KindResident, KindWakeable:
	default:
		return fmt.Errorf("kind 必须是 %q 或 %q，得到 %q", KindResident, KindWakeable, c.Kind)
	}
	if c.Network == "" {
		return errors.New("network 不能为空")
	}
	if c.Listen == "" {
		return errors.New("listen 不能为空")
	}
	if c.DataDir == "" {
		return errors.New("data_dir 不能为空")
	}
	if c.Kind == KindWakeable && c.Wake == nil {
		return errors.New("wakeable 节点必须配置 wake（mode/target/container/host_fp）")
	}
	if c.Wake != nil {
		switch c.Wake.Mode {
		case "ssh":
			if c.Wake.Target == "" || c.Wake.Container == "" {
				return errors.New("ssh 唤醒需要 wake.target 和 wake.container")
			}
		case "local":
			if c.Wake.Container == "" {
				return errors.New("local 唤醒需要 wake.container")
			}
		default:
			return fmt.Errorf("wake.mode 只支持 ssh 或 local，得到 %q", c.Wake.Mode)
		}
	}
	if _, err := c.ParsedBootstrap(); err != nil {
		return err
	}
	return nil
}
