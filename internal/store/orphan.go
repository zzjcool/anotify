package store

import (
	"context"
	"errors"
	"fmt"
)

// 孤儿 pending 任务（#270）。
//
// 背景：`pending` 状态从来没有 abandon 路径。以下情形会让一条任务**永远**停在 pending：
//
//   - 能力升级后，集群里再没有节点声明那个版本
//   - 唯一能跑它的节点下线了，且不再回来
//   - 任务建出来后，那个能力被禁用/删除
//
// 这不只是"垃圾数据"：背压（#269）的判定信号是 `MIN(created_at) WHERE status='pending'`，
// **一条孤儿任务就能让这个值只增不减**，把整个部署的新事件入口钉死在 503 上。
// #269 给背压加了 30 分钟 fail-open 安全阀（正确的取舍），但那只是泄压阀，病根还在这里。
//
// 本文件的定位是**先能看见、再能处置**：
//
//	ListOrphanPendingTasks  只读：列出"pending 超过 N 秒且当前没有任何在线节点能跑"的任务
//	AbandonPendingTask      显式转移 pending → abandoned（带原因），只动仍是 pending 的行
//
// 刻意**不做自动 abandon**：孤儿任务是有价值的事实记录（"这条事件本该被处理，却没被处理"），
// 自动删/自动放弃会把事故证据一起清掉。要不要自动化、阈值多少，是运营策略，不是数据层的事。

// 孤儿原因分类。分类比"一条 boolean"有用得多：同样是派不出去，
// 「没人声明这个能力」要改能力/节点，「节点全下线」要救节点，处置完全不同。
const (
	// OrphanNoNodeDeclares：这个命名空间里**没有任何节点**声明这个能力版本。
	OrphanNoNodeDeclares = "no_node_declares"
	// OrphanNoOnlineNode：有节点声明，但一台都不在线（离线/排空中）。
	OrphanNoOnlineNode = "no_online_node"
)

// OrphanPendingTask 是一条派不出去的 pending 任务 + 它的空闲时长与原因。
type OrphanPendingTask struct {
	Task
	// IdleSec 是「创建至今」的秒数。用创建时间而不是 attempts，
	// 因为孤儿任务通常 attempts=0（连第一次派发都没发生过）。
	IdleSec int64 `json:"idleSec"`
	// Reason 见 OrphanNoNodeDeclares / OrphanNoOnlineNode。
	Reason string `json:"reason"`
}

// ListOrphanPendingTasks 列出命名空间内「pending 且超过 olderThanSec 秒，
// 且当前没有任何在线节点能执行」的任务。
//
// olderThanSec 是**安全阀**：刚建出来的任务在派发瞬间也可能短暂"没有在线节点"
// （节点正在上线、能力正在注册），把它当成孤儿会误报。调用方必须给一个下限，
// 0 或负数按 1 小时兜底——这里不允许"立刻把所有 pending 都列成孤儿"。
//
// limit <= 0 时用 200，上限 1000：这是一个给人看的诊断视图，不是数据导出接口。
func (d *DB) ListOrphanPendingTasks(ctx context.Context, namespaceID string, olderThanSec, limit int) ([]OrphanPendingTask, error) {
	if namespaceID == "" {
		return nil, fmt.Errorf("store: namespaceID required")
	}
	if olderThanSec <= 0 {
		olderThanSec = 3600
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	cutoff := Now() - int64(olderThanSec)

	// 两段查询：先按判据捞出候选 id + 原因分类，再逐条 GetTask 取完整行。
	//
	// 为什么不做成一条 JOIN：`scanTask` 是全仓唯一的任务行解析点（25 列 + 三处 JSON
	// 解码），JOIN 出多余列会让它扫不动，而复制一份解码逻辑迟早两边漂移。
	// 这里最多 200 行，且是给人看的诊断视图，不值得为省几次查询去复制解析代码。
	//
	// 判据用 capability_id（具体版本）而不是能力名：能力升级到 @2 之后，
	// 只有 @1 的节点对 @1 的任务无效；按名字判会把这种情况算成"有人能跑"。
	const q = `
SELECT t.id,
       (SELECT COUNT(*) FROM node_capabilities nc
          JOIN nodes n ON n.id = nc.node_id
         WHERE nc.capability_id = t.capability_id
           AND n.namespace_id = t.namespace_id) AS declared_nodes,
       (SELECT COUNT(*) FROM node_capabilities nc
          JOIN nodes n ON n.id = nc.node_id
         WHERE nc.capability_id = t.capability_id
           AND n.namespace_id = t.namespace_id
           AND n.status = ?) AS online_nodes
  FROM tasks t
 WHERE t.namespace_id = ?
   AND t.status = ?
   AND t.created_at <= ?
   AND NOT EXISTS (
         SELECT 1 FROM node_capabilities nc
           JOIN nodes n ON n.id = nc.node_id
          WHERE nc.capability_id = t.capability_id
            AND n.namespace_id = t.namespace_id
            AND n.status = ?)
 ORDER BY t.created_at ASC
 LIMIT ?`

	rows, err := d.QueryContext(ctx, q,
		NodeStatusOnline, namespaceID, TaskPending, cutoff, NodeStatusOnline, limit)
	if err != nil {
		return nil, fmt.Errorf("list orphan pending tasks: %w", err)
	}
	type candidate struct {
		id       string
		declared int
	}
	cands := make([]candidate, 0, 16)
	for rows.Next() {
		var (
			c         candidate
			onlineCnt int
		)
		if err := rows.Scan(&c.id, &c.declared, &onlineCnt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan orphan candidate: %w", err)
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate orphan candidates: %w", err)
	}
	rows.Close()

	now := Now()
	out := make([]OrphanPendingTask, 0, len(cands))
	for _, c := range cands {
		t, gerr := d.GetTask(ctx, namespaceID, c.id)
		if gerr != nil {
			if errors.Is(gerr, ErrNotFound) {
				continue // 两次查询之间被清理/派发走了
			}
			return nil, fmt.Errorf("load orphan task %s: %w", c.id, gerr)
		}
		reason := OrphanNoOnlineNode
		if c.declared == 0 {
			reason = OrphanNoNodeDeclares
		}
		idle := now - t.CreatedAt
		if idle < 0 {
			idle = 0
		}
		out = append(out, OrphanPendingTask{Task: *t, IdleSec: idle, Reason: reason})
	}
	return out, nil
}

// AbandonPendingTask 把一条仍处于 pending 的任务显式转成 abandoned，并写入原因。
//
// 为什么必须走 CAS（transitionGuarded 的 `AND status='pending'`）而不是"先查再改"：
// 查与改之间任务可能刚被派发出去（节点上线了），此时把它标成 abandoned 会把一条
// **正在执行**的任务变成终态，节点随后回报的 result 会被状态机拒绝，事故现场就毁了。
//
// 返回 ErrNotFound / ErrInvalidTransition 由调用方翻译成 404 / 409：
// 「它已经不在 pending 了」是使用者需要知道的事实，不能假装成功。
func (d *DB) AbandonPendingTask(ctx context.Context, namespaceID, taskID, reason string) error {
	if namespaceID == "" || taskID == "" {
		return fmt.Errorf("store: namespaceID and taskID required")
	}
	if reason == "" {
		reason = "abandoned by operator"
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("abandon pending task: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 状态写只走 transitionGuarded（TestStatusWriterIsUnique 会静态检查本包不许
	// 出现 `UPDATE tasks SET status` 字面量），原因列在同一事务里补上。
	if err := d.transitionGuarded(ctx, tx, namespaceID, taskID, TaskPending, TaskAbandoned, ""); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET error = ? WHERE namespace_id = ? AND id = ? AND status = ?`,
		reason, namespaceID, taskID, TaskAbandoned); err != nil {
		return fmt.Errorf("abandon pending task: set reason: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("abandon pending task: commit: %w", err)
	}
	return nil
}
