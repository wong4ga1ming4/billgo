package outbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"billgo/internal/config"
	"billgo/internal/envelope"
	"billgo/internal/identity"
	"billgo/internal/peers"
	"billgo/internal/store"
	"billgo/internal/transport"
)

type fakeTP struct {
	mu      sync.Mutex
	healthy bool
	fail    bool
	sent    int
	last    *envelope.Envelope
}

func (f *fakeTP) Send(_ context.Context, _ string, _ string, e *envelope.Envelope) (*envelope.Envelope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errors.New("发送失败")
	}
	f.sent++
	f.last = e
	return nil, nil
}

func (f *fakeTP) Health(context.Context, string, string) (transport.Health, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.healthy {
		return transport.Health{}, errors.New("不可达")
	}
	return transport.Health{PeerID: "x", Ready: true}, nil
}

func (f *fakeTP) setHealthy(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.healthy = v
}

type fakeDir struct{ rec peers.Record }

func (d fakeDir) Lookup(id string) (peers.Record, bool) {
	if id == d.rec.PeerID {
		return d.rec, true
	}
	return peers.Record{}, false
}

func newQueue(t *testing.T, tp Transport, dir Directory, waker Waker) (*Queue, *identity.Identity) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	id := identity.FromSeed(make([]byte, 32))
	return New(st, tp, dir, waker, id.Private), id
}

func TestEnsureReadyWakesWakeable(t *testing.T) {
	tp := &fakeTP{}
	dir := fakeDir{rec: peers.Record{
		PeerID: "peach", Kind: config.KindWakeable,
		Endpoint: "https://peach:8787",
		Wake:     &config.Wake{Mode: "ssh", Target: "u@host", Container: "peach"},
	}}
	woken := 0
	q, _ := newQueue(t, tp, dir, func(context.Context, *config.Wake) error {
		woken++
		tp.setHealthy(true)
		return nil
	})

	if err := q.ensureReady(context.Background(), "peach"); err != nil {
		t.Fatalf("唤醒后应就绪: %v", err)
	}
	if woken != 1 {
		t.Fatalf("应恰好唤醒一次，实际 %d", woken)
	}
}

func TestEnsureReadyResidentDown(t *testing.T) {
	tp := &fakeTP{}
	dir := fakeDir{rec: peers.Record{PeerID: "banana", Kind: config.KindResident, Endpoint: "https://banana:8787"}}
	woken := 0
	q, _ := newQueue(t, tp, dir, func(context.Context, *config.Wake) error {
		woken++
		return nil
	})
	if err := q.ensureReady(context.Background(), "banana"); err == nil {
		t.Fatal("常驻节点不可达应报错")
	}
	if woken != 0 {
		t.Fatal("常驻节点不该触发唤醒")
	}
}

func TestEnsureReadyUnknownPeer(t *testing.T) {
	q, _ := newQueue(t, &fakeTP{}, fakeDir{}, nil)
	if err := q.ensureReady(context.Background(), "nobody"); err == nil {
		t.Fatal("名录里没有的节点应报错")
	}
}

func TestAttemptRetriesThenSucceeds(t *testing.T) {
	tp := &fakeTP{healthy: true, fail: true}
	dir := fakeDir{rec: peers.Record{PeerID: "banana", Kind: config.KindResident, Endpoint: "https://banana:8787"}}
	q, id := newQueue(t, tp, dir, nil)
	fixed := time.Unix(1000, 0)
	q.now = func() time.Time { return fixed }

	env, err := envelope.New(envelope.TypeHopIssue, "banana", "t1", 1, fixed.Add(-time.Hour).UnixMilli(), map[string]any{"x": 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Sign(id.Private); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue("banana", env); err != nil {
		t.Fatal(err)
	}

	items := q.List()
	if len(items) != 1 {
		t.Fatalf("队列应有 1 条，实际 %d", len(items))
	}
	q.attempt(context.Background(), items[0])
	items = q.List()
	if len(items) != 1 || items[0].Attempts != 1 {
		t.Fatalf("失败后应重排并发数 1: %+v", items)
	}
	if got := items[0].NextAt - fixed.UnixMilli(); got != 1000 {
		t.Fatalf("第一次退避应为 1s，实际 %dms", got)
	}

	q.attempt(context.Background(), items[0])
	items = q.List()
	if items[0].Attempts != 2 {
		t.Fatalf("第二次失败后尝试数应为 2: %+v", items[0])
	}
	if got := items[0].NextAt - fixed.UnixMilli(); got != 2000 {
		t.Fatalf("第二次退避应为 2s，实际 %dms", got)
	}

	tp.mu.Lock()
	tp.fail = false
	tp.mu.Unlock()
	q.attempt(context.Background(), items[0])
	if items := q.List(); len(items) != 0 {
		t.Fatalf("成功后应删除条目，剩 %+v", items)
	}

	// 发送前必须重签时间戳：原信封是一个小时前的，重放窗口只有 5 分钟。
	if tp.last == nil {
		t.Fatal("没有记录到实际发送的信封")
	}
	if tp.last.TS != fixed.UnixMilli() {
		t.Fatalf("发送前应刷新时间戳，得到 %d，期望 %d", tp.last.TS, fixed.UnixMilli())
	}
	if _, err := tp.last.VerifySignature(); err != nil {
		t.Fatalf("重签后的信封应可验签: %v", err)
	}
}
