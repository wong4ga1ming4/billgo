package access

import (
	"strings"
	"testing"
)

func TestDecideSpecificity(t *testing.T) {
	p := &Policy{
		Default: Allow,
		Rules: []Rule{
			{Peer: "*", Deny: []string{"logs.read"}},
			{Peer: "peer_x", Allow: []string{"config.write", "tickets.list"}},
			{Peer: "peer_y", Deny: []string{"tickets.*"}, Allow: []string{"tickets.list"}},
		},
	}
	cases := []struct {
		peer, action string
		want         bool
	}{
		{"peer_x", ConfigWrite, true},
		{"peer_x", TicketsList, true},
		{"peer_x", LogsRead, false},
		{"peer_x", TicketsCreate, false},
		{"peer_z", TicketsList, true},
		{"peer_z", ConfigWrite, false},
		{"peer_z", LogsRead, false},
		{"peer_y", TicketsList, true},
		{"peer_y", TicketsGet, false},
		{"peer_y", ConfigRead, true},
		{"nobody", Wake, false},
		{"peer_x", "no.such", false},
	}
	for _, c := range cases {
		if got := p.Decide(c.peer, c.action); got != c.want {
			t.Errorf("%s %s: got %v want %v", c.peer, c.action, got, c.want)
		}
	}
}

func TestNilPolicyReadsOpenWritesClosed(t *testing.T) {
	var p *Policy
	if !p.Decide("any", NodeInfo) || !p.Decide("any", ConfigRead) {
		t.Fatal("缺省策略应允许读")
	}
	if p.Decide("any", ConfigWrite) || p.Decide("any", TicketsCreate) {
		t.Fatal("缺省策略应拒绝写")
	}
}

func TestReadsOpen(t *testing.T) {
	if !(*Policy)(nil).ReadsOpen() {
		t.Fatal("省略 access 时读是开放的")
	}
	if (&Policy{Default: Deny}).ReadsOpen() {
		t.Fatal("default=deny 时读不是默认开放")
	}
}

func TestDefaultDenyStillNeedsExplicitRead(t *testing.T) {
	p := &Policy{
		Default: Deny,
		Rules:   []Rule{{Peer: "peer_x", Allow: []string{"tickets.list"}}},
	}
	if !p.Decide("peer_x", TicketsList) {
		t.Fatal("显式 allow 应放行")
	}
	if p.Decide("peer_x", ConfigRead) || p.Decide("peer_y", TicketsGet) {
		t.Fatal("default=deny 时未点名的内容读应拒绝")
	}
	if !p.Decide("peer_y", TicketsList) || !p.Decide("peer_y", NodeInfo) || !p.Decide("peer_y", PeersList) {
		t.Fatal("身份、名录和工单列表不随 default=deny 关闭")
	}
}

func TestValidate(t *testing.T) {
	ok := &Policy{Default: Allow, Rules: []Rule{{Peer: "*", Deny: []string{"logs.*"}}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []Policy{
		{Default: "open"},
		{Rules: []Rule{{Peer: ""}}},
		{Rules: []Rule{{Peer: "x", Deny: []string{"ticket.list"}}}},
		{Rules: []Rule{{Peer: "x"}}},
		{Rules: []Rule{{Peer: "x", Deny: []string{"tickets.list"}}}},
		{Rules: []Rule{{Peer: "x", Deny: []string{"tickets.*"}}}},
		{Rules: []Rule{{Peer: "*", Deny: []string{"node.info"}}}},
	}
	for i, p := range bad {
		if err := p.Validate(); err == nil {
			t.Fatalf("第 %d 条应失败: %+v", i, p)
		}
	}
	if err := (&Policy{Default: "open"}).Validate(); err == nil || !strings.Contains(err.Error(), "default") {
		t.Fatalf("非法 default 应报错，得到 %v", err)
	}
}
