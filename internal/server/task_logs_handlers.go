package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/ws"
)

// 本文件是「任务日志」的唯一 HTTP 端点（CONTRACT-logs-on-node §2）：
//
//	GET /v1/tasks/{taskId}/logs?namespaceId=…（会话 + 命名空间隔离，调用方是控制台）
//
// # 日志的真相在节点上
//
// 一次执行留下的 stdout / stderr 档案只存在于**跑它的那台节点**上
// （`<data-dir>/runs/<trace>/<capability>@<version>/<seq>-a<attempt>/`）。中枢据此
// 按需去取：`ws.NodeHub.FetchLogs` 发一帧 logs_request、按 request_id 等回包
// （CONTRACT-logs-on-node §1），本文件把回包翻成一个 200 响应。
//
// # 为什么「不落库、不缓存」
//
// 上一版是「节点 POST 上传、中枢落 task_logs 表」。那条路有两个结构性问题：
//  1. 中枢手里那份**必然更旧**：节点上的档案是完整的（8 MiB/流、7 天保留），
//     库里那份是上传那一刻的截断副本（每流最后 32 KiB）；看日志就是为了「为什么炸」，
//     恰好最需要完整的那一段时它不在；
//  2. 上传要在任务结束时多走一次网络 + 一次写库，而人可能根本不会点开这个任务——
//     为「可能没人看的东西」付带宽和磁盘，是纯粹的浪费。
//
// 不缓存是同一件事的推论：一旦缓存，就要回答「什么时候失效」——节点上的档案会被
// GC 掉、会被重跑覆盖，中枢没有任何可靠的失效信号；而缓存一份错的日志比没有日志
// 更糟（排障时会作结论）。所以每次请求都真的问节点一次。代价是「一次帧往返」，
// 节点侧读的是本地文件尾部，正常远小于 1 秒。
//
// # 为什么绝不对「取不到」回 5xx
//
// 「这次看不到日志」是一个**产品已处理的降级分支**（trace 页给出降级文案，见契约 §5），
// 不是服务出错。做成 5xx 会有两个后果：前端要在一个正常路径上处理错误响应（它现在
// 只认 200 + available:false），监控也会把「用户点开了一台离线节点的日志」记成服务故障。
//
// 上一版的**上传**端点（POST /v1/node/tasks/{taskId}/logs）已经删掉（契约 §3）：
// 没有写入方了，留着它只会让「中枢也存日志」这个旧事实继续存在。

// reason 的**封闭集合**（CONTRACT-logs-on-node §2）。
//
// 封闭是刻意的：这三个值是前端写文案的键（四语言），多一个值就多一份没人翻译的
// 文案。所以「其它一切错误」都折到 timeout（见 logsReasonForError），绝不新增取值。
const (
	// logsReasonNodeOffline 节点没有活跃连接（也包括「这台节点根本不属于这个命名空间」）。
	logsReasonNodeOffline = "node_offline"
	// logsReasonNoLocalArchive 节点在线、也回了包，但它本机上没有那次执行的档案
	// （被 GC 掉了 / 老任务 / 不是这台节点跑的），以及「任务还没派给任何节点」。
	logsReasonNoLocalArchive = "no_local_archive"
	// logsReasonTimeout 帧发出去了没等到回包，或其它任何没取到的情况。
	logsReasonTimeout = "timeout"
)

// taskLogsSource 是本端点**唯一**的数据来源（冻结接缝，见 iface 文档）。
//
// 声明成接口而不是直接依赖 *ws.NodeHub：测试里塞一个 fake 就能覆盖
// 「命中 / 离线 / 没档案 / 超时 / 意外错误」全部五条分支，不用起真 WS 连接、
// 也不用等 5 秒超时。生产的接线在 mux.go（consoleH.taskLogs = nodeHub）。
type taskLogsSource interface {
	FetchLogs(ctx context.Context, namespaceID, nodeID, taskID string) (*ws.LogsResponseFrame, error)
}

// 编译期断言：mux 里注入的就是这个实现。
//
// 有了它，NodeHub.FetchLogs 的签名一变这里就编译不过——否则要等到跑用例
// （或者更糟，跑到真机上）才发现接错了。
var _ taskLogsSource = (*ws.NodeHub)(nil)

// taskLogsView 是响应形状：四个旧字段 + available / reason（契约 §2）。
//
// 形状**保持兼容**：老前端认的 stdout/stderr/truncated/bytes 一个没动，只是多了
// 两个字段。stdout / stderr 是原文返回——包括节点侧追加的截断标记，**不 trim、
// 不转义、不再裁剪**：中枢再裁一次，截断标记里「共 N 字节」到底指哪一层的 N 就说不清了。
type taskLogsView struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated"`
	// Bytes 是档案里两个流的真实字节数之和（截断前，由节点给）。
	//
	// 保留它是为了「原始多大」这句话：但**不能**用 bytes == 0 判「没取到」——
	// 一次什么都没输出的执行同样是 0 字节。这两件事由 available 区分：没取到时
	// available=false + reason，取到了就是 available=true（哪怕 bytes 是 0）。
	Bytes int64 `json:"bytes"`
	// Available 表示这次的四个字段是**真的从节点上取回来的**。
	//
	// 恒出现（不带 omitempty）：前端要按它分支，缺字段会让「取不到」看起来像
	// 「取到了但为空」。false 时 stdout/stderr 一定是空串、bytes 一定是 0。
	Available bool `json:"available"`
	// Reason 只在 available=false 时出现（取到时 omitempty 让它消失）：
	// 前端据此选降级文案（node_offline / no_local_archive / timeout，封闭集合）。
	Reason string `json:"reason,omitempty"`
}

// serveTaskLogs 处理 GET /v1/tasks/{taskId}/logs?namespaceId=…（CONTRACT-logs-on-node §2）。
//
// 名字带 serve 前缀只是为了与**字段** taskLogs（取数源）区分开：
// 两者在同一个结构上不能同名（Go 不允许字段与方法重名），而字段名是冻结接口的一部分。
//
// 三条不能变的语义：
//  1. 任务存在但取不到日志 → **200 + available:false + reason**：取不到是合法状态
//     （节点离线 / 档案被 GC / 节点没回），做成 4xx/5xx 会让前端在正常路径上处理错误。
//  2. 跨命名空间与「不存在」逐字节相同（404 task not found）——与 /v1/tasks/{id} 同口径，
//     调用方无法据此探测别的命名空间里有没有某个 taskId。
//  3. stdout / stderr 原文透传（含节点侧追加的截断标记），中枢一个字都不改。
func (h *consoleHandler) serveTaskLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	taskID := r.PathValue("taskId")
	if taskID == "" {
		writeErr(w, http.StatusBadRequest, "missing task id")
		return
	}
	// 单对象端点：`*` 无定义 → 400（allowAll=false）。
	// 缺 namespaceId 也是 400：绝不回退默认命名空间（回退等于把人写错的地方藏起来）。
	ns, ok := h.resolveNamespace(w, r, "", false)
	if !ok {
		return
	}

	// 任务行必须先在本命名空间可见：这一步同时挡住了「拿别的命名空间的 taskId 读日志」，
	// 也是向节点取数的**前提**（node_id 在这一行上）。
	task, err := h.db.GetTask(r.Context(), ns.ID, taskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "task not found")
			return
		}
		if handleNamespaceError(w, err) {
			return
		}
		// 这条 500 与「日志取不到」不是同一件事：它是「连任务都查不出来」，
		// 属于服务自身故障（与其它 console 端点同口径）。取数链路本身**永不** 5xx。
		slog.Error("get task for logs failed",
			"event", "console.task_logs.task_failed",
			"task_id", taskID,
			"error", err.Error(),
		)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, h.fetchTaskLogs(r.Context(), ns.ID, task))
}

// fetchTaskLogs 向节点取一次日志并把结果折成响应（**这里不写库、不缓存**）。
//
// 三条前置判断的先后顺序是有讲究的：
//
//  1. `task.NodeID == nil` → no_local_archive。这条任务从没派给任何节点，那次执行
//     根本不存在，节点上也不可能有档案。为什么不报 node_offline：那句话的含义是
//     「节点掉了、连上再看」，而这个任务的节点永远不会「连上」——报错了方向，
//     用户会去查网络，真正的原因却是任务没跑起来。
//  2. source 为 nil → node_offline + 一条 WARN。只有测试/嵌入用法会走到这里
//     （生产 mux 一定注入 nodeHub）。选 node_offline 是因为它对用户最接近事实：
//     「这份日志现在拿不到，稍后再试」，且不会像 no_local_archive 那样断言
//     「档案不存在」——我们其实根本没问过节点。
//  3. 真去取。**任何**错误都折成 200 + available:false（见 logsReasonForError）。
func (h *consoleHandler) fetchTaskLogs(ctx context.Context, namespaceID string, task *store.Task) taskLogsView {
	if task.NodeID == nil || *task.NodeID == "" {
		return taskLogsView{Reason: logsReasonNoLocalArchive}
	}
	nodeID := *task.NodeID

	if h.taskLogs == nil {
		// 装配缺失是**部署错误**（生产 mux 一定会注入），所以要留一条可查的 WARN：
		// 否则「控制台上日志永远是节点离线」会变成一个查不到根因的现象。
		slog.Warn("task logs source not wired: reporting node_offline",
			"event", "console.task_logs.source_missing",
			"task_id", task.ID,
			"node_id", nodeID,
		)
		return taskLogsView{Reason: logsReasonNodeOffline}
	}

	frame, err := h.taskLogs.FetchLogs(ctx, namespaceID, nodeID, task.ID)
	if err != nil {
		reason := logsReasonForError(err)
		slog.Warn("fetch task logs failed",
			"event", "console.task_logs.fetch_failed",
			"task_id", task.ID,
			"node_id", nodeID,
			"reason", reason,
			"error", err.Error(),
		)
		return taskLogsView{Reason: reason}
	}
	if frame == nil {
		// 防御：实现方回了 (nil, nil)。契约里没有这个形状，但**绝不能**因此回 5xx
		// 或者把 available=true 的空结果当成「取到了」——按「没取到」处理。
		slog.Warn("task logs source returned nil frame",
			"event", "console.task_logs.nil_frame",
			"task_id", task.ID,
			"node_id", nodeID,
		)
		return taskLogsView{Reason: logsReasonTimeout}
	}

	return taskLogsView{
		Stdout:    frame.Stdout,
		Stderr:    frame.Stderr,
		Truncated: frame.Truncated,
		Bytes:     frame.Bytes,
		Available: true,
	}
}

// logsReasonForError 把取数错误折成**封闭集合**里的一个 reason（契约 §2）。
//
// 只有两个值能从错误里判出来：节点离线、档案不存在。其余（超时、写帧失败 =
// 连接已断、对端回了别的 error 串、ctx 取消 = 浏览器关了页面）全都折成 timeout：
//
//   - 对用户来说它们是同一件事：「这次没取到，可能再点一次就好了」；
//   - reason 是前端文案的键（四语言），加一个没人翻译的取值等于给用户看空白。
//
// 这里用 errors.Is 而不是 ==：ws.ErrNoLocalArchive 是 errors.New 出来的哨兵，
// 实现方（以及将来可能的包装层）用 %w 包一层也必须能认出来。
func logsReasonForError(err error) string {
	switch {
	case errors.Is(err, ws.ErrNodeOffline):
		return logsReasonNodeOffline
	case errors.Is(err, ws.ErrNoLocalArchive):
		return logsReasonNoLocalArchive
	case errors.Is(err, ws.ErrLogsTimeout):
		// 显式写出来只为可读性：下面的 default 与它同一个结果。
		return logsReasonTimeout
	default:
		return logsReasonTimeout
	}
}
