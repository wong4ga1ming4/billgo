// billgod 是 billgo 节点守护进程。
//
// 启动后它会：加载身份、打开本地库、监听 TLS、从 bootstrap 拉名录、
// 开始反熵、投递队列与工单驱动，并在数据目录开一个仅本机可用的控制 socket。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"billgo/internal/config"
	"billgo/internal/node"
)

// version 由发布脚本通过 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	cfgPath := flag.String("config", "billgo.json", "配置文件路径")
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()
	if *showVersion {
		fmt.Printf("billgod %s\n", version)
		return
	}

	level := slog.LevelInfo
	if os.Getenv("BILLGO_LOG") == "debug" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("读配置失败", "err", err)
		os.Exit(1)
	}
	n, err := node.NewWithOptions(cfg, node.Options{Version: version, ConfigPath: *cfgPath})
	if err != nil {
		slog.Error("装配节点失败", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := n.Run(ctx); err != nil {
		slog.Error("节点退出", "err", err)
		os.Exit(1)
	}
}
