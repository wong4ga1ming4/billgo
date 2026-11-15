// Package envelope 定义 Agent 之间唯一的消息形态：签名信封。
//
// 传输层只是载体，信封本体才是协议。签名覆盖除 sig 外的全部字段，
// 签名对象是 Canonicalize 输出的规范字节。
package envelope

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"billgo/internal/identity"
)

// Version 当前协议版本。
const Version = 1

// 消息类型。见《全流程设计.md》第 5 节。
const (
	TypePeersRecord = "peers.record"
	TypePeersDigest = "peers.digest"
	TypePeersSync   = "peers.sync"
	TypeHopIssue    = "hop.issue"
	TypeHopAccept   = "hop.accept"
	TypeHopReject   = "hop.reject"
	TypeHopDeliver  = "hop.deliver"
)

// 可分类的错误，调用方用 errors.Is 判断后决定重试还是丢弃。
var (
	ErrVersion    = errors.New("协议版本不支持")
	ErrSignature  = errors.New("签名无效")
	ErrSender     = errors.New("发送者公钥与签名不符")
	ErrRecipient  = errors.New("收件人不是本节点")
	ErrStale      = errors.New("消息时间超出窗口")
	ErrMalformed  = errors.New("信封格式错误")
	ErrBodyAbsent = errors.New("缺少 body")
)

// Envelope 所有消息共用的外壳。
type Envelope struct {
	V      int             `json:"v"`
	Type   string          `json:"type"`
	From   string          `json:"from"`             // 发送者完整公钥，base64
	To     string          `json:"to,omitempty"`     // 接收者 peer_id；名录广播留空
	Ticket string          `json:"ticket,omitempty"` // 工单号，仅 hop.*
	Hop    int             `json:"hop,omitempty"`    // 跳号，仅 hop.*
	TS     int64           `json:"ts"`               // 毫秒
	Body   json.RawMessage `json:"body,omitempty"`
	Sig    string          `json:"sig,omitempty"` // base64
}

// New 构造一个未签名的信封。body 会被序列化进 Body。
func New(typ, to, ticket string, hop int, ts int64, body any) (*Envelope, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("序列化 body: %w", err)
	}
	return &Envelope{
		V:      Version,
		Type:   typ,
		To:     to,
		Ticket: ticket,
		Hop:    hop,
		TS:     ts,
		Body:   raw,
	}, nil
}

// Sign 用私钥签名，并填入 From 与 Sig。
func (e *Envelope) Sign(priv ed25519.PrivateKey) error {
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return ErrMalformed
	}
	e.From = base64.StdEncoding.EncodeToString(pub)
	sig, err := signBytes(e, priv)
	if err != nil {
		return err
	}
	e.Sig = base64.StdEncoding.EncodeToString(sig)
	return nil
}

// VerifySignature 校验签名，返回发送者的 peer_id。
func (e *Envelope) VerifySignature() (string, error) {
	if e.V != Version {
		return "", fmt.Errorf("%w: %d", ErrVersion, e.V)
	}
	pub, err := base64.StdEncoding.DecodeString(e.From)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return "", ErrSender
	}
	sig, err := base64.StdEncoding.DecodeString(e.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return "", fmt.Errorf("%w: 签名长度或编码不对", ErrSignature)
	}
	canon, err := signingBytes(e)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), canon, sig) {
		return "", ErrSignature
	}
	return identity.Fingerprint(ed25519.PublicKey(pub))[:identity.PeerIDLen], nil
}

// CheckAddress 校验收件人。名录广播的 To 为空，不做校验。
func (e *Envelope) CheckAddress(self string) error {
	if e.To == "" {
		return nil
	}
	if e.To != self {
		return fmt.Errorf("%w: to=%s self=%s", ErrRecipient, e.To, self)
	}
	return nil
}

// CheckFresh 只对 hop.* 生效：消息必须落在时间窗口内，防止重放。
// 名录消息可能是被转发的旧记录，靠签名和版本号判断，不看时间。
func (e *Envelope) CheckFresh(now time.Time, window time.Duration) error {
	if !isHop(e.Type) {
		return nil
	}
	dt := now.UnixMilli() - e.TS
	if dt < 0 {
		dt = -dt
	}
	if dt > window.Milliseconds() {
		return fmt.Errorf("%w: 偏差 %d ms", ErrStale, dt)
	}
	return nil
}

// IdempotencyKey 返回幂等键；第二个返回值表示该消息是否需要去重。
//
// 工单消息用 (ticket, hop, type)；名录记录用 (peer_id, version)。
// digest/sync 是只读的，不去重。
func (e *Envelope) IdempotencyKey() (string, bool, error) {
	switch e.Type {
	case TypeHopIssue, TypeHopAccept, TypeHopReject, TypeHopDeliver:
		if e.Ticket == "" || e.Hop <= 0 {
			return "", false, fmt.Errorf("%w: 工单消息缺少 ticket/hop", ErrMalformed)
		}
		return fmt.Sprintf("hop|%s|%d|%s", e.Ticket, e.Hop, e.Type), true, nil
	case TypePeersRecord:
		var body struct {
			Version uint64 `json:"version"`
		}
		if err := json.Unmarshal(e.Body, &body); err != nil {
			return "", false, fmt.Errorf("%w: %v", ErrMalformed, err)
		}
		if e.From == "" {
			return "", false, ErrSender
		}
		pub, err := base64.StdEncoding.DecodeString(e.From)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			return "", false, ErrSender
		}
		owner := identity.Fingerprint(ed25519.PublicKey(pub))[:identity.PeerIDLen]
		return fmt.Sprintf("peer|%s|%d", owner, body.Version), true, nil
	default:
		return "", false, nil
	}
}

// Marshal 返回线上字节，body 与调用方写法无关（已规范化）。
func (e *Envelope) Marshal() ([]byte, error) {
	canon, err := signingBytes(e)
	if err != nil {
		return nil, err
	}
	// 线上格式 = 规范体 + sig。规范化在签名时就已确定，避免两端字节不一致。
	var out map[string]any
	if err := decode(canon, &out); err != nil {
		return nil, err
	}
	if e.Sig != "" {
		out["sig"] = e.Sig
	}
	return json.Marshal(out)
}

// Unmarshal 严格解析线上字节。
func Unmarshal(raw []byte) (*Envelope, error) {
	var e Envelope
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if e.Type == "" {
		return nil, fmt.Errorf("%w: 缺少 type", ErrMalformed)
	}
	if e.TS <= 0 {
		return nil, fmt.Errorf("%w: 缺少 ts", ErrMalformed)
	}
	return &e, nil
}

func signBytes(e *Envelope, priv ed25519.PrivateKey) ([]byte, error) {
	canon, err := signingBytes(e)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(priv, canon), nil
}

// signingBytes 把信封转成签名用的 map，不含 sig。
func signingBytes(e *Envelope) ([]byte, error) {
	return Canonicalize(signaturePayloadMap(e))
}

func signaturePayloadMap(e *Envelope) map[string]any {
	m := map[string]any{
		"v":    e.V,
		"type": e.Type,
		"from": e.From,
		"ts":   e.TS,
	}
	if e.To != "" {
		m["to"] = e.To
	}
	if e.Ticket != "" {
		m["ticket"] = e.Ticket
	}
	if e.Hop != 0 {
		m["hop"] = e.Hop
	}
	if len(e.Body) > 0 {
		m["body"] = e.Body
	}
	return m
}

func isHop(typ string) bool {
	switch typ {
	case TypeHopIssue, TypeHopAccept, TypeHopReject, TypeHopDeliver:
		return true
	}
	return false
}
