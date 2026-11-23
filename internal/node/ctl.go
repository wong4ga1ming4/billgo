package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"billgo/internal/access"
	"billgo/internal/audit"
	"billgo/internal/config"
	"billgo/internal/envelope"
	"billgo/internal/id"
	"billgo/internal/peers"
	"billgo/internal/ticket"
	"billgo/internal/transport"
)

// 远程调用返回给本机调用方的错误类别。billui 靠它们区分展示。
const (
	codeDenied      = "denied"
	codeUnreachable = "unreachable"
	codeTimeout     = "timeout"
	codeNotFound    = "not_found"
	codeBadRequest  = "bad_request"
	codeUnsupported = "unsupported"
	codeRemote      = "remote_error"
)

type callError struct {
	code string
	msg  string
}

func (e *callError) Error() string { return e.msg }

func callErr(code, msg string) error { return &callError{code: code, msg: msg} }

type ctlRequest struct {
	ID     string          `json:"id"`
	Action string          `json:"action"`
	Args   json.RawMessage `json:"args,omitempty"`
}

type ctlResult struct {
	ID        string          `json:"id"`
	OK        bool            `json:"ok"`
	ErrorCode string          `json:"error_code,omitempty"`
	Error     string          `json:"error,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

type ticketPage struct {
	Items      []ticket.State `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

// Call 执行一个控制动作。peer 为空或指向自己时在本地执行，不审计；
// 指向邻居时发 ctl.request，由对方验权限并记审计。
func (n *Node) Call(ctx context.Context, peer, action, reqID string, args json.RawMessage) (json.RawMessage, error) {
	if !access.Known(action) {
		return nil, callErr(codeBadRequest, "未知动作 "+action)
	}
	target, err := n.resolveTarget(peer)
	if err != nil {
		return nil, err
	}
	if target == n.id.PeerID {
		return n.exec(action, args)
	}
	return n.callRemote(ctx, target, action, reqID, args)
}

func (n *Node) access() *access.Policy {
	n.cfgMu.RLock()
	defer n.cfgMu.RUnlock()
	return n.cfg.Access
}

func (n *Node) resolveTarget(peer string) (string, error) {
	if peer == "" || peer == n.id.PeerID {
		return n.id.PeerID, nil
	}
	id, err := n.resolvePeer(peer)
	if err == nil {
		return id, nil
	}
	if peer == n.cfg.Name {
		return n.id.PeerID, nil
	}
	return "", callErr(codeNotFound, err.Error())
}

func (n *Node) exec(action string, args json.RawMessage) (json.RawMessage, error) {
	switch action {
	case access.NodeInfo:
		return toJSON(n.nodeInfo())
	case access.PeersList:
		list := n.peers.ListAll()
		if list == nil {
			list = []peers.Record{}
		}
		return toJSON(list)
	case access.ConfigRead:
		return toJSON(n.configView())
	case access.TicketsList:
		var in struct {
			Limit  int    `json:"limit"`
			Cursor string `json:"cursor"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return nil, err
		}
		items, next, err := n.tickets.ListPage(in.Limit, in.Cursor)
		if err != nil {
			return nil, callErr(codeBadRequest, err.Error())
		}
		if items == nil {
			items = []ticket.State{}
		}
		body := map[string]any{"items": publicTickets(items)}
		if next != "" {
			body["next_cursor"] = next
		}
		return toJSON(body)
	case access.TicketsGet:
		var in struct {
			ID string `json:"id"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return nil, err
		}
		if in.ID == "" {
			return nil, callErr(codeBadRequest, "缺少工单号")
		}
		st, evs, err := n.tickets.Get(in.ID)
		if err != nil {
			return nil, callErr(codeNotFound, err.Error())
		}
		return toJSON(map[string]any{"state": st, "events": evs})
	case access.TicketsCreate:
		var req ticket.CreateReq
		if err := decodeArgs(args, &req); err != nil {
			return nil, err
		}
		for i := range req.Flow {
			id, err := n.resolvePeer(req.Flow[i].To)
			if err != nil {
				return nil, callErr(codeBadRequest, err.Error())
			}
			req.Flow[i].To = id
		}
		tid, err := n.tickets.Create(req)
		if err != nil {
			return nil, callErr(codeBadRequest, err.Error())
		}
		return toJSON(map[string]any{"id": tid})
	case access.TicketsRedispatch:
		var in struct {
			ID  string `json:"id"`
			Hop int    `json:"hop"`
			To  string `json:"to"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return nil, err
		}
		if in.ID == "" || in.Hop <= 0 || in.To == "" {
			return nil, callErr(codeBadRequest, "需要 id、hop 和 to")
		}
		to, err := n.resolvePeer(in.To)
		if err != nil {
			return nil, callErr(codeBadRequest, err.Error())
		}
		if err := n.tickets.Redispatch(in.ID, in.Hop, to); err != nil {
			return nil, callErr(codeBadRequest, err.Error())
		}
		return toJSON(map[string]any{"ok": true})
	case access.TicketsVoid:
		var in struct {
			ID string `json:"id"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return nil, err
		}
		if in.ID == "" {
			return nil, callErr(codeBadRequest, "缺少工单号")
		}
		if err := n.tickets.Void(in.ID); err != nil {
			return nil, callErr(codeBadRequest, err.Error())
		}
		return toJSON(map[string]any{"ok": true})
	case access.ConfigWrite:
		return n.writeConfig(args)
	default:
		return nil, callErr(codeUnsupported, "这个动作还不能远程调用")
	}
}

type listedHop struct {
	Step       int    `json:"step"`
	Target     string `json:"target"`
	State      string `json:"state"`
	SentAt     int64  `json:"sent_at,omitempty"`
	AcceptedAt int64  `json:"accepted_at,omitempty"`
}

type listedTicket struct {
	ID        string      `json:"id"`
	Status    string      `json:"status"`
	Hops      []listedHop `json:"hops"`
	CreatedAt int64       `json:"created_at"`
	UpdatedAt int64       `json:"updated_at"`
}

// publicTickets 是工单列表给出去的形状：有没有、走到哪、什么时候。表单和跳的描述留在 tickets.get。
func publicTickets(items []ticket.State) []listedTicket {
	out := make([]listedTicket, len(items))
	for i, st := range items {
		hops := make([]listedHop, len(st.Hops))
		for j, h := range st.Hops {
			hops[j] = listedHop{Step: h.Step, Target: h.Target, State: h.State, SentAt: h.SentAt, AcceptedAt: h.AcceptedAt}
		}
		out[i] = listedTicket{ID: st.ID, Status: st.Status, Hops: hops, CreatedAt: st.CreatedAt, UpdatedAt: st.UpdatedAt}
	}
	return out
}

func (n *Node) writeConfig(args json.RawMessage) (json.RawMessage, error) {
	if n.configPath == "" {
		return nil, callErr(codeBadRequest, "节点没有配置文件路径")
	}
	next, err := config.Parse(args)
	if err != nil {
		return nil, callErr(codeBadRequest, err.Error())
	}
	if next.DataDir != n.cfg.DataDir {
		return nil, callErr(codeBadRequest, "不能修改 data_dir")
	}
	if err := config.Save(n.configPath, next); err != nil {
		return nil, err
	}
	n.cfgMu.Lock()
	n.cfg.Access = next.Access
	n.cfgMu.Unlock()
	if n.access().ReadsOpen() {
		slog.Warn("远程读默认开放", "detail", "这次写入的 access 仍允许没被点名的邻居读取。写仍然默认拒绝。")
	}
	return toJSON(map[string]any{"ok": true, "hot": []string{"access"}, "restart": true})
}

func (n *Node) callRemote(ctx context.Context, peerID, action, reqID string, args json.RawMessage) (json.RawMessage, error) {
	rec, ok := n.peers.Lookup(peerID)
	if !ok || rec.Endpoint == "" {
		return nil, callErr(codeUnreachable, "离线/不可达")
	}
	if reqID == "" {
		reqID = id.New()
	}
	req := ctlRequest{ID: reqID, Action: action, Args: args}
	env, err := envelope.New(envelope.TypeCtlRequest, peerID, "", 0, time.Now().UnixMilli(), req)
	if err != nil {
		return nil, err
	}
	if err := env.Sign(n.id.Private); err != nil {
		return nil, err
	}
	resp, err := n.client.Send(ctx, rec.Endpoint, peerID, env)
	if err != nil {
		return nil, classifySend(ctx, err)
	}
	if resp == nil || resp.Type != envelope.TypeCtlResponse {
		return nil, callErr(codeRemote, "对方没有返回控制响应")
	}
	sender, err := resp.VerifySignature()
	if err != nil {
		return nil, callErr(codeRemote, "响应签名无效")
	}
	if sender != peerID {
		return nil, callErr(codeRemote, "响应签名者不是目标节点")
	}
	var result ctlResult
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return nil, callErr(codeRemote, "控制响应无法解析")
	}
	if result.ID != req.ID {
		return nil, callErr(codeRemote, "控制响应与请求不匹配")
	}
	if !result.OK {
		code := result.ErrorCode
		if code == "" {
			code = codeRemote
		}
		msg := result.Error
		if msg == "" {
			msg = "远程调用失败"
		}
		return nil, callErr(code, msg)
	}
	if len(result.Data) == 0 {
		return json.RawMessage(`{}`), nil
	}
	return result.Data, nil
}

func (n *Node) handleCtlRequest(sender string, e *envelope.Envelope) (*envelope.Envelope, error) {
	var req ctlRequest
	if err := json.Unmarshal(e.Body, &req); err != nil || req.ID == "" || req.Action == "" {
		return n.replyCtl(sender, req.ID, false, codeBadRequest, "控制请求不完整", nil)
	}
	if !access.Known(req.Action) {
		if err := n.auditCtl(sender, req, "deny"); err != nil {
			return nil, err
		}
		return n.replyCtl(sender, req.ID, false, codeBadRequest, "未知动作", nil)
	}
	allowed := n.access().Decide(sender, req.Action)
	result := "allow"
	if !allowed {
		result = "deny"
	}
	if err := n.auditCtl(sender, req, result); err != nil {
		return nil, err
	}
	if !allowed {
		return n.replyCtl(sender, req.ID, false, codeDenied, "拒绝访问", nil)
	}
	data, err := n.exec(req.Action, req.Args)
	if err != nil {
		var ce *callError
		if errors.As(err, &ce) {
			return n.replyCtl(sender, req.ID, false, ce.code, ce.msg, nil)
		}
		return n.replyCtl(sender, req.ID, false, codeBadRequest, err.Error(), nil)
	}
	return n.replyCtl(sender, req.ID, true, "", "", data)
}

func (n *Node) auditCtl(sender string, req ctlRequest, result string) error {
	if n.audit == nil {
		return fmt.Errorf("审计日志未打开")
	}
	return n.audit.Write(audit.Record{
		TS:         time.Now().UnixMilli(),
		From:       sender,
		Action:     req.Action,
		Target:     "self",
		Result:     result,
		EnvelopeID: req.ID,
	})
}

func (n *Node) replyCtl(to, reqID string, ok bool, code, msg string, data json.RawMessage) (*envelope.Envelope, error) {
	res := ctlResult{ID: reqID, OK: ok, ErrorCode: code, Error: msg, Data: data}
	env, err := envelope.New(envelope.TypeCtlResponse, to, "", 0, time.Now().UnixMilli(), res)
	if err != nil {
		return nil, err
	}
	if err := env.Sign(n.id.Private); err != nil {
		return nil, err
	}
	return env, nil
}

func (n *Node) nodeInfo() map[string]any {
	wakeMode := ""
	if n.cfg.Wake != nil {
		wakeMode = n.cfg.Wake.Mode
	}
	return map[string]any{
		"peer_id":    n.id.PeerID,
		"name":       n.cfg.Name,
		"kind":       n.cfg.Kind,
		"endpoint":   n.cfg.Endpoint,
		"network":    n.cfg.Network,
		"version":    n.version,
		"listen":     n.cfg.Listen,
		"data_dir":   n.cfg.DataDir,
		"bootstrap":  n.cfg.Bootstrap,
		"allow_from": n.cfg.AllowFrom,
		"wake_mode":  wakeMode,
	}
}

func (n *Node) configView() map[string]any {
	c := n.cfg
	view := map[string]any{
		"peer_id":             n.id.PeerID,
		"name":                c.Name,
		"desc":                c.Desc,
		"kind":                c.Kind,
		"network":             c.Network,
		"listen":              c.Listen,
		"endpoint":            c.Endpoint,
		"data_dir":            c.DataDir,
		"bootstrap":           c.Bootstrap,
		"allow_from":          c.AllowFrom,
		"version":             n.version,
		"access":              n.access().Effective(),
		"accept_timeout_min":  c.AcceptTimeoutMin,
		"deliver_timeout_min": c.DeliverTimeoutMin,
		"job_timeout_min":     c.JobTimeoutMin,
		"anti_entropy_sec":    c.AntiEntropySec,
	}
	if c.Wake != nil {
		view["wake"] = c.Wake
	}
	if len(c.AgentCmd) > 0 {
		view["agent_cmd"] = c.AgentCmd
	}
	return view
}

func (n *Node) handleCtlHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Peer   string          `json:"peer"`
		Action string          `json:"action"`
		ID     string          `json:"id"`
		Args   json.RawMessage `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeCallErr(w, callErr(codeBadRequest, "请求不是合法 JSON"))
		return
	}
	if req.Action == "" {
		writeCallErr(w, callErr(codeBadRequest, "缺少 action"))
		return
	}
	data, err := n.Call(r.Context(), req.Peer, req.Action, req.ID, req.Args)
	if err != nil {
		writeCallErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func writeCallErr(w http.ResponseWriter, err error) {
	var ce *callError
	if errors.As(err, &ce) {
		writeJSON(w, statusFor(ce.code), map[string]any{"error": ce.msg, "error_code": ce.code})
		return
	}
	writeErr(w, http.StatusInternalServerError, err)
}

func statusFor(code string) int {
	switch code {
	case codeDenied:
		return http.StatusForbidden
	case codeUnreachable, codeRemote:
		return http.StatusBadGateway
	case codeTimeout:
		return http.StatusGatewayTimeout
	case codeNotFound:
		return http.StatusNotFound
	case codeUnsupported:
		return http.StatusNotImplemented
	default:
		return http.StatusBadRequest
	}
}

func classifySend(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return callErr(codeTimeout, "超时")
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return callErr(codeTimeout, "超时")
	}
	var se *transport.StatusError
	if errors.As(err, &se) {
		return callErr(codeRemote, strings.TrimSpace(se.Error()))
	}
	return callErr(codeUnreachable, "离线/不可达")
}

func decodeArgs(raw json.RawMessage, out any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return callErr(codeBadRequest, "参数不是合法 JSON")
	}
	return nil
}

func toJSON(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}
