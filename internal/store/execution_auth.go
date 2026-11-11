package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

// 本文件是**认证执行入口**的唯一归属（G1-6C3 / C3a）。
//
// 为什么必须把认证放进写事务：旧的公开 GrantLease / RenewLease / CompleteTask 只带
// caller 自报的 nodeID，没有任何节点凭证。上层「先校验 session 再调 store」会出现
// TOCTOU——校验与写入之间节点令牌被轮换、namespace 被撤销、任务被取消/重派，
// 旧 session 仍然能把授权写进去。现在每个入口都要求 NodeAuthorization
// （来自当前 NodeSession：nodeId + token sha256），并在**同一个 BEGIN IMMEDIATE
// 事务**里重读 nodes.token_hash、node 归属、namespace 交集与任务当前执行事实。
//
// 公开的无认证执行入口已全部退休（不是保留为兼容层）：GrantLease / RenewLease /
// CompleteTask(At) / AckCancel 不再导出，任何 WS/HTTP 调用方都必须走这里的
// *Authorized 入口。旧签名没有兼容 bypass。

// NodeAuthorization 是一次执行授权请求的**认证身份**。
//
// 它必须由认证层从当前 NodeSession 构造：NodeID 是会话绑定的 nodes.id，
// NodeTokenHash 是该节点令牌的 sha256（明文只在发放时存在一次，永不进 store）。
// 禁止从请求体里取 caller 自报的 nodeId 拼一个 NodeAuthorization。
type NodeAuthorization struct {
	NodeID        string
	NodeTokenHash string
}

// validate 校验认证身份形状（空身份一律 fail closed）。
func (a NodeAuthorization) validate() error {
	if a.NodeID == "" || a.NodeTokenHash == "" {
		return fmt.Errorf("%w: 执行授权必须来自已认证的节点会话（nodeId + tokenHash）", ErrNodeIdentityMismatch)
	}
	return nil
}

// authorizeNodeOn 在同一事务/句柄上校验「这还是不是那台机器」。
//
// 返回 nodes.declared_namespaces 原串，供同事务的 namespace 交集判定复用；
// token_hash 比较针对**当前库里的事实**：令牌轮换后旧 session 立刻失效。
func authorizeNodeOn(ctx context.Context, ex queryExec, auth NodeAuthorization) (string, error) {
	if err := auth.validate(); err != nil {
		return "", err
	}
	var (
		tokenHash     string
		rawDeclaredNS string
	)
	err := ex.queryRowContext(ctx,
		`SELECT token_hash, declared_namespaces FROM nodes WHERE id = ?`, auth.NodeID).
		Scan(&tokenHash, &rawDeclaredNS)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: node %s 不存在", ErrNodeIdentityMismatch, auth.NodeID)
	}
	if err != nil {
		return "", fmt.Errorf("执行授权: 读节点身份: %w", err)
	}
	if tokenHash == "" || tokenHash != auth.NodeTokenHash {
		return "", fmt.Errorf("%w: node %s 的令牌与提交者不一致", ErrNodeIdentityMismatch, auth.NodeID)
	}
	return rawDeclaredNS, nil
}

// namespaceAllowedOn 校验节点对任务所属 namespace 的授权（声明 ∩ 授权）。
//
// 缺失 node_configs 行时默认授权 ["*"]（与 AuthorizedNamespaces 同口径），
// 但节点自己申报的 ns 交集仍然必须覆盖目标 ns。
func namespaceAllowedOn(ctx context.Context, ex queryExec, nodeID, rawDeclaredNS, namespaceID string) error {
	authorized := []string{NamespaceWildcard}
	var rawAuthorized string
	qerr := ex.queryRowContext(ctx,
		`SELECT namespaces FROM node_configs WHERE node_id = ?`, nodeID).Scan(&rawAuthorized)
	if qerr != nil && !errors.Is(qerr, sql.ErrNoRows) {
		return fmt.Errorf("执行授权: 读节点配置: %w", qerr)
	}
	if qerr == nil {
		decoded, derr := decodeStringList(rawAuthorized)
		if derr != nil {
			return fmt.Errorf("执行授权: %w", derr)
		}
		authorized = decoded
	}
	declared, derr := decodeStringList(rawDeclaredNS)
	if derr != nil {
		return fmt.Errorf("执行授权: %w", derr)
	}
	var namespaceName string
	if serr := ex.queryRowContext(ctx,
		`SELECT slug FROM namespaces WHERE id = ?`, namespaceID).Scan(&namespaceName); serr != nil {
		if errors.Is(serr, sql.ErrNoRows) {
			return fmt.Errorf("%w: 任务所属命名空间 %s 不存在", ErrNamespaceRequired, namespaceID)
		}
		return fmt.Errorf("执行授权: 读命名空间: %w", serr)
	}
	if !namespacesAllow(intersectNamespaces(authorized, declared), namespaceName) {
		return fmt.Errorf("%w: 节点 %s 未获授权访问命名空间 %s",
			ErrEmitNamespaceDenied, nodeID, namespaceName)
	}
	return nil
}

// maxPerNodeOn 读任务**钉住的能力版本**里的 concurrency.maxPerNode（跨版本预算是
// pinned 值：执行中的 attempt 不因能力后来改了 concurrency 而改变自己的预算口径）。
//
// 能力行已被删掉（孤儿任务）时给默认值 1，避免把「找不到能力」当成「无限预算」。
func maxPerNodeOn(ctx context.Context, ex queryExec, capabilityID string) (int, error) {
	var specJSON string
	err := ex.queryRowContext(ctx,
		`SELECT spec FROM capability_versions WHERE id = ?`, capabilityID).Scan(&specJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return artifact.DefaultMaxPerNode, nil
	}
	if err != nil {
		return 0, fmt.Errorf("授予租约: 读能力并发预算: %w", err)
	}
	var spec artifact.Spec
	if trimmed := strings.TrimSpace(specJSON); trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &spec); err != nil {
			return 0, fmt.Errorf("授予租约: 解码能力 spec: %w", err)
		}
	}
	return spec.Concurrency.EffectiveMaxPerNode(), nil
}

// grantBudgetOn 在同一授权事务里执行三层独立的执行预算（G1-6C3 / D6）：
//
//  1. **机器总量**：local > node 合并后的 limits.total（machineConfigSnapshot.limits
//     已经是合并结果）。这是整台机器的并发上限，跨 namespace、跨能力。
//  2. **按能力名**：limits.perCapability[能力名]，没有单列就用 "*"；口径是
//     **跨 namespace 的同名能力**（namespaces 不是预算维度；机器只有一台）。
//  3. **pinned maxPerNode**：任务钉住的能力版本里的 concurrency.maxPerNode，
//     口径是**同 namespace + 同能力名、跨版本**在这台机器上的并发数。
//
// 占用口径（三条预算共用）：
//   - **未回收的真实执行**：task_attempts 里 reaped_at IS NULL 的每一行。不看任务
//     相位（running/pending/终态都可能还压着一个没回收的进程组），phase 变化、
//     rehello、租约过期都不会假释放它。
//   - **真实 busy reservation**：节点已接受（Busy=true）并落进真实槽账本
//     （node_instances.busy_lease_id 为空）的候选。纯中枢侧 assignment（节点还没接住）
//     不是物理占用，不进预算；窗口过期/被拒绝时由节点的明确 idle 事实清掉。
//   - 本次请求自己的 reservation **只算一次**：只有 node + task + assignment +
//     proposed + **同一个 slot** 全部相符的那一条被排除；同 task 在别的 slot 上的
//     预约仍是真实物理占用（不能按 taskID 整包排除，否则会少算并越过预算）。grant
//     成功时它原地从 reservation 变成 attempt（槽账本同一条绑定），新增的「+1」
//     正好就是它。
//
// 拒绝只返回 ErrSlotUnavailable（不耗 attempt、不绑槽、不改别人的 reservation）；
// 调用方负责把本条任务放回 pending + waiting_for_slot 的有界退避。缩小预算只阻止
// 新授权，存量 busy 账本原样保留（不假释放）。
func grantBudgetOn(ctx context.Context, ex queryExec, nodeID, taskID, assignmentID string,
	proposedAttempt, slot int, namespaceID, capability string,
	maxPerNode int, limits protocol.Limits) error {
	effectiveTotal := limits.Total
	if effectiveTotal <= 0 {
		effectiveTotal = protocol.DefaultLimitsTotal
	}

	// 未回收执行：每一行 attempt 都是一个仍可能占着物理槽的执行。
	countUnreaped := func(where string, args ...any) (int, error) {
		var n int
		q := `SELECT COUNT(*) FROM task_attempts a JOIN tasks t ON t.id = a.task_id
		       WHERE a.node_id = ? AND a.reaped_at IS NULL` + where
		if err := ex.queryRowContext(ctx, q, append([]any{nodeID}, args...)...).Scan(&n); err != nil {
			return 0, fmt.Errorf("授予租约: 统计未回收执行: %w", err)
		}
		return n, nil
	}
	// 真实 busy reservation：节点已接受（Busy=true）并落进**真实槽账本**的候选。
	// 判别式是 busy_lease_id=''（执行都有 lease，reservation 不伪造 lease）。
	// 纯中枢侧 assignment（节点还没接住）不在这里重复计：它不是物理占用事实。
	// 本次 grant 自己那条同槽预约只排除一次：node+task+assignment+proposed+slot
	// 全部精确相等才行；其它槽/其它 assignment 的 reservation 仍然计入。
	countReserved := func(where string, args ...any) (int, error) {
		var n int
		q := `SELECT COUNT(*) FROM node_instances i JOIN tasks t ON t.id = i.busy_task
		       WHERE i.node_id = ? AND i.busy_task IS NOT NULL AND i.busy_lease_id = ''
		         AND NOT (i.slot = ? AND i.busy_task = ? AND i.busy_attempt = ?
		                  AND i.busy_assignment_id = ?)` + where
		base := []any{nodeID, slot, taskID, proposedAttempt, assignmentID}
		if err := ex.queryRowContext(ctx, q, append(base, args...)...).Scan(&n); err != nil {
			return 0, fmt.Errorf("授予租约: 统计 busy reservation: %w", err)
		}
		return n, nil
	}
	busyOn := func(where string, args ...any) (int, error) {
		unreaped, err := countUnreaped(where, args...)
		if err != nil {
			return 0, err
		}
		reserved, err := countReserved(where, args...)
		if err != nil {
			return 0, err
		}
		return unreaped + reserved, nil
	}

	totalBusy, err := busyOn("")
	if err != nil {
		return err
	}
	if totalBusy+1 > effectiveTotal {
		return fmt.Errorf("%w: node %s 机器总量预算已满（%d/%d）",
			ErrSlotUnavailable, nodeID, totalBusy, effectiveTotal)
	}

	capLimit, hasCap := limits.PerCapability[capability]
	if !hasCap {
		capLimit, hasCap = limits.PerCapability[NamespaceWildcard]
	}
	if hasCap {
		// countUnreaped/countReserved 都是「在固定参数后追加 where 参数」，
		// 因此同一个 where 片段可以同时用于两张查询。
		capBusy, cerr := busyOn(` AND t.capability = ?`, capability)
		if cerr != nil {
			return cerr
		}
		if capBusy+1 > capLimit {
			return fmt.Errorf("%w: node %s 的能力预算 %q 已满（%d/%d）",
				ErrSlotUnavailable, nodeID, capability, capBusy, capLimit)
		}
	}

	if maxPerNode > 0 {
		pinnedBusy, perr := busyOn(` AND t.namespace_id = ? AND t.capability = ?`, namespaceID, capability)
		if perr != nil {
			return perr
		}
		if pinnedBusy+1 > maxPerNode {
			return fmt.Errorf("%w: node %s 的 pinned per-node 预算 %s/%s 已满（%d/%d）",
				ErrSlotUnavailable, nodeID, namespaceID, capability, pinnedBusy, maxPerNode)
		}
	}
	return nil
}

// GrantLeaseAuthorized 授予一次执行租约（**每次执行唯一授权入口**）。
//
// 与旧 GrantLease 的区别不是「加了一个参数」：认证/授权/预算/绑定/快照都在
// **同一个 BEGIN IMMEDIATE 事务**里完成——没有任何「先 GetNode 校验再无条件写」
// 的窗口。事务内顺序：
//
//  1. 当前节点身份（nodes.token_hash == auth.NodeTokenHash）；
//  2. 任务当前候选：assignmentId + node 归属 + proposedAttempt **三项精确匹配**，
//     且窗口未过；stale 请求只被拒绝，不改当前候选；
//  3. namespace 交集（node 声明 ∩ 授权覆盖任务 ns）；
//  4. attempt 预算（maxAttempts，超了直接终态）；
//  5. 机器配置 applied 事实；
//  6. 三层执行预算（grantBudgetOn：总量 / 跨 ns 同名能力 / pinned maxPerNode）；
//  7. 把节点 accept 回报的真实 slot 绑到 (task, attempt, lease)；
//  8. 落 attempt 机器快照（vars/limits/来源/revision/hash）+ 写任务执行授权事实。
//
// 任何失败都不消耗 attempt、不占槽、不伪造 lease；容量拒绝回 pending +
// waiting_for_slot（有界退避）。
func (d *DB) GrantLeaseAuthorized(ctx context.Context, auth NodeAuthorization,
	taskID, assignmentID string, proposedAttempt, slot int, now int64) (
	leaseID, attemptToken string, attempt int, expiresAt int64, err error) {
	if taskID == "" {
		return "", "", 0, 0, fmt.Errorf("grant lease: taskID required")
	}
	if err := auth.validate(); err != nil {
		return "", "", 0, 0, err
	}
	if proposedAttempt <= 0 {
		return "", "", 0, 0, fmt.Errorf("%w: grant 必须带候选的 proposedAttempt", ErrNoAssignment)
	}
	if slot < 0 {
		// presence 缺失：没有真实 slot 的 grant 一律不授权（不猜另一个槽）。
		return "", "", 0, 0, fmt.Errorf("%w: grant 必须带节点 accept 回报的 slot", ErrSlotUnavailable)
	}
	if now == 0 {
		now = Now()
	}
	// 授权事实先算好（leaseID 要写进 attempt 快照与槽位绑定；token 明文只返回一次）。
	leaseID = newID("lse")
	attemptToken = NewTaskToken()
	tokenHash := hashAttemptToken(attemptToken)
	expiresAt = now + int64(protocol.LeaseTTL.Seconds())

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("grant lease %s: begin: %w", taskID, err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	// ① 当前节点身份（令牌轮换后旧 session 立刻失效）。
	rawDeclared, err := authorizeNodeOn(ctx, ex, auth)
	if err != nil {
		return "", "", 0, 0, err
	}

	// 事务内重读任务（BEGIN IMMEDIATE 下读—写原子），并把 lifecycle 钉在同一事务里。
	var (
		phase                 string
		rowAssign             string
		rowAssignExpires      sql.NullInt64
		rowNode               sql.NullString
		rowProposedAttempt    int
		rowAttempt            int
		maxAttempts           int
		capabilityID          sql.NullString
		capability            string
		namespaceID           string
		cancelReq             sql.NullInt64
		namespaceVarsRevision int64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT phase, assignment_id, assignment_expires_at, node_id, proposed_attempt,
		        attempt, max_attempts, capability_id, capability, namespace_id,
		        cancel_requested_at, namespace_vars_revision
		   FROM tasks WHERE id = ?`, taskID).
		Scan(&phase, &rowAssign, &rowAssignExpires, &rowNode, &rowProposedAttempt,
			&rowAttempt, &maxAttempts, &capabilityID, &capability, &namespaceID,
			&cancelReq, &namespaceVarsRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", 0, 0, ErrNotFound
	}
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("grant lease %s: read: %w", taskID, err)
	}
	if phase != TaskPending {
		return "", "", 0, 0, fmt.Errorf("%w: task %s is %s", ErrInvalidTransition, taskID, phase)
	}
	// 未解析派发失败行（capability_id IS NULL）不是可执行任务：即使有人把它改回 pending
	// 也不能拿到租约（表上的形状 CHECK 会先拒绝这种回写，这里是同一语义的入口护栏）。
	if !capabilityID.Valid || capabilityID.String == "" {
		return "", "", 0, 0, fmt.Errorf("%w: task %s 没有解析出的能力行，不能再授执行权", ErrInvalidTransition, taskID)
	}
	if cancelReq.Valid {
		// 取消已经发出：永不再授新执行权（即使它又变回了 pending）。
		return "", "", 0, 0, fmt.Errorf("%w: task %s 已请求取消，不再授权", ErrInvalidTransition, taskID)
	}

	// 节点存在/状态必须在授予事务内重查（身份已在 ① 校验过）。
	var nodeName, nodeStat string
	if qerr := tx.QueryRowContext(ctx, `SELECT name, status FROM nodes WHERE id = ?`, auth.NodeID).
		Scan(&nodeName, &nodeStat); qerr != nil {
		if errors.Is(qerr, sql.ErrNoRows) {
			return "", "", 0, 0, fmt.Errorf("%w: node %s", ErrNotFound, auth.NodeID)
		}
		return "", "", 0, 0, fmt.Errorf("grant lease %s: read node: %w", taskID, qerr)
	}
	if nodeStat != NodeStatusOnline {
		return "", "", 0, 0, fmt.Errorf("%w: node %s is %s（不再接新任务）", ErrInvalidTransition, auth.NodeID, nodeStat)
	}

	// ② 候选身份必须精确匹配（assignment + node + proposedAttempt）。
	// 旧/空/错 proposed 的 grant 绝不能清掉新候选，也不能耗尽预算推进终态。
	if rowAssign == "" || rowAssign != assignmentID || !rowNode.Valid || rowNode.String != auth.NodeID {
		return "", "", 0, 0, fmt.Errorf("%w: task %s 的 assignment %q 不匹配当前候选 %q（node %s）",
			ErrNoAssignment, taskID, assignmentID, rowAssign, auth.NodeID)
	}
	if rowProposedAttempt != proposedAttempt {
		return "", "", 0, 0, fmt.Errorf("%w: task %s 的 proposedAttempt %d != %d",
			ErrNoAssignment, taskID, proposedAttempt, rowProposedAttempt)
	}
	if !rowAssignExpires.Valid || rowAssignExpires.Int64 <= now {
		if _, uerr := tx.ExecContext(ctx,
			`UPDATE tasks SET assignment_id = '', proposed_attempt = 0, assignment_expires_at = NULL,
			        node_id = NULL, node_name = ''
			  WHERE id = ? AND phase = ? AND assignment_id = ? AND node_id = ? AND proposed_attempt = ?`,
			taskID, TaskPending, assignmentID, auth.NodeID, proposedAttempt); uerr != nil {
			return "", "", 0, 0, fmt.Errorf("grant lease %s: clear expired assignment: %w", taskID, uerr)
		}
		if cerr := tx.Commit(); cerr != nil {
			return "", "", 0, 0, fmt.Errorf("grant lease %s: commit expired assignment: %w", taskID, cerr)
		}
		return "", "", 0, 0, fmt.Errorf("%w: task %s 的 assignment %q 已过期", ErrNoAssignment, taskID, assignmentID)
	}
	// ③ namespace 交集在授予事务内重查（assign 之后撤销授权必须生效）。
	if err := namespaceAllowedOn(ctx, ex, auth.NodeID, rawDeclared, namespaceID); err != nil {
		return "", "", 0, 0, err
	}

	lc, lerr := lifecycleOn(ctx, ex, capabilityID.String)
	if lerr != nil {
		return "", "", 0, 0, fmt.Errorf("grant lease %s: %w", taskID, lerr)
	}
	nextAttempt := rowAttempt + 1
	maxAttempts = lc.EffectiveMaxAttempts()

	if nextAttempt > maxAttempts {
		// 预算用尽：不再授租约，直接终态。错误码是调度层的 max_attempts_exceeded，
		// 所以相位取 abandoned（不是能力自己的失败）。这条路径还没占过槽。
		if _, uerr := tx.ExecContext(ctx,
			`UPDATE tasks SET assignment_id = '', proposed_attempt = 0, assignment_expires_at = NULL,
			        node_id = NULL, node_name = ''
			  WHERE id = ?`, taskID); uerr != nil {
			return "", "", 0, 0, fmt.Errorf("grant lease %s: clear assignment: %w", taskID, uerr)
		}
		if ok, ferr := d.finishTxGuarded(ctx, tx, taskID, TaskPending, TaskAbandoned, "", nil, now); ferr != nil {
			return "", "", 0, 0, ferr
		} else if !ok {
			return "", "", 0, 0, fmt.Errorf("%w: task %s changed phase concurrently", ErrInvalidTransition, taskID)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET error = ? WHERE id = ?`, protocol.ReasonMaxAttemptsExceeded, taskID); err != nil {
			return "", "", 0, 0, fmt.Errorf("grant lease %s: write reason: %w", taskID, err)
		}
		if err := tx.Commit(); err != nil {
			return "", "", 0, 0, fmt.Errorf("grant lease %s: commit terminal: %w", taskID, err)
		}
		return "", "", 0, 0, fmt.Errorf("%w: task %s used %d/%d attempts",
			ErrMaxAttemptsExceeded, taskID, rowAttempt, maxAttempts)
	}

	// 校验节点机器配置已 apply（同事务，另一处读机器值的地方不存在）。
	snap, serr := loadMachineSnapshotOn(ctx, ex, auth.NodeID)
	if serr != nil {
		return "", "", 0, 0, fmt.Errorf("grant lease %s: %w", taskID, serr)
	}
	if snap.needsAppliedFact() && !snap.appliedMatches() {
		// 配置尚未生效：这不是执行失败（不计 attempt、不占槽）。语义与 task.reject 的
		// stale_config 同源（assign 与 grant 之间配置又变了的竞态），任务回 pending
		// + waiting_for_config 并退避，不发热循环。
		if _, uerr := tx.ExecContext(ctx,
			`UPDATE tasks
			    SET assignment_id = '', proposed_attempt = 0, assignment_expires_at = NULL,
			        node_id = NULL, node_name = '',
			        waiting_reason = ?, next_attempt_at = ?
			  WHERE id = ? AND phase = ? AND cancel_requested_at IS NULL`,
			WaitingForConfig, now+StaleConfigBackoff(rowAttempt), taskID, TaskPending); uerr != nil {
			return "", "", 0, 0, fmt.Errorf("grant lease %s: write waiting config: %w", taskID, uerr)
		}
		if cerr := tx.Commit(); cerr != nil {
			return "", "", 0, 0, fmt.Errorf("grant lease %s: commit waiting config: %w", taskID, cerr)
		}
		return "", "", 0, 0, fmt.Errorf("%w: 节点 %s 的机器配置尚未 apply（revision %d, applied %d, hash %q != %q）",
			ErrConfigNotApplied, auth.NodeID, snap.revision, snap.appliedRevision, snap.appliedHash, snap.hash)
	}

	// ⑤ 三层执行预算（同事务内统计 + 拒绝；缩小预算不假释放存量 busy）。
	typedLimits, terr := TypedMachineLimits(snap.limits)
	if terr != nil {
		return "", "", 0, 0, fmt.Errorf("grant lease %s: %w", taskID, terr)
	}
	pinnedMaxPerNode, merr := maxPerNodeOn(ctx, ex, capabilityID.String)
	if merr != nil {
		return "", "", 0, 0, fmt.Errorf("grant lease %s: %w", taskID, merr)
	}
	if berr := grantBudgetOn(ctx, ex, auth.NodeID, taskID, assignmentID, proposedAttempt, slot,
		namespaceID, capability, pinnedMaxPerNode, typedLimits); berr != nil {
		if _, uerr := tx.ExecContext(ctx,
			`UPDATE tasks
			    SET assignment_id = '', proposed_attempt = 0, assignment_expires_at = NULL,
			        node_id = NULL, node_name = '', waiting_reason = ?, next_attempt_at = ?
			  WHERE id = ? AND phase = ? AND cancel_requested_at IS NULL`,
			WaitingForSlot, now+SlotUnavailableBackoff(rowAttempt), taskID, TaskPending); uerr != nil {
			return "", "", 0, 0, fmt.Errorf("grant lease %s: write waiting slot: %w", taskID, uerr)
		}
		if cerr := tx.Commit(); cerr != nil {
			return "", "", 0, 0, fmt.Errorf("grant lease %s: commit waiting slot: %w", taskID, cerr)
		}
		return "", "", 0, 0, berr
	}
	// slot 上限仍按机器总量判定（真实槽位编号必须落在当前预算内）。
	effectiveTotal := typedLimits.Total
	if effectiveTotal <= 0 {
		effectiveTotal = protocol.DefaultLimitsTotal
	}
	if slot >= effectiveTotal {
		if _, uerr := tx.ExecContext(ctx,
			`UPDATE tasks
			    SET assignment_id = '', proposed_attempt = 0, assignment_expires_at = NULL,
			        node_id = NULL, node_name = '', waiting_reason = ?, next_attempt_at = ?
			  WHERE id = ? AND phase = ? AND cancel_requested_at IS NULL`,
			WaitingForSlot, now+SlotUnavailableBackoff(rowAttempt), taskID, TaskPending); uerr != nil {
			return "", "", 0, 0, fmt.Errorf("grant lease %s: write waiting slot: %w", taskID, uerr)
		}
		if cerr := tx.Commit(); cerr != nil {
			return "", "", 0, 0, fmt.Errorf("grant lease %s: commit waiting slot: %w", taskID, cerr)
		}
		return "", "", 0, 0, fmt.Errorf("%w: node %s slot %d 超出预算（slot=%d/%d）",
			ErrSlotUnavailable, auth.NodeID, slot, slot, effectiveTotal)
	}

	// 把节点 accept 的真实 slot 绑到这次执行（CAS：该 slot 的旧占用未回收则失败）。
	instanceID, lerr := bindSlotOn(ctx, ex, auth.NodeID, slot, taskID, nextAttempt, leaseID,
		assignmentID, proposedAttempt, now)
	if errors.Is(lerr, ErrSlotUnavailable) {
		// 与 task.reject 的 slot_unavailable 同源：不执行、不授 lease、不计 attempt，
		// 任务回 pending + waiting_for_slot，带有界退避（不热循环）。
		if _, uerr := tx.ExecContext(ctx,
			`UPDATE tasks
			    SET assignment_id = '', proposed_attempt = 0, assignment_expires_at = NULL,
			        node_id = NULL, node_name = '', waiting_reason = ?, next_attempt_at = ?
			  WHERE id = ? AND phase = ? AND cancel_requested_at IS NULL`,
			WaitingForSlot, now+SlotUnavailableBackoff(rowAttempt), taskID, TaskPending); uerr != nil {
			return "", "", 0, 0, fmt.Errorf("grant lease %s: write waiting slot: %w", taskID, uerr)
		}
		if cerr := tx.Commit(); cerr != nil {
			return "", "", 0, 0, fmt.Errorf("grant lease %s: commit waiting slot: %w", taskID, cerr)
		}
		return "", "", 0, 0, lerr
	}
	if lerr != nil {
		return "", "", 0, 0, fmt.Errorf("grant lease %s: %w", taskID, lerr)
	}

	// attempt 快照与任务当前授权事实**同一事务**落盘：两者要么同时存在，要么都不存在。
	if err := insertTaskAttemptOn(ctx, ex, &TaskAttempt{
		CapabilityID:          capabilityID.String,
		TaskID:                taskID,
		Attempt:               nextAttempt,
		NodeID:                auth.NodeID,
		NodeName:              nodeName,
		InstanceID:            instanceID,
		Slot:                  slot,
		LeaseID:               leaseID,
		AttemptTokenHash:      tokenHash,
		ConfigRevision:        snap.revision,
		ConfigHash:            snap.hash,
		MachineVars:           snap.vars,
		MachineVarSources:     snap.varSources,
		MachineLimits:         snap.limits,
		NamespaceVarsRevision: namespaceVarsRevision,
		Phase:                 AttemptPhaseRunning,
		StartedAt:             &now,
	}); err != nil {
		return "", "", 0, 0, fmt.Errorf("grant lease %s: %w", taskID, err)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE tasks
		    SET phase = ?, attempt = ?, assignment_id = '', proposed_attempt = 0,
		        assignment_expires_at = NULL,
		        idempotent = ?, max_attempts = ?, node_id = ?, node_name = ?, config_revision = ?,
		        lease_id = ?, lease_until = ?, lease_instance_id = ?, attempt_token_hash = ?,
		        waiting_reason = '', next_attempt_at = NULL,
		        started_at = COALESCE(started_at, ?)
		  WHERE id = ? AND phase = ? AND cancel_requested_at IS NULL`,
		TaskRunning, nextAttempt, boolToInt(lc.Idempotent), maxAttempts, auth.NodeID, nodeName, snap.revision,
		leaseID, expiresAt, instanceID, tokenHash, now, taskID, TaskPending)
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("grant lease %s: write: %w", taskID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", "", 0, 0, fmt.Errorf("%w: task %s changed phase concurrently", ErrInvalidTransition, taskID)
	}
	if err := tx.Commit(); err != nil {
		return "", "", 0, 0, fmt.Errorf("grant lease %s: commit: %w", taskID, err)
	}
	return leaseID, attemptToken, nextAttempt, expiresAt, nil
}

// RenewLeaseAuthorized 续租（**当前认证身份** + 当前执行事实）。
//
// 同事务判据：nodes.token_hash == auth.NodeTokenHash、任务归 auth.NodeID、
// ns 交集仍成立、phase=running、attempt/lease 相等、**没有取消在飞**、
// 租约未过期（now >= lease_until 一律拒绝——`expiry==now` 不得复活授权）。
// 全部通过才把租约推到 now+60s；任何一项对不上都是 ErrLeaseMismatch，写入零行。
//
// wire 的 lease.renew 没有 attemptToken 字段：当前认证节点 + attempt + lease 就是
// 它的授权凭据（不发明新字段）。
func (d *DB) RenewLeaseAuthorized(ctx context.Context, auth NodeAuthorization,
	taskID string, attempt int, leaseID string, now int64) (int64, error) {
	if taskID == "" || leaseID == "" {
		return 0, fmt.Errorf("renew lease: taskID/leaseID required")
	}
	if err := auth.validate(); err != nil {
		return 0, err
	}
	if now == 0 {
		now = Now()
	}
	expiresAt := now + int64(protocol.LeaseTTL.Seconds())

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("renew lease %s: begin: %w", taskID, err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	rawDeclared, err := authorizeNodeOn(ctx, ex, auth)
	if err != nil {
		return 0, err
	}

	var (
		namespaceID  string
		phase        string
		nodeID       sql.NullString
		rowAttempt   int
		rowLease     string
		leaseUntil   sql.NullInt64
		cancelReq    sql.NullInt64
		cancelReason sql.NullString
	)
	err = tx.QueryRowContext(ctx,
		`SELECT namespace_id, phase, node_id, attempt, lease_id, lease_until,
		        cancel_requested_at, cancel_reason
		   FROM tasks WHERE id = ?`, taskID).
		Scan(&namespaceID, &phase, &nodeID, &rowAttempt, &rowLease, &leaseUntil,
			&cancelReq, &cancelReason)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: task %s", ErrLeaseMismatch, taskID)
	}
	if err != nil {
		return 0, fmt.Errorf("renew lease %s: read: %w", taskID, err)
	}
	if phase != TaskRunning {
		return 0, fmt.Errorf("%w: task %s is %s", ErrLeaseMismatch, taskID, phase)
	}
	if !nodeID.Valid || nodeID.String != auth.NodeID {
		return 0, fmt.Errorf("%w: task %s 归节点 %v，不是 %s",
			ErrLeaseMismatch, taskID, nodeID.String, auth.NodeID)
	}
	if rowAttempt != attempt || rowLease != leaseID {
		return 0, fmt.Errorf("%w: task %s attempt/lease 已变（have %d/%s, want %d/%s）",
			ErrLeaseMismatch, taskID, rowAttempt, rowLease, attempt, leaseID)
	}
	if cancelReq.Valid {
		return 0, fmt.Errorf("%w: task %s 已请求取消（%s），执行授权已收回",
			ErrLeaseMismatch, taskID, cancelReason.String)
	}
	if !leaseUntil.Valid || leaseUntil.Int64 <= now {
		// expiry==now 也拒：已经到点的授权不得被一次续租复活。
		return 0, fmt.Errorf("%w: task %s 的租约已过期", ErrLeaseMismatch, taskID)
	}
	if err := namespaceAllowedOn(ctx, ex, auth.NodeID, rawDeclared, namespaceID); err != nil {
		return 0, err
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE tasks SET lease_until = ?
		  WHERE id = ? AND phase = ? AND attempt = ? AND lease_id = ? AND node_id = ?
		    AND cancel_requested_at IS NULL AND lease_until > ?`,
		expiresAt, taskID, TaskRunning, attempt, leaseID, auth.NodeID, now)
	if err != nil {
		return 0, fmt.Errorf("renew lease %s: %w", taskID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, fmt.Errorf("%w: task %s attempt %d lease %s", ErrLeaseMismatch, taskID, attempt, leaseID)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("renew lease %s: commit: %w", taskID, err)
	}
	return expiresAt, nil
}

// TaskResultDisposition distinguishes a newly committed fact from a read-only ACK.
type TaskResultDisposition string

const (
	TaskResultRecorded  TaskResultDisposition = "recorded"
	TaskResultDuplicate TaskResultDisposition = "duplicate"
)

// CompleteTaskAuthorized 写入一次执行结果（**当前认证身份** + 当前执行事实）。
//
// 五条授权事实全部在同**一**个事务里校验：当前节点凭证、任务归该节点、ns 交集、
// 当前 attempt/lease、phase=running、未取消、**租约未过期**。
// 取消在飞时一律拒（ErrTaskNotRunning）：结局由 cancel.ack 裁定。
// 过期租约的 result 必须拒：那一代执行已经失去授权（巡检随时会重派），
// 接受它会与下一次尝试争着写同一行终态，旧执行不得复活。
//
// task.result 没有明文 attemptToken：认证 NodeSession 的实际凭证 + 当前 lease
// 绑定就是该通道的授权依据，不从库里的 token hash 制造 caller 凭证。
// Upload / Emit 已传的 attemptToken 仍按各自通道严格校验，Renew 不变。
// 正常结束 = 子进程已退出：给当前 attempt 落 reaped 事实并只释放它自己绑定的槽。
func (d *DB) CompleteTaskAuthorized(ctx context.Context, auth NodeAuthorization,
	taskID string, attempt int, leaseID string, now int64, result TaskResult) error {
	_, err := d.CompleteTaskClassifiedAuthorized(ctx, auth, taskID, attempt, leaseID, now, result)
	return err
}

// CompleteTaskClassifiedAuthorized is G2's explicit ACK classification. A
// duplicate authenticates the current credential/ns and exact current binding and retained
// task/attempt/node/lease/result evidence, and performs no DML or new-slot mutation.
func (d *DB) CompleteTaskClassifiedAuthorized(ctx context.Context, auth NodeAuthorization,
	taskID string, attempt int, leaseID string, now int64, result TaskResult) (TaskResultDisposition, error) {
	duplicate := false
	if err := d.completeTaskAuthorized(ctx, auth, taskID, attempt, leaseID, now, result, &duplicate); err != nil {
		return "", err
	}
	if duplicate {
		return TaskResultDuplicate, nil
	}
	return TaskResultRecorded, nil
}

func (d *DB) completeTaskAuthorized(ctx context.Context, auth NodeAuthorization,
	taskID string, attempt int, leaseID string, now int64, result TaskResult, duplicate *bool) error {
	if !IsTerminalPhase(result.Phase) || result.Phase == TaskCancelled {
		return fmt.Errorf("%w: result phase must be succeeded|failed|abandoned, got %q",
			ErrInvalidTransition, result.Phase)
	}
	if taskID == "" || leaseID == "" || attempt <= 0 {
		return fmt.Errorf("%w: result 必须带 task/attempt/lease", ErrLeaseMismatch)
	}
	// Node timestamps are process evidence, never the authorization/TTL clock.
	// Only basic order is checked when both facts exist; before-spawn is legal.
	result.StartedAt = result.StartedAt.UTC()
	result.FinishedAt = result.FinishedAt.UTC()
	if !result.StartedAt.IsZero() && !result.FinishedAt.IsZero() && result.StartedAt.After(result.FinishedAt) {
		return fmt.Errorf("%w: result start is after finish", ErrInvalidTransition)
	}
	if err := auth.validate(); err != nil {
		return err
	}
	if now == 0 {
		now = Now()
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("complete task %s: begin: %w", taskID, err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	rawDeclared, err := authorizeNodeOn(ctx, ex, auth)
	if err != nil {
		return err
	}

	var (
		namespaceID     string
		phase           string
		rowAttempt      int
		rowLease        string
		nodeID          sql.NullString
		cancelReq       sql.NullInt64
		rowCancelReason sql.NullString
		leaseUntil      sql.NullInt64
		instanceID      string
		rowResultJSON   string
		acceptedAt      sql.NullInt64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT namespace_id, phase, attempt, lease_id, node_id,
		        cancel_requested_at, cancel_reason, lease_until, lease_instance_id,
		        result_json, result_accepted_at
		   FROM tasks WHERE id = ?`, taskID).
		Scan(&namespaceID, &phase, &rowAttempt, &rowLease, &nodeID,
			&cancelReq, &rowCancelReason, &leaseUntil, &instanceID, &rowResultJSON, &acceptedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("complete task %s: read: %w", taskID, err)
	}
	// Current identity/ns precede both fresh result and historical idempotence.
	if err := namespaceAllowedOn(ctx, ex, auth.NodeID, rawDeclared, namespaceID); err != nil {
		return err
	}
	historical, err := getTaskAttemptOn(ctx, ex, taskID, attempt)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: result attempt %s/%d has no retained binding", ErrLeaseMismatch, taskID, attempt)
	}
	if err != nil {
		return err
	}
	if historical.NodeID != auth.NodeID || historical.LeaseID != leaseID {
		return ErrLeaseMismatch
	}
	if len(historical.ResultJSON) > 0 {
		// Parent C3b clarification: retained history is evidence, not authority to
		// ACK an old attempt once a new current binding exists. Terminal writes clear
		// tasks.lease_id; its immutable CURRENT attempt row retains the exact lease.
		if rowAttempt != attempt || !nodeID.Valid || nodeID.String != auth.NodeID ||
			(rowLease != "" && rowLease != leaseID) || !IsTerminalPhase(phase) || phase != result.Phase {
			return ErrLeaseMismatch
		}
		canonical, err := canonicalManifest(result.Artifacts)
		if err != nil {
			return err
		}
		result.Artifacts = canonical
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if string(encoded) != string(historical.ResultJSON) || rowResultJSON != string(encoded) ||
			!acceptedAt.Valid || historical.ResultAcceptedAt == nil || acceptedAt.Int64 != *historical.ResultAcceptedAt {
			return fmt.Errorf("%w: complete retained result differs or is missing", ErrInvalidTransition)
		}
		// Retained typed result references must still exist as exact recorded refs.
		// Phase/history JSON alone is not evidence after manifest facts were GCed.
		var resultRefs int
		if err := ex.queryRowContext(ctx, `SELECT COUNT(*) FROM task_artifact_manifests WHERE task_id=? AND attempt=? AND result_ref=1`, taskID, attempt).Scan(&resultRefs); err != nil {
			return err
		}
		if resultRefs != len(result.Artifacts) {
			return ErrArtifactReference
		}
		for _, ref := range result.Artifacts {
			var n int
			if err := ex.queryRowContext(ctx, `SELECT COUNT(*) FROM task_artifact_manifests WHERE task_id=? AND attempt=? AND path=? AND sha256=? AND result_ref=1`, taskID, attempt, ref.Path, ref.SHA256).Scan(&n); err != nil {
				return err
			}
			if n != 1 {
				return ErrArtifactReference
			}
		}
		*duplicate = true
		// Rollback of this read-only transaction deliberately performs zero DML.
		return nil
	}
	if IsTerminalPhase(phase) {
		return fmt.Errorf("%w: task %s already %s", ErrInvalidTransition, taskID, phase)
	}
	if phase != TaskRunning {
		return fmt.Errorf("%w: task %s is %s", ErrInvalidTransition, taskID, phase)
	}
	if !nodeID.Valid || nodeID.String != auth.NodeID {
		return fmt.Errorf("%w: task %s 归节点 %v，不是 %s",
			ErrLeaseMismatch, taskID, nodeID.String, auth.NodeID)
	}
	if rowAttempt != attempt || rowLease != leaseID {
		return fmt.Errorf("%w: task %s attempt/lease mismatch (have %d/%s, want %d/%s)",
			ErrLeaseMismatch, taskID, rowAttempt, rowLease, attempt, leaseID)
	}
	if cancelReq.Valid {
		return fmt.Errorf("%w: task %s 已请求取消（%s），结果由 cancel.ack 裁定",
			ErrTaskNotRunning, taskID, rowCancelReason.String)
	}
	if !leaseUntil.Valid || leaseUntil.Int64 <= now {
		return fmt.Errorf("%w: task %s 的租约已过期（结果不再被接受）", ErrLeaseMismatch, taskID)
	}

	result, err = validateResultManifestOn(ctx, ex, taskID, attempt, result)
	if err != nil {
		return err
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return err
	}
	artifactsJSON, err := json.Marshal(result.Artifacts)
	if err != nil {
		return err
	}
	startedAt, finishedAt := resultTimeSeconds(result.StartedAt), resultTimeSeconds(result.FinishedAt)
	if _, err := tx.ExecContext(ctx, `UPDATE task_attempts SET artifacts=?,result_json=?,started_at=?,finished_at=?,result_accepted_at=COALESCE(result_accepted_at,?) WHERE task_id=? AND attempt=?`, string(artifactsJSON), string(resultJSON), startedAt, finishedAt, now, taskID, attempt); err != nil {
		return fmt.Errorf("record attempt result: %w", err)
	}
	for _, a := range result.Artifacts {
		if _, err := tx.ExecContext(ctx, `UPDATE task_artifact_manifests SET result_ref=1 WHERE task_id=? AND attempt=? AND path=? AND sha256=?`, taskID, attempt, a.Path, a.SHA256); err != nil {
			return fmt.Errorf("record result ref: %w", err)
		}
	}
	ok, err := d.finishTxGuarded(ctx, tx, taskID, TaskRunning, result.Phase, result.Summary, result.Artifacts, now)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: task %s changed phase concurrently", ErrInvalidTransition, taskID)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET reported = 1, error = ?, result_json = ?, started_at = ?, finished_at = ?, result_accepted_at = COALESCE(result_accepted_at, ?) WHERE id = ?`, normalizeTaskError(result), string(resultJSON), startedAt, finishedAt, now, taskID); err != nil {
		return fmt.Errorf("complete task %s: write result: %w", taskID, err)
	}

	// 正常 result 到达 = 子进程已退出：给**当前** attempt 落 reaped 事实，并只释放它
	// 自己绑定的那个槽。旧 attempt 的槽属于另一条绑定（另一个槽/另一个进程），不动。
	if _, aerr := finishAttemptOn(ctx, ex, taskID, attempt, result.Phase,
		normalizeTaskError(result), now, ReclaimReasonResult, true); aerr != nil && !errors.Is(aerr, ErrNotFound) {
		return fmt.Errorf("complete task %s: %w", taskID, aerr)
	}
	if _, rerr := releaseSlotBindingOn(ctx, ex, auth.NodeID, instanceID, taskID, attempt, now); rerr != nil {
		return fmt.Errorf("complete task %s: %w", taskID, rerr)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("complete task %s: commit: %w", taskID, err)
	}
	return nil
}

// AckCancelAuthorized 处理 `cancel.ack`：**当前认证身份 + 精确历史绑定**。
//
// 它取代了旧的公开 AckCancel（只带 task/attempt/reaped，没有节点身份与历史
// lease/slot，无法证明「这是哪一次执行的哪个物理槽」）。语义分两种：
//
//   - **当前 attempt + 取消在飞**：processGroupReaped=true 时在**同一事务**里把任务
//     原子终结（cancelled/abandoned，按取消原因）并回收它自己绑定的槽；false 只表示
//     「收到了」——不改相位、不释放槽、不清租约（等下一次可信回收）。
//   - **历史 attempt**（任务已重排/已进入新 attempt）：旧授权不需要仍然 active，
//     但必须证明身份 + 历史 node/task/attempt/lease/slot 全部相符；只回收那个旧槽，
//     **不**碰新 attempt 的相位/token/槽，也不恢复任何旧 result/emit。
//
// 绑定不符（错 lease/slot/instance/节点）一律 fail closed（ErrLeaseMismatch /
// ErrNodeIdentityMismatch），不 no-op 假成功。
//
// 幂等是**逐字节**的：一次 attempt 只被回收一次（reaped_at IS NULL CAS），重复 ACK
// 不重写 ReapedAt/reclaim_reason、也不重写历史行的 phase/error/finished_at，更不碰
// 任务的当前相位/新 attempt 的槽。历史行的失败原因/结果事实是只读的。
func (d *DB) AckCancelAuthorized(ctx context.Context, auth NodeAuthorization,
	in ReapedInputs, processGroupReaped bool) error {
	if in.TaskID == "" || in.Attempt <= 0 {
		return fmt.Errorf("ack cancel: taskID/attempt required")
	}
	if err := auth.validate(); err != nil {
		return err
	}
	if in.NodeID != "" && in.NodeID != auth.NodeID {
		return fmt.Errorf("%w: ack 的历史节点 %s 不是当前认证节点 %s",
			ErrNodeIdentityMismatch, in.NodeID, auth.NodeID)
	}
	if in.NodeTokenHash != "" && in.NodeTokenHash != auth.NodeTokenHash {
		return fmt.Errorf("%w: ack 的历史令牌摘要与当前认证身份不一致", ErrNodeIdentityMismatch)
	}
	if in.LeaseID == "" || in.Slot < 0 {
		return fmt.Errorf("%w: ack 必须带该 attempt 的历史 leaseId 与 slot", ErrLeaseMismatch)
	}
	now := in.Now
	if now == 0 {
		now = Now()
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ack cancel %s: begin: %w", in.TaskID, err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	if _, err := authorizeNodeOn(ctx, ex, auth); err != nil {
		return err
	}

	// 历史绑定的精确核对（node/lease/slot/instance）：缺一样都不能证明回收对象。
	row, err := getTaskAttemptOn(ctx, ex, in.TaskID, in.Attempt)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: attempt %s/%d 不存在", ErrLeaseMismatch, in.TaskID, in.Attempt)
	}
	if err != nil {
		return err
	}
	if row.NodeID != auth.NodeID {
		return fmt.Errorf("%w: attempt %s/%d 跑在节点 %s，不是 %s",
			ErrLeaseMismatch, in.TaskID, in.Attempt, row.NodeID, auth.NodeID)
	}
	if row.LeaseID != in.LeaseID {
		return fmt.Errorf("%w: attempt %s/%d 的 lease %s != %s",
			ErrLeaseMismatch, in.TaskID, in.Attempt, row.LeaseID, in.LeaseID)
	}
	if row.Slot != in.Slot {
		return fmt.Errorf("%w: attempt %s/%d 的 slot %d != %d",
			ErrLeaseMismatch, in.TaskID, in.Attempt, row.Slot, in.Slot)
	}
	if in.InstanceID != "" && row.InstanceID != in.InstanceID {
		return fmt.Errorf("%w: attempt %s/%d 的实例 %s != %s",
			ErrLeaseMismatch, in.TaskID, in.Attempt, row.InstanceID, in.InstanceID)
	}

	var (
		phase        string
		rowAttempt   int
		cancelReason sql.NullString
		cancelReq    sql.NullInt64
		stopIntent   string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT phase, attempt, cancel_reason, cancel_requested_at, stop_intent FROM tasks WHERE id = ?`, in.TaskID).
		Scan(&phase, &rowAttempt, &cancelReason, &cancelReq, &stopIntent)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("ack cancel %s: read: %w", in.TaskID, err)
	}

	currentAttempt := rowAttempt == in.Attempt
	if currentAttempt && phase == TaskRunning {
		if !cancelReq.Valid {
			// 当前执行没有取消意图：ACK 不是「回收一个已取消的执行」，fail closed。
			return fmt.Errorf("%w: task %s is running without cancel intent", ErrInvalidTransition, in.TaskID)
		}
		if !processGroupReaped {
			// 收到了但还没回收完：不改相位、不释放槽位、也不清租约（等可信回收事实）。
			return nil
		}
		reason := cancelReason.String
		if reason == "" {
			reason = protocol.ReasonAbandonedByUser
		}
		finalPhase := cancelPhase(stopIntent, reason)
		ok, ferr := d.finishTxGuarded(ctx, tx, in.TaskID, TaskRunning, finalPhase, "", nil, now)
		if ferr != nil {
			return ferr
		}
		if !ok {
			return fmt.Errorf("%w: task %s changed phase concurrently", ErrInvalidTransition, in.TaskID)
		}
		if _, werr := tx.ExecContext(ctx,
			`UPDATE tasks SET error = ? WHERE id = ?`, reason, in.TaskID); werr != nil {
			return fmt.Errorf("ack cancel %s: write reason: %w", in.TaskID, werr)
		}
		if rerr := reapAttemptForTaskTx(ctx, tx, in.TaskID, in.Attempt, finalPhase, reason,
			row.NodeID, row.InstanceID, now, ReclaimReasonCancelAck); rerr != nil {
			return fmt.Errorf("ack cancel %s: %w", in.TaskID, rerr)
		}
		if cerr := tx.Commit(); cerr != nil {
			return fmt.Errorf("ack cancel %s: commit: %w", in.TaskID, cerr)
		}
		return nil
	}

	// 其余分支都只处理「回收事实」：终态/已重排/历史 attempt 都不得改新相位或 token。
	if currentAttempt && IsTerminalPhase(phase) && !cancelReq.Valid {
		// 不是取消造成终态（succeeded/failed 后的迟到 ack）：不接。
		return fmt.Errorf("%w: task %s is %s", ErrInvalidTransition, in.TaskID, phase)
	}
	if !processGroupReaped {
		// 明确的「还没回收」：保持一切现状（槽位/相位/租约都不动）。
		return nil
	}
	// 历史/已重排/已终结：只按**精确历史绑定**写 reaped 事实并释放它自己的槽。
	// 这里不能用 finishAttemptOn：它会把历史行上的失败事实（phase/error/finished_at）
	// 重写成空 error。reapAttemptOn 的 `reaped_at IS NULL` CAS 保证第一次 ACK 写入
	// 精确的 ReapedAt/reclaim_reason，重复 ACK（哪怕 now/reason 不同）一行都不改。
	if _, rerr := reapAttemptOn(ctx, ex, in.TaskID, in.Attempt, row.NodeID, row.LeaseID,
		row.InstanceID, now, ReclaimReasonCancelAck); rerr != nil {
		return fmt.Errorf("ack cancel %s: %w", in.TaskID, rerr)
	}
	if cerr := tx.Commit(); cerr != nil {
		return fmt.Errorf("ack cancel %s: commit: %w", in.TaskID, cerr)
	}
	return nil
}
