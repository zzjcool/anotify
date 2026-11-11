package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/zzjcool/anotify/internal/authn"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/resultspool"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 A1 的**上行动作端点**：节点 → 中枢的产物字节唯一入口（#332 / G3-D3）。
//
//	PUT /v1/node/tasks/{taskId}/artifacts/{sha256}
//	Authorization: Bearer <node-identity-token>
//	X-Anotify-Attempt / X-Anotify-Lease-Id / X-Anotify-Task-Token
//	X-Anotify-Artifact-Path: <URL 编码的逻辑路径>
//
// 为什么鉴权不走会话中间件：调用方是节点上的 anode，它手里只有 node token，没有
// 用户 Cookie。所以这里自己做 Bearer → token_hash → 节点身份的第一步，再由 store 的
// Begin/Register 在事务里复核 attempt/lease/attemptToken/相位/撤销/命名空间授权
// （「收完 bytes 再登记时重做全部授权」是 A1 的硬语义：迟到文件不能凭先前验证过）。
//
// 错误体是 `{"error":"<code>"}`，不是 Status 信封：节点客户端（cmd/anode/internal/upload）
// 只解析这个形状，且只接受四个 A1 码；套信封会让所有拒绝在节点侧降级成不可诊断的
// transport。授权失败一律 401/403（节点侧终止不重试），不用 400/409 表达。
//
// 顺序是有意的，且**不可换**：
//  1. 节点身份（token 明文绝不进日志/响应/错误消息）；
//  2. URL 段（sha256 形状）；
//  3. 授权三件套 + 逻辑路径头；
//  4. 声明长度超单件上限 → 直接 413（**不读 body、不建 ticket**）；
//  5. 已知长度时先 Begin 预检授权并建 30s ticket；
//  6. 流式收流到托管草稿（超限 → 413，读错误 → 400 hash 不匹配）；
//  7. 校验实际 sha/大小；
//  8. 先原子发布字节，再用同一批授权事实登记（ticket 过期 → 换 ticket + 原地改名，重试一次）；
//  9. 任何失败都补偿：删本次已发布对象 + 撤销 ticket（best effort），保证零写。
type artifactUploadHandler struct {
	db    *store.DB
	spool *resultspool.Store
	log   *slog.Logger
	// now 是判定租约/ticket 的时刻（秒）。为空回落 store.Now（测试可注入）。
	now func() int64
}

// newArtifactUploadHandler 构造上传 handler（mux 只在这里接线，worker 不改 mux）。
func newArtifactUploadHandler(db *store.DB, spool *resultspool.Store, log *slog.Logger) *artifactUploadHandler {
	return &artifactUploadHandler{db: db, spool: spool, log: log}
}

// 授权失败的错误体（A1 动作文档的封闭取值；节点侧只认这两个之外的四个业务码）。
const (
	artifactUploadErrUnauthorized = "unauthorized"
	artifactUploadErrForbidden    = "forbidden"
)

// logger 返回非 nil logger（避免 handler 里到处判空）。
func (h *artifactUploadHandler) logger() *slog.Logger {
	if h == nil || h.log == nil {
		return slog.Default()
	}
	return h.log
}

// nowUnix 返回当前秒时间；未注入时用 store.Now（与 store 的授权判定同一时钟口径）。
func (h *artifactUploadHandler) nowUnix() int64 {
	if h == nil || h.now == nil {
		return store.Now()
	}
	return h.now()
}

// uploadLogContext 是一次上传的**可记录**事实：taskId/attempt/path/sha/size/nodeId。
//
// 刻意不含 leaseId / attemptToken / node token：它们是凭据，进日志等于泄漏。
type uploadLogContext struct {
	nodeID  string
	taskID  string
	path    string
	sha256  string
	attempt int
	size    int64
}

func (c uploadLogContext) attrs() []any {
	return []any{
		"node_id", c.nodeID,
		"task_id", c.taskID,
		"attempt", c.attempt,
		"path", c.path,
		"sha256", c.sha256,
		"size", c.size,
	}
}

func (h *artifactUploadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := h.logger()

	// ① 节点身份：Bearer → token_hash → 节点。查询失败（含 token 不匹配）一律 401，
	//    不区分「令牌不存在」与「库出错」——细节留在服务端日志，响应只说「这个令牌不能用」。
	token := authn.BearerToken(r)
	if token == "" {
		log.Warn("artifact upload rejected: missing node token",
			"event", "artifact.upload.rejected", "reason", "missing_node_token")
		writeArtifactActionError(w, http.StatusUnauthorized, artifactUploadErrUnauthorized)
		return
	}
	node, err := h.db.GetNodeByTokenHash(ctx, store.HashNodeToken(token))
	if err != nil {
		log.Warn("artifact upload rejected: unknown node token",
			"event", "artifact.upload.rejected", "reason", "invalid_node_token")
		writeArtifactActionError(w, http.StatusUnauthorized, artifactUploadErrUnauthorized)
		return
	}

	// ② URL 段。sha256 是 URL 段里的**字节哈希**（不是 spec digest），形状必须先过。
	taskID := r.PathValue("taskId")
	sha := r.PathValue("sha256")
	lc := uploadLogContext{nodeID: node.ID, taskID: taskID, sha256: sha, size: r.ContentLength}
	if !protocol.ValidArtifactSHA256(sha) {
		h.reject(w, log, lc, http.StatusBadRequest, protocol.ArtifactErrPathInvalid)
		return
	}

	// ③ 授权三件套。缺一个就无法建立授权身份 → 401（不是 400：400 会被节点当成
	//    「请求不合法」直接终止，而真实原因是凭据没带上）。
	attempt, aerr := parseArtifactAttemptHeader(r.Header.Get(protocol.ArtifactHeaderAttempt))
	leaseID := strings.TrimSpace(r.Header.Get(protocol.ArtifactHeaderLeaseID))
	attemptToken := strings.TrimSpace(r.Header.Get(protocol.ArtifactHeaderTaskToken))
	lc.attempt = attempt
	if aerr != nil || leaseID == "" || attemptToken == "" {
		log.Warn("artifact upload rejected: incomplete attempt credentials",
			append([]any{"event", "artifact.upload.rejected", "reason", "missing_attempt_credentials"},
				lc.attrs()...)...)
		writeArtifactActionError(w, http.StatusUnauthorized, artifactUploadErrUnauthorized)
		return
	}
	logicalPath, perr := protocol.DecodeArtifactPathHeader(r.Header.Get(protocol.ArtifactHeaderArtifactPath))
	if perr != nil {
		h.reject(w, log, lc, http.StatusBadRequest, protocol.ArtifactErrPathInvalid)
		return
	}
	lc.path = logicalPath

	// ④ 托管池未配置 → 503（刻意不是 A1 四码：这是部署配置错误，节点会当可重试
	//    传输错误，不会把整批产物误判成 artifact_upload_failed）。
	if h.spool == nil {
		log.Error("artifact upload unavailable: spool not configured",
			append([]any{"event", "artifact.upload.unavailable"}, lc.attrs()...)...)
		writeArtifactActionError(w, http.StatusServiceUnavailable, artifactErrUnavailable)
		return
	}

	// auth 是本次请求的授权事实。**Now 在每次 store 调用前刷新**：A1 要求「收完
	// bytes 发布登记时重做全部授权」，拿请求开始那一刻的时钟去判登记，等于允许
	// 一次慢上传绕过传输途中发生的租约到期/撤销（迟到的文件不能凭先前验证过）。
	auth := store.EmitAuth{
		TaskID:        taskID,
		NodeID:        node.ID,
		NodeTokenHash: node.TokenHash,
		Attempt:       attempt,
		LeaseID:       leaseID,
		AttemptToken:  attemptToken,
	}
	in := protocol.ArtifactUploadResponse{
		TaskID:  taskID,
		Attempt: attempt,
		Path:    logicalPath,
		SHA256:  sha,
		Size:    r.ContentLength,
	}

	// ⑤ 声明长度已超单件上限 → 413 且**不读 body**（大文件不该先被我们读一遍再拒绝）。
	//    放在 Begin 之前也是为了不留 ticket/object 行（零写判据）。
	if r.ContentLength > protocol.MaxArtifactTransferBytes {
		h.reject(w, log, lc, http.StatusRequestEntityTooLarge, protocol.ArtifactErrTooLarge)
		return
	}

	// 已知长度：先预检授权 + 建 ticket。chunked 反过来——先收流拿实际大小，再 Begin
	// （因为 in.Size 是 ticket 的一部分，必须是真的）。
	declared := r.ContentLength >= 0
	var tickets []*store.ArtifactUpload
	var u *store.ArtifactUpload
	if declared {
		auth.Now = h.nowUnix()
		u, err = h.db.BeginArtifactUpload(ctx, auth, in)
		if err != nil {
			h.rejectError(w, log, lc, err)
			return
		}
		tickets = append(tickets, u)
	}

	// ⑥ 流式收流。草稿上限就是单件上限：`ErrTooLarge` 说明声明长度撒谎或 chunked 超限。
	//    限额先由草稿在写入时挡住，⑦ 的 >Max 判据是契约 §6-7 要求的显式二次核对。
	draft, derr := h.spool.CreateDraft(protocol.MaxArtifactTransferBytes)
	if derr != nil {
		h.compensate(ctx, log, lc, tickets, "")
		h.rejectError(w, log, lc, derr)
		return
	}
	// 未 Finalize 的草稿一定 Discard（只删自己的临时文件；已发布后是 nil）。
	defer func() { _ = draft.Discard() }()

	if _, cerr := io.Copy(draft, r.Body); cerr != nil {
		h.compensate(ctx, log, lc, tickets, "")
		if errors.Is(cerr, resultspool.ErrTooLarge) {
			h.reject(w, log, lc, http.StatusRequestEntityTooLarge, protocol.ArtifactErrTooLarge)
			return
		}
		// 读中断/写失败 = body 不完整，字节事实不可信（与哈希不符同语义）。
		log.Warn("artifact upload body read failed",
			append([]any{"event", "artifact.upload.rejected", "reason", protocol.ArtifactErrHashMismatch,
				"error", cerr.Error()}, lc.attrs()...)...)
		writeArtifactActionError(w, http.StatusBadRequest, protocol.ArtifactErrHashMismatch)
		return
	}

	// ⑦ 校验实际字节（声明与实际都查，不截断、不信任 Content-Length）。
	actualSize := draft.Size()
	actualSHA := draft.SHA256()
	lc.size = actualSize
	switch {
	case actualSize > protocol.MaxArtifactTransferBytes:
		// 草稿限额已经在写入时挡住超写；这里仍按契约 §6-7 显式核对一次：
		// 上限是安全边界，不依赖上游的隐含不变量。
		h.compensate(ctx, log, lc, tickets, "")
		h.reject(w, log, lc, http.StatusRequestEntityTooLarge, protocol.ArtifactErrTooLarge)
		return
	case actualSHA != sha:
		h.compensate(ctx, log, lc, tickets, "")
		h.reject(w, log, lc, http.StatusBadRequest, protocol.ArtifactErrHashMismatch)
		return
	case declared && actualSize != r.ContentLength:
		// 长度与声明不符：body 被截断或超发，字节事实不作数。
		h.compensate(ctx, log, lc, tickets, "")
		h.reject(w, log, lc, http.StatusBadRequest, protocol.ArtifactErrHashMismatch)
		return
	}

	if !declared {
		in.Size = actualSize
		auth.Now = h.nowUnix()
		u, err = h.db.BeginArtifactUpload(ctx, auth, in)
		if err != nil {
			h.compensate(ctx, log, lc, tickets, "")
			h.rejectError(w, log, lc, err)
			return
		}
		tickets = append(tickets, u)
	}

	// ⑧ 先原子发布字节，再用同一批授权事实登记（登记会重做全部授权）。
	finalID := u.ObjectID().String()
	if err := draft.Finalize(finalID); err != nil {
		h.compensate(ctx, log, lc, tickets, "")
		h.rejectError(w, log, lc, err)
		return
	}
	auth.Now = h.nowUnix()
	reg, err := h.db.RegisterArtifactUploadClassified(ctx, auth, u, actualSHA, actualSize)
	if errors.Is(err, store.ErrArtifactUploadExpired) {
		// 30s ticket 在慢链路 + 大文件上完全可能过期。这里**不重传字节**：换一张
		// 新 ticket（新 object id）并把已发布的副本原地改名过去（同文件系统 rename），
		// 再登记一次。有界：只重试一次。
		// 换 ticket 也要用当下的时钟：新 ticket 的 30s 窗口从现在才开始。
		auth.Now = h.nowUnix()
		retry, beginErr := h.db.BeginArtifactUpload(ctx, auth, in)
		if beginErr != nil {
			h.compensate(ctx, log, lc, tickets, finalID)
			// 只记错误**类别**：store 的 ErrLeaseMismatch 文本里带 leaseId/attempt
			// （授权事实），与本文件「授权类失败不落凭证」的纪律保持一致。
			log.Warn("artifact upload ticket refresh failed",
				append([]any{"event", "artifact.upload.retry_failed", "reason", "ticket_refresh",
					"ticket_error_class", uploadAuthFailureClass(err),
					"error_class", uploadAuthFailureClass(beginErr)}, lc.attrs()...)...)
			h.rejectError(w, log, lc, beginErr)
			return
		}
		tickets = append(tickets, retry)
		previous := u
		newID := retry.ObjectID().String()
		if moveErr := h.spool.Move(finalID, newID); moveErr != nil {
			h.compensate(ctx, log, lc, tickets, finalID)
			log.Warn("artifact upload object re-key failed; giving up retry",
				append([]any{"event", "artifact.upload.retry_failed", "reason", "object_move",
					"error", moveErr.Error()}, lc.attrs()...)...)
			h.rejectError(w, log, lc, err)
			return
		}
		finalID, u = newID, retry
		// 旧 ticket 已经过期（它永远不可能再登记成功），顺手撤掉：否则一次**成功**的
		// 上传会留下一条「活跃 ticket」残行，只有 ticket GC 才收得掉，排查时看起来
		// 像「有个上传还卡在中途」。这里只撤 ticket，不碰已改名过去的字节。
		if abortErr := h.db.AbortArtifactUpload(context.WithoutCancel(ctx), previous); abortErr != nil {
			log.Warn("artifact upload: abort expired ticket failed",
				append([]any{"event", "artifact.upload.retry_failed", "reason", "abort_expired_ticket",
					"error", abortErr.Error()}, lc.attrs()...)...)
		}
		// 补偿列表只剩新 ticket（此处两条路径都只有一个 ticket：已撤的旧的那个）。
		tickets = []*store.ArtifactUpload{retry}
		auth.Now = h.nowUnix()
		reg, err = h.db.RegisterArtifactUploadClassified(ctx, auth, u, actualSHA, actualSize)
	}
	if err != nil {
		h.compensate(ctx, log, lc, tickets, finalID)
		h.rejectError(w, log, lc, err)
		return
	}

	// ⑨ 成功。200 幂等重传时 owner 可能是**别的** object（早先那次上传赢了）：
	//    本次副本是多余字节，按幂等契约删掉，不让 objects 里长第二份。
	if published := reg.PublishedObjectID.String(); published != finalID {
		if rerr := h.spool.Remove(finalID); rerr != nil {
			log.Warn("artifact upload: remove losing duplicate copy failed",
				append([]any{"event", "artifact.upload.duplicate_cleanup_failed",
					"object_id", finalID, "error", rerr.Error()}, lc.attrs()...)...)
		}
	}
	log.Info("artifact uploaded",
		append([]any{"event", "artifact.upload.accepted", "status", reg.Status}, lc.attrs()...)...)
	writeJSON(w, reg.Status, reg.Response)
}

// uploadAuthFailureClass 把授权类错误归类为可日志的短标志（不含任何凭据/授权事实值）。
func uploadAuthFailureClass(err error) string {
	switch {
	case errors.Is(err, store.ErrNodeIdentityMismatch):
		return "node_identity_mismatch"
	case errors.Is(err, store.ErrAuthRequired):
		return "auth_required"
	case errors.Is(err, store.ErrLeaseMismatch):
		return "lease_mismatch"
	case errors.Is(err, store.ErrAttemptTokenInvalid):
		return "attempt_token_invalid"
	case errors.Is(err, store.ErrTaskNotRunning):
		return "task_not_running"
	case errors.Is(err, store.ErrEmitNamespaceDenied):
		return "namespace_denied"
	}
	return "unknown"
}

// parseArtifactAttemptHeader 解析 `X-Anotify-Attempt`（整数 ≥1）。
func parseArtifactAttemptHeader(raw string) (int, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, errors.New("artifact upload: attempt 头缺失")
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, errors.New("artifact upload: attempt 头不是整数")
	}
	if n < 1 {
		return 0, errors.New("artifact upload: attempt 必须 ≥1")
	}
	return n, nil
}

// reject 写一个「域内已判定」的拒绝（A1 四码 + 已知 HTTP 码），并记一条带上下文的 Warn。
func (h *artifactUploadHandler) reject(w http.ResponseWriter, log *slog.Logger, lc uploadLogContext, code int, reason string) {
	log.Warn("artifact upload rejected",
		append([]any{"event", "artifact.upload.rejected", "reason", reason, "status", code}, lc.attrs()...)...)
	writeArtifactActionError(w, code, reason)
}

// rejectError 把 store/协议错误按契约映射成响应：
// A1 四码（400/409/413）→ 授权类（401/403）→ 其它 500 internal。
func (h *artifactUploadHandler) rejectError(w http.ResponseWriter, log *slog.Logger, lc uploadLogContext, err error) {
	if code, reason, ok := artifactUploadFailure(err); ok {
		h.reject(w, log, lc, code, reason)
		return
	}
	if code, ok := artifactUploadAuthFailure(err); ok {
		reason := artifactUploadErrForbidden
		if code == http.StatusUnauthorized {
			reason = artifactUploadErrUnauthorized
		}
		// 授权类失败只记**类别**，不记原始错误文本：store 的 ErrLeaseMismatch 消息里
		// 带 leaseId（授权事实），日志只该有「哪一类授权没过」。
		log.Warn("artifact upload rejected",
			append([]any{"event", "artifact.upload.rejected", "reason", reason,
				"auth_class", uploadAuthFailureClass(err), "status", code}, lc.attrs()...)...)
		writeArtifactActionError(w, code, reason)
		return
	}
	log.Error("artifact upload failed",
		append([]any{"event", "artifact.upload.failed", "reason", artifactErrInternal,
			"error", err.Error()}, lc.attrs()...)...)
	writeArtifactActionError(w, http.StatusInternalServerError, artifactErrInternal)
}

// compensate 做失败补偿：删掉本次请求已发布的对象字节 + 撤销本次建出的全部 ticket。
//
// 语义要点：
//   - 都 best effort（错误只 log）：补偿不能把一个已确定的失败变成 500 之外的另一种失败；
//   - 用 context.WithoutCancel：客户端中途断开（ctx 已取消）时补偿仍必须落库/落盘，
//     否则会留下「manifest 没有、objects 有半截字节、ticket 到点才被 GC」的脏状态；
//   - spool.Remove 幂等（不存在算成功），AbortArtifactUpload 只撤 ticket、不动已登记事实。
func (h *artifactUploadHandler) compensate(ctx context.Context, log *slog.Logger, lc uploadLogContext, tickets []*store.ArtifactUpload, objectID string) {
	ctx = context.WithoutCancel(ctx)
	if objectID != "" && h.spool != nil {
		if err := h.spool.Remove(objectID); err != nil {
			log.Warn("artifact upload: compensation remove failed",
				append([]any{"event", "artifact.upload.compensate_failed", "object_id", objectID,
					"error", err.Error()}, lc.attrs()...)...)
		}
	}
	for _, ticket := range tickets {
		if ticket == nil {
			continue
		}
		if err := h.db.AbortArtifactUpload(ctx, ticket); err != nil {
			log.Warn("artifact upload: compensation abort ticket failed",
				append([]any{"event", "artifact.upload.compensate_failed",
					"error", err.Error()}, lc.attrs()...)...)
		}
	}
}
