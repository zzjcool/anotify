package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/authn"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 api-conventions §8.2 的**内容通道**（能力包体下行）：
//
//	GET /v1/node/capabilities/{digest}
//	Authorization: Bearer <node token>
//
// 它是 `capability.deploy` 帧的传输另一半：帧里只有
// `{namespace, name, version, digest}`（不带包体，见 internal/protocol/deploy_frames.go），
// 节点拿到 refs 后用**自己的节点令牌**到这里取那份不可变 Artifact 文档，校验 digest / refs
// 后原子缓存。消费者是节点上的 anode（cmd/anode/internal/capability 的 Fetch），
// 不是控制台——所以鉴权是节点身份，**不套 session 中间件**（节点没有用户 Cookie）。
//
// # 三条硬语义（越界就是越权）
//
//  1. **只有「已授权 + 已部署」的内容可读。** 判据是两个条件同时成立：该能力属于这台
//     Node 的**生效**命名空间（申报 ∩ 中枢授权，store.AuthorizedNamespaces），且该
//     `(node, capability)` 上真有一行部署意图（store.GetDeploymentByNodeCapability）。
//     「节点拿着合法令牌」本身**不**够——否则任意节点都能按 digest 拖走整个集群的能力正文。
//  2. **响应体就是 artifact JSON 原文。** 直接写 store 给出的 `Artifact.Spec`
//     （= `artifact.CanonicalJSON` 的规范字节，含完整 `run.files`），不套资源信封、不带
//     `status`、不改 schema。节点会用同一份字节重算 digest 与 refs 比对，任何再包装都会
//     让 `digest_mismatch` 永远成立。
//  3. **绝不回退。** 不读用户 workDir、不读托管池、不跨节点/跨命名空间兜底：内容唯一的
//     来源是能力行派生出的规范字节（store 侧的内容寻址）。读不到就是读不到。
//
// 节点的 status（online / offline / draining）**不参与**判据：能力下行是「部署意图」的传输，
// 不是接新任务。离线节点的部署会在重连后重放，排空中的节点同样需要能取到自己已被部署的内容
// （与 GET /v1/node/binary 的取舍一致：升级/维护动作不看 status）。
//
// # 错误码（按契约的封闭集合，不新造词）
//
//	401 Unauthorized  令牌缺失 / 未知 / 已轮换（撤销）。不区分三者：区分会变成「令牌是否
//	                  存在」的探测面，而调用方要做的动作完全一样（拿一台节点的有效令牌）。
//	403 Forbidden     该 digest 在当前命名空间上下文里**看得见**（生效 ns 内确有这份内容），
//	                  但**没有 deploy 到这台 Node**。这不是 404：能力确实存在，缺的是给这台
//	                  机器的部署意图；节点回执里 `forbidden` 能把运维直接指向「去点部署」。
//	404 NotFound      digest 不存在，或它只存在于这台 Node **未获授权**的命名空间里。
//	                  跨 ns 与不存在**同码**（与 namespace_scope.go 的判据一致）：否则一次
//	                  按 digest 的枚举就能问出「别的命名空间里有没有这份内容」。
//	405              非 GET（ServeMux 的方法前缀也会兜住；handler 内再判一次是为了脱离 mux
//	                  直接使用时行为一致）。
//	500 InternalError 库故障（不含任何凭据）。
//
// 状态判定顺序**不可换**：鉴权 → digest 形状 → 授权（ns ⇒ 部署）。把授权放在鉴权之前会
// 让匿名请求也能探测 digest 是否存在。
type nodeCapabilityContentHandler struct {
	db  *store.DB
	log *slog.Logger
}

// newNodeCapabilityContentHandler 构造内容通道 handler（mux 只在 mux.go 接线一次）。
func newNodeCapabilityContentHandler(db *store.DB, log *slog.Logger) *nodeCapabilityContentHandler {
	if log == nil {
		log = slog.Default()
	}
	return &nodeCapabilityContentHandler{db: db, log: log}
}

// registerNodeCapabilityContent 注册这条端点的**两条** pattern：GET 本体 + 非 GET 的显式 405。
//
// 为什么 405 要一条单独的裸 pattern：生产 mux 末尾挂着静态资源兜底 `/`，方法不匹配的请求
// 会落到那里变成 503/404（「这个方法不支持」被伪装成「服务器/路径有问题」）。与
// `/v1/namespaces/{ns}/edges`（resource_capability.go）同一条理由。
//
// 为什么两条都放在本文件而不是 mux.go：mux.go 只要出现 405 分支，OpenAPI parity 的
// 「裸挂载 handler 有 405 分支 ⇒ spec 必须声明 405」判据就会把 mux.go 里的**其它**裸挂载
// （`/v1/vapid-public-key`：任何方法都回同一个公钥 JSON）一起判红。把这条端点的注册整体
// 收在自己的文件里，判据仍指向真实源码，且不会波及无关端点。
func registerNodeCapabilityContent(mux *http.ServeMux, db *store.DB, log *slog.Logger) {
	mux.Handle("GET /v1/node/capabilities/{digest}", noStore(newNodeCapabilityContentHandler(db, log)))
	// 裸 pattern：GET/HEAD 由上面那条更具体的接住，其余方法直接 405 + Allow。
	mux.Handle("/v1/node/capabilities/{digest}", http.HandlerFunc(methodNotAllowedAllowGet))
}

// capabilityDigestPattern 是 URL 段里 digest 的形状。
//
// 前缀常量直接取 artifact.DigestPrefix（`sha256:`），不在这里再抄一个字面量：它一旦改动
// （不会，但代价是全网节点缓存全失效），这条正则必须跟着动，抄两份必然漏一处。
//
// 线上形态是**字面量** `sha256:<64 hex>`：节点侧用 `url.PathEscape(ref.Digest)` 拼 URL，
// 而 Go 的 path-segment 转义不转 `:`（RFC 3986 §3.3 的 pchar 允许它），所以路径段里看到
// 的就是冒号本身，不需要在这里解 %3A。
var capabilityDigestPattern = regexp.MustCompile(`^` + regexp.QuoteMeta(artifact.DigestPrefix) + `[0-9a-f]{64}$`)

// 两条域内拒绝（用 Status 的封闭 reason；message 只描述判据，不含任何标识）。
var (
	// errCapabilityContentNotFound：digest 在这台 Node 的可见范围内不存在。
	//
	// 刻意**不**区分「全库都没有」与「在未授权 ns 里」：两者对调用方是同一个动作
	// （停止并上报 not_found），而区分就等于给出跨 ns 的存在性探针。
	errCapabilityContentNotFound = errStatus(http.StatusNotFound, reasonNotFound,
		"no such capability content")

	// errCapabilityContentNotDeployed：内容在生效 ns 内可见，但没有部署到这台 Node。
	errCapabilityContentNotDeployed = errStatus(http.StatusForbidden, reasonForbidden,
		"capability is not deployed to this node")
)

// ServeHTTP 实现内容通道。
func (h *nodeCapabilityContentHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeStatus(w, http.StatusMethodNotAllowed, reasonMethodNotAllowed, "method not allowed", nil)
		return
	}

	// ① 节点身份。token 明文**绝不**进日志/响应（它是这台节点的全部凭据）。
	token := authn.BearerToken(r)
	if token == "" {
		h.log.Warn("capability content rejected: missing node token",
			"event", "node.capability.auth_failed",
			"reason", "missing_node_token",
			"ip", r.RemoteAddr,
		)
		writeStatus(w, http.StatusUnauthorized, reasonUnauthorized, "missing_node_token", nil)
		return
	}
	node, err := h.db.GetNodeByTokenHash(r.Context(), store.HashNodeToken(token))
	if err != nil {
		// 查询失败（含 token 不匹配 / 节点已删 / 令牌已轮换）一律 401：撤销要**立即**生效，
		// 而「旧令牌还能问出点什么」正是撤销最不该留下的后门。
		h.log.Warn("capability content rejected: unknown node token",
			"event", "node.capability.auth_failed",
			"reason", "invalid_node_token",
			"error", err.Error(),
			"ip", r.RemoteAddr,
		)
		writeStatus(w, http.StatusUnauthorized, reasonUnauthorized, "invalid_node_token", nil)
		return
	}

	// ② digest 形状。形状不可能命中任何内容行 → 404（不是 400：URL 段不合法对调用方
	//    与「这份内容不存在」是同一个结论，节点回执里也不需要多一个分支）。
	digest := strings.TrimSpace(r.PathValue("digest"))
	if !capabilityDigestPattern.MatchString(digest) {
		h.reject(w, node.ID, digest, errCapabilityContentNotFound, "bad_digest_shape")
		return
	}

	// ③ 授权：生效 ns（申报 ∩ 授权）+ **本节点**的部署意图，两个条件都成立才返回内容。
	art, rerr := h.resolve(r.Context(), node, digest)
	if rerr != nil {
		// 两条域内拒绝各记一条可诊断的 reason（not_found / not_deployed）；其它错误是库故障，
		// 记 ERROR 并按 500 回（不把「我们没预料到」报成调用方的错）。
		switch {
		case errors.Is(rerr, errCapabilityContentNotFound):
			h.reject(w, node.ID, digest, rerr, "not_found")
		case errors.Is(rerr, errCapabilityContentNotDeployed):
			h.reject(w, node.ID, digest, rerr, "not_deployed")
		default:
			h.log.Error("capability content failed",
				"event", "node.capability.failed",
				"node_id", node.ID,
				"digest", digest,
				"error", rerr.Error(),
			)
			writeAPIError(w, rerr)
		}
		return
	}

	// ④ 原始字节。Content-Type 是 JSON（artifact.Parse 同时吃 YAML/JSON，但这里出去的
	//    永远是 canonical JSON）；Content-Length 用实际字节数，节点可以用它判「下载不完整」。
	//
	//    no-store 在 mux 上由 noStore 中间件已经设好，这里再设一次：内容通道的唯一缓存
	//    层是**节点侧**的原子缓存（capability.md §8.5），中枢侧任何一级缓存（浏览器/反代）
	//    都只会让「撤销部署后还能读到旧正文」这件事发生。
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Content-Length", strconv.Itoa(len(art.Spec)))
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(art.Spec)

	h.log.Info("capability content served",
		"event", "node.capability.served",
		"node_id", node.ID,
		"namespace_id", art.NamespaceID,
		"capability_id", art.CapabilityID,
		"digest", art.Digest,
		"bytes", len(art.Spec),
	)
}

// reject 记录拒绝原因并写 Status 响应。
//
// 日志只带 node id / digest / 判据名：**不带** token、不带能力正文。digest 本身是内容哈希，
// 已经在节点的部署 refs 里出现过，不是秘密。
func (h *nodeCapabilityContentHandler) reject(w http.ResponseWriter, nodeID, digest string, err error, why string) {
	h.log.Warn("capability content rejected",
		"event", "node.capability.rejected",
		"reason", why,
		"node_id", nodeID,
		"digest", digest,
	)
	writeAPIError(w, err)
}

// resolve 在「这台 Node 能看见的范围」里找这份内容，并确认它已部署到这台 Node。
//
// 顺序与判据（**唯一**实现，改这里就是改授权语义）：
//
//	生效 ns（AuthorizedNamespaces = 申报 ∩ 授权）→ 逐 ns GetArtifactByDigest
//	→ 命中后 GetDeploymentByNodeCapability(ns, node, capability)
//
// 为什么不直接按 (node, digest) 查部署：store 没有这个索引，而 `deployments.digest` 列里
// 那份是**建行时的副本**（审计用），期望值只有能力行说得准（见 deploymentDigestSQL 的注释）。
// 所以「这份内容是否存在 / 是否部署到这台 Node」必须落在能力行 + 部署行两处事实上，
// 而不是从 digest 列反推。
//
// 「先扫可见 ns、再判部署」还有一个行为要求：**同一 digest 可以同时存在于多个 ns**
// （内容寻址只保证 ns 内唯一）。只要其中**任意一个**既可见又部署到本节点，就必须 200；
// 只有「看得见但全都没部署」才是 403。中途记下 notDeployed，扫完再决定码——不能一命中
// 未部署就立刻 403，那会让「A ns 已部署、B ns 未部署」的正常情况随机失败。
func (h *nodeCapabilityContentHandler) resolve(ctx context.Context, node *store.Node, digest string) (*store.Artifact, error) {
	authorized, err := h.db.AuthorizedNamespaces(ctx, node.ID)
	if err != nil {
		return nil, fmt.Errorf("capability content: 读节点生效命名空间: %w", err)
	}
	namespaces, err := h.db.ListNamespaces(ctx)
	if err != nil {
		return nil, fmt.Errorf("capability content: 列命名空间: %w", err)
	}

	notDeployed := false
	for _, ns := range namespaces {
		if !nodeNamespaceAllowed(authorized, ns.Slug) {
			continue
		}
		art, aerr := h.db.GetArtifactByDigest(ctx, ns.ID, digest)
		if errors.Is(aerr, store.ErrArtifactNotFound) {
			continue
		}
		if aerr != nil {
			return nil, fmt.Errorf("capability content: 读产物 %s/%s: %w", ns.Slug, digest, aerr)
		}
		dp, derr := h.db.GetDeploymentByNodeCapability(ctx, ns.ID, node.ID, art.CapabilityID)
		if errors.Is(derr, store.ErrNotFound) {
			// 内容在、部署意图不在：记下来，继续找下一个 ns（同 digest 可能部署在别处）。
			notDeployed = true
			continue
		}
		if derr != nil {
			return nil, fmt.Errorf("capability content: 读部署 %s/%s: %w", ns.Slug, art.CapabilityID, derr)
		}
		// 部署行的 digest 取能力行（deploymentDigestSQL），与 art.Digest 同源；这里仍显式比一次：
		// 两者若分叉，说明「中枢说的期望」与「要发出去的字节」已经不是同一份，宁可 403 也不能
		// 把一份节点会判 digest_mismatch 的字节发出去（那会让部署永远收敛不了，且症状不可查）。
		if dp.Digest != digest {
			notDeployed = true
			continue
		}
		// 部署状态（pending / applied / failed）**不影响可读性**：三者都表示中枢对这台机器
		// 下达过「持有这份内容」的意图。已失败的行更需要能重取一次（运维修完本机问题后重试），
		// 而 404 会把这条恢复路径堵死。
		return art, nil
	}
	if notDeployed {
		return nil, errCapabilityContentNotDeployed
	}
	return nil, errCapabilityContentNotFound
}

// nodeNamespaceAllowed 判断生效 ns 列表（可含 "*"）是否覆盖某个 ns 名（slug）。
//
// 与 ws 包 `namespaceAllowed` 同一判据、同一语义（internal/ws/hub.go）。之所以在本包再写
// 一遍而不是导出 ws 的：ws 是协议层包，server 不该因为一个字符串比较就依赖它（现在依赖它
// 是因为 Hub 本身）；判据只有两行，重复的成本低于把「谁是授权判据的 owner」搞混。
func nodeNamespaceAllowed(authorized []string, slug string) bool {
	for _, ns := range authorized {
		if ns == store.NamespaceWildcard || ns == slug {
			return true
		}
	}
	return false
}
