package auth

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// newTestServiceFile 用**文件**库构造 Service（连接池是真实的多条，#240 默认 8）。
//
// 为什么不能沿用 auth_test.go 的 newTestDB(":memory:")：store.openWithOptions 对
// 精确字符串 ":memory:" 强制把池压到 1（内存库是每连接一份，池 >1 会看到空库）。
// 池为 1 时所有池操作都被串行化，本文件的竞态用例会**永远绿**——它测的不是修复，
// 是内存库的副作用。所以并发用例必须落在文件库上。
func newTestServiceFile(t *testing.T) (*Service, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "auth-race.db"))
	if err != nil {
		t.Fatalf("open file db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc, err := NewService(db, Config{
		RPDisplayName: "Anotify Test",
		RPID:          "localhost",
		RPOrigins:     []string{"http://localhost"},
		SessionTTL:    time.Hour,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc, db
}

// TestRegisterRole_FirstUserAdminSecondMember 是并发用例的确定性底座：
// 顺序注册时角色规则必须是「第一个 admin、第二个 member」。
// 它不依赖调度，所以并发用例偶发飘红时，这一条能区分「角色判定坏了」与「竞态没命中」。
func TestRegisterRole_FirstUserAdminSecondMember(t *testing.T) {
	svc, _ := newTestServiceFile(t)
	ctx := context.Background()

	first, err := svc.ensureRegisteredUser(ctx, &store.User{ID: store.NewUserID(), Username: "alice"})
	if err != nil {
		t.Fatalf("register first user: %v", err)
	}
	if first.Role != store.RoleAdmin {
		t.Fatalf("首个注册用户应为 admin, got %q", first.Role)
	}

	second, err := svc.ensureRegisteredUser(ctx, &store.User{ID: store.NewUserID(), Username: "bob"})
	if err != nil {
		t.Fatalf("register second user: %v", err)
	}
	if second.Role != store.RoleMember {
		t.Fatalf("第二个注册用户应为 member, got %q", second.Role)
	}

	// 复用路径：同一个 ID 再落一次，不会多建用户、不会改角色。
	again, err := svc.ensureRegisteredUser(ctx, &store.User{ID: first.ID, Username: first.Username})
	if err != nil {
		t.Fatalf("re-register existing user: %v", err)
	}
	if again.ID != first.ID || again.Role != store.RoleAdmin {
		t.Fatalf("已存在用户应原样返回: got id=%q role=%q", again.ID, again.Role)
	}
}

// raceRound 跑一轮「全新库 + 两个并发首注册」，返回每个注册者看到的角色与库里的计数。
func raceRound(t *testing.T, round, registers int) (roles []string, userCount, adminCount int64) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "round.db"))
	if err != nil {
		t.Fatalf("round %d: open file db: %v", round, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc, err := NewService(db, Config{
		RPDisplayName: "Anotify Test",
		RPID:          "localhost",
		RPOrigins:     []string{"http://localhost"},
		SessionTTL:    time.Hour,
	})
	if err != nil {
		t.Fatalf("round %d: new service: %v", round, err)
	}

	var (
		start = make(chan struct{}) // 起跑门：所有 goroutine 就绪后一起冲进判定点
		ready sync.WaitGroup
		done  sync.WaitGroup
		errs  = make([]error, registers)
	)
	roles = make([]string, registers)
	ready.Add(registers)
	done.Add(registers)
	for i := range registers {
		go func() {
			defer done.Done()
			u := &store.User{
				ID:       store.NewUserID(),
				Username: fmt.Sprintf("racer-%02d-%d", round, i),
			}
			ready.Done()
			<-start
			got, err := svc.ensureRegisteredUser(context.Background(), u)
			if err != nil {
				errs[i] = err
				return
			}
			roles[i] = got.Role
		}()
	}
	ready.Wait() // 全部就绪
	close(start) // 一起冲
	done.Wait()  // 全部落库

	for i, err := range errs {
		if err != nil {
			t.Fatalf("round %d: register %d failed: %v", round, i, err)
		}
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&userCount); err != nil {
		t.Fatalf("round %d: count users: %v", round, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = ?`, store.RoleAdmin).Scan(&adminCount); err != nil {
		t.Fatalf("round %d: count admins: %v", round, err)
	}
	return roles, userCount, adminCount
}

// TestConcurrentFirstRegister_ExactlyOneAdmin 是并发首注册的回归护栏（#N1）。
//
// 缺陷（修复前）：首用户判定是 s.db.UserCount() 的一次池操作，随后的
// RegisterUser() 是**另一次**池操作，两者之间没有事务。#240 把连接池从 1 放开到 8
// 之后两个并发首注册可以同时读到 count==0，于是**双双拿到 admin** —— 一次多出一个
// 互不知情的超管账户。
//
// 断言：恰好一个 admin（另一个是 member），且库里 admin 总数 = 1、用户数 = 2
// （不能互相覆盖）。必须用文件库：":memory:" 会把池压到 1，测不出这个竞态。
//
// 为什么要跑多轮：缺陷是竞态窗口，单轮命中率不保证 100%；每轮用**全新的库**
// （「首注册」每个库只有一次机会），用轮数换稳定的回归信号。
func TestConcurrentFirstRegister_ExactlyOneAdmin(t *testing.T) {
	const (
		rounds    = 20
		registers = 2
	)

	for round := 0; round < rounds; round++ {
		roles, userCount, adminCount := raceRound(t, round, registers)

		admins := 0
		for i, role := range roles {
			switch role {
			case store.RoleAdmin:
				admins++
			case store.RoleMember:
			default:
				t.Fatalf("round %d: register %d 拿到非法角色 %q", round, i, role)
			}
		}
		if userCount != registers {
			t.Fatalf("round %d: 用户数 = %d, want %d（并发注册丢了行）", round, userCount, registers)
		}
		if admins != 1 || adminCount != 1 {
			t.Fatalf("round %d: 并发首注册产生 admin %d 个（库里 %d 个），应恰好 1 个 —— "+
				"「首个用户」判定必须与 INSERT 在同一事务内完成，否则两个注册者都会看到 count==0",
				round, admins, adminCount)
		}
	}
}
