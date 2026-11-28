package store

import (
	"context"
	"fmt"
)

// RoleAuto 是 RegisterUser 的**入参专用**值，永不落库：0 个用户 ⇒ RoleAdmin（首个注册用户是超管），
// 否则 RoleMember。必须在**同一个事务**里判：调用方「先查 count 再 INSERT」是两次池操作，池放开到 8
// 后两个并发首注册会双双拿到 admin（#240）；移进本函数后 count 与 INSERT 共享 BEGIN IMMEDIATE，
// 后一个 BeginTx 要等前一个提交，必然读到那一行。本值**不能**被其它写用户路径接受（它们只认 admin|member）。
const RoleAuto = "auto"

// 注册（#204 A1 / #205）：建用户 + 建 `default` 命名空间，**在一个事务内**（不存在
// 「没有命名空间」的分支）。首用户判定与注册闸门（#574）也在这个事务里（见 RoleAuto）。
func (d *DB) RegisterUser(ctx context.Context, u *User) (*Namespace, error) {
	// Passkey 注册路径：不写密码列（"" / 0 = 迁移 8 的列默认值）。
	return d.registerUser(ctx, u, "", 0)
}

// RegisterUserWithPassword 与 RegisterUser **完全同语义**（RoleAuto 首用户判定 /
// 默认命名空间），额外在**同一个事务**里写入密码列（契约「追加：用密码注册」§A1）：
// 先提交用户行再 UPDATE 密码会在中途失败时留下「有账号、没有任何登录方式」的半成品。
// passwordHash 必须是 argon2id PHC 串；store 不解析也不校验（只保证存什么读什么）。
func (d *DB) RegisterUserWithPassword(
	ctx context.Context, u *User, passwordHash string, passwordUpdatedAt int64,
) (*Namespace, error) {
	return d.registerUser(ctx, u, passwordHash, passwordUpdatedAt)
}

// registerUser 是两个导出注册入口的共同主体：建用户 + 建 `default` 命名空间，同一个事务里完成。
// v4 不再播种内置能力（链尾就是 next: [] 的普通脚本能力，没有中枢内置执行器）。
func (d *DB) registerUser(ctx context.Context, u *User, passwordHash string, passwordUpdatedAt int64) (*Namespace, error) {
	if u == nil {
		return nil, fmt.Errorf("register user: nil user")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin register user: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 自助注册闸门（#574）：与 INSERT 同事务（理由同 RoleAuto）；计数同时服务两者。
	policy := d.RegistrationPolicy()
	needCount := policy == RegistrationFirstUserOnly || u.Role == RoleAuto
	var userCount int64
	if needCount {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&userCount); err != nil {
			return nil, fmt.Errorf("count users for registration policy: %w", err)
		}
	}
	if policy == RegistrationFirstUserOnly && userCount > 0 {
		// 提交前返回（defer 的 Rollback 收尾）：拒绝路径不写任何行。
		return nil, ErrRegistrationClosed
	}

	role := u.Role
	switch role {
	case "":
		role = RoleMember
	case RoleAuto:
		// 首用户判定与 INSERT 同事务：userCount 就是上面那次 tx 查询的结果。
		if userCount == 0 {
			role = RoleAdmin
		} else {
			role = RoleMember
		}
	}
	disabled := 0
	if u.Disabled {
		disabled = 1
	}
	// INSERT 的列集分两档：
	//   - 密码路径：password_hash / password_updated_at 与用户行同一个 INSERT 写入；
	//   - Passkey 路径：**不点名**这两列，让它们取列默认值（v4 baseline：'' / 0）。
	//
	// 两种写法语义等价：省略列 == 列默认值。
	var ierr error
	if passwordHash == "" && passwordUpdatedAt == 0 {
		_, ierr = tx.ExecContext(ctx,
			`INSERT INTO users (id, username, display_name, role, disabled, created_at) VALUES (?,?,?,?,?,?)`,
			u.ID, u.Username, u.DisplayName, role, disabled, u.CreatedAt)
	} else {
		_, ierr = tx.ExecContext(ctx,
			`INSERT INTO users (id, username, display_name, role, disabled, created_at, password_hash, password_updated_at)
			 VALUES (?,?,?,?,?,?,?,?)`,
			u.ID, u.Username, u.DisplayName, role, disabled, u.CreatedAt, passwordHash, passwordUpdatedAt)
	}
	if ierr != nil {
		return nil, fmt.Errorf("insert user: %w", ierr)
	}

	ns, err := createDefaultNamespaceTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit register user: %w", err)
	}

	u.Role = role
	u.Disabled = disabled != 0
	return ns, nil
}
