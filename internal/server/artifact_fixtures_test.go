package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/resultspool"
	"github.com/zzjcool/anotify/internal/store"
)

// artifactEnv 是 #332 上传/下载两条端点共用的**真实 fixture**。
//
// 为什么需要它（而不是各自在测试里手搓）：两条端点的语义都建立在真事实上——
// 真 sqlite（manifest/result_ref/租约）、真节点身份（token 明文 + 库里的 sha256）、
// 真授权链（AssignTask + GrantLeaseAuthorized 发出的 lease/attemptToken）、
// 真托管目录（0600 文件 + 原子发布）。任何一环用 stub 替代，测的就不是这条链路了。
//
// 刻意**不**做的事：不伪造 attemptToken 的哈希（store 的哈希口径是内部实现，
// 这里走真 grant 拿明文），不绕过 store 的授权函数（B 的拒绝用例正是要打这条路径）。
type artifactEnv struct {
	t     *testing.T
	ctx   context.Context
	db    *store.DB
	spool *resultspool.Store
	deps  *serverDeps

	userID  string
	cookie  *http.Cookie
	ns      *store.Namespace
	nsOther *store.Namespace
	cap     *store.CapabilityVersion

	node      *store.Node
	nodeToken string

	task        *store.Task
	leaseID     string
	attemptTok  string
	attempt     int
	spoolDirTag string
}

// newArtifactEnv 建库 + 用户/两个命名空间/能力 + 一台真节点 + 一条**已 grant**的任务。
func newArtifactEnv(t *testing.T) *artifactEnv {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(filepath.Join(t.TempDir(), "artifact.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	user := &store.User{ID: store.NewUserID(), Username: "artifact-tester", CreatedAt: store.Now()}
	ns, err := db.RegisterUser(ctx, user)
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	nsOther, err := db.CreateNamespace(ctx, "artifact-b", "产物命名空间 B")
	if err != nil {
		t.Fatalf("create second namespace: %v", err)
	}

	maxAttempts := 2
	capDef := &store.CapabilityVersion{
		NamespaceID: ns.ID,
		Name:        "export",
		Version:     1,
		Spec: artifact.Spec{
			Params: []byte(`{"type":"object"}`),
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
			},
			// 幂等 + 2 次尝试：下载端点的「历史 attempt」用例需要真链路能走到
			// attempt 2（非幂等任务在 RecoverLostLeases 里直接 abandoned，
			// 重派会被 store 拒掉）。夹具声明真实的产品事实，测试不用再手改 tasks 行。
			Lifecycle: artifact.Lifecycle{Idempotent: true, MaxAttempts: &maxAttempts},
		},
	}
	if err := db.CreateCapability(ctx, capDef); err != nil {
		t.Fatalf("create capability: %v", err)
	}

	spoolDir := filepath.Join(t.TempDir(), "spool")
	spool, err := resultspool.New(spoolDir)
	if err != nil {
		t.Fatalf("open resultspool: %v", err)
	}

	sm := auth.NewSessionManager(db, 0, false)
	sess, err := sm.Create(user.ID, "artifact transport test")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	env := &artifactEnv{
		t: t, ctx: ctx, db: db, spool: spool,
		userID: user.ID, cookie: &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID},
		ns: ns, nsOther: nsOther, cap: capDef,
	}
	env.deps = &serverDeps{
		DB:      db,
		Spool:   spool,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:     store.Now,
		Session: sm.Middleware,
	}
	env.seedGrantedTask(t)
	return env
}

// seedGrantedTask 走真实派发链建一条 running 任务（AssignTask + GrantLeaseAuthorized），
// 因此 lease/attemptToken/attempt 都是 store 自己发出的权威事实。
func (e *artifactEnv) seedGrantedTask(t *testing.T) {
	t.Helper()
	ctx := e.ctx
	n := &store.Node{Name: "artifact-node", Status: store.NodeStatusOnline, DeclaredNamespaces: []string{"*"}}
	token, err := e.db.CreateNode(ctx, n)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	e.nodeToken = token // 明文只在这里出现一次（与真握手一致）
	if _, err := e.db.UpsertInstances(ctx, n.ID, []store.InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("upsert instances: %v", err)
	}
	cfg, err := e.db.GetNodeConfig(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node config: %v", err)
	}
	cfg.Namespaces = []string{"*"}
	cfg.Vars = map[string]string{}
	cfg.Limits = map[string]any{"total": 1}
	if err := e.db.PutNodeConfig(ctx, cfg); err != nil {
		t.Fatalf("put node config: %v", err)
	}
	typed, err := store.TypedMachineLimits(cfg.Limits)
	if err != nil {
		t.Fatalf("typed limits: %v", err)
	}
	hash, err := protocol.MachineConfigHash(map[string]string{}, typed)
	if err != nil {
		t.Fatalf("machine config hash: %v", err)
	}
	if err := e.db.RecordApplied(ctx, n.ID, cfg.Revision, hash); err != nil {
		t.Fatalf("record applied: %v", err)
	}

	node, err := e.db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("read node: %v", err)
	}
	// CreateNode 只在返回时给一次 token 明文；重新登记一台等价节点的是测试噪音，
	// 这里直接用 store 的公开摘要口径重算（HashNodeToken）——库里的 token_hash 是权威值。
	e.node = node

	task := &store.Task{
		NamespaceID:  e.ns.ID,
		TraceID:      "trace-artifact",
		CapabilityID: e.cap.ID,
		Capability:   e.cap.Name,
		Version:      e.cap.Version,
		Digest:       e.cap.Digest,
		Params:       []byte(`{}`),
	}
	if err := e.db.CreateTasks(ctx, []*store.Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	got, err := e.db.GetTask(ctx, e.ns.ID, task.ID)
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	e.task = got

	if err := e.db.AssignTask(ctx, task.ID, node.ID, "asg-artifact", 1); err != nil {
		t.Fatalf("assign task: %v", err)
	}
	lease, token, attempt, _, err := e.db.GrantLeaseAuthorized(ctx,
		store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash},
		task.ID, "asg-artifact", 1, 0, store.Now())
	if err != nil {
		t.Fatalf("grant lease: %v", err)
	}
	e.leaseID, e.attemptTok, e.attempt = lease, token, attempt
}

// uploadURL 拼上传路径（{sha256} 段就是内容哈希，逻辑路径走 header）。
func (e *artifactEnv) uploadURL(sha string) string {
	return protocol.NodeTaskArtifactsPathPrefix + e.task.ID + protocol.NodeTaskArtifactsPathInfix + sha
}

// downloadURL 拼用户下载路径（命名空间用 slug 身份）。
func (e *artifactEnv) downloadURL(nsSlug, taskID, sha string) string {
	return "/v1/namespaces/" + nsSlug + "/tasks/" + taskID + "/artifacts/" + sha
}

// artifactSHA 返回内容字节的 sha256 hex（与 protocol 的 64 位小写口径一致）。
func artifactSHA(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// seed 登记一条 manifest 并把真实字节写进托管目录（模拟一次成功上传的**终态**）。
//
// 下载端点的测试需要一个「已发布副本 + 已登记 manifest」的起点；这里直接调 store 的
// 授权登记入口（Begin+Register）并在 spool 里发布同 id 的文件，与真实 handler 的
// 落地顺序（先发布后登记）一致。
func (e *artifactEnv) seed(t *testing.T, logicalPath string, content []byte) protocol.TaskArtifact {
	t.Helper()
	ctx := e.ctx
	sha := artifactSHA(content)
	in := protocol.ArtifactUploadResponse{
		TaskID: e.task.ID, Attempt: e.attempt, Path: logicalPath, SHA256: sha, Size: int64(len(content)),
	}
	auth := e.storeAuth()
	u, err := e.db.BeginArtifactUpload(ctx, auth, in)
	if err != nil {
		t.Fatalf("seed begin %s: %v", logicalPath, err)
	}
	draft, err := e.spool.CreateDraft(protocol.MaxArtifactTransferBytes)
	if err != nil {
		t.Fatalf("seed draft: %v", err)
	}
	if _, err := draft.Write(content); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	if err := draft.Finalize(u.ObjectID().String()); err != nil {
		t.Fatalf("seed finalize: %v", err)
	}
	if _, _, err := e.db.RegisterArtifactUpload(ctx, auth, u, sha, int64(len(content))); err != nil {
		t.Fatalf("seed register: %v", err)
	}
	return protocol.TaskArtifact{Path: logicalPath, SHA256: sha}
}

// complete 让当前 attempt 以引用这些产物的事实终态化（写 result_ref + reported=1）。
func (e *artifactEnv) complete(t *testing.T, refs ...protocol.TaskArtifact) {
	t.Helper()
	// 重新读一次任务，确保看到上一步（例如 seed 之后）的最新事实。
	result := store.TaskResult{Phase: store.TaskSucceeded, Artifacts: refs}
	if err := e.db.CompleteTaskAuthorized(e.ctx,
		store.NodeAuthorization{NodeID: e.node.ID, NodeTokenHash: e.node.TokenHash},
		e.task.ID, e.attempt, e.leaseID, e.authNow(), result); err != nil {
		t.Fatalf("complete task: %v", err)
	}
	got, err := e.db.GetTask(e.ctx, e.ns.ID, e.task.ID)
	if err != nil {
		t.Fatalf("read completed task: %v", err)
	}
	e.task = got
}

func (e *artifactEnv) authNow() int64 { return store.Now() }

// storeAuth 构造与 fixture 当前授权事实一致的 store.EmitAuth。
func (e *artifactEnv) storeAuth() store.EmitAuth {
	return store.EmitAuth{
		TaskID: e.task.ID, NodeID: e.node.ID, NodeTokenHash: e.node.TokenHash,
		Attempt: e.attempt, LeaseID: e.leaseID, AttemptToken: e.attemptTok, Now: store.Now(),
	}
}

// do 发一次请求（默认带登录 cookie；需要匿名时用 doAnon）。
func (e *artifactEnv) do(mux *http.ServeMux, method, target string, body io.Reader, header map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, target, body)
	req.AddCookie(e.cookie)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

// doAnon 发一次无 cookie 的请求。
func (e *artifactEnv) doAnon(mux *http.ServeMux, method, target string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

// uploadHeaders 是节点侧发一次上传所需的全部头（与 cmd/anode 客户端逐字一致）。
func (e *artifactEnv) uploadHeaders(logicalPath string) map[string]string {
	encoded, err := protocol.EncodeArtifactPathHeader(logicalPath)
	if err != nil {
		e.t.Fatalf("encode path %q: %v", logicalPath, err)
	}
	return map[string]string{
		"Authorization":                     "Bearer " + e.nodeToken,
		protocol.ArtifactHeaderAttempt:      fmt.Sprint(e.attempt),
		protocol.ArtifactHeaderLeaseID:      e.leaseID,
		protocol.ArtifactHeaderTaskToken:    e.attemptTok,
		protocol.ArtifactHeaderArtifactPath: encoded,
		"Content-Type":                      protocol.ArtifactTransferContentType,
	}
}

// objectFiles 列出托管目录下已发布的对象文件（排序，便于断言）。
func (e *artifactEnv) objectFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(e.spool.Root(), "objects"))
	if err != nil {
		t.Fatalf("read objects dir: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	sort.Strings(out)
	return out
}

// countRows 跑一条 COUNT 查询（零写断言的通用口径）。
func (e *artifactEnv) countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.QueryRowContext(e.ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// assertNoArtifactWrites 断言一次被拒绝的上传没有留下任何产物事实：
// manifest 0 行、活跃 ticket 0 行、托管目录没有对象文件。object 行（GC 工作）允许残留——
// 它是 store 在 Begin 时铸造的身份，由有界 GC 回收（A1 的补偿清理语义）。
func (e *artifactEnv) assertNoArtifactWrites(t *testing.T) {
	t.Helper()
	if n := e.countRows(t, `SELECT COUNT(*) FROM task_artifact_manifests WHERE task_id=?`, e.task.ID); n != 0 {
		t.Fatalf("manifest rows = %d, want 0", n)
	}
	if n := e.countRows(t, `SELECT COUNT(*) FROM task_artifact_uploads WHERE task_id=?`, e.task.ID); n != 0 {
		t.Fatalf("active tickets = %d, want 0", n)
	}
	if files := e.objectFiles(t); len(files) != 0 {
		t.Fatalf("spool objects = %v, want none", files)
	}
}

// setLeaseUntil 直接改任务的租约到期时刻（模拟节点侧租约在传输途中过期）。
func (e *artifactEnv) setLeaseUntil(t *testing.T, until int64) {
	e.t.Helper()
	if _, err := e.db.Exec(`UPDATE tasks SET lease_until=? WHERE id=?`, until, e.task.ID); err != nil {
		t.Fatalf("set lease_until: %v", err)
	}
}

// cancelTask 让任务进入「已请求取消」状态（取消即撤权）。
func (e *artifactEnv) cancelTask(t *testing.T) {
	e.t.Helper()
	if err := e.db.RequestCancel(e.ctx, e.task.ID, protocol.ReasonCancelledByUser, store.Now()); err != nil {
		t.Fatalf("request cancel: %v", err)
	}
}

// rotateNodeToken 轮换节点身份（旧 Bearer 立刻失效）。
func (e *artifactEnv) rotateNodeToken(t *testing.T) {
	e.t.Helper()
	if err := e.db.SetNodeTokenHash(e.ctx, e.node.ID, store.HashNodeToken("rotated-token")); err != nil {
		t.Fatalf("rotate node token: %v", err)
	}
}

// denyNamespace 把节点配置里的命名空间收窄到别的 ns（撤掉对本任务 ns 的授权）。
func (e *artifactEnv) denyNamespace(t *testing.T) {
	e.t.Helper()
	cfg, err := e.db.GetNodeConfig(e.ctx, e.node.ID)
	if err != nil {
		t.Fatalf("get node config: %v", err)
	}
	cfg.Namespaces = []string{"some-other-ns"}
	if err := e.db.PutNodeConfig(e.ctx, cfg); err != nil {
		t.Fatalf("put node config: %v", err)
	}
}

// expireUploadTicket 把当前活跃 ticket 的窗口推到过去（模拟慢链路超过 30s ticket）。
func (e *artifactEnv) expireUploadTicket(t *testing.T) {
	e.t.Helper()
	if _, err := e.db.Exec(`UPDATE task_artifact_uploads SET expires_at=? WHERE task_id=?`, store.Now()-1, e.task.ID); err != nil {
		t.Fatalf("expire ticket: %v", err)
	}
}

// bumpAttempt 直接把 task 的当前 attempt 推进一格（模拟节点侧持有的是旧 attempt）。
func (e *artifactEnv) bumpAttempt(t *testing.T) {
	e.t.Helper()
	if _, err := e.db.Exec(`UPDATE tasks SET attempt=attempt+1 WHERE id=?`, e.task.ID); err != nil {
		t.Fatalf("bump attempt: %v", err)
	}
}

// mustContainString 断言 needle 在 haystack 里（错误体/头的断言小工具）。
func mustContainString(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("%s = %q, want it to contain %q", what, haystack, needle)
	}
}
