package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件覆盖**任务日志上传**（logs.go，契约 §2）与它在 client 里的接线。
//
// 三件事必须钉死：
//  1. 端点推导：server → POST /v1/node/tasks/<escaped taskID>/logs（与 binaryEndpoint
//     同一套 scheme 规则；taskID 是对端给的字符串，必须转义）；
//  2. 请求本身：字段名逐字、只有 204 算成功、令牌只走 Authorization 头；
//  3. 纪律：上传失败**绝不影响任务成败**（日志丢了可惜，但不能因此把跑成功的任务
//     判成失败）。

// TestTaskLogsEndpoint 是端点推导的全部形态。
func TestTaskLogsEndpoint(t *testing.T) {
	cases := []struct {
		name    string
		server  string
		taskID  string
		want    string
		wantErr bool
	}{
		{
			name:   "http 原样",
			server: "http://example.com", taskID: "tsk_1",
			want: "http://example.com/v1/node/tasks/tsk_1/logs",
		},
		{
			name:   "ws → http",
			server: "ws://example.com", taskID: "tsk_1",
			want: "http://example.com/v1/node/tasks/tsk_1/logs",
		},
		{
			name:   "https 原样",
			server: "https://example.com", taskID: "tsk_1",
			want: "https://example.com/v1/node/tasks/tsk_1/logs",
		},
		{
			name:   "wss → https，末尾斜杠不重复",
			server: "wss://example.com/", taskID: "tsk_1",
			want: "https://example.com/v1/node/tasks/tsk_1/logs",
		},
		{
			name:   "server 自带的 path 前缀保留",
			server: "https://example.com/anotify/", taskID: "tsk_1",
			want: "https://example.com/anotify/v1/node/tasks/tsk_1/logs",
		},
		{
			name:   "taskID 逐段转义（斜杠不能变成路径分级）",
			server: "http://example.com", taskID: "tsk/1 2",
			want: "http://example.com/v1/node/tasks/tsk%2F1%202/logs",
		},
		{
			name:   "没 scheme → 报错（与 streamURL / binaryEndpoint 一致）",
			server: "example.com", taskID: "tsk_1",
			wantErr: true,
		},
		{
			name:   "不认识的 scheme → 报错",
			server: "ftp://example.com", taskID: "tsk_1",
			wantErr: true,
		},
		{
			name:   "没有 host → 报错",
			server: "http://", taskID: "tsk_1",
			wantErr: true,
		},
		{
			name:   "server 为空 → 报错",
			server: "", taskID: "tsk_1",
			wantErr: true,
		},
		{
			name:   "taskID 为空 → 报错（/tasks//logs 不是一条端点）",
			server: "http://example.com", taskID: "  ",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := taskLogsEndpoint(tc.server, tc.taskID)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("taskLogsEndpoint(%q, %q) = %q, want error", tc.server, tc.taskID, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("taskLogsEndpoint(%q, %q): %v", tc.server, tc.taskID, err)
			}
			if got != tc.want {
				t.Fatalf("taskLogsEndpoint(%q, %q) =\n got %q\nwant %q", tc.server, tc.taskID, got, tc.want)
			}
			// 令牌/凭据绝不能出现在 URL 里这件事在端点层就能断言：这里没有任何凭据。
			if strings.Contains(got, "?") {
				t.Fatalf("端点上不该有 query：%q", got)
			}
		})
	}
}

// TestNewHTTPLogSender 是「什么时候真的有上传这条路」的判据。
func TestNewHTTPLogSender(t *testing.T) {
	cases := []struct {
		name     string
		cfg      *nodeConfig
		wantNil  bool
		wantWarn bool
	}{
		// 没有配置就连「节点」都不是，不必告警。
		{name: "nil 配置", cfg: nil, wantNil: true},
		{name: "standalone：没有 server", cfg: &nodeConfig{Token: "nt_x"}, wantNil: true, wantWarn: true},
		{name: "没有 token", cfg: &nodeConfig{Server: "https://example.com"}, wantNil: true, wantWarn: true},
		{name: "server 不合法", cfg: &nodeConfig{Server: "example.com", Token: "nt_x"}, wantNil: true, wantWarn: true},
		{name: "合法配置", cfg: &nodeConfig{Server: "https://example.com", Token: "nt_x"}, wantNil: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			got := newHTTPLogSender(tc.cfg, log)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("newHTTPLogSender = %T, want nil（配置不可用时不上传）", got)
				}
				if tc.wantWarn && !strings.Contains(logs.String(), "agent.logs.disabled") {
					t.Fatalf("配置不可用时必须 WARN 一次（否则运维不知道日志没在传）：%q", logs.String())
				}
				return
			}
			if got == nil {
				t.Fatal("newHTTPLogSender = nil, want 可用上传器")
			}
			if logs.Len() != 0 {
				t.Fatalf("合法配置不该 WARN：%q", logs.String())
			}
		})
	}
}

// TestHTTPLogSenderUploadOK 是上传本身的形状：路径、方法、鉴权头、请求体字段。
func TestHTTPLogSenderUploadOK(t *testing.T) {
	const token = "nt_secret_token"

	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotCType  string
		gotKeys   []string
		gotBody   taskLogsPayload
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCType = r.Header.Get("Content-Type")
		if strings.Contains(r.URL.String(), token) {
			t.Errorf("令牌进了 URL（会落进反代日志）：%s", r.URL.String())
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读请求体: %v", err)
		}
		// 字段名必须**逐字**是契约里的四个（多一个/少一个服务端就可能 400）。
		var keys map[string]any
		if err := json.Unmarshal(raw, &keys); err != nil {
			t.Errorf("请求体不是 JSON: %v（%q）", err, raw)
		}
		for k := range keys {
			gotKeys = append(gotKeys, k)
		}
		sort.Strings(gotKeys)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Errorf("请求体解不出来: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	sender := &httpLogSender{client: srv.Client(), server: srv.URL, token: token}
	snap := logSnapshot{Stdout: "out\n", Stderr: "err\n", Truncated: true, Bytes: 1 << 20}
	if err := sender.upload(context.Background(), "tsk_1", snap); err != nil {
		t.Fatalf("upload: %v（204 必须算成功）", err)
	}

	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if want := "/v1/node/tasks/tsk_1/logs"; gotPath != want {
		t.Fatalf("path = %q, want %q", gotPath, want)
	}
	if gotAuth != "Bearer "+token {
		t.Fatalf("Authorization = %q, want %q（令牌只走头）", gotAuth, "Bearer "+token)
	}
	if gotCType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", gotCType)
	}
	wantKeys := []string{"bytes", "stderr", "stdout", "truncated"}
	if strings.Join(gotKeys, ",") != strings.Join(wantKeys, ",") {
		t.Fatalf("请求体字段 = %v, want %v（字段名逐字按契约 §2）", gotKeys, wantKeys)
	}
	if gotBody != (taskLogsPayload{Stdout: snap.Stdout, Stderr: snap.Stderr, Truncated: snap.Truncated, Bytes: snap.Bytes}) {
		t.Fatalf("请求体 = %+v, want %+v", gotBody, snap)
	}
}

// TestHTTPLogSenderUploadErrors：只有 204 算成功；4xx/5xx 带状态码与响应体前 200 字节。
func TestHTTPLogSenderUploadErrors(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantParts []string
	}{
		{name: "404 任务不存在", status: http.StatusNotFound, body: "task not found", wantParts: []string{"404", "task not found"}},
		{name: "413 太大", status: http.StatusRequestEntityTooLarge, body: "payload too large", wantParts: []string{"413", "payload too large"}},
		{name: "500 库挂了", status: http.StatusInternalServerError, body: "db is locked", wantParts: []string{"500", "db is locked"}},
		// 200 也不行：契约只认 204（200 说明服务端行为变了，得看得见）。
		{name: "200 不算成功", status: http.StatusOK, body: "ok", wantParts: []string{"200"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			sender := &httpLogSender{client: srv.Client(), server: srv.URL, token: "nt_x"}
			err := sender.upload(context.Background(), "tsk_1", logSnapshot{Stdout: "x"})
			if err == nil {
				t.Fatalf("status %d → nil, want error（只有 204 算成功）", tc.status)
			}
			for _, want := range tc.wantParts {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %q, want 含 %q", err.Error(), want)
				}
			}
		})
	}
}

// TestHTTPLogSenderUploadTruncatesErrorBody：错误响应很长时只带前 200 字节
// （别把一整页 HTML 塞进 slog）。
func TestHTTPLogSenderUploadTruncatesErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, strings.Repeat("x", 5000))
	}))
	defer srv.Close()

	sender := &httpLogSender{client: srv.Client(), server: srv.URL, token: "nt_x"}
	err := sender.upload(context.Background(), "tsk_1", logSnapshot{})
	if err == nil {
		t.Fatal("500 → nil, want error")
	}
	if n := strings.Count(err.Error(), "x"); n > logErrorBodyBytes {
		t.Fatalf("error 里带了 %d 个 x，want <= %d（响应体只截前 200 字节）", n, logErrorBodyBytes)
	}
}

// TestHTTPLogSenderUploadCanceled：ctx 到点必须立刻返回（日志上传不能拖住 result 帧）。
func TestHTTPLogSenderUploadCanceled(t *testing.T) {
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked // 永不主动响应：只有 ctx 能救
	}))
	defer srv.Close()
	defer close(blocked)

	sender := &httpLogSender{client: srv.Client(), server: srv.URL, token: "nt_x"}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := sender.upload(ctx, "tsk_1", logSnapshot{}); err == nil {
		t.Fatal("ctx 超时 → nil, want error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("upload 用了 %s，ctx 超时没生效", elapsed)
	}
}

// TestTaskLogsUploadFailureKeepsResultSucceeded 是本期最重要的一条纪律：
// 日志端点 500 时任务仍然是 succeeded（只是日志丢了）。
//
// 用真的 HTTP 服务 + 真的 httpLogSender（由 cfg.Server 推导）：这里要证明的是
// 「接线本身不会把失败传染给任务成败」，而不是「某个 fake 返回了 error」。
func TestTaskLogsUploadFailureKeepsResultSucceeded(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "logs endpoint is broken")
	}))
	defer srv.Close()

	cfg := w4Config()
	// 日志端点由 cfg.Server 推导（WebSocket 那条腿在这里用不到，serveNode 已经建好连接）。
	cfg.Server = srv.URL
	h := w4Start(t, cfg, 60, 100*time.Millisecond)

	art := w4Artifact("demo.logs", 1, "echo hi")
	h.deploy(art)
	h.fc.feed(t, &assignFrame{
		Type: frameTypeAssign, TaskID: "tsk_logs_500",
		Capability: "demo.logs", Version: 1, MaxDuration: 30,
	})

	frames := h.fc.waitFor(t, 10*time.Second, "任务 result", w4Cond(frameTypeResult, "tsk_logs_500"))
	res, _, ok := w4Find(frames, frameTypeResult, "tsk_logs_500")
	if !ok {
		t.Fatalf("没有 result 帧：%s", w4Describe(frames))
	}
	if res["status"] != statusSucceeded {
		t.Fatalf("status = %v, want %s（日志上传失败绝不能把任务判成失败）", res["status"], statusSucceeded)
	}
	// 上传确实被尝试过（否则这条用例什么都没证明）。上传在 result 之前完成，
	// 所以看到 result 时计数一定已经 >= 1。
	if hits.Load() == 0 {
		t.Fatal("日志端点一次都没被访问：接线没生效")
	}
	h.stop()
}

// TestTaskLogsUploadCarriesRealCapture 是「真任务 → 上传」的**数据链路**证据。
//
// 其余上传用例用的是手工构造的 logSnapshot：它们只证明「sender 会原样编 JSON」。
// 接线处（client.uploadLogs）如果把 out.Stdout / out.Stderr 搞反、或误传了 summary，
// 那些用例仍然全绿——而这条链路恰恰是本期最值钱的东西（「任务为什么炸」）。
//
// 这里在**真 WebSocket 连接 + 真子进程**上跑一遍，并在假中枢那一侧把请求体解出来，
// 逐字比对两个流；同时钉住时序：result 帧到达时上传必须已经完成（契约 §2）。
func TestTaskLogsUploadCarriesRealCapture(t *testing.T) {
	hub := w4StartHub(t, 1, &assignFrame{
		Type:        frameTypeAssign,
		TaskID:      "tsk_body",
		Capability:  "demo.body",
		Version:     1,
		LeaseUntil:  time.Now().Add(time.Minute).Unix(),
		MaxDuration: 10,
	})
	cfg := w4RealWSConfig(t, hub.srv.URL)
	dataDir := w4PreloadedDataDir(t, w4Artifact("demo.body", 1,
		"printf 'out-line\\n'; printf 'err-line\\n' >&2"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := w4ServeRealWS(t, ctx, cfg, dataDir, 2*time.Second)

	frames := hub.waitFrame(t, 10*time.Second, "tsk_body 的 result", w4Cond(frameTypeResult, "tsk_body"))
	res, _, ok := w4Find(frames, frameTypeResult, "tsk_body")
	if !ok {
		t.Fatalf("真连接上没收到 result: %s", w4Describe(frames))
	}
	if res["status"] != statusSucceeded {
		t.Fatalf("result.status = %v, want %s", res["status"], statusSucceeded)
	}
	// summary 只认 stdout 的首行（stderr 里那句不参与）。
	if res["summary"] != "out-line" {
		t.Fatalf("result.summary = %#v, want %q（summary 是 stdout 第一行）", res["summary"], "out-line")
	}

	paths, bodies, atResult := hub.logsState()
	if len(paths) != 1 || paths[0] != "/v1/node/tasks/tsk_body/logs" {
		t.Fatalf("上传请求 = %v, want [/v1/node/tasks/tsk_body/logs]", paths)
	}
	// 时序：收到 result 的那一刻，这条任务的上传已经落过（不是「事后才传」）。
	if atResult != 1 {
		t.Fatalf("result 到达时已完成的上传笔数 = %d，want 1（契约 §2：先传日志，再发 result）", atResult)
	}

	got := bodies[0]
	if got.Stdout != "out-line\n" {
		t.Fatalf("上传的 stdout = %q, want %q（两个流不得串味）", got.Stdout, "out-line\n")
	}
	if got.Stderr != "err-line\n" {
		t.Fatalf("上传的 stderr = %q, want %q（报错通常在这里，最不能丢）", got.Stderr, "err-line\n")
	}
	if got.Truncated {
		t.Errorf("truncated = true, want false（这么短的输出没截断）")
	}
	if want := int64(len("out-line\n") + len("err-line\n")); got.Bytes != want {
		t.Fatalf("bytes = %d, want %d（两流真实字节数之和）", got.Bytes, want)
	}

	cancel()
	w4WaitServe(t, done, 15*time.Second)
}

// TestEncodeTaskLogsBodyBounded 钉住「编成后的请求体一定不超过服务端 256 KiB 上限」。
//
// 为什么需要它：JSON 必须转义控制字节（1 → 6 字节），而 HTML 转义会多转 '<' / '>' / '&'。
// 不设防的话，一份**完全合规**的 64 KiB 采集（每流 32 KiB）能被撑到 390 KiB → 服务端
// 413 → 日志丢弃（只 WARN）。那正是本期要消灭的「日志静默消失」，只是换了个位置发生。
func TestEncodeTaskLogsBodyBounded(t *testing.T) {
	decode := func(t *testing.T, body []byte) taskLogsPayload {
		t.Helper()
		var p taskLogsPayload
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatalf("编出来的 body 解不开: %v", err)
		}
		return p
	}

	t.Run("HTML 密集（'<' × 32 KiB × 2）不靠折叠也要在上限内", func(t *testing.T) {
		html := strings.Repeat("<", maxLogStreamBytes)
		body, err := encodeTaskLogsBody(logSnapshot{
			Stdout: html, Stderr: html, Truncated: true, Bytes: int64(2 * maxLogStreamBytes),
		})
		if err != nil {
			t.Fatalf("encodeTaskLogsBody: %v", err)
		}
		if len(body) > maxTaskLogsUploadBody {
			t.Fatalf("body = %d 字节，超过服务端上限 %d（会被 413 丢掉）", len(body), maxTaskLogsUploadBody)
		}
		// 关掉 HTML 转义之后这一份应当是 ~64 KiB 而不是 ~390 KiB。
		if len(body) > 80<<10 {
			t.Fatalf("body = %d 字节，说明 HTML 转义没关（'<' 被编成 \\u003c 了）", len(body))
		}
		p := decode(t, body)
		if p.Stdout != html || p.Stderr != html {
			t.Fatal("编出来的内容与原文不一致（关 HTML 转义只该影响线上字节，不该影响解出来的字符串）")
		}
		if !p.Truncated || p.Bytes != int64(2*maxLogStreamBytes) {
			t.Fatalf("元数据被改动了：truncated=%v bytes=%d", p.Truncated, p.Bytes)
		}
	})

	t.Run("控制字节密集（\\x01 × 32 KiB × 2）走兜底折叠", func(t *testing.T) {
		ctrl := strings.Repeat("\x01", maxLogStreamBytes)
		// 先确认「不兜底」确实会超限：否则这条用例证明不了兜底的必要性。
		raw, err := marshalTaskLogs(taskLogsPayload{Stdout: ctrl, Stderr: ctrl})
		if err != nil {
			t.Fatalf("marshalTaskLogs: %v", err)
		}
		if len(raw) <= maxTaskLogsUploadBody {
			t.Fatalf("夹具失效：控制字节密集的原文 body 只有 %d 字节（没有超过上限 %d）",
				len(raw), maxTaskLogsUploadBody)
		}

		body, err := encodeTaskLogsBody(logSnapshot{
			Stdout: ctrl, Stderr: ctrl, Truncated: true, Bytes: int64(2 * maxLogStreamBytes),
		})
		if err != nil {
			t.Fatalf("encodeTaskLogsBody: %v", err)
		}
		if len(body) > maxTaskLogsUploadBody {
			t.Fatalf("兜底后 body 仍然 = %d 字节，超过上限 %d", len(body), maxTaskLogsUploadBody)
		}
		p := decode(t, body)
		want := strings.Repeat("?", maxLogStreamBytes)
		if p.Stdout != want || p.Stderr != want {
			t.Fatalf("兜底应当把 C0 控制字节折成 '?'（保住「传得上去」），实际 len=%d/%d",
				len(p.Stdout), len(p.Stderr))
		}
		if !p.Truncated || p.Bytes != int64(2*maxLogStreamBytes) {
			t.Fatalf("兜底不该改动元数据：truncated=%v bytes=%d", p.Truncated, p.Bytes)
		}
	})

	t.Run("普通日志不走兜底：引号 / 反斜杠 / 换行 / 制表符 / ANSI 原样", func(t *testing.T) {
		normal := "a\"b\\c\n\x1b[31mred\x1b[0m\tend\r\n"
		body, err := encodeTaskLogsBody(logSnapshot{Stdout: normal, Stderr: "", Bytes: int64(len(normal))})
		if err != nil {
			t.Fatalf("encodeTaskLogsBody: %v", err)
		}
		if got := decode(t, body).Stdout; got != normal {
			t.Fatalf("普通日志被改动了：\n got %q\nwant %q（ANSI 颜色码必须原样到达中枢）", got, normal)
		}
	})

	t.Run("foldControlBytes 只碰 C0，不碰多字节 UTF-8", func(t *testing.T) {
		in := "中文\x00ok\x1f✅\n\tend"
		want := "中文?ok?✅\n\tend"
		if got := foldControlBytes(in); got != want {
			t.Fatalf("foldControlBytes = %q, want %q", got, want)
		}
		// 没有需要折的字节时原样返回（快路径）。
		if got := foldControlBytes("没有控制字节\n"); got != "没有控制字节\n" {
			t.Fatalf("快路径改动了内容：%q", got)
		}
	})
}
