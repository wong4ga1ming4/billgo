// Package store 是节点的本地存储：一个 bbolt 文件加一个 blobs 目录。
//
// 所有节点的库结构完全一样，角色差异只由配置和用法决定。
// 桶名与 schema 版本一旦发布就不再改含义，升级走版本号迁移。
package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// ErrClosed 库已关闭。后台作业可能在节点退出后仍尝试写库，必须返回错误而不是崩溃。
var ErrClosed = errors.New("store 已关闭")

// SchemaVersion 当前库结构版本。结构不兼容时拒绝启动，避免悄悄写坏。
const SchemaVersion = 1

// 桶名。语义见《全流程设计.md》第 4 节。
var buckets = []string{
	"peers",    // 名录：peer 记录 + 墓碑
	"tickets",  // 工单事件日志（追加写）
	"jobs",     // 接活侧的作业状态
	"receipts", // 签名回执
	"outbox",   // 待发出的信封
	"dedupe",   // 幂等键 → 上次响应
	"meta",     // schema 版本等
}

// Store 一个打开的库。
type Store struct {
	mu     sync.RWMutex
	db     *bolt.DB
	path   string
	closed bool
}

func (s *Store) handle() (*bolt.DB, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.db == nil {
		return nil, ErrClosed
	}
	return s.db, nil
}

// Open 打开（或创建）数据目录下的 billgo.db。
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("建数据目录 %s: %w", dataDir, err)
	}
	path := filepath.Join(dataDir, "billgo.db")
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("打开 %s: %w", path, err)
	}
	s := &Store{db: db, path: path}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		for _, name := range buckets {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return fmt.Errorf("建桶 %s: %w", name, err)
			}
		}
		meta := tx.Bucket([]byte("meta"))
		raw := meta.Get([]byte("schema"))
		if raw == nil {
			var buf [8]byte
			binary.BigEndian.PutUint64(buf[:], SchemaVersion)
			return meta.Put([]byte("schema"), buf[:])
		}
		if got := binary.BigEndian.Uint64(raw); got != SchemaVersion {
			return fmt.Errorf(
				"库 schema 版本 %d，程序期望 %d（%s）: 需要迁移或换数据目录",
				got, SchemaVersion, s.path)
		}
		return nil
	})
}

// Path 返回库文件路径。
func (s *Store) Path() string { return s.path }

// Close 关闭库。可重复调用，第二次是空操作。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// BlobDir 交付物按内容寻址存放的目录。
func (s *Store) BlobDir() string {
	return filepath.Join(filepath.Dir(s.path), "blobs")
}

// Get 读一个键；不存在返回 nil, nil。
func (s *Store) Get(bucket, key []byte) ([]byte, error) {
	db, err := s.handle()
	if err != nil {
		return nil, err
	}
	var out []byte
	err = db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucket).Get(key)
		if v == nil {
			return nil
		}
		out = append([]byte(nil), v...)
		return nil
	})
	return out, err
}

// Put 写一个键。
func (s *Store) Put(bucket, key, val []byte) error {
	db, err := s.handle()
	if err != nil {
		return err
	}
	return db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Put(key, val)
	})
}

// Delete 删一个键。
func (s *Store) Delete(bucket, key []byte) error {
	db, err := s.handle()
	if err != nil {
		return err
	}
	return db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Delete(key)
	})
}

// ForEach 顺序遍历一个桶。fn 返回错误则中止。
func (s *Store) ForEach(bucket []byte, fn func(k, v []byte) error) error {
	db, err := s.handle()
	if err != nil {
		return err
	}
	return db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).ForEach(fn)
	})
}

// ForEachPrefix 只遍历以 prefix 开头的键。键按字节序排列，工单事件因此不必扫全表。
func (s *Store) ForEachPrefix(bucket, prefix []byte, fn func(k, v []byte) error) error {
	db, err := s.handle()
	if err != nil {
		return err
	}
	return db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			if err := fn(k, v); err != nil {
				return err
			}
		}
		return nil
	})
}

// Update 提供事务入口，供需要原子多键写的服务使用。
func (s *Store) Update(fn func(tx *bolt.Tx) error) error {
	db, err := s.handle()
	if err != nil {
		return err
	}
	return db.Update(fn)
}

// View 提供只读事务入口。
func (s *Store) View(fn func(tx *bolt.Tx) error) error {
	db, err := s.handle()
	if err != nil {
		return err
	}
	return db.View(fn)
}

// NextSeq 返回桶内自增序列，用作追加日志的序号。
func (s *Store) NextSeq(bucket []byte) (uint64, error) {
	db, err := s.handle()
	if err != nil {
		return 0, err
	}
	var id uint64
	err = db.Update(func(tx *bolt.Tx) error {
		n, err := tx.Bucket(bucket).NextSequence()
		id = n
		return err
	})
	return id, err
}
