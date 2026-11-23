package server

// resource_secret_test.go 是 #554 的验收面：凭据 CRUD 的四条端点 + 六条语义。
//
// 夹具口径与 resource_source_test.go 一致：真 SQLite + 真 store + 真 route/source service，
// 只把会话中间件换成「直接往 context 里放登录用户」——被替换的那一层不参与任何被测逻辑。
// 轮换「立即生效」用**真 Poller.Sweep** 打真 httptest 上游（断言请求头），不用注释声称。
//
// 每条用例点名它守的验收号（issue #554「验收」1–7）。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/store"
)

const secretResClock = 1_700_000_000

type secretResEnv struct {
	t      *testing.T
	db     *store.DB
	mux    http.Handler
	svc    *source.Service
	nsID   string
	userID string
}

func newSecretResEnv(t *testing.T) *secretResEnv {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "secret.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if cerr := db.Close(); cerr != nil {
			t.Errorf("close db: %v", cerr)
		}
	})
	userID, err := db.InsertUser(ctx, "", "secuser", "Secret User")
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	ns, err := db.CreateNamespace(ctx, "default", "默认")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	if err := db.CreateCapability(ctx, &store.CapabilityVersion{
		NamespaceID: ns.ID,
		Name:        "notify",
		Version:     1,
		Spec: artifact.Spec{
			Params: json.RawMessage(`{"type":"object"}`),
			Run:    artifact.Run{Entrypoint: "run.sh", Files: map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"}},
		},
	}); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	router := route.New(db)
	svc := source.NewWithClock(db, router, func() int64 { return secretResClock })
	deps := &serverDeps{
		DB:      db,
		Router:  router,
		Sources: svc,
		Now:     func() int64 { return secretResClock * 1000 },
		Session: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(auth.WithUserID(r.Context(), userID)))
			})
		},
	}
	mux := http.NewServeMux()
	newSecretAPI(deps).register(mux)
	newSourceAPI(deps).register(mux)
	return &secretResEnv{t: t, db: db, mux: mux, svc: svc, nsID: ns.ID, userID: userID}
}

func (e *secretResEnv) secretsPath() string { return "/v1/namespaces/" + e.nsID + "/secrets" }

func (e *secretResEnv) secretPath(name string) string { return e.secretsPath() + "/" + name }

func (e *secretResEnv) sourcesPath() string { return "/v1/namespaces/" + e.nsID + "/sources" }

func (e *secretResEnv) request(method, target string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader io.Reader
	switch v := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(v)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			e.t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, httptest.NewRequest(method, target, reader))
	return rr
}

func (e *secretResEnv) doc(rr *httptest.ResponseRecorder) map[string]any {
	e.t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		e.t.Fatalf("响应不是 JSON 对象（%d）：%s", rr.Code, rr.Body.String())
	}
	return out
}

// putSecret 走 HTTP 写一个凭据，返回读投影文档。
func (e *secretResEnv) putSecret(name, value string) map[string]any {
	e.t.Helper()
	rr := e.request(http.MethodPut, e.secretPath(name), map[string]any{"value": value})
	if rr.Code != http.StatusOK {
		e.t.Fatalf("PUT 凭据期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), value) {
		e.t.Fatalf("PUT 响应回显了值：#554 语义 1 要求值永不回显：%s", rr.Body.String())
	}
	return e.doc(rr)
}

// createPollSource 走 HTTP 建一个 poll 源（config 由调用方给）。
func (e *secretResEnv) createPollSource(name string, config map[string]any) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.request(http.MethodPost, e.sourcesPath(), map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"kind":    "poll",
			"config":  config,
			"targets": []any{map[string]any{"capability": "notify"}},
		},
	})
}

func (e *secretResEnv) auditRows(name string) []store.NamespaceSecretAuditEntry {
	e.t.Helper()
	rows, err := e.db.ListNamespaceSecretAudit(context.Background(), e.nsID, name, 50)
	if err != nil {
		e.t.Fatalf("read audit: %v", err)
	}
	return rows
}

// expectCauseField 断言 400 的 details.causes[] 里有指向该字段的一条。
func expectCauseField(t *testing.T, rr *httptest.ResponseRecorder, field string) map[string]any {
	t.Helper()
	var body struct {
		Reason  string `json:"reason"`
		Details struct {
			Causes []struct {
				Field   string `json:"field"`
				Message string `json:"message"`
			} `json:"causes"`
		} `json:"details"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误体不是 Status：%s", rr.Body.String())
	}
	for _, c := range body.Details.Causes {
		if c.Field == field {
			return map[string]any{"reason": body.Reason, "message": c.Message}
		}
	}
	t.Fatalf("details.causes[] 里没有 field=%q 的一条：%s", field, rr.Body.String())
	return nil
}

// TestSecretResourceReadFaceHasNoValue 守验收 4 的读面一半 + 语义 1/5/6：
// 四条端点里只有 name/updatedAt/usedBy，审计只记 name/action/actor。
func TestSecretResourceReadFaceHasNoValue(t *testing.T) {
	env := newSecretResEnv(t)
	const v1 = "tok-first-9f1"
	const v2 = "tok-rotated-2b7"

	created := env.putSecret("gh-token", v1)
	if created["name"] != "gh-token" || created["updatedAt"] == "" {
		t.Fatalf("读投影缺 name/updatedAt: %v", created)
	}
	if usedBy, ok := created["usedBy"].([]any); !ok || len(usedBy) != 0 {
		t.Fatalf("未引用时 usedBy 必须是 []: %v", created["usedBy"])
	}
	if _, ok := created["value"]; ok {
		t.Fatal("读投影里出现了 value 键")
	}

	list := env.request(http.MethodGet, env.secretsPath(), nil)
	if list.Code != http.StatusOK {
		t.Fatalf("列表期望 200，得到 %d：%s", list.Code, list.Body.String())
	}
	if strings.Contains(list.Body.String(), v1) {
		t.Fatalf("列表回显了值：%s", list.Body.String())
	}
	var listDoc struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listDoc); err != nil || len(listDoc.Items) != 1 {
		t.Fatalf("列表形状不对（%v）：%s", err, list.Body.String())
	}

	single := env.request(http.MethodGet, env.secretPath("gh-token"), nil)
	if single.Code != http.StatusOK || strings.Contains(single.Body.String(), v1) {
		t.Fatalf("单条读回显了值或状态不对（%d）：%s", single.Code, single.Body.String())
	}

	// 轮换：响应仍不含新值，store 里是真新值。
	env.putSecret("gh-token", v2)
	if got, err := env.db.GetNamespaceSecret(context.Background(), env.nsID, "gh-token"); err != nil || got != v2 {
		t.Fatalf("轮换后 store 里必须是新值：%q %v", got, err)
	}

	rows := env.auditRows("gh-token")
	if len(rows) != 2 || rows[0].Action != store.NamespaceSecretRotated || rows[1].Action != store.NamespaceSecretCreated {
		t.Fatalf("创建/轮换各一条审计: %+v", rows)
	}
	for _, row := range rows {
		if row.ActorID != env.userID {
			t.Fatalf("审计必须记 actor: %+v", row)
		}
		if strings.Contains(row.Name, v1) || strings.Contains(row.Name, v2) || strings.Contains(row.ActorID, v1) {
			t.Fatalf("审计行不许含值: %+v", row)
		}
	}
}

// TestSecretResourceDeleteGuardListsReferencingSources 守验收 3 + 语义 3。
func TestSecretResourceDeleteGuardListsReferencingSources(t *testing.T) {
	env := newSecretResEnv(t)
	env.putSecret("gh-token", "tok-1")
	cfg := map[string]any{
		"url":      "https://example.com/feed",
		"schedule": map[string]any{"expr": "*/5 * * * *"},
		"secretRef": map[string]any{
			"name": "gh-token", "header": "Authorization", "prefix": "Bearer ",
		},
	}
	if rr := env.createPollSource("poll-src", cfg); rr.Code != http.StatusCreated {
		t.Fatalf("按 secretRef 建源期望 201，得到 %d：%s", rr.Code, rr.Body.String())
	}
	single := env.request(http.MethodGet, env.secretPath("gh-token"), nil)
	if !strings.Contains(single.Body.String(), "poll-src") {
		t.Fatalf("usedBy 必须列出引用它的源名：%s", single.Body.String())
	}

	blocked := env.request(http.MethodDelete, env.secretPath("gh-token"), nil)
	if blocked.Code != http.StatusConflict {
		t.Fatalf("被引用时 DELETE 期望 409，得到 %d：%s", blocked.Code, blocked.Body.String())
	}
	var conflict struct {
		Reason  string `json:"reason"`
		Details struct {
			Causes []struct {
				Field   string `json:"field"`
				Message string `json:"message"`
			} `json:"causes"`
		} `json:"details"`
	}
	if err := json.Unmarshal(blocked.Body.Bytes(), &conflict); err != nil {
		t.Fatalf("409 错误体不是 Status：%s", blocked.Body.String())
	}
	if conflict.Reason != reasonSecretInUse {
		t.Fatalf("409 reason 必须是 %q，得到 %q", reasonSecretInUse, conflict.Reason)
	}
	if len(conflict.Details.Causes) != 1 || !strings.Contains(conflict.Details.Causes[0].Message, "poll-src") {
		t.Fatalf("causes 必须列引用它的源名：%s", blocked.Body.String())
	}
	if rr := env.request(http.MethodGet, env.secretPath("gh-token"), nil); rr.Code != http.StatusOK {
		t.Fatalf("拒删不得删掉凭据：%d %s", rr.Code, rr.Body.String())
	}

	// 先改源的 secretRef（PATCH 对 poll 是整份配置替换），再删。
	cur := env.request(http.MethodGet, env.sourcesPath()+"/poll-src", nil)
	if cur.Code != http.StatusOK {
		t.Fatalf("读源取 CAS 令牌失败：%d %s", cur.Code, cur.Body.String())
	}
	rv, _ := env.doc(cur)["metadata"].(map[string]any)["resourceVersion"].(string)
	patch := map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   map[string]any{"name": "poll-src", "resourceVersion": rv},
		"spec": map[string]any{
			"kind": "poll",
			"config": map[string]any{
				"url":      "https://example.com/feed",
				"schedule": map[string]any{"expr": "*/5 * * * *"},
			},
		},
	}
	if rr := env.request(http.MethodPatch, env.sourcesPath()+"/poll-src", patch); rr.Code != http.StatusOK {
		t.Fatalf("改源 secretRef 期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	ok := env.request(http.MethodDelete, env.secretPath("gh-token"), nil)
	if ok.Code != http.StatusNoContent {
		t.Fatalf("没人引用后 DELETE 期望 204，得到 %d：%s", ok.Code, ok.Body.String())
	}
	if missing := env.request(http.MethodGet, env.secretPath("gh-token"), nil); missing.Code != http.StatusNotFound ||
		!strings.Contains(missing.Body.String(), reasonSecretAbsent) {
		t.Fatalf("删除后读单条必须 404 %s：%d %s", reasonSecretAbsent, missing.Code, missing.Body.String())
	}
	if again := env.request(http.MethodDelete, env.secretPath("gh-token"), nil); again.Code != http.StatusNotFound {
		t.Fatalf("删不存在的凭据必须 404：%d %s", again.Code, again.Body.String())
	}
	if rows := env.auditRows("gh-token"); len(rows) != 2 || rows[0].Action != store.NamespaceSecretDeleted {
		t.Fatalf("删除也要留一条审计: %+v", rows)
	}
}

// TestSecretResourceNameRuleSharedWithSecretRef 守验收 6 + 语义 5：非法名字 400 且 field=name，
// 同一串名字在 config.secretRef 上被判非法（同一个校验函数的两个调用点）。
func TestSecretResourceNameRuleSharedWithSecretRef(t *testing.T) {
	env := newSecretResEnv(t)
	for _, name := range []string{"-bad", "has%20space", strings.Repeat("a", 65)} {
		rr := env.request(http.MethodPut, env.secretPath(name), map[string]any{"value": "v"})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%q 期望 400，得到 %d：%s", name, rr.Code, rr.Body.String())
		}
		cause := expectCauseField(t, rr, "name")
		if !strings.Contains(cause["message"].(string), "只允许") && !strings.Contains(cause["message"].(string), "最长") &&
			!strings.Contains(cause["message"].(string), "必填") {
			t.Fatalf("%q 的 cause 文案不像名字规则：%v", name, cause)
		}

		bad := env.createPollSource("src-bad", map[string]any{
			"url":       "https://example.com/feed",
			"schedule":  map[string]any{"expr": "*/5 * * * *"},
			"secretRef": map[string]any{"name": name, "header": "Authorization"},
		})
		if bad.Code != http.StatusBadRequest {
			t.Fatalf("config.secretRef.name=%q 期望 400，得到 %d：%s", name, bad.Code, bad.Body.String())
		}
		if !strings.Contains(bad.Body.String(), "secretRef") {
			t.Fatalf("老路径的 cause 要点到 config.secretRef：%s", bad.Body.String())
		}
	}
}

// TestSecretResourceWriteBodyIsStrict 守 PUT 的入参边界（未知字段 / 缺 value / 空值 / 头注入）。
func TestSecretResourceWriteBodyIsStrict(t *testing.T) {
	env := newSecretResEnv(t)
	cases := []struct {
		name string
		body any
		want int
		code string
	}{
		{"未知字段", map[string]any{"value": "v", "extra": 1}, http.StatusBadRequest, reasonUnknownField},
		{"缺 value", map[string]any{}, http.StatusBadRequest, reasonValueInvalid},
		{"空 value", map[string]any{"value": ""}, http.StatusBadRequest, reasonValueInvalid},
		{"CRLF", map[string]any{"value": "a\r\nb"}, http.StatusBadRequest, reasonValueInvalid},
	}
	for _, tc := range cases {
		rr := env.request(http.MethodPut, env.secretPath("gh-token"), tc.body)
		if rr.Code != tc.want {
			t.Fatalf("%s 期望 %d，得到 %d：%s", tc.name, tc.want, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), tc.code) {
			t.Fatalf("%s 的 reason 期望 %q：%s", tc.name, tc.code, rr.Body.String())
		}
	}
	if rr := env.request(http.MethodGet, env.secretPath("gh-token"), nil); rr.Code != http.StatusNotFound {
		t.Fatalf("被拒的写不该落库：%d %s", rr.Code, rr.Body.String())
	}
}

// TestSecretResourceOldPathStillCreates 守验收 5：config.secret + secretRef 一次写入仍能创建，
// 且用同一份 store 写入（读面看得见它、usedBy 指向那个源）。
func TestSecretResourceOldPathStillCreates(t *testing.T) {
	env := newSecretResEnv(t)
	const legacy = "legacy-tok-77"
	rr := env.createPollSource("legacy-src", map[string]any{
		"url":       "https://example.com/feed",
		"schedule":  map[string]any{"expr": "*/5 * * * *"},
		"secretRef": map[string]any{"name": "gh", "header": "Authorization", "prefix": "Bearer "},
		"secret":    legacy,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("老路径期望 201，得到 %d：%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), legacy) {
		t.Fatalf("源响应回显了 config.secret：%s", rr.Body.String())
	}
	list := env.request(http.MethodGet, env.secretsPath(), nil)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"gh"`) ||
		!strings.Contains(list.Body.String(), "legacy-src") {
		t.Fatalf("老路径建的凭据要出现在读面并带引用者：%d %s", list.Code, list.Body.String())
	}
	if value, err := env.db.GetNamespaceSecret(context.Background(), env.nsID, "gh"); err != nil || value != legacy {
		t.Fatalf("老路径落库值不一致：%q %v", value, err)
	}
}

// TestSecretResourceRotationTakesEffectOnNextPoll 守验收 1 + 2：PUT 之后真 Poller 的下一轮
// 请求头就是新值（旧值不再出现）。这是「轮换不必动源」的实证。
func TestSecretResourceRotationTakesEffectOnNextPoll(t *testing.T) {
	env := newSecretResEnv(t)
	const v1 = "token-one-31"
	const v2 = "token-two-42"

	headers := make(chan string, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[{"id":"a","type":"advisory"}]`))
	}))
	defer upstream.Close()

	// 验收 1：先 PUT 凭据，再建只带 secretRef 的源（老路径不参与）。
	env.putSecret("gh-token", v1)
	created := env.createPollSource("poll-src", map[string]any{
		"url":           upstream.URL,
		"schedule":      map[string]any{"expr": "*/5 * * * *"},
		"allowInsecure": true,
		"secretRef":     map[string]any{"name": "gh-token", "header": "Authorization", "prefix": "Bearer "},
		"envelope":      map[string]any{"items": ".", "type": map[string]any{"pointer": "type"}, "deliveryId": map[string]any{"pointer": "id"}},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("建 poll 源期望 201，得到 %d：%s", created.Code, created.Body.String())
	}
	src, err := env.db.GetSourceByName(context.Background(), env.nsID, "poll-src")
	if err != nil || src.NextFireAt == nil {
		t.Fatalf("回读源失败（nextFireAt 必须有）：%v %+v", err, src)
	}
	now := time.Unix(*src.NextFireAt, 0)

	// 每一轮用**独立**的 Poller：共用实例靠 begin() 去重，上一轮未收尾会被记成 skippedOverlaps；
	// 收尾判据是 LastPollAt 真的变了（读到上一轮的值会被当成「这轮已完成」，那不是证据）。
	round := func(want string, prevLastPoll *int64) *int64 {
		t.Helper()
		poller := source.NewPoller(env.db, env.svc, nil, source.WithClock(func() time.Time { return now }))
		if n, err := poller.Sweep(context.Background()); err != nil || n != 1 {
			t.Fatalf("Sweep 应起 1 轮（%d, %v）——源必须到点", n, err)
		}
		select {
		case got := <-headers:
			if got != "Bearer "+want {
				t.Fatalf("下一轮 poll 的请求头 = %q，want %q", got, "Bearer "+want)
			}
			if want == v2 && strings.Contains(got, v1) {
				t.Fatalf("轮换后旧值仍出现在请求头里：%q", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("上游没收到 poll 请求")
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			got, err := env.db.GetSourceByName(context.Background(), env.nsID, "poll-src")
			if err != nil {
				t.Fatalf("回读源: %v", err)
			}
			if got.LastPollAt != nil && (prevLastPoll == nil || *got.LastPollAt != *prevLastPoll) {
				last := *got.LastPollAt
				return &last
			}
			if time.Now().After(deadline) {
				t.Fatalf("这一轮没跑完：%+v", got)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	first := round(v1, nil)
	env.putSecret("gh-token", v2)
	now = now.Add(5 * time.Minute)
	second := round(v2, first)
	if *second == *first {
		t.Fatalf("两轮的时间戳相同（%d）—— 收尾判据退化，这不是证据", *second)
	}
}

// TestSecretResourcePlaintextNeverLeaks 守验收 4：全流程（成功/失败/错误体/日志/审计）里
// grep 不到任何明文值。
func TestSecretResourcePlaintextNeverLeaks(t *testing.T) {
	var logBuf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	env := newSecretResEnv(t)
	const v1 = "plain-v1-9c2a"
	const v2 = "plain-v2-4b8e"
	const legacy = "plain-legacy-71f3"

	var bodies bytes.Buffer
	keep := func(rr *httptest.ResponseRecorder) *httptest.ResponseRecorder {
		bodies.WriteString(rr.Body.String())
		bodies.WriteString("\n")
		return rr
	}
	keep(env.request(http.MethodPut, env.secretPath("gh-token"), map[string]any{"value": v1}))
	keep(env.request(http.MethodPut, env.secretPath("gh-token"), map[string]any{"value": v2}))
	keep(env.request(http.MethodGet, env.secretsPath(), nil))
	keep(env.request(http.MethodGet, env.secretPath("gh-token"), nil))
	keep(env.request(http.MethodPut, env.secretPath("-bad"), map[string]any{"value": v1}))
	keep(env.request(http.MethodPut, env.secretPath("gh-token"), map[string]any{"value": ""}))
	keep(env.createPollSource("poll-src", map[string]any{
		"url":       "https://example.com/feed",
		"schedule":  map[string]any{"expr": "*/5 * * * *"},
		"secretRef": map[string]any{"name": "gh-token", "header": "Authorization"},
	}))
	keep(env.request(http.MethodDelete, env.secretPath("gh-token"), nil))
	keep(env.createPollSource("legacy-src", map[string]any{
		"url":       "https://example.com/other",
		"schedule":  map[string]any{"expr": "*/5 * * * *"},
		"secretRef": map[string]any{"name": "legacy", "header": "Authorization"},
		"secret":    legacy,
	}))

	for _, value := range []string{v1, v2, legacy} {
		if strings.Contains(bodies.String(), value) {
			t.Fatalf("响应体里出现了明文值 %q：\n%s", value, bodies.String())
		}
		if strings.Contains(logBuf.String(), value) {
			t.Fatalf("日志里出现了明文值 %q：\n%s", value, logBuf.String())
		}
	}
	var auditDump strings.Builder
	rows, err := env.db.Query(`SELECT name || action || actor_id FROM namespace_secret_audit`)
	if err != nil {
		t.Fatalf("dump audit: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan audit: %v", err)
		}
		auditDump.WriteString(line)
	}
	for _, value := range []string{v1, v2, legacy} {
		if strings.Contains(auditDump.String(), value) {
			t.Fatalf("审计里出现了明文值 %q：%s", value, auditDump.String())
		}
	}
}
