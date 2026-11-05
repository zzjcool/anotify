package server

// 本文件是 agent 自升级的**中枢 HTTP 面**验收（CONTRACT-agent-upgrade §2 / §5 / §5.1）：
//
//	GET  /v1/node/binary          node token 鉴权 + 二进制原文 + 两个版本头 + 501
//	POST /v1/nodes/{id}/upgrade   会话鉴权 + 命名空间隔离 + 409/501/up_to_date/sent
//	GET  /v1/nodes                targetAgentVersion + 每台节点的 agentUpgrade（§5.1）
//
// WS 那一侧（帧内容、ack 落库）在 internal/ws/node_upgrade_test.go；
// 这里用假 publisher 覆盖「推没推、推了什么」，不起真连接。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/ws"
)

// fakeUpgradePublisher 记录「端点有没有推帧、推了什么」。
type fakeUpgradePublisher struct {
	online    bool
	err       error
	calls     int
	namespace string
	nodeID    string
	frame     *ws.UpgradeFrame
}

func (f *fakeUpgradePublisher) Online(string) bool { return f.online }

func (f *fakeUpgradePublisher) PublishUpgrade(namespaceID, nodeID string, frame *ws.UpgradeFrame) error {
	f.calls++
	f.namespace, f.nodeID, f.frame = namespaceID, nodeID, frame
	return f.err
}

// upgradeFixture 是一套跑起来的升级环境（真 DB + 真 HTTP，假节点连接）。
type upgradeFixture struct {
	env       *consoleEnv
	mux       *http.ServeMux
	pub       *fakeUpgradePublisher
	bin       *agentBinary
	handler   *nodeUpgradeHandler
	node      *store.Node
	nodeToken string
	// target 是中枢自带的 agent 版本（= 升级目标版本）。
	target string
}

// newUpgradeFixture 建库/会话/节点 + 一个真的 agent 二进制文件（内容已知，便于比哈希）。
//
// binEnabled=false 时给一个**不存在**的路径：模拟「服务端没带二进制」的部署。
func newUpgradeFixture(t *testing.T, binEnabled bool) *upgradeFixture {
	t.Helper()
	env := newConsoleEnv(t)
	f := &upgradeFixture{env: env, target: "9.9.9+test"}

	binPath := filepath.Join(t.TempDir(), "nonexistent-agent")
	if binEnabled {
		dir := t.TempDir()
		binPath = filepath.Join(dir, "agent")
		if err := os.WriteFile(binPath, []byte("#!/bin/sh\necho anotify-agent-test\n"), 0o755); err != nil {
			t.Fatalf("write agent binary: %v", err)
		}
	}
	f.bin = loadAgentBinary(binPath, f.target)
	if f.bin.enabled() != binEnabled {
		t.Fatalf("loadAgentBinary enabled = %v, want %v", f.bin.enabled(), binEnabled)
	}

	f.pub = &fakeUpgradePublisher{online: true}
	consoleH := &consoleHandler{
		db: env.db, auth: env.svc,
		// 与生产 mux 的接线一致：顶层 targetAgentVersion 来自同一个 agentBinary。
		targetAgentVersion: f.bin.targetVersion(),
	}
	f.handler = &nodeUpgradeHandler{db: env.db, console: consoleH, hub: f.pub, bin: f.bin}

	sessMW := env.svc.Sessions().Middleware
	f.mux = http.NewServeMux()
	f.mux.Handle("GET /v1/node/binary", noStore(&agentBinaryHandler{bin: f.bin, auth: env.db}))
	f.mux.Handle("GET /v1/nodes", noStore(sessMW(http.HandlerFunc(consoleH.nodes))))
	f.mux.Handle("POST /v1/nodes/{id}/upgrade", noStore(sessMW(http.HandlerFunc(f.handler.upgrade))))

	node := &store.Node{NamespaceID: env.nsID, Name: "upgrade-node"}
	token, err := env.db.CreateNode(context.Background(), node)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	f.node, f.nodeToken = node, token
	return f
}

// do 发一个带会话 Cookie 的请求。
func (f *upgradeFixture) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.AddCookie(f.env.cookie)
	rr := httptest.NewRecorder()
	f.mux.ServeHTTP(rr, req)
	return rr
}

// doBinary 发一个 /v1/node/binary 请求，Authorization 由调用方给（"" = 不带）。
func (f *upgradeFixture) doBinary(t *testing.T, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/node/binary", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rr := httptest.NewRecorder()
	f.mux.ServeHTTP(rr, req)
	return rr
}

// ---------- GET /v1/node/binary ----------

// TestAgentBinaryEndpoint 二进制下载端点：鉴权（node token，不是会话）+ 头 + 原文。
func TestAgentBinaryEndpoint(t *testing.T) {
	f := newUpgradeFixture(t, true)
	wantBody, err := os.ReadFile(f.bin.path)
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}
	sum := sha256.Sum256(wantBody)
	wantSHA := hex.EncodeToString(sum[:])

	t.Run("未登录 → 401", func(t *testing.T) {
		rr := f.doBinary(t, "")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rr.Code)
		}
		if got := decode(t, rr)["error"]; got != "missing_node_token" {
			t.Fatalf("error = %v, want missing_node_token", got)
		}
	})

	t.Run("会话令牌不行 → 403", func(t *testing.T) {
		// 会话 id 是浏览器 Cookie 里的凭据，不是节点令牌：拿它当 Bearer 必须被拒。
		// （这正是「节点端点和控制台端点用两套鉴权」的护栏。）
		rr := f.doBinary(t, "Bearer "+f.env.cookie.Value)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403（会话令牌不能当 node token）", rr.Code)
		}
	})

	t.Run("node token → 200 + 原文 + 两个版本头", func(t *testing.T) {
		rr := f.doBinary(t, "Bearer "+f.nodeToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get("Content-Type"); got != "application/octet-stream" {
			t.Errorf("Content-Type = %q, want application/octet-stream", got)
		}
		if got := rr.Header().Get("X-Agent-Version"); got != f.target {
			t.Errorf("X-Agent-Version = %q, want %q", got, f.target)
		}
		// sha256 头必须等于**实际 body** 的哈希（不是文件名的哈希、不是旧快照的）。
		if got := rr.Header().Get("X-Agent-Sha256"); got != wantSHA {
			t.Errorf("X-Agent-Sha256 = %q, want %q", got, wantSHA)
		}
		if !bytes.Equal(rr.Body.Bytes(), wantBody) {
			t.Fatalf("body 与磁盘上的二进制不一致（%d vs %d bytes）", rr.Body.Len(), len(wantBody))
		}
		if got := rr.Header().Get("Content-Length"); got != "" && got != strconv.Itoa(len(wantBody)) {
			t.Errorf("Content-Length = %q, want %d", got, len(wantBody))
		}
	})

	t.Run("排空中的节点仍可下载（维护动作不是接新任务）", func(t *testing.T) {
		if err := f.env.db.UpdateNodeStatus(context.Background(), f.env.nsID, f.node.ID, store.NodeStatusDraining); err != nil {
			t.Fatalf("drain node: %v", err)
		}
		rr := f.doBinary(t, "Bearer "+f.nodeToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（draining 不该挡升级）", rr.Code)
		}
	})
}

// TestAgentBinaryEndpointUnavailable 服务端没带二进制 → 501（明确语义，不是 404）。
func TestAgentBinaryEndpointUnavailable(t *testing.T) {
	f := newUpgradeFixture(t, false)

	rr := f.doBinary(t, "Bearer "+f.nodeToken)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rr.Code)
	}
	if got := decode(t, rr)["error"]; got != "agent_binary_unavailable" {
		t.Fatalf("error = %v, want agent_binary_unavailable", got)
	}
	// 未配置时**也要**先鉴权：不能把「服务端没带二进制」当成公开信息。
	rr = f.doBinary(t, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌 status = %d, want 401（鉴权先于配置探测）", rr.Code)
	}
}

// ---------- POST /v1/nodes/{id}/upgrade ----------

// TestNodeUpgradeEndpointGuards 判定顺序与两种拒绝：离线 409 / 无二进制 501。
func TestNodeUpgradeEndpointGuards(t *testing.T) {
	t.Run("离线 → 409 node_offline", func(t *testing.T) {
		f := newUpgradeFixture(t, true)
		f.pub.online = false
		rr := f.do(t, http.MethodPost, "/v1/nodes/"+f.node.ID+"/upgrade?namespaceId="+f.env.nsID, nil)
		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409（body=%s）", rr.Code, rr.Body.String())
		}
		if got := decode(t, rr)["error"]; got != "node_offline" {
			t.Fatalf("error = %v, want node_offline", got)
		}
		if f.pub.calls != 0 {
			t.Fatalf("离线时推了 %d 帧，want 0", f.pub.calls)
		}
	})

	t.Run("服务端无二进制 → 501", func(t *testing.T) {
		f := newUpgradeFixture(t, false)
		rr := f.do(t, http.MethodPost, "/v1/nodes/"+f.node.ID+"/upgrade?namespaceId="+f.env.nsID, nil)
		if rr.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501（body=%s）", rr.Code, rr.Body.String())
		}
		if got := decode(t, rr)["error"]; got != "agent_binary_unavailable" {
			t.Fatalf("error = %v, want agent_binary_unavailable", got)
		}
		if f.pub.calls != 0 {
			t.Fatalf("没有二进制时推了 %d 帧，want 0", f.pub.calls)
		}
	})

	t.Run("推帧时节点恰好掉线 → 409", func(t *testing.T) {
		f := newUpgradeFixture(t, true)
		f.pub.err = ws.ErrNodeOffline
		rr := f.do(t, http.MethodPost, "/v1/nodes/"+f.node.ID+"/upgrade?namespaceId="+f.env.nsID, nil)
		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409（body=%s）", rr.Code, rr.Body.String())
		}
	})
}

// TestNodeUpgradeEndpointDispatch 目标版本判定 + 推帧内容 + sent 落库（§5 / §5.1）。
func TestNodeUpgradeEndpointDispatch(t *testing.T) {
	t.Run("同版本且未 force → up_to_date，不推帧", func(t *testing.T) {
		f := newUpgradeFixture(t, true)
		setAgentVersion(t, f, f.target, "")
		rr := f.do(t, http.MethodPost, "/v1/nodes/"+f.node.ID+"/upgrade?namespaceId="+f.env.nsID, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
		}
		body := decode(t, rr)
		if body["status"] != "up_to_date" || body["targetVersion"] != f.target {
			t.Fatalf("响应 = %v, want {status:up_to_date, targetVersion:%s}", body, f.target)
		}
		if f.pub.calls != 0 {
			t.Fatalf("已有最新版本时推了 %d 帧，want 0", f.pub.calls)
		}
	})

	t.Run("force 越过同版本判断 → sent（body.force）", func(t *testing.T) {
		f := newUpgradeFixture(t, true)
		setAgentVersion(t, f, f.target, "")
		rr := f.do(t, http.MethodPost, "/v1/nodes/"+f.node.ID+"/upgrade?namespaceId="+f.env.nsID,
			map[string]any{"force": true})
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
		}
		body := decode(t, rr)
		if body["status"] != "sent" || body["targetVersion"] != f.target {
			t.Fatalf("响应 = %v, want {status:sent, targetVersion:%s}", body, f.target)
		}
		assertUpgradeFrame(t, f)
	})

	t.Run("force 也可用 query 表达", func(t *testing.T) {
		f := newUpgradeFixture(t, true)
		setAgentVersion(t, f, f.target, "")
		rr := f.do(t, http.MethodPost, "/v1/nodes/"+f.node.ID+"/upgrade?namespaceId="+f.env.nsID+"&force=1", nil)
		if rr.Code != http.StatusOK || decode(t, rr)["status"] != "sent" {
			t.Fatalf("status = %d body=%s, want 200 sent", rr.Code, rr.Body.String())
		}
		assertUpgradeFrame(t, f)
	})

	t.Run("版本不同（老 agent）→ sent", func(t *testing.T) {
		f := newUpgradeFixture(t, true)
		setAgentVersion(t, f, "0.9.0+old", "")
		rr := f.do(t, http.MethodPost, "/v1/nodes/"+f.node.ID+"/upgrade?namespaceId="+f.env.nsID, nil)
		if rr.Code != http.StatusOK || decode(t, rr)["status"] != "sent" {
			t.Fatalf("status = %d body=%s, want 200 sent", rr.Code, rr.Body.String())
		}
		assertUpgradeFrame(t, f)
	})

	t.Run("未知 agent 版本（空串）→ sent", func(t *testing.T) {
		f := newUpgradeFixture(t, true)
		rr := f.do(t, http.MethodPost, "/v1/nodes/"+f.node.ID+"/upgrade?namespaceId="+f.env.nsID, nil)
		if rr.Code != http.StatusOK || decode(t, rr)["status"] != "sent" {
			t.Fatalf("status = %d body=%s, want 200 sent", rr.Code, rr.Body.String())
		}
	})
}

// TestNodeUpgradeEndpointAuthAndScope 会话鉴权 + 命名空间隔离 + 缺参。
func TestNodeUpgradeEndpointAuthAndScope(t *testing.T) {
	f := newUpgradeFixture(t, true)

	t.Run("无会话 → 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost,
			"/v1/nodes/"+f.node.ID+"/upgrade?namespaceId="+f.env.nsID, nil)
		rr := httptest.NewRecorder()
		f.mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rr.Code)
		}
	})

	t.Run("缺 namespaceId → 400", func(t *testing.T) {
		rr := f.do(t, http.MethodPost, "/v1/nodes/"+f.node.ID+"/upgrade", nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400（body=%s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("别的命名空间查询这台节点 → 404", func(t *testing.T) {
		other := f.env.seedNamespace(t, "upgrade-other", "升级隔离")
		rr := f.do(t, http.MethodPost, "/v1/nodes/"+f.node.ID+"/upgrade?namespaceId="+other.ID, nil)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404（隔离边界）", rr.Code)
		}
		if f.pub.calls != 0 {
			t.Fatalf("跨命名空间推了 %d 帧，want 0", f.pub.calls)
		}
	})

	t.Run("不存在的节点 → 404", func(t *testing.T) {
		rr := f.do(t, http.MethodPost, "/v1/nodes/nd_ghost/upgrade?namespaceId="+f.env.nsID, nil)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rr.Code)
		}
	})
}

// assertUpgradeFrame 断言 fake publisher 收到了契约 §5 的帧，且 sent 已落库（§5.1）。
func assertUpgradeFrame(t *testing.T, f *upgradeFixture) {
	t.Helper()
	if f.pub.calls != 1 {
		t.Fatalf("推帧次数 = %d, want 1", f.pub.calls)
	}
	if f.pub.namespace != f.env.nsID || f.pub.nodeID != f.node.ID {
		t.Fatalf("推给了 (%s,%s), want (%s,%s)",
			f.pub.namespace, f.pub.nodeID, f.env.nsID, f.node.ID)
	}
	fr := f.pub.frame
	if fr == nil || fr.Type != ws.NodeFrameUpgrade {
		t.Fatalf("帧类型 = %+v, want upgrade", fr)
	}
	if fr.TargetVersion != f.target {
		t.Errorf("target_version = %q, want %q", fr.TargetVersion, f.target)
	}
	if fr.SHA256 != f.bin.sha256 || len(fr.SHA256) != 64 {
		t.Errorf("sha256 = %q, want 64 位 hex（%s）", fr.SHA256, f.bin.sha256)
	}
	if fr.URLPath != ws.NodeBinaryPath {
		t.Errorf("url_path = %q, want %q", fr.URLPath, ws.NodeBinaryPath)
	}

	// sent 落库：控制台刷新后要能看到「已经下发、还在等 ack」。
	got, err := f.env.db.GetNode(context.Background(), f.env.nsID, f.node.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.AgentUpgrade.Status != store.AgentUpgradeSent {
		t.Fatalf("agentUpgrade.status = %q, want sent", got.AgentUpgrade.Status)
	}
	if got.AgentUpgrade.TargetVersion != f.target || got.AgentUpgrade.At <= 0 {
		t.Fatalf("agentUpgrade = %+v, want targetVersion=%s 且 at>0", got.AgentUpgrade, f.target)
	}
}

// ---------- GET /v1/nodes 的读侧（§5.1） ----------

// TestNodesListAgentUpgradeFields 顶层 targetAgentVersion + 每台节点的 agentVersion /
// agentCommit / agentUpgrade（**恒存在**，默认四键齐全）。
func TestNodesListAgentUpgradeFields(t *testing.T) {
	f := newUpgradeFixture(t, true)
	ctx := context.Background()

	// 两台节点：一台什么都没试过（默认对象），一台有 sent 状态。
	fresh := &store.Node{NamespaceID: f.env.nsID, Name: "fresh-node"}
	if _, err := f.env.db.CreateNode(ctx, fresh); err != nil {
		t.Fatalf("create fresh node: %v", err)
	}
	setAgentVersion(t, f, "1.4.0+old", "oldsha")
	if err := f.env.db.MarkAgentUpgradeSent(ctx, f.env.nsID, f.node.ID, f.target, 1762329600); err != nil {
		t.Fatalf("mark sent: %v", err)
	}

	rr := f.do(t, http.MethodGet, "/v1/nodes?namespaceId="+f.env.nsID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
	}
	var body struct {
		TargetAgentVersion *string `json:"targetAgentVersion"`
		Nodes              []struct {
			ID           string          `json:"id"`
			AgentVersion string          `json:"agentVersion"`
			AgentCommit  string          `json:"agentCommit"`
			AgentUpgrade json.RawMessage `json:"agentUpgrade"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v（body=%s）", err, rr.Body.String())
	}
	if body.TargetAgentVersion == nil {
		t.Fatal("响应顶层缺 targetAgentVersion（§5.1 冻结字段）")
	}
	if *body.TargetAgentVersion != f.target {
		t.Fatalf("targetAgentVersion = %q, want %q", *body.TargetAgentVersion, f.target)
	}
	if len(body.Nodes) != 2 {
		t.Fatalf("nodes 数 = %d, want 2", len(body.Nodes))
	}
	byID := map[string]int{}
	for i := range body.Nodes {
		byID[body.Nodes[i].ID] = i
	}
	// ① 有 sent 的那台：三个字段都要有值 / 状态。
	upd := body.Nodes[byID[f.node.ID]]
	if upd.AgentVersion != "1.4.0+old" || upd.AgentCommit != "oldsha" {
		t.Fatalf("agentVersion/agentCommit = (%q,%q), want (1.4.0+old, oldsha)",
			upd.AgentVersion, upd.AgentCommit)
	}
	wantUpgrade := map[string]any{
		"status": "sent", "targetVersion": f.target, "error": "", "at": float64(1762329600),
	}
	assertJSONObject(t, upd.AgentUpgrade, wantUpgrade)
	// ② 从未尝试的那台：**默认对象**，四个键一个都不能少（UI 直接读 status/error）。
	freshView := body.Nodes[byID[fresh.ID]]
	assertJSONObject(t, freshView.AgentUpgrade, map[string]any{
		"status": "", "targetVersion": "", "error": "", "at": float64(0),
	})
	if freshView.AgentVersion != "" || freshView.AgentCommit != "" {
		t.Fatalf("新节点 agentVersion/agentCommit = (%q,%q), want 空串",
			freshView.AgentVersion, freshView.AgentCommit)
	}
}

// TestNodesListTargetAgentVersionEmptyWithoutBinary 服务端没带二进制 → 顶层空串
// （UI 据此不显示升级按钮）。
func TestNodesListTargetAgentVersionEmptyWithoutBinary(t *testing.T) {
	f := newUpgradeFixture(t, false)

	rr := f.do(t, http.MethodGet, "/v1/nodes?namespaceId="+f.env.nsID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, ok := body["targetAgentVersion"]
	if !ok {
		t.Fatal("响应顶层缺 targetAgentVersion（空也要出现）")
	}
	if got != "" {
		t.Fatalf("targetAgentVersion = %v, want 空串", got)
	}
}

// ---------- 小工具 ----------

func setAgentVersion(t *testing.T, f *upgradeFixture, version, commit string) {
	t.Helper()
	ctx := context.Background()
	if err := f.env.db.UpdateNodeAgentVersion(ctx, f.env.nsID, f.node.ID, version); err != nil {
		t.Fatalf("set agent version: %v", err)
	}
	if commit != "" {
		if err := f.env.db.RecordAgentUpgrade(ctx, f.env.nsID, f.node.ID, version, commit, 1); err != nil {
			t.Fatalf("set agent commit: %v", err)
		}
	}
}

// assertJSONObject 断言一个 JSON 对象的**键集合与值**完全等于 want（不多不少）。
func assertJSONObject(t *testing.T, raw json.RawMessage, want map[string]any) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode agentUpgrade %s: %v", raw, err)
	}
	if len(got) != len(want) {
		t.Fatalf("agentUpgrade 键集合 = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("agentUpgrade[%q] = %v, want %v", k, got[k], v)
		}
	}
}
