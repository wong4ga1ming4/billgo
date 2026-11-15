// Package control 是节点与本机前端（CLI/MCP）之间的接口：Unix socket 上的 HTTP。
//
// 控制面只对本机开放，权限 0600，不暴露到网络。网络端口只认签名信封。
package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// SocketPath 返回数据目录下的控制 socket 路径。
func SocketPath(dataDir string) string {
	return filepath.Join(dataDir, "control.sock")
}

// Server 本地控制面。
type Server struct {
	ln  net.Listener
	srv *http.Server
}

// Serve 在 dataDir/control.sock 上提供服务，ctx 结束时关闭。
func Serve(ctx context.Context, dataDir string, h http.Handler) (*Server, error) {
	path := SocketPath(dataDir)
	// Unix socket 路径在 macOS 上约 104 字节上限，超了会报一个很难懂的错误。
	if len(path) > 100 {
		return nil, fmt.Errorf("控制 socket 路径过长（%d 字节，上限约 104）: %s", len(path), path)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("建数据目录: %w", err)
	}
	_ = os.Remove(path) // 上次异常退出留下的 socket
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("监听 %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("设置 socket 权限: %w", err)
	}
	s := &Server{ln: ln, srv: &http.Server{Handler: h}}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// 控制面退出不影响节点本体，记录即可。
			fmt.Fprintf(os.Stderr, "control server: %v\n", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
		_ = os.Remove(path)
	}()
	return s, nil
}
