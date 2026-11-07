package main

// 本文件覆盖 `emit --task-file`：hc 托管的 TUI agent 拿不到节点注入的环境变量
// （env 进不去 hc），所以同一身份改从一份 0600 JSON 读。
//
// 这里钉的不是「能读文件」，而是两条纪律 + 退出码表：
//
//   - 两套入口**互斥**（同时给 = exit 2，不做隐式优先级）；
//   - 文件必须是 **0600**（里面是一次性 token，组/其他人可读就拒）；
//   - 坏 JSON / 缺字段 / 坏 ports 全部是 exit 2（修参数，别重试），
//     sock 为空才是 exit 6（本地模式没有中枢，与 env 入口同一语义）。

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/ws"
)

// noEnv 是「进程环境里什么都没注入」的 getenv（task-file 模式的正常前提）。
func noEnv(string) string { return "" }

// writeTaskFile 写一份 0600 的 task.json（返回路径）。
func writeTaskFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "task.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write task file: %v", err)
	}
	return path
}

// taskFileJSON 组装一份合法的 task.json。
func taskFileJSON(t *testing.T, sock, token, ports string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"sock":    sock,
		"token":   token,
		"ports":   json.RawMessage(ports),
		"taskId":  "tsk_tf",
		"traceId": "trc_tf",
	})
	if err != nil {
		// RawMessage 坏了（ports 不是 JSON）→ 调用方给的就是坏样本
		raw, err = json.Marshal(map[string]any{
			"sock": sock, "token": token, "ports": ports,
			"taskId": "tsk_tf", "traceId": "trc_tf",
		})
		if err != nil {
			t.Fatalf("marshal task file: %v", err)
		}
	}
	return string(raw)
}

// TestRunEmitTaskFileHappyPath：sock/token/ports 全从文件来，帧照常发出去。
func TestRunEmitTaskFileHappyPath(t *testing.T) {
	opt := defaultEmitOptions(t, t.TempDir())
	opt.Expects = map[string]ws.PortExpect{
		"fix": {Schema: json.RawMessage(`{"type":"object","required":["issue"]}`)},
	}
	srv, rec := startEmitServer(t, opt)

	path := writeTaskFile(t, taskFileJSON(t, srv.Path(), emitTestToken, emitTestContract))
	var stdout, stderr bytes.Buffer
	code, err := runEmit(
		[]string{"--task-file", path, "--to", "fixer", "--data", `{"issue":"#321"}`},
		strings.NewReader(""), &stdout, &stderr, noEnv)
	if code != exitOK || err != nil {
		t.Fatalf("应当退 0，实际 (%d, %v), stderr=%s", code, err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "emit ok: port=fix") {
		t.Fatalf("stdout 应当报告 port=fix，实际 %q", stdout.String())
	}
	frames := rec.wait(t, 10*time.Second, "task-file 的 emit 上行", func(fs []*emitFrame) bool {
		return len(fs) >= 1
	})
	if frames[0].Port != "fix" || frames[0].To != "fixer" {
		t.Fatalf("上行帧不对：%+v", frames[0])
	}
	if string(frames[0].Data) != `{"issue":"#321"}` {
		t.Fatalf("data 没传对：%s", frames[0].Data)
	}
}

// TestRunEmitTaskFileSinglePortNoTo：文件里的 ports 决定「单出口自动取」。
func TestRunEmitTaskFileSinglePortNoTo(t *testing.T) {
	opt := defaultEmitOptions(t, t.TempDir())
	opt.Ports = []string{"out"}
	opt.Expects = map[string]ws.PortExpect{}
	srv, rec := startEmitServer(t, opt)

	contract := `[{"port":"out","targets":[{"capability":"script.echo","version":1}]}]`
	path := writeTaskFile(t, taskFileJSON(t, srv.Path(), emitTestToken, contract))
	var stdout, stderr bytes.Buffer
	code, err := runEmit([]string{"--task-file", path, "--data", `{}`},
		strings.NewReader(""), &stdout, &stderr, noEnv)
	if code != exitOK || err != nil {
		t.Fatalf("应当退 0（单出口自动取 out），实际 (%d, %v), stderr=%s", code, err, stderr.String())
	}
	rec.wait(t, 10*time.Second, "out 的上行帧", func(fs []*emitFrame) bool {
		return len(fs) >= 1 && fs[0].Port == "out"
	})
}

// TestEmitSourceRejectsBothEntrypoints：两套入口同时给 → exit 2，不做隐式优先级。
func TestEmitSourceRejectsBothEntrypoints(t *testing.T) {
	path := writeTaskFile(t, taskFileJSON(t, "/tmp/x.sock", "tok", emitTestContract))
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"sock", map[string]string{envEmitSock: "/tmp/other.sock"}},
		{"token", map[string]string{envTaskToken: "tok2"}},
		{"ports", map[string]string{envEmitPorts: emitTestContract}},
		{"taskdir", map[string]string{envTaskDir: "/tmp/taskdir"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(name string) string { return tc.env[name] }
			var stdout, stderr bytes.Buffer
			code, err := runEmit([]string{"--task-file", path, "--data", `{}`},
				strings.NewReader(""), &stdout, &stderr, getenv)
			if code != exitEmitBadRequest {
				t.Fatalf("同时给两套入口应当退 %d，实际 %d", exitEmitBadRequest, code)
			}
			// 错误里必须点名是哪个环境变量，否则用户不知道该删哪边。
			var want string
			for k := range tc.env {
				want = k
			}
			if err == nil || !strings.Contains(err.Error(), want) ||
				!strings.Contains(err.Error(), "不做隐式优先级") {
				t.Fatalf("错误里应当点名 %s 且说明两套入口不做隐式优先级，实际 %v", want, err)
			}
		})
	}
}

// TestEmitSourceRejectsBadTaskFile：坏 JSON / 缺字段 / 坏 ports 全是 exit 2。
func TestEmitSourceRejectsBadTaskFile(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"不是 JSON", `{`, "不是合法 JSON"},
		{"不是对象", `[1,2]`, "不是合法 JSON"},
		{"缺 token", `{"sock":"/tmp/a.sock","ports":[]}`, "缺 token"},
		{"缺 ports", `{"sock":"/tmp/a.sock","token":"t"}`, "缺 ports"},
		{"ports 不是数组", `{"sock":"/tmp/a.sock","token":"t","ports":{"port":"out"}}`, "不是合法 JSON 数组"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTaskFile(t, tc.body)
			var stdout, stderr bytes.Buffer
			code, err := runEmit([]string{"--task-file", path, "--data", `{}`},
				strings.NewReader(""), &stdout, &stderr, noEnv)
			if code != exitEmitBadRequest {
				t.Fatalf("应当退 %d（修参数，别重试），实际 %d", exitEmitBadRequest, code)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误里应当含 %q，实际 %v", tc.want, err)
			}
		})
	}
}

// TestEmitSourceTaskFilePermTooOpen：token 在文件里，组/其他人可读就是安全问题。
func TestEmitSourceTaskFilePermTooOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.json")
	body := taskFileJSON(t, "/tmp/a.sock", "tok", emitTestContract)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code, err := runEmit([]string{"--task-file", path, "--data", `{}`},
		strings.NewReader(""), &stdout, &stderr, noEnv)
	if code != exitEmitBadRequest {
		t.Fatalf("权限太宽应当退 %d，实际 %d", exitEmitBadRequest, code)
	}
	for _, want := range []string{"0600", "chmod 600"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("错误里应当给出 %q，实际 %v", want, err)
		}
	}
}

// TestEmitSourceTaskFileMissing：文件不存在也是 exit 2（路径写错=修参数）。
func TestEmitSourceTaskFileMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.json")
	var stdout, stderr bytes.Buffer
	code, err := runEmit([]string{"--task-file", missing, "--data", `{}`},
		strings.NewReader(""), &stdout, &stderr, noEnv)
	if code != exitEmitBadRequest {
		t.Fatalf("应当退 %d，实际 %d", exitEmitBadRequest, code)
	}
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("错误里应当带上路径，实际 %v", err)
	}
}

// TestEmitSourceTaskFileEmptySock：sock 为空 = 本地模式（与 env 入口同一个 6）。
func TestEmitSourceTaskFileEmptySock(t *testing.T) {
	path := writeTaskFile(t, taskFileJSON(t, "", "tok", emitTestContract))
	var stdout, stderr bytes.Buffer
	code, err := runEmit([]string{"--task-file", path, "--data", `{}`},
		strings.NewReader(""), &stdout, &stderr, noEnv)
	if code != exitEmitComm {
		t.Fatalf("sock 为空应当退 %d，实际 %d", exitEmitComm, code)
	}
	if err == nil || !strings.Contains(err.Error(), "本地模式") {
		t.Fatalf("错误里应当说明这是本地模式，实际 %v", err)
	}
}

// TestEmitCLITaskFileRealProcess：走真进程确认 --task-file 的 flag 接线（main 分派 +
// argv 里只出现**路径**，token 不出现）。
func TestEmitCLITaskFileRealProcess(t *testing.T) {
	bin := buildAgentBinary(t)

	opt := defaultEmitOptions(t, t.TempDir())
	opt.Expects = map[string]ws.PortExpect{
		"fix": {Schema: json.RawMessage(`{"type":"object","required":["issue"]}`)},
	}
	srv, rec := startEmitServer(t, opt)

	path := writeTaskFile(t, taskFileJSON(t, srv.Path(), emitTestToken, emitTestContract))
	args := []string{"emit", "--task-file", path, "--to", "fixer", "--data", `{"issue":"#321"}`}
	for _, a := range args {
		if strings.Contains(a, emitTestToken) {
			t.Fatalf("token 不许出现在 argv 里：%q", a)
		}
	}

	cmd := exec.Command(bin, args...)
	// 环境里**故意**留着上一次运行的 ANOTIFY_*：agent 侧会剔除它们，但这里更要确认
	// 「文件模式 + 空环境」是干净的组合（真跑时 hc 起的环境本来就没这些变量）。
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("真进程跑失败: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	rec.wait(t, 10*time.Second, "真进程的 task-file emit", func(fs []*emitFrame) bool {
		return len(fs) >= 1 && fs[0].Port == "fix"
	})
}
