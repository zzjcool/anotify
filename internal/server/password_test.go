package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/store"
)

// 密码登录的 HTTP 面（契约 §2）：状态码、JSON 字段名、Cookie 落地、限速、Passkey 护栏。
//
// 装配方式对齐 recovery_test.go / passkeys_test.go：真 store（:memory:）+ 真 auth.Service，
// 手搭一个与生产 mux.go 同形的路由表（含显式密码路由），不启动真服务。
//
// 每个 env 都重置密码登录的两个全局限速器：httptest 的 RemoteAddr 固定，
// 所有用例共用一个 IP 桶——不重置的话，前一个用例的登录次数会把后一个顶成 429。

type passwordTestEnv struct {
	mux    http.Handler
	db     *store.DB
	svc    *auth.Service
	userID string
	cookie *http.Cookie
}

const (
	// 与生产 mux.go 完全相同的两条路径（契约 §2）。
	passwordLoginPath = "/v1/auth/password/login"
	passwordPath      = "/v1/auth/password"
	// 用密码注册的路径 = 密码资源路径 + /register（与 mux.go 的显式路由同形）。
	passwordRegisterPath = passwordPath + "/register"

	testOldPassword = "old-password-1"
	testNewPassword = "new-password-2"
	testPassword    = "correct horse battery"
)

func newPasswordTestEnv(t *testing.T) *passwordTestEnv {
	t.Helper()
	rlPasswordLoginIP = newFixedWindow(rlPasswordLoginIPPerMin, time.Minute)
	rlPasswordLoginUser = newFixedWindow(rlPasswordLoginUserPerMin, time.Minute)
	// 注册的 IP 维同样要重置：所有用例共用一个 httptest RemoteAddr IP 桶。
	rlPasswordRegisterIP = newFixedWindow(rlPasswordRegisterPerMin, time.Minute)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	svc, err := auth.NewService(db, auth.Config{
		RPDisplayName: "Anotify Test",
		RPID:          "localhost",
		RPOrigins:     []string{"http://localhost"},
		SessionTTL:    time.Hour,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	uid, err := db.InsertUser(context.Background(), "", "pwuser", "PW User")
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	sess, err := svc.Sessions().Create(uid, "Test · Linux")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	authH := &authHandler{svc: svc, db: db}
	sessMW := svc.Sessions().Middleware
	mux := http.NewServeMux()
	// 与生产 mux.go 一致：显式的密码路由 + Passkey 路由 + /v1/auth/ 兜底。
	mux.Handle("/v1/auth/me", noStore(sessMW(http.HandlerFunc(authH.me))))
	mux.Handle("/v1/auth/passkeys", noStore(sessMW(http.HandlerFunc(authH.passkeysRoot))))
	mux.Handle("/v1/auth/passkeys/", noStore(sessMW(http.HandlerFunc(authH.passkeysItem))))
	mux.Handle(passwordLoginPath, noStore(http.HandlerFunc(authH.passwordLogin)))
	mux.Handle(passwordRegisterPath, noStore(http.HandlerFunc(authH.passwordRegister)))
	mux.Handle(passwordPath, noStore(sessMW(http.HandlerFunc(authH.password))))
	mux.Handle("/v1/auth/", noStore(http.HandlerFunc(authH.ServeHTTP)))

	return &passwordTestEnv{
		mux: mux, db: db, svc: svc, userID: uid,
		cookie: &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID},
	}
}

func pwReq(t *testing.T, env *passwordTestEnv, method, path string, body any, withCookie bool) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	if withCookie {
		req.AddCookie(env.cookie)
	}
	// 固定 UA：登录必须能推出设备名（契约 §3：DeviceNameFromUA(r.UserAgent())）。
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	return rr
}

// TestPassword_Unauthorized 未登录：三个方法都必须 401（而不是 404/405）。
func TestPassword_Unauthorized(t *testing.T) {
	env := newPasswordTestEnv(t)
	for _, c := range []struct {
		method string
		body   any
	}{
		{"GET", nil},
		{"POST", map[string]any{"newPassword": testPassword}},
		{"DELETE", map[string]any{"currentPassword": testPassword}},
	} {
		rr := pwReq(t, env, c.method, passwordPath, c.body, false)
		if rr.Code != 401 {
			t.Errorf("%s %s 未登录应 401, got %d (body=%s)", c.method, passwordPath, rr.Code, rr.Body.String())
		}
	}
}

// TestPassword_Login 登录成功/失败的状态码与 Cookie 落地。
func TestPassword_Login(t *testing.T) {
	env := newPasswordTestEnv(t)
	if _, err := env.svc.SetPassword(env.userID, "", testPassword); err != nil {
		t.Fatalf("set password: %v", err)
	}

	// 缺字段 → 400（不是 401：请求本身不完整）。
	rr := pwReq(t, env, "POST", passwordLoginPath, map[string]any{"username": "pwuser"}, false)
	if rr.Code != 400 {
		t.Errorf("缺 password 应 400, got %d (body=%s)", rr.Code, rr.Body.String())
	}

	// 错密码 → 401。
	rr = pwReq(t, env, "POST", passwordLoginPath, map[string]any{"username": "pwuser", "password": "wrong-password"}, false)
	if rr.Code != 401 {
		t.Errorf("错密码应 401, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	// 文案统一（不泄露用户是否存在）。
	if m := decodeBody(t, rr); m["error"] == nil || !strings.Contains(m["error"].(string), "用户名或密码错误") {
		t.Errorf("错密码文案 = %v", m["error"])
	}

	// 不存在的用户 → 401，且文案与错密码一模一样。
	rr = pwReq(t, env, "POST", passwordLoginPath, map[string]any{"username": "ghostuser", "password": testPassword}, false)
	if rr.Code != 401 {
		t.Errorf("不存在用户应 401, got %d", rr.Code)
	}
	if m := decodeBody(t, rr); !strings.Contains(m["error"].(string), "用户名或密码错误") {
		t.Errorf("不存在用户文案 = %v（不应泄露存在性）", m["error"])
	}

	// 正确密码 → 200 + Set-Cookie；用该 Cookie 能访问 /v1/auth/me。
	rr = pwReq(t, env, "POST", passwordLoginPath, map[string]any{"username": "pwuser", "password": testPassword}, false)
	if rr.Code != 200 {
		t.Fatalf("正确密码应 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	if m := decodeBody(t, rr); m["ok"] != true {
		t.Errorf("响应应为 {ok:true}, got %v", m)
	}
	var sessCookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == auth.SessionCookieName && c.Value != "" {
			sessCookie = c
		}
	}
	if sessCookie == nil {
		t.Fatal("登录成功没有种会话 Cookie")
	}
	// 会话里的设备名来自 UA（不是空串）。
	sess, err := env.svc.Sessions().Validate(sessCookie.Value)
	if err != nil {
		t.Fatalf("validate session: %v", err)
	}
	if sess.DeviceName != "Chrome · macOS" {
		t.Errorf("deviceName = %q, 期望 Chrome · macOS（来自 UA）", sess.DeviceName)
	}

	// 用登录拿到的 Cookie 打通 /v1/auth/me。
	req := httptest.NewRequest("GET", "/v1/auth/me", nil)
	req.AddCookie(sessCookie)
	meRR := httptest.NewRecorder()
	env.mux.ServeHTTP(meRR, req)
	if meRR.Code != 200 {
		t.Fatalf("用登录 Cookie 访问 /v1/auth/me 应 200, got %d (body=%s)", meRR.Code, meRR.Body.String())
	}
	if m := decodeBody(t, meRR); m["username"] != "pwuser" {
		t.Errorf("/me username = %v", m["username"])
	}
	// /me 不得泄露密码列。
	if strings.Contains(meRR.Body.String(), "argon2id") || strings.Contains(meRR.Body.String(), "hasPassword") {
		t.Errorf("/me 泄露了密码信息: %s", meRR.Body.String())
	}
}

// TestPassword_LoginNoPasswordAccount 无密码账号：401 + 明确指向 Passkey 的文案。
func TestPassword_LoginNoPasswordAccount(t *testing.T) {
	env := newPasswordTestEnv(t)
	rr := pwReq(t, env, "POST", passwordLoginPath, map[string]any{"username": "pwuser", "password": testPassword}, false)
	if rr.Code != 401 {
		t.Fatalf("无密码账号登录应 401, got %d", rr.Code)
	}
	if m := decodeBody(t, rr); !strings.Contains(m["error"].(string), "未设置密码") {
		t.Errorf("文案 = %v, 期望提示未设置密码", m["error"])
	}
}

// TestPassword_GetAfterSet 设置密码后 hasPassword=true，且响应里没有哈希。
func TestPassword_GetAfterSet(t *testing.T) {
	env := newPasswordTestEnv(t)

	rr := pwReq(t, env, "GET", passwordPath, nil, true)
	if rr.Code != 200 {
		t.Fatalf("GET 应 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	m := decodeBody(t, rr)
	if m["hasPassword"] != false {
		t.Errorf("未设置时 hasPassword = %v", m["hasPassword"])
	}
	if m["updatedAt"] != float64(0) {
		t.Errorf("未设置时 updatedAt = %v, 期望 0", m["updatedAt"])
	}
	// 字段名必须是 camelCase 的两个（不多不少）。
	for _, k := range []string{"hasPassword", "updatedAt"} {
		if _, ok := m[k]; !ok {
			t.Errorf("响应缺字段 %q: %v", k, m)
		}
	}

	// 设置密码（当前密码留空 = 首次设置）。
	rr = pwReq(t, env, "POST", passwordPath, map[string]any{"newPassword": testPassword}, true)
	if rr.Code != 200 {
		t.Fatalf("首次设置密码应 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	m = decodeBody(t, rr)
	if m["ok"] != true || m["hasPassword"] != true {
		t.Errorf("设置响应 = %v", m)
	}
	if _, ok := m["updatedAt"].(float64); !ok {
		t.Errorf("updatedAt 应为数字, got %T", m["updatedAt"])
	}

	// GET 复核：状态变了，且响应体里没有哈希（任何形态）。
	rr = pwReq(t, env, "GET", passwordPath, nil, true)
	if rr.Code != 200 {
		t.Fatalf("GET 应 200, got %d", rr.Code)
	}
	m = decodeBody(t, rr)
	if m["hasPassword"] != true {
		t.Errorf("设置后 hasPassword = %v", m["hasPassword"])
	}
	if m["updatedAt"] == float64(0) {
		t.Error("设置后 updatedAt 应 > 0")
	}
	if strings.Contains(rr.Body.String(), "argon2id") {
		t.Errorf("GET 响应泄露哈希: %s", rr.Body.String())
	}
}

// TestPassword_ChangeInvalidatesOld 改密码：旧密码登录 401、新密码 200。
func TestPassword_ChangeInvalidatesOld(t *testing.T) {
	env := newPasswordTestEnv(t)
	if _, err := env.svc.SetPassword(env.userID, "", testOldPassword); err != nil {
		t.Fatalf("set: %v", err)
	}

	// 当前密码错 → 400。
	rr := pwReq(t, env, "POST", passwordPath, map[string]any{
		"currentPassword": "not-the-password", "newPassword": testNewPassword,
	}, true)
	if rr.Code != 400 {
		t.Fatalf("当前密码错应 400, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	if m := decodeBody(t, rr); !strings.Contains(m["error"].(string), "当前密码不正确") {
		t.Errorf("文案 = %v", m["error"])
	}

	// 新密码太短 → 400。
	rr = pwReq(t, env, "POST", passwordPath, map[string]any{
		"currentPassword": testOldPassword, "newPassword": "short",
	}, true)
	if rr.Code != 400 {
		t.Fatalf("新密码太短应 400, got %d", rr.Code)
	}

	// 正确 → 200。
	rr = pwReq(t, env, "POST", passwordPath, map[string]any{
		"currentPassword": testOldPassword, "newPassword": testNewPassword,
	}, true)
	if rr.Code != 200 {
		t.Fatalf("改密码应 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}

	// 旧密码 → 401；新密码 → 200。
	rr = pwReq(t, env, "POST", passwordLoginPath, map[string]any{"username": "pwuser", "password": testOldPassword}, false)
	if rr.Code != 401 {
		t.Errorf("旧密码应失效(401), got %d", rr.Code)
	}
	rr = pwReq(t, env, "POST", passwordLoginPath, map[string]any{"username": "pwuser", "password": testNewPassword}, false)
	if rr.Code != 200 {
		t.Errorf("新密码应可用(200), got %d (body=%s)", rr.Code, rr.Body.String())
	}
}

// TestPassword_Remove 移除密码：无 Passkey → 409（最后一种登录方式），有 Passkey → 200。
func TestPassword_Remove(t *testing.T) {
	env := newPasswordTestEnv(t)
	if _, err := env.svc.SetPassword(env.userID, "", testPassword); err != nil {
		t.Fatalf("set: %v", err)
	}

	// 当前密码错 → 400。
	rr := pwReq(t, env, "DELETE", passwordPath, map[string]any{"currentPassword": "wrong-password"}, true)
	if rr.Code != 400 {
		t.Errorf("当前密码错应 400, got %d (body=%s)", rr.Code, rr.Body.String())
	}

	// 没有 Passkey → 409（否则账号再没有任何登录方式）。
	rr = pwReq(t, env, "DELETE", passwordPath, map[string]any{"currentPassword": testPassword}, true)
	if rr.Code != 409 {
		t.Fatalf("无 Passkey 移除密码应 409, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	if m := decodeBody(t, rr); !strings.Contains(m["error"].(string), "至少保留一种登录方式") {
		t.Errorf("409 文案 = %v", m["error"])
	}
	// 被拒绝后密码仍然在。
	if st, err := env.svc.PasswordStatus(env.userID); err != nil || !st.HasPassword {
		t.Errorf("被拒绝的移除不应清掉密码: st=%+v err=%v", st, err)
	}

	// 加一个 Passkey → 移除成功。
	if err := env.db.CreatePasskey(&store.Passkey{
		ID: "cred-pw-remove", UserID: env.userID, PublicKey: []byte{1, 2, 3},
		Name: "手机", Transports: []string{"internal"}, CreatedAt: store.Now(),
	}); err != nil {
		t.Fatalf("create passkey: %v", err)
	}
	rr = pwReq(t, env, "DELETE", passwordPath, map[string]any{"currentPassword": testPassword}, true)
	if rr.Code != 200 {
		t.Fatalf("有 Passkey 移除密码应 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	if m := decodeBody(t, rr); m["ok"] != true || m["hasPassword"] != false {
		t.Errorf("移除响应 = %v", m)
	}
	// 复核：密码登录已失效（未设置密码）。
	rr = pwReq(t, env, "POST", passwordLoginPath, map[string]any{"username": "pwuser", "password": testPassword}, false)
	if rr.Code != 401 {
		t.Errorf("移除后密码登录应 401, got %d", rr.Code)
	}
}

// TestPassword_LoginRateLimit 同一 username 连续错 11 次 → 第 11 次 429 + Retry-After。
func TestPassword_LoginRateLimit(t *testing.T) {
	env := newPasswordTestEnv(t)
	// env 构造时已重置两个窗口；这里再显式重置一次，避免将来别的用例改顺序后互相干扰。
	rlPasswordLoginIP = newFixedWindow(rlPasswordLoginIPPerMin, time.Minute)
	rlPasswordLoginUser = newFixedWindow(rlPasswordLoginUserPerMin, time.Minute)

	body := map[string]any{"username": "pwuser", "password": "always-wrong"}
	for i := 0; i < 10; i++ {
		rr := pwReq(t, env, "POST", passwordLoginPath, body, false)
		if rr.Code != 401 {
			t.Fatalf("第 %d 次应 401, got %d (body=%s)", i+1, rr.Code, rr.Body.String())
		}
	}
	rr := pwReq(t, env, "POST", passwordLoginPath, body, false)
	if rr.Code != 429 {
		t.Fatalf("第 11 次应 429, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	if ra := rr.Header().Get("Retry-After"); ra == "" {
		t.Error("429 应带 Retry-After")
	}
	// 429 的文案走统一的 writeErr 通道。
	if m := decodeBody(t, rr); !strings.Contains(m["error"].(string), "请求过于频繁") {
		t.Errorf("429 文案 = %v", m["error"])
	}
	// 换一个 username 仍然能过 username 维（说明限的是这个 key，不是全局打死）。
	// 注：IP 维此时已满，所以这里换个桶验证的是 username 维本身——
	// 该请求会因为 IP 维 429，这正是"两维都过才放行"的预期，故只断言不会 401。
	rr = pwReq(t, env, "POST", passwordLoginPath, map[string]any{"username": "anotheruser", "password": "always-wrong"}, false)
	if rr.Code == 401 {
		t.Errorf("不同 username 的第一次请求不该被 username 维拦成 401，got %d", rr.Code)
	}
}

// TestPassword_DeleteLastPasskeyAllowedWithPassword 护栏改造：有密码时允许删最后一个 Passkey。
func TestPassword_DeleteLastPasskeyAllowedWithPassword(t *testing.T) {
	env := newPasswordTestEnv(t)
	if err := env.db.CreatePasskey(&store.Passkey{
		ID: "cred-only-pw", UserID: env.userID, PublicKey: []byte{1},
		Name: "唯一", Transports: []string{"internal"}, CreatedAt: store.Now(),
	}); err != nil {
		t.Fatalf("create passkey: %v", err)
	}

	// 没有密码 → 409，文案要求「先设置密码」。
	rr := pwReq(t, env, "DELETE", "/v1/auth/passkeys/cred-only-pw", nil, true)
	if rr.Code != 409 {
		t.Fatalf("无密码删唯一 Passkey 应 409, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	if m := decodeBody(t, rr); !strings.Contains(m["error"].(string), "请先设置密码") {
		t.Errorf("409 文案 = %v", m["error"])
	}
	if _, err := env.db.GetPasskeyByID("cred-only-pw"); err != nil {
		t.Errorf("被拒绝的删除不该动数据: %v", err)
	}

	// 设置密码后 → 允许删（200），且真的删掉了。
	if _, err := env.svc.SetPassword(env.userID, "", testPassword); err != nil {
		t.Fatalf("set password: %v", err)
	}
	rr = pwReq(t, env, "DELETE", "/v1/auth/passkeys/cred-only-pw", nil, true)
	if rr.Code != 200 {
		t.Fatalf("有密码删最后一个 Passkey 应 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	if _, err := env.db.GetPasskeyByID("cred-only-pw"); err == nil {
		t.Error("Passkey 应已被删除")
	}
	// 删完之后密码还在 → 账号没被锁死。
	if st, err := env.svc.PasswordStatus(env.userID); err != nil || !st.HasPassword {
		t.Errorf("删 Passkey 不该影响密码: st=%+v err=%v", st, err)
	}
}

// TestPassword_RoutesUseExplicitPaths 兜住契约 §2 的路径形状：
// 请求打到 /v1/auth/password 与 /v1/auth/password/login 必须命中密码处理器，
// 而不是被 /v1/auth/ 兜底成「unknown auth endpoint」。
func TestPassword_RoutesUseExplicitPaths(t *testing.T) {
	env := newPasswordTestEnv(t)

	// 未登录的管理端点：401（mux 层 sessMW 命中，而不是 404）。
	rr := pwReq(t, env, "GET", passwordPath, nil, false)
	if rr.Code != 401 || strings.Contains(rr.Body.String(), "unknown auth endpoint") {
		t.Errorf("GET %s = %d %s", passwordPath, rr.Code, rr.Body.String())
	}
	// login 端点：缺字段 400（说明命中的是 passwordLogin，不是兜底）。
	rr = pwReq(t, env, "POST", passwordLoginPath, map[string]any{}, false)
	if rr.Code != 400 || strings.Contains(rr.Body.String(), "unknown auth endpoint") {
		t.Errorf("POST %s = %d %s", passwordLoginPath, rr.Code, rr.Body.String())
	}
}

// ---------- 用密码注册的 HTTP 面（契约「追加：用密码注册」§A2 / §A6） ----------
//
// 端点契约：匿名 + IP 维 10/min 限速；200 带 Set-Cookie 与 8 个恢复码；
// 用户名/密码不合规与「用户名已存在」都是 400；超限 429。

// registerReq 是一次 POST <password>/register（匿名，不带旧 Cookie）。
func registerReq(t *testing.T, env *passwordTestEnv, body any) *httptest.ResponseRecorder {
	t.Helper()
	return pwReq(t, env, "POST", passwordRegisterPath, body, false)
}

// TestPasswordRegister_Success 成功路径：200 + Cookie + 8 个恢复码，
// 且该账号 hasPassword=true、Passkey 为 0（密码就是它的登录方式）。
func TestPasswordRegister_Success(t *testing.T) {
	env := newPasswordTestEnv(t)

	rr := registerReq(t, env, map[string]any{"username": "signup-user", "password": testPassword})
	if rr.Code != 200 {
		t.Fatalf("注册应 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	// 命中显式路由：否则这条路径不在 /v1/auth/ 前缀下，会掉进静态资源（SPA）。
	if strings.Contains(rr.Body.String(), "unknown auth endpoint") {
		t.Fatalf("请求掉进了兜底 handler: %s", rr.Body.String())
	}
	m := decodeBody(t, rr)
	if m["ok"] != true {
		t.Errorf("响应 ok = %v, want true", m["ok"])
	}
	codes, ok := m["recoveryCodes"].([]any)
	if !ok {
		t.Fatalf("recoveryCodes 应为数组, got %T (%v)", m["recoveryCodes"], m["recoveryCodes"])
	}
	if len(codes) != 8 {
		t.Fatalf("recoveryCodes 长度 = %d, want 8", len(codes))
	}
	for _, c := range codes {
		s, _ := c.(string)
		if len(s) != len("XXXX-XXXX-XXXX-XXXX") || strings.Count(s, "-") != 3 {
			t.Errorf("恢复码形状异常: %q", s)
		}
	}
	// 响应里不该有任何哈希形态（连 PHC 前缀都不能出现）。
	if strings.Contains(rr.Body.String(), "argon2id") {
		t.Errorf("注册响应泄露了哈希: %s", rr.Body.String())
	}

	var sessCookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == auth.SessionCookieName && c.Value != "" {
			sessCookie = c
		}
	}
	if sessCookie == nil {
		t.Fatal("注册成功没有种会话 Cookie")
	}
	sess, err := env.svc.Sessions().Validate(sessCookie.Value)
	if err != nil {
		t.Fatalf("validate session: %v", err)
	}
	if sess.DeviceName != "Chrome · macOS" {
		t.Errorf("deviceName = %q, 期望 Chrome · macOS（来自 UA）", sess.DeviceName)
	}

	// 用注册拿到的 Cookie 访问 /v1/auth/me：身份正确、且不泄露密码列。
	env.cookie = sessCookie
	rr = pwReq(t, env, "GET", "/v1/auth/me", nil, true)
	if rr.Code != 200 {
		t.Fatalf("GET /v1/auth/me 应 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	if m := decodeBody(t, rr); m["username"] != "signup-user" {
		t.Errorf("/me username = %v, want signup-user", m["username"])
	}
	if strings.Contains(rr.Body.String(), "argon2id") || strings.Contains(rr.Body.String(), "hasPassword") {
		t.Errorf("/me 泄露了密码信息: %s", rr.Body.String())
	}

	// GET <password>：hasPassword=true。
	rr = pwReq(t, env, "GET", passwordPath, nil, true)
	if rr.Code != 200 {
		t.Fatalf("GET 密码状态应 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	m = decodeBody(t, rr)
	if m["hasPassword"] != true {
		t.Errorf("注册后 hasPassword = %v, want true", m["hasPassword"])
	}
	if m["updatedAt"] == float64(0) {
		t.Error("注册后 updatedAt 应 > 0")
	}

	// GET /v1/auth/passkeys：0 个，且是 []（不是 null）。
	rr = pwReq(t, env, "GET", "/v1/auth/passkeys", nil, true)
	if rr.Code != 200 {
		t.Fatalf("GET passkeys 应 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	m = decodeBody(t, rr)
	if m["count"] != float64(0) {
		t.Errorf("Passkey count = %v, want 0（密码注册不建 Passkey）", m["count"])
	}
	if pk, ok := m["passkeys"].([]any); !ok || len(pk) != 0 {
		t.Errorf("passkeys = %v, want []", m["passkeys"])
	}
	if !strings.Contains(rr.Body.String(), `"passkeys":[]`) {
		t.Errorf("空列表必须是 [] 而不是 null: %s", rr.Body.String())
	}

	// 注册完能立刻用密码登录（第二条独立会话）。
	rr = pwReq(t, env, "POST", passwordLoginPath, map[string]any{"username": "signup-user", "password": testPassword}, false)
	if rr.Code != 200 {
		t.Fatalf("注册后密码登录应 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
}

// TestPasswordRegister_DuplicateUsername 重复用户名 → 400 + 同一句文案（不泄露内部错误）。
func TestPasswordRegister_DuplicateUsername(t *testing.T) {
	env := newPasswordTestEnv(t)
	body := map[string]any{"username": "dup-signup", "password": testPassword}
	if rr := registerReq(t, env, body); rr.Code != 200 {
		t.Fatalf("首次注册应 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}

	rr := registerReq(t, env, body)
	if rr.Code != 400 {
		t.Fatalf("重复用户名应 400, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	m := decodeBody(t, rr)
	if s, _ := m["error"].(string); !strings.Contains(s, "用户名已存在") {
		t.Errorf("文案 = %v, want 含「用户名已存在」", m["error"])
	}
	// 不能把 sqlite 的约束名/列名漏给前端。
	if s, _ := m["error"].(string); strings.Contains(s, "UNIQUE") || strings.Contains(s, "insert user") {
		t.Errorf("文案泄露了存储层细节: %q", s)
	}
}

// TestPasswordRegister_RejectsBadInput 短密码 / 非法用户名 / 缺字段 → 400，且不留用户行。
func TestPasswordRegister_RejectsBadInput(t *testing.T) {
	env := newPasswordTestEnv(t)

	cases := []struct {
		name string
		body any
		want string
	}{
		{"短密码", map[string]any{"username": "shortpw", "password": "short"}, "密码至少 8 个字符"},
		{"非法用户名", map[string]any{"username": "bad name", "password": testPassword}, "用户名只能包含字母、数字"},
		{"缺 username", map[string]any{"password": testPassword}, "缺少 username 或 password"},
		{"缺 password", map[string]any{"username": "nopass"}, "缺少 username 或 password"},
		{"空 body", map[string]any{}, "缺少 username 或 password"},
	}
	for _, tc := range cases {
		rr := registerReq(t, env, tc.body)
		if rr.Code != 400 {
			t.Errorf("%s: 应 400, got %d (body=%s)", tc.name, rr.Code, rr.Body.String())
			continue
		}
		m := decodeBody(t, rr)
		if s, _ := m["error"].(string); !strings.Contains(s, tc.want) {
			t.Errorf("%s: 文案 = %v, want 含 %q", tc.name, m["error"], tc.want)
		}
	}
	// 失败路径不许留下用户行（§A1：不能有「有账号没登录方式」的半成品）。
	for _, name := range []string{"shortpw", "nopass"} {
		if _, err := env.db.GetUserByUsername(name); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s 不应被创建: %v", name, err)
		}
	}
}

// TestPasswordRegister_RateLimit 同一 IP 连续 11 次 → 第 11 次 429 + Retry-After。
//
// 限速判定**先于读 body**：这里前 10 次用空 body（400）也照样占额度——
// 否则攻击者可以靠发坏请求白嫖 argon2 与恢复码签发的 CPU。
func TestPasswordRegister_RateLimit(t *testing.T) {
	env := newPasswordTestEnv(t)
	rlPasswordRegisterIP = newFixedWindow(rlPasswordRegisterPerMin, time.Minute)

	for i := 0; i < rlPasswordRegisterPerMin; i++ {
		rr := registerReq(t, env, map[string]any{})
		if rr.Code != 400 {
			t.Fatalf("第 %d 次应 400（限速尚未触发）, got %d (body=%s)", i+1, rr.Code, rr.Body.String())
		}
	}
	rr := registerReq(t, env, map[string]any{"username": "ratelimit-user", "password": testPassword})
	if rr.Code != 429 {
		t.Fatalf("第 %d 次应 429, got %d (body=%s)", rlPasswordRegisterPerMin+1, rr.Code, rr.Body.String())
	}
	if ra := rr.Header().Get("Retry-After"); ra == "" {
		t.Error("429 应带 Retry-After")
	}
	if m := decodeBody(t, rr); !strings.Contains(m["error"].(string), "请求过于频繁") {
		t.Errorf("429 文案 = %v", m["error"])
	}
	// 被限速的这次不该建出用户（限速在读写之前）。
	if _, err := env.db.GetUserByUsername("ratelimit-user"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("429 的请求不应建出用户: %v", err)
	}
}

// TestPasswordRegister_RouteUseExplicitPath 兜住 §A2 的「显式路由」：
// 这条路径不在 /v1/auth/ 的兜底下，漏挂会掉进静态资源（SPA），而不是一个显式错误码。
func TestPasswordRegister_RouteUseExplicitPath(t *testing.T) {
	env := newPasswordTestEnv(t)
	// GET 打到注册端点：命中 handler（405），而不是掉进静态资源。
	rr := pwReq(t, env, "GET", passwordRegisterPath, nil, false)
	if rr.Code != 405 {
		t.Errorf("GET 注册端点应 405, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "unknown auth endpoint") {
		t.Errorf("注册端点掉进了兜底 handler: %s", rr.Body.String())
	}
}
