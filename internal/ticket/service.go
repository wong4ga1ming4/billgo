package ticket

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"billgo/internal/blob"
	"billgo/internal/config"
	"billgo/internal/envelope"
	"billgo/internal/id"
	"billgo/internal/identity"
	"billgo/internal/peers"
	"billgo/internal/store"
)

// 桶名。
var (
	TicketsBucket  = []byte("tickets")
	ReceiptsBucket = []byte("receipts")
	JobsBucket     = []byte("jobs")
)

const tickInterval = 500 * time.Millisecond

// Transport 工单需要的出站能力。
type Transport interface {
	Send(ctx context.Context, endpoint, peerID string, e *envelope.Envelope) (*envelope.Envelope, error)
	FetchBlob(ctx context.Context, endpoint, peerID, hash string) ([]byte, error)
}

// Sender 投递队列。
type Sender interface {
	Enqueue(peerID string, e *envelope.Envelope) error
}

// Directory 名录只读能力。
type Directory interface {
	Lookup(peerID string) (peers.Record, bool)
}

// JobRecord 接活侧作业状态，保证进程重启后能继续。
type JobRecord struct {
	Ticket  string `json:"ticket"`
	Hop     int    `json:"hop"`
	From    string `json:"from"`
	Status  string `json:"status"` // running | delivered | rejected
	Err     string `json:"err,omitempty"`
	Updated int64  `json:"updated"`
}

// Service 工单服务，同时承担发起侧与接活侧。
type Service struct {
	id      *identity.Identity
	st      *store.Store
	dir     Directory
	tp      Transport
	out     Sender
	cfg     *config.Config
	blobDir string
	sem     chan struct{}
}

// New 构造工单服务。
func New(id *identity.Identity, st *store.Store, dir Directory, tp Transport, out Sender, cfg *config.Config) *Service {
	return &Service{
		id:      id,
		st:      st,
		dir:     dir,
		tp:      tp,
		out:     out,
		cfg:     cfg,
		blobDir: st.BlobDir(),
		sem:     make(chan struct{}, 1),
	}
}

// Create 创建一张工单，返回工单号。
func (s *Service) Create(req CreateReq) (string, error) {
	if err := req.Validate(); err != nil {
		return "", err
	}
	tid := id.New()
	data, err := json.Marshal(createdData{Form: req.Form, Flow: req.Flow})
	if err != nil {
		return "", err
	}
	if err := s.append(tid, Event{Type: EvCreated, Data: data}); err != nil {
		return "", err
	}
	return tid, nil
}

// Get 返回工单状态与完整事件流。
func (s *Service) Get(tid string) (State, []Event, error) {
	evs, err := s.Events(tid)
	if err != nil {
		return State{}, nil, err
	}
	if len(evs) == 0 {
		return State{}, nil, fmt.Errorf("工单 %s 不存在", tid)
	}
	st, err := Derive(tid, evs)
	return st, evs, err
}

// List 列出本节点发起或参与过的全部工单。
func (s *Service) List() []State {
	ids := map[string]bool{}
	s.st.ForEach(TicketsBucket, func(k, _ []byte) error {
		if i := strings.IndexByte(string(k), '|'); i > 0 {
			ids[string(k)[:i]] = true
		}
		return nil
	})
	out := make([]State, 0, len(ids))
	for tid := range ids {
		if st, _, err := s.Get(tid); err == nil {
			out = append(out, st)
		}
	}
	return out
}

// Events 按序返回一张工单的事件。
func (s *Service) Events(tid string) ([]Event, error) {
	prefix := tid + "|"
	var out []Event
	err := s.st.ForEach(TicketsBucket, func(k, v []byte) error {
		if !strings.HasPrefix(string(k), prefix) {
			return nil
		}
		var ev Event
		if err := json.Unmarshal(v, &ev); err == nil {
			out = append(out, ev)
		}
		return nil
	})
	return out, err
}

// Redispatch 拒绝或超时后，把这一跳改派给另一个节点。
func (s *Service) Redispatch(tid string, hop int, to string) error {
	st, _, err := s.Get(tid)
	if err != nil {
		return err
	}
	if st.Status != StatusRunning {
		return fmt.Errorf("工单 %s 已结束（%s）", tid, st.Status)
	}
	var target *HopState
	for i := range st.Hops {
		if st.Hops[i].Step == hop {
			target = &st.Hops[i]
		}
	}
	if target == nil {
		return fmt.Errorf("工单 %s 没有第 %d 跳", tid, hop)
	}
	if _, ok := s.dir.Lookup(to); !ok {
		return fmt.Errorf("名录里没有节点 %s", to)
	}
	switch target.State {
	case HopRejected, HopExpired, HopPending:
	default:
		return fmt.Errorf("第 %d 跳处于 %s，不能改派", hop, target.State)
	}
	data, _ := json.Marshal(AmendData{Hop: hop, To: to})
	return s.append(tid, Event{Type: EvAmended, Hop: hop, Peer: to, Data: data})
}

// Void 作废一张工单。
func (s *Service) Void(tid string) error {
	st, _, err := s.Get(tid)
	if err != nil {
		return err
	}
	if st.Status != StatusRunning {
		return fmt.Errorf("工单 %s 已结束（%s）", tid, st.Status)
	}
	return s.append(tid, Event{Type: EvVoided})
}

// Run 驱动所有进行中的工单，并在启动时恢复未完成的作业。
func (s *Service) Run(ctx context.Context) {
	s.resumeJobs()
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.driveOnce(ctx)
		}
	}
}

func (s *Service) driveOnce(ctx context.Context) {
	for _, st := range s.List() {
		if st.Status != StatusRunning {
			continue
		}
		cur := st.Current()
		if cur == nil {
			if err := s.append(st.ID, Event{Type: EvClosed}); err != nil {
				slog.Error("关闭工单失败", "ticket", st.ID, "err", err)
			}
			continue
		}
		now := time.Now().UnixMilli()
		switch cur.State {
		case HopPending:
			s.sendHop(ctx, st, cur)
		case HopSent:
			if now-cur.SentAt > int64(s.cfg.AcceptTimeoutMin)*60_000 {
				slog.Warn("派单超时", "ticket", st.ID, "hop", cur.Step, "peer", cur.Target)
				_ = s.append(st.ID, Event{Type: EvExpired, Hop: cur.Step, Peer: cur.Target})
			}
		case HopAccepted:
			if now-cur.AcceptedAt > int64(s.cfg.DeliverTimeoutMin)*60_000 {
				slog.Warn("交付超时", "ticket", st.ID, "hop", cur.Step, "peer", cur.Target)
				_ = s.append(st.ID, Event{Type: EvExpired, Hop: cur.Step, Peer: cur.Target})
			}
		}
	}
}

func (s *Service) sendHop(ctx context.Context, st State, h *HopState) {
	env, err := envelope.New(envelope.TypeHopIssue, h.Target, st.ID, h.Step, time.Now().UnixMilli(), IssueBody{Form: st.Form})
	if err != nil {
		slog.Error("构造派单信封失败", "ticket", st.ID, "err", err)
		return
	}
	if err := env.Sign(s.id.Private); err != nil {
		slog.Error("签派单信封失败", "ticket", st.ID, "err", err)
		return
	}
	if err := s.append(st.ID, Event{Type: EvSent, Hop: h.Step, Peer: h.Target}); err != nil {
		slog.Error("写派单事件失败", "ticket", st.ID, "err", err)
		return
	}
	if err := s.out.Enqueue(h.Target, env); err != nil {
		slog.Error("派单入队失败", "ticket", st.ID, "err", err)
	}
}

// HandleIssue 接活侧：验策略、立即回 accept、后台跑作业。
func (s *Service) HandleIssue(ctx context.Context, sender string, e *envelope.Envelope) (*envelope.Envelope, error) {
	var body IssueBody
	if err := json.Unmarshal(e.Body, &body); err != nil || len(body.Form) == 0 {
		return s.rejectEnvelope(sender, e, "表单为空或格式错误")
	}
	if !s.allowed(sender) {
		return s.rejectEnvelope(sender, e, "发起者不在白名单里")
	}
	if e.Ticket == "" || e.Hop <= 0 {
		return nil, fmt.Errorf("%w: 派单缺少 ticket/hop", envelope.ErrMalformed)
	}
	if err := s.saveJob(JobRecord{Ticket: e.Ticket, Hop: e.Hop, From: sender, Status: "running"}); err != nil {
		return nil, err
	}
	go s.runJob(e.Ticket, e.Hop, sender, body.Form)
	return s.acceptEnvelope(sender, e)
}

// HandleAccept 发起侧：某跳被受理。
func (s *Service) HandleAccept(sender string, e *envelope.Envelope) error {
	st, _, err := s.Get(e.Ticket)
	if err != nil {
		return err
	}
	h, err := findHop(st, e.Hop)
	if err != nil {
		return err
	}
	if h.Target != sender {
		return fmt.Errorf("%w: %s 不是第 %d 跳的目标", envelope.ErrSender, sender, e.Hop)
	}
	if h.State != HopSent {
		return nil // 重复或迟到，忽略
	}
	return s.append(e.Ticket, Event{Type: EvAccepted, Hop: e.Hop, Peer: sender})
}

// HandleReject 发起侧：某跳被拒绝。
func (s *Service) HandleReject(sender string, e *envelope.Envelope) error {
	st, _, err := s.Get(e.Ticket)
	if err != nil {
		return err
	}
	h, err := findHop(st, e.Hop)
	if err != nil {
		return err
	}
	if h.Target != sender {
		return fmt.Errorf("%w: %s 不是第 %d 跳的目标", envelope.ErrSender, sender, e.Hop)
	}
	if h.State == HopRejected || h.State == HopVerified {
		return nil
	}
	return s.append(e.Ticket, Event{Type: EvRejected, Hop: e.Hop, Peer: sender, Data: e.Body})
}

// HandleDeliver 发起侧：收交付物、核对回执、推进下一跳。
func (s *Service) HandleDeliver(ctx context.Context, sender string, e *envelope.Envelope) error {
	st, _, err := s.Get(e.Ticket)
	if err != nil {
		return err
	}
	h, err := findHop(st, e.Hop)
	if err != nil {
		return err
	}
	if h.Target != sender {
		return fmt.Errorf("%w: %s 不是第 %d 跳的目标", envelope.ErrSender, sender, e.Hop)
	}
	if h.State == HopVerified {
		return nil
	}

	var body DeliverBody
	if err := json.Unmarshal(e.Body, &body); err != nil {
		return fmt.Errorf("%w: 交付体格式错误", envelope.ErrMalformed)
	}
	if !blob.ValidHash(body.Blob) {
		return fmt.Errorf("%w: 交付物哈希非法", envelope.ErrMalformed)
	}
	rec, ok := s.dir.Lookup(sender)
	if !ok {
		return fmt.Errorf("%w: 名录里没有 %s", envelope.ErrSender, sender)
	}
	pub, err := base64.StdEncoding.DecodeString(rec.PubKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: 交付方公钥无效", envelope.ErrSender)
	}
	r := body.Receipt
	if r.Ticket != e.Ticket || r.Hop != e.Hop || r.From != sender || r.To != s.id.PeerID || r.Blob != body.Blob {
		return fmt.Errorf("%w: 回执与工单不一致", envelope.ErrMalformed)
	}
	if err := VerifyReceipt(ed25519.PublicKey(pub), r); err != nil {
		return err
	}

	data, err := s.tp.FetchBlob(ctx, rec.Endpoint, sender, body.Blob)
	if err != nil {
		return fmt.Errorf("下载交付物: %w", err)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != body.Blob {
		return fmt.Errorf("%w: 交付物内容与哈希不符", envelope.ErrSignature)
	}
	hash, size, err := blob.PutBytes(s.blobDir, data)
	if err != nil {
		return err
	}
	if hash != body.Blob || size != body.Size {
		return fmt.Errorf("%w: 交付物尺寸不一致", envelope.ErrMalformed)
	}

	rawRec, _ := json.Marshal(r)
	if err := s.st.Put(ReceiptsBucket, receiptKey(e.Ticket, e.Hop), rawRec); err != nil {
		return err
	}
	delivered, _ := json.Marshal(DeliveredData{Blob: body.Blob, Size: body.Size})
	if err := s.append(e.Ticket, Event{Type: EvDelivered, Hop: e.Hop, Peer: sender, Data: delivered}); err != nil {
		return err
	}
	if err := s.append(e.Ticket, Event{Type: EvVerified, Hop: e.Hop, Peer: sender}); err != nil {
		return err
	}
	// 全部跳完成则关闭。留一拍给 driveOnce 也行，这里直接判掉更利落。
	st, _, err = s.Get(e.Ticket)
	if err == nil && st.Current() == nil {
		return s.append(e.Ticket, Event{Type: EvClosed})
	}
	return nil
}

func (s *Service) allowed(sender string) bool {
	for _, p := range s.cfg.AllowFrom {
		if p == sender {
			return true
		}
	}
	return false
}

func (s *Service) acceptEnvelope(to string, req *envelope.Envelope) (*envelope.Envelope, error) {
	out, err := envelope.New(envelope.TypeHopAccept, to, req.Ticket, req.Hop, time.Now().UnixMilli(), map[string]any{"ok": true})
	if err != nil {
		return nil, err
	}
	return out, out.Sign(s.id.Private)
}

func (s *Service) rejectEnvelope(to string, req *envelope.Envelope, reason string) (*envelope.Envelope, error) {
	out, err := envelope.New(envelope.TypeHopReject, to, req.Ticket, req.Hop, time.Now().UnixMilli(), RejectBody{Reason: reason})
	if err != nil {
		return nil, err
	}
	return out, out.Sign(s.id.Private)
}

func (s *Service) append(tid string, ev Event) error {
	seq, err := s.st.NextSeq(TicketsBucket)
	if err != nil {
		return err
	}
	ev.Seq = seq
	if ev.TS == 0 {
		ev.TS = time.Now().UnixMilli()
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return s.st.Put(TicketsBucket, eventKey(tid, seq), raw)
}

func eventKey(tid string, seq uint64) []byte {
	return []byte(fmt.Sprintf("%s|%016x", tid, seq))
}

func receiptKey(tid string, hop int) []byte {
	return []byte(fmt.Sprintf("%s|%d", tid, hop))
}

func jobKey(tid string, hop int) []byte {
	return []byte(fmt.Sprintf("%s|%d", tid, hop))
}

func findHop(st State, step int) (*HopState, error) {
	for i := range st.Hops {
		if st.Hops[i].Step == step {
			return &st.Hops[i], nil
		}
	}
	return nil, fmt.Errorf("工单 %s 没有第 %d 跳", st.ID, step)
}

func (s *Service) saveJob(j JobRecord) error {
	j.Updated = time.Now().UnixMilli()
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return s.st.Put(JobsBucket, jobKey(j.Ticket, j.Hop), raw)
}

func (s *Service) loadJobs() []JobRecord {
	var out []JobRecord
	s.st.ForEach(JobsBucket, func(_, v []byte) error {
		var j JobRecord
		if json.Unmarshal(v, &j) == nil {
			out = append(out, j)
		}
		return nil
	})
	return out
}

func (s *Service) resumeJobs() {
	for _, j := range s.loadJobs() {
		if j.Status != "running" {
			continue
		}
		j := j
		slog.Info("恢复未完成的作业", "ticket", j.Ticket, "hop", j.Hop)
		go s.runJob(j.Ticket, j.Hop, j.From, s.formOf(j.Ticket))
	}
}

// formOf 取工单表单；取不到返回空（作业会以拒绝收场）。
func (s *Service) formOf(tid string) json.RawMessage {
	st, _, err := s.Get(tid)
	if err != nil {
		return nil
	}
	return st.Form
}
