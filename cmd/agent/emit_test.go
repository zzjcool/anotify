package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/ws"
)

// 本文件覆盖本地 emit 服务（emit.go）与它在 client / runner 里的接线。
//
// 验收要点（任务书 ①–⑦）逐条对应：
//
//	①  正常路径：emit → durable → outbox 里有这一条            TestEmitServerDurablePath
//	②  token 错 / socket 不存在 → 对应退出码                    TestEmitRejectToken / TestRunEmitExitCodes
//	③  port 不在白名单 → 拒且原因可读                          TestEmitRejectPort
//	④  data 不满足 expects → 拒且说缺哪个字段                   TestEmitRejectSchemaMissingField
//	⑤  超体积 / 超次数 → 拒                                     TestEmitRejectOversize / TestEmitRejectRateLimit
//	⑥  seq 单调且持久化（重启后不复用）                          TestEmitSeqPersistedAcrossRestart
//	⑦  进程退出后 socket 被删除                                 TestEmitSocketRemovedWithTask
//
// 除了单测，还有一条**真进程**的端到端：TestEmitCLIRealProcess 把 agent 二进制编出来，
// 用真实的 `anotify-agent emit` 打到测试进程里的 socket 服务上。

// ---------- 夹具 ----------

const emitTestToken = "tok_emit_secret"

// emitRecorder 是「中枢那一侧」的 fake：记录 agent 想发出去的 emit 帧。
type emitRecorder struct {
	mu     sync.Mutex
	frames []*emitFrame
	err    error // 非 nil = 发送失败（模拟连接断）
}

func (r *emitRecorder) send(f *emitFrame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	cp := *f
	cp.Data = append(json.RawMessage(nil), f.Data...)
	r.frames = append(r.frames, &cp)
	return nil
}

func (r *emitRecorder) all() []*emitFrame {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*emitFrame, len(r.frames))
	copy(out, r.frames)
	return out
}

// wait 等 recorder 收到满足条件的帧（发送是异步的：durable 不等中枢）。
func (r *emitRecorder) wait(t *testing.T, timeout time.Duration, what string, cond func([]*emitFrame) bool) []*emitFrame {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		frames := r.all()
		if cond(frames) {
			return frames
		}
		if time.Now().After(deadline) {
			t.Fatalf("等 %s 超时（%s）：已收到 %d 帧", what, timeout, len(frames))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// defaultEmitOptions 是一份能跑的默认配置（每个测试自己改需要的字段）。
func defaultEmitOptions(t *testing.T, dataDir string) emitServerOptions {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return emitServerOptions{
		DataDir: dataDir,
		TaskID:  "tsk_emit",
		Token:   emitTestToken,
		Ports:   []string{"fix", "note"},
		Ctx:     ctx,
		Log:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

// startEmitServer 起一个 emit 服务（recorder 当上游），测试结束自动关闭。
func startEmitServer(t *testing.T, opt emitServerOptions) (*emitServer, *emitRecorder) {
	t.Helper()
	rec := &emitRecorder{}
	if opt.Send == nil {
		opt.Send = rec.send
	}
	srv, err := newEmitServer(opt)
	if err != nil {
		t.Fatalf("newEmitServer: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv, rec
}

// emitCall 走真 socket 发一条请求并读一条响应（返回 error 而不是 Fatal：
// --wait-routed 的用例要能自己控制超时）。
func emitCall(sockPath string, req emitRequest, timeout time.Duration) (emitResponse, error) {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return emitResponse{}, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	line, err := json.Marshal(req)
	if err != nil {
		return emitResponse{}, fmt.Errorf("marshal: %w", err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return emitResponse{}, fmt.Errorf("write: %w", err)
	}
	var resp emitResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return emitResponse{}, fmt.Errorf("decode: %w", err)
	}
	return resp, nil
}

// mustEmit 是 emitCall 的「必须成功返回」版本。
func mustEmit(t *testing.T, sockPath string, req emitRequest) emitResponse {
	t.Helper()
	resp, err := emitCall(sockPath, req, 5*time.Second)
	if err != nil {
		t.Fatalf("emitCall(%+v): %v", req, err)
	}
	return resp
}

// outboxRecords 读一遍 outbox 文件（排障用：验收要看「文件里确实有这一条」）。
func outboxRecords(t *testing.T, path string) []outboxRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 outbox %s: %v", path, err)
	}
	var recs []outboxRecord
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec outboxRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("outbox 行不是合法 JSON（%q）: %v", line, err)
		}
		recs = append(recs, rec)
	}
	return recs
}

// ---------- ① 正常路径 ----------

// TestEmitServerDurablePath：起 socket → 客户端 emit → durable → outbox 里有这一条。
func TestEmitServerDurablePath(t *testing.T) {
	srv, rec := startEmitServer(t, defaultEmitOptions(t, t.TempDir()))

	resp := mustEmit(t, srv.Path(), emitRequest{
		Op:    emitOpName,
		Token: emitTestToken,
		Port:  "fix",
		Data:  json.RawMessage(`{"issue":"#42","title":"空指针"}`),
		Note:  "第 2 项",
	})

	if !resp.OK || resp.Acked != emitAckedDurable {
		t.Fatalf("emit 应答应该 ok/durable，实际 %+v", resp)
	}
	if resp.Seq != 1 {
		t.Fatalf("第一个 seq 应当是 1，实际 %d", resp.Seq)
	}

	// outbox：先落盘再 ack —— 所以应答回来时文件里必须已经有这一条。
	recs := outboxRecords(t, srv.OutboxPath())
	if len(recs) != 1 {
		t.Fatalf("outbox 应当有 1 条记录，实际 %d 条：%+v", len(recs), recs)
	}
	got := recs[0]
	if got.Seq != 1 || got.Port != "fix" || got.Note != "第 2 项" || got.Ack != nil {
		t.Fatalf("outbox 记录不对：%+v", got)
	}
	if string(got.Data) != `{"issue":"#42","title":"空指针"}` {
		t.Fatalf("outbox data = %s", got.Data)
	}

	// 发往中枢的那一帧：type/task_id/seq/port/data 都要对（线格式见 internal/ws）。
	frames := rec.wait(t, 10*time.Second, "emit 帧发往中枢", func(fs []*emitFrame) bool { return len(fs) == 1 })
	f := frames[0]
	if f.Type != frameTypeEmit || f.TaskID != "tsk_emit" || f.Seq != 1 || f.Port != "fix" {
		t.Fatalf("上行 emit 帧不对：%+v", f)
	}
	if string(f.Data) != `{"issue":"#42","title":"空指针"}` {
		t.Fatalf("上行 emit.data = %s", f.Data)
	}

	// 同一个 socket 支持多条连接：第二条 emit 的 seq 必须递增。
	resp2 := mustEmit(t, srv.Path(), emitRequest{Op: emitOpName, Token: emitTestToken, Port: "note"})
	if !resp2.OK || resp2.Seq != 2 {
		t.Fatalf("第二条 emit 应答不对（want seq=2）：%+v", resp2)
	}
	// data 缺省 = {}
	if got := string(outboxRecords(t, srv.OutboxPath())[1].Data); got != "{}" {
		t.Fatalf("data 缺省必须归一成 {}，实际 %q", got)
	}
}

// TestEmitSocketPermissions：socket 0600、目录 0700（目录可写就能替换 socket 文件）。
func TestEmitSocketPermissions(t *testing.T) {
	dataDir := t.TempDir()
	srv, _ := startEmitServer(t, defaultEmitOptions(t, dataDir))

	st, err := os.Stat(srv.Path())
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := st.Mode().Perm(); perm != emitFileMode {
		t.Errorf("socket 权限 = %04o, want %04o", perm, emitFileMode)
	}
	dirSt, err := os.Stat(filepath.Join(dataDir, emitSocketDirName))
	if err != nil {
		t.Fatalf("stat socket 目录: %v", err)
	}
	if perm := dirSt.Mode().Perm(); perm != emitDirMode {
		t.Errorf("socket 目录权限 = %04o, want %04o", perm, emitDirMode)
	}
}

// ---------- ② token ----------

func TestEmitRejectToken(t *testing.T) {
	srv, rec := startEmitServer(t, defaultEmitOptions(t, t.TempDir()))

	resp := mustEmit(t, srv.Path(), emitRequest{
		Op:    emitOpName,
		Token: "tok_wrong",
		Port:  "fix",
	})
	if resp.OK || resp.Rejected != emitRejectToken {
		t.Fatalf("错 token 应当被拒（rejected=token），实际 %+v", resp)
	}
	if !strings.Contains(resp.Error, "token") {
		t.Errorf("拒绝原因应当能读懂（含 token），实际 %q", resp.Error)
	}
	if resp.Acked != "" {
		t.Errorf("被拒时 acked 必须为空，实际 %q", resp.Acked)
	}
	// 被拒的 emit 不能进 outbox、更不能发往中枢。
	if recs := readOutbox(t, srv.OutboxPath()); len(recs) != 0 {
		t.Errorf("被拒的 emit 不该落盘，outbox 里有 %d 条", len(recs))
	}
	if frames := rec.all(); len(frames) != 0 {
		t.Errorf("被拒的 emit 不该发往中枢，实际 %d 帧", len(frames))
	}
}

// readOutbox 是「读出来看看有几条」的宽容版本（文件不存在 = 0 条）。
func readOutbox(t *testing.T, path string) []outboxRecord {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	return outboxRecords(t, path)
}

// TestEmitTokenEmptyMeansNoAuth：中枢没下发令牌时不鉴权（旧中枢的退化），但 emit 仍要能跑通。
func TestEmitTokenEmptyMeansNoAuth(t *testing.T) {
	opt := defaultEmitOptions(t, t.TempDir())
	opt.Token = ""
	srv, _ := startEmitServer(t, opt)

	resp := mustEmit(t, srv.Path(), emitRequest{Op: emitOpName, Port: "fix"})
	if !resp.OK {
		t.Fatalf("没有令牌时不该被拒：%+v", resp)
	}
}

// ---------- ③ port 白名单 ----------

func TestEmitRejectPort(t *testing.T) {
	srv, _ := startEmitServer(t, defaultEmitOptions(t, t.TempDir()))

	resp := mustEmit(t, srv.Path(), emitRequest{
		Op:    emitOpName,
		Token: emitTestToken,
		Port:  "nope",
		Data:  json.RawMessage(`{}`),
	})
	if resp.OK || resp.Rejected != emitRejectPort {
		t.Fatalf("白名单外的 port 应当被拒（rejected=port），实际 %+v", resp)
	}
	// 原因必须可读：哪个 port 不行 + 本次允许哪些（实现据此主动改，而不是被拒了再猜）。
	if !strings.Contains(resp.Error, `"nope"`) || !strings.Contains(resp.Error, "[fix note]") {
		t.Errorf("拒绝原因应当含越界的 port 与白名单，实际 %q", resp.Error)
	}

	// 空 port：也是 port 类拒绝。
	resp = mustEmit(t, srv.Path(), emitRequest{Op: emitOpName, Token: emitTestToken, Port: " "})
	if resp.OK || resp.Rejected != emitRejectPort {
		t.Fatalf("空 port 应当被拒（rejected=port），实际 %+v", resp)
	}
}

// ---------- ④ schema ----------

// TestEmitRejectSchemaMissingField：expects[port].schema 的必填字段缺失 → 拒，且**说清缺哪个字段**。
func TestEmitRejectSchemaMissingField(t *testing.T) {
	opt := defaultEmitOptions(t, t.TempDir())
	opt.Expects = map[string]ws.PortExpect{
		"fix": {Schema: json.RawMessage(`{"type":"object","required":["issue","title"],` +
			`"properties":{"issue":{"type":"string"},"title":{"type":"string"}}}`)},
	}
	srv, _ := startEmitServer(t, opt)

	resp := mustEmit(t, srv.Path(), emitRequest{
		Op:    emitOpName,
		Token: emitTestToken,
		Port:  "fix",
		Data:  json.RawMessage(`{"issue":"#42"}`),
	})
	if resp.OK || resp.Rejected != emitRejectSchema {
		t.Fatalf("缺必填字段应当被拒（rejected=schema），实际 %+v", resp)
	}
	if !strings.Contains(resp.Error, `missing required field "title"`) {
		t.Fatalf("拒绝原因必须指出缺哪个字段（title），实际 %q", resp.Error)
	}

	// 补齐之后同一次运行可以重发（exit 4 的语义）。
	resp = mustEmit(t, srv.Path(), emitRequest{
		Op:    emitOpName,
		Token: emitTestToken,
		Port:  "fix",
		Data:  json.RawMessage(`{"issue":"#42","title":"空指针"}`),
	})
	if !resp.OK || resp.Seq != 1 {
		t.Fatalf("补齐字段后应当成功且 seq 从 1 开始（被拒的不占号），实际 %+v", resp)
	}

	// 类型不对也要说清期望什么（schema.Value 的报错：want type …）。
	resp = mustEmit(t, srv.Path(), emitRequest{
		Op:    emitOpName,
		Token: emitTestToken,
		Port:  "fix",
		Data:  json.RawMessage(`{"issue":42,"title":"x"}`),
	})
	if resp.OK || !strings.Contains(resp.Error, "want type string") {
		t.Fatalf("类型不对时应当说清期望类型，实际 %+v", resp)
	}
}

// TestEmitRejectDataNotObject：data 不是 JSON 对象（§6 第 4 条，rejected=schema / exit 2）。
func TestEmitRejectDataNotObject(t *testing.T) {
	srv, _ := startEmitServer(t, defaultEmitOptions(t, t.TempDir()))

	for _, raw := range []string{`[1,2]`, `"just a string"`, `42`} {
		resp := mustEmit(t, srv.Path(), emitRequest{
			Op:    emitOpName,
			Token: emitTestToken,
			Port:  "fix",
			Data:  json.RawMessage(raw),
		})
		if resp.OK || resp.Rejected != emitRejectSchema {
			t.Fatalf("data=%s 应当被拒（rejected=schema），实际 %+v", raw, resp)
		}
		if !strings.Contains(resp.Error, "JSON 对象") {
			t.Errorf("data=%s 的拒绝原因应当说明要 JSON 对象，实际 %q", raw, resp.Error)
		}
	}
}

// ---------- ⑤ 体积 / 次数 ----------

func TestEmitRejectOversize(t *testing.T) {
	srv, _ := startEmitServer(t, defaultEmitOptions(t, t.TempDir()))

	// 64 KiB 上限：塞一个刚好越界的字段。
	big := strings.Repeat("a", emitMaxDataBytes+16)
	resp := mustEmit(t, srv.Path(), emitRequest{
		Op:    emitOpName,
		Token: emitTestToken,
		Port:  "fix",
		Data:  json.RawMessage(fmt.Sprintf(`{"blob":%q}`, big)),
	})
	if resp.OK || resp.Rejected != emitRejectSize {
		t.Fatalf("超体积应当被拒（rejected=size），实际 %+v", resp)
	}
	if !strings.Contains(resp.Error, "超过上限") {
		t.Errorf("拒绝原因应当说明超上限，实际 %q", resp.Error)
	}

	// 刚好在上限内要放行（边界不是「大概」）。
	ok := strings.Repeat("a", emitMaxDataBytes-16)
	resp = mustEmit(t, srv.Path(), emitRequest{
		Op:    emitOpName,
		Token: emitTestToken,
		Port:  "fix",
		Data:  json.RawMessage(fmt.Sprintf(`{"blob":%q}`, ok)),
	})
	if !resp.OK {
		t.Fatalf("上限内的 data 不该被拒：%+v", resp)
	}
}

// TestEmitRejectRateLimit：本任务累计 emit 达到 1000 次后一律拒（含跨重启的累计）。
//
// 预置 1000 条 outbox 记录（而不是真跑 1000 次 fsync）：测的是**计数器**，
// 不是磁盘速度。
func TestEmitRejectRateLimit(t *testing.T) {
	dataDir := t.TempDir()
	outboxDir := filepath.Join(dataDir, emitOutboxDirName)
	if err := os.MkdirAll(outboxDir, emitDirMode); err != nil {
		t.Fatalf("建 outbox 目录: %v", err)
	}
	var sb strings.Builder
	for i := 1; i <= emitMaxCount; i++ {
		sb.WriteString(fmt.Sprintf("{\"seq\":%d,\"port\":\"fix\",\"data\":{},\"ts\":1}\n", i))
	}
	if err := os.WriteFile(filepath.Join(outboxDir, "tsk_emit.jsonl"), []byte(sb.String()), emitFileMode); err != nil {
		t.Fatalf("预置 outbox: %v", err)
	}

	srv, _ := startEmitServer(t, defaultEmitOptions(t, dataDir))
	resp := mustEmit(t, srv.Path(), emitRequest{Op: emitOpName, Token: emitTestToken, Port: "fix"})
	if resp.OK || resp.Rejected != emitRejectRate {
		t.Fatalf("超次数应当被拒（rejected=rate），实际 %+v", resp)
	}
	if !strings.Contains(resp.Error, fmt.Sprintf("%d", emitMaxCount)) {
		t.Errorf("拒绝原因应当带上限数字，实际 %q", resp.Error)
	}
}

// ---------- ⑥ seq 单调 + 持久化 ----------

// TestEmitSeqPersistedAcrossRestart：重启后不复用已用 seq（中枢按 (task_id, seq) 去重，
// 复用会变成 "seq reused with different payload"）。
func TestEmitSeqPersistedAcrossRestart(t *testing.T) {
	dataDir := t.TempDir()

	opt := defaultEmitOptions(t, dataDir)
	srv1, rec1 := startEmitServer(t, opt)
	// 三条：第 2 条故意让上游“断线”，只验证本地 seq 与落盘。
	if r := mustEmit(t, srv1.Path(), emitRequest{Op: emitOpName, Token: emitTestToken, Port: "fix",
		Data: json.RawMessage(`{"n":1}`)}); r.Seq != 1 {
		t.Fatalf("seq = %d, want 1", r.Seq)
	}
	if r := mustEmit(t, srv1.Path(), emitRequest{Op: emitOpName, Token: emitTestToken, Port: "fix",
		Data: json.RawMessage(`{"n":2}`)}); r.Seq != 2 {
		t.Fatalf("seq = %d, want 2", r.Seq)
	}
	rec1.wait(t, 10*time.Second, "两条 emit 发往中枢", func(fs []*emitFrame) bool { return len(fs) == 2 })
	srv1.Close()

	// 重启：同一个 data-dir / task_id。
	srv2, rec2 := startEmitServer(t, defaultEmitOptions(t, dataDir))
	resp := mustEmit(t, srv2.Path(), emitRequest{Op: emitOpName, Token: emitTestToken, Port: "fix"})
	if !resp.OK {
		t.Fatalf("重启后的 emit 应当成功：%+v", resp)
	}
	if resp.Seq != 3 {
		t.Fatalf("重启后 seq 应当是 3（不复用 1/2），实际 %d", resp.Seq)
	}

	// 补发：重启后应把 1、2 按 seq 顺序重发（内容一致 → 中枢幂等）。
	frames := rec2.wait(t, 10*time.Second, "补发 + 新 emit", func(fs []*emitFrame) bool { return len(fs) >= 3 })
	if len(frames) != 3 {
		t.Fatalf("重启后应当补发 2 条 + 新增 1 条，实际 %d 帧：%+v", len(frames), frames)
	}
	for i, f := range frames {
		if f.Seq != int64(i+1) {
			t.Fatalf("补发必须按 seq 升序（第 %d 帧 seq=%d）", i, f.Seq)
		}
	}
	if string(frames[0].Data) != `{"n":1}` || string(frames[1].Data) != `{"n":2}` {
		t.Fatalf("补发内容必须与原来一致：%s / %s", frames[0].Data, frames[1].Data)
	}
}

// TestEmitAckStopsResend：中枢 ack 过的条目不再补发；被拒的条目**也不再重发**（不无限重试）。
func TestEmitAckStopsResend(t *testing.T) {
	dataDir := t.TempDir()
	opt := defaultEmitOptions(t, dataDir)
	srv1, _ := startEmitServer(t, opt)
	mustEmit(t, srv1.Path(), emitRequest{Op: emitOpName, Token: emitTestToken, Port: "fix"})
	mustEmit(t, srv1.Path(), emitRequest{Op: emitOpName, Token: emitTestToken, Port: "fix"})
	srv1.acknowledge(&emitAckFrame{Type: frameTypeEmitAck, TaskID: "tsk_emit", Seq: 1, Routed: 2})
	srv1.acknowledge(&emitAckFrame{Type: frameTypeEmitAck, TaskID: "tsk_emit", Seq: 2, Error: "bad payload"})
	srv1.Close()

	srv2, rec2 := startEmitServer(t, defaultEmitOptions(t, dataDir))
	// 都不该补发：seq 1 成功过，seq 2 被中枢明确拒收过。
	time.Sleep(100 * time.Millisecond)
	if frames := rec2.all(); len(frames) != 0 {
		t.Fatalf("已 ack / 已拒收的条目不应当补发，实际 %d 帧：%+v", len(frames), frames)
	}
	// 新 emit 从 3 开始（seq 2 也不能复用）。
	if r := mustEmit(t, srv2.Path(), emitRequest{Op: emitOpName, Token: emitTestToken, Port: "fix"}); r.Seq != 3 {
		t.Fatalf("seq = %d, want 3", r.Seq)
	}
	rec2.wait(t, 10*time.Second, "新 emit 发往中枢", func(fs []*emitFrame) bool { return len(fs) == 1 })
}

// ---------- --wait-routed ----------

// TestEmitWaitRouted：--wait-routed 等中枢 emit_ack 再回 routed。
func TestEmitWaitRouted(t *testing.T) {
	srv, rec := startEmitServer(t, defaultEmitOptions(t, t.TempDir()))

	type result struct {
		resp emitResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := emitCall(srv.Path(), emitRequest{
			Op:    emitOpName,
			Token: emitTestToken,
			Port:  "fix",
			Data:  json.RawMessage(`{"issue":"#42"}`),
			Wait:  emitWaitRoutedName,
		}, 10*time.Second)
		done <- result{resp, err}
	}()

	// agent 先把 emit 发给中枢，然后等 emit_ack。
	frames := rec.wait(t, 10*time.Second, "emit 发往中枢", func(fs []*emitFrame) bool { return len(fs) == 1 })
	srv.acknowledge(&emitAckFrame{Type: frameTypeEmitAck, TaskID: "tsk_emit", Seq: frames[0].Seq, Routed: 2})

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("emitCall: %v", r.err)
		}
		if !r.resp.OK || r.resp.Acked != emitAckedRouted {
			t.Fatalf("--wait-routed 应当回 ok/routed，实际 %+v", r.resp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("--wait-routed 没有被 emit_ack 唤醒")
	}

	// outbox 里应当留下 ack 记录（重启后据此不重发）。
	recs := outboxRecords(t, srv.OutboxPath())
	if len(recs) != 2 || recs[1].Ack == nil || recs[1].Ack.Routed != 2 {
		t.Fatalf("outbox 应当有 emit + ack 两条记录，实际 %+v", recs)
	}
}

// TestEmitWaitRoutedHubRejects：中枢回 error 非空的 emit_ack → 把错误回给调用方，且不再重发。
func TestEmitWaitRoutedHubRejects(t *testing.T) {
	srv, rec := startEmitServer(t, defaultEmitOptions(t, t.TempDir()))

	type result struct {
		resp emitResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := emitCall(srv.Path(), emitRequest{
			Op:    emitOpName,
			Token: emitTestToken,
			Port:  "fix",
			Wait:  emitWaitRoutedName,
		}, 10*time.Second)
		done <- result{resp, err}
	}()

	frames := rec.wait(t, 10*time.Second, "emit 发往中枢", func(fs []*emitFrame) bool { return len(fs) == 1 })
	srv.acknowledge(&emitAckFrame{
		Type: frameTypeEmitAck, TaskID: "tsk_emit", Seq: frames[0].Seq,
		Error: "seq reused with different payload",
	})

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("emitCall: %v", r.err)
		}
		if r.resp.OK {
			t.Fatalf("中枢拒收时不能回 ok：%+v", r.resp)
		}
		if !strings.Contains(r.resp.Error, "seq reused with different payload") {
			t.Fatalf("错误必须原样带回给调用方，实际 %q", r.resp.Error)
		}
		if r.resp.Acked != emitAckedDurable {
			t.Errorf("拒收时 emit 确实已落盘，acked 应当是 durable，实际 %q", r.resp.Acked)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("中枢拒收没有唤醒等待者")
	}

	// 「不要无限重试」：拒收之后不再补发这个 seq。
	time.Sleep(50 * time.Millisecond)
	if got := rec.all(); len(got) != 1 {
		t.Fatalf("被拒的 seq 不应当重发，实际发了 %d 次", len(got))
	}
}

// TestEmitGapRequeuesEarlier：中枢回 seq gap → 把更小的未确认条目重新排队补发（§4）。
func TestEmitGapRequeuesEarlier(t *testing.T) {
	srv, rec := startEmitServer(t, defaultEmitOptions(t, t.TempDir()))
	// 上游先“断线”：两条 emit 都进 outbox 但发不出去。
	rec.mu.Lock()
	rec.err = fmt.Errorf("connection closed")
	rec.mu.Unlock()
	mustEmit(t, srv.Path(), emitRequest{Op: emitOpName, Token: emitTestToken, Port: "fix"})
	mustEmit(t, srv.Path(), emitRequest{Op: emitOpName, Token: emitTestToken, Port: "fix"})

	// 连接恢复后，中枢对 seq 2 报缺口 → 应该把 seq 1 和 2 都补上（升序）。
	rec.mu.Lock()
	rec.err = nil
	rec.mu.Unlock()
	srv.acknowledge(&emitAckFrame{
		Type: frameTypeEmitAck, TaskID: "tsk_emit", Seq: 2, Error: "seq gap (missing 1)",
	})
	srv.wakeFlush()

	frames := rec.wait(t, 10*time.Second, "缺口补齐", func(fs []*emitFrame) bool { return len(fs) >= 2 })
	// seq 2 被中枢明确拒收过 → 只补 1（拒收的那个不重发），补发后 1 又会被中枢确认。
	var seqs []int64
	for _, f := range frames {
		seqs = append(seqs, f.Seq)
	}
	if len(seqs) == 0 || seqs[0] != 1 {
		t.Fatalf("补发应当从缺的那个 seq(1) 开始，实际 %v", seqs)
	}
}

// ---------- 任务运行态 ----------

// TestEmitRejectsAfterTaskCancel：任务 ctx 一取消（cancel 帧 / 收尾），emit 立即被拒（exit 5）。
func TestEmitRejectsAfterTaskCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	opt := defaultEmitOptions(t, t.TempDir())
	opt.Ctx = ctx
	srv, _ := startEmitServer(t, opt)

	cancel()
	// watchCtx 会异步收口 socket；这里循环等到「要么连不上，要么被拒（task）」。
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := emitCall(srv.Path(), emitRequest{Op: emitOpName, Token: emitTestToken, Port: "fix"}, time.Second)
		if err != nil {
			// 取消后 socket 正在收口，两种传输层结果都合法：
			//   dial 失败（socket 已删）= §8 的「晚到的 emit → exit 6」
			//   connection reset（监听已关、连接在建/在读中间态）
			// 唯一不能接受的是**被接受**——那由下面 resp 分支判。
			// 曾经这里只认 "dial"，把正常收口时序判成失败（随机红）。
			return
		}
		if resp.OK || resp.Rejected != emitRejectTask {
			t.Fatalf("取消后 emit 必须被拒（rejected=task），实际 %+v", resp)
		}
		if time.Now().After(deadline) {
			t.Fatalf("取消后 socket 一直没关：%+v", resp)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---------- ⑦ socket 生命周期 ----------

// TestEmitSocketRemovedOnClose：Close 之后 socket 文件必须消失（否则同一个任务重跑会撞 EADDRINUSE）。
func TestEmitSocketRemovedOnClose(t *testing.T) {
	dataDir := t.TempDir()
	opt := defaultEmitOptions(t, dataDir)
	srv, _ := startEmitServer(t, opt)

	sockPath := srv.Path()
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("socket 应当存在: %v", err)
	}
	srv.Close()
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Fatalf("Close 之后 socket 文件必须被删除，stat 返回 %v", err)
	}
	// Close 幂等。
	srv.Close()
	// 同一个任务重跑：能重新 Listen（旧文件已清）。
	srv2, _ := startEmitServer(t, defaultEmitOptions(t, dataDir))
	if _, err := os.Stat(srv2.Path()); err != nil {
		t.Fatalf("重跑后 socket 应当重新出现: %v", err)
	}
}

// TestEmitSocketRemovedWithTask 是 ⑦ 的**整条任务路径**版本：真起一个能力进程，
// 进程退出后 socket 必须被删（且运行期间确实存在、能 emit）。
func TestEmitSocketRemovedWithTask(t *testing.T) {
	h := w4Start(t, w4Config(), 10, 200*time.Millisecond)

	// 脚本：等到 allow-exit 出现再退出 —— 让测试能确定地「在进程还活着的时候 emit」。
	// `started` 是给测试的握手：任务工作目录由 agent 在**起进程前**清空重建，
	// 测试不等这个标记就写 allow-exit，会被那次 RemoveAll 抹掉（随机超时）。
	script := `
if [ -S "$ANOTIFY_EMIT_SOCK" ]; then echo "sock=present"; else echo "sock=missing"; fi
: > "$ANOTIFY_TASKDIR/started"
until [ -f "$ANOTIFY_TASKDIR/allow-exit" ]; do sleep 0.01; done
echo bye
`
	h.deploy(w4Artifact("demo.emit", 1, script))
	h.fc.feed(t, &assignFrame{
		Type:        frameTypeAssign,
		TaskID:      "tsk_emit_task",
		Capability:  "demo.emit",
		Version:     1,
		Params:      map[string]any{},
		LeaseUntil:  time.Now().Add(time.Minute).Unix(),
		MaxDuration: 30,
		Ports:       []string{"fix"},
		Token:       emitTestToken,
	})

	sockPath := filepath.Join(h.dataDir, emitSocketDirName, "tsk_emit_task"+emitSockSuffix)
	w4WaitFile(t, sockPath, 5*time.Second)

	// 运行期间：socket 在，emit 能成。
	resp := mustEmit(t, sockPath, emitRequest{Op: emitOpName, Token: emitTestToken, Port: "fix",
		Data: json.RawMessage(`{"issue":"#42"}`)})
	if !resp.OK || resp.Acked != emitAckedDurable {
		t.Fatalf("任务运行期间的 emit 应当成功：%+v", resp)
	}
	sent := h.fc.waitFor(t, 10*time.Second, "上行 emit 帧", w4Cond(frameTypeEmit, "tsk_emit_task"))
	if _, _, ok := w4Find(sent, frameTypeEmit, "tsk_emit_task"); !ok {
		t.Fatalf("没收到上行 emit 帧：%s", w4Describe(sent))
	}

	// 让脚本退出 → 任务结束 → socket 必须消失。
	w4WaitFile(t, filepath.Join(h.dataDir, "work", "tsk_emit_task", "started"), 5*time.Second)
	if err := os.WriteFile(filepath.Join(h.dataDir, "work", "tsk_emit_task", "allow-exit"), nil, 0o644); err != nil {
		t.Fatalf("放行脚本: %v", err)
	}
	frames := h.fc.waitFor(t, 10*time.Second, "任务 result", w4Cond(frameTypeResult, "tsk_emit_task"))
	res, _, _ := w4Find(frames, frameTypeResult, "tsk_emit_task")
	if res["status"] != statusSucceeded {
		t.Fatalf("任务应当成功（能力进程与 socket 共存、退出后干净收尾），实际 %v（error=%v）",
			res["status"], res["error"])
	}
	if summary, _ := res["summary"].(string); !strings.Contains(summary, "sock=present") {
		// 脚本启动那一刻 socket 就该在：这证明「先 Listen 再起进程」这个顺序真的生效。
		t.Fatalf("能力进程启动时 socket 不在，summary=%q", summary)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(sockPath); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("能力进程退出后 socket 仍然存在：%s", sockPath)
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.stop()
}

// ---------- client 侧接线：emit → 上行帧 → emit_ack → 唤醒 ----------

// TestClientEmitSocketAndAck：整条链路（task goroutine 起 socket → CLI 进来 → 上行 emit →
// 中枢 emit_ack → --wait-routed 拿到 routed）。
func TestClientEmitSocketAndAck(t *testing.T) {
	h := w4Start(t, w4Config(), 10, 200*time.Millisecond)

	script := `
if [ -S "$ANOTIFY_EMIT_SOCK" ] && [ -n "$ANOTIFY_TASK_TOKEN" ] && [ -n "$ANOTIFY_TASK_ID" ]; then
  echo "env=ok"
else
  echo "env=missing"
fi
: > "$ANOTIFY_TASKDIR/started"
until [ -f "$ANOTIFY_TASKDIR/allow-exit" ]; do sleep 0.01; done
`
	h.deploy(w4Artifact("demo.emit2", 1, script))
	h.fc.feed(t, &assignFrame{
		Type:        frameTypeAssign,
		TaskID:      "tsk_emit_ack",
		Capability:  "demo.emit2",
		Version:     1,
		LeaseUntil:  time.Now().Add(time.Minute).Unix(),
		MaxDuration: 30,
		Ports:       []string{"fix"},
		Expects: map[string]ws.PortExpect{
			"fix": {Schema: json.RawMessage(`{"type":"object","required":["issue"]}`)},
		},
		Token: emitTestToken,
	})

	sockPath := filepath.Join(h.dataDir, emitSocketDirName, "tsk_emit_ack"+emitSockSuffix)
	w4WaitFile(t, sockPath, 5*time.Second)

	type result struct {
		resp emitResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := emitCall(sockPath, emitRequest{
			Op:    emitOpName,
			Token: emitTestToken,
			Port:  "fix",
			Data:  json.RawMessage(`{"issue":"#42"}`),
			Wait:  emitWaitRoutedName,
		}, 10*time.Second)
		done <- result{resp, err}
	}()

	frames := h.fc.waitFor(t, 5*time.Second, "上行 emit 帧", w4Cond(frameTypeEmit, "tsk_emit_ack"))
	frame, _, _ := w4Find(frames, frameTypeEmit, "tsk_emit_ack")
	seq := int64(frame["seq"].(float64))
	h.fc.feed(t, &emitAckFrame{Type: frameTypeEmitAck, TaskID: "tsk_emit_ack", Seq: seq, Routed: 1})

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("emitCall: %v", r.err)
		}
		if !r.resp.OK || r.resp.Acked != emitAckedRouted {
			t.Fatalf("emit_ack 之后 --wait-routed 应当拿到 routed，实际 %+v", r.resp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("--wait-routed 没被 emit_ack 唤醒（下行帧是不是没认出 emit_ack？）")
	}

	w4WaitFile(t, filepath.Join(h.dataDir, "work", "tsk_emit_ack", "started"), 5*time.Second)
	if err := os.WriteFile(filepath.Join(h.dataDir, "work", "tsk_emit_ack", "allow-exit"), nil, 0o644); err != nil {
		t.Fatalf("放行脚本: %v", err)
	}
	frames = h.fc.waitFor(t, 10*time.Second, "任务 result", w4Cond(frameTypeResult, "tsk_emit_ack"))
	res, _, _ := w4Find(frames, frameTypeResult, "tsk_emit_ack")
	if res["status"] != statusSucceeded {
		t.Fatalf("任务应当成功，实际 %v（error=%v）", res["status"], res["error"])
	}
	if summary, _ := res["summary"].(string); !strings.Contains(summary, "env=ok") {
		t.Fatalf("能力进程没拿到 emit 环境变量，summary=%q", summary)
	}
	h.stop()
}

// TestRunnerInjectsEmitEnv：起能力进程前必须注入 ANOTIFY_EMIT_SOCK / ANOTIFY_TASK_TOKEN
// （以及原本就有的 ANOTIFY_TASK_ID）。
func TestRunnerInjectsEmitEnv(t *testing.T) {
	dataDir := t.TempDir()
	opt := defaultEmitOptions(t, dataDir)
	opt.TaskID = "tsk_env"
	srv, _ := startEmitServer(t, opt)

	j := newRunnerJob(t, `printf '%s|%s|%s\n' "$ANOTIFY_EMIT_SOCK" "$ANOTIFY_TASK_TOKEN" "$ANOTIFY_TASK_ID"`+"\n")
	j.TaskID = "tsk_env"
	j.Emit = srv

	out := runJob(context.Background(), j)
	if out.Status != statusSucceeded {
		t.Fatalf("任务应当成功：%+v", out)
	}
	want := fmt.Sprintf("%s|%s|%s", srv.Path(), emitTestToken, "tsk_env")
	if !strings.Contains(out.Output, want) {
		t.Fatalf("子进程环境不对：want 含 %q，实际 %q", want, out.Output)
	}

	// 没有 socket（standalone / 起不来）→ 两个变量都**不存在**（脚本据此走本地模式分支）。
	j2 := newRunnerJob(t, `printf 'sock=[%s] token=[%s]\n' "${ANOTIFY_EMIT_SOCK:-}" "${ANOTIFY_TASK_TOKEN:-}"`+"\n")
	j2.TaskID = "tsk_nosock"
	out2 := runJob(context.Background(), j2)
	if out2.Status != statusSucceeded || !strings.Contains(out2.Output, "sock=[] token=[]") {
		t.Fatalf("没有 emit 服务时不该注入这两个变量：%+v", out2)
	}
}

// TestClientResendsUnackedOutboxOnRedispatch：中转重启 / 同一任务被重派后，
// client 侧必须把 outbox 里没 ack 的条目按 seq 顺序补发（emit-protocol.md §7 的最后一条）。
//
// 这一条是「agent 在 emit 与 ack 之间崩了」的落地证明：第一轮只落盘不 ack，
// 第二轮（同一个 data-dir、同一个 task_id）应当看到同一 seq 的重发帧。
func TestClientResendsUnackedOutboxOnRedispatch(t *testing.T) {
	dataDir := t.TempDir()
	// 脚本自报「我起来了」再等放行：任务工作目录由 agent 在**起进程前**清空重建，
	// 测试若不看这个标记就写 allow-exit，会被那次 RemoveAll 抹掉，脚本永远等不到 → 超时。
	script := `
if [ -S "$ANOTIFY_EMIT_SOCK" ]; then echo "sock=present"; fi
: > "$ANOTIFY_TASKDIR/started"
until [ -f "$ANOTIFY_TASKDIR/allow-exit" ]; do sleep 0.01; done
`
	assign := func() *assignFrame {
		return &assignFrame{
			Type:        frameTypeAssign,
			TaskID:      "tsk_resend",
			Capability:  "demo.resend",
			Version:     1,
			LeaseUntil:  time.Now().Add(time.Minute).Unix(),
			MaxDuration: 30,
			Ports:       []string{"fix"},
			Token:       emitTestToken,
		}
	}
	sockPath := filepath.Join(dataDir, emitSocketDirName, "tsk_resend"+emitSockSuffix)

	// ---------- 第一轮：emit 一条，**不给 ack**，然后让任务正常结束 ----------
	h1 := w4StartRawAt(t, w4Config(), dataDir, 200*time.Millisecond)
	h1.welcome(10)
	h1.deploy(w4Artifact("demo.resend", 1, script))
	h1.fc.feed(t, assign())
	w4WaitFile(t, sockPath, 5*time.Second)
	if r := mustEmit(t, sockPath, emitRequest{Op: emitOpName, Token: emitTestToken, Port: "fix",
		Data: json.RawMessage(`{"issue":"#42"}`)}); !r.OK || r.Seq != 1 {
		t.Fatalf("第一轮 emit 应当成功且 seq=1：%+v", r)
	}
	h1.fc.waitFor(t, 5*time.Second, "第一轮的上行 emit", w4Cond(frameTypeEmit, "tsk_resend"))
	w4WaitFile(t, filepath.Join(dataDir, "work", "tsk_resend", "started"), 5*time.Second)
	if err := os.WriteFile(filepath.Join(dataDir, "work", "tsk_resend", "allow-exit"), nil, 0o644); err != nil {
		t.Fatalf("放行脚本: %v", err)
	}
	h1.fc.waitFor(t, 10*time.Second, "第一轮 result", w4Cond(frameTypeResult, "tsk_resend"))
	h1.stop()

	// ---------- 第二轮：同一个 data-dir / task_id → 补发 seq=1 ----------
	h2 := w4StartRawAt(t, w4Config(), dataDir, 200*time.Millisecond)
	h2.welcome(10)
	h2.deploy(w4Artifact("demo.resend", 1, script))
	h2.fc.feed(t, assign())

	frames := h2.fc.waitFor(t, 5*time.Second, "补发的上行 emit", w4Cond(frameTypeEmit, "tsk_resend"))
	frame, _, _ := w4Find(frames, frameTypeEmit, "tsk_resend")
	if frame["seq"] != float64(1) {
		t.Fatalf("补发的必须是 seq=1（同一 seq 重发靠中枢幂等去重），实际 %v", frame["seq"])
	}
	// 新 emit 的 seq 接着往下走（不复用 1）。
	w4WaitFile(t, sockPath, 5*time.Second)
	if r := mustEmit(t, sockPath, emitRequest{Op: emitOpName, Token: emitTestToken, Port: "fix"}); r.Seq != 2 {
		t.Fatalf("重派后新 emit 的 seq 应当是 2，实际 %d", r.Seq)
	}
	w4WaitFile(t, filepath.Join(dataDir, "work", "tsk_resend", "started"), 5*time.Second)
	if err := os.WriteFile(filepath.Join(dataDir, "work", "tsk_resend", "allow-exit"), nil, 0o644); err != nil {
		t.Fatalf("放行脚本: %v", err)
	}
	h2.fc.waitFor(t, 10*time.Second, "第二轮 result", w4Cond(frameTypeResult, "tsk_resend"))
	h2.stop()
}
