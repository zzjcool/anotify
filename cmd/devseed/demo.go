// demo.go —— `devseed -demo`：为开发环境播种 v4 真实资源，并跑一条**真实**事件链。
//
// v4 的资源形状与 v3 完全不同（`docs/design/v4/{capability,source}.md` 是唯一权威）：
//
//   - 能力 = `params` + `next` + `run`：没有 ports / outputs / 边。**边是 `next` 的
//     派生投影**（由 store 在同一事务里重建），不是用户实体，也没有写它的 API；
//   - 源   = `kind`(cron|webhook|manual) + `config` + `targets`：入口边同样是
//     `targets` 的派生投影；
//   - 出口 = 脚本里的 `anode emit <name>`（`ANOTIFY_EMIT_ENDPOINT` +
//     `ANOTIFY_TASK_TOKEN`，capability.md §4.0）。
//
// 所以这里只做三件事，一件也不越界：
//
//  1. 用 store 的真实写入方法播种能力 / 源 / 节点（**不写 edges、不写 deployments、
//     不写 tasks** —— 前两个本来就是派生投影 / 人工动作）；
//  2. 用真实 WS hello 把一台节点挂上线（身份来自节点 token，本机事实一并上报）；
//  3. 通过真实 HTTP `POST /v1/namespaces/{ns}/events`（Bearer API Key）投递事件，
//     让中枢自己完成「事件 → 路由 → 任务 / 剪枝留痕」，然后**有界地**观察真实事实。
//
// # 「不造假」这条线画在哪
//
// v3 的 devseed 在进程内跑了一个假执行器：编出 `succeeded` + 出口，往库里灌没有
// 发生过的成功。v4 不这么干：
//
//   - **不**插 Task 行、**不**写 result、**不**写 deployments、**不**代替节点 ack/result；
//   - 示例任务一定是中枢按 `spec.next` / `source.targets` 自己建出来的（pending）；
//   - 这里连的是一台**没有任何执行器、也没有并发槽位**的节点（`capabilities: []` /
//     `instances: []`），因此不会被派到任务，也不会抢走真实 `anode` 的活。
//
// 于是 `-demo` 的产出是：真实资源 + 真在线节点 + 真事件 / 真任务 / 真剪枝留痕。
// 任务相位由中枢按事实给出（没有部署时就是 `pending`，`EligibleNodesForTask` 报
// `NotDeployed`），devseed 不催熟它——要看到真执行，得让一台装了 `anode` 的节点把
// 这些能力部署上去（`POST …/capabilities/{name}/deploy`，capability.md §5）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

// demoNodeName 是示例节点的名字（集群内唯一；重开 devseed 复用同一行，只轮换 token）。
const demoNodeName = "demo-node"

// ---------- 能力 ----------

// demoCapability 是一条示例能力定义（v4：params / next / run 三件事）。
type demoCapability struct {
	// Name 是能力名（谱系：`next` 引用它、任务记它）。改名 = 另一个能力。
	Name string
	// DisplayName 纯展示，不进 digest（capability.md §2.2）。
	DisplayName string
	// Next 是下游引用（`name` 或 `name@version`）。空 = **终端能力**（跑完就结束）。
	Next []string
	// Params 是输入契约（JSON Schema 对象；省略 = 怎么传都过）。
	Params json.RawMessage
	// Script 是能力的**实现**（`run.sh` 正文）。
	//
	// v4 只有这一种实现形式（capability.md §7）：没有 handler、没有中枢内置执行器。
	// 正文是真脚本——真节点上的 anode 会把它展开成 0700 的入口文件直接执行。
	Script string
}

// demoCapabilityRequires 是示例能力共同声明「节点要提供什么」。
//
// 两条都是脚本正文里真的会用到的东西：`bash` 跑 shebang 与 `[ ]`，`anode` 发 emit。
// 声明了它，节点匹配（`NodesAvailable` 的 `CommandMissing`）才是在回答真问题。
// devseed 自己也会对它们做一次真实 `LookPath` 探测并打印结果。
var demoCapabilityRequires = []string{"bash", "anode"}

// demoCapabilities 是示例能力，**按依赖顺序**排列：终端在前，上游在后。
//
// 顺序不是为了 store（`next` 的名字在 store 层不查存在性），而是让「先建出来的
// 被谁引用」这件事在日志里读起来是一条链，也避免将来加 `unknown_target` 校验时返工。
//
// 链条（v4 里没有「端口」，分支是脚本自己的 `if`）：
//
//	分析改动 analyze-pr ──┬─(改动 >200 行)──> 按意见修 apply-fix ─┐
//	                      └─(改动 ≤200 行)──────────────────────┴─> 部署预览 deploy-preview
//
// apply-fix / deploy-preview 都是**先**建好的：它们是 analyze-pr 的 next。
var demoCapabilities = []demoCapability{
	{
		Name:        "deploy-preview",
		DisplayName: "部署预览环境",
		Params:      json.RawMessage(`{"type":"object","properties":{"repo":{"type":"string"}}}`),
		Script: `#!/usr/bin/env bash
# 示例能力：链条终点（next: []），所以不发 emit。
# 参数从 ANOTIFY_PARAM_<NAME> 进来（capability.md §2.3），不是单个 JSON 变量。
set -euo pipefail
repo="${ANOTIFY_PARAM_REPO:-unknown}"
echo "deploy-preview · repo=${repo}（示例：这里本该真的部署预览环境）"
`,
	},
	{
		Name:        "apply-fix",
		DisplayName: "按评审意见改代码",
		Next:        []string{"deploy-preview"},
		Params:      json.RawMessage(`{"type":"object","properties":{"repo":{"type":"string"}}}`),
		Script: `#!/usr/bin/env bash
# 示例能力：修完交给 deploy-preview。出口只有 anode emit（capability.md §4.0）。
set -euo pipefail
repo="${ANOTIFY_PARAM_REPO:-unknown}"
echo "apply-fix · repo=${repo}（示例：这里本该真的改代码）"
anode emit deploy-preview --data "{\"repo\":\"${repo}\"}"
`,
	},
	{
		Name:        "analyze-pr",
		DisplayName: "分析 PR 改动",
		Next:        []string{"apply-fix", "deploy-preview"},
		// changedLines 必填且**没有 default**：少传就是一次真实剪枝（param_missing），
		// 不是让能力自己猜个数。repo / title 是可选，只用来展示「同名直取」。
		Params: json.RawMessage(`{
  "type": "object",
  "properties": {
    "changedLines": {"type": "integer"},
    "repo": {"type": "string"},
    "title": {"type": "string"}
  },
  "required": ["changedLines"]
}`),
		Script: `#!/usr/bin/env bash
# 示例能力：改动超过 200 行就交给 apply-fix，否则直接去 deploy-preview。
# 分支是脚本自己的 if —— v4 没有「能力多出口」这种概念（capability.md §1）。
set -euo pipefail
lines="${ANOTIFY_PARAM_CHANGEDLINES:-0}"
repo="${ANOTIFY_PARAM_REPO:-unknown}"
title="${ANOTIFY_PARAM_TITLE:-}"
echo "analyze-pr · repo=${repo} changedLines=${lines} title=${title}"
if [ "${lines}" -gt 200 ]; then
  echo "→ ${lines} 行改动：需要返工，交给 apply-fix"
  anode emit apply-fix --data "{\"repo\":\"${repo}\"}"
else
  echo "→ ${lines} 行改动：直接通过，交给 deploy-preview"
  anode emit deploy-preview --data "{\"repo\":\"${repo}\"}"
fi
`,
	},
}

// ---------- 源 ----------

// demoSource 是一条示例源定义（v4：kind / config / targets）。
type demoSource struct {
	// Name 是命名空间内唯一的源名，也是事件投递体里的 `source`。
	Name        string
	DisplayName string
	// Kind ∈ cron / webhook / manual（注册表里的内置项）。
	Kind string
	// Config 是 kind 专属配置。webhook 的 token 留空 = store 创建时随机生成。
	Config json.RawMessage
	// Targets 是出口声明：能力引用 + 参数模板（`$(payload.点号路径)`）。
	Targets []sourcespec.Target
}

// demoSources 覆盖三个内置 kind，各自接一条链路：
//
//	github（webhook）  → analyze-pr        （外部系统 POST /v1/hooks/<token> 或本文件的 events 入口）
//	nightly（cron）    → analyze-pr        （中枢按表达式到点触发；这里只播种，不等它到点）
//	release-preview（manual）→ deploy-preview（人在页面上点一下）
var demoSources = []demoSource{
	{
		Name:        "github",
		DisplayName: "GitHub PR",
		Kind:        store.SourceKindWebhook,
		Config: json.RawMessage(`{
  "payloadSchema": {
    "type": "object",
    "required": ["number"],
    "properties": {
      "number": {"type": "integer"},
      "title": {"type": "string"},
      "repository": {"type": "object", "properties": {"full_name": {"type": "string"}}},
      "changedLines": {"type": "integer"}
    }
  },
  "sample": {
    "number": 1284,
    "title": "修复登录超时后无限重试",
    "repository": {"full_name": "acme/api"},
    "changedLines": 42
  }
}`),
		Targets: []sourcespec.Target{{
			Capability: "analyze-pr",
			Params: map[string]any{
				"changedLines": "$(payload.changedLines)",
				"repo":         "$(payload.repository.full_name)",
				"title":        "$(payload.title)",
			},
		}},
	},
	{
		Name:        "nightly",
		DisplayName: "夜间体检",
		Kind:        store.SourceKindCron,
		Config: json.RawMessage(`{
  "expr": "0 3 * * *",
  "tz": "Local",
  "payload": {"changedLines": 12, "note": "nightly 定时体检"}
}`),
		Targets: []sourcespec.Target{{
			Capability: "analyze-pr",
			Params:     map[string]any{"changedLines": "$(payload.changedLines)"},
		}},
	},
	{
		Name:        "release-preview",
		DisplayName: "手动预览",
		Kind:        store.SourceKindManual,
		Config:      json.RawMessage(`{"payload": {"repo": "acme/api"}}`),
		Targets: []sourcespec.Target{{
			Capability: "deploy-preview",
			Params:     map[string]any{"repo": "$(payload.repo)"},
		}},
	},
}

// ---------- 事件 ----------

// demoEvent 是一次要投递给中枢的事件（走真实 HTTP ingress）。
type demoEvent struct {
	// Source 是**源名**（不是 id、不是 token）：它决定哪份 targets 模板生效。
	Source string
	// Type 是事件类型；空 = 回落源名（source.md §5）。
	Type string
	// DeliveryID 是外部投递幂等键（source.md §3.3 D7）。
	//
	// 用**稳定**的值是 devseed 重开幂等的关键：窗口内同一条再投一次只算一条事件、
	// 不重复派发，回执里给的是原来那条 event/trace。
	DeliveryID string
	// Payload 是投递体里的载荷；nil = 只用源配置的打底载荷（cron / manual）。
	Payload map[string]any
}

// demoEvents 覆盖「小改 / 大改 / 缺必填字段 / cron / manual」五种真实走向。
//
// 第三条**故意**不带 changedLines：analyze-pr 的 params 里它是必填且没有 default，
// 所以这条事件会真的被剪枝（`param_missing`），并在 events 表里留下 `source='route'`
// 的痕——剪枝不是错误，是这个系统应有的可观测性（source.md §6）。
var demoEvents = []demoEvent{
	{
		Source: "github", Type: "pull_request.opened", DeliveryID: "devseed-gh-1284",
		Payload: map[string]any{
			"number": 1284, "title": "修复登录超时后无限重试",
			"repository": map[string]any{"full_name": "acme/api"}, "changedLines": 42,
		},
	},
	{
		Source: "github", Type: "pull_request.opened", DeliveryID: "devseed-gh-1285",
		Payload: map[string]any{
			"number": 1285, "title": "重构事件派发器（大改）",
			"repository": map[string]any{"full_name": "acme/api"}, "changedLines": 612,
		},
	},
	{
		Source: "github", Type: "pull_request.opened", DeliveryID: "devseed-gh-1287",
		Payload: map[string]any{
			"number": 1287, "title": "缺 changedLines 的投递（演示剪枝）",
			"repository": map[string]any{"full_name": "acme/api"},
		},
	},
	{Source: "nightly", Type: "nightly.healthcheck", DeliveryID: "devseed-nightly-1"},
	{Source: "release-preview", Type: "manual.run", DeliveryID: "devseed-manual-1"},
}

// ---------- 播种（全部走 store 的真实写入路径） ----------

// seedCapabilities 播种示例能力，返回按创建顺序排列的行。
//
// Version 留 **0 = 内容寻址**（capability.md §2.2）：同一份正文重跑会复用同一行
// （foldKey 命中 → reuse，不新建版本），所以 `devseed -demo` 可以反复跑。
func seedCapabilities(ctx context.Context, db *store.DB, namespaceID string) ([]*store.CapabilityVersion, error) {
	out := make([]*store.CapabilityVersion, 0, len(demoCapabilities))
	for _, d := range demoCapabilities {
		c := &store.CapabilityVersion{
			NamespaceID: namespaceID,
			Name:        d.Name,
			DisplayName: d.DisplayName,
			Spec: artifact.Spec{
				Params: d.Params,
				Next:   append([]string(nil), d.Next...),
				Run: artifact.Run{
					Entrypoint: "run.sh",
					Files:      map[string]string{"run.sh": d.Script},
				},
				Requires: artifact.Requires{Commands: append([]string(nil), demoCapabilityRequires...)},
				// 示例能力给一个本地执行超时：真节点上跑飞了也不会永远挂着。
				Lifecycle: artifact.Lifecycle{TimeoutSec: 120},
			},
		}
		if err := db.CreateCapability(ctx, c); err != nil {
			return nil, fmt.Errorf("建能力 %s: %w", d.Name, err)
		}
		out = append(out, c)
	}
	return out, nil
}

// seedSources 播种示例源；已存在就**收敛**（targets / config 变了才写）。
//
// 入口边由 store 在同一个事务里按 targets 重建（graph.go 的 ReplaceSourceEdges），
// 所以这里不需要（也不能）自己写 edges。
func seedSources(ctx context.Context, db *store.DB, namespaceID string) ([]*store.Source, error) {
	out := make([]*store.Source, 0, len(demoSources))
	for _, d := range demoSources {
		existing, err := db.GetSourceByName(ctx, namespaceID, d.Name)
		switch {
		case err == nil:
			// webhook 的 token 是凭据：合并时省略 = 保留已有（sourcespec.MergeConfig
			// 的既定语义），否则每跑一次 devseed 都会把 hook URL 换掉。
			merged, merr := sourcespec.MergeConfig(d.Kind, existing.Config, d.Config)
			if merr != nil {
				return nil, fmt.Errorf("合并源 %s 的 config: %w", d.Name, merr)
			}
			if sourcespec.SameConfig(d.Kind, existing.Config, merged) &&
				sourcespec.SameTargets(existing.Targets, d.Targets) && existing.Enabled {
				out = append(out, existing)
				continue
			}
			updated := *existing
			updated.DisplayName = d.DisplayName
			updated.Config = merged
			updated.Targets = d.Targets
			updated.Enabled = true
			if uerr := db.UpdateSource(ctx, &updated); uerr != nil {
				return nil, fmt.Errorf("更新源 %s: %w", d.Name, uerr)
			}
			out = append(out, &updated)
		case errors.Is(err, store.ErrSourceNotFound):
			s := &store.Source{
				NamespaceID: namespaceID,
				Name:        d.Name,
				DisplayName: d.DisplayName,
				Kind:        d.Kind,
				Config:      d.Config,
				Targets:     d.Targets,
				Enabled:     true,
			}
			if cerr := db.CreateSource(ctx, s); cerr != nil {
				return nil, fmt.Errorf("建源 %s: %w", d.Name, cerr)
			}
			out = append(out, s)
		default:
			return nil, fmt.Errorf("读源 %s: %w", d.Name, err)
		}
	}
	return out, nil
}

// seedNode 建/复用示例节点，返回节点 id 与新生成的明文 token。
//
// 复用时**只轮换 token**（`SetNodeTokenHash`），不新建行：早先的实现「撞名就加序号
// 新建」会让库里堆出一台台永远离线、永远连不上的死节点。
func seedNode(ctx context.Context, db *store.DB, name string) (nodeID, token string, err error) {
	existing, gerr := db.GetNodeByName(ctx, name)
	switch {
	case gerr == nil:
		fresh, hash, terr := store.NewNodeToken()
		if terr != nil {
			return "", "", terr
		}
		if serr := db.SetNodeTokenHash(ctx, existing.ID, hash); serr != nil {
			return "", "", fmt.Errorf("轮换节点 %s 的 token: %w", name, serr)
		}
		return existing.ID, fresh, nil
	case errors.Is(gerr, store.ErrNotFound):
		n := &store.Node{Name: name}
		created, cerr := db.CreateNode(ctx, n)
		if cerr != nil {
			return "", "", fmt.Errorf("建节点 %s: %w", name, cerr)
		}
		return n.ID, created, nil
	default:
		return "", "", fmt.Errorf("读节点 %s: %w", name, gerr)
	}
}

// ---------- 节点（真实 WS 握手） ----------

// demoNode 是进程内的**真节点连接**：连 /v1/node/stream、发 hello、读帧。
//
// 它没有任何执行器：不接单、不 ack、不回报。这不是偷懒，是 v4 的边界——能力只在
// 装了 anode 的节点上跑（capability.md §0/§7），devseed 再造一个执行器就是第二份真相。
type demoNode struct {
	conn *websocket.Conn
	name string
	once sync.Once
}

// dialDemoNode 建立 WS 连接并完成 v4 hello 握手（读到 welcome 才算上线）。
//
// hello 的形状是 v4 的（config.md §4.2 / anode.md §3.3）：身份在**帧里**（token），
// namespaces 是**申报**的 ns 名列表，facts 是本机内置事实，capabilities / instances
// 是能力探测与物理槽位。
func dialDemoNode(ctx context.Context, baseURL, nodeName, token, namespaceSlug string) (*demoNode, error) {
	url := nodeStreamURL(baseURL)

	hello := protocol.HelloFrame{
		Type:  "hello",
		Name:  nodeName,
		Token: token,
		Namespaces: []string{
			namespaceSlug,
		},
		LocalVars: map[string]string{},
		Facts:     demoNodeFacts(),
		// 两个空数组是**声明**，不是省略：
		//   - capabilities: 这台"节点"上没有部署任何能力（devseed 不持有产物字节）；
		//   - instances:    它没有并发槽位（没有执行器）。
		// 中枢据此不会把任务派给它 —— 这正是我们要的事实，别为了好看谎报。
		Capabilities: []protocol.HelloCapability{},
		Instances:    []protocol.HelloInstance{},
	}
	raw, err := json.Marshal(hello)
	if err != nil {
		return nil, fmt.Errorf("编码 hello: %w", err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, derr := websocket.Dial(dialCtx, url, nil)
	if derr != nil {
		return nil, fmt.Errorf("WS 连不上 %s（server 没在跑？）: %w", url, derr)
	}

	writeCtx, writeCancel := context.WithTimeout(ctx, 10*time.Second)
	defer writeCancel()
	if werr := conn.Write(writeCtx, websocket.MessageText, raw); werr != nil {
		_ = conn.Close(websocket.StatusInternalError, "hello write failed")
		return nil, fmt.Errorf("发 hello: %w", werr)
	}
	// 读到 welcome 才算真的上线：中枢在 welcome **之前**已写完 node 状态与配置对齐，
	// 所以「收到 welcome」是「库里已经是 online」的证据，不需要 sleep 猜。
	readCtx, readCancel := context.WithTimeout(ctx, 15*time.Second)
	defer readCancel()
	for {
		_, data, rerr := conn.Read(readCtx)
		if rerr != nil {
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return nil, fmt.Errorf("等 welcome 失败（hello 被拒？）: %w", rerr)
		}
		frameType, perr := protocol.FrameTypeOf(data)
		if perr != nil {
			continue
		}
		if frameType == protocol.FrameWelcome {
			break
		}
	}
	return &demoNode{conn: conn, name: nodeName}, nil
}

// watch 读帧直到连接结束。任务派发帧只记录：这台节点没有执行器，接了就是伪执行。
func (n *demoNode) watch(ctx context.Context) {
	for {
		_, data, err := n.conn.Read(ctx)
		if err != nil {
			return
		}
		frameType, ferr := protocol.FrameTypeOf(data)
		if ferr != nil {
			continue
		}
		if frameType == protocol.FrameTaskDispatch {
			log.Printf("  ℹ️  收到 task.dispatch（节点没有执行器，不接单；部署是人工动作）")
		}
	}
}

// close 断开连接（幂等）。断开后中枢会把节点标 offline —— 这是真实事实，不是清理噪音。
func (n *demoNode) close() {
	n.once.Do(func() {
		_ = n.conn.Close(websocket.StatusNormalClosure, "devseed demo done")
		log.Printf("  节点 %s 已断开（中枢把它标 offline）", n.name)
	})
}

// nodeStreamURL 把 HTTP base URL 换成 WS 的节点入口。
func nodeStreamURL(baseURL string) string {
	u := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	u = strings.Replace(u, "https://", "wss://", 1)
	u = strings.Replace(u, "http://", "ws://", 1)
	return u + "/v1/node/stream"
}

// demoNodeFacts 是本机内置事实（status.* 那一组）。值全部来自真实探测。
func demoNodeFacts() protocol.NodeFacts {
	host, _ := os.Hostname()
	home, _ := os.UserHomeDir()
	return protocol.NodeFacts{
		HostIP:   localIP(),
		Hostname: host,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		CPU:      runtime.NumCPU(),
		WorkRoot: os.TempDir(),
		HomeDir:  home,
	}
}

// localIP 取本机第一个非回环 IPv4；拿不到就回 127.0.0.1。
//
// 不发起 UDP "拨号"探测：devseed 不该为了让 hello 好看而要求外网可达。
func localIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, aerr := iface.Addrs()
		if aerr != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.To4() == nil {
				continue
			}
			return ip.String()
		}
	}
	return "127.0.0.1"
}

// ---------- 事件投递（真实 HTTP ingress） ----------

// preflight 确认目标服务器真的在跑。
//
// 为什么要有这一步：`-demo` 的价值全在「真投递、真握手」，服务器不在时唯一正确的
// 行为是**立刻把原因说清楚并退出**，而不是连写三次 connection refused 或者挂在那里。
func preflight(ctx context.Context, baseURL string) error {
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("-demo 需要一台真实服务器，但连不上 %s/health（先起 ./cmd/server）：%w", baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("-demo 需要一台真实服务器，%s/health 返回 %d", baseURL, resp.StatusCode)
	}
	return nil
}

// ingestDemo 逐条投递示例事件——走真实 `POST /v1/namespaces/{ns}/events`。
//
// 为什么不能同进程直接调 source.Service.Ingest：那等于自己扮演中枢，节点、事件、
// 任务全在这一侧的内存里自娱自乐。走 HTTP 就是"像一个真实的 webhook 发送方"，
// 中枢用它自己的路由/剪枝/幂等逻辑处理这批事件——这才叫真实事件链。
func ingestDemo(ctx context.Context, baseURL, namespaceSlug, apiKey string) ([]*source.Receipt, error) {
	endpoint := strings.TrimSuffix(baseURL, "/") + "/v1/namespaces/" + namespaceSlug + "/events"
	out := make([]*source.Receipt, 0, len(demoEvents))
	for _, e := range demoEvents {
		body := map[string]any{"source": e.Source}
		if e.Type != "" {
			body["type"] = e.Type
		}
		if e.DeliveryID != "" {
			body["id"] = e.DeliveryID
		}
		if e.Payload != nil {
			body["payload"] = e.Payload
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("编码事件 %s: %w", e.DeliveryID, err)
		}
		reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		req, rerr := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(raw))
		if rerr != nil {
			cancel()
			return nil, rerr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, derr := http.DefaultClient.Do(req)
		if derr != nil {
			cancel()
			return nil, fmt.Errorf("投递事件 %s: %w", e.DeliveryID, derr)
		}
		// 回执是外部输入：读取一律设上限，不信任对端长度。
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		cancel()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("投递事件 %s（source=%s）返回 %d: %s",
				e.DeliveryID, e.Source, resp.StatusCode, strings.TrimSpace(string(respBody)))
		}
		var receipt source.Receipt
		if jerr := json.Unmarshal(respBody, &receipt); jerr != nil {
			return nil, fmt.Errorf("解析事件回执 %s: %w", e.DeliveryID, jerr)
		}
		out = append(out, &receipt)
		// 别把一批事件挤在同一毫秒：列表面板按时间排序，同一刻会看不出先后。
		time.Sleep(120 * time.Millisecond)
	}
	return out, nil
}

// ---------- 观察（只读真实事实） ----------

// taskObservation 是一条示例任务的观测结果。
type taskObservation struct {
	task   *store.Task
	reason string // 不可推进的真实 reason（空 = 有候选节点）
}

// waitTasks 有界地等任务行可见（事件与任务是同一事务写的，正常情况下立即命中）。
func waitTasks(ctx context.Context, db *store.DB, namespaceID string, ids []string, limit time.Duration) ([]*store.Task, error) {
	deadline := time.Now().Add(limit)
	for {
		tasks := make([]*store.Task, 0, len(ids))
		missing := 0
		for _, id := range ids {
			t, err := db.GetTask(ctx, namespaceID, id)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					missing++
					continue
				}
				return nil, fmt.Errorf("读任务 %s: %w", id, err)
			}
			tasks = append(tasks, t)
		}
		if missing == 0 {
			return tasks, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("有 %d 条任务在 %s 内没有落库（事件回执与 tasks 表不一致）", missing, limit)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// observeTasks 给每条任务贴一个真实的可推进判定。
//
// 判定用的是**中枢自己的调度逻辑**（`EligibleNodesForTask` → `nodeIneligibilityReason`），
// 所以「为什么没跑」这个答案是系统给的，不是 devseed 猜的。没有 deployments 行时它
// 会如实回答 `NotDeployed`——那不是缺陷，是「还没人把这份能力部署到某台节点上」。
func observeTasks(ctx context.Context, db *store.DB, tasks []*store.Task) ([]taskObservation, error) {
	// 节点名只在日志里用，但 `nd_xxx` 对人不友好：先把 id → name 读一次。
	names, err := nodeNameIndex(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make([]taskObservation, 0, len(tasks))
	for _, t := range tasks {
		obs := taskObservation{task: t}
		if !store.IsTerminalPhase(t.Phase) {
			eligible, reasons, err := db.EligibleNodesForTask(ctx, t)
			if err != nil {
				return nil, fmt.Errorf("判定任务 %s 的候选节点: %w", t.ID, err)
			}
			if len(eligible) == 0 {
				obs.reason = formatReasons(reasons, names)
			}
		}
		out = append(out, obs)
	}
	return out, nil
}

// nodeNameIndex 读一次节点表，给日志里的 reason 用名字而不是 `nd_…` id。
func nodeNameIndex(ctx context.Context, db *store.DB) (map[string]string, error) {
	nodes, err := db.ListNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出节点: %w", err)
	}
	out := make(map[string]string, len(nodes))
	for _, n := range nodes {
		out[n.ID] = n.Name
	}
	return out, nil
}

// formatReasons 把 nodeID → reason 压成一段稳定可读的文本（优先用节点名）。
func formatReasons(reasons map[string]string, names map[string]string) string {
	if len(reasons) == 0 {
		return "no_eligible_node"
	}
	parts := make([]string, 0, len(reasons))
	for node, reason := range reasons {
		label := node
		if name, ok := names[node]; ok && name != "" {
			label = name
		}
		if reason == "" {
			reason = "eligible"
		}
		parts = append(parts, label+"="+reason)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// waitTerminal 有界地等这批任务进终态；返回是否全部终态。
func waitTerminal(ctx context.Context, db *store.DB, namespaceID string, ids []string, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		all := true
		for _, id := range ids {
			t, err := db.GetTask(ctx, namespaceID, id)
			if err != nil || !store.IsTerminalPhase(t.Phase) {
				all = false
				break
			}
		}
		if all {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
}

// probeCommands 对示例能力的 requires.commands 做一次真实 LookPath 探测。
//
// 这不是"装饰"：真节点在 hello 里报的就是这份事实，中枢靠它回答 `CommandMissing`。
// devseed 在本机探一次，等于提前告诉开发者「这台机器就算跑 anode 也缺什么」。
func probeCommands(commands []string) []protocol.HelloCommand {
	out := make([]protocol.HelloCommand, 0, len(commands))
	for _, name := range commands {
		path, err := exec.LookPath(name)
		out = append(out, protocol.HelloCommand{Name: name, Found: err == nil, Path: path})
	}
	return out
}

// ---------- 主流程 ----------

// runDemo 是 `-demo` 的入口：播种资源 → 真节点上线 → 真事件投递 → 有界观察事实。
func runDemo(ctx context.Context, db *store.DB, nsID, baseURL, apiKey string, followSec int) error {
	ns, err := db.GetNamespace(ctx, nsID)
	if err != nil {
		return fmt.Errorf("读命名空间 %s: %w", nsID, err)
	}
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")

	// 先确认服务器在跑：这一步失败要**立刻**说清楚原因，不留下半套资源。
	if err := preflight(ctx, base); err != nil {
		return err
	}

	log.Printf("▶ 播种 v4 示例资源（namespace %s / %s）…", ns.Slug, ns.ID)

	caps, err := seedCapabilities(ctx, db, nsID)
	if err != nil {
		return err
	}
	for _, c := range caps {
		log.Printf("  能力 %s@%d digest=%s（next=%v）", c.Name, c.Version, shortDigest(c.Digest), c.Spec.Next)
	}

	srcs, err := seedSources(ctx, db, nsID)
	if err != nil {
		return err
	}
	for _, s := range srcs {
		log.Printf("  源 %s（kind=%s）%s", s.Name, s.Kind, hookHint(s))
	}

	nodeID, token, err := seedNode(ctx, db, demoNodeName)
	if err != nil {
		return err
	}
	log.Printf("  节点已注册: %s（%s）", demoNodeName, nodeID)
	for _, probe := range probeCommands(demoCapabilityRequires) {
		if probe.Found {
			log.Printf("  本机命令探测 %s → %s", probe.Name, probe.Path)
		} else {
			log.Printf("  本机命令探测 %s → 未找到（真节点上会判 CommandMissing）", probe.Name)
		}
	}

	node, err := dialDemoNode(ctx, base, demoNodeName, token, ns.Slug)
	if err != nil {
		return err
	}
	defer node.close()
	go node.watch(ctx)
	log.Printf("  节点已上线（真实 WS 握手；capabilities=0 / slots=0）")

	receipts, err := ingestDemo(ctx, base, ns.Slug, apiKey)
	if err != nil {
		return err
	}
	log.Printf("  已投递 %d 条事件（POST /v1/namespaces/%s/events）", len(receipts), ns.Slug)
	taskIDs := make([]string, 0, len(receipts))
	for i, r := range receipts {
		duplicate := ""
		if r.Duplicate {
			duplicate = " · duplicate"
		}
		log.Printf("  回执 %s/%s → event=%s tasks=%d pruned=%d%s",
			demoEvents[i].Source, demoEvents[i].Type, shortID(r.EventID),
			len(r.Tasks), len(r.Pruned), duplicate)
		for _, p := range r.Pruned {
			log.Printf("    ✂ 剪枝 %s：%s（%s）", p.Capability, p.Reason, p.Detail)
		}
		for _, t := range r.Tasks {
			taskIDs = append(taskIDs, t.TaskID)
		}
	}
	if len(taskIDs) == 0 {
		return fmt.Errorf("一条事件都没有建出任务：源 targets 与事件载荷对不上，检查上面回执里的剪枝原因")
	}

	tasks, err := waitTasks(ctx, db, nsID, taskIDs, 10*time.Second)
	if err != nil {
		return err
	}
	observations, err := observeTasks(ctx, db, tasks)
	if err != nil {
		return err
	}
	canProgress := false
	for _, obs := range observations {
		phase := obs.task.Phase
		if obs.reason == "" {
			canProgress = true
			log.Printf("  任务 %s → %s（有候选节点）", taskLabel(obs.task), phase)
			continue
		}
		log.Printf("  任务 %s → %s（无候选节点：%s）", taskLabel(obs.task), phase, obs.reason)
	}

	if canProgress {
		// 真的有节点能跑才等：这时"等"是在等真实执行，不是空转。
		limit := time.Duration(followSec) * time.Second
		if limit <= 0 {
			limit = 25 * time.Second
		}
		if waitTerminal(ctx, db, nsID, taskIDs, limit) {
			log.Printf("  链路已全部跑完")
			if final, ferr := reloadTasks(ctx, db, nsID, taskIDs); ferr == nil {
				for _, t := range final {
					log.Printf("  任务 %s → %s", taskLabel(t), t.Phase)
				}
			}
		} else {
			log.Printf("  %s 内任务未收尾（节点在线但没跑完），先断开", limit)
		}
	} else {
		// 没有候选节点 = 这批任务**不会**自己动。说清下一步，别让开发者以为卡住了。
		log.Printf("  任务停在 pending 是真实状态：示例能力还没部署到任何节点")
		log.Printf("    → 部署是人工动作：POST /v1/namespaces/%s/capabilities/{name}/deploy", ns.Slug)
		log.Printf("    → 真执行需要一台装了 anode 的节点（它自己注册、拉包、跑脚本）")
	}

	return summarize(ctx, db, nsID)
}

// reloadTasks 重新读一遍任务（收尾打印用）。
func reloadTasks(ctx context.Context, db *store.DB, namespaceID string, ids []string) ([]*store.Task, error) {
	out := make([]*store.Task, 0, len(ids))
	for _, id := range ids {
		t, err := db.GetTask(ctx, namespaceID, id)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// summarize 打印库里**真实行数**，让"到底种进去了什么"可对账。
func summarize(ctx context.Context, db *store.DB, namespaceID string) error {
	caps, err := db.ListCapabilities(ctx, namespaceID)
	if err != nil {
		return fmt.Errorf("统计能力: %w", err)
	}
	srcs, err := db.ListSources(ctx, namespaceID)
	if err != nil {
		return fmt.Errorf("统计源: %w", err)
	}
	// 事件列表（无 trace / source）默认**不含**剪枝留痕（store.ListEvents 的既定语义：
	// 一次扇出里被剪的边不该把「外部世界发生了什么」淹成两倍长度），所以剪枝单独查一遍。
	events, err := db.ListEvents(ctx, namespaceID, "", "", 500)
	if err != nil {
		return fmt.Errorf("统计事件: %w", err)
	}
	prunes, err := db.ListEvents(ctx, namespaceID, "", store.EventPruneSource, 500)
	if err != nil {
		return fmt.Errorf("统计剪枝留痕: %w", err)
	}
	tasks, err := db.ListTasks(ctx, namespaceID, store.TaskFilter{Limit: 500})
	if err != nil {
		return fmt.Errorf("统计任务: %w", err)
	}
	log.Printf("  数据库事实：能力 %d 行 / 源 %d 行 / 事件 %d 行（其中剪枝留痕 %d 行，合计 %d）/ 任务 %d 行",
		len(caps), len(srcs), len(events), len(prunes), len(events)+len(prunes), len(tasks))
	return nil
}

// taskLabel 是任务的一行标识（`能力@版本`）。
func taskLabel(t *store.Task) string {
	if t == nil {
		return "?"
	}
	return fmt.Sprintf("%s@%d", t.Capability, t.Version)
}

// hookHint 给 webhook 源拼一条可手工调用的提示（token 只在库里，不回显给前端）。
func hookHint(s *store.Source) string {
	if s == nil || s.Kind != store.SourceKindWebhook {
		return ""
	}
	token := sourcespec.ParseConfig(s.Kind, s.Config).Token
	if token == "" {
		return ""
	}
	return "hook=/v1/hooks/" + token
}

// shortDigest / shortID 只用于日志可读性。
func shortDigest(digest string) string {
	if len(digest) <= 19 {
		return digest
	}
	return digest[:19] + "…"
}

func shortID(id string) string {
	if len(id) <= 14 {
		return id
	}
	return id[:14]
}
