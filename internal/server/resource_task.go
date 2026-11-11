package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/ws"
)

// 本文件是 v4 资源面的「执行事实读取域」：Task / Event / Trace 三个只读资源，
// 以及它们的三条子资源/读模型端点（logs、emissions、console events）。
//
// 分工与纪律（api-conventions §7.6 / §7.7、W6、A1/G3-D3）：
//
//   - Task 是**系统对象**：只有 dispatcher / ingress 建行，HTTP 面没有 POST / DELETE，
//     `spec` 不可变（没有 PATCH）。所以本文件不出现 decodeWriteEnvelope / CAS 令牌：
//     没有可写面就没有并发控制问题。`metadata.resourceVersion` **刻意不下发**——
//     tasks 表没有 resource_version 列，编一个假令牌会让客户端以为可以拿它做 CAS。
//   - status **从事实派生**（S2）：全部来自 tasks 行的投影，不落库、不需要 reconciler。
//     没事实的字段一律 null / 省略（S1/E1：未声明 version 不公开 0，不造 digest）。
//   - 日志的真相在**节点**上（A1/G3-D3）：中枢只转发一次 logs_request，不落库、不缓存。
//     取不到不是「服务出错」，但**也绝不伪造空成功**：唯一允许的空结果是
//     `previous=true` 且没有上一次执行（200 / attempt 0 / `lines: []`）；
//     「没有档案」（任务还没 grant 或节点回 no_local_archive）→ 410；
//     节点离线 → 503；超时 → 504；其它 → 502。
//   - emissions 是**任务子资源**：一次 emit 调用一条事实（不是端口 / 自动业务 seq）。
//     默认最新在前，`limit` + 不透明 `continue`（绑 ns + taskId）。
//   - console events 是唯一的跨命名空间汇总读：`namespace=*` 先过登录，再逐 ns 查，
//     合并后仍按 (created_at desc, id desc) 排；cursor 绑 "*" + 筛选条件。

// ---------- 路由 ----------

// taskAPI 是 Task / Event / Trace / logs / emissions / console events 的路由集合。
type taskAPI struct{ d *serverDeps }

// newTaskAPI 绑定共享依赖。
func newTaskAPI(d *serverDeps) *taskAPI { return &taskAPI{d: d} }

const (
	// tasksPath 是命名空间内的任务集合根（`{ns}` 由 ServeMux 通配捕获）。
	tasksPath = "/v1/namespaces/{ns}/tasks"
	// nsEventsPath 是命名空间内的事件流（资源列表，非投递入口）。
	nsEventsPath = "/v1/namespaces/{ns}/events"
	// nsTracePath 是一条 trace 的全景读模型（只读、不能 list）。
	nsTracePath = "/v1/namespaces/{ns}/traces/{traceId}"
	// consoleEventsPath 是控制台事件流（cluster 级读模型，`?namespace=` 唯一来源）。
	consoleEventsPath = "/v1/console/events"

	// taskListDefaultLimit 与 store 的列表口径一致：limit <= 0 用 50。
	taskListDefaultLimit = 50
	// taskListLimitCap 是列表的硬上限（store 对 > 500 的处理是折回 50，不是钳到 500，
	// 所以两个明细集合在 handler 侧显式钳到 500：要多少给多少，不悄悄换一个窗口）。
	taskListLimitCap = 500
	// taskLogsTimeout 是等一帧 logs_response 的上限（有界：节点可能永远不回）。
	taskLogsTimeout = 10 * time.Second
	// taskCancelGraceSeconds 是 task.cancel 帧里 TERM → KILL 的宽限（与节点侧同一常量口径）。
	taskCancelGraceSeconds = 5
)

// register 挂上本域的全部路由。
//
// 全部过会话中间件（未登录 401），并且 no-store：这些都是实时事实，缓存一帧就会
// 让「我刚取消的任务」在页面上继续显示 running。
func (a *taskAPI) register(mux *http.ServeMux) {
	if mux == nil || a == nil || a.d == nil {
		return
	}
	route := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, noStore(a.d.session(h)))
	}
	route("GET "+tasksPath, a.list)
	route("GET "+tasksPath+"/{taskId}", a.get)
	route("POST "+tasksPath+"/{taskId}/cancel", a.cancel)
	route("POST "+tasksPath+"/{taskId}/abandon", a.abandon)
	route("GET "+tasksPath+"/{taskId}/logs", a.logs)
	route("GET "+tasksPath+"/{taskId}/emissions", a.emissions)
	// 运行产物下载（#332 / A1 §4）：Task 子资源，仍走会话鉴权 + no-store；
	// 授权与引用判定在 handler 内（store.GetTaskArtifactDownload 只认当前
	// reported result 的 ResultRef），字节只从 deps.Spool 读，绝不 fallback 宿主文件。
	route("GET "+tasksPath+"/{taskId}/artifacts/{sha256}", newArtifactDownloadAPI(a.d).get)
	route("GET "+nsEventsPath, a.events)
	route("GET "+nsTracePath, a.trace)
	route("GET "+consoleEventsPath, a.consoleEvents)
}

// ---------- GET …/tasks（List）----------

// list 列出命名空间内的任务（最新在前，OFFSET 分页无 cursor：store.ListTasks 的既有口径）。
//
// 过滤参数是显式白名单：`status` / `traceId` / `nodeId`（v3 console 与 §3.3 的现有参数名）。
// 未知参数一律 400 `unknown_field`：静默忽略会让 `?phase=running` 这类旧写法拿到
// **未过滤的全量列表**（客户端以为筛过了，最危险的一种错误成功）。
func (a *taskAPI) list(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	q := r.URL.Query()
	if err := taskListUnknownParams(q); err != nil {
		writeAPIError(w, err)
		return
	}
	limit, err := taskQueryInt(r, "limit", 0)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	rows, err := a.d.DB.ListTasks(r.Context(), ns, store.TaskFilter{
		Phase:   strings.TrimSpace(q.Get("status")),
		TraceID: strings.TrimSpace(q.Get("traceId")),
		NodeID:  strings.TrimSpace(q.Get("nodeId")),
		// store 的夹取是 <=0||>500 → 50；这里先算一次，行为与 store 逐字一致。
		Limit: taskListLimit(limit),
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	items := make([]apiEnvelope, 0, len(rows))
	for _, t := range rows {
		items = append(items, taskEnvelope(t, a.d.namespaceDisplay(r.Context(), ns)))
	}
	writeList(w, http.StatusOK, "TaskList", listMeta{}, items)
}

// ---------- GET …/tasks/{taskId} ----------

// get 读单个任务信封。
//
// 跨命名空间与「不存在」逐字节相同（404）：调用方无法据此探测别的 ns 里有没有这个 id。
func (a *taskAPI) get(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	id, err := taskPathID(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	task, err := a.d.DB.GetTask(r.Context(), ns, id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeEnvelope(w, http.StatusOK, taskEnvelope(task, a.d.namespaceDisplay(r.Context(), ns)))
}

// ---------- POST …/tasks/{taskId}/cancel · /abandon ----------

// cancel 记录取消意图（reason 缺省 = cancelled_by_user）。
func (a *taskAPI) cancel(w http.ResponseWriter, r *http.Request) {
	a.requestStop(w, r, "")
}

// abandon 记录放弃意图（reason 缺省 = abandoned_by_user；与 cancel 同一套状态机）。
func (a *taskAPI) abandon(w http.ResponseWriter, r *http.Request) {
	a.requestStop(w, r, protocol.ReasonAbandonedByUser)
}

// requestStop 是 cancel / abandon 的唯一实现。
//
// 顺序与「谁权威」是这里唯一的难点：
//
//  1. 先 GetTask(ns, id)：任务 id 全局唯一，但**可见性**必须在本次请求的命名空间里成立。
//     store.RequestCancel 只按 taskID 写，不带 ns —— 少了这一步就能拿别的 ns 的 id 取消别人的任务。
//  2. 意图落库走 store.RequestCancel（唯一线性化点：pending 直接终态、running 只记意图等 ack）。
//  3. running 的任务才需要「下发」：把 task.cancel 推给执行它的节点。这是**尽力**动作——
//     意图已经落库，节点重连后会按 cancel_reason 补 ack；推送失败不改变已完成的事实，
//     所以只记一条 WARN，不把请求判成失败（那会让调用方以为取消没生效而重复点）。
//
// 不用 hub.CancelTask 作为主路径的原因：它内部已经包了一次 RequestCancel，先写一次再调它
// 会在 pending 上撞到 ErrInvalidTransition（第一步已把任务推到终态）。running 时它是幂等的
// （cancel_requested_at 已存在 → 直接返回 nil 并推帧），所以只有 running 才调它。
func (a *taskAPI) requestStop(w http.ResponseWriter, r *http.Request, defaultReason string) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	id, err := taskPathID(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	reason, err := taskActionReason(r, defaultReason)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	task, err := a.d.DB.GetTask(r.Context(), ns, id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := a.d.DB.RequestCancel(r.Context(), id, reason, taskNowSeconds(a.d)); err != nil {
		if errors.Is(err, store.ErrInvalidTransition) {
			// 终态任务不能再停：这是调用方可理解的冲突，不是服务端错误。
			writeAPIError(w, errStatus(http.StatusConflict, reasonConflict,
				fmt.Sprintf("task %s is already %s", id, task.Phase)))
			return
		}
		writeAPIError(w, err)
		return
	}
	if task.Phase == store.TaskRunning && a.d.Hub != nil {
		if herr := a.d.Hub.CancelTask(r.Context(), ns, id, reason, taskCancelGraceSeconds); herr != nil {
			a.d.logger().Warn("task stop frame not delivered",
				"event", "task.stop.frame_failed",
				"namespace", ns, "task_id", id, "error", herr.Error())
		}
	}
	updated, gerr := a.d.DB.GetTask(r.Context(), ns, id)
	if gerr != nil {
		updated = task
	}
	writeEnvelope(w, http.StatusOK, taskEnvelope(updated, a.d.namespaceDisplay(r.Context(), ns)))
}

// ---------- GET …/tasks/{taskId}/logs ----------

// logs 向执行过该 attempt 的节点取一次历史日志（A1/G3-D3）。
//
// 参数域是封闭的：tailLines ∈ [0, LogsTailLinesMax]、sinceSeconds >= 0、previous ∈ {true,false}。
// 越界一律 400 —— 把越界值归一成「0 = 不限」会让中枢发出的请求与它记录的成功无法一一对应。
func (a *taskAPI) logs(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	id, err := taskPathID(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	tailLines, err := taskQueryInt(r, "tailLines", 0)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if tailLines < 0 || tailLines > protocol.LogsTailLinesMax {
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonBadRequest,
			fmt.Sprintf("tailLines must be within 0..%d", protocol.LogsTailLinesMax)))
		return
	}
	sinceSeconds, err := taskQueryInt(r, "sinceSeconds", 0)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if sinceSeconds < 0 {
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonBadRequest, "sinceSeconds must be >= 0"))
		return
	}
	previous := taskQueryBool(r, "previous")

	if a.d.Hub == nil {
		// 装配缺失是部署错误：不能回 200 空 lines（那会把「没装配」伪装成「这次什么都没打印」）。
		writeAPIError(w, errStatus(http.StatusBadGateway, reasonInternal, "node logs channel is not wired"))
		return
	}

	// attempt=0 = 任务当前 attempt；previous=true 时由 hub 按 task_attempts 历史定位上一次执行
	// （没有上一次 → LogsResult.Empty 的真空结果，不是错误）。
	res, rerr := a.d.Hub.RequestLogs(r.Context(), ns, id, 0, previous, tailLines, sinceSeconds, taskLogsTimeout)
	if rerr != nil {
		switch {
		case errors.Is(rerr, store.ErrNotFound), errors.Is(rerr, store.ErrNamespaceNotFound):
			writeAPIError(w, errStatus(http.StatusNotFound, reasonNotFound, "task not found"))
		case errors.Is(rerr, ws.ErrNoAttempt):
			// 任务还没 grant / 历史 attempt 行不存在：**没有档案**。
			// G3-D3 只允许「previous 指定了一次不存在的上一次」回 200 空；这里不是，
			// 所以 410 + 封闭 reason，绝不把「没有档案」伪装成「这次没输出」。
			writeLogsGone(w, id)
		case errors.Is(rerr, ws.ErrNodeOffline):
			writeAPIError(w, errStatus(http.StatusServiceUnavailable, "node_offline",
				"the node that ran this attempt is offline"))
		case errors.Is(rerr, ws.ErrLogsTimeout),
			errors.Is(rerr, context.DeadlineExceeded), errors.Is(rerr, context.Canceled):
			writeAPIError(w, errStatus(http.StatusGatewayTimeout, protocol.ReasonTimeout,
				"the node did not answer the logs request in time"))
		default:
			writeAPIError(w, errStatus(http.StatusBadGateway, reasonInternal,
				fmt.Sprintf("logs request failed: %v", rerr)))
		}
		return
	}
	if res == nil {
		// 契约里没有 (nil, nil) 这个形状：按上游故障处理，不当成「取到了空日志」。
		writeAPIError(w, errStatus(http.StatusBadGateway, reasonInternal, "logs request returned no result"))
		return
	}
	if res.Empty || res.Frame == nil {
		// previous=true 但没有上一次执行：唯一允许「200 + attempt 0 + 空 lines」的无档案情形。
		writeLogsEmpty(w, id)
		return
	}
	if ferr := strings.TrimSpace(res.Frame.Error); ferr != "" {
		if ferr == protocol.LogsErrNoLocalArchive {
			// 节点在线、档案不在（被 GC / 不是这台机器跑的）：**没有档案**。
			// G3-D3 明确 noarchive410 —— 不能缺档案假空成功（那会让「日志丢了」
			// 与「脚本什么都没打印」在页面上长得一样）。
			writeLogsGone(w, id)
			return
		}
		// 其它节点侧错误（logs_unreadable / illegal_log_request / 未知码）**不伪造空成功**：
		// 502 + 节点给的封闭码，调用方能据 reason 分支。
		writeAPIError(w, errStatus(http.StatusBadGateway, ferr,
			"the node could not read this attempt's log archive"))
		return
	}
	lines := res.Frame.LogLines()
	if lines == nil {
		lines = []protocol.LogLine{}
	}
	writeJSON(w, http.StatusOK, taskLogsDoc{
		TaskID:    id,
		Attempt:   res.Frame.Attempt,
		Lines:     lines,
		Truncated: res.Frame.Truncated,
	})
}

// writeLogsEmpty 写 G3-D3 允许的**唯一**空结果：previous 指定了不存在的上一次执行
// （200 + attempt 0 + lines [] + truncated false）。
func writeLogsEmpty(w http.ResponseWriter, taskID string) {
	writeJSON(w, http.StatusOK, taskLogsDoc{TaskID: taskID, Lines: []protocol.LogLine{}})
}

// writeLogsGone 写「没有档案」的 410（G3-D3 的 noarchive410）：任务还没 grant、
// 历史 attempt 行不存在，或节点回 no_local_archive。reason 用节点侧的同一个封闭码，
// 调用方无需区分是中枢没档案还是节点没档案——两者对「这次日志取不到」是同一个结论。
func writeLogsGone(w http.ResponseWriter, taskID string) {
	writeAPIError(w, errStatus(http.StatusGone, protocol.LogsErrNoLocalArchive,
		fmt.Sprintf("no local log archive for task %s", taskID)))
}

// ---------- GET …/tasks/{taskId}/emissions ----------

// emissions 列出某任务的全部 emit 事实（默认最新在前，limit + 不透明 continue）。
//
// 形状是 §7.6 的例外文档（不是资源信封）：{metadata:{continue}, items:[…]}。
// 每条 item 的 eventId / to / version / taskId / pruned 全部**从落库回执**（rec.Result）
// 解析；解析不出就给 null / 省略，不编造（回执是首次派发那一刻的持久事实）。
func (a *taskAPI) emissions(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	id, err := taskPathID(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	// 任务行必须先在本命名空间可见（子资源也是命名空间资源；跨 ns 一律 404）。
	task, err := a.d.DB.GetTask(r.Context(), ns, id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	limit, err := taskQueryInt(r, "limit", 0)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if limit <= 0 {
		limit = taskListDefaultLimit
	}
	if limit > taskListLimitCap {
		limit = taskListLimitCap
	}
	rows, err := a.d.DB.ListEmits(r.Context(), task.ID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	binding := emissionCursorBinding(ns, task.ID)
	afterID, afterAt, err := decodeCursor(r.URL.Query().Get("continue"), binding)
	if err != nil {
		// 坏令牌 / 别的任务（或别的 ns）的令牌：400，不做「尽力解释」（会读越权记录）。
		writeAPIError(w, err)
		return
	}

	// store 给的是 (created_at, id) 升序；翻成「最新在前」后按 same-key 语义跳过游标之前的行。
	page := make([]*store.EmitRecord, 0, limit+1)
	for i := len(rows) - 1; i >= 0 && len(page) < limit+1; i-- {
		rec := rows[i]
		if afterID != "" && !taskEmitOlderThan(rec, afterAt, afterID) {
			continue
		}
		page = append(page, rec)
	}
	more := len(page) > limit
	if more {
		page = page[:limit]
	}
	items := make([]taskEmissionItem, 0, len(page))
	for _, rec := range page {
		items = append(items, taskEmissionItemOf(rec))
	}
	meta := listMeta{}
	if more && len(page) > 0 {
		last := page[len(page)-1]
		meta.Continue = encodeCursor(binding, last.ID, last.CreatedAt)
	}
	writeJSON(w, http.StatusOK, taskEmissionsList{Metadata: meta, Items: items})
}

// taskEmitOlderThan 报告 rec 是否严格旧于游标位置（与 store 的 (created_at, id) 序一致）。
func taskEmitOlderThan(rec *store.EmitRecord, at int64, id string) bool {
	if rec.CreatedAt != at {
		return rec.CreatedAt < at
	}
	return rec.ID < id
}

// ---------- GET …/events ----------

// events 是命名空间内的事件流（资源列表，只读）。
//
// source 过滤规则**不在 handler 里重写**（store.ListEvents 已经收口）：
//   - 按 trace 查：含剪枝留痕（那条 trace 的「为什么没跑」就在里面）；
//   - 事件流（无 trace、无 source）：不含剪枝（source='route'）；
//   - 显式给 source（包括 route）：听调用方的。
func (a *taskAPI) events(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	q := r.URL.Query()
	limit, err := taskQueryInt(r, "limit", 0)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	// 契约 `api-conventions.md:167`：资源列表沿用现有 limit（默认/上限不变）。
	// store.ListEvents 对 >500 的处理是**折回 50**（一个静默的不同窗口），所以 handler
	// 先钳到 500：要 501 条就给你 500 条，不悄悄换成 50（#236 的守卫必须留住）。
	limit = taskClampListLimit(limit)
	rows, err := a.d.DB.ListEvents(r.Context(), ns,
		strings.TrimSpace(q.Get("trace")), strings.TrimSpace(q.Get("source")), limit)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	items := make([]apiEnvelope, 0, len(rows))
	for _, e := range rows {
		// metadata.namespace 是**对象自身**的命名空间（§3.2）：逐条解析，不拿路径/查询的
		// ns 去覆盖它（`*` 汇总时那是唯一能归属每条事件的信息）。
		items = append(items, eventEnvelope(e, a.d.namespaceDisplay(r.Context(), e.NamespaceID)))
	}
	writeList(w, http.StatusOK, "EventList", listMeta{}, items)
}

// ---------- GET …/traces/{traceId} ----------

// trace 返回一条 trace 的全景（§7.7：{events[], tasks[], pruned[]}）。
//
// 三份事实各自的真相来源：
//   - events：store.ListEvents（带 traceId 时**含**剪枝行，过滤规则在 store 层）；
//   - tasks：store.ListTasksByTrace（建行顺序，时间线按它走）；
//   - pruned：events 里 source='route' 的那些行（中枢自己的剪枝观察），原样给事件信封。
//
// 两个信号都空 = 这条 trace 在本命名空间不存在 → 404（不区分「没事件」与「不存在」：
// trace_id 只由事件/任务产生，两者都没有就是没有）。
func (a *taskAPI) trace(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	traceID := strings.TrimSpace(r.PathValue("traceId"))
	if traceID == "" {
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonBadRequest, "traceId is required"))
		return
	}
	_, perr := a.d.DB.GetEventByTrace(r.Context(), ns, traceID)
	if perr != nil && !errors.Is(perr, store.ErrNotFound) {
		writeAPIError(w, perr)
		return
	}
	tasks, err := a.d.DB.ListTasksByTrace(r.Context(), ns, traceID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if errors.Is(perr, store.ErrNotFound) && len(tasks) == 0 {
		writeAPIError(w, errStatus(http.StatusNotFound, reasonNotFound, "trace not found"))
		return
	}
	rows, err := a.d.DB.ListEvents(r.Context(), ns, traceID, "", taskListLimitCap)
	if err != nil {
		writeAPIError(w, err)
		return
	}

	view := taskTraceView{
		Events: make([]apiEnvelope, 0, len(rows)),
		Tasks:  make([]apiEnvelope, 0, len(tasks)),
		Pruned: make([]apiEnvelope, 0, len(rows)),
	}
	for _, e := range rows {
		env := eventEnvelope(e, a.d.namespaceDisplay(r.Context(), e.NamespaceID))
		view.Events = append(view.Events, env)
		// pruned[] 只收**纯剪枝**（事件收到了但没派发）。未解析派发失败
		// （downstream_missing）是 failed Task + 观察事件，不是剪枝（capability.md §6.2）。
		if e.Source == store.EventPruneSource && !eventIsDispatchFailure(e) {
			view.Pruned = append(view.Pruned, env)
		}
	}
	for _, t := range tasks {
		view.Tasks = append(view.Tasks, taskEnvelope(t, a.d.namespaceDisplay(r.Context(), ns)))
	}
	writeJSON(w, http.StatusOK, view)
}

// ---------- GET /v1/console/events ----------

// consoleEvents 是控制台事件流：`?namespace=NAME`（或 `*` 跨 ns 汇总）+ limit + 不透明 continue。
//
// 分页是 **keyset**（不是 OFFSET）：后续页走 store.ListEventsBefore，同一个
// (created_at, id) 游标下每页窗口互不重叠——事件同秒批量落库是常态，只按时间比较
// 会在页边界丢行或重复行。
//
// cursor 绑 ns + 筛选条件（source / traceId）：拿另一个 ns 的 cursor 读越权记录必须 400，
// 而不是被当成「第一页」静默回退（那会让翻页莫名跳回最新）。
func (a *taskAPI) consoleEvents(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizeQueryNamespace(r, true)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	q := r.URL.Query()
	source := strings.TrimSpace(q.Get("source"))
	traceID := strings.TrimSpace(q.Get("traceId"))
	limit, err := taskQueryInt(r, "limit", 0)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if limit <= 0 {
		limit = taskListDefaultLimit
	}
	if limit > taskListLimitCap {
		limit = taskListLimitCap
	}
	binding := eventCursorBinding(ns, source, traceID)
	beforeID, beforeAt, err := decodeCursor(q.Get("continue"), binding)
	if err != nil {
		writeAPIError(w, err)
		return
	}

	nsIDs := []string{ns}
	if ns == store.AllNamespaces {
		all, lerr := a.d.DB.ListNamespaces(r.Context())
		if lerr != nil {
			writeAPIError(w, lerr)
			return
		}
		nsIDs = make([]string, 0, len(all))
		for _, item := range all {
			nsIDs = append(nsIDs, item.ID)
		}
	}
	// 「多看一行」：多取一行只用于**判定**还有没有更旧的行，不发给客户端。
	// 但它必须 <= 500（store 对 > 500 是折回 50，取不到多出来的那行），因此 500 时退化。
	fetch := limit
	if fetch < taskListLimitCap {
		fetch++
	}

	groups := make([][]*store.Event, 0, len(nsIDs))
	for _, nsID := range nsIDs {
		var got []*store.Event
		var gerr error
		if beforeID != "" {
			got, gerr = a.d.DB.ListEventsBefore(r.Context(), nsID, traceID, source, beforeAt, beforeID, fetch)
		} else {
			got, gerr = a.d.DB.ListEvents(r.Context(), nsID, traceID, source, fetch)
		}
		if gerr != nil {
			writeAPIError(w, gerr)
			return
		}
		groups = append(groups, got)
	}
	merged := mergeTaskEvents(groups...)
	more := len(merged) > limit
	if !more && limit >= taskListLimitCap {
		more = len(merged) >= limit
	}
	if len(merged) > limit {
		merged = merged[:limit]
	}
	meta := listMeta{}
	if more && len(merged) > 0 {
		last := merged[len(merged)-1]
		meta.Continue = encodeCursor(binding, last.ID, last.CreatedAt)
	}
	nsDisplay := a.d.namespaceDisplay(r.Context(), ns)
	items := make([]apiEnvelope, 0, len(merged))
	for _, e := range merged {
		// 逐条回显事件**自己**的命名空间（§3.2）：`*` 汇总下如果每行都写 "*"，
		// 客户端就无法把事件归属回命名空间——而这正是跨 ns 视图唯一多出来的信息。
		// 单 ns 视图下 namespaceDisplay(e.NamespaceID) == nsDisplay（已被 display 缓存）。
		display := nsDisplay
		if ns == store.AllNamespaces {
			display = a.d.namespaceDisplay(r.Context(), e.NamespaceID)
		}
		items = append(items, eventEnvelope(e, display))
	}
	writeList(w, http.StatusOK, "EventList", meta, items)
}

// mergeTaskEvents 把多组事件按 store 的序（created_at desc, id desc）合并。
//
// 跨命名空间汇总必须用**同一份**排序判据：分组各自的顺序在合并后不再成立，
// 而 cursor 是从合并结果的最旧一行算出来的。
func mergeTaskEvents(groups ...[]*store.Event) []*store.Event {
	total := 0
	for _, g := range groups {
		total += len(g)
	}
	out := make([]*store.Event, 0, total)
	for _, g := range groups {
		out = append(out, g...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// ---------- wire 形状：Task（§7.6）----------

// taskSpecDoc 是 Task.spec（系统写、不可变；客户端不能创建 / 修改）。
//
// sequence / sourceRef **不在**这里：tasks 表没有这两个事实（谁触发的在 events 上）。
// 拿不到就省略，不给 null 占位——null 会让人以为「这条任务确实没有来源」。
type taskSpecDoc struct {
	TraceUID     string             `json:"traceUID"`
	ParentRef    *taskRefDoc        `json:"parentRef"`
	ParentTaskID *string            `json:"parentTaskId"`
	Capability   *taskCapabilityRef `json:"capability"`
	Params       json.RawMessage    `json:"params"`
}

// taskRefDoc 是父子引用（只给真实存在的 name；kind 恒 Task）。
type taskRefDoc struct {
	Kind string `json:"kind,omitempty"`
	Name string `json:"name"`
}

// taskCapabilityRef 是任务钉住的能力身份。
//
// version / digest **只在有真实事实时出现**（S1/E1）：未解析派发失败的任务没有解析出
// 能力行，就不该公开 version 0，更不该造一个 digest。
type taskCapabilityRef struct {
	Name    string `json:"name"`
	Version *int   `json:"version,omitempty"`
	Digest  string `json:"digest,omitempty"`
}

// taskStatusDoc 是 Task.status（全部从 tasks 行派生，S2）。
//
// configRevision / namespaceVarsRevision 是**配置生效版本**（字符串，按 §7.6），
// 与 metadata.resourceVersion（写并发令牌）是两件事，不能混用——而 Task 没有后者。
type taskStatusDoc struct {
	Phase                 string              `json:"phase"`
	WaitingReason         *string             `json:"waitingReason"`
	NextAttemptAt         *string             `json:"nextAttemptAt"`
	NodeName              *string             `json:"nodeName"`
	Attempt               int                 `json:"attempt"`
	MaxAttempts           int                 `json:"maxAttempts"`
	LeaseExpiresAt        *string             `json:"leaseExpiresAt"`
	StartedAt             *string             `json:"startedAt"`
	FinishedAt            *string             `json:"finishedAt"`
	ConfigRevision        string              `json:"configRevision"`
	NamespaceVarsRevision string              `json:"namespaceVarsRevision"`
	Result                taskResultDoc       `json:"result"`
	EnvDiagnostics        []taskEnvDiagnostic `json:"envDiagnostics,omitempty"`
}

// taskEnvDiagnostic 是 C11 的只诊断投影：只有名字 + 来源层，没有解析后的值。
type taskEnvDiagnostic struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

// taskResultDoc 是 Task.status.result。artifacts 恒 `[]`（不是 null）。
type taskResultDoc struct {
	Summary   string                  `json:"summary"`
	Artifacts []protocol.TaskArtifact `json:"artifacts"`
	Error     *taskResultErrorDoc     `json:"error"`
}

// taskResultErrorDoc 是失败/放弃的机器码 + 人话（reason 来自封闭集合）。
type taskResultErrorDoc struct {
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

// taskEnvelope 组装 Task 信封。
func taskEnvelope(t *store.Task, nsDisplay string) apiEnvelope {
	if t == nil {
		return newEnvelope("Task", objectMeta{}, nil, nil)
	}
	if nsDisplay == "" {
		nsDisplay = t.NamespaceID
	}
	meta := objectMeta{
		Name:      t.ID,
		Namespace: nsDisplay,
		UID:       t.ID,
		// 刻意不给 ResourceVersion：tasks 没有 resource_version 列、Task 无 CAS。
		CreationTimestamp: taskTimestamp(t.CreatedAt),
	}
	return newEnvelope("Task", meta, taskSpecOf(t), taskStatusOf(t))
}

// taskSpecOf 从任务行投影 spec。
func taskSpecOf(t *store.Task) taskSpecDoc {
	params := t.Params
	if len(bytes.TrimSpace(params)) == 0 {
		params = json.RawMessage("{}")
	}
	doc := taskSpecDoc{TraceUID: t.TraceID, Params: params}
	if t.ParentTaskID != nil && *t.ParentTaskID != "" {
		parent := *t.ParentTaskID
		doc.ParentTaskID = &parent
		doc.ParentRef = &taskRefDoc{Kind: "Task", Name: parent}
	}
	// capability_id 为空 = 未解析派发失败（唯一允许没有能力行的形状）。
	//
	// 但 **name 必须保留**：S1/E1 要求失败 Task 保「真实请求 basename」，否则控制台与
	// CLI 只能看到一条「不知道想调什么」的失败任务。version / digest 才是没有事实的
	// 部分——未声明版本时省略（不公开哨兵 0），没有解析出 digest 时省略字符串。
	ref := &taskCapabilityRef{Name: t.Capability}
	if t.CapabilityID != "" {
		ref.Digest = t.Digest
		if t.Version > 0 {
			version := t.Version
			ref.Version = &version
		}
	} else if t.Version > 0 {
		// 请求显式声明了正版本（但未解析出能力行）：保留声明值，不造 digest。
		version := t.Version
		ref.Version = &version
	}
	if ref.Name != "" {
		doc.Capability = ref
	}
	return doc
}

// taskStatusOf 从任务行投影 status。
func taskStatusOf(t *store.Task) taskStatusDoc {
	artifacts := t.Artifacts
	if artifacts == nil {
		artifacts = []protocol.TaskArtifact{}
	}
	status := taskStatusDoc{
		Phase:                 t.Phase,
		WaitingReason:         taskOptionalString(t.WaitingReason),
		NextAttemptAt:         taskTimestampPtr(t.NextAttemptAt),
		NodeName:              taskOptionalString(t.NodeName),
		Attempt:               t.Attempt,
		MaxAttempts:           t.MaxAttempts,
		LeaseExpiresAt:        taskTimestampPtr(t.LeaseUntil),
		StartedAt:             taskTimestampPtr(t.StartedAt),
		FinishedAt:            taskTimestampPtr(t.FinishedAt),
		ConfigRevision:        strconv.FormatInt(t.ConfigRevision, 10),
		NamespaceVarsRevision: strconv.FormatInt(t.NamespaceVarsRevision, 10),
		Result: taskResultDoc{
			Summary:   t.Summary,
			Artifacts: artifacts,
			Error:     taskResultErrorOf(t),
		},
	}
	return status
}

// taskResultErrorOf 投影 status.result.error（没有原因事实时给 null，不编造 code）。
//
// 取值顺序：
//  1. 有 result 事实（result_json）→ 它的 Reason（封闭码）与 Error/Summary（人话）；
//  2. 没有 result 事实、但 error 列本身就是封闭码（未解析派发失败的 error 列就是原因码）
//     → 用它当 reason；
//  3. 其它终态行：error 列是**人话**（例如脚本输出），只当 message，不冒充 reason。
func taskResultErrorOf(t *store.Task) *taskResultErrorDoc {
	reason, message := taskResultReasonMessage(t)
	if reason == "" && message == "" {
		return nil
	}
	if reason == message {
		// 同一份事实不要重复两遍（例如 error 列既是原因码又是唯一说明）。
		message = ""
	}
	return &taskResultErrorDoc{Reason: reason, Message: message}
}

// taskResultReasonMessage 从任务行取 (reason, message)。
func taskResultReasonMessage(t *store.Task) (string, string) {
	if len(bytes.TrimSpace(t.ResultJSON)) > 0 {
		var res store.TaskResult
		if err := json.Unmarshal(t.ResultJSON, &res); err == nil {
			reason := strings.TrimSpace(res.Reason)
			message := strings.TrimSpace(res.Error)
			if message == "" {
				message = strings.TrimSpace(res.Summary)
			}
			if reason != "" || message != "" {
				return reason, message
			}
		}
	}
	errText := strings.TrimSpace(t.Error)
	if taskResultReasonCode(errText) {
		return errText, ""
	}
	if errText != "" && store.IsTerminalPhase(t.Phase) && t.Phase != store.TaskSucceeded {
		return "", errText
	}
	return "", ""
}

// taskResultReasonCodes 是「任务结果原因码」的封闭集合（capability.md §6.2）。
//
// 只用来判断 error 列里的字面是不是一个机器码：是 → 当 reason；不是 → 当人话（message）。
// 不用它校验任何输入（这里没有可写面）。
var taskResultReasonCodes = []string{
	protocol.ReasonTimeout,
	protocol.ReasonScriptFailed,
	protocol.ReasonFilesEscape,
	protocol.ReasonWorkDirUnavailable,
	protocol.ReasonHomeUnavailable,
	protocol.ReasonMissingRequirement,
	protocol.ReasonArtifactExportInvalid,
	protocol.ReasonMaxAttemptsExceeded,
	protocol.ReasonNodeLost,
	protocol.ReasonLeaseExpired,
	protocol.ReasonInfraTransient,
	protocol.ReasonDownstreamMissing,
	protocol.ReasonCancelledByUser,
	protocol.ReasonAbandonedByUser,
	protocol.ReasonStaleConfig,
	protocol.ReasonSlotUnavailable,
}

// taskResultReasonCode 报告 s 是否是任务结果原因码。
func taskResultReasonCode(s string) bool {
	if s == "" {
		return false
	}
	for _, code := range taskResultReasonCodes {
		if s == code {
			return true
		}
	}
	return false
}

// ---------- wire 形状：Event（§7.7）----------

// eventSpecDoc 是 Event.spec（只读、不可变）。
//
// `type`（Normal/Warning 严重度）与 `signature`（HMAC 验证结论）**刻意省略**：
// events 表没有这两列事实，编一个默认值等于把「没验证」说成「已验证」。
//
// reason 对**未解析派发失败**的观察事件也省略（见 eventIsDispatchFailure）：store 把
// 失败观察行统一写成 reason='Pruned'，照抄会把「任务失败」报告成「剪枝」。events 表
// 没有独立的失败观察 reason 事实，所以宁可不给（§7.7 的封闭值里没有对应项）。
type eventSpecDoc struct {
	Reason         string          `json:"reason,omitempty"`
	EventType      string          `json:"eventType"`
	InvolvedObject eventObjectRef  `json:"involvedObject"`
	TraceUID       string          `json:"traceUID"`
	DeliveryID     *string         `json:"deliveryId"`
	Payload        json.RawMessage `json:"payload"`
}

// eventObjectRef 是 involvedObject（来源只存名字，没有 UID 就不给）。
type eventObjectRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// eventStatusDoc 是 Event.status：剪枝观察（只有剪枝行有）。
type eventStatusDoc struct {
	Pruned *pruneFactDoc `json:"pruned"`
}

// pruneFactDoc 是剪枝观察的形状（reason 来自封闭集合 + 人话 detail）。
type pruneFactDoc struct {
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// eventEnvelope 组装 Event 信封。
func eventEnvelope(e *store.Event, nsDisplay string) apiEnvelope {
	if e == nil {
		return newEnvelope("Event", objectMeta{}, nil, nil)
	}
	if nsDisplay == "" {
		nsDisplay = e.NamespaceID
	}
	meta := objectMeta{
		Name:              e.ID,
		Namespace:         nsDisplay,
		UID:               e.ID,
		CreationTimestamp: taskTimestamp(e.CreatedAt),
	}
	payload := e.Payload
	if len(bytes.TrimSpace(payload)) == 0 {
		payload = json.RawMessage("{}")
	}
	reason := e.Reason
	if eventIsDispatchFailure(e) {
		// 失败观察行在 store 里的 reason 固定为 'Pruned'（insertPruneFactEventOn 的
		// 同一份边写）：直接给出去等于把「任务失败」报告成「剪枝」。省略比编造安全。
		reason = ""
	}
	return newEnvelope("Event", meta, eventSpecDoc{
		Reason:         reason,
		EventType:      e.Type,
		InvolvedObject: eventObjectRef{Kind: "Source", Name: e.Source},
		TraceUID:       e.TraceID,
		DeliveryID:     e.DeliveryID,
		Payload:        payload,
	}, eventStatusDoc{Pruned: eventPruneFactOf(e)})
}

// eventIsDispatchFailure 报告这条观察行是不是**未解析派发失败**（downstream_missing）。
//
// store 的失败观察事件和剪枝留痕共用 `insertPruneFactEventOn`（source='route'、
// reason='Pruned'），只有 type 能区分：这里 type 就是封闭原因码。capability.md §6.2
// 把 downstream_missing 定义为「任务失败 + 事件」，不是剪枝，所以投影层必须把它从
// Pruned 语义里拿出来（store 加独立 reason / 失败标记是后续缺口，owner 见 inventory）。
func eventIsDispatchFailure(e *store.Event) bool {
	return e != nil && e.Source == store.EventPruneSource && e.Type == protocol.ReasonDownstreamMissing
}

// eventPruneFactOf 从剪枝留痕行取观察原因（非剪枝行 → nil）。
//
// 剪枝行的 type 就是原因码（store 保证是封闭集合），payload 里另有一份 reason + detail
// （+ capability/taskId 等诊断），这里只用 reason/detail：其它字段属于内部链路身份。
// 失败观察行（downstream_missing）不是剪枝 → status.pruned 必须是 null。
func eventPruneFactOf(e *store.Event) *pruneFactDoc {
	if e == nil || e.Source != store.EventPruneSource || eventIsDispatchFailure(e) {
		return nil
	}
	var payload struct {
		Reason string `json:"reason"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(e.Payload, &payload)
	reason := strings.TrimSpace(payload.Reason)
	if reason == "" {
		reason = e.Type
	}
	return &pruneFactDoc{Reason: reason, Detail: payload.Detail}
}

// ---------- wire 形状：logs / emissions / trace（例外文档）----------

// taskLogsDoc 是 logs 子资源响应（§7.6）。lines 恒 `[]`，attempt 是**真实读到**的 attempt。
type taskLogsDoc struct {
	TaskID    string             `json:"taskId"`
	Attempt   int                `json:"attempt"`
	Lines     []protocol.LogLine `json:"lines"`
	Truncated bool               `json:"truncated"`
}

// taskEmissionsList 是 emissions 子资源响应（§7.6 例外文档：没有 apiVersion/kind）。
type taskEmissionsList struct {
	Metadata listMeta           `json:"metadata"`
	Items    []taskEmissionItem `json:"items"`
}

// taskEmissionItem 是**一次 emit 调用**的事实（不是端口 / 自动业务序号）。
//
// eventId 区分独立调用；显式 id（调用方给的幂等键）可重复请求但只留一条事实。
// taskId / pruned 是**可空事实**：剪枝回执的 taskId 就是真的 null（已接收、没派发）。
type taskEmissionItem struct {
	EventID string        `json:"eventId"`
	To      string        `json:"to"`
	Version *int          `json:"version,omitempty"`
	TaskID  *string       `json:"taskId"`
	ID      string        `json:"id,omitempty"`
	Pruned  *pruneFactDoc `json:"pruned"`
}

// taskEmissionItemOf 把落库行翻成 item。
//
// 解析失败时的退化是**保守**的：只能回退到行上确定的事实（toRef / emitId），
// eventId / version / taskId / pruned 保持缺省（省略或 null），不猜。
func taskEmissionItemOf(rec *store.EmitRecord) taskEmissionItem {
	item := taskEmissionItem{To: rec.ToRef}
	if rec.EmitID != nil {
		item.ID = *rec.EmitID
	}
	if len(bytes.TrimSpace(rec.Result)) > 0 {
		var res protocol.EmitResponse
		if err := json.Unmarshal(rec.Result, &res); err == nil {
			item.EventID = res.EventID
			if res.To != "" {
				item.To = res.To
			}
			if res.Version > 0 {
				version := res.Version
				item.Version = &version
			}
			item.TaskID = res.TaskID
			if res.Pruned != nil {
				item.Pruned = &pruneFactDoc{Reason: res.Pruned.Reason, Detail: res.Pruned.Detail}
			}
		}
	}
	return item
}

// taskTraceView 是一条 trace 的全景读模型（§7.7）：三份事实各自成数组，恒 `[]` 不为 null。
type taskTraceView struct {
	Events []apiEnvelope `json:"events"`
	Tasks  []apiEnvelope `json:"tasks"`
	Pruned []apiEnvelope `json:"pruned"`
}

// ---------- 小工具 ----------

// taskPathID 取并校验路径里的任务 id。
func taskPathID(r *http.Request) (string, error) {
	id := strings.TrimSpace(r.PathValue("taskId"))
	if id == "" {
		return "", errStatus(http.StatusBadRequest, reasonBadRequest, "taskId is required")
	}
	return id, nil
}

// taskActionReason 解析 cancel / abandon 的可选 body `{reason?}`（空 body = 用默认原因）。
func taskActionReason(r *http.Request, def string) (string, error) {
	if r.Body == nil {
		return def, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<10))
	if err != nil {
		return "", errStatus(http.StatusBadRequest, reasonBadRequest, "cannot read request body")
	}
	_ = r.Body.Close()
	if len(bytes.TrimSpace(raw)) == 0 {
		return def, nil
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", errStatus(http.StatusBadRequest, reasonBadRequest, "body must be a JSON object with an optional reason")
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		return def, nil
	}
	return reason, nil
}

// taskListUnknownParams 拒绝 Task 列表上未定义的 query 参数。
//
// 契约（api-conventions §3.3）定义的现有参数名是 `status` / `traceId` / `nodeId`；
// `limit` 是分页。旧实现用过 `phase` / `trace` / `node` / `capability`——保留它们
// 看似宽容，实际后果是「传旧名 → 拿到未过滤全量」的静默错结果，所以显式 400。
func taskListUnknownParams(q map[string][]string) error {
	allowed := map[string]bool{"limit": true, "status": true, "traceId": true, "nodeId": true}
	bad := make([]string, 0, len(q))
	for name := range q {
		if !allowed[name] {
			bad = append(bad, name)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return errStatus(http.StatusBadRequest, reasonUnknownField,
		fmt.Sprintf("query parameter %q is not defined for this list", bad[0]))
}

// taskQueryInt 取整数 query 参数；缺省用 def，非整数 → 400（不静默落回默认值）。
func taskQueryInt(r *http.Request, name string, def int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def, errStatus(http.StatusBadRequest, reasonBadRequest,
			fmt.Sprintf("%s must be an integer", name))
	}
	return n, nil
}

// taskQueryBool 取布尔 query 参数（true / 1 为真，其它为假）。
func taskQueryBool(r *http.Request, name string) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name))) {
	case "true", "1":
		return true
	}
	return false
}

// taskListLimit 与 store 的列表夹取逐字一致：<=0 或 >500 → 50。
//
// 为什么先算一次再传 store：store.ListTasks 的判据就是这一条（<=0||>500 → 50），
// 这里保持逐字等价，避免 handler 与 store 两个窗口。
func taskListLimit(n int) int {
	if n <= 0 || n > taskListLimitCap {
		return taskListDefaultLimit
	}
	return n
}

// taskClampListLimit 是资源列表（events）的 limit 口径：缺省 50、超过上限**钳到** 500。
//
// 与 taskListLimit 的差别是刻意的：tasks 列表沿用 store 的既有夹取（>500 折回 50），
// 而 events 是资源列表——契约 §3.3 要求默认/上限不变，折回 50 会让 `limit=501`
// 静默拿到一个**更小**的窗口（#236 的回归）。明细集合（emissions / console events）
// 用的是同一档钳位。
func taskClampListLimit(n int) int {
	if n <= 0 {
		return taskListDefaultLimit
	}
	if n > taskListLimitCap {
		return taskListLimitCap
	}
	return n
}

// taskNowSeconds 返回 server 的可信「秒」时间。
//
// serverDeps.Now 是毫秒（D1），而 store 的时钟（store.Now / RequestCancel 的 now 参数）
// 是秒：两者混用会让取消意图的时间落在 5 万年后。
func taskNowSeconds(d *serverDeps) int64 {
	if now := d.now() / 1000; now > 0 {
		return now
	}
	return store.Now()
}

// taskTimestamp 把 store 的秒级时间翻成 RFC3339（<=0 视为没有时间事实）。
func taskTimestamp(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// taskTimestampPtr 是可空时间的投影（nil / <=0 → null）。
func taskTimestampPtr(sec *int64) *string {
	if sec == nil || *sec <= 0 {
		return nil
	}
	out := taskTimestamp(*sec)
	return &out
}

// taskOptionalString 把空串翻成 null（「没有事实」与「空值」在 wire 上要分得开）。
func taskOptionalString(v string) *string {
	trimmed := strings.TrimSpace(v)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
