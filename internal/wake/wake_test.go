package wake

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"billgo/internal/config"
)

// fakes 在 PATH 前面放假的 ssh / docker，记录参数并返回指定退出码。
type fakes struct {
	dir string
}

func newFakes(t *testing.T, sshCode, dockerCode int) fakes {
	t.Helper()
	dir := t.TempDir()
	writeFake(t, dir, "ssh", sshCode)
	writeFake(t, dir, "docker", dockerCode)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return fakes{dir: dir}
}

func writeFake(t *testing.T, dir, name string, code int) {
	t.Helper()
	argsFile := filepath.Join(dir, name+".args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (f fakes) args(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, name+".args"))
	if err != nil {
		t.Fatalf("假 %s 没有被调用: %v", name, err)
	}
	return string(raw)
}

func TestSSHInvocation(t *testing.T) {
	f := newFakes(t, 0, 0)
	err := Do(context.Background(), &config.Wake{Mode: "ssh", Target: "u@host", Container: "peach"})
	if err != nil {
		t.Fatalf("唤醒应成功: %v", err)
	}
	args := f.args(t, "ssh")
	if !strings.Contains(args, "u@host") {
		t.Fatalf("ssh 参数里应有目标: %q", args)
	}
	if !strings.Contains(args, "BatchMode=yes") || !strings.Contains(args, "StrictHostKeyChecking=yes") {
		t.Fatalf("ssh 参数应开启批模式和主机校验: %q", args)
	}
}

func TestLocalInvocation(t *testing.T) {
	f := newFakes(t, 0, 0)
	err := Do(context.Background(), &config.Wake{Mode: "local", Container: "peach"})
	if err != nil {
		t.Fatalf("本地唤醒应成功: %v", err)
	}
	args := f.args(t, "docker")
	if !strings.Contains(args, "start") || !strings.Contains(args, "peach") {
		t.Fatalf("应执行 docker start peach，实际参数 %q", args)
	}
}

func TestLocalRejectsBadContainerName(t *testing.T) {
	newFakes(t, 0, 0)
	for _, bad := range []string{"", "peach; rm -rf /", "../peach", "peach --force"} {
		err := Local(context.Background(), &config.Wake{Mode: "local", Container: bad})
		if err == nil {
			t.Fatalf("容器名 %q 应被拒绝", bad)
		}
	}
}

func TestDoDispatch(t *testing.T) {
	f := newFakes(t, 0, 0)
	if err := Do(context.Background(), &config.Wake{Mode: "ssh", Target: "u@h"}); err != nil {
		t.Fatal(err)
	}
	if err := Do(context.Background(), &config.Wake{Mode: "local", Container: "peach"}); err != nil {
		t.Fatal(err)
	}
	if got := f.args(t, "ssh"); !strings.Contains(got, "u@h") {
		t.Fatalf("ssh 未被调用: %q", got)
	}
	if got := f.args(t, "docker"); !strings.Contains(got, "peach") {
		t.Fatalf("docker 未被调用: %q", got)
	}
}

func TestSSHFailure(t *testing.T) {
	newFakes(t, 1, 0)
	err := SSH(context.Background(), &config.Wake{Mode: "ssh", Target: "u@host"})
	if err == nil {
		t.Fatal("退出码非 0 应报错")
	}
	if !strings.Contains(err.Error(), "u@host") {
		t.Fatalf("错误里应带目标: %v", err)
	}
}

func TestLocalFailure(t *testing.T) {
	newFakes(t, 0, 1)
	err := Local(context.Background(), &config.Wake{Mode: "local", Container: "peach"})
	if err == nil {
		t.Fatal("docker start 失败应报错")
	}
	if !strings.Contains(err.Error(), "peach") {
		t.Fatalf("错误里应带容器名: %v", err)
	}
}

func TestNoWakeInfo(t *testing.T) {
	if err := Do(context.Background(), nil); err == nil {
		t.Fatal("缺唤醒信息应报错")
	}
	if err := Do(context.Background(), &config.Wake{Mode: "magic"}); err == nil {
		t.Fatal("不支持的方式应报错")
	}
}
