package store

import (
	"context"
	"fmt"
)

// RoleAuto 是 RegisterUser 的**入参专用**值，永不落库：它要求本函数在自己那个
// 事务里数一次 users，据此决定角色 —— 0 个用户 ⇒ RoleAdmin（首个注册用户是超管，
// 契约 §2.1），否则 RoleMember。
//
// 为什么必须在**同一个事务**里判：调用方「先查 count、再 INSERT」是两次独立的
// 连接池操作。连接池放开到 8（store.go defaultMaxOpenConns，#240）之后，两个并发
// 首注册可以先后读到 count=0，于是**双双拿到 admin**（凭空多出一个超管账户）。
// 移进本函数后，count 与 INSERT 共享同一个 BEGIN IMMEDIATE 事务（写锁在事务起点
// 就持有），后一个注册者的 BeginTx 会等到前一个提交后才开始 —— 它必然读到已提交
// 的那一行，因此只可能有一个 admin。
//
// 注意：本值是 role 字段的第三种取值，**不能**被其它写用户路径（CreateUser /
// UpdateUserRole）接受；它们仍只认 admin|member。
const RoleAuto = "auto"

// 注册（#204 A1 / #205）：建用户 + 建 `default` 命名空间，**在一个事务内**。
//
// 为什么必须原子：代码里不存在「没有命名空间」的分支。用户建好了却没有命名空间，
// 后续所有资源端点无处安放 —— 只能靠"稍后补"，而"稍后补"在失败时没人会发现。
//
// 与旧 RegisterUserWithOrg 的差别（组织层已删之后）：
//   - 不再建组织、不再写 org_members（这两张表已经删了）；
//   - 「首个注册用户是超管」的判定也在本函数内：调用方传 RoleAuto，本函数在
//     建用户的**同一个事务**里数用户数并定角色（见 RoleAuto）。让调用方先查后写
//     会与 INSERT 拆成两次池操作，池放开后就不再串行（#240 → 双 admin 竞态）。
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
	switch role {
	case "":
		role = RoleMember
	case RoleAuto:
		// 首用户判定与 INSERT 同事务：读必须走 tx（同快照），不能走池（见 RoleAuto）。
		var n int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
			return nil, fmt.Errorf("count users for auto role: %w", err)
		}
		if n == 0 {
			role = RoleAdmin
		} else {
			role = RoleMember
		}
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
