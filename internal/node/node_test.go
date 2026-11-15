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

	"billgo/internal/config"
	"billgo/internal/envelope"
	"billgo/internal/identity"
	"billgo/internal/node"
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
	n, err := node.New(cfg)
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
