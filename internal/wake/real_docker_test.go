//go:build integration

package wake

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"billgo/internal/config"
)

// TestRealLocalDocker 需要本机有 docker 守护进程，默认不跑：
//
//	go test -tags integration ./internal/wake -run TestRealLocalDocker
func TestRealLocalDocker(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("没有 docker")
	}
	name := fmt.Sprintf("bg-wake-test-%d", time.Now().UnixNano())
	if out, err := exec.Command("docker", "create", "--name", name, "alpine:3.20", "sleep", "60").CombinedOutput(); err != nil {
		t.Skipf("建测试容器失败: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	if err := Local(context.Background(), &config.Wake{Mode: "local", Container: name}); err != nil {
		t.Fatalf("docker start 失败: %v", err)
	}
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", name).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "true" {
		t.Fatalf("容器没有启动: %s", out)
	}
}
