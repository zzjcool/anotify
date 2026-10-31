package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// 保留策略（保留期过期清理，issue #143）。
//
// 为什么需要这层：`events` / `tasks` 在此之前**只进不出**——没有任何 DELETE。
// 表与索引无限增长，最终把查询和写入一起拖垮；多租户下任意一个命名空间的洪峰
// 都会永久留在库里，拖慢所有命名空间。这才是「量大了会出问题」的真实原因。
//
// 两条清理规则（docs/design/nats-transport.md §7.4 / §16.6）：
//
//	tasks   按**能力级** TTL：capability_versions.lifecycle.resultTtlSec
//	        只清理终态（succeeded/failed/canceled/abandoned）且 finished_at 早于
//	        now - resultTtlSec 的行。未声明 resultTtlSec（0）的能力**不清理**。
//	events  按**部署级**保留期：调用方不显式给出保留期时（EventRetentionSec <= 0），
//	        整个 events 阶段跳过（DefaultEventRetentionSec == EventRetentionDisabled）；
//	        部署配置 ANOTIFY_EVENT_RETENTION_DAYS 默认 **30 天**，由 internal/lease 注入。
//	        source='route' 的剪枝行（#142）同样受此约束，不搞特例。
//	        保留期是部署级配置（org 层删除后不再有 per-org 配置，#205），
//	        **删除粒度是命名空间**（events.namespace_id）：一份保留期对所有命名空间生效。
//
// 三条硬约束（都是为了让清理不反过来打垮库）：
//
//  1. **按命名空间 / 能力分批**：单条 DELETE 只删 Batch 行。一次大删除会长时间持锁，
//     把同库（SQLite 单写）甚至别的命名空间的写入全堵住。
//  2. **幂等 + 可重入**：多轮跑、中途被打断、两个进程同时跑，结果都一样；
//     删多删少只取决于「此刻谁过期了」。
//  3. **可度量**：每轮返回 RetentionReport，日志里能看见删了多少、是否被预算截断。

// EventRetentionDisabled 表示「不清理 events」。0 在这里是**禁用**，不是「保留 0 秒」。
const EventRetentionDisabled = 0

// DefaultEventRetentionSec 是 events 保留期的**store 层**默认值：禁用（== EventRetentionDisabled）。
//
// 它只回答一个问题：「调用方没给值时 store 要不要删 events」。而**部署默认**是另一件事：
// ANOTIFY_EVENT_RETENTION_DAYS（server.Config.EventRetentionDays）默认 **30 天**，
// 由 internal/lease 的 Sweeper.RetentionEventSec 显式注入到 RetentionOptions。
// store 这里保持「禁用」是刻意的：底层默认绝不能自己发起不可逆的删除；「默认删 30 天前
// 的事件」这个产品决策由部署层显式表达，才能被启动日志告知、被配置关掉。
//
// 具体含义：`retentionWithDefaults` **不会**把未配置的保留期补成某个正数秒；
// SweepRetention 在 EventRetentionSec <= 0 时跳过整个 events 阶段并置
// RetentionReport.EventsDisabled——「压根没扫」与「扫了但没东西删」必须能区分开。
//
// 两种「0」含义一致：store 层没配置（本默认值）与运维显式配
// `ANOTIFY_EVENT_RETENTION_DAYS=0`，都是**永久保留**（events 会持续增长，启动日志 WARN）。
const DefaultEventRetentionSec = EventRetentionDisabled

// DefaultRetentionBatch 是单条 DELETE 的行数上限（先度量后删除）。
const DefaultRetentionBatch = 1000

// DefaultRetentionBatches 是每个范围（命名空间×能力 / 命名空间）每轮最多跑几批。
//
// 默认 20 → 每个范围每轮最多删 20×1000 = 20,000 行。
//
// 为何从 5 提到 20（#229 / A5）：5×1000 行 × 1 小时一轮 = 12 万行/天，而 30 ev/s 的
// 写入（约 2 行/事件）是 ~518 万行/天——预算差一个数量级，保留期开着也永远删不完。
// 提到 20 只是**单轮预算**的手调，不是「一次大事务」：每批仍是一次独立的、1000 行的
// 短事务（写锁持有量没变），只是单轮多跑几批；「多久一轮」由 lease 层控制
// （常态 1 小时；上一轮被本预算截断时改 30s 追赶，见 internal/lease/retention.go）。
// 被预算截断时 RetentionReport.Truncated 置位——这正是追赶的触发信号。
const DefaultRetentionBatches = 20

// terminalStatusList 是「结果已经落定、可以被保留期清理」的四态。
// retention_test.go 会对照 IsTerminalStatus 校验两者不漂移。
var terminalStatusList = []string{TaskSucceeded, TaskFailed, TaskCanceled, TaskAbandoned}

// ResultRetentionScope 是一个「按 TTL 清理终态任务」的范围：某命名空间下的某能力版本。
//
// 为什么按能力版本而不是按命名空间：TTL 是能力定义的一部分（lifecycle.resultTtlSec），
// 不同能力的结果该留多久本来就不同（日志类很快可丢，评审结论要留）。
// NamespaceID 是删除时的隔离维度（#162），也是唯一的隔离维度（#205 删了组织层）。
type ResultRetentionScope struct {
	NamespaceID  string `json:"namespaceId"`
	CapabilityID string `json:"capabilityId"`
	ResultTTLSec int    `json:"resultTtlSec"`
}

// RetentionOptions 是一轮保留策略巡检的参数。
type RetentionOptions struct {
	// Now 是「现在」（unixepoch 秒）；0 → store.Now()。测试注入用。
	Now int64
	// EventRetentionSec 是 events 的保留秒数；<= 0 = 禁用（未配置）。
	EventRetentionSec int
	// Batch 是单条 DELETE 的行数上限；<=0 → DefaultRetentionBatch。
	Batch int
	// Batches 是每个范围每轮最多跑几批；<=0 → DefaultRetentionBatches。
	Batches int
}

// RetentionReport 汇总一轮清理的结果（日志与测试用；「先度量后删除」）。
type RetentionReport struct {
	// TaskScopes 是参与本轮的（命名空间×能力）范围数。
	TaskScopes int
	// TasksDeleted 是本轮删掉的任务行数。
	TasksDeleted int64
	// Namespaces 是本轮真正扫过的命名空间数（事件按命名空间逐个删；#205 之后不再有组织）。
	Namespaces int
	// EventsDeleted 是本轮删掉的事件行数。
	EventsDeleted int64
	// EventsDisabled 表示本轮 events 清理被**禁用**（保留期 <= 0，未配置）。
	// 有它才能区分「没扫」和「扫了但没东西删」：EventsDeleted=0 且 EventsDisabled=true = 压根没扫。
	EventsDisabled bool
	// Truncated 表示本轮有范围被批数预算截断（还有积压，下一轮继续）。
	Truncated bool
}

// ---------- 实现 ----------

// ListResultRetentionScopes 列出需要按 TTL 清理终态任务的范围（命名空间 × 能力版本）。
//
// 只要三类行：终态四态、finished_at 已落（非终态不该有 finished_at，脏数据同理跳过）、
// 以及所属能力版本声明了 resultTtlSec > 0。
//
// lifecycle 用 Go 侧的 json.Unmarshal 解析，而不是 SQLite 的 json_extract：
// json1 扩展是否被编译进驱动取决于构建（pure-Go 驱动的可选项），而这条路径
// 必须在任何构建下都成立。兼容性优先于「一条 SQL 搞定」的简洁。
//
// 输出按 (namespace_id, capability_id) 排序：调用方（SweepRetention）靠它保证多轮之间
// 的处理顺序确定，也让测试与日志可复现。
//
// **查询计划**（迁移 12 之后）：scope 的筛选条件与迁移 12 新建的
// `idx_tasks_retention(namespace_id, capability_id, status, finished_at)` 前缀完全对齐，
// 所以候选集能收窄到一个命名空间（+ 能力）而不是全库终态行；迁移 12 之前记录的计划
// 是「idx_tasks_lease(status=?) 全库扫终态行 + 两次 TEMP B-TREE」，不再适用。
// DISTINCT + ORDER BY 仍可能用到临时 B 树（计划未重新实测，不在此处断言）。
//
// **坏 lifecycle 不毒丸**：单条 scope 的 lifecycle 解析失败时 warn + 跳过该 scope，
// 其余 scope 与 events 阶段照常跑。老库 / 人工改库 / e2e 直插都能造出坏 JSON，
// 若在这里返回错误，SweepRetention 会在 events 阶段之前整轮中止，tasks/events
// **全部**停止清理，而且每小时重复失败一次（一轮坏数据 = 保留策略永久失效）。
func (d *DB) ListResultRetentionScopes(ctx context.Context) ([]ResultRetentionScope, error) {
	statusIn, statusArgs := terminalStatusSQL()
	rows, err := d.QueryContext(ctx,
		`SELECT DISTINCT t.namespace_id, t.capability_id, cv.lifecycle
		   FROM tasks t
		   JOIN capability_versions cv ON cv.id = t.capability_id
		  WHERE t.status IN (`+statusIn+`) AND t.finished_at IS NOT NULL
		  ORDER BY t.namespace_id, t.capability_id`, statusArgs...)
	if err != nil {
		return nil, fmt.Errorf("list retention scopes: %w", err)
	}
	defer rows.Close()

	out := []ResultRetentionScope{}
	for rows.Next() {
		var namespaceID, capabilityID, lifecycleJSON string
		if err := rows.Scan(&namespaceID, &capabilityID, &lifecycleJSON); err != nil {
			return nil, fmt.Errorf("scan retention scope: %w", err)
		}
		var lc Lifecycle
		if lifecycleJSON != "" {
			if err := json.Unmarshal([]byte(lifecycleJSON), &lc); err != nil {
				// 见方法注释「坏 lifecycle 不毒丸」：跳过这一条，不要整轮失败。
				slog.Warn("skip capability with invalid lifecycle",
					"event", "store.retention.bad_lifecycle",
					"namespace_id", namespaceID,
					"capability_id", capabilityID,
					"error", err.Error())
				continue
			}
		}
		if lc.ResultTTLSec <= 0 {
			// 未声明 TTL（0）意味着**永不清理**：放进 scope 列表只会每轮白扫一遍。
			continue
		}
		out = append(out, ResultRetentionScope{
			NamespaceID:  namespaceID,
			CapabilityID: capabilityID,
			ResultTTLSec: lc.ResultTTLSec,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate retention scopes: %w", err)
	}
	// SQL 已经排过序，这里再排一次只是把「顺序确定」写进 Go 侧契约，
	// 不依赖 SQLite 对 DISTINCT + ORDER BY 的实现细节。
	//
	// 这里不需要再按 (namespace_id, capability_id) 去重：SELECT 已经 DISTINCT，而
	// capability_versions.id 是主键（JOIN 一侧一对一），所以 (命名空间, 能力) 天然唯一。
	sort.Slice(out, func(i, j int) bool {
		if out[i].NamespaceID != out[j].NamespaceID {
			return out[i].NamespaceID < out[j].NamespaceID
		}
		return out[i].CapabilityID < out[j].CapabilityID
	})
	return out, nil
}

// DeleteExpiredTerminalTasks 删除某 (命名空间, 能力) 下 finished_at 早于 cutoff 的终态任务，
// 单次最多 limit 行，返回实际删除行数。
//
// 限批形式必须是 `DELETE ... WHERE id IN (SELECT id ... LIMIT ?)`：
// `DELETE ... LIMIT` 需要 SQLite 编译期开关（SQLITE_ENABLE_UPDATE_DELETE_LIMIT），
// 依赖它等于把可移植性交给构建产物的运气。
//
// 为什么按 finished_at 排序：一批删不完时下一轮从更老的开始，
// 积压再大也不会出现「新过期的一直被删、最老的一直留着」。
//
// **查询计划**（迁移 12 之后）：子查询的 `namespace_id = ? AND capability_id = ?`
// 与迁移 12 新建的 `idx_tasks_retention(namespace_id, capability_id, status, finished_at)`
// 前缀对齐，候选集从「整个命名空间」收窄到「某能力」；迁移 12 之前记录的
// `idx_tasks_parent(namespace_id=?)` 前缀范围扫 + TEMP B-TREE 排序。
// 仍要记住一条不变的事实：`LIMIT` 只限**删除行数**，**不限扫描/排序行数**
// ——即使只删 1000 行，子查询仍会把该范围的候选行全部扫一遍再排序；
// ORDER BY finished_at 能不能免掉排序取决于索引与计划，这里不重新实测、不做断言。
//
// 所以单批成本 ∝ 该 (命名空间, 能力) 已过期行数（首轮积压时最大），单轮最坏 ≈
// （scope 数 + 命名空间数）× Batches × Batch 行。因此 lease 侧必须异步跑（不堵 5s 巡检）。
//
// **已知残留**：`finished_at IS NOT NULL` 意味着「终态但 finished_at 为 NULL」的脏行
// **永不被回收**（生产写路径不会造出这种行，但历史库可能有）。不改行为，只记档：
// 这类行会被当成「无过期时刻」，宁可留着也不按未知时刻删。
//
// limit <= 0 直接返回 0 而不删任何行：调用方传错时「少删」远好于「删全表」。
func (d *DB) DeleteExpiredTerminalTasks(ctx context.Context, namespaceID, capabilityID string, cutoff int64, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	statusIn, statusArgs := terminalStatusSQL()
	args := make([]any, 0, 5+len(statusArgs))
	args = append(args, namespaceID, capabilityID)
	args = append(args, statusArgs...)
	args = append(args, cutoff, limit)

	res, err := d.ExecContext(ctx,
		`DELETE FROM tasks WHERE id IN (
		   SELECT id FROM tasks
		    WHERE namespace_id = ? AND capability_id = ?
		      AND finished_at IS NOT NULL
		      AND status IN (`+statusIn+`)
		      AND finished_at < ?
		    ORDER BY finished_at
		    LIMIT ?)`, args...)
	if err != nil {
		return 0, fmt.Errorf("delete expired tasks (namespace %s, capability %s): %w",
			namespaceID, capabilityID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete expired tasks rows affected: %w", err)
	}
	return n, nil
}

// DeleteExpiredEvents 删除某命名空间下 created_at 早于 cutoff 的事件，单次最多 limit 行。
//
// **只在调用方显式给出保留期时才被调用**：本函数自身不看配置，门禁在
// SweepRetention（EventRetentionSec <= 0 = 禁用，默认就是禁用）。直接调用它
// = 调用方显式表达了「这一批我要删」的意图，所以这里不再做一次开关判断。
//
// **不按 source 过滤**：source='route' 的剪枝行（#142）与外部事件受同一份保留期约束。
// 剪枝行同样是「只进不出」的增长来源，给它开特例等于把泄漏留在库里。
//
// **查询计划**（迁移 12 之后）：`namespace_id = ? AND created_at < ?` 与迁移 12 新建的
// `idx_events_retention(namespace_id, created_at)` 对齐，候选集从「整个命名空间」的
// 全表扫变成索引范围扫；迁移 12 之前记录的 `idx_events_trace(namespace_id=?)` 范围扫
// + TEMP B-TREE 排序已不适用。同 DeleteExpiredTerminalTasks：`LIMIT` 只限**删除行数**，
// 不限**扫描/排序行数**；ORDER BY created_at 是否能免掉排序这里不重新实测。
//
// **语义变化（重要）**：删 events 行会同时回收 `idx_events_idem(namespace_id, source, idem_key)`
// 的去重窗口——`idem_key` 的「收过就不重做」从**永久**缩成**保留期**（部署配置
// ANOTIFY_EVENT_RETENTION_DAYS，默认 30 天；显式配 0 = 永久保留时该清理不发生，
// 去重窗口仍是永久）。
// 超期重投不会命中旧行，会新建 trace 并重新派整条链；对上游重投语义来说这通常是可接受的
// （同一个投递 ID 隔很久重放本就少见），但它是行为变化，不是纯存储优化。本轮不改这个行为。
func (d *DB) DeleteExpiredEvents(ctx context.Context, namespaceID string, cutoff int64, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	res, err := d.ExecContext(ctx,
		`DELETE FROM events WHERE id IN (
		   SELECT id FROM events
		    WHERE namespace_id = ? AND created_at < ?
		    ORDER BY created_at
		    LIMIT ?)`, namespaceID, cutoff, limit)
	if err != nil {
		return 0, fmt.Errorf("delete expired events (namespace %s): %w", namespaceID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete expired events rows affected: %w", err)
	}
	return n, nil
}

// listRetentionNamespaceIDs 列出全部命名空间 id（按 id 升序；空返回 [] 而不是 nil）。
//
// 为何按命名空间而不是按 org：隔离维度与索引前缀都是 `namespace_id`（#162），
// 而 org 层已经删除（#205）。按 org 删会变成跨命名空间的全表扫，也拿不到
// `idx_events_retention(namespace_id, created_at)` 的前缀。
func (d *DB) listRetentionNamespaceIDs(ctx context.Context) ([]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT id FROM namespaces ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list retention namespaces: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan retention namespace: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate retention namespaces: %w", err)
	}
	return out, nil
}

// SweepRetention 跑一轮保留策略巡检：先按能力清终态任务，再按命名空间清事件。
//
// 顺序是「先任务后事件」：任务行引用能力与图快照，事件是链的根。这个顺序**只**保证
// 一件事——**中断**（进程被杀 / ctx 取消）时残留的方向恒定：下游（tasks）只会比根
// （events）先少，不会反过来。它**不保证**整轮扫完不留中间态。
//
// 为什么不保证：两张表的 TTL 口径本来就不同——tasks 是**能力级** `resultTtlSec`，
// events 是**部署级**（默认 30 天，见 server.Config.EventRetentionDays；store 侧默认
// 禁用见 DefaultEventRetentionSec）。events 侧开着时，一轮**完整跑完**就会留下
// 「tasks.trace_id 指向已删 events 行」的记录：
// schema 没有跨表外键，不会报错，代价只是少了那一行「发生了什么」。
// 这是**预期状态**（父事件已删而子任务仍在，就是跨表 TTL 差异的正常结果），不是数据损坏；
// 想两边严格一致只能把两个 TTL 绑成同一个值，靠删除顺序做不到。
//
// events 阶段是**可关的**：opts.EventRetentionSec <= 0（含未配置与运维显式配 0）= 永久保留，
// 此时整个 events 阶段跳过——不 listRetentionNamespaceIDs、不 DELETE、**不返回错误**，只把
// RetentionReport.EventsDisabled 置位，Namespaces / EventsDeleted 保持 0；tasks 阶段照常跑完。
// 为什么 store 层默认关：删除不可逆，且会静默改变 store/stats.go 的「总数 / 今日 / 热力图」
// 口径。开启必须由调用方显式注入保留期（部署配置 ANOTIFY_EVENT_RETENTION_DAYS 默认 30 天，
// 经 internal/lease 注入；这是有意的分层，别把两层的默认值混成一个）。
//
// 预算：每个范围最多 Batches 批、每批 Batch 行；某批没删满说明该范围已清干净，
// 立刻换下一个范围（不必把预算空跑完）。批量删满预算 = 还有积压 → Truncated 置位，
// 交给下一轮继续排空（常态一小时后；被截断时 lease 层会用 30s 的追赶周期），
// 避免一轮长时间持写锁。
//
// 错误语义：返回**已完成的 partial report** + 包装错误（不是零值 report）——
// 一轮可能删了一部分才被打断，把这些计数丢掉就等于「删了但没人知道」。
// 调用方（lease 巡检）只记日志不中断；**本函数不负责把 partial report 写进日志**，
// 记录是调用方的义务：出错分支也应当把 report 的计数一并打出来
// （见 internal/lease 的 lease.sweeper.retention_failed / retention_cancelled）。
// 这里只保证「计数备好了」，不断言调用方一定会记。
func (d *DB) SweepRetention(ctx context.Context, opts RetentionOptions) (RetentionReport, error) {
	var rep RetentionReport
	opts = retentionWithDefaults(opts)

	if err := ctx.Err(); err != nil {
		return rep, fmt.Errorf("sweep retention: %w", err)
	}

	// ① 任务：按能力级 TTL
	scopes, err := d.ListResultRetentionScopes(ctx)
	if err != nil {
		return rep, fmt.Errorf("sweep retention: %w", err)
	}
	rep.TaskScopes = len(scopes)
	for _, sc := range scopes {
		if err := ctx.Err(); err != nil {
			return rep, fmt.Errorf("sweep retention: %w", err)
		}
		cutoff := opts.Now - int64(sc.ResultTTLSec)
		deleted, saturated, derr := drainRetentionBatches(ctx, opts.Batches, opts.Batch,
			func(limit int) (int64, error) {
				return d.DeleteExpiredTerminalTasks(ctx, sc.NamespaceID, sc.CapabilityID, cutoff, limit)
			})
		rep.TasksDeleted += deleted
		if saturated {
			rep.Truncated = true
		}
		if derr != nil {
			return rep, fmt.Errorf("sweep retention: tasks of capability %s (namespace %s): %w",
				sc.CapabilityID, sc.NamespaceID, derr)
		}
	}

	// ② 事件：按部署级保留期（**默认禁用**），删除粒度是命名空间
	//
	// 保留期 <= 0 = 未配置 = 禁用：整个 events 阶段跳过。这里刻意**不**返回错误——
	// 默认配置走的就是这条路，报错会让巡检每小时记一次假警。也不查 namespaces：
	// 既然一行都不删，就没必要每轮去扫一遍。
	if opts.EventRetentionSec <= 0 {
		rep.EventsDisabled = true
		return rep, nil
	}

	namespaceIDs, err := d.listRetentionNamespaceIDs(ctx)
	if err != nil {
		return rep, fmt.Errorf("sweep retention: %w", err)
	}
	// 保留期是**部署级配置**（#205 之后不再有 per-org 配置），删除动作按命名空间逐个做
	// （索引与隔离维度都是 namespace_id）。
	rep.Namespaces = len(namespaceIDs)
	eventsCutoff := opts.Now - int64(opts.EventRetentionSec)
	for _, nsID := range namespaceIDs {
		if err := ctx.Err(); err != nil {
			return rep, fmt.Errorf("sweep retention: %w", err)
		}
		deleted, saturated, derr := drainRetentionBatches(ctx, opts.Batches, opts.Batch,
			func(limit int) (int64, error) {
				return d.DeleteExpiredEvents(ctx, nsID, eventsCutoff, limit)
			})
		rep.EventsDeleted += deleted
		if saturated {
			rep.Truncated = true
		}
		if derr != nil {
			return rep, fmt.Errorf("sweep retention: events of namespace %s: %w", nsID, derr)
		}
	}
	return rep, nil
}

// retentionWithDefaults 把未设置的 Now / Batch / Batches 补成默认值。
//
// **EventRetentionSec 不补默认值**：该字段的 0 是「禁用」而不是「没设置」，
// 补成某个正数秒会让「默认禁用」永远到不了 SweepRetention 的 events 阶段。
// 负值在这里归一成 0（仍然不会开启 events 清理）。
func retentionWithDefaults(opts RetentionOptions) RetentionOptions {
	if opts.Now == 0 {
		opts.Now = Now()
	}
	if opts.EventRetentionSec < 0 {
		opts.EventRetentionSec = EventRetentionDisabled
	}
	if opts.Batch <= 0 {
		opts.Batch = DefaultRetentionBatch
	}
	if opts.Batches <= 0 {
		opts.Batches = DefaultRetentionBatches
	}
	return opts
}

// drainRetentionBatches 在一个范围内按批调用 one，返回删掉的总行数与「预算是否用尽」。
//
// saturated=true 表示 Batches 批全部删满（每批都是 limit 行），即该范围**可能**
// 还有积压——置 Truncated 让运维知道下一轮还得跑。某批没删满（含 0）即视为清干净。
func drainRetentionBatches(ctx context.Context, batches, batch int, one func(limit int) (int64, error)) (deleted int64, saturated bool, err error) {
	for i := 0; i < batches; i++ {
		if err := ctx.Err(); err != nil {
			return deleted, saturated, err
		}
		n, err := one(batch)
		if err != nil {
			return deleted, saturated, err
		}
		deleted += n
		if n < int64(batch) {
			return deleted, false, nil // 没删满 = 这个范围已清干净
		}
		saturated = true
	}
	return deleted, saturated, nil
}

// terminalStatusSQL 生成终态列表对应的占位符与参数。
//
// 手写 `IN (?,?,?,?)` 与 terminalStatusList 是两份事实——加一个终态时漏改一处
// 会静默地少清理一类任务。用同一个列表生成，天然不漂移。
func terminalStatusSQL() (placeholders string, args []any) {
	ph := make([]string, len(terminalStatusList))
	args = make([]any, len(terminalStatusList))
	for i, status := range terminalStatusList {
		ph[i] = "?"
		args[i] = status
	}
	return strings.Join(ph, ","), args
}
