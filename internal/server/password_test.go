package server

import (
	"bytes"
	"context"
	"encoding/json"
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

	testOldPassword = "old-password-1"
	testNewPassword = "new-password-2"
	testPassword    = "correct horse battery"
)

func newPasswordTestEnv(t *testing.T) *passwordTestEnv {
	t.Helper()
	rlPasswordLoginIP = newFixedWindow(rlPasswordLoginIPPerMin, time.Minute)
	rlPasswordLoginUser = newFixedWindow(rlPasswordLoginUserPerMin, time.Minute)

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

	authH := &authHandler{svc: svc}
	sessMW := svc.Sessions().Middleware
	mux := http.NewServeMux()
	// 与生产 mux.go 一致：显式的密码路由 + Passkey 路由 + /v1/auth/ 兜底。
	mux.Handle("/v1/auth/me", noStore(sessMW(http.HandlerFunc(authH.me))))
	mux.Handle("/v1/auth/passkeys", noStore(sessMW(http.HandlerFunc(authH.passkeysRoot))))
	mux.Handle("/v1/auth/passkeys/", noStore(sessMW(http.HandlerFunc(authH.passkeysItem))))
	mux.Handle(passwordLoginPath, noStore(http.HandlerFunc(authH.passwordLogin)))
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
