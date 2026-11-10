package server

import (
	"bytes"
	"encoding/json"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 A1 下载端点（#332 / G3-D3、D4）的验收套件：真 httptest + 真 sqlite +
// 真托管池 + 真会话中间件（`newArtifactEnv`）。
//
// 三条断言纪律，每条用例都遵守：
//
//   - 成功用例必须同时核对「状态码」「响应体逐字节一致」「Content-Length 与真实字节数
//     一致」「Content-Disposition 的 filename 与逻辑 path 的 basename 一致」「Cache-Control
//     含 no-store」——只断言 200 + 非空 body 会把「下发了另一份字节」「长度声明错误」
//     「文件名被注入破坏」全部当 PASS。
//   - 阴性用例必须同时核对「HTTP 码 + reason + 响应体不含产物字节」：产物端点最坏的
//     形态不是报错，而是「报了 410 却仍然把字节吐出来了」。
//   - 路径逃逸/符号链接/宿主文件 fallback 三类用例必须真的在**托管根之外**造 decoy，
//     否则测的只是「文件不存在」。

// downloadPathPattern 是下载端点的路由形状（与生产接线一致）。
const downloadPathPattern = "/v1/namespaces/{ns}/tasks/{taskId}/artifacts/{sha256}"

// downloadMux 把下载路由挂到一个**新建**的 mux 上。
//
// 包装顺序与生产接线逐字一致：`noStore(会话中间件(handler))`——验收点里的
// `Cache-Control: no-store` 就来自最外层，不是 handler 自己设的。
func downloadMux(env *artifactEnv) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET "+downloadPathPattern,
		noStore(env.deps.session(http.HandlerFunc(newArtifactDownloadAPI(env.deps).get))))
	return mux
}

// downloadDirect 绕过 mux 直接打 handler（路径段用 SetPathValue 显式给）。
//
// 为什么需要它：Go 1.22+ 的 ServeMux 在**匹配之前**就对 `.`/`..` 段做了路径规范化，
// 原始 `..` 会得到 301 重定向、永远到不了任何 handler。所以「handler 自己也不接受
// `..`」这条必须用直连证明，否则只是证明了 net/http 的行为。
func downloadDirect(env *artifactEnv, taskID, sha string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/direct", nil)
	req.SetPathValue("ns", env.ns.Slug)
	req.SetPathValue("taskId", taskID)
	req.SetPathValue("sha256", sha)
	req.AddCookie(env.cookie)
	h := noStore(env.deps.session(http.HandlerFunc(newArtifactDownloadAPI(env.deps).get)))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// downloadAssertStatus 断言一次拒绝：HTTP 码 + Status 信封的 reason。
//
// 下载的失败体走**现有** Status 信封（A1 §5：成功是原始 bytes，失败不另造形状）。
func downloadAssertStatus(t *testing.T, rr *httptest.ResponseRecorder, wantCode int, wantReason, what string) {
	t.Helper()
	if rr.Code != wantCode {
		t.Fatalf("%s: HTTP 状态 = %d, want %d（body=%s）", what, rr.Code, wantCode, rr.Body.String())
	}
	var doc statusDoc
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatalf("%s: 错误体不是 Status 信封: %v（body=%s）", what, err, rr.Body.String())
	}
	if doc.Status != "Failure" || doc.Code != wantCode {
		t.Fatalf("%s: Status = %q/%d, want Failure/%d", what, doc.Status, doc.Code, wantCode)
	}
	if doc.Reason != wantReason {
		t.Fatalf("%s: reason = %q, want %q（body=%s）", what, doc.Reason, wantReason, rr.Body.String())
	}
}

// downloadAssertNoArtifactBytes 断言响应体里没有出现不该有的字节。
func downloadAssertNoArtifactBytes(t *testing.T, rr *httptest.ResponseRecorder, secret []byte, what string) {
	t.Helper()
	if len(secret) == 0 {
		t.Fatalf("%s: 断言判据为空字节串（测试写错了）", what)
	}
	if bytes.Contains(rr.Body.Bytes(), secret) {
		t.Fatalf("%s: 响应体里出现了不该下发的字节 %q（body=%q）", what, secret, rr.Body.String())
	}
}

// downloadDispositionFilename 解析 Content-Disposition 并返回 filename。
//
// 用 mime.ParseMediaType 而不是字符串 contains：只有真正可解析的头才能证明
// 「注入没有破坏语法」，也只有它会按 RFC 2231 解出非 ASCII 的 filename*。
func downloadDispositionFilename(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	raw := rr.Header().Get("Content-Disposition")
	if raw == "" {
		t.Fatal("缺 Content-Disposition")
	}
	if strings.ContainsAny(raw, "\r\n") {
		t.Fatalf("Content-Disposition 含 CR/LF（头注入）: %q", raw)
	}
	mediatype, params, err := mime.ParseMediaType(raw)
	if err != nil {
		t.Fatalf("Content-Disposition 不可解析: %v（%q）", err, raw)
	}
	if mediatype != "attachment" {
		t.Fatalf("Content-Disposition 类型 = %q, want attachment（%q）", mediatype, raw)
	}
	return params["filename"]
}

// objectPath 返回托管目录下唯一那个对象文件的绝对路径（测试用）。
//
// 只用于**构造**阴性条件（删掉/换成符号链接），handler 自己从不用这个路径。
func objectPath(t *testing.T, env *artifactEnv) string {
	t.Helper()
	files := env.objectFiles(t)
	if len(files) != 1 {
		t.Fatalf("托管目录对象数 = %d (%v), want 1", len(files), files)
	}
	return filepath.Join(env.spool.Root(), "objects", files[0])
}

// seedReported 登记一条产物并让当前 attempt 以引用它的结果终态化（成功用例的起点）。
//
// 返回的 TaskArtifact 就是 URL 里那个 `{sha256}` 的取值来源：下载端点没有别的入口
// 能表达「我要这份字节」，hash 就是它的地址。
func seedReported(t *testing.T, env *artifactEnv, logicalPath string, content []byte) protocol.TaskArtifact {
	t.Helper()
	art := env.seed(t, logicalPath, content)
	env.complete(t, art)
	return art
}

// ---------- D1：正常下载 ----------

func TestArtifactDownloadHappyPath(t *testing.T) {
	env := newArtifactEnv(t)
	// 内容刻意含 NUL/高位字节：下载端点必须**逐字节**透传，不能是文本通道。
	content := []byte("# report\n\n\x00\x01\x02 binary tail \xff\xfe\n")
	tk := seedReported(t, env, "reports/report.md", content)

	mux := downloadMux(env)
	rr := env.do(mux, http.MethodGet, env.downloadURL(env.ns.Slug, env.task.ID, tk.SHA256), nil, nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("HTTP 状态 = %d, want 200（body=%s）", rr.Code, rr.Body.String())
	}
	got := rr.Body.Bytes()
	if !bytes.Equal(got, content) {
		t.Fatalf("响应体 = %q, want 逐字节等于上传内容 %q", got, content)
	}
	if sha := artifactSHA(got); sha != tk.SHA256 {
		t.Fatalf("响应体 sha256 = %q, want %q", sha, tk.SHA256)
	}
	if cl := rr.Header().Get("Content-Length"); cl != strconv.Itoa(len(content)) {
		t.Fatalf("Content-Length = %q, want %d（实际字节数）", cl, len(content))
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want application/octet-stream", ct)
	}
	if fn := downloadDispositionFilename(t, rr); fn != "report.md" {
		t.Fatalf("disposition filename = %q, want report.md", fn)
	}
	if xcto := rr.Header().Get("X-Content-Type-Options"); xcto != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", xcto)
	}
	mustContainString(t, rr.Header().Get("Cache-Control"), "no-store", "Cache-Control")
	if lm := rr.Header().Get("Last-Modified"); lm != "" {
		t.Fatalf("Last-Modified = %q, want 空（零 modtime 不下发）", lm)
	}
	// 成功体是原始字节，**不是** Status 信封（A1 §4 的明文例外）。
	if json.Valid(got) && strings.HasPrefix(strings.TrimSpace(string(got)), "{") {
		t.Fatalf("成功响应体看起来是 JSON 信封: %q", got)
	}
}

// ---------- D2：Content-Disposition 注入阴性 ----------

func TestArtifactDownloadContentDispositionInjection(t *testing.T) {
	// 三条都必须是**合法**逻辑路径（ValidArtifactLogicalPath 放行），但 basename 里含
	// 引号/分号/非 ASCII：这些正是能把头语法撑破的字符。
	paths := []string{
		`reports/报告 "v1".md`,
		`docs/a;b=c.md`,
		`deep/dir name/with space.txt`,
	}
	env := newArtifactEnv(t)
	mux := downloadMux(env)

	refs := make([]protocol.TaskArtifact, 0, len(paths))
	contents := make(map[string][]byte, len(paths))
	for i, p := range paths {
		if !protocol.ValidArtifactLogicalPath(p) {
			t.Fatalf("夹具路径 %q 不是合法逻辑路径（测试写错了）", p)
		}
		body := []byte("payload-" + strconv.Itoa(i) + "-" + p)
		refs = append(refs, env.seed(t, p, body))
		contents[p] = body
	}
	env.complete(t, refs...)

	for _, art := range refs {
		t.Run(art.Path, func(t *testing.T) {
			rr := env.do(mux, http.MethodGet, env.downloadURL(env.ns.Slug, env.task.ID, art.SHA256), nil, nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("HTTP 状态 = %d, want 200（body=%s）", rr.Code, rr.Body.String())
			}
			if !bytes.Equal(rr.Body.Bytes(), contents[art.Path]) {
				t.Fatalf("响应体 = %q, want %q", rr.Body.Bytes(), contents[art.Path])
			}
			raw := rr.Header().Get("Content-Disposition")
			if strings.ContainsAny(raw, "\r\n") {
				t.Fatalf("Content-Disposition 含 CR/LF: %q", raw)
			}
			want := path.Base(art.Path)
			if fn := downloadDispositionFilename(t, rr); fn != want {
				t.Fatalf("disposition filename = %q, want %q（头值=%q）", fn, want, raw)
			}
		})
	}
}

// ---------- D3：路径逃逸阴性 ----------

func TestArtifactDownloadPathEscapeIsNotFound(t *testing.T) {
	env := newArtifactEnv(t)
	content := []byte("secret-managed-bytes-should-never-leak")
	tk := seedReported(t, env, "reports/report.md", content)
	mux := downloadMux(env)

	// 这些形式都能**穿过**路由到达 handler（`{sha256}` 段被逐段反转义），
	// 因此必须由 handler/store 判 404——不是 500，也不是 400 泄露内部判据。
	// 注意 63 个 a 只是「形状对但不存在的 hash」：它必须与真实产物 hash 不同。
	if strings.Repeat("a", 63) == tk.SHA256 {
		t.Fatal("用例 sha 与真实产物 hash 撞了（测试写错了）")
	}
	for _, sha := range []string{"%2e%2e", "..%2F..%2F..%2Fetc%2Fpasswd", "%2e%2e%2F%2e%2e", "not-a-hash", strings.Repeat("a", 63)} {
		t.Run("sha="+sha, func(t *testing.T) {
			rr := env.do(mux, http.MethodGet, env.downloadURL(env.ns.Slug, env.task.ID, sha), nil, nil)
			downloadAssertStatus(t, rr, http.StatusNotFound, reasonNotFound, "非法 sha 段")
			downloadAssertNoArtifactBytes(t, rr, content, "非法 sha 段")
		})
	}

	// 原始（未转义）`..` 在 net/http 里到不了 handler：ServeMux 匹配前就规范掉路径并
	// 301 重定向。可接受的是「301 或 404」，不可接受的是 200（吐字节）或 500（把攻击
	// 探测伪装成故障）。
	rr := env.do(mux, http.MethodGet, env.downloadURL(env.ns.Slug, env.task.ID, ".."), nil, nil)
	switch rr.Code {
	case http.StatusMovedPermanently, http.StatusNotFound:
	default:
		t.Fatalf("原始 `..` 段: HTTP 状态 = %d, want 301（mux 规范化）或 404（body=%s）", rr.Code, rr.Body.String())
	}
	downloadAssertNoArtifactBytes(t, rr, content, "原始 `..` 段")
	if loc := rr.Header().Get("Location"); strings.Contains(loc, "..") {
		t.Fatalf("重定向 Location 仍含 `..`: %q", loc)
	}

	// handler 直连：证明「就算路由被绕过，`..` 也不会被当成查找键解开任何东西」。
	for _, sha := range []string{"..", "../..", "objects/obj_abc"} {
		t.Run("direct-sha="+sha, func(t *testing.T) {
			rr := downloadDirect(env, env.task.ID, sha)
			downloadAssertStatus(t, rr, http.StatusNotFound, reasonNotFound, "直连非法 sha")
			downloadAssertNoArtifactBytes(t, rr, content, "直连非法 sha")
		})
	}
}

// ---------- D4：符号链接阴性 ----------

func TestArtifactDownloadSymlinkedObjectIsExpired(t *testing.T) {
	env := newArtifactEnv(t)
	content := []byte("managed copy bytes")
	tk := seedReported(t, env, "reports/link.md", content)

	// 把托管对象换成一个指向**托管根之外** decoy 的符号链接。
	decoyContent := []byte("DECOY-MUST-NEVER-BE-SERVED")
	decoy := filepath.Join(t.TempDir(), "decoy.bin")
	if err := os.WriteFile(decoy, decoyContent, 0o600); err != nil {
		t.Fatalf("写 decoy: %v", err)
	}
	target := objectPath(t, env)
	if err := os.Remove(target); err != nil {
		t.Fatalf("移除托管对象: %v", err)
	}
	if err := os.Symlink(decoy, target); err != nil {
		t.Fatalf("造符号链接: %v", err)
	}

	mux := downloadMux(env)
	rr := env.do(mux, http.MethodGet, env.downloadURL(env.ns.Slug, env.task.ID, tk.SHA256), nil, nil)
	downloadAssertStatus(t, rr, http.StatusGone, reasonArtifactExpired, "符号链接对象")
	downloadAssertNoArtifactBytes(t, rr, decoyContent, "符号链接对象")
	downloadAssertNoArtifactBytes(t, rr, content, "符号链接对象")

	// decoy 本身不得被删（错误路径只许关句柄，不许动根外的东西）。
	stillThere, err := os.ReadFile(decoy)
	if err != nil {
		t.Fatalf("decoy 被删/不可读: %v", err)
	}
	if !bytes.Equal(stillThere, decoyContent) {
		t.Fatalf("decoy 内容被改: %q", stillThere)
	}
}

// ---------- D5：宿主文件 fallback 阴性 ----------

func TestArtifactDownloadNoHostFileFallback(t *testing.T) {
	env := newArtifactEnv(t)
	content := []byte("managed bytes for the fallback test")
	tk := seedReported(t, env, "reports/fallback.md", content)

	// 托管根之外放一个**同内容**的 decoy（模拟「节点/用户目录里有同名同内容的文件」）。
	decoyDir := t.TempDir()
	decoy := filepath.Join(decoyDir, "fallback.md")
	if err := os.WriteFile(decoy, content, 0o600); err != nil {
		t.Fatalf("写 decoy: %v", err)
	}
	// 删掉托管副本：元数据仍在（manifest 行还在），字节没了。
	if err := os.Remove(objectPath(t, env)); err != nil {
		t.Fatalf("移除托管对象: %v", err)
	}
	if n := env.countRows(t, `SELECT COUNT(*) FROM task_artifact_manifests WHERE task_id=?`, env.task.ID); n != 1 {
		t.Fatalf("manifest 行 = %d, want 1（元数据必须仍在，语义才是 410 而非 404）", n)
	}

	mux := downloadMux(env)
	rr := env.do(mux, http.MethodGet, env.downloadURL(env.ns.Slug, env.task.ID, tk.SHA256), nil, nil)
	downloadAssertStatus(t, rr, http.StatusGone, reasonArtifactExpired, "副本已清理")
	downloadAssertNoArtifactBytes(t, rr, content, "副本已清理")

	// decoy 仍在且内容未变：错误路径绝不读/删托管根之外的文件。
	stillThere, err := os.ReadFile(decoy)
	if err != nil {
		t.Fatalf("decoy 被删/不可读: %v", err)
	}
	if !bytes.Equal(stillThere, content) {
		t.Fatalf("decoy 内容被改: %q", stillThere)
	}
}

// ---------- D6：鉴权与命名空间隔离 ----------

func TestArtifactDownloadAuthAndNamespaceIsolation(t *testing.T) {
	env := newArtifactEnv(t)
	content := []byte("managed bytes")
	tk := seedReported(t, env, "reports/report.md", content)
	mux := downloadMux(env)
	target := env.downloadURL(env.ns.Slug, env.task.ID, tk.SHA256)

	// 未登录 → 401（会话中间件先挡住，根本进不到 handler）。
	rr := env.doAnon(mux, http.MethodGet, target)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("未登录: HTTP 状态 = %d, want 401（body=%s）", rr.Code, rr.Body.String())
	}
	downloadAssertNoArtifactBytes(t, rr, content, "未登录")

	// 不存在的命名空间 → 404。
	rr = env.do(mux, http.MethodGet, env.downloadURL("no-such-namespace", env.task.ID, tk.SHA256), nil, nil)
	downloadAssertStatus(t, rr, http.StatusNotFound, reasonNotFound, "未知命名空间")
	downloadAssertNoArtifactBytes(t, rr, content, "未知命名空间")

	// 跨命名空间：nsOther 是真实存在且当前用户可访问的命名空间，但任务属于 ns。
	// 必须 404，且与「任务不存在」逐字节同形（调用方不能据此探测别的 ns 里有没有这个 id）。
	rr = env.do(mux, http.MethodGet, env.downloadURL(env.nsOther.Slug, env.task.ID, tk.SHA256), nil, nil)
	downloadAssertStatus(t, rr, http.StatusNotFound, reasonNotFound, "跨 namespace")
	downloadAssertNoArtifactBytes(t, rr, content, "跨 namespace")

	// 同一请求打对的命名空间仍然 200（证明上面三条不是「全都 404」的假阴性）。
	if rr := env.do(mux, http.MethodGet, target, nil, nil); rr.Code != http.StatusOK {
		t.Fatalf("对照请求: HTTP 状态 = %d, want 200（body=%s）", rr.Code, rr.Body.String())
	}
}

// ---------- D6b：托管池未配置 ----------

func TestArtifactDownloadSpoolNotConfigured(t *testing.T) {
	env := newArtifactEnv(t)
	content := []byte("managed bytes")
	tk := seedReported(t, env, "reports/report.md", content)
	mux := downloadMux(env)
	target := env.downloadURL(env.ns.Slug, env.task.ID, tk.SHA256)

	// 部署缺配置：诚实的 503，不是「404 假装没有」也不是「500 假装坏了」，
	// 更不能临时去读宿主路径顶上。
	env.deps.Spool = nil
	rr := env.do(mux, http.MethodGet, target, nil, nil)
	downloadAssertStatus(t, rr, http.StatusServiceUnavailable, artifactErrDownloadUnavailable, "托管池未配置")
	downloadAssertNoArtifactBytes(t, rr, content, "托管池未配置")
}

// ---------- D7：历史 attempt 不能服务当前 attempt ----------

func TestArtifactDownloadHistoricalAttemptNotServed(t *testing.T) {
	env := newArtifactEnv(t)
	ctx := env.ctx
	content := []byte("attempt-1 bytes")
	art := env.seed(t, "reports/old-attempt.md", content)
	now := store.Now()

	// 序列与 store 的 D4 用例一致：attempt 1 只上传（不提交结果）→ 失联回收/收束 →
	// attempt 2 提交**无产物**的结果。于是 sha 只存在于历史 attempt 的 manifest 里。
	//
	// 夹具的能力 spec 已声明幂等 + maxAttempts=2（见 artifact_fixtures_test.go），
	// 所以失联回收后任务可重派——下面每一步都走真实 store API，授权链不打折。
	if _, _, err := env.db.RecoverLostLeases(ctx, now+60); err != nil {
		t.Fatalf("recover lost leases: %v", err)
	}
	if _, err := env.db.ReapTaskAttempt(ctx, store.ReapedInputs{
		TaskID: env.task.ID, Attempt: env.attempt, NodeID: env.node.ID,
		NodeTokenHash: env.node.TokenHash, LeaseID: env.leaseID, Slot: 0, Now: now + 60,
	}); err != nil {
		t.Fatalf("reap attempt: %v", err)
	}
	if err := env.db.AssignTaskAt(ctx, env.task.ID, env.node.ID, "asg-artifact-2", 2, now+65); err != nil {
		t.Fatalf("reassign: %v", err)
	}
	nodeAuth := store.NodeAuthorization{NodeID: env.node.ID, NodeTokenHash: env.node.TokenHash}
	lease, _, attempt, _, err := env.db.GrantLeaseAuthorized(ctx, nodeAuth, env.task.ID, "asg-artifact-2", 2, 0, now+65)
	if err != nil {
		t.Fatalf("grant next attempt: %v", err)
	}
	if attempt == env.attempt {
		t.Fatalf("夹具没有推进 attempt: %d", attempt)
	}
	if err := env.db.CompleteTaskAuthorized(ctx, nodeAuth, env.task.ID, attempt, lease, now+66,
		store.TaskResult{Phase: store.TaskSucceeded}); err != nil {
		t.Fatalf("complete next attempt: %v", err)
	}

	mux := downloadMux(env)
	rr := env.do(mux, http.MethodGet, env.downloadURL(env.ns.Slug, env.task.ID, art.SHA256), nil, nil)
	downloadAssertStatus(t, rr, http.StatusNotFound, reasonNotFound, "历史 attempt")
	downloadAssertNoArtifactBytes(t, rr, content, "历史 attempt")
	// 404 必须来自「当前 attempt 没引用它」，而不是别的闸门：当前 attempt 确实已上报、
	// manifest 行确实还在（挂在旧的 attempt 下）、托管字节也还在。
	if n := env.countRows(t, `SELECT COUNT(*) FROM tasks WHERE id=? AND reported=1 AND attempt=?`, env.task.ID, attempt); n != 1 {
		t.Fatalf("当前 attempt 未上报/未推进（=%d），404 的判据不成立", n)
	}
	if n := env.countRows(t, `SELECT COUNT(*) FROM task_artifact_manifests WHERE task_id=? AND attempt=?`, env.task.ID, env.attempt); n != 1 {
		t.Fatalf("旧 attempt 的 manifest 行 = %d, want 1", n)
	}
	if n := len(env.objectFiles(t)); n != 1 {
		t.Fatalf("托管对象数 = %d, want 1（404 的判据必须是引用关系，不是文件缺失）", n)
	}
}

// ---------- D8：全过期 → 410 ----------

func TestArtifactDownloadFullyExpiredIsGone(t *testing.T) {
	mutate := map[string]string{
		// 字节撤销事实（GC 写的 expired_at）：元数据保留、副本不可得。
		"expired_at": `UPDATE task_artifact_manifests SET expired_at=? WHERE task_id=?`,
		// 可信时间边界：expires_at 已过。
		"expires_at": `UPDATE task_artifact_manifests SET expires_at=? WHERE task_id=?`,
	}
	for name, stmt := range mutate {
		t.Run(name, func(t *testing.T) {
			env := newArtifactEnv(t)
			content := []byte("expired bytes")
			tk := seedReported(t, env, "reports/expired.md", content)

			if _, err := env.db.Exec(stmt, store.Now()-1, env.task.ID); err != nil {
				t.Fatalf("%s 落库: %v", name, err)
			}
			// 字节仍在托管目录：410 必须由**元数据**给出，不是靠文件缺失。
			if n := len(env.objectFiles(t)); n != 1 {
				t.Fatalf("托管对象数 = %d, want 1", n)
			}

			mux := downloadMux(env)
			rr := env.do(mux, http.MethodGet, env.downloadURL(env.ns.Slug, env.task.ID, tk.SHA256), nil, nil)
			downloadAssertStatus(t, rr, http.StatusGone, reasonArtifactExpired, name)
			downloadAssertNoArtifactBytes(t, rr, content, name)
		})
	}
}

// ---------- D9：expires_at=0 永久 ----------

func TestArtifactDownloadZeroExpiryIsPermanent(t *testing.T) {
	env := newArtifactEnv(t)
	content := []byte("permanent bytes")
	tk := seedReported(t, env, "reports/keep-forever.md", content)
	if _, err := env.db.Exec(`UPDATE task_artifact_manifests SET expires_at=0 WHERE task_id=?`, env.task.ID); err != nil {
		t.Fatalf("置 expires_at=0: %v", err)
	}
	mux := downloadMux(env)
	target := env.downloadURL(env.ns.Slug, env.task.ID, tk.SHA256)

	// ① 当下：200 + 正确字节。
	rr := env.do(mux, http.MethodGet, target, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expires_at=0 当下: HTTP 状态 = %d, want 200（body=%s）", rr.Code, rr.Body.String())
	}
	if !bytes.Equal(rr.Body.Bytes(), content) {
		t.Fatalf("expires_at=0 当下: 响应体 = %q, want %q", rr.Body.Bytes(), content)
	}

	// ② 远期仍然 200：`deps.Now` 是既有的可注入时钟接缝（不改生产代码），
	//    把它推到 50 年后，永久条目不得过期。
	//
	//    单位必须是**毫秒**（serverDeps.Now 的契约，#340）：注入秒会被 handler 的
	//    秒折算除成 1970 年，这条断言就会在「时钟根本没生效」的情况下也变绿。
	env.deps.Now = func() int64 { return time.Now().AddDate(50, 0, 0).UnixMilli() }
	rr = env.do(mux, http.MethodGet, target, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expires_at=0 远期: HTTP 状态 = %d, want 200（permanent 语义，body=%s）", rr.Code, rr.Body.String())
	}
	if !bytes.Equal(rr.Body.Bytes(), content) {
		t.Fatalf("expires_at=0 远期: 响应体 = %q, want %q", rr.Body.Bytes(), content)
	}
}

// ---------- D10：task 作用域 + reported 闸门 ----------

func TestArtifactDownloadTaskScopeAndReportedGate(t *testing.T) {
	t.Run("other-task-not-served", func(t *testing.T) {
		env := newArtifactEnv(t)
		content := []byte("bytes owned by env.task")
		tk := seedReported(t, env, "reports/report.md", content)
		mux := downloadMux(env)

		// 同样的 ns、同样的 hash、字节也还躺在托管目录里，但 URL 里的 task 不是它的 owner。
		otherTaskID := store.NewTaskID()
		if otherTaskID == env.task.ID {
			t.Fatal("任务 id 撞了（测试写错了）")
		}
		rr := env.do(mux, http.MethodGet, env.downloadURL(env.ns.Slug, otherTaskID, tk.SHA256), nil, nil)
		downloadAssertStatus(t, rr, http.StatusNotFound, reasonNotFound, "别 task 同 hash")
		downloadAssertNoArtifactBytes(t, rr, content, "别 task 同 hash")

		// 对照：owning task 仍然 200。
		if rr := env.do(mux, http.MethodGet, env.downloadURL(env.ns.Slug, env.task.ID, tk.SHA256), nil, nil); rr.Code != http.StatusOK {
			t.Fatalf("对照请求: HTTP 状态 = %d, want 200（body=%s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("unreported-and-unreferenced", func(t *testing.T) {
		env := newArtifactEnv(t)
		content := []byte("bytes with no result reference")
		art := env.seed(t, "reports/report.md", content)
		mux := downloadMux(env)
		target := env.downloadURL(env.ns.Slug, env.task.ID, art.SHA256)

		// ① 结果还没上报（tasks.reported=0）：manifest 行在也不算「可下载」。
		rr := env.do(mux, http.MethodGet, target, nil, nil)
		downloadAssertStatus(t, rr, http.StatusNotFound, reasonNotFound, "未上报")
		downloadAssertNoArtifactBytes(t, rr, content, "未上报")

		// ② 上报了但结果**没有引用**这条 manifest（result_ref=0）→ 仍然 404。
		env.complete(t)
		rr = env.do(mux, http.MethodGet, target, nil, nil)
		downloadAssertStatus(t, rr, http.StatusNotFound, reasonNotFound, "未被结果引用")
		downloadAssertNoArtifactBytes(t, rr, content, "未被结果引用")
	})
}
