// Package webui 是节点内置的只读展示面：本机浏览器可看状态、名录、工单，
// 并可发起工单。页面分内置页和 Agent 页，后者放在数据目录的 pages/ 下。
package webui

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"billgo/internal/outbox"
	"billgo/internal/peers"
	"billgo/internal/ticket"
)

//go:embed index.html new.html ticket.html assets/app.js assets/style.css
//go:embed scaffold/pages/README.md scaffold/pages/example.html scaffold/pages/example.json
var files embed.FS

const (
	builtinCSP = "default-src 'none'; script-src 'self'; style-src 'self'; " +
		"img-src 'self' data:; font-src 'self' data:; media-src 'self'; " +
		"connect-src 'self'; form-action 'self'; frame-ancestors 'none'; " +
		"base-uri 'none'; object-src 'none'"

	// Agent 页面允许内联脚本与样式，但禁止一切外部资源与外发通道。
	agentCSP = "default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; font-src 'self' data:; media-src 'self'; " +
		"connect-src 'self'; form-action 'self'; frame-ancestors 'none'; " +
		"base-uri 'none'; object-src 'none'"
)

// Directory 名录只读能力。
type Directory interface {
	Lookup(peerID string) (peers.Record, bool)
	ByName(name string) []peers.Record
	ListAll() []peers.Record
}

// Tickets 工单读写能力。
type Tickets interface {
	List() []ticket.State
	Get(id string) (ticket.State, []ticket.Event, error)
	Create(ticket.CreateReq) (string, error)
	Receipts(id string) ([]ticket.Receipt, error)
}

// Outbox 待发队列只读能力。
type Outbox interface {
	List() []outbox.Item
}

// Info 页面展示的节点信息。
type Info struct {
	PeerID    string
	Name      string
	Kind      string
	Network   string
	Endpoint  string
	Version   string
	Listen    string
	Bootstrap []string
	AllowFrom []string
	WakeMode  string
}

// Server 内置页面服务。
type Server struct {
	info   Info
	dir    Directory
	tk     Tickets
	out    Outbox
	root   string
	blobs  string
	pages  string
	ln     net.Listener
	srv    *http.Server
	now    func() time.Time
	pageFS map[string][]byte
}

// New 创建服务并占用监听端口。listen 必须已经过 loopback 校验。
func New(info Info, dir Directory, tk Tickets, out Outbox, root, blobDir, listen string) (*Server, error) {
	s := &Server{
		info:  info,
		dir:   dir,
		tk:    tk,
		out:   out,
		root:  root,
		blobs: blobDir,
		pages: filepath.Join(root, "pages"),
		now:   time.Now,
	}
	if err := s.loadBuiltin(); err != nil {
		return nil, err
	}
	if err := s.ensureScaffold(); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("监听页面 %s: %w", listen, err)
	}
	s.ln = ln
	s.srv = &http.Server{
		Handler:           s.middleware(s.routes()),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	return s, nil
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Handler 暴露处理器，测试用。
func (s *Server) Handler() http.Handler { return s.srv.Handler }

// Start 在后台开始服务。
func (s *Server) Start() {
	go func() {
		if err := s.srv.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("页面服务退出", "err", err)
		}
	}()
}

// Close 关闭服务。
func (s *Server) Close(ctx context.Context) error { return s.srv.Shutdown(ctx) }

func (s *Server) loadBuiltin() error {
	s.pageFS = map[string][]byte{}
	for _, name := range []string{"index.html", "new.html", "ticket.html"} {
		raw, err := files.ReadFile(name)
		if err != nil {
			return fmt.Errorf("读取内置页面 %s: %w", name, err)
		}
		s.pageFS[name] = raw
	}
	return nil
}

// ensureScaffold 首次启动时创建 pages/ 并放入说明与示例；之后不覆盖。
func (s *Server) ensureScaffold() error {
	if _, err := os.Stat(s.pages); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("检查 %s: %w", s.pages, err)
	}
	if err := os.MkdirAll(s.pages, 0o755); err != nil {
		return fmt.Errorf("建 pages 目录: %w", err)
	}
	for _, name := range []string{"README.md", "example.html", "example.json"} {
		raw, err := files.ReadFile("scaffold/pages/" + name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(s.pages, name), raw, 0o644); err != nil {
			return fmt.Errorf("写入示例 %s: %w", name, err)
		}
	}
	return nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.builtin("index.html"))
	mux.HandleFunc("GET /new.html", s.builtin("new.html"))
	mux.HandleFunc("GET /ticket.html", s.builtin("ticket.html"))
	mux.HandleFunc("GET /_ui/{name}", s.asset)
	mux.HandleFunc("GET /pages/{path...}", s.page)
	mux.HandleFunc("GET /api/node", s.apiNode)
	mux.HandleFunc("GET /api/status", s.apiStatus)
	mux.HandleFunc("GET /api/peers", s.apiPeers)
	mux.HandleFunc("GET /api/pages", s.apiPages)
	mux.HandleFunc("GET /api/tickets", s.apiTickets)
	mux.HandleFunc("GET /api/tickets/{id}", s.apiTicket)
	mux.HandleFunc("GET /api/tickets/{id}/artifacts", s.apiArtifacts)
	mux.HandleFunc("GET /api/blobs/{hash}", s.apiBlob)
	mux.HandleFunc("POST /api/tickets", s.apiCreate)
	return mux
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

func originAllowed(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return loopbackHost(u.Host)
}
