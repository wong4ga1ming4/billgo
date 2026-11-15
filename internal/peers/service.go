package peers

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"billgo/internal/config"
	"billgo/internal/envelope"
	"billgo/internal/identity"
	"billgo/internal/store"
	"billgo/internal/transport"
)

// Bucket 名录所在的桶。
var Bucket = []byte("peers")

// DigestBody 反熵摘要：peer_id → 已知版本。
type DigestBody struct {
	Versions map[string]uint64 `json:"versions"`
}

// SyncBody 反熵补齐：对方缺的或版本旧的记录，加上自己的版本视图。
type SyncBody struct {
	Versions map[string]uint64 `json:"versions"`
	Records  []json.RawMessage `json:"records"`
}

// Service 名录服务。每个节点各有一份完整副本。
type Service struct {
	id        *identity.Identity
	st        *store.Store
	client    *transport.Client
	network   string
	bootstrap []config.Bootstrap

	mu     sync.RWMutex
	own    Record
	ownEnv *envelope.Envelope
}

// ownSnapshot 返回自己的记录与信封。反熵和退出注销会并发访问。
func (s *Service) ownSnapshot() (Record, *envelope.Envelope) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.own, s.ownEnv
}

// New 构造名录服务，并确定自己的记录（内容没变就沿用旧版本）。
func New(id *identity.Identity, st *store.Store, client *transport.Client, cfg *config.Config) (*Service, error) {
	bs, err := cfg.ParsedBootstrap()
	if err != nil {
		return nil, err
	}
	s := &Service{id: id, st: st, client: client, network: cfg.Network, bootstrap: bs}

	desired := Record{
		PeerID:   id.PeerID,
		PubKey:   base64.StdEncoding.EncodeToString(id.Public),
		Name:     cfg.Name,
		Desc:     cfg.Desc,
		Kind:     cfg.Kind,
		Endpoint: cfg.AdvertisedEndpoint(),
		Wake:     cfg.Wake,
		Network:  cfg.Network,
	}
	if err := s.loadOwn(desired); err != nil {
		return nil, err
	}
	return s, nil
}

// loadOwn 读旧记录，内容有变化才加版本并重签。
func (s *Service) loadOwn(desired Record) error {
	if raw, err := s.st.Get(Bucket, []byte(desired.PeerID)); err != nil {
		return err
	} else if raw != nil {
		old, err := envelope.Unmarshal(raw)
		if err != nil {
			return fmt.Errorf("解析本地记录: %w", err)
		}
		oldRec, err := decodeRecord(old)
		if err != nil {
			return fmt.Errorf("解析本地记录: %w", err)
		}
		prevVersion := oldRec.Version
		desired.Version = prevVersion
		a, err := desired.ContentHash()
		if err != nil {
			return err
		}
		b, err := oldRec.ContentHash()
		if err != nil {
			return err
		}
		if a == b {
			s.own, s.ownEnv = oldRec, old
			return nil
		}
		desired.Version = prevVersion + 1
	} else {
		desired.Version = 1
	}
	if err := desired.Validate(); err != nil {
		return fmt.Errorf("自己的记录不合法: %w", err)
	}
	env, err := signRecord(s.id.Private, desired)
	if err != nil {
		return err
	}
	raw, err := env.Marshal()
	if err != nil {
		return err
	}
	if err := s.st.Put(Bucket, []byte(desired.PeerID), raw); err != nil {
		return err
	}
	s.own, s.ownEnv = desired, env
	slog.Info("更新自己的名录记录", "version", desired.Version, "name", desired.Name)
	return nil
}

// Own 返回自己的记录。
func (s *Service) Own() Record {
	own, _ := s.ownSnapshot()
	return own
}

// Lookup 按 peer_id 取一条有效记录（墓碑不算）。
func (s *Service) Lookup(peerID string) (Record, bool) {
	rec, ok := s.get(peerID)
	if !ok || rec.Deleted {
		return Record{}, false
	}
	return rec, true
}

// ByName 按别名找节点，可能返回多个重名。
func (s *Service) ByName(name string) []Record {
	var out []Record
	for _, r := range s.List() {
		if r.Name == name {
			out = append(out, r)
		}
	}
	return out
}

// List 返回所有有效记录（不含墓碑、不含自己）。
func (s *Service) List() []Record {
	all := s.ListAll()
	out := make([]Record, 0, len(all))
	for _, r := range all {
		if r.Deleted || r.PeerID == s.id.PeerID {
			continue
		}
		out = append(out, r)
	}
	return out
}

// ListAll 返回全部记录，含墓碑与自己。
func (s *Service) ListAll() []Record {
	var out []Record
	s.st.ForEach(Bucket, func(_, v []byte) error {
		e, err := envelope.Unmarshal(v)
		if err != nil {
			return nil
		}
		rec, err := decodeRecord(e)
		if err != nil {
			return nil
		}
		out = append(out, rec)
		return nil
	})
	return out
}

func (s *Service) get(peerID string) (Record, bool) {
	raw, err := s.st.Get(Bucket, []byte(peerID))
	if err != nil || raw == nil {
		return Record{}, false
	}
	e, err := envelope.Unmarshal(raw)
	if err != nil {
		return Record{}, false
	}
	rec, err := decodeRecord(e)
	if err != nil {
		return Record{}, false
	}
	return rec, true
}

func (s *Service) envelopeOf(peerID string) ([]byte, bool) {
	raw, err := s.st.Get(Bucket, []byte(peerID))
	if err != nil || raw == nil {
		return nil, false
	}
	return raw, true
}

// Bootstrap 从引导节点各拉一次全量名录。
func (s *Service) Bootstrap(ctx context.Context) {
	for _, b := range s.bootstrap {
		if err := s.syncWithEndpoint(ctx, b.Endpoint, b.PeerID); err != nil {
			slog.Warn("引导节点拉取失败", "endpoint", b.Endpoint, "peer", b.PeerID, "err", err)
			continue
		}
		slog.Info("已从引导节点拉取名录", "peer", b.PeerID, "records", len(s.ListAll()))
	}
}

// Broadcast 把自己的记录推给所有已知节点，尽力而为。
func (s *Service) Broadcast(ctx context.Context) {
	_, ownEnv := s.ownSnapshot()
	for _, r := range s.List() {
		if _, err := s.client.Send(ctx, r.Endpoint, r.PeerID, ownEnv); err != nil {
			slog.Debug("广播记录失败", "peer", r.PeerID, "err", err)
		}
	}
}

// Loop 周期性反熵，直到 ctx 结束。
func (s *Service) Loop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.antiEntropyOnce(ctx)
		}
	}
}

func (s *Service) antiEntropyOnce(ctx context.Context) {
	peers := s.List()
	if len(peers) == 0 {
		return
	}
	r := peers[rand.Intn(len(peers))]
	if err := s.syncWithEndpoint(ctx, r.Endpoint, r.PeerID); err != nil {
		slog.Debug("反熵失败", "peer", r.PeerID, "err", err)
	}
}

// syncWithEndpoint 一轮反熵：发摘要，收补齐，必要时把自己的记录推给对方。
func (s *Service) syncWithEndpoint(ctx context.Context, endpoint, peerID string) error {
	body := DigestBody{Versions: s.Versions()}
	req, err := envelope.New(envelope.TypePeersDigest, peerID, "", 0, time.Now().UnixMilli(), body)
	if err != nil {
		return err
	}
	if err := req.Sign(s.id.Private); err != nil {
		return err
	}
	resp, err := s.client.Send(ctx, endpoint, peerID, req)
	if err != nil {
		return err
	}
	if resp == nil {
		return fmt.Errorf("对端没有返回摘要")
	}
	var sb SyncBody
	if err := json.Unmarshal(resp.Body, &sb); err != nil {
		return fmt.Errorf("解析补齐结果: %w", err)
	}
	for _, raw := range sb.Records {
		e, err := envelope.Unmarshal(raw)
		if err != nil {
			continue
		}
		sender, err := e.VerifySignature()
		if err != nil {
			continue
		}
		if err := s.apply(sender, e); err != nil {
			slog.Debug("丢弃记录", "err", err)
		}
	}
	own, ownEnv := s.ownSnapshot()
	if v, ok := sb.Versions[s.id.PeerID]; !ok || v < own.Version {
		if _, err := s.client.Send(ctx, endpoint, peerID, ownEnv); err != nil {
			return fmt.Errorf("推送自己的记录: %w", err)
		}
	}
	return nil
}

// Versions 返回本地全部记录的版本视图。
func (s *Service) Versions() map[string]uint64 {
	out := map[string]uint64{}
	for _, r := range s.ListAll() {
		out[r.PeerID] = r.Version
	}
	return out
}

// HandleRecord 处理一条广播来的记录。transport 已验过外层签名。
func (s *Service) HandleRecord(sender string, e *envelope.Envelope) error {
	return s.apply(sender, e)
}

// HandleDigest 回应反熵摘要：返回对方缺的或版本旧的记录。
func (s *Service) HandleDigest(sender string, e *envelope.Envelope) (*envelope.Envelope, error) {
	var req DigestBody
	if err := json.Unmarshal(e.Body, &req); err != nil {
		return nil, fmt.Errorf("%w: %v", envelope.ErrMalformed, err)
	}
	versions := s.Versions()
	resp := SyncBody{Versions: versions}
	for pid, v := range versions {
		if req.Versions[pid] >= v {
			continue
		}
		if raw, ok := s.envelopeOf(pid); ok {
			resp.Records = append(resp.Records, raw)
		}
	}
	out, err := envelope.New(envelope.TypePeersSync, sender, "", 0, time.Now().UnixMilli(), resp)
	if err != nil {
		return nil, err
	}
	if err := out.Sign(s.id.Private); err != nil {
		return nil, err
	}
	return out, nil
}

// HandleSync 处理一次补齐响应（也用于主动推送的接收方）。
func (s *Service) HandleSync(sender string, e *envelope.Envelope) error {
	var sb SyncBody
	if err := json.Unmarshal(e.Body, &sb); err != nil {
		return fmt.Errorf("%w: %v", envelope.ErrMalformed, err)
	}
	for _, raw := range sb.Records {
		inner, err := envelope.Unmarshal(raw)
		if err != nil {
			continue
		}
		who, err := inner.VerifySignature()
		if err != nil {
			continue
		}
		if err := s.apply(who, inner); err != nil {
			slog.Debug("丢弃记录", "err", err)
		}
	}
	return nil
}

// apply 落一条记录：身份、域、版本冲突都在这裁决。
func (s *Service) apply(sender string, e *envelope.Envelope) error {
	rec, err := decodeRecord(e)
	if err != nil {
		return err
	}
	if rec.PeerID != sender {
		return fmt.Errorf("%w: 记录说是 %s，签名者是 %s", envelope.ErrSender, rec.PeerID, sender)
	}
	if rec.Network != s.network {
		return nil // 别的域，静默丢弃
	}
	if rec.PeerID == s.id.PeerID {
		return nil
	}

	old, ok := s.get(rec.PeerID)
	if ok {
		if rec.Version < old.Version {
			return nil
		}
		if rec.Version == old.Version {
			ha, err := rec.ContentHash()
			if err != nil {
				return err
			}
			hb, err := old.ContentHash()
			if err != nil {
				return err
			}
			if ha == hb {
				return nil
			}
			slog.Warn("同版本不同内容的名录记录", "peer", rec.PeerID, "version", rec.Version)
			if hb > ha {
				return nil
			}
		}
	}
	raw, err := e.Marshal()
	if err != nil {
		return err
	}
	if err := s.st.Put(Bucket, []byte(rec.PeerID), raw); err != nil {
		return err
	}
	slog.Debug("名录更新", "peer", rec.PeerID, "name", rec.Name, "version", rec.Version, "deleted", rec.Deleted)
	return nil
}

// Withdraw 注销自己：发一条墓碑记录。
func (s *Service) Withdraw(ctx context.Context) error {
	s.mu.Lock()
	own := s.own
	own.Deleted = true
	own.Version++
	env, err := signRecord(s.id.Private, own)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	raw, err := env.Marshal()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if err := s.st.Put(Bucket, []byte(own.PeerID), raw); err != nil {
		s.mu.Unlock()
		return err
	}
	s.own, s.ownEnv = own, env
	s.mu.Unlock()
	s.Broadcast(ctx)
	return nil
}

func signRecord(priv ed25519.PrivateKey, rec Record) (*envelope.Envelope, error) {
	e, err := envelope.New(envelope.TypePeersRecord, "", "", 0, time.Now().UnixMilli(), rec)
	if err != nil {
		return nil, err
	}
	if err := e.Sign(priv); err != nil {
		return nil, err
	}
	return e, nil
}
