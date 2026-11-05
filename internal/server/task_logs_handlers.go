package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/ws"
)

// 本文件是「任务日志」的两个 HTTP 端点（CONTRACT-task-logs §2 / §4）：
//
//	POST /v1/node/tasks/{taskId}/logs   （node token，调用方是节点上的 agent）
//	GET  /v1/tasks/{taskId}/logs?namespaceId=…（会话 + 命名空间隔离，调用方是控制台）
//
// 为什么日志不走 result 帧：帧的软上限是 1 MiB，而同一条 WS 连接上还跑着心跳与
// assign；一次几十 MB 的构建日志塞进帧里，被堵死的是**心跳**，症状看起来像「节点掉线」。
// 所以日志单独走一条普通 HTTP、单独落一张表（task_logs），热路径（任务列表 / stats /
// 巡检）一行都不碰它。
//
// 为什么上传端点用 **node token** 而不是会话：上传方是节点上的 agent，它手里只有
// 自己的 node token（= 它属于哪个命名空间），没有浏览器会话。这也顺带把归属钉死了：
// 命名空间来自令牌，不由请求体声明——调用方无法替别人的命名空间写日志。
//
// 为什么读端点用 `?namespaceId=` 而不是路径段：它取的是**单个对象**（一个任务的一份日志），
// 命名空间在这里是「这个资源属于谁」的收口参数，与 /v1/tasks/{id} 同口径；路径段形状
// （/v1/namespaces/{ns}/tasks/{taskId}/…）留给「按命名空间列子资源」的那类端点（如 emissions）。

// maxTaskLogsBody 是任务日志上传的请求体硬上限（256 KiB，契约 §2）。
//
// 为什么是 256 KiB 而不是「随便多大」：agent 侧已经把每个流截断到 32 KiB（合计 64 KiB），
// 留 4 倍余量给 JSON 转义、中文多字节与截断标记。超限说明**采集侧的截断逻辑坏了**，
// 这时候拒绝（413）比默默存一份 20 MB 的日志有用得多——后者会同时拖慢库和读端点。
const maxTaskLogsBody = 256 << 10

// taskLogsBody 是上传请求体的形状（字段名逐字，agent 侧就是这么编的）。
//
// Truncated 必须由采集侧给：它不能由长度反推（恰好 32 KiB 的日志反推不出来），
// 而「你看的不是全部」这句话必须说得准。Bytes 是截断**前**两流的真实字节数之和。
type taskLogsBody struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated"`
	Bytes     int64  `json:"bytes"`
}

// taskLogsView 是读端点的响应形状：**恰好四个字段**（契约 §4）。
//
// 不额外塞 taskId / namespaceId / createdAt：调用方已经知道自己在问哪个任务，
// 加进来只会让「这份日志是谁的」出现两个事实源。stdout / stderr 是原文返回——
// 包括节点侧追加的截断标记，**不 trim、不转义、不再裁剪**：中枢再裁一次，
// 截断标记里「共 N 字节」到底指哪一层的 N 就说不清了。
type taskLogsView struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated"`
	Bytes     int64  `json:"bytes"`
}

// taskLogUploadHandler 处理 POST /v1/node/tasks/{taskId}/logs（CONTRACT-task-logs §2）。
//
// 鉴权是 **node token**（AuthenticateNodeHTTP），不是会话 Cookie——与
// GET /v1/node/binary 同一套口径：调用方是机器，不是浏览器。
//
// 幂等：同一任务重复上传直接覆盖（store 层是 ON CONFLICT DO UPDATE），handler 不额外分支。
// 这条幂等是硬要求：重派、agent 重启补传、客户端重试都会重复上传同一条任务。
type taskLogUploadHandler struct {
	db   *store.DB
	auth ws.NodeAuthenticator
}

func (h *taskLogUploadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	// 鉴权放在读 body 之前：未鉴权的调用方不该让我们先吃掉 256 KiB 的请求体。
	node, err := ws.AuthenticateNodeHTTP(r.Context(), h.auth, r)
	if err != nil {
		// 401 = 没带令牌；403 = 令牌无效 / 命名空间没了。响应体不区分后两者：
		// 对调用方来说它们是同一件事（这个令牌不能用），细节进服务端日志。
		status := ws.HTTPStatusForAuthError(err)
		msg := "invalid_node_token"
		if status == http.StatusUnauthorized {
			msg = "missing_node_token"
		}
		slog.Warn("task log upload auth failed",
			"event", "node.task_logs.auth_failed",
			"status", status,
			"error", err.Error(),
			"ip", r.RemoteAddr,
		)
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	taskID := r.PathValue("taskId")
	if taskID == "" {
		writeErr(w, http.StatusBadRequest, "missing task id")
		return
	}

	// 硬上限（契约 §2）：解不开 + 超限要分开报——前者是「agent 编错了」，
	// 后者是「agent 忘了截断」，两者的处置完全不同。
	//
	// 解码成功后还要把请求体的**剩余字节读掉**再判一次：json.Decoder 停在第一个
	// 完整的 JSON 值之后，尾巴根本没读——「小 JSON + 1 MB 尾巴」这种请求体会
	// 绕过上限（契约要求的是**请求体** 256 KiB，不是第一份 JSON）。
	r.Body = http.MaxBytesReader(w, r.Body, maxTaskLogsBody)
	var body taskLogsBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		if writeTooLarge(w, err) {
			return
		}
		writeErr(w, http.StatusBadRequest, "invalid_json")
		return
	}
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		if writeTooLarge(w, err) {
			return
		}
		// 除超限之外的读错误（连接被断等）：JSON 已经完整解出来了，按成功处理——
		// 不能因为客户端提前关了写端就把一份已经收齐的日志扔掉。
	}

	// 归属：任务必须在这个令牌的命名空间里，且**确实是这台节点跑的**。
	// 任一条件不成立都回同一个 404（与读取端点的口径一致）：对节点来说
	// 「没这条任务」与「不是你的任务」是同一件事，细节进服务端日志。
	task, err := h.db.GetTask(r.Context(), node.NamespaceID, taskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "task_not_found")
			return
		}
		slog.Error("get task for log upload failed",
			"event", "node.task_logs.task_failed",
			"task_id", taskID,
			"node_id", node.ID,
			"error", err.Error(),
		)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if task.NodeID == nil || *task.NodeID != node.ID {
		// 真实 node_id 进日志：这条 WARN 是「谁在给别人的任务写日志」的唯一证据
		// （重派后老节点补传就会落到这里，属预期噪声，但不该静默）。
		taskNodeID := ""
		if task.NodeID != nil {
			taskNodeID = *task.NodeID
		}
		slog.Warn("task log upload for task of another node",
			"event", "node.task_logs.foreign_node",
			"task_id", taskID,
			"node_id", node.ID,
			"task_node_id", taskNodeID,
		)
		writeErr(w, http.StatusNotFound, "task_not_found")
		return
	}

	logs := &store.TaskLogs{
		TaskID:      taskID,
		NamespaceID: node.NamespaceID,
		Stdout:      body.Stdout,
		Stderr:      body.Stderr,
		Truncated:   body.Truncated,
		Bytes:       body.Bytes,
		// 幂等覆盖时 created_at 一起重写：「这份内容是什么时候到的」比
		// 「第一次是什么时候到的」对排障更有用（store 侧注释同口径）。
		CreatedAt: store.Now(),
	}
	if err := h.db.UpsertTaskLogs(r.Context(), logs); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// 先查后写之间任务被删/改派：与上面同一件事，回 404。
			writeErr(w, http.StatusNotFound, "task_not_found")
			return
		}
		slog.Error("upsert task logs failed",
			"event", "node.task_logs.write_failed",
			"task_id", taskID,
			"node_id", node.ID,
			"error", err.Error(),
		)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	// 204：没有可回的内容（日志已落库），也没有资源需要回显。
	w.WriteHeader(http.StatusNoContent)

	slog.Info("task logs uploaded",
		"event", "node.task_logs.uploaded",
		"task_id", taskID,
		"node_id", node.ID,
		"bytes", logs.Bytes,
		"stdout_len", len(logs.Stdout),
		"stderr_len", len(logs.Stderr),
		"truncated", logs.Truncated,
	)
}

// writeTooLarge 把「请求体超过 256 KiB」翻成 413 并返回 true。
//
// 用 errors.As 判 *http.MaxBytesError 而不是字符串匹配：文案是标准库的，
// 但拼写会变（也不该把标准库文案当协议）。读 body 与读剩余字节两处共用它，
// 保证「超限」这个判定只有一份实现。
func writeTooLarge(w http.ResponseWriter, err error) bool {
	var tooLarge *http.MaxBytesError
	if !errors.As(err, &tooLarge) {
		return false
	}
	writeErr(w, http.StatusRequestEntityTooLarge, "payload_too_large")
	return true
}

// taskLogs 处理 GET /v1/tasks/{taskId}/logs?namespaceId=…（CONTRACT-task-logs §4）。
//
// 三条不能变的语义：
//  1. **任务存在但没有日志 → 200 + 空对象**：「没日志」是合法状态（老任务、
//     上传时 agent 挂了、日志功能上线前跑的任务）；做成 404 会让前端在正常路径上
//     处理错误。
//  2. 跨命名空间与「不存在」逐字节相同（404 task not found）——与 /v1/tasks/{id} 同口径，
//     调用方无法据此探测别的命名空间里有没有某个 taskId。
//  3. 响应恰好四个字段（stdout / stderr / truncated / bytes），原文透传。
func (h *consoleHandler) taskLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	taskID := r.PathValue("taskId")
	if taskID == "" {
		writeErr(w, http.StatusBadRequest, "missing task id")
		return
	}
	// 单对象读端点：`*` 无定义 → 400（allowAll=false）。
	// 缺 namespaceId 也是 400：绝不回退默认命名空间（回退等于把人写错的地方藏起来）。
	ns, ok := h.resolveNamespace(w, r, "", false)
	if !ok {
		return
	}

	// 任务行必须先在本命名空间可见：这一步同时挡住了「拿别的命名空间的 taskId 读日志」。
	task, err := h.db.GetTask(r.Context(), ns.ID, taskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "task not found")
			return
		}
		if handleNamespaceError(w, err) {
			return
		}
		slog.Error("get task for logs failed",
			"event", "console.task_logs.task_failed",
			"task_id", taskID,
			"error", err.Error(),
		)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	logs, err := h.db.GetTaskLogs(r.Context(), ns.ID, task.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// 任务在、日志没有 = 合法状态：200 + 四字段全空（契约 §4）。
			// 注意绝不能在这里回 404——前端会把「老任务的日志还没上传」当成一次失败。
			writeJSON(w, http.StatusOK, taskLogsView{})
			return
		}
		if handleNamespaceError(w, err) {
			return
		}
		slog.Error("get task logs failed",
			"event", "console.task_logs.get_failed",
			"task_id", taskID,
			"error", err.Error(),
		)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, taskLogsView{
		Stdout:    logs.Stdout,
		Stderr:    logs.Stderr,
		Truncated: logs.Truncated,
		Bytes:     logs.Bytes,
	})
}
