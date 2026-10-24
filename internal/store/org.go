package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNotImplemented 表示该 store 方法尚未实现（重构期脚手架，#2 填充）。
var ErrNotImplemented = errors.New("store: not implemented")

// 组织角色（契约 §2.1）。代码里不存在「没有组织」的分支：注册即建组织。
const (
	OrgRoleOwner  = "owner"
	OrgRoleAdmin  = "admin"
	OrgRoleMember = "member"
)

// Org 是 orgs 表的一行。
type Org struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"createdAt"`
}

// OrgMember 是 org_members 表的一行（带用户展示信息）。
type OrgMember struct {
	OrgID    string `json:"orgId"`
	UserID   string `json:"userId"`
	Username string `json:"username"`
	Role     string `json:"role"`
	JoinedAt int64  `json:"joinedAt"`
}

// CreateOrg 创建组织。
func (d *DB) CreateOrg(ctx context.Context, name string) (*Org, error) {
	if name == "" {
		return nil, fmt.Errorf("create org: empty name")
	}
	o := &Org{ID: NewOrgID(), Name: name, CreatedAt: Now()}
	if _, err := d.ExecContext(ctx,
		`INSERT INTO orgs (id, name, created_at) VALUES (?, ?, ?)`,
		o.ID, o.Name, o.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert org: %w", err)
	}
	return o, nil
}

// CreateOrgTx 在调用方事务内创建组织，供注册（用户 + 组织 + 成员必须原子）使用。
func (d *DB) CreateOrgTx(ctx context.Context, tx *sql.Tx, name string) (*Org, error) {
	o := &Org{ID: NewOrgID(), Name: name, CreatedAt: Now()}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO orgs (id, name, created_at) VALUES (?, ?, ?)`,
		o.ID, o.Name, o.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert org: %w", err)
	}
	return o, nil
}

// GetOrg 按 ID 取组织；未命中返回 ErrNotFound。
func (d *DB) GetOrg(ctx context.Context, id string) (*Org, error) {
	var o Org
	err := d.QueryRowContext(ctx,
		`SELECT id, name, created_at FROM orgs WHERE id = ?`, id).
		Scan(&o.ID, &o.Name, &o.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get org: %w", err)
	}
	return &o, nil
}

// AddOrgMember 把用户加入组织（幂等：重复加入更新角色）。
func (d *DB) AddOrgMember(ctx context.Context, orgID, userID, role string) error {
	if role == "" {
		role = OrgRoleMember
	}
	if _, err := d.ExecContext(ctx,
		`INSERT INTO org_members (org_id, user_id, role, joined_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(org_id, user_id) DO UPDATE SET role = excluded.role`,
		orgID, userID, role, Now()); err != nil {
		return fmt.Errorf("add org member: %w", err)
	}
	return nil
}

// AddOrgMemberTx 在调用方事务内加入组织成员。
func (d *DB) AddOrgMemberTx(ctx context.Context, tx *sql.Tx, orgID, userID, role string) error {
	if role == "" {
		role = OrgRoleMember
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO org_members (org_id, user_id, role, joined_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(org_id, user_id) DO UPDATE SET role = excluded.role`,
		orgID, userID, role, Now()); err != nil {
		return fmt.Errorf("add org member: %w", err)
	}
	return nil
}

// IsOrgMember 判定用户是否属于该组织。路由层 foreign_user 剪枝的唯一依据（契约 §10.1.1）。
func (d *DB) IsOrgMember(ctx context.Context, orgID, userID string) (bool, error) {
	var n int
	if err := d.QueryRowContext(ctx,
		`SELECT count(*) FROM org_members WHERE org_id = ? AND user_id = ?`,
		orgID, userID).Scan(&n); err != nil {
		return false, fmt.Errorf("check org member: %w", err)
	}
	return n > 0, nil
}

// OrgRoleOf 返回用户在组织里的角色；不是成员时 ok=false。
func (d *DB) OrgRoleOf(ctx context.Context, orgID, userID string) (role string, ok bool, err error) {
	err = d.QueryRowContext(ctx,
		`SELECT role FROM org_members WHERE org_id = ? AND user_id = ?`,
		orgID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get org role: %w", err)
	}
	return role, true, nil
}

// ListOrgsByUser 列出用户所属的组织（按加入时间升序）。
func (d *DB) ListOrgsByUser(ctx context.Context, userID string) ([]*Org, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT o.id, o.name, o.created_at
		 FROM org_members m JOIN orgs o ON o.id = m.org_id
		 WHERE m.user_id = ?
		 ORDER BY m.joined_at, o.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list orgs by user: %w", err)
	}
	defer rows.Close()
	out := []*Org{}
	for rows.Next() {
		var o Org
		if err := rows.Scan(&o.ID, &o.Name, &o.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan org: %w", err)
		}
		out = append(out, &o)
	}
	return out, rows.Err()
}

// ListOrgMembers 列出组织成员（含用户名，便于前端展示）。
func (d *DB) ListOrgMembers(ctx context.Context, orgID string) ([]*OrgMember, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT m.org_id, m.user_id, COALESCE(u.username,''), m.role, m.joined_at
		 FROM org_members m LEFT JOIN users u ON u.id = m.user_id
		 WHERE m.org_id = ?
		 ORDER BY m.joined_at, m.user_id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list org members: %w", err)
	}
	defer rows.Close()
	out := []*OrgMember{}
	for rows.Next() {
		var m OrgMember
		if err := rows.Scan(&m.OrgID, &m.UserID, &m.Username, &m.Role, &m.JoinedAt); err != nil {
			return nil, fmt.Errorf("scan org member: %w", err)
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

// PrimaryOrgOf 返回用户最早加入的组织 ID（无组织返回 ErrNotFound）。
// 供「代码里不存在没有组织的分支」使用：session/api key 都挂在用户上，
// 需要 org 维度时取主组织。
//
// 排序用 rowid 而不是 joined_at：joined_at 是秒级时间戳，同一秒内加入两个组织时
// 两个值相同，只能靠插入顺序（rowid）才是确定的「最早」。
func (d *DB) PrimaryOrgOf(ctx context.Context, userID string) (string, error) {
	var orgID string
	err := d.QueryRowContext(ctx,
		`SELECT org_id FROM org_members WHERE user_id = ?
		 ORDER BY rowid LIMIT 1`, userID).Scan(&orgID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get primary org: %w", err)
	}
	return orgID, nil
}

// defaultOrgNameFor 用用户名派生组织名（改用户名的场景不存在，名字稳定即可）。
func defaultOrgNameFor(username string) string {
	if username == "" {
		return "org-" + itoa64(Now())
	}
	return username
}

// RegisterUserWithOrg 在**一个事务内**建用户 + 建组织 + 加 owner 成员（契约 §2.1 / A1）。
//
// 「代码里不存在没有组织的分支」靠这里保证：任何建用户的地方都用它，
// 而不是先 CreateUser 再补建组织（中途失败会留下没有组织的用户）。
//
// firstUserAdmin 为 true 时把用户提升为 admin（沿用「首个注册用户是超管」的既有行为）。
func (d *DB) RegisterUserWithOrg(ctx context.Context, u *User, firstUserAdmin bool) (*Org, error) {
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
	org, err := d.CreateOrgTx(ctx, tx, defaultOrgNameFor(u.Username))
	if err != nil {
		// 组织名冲突时退化为带 ID 后缀的名字（同名用户可并存于不同… 实际用户名唯一，
		// 这里只为兜住极端情况，不掩盖其他错误）
		org, err = d.CreateOrgTx(ctx, tx, defaultOrgNameFor(u.Username)+"-"+u.ID)
		if err != nil {
			return nil, err
		}
	}
	if err := d.AddOrgMemberTx(ctx, tx, org.ID, u.ID, OrgRoleOwner); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit register user: %w", err)
	}
	u.Role = role
	u.Disabled = disabled != 0
	return org, nil
}

// EnsureOrgForUser 给存量用户补建组织（幂等）。老库升级后已有用户没有组织，
// 首次登录/访问需要时调用一次。
func (d *DB) EnsureOrgForUser(ctx context.Context, userID, username string) (*Org, error) {
	if orgID, err := d.PrimaryOrgOf(ctx, userID); err == nil {
		return d.GetOrg(ctx, orgID)
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin ensure org: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	org, err := d.CreateOrgTx(ctx, tx, defaultOrgNameFor(username)+"-"+userID)
	if err != nil {
		return nil, err
	}
	if err := d.AddOrgMemberTx(ctx, tx, org.ID, userID, OrgRoleOwner); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit ensure org: %w", err)
	}
	return org, nil
}
