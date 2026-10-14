package auth

import (
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

func createUser(t *testing.T, svc *Service, username string) *store.User {
	t.Helper()
	u := &store.User{ID: store.NewUserID(), Username: username, CreatedAt: store.Now()}
	if err := svc.db.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func TestRecovery_IssueStoresHashNotPlaintext(t *testing.T) {
	svc := newTestService(t)
	u := createUser(t, svc, "alice")

	codes, err := svc.IssueRecoveryCodes(u.ID)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if len(codes) != recoveryCodeCount {
		t.Fatalf("want %d codes, got %d", recoveryCodeCount, len(codes))
	}
	for _, c := range codes {
		norm := normalizeRecoveryCode(c)
		if len(norm) != recoveryCodeRawLen {
			t.Errorf("code %q normalized len=%d want %d", c, len(norm), recoveryCodeRawLen)
		}
		if !strings.Contains(c, "-") {
			t.Errorf("display code should contain dashes: %q", c)
		}
	}

	unused, err := svc.db.ListUnusedRecoveryCodesByUser(u.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(unused) != recoveryCodeCount {
		t.Fatalf("stored unused=%d want %d", len(unused), recoveryCodeCount)
	}
	for _, rec := range unused {
		if !strings.HasPrefix(rec.CodeHash, "$argon2id$") {
			t.Errorf("hash should be argon2id PHC: %q", rec.CodeHash)
		}
		for _, plain := range codes {
			if rec.CodeHash == plain || rec.CodeHash == normalizeRecoveryCode(plain) {
				t.Errorf("hash must not equal plaintext")
			}
		}
		if rec.Prefix == "" || len(rec.Prefix) > recoveryPrefixLen {
			t.Errorf("bad prefix %q", rec.Prefix)
		}
	}
}

func TestRecovery_LoginRedeemsOnce(t *testing.T) {
	svc := newTestService(t)
	u := createUser(t, svc, "bob")
	codes, err := svc.IssueRecoveryCodes(u.ID)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	code := codes[0]

	sessID, err := svc.LoginWithRecoveryCode("bob", code, "Test · Linux")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if sessID == "" {
		t.Fatal("empty session id")
	}
	sess, err := svc.Sessions().Validate(sessID)
	if err != nil {
		t.Fatalf("validate session: %v", err)
	}
	if sess.UserID != u.ID {
		t.Errorf("session user %q want %q", sess.UserID, u.ID)
	}

	// 同一码不可再兑
	if _, err := svc.LoginWithRecoveryCode("bob", code, "Test · Linux"); err == nil {
		t.Fatal("reused code should fail")
	}

	n, err := svc.RecoveryStatus(u.ID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if n != recoveryCodeCount-1 {
		t.Errorf("unused=%d want %d", n, recoveryCodeCount-1)
	}
}

func TestRecovery_LoginAcceptsNormalizedInput(t *testing.T) {
	svc := newTestService(t)
	u := createUser(t, svc, "carol")
	codes, err := svc.IssueRecoveryCodes(u.ID)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// 小写、无连字符、带空格
	raw := normalizeRecoveryCode(codes[1])
	messy := strings.ToLower(raw[:4] + " " + raw[4:8] + "-" + raw[8:])
	if _, err := svc.LoginWithRecoveryCode("carol", messy, "UA"); err != nil {
		t.Fatalf("normalized login failed: %v (input=%q)", err, messy)
	}
}

func TestRecovery_WrongCodeAndUnknownUser(t *testing.T) {
	svc := newTestService(t)
	u := createUser(t, svc, "dave")
	if _, err := svc.IssueRecoveryCodes(u.ID); err != nil {
		t.Fatalf("issue: %v", err)
	}

	if _, err := svc.LoginWithRecoveryCode("dave", "AAAA-BBBB-CCCC-DDDD", "UA"); err == nil {
		t.Fatal("wrong code should fail")
	} else if !strings.Contains(err.Error(), "恢复码无效") {
		t.Errorf("error should be generic invalid: %v", err)
	}

	if _, err := svc.LoginWithRecoveryCode("nosuch", "AAAA-BBBB-CCCC-DDDD", "UA"); err == nil {
		t.Fatal("unknown user should fail")
	} else if !strings.Contains(err.Error(), "恢复码无效") {
		t.Errorf("unknown user error should not leak: %v", err)
	}
}

func TestRecovery_DisabledUserCannotLogin(t *testing.T) {
	svc := newTestService(t)
	u := createUser(t, svc, "erin")
	codes, err := svc.IssueRecoveryCodes(u.ID)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := svc.db.Exec(`UPDATE users SET disabled = 1 WHERE id = ?`, u.ID); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	if _, err := svc.LoginWithRecoveryCode("erin", codes[0], "UA"); err == nil {
		t.Fatal("disabled user should fail")
	} else if !strings.Contains(err.Error(), "禁用") {
		t.Errorf("want disabled error, got %v", err)
	}
}

func TestRecovery_RegenerateRevokesOld(t *testing.T) {
	svc := newTestService(t)
	u := createUser(t, svc, "frank")
	old, err := svc.IssueRecoveryCodes(u.ID)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	neu, err := svc.RegenerateRecoveryCodes(u.ID)
	if err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if len(neu) != recoveryCodeCount {
		t.Fatalf("new count=%d", len(neu))
	}
	// 旧码全部失效
	if _, err := svc.LoginWithRecoveryCode("frank", old[0], "UA"); err == nil {
		t.Fatal("old code should be revoked after regenerate")
	}
	// 新码可用
	if _, err := svc.LoginWithRecoveryCode("frank", neu[0], "UA"); err != nil {
		t.Fatalf("new code login: %v", err)
	}
	n, _ := svc.RecoveryStatus(u.ID)
	if n != recoveryCodeCount-1 {
		t.Errorf("unused after one redeem=%d", n)
	}
}

func TestNormalizeAndFormatRecoveryCode(t *testing.T) {
	raw := "ABCD2345EFGH6789"
	disp := formatRecoveryCode(raw)
	if disp != "ABCD-2345-EFGH-6789" {
		t.Errorf("format got %q", disp)
	}
	if normalizeRecoveryCode("abcd-2345-efgh-6789") != raw {
		t.Errorf("normalize dashed failed")
	}
	if normalizeRecoveryCode("  abcd 2345 efgh 6789 ") != raw {
		t.Errorf("normalize spaced failed")
	}
}
