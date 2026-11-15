// Package peers 维护每个节点本地的完整名录：自签名记录、广播、反熵、墓碑。
package peers

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"billgo/internal/config"
	"billgo/internal/envelope"
)

// Record 是名录里的一行。它由主人签名（外层信封的签名），转发者原样转发。
type Record struct {
	PeerID   string       `json:"peer_id"`
	PubKey   string       `json:"pubkey"` // 完整公钥，base64
	Name     string       `json:"name"`
	Desc     string       `json:"desc"`
	Kind     string       `json:"kind"`
	Endpoint string       `json:"endpoint"`
	Wake     *config.Wake `json:"wake,omitempty"`
	Network  string       `json:"network"`
	Version  uint64       `json:"version"`
	Deleted  bool         `json:"deleted,omitempty"`
}

// Validate 校验一条记录自身是否完整。
func (r *Record) Validate() error {
	if r.PeerID == "" {
		return fmt.Errorf("%w: 缺少 peer_id", envelope.ErrMalformed)
	}
	pub, err := base64.StdEncoding.DecodeString(r.PubKey)
	if err != nil || len(pub) != 32 {
		return fmt.Errorf("%w: 公钥无效", envelope.ErrSender)
	}
	switch r.Kind {
	case config.KindResident, config.KindWakeable:
	default:
		return fmt.Errorf("%w: kind=%q", envelope.ErrMalformed, r.Kind)
	}
	if r.Network == "" {
		return fmt.Errorf("%w: 缺少 network", envelope.ErrMalformed)
	}
	if r.Version == 0 {
		return fmt.Errorf("%w: version 必须从 1 开始", envelope.ErrMalformed)
	}
	return nil
}

// ContentHash 返回记录内容的哈希，忽略版本号，用于同版本冲突时裁决。
func (r *Record) ContentHash() (string, error) {
	c := *r
	c.Version = 0
	canon, err := envelope.Canonicalize(c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}

// decodeRecord 从信封 body 解出记录。
func decodeRecord(e *envelope.Envelope) (Record, error) {
	var r Record
	if err := json.Unmarshal(e.Body, &r); err != nil {
		return r, fmt.Errorf("%w: %v", envelope.ErrMalformed, err)
	}
	if err := r.Validate(); err != nil {
		return r, err
	}
	return r, nil
}
