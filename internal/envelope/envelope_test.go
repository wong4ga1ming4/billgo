package envelope

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"billgo/internal/identity"
)

func seed(n byte) *identity.Identity {
	s := make([]byte, 32)
	s[0] = n
	return identity.FromSeed(s)
}

func signedHop(t *testing.T, id *identity.Identity, to string, ts int64) *Envelope {
	t.Helper()
	e, err := New(TypeHopIssue, to, "t-1", 1, ts, map[string]any{"form": map[string]any{"task": "写文档"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Sign(id.Private); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestRoundTrip(t *testing.T) {
	apple, peach := seed(1), seed(2)
	e := signedHop(t, apple, peach.PeerID, time.Now().UnixMilli())

	raw, err := e.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := got.VerifySignature()
	if err != nil {
		t.Fatal(err)
	}
	if sender != apple.PeerID {
		t.Fatalf("发送者 %s，期望 %s", sender, apple.PeerID)
	}
	if err := got.CheckAddress(peach.PeerID); err != nil {
		t.Fatal(err)
	}
	if err := got.CheckFresh(time.Now(), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestReformattedBodyStillVerifies(t *testing.T) {
	apple, peach := seed(1), seed(2)
	e := signedHop(t, apple, peach.PeerID, time.Now().UnixMilli())

	// 语义相同、写法不同：规范化之后签名应当仍然有效。
	alt, err := New(TypeHopIssue, peach.PeerID, "t-1", 1, e.TS, json.RawMessage(`{"form":{"task":"写文档"}}`))
	if err != nil {
		t.Fatal(err)
	}
	alt.From = e.From
	alt.Sig = e.Sig
	if _, err := alt.VerifySignature(); err != nil {
		t.Fatalf("同样语义的 body 不应导致验签失败: %v", err)
	}
}

func TestTamperedBodyFails(t *testing.T) {
	apple, peach := seed(1), seed(2)
	e := signedHop(t, apple, peach.PeerID, time.Now().UnixMilli())
	e.Body = json.RawMessage(`{"form":{"task":"改一下"}}`)
	if _, err := e.VerifySignature(); !errors.Is(err, ErrSignature) {
		t.Fatalf("篡改 body 应为签名错误，得到 %v", err)
	}
}

func TestTamperedToFails(t *testing.T) {
	apple, peach := seed(1), seed(2)
	e := signedHop(t, apple, peach.PeerID, time.Now().UnixMilli())
	e.To = seed(3).PeerID
	if _, err := e.VerifySignature(); !errors.Is(err, ErrSignature) {
		t.Fatalf("篡改 to 应为签名错误，得到 %v", err)
	}
}

func TestWrongRecipient(t *testing.T) {
	apple, peach := seed(1), seed(2)
	e := signedHop(t, apple, peach.PeerID, time.Now().UnixMilli())
	if err := e.CheckAddress(seed(3).PeerID); !errors.Is(err, ErrRecipient) {
		t.Fatalf("收件人不符应报错，得到 %v", err)
	}
	if err := e.CheckAddress(""); !errors.Is(err, ErrRecipient) {
		t.Fatalf("带 to 的消息不该被空地址放行，得到 %v", err)
	}
}

func TestStaleHopRejected(t *testing.T) {
	apple, peach := seed(1), seed(2)
	old := time.Now().Add(-10 * time.Minute).UnixMilli()
	e := signedHop(t, apple, peach.PeerID, old)
	if err := e.CheckFresh(time.Now(), 5*time.Minute); !errors.Is(err, ErrStale) {
		t.Fatalf("过期工单消息应被拒绝，得到 %v", err)
	}
}

func TestOldPeersRecordAccepted(t *testing.T) {
	apple := seed(1)
	e, err := New(TypePeersRecord, "", "", 0, time.Now().Add(-time.Hour).UnixMilli(),
		map[string]any{"version": 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Sign(apple.Private); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckFresh(time.Now(), 5*time.Minute); err != nil {
		t.Fatalf("名录消息不受重放窗口限制，得到 %v", err)
	}
}

func TestIdempotencyKeyHop(t *testing.T) {
	apple, peach := seed(1), seed(2)
	e := signedHop(t, apple, peach.PeerID, time.Now().UnixMilli())
	key, ok, err := e.IdempotencyKey()
	if err != nil || !ok {
		t.Fatal(err)
	}
	if key != "hop|t-1|1|hop.issue" {
		t.Fatalf("幂等键 %q", key)
	}
}

func TestIdempotencyKeyPeersRecord(t *testing.T) {
	apple := seed(1)
	e, err := New(TypePeersRecord, "", "", 0, time.Now().UnixMilli(),
		map[string]any{"version": 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Sign(apple.Private); err != nil {
		t.Fatal(err)
	}
	key, ok, err := e.IdempotencyKey()
	if err != nil || !ok {
		t.Fatal(err)
	}
	want := "peer|" + apple.PeerID + "|7"
	if key != want {
		t.Fatalf("幂等键 %q，期望 %q", key, want)
	}
}

func TestStaleCtlRejected(t *testing.T) {
	apple, peach := seed(1), seed(2)
	old := time.Now().Add(-10 * time.Minute).UnixMilli()
	e, err := New(TypeCtlRequest, peach.PeerID, "", 0, old, map[string]any{"id": "r1", "action": "node.info"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Sign(apple.Private); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckFresh(time.Now(), 5*time.Minute); !errors.Is(err, ErrStale) {
		t.Fatalf("过期控制请求应被拒绝，得到 %v", err)
	}
}

func TestCtlIdempotencyKey(t *testing.T) {
	apple, peach := seed(1), seed(2)
	e, err := New(TypeCtlRequest, peach.PeerID, "", 0, time.Now().UnixMilli(), map[string]any{"id": "r1", "action": "node.info"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Sign(apple.Private); err != nil {
		t.Fatal(err)
	}
	key, ok, err := e.IdempotencyKey()
	if err != nil || !ok || key != "ctl|r1" {
		t.Fatalf("控制请求幂等键不对: %q ok=%v err=%v", key, ok, err)
	}
}

func TestDigestNotDeduped(t *testing.T) {
	e, err := New(TypePeersDigest, "", "", 0, time.Now().UnixMilli(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := e.IdempotencyKey(); err != nil || ok {
		t.Fatalf("digest 不需要去重，得到 ok=%v err=%v", ok, err)
	}
}

func TestUnmarshalStrict(t *testing.T) {
	apple, peach := seed(1), seed(2)
	e := signedHop(t, apple, peach.PeerID, time.Now().UnixMilli())
	raw, err := e.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["extra"] = 1
	bad, _ := json.Marshal(m)
	if _, err := Unmarshal(bad); !errors.Is(err, ErrMalformed) {
		t.Fatalf("未知字段应报错，得到 %v", err)
	}

	if _, err := Unmarshal([]byte(`{"v":1,"ts":1}`)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("缺 type 应报错，得到 %v", err)
	}
}

func TestVersionMismatch(t *testing.T) {
	apple, peach := seed(1), seed(2)
	e := signedHop(t, apple, peach.PeerID, time.Now().UnixMilli())
	e.V = 99
	_, err := e.VerifySignature()
	if !errors.Is(err, ErrVersion) && !errors.Is(err, ErrSignature) {
		t.Fatalf("版本不符应被拒绝，得到 %v", err)
	}
}

func TestBadSenderKey(t *testing.T) {
	apple, peach := seed(1), seed(2)
	e := signedHop(t, apple, peach.PeerID, time.Now().UnixMilli())
	e.From = "bm90LWEta2V5"
	_, err := e.VerifySignature()
	if !errors.Is(err, ErrSender) {
		t.Fatalf("公钥长度不对应报 ErrSender，得到 %v", err)
	}
}
