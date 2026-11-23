package nodestatus

import (
	"testing"
	"time"

	"billgo/internal/outbox"
	"billgo/internal/peers"
	"billgo/internal/ticket"
)

type fakeTickets struct{ states []ticket.State }

func (f fakeTickets) List() []ticket.State { return f.states }

type fakeDir struct{}

func (fakeDir) Lookup(string) (peers.Record, bool) { return peers.Record{}, false }

type fakeOutbox struct{ items []outbox.Item }

func (f fakeOutbox) List() []outbox.Item { return f.items }

func TestComputeNoPeer(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	st := Compute(fakeTickets{states: []ticket.State{{
		ID: "t1", Status: ticket.StatusRunning, UpdatedAt: now.UnixMilli() - 5000,
		Hops: []ticket.HopState{{Step: 1, Target: "gone", State: ticket.HopPending}},
	}}}, fakeDir{}, fakeOutbox{}, now)

	if st.Summary.Tickets[ticket.StatusRunning] != 1 {
		t.Fatalf("汇总不对: %+v", st.Summary)
	}
	if len(st.Attention) != 1 || st.Attention[0].Kind != "no_peer" {
		t.Fatalf("应产生 no_peer: %+v", st.Attention)
	}
	if st.Attention[0].AgeSec != 5 {
		t.Fatalf("age 不对: %+v", st.Attention[0])
	}
}

func TestComputeStuckOutbox(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	st := Compute(fakeTickets{}, fakeDir{}, fakeOutbox{items: []outbox.Item{{
		ID: 1, PeerID: "p", Attempts: 6, CreatedAt: now.UnixMilli() - 11*60*1000, LastErr: "超时",
	}}}, now)

	if st.Summary.Outbox["stuck"] != 1 || st.Summary.Outbox["pending"] != 1 {
		t.Fatalf("队列汇总不对: %+v", st.Summary.Outbox)
	}
	if len(st.Attention) != 1 || st.Attention[0].Kind != "outbox_stuck" {
		t.Fatalf("应产生 outbox_stuck: %+v", st.Attention)
	}
}
