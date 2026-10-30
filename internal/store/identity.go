package store

import (
	"context"
	"fmt"
)

// 注册（#204 A1 / #205）：建用户 + 建 `default` 命名空间，**在一个事务内**。
//
// 为什么必须原子：代码里不存在「没有命名空间」的分支。用户建好了却没有命名空间，
// 后续所有资源端点无处安放 —— 只能靠"稍后补"，而"稍后补"在失败时没人会发现。
//
// 与旧 RegisterUserWithOrg 的差别（迁移 14 之后）：
//   - 不再建组织、不再写 org_members（这两张表已经删了）；
//   - 「首个注册用户是超管」的判定仍在调用方（auth 包查 users 计数后填 u.Role），
//     本函数只管「用户 + 默认命名空间 + 内置能力」这一组的原子性。
func (d *DB) RegisterUser(ctx context.Context, u *User) (*Namespace, error) {
	if u == nil {
		return nil, fmt.Errorf("register user: nil user")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin register user: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	role := u.Role
	if role == "" {
		role = RoleMember
	}
	disabled := 0
	if u.Disabled {
		disabled = 1
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO users (id, username, display_name, role, disabled, created_at) VALUES (?,?,?,?,?,?)`,
		u.ID, u.Username, u.DisplayName, role, disabled, u.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}

	ns, err := createDefaultNamespaceTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	// 内置能力与命名空间同事务：否则新命名空间里连 notify.phone 都没有（#162）。
	if err := SeedBuiltinCapabilitiesTx(ctx, tx, ns.ID); err != nil {
		return nil, fmt.Errorf("seed builtin capabilities for namespace %s: %w", ns.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit register user: %w", err)
	}

	u.Role = role
	u.Disabled = disabled != 0
	return ns, nil
}
