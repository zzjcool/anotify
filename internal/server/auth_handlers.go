package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/store"
)

// authHandler 包装 auth.Service，提供 /v1/auth/* HTTP 端点。
//
// db 只给「列表分页」这类只读查询用（#377）：会话/凭证列表需要 keyset 查询，
// 而 auth.Service 只暴露全量 ListByUser（形状不对齐分页）。写入仍然全走 Service，
// 不把认证逻辑搬进 handler。
type authHandler struct {
	svc *auth.Service
	db  *store.DB
}

type usernameReq struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Token       string `json:"token"` // discoverable login 的会话 token
}

// ServeHTTP 路由 /v1/auth/*。
func (h *authHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sub := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/auth"), "/")
	switch sub {
	case "register/options":
		h.registerOptions(w, r)
	case "register":
		h.register(w, r)
	case "login/options":
		h.loginOptions(w, r)
	case "login":
		h.login(w, r)
	case "logout":
		h.logout(w, r)
	case "sessions":
		h.sessions(w, r)
	case "me":
		h.me(w, r)
	case "recovery/login":
		h.recoveryLogin(w, r)
	case "password/login":
		// 显式路由在 mux.go（契约 §2）；这里再留一条兜底：哪怕只挂了 /v1/auth/，
		// 也不会退化成 404——匿名登录端点必须总是可达。
		h.passwordLogin(w, r)
	case "password":
		h.password(w, r)
	default:
		writeErr(w, 404, "unknown auth endpoint: "+sub)
	}
}

func (h *authHandler) registerOptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	var req usernameReq
	if err := readJSON(r, &req); err != nil || req.Username == "" {
		writeErr(w, 400, "缺少 username")
		return
	}
	if err := auth.ValidateUsername(req.Username); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	opts, err := h.svc.BeginRegister(req.Username, req.DisplayName)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, opts)
}

func (h *authHandler) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	// username 从 query 传（WebAuthn 断言在 body）
	username := r.URL.Query().Get("username")
	if username == "" {
		writeErr(w, 400, "缺少 username（query）")
		return
	}
	sessID, codes, err := h.svc.FinishRegister(username, r)
	if err != nil {
		slog.Warn("register failed",
			"event", "auth.register.fail",
			"ip", clientIP(r),
			"error", err.Error(),
		)
		writeErr(w, 400, err.Error())
		return
	}
	h.svc.Sessions().SetCookie(w, sessID)
	slog.Info("register success",
		"event", "auth.register.success",
		"ip", clientIP(r),
	)
	// recoveryCodes 为可选兼容字段：明文仅此一次；前端未接时忽略即可。
	if codes == nil {
		codes = []string{}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "recoveryCodes": codes})
}

func (h *authHandler) loginOptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	var req usernameReq
	_ = readJSON(r, &req) // username 可空（discoverable）
	var (
		opts any
		err  error
	)
	if req.Username != "" {
		if verr := auth.ValidateUsername(req.Username); verr != nil {
			writeErr(w, 400, verr.Error())
			return
		}
		opts, err = h.svc.BeginLogin(req.Username)
	} else {
		opts, err = h.svc.BeginDiscoverableLogin(req.Token)
	}
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, opts)
}

func (h *authHandler) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	username := r.URL.Query().Get("username")
	token := r.URL.Query().Get("token")
	var (
		sessID string
		err    error
	)
	if username != "" {
		sessID, err = h.svc.FinishLogin(username, r)
	} else {
		sessID, err = h.svc.FinishDiscoverableLogin(token, r)
	}
	if err != nil {
		slog.Warn("login failed",
			"event", "auth.login.fail",
			"ip", clientIP(r),
			"error", err.Error(),
		)
		writeErr(w, 401, err.Error())
		return
	}
	h.svc.Sessions().SetCookie(w, sessID)
	slog.Info("login success",
		"event", "auth.login.success",
		"ip", clientIP(r),
	)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (h *authHandler) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	if c, err := r.Cookie(auth.SessionCookieName); err == nil {
		if rerr := h.svc.Sessions().Revoke(c.Value); rerr != nil {
			// 不能把 500 抛给用户（Cookie 已经要清了，抛错只会让前端卡在“点了没反应”），
			// 但**绝不能静默吞**：会话行还在库里，谁拿到那个 Cookie 值谁就还能继续用。
			// 留 Error 级证据让运维能发现（#241 审查发现）。
			// 只记 session id 的前缀：它本身就是一条 bearer 凭据，不能因日志泄露。
			slog.Error("logout: 会话删除失败（Cookie 已清但会话行可能仍存在）",
				"event", "auth.logout.revoke_failed",
				"session_prefix", sessionIDPrefix(c.Value),
				"error", rerr.Error(),
			)
		}
	}
	h.svc.Sessions().ClearCookie(w)
	slog.Info("logout",
		"event", "auth.logout",
	)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// me 返回当前登录用户（前端侧栏/顶栏显示真实用户名用）。
func (h *authHandler) me(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	uid := auth.UserIDFromContext(r.Context())
	if uid == "" {
		writeErr(w, 401, "未登录")
		return
	}
	u, err := h.svc.GetUser(uid)
	if err != nil {
		writeErr(w, 404, "用户不存在")
		return
	}
	writeJSON(w, 200, map[string]any{
		"id": u.ID, "username": u.Username, "displayName": u.DisplayName,
		"role": u.Role, "disabled": u.Disabled, "createdAt": u.CreatedAt,
	})
}

// sessionIDPrefix 取会话 id 的前缀，用于日志（会话 id 本身是 bearer 凭据，不能整条落日志）。
func sessionIDPrefix(id string) string {
	if len(id) > 12 {
		return id[:12] + "…"
	}
	return id
}

func (h *authHandler) sessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	uid := auth.UserIDFromContext(r.Context())
	if uid == "" {
		writeErr(w, 401, "未登录")
		return
	}
	limit, msg := pageLimitPlain(r)
	if msg != "" {
		writeErr(w, 400, msg)
		return
	}
	binding := "sessions|" + uid
	keys, err := decodePageCursor(r.URL.Query().Get("continue"), binding, 2)
	if err != nil {
		writeErr(w, 400, pageCursorMessage(err))
		return
	}
	afterAt, afterID := int64(0), ""
	if len(keys) == 2 {
		if afterAt, err = pageCursorInt64(keys[0]); err != nil {
			writeErr(w, 400, pageCursorMessage(err))
			return
		}
		afterID = keys[1]
	}
	list, err := h.db.ListSessionsByUserPage(r.Context(), uid, afterAt, afterID, limit+1)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	list, cont := pageCut(list, limit, binding, func(s *store.Session) []string {
		return []string{pageKeyInt64(s.CreatedAt), s.ID}
	})
	// count 是**本页**条数；`continue` 非空表示还有下一页（末页回 ""）。
	writeJSON(w, 200, map[string]any{"sessions": list, "count": len(list), "continue": cont})
}

// passkeysRoot 路由 /v1/auth/passkeys（无尾斜杠）：GET 列表 / POST 补建凭证完成。
// 补建凭证的 options 与 finish 走 /v1/auth/passkeys/register* 子路径，由 passkeysItem 处理。
func (h *authHandler) passkeysRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.listPasskeys(w, r)
	default:
		writeErr(w, 405, "method not allowed")
	}
}

// passkeysItem 路由 /v1/auth/passkeys/*：补建凭证 options/finish、重命名、删除。
func (h *authHandler) passkeysItem(w http.ResponseWriter, r *http.Request) {
	sub := strings.TrimPrefix(r.URL.Path, "/v1/auth/passkeys/")
	switch {
	case sub == "register/options" && r.Method == http.MethodPost:
		h.addPasskeyOptions(w, r)
	case sub == "register" && r.Method == http.MethodPost:
		h.addPasskeyFinish(w, r)
	case strings.Contains(sub, "/"):
		// /v1/auth/passkeys/register/options 已在上面处理；其它多段路径不接受。
		writeErr(w, 404, "unknown passkey endpoint: "+sub)
	case r.Method == http.MethodPatch:
		h.renamePasskey(w, r, sub)
	case r.Method == http.MethodDelete:
		h.deletePasskey(w, r, sub)
	default:
		writeErr(w, 405, "method not allowed")
	}
}

// ---------- Passkey 管理（已登录用户） ----------

// passkeyOut 是返回给前端的 Passkey 表示（不泄露 publicKey 原始字节）。
type passkeyOut struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	Transports     []string `json:"transports"`
	BackupEligible bool     `json:"backupEligible"`
	CreatedAt      int64    `json:"createdAt"`
	LastUsedAt     int64    `json:"lastUsedAt"`
	Current        bool     `json:"current"`
}

func toPasskeyOut(p *store.Passkey) passkeyOut {
	tr := p.Transports
	if tr == nil {
		tr = []string{}
	}
	var lastUsed int64
	if p.LastUsedAt.Valid {
		lastUsed = p.LastUsedAt.Int64
	}
	return passkeyOut{
		ID:             p.ID,
		Name:           p.Name,
		Type:           "passkey",
		Transports:     tr,
		BackupEligible: p.BackupEligible,
		CreatedAt:      p.CreatedAt,
		LastUsedAt:     lastUsed,
	}
}

// listPasskeys GET /v1/auth/passkeys —— 列当前用户的凭证。
// 返回空数组 [] 而非 null，否则前端误判 demo 模式（AGENTS.md 第 3 节）。
func (h *authHandler) listPasskeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	uid := auth.UserIDFromContext(r.Context())
	if uid == "" {
		writeErr(w, 401, "未登录")
		return
	}
	limit, msg := pageLimitPlain(r)
	if msg != "" {
		writeErr(w, 400, msg)
		return
	}
	binding := "passkeys|" + uid
	keys, err := decodePageCursor(r.URL.Query().Get("continue"), binding, 2)
	if err != nil {
		writeErr(w, 400, pageCursorMessage(err))
		return
	}
	afterAt, afterID := int64(0), ""
	if len(keys) == 2 {
		if afterAt, err = pageCursorInt64(keys[0]); err != nil {
			writeErr(w, 400, pageCursorMessage(err))
			return
		}
		afterID = keys[1]
	}
	recs, err := h.db.ListPasskeysByUserPage(r.Context(), uid, afterAt, afterID, limit+1)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	recs, cont := pageCut(recs, limit, binding, func(p *store.Passkey) []string {
		return []string{pageKeyInt64(p.CreatedAt), p.ID}
	})
	out := make([]passkeyOut, 0, len(recs))
	for _, p := range recs {
		out = append(out, toPasskeyOut(p))
	}
	// count 是**本页**条数；`continue` 非空表示还有下一页（末页回 ""）。
	writeJSON(w, 200, map[string]any{"passkeys": out, "count": len(out), "continue": cont})
}

// addPasskeyOptions POST /v1/auth/passkeys/register/options —— 已登录用户补建凭证。
// 与注册新用户的 /register/options 区分：不传 username，从会话取 userID。
func (h *authHandler) addPasskeyOptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	uid := auth.UserIDFromContext(r.Context())
	if uid == "" {
		writeErr(w, 401, "未登录")
		return
	}
	creation, err := h.svc.BeginAddCredential(uid)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, creation)
}

// addPasskeyFinish POST /v1/auth/passkeys/register —— 完成补建凭证。
// body: 裸 WebAuthn attestation JSON（{id,rawId,type,response:{clientDataJSON,attestationObject}}）。
// name 从 query ?name= 取（可选，默认「新设备」），与注册新用户的 ?username= 风格一致。
// 注意：不能预读 body，go-webauthn 的 FinishRegistration 直接从 r.Body 解析。
func (h *authHandler) addPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	uid := auth.UserIDFromContext(r.Context())
	if uid == "" {
		writeErr(w, 401, "未登录")
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "新设备"
	}
	if err := h.svc.FinishAddCredential(uid, name, r); err != nil {
		slog.Warn("passkey add failed",
			"event", "auth.passkey.add_fail",
			"user_id", uid,
			"error", err.Error(),
		)
		writeErr(w, 400, err.Error())
		return
	}
	slog.Info("passkey created",
		"event", "auth.passkey.created",
		"user_id", uid,
	)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// renamePasskey PATCH /v1/auth/passkeys/{id} —— 重命名。
// 越权（改别人的凭证）应返回 404，不泄露存在性。
func (h *authHandler) renamePasskey(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPatch {
		writeErr(w, 405, "method not allowed")
		return
	}
	uid := auth.UserIDFromContext(r.Context())
	if uid == "" {
		writeErr(w, 401, "未登录")
		return
	}
	// 先校验凭证属于当前用户（越权转 404）。
	p, err := h.svc.GetPasskey(id)
	if err != nil || p.UserID != uid {
		writeErr(w, 404, "凭证不存在")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil || req.Name == "" {
		writeErr(w, 400, "缺少 name")
		return
	}
	if err := h.svc.RenamePasskey(id, req.Name); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	slog.Info("passkey renamed",
		"event", "auth.passkey.renamed",
		"user_id", uid,
		"passkey_id", id,
	)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// deletePasskey DELETE /v1/auth/passkeys/{id} —— 删除。
// 越权（删别人的凭证）转 404。删最后一个时的判定见下面的不变量（有密码才允许）。
func (h *authHandler) deletePasskey(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodDelete {
		writeErr(w, 405, "method not allowed")
		return
	}
	uid := auth.UserIDFromContext(r.Context())
	if uid == "" {
		writeErr(w, 401, "未登录")
		return
	}
	p, err := h.svc.GetPasskey(id)
	if err != nil || p.UserID != uid {
		writeErr(w, 404, "凭证不存在")
		return
	}
	// 删的如果是最后一个凭证，就必须确认还有另一种登录方式（已设置密码），
	// 否则删完只能靠恢复码兜底。
	list, err := h.svc.ListPasskeys(uid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if len(list) <= 1 {
		// 不变量：每个账号至少保留一种登录方式（≥1 Passkey **或**已设置密码）。
		//
		// 老实现无条件拒绝删最后一个 Passkey，那在只有 Passkey 的年代是对的；
		// 现在密码也是合法登录方式——“有密码”时删掉最后一个 Passkey 不能把人锁在外面。
		// 失败方向选 fail-closed：读不到密码状态就当作没有（宁可拒删，不可锁号）。
		allowed := false
		st, perr := h.svc.PasswordStatus(uid)
		switch {
		case perr != nil:
			slog.Warn("passkey delete blocked: last credential and password status unreadable",
				"event", "auth.passkey.delete_last_blocked",
				"user_id", uid,
				"passkey_id", id,
				"error", perr.Error(),
			)
		case st == nil || !st.HasPassword:
			slog.Warn("passkey delete blocked: last credential and no password",
				"event", "auth.passkey.delete_last_blocked",
				"user_id", uid,
				"passkey_id", id,
			)
		default:
			allowed = true
		}
		if !allowed {
			writeErr(w, 409, "至少保留一种登录方式：请先设置密码，再删除最后一个 Passkey。")
			return
		}
	}
	if err := h.svc.DeletePasskey(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	slog.Info("passkey deleted",
		"event", "auth.passkey.deleted",
		"user_id", uid,
		"passkey_id", id,
	)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 密码登录（第二套登录模型，契约 §2/§3） ----------

// passwordLogin POST /v1/auth/password/login —— username + password → 会话 Cookie。
//
// 匿名端点，双维度限速（10/min/IP + 10/min/username）：与恢复码登录同一口径。
// 两个维度都放行才继续——IP 维管单机狂刷，username 维管分布式撞库。
// 日志不得包含明文或哈希（这里只记 ip 与错误文案）。
func (h *authHandler) passwordLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	// IP 限速先于读 body：挡住空请求洪泛（不解析 JSON、不打 argon2）。
	if rateLimited(w, rlPasswordLoginIP, clientIP(r)) {
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil || req.Username == "" || req.Password == "" {
		writeErr(w, 400, "缺少 username 或 password")
		return
	}
	// per-username 限流：key 小写，大小写变体不能换额度。
	if rateLimited(w, rlPasswordLoginUser, strings.ToLower(req.Username)) {
		return
	}
	sessID, err := h.svc.LoginWithPassword(req.Username, req.Password, auth.DeviceNameFromUA(r.UserAgent()))
	if err != nil {
		// 文案不区分「用户不存在 / 密码错 / 未设置密码」以外的细节；也不记 username/明文。
		slog.Warn("password login failed",
			"event", "auth.password.login_fail",
			"ip", clientIP(r),
			"error", err.Error(),
		)
		writeErr(w, 401, err.Error())
		return
	}
	h.svc.Sessions().SetCookie(w, sessID)
	slog.Info("password login success",
		"event", "auth.password.login_success",
		"ip", clientIP(r),
	)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// passwordRegister POST <password>/register —— 用用户名 + 密码注册（契约「追加：用密码注册」§A2）。
//
// 匿名端点，**显式路由**（mux.go），因为 /v1/auth/ 的兜底 handler 根本不覆盖
// 与本路径同族的前缀；不显式挂的话它会掉到静态资源里去（SPA 404）。
//
// 限速：只做 IP 维（10/min/IP）——注册不是秘密猜测接口，username 维留给登录；
// 但它是**一条会算 argon2 与签发 8 个恢复码的匿名写入口**，没有 IP 维就是一个
// 现成的“灌号 + 烧 CPU”放大镜。IP 维先于读 body（与 recovery/password login 一致）：
// 空请求洪泛连 JSON 都不解析。
//
// 日志：auth.register.success / auth.register.fail（字段与 Passkey 注册同一形状：只有
// 事件名 + ip + 错误文案；**绝不含 username / 密码 / 哈希**）。
func (h *authHandler) passwordRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	// IP 限速先于读 body：挡住空请求洪泛（不解析 JSON、不打 argon2）。
	if rateLimited(w, rlPasswordRegisterIP, clientIP(r)) {
		return
	}
	var req struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		DisplayName string `json:"displayName"`
	}
	if err := readJSON(r, &req); err != nil || req.Username == "" || req.Password == "" {
		writeErr(w, 400, "缺少 username 或 password")
		return
	}
	sessID, codes, err := h.svc.RegisterWithPassword(
		r.Context(), req.Username, req.DisplayName, req.Password, auth.DeviceNameFromUA(r.UserAgent()))
	if err != nil {
		// 文案原样回给前端（用户名已存在 / 密码太短 / 用户名不合法都由 auth 层给定）；
		// 不记 username、不记明文、不记哈希。
		slog.Warn("password register failed",
			"event", "auth.register.fail",
			"ip", clientIP(r),
			"error", err.Error(),
		)
		writeErr(w, 400, err.Error())
		return
	}
	h.svc.Sessions().SetCookie(w, sessID)
	slog.Info("password register success",
		"event", "auth.register.success",
		"ip", clientIP(r),
	)
	// recoveryCodes 明文仅此一次；空时回 [] 而不是 null（AGENTS.md 第 3 节）。
	if codes == nil {
		codes = []string{}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "recoveryCodes": codes})
}

// password GET / POST / DELETE /v1/auth/password —— 密码管理（需登录会话）。
//
// 路由在 mux.go 里包了 sessMW，所以未登录在这里已经 401（不会走到这个方法）。
// 方法分发留在本方法内：三条语义共享“同一个资源”，拆成三个 handler 只会多两处拷贝。
func (h *authHandler) password(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserIDFromContext(r.Context())
	if uid == "" {
		writeErr(w, 401, "未登录")
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.passwordStatus(w, uid)
	case http.MethodPost:
		h.passwordSet(w, r, uid)
	case http.MethodDelete:
		h.passwordRemove(w, r, uid)
	default:
		writeErr(w, 405, "method not allowed")
	}
}

// passwordStatus GET /v1/auth/password —— {hasPassword, updatedAt}。
// 不返回哈希（也不返回前缀）：密码的存在性对前端已足够。
func (h *authHandler) passwordStatus(w http.ResponseWriter, uid string) {
	st, err := h.svc.PasswordStatus(uid)
	if err != nil {
		// 会话中间件已经验过用户存在，走到这里基本是库故障 → 500（不伪造“未设置”）。
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"hasPassword": st.HasPassword,
		"updatedAt":   st.UpdatedAt,
	})
}

// passwordSet POST /v1/auth/password —— 设置/修改密码。
// body: {currentPassword, newPassword}；已设置密码时必须带对的 currentPassword。
func (h *authHandler) passwordSet(w http.ResponseWriter, r *http.Request, uid string) {
	var req struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := readJSON(r, &req); err != nil || req.NewPassword == "" {
		writeErr(w, 400, "缺少 newPassword")
		return
	}
	st, err := h.svc.SetPassword(uid, req.CurrentPassword, req.NewPassword)
	if err != nil {
		// 契约枚举的失败面只有这个：新密码不合规 / 当前密码错（都是 400）。
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":          true,
		"hasPassword": st.HasPassword,
		"updatedAt":   st.UpdatedAt,
	})
}

// passwordRemove DELETE /v1/auth/password —— 移除密码。
// body: {currentPassword}；未设置密码时幂等成功（hasPassword=false）。
// 没有 Passkey 时不得移除（否则账号再没有任何登录方式）→ 409。
func (h *authHandler) passwordRemove(w http.ResponseWriter, r *http.Request, uid string) {
	var req struct {
		CurrentPassword string `json:"currentPassword"`
	}
	// body 可缺：未设置密码时移除是幂等的，前端可能根本不带 body。
	_ = readJSON(r, &req)
	if err := h.svc.RemovePassword(uid, req.CurrentPassword); err != nil {
		if errors.Is(err, auth.ErrLastAuthMethod) {
			writeErr(w, 409, err.Error())
			return
		}
		// 当前密码错 / 用户不存在 / 数据异常：统一 400（契约对 DELETE 只定义了 400/409）。
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "hasPassword": false})
}

// ---------- 恢复码 ----------

// recoveryLogin POST /v1/auth/recovery/login —— username + code → 会话 Cookie；成功后该码作废。
// 限速：10/min/IP + 10/min/username（小写）；超限 429 + Retry-After: 60（见 ratelimit.go）。
func (h *authHandler) recoveryLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	// IP 限速先于读 body，挡住空请求洪泛。
	if rateLimited(w, rlRecoveryLoginIP, clientIP(r)) {
		return
	}
	var req struct {
		Username string `json:"username"`
		Code     string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil || req.Username == "" || req.Code == "" {
		writeErr(w, 400, "缺少 username 或 code")
		return
	}
	// per-username 防分布式撞库；key 小写，避免大小写变体绕过。
	if rateLimited(w, rlRecoveryLoginUser, strings.ToLower(req.Username)) {
		return
	}
	sessID, err := h.svc.LoginWithRecoveryCode(req.Username, req.Code, auth.DeviceNameFromUA(r.UserAgent()))
	if err != nil {
		slog.Warn("recovery login failed",
			"event", "auth.recovery.login_fail",
			"ip", clientIP(r),
			"error", err.Error(),
		)
		// 账户禁用用 401；其余统一 401（不区分用户/码错误）。
		writeErr(w, 401, err.Error())
		return
	}
	h.svc.Sessions().SetCookie(w, sessID)
	slog.Info("recovery login success",
		"event", "auth.recovery.login_success",
		"ip", clientIP(r),
	)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// recoveryRegenerate POST /v1/auth/recovery/regenerate —— 需登录；作废旧码、签发新集合，明文仅返回一次。
func (h *authHandler) recoveryRegenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	uid := auth.UserIDFromContext(r.Context())
	if uid == "" {
		writeErr(w, 401, "未登录")
		return
	}
	codes, err := h.svc.RegenerateRecoveryCodes(uid)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if codes == nil {
		codes = []string{}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "recoveryCodes": codes})
}

// recoveryStatus GET /v1/auth/recovery/status —— 需登录；返回未用码数量（不含明文）。
func (h *authHandler) recoveryStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	uid := auth.UserIDFromContext(r.Context())
	if uid == "" {
		writeErr(w, 401, "未登录")
		return
	}
	n, err := h.svc.RecoveryStatus(uid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"unusedCount": n, "hasCodes": n > 0})
}
