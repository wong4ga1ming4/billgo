// Package access 决定一个邻居能否对本节点执行某个远程动作。
//
// 读动作在 default=allow 时默认放行。写动作始终默认拒绝，
// 只有规则显式 allow 才放行。社区透明是读，不是谁都能改配置。
package access

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// 动作目录。新增能力时追加，不改已有语义。
const (
	NodeInfo          = "node.info"
	PeersList         = "peers.list"
	TicketsList       = "tickets.list"
	TicketsGet        = "tickets.get"
	TicketsCreate     = "tickets.create"
	TicketsRedispatch = "tickets.redispatch"
	TicketsVoid       = "tickets.void"
	ConfigRead        = "config.read"
	ConfigWrite       = "config.write"
	Wake              = "wake"
	LogsRead          = "logs.read" // 目录里预留，当前没有实现
)

const (
	Allow = "allow"
	Deny  = "deny"
)

// 第一版实际能执行的读动作。写动作和 logs.read 只参与权限判断。
var readable = map[string]bool{
	NodeInfo:    true,
	PeersList:   true,
	TicketsList: true,
	TicketsGet:  true,
	ConfigRead:  true,
}

var known = map[string]bool{
	NodeInfo:          true,
	PeersList:         true,
	TicketsList:       true,
	TicketsGet:        true,
	TicketsCreate:     true,
	TicketsRedispatch: true,
	TicketsVoid:       true,
	ConfigRead:        true,
	ConfigWrite:       true,
	Wake:              true,
	LogsRead:          true,
}

var modules = map[string]bool{
	"node": true, "peers": true, "tickets": true, "config": true, "wake": true, "logs": true,
}

// Rule 一条针对某个 peer 的允许或拒绝。peer 为 "*" 时匹配所有人。
type Rule struct {
	Peer  string   `json:"peer"`
	Deny  []string `json:"deny,omitempty"`
	Allow []string `json:"allow,omitempty"`
}

// UnmarshalJSON 拒绝拼错的字段名。
func (r *Rule) UnmarshalJSON(data []byte) error {
	var raw struct {
		Peer  string   `json:"peer"`
		Deny  []string `json:"deny,omitempty"`
		Allow []string `json:"allow,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	*r = Rule{Peer: raw.Peer, Deny: raw.Deny, Allow: raw.Allow}
	return nil
}

// Policy 节点自己的远程访问策略。nil 等同于读允许、写拒绝。
type Policy struct {
	Default string `json:"default,omitempty"`
	Rules   []Rule `json:"rules,omitempty"`
}

// UnmarshalJSON 拒绝拼错的字段名。
func (p *Policy) UnmarshalJSON(data []byte) error {
	var raw struct {
		Default string `json:"default,omitempty"`
		Rules   []Rule `json:"rules,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	*p = Policy{Default: raw.Default, Rules: raw.Rules}
	return nil
}

// Known 报告动作是否在目录里。
func Known(action string) bool { return known[action] }

// Readable 报告这个读动作现在能不能执行。
func Readable(action string) bool { return readable[action] }

// Public 是协议约定公开的动作：身份、名录、工单列表。配置不能把它们关掉。
func Public(action string) bool {
	switch action {
	case NodeInfo, PeersList, TicketsList:
		return true
	default:
		return false
	}
}

// IsWrite 报告动作会不会改节点状态。
func IsWrite(action string) bool {
	switch action {
	case TicketsCreate, TicketsRedispatch, TicketsVoid, ConfigWrite, Wake:
		return true
	default:
		return false
	}
}

// ReadsOpen 表示没被点名的邻居默认可以读。写不受 default 影响，始终默认拒绝。
func (p *Policy) ReadsOpen() bool {
	return p.Effective().Default == Allow
}

// Effective 补齐省略的 default，便于展示。nil 策略变成读允许。
func (p *Policy) Effective() *Policy {
	if p == nil {
		return &Policy{Default: Allow}
	}
	cp := *p
	if cp.Default == "" {
		cp.Default = Allow
	}
	return &cp
}

// Validate 检查配置能加载。拼错的动作直接失败，避免一条永远不命中的规则。
func (p *Policy) Validate() error {
	if p == nil {
		return nil
	}
	switch p.Default {
	case "", Allow, Deny:
	default:
		return fmt.Errorf("access.default 只能是 %q 或 %q，得到 %q", Allow, Deny, p.Default)
	}
	for i, r := range p.Rules {
		if strings.TrimSpace(r.Peer) == "" {
			return fmt.Errorf("access.rules[%d] 缺少 peer", i)
		}
		if strings.ContainsAny(r.Peer, " \t") {
			return fmt.Errorf("access.rules[%d] 的 peer 含空白", i)
		}
		if len(r.Deny) == 0 && len(r.Allow) == 0 {
			return fmt.Errorf("access.rules[%d] 至少要有 allow 或 deny", i)
		}
		for _, pat := range append(append([]string{}, r.Deny...), r.Allow...) {
			if !validPattern(pat) {
				return fmt.Errorf("access.rules[%d] 含未知动作 %q", i, pat)
			}
		}
		for _, pat := range r.Deny {
			for _, action := range []string{NodeInfo, PeersList, TicketsList} {
				if matches([]string{pat}, action) {
					return fmt.Errorf("access.rules[%d] 不能拒绝 %s", i, action)
				}
			}
		}
	}
	return nil
}

// Decide 判断 peer 能否执行 action。
//
// 先看这个 peer 自己的规则，再看 peer 为 "*" 的规则，最后才用 default。
// 同一层里 deny 优先于 allow。规则没提到的动作继续往下看。
// 写动作走到最后仍然拒绝，不受 default=allow 影响。
func (p *Policy) Decide(peer, action string) bool {
	if !Known(action) {
		return false
	}
	if Public(action) {
		return true
	}
	eff := p.Effective()
	if allowed, ok := match(eff.Rules, peer, action); ok {
		return allowed
	}
	if peer != "*" {
		if allowed, ok := match(eff.Rules, "*", action); ok {
			return allowed
		}
	}
	if IsWrite(action) {
		return false
	}
	return eff.Default == Allow
}

// match 在同一 peer 的全部规则里找这个动作。没提到则 ok=false。
func match(rules []Rule, peer, action string) (allowed bool, ok bool) {
	deny, allow := false, false
	for _, r := range rules {
		if r.Peer != peer {
			continue
		}
		if matches(r.Deny, action) {
			deny = true
			ok = true
		}
		if matches(r.Allow, action) {
			allow = true
			ok = true
		}
	}
	if !ok {
		return false, false
	}
	if deny {
		return false, true
	}
	return allow, true
}

func matches(patterns []string, action string) bool {
	for _, pat := range patterns {
		if pat == action {
			return true
		}
		mod, wild, ok := strings.Cut(pat, ".*")
		if ok && wild == "" && (action == mod || strings.HasPrefix(action, mod+".")) {
			return true
		}
	}
	return false
}

func validPattern(pat string) bool {
	if Known(pat) {
		return true
	}
	mod, wild, ok := strings.Cut(pat, ".*")
	return ok && wild == "" && modules[mod]
}
