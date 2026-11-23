// billgo 是与本机节点对话的前端：发起工单、看名录、查事件流。
//
// 它不直接对外通信，所有动作都经过数据目录下的 Unix socket。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"billgo/internal/audit"
	"billgo/internal/control"
	"billgo/internal/nodestatus"
	"billgo/internal/outbox"
	"billgo/internal/peers"
	"billgo/internal/ticket"
)

// version 由发布脚本通过 -ldflags "-X main.version=..." 注入。
var version = "dev"

type nodeInfo struct {
	PeerID    string   `json:"peer_id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Network   string   `json:"network"`
	Endpoint  string   `json:"endpoint"`
	Version   string   `json:"version"`
	Listen    string   `json:"listen"`
	DataDir   string   `json:"data_dir"`
	Bootstrap []string `json:"bootstrap"`
	AllowFrom []string `json:"allow_from"`
	WakeMode  string   `json:"wake_mode"`
}

func main() {
	args, dataDir, peer := extractGlobals(os.Args[1:])
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	c := control.NewClient(dataDir)
	ctx := context.Background()

	var err error
	switch args[0] {
	case "version", "-version", "--version":
		err = remoteOnly(peer, "version")
		if err == nil {
			fmt.Printf("billgo %s\n", version)
		}
	case "node":
		err = cmdNode(ctx, c, peer)
	case "status":
		err = remoteOnly(peer, "status")
		if err == nil {
			err = cmdStatus(ctx, c)
		}
	case "config":
		err = cmdConfig(ctx, c, peer, args[1:])
	case "peers":
		err = cmdPeers(ctx, c, peer)
	case "ticket":
		err = cmdTicket(ctx, c, peer, args[1:])
	case "outbox":
		err = remoteOnly(peer, "outbox")
		if err == nil {
			err = cmdOutbox(ctx, c)
		}
	case "audit":
		err = remoteOnly(peer, "audit")
		if err == nil {
			err = cmdAudit(ctx, c, args[1:])
		}
	case "withdraw":
		err = remoteOnly(peer, "withdraw")
		if err == nil {
			err = c.Do(ctx, "POST", "/withdraw", nil, nil)
		}
		if err == nil {
			fmt.Println("已注销（墓碑会同步给其他节点）")
		}
	case "wake":
		err = remoteOnly(peer, "wake")
		if err != nil {
			break
		}
		if len(args) != 2 {
			err = fmt.Errorf("用法: billgo wake <节点名或 peer_id>")
			break
		}
		err = c.Do(ctx, "POST", "/wake", map[string]string{"peer": args[1]}, nil)
		if err == nil {
			fmt.Println("已唤醒")
		}
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("未知命令 %q", args[0])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

// extractGlobals 支持 --data 和 --peer 写在任意位置。
// 数据目录否则读 BILLGO_DATA，默认 data。
func extractGlobals(args []string) ([]string, string, string) {
	dir := os.Getenv("BILLGO_DATA")
	if dir == "" {
		dir = "data"
	}
	var peer string
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--data" || a == "-data":
			if i+1 < len(args) {
				dir = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--data="):
			dir = strings.TrimPrefix(a, "--data=")
		case strings.HasPrefix(a, "-data="):
			dir = strings.TrimPrefix(a, "-data=")
		case a == "--peer" || a == "-peer":
			if i+1 < len(args) {
				peer = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--peer="):
			peer = strings.TrimPrefix(a, "--peer=")
		case strings.HasPrefix(a, "-peer="):
			peer = strings.TrimPrefix(a, "-peer=")
		default:
			out = append(out, a)
		}
	}
	return out, dir, peer
}

func remoteOnly(peer, cmd string) error {
	if peer == "" {
		return nil
	}
	return fmt.Errorf("%s 不能对远程节点执行", cmd)
}

func usage() {
	fmt.Print(`billgo — 去中心化 Agent 工单工具

用法: billgo [--data 数据目录] [--peer 节点] <命令>

命令:
  node                                节点信息
  status                              状态、待处理与配置摘要
  config                              读当前生效配置
  config write --file <cfg.json>      写入配置文件（access 立即生效）
  peers                               查看名录
  ticket create --form <f|-> --flow a,b   发起工单
  ticket list                         工单列表
  ticket show <id>                    工单详情与事件流
  ticket redispatch <id> --hop N --to <节点>
  ticket void <id>                    作废工单
  audit [--limit N] [--cursor C]     本机审计（谁访问过这里）
  outbox                              查看待发队列
  wake <节点>                         手动叫醒一个节点
  withdraw                            注销本节点（写墓碑）
  version                             版本

--peer 可以读 node、config、peers、ticket，也可以发起、改派、作废和写配置。
工单列表可加 --limit 和 --cursor。拒绝、离线、超时是不同的错误。
写配置：access 立即生效，其余字段写入文件，重启后生效。不能改 data_dir。
audit 只看本机。远程读默认开放时，节点启动会打一条警告。
`)
}

func cmdAudit(ctx context.Context, c *control.Client, args []string) error {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	limit := fs.Int("limit", 50, "每页条数")
	cursor := fs.String("cursor", "", "上一页返回的游标")
	if err := fs.Parse(args); err != nil {
		return err
	}
	q := url.Values{}
	q.Set("limit", fmt.Sprintf("%d", *limit))
	if *cursor != "" {
		q.Set("cursor", *cursor)
	}
	var page struct {
		Items      []audit.Record `json:"items"`
		NextCursor string         `json:"next_cursor"`
	}
	if err := c.Do(ctx, "GET", "/audit?"+q.Encode(), nil, &page); err != nil {
		return err
	}
	if len(page.Items) == 0 {
		fmt.Println("（空）")
		return nil
	}
	for _, rec := range page.Items {
		when := time.UnixMilli(rec.TS).Format("2006-01-02 15:04:05")
		fmt.Printf("%s  %-6s  %-20s  %s  %s\n", when, rec.Result, rec.Action, short(rec.From), rec.EnvelopeID)
	}
	if page.NextCursor != "" {
		fmt.Printf("下一页  --cursor %s\n", page.NextCursor)
	}
	return nil
}

func cmdNode(ctx context.Context, c *control.Client, peer string) error {
	var out nodeInfo
	var err error
	if peer == "" {
		err = c.Do(ctx, "GET", "/node", nil, &out)
	} else {
		err = c.Call(ctx, peer, "node.info", nil, &out)
	}
	if err != nil {
		return err
	}
	fmt.Printf("peer_id   %s\nname      %s\nkind      %s\nendpoint  %s\nnetwork   %s\nversion   %s\n",
		out.PeerID, out.Name, out.Kind, out.Endpoint, out.Network, out.Version)
	return nil
}

func cmdStatus(ctx context.Context, c *control.Client) error {
	var node nodeInfo
	if err := c.Do(ctx, "GET", "/node", nil, &node); err != nil {
		return err
	}
	var st nodestatus.Status
	if err := c.Do(ctx, "GET", "/status", nil, &st); err != nil {
		return err
	}

	wake := node.WakeMode
	if wake == "" {
		wake = "-"
	}
	fmt.Printf("节点   %s#%s · %s · network=%s · %s\n",
		node.Name, short(node.PeerID), node.Kind, node.Network, node.Version)
	fmt.Printf("配置   endpoint=%s · bootstrap=%d · allow_from=%d · wake=%s\n",
		node.Endpoint, len(node.Bootstrap), len(node.AllowFrom), wake)
	fmt.Printf("工单   运行 %d · 完成 %d · 作废 %d\n",
		st.Summary.Tickets[ticket.StatusRunning],
		st.Summary.Tickets[ticket.StatusClosed],
		st.Summary.Tickets[ticket.StatusVoided])
	fmt.Printf("队列   outbox %d 条（卡住 %d）\n",
		st.Summary.Outbox["pending"], st.Summary.Outbox["stuck"])
	fmt.Printf("待处理 %d\n", len(st.Attention))
	for _, a := range st.Attention {
		line := "  " + a.Kind
		if a.Ticket != "" {
			line += "  " + shortTicket(a.Ticket)
		}
		if a.Hop > 0 {
			line += fmt.Sprintf(" 第%d跳", a.Hop)
		}
		if a.Peer != "" {
			line += " → " + short(a.Peer)
		}
		if a.Reason != "" {
			line += " · " + a.Reason
		}
		fmt.Println(line)
		if a.Hint != "" {
			fmt.Println("      " + a.Hint)
		}
	}
	return nil
}

func cmdConfig(ctx context.Context, c *control.Client, peer string, args []string) error {
	if len(args) > 0 {
		return configWrite(ctx, c, peer, args)
	}
	if peer != "" {
		var view map[string]any
		if err := c.Call(ctx, peer, "config.read", nil, &view); err != nil {
			return err
		}
		raw, err := json.MarshalIndent(view, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(raw))
		return nil
	}
	var n nodeInfo
	if err := c.Do(ctx, "GET", "/node", nil, &n); err != nil {
		return err
	}
	wake := n.WakeMode
	if wake == "" {
		wake = "-"
	}
	rows := [][2]string{
		{"peer_id", n.PeerID},
		{"name", n.Name},
		{"kind", n.Kind},
		{"network", n.Network},
		{"endpoint", n.Endpoint},
		{"listen", n.Listen},
		{"data_dir", n.DataDir},
		{"version", n.Version},
		{"bootstrap", joinOr(n.Bootstrap, "（空）")},
		{"allow_from", joinOr(n.AllowFrom, "（空）")},
		{"wake_mode", wake},
	}
	for _, r := range rows {
		fmt.Printf("%-12s %s\n", r[0], r[1])
	}
	return nil
}

func configWrite(ctx context.Context, c *control.Client, peer string, args []string) error {
	if args[0] != "write" {
		return fmt.Errorf("用法: billgo config write --file <cfg.json>")
	}
	fs := flag.NewFlagSet("write", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("file", "", "配置文件")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *file == "" {
		return fmt.Errorf("--file 必填")
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	var out struct {
		OK bool `json:"ok"`
	}
	if err := c.Call(ctx, peer, "config.write", json.RawMessage(raw), &out); err != nil {
		return err
	}
	fmt.Println("已写入。access 立即生效，其余配置重启后生效。")
	return nil
}

func shortTicket(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

func joinOr(v []string, empty string) string {
	if len(v) == 0 {
		return empty
	}
	return strings.Join(v, ", ")
}

func cmdPeers(ctx context.Context, c *control.Client, peer string) error {
	var list []peers.Record
	var err error
	if peer == "" {
		err = c.Do(ctx, "GET", "/peers", nil, &list)
	} else {
		err = c.Call(ctx, peer, "peers.list", nil, &list)
	}
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("（空）")
		return nil
	}
	fmt.Printf("%-22s %-8s %-9s %-30s %s\n", "节点", "类型", "版本", "地址", "描述")
	for _, p := range list {
		name := p.Name
		if name == "" {
			name = "-"
		}
		label := fmt.Sprintf("%s#%s", name, short(p.PeerID))
		state := ""
		if p.Deleted {
			state = "  [已注销]"
		}
		fmt.Printf("%-22s %-8s %-9d %-30s %s%s\n", label, p.Kind, p.Version, p.Endpoint, p.Desc, state)
	}
	return nil
}

func cmdTicket(ctx context.Context, c *control.Client, peer string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: billgo ticket <create|list|show|redispatch|void>")
	}
	switch args[0] {
	case "create":
		return ticketCreate(ctx, c, peer, args[1:])
	case "list":
		return ticketList(ctx, c, peer, args[1:])
	case "show":
		if len(args) != 2 {
			return fmt.Errorf("用法: billgo ticket show <id>")
		}
		return ticketShow(ctx, c, peer, args[1])
	case "redispatch":
		if len(args) < 2 {
			return fmt.Errorf("用法: billgo ticket redispatch <id> --hop N --to <节点>")
		}
		fs := flag.NewFlagSet("redispatch", flag.ContinueOnError)
		hop := fs.Int("hop", 0, "第几跳")
		to := fs.String("to", "", "目标节点")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if *hop == 0 || *to == "" {
			return fmt.Errorf("--hop 和 --to 必填")
		}
		body := map[string]any{"hop": *hop, "to": *to}
		if peer == "" {
			return c.Do(ctx, "POST", "/tickets/"+args[1]+"/redispatch", body, nil)
		}
		body["id"] = args[1]
		if err := c.Call(ctx, peer, "tickets.redispatch", body, nil); err != nil {
			return err
		}
		fmt.Println("已改派")
		return nil
	case "void":
		if len(args) != 2 {
			return fmt.Errorf("用法: billgo ticket void <id>")
		}
		var err error
		if peer == "" {
			err = c.Do(ctx, "POST", "/tickets/"+args[1]+"/void", nil, nil)
		} else {
			err = c.Call(ctx, peer, "tickets.void", map[string]string{"id": args[1]}, nil)
		}
		if err != nil {
			return err
		}
		fmt.Println("已作废")
		return nil
	default:
		return fmt.Errorf("未知 ticket 子命令 %q", args[0])
	}
}

func ticketCreate(ctx context.Context, c *control.Client, peer string, args []string) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	formPath := fs.String("form", "", "表单文件，- 表示 stdin")
	flow := fs.String("flow", "", "逗号分隔的节点名，按顺序流转")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *formPath == "" || *flow == "" {
		return fmt.Errorf("--form 和 --flow 必填")
	}
	form, err := readForm(*formPath)
	if err != nil {
		return err
	}
	var hops []ticket.Hop
	for _, name := range strings.Split(*flow, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		step := len(hops) + 1
		hop := ticket.Hop{Step: step, To: name, Desc: name}
		if step > 1 {
			hop.DependsOn = []int{step - 1}
		}
		hops = append(hops, hop)
	}
	req := ticket.CreateReq{Form: form, Flow: hops}
	var out struct {
		ID string `json:"id"`
	}
	if peer == "" {
		err = c.Do(ctx, "POST", "/tickets", req, &out)
	} else {
		err = c.Call(ctx, peer, "tickets.create", req, &out)
	}
	if err != nil {
		return err
	}
	fmt.Println(out.ID)
	return nil
}

func readForm(path string) (json.RawMessage, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("表单不是合法 JSON: %w", err)
	}
	return raw, nil
}

func ticketList(ctx context.Context, c *control.Client, peer string, args []string) error {
	if peer == "" {
		if len(args) != 0 {
			return fmt.Errorf("用法: billgo ticket list")
		}
		return ticketListLocal(ctx, c)
	}
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	limit := fs.Int("limit", 20, "每页条数")
	cursor := fs.String("cursor", "", "上一页返回的游标")
	if err := fs.Parse(args); err != nil {
		return err
	}
	argsMap := map[string]any{"limit": *limit}
	if *cursor != "" {
		argsMap["cursor"] = *cursor
	}
	var page struct {
		Items      []ticket.State `json:"items"`
		NextCursor string         `json:"next_cursor"`
	}
	if err := c.Call(ctx, peer, "tickets.list", argsMap, &page); err != nil {
		return err
	}
	if err := printTickets(page.Items); err != nil {
		return err
	}
	if page.NextCursor != "" {
		fmt.Printf("下一页  --cursor %s\n", page.NextCursor)
	}
	return nil
}

func ticketListLocal(ctx context.Context, c *control.Client) error {
	var list []ticket.State
	if err := c.Do(ctx, "GET", "/tickets", nil, &list); err != nil {
		return err
	}
	return printTickets(list)
}

func printTickets(list []ticket.State) error {
	if len(list) == 0 {
		fmt.Println("（空）")
		return nil
	}
	fmt.Printf("%-38s %-9s %s\n", "工单", "状态", "当前跳")
	for _, st := range list {
		cur := "-"
		if h := st.Current(); h != nil {
			cur = fmt.Sprintf("第%d跳 %s (%s)", h.Step, short(h.Target), h.State)
		}
		fmt.Printf("%-38s %-9s %s\n", st.ID, st.Status, cur)
	}
	return nil
}

func ticketShow(ctx context.Context, c *control.Client, peer, id string) error {
	var out struct {
		State  ticket.State   `json:"state"`
		Events []ticket.Event `json:"events"`
	}
	var err error
	if peer == "" {
		err = c.Do(ctx, "GET", "/tickets/"+id, nil, &out)
	} else {
		err = c.Call(ctx, peer, "tickets.get", map[string]string{"id": id}, &out)
	}
	if err != nil {
		return err
	}
	st := out.State
	fmt.Printf("工单 %s  [%s]\n", st.ID, st.Status)
	fmt.Printf("表单 %s\n", st.Form)
	for _, h := range st.Hops {
		fmt.Printf("  第%d跳 → %s (%s)  %s\n", h.Step, short(h.Target), h.Desc, h.State)
	}
	fmt.Println("事件:")
	for _, ev := range out.Events {
		extra := ""
		if len(ev.Data) > 0 && (ev.Type == ticket.EvRejected || ev.Type == ticket.EvDelivered) {
			extra = " " + strings.TrimSpace(string(ev.Data))
		}
		fmt.Printf("  %d  %s  +%dms  hop=%d peer=%s%s\n",
			ev.Seq, ev.Type, ev.TS-st.CreatedAt, ev.Hop, short(ev.Peer), extra)
	}
	return nil
}

func cmdOutbox(ctx context.Context, c *control.Client) error {
	var items []outbox.Item
	if err := c.Do(ctx, "GET", "/outbox", nil, &items); err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Println("（空）")
		return nil
	}
	for _, it := range items {
		fmt.Printf("#%d → %s  尝试 %d 次  %s\n", it.ID, short(it.PeerID), it.Attempts, it.LastErr)
	}
	return nil
}

func short(id string) string {
	if len(id) > 4 {
		return id[:4]
	}
	return id
}
