package server

// 本文件是 api-conventions §8.2 **内容通道**（能力包体下行）的验收：
//
//	GET /v1/node/capabilities/{digest}   Authorization: Bearer <node token>
//
// 为什么用**真实装配**（newProductionAppForTest）而不是自己拼一个小 mux：
// 这条端点最容易坏的地方不在 handler 里，而在接线 —— 它必须挂在资源域**之外**
// （域内 register 一律包 session 中间件，节点请求会被 401），并且带 noStore。用手搭的
// mux 测，这两条都得由测试自己声明一遍，等于把被测事实抄成断言（真接错线也照样绿）。
//
// 数据全部是**真事实**：真 SQLite（namespaces / capability_versions / deployments）、
// 真节点身份（CreateNode 发的明文 token + 库里的 sha256）、真部署意图
// （CreateDeployments 写行）。内容字节也走 store 的真拼装（canonical JSON），
// 断言里再按**节点侧的算法**（artifact.Decode）重算一次 digest —— 那正是节点拿到字节
// 之后做的第一件事（cmd/anode/internal/capability 的 Fetch）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/store"
)

// nodeCapabilityContentPathPrefix 是节点侧拼 URL 用的同一前缀
// （cmd/anode/internal/capability 的 ArtifactPathPrefix）。
//
// 这里刻意抄一份字面量而不是 import 节点的常量：cmd/anode/internal/** 按 Go 的
// internal 规则不可被 internal/server 引用，而「路径前缀一致」正是本用例要钉的事实
// 之一——抄成常量 + 在断言里逐字比对，漂移会被立刻发现。
const nodeCapabilityContentPathPrefix = "/v1/node/capabilities/"

// nodeCapabilityEnv 是一套跑起来的内容通道环境。
type nodeCapabilityEnv struct {
	t   *testing.T
	app *App
	db  *store.DB

	nsA *store.Namespace // 用户注册时的 `default`
	nsB *store.Namespace // "cap-b"

	// A 里的三类内容：
	//   shared  —— 与 B 里的同名同版本内容**同字节**（同一 digest 存在于两个 ns）
	//   other   —— 只在 A，且没有任何节点被部署
	//   onlyA   —— 只在 A，部署给 nodeAOnly（授权收窄用例）
	capSharedA *store.CapabilityVersion
	capOtherA  *store.CapabilityVersion
	capOnlyA   *store.CapabilityVersion
	// B 里的内容：shared（与 A 同 digest）+ onlyB
	capSharedB *store.CapabilityVersion
	capOnlyB   *store.CapabilityVersion

	// nodeAll：申报 ["*"] + 授权 ["*"]（生效 ns = 全部）
	nodeAll  *store.Node
	tokenAll string
	// nodeB：申报 ["*"] + 授权 ["cap-b"]（只看得到 B）
	nodeB  *store.Node
	tokenB string
	// nodeAOnly：申报 ["*"] + 授权 ["default"]（只看得到 A）
	nodeAOnly  *store.Node
	tokenAOnly string

	// deploySharedA / deployOnlyA / deployOnlyB 是各自部署行的 id（状态流转用例要用）。
	deploySharedA map[string]string // nodeID → deploymentID
	deployOnlyA   map[string]string
	deployOnlyB   map[string]string
}

// newNodeCapabilityEnv 装配真库 + 两个 ns + 五个能力 + 三台节点 + 三批部署。
func newNodeCapabilityEnv(t *testing.T) *nodeCapabilityEnv {
	t.Helper()
	ctx := context.Background()
	app := newProductionAppForTest(t)
	db := app.DB

	user := &store.User{ID: store.NewUserID(), Username: "node-cap-" + store.NewUserID(), CreatedAt: store.Now()}
	nsA, err := db.RegisterUser(ctx, user)
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	nsB, err := db.CreateNamespace(ctx, "cap-b", "能力内容 B")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	env := &nodeCapabilityEnv{t: t, app: app, db: db, nsA: nsA, nsB: nsB}

	// 内容夹具：params 显式声明成 object（与 artifact.Normalize 的默认一致），
	// run.files 里带一段可识别的正文（用来断言「完整 run.files 下发，而不是被裁掉」）。
	sharedSpec := artifact.Spec{
		Params: []byte(`{"type":"object"}`),
		Run: artifact.Run{
			Entrypoint: "run.sh",
			Files:      map[string]string{"run.sh": "#!/bin/sh\necho shared-body\n"},
		},
	}
	otherSpec := artifact.Spec{
		Params: []byte(`{"type":"object"}`),
		Run: artifact.Run{
			Entrypoint: "other.sh",
			Files:      map[string]string{"other.sh": "#!/bin/sh\necho other-body\n"},
		},
	}
	onlyASpec := artifact.Spec{
		Params: []byte(`{"type":"object"}`),
		Run: artifact.Run{
			Entrypoint: "only-a.sh",
			Files:      map[string]string{"only-a.sh": "#!/bin/sh\necho only-a-body\n"},
		},
	}
	onlyBSpec := artifact.Spec{
		Params: []byte(`{"type":"object"}`),
		Run: artifact.Run{
			Entrypoint: "only-b.sh",
			Files:      map[string]string{"only-b.sh": "#!/bin/sh\necho only-b-body\n"},
		},
	}

	env.capSharedA = env.createCapability(nsA.ID, "shared", sharedSpec)
	env.capSharedB = env.createCapability(nsB.ID, "shared", sharedSpec)
	if env.capSharedA.Digest != env.capSharedB.Digest {
		// 内容寻址的前提：同一份内容在不同命名空间得到**同一个** digest。夹具若漂了，
		// 「同一 digest 多 ns」的用例就名不副实。
		t.Fatalf("同一内容在两个 ns 的 digest 不同：%s vs %s", env.capSharedA.Digest, env.capSharedB.Digest)
	}
	env.capOtherA = env.createCapability(nsA.ID, "other", otherSpec)
	env.capOnlyA = env.createCapability(nsA.ID, "only-a", onlyASpec)
	env.capOnlyB = env.createCapability(nsB.ID, "only-b", onlyBSpec)

	env.nodeAll, env.tokenAll = env.createNode("node-all-"+store.NewUserID(), "*")
	env.nodeB, env.tokenB = env.createNode("node-b-"+store.NewUserID(), "cap-b")
	env.nodeAOnly, env.tokenAOnly = env.createNode("node-a-"+store.NewUserID(), "default")

	// 部署意图（人工部署）：shared 在 A 部署给 nodeAll + nodeAOnly；only-a 部署给
	// nodeAOnly；only-b 在 B 部署给 nodeB。other 谁都不部署。
	//
	// 注意 nodeB **不能**被部署 A 的内容（CreateDeployments 会按申报 ∩ 授权拒绝）——
	// 这正是「同一 digest 多 ns」的判别点：nodeB 看得见 B 里的 shared，却没有给它的
	// 部署意图，所以必须 403；而 nodeAOnly 在 A 里有部署意图，所以 200。
	env.deploySharedA = env.deploy(nsA.ID, env.capSharedA, env.nodeAll, env.nodeAOnly)
	env.deployOnlyA = env.deploy(nsA.ID, env.capOnlyA, env.nodeAOnly)
	env.deployOnlyB = env.deploy(nsB.ID, env.capOnlyB, env.nodeB)
	return env
}

// createCapability 建一行能力（内容寻址：version 由 store 分配，digest 由内容算）。
func (e *nodeCapabilityEnv) createCapability(namespaceID, name string, spec artifact.Spec) *store.CapabilityVersion {
	e.t.Helper()
	c := &store.CapabilityVersion{NamespaceID: namespaceID, Name: name, Spec: spec}
	if err := e.db.CreateCapability(context.Background(), c); err != nil {
		e.t.Fatalf("create capability %s: %v", name, err)
	}
	if c.Digest == "" || c.ID == "" {
		e.t.Fatalf("create capability %s 没有回填 id/digest（%+v）", name, c)
	}
	return c
}

// createNode 建一台节点：申报 ["*"]，中枢授权是 authorizedNS（空串 = ["*"]）。
func (e *nodeCapabilityEnv) createNode(name, authorizedNS string) (*store.Node, string) {
	e.t.Helper()
	ctx := context.Background()
	n := &store.Node{Name: name, Status: store.NodeStatusOnline, DeclaredNamespaces: []string{"*"}}
	token, err := e.db.CreateNode(ctx, n)
	if err != nil {
		e.t.Fatalf("create node %s: %v", name, err)
	}
	if authorizedNS != "*" {
		cfg, cerr := e.db.GetNodeConfig(ctx, n.ID)
		if cerr != nil {
			e.t.Fatalf("get node config %s: %v", name, cerr)
		}
		cfg.Namespaces = []string{authorizedNS}
		if perr := e.db.PutNodeConfig(ctx, cfg); perr != nil {
			e.t.Fatalf("put node config %s: %v", name, perr)
		}
	}
	return n, token
}

// deploy 写部署意图并返回 nodeID → deploymentID。
func (e *nodeCapabilityEnv) deploy(namespaceID string, c *store.CapabilityVersion, nodes ...*store.Node) map[string]string {
	e.t.Helper()
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	rows, err := e.db.CreateDeployments(context.Background(), namespaceID, c.ID, c.Digest, ids)
	if err != nil {
		e.t.Fatalf("create deployments for %s: %v", c.Name, err)
	}
	if len(rows) != len(ids) {
		e.t.Fatalf("deploy %s 写了 %d 行，期望 %d 行", c.Name, len(rows), len(ids))
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.NodeID] = row.ID
	}
	return out
}

// contentURL 按**节点侧**的拼法构造请求路径（url.PathEscape，与 Fetch 同一行逻辑）。
func contentURL(digest string) string {
	return nodeCapabilityContentPathPrefix + url.PathEscape(digest)
}

// get 发一次内容通道请求（token 为空 = 不带 Authorization）。
func (e *nodeCapabilityEnv) get(digest, token string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(http.MethodGet, contentURL(digest), token)
}

// do 发任意请求（路径由调用方给）。
func (e *nodeCapabilityEnv) do(method, path, token string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	e.app.Handler.ServeHTTP(rr, req)
	return rr
}

// assertReject 断言拒绝码 + Status.reason，并确认**错误体不回显凭据/正文**。
//
// 「不回显凭据」是一条产品要求（token 是节点的全部凭据），所以每个拒绝都过这道闸：
// 响应体里不许出现提交的令牌明文，也不许出现能力正文的片段（错误不能变成内容通道的
// 侧信道）。
func assertReject(t *testing.T, rr *httptest.ResponseRecorder, token string, wantCode int, wantReason string) {
	t.Helper()
	if rr.Code != wantCode {
		t.Fatalf("status = %d, want %d（body=%s）", rr.Code, wantCode, rr.Body.String())
	}
	if got := reasonOf(t, rr); got != wantReason {
		t.Fatalf("reason = %q, want %q（body=%s）", got, wantReason, rr.Body.String())
	}
	body := rr.Body.String()
	if token != "" && strings.Contains(body, token) {
		t.Fatalf("错误体回显了节点令牌明文（body=%s）", body)
	}
	for _, leak := range []string{"echo shared-body", "echo other-body", "echo only-a-body", "echo only-b-body"} {
		if strings.Contains(body, leak) {
			t.Fatalf("错误体泄漏了能力正文（%q，body=%s）", leak, body)
		}
	}
}

// ---------- 401：节点身份 ----------

// TestNodeCapabilityContentAuth 钉「鉴权先于一切」：缺令牌 / 未知令牌 / 已轮换（撤销）
// 的令牌都必须 401，且响应体不回显令牌。
func TestNodeCapabilityContentAuth(t *testing.T) {
	env := newNodeCapabilityEnv(t)
	digest := env.capSharedA.Digest

	t.Run("没带令牌 → 401", func(t *testing.T) {
		rr := env.get(digest, "")
		assertReject(t, rr, "", http.StatusUnauthorized, reasonUnauthorized)
	})

	t.Run("未知令牌 → 401", func(t *testing.T) {
		bad := "ant_node_not_a_real_token"
		rr := env.get(digest, bad)
		assertReject(t, rr, bad, http.StatusUnauthorized, reasonUnauthorized)
	})

	t.Run("令牌轮换（撤销）后旧令牌立即失效 → 401", func(t *testing.T) {
		// 撤销在 v4 里就是「换掉 token_hash」（store.SetNodeTokenHash）：下一次请求就必须失效，
		// 不能等缓存过期——否则撤销之后旧令牌仍能把整个能力正文拖走。
		fresh := "ant_node_rotated_" + store.NewUserID()
		if err := env.db.SetNodeTokenHash(context.Background(), env.nodeAll.ID, store.HashNodeToken(fresh)); err != nil {
			t.Fatalf("rotate node token: %v", err)
		}
		rr := env.get(digest, env.tokenAll)
		assertReject(t, rr, env.tokenAll, http.StatusUnauthorized, reasonUnauthorized)

		// 新令牌立刻可用：证明上一条 401 是「旧令牌作废」而不是「这条端点坏了」。
		rr = env.get(digest, fresh)
		if rr.Code != http.StatusOK {
			t.Fatalf("轮换后的新令牌 status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
		}
		// 收尾：把库里的身份还原，避免后续子用例（同一 env）受轮换影响。
		if err := env.db.SetNodeTokenHash(context.Background(), env.nodeAll.ID, store.HashNodeToken(env.tokenAll)); err != nil {
			t.Fatalf("restore node token: %v", err)
		}
	})

	t.Run("会话 Cookie 不是节点令牌 → 401", func(t *testing.T) {
		// 控制台会话 id 拿来当 Bearer 必须被拒：两套鉴权不能互相顶替。
		rr := env.get(digest, "sess_not_a_node_token")
		assertReject(t, rr, "sess_not_a_node_token", http.StatusUnauthorized, reasonUnauthorized)
	})
}

// ---------- 授权矩阵：已授权 + 已部署才 200 ----------

// TestNodeCapabilityContentAuthorization 是这条端点的核心判据矩阵。
//
// 每一行都在问同一个问题的一个侧面：「这台节点，此刻，能不能拿到这份内容」。
func TestNodeCapabilityContentAuthorization(t *testing.T) {
	env := newNodeCapabilityEnv(t)

	t.Run("已授权 ns + 已部署到本节点 → 200", func(t *testing.T) {
		rr := env.get(env.capSharedA.Digest, env.tokenAll)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("同一 digest 存在于两个 ns：只在已部署的那个 ns 上可读", func(t *testing.T) {
		// nodeAOnly 的生效 ns 只有 default（A）：D_shared 在 A 里部署给它 → 200。
		if rr := env.get(env.capSharedA.Digest, env.tokenAOnly); rr.Code != http.StatusOK {
			t.Fatalf("A 侧 status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
		}
		// nodeB 的生效 ns 只有 cap-b（B）：D_shared 在 B 里也**存在**，但没有部署给 nodeB
		// → 403（不是 200：存在 ≠ 部署；也不是 404：内容确实看得见）。
		rr := env.get(env.capSharedB.Digest, env.tokenB)
		assertReject(t, rr, env.tokenB, http.StatusForbidden, reasonForbidden)
		// nodeAll 在 A 里部署过同一个 digest，而 B 里的那份没部署：仍然必须 200 ——
		// 「扫到一个未部署的位置就立刻拒绝」是错的（同 digest 多 ns 时随机失败）。
		if rr := env.get(env.capSharedA.Digest, env.tokenAll); rr.Code != http.StatusOK {
			t.Fatalf("nodeAll status = %d, want 200（同 digest 的另一份未部署不得连坐，body=%s）",
				rr.Code, rr.Body.String())
		}
	})

	t.Run("同一 ns 内存在但没有部署到本节点 → 403", func(t *testing.T) {
		rr := env.get(env.capOtherA.Digest, env.tokenAll)
		assertReject(t, rr, env.tokenAll, http.StatusForbidden, reasonForbidden)
	})

	t.Run("内容只在本节点未获授权的 ns 里 → 404（跨 ns 与不存在同码）", func(t *testing.T) {
		// nodeAOnly 只看得到 default：only-b 在 cap-b 里，不能因为「猜对 digest」就 200，
		// 也不能给 403（那等于确认「这个 digest 在别处存在」）。
		rr := env.get(env.capOnlyB.Digest, env.tokenAOnly)
		assertReject(t, rr, env.tokenAOnly, http.StatusNotFound, reasonNotFound)
	})

	t.Run("已授权但未部署的另一个位置 → 403（nodeAll 看得到 B，却没被部署 only-b）", func(t *testing.T) {
		rr := env.get(env.capOnlyB.Digest, env.tokenAll)
		assertReject(t, rr, env.tokenAll, http.StatusForbidden, reasonForbidden)
	})

	t.Run("别的节点部署过不等于本节点可读", func(t *testing.T) {
		// only-a 只部署给 nodeAOnly。nodeAll 的生效 ns 覆盖 A，但没有给它的部署意图 → 403。
		rr := env.get(env.capOnlyA.Digest, env.tokenAll)
		assertReject(t, rr, env.tokenAll, http.StatusForbidden, reasonForbidden)
	})

	t.Run("digest 不存在 → 404", func(t *testing.T) {
		missing := "sha256:" + strings.Repeat("0", 64)
		rr := env.get(missing, env.tokenAll)
		assertReject(t, rr, env.tokenAll, http.StatusNotFound, reasonNotFound)
	})

	t.Run("digest 形状不合法 → 404（不是 400）", func(t *testing.T) {
		// 形状不对的 URL 段不可能命中任何内容行：对调用方就是「这份内容不存在」。
		for _, bad := range []string{
			"not-a-digest",
			"sha256:xyz",
			"sha256:" + strings.Repeat("A", 64), // 大写不是本项目口径
			strings.Repeat("0", 64),             // 少了 sha256: 前缀
		} {
			rr := env.do(http.MethodGet, nodeCapabilityContentPathPrefix+url.PathEscape(bad), env.tokenAll)
			assertReject(t, rr, env.tokenAll, http.StatusNotFound, reasonNotFound)
		}
	})

	t.Run("非 GET → 405 + Allow", func(t *testing.T) {
		rr := env.do(http.MethodPut, contentURL(env.capSharedA.Digest), env.tokenAll)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405（body=%s）", rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get("Allow"); !strings.Contains(got, http.MethodGet) {
			t.Fatalf("Allow = %q, want 包含 %q", got, http.MethodGet)
		}
	})
}

// ---------- 200：原始字节 ----------

// TestNodeCapabilityContentBody 钉成功响应的形状：**原文**（不套信封、不带 status）、
// 正确 Content-Type、no-store、Content-Length 与实际字节一致，并且字节按节点侧算法
// 重算出的 digest 与请求的 digest 相同（否则节点必然判 digest_mismatch）。
func TestNodeCapabilityContentBody(t *testing.T) {
	env := newNodeCapabilityEnv(t)
	want, err := env.db.CurrentArtifact(context.Background(), env.nsA.ID, env.capSharedA.ID)
	if err != nil {
		t.Fatalf("read current artifact: %v", err)
	}

	rr := env.get(want.Digest, env.tokenAll)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
	}

	body := rr.Body.Bytes()
	if string(body) != string(want.Spec) {
		t.Fatalf("响应体不是 store 的规范字节\n got=%s\nwant=%s", body, want.Spec)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store（中枢侧不许缓存：唯一缓存层是节点）", got)
	}
	if got := rr.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length = %q, want %d", got, len(body))
	}

	// 资源信封的五个键一个都不许出现：内容通道是 api-conventions §3.1 的显式例外，
	// 套上 status 会让节点把「派生状态」当成内容的一部分去算 digest。
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("响应体不是 JSON: %v（body=%s）", err, body)
	}
	if _, has := raw["status"]; has {
		t.Fatalf("响应体带了 status（内容通道不许有派生状态）：%s", body)
	}
	if raw["apiVersion"] != artifact.APIVersion || raw["kind"] != artifact.KindCapability {
		t.Fatalf("响应体不是 artifact 文档：%s", body)
	}

	// 节点侧校验：Decode = Parse + 重算 digest。digest 必须与请求的一致。
	a, digest, err := artifact.Decode(body)
	if err != nil {
		t.Fatalf("artifact.Decode(body): %v", err)
	}
	if digest != want.Digest {
		t.Fatalf("节点重算 digest = %s, 请求的是 %s（节点会判 digest_mismatch）", digest, want.Digest)
	}
	if a.Metadata.Name != "shared" || a.Metadata.Version != env.capSharedA.Version {
		t.Fatalf("metadata = %+v, want shared@%d", a.Metadata, env.capSharedA.Version)
	}
	// 完整 run.files 必须在（契约：内容通道下发的是**完整** ArtifactJSON）。
	if got := a.Spec.Run.Files["run.sh"]; !strings.Contains(got, "echo shared-body") {
		t.Fatalf("run.files 不完整（run.sh = %q）", got)
	}

	// 路径段的形状：节点用 url.PathEscape 拼 URL，不能出现 %3A —— 否则服务端的
	// {digest} 段会拿到转义后的串，永远匹配不到内容。
	if strings.Contains(contentURL(want.Digest), "%3A") {
		t.Fatalf("节点侧拼出的 URL 把冒号转义了：%s", contentURL(want.Digest))
	}
}

// ---------- 授权撤销：立即生效 ----------

// TestNodeCapabilityContentRevocation 钉「撤销立刻生效」的两条路径：命名空间授权收窄
// 之后旧部署行不再能读到内容（部署行不是授权），以及部署状态变化不影响可读性。
func TestNodeCapabilityContentRevocation(t *testing.T) {
	env := newNodeCapabilityEnv(t)
	ctx := context.Background()

	if rr := env.get(env.capOnlyA.Digest, env.tokenAOnly); rr.Code != http.StatusOK {
		t.Fatalf("收窄前 status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
	}

	t.Run("命名空间授权收窄后旧部署行立即失效 → 404", func(t *testing.T) {
		// 部署行仍然在库里（部署是历史意图），但节点已不被授权访问 default：
		// 读结果必须只由**当前**授权决定，不能拿旧部署行当通行证。
		cfg, err := env.db.GetNodeConfig(ctx, env.nodeAOnly.ID)
		if err != nil {
			t.Fatalf("get node config: %v", err)
		}
		cfg.Namespaces = []string{"cap-b"}
		if err := env.db.PutNodeConfig(ctx, cfg); err != nil {
			t.Fatalf("put node config: %v", err)
		}
		rr := env.get(env.capOnlyA.Digest, env.tokenAOnly)
		assertReject(t, rr, env.tokenAOnly, http.StatusNotFound, reasonNotFound)
	})

	t.Run("能力被删除后立即不可读 → 404", func(t *testing.T) {
		// 删除是「隐藏」语义（能力行打 deleted_at）：内容不再可部署，也就不该再能被下载。
		// 部署行还留在库里的情况（nodeAll 上 never deployed，这里用 other 已经 403）——
		// 删除之后必须收到 404（内容不存在），而不是 403 或 200。
		cur, err := env.db.GetCapability(ctx, env.nsA.ID, env.capOtherA.Name, env.capOtherA.Version)
		if err != nil {
			t.Fatalf("read capability: %v", err)
		}
		if err := env.db.DeleteCapability(ctx, env.nsA.ID, cur.Name, store.ResourceVersion(cur.ID, cur.ResourceVersion)); err != nil {
			t.Fatalf("delete capability: %v", err)
		}
		rr := env.get(env.capOtherA.Digest, env.tokenAll)
		assertReject(t, rr, env.tokenAll, http.StatusNotFound, reasonNotFound)
	})

	t.Run("部署状态 failed 仍然可读（回执是观测，不是权限）", func(t *testing.T) {
		id, ok := env.deployOnlyB[env.nodeB.ID]
		if !ok {
			t.Fatalf("找不到 nodeB 的部署行 id")
		}
		if err := env.db.MarkDeployResult(ctx, env.nsB.ID, id, store.DeploymentFailed, "boom"); err != nil {
			t.Fatalf("mark deploy result: %v", err)
		}
		rr := env.get(env.capOnlyB.Digest, env.tokenB)
		if rr.Code != http.StatusOK {
			t.Fatalf("failed 部署 status = %d, want 200（失败后运维要能重取一次，body=%s）",
				rr.Code, rr.Body.String())
		}
	})
}
