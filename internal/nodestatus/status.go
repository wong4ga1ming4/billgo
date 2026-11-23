// Package nodestatus 从本地工单、名录和 outbox 派生节点状态，
// 供本机页面和控制面共用。
package nodestatus

import (
	"fmt"
	"time"

	"billgo/internal/outbox"
	"billgo/internal/peers"
	"billgo/internal/ticket"
)

const (
	outboxStuckAttempts = 5
	outboxStuckAge      = 10 * time.Minute
)

// Summary 数量汇总。
type Summary struct {
	Tickets map[string]int `json:"tickets"`
	Hops    map[string]int `json:"hops"`
	Outbox  map[string]int `json:"outbox"`
}

// AttentionItem 需要发起方处理的一件事。
type AttentionItem struct {
	Kind       string `json:"kind"`
	Ticket     string `json:"ticket,omitempty"`
	Hop        int    `json:"hop,omitempty"`
	Peer       string `json:"peer,omitempty"`
	Reason     string `json:"reason,omitempty"`
	AgeSec     int64  `json:"age_sec"`
	NextAction string `json:"next_action"`
	Hint       string `json:"hint"`
}

// Status 一个节点的状态快照。
type Status struct {
	Summary   Summary         `json:"summary"`
	Attention []AttentionItem `json:"attention"`
}

// Tickets 工单只读能力。
type Tickets interface {
	List() []ticket.State
}

// Directory 名录只读能力。
type Directory interface {
	Lookup(peerID string) (peers.Record, bool)
}

// Outbox 待发队列只读能力。
type Outbox interface {
	List() []outbox.Item
}

// Compute 从本地数据派生状态。now 由调用方传入，便于测试。
func Compute(tickets Tickets, dir Directory, out Outbox, now time.Time) Status {
	sum := Summary{
		Tickets: map[string]int{ticket.StatusRunning: 0, ticket.StatusClosed: 0, ticket.StatusVoided: 0},
		Hops: map[string]int{
			ticket.HopPending: 0, ticket.HopSent: 0, ticket.HopAccepted: 0,
			ticket.HopDelivered: 0, ticket.HopVerified: 0,
			ticket.HopRejected: 0, ticket.HopExpired: 0,
		},
		Outbox: map[string]int{"pending": 0, "stuck": 0},
	}
	att := []AttentionItem{}

	for _, st := range tickets.List() {
		sum.Tickets[st.Status]++
		for _, h := range st.Hops {
			sum.Hops[h.State]++
		}
		if st.Status != ticket.StatusRunning {
			continue
		}
		cur := st.Current()
		if cur == nil {
			continue
		}
		switch cur.State {
		case ticket.HopRejected, ticket.HopExpired:
			reason := fmt.Sprintf("第 %d 跳被拒绝", cur.Step)
			if cur.State == ticket.HopExpired {
				reason = fmt.Sprintf("第 %d 跳超时", cur.Step)
			}
			att = append(att, AttentionItem{
				Kind:       "hop_" + cur.State,
				Ticket:     st.ID,
				Hop:        cur.Step,
				Peer:       cur.Target,
				Reason:     reason,
				AgeSec:     ageSec(now, st.UpdatedAt),
				NextAction: "redispatch|void",
				Hint: fmt.Sprintf("billgo ticket redispatch %s --hop %d --to %s",
					st.ID, cur.Step, cur.Target),
			})
		case ticket.HopPending, ticket.HopSent, ticket.HopAccepted:
			if _, ok := dir.Lookup(cur.Target); !ok {
				att = append(att, AttentionItem{
					Kind:       "no_peer",
					Ticket:     st.ID,
					Hop:        cur.Step,
					Peer:       cur.Target,
					Reason:     "目标节点已不在名录里",
					AgeSec:     ageSec(now, st.UpdatedAt),
					NextAction: "amend",
					Hint: fmt.Sprintf("billgo ticket redispatch %s --hop %d --to <节点名>",
						st.ID, cur.Step),
				})
			}
		}
	}

	items := out.List()
	sum.Outbox["pending"] = len(items)
	for _, it := range items {
		old := it.CreatedAt > 0 && now.Sub(time.UnixMilli(it.CreatedAt)) > outboxStuckAge
		if it.Attempts < outboxStuckAttempts && !old {
			continue
		}
		sum.Outbox["stuck"]++
		reason := it.LastErr
		if reason == "" {
			reason = "多次重试仍未送达"
		}
		att = append(att, AttentionItem{
			Kind:       "outbox_stuck",
			Peer:       it.PeerID,
			Reason:     reason,
			AgeSec:     ageSec(now, it.CreatedAt),
			NextAction: "inspect_peer",
			Hint:       "billgo outbox",
		})
	}

	return Status{Summary: sum, Attention: att}
}

func ageSec(now time.Time, ms int64) int64 {
	if ms <= 0 {
		return 0
	}
	d := now.Sub(time.UnixMilli(ms))
	if d < 0 {
		return 0
	}
	return int64(d.Seconds())
}
