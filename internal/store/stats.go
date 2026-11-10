package store

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// DayCount 是某一天的数量（热力图用）。
type DayCount struct {
	Day   string `json:"day"`   // YYYY-MM-DD（本地时区日期，由服务端时区决定）
	Count int    `json:"count"` // 当天数量
}

// Stats 是用户工作台的真实统计（形状沿用旧契约，语义按 v4 口径）。
type Stats struct {
	Total int `json:"total"` // 事件总数
	Today int `json:"today"` // 今日事件数
	// Delivered / DeliveryTotal 是任务口径：成功数 / 已结束（终态）数。
	Delivered     int `json:"delivered"`
	DeliveryTotal int `json:"deliveryTotal"`
	// DeviceCount 是已启用推送设备数（设备仍按用户维度）。
	DeviceCount int        `json:"deviceCount"`
	Daily       []DayCount `json:"daily"`
}

// PhaseCount 是「相位 → 任务数」一行（按 PhaseCounts 的固定顺序出现）。
type PhaseCount struct {
	Phase string `json:"phase"`
	Count int    `json:"count"`
}

// RefCount 是「能力名或节点名 → 任务数」一行（按 Count 降序、Ref 升序）。
type RefCount struct {
	Ref   string `json:"ref"`
	Count int    `json:"count"`
}

// TaskStats 是按 phase / 能力 / 节点切分的任务计数。
//
// 为什么不复用 v3 的 9 态名：v4 只有六个相位（capability.md §8.3），
// 报出 assigned/acked/lease_expired 只会让前端去渲染不存在的状态。
type TaskStats struct {
	// ByPhase 恒含全部六个相位（计数为 0 也出现）：前端不必自己补零。
	ByPhase []PhaseCount `json:"byPhase"`
	// ByCapability / ByNode 只含有任务行的那些。
	ByCapability []RefCount `json:"byCapability"`
	ByNode       []RefCount `json:"byNode"`
}

// TaskStats 计算某命名空间的任务计数（按相位、能力名、节点名）。
func (d *DB) TaskStats(ctx context.Context, namespaceID string) (*TaskStats, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	out := &TaskStats{
		ByPhase:      phaseCountsZeroed(),
		ByCapability: []RefCount{},
		ByNode:       []RefCount{},
	}

	rows, err := d.QueryContext(ctx,
		`SELECT phase, COUNT(*) FROM tasks WHERE namespace_id = ? GROUP BY phase`, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("task stats by phase: %w", err)
	}
	byPhase := map[string]int{}
	for rows.Next() {
		var phase string
		var n int
		if err := rows.Scan(&phase, &n); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan task phase count: %w", err)
		}
		byPhase[phase] = n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate task phase counts: %w", err)
	}
	rows.Close()
	for i := range out.ByPhase {
		out.ByPhase[i].Count = byPhase[out.ByPhase[i].Phase]
	}

	if out.ByCapability, err = d.taskRefCounts(ctx,
		`SELECT capability, COUNT(*) FROM tasks WHERE namespace_id = ? GROUP BY capability`,
		namespaceID); err != nil {
		return nil, err
	}
	if out.ByNode, err = d.taskRefCounts(ctx,
		`SELECT node_name, COUNT(*) FROM tasks
		  WHERE namespace_id = ? AND node_name <> '' GROUP BY node_name`,
		namespaceID); err != nil {
		return nil, err
	}
	return out, nil
}

// phaseCountsZeroed 返回六个相位的零值行（固定顺序：排队 → 运行 → 终态四类）。
func phaseCountsZeroed() []PhaseCount {
	phases := TaskPhases()
	out := make([]PhaseCount, 0, len(phases))
	for _, p := range phases {
		out = append(out, PhaseCount{Phase: p})
	}
	return out
}

// taskRefCounts 执行一条 group-by 并排序成 RefCount 列表。
//
// 排序在 Go 侧做：SQLite 的 ORDER BY 在多列上有 NULL/大小写细节，
// 而这里要的是稳定的「多者在前、同数按名字」——一次排序不值得依赖 SQL 方言。
func (d *DB) taskRefCounts(ctx context.Context, query string, args ...any) ([]RefCount, error) {
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("task ref counts: %w", err)
	}
	defer rows.Close()
	out := []RefCount{}
	for rows.Next() {
		var rc RefCount
		if err := rows.Scan(&rc.Ref, &rc.Count); err != nil {
			return nil, fmt.Errorf("scan task ref count: %w", err)
		}
		out = append(out, rc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task ref counts: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Ref < out[j].Ref
	})
	return out, nil
}

// MessageStats 计算某命名空间的工作台统计（sinceSec 为热力图起点，unixepoch 秒）。
//
// 口径：#80 之后「发生了什么」是 events、「做完没有」是 tasks：
//
//	Total/Today/Daily = 事件数（今日与日桶用**服务端本地时区**的日界）
//	Delivered/DeliveryTotal = 终态任务里 succeeded 的占比
//	DeviceCount = 该用户启用的推送设备数（设备仍按 user 维度）
//
// 字段名与 JSON key 沿用旧契约：改名会让前端变成破坏性变更，而收益只是语义更贴切。
func (d *DB) MessageStats(ctx context.Context, namespaceID, userID string, sinceSec int64) (*Stats, error) {
	return d.messageStats(ctx, namespaceID, userID, sinceSec, time.Now())
}

// startOfLocalDayUnix 返回 now 所在时区当天 00:00:00 的 Unix 秒。
//
// 日界取**服务端进程本地时区**而不是 UTC：对 UTC+8 的用户来说 UTC 0 点起算意味着
// 「今天的统计从早上 8 点开始」。用 time.Date 重组年月日而不是减小时数：
// 后者在半小时时区（UTC+5:30 等）上直接算错。
func startOfLocalDayUnix(now time.Time) int64 {
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
}

// messageStats 是 MessageStats 的可注入时间版本（now 由调用方给，测试才能不依赖
// 机器时区/真实时钟地验证「今日」与日桶口径）。
func (d *DB) messageStats(ctx context.Context, namespaceID, userID string, sinceSec int64, now time.Time) (*Stats, error) {
	s := &Stats{Daily: []DayCount{}}
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}

	if err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE namespace_id=?`, namespaceID).Scan(&s.Total); err != nil {
		return nil, fmt.Errorf("stats total: %w", err)
	}
	if err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE namespace_id=? AND created_at >= ?`,
		namespaceID, startOfLocalDayUnix(now)).Scan(&s.Today); err != nil {
		return nil, fmt.Errorf("stats today: %w", err)
	}
	if err := d.QueryRowContext(ctx,
		`SELECT
		   COALESCE(SUM(CASE WHEN phase = ? THEN 1 ELSE 0 END),0),
		   COUNT(*)
		 FROM tasks
		 WHERE namespace_id=? AND phase IN (?,?,?,?)`,
		TaskSucceeded, namespaceID, TaskSucceeded, TaskFailed, TaskCancelled, TaskAbandoned).
		Scan(&s.Delivered, &s.DeliveryTotal); err != nil {
		return nil, fmt.Errorf("stats tasks: %w", err)
	}
	if err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM devices WHERE user_id=? AND enabled=1`, userID).Scan(&s.DeviceCount); err != nil {
		return nil, fmt.Errorf("stats devices: %w", err)
	}

	// 近 N 天每天数量：日界与 Today 用同一个本地时区。
	//
	// SQLite 的 'localtime' 修饰符读进程 TZ，结果不确定（同一次请求里 Today 与 Daily
	// 可能落在不同时区），所以把 now 的时区偏移加到 created_at 上再按 UTC 取日期
	// ——等价于「按本地墙钟分桶」，且完全由 now 决定、可复现。
	// DST 取舍：offset 只从 now 取一次，切换日附近可能有 ±1h 的分桶偏差；宁可接受
	// 这个极小边界偏差，也不要依赖 SQLite 读进程 TZ 这种不可测的全局状态。
	_, offset := now.Zone()
	rows, err := d.QueryContext(ctx,
		`SELECT strftime('%Y-%m-%d', created_at + ?, 'unixepoch') AS day, COUNT(*)
		 FROM events
		 WHERE namespace_id=? AND created_at >= ?
		 GROUP BY day ORDER BY day`, offset, namespaceID, sinceSec)
	if err != nil {
		return nil, fmt.Errorf("stats daily: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var dc DayCount
		if err := rows.Scan(&dc.Day, &dc.Count); err != nil {
			return nil, fmt.Errorf("stats daily scan: %w", err)
		}
		s.Daily = append(s.Daily, dc)
	}
	return s, rows.Err()
}
