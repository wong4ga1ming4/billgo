package webui

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"os"
	"strings"

	"billgo/internal/blob"
	"billgo/internal/ticket"
)

type peerView struct {
	PeerID   string `json:"peer_id"`
	Name     string `json:"name"`
	Desc     string `json:"desc"`
	Kind     string `json:"kind"`
	Endpoint string `json:"endpoint"`
	Network  string `json:"network"`
	Version  uint64 `json:"version"`
	Deleted  bool   `json:"deleted"`
	WakeMode string `json:"wake_mode,omitempty"`
}

type artifactView struct {
	Hop    int    `json:"hop"`
	Blob   string `json:"blob"`
	Size   int64  `json:"size"`
	TS     int64  `json:"ts"`
	Signer string `json:"signer"`
	URL    string `json:"url"`
}

func (s *Server) apiNode(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"peer_id":    s.info.PeerID,
		"name":       s.info.Name,
		"kind":       s.info.Kind,
		"network":    s.info.Network,
		"endpoint":   s.info.Endpoint,
		"version":    s.info.Version,
		"listen":     s.info.Listen,
		"bootstrap":  s.info.Bootstrap,
		"allow_from": s.info.AllowFrom,
		"wake_mode":  s.info.WakeMode,
	})
}

func (s *Server) apiPeers(w http.ResponseWriter, _ *http.Request) {
	recs := s.dir.ListAll()
	out := make([]peerView, 0, len(recs))
	for _, r := range recs {
		v := peerView{
			PeerID:   r.PeerID,
			Name:     r.Name,
			Desc:     r.Desc,
			Kind:     r.Kind,
			Endpoint: r.Endpoint,
			Network:  r.Network,
			Version:  r.Version,
			Deleted:  r.Deleted,
		}
		if r.Wake != nil {
			v.WakeMode = r.Wake.Mode
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiTickets(w http.ResponseWriter, _ *http.Request) {
	out := s.tk.List()
	if out == nil {
		out = []ticket.State{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiTicket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	st, evs, err := s.tk.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	receipts, err := s.tk.Receipts(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if evs == nil {
		evs = []ticket.Event{}
	}
	if receipts == nil {
		receipts = []ticket.Receipt{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state":    st,
		"events":   evs,
		"receipts": receipts,
	})
}

func (s *Server) apiArtifacts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	receipts, err := s.tk.Receipts(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]artifactView, 0, len(receipts))
	for _, rc := range receipts {
		out = append(out, artifactView{
			Hop:    rc.Hop,
			Blob:   rc.Blob,
			Size:   rc.Size,
			TS:     rc.TS,
			Signer: rc.From,
			URL:    "/api/blobs/" + rc.Blob,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiBlob(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !blob.ValidHash(hash) {
		http.NotFound(w, r)
		return
	}
	path := blob.Path(s.blobs, hash)
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	name := fmt.Sprintf("billgo-%s.bin", hash[:12])
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	http.ServeContent(w, r, name, info.ModTime(), f)
}

func (s *Server) apiCreate(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && !originAllowed(origin) {
		writeError(w, http.StatusForbidden, fmt.Errorf("跨源请求被拒绝"))
		return
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, fmt.Errorf("只接受 application/json"))
		return
	}
	var req struct {
		Form json.RawMessage `json:"form"`
		Flow []string        `json:"flow"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	hops := make([]ticket.Hop, 0, len(req.Flow))
	for i, name := range req.Flow {
		id, err := s.resolvePeer(name)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		h := ticket.Hop{Step: i + 1, To: id, Desc: name}
		if i > 0 {
			h.DependsOn = []int{i}
		}
		hops = append(hops, h)
	}
	tid, err := s.tk.Create(ticket.CreateReq{Form: req.Form, Flow: hops})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": tid})
}

func (s *Server) resolvePeer(name string) (string, error) {
	if _, ok := s.dir.Lookup(name); ok {
		return name, nil
	}
	matches := s.dir.ByName(name)
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("名录里没有节点 %q", name)
	case 1:
		return matches[0].PeerID, nil
	default:
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, m.Name+"#"+m.PeerID[:4])
		}
		return "", fmt.Errorf("节点名 %q 有多个匹配：%s", name, strings.Join(ids, ", "))
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
