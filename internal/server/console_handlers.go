package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
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
	// targetAgentVersion 是中枢当前带的 agent 二进制版本（CONTRACT-agent-upgrade §5.1）：
	// GET /v1/nodes 顶层下发它，控制台据此显示「该升到哪一版」与「已是最新」。
	// 空串 = 服务端没带二进制（UI 不显示升级按钮）。由 mux 从 agentBinary 注入。
	targetAgentVersion string
	// taskLogs 是「按需向节点取日志」的数据来源（CONTRACT-logs-on-node §2）：
	// 生产接线是同一个 NodeHub（mux 里 consoleH.taskLogs = nodeHub），端点每次请求
	// 都经它现取一次——日志只存在节点上，中枢不落库、不缓存。
	//
	// 声明成窄接口而不是直接依赖 *ws.NodeHub：测试里塞 fake 就能覆盖命中/离线/
	// 没档案/超时/意外错误全部五条分支（不用起真 WS、也不用等 5 秒超时）。
	// nil = 未装配（仅测试/嵌入用法）→ 端点回 200 + available:false。
	taskLogs taskLogsSource
	// nudge 是「槽位已释放，巡检立刻再接一轮」的通知（#219）。
	//
	// 为什么取消也要它：docs/contracts/CONTRACT.md §3.3 要求
	// 「任务终态落库（槽位释放）→ 立即重派」。store.CancelTask 在终态落库后
	// 会 ReleaseInstanceByTask，但它的两步（Transition → ReleaseInstance）
	// 都不是事务、也不该知道"谁来重派"——所以由 handler 在成功后把「空出一个槽」
	// 这一个事实告诉巡检（生产接线 = lease.Sweeper.Nudge）。
	// 不接的话，被取消腾出的槽位最坏要等下一轮巡检（5s）才有人去用。
	//
	// nil = 未装配（测试/嵌入用法）→ 跳过，已有的调用路径行为不变。
	nudge func()
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
//
// 比较的是**引用**（P2.2）：`fromRef`/`toRef` 才是边的两端，按 id 排会排出一份
// 用户看不懂的顺序（升级后 id 还会变）。
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
		if out[i].FromRef != out[j].FromRef {
			return out[i].FromRef < out[j].FromRef
		}
		if out[i].FromPort != out[j].FromPort {
			return out[i].FromPort < out[j].FromPort
		}
		if out[i].ToRef != out[j].ToRef {
			return out[i].ToRef < out[j].ToRef
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
//
// 支持游标分页（#235）：`before=<上一页的 nextCursor>` 取更旧的一页，
// 响应回 `nextCursor`（空串 = 没有更旧的了）。游标编码只在本文件
// （encodeEventCursor / decodeEventCursor），机器入口不加游标（契约 §3.3）。
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
	// #236：这里曾是 1000，但 store 对 `limit > 500` 会兜底成 50（不是钳到 500），
	// 于是 `limit=501..1000` 会悄悄变成「每个命名空间 50 条」。收到真实硬顶
	// listLimitCap（= 500），并把生效值回显进响应——要多少给多少、给了多少都可观测。
	limit := intParam(q.Get("limit"), 100, 1, listLimitCap)
	// 「多看一行」（#235 收尾）：取 perNS = limit+1 行是为了**确认**还有没有更旧的行，
	// 而不是拿「行数 == limit」去猜（那样库里正好整页时，用户点「更旧」会先看到一个空页）。
	//
	// 但 perNS 必须 ≤ listLimitCap：store 的列表查询对 `limit > 500` 会兜底成 50，
	// 传 501 会把整页悄悄变成 50 条（#236 的病根）。因此 `limit == listLimitCap`（500）时
	// perNS == limit，取不到那多出来的一行 —— 见下面的退化判据。
	perNS := limit
	if perNS < listLimitCap {
		perNS = limit + 1
	}

	// 游标分页（#235）：`before` 是上一页响应的 `nextCursor`，给了就只取比它**更旧**的行。
	// 解析失败一律 400 —— 不静默降级（把它当「没有游标」就回第一页，翻页会莫名跳回
	// 最新，这正是 #236 那类「悄悄换一个窗口」的病根，不能在新参数上重犯）。
	rawBefore := q.Get("before")
	var beforeAt int64
	var beforeID string
	if rawBefore != "" {
		at, id, cerr := decodeEventCursor(rawBefore)
		if cerr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid before cursor"})
			return
		}
		beforeAt, beforeID = at, id
	}

	nsIDs, err := h.namespaceIDs(r.Context(), ns)
	if err != nil {
		slog.Error("list namespaces failed", "event", "console.events.namespaces_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	groups := make([][]*store.Event, 0, len(nsIDs))
	for _, nsID := range nsIDs {
		var got []*store.Event
		var lerr error
		if rawBefore != "" {
			// 每个命名空间各自按同一游标查（游标是 (created_at,id) 全局序，不命名空间局部）
			got, lerr = h.db.ListEventsBefore(r.Context(), nsID, q.Get("traceId"), q.Get("source"), beforeAt, beforeID, perNS)
		} else {
			// 首屏路径不变（不带游标就不多一个谓词，SQL 文本也不变）
			got, lerr = h.db.ListEvents(r.Context(), nsID, q.Get("traceId"), q.Get("source"), perNS)
		}
		if lerr != nil {
			if handleNamespaceError(w, lerr) {
				return
			}
			slog.Error("list events failed", "event", "console.events.list_failed", "error", lerr.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		groups = append(groups, got)
	}
	events := mergeEvents(perNS, groups...) // 空列表是 [] 不是 null（I9）
	// 合并后拿到至多 perNS 行（单命名空间快路径也按 perNS 截断）：比 limit 多出来的一行
	// 只用于**判定**「还有更旧的行」，不发给客户端。
	hasMore := len(events) > limit
	if !hasMore && limit >= listLimitCap {
		// 已知上限（退化）：limit == listLimitCap 时 perNS == limit，取不到「多一行」，
		// 只能回到旧判据（行数 >= limit ⇒ 给游标）。于是「库里正好 500 条 + limit=500」
		// 仍会多给一个游标（用户多看一个空页）。要拍平它得突破 store 的 500 硬顶，
		// 那是 #236 的另一件事，不在这里顺手改。
		hasMore = len(events) >= limit
	}
	if len(events) > limit {
		events = events[:limit]
	}
	// nextCursor 必须在 traceViewRootFirst **之前**、且从**截断后**的末行算：
	// 那一步会把剪枝行排到最后，重排后的末行不再是「本页最旧的行」，
	// 拿它当游标会跳过或重复事件（trace 视图里剪枝行时间上晚于根事件）。
	// 判据与契约§3.2 一致：hasMore 为假 ⇒ 服务端已确认没有更旧的行，给空串。
	nextCursor := ""
	if hasMore {
		last := events[len(events)-1]
		nextCursor = encodeEventCursor(last.CreatedAt, last.ID)
	}
	// trace 视图（带 traceId）：根事件必须在第一条（见 traceViewRootFirst 的注释）。
	// 事件流（不带 traceId）不需要：剪枝行在那条路径上已被 ListEvents 过滤掉。
	if q.Get("traceId") != "" {
		events = traceViewRootFirst(events)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events":      events,
		"namespaceId": namespaceLabel(ns),
		"limit":       limit, // 生效值（已夹到 listLimitCap，#236）
		"nextCursor":  nextCursor,
	})
}

// encodeEventCursor 把 (createdAt, id) 编成 URL 安全的不透明游标（#235 契约 §3.2）。
//
// base64.RawURLEncoding：不带 padding，字符集只有 [A-Za-z0-9_-]，
// 直接放 query 里不用再转义（标准 base64 的 `+`/`/`/`=` 都要 encode）。
// 里面就是 `<createdAt>:<id>`，**客户端不该解析它** —— 不透明才有换编码的自由。
func encodeEventCursor(createdAt int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(createdAt, 10) + ":" + id))
}

// decodeEventCursor 解出游标；任何不合法的输入都返回错误（调用方翻成 400）。
//
// 校验项：base64 能解、能按**第一个** `:` 切开、createdAt 是 int64 ≥ 0、id 非空。
// 严格是有意义的：`before` 是客户端回传的值，宽松解释一个坏游标（比如把 id 当成
// 空串）会让「更旧」退化成「更旧或相同」，翻页出现重复行而难以归因。
func decodeEventCursor(raw string) (int64, string, error) {
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, "", fmt.Errorf("decode base64: %w", err)
	}
	at, id, ok := strings.Cut(string(payload), ":")
	if !ok {
		return 0, "", fmt.Errorf("missing ':' separator")
	}
	n, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("parse createdAt: %w", err)
	}
	if n < 0 {
		return 0, "", fmt.Errorf("createdAt must be >= 0, got %d", n)
	}
	if id == "" {
		return 0, "", fmt.Errorf("empty id")
	}
	return n, id, nil
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

	// 取消成功 = 这条任务腾出了一个槽位（pending 的任务本来就没占槽，叫醒巡检
	// 只是多一次空转，由 ListRedispatchable 自然返回空——宁可多叫一次也不能漏）。
	// 位置在 CancelTask 返回 nil 之后：失败分支（409/500）已经在上面返回了。
	if h.nudge != nil {
		h.nudge()
	}

	// 读回最新状态返回
	updated, err := h.db.GetTask(r.Context(), ns.ID, taskID)
	if err != nil {
		updated = task
		updated.Status = store.TaskCanceled
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": updated})
}

// ---------- GET /v1/orphans ----------

// orphanTasks 列出「pending 超过一段时间且当前没有任何在线节点能跑」的任务（#270）。
//
// 为什么需要这个只读视图：孤儿任务不会报错、不会告警，只会静静地占着队列，
// 并且让背压的 `MIN(created_at) WHERE status='pending'` 只增不减。没有这个视图，
// 运维只能靠读一条 30 分钟才出现的 ERROR 日志才猜到"有孤儿"。
//
// 参数：
//
//	olderThan  时长（"1h" / "30m" / 纯数字=秒），默认 1h。刻意**不允许** 0：
//	           刚建出来的任务在派发瞬间也可能"暂时没有在线节点"，把它算孤儿会误报。
//	limit      默认 200，上限 1000（这是给人看的诊断视图，不是导出接口）
//
// 跨命名空间 `*` 允许：诊断恰恰需要一次看全。
func (h *consoleHandler) orphanTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	ns, ok := h.resolveNamespace(w, r, "", true)
	if !ok {
		return
	}
	olderThanSec, err := parseOlderThan(r.URL.Query().Get("olderThan"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be a positive integer"})
			return
		}
		limit = n
	}

	nsIDs := []string{ns.ID}
	if ns.All {
		list, lerr := h.db.ListNamespaces(r.Context())
		if lerr != nil {
			slog.Error("list namespaces failed", "event", "console.orphans.ns_failed", "error", lerr.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		nsIDs = nsIDs[:0]
		for _, n := range list {
			nsIDs = append(nsIDs, n.ID)
		}
	}

	items := make([]store.OrphanPendingTask, 0, 16)
	for _, id := range nsIDs {
		got, lerr := h.db.ListOrphanPendingTasks(r.Context(), id, olderThanSec, limit)
		if lerr != nil {
			slog.Error("list orphan tasks failed",
				"event", "console.orphans.list_failed", "namespace_id", id, "error", lerr.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		items = append(items, got...)
	}
	// 空列表必须是 []，不能是 null（前端把 null 当成"未连接"）。
	if items == nil {
		items = []store.OrphanPendingTask{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"orphans":   items,
		"count":     len(items),
		"olderThan": olderThanSec,
		"namespace": ns.ID,
		"allScopes": ns.All,
		"hint":      "孤儿任务不会被自动清理：确认后用 POST /v1/tasks/{id}/abandon 显式处置",
	})
}

// parseOlderThan 解析 olderThan 参数：支持 "1h"/"30m"/"90s" 与纯数字（秒）。
//
// 上限 30 天、下限 1 分钟：下限防误报（刚派发的任务可能暂时没有在线节点），
// 上限防"用 olderThan=10年 一次性列出全部 pending"这种把诊断接口当导出的用法。
func parseOlderThan(v string) (int, error) {
	const (
		minSec = 60
		maxSec = 30 * 24 * 3600
		defSec = 3600
	)
	v = strings.TrimSpace(v)
	if v == "" {
		return defSec, nil
	}
	sec := 0
	if n, err := strconv.Atoi(v); err == nil {
		sec = n
	} else {
		d, derr := time.ParseDuration(v)
		if derr != nil {
			return 0, errors.New("olderThan 需要是时长（如 1h）或秒数")
		}
		sec = int(d.Seconds())
	}
	if sec < minSec {
		return 0, errors.New("olderThan 不能小于 1m（刚派发的任务可能暂时没有在线节点）")
	}
	if sec > maxSec {
		sec = maxSec
	}
	return sec, nil
}

// ---------- POST /v1/tasks/{id}/abandon ----------

// abandonTask 把一条 pending 任务显式转成 abandoned（#270）。
//
// 刻意**不自动**：孤儿任务是有价值的事实记录（这条事件本该被处理却没被处理），
// 自动放弃会把事故证据一起清掉。要不要自动化是运营策略，由人拍板。
func (h *consoleHandler) abandonTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	taskID := r.PathValue("id")
	if taskID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task id required"})
		return
	}
	// 写端点：`*` → 400（不能在一个"跨命名空间"的任务上执行写操作）
	ns, ok := h.resolveNamespace(w, r, "", false)
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body)

	if err := h.db.AbandonPendingTask(r.Context(), ns.ID, taskID, body.Reason); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		case errors.Is(err, store.ErrInvalidTransition):
			// 已经派发/取消/完成：如实告诉调用方，不能假装成功
			cur := ""
			if t, gerr := h.db.GetTask(r.Context(), ns.ID, taskID); gerr == nil {
				cur = t.Status
			}
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "task is not pending" + map[bool]string{true: " (now " + cur + ")"}[cur != ""],
				"code":  "invalid_transition",
			})
		default:
			slog.Error("abandon task failed",
				"event", "console.tasks.abandon_failed", "task_id", taskID, "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		}
		return
	}

	updated, gerr := h.db.GetTask(r.Context(), ns.ID, taskID)
	if gerr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
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

	// Run 是这个能力的**实现**（可选）。有 run → 编译成一份产物并落 artifacts，
	// 之后就能在控制台点「部署」。没有 run → 纯定义（例如占位节点、内置能力）。
	//
	// v2 的语义变化：执行方式属于能力定义，不再由节点在 node.yaml 里自报。
	Run *capabilityRunInput `json:"run,omitempty"`
	// Requires 是机器上必须预装的外部命令（部署时探测，缺了部署失败）。
	Requires []string `json:"requires,omitempty"`

	// Force 显式越过**升级闸门**（P2.2，设计 §3.6）。
	//
	// 只有「新版本成为 latest 且会破坏已有 name 型 link」时需要它：
	// 那种提交是一次**改线**（没人点过任何东西，线上那条链却换了契约），
	// 默认必须被拒，让调用方先看清楚 `upgradeImpact` 再决定。
	Force bool `json:"force,omitempty"`

	// Links 是「新建能力时一起建的下游边」（可选，camelCase：fromPort/toRef/literals）。
	//
	// 有它就走**原子路径**：能力行、产物行、边行写在同一个事务里（见
	// store.CreateCapabilityWithLinksAndArtifact）。没有它 = 今天的行为，一个字节都不变：
	// 建能力 → （可选）存产物；下游边由调用方自己 PUT /v1/capabilities/{name}/downstreams 整体替换。
	//
	// 为什么不能逐条建：中间失败就留下「能力在、边缺」的半成品，而能力表是
	// append-only 的——那个半成品永远撤不回来，客户端只能自己记一份待重试清单。
	Links []store.LinkSpec `json:"links,omitempty"`

	// Balance 是端口级平衡策略（契约 §1）：`{端口: {"mode": "priority"}}`，缺省 = priority。
	//
	// 本期只实现 priority；all / random / least_loaded / hash 会被 store 拒成
	// 400 balance_rejected（"尚未实现"），**不静默降级** —— 静默降级会让
	// 「我配了 all、跑起来却是 1:1」永远查不出来。
	Balance store.Balance `json:"balance,omitempty"`
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

// capabilityArtifactView 是「最新产物里详情页直接能用的那几项」的线上形状。
//
// Requires 是产物声明的外部命令（节点探活/部署前检查用），详情页拿它显示
// 「这台机器上要预装什么」——它属于能力定义，和脚本正文一样只在这里暴露。
type capabilityArtifactView struct {
	Digest     string            `json:"digest"`
	Entrypoint string            `json:"entrypoint"`
	Files      map[string]string `json:"files"`
	TimeoutSec int               `json:"timeoutSec"`
	WorkDir    string            `json:"workDir"`
	Requires   []string          `json:"requires"`
}

// capabilityArtifact 返回某能力**最新产物**的解析后内容（详情页展示脚本用）。
//
// 与 GET /v1/artifacts 的分工：那个返回的是 base64 的 spec 原文（部署与排障用），
// 这里返回解析后的文件正文（「让人看一眼脚本」用）。两份形状各有用途，不合并——
// 一个是传输格式，一个是展示格式，合起来两种用法都得将就。
//
// 为什么不把它塞进 GET /v1/capabilities：files 是**全部文件正文**
// （单文件 256 KiB、合计 1 MiB，见 internal/artifact 的上限），列表一次几十条能力
// 会被撑爆；而绝大多数时候没人展开详情。所以详情展开时才懒加载这一个端点。
//
// 404 语义（防探测）：**能力不属于本命名空间**与**能力没有产物**返回逐字节相同的
// body。否则用两种 body 就能问出「这个 id 在本命名空间存在吗」，而能力 id 是跨
// 命名空间可枚举的。
func (h *consoleHandler) capabilityArtifact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	// 单对象读：`*` 没有定义（「一整个汇总的产物」是什么？）→ 400。
	ns, ok := h.resolveNamespace(w, r, "", false)
	if !ok {
		return
	}
	// 两种 404 共用这一个写入点：body 逐字节相同是**刻意**的（见函数注释）。
	notFound := func() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "capability not found", "code": "not_found"})
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		notFound()
		return
	}
	if _, err := h.db.GetCapabilityByID(r.Context(), ns.ID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			notFound()
			return
		}
		if handleNamespaceError(w, err) {
			return
		}
		slog.Error("get capability for artifact failed",
			"event", "console.caps.artifact_get_failed", "namespace_id", ns.ID, "capability_id", id, "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	row, err := h.db.LatestArtifact(r.Context(), ns.ID, id)
	if err != nil {
		if errors.Is(err, store.ErrArtifactNotFound) {
			// 还没产物是**正常状态**（刚定义、还没部署），但对「这一份产物」
			// 而言就是不存在 —— 与能力不存在同一个回答，不额外泄露信息。
			notFound()
			return
		}
		if handleNamespaceError(w, err) {
			return
		}
		slog.Error("latest artifact failed",
			"event", "console.caps.artifact_latest_failed", "namespace_id", ns.ID, "capability_id", id, "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// artifacts.spec 存的是**整份产物**的 canonical JSON，run 在 spec 下面。
	var art artifact.Artifact
	if err := json.Unmarshal(row.Spec, &art); err != nil {
		// 解不开不是「不存在」：这是库里的数据坏了，如实报 500（前端会显示载入失败 + 重试）。
		slog.Error("decode artifact spec failed",
			"event", "console.caps.artifact_decode_failed",
			"namespace_id", ns.ID, "capability_id", id, "artifact_id", row.ID, "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// 空 map / 空切片一律给 `{}` / `[]`（不变量 I9）：前端不必为一个 null 判空。
	files := art.Spec.Run.Files
	if files == nil {
		files = map[string]string{}
	}
	requires := art.Spec.Requires
	if requires == nil {
		requires = []string{}
	}
	writeJSON(w, http.StatusOK, capabilityArtifactView{
		Digest:     row.Digest,
		Entrypoint: art.Spec.Run.Entrypoint,
		Files:      files,
		TimeoutSec: art.Spec.Run.TimeoutSec,
		WorkDir:    art.Spec.Run.WorkDir,
		Requires:   requires,
	})
}

// capabilityKind 推导一个能力的 kind（推导量，不落库：不变量 I3）。
//
// #383 起 kind **看边**（有出边 = 不是终点；有出边且有入边 = 中间段），不再看
// outputs —— outputs 是产物里的端口声明，把一个 outputs: [] 的能力接上下游之后，
// 它就不再是「链到这里结束」。
//
// 读接线失败时降级成「没有任何边」并记一条日志：kind 只是展示量，不该因为这个
// 查询失败让整个能力列表 500（与 assign 的 enrichment 同一个取舍）。
func (h *consoleHandler) capabilityKind(ctx context.Context, c *store.CapabilityVersion) route.Kind {
	if c == nil {
		return route.KindOrigin
	}
	shape, err := h.db.CapabilityEdgeShape(ctx, c.NamespaceID, c.Name)
	if err != nil {
		slog.Warn("capability kind: read edge shape failed, falling back to no edges",
			"event", "console.caps.kind_shape_failed",
			"namespace_id", c.NamespaceID, "capability", c.Name, "error", err.Error())
	}
	return route.KindForCapability(c, shape)
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
		// Run 是「这个能力怎么跑」的只读投影（超时 / 工作目录 / 依赖）。
		//
		// 真相在产物里（artifacts.spec.run），随 digest 走、中枢只存不解释（I1）。
		// 之所以在列表里再投影一份：详情页要回答「它跑多久超时、在哪个目录跑」，
		// 而这属于**能力定义**的一部分（#289），不该要求用户去翻产物。
		// 取不到产物时留 null：前端据此显示「—」，而不是把两行藏起来
		// ——藏起来会让人以为这个能力没有超时。
		Run *capabilityRunView `json:"run,omitempty"`
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
			Kind:              h.capabilityKind(r.Context(), c),
			DeclaredBy:        list,
			ExecutedBy:        executedBy,
			Run:               h.capabilityRun(r.Context(), c),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"capabilities": views,
		"namespaceId":  namespaceLabel(ns),
	})
}

// specLifecycle 把扁平请求里的 lifecycle 搬进正文（nil = 不声明）。
//
// 不声明时给 nil 而不是 `{}`：`{}` 与「没有这个键」在规范字节里是两种不同的
// 内容，同一份配置不该因为客户端有没有把 lifecycle 传成空对象而算出两个 digest。
func specLifecycle(lc *store.Lifecycle) json.RawMessage {
	if lc == nil {
		return nil
	}
	b, err := json.Marshal(lc)
	if err != nil {
		return nil // Lifecycle 全是标量/指针，实际不可能失败
	}
	return b
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
		Balance:      req.Balance,
		Sticky:       req.Sticky,
		Source:       source,
	}
	if req.Lifecycle != nil {
		c.Lifecycle = *req.Lifecycle
	}

	// 正文（spec）在这里组装：P1-b1 的请求体仍是旧的**扁平形状**（契约 §5 把形状切换
	// 推到 P4），服务端负责把它拼成 spec —— store 只认 spec，派生列由它自己算。
	//
	// 没有 run 不算错：这个版本里它等价于 `handler: noop` 占位（合法、可部署、
	// 无副作用），与回填老能力是同一条口径。**「缺实现 → 400 impl_missing」的行为
	// 收紧属于 P1-b2**（契约 §5 的行为列 + §7 的分期）：现在做会让所有既有客户端
	// 与 e2e 夹具一起变红，那不是 P1-b1 的职责。
	if req.Run != nil {
		// 产物必须在**建能力之前**先校验一遍：一份 run 写错的请求不该留下一个
		// 只建了一半的能力（append-only 的能力表里，那会成为一条永远改不掉、也
		// 永远跑不起来的定义）。这里只做校验、不落库 —— 落库用的是 c 归一后的正文。
		built, err := buildArtifact(c, req.Run, req.Requires)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": err.Error(),
				"code":  "invalid_artifact",
			})
			return
		}
		c.Spec = built.Spec
	} else {
		c.Spec = store.HandlerSpec(artifact.HandlerNoop, schema, req.Outputs, specLifecycle(req.Lifecycle))
	}

	// 带 links → 原子路径（能力 + 产物 + 边，同一事务，要么全成要么全不成）。
	// 不带 links → 今天的行为一个字节都不变（下面这条路径）。
	if len(req.Links) > 0 {
		h.createCapabilityWithLinks(w, r, ns, c, req)
		return
	}

	// 升级闸门（P2.2，设计 §3.6）：先把影响面算出来回给调用方。
	//
	// 两件事一起做：
	//  1. 前端要能**先看到**「这一版会让哪几条 link 断」（不是先被拒再猜）；
	//  2. 破坏性变更且没传 force → 409 带结构化 impact，而不是一句
	//     “conflict”（那会被当成「版本号重了」，与「会改线」完全不是一回事）。
	impact, err := h.db.CheckUpgrade(r.Context(), ns.ID, c)
	if err != nil {
		if handleNamespaceError(w, err) {
			return
		}
		slog.Error("capability upgrade check failed",
			"event", "console.caps.upgrade_check_failed",
			"namespace_id", ns.ID, "name", req.Name, "version", version, "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if impact.Breaking && !req.Force {
		slog.Warn("capability upgrade rejected by gate",
			"event", "console.caps.upgrade_rejected",
			"namespace_id", ns.ID, "name", req.Name, "version", version,
			"changes", len(impact.Changes), "links", len(impact.Links))
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":         (&store.UpgradeBreakingError{Impact: impact}).Error(),
			"code":          "upgrade_breaking",
			"upgradeImpact": impact,
		})
		return
	}

	if err := h.db.CreateCapabilityForce(r.Context(), c, req.Force); err != nil {
		// 命名空间类错误（如 namespace 已不存在）有自己确实的状态码，不能当成 409
		if handleNamespaceError(w, err) {
			return
		}
		// 闸门在 store 里还会再判一次（两处同一份实现）：并发下别的请求先推了
		// 一个新版本，这里的预计算可能已经过期——那种情况照样返回结构化拒绝，
		// 不能让用户拿到一个「看起来没问题」的 200。
		var breaking *store.UpgradeBreakingError
		if errors.As(err, &breaking) {
			slog.Warn("create capability rejected by upgrade gate",
				"event", "console.caps.upgrade_rejected",
				"namespace_id", ns.ID, "name", req.Name, "version", version)
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":         breaking.Error(),
				"code":          "upgrade_breaking",
				"upgradeImpact": breaking.Impact,
			})
			return
		}
		// 正文不合法（缺实现 / 实现二义 / handler 不在白名单）：客户端可修 → 400。
		// 必须排在「统一 409」之前：内容错误不是版本冲突，用户改正文就能过。
		if errors.Is(err, store.ErrCapabilityInvalid) {
			slog.Warn("create capability rejected: invalid spec",
				"event", "console.caps.invalid_spec",
				"namespace_id", ns.ID, "name", req.Name, "error", err.Error())
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": err.Error(),
				"code":  "invalid_artifact",
			})
			return
		}
		// balance 不合法（未实现的 mode / 端口不在 outputs）：客户端可修 → 400。
		// 必须排在「统一 409」之前：否则「配了 all」会被当成版本冲突，用户改不对。
		if errors.Is(err, store.ErrBalanceRejected) {
			slog.Warn("create capability rejected: invalid balance",
				"event", "console.caps.balance_rejected",
				"namespace_id", ns.ID, "name", req.Name, "error", err.Error())
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": err.Error(),
				"code":  "balance_rejected",
			})
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
	resp := map[string]any{
		"capability": c,
		// 新建时还不可能有节点声明，但 kind 看的是**边**（同名旧版本的出边也归这个名字管），
		// 所以这里按真库读一次，而不是写死一个常量。
		"kind": h.capabilityKind(r.Context(), c),
		// 影响面**总是**回（空影响面也是 [] 而不是 null）：
		// 前端不必为「这一次到底查过没有」判空，也不必自己再算一遍。
		"upgradeImpact": impact,
	}
	if req.Run != nil {
		// **用 c 归一后的正文**重建产物：store 可能把行改成另一份内容（幂等重放回到
		// 已有那一行），此时请求里的版本号是过期的。行上的 Spec 才是权威的那份正文
		// ——从它拼产物，与 capability_versions.spec 与 content_hash 天然同源。
		built := c.Artifact()
		saved, digest, err := storeArtifact(r.Context(), h.db, ns.ID, c.ID, &built)
		if err != nil {
			// 能力已经建好了（append-only，无法回滚），产物落库失败只能如实报错：
			// 用户看到的应该是「能力建了但产物没存，请重试」，而不是一个假的 200。
			slog.Error("store artifact failed",
				"event", "console.caps.artifact_failed",
				"namespace_id", ns.ID, "name", c.Name, "version", c.Version, "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "capability created but artifact could not be stored: " + err.Error(),
				"code":  "artifact_store_failed",
			})
			return
		}
		resp["artifact"] = saved
		resp["digest"] = digest
	}
	writeJSON(w, http.StatusOK, resp)
}

// createCapabilityWithLinks 是 POST /v1/capabilities 带 `links` 时的**原子**路径。
//
// 目标语义：要么全成，要么全不成。所以能力行、产物行、边行在 store 里写在
// 同一个事务（见 store.CreateCapabilityWithLinksAndArtifact），这里只负责：
//
//	① 升级闸门预计算（给调用方先看影响面 / 直接 409）
//	② 把「产物怎么构建」包成回调（归一化之后才能算对 digest，见 ArtifactBuilder）
//	③ 把 store 的拒绝翻成 4xx（边不合法 = 客户端可修 → 400，并且带下标）
//	④ 拼响应：能力视图 + links（现有 edge 视图）+ upgradeImpact
//
// 与不带 links 的路径相比，这里**没有**「能力建了但产物/边没成」这种中间态：
// 任何一步失败，库里一行都不会留下。
func (h *consoleHandler) createCapabilityWithLinks(w http.ResponseWriter, r *http.Request, ns nsScope, c *store.CapabilityVersion, req capabilityListRequest) {
	// 升级闸门（P2.2，设计 §3.6）：与老路径同一口径——先把影响面算出来回给调用方；
	// 破坏性变更且没传 force → 409 带结构化 impact。
	// 真正的拦截在 store 的事务里（那里的快照才是权威的，这里是「先看清楚再决定」）。
	impact, err := h.db.CheckUpgrade(r.Context(), ns.ID, c)
	if err != nil {
		if handleNamespaceError(w, err) {
			return
		}
		slog.Error("capability upgrade check failed",
			"event", "console.caps.upgrade_check_failed",
			"namespace_id", ns.ID, "name", c.Name, "version", c.Version, "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if impact.Breaking && !req.Force {
		slog.Warn("capability upgrade rejected by gate",
			"event", "console.caps.upgrade_rejected",
			"namespace_id", ns.ID, "name", c.Name, "version", c.Version,
			"changes", len(impact.Changes), "links", len(impact.Links))
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":         (&store.UpgradeBreakingError{Impact: impact}).Error(),
			"code":          "upgrade_breaking",
			"upgradeImpact": impact,
		})
		return
	}

	// 产物：把「构建 + 算 digest」包成回调，交给事务在**能力行归一化之后**调用。
	//
	// 为什么不能在这里先建好再传：content-hash 幂等会把「同内容不同版本」折叠到已有行，
	// 归一化之后版本号可能变，而产物里嵌着 name@version（digest 也吃它）。
	// 用请求里的版本建产物 = 写出一份元数据与能力定义不一致、digest 也算错的产物。
	var digest string
	var build store.ArtifactBuilder
	if req.Run != nil {
		build = func(authoritative *store.CapabilityVersion) (*store.Artifact, error) {
			// 正文以事务内归一后的那一行为准（见上面的 ArtifactBuilder 注释）：
			// 从行拼产物，digest 与 content_hash 必然同源。
			built := authoritative.Artifact()
			row, dg, berr := newStoreArtifact(authoritative.NamespaceID, authoritative.ID, &built)
			if berr != nil {
				return nil, berr
			}
			digest = dg
			return row, nil
		}
	}

	res, err := h.db.CreateCapabilityWithLinksAndArtifact(r.Context(), c, req.Links, build, req.Force)
	if err != nil {
		// 跨命名空间引用（I11）也是客户端可修的错误 → 400。
		//
		// 但它**不能**直接走 handleNamespaceError：那条路会写 CrossNamespaceError 自己的
		// 文案，而真正有用的信息是「第几条」（在外层 wrapper 的 Error() 里）。
		// 所以先把带下标的整句写出去，再兜其余命名空间类错误。
		var cross *store.CrossNamespaceError
		if errors.As(err, &cross) {
			slog.Warn("create capability rejected: cross-namespace link",
				"event", "console.caps.link_rejected",
				"namespace_id", ns.ID, "name", req.Name, "error", err.Error())
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": err.Error(),
				"code":  "edge_rejected",
			})
			return
		}
		// 其余命名空间类错误（如 namespace 已不存在）有自己确实的状态码
		if handleNamespaceError(w, err) {
			return
		}
		// 闸门在 store 里还会再判一次（同一份实现）：并发下别的请求先推了一个新版本，
		// 这里的预计算可能已经过期——那种情况照样返回结构化拒绝。
		var breaking *store.UpgradeBreakingError
		if errors.As(err, &breaking) {
			slog.Warn("create capability rejected by upgrade gate",
				"event", "console.caps.upgrade_rejected",
				"namespace_id", ns.ID, "name", req.Name, "version", c.Version)
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":         breaking.Error(),
				"code":          "upgrade_breaking",
				"upgradeImpact": breaking.Impact,
			})
			return
		}
		// 边不合法（目标不存在 / port 不在 outputs / literals 键不存在 / 成环）：
		// 都是客户端可修的错误 → 400，且错误文案带 `links[i]` 下标，前端能直接定位是哪一条。
		if errors.Is(err, store.ErrEdgeRejected) {
			slog.Warn("create capability rejected: invalid link",
				"event", "console.caps.link_rejected",
				"namespace_id", ns.ID, "name", req.Name, "error", err.Error())
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": err.Error(),
				"code":  "edge_rejected",
			})
			return
		}
		// 正文不合法（缺实现 / 实现二义 / handler 不在白名单）：客户端可修 → 400。
		if errors.Is(err, store.ErrCapabilityInvalid) {
			slog.Warn("create capability rejected: invalid spec",
				"event", "console.caps.invalid_spec",
				"namespace_id", ns.ID, "name", req.Name, "error", err.Error())
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": err.Error(),
				"code":  "invalid_artifact",
			})
			return
		}
		// balance 不合法（未实现的 mode / 端口不在 outputs）：客户端可修 → 400
		if errors.Is(err, store.ErrBalanceRejected) {
			slog.Warn("create capability rejected: invalid balance",
				"event", "console.caps.balance_rejected",
				"namespace_id", ns.ID, "name", req.Name, "error", err.Error())
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": err.Error(),
				"code":  "balance_rejected",
			})
			return
		}
		// 事务内的产物构建失败（run 写错）：整体已回滚，能力一行都没留下
		var buildErr *store.ArtifactBuildError
		if errors.As(err, &buildErr) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": buildErr.Error(),
				"code":  "invalid_artifact",
			})
			return
		}
		// 其余（同 name@version 换内容 / 同内容已有）由 store 区分，这里统一按冲突处理
		slog.Warn("create capability rejected",
			"event", "console.caps.create_rejected",
			"name", req.Name, "version", c.Version, "error", err.Error())
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": err.Error(),
			"code":  "capability_conflict",
		})
		return
	}

	resp := map[string]any{
		"capability": c,
		"kind":       h.capabilityKind(r.Context(), c),
		// 影响面以**事务内**算出的为准（与写入同一个快照）；空影响面也是 [] 而不是 null。
		"upgradeImpact": res.Impact,
		// links 是建成的边（现有 edge 视图：带两端引用此刻解析到的能力 id）。
		// 空列表也是 []（不变量 I9）：前端不必判空就能遍历。
		"links": h.edgeViews(r.Context(), res.Edges),
	}
	if res.Artifact != nil {
		resp["artifact"] = res.Artifact
		resp["digest"] = digest
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------- POST /v1/capabilities/batch（契约 §4）----------

// capabilityBatchRequest 是 POST /v1/capabilities/batch 的请求体。
//
// `Capabilities` 的元素类型**就是**单建的请求体（`capabilityListRequest`）：
// 不另造一份 decode 结构。单建以后加字段（例如 balance），批量会自动跟着有——
// 造第二份结构的代价就是「同一个字段在一个端点生效、在另一个端点静默丢弃」。
//
// 三个字段随元素一起被解码（因为元素形状与单建一致），但**语义在批级**：
//
//	元素里的 namespaceId 与批级冲突 → 400（不静默采用其中一个）
//	元素里的 links       → 400（边只能走批级 links；静默丢弃等于用户以为建了）
//	元素里的 force       → 与批级 force 取或（闸门是整批的，任一处声明都算数）
type capabilityBatchRequest struct {
	NamespaceID  string                  `json:"namespaceId"`
	Capabilities []capabilityListRequest `json:"capabilities"`
	// Links 是一条表单里声明的全部下游边：两端都是**引用**，可以指向本批新建的名字
	// （能力行先插，边后解析）。`priority` 见 store.BatchLinkSpec 的 SEAM 说明。
	Links []store.BatchLinkSpec `json:"links,omitempty"`
	// Force 越过**整批**的升级闸门（与单建同语义）。
	Force bool `json:"force,omitempty"`
}

// createCapabilitiesBatch 在一个事务里建多个能力 + 产物 + 边（契约 §4）。
//
// 为什么需要它：一个表单里常常要同时定义 a 和 b 再把 a→b 接起来。逐条调用单建时，
// 中间任何一步失败都会留下半成品（能力表 append-only，撤不回来），而「a 指向 b」这种
// 互相依赖的写法在逐条路径上甚至无法表达（先建的指向后建的）。
//
// 与单建的分工是**分层**而不是分叉：校验 / 闸门 / 幂等折叠全部复用同一套
// （store.createCapabilityTx / checkUpgradeOn / createEdgeTx），这里只负责：
//
//	① 逐元素 decode 并把「单建的请求体」翻成 CapabilityVersion（同一套默认值与版本规则）
//	② 元素里的批级字段收口（namespaceId 冲突 / 内嵌 links / force 取或）
//	③ 把「怎么构建产物」包成回调（归一化之后才算得对 digest，见 ArtifactBuilder）
//	④ 把 store 的拒绝翻成 4xx（带下标，能直接定位是第几条）
func (h *consoleHandler) createCapabilitiesBatch(w http.ResponseWriter, r *http.Request) {
	// 批量的 body 里是 N 份能力定义（单建的上限是 1 MiB/次），所以上限按「批」放宽。
	var req capabilityBatchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if len(req.Capabilities) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "capabilities is required（批量创建至少要有一个能力）"})
		return
	}
	// 写端点：`*` → 400（一批能力定义只能属于一个命名空间）
	ns, ok := h.resolveNamespace(w, r, req.NamespaceID, false)
	if !ok {
		return
	}

	force := req.Force
	caps := make([]*store.CapabilityVersion, 0, len(req.Capabilities))
	// builders 与 caps **按下标对齐**（store 侧会校验长度）：错位会把 A 的产物挂到 B 上。
	builders := make([]store.ArtifactBuilder, len(req.Capabilities))
	for i, item := range req.Capabilities {
		if strings.TrimSpace(item.Name) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("capabilities[%d]: name is required", i),
			})
			return
		}
		if itemNS := strings.TrimSpace(item.NamespaceID); itemNS != "" && itemNS != ns.ID {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("capabilities[%d]: namespaceId %q 与批级 %q 不一致（一批只能属于一个命名空间）", i, itemNS, ns.ID),
			})
			return
		}
		if len(item.Links) > 0 {
			// 不静默忽略：用户写在元素上的边不会有人替他建，而这种“看起来配好了”的
			// 半成功比一个 400 贵得多。
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("capabilities[%d]: links 必须放在批级 links（元素里的边无法引用本批新建的名字）", i),
			})
			return
		}
		force = force || item.Force

		version := item.Version
		if version <= 0 {
			// 不指定版本时自动开下一个版本（append-only，不变量 I8）——与单建同一口径。
			next, err := h.db.NextCapabilityVersion(r.Context(), ns.ID, item.Name)
			if err != nil {
				if handleNamespaceError(w, err) {
					return
				}
				slog.Error("next capability version failed",
					"event", "console.caps.batch_next_failed",
					"namespace_id", ns.ID, "name", item.Name, "error", err.Error())
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
			version = next
		}

		schema := item.ParamsSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		source := item.Source
		if source == "" {
			source = store.CapSourceCustom
		}
		c := &store.CapabilityVersion{
			NamespaceID:  ns.ID,
			Name:         item.Name,
			DisplayName:  strings.TrimSpace(item.DisplayName),
			Version:      version,
			ParamsSchema: schema,
			Outputs:      item.Outputs,
			Sticky:       item.Sticky,
			Source:       source,
		}
		if item.Lifecycle != nil {
			c.Lifecycle = *item.Lifecycle
		}

		// 正文（spec）在这里组装，口径与单建逐字一致（见 createCapability）：
		// 有 run → 产物正文；没有 run → handler: noop 占位。
		//
		// 产物必须在**建能力之前**先校验一遍（与单建同一个理由）：一份 run 写错的
		// 请求不该留下一个永远跑不起来的定义。这里只校验、不落库。
		if item.Run != nil {
			built, err := buildArtifact(c, item.Run, item.Requires)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": fmt.Sprintf("capabilities[%d]: %v", i, err),
					"code":  "invalid_artifact",
				})
				return
			}
			c.Spec = built.Spec
			// 落库的产物与能力行必须同源：用事务内**归一后**那一行的正文重建
			// （幂等重放时版本号可能来自已有行）。
			builders[i] = func(authoritative *store.CapabilityVersion) (*store.Artifact, error) {
				art := authoritative.Artifact()
				row, _, berr := newStoreArtifact(authoritative.NamespaceID, authoritative.ID, &art)
				if berr != nil {
					return nil, berr
				}
				return row, nil
			}
		} else {
			c.Spec = store.HandlerSpec(artifact.HandlerNoop, schema, item.Outputs, specLifecycle(item.Lifecycle))
		}
		caps = append(caps, c)
	}

	res, err := h.db.CreateCapabilitiesBatch(r.Context(), caps, req.Links, builders, force)
	if err != nil {
		h.writeBatchError(w, ns, caps, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		// 能力行用**归一后**的那些（幂等折叠时 version/id 来自已有行），与单建的
		// `capability` 同形状。空列表不可能是 []（至少一个），但 edges 可以是 []。
		"capabilities": res.Capabilities,
		// edges 与单建的 `links` 同形状（带两端引用此刻解析到的能力 id）。
		"edges": h.edgeViews(r.Context(), res.Edges),
		// 影响面以**事务内**算出的为准（与写入同一个快照）；合并口径见 CapabilityBatchWrite.
		"upgradeImpact": res.Impact,
	})
}

// writeBatchError 把批量创建的 store 错误翻成 HTTP 响应。
//
// 分类与单建路径**逐条对齐**（同一类错误在两个端点上不能有不同的状态码），
// 唯一新增的是批量自己的两个哨兵：批内重名 / 空批（都是调用方可修的 400）。
func (h *consoleHandler) writeBatchError(w http.ResponseWriter, ns nsScope, caps []*store.CapabilityVersion, err error) {
	// 批内重名 / 空批：请求本身错了 → 400（文案里带下标，前端能直接高亮第几行）。
	if errors.Is(err, store.ErrBatchDuplicateName) {
		slog.Warn("create capabilities batch rejected: duplicate name",
			"event", "console.caps.batch_rejected", "namespace_id", ns.ID, "error", err.Error())
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": err.Error(),
			"code":  "duplicate_name",
		})
		return
	}
	if errors.Is(err, store.ErrBatchEmpty) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// 跨命名空间引用（I11）：客户端可修 → 400。
	// 不走 handleNamespaceError：那条路会写 CrossNamespaceError 自己的文案，
	// 而这里真正有用的是「第几条」（在外层 wrapper 的 Error() 里）。
	var cross *store.CrossNamespaceError
	if errors.As(err, &cross) {
		slog.Warn("create capabilities batch rejected: cross-namespace link",
			"event", "console.caps.batch_rejected", "namespace_id", ns.ID, "error", err.Error())
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "edge_rejected"})
		return
	}
	if handleNamespaceError(w, err) {
		return
	}
	// 闸门（整批合并后的影响面）：store 里还会再判一次，并发下预计算可能已过期。
	var breaking *store.UpgradeBreakingError
	if errors.As(err, &breaking) {
		slog.Warn("create capabilities batch rejected by upgrade gate",
			"event", "console.caps.batch_upgrade_rejected", "namespace_id", ns.ID)
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":         breaking.Error(),
			"code":          "upgrade_breaking",
			"upgradeImpact": breaking.Impact,
		})
		return
	}
	// 边不合法（目标不存在 / port 不在 outputs / literals 键不存在）：客户端可修 → 400，
	// 错误文案带 `links[i]` 下标。
	if errors.Is(err, store.ErrEdgeRejected) {
		slog.Warn("create capabilities batch rejected: invalid link",
			"event", "console.caps.batch_rejected", "namespace_id", ns.ID, "error", err.Error())
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "edge_rejected"})
		return
	}
	// 事务内的产物构建失败（run 写错）：整体已回滚，能力一行都没留下。
	var buildErr *store.ArtifactBuildError
	if errors.As(err, &buildErr) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": buildErr.Error(),
			"code":  "invalid_artifact",
		})
		return
	}
	// 其余（同 name@version 换内容 / 同内容已有 / 保留名）：与单建一样按冲突处理。
	names := make([]string, 0, len(caps))
	for _, c := range caps {
		names = append(names, c.Name)
	}
	slog.Warn("create capabilities batch rejected",
		"event", "console.caps.batch_create_rejected",
		"namespace_id", ns.ID, "names", names, "error", err.Error())
	writeJSON(w, http.StatusConflict, map[string]string{
		"error": err.Error(),
		"code":  "capability_conflict",
	})
}

// ---------- PUT /v1/capabilities/{name}/downstreams ----------

// downstreamsRequest 是 PUT /v1/capabilities/{name}/downstreams 的请求体。
//
// Links 用**指针**：`{"links":[]}` 是合法的（清空 = 终端能力），而**缺字段**
// 是请求写错了——把两者混成一种，任何一个漏传字段的客户端都会静默清掉拓扑。
type downstreamsRequest struct {
	// NamespaceID 是命名空间（query 里也可以给，query 优先）。
	NamespaceID string `json:"namespaceId"`
	// Links 是这组下游的全部（replace 语义，不是增量）。
	Links *[]store.DownstreamSpec `json:"links"`
}

// capabilityDownstreams 在**能力里**整体替换它的下游（CONTRACT-downstream-edit §2）。
//
// 这是能力边**唯一**的写入口：`POST /v1/edges` 的能力边分支已经封掉（405
// edge_write_removed），`DELETE /v1/edges` 只留作运维纠正。理由见契约 §0：
// 原来「建下游在能力页、删下游在出口页、改优先级两个地方都没有」，用户配了 3 个
// 下游想删 1 个就得换一页；而能力页还写着「点『编辑』可调优先级」——承诺了不存在的功能。
//
// 语义是 **replace**：提交的这组就是全部。空数组 = 清空。
//
// 响应带 `toCapabilityId` / `resolvedVersion`（引用此刻解析到的定义与版本），
// 让前端不必自己实现 name→latest —— 那是第二份解析语义，两边一漂就是
// 「页面说 v2、派发走了 v3」。
func (h *consoleHandler) capabilityDownstreams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		methodNotAllowed(w, "PUT")
		return
	}
	var req downstreamsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	// 写端点：`*` → 400（一组下游属于且只属于一个命名空间）。
	ns, ok := h.resolveNamespace(w, r, req.NamespaceID, false)
	if !ok {
		return
	}
	if req.Links == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "links is required（空数组 = 清空下游）",
			"code":  "links_required",
		})
		return
	}
	// 路径里是能力名（不是版本）：一个能力的全部版本共用同一组出边。
	name := store.RefName(strings.TrimSpace(r.PathValue("name")))
	if name == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "capability not found", "code": "not_found"})
		return
	}

	links, err := h.db.ReplaceCapabilityDownstreams(r.Context(), ns.ID, name, *req.Links)
	if err != nil {
		// 客户端可修的校验失败（带机器码，前端据此高亮是哪一行）。
		var rejected *store.DownstreamError
		if errors.As(err, &rejected) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": rejected.Error(),
				"code":  rejected.Code,
			})
			return
		}
		// 跨命名空间引用（I11）**必须排在 ErrNotFound 前面**：CrossNamespaceError
		// 内部也 Is(ErrNotFound)（目标在本命名空间确实"不存在"），先判 404 会把一个
		// 明确的"你引用了别的命名空间"报成"能力不存在"——用户照着改一辈子名字也修不好。
		if handleNamespaceError(w, err) {
			return
		}
		// 能力本身不存在：这个端点只改**已有能力**的出边。
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "capability not found", "code": "not_found"})
			return
		}
		// literals 不合法：与 POST /v1/edges 同一个哨兵、同一个机器码。
		if errors.Is(err, store.ErrEdgeRejected) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": err.Error(),
				"code":  "edge_rejected",
			})
			return
		}
		slog.Error("replace capability downstreams failed",
			"event", "console.caps.downstreams_failed",
			"namespace_id", ns.ID, "name", name, "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"name":  name,
		"links": links, // 与请求同序；空时是 [] 而不是 null（I9）
	})
}

// ---------- GET/POST/DELETE /v1/edges ----------

// edgeRequest 是 POST/DELETE /v1/edges 的请求体。
//
// 两端是**引用**（P2.2，设计 §3.2）：`name` 跟随 latest / `name@version` 钉死。
// 旧字段名（fromCapability / toCapability）**输入兼容一个版本**：值可以是
// 能力 id（老客户端）或引用（新客户端），store 统一把它解析成引用落库——
// **绝不把 id 存进边**（那是 P2.2 要消灭的形状）。
//
// POST 现在只接受**入口边**（fromIsSource=true）；能力边走
// PUT /v1/capabilities/{name}/downstreams（整体替换）。DELETE 仍两种都认。
type edgeRequest struct {
	// NamespaceID 是这条边所属的命名空间（query 里也可以给，query 优先）。
	NamespaceID string `json:"namespaceId"`
	// FromIsSource 为 true 时 FromRef 被解释为**来源名**（"github"），
	// 用于建「入口边」。
	//
	// POST 只接受 true：能力边（false）的写入口已迁到
	// PUT /v1/capabilities/{name}/downstreams（整体替换），旧路径返回 405
	// `edge_write_removed`。DELETE 仍然两种都认（运维纠正用）。
	FromIsSource bool   `json:"fromIsSource"`
	FromRef      string `json:"fromRef"`
	FromPort     string `json:"fromPort"`
	ToRef        string `json:"toRef"`

	// Deprecated: 旧字段名（P2.2 之前），只在 FromRef/ToRef 为空时兜底。
	// 下一次清理（前端/mock/e2e 那一波）连同这层兼容一起删。
	FromCapability string `json:"fromCapability"`
	ToCapability   string `json:"toCapability"`
	// Literals 是这条边的**常量覆盖**（设计 §3.3）：进入下游的 params = data ⊎ literals，
	// 字面量优先。不传 / {} = 没有覆盖（老客户端行为不变）。
	//
	// 用 json.RawMessage 而不是 map：原样交给 store（写进库的就是用户给的字节），
	// 也不用在这里再写一遍「是不是对象」——校验只有 store 一处（ErrEdgeRejected 透传）。
	Literals json.RawMessage `json:"literals"`
	// Mapping 是这条边的**引用模板**（P2.1，设计 §11.5）：进入下游的 params =
	// ResolveParams(mapping, 事件 payload) ⊎ literals。只有入口边（FromIsSource）允许非空，
	// 能力边带非空 mapping 会被 store 硬拒（code=edge_rejected，400）。
	Mapping json.RawMessage `json:"mapping"`
	// Priority 是这条边的**候选优先级**（契约 §3：小的优先，0 = 默认）。
	// 它只影响「同一个 (能力, 端口) 有多个下游时先选谁」，不影响这条边通不通。
	Priority int `json:"priority"`
}

// linkRefs 返回这条边的两端引用，**新字段名优先、旧字段名兜底**（P2.2 输入兼容）。
//
// 值本身可以是引用（`review` / `review@1`）也可以是能力 id：store.CreateEdge /
// DeleteEdge 统一归一（id → `name@version`）。这一层只决定「用哪个字段」，
// 不做任何解析——解析只有一处（store.resolveLinkRef）。
func (req edgeRequest) linkRefs() (from, to string) {
	from = firstNonEmpty(req.FromRef, req.FromCapability)
	to = firstNonEmpty(req.ToRef, req.ToCapability)
	return strings.TrimSpace(from), strings.TrimSpace(to)
}

// firstNonEmpty 返回第一个**非空**字符串（去空白后判断）。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// edgeListFilter 返回 GET /v1/edges 的「左端是谁」过滤值。
//
// 同时认 `fromRef`（新）与 `fromCapability`（旧）：控制台的链接口从「按能力 id
// 过滤」改成「按引用过滤」，两个名字并存一个版本期，谁先升级都不会查不到。
func edgeListFilter(r *http.Request) string {
	q := r.URL.Query()
	return firstNonEmpty(q.Get("fromRef"), q.Get("fromCapability"))
}

// edges 处理出口表：GET（列表）/ POST（只建**入口边**）/ DELETE（两种边都认）。
//
// 能力边的写在 PUT /v1/capabilities/{name}/downstreams（整体替换）：
// 能力的下游只在能力里改（CONTRACT-downstream-edit §0/§3）。
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
		got, err := h.db.ListEdges(r.Context(), nsID, edgeListFilter(r))
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
		"edges":       h.edgeViews(r.Context(), edges),
		"namespaceId": namespaceLabel(ns),
	})
}

// edgeView 是 GET /v1/edges 的一行：边本身 + 两端引用**此刻**解析到的能力 id。
//
// 为什么要有派生字段：边里的真值只有**引用**（`review` 跟随 latest / `review@2`
// 钉死）—— 存 id 的话能力一升级，原来那条边还指着旧 id，升级永远不生效（P2.2）。
// 但界面要「点进去看这一版」就必须知道**这个引用现在解析到哪一版**；让浏览器自己
// 算 name→latest 等于把解析语义复制一份到前端，两边一漂就是「页面说 review@2、
// 派发走了 review@3」。所以解析留在服务端（唯一实现：store.ResolveRef），
// 前端只读结果。
//
// 解析不到（引用指向的能力已被删）时 id 是**空串**而不是报错：一条坏边不该让整张
// 出口表变成错误页 —— 那种情况要显示成「这条边现在走不通」，而不是「页面挂了」。
type edgeView struct {
	*store.Edge

	// FromCapabilityID 是 fromRef 解析到的能力定义 id。
	// 入口边（FromIsSource=true）恒为空串：左端是**来源名**，没有「哪一版」。
	FromCapabilityID string `json:"fromCapabilityId"`
	// ToCapabilityID 是 toRef 解析到的能力定义 id；解析不到时为空串。
	ToCapabilityID string `json:"toCapabilityId"`
}

// edgeViews 给每条边补上「两端引用此刻解析到的能力 id」。
//
// 同一个 (命名空间, 引用) 只解析一次：出口表动辄几十行，而引用高度重复
// （同一个能力出现在多条边上），逐行 ResolveRef 会变成几十次往返。
// 解析本身仍只走 store.ResolveRef —— 这里不另写一份 name→latest。
func (h *consoleHandler) edgeViews(ctx context.Context, edges []*store.Edge) []edgeView {
	out := make([]edgeView, 0, len(edges))
	cache := make(map[string]string, len(edges))
	for _, e := range edges {
		v := edgeView{Edge: e}
		if !e.FromIsSource {
			v.FromCapabilityID = h.resolveRefID(ctx, cache, e.NamespaceID, e.FromRef)
		}
		v.ToCapabilityID = h.resolveRefID(ctx, cache, e.NamespaceID, e.ToRef)
		out = append(out, v)
	}
	return out
}

// resolveRefID 把一个引用解析成能力定义 id；解析不到返回空串。
//
// cache 的键带上命名空间：同一串引用（`lint@1`）在不同命名空间是**不同定义**
// （I11），拿错作用域会拼出一个看似合理、实际指错版本的 id。
func (h *consoleHandler) resolveRefID(ctx context.Context, cache map[string]string, nsID, ref string) string {
	key := nsID + "\x00" + ref
	if id, ok := cache[key]; ok {
		return id
	}
	id := ""
	if strings.TrimSpace(ref) != "" {
		c, err := h.db.ResolveRef(ctx, nsID, ref)
		switch {
		case err == nil:
			id = c.ID
		case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrRefUnresolved):
			// 数据事实：这个引用现在指向不到任何版本（能力被删/名字改了）。
			// 保持空串，让界面如实显示「解析不到」，而不是让整张表 500。
		default:
			slog.Warn("resolve edge ref failed",
				"event", "console.edges.resolve_ref_failed",
				"namespace_id", nsID, "ref", ref, "error", err.Error())
		}
	}
	cache[key] = id
	return id
}

// createEdge 写入一条**入口边**（fromIsSource=true）。
//
// 能力边（fromIsSource=false）的写入口已经收掉：能力的下游只能在能力里改
// （PUT /v1/capabilities/{name}/downstreams，整体替换）——见 CONTRACT-downstream-edit §3。
// 那条分支返回 405 `edge_write_removed`，而不是在每个客户端里再写一遍
// 「先删后建」的拼装逻辑（拼错了就是一条谁也发现不了的半条线）。
// DELETE /v1/edges 保留（运维纠正用），GET 保留（图 / 详情 / trace 都靠它）。
//
// 入口边的校验与以前一致：
//  1. store 层：两端引用要能解析（`name` / `name@version`；也接受能力 id 做输入兼容）、
//     两端同命名空间（契约 I2 / I11）；
//     literals 必须是对象、键 ∈ 目标 params_schema、类型精确相等（设计 §5 定义期（存 link））
//  2. 这里把 store 的拒绝翻译成 400（而不是 500）
//
// 注意边里**只存引用**（`fromRef`/`toRef`）：传 id 也能建，但落库的是
// `name@version`——P2.2 之后 id 不再是 link 的一部分。
func (h *consoleHandler) createEdge(w http.ResponseWriter, r *http.Request) {
	var req edgeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	// 写端点：`*` → 400（一条边属于且只属于一个命名空间）
	ns, ok := h.resolveNamespace(w, r, req.NamespaceID, false)
	if !ok {
		return
	}
	// 能力边：405。**故意**回 405 而不是 400/410：这个方法在当前资源上不再提供
	// 那个语义，而入口边仍然走同一个 POST —— 客户端可以按 fromIsSource 区分。
	// 放在命名空间校验之后：`namespaceId=*` / 缺参这种明显写错的请求先得到 400，
	// 不让人误以为「换掉 fromIsSource 就能过」。
	//
	// 不设 `Allow` 头：POST 在这个资源上**是**允许的（入口边），只是不再提供
	// 「建能力边」这个语义 —— 写一个 `Allow: GET, POST, DELETE` 只会让客户端以为
	// 「改个 body 就能过」。迁移指引放在 `error` 里。
	if !req.FromIsSource {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"code":  "edge_write_removed",
			"error": "能力的下游请在能力里改（PUT /v1/capabilities/{name}/downstreams）",
		})
		return
	}
	fromRef, toRef := req.linkRefs()
	if fromRef == "" || req.FromPort == "" || toRef == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "fromRef, fromPort, toRef are all required (legacy fromCapability/toCapability accepted)",
		})
		return
	}
	e := &store.Edge{
		NamespaceID:  ns.ID,
		FromIsSource: req.FromIsSource,
		FromRef:      fromRef,
		FromPort:     req.FromPort,
		ToRef:        toRef,
		Literals:     req.Literals,
		Mapping:      req.Mapping,
		Priority:     req.Priority,
	}
	if err := h.db.CreateEdge(r.Context(), e); err != nil {
		// I11：两端能力必须同命名空间，跨命名空间引用 → 400
		if handleNamespaceError(w, err) {
			return
		}
		if errors.Is(err, store.ErrEdgeRejected) {
			// from_port 不在 outputs 里 / 能力不存在 / literals 不合法——都是客户端可修的错误。
			// err.Error() 里带着具体原因（哪个键不存在、期望什么类型），直接透传。
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
	from := firstNonEmpty(q.Get("fromRef"), q.Get("fromCapability"))
	port := q.Get("fromPort")
	to := firstNonEmpty(q.Get("toRef"), q.Get("toCapability"))
	if from == "" {
		from = req.FromRef
	}
	if port == "" {
		port = req.FromPort
	}
	if to == "" {
		to = req.ToRef
	}
	if from == "" || to == "" {
		// 旧字段名兜底（值可能是 id，store 会归一成引用再删）
		legacyFrom, legacyTo := req.linkRefs()
		if from == "" {
			from = legacyFrom
		}
		if to == "" {
			to = legacyTo
		}
	}
	fromIsSource := req.FromIsSource
	if v := q.Get("fromIsSource"); v != "" {
		fromIsSource = v == "1" || v == "true"
	}
	if from == "" || port == "" || to == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "fromRef, fromPort, toRef are all required (legacy fromCapability/toCapability accepted)",
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
			capViews = append(capViews, nodeCapView{
				ID:          c.ID,
				Name:        c.Name,
				Version:     c.Version,
				DisplayName: c.DisplayName,
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
		// 控制台在**点之前**就要知道目标版本（CONTRACT-agent-upgrade §5.1）：
		// 没有它，「同版本 → 按钮显示已是最新」要额外再问一次服务端；空串 = 服务端
		// 没带二进制（UI 不显示升级按钮，或显示「服务端未提供」）。
		"targetAgentVersion": h.targetAgentVersion,
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

// capabilityRun 返回一个能力「怎么跑」的只读投影（产物 spec 里的 run 块）。
//
// 读不到（还没产物 / spec 坏了）返回 nil：这是**正常状态**（刚定义、还没部署），
// 不该让整个能力列表 500——列表页的价值在于"一眼看到全部定义"，一条坏行不能把它带崩。
//
// 每个能力一次查询：列表本来就有同样量级的 N 次查询（谁声明了它），
// 这里不额外引入批量接口；等能力数上千再优化。
// capabilityRunView 是列表里「怎么跑」的**瘦投影**：不含 files。
//
// 为什么必须剥掉 files：artifact.Run 的 files 没有 omitempty，直接把 Run 塞进
// 列表响应会让**每一行能力都带全部脚本正文**——列表页不需要它（详情区有专门的
// GET /v1/capabilities/{id}/artifact 懒加载），而一份几十个能力、每个几 KB 脚本的
// 列表会白涨几百 KB。
type capabilityRunView struct {
	Entrypoint string `json:"entrypoint"`
	TimeoutSec int    `json:"timeoutSec"`
	WorkDir    string `json:"workDir"`
}

func (h *consoleHandler) capabilityRun(ctx context.Context, c *store.CapabilityVersion) *capabilityRunView {
	a, err := h.db.LatestArtifact(ctx, c.NamespaceID, c.ID)
	if err != nil || a == nil {
		return nil
	}
	// artifacts.spec 里存的是**整份产物**的 canonical JSON
	// （artifact.CanonicalJSON(a)：{apiVersion, kind, metadata, spec:{…}}），
	// run 在 artifact.Spec.Run 下面，不在顶层。解错一层不会报错——只会得到
	// 一堆零值，于是详情页悄无声息地显示「—」。这里解 artifact.Artifact。
	var art artifact.Artifact
	if err := json.Unmarshal(a.Spec, &art); err != nil {
		return nil
	}
	// 产物里没有 run 块（内置中枢能力一类）：详情页显示「—」。
	// 不能直接比结构体（Run 里有 map，不可比较），看几个能代表「有没有」的字段。
	r := art.Spec.Run
	if r.Entrypoint == "" && r.TimeoutSec == 0 && r.WorkDir == "" && len(r.Files) == 0 {
		return nil
	}
	return &capabilityRunView{Entrypoint: r.Entrypoint, TimeoutSec: r.TimeoutSec, WorkDir: r.WorkDir}
}
