package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/zzjcool/anotify/internal/store"
)

// 命名空间自身的 CRUD（契约 §6.1 / docs/design/namespaces.md §6.1）。
//
// 一个容易搞混的点，先写在这里：**/v1/namespaces 是组织级资源**。
// 它的请求**不带** namespaceId，响应里的 namespace 对象也**不带** namespaceId
// ——给这个端点加 namespaceId 等于问"这个命名空间属于哪个命名空间"，
// 是自指的死循环。资源端点（/v1/sources、/v1/stats ……）才需要 namespaceId，
// 那部分见 namespace_scope.go 与各资源 handler。
//
// 组织维度仍然要过：谁能列出/新建命名空间取决于他是不是该组织的成员（I10），
// 所以这里复用 consoleHandler.resolveOrg，而不是自己再解析一遍 org。
type namespaceHandler struct {
	db *store.DB
	// resolveOrg 复用 consoleHandler 的组织解析（I10 的唯一入口）
	resolveOrg func(w http.ResponseWriter, r *http.Request, requested string) (string, bool)
}

// namespaceSlugRe 是 slug 的合法字符集。
//
// **权威定义在 store（namespaceSlugPattern）**，这里只是同一规则的复制：
// store 对非法 slug 返回的是普通 error（没有哨兵），只有 handler 侧先拦一次
// 才能给出 400 而不是 500。两处必须同时改 —— 改这里就等于改 API 契约。
var namespaceSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// list 处理 GET /v1/namespaces：列出本组织的命名空间（带资源计数，UI 直接用）。
func (h *namespaceHandler) list(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	orgID, ok := h.resolveOrg(w, r, r.URL.Query().Get("orgId"))
	if !ok {
		return
	}
	list, err := h.db.ListNamespaces(r.Context(), orgID)
	if err != nil {
		slog.Error("list namespaces failed", "event", "console.namespaces.list_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if list == nil {
		list = []*store.Namespace{} // 空列表是 [] 不是 null（I9）
	}
	writeJSON(w, http.StatusOK, map[string]any{"namespaces": list})
}

// create 处理 POST /v1/namespaces（body {slug,name}）。
//
// slug 是**稳定标识**：它出现在 URL 与节点令牌作用域里，建了就不可改
// （改名走 PATCH 的 name）。所以这里先按 store 的字符集校验，
// 非法 → 400；同组织重复 → 409（store 的 ErrNamespaceExists）。
//
// 内置能力播种在 store 里与建行**同事务**完成（namespace.go 的注释写了原因：
// 新命名空间没有 notify.phone 的话连一个 end 节点都声明不出来）。
func (h *namespaceHandler) create(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	orgID, ok := h.resolveOrg(w, r, r.URL.Query().Get("orgId"))
	if !ok {
		return
	}
	var req struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	slug := strings.TrimSpace(req.Slug)
	if !namespaceSlugRe.MatchString(slug) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "slug 只允许小写字母、数字、连字符，且以字母或数字开头（1~31 字符）",
			"code":  "invalid_slug",
		})
		return
	}
	ns, err := h.db.CreateNamespace(r.Context(), orgID, slug, strings.TrimSpace(req.Name))
	if err != nil {
		// slug 重复 → 409（writeNamespaceError 里映射）
		writeNamespaceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"namespace": ns})
}

// byID 处理 PATCH / DELETE /v1/namespaces/{id}。
func (h *namespaceHandler) byID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodPatch:
		h.rename(w, r, id)
	case http.MethodDelete:
		h.remove(w, r, id)
	default:
		methodNotAllowed(w, "PATCH, DELETE")
	}
}

// rename 处理 PATCH /v1/namespaces/{id}：**只改展示名 name**。
//
// 带 slug 的请求一律 400，而不是静默忽略：
//
//	slug 是稳定标识（URL、令牌作用域），"我改了但没生效"比"你不能改"难查得多。
//	静默忽略会让人以为改名成功，直到某天发现 URL 还是旧的。
func (h *namespaceHandler) rename(w http.ResponseWriter, r *http.Request, id string) {
	orgID, ok := h.resolveOrg(w, r, "")
	if !ok {
		return
	}
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing namespace id"})
		return
	}
	var req struct {
		Name *string `json:"name"`
		Slug *string `json:"slug"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if req.Slug != nil && strings.TrimSpace(*req.Slug) != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "slug 是稳定标识（出现在 URL 与令牌作用域里），不可修改；只能改 name",
			"code":  "slug_immutable",
		})
		return
	}
	if req.Name == nil || strings.TrimSpace(*req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if err := h.db.RenameNamespace(r.Context(), orgID, id, strings.TrimSpace(*req.Name)); err != nil {
		writeNamespaceError(w, err)
		return
	}
	// 读回最新的行：前端拿到的是"改完之后的样子"，不用自己拼
	ns, err := h.db.GetNamespace(r.Context(), orgID, id)
	if err != nil {
		writeNamespaceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"namespace": ns})
}

// remove 处理 DELETE /v1/namespaces/{id}?force=true。
//
// 三档状态码（store 提供了全部信息，这里只做翻译）：
//
//	409 + blocking 清单  里面还有 sources/edges/tasks/nodes，且没带 force
//	400                  最后一个命名空间（删了组织就没有"这套流程属于哪里"了）
//	200                  force 级联删除成功
//
// force 的意思是"我知道里面的机器会全部掉线"。所以默认档必须把阻塞对象列出来，
// 让人看清自己在拆什么（docs/design/namespaces.md §3.2）。
func (h *namespaceHandler) remove(w http.ResponseWriter, r *http.Request, id string) {
	orgID, ok := h.resolveOrg(w, r, "")
	if !ok {
		return
	}
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing namespace id"})
		return
	}
	q := r.URL.Query()
	force := q.Get("force") == "true" || q.Get("force") == "1"
	if err := h.db.DeleteNamespace(r.Context(), orgID, id, force); err != nil {
		writeNamespaceError(w, err)
		return
	}
	slog.Info("namespace deleted",
		"event", "console.namespaces.deleted",
		"namespace_id", id, "org_id", orgID, "force", force,
	)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}
