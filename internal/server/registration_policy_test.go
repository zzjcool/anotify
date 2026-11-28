package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// 自助注册策略（#574）的整机判据。

// resetRegistrationRateLimits：httptest 的 RemoteAddr 全是 192.0.2.1，限速器会串味。
func resetRegistrationRateLimits() {
	rlPasskeyRegisterIP = newFixedWindow(rlPasskeyRegisterPerMin, time.Minute)
	rlPasswordRegisterIP = newFixedWindow(rlPasswordRegisterPerMin, time.Minute)
	rlRegistrationStatusIP = newFixedWindow(rlRegistrationStatusPerMin, time.Minute)
}

func registrationApp(t *testing.T, mode string) *App {
	t.Helper()
	resetRegistrationRateLimits()
	return newProductionAppWithConfig(t, func(cfg *Config) { cfg.Registration = mode })
}

func appDoJSON(t *testing.T, app *App, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	app.Handler.ServeHTTP(rr, req)
	return rr
}

func registrationState(t *testing.T, app *App) string {
	t.Helper()
	rr := appDo(t, app, http.MethodGet, "/v1/auth/registration", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v1/auth/registration = %d（body=%s）, want 200", rr.Code, rr.Body.String())
	}
	var out registrationStatusResp
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("解 /v1/auth/registration: %v（body=%s）", err, rr.Body.String())
	}
	return out.Registration
}

func registerPassword(t *testing.T, app *App, username string) *httptest.ResponseRecorder {
	t.Helper()
	return appDoJSON(t, app, http.MethodPost, "/v1/auth/password/register", map[string]any{
		"username": username, "password": "test-only-not-secret", "displayName": username,
	})
}

func TestRegistrationStatus_ReportsPolicy(t *testing.T) {
	t.Run("空库 first-user-only 折算 open", func(t *testing.T) {
		app := registrationApp(t, registrationFirstUserOnly)
		if got := registrationState(t, app); got != registrationOpen {
			t.Fatalf("空库 registration = %q, want open", got)
		}
	})
	t.Run("有用户 first-user-only 折算 closed", func(t *testing.T) {
		app := registrationApp(t, registrationFirstUserOnly)
		if rr := registerPassword(t, app, "first"); rr.Code != http.StatusOK {
			t.Fatalf("首个注册 = %d（body=%s）, want 200", rr.Code, rr.Body.String())
		}
		if got := registrationState(t, app); got != registrationClosed {
			t.Fatalf("首个用户之后 registration = %q, want closed", got)
		}
	})
	t.Run("open 策略恒 open", func(t *testing.T) {
		app := registrationApp(t, registrationOpen)
		if rr := registerPassword(t, app, "first"); rr.Code != http.StatusOK {
			t.Fatalf("首个注册 = %d, want 200", rr.Code)
		}
		if got := registrationState(t, app); got != registrationOpen {
			t.Fatalf("open 策略下 registration = %q, want open", got)
		}
	})
}

func TestRegistrationPolicy_FirstUserOnly_ClosesAfterFirstUser(t *testing.T) {
	app := registrationApp(t, registrationFirstUserOnly)
	if rr := registerPassword(t, app, "first"); rr.Code != http.StatusOK {
		t.Fatalf("首个注册 = %d（body=%s）, want 200", rr.Code, rr.Body.String())
	}
	n, err := app.DB.UserCount(t.Context())
	if err != nil || n != 1 {
		t.Fatalf("用户数 = %d (err=%v), want 1", n, err)
	}
	firstID := ""
	if u, err := app.DB.GetUserByUsername("first"); err == nil {
		firstID = u.ID
	}

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"密码注册", http.MethodPost, "/v1/auth/password/register",
			map[string]any{"username": "second-password", "password": "test-only-not-secret"}},
		{"Passkey 第一跳 register/options", http.MethodPost, "/v1/auth/register/options",
			map[string]any{"username": "second-passkey"}},
		{"Passkey 第二跳 register", http.MethodPost, "/v1/auth/register?username=second-passkey",
			map[string]any{"id": "x", "rawId": "x", "type": "public-key"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := appDoJSON(t, app, c.method, c.path, c.body)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("%s = %d（body=%s）, want 403", c.path, rr.Code, rr.Body.String())
			}
			var out map[string]string
			_ = json.Unmarshal(rr.Body.Bytes(), &out)
			if out["error"] != "registration_closed" {
				t.Fatalf("error = %q, want registration_closed", out["error"])
			}
		})
	}

	// 闸门拒绝后库没有变。
	if n, err := app.DB.UserCount(t.Context()); err != nil || n != 1 {
		t.Fatalf("拒绝后用户数 = %d (err=%v), want 1", n, err)
	}
	for _, name := range []string{"second-password", "second-passkey"} {
		if _, err := app.DB.GetUserByUsername(name); err == nil {
			t.Fatalf("被拒用户 %s 竟然存在", name)
		}
	}
	if firstID == "" {
		t.Fatal("首个用户查不到：闸门把合法注册也拒了？")
	}
}

func TestRegistrationPolicy_OpenAllowsSecondUser(t *testing.T) {
	app := registrationApp(t, registrationOpen)
	for _, name := range []string{"first", "second"} {
		if rr := registerPassword(t, app, name); rr.Code != http.StatusOK {
			t.Fatalf("open 策略下 %s 注册 = %d（body=%s）, want 200", name, rr.Code, rr.Body.String())
		}
	}
}

func TestRegistrationPolicy_ConcurrentFirstUser_ExactlyOneAccount(t *testing.T) {
	app := registrationApp(t, registrationFirstUserOnly)

	const n = 6
	var wg sync.WaitGroup
	codes := make([]int, n)
	bodies := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/auth/password/register",
				strings.NewReader(fmt.Sprintf(`{"username":"race-%d","password":"test-only-not-secret"}`, i)))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			app.Handler.ServeHTTP(rr, req)
			codes[i] = rr.Code
			bodies[i] = rr.Body.String()
		}(i)
	}
	wg.Wait()

	ok := 0
	for i, code := range codes {
		switch code {
		case http.StatusOK:
			ok++
		case http.StatusForbidden:
			if !strings.Contains(bodies[i], "registration_closed") {
				t.Errorf("并发输家 #%d 403 但 body=%s", i, bodies[i])
			}
		default:
			t.Errorf("并发注册 #%d = %d（body=%s）", i, code, bodies[i])
		}
	}
	if ok != 1 {
		t.Fatalf("并发首注册成功数 = %d, want 1", ok)
	}
	nUsers, err := app.DB.UserCount(t.Context())
	if err != nil || nUsers != 1 {
		t.Fatalf("用户数 = %d (err=%v), want 1", nUsers, err)
	}
}

func TestPasskeyRegisterOptions_RateLimited(t *testing.T) {
	app := registrationApp(t, registrationOpen) // 用 open 排除闸门干扰，只测限流
	for i := 1; i <= rlPasskeyRegisterPerMin; i++ {
		rr := appDoJSON(t, app, http.MethodPost, "/v1/auth/register/options",
			map[string]any{"username": fmt.Sprintf("rl-%d", i)})
		if rr.Code == http.StatusTooManyRequests {
			t.Fatalf("第 %d 次 register/options 就 429 了（额度 %d/min）", i, rlPasskeyRegisterPerMin)
		}
	}
	rr := appDoJSON(t, app, http.MethodPost, "/v1/auth/register/options",
		map[string]any{"username": "rl-over"})
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("第 %d 次 register/options = %d, want 429", rlPasskeyRegisterPerMin+1, rr.Code)
	}
	// 第二跳共用同一个限速器。
	if rr := appDoJSON(t, app, http.MethodPost, "/v1/auth/register?username=rl-over",
		map[string]any{"id": "x"}); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("额度用尽后 register = %d, want 429", rr.Code)
	}
}

func TestRegistrationStatus_RateLimited(t *testing.T) {
	app := registrationApp(t, registrationFirstUserOnly)
	for i := 0; i < rlRegistrationStatusPerMin; i++ {
		if rr := appDo(t, app, http.MethodGet, "/v1/auth/registration", nil); rr.Code == http.StatusTooManyRequests {
			t.Fatalf("第 %d 次 GET /v1/auth/registration 就 429 了（额度 %d/min）", i+1, rlRegistrationStatusPerMin)
		}
	}
	if rr := appDo(t, app, http.MethodGet, "/v1/auth/registration", nil); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("第 %d 次 GET /v1/auth/registration = %d, want 429", rlRegistrationStatusPerMin+1, rr.Code)
	}
}

func TestConfig_RegistrationPolicyParsing(t *testing.T) {
	cases := []struct {
		raw     string
		want    string
		wantPol store.RegistrationPolicy
	}{
		{"", registrationFirstUserOnly, store.RegistrationFirstUserOnly},
		{"first-user-only", registrationFirstUserOnly, store.RegistrationFirstUserOnly},
		{"OPEN", registrationOpen, store.RegistrationOpen},
		{"  open  ", registrationOpen, store.RegistrationOpen},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			t.Setenv("ANOTIFY_REGISTRATION", c.raw)
			cfg := FromEnv()
			if cfg.Registration != c.want {
				t.Fatalf("Registration = %q, want %q", cfg.Registration, c.want)
			}
			if got := cfg.RegistrationPolicy(); got != c.wantPol {
				t.Fatalf("RegistrationPolicy() = %v, want %v", got, c.wantPol)
			}
		})
	}
	if got := (Config{}).RegistrationPolicy(); got != store.RegistrationFirstUserOnly {
		t.Fatalf("零值 Config 的策略 = %v, want first-user-only", got)
	}
}

func TestConfig_RegistrationInvalidValue_FailsStartup(t *testing.T) {
	for _, raw := range []string{"nope", "Open-All", "first_user_only", registrationInvite} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("ANOTIFY_REGISTRATION", raw)
			t.Setenv("ANOTIFY_MAX_HOPS", "")
			t.Setenv("ANOTIFY_TRACE_TASK_BUDGET", "")
			t.Setenv("ANOTIFY_LEASE_TTL", "")
			cfg := FromEnv()
			if err := cfg.Validate(); err == nil {
				t.Fatalf("ANOTIFY_REGISTRATION=%q 应让 Validate 失败", raw)
			}
		})
	}
	for _, raw := range []string{"", registrationFirstUserOnly, registrationOpen} {
		t.Run("ok/"+raw, func(t *testing.T) {
			t.Setenv("ANOTIFY_REGISTRATION", raw)
			t.Setenv("ANOTIFY_MAX_HOPS", "")
			t.Setenv("ANOTIFY_TRACE_TASK_BUDGET", "")
			t.Setenv("ANOTIFY_LEASE_TTL", "")
			if err := FromEnv().Validate(); err != nil {
				t.Fatalf("ANOTIFY_REGISTRATION=%q Validate: %v", raw, err)
			}
		})
	}
}
