package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMissingReturnsDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("缺文件应返回默认值: %v", err)
	}
	if c.Kind != KindResident || c.Network != "default" || c.DataDir != "data" {
		t.Fatalf("默认值不对: %+v", c)
	}
}

func TestLoadOverrides(t *testing.T) {
	p := write(t, `{
		"name": "peach",
		"desc": "写文档",
		"kind": "wakeable",
		"listen": "10.0.0.2:9000",
		"data_dir": "/tmp/peach",
		"wake": {"mode": "ssh", "target": "u@10.0.0.2", "container": "peach", "host_fp": "SHA256:x"},
		"bootstrap": ["https://10.0.0.1:8787#aaaaaaaaaaaaaaaa"]
	}`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "peach" || c.Kind != KindWakeable || c.Listen != "10.0.0.2:9000" {
		t.Fatalf("字段没覆盖上: %+v", c)
	}
	if len(c.Bootstrap) != 1 {
		t.Fatalf("bootstrap 没读进来: %+v", c)
	}
}

func TestAccessPolicy(t *testing.T) {
	p := write(t, `{
		"name": "a",
		"access": {
			"default": "allow",
			"rules": [{"peer": "*", "deny": ["config.write"]}]
		}
	}`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Access == nil || !c.Access.Decide("peer", "config.read") || c.Access.Decide("peer", "config.write") {
		t.Fatalf("access 判定不对: %+v", c.Access)
	}
	bad := write(t, `{"name":"a","access":{"default":"open"}}`)
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "default") {
		t.Fatalf("非法 access 应报错，得到 %v", err)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	p := write(t, `{"name":"a","knd":"resident"}`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "knd") {
		t.Fatalf("拼错的字段应该报错，得到 %v", err)
	}
}

func TestInvalidKind(t *testing.T) {
	p := write(t, `{"name":"a","kind":"sometimes"}`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("非法 kind 应该报错，得到 %v", err)
	}
}

func TestLocalWakeNeedsOnlyContainer(t *testing.T) {
	p := write(t, `{"name":"a","kind":"wakeable","wake":{"mode":"local","container":"peach"}}`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("local 唤醒不需要 target: %v", err)
	}
	if c.Wake.Mode != "local" || c.Wake.Container != "peach" {
		t.Fatalf("wake 没读对: %+v", c.Wake)
	}
}

func TestSSHWakeNeedsTarget(t *testing.T) {
	p := write(t, `{"name":"a","kind":"wakeable","wake":{"mode":"ssh","container":"peach"}}`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "target") {
		t.Fatalf("ssh 唤醒缺 target 应报错，得到 %v", err)
	}
}

func TestUnknownFieldUIRejected(t *testing.T) {
	p := write(t, `{"name":"a","ui":{"listen":"127.0.0.1:7878"}}`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "ui") {
		t.Fatalf("节点配置不再接受 ui，得到 %v", err)
	}
}

func TestUnknownWakeMode(t *testing.T) {
	p := write(t, `{"name":"a","kind":"wakeable","wake":{"mode":"magic","container":"peach"}}`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "wake.mode") {
		t.Fatalf("未知唤醒方式应报错，得到 %v", err)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	p := write(t, `{"name":"from-file","bootstrap":["https://old:1#aaaaaaaaaaaaaaaa"]}`)
	t.Setenv("BILLGO_NAME", "from-env")
	t.Setenv("BILLGO_KIND", "resident")
	t.Setenv("BILLGO_NETWORK", "envnet")
	t.Setenv("BILLGO_ENDPOINT", "https://env:8787")
	t.Setenv("BILLGO_BOOTSTRAP", "https://a:1#aaaaaaaaaaaaaaaa, https://b:2#bbbbbbbbbbbbbbbb")
	t.Setenv("BILLGO_ALLOW_FROM", "aaaaaaaaaaaaaaaa, bbbbbbbbbbbbbbbb")

	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "from-env" || c.Network != "envnet" || c.Endpoint != "https://env:8787" {
		t.Fatalf("字符串覆盖失败: %+v", c)
	}
	if len(c.Bootstrap) != 2 || c.Bootstrap[0] != "https://a:1#aaaaaaaaaaaaaaaa" {
		t.Fatalf("bootstrap 覆盖失败: %+v", c.Bootstrap)
	}
	if len(c.AllowFrom) != 2 || c.AllowFrom[1] != "bbbbbbbbbbbbbbbb" {
		t.Fatalf("allow_from 覆盖失败: %+v", c.AllowFrom)
	}
}

func TestEnvWithoutFile(t *testing.T) {
	t.Setenv("BILLGO_NAME", "envonly")
	t.Setenv("BILLGO_BOOTSTRAP", "https://a:1#aaaaaaaaaaaaaaaa")

	c, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "envonly" || len(c.Bootstrap) != 1 {
		t.Fatalf("无文件时环境变量应生效: %+v", c)
	}
}

func TestEnvListTrimsEmpty(t *testing.T) {
	p := write(t, `{"name":"a"}`)
	t.Setenv("BILLGO_ALLOW_FROM", " a , , b ")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AllowFrom) != 2 || c.AllowFrom[0] != "a" || c.AllowFrom[1] != "b" {
		t.Fatalf("列表解析不对: %+v", c.AllowFrom)
	}
}

func TestEnvInvalidBootstrap(t *testing.T) {
	p := write(t, `{"name":"a"}`)
	t.Setenv("BILLGO_BOOTSTRAP", "not-a-url")
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "bootstrap") {
		t.Fatalf("非法 bootstrap 应报错，得到 %v", err)
	}
}
