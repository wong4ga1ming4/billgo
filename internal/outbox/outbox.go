// Package outbox 保证"至少一次投递 + 幂等执行"：先落盘，再重试到成功。
package outbox

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"billgo/internal/config"
	"billgo/internal/envelope"
	"billgo/internal/peers"
	"billgo/internal/store"
	"billgo/internal/transport"
)

// Bucket 待发队列所在的桶。
var Bucket = []byte("outbox")

const (
	maxBackoff    = 5 * time.Minute
	healthTimeout = 3 * time.Second
	wakeTimeout   = 30 * time.Second
	pollInterval  = 500 * time.Millisecond
)

// Transport 出站能力，测试可替换。
type Transport interface {
	Send(ctx context.Context, endpoint, peerID string, e *envelope.Envelope) (*envelope.Envelope, error)
	Health(ctx context.Context, endpoint, peerID string) (transport.Health, error)
}

// Directory 名录只读能力。
type Directory interface {
	Lookup(peerID string) (peers.Record, bool)
}

// Waker 唤醒实现，测试可替换。
type Waker func(ctx context.Context, w *config.Wake) error

// Item 一条待发信封。
type Item struct {
	ID       uint64          `json:"id"`
	PeerID   string          `json:"peer_id"`
	Envelope json.RawMessage `json:"envelope"`
	Attempts int             `json:"attempts"`
	NextAt   int64           `json:"next_at"` // Unix 毫秒
	LastErr  string          `json:"last_err,omitempty"`
}

// Queue 出站队列。
type Queue struct {
	st     *store.Store
	tp     Transport
	dir    Directory
	wake   Waker
	priv   ed25519.PrivateKey
	signal chan struct{}
	now    func() time.Time
}

// New 构造队列。wake 为 nil 时不做唤醒；priv 用于每次重试前重签时间戳。
func New(st *store.Store, tp Transport, dir Directory, wake Waker, priv ed25519.PrivateKey) *Queue {
	return &Queue{
		st:     st,
		tp:     tp,
		dir:    dir,
		wake:   wake,
		priv:   priv,
		signal: make(chan struct{}, 1),
		now:    time.Now,
	}
}

// Enqueue 把一封已签名的信封放入队列，立即返回。
func (q *Queue) Enqueue(peerID string, e *envelope.Envelope) error {
	raw, err := e.Marshal()
	if err != nil {
		return fmt.Errorf("编码信封: %w", err)
	}
	seq, err := q.st.NextSeq(Bucket)
	if err != nil {
		return err
	}
	it := Item{ID: seq, PeerID: peerID, Envelope: raw, NextAt: q.now().UnixMilli()}
	buf, err := json.Marshal(it)
	if err != nil {
		return err
	}
	if err := q.st.Put(Bucket, key(seq), buf); err != nil {
		return err
	}
	select {
	case q.signal <- struct{}{}:
	default:
	}
	return nil
}

// Run 持续投递，直到 ctx 结束。
func (q *Queue) Run(ctx context.Context) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		q.drainDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-q.signal:
		}
	}
}

// List 返回队列快照，用于排查。
func (q *Queue) List() []Item {
	var out []Item
	q.st.ForEach(Bucket, func(_, v []byte) error {
		var it Item
		if json.Unmarshal(v, &it) == nil {
			out = append(out, it)
		}
		return nil
	})
	return out
}

func (q *Queue) drainDue(ctx context.Context) {
	now := q.now().UnixMilli()
	var due []Item
	q.st.ForEach(Bucket, func(_, v []byte) error {
		var it Item
		if json.Unmarshal(v, &it) == nil && it.NextAt <= now {
			due = append(due, it)
		}
		return nil
	})
	for _, it := range due {
		if ctx.Err() != nil {
			return
		}
		q.attempt(ctx, it)
	}
}

func (q *Queue) attempt(ctx context.Context, it Item) {
	var e envelope.Envelope
	if err := json.Unmarshal(it.Envelope, &e); err != nil {
		q.drop(it, fmt.Errorf("信封损坏: %w", err))
		return
	}
	// 每次尝试前重签时间戳：重放窗口只有 5 分钟，而重试可能持续很久。
	// 幂等键不含时间，重签不影响接收方去重。
	e.TS = q.now().UnixMilli()
	if err := e.Sign(q.priv); err != nil {
		q.drop(it, fmt.Errorf("重签信封: %w", err))
		return
	}
	err := q.ensureReady(ctx, it.PeerID)
	if err == nil {
		_, err = q.tp.Send(ctx, q.endpointOf(it.PeerID), it.PeerID, &e)
	}
	if err == nil {
		if derr := q.st.Delete(Bucket, key(it.ID)); derr != nil {
			slog.Warn("删除已发条目失败", "id", it.ID, "err", derr)
		}
		return
	}
	q.reschedule(it, err)
}

func (q *Queue) endpointOf(peerID string) string {
	if rec, ok := q.dir.Lookup(peerID); ok {
		return rec.Endpoint
	}
	return ""
}

// ensureReady 投递前确认对方能接单；唤醒类先叫醒，再等 health。
func (q *Queue) ensureReady(ctx context.Context, peerID string) error {
	rec, ok := q.dir.Lookup(peerID)
	if !ok {
		return fmt.Errorf("名录里没有 %s", peerID)
	}
	if _, err := q.health(ctx, rec); err == nil {
		return nil
	}
	if rec.Kind != config.KindWakeable || q.wake == nil || rec.Wake == nil {
		return fmt.Errorf("%s 不可达", peerID)
	}
	slog.Info("唤醒节点", "peer", peerID, "target", rec.Wake.Target)
	wctx, cancel := context.WithTimeout(ctx, wakeTimeout)
	defer cancel()
	if err := q.wake(wctx, rec.Wake); err != nil {
		return fmt.Errorf("唤醒 %s: %w", peerID, err)
	}
	deadline := time.Now().Add(wakeTimeout)
	for time.Now().Before(deadline) {
		if _, err := q.health(ctx, rec); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	return fmt.Errorf("%s 唤醒后仍不可达", peerID)
}

func (q *Queue) health(ctx context.Context, rec peers.Record) (transport.Health, error) {
	cctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	return q.tp.Health(cctx, rec.Endpoint, rec.PeerID)
}

func (q *Queue) reschedule(it Item, cause error) {
	it.Attempts++
	backoff := time.Second << uint(min(it.Attempts-1, 10))
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	it.NextAt = q.now().Add(backoff).UnixMilli()
	it.LastErr = cause.Error()
	buf, err := json.Marshal(it)
	if err != nil {
		q.drop(it, err)
		return
	}
	if err := q.st.Put(Bucket, key(it.ID), buf); err != nil {
		slog.Error("重排队列条目失败", "id", it.ID, "err", err)
	}
}

func (q *Queue) drop(it Item, cause error) {
	slog.Error("丢弃队列条目", "id", it.ID, "peer", it.PeerID, "err", cause)
	_ = q.st.Delete(Bucket, key(it.ID))
}

func key(id uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], id)
	return b[:]
}
