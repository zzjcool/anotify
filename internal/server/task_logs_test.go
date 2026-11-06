package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/ws"
)

// 本文件覆盖任务日志的**按需取**端点（CONTRACT-logs-on-node §2）：
//
//	GET /v1/tasks/{taskId}/logs?namespaceId=…（会话 + 命名空间隔离，调用方是控制台）
//
// 判据按「回归会静默」的程度排：
//  1. **原文逐字透传**：中文 / 空行 / 截断标记一个字节都不许变——日志的价值全在
//     字节级还原，任何 trim / 转义都会让「为什么炸了」失真；
//  2. **取不到 = 200 + available:false**：节点离线 / 没档案 / 超时 / 意外错误四种
//     全部**不得**回 5xx（前端已有的降级分支只认 200），且 reason 落在封闭集合里；
//  3. **每次请求都真的问节点一次**：不缓存是契约硬要求（日志只存在节点上，
//     中枢的任何副本都只会更旧）；
//  4. **归属**：任务不存在 / 跨命名空间都是 404，且**不去问节点**（不为看不见的
//     任务消耗一次帧往返）；
//  5. `task.NodeID == nil` → no_local_archive（不是 node_offline：那次执行没有节点，
//     报「节点掉线」会把用户引向网络排查）；
//  6. 上传端点已经删掉：mux 的源码里不许再出现 `node/tasks/{taskId}/logs`。

// ---------- 夹具 ----------

// fakeLogsSource 是 taskLogsSource 的替身。
//
// 用它而不是起真 WS 连接：五条取数分支（命中 / 离线 / 没档案 / 超时 / 其它）
// 里有三条要等 5 秒超时或者要真的断一条连接，用真 hub 跑会又慢又不稳；
// 而端点的职责只是「把 hub 的结果翻译成 HTTP 形状」，翻译逻辑才是这里的被测对象。
type fakeLogsSource struct {
	// frame / err 是默认答案（reply 为 nil 时使用）。
	frame *ws.LogsResponseFrame
	err   error
	// reply 非 nil 时按调用次序逐个产出答案（用于验「每次都真去取、不缓存」）。
	reply func(call int, namespaceID, nodeID, taskID string) (*ws.LogsResponseFrame, error)
	// calls 是取数次数：判「每次请求都调一次」的计数器。
	calls int
	// gotNS / gotNode / gotTask 是**最近一次**调用的入参（判隔离维度传对了没有）。
	gotNS   string
	gotNode string
	gotTask string
}

func (f *fakeLogsSource) FetchLogs(
	ctx context.Context, namespaceID, nodeID, taskID string,
) (*ws.LogsResponseFrame, error) {
	f.calls++
	f.gotNS, f.gotNode, f.gotTask = namespaceID, nodeID, taskID
	if f.reply != nil {
		return f.reply(f.calls, namespaceID, nodeID, taskID)
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.frame, nil
}

// logsEnv 是端点的测试环境：newConsoleEnv 的真库 / 真会话 / 真命名空间
// + 一个可以是任意 fake（或 nil）的取数源。
type logsEnv struct {
	*consoleEnv
	h   *consoleHandler
	mux http.Handler
}

// newLogsEnv 建环境并把 src 装进 consoleHandler。
//
// 这里自己搭一个 mux 而不是复用 consoleEnv.mux：那个 mux 里的 consoleHandler 是
// 别人建的（taskLogs 恒为 nil），塞不进 fake。路由 pattern 与生产 mux **逐字相同**
// （`GET /v1/tasks/{taskId}/logs`）——PathValue("taskId") 只有经过 mux 才有值，
// 单测走别的形状等于测一条不存在的路由。
func newLogsEnv(t *testing.T, src taskLogsSource) *logsEnv {
	t.Helper()
	env := newConsoleEnv(t)
	h := &consoleHandler{db: env.db, auth: env.svc, taskLogs: src}
	mux := http.NewServeMux()
	mux.Handle("GET /v1/tasks/{taskId}/logs",
		noStore(env.svc.Sessions().Middleware(http.HandlerFunc(h.serveTaskLogs))))
	return &logsEnv{consoleEnv: env, h: h, mux: mux}
}

// get 发一次读请求（带会话 Cookie），走上面那个与生产同形的 mux。
func (e *logsEnv) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(e.cookie)
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	return rr
}

// logsPath 是这条任务的读日志路径（带默认命名空间）。
func (e *logsEnv) logsPath(taskID string) string {
	return e.withNS("/v1/tasks/" + taskID + "/logs")
}

// seedNode 在本环境的默认命名空间里建一台节点，返回它的 id。
func (e *logsEnv) seedNode(t *testing.T, name string) string {
	t.Helper()
	n := &store.Node{NamespaceID: e.nsID, Name: name}
	if _, err := e.db.CreateNode(context.Background(), n); err != nil {
		t.Fatalf("create node %s: %v", name, err)
	}
	return n.ID
}

// seedTask 在指定命名空间建一条任务；nodeID 非空时把任务的 node_id 钉成它
// （端点就是用这一列决定「去问哪台节点」的）。
func (e *consoleEnv) seedTask(t *testing.T, nsID, traceID, capName, nodeID string) *store.Task {
	t.Helper()
	capDef := e.seedCapabilityIn(t, nsID, capName, 1, []string{"fix"})
	task := &store.Task{
		NamespaceID: nsID, TraceID: traceID, CapabilityID: capDef.ID,
		Capability: capDef.Name, Version: 1, Params: json.RawMessage(`{}`),
		CapHash: capDef.ContentHash, GraphID: "grf_x",
	}
	if nodeID != "" {
		id := nodeID
		task.NodeID = &id
	}
	if err := e.db.CreateTasks(context.Background(), []*store.Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task
}

// logsPayload 是响应的结构化视图。
//
// available / reason 用指针：要判的是「字段在不在」，不是零值——`available:false`
// 与「没有 available 字段」对前端是两件事（后者会让「没取到」看起来像「取到了但为空」）。
type logsPayload struct {
	Stdout    string         `json:"stdout"`
	Stderr    string         `json:"stderr"`
	Truncated bool           `json:"truncated"`
	Bytes     int64          `json:"bytes"`
	Available *bool          `json:"available"`
	Reason    *string        `json:"reason"`
	raw       map[string]any `json:"-"`
}

// decodeLogs 解出响应，并把原始 map 留在 raw 里（判字段集合用）。
func decodeLogs(t *testing.T, rr *httptest.ResponseRecorder) logsPayload {
	t.Helper()
	var p logsPayload
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode 响应 %q: %v", rr.Body.String(), err)
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &p.raw); err != nil {
		t.Fatalf("decode 响应（map）%q: %v", rr.Body.String(), err)
	}
	if p.Available == nil {
		t.Fatalf("响应缺 available 字段（前端按它分支）：%s", rr.Body.String())
	}
	return p
}

// 一段「真实形状」的日志：中文 + 空行 + 截断标记 + 尾部换行。
// 任何一个 trim / 转义 / 换行归一都会让下面的逐字断言红。
const (
	logsStdout = "第一步\n\n第二步：跑测试 ✅\n\n…（日志已截断：共 1048576 字节，只保留最后 524288 字节）\n"
	logsStderr = "flag provided but not defined: -to\n用法：anotify-agent emit --to <能力>\n\n"
)

// wire 造一帧「节点回了日志」的响应（字段名与 ws.LogsResponseFrame 一致）。
func wire(stdout, stderr string, truncated bool, bytes int64) *ws.LogsResponseFrame {
	return &ws.LogsResponseFrame{
		Type:      ws.NodeFrameLogsResponse,
		TaskID:    "tsk_x",
		RequestID: "lreq_x",
		Stdout:    stdout,
		Stderr:    stderr,
		Truncated: truncated,
		Bytes:     bytes,
	}
}

// ---------- 命中 ----------

// TestTaskLogsFetchesFromNode 命中：四个字段逐字透传 + available:true + 没有 reason。
func TestTaskLogsFetchesFromNode(t *testing.T) {
	src := &fakeLogsSource{frame: wire(logsStdout, logsStderr, true, 1048576)}
	e := newLogsEnv(t, src)
	nodeID := e.seedNode(t, "node-a")
	task := e.seedTask(t, e.nsID, "trace_logs", "runner", nodeID)

	rr := e.get(t, e.logsPath(task.ID))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
	}
	got := decodeLogs(t, rr)

	if got.Stdout != logsStdout {
		t.Errorf("stdout 不逐字相等（不能 trim / 转义 / 丢空行）:\n got=%q\nwant=%q", got.Stdout, logsStdout)
	}
	if got.Stderr != logsStderr {
		t.Errorf("stderr 不逐字相等:\n got=%q\nwant=%q", got.Stderr, logsStderr)
	}
	if !got.Truncated {
		t.Error("truncated 丢了（节点侧的事实必须原样透传）")
	}
	if got.Bytes != 1048576 {
		t.Errorf("bytes = %d, want 1048576（截断前的真实字节数）", got.Bytes)
	}
	if !*got.Available {
		t.Errorf("available = false, want true（真的取到了）: %s", rr.Body.String())
	}
	// reason 只在 available:false 时出现（omitempty）：成功时它必须**不在**，
	// 否则前端会同时看到「取到了」和「为什么没取到」。
	if _, ok := got.raw["reason"]; ok {
		t.Errorf("成功响应不许带 reason: %s", rr.Body.String())
	}
	if len(got.raw) != 5 {
		t.Errorf("响应字段数 = %d, want 5（stdout/stderr/truncated/bytes/available）: %s",
			len(got.raw), rr.Body.String())
	}

	// 取数维度必须对：命名空间取请求作用域、节点取任务那一列、任务就是路径里那个。
	// 传错任何一个都会在真机上变成「日志串台」或者「永远没档案」。
	if src.calls != 1 {
		t.Fatalf("fetch 调用次数 = %d, want 1", src.calls)
	}
	if src.gotNS != e.nsID || src.gotNode != nodeID || src.gotTask != task.ID {
		t.Fatalf("取数入参 = (%q, %q, %q), want (%q, %q, %q)",
			src.gotNS, src.gotNode, src.gotTask, e.nsID, nodeID, task.ID)
	}
}

// TestTaskLogsZeroBytesIsStillAvailable 0 字节的档案照样是 available:true。
//
// 「没取到」与「取到了但什么都没输出」都是 bytes=0：必须靠 available 区分，
// 不许拿 bytes 当判据（否则一次静默成功的执行会显示成「节点离线」）。
func TestTaskLogsZeroBytesIsStillAvailable(t *testing.T) {
	src := &fakeLogsSource{frame: wire("", "", false, 0)}
	e := newLogsEnv(t, src)
	task := e.seedTask(t, e.nsID, "trace_empty", "runner", e.seedNode(t, "node-a"))

	rr := e.get(t, e.logsPath(task.ID))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
	}
	got := decodeLogs(t, rr)
	if !*got.Available {
		t.Errorf("available = false, want true（0 字节也是取到了）: %s", rr.Body.String())
	}
	if _, ok := got.raw["reason"]; ok {
		t.Errorf("取到了就不许有 reason: %s", rr.Body.String())
	}
	if got.Bytes != 0 || got.Stdout != "" || got.Stderr != "" || got.Truncated {
		t.Errorf("0 字节档案的四个字段必须是零值: %s", rr.Body.String())
	}
}

// ---------- 没取到：四种错误 + 未装配 ----------

// TestTaskLogsDegradedNever5xx 四种取数失败全部 → **200 + available:false + reason**。
//
// 「绝不 5xx」是契约 §2 的硬要求：前端只认 200 + available:false 的降级分支，
// 5xx 会让它在正常路径上处理错误，也会把「用户点开了一台离线节点的日志」记成服务故障。
// 所以每条用例都**显式**断言状态码是 200（顺带断言「不是 5xx」）。
func TestTaskLogsDegradedNever5xx(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		reason string
	}{
		{"节点离线", ws.ErrNodeOffline, "node_offline"},
		{"节点上没有档案", ws.ErrNoLocalArchive, "no_local_archive"},
		{"5 秒没等到回包", ws.ErrLogsTimeout, "timeout"},
		// 写帧失败（连接已断）/ 对端回了别的 error 串 / ctx 取消：都折成 timeout，
		// reason 是封闭集合，不新增取值（多一个值就多一份没人翻译的前端文案）。
		{"意外错误", errors.New("boom"), "timeout"},
		{"包装过的写帧失败", fmt.Errorf("send logs_request: %w", errors.New("broken pipe")), "timeout"},
		// 哨兵被 %w 包一层也必须认得出来（实现方将来加包装不能改变语义）。
		{"包装过的离线", fmt.Errorf("fetch logs: %w", ws.ErrNodeOffline), "node_offline"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := &fakeLogsSource{err: c.err}
			e := newLogsEnv(t, src)
			task := e.seedTask(t, e.nsID, "trace_degraded", "runner", e.seedNode(t, "node-a"))

			rr := e.get(t, e.logsPath(task.ID))
			if rr.Code >= 500 {
				t.Fatalf("status = %d，取不到日志绝不回 5xx（body=%s）", rr.Code, rr.Body.String())
			}
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 + available:false（body=%s）", rr.Code, rr.Body.String())
			}
			got := decodeLogs(t, rr)
			if *got.Available {
				t.Fatalf("available = true，取不到时必须是 false: %s", rr.Body.String())
			}
			if got.Reason == nil {
				t.Fatalf("available:false 必须带 reason: %s", rr.Body.String())
			}
			if *got.Reason != c.reason {
				t.Fatalf("reason = %q, want %q（封闭集合）", *got.Reason, c.reason)
			}
			// 没取到时四个数据字段必须是零值：前端会把它们直接渲染，
			// 带一半内容的「失败」比空更难解释。
			if got.Stdout != "" || got.Stderr != "" || got.Truncated || got.Bytes != 0 {
				t.Fatalf("没取到时数据字段必须全空: %s", rr.Body.String())
			}
			if src.calls != 1 {
				t.Fatalf("fetch 调用次数 = %d, want 1", src.calls)
			}
		})
	}
}

// TestTaskLogsSourceNotWired 未装配（taskLogs == nil）→ 200 + node_offline，绝不 panic / 5xx。
//
// 生产 mux 一定会注入 nodeHub；这条分支只为「测试/嵌入用法漏装」兜底——
// 它必须是 200（前端仍能正常降级），而不是 nil 解引用。
func TestTaskLogsSourceNotWired(t *testing.T) {
	e := newLogsEnv(t, nil)
	task := e.seedTask(t, e.nsID, "trace_unwired", "runner", e.seedNode(t, "node-a"))

	rr := e.get(t, e.logsPath(task.ID))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（未装配也要能降级，body=%s）", rr.Code, rr.Body.String())
	}
	got := decodeLogs(t, rr)
	if *got.Available {
		t.Fatalf("available = true，未装配时不可能取到: %s", rr.Body.String())
	}
	if got.Reason == nil || *got.Reason != "node_offline" {
		t.Fatalf("reason = %v, want node_offline（「现在拿不到、稍后再试」最接近事实）", got.Reason)
	}
}

// TestTaskLogsNilNodeIDIsNoLocalArchive 还没派给任何节点的任务 → no_local_archive。
//
// 这类任务的那次执行**根本不存在**，节点上也不可能有档案。报 node_offline 是错的：
// 那句话的含义是「节点掉了、连上再看」，而这条任务的节点永远不会「连上」——
// 用户会去查网络，真正的原因却是任务没跑起来。
func TestTaskLogsNilNodeIDIsNoLocalArchive(t *testing.T) {
	src := &fakeLogsSource{frame: wire("不该被取到", "", false, 9)}
	e := newLogsEnv(t, src)
	task := e.seedTask(t, e.nsID, "trace_nil_node", "runner", "") // node_id 留空
	if task.NodeID != nil {
		t.Fatalf("夹具错了：node_id 应当为 nil，实际 %q", *task.NodeID)
	}

	rr := e.get(t, e.logsPath(task.ID))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
	}
	got := decodeLogs(t, rr)
	if *got.Available {
		t.Fatalf("available = true，没有节点的任务不可能取到日志: %s", rr.Body.String())
	}
	if got.Reason == nil || *got.Reason != "no_local_archive" {
		t.Fatalf("reason = %v, want no_local_archive", got.Reason)
	}
	// 没有节点可问：一次请求都不该发（白跑一次帧往返）。
	if src.calls != 0 {
		t.Fatalf("fetch 调用次数 = %d, want 0（node_id 为空时无可问之处）", src.calls)
	}
}

// ---------- 归属：404 且不问节点 ----------

// TestTaskLogsNotFound 任务不存在 / 跨命名空间 → 404（与 /v1/tasks/{id} 同形）。
//
// 附带一条更关键的断言：**没有向节点取数**。若先取数再判归属，等于给了外部一个
// 「用任意 taskId 让中枢发帧」的放大器，也会把别人的日志读进本进程内存。
func TestTaskLogsNotFound(t *testing.T) {
	src := &fakeLogsSource{frame: wire("别人的日志", "", false, 5)}
	e := newLogsEnv(t, src)
	otherNS := e.seedNamespace(t, "other", "另一个命名空间")
	otherNode := e.seedNode(t, "node-a")
	foreignTask := e.seedTask(t, otherNS.ID, "trace_other_ns", "runner", otherNode)

	cases := []struct {
		name   string
		taskID string
	}{
		{"任务不存在", "tsk_doesnotexist"},
		{"任务在另一个命名空间", foreignTask.ID},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := src.calls
			rr := e.get(t, e.logsPath(c.taskID))
			if rr.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404（body=%s）", rr.Code, rr.Body.String())
			}
			// 与 /v1/tasks/{id} 同文案：调用方无法据此探测别的命名空间里有没有这个 id。
			if !strings.Contains(rr.Body.String(), "task not found") {
				t.Fatalf("body = %q, want task not found", rr.Body.String())
			}
			if src.calls != before {
				t.Fatalf("404 也去问节点了（calls %d → %d）", before, src.calls)
			}
		})
	}
}

// TestTaskLogsRequestGuards 请求形状本身的守卫（缺参 / 通配 / 无会话 / 错方法）。
func TestTaskLogsRequestGuards(t *testing.T) {
	e := newLogsEnv(t, &fakeLogsSource{frame: wire("x", "", false, 1)})
	task := e.seedTask(t, e.nsID, "trace_guard", "runner", e.seedNode(t, "node-a"))

	t.Run("缺 namespaceId → 400（绝不回退默认命名空间）", func(t *testing.T) {
		rr := e.get(t, "/v1/tasks/"+task.ID+"/logs")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400（body=%s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("namespaceId=* → 400（单对象端点不接受通配）", func(t *testing.T) {
		rr := e.get(t, "/v1/tasks/"+task.ID+"/logs?namespaceId=*")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400（body=%s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("没有会话 → 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, e.logsPath(task.ID), nil)
		rr := httptest.NewRecorder()
		e.mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401（body=%s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("POST → 405", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, e.logsPath(task.ID), nil)
		req.AddCookie(e.cookie)
		rr := httptest.NewRecorder()
		e.mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405（body=%s）", rr.Code, rr.Body.String())
		}
	})
}

// ---------- 不缓存：每次请求都真的问节点 ----------

// TestTaskLogsEveryRequestHitsNode 连点两次 → 两次都真的去取，且第二次拿到新内容。
//
// 不缓存是契约 §2 的硬要求：节点上的档案会被重跑覆盖、会被 GC 掉，中枢没有任何
// 可靠的失效信号——缓存一份错的日志比没有日志更危险（排障时会据此下结论）。
func TestTaskLogsEveryRequestHitsNode(t *testing.T) {
	src := &fakeLogsSource{
		reply: func(call int, _, _, _ string) (*ws.LogsResponseFrame, error) {
			return wire(fmt.Sprintf("第 %d 次执行\n", call), "", false, int64(call)), nil
		},
	}
	e := newLogsEnv(t, src)
	task := e.seedTask(t, e.nsID, "trace_nocache", "runner", e.seedNode(t, "node-a"))

	first := e.get(t, e.logsPath(task.ID))
	if first.Code != http.StatusOK {
		t.Fatalf("第一次 status = %d, want 200", first.Code)
	}
	if got := decodeLogs(t, first); got.Stdout != "第 1 次执行\n" {
		t.Fatalf("第一次 stdout = %q", got.Stdout)
	}

	second := e.get(t, e.logsPath(task.ID))
	if second.Code != http.StatusOK {
		t.Fatalf("第二次 status = %d, want 200", second.Code)
	}
	got := decodeLogs(t, second)
	if got.Stdout != "第 2 次执行\n" {
		t.Fatalf("第二次 stdout = %q，want 第 2 次执行（缓存了？）", got.Stdout)
	}
	if got.Bytes != 2 {
		t.Fatalf("第二次 bytes = %d, want 2（跟着新回包走）", got.Bytes)
	}
	if src.calls != 2 {
		t.Fatalf("fetch 调用次数 = %d, want 2（每次请求一次，不缓存）", src.calls)
	}
}

// ---------- 路由守卫：上传端点已删 ----------

// TestMuxHasNoLogsUploadRoute mux.go 的源码里不许再出现上传路径。
//
// 为什么读源码而不是发请求：`http.ServeMux` 对**未注册**的路径回的是 404/405，
// 与「注册了但 handler 判方法」在响应上无法区分；契约 §3 要的是**这条路由不存在**，
// 那是个静态事实，只能静态判（本仓库已有读源码做静态断言的先例）。
func TestMuxHasNoLogsUploadRoute(t *testing.T) {
	src, err := os.ReadFile("mux.go")
	if err != nil {
		t.Fatalf("读 mux.go: %v", err)
	}
	if strings.Contains(string(src), "node/tasks/{taskId}/logs") {
		t.Fatal("mux.go 里仍然注册着上传端点 POST /v1/node/tasks/{taskId}/logs" +
			"（CONTRACT-logs-on-node §3：日志只存在节点上，中枢不再接收上传）")
	}
	// 正对照：读端点必须还在（否则上面那条会因为「整个文件没日志」而假绿）。
	if !strings.Contains(string(src), "GET /v1/tasks/{taskId}/logs") {
		t.Fatal("mux.go 里找不到读端点 GET /v1/tasks/{taskId}/logs")
	}
}
