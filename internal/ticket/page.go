package ticket

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	// DefaultPageLimit 远程工单列表的默认页大小。
	DefaultPageLimit = 20
	// MaxPageLimit 服务端接受的最大页大小。
	MaxPageLimit = 100
)

// NormalizeLimit 把调用方给的 limit 收进默认值和上限之间。
func NormalizeLimit(n int) int {
	if n <= 0 {
		return DefaultPageLimit
	}
	if n > MaxPageLimit {
		return MaxPageLimit
	}
	return n
}

// ListPage 按更新时间倒序分页。游标不透明，内容是上一页最后一条的 (updated_at, id)。
func (s *Service) ListPage(limit int, cursor string) ([]State, string, error) {
	return pageStates(s.List(), limit, cursor)
}

func pageStates(all []State, limit int, cursor string) ([]State, string, error) {
	sorted := append([]State(nil), all...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].UpdatedAt != sorted[j].UpdatedAt {
			return sorted[i].UpdatedAt > sorted[j].UpdatedAt
		}
		return sorted[i].ID > sorted[j].ID
	})
	start := 0
	if cursor != "" {
		ts, id, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		start = sort.Search(len(sorted), func(i int) bool {
			st := sorted[i]
			return st.UpdatedAt < ts || (st.UpdatedAt == ts && st.ID < id)
		})
	}
	limit = NormalizeLimit(limit)
	items := []State{}
	if start < len(sorted) {
		end := start + limit
		if end > len(sorted) {
			end = len(sorted)
		}
		items = sorted[start:end]
	}
	next := ""
	if start+len(items) < len(sorted) && len(items) > 0 {
		last := items[len(items)-1]
		next = encodeCursor(last.UpdatedAt, last.ID)
	}
	return items, next, nil
}

func encodeCursor(updatedAt int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(updatedAt, 10) + "|" + id))
}

func decodeCursor(cursor string) (int64, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, "", fmt.Errorf("游标无效")
	}
	tsText, id, ok := strings.Cut(string(raw), "|")
	if !ok || id == "" {
		return 0, "", fmt.Errorf("游标无效")
	}
	ts, err := strconv.ParseInt(tsText, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("游标无效")
	}
	return ts, id, nil
}
