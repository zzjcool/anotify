package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/zzjcool/anotify/internal/store"
)

// 预案（playbook）的 HTTP 层（#193 / 契约 §6）。
//
// 预案是**清单，不是所有权**：
//
//	POST   /v1/playbooks                  只落一行清单（不写能力 / 边 / 源）
//	POST   /v1/playbooks/{idOrSlug}/install  把清单幂等地补成真实资源（能力 → 边 → 源，单事务）
//	DELETE /v1/playbooks/{idOrSlug}       只删清单，**不回滚**已装的零件
//
// 业务口径（体检怎么算、安装怎么幂等）全在 store 层（`internal/store/playbook.go`）；
// 这里只做三件事：作用域解析、请求体解码、错误映射。
type playbookHandler struct {
	db *store.DB
	// console 复用 resolveOrg / resolveNamespaceCreate / namespaceLabel：
	// org 与命名空间的判定规则只允许有一份实现，抄一份必然漂移。
	console *consoleHandler
}

// 预案端点的作用域口径（契约 §6 / D2）：5 个端点**全部**用 resolveNamespaceCreate。
//
// 与读端点的 `*` 汇总不同，预案**没有**跨命名空间形态：体检结论（status / ready /
// total）是按单个命名空间算的推导量，把几个命名空间混在一起算没有意义。
// 「别的 org」按 404 而不是 403：预案全是写端点（创建 / 安装 / 删除），
// 403 会变成「命名空间 id 存在性探测器」（响应体与「不存在」逐字节相同）。

// playbookCreateRequest 是 POST /v1/playbooks 的请求体（契约 §6）。
//
// Manifest 用 json.RawMessage 接：中枢只校验、不重新编码（D1），
// runner / paramsSchema / 自定义键原样入库、原样回传。
type playbookCreateRequest struct {
	// NamespaceID 可以放 body（query 优先，见 namespaceParam）。
	NamespaceID string          `json:"namespaceId"`
	Slug        string          `json:"slug"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Manifest    json.RawMessage `json:"manifest"`
}

// list 处理 GET /v1/playbooks：列表，每项带就绪体检结论（**不带** items）。
func (h *playbookHandler) list(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET, POST")
		return
	}
	orgID, ok := h.console.resolveOrg(w, r, r.URL.Query().Get("orgId"))
	if !ok {
		return
	}
	// 读端点也走 resolveNamespaceCreate：预案没有 `*` 形态（见文件头）。
	ns, ok := h.console.resolveNamespaceCreate(w, r, orgID, "")
	if !ok {
		return
	}
	views, err := h.db.ListPlaybookViews(r.Context(), ns.ID)
	if err != nil {
		writePlaybookError(w, err, "console.playbooks.list_failed")
		return
	}
	// 空列表是 []（不变量 I9：ListPlaybookViews 返回空切片而不是 nil）。
	writeJSON(w, http.StatusOK, map[string]any{
		"playbooks":   views,
		"namespaceId": namespaceLabel(ns),
	})
}

// create 处理 POST /v1/playbooks：只落一行清单，**不安装**。
func (h *playbookHandler) create(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "GET, POST")
		return
	}
	orgID, ok := h.console.resolveOrg(w, r, r.URL.Query().Get("orgId"))
	if !ok {
		return
	}
	var req playbookCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	// namespaceId：query 优先、body 兜底（namespaceParam 在 resolveNamespaceCreate 内部）。
	ns, ok := h.console.resolveNamespaceCreate(w, r, orgID, req.NamespaceID)
	if !ok {
		return
	}

	pb := &store.Playbook{
		NamespaceID: ns.ID,
		// org_id 由 store 从命名空间派生（I10），这里不填。
		Slug:        req.Slug,
		Name:        req.Name,
		Description: req.Description,
		Manifest:    req.Manifest,
	}
	// slug / name / manifest 的校验全在 store 的写入口（ValidatePlaybookSlug /
	// CreatePlaybook / ParsePlaybookManifest）：handler 不再抄一份规则，
	// 两份校准口径迟早分叉（契约 §5）。
	if err := h.db.CreatePlaybook(r.Context(), pb); err != nil {
		writePlaybookError(w, err, "console.playbooks.create_failed")
		return
	}
	// 回详情形态（带 items）：创建后前端直接就能渲染体检清单。
	detail, err := h.db.GetPlaybookDetail(r.Context(), ns.ID, pb.ID)
	if err != nil {
		writePlaybookError(w, err, "console.playbooks.create_reload_failed")
		return
	}
	// 201：新增了资源（对齐 POST /v1/sources 的口径，D4）。
	writeJSON(w, http.StatusCreated, map[string]any{"playbook": detail})
}

// byKey 处理 GET / DELETE /v1/playbooks/{idOrSlug}（id 与 slug 都能定位）。
func (h *playbookHandler) byKey(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.console.resolveOrg(w, r, r.URL.Query().Get("orgId"))
	if !ok {
		return
	}
	idOrSlug := r.PathValue("idOrSlug")
	switch r.Method {
	case http.MethodGet:
		ns, ok := h.console.resolveNamespaceCreate(w, r, orgID, "")
		if !ok {
			return
		}
		detail, err := h.db.GetPlaybookDetail(r.Context(), ns.ID, idOrSlug)
		if err != nil {
			writePlaybookError(w, err, "console.playbooks.get_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"playbook": detail})

	case http.MethodDelete:
		ns, ok := h.console.resolveNamespaceCreate(w, r, orgID, playbookBodyNamespace(w, r))
		if !ok {
			return
		}
		// 只删清单行：已装出去的能力 / 边 / 源原样保留（D14）。
		// 重复删同一个预案 → 第二次是 404（未命中就是不存在）。
		if err := h.db.DeletePlaybook(r.Context(), ns.ID, idOrSlug); err != nil {
			writePlaybookError(w, err, "console.playbooks.delete_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})

	default:
		methodNotAllowed(w, "GET, DELETE")
	}
}

// install 处理 POST /v1/playbooks/{idOrSlug}/install：幂等安装（能力 → 边 → 源，单事务）。
func (h *playbookHandler) install(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	orgID, ok := h.console.resolveOrg(w, r, r.URL.Query().Get("orgId"))
	if !ok {
		return
	}
	ns, ok := h.console.resolveNamespaceCreate(w, r, orgID, playbookBodyNamespace(w, r))
	if !ok {
		return
	}

	pb, res, err := h.db.InstallPlaybook(r.Context(), ns.ID, r.PathValue("idOrSlug"))
	if err != nil {
		// 安装是单事务：出错时一个零件都不会落地（store 已整体回滚）。
		writePlaybookError(w, err, "console.playbooks.install_failed")
		return
	}
	// 返回**装完之后**重新体检过的详情：状态要随安装变化，否则前端得多打一次。
	detail, err := h.db.GetPlaybookDetail(r.Context(), ns.ID, pb.ID)
	if err != nil {
		writePlaybookError(w, err, "console.playbooks.install_reload_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"playbook": detail,
		"created":  res.Created,
		"skipped":  res.Skipped,
		"notes":    res.Notes,
	})
}

// playbookBodyNamespace 从请求体里取 namespaceId（query 优先由 namespaceParam 处理）。
//
// GET / DELETE / install 没有 body 是常态，所以**读不出来不算错误**：
// 这类请求的 namespaceId 通常就在 query 里，硬报 400 会把「DELETE 带空 body」
// 判成参数错误（对照 deleteEdge 的口径）。
func playbookBodyNamespace(w http.ResponseWriter, r *http.Request) string {
	if r.Body == nil || r.Body == http.NoBody {
		return ""
	}
	var req struct {
		NamespaceID string `json:"namespaceId"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
	return req.NamespaceID
}

// writePlaybookError 是预案端点错误映射的**唯一**实现（契约 §6）。
//
// event 是 slog 事件名（`console.playbooks.<op>_failed`）：5xx 只回
// `internal error`，细节只能从服务端日志里查 —— 每个端点一个事件名，
// 排障时才能立刻看出是哪一步崩的。
func writePlaybookError(w http.ResponseWriter, err error, event string) {
	switch {
	case errors.Is(err, store.ErrPlaybookNotFound):
		// 别的命名空间的 id / slug 在这里等同于不存在（I11：不跨命名空间取）。
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "预案不存在",
			"code":  "not_found",
		})
	case errors.Is(err, store.ErrPlaybookSlugTaken):
		// slug 是命名空间内的身份：撞名要人改一个，不是"顺手改成 -2"能糊过去的事。
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "同命名空间里已有同名 slug 的预案",
			"code":  "slug_taken",
		})
	case errors.Is(err, store.ErrPlaybookSlugInvalid),
		errors.Is(err, store.ErrPlaybookNameRequired),
		errors.Is(err, store.ErrPlaybookManifestInvalid),
		errors.Is(err, store.ErrInvalidSourceKind),
		errors.Is(err, store.ErrInvalidSourceConfig):
		// 全是**调用方可修的输入问题**：把具体原因原样回给他（哪一项、哪个字段），
		// 前端表单才能指出来。这些不是内部故障，不能吞成 "internal error"。
		//
		// 命名空间类错误（含 ErrNamespaceRequired）**不在**这里：作用域在进入 handler
		// 时已由 resolveNamespaceCreate 判定，剩下的交 handleNamespaceError —— 状态码
		// 只有一处真相（writeNamespaceError），这里再抄一份必然分叉（NIT2）。
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": err.Error(),
			"code":  "invalid_playbook",
		})
	default:
		// 命名空间类（不存在 / 别的 org / 跨命名空间引用）交给既有映射：
		// 状态码只有一处真相（writeNamespaceError），这里再抄一份必然分叉。
		if handleNamespaceError(w, err) {
			return
		}
		slog.Error("playbook request failed", "event", event, "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}
}
