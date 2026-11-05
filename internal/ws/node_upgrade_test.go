package ws

// 本文件是 agent 自升级的协议层验收（CONTRACT-agent-upgrade §3 / §5 / §5.1）：
//
//   - 两帧（upgrade 下行 / upgrade_ack 上行）的编解码与线上字段名；
//   - 推送通道（PublishUpgrade）的离线 / 跨命名空间 / 送达三种结果；
//   - hello.agent_version 落库（带 / 不带都不报错）；
//   - upgrade_ack 的两种终态落库（成功写版本，失败只写状态 + WARN 日志）。
//
// 不在这里的部分：「控制台点一下就下发」的 HTTP 端点（internal/server），
// 以及节点侧真正替换二进制（cmd/agent，A 组）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// TestDecodeUpstreamUpgradeAck upgrade_ack 是上行帧：DecodeUpstream 必须解出全部字段。
//
// 漏一个 case 的后果与 apply 帧相同：帧被当成 *UnknownFrame，中枢回 unknown_type，
// 于是「升级成功了但版本列永远不变」——控制台会一直显示旧版本。
func TestDecodeUpstreamUpgradeAck(t *testing.T) {
	raw := `{"type":"upgrade_ack","ok":true,"version":"1.5.0+3fd5a59","commit":"3fd5a59","error":""}`
	got, err := DecodeUpstream([]byte(raw))
	if err != nil {
		t.Fatalf("DecodeUpstream: %v", err)
	}
	f, ok := got.(*UpgradeAckFrame)
	if !ok {
		t.Fatalf("DecodeUpstream 返回 %T, want *UpgradeAckFrame", got)
	}
	if f.Type != NodeFrameUpgradeAck || !f.OK || f.Version != "1.5.0+3fd5a59" || f.Commit != "3fd5a59" {
		t.Fatalf("upgrade_ack 字段解析不对: %+v", f)
	}
}

// TestUpgradeFrameWire upgrade 帧的**线上字段名**是契约的一部分（§5 的 JSON 逐字）：
// 节点按 target_version / sha256 / url_path 取值，名字一旦漂移，节点就下不到东西。
func TestUpgradeFrameWire(t *testing.T) {
	data, err := Encode(&UpgradeFrame{
		Type:          NodeFrameUpgrade,
		TargetVersion: "1.5.0+3fd5a59",
		SHA256:        strings.Repeat("a", 64),
		URLPath:       NodeBinaryPath,
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["type"] != NodeFrameUpgrade || m["target_version"] != "1.5.0+3fd5a59" ||
		m["sha256"] != strings.Repeat("a", 64) || m["url_path"] != NodeBinaryPath {
		t.Fatalf("upgrade 帧线上形状不对: %s", data)
	}
	// 路径常量与契约 §5 逐字一致（帧里下发它，节点拿它拼 URL）
	if NodeBinaryPath != "/v1/node/binary" {
		t.Fatalf("NodeBinaryPath = %q, want /v1/node/binary", NodeBinaryPath)
	}
}

// TestNormalizeUpgradeError 未知短码不落库（出线形状是封闭集合，§5.1）。
func TestNormalizeUpgradeError(t *testing.T) {
	for _, code := range []string{
		UpgradeErrNeedRoot, UpgradeErrBadSHA, UpgradeErrDownloadFailed, UpgradeErrNotSupported,
	} {
		if got := NormalizeUpgradeError(code); got != code {
			t.Errorf("NormalizeUpgradeError(%q) = %q, want 原样", code, got)
		}
	}
	for _, code := range []string{"", "boom", "NEED_ROOT"} {
		if got := NormalizeUpgradeError(code); got != "" {
			t.Errorf("NormalizeUpgradeError(%q) = %q, want 空串（未知码）", code, got)
		}
	}
}

// TestPublishUpgradeOfflineNode 没有活跃连接 → ErrNodeOffline（端点据此回 409）。
func TestPublishUpgradeOfflineNode(t *testing.T) {
	f := newNodeFixture(t)

	err := f.handler.hub().PublishUpgrade(f.nsID, f.node.ID, &UpgradeFrame{
		Type: NodeFrameUpgrade, TargetVersion: "1.5.0+aaa", SHA256: "x", URLPath: NodeBinaryPath,
	})
	if !errors.Is(err, ErrNodeOffline) {
		t.Fatalf("离线节点推升级 err = %v, want ErrNodeOffline", err)
	}
	// 不存在的节点同样 ErrNodeOffline（不 panic、不静默成功）
	if err := f.handler.hub().PublishUpgrade(f.nsID, "nd_does_not_exist", &UpgradeFrame{
		Type: NodeFrameUpgrade, TargetVersion: "1.5.0+aaa", URLPath: NodeBinaryPath,
	}); !errors.Is(err, ErrNodeOffline) {
		t.Fatalf("未知节点推升级 err = %v, want ErrNodeOffline", err)
	}
}

// TestPublishUpgradeDeliversFrame 在线节点收到 upgrade 帧：三件「去哪拿、拿到后该是什么」原样到达。
//
// 这是「控制台点一下 → 节点真的收到」这条链在中枢侧可测的全部（替换逻辑在 cmd/agent）。
func TestPublishUpgradeDeliversFrame(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	sum := strings.Repeat("b", 64)
	if err := f.handler.hub().PublishUpgrade(f.nsID, f.node.ID, &UpgradeFrame{
		Type:          NodeFrameUpgrade,
		TargetVersion: "1.5.0+3fd5a59",
		SHA256:        sum,
		URLPath:       NodeBinaryPath,
	}); err != nil {
		t.Fatalf("publish upgrade: %v", err)
	}

	got := readUntil(t, conn, NodeFrameUpgrade)
	if got["target_version"] != "1.5.0+3fd5a59" {
		t.Errorf("target_version = %v, want 1.5.0+3fd5a59", got["target_version"])
	}
	if got["sha256"] != sum {
		t.Errorf("sha256 = %v, want %s", got["sha256"], sum)
	}
	if got["url_path"] != NodeBinaryPath {
		t.Errorf("url_path = %v, want %s", got["url_path"], NodeBinaryPath)
	}
}

// TestPublishUpgradeCrossNamespaceRejected 绝不跨命名空间推升级（不变量 I10/I11）。
func TestPublishUpgradeCrossNamespaceRejected(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	otherNS := newOtherNamespace(t, f)
	err := f.handler.hub().PublishUpgrade(otherNS, f.node.ID, &UpgradeFrame{
		Type: NodeFrameUpgrade, TargetVersion: "1.5.0+aaa", URLPath: NodeBinaryPath,
	})
	if err == nil {
		t.Fatal("跨命名空间推升级成功了（隔离边界破了）")
	}
	if !strings.Contains(err.Error(), "another namespace") {
		t.Fatalf("错误信息 = %q, want 说明是跨命名空间", err.Error())
	}
	assertNoFrame(t, conn, 300*time.Millisecond, "跨命名空间的升级不该送到这条连接上")
}

// TestHelloAgentVersionPersisted hello 里的 agent_version 落库（契约 §3）。
//
// 这里用 sendRaw 而不是 helloFrame：字段由 A 组加在 HelloFrame 上（§7），
// B 组不改那个 struct，所以测试也从**线上 JSON**出发——测的是同一个键。
func TestHelloAgentVersionPersisted(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)

	sendRaw(t, conn, fmt.Sprintf(
		`{"type":"hello","node_id":%q,"protocol":%d,"applied":[],"instances":[{"slot":0,"concurrency":1}],"agent_version":"1.5.0+3fd5a59"}`,
		f.node.ID, ProtocolVersion))
	readUntil(t, conn, NodeFrameWelcome)

	got, err := f.db.GetNode(context.Background(), f.nsID, f.node.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.AgentVersion != "1.5.0+3fd5a59" {
		t.Fatalf("hello 之后 AgentVersion = %q, want 1.5.0+3fd5a59", got.AgentVersion)
	}
	// hello 没有 commit 字段：不能凭空写一列值。
	if got.AgentCommit != "" {
		t.Fatalf("hello 不该写 agent_commit，得到 %q", got.AgentCommit)
	}
	if got.AgentUpgrade != (store.AgentUpgrade{}) {
		t.Fatalf("hello 不该碰 upgrade 状态，得到 %+v", got.AgentUpgrade)
	}
}

// TestHelloWithoutAgentVersion 老 agent 不带 agent_version → 空串，握手照常成功。
func TestHelloWithoutAgentVersion(t *testing.T) {
	f := newNodeFixture(t)
	// 先让库里有一个「旧版本」（上一次连接写的），确认不带字段的新 hello 会把它刷成空串：
	// 「现在这台机器上的版本未知」是事实，保留旧值才是撒谎。
	if err := f.db.UpdateNodeAgentVersion(context.Background(), f.nsID, f.node.ID, "0.9.0+old"); err != nil {
		t.Fatalf("seed agent version: %v", err)
	}
	conn := f.mustDial(t)
	sendRaw(t, conn, fmt.Sprintf(
		`{"type":"hello","node_id":%q,"protocol":%d,"applied":[],"instances":[{"slot":0,"concurrency":1}]}`,
		f.node.ID, ProtocolVersion))
	readUntil(t, conn, NodeFrameWelcome)

	got, err := f.db.GetNode(context.Background(), f.nsID, f.node.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.AgentVersion != "" {
		t.Fatalf("老 agent 之后 AgentVersion = %q, want 空串", got.AgentVersion)
	}
}

// TestUpgradeAckSuccessPersists ack ok=true → 版本 + commit + agentUpgrade=ok 一起落库。
func TestUpgradeAckSuccessPersists(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})
	// 先模拟端点写过的 sent（真实链路里由 POST /upgrade 写）。
	if err := f.db.MarkAgentUpgradeSent(context.Background(), f.nsID, f.node.ID, "1.5.0+3fd5a59", 1); err != nil {
		t.Fatalf("seed sent: %v", err)
	}

	send(t, conn, &UpgradeAckFrame{
		Type: NodeFrameUpgradeAck, OK: true, Version: "1.5.0+3fd5a59", Commit: "3fd5a59",
	})

	got := waitForNode(t, f, func(n *store.Node) bool { return n.AgentUpgrade.Status == store.AgentUpgradeOK })
	if got.AgentVersion != "1.5.0+3fd5a59" || got.AgentCommit != "3fd5a59" {
		t.Fatalf("ack 成功之后版本列 = (%q, %q), want (1.5.0+3fd5a59, 3fd5a59)",
			got.AgentVersion, got.AgentCommit)
	}
	if got.AgentUpgrade.TargetVersion != "1.5.0+3fd5a59" {
		t.Fatalf("agentUpgrade.targetVersion = %q, want 目标版本", got.AgentUpgrade.TargetVersion)
	}
	if got.AgentUpgrade.Error != "" || got.AgentUpgrade.At <= 0 {
		t.Fatalf("agentUpgrade = %+v, want 无错误且有 at", got.AgentUpgrade)
	}
}

// TestUpgradeAckFailurePersistsAndLogs ack ok=false →
//
//	① agentUpgrade = failed + 契约短码（targetVersion 沿用 sent 的值）；
//	② 版本列**不变**（节点上的二进制没换，写成目标版本 = 谎报成功）；
//	③ 一条 WARN 日志（带 node_id 与错误码）——控制台之外的第二份事实。
func TestUpgradeAckFailurePersistsAndLogs(t *testing.T) {
	logs := installCaptureLogger(t)

	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})
	ctx := context.Background()
	if err := f.db.UpdateNodeAgentVersion(ctx, f.nsID, f.node.ID, "1.4.0+old"); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	if err := f.db.MarkAgentUpgradeSent(ctx, f.nsID, f.node.ID, "1.5.0+3fd5a59", 1); err != nil {
		t.Fatalf("seed sent: %v", err)
	}

	send(t, conn, &UpgradeAckFrame{Type: NodeFrameUpgradeAck, OK: false, Error: UpgradeErrNeedRoot})

	got := waitForNode(t, f, func(n *store.Node) bool { return n.AgentUpgrade.Status == store.AgentUpgradeFailed })
	if got.AgentUpgrade.Error != UpgradeErrNeedRoot {
		t.Fatalf("agentUpgrade.error = %q, want %q", got.AgentUpgrade.Error, UpgradeErrNeedRoot)
	}
	if got.AgentUpgrade.TargetVersion != "1.5.0+3fd5a59" {
		t.Fatalf("失败的尝试丢了目标版本: %+v", got.AgentUpgrade)
	}
	if got.AgentVersion != "1.4.0+old" || got.AgentCommit != "" {
		t.Fatalf("失败的升级改了版本列: (%q, %q), want (1.4.0+old, \"\")",
			got.AgentVersion, got.AgentCommit)
	}

	// 日志是闭环证据：等它出现（DB 已落库，日志在同一函数的下一行）。
	waitFor(t, func() bool {
		s := logs.String()
		return strings.Contains(s, "node.ws.upgrade.failed") &&
			strings.Contains(s, "node_id="+f.node.ID) &&
			strings.Contains(s, UpgradeErrNeedRoot)
	}, "升级失败的 WARN 日志（event/node_id/错误短码）")
}

// TestUpgradeAckUnknownErrorCodeNormalized 未知错误码不落库（出线枚举是封闭集合）。
func TestUpgradeAckUnknownErrorCodeNormalized(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	send(t, conn, &UpgradeAckFrame{Type: NodeFrameUpgradeAck, OK: false, Error: "boom"})

	got := waitForNode(t, f, func(n *store.Node) bool { return n.AgentUpgrade.Status == store.AgentUpgradeFailed })
	if got.AgentUpgrade.Error != "" {
		t.Fatalf("未知错误码落库成了 %q，want 空串", got.AgentUpgrade.Error)
	}
}

// ---------- 本地小工具 ----------

// waitForNode 轮询库里的节点行直到 cond 成立（超时即失败）。
//
// 为什么需要轮询：成功/失败的 ack 都**没有响应帧**（节点发完就退出/继续干活），
// 测试只能等库里的副作用。窗口 5s 足够覆盖一次本地 SQLite 写。
func waitForNode(t *testing.T, f *nodeFixture, cond func(*store.Node) bool) *store.Node {
	t.Helper()
	var last *store.Node
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, err := f.db.GetNode(context.Background(), f.nsID, f.node.ID)
		if err != nil {
			t.Fatalf("get node: %v", err)
		}
		last = n
		if cond(n) {
			return n
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待节点状态超时，最后一次读到: %+v", last)
	return nil
}

// waitFor 轮询一个纯内存条件（日志/计数器），超时即失败。
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时", what)
}

// captureHandler 是线程安全的 slog.Handler（测试要读日志）。
//
// 用自定义 handler 而不是 slog.NewTextHandler(bytes.Buffer)：Buffer 没有并发保护，
// handler goroutine 写、测试 goroutine 读是真实的数据竞争（-race 下会红）。
type captureHandler struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(&h.buf, "level=%s msg=%q", r.Level, r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&h.buf, " %s=%v", a.Key, a.Value)
		return true
	})
	h.buf.WriteString("\n")
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.buf.String()
}

// installCaptureLogger 把默认 logger 换成捕获器（用例结束自动还原）。
func installCaptureLogger(t *testing.T) *captureHandler {
	t.Helper()
	h := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}
