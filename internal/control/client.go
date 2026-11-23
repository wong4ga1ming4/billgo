package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client 访问本机节点的控制面。
type Client struct {
	socket string
	hc     *http.Client
}

// NewClient 构造控制面客户端。
func NewClient(dataDir string) *Client {
	socket := SocketPath(dataDir)
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{socket: socket, hc: &http.Client{Transport: tr, Timeout: 30 * time.Second}}
}

// Call 让本机节点执行一个控制动作。peer 为空表示自己，否则是邻居的名字或 peer_id。
func (c *Client) Call(ctx context.Context, peer, action string, args, out any) error {
	body := map[string]any{"action": action}
	if peer != "" {
		body["peer"] = peer
	}
	if args != nil {
		body["args"] = args
	}
	if err := c.Do(ctx, http.MethodPost, "/ctl", body, out); err != nil {
		return unwrapAPIError(err)
	}
	return nil
}

// APIError 是控制面返回的可分类错误。Code 见节点的 denied / unreachable / timeout / not_found。
type APIError struct {
	Message string
	Code    string
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Message + " (" + e.Code + ")"
}

func unwrapAPIError(err error) error {
	var parsed struct {
		Error string `json:"error"`
		Code  string `json:"error_code"`
	}
	if json.Unmarshal([]byte(err.Error()), &parsed) == nil && parsed.Error != "" {
		return &APIError{Message: parsed.Error, Code: parsed.Code}
	}
	return err
}

// Do 发一个 JSON 请求并解析 JSON 响应。
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://billgo"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接节点（%s）: %w", c.socket, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s", bytes.TrimSpace(raw))
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("解析响应: %w", err)
		}
	}
	return nil
}
