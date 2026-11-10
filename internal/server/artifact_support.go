package server

import (
	"errors"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/resultspool"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 #332（A1 / G3-D3/D4）两条产物传输端点的**共享接缝**，只有一份实现：
//
//   - 上传错误的动作文档形状（{"error":"<code>"}）；
//   - 上传错误的 HTTP 码映射（授权类 401/403、冲突 409、超限 413、哈希 400）；
//   - 下载 Content-Disposition 的安全 basename（防 header 注入 / 路径逃逸）。
//
// 为什么不放进各自的 handler：节点客户端只认**一种**错误体（`upload.go` 的
// errorCode 只读 `{"error":…}` 且只接受 4 个 A1 码），下载的 disposition 又必须与
// 上传登记的逻辑 path 用同一套 basename 规则。两份实现迟早分叉，症状是
// 「同一次失败在节点侧显示 transport、在浏览器里下载成另一个文件名」。

const (
	// reasonArtifactExpired 是 G3-D4 冻结的 410 reason：manifest 元数据仍在，
	// 但托管副本已被保留期清理（不是 404 —— 引用确实存在过，客户端据此区分
	// 「拿错 hash」与「来晚了」）。
	reasonArtifactExpired = "artifact_expired"

	// artifactErrUnavailable 是「中枢没配置托管目录」的 503。刻意**不**用 A1 的
	// 四个封闭码：这是部署配置错误（可重试的运维事实），不是请求本身不合法；
	// 节点侧会把它当可重试传输错误，避免把整批产物误判成 artifact_upload_failed。
	artifactErrUnavailable = "artifact_upload_unavailable"

	// artifactErrInternal 是上传端点未被 A1 封闭集合覆盖的内部错误码。
	artifactErrInternal = "internal"
)

// writeArtifactActionError 写 A1 动作文档的错误体：{"error":"<code>"}。
//
// 这是**故意的**非 Status 信封例外：A1 的上行是节点→中枢的动作文档，客户端
// （cmd/anode/internal/upload）只解析这个形状，且只接受 artifact_path_invalid /
// artifact_hash_mismatch / artifact_too_large / artifact_conflict 四个码。
// 套 Status 信封会让所有 400/409/413 在节点侧降级成 transport（不可诊断）。
func writeArtifactActionError(w http.ResponseWriter, code int, reason string) {
	writeJSON(w, code, map[string]string{"error": reason})
}

// artifactUploadFailure 把 store 的产物 sentinel 映射成 (HTTP 码, A1 错误码)。
//
// 第三个返回值 false = 「不是 A1 封闭集合里的错误」，调用方自行决定 500。
func artifactUploadFailure(err error) (int, string, bool) {
	switch {
	case errors.Is(err, store.ErrArtifactPathInvalid):
		return http.StatusBadRequest, protocol.ArtifactErrPathInvalid, true
	case errors.Is(err, store.ErrArtifactHashMismatch):
		return http.StatusBadRequest, protocol.ArtifactErrHashMismatch, true
	case errors.Is(err, store.ErrArtifactTooLarge):
		return http.StatusRequestEntityTooLarge, protocol.ArtifactErrTooLarge, true
	case errors.Is(err, store.ErrArtifactConflict):
		return http.StatusConflict, protocol.ArtifactErrConflict, true
	}
	return 0, "", false
}

// artifactUploadAuthFailure 把授权类失败映射成 401/403。
//
// 为什么是这两个码而不是 400/409：节点客户端对 401/403 是**终止不重试**，
// 对 400/409/413 也是终止但会尝试读错误码；授权失败必须让节点立刻停手
// （继续重试只会在租约已失效的连接上反复烧 CPU），且不能伪装成 artifact_conflict。
func artifactUploadAuthFailure(err error) (int, bool) {
	switch {
	case errors.Is(err, store.ErrNodeIdentityMismatch), errors.Is(err, store.ErrAuthRequired):
		return http.StatusUnauthorized, true
	case errors.Is(err, store.ErrLeaseMismatch),
		errors.Is(err, store.ErrAttemptTokenInvalid),
		errors.Is(err, store.ErrTaskNotRunning),
		errors.Is(err, store.ErrEmitNamespaceDenied):
		return http.StatusForbidden, true
	}
	return 0, false
}

// artifactBasename 取逻辑路径的安全 basename（用于 Content-Disposition）。
//
// 逻辑路径已由 protocol.ValidArtifactLogicalPath 校验过（无控制字符、无 ..、
// 非绝对、`/` 分隔），这里再取最后一段：`path.Base` 只做分段，不解析宿主路径，
// 因此永远不可能把用户 workDir / 托管目录前缀带进响应头。
func artifactBasename(logicalPath string) string {
	base := path.Base(logicalPath)
	if base == "" || base == "." || base == "/" || base == ".." {
		return "artifact"
	}
	return base
}

// artifactContentDisposition 生成 attachment 的 Content-Disposition。
//
// 用 mime.FormatMediaType 而不是字符串拼接：它会正确引用/编码 filename
// （含 " / 反斜杠 / 非 ASCII 时给出 RFC 2231 的 filename*=UTF-8”… 形式），
// 从根上排除 CRLF 注入（逻辑路径本身也已被 ValidArtifactLogicalPath 拒掉控制字符，
// 两道都留）。返回值解析失败时退化成不含文件名的 attachment —— 宁可丢名字，
// 也不把一个可能畸形的头发出去。
func artifactContentDisposition(logicalPath string) string {
	base := artifactBasename(logicalPath)
	value := mime.FormatMediaType("attachment", map[string]string{"filename": base})
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "attachment"
	}
	return value
}

// artifactSpool 返回 deps 里的托管池（nil = 未配置）。
func (d *serverDeps) artifactSpool() *resultspool.Store {
	if d == nil {
		return nil
	}
	return d.Spool
}
