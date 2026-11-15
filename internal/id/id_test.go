package id

import (
	"strings"
	"testing"
)

func TestNewFormat(t *testing.T) {
	a := New()
	parts := strings.Split(a, "-")
	if len(parts) != 5 {
		t.Fatalf("不是 UUID 格式: %s", a)
	}
	if len(a) != 36 {
		t.Fatalf("长度 %d: %s", len(a), a)
	}
	if a[14] != '7' {
		t.Fatalf("第 15 位应是版本号 7: %s", a)
	}
}

func TestUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		v := New()
		if seen[v] {
			t.Fatalf("重复: %s", v)
		}
		seen[v] = true
	}
}
