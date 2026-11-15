// Package wake 叫醒一个休眠容器。两种方式：
//
//   - ssh：连到 host，由 authorized_keys 的强制命令执行 docker start。
//     工具不管理凭证；权限边界在 sshd，billgod 不需要 docker 权限。
//   - local：直接执行 docker start。适合 billgod 跑在宿主机上，
//     或容器里挂载了 docker socket 的部署（后者等价于宿主机 root 权限）。
package wake

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"billgo/internal/config"
)

// ErrNoWake 该记录没有唤醒信息。
var ErrNoWake = errors.New("记录里没有唤醒信息")

// containerRE 容器名只允许常见字符，防止被当作额外参数注入。
var containerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Do 按记录里的方式唤醒。
func Do(ctx context.Context, w *config.Wake) error {
	if w == nil {
		return ErrNoWake
	}
	switch w.Mode {
	case "ssh":
		return SSH(ctx, w)
	case "local":
		return Local(ctx, w)
	default:
		return fmt.Errorf("不支持的唤醒方式 %q", w.Mode)
	}
}

// SSH 执行一次唤醒。强制命令在对方 sshd 侧执行，这里只负责发起连接。
// HostFP 由使用方写进 known_hosts 后由 ssh 自己核对。
func SSH(ctx context.Context, w *config.Wake) error {
	if w == nil {
		return ErrNoWake
	}
	if w.Mode != "ssh" {
		return fmt.Errorf("不支持的唤醒方式 %q", w.Mode)
	}
	if w.Target == "" {
		return fmt.Errorf("唤醒目标为空")
	}
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "ConnectTimeout=5",
		w.Target,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return withOutput(fmt.Errorf("ssh %s 失败: %w", w.Target, err), out)
	}
	return nil
}

// Local 在本机执行 docker start。
//
// 需要 billgod 能访问 docker（宿主机上安装了 docker CLI，
// 或容器里挂载了 /var/run/docker.sock）。
func Local(ctx context.Context, w *config.Wake) error {
	if w == nil {
		return ErrNoWake
	}
	if w.Mode != "local" {
		return fmt.Errorf("不支持的唤醒方式 %q", w.Mode)
	}
	if !containerRE.MatchString(w.Container) {
		return fmt.Errorf("非法容器名 %q", w.Container)
	}
	// docker start 会等到容器启动完成；冷启动可能较慢，给足时间，
	// 真正的上限由调用方的 context 决定。
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "start", w.Container)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return withOutput(fmt.Errorf("docker start %s 失败: %w", w.Container, err), out)
	}
	return nil
}

func withOutput(err error, out []byte) error {
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return fmt.Errorf("%w: %s", err, msg)
	}
	return err
}
