package ws

// 本文件是「向节点按需取日志」的中枢侧验收（CONTRACT-logs-on-node §1）：
//
//   - 两帧的**线上字段名**（task_id / request_id / stdout / stderr / truncated / bytes / error）
//     是协议的一部分：漂移一个字符，取日志就静默变成「永远是空」；
//   - 配对：request_id 对不上 / task_id 对不上的回包**必须丢弃**（宁可超时，不能错配）；
//   - 三种「没取到」：节点离线、超时、节点回 no_local_archive；
//   - 连接断开时等待者立刻醒来（不等 5s 超时）。
//
// 不在这里的部分：节点侧怎么读档案（cmd/agent）、HTTP 端点怎么翻成 available/reason
// （internal/server）——两边各自钉在自己的包，接口见 node_logs.go 的 FetchLogs。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// logsFetchResult 是后台 FetchLogs 的返回值（在测试 goroutine 里取）。
type logsFetchResult struct {
	resp *LogsResponseFrame
	err  error
}

// fetchAsync 起一个后台 FetchLogs（node 侧要在主 goroutine 里回包）。
func fetchAsync(h *NodeHub, nsID, nodeID, taskID string) chan logsFetchResult {
	ch := make(chan logsFetchResult, 1)
	go func() {
		resp, err := h.FetchLogs(context.Background(), nsID, nodeID, taskID)
		ch <- logsFetchResult{resp: resp, err: err}
	}()
	return ch
}

// awaitFetch 等后台结果（上限远大于一次夹具往返，超时即测试失败而不是挂住）。
func awaitFetch(t *testing.T, ch chan logsFetchResult) logsFetchResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("FetchLogs 没有在 10s 内返回")
		return logsFetchResult{}
	}
}

// TestLogsFrameWire 两帧的线上字段名逐字对齐契约 §1。
//
// 为什么这条必须存在：帧是跨进程的字符串协议，字段名写错**不会**编译失败，
// 症状是「控制台永远是空日志」——而两边各自的单测都能过。
func TestLogsFrameWire(t *testing.T) {
	req, err := Encode(&LogsRequestFrame{
		Type: NodeFrameLogsRequest, TaskID: "tsk_1", RequestID: "lreq_abc",
	})
	if err != nil {
		t.Fatalf("Encode logs_request: %v", err)
	}
	var reqMap map[string]any
	if err := json.Unmarshal(req, &reqMap); err != nil {
		t.Fatalf("decode logs_request: %v", err)
	}
	if reqMap["type"] != "logs_request" || reqMap["task_id"] != "tsk_1" || reqMap["request_id"] != "lreq_abc" {
		t.Fatalf("logs_request 线上形状不对: %s", req)
	}
	// 契约 §1：帧里**不带 params**（入参属于 input.json，控制台暂不展示）。
	if len(reqMap) != 3 {
		t.Fatalf("logs_request 只该有三个键（type/task_id/request_id），实际 %v", reqMap)
	}

	resp, err := Encode(&LogsResponseFrame{
		Type: NodeFrameLogsResponse, TaskID: "tsk_1", RequestID: "lreq_abc",
		Stdout: "out\n", Stderr: "err\n", Truncated: true, Bytes: 42,
	})
	if err != nil {
		t.Fatalf("Encode logs_response: %v", err)
	}
	var respMap map[string]any
	if err := json.Unmarshal(resp, &respMap); err != nil {
		t.Fatalf("decode logs_response: %v", err)
	}
	want := map[string]any{
		"type": "logs_response", "task_id": "tsk_1", "request_id": "lreq_abc",
		"stdout": "out\n", "stderr": "err\n", "truncated": true, "bytes": float64(42),
	}
	for k, v := range want {
		if respMap[k] != v {
			t.Fatalf("logs_response.%s = %v, want %v（帧: %s）", k, respMap[k], v, resp)
		}
	}
	// error 空时不出线（omitempty）：节点「取到了但什么都没打印」不能长得像「没取到」。
	if _, ok := respMap["error"]; ok {
		t.Fatalf("error 为空时不该出线: %s", resp)
	}

	// 上行/下行方向：写反了会被当 unknown_type 回一帧 error，取日志只能超时。
	if !IsUpstream(NodeFrameLogsResponse) || !IsDownstream(NodeFrameLogsRequest) {
		t.Fatalf("帧方向错了: logs_response upstream=%v, logs_request downstream=%v",
			IsUpstream(NodeFrameLogsResponse), IsDownstream(NodeFrameLogsRequest))
	}
	if IsDownstream(NodeFrameLogsResponse) || IsUpstream(NodeFrameLogsRequest) {
		t.Fatal("日志帧方向被写反了（upstream/downstream 两边都判成 true）")
	}
}

// TestDecodeUpstreamLogsResponse 漏 case 的后果与 apply 帧那次一样：回包被当 unknown_type，
// 中枢回一帧 error，取日志只能等到超时。
func TestDecodeUpstreamLogsResponse(t *testing.T) {
	raw := `{"type":"logs_response","task_id":"tsk_1","request_id":"lreq_x",` +
		`"stdout":"A-ran\n","stderr":"","truncated":false,"bytes":6,"error":""}`
	got, err := DecodeUpstream([]byte(raw))
	if err != nil {
		t.Fatalf("DecodeUpstream: %v", err)
	}
	f, ok := got.(*LogsResponseFrame)
	if !ok {
		t.Fatalf("DecodeUpstream 返回 %T, want *LogsResponseFrame", got)
	}
	if f.TaskID != "tsk_1" || f.RequestID != "lreq_x" || f.Stdout != "A-ran\n" || f.Bytes != 6 {
		t.Fatalf("logs_response 字段解析不对: %+v", f)
	}
}

// TestFetchLogsDeliversResponse 命中：请求帧真的发到了节点，回包按 request_id 配上。
func TestFetchLogsDeliversResponse(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, nil, []InstanceDecl{{Slot: 0, Concurrency: 1}})

	ch := fetchAsync(f.handler.hub(), f.nsID, f.node.ID, "tsk_hit")

	req, ok := tryReadFrame(conn, 2*time.Second)
	if !ok {
		t.Fatal("节点没收到 logs_request")
	}
	if req["type"] != NodeFrameLogsRequest || req["task_id"] != "tsk_hit" {
		t.Fatalf("logs_request 内容不对: %v", req)
	}
	requestID, _ := req["request_id"].(string)
	if !strings.HasPrefix(requestID, logsRequestIDPrefix) {
		t.Fatalf("request_id = %q, want 前缀 %q", requestID, logsRequestIDPrefix)
	}

	send(t, conn, &LogsResponseFrame{
		Type: NodeFrameLogsResponse, TaskID: "tsk_hit", RequestID: requestID,
		Stdout: "first\nlast\n", Stderr: "boom\n", Truncated: true, Bytes: 4096,
	})

	r := awaitFetch(t, ch)
	if r.err != nil {
		t.Fatalf("FetchLogs err = %v", r.err)
	}
	if r.resp.Stdout != "first\nlast\n" || r.resp.Stderr != "boom\n" ||
		!r.resp.Truncated || r.resp.Bytes != 4096 {
		t.Fatalf("回包字段没有逐字透传: %+v", r.resp)
	}
}

// TestFetchLogsNoLocalArchive 节点上没有档案 → ErrNoLocalArchive（端点据此给 reason）。
func TestFetchLogsNoLocalArchive(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, nil, []InstanceDecl{{Slot: 0, Concurrency: 1}})

	ch := fetchAsync(f.handler.hub(), f.nsID, f.node.ID, "tsk_gone")
	req, ok := tryReadFrame(conn, 2*time.Second)
	if !ok {
		t.Fatal("节点没收到 logs_request")
	}
	send(t, conn, &LogsResponseFrame{
		Type: NodeFrameLogsResponse, TaskID: "tsk_gone",
		RequestID: req["request_id"].(string),
		Error:     LogsErrNoLocalArchive,
	})

	r := awaitFetch(t, ch)
	if !errors.Is(r.err, ErrNoLocalArchive) {
		t.Fatalf("FetchLogs err = %v, want ErrNoLocalArchive", r.err)
	}
}

// TestFetchLogsOffline 没有活跃连接（含「任务还没派给任何节点」）→ ErrNodeOffline。
func TestFetchLogsOffline(t *testing.T) {
	f := newNodeFixture(t)
	hub := f.handler.hub()

	if _, err := hub.FetchLogs(context.Background(), f.nsID, f.node.ID, "tsk_x"); !errors.Is(err, ErrNodeOffline) {
		t.Fatalf("离线节点 FetchLogs err = %v, want ErrNodeOffline", err)
	}
	// 未知节点同样是 ErrNodeOffline（不 panic、不静默成功）
	if _, err := hub.FetchLogs(context.Background(), f.nsID, "nd_nope", "tsk_x"); !errors.Is(err, ErrNodeOffline) {
		t.Fatalf("未知节点 FetchLogs err = %v, want ErrNodeOffline", err)
	}
	// 空 task id 直接拒（不发帧、不登记）
	if _, err := hub.FetchLogs(context.Background(), f.nsID, f.node.ID, ""); err == nil {
		t.Fatal("空 task id 应该报错")
	}
}

// TestFetchLogsCrossNamespace 跨命名空间取日志必须被拒（不变量 I10/I11）。
func TestFetchLogsCrossNamespace(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, nil, []InstanceDecl{{Slot: 0, Concurrency: 1}})

	if _, err := f.handler.hub().FetchLogs(context.Background(), "ns_other", f.node.ID, "tsk_x"); err == nil {
		t.Fatal("跨命名空间 FetchLogs 应该被拒")
	}
	// 且**没有**帧被发出去：节点侧读到的是超时，不该收到一条属于别人命名空间的请求。
	if m, ok := tryReadFrame(conn, 300*time.Millisecond); ok {
		t.Fatalf("跨命名空间不该发帧，却收到 %v", m)
	}
}

// TestFetchLogsTimeout 节点不回包 → ErrLogsTimeout（用可注入的短超时，不睡 5s）。
func TestFetchLogsTimeout(t *testing.T) {
	f := newNodeFixture(t, func(h *NodeHandler) {
		h.Hub = NewNodeHub()
		h.Hub.LogsTimeout = 80 * time.Millisecond
	})
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, nil, []InstanceDecl{{Slot: 0, Concurrency: 1}})

	start := time.Now()
	ch := fetchAsync(f.handler.hub(), f.nsID, f.node.ID, "tsk_slow")
	if _, ok := tryReadFrame(conn, 2*time.Second); !ok {
		t.Fatal("节点没收到 logs_request")
	}
	r := awaitFetch(t, ch)
	if !errors.Is(r.err, ErrLogsTimeout) {
		t.Fatalf("FetchLogs err = %v, want ErrLogsTimeout", r.err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("超时用掉 %v，注入的 80ms 没生效（写死了 sleep？）", elapsed)
	}
	// 超时之后再晚到的回包只能被丢弃（不能 panic、也不能影响连接）
	send(t, conn, &LogsResponseFrame{
		Type: NodeFrameLogsResponse, TaskID: "tsk_slow", RequestID: "lreq_late", Stdout: "late",
	})
	if m, ok := tryReadFrame(conn, 300*time.Millisecond); ok {
		t.Fatalf("晚到的回包不该换来一帧下行（会误导节点以为协议坏了）: %v", m)
	}
}

// TestFetchLogsDropsUnpairedResponse request_id 对不上的回包必须被丢弃：
// 它既不能唤醒错误的任务，也不能让连接进入任何异常状态。
func TestFetchLogsDropsUnpairedResponse(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, nil, []InstanceDecl{{Slot: 0, Concurrency: 1}})

	ch := fetchAsync(f.handler.hub(), f.nsID, f.node.ID, "tsk_pair")
	req, ok := tryReadFrame(conn, 2*time.Second)
	if !ok {
		t.Fatal("节点没收到 logs_request")
	}
	requestID := req["request_id"].(string)

	// ① 完全没登记过的 request_id：丢弃。
	send(t, conn, &LogsResponseFrame{
		Type: NodeFrameLogsResponse, TaskID: "tsk_pair", RequestID: "lreq_not_mine", Stdout: "WRONG",
	})
	// ② 配对键对、task_id 不对：同样丢弃（错配的日志比没有日志危险）。
	send(t, conn, &LogsResponseFrame{
		Type: NodeFrameLogsResponse, TaskID: "tsk_other", RequestID: requestID, Stdout: "ALSO WRONG",
	})
	// 两次错包都不能唤醒等待者：给服务端一个“确实处理过它们”的窗口，再确认
	// 后台 FetchLogs 还在等。
	//
	// 这里用 sleep 而不是「读一帧看有没有 error」：coder/websocket 的 Read(ctx)
	// 在 ctx 超时时会**关掉连接**，用它探测“没有帧”会把后面的正确回包也一起弄死。
	time.Sleep(80 * time.Millisecond)
	select {
	case r := <-ch:
		t.Fatalf("错配的回包被投递了: resp=%+v err=%v", r.resp, r.err)
	default:
	}
	// ③ 正确的回包仍然能配上（说明①②只是被丢弃，没有把等待者弄坏）。
	send(t, conn, &LogsResponseFrame{
		Type: NodeFrameLogsResponse, TaskID: "tsk_pair", RequestID: requestID, Stdout: "RIGHT",
	})
	r := awaitFetch(t, ch)
	if r.err != nil {
		t.Fatalf("FetchLogs err = %v", r.err)
	}
	if r.resp.Stdout != "RIGHT" {
		t.Fatalf("stdout = %q, want RIGHT（错配的回包被投递了？）", r.resp.Stdout)
	}
}

// TestFetchLogsWakesOnDisconnect 连接在请求在飞的时候断掉 → 立刻返回 ErrNodeOffline，
// 不让调用方白等 5s（控制台的一次点击不该挂在一个已经没了的节点上）。
func TestFetchLogsWakesOnDisconnect(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, nil, []InstanceDecl{{Slot: 0, Concurrency: 1}})

	ch := fetchAsync(f.handler.hub(), f.nsID, f.node.ID, "tsk_break")
	if _, ok := tryReadFrame(conn, 2*time.Second); !ok {
		t.Fatal("节点没收到 logs_request")
	}
	start := time.Now()
	_ = conn.CloseNow()

	r := awaitFetch(t, ch)
	if !errors.Is(r.err, ErrNodeOffline) {
		t.Fatalf("FetchLogs err = %v, want ErrNodeOffline", r.err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("断连唤醒用掉 %v（在等 5s 超时？）", elapsed)
	}
}

// TestLogsRequestIDUniqueAndSafe request_id 会进日志与错误信息，必须无特殊字符且不重复。
func TestLogsRequestIDUniqueAndSafe(t *testing.T) {
	seen := make(map[string]struct{}, 500)
	for i := 0; i < 500; i++ {
		id := newLogsRequestID()
		if !strings.HasPrefix(id, logsRequestIDPrefix) {
			t.Fatalf("id %q 缺前缀 %q", id, logsRequestIDPrefix)
		}
		for _, r := range strings.TrimPrefix(id, logsRequestIDPrefix) {
			if !strings.ContainsRune(base62Alphabet, r) {
				t.Fatalf("id %q 含非 base62 字符 %q", id, r)
			}
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("request_id 重复: %s", id)
		}
		seen[id] = struct{}{}
	}
}
