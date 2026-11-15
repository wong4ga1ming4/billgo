package store

import (
	"encoding/binary"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestOpenCreatesBuckets(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	err = s.db.View(func(tx *bolt.Tx) error {
		for _, name := range buckets {
			if tx.Bucket([]byte(name)) == nil {
				t.Errorf("缺桶 %s", name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestReopenKeepsSchema(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("同版本重开应成功: %v", err)
	}
	s2.Close()
	if err := s2.Close(); err != nil {
		t.Fatalf("重复 Close 应为空操作: %v", err)
	}
}

func TestSchemaMismatchRejected(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = s.db.Update(func(tx *bolt.Tx) error {
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], SchemaVersion+1)
		return tx.Bucket([]byte("meta")).Put([]byte("schema"), buf[:])
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	if _, err := Open(dir); err == nil {
		t.Fatal("schema 版本不一致时应拒绝启动")
	}
}

func TestBlobDir(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, want := s.BlobDir(), dir+"/blobs"; got != want {
		t.Fatalf("blob 目录 %s，期望 %s", got, want)
	}
}
