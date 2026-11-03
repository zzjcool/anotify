package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Emission 是一次 emit 的落库行（迁移 22，docs/design/emit-protocol.md §4 / §7）。
//
// 它是**唯一**的幂等依据：节点的 outbox 会在重连后按 seq 重发，中枢靠
// PRIMARY KEY(task_id, seq) 把它折成一次派发。所以这张表不是日志——
// 少了它，「agent 在 emit 与 ack 之间崩溃」就会变成「下游被派发两遍」。
//
// Routed / Pruned 存的是**首次**处理的结果：重复 seq 一律回同一份计数
// （emit_ack 的幂等语义要求如此，见 emit-protocol §3.2）。
type Emission struct {
	TaskID string `json:"taskId"`
	// Seq 是任务内单调递增的序号（从 1 开始）。0 / 负数是非法值，插入直接拒。
	Seq int64 `json:"seq"`
	// Port 是上游回报的出口名，路由的唯一输入（不变量 I2/I4）。
	Port string `json:"port"`
	// Data 是原样保存的 JSON 对象（审计要的是当时那份字节，不重排键）。
	Data json.RawMessage `json:"data"`
	// Routed 是首次处理派发出去的下游条数（= 建出的子任务行数）。
	Routed int `json:"routed"`
	// Pruned 是首次处理剪掉的 link 条数（no_node / foreign_user / path / param_*）。
	Pruned int `json:"pruned"`
	// CreatedAt 是首次落库时刻（unixepoch 秒）；重复 seq 不会更新时间。
	CreatedAt int64 `json:"createdAt"`
}

// emissionColumns 是统一的列顺序，scanEmission 依赖它。
const emissionColumns = `task_id, seq, port, data, routed, pruned, created_at`

// InsertEmission 落一条 emission（幂等：重复 (task_id, seq) 什么都不写）。
//
// 返回值 first=false 表示这一条已经存在——调用方**不得**再派发下游，
// 应回到库里读那份首次结果（见 GetEmission）。
//
// 单独一条插入不建子任务行；真正派发走 InsertEmissionAndCreateTasks
// （两者必须同一个事务，否则进程在中间退出就静默丢一条 emit）。
func (d *DB) InsertEmission(ctx context.Context, e *Emission) (bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin insert emission: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	first, err := d.insertEmissionTx(ctx, tx, e)
	if err != nil {
		return false, err
	}
	if !first {
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit insert emission: %w", err)
	}
	return true, nil
}

// InsertEmissionAndCreateTasks 在**一个事务**里完成「幂等写 emission + 建下一跳子任务行」。
//
// 为什么必须同一个事务（emit-protocol §7，与 #140 的接力原子性是同一条纪律）：
// 拆成两次提交时，进程在「emission 已提交、子任务行还没建」之间退出，就会留下
// 「这次 emit 中枢收到了、下游却永远不存在」的永久空洞——而节点已经拿到 ack，
// 不会重发（重发本来也只会命中幂等）。反过来（先建子任务、后写 emission）
// 则会在重发时**重复派发**，那是同一件事的另一种坏法。
//
// ts 为空是合法的（该 port 没接下游 / 全被剪枝）：emission 照样落库。
// 这一步不能省——seq 的连续性靠它，漏一条会把后续 seq 全判成缺口。
//
// 返回 first=false 表示 (task_id, seq) 已存在：**什么都不写**（事务回滚），
// 调用方读到首次结果后原样回给节点，绝不重复派发。
func (d *DB) InsertEmissionAndCreateTasks(ctx context.Context, e *Emission, ts []*Task) (bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin emit transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	first, err := d.insertEmissionTx(ctx, tx, e)
	if err != nil {
		return false, err
	}
	if !first {
		return false, nil // 幂等命中：不建行、不提交（defer 会回滚掉这次的空事务）
	}
	// 子任务行与 emission 同生共死：任何一条不合格都整体回滚，
	// 于是「emission 在、下游不在」这个形状在库里**不可能**出现。
	if err := d.createTasksTx(ctx, tx, ts); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit emit transaction: %w", err)
	}
	return true, nil
}

// insertEmissionTx 是 InsertEmission / InsertEmissionAndCreateTasks 共用的事务内插入。
//
// ON CONFLICT(task_id, seq) DO NOTHING 是幂等的实现：不靠「先查再插」
// （那样两个并发 emit 会都查到「不存在」然后都去派发），而是把判定交给唯一键。
func (d *DB) insertEmissionTx(ctx context.Context, tx *sql.Tx, e *Emission) (bool, error) {
	if e == nil {
		return false, fmt.Errorf("insert emission: nil")
	}
	if e.TaskID == "" {
		return false, fmt.Errorf("insert emission: empty task_id")
	}
	if e.Seq <= 0 {
		return false, fmt.Errorf("insert emission: seq must be >= 1 (got %d)", e.Seq)
	}
	data := e.Data
	if len(data) == 0 {
		data = json.RawMessage("{}")
	}
	createdAt := e.CreatedAt
	if createdAt == 0 {
		createdAt = Now()
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO emissions (task_id, seq, port, data, routed, pruned, created_at)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(task_id, seq) DO NOTHING`,
		e.TaskID, e.Seq, e.Port, string(data), e.Routed, e.Pruned, createdAt)
	if err != nil {
		return false, fmt.Errorf("insert emission (%s#%d): %w", e.TaskID, e.Seq, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("insert emission rows affected: %w", err)
	}
	e.CreatedAt = createdAt
	return n > 0, nil
}

// GetEmission 按 (task_id, seq) 取一条 emission；未命中返回 ErrNotFound。
//
// 它是幂等路径的读口：路由层先用它判「这条 seq 是不是重发」，
// 命中就直接把首次的 routed/pruned 回给节点（不再派发一次）。
func (d *DB) GetEmission(ctx context.Context, taskID string, seq int64) (*Emission, error) {
	if taskID == "" {
		return nil, fmt.Errorf("get emission: empty task_id")
	}
	row := d.QueryRowContext(ctx,
		`SELECT `+emissionColumns+` FROM emissions WHERE task_id = ? AND seq = ?`, taskID, seq)
	e, err := scanEmissionRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get emission %s#%d: %w", taskID, seq, err)
	}
	return e, nil
}

// ListEmissions 按 seq 升序列出某任务的全部 emission（给 trace / 前端事件流用）。
//
// 空列表返回 []（不是 nil）：调用方常常直接把它塞进 JSON 响应，
// 返回 null 会让前端把「还没 emit」和「查不到」混成一件事。
func (d *DB) ListEmissions(ctx context.Context, taskID string) ([]*Emission, error) {
	if taskID == "" {
		return nil, fmt.Errorf("list emissions: empty task_id")
	}
	rows, err := d.QueryContext(ctx,
		`SELECT `+emissionColumns+` FROM emissions WHERE task_id = ? ORDER BY seq`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list emissions %s: %w", taskID, err)
	}
	defer rows.Close()
	out := []*Emission{}
	for rows.Next() {
		e, err := scanEmissionRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan emission of %s: %w", taskID, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EmissionSeqStatus 返回某任务已落库的 emission 条数与该任务的最大 seq。
//
// 缺口判定用（emit-protocol §4）：写入永远从 1 开始且连续（路由层自己把关），
// 所以「条数 == n-1」且「最大 seq == n-1」两个条件同时成立，就等价于
// 「集合恰好是 {1..n-1}」——中间有洞时条数会小于最大 seq，同样被这组条件挡住。
//
// 一次聚合查询而不是把整表读回来：这在一个**可能被高频调用**的路径上
// （能力进程一次运行可以 emit 很多次），而返回的只有两个整数。
func (d *DB) EmissionSeqStatus(ctx context.Context, taskID string) (count int, maxSeq int64, err error) {
	if taskID == "" {
		return 0, 0, fmt.Errorf("emission seq status: empty task_id")
	}
	if err := d.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MAX(seq), 0) FROM emissions WHERE task_id = ?`, taskID).
		Scan(&count, &maxSeq); err != nil {
		return 0, 0, fmt.Errorf("emission seq status %s: %w", taskID, err)
	}
	return count, maxSeq, nil
}

// scanEmissionRow 从一行结果解析 Emission（*sql.Row 与 *sql.Rows 都满足 scanRow）。
func scanEmissionRow(row scanRow) (*Emission, error) {
	var e Emission
	var data string
	if err := row.Scan(&e.TaskID, &e.Seq, &e.Port, &data, &e.Routed, &e.Pruned, &e.CreatedAt); err != nil {
		return nil, err
	}
	if data == "" {
		data = "{}"
	}
	e.Data = json.RawMessage(data)
	return &e, nil
}

// scanRow 是 *sql.Row / *sql.Rows 的最小公共面（只有 Scan）。
type scanRow interface {
	Scan(dest ...any) error
}
