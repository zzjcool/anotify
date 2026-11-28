package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func openRegistrationTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := OpenWithConns(filepath.Join(t.TempDir(), "policy.db"), 8)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func testUser(username string) *User {
	return &User{ID: NewUserID(), Username: username, DisplayName: username, Role: RoleAuto, CreatedAt: Now()}
}

func TestRegistrationPolicy_DefaultIsOpen(t *testing.T) {
	db := openRegistrationTestDB(t)
	ctx := context.Background()
	if got := db.RegistrationPolicy(); got != RegistrationOpen {
		t.Fatalf("零值策略 = %v, want open", got)
	}
	for _, name := range []string{"u1", "u2"} {
		if _, err := db.RegisterUser(ctx, testUser(name)); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
}

func TestRegistrationPolicy_FirstUserOnly_ClosesAfterFirstUser(t *testing.T) {
	db := openRegistrationTestDB(t)
	ctx := context.Background()
	db.SetRegistrationPolicy(RegistrationFirstUserOnly)

	first := testUser("first")
	if _, err := db.RegisterUser(ctx, first); err != nil {
		t.Fatalf("首个用户应可注册: %v", err)
	}
	if first.Role != RoleAdmin {
		t.Fatalf("首个用户角色 = %q, want admin", first.Role)
	}

	if _, err := db.RegisterUser(ctx, testUser("second-passkey")); !errors.Is(err, ErrRegistrationClosed) {
		t.Fatalf("Passkey 注册 err = %v, want ErrRegistrationClosed", err)
	}
	hash := "$argon2id$v=19$m=65536,t=3,p=1$c2FsdA$aGFzaA" // store 不解析它，只求「存什么读什么」
	if _, err := db.RegisterUserWithPassword(ctx, testUser("second-password"), hash, Now()); !errors.Is(err, ErrRegistrationClosed) {
		t.Fatalf("密码注册 err = %v, want ErrRegistrationClosed", err)
	}

	n, err := db.UserCount(ctx)
	if err != nil {
		t.Fatalf("user count: %v", err)
	}
	if n != 1 {
		t.Fatalf("拒绝后用户数 = %d, want 1（闸门不得落任何行）", n)
	}
	for _, name := range []string{"second-passkey", "second-password"} {
		if _, err := db.GetUserByUsername(name); !errors.Is(err, ErrNotFound) {
			t.Fatalf("被拒用户 %s 竟然存在（err=%v）", name, err)
		}
	}
}

func TestRegistrationPolicy_ConcurrentFirstUser_ExactlyOne(t *testing.T) {
	db := openRegistrationTestDB(t)
	ctx := context.Background()
	db.SetRegistrationPolicy(RegistrationFirstUserOnly)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	users := make([]*User, n)
	for i := 0; i < n; i++ {
		users[i] = testUser(fmt.Sprintf("race-%d", i))
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = db.RegisterUser(ctx, users[i])
		}(i)
	}
	wg.Wait()

	ok := 0
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
			if users[i].Role != RoleAdmin {
				t.Errorf("赢家 %s 角色 = %q, want admin", users[i].Username, users[i].Role)
			}
		case errors.Is(err, ErrRegistrationClosed):
			// 期望：闸门在同一个事务里已经看到赢家那一行。
		default:
			t.Errorf("并发注册 #%d 意外错误: %v", i, err)
		}
	}
	if ok != 1 {
		t.Fatalf("并发首注册成功数 = %d, want 1", ok)
	}
	count, err := db.UserCount(ctx)
	if err != nil {
		t.Fatalf("user count: %v", err)
	}
	if count != 1 {
		t.Fatalf("用户数 = %d, want 1", count)
	}
}

func TestRegistrationPolicy_ReopenAfterPolicyChange(t *testing.T) {
	db := openRegistrationTestDB(t)
	ctx := context.Background()
	db.SetRegistrationPolicy(RegistrationFirstUserOnly)
	if _, err := db.RegisterUser(ctx, testUser("first")); err != nil {
		t.Fatalf("首个用户: %v", err)
	}
	if _, err := db.RegisterUser(ctx, testUser("blocked")); !errors.Is(err, ErrRegistrationClosed) {
		t.Fatalf("闸门应拒绝，err = %v", err)
	}
	db.SetRegistrationPolicy(RegistrationOpen)
	if _, err := db.RegisterUser(ctx, testUser("allowed")); err != nil {
		t.Fatalf("open 策略下应可注册: %v", err)
	}
}

func TestRegistrationPolicyString(t *testing.T) {
	cases := map[RegistrationPolicy]string{
		RegistrationOpen:          "open",
		RegistrationFirstUserOnly: "first-user-only",
		RegistrationPolicy(99):    "invalid",
	}
	for p, want := range cases {
		if got := p.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int32(p), got, want)
		}
	}
}
