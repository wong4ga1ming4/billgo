// Package ticket 是工单本体：事件日志、状态推导、派单与接活。
//
// 工单状态不单独存表，全部由追加写的事件日志推导。
package ticket

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"billgo/internal/envelope"
)

// 事件类型。
const (
	EvCreated   = "ticket.created"
	EvSent      = "hop.sent"
	EvAccepted  = "hop.accepted"
	EvRejected  = "hop.rejected"
	EvDelivered = "hop.delivered"
	EvVerified  = "hop.verified"
	EvExpired   = "hop.expired"
	EvAmended   = "ticket.amended"
	EvClosed    = "ticket.closed"
	EvVoided    = "ticket.voided"
)

// 每跳状态。
const (
	HopPending   = "pending"
	HopSent      = "sent"
	HopAccepted  = "accepted"
	HopDelivered = "delivered"
	HopVerified  = "verified"
	HopRejected  = "rejected"
	HopExpired   = "expired"
)

// 工单整体状态。
const (
	StatusRunning = "running"
	StatusClosed  = "closed"
	StatusVoided  = "voided"
)

// Hop 流转图的一跳。v1 校验强制成链，depends_on 为将来 DAG 预留。
type Hop struct {
	Step      int    `json:"step"`
	To        string `json:"to"`
	DependsOn []int  `json:"depends_on,omitempty"`
	Desc      string `json:"desc,omitempty"`
}

// CreateReq 创建工单的请求。
type CreateReq struct {
	Form json.RawMessage `json:"form"`
	Flow []Hop           `json:"flow"`
}

// Validate 校验表单与流转图。v1 只接受一条链。
func (r *CreateReq) Validate() error {
	if len(r.Form) == 0 {
		return fmt.Errorf("%w: form 不能为空", envelope.ErrMalformed)
	}
	var probe any
	if err := json.Unmarshal(r.Form, &probe); err != nil {
		return fmt.Errorf("%w: form 不是合法 JSON", envelope.ErrMalformed)
	}
	if len(r.Flow) == 0 {
		return fmt.Errorf("%w: flow 至少一跳", envelope.ErrMalformed)
	}
	seen := map[string]bool{}
	for i, h := range r.Flow {
		if h.Step != i+1 {
			return fmt.Errorf("%w: 第 %d 跳的 step 应为 %d", envelope.ErrMalformed, i+1, i+1)
		}
		if h.To == "" {
			return fmt.Errorf("%w: 第 %d 跳缺少目标节点", envelope.ErrMalformed, h.Step)
		}
		if seen[h.To] {
			return fmt.Errorf("%w: 节点 %s 在流转图里重复出现", envelope.ErrMalformed, h.To)
		}
		seen[h.To] = true
		if i == 0 {
			if len(h.DependsOn) != 0 {
				return fmt.Errorf("%w: 第一跳不应有前置", envelope.ErrMalformed)
			}
		} else if len(h.DependsOn) != 1 || h.DependsOn[0] != h.Step-1 {
			return fmt.Errorf("%w: v1 只支持链式流转，第 %d 跳应依赖第 %d 跳", envelope.ErrMalformed, h.Step, h.Step-1)
		}
	}
	return nil
}

// Event 事件日志的一行。
type Event struct {
	Seq  uint64          `json:"seq"`
	Type string          `json:"type"`
	TS   int64           `json:"ts"`
	Hop  int             `json:"hop,omitempty"`
	Peer string          `json:"peer,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// HopState 推导出的某一跳状态。
type HopState struct {
	Step       int    `json:"step"`
	Target     string `json:"target"`
	Desc       string `json:"desc,omitempty"`
	State      string `json:"state"`
	SentAt     int64  `json:"sent_at,omitempty"`
	AcceptedAt int64  `json:"accepted_at,omitempty"`
}

// State 推导出的整张工单状态。
type State struct {
	ID        string          `json:"id"`
	Status    string          `json:"status"`
	Form      json.RawMessage `json:"form"`
	Hops      []HopState      `json:"hops"`
	CreatedAt int64           `json:"created_at"`
	UpdatedAt int64           `json:"updated_at"`
}

// Current 返回当前正在走的那一跳；全部完成时返回 nil。
func (s *State) Current() *HopState {
	for i := range s.Hops {
		if s.Hops[i].State != HopVerified {
			return &s.Hops[i]
		}
	}
	return nil
}

// Receipt 接活方签名的一跳交付凭证。
type Receipt struct {
	Ticket string `json:"ticket"`
	Hop    int    `json:"hop"`
	Blob   string `json:"blob"`
	Size   int64  `json:"size"`
	From   string `json:"from"`
	To     string `json:"to"`
	TS     int64  `json:"ts"`
	Sig    string `json:"sig"`
}

// SignReceipt 用交付方私钥签回执。
func SignReceipt(priv ed25519.PrivateKey, r *Receipt) error {
	r.Sig = ""
	canon, err := envelope.Canonicalize(r)
	if err != nil {
		return err
	}
	r.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, canon))
	return nil
}

// VerifyReceipt 用交付方公钥核对回执。
func VerifyReceipt(pub ed25519.PublicKey, r Receipt) error {
	sig, err := base64.StdEncoding.DecodeString(r.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w: 回执签名格式错误", envelope.ErrSignature)
	}
	sigSaved := r.Sig
	r.Sig = ""
	canon, err := envelope.Canonicalize(r)
	if err != nil {
		return err
	}
	r.Sig = sigSaved
	if !ed25519.Verify(pub, canon, sig) {
		return fmt.Errorf("%w: 回执签名无效", envelope.ErrSignature)
	}
	return nil
}

// IssueBody hop.issue 的 body。
type IssueBody struct {
	Form json.RawMessage `json:"form"`
}

// DeliverBody hop.deliver 的 body。
type DeliverBody struct {
	Blob    string  `json:"blob"`
	Size    int64   `json:"size"`
	Receipt Receipt `json:"receipt"`
}

// RejectBody hop.reject 的 body。
type RejectBody struct {
	Reason string `json:"reason"`
}

// AmendData ticket.amended 的 data。
type AmendData struct {
	Hop int    `json:"hop"`
	To  string `json:"to"`
}

// createdData ticket.created 的 data。
type createdData struct {
	Form json.RawMessage `json:"form"`
	Flow []Hop           `json:"flow"`
}

// DeliveredData hop.delivered 的 data。
type DeliveredData struct {
	Blob string `json:"blob"`
	Size int64  `json:"size"`
}

// Derive 由事件日志推导工单状态。
func Derive(id string, events []Event) (State, error) {
	st := State{ID: id, Status: StatusRunning}
	index := map[int]*HopState{}

	for _, ev := range events {
		if ev.TS > st.UpdatedAt {
			st.UpdatedAt = ev.TS
		}
		switch ev.Type {
		case EvCreated:
			var c createdData
			if err := json.Unmarshal(ev.Data, &c); err != nil {
				return st, fmt.Errorf("解析创建事件: %w", err)
			}
			st.Form = c.Form
			st.CreatedAt = ev.TS
			st.UpdatedAt = ev.TS
			st.Hops = make([]HopState, 0, len(c.Flow))
			for _, h := range c.Flow {
				st.Hops = append(st.Hops, HopState{Step: h.Step, Target: h.To, Desc: h.Desc, State: HopPending})
			}
			// 切片一次性建好后不再扩容，指针才稳定。
			for i := range st.Hops {
				index[st.Hops[i].Step] = &st.Hops[i]
			}
		case EvSent:
			if h := index[ev.Hop]; h != nil {
				h.State = HopSent
				h.SentAt = ev.TS
			}
		case EvAccepted:
			if h := index[ev.Hop]; h != nil {
				h.State = HopAccepted
				h.AcceptedAt = ev.TS
			}
		case EvRejected:
			if h := index[ev.Hop]; h != nil {
				h.State = HopRejected
			}
		case EvExpired:
			if h := index[ev.Hop]; h != nil {
				h.State = HopExpired
			}
		case EvDelivered:
			if h := index[ev.Hop]; h != nil {
				h.State = HopDelivered
			}
		case EvVerified:
			if h := index[ev.Hop]; h != nil {
				h.State = HopVerified
			}
		case EvAmended:
			var a AmendData
			if err := json.Unmarshal(ev.Data, &a); err == nil {
				if h := index[a.Hop]; h != nil {
					h.Target = a.To
					h.State = HopPending
					h.SentAt = 0
					h.AcceptedAt = 0
				}
			}
		case EvClosed:
			st.Status = StatusClosed
		case EvVoided:
			st.Status = StatusVoided
		}
	}
	// index 里的指针指向 st.Hops 的切片元素；切片扩容后指针会失效，
	// 所以最后按 step 重新对齐一次（v1 的 flow 在创建时一次性确定）。
	return st, nil
}
