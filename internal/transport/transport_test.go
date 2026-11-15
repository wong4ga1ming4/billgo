package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"billgo/internal/blob"
	"billgo/internal/envelope"
	"billgo/internal/identity"
)

func seed(n byte) *identity.Identity {
	s := make([]byte, 32)
	s[0] = n
	return identity.FromSeed(s)
}

func start(t *testing.T, id *identity.Identity, ready func() bool, h HandlerFunc) (*Server, string) {
	t.Helper()
	s, err := NewServer(id.PeerID, id.Private, "127.0.0.1:0", ready, h, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.Close(ctx)
	})
	return s, "https://" + s.Addr()
}

func hop(t *testing.T, id *identity.Identity, to string, ts int64) *envelope.Envelope {
	t.Helper()
	e, err := envelope.New(envelope.TypeHopIssue, to, "t-1", 1, ts, map[string]any{"n": 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Sign(id.Private); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestHealth(t *testing.T) {
	srv := seed(2)
	_, endpoint := start(t, srv, nil, nil)

	h, err := NewClient(seed(1).PeerID).Health(context.Background(), endpoint, srv.PeerID)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Ready || h.PeerID != srv.PeerID {
		t.Fatalf("health = %+v", h)
	}
}

func TestSendAndResponse(t *testing.T) {
	apple, peach := seed(1), seed(2)
	_, endpoint := start(t, peach, nil, func(_ context.Context, sender string, e *envelope.Envelope) (*envelope.Envelope, error) {
		if sender != apple.PeerID {
			t.Errorf("handler 收到的 sender=%s，期望 %s", sender, apple.PeerID)
		}
		resp, err := envelope.New(envelope.TypeHopAccept, sender, e.Ticket, e.Hop, time.Now().UnixMilli(), map[string]any{"ok": true})
		if err != nil {
			t.Fatal(err)
		}
		return resp, resp.Sign(peach.Private)
	})

	resp, err := NewClient(apple.PeerID).Send(context.Background(), endpoint, peach.PeerID,
		hop(t, apple, peach.PeerID, time.Now().UnixMilli()))
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Type != envelope.TypeHopAccept {
		t.Fatalf("响应不对: %+v", resp)
	}
	sender, err := resp.VerifySignature()
	if err != nil {
		t.Fatal(err)
	}
	if sender != peach.PeerID {
		t.Fatalf("响应发送者 %s", sender)
	}
}

func TestPinMismatchRejected(t *testing.T) {
	apple, peach, banana := seed(1), seed(2), seed(3)
	_, endpoint := start(t, peach, nil, nil)

	_, err := NewClient(apple.PeerID).Health(context.Background(), endpoint, banana.PeerID)
	if err == nil {
		t.Fatal("指纹不符的连接应失败")
	}
	if !strings.Contains(err.Error(), "对端身份不符") {
		t.Fatalf("错误应来自指纹比对，得到 %v", err)
	}
}

func TestUnsignedRejected(t *testing.T) {
	apple, peach := seed(1), seed(2)
	_, endpoint := start(t, peach, nil, func(context.Context, string, *envelope.Envelope) (*envelope.Envelope, error) {
		t.Fatal("非法请求不应进入 handler")
		return nil, nil
	})

	e, err := envelope.New(envelope.TypeHopIssue, peach.PeerID, "t-1", 1, time.Now().UnixMilli(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	e.From = base64.StdEncoding.EncodeToString(apple.Public)
	raw, err := e.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := NewClient(apple.PeerID).httpFor(endpoint, peach.PeerID).Post(
		endpoint+"/envelope", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("无签名请求应 400，得到 %d", resp.StatusCode)
	}
}

func TestStaleRejected(t *testing.T) {
	apple, peach := seed(1), seed(2)
	_, endpoint := start(t, peach, nil, func(context.Context, string, *envelope.Envelope) (*envelope.Envelope, error) {
		t.Fatal("过期消息不应进入 handler")
		return nil, nil
	})

	old := hop(t, apple, peach.PeerID, time.Now().Add(-time.Hour).UnixMilli())
	_, err := NewClient(apple.PeerID).Send(context.Background(), endpoint, peach.PeerID, old)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusBadRequest {
		t.Fatalf("过期消息应被 400 拒绝，得到 %v", err)
	}
}

func TestWrongRecipientRejected(t *testing.T) {
	apple, peach, banana := seed(1), seed(2), seed(3)
	_, endpoint := start(t, peach, nil, func(context.Context, string, *envelope.Envelope) (*envelope.Envelope, error) {
		t.Fatal("收件人不符不应进入 handler")
		return nil, nil
	})

	_, err := NewClient(apple.PeerID).Send(context.Background(), endpoint, peach.PeerID,
		hop(t, apple, banana.PeerID, time.Now().UnixMilli()))
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusBadRequest {
		t.Fatalf("收件人不符应被 400 拒绝，得到 %v", err)
	}
}

func TestBlobDownload(t *testing.T) {
	apple, peach := seed(1), seed(2)
	blobDir := t.TempDir()
	content := []byte("deliverable bytes")
	hash, size, err := blob.PutBytes(blobDir, content)
	if err != nil {
		t.Fatal(err)
	}

	s, err := NewServer(peach.PeerID, peach.Private, "127.0.0.1:0", nil, nil, blobDir)
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.Close(ctx)
	})

	got, err := NewClient(apple.PeerID).FetchBlob(context.Background(), "https://"+s.Addr(), peach.PeerID, hash)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) || int64(len(got)) != size {
		t.Fatalf("内容不对: %q", got)
	}

	// 非法哈希不该被当成文件路径。
	_, err = NewClient(apple.PeerID).FetchBlob(context.Background(), "https://"+s.Addr(), peach.PeerID, "../../etc/passwd")
	if err == nil {
		t.Fatal("非法哈希应被拒绝")
	}
}

func TestNotReady(t *testing.T) {
	apple, peach := seed(1), seed(2)
	_, endpoint := start(t, peach, func() bool { return false }, func(context.Context, string, *envelope.Envelope) (*envelope.Envelope, error) {
		t.Fatal("未就绪时不应进入 handler")
		return nil, nil
	})

	c := NewClient(apple.PeerID)
	h, err := c.Health(context.Background(), endpoint, peach.PeerID)
	if err != nil {
		t.Fatal(err)
	}
	if h.Ready {
		t.Fatal("health 应返回未就绪")
	}
	_, err = c.Send(context.Background(), endpoint, peach.PeerID, hop(t, apple, peach.PeerID, time.Now().UnixMilli()))
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusServiceUnavailable {
		t.Fatalf("未就绪应返回 503，得到 %v", err)
	}
}
