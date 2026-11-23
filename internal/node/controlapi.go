package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"billgo/internal/audit"
	"billgo/internal/nodestatus"
	"billgo/internal/ticket"
	"billgo/internal/wake"
)

// controlMux 本地控制面的路由。只对本机开放。
func (n *Node) controlMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /node", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, n.nodeInfo())
	})
	mux.HandleFunc("POST /ctl", n.handleCtlHTTP)
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, nodestatus.Compute(n.tickets, n.peers, n.out, time.Now()))
	})
	mux.HandleFunc("GET /peers", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, n.peers.ListAll())
	})
	mux.HandleFunc("GET /tickets", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, n.tickets.List())
	})
	mux.HandleFunc("POST /tickets", func(w http.ResponseWriter, r *http.Request) {
		var req ticket.CreateReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		for i := range req.Flow {
			id, err := n.resolvePeer(req.Flow[i].To)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			req.Flow[i].To = id
		}
		tid, err := n.tickets.Create(req)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": tid})
	})
	mux.HandleFunc("GET /tickets/{id}", func(w http.ResponseWriter, r *http.Request) {
		st, evs, err := n.tickets.Get(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"state": st, "events": evs})
	})
	mux.HandleFunc("POST /tickets/{id}/redispatch", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Hop int    `json:"hop"`
			To  string `json:"to"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		to, err := n.resolvePeer(req.To)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := n.tickets.Redispatch(r.PathValue("id"), req.Hop, to); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /tickets/{id}/void", func(w http.ResponseWriter, r *http.Request) {
		if err := n.tickets.Void(r.PathValue("id")); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /withdraw", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := n.peers.Withdraw(ctx); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /audit", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		items, next, err := n.audit.List(limit, r.URL.Query().Get("cursor"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if items == nil {
			items = []audit.Record{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
	})
	mux.HandleFunc("GET /outbox", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, n.out.List())
	})
	mux.HandleFunc("POST /wake", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Peer string `json:"peer"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		id, err := n.resolvePeer(req.Peer)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		rec, ok := n.peers.Lookup(id)
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Errorf("名录里没有 %s", id))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := wake.Do(ctx, rec.Wake); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	return mux
}

// resolvePeer 接受 peer_id 或别名，返回唯一确定的 peer_id。
func (n *Node) resolvePeer(s string) (string, error) {
	if _, ok := n.peers.Lookup(s); ok {
		return s, nil
	}
	matches := n.peers.ByName(s)
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("名录里没有节点 %q", s)
	case 1:
		return matches[0].PeerID, nil
	default:
		ids := ""
		for _, m := range matches {
			ids += fmt.Sprintf("\n  %s (%s)", m.Name+"#"+m.PeerID[:4], m.PeerID)
		}
		return "", fmt.Errorf("节点名 %q 有多个匹配，请用 peer_id 指定:%s", s, ids)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}
