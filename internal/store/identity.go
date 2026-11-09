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
	// Passkey 注册路径：不写密码列（"" / 0 = 迁移 8 的列默认值）。
	return d.registerUser(ctx, u, "", 0)
}

// RegisterUserWithPassword 与 RegisterUser **完全同语义**（RoleAuto 首用户判定 /
// 默认命名空间），额外在**同一个事务**里写入密码列。
//
// 为什么必须同事务（契约「追加：用密码注册」§A1）：密码是这条注册路径唯一的登录方式。
// 若先把用户行提交、再 UPDATE 密码，中间失败就留下一个「有账号、却没有任何登录方式」
// 的半成品——它既进不去（没有 Passkey），又会占住用户名。一个事务全成或全不成。
//
// passwordHash 必须是 argon2id PHC 串（由 auth 层用 hashKey 生成）；store 不解析它，
// 也不做校验——这层只保证「存什么读什么」。
func (d *DB) RegisterUserWithPassword(
	ctx context.Context, u *User, passwordHash string, passwordUpdatedAt int64,
) (*Namespace, error) {
	return d.registerUser(ctx, u, passwordHash, passwordUpdatedAt)
}

// registerUser 是两个导出注册入口的共同主体：
// 建用户 + 建 `default` 命名空间，**在同一个事务里**完成。
//
// v4 不再播种任何内置能力：链尾就是 next: [] 的普通脚本能力，没有中枢内置执行器，
// 因此也没有「每个命名空间必须有一份 notify.phone」这种起点。
func (d *DB) registerUser(ctx context.Context, u *User, passwordHash string, passwordUpdatedAt int64) (*Namespace, error) {
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
