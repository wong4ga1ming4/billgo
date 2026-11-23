// Package audit 把远程控制请求记成本地 JSONL。
//
// 允许和拒绝都记。文件按大小轮转，避免只读查询把数据目录写满。
package audit

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const (
	fileName    = "audit.jsonl"
	maxBytes    = 8 << 20
	keep        = 3
	defaultList = 50
	maxList     = 200
)

// Record 一行审计。Result 只有 allow 和 deny。
type Record struct {
	TS         int64  `json:"ts"`
	From       string `json:"from"`
	Action     string `json:"action"`
	Target     string `json:"target"`
	Result     string `json:"result"`
	EnvelopeID string `json:"envelope_id"`
}

// Log 一个节点的审计日志。
type Log struct {
	path string
	max  int64
	keep int
	mu   sync.Mutex
}

// Open 打开 dataDir/audit.jsonl，没有就等第一次写入时创建。
func Open(dataDir string) (*Log, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("建审计目录: %w", err)
	}
	return &Log{
		path: filepath.Join(dataDir, fileName),
		max:  maxBytes,
		keep: keep,
	}, nil
}

// Path 日志文件路径。
func (l *Log) Path() string { return l.path }

// SetLimits 测试时缩小轮转阈值。
func (l *Log) SetLimits(max int64, keep int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if max > 0 {
		l.max = max
	}
	if keep > 0 {
		l.keep = keep
	}
}

// Write 追加一行。超过大小上限时把当前文件轮转走。
func (l *Log) Write(rec Record) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	if fi, err := os.Stat(l.path); err == nil && fi.Size()+int64(len(line)) > l.max {
		l.rotate()
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("打开审计日志: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("写审计日志: %w", err)
	}
	return nil
}

// List 按时间倒序返回本机审计。cursor 指向上一页最后一条，新写入的记录出现在第一页。
// 只读当前文件和仍保留的轮转文件，不把日志暴露给远程调用。
func (l *Log) List(limit int, cursor string) ([]Record, string, error) {
	if limit <= 0 {
		limit = defaultList
	}
	if limit > maxList {
		limit = maxList
	}
	all, err := l.readNewest()
	if err != nil {
		return nil, "", err
	}
	start := 0
	if cursor != "" {
		ts, id, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		start = -1
		for i, rec := range all {
			if rec.TS == ts && rec.EnvelopeID == id {
				start = i + 1
				break
			}
		}
		if start < 0 {
			return nil, "", fmt.Errorf("游标无效")
		}
	}
	if start > len(all) {
		start = len(all)
	}
	end := start + limit
	if end > len(all) {
		end = len(all)
	}
	items := append([]Record(nil), all[start:end]...)
	next := ""
	if end < len(all) && len(items) > 0 {
		last := items[len(items)-1]
		next = encodeCursor(last.TS, last.EnvelopeID)
	}
	return items, next, nil
}

func (l *Log) readNewest() ([]Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	paths := []string{l.path}
	for i := 1; i <= l.keep; i++ {
		paths = append(paths, fmt.Sprintf("%s.%d", l.path, i))
	}
	var out []Record
	for _, path := range paths {
		recs, err := readFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for i := len(recs) - 1; i >= 0; i-- {
			out = append(out, recs[i])
		}
	}
	return out, nil
}

func readFile(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := bytesTrim(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec Record
		if json.Unmarshal(line, &rec) == nil {
			out = append(out, rec)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读审计日志: %w", err)
	}
	return out, nil
}

func bytesTrim(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func encodeCursor(ts int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(ts, 10) + "|" + id))
}

func decodeCursor(cursor string) (int64, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, "", fmt.Errorf("游标无效")
	}
	tsText, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return 0, "", fmt.Errorf("游标无效")
	}
	ts, err := strconv.ParseInt(tsText, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("游标无效")
	}
	return ts, id, nil
}

func (l *Log) rotate() {
	_ = os.Remove(fmt.Sprintf("%s.%d", l.path, l.keep))
	for i := l.keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", l.path, i), fmt.Sprintf("%s.%d", l.path, i+1))
	}
	_ = os.Rename(l.path, l.path+".1")
}
