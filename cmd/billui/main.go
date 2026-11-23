// billui 是可选的页面进程。它等本机 billgod 的控制 socket 出现后再监听，
// 连不上就一直重试。billgod 不知道它的存在。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"billgo/internal/billui"
	"billgo/internal/control"
)

// version 由发布脚本通过 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	dataDefault := os.Getenv("BILLGO_DATA")
	if dataDefault == "" {
		dataDefault = "data"
	}
	listenDefault := os.Getenv("BILLUI_LISTEN")
	if listenDefault == "" {
		listenDefault = "127.0.0.1:7878"
	}
	data := flag.String("data", dataDefault, "数据目录，与 billgod 相同")
	listen := flag.String("listen", listenDefault, "页面监听地址")
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()
	if *showVersion {
		fmt.Printf("billui %s\n", version)
		return
	}
	if _, _, err := net.SplitHostPort(*listen); err != nil {
		slog.Error("监听地址需要 host:port", "listen", *listen, "err", err)
		os.Exit(1)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	ui, err := billui.Open(*data)
	if err != nil {
		slog.Error("准备页面失败", "err", err)
		os.Exit(1)
	}
	slog.Info("billui 密码", "password", ui.Password(), "file", ui.PasswordPath())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := waitSocket(ctx, control.SocketPath(*data)); err != nil {
		slog.Error("退出", "err", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           ui.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		slog.Info("billui 已启动", "listen", *listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	select {
	case err := <-errCh:
		slog.Error("页面退出", "err", err)
		os.Exit(1)
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}
}

func waitSocket(ctx context.Context, path string) error {
	slog.Info("等待 billgod", "socket", path)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	logged := false
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if !logged {
			slog.Warn("还没连上 billgod，继续等", "socket", path)
			logged = true
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("billgod 未就绪: %s", path)
		case <-tick.C:
		}
	}
}
