package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
)

// consoleHandler 是控制台 API 的处理器（契约 §6.2 的 8 个新端点）。
//
// 组织维度：所有端点先确定 org_id，并且**必须**校验调用者是该组织成员
// （不变量 I10「一切租户数据先过 org_id」）。org 的来源见 resolveOrg。
type consoleHandler struct {
	db   *store.DB
	auth *auth.Service
}

// ---------- 组织解析（I10 的唯一入口） ----------

// resolveOrg 确定这次请求作用于哪个组织，并校验调用者有权限。
//
// 三种来源，优先级从高到低：
//
//  1. query/body 里的 orgId —— 必须校验成员身份
//  2. X-Org-Id 头          —— 必须校验成员身份
//  3. 调用者的主组织        —— 天然有权限（是它的成员）
//
// 返回 (orgID, ok)；ok=false 表示已经写过错误响应，调用方直接 return。
func (h *consoleHandler) resolveOrg(w http.ResponseWriter, r *http.Request, requested string) (string, bool) {
	userID, ok := h.sessionUser(w, r)
	if !ok {
		return "", false
	}
	if requested == "" {
		requested = r.Header.Get("X-Org-Id")
	}
	if requested == "" {
		orgID, err := h.db.PrimaryOrgOf(r.Context(), userID)
		if err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "no org specified and user has no primary org",
			})
			return "", false
		}
		return orgID, true
	}
	member, err := h.db.IsOrgMember(r.Context(), requested, userID)
	if err != nil {
		slog.Error("org membership check failed", "event", "console.org.check_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return "", false
	}
	if !member {
		// 不区分「组织不存在」与「你不是成员」——避免探测组织 ID
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "not a member of this org"})
		return "", false
	}
	return requested, true
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

// ---------- GET /v1/tasks ----------

// taskListHandler 处理 GET /v1/tasks（按 trace/node/status 过滤）。
func (h *consoleHandler) tasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	orgID, ok := h.resolveOrg(w, r, r.URL.Query().Get("orgId"))
	if !ok {
		return
	}
	q := r.URL.Query()
	limit := intParam(q.Get("limit"), 100, 1, 1000)

	tasks, err := h.db.ListTasks(r.Context(), orgID, q.Get("traceId"), q.Get("nodeId"), q.Get("status"), limit)
	if err != nil {
		slog.Error("list tasks failed", "event", "console.tasks.list_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if tasks == nil {
		tasks = []*store.Task{} // 空列表是 [] 不是 null（I9）
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
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
	orgID, ok := h.resolveOrg(w, r, r.URL.Query().Get("orgId"))
	if !ok {
		return
	}
	q := r.URL.Query()
	limit := intParam(q.Get("limit"), 100, 1, 1000)

	events, err := h.db.ListEvents(r.Context(), orgID, q.Get("traceId"), q.Get("source"), limit)
	if err != nil {
		slog.Error("list events failed", "event", "console.events.list_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if events == nil {
		events = []*store.Event{} // 空列表是 [] 不是 null（I9）
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
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
	orgID, ok := h.resolveOrg(w, r, r.URL.Query().Get("orgId"))
	if !ok {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing task id"})
		return
	}

	task, err := h.db.GetTask(r.Context(), orgID, id)
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
	chain, err := h.db.ListTasks(r.Context(), orgID, task.TraceID, "", "", 100)
	if err != nil {
		chain = []*store.Task{task}
	}
	if chain == nil {
		chain = []*store.Task{} // 不变量 I9
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": task, "chain": chain})
}

// ---------- POST /v1/tasks/{id}/cancel ----------

// cancelTask 处理 POST /v1/tasks/{id}/cancel。
func (h *consoleHandler) cancelTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	orgID, ok := h.resolveOrg(w, r, r.URL.Query().Get("orgId"))
	if !ok {
		return
	}
	taskID := strings.TrimPrefix(r.URL.Path, "/v1/tasks/")
	taskID = strings.TrimSuffix(taskID, "/cancel")
	if taskID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task id required"})
		return
	}

	// 先确认任务存在且属于本组织（GetTask 内部已经过 org_id）
	task, err := h.db.GetTask(r.Context(), orgID, taskID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}
	if err != nil {
		slog.Error("get task failed", "event", "console.tasks.get_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if err := h.db.CancelTask(r.Context(), orgID, taskID); err != nil {
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
	updated, err := h.db.GetTask(r.Context(), orgID, taskID)
	if err != nil {
		updated = task
		updated.Status = store.TaskCanceled
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": updated})
}

// ---------- GET/POST /v1/capabilities ----------

// capabilityListRequest 是 POST /v1/capabilities 的请求体（camelCase）。
type capabilityListRequest struct {
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
	orgID, ok := h.resolveOrg(w, r, r.URL.Query().Get("orgId"))
	if !ok {
		return
	}
	var req struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is required"})
		return
	}
	// 先确认这个能力属于本组织（不变量 I10），顺便拿到更新后的行回给前端
	if _, err := h.db.GetCapabilityByID(r.Context(), orgID, req.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "capability not found", "code": "not_found"})
			return
		}
		slog.Error("get capability for patch failed", "event", "console.caps.patch_get_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := h.db.SetCapabilityDisplayName(r.Context(), orgID, req.ID, strings.TrimSpace(req.DisplayName)); err != nil {
		slog.Error("set capability display name failed", "event", "console.caps.patch_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	updated, err := h.db.GetCapabilityByID(r.Context(), orgID, req.ID)
	if err != nil {
		slog.Error("reload capability after patch failed", "event", "console.caps.patch_reload_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"capability": updated})
}

func (h *consoleHandler) listCapabilities(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.resolveOrg(w, r, r.URL.Query().Get("orgId"))
	if !ok {
		return
	}
	caps, err := h.db.ListCapabilities(r.Context(), orgID)
	if err != nil {
		slog.Error("list capabilities failed", "event", "console.caps.list_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if caps == nil {
		caps = []*store.CapabilityVersion{}
	}

	// 附带推导出来的 kind（不落库，不变量 I3）
	type capView struct {
		*store.CapabilityVersion
		Kind route.Kind `json:"kind"`
	}
	// 每种能力是否有节点声明
	declared := make(map[string]bool, len(caps))
	for _, c := range caps {
		nodes, err := h.db.NodesForCapability(r.Context(), orgID, c.ID)
		if err != nil {
			slog.Error("nodes for capability failed", "event", "console.caps.nodes_failed", "error", err.Error())
			continue
		}
		declared[c.ID] = len(nodes) > 0
	}
	views := make([]capView, 0, len(caps))
	for _, c := range caps {
		views = append(views, capView{
			CapabilityVersion: c,
			Kind:              route.KindForCapability(c, declared[c.ID]),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"capabilities": views})
}

func (h *consoleHandler) createCapability(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.resolveOrg(w, r, r.URL.Query().Get("orgId"))
	if !ok {
		return
	}
	var req capabilityListRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}

	version := req.Version
	if version <= 0 {
		// 不指定版本时自动开下一个版本（append-only，不变量 I8）
		next, err := h.db.NextCapabilityVersion(r.Context(), orgID, req.Name)
		if err != nil {
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
		OrgID:        orgID,
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
	orgID, ok := h.resolveOrg(w, r, r.URL.Query().Get("orgId"))
	if !ok {
		return
	}
	edges, err := h.db.ListEdges(r.Context(), orgID, r.URL.Query().Get("fromCapability"))
	if err != nil {
		slog.Error("list edges failed", "event", "console.edges.list_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if edges == nil {
		edges = []*store.Edge{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"edges": edges})
}

// createEdge 写入一条边。
//
// 校验分两层：
//  1. store 层：from_port ∈ from_capability.outputs、两端同组织（契约 I2 / §1.2）
//  2. 这里把 store 的拒绝翻译成 400（而不是 500）
func (h *consoleHandler) createEdge(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.resolveOrg(w, r, r.URL.Query().Get("orgId"))
	if !ok {
		return
	}
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

	err := h.db.CreateEdge(r.Context(), &store.Edge{
		OrgID:          orgID,
		FromIsSource:   req.FromIsSource,
		FromCapability: req.FromCapability,
		FromPort:       req.FromPort,
		ToCapability:   req.ToCapability,
	})
	if err != nil {
		if errors.Is(err, store.ErrEdgeRejected) {
			// from_port 不在 outputs 里 / 跨组织 / 能力不存在——都是客户端可修的错误
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
	writeJSON(w, http.StatusOK, map[string]any{"edge": &store.Edge{
		OrgID: orgID, FromIsSource: req.FromIsSource,
		FromCapability: req.FromCapability,
		FromPort:       req.FromPort, ToCapability: req.ToCapability,
	}})
}

func (h *consoleHandler) deleteEdge(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.resolveOrg(w, r, r.URL.Query().Get("orgId"))
	if !ok {
		return
	}
	// DELETE 没有 body 是常态，允许从 query 取
	q := r.URL.Query()
	from, port, to := q.Get("fromCapability"), q.Get("fromPort"), q.Get("toCapability")
	fromIsSource := q.Get("fromIsSource") == "1" || q.Get("fromIsSource") == "true"
	if from == "" && r.Body != nil {
		var req edgeRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err == nil {
			from, port, to = req.FromCapability, req.FromPort, req.ToCapability
			fromIsSource = req.FromIsSource
		}
	}
	if from == "" || port == "" || to == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "fromCapability, fromPort, toCapability are all required",
		})
		return
	}
	if err := h.db.DeleteEdge(r.Context(), orgID, fromIsSource, from, port, to); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "edge not found"})
			return
		}
		slog.Error("delete edge failed", "event", "console.edges.delete_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// ---------- GET /v1/nodes + POST /v1/nodes/{id}/drain ----------

// nodes 处理 GET /v1/nodes（列表 + 在线状态）。
func (h *consoleHandler) nodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	orgID, ok := h.resolveOrg(w, r, r.URL.Query().Get("orgId"))
	if !ok {
		return
	}
	nodes, err := h.db.ListNodes(r.Context(), orgID)
	if err != nil {
		slog.Error("list nodes failed", "event", "console.nodes.list_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if nodes == nil {
		nodes = []*store.Node{}
	}

	// 附带每台节点的能力与实例数（控制台要展示「谁跑什么」）
	//
	// capabilities 给的是**对象**而不是 "name@version" 字符串：节点页要
	// 显示用户自己取的中文显示名（「分析 PR」比 analyze-pr@1 好读得多），
	// 而且标签要能点进能力详情，需要一个稳定的 id。
	type nodeCapView struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Version     int    `json:"version"`
		DisplayName string `json:"displayName"`
	}
	type nodeView struct {
		*store.Node
		Capabilities []nodeCapView `json:"capabilities"`
		Instances    int           `json:"instances"`
		Busy         int           `json:"busy"`
	}
	views := make([]nodeView, 0, len(nodes))
	for _, n := range nodes {
		caps, err := h.db.ListNodeCapabilities(r.Context(), n.ID)
		if err != nil {
			slog.Error("list node capabilities failed", "event", "console.nodes.caps_failed", "error", err.Error())
			caps = nil
		}
		capViews := make([]nodeCapView, 0, len(caps))
		for _, c := range caps {
			capViews = append(capViews, nodeCapView{
				ID:          c.ID,
				Name:        c.Name,
				Version:     c.Version,
				DisplayName: c.DisplayName,
			})
		}
		insts, err := h.db.ListInstances(r.Context(), n.ID)
		if err != nil {
			slog.Error("list node instances failed", "event", "console.nodes.insts_failed", "error", err.Error())
			insts = nil
		}
		busy := 0
		for _, in := range insts {
			if in.BusyTask != nil {
				busy++
			}
		}
		views = append(views, nodeView{
			Node: n, Capabilities: capViews, Instances: len(insts), Busy: busy,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": views})
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
	orgID, ok := h.resolveOrg(w, r, r.URL.Query().Get("orgId"))
	if !ok {
		return
	}
	nodeID := strings.TrimPrefix(r.URL.Path, "/v1/nodes/")
	nodeID = strings.TrimSuffix(nodeID, "/drain")
	if nodeID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "node id required"})
		return
	}

	node, err := h.db.GetNode(r.Context(), orgID, nodeID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "node not found"})
		return
	}
	if err != nil {
		slog.Error("get node failed", "event", "console.nodes.get_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := h.db.UpdateNodeStatus(r.Context(), orgID, nodeID, store.NodeStatusDraining); err != nil {
		slog.Error("drain node failed", "event", "console.nodes.drain_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	node.Status = store.NodeStatusDraining
	slog.Info("node draining", "event", "console.nodes.draining", "node_id", nodeID, "org_id", orgID)
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
