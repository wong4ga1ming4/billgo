package node_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"billgo/internal/access"
	"billgo/internal/config"
	"billgo/internal/control"
	"billgo/internal/envelope"
	"billgo/internal/identity"
	"billgo/internal/node"
	"billgo/internal/nodestatus"
	"billgo/internal/ticket"
	"billgo/internal/transport"
)

type running struct {
	t      *testing.T
	n      *node.Node
	cfg    *config.Config
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func (r *running) stop() {
	r.cancel()
	<-r.done
	if r.err != nil && r.t != nil {
		r.t.Errorf("%s 的 Run 返回错误: %v", r.cfg.Name, r.err)
	}
}

// shortDir 用短路径做数据目录：Unix socket 路径上限约 104 字节。
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "af")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func baseCfg(t *testing.T, name string) *config.Config {
	t.Helper()
	c := config.Default()
	c.Name = name
	c.DataDir = shortDir(t)
	c.Listen = "127.0.0.1:0"
	c.Network = "test"
	c.AntiEntropySec = 1
	c.AcceptTimeoutMin = 5
	c.DeliverTimeoutMin = 5
	c.JobTimeoutMin = 2
	return c
}

func bootstrap(n *node.Node) string {
	return n.Endpoint() + "#" + n.PeerID()
}

func start(t *testing.T, cfg *config.Config) *running {
	t.Helper()
	return startWith(t, cfg, node.Options{})
}

func startWith(t *testing.T, cfg *config.Config, opt node.Options) *running {
	t.Helper()
	n, err := node.NewWithOptions(cfg, opt)
	if err != nil {
		t.Fatalf("装配 %s: %v", cfg.Name, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{t: t, n: n, cfg: cfg, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		r.err = n.Run(ctx)
	}()
	t.Cleanup(r.stop)
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		h, err := transport.NewClient("").Health(context.Background(), n.Endpoint(), n.PeerID())
		if err == nil && h.Ready {
			return r
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s 未就绪: %v", cfg.Name, lastErr)
	return r
}

func waitFor(t *testing.T, d time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

func knows(r, other *running) bool {
	_, ok := r.n.Peers().Lookup(other.n.PeerID())
	return ok
}

func acceptAgent() []string {
	return []string{"/bin/sh", "-c", `printf 'done by %s' "$BILLGO_TICKET" > "$BILLGO_OUT_DIR/result.txt"`}
}

func rejectAgent() []string {
	return []string{"/bin/sh", "-c", `printf '{"reason":"not my job"}' > "$BILLGO_JOB_DIR/result.json"; exit 1`}
}

func form(t *testing.T) json.RawMessage {
	t.Helper()
	return json.RawMessage(`{"task":"写文档","lang":"zh"}`)
}

func countBucket(n *node.Node, bucket []byte, prefix string) int {
	c := 0
	n.Store().ForEach(bucket, func(k, _ []byte) error {
		if strings.HasPrefix(string(k), prefix) {
			c++
		}
		return nil
	})
	return c
}

func TestTwoHopTicketFlow(t *testing.T) {
	apple := start(t, baseCfg(t, "apple"))

	bananaCfg := baseCfg(t, "banana")
	bananaCfg.AllowFrom = []string{apple.n.PeerID()}
	bananaCfg.AgentCmd = acceptAgent()
	bananaCfg.Bootstrap = []string{bootstrap(apple.n)}
	banana := start(t, bananaCfg)

	peachCfg := baseCfg(t, "peach")
	peachCfg.AllowFrom = []string{apple.n.PeerID()}
	peachCfg.AgentCmd = acceptAgent()
	peachCfg.Bootstrap = []string{bootstrap(banana.n)}
	peach := start(t, peachCfg)

	waitFor(t, 10*time.Second, "apple 发现 banana", func() bool { return knows(apple, banana) })
	waitFor(t, 10*time.Second, "apple 发现 peach", func() bool { return knows(apple, peach) })
	waitFor(t, 10*time.Second, "banana 发现 peach", func() bool { return knows(banana, peach) })

	tid, err := apple.n.Tickets().Create(ticket.CreateReq{
		Form: form(t),
		Flow: []ticket.Hop{
			{Step: 1, To: banana.n.PeerID(), Desc: "第一步"},
			{Step: 2, To: peach.n.PeerID(), DependsOn: []int{1}, Desc: "第二步"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, 30*time.Second, "工单完成", func() bool {
		st, _, err := apple.n.Tickets().Get(tid)
		return err == nil && st.Status == ticket.StatusClosed
	})

	st, evs, err := apple.n.Tickets().Get(tid)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range st.Hops {
		if h.State != ticket.HopVerified {
			t.Fatalf("第 %d 跳状态 %s", h.Step, h.State)
		}
	}
	if got := countBucket(apple.n, ticket.ReceiptsBucket, tid+"|"); got != 2 {
		t.Fatalf("回执应该有 2 张，实际 %d（事件 %d 条）", got, len(evs))
	}
	entries, _ := filepath.Glob(filepath.Join(apple.n.Store().BlobDir(), "*"))
	if len(entries) == 0 {
		t.Fatal("apple 本地应该存有交付物")
	}

	// 幂等：重发同一跳的 hop.issue，应拿到缓存的 accept，且不会重复开作业。
	idA, err := identity.LoadOrCreate(filepath.Join(apple.cfg.DataDir, "identity.key"))
	if err != nil {
		t.Fatal(err)
	}
	env, err := envelope.New(envelope.TypeHopIssue, banana.n.PeerID(), tid, 1, time.Now().UnixMilli(),
		ticket.IssueBody{Form: form(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Sign(idA.Private); err != nil {
		t.Fatal(err)
	}
	resp, err := transport.NewClient(idA.PeerID).Send(context.Background(), banana.n.Endpoint(), banana.n.PeerID(), env)
	if err != nil {
		t.Fatalf("重发派单失败: %v", err)
	}
	if resp == nil || resp.Type != envelope.TypeHopAccept {
		t.Fatalf("重发应返回缓存的 accept，得到 %+v", resp)
	}
	if got := countBucket(banana.n, ticket.JobsBucket, fmt.Sprintf("%s|1", tid)); got != 1 {
		t.Fatalf("重复派单不应重复开作业，作业数 %d", got)
	}
}

func TestRejectThenRedispatch(t *testing.T) {
	apple := start(t, baseCfg(t, "apple"))

	chiliCfg := baseCfg(t, "chili")
	chiliCfg.AllowFrom = []string{apple.n.PeerID()}
	chiliCfg.AgentCmd = rejectAgent()
	chiliCfg.Bootstrap = []string{bootstrap(apple.n)}
	chili := start(t, chiliCfg)

	bananaCfg := baseCfg(t, "banana")
	bananaCfg.AllowFrom = []string{apple.n.PeerID()}
	bananaCfg.AgentCmd = acceptAgent()
	bananaCfg.Bootstrap = []string{bootstrap(apple.n)}
	banana := start(t, bananaCfg)

	waitFor(t, 10*time.Second, "apple 发现 chili", func() bool { return knows(apple, chili) })
	waitFor(t, 10*time.Second, "apple 发现 banana", func() bool { return knows(apple, banana) })

	tid, err := apple.n.Tickets().Create(ticket.CreateReq{
		Form: form(t),
		Flow: []ticket.Hop{{Step: 1, To: chili.n.PeerID(), Desc: "先给 chili"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "chili 拒绝", func() bool {
		st, _, err := apple.n.Tickets().Get(tid)
		return err == nil && st.Hops[0].State == ticket.HopRejected
	})

	if err := apple.n.Tickets().Redispatch(tid, 1, banana.n.PeerID()); err != nil {
		t.Fatalf("改派失败: %v", err)
	}
	waitFor(t, 30*time.Second, "改派后完成", func() bool {
		st, _, err := apple.n.Tickets().Get(tid)
		return err == nil && st.Status == ticket.StatusClosed
	})
}

func TestUnauthorizedIssueRejected(t *testing.T) {
	apple := start(t, baseCfg(t, "apple"))
	bananaCfg := baseCfg(t, "banana")
	bananaCfg.AllowFrom = []string{apple.n.PeerID()}
	bananaCfg.Bootstrap = []string{bootstrap(apple.n)}
	banana := start(t, bananaCfg)
	waitFor(t, 10*time.Second, "apple 发现 banana", func() bool { return knows(apple, banana) })

	intruder, err := identity.LoadOrCreate(filepath.Join(t.TempDir(), "identity.key"))
	if err != nil {
		t.Fatal(err)
	}
	env, err := envelope.New(envelope.TypeHopIssue, banana.n.PeerID(), "t-intruder", 1,
		time.Now().UnixMilli(), ticket.IssueBody{Form: form(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Sign(intruder.Private); err != nil {
		t.Fatal(err)
	}
	resp, err := transport.NewClient(intruder.PeerID).Send(context.Background(), banana.n.Endpoint(), banana.n.PeerID(), env)
	if err != nil {
		t.Fatalf("应返回拒绝信封而不是 HTTP 错误: %v", err)
	}
	if resp == nil || resp.Type != envelope.TypeHopReject {
		t.Fatalf("白名单外的派单应被拒绝，得到 %+v", resp)
	}
	if !strings.Contains(string(resp.Body), "白名单") {
		t.Fatalf("拒绝原因应说明白名单，得到 %s", resp.Body)
	}
}

func TestWithdrawTombstone(t *testing.T) {
	apple := start(t, baseCfg(t, "apple"))
	bananaCfg := baseCfg(t, "banana")
	bananaCfg.AllowFrom = []string{apple.n.PeerID()}
	bananaCfg.Bootstrap = []string{bootstrap(apple.n)}
	banana := start(t, bananaCfg)
	waitFor(t, 10*time.Second, "apple 发现 banana", func() bool { return knows(apple, banana) })
	waitFor(t, 10*time.Second, "banana 发现 apple", func() bool { return knows(banana, apple) })

	apple.stop()
	waitFor(t, 10*time.Second, "banana 收到 tombstone", func() bool {
		for _, r := range banana.n.Peers().ListAll() {
			if r.PeerID == apple.n.PeerID() && r.Deleted {
				return true
			}
		}
		return false
	})

	// 新节点从 banana 引导，应继承墓碑而不是复活 apple。
	dateCfg := baseCfg(t, "date")
	dateCfg.Bootstrap = []string{bootstrap(banana.n)}
	date := start(t, dateCfg)
	waitFor(t, 10*time.Second, "date 继承墓碑", func() bool {
		for _, r := range date.n.Peers().ListAll() {
			if r.PeerID == apple.n.PeerID() && r.Deleted {
				return true
			}
		}
		return false
	})
}

func TestWakeableNotTombstonedOnStop(t *testing.T) {
	apple := start(t, baseCfg(t, "apple"))

	bananaCfg := baseCfg(t, "banana")
	bananaCfg.Kind = config.KindWakeable
	bananaCfg.Wake = &config.Wake{Mode: "local", Container: "banana"}
	bananaCfg.AllowFrom = []string{apple.n.PeerID()}
	bananaCfg.Bootstrap = []string{bootstrap(apple.n)}
	banana := start(t, bananaCfg)
	waitFor(t, 10*time.Second, "apple 发现 banana", func() bool { return knows(apple, banana) })

	// 唤醒类"睡着"是正常状态，优雅退出不该写墓碑，否则没人能再叫醒它。
	banana.stop()
	time.Sleep(300 * time.Millisecond)

	for _, r := range apple.n.Peers().ListAll() {
		if r.PeerID == banana.n.PeerID() {
			if r.Deleted {
				t.Fatal("唤醒类退出后不应产生墓碑")
			}
			return
		}
	}
	t.Fatal("apple 名录里应该还留着 banana 的记录")
}

func TestControlStatusAndConfig(t *testing.T) {
	cfg := baseCfg(t, "apple")
	r := start(t, cfg)
	ctx := context.Background()

	if _, err := r.n.Tickets().Create(ticket.CreateReq{
		Form: json.RawMessage(`{"x":1}`),
		Flow: []ticket.Hop{{Step: 1, To: r.n.PeerID()}},
	}); err != nil {
		t.Fatal(err)
	}

	c := control.NewClient(cfg.DataDir)
	var st nodestatus.Status
	if err := c.Do(ctx, "GET", "/status", nil, &st); err != nil {
		t.Fatal(err)
	}
	if st.Summary.Tickets[ticket.StatusRunning] < 1 {
		t.Fatalf("状态里应有一张运行中的工单: %+v", st.Summary)
	}

	var info struct {
		Version   string   `json:"version"`
		DataDir   string   `json:"data_dir"`
		Bootstrap []string `json:"bootstrap"`
		AllowFrom []string `json:"allow_from"`
	}
	if err := c.Do(ctx, "GET", "/node", nil, &info); err != nil {
		t.Fatal(err)
	}
	if info.Version == "" || info.DataDir == "" {
		t.Fatalf("配置字段缺失: %+v", info)
	}
}

func TestResumeJobAfterRestart(t *testing.T) {
	apple := start(t, baseCfg(t, "apple"))

	slow := baseCfg(t, "slow")
	slow.AllowFrom = []string{apple.n.PeerID()}
	slow.AgentCmd = []string{"/bin/sh", "-c", `sleep 1; printf 'done' > "$BILLGO_OUT_DIR/result.txt"`}
	slow.Bootstrap = []string{bootstrap(apple.n)}
	slowNode := start(t, slow)
	waitFor(t, 10*time.Second, "apple 发现 slow", func() bool { return knows(apple, slowNode) })

	tid, err := apple.n.Tickets().Create(ticket.CreateReq{
		Form: form(t),
		Flow: []ticket.Hop{{Step: 1, To: slowNode.n.PeerID()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "slow 受理并开始作业", func() bool {
		return countBucket(slowNode.n, ticket.JobsBucket, tid+"|1") == 1
	})

	// 在作业跑完之前停掉节点，再用同一个数据目录拉起来。
	slowNode.stop()
	time.Sleep(200 * time.Millisecond)

	restartedCfg := baseCfg(t, "slow")
	restartedCfg.DataDir = slowNode.cfg.DataDir
	restartedCfg.AllowFrom = slow.AllowFrom
	restartedCfg.AgentCmd = slow.AgentCmd
	restartedCfg.Bootstrap = slow.Bootstrap
	restarted := start(t, restartedCfg)

	waitFor(t, 60*time.Second, "重启后工单完成", func() bool {
		st, _, err := apple.n.Tickets().Get(tid)
		return err == nil && st.Status == ticket.StatusClosed
	})
	_ = restarted
}

func TestRemoteReadDenyAndAudit(t *testing.T) {
	appleCfg := baseCfg(t, "apple")
	bananaCfg := baseCfg(t, "banana")
	bid, err := identity.LoadOrCreate(filepath.Join(bananaCfg.DataDir, "identity.key"))
	if err != nil {
		t.Fatal(err)
	}
	appleCfg.Access = &access.Policy{
		Default: access.Allow,
		Rules: []access.Rule{{
			Peer: bid.PeerID,
			Deny: []string{access.TicketsGet, access.ConfigWrite},
		}},
	}
	apple := start(t, appleCfg)
	bananaCfg.Bootstrap = []string{bootstrap(apple.n)}
	banana := start(t, bananaCfg)
	waitFor(t, 10*time.Second, "banana 发现 apple", func() bool { return knows(banana, apple) })

	tid, err := apple.n.Tickets().Create(ticket.CreateReq{
		Form: form(t),
		Flow: []ticket.Hop{{Step: 1, To: banana.n.PeerID(), Desc: "一步"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	remote := control.NewClient(banana.cfg.DataDir)
	var got struct {
		State ticket.State `json:"state"`
	}
	if err := remote.Call(ctx, apple.n.PeerID(), access.TicketsGet, map[string]string{"id": tid}, &got); err == nil || !strings.Contains(err.Error(), "拒绝访问") {
		t.Fatalf("工单内容应被拒绝，得到 %v", err)
	}

	var view map[string]any
	if err := remote.Call(ctx, "apple", access.ConfigRead, nil, &view); err != nil {
		t.Fatalf("远程读配置: %v", err)
	}
	if view["name"] != "apple" {
		t.Fatalf("配置名不对: %v", view["name"])
	}

	var listed struct {
		Items []ticket.State `json:"items"`
	}
	if err := remote.Call(ctx, apple.n.PeerID(), access.TicketsList, map[string]any{"limit": 10}, &listed); err != nil {
		t.Fatalf("工单列表应公开: %v", err)
	}
	if len(listed.Items) != 1 || listed.Items[0].ID != tid || len(listed.Items[0].Form) != 0 || listed.Items[0].Hops[0].Desc != "" {
		t.Fatalf("列表应有工单且不含表单和描述: %+v", listed.Items)
	}
	if err := remote.Call(ctx, apple.n.PeerID(), access.ConfigWrite, map[string]any{}, nil); err == nil || !strings.Contains(err.Error(), "拒绝访问") {
		t.Fatalf("写配置应默认拒绝，得到 %v", err)
	}
	if err := remote.Call(ctx, apple.n.PeerID(), access.TicketsCreate, map[string]any{}, nil); err == nil || !strings.Contains(err.Error(), "拒绝访问") {
		t.Fatalf("发起工单应默认拒绝，得到 %v", err)
	}
	if err := remote.Call(ctx, "0123456789abcdef", access.NodeInfo, nil, &view); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("未知节点应是 not_found，得到 %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(apple.cfg.DataDir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, `"result":"allow"`) || !strings.Contains(text, `"result":"deny"`) {
		t.Fatalf("审计应同时有允许和拒绝:\n%s", text)
	}
	if !strings.Contains(text, bid.PeerID) {
		t.Fatalf("审计应记下请求方:\n%s", text)
	}

	local := control.NewClient(apple.cfg.DataDir)
	var page struct {
		Items []ticket.State `json:"items"`
	}
	if err := local.Call(ctx, "", access.TicketsList, map[string]any{"limit": 1}, &page); err != nil {
		t.Fatalf("本机分页: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != tid {
		t.Fatalf("本机分页结果不对: %+v", page.Items)
	}

	var auditPage struct {
		Items []struct {
			Action string `json:"action"`
			Result string `json:"result"`
		} `json:"items"`
		Next string `json:"next_cursor"`
	}
	if err := local.Do(ctx, "GET", "/audit?limit=2", nil, &auditPage); err != nil {
		t.Fatalf("读审计: %v", err)
	}
	if len(auditPage.Items) != 2 || auditPage.Next == "" {
		t.Fatalf("审计应分页: %+v next=%q", auditPage.Items, auditPage.Next)
	}
	if auditPage.Items[0].Result != "deny" {
		t.Fatalf("最新一条应是拒绝: %+v", auditPage.Items[0])
	}
}

func TestRemoteWriteIdempotentAndAccessHot(t *testing.T) {
	appleCfg := baseCfg(t, "apple")
	bananaCfg := baseCfg(t, "banana")
	bid, err := identity.LoadOrCreate(filepath.Join(bananaCfg.DataDir, "identity.key"))
	if err != nil {
		t.Fatal(err)
	}
	appleCfg.Access = &access.Policy{
		Default: access.Allow,
		Rules: []access.Rule{{
			Peer: bid.PeerID,
			Allow: []string{
				access.TicketsCreate,
				access.TicketsVoid,
				access.ConfigWrite,
			},
		}},
	}
	cfgPath := filepath.Join(appleCfg.DataDir, "node.json")
	apple := startWith(t, appleCfg, node.Options{ConfigPath: cfgPath})
	bananaCfg.Bootstrap = []string{bootstrap(apple.n)}
	banana := start(t, bananaCfg)
	waitFor(t, 10*time.Second, "banana 发现 apple", func() bool { return knows(banana, apple) })

	ctx := context.Background()
	remote := control.NewClient(banana.cfg.DataDir)
	body := map[string]any{
		"peer":   apple.n.PeerID(),
		"action": access.TicketsCreate,
		"id":     "idem-1",
		"args": ticket.CreateReq{
			Form: form(t),
			Flow: []ticket.Hop{{Step: 1, To: banana.n.PeerID(), Desc: "一步"}},
		},
	}
	var first, second struct {
		ID string `json:"id"`
	}
	if err := remote.Do(ctx, "POST", "/ctl", body, &first); err != nil {
		t.Fatalf("第一次发起: %v", err)
	}
	if err := remote.Do(ctx, "POST", "/ctl", body, &second); err != nil {
		t.Fatalf("重复发起: %v", err)
	}
	if first.ID == "" || first.ID != second.ID {
		t.Fatalf("同一请求应得到同一工单，得到 %q 和 %q", first.ID, second.ID)
	}
	if n := len(apple.n.Tickets().List()); n != 1 {
		t.Fatalf("重复请求应只留下 1 张工单，得到 %d", n)
	}
	if err := remote.Call(ctx, apple.n.PeerID(), access.TicketsVoid, map[string]string{"id": first.ID}, nil); err != nil {
		t.Fatalf("作废: %v", err)
	}
	st, _, err := apple.n.Tickets().Get(first.ID)
	if err != nil || st.Status != ticket.StatusVoided {
		t.Fatalf("工单应已作废: %+v %v", st, err)
	}

	written := *apple.cfg
	written.Name = "renamed"
	written.Access = &access.Policy{Default: access.Deny}
	raw, err := json.Marshal(written)
	if err != nil {
		t.Fatal(err)
	}
	var ack struct {
		OK bool `json:"ok"`
	}
	if err := remote.Call(ctx, apple.n.PeerID(), access.ConfigWrite, json.RawMessage(raw), &ack); err != nil {
		t.Fatalf("写配置: %v", err)
	}
	if !ack.OK {
		t.Fatal("写配置没有确认")
	}
	if err := remote.Call(ctx, apple.n.PeerID(), access.TicketsGet, map[string]string{"id": first.ID}, nil); err == nil || !strings.Contains(err.Error(), "拒绝访问") {
		t.Fatalf("access 应立即关掉内容，得到 %v", err)
	}
	if err := remote.Call(ctx, apple.n.PeerID(), access.TicketsList, map[string]any{"limit": 10}, &struct{}{}); err != nil {
		t.Fatalf("工单列表应保持公开: %v", err)
	}
	if apple.n.Config().Name != "apple" {
		t.Fatalf("名字应留到重启，当前是 %q", apple.n.Config().Name)
	}
	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), `"name": "renamed"`) {
		t.Fatalf("文件里应是新名字:\n%s", saved)
	}
}
