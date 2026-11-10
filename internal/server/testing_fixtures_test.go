package server

// testing_fixtures_test.go 是 v4 保留测试的**共用夹具**收口（#331 / D0.1）。
//
// 为什么需要这个文件：v3 的资源域 handler 与它们的测试文件在 #331 具名退役
// （console_test.go / source_handlers_test.go 等，见 docs/v4/g3a-test-inventory.md），
// 但仍有几份**与 v3 产品语义无关**的通用夹具被保留测试引用着：
//
//   - closeDBForTempDir：收尾竞态断言（#259），auth_cache_wiring_test.go 在用；
//   - decode：响应体解码，任务读模型用例在用；
//   - hook 入口环境：POST /v1/hooks/{token} 的限流用例（#245）需要一个真 store +
//     真能力 + 真 Source 路由表；
//   - 任务读模型环境：Task / Trace 的「响应里没有 out」用例（A8）需要真会话 cookie。
//
// 搬迁纪律：能逐字照抄的（closeDBForTempDir / decode）就逐字照抄，不改断言；
// 必须适配 v4 的（builtin notify.phone 已随中央执行域退役 → 用例自建真能力）
// 只在**装配**层适配，被测断言一行不动。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/store"
)

// ---------- 关库收尾 ----------

// closeDBForTempDir 是测试环境的确定性收口（只本包用，不跨包抽象；#259）。
//
// 逐字搬迁自 b30e998:internal/server/console_test.go（随 console_test.go 退役，
// 但断言的是**收尾竞态**这件与 v3 产品语义无关的事，auth_cache_wiring_test.go 依赖它）。
//
// 三条断言都是「收尾竞态」的机器判据，不是新的产品行为：
//  1. 关库之前仍有 goroutine 持有连接 ⇒ 它随时可能发起一次写库，与 db.Close() 打架；
//  2. db.Close() 之后连接数仍不为零 ⇒ 有连接是在 Close 期间被借出的（Close 只关空闲连接）；
//  3. 目录里残留 -wal/-shm ⇒ 正是 #259 的失败签名（TempDir 里只剩 console.db-shm），
//     接着的 RemoveAll 就会撞上「directory not empty」。
//
// 换句话说：db.Close 之后的并发写者只会看到 `database is closed`（postclose_guard_test.go
// 的护栏会把它判红），这里把同一个竞态在收尾点上提前逮住。
func closeDBForTempDir(t *testing.T, db *store.DB, dir string) {
	t.Helper()
	// 任何一条断言失败都会走 t.Fatalf（跳过本函数余下语句）：用 defer 兜底关库，
	// 免得失败路径上把库留开着、又在 t.TempDir 的 RemoveAll 里制造第二层噪声。
	defer func() { _ = db.Close() }()

	// ① 先问「还有谁在用」：in-use 连接意味着某条 goroutine 正在一次查询/事务里。
	if inUse := db.Stats().InUse; inUse != 0 {
		reportPostCloseRace(fmt.Sprintf("closeDBForTempDir: InUse=%d dir=%s（后台写者会在关库后看到 `%s`）",
			inUse, dir, dbClosedMarker))
		t.Fatalf("收尾竞态：%d 条 DB 连接仍被 goroutine 持有（cleanup 必须等所有会话退出后再关库）", inUse)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("收尾竞态：db.Close 失败（有 goroutine 在关库窗口里用库？）: %v", err)
	}

	// ② Close 之后仍有连接 → 说明它是 Close 期间被借出的（归还时才真正关闭）。
	if open := db.Stats().OpenConnections; open != 0 {
		reportPostCloseRace(fmt.Sprintf("closeDBForTempDir: OpenConnections=%d dir=%s（Close 只关空闲连接）",
			open, dir))
		t.Fatalf("收尾竞态：db.Close 后仍有 %d 条连接未关（并发写者只会看到 `%s`）", open, dbClosedMarker)
	}

	// ③ 干净关闭会删掉 -wal/-shm；留下就说明有连接在关库之后又碰了库（或没能关掉）。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读临时目录 %s: %v", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".db-wal") || strings.HasSuffix(name, ".db-shm") {
			reportPostCloseRace(fmt.Sprintf("closeDBForTempDir: 残留 WAL=%s dir=%s（有连接在 RemoveAll 期间重建文件，写者只会看到 `%s`）",
				name, dir, dbClosedMarker))
			t.Fatalf("收尾竞态：db.Close 后仍残留 WAL 文件 %s（并发写者只会看到 `%s`）", name, dbClosedMarker)
		}
	}
}

// ---------- 响应体解码 ----------

// decode 把 recorder 的响应体解成一个 JSON 对象（失败即 Fatal，避免用例漏断言）。
//
// 逐字搬迁自 b30e998:internal/server/console_test.go。
func decode(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode response %q: %v", rr.Body.String(), err)
	}
	return m
}

// ---------- hook 入口环境（#245 限流用例） ----------

// hookTestEnv 是 POST /v1/hooks/{token} 的测试环境：真 SQLite + 真 store + 真
// Source 路由表（newSourceAPI().register，测的是真实注册结果而不是手抄的 pattern）。
//
// 与 v3 的 nsTestEnv 的差别只有一处：v3 的用例会落一个 builtin `notify.phone`
// 出口（注册用户时同事务播种），v4 的 builtin 播种随中央执行域一起退役（D0.2），
// 所以这里**显式建一个真实能力**当出口 —— 断言口径不变（限额内的投递必须真的走到
// 派发并 200，而不是只证明「限流没拒」）。
type hookTestEnv struct {
	db   *store.DB
	mux  http.Handler
	nsID string
	// capability 是夹具建出的出口能力名（webhook 源的 targets 指向它）。
	capability string
}

// hookTestCapabilityName 是夹具能力名。不要叫 `notify.phone`：那个名字属于已退役的
// builtin 播种，用例若依赖它就会掩盖「内置能力没了」这件事。
const hookTestCapabilityName = "notify"

// newHookTestEnv 建库 + 默认命名空间 + 一个可解析的出口能力，并挂上 Source 路由表。
func newHookTestEnv(t *testing.T) *hookTestEnv {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ns, err := db.CreateNamespace(ctx, "default", "默认")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	// 出口能力：targets 要在**源的命名空间**内可解析，否则建源就被 store 拒。
	// 参数 schema 留空（无 required），让限额内的投递能真的走到派发。
	if err := db.CreateCapability(ctx, &store.CapabilityVersion{
		NamespaceID: ns.ID,
		Name:        hookTestCapabilityName,
		Version:     1,
		Spec: artifact.Spec{
			Params: []byte(`{"type":"object"}`),
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
			},
		},
	}); err != nil {
		t.Fatalf("create capability %s: %v", hookTestCapabilityName, err)
	}

	router := route.New(db)
	deps := &serverDeps{DB: db, Router: router, Sources: source.New(db, router), Now: nowMilli}
	mux := http.NewServeMux()
	newSourceAPI(deps).register(mux)

	return &hookTestEnv{db: db, mux: mux, nsID: ns.ID, capability: hookTestCapabilityName}
}

// doAnon 匿名发一次 JSON 请求（hook 入口没有会话：URL 里的 token 就是凭据）。
func (e *hookTestEnv) doAnon(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	return rr
}

// ---------- 任务读模型环境（A8：产出只走 emit） ----------

// taskOutEnv 是 Task / Trace 读模型的测试环境：真 SQLite + 真用户/默认命名空间 +
// 真会话 cookie + 真 taskAPI 路由表。
//
// 用真 `auth.NewSessionManager` 而不是「往 context 里塞 userID」：这两个用例断言的是
// **HTTP 字节流**里没有 `out`，走完整 会话中间件 → 授权 → handler → store 链路才有意义
// （v3 的 consoleEnv 也是这个口径）。
type taskOutEnv struct {
	db   *store.DB
	mux  http.Handler
	nsID string
	// nsSlug 是 URL 里用的命名空间段（路径用的是 slug，store 用的是 id）。
	nsSlug string
	// capability 是夹具建出的能力行（任务的 version/digest 必须钉真实事实）。
	capability *store.CapabilityVersion
	cookie     *http.Cookie
}

// newTaskOutEnv 建库 + 用户（注册即建出默认命名空间）+ 一个能力 + 会话，并挂上
// Task / Event / Trace 路由表。
func newTaskOutEnv(t *testing.T) *taskOutEnv {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	user := &store.User{
		ID: store.NewUserID(), Username: "taskout", DisplayName: "任务读模型",
		CreatedAt: store.Now(),
	}
	ns, err := db.RegisterUser(ctx, user)
	if err != nil {
		t.Fatalf("register user: %v", err)
	}

	capDef := &store.CapabilityVersion{
		NamespaceID: ns.ID,
		Name:        "review",
		Version:     1,
		Spec: artifact.Spec{
			Params: []byte(`{"type":"object"}`),
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
			},
		},
	}
	if err := db.CreateCapability(ctx, capDef); err != nil {
		t.Fatalf("create capability: %v", err)
	}

	sm := auth.NewSessionManager(db, 0, false)
	sess, err := sm.Create(user.ID, "task out test")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	deps := &serverDeps{DB: db, Session: sm.Middleware, Now: nowMilli}
	mux := http.NewServeMux()
	newTaskAPI(deps).register(mux)

	return &taskOutEnv{
		db: db, mux: mux, nsID: ns.ID, nsSlug: ns.Slug, capability: capDef,
		cookie: &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID},
	}
}

// tasksPath 是命名空间内的任务集合根。
func (e *taskOutEnv) tasksPath() string {
	return "/v1/namespaces/" + e.nsSlug + "/tasks"
}

// tracePath 是一条 trace 的全景读模型路径。
func (e *taskOutEnv) tracePath(traceID string) string {
	return "/v1/namespaces/" + e.nsSlug + "/traces/" + traceID
}

// do 以夹具的登录态发一次 GET。
func (e *taskOutEnv) do(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.AddCookie(e.cookie)
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	return rr
}
