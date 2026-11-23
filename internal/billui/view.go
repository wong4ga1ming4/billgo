package billui

import (
	"fmt"
	"sort"
	"strconv"

	"billgo/internal/nodestatus"
	"billgo/internal/peers"
	"billgo/internal/ticket"
)

type loginView struct {
	Title  string
	Authed bool
	Error  string
}

type problemView struct {
	Title   string
	Authed  bool
	Message string
	Hint    string
	Retry   string
}

type nodeInfo struct {
	PeerID    string   `json:"peer_id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Network   string   `json:"network"`
	Endpoint  string   `json:"endpoint"`
	Version   string   `json:"version"`
	Listen    string   `json:"listen"`
	Bootstrap []string `json:"bootstrap"`
	AllowFrom []string `json:"allow_from"`
	WakeMode  string   `json:"wake_mode"`
}

func (n nodeInfo) Short() string { return short(n.PeerID) }

type peerRow struct {
	Label    string
	Kind     string
	Endpoint string
	Desc     string
	Deleted  bool
	Href     string
}

type ticketRow struct {
	ID      string
	Status  string
	Current string
	Updated string
	Href    string
}

type homeView struct {
	Title        string
	Authed       bool
	Node         nodeInfo
	Status       nodestatus.Status
	Peers        []peerRow
	Tickets      []ticketRow
	NextHref     string
	CreateAction string
	AccessNote   string
	AccessOpen   bool
}

type auditRow struct {
	When     string
	From     string
	Action   string
	Result   string
	Envelope string
}

type auditView struct {
	Title    string
	Authed   bool
	Rows     []auditRow
	NextHref string
}

type configRow struct {
	Key   string
	Value string
}

type peerView struct {
	Title        string
	Authed       bool
	Label        string
	Config       []configRow
	Tickets      []ticketRow
	NextHref     string
	CreateAction string
	ConfigNote   string
	ListNote     string
}

type ticketView struct {
	Title            string
	Authed           bool
	Back             string
	State            ticket.State
	Events           []ticket.Event
	Updated          string
	Running          bool
	VoidAction       string
	RedispatchAction string
}

func accessNote(cfg map[string]any) (string, bool) {
	raw, _ := cfg["access"].(map[string]any)
	def, _ := raw["default"].(string)
	if def == "" {
		def = "allow"
	}
	if def == "allow" {
		return "远程读默认开放：没被单独拒绝的邻居可以读工单和配置。写仍然默认拒绝，必须在规则里显式允许。", true
	}
	return "远程读默认拒绝：只有规则里允许的邻居能读。写仍然必须显式允许。", false
}

func peerRows(list []peers.Record, self string) []peerRow {
	out := make([]peerRow, 0, len(list))
	for _, p := range list {
		label := p.Name
		if label == "" {
			label = p.PeerID
		}
		if len(p.PeerID) >= 4 {
			label += "#" + p.PeerID[:4]
		}
		row := peerRow{Label: label, Kind: p.Kind, Endpoint: p.Endpoint, Desc: p.Desc, Deleted: p.Deleted}
		if !p.Deleted && p.PeerID != self && validPeer(p.PeerID) {
			row.Href = "/peers/" + p.PeerID
		}
		out = append(out, row)
	}
	return out
}

func ticketRows(list []ticket.State, prefix string) []ticketRow {
	out := make([]ticketRow, 0, len(list))
	for _, st := range list {
		cur := "—"
		if h := st.Current(); h != nil {
			cur = fmt.Sprintf("第%d跳 %s (%s)", h.Step, short(h.Target), h.State)
		}
		out = append(out, ticketRow{
			ID:      st.ID,
			Status:  st.Status,
			Current: cur,
			Updated: fmtTime(st.UpdatedAt),
			Href:    prefix + st.ID,
		})
	}
	return out
}

func configRows(cfg map[string]any) []configRow {
	keys := make([]string, 0, len(cfg))
	for k, v := range cfg {
		if skipConfig(k, v) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]configRow, 0, len(keys))
	for _, k := range keys {
		out = append(out, configRow{Key: k, Value: fmtConfig(cfg[k])})
	}
	return out
}

func skipConfig(key string, _ any) bool {
	switch key {
	case "wake", "agent_cmd":
		return true
	default:
		return false
	}
}

func fmtConfig(v any) string {
	switch x := v.(type) {
	case string:
		if x == "" {
			return "—"
		}
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case []any:
		return fmt.Sprintf("%d 项", len(x))
	case map[string]any:
		return fmt.Sprintf("%d 项", len(x))
	case nil:
		return "—"
	default:
		return fmt.Sprint(x)
	}
}
