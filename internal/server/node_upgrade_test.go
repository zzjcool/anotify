package server

// 本文件是 agent 自升级在 **v4 HTTP 面** 的验收（CONTRACT-agent-upgrade §2 / api-conventions §8.2）：
//
//	GET  /v1/node/binary            node token 鉴权 + 二进制原文 + 两个版本头 + 501
//	POST /v1/nodes/{name}/upgrade   会话鉴权 + 节点不存在 404 + 无升级源 501 + 节点离线 409
//
// v3 的重构（写在这里，避免下一个人以为漏了半个套件）：
//
//   - **帧内容不再在本包断言**：v3 用 fakeUpgradePublisher 记录 `ws.UpgradeFrame` 来钉
//     「推了什么」。v4 的帧类型搬进共享协议包（`protocol.UpgradeFrame`），下发通道是
//     `ws.Hub.RequestUpgrade`，帧/ack 的端到端覆盖在 `internal/ws/ws_test.go`
//     （TestUpgradeFrameAndAck）。本包只钉 HTTP 面的状态码与响应体。
//   - **fakeUpgradePublisher 已删**：`serverDeps.Hub` 是具体的 `*ws.Hub`（冻结骨架），
//     没有注入点；用「真 hub + 没有连接」表达「离线」，比再造一个接口更接近线上。
//   - **升级端点不再有本包自己的 handler**：它由 Node 资源域实现（resource_node.go 的
//     nodeAPI.upgradeNode，路径 `/v1/nodes/{name}/upgrade`），本文件通过**真实装配**
//     （`newNodeAPI(deps)` + `setUpgradeSource` + `register`）覆盖它。本包的
//     node_upgrade_handlers.go 只提供升级源适配器（agentUpgradeSource）。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/ws"
)

// upgradeFixture 是一套跑起来的升级环境（真 DB + 真会话 + 真 hub + 真路由）。
type upgradeFixture struct {
	db     *store.DB
	bin    *agentBinary
	mux    *http.ServeMux
	cookie *http.Cookie
	node   *store.Node
	token  string
	target string
}

// newUpgradeFixture 建库 / 会话 / 节点 + 一个内容已知的 agent 二进制文件。
//
// binEnabled=false → 给一个**不存在**的路径：模拟「服务端没带二进制」的部署。
func newUpgradeFixture(t *testing.T, binEnabled bool) *upgradeFixture {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	svc, err := auth.NewService(db, auth.Config{
		RPDisplayName: "Anotify Test",
		RPID:          "localhost",
		RPOrigins:     []string{"http://localhost"},
		SessionTTL:    time.Hour,
	})
	if err != nil {
		t.Fatalf("new auth service: %v", err)
	}
	uid, err := db.InsertUser(ctx, "", "upgrade-user-"+store.NewUserID(), "Upgrade User")
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	sess, err := svc.Sessions().Create(uid, "Upgrade · test")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	f := &upgradeFixture{
		db:     db,
		target: "9.9.9+test",
		cookie: &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID},
	}

	binPath := filepath.Join(t.TempDir(), "nonexistent-agent")
	if binEnabled {
		binPath = filepath.Join(t.TempDir(), "agent")
		if err := os.WriteFile(binPath, []byte("#!/bin/sh\necho anotify-agent-test\n"), 0o755); err != nil {
			t.Fatalf("write agent binary: %v", err)
		}
	}
	f.bin = loadAgentBinary(binPath, f.target)
	if f.bin.enabled() != binEnabled {
		t.Fatalf("loadAgentBinary enabled = %v, want %v", f.bin.enabled(), binEnabled)
	}

	node := &store.Node{Name: "upgrade-node-" + store.NewUserID()}
	token, err := db.CreateNode(ctx, node)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	f.node, f.token = node, token

	// 真 hub（一个会话都没有 → Online=false）。会话中间件用真的 auth.Service：
	// 「未登录 401」这条断言必须来自生产同一条中间件，而不是测试里的假守卫。
	deps := &serverDeps{
		DB: db, Router: route.New(db), Hub: ws.NewHub(db, route.New(db)),
		Log: slog.Default(), Now: store.Now, Session: svc.Sessions().Middleware,
	}

	f.mux = http.NewServeMux()
	f.mux.Handle("GET /v1/node/binary", noStore(&agentBinaryHandler{bin: f.bin, db: db}))
	api := newNodeAPI(deps)
	api.setUpgradeSource(newAgentUpgradeSource(f.bin))
	api.register(f.mux)
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
	req.AddCookie(f.cookie)
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

// decodeJSON 把响应体解成 map（错误体要看 reason 字段）。
func decodeJSON(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body %q: %v", rr.Body.String(), err)
	}
	return out
}

// reasonOf 读 Status.reason（v4 的错误体统一形状）。
func reasonOf(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	body := decodeJSON(t, rr)
	if body["kind"] != kindStatus {
		t.Fatalf("错误体 kind = %v, want %s（body=%s）", body["kind"], kindStatus, rr.Body.String())
	}
	reason, _ := body["reason"].(string)
	return reason
}

// ---------- GET /v1/node/binary ----------

// TestAgentBinaryEndpoint 二进制下载端点：node token 鉴权（不是会话）+ 头 + 原文 + 501。
func TestAgentBinaryEndpoint(t *testing.T) {
	f := newUpgradeFixture(t, true)
	wantBody, err := os.ReadFile(f.bin.path)
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}
	sum := sha256.Sum256(wantBody)
	wantSHA := hex.EncodeToString(sum[:])

	t.Run("没带令牌 → 401", func(t *testing.T) {
		rr := f.doBinary(t, "")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401（body=%s）", rr.Code, rr.Body.String())
		}
		if got := reasonOf(t, rr); got != reasonUnauthorized {
			t.Fatalf("reason = %q, want %q", got, reasonUnauthorized)
		}
	})

	t.Run("错的令牌 → 401", func(t *testing.T) {
		rr := f.doBinary(t, "Bearer ant_not_a_node_token")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401（body=%s）", rr.Code, rr.Body.String())
		}
		if got := reasonOf(t, rr); got != reasonUnauthorized {
			t.Fatalf("reason = %q, want %q", got, reasonUnauthorized)
		}
	})

	t.Run("会话令牌不行 → 401", func(t *testing.T) {
		// 会话 id 是浏览器 Cookie 里的凭据，不是节点令牌：拿它当 Bearer 必须被拒。
		// （这正是「节点端点和控制台端点用两套鉴权」的护栏。）
		//
		// v3 这里回 403（「令牌有效但没权限」）；v4 统一 401：会话 id 根本不是
		// node token，说 403 会把调用方指向「去申请权限」这个错误方向。
		rr := f.doBinary(t, "Bearer "+f.cookie.Value)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401（会话令牌不能当 node token）", rr.Code)
		}
	})

	t.Run("node token → 200 + 原文 + 两个版本头", func(t *testing.T) {
		rr := f.doBinary(t, "Bearer "+f.token)
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
		if err := f.db.UpdateNodeStatus(context.Background(), f.node.ID, store.NodeStatusDraining); err != nil {
			t.Fatalf("drain node: %v", err)
		}
		rr := f.doBinary(t, "Bearer "+f.token)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（draining 不该挡升级）", rr.Code)
		}
	})
}

// TestAgentBinaryEndpointUnavailable 服务端没带二进制 → 501（明确语义，不是 404）。
func TestAgentBinaryEndpointUnavailable(t *testing.T) {
	f := newUpgradeFixture(t, false)

	rr := f.doBinary(t, "Bearer "+f.token)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501（body=%s）", rr.Code, rr.Body.String())
	}
	if got := reasonOf(t, rr); got != reasonAgentBinaryUnavailable {
		t.Fatalf("reason = %q, want %q", got, reasonAgentBinaryUnavailable)
	}
	// 未配置时**也要**先鉴权：不能把「服务端没带二进制」当成公开信息。
	rr = f.doBinary(t, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌 status = %d, want 401（鉴权先于配置探测）", rr.Code)
	}
}

// ---------- POST /v1/nodes/{name}/upgrade ----------

// TestNodeUpgradeEndpointGuards 四类拒绝：未登录 401 / 不存在 404 / 无升级源 501 / 离线 409。
func TestNodeUpgradeEndpointGuards(t *testing.T) {
	t.Run("无会话 → 401", func(t *testing.T) {
		f := newUpgradeFixture(t, true)
		req := httptest.NewRequest(http.MethodPost, "/v1/nodes/"+f.node.Name+"/upgrade", nil)
		rr := httptest.NewRecorder()
		f.mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401（body=%s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("不存在的节点 → 404", func(t *testing.T) {
		f := newUpgradeFixture(t, true)
		rr := f.do(t, http.MethodPost, "/v1/nodes/nd_ghost/upgrade", nil)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404（body=%s）", rr.Code, rr.Body.String())
		}
		if got := reasonOf(t, rr); got != reasonNotFound {
			t.Fatalf("reason = %q, want %q", got, reasonNotFound)
		}
	})

	t.Run("服务端无二进制 → 501", func(t *testing.T) {
		f := newUpgradeFixture(t, false)
		rr := f.do(t, http.MethodPost, "/v1/nodes/"+f.node.Name+"/upgrade", nil)
		if rr.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501（body=%s）", rr.Code, rr.Body.String())
		}
		if got := reasonOf(t, rr); got != reasonAgentBinaryUnavailable {
			t.Fatalf("reason = %q, want %q", got, reasonAgentBinaryUnavailable)
		}
	})

	t.Run("节点离线 → 409", func(t *testing.T) {
		// hub 是真的、但没有会话：Online=false。选 409（不是 503）的理由：
		// 「没连接」是**目标节点的状态**（换台节点/等它上线就能做），而 503 会读成
		// 「中枢过载，退避重试」——两者对调用方的下一步动作完全不同。
		f := newUpgradeFixture(t, true)
		rr := f.do(t, http.MethodPost, "/v1/nodes/"+f.node.Name+"/upgrade", nil)
		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409（body=%s）", rr.Code, rr.Body.String())
		}
		if got := reasonOf(t, rr); got != "node_offline" {
			t.Fatalf("reason = %q, want node_offline", got)
		}
	})
}

// TestAgentUpgradeSourceUnwrappedWhenDisabled 钉住适配器的 nil 语义：
//
// 「没带二进制」必须在**接口层面**是 nil（走 501 分支），而不是一个包着 nil
// *agentBinary 的非 nil 接口 —— 后者会让 `a.upgrade != nil` 为真，501 落到
// 「agent binary is not loaded」那条更模糊的同码分支上（现象一样，排查方向差很远）。
func TestAgentUpgradeSourceUnwrappedWhenDisabled(t *testing.T) {
	disabled := loadAgentBinary(filepath.Join(t.TempDir(), "missing"), "1.0.0")
	if src := newAgentUpgradeSource(disabled); src != nil {
		t.Fatalf("未启用时 newAgentUpgradeSource = %#v, want nil 接口", src)
	}
	if src := newAgentUpgradeSource(nil); src != nil {
		t.Fatalf("nil binary 时 newAgentUpgradeSource = %#v, want nil 接口", src)
	}

	enabled := newUpgradeFixture(t, true)
	src := newAgentUpgradeSource(enabled.bin)
	if src == nil {
		t.Fatal("启用时 newAgentUpgradeSource 不该是 nil")
	}
	if src.TargetVersion() != enabled.target {
		t.Fatalf("TargetVersion = %q, want %q", src.TargetVersion(), enabled.target)
	}
	if len(src.SHA256()) != 64 {
		t.Fatalf("SHA256 = %q, want 64 位十六进制", src.SHA256())
	}
}
