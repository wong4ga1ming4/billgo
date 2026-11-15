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
	"os"
	"strings"

	"billgo/internal/control"
	"billgo/internal/outbox"
	"billgo/internal/peers"
	"billgo/internal/ticket"
)

// version 由发布脚本通过 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	args, dataDir := extractDataDir(os.Args[1:])
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	c := control.NewClient(dataDir)
	ctx := context.Background()

	var err error
	switch args[0] {
	case "version", "-version", "--version":
		fmt.Printf("billgo %s\n", version)
	case "node":
		err = cmdNode(ctx, c)
	case "peers":
		err = cmdPeers(ctx, c)
	case "ticket":
		err = cmdTicket(ctx, c, args[1:])
	case "outbox":
		err = cmdOutbox(ctx, c)
	case "withdraw":
		err = c.Do(ctx, "POST", "/withdraw", nil, nil)
		if err == nil {
			fmt.Println("已注销（墓碑会同步给其他节点）")
		}
	case "wake":
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

// extractDataDir 支持 --data 写在任意位置；否则读 BILLGO_DATA，默认 data。
func extractDataDir(args []string) ([]string, string) {
	dir := os.Getenv("BILLGO_DATA")
	if dir == "" {
		dir = "data"
	}
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
		default:
			out = append(out, a)
		}
	}
	return out, dir
}

func usage() {
	fmt.Print(`billgo — 去中心化 Agent 工单工具

用法: billgo [--data 数据目录] <命令>

命令:
  node                                节点信息
  peers                               查看名录
  ticket create --form <f|-> --flow a,b   发起工单
  ticket list                         工单列表
  ticket show <id>                    工单详情与事件流
  ticket redispatch <id> --hop N --to <节点>
  ticket void <id>                    作废工单
  outbox                              查看待发队列
  wake <节点>                         手动叫醒一个节点
  withdraw                            注销本节点（写墓碑）
  version                             版本
`)
}

func cmdNode(ctx context.Context, c *control.Client) error {
	var out map[string]any
	if err := c.Do(ctx, "GET", "/node", nil, &out); err != nil {
		return err
	}
	fmt.Printf("peer_id   %v\nname      %v\nkind      %v\nendpoint  %v\nnetwork   %v\n",
		out["peer_id"], out["name"], out["kind"], out["endpoint"], out["network"])
	return nil
}

func cmdPeers(ctx context.Context, c *control.Client) error {
	var list []peers.Record
	if err := c.Do(ctx, "GET", "/peers", nil, &list); err != nil {
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

func cmdTicket(ctx context.Context, c *control.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: billgo ticket <create|list|show|redispatch|void>")
	}
	switch args[0] {
	case "create":
		return ticketCreate(ctx, c, args[1:])
	case "list":
		return ticketList(ctx, c)
	case "show":
		if len(args) != 2 {
			return fmt.Errorf("用法: billgo ticket show <id>")
		}
		return ticketShow(ctx, c, args[1])
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
		return c.Do(ctx, "POST", "/tickets/"+args[1]+"/redispatch", map[string]any{"hop": *hop, "to": *to}, nil)
	case "void":
		if len(args) != 2 {
			return fmt.Errorf("用法: billgo ticket void <id>")
		}
		if err := c.Do(ctx, "POST", "/tickets/"+args[1]+"/void", nil, nil); err != nil {
			return err
		}
		fmt.Println("已作废")
		return nil
	default:
		return fmt.Errorf("未知 ticket 子命令 %q", args[0])
	}
}

func ticketCreate(ctx context.Context, c *control.Client, args []string) error {
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
	if err := c.Do(ctx, "POST", "/tickets", req, &out); err != nil {
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

func ticketList(ctx context.Context, c *control.Client) error {
	var list []ticket.State
	if err := c.Do(ctx, "GET", "/tickets", nil, &list); err != nil {
		return err
	}
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

func ticketShow(ctx context.Context, c *control.Client, id string) error {
	var out struct {
		State  ticket.State   `json:"state"`
		Events []ticket.Event `json:"events"`
	}
	if err := c.Do(ctx, "GET", "/tickets/"+id, nil, &out); err != nil {
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
