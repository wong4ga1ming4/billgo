package webui

import (
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func (s *Server) builtin(name string) http.HandlerFunc {
	raw := s.pageFS[name]
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Security-Policy", builtinCSP)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(raw)
	}
}

func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}
	raw, err := files.ReadFile("assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := mime.TypeByExtension(filepath.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Security-Policy", builtinCSP)
	w.Header().Set("Content-Type", ct)
	_, _ = w.Write(raw)
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	if !safeRel(rel) {
		http.NotFound(w, r)
		return
	}
	realRoot, err := filepath.EvalSymlinks(s.pages)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(s.pages, filepath.FromSlash(rel))
	info, err := os.Lstat(full)
	if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		http.NotFound(w, r)
		return
	}
	real, err := filepath.EvalSymlinks(full)
	if err != nil || !strings.HasPrefix(real, realRoot+string(os.PathSeparator)) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Security-Policy", agentCSP)
	http.ServeFile(w, r, real)
}

// safeRel 只接受不含空段、. / .. 和点开头的相对路径。
func safeRel(rel string) bool {
	if rel == "" {
		return false
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}
