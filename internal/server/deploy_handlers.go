package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/ws"
)

// 本文件是「部署模型」的控制台端点（console-declares epic）。
//
//	GET    /v1/capability-templates  内置模板（模板 = 代码生成器的输入，不是执行器类型）
//	GET    /v1/artifacts             某能力的产物列表
//	POST   /v1/deployments           把产物部署到选中的节点
//	GET    /v1/deployments           部署状态列表
//	DELETE /v1/deployments           撤销
//
// 契约见 .pi-orchestrator/console-declares/CONTRACT-protocol.md §11。

// deployHandler 持有部署端点需要的最小依赖。
type deployHandler struct {
	db      *store.DB
	console *consoleHandler
	hub     DeployPublisher
	nowFn   func() int64
}

// DeployPublisher 是中枢 → 节点的部署通道（*ws.NodeHub 满足它）。
//
// 抽成接口是为了让 handler 的测试不必起一条真 WebSocket：部署的**建行**必须
// 成功，推帧失败只是「节点离线，等它上线补推」，两者不该绑死。
type DeployPublisher interface {
	PublishDeploy(namespaceID, nodeID string, f *ws.DeployFrame) error
}

// publish 的真实实现由 mux 注入；为空表示「当前没有节点连接层」（测试/预览模式）。
func (h *deployHandler) publish(nsID, nodeID string, f *ws.DeployFrame) error {
	if h.hub == nil {
		return ws.ErrNodeOffline
	}
	return h.hub.PublishDeploy(nsID, nodeID, f)
}

func (h *deployHandler) now() int64 {
	if h.nowFn != nil {
		return h.nowFn()
	}
	return store.Now()
}

// ---------- GET /v1/capability-templates ----------

// capabilityTemplates 返回全部内置模板。
//
// 模板放在服务端而不是前端硬编码：只有一个事实源，Go 测试能钉住「每个模板渲染出的
// 产物都合法」，控制台少一份会漂移的副本。
func (h *deployHandler) capabilityTemplates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": artifact.Templates()})
}

// ---------- GET /v1/artifacts ----------

// artifacts 列出某个能力的产物。
func (h *deployHandler) artifacts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	ns, ok := h.console.resolveNamespace(w, r, "", true)
	if !ok {
		return
	}
	capID := r.URL.Query().Get("capabilityId")
	if capID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "capabilityId is required",
			"code":  "missing_capability_id",
		})
		return
	}

	nsIDs := []string{ns.ID}
	if ns.All {
		// 读端点允许 `*`：跨命名空间汇总（与服务端其它读端点同口径）。
		ids, err := h.console.namespaceIDs(r.Context(), ns)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		nsIDs = ids
	}

	out := []*store.Artifact{}
	for _, nsID := range nsIDs {
		got, err := h.db.ListArtifacts(r.Context(), nsID, capID)
		if err != nil {
			if handleNamespaceError(w, err) {
				return
			}
			slog.Error("list artifacts failed", "event", "console.artifacts.list_failed", "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		out = append(out, got...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": out})
}

// ---------- /v1/deployments ----------

// createDeploymentRequest 是 POST /v1/deployments 的请求体。
type createDeploymentRequest struct {
	NamespaceID  string `json:"namespaceId"`
	CapabilityID string `json:"capabilityId"`
	// Digest 缺省取该能力最新一份产物。
	Digest  string   `json:"digest,omitempty"`
	NodeIDs []string `json:"nodeIds,omitempty"`
	// Tags 是「按标签选机器」：与 NodeIDs 取并集。
	Tags []string `json:"tags,omitempty"`
}

// deploymentResult 是逐台的部署结果。
//
// 关键语义：`pushed=false` **不代表失败** —— 行已经建好了（state=pending），
// 节点上线后由重连对账补推。UI 必须区分「已下发」与「已排队」，
// 否则运维会以为离线机器上的东西已经生效。
type deploymentResult struct {
	NodeID   string `json:"nodeId"`
	NodeName string `json:"nodeName"`
	DeployID string `json:"deployId,omitempty"`
	Pushed   bool   `json:"pushed"`
	Error    string `json:"error,omitempty"`
}

// deployments 处理 GET / POST / DELETE。
func (h *deployHandler) deployments(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.listDeployments(w, r)
	case http.MethodPost:
		h.createDeployments(w, r)
	case http.MethodDelete:
		h.deleteDeployment(w, r)
	default:
		methodNotAllowed(w, "GET, POST, DELETE")
	}
}

func (h *deployHandler) listDeployments(w http.ResponseWriter, r *http.Request) {
	ns, ok := h.console.resolveNamespace(w, r, "", true)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := store.DeploymentFilter{
		NodeID:       q.Get("nodeId"),
		CapabilityID: q.Get("capabilityId"),
	}

	nsIDs := []string{ns.ID}
	if ns.All {
		ids, err := h.console.namespaceIDs(r.Context(), ns)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		nsIDs = ids
	}

	out := []*store.Deployment{}
	for _, nsID := range nsIDs {
		got, err := h.db.ListDeployments(r.Context(), nsID, f)
		if err != nil {
			if handleNamespaceError(w, err) {
				return
			}
			slog.Error("list deployments failed", "event", "console.deployments.list_failed", "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		out = append(out, got...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployments": out})
}

// createDeployments 把一份产物部署到选中的机器。
//
// 流程：解析选择器 → 逐台建行（pending）→ 逐台推 deploy 帧。
//
// 建行**先于**推帧：推帧失败不能让「中枢还记得它该有这份产物」这件事丢掉，
// 否则节点上线后没有任何记录能触发补推（行不存在 = 永远补不上）。
func (h *deployHandler) createDeployments(w http.ResponseWriter, r *http.Request) {
	var req createDeploymentRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "invalid_json"})
		return
	}
	ns, ok := h.console.resolveNamespace(w, r, req.NamespaceID, false)
	if !ok {
		return
	}
	if req.CapabilityID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "capabilityId is required", "code": "missing_capability_id",
		})
		return
	}

	// 1. 解析要部署的产物
	art, err := h.resolveArtifact(r.Context(), ns.ID, req.CapabilityID, req.Digest)
	if err != nil {
		h.writeArtifactError(w, err)
		return
	}

	// 2. 展开目标节点
	nodes, err := h.resolveTargetNodes(r.Context(), ns.ID, req.NodeIDs, req.Tags)
	if err != nil {
		slog.Error("resolve target nodes failed", "event", "console.deployments.nodes_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if len(nodes) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "no node matched the selector", "code": "no_target_node",
		})
		return
	}

	// 3. 逐台建行 + 推帧
	results := make([]deploymentResult, 0, len(nodes))
	for _, n := range nodes {
		dp, err := h.db.CreateDeployment(r.Context(), ns.ID, n.ID, art.ID)
		if err != nil {
			results = append(results, deploymentResult{
				NodeID: n.ID, NodeName: n.Name, Error: err.Error(),
			})
			continue
		}
		res := deploymentResult{NodeID: n.ID, NodeName: n.Name, DeployID: dp.ID}
		if err := h.publish(ns.ID, n.ID, &ws.DeployFrame{
			Type:     ws.NodeFrameDeploy,
			DeployID: dp.ID,
			Action:   ws.DeployActionApply,
			Artifact: json.RawMessage(art.Spec),
		}); err != nil {
			// 节点离线不是错误：行已经建好，它上线时会补推。
			res.Error = err.Error()
		} else {
			res.Pushed = true
		}
		results = append(results, res)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"artifact":    art,
		"deployments": results,
	})
}

// resolveArtifact 取要部署的产物：显式 digest 优先，否则取该能力最新一份。
func (h *deployHandler) resolveArtifact(ctx context.Context, nsID, capabilityID, digest string) (*store.Artifact, error) {
	if digest != "" {
		return h.db.GetArtifactByDigest(ctx, nsID, digest)
	}
	return h.db.LatestArtifact(ctx, nsID, capabilityID)
}

func (h *deployHandler) writeArtifactError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrArtifactNotFound):
		// 「这个能力还没有产物」是最常见的用户路径（建了能力但没给 run 块）：
		// 给一个能直接指导操作的说法，而不是 404 not found。
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "这个能力还没有可部署的产物：新建能力时需要填 run（入口文件 + 文件内容）",
			"code":  "artifact_not_found",
		})
	case handleNamespaceError(w, err):
		// 已写响应
	default:
		slog.Error("resolve artifact failed", "event", "console.deployments.artifact_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}
}

// resolveTargetNodes 把 (nodeIds ∪ 带任一匹配 tag 的节点) 展开成节点列表。
//
// 顺序稳定（按 nodeId 排序），去重：同一个节点既被点选又命中 tag 时只出现一次。
func (h *deployHandler) resolveTargetNodes(ctx context.Context, nsID string, nodeIDs, tags []string) ([]*store.Node, error) {
	picked := make(map[string]*store.Node, len(nodeIDs))
	for _, id := range nodeIDs {
		if id == "" {
			continue
		}
		n, err := h.db.GetNode(ctx, nsID, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue // 点了别人的机器 / 已删除：跳过，不整单失败
			}
			return nil, err
		}
		picked[n.ID] = n
	}

	if len(tags) > 0 {
		all, err := h.db.ListNodes(ctx, nsID)
		if err != nil {
			return nil, err
		}
		for _, n := range all {
			if _, done := picked[n.ID]; done {
				continue
			}
			if hasAnyTag(n.Tags, tags) {
				picked[n.ID] = n
			}
		}
	}

	out := make([]*store.Node, 0, len(picked))
	for _, n := range picked {
		out = append(out, n)
	}
	// 稳定顺序：部署结果逐台展示，顺序跳动会让用户以为发生了别的事。
	sortNodesByID(out)
	return out, nil
}

func hasAnyTag(have []string, want []string) bool {
	for _, w := range want {
		for _, h := range have {
			if h == w {
				return true
			}
		}
	}
	return false
}

func sortNodesByID(nodes []*store.Node) {
	for i := 1; i < len(nodes); i++ {
		for j := i; j > 0 && nodes[j-1].ID > nodes[j].ID; j-- {
			nodes[j-1], nodes[j] = nodes[j], nodes[j-1]
		}
	}
}

// deleteDeployment 撤销一次部署。
//
// 语义：先推 remove 帧（尽力而为），再删行。**先推后删**：删完再推就拿不到
// deploy_id 与节点归属了；推失败也没关系——节点上多缓存一份产物不构成故障
// （它只是没有归属，不会被派发）。
func (h *deployHandler) deleteDeployment(w http.ResponseWriter, r *http.Request) {
	ns, ok := h.console.resolveNamespace(w, r, "", false)
	if !ok {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "id is required", "code": "missing_id",
		})
		return
	}

	dp, err := h.db.DeleteDeployment(r.Context(), ns.ID, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error": "deployment not found", "code": "not_found",
			})
			return
		}
		slog.Error("delete deployment failed", "event", "console.deployments.delete_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if err := h.publish(ns.ID, dp.NodeID, &ws.DeployFrame{
		Type:     ws.NodeFrameDeploy,
		DeployID: dp.ID,
		Action:   ws.DeployActionRemove,
	}); err != nil {
		slog.Warn("publish remove failed",
			"event", "console.deployments.remove_push_failed",
			"deployment_id", dp.ID, "node_id", dp.NodeID, "error", err.Error())
	}

	writeJSON(w, http.StatusOK, map[string]any{"deleted": dp.ID})
}

// capabilityRunInput 是 POST /v1/capabilities 里的 run 块（可选）。
//
// 有 run = 这个能力有可部署的实现；没有 run = 纯定义（例如内置的 notify.phone，
// 或者只用来占位的中间节点）。
type capabilityRunInput struct {
	Entrypoint string            `json:"entrypoint,omitempty"`
	Files      map[string]string `json:"files,omitempty"`
	TimeoutSec int               `json:"timeoutSec,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

// buildArtifact 把「能力定义 + run 块」编译成一份产物。
//
// 能力定义里的 paramsSchema / outputs / displayName 与 run 一起进产物，
// 所以产物是自包含的：拷到另一台机器就能跑，不需要回中枢取参数契约。
func buildArtifact(c *store.CapabilityVersion, run *capabilityRunInput, requires []string) (*artifact.Artifact, error) {
	entrypoint := ""
	files := map[string]string{}
	timeout := 0
	env := map[string]string(nil)
	if run != nil {
		entrypoint = run.Entrypoint
		if len(run.Files) > 0 {
			files = run.Files
		}
		timeout = run.TimeoutSec
		env = run.Env
	}
	if entrypoint == "" {
		entrypoint = "run.sh"
	}

	a := &artifact.Artifact{
		APIVersion: artifact.APIVersion,
		Kind:       artifact.KindCapability,
		Metadata:   artifact.Metadata{Name: c.Name, Version: c.Version},
		Spec: artifact.Spec{
			DisplayName: c.DisplayName,
			Params:      c.ParamsSchema,
			Outputs:     append([]string{}, c.Outputs...),
			Requires:    append([]string{}, requires...),
			Run: artifact.Run{
				Entrypoint: entrypoint,
				Files:      files,
				TimeoutSec: timeout,
				Env:        env,
			},
		},
	}
	if err := artifact.Validate(a); err != nil {
		return nil, err
	}
	return a, nil
}

// storeArtifact 把产物落库（按 digest 幂等）。
func storeArtifact(ctx context.Context, db *store.DB, nsID string, a *artifact.Artifact) (*store.Artifact, string, error) {
	digest, err := artifact.Digest(a)
	if err != nil {
		return nil, "", fmt.Errorf("digest artifact: %w", err)
	}
	spec, err := artifact.CanonicalJSON(a)
	if err != nil {
		return nil, "", fmt.Errorf("canonical json: %w", err)
	}

	// capability_id 从能力名 + 版本反查：调用方刚创建完，这里必能查到。
	capVer, err := db.GetCapability(ctx, nsID, a.Metadata.Name, a.Metadata.Version)
	if err != nil {
		return nil, "", err
	}

	saved, err := db.CreateArtifact(ctx, &store.Artifact{
		NamespaceID:  nsID,
		CapabilityID: capVer.ID,
		Digest:       digest,
		Spec:         spec,
	})
	if err != nil {
		return nil, "", err
	}
	return saved, digest, nil
}
