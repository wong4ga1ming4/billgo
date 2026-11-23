package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAndRotate(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l.SetLimits(80, 2)
	for i := 0; i < 6; i++ {
		if err := l.Write(Record{
			TS: int64(i), From: "peer", Action: "tickets.list", Target: "self", Result: "allow", EnvelopeID: "id",
		}); err != nil {
			t.Fatal(err)
		}
	}
	cur, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cur), `"action":"tickets.list"`) {
		t.Fatalf("当前日志缺少记录: %s", cur)
	}
	if _, err := os.Stat(l.Path() + ".1"); err != nil {
		t.Fatalf("应轮转出 .1: %v", err)
	}
	entries, _ := filepath.Glob(l.Path() + ".*")
	if len(entries) > 2 {
		t.Fatalf("最多保留 2 个旧文件，得到 %v", entries)
	}
	info, err := os.Stat(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("审计文件权限应为 0600，得到 %o", perm)
	}
}

func TestListNewestAndCursor(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := l.Write(Record{
			TS: int64(i + 1), From: "peer", Action: "tickets.list", Target: "self", Result: "allow",
			EnvelopeID: string(rune('a' + i)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	page, next, err := l.List(2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].EnvelopeID != "e" || page[1].EnvelopeID != "d" {
		t.Fatalf("第一页应是最新两条: %+v", page)
	}
	if next == "" {
		t.Fatal("应给出下一页游标")
	}
	rest, next2, err := l.List(2, next)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 2 || rest[0].EnvelopeID != "c" || rest[1].EnvelopeID != "b" || next2 == "" {
		t.Fatalf("第二页不对: %+v next=%q", rest, next2)
	}
	if _, _, err := l.List(2, "!!!"); err == nil {
		t.Fatal("坏游标应报错")
	}
}
