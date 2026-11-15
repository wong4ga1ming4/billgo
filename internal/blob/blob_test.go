package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

func TestPutVerify(t *testing.T) {
	dir := t.TempDir()
	data := []byte("hello deliverable")
	hash, size, err := PutBytes(dir, data)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if hash != hex.EncodeToString(sum[:]) {
		t.Fatalf("哈希不对: %s", hash)
	}
	if size != int64(len(data)) {
		t.Fatalf("尺寸 %d", size)
	}
	if err := Verify(dir, hash); err != nil {
		t.Fatal(err)
	}
	// 相同内容只存一份
	if _, _, err := PutBytes(dir, data); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("同内容应只留一个文件，实际 %d", len(entries))
	}
}

func TestVerifyDetectsTamper(t *testing.T) {
	dir := t.TempDir()
	hash, _, err := PutBytes(dir, []byte("original"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(dir, hash), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(dir, hash); err == nil {
		t.Fatal("内容被改后校验应失败")
	}
}

func TestValidHash(t *testing.T) {
	if !ValidHash("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef") {
		t.Fatal("合法哈希被判非法")
	}
	for _, bad := range []string{"", "xyz", "../etc/passwd", "0123"} {
		if ValidHash(bad) {
			t.Fatalf("非法哈希 %q 被判合法", bad)
		}
	}
}
