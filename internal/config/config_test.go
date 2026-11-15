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

func TestUnknownWakeMode(t *testing.T) {
	p := write(t, `{"name":"a","kind":"wakeable","wake":{"mode":"magic","container":"peach"}}`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "wake.mode") {
		t.Fatalf("未知唤醒方式应报错，得到 %v", err)
	}
}
