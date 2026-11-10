package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/ws"
)

// Node 资源域（cluster-scoped）+ 节点配置子资源 + 身份令牌（无命名空间）
// —— 契约：api-conventions §7.4 / config.md §4（W4 / W5）/ WIRE-HANDOFF W4、W5。
//
// 路由（全部 session 包裹，会话鉴权由 serverDeps.session 提供）：
//
//	GET    /v1/nodes                        NodeList（无 namespace；空列表 []）
//	POST   /v1/nodes                        登记离线 Node（metadata.name 必填；**不签 token**）
//	POST   /v1/nodes/config/batch           逐节点 CAS 批量写配置（可部分成功）
//	GET    /v1/nodes/{name}                 单个 Node；?namespace=X 时附 ns 上下文
//	PATCH  /v1/nodes/{name}                 改 spec（namespaces/vars/limits/tags），要 metadata.resourceVersion
//	DELETE /v1/nodes/{name}?resourceVersion= 删除（唯一 CAS 承载是 query）
//	POST   /v1/nodes/{name}/tokens          签发 / 轮换身份令牌 → NodeToken（明文只返回这一次）
//	GET    /v1/nodes/{name}/config          节点默认值 wrapper（W5，不是独立 kind）
//	PUT    /v1/nodes/{name}/config          全量替换默认值（namespaces/vars/limits），要 metadata.resourceVersion
//	POST   /v1/nodes/{name}/drain           动作：置 draining
//	POST   /v1/nodes/{name}/upgrade         动作：下发 agent 升级帧
//
// 三条与 store 的分工不能越界：
//
//   - **Node.spec 与 config 子资源是同一份事实/同一个底层版本**（config.md §4.1）：
//     两者都走 store 的名字版 CAS 入口（UpdateNode / PutNodeConfigByName /
//     DeleteNodeByName），本文件不自己算 resourceVersion、不自己开事务。
//   - **令牌明文只出现在 tokens 动作的响应体里一次**：Node / config / 读端点全部用
//     显式投影（从不 marshal store.Node），所以 token_hash 结构上就没有出口。
//   - **status 只放 store 行里真实存在的事实**（nodes / node_configs / node_instances），
//     拿不到的字段省略，不编造；节点生命周期（online/offline/draining）不在 api-conventions
//     §7.4 的 status 投影里，这里不发明字段名（已在 #331 交付报告里作为契约缺口上报）。

// 本域的三个 kind 字面量（NodeToken 是动作文档，不是资源）。
const (
	kindNode      = "Node"
	kindNodeList  = "NodeList"
	kindNodeToken = "NodeToken"
)

// nodeUpgradeSource 是「中枢自带 agent 二进制」（升级源）在资源层的只读投影。
//
// 为什么是接口而不是直接读 *agentBinary：升级源由装配层（mux.go / W-asm）在启动时
// 载入一次并缓存字节 + 哈希，本域只需要「目标版本」与「内容 sha256」两个事实。
// **未注入 = 这个部署没带二进制**（合法部署形态），POST /upgrade 回 501
// agent_binary_unavailable，与 GET /v1/node/binary 的 501 同语义。
type nodeUpgradeSource interface {
	// TargetVersion 是本次下发的目标版本（= 中枢自己的版本）。
	TargetVersion() string
	// SHA256 是二进制内容的 sha256（64 位小写十六进制，不带 "sha256:" 前缀）。
	SHA256() string
}

// nodeAPI 是 Node 资源域的 handler 集合（全部端点共用同一份运行期依赖）。
type nodeAPI struct {
	d *serverDeps
	// upgrade 由装配层注入（setUpgradeSource）；nil = 未启用自升级。
	upgrade nodeUpgradeSource
}

// newNodeAPI 构造 Node 资源域。
func newNodeAPI(d *serverDeps) *nodeAPI { return &nodeAPI{d: d} }

// setUpgradeSource 注入升级源（装配层专用；不注入 = 功能未启用）。
//
// 用未导出方法而不是构造参数：newNodeAPI 的签名是冻结契约，装配层在构造之后补这一句
// 即可（`api := newNodeAPI(deps); api.setUpgradeSource(bin)`）。
func (a *nodeAPI) setUpgradeSource(src nodeUpgradeSource) { a.upgrade = src }

// register 把本域全部路由挂到 mux 上（全部 session 包裹）。
//
// 方法维度交给 ServeMux 的 "METHOD /path" 模式：错误方法由 mux 直接回 405，
// 不需要每个 handler 再抄一遍 methodNotAllowed。
func (a *nodeAPI) register(mux *http.ServeMux) {
	if mux == nil || a == nil || a.d == nil {
		return
	}
	route := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, noStore(a.d.session(h)))
	}
	route("GET "+nodesPath, a.list)
	route("POST "+nodesPath, a.create)
	// 字面量段 "config" 比 "*/{name}" 更具体，ServeMux 优先匹配它，与 {name} 路由不冲突。
	route("POST "+nodesPath+"/config/batch", a.configBatch)
	route("GET "+nodesPath+"/{name}", a.get)
	route("PATCH "+nodesPath+"/{name}", a.patch)
	route("DELETE "+nodesPath+"/{name}", a.delete)
	route("POST "+nodesPath+"/{name}/tokens", a.issueToken)
	route("GET "+nodesPath+"/{name}/config", a.configGet)
	route("PUT "+nodesPath+"/{name}/config", a.configPut)
	route("POST "+nodesPath+"/{name}/drain", a.drain)
	route("POST "+nodesPath+"/{name}/upgrade", a.upgradeNode)
}

// nodesPath 是 Node 集合的根路径（cluster-scoped：路径里没有 namespace）。
const nodesPath = "/v1/nodes"

// ---------- wire 形状 ----------

// nodeSpec 是 Node.spec（中枢说的期望 / 授权）。
//
// namespaces 是**授权**列表（与本机申报取交集，只能收紧）；vars/limits 是机器级默认值；
// tags 沿用现有 tags（v4 不引 labels）。
type nodeSpec struct {
	Namespaces []string          `json:"namespaces"`
	Vars       map[string]string `json:"vars"`
	Limits     map[string]any    `json:"limits"`
	Tags       []string          `json:"tags"`
}

// nodeSpecPatch 是写请求里的 spec：指针/可空形态用以区分「没给」与「给了空值」。
//
// merge-patch 语义（§3.4）：nil = 这一项不动；显式空对象/空数组 = 清空。
type nodeSpecPatch struct {
	Namespaces *[]string         `json:"namespaces"`
	Vars       map[string]string `json:"vars"`
	Limits     map[string]any    `json:"limits"`
	Tags       *[]string         `json:"tags"`
}

// nodeCapacity 是 status.capacity（anode.md §3.6 的内置机器事实，wire 上是字符串）。
type nodeCapacity struct {
	CPU string `json:"cpu"`
}

// nodeInstanceRef 是 status.instances[] 的一条只读诊断投影。
//
// id 是 store 的实例身份（`nins_…`），**不是** wire 上的 slot（§7.4 明确）。
// task 为 null = 该槽此刻空闲；busy 与 task 是同一事实的两个读法，都给是刻意的。
type nodeInstanceRef struct {
	ID   string  `json:"id"`
	Task *string `json:"task"`
	Busy bool    `json:"busy"`
}

// nodeStatus 是 Node.status（只从事实派生，不落库）。
//
// 只有 store 行里真实存在的字段；取不到的省略（omitempty）。localVars/nodeVars/
// machineVars/effectiveNamespaces/instances 恒出现（空给 `{}` / `[]`）——它们是
// 「配置长什么样」的答案，缺省与"没有"必须可区分。
type nodeStatus struct {
	HostIP   string        `json:"hostIP,omitempty"`
	Hostname string        `json:"hostname,omitempty"`
	OS       string        `json:"os,omitempty"`
	Arch     string        `json:"arch,omitempty"`
	Capacity *nodeCapacity `json:"capacity,omitempty"`
	WorkRoot string        `json:"workRoot,omitempty"`
	HomeDir  string        `json:"homeDir,omitempty"`

	// 机器级配置：本机快照 / 节点默认值 / 两者合并（local 压住 node）。
	LocalVars   map[string]string `json:"localVars"`
	NodeVars    map[string]string `json:"nodeVars"`
	MachineVars map[string]string `json:"machineVars"`

	// 两个版本是不同的事实（C18）：configRevision 是配置生效版本（中枢生成），
	// appliedRevision/appliedHash 是节点回报的那一版。
	ConfigRevision  string `json:"configRevision"`
	AppliedRevision string `json:"appliedRevision"`
	AppliedHash     string `json:"appliedHash"`

	EffectiveNamespaces []string          `json:"effectiveNamespaces"`
	AgentVersion        string            `json:"agentVersion,omitempty"`
	Instances           []nodeInstanceRef `json:"instances"`
	LastSeenAt          string            `json:"lastSeenAt,omitempty"`

	// 只有 GET /v1/nodes/{name}?namespace=X 才出现（§7.4：有效值必须带 ns 上下文，
	// 不存在一份覆盖全部 ns 的扁平 effectiveVars）。
	NamespaceVars map[string]string             `json:"namespaceVars,omitempty"`
	EffectiveVars map[string]store.EffectiveVar `json:"effectiveVars,omitempty"`
}

// nodeReadContext 标记本次读的命名空间上下文（与 status.namespaceVars/effectiveVars 配套）。
type nodeReadContext struct {
	Namespace string `json:"namespace"`
}

// nodeReadEnvelope 是 Node 信封；?namespace= 时多一个顶层 context。
//
// 顶层多一个键是**契约明文要求**（§7.4「并标 context.namespace」）：它不属于
// metadata/spec/status 任何一段，塞进 status 会让"哪个 ns"和"这个 ns 的值"混在一起。
type nodeReadEnvelope struct {
	apiEnvelope
	Context *nodeReadContext `json:"context,omitempty"`
}

// nodeConfigWrapper 是节点默认值子资源（W5：**不是独立 kind**，所以没有 apiVersion/kind/spec）。
//
// configRevision / appliedRevision / appliedHash 只读（UI 不提交，也不当 CAS 令牌）。
type nodeConfigWrapper struct {
	Metadata        nodeConfigMeta    `json:"metadata"`
	Namespaces      []string          `json:"namespaces"`
	Vars            map[string]string `json:"vars"`
	Limits          map[string]any    `json:"limits"`
	ConfigRevision  string            `json:"configRevision"`
	AppliedRevision string            `json:"appliedRevision"`
	AppliedHash     string            `json:"appliedHash"`
}

// nodeConfigMeta 是配置子资源的 metadata（只有 CAS 令牌，没有 name/uid/timestamp）。
type nodeConfigMeta struct {
	ResourceVersion string `json:"resourceVersion"`
}

// nodeTokenDoc 是 POST /v1/nodes/{name}/tokens 的动作响应（W4）。
//
// **不是资源**：无 GET / list / DELETE。token 是明文，只在这个响应里出现一次；
// 之后库里只有 sha256，任何读端点都不可能再回显它。
type nodeTokenDoc struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Node       nodeTokenTarget `json:"node"`
	Token      string          `json:"token"`
}

// nodeTokenTarget 是 NodeToken.node（只有名字：令牌不绑 namespace，§7.4）。
type nodeTokenTarget struct {
	Name string `json:"name"`
}

// nodeConfigBatchRequest 是 POST /v1/nodes/config/batch 的请求体（config.md §4.4）。
type nodeConfigBatchRequest struct {
	Updates []nodeConfigBatchUpdate `json:"updates"`
}

// nodeConfigBatchUpdate 是一台节点的更新：name + CAS 令牌 + 可选的三个可写字段。
//
// 三个字段都是**可选**（`?`）：省略 = 这一项不动（沿用当前值）。这一点很重要——
// 只改 vars 的批量编辑若把省略的 namespaces 当成空列表，就是一次静默的权限扩张。
type nodeConfigBatchUpdate struct {
	Name       string            `json:"name"`
	Metadata   json.RawMessage   `json:"metadata"`
	Vars       map[string]string `json:"vars"`
	Limits     map[string]any    `json:"limits"`
	Namespaces *[]string         `json:"namespaces"`
}

// nodeConfigBatchItem 是逐台结果：成功给 config wrapper，失败给 Status 文档。
//
// 失败项只回 Status（code/reason/message/details），**不带**任何凭证值或原始配置值
// （config.md §4.4：错误里不得带原始凭证值）。
type nodeConfigBatchItem struct {
	Name   string             `json:"name"`
	OK     bool               `json:"ok"`
	Config *nodeConfigWrapper `json:"config,omitempty"`
	Error  *statusDoc         `json:"error,omitempty"`
}

// nodeConfigBatchResponse 是批量响应（items 恒为数组）。
type nodeConfigBatchResponse struct {
	Items []nodeConfigBatchItem `json:"items"`
}

// ---------- 读端点 ----------

// list 处理 GET /v1/nodes。
//
// Node 是 cluster 对象：没有 namespace 参数、没有 ns 授权，只有会话鉴权。
func (a *nodeAPI) list(w http.ResponseWriter, r *http.Request) {
	nodes, err := a.d.DB.ListNodes(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	items := make([]nodeReadEnvelope, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		env, err := a.readEnvelope(r.Context(), n, "")
		if err != nil {
			writeAPIError(w, err)
			return
		}
		items = append(items, env)
	}
	writeList(w, http.StatusOK, kindNodeList, listMeta{}, items)
}

// get 处理 GET /v1/nodes/{name}。
//
// `?namespace=X` 时额外给 status.namespaceVars + status.effectiveVars 并标 context.namespace：
// 有效值必须带 ns 上下文（config.md §2.3），因为 local > node > namespace 是**逐键**解析，
// 同一台机器在不同 ns 下的结果可以不同。缺省不带 → 只给机器级事实。
func (a *nodeAPI) get(w http.ResponseWriter, r *http.Request) {
	n, err := a.nodeByName(r.Context(), r.PathValue("name"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	// `?namespace=` **出现即校验**（哪怕值是空串）：显式问「这个 ns 的有效值」却给了
	// 空值，必须报 400 而不是静默退回「只有机器级事实」——后者会让前端以为拿到了
	// 某个 ns 的解析结果。跨 ns 汇总("*")在单对象路径上没有定义 → allowAll=false；
	// 未登录 401 / ns 不存在 404 都由同一份收口给出（namespace_scope.go）。
	nsID := ""
	if r.URL.Query().Has("namespace") {
		if nsID, err = a.d.authorizeQueryNamespace(r, false); err != nil {
			writeAPIError(w, err)
			return
		}
	}
	env, err := a.readEnvelope(r.Context(), n, nsID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeEnvelope(w, http.StatusOK, env)
}

// ---------- 创建 / 更新 / 删除 ----------

// create 处理 POST /v1/nodes：登记一台**离线**节点。
//
// 令牌不在这里发（W4 顺序固定：先登记 → 再 POST /tokens）：store.CreateNode 一定会
// 生成明文令牌，本端点**丢弃**它，只落 sha256（结构上无法泄漏）。
//
// spec 里的可写字段（namespaces/vars/limits/tags）会被接受并立即生效，而不是静默
// 忽略——静默忽略会让调用方以为配置生效了。配置项在 CreateNode **之前**校验，
// 避免"节点建好了但配置被拒"留下半个对象。
func (a *nodeAPI) create(w http.ResponseWriter, r *http.Request) {
	var in nodeSpecPatch
	meta, err := decodeWriteEnvelope(r, kindNode, &in)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	name, err := requireMetaName(meta)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if msg := store.ValidateNodeName(name); msg != "" {
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonValueInvalid, msg))
		return
	}
	if err := nodeValidateConfigInput(in.Namespaces, in.Vars, in.Limits); err != nil {
		writeAPIError(w, err)
		return
	}

	n := &store.Node{Name: name}
	if in.Tags != nil {
		n.Tags = append([]string{}, (*in.Tags)...)
	}
	// 明文令牌被丢弃（`_`）：本端点不签凭证，token 只能从 /tokens 拿。
	if _, err := a.d.DB.CreateNode(r.Context(), n); err != nil {
		writeAPIError(w, err)
		return
	}
	if in.Namespaces != nil || in.Vars != nil || in.Limits != nil {
		cfg := &store.NodeConfig{Vars: in.Vars, Limits: in.Limits}
		if in.Namespaces != nil {
			cfg.Namespaces = append([]string{}, (*in.Namespaces)...)
		}
		// 与 config 子资源同一个入口 / 同一个底层版本（config.md §4.1）：
		// 新节点的 node_configs 还没有行 → 当前令牌是 rv1:<uid>:1。
		if err := a.d.DB.PutNodeConfigByName(r.Context(), name, cfg,
			store.ResourceVersion(n.ID, n.ResourceVersion)); err != nil {
			writeAPIError(w, err)
			return
		}
	}
	env, err := a.readEnvelope(r.Context(), n, "")
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeEnvelope(w, http.StatusCreated, env)
}

// patch 处理 PATCH /v1/nodes/{name}：merge-patch spec（namespaces/vars/limits/tags）。
//
// CAS 令牌只在 body 的 metadata.resourceVersion（缺失 400 / 旧值 409 +
// details.current）；Node.spec 与 config 子资源共享同一个底层版本，所以一次 PATCH
// 改 tags 也会 bump 同一个 rv（store.UpdateNode 的既有事实，不是本文件决定的）。
func (a *nodeAPI) patch(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	var in nodeSpecPatch
	meta, err := decodeWriteEnvelope(r, kindNode, &in)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := nodeCheckPathName(meta, name); err != nil {
		writeAPIError(w, err)
		return
	}
	rv, err := casTokenFromMeta(meta)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	patch := store.NodePatch{Tags: in.Tags, Namespaces: in.Namespaces, Vars: in.Vars, Limits: in.Limits}
	n, err := a.d.DB.UpdateNode(r.Context(), name, patch, rv)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	env, err := a.readEnvelope(r.Context(), n, "")
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeEnvelope(w, http.StatusOK, env)
}

// delete 处理 DELETE /v1/nodes/{name}?resourceVersion=。
//
// 没有请求体：CAS 令牌的唯一承载是 query（§3.4 / W1）。删除只删该对象（槽位 / 节点
// 配置 / 部署随 FK 级联），不级联改引用方。
func (a *nodeAPI) delete(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	rv, err := casTokenFromQuery(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := a.d.DB.DeleteNodeByName(r.Context(), name, rv); err != nil {
		writeAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- 身份令牌（W4） ----------

// issueToken 处理 POST /v1/nodes/{name}/tokens：签发 / 轮换节点身份凭证。
//
// 顺序与语义（W4）：
//   - 节点不存在 → 404（**不隐式创建**节点，也不隐式选 ns）；
//   - 每次 POST = 显式轮换：旧令牌立刻失效（库里换 sha256，已建立的会话在下一次
//     授权写入口重新比对当前哈希时失效）；
//   - 明文只在这个响应里出现**一次**，之后任何读端点都不会回显；
//   - body 可为 `{}` / 空（本端点没有可写字段）。
func (a *nodeAPI) issueToken(w http.ResponseWriter, r *http.Request) {
	if err := nodeRejectNonEmptyBody(r, "tokens"); err != nil {
		writeAPIError(w, err)
		return
	}
	n, err := a.nodeByName(r.Context(), r.PathValue("name"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	token, hash, err := store.NewNodeToken()
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := a.d.DB.SetNodeTokenHash(r.Context(), n.ID, hash); err != nil {
		writeAPIError(w, err)
		return
	}
	// 只记 id：令牌绝不进日志（日志会被收集、转发、贴进工单）。
	a.d.logger().Info("node token issued",
		"event", "api.nodes.token_issued", "node_id", n.ID)
	writeJSON(w, http.StatusOK, nodeTokenDoc{
		APIVersion: apiVersionV1,
		Kind:       kindNodeToken,
		Node:       nodeTokenTarget{Name: n.Name},
		Token:      token,
	})
}

// ---------- 配置子资源（W5） ----------

// configGet 处理 GET /v1/nodes/{name}/config。
//
// 没有配置行时给默认形状（namespaces=["*"]、vars/limits 空、revision 0、
// resourceVersion=1）——首次 PUT 必须能带一个合法的前置条件。
func (a *nodeAPI) configGet(w http.ResponseWriter, r *http.Request) {
	n, err := a.nodeByName(r.Context(), r.PathValue("name"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	cfg, err := a.d.DB.GetNodeConfig(r.Context(), n.ID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nodeConfigWrap(n, cfg))
}

// configPut 处理 PUT /v1/nodes/{name}/config：**全量替换** namespaces/vars/limits。
//
// 只接 metadata.resourceVersion + 三个可写字段；configRevision / appliedRevision /
// appliedHash 是只读派生字段（提交它们 → 400 unknown_field，而不是静默忽略：
// 静默忽略会让人以为"我把 revision 设成 7 了"）。
//
// 省略某个可写字段 = 用缺省值（namespaces→["*"]、vars/limits→{}）：PUT 是全量替换，
// 只有 PATCH 才是 merge。只改部分字段的调用方应该用 POST /config/batch（那里省略=不动）。
func (a *nodeAPI) configPut(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	var in nodeConfigPUT
	if err := nodeDecodeConfigStrictJSON(r, &in); err != nil {
		writeAPIError(w, err)
		return
	}
	meta := objectMeta{}
	if !isEmptyJSON(in.Metadata) {
		var merr error
		if meta, merr = decodeObjectMeta(in.Metadata, map[string]bool{"resourceVersion": true}); merr != nil {
			writeAPIError(w, merr)
			return
		}
	}
	rv, err := casTokenFromMeta(meta)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	n, err := a.nodeByName(r.Context(), name)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	cfg := &store.NodeConfig{Vars: in.Vars, Limits: in.Limits}
	if in.Namespaces != nil {
		cfg.Namespaces = append([]string{}, (*in.Namespaces)...)
	}
	if err := a.d.DB.PutNodeConfigByName(r.Context(), name, cfg, rv); err != nil {
		writeAPIError(w, err)
		return
	}
	// 回读一次拿 applied_*（节点回报的事实）：写入口只写中枢默认值，不碰 applied_*，
	// 用入参拼响应会把 appliedRevision 谎报成 0。
	fresh, err := a.d.DB.GetNodeConfig(r.Context(), n.ID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nodeConfigWrap(n, fresh))
}

// configBatch 处理 POST /v1/nodes/config/batch（config.md §4.4）。
//
// 逐节点：各自 CAS、各自 revision+1、**逐台原子**；一台失败不连坐其它台
// （不是 Playbook 的整包原子规则）。因此请求本身只有两类结果：
//   - 请求体本身不合法（未知字段 / 坏 JSON） → 400，一台都不写；
//   - 请求体合法 → 200 + items[]，逐台 ok / Status。
//
// 省略的字段 = 不动（沿用当前值）：只改 vars 的批量编辑绝不能被解释成
// "把 namespaces 重置成 ['*']"（那是权限扩张，不可接受）。
func (a *nodeAPI) configBatch(w http.ResponseWriter, r *http.Request) {
	var in nodeConfigBatchRequest
	if err := nodeDecodeConfigStrictJSON(r, &in); err != nil {
		writeAPIError(w, err)
		return
	}
	items := make([]nodeConfigBatchItem, 0, len(in.Updates))
	for _, upd := range in.Updates {
		item, err := a.putOneConfig(r.Context(), upd)
		if err != nil {
			items = append(items, nodeConfigBatchItem{
				Name:  strings.TrimSpace(upd.Name),
				OK:    false,
				Error: nodeStatusDocFor(err),
			})
			continue
		}
		items = append(items, nodeConfigBatchItem{Name: strings.TrimSpace(upd.Name), OK: true, Config: item})
	}
	writeJSON(w, http.StatusOK, nodeConfigBatchResponse{Items: items})
}

// putOneConfig 是批量里的一台：读当前值 → 覆盖给了的字段 → 带 CAS 写回。
func (a *nodeAPI) putOneConfig(ctx context.Context, upd nodeConfigBatchUpdate) (*nodeConfigWrapper, error) {
	name := strings.TrimSpace(upd.Name)
	if name == "" {
		return nil, errStatus(http.StatusBadRequest, reasonBadRequest, "updates[].name is required")
	}
	meta := objectMeta{}
	if !isEmptyJSON(upd.Metadata) {
		var merr error
		if meta, merr = decodeObjectMeta(upd.Metadata, map[string]bool{"resourceVersion": true}); merr != nil {
			return nil, merr
		}
	}
	rv, err := casTokenFromMeta(meta)
	if err != nil {
		return nil, err
	}
	n, err := a.nodeByName(ctx, name)
	if err != nil {
		return nil, err
	}
	cfg, err := a.d.DB.GetNodeConfig(ctx, n.ID)
	if err != nil {
		return nil, err
	}
	if upd.Namespaces != nil {
		cfg.Namespaces = append([]string{}, (*upd.Namespaces)...)
	}
	if upd.Vars != nil {
		cfg.Vars = upd.Vars
	}
	if upd.Limits != nil {
		cfg.Limits = upd.Limits
	}
	if err := a.d.DB.PutNodeConfigByName(ctx, name, cfg, rv); err != nil {
		return nil, err
	}
	wrapper := nodeConfigWrap(n, cfg)
	return &wrapper, nil
}

// ---------- 动作端点 ----------

// drain 处理 POST /v1/nodes/{name}/drain：置 draining（不再接新任务）。
//
// 排空是**渐进退役**：已在跑的任务继续到租约自然结束，不主动 Cancel
// （取消是破坏性操作，语义不同）。store 没有独立的 Drain 入口，
// 等价事实是 UpdateNodeStatus(nodeID, NodeStatusDraining)
// （internal/store/node.go:437，同时刷新 last_seen_at 并 bump resource_version）。
func (a *nodeAPI) drain(w http.ResponseWriter, r *http.Request) {
	if err := nodeRejectNonEmptyBody(r, "drain"); err != nil {
		writeAPIError(w, err)
		return
	}
	n, err := a.nodeByName(r.Context(), r.PathValue("name"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := a.d.DB.UpdateNodeStatus(r.Context(), n.ID, store.NodeStatusDraining); err != nil {
		writeAPIError(w, err)
		return
	}
	env, err := a.readEnvelope(r.Context(), n, "")
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeEnvelope(w, http.StatusOK, env)
}

// upgradeNode 处理 POST /v1/nodes/{name}/upgrade：把「去哪拿、拿到后该是什么」推给节点。
//
// 顺序（agent-upgrade.md §4 / CONTRACT.md:1185）：
//  1. 节点不存在 404；
//  2. 装配缺 hub → 500（部署错误，不是调用方错误）；
//  3. 节点不在线 → 409 `node_offline`（没有下发对象，与「有没有二进制」无关）；
//  4. 服务端没有升级源 → 501 `agent_binary_unavailable`；
//  5. `agent_version == 目标版本` 且未 `force` → 200 `{"status":"up_to_date"}`，**不写**
//     `agent_upgrade=sent`、不推 upgrade 帧（已最新的节点不该被多重启一次：agent 侧
//     apply 不比较字节，收到帧就 Restart）；
//  6. 否则写 sent + 推帧 → 200 `{"status":"sent"}`。
//
// `force` 支持 body `{"force":true}` 与 query `?force=1|true`（v3 两种都认；
// curl 用 query 更顺手，控制台用 body）。脏构建（`dev` / 含 `dirty`）**永不判最新**：
// 同一版本号可以对应任意多次不同构建，按版本号判等会让「点了升级却没发生」成为常态。
//
// body 的 version 可选：不给就用中枢自己的版本（二进制就是那次构建的产物，
// 版本与内容必须同源，否则节点会拿「版本对不上」的包）。
func (a *nodeAPI) upgradeNode(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Version string `json:"version"`
		Force   bool   `json:"force"`
	}
	if err := nodeDecodeOptionalJSON(r, &in); err != nil {
		writeAPIError(w, err)
		return
	}
	n, err := a.nodeByName(r.Context(), r.PathValue("name"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if a.d.Hub == nil {
		writeAPIError(w, errStatus(http.StatusInternalServerError, reasonInternal, "node hub is not wired"))
		return
	}
	// 离线先判：没有连接就没有下发对象，这一步与「服务端带没带二进制」无关。
	if !a.d.Hub.Online(n.ID) {
		writeAPIError(w, errStatus(http.StatusConflict, "node_offline", "node has no live connection"))
		return
	}
	if a.upgrade == nil {
		writeAPIError(w, errStatus(http.StatusNotImplemented, "agent_binary_unavailable",
			"this deployment does not carry an agent binary (ANOTIFY_AGENT_BIN)"))
		return
	}
	version := strings.TrimSpace(in.Version)
	if version == "" {
		version = strings.TrimSpace(a.upgrade.TargetVersion())
	}
	sha := strings.TrimSpace(a.upgrade.SHA256())
	if version == "" || sha == "" {
		writeAPIError(w, errStatus(http.StatusNotImplemented, "agent_binary_unavailable",
			"agent binary is not loaded"))
		return
	}
	force := in.Force || nodeForceFromQuery(r)
	if !force && isUniqueVersion(version) && strings.TrimSpace(n.AgentVersion) == version {
		// 已最新：只回事实，不写 sent、不推帧。
		writeJSON(w, http.StatusOK, map[string]any{"status": "up_to_date", "targetVersion": version})
		return
	}
	err = a.d.Hub.RequestUpgrade(r.Context(), n.ID, version, sha, protocol.NodeBinaryPath)
	if errors.Is(err, ws.ErrNodeOffline) {
		// 查库与推帧之间掉线是正常竞态：仍报 409（不假装已下发）。
		writeAPIError(w, errStatus(http.StatusConflict, "node_offline", "node has no live connection"))
		return
	}
	if err != nil {
		writeAPIError(w, err)
		return
	}
	a.d.logger().Info("agent upgrade dispatched",
		"event", "api.nodes.upgrade_sent", "node_id", n.ID,
		"target_version", version, "sha256", sha, "force", force)
	// 动作回执（W1 明确：动作 POST 不套 CAS，也不需要伪装成资源信封）。
	writeJSON(w, http.StatusOK, map[string]any{"status": "sent", "targetVersion": version})
}

// nodeForceFromQuery 读 `?force=`（true / 1 为真，与 v3 的两种写法一致）。
func nodeForceFromQuery(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("force"))) {
	case "true", "1":
		return true
	}
	return false
}

// ---------- 内部工具 ----------

// nodeByName 取节点（空名 / 不存在 → 404，不打日志、不回显"这台机器存在吗"的差别）。
func (a *nodeAPI) nodeByName(ctx context.Context, raw string) (*store.Node, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return nil, errStatus(http.StatusBadRequest, reasonBadRequest, "node name is required")
	}
	return a.d.DB.GetNodeByName(ctx, name)
}

// readEnvelope 组装一台节点的完整信封（metadata + spec + status）。
//
// nsID 非空时额外给 ns 上下文（status.namespaceVars / status.effectiveVars +
// 顶层 context.namespace）。
func (a *nodeAPI) readEnvelope(ctx context.Context, n *store.Node, nsID string) (nodeReadEnvelope, error) {
	cfg, err := a.d.DB.GetNodeConfig(ctx, n.ID)
	if err != nil {
		return nodeReadEnvelope{}, err
	}
	insts, err := a.d.DB.ListInstances(ctx, n.ID)
	if err != nil {
		return nodeReadEnvelope{}, err
	}
	effective, err := a.d.DB.AuthorizedNamespaces(ctx, n.ID)
	if err != nil {
		return nodeReadEnvelope{}, err
	}

	status := nodeStatus{
		HostIP:              n.Facts.HostIP,
		Hostname:            n.Facts.Hostname,
		OS:                  n.Facts.OS,
		Arch:                n.Facts.Arch,
		WorkRoot:            n.Facts.WorkRoot,
		HomeDir:             n.Facts.HomeDir,
		LocalVars:           nodeNonNilStringMap(cfg.LocalVars),
		NodeVars:            nodeNonNilStringMap(cfg.Vars),
		MachineVars:         nodeMergeMachineVars(cfg.LocalVars, cfg.Vars),
		ConfigRevision:      strconv.FormatInt(cfg.Revision, 10),
		AppliedRevision:     strconv.FormatInt(cfg.AppliedRevision, 10),
		AppliedHash:         cfg.AppliedHash,
		EffectiveNamespaces: nodeNonNilStrings(effective),
		AgentVersion:        n.AgentVersion,
		Instances:           nodeInstanceRefs(insts),
	}
	if n.Facts.CPU > 0 {
		status.Capacity = &nodeCapacity{CPU: strconv.Itoa(n.Facts.CPU)}
	}
	if n.LastSeenAt != nil {
		status.LastSeenAt = nodeFormatUnix(*n.LastSeenAt)
	}

	env := nodeReadEnvelope{apiEnvelope: newEnvelope(kindNode, objectMeta{
		Name:              n.Name,
		UID:               n.ID,
		ResourceVersion:   store.ResourceVersion(n.ID, cfg.ResourceVersion),
		CreationTimestamp: nodeFormatUnix(n.CreatedAt),
	}, nodeSpec{
		Namespaces: nodeNonNilStrings(cfg.Namespaces),
		Vars:       nodeNonNilStringMap(cfg.Vars),
		Limits:     nodeNonNilAnyMap(cfg.Limits),
		Tags:       nodeNonNilStrings(n.Tags),
	}, status)}

	if nsID != "" {
		nsCfg, err := a.d.DB.GetNamespaceConfig(ctx, nsID)
		if err != nil {
			return nodeReadEnvelope{}, err
		}
		names := nodeUnionVarNames(cfg.LocalVars, cfg.Vars, nsCfg.Vars)
		effVars, err := a.d.DB.EffectiveVars(ctx, n.ID, nsID, names)
		if err != nil {
			return nodeReadEnvelope{}, err
		}
		status.NamespaceVars = nodeNonNilStringMap(nsCfg.Vars)
		status.EffectiveVars = effVars
		env.Status = status
		// context.namespace 是**对外名字**（slug）：客户端在 query 里写的就是它，
		// 回显 store 的 ns_ id 会让「同一个命名空间在 URL 与响应里叫两个名字」。
		env.Context = &nodeReadContext{Namespace: a.d.namespaceDisplay(ctx, nsID)}
	}
	return env, nil
}

// nodeConfigWrap 把 store 的配置行投影成 wire wrapper（W5）。
func nodeConfigWrap(n *store.Node, cfg *store.NodeConfig) nodeConfigWrapper {
	return nodeConfigWrapper{
		Metadata:        nodeConfigMeta{ResourceVersion: store.ResourceVersion(n.ID, cfg.ResourceVersion)},
		Namespaces:      nodeNonNilStrings(cfg.Namespaces),
		Vars:            nodeNonNilStringMap(cfg.Vars),
		Limits:          nodeNonNilAnyMap(cfg.Limits),
		ConfigRevision:  strconv.FormatInt(cfg.Revision, 10),
		AppliedRevision: strconv.FormatInt(cfg.AppliedRevision, 10),
		AppliedHash:     cfg.AppliedHash,
	}
}

// nodeConfigPUT 是 PUT config 的请求体（W5：只接四个键）。
type nodeConfigPUT struct {
	Metadata   json.RawMessage   `json:"metadata"`
	Namespaces *[]string         `json:"namespaces"`
	Vars       map[string]string `json:"vars"`
	Limits     map[string]any    `json:"limits"`
}

// nodeCheckPathName 校验 body 的 metadata.name（给了就必须与路径一致，§3.2）。
func nodeCheckPathName(meta objectMeta, pathName string) error {
	if meta.Name == "" || pathName == "" || meta.Name == pathName {
		return nil
	}
	return errStatus(http.StatusBadRequest, reasonNameImmutable,
		"metadata.name is immutable and must match the path")
}

// nodeValidateConfigInput 在落库前校验可写配置（避免"节点建好了但配置被拒"）。
func nodeValidateConfigInput(namespaces *[]string, vars map[string]string, limits map[string]any) error {
	if namespaces != nil {
		if err := store.ValidateConfigNamespaces(*namespaces); err != nil {
			return err
		}
	}
	if vars != nil {
		if err := store.ValidateVars(vars); err != nil {
			return err
		}
	}
	if limits != nil {
		if err := store.ValidateLimits(limits); err != nil {
			return err
		}
	}
	return nil
}

// nodeDecodeStrictJSON 解码一个**没有信封**的写请求体（配置子资源 / 批量 / 动作）。
//
// 未知字段一律 400 unknown_field：静默忽略会让调用方以为字段生效了（§3.1 E4）。
func nodeDecodeStrictJSON(r *http.Request, dst any) error {
	raw, err := nodeReadJSONBody(r, 8<<20)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return unknownFieldError(err)
	}
	return nil
}

// nodeDecodeConfigStrictJSON 解码配置写请求，并把 `status` 识别为**只读字段**。
//
// 为什么单独一层：config wrapper（PUT /v1/nodes/{name}/config 与批量）没有 spec/status
// 结构，`status` 不在 dst 里，strict 解码会把它报成 `unknown_field`。但契约 E6 与 S1
// 都规定「写 status → 400 status_not_writable」（信封路径与 ns/system 配置也是后者）。
// 同一个客户端错误在两个入口给两个 reason，调用方只能把它当未知错误。
//
// 判据同时覆盖两种承载：顶层 `status`（单台）与 `updates[].status`（批量）。
func nodeDecodeConfigStrictJSON(r *http.Request, dst any) error {
	raw, err := nodeReadJSONBody(r, 8<<20)
	if err != nil {
		return err
	}
	if nodeHasStatusKey(raw) {
		return errStatus(http.StatusBadRequest, reasonStatusNotWritable, "status is read-only")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return unknownFieldError(err)
	}
	return nil
}

// nodeHasStatusKey 在请求体里找 `status`：顶层键或 `updates[]` 每一项的顶层键。
//
// 只看对象顶层的键：spec 深处的同名键不属于写语义（这里也没有 spec 层）。
func nodeHasStatusKey(raw []byte) bool {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return false // 坏 JSON 交给 strict 解码报可理解的 400
	}
	if _, ok := top["status"]; ok {
		return true
	}
	var updates []json.RawMessage
	if rawUpdates, ok := top["updates"]; ok {
		_ = json.Unmarshal(rawUpdates, &updates)
	}
	for _, item := range updates {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(item, &obj); err != nil {
			continue
		}
		if _, ok := obj["status"]; ok {
			return true
		}
	}
	return false
}

// nodeReadJSONBody 读一次有界的 JSON body（空 body → 400 BadRequest）。
func nodeReadJSONBody(r *http.Request, limit int64) ([]byte, error) {
	if r == nil || r.Body == nil {
		return nil, errStatus(http.StatusBadRequest, reasonBadRequest, errEmptyBody.Error())
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit))
	if err != nil {
		return nil, errStatus(http.StatusBadRequest, reasonBadRequest, "cannot read request body")
	}
	_ = r.Body.Close()
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errStatus(http.StatusBadRequest, reasonBadRequest, errEmptyBody.Error())
	}
	return raw, nil
}

// nodeDecodeOptionalJSON 解码**可选** body（空 body / `null` / `{}` 都当"没给"）。
func nodeDecodeOptionalJSON(r *http.Request, dst any) error {
	if r == nil || r.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return errStatus(http.StatusBadRequest, reasonBadRequest, "cannot read request body")
	}
	_ = r.Body.Close()
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return unknownFieldError(err)
	}
	return nil
}

// nodeRejectNonEmptyBody 拒绝带内容的动作 body（tokens / drain 没有可写字段）。
//
// 允许 `{}`（W4：tokens 动作可接受空 body）与完全空 body；其它内容一律 400——
// 一个字段都不认的端点上"看起来收下了但没生效"是最难查的静默失败。
func nodeRejectNonEmptyBody(r *http.Request, action string) error {
	if r == nil || r.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		return errStatus(http.StatusBadRequest, reasonBadRequest, "cannot read request body")
	}
	_ = r.Body.Close()
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("{}")) || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	return errStatus(http.StatusBadRequest, reasonUnknownField,
		"POST "+action+" takes no writable field (empty body or {})")
}

// nodeStatusDocFor 把 error 翻成 **Status 文档值**（批量响应里的逐项错误）。
//
// 为什么不复用 writeAPIError：那个函数只能往 ResponseWriter 写；批量响应需要把同一份
// 映射塞进 items[].error。映射表刻意与 writeAPIError 保持一致（本域会遇到的几种错误），
// 不是在造第二套语义。
func nodeStatusDocFor(err error) *statusDoc {
	doc := &statusDoc{APIVersion: apiVersionV1, Kind: kindStatus, Status: "Failure"}
	var ae *apiError
	var conflict *store.ResourceVersionConflictError
	switch {
	case errors.As(err, &ae):
		doc.Code, doc.Reason, doc.Message = ae.Code, ae.Reason, ae.Message
	case errors.As(err, &conflict):
		doc.Code, doc.Reason = http.StatusConflict, reasonResourceVersionConflict
		doc.Message = "resourceVersion is stale"
		doc.Details = &statusDetails{Current: map[string]any{"resourceVersion": conflict.CurrentResourceVersion}}
	case errors.Is(err, store.ErrResourceVersionRequired):
		doc.Code, doc.Reason = http.StatusBadRequest, reasonResourceVersionRequired
		doc.Message = "metadata.resourceVersion is required"
	case errors.Is(err, store.ErrResourceVersionInvalid):
		doc.Code, doc.Reason = http.StatusBadRequest, reasonResourceVersionInvalid
		doc.Message = "resourceVersion is malformed"
	case errors.Is(err, store.ErrConfigInvalid):
		doc.Code, doc.Reason = http.StatusBadRequest, reasonValueInvalid
		doc.Message = err.Error()
	case errors.Is(err, store.ErrNotFound):
		doc.Code, doc.Reason = http.StatusNotFound, reasonNotFound
		doc.Message = "not found"
	default:
		doc.Code, doc.Reason = http.StatusInternalServerError, reasonInternal
		doc.Message = err.Error()
	}
	return doc
}

// nodeInstanceRefs 把槽位行投影成 status.instances（恒 `[]`）。
func nodeInstanceRefs(insts []*store.NodeInstance) []nodeInstanceRef {
	out := make([]nodeInstanceRef, 0, len(insts))
	for _, in := range insts {
		if in == nil {
			continue
		}
		ref := nodeInstanceRef{ID: in.ID, Task: in.BusyTask}
		if in.BusyTask != nil && *in.BusyTask != "" {
			ref.Busy = true
		}
		out = append(out, ref)
	}
	return out
}

// nodeMergeMachineVars 是「local > node」逐键合并的机器级结果（config.md §2.3）。
func nodeMergeMachineVars(local, node map[string]string) map[string]string {
	out := make(map[string]string, len(local)+len(node))
	for k, v := range node {
		out[k] = v
	}
	for k, v := range local {
		out[k] = v
	}
	return out
}

// nodeUnionVarNames 收集三层里出现过的全部名字（排序，保证请求确定）。
func nodeUnionVarNames(layers ...map[string]string) []string {
	seen := make(map[string]struct{}, 8)
	for _, layer := range layers {
		for k := range layer {
			seen[k] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// nodeFormatUnix 把 store 的 unix 秒格式化成 RFC3339（与 metadata.creationTimestamp 同口径）。
func nodeFormatUnix(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// nodeNonNilStrings 保证 JSON 是 `[]` 而不是 `null`（不变量 I9 / I12）。
func nodeNonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// nodeNonNilStringMap 保证 JSON 是 `{}` 而不是 `null`。
func nodeNonNilStringMap(v map[string]string) map[string]string {
	if v == nil {
		return map[string]string{}
	}
	return v
}

// nodeNonNilAnyMap 保证 JSON 是 `{}` 而不是 `null`。
func nodeNonNilAnyMap(v map[string]any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	return v
}
