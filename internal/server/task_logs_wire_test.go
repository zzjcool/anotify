package server

// 本文件是「按需取日志」的**跨包端到端**验收（CONTRACT-logs-on-node §2 / §7）：
//
//	浏览器请求 GET /v1/tasks/{taskId}/logs
//	  → consoleHandler.serveTaskLogs
//	  → ws.NodeHub.FetchLogs            （真 hub，不是 fake）
//	  → 一帧 logs_request 走真 WS 连接
//	  → 假节点（本文件里的一条真 websocket 客户端）回 logs_response
//	  → 200 + available:true + stdout 原文
//
// 为什么需要它（其它用例各自都能过，唯独这条链断掉时不会红）：
//   - internal/ws 的用例证明「配对/超时/离线」，但它用的是自己的夹具，不经 HTTP；
//   - internal/server 的用例证明「HTTP 语义」，但它注入的是 fake fetcher；
//   - 于是「mux 注入的是不是同一个 hub」「errors.Is 能不能认出跨包的错误」
//     「一帧真回包能不能真的被翻成 200」这三处**没人验**——真出问题时的症状是
//     「控制台永远显示节点离线」，而两边单测全绿。
//
// 顺带钉住「不缓存」：连续两次请求，节点回两份不同内容，两次都要看到新的那份。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/ws"
)

// wireFrame 是线上帧的通用形状（测试只关心 type / task_id / request_id）。
type wireFrame struct {
	Type      string `json:"type"`
	TaskID    string `json:"task_id"`
	RequestID string `json:"request_id"`
	NodeID    string `json:"node_id"`
	Protocol  int    `json:"protocol"`
}

// wireRead 读一帧并解成 wireFrame（超时/坏帧都算失败：说明链路上根本没帧）。
func wireRead(t *testing.T, conn *websocket.Conn, within time.Duration) wireFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("读 WS 帧失败: %v", err)
	}
	var f wireFrame
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("解 WS 帧失败: %v（%s）", err, data)
	}
	return f
}

// wireWrite 写一帧（测试用；任何失败都直接判死）。
func wireWrite(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("编帧失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("写 WS 帧失败: %v", err)
	}
}

// logsWireEnv 是这条端到端链的夹具：真库 + 真节点（WS 客户端）+ 真 hub + 真 handler。
type logsWireEnv struct {
	env  *consoleEnv
	hub  *ws.NodeHub
	conn *websocket.Conn
	task *store.Task
	// served 在「WS handler 返回」时归零——它保证 cleanup（同步写库）已经跑完，
	// 之后才允许用例结束（否则 db.Close 与 cleanup 会并发，见 #255）。
	served sync.WaitGroup
	get    func() *httptest.ResponseRecorder
}

// newLogsWireEnv 建库/任务/真 hub，并把「控制台日志端点」接在**同一个 hub** 上。
func newLogsWireEnv(t *testing.T) *logsWireEnv {
	t.Helper()
	ctx := context.Background()
	env := newConsoleEnv(t)

	node := &store.Node{NamespaceID: env.nsID, Name: "wire-node"}
	token, err := env.db.CreateNode(ctx, node)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	task := env.seedTask(t, env.nsID, "trc_wire", "runner", node.ID)

	hub := ws.NewNodeHub()
	nodeH := ws.NewNodeHandler(env.db)
	nodeH.Hub = hub

	e := &logsWireEnv{env: env, hub: hub, task: task}

	// 节点侧监听：包一层 WaitGroup，把「ServeHTTP 返回」变成可等待的事实。
	wsMux := http.NewServeMux()
	wsMux.Handle("GET /v1/node/stream", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.served.Add(1)
		defer e.served.Done()
		nodeH.ServeHTTP(w, r)
	}))
	srv := httptest.NewServer(wsMux)
	t.Cleanup(srv.Close)

	// 控制台侧：只挂日志端点，数据源是**真的** hub（与 mux.go 的生产接线同形）。
	ch := &consoleHandler{db: env.db, taskLogs: hub}
	mux := http.NewServeMux()
	mux.Handle("GET /v1/tasks/{taskId}/logs",
		noStore(env.svc.Sessions().Middleware(http.HandlerFunc(ch.serveTaskLogs))))

	// get 不碰 testing.T 的 Fatal 通道：它会在 goroutine 里被调用（HTTP 请求要等
	// 主 goroutine 在 WS 上回包，两者必须并发）。
	e.get = func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet,
			"/v1/tasks/"+task.ID+"/logs?namespaceId="+env.nsID, nil)
		req.AddCookie(env.cookie)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		return rr
	}

	// 连上节点（真 WS + 真鉴权 + 真 hello）。
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/node/stream"
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}},
	})
	if err != nil {
		t.Fatalf("dial node stream: %v", err)
	}
	e.conn = conn
	t.Cleanup(func() { _ = conn.CloseNow() })

	wireWrite(t, conn, &ws.HelloFrame{
		Type: ws.NodeFrameHello, NodeID: node.ID, Protocol: ws.ProtocolVersion,
		Instances: []ws.InstanceDecl{{Slot: 0, Concurrency: 1}},
	})
	if got := wireRead(t, conn, 5*time.Second); got.Type != ws.NodeFrameWelcome {
		t.Fatalf("握手后第一帧 = %q, want welcome", got.Type)
	}
	return e
}

// TestTaskLogsThroughRealNodeHub 端到端：HTTP → 真 hub → 真 WS → 节点回包 → 200。
func TestTaskLogsThroughRealNodeHub(t *testing.T) {
	e := newLogsWireEnv(t)

	// 两轮，节点回不同内容：第二轮必须看到第二份（不缓存）。
	// 这是「每次都向节点取」在**真链路**上的证据——fake fetcher 只能证明 handler 没缓存。
	for round, want := range []string{"first-round-stdout\n", "second-round-stdout\n"} {
		recCh := make(chan *httptest.ResponseRecorder, 1)
		go func() { recCh <- e.get() }()

		req := wireRead(t, e.conn, 5*time.Second)
		if req.Type != ws.NodeFrameLogsRequest {
			t.Fatalf("round %d: 节点收到的帧 = %q, want logs_request", round, req.Type)
		}
		if req.TaskID != e.task.ID || req.RequestID == "" {
			t.Fatalf("round %d: logs_request 内容不对: %+v", round, req)
		}
		wireWrite(t, e.conn, &ws.LogsResponseFrame{
			Type: ws.NodeFrameLogsResponse, TaskID: req.TaskID, RequestID: req.RequestID,
			Stdout: want, Stderr: "stderr-" + want, Truncated: true, Bytes: 4096,
		})

		var rec *httptest.ResponseRecorder
		select {
		case rec = <-recCh:
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: HTTP 请求没有返回", round)
		}

		if rec.Code != http.StatusOK {
			t.Fatalf("round %d: status = %d, want 200（取日志绝不该 5xx）: %s", round, rec.Code, rec.Body.String())
		}
		got := decodeLogs(t, rec)
		if !*got.Available {
			t.Fatalf("round %d: available = false, want true（节点明明回了包）: %s", round, rec.Body.String())
		}
		if got.Stdout != want || got.Stderr != "stderr-"+want {
			t.Fatalf("round %d: 回包内容没有逐字透传: %+v", round, got)
		}
		if !got.Truncated || got.Bytes != 4096 {
			t.Fatalf("round %d: truncated/bytes 没透传: %+v", round, got)
		}
		if got.Reason != nil {
			t.Fatalf("round %d: 成功时不该带 reason（%q）", round, *got.Reason)
		}
	}

	// 断开节点：下一次请求必须落到**真** hub 的离线分支（ErrNodeOffline → node_offline），
	// 而不是 fake 里那句 errors.New。
	_ = e.conn.CloseNow()
	e.served.Wait() // handler 返回 ⇒ nodeSession.cleanup（同步）已经跑完

	rr := e.get()
	if rr.Code != http.StatusOK {
		t.Fatalf("离线时 status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	off := decodeLogs(t, rr)
	if *off.Available {
		t.Fatalf("离线时 available = true, want false: %s", rr.Body.String())
	}
	if off.Reason == nil || *off.Reason != "node_offline" {
		t.Fatalf("离线时 reason = %v, want node_offline: %s", off.Reason, rr.Body.String())
	}
	if off.Stdout != "" || off.Stderr != "" || off.Bytes != 0 {
		t.Fatalf("离线时不该带旧内容: %+v", off)
	}
}
