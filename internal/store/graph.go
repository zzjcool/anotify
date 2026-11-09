package store

import (
	"context"
	"fmt"
	"strings"
)

// 边是**派生投影**（capability.md §1/§8.1）：
//
//	能力边    来自 spec.next         from_is_source=0, from_ref=能力名
//	入口边    来自源 spec.targets    from_is_source=1, from_ref=源名
//
// 只有四列身份 + created_at：没有端口、没有映射、没有字面量、没有优先级。
// 用户不可直接写边，也没有写 Edge 的 API；写能力 / 写源时在**同一事务**内整体替换。
// 「一次运行只有一个结果」= 一次 emit 只对一个下游负责（扇出 = 多次 emit），
// 所以这里没有候选排序：一个 to_ref 就是一条边。

// Edge 是 edges 表的一行（派生投影，不是用户实体）。
type Edge struct {
	// NamespaceID 是隔离维度：边属于某个命名空间。
	NamespaceID string `json:"namespaceId"`
	// FromIsSource 决定 FromRef 怎么解释：false = 能力名，true = 源名。
	FromIsSource bool `json:"fromIsSource"`
	// FromRef 是上游引用：能力边存能力名（`review`），入口边存源名。
	FromRef string `json:"fromRef"`
	// ToRef 是下游引用：能力边原样保存 next 的写法（`fixer` / `notify@2`），
	// 入口边原样保存 targets[].capability。解析（裸名 = latest）在读取方（route）做。
	ToRef     string `json:"toRef"`
	CreatedAt int64  `json:"createdAt"`
}

// EdgeTarget 是「这一跳通向哪里」的最小投影（EdgesFrom 的一行）。
//
// 保留独立类型是为了让派发路径只依赖 ToRef：边上的 created_at 与方向不参与路由。
type EdgeTarget struct {
	ToRef string `json:"toRef"`
}

// EdgesFrom 返回 (namespace, fromIsSource, fromRef) 的全部下游引用（按 to_ref 稳定排序）。
//
// 能力边：fromRef 是能力名，匹配 `from_ref = name` 的出边（`next` 的 to_ref 原样返回，
// `name` / `name@version` 两种写法都由调用方用 ResolveRef 解析）。
// 入口边：fromRef 是源名，返回该源的 targets 投影。
func (d *DB) EdgesFrom(ctx context.Context, namespaceID string, fromIsSource bool, fromRef string) ([]EdgeTarget, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	fromRef = strings.TrimSpace(fromRef)
	if fromRef == "" {
		return nil, fmt.Errorf("%w: empty from ref", ErrRefUnresolved)
	}
	rows, err := d.QueryContext(ctx,
		`SELECT to_ref FROM edges
		  WHERE namespace_id = ? AND from_is_source = ? AND from_ref = ?
		  ORDER BY to_ref`, namespaceID, boolToInt(fromIsSource), fromRef)
	if err != nil {
		return nil, fmt.Errorf("edges from: %w", err)
	}
	defer rows.Close()
	out := []EdgeTarget{}
	for rows.Next() {
		var t EdgeTarget
		if err := rows.Scan(&t.ToRef); err != nil {
			return nil, fmt.Errorf("scan edge target: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListEdges 列出一个命名空间的全部边（图渲染 / 影响面对账用；空列表返回 []）。
func (d *DB) ListEdges(ctx context.Context, namespaceID string) ([]Edge, error) {
	return listEdgesOn(ctx, d, namespaceID)
}

// listEdgesOn 是 ListEdges 的 ex 版（事务内的读必须看到本事务刚写的行）。
func listEdgesOn(ctx context.Context, ex queryExec, namespaceID string) ([]Edge, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := ex.queryContext(ctx,
		`SELECT namespace_id, from_is_source, from_ref, to_ref, created_at
		   FROM edges WHERE namespace_id = ?
		  ORDER BY from_is_source, from_ref, to_ref`, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("list edges: %w", err)
	}
	defer rows.Close()
	out := []Edge{}
	for rows.Next() {
		var e Edge
		var isSrc int
		if err := rows.Scan(&e.NamespaceID, &isSrc, &e.FromRef, &e.ToRef, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan edge: %w", err)
		}
		e.FromIsSource = isSrc == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

// ReplaceCapabilityEdges 按能力 spec.next 整体替换该能力的出边（同一事务）。
//
// 派生顺序不落库（edges 只有四列身份）：读取方按 to_ref 稳定排序。
// 这是 G1-4 写路径可直接调用的入口；能力写入路径（capability.go）在事务内
// 复用同一个私有实现（rebuildCapabilityEdgesOn），保证「投影只有一份 SQL」。
func (d *DB) ReplaceCapabilityEdges(ctx context.Context, c *CapabilityVersion) error {
	if c == nil {
		return fmt.Errorf("replace capability edges: nil capability")
	}
	if c.NamespaceID == "" || c.NamespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("%w: empty capability name", ErrRefUnresolved)
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace capability edges: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := rebuildCapabilityEdgesOn(ctx, txExec{tx: tx}, c.NamespaceID, c.Name, c.Spec.Next); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replace capability edges: commit: %w", err)
	}
	return nil
}

// ReplaceSourceEdges 按源 targets 整体替换入口边（同一事务）。
//
// from_is_source=1、from_ref=源名；to_ref 原样保存 targets 里的目标引用
// （G1-4 的 source 写路径保证它是 `name` / `name@version`）。派生顺序不落库。
func (d *DB) ReplaceSourceEdges(ctx context.Context, s *Source) error {
	if s == nil {
		return fmt.Errorf("replace source edges: nil source")
	}
	if s.NamespaceID == "" || s.NamespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("replace source edges: empty source name")
	}
	refs := make([]string, 0, len(s.Targets))
	for _, t := range s.Targets {
		ref := strings.TrimSpace(t.CapabilityID)
		if ref == "" {
			continue
		}
		refs = append(refs, ref)
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace source edges: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := replaceSourceEdgesOn(ctx, txExec{tx: tx}, s.NamespaceID, s.Name, refs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replace source edges: commit: %w", err)
	}
	return nil
}

// replaceSourceEdgesOn 整体替换一个源的入口边（事务内外共用一份 SQL）。
func replaceSourceEdgesOn(ctx context.Context, ex queryExec, namespaceID, sourceName string, refs []string) error {
	if _, err := ex.execContext(ctx,
		`DELETE FROM edges WHERE namespace_id = ? AND from_is_source = 1 AND from_ref = ?`,
		namespaceID, sourceName); err != nil {
		return fmt.Errorf("clear entry edges: %w", err)
	}
	now := Now()
	for _, raw := range refs {
		ref := strings.TrimSpace(raw)
		if ref == "" {
			continue
		}
		if _, err := ex.execContext(ctx,
			`INSERT INTO edges (namespace_id, from_is_source, from_ref, to_ref, created_at)
			 VALUES (?,1,?,?,?)
			 ON CONFLICT(namespace_id, from_is_source, from_ref, to_ref) DO NOTHING`,
			namespaceID, sourceName, ref, now); err != nil {
			return fmt.Errorf("insert entry edge %s -> %s: %w", sourceName, ref, err)
		}
	}
	return nil
}

// boolToInt 把 bool 存成 SQLite 的 0/1（没有原生 bool 类型）。
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
