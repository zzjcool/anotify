package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/ws"
)

// 本文件覆盖 `anotify-agent emit` 子命令（emit_cmd.go）：参数形状、退出码表
// （emit-protocol.md §2）、以及一条**真进程**的端到端。
//
// 退出码是这层唯一真正重要的东西：调用方（能力进程 / LLM）不看错误文本，只看
// 「该修参数、该停、还是该重试」。所以每条码都单独钉一个用例。

// emitTestEnv 造一个 getenv（等价于进程环境里只有这几项）。
func emitTestEnv(sock, token string) func(string) string {
	return func(name string) string {
		switch name {
		case envEmitSock:
			return sock
		case envTaskToken:
			return token
		}
		return ""
	}
}

// ---------- 本地能判定的失败（不连 socket） ----------

// TestRunEmitLocalModeNoSocket：本地模式（`anotify-agent run`）没有中枢 → 明确报错并退 6。
func TestRunEmitLocalModeNoSocket(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := runEmit([]string{"--port", "fix", "--data", `{"issue":"#42"}`},
		strings.NewReader(""), &stdout, &stderr, emitTestEnv("", ""))
	if code != exitEmitComm {
		t.Fatalf("没有 ANOTIFY_EMIT_SOCK 时应当退 %d，实际 %d", exitEmitComm, code)
	}
	if err == nil || !strings.Contains(err.Error(), "本地模式无中枢，emit 不可用") {
		t.Fatalf("错误里必须明确说「本地模式无中枢，emit 不可用」，实际 %v", err)
	}
}

// TestRunEmitSocketMissing：socket 文件不存在（agent 没在跑 / 任务已结束）→ 退 6。
func TestRunEmitSocketMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.sock")
	var stdout, stderr bytes.Buffer
	code, err := runEmit([]string{"--port", "fix"},
		strings.NewReader(""), &stdout, &stderr, emitTestEnv(missing, "tok"))
	if code != exitEmitComm {
		t.Fatalf("socket 不存在时应当退 %d，实际 %d（err=%v）", exitEmitComm, code, err)
	}
	if err == nil || !strings.Contains(err.Error(), "socket 不存在") {
		t.Fatalf("错误里应当说明 socket 不存在，实际 %v", err)
	}
}

// TestRunEmitBadArguments：参数/形状错全部退 2（「修，别重试」），且**不碰 socket**。
func TestRunEmitBadArguments(t *testing.T) {
	bogusSock := filepath.Join(t.TempDir(), "never.sock") // 真去 dial 会退 6，所以退 2 就证明没 dial
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"port 为空", []string{"--data", `{}`}, "--port 不能为空"},
		{"data 不是 JSON 对象", []string{"--port", "fix", "--data", `[1,2]`}, "必须是 JSON 对象"},
		{"data 是坏 JSON", []string{"--port", "fix", "--data", `{`}, "不是合法 JSON"},
		{"data 超体积", []string{"--port", "fix", "--data", fmt.Sprintf(`{"b":%q}`,
			strings.Repeat("a", emitMaxDataBytes+8))}, "超过上限"},
		{"未知 flag", []string{"--port", "fix", "--nope"}, ""},
		{"多余的位置参数", []string{"--port", "fix", "extra"}, "多余的参数"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code, err := runEmit(tc.args, strings.NewReader(""), &stdout, &stderr,
				emitTestEnv(bogusSock, "tok"))
			if code != exitEmitBadRequest {
				t.Fatalf("应当退 %d（参数/形状错），实际 %d（err=%v）", exitEmitBadRequest, code, err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("错误里应当含 %q，实际 %v", tc.want, err)
			}
		})
	}
}

// TestRunEmitHelp：-h 退 0（usage 由 flag 包写 stderr）。
func TestRunEmitHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := runEmit([]string{"-h"}, strings.NewReader(""), &stdout, &stderr,
		emitTestEnv("", ""))
	if code != exitOK || err != nil {
		t.Fatalf("-h 应当退 0，实际 (%d, %v)", code, err)
	}
	if !strings.Contains(stderr.String(), "-port") {
		t.Errorf("usage 应当写到 stderr，实际 %q", stderr.String())
	}
}

// ---------- 与真服务对接：正常路径 + 各档拒绝 ----------

// TestRunEmitAgainstServer：CLI ↔ emitServer 的真实往返（同一进程里的 unix socket）。
func TestRunEmitAgainstServer(t *testing.T) {
	opt := defaultEmitOptions(t, t.TempDir())
	opt.Expects = map[string]ws.PortExpect{
		"fix": {Schema: json.RawMessage(`{"type":"object","required":["issue"]}`)},
	}
	srv, rec := startEmitServer(t, opt)
	env := emitTestEnv(srv.Path(), emitTestToken)

	t.Run("成功", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code, err := runEmit([]string{"--port", "fix", "--data", `{"issue":"#42"}`, "--note", "第 1 项"},
			strings.NewReader(""), &stdout, &stderr, env)
		if code != exitOK || err != nil {
			t.Fatalf("应当退 0，实际 (%d, %v)", code, err)
		}
		if !strings.Contains(stdout.String(), "acked=durable") {
			t.Fatalf("stdout 应当报告 acked=durable，实际 %q", stdout.String())
		}
		frames := rec.wait(t, 3*time.Second, "emit 上行", func(fs []*emitFrame) bool { return len(fs) >= 1 })
		if frames[0].Port != "fix" || frames[0].Note != "第 1 项" {
			t.Fatalf("上行帧不对：%+v", frames[0])
		}
	})

	t.Run("port 不在白名单 → 3", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code, err := runEmit([]string{"--port", "nope", "--data", `{}`},
			strings.NewReader(""), &stdout, &stderr, env)
		if code != exitEmitPort {
			t.Fatalf("应当退 %d，实际 %d（err=%v）", exitEmitPort, code, err)
		}
		if err == nil || !strings.Contains(err.Error(), "nope") {
			t.Fatalf("错误里应当带上越界的 port，实际 %v", err)
		}
	})

	t.Run("data 不满足 expects → 4", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code, err := runEmit([]string{"--port", "fix", "--data", `{}`},
			strings.NewReader(""), &stdout, &stderr, env)
		if code != exitEmitSchema {
			t.Fatalf("应当退 %d，实际 %d（err=%v）", exitEmitSchema, code, err)
		}
		if err == nil || !strings.Contains(err.Error(), `missing required field "issue"`) {
			t.Fatalf("错误里应当指出缺哪个字段，实际 %v", err)
		}
	})

	t.Run("token 错 → 6", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code, err := runEmit([]string{"--port", "fix", "--data", `{"issue":"#42"}`},
			strings.NewReader(""), &stdout, &stderr, emitTestEnv(srv.Path(), "tok_wrong"))
		if code != exitEmitComm {
			t.Fatalf("应当退 %d，实际 %d（err=%v）", exitEmitComm, code, err)
		}
		if err == nil || !strings.Contains(err.Error(), "token") {
			t.Fatalf("错误里应当提到 token，实际 %v", err)
		}
	})

	t.Run("--data @- 从 stdin 读", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code, err := runEmit([]string{"--port", "note", "--data", "@-"},
			strings.NewReader(`{"issue":"#from-stdin"}`), &stdout, &stderr, env)
		if code != exitOK || err != nil {
			t.Fatalf("应当退 0，实际 (%d, %v)", code, err)
		}
		// 按 port 找这一条，**不能取最后一帧**：前面子用例的上行帧是异步到达的，
		// 抢占 last 会让断言随机失败（失败信息里出现 Port:fix 就是它）。
		var got *emitFrame
		for _, f := range rec.all() {
			if f.Port == "note" {
				got = f
			}
		}
		if got == nil {
			t.Fatalf("没收到 --port note 的 emit 帧")
		}
		if string(got.Data) != `{"issue":"#from-stdin"}` {
			t.Fatalf("stdin 的 data 没传对：%+v", got)
		}
	})
}

// ---------- 响应 → 退出码：整张表 ----------

// fakeEmitSocket 起一个只会回一条固定响应的 unix socket（覆盖 CLI 的映射表用）。
func fakeEmitSocket(t *testing.T, resp emitResponse) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadString('\n')
		data, _ := json.Marshal(resp)
		_, _ = conn.Write(append(data, '\n'))
	}()
	return path
}

// TestRunEmitExitCodeTable 钉死 emit-protocol.md §2 的整张表（含 acked/error 的兜底）。
func TestRunEmitExitCodeTable(t *testing.T) {
	cases := []struct {
		name string
		resp emitResponse
		want int
	}{
		{"成功 durable", emitResponse{OK: true, Seq: 1, Acked: emitAckedDurable}, exitOK},
		{"成功 routed", emitResponse{OK: true, Seq: 1, Acked: emitAckedRouted}, exitOK},
		{"port", emitResponse{Seq: 1, Rejected: emitRejectPort, Error: "x"}, exitEmitPort},
		{"schema", emitResponse{Seq: 1, Rejected: emitRejectSchema, Error: "x"}, exitEmitSchema},
		{"size", emitResponse{Seq: 1, Rejected: emitRejectSize, Error: "x"}, exitEmitBadRequest},
		{"rate", emitResponse{Seq: 1, Rejected: emitRejectRate, Error: "x"}, exitEmitBadRequest},
		{"task", emitResponse{Seq: 1, Rejected: emitRejectTask, Error: "x"}, exitEmitTask},
		{"token", emitResponse{Seq: 1, Rejected: emitRejectToken, Error: "x"}, exitEmitComm},
		// 默认路径落盘失败 / --wait-routed 超时 / 中枢拒收：兜底 7。
		{"落盘失败", emitResponse{Seq: 1, Error: "outbox 落盘失败"}, exitEmitAckTimeout},
		{"等 routed 超时", emitResponse{Seq: 1, Acked: emitAckedDurable, Error: "timeout"}, exitEmitAckTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sock := fakeEmitSocket(t, tc.resp)
			var stdout, stderr bytes.Buffer
			code, _ := runEmit([]string{"--port", "fix", "--data", `{}`},
				strings.NewReader(""), &stdout, &stderr, emitTestEnv(sock, "tok"))
			if code != tc.want {
				t.Fatalf("响应 %+v 应当退 %d，实际 %d", tc.resp, tc.want, code)
			}
		})
	}
}

// TestRunEmitWaitRoutedFlag：--wait-routed 必须在请求里带 wait:"routed"（服务端据此等中枢）。
func TestRunEmitWaitRoutedFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fake.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	got := make(chan emitRequest, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadString('\n')
		var req emitRequest
		_ = json.Unmarshal([]byte(line), &req)
		got <- req
		data, _ := json.Marshal(emitResponse{OK: true, Seq: 7, Acked: emitAckedRouted})
		_, _ = conn.Write(append(data, '\n'))
	}()

	var stdout, stderr bytes.Buffer
	code, err := runEmit([]string{"--port", "fix", "--data", `{"a":1}`, "--wait-routed"},
		strings.NewReader(""), &stdout, &stderr, emitTestEnv(path, "tok"))
	if code != exitOK || err != nil {
		t.Fatalf("应当退 0，实际 (%d, %v)", code, err)
	}
	select {
	case req := <-got:
		if req.Op != emitOpName || req.Wait != emitWaitRoutedName || req.Token != "tok" ||
			req.Port != "fix" || string(req.Data) != `{"a":1}` {
			t.Fatalf("请求线格式不对：%+v", req)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("假 socket 没收到请求")
	}
	if !strings.Contains(stdout.String(), "acked=routed") {
		t.Fatalf("stdout 应当报告 acked=routed，实际 %q", stdout.String())
	}
}

// ---------- main 分派 ----------

// TestMainWithEmitDispatch：main 必须按 args[0] == "emit" 分派出去，且**不改写**退出码。
func TestMainWithEmitDispatch(t *testing.T) {
	t.Run("本地模式 → 6", func(t *testing.T) {
		t.Setenv(envEmitSock, "")
		var stdout, stderr bytes.Buffer
		code := mainWith([]string{"emit", "--port", "fix", "--data", `{}`}, &stdout, &stderr)
		if code != exitEmitComm {
			t.Fatalf("mainWith(emit) = %d, want %d", code, exitEmitComm)
		}
		if !strings.Contains(stderr.String(), "anotify-agent emit:") ||
			!strings.Contains(stderr.String(), "本地模式无中枢") {
			t.Fatalf("stderr 必须给出可读原因，实际 %q", stderr.String())
		}
	})

	t.Run("参数错 → 2", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := mainWith([]string{"emit"}, &stdout, &stderr); code != exitEmitBadRequest {
			t.Fatalf("mainWith(emit) = %d, want %d", code, exitEmitBadRequest)
		}
	})

	t.Run("-h → 0", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := mainWith([]string{"emit", "-h"}, &stdout, &stderr); code != exitOK {
			t.Fatalf("mainWith(emit -h) = %d, want 0", code)
		}
	})
}

// ---------- 真进程端到端 ----------

var (
	agentBinaryOnce sync.Once
	agentBinaryPath string
	agentBinaryErr  error
)

// buildAgentBinary 编译出**真的** anotify-agent（整个测试包共用一次）。
func buildAgentBinary(t *testing.T) string {
	t.Helper()
	agentBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "anotify-agent-bin")
		if err != nil {
			agentBinaryErr = err
			return
		}
		out := filepath.Join(dir, "anotify-agent")
		cmd := exec.Command("go", "build", "-o", out, ".")
		cmd.Dir = "."
		combined, err := cmd.CombinedOutput()
		if err != nil {
			agentBinaryErr = fmt.Errorf("go build: %w\n%s", err, combined)
			return
		}
		agentBinaryPath = out
	})
	if agentBinaryErr != nil {
		t.Skipf("编不出 agent 二进制（跳过真进程用例）: %v", agentBinaryErr)
	}
	return agentBinaryPath
}

// TestEmitCLIRealProcess 是**两条进程**的端到端：测试进程跑 emit 服务（socket），
// 子进程跑真的 `anotify-agent emit`。它验证的是「能力进程在真环境里怎么用」：
// 环境变量 + 参数 + 退出码，全部经真实 exec 走一遍。
func TestEmitCLIRealProcess(t *testing.T) {
	bin := buildAgentBinary(t)

	opt := defaultEmitOptions(t, t.TempDir())
	opt.Expects = map[string]ws.PortExpect{
		"fix": {Schema: json.RawMessage(`{"type":"object","required":["issue"]}`)},
	}
	srv, rec := startEmitServer(t, opt)

	run := func(t *testing.T, token string, args ...string) (int, string, string) {
		t.Helper()
		cmd := exec.Command(bin, append([]string{"emit"}, args...)...)
		cmd.Env = append(os.Environ(),
			envEmitSock+"="+srv.Path(),
			envTaskToken+"="+token,
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		code := 0
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("跑子进程失败: %v", err)
			}
			code = exitErr.ExitCode()
		}
		return code, stdout.String(), stderr.String()
	}

	t.Run("成功：durable + outbox", func(t *testing.T) {
		code, stdout, stderr := run(t, emitTestToken, "--port", "fix", "--data", `{"issue":"#42"}`)
		if code != exitOK {
			t.Fatalf("exit = %d, want 0（stdout=%q stderr=%q）", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "acked=durable") {
			t.Fatalf("stdout 应当报告 acked=durable，实际 %q", stdout)
		}
		recs := outboxRecords(t, srv.OutboxPath())
		if len(recs) == 0 {
			t.Fatal("outbox 里没有这一条")
		}
		last := recs[len(recs)-1]
		if last.Port != "fix" || string(last.Data) != `{"issue":"#42"}` {
			t.Fatalf("outbox 记录不对：%+v", last)
		}
	})

	t.Run("缺必填字段 → 4", func(t *testing.T) {
		code, _, stderr := run(t, emitTestToken, "--port", "fix", "--data", `{}`)
		if code != exitEmitSchema {
			t.Fatalf("exit = %d, want %d（stderr=%q）", code, exitEmitSchema, stderr)
		}
		if !strings.Contains(stderr, "issue") {
			t.Fatalf("stderr 应当指出缺 issue，实际 %q", stderr)
		}
	})

	t.Run("port 越界 → 3", func(t *testing.T) {
		code, _, _ := run(t, emitTestToken, "--port", "nope", "--data", `{}`)
		if code != exitEmitPort {
			t.Fatalf("exit = %d, want %d", code, exitEmitPort)
		}
	})

	t.Run("token 错 → 6", func(t *testing.T) {
		code, _, _ := run(t, "tok_wrong", "--port", "fix", "--data", `{"issue":"#1"}`)
		if code != exitEmitComm {
			t.Fatalf("exit = %d, want %d", code, exitEmitComm)
		}
	})

	t.Run("--data @- 从 stdin 读", func(t *testing.T) {
		cmd := exec.Command(bin, "emit", "--port", "note", "--data", "@-")
		cmd.Env = append(os.Environ(), envEmitSock+"="+srv.Path(), envTaskToken+"="+emitTestToken)
		cmd.Stdin = strings.NewReader(`{"issue":"#stdin"}`)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("子进程失败: %v（stderr=%q）", err, stderr.String())
		}
		frames := rec.all()
		last := frames[len(frames)-1]
		if last.Port != "note" || string(last.Data) != `{"issue":"#stdin"}` {
			t.Fatalf("stdin 的 data 没传对：%+v", last)
		}
	})
}

// TestEmitCLIRealProcessLocalMode：整条 `anotify-agent run`（本地模式）里调 emit，
// 必须拿到「本地模式无中枢，emit 不可用」而不是静默丢事件。
func TestEmitCLIRealProcessLocalMode(t *testing.T) {
	bin := buildAgentBinary(t)

	// 一个会调 emit 的能力产物：脚本用同一份二进制调自己（真实用法）。
	capYAML := fmt.Sprintf(`apiVersion: anotify/v1
kind: Capability
metadata:
  name: demo.emit-local
  version: 1
spec:
  outputs: []
  run:
    entrypoint: run.sh
    files:
      run.sh: |
        #!/bin/bash
        echo "sock=[${ANOTIFY_EMIT_SOCK:-}]"
        %q emit --port fix --data '{"issue":"#42"}'
        echo "emit-exit=$?"
`, bin)
	dir := t.TempDir()
	path := filepath.Join(dir, "cap.yaml")
	if err := os.WriteFile(path, []byte(capYAML), 0o600); err != nil {
		t.Fatalf("写产物: %v", err)
	}

	cmd := exec.Command(bin, "run", path)
	cmd.Env = append(os.Environ(), envEmitSock+"=", envTaskToken+"=")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		// 脚本最后一条命令是 echo，所以 run 应当退 0；非 0 就是 agent 侧故障。
		t.Fatalf("run 失败: %v（stdout=%q stderr=%q）", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "sock=[]") {
		t.Fatalf("本地模式不该注入 ANOTIFY_EMIT_SOCK，stdout=%q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "emit-exit=6") {
		t.Fatalf("本地模式里 emit 应当退 6，stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "本地模式无中枢，emit 不可用") {
		t.Fatalf("stderr 应当明确说「本地模式无中枢，emit 不可用」，实际 %q", stderr.String())
	}
}
