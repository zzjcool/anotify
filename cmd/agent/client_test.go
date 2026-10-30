package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/zzjcool/anotify/internal/ws"
)

// 本文件覆盖契约 §5 的 W4 必测矩阵 + §11.1/§11.3 的返工项（F1/F3/F4）。
//
// 两条腿走路：
//   - 内存 fake nodeConn（快、确定）：调度细节（ack 顺序、cancel、信号量、params 透传）；
//   - httptest + coder/websocket 起**真**连接（§11.1 的 F1 回归锚）：优雅退出时 result
//     到底能不能发出去。这一条**不能**交给 fake：fake 的读语义是本文件自己写的，
//     而 F1 恰恰死在真库的读语义上（Conn.Read(ctx) 取消 ctx = c.close()）。
//
// 命名一律带 w4 前缀：同包的 config_test.go / protocol_test.go / runner_test.go
// 是别人的文件，helper 名不能撞。

// ---------- fake 连接 ----------

// w4Conn 是 nodeConn 的内存实现：
//   - read 从测试塞进去的 in 通道取帧（也可以被 ctx 取消 / close 打断），
//     并记下「交出去的帧」，供测试断言某帧真的被读到了；
//   - write 把 agent 发出的帧**按序**记下来（含原始 JSON），供断言顺序与内容。
type w4Conn struct {
	in     chan []byte
	closed chan struct{}
	once   sync.Once

	mu      sync.Mutex
	sent    []map[string]any
	readLog []map[string]any
}

func newW4Conn() *w4Conn {
	return &w4Conn{
		in:     make(chan []byte, 32),
		closed: make(chan struct{}),
	}
}

// read 的语义**对齐真库**：读的 ctx 一取消就等于「连接没了」。
//
// coder/websocket 的 Conn.Read(ctx) 走 setupReadTimeout → context.AfterFunc(ctx,
// func() { c.close() })（conn.go:188-200），所以取消读 ctx 是「关连接」，不是「读超时」；
// 这里也立刻返回 ctx.Err()。
//
// 历史上这里有过一个 postCancelReadWindow（取消后还再交付 150ms 内到达的帧），
// 那是**假的**：它把「父 ctx 取消并不影响读」这个错误前提固化成了测试语义，
// 于是 F1（SIGINT 一来连接被库关掉、grace 期的 result 全丢）在 fake 上永远绿。
// 现在契约要求读循环用连接自己的 ctx（a.base），本 fake 的 ctx 也就是 a.base，
// 因此「取消后读不到帧」= 收尾，与真库一致。
func (c *w4Conn) read(ctx context.Context) ([]byte, error) {
	select {
	case data := <-c.in:
		return c.deliver(data), nil
	case <-c.closed:
		return nil, errors.New("fake conn closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// deliver 记一笔「这帧真的交给 agent 了」（type/task_id 足够定位）。
func (c *w4Conn) deliver(data []byte) []byte {
	var frame map[string]any
	if err := json.Unmarshal(data, &frame); err == nil {
		c.mu.Lock()
		c.readLog = append(c.readLog, frame)
		c.mu.Unlock()
	}
	return data
}

// delivered 返回已经交给 agent 的下行帧（快照）。
func (c *w4Conn) delivered() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]map[string]any, len(c.readLog))
	copy(out, c.readLog)
	return out
}

func (c *w4Conn) write(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var frame map[string]any
	if err := json.Unmarshal(data, &frame); err != nil {
		return fmt.Errorf("fake conn: frame is not json (%q): %w", data, err)
	}

	c.mu.Lock()
	c.sent = append(c.sent, frame)
	c.mu.Unlock()

	select {
	case <-c.closed:
		// 真连接关掉之后也写不进去；这里同样拒绝，免得掩盖「关连接后还在写」。
		return errors.New("fake conn closed")
	default:
		return nil
	}
}

func (c *w4Conn) close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// feed 塞一帧下行帧给 agent。
func (c *w4Conn) feed(t *testing.T, f any) {
	t.Helper()
	data, err := encodeFrame(f)
	if err != nil {
		t.Fatalf("编码下行帧失败: %v", err)
	}
	c.in <- data
}

// frames 返回 agent 到目前为止发出的全部帧（快照，按序）。
func (c *w4Conn) frames() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]map[string]any, len(c.sent))
	copy(out, c.sent)
	return out
}

// waitFor 轮询到 cond 成立为止（避免再引一套 signal/notify 机制）。
func (c *w4Conn) waitFor(t *testing.T, timeout time.Duration, what string, cond func([]map[string]any) bool) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		frames := c.frames()
		if cond(frames) {
			return frames
		}
		if time.Now().After(deadline) {
			t.Fatalf("等 %s 超时（%s）；agent 已发帧: %s", what, timeout, w4Describe(frames))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ---------- 帧断言 helper ----------

// w4Find 找第一个 type（taskID 非空时还要求 task_id）匹配的帧。
func w4Find(frames []map[string]any, typ, taskID string) (map[string]any, int, bool) {
	for i, f := range frames {
		if f["type"] != typ {
			continue
		}
		if taskID != "" && f["task_id"] != taskID {
			continue
		}
		return f, i, true
	}
	return nil, -1, false
}

func w4Has(frames []map[string]any, typ, taskID string) bool {
	_, _, ok := w4Find(frames, typ, taskID)
	return ok
}

// w4Cond 是 waitFor 的常用条件：出现过某个 type（+ task_id）的帧。
func w4Cond(typ, taskID string) func([]map[string]any) bool {
	return func(frames []map[string]any) bool { return w4Has(frames, typ, taskID) }
}

// w4Describe 把帧序列压成一行，失败时能一眼看出「到底走到了哪一步」。
func w4Describe(frames []map[string]any) string {
	parts := make([]string, 0, len(frames))
	for _, f := range frames {
		typ, _ := f["type"].(string)
		taskID, _ := f["task_id"].(string)
		status, _ := f["status"].(string)
		part := typ
		if taskID != "" {
			part += ":" + taskID
		}
		if status != "" {
			part += "=" + status
		}
		parts = append(parts, part)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// w4WaitFile 等到文件出现（子进程真的开始跑了的标志）。
func w4WaitFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等 %s 出现超时（%s）：命令没跑起来", path, timeout)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ---------- 测试夹具 ----------

// w4Config 直接构造 nodeConfig（不走 yaml：client 层只用到这些字段）。
func w4Config(caps ...capabilityConfig) *nodeConfig {
	return &nodeConfig{
		Name:         "test-node",
		Server:       "http://localhost:8080",
		Token:        "test-token",
		Instances:    1,
		Capabilities: caps,
	}
}

func w4Cap(capability string, version int, command string) capabilityConfig {
	return capabilityConfig{
		Capability: capability,
		Version:    version,
		Run:        &runSpec{Runner: "bash", Command: command},
	}
}

// w4Harness 是一轮 serveNode 的测试上下文。
type w4Harness struct {
	t      *testing.T
	fc     *w4Conn
	cancel context.CancelFunc
	done   chan error

	mu  sync.Mutex
	got bool
	err error
}

// w4StartRaw 起 serveNode（不替测试发 welcome），等 hello 发出后返回。
func w4StartRaw(t *testing.T, cfg *nodeConfig, grace time.Duration) *w4Harness {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	fc := newW4Conn()
	// 日志默认静音：断言失败时要看的是帧序列，不是 slog 输出。
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))

	done := make(chan error, 1)
	go func() { done <- serveNode(ctx, cfg, fc, logger, grace) }()

	h := &w4Harness{t: t, fc: fc, cancel: cancel, done: done}
	t.Cleanup(func() {
		cancel()
		h.wait(5 * time.Second)
	})

	// hello 必须是连接上的第一帧（契约 §3.4.1）。
	h.fc.waitFor(t, 3*time.Second, "hello 帧", func(frames []map[string]any) bool {
		return len(frames) > 0
	})
	if got := h.fc.frames()[0]["type"]; got != frameTypeHello {
		t.Fatalf("第一帧必须是 hello，实际 %v", got)
	}
	return h
}

// w4Start 起 serveNode 并完成握手（发 welcome）。
func w4Start(t *testing.T, cfg *nodeConfig, heartbeatSec int, grace time.Duration) *w4Harness {
	t.Helper()
	h := w4StartRaw(t, cfg, grace)
	h.fc.feed(t, &welcomeFrame{
		Type:         frameTypeWelcome,
		NodeID:       "nod_test",
		Protocol:     ws.ProtocolVersion,
		HeartbeatSec: heartbeatSec,
		LeaseTTLSec:  30,
	})
	return h
}

// wait 等 serveNode 返回（可重复调用，结果缓存）。
func (h *w4Harness) wait(timeout time.Duration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.got {
		return h.err
	}
	select {
	case err := <-h.done:
		h.got, h.err = true, err
	case <-time.After(timeout):
		h.t.Errorf("serveNode 未在 %s 内返回", timeout)
		h.got, h.err = true, nil
	}
	return h.err
}

// stop 用 SIGINT 的方式退出并断言「干净返回 nil」。
func (h *w4Harness) stop() {
	h.t.Helper()
	h.cancel()
	if err := h.wait(5 * time.Second); err != nil {
		h.t.Fatalf("优雅退出应返回 nil，实际 %v", err)
	}
}

// ---------- W4-1: hello → welcome → 心跳 ----------

func TestClientHelloWelcomeHeartbeat(t *testing.T) {
	cfg := w4Config(w4Cap("demo.prep", 1, "echo prep"))
	// heartbeat_sec=1 → 间隔 max(1s, 0.5s) = 1s，测试 3s 内必有一拍。
	h := w4Start(t, cfg, 1, 100*time.Millisecond)

	hello := h.fc.frames()[0]
	if hello["type"] != frameTypeHello {
		t.Fatalf("第一帧必须是 hello，实际 %v", hello["type"])
	}
	if got := hello["protocol"]; got != float64(ws.ProtocolVersion) {
		t.Errorf("hello.protocol 应为 %d，实际 %v", ws.ProtocolVersion, got)
	}
	caps, ok := hello["capabilities"].([]any)
	if !ok || len(caps) != len(cfg.Capabilities) {
		t.Errorf("hello.capabilities 应声明 %d 条，实际 %#v", len(cfg.Capabilities), hello["capabilities"])
	}

	frames := h.fc.waitFor(t, 4*time.Second, "心跳帧", w4Cond(frameTypeHeartbeat, ""))
	hb, _, ok := w4Find(frames, frameTypeHeartbeat, "")
	if !ok {
		t.Fatalf("没收到心跳帧: %s", w4Describe(frames))
	}
	ts, ok := hb["ts"].(float64)
	if !ok || ts <= 0 {
		t.Fatalf("心跳 ts 必须是正的 unix 秒，实际 %#v", hb["ts"])
	}

	h.stop()
}

// ---------- W4-2: assign → ack 先于 result，result succeeded ----------

func TestClientAssignAckThenResult(t *testing.T) {
	cfg := w4Config(w4Cap("demo.prep", 1, "echo hello-agent"))
	h := w4Start(t, cfg, 10, 200*time.Millisecond)

	h.fc.feed(t, &assignFrame{
		Type:        frameTypeAssign,
		TaskID:      "tsk_1",
		Capability:  "demo.prep",
		Version:     1,
		Params:      map[string]any{"title": "hello"},
		LeaseUntil:  time.Now().Add(time.Minute).Unix(),
		MaxDuration: 10,
	})

	frames := h.fc.waitFor(t, 5*time.Second, "tsk_1 的 result", w4Cond(frameTypeResult, "tsk_1"))

	_, ackIdx, ok := w4Find(frames, frameTypeAck, "tsk_1")
	if !ok {
		t.Fatalf("assign 必须先 ack: %s", w4Describe(frames))
	}
	res, resIdx, ok := w4Find(frames, frameTypeResult, "tsk_1")
	if !ok {
		t.Fatalf("没收到 result: %s", w4Describe(frames))
	}
	if ackIdx > resIdx {
		t.Fatalf("ack 必须先于 result（ack@%d, result@%d）", ackIdx, resIdx)
	}
	if res["task_id"] != "tsk_1" {
		t.Errorf("result.task_id 应为 tsk_1，实际 %v", res["task_id"])
	}
	if res["status"] != statusSucceeded {
		t.Errorf("status 应为 %s，实际 %v（error=%v）", statusSucceeded, res["status"], res["error"])
	}
	if res["summary"] != "hello-agent" {
		t.Errorf("summary 应是命令输出的首行（hello-agent），实际 %#v", res["summary"])
	}
	if res["error"] != "" {
		t.Errorf("成功的任务 error 必须为空，实际 %#v", res["error"])
	}
	if _, present := res["out"]; present {
		t.Errorf("未声明 run.out 时 result 帧不能出现 out 键，实际 %#v", res)
	}
	artifacts, ok := res["artifacts"].([]any)
	if !ok || len(artifacts) != 0 {
		t.Errorf("artifacts 必须是 []（不是 null），实际 %#v", res["artifacts"])
	}

	h.stop()
}

// ---------- 附加：声明了 run.out → result 帧带 out（sendResult 透传的回归锁） ----------

// TestClientReportsOut 证明「node.yaml 声明 run.out 且任务成功」时 result 帧真的带
// "out":"ok"：runner 设了 outcome.Out，还得靠 sendResult 透传到帧上（两处都得对）。
func TestClientReportsOut(t *testing.T) {
	cfg := w4Config(w4Cap("demo.prep", 1, "echo hello-out"))
	cfg.Capabilities[0].Run.Out = "ok" // 对应 node.yaml 的 run.out: ok
	h := w4Start(t, cfg, 10, 200*time.Millisecond)

	h.fc.feed(t, &assignFrame{
		Type:       frameTypeAssign,
		TaskID:     "tsk_out",
		Capability: "demo.prep",
		Version:    1,
	})

	frames := h.fc.waitFor(t, 5*time.Second, "tsk_out 的 result", w4Cond(frameTypeResult, "tsk_out"))
	res, _, _ := w4Find(frames, frameTypeResult, "tsk_out")
	if res["status"] != statusSucceeded {
		t.Fatalf("status 应为 %s，实际 %v（error=%v）", statusSucceeded, res["status"], res["error"])
	}
	got, present := res["out"]
	if !present {
		t.Fatalf("声明了 run.out 的 success result 必须带 out 键，实际 %#v", res)
	}
	if got != "ok" {
		t.Fatalf("result.out = %#v, want %q", got, "ok")
	}

	h.stop()
}

// ---------- W4-3: 失败命令 → result failed ----------

func TestClientFailedCommand(t *testing.T) {
	cfg := w4Config(w4Cap("demo.prep", 1, "echo boom >&2; exit 7"))
	h := w4Start(t, cfg, 10, 200*time.Millisecond)

	h.fc.feed(t, &assignFrame{
		Type:       frameTypeAssign,
		TaskID:     "tsk_fail",
		Capability: "demo.prep",
		Version:    1,
	})

	frames := h.fc.waitFor(t, 5*time.Second, "tsk_fail 的 result", w4Cond(frameTypeResult, "tsk_fail"))
	res, _, _ := w4Find(frames, frameTypeResult, "tsk_fail")
	if res["status"] != statusFailed {
		t.Fatalf("status 应为 %s，实际 %v", statusFailed, res["status"])
	}
	errText, _ := res["error"].(string)
	if strings.TrimSpace(errText) == "" {
		t.Fatalf("失败的任务 error 必须非空，实际 %#v", res["error"])
	}

	h.stop()
}

// ---------- W4-4: cancel → 3s 内 result failed（error 含 canceled） ----------

func TestClientCancel(t *testing.T) {
	// 命令先落一个 marker 文件再睡：marker 出现 = 子进程真的在跑了，
	// 这时才发 cancel，测的才是「杀进程组」而不是「还没起来就被取消」。
	marker := filepath.Join(t.TempDir(), "started")
	cfg := w4Config(w4Cap("demo.prep", 1, "touch "+marker+"; sleep 5"))
	h := w4Start(t, cfg, 10, 2*time.Second)

	h.fc.feed(t, &assignFrame{
		Type:       frameTypeAssign,
		TaskID:     "tsk_cancel",
		Capability: "demo.prep",
		Version:    1,
	})
	h.fc.waitFor(t, 3*time.Second, "tsk_cancel 的 ack", w4Cond(frameTypeAck, "tsk_cancel"))
	w4WaitFile(t, marker, 2*time.Second)

	started := time.Now()
	h.fc.feed(t, &cancelFrame{Type: frameTypeCancel, TaskID: "tsk_cancel", Reason: "user canceled"})

	frames := h.fc.waitFor(t, 3*time.Second, "tsk_cancel 的 failed result", func(frames []map[string]any) bool {
		f, _, ok := w4Find(frames, frameTypeResult, "tsk_cancel")
		return ok && f["status"] == statusFailed
	})
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("cancel 后 %s 才返回（契约要求 3s 内被取消的进程组死掉并回 result）", elapsed)
	}

	res, _, _ := w4Find(frames, frameTypeResult, "tsk_cancel")
	errText, _ := res["error"].(string)
	if !strings.Contains(strings.ToLower(errText), "canceled") {
		t.Fatalf("cancel 后的 result.error 必须含 canceled，实际 %q", errText)
	}

	h.stop()
}

// ---------- W4-5: 优雅退出（等在跑任务 + draining 不 ack） ----------

func TestClientGracefulShutdown(t *testing.T) {
	// 任务要睡得比「取消 ctx」久一点，才真的测到「等任务收尾」而不是等一个空集合。
	cfg := w4Config(w4Cap("demo.prep", 1, "sleep 0.3; echo drained"))
	h := w4Start(t, cfg, 10, 2*time.Second)

	h.fc.feed(t, &assignFrame{
		Type:        frameTypeAssign,
		TaskID:      "tsk_grace",
		Capability:  "demo.prep",
		Version:     1,
		Params:      map[string]any{},
		MaxDuration: 10,
	})
	h.fc.waitFor(t, 3*time.Second, "tsk_grace 的 ack", w4Cond(frameTypeAck, "tsk_grace"))

	// 模拟 SIGINT：任务还在跑就取消 ctx（grace=2s 足够它跑完）。
	h.cancel()

	// draining 之后中枢又派了一单：不 ack（让租约到期回收，契约 §3.4.6）。
	h.fc.feed(t, &assignFrame{
		Type:       frameTypeAssign,
		TaskID:     "tsk_after",
		Capability: "demo.prep",
		Version:    1,
	})

	if err := h.wait(5 * time.Second); err != nil {
		t.Fatalf("优雅退出应返回 nil，实际 %v", err)
	}

	frames := h.fc.frames()
	res, _, ok := w4Find(frames, frameTypeResult, "tsk_grace")
	if !ok {
		t.Fatalf("grace 内跑完的任务必须把 result 发出去: %s", w4Describe(frames))
	}
	if res["status"] != statusSucceeded {
		t.Fatalf("grace 内跑完的任务应 succeeded（不该被当成「退出即杀」），实际 %v（error=%v）",
			res["status"], res["error"])
	}
	if res["summary"] != "drained" {
		t.Errorf("summary 应为 drained，实际 %#v", res["summary"])
	}
	// 先确认「draining 期间的 assign 真的被读循环收到了」：没收到的话下面两条断言
	// 就是空转（换了实现也会绿），不算覆盖。
	if !w4Has(h.fc.delivered(), frameTypeAssign, "tsk_after") {
		t.Fatalf("draining 期间塞的 assign 没被读到，这条用例没真正覆盖到；已交付帧: %s",
			w4Describe(h.fc.delivered()))
	}
	if w4Has(frames, frameTypeAck, "tsk_after") {
		t.Errorf("draining 期间不该 ack 新 assign: %s", w4Describe(frames))
	}
	if w4Has(frames, frameTypeResult, "tsk_after") {
		t.Errorf("draining 期间的 assign 不该被执行: %s", w4Describe(frames))
	}
}

// ---------- W4-6: 找不到 run 映射 → ack + result failed ----------

func TestClientNoRunSpec(t *testing.T) {
	cfg := w4Config(w4Cap("demo.prep", 1, "echo prep"))
	h := w4Start(t, cfg, 10, 200*time.Millisecond)

	h.fc.feed(t, &assignFrame{
		Type:       frameTypeAssign,
		TaskID:     "tsk_nospec",
		Capability: "demo.other",
		Version:    2,
	})

	frames := h.fc.waitFor(t, 5*time.Second, "tsk_nospec 的 result", w4Cond(frameTypeResult, "tsk_nospec"))

	_, ackIdx, ok := w4Find(frames, frameTypeAck, "tsk_nospec")
	if !ok {
		t.Fatalf("找不到 run 也要先 ack（否则中枢只能等租约）: %s", w4Describe(frames))
	}
	res, resIdx, ok := w4Find(frames, frameTypeResult, "tsk_nospec")
	if !ok {
		t.Fatalf("没收到 result: %s", w4Describe(frames))
	}
	if ackIdx > resIdx {
		t.Errorf("ack 必须先于 result（ack@%d, result@%d）", ackIdx, resIdx)
	}
	if res["status"] != statusFailed {
		t.Errorf("status 应为 %s，实际 %v", statusFailed, res["status"])
	}
	errText, _ := res["error"].(string)
	if !strings.Contains(errText, "no run spec for demo.other@2") {
		t.Errorf("error 应含 %q，实际 %q", "no run spec for demo.other@2", errText)
	}

	h.stop()
}

// ---------- 附加：params 透传 / 每能力信号量限流（契约 §3.4.3） ----------

func TestClientPassesParams(t *testing.T) {
	// 参数只走环境变量（不拼命令）：client 的职责是把 assign.Params 原样交给 job。
	cfg := w4Config(w4Cap("demo.prep", 1, `printf '%s\n' "$ANOTIFY_PARAM_TITLE"`))
	h := w4Start(t, cfg, 10, 200*time.Millisecond)

	h.fc.feed(t, &assignFrame{
		Type:       frameTypeAssign,
		TaskID:     "tsk_params",
		Capability: "demo.prep",
		Version:    1,
		Params:     map[string]any{"title": "hello-param"},
	})

	frames := h.fc.waitFor(t, 5*time.Second, "tsk_params 的 result", w4Cond(frameTypeResult, "tsk_params"))
	res, _, _ := w4Find(frames, frameTypeResult, "tsk_params")
	if res["status"] != statusSucceeded {
		t.Fatalf("status 应为 succeeded，实际 %v（error=%v）", res["status"], res["error"])
	}
	if res["summary"] != "hello-param" {
		t.Errorf("params 没透传到任务（summary 应为 hello-param），实际 %#v", res["summary"])
	}

	h.stop()
}

func TestClientSemaphoreLimitsConcurrency(t *testing.T) {
	// instances=1 + concurrency 缺省 → taskLimit=1：两个任务必须串行（不能同时 start）。
	trace := filepath.Join(t.TempDir(), "trace")
	command := "echo start >> " + trace + "; sleep 0.3; echo end >> " + trace
	cfg := w4Config(w4Cap("demo.prep", 1, command))
	h := w4Start(t, cfg, 10, 2*time.Second)

	for _, id := range []string{"tsk_a", "tsk_b"} {
		h.fc.feed(t, &assignFrame{
			Type:       frameTypeAssign,
			TaskID:     id,
			Capability: "demo.prep",
			Version:    1,
		})
	}

	frames := h.fc.waitFor(t, 10*time.Second, "两个任务都回 result", func(frames []map[string]any) bool {
		return w4Has(frames, frameTypeResult, "tsk_a") && w4Has(frames, frameTypeResult, "tsk_b")
	})
	for _, id := range []string{"tsk_a", "tsk_b"} {
		res, _, _ := w4Find(frames, frameTypeResult, id)
		if res["status"] != statusSucceeded {
			t.Fatalf("%s 应 succeeded，实际 %v（error=%v）", id, res["status"], res["error"])
		}
	}

	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatalf("读 trace 失败: %v", err)
	}
	// 串行的唯一证据：start/end 严格交替。并发跑会是 start start end end。
	got := strings.Fields(string(data))
	want := []string{"start", "end", "start", "end"}
	if !slices.Equal(got, want) {
		t.Fatalf("两个任务被并发执行了（信号量没生效）：trace = %v，期望 %v", got, want)
	}

	h.stop()
}

// ---------- 附加：握手被拒 / 连接断开（契约 §3.4.1、§3.4.7） ----------

func TestClientHandshakeRejected(t *testing.T) {
	cfg := w4Config(w4Cap("demo.prep", 1, "echo prep"))
	h := w4StartRaw(t, cfg, time.Second) // 故意不发 welcome

	h.fc.feed(t, &errorFrame{
		Type:    frameTypeError,
		Code:    ws.ErrCodeInvalidManifest,
		Message: "run is required",
	})

	err := h.wait(5 * time.Second)
	if err == nil {
		t.Fatalf("welcome 前收到 error 帧必须作为握手失败返回")
	}
	want := "handshake rejected: " + ws.ErrCodeInvalidManifest + ": run is required"
	if err.Error() != want {
		t.Fatalf("错误文案必须逐字固定（契约 §3.4.1）:\n want %q\n  got %q", want, err.Error())
	}
}

func TestClientConnectionClosed(t *testing.T) {
	cfg := w4Config(w4Cap("demo.prep", 1, "echo prep"))
	h := w4Start(t, cfg, 10, time.Second)

	if err := h.fc.close(); err != nil {
		t.Fatalf("关闭 fake 连接失败: %v", err)
	}

	err := h.wait(5 * time.Second)
	if err == nil {
		t.Fatalf("连接断开应返回错误（v1 不重连）")
	}
	if !strings.HasPrefix(err.Error(), "connection closed: ") {
		t.Fatalf("错误文案应以 connection closed 开头（契约 §3.4.7），实际 %q", err.Error())
	}
}

// ---------- F3：心跳间隔（契约 §3.4.2 / §11.3） ----------

// TestHeartbeatInterval 钉住 `max(1s, heartbeatSec/2)`，缺省/非正数按 10s 算。
func TestHeartbeatInterval(t *testing.T) {
	cases := []struct {
		sec  int
		want time.Duration
	}{
		{1, time.Second},      // max(1s, 0.5s) → 下限 1s 生效
		{10, 5 * time.Second}, // 就是一半
		{0, 5 * time.Second},  // 缺省 10s → 一半
		{-1, 5 * time.Second}, // 非正数同样按缺省 10s（不是「1s 下限」）
	}
	for _, tc := range cases {
		if got := heartbeatInterval(tc.sec); got != tc.want {
			t.Errorf("heartbeatInterval(%d) = %s，期望 %s（契约 §3.4.2: max(1s, heartbeatSec/2)）",
				tc.sec, got, tc.want)
		}
	}
}

// ---------- F4：ack 立刻（不等信号量，契约 §11.3） ----------

// TestClientAckBeforeSemaphore：taskLimit=1 时连派两单，两条 ack 都必须**早于**
// 首单的 result。ack 是租约开始计时的信号，拖住它会让中枢以为节点没收到
// （契约 §3.4.3）；排队的只能是执行，不能是 ack。
func TestClientAckBeforeSemaphore(t *testing.T) {
	// instances=1 + concurrency 缺省 → taskLimit=1：两单必然串行。
	cfg := w4Config(w4Cap("demo.prep", 1, "sleep 0.3; echo first"))
	h := w4Start(t, cfg, 10, 2*time.Second)

	for _, id := range []string{"tsk_first", "tsk_second"} {
		h.fc.feed(t, &assignFrame{
			Type:       frameTypeAssign,
			TaskID:     id,
			Capability: "demo.prep",
			Version:    1,
		})
	}

	frames := h.fc.waitFor(t, 10*time.Second, "两条 ack 与首单 result", func(frames []map[string]any) bool {
		return w4Has(frames, frameTypeAck, "tsk_first") &&
			w4Has(frames, frameTypeAck, "tsk_second") &&
			w4Has(frames, frameTypeResult, "tsk_first")
	})

	_, ackFirst, _ := w4Find(frames, frameTypeAck, "tsk_first")
	_, ackSecond, _ := w4Find(frames, frameTypeAck, "tsk_second")
	_, resFirst, _ := w4Find(frames, frameTypeResult, "tsk_first")
	if ackFirst > resFirst || ackSecond > resFirst {
		t.Fatalf("ack 排队等了信号量（ack@%d/%d，首单 result@%d）: %s",
			ackFirst, ackSecond, resFirst, w4Describe(frames))
	}
	if ackSecond < ackFirst {
		t.Fatalf("ack 顺序应与派单顺序一致（first@%d, second@%d）", ackFirst, ackSecond)
	}

	// 两单都要真跑完：否则上面的顺序断言可以靠「第二单根本没执行」蒙过去。
	frames = h.fc.waitFor(t, 10*time.Second, "第二单 result", w4Cond(frameTypeResult, "tsk_second"))
	for _, id := range []string{"tsk_first", "tsk_second"} {
		res, _, _ := w4Find(frames, frameTypeResult, id)
		if res["status"] != statusSucceeded {
			t.Fatalf("%s 应 succeeded，实际 %v（error=%v）", id, res["status"], res["error"])
		}
	}

	h.stop()
}

// ---------- 真 WebSocket：F1 回归锚（契约 §11.1） ----------
//
// 下面这组用例**必须**跑在真连接上：F1（SIGINT 一来连接被库关掉 → grace 期的
// result 全丢 → 中枢只能等租约到期重派 → 重复执行）只存在于 coder/websocket 的
// 读语义里（Conn.Read(ctx) 挂 context.AfterFunc(ctx, c.close)）。fake nodeConn
// 的读语义是本文件自己写的，用它测 F1 = 自己证明自己。

// w4Hub 是跑在 httptest.Server 上的假中枢：在一条真 WS 连接上完成
// hello → welcome →（可选）assign → 一直读，收到的上行帧按序记录。
type w4Hub struct {
	srv    *httptest.Server
	hbSec  int          // welcome 里给的 heartbeat_sec
	assign *assignFrame // welcome 之后立刻派这一单（nil = 不派）

	mu      sync.Mutex
	conn    *websocket.Conn
	accepts int
	path    string
	auth    string
	frames  []map[string]any
	at      []time.Time // frames 的同下标到达时刻
	err     error       // hub 侧的故障（第一个）
}

func w4StartHub(t *testing.T, hbSec int, assign *assignFrame) *w4Hub {
	t.Helper()
	h := &w4Hub{hbSec: hbSec, assign: assign}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(func() {
		// 兜底：handler 可能还卡在 Read 上（被测实现没关连接时），先 CloseNow 再
		// srv.Close，否则 httptest 会一直等 handler 返回。
		h.mu.Lock()
		c := h.conn
		h.mu.Unlock()
		if c != nil {
			_ = c.CloseNow()
		}
		h.srv.Close()
	})
	return h
}

// serve 是假中枢的服务端脚本（单条连接）。
func (h *w4Hub) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.accepts++
	h.path = r.URL.Path
	h.auth = r.Header.Get("Authorization")
	h.mu.Unlock()

	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		h.fail(fmt.Sprintf("accept: %v", err))
		return
	}
	h.mu.Lock()
	h.conn = c
	h.mu.Unlock()

	ctx := context.Background()

	// 1) hello 必须是第一帧（契约 §3.4.1）。
	_, raw, err := c.Read(ctx)
	if err != nil {
		h.fail(fmt.Sprintf("读 hello 失败: %v", err))
		return
	}
	frame := w4DecodeHubFrame(h, raw)
	if frame["type"] != frameTypeHello {
		h.fail(fmt.Sprintf("第一帧不是 hello，是 %v", frame["type"]))
		return
	}
	h.push(frame)

	// 2) welcome（心跳设小：真连接上 idle 语义与 fake 不同）。
	if err := w4HubWrite(ctx, c, &welcomeFrame{
		Type:         frameTypeWelcome,
		NodeID:       "nod_ws",
		Protocol:     ws.ProtocolVersion,
		HeartbeatSec: h.hbSec,
		LeaseTTLSec:  30,
	}); err != nil {
		h.fail(fmt.Sprintf("写 welcome 失败: %v", err))
		return
	}

	// 3) 可选派单。
	if h.assign != nil {
		if err := w4HubWrite(ctx, c, h.assign); err != nil {
			h.fail(fmt.Sprintf("写 assign 失败: %v", err))
			return
		}
	}

	// 4) 一直读到连接结束（对端正常关闭也是读错误，不是故障）。
	for {
		_, raw, err := c.Read(ctx)
		if err != nil {
			return
		}
		h.push(w4DecodeHubFrame(h, raw))
	}
}

func (h *w4Hub) push(frame map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.frames = append(h.frames, frame)
	h.at = append(h.at, time.Now())
}

func (h *w4Hub) fail(msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err == nil {
		h.err = errors.New(msg)
	}
}

func (h *w4Hub) snapshot() ([]map[string]any, []time.Time, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	frames := make([]map[string]any, len(h.frames))
	copy(frames, h.frames)
	at := make([]time.Time, len(h.at))
	copy(at, h.at)
	return frames, at, h.err
}

// request 返回握手请求的 path / Authorization（钉住 dialNode 的线上形状）。
func (h *w4Hub) request() (path, auth string, accepts int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.path, h.auth, h.accepts
}

// waitFrame 轮询到对端收到满足 cond 的帧序列（超时报出已收帧与 hub 侧故障）。
func (h *w4Hub) waitFrame(t *testing.T, timeout time.Duration, what string, cond func([]map[string]any) bool) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		frames, _, herr := h.snapshot()
		if cond(frames) {
			return frames
		}
		if herr != nil {
			t.Fatalf("等 %s 时假中枢报错: %v（已收帧: %s）", what, herr, w4Describe(frames))
		}
		if time.Now().After(deadline) {
			t.Fatalf("等 %s 超时（%s）；假中枢已收帧: %s", what, timeout, w4Describe(frames))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// frameAt 返回已收帧里第 i 帧到达对端的时刻（用来证明它是在 cancel **之后**到的）。
func (h *w4Hub) frameAt(i int) time.Time {
	_, at, _ := h.snapshot()
	if i < 0 || i >= len(at) {
		return time.Time{}
	}
	return at[i]
}

// w4DecodeHubFrame 把上行帧解成 map（断言用；tag 属于 internal/ws，不在测试里重写）。
func w4DecodeHubFrame(h *w4Hub, raw []byte) map[string]any {
	var frame map[string]any
	if err := json.Unmarshal(raw, &frame); err != nil {
		h.fail(fmt.Sprintf("上行帧不是 JSON（%q）: %v", raw, err))
		return map[string]any{}
	}
	return frame
}

// w4HubWrite 从假中枢写一帧下行帧。
func w4HubWrite(ctx context.Context, c *websocket.Conn, f any) error {
	data, err := encodeFrame(f)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, data)
}

// w4RealWSConfig 写一份真 node.yaml 并 loadConfig（顺带覆盖 server→streamURL 的映射）。
func w4RealWSConfig(t *testing.T, serverURL, command string) *nodeConfig {
	t.Helper()
	body := fmt.Sprintf(`name: ws-node
server: %s
token: tok_ws
instances: 1
capabilities:
  - capability: demo.prep
    version: 1
    run:
      runner: bash
      command: %q
      timeoutSec: 10
`, serverURL, command)
	path := filepath.Join(t.TempDir(), "node.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写 node.yaml 失败: %v", err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	return cfg
}

// w4ServeRealWS 用本包的 dialNode + serveNode 跑一条真连接（契约 §11.1）。
func w4ServeRealWS(t *testing.T, ctx context.Context, cfg *nodeConfig, grace time.Duration) <-chan error {
	t.Helper()
	url, err := cfg.streamURL()
	if err != nil {
		t.Fatalf("streamURL: %v", err)
	}
	conn, err := dialNode(ctx, url, cfg.Token)
	if err != nil {
		t.Fatalf("dialNode: %v", err)
	}
	// 兜底：用例中途 Fatal 时也要解开连接，否则 serveNode 的读 goroutine 会悬着。
	t.Cleanup(func() { _ = conn.close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	done := make(chan error, 1)
	go func() { done <- serveNode(ctx, cfg, conn, logger, grace) }()
	return done
}

// w4WaitServe 等 serveNode 返回，并断言必须是 nil（正常退出）。
func w4WaitServe(t *testing.T, done <-chan error, timeout time.Duration) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveNode 正常退出必须返回 nil，实际 %v", err)
		}
	case <-time.After(timeout):
		t.Fatalf("serveNode 未在 %s 内返回", timeout)
	}
}

// TestServeNodeRealWSAssignAckResult 是真连接上的正常路径（契约 §11.1）：
// hello → welcome → assign → ack + result（succeeded、summary=ok）。
func TestServeNodeRealWSAssignAckResult(t *testing.T) {
	hub := w4StartHub(t, 1, &assignFrame{
		Type:        frameTypeAssign,
		TaskID:      "tsk_ws",
		Capability:  "demo.prep",
		Version:     1,
		Params:      map[string]any{"title": "ws"},
		LeaseUntil:  time.Now().Add(time.Minute).Unix(),
		MaxDuration: 10,
	})
	// 命令形状与 node.yaml 里的一模一样（sleep 0.3 让「在跑」这个状态真的存在）。
	cfg := w4RealWSConfig(t, hub.srv.URL, "sleep 0.3; echo ok")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := w4ServeRealWS(t, ctx, cfg, 2*time.Second)

	hub.waitFrame(t, 5*time.Second, "hello", w4Cond(frameTypeHello, ""))
	if path, auth, _ := hub.request(); path != "/v1/node/stream" || auth != "Bearer "+cfg.Token {
		t.Fatalf("连线形状不对：path=%q auth=%q（应为 /v1/node/stream + Bearer 令牌）", path, auth)
	}

	frames := hub.waitFrame(t, 10*time.Second, "tsk_ws 的 result", w4Cond(frameTypeResult, "tsk_ws"))
	_, ackIdx, ok := w4Find(frames, frameTypeAck, "tsk_ws")
	if !ok {
		t.Fatalf("真连接上没收到 ack: %s", w4Describe(frames))
	}
	res, resIdx, ok := w4Find(frames, frameTypeResult, "tsk_ws")
	if !ok {
		t.Fatalf("真连接上没收到 result: %s", w4Describe(frames))
	}
	if ackIdx > resIdx {
		t.Fatalf("ack 必须先于 result（ack@%d, result@%d）", ackIdx, resIdx)
	}
	if res["status"] != statusSucceeded {
		t.Fatalf("result.status 应为 %s，实际 %v（error=%v）", statusSucceeded, res["status"], res["error"])
	}
	if res["summary"] != "ok" {
		t.Errorf("result.summary 应为命令输出首行 ok，实际 %#v", res["summary"])
	}

	// 正常收尾：父 ctx 取消（模拟 SIGTERM）后 serveNode 必须返回 nil。
	cancel()
	w4WaitServe(t, done, 15*time.Second)
}

// TestServeNodeRealWSGracefulShutdownKeepsResult 是 **F1 的回归锚**（契约 §11.1）：
// 任务跑完之前 SIGINT（父 ctx 取消，grace=2s），真连接上**必须真的收到** result。
//
// 把 client.go 里的 readLoop(a.base) 改回 readLoop(ctx) 会让这条红：父 ctx 取消
// → 库的 setupReadTimeout 直接 c.close() → drain 期间任务跑完了也发不出 result。
func TestServeNodeRealWSGracefulShutdownKeepsResult(t *testing.T) {
	hub := w4StartHub(t, 1, &assignFrame{
		Type:        frameTypeAssign,
		TaskID:      "tsk_ws_grace",
		Capability:  "demo.prep",
		Version:     1,
		Params:      map[string]any{},
		LeaseUntil:  time.Now().Add(time.Minute).Unix(),
		MaxDuration: 10,
	})
	cfg := w4RealWSConfig(t, hub.srv.URL, "sleep 0.3; echo ok")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := w4ServeRealWS(t, ctx, cfg, 2*time.Second)

	// 等到 ack：证明 assign 真的被读到了，且命令（sleep 0.3）已经在跑还没结束。
	hub.waitFrame(t, 5*time.Second, "tsk_ws_grace 的 ack", w4Cond(frameTypeAck, "tsk_ws_grace"))

	cancelAt := time.Now()
	cancel() // 模拟 SIGINT：任务跑到一半，连接不能跟着死

	// 核心断言：对端**真的收到** result（不是「发了但发不出去」）。
	frames := hub.waitFrame(t, 10*time.Second, "退出期跑完任务的 result",
		w4Cond(frameTypeResult, "tsk_ws_grace"))
	res, resIdx, ok := w4Find(frames, frameTypeResult, "tsk_ws_grace")
	if !ok {
		t.Fatalf("grace 期间跑完的任务没把 result 发到对端: %s", w4Describe(frames))
	}
	if res["status"] != statusSucceeded {
		t.Fatalf("退出期跑完的任务应 succeeded（不该被当成「退出即杀」），实际 %v（error=%v）",
			res["status"], res["error"])
	}
	if res["summary"] != "ok" {
		t.Errorf("result.summary 应为 ok，实际 %#v", res["summary"])
	}
	// 这一帧必须是在取消**之后**才到对端：否则用例没真正覆盖「退出期发送」。
	if got := hub.frameAt(resIdx); got.Before(cancelAt) {
		t.Fatalf("result 在取消之前就到对端了（收到于 %s，取消于 %s），这条用例没测到退出期发送",
			got.Format(time.RFC3339Nano), cancelAt.Format(time.RFC3339Nano))
	}
	// 全程只有一条连接：result 是**同一条**连接上发出来的，不是重连出来的。
	if _, _, accepts := hub.request(); accepts != 1 {
		t.Fatalf("应只有 1 次握手，实际 %d 次（result 必须走原连接）", accepts)
	}

	w4WaitServe(t, done, 15*time.Second)
}
