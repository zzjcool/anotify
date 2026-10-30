package store

import (
	"context"
	"database/sql"
	"fmt"
)

// AuthorizeNamespace 是「这个用户能不能碰这个命名空间」的**唯一收口**
// （#204 安全语义 / #205 收口）。
//
// 今天的安全语义（明确写在这里，避免以后误读）：
//   - 一套部署 = 一个团队；
//   - **本部署任何已登录用户，都可以访问任何存在的命名空间**；
//   - 数据隔离只在命名空间这一级（图 / 事件 / 任务 / 源各自独立），
//     组织那一级已经删除（迁移 14）。
//
// 将来若要按命名空间分权限：新增 `namespace_members` 表，**只改这一个函数**
// （以及 CreateNamespace 时的成员写入），其余调用点的形状完全不变 —— 这就是
// 把它做成单一收口的目的。不要在各个 handler 里散着写归属判断。
//
// 返回：
//   - namespaceID 空 / `*`（AllNamespaces）→ ErrNamespaceRequired（HTTP 400）
//   - userID 空或用户不存在 → ErrAuthRequired（HTTP 401）
//   - 命名空间不存在 → ErrNamespaceNotFound（HTTP 404）
//   - 否则 nil
func (d *DB) AuthorizeNamespace(ctx context.Context, userID, namespaceID string) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	if userID == "" {
		return ErrAuthRequired
	}
	// 一次往返判两件事：用户存在吗、命名空间存在吗。
	// 用 EXISTS 而不是 count(*)：只关心有没有，且两者都是主键查找。
	var nsExists, userExists int
	err := d.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM namespaces WHERE id = ?),
		        EXISTS(SELECT 1 FROM users WHERE id = ?)`,
		namespaceID, userID).Scan(&nsExists, &userExists)
	if err != nil {
		return fmt.Errorf("authorize namespace %s for user %s: %w", namespaceID, userID, err)
	}
	if userExists == 0 {
		return ErrAuthRequired
	}
	if nsExists == 0 {
		return ErrNamespaceNotFound
	}
	return nil
}

// AuthorizeNamespaceTx 是 AuthorizeNamespace 的事务版（写路径在事务内收口用）。
func AuthorizeNamespaceTx(ctx context.Context, tx *sql.Tx, userID, namespaceID string) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	if userID == "" {
		return ErrAuthRequired
	}
	var nsExists, userExists int
	err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM namespaces WHERE id = ?),
		        EXISTS(SELECT 1 FROM users WHERE id = ?)`,
		namespaceID, userID).Scan(&nsExists, &userExists)
	if err != nil {
		return fmt.Errorf("authorize namespace %s for user %s: %w", namespaceID, userID, err)
	}
	if userExists == 0 {
		return ErrAuthRequired
	}
	if nsExists == 0 {
		return ErrNamespaceNotFound
	}
	return nil
}
