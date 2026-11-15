package ticket

import (
	"encoding/json"
	"strings"
	"testing"

	"billgo/internal/identity"
)

func TestCreateReqValidate(t *testing.T) {
	form := json.RawMessage(`{"task":"写文档"}`)
	cases := []struct {
		name string
		req  CreateReq
		want string
	}{
		{"空表单", CreateReq{Flow: []Hop{{Step: 1, To: "a"}}}, "form"},
		{"空流转图", CreateReq{Form: form}, "flow"},
		{"step 不连续", CreateReq{Form: form, Flow: []Hop{{Step: 2, To: "a"}}}, "step"},
		{"缺目标", CreateReq{Form: form, Flow: []Hop{{Step: 1}}}, "目标"},
		{"重复节点", CreateReq{Form: form, Flow: []Hop{{Step: 1, To: "a"}, {Step: 2, To: "a", DependsOn: []int{1}}}}, "重复"},
		{"第二跳没依赖", CreateReq{Form: form, Flow: []Hop{{Step: 1, To: "a"}, {Step: 2, To: "b"}}}, "依赖"},
		{"第一跳有依赖", CreateReq{Form: form, Flow: []Hop{{Step: 1, To: "a", DependsOn: []int{1}}}}, "前置"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.req.Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("期望包含 %q 的错误，得到 %v", c.want, err)
			}
		})
	}
	ok := CreateReq{Form: form, Flow: []Hop{{Step: 1, To: "a"}, {Step: 2, To: "b", DependsOn: []int{1}}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法流转图被拒: %v", err)
	}
}

func bytes32(v byte) []byte {
	s := make([]byte, 32)
	s[0] = v
	return s
}

func data(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDeriveLifecycle(t *testing.T) {
	evs := []Event{
		{Seq: 1, Type: EvCreated, TS: 100, Data: data(t, createdData{
			Form: json.RawMessage(`{"x":1}`),
			Flow: []Hop{{Step: 1, To: "b"}, {Step: 2, To: "c", DependsOn: []int{1}}},
		})},
		{Seq: 2, Type: EvSent, TS: 110, Hop: 1},
		{Seq: 3, Type: EvAccepted, TS: 120, Hop: 1},
		{Seq: 4, Type: EvDelivered, TS: 130, Hop: 1},
		{Seq: 5, Type: EvVerified, TS: 140, Hop: 1},
		{Seq: 6, Type: EvSent, TS: 150, Hop: 2},
		{Seq: 7, Type: EvAccepted, TS: 160, Hop: 2},
		{Seq: 8, Type: EvDelivered, TS: 170, Hop: 2},
		{Seq: 9, Type: EvVerified, TS: 180, Hop: 2},
		{Seq: 10, Type: EvClosed, TS: 190},
	}
	st, err := Derive("t1", evs)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != StatusClosed {
		t.Fatalf("状态 %s", st.Status)
	}
	if st.Current() != nil {
		t.Fatal("全部完成后 Current 应为 nil")
	}
	if st.Hops[0].State != HopVerified || st.Hops[1].Target != "c" {
		t.Fatalf("推导结果不对: %+v", st.Hops)
	}
	if st.CreatedAt != 100 || st.UpdatedAt != 190 {
		t.Fatalf("时间不对: %+v", st)
	}
}

func TestDeriveAmendResets(t *testing.T) {
	evs := []Event{
		{Seq: 1, Type: EvCreated, TS: 100, Data: data(t, createdData{
			Form: json.RawMessage(`{}`),
			Flow: []Hop{{Step: 1, To: "b"}},
		})},
		{Seq: 2, Type: EvSent, TS: 110, Hop: 1},
		{Seq: 3, Type: EvRejected, TS: 120, Hop: 1},
		{Seq: 4, Type: EvAmended, TS: 130, Hop: 1, Data: data(t, AmendData{Hop: 1, To: "c"})},
	}
	st, err := Derive("t1", evs)
	if err != nil {
		t.Fatal(err)
	}
	if st.Hops[0].State != HopPending || st.Hops[0].Target != "c" {
		t.Fatalf("改派后应回到 pending: %+v", st.Hops[0])
	}
	if st.Hops[0].SentAt != 0 {
		t.Fatalf("改派后应清空时间: %+v", st.Hops[0])
	}
	if st.Status != StatusRunning {
		t.Fatalf("状态 %s", st.Status)
	}
}

func TestReceiptSignVerify(t *testing.T) {
	id := identity.FromSeed(make([]byte, 32))
	r := Receipt{Ticket: "t1", Hop: 1, Blob: strings.Repeat("a", 64), Size: 3, From: id.PeerID, To: "x", TS: 1}
	if err := SignReceipt(id.Private, &r); err != nil {
		t.Fatal(err)
	}
	if err := VerifyReceipt(id.Public, r); err != nil {
		t.Fatalf("应验签通过: %v", err)
	}
	r.Size = 4
	if err := VerifyReceipt(id.Public, r); err == nil {
		t.Fatal("篡改后应验签失败")
	}
	other := identity.FromSeed(bytes32(9))
	if err := VerifyReceipt(other.Public, Receipt{Sig: r.Sig}); err == nil {
		t.Fatal("别的公钥不应通过")
	}
}
