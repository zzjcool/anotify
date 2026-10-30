package store

// AuthorizeNamespace 是 #204 / #205 的**唯一收口**：谁能碰哪个命名空间。
//
// 今天的安全语义（明确写死在这里，避免以后误读）：
// 一套部署 = 一个团队，**本部署任何已登录用户都可以访问任何存在的命名空间**。
// 将来按命名空间分权限时只改 AuthorizeNamespace 并新增 namespace_members ——
// 所以这四条分支必须钉住，改一处就能被这些用例发现。

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestAuthorizeNamespace(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, userID := newTestNS(t, db)
	_, otherUserID := newTestNS(t, db)

	// ① 本部署的任意已登录用户 + 存在的命名空间 → 通过（命名空间是唯一隔离边界，
	//    用户之间不再有组织归属关系）
	if err := db.AuthorizeNamespace(ctx, userID, nsID); err != nil {
		t.Fatalf("AuthorizeNamespace(own) err = %v, want nil", err)
	}
	if err := db.AuthorizeNamespace(ctx, otherUserID, nsID); err != nil {
		t.Fatalf("AuthorizeNamespace(别的用户,同一部署) err = %v, want nil（本部署内共享）", err)
	}

	// ② 没带命名空间 / "*"（跨命名空间汇总标记）→ ErrNamespaceRequired（HTTP 400）
	for _, ns := range []string{"", AllNamespaces} {
		if err := db.AuthorizeNamespace(ctx, userID, ns); !errors.Is(err, ErrNamespaceRequired) {
			t.Errorf("AuthorizeNamespace(ns=%q) err = %v, want ErrNamespaceRequired", ns, err)
		}
	}

	// ③ 没有已登录用户（空串 / 用户不存在）→ ErrAuthRequired（HTTP 401）
	if err := db.AuthorizeNamespace(ctx, "", nsID); !errors.Is(err, ErrAuthRequired) {
		t.Errorf("AuthorizeNamespace(anonymous) err = %v, want ErrAuthRequired", err)
	}
	if err := db.AuthorizeNamespace(ctx, "usr_does_not_exist", nsID); !errors.Is(err, ErrAuthRequired) {
		t.Errorf("AuthorizeNamespace(unknown user) err = %v, want ErrAuthRequired", err)
	}

	// ④ 命名空间不存在 → ErrNamespaceNotFound（HTTP 404）
	if err := db.AuthorizeNamespace(ctx, userID, "ns_does_not_exist"); !errors.Is(err, ErrNamespaceNotFound) {
		t.Errorf("AuthorizeNamespace(unknown ns) err = %v, want ErrNamespaceNotFound", err)
	}

	// 事务版必须与 DB 版行为一致（写路径用它收口）。
	//
	// 注意：DSN 是 MaxOpenConns(1)，事务会独占唯一连接 —— 所以这里的断言必须
	// 在同一个 block 里做完并立刻回滚，之后再走任何 db.* 调用。
	if err := func() error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := AuthorizeNamespaceTx(ctx, tx, userID, nsID); err != nil {
			return fmt.Errorf("AuthorizeNamespaceTx(ok) err = %v, want nil", err)
		}
		if err := AuthorizeNamespaceTx(ctx, tx, "usr_does_not_exist", nsID); !errors.Is(err, ErrAuthRequired) {
			return fmt.Errorf("AuthorizeNamespaceTx(unknown user) err = %v, want ErrAuthRequired", err)
		}
		if err := AuthorizeNamespaceTx(ctx, tx, userID, "ns_does_not_exist"); !errors.Is(err, ErrNamespaceNotFound) {
			return fmt.Errorf("AuthorizeNamespaceTx(unknown ns) err = %v, want ErrNamespaceNotFound", err)
		}
		return nil
	}(); err != nil {
		t.Error(err)
	}

	// 命名空间被删掉之后立刻失去访问（判定不缓存）
	extra := newTestExtraNS(t, db, "temp-ns")
	if err := db.AuthorizeNamespace(ctx, userID, extra); err != nil {
		t.Fatalf("AuthorizeNamespace(new ns) err = %v, want nil", err)
	}
	if err := db.DeleteNamespace(ctx, extra, true); err != nil {
		t.Fatalf("delete namespace: %v", err)
	}
	if err := db.AuthorizeNamespace(ctx, userID, extra); !errors.Is(err, ErrNamespaceNotFound) {
		t.Errorf("删掉后 err = %v, want ErrNamespaceNotFound", err)
	}
}
