package webui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"billgo/internal/blob"
	"billgo/internal/config"
	"billgo/internal/outbox"
	"billgo/internal/peers"
	"billgo/internal/ticket"
)

type fakeDir struct{ recs []peers.Record }

func (f *fakeDir) Lookup(id string) (peers.Record, bool) {
	for _, r := range f.recs {
		if r.PeerID == id && !r.Deleted {
			return r, true
		}
	}
	return peers.Record{}, false
}

func (f *fakeDir) ByName(name string) []peers.Record {
	var out []peers.Record
	for _, r := range f.recs {
		if r.Name == name && !r.Deleted {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeDir) ListAll() []peers.Record { return f.recs }

type fakeTickets struct {
	states   []ticket.State
	events   map[string][]ticket.Event
	receipts map[string][]ticket.Receipt
	created  *ticket.CreateReq
	id       string
}

func (f *fakeTickets) List() []ticket.State { return f.states }
func (f *fakeTickets) Get(id string) (ticket.State, []ticket.Event, error) {
	for _, st := range f.states {
		if st.ID == id {
			return st, f.events[id], nil
		}
	}
	return ticket.State{}, nil, os.ErrNotExist
}
func (f *fakeTickets) Create(req ticket.CreateReq) (string, error) {
	f.created = &req
	if f.id == "" {
		return "t-created", nil
	}
	return f.id, nil
}
func (f *fakeTickets) Receipts(id string) ([]ticket.Receipt, error) { return f.receipts[id], nil }

type fakeOutbox struct{ items []outbox.Item }

func (f *fakeOutbox) List() []outbox.Item { return f.items }

type harness struct {
	srv   *Server
	dir   *fakeDir
	tk    *fakeTickets
	out   *fakeOutbox
	root  string
	blobs string
}

func newHarness(t *testing.T, recs []peers.Record, tk *fakeTickets, items []outbox.Item) *harness {
	t.Helper()
	if tk == nil {
		tk = &fakeTickets{id: "t-created"}
	}
	h := &harness{
		dir:   &fakeDir{recs: recs},
		tk:    tk,
		out:   &fakeOutbox{items: items},
		root:  t.TempDir(),
		blobs: t.TempDir(),
	}
	// 预先建 pages/，避免触发脚手架，测试自己控制目录内容。
	if err := os.MkdirAll(filepath.Join(h.root, "pages"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := New(Info{
		PeerID:    "applereceiver0001",
		Name:      "apple",
		Kind:      config.KindResident,
		Network:   "test",
		Endpoint:  "https://apple:8787",
		Version:   "v9.9.9",
		Listen:    "127.0.0.1:0",
		Bootstrap: []string{"https://banana:8787#bbbbbbbbbbbbbbbb"},
		AllowFrom: []string{"bbbbbbbbbbbbbbbb"},
		WakeMode:  "local",
	}, h.dir, h.tk, h.out, h.root, h.blobs, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.UnixMilli(1_700_000_000_000) }
	h.srv = s
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Close(ctx)
	})
	return h
}

func (h *harness) do(t *testing.T, method, path string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://127.0.0.1"+path, body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestNodeAPI(t *testing.T) {
	h := newHarness(t, nil, nil, nil)
	rec := h.do(t, "GET", "/api/node", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["version"] != "v9.9.9" || out["peer_id"] != "applereceiver0001" {
		t.Fatalf("响应不对: %+v", out)
	}
	if out["wake_mode"] != "local" {
		t.Fatalf("wake_mode 缺失: %+v", out)
	}
}

func TestPeersRedacted(t *testing.T) {
	rec := peers.Record{
		PeerID: "bbbbbbbbbbbbbbbb", PubKey: "SUPERSECRETKEY", Name: "banana",
		Desc: "发单节点", Kind: config.KindWakeable, Endpoint: "https://banana:8787",
		Network: "test", Version: 3,
		Wake: &config.Wake{Mode: "local", Target: "user@host", Container: "banana", HostFP: "HOSTFP"},
	}
	h := newHarness(t, []peers.Record{rec}, nil, nil)
	rec2 := h.do(t, "GET", "/api/peers", nil, nil)
	body := rec2.Body.String()
	for _, secret := range []string{"SUPERSECRETKEY", "user@host", "HOSTFP", "container"} {
		if strings.Contains(body, secret) {
			t.Fatalf("响应泄露了 %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, `"wake_mode":"local"`) {
		t.Fatalf("应保留 wake_mode: %s", body)
	}
}

func TestPagesDiscovery(t *testing.T) {
	h := newHarness(t, nil, nil, nil)
	pages := filepath.Join(h.root, "pages")
	for _, d := range []string{"", "nested"} {
		if err := os.MkdirAll(filepath.Join(pages, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"a.html":        "<html><title>Alpha</title></html>",
		"b.html":        "<html></html>",
		"b.json":        `{"title":"Bee","desc":"描述B"}`,
		"_draft.html":   "<title>Draft</title>",
		"nested/c.html": "<title>Charlie</title>",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(pages, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(-time.Hour)
	for i, name := range []string{"a.html", "b.html", "nested/c.html"} {
		ts := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(filepath.Join(pages, name), ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(pages, "a.html"), filepath.Join(pages, "link.html")); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}

	rec := h.do(t, "GET", "/api/pages", nil, nil)
	var got []pageInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("应发现 3 个页面，得到 %d: %+v", len(got), got)
	}
	if got[0].Path != "nested/c.html" || got[0].Title != "Charlie" {
		t.Fatalf("排序或标题不对: %+v", got)
	}
	if got[1].Path != "b.html" || got[1].Title != "Bee" || got[1].Desc != "描述B" {
		t.Fatalf("元数据不对: %+v", got)
	}
}

func TestStaticPageAndTraversal(t *testing.T) {
	h := newHarness(t, nil, nil, nil)
	pages := filepath.Join(h.root, "pages")
	if err := os.WriteFile(filepath.Join(pages, "a.html"), []byte("<title>A</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(h.root, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(pages, "link.html")); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}

	rec := h.do(t, "GET", "/pages/a.html", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "A") {
		t.Fatalf("页面读取失败: %d %s", rec.Code, rec.Body.String())
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "'unsafe-inline'") {
		t.Fatalf("Agent 页应允许内联脚本: %s", csp)
	}

	for _, p := range []string{"/pages/link.html", "/pages/%2e%2e/secret.txt", "/pages/.hidden.html"} {
		if rec := h.do(t, "GET", p, nil, nil); rec.Code == http.StatusOK {
			t.Fatalf("%s 不应可访问", p)
		}
	}
}

func TestBuiltinCSP(t *testing.T) {
	h := newHarness(t, nil, nil, nil)
	rec := h.do(t, "GET", "/", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("主页失败: %d", rec.Code)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if strings.Contains(csp, "'unsafe-inline'") {
		t.Fatalf("内置页不应允许内联脚本: %s", csp)
	}
	if !strings.Contains(csp, "connect-src 'self'") {
		t.Fatalf("CSP 缺少 connect-src: %s", csp)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("缺少 no-store")
	}
}

func TestHostAndOriginChecks(t *testing.T) {
	h := newHarness(t, nil, nil, nil)

	req := httptest.NewRequest("GET", "http://127.0.0.1/api/node", nil)
	req.Host = "evil.example"
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非法 Host 应 403，得到 %d", rec.Code)
	}

	rec = h.do(t, "POST", "/api/tickets", strings.NewReader(`{"form":{},"flow":[]}`), map[string]string{
		"Content-Type": "application/json",
		"Origin":       "http://evil.example",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("跨源 POST 应 403，得到 %d", rec.Code)
	}

	rec = h.do(t, "POST", "/api/tickets", strings.NewReader(`{}`), map[string]string{
		"Content-Type": "text/plain",
	})
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("非 JSON 应 415，得到 %d", rec.Code)
	}
}

func TestCreateTicket(t *testing.T) {
	recs := []peers.Record{
		{PeerID: "bbbbbbbbbbbbbbbb", Name: "banana", Kind: config.KindResident, Network: "test"},
		{PeerID: "cccccccccccccccc", Name: "peach", Kind: config.KindResident, Network: "test"},
	}
	h := newHarness(t, recs, nil, nil)
	body := `{"form":{"task":"x"},"flow":["banana","peach"]}`
	rec := h.do(t, "POST", "/api/tickets", strings.NewReader(body), map[string]string{
		"Content-Type": "application/json",
		"Origin":       "http://127.0.0.1:7878",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("创建失败: %d %s", rec.Code, rec.Body.String())
	}
	if h.tk.created == nil {
		t.Fatal("没有调用 Create")
	}
	if got := h.tk.created.Flow[0].To; got != "bbbbbbbbbbbbbbbb" {
		t.Fatalf("名称解析失败: %s", got)
	}
	if len(h.tk.created.Flow[1].DependsOn) != 1 || h.tk.created.Flow[1].DependsOn[0] != 1 {
		t.Fatalf("链式依赖不对: %+v", h.tk.created.Flow[1])
	}
}

func TestStatusAttention(t *testing.T) {
	now := int64(1_700_000_000_000)
	tk := &fakeTickets{
		id: "t-created",
		states: []ticket.State{
			{
				ID: "t1", Status: ticket.StatusRunning, UpdatedAt: now - 120_000,
				Hops: []ticket.HopState{
					{Step: 1, Target: "bbbbbbbbbbbbbbbb", State: ticket.HopRejected},
				},
			},
		},
	}
	items := []outbox.Item{
		{ID: 1, PeerID: "cccccccccccccccc", Attempts: 6, CreatedAt: now - 11*60*1000, LastErr: "连接超时"},
	}
	h := newHarness(t, nil, tk, items)
	rec := h.do(t, "GET", "/api/status", nil, nil)
	var out statusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Summary.Tickets[ticket.StatusRunning] != 1 {
		t.Fatalf("汇总不对: %+v", out.Summary)
	}
	kinds := map[string]bool{}
	for _, a := range out.Attention {
		kinds[a.Kind] = true
	}
	if !kinds["hop_rejected"] || !kinds["outbox_stuck"] {
		t.Fatalf("待处理项不对: %+v", out.Attention)
	}
	for _, a := range out.Attention {
		if a.Kind == "hop_rejected" && !strings.Contains(a.Hint, "redispatch") {
			t.Fatalf("缺少可复制命令: %+v", a)
		}
	}
}

func TestArtifactsAndBlob(t *testing.T) {
	hash, size, err := blob.PutBytes(t.TempDir(), []byte("x")) // 用一个临时目录拿哈希，再写到 harness 的 blobs
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, nil, &fakeTickets{
		id: "t-created",
		receipts: map[string][]ticket.Receipt{
			"t1": {{Ticket: "t1", Hop: 1, Blob: hash, Size: size, From: "bbbbbbbbbbbbbbbb"}},
		},
	}, nil)
	if _, _, err := blob.PutBytes(h.blobs, []byte("x")); err != nil {
		t.Fatal(err)
	}

	rec := h.do(t, "GET", "/api/tickets/t1/artifacts", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/api/blobs/"+hash) {
		t.Fatalf("交付物列表不对: %d %s", rec.Code, rec.Body.String())
	}

	rec = h.do(t, "GET", "/api/blobs/"+hash, nil, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "x" {
		t.Fatalf("下载失败: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("应强制下载")
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("缺少 nosniff")
	}

	if rec := h.do(t, "GET", "/api/blobs/not-a-hash", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("非法哈希应 404，得到 %d", rec.Code)
	}
}

func TestScaffold(t *testing.T) {
	root := t.TempDir()
	s, err := New(Info{PeerID: "p", Name: "n", Listen: "127.0.0.1:0"},
		&fakeDir{}, &fakeTickets{id: "x"}, &fakeOutbox{}, root, t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Close(ctx)
	}()

	pages := filepath.Join(root, "pages")
	for _, name := range []string{"README.md", "example.html", "example.json"} {
		if _, err := os.Stat(filepath.Join(pages, name)); err != nil {
			t.Fatalf("首次启动应生成 %s: %v", name, err)
		}
	}
}
