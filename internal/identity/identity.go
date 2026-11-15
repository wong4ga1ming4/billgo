// Package identity 负责节点的 ed25519 身份。
//
// 同一把密钥复用三处：签名信封、TLS 自签证书、SSH 唤醒凭证。
// peer_id 是公钥指纹，不是任何中心签发的名字。
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// PeerIDLen 是 peer_id 的字符数，取完整指纹的前 16 位。
const PeerIDLen = 16

// Identity 一把密钥 + 由它推导出的两个标识。
type Identity struct {
	Private     ed25519.PrivateKey
	Public      ed25519.PublicKey
	PeerID      string // 指纹前 16 位，小写，唯一键
	Fingerprint string // 完整指纹，小写
}

// LoadOrCreate 从 path 读种子；不存在就生成并落盘（0600）。
func LoadOrCreate(path string) (*Identity, error) {
	seed, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("私钥文件 %s 长度 %d，期望 %d", path, len(seed), ed25519.SeedSize)
		}
	case errors.Is(err, fs.ErrNotExist):
		seed = make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, fmt.Errorf("生成私钥: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("建数据目录: %w", err)
		}
		if err := os.WriteFile(path, seed, 0o600); err != nil {
			return nil, fmt.Errorf("落盘私钥: %w", err)
		}
	default:
		return nil, fmt.Errorf("读私钥 %s: %w", path, err)
	}
	return FromSeed(seed), nil
}

// FromSeed 由 32 字节种子推导身份。
func FromSeed(seed []byte) *Identity {
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	fp := Fingerprint(pub)
	return &Identity{
		Private:     priv,
		Public:      pub,
		PeerID:      fp[:PeerIDLen],
		Fingerprint: fp,
	}
}

// Fingerprint 返回公钥的完整指纹：base32(sha256(pubkey))，小写、无填充。
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return toLower(enc)
}

func toLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
