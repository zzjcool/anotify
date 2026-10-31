package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
)

// consoleHandler 是控制台 API 的处理器（契约 §6.2 的 8 个新端点）。
//
// 隔离边界：命名空间（不变量 I10 —— 组织层已在迁移 14 删除）。
// 每个命名空间资源端点都要解析 namespaceId，解析时过 store.AuthorizeNamespace
// 收口（见 namespace_scope.go 的 namespaceScope）。
type consoleHandler struct {
	db   *store.DB
	auth *auth.Service
}

// sessionUser 从会话中间件注入的 context 里取用户 ID。
//
// 这里的端点都挂在 sessMW 之后，所以理论上一定有值；
// 取不到时按 401 处理（而不是信任空用户）。
func (h *consoleHandler) sessionUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	userID := auth.UserIDFromContext(r.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return "", false
	}
	return userID, true
}

// ---------- 命名空间作用域（#162 / 契约 §4） ----------

// listLimitCap 是 store 列表查询的硬上限。
//
// 为什么要在 handler 侧也钳一次：store.ListTasks / ListEvents 对 `limit > 500` 的处理是
// **折回默认值 50**（而不是钳到 500）。跨命名空间汇总时每个命名空间都得取够，
// 否则 limit=600 会悄悄变成「每个命名空间只回 50 条」，合并后数据离奇地少。
const listLimitCap = 500

// namespaceIDs 把一次请求的命名空间作用域展开成要逐个查询的 id 列表。
//
// ns.All（namespaceId=*）时遍历本部署的全部命名空间，由调用方合并结果后再统一
// 排序/截断 —— **绝不把 `*` 传进 store**（store 层明确不接受它，见 store.AllNamespaces）。
func (h *consoleHandler) namespaceIDs(ctx context.Context, ns nsScope) ([]string, error) {
	if !ns.All {
		return []string{ns.ID}, nil
	}
	all, err := h.db.ListNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(all))
	for _, n := range all {
		ids = append(ids, n.ID)
	}
	return ids, nil
}

// namespaceLabel 是回给前端的 namespaceId。
//
// `*` 原样回（跨命名空间汇总）：不伪造成某个具体 id——那会让页面以为
// 「这些数据都属于某一个命名空间」，比不给更糟。
func namespaceLabel(ns nsScope) string {
	if ns.All {
		return store.AllNamespaces
	}
	return ns.ID
}

// isNamespaceError 判定错误是否属于「命名空间类」（由 writeNamespaceError 翻译）。
//
// 只把这一类交出去：其它 store 错误的处理方式保持原样（500 "internal error"，
// 不把内部错误文案漏给调用方）。
func isNamespaceError(err error) bool {
	var cross *store.CrossNamespaceError
	return errors.As(err, &cross) ||
		errors.Is(err, store.ErrNamespaceRequired) ||
		errors.Is(err, store.ErrNamespaceNotFound) ||
		errors.Is(err, store.ErrAuthRequired) ||
		errors.Is(err, store.ErrNamespaceExists) ||
		errors.Is(err, store.ErrLastNamespace)
}

// handleNamespaceError 把命名空间类错误翻成 HTTP 状态码并返回 true。
//
// 跨命名空间引用（I11）→ 400（writeNamespaceError 的 CrossNamespaceError 分支）：
// 那是**调用方请求错了**，静默忽略会让人以为配置生效了。
func handleNamespaceError(w http.ResponseWriter, err error) bool {
	if err == nil || !isNamespaceError(err) {
		return false
	}
	writeNamespaceError(w, err)
	return true
}

// mergeTasks 合并多个命名空间的任务列表，按 store 的排序口径
// （created_at DESC, id DESC）重排后截断到 limit。
//
// 单命名空间走快路径：store.ListTasks 的 SQL 已经是 `ORDER BY created_at DESC,
// id DESC`，groups 只有一个元素时再排一遍是纯浪费（#243 同一类每请求固定开销）。
// 跨命名空间（`*`）仍然必须归并重排 —— 那才是这个函数存在的理由。
func mergeTasks(limit int, groups ...[]*store.Task) []*store.Task {
	if len(groups) <= 1 {
		return truncateSingleGroup(groups, limit)
	}
	total := 0
	for _, g := range groups {
		total += len(g)
	}
	out := make([]*store.Task, 0, total)
	for _, g := range groups {
		out = append(out, g...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].ID > out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// mergeEvents 合并多个命名空间的事件列表（created_at DESC, id DESC）后截断。
//
// 单命名空间的快路径（#243）：`limit=500` 的事件流是热点，而绝大多数请求只作用于
// 一个命名空间 —— store.ListEvents 出来就已经是 created_at DESC, id DESC，再
// sort.SliceStable 一遍 500 个元素（含 reflect 交换）是每请求白付的 CPU。
func mergeEvents(limit int, groups ...[]*store.Event) []*store.Event {
	if len(groups) <= 1 {
		return truncateSingleGroup(groups, limit)
	}
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
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// truncateSingleGroup 是「只有一个命名空间」时的快路径：直接把该组（截断到 limit）
// 交回去，不再合并/重排。
//
// 两个前提（少一个都不能用）：① store 的 SQL 已经按调用方要的顺序排好；
// ② 只有一个分组时不存在跨组归并。空组要归一成**非 nil** 空切片（不变量 I9：
// 空列表给 []，不给 null）—— 这也是它与“直接 return groups[0]”的唯一差别，别省掉。
func truncateSingleGroup[T any](groups [][]T, limit int) []T {
	out := make([]T, 0)
	if len(groups) == 1 && groups[0] != nil {
		out = groups[0]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// mergeCapabilities 合并多个命名空间的能力定义（name, version, namespaceId 升序）。
//
// 末尾按 namespaceId 破平局：同名同版本的能力在两个命名空间里是两个不同定义，
// 汇总视图里必须相邻且顺序稳定（否则每次请求顺序都可能变，前端卡片会跳）。
func mergeCapabilities(groups ...[]*store.CapabilityVersion) []*store.CapabilityVersion {
	total := 0
	for _, g := range groups {
		total += len(g)
	}
	out := make([]*store.CapabilityVersion, 0, total)
	for _, g := range groups {
		out = append(out, g...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].Version != out[j].Version {
			return out[i].Version < out[j].Version
		}
		return out[i].NamespaceID < out[j].NamespaceID
	})
	return out
}

// mergeNodes 合并多个命名空间的节点列表（name, id 升序）。
//
// 单命名空间同样走快路径：store.ListNodes 的 SQL 已经是 `ORDER BY name, id`，
// 比较器 (Name, ID) 与之逐字段等价（#243 那类“每请求白排”的同类开销）。
func mergeNodes(groups ...[]*store.Node) []*store.Node {
	if len(groups) <= 1 {
		return truncateSingleGroup(groups, 0) // 节点列表不截断
	}
	total := 0
	for _, g := range groups {
		total += len(g)
	}
	out := make([]*store.Node, 0, total)
	for _, g := range groups {
		out = append(out, g...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// mergeEdges 合并多个命名空间的边（store 的 from/port/to 升序，再按命名空间破平局）。
func mergeEdges(groups ...[]*store.Edge) []*store.Edge {
	total := 0
	for _, g := range groups {
		total += len(g)
	}
	out := make([]*store.Edge, 0, total)
	for _, g := range groups {
		out = append(out, g...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].FromIsSource != out[j].FromIsSource {
			return !out[i].FromIsSource
		}
		if out[i].FromCapability != out[j].FromCapability {
			return out[i].FromCapability < out[j].FromCapability
		}
		if out[i].FromPort != out[j].FromPort {
			return out[i].FromPort < out[j].FromPort
		}
		if out[i].ToCapability != out[j].ToCapability {
			return out[i].ToCapability < out[j].ToCapability
		}
		return out[i].NamespaceID < out[j].NamespaceID
	})
	return out
}

// ---------- GET /v1/tasks ----------

// tasks 处理 GET /v1/tasks（按 trace/node/status 过滤）。
//
// namespaceId 必填（契约 §6.2）：给 `*` 时逐个命名空间查询后合并，
// 再按 limit 统一截断——跨命名空间的「最近 100 条」不能被拆成
// 「每个命名空间 100 条」（那就不是 100 条了）。
func (h *consoleHandler) tasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	q := r.URL.Query()
	limit := intParam(q.Get("limit"), 100, 1, 1000)

	ns, ok := h.resolveNamespace(w, r, "", true)
	if !ok {
		return
	}
	perNS := limit
	if perNS > listLimitCap {
		perNS = listLimitCap
	}

	nsIDs, err := h.namespaceIDs(r.Context(), ns)
	if err != nil {
		slog.Error("list namespaces failed", "event", "console.tasks.namespaces_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	groups := make([][]*store.Task, 0, len(nsIDs))
	for _, nsID := range nsIDs {
		got, err := h.db.ListTasks(r.Context(), nsID, q.Get("traceId"), q.Get("nodeId"), q.Get("status"), perNS)
		if err != nil {
			if handleNamespaceError(w, err) {
				return
			}
			slog.Error("list tasks failed", "event", "console.tasks.list_failed", "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		groups = append(groups, got)
	}
	tasks := mergeTasks(limit, groups...) // 空列表是 [] 不是 null（I9）
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks":       tasks,
		"namespaceId": namespaceLabel(ns),
	})
}

// ---------- GET /v1/events（控制台读）----------

// events 处理 GET /v1/events —— **控制台读取**用，走会话 Cookie。
//
// 为什么和 api.EventHandler 的 GET /v1/events 分开：
// 那个是**机器入口**（Bearer API Key + ScopeNotifySend），给外部系统拉事件；
// 控制台页面需要的是"我登录了这个组织，让我看看本组织的事件流"。
// 两者鉴权模型不同，硬塞进一个 handler 会让权限语义含混
// （一个需要 scope，一个需要会话成员身份）。
//
// mux 里两条路由靠方法区分不了（都是 GET），所以控制台的这条挂在
// /v1/console/events 下，避免和机器入口抢占同一路径。
func (h *consoleHandler) events(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	ns, ok := h.resolveNamespace(w, r, "", true)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit := intParam(q.Get("limit"), 100, 1, 1000)
	perNS := limit
	if perNS > listLimitCap {
		perNS = listLimitCap
	}

	nsIDs, err := h.namespaceIDs(r.Context(), ns)
	if err != nil {
		slog.Error("list namespaces failed", "event", "console.events.namespaces_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	groups := make([][]*store.Event, 0, len(nsIDs))
	for _, nsID := range nsIDs {
		got, err := h.db.ListEvents(r.Context(), nsID, q.Get("traceId"), q.Get("source"), perNS)
		if err != nil {
			if handleNamespaceError(w, err) {
				return
			}
			slog.Error("list events failed", "event", "console.events.list_failed", "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		groups = append(groups, got)
	}
	events := mergeEvents(limit, groups...) // 空列表是 [] 不是 null（I9）
	// trace 视图（带 traceId）：根事件必须在第一条（见 traceViewRootFirst 的注释）。
	// 事件流（不带 traceId）不需要：剪枝行在那条路径上已被 ListEvents 过滤掉。
	if q.Get("traceId") != "" {
		events = traceViewRootFirst(events)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events":      events,
		"namespaceId": namespaceLabel(ns),
	})
}

// traceViewRootFirst 把根事件（非剪枝）排到最前，剪枝行留在后面。
//
// 为什么需要：控制台 trace 视图的调用方（trace 页）把**第一条事件**当根事件，
// 用它的 source 去找源配置的拓扑当底图。剪枝行（#142 起 source='route'）
// 时间上一定晚于根事件，倒序排列就会被排到第一条 —— 页面于是拿到
// source="route" 当「谁触发的」，找不到源，底图丢失，
// 被剪枝的分支整片从图上消失（正是 #142 要回答的那个问题）。
// 同秒时 ListEvents 靠随机 id 破平局，所以不改就是**时好时坏**的 flaky 回归。
//
// 排序是**稳定**的：两组内部各自保持 ListEvents 的原有倒序，不重排时间。
// 不用 SQL 排序的理由：这是「控制台 trace 视图第一条是根事件」这个**视图契约**，
// 与 store 的通用排序语义无关；塞进 ListEvents 会连带改掉机器入口
// GET /v1/events?traceId= 的顺序（那是给外部系统的，不该动）。
func traceViewRootFirst(events []*store.Event) []*store.Event {
	if len(events) < 2 {
		return events
	}
	out := make([]*store.Event, 0, len(events))
	for _, e := range events {
		if e.Source != route.PruneEventSource {
			out = append(out, e)
		}
	}
	for _, e := range events {
		if e.Source == route.PruneEventSource {
			out = append(out, e)
		}
	}
	return out
}

// ---------- GET /v1/tasks/{id} ----------

// task 处理 GET /v1/tasks/{id} —— 按 ID 取单个任务。
//
// 为什么需要它：深链（点通知跳到 ?id=tsk_xxx）只有任务 ID，
// 不知道它属于哪条 trace。没有这个端点就只能靠 traceId 猜，
// 而 traceId 不是任务 ID——猜不中。
func (h *consoleHandler) task(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing task id"})
		return
	}
	// 单对象读端点：`*` 无定义 → 400（allowAll=false）
	ns, ok := h.resolveNamespace(w, r, "", false)
	if !ok {
		return
	}

	task, err := h.db.GetTask(r.Context(), ns.ID, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
			return
		}
		slog.Error("get task failed", "event", "console.task.get_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// 同时返回整条链，详情页一次请求就能画出任务链（省一次往返）
	chain, err := h.db.ListTasks(r.Context(), ns.ID, task.TraceID, "", "", 100)
	if err != nil {
		chain = []*store.Task{task}
	}
	if chain == nil {
		chain = []*store.Task{} // 不变量 I9
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": task, "chain": chain})
}

// ---------- GET /v1/traces/{id}（#142）----------

// tracePrune 是 trace 视图里的剪枝摘要（从 source='route' 的事件解析出来）。
//
// 字段全部带值、不用 omitempty：trace 页是「一条 trace 的全景」，
// 固定形状前端就不用为每个字段判空（拿不到的字段就是空串）。
type tracePrune struct {
	Reason           string `json:"reason"`
	FromCapability   string `json:"fromCapability"`
	Port             string `json:"port"`
	TargetCapability string `json:"targetCapability"`
	Detail           string `json:"detail"`
	CreatedAt        int64  `json:"createdAt"`
}

// trace 处理 GET /v1/traces/{id} —— 一条 trace 的全景（#142）。
//
// 为什么要这个端点：剪枝记录从 #142 起落进了 events（source='route'），
// 但「这条分支为什么没跑」是一个**一次请求就能答完**的问题：
// 光有事件流还得自己从 payload 里拼回来，任务链又要另一次往返。
// 所以一次给齐 events + tasks + pruned。
//
// 注意 events 走 ListEvents(namespaceID, id, "", …)：带 traceId 时它**含**剪枝行
// （过滤规则在 store 层，handler 不再筛一遍——两处筛就会有两种口径）。
func (h *consoleHandler) trace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing trace id"})
		return
	}
	// trace 是单对象视图（一条 trace 只属于一个命名空间），`*` 无定义 → 400。
	ns, ok := h.resolveNamespace(w, r, "", false)
	if !ok {
		return
	}

	events, err := h.db.ListEvents(r.Context(), ns.ID, id, "", 500)
	if err != nil {
		slog.Error("list trace events failed", "event", "console.trace.events_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	tasks, err := h.db.ListTasks(r.Context(), ns.ID, id, "", "", 500)
	if err != nil {
		slog.Error("list trace tasks failed", "event", "console.trace.tasks_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if events == nil {
		events = []*store.Event{} // 空列表是 [] 不是 null（I9）
	}
	if tasks == nil {
		tasks = []*store.Task{}
	}

	// 两个都空 = 这条 trace 在本组织不存在：不区分「trace 没事件」与「trace 不存在」，
	// 因为 trace_id 只能由事件/任务产生，两者都没有就是没有。
	if len(events) == 0 && len(tasks) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "trace not found"})
		return
	}

	pruned := make([]tracePrune, 0, len(events))
	for _, e := range events {
		if e.Source != route.PruneEventSource {
			continue
		}
		var p route.PruneEventPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			// payload 坏了不能把整个 trace 页拉串：跳过这一行，其它照常回
			continue
		}
		pruned = append(pruned, tracePrune{
			Reason:           strings.TrimPrefix(e.Type, "pruned."),
			FromCapability:   p.FromCapability,
			Port:             p.Port,
			TargetCapability: p.TargetCapability,
			Detail:           p.Detail,
			CreatedAt:        e.CreatedAt,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"traceId":     id,
		"namespaceId": ns.ID,
		"events":      events,
		"tasks":       tasks,
		"pruned":      pruned,
	})
}

// ---------- POST /v1/tasks/{id}/cancel ----------

// cancelTask 处理 POST /v1/tasks/{id}/cancel。
func (h *consoleHandler) cancelTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	taskID := strings.TrimPrefix(r.URL.Path, "/v1/tasks/")
	taskID = strings.TrimSuffix(taskID, "/cancel")
	if taskID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task id required"})
		return
	}
	// 写端点：`*` → 400（不能在一个“跨命名空间”的任务上执行取消）
	ns, ok := h.resolveNamespace(w, r, "", false)
	if !ok {
		return
	}

	// 先确认任务存在且属于本组织、本命名空间（GetTask 内部已按 namespace_id 过滤）
	task, err := h.db.GetTask(r.Context(), ns.ID, taskID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}
	if err != nil {
		slog.Error("get task failed", "event", "console.tasks.get_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if err := h.db.CancelTask(r.Context(), ns.ID, taskID); err != nil {
		if errors.Is(err, store.ErrInvalidTransition) {
			// 终态任务不能再取消——这是客户端可理解的冲突，不是服务端错误
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "task is already " + task.Status,
				"code":  "invalid_transition",
			})
			return
		}
		slog.Error("cancel task failed", "event", "console.tasks.cancel_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// 读回最新状态返回
	updated, err := h.db.GetTask(r.Context(), ns.ID, taskID)
	if err != nil {
		updated = task
		updated.Status = store.TaskCanceled
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": updated})
}

// ---------- GET/POST /v1/capabilities ----------

// capabilityListRequest 是 POST /v1/capabilities 的请求体（camelCase）。
//
// NamespaceID 也可以放在 query 里（query 优先，见 namespaceParam）；
// body 与 query 都没给 → 400（绝不回退默认命名空间）。
type capabilityListRequest struct {
	NamespaceID  string           `json:"namespaceId"`
	Name         string           `json:"name"`
	DisplayName  string           `json:"displayName,omitempty"`
	Version      int              `json:"version,omitempty"`
	ParamsSchema json.RawMessage  `json:"paramsSchema,omitempty"`
	Outputs      []string         `json:"outputs,omitempty"`
	Sticky       string           `json:"sticky,omitempty"`
	Lifecycle    *store.Lifecycle `json:"lifecycle,omitempty"`
	Source       string           `json:"source,omitempty"`
}

// capabilities 处理 GET（列表）/ POST（定义新能力）。
func (h *consoleHandler) capabilities(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.listCapabilities(w, r)
	case http.MethodPost:
		h.createCapability(w, r)
	case http.MethodPatch:
		h.updateCapabilityDisplayName(w, r)
	default:
		methodNotAllowed(w, "GET, POST, PATCH")
	}
}

// updateCapabilityDisplayName 只改 display_name 一列。
//
// 为什么不把它做成"新版本"：display_name 不属于能力定义。
// 不变量 I8 说的是「能力定义 append-only，钉 hash」——指的是
// name / params_schema / outputs / lifecycle / sticky 这些**影响行为**的字段，
// 它们一变，旧 trace 就无法解释。而显示名不参与路由、不参与 content_hash，
// 改它不会让任何历史 trace 变得看不懂。
//
// 所以这是一个**装饰字段的可变更新**，不是能力定义的修改。
// 能力定义本身仍然 append-only。
func (h *consoleHandler) updateCapabilityDisplayName(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NamespaceID string `json:"namespaceId"`
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	// 写端点：`*` → 400
	ns, ok := h.resolveNamespace(w, r, req.NamespaceID, false)
	if !ok {
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is required"})
		return
	}
	// 先确认这个能力属于本命名空间（不变量 I10）——别处的 id 在这里是 404，不泄露存在性
	if _, err := h.db.GetCapabilityByID(r.Context(), ns.ID, req.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "capability not found", "code": "not_found"})
			return
		}
		if handleNamespaceError(w, err) {
			return
		}
		slog.Error("get capability for patch failed", "event", "console.caps.patch_get_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := h.db.SetCapabilityDisplayName(r.Context(), ns.ID, req.ID, strings.TrimSpace(req.DisplayName)); err != nil {
		if handleNamespaceError(w, err) {
			return
		}
		slog.Error("set capability display name failed", "event", "console.caps.patch_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	updated, err := h.db.GetCapabilityByID(r.Context(), ns.ID, req.ID)
	if err != nil {
		slog.Error("reload capability after patch failed", "event", "console.caps.patch_reload_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"capability": updated})
}

func (h *consoleHandler) listCapabilities(w http.ResponseWriter, r *http.Request) {
	// 读端点：`*` 允许（跨命名空间汇总）。
	ns, ok := h.resolveNamespace(w, r, "", true)
	if !ok {
		return
	}
	nsIDs, err := h.namespaceIDs(r.Context(), ns)
	if err != nil {
		slog.Error("list namespaces failed", "event", "console.caps.namespaces_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	groups := make([][]*store.CapabilityVersion, 0, len(nsIDs))
	for _, nsID := range nsIDs {
		got, err := h.db.ListCapabilities(r.Context(), nsID)
		if err != nil {
			if handleNamespaceError(w, err) {
				return
			}
			slog.Error("list capabilities failed", "event", "console.caps.list_failed", "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		groups = append(groups, got)
	}
	caps := mergeCapabilities(groups...)

	// capNode 是节点在能力视图里的最小投影（够页面显示状态点就行）。
	// 必须写在 capView 之前：Go 的**局部**类型声明不支持先用后声明。
	type capNode struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	// 附带推导出来的 kind（不落库，不变量 I3）与"谁声明了它"。
	type capView struct {
		*store.CapabilityVersion
		Kind route.Kind `json:"kind"`
		// DeclaredBy 是声明了这个能力的节点（含 offline / draining）。
		//
		// 为什么要有：能力定义只是"能干什么"的白名单，真正让它跑得起来的是
		// **有机器声明它**。一个没有任何节点声明的能力是永远跑不起来的配置 ——
		// 这个事实在能力页上必须一眼看见，否则只能反向到节点页去拼。
		DeclaredBy []capNode `json:"declaredBy"`
		// ExecutedBy 说明这个能力由谁执行（推导量，不落库）：
		//
		//	hub   中枢自己执行（notify.phone）—— **本来就不需要节点声明**
		//	none  事件起点，不执行代码、不产生任务
		//	node  需要节点声明才跑得起来
		//
		// 没有这个字段的话，"没有节点声明"就无法区分"中枢执行"和
		// "根本没机器能跑"—— 前者是正常的，后者是配了个跑不起来的空壳。
		ExecutedBy string `json:"executedBy"`
	}
	// 每种能力由哪些节点声明。
	//
	// 用 NodesDeclaringCapability 而不是 NodesForCapability：后者是**派发**
	// 入口，会滤掉 draining 的机器；但"谁声明了这个能力"是配置事实，
	// 排空中的机器仍然声明着它，页面上必须看得到。
	//
	// 命名空间取能力行自己的 NamespaceID（汇总视图下每条能力的归属不同）。
	declaring := make(map[string][]capNode, len(caps))
	for _, c := range caps {
		nodes, err := h.db.NodesDeclaringCapability(r.Context(), c.NamespaceID, c.ID)
		if err != nil {
			slog.Error("nodes declaring capability failed", "event", "console.caps.nodes_failed", "error", err.Error())
			continue
		}
		list := make([]capNode, 0, len(nodes))
		for _, n := range nodes {
			list = append(list, capNode{ID: n.ID, Name: n.Name, Status: n.Status})
		}
		declaring[c.ID] = list
	}
	views := make([]capView, 0, len(caps))
	for _, c := range caps {
		list := declaring[c.ID]
		if list == nil {
			list = []capNode{}
		}
		executedBy := "node"
		if bc, ok := store.IsBuiltinCapability(c.Name, c.Version); ok {
			if bc.Central {
				executedBy = "hub"
			} else if bc.Origin {
				executedBy = "none"
			}
		}
		views = append(views, capView{
			CapabilityVersion: c,
			Kind:              route.KindForCapability(c, len(list) > 0),
			DeclaredBy:        list,
			ExecutedBy:        executedBy,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"capabilities": views,
		"namespaceId":  namespaceLabel(ns),
	})
}

func (h *consoleHandler) createCapability(w http.ResponseWriter, r *http.Request) {
	var req capabilityListRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	// 写端点：`*` → 400（一条能力定义只能属于一个命名空间）
	ns, ok := h.resolveNamespace(w, r, req.NamespaceID, false)
	if !ok {
		return
	}

	version := req.Version
	if version <= 0 {
		// 不指定版本时自动开下一个版本（append-only，不变量 I8）
		next, err := h.db.NextCapabilityVersion(r.Context(), ns.ID, req.Name)
		if err != nil {
			if handleNamespaceError(w, err) {
				return
			}
			slog.Error("next capability version failed", "event", "console.caps.next_failed", "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		version = next
	}

	schema := req.ParamsSchema
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	source := req.Source
	if source == "" {
		source = store.CapSourceCustom
	}
	c := &store.CapabilityVersion{
		NamespaceID:  ns.ID,
		Name:         req.Name,
		DisplayName:  strings.TrimSpace(req.DisplayName),
		Version:      version,
		ParamsSchema: schema,
		Outputs:      req.Outputs,
		Sticky:       req.Sticky,
		Source:       source,
	}
	if req.Lifecycle != nil {
		c.Lifecycle = *req.Lifecycle
	}

	if err := h.db.CreateCapability(r.Context(), c); err != nil {
		// 命名空间类错误（如 namespace 已不存在）有自己确实的状态码，不能当成 409
		if handleNamespaceError(w, err) {
			return
		}
		// 「同 name@version 换内容」与「同内容已有」由 store 区分，这里统一按冲突处理
		slog.Warn("create capability rejected",
			"event", "console.caps.create_rejected",
			"name", req.Name, "version", version, "error", err.Error())
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": err.Error(),
			"code":  "capability_conflict",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"capability": c,
		// 新建时该能力还没有节点声明（declared=false），但内置能力要走特例
		"kind": route.KindForCapability(c, false),
	})
}

// ---------- GET/POST/DELETE /v1/edges ----------

// edgeRequest 是 POST/DELETE /v1/edges 的请求体。
type edgeRequest struct {
	// NamespaceID 是这条边所属的命名空间（query 里也可以给，query 优先）。
	NamespaceID string `json:"namespaceId"`
	// FromIsSource 为 true 时 FromCapability 被解释为**来源名**（"github"），
	// 用于建「入口边」；否则是上游能力 id。
	FromIsSource   bool   `json:"fromIsSource"`
	FromCapability string `json:"fromCapability"`
	FromPort       string `json:"fromPort"`
	ToCapability   string `json:"toCapability"`
}

// edges 处理出口表 CRUD。
func (h *consoleHandler) edges(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.listEdges(w, r)
	case http.MethodPost:
		h.createEdge(w, r)
	case http.MethodDelete:
		h.deleteEdge(w, r)
	default:
		methodNotAllowed(w, "GET, POST, DELETE")
	}
}

func (h *consoleHandler) listEdges(w http.ResponseWriter, r *http.Request) {
	// 读端点：`*` 允许（跨命名空间汇总）。
	ns, ok := h.resolveNamespace(w, r, "", true)
	if !ok {
		return
	}
	nsIDs, err := h.namespaceIDs(r.Context(), ns)
	if err != nil {
		slog.Error("list namespaces failed", "event", "console.edges.namespaces_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	groups := make([][]*store.Edge, 0, len(nsIDs))
	for _, nsID := range nsIDs {
		got, err := h.db.ListEdges(r.Context(), nsID, r.URL.Query().Get("fromCapability"))
		if err != nil {
			if handleNamespaceError(w, err) {
				return
			}
			slog.Error("list edges failed", "event", "console.edges.list_failed", "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		groups = append(groups, got)
	}
	edges := mergeEdges(groups...)
	writeJSON(w, http.StatusOK, map[string]any{
		"edges":       edges,
		"namespaceId": namespaceLabel(ns),
	})
}

// createEdge 写入一条边。
//
// 校验分两层：
//  1. store 层：from_port ∈ from_capability.outputs、两端同组织（契约 I2 / §1.2）
//  2. 这里把 store 的拒绝翻译成 400（而不是 500）
func (h *consoleHandler) createEdge(w http.ResponseWriter, r *http.Request) {
	var req edgeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if req.FromCapability == "" || req.FromPort == "" || req.ToCapability == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "fromCapability, fromPort, toCapability are all required",
		})
		return
	}
	// 写端点：`*` → 400（一条边属于且只属于一个命名空间）
	ns, ok := h.resolveNamespace(w, r, req.NamespaceID, false)
	if !ok {
		return
	}

	e := &store.Edge{
		NamespaceID:    ns.ID,
		FromIsSource:   req.FromIsSource,
		FromCapability: req.FromCapability,
		FromPort:       req.FromPort,
		ToCapability:   req.ToCapability,
	}
	if err := h.db.CreateEdge(r.Context(), e); err != nil {
		// I11：两端能力必须同命名空间，跨命名空间引用 → 400
		if handleNamespaceError(w, err) {
			return
		}
		if errors.Is(err, store.ErrEdgeRejected) {
			// from_port 不在 outputs 里 / 能力不存在——都是客户端可修的错误
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": err.Error(),
				"code":  "edge_rejected",
			})
			return
		}
		slog.Error("create edge failed", "event", "console.edges.create_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"edge": e})
}

func (h *consoleHandler) deleteEdge(w http.ResponseWriter, r *http.Request) {
	// DELETE 没有 body 是常态。先把 body 解析成"可选补充"：
	// namespaceId 可以只放在 body 里，其它四个字段 query 优先、body 兜底。
	var req edgeRequest
	if r.Body != nil && r.Body != http.NoBody {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			// 没 body / body 不是 JSON 都不算错误（参数可以全放 query）
			req = edgeRequest{}
		}
	}
	// 写端点：`*` → 400
	ns, ok := h.resolveNamespace(w, r, req.NamespaceID, false)
	if !ok {
		return
	}
	q := r.URL.Query()
	from, port, to := q.Get("fromCapability"), q.Get("fromPort"), q.Get("toCapability")
	if from == "" {
		from = req.FromCapability
	}
	if port == "" {
		port = req.FromPort
	}
	if to == "" {
		to = req.ToCapability
	}
	fromIsSource := req.FromIsSource
	if v := q.Get("fromIsSource"); v != "" {
		fromIsSource = v == "1" || v == "true"
	}
	if from == "" || port == "" || to == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "fromCapability, fromPort, toCapability are all required",
		})
		return
	}
	if err := h.db.DeleteEdge(r.Context(), ns.ID, fromIsSource, from, port, to); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "edge not found"})
			return
		}
		if handleNamespaceError(w, err) {
			return
		}
		slog.Error("delete edge failed", "event", "console.edges.delete_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// ---------- GET /v1/nodes + POST /v1/nodes + GET /v1/nodes/{id} + POST /v1/nodes/{id}/drain ----------

// nodeCapView 是"节点声明的能力"在控制台视图里的投影。
//
// 为什么不直接给 "name@version" 字符串：节点页与节点详情页要显示用户自己
// 取的中文显示名（「分析 PR」比 analyze-pr@1 好读），而且标签要能点进
// 能力详情，需要一个稳定的 id。
type nodeCapView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     int    `json:"version"`
	DisplayName string `json:"displayName"`
	// RunSpec 是节点上报的执行方式原文（`{}` = 声明了能力但没声明执行方式）。
	// 只用于展示与诊断，中枢从不执行它（I1 边界）。
	RunSpec json.RawMessage `json:"runSpec"`
}

// nodeView 是 nodes 表的一行 + 控制台需要的派生信息。
type nodeView struct {
	*store.Node
	Capabilities []nodeCapView `json:"capabilities"`
	Instances    int           `json:"instances"`
	Busy         int           `json:"busy"`
}

// viewsFor 批量组装控制台视图（返回顺序与入参 nodes 一致）。
//
// 为什么要批量（#223）：旧的 viewFor 每台节点跑 2 条 SQL（ListNodeCapabilities +
// ListInstances），列表页 N=100 就是 201 条**串行** SQL（连接池上限 1），
// 64 台机器只能跑出 104 RPS / p50 76ms。这里把整批 id 合并成一个去重列表，
// 固定 2 条批量查 —— SQL 条数与节点数无关。
//
// 语义必须与旧实现完全一致：
//   - capabilities 按 (name, version) 升序（排序由 store 的批量查保证）
//   - 没有能力 / 没有槽位 → `[]` 与 0（不变量 I9：不给 null）
//   - 批量查失败**降级**而不是整页 500：列表页宁可少一半信息，也要让运维
//     看见剩下的机器（旧行为如此，改成本地失败会把可用性降低）
//
// 日志事件名沿用 console.nodes.caps_failed / insts_failed：排障时按事件名
// 统计的告警与看板不因这次优化失配。
func (h *consoleHandler) viewsFor(ctx context.Context, nodes []*store.Node) []nodeView {
	views := make([]nodeView, len(nodes))
	if len(nodes) == 0 {
		return views
	}
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	// 两次批量查彼此独立：能力查失败不该连带丢掉槽位统计（反之亦然）。
	capsByNode, err := h.db.ListCapabilitiesByNode(ctx, ids)
	if err != nil {
		slog.Error("list node capabilities failed", "event", "console.nodes.caps_failed", "error", err.Error())
		capsByNode = nil
	}
	statsByNode, err := h.db.InstanceStatsByNode(ctx, ids)
	if err != nil {
		slog.Error("list node instances failed", "event", "console.nodes.insts_failed", "error", err.Error())
		statsByNode = nil
	}
	for i, n := range nodes {
		caps := capsByNode[n.ID]
		capViews := make([]nodeCapView, 0, len(caps))
		for _, c := range caps {
			runSpec := c.RunSpec
			if len(runSpec) == 0 {
				runSpec = json.RawMessage(`{}`) // I9：宁可是空对象，也不要是 null/省略
			}
			capViews = append(capViews, nodeCapView{
				ID:          c.ID,
				Name:        c.Name,
				Version:     c.Version,
				DisplayName: c.DisplayName,
				RunSpec:     runSpec,
			})
		}
		stat := statsByNode[n.ID] // 没申报槽位的节点查不到 → 零值
		views[i] = nodeView{Node: n, Capabilities: capViews, Instances: stat.Total, Busy: stat.Busy}
	}
	return views
}

// viewFor 组装一台节点的控制台视图。
//
// 列表与详情**共用**它：这类"两处各拼一套字段"的分叉最后一定表现为
// "同一个东西两个页面不一样"（图例、全屏入口都踩过）。
//
// 单节点入口（createNode / nodeByID）直接走 viewsFor 的同一份代码，
// 避免批量版与单台版漂移成两套拼装逻辑。
func (h *consoleHandler) viewFor(ctx context.Context, n *store.Node) nodeView {
	return h.viewsFor(ctx, []*store.Node{n})[0]
}

// createNodeRequest 是 POST /v1/nodes 的请求体。
//
// Instances 只做非负校验、**不落库**（契约 Q5/D5）：实例是节点在 hello 帧里**申报**的，
// 不是中枢预创建的；`nodes.max_instances` 是安全硬顶，不能被一个「建议值」静默改写。
// 字段保留是为了让客户端能把同一份节点配置原样发过来。
type createNodeRequest struct {
	NamespaceID string   `json:"namespaceId"`
	Name        string   `json:"name"`
	Tags        []string `json:"tags,omitempty"`
	Instances   int      `json:"instances,omitempty"`
}

// createNode 处理 POST /v1/nodes（注册节点，返回只出现一次的明文令牌）。
//
// 顺序不是随意的：**先解析 body、再解析命名空间**。空 body 当「全字段为零值」继续走，
// 这样「空 body + 无 query namespaceId」报的是 namespaceId 缺失（调用方要知道该补什么），
// 而不是被空 body 的 EOF 抢先变成 "invalid JSON body"。
//
// 命名空间走与读端点同一份口径（resolveNamespace，allowAll=false）：缺参 400 /
// `*` 400 / 不存在 404。组织层删除后不再有「别人的命名空间」，所以原先为写端点
// 单独准备的「跨组织也报 404 以免当 id 探测器」这套映射（#185 / 契约 D2）已无对象。
//
// 令牌只在 201 响应体里出现这**一次**，库里只有 sha256（契约 §10.3 / D3）；
// 日志里绝不打印它——日志会被收集、转发、贴进工单。
func (h *consoleHandler) createNode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}

	var req createNodeRequest
	// 空 body（或只有空白）→ io.EOF → 当成零值结构继续走；只有非空但坏 JSON 才 400。
	if r.Body != nil && r.Body != http.NoBody {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
			return
		}
	}
	// 写端点：缺参 400、`*` 无定义 400、不存在 404（组织层删除后不存在「别人的」）。
	ns, ok := h.resolveNamespace(w, r, req.NamespaceID, false)
	if !ok {
		return
	}
	if msg := store.ValidateNodeName(req.Name); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg, "code": "invalid_name"})
		return
	}
	if req.Instances < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "instances must be >= 0"})
		return
	}

	// 唯一性交给表约束 UNIQUE(namespace_id, name)：不预查（预查会给并发注册留 TOCTOU 窗口）。
	n := &store.Node{NamespaceID: ns.ID, Name: req.Name, Tags: req.Tags}
	token, err := h.db.CreateNode(r.Context(), n)
	if errors.Is(err, store.ErrNodeExists) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "同名节点已存在", "code": "name_taken"})
		return
	}
	if handleNamespaceError(w, err) {
		// 防御：命名空间在解析之后被删掉了（CreateNode 会报 NotFound）。
		return
	}
	if err != nil {
		slog.Error("create node failed", "event", "console.nodes.create_failed",
			"namespace_id", ns.ID, "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// 事件日志只记 id（token 绝不进日志）。
	slog.Info("node created", "event", "console.nodes.created",
		"node_id", n.ID, "namespace_id", ns.ID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"node":  h.viewFor(r.Context(), n),
		"token": token,
	})
}

// nodes 处理 GET /v1/nodes（列表 + 在线状态）。
func (h *consoleHandler) nodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	// 读端点：`*` 允许（跨命名空间汇总）。
	ns, ok := h.resolveNamespace(w, r, "", true)
	if !ok {
		return
	}
	nsIDs, err := h.namespaceIDs(r.Context(), ns)
	if err != nil {
		slog.Error("list namespaces failed", "event", "console.nodes.namespaces_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	groups := make([][]*store.Node, 0, len(nsIDs))
	for _, nsID := range nsIDs {
		got, err := h.db.ListNodes(r.Context(), nsID)
		if err != nil {
			if handleNamespaceError(w, err) {
				return
			}
			slog.Error("list nodes failed", "event", "console.nodes.list_failed", "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		groups = append(groups, got)
	}
	nodes := mergeNodes(groups...)
	// 一次批量组装：整页固定 2 条派生查询（旧实现是 2×N 条串行）。
	views := h.viewsFor(r.Context(), nodes)
	writeJSON(w, http.StatusOK, map[string]any{
		"nodes":       views,
		"namespaceId": namespaceLabel(ns),
	})
}

// nodeByID 处理 GET /v1/nodes/{id}（节点详情）。
//
// 比列表多给一样东西：**每个并发槽位的具体状态**（哪个槽在跑哪个任务）。
// 列表只给数量 —— 一个列表页不需要 N×M 行，但详情页要能回答
// "这台机器现在到底卡在哪个槽上"。
func (h *consoleHandler) nodeByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	nodeID := r.PathValue("id")
	// 单对象读端点：`*` 无定义 → 400
	ns, ok := h.resolveNamespace(w, r, "", false)
	if !ok {
		return
	}
	n, err := h.db.GetNode(r.Context(), ns.ID, nodeID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "node not found"})
		return
	}
	if err != nil {
		slog.Error("get node failed", "event", "console.nodes.get_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	insts, err := h.db.ListInstances(r.Context(), n.ID)
	if err != nil {
		slog.Error("list node instances failed", "event", "console.nodes.insts_failed", "error", err.Error())
		insts = nil
	}
	if insts == nil {
		insts = []*store.NodeInstance{} // 不变量 I9：空列表给 []
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"node":      h.viewFor(r.Context(), n),
		"instances": insts,
	})
}

// drainNode 处理 POST /v1/nodes/{id}/drain（排空节点）。
//
// 排空语义：不再接新任务（NodesForCapability 会过滤掉 draining），
// 已在跑的任务继续到租约自然结束。**不主动 Cancel**那些任务——
// 取消是破坏性操作，排空是渐进退役。
func (h *consoleHandler) drainNode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	nodeID := strings.TrimPrefix(r.URL.Path, "/v1/nodes/")
	nodeID = strings.TrimSuffix(nodeID, "/drain")
	if nodeID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "node id required"})
		return
	}
	// 写端点：`*` → 400
	ns, ok := h.resolveNamespace(w, r, "", false)
	if !ok {
		return
	}

	node, err := h.db.GetNode(r.Context(), ns.ID, nodeID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "node not found"})
		return
	}
	if err != nil {
		slog.Error("get node failed", "event", "console.nodes.get_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := h.db.UpdateNodeStatus(r.Context(), ns.ID, nodeID, store.NodeStatusDraining); err != nil {
		if handleNamespaceError(w, err) {
			return
		}
		slog.Error("drain node failed", "event", "console.nodes.drain_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	node.Status = store.NodeStatusDraining
	slog.Info("node draining", "event", "console.nodes.draining",
		"node_id", nodeID, "namespace_id", ns.ID)
	writeJSON(w, http.StatusOK, map[string]any{"node": node})
}

// ---------- 小工具 ----------

// methodNotAllowed 返回 405 并带上 Allow 头。
func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
}

// intParam 解析整数 query 参数，带默认值与范围钳制（防 limit=999999999）。
func intParam(raw string, def, min, max int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}
