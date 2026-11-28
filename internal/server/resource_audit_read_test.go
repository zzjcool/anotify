package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

type auditReadEnv struct {
	t      *testing.T
	db     *store.DB
	mux    http.Handler
	cookie *http.Cookie
	userID string
	nsA    string
	nsB    string
}

func newAuditReadEnv(t *testing.T) *auditReadEnv {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "audit-read.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if cerr := db.Close(); cerr != nil {
			t.Errorf("close db: %v", cerr)
		}
	})

	user := &store.User{
		ID:          store.NewUserID(),
		Username:    "audit-reader",
		DisplayName: "审计读测试用户",
		CreatedAt:   store.Now(),
	}
	defaultNS, err := db.RegisterUser(ctx, user)
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	secondNS, err := db.CreateNamespace(ctx, "audit-read-b", "第二个命名空间")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	sm := auth.NewSessionManager(db, 0, false)
	sess, err := sm.Create(user.ID, "audit · read")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	deps := &serverDeps{DB: db, Session: sm.Middleware}
	mux := http.NewServeMux()
	newSourceAPI(deps).register(mux)
	newSecretAPI(deps).register(mux)

	return &auditReadEnv{
		t: t, db: db, mux: mux,
		cookie: &http.Cookie{Name: auth.SessionCookieName, Value: sess.Token},
		userID: user.ID,
		nsA:    defaultNS.ID, nsB: secondNS.ID,
	}
}

func (e *auditReadEnv) get(target string, withSession bool) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if withSession {
		req.AddCookie(e.cookie)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e *auditReadEnv) sourceAuditPath(ns, name string) string {
	return "/v1/namespaces/" + ns + "/sources/" + name + "/audit"
}

func (e *auditReadEnv) secretAuditPath(ns, name string) string {
	return "/v1/namespaces/" + ns + "/secrets/" + name + "/audit"
}

func (e *auditReadEnv) createPollSource(nsID, name string) *store.Source {
	e.t.Helper()
	src := &store.Source{
		NamespaceID: nsID,
		Name:        name,
		Kind:        store.SourceKindPoll,
		Config:      json.RawMessage(`{"url":"https://example.com/feed","schedule":{"expr":"*/5 * * * *"}}`),
		Enabled:     true,
		Targets:     []sourcespec.Target{},
	}
	if err := e.db.CreateSource(context.Background(), src); err != nil {
		e.t.Fatalf("建 poll 源 %s: %v", name, err)
	}
	return src
}

func (e *auditReadEnv) appendSourceAudit(nsID, sourceID string, at int64) {
	e.t.Helper()
	if _, err := e.db.AppendSourceAudit(context.Background(), store.SourceAuditInput{
		NamespaceID: nsID, SourceID: sourceID, Action: store.SourceAuditPolledManually,
		ActorID: e.userID, Detail: json.RawMessage(`{"ok":true,"items":2,"created":2,"duplicates":0}`), Now: at,
	}); err != nil {
		e.t.Fatalf("追加源审计: %v", err)
	}
}

func (e *auditReadEnv) putSecret(nsID, name, value string) {
	e.t.Helper()
	if _, err := e.db.PutNamespaceSecret(context.Background(), nsID, name, value, e.userID); err != nil {
		e.t.Fatalf("写凭据 %s: %v", name, err)
	}
}

func auditAt(sec int64) string { return time.Unix(sec, 0).UTC().Format(time.RFC3339) }

func auditItemsOf(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("响应不是审计列表：%v（%s）", err, rec.Body.String())
	}
	if doc.Items == nil {
		t.Fatalf("items 必须是 [] 不是 null：%s", rec.Body.String())
	}
	return doc.Items
}

func auditKeysOf(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func auditStringField(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key].(string)
	if !ok {
		t.Fatalf("字段 %s 不是字符串：%#v", key, m)
	}
	return v
}

func expectStatusReason(t *testing.T, rec *httptest.ResponseRecorder, code int, reason string) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("期望 %d，实际 %d：%s", code, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"reason":"`+reason+`"`) {
		t.Fatalf("错误体 reason 不是 %s：%s", reason, rec.Body.String())
	}
}

func expectBadRequestMessage(t *testing.T, rec *httptest.ResponseRecorder, message string) {
	t.Helper()
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), message) {
		t.Fatalf("400 文案缺 %q：%s", message, rec.Body.String())
	}
}

func TestSourceAuditReadEndpoint(t *testing.T) {
	env := newAuditReadEnv(t)
	src := env.createPollSource(env.nsA, "watcher")
	const (
		older = int64(1_760_000_000)
		newer = int64(1_760_000_100)
	)
	env.appendSourceAudit(env.nsA, src.ID, older)
	env.appendSourceAudit(env.nsA, src.ID, newer)

	rec := env.get(env.sourceAuditPath(env.nsA, "watcher"), true)
	items := auditItemsOf(t, rec)
	if len(items) != 2 {
		t.Fatalf("审计应有 2 条，实际 %d：%#v", len(items), items)
	}
	if got := auditKeysOf(items[0]); got != "action,actor,at,detail,id" {
		t.Fatalf("源审计项字段集合 = %s，want action,actor,at,detail,id", got)
	}
	if got := auditStringField(t, items[0], "action"); got != store.SourceAuditPolledManually {
		t.Fatalf("action = %q, want %q", got, store.SourceAuditPolledManually)
	}
	if got := auditStringField(t, items[0], "actor"); got != env.userID {
		t.Fatalf("actor = %q, want %q", got, env.userID)
	}
	at := auditStringField(t, items[0], "at")
	parsed, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatalf("at = %q 不是 RFC3339: %v", at, err)
	}
	if want := auditAt(newer); at != want {
		t.Fatalf("at = %q, want %q（最新在前）", at, want)
	}
	if parsed.UTC() != parsed {
		t.Fatalf("at 不是 UTC：%v", parsed)
	}
	detail, ok := items[0]["detail"].(map[string]any)
	if !ok {
		t.Fatalf("detail 必须是对象：%#v", items[0]["detail"])
	}
	if got, ok := detail["items"].(float64); !ok || got != 2 {
		t.Fatalf("detail.items = %#v, want 2", detail["items"])
	}
	if got := auditStringField(t, items[1], "at"); got != auditAt(older) {
		t.Fatalf("第二条 at = %q（最新在前）", got)
	}

	if limited := auditItemsOf(t, env.get(env.sourceAuditPath(env.nsA, "watcher")+"?limit=1", true)); len(limited) != 1 {
		t.Fatalf("limit=1 只回 1 条，实际 %d", len(limited))
	}
	if beforeOlder := auditItemsOf(t, env.get(env.sourceAuditPath(env.nsA, "watcher")+"?before="+auditAt(older), true)); len(beforeOlder) != 0 {
		t.Fatalf("before=older 必须为空（严格早于）：%#v", beforeOlder)
	}
	beforeNewer := auditItemsOf(t, env.get(env.sourceAuditPath(env.nsA, "watcher")+"?before="+auditAt(newer), true))
	if len(beforeNewer) != 1 || auditStringField(t, beforeNewer[0], "at") != auditAt(older) {
		t.Fatalf("before=newer 必须只回更早那条：%#v", beforeNewer)
	}
	pageOne := auditItemsOf(t, env.get(env.sourceAuditPath(env.nsA, "watcher")+"?limit=1", true))
	cursor := auditStringField(t, pageOne[0], "at")
	pageTwo := auditItemsOf(t, env.get(env.sourceAuditPath(env.nsA, "watcher")+"?limit=1&before="+cursor, true))
	if len(pageTwo) != 1 || auditStringField(t, pageTwo[0], "at") == cursor {
		t.Fatalf("游标翻页不重不漏：%#v", pageTwo)
	}

	for _, bad := range []string{"limit=0", "limit=201", "limit=-1", "limit=abc"} {
		expectBadRequestMessage(t, env.get(env.sourceAuditPath(env.nsA, "watcher")+"?"+bad, true), "limit must be ")
	}
	expectBadRequestMessage(t, env.get(env.sourceAuditPath(env.nsA, "watcher")+"?limit=0", true), "limit must be between 1 and 200")
	expectBadRequestMessage(t, env.get(env.sourceAuditPath(env.nsA, "watcher")+"?limit=abc", true), "limit must be an integer")
	expectBadRequestMessage(t, env.get(env.sourceAuditPath(env.nsA, "watcher")+"?before=nope", true), "before must be an RFC3339 timestamp")

	env.createPollSource(env.nsA, "quiet")
	quiet := env.get(env.sourceAuditPath(env.nsA, "quiet"), true)
	if quiet.Code != http.StatusOK || strings.TrimSpace(quiet.Body.String()) != `{"items":[]}` {
		t.Fatalf("空审计正文必须是 {\"items\":[]}：%d %s", quiet.Code, quiet.Body.String())
	}
	if empty := auditItemsOf(t, env.get(env.sourceAuditPath(env.nsA, "watcher")+"?before=1970-01-01T00:00:00Z", true)); len(empty) != 0 {
		t.Fatalf("before=1970-01-01 必须为空：%#v", empty)
	}

	expectStatusReason(t, env.get(env.sourceAuditPath(env.nsA, "missing"), true), http.StatusNotFound, reasonNotFound)
	expectStatusReason(t, env.get(env.sourceAuditPath(env.nsB, "watcher"), true), http.StatusNotFound, reasonNotFound)
	if anon := env.get(env.sourceAuditPath(env.nsA, "watcher"), false); anon.Code != http.StatusUnauthorized {
		t.Fatalf("匿名读源审计必须 401，实际 %d：%s", anon.Code, anon.Body.String())
	}
}

func TestSourceAuditReadIsolatesNamespaces(t *testing.T) {
	env := newAuditReadEnv(t)
	nsASrc := env.createPollSource(env.nsA, "shared")
	nsBSrc := env.createPollSource(env.nsB, "shared")
	if nsASrc.ID == nsBSrc.ID {
		t.Fatal("两个命名空间的同名源必须是两条不同的行")
	}
	env.appendSourceAudit(env.nsA, nsASrc.ID, 1_760_000_000)

	if nsAItems := auditItemsOf(t, env.get(env.sourceAuditPath(env.nsA, "shared"), true)); len(nsAItems) != 1 {
		t.Fatalf("nsA 应回自己那一条：%#v", nsAItems)
	}
	nsBRec := env.get(env.sourceAuditPath(env.nsB, "shared"), true)
	if nsBRec.Code != http.StatusOK {
		t.Fatalf("nsB 同名源（存在但无审计）必须 200：%d %s", nsBRec.Code, nsBRec.Body.String())
	}
	if strings.TrimSpace(nsBRec.Body.String()) != `{"items":[]}` {
		t.Fatalf("nsB 不许读到 nsA 的行：%s", nsBRec.Body.String())
	}
}

func TestSecretAuditReadEndpoint(t *testing.T) {
	env := newAuditReadEnv(t)
	const secretValue = "audit-read-secret-559"
	env.putSecret(env.nsA, "gh-token", secretValue+"-v1")
	env.putSecret(env.nsA, "gh-token", secretValue+"-v2")
	env.putSecret(env.nsA, "gone", secretValue+"-v3")
	if err := env.db.DeleteNamespaceSecret(context.Background(), env.nsA, "gone", env.userID); err != nil {
		t.Fatalf("删凭据: %v", err)
	}
	if _, err := env.db.ExecContext(context.Background(),
		`INSERT INTO namespace_secrets (namespace_id, name, value, created_at, updated_at) VALUES (?,?,?,?,?)`,
		env.nsA, "silent", secretValue+"-v4", 1_760_000_000, 1_760_000_000); err != nil {
		t.Fatalf("插无审计凭据行: %v", err)
	}

	rec := env.get(env.secretAuditPath(env.nsA, "gh-token"), true)
	items := auditItemsOf(t, rec)
	if len(items) != 2 {
		t.Fatalf("凭据审计应有 created+rotated 两条，实际 %d：%#v", len(items), items)
	}
	if got := auditKeysOf(items[0]); got != "action,actor,at,id" {
		t.Fatalf("凭据审计项字段集合 = %s，want action,actor,at,id（没有 detail、没有 value）", got)
	}
	if _, has := items[0]["detail"]; has {
		t.Fatal("凭据审计不许有 detail 键")
	}
	if _, has := items[0]["value"]; has {
		t.Fatal("凭据审计不许有 value 键")
	}
	if got := auditStringField(t, items[0], "action"); got != store.NamespaceSecretRotated {
		t.Fatalf("audit[0].action = %q, want rotated（最新在前）", got)
	}
	if got := auditStringField(t, items[1], "action"); got != store.NamespaceSecretCreated {
		t.Fatalf("audit[1].action = %q, want created", got)
	}
	if got := auditStringField(t, items[0], "actor"); got != env.userID {
		t.Fatalf("actor = %q, want %q", got, env.userID)
	}
	at := auditStringField(t, items[0], "at")
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Fatalf("at = %q 不是 RFC3339: %v", at, err)
	}
	if strings.Contains(rec.Body.String(), secretValue) {
		t.Fatalf("凭据值泄漏进审计响应：%s", rec.Body.String())
	}

	if limited := auditItemsOf(t, env.get(env.secretAuditPath(env.nsA, "gh-token")+"?limit=1", true)); len(limited) != 1 {
		t.Fatalf("limit=1 只回 1 条，实际 %d", len(limited))
	}
	for _, bad := range []string{"limit=0", "limit=201", "limit=-1", "limit=abc"} {
		expectBadRequestMessage(t, env.get(env.secretAuditPath(env.nsA, "gh-token")+"?"+bad, true), "limit must be ")
	}
	expectBadRequestMessage(t, env.get(env.secretAuditPath(env.nsA, "gh-token")+"?limit=0", true), "limit must be between 1 and 200")
	expectBadRequestMessage(t, env.get(env.secretAuditPath(env.nsA, "gh-token")+"?before=nope", true), "before must be an RFC3339 timestamp")

	silentRec := env.get(env.secretAuditPath(env.nsA, "silent"), true)
	if silentRec.Code != http.StatusOK || strings.TrimSpace(silentRec.Body.String()) != `{"items":[]}` {
		t.Fatalf("空审计正文必须是 {\"items\":[]}：%d %s", silentRec.Code, silentRec.Body.String())
	}
	if empty := auditItemsOf(t, env.get(env.secretAuditPath(env.nsA, "gh-token")+"?before=1970-01-01T00:00:00Z", true)); len(empty) != 0 {
		t.Fatalf("before=1970-01-01 必须为空：%#v", empty)
	}

	expectStatusReason(t, env.get(env.secretAuditPath(env.nsA, "gone"), true), http.StatusNotFound, reasonSecretAbsent)
	expectStatusReason(t, env.get(env.secretAuditPath(env.nsA, "nobody"), true), http.StatusNotFound, reasonSecretAbsent)
	expectStatusReason(t, env.get(env.secretAuditPath(env.nsB, "gh-token"), true), http.StatusNotFound, reasonSecretAbsent)
	if anon := env.get(env.secretAuditPath(env.nsA, "gh-token"), false); anon.Code != http.StatusUnauthorized {
		t.Fatalf("匿名读凭据审计必须 401，实际 %d：%s", anon.Code, anon.Body.String())
	}
}

func TestSecretAuditReadIsolatesNamespaces(t *testing.T) {
	env := newAuditReadEnv(t)
	env.putSecret(env.nsA, "shared-secret", "value-a-559")
	env.putSecret(env.nsB, "shared-secret", "value-b-559")

	nsAItems := auditItemsOf(t, env.get(env.secretAuditPath(env.nsA, "shared-secret"), true))
	nsBItems := auditItemsOf(t, env.get(env.secretAuditPath(env.nsB, "shared-secret"), true))
	if len(nsAItems) != 1 || len(nsBItems) != 1 {
		t.Fatalf("两个 ns 各一条：nsA=%#v nsB=%#v", nsAItems, nsBItems)
	}
	nsAID := auditStringField(t, nsAItems[0], "id")
	nsBID := auditStringField(t, nsBItems[0], "id")
	if nsAID == nsBID {
		t.Fatal("跨命名空间同名凭据的审计行不许被串起来")
	}
	env.putSecret(env.nsA, "only-a", "value-c-559")
	expectStatusReason(t, env.get(env.secretAuditPath(env.nsB, "only-a"), true), http.StatusNotFound, reasonSecretAbsent)
}
