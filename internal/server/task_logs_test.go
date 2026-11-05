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

	"github.com/zzjcool/anotify/internal/store"
)

// 本文件覆盖任务日志的两个 HTTP 端点（CONTRACT-task-logs §2 / §4）：
//
//	POST /v1/node/tasks/{taskId}/logs        节点上传（node token）
//	GET  /v1/tasks/{taskId}/logs?namespaceId=…  控制台读（会话 + 命名空间隔离）
//
// 四类判据，每类都钉在「回归会静默」的那一点上：
//  1. **两套鉴权不串**：会话 Cookie 不能当 node token 用（上传端点必须拒绝它），
//     否则浏览器会话就成了「给任意节点写日志」的凭据；
//  2. **原文逐字落库、逐字返回**：中文 / 空行 / 截断标记一个字节都不许变——
//     日志的价值全在字节级还原，任何 trim / 转义都会让「为什么炸了」失真；
//  3. **归属**：跨命名空间、别台节点的任务都必须是 404（与「不存在」同形，不留探测面）；
//  4. **没日志 = 200 空对象**，不是 404：这是契约 §4 里最容易写成 404 的一条。

// ---------- 夹具 ----------

// logsFixture 是任务日志端点的测试夹具：一套真库 + 一台节点 + 它的明文令牌。
type logsFixture struct {
	env *consoleEnv
	// node 是上传方（node token 属于它）。
	node *store.Node
	// token 是 node 的明文令牌（库里只存 sha256）。
	token string
	// task 是 node 跑的那条任务（已有 node_id）。
	task *store.Task
}

// newLogsFixture 建库/会话/命名空间（newConsoleEnv）+ 一台节点 + 一条归属它的任务。
func newLogsFixture(t *testing.T) *logsFixture {
	t.Helper()
	env := newConsoleEnv(t)
	ctx := context.Background()

	node := &store.Node{NamespaceID: env.nsID, Name: "node-a"}
	token, err := env.db.CreateNode(ctx, node)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	return &logsFixture{env: env, node: node, token: token, task: env.seedTask(t, env.nsID, "trace_logs", "runner", node.ID)}
}

// seedTask 在指定命名空间建一条任务；nodeID 非空时把任务的 node_id 钉成它
// （任务日志的归属校验就是拿这一列比的）。
func (e *consoleEnv) seedTask(t *testing.T, nsID, traceID, capName, nodeID string) *store.Task {
	t.Helper()
	capDef := e.seedCapabilityIn(t, nsID, capName, 1, []string{"fix"})
	task := &store.Task{
		NamespaceID: nsID, TraceID: traceID, CapabilityID: capDef.ID,
		Capability: capDef.Name, Version: 1, Params: json.RawMessage(`{}`),
		CapHash: capDef.ContentHash, GraphID: "grf_x",
	}
	if nodeID != "" {
		id := nodeID
		task.NodeID = &id
	}
	if err := e.db.CreateTasks(context.Background(), []*store.Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task
}

// uploadMux 是上传端点的**真路由**（与生产 mux 同一条 pattern）。
//
// 为什么单独的 mux 而不是整个生产 mux：这条路由的鉴权是 node token，与会话中间件
// 无关；把整张路由表拉进来只会多出无关变量。PathValue("taskId") 需要 mux 的
// 模式匹配才会有值，所以这里仍然经过 mux。
func (f *logsFixture) uploadMux() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/node/tasks/{taskId}/logs",
		noStore(&taskLogUploadHandler{db: f.env.db, auth: f.env.db}))
	return mux
}

// upload 发一次上传请求（authorization="" 表示不带令牌），body 原样发出。
func (f *logsFixture) upload(t *testing.T, authorization, taskID string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/node/tasks/"+taskID+"/logs", bytes.NewReader(body))
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rr := httptest.NewRecorder()
	f.uploadMux().ServeHTTP(rr, req)
	return rr
}

// uploadJSON 编一个上传体并发出（带正确 node token）。
func (f *logsFixture) uploadJSON(t *testing.T, taskID, stdout, stderr string, truncated bool, bytes int64) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(taskLogsBody{Stdout: stdout, Stderr: stderr, Truncated: truncated, Bytes: bytes})
	if err != nil {
		t.Fatalf("marshal upload body: %v", err)
	}
	return f.upload(t, "Bearer "+f.token, taskID, body)
}

// readLogs 发一次读取请求（带会话 Cookie），返回状态码 + 原始 body。
func (f *logsFixture) readLogs(t *testing.T, path string) (int, string) {
	t.Helper()
	rr := f.env.do(t, http.MethodGet, path, nil)
	return rr.Code, rr.Body.String()
}

// mustStored 读库里的日志行（不经 HTTP），用于判「落库内容逐字相等」。
func (f *logsFixture) mustStored(t *testing.T, nsID, taskID string) *store.TaskLogs {
	t.Helper()
	logs, err := f.env.db.GetTaskLogs(context.Background(), nsID, taskID)
	if err != nil {
		t.Fatalf("get task logs %s: %v", taskID, err)
	}
	return logs
}

// 一段"真实形状"的日志：中文 + 空行 + 截断标记 + 尾部换行。
// stdout/stderr 都是原样存取的，任何 trim/转义都会让下面几条断言红。
const (
	logsStdout = "第一步\n\n第二步：跑测试 ✅\n\n…（日志已截断：共 1048576 字节，只保留最后 32768 字节）\n"
	logsStderr = "flag provided but not defined: -to\n用法：anotify-agent emit --to <能力>\n\n"
)

// ---------- 上传：鉴权 ----------

// TestTaskLogUploadAuthNodeTokenOnly 钉住「这个端点认 node token，不认会话」。
//
// 两种凭据在同一张表里（nodes.token_hash vs sessions），串了就是安全问题：
// 会话 Cookie 能写任意节点日志的话，浏览器就成了伪造排障材料的入口。
func TestTaskLogUploadAuthNodeTokenOnly(t *testing.T) {
	f := newLogsFixture(t)
	body, _ := json.Marshal(taskLogsBody{Stdout: "x"})

	t.Run("只有会话 Cookie、没有 Bearer → 401", func(t *testing.T) {
		// 会话中间件不在这条路由上，但请求仍然带着 Cookie：handler 必须只看 Bearer。
		req := httptest.NewRequest(http.MethodPost, "/v1/node/tasks/"+f.task.ID+"/logs", bytes.NewReader(body))
		req.AddCookie(f.env.cookie)
		rr := httptest.NewRecorder()
		f.uploadMux().ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401（body=%s）", rr.Code, rr.Body.String())
		}
		if got := decode(t, rr)["error"]; got != "missing_node_token" {
			t.Fatalf("error = %v, want missing_node_token", got)
		}
	})

	t.Run("会话 id 当 Bearer → 403", func(t *testing.T) {
		// 这正是「会话令牌不能当 node token」的护栏。
		rr := f.upload(t, "Bearer "+f.env.cookie.Value, f.task.ID, body)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403（会话令牌不能当 node token）", rr.Code)
		}
		if got := decode(t, rr)["error"]; got != "invalid_node_token" {
			t.Fatalf("error = %v, want invalid_node_token", got)
		}
	})

	t.Run("正确 node token → 204", func(t *testing.T) {
		rr := f.upload(t, "Bearer "+f.token, f.task.ID, body)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204（body=%s）", rr.Code, rr.Body.String())
		}
	})
}

// ---------- 上传：成功 + 原文落库 ----------

// TestTaskLogUploadStoresVerbatim 204 且**没有响应体**，库里内容逐字相等。
func TestTaskLogUploadStoresVerbatim(t *testing.T) {
	f := newLogsFixture(t)
	rr := f.uploadJSON(t, f.task.ID, logsStdout, logsStderr, true, 1048576)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("204 不能带 body，got %q", rr.Body.String())
	}

	got := f.mustStored(t, f.env.nsID, f.task.ID)
	if got.Stdout != logsStdout {
		t.Errorf("stdout 落了不同内容（不能 trim / 转义 / 丢空行）:\n got=%q\nwant=%q", got.Stdout, logsStdout)
	}
	if got.Stderr != logsStderr {
		t.Errorf("stderr 落了不同内容:\n got=%q\nwant=%q", got.Stderr, logsStderr)
	}
	if !got.Truncated {
		t.Error("truncated 丢了（必须原样透传采集侧的事实）")
	}
	if got.Bytes != 1048576 {
		t.Errorf("bytes = %d, want 1048576", got.Bytes)
	}
	if got.NamespaceID != f.env.nsID {
		t.Errorf("namespaceId = %q, want %q（命名空间来自令牌）", got.NamespaceID, f.env.nsID)
	}
	if got.CreatedAt == 0 {
		t.Error("createdAt 没写（幂等覆盖时也要重写）")
	}
}

// TestTaskLogUploadIdempotentOverwrite 连传两次 → 只有一行，且是第二次的内容。
//
// 幂等是硬要求（契约 §2）：重派、agent 重启补传、客户端重试都会重复上传同一条任务。
// 如果第二次变成插第二行，主键会报错或读端点随机返回旧日志——两种都难查。
func TestTaskLogUploadIdempotentOverwrite(t *testing.T) {
	f := newLogsFixture(t)
	if rr := f.uploadJSON(t, f.task.ID, "first", "first-err", false, 5); rr.Code != http.StatusNoContent {
		t.Fatalf("first upload status = %d, want 204", rr.Code)
	}
	if rr := f.uploadJSON(t, f.task.ID, logsStdout, logsStderr, true, 42); rr.Code != http.StatusNoContent {
		t.Fatalf("second upload status = %d, want 204", rr.Code)
	}

	var rows int
	if err := f.env.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM task_logs WHERE task_id = ?`, f.task.ID).Scan(&rows); err != nil {
		t.Fatalf("count task_logs: %v", err)
	}
	if rows != 1 {
		t.Fatalf("task_logs 行数 = %d, want 1（重复上传是覆盖，不是插入）", rows)
	}
	got := f.mustStored(t, f.env.nsID, f.task.ID)
	if got.Stdout != logsStdout || got.Stderr != logsStderr || !got.Truncated || got.Bytes != 42 {
		t.Fatalf("留下的是第一次的内容，不是第二次：%+v", got)
	}
}

// ---------- 上传：404 三种形状 ----------

// TestTaskLogUploadNotFound 任务不存在 / 任务在别的命名空间 / 任务属于别台节点 → 全 404。
func TestTaskLogUploadNotFound(t *testing.T) {
	f := newLogsFixture(t)
	ctx := context.Background()

	otherNS := f.env.seedNamespace(t, "other", "另一个命名空间")
	foreignNS := f.env.seedTask(t, otherNS.ID, "trace_other_ns", "runner", "")
	otherNode := &store.Node{NamespaceID: f.env.nsID, Name: "node-b"}
	if _, err := f.env.db.CreateNode(ctx, otherNode); err != nil {
		t.Fatalf("create second node: %v", err)
	}
	foreignNode := f.env.seedTask(t, f.env.nsID, "trace_other_node", "runner2", otherNode.ID)

	cases := []struct {
		name   string
		taskID string
	}{
		{"任务不存在", "tsk_doesnotexist"},
		{"任务在另一个命名空间", foreignNS.ID},
		{"任务属于另一台节点", foreignNode.ID},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := f.uploadJSON(t, c.taskID, "x", "y", false, 1)
			if rr.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404（body=%s）", rr.Code, rr.Body.String())
			}
			if got := decode(t, rr)["error"]; got != "task_not_found" {
				t.Fatalf("error = %v, want task_not_found", got)
			}
			// 反向证据：这些任务（除了第一条）在库里**确实存在**，
			// 所以 404 来自归属校验，不是「找不到行」凑巧。
			if c.taskID == foreignNS.ID || c.taskID == foreignNode.ID {
				if _, err := f.env.db.GetTaskLogs(ctx, f.env.nsID, c.taskID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("不该落库，GetTaskLogs err = %v", err)
				}
			}
		})
	}
}

// ---------- 上传：413 / 边界 ----------

// TestTaskLogUploadBodyLimit 超过 256 KiB → 413；**恰好** 256 KiB → 204。
//
// 边界不许写反：MaxBytesReader 允许读满 limit 字节，第 limit+1 个字节才报错。
// 「正好卡在上限」的请求必须成功，否则 32 KiB×2 + 转义余量的正常日志会被误拒。
func TestTaskLogUploadBodyLimit(t *testing.T) {
	f := newLogsFixture(t)

	t.Run("超过 256 KiB → 413", func(t *testing.T) {
		// 300 KiB 的 stdout：解析器在看到收尾的 `}` 之前就必须读超过上限。
		huge := strings.Repeat("x", 300<<10)
		rr := f.uploadJSON(t, f.task.ID, huge, "", false, int64(len(huge)))
		if rr.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413（body=%s）", rr.Code, rr.Body.String())
		}
		if got := decode(t, rr)["error"]; got != "payload_too_large" {
			t.Fatalf("error = %v, want payload_too_large", got)
		}
		if _, err := f.env.db.GetTaskLogs(context.Background(), f.env.nsID, f.task.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("413 的请求不该落库，GetTaskLogs err = %v", err)
		}
	})

	t.Run("合法 JSON + 超大尾巴 → 413（不能只拦第一个 JSON 值）", func(t *testing.T) {
		// json.Decoder 停在第一个完整的值之后，尾巴它不读；不把剩余字节读掉的话，
		// 「小请求体 + 1 MB 垃圾」就会绕过上限——契约要求的是**请求体** ≤ 256 KiB。
		tail := append([]byte(`{"stdout":"x"}`), bytes.Repeat([]byte("y"), 300<<10)...)
		if len(tail) <= maxTaskLogsBody {
			t.Fatalf("夹具算错了：请求体 = %d 字节，want > %d", len(tail), maxTaskLogsBody)
		}
		rr := f.upload(t, "Bearer "+f.token, f.task.ID, tail)
		if rr.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413（body=%s）", rr.Code, rr.Body.String())
		}
		if got := decode(t, rr)["error"]; got != "payload_too_large" {
			t.Fatalf("error = %v, want payload_too_large", got)
		}
		if _, err := f.env.db.GetTaskLogs(context.Background(), f.env.nsID, f.task.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("413 的请求不该落库，GetTaskLogs err = %v", err)
		}
	})

	t.Run("恰好 256 KiB → 204", func(t *testing.T) {
		// 先量空 stdout 时的固定开销，再把它补到正好 maxTaskLogsBody 字节。
		// 内容全是 ASCII 'x'：JSON 编码不会变长，长度可精确预期。
		base, err := json.Marshal(taskLogsBody{Stdout: "", Stderr: "", Truncated: true, Bytes: 12345})
		if err != nil {
			t.Fatalf("marshal base body: %v", err)
		}
		stdout := strings.Repeat("x", maxTaskLogsBody-len(base))
		body, err := json.Marshal(taskLogsBody{Stdout: stdout, Stderr: "", Truncated: true, Bytes: 12345})
		if err != nil {
			t.Fatalf("marshal boundary body: %v", err)
		}
		if len(body) != maxTaskLogsBody {
			t.Fatalf("夹具算错了：body = %d 字节，want %d", len(body), maxTaskLogsBody)
		}
		rr := f.upload(t, "Bearer "+f.token, f.task.ID, body)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204（正好等于上限不该被拒）; body=%s", rr.Code, rr.Body.String())
		}
		if got := f.mustStored(t, f.env.nsID, f.task.ID); got.Stdout != stdout {
			t.Fatalf("边界请求落库内容不一致：len(stdout) = %d, want %d", len(got.Stdout), len(stdout))
		}
	})

	t.Run("坏 JSON → 400", func(t *testing.T) {
		rr := f.upload(t, "Bearer "+f.token, f.task.ID, []byte(`{"stdout":`))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400（body=%s）", rr.Code, rr.Body.String())
		}
		if got := decode(t, rr)["error"]; got != "invalid_json" {
			t.Fatalf("error = %v, want invalid_json", got)
		}
	})
}

// TestTaskLogUploadMethodNotAllowed GET 到上传端点 → 405。
func TestTaskLogUploadMethodNotAllowed(t *testing.T) {
	f := newLogsFixture(t)

	// ① 经过 mux：Go 的 ServeMux 在方法不匹配时自己回 405（生产路径上的真实行为）。
	req := httptest.NewRequest(http.MethodGet, "/v1/node/tasks/"+f.task.ID+"/logs", nil)
	rr := httptest.NewRecorder()
	f.uploadMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("mux status = %d, want 405", rr.Code)
	}

	// ② 直接打 handler：这条分支才是 handler 自己的判据（不从 mux 走时也要拦）。
	hreq := httptest.NewRequest(http.MethodGet, "/v1/node/tasks/"+f.task.ID+"/logs", nil)
	hrr := httptest.NewRecorder()
	(&taskLogUploadHandler{db: f.env.db, auth: f.env.db}).ServeHTTP(hrr, hreq)
	if hrr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("handler status = %d, want 405", hrr.Code)
	}
	if got := hrr.Header().Get("Allow"); got != http.MethodPost {
		t.Fatalf("Allow = %q, want POST", got)
	}
}

// ---------- 读取 ----------

// TestTaskLogsReadEmpty 任务存在但没有日志 → **200 + 空对象**（契约 §4）。
func TestTaskLogsReadEmpty(t *testing.T) {
	f := newLogsFixture(t)
	code, body := f.readLogs(t, f.env.withNS("/v1/tasks/"+f.task.ID+"/logs"))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200（没日志是合法状态，绝不是 404）; body=%s", code, body)
	}
	var view map[string]any
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if len(view) != 4 {
		t.Fatalf("响应字段数 = %d, want 4（只能有 stdout/stderr/truncated/bytes）: %s", len(view), body)
	}
	if view["stdout"] != "" || view["stderr"] != "" {
		t.Fatalf("stdout/stderr 必须是空串: %s", body)
	}
	if view["truncated"] != false {
		t.Fatalf("truncated = %v, want false", view["truncated"])
	}
	if got, ok := view["bytes"].(float64); !ok || got != 0 {
		t.Fatalf("bytes = %v, want 0", view["bytes"])
	}
}

// TestTaskLogsReadVerbatim 上传什么就读什么：截断标记、空行、中文、bytes 全原样。
func TestTaskLogsReadVerbatim(t *testing.T) {
	f := newLogsFixture(t)
	if rr := f.uploadJSON(t, f.task.ID, logsStdout, logsStderr, true, 1048576); rr.Code != http.StatusNoContent {
		t.Fatalf("upload status = %d, want 204", rr.Code)
	}

	code, body := f.readLogs(t, f.env.withNS("/v1/tasks/"+f.task.ID+"/logs"))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	var view map[string]any
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if len(view) != 4 {
		t.Fatalf("响应字段数 = %d, want 4（不许多塞 taskId/namespaceId/createdAt）: %s", len(view), body)
	}
	if view["stdout"] != logsStdout {
		t.Errorf("stdout 不逐字相等:\n got=%q\nwant=%q", view["stdout"], logsStdout)
	}
	if view["stderr"] != logsStderr {
		t.Errorf("stderr 不逐字相等:\n got=%q\nwant=%q", view["stderr"], logsStderr)
	}
	if view["truncated"] != true {
		t.Errorf("truncated = %v, want true", view["truncated"])
	}
	if got, _ := view["bytes"].(float64); got != 1048576 {
		t.Errorf("bytes = %v, want 1048576", view["bytes"])
	}
}

// TestTaskLogsReadIsolationAnd404 跨命名空间 / 不存在 → 404；缺参 / `*` → 400。
func TestTaskLogsReadIsolationAnd404(t *testing.T) {
	f := newLogsFixture(t)
	otherNS := f.env.seedNamespace(t, "other", "另一个命名空间")
	otherTask := f.env.seedTask(t, otherNS.ID, "trace_read_other", "runner", "")

	t.Run("跨命名空间 → 404（与不存在同形）", func(t *testing.T) {
		code, body := f.readLogs(t, f.env.withNS("/v1/tasks/"+otherTask.ID+"/logs"))
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body=%s", code, body)
		}
		if !strings.Contains(body, "task not found") {
			t.Fatalf("body = %q, want task not found（与 /v1/tasks/{id} 同文案）", body)
		}
	})

	t.Run("任务不存在 → 404", func(t *testing.T) {
		code, _ := f.readLogs(t, f.env.withNS("/v1/tasks/tsk_nope/logs"))
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", code)
		}
	})

	t.Run("缺 namespaceId → 400", func(t *testing.T) {
		code, body := f.readLogs(t, "/v1/tasks/"+f.task.ID+"/logs")
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400（绝不回退默认命名空间）; body=%s", code, body)
		}
	})

	t.Run("namespaceId=* → 400", func(t *testing.T) {
		code, body := f.readLogs(t, "/v1/tasks/"+f.task.ID+"/logs?namespaceId=*")
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400（单对象端点不接受 *）; body=%s", code, body)
		}
	})

	t.Run("没有会话 → 401", func(t *testing.T) {
		rr := f.env.doNoCookie(t, http.MethodGet, f.env.withNS("/v1/tasks/"+f.task.ID+"/logs"))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rr.Code)
		}
	})
}

// TestTaskLogsReadMethodNotAllowed POST 到读端点 → 405。
func TestTaskLogsReadMethodNotAllowed(t *testing.T) {
	f := newLogsFixture(t)
	rr := f.env.do(t, http.MethodPost, f.env.withNS("/v1/tasks/"+f.task.ID+"/logs"), nil)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body=%s", rr.Code, rr.Body.String())
	}
}

// TestTaskLogsRoundTripThroughBothEndpoints 端到端一遍：上传 → 读回 → 再上传覆盖 → 再读。
//
// 这条用例同时当「生产 mux 接线」的护栏：读端点走的是 newConsoleEnv 里注册的真路由，
// 写端点走上传 mux，两边都不经过额外的适配层。
func TestTaskLogsRoundTripThroughBothEndpoints(t *testing.T) {
	f := newLogsFixture(t)
	path := f.env.withNS("/v1/tasks/" + f.task.ID + "/logs")

	if rr := f.uploadJSON(t, f.task.ID, "第一版\n", "", false, 4); rr.Code != http.StatusNoContent {
		t.Fatalf("upload #1 status = %d, want 204", rr.Code)
	}
	code, body := f.readLogs(t, path)
	if code != http.StatusOK || !strings.Contains(body, "第一版") {
		t.Fatalf("read #1 = %d %s, want 200 含「第一版」", code, body)
	}

	if rr := f.uploadJSON(t, f.task.ID, "第二版\n", "错了\n", true, 999); rr.Code != http.StatusNoContent {
		t.Fatalf("upload #2 status = %d, want 204", rr.Code)
	}
	code, body = f.readLogs(t, path)
	if code != http.StatusOK {
		t.Fatalf("read #2 status = %d, body=%s", code, body)
	}
	var view map[string]any
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if view["stdout"] != "第二版\n" || view["stderr"] != "错了\n" {
		t.Fatalf("覆盖没生效: %s", body)
	}
	if view["bytes"] != float64(999) || view["truncated"] != true {
		t.Fatalf("bytes/truncated 没跟着覆盖: %s", body)
	}
}
