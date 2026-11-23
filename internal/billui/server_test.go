package billui

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"billgo/internal/access"
	"billgo/internal/config"
	"billgo/internal/control"
	"billgo/internal/identity"
	"billgo/internal/node"
	"billgo/internal/ticket"
	"billgo/internal/transport"
)

func TestLoginHomeAndDeniedPeer(t *testing.T) {
	appleDir := shortDir(t)
	bananaDir := shortDir(t)
	appleID, err := identity.LoadOrCreate(filepath.Join(appleDir, "identity.key"))
	if err != nil {
		t.Fatal(err)
	}
	bananaID, err := identity.LoadOrCreate(filepath.Join(bananaDir, "identity.key"))
	if err != nil {
		t.Fatal(err)
	}

	bananaCfg := testCfg("banana", bananaDir)
	bananaCfg.Desc = "secret-desc"
	bananaCfg.Access = &access.Policy{
		Rules: []access.Rule{{Peer: appleID.PeerID, Deny: []string{access.ConfigRead, access.TicketsGet}}},
	}
	banana := startNode(t, bananaCfg)

	appleCfg := testCfg("apple", appleDir)
	appleCfg.Bootstrap = []string{banana.Endpoint() + "#" + bananaID.PeerID}
	apple := startNode(t, appleCfg)
	waitUntil(t, 8*time.Second, "apple 看见 banana", func() bool {
		_, ok := apple.Peers().Lookup(bananaID.PeerID)
		return ok
	})
	tid, err := apple.Tickets().Create(ticket.CreateReq{
		Form: []byte(`{"task":"看一眼"}`),
		Flow: []ticket.Hop{{Step: 1, To: bananaID.PeerID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	btid, err := banana.Tickets().Create(ticket.CreateReq{
		Form: []byte(`{"task":"secret-form"}`),
		Flow: []ticket.Hop{{Step: 1, To: appleID.PeerID, Desc: "secret-hop"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	ui, err := Open(appleDir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(ui.PasswordPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("密码文件权限 %o", info.Mode().Perm())
	}
	ts := httptest.NewServer(ui.Handler())
	t.Cleanup(ts.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	res, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/login") {
		t.Fatalf("未登录应去登录页，得到 %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	res.Body.Close()

	res, err = client.PostForm(ts.URL+"/login", url.Values{"password": {"nope"}})
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, res)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "密码不对") || res.Header.Get("Set-Cookie") != "" {
		t.Fatalf("错误密码应停在登录页，得到 %d %q", res.StatusCode, body)
	}

	res, err = client.PostForm(ts.URL+"/login", url.Values{"password": {ui.Password()}})
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("登录应跳转，得到 %d", res.StatusCode)
	}
	res.Body.Close()
	var session *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == cookieName {
			session = c
		}
	}
	if session == nil {
		t.Fatal("登录没有发会话 cookie")
	}

	home := getAuth(t, client, ts.URL+"/", session)
	if !strings.Contains(home, "apple") || !strings.Contains(home, tid) || !strings.Contains(home, `action="/tickets"`) || !strings.Contains(home, "远程读默认开放") {
		t.Fatalf("主页应有本节点、工单、发起表单和读开放提示:\n%s", home)
	}
	auditPage := getAuth(t, client, ts.URL+"/audit", session)
	if !strings.Contains(auditPage, "没有记录") {
		t.Fatalf("本机还没有远程访问记录:\n%s", auditPage)
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/tickets", strings.NewReader(url.Values{
		"form": {`{"task":"页面发起"}`},
		"flow": {bananaID.PeerID},
	}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(session)
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	loc := res.Header.Get("Location")
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/tickets/") {
		t.Fatalf("发起应跳到工单页，得到 %d %s", res.StatusCode, loc)
	}
	page := getAuth(t, client, ts.URL+loc, session)
	if !strings.Contains(page, "页面发起") || !strings.Contains(page, "作废") {
		t.Fatalf("工单页应有表单和作废:\n%s", page)
	}
	req, err = http.NewRequest(http.MethodPost, ts.URL+loc+"/void", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(session)
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("作废应跳回工单页，得到 %d", res.StatusCode)
	}
	voided := getAuth(t, client, ts.URL+loc, session)
	if !strings.Contains(voided, "voided") || strings.Contains(voided, ">作废<") {
		t.Fatalf("作废后不应再提供作废按钮:\n%s", voided)
	}

	denied := getAuth(t, client, ts.URL+"/peers/"+bananaID.PeerID, session)
	if !strings.Contains(denied, "banana") || !strings.Contains(denied, btid) || !strings.Contains(denied, "配置不可见") || strings.Contains(denied, "secret-desc") || strings.Contains(denied, "secret-form") || strings.Contains(denied, "secret-hop") {
		t.Fatalf("邻居页应留下身份和工单列表，藏起配置和内容:\n%s", denied)
	}
	hidden := getAuth(t, client, ts.URL+"/peers/"+bananaID.PeerID+"/tickets/"+btid, session)
	if !strings.Contains(hidden, "内容不可见") || !strings.Contains(hidden, "再试一次") || strings.Contains(hidden, "secret-form") {
		t.Fatalf("工单内容被拒时应说明不可见:\n%s", hidden)
	}

	restarted, err := Open(appleDir)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Password() == ui.Password() {
		t.Fatal("重启应更换密码")
	}
	ts2 := httptest.NewServer(restarted.Handler())
	t.Cleanup(ts2.Close)
	req, err = http.NewRequest(http.MethodGet, ts2.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(session)
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("旧会话在新进程应失效，得到 %d", res.StatusCode)
	}
}

func startNode(t *testing.T, cfg *config.Config) *node.Node {
	t.Helper()
	n, err := node.New(cfg)
	if err != nil {
		t.Fatalf("装配 %s: %v", cfg.Name, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- n.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-errCh
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h, err := transport.NewClient("").Health(context.Background(), n.Endpoint(), n.PeerID())
		if err == nil && h.Ready {
			if _, err := os.Stat(control.SocketPath(cfg.DataDir)); err == nil {
				return n
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s 未就绪", cfg.Name)
	return nil
}

func testCfg(name, dir string) *config.Config {
	c := config.Default()
	c.Name = name
	c.DataDir = dir
	c.Listen = "127.0.0.1:0"
	c.Network = "uitest"
	c.AntiEntropySec = 1
	return c
}

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "bu")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func waitUntil(t *testing.T, d time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("等待超时: " + what)
}

func getAuth(t *testing.T, client *http.Client, rawURL string, cookie *http.Cookie) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%s 返回 %d\n%s", rawURL, res.StatusCode, body)
	}
	return body
}

func readAll(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
