package store

import (
	"context"
	"fmt"
	"strings"
)

// 保留策略（config.md §3.6）。
//
// 三张表只进不出会把查询和写入一起拖垮，所以每轮巡检按**中枢系统配置**的 TTL 清理：
//
//	retention.eventTtlSec   events（含剪枝留痕）→ 删除
//	retention.taskTtlSec    终态任务（emits 级联）→ 删除
//	retention.resultTtlSec  只在节点侧清结果目录时用；store **不碰文件系统**
//
// 两条硬规则：
//   - **0 = 永久**（不删）。三种 TTL 各自独立，谁也不代谁兜底。
//   - 活动任务（pending/running）永不删：它们没有 finished_at，也没有「过期」可言。
//
// 范围是 events / tasks 与**过期的投递去重记录**（event_deliveries）。
//
// 两条已裁决的边界（父 D3/D6）：
//   - **引用组安全**：一条事件只要其 (namespace, trace_id) 还有任务行就不删
//     （trace 视图里「根还在、链还在跑」是正在被看的东西）；任务被清掉之后
//     下一轮自然就不再保护它。
//   - **回执不进自动 sweeper**：`playbook_installs` 是安装审计与 installed_from 的锚点，
//     它的保留期是产品决策，不是「事件过期」的副作用。本函数一行回执都不删，
//     只保留调用方显式给 cutoff 的 primitive（DeleteExpiredPlaybookInstalls）。

// DefaultRetentionBatch 是单条 DELETE 的行数上限（先度量后删除）。
const DefaultRetentionBatch = 1000

// DefaultRetentionBatches 是每轮最多跑几批（单轮预算，避免长时间持写锁）。
const DefaultRetentionBatches = 20

// RetentionStats 汇总一轮清理（日志与运维视图用）。
type RetentionStats struct {
	// EventTTLSec / TaskTTLSec / ResultTTLSec 是本轮生效的配置（0 = 永久）。
	EventTTLSec  int64 `json:"eventTtlSec"`
	TaskTTLSec   int64 `json:"taskTtlSec"`
	ResultTTLSec int64 `json:"resultTtlSec"`
	// EventsDeleted / TasksDeleted 是本轮删掉的行数。
	EventsDeleted int64 `json:"eventsDeleted"`
	TasksDeleted  int64 `json:"tasksDeleted"`
	// DeliveriesDeleted 是本轮清掉的**过期投递去重记录**数（只清窗口，不删事件实体）。
	DeliveriesDeleted int64 `json:"deliveriesDeleted"`
	// EventsSkipped / TasksSkipped 表示对应阶段因 TTL=0 被跳过
	// （「压根没扫」与「扫了但没东西删」必须能区分开）。
	EventsSkipped bool `json:"eventsSkipped"`
	TasksSkipped  bool `json:"tasksSkipped"`
	// Truncated 表示某阶段被批数预算截断（还有积压，下一轮继续）。
	Truncated bool `json:"truncated"`
}

// RunRetention 跑一轮保留期清理（events + tasks）。
//
// now 由调用方给（测试可注入）；0 → Now()。配置从 system_config 读（GetSystemConfig），
// 所以「运维改了保留期」下一轮就生效，不需要重启。
//
// 顺序固定为「先执行数据（tasks）→ 再事件」：中断时残留的方向恒定（下游只会比根先少），
// 不会出现「事件没了、任务还在跑」这种看起来像链路断裂的形状。
//
// 返回的 stats 在出错时也是**部分有效**的：一轮可能删了一部分才被打断，把计数丢掉
// 等于「删了但没人知道」。调用方（lease 巡检）把 stats 打进日志即可，不要中断服务。
func (d *DB) RunRetention(ctx context.Context, now int64) (RetentionStats, error) {
	var stats RetentionStats
	if now == 0 {
		now = Now()
	}
	cfg, err := d.GetSystemConfig(ctx)
	if err != nil {
		return stats, fmt.Errorf("run retention: %w", err)
	}
	stats.EventTTLSec = cfg.RetentionEventTTLSec
	stats.TaskTTLSec = cfg.RetentionTaskTTLSec
	stats.ResultTTLSec = cfg.RetentionResultTTLSec

	// ① 终态任务（执行数据）：emits 由外键 ON DELETE CASCADE 一起回收。
	if stats.TaskTTLSec <= 0 {
		stats.TasksSkipped = true
	} else {
		cutoff := now - stats.TaskTTLSec
		n, truncated, derr := drainRetention(ctx, func(limit int) (int64, error) {
			return d.DeleteExpiredTasks(ctx, cutoff, limit)
		})
		stats.TasksDeleted = n
		stats.Truncated = stats.Truncated || truncated
		if derr != nil {
			return stats, fmt.Errorf("run retention: tasks: %w", derr)
		}
	}

	// ② 事件（含剪枝留痕），按**引用组安全**保守清理：trace 上还有任务行的事件不删
	// （见 DeleteExpiredEvents）。去重窗是独立记录（event_deliveries），它的过期回收
	// 只清窗口、不删事件实体。
	if stats.EventTTLSec <= 0 {
		stats.EventsSkipped = true
	} else {
		cutoff := now - stats.EventTTLSec
		n, truncated, derr := drainRetention(ctx, func(limit int) (int64, error) {
			return d.DeleteExpiredEvents(ctx, cutoff, limit)
		})
		stats.EventsDeleted = n
		stats.Truncated = stats.Truncated || truncated
		if derr != nil {
			return stats, fmt.Errorf("run retention: events: %w", derr)
		}
		// 过期的**去重记录**可以清（它只是一条不再起作用的窗口）；事件实体不受影响。
		// 回执不在这条路径上——父裁决：自动 sweeper 不删回执。
		dn, dtrunc, derr2 := drainRetention(ctx, func(limit int) (int64, error) {
			return d.DeleteExpiredEventDeliveries(ctx, now, limit)
		})
		stats.DeliveriesDeleted = dn
		stats.Truncated = stats.Truncated || dtrunc
		if derr2 != nil {
			return stats, fmt.Errorf("run retention: event deliveries: %w", derr2)
		}
	}

	// ③ playbook 回执**不在这里删**：回执不是「事件类数据」，它的保留期不是
	// eventTtlSec，也不是 taskTtlSec（删除动作见 DeleteExpiredPlaybookInstalls，
	// 由调用方显式给 cutoff）。事件清理顺手删回执 = 删掉安装审计并让
	// installed_from 悬空，那是数据丢失，不是保留策略。
	return stats, nil
}

// DeleteExpiredTasks 删除可信终态观察早于 cutoff 的任务，单次最多 limit 行。
// 收到 Node result 用首 result_accepted_at；服务器裁定的未上报终态用 finished_at。
// Node 报告的进程 finish（可很旧/很未来）不驱动保留期。
//
// 限批形式必须是 `DELETE ... WHERE id IN (SELECT ... LIMIT ?)`：`DELETE ... LIMIT`
// 需要 SQLite 编译期开关，依赖它等于把可移植性交给构建产物的运气。
// 按可信观察时间排序：一批删不完时下一轮从更老的开始，积压再大也不会「新的先删」。
//
// **物理事实保留（G1-6S）**：任务行不能比它的执行事实先消失。只要还有一条
// `task_attempts.reaped_at IS NULL` 的 attempt（进程组是否回收没被可信事实证明），
// 这条终态任务就不删：删了之后槽位绑定/历史 attempt 会随外键级联消失，旧进程再也
// 无从对被回收，同一个物理槽可能被新任务复用。
// 代价（接受）：一个永远拿不到 reaped 事实的坏节点会把对应任务钉到人工处理。
//
// limit <= 0 直接返回 0 而不删：调用方传错时「少删」远好于「删全表」。
func (d *DB) DeleteExpiredTasks(ctx context.Context, cutoff int64, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	phaseIn, args := terminalPhaseIn()
	args = append(args, cutoff, limit)
	res, err := d.ExecContext(ctx,
		`DELETE FROM tasks WHERE id IN (
		   SELECT t.id FROM tasks t
		    WHERE COALESCE(t.result_accepted_at, CASE WHEN t.reported=0 THEN t.finished_at END) IS NOT NULL AND t.phase IN (`+phaseIn+`) AND COALESCE(t.result_accepted_at, CASE WHEN t.reported=0 THEN t.finished_at END) < ?
		      AND NOT EXISTS (SELECT 1 FROM node_instances i WHERE i.busy_task=t.id)
		      AND NOT EXISTS (SELECT 1 FROM task_artifact_uploads u WHERE u.task_id=t.id AND u.expires_at > unixepoch())
		      AND NOT EXISTS (
		            SELECT 1 FROM task_attempts a
		             WHERE a.task_id = t.id AND a.reaped_at IS NULL)
		    ORDER BY COALESCE(t.result_accepted_at, CASE WHEN t.reported=0 THEN t.finished_at END)
		    LIMIT ?)`, args...)
	if err != nil {
		return 0, fmt.Errorf("delete expired tasks: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete expired tasks rows affected: %w", err)
	}
	return n, nil
}

// DeleteExpiredEvents 删除 created_at 早于 cutoff 的事件（含剪枝留痕），单次最多 limit 行。
//
// **引用组安全**（父裁决）：只要同一个 (namespace, trace_id) 还有**任务行**，这条 trace
// 的事件就不删——trace 视图里「根事件还在、链还在跑」是用户正在看的东西，
// 提前把根删掉会让一条活着的链路变成孤儿数据。任务行自己的保留期到了之后，
// 下一轮就会把这些事件一起清掉（引用消失 = 保护失效）。
//
// **不按 source 过滤**：剪枝行同样是「只进不出」的增长来源，给它开特例等于把泄漏留在库里。
//
// 注意本函数**不动去重记录**：去重窗的过期判定在接收路径按 expires_at 现算
// （见 InsertEvent），与事件实体是否被删无关。清理过期去重记录是另一件事
// （DeleteExpiredEventDeliveries），它只清窗口记录，绝不删事件实体。
//
// limit <= 0 直接返回 0 而不删：调用方传错时「少删」远好于「删全表」。
func (d *DB) DeleteExpiredEvents(ctx context.Context, cutoff int64, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	res, err := d.ExecContext(ctx,
		`DELETE FROM events WHERE id IN (
		   SELECT e.id FROM events e
		    WHERE e.created_at < ?
		      AND NOT EXISTS (
		            SELECT 1 FROM tasks t
		             WHERE t.namespace_id = e.namespace_id AND t.trace_id = e.trace_id)
		    ORDER BY e.created_at
		    LIMIT ?)`, cutoff, limit)
	if err != nil {
		return 0, fmt.Errorf("delete expired events: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete expired events rows affected: %w", err)
	}
	return n, nil
}

// DeleteExpiredEventDeliveries 清理**已过期**的投递去重记录（只清窗口，不删事件实体）。
//
// 它删的是一条「不再起作用的窗口」：删掉之后同 id 的投递会被当成新事件接收
// （与过期语义完全一致），而旧事件实体该留多久仍由 DeleteExpiredEvents 决定。
//
// limit <= 0 → 0（同其它清理 primitive）。
func (d *DB) DeleteExpiredEventDeliveries(ctx context.Context, now int64, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	res, err := d.ExecContext(ctx,
		`DELETE FROM event_deliveries WHERE rowid IN (
		   SELECT rowid FROM event_deliveries
		    WHERE expires_at > 0 AND expires_at <= ?
		    ORDER BY expires_at
		    LIMIT ?)`, now, limit)
	if err != nil {
		return 0, fmt.Errorf("delete expired event deliveries: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete expired event deliveries rows affected: %w", err)
	}
	return n, nil
}

// DeleteExpiredPlaybookInstalls 删除**未被引用**且 created_at 早于 cutoff 的安装回执。
//
// 它是**独立 primitive**，不挂在任何自动 sweeper 里（父裁决：event/task/result 三类
// 自动清理都不删回执）：回执是安装审计（谁在哪一版装了什么，receipt 原文就在行内），
// 它的保留期是产品决策，不是「事件过期」的副作用。call 它的人必须显式给 cutoff。
//
// 引用保护（**检查与删除在同一个事务里**，否则「查完到删之前刚好有人引用」会删掉活引用）：
//   - capability_versions.installed_from / sources.installed_from 指向的 → 永不删；
//   - 被**保留的 trace** 引用的 → 永不删（安装动作以回执 id 作 trace 根时，
//     那条 trace 的事件/任务还在，回执就是它的锚点）。
//
// 引用列没有外键（回执可以被清、能力行不该跟着走），所以保护只能在这里显式判。
// limit <= 0 直接返回 0（同 DeleteExpiredTasks：少删好于删全表）。
func (d *DB) DeleteExpiredPlaybookInstalls(ctx context.Context, cutoff int64, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("delete expired playbook installs: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`DELETE FROM playbook_installs WHERE id IN (
		   SELECT pi.id FROM playbook_installs pi
		    WHERE pi.created_at < ?
		      AND NOT EXISTS (SELECT 1 FROM capability_versions cv WHERE cv.installed_from = pi.id)
		      AND NOT EXISTS (SELECT 1 FROM sources s WHERE s.installed_from = pi.id)
		      AND NOT EXISTS (SELECT 1 FROM events e WHERE e.trace_id = pi.id)
		      AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.trace_id = pi.id)
		    ORDER BY pi.created_at
		    LIMIT ?)`, cutoff, limit)
	if err != nil {
		return 0, fmt.Errorf("delete expired playbook installs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete expired playbook installs rows affected: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("delete expired playbook installs: commit: %w", err)
	}
	return n, nil
}

// drainRetention 按批调用 one，返回删掉的总行数与「预算是否用尽」。
//
// saturated=true 表示每批都删满（limit 行），即该范围**可能**还有积压——置 Truncated
// 让运维知道下一轮还得跑。某批没删满（含 0）即视为清干净。
func drainRetention(ctx context.Context, one func(limit int) (int64, error)) (deleted int64, saturated bool, err error) {
	for i := 0; i < DefaultRetentionBatches; i++ {
		if err := ctx.Err(); err != nil {
			return deleted, saturated, err
		}
		n, err := one(DefaultRetentionBatch)
		if err != nil {
			return deleted, saturated, err
		}
		deleted += n
		if n < int64(DefaultRetentionBatch) {
			return deleted, false, nil
		}
		saturated = true
	}
	return deleted, saturated, nil
}

// terminalPhaseIn 生成终态列表对应的占位符与参数（与 IsTerminalPhase 同一份事实）。
func terminalPhaseIn() (string, []any) {
	phases := []string{TaskSucceeded, TaskFailed, TaskCancelled, TaskAbandoned}
	ph := make([]string, len(phases))
	args := make([]any, len(phases))
	for i, p := range phases {
		ph[i] = "?"
		args[i] = p
	}
	return strings.Join(ph, ","), args
}

// PurgeDeletedCapabilities physically deletes only immutable tombstone IDs with
// no task, attempt, manifest, deployment, or retained install-receipt reference.
// All predicates and deletion execute as one writer statement/transaction;
// this operation never deletes by name/digest or touches system/user files.
func (d *DB) PurgeDeletedCapabilities(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM capability_versions WHERE id IN (
 SELECT c.id FROM capability_versions c WHERE c.deleted_at IS NOT NULL
 AND NOT EXISTS(SELECT 1 FROM tasks t WHERE t.capability_id=c.id)
 AND NOT EXISTS(SELECT 1 FROM task_attempts a WHERE a.capability_id=c.id)
 AND NOT EXISTS(SELECT 1 FROM task_artifact_manifests m WHERE m.capability_id=c.id)
 AND NOT EXISTS(SELECT 1 FROM deployments d WHERE d.capability_id=c.id)
 AND NOT EXISTS(SELECT 1 FROM playbook_installs p WHERE p.id=c.installed_from OR EXISTS(SELECT 1 FROM json_tree(p.receipt) j WHERE j.type='text' AND j.value=c.id))
 ORDER BY c.deleted_at,c.id LIMIT ?)`, limit)
	if err != nil {
		return 0, fmt.Errorf("purge capability references: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}
