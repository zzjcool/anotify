package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// 本文件是 task_logs（迁移 4）的读写层：任务的 stdout / stderr 原文
// （CONTRACT-task-logs §2/§3/§4）。
//
// 为什么日志要走 HTTP 上传 + 落库，而不是塞进 result 帧：
// 帧的软上限是 1 MiB，而一次构建日志轻松到几十 MB；把日志塞进帧里，同一条连接上
// 的心跳与 assign 就会被堵在后面——症状是「节点掉线」，根因却是「任务话多」。
// 上传走普通 HTTP、落库在 tasks 之外的一张表，热路径（ListTasks / stats / 巡检）
// 一行都不碰它。
//
// 它与 emissions 的区别：emissions 是**数据**（参与路由、是幂等依据，缺一条会把
// 后续 seq 全判成缺口）；task_logs 是**排障材料**（丢了可惜，但绝不能因为丢日志
// 把跑成功的任务判成失败）。所以这里的写入口没有事务协调、没有 outbox 重投，
// 只有一条「幂等覆盖」。

// TaskLogs 是一个任务的一份日志快照（agent 侧采集后的原文）。
//
// Stdout / Stderr 已经是 **UTF-8 安全 + 尾部截断**后的文本（截断在节点侧完成，
// 见 cmd/agent/runner.go）：中枢只管存，不再二次裁剪——两处各裁一次的话，
// 截断标记里的「共 N 字节」到底指哪一层的 N 就说不清了。
type TaskLogs struct {
	// TaskID 是任务 id（= task_logs 主键）。
	TaskID string `json:"taskId"`
	// NamespaceID 是任务所属命名空间（读路径的隔离维度，也是写路径的归属校验）。
	NamespaceID string `json:"namespaceId"`
	// Stdout / Stderr 是子进程的两个流，**分别**保存（合并之后 stderr 的
	// 「谁先写谁先上」顺序就没了，而报错时恰恰要看清是哪个流）。
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	// Truncated 表示节点侧截断过（每流只留了最后 32 KiB）。
	//
	// 它是**采集侧的事实**，不能由 LENGTH(stdout) 反推：32 KiB 恰好整除时
	// 反推不出来，而「看得不是全部」这句话必须说得准。
	Truncated bool `json:"truncated"`
	// Bytes 是截断**前**两个流的真实字节数之和（前端据此显示「原始多大」）。
	Bytes int64 `json:"bytes"`
	// CreatedAt 是最近一次上传的时刻（unix 秒）。幂等覆盖时会一起重写：
	// 同一条日志被补传时，「这份内容是什么时候到的」比「第一次是什么时候到的」
	// 更有用（后者对排障没有意义）。
	CreatedAt int64 `json:"createdAt"`
}

// taskLogsColumns 是统一的列顺序，scanTaskLogs 依赖它。
const taskLogsColumns = `task_id, namespace_id, stdout, stderr, truncated, bytes, created_at`

// UpsertTaskLogs 写入/覆盖一个任务的日志（幂等）。
//
// 「幂等」是硬要求（契约 §2）：重派、agent 重启补传、客户端重试都会重复上传同一条
// 任务；重复上传必须是**覆盖**而不是报错，也不是插入第二行。
//
// **最后写入者赢**：同一任务被重派时两次 attempt 都可能有日志（旧那次可能晚到），
// 后落库的那份覆盖先落库的。这里刻意不做 attempt 级别的先后判断：契约要的是
// 幂等覆盖，而「哪一次的日志更真」在库层无法判定（attempts 在 tasks 上，不在每次
// 上传里），猜一个只会造成「同一行时而旧时而新」。排障时以 created_at 为准。
//
// 命名空间归属与写入是**同一条语句**：`INSERT ... SELECT ... FROM tasks WHERE
// namespace_id = ? AND id = ?`。这样「任务不在这个命名空间」在 SQL 层就直接落成
// 「一行也没写」（RowsAffected == 0 → ErrNotFound），不存在「先查后写」之间任务被
// 删掉的窗口，也不可能给**别的命名空间**的任务写日志。
//
// 注意边界：「任务是不是**这台节点**跑的」是 handler 的 GetTask 判的，不是这条
// SQL 判的——两次查询之间任务被改派给另一台节点时，旧节点仍可能写入。那个窗口
// 小到只能靠日志里的 foreign_node WARN 观测，不影响数据隔离（命名空间仍由
// 令牌钉死），所以不为此再加一个事务。
//
// 启动时 PRAGMA foreign_keys=ON（见 store.go 的 DSN），所以 task_logs 行不可能
// 悬空在已删除的任务上。
func (d *DB) UpsertTaskLogs(ctx context.Context, l *TaskLogs) error {
	if l == nil {
		return fmt.Errorf("upsert task logs: nil")
	}
	if l.TaskID == "" {
		return fmt.Errorf("upsert task logs: empty task_id")
	}
	if l.NamespaceID == "" || l.NamespaceID == AllNamespaces {
		// 与 GetTask 同一条纪律：写操作必须落在**一个**命名空间里，
		// `*` 在写路径上没有定义（它不是一个真的命名空间）。
		return ErrNamespaceRequired
	}
	createdAt := l.CreatedAt
	if createdAt == 0 {
		createdAt = Now()
	}
	res, err := d.ExecContext(ctx,
		`INSERT INTO task_logs (task_id, namespace_id, stdout, stderr, truncated, bytes, created_at)
		 SELECT id, namespace_id, ?, ?, ?, ?, ? FROM tasks WHERE namespace_id = ? AND id = ?
		 ON CONFLICT(task_id) DO UPDATE SET
		     namespace_id = excluded.namespace_id,
		     stdout       = excluded.stdout,
		     stderr       = excluded.stderr,
		     truncated    = excluded.truncated,
		     bytes        = excluded.bytes,
		     created_at   = excluded.created_at`,
		l.Stdout, l.Stderr, boolToInt(l.Truncated), l.Bytes, createdAt, l.NamespaceID, l.TaskID)
	if err != nil {
		return fmt.Errorf("upsert task logs for %s: %w", l.TaskID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("upsert task logs rows affected: %w", err)
	}
	if n == 0 {
		// SELECT 没有命中 = 任务不存在 / 不属于这个命名空间。调用方按 404 处理
		// （契约 §2：两者对节点是同一件事，细节进服务端日志）。
		return ErrNotFound
	}
	l.CreatedAt = createdAt
	return nil
}

// GetTaskLogs 读一个任务的日志；任务存在但这张表里没有行时返回 ErrNotFound。
//
// **注意**：这个 ErrNotFound 是「没有日志」，不是「没有任务」。读端点
// （GET /v1/tasks/{id}/logs）必须先用 GetTask 判任务，再把这里的 ErrNotFound
// 折成 200 + 空对象（契约 §4）——「没日志」是合法状态，做成 404 会让前端在
// 正常路径上处理错误。
func (d *DB) GetTaskLogs(ctx context.Context, namespaceID, taskID string) (*TaskLogs, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if taskID == "" {
		return nil, fmt.Errorf("get task logs: empty task_id")
	}
	row := d.QueryRowContext(ctx,
		`SELECT `+taskLogsColumns+` FROM task_logs WHERE namespace_id = ? AND task_id = ?`,
		namespaceID, taskID)
	l, err := scanTaskLogsRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get task logs %s: %w", taskID, err)
	}
	return l, nil
}

// DeleteTaskLogs 删掉一个任务的日志（幂等：本来没有也算成功）。
//
// 只给测试与运维工具用：正常路径上日志**不单独删**，它跟随任务级联删除
// （ON DELETE CASCADE）——单独删会造出「任务在、日志没了」这种与「老任务」同形的
// 状态，除非确实要清掉一份敏感输出。
func (d *DB) DeleteTaskLogs(ctx context.Context, namespaceID, taskID string) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	if taskID == "" {
		return fmt.Errorf("delete task logs: empty task_id")
	}
	if _, err := d.ExecContext(ctx,
		`DELETE FROM task_logs WHERE namespace_id = ? AND task_id = ?`,
		namespaceID, taskID); err != nil {
		return fmt.Errorf("delete task logs %s: %w", taskID, err)
	}
	return nil
}

// scanTaskLogsRow 从一行结果解析 TaskLogs（*sql.Row 与 *sql.Rows 都满足 scanRow）。
func scanTaskLogsRow(row scanRow) (*TaskLogs, error) {
	var l TaskLogs
	var truncated int
	if err := row.Scan(&l.TaskID, &l.NamespaceID, &l.Stdout, &l.Stderr,
		&truncated, &l.Bytes, &l.CreatedAt); err != nil {
		return nil, err
	}
	l.Truncated = truncated != 0
	return &l, nil
}
