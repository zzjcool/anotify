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
	// 语句缓存（#242）：这是每个带命名空间的请求都要打的鉴权收口，SQL 文本恒定。
	var nsExists, userExists int
	err := d.cachedQueryRowContext(ctx,
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

// AuthorizeNamespaceForNode 是 AuthorizeNamespace 的**机器主体**版本（#207 阶段 2）。
//
// 为什么需要单独一个入口：WS 节点握手的主体不是登录用户，而是 opaque 节点令牌。
// 令牌本身已经把命名空间钉死（`nodes.token_hash` → 那一行的 `namespace_id`，
// 见 internal/ws/node_handshake.go），所以「已登录」这一环由令牌证明，
// 这里只需要判断那个命名空间**仍然存在**（命名空间被删后，旧令牌必须立刻失效，
// 而不是继续拿着一个已消失的隔离边界当作用域跑任务）。
//
// 与 AuthorizeNamespace 的关系：两者是同一个安全收口的两条主体路径，
// 判断都收敛在 store 包内，**不要**在 internal/ws 里自己写存在性判断。
// 将来新增 `namespace_members` 时：用户路径改 AuthorizeNamespace（按成员表判），
// 机器路径保持「令牌合法 ⇒ 该命名空间下的机器已获授权」——节点令牌是
// 按命名空间签发的，成员关系改变不追溯已有令牌，这一点必须显式保持。
//
// 返回：namespaceID 空 / `*` → ErrNamespaceRequired；命名空间不存在 → ErrNamespaceNotFound。
func (d *DB) AuthorizeNamespaceForNode(ctx context.Context, namespaceID string) error {
	exists, err := d.namespaceExists(ctx, namespaceID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrNamespaceNotFound
	}
	return nil
}

// namespaceExists 是「这个命名空间存在吗」的**唯一**实现，两条主体路径共用。
// 用 EXISTS 而不是 count(*)：只关心有没有，且 id 是主键查找。
func (d *DB) namespaceExists(ctx context.Context, namespaceID string) (bool, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return false, ErrNamespaceRequired
	}
	var exists int
	if err := d.cachedQueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM namespaces WHERE id = ?)`, namespaceID).
		Scan(&exists); err != nil {
		return false, fmt.Errorf("check namespace %s exists: %w", namespaceID, err)
	}
	return exists != 0, nil
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
