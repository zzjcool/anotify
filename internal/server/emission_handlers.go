package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/zzjcool/anotify/internal/store"
)

// ---------- GET /v1/namespaces/{ns}/tasks/{taskId}/emissions ----------
//
// P1.6（docs/design/typed-pipeline.md §1.3 / §2、docs/design/emit-protocol.md §3）：
// emit 已经是唯一的产出通道，而「这次运行到底发了什么」在控制台上必须**看得见**。
// 这个端点就是那件事的读口：一个任务的每一次 emit（含被剪枝的那次），按 seq 升序。
//
// 为什么是 `/v1/namespaces/{ns}/tasks/{taskId}/emissions` 而不是
// `/v1/tasks/{id}/emissions?namespaceId=`：
// emissions 是**任务的子资源**，而任务是命名空间资源（I10/I11）。把它写成
// 路径段让「谁的作用域」这件事长在 URL 上，而不是靠一个可以被漏掉的 query——
// 漏了 namespaceId 的任务端点一律 400（契约 §6.2），那是能查出来的；但漏了
// 作用域的**子资源**查询会变成「拿 id 猜数据」，这里不给自己留那个形状。
//
// 归属判定只有两步，且都在 store 的命名空间作用域里：
//  ① 命名空间必须存在且已登录（resolvePathNamespace，与其它资源端点同一份规则）；
//  ② 任务必须在**这个**命名空间里取得到（GetTask(ns, taskID)）。
// 跨命名空间的 taskId 一律 404（不是 403）：组织层删除后（#204）本部署内不存在
// 「有、但属于别人」这一态，所以「看不见」与「不存在」的回答必须逐字节相同。
//
// 数据形状固定：`data` 是**原样 JSON 对象**（不是字符串，前端不必再 parse 一次）；
// 空列表是 `[]` 而不是 null（不变量 I9：前端不能把「还没 emit」和「查不到」混了）。

// emissionView 是一次 emit 的读视图（§3.1 的帧 + §3.2 的派发计数）。
//
// 字段全部常驻、不带 omitempty：trace 页要逐条渲染固定列，缺字段会让前端
// 每个字段都要判一次空，且「routed=0」与「没有 routed」在语义上完全不同
// （前者是合法结果：这个出口没接下游 / 全被剪枝）。
//
// Seq 从 1 开始、任务内单调递增（emit-protocol §4）：它就是「第几次 emit」，
// 也是幂等键的一半，所以页面上必须显式显示，不能藏成行号。
type emissionView struct {
	Seq  int64           `json:"seq"`
	Port string          `json:"port"`
	Data json.RawMessage `json:"data"`
	// Note 是 emit 帧里那句人类可读备注（§3.1，「不参与路由」）。
	//
	// 目前恒为空串：emissions 表还没有 note 列（迁移 22 只存路由所需字段 +
	// 审计用的 data），中枢收到帧后 note 只用于日志。字段先占位，是为了让
	// 响应形状**现在**就固定下来——补列之后这里直接接上，前端不用改一版。
	// 见 P1.6 上报里的「发现的产品缺口」。
	Note      string `json:"note"`
	Routed    int    `json:"routed"`
	Pruned    int    `json:"pruned"`
	CreatedAt int64  `json:"createdAt"`
}

// newEmissionView 把落库行翻成响应对象。
//
// data 的兜底不是洁癖：`json.RawMessage` 里若出现空字节，编码器会直接
// 写 `null`（前端拿到 null 之后 .xxx 就抛），而「没有 data」的语义是 `{}`
// （emit-protocol §3.1：data 可缺省 = {}）。所以这里统一折成 `{}`。
func newEmissionView(e *store.Emission) emissionView {
	data := e.Data
	if len(data) == 0 {
		data = json.RawMessage("{}")
	}
	return emissionView{
		Seq:       e.Seq,
		Port:      e.Port,
		Data:      data,
		Routed:    e.Routed,
		Pruned:    e.Pruned,
		CreatedAt: e.CreatedAt,
	}
}

// emissions 处理 GET /v1/namespaces/{ns}/tasks/{taskId}/emissions。
func (h *consoleHandler) emissions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	taskID := r.PathValue("taskId")
	if taskID == "" {
		writeErr(w, http.StatusBadRequest, "missing task id")
		return
	}
	// 子资源也是命名空间资源：`*` 无定义（跨命名空间汇总一条任务的 emit 没有含义）→ 400。
	ns, ok := h.resolvePathNamespace(w, r, false)
	if !ok {
		return
	}

	// 任务行必须先在本命名空间可见：这一步同时挡住了「拿别的命名空间的 taskId 读 emit」。
	task, err := h.db.GetTask(r.Context(), ns.ID, taskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "task not found")
			return
		}
		if handleNamespaceError(w, err) {
			return
		}
		slog.Error("get task for emissions failed",
			"event", "console.emissions.task_failed",
			"task_id", taskID,
			"error", err.Error(),
		)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	rows, err := h.db.ListEmissions(r.Context(), task.ID)
	if err != nil {
		slog.Error("list emissions failed",
			"event", "console.emissions.list_failed",
			"task_id", task.ID,
			"error", err.Error(),
		)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	// 空列表是 [] 不是 null（I9）：store 已经返回非 nil 切片，这里再兜一次，
	// 是为了「哪天 store 改了返回 nil」时前端不会突然收到 null。
	out := make([]emissionView, 0, len(rows))
	for _, e := range rows {
		out = append(out, newEmissionView(e))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"taskId":      task.ID,
		"namespaceId": ns.ID,
		"emissions":   out,
	})
}
