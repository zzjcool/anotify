package server

import (
	"bytes"
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
	// resolveNamespace 复用 consoleHandler 的命名空间解析（契约 §6.2 的唯一入口）。
	//
	// 源是**命名空间资源**（sources / source_targets 都带 namespace_id）：
	// 除 POST /v1/hooks/{token}（外部入口，token 自己就是凭据）
	// 外，每个方法都必须解析 namespaceId —— 缺一律 400，绝不回退默认命名空间。
	resolveNamespace func(w http.ResponseWriter, r *http.Request, orgID, bodyNS string, allowAll bool) (nsScope, bool)
}

// sourceRequest 是创建/更新源的请求体。
type sourceRequest struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// NamespaceID 是这个源属于哪个命名空间（**必填**，也可以放 query）。
	// 契约 §4 的取值顺序：query namespaceId → body namespaceId → 空（→ 400）。
	NamespaceID string          `json:"namespaceId"`
	Config      json.RawMessage `json:"config"`
	Enabled     *bool           `json:"enabled"`
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

// validateTargetParams 逐个出口做两件事：校验参数模板的**形式**，再用该能力的
// params_schema 校验**字面量**（契约 §2.4）。
//
// 两件事必须分开：
//
//  1. 形式校验（route.ValidateParamTemplates）：占位符闭合、路径形态、as 枚举、
//     from/var 互斥、{var} 必须已声明。全是"看一眼就能确定错"的输入问题，
//     配置期 400 是唯一能把话说清楚的位置（推到触发时就只剩一条剪枝）。
//  2. 类型校验（route.ValidateLiteralParams）：**只对字面量**做。含占位符的值类型
//     要求值后才知道，`"{{n}}"` 硬塞给 integer schema 会被误拒；映射声明
//     {from}/{var} 同理。它们的类型错由触发期的 param_type 报。
//
// 第 2 条**不能**用「剔掉占位符/映射后再拿字面子集跑 route.ValidateParams」实现（契约
// §8.5 A14）：那样 schema 的 required 会误伤合法配置——能力 schema 的
// required:["to","title"] 遇上 `{"to":"{{pr.user.login}}","title":"PR 已合并"}`，
// 剔完只剩 title，于是报 missing required "to" 400，而派发时求值后完全合法
// （dry-run 能过、点保存却 400，最伤信任）。正确的口径在 route 包里：required 按
// **模板键集合**判（键在即满足），只有完全解析得出的值才逐字段校验。
//
// 为什么必须在建源时就校验，而不是等触发时让它被剪枝：
// 用户填完表单点保存，就该知道"channel 必须是 slack/email"；
// 等到触发才发现，只会看到"事件入库但没派出去"，而那时人已经离开页面了。
//
// 用的是 route 包的函数 —— 与派发期同一套口径，否则会出现
// "建源说没问题、触发被剪枝"这种最让人困惑的情况。
//
// 能力按**源的命名空间**查（I11 第②条）：跨命名空间引用能力时，
// GetCapabilityByID 在本命名空间里查不到 → 400，与 store.CreateSource
// 的 checkCapabilityInNamespace 同一个结论（这里先拦住，报错文案更好读）。
//
// declaredVars 是源声明的变量名（config.vars）：`{var:"name"}` 是显式引用，
// 拼错必须当场 400；`{{name}}` 不要求声明（它可以是 payload 路径）。
func (h *sourceHandler) validateTargetParams(ctx context.Context, namespaceID string, targets []store.SourceTarget, declaredVars []string) error {
	for _, tg := range targets {
		if len(tg.Params) == 0 {
			continue // 没配参数：交给派发期的 schema 校验（可能该能力本来就不要参数）
		}
		capDef, err := h.db.GetCapabilityByID(ctx, namespaceID, tg.CapabilityID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("出口能力 %s 不存在", tg.CapabilityID)
			}
			return err
		}
		// ① 形式校验先于一切取值判断：半截占位符（"PR {{title"）绝不能被当字面量存下来
		if msg := route.ValidateParamTemplates(tg.Params, declaredVars); msg != "" {
			return fmt.Errorf("%s@%d 的参数不合法：%s", capDef.Name, capDef.Version, msg)
		}
		// ② 能确定的字面量当场按 schema 判（契约 §8.5 A14）：顶层 required 按**模板键
		// 集合**判（键在就满足，值是不是占位符不看）、只对完全解析得出的值逐字段校验、
		// 含占位符 / 映射声明的子树整体跳过（派发期 ValidateParams 兜底）。
		// 与派发期同一个 schema 引擎，只是输入换成了模板并让 required/未解析子树让路。
		if msg := route.ValidateLiteralParams(capDef.ParamsSchema, tg.Params); msg != "" {
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
	// 列表是**读**端点，允许 namespaceId=* （管理员跨命名空间汇总）。
	scope, ok := h.resolveNamespace(w, r, orgID, "", true)
	if !ok {
		return
	}
	list, err := h.listSources(r.Context(), orgID, scope)
	if err != nil {
		slog.Error("list sources failed", "event", "console.sources.list_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	views, err := h.views(r.Context(), list)
	if err != nil {
		slog.Error("build source views failed", "event", "console.sources.views_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// 空列表返回 []（不变量 I9）
	writeJSON(w, http.StatusOK, map[string]any{"sources": views})
}

// listSources 按命名空间作用域取源。
//
// scope.All（namespaceId=*）时遍历本组织的每个命名空间分别取，再按
// ListNamespaces 的顺序（created_at, id）拼接：合并结果必须是**确定性的**，
// 否则同一份数据每次刷新条序不同，前端的分页与对比都无法做。
func (h *sourceHandler) listSources(ctx context.Context, orgID string, scope nsScope) ([]*store.Source, error) {
	if !scope.All {
		return h.db.ListSources(ctx, scope.ID)
	}
	namespaces, err := h.db.ListNamespaces(ctx, orgID)
	if err != nil {
		return nil, err
	}
	out := []*store.Source{}
	for _, ns := range namespaces {
		part, err := h.db.ListSources(ctx, ns.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

func (h *sourceHandler) get(w http.ResponseWriter, r *http.Request, id string) {
	orgID, ok := h.resolveOrg(w, r, "")
	if !ok {
		return
	}
	// 单对象读：`*` 没有定义（一个源属于哪个命名空间？）→ 400
	scope, ok := h.resolveNamespace(w, r, orgID, "", false)
	if !ok {
		return
	}
	src, err := h.db.GetSource(r.Context(), scope.ID, id)
	if err != nil {
		h.writeSourceErr(w, err, "get")
		return
	}
	views, err := h.views(r.Context(), []*store.Source{src})
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
	// 写入端点：`*` 不允许（写一条源属于哪个命名空间？）→ 400
	scope, ok := h.resolveNamespace(w, r, orgID, req.NamespaceID, false)
	if !ok {
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
		NamespaceID: scope.ID,
		OrgID:       orgID,
		Name:        strings.TrimSpace(req.Name),
		Kind:        req.Kind,
		Config:      req.Config,
		Enabled:     enabled,
		Targets:     toStoreTargets(req.Targets),
	}
	// 变量声明先过配置期校验（契约 §2.4）：非法名 / 重名 / from+value 并存 /
	// 路径写成 `a..b`，这些在触发时只会变成"参数取不到"的剪枝。
	// 本行必须在 validateTargetParams **之前**：后者要用变量名判断
	// `{var:"name"}` 是否已声明。
	if msg := store.ValidateVarDefs(src.VarDefs()); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	// 先校验参数再落库：填错了立刻告诉用户，别等触发时被剪枝
	if err := h.validateTargetParams(r.Context(), scope.ID, src.Targets, src.VarNames()); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := h.db.CreateSource(r.Context(), src); err != nil {
		h.writeSourceErr(w, err, "create")
		return
	}
	// 重新读一次：创建时算出来的 nextFireAt 只在库里，不在入参对象上
	fresh, err := h.db.GetSource(r.Context(), scope.ID, src.ID)
	if err == nil {
		src = fresh
	}
	views, err := h.views(r.Context(), []*store.Source{src})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"source": views[0]})
}

// inheritedConfigKeys 是 PATCH 语义下需要从旧配置继承的键。
//
// 只有这两个：它们由**另一个入口**写入（变量声明走源详情/接口，事件打底数据
// 走 cron/manual 的高级表单），控制台改 expr/token 的那次保存不会带上它们。
// 别的键（expr/tz/token/event）都是「这次保存就是它的全部真相」，照常覆盖。
var inheritedConfigKeys = []string{"vars", "payload"}

// isJSONNull 判断 raw 是否就是 JSON 的 null（PATCH 请求里的 {"config": null}）。
//
// 解码成 json.RawMessage 的 null **不是** nil 切片，而是字面量 "null"，所以
// `req.Config != nil` 拦不住它，得单独看一眼（契约 §8.5 A18）。
func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// mergeSourceConfig 在 PATCH 语义下合并 config：新配置未带某键时继承旧值。
//
// 为什么不是整份替换：config.vars / config.payload 是“另一个入口”写入的声明，
// 控制台保存 expr/token 时不会带上它们；整份替换等于静默丢数据（设计 A.5 也点名了这个坑）。
// 显式传 "vars": [] 或 "payload": {} 才是真的清空。
func mergeSourceConfig(cur, next json.RawMessage) json.RawMessage {
	// next 为空 / 非法 JSON：没有可合并的新配置，按“本次不改 config”处理。
	if !json.Valid(next) {
		return cur
	}
	// 只有 JSON 对象才谈得上“某个键有没有带”：null / 数组 / 标量都不是 config，
	// 在合并这一步我们不擅自解释它（否则 null 会被当成“保留 vars、清掉其余”，
	// 那是猜出来的语义）。
	if trimmed := bytes.TrimSpace(next); len(trimmed) == 0 || trimmed[0] != '{' {
		return cur
	}
	// 用 map[string]json.RawMessage 而不是 struct：config 里将来可能长出新键，
	// 解成 struct 会把未知键整个丢掉（这正是要避免的静默丢数据）。
	// RawMessage 保住原始字节，不做“读出来再写回去”的重编码。
	nextFields := map[string]json.RawMessage{}
	if err := json.Unmarshal(next, &nextFields); err != nil {
		// next 不是 JSON 对象（数组 / 标量 / null）：不是能合并的 config，
		// 交回旧配置，别在合并这一步擅自决定语义。
		return cur
	}
	curFields := map[string]json.RawMessage{}
	if len(cur) > 0 && json.Valid(cur) {
		// 旧配置坏掉时当“没有旧配置”：不能让一次合并把请求也弄失败。
		_ = json.Unmarshal(cur, &curFields)
	}

	merged := make(map[string]json.RawMessage, len(nextFields)+len(inheritedConfigKeys))
	for k, v := range nextFields {
		merged[k] = v
	}
	inherited := false
	for _, k := range inheritedConfigKeys {
		if _, has := merged[k]; has {
			continue // next 显式带了（哪怕 [] / {}）：尊重调用方的清空意图
		}
		if v, ok := curFields[k]; ok {
			merged[k] = v
			inherited = true
		}
	}
	if !inherited {
		// 没有可继承的键：原样交回 next，避免一次无谓的重编码
		return next
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return next
	}
	return out
}

func (h *sourceHandler) update(w http.ResponseWriter, r *http.Request, id string) {
	orgID, ok := h.resolveOrg(w, r, "")
	if !ok {
		return
	}
	// 先解 body 再解命名空间：body 里的 namespaceId 是 §4 规定的第二优先取值。
	var req sourceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	scope, ok := h.resolveNamespace(w, r, orgID, req.NamespaceID, false)
	if !ok {
		return
	}
	cur, err := h.db.GetSource(r.Context(), scope.ID, id)
	if err != nil {
		h.writeSourceErr(w, err, "update")
		return
	}
	if req.Name != "" {
		cur.Name = strings.TrimSpace(req.Name)
	}
	// config 显式传 JSON null 视为「本次不改 config」（契约 §8.5 A18）：
	// mergeSourceConfig 恰好也原样返回旧配置，但这条语义必须在这里写死——
	// 否则以后有人顺手把 null 当"清空 config"实现，一次带 null 的保存就会
	// 连带抹掉 config.vars / config.payload（正是 A6 那个坑）。
	if req.Config != nil && !isJSONNull(req.Config) {
		// 不能整份替换：req.Config 通常只带 expr/token，整份替换会把
		// config.vars / config.payload 静默抹掉，紧接着下面用被清空的
		// VarNames() 校验既有 targets，含 {var:"x"} 的源连改个 cron
		// 表达式都会 400（issue #114 A6）。merge 保留“没带的键”。
		cur.Config = mergeSourceConfig(cur.Config, req.Config)
	}
	if req.Enabled != nil {
		cur.Enabled = *req.Enabled
	}
	if len(req.Targets) > 0 {
		cur.Targets = toStoreTargets(req.Targets)
	}
	// 变量声明与参数模板都要用**合并后的** cur.Config 校验：PATCH 只带
	// 部分字段时 req.Config 可能是 nil，读它会把"用户没改变量"误判成
	// "变量全没了"。
	//
	// cur.Config 确实是“本次请求之后的最终配置”——但前提是上面那次赋值走了
	// mergeSourceConfig（没带的 vars/payload 从旧配置继承）。若哪天有人把合并
	// 换回整份替换，这两行校验就会拿被清空的配置去验既有 targets，于是
	// {var:"x"} 变成“未声明的变量”：改 cron 表达式也会 400。
	// 回归用例见 source_handlers_test.go 的 TestPatchSourceKeepsConfigVarsHTTP。
	if msg := store.ValidateVarDefs(cur.VarDefs()); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	if err := h.validateTargetParams(r.Context(), scope.ID, cur.Targets, cur.VarNames()); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// kind 不可改：换种类等于换语义（见 store.UpdateSource 注释）
	if err := h.db.UpdateSource(r.Context(), cur); err != nil {
		h.writeSourceErr(w, err, "update")
		return
	}
	fresh, err := h.db.GetSource(r.Context(), scope.ID, id)
	if err == nil {
		cur = fresh
	}
	views, err := h.views(r.Context(), []*store.Source{cur})
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
	scope, ok := h.resolveNamespace(w, r, orgID, "", false)
	if !ok {
		return
	}
	if err := h.db.DeleteSource(r.Context(), scope.ID, id); err != nil {
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

	// 先解 body（带自定义 payload，也可能带 namespaceId），再解析命名空间。
	var body struct {
		NamespaceID string          `json:"namespaceId"`
		Type        string          `json:"type"`
		Payload     json.RawMessage `json:"payload"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)

	// 触发是**写**操作（建事件 + 派任务）→ `*` 不允许。
	scope, ok := h.resolveNamespace(w, r, orgID, body.NamespaceID, false)
	if !ok {
		return
	}
	id := r.PathValue("id")
	src, err := h.db.GetSource(r.Context(), scope.ID, id)
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
		// NamespaceID 与 source.namespaceId 都可作为 body 取值来源（契约 §4）
		NamespaceID string `json:"namespaceId"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)

	bodyNS := body.NamespaceID
	if bodyNS == "" && body.Source != nil {
		bodyNS = body.Source.NamespaceID
	}
	// 试运行不落库，但它要推演"会派给哪些节点/能力"，那些都在某个命名空间里，
	// 所以同样必填（缺 → 400，`*` → 400：试运行一个跨命名空间的派发没有意义）。
	scope, ok := h.resolveNamespace(w, r, orgID, bodyNS, false)
	if !ok {
		return
	}

	// 有 id 走已保存的源；没有就用请求体里的草稿
	var src *store.Source
	if id := r.PathValue("id"); id != "" {
		got, err := h.db.GetSource(r.Context(), scope.ID, id)
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
		// 草稿的命名空间/组织以会话与参数为准，不信请求体里的手写值：
		// 否则能用一个 body 推到别的命名空间去试运行（跳且不利）。
		src.NamespaceID = scope.ID
		src.OrgID = orgID
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
//
// 目标能力按**源自己的** namespace_id 查（I11：出口能力必须同命名空间）：
// 跨命名空间汇总（namespaceId=*）时不同源的 `lint@1` 是**不同定义**，
// 用会话的 org 一把梭会拼出"同名不同内容"的错误视图。
// 同一命名空间只查一次（N 个源不会变成 N 次能力全量查询）。
func (h *sourceHandler) views(ctx context.Context, list []*store.Source) ([]sourceView, error) {
	byNamespace := make(map[string]map[string]*store.CapabilityVersion)
	capsIn := func(namespaceID string) (map[string]*store.CapabilityVersion, error) {
		if m, ok := byNamespace[namespaceID]; ok {
			return m, nil
		}
		caps, err := h.db.ListCapabilities(ctx, namespaceID)
		if err != nil {
			return nil, err
		}
		m := make(map[string]*store.CapabilityVersion, len(caps))
		for _, c := range caps {
			m[c.ID] = c
		}
		byNamespace[namespaceID] = m
		return m, nil
	}

	out := make([]sourceView, 0, len(list))
	for _, s := range list {
		byID, err := capsIn(s.NamespaceID)
		if err != nil {
			return nil, err
		}
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
	case errors.Is(err, store.ErrReservedEventSource):
		// 保留名是**调用方的输入问题**，不是服务端故障：必须 400，
		// 落到 default 的 500 会让用户以为中枢坏了（而改个名字就好）。
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
//
// **这个入口不需要 namespaceId**（契约 §6.2 的例外）：它是外部系统用的路径，
// 鉴权就是 URL 里的 token，而 token 全局唯一 —— 源行自己带着 namespace_id，
// 后续（写事件 / 派发 / 建任务）全部用 `src.NamespaceID`，不靠请求参数。
// 要求外部系统先知道 namespaceId 才能调 webhook 是没道理的（GitHub 不会）。
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
