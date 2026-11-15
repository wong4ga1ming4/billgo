package identity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreatePersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "identity.key")

	a, err := LoadOrCreate(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreate(p)
	if err != nil {
		t.Fatal(err)
	}
	if a.PeerID != b.PeerID || a.Fingerprint != b.Fingerprint {
		t.Fatalf("重载后身份变了: %s != %s", a.PeerID, b.PeerID)
	}
	if len(a.PeerID) != PeerIDLen {
		t.Fatalf("peer_id 长度 %d，期望 %d", len(a.PeerID), PeerIDLen)
	}
	if !strings.HasPrefix(a.Fingerprint, a.PeerID) {
		t.Fatalf("peer_id 应该是完整指纹的前缀: %s / %s", a.PeerID, a.Fingerprint)
	}
	if a.PeerID != strings.ToLower(a.PeerID) {
		t.Fatalf("peer_id 应为小写: %s", a.PeerID)
	}
}

func TestDifferentSeedsDifferentIDs(t *testing.T) {
	a := FromSeed(make([]byte, 32))
	seed := make([]byte, 32)
	seed[0] = 1
	b := FromSeed(seed)
	if a.PeerID == b.PeerID {
		t.Fatalf("不同种子得到同一个 peer_id: %s", a.PeerID)
	}
}

func TestKeyFileMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "identity.key")
	if _, err := LoadOrCreate(p); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("私钥权限 %o，期望 600", perm)
	}
}

func TestBadKeyLength(t *testing.T) {
	p := filepath.Join(t.TempDir(), "identity.key")
	if err := os.WriteFile(p, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(p); err == nil {
		t.Fatal("长度不对的私钥文件应该报错")
	}
}
