package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 A1 上传端点（#332 / G3-D3）的验收套件：真 httptest + 真 sqlite + 真托管池。
//
// 三条断言纪律，每条用例都遵守：
//   - 成功用例必须同时核对「响应恰好 5 键且与请求四元组一致」「托管副本字节/权限」
//     「manifest 行内容」——只断言状态码会把「登记成功但字节丢了」当 PASS；
//   - 拒绝用例必须同时核对 HTTP 码 + `{"error":"<code>"}` 错误体 + **零写**
//     （manifest 0 行 / ticket 0 行 / objects 无文件）：产物端点的失败语义是
//     「拒绝 = 什么都别留下」，只看状态码会漏掉「已登记但返回 403」这类最坏形态；
//   - 每个拒绝用例都必须走真授权链（真 node token + 真 lease/attemptToken），
//     不用 stub 伪造——拒绝的判据本身就是 store 在事务里重做的授权。

// uploadMux 把上传路由挂到一个**新建**的 mux 上（与 mux.go 的接线形状一致）。
func uploadMux(env *artifactEnv) *http.ServeMux {
	return uploadMuxWithLogger(env, env.deps.logger())
}

// uploadMuxWithLogger 允许注入 logger（用于断言 token/令牌不落日志）。
func uploadMuxWithLogger(env *artifactEnv, log *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("PUT "+protocol.NodeTaskArtifactsPathPrefix+"{taskId}"+protocol.NodeTaskArtifactsPathInfix+"{sha256}",
		newArtifactUploadHandler(env.db, env.spool, log))
	return mux
}

// uploadPut 发一次上传。contentLength 显式给：-1 表示 chunked（长度未知）路径。
func uploadPut(t *testing.T, mux *http.ServeMux, target string, body io.Reader, contentLength int64, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, target, body)
	req.ContentLength = contentLength
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

// uploadAssertError 断言一次拒绝：HTTP 码 + 恰好一个 error 键 + A1 码。
func uploadAssertError(t *testing.T, rr *httptest.ResponseRecorder, wantStatus int, wantError string) {
	t.Helper()
	if rr.Code != wantStatus {
		t.Fatalf("HTTP 状态 = %d, want %d（body=%s）", rr.Code, wantStatus, rr.Body.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("错误体不是 JSON 对象: %v（body=%s）", err, rr.Body.String())
	}
	if len(payload) != 1 {
		t.Fatalf("错误体 = %v, want 恰好一个 error 键（A1 动作文档形状）", payload)
	}
	if payload["error"] != wantError {
		t.Fatalf("错误码 = %q, want %q（body=%s）", payload["error"], wantError, rr.Body.String())
	}
}

// uploadDecodeResponse 断言成功响应**恰好 5 键**，并用节点侧的严格解码器复核。
//
// 用 protocol.DecodeArtifactUploadResponse 而不是本地 struct：节点客户端就是这套
// DisallowUnknownFields + Validate，多一个字段/少一个字段在节点侧都是传输失败。
func uploadDecodeResponse(t *testing.T, rr *httptest.ResponseRecorder) protocol.ArtifactUploadResponse {
	t.Helper()
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应不是 JSON 对象: %v（body=%s）", err, rr.Body.String())
	}
	want := []string{"taskId", "attempt", "path", "sha256", "size"}
	if len(raw) != len(want) {
		t.Fatalf("响应键 = %v, want 恰好 %d 键 %v", uploadKeys(raw), len(want), want)
	}
	for _, key := range want {
		if _, ok := raw[key]; !ok {
			t.Fatalf("响应缺键 %q（实际 %v）", key, uploadKeys(raw))
		}
	}
	decoded, err := protocol.DecodeArtifactUploadResponse(rr.Body.Bytes())
	if err != nil {
		t.Fatalf("节点侧严格解码失败: %v（body=%s）", err, rr.Body.String())
	}
	return *decoded
}

func uploadKeys(raw map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	return keys
}

// uploadAssertResponseEcho 断言响应回显的四元组与请求一致。
func uploadAssertResponseEcho(t *testing.T, got protocol.ArtifactUploadResponse, env *artifactEnv, logicalPath, sha string, size int64) {
	t.Helper()
	if got.TaskID != env.task.ID {
		t.Fatalf("taskId = %q, want %q", got.TaskID, env.task.ID)
	}
	if got.Attempt != env.attempt {
		t.Fatalf("attempt = %d, want %d", got.Attempt, env.attempt)
	}
	if got.Path != logicalPath {
		t.Fatalf("path = %q, want %q", got.Path, logicalPath)
	}
	if got.SHA256 != sha {
		t.Fatalf("sha256 = %q, want %q", got.SHA256, sha)
	}
	if got.Size != size {
		t.Fatalf("size = %d, want %d", got.Size, size)
	}
}

// uploadManifest 读当前 attempt 的 manifest（store 的权威读模型）。
func uploadManifest(t *testing.T, env *artifactEnv) []store.ArtifactManifestEntry {
	t.Helper()
	entries, err := env.db.ListArtifactManifest(env.ctx, env.ns.ID, env.task.ID, env.attempt)
	if err != nil {
		t.Fatalf("list artifact manifest: %v", err)
	}
	return entries
}

// uploadAssertPublishedObject 断言托管副本就是这次上传的字节且是 0600。
func uploadAssertPublishedObject(t *testing.T, env *artifactEnv, objectID string, want []byte) {
	t.Helper()
	path, err := env.spool.ObjectPath(objectID)
	if err != nil {
		t.Fatalf("object path %q: %v", objectID, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("托管副本 %s 不存在: %v", objectID, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("托管副本权限 = %o, want 600", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读托管副本 %s: %v", objectID, err)
	}
	if len(data) != len(want) {
		t.Fatalf("托管副本大小 = %d, want %d", len(data), len(want))
	}
	if sha := artifactSHA(data); sha != artifactSHA(want) {
		t.Fatalf("托管副本 sha256 = %s, want %s", sha, artifactSHA(want))
	}
}

// uploadTmpFiles 列出草稿目录里的残留（成功的上传必须一个都不留）。
func uploadTmpFiles(t *testing.T, env *artifactEnv) []string {
	t.Helper()
	entries, err := os.ReadDir(env.spool.Root() + "/tmp")
	if err != nil {
		t.Fatalf("read spool tmp: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	return out
}

// uploadTicketCount 数某个任务上仍在的 ticket（拒绝路径必须为 0）。
func uploadTicketCount(t *testing.T, env *artifactEnv, taskID string) int {
	t.Helper()
	return env.countRows(t, `SELECT COUNT(*) FROM task_artifact_uploads WHERE task_id=?`, taskID)
}

// uploadAssertNoWritesForTask 是 assertNoArtifactWrites 的按 taskId 版本
// （跨 ns 用例要断言的是**另一个**任务上没有产物事实）。
func uploadAssertNoWritesForTask(t *testing.T, env *artifactEnv, taskID string) {
	t.Helper()
	if n := env.countRows(t, `SELECT COUNT(*) FROM task_artifact_manifests WHERE task_id=?`, taskID); n != 0 {
		t.Fatalf("task %s manifest rows = %d, want 0", taskID, n)
	}
	if n := uploadTicketCount(t, env, taskID); n != 0 {
		t.Fatalf("task %s active tickets = %d, want 0", taskID, n)
	}
}

// uploadMutateOnRead 在 handler 第一次读 body 时执行一次 mutate。
//
// 为什么需要它：A1 的授权是**两段式**（Begin 预检 → 登记时重做），要覆盖
// 「Begin 之后、Register 之前授权事实变了」（租约过期 / ticket 过期）只能在这段
// 窗口里改库。httptest 的 mux.ServeHTTP 与测试同 goroutine，mutate 里可以直接用 t。
type uploadMutateOnRead struct {
	r      io.Reader
	mutate func()
	read   int
	once   sync.Once
}

func (u *uploadMutateOnRead) Read(p []byte) (int, error) {
	u.once.Do(u.mutate)
	n, err := u.r.Read(p)
	u.read += n
	return n, err
}

// uploadUnreadableBody 记录被读次数：413（声明超限）必须**一次都不读**。
type uploadUnreadableBody struct{ reads int }

func (b *uploadUnreadableBody) Read([]byte) (int, error) {
	b.reads++
	return 0, fmt.Errorf("body 不该被读")
}

// uploadSeedItems 用真授权链预登记 n 条产物（每条的字节都真的落在托管目录里）。
func uploadSeedItems(t *testing.T, env *artifactEnv, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("bulk/%03d.txt", i)
		content := []byte(fmt.Sprintf("seeded-item-%03d", i))
		env.seed(t, path, content)
	}
}

// T1：首次上传 201，响应五键回显、托管副本字节/权限正确、manifest 行内容正确。
func TestArtifactUploadFirstPutCreated(t *testing.T) {
	env := newArtifactEnv(t)
	var logs bytes.Buffer
	mux := uploadMuxWithLogger(env, slog.New(slog.NewTextHandler(&logs, nil)))

	content := []byte("# 产物\nhello artifact\n")
	sha := artifactSHA(content)
	logicalPath := "export/report.md"
	const wantStatus = http.StatusCreated

	rr := uploadPut(t, mux, env.uploadURL(sha), bytes.NewReader(content), int64(len(content)), env.uploadHeaders(logicalPath))
	if rr.Code != wantStatus {
		t.Fatalf("HTTP 状态 = %d, want %d（body=%s）", rr.Code, wantStatus, rr.Body.String())
	}
	resp := uploadDecodeResponse(t, rr)
	uploadAssertResponseEcho(t, resp, env, logicalPath, sha, int64(len(content)))

	files := env.objectFiles(t)
	if len(files) != 1 {
		t.Fatalf("objects = %v, want 恰好 1 个对象文件", files)
	}
	entries := uploadManifest(t, env)
	if len(entries) != 1 {
		t.Fatalf("manifest rows = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.Path != logicalPath || entry.SHA256 != sha || entry.Size != int64(len(content)) || entry.Attempt != env.attempt {
		t.Fatalf("manifest 行 = %+v, want path=%q sha=%q size=%d attempt=%d",
			entry, logicalPath, sha, len(content), env.attempt)
	}
	if entry.ObjectID.String() != files[0] {
		t.Fatalf("manifest object = %q, want 托管目录里那个 %q", entry.ObjectID.String(), files[0])
	}
	if got := entry.ExpiresAt - entry.PublishedAt; got != store.DefaultRetentionResultTTLSec {
		t.Fatalf("manifest 保留期 = %d 秒, want %d（保留期事实源是 store 的默认 TTL）", got, store.DefaultRetentionResultTTLSec)
	}
	uploadAssertPublishedObject(t, env, entry.ObjectID.String(), content)

	if n := uploadTicketCount(t, env, env.task.ID); n != 0 {
		t.Fatalf("active tickets = %d, want 0（成功登记后 ticket 必须被消费掉）", n)
	}
	if tmp := uploadTmpFiles(t, env); len(tmp) != 0 {
		t.Fatalf("spool tmp 残留 = %v, want 空", tmp)
	}
	// token 明文（含 attemptToken / leaseId 这类凭据）绝不进日志。
	for _, secret := range []string{env.nodeToken, env.attemptTok, env.leaseID} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("日志里出现了凭据明文 %q：%s", secret, logs.String())
		}
	}
}

// T2：同 task+attempt+path+hash 重复上传 → 200，manifest 仍 1 行，objects 不留第二份。
func TestArtifactUploadDuplicateIsIdempotent(t *testing.T) {
	env := newArtifactEnv(t)
	mux := uploadMux(env)

	content := []byte("same bytes twice")
	sha := artifactSHA(content)
	logicalPath := "export/idempotent.txt"
	headers := env.uploadHeaders(logicalPath)

	first := uploadPut(t, mux, env.uploadURL(sha), bytes.NewReader(content), int64(len(content)), headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("首次上传状态 = %d, want 201（body=%s）", first.Code, first.Body.String())
	}
	second := uploadPut(t, mux, env.uploadURL(sha), bytes.NewReader(content), int64(len(content)), env.uploadHeaders(logicalPath))
	if second.Code != http.StatusOK {
		t.Fatalf("重复上传状态 = %d, want 200（body=%s）", second.Code, second.Body.String())
	}
	uploadAssertResponseEcho(t, uploadDecodeResponse(t, second), env, logicalPath, sha, int64(len(content)))

	if entries := uploadManifest(t, env); len(entries) != 1 {
		t.Fatalf("manifest rows = %d, want 1（幂等重传不得新增行）", len(entries))
	}
	files := env.objectFiles(t)
	if len(files) != 1 {
		t.Fatalf("objects = %v, want 恰好 1 个对象文件（200 幂等时本次副本是多余字节，必须删掉）", files)
	}
	uploadAssertPublishedObject(t, env, files[0], content)
	if n := uploadTicketCount(t, env, env.task.ID); n != 0 {
		t.Fatalf("active tickets = %d, want 0", n)
	}
}

// T3：同 attempt 同逻辑 path 但不同 hash → 409 artifact_conflict，且不新增任何行/文件。
func TestArtifactUploadSamePathDifferentHashConflicts(t *testing.T) {
	env := newArtifactEnv(t)
	mux := uploadMux(env)

	logicalPath := "export/conflict.txt"
	firstContent := []byte("first version")
	firstSHA := artifactSHA(firstContent)
	first := uploadPut(t, mux, env.uploadURL(firstSHA), bytes.NewReader(firstContent), int64(len(firstContent)), env.uploadHeaders(logicalPath))
	if first.Code != http.StatusCreated {
		t.Fatalf("首次上传状态 = %d, want 201（body=%s）", first.Code, first.Body.String())
	}

	secondContent := []byte("second version")
	secondSHA := artifactSHA(secondContent)
	second := uploadPut(t, mux, env.uploadURL(secondSHA), bytes.NewReader(secondContent), int64(len(secondContent)), env.uploadHeaders(logicalPath))
	uploadAssertError(t, second, http.StatusConflict, protocol.ArtifactErrConflict)

	entries := uploadManifest(t, env)
	if len(entries) != 1 || entries[0].SHA256 != firstSHA {
		t.Fatalf("manifest = %+v, want 只剩首次那行（sha=%s）", entries, firstSHA)
	}
	files := env.objectFiles(t)
	if len(files) != 1 {
		t.Fatalf("objects = %v, want 恰好 1 个（冲突不得发布新字节）", files)
	}
	if n := uploadTicketCount(t, env, env.task.ID); n != 0 {
		t.Fatalf("active tickets = %d, want 0", n)
	}
}

// T4：body 实际 sha 与 URL sha 不符 → 400 artifact_hash_mismatch 且零写。
func TestArtifactUploadBodySHA256MismatchRejected(t *testing.T) {
	env := newArtifactEnv(t)
	mux := uploadMux(env)

	declared := []byte("AAAA-declared-bytes")
	actual := []byte("BBBB-declared-bytes") // 同长度，只有内容不同
	sha := artifactSHA(declared)
	if len(declared) != len(actual) {
		t.Fatalf("用例构造错误：两个 body 长度必须相同（%d vs %d）", len(declared), len(actual))
	}

	rr := uploadPut(t, mux, env.uploadURL(sha), bytes.NewReader(actual), int64(len(actual)), env.uploadHeaders("export/mismatch.txt"))
	uploadAssertError(t, rr, http.StatusBadRequest, protocol.ArtifactErrHashMismatch)
	env.assertNoArtifactWrites(t)
	if tmp := uploadTmpFiles(t, env); len(tmp) != 0 {
		t.Fatalf("spool tmp 残留 = %v, want 空", tmp)
	}
}

// T5：单件超过 16 MiB → 413 artifact_too_large。
//
// 两个分支都要：声明长度超限（必须一次都不读 body、不建 ticket）与
// chunked 实收超限（由草稿上限挡住）。两者都零写。
func TestArtifactUploadSingleItemTooLargeRejected(t *testing.T) {
	t.Run("declared_length_over_limit_not_read", func(t *testing.T) {
		env := newArtifactEnv(t)
		mux := uploadMux(env)
		body := &uploadUnreadableBody{}
		oversized := protocol.MaxArtifactTransferBytes + 1
		rr := uploadPut(t, mux, env.uploadURL(artifactSHA([]byte("x"))), body, oversized, env.uploadHeaders("export/huge.bin"))
		uploadAssertError(t, rr, http.StatusRequestEntityTooLarge, protocol.ArtifactErrTooLarge)
		if body.reads != 0 {
			t.Fatalf("声明长度已超限时 handler 仍读了 %d 次 body（应当先 413 再拒读）", body.reads)
		}
		env.assertNoArtifactWrites(t)
	})

	t.Run("chunked_actual_over_limit", func(t *testing.T) {
		env := newArtifactEnv(t)
		mux := uploadMux(env)
		oversized := make([]byte, protocol.MaxArtifactTransferBytes+1)
		rr := uploadPut(t, mux, env.uploadURL(artifactSHA(nil)), bytes.NewReader(oversized), -1, env.uploadHeaders("export/huge-chunked.bin"))
		uploadAssertError(t, rr, http.StatusRequestEntityTooLarge, protocol.ArtifactErrTooLarge)
		env.assertNoArtifactWrites(t)
		if tmp := uploadTmpFiles(t, env); len(tmp) != 0 {
			t.Fatalf("spool tmp 残留 = %v, want 空", tmp)
		}
	})
}

// T6：单 attempt 第 33 个条目 → 413（已经登记的 32 条保持原样，且不残留第 33 份字节）。
func TestArtifactUploadItemCountLimitRejected(t *testing.T) {
	env := newArtifactEnv(t)
	mux := uploadMux(env)
	uploadSeedItems(t, env, protocol.MaxArtifactTransferItems)

	if entries := uploadManifest(t, env); len(entries) != protocol.MaxArtifactTransferItems {
		t.Fatalf("预置 manifest rows = %d, want %d", len(entries), protocol.MaxArtifactTransferItems)
	}
	if files := env.objectFiles(t); len(files) != protocol.MaxArtifactTransferItems {
		t.Fatalf("预置 objects = %d, want %d", len(files), protocol.MaxArtifactTransferItems)
	}

	content := []byte("one item too many")
	sha := artifactSHA(content)
	rr := uploadPut(t, mux, env.uploadURL(sha), bytes.NewReader(content), int64(len(content)), env.uploadHeaders("bulk/033.txt"))
	uploadAssertError(t, rr, http.StatusRequestEntityTooLarge, protocol.ArtifactErrTooLarge)

	if entries := uploadManifest(t, env); len(entries) != protocol.MaxArtifactTransferItems {
		t.Fatalf("manifest rows = %d, want %d（超限条目不得登记）", len(entries), protocol.MaxArtifactTransferItems)
	}
	if files := env.objectFiles(t); len(files) != protocol.MaxArtifactTransferItems {
		t.Fatalf("objects = %d, want %d（超限条目已发布的字节必须补偿删除）", len(files), protocol.MaxArtifactTransferItems)
	}
	if n := uploadTicketCount(t, env, env.task.ID); n != 0 {
		t.Fatalf("active tickets = %d, want 0", n)
	}
}

// T7：单 attempt 累计字节超 64 MiB → 413（配额按 manifest 求和，不按当前请求）。
func TestArtifactUploadAttemptByteBudgetRejected(t *testing.T) {
	env := newArtifactEnv(t)
	mux := uploadMux(env)

	// 4 × 16 MiB = 64 MiB 恰好用满单 attempt 预算（每件都在单件上限内）。
	big := make([]byte, protocol.MaxArtifactTransferBytes)
	for i := 0; i < 4; i++ {
		big[0] = byte(i) // 让 4 份内容不同（哈希不同），更贴近真实
		env.seed(t, fmt.Sprintf("budget/%d.bin", i), big)
	}
	if entries := uploadManifest(t, env); len(entries) != 4 {
		t.Fatalf("预置 manifest rows = %d, want 4", len(entries))
	}

	content := []byte("over budget")
	sha := artifactSHA(content)
	rr := uploadPut(t, mux, env.uploadURL(sha), bytes.NewReader(content), int64(len(content)), env.uploadHeaders("budget/one-more.txt"))
	uploadAssertError(t, rr, http.StatusRequestEntityTooLarge, protocol.ArtifactErrTooLarge)

	if entries := uploadManifest(t, env); len(entries) != 4 {
		t.Fatalf("manifest rows = %d, want 4（超预算条目不得登记）", len(entries))
	}
	if files := env.objectFiles(t); len(files) != 4 {
		t.Fatalf("objects = %d, want 4（超预算条目已发布的字节必须补偿删除）", len(files))
	}
	if n := uploadTicketCount(t, env, env.task.ID); n != 0 {
		t.Fatalf("active tickets = %d, want 0", n)
	}
}

// T8：授权被撤的各种形态 → 401/403（节点侧终止语义），每种都零写。
func TestArtifactUploadRevokedAuthorizationRejected(t *testing.T) {
	content := []byte("artifact bytes")
	sha := artifactSHA(content)
	cases := []struct {
		name   string
		mutate func(t *testing.T, env *artifactEnv)
		status int
		code   string
	}{
		{
			name:   "node_token_rotated",
			mutate: func(t *testing.T, env *artifactEnv) { env.rotateNodeToken(t) },
			status: http.StatusUnauthorized,
			code:   artifactUploadErrUnauthorized,
		},
		{
			name:   "lease_expired",
			mutate: func(t *testing.T, env *artifactEnv) { env.setLeaseUntil(t, store.Now()-1) },
			status: http.StatusForbidden,
			code:   artifactUploadErrForbidden,
		},
		{
			name:   "task_cancel_requested",
			mutate: func(t *testing.T, env *artifactEnv) { env.cancelTask(t) },
			status: http.StatusForbidden,
			code:   artifactUploadErrForbidden,
		},
		{
			name:   "attempt_mismatch",
			mutate: func(t *testing.T, env *artifactEnv) { env.bumpAttempt(t) },
			status: http.StatusForbidden,
			code:   artifactUploadErrForbidden,
		},
		{
			name:   "namespace_authorization_revoked",
			mutate: func(t *testing.T, env *artifactEnv) { env.denyNamespace(t) },
			status: http.StatusForbidden,
			code:   artifactUploadErrForbidden,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newArtifactEnv(t)
			mux := uploadMux(env)
			tc.mutate(t, env)
			rr := uploadPut(t, mux, env.uploadURL(sha), bytes.NewReader(content), int64(len(content)), env.uploadHeaders("export/revoked.txt"))
			uploadAssertError(t, rr, tc.status, tc.code)
			env.assertNoArtifactWrites(t)
			if tmp := uploadTmpFiles(t, env); len(tmp) != 0 {
				t.Fatalf("spool tmp 残留 = %v, want 空", tmp)
			}
		})
	}
}

// T9（rollback）：Begin 成功、字节已发布，但登记前租约过期 → 403 + 零写 + 无残留副本。
//
// 这条是「先发布再登记」补偿语义的核心用例：manifest 不能有行、objects 不能留文件。
func TestArtifactUploadMidFlightLeaseExpiryRollsBack(t *testing.T) {
	env := newArtifactEnv(t)
	mux := uploadMux(env)

	content := []byte("rollback these bytes")
	sha := artifactSHA(content)
	body := &uploadMutateOnRead{r: bytes.NewReader(content), mutate: func() { env.setLeaseUntil(t, store.Now()-1) }}
	rr := uploadPut(t, mux, env.uploadURL(sha), body, int64(len(content)), env.uploadHeaders("export/rollback.txt"))
	if body.read == 0 {
		t.Fatalf("handler 一次都没读 body，用例没走到「Begin 之后」的窗口")
	}
	uploadAssertError(t, rr, http.StatusForbidden, artifactUploadErrForbidden)
	env.assertNoArtifactWrites(t)
	if tmp := uploadTmpFiles(t, env); len(tmp) != 0 {
		t.Fatalf("spool tmp 残留 = %v, want 空", tmp)
	}
}

// T10：非法逻辑路径头（`..` / 绝对路径 / NUL / 坏转义 / 缺失）→ 400 artifact_path_invalid。
func TestArtifactUploadInvalidPathHeaderRejected(t *testing.T) {
	content := []byte("path header cases")
	sha := artifactSHA(content)
	cases := []struct {
		name   string
		mutate func(header map[string]string)
	}{
		{"dot_dot_segment", func(h map[string]string) { h[protocol.ArtifactHeaderArtifactPath] = "..%2Fsecret.txt" }},
		{"absolute_path", func(h map[string]string) { h[protocol.ArtifactHeaderArtifactPath] = "%2Fetc%2Fpasswd" }},
		{"nul_byte", func(h map[string]string) { h[protocol.ArtifactHeaderArtifactPath] = "a%00b.txt" }},
		{"bad_escape", func(h map[string]string) { h[protocol.ArtifactHeaderArtifactPath] = "%zz-broken" }},
		{"missing_header", func(h map[string]string) { delete(h, protocol.ArtifactHeaderArtifactPath) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newArtifactEnv(t)
			mux := uploadMux(env)
			headers := env.uploadHeaders("export/original.txt")
			tc.mutate(headers)
			rr := uploadPut(t, mux, env.uploadURL(sha), bytes.NewReader(content), int64(len(content)), headers)
			uploadAssertError(t, rr, http.StatusBadRequest, protocol.ArtifactErrPathInvalid)
			env.assertNoArtifactWrites(t)
		})
	}
}

// T10（续）：`{sha256}` 段不是 64 位小写 hex → 400 artifact_path_invalid。
func TestArtifactUploadInvalidSHASegmentRejected(t *testing.T) {
	valid := artifactSHA([]byte("sha segment cases"))
	cases := []struct{ name, sha string }{
		{"uppercase", strings.ToUpper(valid)},
		{"too_short", valid[:63]},
		{"non_hex", strings.Repeat("g", 64)},
		{"spec_digest_prefix", "sha256:" + valid},
		{"plain_word", "not-a-sha"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newArtifactEnv(t)
			mux := uploadMux(env)
			content := []byte("sha segment cases")
			rr := uploadPut(t, mux, env.uploadURL(tc.sha), bytes.NewReader(content), int64(len(content)), env.uploadHeaders("export/sha.txt"))
			uploadAssertError(t, rr, http.StatusBadRequest, protocol.ArtifactErrPathInvalid)
			env.assertNoArtifactWrites(t)
		})
	}
}

// T11：attempt / lease / attemptToken / 节点 token 任一缺失或不可解析 → 401。
func TestArtifactUploadMissingCredentialsRejected(t *testing.T) {
	content := []byte("credential cases")
	sha := artifactSHA(content)
	cases := []struct {
		name   string
		mutate func(header map[string]string)
	}{
		{"missing_node_token", func(h map[string]string) { delete(h, protocol.EmitAuthHeader) }},
		{"missing_attempt", func(h map[string]string) { delete(h, protocol.ArtifactHeaderAttempt) }},
		{"non_numeric_attempt", func(h map[string]string) { h[protocol.ArtifactHeaderAttempt] = "not-a-number" }},
		{"zero_attempt", func(h map[string]string) { h[protocol.ArtifactHeaderAttempt] = "0" }},
		{"missing_lease_id", func(h map[string]string) { delete(h, protocol.ArtifactHeaderLeaseID) }},
		{"missing_task_token", func(h map[string]string) { delete(h, protocol.ArtifactHeaderTaskToken) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newArtifactEnv(t)
			mux := uploadMux(env)
			headers := env.uploadHeaders("export/credentials.txt")
			tc.mutate(headers)
			rr := uploadPut(t, mux, env.uploadURL(sha), bytes.NewReader(content), int64(len(content)), headers)
			uploadAssertError(t, rr, http.StatusUnauthorized, artifactUploadErrUnauthorized)
			env.assertNoArtifactWrites(t)
		})
	}
}

// T12：跨命名空间隔离——本节点拿另一 ns 的 taskId 上传 → 401/403，且两个任务都零写。
func TestArtifactUploadCrossNamespaceTaskRejected(t *testing.T) {
	env := newArtifactEnv(t)
	mux := uploadMux(env)

	// 另一 ns 里一条真实的 pending 任务（真事实，不是编造的 id）。
	other := &store.Task{
		NamespaceID:  env.nsOther.ID,
		TraceID:      "trace-cross-ns",
		CapabilityID: env.cap.ID,
		Capability:   env.cap.Name,
		Version:      env.cap.Version,
		Digest:       env.cap.Digest,
		Params:       []byte(`{}`),
	}
	if err := env.db.CreateTasks(env.ctx, []*store.Task{other}); err != nil {
		t.Fatalf("create cross-ns task: %v", err)
	}

	content := []byte("cross namespace bytes")
	sha := artifactSHA(content)
	target := protocol.NodeTaskArtifactsPathPrefix + other.ID + protocol.NodeTaskArtifactsPathInfix + sha
	rr := uploadPut(t, mux, target, bytes.NewReader(content), int64(len(content)), env.uploadHeaders("export/cross-ns.txt"))
	if rr.Code != http.StatusUnauthorized && rr.Code != http.StatusForbidden {
		t.Fatalf("HTTP 状态 = %d, want 401 或 403（body=%s）", rr.Code, rr.Body.String())
	}
	uploadAssertError(t, rr, http.StatusForbidden, artifactUploadErrForbidden)

	uploadAssertNoWritesForTask(t, env, other.ID)
	env.assertNoArtifactWrites(t)
}

// T13：ticket 在传输途中过期 → 换 ticket + 原地改名，重试一次后仍 201，
// objects 只剩最终 id 那一个文件（不是两份）。
func TestArtifactUploadExpiredTicketRetriesWithMove(t *testing.T) {
	env := newArtifactEnv(t)
	mux := uploadMux(env)

	content := []byte("slow link bytes")
	sha := artifactSHA(content)
	logicalPath := "export/slow.txt"
	body := &uploadMutateOnRead{r: bytes.NewReader(content), mutate: func() { env.expireUploadTicket(t) }}
	rr := uploadPut(t, mux, env.uploadURL(sha), body, int64(len(content)), env.uploadHeaders(logicalPath))
	if body.read == 0 {
		t.Fatalf("handler 一次都没读 body，用例没走到「ticket 已建、登记未做」的窗口")
	}
	if rr.Code != http.StatusCreated {
		t.Fatalf("HTTP 状态 = %d, want 201（body=%s）", rr.Code, rr.Body.String())
	}
	uploadAssertResponseEcho(t, uploadDecodeResponse(t, rr), env, logicalPath, sha, int64(len(content)))

	entries := uploadManifest(t, env)
	if len(entries) != 1 {
		t.Fatalf("manifest rows = %d, want 1", len(entries))
	}
	files := env.objectFiles(t)
	if len(files) != 1 {
		t.Fatalf("objects = %v, want 只剩最终 id 一个文件（旧 id 必须被 Move 掉）", files)
	}
	if entries[0].ObjectID.String() != files[0] {
		t.Fatalf("manifest object = %q, want 托管目录里那个 %q", entries[0].ObjectID.String(), files[0])
	}
	uploadAssertPublishedObject(t, env, files[0], content)
	if n := uploadTicketCount(t, env, env.task.ID); n != 0 {
		t.Fatalf("active tickets = %d, want 0", n)
	}
	if tmp := uploadTmpFiles(t, env); len(tmp) != 0 {
		t.Fatalf("spool tmp 残留 = %v, want 空", tmp)
	}
}

// T14：未知节点 token → 401，且响应体与日志都不含 token 明文。
func TestArtifactUploadUnknownNodeTokenRejected(t *testing.T) {
	env := newArtifactEnv(t)
	var logs bytes.Buffer
	mux := uploadMuxWithLogger(env, slog.New(slog.NewTextHandler(&logs, nil)))

	unknown := "unknown-node-token-4d1c9f2a"
	content := []byte("unknown token bytes")
	sha := artifactSHA(content)
	headers := env.uploadHeaders("export/unknown.txt")
	headers[protocol.EmitAuthHeader] = protocol.EmitAuthScheme + " " + unknown

	rr := uploadPut(t, mux, env.uploadURL(sha), bytes.NewReader(content), int64(len(content)), headers)
	uploadAssertError(t, rr, http.StatusUnauthorized, artifactUploadErrUnauthorized)
	if strings.Contains(rr.Body.String(), unknown) {
		t.Fatalf("响应体泄漏了 token 明文: %s", rr.Body.String())
	}
	if strings.Contains(logs.String(), unknown) {
		t.Fatalf("日志泄漏了 token 明文: %s", logs.String())
	}
	env.assertNoArtifactWrites(t)
}

// T15：登记用的是**登记时刻**的授权时钟，不是请求开始那一刻。
//
// 这是 A1「收完 bytes 再同事务重做全部授权」的直接推论：租约在请求开始后、登记前
// 到期时，一次慢上传不得靠「开始时验过」把字节登记进去。注入一个递增时钟，把租约
// 到期时刻落在两次时钟之间——只有用新鲜时钟才会判为 403。
func TestArtifactUploadRegisterUsesFreshAuthorizationClock(t *testing.T) {
	env := newArtifactEnv(t)
	handler := newArtifactUploadHandler(env.db, env.spool, env.deps.logger())
	base := store.Now()
	var mu sync.Mutex
	calls := 0
	handler.now = func() int64 {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return base + int64(calls-1)*100 // 每次调用推进 100 秒（远超 30s ticket）
	}
	mux := http.NewServeMux()
	mux.Handle("PUT "+protocol.NodeTaskArtifactsPathPrefix+"{taskId}"+protocol.NodeTaskArtifactsPathInfix+"{sha256}", handler)

	content := []byte("stale clock bytes")
	sha := artifactSHA(content)
	// Begin 用 base 判：租约 base+1 仍有效；登记用 base+100 判：已过期。
	body := &uploadMutateOnRead{r: bytes.NewReader(content), mutate: func() { env.setLeaseUntil(t, base+1) }}
	rr := uploadPut(t, mux, env.uploadURL(sha), body, int64(len(content)), env.uploadHeaders("export/stale-clock.txt"))
	if body.read == 0 {
		t.Fatalf("handler 一次都没读 body，用例没走到「Begin 之后」的窗口")
	}
	uploadAssertError(t, rr, http.StatusForbidden, artifactUploadErrForbidden)
	env.assertNoArtifactWrites(t)
	if tmp := uploadTmpFiles(t, env); len(tmp) != 0 {
		t.Fatalf("spool tmp 残留 = %v, want 空", tmp)
	}
}

// T16（chunked 补充）：ContentLength < 0 时先收流、用实际大小建 ticket → 仍 201。
func TestArtifactUploadChunkedBodyUsesActualSize(t *testing.T) {
	env := newArtifactEnv(t)
	mux := uploadMux(env)

	content := []byte("chunked body without declared length")
	sha := artifactSHA(content)
	logicalPath := "export/chunked.txt"
	rr := uploadPut(t, mux, env.uploadURL(sha), bytes.NewReader(content), -1, env.uploadHeaders(logicalPath))
	if rr.Code != http.StatusCreated {
		t.Fatalf("HTTP 状态 = %d, want 201（body=%s）", rr.Code, rr.Body.String())
	}
	uploadAssertResponseEcho(t, uploadDecodeResponse(t, rr), env, logicalPath, sha, int64(len(content)))

	entries := uploadManifest(t, env)
	if len(entries) != 1 {
		t.Fatalf("manifest rows = %d, want 1", len(entries))
	}
	if entries[0].Size != int64(len(content)) {
		t.Fatalf("manifest size = %d, want 实际字节数 %d", entries[0].Size, len(content))
	}
	uploadAssertPublishedObject(t, env, entries[0].ObjectID.String(), content)
}
