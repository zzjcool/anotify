package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 内置能力清单（契约设计见 design/wiki/08-scope.html）。
//
// 内置能力与自定义能力走同一套表、同一套版本化，唯一区别是 `source` 列。
// 但有一类内置能力**没有节点执行者**：`notify.phone` 这类终端能力是中枢
// 自己的能力——没人会写一个"发推送节点"。所以派发时它们跳过「找节点」，
// 由中枢执行器执行（见 route.Dispatcher 与 internal/executor）。
//
// 为什么放在 store 而不是 route：这是**能力定义数据**，和 params_schema
// 一样属于"什么是一个合法能力"，不是路由算法。
type BuiltinCapability struct {
	Name string
	// DisplayName 是内置能力**预先取好的**显示名（「GitHub webhook」这种）。
	//
	// 用户不需要给内置能力取名 —— 它们随项目发布，名字由我们定。
	// 用户仍然可以改（不满意就改掉），但默认得有个能读的名字，
	// 否则能力页一打开满屏 github@1 / notify.phone@1。
	DisplayName  string
	Version      int
	ParamsSchema string // JSON
	Outputs      []string
	// Central 表示由中枢执行（不找节点）。终端能力为 true。
	Central bool
	// Origin 表示它是**事件起点**：没有节点执行者，也不产生任务，
	// 只提供「事件从这里进来」的命名锚点 + 任意事件类型作为出口。
	//
	// 与 Central 的区别：Central 会派发成中枢执行的任务；Origin 不建任务行，
	// 它是链的 0 号位（trigger 按 source 找到它，再用 type 当出口派发）。
	Origin bool
}

// NotifyPhoneCapability 是 `notify.phone@1` 的定义。
//
// params 就是通知内容本身——**没有模板替换、没有表达式求值**（不变量 I6）：
// 上游事件带什么就推什么。`to` 走 foreign_user 校验（契约 §10.1.1）。
var NotifyPhoneCapability = BuiltinCapability{
	Name:        "notify.phone",
	DisplayName: "推送通知",
	Version:     1,
	ParamsSchema: `{
		"type": "object",
		"required": ["to", "title"],
		"properties": {
			"to": {"type": "string"},
			"title": {"type": "string"},
			"body": {"type": "string"},
			"link": {"type": "string"},
			"agentState": {"type": "string"},
			"deviceTags": {"type": "array", "items": {"type": "string"}}
		}
	}`,
	Outputs: []string{},
	Central: true,
}

// OriginCapability 构造一个内置起点能力。
//
// outputs 保持为空（不是 ["*"]）。原因：
//
//   - `EdgesFrom` 是精确匹配（`WHERE from_port = ?`），通配符在 SQL 里配不上
//     具体事件类型，写 "*" 只会让用户在建边下拉里看到一个用不了的选项；
//   - kind 推导对「无节点声明」的能力直接返回 origin，也不需要非空 outputs。
//
// 通配语义放在**建边校验**侧：内置起点的出口允许任意非空事件类型
// （见 store.CreateEdge）。这样用户建边时填的是真实事件名（github 的
// `pull_request.opened`），派发时也能精确命中。
func OriginCapability(name, displayName string) BuiltinCapability {
	return BuiltinCapability{
		Name:         name,
		DisplayName:  displayName,
		Version:      1,
		ParamsSchema: `{"type": "object"}`,
		Outputs:      []string{},
		Origin:       true,
	}
}

// BuiltinCapabilities 是随项目发布的全部内置能力。
//
// 两类：
//   - 中枢执行（Central）：notify.phone —— 没有节点会去"发推送"
//   - 事件起点（Origin）：github/cron/http/manual —— 没有节点会去"当来源"
//
// 起点做成内置的收益：用户接入一个 GitHub webhook 时，**不需要先伪造一个
// 叫 github、没有出口、没有节点的"能力"**，直接在能力页建边即可。
// 这是把「入口」和「能干活的东西」两个概念分开，而不是绑死。
//
// 自定义来源依然支持：用户可以自己定义一个能力（比如 jira@1），
// source = "jira" 一样能找到它——内置只是把常见来源预先备好。
var BuiltinCapabilities = []BuiltinCapability{
	NotifyPhoneCapability,
	// github/cron/http/manual **不再是能力**（契约 §2.4 / #110）。
	//
	// 它们原来是以「内置起点能力」的形式存在的：不执行代码、不产生任务、
	// 也没地方存配置（cron 表达式之类）。唯一作用是给入口边当左端锚点。
	// 代价是能力列表上多出 4 个用户看得见却用不上的条目。
	//
	// 现在入口边的左端直接是来源名（edges.from_is_source=1），
	// 用户视角的「事件从哪来」完全由 sources 承担。
}

// EnsureBuiltinCapabilities 给一个组织的**所有命名空间**补齐内置能力（幂等）。
//
// 语义在 #162 之后从「一个组织一份」变成「每个命名空间一份」：能力属于命名空间，
// `notify.phone` 在 prod 与 staging 是两行不同的定义（各自 namespace_id）。
//
// 为什么不只在新命名空间播种：内置能力清单会随版本升级增加（比如以后加
// notify.webhook）。那时已有命名空间也需要补上——启动时全量跑一遍比"只在
// 创建时播种"可靠（少一条路径就是一次线上事故）。
func (d *DB) EnsureBuiltinCapabilities(ctx context.Context, orgID string) error {
	if orgID == "" {
		return fmt.Errorf("ensure builtin capabilities: empty org_id")
	}
	namespaces, err := d.ListNamespaces(ctx, orgID)
	if err != nil {
		return err
	}
	for _, ns := range namespaces {
		if err := d.SeedBuiltinCapabilities(ctx, ns.ID, orgID); err != nil {
			return err
		}
	}
	return nil
}

// SeedBuiltinCapabilities 给一个命名空间播种内置能力（自带事务的便捷包装）。
func (d *DB) SeedBuiltinCapabilities(ctx context.Context, namespaceID, orgID string) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin seed builtin capabilities: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := SeedBuiltinCapabilitiesTx(ctx, tx, namespaceID, orgID); err != nil {
		return err
	}
	return tx.Commit()
}

// SeedBuiltinCapabilitiesTx 在调用方事务内给一个命名空间播种全部内置能力。
//
// 两条必须都走这条路：
//  1. 新建命名空间（/ 建组织时的 default）—— 否则新命名空间里连一个
//     end 节点（notify.phone）都声明不出来，推送直接失效；
//  2. 内置能力升版 —— 遍历所有命名空间各插一行（append-only，不改旧行）。
func SeedBuiltinCapabilitiesTx(ctx context.Context, tx *sql.Tx, namespaceID, orgID string) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	for _, bc := range BuiltinCapabilities {
		if err := seedOneBuiltinTx(ctx, tx, namespaceID, orgID, bc); err != nil {
			return err
		}
	}
	return nil
}

// seedOneBuiltinTx 在某个命名空间里播种一个内置能力（已存在则只补显示名）。
//
// 已有行只补**空**的 display_name：预设名是给老库用的（这一列是后加的），
// 而用户改过的名字不能被启动流程改回去。
func seedOneBuiltinTx(ctx context.Context, tx *sql.Tx, namespaceID, orgID string, bc BuiltinCapability) error {
	var existingID, existingDisplay string
	err := tx.QueryRowContext(ctx,
		`SELECT id, display_name FROM capability_versions
		  WHERE namespace_id = ? AND name = ? AND version = ?`,
		namespaceID, bc.Name, bc.Version).Scan(&existingID, &existingDisplay)
	if err == nil {
		if bc.DisplayName != "" && existingDisplay == "" {
			if _, uerr := tx.ExecContext(ctx,
				`UPDATE capability_versions SET display_name = ? WHERE id = ?`,
				bc.DisplayName, existingID); uerr != nil {
				return fmt.Errorf("backfill display name for builtin %s@%d: %w", bc.Name, bc.Version, uerr)
			}
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("probe builtin capability %s@%d: %w", bc.Name, bc.Version, err)
	}

	outputs := bc.Outputs
	if outputs == nil {
		outputs = []string{} // 不变量 I9：空列表是 [] 不是 null
	}
	c := &CapabilityVersion{
		NamespaceID:  namespaceID,
		OrgID:        orgID,
		Name:         bc.Name,
		DisplayName:  bc.DisplayName,
		Version:      bc.Version,
		ParamsSchema: json.RawMessage(bc.ParamsSchema),
		Outputs:      outputs,
		Lifecycle:    Lifecycle{},
		Source:       CapSourceBuiltin,
	}
	hash, err := NormalizeCapability(c)
	if err != nil {
		return fmt.Errorf("seed builtin capability %s@%d: %w", bc.Name, bc.Version, err)
	}
	schema := string(c.ParamsSchema)
	if schema == "" {
		schema = "{}"
	}
	outputsJSON, err := json.Marshal(outputs)
	if err != nil {
		return fmt.Errorf("marshal builtin outputs: %w", err)
	}
	lifecycleJSON, err := json.Marshal(c.Lifecycle)
	if err != nil {
		return fmt.Errorf("marshal builtin lifecycle: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO capability_versions
		   (id, namespace_id, org_id, name, display_name, version, params_schema, outputs,
		    lifecycle, sticky, source, content_hash, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		NewCapabilityID(hash), namespaceID, orgID, c.Name, c.DisplayName, c.Version, schema,
		string(outputsJSON), string(lifecycleJSON), c.Sticky, CapSourceBuiltin, hash, Now()); err != nil {
		return fmt.Errorf("seed builtin capability %s@%d: %w", bc.Name, bc.Version, err)
	}
	return nil
}

// seedBuiltinsForAllNamespacesTx 给每个命名空间播种内置能力（迁移 12 用）。
func seedBuiltinsForAllNamespacesTx(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT id, org_id FROM namespaces ORDER BY org_id, created_at, id`)
	if err != nil {
		return fmt.Errorf("list namespaces for builtin seeding: %w", err)
	}
	type nsRow struct{ id, orgID string }
	var list []nsRow
	for rows.Next() {
		var r nsRow
		if err := rows.Scan(&r.id, &r.orgID); err != nil {
			rows.Close()
			return fmt.Errorf("scan namespace row: %w", err)
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate namespaces: %w", err)
	}
	rows.Close()
	for _, r := range list {
		if err := SeedBuiltinCapabilitiesTx(context.Background(), tx, r.id, r.orgID); err != nil {
			return err
		}
	}
	return nil
}

// IsBuiltinCapability 判断某个 name@version 是不是内置能力（中央执行或起点）。
//
// 派发器用它决定「是否跳过找节点」：内置终端能力没有节点执行者，
// 查 node_capabilities 必然是 0 行，若照常走第 ③ 步就会 pruned/no_node——
// 推送能力永远发不出去。
//
// 注意调用方要**同时**判断 bc.Central：起点能力也是 builtin，但它不该
// 被当成中枢任务来执行（它根本不产生任务行）。
func IsBuiltinCapability(name string, version int) (BuiltinCapability, bool) {
	for _, bc := range BuiltinCapabilities {
		if bc.Name == name && bc.Version == version {
			return bc, true
		}
	}
	return BuiltinCapability{}, false
}

// IsBuiltinCapabilityName 按名字判断是不是内置能力（不看版本）。
//
// 用于「名字是否保留」的判定：内置起点的名字不能被用户开新版本接管，
// 否则 findOrigin 取最高版本会让 source 解析到自定义版本上，
// 而自定义版本不享受起点的任意出口——失败得极其隐蔽。
func IsBuiltinCapabilityName(name string) (BuiltinCapability, bool) {
	for _, bc := range BuiltinCapabilities {
		if bc.Name == name {
			return bc, true
		}
	}
	return BuiltinCapability{}, false
}

// CentralCapabilityNames 返回中枢执行的能力名（派发器与执行器共用）。
func (d *DB) CentralCapabilityNames() []string {
	var out []string
	for _, bc := range BuiltinCapabilities {
		if bc.Central {
			out = append(out, bc.Name)
		}
	}
	return out
}

// ClaimCentralTasks 领取中枢执行的 pending 任务，并推进到 running。
//
// 为什么跳过 assigned/acked：那两个状态的含义是「已派给节点，等它接单/开始」。
// 中枢执行没有节点参与，走那两个状态会让状态机语义失真（也会让租约巡检
// 去扫它们——而中枢任务没有租约，扫不到就会一直挂着）。
//
// 并发安全：用 transitionGuarded 的 CAS（status=pending 作为守卫），
// 多实例部署时只有一个能领到同一条任务。
func (d *DB) ClaimCentralTasks(ctx context.Context, limit int) ([]*Task, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	names := d.CentralCapabilityNames()
	if len(names) == 0 {
		return []*Task{}, nil
	}

	// 先查候选（pending + 中枢能力名）
	placeholders := make([]string, len(names))
	args := make([]any, 0, len(names)+limit)
	for i, n := range names {
		placeholders[i] = "?"
		args = append(args, n)
	}
	q := `SELECT ` + taskColumns + ` FROM tasks
	      WHERE status = ? AND node_id IS NULL AND capability IN (` +
		strings.Join(placeholders, ",") + `)
	      ORDER BY created_at, id LIMIT ?`
	queryArgs := append([]any{TaskPending}, args...)
	queryArgs = append(queryArgs, limit)

	rows, err := d.QueryContext(ctx, q, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("query central tasks: %w", err)
	}
	candidates := []*Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate central tasks: %w", err)
	}
	rows.Close()

	// 逐条 CAS 推进到 running（pending → running 不在合法迁移表里，
	// 所以走 pending → assigned → running：assigned 在这里只表示"被中枢认领"）。
	claimed := make([]*Task, 0, len(candidates))
	for _, t := range candidates {
		// 作用域是行自己的 namespace_id（#162：任务的隔离维度是命名空间，
		// 中枢任务也属于某个命名空间）。
		if err := d.transitionGuarded(ctx, nil, t.NamespaceID, t.ID, TaskPending, TaskAssigned, ""); err != nil {
			if errors.Is(err, ErrTransitionGuardFailed) || errors.Is(err, ErrInvalidTransition) {
				continue // 别人先领了
			}
			return claimed, fmt.Errorf("claim central task %s: %w", t.ID, err)
		}
		if err := d.transitionGuarded(ctx, nil, t.NamespaceID, t.ID, TaskAssigned, TaskRunning, ""); err != nil {
			return claimed, fmt.Errorf("start central task %s: %w", t.ID, err)
		}
		if _, err := d.ExecContext(ctx,
			`UPDATE tasks SET started_at = COALESCE(started_at, ?) WHERE namespace_id = ? AND id = ?`,
			Now(), t.NamespaceID, t.ID); err != nil {
			return claimed, fmt.Errorf("stamp central task start: %w", err)
		}
		t.Status = TaskRunning
		claimed = append(claimed, t)
	}
	return claimed, nil
}

// FinishTask 把 running 的中枢任务推进到终态并写回报。
//
// 与 ReportResult 的区别：ReportResult 是**节点**回报路径（要求任务有
// node_id 归属、会校验租约）。中枢任务没有归属，所以走这条。
func (d *DB) FinishTask(ctx context.Context, namespaceID, taskID, status string, out *string, reported bool, summary, errMsg string) error {
	if status != TaskSucceeded && status != TaskFailed {
		return fmt.Errorf("%w: finish status must be succeeded|failed, got %q", ErrInvalidTransition, status)
	}
	if out != nil && *out == "" {
		return fmt.Errorf("finish task: empty out string is not a valid port (use nil)")
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin finish task: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// running → succeeded|failed（走唯一写入口，守卫 status=running）
	if err := d.transitionGuarded(ctx, tx, namespaceID, taskID, TaskRunning, status, ""); err != nil {
		return fmt.Errorf("finish central task %s: %w", taskID, err)
	}

	rep := 0
	if reported {
		rep = 1
	}
	var outVal any
	if out != nil {
		outVal = *out
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET out = ?, reported = ?, summary = ?, error = ? WHERE namespace_id = ? AND id = ?`,
		outVal, rep, summary, errMsg, namespaceID, taskID); err != nil {
		return fmt.Errorf("write central task result: %w", err)
	}
	return tx.Commit()
}
