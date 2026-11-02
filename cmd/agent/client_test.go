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
	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/ws"
)

// 本文件覆盖连接与调度层（client.go）的 v2 行为。
//
// 与 v1 最大的差异：**能力不再是 node.yaml 里的声明，而是缓存里的产物**。所以每个
// 用例的夹具变成「先把产物经 deploy 帧放进来，再派单」——这正是线上的顺序，
// 顺带把 apply.go / cache.go / hello.applied 一起串起来了。
//
// 两条腿走路（与 v1 相同）：
//   - 内存 fake nodeConn（快、确定）：ack 顺序、cancel、信号量、params 透传；
//   - httptest + coder/websocket 起**真**连接（§11.1 的 F1 回归锚）：优雅退出时
//     result 到底能不能发出去。这一条**不能**交给 fake：fake 的读语义是本文件自己
//     写的，而 F1 恰恰死在真库的读语义上。

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
// func() { c.close() })，所以取消读 ctx 是「关连接」，不是「读超时」；这里也立刻
// 返回 ctx.Err()。历史上这里有过一个「取消后还再交付 150ms 内到达的帧」的窗口，
// 那是假的：它把「父 ctx 取消并不影响读」这个错误前提固化成了测试语义，于是 F1
// 在 fake 上永远绿。现在契约要求读循环用连接自己的 ctx（a.base），本 fake 的 ctx
// 也就是 a.base，因此「取消后读不到帧」= 收尾，与真库一致。
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
		state, _ := f["state"].(string)
		part := typ
		if taskID != "" {
			part += ":" + taskID
		}
		if status != "" {
			part += "=" + status
		}
		if state != "" {
			part += "=" + state
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
//
// v2 的 node.yaml 只有 bootstrap 四件——能力不在里面（见 config.go）。
func w4Config() *nodeConfig {
	return &nodeConfig{
		Name:      "test-node",
		Server:    "http://localhost:8080",
		Token:     "test-token",
		Instances: 1,
	}
}

// w4Artifact 造一份最小产物（一个 #!/bin/bash 的 run.sh）。
func w4Artifact(name string, version int, body string) *artifact.Artifact {
	return testScriptArtifact(name, version, bashScript(body))
}

// w4Harness 是一轮 serveNode 的测试上下文。
type w4Harness struct {
	t       *testing.T
	fc      *w4Conn
	cancel  context.CancelFunc
	done    chan error
	dataDir string

	mu  sync.Mutex
	got bool
	err error
}

// w4StartRaw 起 serveNode（不替测试发 welcome），等 hello 发出后返回。
//
// 数据目录是每个 harness 独立的临时目录（产物缓存 + 工作目录都在里面）。
func w4StartRaw(t *testing.T, cfg *nodeConfig, grace time.Duration) *w4Harness {
	t.Helper()
	return w4StartRawAt(t, cfg, t.TempDir(), grace)
}

// w4StartRawAt 与 w4StartRaw 相同，但由调用方指定数据目录
// （用来预置缓存：hello 里必须能报出「本机已缓存」的产物）。
func w4StartRawAt(t *testing.T, cfg *nodeConfig, dataDir string, grace time.Duration) *w4Harness {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	fc := newW4Conn()
	// 日志默认静音：断言失败时要看的是帧序列，不是 slog 输出。
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))

	done := make(chan error, 1)
	go func() { done <- serveNode(ctx, cfg, fc, dataDir, logger, grace) }()

	h := &w4Harness{t: t, fc: fc, cancel: cancel, done: done, dataDir: dataDir}
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
	h.welcome(heartbeatSec)
	return h
}

// welcome 发一帧 welcome（完成握手）。
func (h *w4Harness) welcome(heartbeatSec int) {
	h.t.Helper()
	h.fc.feed(h.t, &welcomeFrame{
		Type:         frameTypeWelcome,
		NodeID:       "nod_test",
		Protocol:     ws.ProtocolVersion,
		HeartbeatSec: heartbeatSec,
		LeaseTTLSec:  30,
	})
}

// deploy 把一份产物经 deploy 帧放进本机缓存（走真实路径：apply.go + cache.go），
// 等 apply 帧回报 applied 之后返回该产物的 digest。
//
// 用例这样写而不是直接往 cache 里塞文件，是因为**派单前的顺序**本身重要：
// 中枢总是先 deploy 再 assign，节点必须在这个顺序下工作。
func (h *w4Harness) deploy(art *artifact.Artifact) string {
	h.t.Helper()
	digest := mustDigest(h.t, art)
	raw, err := artifact.CanonicalJSON(art)
	if err != nil {
		h.t.Fatalf("artifact.CanonicalJSON: %v", err)
	}
	id := fmt.Sprintf("dep_%s_%d", art.Metadata.Name, art.Metadata.Version)
	h.fc.feed(h.t, &deployFrame{
		Type:     frameTypeDeploy,
		DeployID: id,
		Action:   ws.DeployActionApply,
		Artifact: raw,
	})
	frames := h.fc.waitFor(h.t, 3*time.Second, "deploy "+id+" 的 apply", func(frames []map[string]any) bool {
		for _, f := range frames {
			if f["type"] == frameTypeApply && f["deploy_id"] == id {
				return true
			}
		}
		return false
	})
	for _, f := range frames {
		if f["type"] == frameTypeApply && f["deploy_id"] == id {
			if f["state"] != ws.ApplyStateApplied {
				h.t.Fatalf("deploy %s 失败: state=%v reason=%v", id, f["state"], f["reason"])
			}
		}
	}
	return digest
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

// ---------- hello → welcome → 心跳 ----------

func TestClientHelloWelcomeHeartbeat(t *testing.T) {
	cfg := w4Config()
	// heartbeat_sec=1 → 间隔 max(1s, 0.5s) = 1s，测试 4s 内必有一拍。
	h := w4Start(t, cfg, 1, 100*time.Millisecond)

	hello := h.fc.frames()[0]

	if hello["type"] != frameTypeHello {
		t.Fatalf("第一帧必须是 hello，实际 %v", hello["type"])
	}
	if got := hello["protocol"]; got != float64(ws.ProtocolVersion) {
		t.Errorf("hello.protocol 应为 %d，实际 %v", ws.ProtocolVersion, got)
	}
	// v2：没有 capabilities / manifest 两个键，只有 applied（本机缓存了什么）。
	for _, banned := range []string{"capabilities", "manifest"} {
		if _, present := hello[banned]; present {
			t.Errorf("hello 里仍然有 %q 键（v2 已删除）: %#v", banned, hello)
		}
	}
	applied, ok := hello["applied"].([]any)
	if !ok {
		t.Fatalf("hello.applied 必须是数组（空也要是 []），实际 %#v", hello["applied"])
	}
	if len(applied) != 0 {
		t.Errorf("新节点还没有缓存，hello.applied 应为空，实际 %#v", applied)
	}
	instances, ok := hello["instances"].([]any)
	if !ok || len(instances) != cfg.slots() {
		t.Errorf("hello.instances 应为 %d 槽，实际 %#v", cfg.slots(), hello["instances"])
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

// TestClientHelloReportsCachedArtifacts：握手时 hello.applied 必须报出本机已缓存的
// 产物（中枢靠它全量对账 node_capabilities，裁决 D4）。
//
// 预置缓存而不是发 deploy 帧：这正是「agent 重启」的场景——缓存还在磁盘上，
// 连接是新的。中枢只能靠这一帧知道「这台机器还能跑什么」。
func TestClientHelloReportsCachedArtifacts(t *testing.T) {
	dataDir := t.TempDir()
	art := w4Artifact("demo.prep", 4, "echo hi\n")
	digest := mustDigest(t, art)
	if err := newCache(dataDir).put(art, digest); err != nil {
		t.Fatalf("预置缓存: %v", err)
	}

	h := w4StartRawAt(t, w4Config(), dataDir, 100*time.Millisecond)
	hello := h.fc.frames()[0]
	applied, ok := hello["applied"].([]any)
	if !ok || len(applied) != 1 {
		t.Fatalf("hello.applied = %#v, want 1 条（本机缓存的产物）", hello["applied"])
	}
	entry, _ := applied[0].(map[string]any)
	if entry["capability"] != "demo.prep" || entry["version"] != float64(4) || entry["digest"] != digest {
		t.Errorf("hello.applied[0] = %#v, want {demo.prep 4 %s}", entry, digest)
	}

	h.welcome(10)
	h.stop()
}

// TestClientDeployThenHelloReconcile：收到 deploy 之后必须**重发 hello**（带新 applied）。
func TestClientDeployThenHelloReconcile(t *testing.T) {
	h := w4Start(t, w4Config(), 10, 200*time.Millisecond)
	art := w4Artifact("demo.review", 2, "echo review\n")
	digest := h.deploy(art)

	// deploy() 只等到 apply 帧；重发的 hello 在它之后才写出，所以这里显式等它。
	frames := h.fc.waitFor(t, 3*time.Second, "带新 applied 的 hello", func(frames []map[string]any) bool {
		for _, f := range frames {
			if f["type"] != frameTypeHello {
				continue
			}
			if applied, ok := f["applied"].([]any); ok && len(applied) == 1 {
				return true
			}
		}
		return false
	})

	// 顺序：先 apply 再 hello（中枢收到「已应用」之前不该先看到对账帧），
	// 第一次 hello 是空的（握手），最后一次带一条。
	var helloIdx []int
	var applyIdx = -1
	for i, f := range frames {
		switch f["type"] {
		case frameTypeHello:
			helloIdx = append(helloIdx, i)
		case frameTypeApply:
			if applyIdx < 0 {
				applyIdx = i
			}
		}
	}
	if len(helloIdx) != 2 {
		t.Fatalf("hello 帧数 = %d, want 2（握手 + 部署后对账）: %s", len(helloIdx), w4Describe(frames))
	}
	if applied, _ := frames[helloIdx[0]]["applied"].([]any); len(applied) != 0 {
		t.Errorf("第一次 hello.applied = %#v, want 空（部署前）", frames[helloIdx[0]]["applied"])
	}
	if applyIdx < 0 || applyIdx > helloIdx[1] {
		t.Fatalf("顺序不对：apply@%d，重发 hello@%d（应先 apply 后 hello）", applyIdx, helloIdx[1])
	}
	applied, _ := frames[helloIdx[1]]["applied"].([]any)
	entry, _ := applied[0].(map[string]any)
	if entry["digest"] != digest {
		t.Errorf("hello.applied[0].digest = %v, want %v", entry["digest"], digest)
	}

	h.stop()
}

// ---------- assign → ack → result ----------

// TestClientAssignAckThenResult：assign → ack 先于 result，result succeeded。
func TestClientAssignAckThenResult(t *testing.T) {
	h := w4Start(t, w4Config(), 10, 200*time.Millisecond)
	h.deploy(w4Artifact("demo.prep", 1, "echo hello-agent\n"))

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
		t.Errorf("v2 节点不回报出口（产物没有 out 字段），result 帧不能出现 out 键，实际 %#v", res)
	}
	artifacts, ok := res["artifacts"].([]any)
	if !ok || len(artifacts) != 0 {
		t.Errorf("artifacts 必须是 []（不是 null），实际 %#v", res["artifacts"])
	}

	h.stop()
}

// TestClientAssignByDigest：assign 带了 digest 就按 digest 定位产物（哪怕能力名/版本
// 与产物元数据不一致）。中枢据此把一次执行**钉死**在审计过的那一版产物上。
func TestClientAssignByDigest(t *testing.T) {
	h := w4Start(t, w4Config(), 10, 200*time.Millisecond)
	digest := h.deploy(w4Artifact("demo.prep", 1, "echo via-digest\n"))

	h.fc.feed(t, &assignFrame{
		Type:       frameTypeAssign,
		TaskID:     "tsk_digest",
		Capability: "demo.renamed", // 与产物元数据不同：证明走的是 digest 而不是 name@ver
		Version:    9,
		Digest:     digest,
	})

	frames := h.fc.waitFor(t, 5*time.Second, "tsk_digest 的 result", w4Cond(frameTypeResult, "tsk_digest"))
	res, _, _ := w4Find(frames, frameTypeResult, "tsk_digest")
	if res["status"] != statusSucceeded {
		t.Fatalf("status = %v, want succeeded（error=%v）", res["status"], res["error"])
	}
	if res["summary"] != "via-digest" {
		t.Fatalf("summary = %#v, want via-digest", res["summary"])
	}

	h.stop()
}

// TestClientFailedCommand：非 0 退出 → result failed。
func TestClientFailedCommand(t *testing.T) {
	h := w4Start(t, w4Config(), 10, 200*time.Millisecond)
	h.deploy(w4Artifact("demo.prep", 1, "echo boom >&2; exit 7\n"))

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

// TestClientCancel：cancel → result failed（error 含 canceled），进程组被杀。
func TestClientCancel(t *testing.T) {
	// 命令先落一个 marker 文件再睡：marker 出现 = 子进程真的在跑了，
	// 这时才发 cancel，测的才是「杀进程组」而不是「还没起来就被取消」。
	marker := filepath.Join(t.TempDir(), "started")
	h := w4Start(t, w4Config(), 10, 2*time.Second)
	h.deploy(w4Artifact("demo.prep", 1, "touch "+marker+"; sleep 5\n"))

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

// ---------- 优雅退出（等在跑任务 + draining 不 ack） ----------

func TestClientGracefulShutdown(t *testing.T) {
	// 任务要睡得比「取消 ctx」久一点，才真的测到「等任务收尾」而不是等一个空集合。
	h := w4Start(t, w4Config(), 10, 2*time.Second)
	h.deploy(w4Artifact("demo.prep", 1, "sleep 0.3; echo drained\n"))

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

// ---------- 本地没有这份产物 → ack + result failed ----------

// TestClientNoArtifact：派了一个本机没缓存的产物 → 先 ack 再回 failed。
//
// 文案固定为 `no artifact for <cap>@<ver>`：中枢/运维要能一眼区分「节点没接单」
// 与「节点接了但本地没有这份产物」（后者通常是部署还没生效，或缓存被清过）。
func TestClientNoArtifact(t *testing.T) {
	h := w4Start(t, w4Config(), 10, 200*time.Millisecond)
	h.deploy(w4Artifact("demo.prep", 1, "echo prep\n")) // 只部署 demo.prep@1

	h.fc.feed(t, &assignFrame{
		Type:       frameTypeAssign,
		TaskID:     "tsk_noart",
		Capability: "demo.other",
		Version:    2,
	})

	frames := h.fc.waitFor(t, 5*time.Second, "tsk_noart 的 result", w4Cond(frameTypeResult, "tsk_noart"))

	_, ackIdx, ok := w4Find(frames, frameTypeAck, "tsk_noart")
	if !ok {
		t.Fatalf("找不到产物也要先 ack（否则中枢只能等租约）: %s", w4Describe(frames))
	}
	res, resIdx, ok := w4Find(frames, frameTypeResult, "tsk_noart")
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
	if !strings.Contains(errText, "no artifact for demo.other@2") {
		t.Errorf("error 应含 %q，实际 %q", "no artifact for demo.other@2", errText)
	}

	h.stop()
}

// ---------- params 透传 / 每能力信号量限流 ----------

func TestClientPassesParams(t *testing.T) {
	// 参数只走环境变量（不拼命令）：client 的职责是把 assign.Params 原样交给 job。
	h := w4Start(t, w4Config(), 10, 200*time.Millisecond)
	h.deploy(w4Artifact("demo.prep", 1, "printf '%s\\n' \"$ANOTIFY_PARAM_TITLE\"\n"))

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

// TestClientTraceIDAndDigestReachTask：assign.trace_id 与产物 digest 必须进到脚本里
// （契约 §6 的 ANOTIFY_TRACE_ID / ANOTIFY_DIGEST，排障全靠它们把日志串回链路）。
func TestClientTraceIDAndDigestReachTask(t *testing.T) {
	h := w4Start(t, w4Config(), 10, 200*time.Millisecond)
	digest := h.deploy(w4Artifact("demo.prep", 1,
		"printf '%s|%s' \"$ANOTIFY_TRACE_ID\" \"$ANOTIFY_DIGEST\"\n"))

	h.fc.feed(t, &assignFrame{
		Type:       frameTypeAssign,
		TaskID:     "tsk_trace",
		Capability: "demo.prep",
		Version:    1,
		TraceID:    "trc_deadbeef",
	})

	frames := h.fc.waitFor(t, 5*time.Second, "tsk_trace 的 result", w4Cond(frameTypeResult, "tsk_trace"))
	res, _, _ := w4Find(frames, frameTypeResult, "tsk_trace")
	if res["status"] != statusSucceeded {
		t.Fatalf("status = %v, want succeeded（error=%v）", res["status"], res["error"])
	}
	if want := "trc_deadbeef|" + digest; res["summary"] != want {
		t.Fatalf("summary = %#v, want %q（trace_id 与 digest 没进到脚本）", res["summary"], want)
	}

	h.stop()
}

func TestClientSemaphoreLimitsConcurrency(t *testing.T) {
	// instances=1 → taskLimit=1：两个任务必须串行（不能同时 start）。
	trace := filepath.Join(t.TempDir(), "trace")
	h := w4Start(t, w4Config(), 10, 2*time.Second)
	h.deploy(w4Artifact("demo.prep", 1, "echo start >> "+trace+"; sleep 0.3; echo end >> "+trace+"\n"))

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

// ---------- 握手被拒 / 连接断开 ----------

func TestClientHandshakeRejected(t *testing.T) {
	cfg := w4Config()
	h := w4StartRaw(t, cfg, time.Second) // 故意不发 welcome

	h.fc.feed(t, &errorFrame{
		Type:    frameTypeError,
		Code:    ws.ErrCodeTooManyInstances,
		Message: "too many instances",
	})

	err := h.wait(5 * time.Second)
	if err == nil {
		t.Fatalf("welcome 前收到 error 帧必须作为握手失败返回")
	}
	want := "handshake rejected: " + ws.ErrCodeTooManyInstances + ": too many instances"
	if err.Error() != want {
		t.Fatalf("错误文案必须逐字固定（契约 §3.4.1）:\n want %q\n  got %q", want, err.Error())
	}
}

func TestClientConnectionClosed(t *testing.T) {
	cfg := w4Config()
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

// ---------- F3：心跳间隔 ----------

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

// ---------- F4：ack 立刻（不等信号量） ----------

// TestClientAckBeforeSemaphore：taskLimit=1 时连派两单，两条 ack 都必须**早于**
// 首单的 result。ack 是租约开始计时的信号；排队的只能是执行，不能是 ack。
func TestClientAckBeforeSemaphore(t *testing.T) {
	h := w4Start(t, w4Config(), 10, 2*time.Second)
	h.deploy(w4Artifact("demo.prep", 1, "sleep 0.3; echo first\n"))

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
//
// v2 的 node.yaml 只有 bootstrap 四件：产物必须先经 deploy 落到缓存里，
// 所以调用方还要自己预置数据目录（见 w4PreloadedDataDir）。
func w4RealWSConfig(t *testing.T, serverURL string) *nodeConfig {
	t.Helper()
	body := fmt.Sprintf(`name: ws-node
server: %s
token: tok_ws
instances: 1
`, serverURL)
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

// w4PreloadedDataDir 建一个已经缓存了指定产物的数据目录（模拟「部署已生效」）。
func w4PreloadedDataDir(t *testing.T, arts ...*artifact.Artifact) string {
	t.Helper()
	dir := t.TempDir()
	c := newCache(dir)
	for _, a := range arts {
		if err := c.put(a, mustDigest(t, a)); err != nil {
			t.Fatalf("预置缓存 %s@%d: %v", a.Metadata.Name, a.Metadata.Version, err)
		}
	}
	return dir
}

// w4ServeRealWS 用本包的 dialNode + serveNode 跑一条真连接（契约 §11.1）。
func w4ServeRealWS(t *testing.T, ctx context.Context, cfg *nodeConfig, dataDir string, grace time.Duration) <-chan error {
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
	go func() { done <- serveNode(ctx, cfg, conn, dataDir, logger, grace) }()
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
	cfg := w4RealWSConfig(t, hub.srv.URL)
	dataDir := w4PreloadedDataDir(t, w4Artifact("demo.prep", 1, "sleep 0.3; echo ok\n"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := w4ServeRealWS(t, ctx, cfg, dataDir, 2*time.Second)

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
	cfg := w4RealWSConfig(t, hub.srv.URL)
	dataDir := w4PreloadedDataDir(t, w4Artifact("demo.prep", 1, "sleep 0.3; echo ok\n"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := w4ServeRealWS(t, ctx, cfg, dataDir, 2*time.Second)

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
