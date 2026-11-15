package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"billgo/internal/envelope"
)

// DefaultTimeout 单次请求的超时。
const DefaultTimeout = 10 * time.Second

// Health 健康检查的返回。
type Health struct {
	PeerID string `json:"peer_id"`
	Ready  bool   `json:"ready"`
}

// StatusError 对端返回了非 2xx。
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Code, strings.TrimSpace(e.Body))
}

// Is 让 errors.Is(err, ErrStatus) 成立。
func (e *StatusError) Is(target error) bool { return target == ErrStatus }

// Client 出站面。按 (endpoint, peer_id) 缓存连接，指纹变化时自然换连接。
type Client struct {
	self    string
	timeout time.Duration
	mu      sync.Mutex
	clients map[string]*http.Client
}

// NewClient self 是本节点的 peer_id，用于校验收件人。
func NewClient(self string) *Client {
	return &Client{self: self, timeout: DefaultTimeout, clients: map[string]*http.Client{}}
}

func (c *Client) httpFor(endpoint, peerID string) *http.Client {
	key := endpoint + "|" + peerID
	c.mu.Lock()
	defer c.mu.Unlock()
	if hc, ok := c.clients[key]; ok {
		return hc
	}
	tr := &http.Transport{
		TLSClientConfig:     PinnedConfig(peerID),
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}
	hc := &http.Client{Transport: tr, Timeout: c.timeout}
	c.clients[key] = hc
	return hc
}

// Send 把信封投给对端，返回对方回的信封（可能为 nil）。
// 响应信封同样要验签、核对收件人和时间窗口。
func (c *Client) Send(ctx context.Context, endpoint, peerID string, e *envelope.Envelope) (*envelope.Envelope, error) {
	body, err := e.Marshal()
	if err != nil {
		return nil, fmt.Errorf("编码信封: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/envelope", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpFor(endpoint, peerID).Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTransport, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
	if err != nil {
		return nil, fmt.Errorf("%w: 读响应: %v", ErrTransport, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &StatusError{Code: resp.StatusCode, Body: string(raw)}
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	out, err := envelope.Unmarshal(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: 响应信封: %v", ErrTransport, err)
	}
	if _, err := out.VerifySignature(); err != nil {
		return nil, fmt.Errorf("%w: 响应签名: %v", ErrTransport, err)
	}
	if err := out.CheckAddress(c.self); err != nil {
		return nil, fmt.Errorf("%w: 响应收件人: %v", ErrTransport, err)
	}
	if err := out.CheckFresh(time.Now(), FreshWindow); err != nil {
		return nil, fmt.Errorf("%w: 响应时间: %v", ErrTransport, err)
	}
	return out, nil
}

// FetchBlob 从对端下载一个交付物，返回内容。调用方负责校验哈希。
func (c *Client) FetchBlob(ctx context.Context, endpoint, peerID, hash string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(endpoint, "/")+"/blobs/"+hash, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求: %w", err)
	}
	resp, err := c.httpFor(endpoint, peerID).Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTransport, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &StatusError{Code: resp.StatusCode, Body: string(raw)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: 读交付物: %v", ErrTransport, err)
	}
	return data, nil
}

// Health 查对方是否就绪。TLS 固定指纹，返回的 peer_id 再核对一次。
func (c *Client) Health(ctx context.Context, endpoint, peerID string) (Health, error) {
	var zero Health
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/health", nil)
	if err != nil {
		return zero, fmt.Errorf("构造请求: %w", err)
	}
	resp, err := c.httpFor(endpoint, peerID).Do(req)
	if err != nil {
		return zero, fmt.Errorf("%w: %v", ErrTransport, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return zero, &StatusError{Code: resp.StatusCode, Body: string(raw)}
	}
	var h Health
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&h); err != nil {
		return zero, fmt.Errorf("%w: 解析 health: %v", ErrTransport, err)
	}
	if h.PeerID != peerID {
		return zero, fmt.Errorf("%w: health 返回 %s，期望 %s", ErrIdentity, h.PeerID, peerID)
	}
	return h, nil
}
