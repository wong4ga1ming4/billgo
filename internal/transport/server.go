// Package transport 是信封的载体：HTTP POST + GET /health，TLS 指纹绑定。
//
// 传输层不做业务判断，只做协议级校验：版本、签名、收件人、重放窗口。
package transport

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"billgo/internal/blob"
	"billgo/internal/envelope"
)

// 传输层错误，调用方据此决定重试还是放弃。
var (
	ErrIdentity  = errors.New("对端身份不符")
	ErrTransport = errors.New("传输失败")
	ErrStatus    = errors.New("对端返回错误状态")
)

// MaxBody 单个信封的大小上限。
const MaxBody = 8 << 20

// FreshWindow hop.* 消息允许的时间偏差。
const FreshWindow = 5 * time.Minute

// HandlerFunc 处理一封已通过协议校验的信封，返回要回给对方的信封。
type HandlerFunc func(ctx context.Context, sender string, e *envelope.Envelope) (*envelope.Envelope, error)

// Server 一个节点的入站面。
type Server struct {
	self    string
	ready   func() bool
	handler HandlerFunc
	ln      net.Listener
	srv     *http.Server
}

// NewServer 在 addr 上监听 TLS。addr 传 ":0" 可让系统分配端口。
// blobDir 用于对外提供交付物下载。
func NewServer(self string, priv ed25519.PrivateKey, addr string, ready func() bool, h HandlerFunc, blobDir string) (*Server, error) {
	cert, err := Certificate(priv)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("监听 %s: %w", addr, err)
	}
	if ready == nil {
		ready = func() bool { return true }
	}
	s := &Server{self: self, ready: ready, handler: h, ln: tls.NewListener(ln, ServerTLSConfig(cert))}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /envelope", s.handleEnvelope)
	mux.HandleFunc("GET /blobs/{hash}", func(w http.ResponseWriter, r *http.Request) {
		hash := r.PathValue("hash")
		if !blob.ValidHash(hash) {
			httpError(w, http.StatusBadRequest, fmt.Errorf("非法哈希"))
			return
		}
		http.ServeFile(w, r, blob.Path(blobDir, hash))
	})
	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	return s, nil
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Start 在后台开始服务。
func (s *Server) Start() {
	go func() {
		if err := s.srv.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP 服务退出", "err", err)
		}
	}()
}

// Close 优雅关闭。
func (s *Server) Close(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"peer_id": s.self,
		"ready":   s.ready(),
	})
}

func (s *Server) handleEnvelope(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		httpError(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	e, err := envelope.Unmarshal(raw)
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	sender, err := e.VerifySignature()
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	if err := e.CheckAddress(s.self); err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	if err := e.CheckFresh(time.Now(), FreshWindow); err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	if !s.ready() {
		httpError(w, http.StatusServiceUnavailable, errors.New("节点未就绪"))
		return
	}

	resp, err := s.handler(r.Context(), sender, e)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if resp == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	out, err := resp.Marshal()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

func httpError(w http.ResponseWriter, code int, err error) {
	http.Error(w, err.Error(), code)
}
