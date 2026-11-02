// Command fakebroker 是**安装脚本自测用的最小中枢**：只握手，不派活。
//
// 为什么要有它：install-agent.sh 的核心承诺是「装完即上线」，而它靠预检里的一行
// 日志（agent.handshake.ok）判断"真的连上了"。这一行要是写错，每个用户的安装都会
// 在最后一步莫名失败——只有真握一次手才验得出来，纯文本断言验不出来。
//
// 另一个开关 -fail-first N 模拟中枢拒绝：「节点申报了自己不认识的产物」
// （unknown_capability）在真中枢上是**整条连接拒绝**，安装脚本会挪走过期索引重试一次。
//
// 放在 testdata/ 下：go 工具的 `./...` 会跳过 testdata 目录，所以它不进 CI 的
// go build / go vet 视野；自测脚本用显式路径 `go build ./scripts/testdata/fakebroker`
// 编它（显式路径不受该规则限制）。
//
// 用法：fakebroker -addr 127.0.0.1:0 [-fail-first 1]
//
//	就绪后在 stdout 打一行 `listening on 127.0.0.1:<port>`（测试据此拿端口）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/coder/websocket"
)

var (
	addr      = flag.String("addr", "127.0.0.1:0", "监听地址（:0 = 让内核挑空闲端口）")
	failFirst = flag.Int("fail-first", 0, "前 N 次连接回 unknown_capability，之后正常握手")

	mu    sync.Mutex // 保护 conns（连接可能并发）
	conns int
)

func main() {
	flag.Parse()

	log.SetFlags(0)
	log.SetOutput(os.Stderr)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/node/stream", handleNodeStream)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}
	// 这一行是测试的同步点：先看见它，再去跑安装脚本。
	fmt.Printf("listening on %s\n", ln.Addr().String())

	srv := &http.Server{Handler: mux}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(ln); err != nil && ctx.Err() == nil {
		log.Fatalf("serve: %v", err)
	}
}

// handleNodeStream 走完 hello → welcome 就够：agent 收到 welcome 会打
// `event=agent.handshake.ok`，安装脚本据此判定预检通过。
//
// welcome / error 的字段名以 internal/ws 为准（node_id / heartbeat_sec / lease_ttl_sec、
// code / message）；这里写字面量是因为它只是测试替身，真中枢的序列化由 internal/ws 负责。
func handleNodeStream(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := r.Context()

	mu.Lock()
	conns++
	n := conns
	mu.Unlock()

	// 第 1 帧必须是 hello；内容不关心（fakebroker 不保管任何状态）。
	typ, data, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		return
	}
	var hello struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &hello); err != nil || hello.Type != "hello" {
		_ = c.Write(ctx, websocket.MessageText, mustJSON(map[string]any{
			"type": "error", "code": "bad_hello", "message": "first frame must be hello",
		}))
		return
	}

	if n <= *failFirst {
		_ = c.Write(ctx, websocket.MessageText, mustJSON(map[string]any{
			"type":    "error",
			"code":    "unknown_capability",
			"message": "unknown capability: stale.cap@" + fmt.Sprint(n),
		}))
		return
	}

	if err := c.Write(ctx, websocket.MessageText, mustJSON(map[string]any{
		"type":          "welcome",
		"node_id":       "node_selftest",
		"protocol":      2,
		"heartbeat_sec": 30,
		"lease_ttl_sec": 60,
	})); err != nil {
		return
	}
	// 挂着直到对端断开：安装脚本握手成功后会杀掉预检进程。
	for {
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		log.Fatalf("marshal: %v", err)
	}
	return b
}
