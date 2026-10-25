// demo.go —— `devseed -demo`：给开发环境造一份**真跑过**的示例数据。
//
// 为什么不是直接往库里 INSERT 任务行："能派发、能执行、能扇出"才是 Anotify
// 的核心，硬塞的假数据看不出这些。所以这里真的起一个节点（走真实 WS 握手），
// 让它接单、ack、回报结果，整条链自然长出来。
//
// 数据形状：
//
//	内置起点                用户能力
//	github -opened--------> analyze-pr ─needs-fix─> apply-fix ─ok─> deploy-preview
//	cron   -nightly------->               └─approved──────────────────────┘
//	http   -webhook-------> deploy-preview
//	manual -manual.run----> deploy-preview
//
// analyze-pr 按 payload.changedLines 决定走哪个出口，所以同一个图能演
// 「小改直接过」和「大改要返工」两种真实走向。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"bytes"

	"github.com/coder/websocket"
	"github.com/zzjcool/anotify/internal/store"
)

// demoCapability 是一条示例能力定义。
type demoCapability struct {
	Name    string
	Outputs []string
	Desc    string
}

// demoCapabilities 是示例图里的用户能力（内置起点由 store 自动播种）。
var demoCapabilities = []demoCapability{
	{"analyze-pr", []string{"needs-fix", "approved"}, "分析 PR 改动，判断是否需要返工"},
	{"apply-fix", []string{"ok"}, "按评审意见改代码"},
	{"deploy-preview", nil, "部署预览环境"},
}

// demoEdge 是一条边。from 用名字，运行时解析成 id。
type demoEdge struct{ from, port, to string }

var demoEdges = []demoEdge{
	{"github", "pull_request.opened", "analyze-pr"},
	{"github", "pull_request.synchronize", "analyze-pr"},
	{"cron", "nightly.healthcheck", "analyze-pr"},
	{"http", "webhook.incoming", "deploy-preview"},
	{"manual", "manual.run", "deploy-preview"},
	{"analyze-pr", "needs-fix", "apply-fix"},
	{"analyze-pr", "approved", "deploy-preview"},
	{"apply-fix", "ok", "deploy-preview"},
}

// demoEvent 是一次要发的示例事件。
type demoEvent struct {
	Source, Type, IdemKey string
	Payload               map[string]any
}

// demoEvents 覆盖四种起点与两条分支。
//
// changedLines 是 analyze-pr 的分支依据（>200 走 needs-fix）。
var demoEvents = []demoEvent{
	{"github", "pull_request.opened", "demo-pr-1284",
		map[string]any{"number": 1284, "title": "修复登录超时后无限重试", "author": "alice", "changedLines": 42}},
	{"github", "pull_request.opened", "demo-pr-1285",
		map[string]any{"number": 1285, "title": "重构事件派发器（大改）", "author": "bob", "changedLines": 612}},
	{"github", "pull_request.opened", "demo-pr-1286",
		map[string]any{"number": 1286, "title": "补充日语文案", "author": "carol", "changedLines": 8}},
	{"github", "pull_request.synchronize", "demo-pr-1285-sync",
		map[string]any{"number": 1285, "title": "重构事件派发器（已修）", "author": "bob", "changedLines": 640}},
	{"cron", "nightly.healthcheck", "",
		map[string]any{"window": "nightly", "changedLines": 0, "note": "定时体检"}},
	{"http", "webhook.incoming", "",
		map[string]any{"from": "grafana", "alert": "disk_usage_high", "host": "db-primary", "value": "91%"}},
	{"manual", "manual.run", "",
		map[string]any{"by": "demo", "reason": "手动验证预览环境"}},
}

// seedDemo 建图 + 注册节点，返回节点明文 token（worker 要用）。
//
// 幂等：能力「同内容已存在」直接复用；边重复视为已建；节点同名复用并轮换 token。
func seedDemo(ctx context.Context, db *store.DB, orgID, nodeName string) (nodeID, token string, err error) {
	ids := make(map[string]string, len(demoCapabilities)+8)

	// 内置起点（注册组织时已播种，这里只查 id）
	existing, err := db.ListCapabilities(ctx, orgID)
	if err != nil {
		return "", "", fmt.Errorf("list capabilities: %w", err)
	}
	latest := make(map[string]*store.CapabilityVersion)
	for _, c := range existing {
		if cur, ok := latest[c.Name]; !ok || c.Version > cur.Version {
			latest[c.Name] = c
		}
	}
	for _, name := range []string{"github", "cron", "http", "manual"} {
		if c, ok := latest[name]; ok {
			ids[name] = c.ID
		}
	}

	// 用户能力：已存在就复用（devseed 可能重复跑）
	for _, d := range demoCapabilities {
		if c, ok := latest[d.Name]; ok {
			ids[d.Name] = c.ID
			continue
		}
		c := &store.CapabilityVersion{
			OrgID:        orgID,
			Name:         d.Name,
			Version:      1,
			ParamsSchema: json.RawMessage(`{"type":"object"}`),
			Outputs:      append([]string{}, d.Outputs...),
			Source:       store.CapSourceCustom,
		}
		if err := db.CreateCapability(ctx, c); err != nil {
			return "", "", fmt.Errorf("create capability %s: %w", d.Name, err)
		}
		ids[d.Name] = c.ID
	}

	// 边
	for _, e := range demoEdges {
		from, okF := ids[e.from]
		to, okT := ids[e.to]
		if !okF || !okT {
			return "", "", fmt.Errorf("edge %s->%s: 能力缺失（from=%v to=%v）", e.from, e.to, okF, okT)
		}
		if err := db.CreateEdge(ctx, &store.Edge{
			OrgID: orgID, FromCapability: from, FromPort: e.port, ToCapability: to,
		}); err != nil {
			return "", "", fmt.Errorf("create edge %s -%s-> %s: %w", e.from, e.port, e.to, err)
		}
	}

	// 节点：同名已存在就**复用同一行**（只换 token），不要每次多造一个。
	//
	// 早先的实现是"撞名就加序号新建"，结果每跑一次 devseed 库里就多一个
	// 永远离线、永远连不上的死节点，节点页很快就没法看了。
	// 明文 token 只在创建时返回一次，但这里可以自己生成 token 再写 hash
	// （CreateNode 支持预置 TokenHash），所以复用行完全可行。
	if n, err := db.GetNodeByName(ctx, orgID, nodeName); err == nil && n != nil {
		t, hash, err := store.NewNodeToken()
		if err != nil {
			return "", "", err
		}
		if err := db.SetNodeTokenHash(ctx, n.ID, hash); err != nil {
			return "", "", fmt.Errorf("refresh node token: %w", err)
		}
		nodeID, token = n.ID, t
	} else {
		n := &store.Node{OrgID: orgID, Name: nodeName}
		t, err := db.CreateNode(ctx, n)
		if err != nil {
			return "", "", fmt.Errorf("create node: %w", err)
		}
		nodeID, token = n.ID, t
	}

	// 节点声明能力。必须走 ConfirmCapabilities（全量替换）——
	// 它顺带校验能力存在性（不变量 I2：节点不能声明不存在的 @版本）。
	if _, err := db.ConfirmCapabilities(ctx, orgID, nodeID, []store.CapabilityRef{
		{Name: "analyze-pr", Version: 1},
		{Name: "apply-fix", Version: 1},
		{Name: "deploy-preview", Version: 1},
	}); err != nil {
		return "", "", fmt.Errorf("declare capabilities: %w", err)
	}
	return nodeID, token, nil
}

// demoWorker 是一个跑在进程内的真节点：连 WS、握手、接单、回报、心跳。
//
// 它存在的唯一原因是「让示例数据真的走一遍派发与执行」——不然前端看到的
// 全是手工塞进去的状态，看不出扇出、看不出 out 三态。
type demoWorker struct {
	conn     *websocket.Conn
	nodeID   string
	orgID    string
	done     chan struct{}
	assigns  int
	onAssign func(capability, taskID string, out *string)
}

// dialWorker 连上 /v1/node/stream 并完成 hello。
func dialWorker(ctx context.Context, base, token, nodeID, orgID string) (*demoWorker, []store.CapabilityRef, error) {
	url := strings.Replace(base, "http://", "ws://", 1)
	url = strings.Replace(url, "https://", "wss://", 1)
	url = strings.TrimSuffix(url, "/") + "/v1/node/stream"

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		return nil, nil, fmt.Errorf("ws dial: %w", err)
	}

	caps := []store.CapabilityRef{
		{Name: "analyze-pr", Version: 1},
		{Name: "apply-fix", Version: 1},
		{Name: "deploy-preview", Version: 1},
	}
	hello := map[string]any{
		"type":     "hello",
		"node_id":  nodeID,
		"protocol": 1,
		"capabilities": func() []map[string]any {
			out := make([]map[string]any, 0, len(caps))
			for _, c := range caps {
				out = append(out, map[string]any{"capability": c.Name, "version": c.Version})
			}
			return out
		}(),
		"instances": []map[string]any{{"slot": 0, "concurrency": 4}},
	}
	b, _ := json.Marshal(hello)
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		conn.Close(websocket.StatusInternalError, "hello write failed")
		return nil, nil, fmt.Errorf("send hello: %w", err)
	}
	return &demoWorker{conn: conn, nodeID: nodeID, orgID: orgID, done: make(chan struct{})}, caps, nil
}

// run 读帧直到 ctx 结束。assign 帧会 ack + 回报 result。
func (w *demoWorker) run(ctx context.Context) {
	defer close(w.done)
	// 心跳：服务端 20s 没收到任何上行帧就判超时断连，所以 8s 一次
	hb := time.NewTicker(8 * time.Second)
	defer hb.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hb.C:
				b, _ := json.Marshal(map[string]any{"type": "heartbeat", "ts": time.Now().Unix()})
				if err := w.conn.Write(ctx, websocket.MessageText, b); err != nil {
					return
				}
			}
		}
	}()

	for {
		typ, raw, err := w.conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var f struct {
			Type       string         `json:"type"`
			TaskID     string         `json:"task_id"`
			Capability string         `json:"capability"`
			Params     map[string]any `json:"params"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			continue
		}
		if f.Type != "assign" {
			continue
		}
		go w.handle(ctx, f.TaskID, f.Capability, f.Params)
	}
}

// handle 执行一个任务：ack → 模拟耗时 → result。
func (w *demoWorker) handle(ctx context.Context, taskID, capability string, params map[string]any) {
	send := func(v any) {
		b, _ := json.Marshal(v)
		_ = w.conn.Write(ctx, websocket.MessageText, b)
	}
	send(map[string]any{"type": "ack", "task_id": taskID})
	time.Sleep(600 * time.Millisecond) // 让「运行中」这个状态看得见

	out, summary := demoResult(capability, params)
	// out 三态：nil → 明确到头（JSON null）；否则给出口名
	send(map[string]any{
		"type": "result", "task_id": taskID,
		"status": "succeeded", "out": out, "summary": summary,
	})
	w.assigns++
	if w.onAssign != nil {
		w.onAssign(capability, taskID, out)
	}
}

// demoResult 按能力算出口，模拟真实的「按输入走不同分支」。
func demoResult(capability string, params map[string]any) (*string, string) {
	str := func(p *string) *string { return p }
	switch capability {
	case "analyze-pr":
		lines := 0
		if v, ok := params["changedLines"].(float64); ok {
			lines = int(v)
		}
		if lines > 200 {
			return str(ptr("needs-fix")), fmt.Sprintf("改动 %d 行，需要返工", lines)
		}
		return str(ptr("approved")), fmt.Sprintf("改动 %d 行，直接通过", lines)
	case "apply-fix":
		return str(ptr("ok")), "已按评审意见修改"
	case "deploy-preview":
		return nil, "预览环境已部署"
	}
	return nil, "完成"
}

func ptr(s string) *string { return &s }

// ingestDemo 逐条发事件——**走 HTTP `/v1/events`**，不是直接调 trigger。
//
// 为什么不能同进程调 trigger.Ingest：派发器需要一个 Publisher（ws.NodeHub）
// 才能把 assign 帧真的推给节点，而那个 hub 活在 server 进程的内存里。
// devseed 是独立进程，自己 new 一个 hub 只会把帧推给空气。
// 走 HTTP 就是"像个真实的 webhook 发送方"——这也正是示例数据的意义。
func ingestDemo(ctx context.Context, baseURL, apiKey string) (int, error) {
	n := 0
	for _, e := range demoEvents {
		body := map[string]any{
			"source":  e.Source,
			"type":    e.Type,
			"payload": e.Payload,
		}
		if e.IdemKey != "" {
			body["idemKey"] = e.IdemKey
		}
		b, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			strings.TrimSuffix(baseURL, "/")+"/v1/events", bytes.NewReader(b))
		if err != nil {
			return n, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Printf("⚠️  事件 %s/%s 失败: %v", e.Source, e.Type, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			log.Printf("⚠️  事件 %s/%s 返回 %d", e.Source, e.Type, resp.StatusCode)
			continue
		}
		n++
		time.Sleep(350 * time.Millisecond) // 别把一批事件挤在同一毫秒
	}
	return n, nil
}

// idemOrNil 把空串当作「没有幂等键」（没有稳定投递 ID 的来源就是这种情况）。
func idemOrNil(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// runDemo 是 -demo 的入口：建图 → 起节点 → 发事件 → 等链跑完 → 断开。
//
// 全程走真实路径（trigger.Ingest + 真 WS 握手），所以数据是真跑出来的。
func runDemo(ctx context.Context, db *store.DB, orgID, baseURL, apiKey string, followSec int) error {
	log.Printf("▶ 造示例数据：建图 + 注册节点…")
	nodeID, token, err := seedDemo(ctx, db, orgID, "demo-worker")
	if err != nil {
		return err
	}
	log.Printf("  节点已注册: %s", nodeID)

	ctx, cancel := context.WithTimeout(ctx, time.Duration(followSec+30)*time.Second)
	defer cancel()

	w, caps, err := dialWorker(ctx, baseURL, token, nodeID, orgID)
	if err != nil {
		return fmt.Errorf("节点上线失败（%s 是否在跑？）: %w", baseURL, err)
	}
	defer w.conn.Close(websocket.StatusNormalClosure, "demo done")

	done := make(chan struct{})
	go func() {
		w.run(ctx)
		close(done)
	}()
	log.Printf("  节点已上线（%d 个能力，真实 WS 握手）", len(caps))

	// 等 welcome 后立刻发事件；节点在跑，链路会自己长出来
	time.Sleep(400 * time.Millisecond)

	// 只关心**本次投递产生**的任务：库里可能还有以前遗留的未终态行
	// （比如上次 devseed 提前断开留下的 pending），那些不该让这里一直等。
	since := store.Now()
	n, err := ingestDemo(ctx, baseURL, apiKey)
	if err != nil {
		return err
	}
	log.Printf("  已投递 %d 条事件，等待任务链执行…", n)

	// 等**链真的跑完**再断开，而不是睡固定秒数就拔线。
	//
	// 睡固定时长的后果很脏：断连时还有任务在途，它们会回到 pending（租约巡检
	// 再重派），库里于是留下永远不动的 pending 行——看起来像 bug，其实是
	// devseed 自己走得太早。
	deadline := time.After(time.Duration(followSec) * time.Second)
	tick := time.NewTicker(800 * time.Millisecond)
	defer tick.Stop()
	started := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-deadline:
			log.Printf("  节点执行了 %d 个任务（等待超时），断开", w.assigns)
			return nil
		case <-tick.C:
			open, err := db.CountOpenTasksSince(ctx, orgID, since)
			if err != nil {
				continue
			}
			if open == 0 && time.Since(started) > 2*time.Second {
				log.Printf("  节点执行了 %d 个任务，链路已全部跑完，断开", w.assigns)
				return nil
			}
		}
	}
}
