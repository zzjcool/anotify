package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/ws"
)

// 本文件覆盖契约 §5 的 W4 必测矩阵：用内存 fake nodeConn 驱动 serveNode，
// **不连真 WebSocket、不联网**。
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

// postCancelReadWindow 是「ctx 已取消后还愿意交付已到达帧」的窗口。
//
// 真连接同样会出现「帧已经到了、读的取消还没来得及生效」这种时序；这个窗口把
// 那条路变得可测：draining 期间收到的 assign 必须不 ack（如果没有窗口，读往往直接
// 因为 ctx 取消而返回，那条分支就永远踩不到）。
const postCancelReadWindow = 150 * time.Millisecond

func (c *w4Conn) read(ctx context.Context) ([]byte, error) {
	select {
	case data := <-c.in:
		return c.deliver(data), nil
	case <-c.closed:
		return nil, errors.New("fake conn closed")
	case <-ctx.Done():
		// 已经取消：再给已到达的帧一个窗口。
		select {
		case data := <-c.in:
			return c.deliver(data), nil
		case <-time.After(postCancelReadWindow):
		}
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
		t.Errorf("v1 不回报出口：result 帧不能出现 out 键，实际 %#v", res)
	}
	artifacts, ok := res["artifacts"].([]any)
	if !ok || len(artifacts) != 0 {
		t.Errorf("artifacts 必须是 []（不是 null），实际 %#v", res["artifacts"])
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
