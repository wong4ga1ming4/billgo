// Package billui 是独立的页面进程。它只通过本机 Unix socket 问 billgod，
// 自己不保存工单，也不代替节点做权限判断。
package billui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"billgo/internal/audit"
	"billgo/internal/control"
	"billgo/internal/nodestatus"
	"billgo/internal/peers"
	"billgo/internal/ticket"
)

//go:embed page.html
var files embed.FS

const (
	passwordFile = "billui.password"
	cookieName   = "billui"
	pageLimit    = 20
	csp          = "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"
)

// Server 一个 billui 进程。会话只在内存里，重启即失效。
type Server struct {
	data     string
	password string
	client   *control.Client
	tmpl     *template.Template

	mu       sync.Mutex
	sessions map[string]struct{}
}

// Open 生成新密码，覆盖写入数据目录，并准备好页面。旧密码和旧会话都不再有效。
func Open(dataDir string) (*Server, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("建数据目录: %w", err)
	}
	pw, err := newPassword()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, passwordFile)
	if err := os.WriteFile(path, []byte(pw+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("写密码文件: %w", err)
	}
	tmpl, err := template.ParseFS(files, "page.html")
	if err != nil {
		return nil, err
	}
	return &Server{
		data:     dataDir,
		password: pw,
		client:   control.NewClient(dataDir),
		tmpl:     tmpl,
		sessions: map[string]struct{}{},
	}, nil
}

// Password 本次启动的密码。
func (s *Server) Password() string { return s.password }

// PasswordPath 密码文件路径。
func (s *Server) PasswordPath() string { return filepath.Join(s.data, passwordFile) }

// Handler 页面路由。未登录的请求一律回到登录页。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.loginGet)
	mux.HandleFunc("POST /login", s.loginPost)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /{$}", s.auth(s.home))
	mux.HandleFunc("GET /audit", s.auth(s.audit))
	mux.HandleFunc("POST /tickets", s.auth(s.createLocal))
	mux.HandleFunc("POST /tickets/{id}/void", s.auth(s.voidLocal))
	mux.HandleFunc("POST /tickets/{id}/redispatch", s.auth(s.redispatchLocal))
	mux.HandleFunc("GET /tickets/{id}", s.auth(s.ticket))
	mux.HandleFunc("GET /peers/{id}", s.auth(s.peer))
	mux.HandleFunc("POST /peers/{id}/tickets", s.auth(s.createPeer))
	mux.HandleFunc("POST /peers/{id}/tickets/{tid}/void", s.auth(s.voidPeer))
	mux.HandleFunc("POST /peers/{id}/tickets/{tid}/redispatch", s.auth(s.redispatchPeer))
	mux.HandleFunc("GET /peers/{id}/tickets/{tid}", s.auth(s.peerTicket))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", csp)
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) loginGet(w http.ResponseWriter, r *http.Request) {
	if s.authed(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, "login", loginView{Title: "登录"})
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, "login", loginView{Title: "登录", Error: "请求无法解析"})
		return
	}
	got := strings.TrimSpace(r.FormValue("password"))
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.password)) != 1 {
		slog.Warn("billui 登录失败", "remote", r.RemoteAddr)
		s.render(w, "login", loginView{Title: "登录", Error: "密码不对"})
		return
	}
	token, err := newToken()
	if err != nil {
		http.Error(w, "无法建立会话", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.sessions[token] = struct{}{}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	slog.Info("billui 登录成功", "remote", r.RemoteAddr)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func (s *Server) authed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	s.mu.Lock()
	_, ok := s.sessions[c.Value]
	s.mu.Unlock()
	return ok
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var node nodeInfo
	if err := s.client.Do(ctx, http.MethodGet, "/node", nil, &node); err != nil {
		s.problem(w, r, explain(err))
		return
	}
	var st nodestatus.Status
	if err := s.client.Do(ctx, http.MethodGet, "/status", nil, &st); err != nil {
		s.problem(w, r, explain(err))
		return
	}
	var list []peers.Record
	if err := s.client.Do(ctx, http.MethodGet, "/peers", nil, &list); err != nil {
		s.problem(w, r, explain(err))
		return
	}
	var cfg map[string]any
	if err := s.client.Call(ctx, "", "config.read", nil, &cfg); err != nil {
		s.problem(w, r, explain(err))
		return
	}
	items, next, err := s.ticketPage(ctx, "", r.URL.Query().Get("cursor"))
	if err != nil {
		s.problem(w, r, explain(err))
		return
	}
	nextHref := ""
	if next != "" {
		nextHref = "/?cursor=" + next
	}
	note, open := accessNote(cfg)
	s.render(w, "home", homeView{
		Title:        node.Name,
		Authed:       true,
		Node:         node,
		Status:       st,
		Peers:        peerRows(list, node.PeerID),
		Tickets:      ticketRows(items, "/tickets/"),
		NextHref:     nextHref,
		CreateAction: "/tickets",
		AccessNote:   note,
		AccessOpen:   open,
	})
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	q := url.Values{}
	q.Set("limit", "50")
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		q.Set("cursor", cursor)
	}
	var page struct {
		Items      []audit.Record `json:"items"`
		NextCursor string         `json:"next_cursor"`
	}
	if err := s.client.Do(r.Context(), http.MethodGet, "/audit?"+q.Encode(), nil, &page); err != nil {
		s.problem(w, r, explain(err))
		return
	}
	rows := make([]auditRow, 0, len(page.Items))
	for _, rec := range page.Items {
		result := rec.Result
		switch result {
		case "allow":
			result = "允许"
		case "deny":
			result = "拒绝"
		}
		rows = append(rows, auditRow{
			When:     fmtTime(rec.TS),
			From:     rec.From,
			Action:   rec.Action,
			Result:   result,
			Envelope: rec.EnvelopeID,
		})
	}
	nextHref := ""
	if page.NextCursor != "" {
		nextHref = "/audit?cursor=" + url.QueryEscape(page.NextCursor)
	}
	s.render(w, "audit", auditView{
		Title:    "审计",
		Authed:   true,
		Rows:     rows,
		NextHref: nextHref,
	})
}

func (s *Server) peer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validPeer(id) {
		s.problem(w, r, "节点标识不对")
		return
	}
	ctx := r.Context()
	var info nodeInfo
	if err := s.client.Call(ctx, id, "node.info", nil, &info); err != nil {
		s.problem(w, r, explain(err))
		return
	}
	var cfg map[string]any
	configNote := ""
	if err := s.client.Call(ctx, id, "config.read", nil, &cfg); err != nil {
		if !denied(err) {
			s.problem(w, r, explain(err))
			return
		}
		configNote = "配置不可见"
	}
	items, next, err := s.ticketPage(ctx, id, r.URL.Query().Get("cursor"))
	listNote := ""
	if err != nil {
		if !denied(err) {
			s.problem(w, r, explain(err))
			return
		}
		listNote = "对方拒绝列出工单。列表按约定应当公开。"
		items, next = nil, ""
	}
	nextHref := ""
	if next != "" {
		nextHref = "/peers/" + id + "?cursor=" + next
	}
	s.render(w, "peer", peerView{
		Title:        info.Name,
		Authed:       true,
		Label:        info.Name + "#" + short(info.PeerID),
		Config:       configRows(cfg),
		Tickets:      ticketRows(items, "/peers/"+id+"/tickets/"),
		NextHref:     nextHref,
		CreateAction: "/peers/" + id + "/tickets",
		ConfigNote:   configNote,
		ListNote:     listNote,
	})
}

func (s *Server) ticket(w http.ResponseWriter, r *http.Request) {
	s.showTicket(w, r, "", r.PathValue("id"), "/")
}

func (s *Server) peerTicket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validPeer(id) {
		s.problem(w, r, "节点标识不对")
		return
	}
	s.showTicket(w, r, id, r.PathValue("tid"), "/peers/"+id)
}

func (s *Server) showTicket(w http.ResponseWriter, r *http.Request, peer, tid, back string) {
	if !validTicket(tid) {
		s.problem(w, r, "工单号不对")
		return
	}
	var out struct {
		State  ticket.State   `json:"state"`
		Events []ticket.Event `json:"events"`
	}
	var err error
	if peer == "" {
		err = s.client.Do(r.Context(), http.MethodGet, "/tickets/"+tid, nil, &out)
	} else {
		err = s.client.Call(r.Context(), peer, "tickets.get", map[string]string{"id": tid}, &out)
	}
	if err != nil {
		msg := explain(err)
		if denied(err) && peer != "" {
			msg = "内容不可见"
		}
		s.problem(w, r, msg)
		return
	}
	if out.Events == nil {
		out.Events = []ticket.Event{}
	}
	base := "/tickets/" + out.State.ID
	if peer != "" {
		base = "/peers/" + peer + "/tickets/" + out.State.ID
	}
	s.render(w, "ticket", ticketView{
		Title:            short(out.State.ID),
		Authed:           true,
		Back:             back,
		State:            out.State,
		Events:           out.Events,
		Updated:          fmtTime(out.State.UpdatedAt),
		Running:          out.State.Status == ticket.StatusRunning,
		VoidAction:       base + "/void",
		RedispatchAction: base + "/redispatch",
	})
}

func (s *Server) createLocal(w http.ResponseWriter, r *http.Request) {
	s.create(w, r, "")
}

func (s *Server) createPeer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validPeer(id) {
		s.problem(w, r, "节点标识不对")
		return
	}
	s.create(w, r, id)
}

func (s *Server) create(w http.ResponseWriter, r *http.Request, peer string) {
	if err := r.ParseForm(); err != nil {
		s.problem(w, r, "请求无法解析")
		return
	}
	raw := strings.TrimSpace(r.FormValue("form"))
	if raw == "" || !json.Valid([]byte(raw)) {
		s.problem(w, r, "表单不是合法 JSON")
		return
	}
	hops, err := flowHops(r.FormValue("flow"))
	if err != nil {
		s.problem(w, r, err.Error())
		return
	}
	var out struct {
		ID string `json:"id"`
	}
	req := ticket.CreateReq{Form: json.RawMessage(raw), Flow: hops}
	if err := s.client.Call(r.Context(), peer, "tickets.create", req, &out); err != nil {
		s.problem(w, r, explain(err))
		return
	}
	if !validTicket(out.ID) {
		s.problem(w, r, "节点没有返回工单号")
		return
	}
	dest := "/tickets/" + out.ID
	if peer != "" {
		dest = "/peers/" + peer + "/tickets/" + out.ID
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (s *Server) voidLocal(w http.ResponseWriter, r *http.Request) {
	s.void(w, r, "", r.PathValue("id"))
}

func (s *Server) voidPeer(w http.ResponseWriter, r *http.Request) {
	s.void(w, r, r.PathValue("id"), r.PathValue("tid"))
}

func (s *Server) void(w http.ResponseWriter, r *http.Request, peer, tid string) {
	if peer != "" && !validPeer(peer) {
		s.problem(w, r, "节点标识不对")
		return
	}
	if !validTicket(tid) {
		s.problem(w, r, "工单号不对")
		return
	}
	if err := s.client.Call(r.Context(), peer, "tickets.void", map[string]string{"id": tid}, nil); err != nil {
		s.problem(w, r, explain(err))
		return
	}
	dest := "/tickets/" + tid
	if peer != "" {
		dest = "/peers/" + peer + "/tickets/" + tid
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (s *Server) redispatchLocal(w http.ResponseWriter, r *http.Request) {
	s.redispatch(w, r, "", r.PathValue("id"))
}

func (s *Server) redispatchPeer(w http.ResponseWriter, r *http.Request) {
	s.redispatch(w, r, r.PathValue("id"), r.PathValue("tid"))
}

func (s *Server) redispatch(w http.ResponseWriter, r *http.Request, peer, tid string) {
	if peer != "" && !validPeer(peer) {
		s.problem(w, r, "节点标识不对")
		return
	}
	if !validTicket(tid) {
		s.problem(w, r, "工单号不对")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.problem(w, r, "请求无法解析")
		return
	}
	hop, err := strconv.Atoi(strings.TrimSpace(r.FormValue("hop")))
	to := strings.TrimSpace(r.FormValue("to"))
	if err != nil || hop <= 0 || to == "" {
		s.problem(w, r, "需要 hop 和 to")
		return
	}
	body := map[string]any{"id": tid, "hop": hop, "to": to}
	if err := s.client.Call(r.Context(), peer, "tickets.redispatch", body, nil); err != nil {
		s.problem(w, r, explain(err))
		return
	}
	dest := "/tickets/" + tid
	if peer != "" {
		dest = "/peers/" + peer + "/tickets/" + tid
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func flowHops(flow string) ([]ticket.Hop, error) {
	var hops []ticket.Hop
	for _, name := range strings.Split(flow, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		step := len(hops) + 1
		hop := ticket.Hop{Step: step, To: name, Desc: name}
		if step > 1 {
			hop.DependsOn = []int{step - 1}
		}
		hops = append(hops, hop)
	}
	if len(hops) == 0 {
		return nil, errors.New("流转不能为空")
	}
	return hops, nil
}

func (s *Server) ticketPage(ctx context.Context, peer, cursor string) ([]ticket.State, string, error) {
	args := map[string]any{"limit": pageLimit}
	if cursor != "" {
		args["cursor"] = cursor
	}
	var page struct {
		Items      []ticket.State `json:"items"`
		NextCursor string         `json:"next_cursor"`
	}
	if err := s.client.Call(ctx, peer, "tickets.list", args, &page); err != nil {
		return nil, "", err
	}
	if page.Items == nil {
		page.Items = []ticket.State{}
	}
	return page.Items, page.NextCursor, nil
}

func (s *Server) problem(w http.ResponseWriter, r *http.Request, msg string) {
	view := problemView{
		Title:   "billui",
		Authed:  s.authed(r),
		Message: msg,
		Hint:    hintFor(msg),
	}
	if r.Method == http.MethodGet {
		view.Retry = r.URL.RequestURI()
	}
	s.render(w, "problem", view)
}

func hintFor(msg string) string {
	switch msg {
	case "拒绝访问":
		return "对方拒绝了本节点。不会自动再问。要看它的数据，打开它自己的 billui，或让它在 access 里放行本节点。"
	case "内容不可见":
		return "工单还在列表里。对方拒绝了表单和交付物。不会自动再问。"
	case "配置不可见":
		return "对方拒绝了配置。身份和工单列表仍然可以看。"
	case "离线/不可达":
		return "对方现在连不上。不会留下上次看到的内容。"
	case "超时":
		return "对方没有及时回答。不会留下上次看到的内容。"
	case "名录里没有这个节点":
		return "本机名录里没有它。"
	default:
		return ""
	}
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("渲染页面失败", "page", name, "err", err)
	}
}

func denied(err error) bool {
	var api *control.APIError
	return errors.As(err, &api) && api.Code == "denied"
}

func explain(err error) string {
	var api *control.APIError
	if errors.As(err, &api) {
		switch api.Code {
		case "denied":
			return "拒绝访问"
		case "unreachable":
			return "离线/不可达"
		case "timeout":
			return "超时"
		case "not_found":
			return "名录里没有这个节点"
		default:
			if api.Message != "" {
				return api.Message
			}
		}
	}
	return "连不上本机 billgod"
}

func newPassword() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成密码: %w", err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])), nil
}

func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func validPeer(id string) bool {
	if len(id) != 16 {
		return false
	}
	for _, c := range id {
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

func validTicket(id string) bool {
	if len(id) < 16 || len(id) > 40 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F', c == '-':
		default:
			return false
		}
	}
	return true
}

func short(id string) string {
	if len(id) > 4 {
		return id[:4]
	}
	return id
}

func fmtTime(ms int64) string {
	if ms <= 0 {
		return "—"
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04:05")
}
