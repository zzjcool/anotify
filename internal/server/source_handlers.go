package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/store"
)

// consoleHandler 上的源相关字段在 mux.go 里注入（避免循环依赖：
// source.Scheduler 需要 route.Dispatcher，而 route 不依赖 server）。
//
// 这里只放 handler 方法；依赖通过结构体字段传入。
//
// 见 sourceHandler。

// sourceHandler 提供「源」的增删改查与手动触发。
//
// 源是事件的入口实例（cron / webhook / manual）。
// 它与能力的区分见 internal/source 包注释。
type sourceHandler struct {
	db        *store.DB
	commander *source.Trigger
	// resolveOrg 复用 consoleHandler 的组织解析（I10 的唯一入口）
	resolveOrg func(w http.ResponseWriter, r *http.Request, requested string) (string, bool)
}

// sourceRequest 是创建/更新源的请求体。
type sourceRequest struct {
	Name    string          `json:"name"`
	Kind    string          `json:"kind"`
	Config  json.RawMessage `json:"config"`
	Enabled *bool           `json:"enabled"`
	// Targets 支持两种写法，兼容简单调用方：
	//   ["cap_a", "cap_b"]                                ← 只要 ID，无参数
	//   [{"capabilityId":"cap_a","params":{"x":1}}, ...]   ← 带参数
	Targets []sourceTargetInput `json:"targets"`
}

// sourceTargetInput 是请求里的一个出口。
//
// 自定义 UnmarshalJSON 让同一个字段既能接字符串也能接对象：
// 大多数调用方（脚本、e2e）只关心"派给谁"、不关心参数，
// 强制它们写 {"capabilityId": "..."} 是没必要的摩擦。
type sourceTargetInput struct {
	CapabilityID string         `json:"capabilityId"`
	Params       map[string]any `json:"params,omitempty"`
}

func (t *sourceTargetInput) UnmarshalJSON(b []byte) error {
	// 先试字符串形式
	var id string
	if err := json.Unmarshal(b, &id); err == nil {
		t.CapabilityID = id
		return nil
	}
	// 再试对象形式
	type alias sourceTargetInput
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*t = sourceTargetInput(a)
	return nil
}

// sourceView 是返回给前端的源视图。
//
// 除库里存的字段外，附带：
//   - targetCaps：出口能力的可读信息（前端不用再拉一次能力列表去 join）
//   - hookUrl：webhook 的完整路径（可直接复制给外部系统）
type sourceView struct {
	*store.Source
	TargetCaps []sourceTargetView `json:"targetCaps"`
	HookURL    string             `json:"hookUrl,omitempty"`
}

type sourceTargetView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version int    `json:"version"`
	Missing bool   `json:"missing,omitempty"` // 能力已被删（源指向不存在的目标）

	// Params 是这个出口已配置的参数（触发时传给该能力）
	Params map[string]any `json:"params,omitempty"`
	// ParamsSchema 是该能力的参数 schema（前端据此生成表单）。
	// 带上它前端就不用再拉一次能力列表做 join。
	ParamsSchema json.RawMessage `json:"paramsSchema,omitempty"`
}

// validateTargetParams 逐个出口用该能力的 params_schema 校验参数。
//
// 为什么必须在建源时就校验，而不是等触发时让它被剪枝：
// 用户填完表单点保存，就该知道"channel 必须是 slack/email"；
// 等到触发才发现，只会看到"事件入库但没派出去"，而那时人已经离开页面了。
//
// 用的是 route.ValidateParams —— 与派发期**同一个函数**。两边口径必须一致，
// 否则会出现"建源说没问题、触发被剪枝"这种最让人困惑的情况。
func (h *sourceHandler) validateTargetParams(ctx context.Context, orgID string, targets []store.SourceTarget) error {
	for _, tg := range targets {
		if len(tg.Params) == 0 {
			continue // 没配参数：交给派发期的 schema 校验（可能该能力本来就不要参数）
		}
		capDef, err := h.db.GetCapabilityByID(ctx, orgID, tg.CapabilityID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("出口能力 %s 不存在", tg.CapabilityID)
			}
			return err
		}
		if msg := route.ValidateParams(capDef.ParamsSchema, tg.Params); msg != "" {
			return fmt.Errorf("%s@%d 的参数不合法：%s", capDef.Name, capDef.Version, msg)
		}
	}
	return nil
}

// toStoreTargets 把请求里的出口转成 store 类型。
func toStoreTargets(in []sourceTargetInput) []store.SourceTarget {
	out := make([]store.SourceTarget, 0, len(in))
	for _, t := range in {
		if t.CapabilityID == "" {
			continue
		}
		out = append(out, store.SourceTarget{
			CapabilityID: t.CapabilityID,
			Params:       t.Params,
		})
	}
	return out
}

// sources 处理 /v1/sources 的 GET 与 POST。
func (h *sourceHandler) sources(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.list(w, r)
	case http.MethodPost:
		h.create(w, r)
	default:
		methodNotAllowed(w, "GET, POST")
	}
}

// sourceByID 处理 /v1/sources/{id} 的 GET / PATCH / DELETE。
func (h *sourceHandler) sourceByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodGet:
		h.get(w, r, id)
	case http.MethodPatch:
		h.update(w, r, id)
	case http.MethodDelete:
		h.delete(w, r, id)
	default:
		methodNotAllowed(w, "GET, PATCH, DELETE")
	}
}

func (h *sourceHandler) list(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.resolveOrg(w, r, r.URL.Query().Get("orgId"))
	if !ok {
		return
	}
	list, err := h.db.ListSources(r.Context(), orgID)
	if err != nil {
		slog.Error("list sources failed", "event", "console.sources.list_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	views, err := h.views(r.Context(), orgID, list)
	if err != nil {
		slog.Error("build source views failed", "event", "console.sources.views_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// 空列表返回 []（不变量 I9）
	writeJSON(w, http.StatusOK, map[string]any{"sources": views})
}

func (h *sourceHandler) get(w http.ResponseWriter, r *http.Request, id string) {
	orgID, ok := h.resolveOrg(w, r, "")
	if !ok {
		return
	}
	src, err := h.db.GetSource(r.Context(), orgID, id)
	if err != nil {
		h.writeSourceErr(w, err, "get")
		return
	}
	views, err := h.views(r.Context(), orgID, []*store.Source{src})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"source": views[0]})
}

func (h *sourceHandler) create(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.resolveOrg(w, r, r.URL.Query().Get("orgId"))
	if !ok {
		return
	}
	var req sourceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if !store.IsValidSourceKind(req.Kind) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":     "kind 必须是 " + strings.Join(store.SourceKinds, " / "),
			"available": store.SourceKinds,
		})
		return
	}
	if len(req.Targets) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "targets 至少要选一个出口能力（否则这个源产生的事件永远派不出去）",
		})
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	src := &store.Source{
		OrgID:   orgID,
		Name:    strings.TrimSpace(req.Name),
		Kind:    req.Kind,
		Config:  req.Config,
		Enabled: enabled,
		Targets: toStoreTargets(req.Targets),
	}
	// 先校验参数再落库：填错了立刻告诉用户，别等触发时被剪枝
	if err := h.validateTargetParams(r.Context(), orgID, src.Targets); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := h.db.CreateSource(r.Context(), src); err != nil {
		h.writeSourceErr(w, err, "create")
		return
	}
	// 重新读一次：创建时算出来的 nextFireAt 只在库里，不在入参对象上
	fresh, err := h.db.GetSource(r.Context(), orgID, src.ID)
	if err == nil {
		src = fresh
	}
	views, err := h.views(r.Context(), orgID, []*store.Source{src})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"source": views[0]})
}

func (h *sourceHandler) update(w http.ResponseWriter, r *http.Request, id string) {
	orgID, ok := h.resolveOrg(w, r, "")
	if !ok {
		return
	}
	cur, err := h.db.GetSource(r.Context(), orgID, id)
	if err != nil {
		h.writeSourceErr(w, err, "update")
		return
	}
	var req sourceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if req.Name != "" {
		cur.Name = strings.TrimSpace(req.Name)
	}
	if req.Config != nil {
		cur.Config = req.Config
	}
	if req.Enabled != nil {
		cur.Enabled = *req.Enabled
	}
	if len(req.Targets) > 0 {
		cur.Targets = toStoreTargets(req.Targets)
	}
	if err := h.validateTargetParams(r.Context(), orgID, cur.Targets); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// kind 不可改：换种类等于换语义（见 store.UpdateSource 注释）
	if err := h.db.UpdateSource(r.Context(), cur); err != nil {
		h.writeSourceErr(w, err, "update")
		return
	}
	fresh, err := h.db.GetSource(r.Context(), orgID, id)
	if err == nil {
		cur = fresh
	}
	views, err := h.views(r.Context(), orgID, []*store.Source{cur})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"source": views[0]})
}

func (h *sourceHandler) delete(w http.ResponseWriter, r *http.Request, id string) {
	orgID, ok := h.resolveOrg(w, r, "")
	if !ok {
		return
	}
	if err := h.db.DeleteSource(r.Context(), orgID, id); err != nil {
		h.writeSourceErr(w, err, "delete")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// fire 处理 POST /v1/sources/{id}/fire：手动触发一次。
//
// 只有 manual 源该用它，但 cron/webhook 源手动跑一次也有调试价值，
// 所以不按 kind 拦截——真实用途由用户判断。
func (h *sourceHandler) fire(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	orgID, ok := h.resolveOrg(w, r, "")
	if !ok {
		return
	}
	id := r.PathValue("id")
	src, err := h.db.GetSource(r.Context(), orgID, id)
	if err != nil {
		h.writeSourceErr(w, err, "fire")
		return
	}
	if h.commander == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "源调度器未启用（server 未注入 dispatcher）",
		})
		return
	}

	// 允许调用方带自定义 payload；不带就给一个最小体
	var body struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)

	eventType := body.Type
	if eventType == "" {
		eventType = src.Name
	}
	res, err := h.commander.Fire(r.Context(), src, eventType, "", body.Payload)
	if err != nil {
		slog.Error("fire source failed",
			"event", "console.sources.fire_failed", "source_id", id, "error", err.Error())
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"event":      res.Event,
		"inserted":   res.Inserted,
		"idempotent": res.Idempotent,
		"dispatch":   res.Dispatch,
	})
}

// dryRun 处理 POST /v1/sources/{id}/dry-run 和 POST /v1/sources/dry-run。
//
// 两条路径的分工：
//   - /{id}/dry-run：拿已保存的源试运行
//   - /dry-run     ：**拿请求体里的草稿**试运行（还没保存的配置）
//
// 后者是主路径：用户改完参数、还没保存时最想试。如果只支持已保存的源，
// 就得"先保存坏的配置再试"——那正是要消灭的往返。
//
// 不检查 enabled，也不写事件：它是个只读推演。
func (h *sourceHandler) dryRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	orgID, ok := h.resolveOrg(w, r, "")
	if !ok {
		return
	}
	if h.commander == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "源调度器未启用（server 未注入 dispatcher）",
		})
		return
	}

	var body struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
		// Source 只在 /dry-run（无 id）时使用：前端把弹窗里的草稿原样发来
		Source *store.Source `json:"source"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)

	// 有 id 走已保存的源；没有就用请求体里的草稿
	var src *store.Source
	if id := r.PathValue("id"); id != "" {
		got, err := h.db.GetSource(r.Context(), orgID, id)
		if err != nil {
			h.writeSourceErr(w, err, "dry_run")
			return
		}
		src = got
	} else {
		if body.Source == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source is required"})
			return
		}
		src = body.Source
		src.OrgID = orgID // 草稿的组织以会话为准，不信请求体
	}

	out, err := h.commander.DryRun(r.Context(), src, body.Type, body.Payload)
	if err != nil {
		slog.Error("dry run source failed",
			"event", "console.sources.dry_run_failed", "error", err.Error())
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":  src.Enabled,
		"dispatch": out,
	})
}

// views 批量组装返回视图（出口能力信息 + webhook URL）。
func (h *sourceHandler) views(ctx context.Context, orgID string, list []*store.Source) ([]sourceView, error) {
	// 一次取全量能力，避免每个源都查一遍
	caps, err := h.db.ListCapabilities(ctx, orgID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*store.CapabilityVersion, len(caps))
	for _, c := range caps {
		byID[c.ID] = c
	}

	out := make([]sourceView, 0, len(list))
	for _, s := range list {
		v := sourceView{Source: s, TargetCaps: []sourceTargetView{}}
		for _, t := range s.Targets {
			if c, ok := byID[t.CapabilityID]; ok {
				v.TargetCaps = append(v.TargetCaps, sourceTargetView{
					ID: c.ID, Name: c.Name, Version: c.Version,
					Params: t.Params,
					// 带上 schema，前端才能按它生成参数表单
					ParamsSchema: c.ParamsSchema,
				})
			} else {
				// 能力被删了但源还指着它：如实标出来，别静默消失
				v.TargetCaps = append(v.TargetCaps, sourceTargetView{
					ID: t.CapabilityID, Missing: true, Params: t.Params,
				})
			}
		}
		if s.Kind == store.SourceKindWebhook || s.Kind == store.SourceKindGitHub {
			var wc store.WebhookConfig
			_ = json.Unmarshal(s.Config, &wc)
			if wc.Token != "" {
				v.HookURL = "/v1/hooks/" + wc.Token
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// writeSourceErr 把 store 的哨兵错误映射成合适的状态码。
func (h *sourceHandler) writeSourceErr(w http.ResponseWriter, err error, op string) {
	switch {
	case errors.Is(err, store.ErrSourceNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "源不存在"})
	case errors.Is(err, store.ErrSourceNameTaken):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "同名源已存在，换个名字"})
	case errors.Is(err, store.ErrInvalidSourceKind):
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":     err.Error(),
			"available": store.SourceKinds,
		})
	case errors.Is(err, store.ErrInvalidSourceConfig):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		slog.Error("source "+op+" failed",
			"event", "console.sources."+op+"_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

// hook 处理 POST /v1/hooks/{token}：外部系统投递事件。
//
// 鉴权就是 URL 里的 token 本身（源是 webhook 类型时才有）：
// 谁拿到这个 URL 谁就能往里发事件，所以 token 必须不可枚举、可轮换。
// 这正是「源」相对「API Key」的取舍——单个 URL 更好贴到外部系统的
// webhook 配置里，代价是它比 Key 更容易在日志/截图里泄漏。
//
// 事件类型：默认取源的名字；调用方可以用 body.type 覆盖，
// 这样同一入口能区分不同事件（如 GitHub 的 opened / synchronize）。
func (h *sourceHandler) hook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	token := r.PathValue("token")
	if token == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	src, err := h.db.GetSourceByWebhookToken(r.Context(), token)
	if err != nil {
		// 不区分"token 不存在"和"源已停用"：对外都是 404，
		// 避免用错误码差异探测 token 是否有效。
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body: " + err.Error()})
		return
	}

	// 两种投递格式，靠 kind 区分：
	//
	//   webhook —— 本系统自定义的包装体 {"type":"...","payload":{...}}。
	//              对接方是我们自己写脚本 / curl 的场景，怎么方便怎么来。
	//
	//   github  —— **GitHub 的原生格式**：body 就是 payload 本身，
	//              事件名在 X-GitHub-Event 请求头里。
	//              这里绝不能要求包装体：GitHub 不会迁就我们的格式，
	//              要求包装体等于这个源永远收不到任何事件。
	var payload json.RawMessage
	eventType := ""
	if src.Kind == store.SourceKindGitHub {
		eventType = strings.TrimSpace(r.Header.Get("X-GitHub-Event"))
		if eventType == "" {
			// 少了这个头就不是 GitHub 投递（可能是人手 curl 测试）。
			// 不拒绝：用配置里订阅的事件名兜底，方便用户手动调。
			var wc store.WebhookConfig
			if len(src.Config) > 0 {
				_ = json.Unmarshal(src.Config, &wc)
			}
			eventType = wc.Event
		}
		payload = raw
	} else {
		// 允许空 body：有些系统只发 ping。此时用最小 payload。
		if len(raw) > 0 {
			var body struct {
				Type    string          `json:"type"`
				Payload json.RawMessage `json:"payload"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
				return
			}
			eventType = strings.TrimSpace(body.Type)
			payload = body.Payload
		}
	}
	if eventType == "" {
		eventType = src.Name
	}
	if len(payload) == 0 || !json.Valid(payload) {
		payload = json.RawMessage(`{}`)
	}
	if h.commander == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "源调度器未启用"})
		return
	}
	res, err := h.commander.Fire(r.Context(), src, eventType, "", payload)
	if err != nil {
		slog.Error("webhook fire failed",
			"event", "console.sources.hook_failed", "source_id", src.ID, "error", err.Error())
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"event":    res.Event,
		"inserted": res.Inserted,
		"dispatch": res.Dispatch,
	})
}

// compile-time 断言：确保 route.Dispatcher 满足 source.Dispatcher。
var _ source.Dispatcher = (*route.Dispatcher)(nil)
