package store

// 【历史 schema 专用】迁移 12（#162，命名空间隔离）用到的辅助函数。
//
// 这些函数描述的是**迁移 12 那个时点**的 schema：orgs / org_members 还在，
// 每张业务表都带 org_id，namespaces 的唯一键是 (org_id, slug)。
//
// 迁移 14（migrateDropOrgLayer）之后这些列与表都不存在了，所以
// **运行时代码不得调用本文件的任何函数** —— 它们只由迁移链
// （migrate.go 的 migrationHooks[12]）在老库上按顺序重放。
// 让它们单独存在一个文件，就是为了让这条边界一眼可见：
// 运行时建命名空间 / 播种内置能力走 namespace.go 与 builtin.go 的 org-free 实现。

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// nsColumnsV12 是迁移 12 时点的命名空间列清单（还带 org_id）。
const nsColumnsV12 = `id, org_id, slug, name, created_at`

// nsRowV12 是迁移 12 时点的一行命名空间。
type nsRowV12 struct {
	ID        string
	OrgID     string
	Slug      string
	Name      string
	CreatedAt int64
}

// ensureDefaultNamespacesTx 给每个还没有命名空间的组织建一个 `default`（迁移 12 用）。
//
// 排序后逐个处理：老数据全部回填到这个默认命名空间，所以它必须是**最早**的那一行
// （`ORDER BY created_at, id LIMIT 1` 的取法见 migrate.go 的 defaultNSExpr）。
func ensureDefaultNamespacesTx(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT id FROM orgs ORDER BY created_at, id`)
	if err != nil {
		return fmt.Errorf("list orgs for default namespace: %w", err)
	}
	var orgIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan org id: %w", err)
		}
		orgIDs = append(orgIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate orgs: %w", err)
	}
	rows.Close()

	for _, orgID := range orgIDs {
		if _, err := ensureDefaultNamespaceTx(tx, orgID); err != nil {
			return err
		}
	}
	return nil
}

// ensureDefaultNamespaceTx 幂等地保证组织有默认命名空间，返回它（迁移 12 用）。
func ensureDefaultNamespaceTx(tx *sql.Tx, orgID string) (*nsRowV12, error) {
	var ns nsRowV12
	err := tx.QueryRow(
		`SELECT `+nsColumnsV12+` FROM namespaces WHERE org_id = ? ORDER BY created_at, id LIMIT 1`, orgID).
		Scan(&ns.ID, &ns.OrgID, &ns.Slug, &ns.Name, &ns.CreatedAt)
	if err == nil {
		return &ns, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("probe default namespace: %w", err)
	}
	ns = nsRowV12{
		ID:        NewNamespaceID(),
		OrgID:     orgID,
		Slug:      DefaultNamespaceSlug,
		Name:      DefaultNamespaceName,
		CreatedAt: Now(),
	}
	if _, err := tx.Exec(
		`INSERT INTO namespaces (id, org_id, slug, name, created_at) VALUES (?,?,?,?,?)`,
		ns.ID, ns.OrgID, ns.Slug, ns.Name, ns.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert default namespace: %w", err)
	}
	return &ns, nil
}

// seedBuiltinsForAllNamespacesTx 给每个命名空间播种内置能力（迁移 12 用）。
//
// 与运行时的 BackfillBuiltinCapabilities 的差别：这里 capability_versions 还有
// org_id 列（NOT NULL），所以插入必须带上它 —— 这条 SQL 只在这个历史时点成立。
func seedBuiltinsForAllNamespacesTx(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT id, org_id FROM namespaces ORDER BY org_id, created_at, id`)
	if err != nil {
		return fmt.Errorf("list namespaces for builtin seeding: %w", err)
	}
	var list []nsRowV12
	for rows.Next() {
		var r nsRowV12
		if err := rows.Scan(&r.ID, &r.OrgID); err != nil {
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
		for _, bc := range BuiltinCapabilities {
			if err := seedOneBuiltinV12Tx(tx, r.ID, r.OrgID, bc); err != nil {
				return err
			}
		}
	}
	return nil
}

// seedOneBuiltinV12Tx 是迁移 12 时点的「播种一个内置能力」（插入带 org_id）。
func seedOneBuiltinV12Tx(tx *sql.Tx, namespaceID, orgID string, bc BuiltinCapability) error {
	var existingID, existingDisplay string
	err := tx.QueryRow(
		`SELECT id, display_name FROM capability_versions
		  WHERE namespace_id = ? AND name = ? AND version = ?`,
		namespaceID, bc.Name, bc.Version).Scan(&existingID, &existingDisplay)
	if err == nil {
		if bc.DisplayName != "" && existingDisplay == "" {
			if _, uerr := tx.Exec(
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
	if _, err := tx.Exec(
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
