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

// 工作台统计的聚合 SQL —— 单 ns（MessageStats）与跨 ns 汇总（MessageStatsAll）
// 共用**同一份**语句。
//
// 为什么抽成常量而不是各写一份：两条路径的口径必须逐字一致（今日日界、终态集合、
// 日桶表达式）。出现第二份副本的症状是「全部命名空间」的卡片和单 ns 卡片对同一个 ns
// 给出不同数字，而且改动只会改到一半。EXPLAIN 计划断言（stats_all_test.go）也直接对
// 这几个常量跑，所以「查询走哪个索引」同样只有一处真源。
const (
	statsTotalSQL = `SELECT COUNT(*) FROM events WHERE namespace_id=?`
	statsTodaySQL = `SELECT COUNT(*) FROM events WHERE namespace_id=? AND created_at >= ?`
	// 终态集合：succeeded 计入分子，四个终态计入分母（pending/running 不计）。
	statsTasksSQL = `SELECT
		   COALESCE(SUM(CASE WHEN phase = ? THEN 1 ELSE 0 END),0),
		   COUNT(*)
		 FROM tasks
		 WHERE namespace_id=? AND phase IN (?,?,?,?)`
	statsDevicesSQL = `SELECT COUNT(*) FROM devices WHERE user_id=? AND enabled=1`
	// 近 N 天每天数量：日界与 Today 用同一个本地时区。
	//
	// SQLite 的 'localtime' 修饰符读进程 TZ，结果不确定（同一次请求里 Today 与 Daily
	// 可能落在不同时区），所以把 now 的时区偏移加到 created_at 上再按 UTC 取日期
	// ——等价于「按本地墙钟分桶」，且完全由 now 决定、可复现。
	// DST 取舍：offset 只从 now 取一次，切换日附近可能有 ±1h 的分桶偏差；宁可接受
	// 这个极小边界偏差，也不要依赖 SQLite 读进程 TZ 这种不可测的全局状态。
	statsDailySQL = `SELECT strftime('%Y-%m-%d', created_at + ?, 'unixepoch') AS day, COUNT(*)
		 FROM events
		 WHERE namespace_id=? AND created_at >= ?
		 GROUP BY day ORDER BY day`
)

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

// MessageStatsAll 把**显式给定**的一组命名空间的统计求和（「全部命名空间」视图）。
//
// 为什么签名是 nsID 列表而不是 `AllNamespaces`：`*` 是 wire 上的写法，不是 store 的
// 作用域。调用方（server）必须先把 `*` 展开成「逐 ns 授权过的真实 id 列表」，store 只
// 接受确定的命名空间；收到空串或 `*` 一律 ErrNamespaceRequired（防御，不是可选项）。
//
// 口径与单 ns **完全一致**（共用 accumulateMessageStats 里那一份 SQL）：
//   - Total/Today/Delivered/DeliveryTotal 逐 ns 相加；
//   - Daily 按 day 合并（不同 ns 的同一天合成一行，不重复计同一天），升序；
//   - DeviceCount 是用户维度，只查一次、**不**乘 ns 数量；
//   - 空列表 = 全零 + `daily: []`（**不报错**：没有命名空间是合法的空集合，不是调用错误）。
func (d *DB) MessageStatsAll(ctx context.Context, namespaceIDs []string, userID string, sinceSec int64) (*Stats, error) {
	return d.messageStatsAll(ctx, namespaceIDs, userID, sinceSec, time.Now())
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
	if err := d.accumulateMessageStats(ctx, s, namespaceID, userID, sinceSec, now, true); err != nil {
		return nil, err
	}
	return s, nil
}

// messageStatsAll 是 MessageStatsAll 的可注入时间版本（理由同 messageStats）。
func (d *DB) messageStatsAll(ctx context.Context, namespaceIDs []string, userID string, sinceSec int64, now time.Time) (*Stats, error) {
	s := &Stats{Daily: []DayCount{}}
	for _, namespaceID := range namespaceIDs {
		if err := d.accumulateMessageStats(ctx, s, namespaceID, userID, sinceSec, now, false); err != nil {
			return nil, err
		}
	}
	// 设备是**用户维度**的：只查一次，不参与逐 ns 求和（否则会被乘上 ns 数量）。
	if err := d.QueryRowContext(ctx, statsDevicesSQL, userID).Scan(&s.DeviceCount); err != nil {
		return nil, fmt.Errorf("stats devices: %w", err)
	}
	return s, nil
}

// accumulateMessageStats 把**一个**命名空间的聚合叠加进 s。
//
// 这是两条读路径唯一的事实来源：单 ns（MessageStats）只调一次且带设备计数，跨 ns
// 汇总（MessageStatsAll）对每个 ns 调一次、设备计数由调用方单独查一次。任何口径
// 改动都只发生在这里，不存在「第二份 SQL 漂了」的可能。
func (d *DB) accumulateMessageStats(ctx context.Context, s *Stats, namespaceID, userID string,
	sinceSec int64, now time.Time, withDevices bool) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}

	var total, today, delivered, deliveryTotal int
	if err := d.QueryRowContext(ctx, statsTotalSQL, namespaceID).Scan(&total); err != nil {
		return fmt.Errorf("stats total: %w", err)
	}
	if err := d.QueryRowContext(ctx, statsTodaySQL, namespaceID, startOfLocalDayUnix(now)).Scan(&today); err != nil {
		return fmt.Errorf("stats today: %w", err)
	}
	if err := d.QueryRowContext(ctx, statsTasksSQL,
		TaskSucceeded, namespaceID, TaskSucceeded, TaskFailed, TaskCancelled, TaskAbandoned).
		Scan(&delivered, &deliveryTotal); err != nil {
		return fmt.Errorf("stats tasks: %w", err)
	}
	s.Total += total
	s.Today += today
	s.Delivered += delivered
	s.DeliveryTotal += deliveryTotal

	if withDevices {
		if err := d.QueryRowContext(ctx, statsDevicesSQL, userID).Scan(&s.DeviceCount); err != nil {
			return fmt.Errorf("stats devices: %w", err)
		}
	}

	_, offset := now.Zone()
	rows, err := d.QueryContext(ctx, statsDailySQL, offset, namespaceID, sinceSec)
	if err != nil {
		return fmt.Errorf("stats daily: %w", err)
	}
	defer rows.Close()
	add := []DayCount{}
	for rows.Next() {
		var dc DayCount
		if err := rows.Scan(&dc.Day, &dc.Count); err != nil {
			return fmt.Errorf("stats daily scan: %w", err)
		}
		add = append(add, dc)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	mergeDaily(&s.Daily, add)
	return nil
}

// mergeDaily 把 add 里的日桶按 day 叠加进 dst，并保持 day 升序。
//
// 单 ns 只有一次叠加（结果与 SQL 的 `ORDER BY day` 逐字节一致）；跨 ns 汇总时同一
// day 的多行必须**合并成一行**——热力图的口径是「每天一个格子」，不是「每 ns 每天
// 一个格子」，否则 371 天的窗口会被 ns 数量拉长成 371×N 个点。
//
// 日期键是 `YYYY-MM-DD`，字典序 == 时间序，所以升序就是字符串升序。
func mergeDaily(dst *[]DayCount, add []DayCount) {
	if len(add) == 0 {
		return
	}
	if *dst == nil {
		*dst = []DayCount{}
	}
	idx := make(map[string]int, len(*dst)+len(add))
	for i := range *dst {
		idx[(*dst)[i].Day] = i
	}
	for _, dc := range add {
		if i, ok := idx[dc.Day]; ok {
			(*dst)[i].Count += dc.Count
			continue
		}
		idx[dc.Day] = len(*dst)
		*dst = append(*dst, dc)
	}
	sort.Slice(*dst, func(i, j int) bool { return (*dst)[i].Day < (*dst)[j].Day })
}
