package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 A1 的**下行读端点**：用户侧取回中枢托管副本的唯一入口（#332 / G3-D3、D4）。
//
//	GET /v1/namespaces/{ns}/tasks/{taskId}/artifacts/{sha256}
//
// 与上传端点方向相反，但纪律是同构的三条：
//
//  1. **选哪份字节只由 store 决定。** `GetTaskArtifactDownload` 把「当前 task + 当前
//     attempt + 该 attempt 结果里真的引用过这个 sha256」三层绑在一起。handler 不做
//     自己的匹配，也不拿 URL 段去拼路径：`{sha256}` 只是查找键，形状不合法就由 store
//     判 404（一份判据，不在两处各写一遍）。
//  2. **只开托管对象。** 打开的是 `entry.ObjectID`（store 铸造的不透明身份），
//     `entry.Path` 只是**逻辑名**（它长得像 `reports/x.md`，但 handler 永远不 stat/open
//     它），也绝不回退到节点/用户 workDir：那会把「谁上传的」变成「谁有权限读」的
//     越权漏洞（A1 明令禁止的 fallback）。
//  3. **元数据在、副本没了 = 410 `artifact_expired`。** 不是 404（引用确实存在过）、
//     也不是 500（那不是服务器坏了，是保留期到了）。客户端据此区分「拿错 hash」与
//     「来晚了」，这是行为断层最容易做坏的一档。
//
// 响应体是**原始字节**（A1 §4 的明文例外），不套 Status 信封；失败才走 writeAPIError。

// artifactErrDownloadUnavailable 是「中枢没配置托管目录」时下载的 503 reason。
//
// 刻意与上传的 `artifact_upload_unavailable` 分开：同一个部署错误在两个方向上的
// 排查入口不同（上传失败会在节点侧的批次里显示成 artifact_upload_failed，下载失败
// 是用户在页面上点下载拿不到东西），共用一个码会让运维分不清是哪一端没配好。
const artifactErrDownloadUnavailable = "artifact_download_unavailable"

// artifactDownloadContentType 是产物字节的唯一响应类型。
//
// 恒为 octet-stream，不按扩展名猜：产物是用户/节点产出的任意文件（可能含 HTML/SVG），
// 按 `.html` 下发就会让下载端点在**中枢域**里渲染任意脚本。配合 nosniff 彻底关掉
// 内容嗅探，浏览器只能落盘，不能执行。
const artifactDownloadContentType = "application/octet-stream"

// artifactDownloadAPI 是 Task 子资源下载（用户会话鉴权 + 当前 reported manifest 引用）。
type artifactDownloadAPI struct{ d *serverDeps }

// newArtifactDownloadAPI 绑定共享依赖（mux 只在这里接线，worker 不改 mux）。
func newArtifactDownloadAPI(d *serverDeps) *artifactDownloadAPI {
	return &artifactDownloadAPI{d: d}
}

// get 是下载 handler。顺序不可换：
//
//	命名空间授权 → taskId → manifest 引用（store 判 404/410）→ 托管副本（判 410）
//	→ 响应头 → 原始字节
//
// 为什么「先查 manifest 再开文件」不能反：先开文件就需要一个路径，而没有 manifest
// 就没有可信的 object id，只能从 URL 段猜——那正是路径逃逸的入口。
func (a *artifactDownloadAPI) get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := a.d.logger()

	// ① 命名空间作用域：路径 `{ns}` → store id（未登录 401 / 不存在 404 / 跨 ns 一律
	//    与「不存在」同码，调用方无法据此探测别的命名空间里有没有这个 id）。
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}

	// ② taskId 是必填路径段（空 → 400，与其它 Task 子资源同一口径）。
	taskID, err := taskPathID(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	sha := r.PathValue("sha256")

	// ③ 结果引用：这一步同时完成「任务属于这个 ns」「结果已上报」「当前 attempt 引用过
	//    这个 hash」「副本未过期」四件事，全部在 store 里一次判定（G3-D4：同 hash 有多条
	//    live 引用时按 path 序取首条，live 优先于 expired）。
	entry, err := a.d.DB.GetTaskArtifactDownload(ctx, ns, taskID, sha, a.d.now())
	if err != nil {
		// 404（无引用/跨 ns/未上报）、410（全过期）、500（真故障）三档都交给
		// writeAPIError 的既有映射：#332 已把 store.ErrArtifactExpired 收口成
		// 410 artifact_expired，在这里再写一份映射只会让两个读端点给出不同码。
		switch {
		case errors.Is(err, store.ErrArtifactExpired):
			log.Info("artifact download: copy expired",
				"event", "artifact.download.expired",
				"namespace_id", ns, "task_id", taskID, "sha256", sha)
		case errors.Is(err, store.ErrNotFound):
			// 404 是常态（客户端猜错 hash、任务还没上报），不刷日志。
		default:
			log.Error("artifact download: result reference lookup failed",
				"event", "artifact.download.failed",
				"namespace_id", ns, "task_id", taskID, "sha256", sha, "error", err.Error())
		}
		writeAPIError(w, err)
		return
	}

	// ④ 托管池未配置 → 503。诚实的「没配置好」，不是假成功、也绝不临时读宿主路径顶上。
	spool := a.d.artifactSpool()
	if spool == nil {
		log.Error("artifact download unavailable: spool not configured",
			"event", "artifact.download.unavailable",
			"namespace_id", ns, "task_id", taskID, "sha256", sha)
		writeStatus(w, http.StatusServiceUnavailable, artifactErrDownloadUnavailable,
			"artifact storage is not configured", nil)
		return
	}

	// ⑤ 打开托管副本。失败（对象被 GC 删了 / 非普通文件 / 被换成符号链接 / id 不合法）
	//    一律 410：元数据还在但字节不可得，对下载方就是「来晚了」。这里不区分具体原因
	//    也是刻意的——把「被换成符号链接」暴露成 500 只会引诱攻击者用状态码做探测。
	objectID := entry.ObjectID.String()
	f, size, err := spool.Open(objectID)
	if err != nil {
		log.Warn("artifact download: system copy unavailable",
			"event", "artifact.download.expired",
			"namespace_id", ns, "task_id", taskID, "sha256", sha,
			"object_id", objectID, "error", err.Error())
		writeAPIError(w, errStatus(http.StatusGone, reasonArtifactExpired, "artifact copy expired"))
		return
	}
	// 句柄必须关：ServeContent 只读不关（它可能只消费了 Range 的一段）。
	defer func() { _ = f.Close() }()

	// ⑥ 响应头（全部必须设）：
	//   - Content-Type 固定 octet-stream（见 artifactDownloadContentType）；
	//   - Content-Length 用**实际副本大小**（不是 manifest 里的大小：不一致时唯一
	//     重要的不变量是「声明的长度 == 发出的字节」，否则客户端会截断或空等）；
	//   - Content-Disposition 的文件名只取逻辑 path 的安全 basename（mime.FormatMediaType
	//     编码，非 ASCII 走 filename*=，从根上排除 CRLF/引号注入）；
	//   - nosniff 与固定的 Content-Type 成对出现。
	h := w.Header()
	h.Set("Content-Type", artifactDownloadContentType)
	h.Set("Content-Length", strconv.Itoa(int(size)))
	h.Set("Content-Disposition", artifactContentDisposition(entry.Path))
	h.Set("X-Content-Type-Options", "nosniff")

	// ⑦ 原始字节（A1 §4：下载成功体不套信封）。用 ServeContent 而不是 io.Copy：
	//    它带来 Range/HEAD 语义（浏览器下载可续传）并且**不会**嗅探 Content-Type
	//    （我们已显式设好）。零 modtime = 不下发 Last-Modified：产物字节不可变，
	//    它的「修改时间」是内部实现细节，给客户端只会引出条件请求的伪语义。
	http.ServeContent(w, r, "", time.Time{}, f)
}
