package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/zzjcool/anotify/internal/authn"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是「中枢自带 agent 二进制」的升级源（CONTRACT-agent-upgrade §2 / api-conventions §8.2）。
//
// 两个消费方：
//   - GET /v1/node/binary：节点/CLI 下载（node token 鉴权，不是会话；本文件实现）；
//   - POST /v1/nodes/{name}/upgrade：控制台把「去哪个路径拿、拿到后该是什么」推给节点
//     （端点实现在 resource_node.go 的 nodeAPI.upgradeNode；本文件只提供它要的
//     升级源事实，见 node_upgrade_handlers.go 的 agentUpgradeSource）。
//
// 二进制**不**走 WebSocket：agent 是 15 MB 量级，一帧塞进去会阻塞同一条连接上的
// 心跳与 assign（节点读超时 = 掉线）。帧里只有版本 + sha256 + 路径，下载走普通 HTTP。

// agentBinary 是启动时读进内存的 agent 二进制。
//
// 为什么启动时就整块读进来（而不是每次请求现读磁盘）：**sha256 必须与字节是同一份
// 快照**。现读磁盘时，运维换掉文件的那一刻会出现「头里的 sha256 是旧文件的、body 是
// 新文件的」，节点校验必然失败（bad_sha），而且只在换文件的那几毫秒里发生——最难查的
// 那种偶发。启动时读一次，文件与哈希天然同源；换二进制用重启发布（与 server 自身
// 版本注入同一节奏）。
type agentBinary struct {
	data    []byte
	sha256  string // 64 位十六进制（不带 sha256: 前缀）
	version string // 中枢版本（= 升级目标版本）
	path    string
}

// loadAgentBinary 读取并摘要 agent 二进制；文件不存在 = 功能未启用（**不是错误**）。
//
// 契约 §2：服务端启动时读 ANOTIFY_AGENT_BIN（默认 /app/agent）；文件不存在 = 该功能
// 未启用，日志 WARN 一句即可。绝大多数开发/裸机部署本来就不带 agent 二进制，
// 把它当启动失败等于让「不升级」这个合法选择变成部署故障。
func loadAgentBinary(path, version string) *agentBinary {
	if path == "" {
		path = defaultAgentBinPath
	}
	b := &agentBinary{path: path, version: version}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			slog.Warn("agent binary not found, self-upgrade disabled",
				"event", "agent.binary.disabled",
				"path", path,
				"hint", "ANOTIFY_AGENT_BIN 指向的文件不存在；GET /v1/node/binary 返回 501，控制台不显示升级按钮",
			)
			return b
		}
		// 权限/IO 错误同样降级为「未启用」：它不该阻止中枢启动，但必须留下可查的原因。
		slog.Warn("agent binary unreadable, self-upgrade disabled",
			"event", "agent.binary.disabled",
			"path", path,
			"error", err.Error(),
		)
		return b
	}
	sum := sha256.Sum256(data)
	b.data = data
	b.sha256 = hex.EncodeToString(sum[:])
	slog.Info("agent binary ready",
		"event", "agent.binary.ready",
		"version", version,
		"sha256", b.sha256,
		"bytes", len(data),
		"path", path,
	)
	return b
}

// enabled 报告本次部署是否带 agent 二进制（= 自升级功能是否启用）。
func (b *agentBinary) enabled() bool { return b != nil && b.data != nil }

// targetVersion 是控制台看到的「目标版本」：没带二进制时是空串（UI 不显示升级按钮）。
func (b *agentBinary) targetVersion() string {
	if !b.enabled() {
		return ""
	}
	return b.version
}

// reasonAgentBinaryUnavailable 是「这台中枢没带 agent 二进制」的 Status.reason（501）。
//
// 它**不在** api-conventions §4.2 的通用集合里：501 本身也不在那张 HTTP 码表里。这里
// 保留一个专用 reason 而不是把 501 硬塞进 BadRequest/Conflict，是因为调用方必须能把
// 「服务端没有升级源」与「节点/请求出错」分开（CLI 退出码 2 就映射到这里）；无 body
// 或通用 reason 都会让这条分支在客户端侧不可判。与 `slug_taken` / `playbook_conflict`
// 这类域内码同性质，属于本域的封闭补充。
const reasonAgentBinaryUnavailable = "agent_binary_unavailable"

// agentBinaryHandler 处理 GET /v1/node/binary（api-conventions §8.2）。
//
// 鉴权是 **node token**（Authorization: Bearer <token>），不是会话 Cookie：下载方是
// 节点上的 anode / CLI，它们手里只有 node token。v4 的 node 是 cluster-scoped 资源，
// 所以这里只做「这个 token 属于一台存在的节点」这一件事——token 的 sha256 命中
// nodes.token_hash 即通过（与 WS hello 的握手同一条判据）。
//
// 不区分「没带令牌」与「令牌无效」的响应体措辞可以更精确，但状态码必须一致（401）：
// 403 会让调用方以为「令牌有效但没权限」从而去申请权限，而真实原因是令牌根本不对。
//
// 排空中的节点仍然可以下载自己（维护动作，不是接新任务）：本 handler 不看 node.Status。
type agentBinaryHandler struct {
	bin *agentBinary
	db  *store.DB
}

func (h *agentBinaryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		// 错误体统一 Status（§4.1）；405 + reason 是封闭集合里的成员。
		w.Header().Set("Allow", "GET")
		writeStatus(w, http.StatusMethodNotAllowed, reasonMethodNotAllowed, "method not allowed", nil)
		return
	}

	token := authn.BearerToken(r)
	if token == "" {
		// token 本身绝不进日志（它是节点的全部凭据）；只记来源地址。
		slog.Warn("agent binary download rejected: missing node token",
			"event", "agent.binary.auth_failed",
			"reason", "missing_node_token",
			"ip", r.RemoteAddr,
		)
		writeStatus(w, http.StatusUnauthorized, reasonUnauthorized, "missing_node_token", nil)
		return
	}
	node, err := h.db.GetNodeByTokenHash(r.Context(), store.HashNodeToken(token))
	if err != nil {
		// 查询失败（含 token 不匹配）一律 401：不区分「令牌不存在」与「库出错」，
		// 细节进服务端日志，响应体只说「这个令牌不能用」。
		slog.Warn("agent binary download rejected: unknown node token",
			"event", "agent.binary.auth_failed",
			"reason", "invalid_node_token",
			"error", err.Error(),
			"ip", r.RemoteAddr,
		)
		writeStatus(w, http.StatusUnauthorized, reasonUnauthorized, "invalid_node_token", nil)
		return
	}

	if !h.bin.enabled() {
		// 501 是**明确语义**（不是 404）：调用方据此区分「服务端没带二进制」
		// 与「路径写错了」。CLI 的退出码 2 就映射到这里（契约 §4）。
		//
		// 鉴权先于配置探测：不能把「服务端带没带二进制」当成匿名可读的信息。
		writeStatus(w, http.StatusNotImplemented, reasonAgentBinaryUnavailable,
			"this server ships no agent binary", nil)
		return
	}

	hdr := w.Header()
	hdr.Set("Content-Type", "application/octet-stream")
	hdr.Set("X-Agent-Version", h.bin.version)
	hdr.Set("X-Agent-Sha256", h.bin.sha256)
	// Content-Length 显式给：客户端可以用它做进度与「下载不完整」判断，
	// 而我们的 body 是内存里一整块，长度天然已知（不需要 chunked）。
	hdr.Set("Content-Length", strconv.Itoa(len(h.bin.data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(h.bin.data)

	slog.Info("agent binary downloaded",
		"event", "agent.binary.downloaded",
		"node_id", node.ID,
		"version", h.bin.version,
		"sha256", h.bin.sha256,
		"bytes", len(h.bin.data),
	)
}
