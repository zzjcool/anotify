package store

// 能力 `status.observed` 的事实查询（capability.md §2「observed」/ CONTRACT-observed）。
//
// 这一层只交付**事实**，不做展示裁剪：没有历史就是零值，由 server 决定「什么时候写
// 空对象」。空集合一律是 `[]` 而不是 `nil`（契约：空列表不是 null）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// observed 的三个上限/窗口都是**读面契约**的一部分：测试直接断言它们，查询里因此
// 不许再出现魔法数（否则改常量不改 SQL，断言就变成一句空话）。
const (
	// ObservedEmitLimit 是 observed.emits[] 的上限（契约「查询与上限」第 2 条）。
	ObservedEmitLimit = 50
	// ObservedPruneLimit 是 observed.pruned[] 的上限（契约第 3 条，与 emits 同值）。
	ObservedPruneLimit = 50
	// ObservedWindow 是 observed.tasks{total,failed} 的统计窗口（近 24h，契约第 4 条）。
	// 窗口起点按调用时刻现算（Now()-ObservedWindow）：不落库、不缓存，读两次可能不同。
	ObservedWindow = 24 * time.Hour
)

// CapabilityObserved 是一个能力的观察事实（status.observed 的 store 侧形状）。
//
// 字段与 capability.md §2 的四个键一一对应，但**不做嵌套**：emits/pruned 的条目
// 各自是元素类型，tasks 摊平成 Total/Failed（server 负责拼回 `tasks{total,failed}`）。
type CapabilityObserved struct {
	// TaskID 是最近一个逻辑任务的 id（按 created_at DESC, id DESC 的第一条）；无历史时为空。
	TaskID string
	// Emits 是最近任务自己的 emit（按 created_at, id 正序，至多 ObservedEmitLimit 条）。
	Emits []CapabilityObservedEmit
	// Pruned 是最近任务里被剪掉的下游（按 created_at, id 正序，至多 ObservedPruneLimit 条）。
	Pruned []CapabilityObservedPrune
	// Total / Failed 是 ObservedWindow 内该能力的任务数与其中 phase=failed 的数量。
	Total  int
	Failed int
}

// CapabilityObservedEmit 是一次 emit 的观察行。
type CapabilityObservedEmit struct {
	// To 是目标引用（`name` 或 `name@version`，来自 emits.to_ref 原样）。
	To string
	// Fields 是本次请求体（emits.data）的**键名**，排序后；非对象请求体没有键名，给 []。
	Fields []string
	// OK 是「这次 emit 有没有真的建出任务」，判定规则见 observedEmitOK。
	OK bool
}

// CapabilityObservedPrune 是一条剪枝事实（事件流里的 route 观察事件）。
type CapabilityObservedPrune struct {
	// To 是没被派发出去的那个下游（events.payload.capability）。
	To string
	// Reason 是剪枝码（events.type；闭集见 protocol.PruneReasons）。
	Reason string
	// Detail 是人话补充（events.payload.detail），可为空。
	Detail string
}

// observedCountsSQL 是 24h 窗口计数（契约第 4 条）。
//
// 形状被 capability_observed_test.go 的计划断言钉住：`(ns, capability, created_at>=?)`
// 必须走 idx_tasks_cap_created 的三段 seek。改这条 SQL 就要连 EXPLAIN 断言一起改。
const observedCountsSQL = `SELECT count(*), count(CASE WHEN phase = 'failed' THEN 1 END)
	  FROM tasks
	 WHERE namespace_id = ? AND capability = ? AND created_at >= ?`

// observedPruneSQL 取「最近那个任务自己发起的」剪枝观察事件。
//
// trace_id 走既有 idx_events_trace(ns, trace_id) 定位一个 trace 的事件，再用
// payload.taskId 收窄到这个任务本身（同一个 trace 里其它任务/源投递的剪枝不算它的）。
// 过滤写在 SQL 里而不是读出来再筛：LIMIT 必须作用在**匹配行**上，否则一个 trace 里
// 别的任务的剪枝会把真正的 50 条挤出去。
// CASE 包住 json_extract：历史上若有坏 payload，宁可当作不匹配，也不让整条查询报错。
const observedPruneSQL = `SELECT type, payload
	  FROM events
	 WHERE namespace_id = ? AND trace_id = ? AND source = ?
	   AND CASE WHEN json_valid(payload) THEN json_extract(payload, '$.taskId') END = ?
	 ORDER BY created_at, id
	 LIMIT ?`

// ObservedForCapability 汇总一个能力的 status.observed 事实。
//
// 四段查询各走哪条索引（改形状前先跑 EXPLAIN，测试里有断言）：
//
//  1. 最近任务：`ORDER BY created_at DESC, id DESC LIMIT 1` 走**既有**
//     idx_tasks_ns_created(namespace_id, created_at, id) 的**反向扫**——零排序，
//     取到第一条命中 capability 的行就停。刻意不为它加 (ns, capability, created_at)
//     索引：实测规划器要么仍选 ns_created，要么多出一个 temp B-tree，都更差。
//  2. 24h 计数：三段 seek idx_tasks_cap_created(ns, capability, created_at DESC, id)。
//     这是这条索引存在的**唯一**理由。
//  3. emits：既有 idx_emits_task(task_id)。
//  4. pruned：既有 idx_events_trace(ns, trace_id)。
//
// 没有最近任务行就直接返回零值：窗口计数是全体任务行的子集，必然为 0，不必再查。
func (d *DB) ObservedForCapability(ctx context.Context, namespaceID, capability string) (CapabilityObserved, error) {
	out := CapabilityObserved{
		Emits:  []CapabilityObservedEmit{},
		Pruned: []CapabilityObservedPrune{},
	}
	if namespaceID == "" || capability == "" {
		return out, fmt.Errorf("observed: namespace_id 与 capability 都必填")
	}

	var traceID string
	err := d.queryRowContext(ctx,
		`SELECT id, trace_id FROM tasks
		  WHERE namespace_id = ? AND capability = ?
		  ORDER BY created_at DESC, id DESC
		  LIMIT 1`,
		namespaceID, capability).Scan(&out.TaskID, &traceID)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("observed %s/%s: 读最近任务: %w", namespaceID, capability, err)
	}

	windowStart := Now() - int64(ObservedWindow/time.Second)
	if err := d.queryRowContext(ctx, observedCountsSQL, namespaceID, capability, windowStart).
		Scan(&out.Total, &out.Failed); err != nil {
		return out, fmt.Errorf("observed %s/%s: 窗口计数: %w", namespaceID, capability, err)
	}
	if err := d.observedEmits(ctx, out.TaskID, &out); err != nil {
		return out, err
	}
	if err := d.observedPrunes(ctx, namespaceID, traceID, out.TaskID, &out); err != nil {
		return out, err
	}
	return out, nil
}

// observedEmits 读最近任务的 emit 行（至多 ObservedEmitLimit 条）。
func (d *DB) observedEmits(ctx context.Context, taskID string, out *CapabilityObserved) error {
	rows, err := d.queryContext(ctx,
		`SELECT to_ref, data, result FROM emits
		  WHERE task_id = ?
		  ORDER BY created_at, id
		  LIMIT ?`,
		taskID, ObservedEmitLimit)
	if err != nil {
		return fmt.Errorf("observed %s: 读 emits: %w", taskID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			toRef  string
			data   string
			result string
		)
		if err := rows.Scan(&toRef, &data, &result); err != nil {
			return fmt.Errorf("observed %s: 扫描 emit: %w", taskID, err)
		}
		out.Emits = append(out.Emits, CapabilityObservedEmit{
			To:     toRef,
			Fields: observedEmitFields(json.RawMessage(data)),
			OK:     observedEmitOK(json.RawMessage(result)),
		})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("observed %s: 迭代 emits: %w", taskID, err)
	}
	return nil
}

// observedPrunes 读最近任务的剪枝观察事件（至多 ObservedPruneLimit 条）。
func (d *DB) observedPrunes(ctx context.Context, namespaceID, traceID, taskID string, out *CapabilityObserved) error {
	rows, err := d.queryContext(ctx, observedPruneSQL, namespaceID, traceID, EventPruneSource, taskID, ObservedPruneLimit)
	if err != nil {
		return fmt.Errorf("observed %s: 读剪枝事件: %w", taskID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			eventType string
			payload   string
		)
		if err := rows.Scan(&eventType, &payload); err != nil {
			return fmt.Errorf("observed %s: 扫描剪枝事件: %w", taskID, err)
		}
		var body struct {
			Capability string `json:"capability"`
			Detail     string `json:"detail"`
		}
		// 坏 payload 只丢 detail/capability，不丢这条事实：reason 来自 events.type，
		// 那才是判断依据；payload 只是展示补充。
		_ = json.Unmarshal([]byte(payload), &body)
		out.Pruned = append(out.Pruned, CapabilityObservedPrune{
			To:     body.Capability,
			Reason: eventType,
			Detail: body.Detail,
		})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("observed %s: 迭代剪枝事件: %w", taskID, err)
	}
	return nil
}

// observedEmitFields 取请求体的**键名**（排序后）。
//
// `data` 是调用方原样提交的 JSON。非对象（数组/标量/null）没有「字段名」可言，给 []；
// 排空 `{}` 同样是 []。排序是为了让 `fields` 稳定（同一请求体不该因为 map 迭代顺序
// 在两次读里换个样子）。
func observedEmitFields(data json.RawMessage) []string {
	out := []string{}
	if len(data) == 0 {
		return out
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return out
	}
	names := make([]string, 0, len(obj))
	for key := range obj {
		names = append(names, key)
	}
	sort.Strings(names)
	return names
}

// observedEmitOK 从 emit 回执（emits.result）判定这次 emit **有没有真的建出任务**。
//
// result 存的是首次派发回执的**原字节**（emission.go 的 emitResult：调用方给了就原样
// 回放，没给就按建出的行生成 `{"taskIds":[…]}`），所以现存的形状只有这几种：
//
//	空 result                                     → true（契约「无 result 视为 ok」：
//	                                                未写回执的旧事实不因此被判成失败）
//	{"taskId":"tsk_…","to":…,"eventId":…}         → true（回执点名了本次真实建出的任务）
//	{"taskId":null,…,"pruned":{"reason":…}}       → **false**（route/emit.go 的剪枝回执：
//	                                                事件收到了、下游没建 task —— 这正是
//	                                                observed.pruned[] 记的那一条）
//	{"taskIds":["tsk_…",…]}                       → true；空数组 → false（没有任何派发事实）
//	其余（含非法 JSON）                            → false：坏字节不是「派发成功」的证据，
//	                                                静默算 ok 会让一条损坏行永远看起来正常
//
// 刻意**不**回查 tasks 判 phase：下游任务执行失败（phase=failed，含 downstream_missing
// 的派发失败任务）仍然是「派发成功」——它确实建出了行。两个字段分头回答不同问题：
// ok 说「发出去没有」，tasks.failed 说「发出去之后成不成」。
func observedEmitOK(result json.RawMessage) bool {
	if len(result) == 0 {
		return true
	}
	var receipt struct {
		TaskID  *string          `json:"taskId"`
		TaskIDs []string         `json:"taskIds"`
		Pruned  *json.RawMessage `json:"pruned"`
	}
	if err := json.Unmarshal(result, &receipt); err != nil {
		return false
	}
	if receipt.Pruned != nil {
		return false
	}
	if receipt.TaskID != nil && *receipt.TaskID != "" {
		return true
	}
	return len(receipt.TaskIDs) > 0
}
