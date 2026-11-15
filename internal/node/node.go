// Package node 把一个节点需要的部件装配起来：身份、存储、传输、名录、
// 投递队列、工单服务、本地控制面。
package node

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"billgo/internal/config"
	"billgo/internal/control"
	"billgo/internal/envelope"
	"billgo/internal/identity"
	"billgo/internal/outbox"
	"billgo/internal/peers"
	"billgo/internal/store"
	"billgo/internal/ticket"
	"billgo/internal/transport"
	"billgo/internal/wake"
	"billgo/internal/webui"
)

// Options 装配参数。
type Options struct {
	Version string
}

// Node 一个运行中的 Agent 节点。
type Node struct {
	cfg     *config.Config
	version string
	id      *identity.Identity
	st      *store.Store
	client  *transport.Client
	srv     *transport.Server
	peers   *peers.Service
	out     *outbox.Queue
	tickets *ticket.Service
	ui      *webui.Server
	ready   atomic.Bool
}

// New 装配节点，但不启动监听。
func New(cfg *config.Config) (*Node, error) {
	return NewWithOptions(cfg, Options{})
}

// NewWithOptions 装配节点，version 用于页面展示。
func NewWithOptions(cfg *config.Config, opts Options) (*Node, error) {
	if opts.Version == "" {
		opts.Version = "dev"
	}
	id, err := identity.LoadOrCreate(filepath.Join(cfg.DataDir, "identity.key"))
	if err != nil {
		return nil, err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(st.BlobDir(), 0o755); err != nil {
		st.Close()
		return nil, fmt.Errorf("建 blobs 目录: %w", err)
	}

	n := &Node{cfg: cfg, version: opts.Version, id: id, st: st}
	n.client = transport.NewClient(id.PeerID)

	srv, err := transport.NewServer(id.PeerID, id.Private, cfg.Listen, n.isReady, n.handle, st.BlobDir())
	if err != nil {
		st.Close()
		return nil, err
	}
	n.srv = srv

	// 监听 :0 时，实际端口只有启动后才知道，用它修正对外地址。
	c := *cfg
	if c.Endpoint == "" {
		c.Endpoint = "https://" + srv.Addr()
	}
	n.cfg = &c

	n.peers, err = peers.New(id, st, n.client, &c)
	if err != nil {
		srv.Close(context.Background())
		st.Close()
		return nil, err
	}
	n.out = outbox.New(st, n.client, n.peers, func(ctx context.Context, w *config.Wake) error {
		return wake.Do(ctx, w)
	}, id.Private)
	n.tickets = ticket.New(id, st, n.peers, n.client, n.out, &c)

	if c.UI != nil && c.UI.Listen != "" {
		info := webui.Info{
			PeerID:    id.PeerID,
			Name:      c.Name,
			Kind:      c.Kind,
			Network:   c.Network,
			Endpoint:  c.Endpoint,
			Version:   opts.Version,
			Listen:    c.UI.Listen,
			Bootstrap: c.Bootstrap,
			AllowFrom: c.AllowFrom,
		}
		if c.Wake != nil {
			info.WakeMode = c.Wake.Mode
		}
		ui, err := webui.New(info, n.peers, n.tickets, n.out, c.UI.Root, st.BlobDir(), c.UI.Listen)
		if err != nil {
			srv.Close(context.Background())
			st.Close()
			return nil, err
		}
		n.ui = ui
	}
	return n, nil
}

// Run 启动节点并阻塞到 ctx 结束。
func (n *Node) Run(ctx context.Context) error {
	n.srv.Start()
	n.ready.Store(true)
	defer n.ready.Store(false)

	if n.ui != nil {
		n.ui.Start()
		slog.Info("页面已启动", "url", "http://"+n.ui.Addr()+"/")
	}

	slog.Info("节点已启动",
		"peer_id", n.id.PeerID,
		"name", n.cfg.Name,
		"kind", n.cfg.Kind,
		"endpoint", n.cfg.Endpoint,
		"listen", n.srv.Addr(),
		"data_dir", n.cfg.DataDir,
	)

	n.peers.Bootstrap(ctx)
	n.peers.Broadcast(ctx)

	go n.peers.Loop(ctx, time.Duration(n.cfg.AntiEntropySec)*time.Second)
	go n.out.Run(ctx)
	go n.tickets.Run(ctx)

	if _, err := control.Serve(ctx, n.cfg.DataDir, n.controlMux()); err != nil {
		return err
	}

	<-ctx.Done()
	slog.Info("收到退出信号，正在关闭")

	// 常驻节点优雅退出时留一条墓碑；唤醒类"睡着"是正常状态，
	// 注销它等于让别人再也找不到、也没法叫醒它。唤醒类要注销请用
	// `billgo withdraw` 显式发起。
	if n.cfg.Kind == config.KindResident {
		wctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = n.peers.Withdraw(wctx)
		cancel()
	}

	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if n.ui != nil {
		_ = n.ui.Close(sctx)
	}
	_ = n.srv.Close(sctx)
	return n.st.Close()
}

func (n *Node) isReady() bool { return n.ready.Load() }

// PeerID 本节点标识。
func (n *Node) PeerID() string { return n.id.PeerID }

// Version 构建版本。
func (n *Node) Version() string { return n.version }

// UIAddr 页面监听地址；未启用时为空。
func (n *Node) UIAddr() string {
	if n.ui == nil {
		return ""
	}
	return n.ui.Addr()
}

// Endpoint 别人连本节点用的地址。
func (n *Node) Endpoint() string { return n.cfg.Endpoint }

// Store 本地库，测试用。
func (n *Node) Store() *store.Store { return n.st }

// Peers 名录服务。
func (n *Node) Peers() *peers.Service { return n.peers }

// Tickets 工单服务。
func (n *Node) Tickets() *ticket.Service { return n.tickets }

// Outbox 投递队列。
func (n *Node) Outbox() *outbox.Queue { return n.out }

// Config 生效配置（Endpoint 已按实际监听地址修正）。
func (n *Node) Config() *config.Config { return n.cfg }

// handle 是所有入站信封的总入口：先查幂等，再分派。
func (n *Node) handle(ctx context.Context, sender string, e *envelope.Envelope) (*envelope.Envelope, error) {
	key, needDedupe, err := e.IdempotencyKey()
	if err != nil {
		return nil, err
	}
	if needDedupe {
		if raw, ok := n.dedupeGet(key); ok {
			return n.reviveCached(raw), nil
		}
	}
	resp, err := n.route(ctx, sender, e)
	if err != nil {
		return nil, err
	}
	if needDedupe {
		n.dedupePut(key, resp)
	}
	return resp, nil
}

func (n *Node) route(ctx context.Context, sender string, e *envelope.Envelope) (*envelope.Envelope, error) {
	switch e.Type {
	case envelope.TypePeersRecord:
		return nil, n.peers.HandleRecord(sender, e)
	case envelope.TypePeersDigest:
		return n.peers.HandleDigest(sender, e)
	case envelope.TypePeersSync:
		return nil, n.peers.HandleSync(sender, e)
	case envelope.TypeHopIssue:
		return n.tickets.HandleIssue(ctx, sender, e)
	case envelope.TypeHopAccept:
		return nil, n.tickets.HandleAccept(sender, e)
	case envelope.TypeHopReject:
		return nil, n.tickets.HandleReject(sender, e)
	case envelope.TypeHopDeliver:
		return nil, n.tickets.HandleDeliver(ctx, sender, e)
	default:
		return nil, fmt.Errorf("%w: 未知消息类型 %q", envelope.ErrMalformed, e.Type)
	}
}

func (n *Node) dedupeGet(key string) ([]byte, bool) {
	raw, err := n.st.Get([]byte("dedupe"), []byte(key))
	if err != nil || raw == nil {
		return nil, false
	}
	return raw, true
}

func (n *Node) dedupePut(key string, resp *envelope.Envelope) {
	var raw []byte
	if resp == nil {
		raw = []byte("nil")
	} else {
		var err error
		raw, err = resp.Marshal()
		if err != nil {
			return
		}
	}
	if err := n.st.Put([]byte("dedupe"), []byte(key), raw); err != nil {
		slog.Warn("写幂等缓存失败", "key", key, "err", err)
	}
}

// reviveCached 把缓存的响应信封重新签名并刷新时间戳，
// 否则重试超过 5 分钟后，对方会把它当成过期消息。
func (n *Node) reviveCached(raw []byte) *envelope.Envelope {
	if string(raw) == "nil" {
		return nil
	}
	e, err := envelope.Unmarshal(raw)
	if err != nil {
		return nil
	}
	e.TS = time.Now().UnixMilli()
	if err := e.Sign(n.id.Private); err != nil {
		return nil
	}
	return e
}
