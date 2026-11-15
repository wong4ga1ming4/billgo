// Package blob 按内容寻址存交付物：哈希即文件名，重复内容自然只存一份。
package blob

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidHash 判断是不是合法的 sha256 十六进制。
func ValidHash(h string) bool { return hashRE.MatchString(h) }

// Path 返回哈希对应的文件路径。
func Path(dir, hash string) string { return filepath.Join(dir, hash) }

// Put 写入内容，返回哈希与字节数。先写临时文件再改名，避免半截文件。
func Put(dir string, r io.Reader) (string, int64, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, fmt.Errorf("建 blobs 目录: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", 0, fmt.Errorf("建临时文件: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	closeErr := tmp.Close()
	if err != nil {
		return "", 0, fmt.Errorf("写交付物: %w", err)
	}
	if closeErr != nil {
		return "", 0, fmt.Errorf("关闭交付物: %w", closeErr)
	}
	hash := hex.EncodeToString(h.Sum(nil))
	if err := os.Rename(tmpName, Path(dir, hash)); err != nil {
		return "", 0, fmt.Errorf("落盘交付物: %w", err)
	}
	return hash, n, nil
}

// PutBytes 写一段字节。
func PutBytes(dir string, b []byte) (string, int64, error) {
	return Put(dir, bytes.NewReader(b))
}

// Verify 核对已有文件的内容与哈希一致。
func Verify(dir, hash string) error {
	f, err := os.Open(Path(dir, hash))
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != hash {
		return fmt.Errorf("交付物哈希不符: 期望 %s，实际 %s", hash, got)
	}
	return nil
}
