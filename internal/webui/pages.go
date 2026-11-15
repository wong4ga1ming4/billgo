package webui

import (
	"encoding/json"
	"html"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type pageInfo struct {
	Path    string `json:"path"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	Desc    string `json:"desc,omitempty"`
	Updated int64  `json:"updated"`
}

var titleRE = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

func (s *Server) apiPages(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.discoverPages())
}

// discoverPages 扫描 pages/：忽略 _ 和 . 开头、跳过符号链接，
// 标题优先取同名 .json，其次 <title>，最后用文件名。
func (s *Server) discoverPages() []pageInfo {
	out := []pageInfo{}
	_ = filepath.WalkDir(s.pages, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == s.pages {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(name), ".html") {
			return nil
		}
		rel, err := filepath.Rel(s.pages, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return nil
		}
		title, desc := pageMeta(path)
		if title == "" {
			title = strings.TrimSuffix(name, filepath.Ext(name))
		}
		out = append(out, pageInfo{
			Path:    rel,
			URL:     "/pages/" + rel,
			Title:   title,
			Desc:    desc,
			Updated: info.ModTime().UnixMilli(),
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Updated == out[j].Updated {
			return out[i].Path < out[j].Path
		}
		return out[i].Updated > out[j].Updated
	})
	return out
}

func pageMeta(path string) (title, desc string) {
	side := strings.TrimSuffix(path, filepath.Ext(path)) + ".json"
	if raw, err := os.ReadFile(side); err == nil {
		var m struct {
			Title string `json:"title"`
			Desc  string `json:"desc"`
		}
		if json.Unmarshal(raw, &m) == nil {
			title = strings.TrimSpace(m.Title)
			desc = strings.TrimSpace(m.Desc)
		}
	}
	if title != "" {
		return title, desc
	}
	f, err := os.Open(path)
	if err != nil {
		return "", desc
	}
	defer f.Close()
	raw, _ := io.ReadAll(io.LimitReader(f, 64<<10))
	if m := titleRE.FindSubmatch(raw); m != nil {
		title = strings.TrimSpace(html.UnescapeString(string(m[1])))
	}
	return title, desc
}
