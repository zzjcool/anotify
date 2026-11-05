package ws

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/zzjcool/anotify/internal/store"
)

// NodeAuthenticator 是「节点令牌 → 节点行」需要的最小 store 表面。
//
// 单独抽出来给 **HTTP 端点**复用：GET /v1/node/binary 只需要鉴权，不需要整个
// NodeStore（任务/租约/部署全套）。*store.DB 与 NodeStore 都满足它。
type NodeAuthenticator interface {
	GetNodeByTokenHash(ctx context.Context, tokenHash string) (*store.Node, error)
	// AuthorizeNamespaceForNode 是节点侧的安全收口（#207）：令牌已经把命名空间钉死，
	// 这里只判它仍然存在。**不要**在 ws 包内自己写存在性判断——
	// 将来加 namespace_members 时，收口位置不变（见 store.AuthorizeNamespaceForNode）。
	AuthorizeNamespaceForNode(ctx context.Context, namespaceID string) error
}

// NodeStore 是节点协议需要的最小 store 表面（便于测试注入）。
//
// *store.DB 满足它。
//
// 作用域参数一律是 **namespaceID**（#162）：命名空间资源（节点/能力/任务）的
// 所有读写都按命名空间隔离，命名空间就是唯一作用域参数。
// 它从哪来：token → nodes.token_hash → 那一行的 namespace_id（令牌绑命名空间），
// 所以 hello 帧不需要也不该带命名空间字段（设计文档 §2.1）。
//
// 例外：MarkInstancesDead / ListInstances 以 node_id 定位（node_id 全局唯一），
// 不需要作用域参数。
type NodeStore interface {
	// NodeAuthenticator 是节点令牌鉴权的最小表面（握手与 HTTP 下载端点共用）。
	NodeAuthenticator
	UpdateNodeStatus(ctx context.Context, namespaceID, nodeID, status string) error
	// UpdateNodeAgentVersion 落 hello.agent_version（契约 §3）；只写版本列。
	UpdateNodeAgentVersion(ctx context.Context, namespaceID, nodeID, version string) error
	// RecordAgentUpgrade 落 upgrade_ack 成功（version + commit + agent_upgrade=ok）。
	RecordAgentUpgrade(ctx context.Context, namespaceID, nodeID, version, commit string, at int64) error
	// FailAgentUpgrade 落 upgrade_ack 失败（只改 agent_upgrade，版本列保持旧值）。
	FailAgentUpgrade(ctx context.Context, namespaceID, nodeID, errCode string, at int64) error
	TouchNode(ctx context.Context, namespaceID, nodeID string, at int64) error
	ConfirmCapabilities(ctx context.Context, namespaceID, nodeID string, caps []store.CapabilityRef) ([]string, error)
	// ReconciledApplied 用 hello.applied[] 重建 node_capabilities（v2 / 契约 D4）。
	//
	// 旧路径 ConfirmCapabilities / ConfirmCapabilityDecls 已删：能力归属不再由节点
	// 自报，只能由控制台的部署产生。未知能力（中枢没定义过）仍整帧拒绝。
	//
	// 返回的 ids 与 applied 逐个对应（解析失败的那条是""），供握手后的补推建索引。
	ReconcileApplied(ctx context.Context, namespaceID, nodeID string, applied []store.AppliedRef) (ids []string, stale []store.AppliedRef, err error)
	// MarkDeployResult 落一条来自 apply 帧的部署结果（只碰 state/reason/updated_at）。
	//
	// 行不存在返回 store.ErrNotFound：撤销与回报天然会撞车，调用方按「节点晚了一步」处理。
	MarkDeployResult(ctx context.Context, namespaceID, deployID, state, reason string) error
	UpsertInstances(ctx context.Context, namespaceID, nodeID string, specs []store.InstanceSpec) ([]*store.NodeInstance, error)
	MarkInstancesDead(ctx context.Context, nodeID string) error
	ListInstances(ctx context.Context, nodeID string) ([]*store.NodeInstance, error)
	// DeploymentsForNode 是握手结束后的补推来源：每台节点当前期望的产物（含正文）。
	//
	// 中枢是部署的唯一写者，所以这里不做"本地变体"协商：节点上那份不是中枢期望的，
	// 就把它推下去覆盖（见 nodeSession.resendDeployments）。
	DeploymentsForNode(ctx context.Context, namespaceID, nodeID string) ([]*store.DeploymentDelivery, error)
	GetTask(ctx context.Context, namespaceID, id string) (*store.Task, error)
	// GetCapabilityByID 读能力定义（P1.4：assign.ports 取它的 outputs）。
	//
	// 必须在发布 assign 时读，而不是建任务时快照：能力定义不可变（I8），
	// 而任务行只钉 name@version，与中枢此刻的定义是同一份。
	GetCapabilityByID(ctx context.Context, namespaceID, id string) (*store.CapabilityVersion, error)
	// EnsureTaskToken 写回/读取任务令牌（P1：本地 emit socket 鉴权）。
	//
	// 语义是「只在为空时生成一次」：断线重连重发 assign 时必须拿到同一个，
	// 否则节点上 outbox 的重发会被判成 token 无效（emit-protocol §1）。
	EnsureTaskToken(ctx context.Context, namespaceID, taskID, candidate string) (string, error)
	Transition(ctx context.Context, namespaceID, taskID, from, to string) error
	// ProgressTask 一个事务内完成 progress 帧的三件事：→running + started_at + 续租。
	ProgressTask(ctx context.Context, namespaceID, taskID string, leaseUntil int64) error
	// ReportResultAndCreateTasks 在**一个事务**里完成「父任务终态写入 + 下一跳子任务建行」
	// （#140）。next 为空 == 旧的 ReportResult 行为。
	//
	// P1.5 之后的实际调用形状固定是 `next=nil`：出口已退役（路由只看 emit），
	// 子任务行也不再由 result 建（那件事在 route.Emit 自己的事务里），
	// `tasks.out` 列本身已在迁移 28 删掉。ws 只写终态。
	//
	// 为什么库侧仍要保留「终态 + 子任务行同一个事务」的能力：这是 emit 之外任何
	// 未来的批量建行入口必须满足的形状——拆成两步（先提交终态、后台再建行）就会
	// 留下「父任务 succeeded、子任务行不存在」的永久空洞：
	// ListRedispatchable 只捞已存在的 pending 行，捞不到一条不存在的行，链永久停。
	// 子任务插入失败 → 整体回滚（父任务不留在终态）。
	//
	// 补偿路径：本事务提交后，next 里的行已是 pending + node_id IS NULL + attempts=0，
	// 即使调用方的 publish 丢失/失败/随进程消失，store.ListRedispatchable
	// （internal/lease sweeper，每 5s tick，Redispatch = Dispatcher.RedispatchTask）
	// 会把它捞回来重派。所以 publish 不需要也不该进事务。
	ReportResultAndCreateTasks(ctx context.Context, namespaceID, taskID, status string,
		summary string, artifacts []string, errMsg string, next []*store.Task) error
	ExtendLease(ctx context.Context, namespaceID, taskID string, leaseUntil int64) error
	// ExtendLeasesForTasks 是心跳的真正入口（#218 根修）：**只续本连接名下的这批任务**。
	// 旧的 ExtendLeasesForNode（按节点批续）不在这个接口里了——生产路径上不该再有人能调到它。
	ExtendLeasesForTasks(ctx context.Context, namespaceID, nodeID string, taskIDs []string, leaseUntil int64) (int, error)
	// ResolveLostTasks 在连接结束时把它名下的任务交还给调度（clean → pending / unclean → abandoned）。
	ResolveLostTasks(ctx context.Context, namespaceID, nodeID string, owners []store.TaskOwner,
		policy store.LostPolicy, reason string, now int64) (requeued, abandoned int, err error)
	// ReclaimStaleSlots 清掉「槽位指向的任务已经不在这台机器上跑了」的钉子（#218 兜底）。
	ReclaimStaleSlots(ctx context.Context, namespaceID, nodeID string, now int64) (int, error)
}

// 握手鉴权失败的错误类型（供 HTTP 层区分 401 / 403）。
var (
	// ErrMissingToken 请求没带 Authorization: Bearer。
	ErrMissingToken = errors.New("missing node token")
	// ErrBadToken token 无效（查不到节点）。
	ErrBadToken = errors.New("invalid node token")
	// ErrNodeDraining 节点在排空，拒绝接入新连接。
	ErrNodeDraining = errors.New("node is draining")
	// ErrNamespaceGone 令牌指向的命名空间已被删除（#207）。与 ErrBadToken 同样是 403：
	// 对节点侧不区分「令牌无效」和「命名空间没了」—— 两者对客户端都是「这个令牌不能用了」。
	ErrNamespaceGone = errors.New("node namespace no longer exists")
)

// AuthenticateNode 在 **Upgrade 之前**完成握手鉴权（契约 §6.1 / §10.3）。
//
// token 是 **opaque**（不是 JWT）：查 nodes.token_hash（sha256）得到节点那一行。
// 行上的 (node_id, namespace_id) 就是这条连接的全部身份：
// **namespace_id 是后续所有 store 调用的作用域**（令牌绑命名空间）。
// 拿到这些值之后，该连接上所有帧都用它们，**不再查库**。
//
// **命名空间收口（#207）**：token 合法之后还要过 store.AuthorizeNamespaceForNode——
// 令牌签发时命名空间是存在的，但命名空间可能已经被删（迁移后没有任何 FK 能
// 挡住一条活着的连接），旧令牌必须立刻失效。收口放在这里而不是 handler 里，
// 是为了让「HTTP 会话路径」和「节点令牌路径」共用 store 包内的同一处判断。
//
// 返回的错误：ErrMissingToken → 401；ErrBadToken / ErrNodeDraining → 403；
// 令牌指向的命名空间已不存在 → ErrNamespaceGone → 403。
// 关键：鉴权失败时**绝不能**返回 101（升级成功后才发现没权限，
// 会让客户端以为连上了）。
func AuthenticateNode(ctx context.Context, st NodeAuthenticator, r *http.Request) (*store.Node, error) {
	node, err := lookupNodeByToken(ctx, st, r)
	if err != nil {
		return nil, err
	}
	if node.Status == store.NodeStatusDraining {
		return nil, ErrNodeDraining
	}
	return node, nil
}

// lookupNodeByToken 是令牌 → 节点行的**唯一**实现（不含连接层语义）。
//
// AuthenticateNode 与 AuthenticateNodeHTTP 都在它上面加各自的策略：
// 前者多了「draining 不许接入新连接」，后者没有（见下）。
func lookupNodeByToken(ctx context.Context, st NodeAuthenticator, r *http.Request) (*store.Node, error) {
	token := BearerToken(r)
	if token == "" {
		return nil, ErrMissingToken
	}
	node, err := st.GetNodeByTokenHash(ctx, store.HashNodeToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrBadToken
	}
	if err != nil {
		return nil, fmt.Errorf("lookup node by token: %w", err)
	}
	// 安全收口：命名空间仍存在吗（#207）。错误一律 fail closed。
	if err := st.AuthorizeNamespaceForNode(ctx, node.NamespaceID); err != nil {
		if errors.Is(err, store.ErrNamespaceNotFound) {
			return nil, ErrNamespaceGone
		}
		return nil, fmt.Errorf("authorize node namespace: %w", err)
	}
	return node, nil
}

// AuthenticateNodeHTTP 是**非连接型** HTTP 端点（GET /v1/node/binary）的节点令牌鉴权。
//
// 与 AuthenticateNode 的差别只有一条：不把 draining 当拒绝。排空只意味着
// 「不再接新任务」；下载自己的升级包是维护动作，排空中的机器更需要它。
// 令牌 + 命名空间收口完全共用 lookupNodeByToken，安全语义不会分叉。
func AuthenticateNodeHTTP(ctx context.Context, st NodeAuthenticator, r *http.Request) (*store.Node, error) {
	return lookupNodeByToken(ctx, st, r)
}

// BearerToken 从 Authorization 头取 Bearer token（大小写不敏感的前缀）。
func BearerToken(r *http.Request) string {
	if r == nil {
		return ""
	}
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// HTTPStatusForAuthError 把握手鉴权错误映射成 HTTP 状态码（都在 Upgrade 之前返回）。
func HTTPStatusForAuthError(err error) int {
	switch {
	case errors.Is(err, ErrMissingToken):
		return http.StatusUnauthorized
	case errors.Is(err, ErrBadToken), errors.Is(err, ErrNodeDraining), errors.Is(err, ErrNamespaceGone):
		return http.StatusForbidden
	default:
		return http.StatusInternalServerError
	}
}

// helloError 是处理 hello 帧时可能出现的协议错误。
type helloError struct {
	code    string
	message string
}

func (e *helloError) Error() string { return e.message }

// codeOf 把 hello 处理错误映射成协议错误码。
func codeOf(err error) string {
	var he *helloError
	if errors.As(err, &he) {
		return he.code
	}
	return ErrCodeBadFrame
}

// applyHello 校验并落库 hello 帧带来的陈述。
//
// 三件事，任一失败都拒绝整帧：
//  1. 协议版本 / 身份一致性
//  2. 已应用产物：name@version 必须在中枢已定义，否则 unknown_capability；
//     并按 applied[] 全量重建 node_capabilities（契约 D4）
//  3. 实例申报：数量受 nodes.max_instances 硬顶约束，否则 too_many_instances
//
// 顺序有讲究：先校能力再落实例——一个声称缓存了不存在产物的节点不该先占住槽位。
//
// 作用域一律用 node.NamespaceID：hello 帧不带命名空间（一个字节不改），
// 节点属于哪个命名空间完全由令牌决定（设计文档 §2.1）。
func applyHello(ctx context.Context, st NodeStore, node *store.Node, f *HelloFrame) (map[string]string, []store.AppliedRef, error) {
	// 协议版本
	if f.Protocol != 0 && f.Protocol != ProtocolVersion {
		return nil, nil, &helloError{
			code:    ErrCodeProtocolMismatch,
			message: fmt.Sprintf("unsupported protocol %d (server speaks %d)", f.Protocol, ProtocolVersion),
		}
	}
	// 协议要求节点自报的身份与 token 对应的节点一致（防「拿 A 的 token 冒充 B」）
	if f.NodeID != "" && f.NodeID != node.ID {
		return nil, nil, &helloError{
			code:    ErrCodeBadFrame,
			message: fmt.Sprintf("hello node_id %q does not match the token's node %q", f.NodeID, node.ID),
		}
	}

	// ① 已应用产物对账
	applied, stale, err := reconcileHelloApplied(ctx, st, node, f)
	if err != nil {
		return nil, nil, err
	}

	// ② 实例申报
	if _, err := st.UpsertInstances(ctx, node.NamespaceID, node.ID, instanceSpecs(f)); err != nil {
		if errors.Is(err, store.ErrTooManyInstances) {
			return nil, nil, &helloError{code: ErrCodeTooManyInstances, message: err.Error()}
		}
		return nil, nil, err
	}

	// ③ 槽位兜底（#218）：清掉「槽位指向的任务已经不在这台机器上跑了」的钉子。
	//
	// 为什么在 hello 里做（而且是幂等的一刷）：UpsertInstances 刻意保留 busy_task
	// （“重启不该把正在跑的任务丢掉”），但中枢被 kill -9 / 旧版本升级上来时没人清它，
	// 槽位就永久卡住（节点显示 online 却再也接不到任务）。hello 是唯一「进程刚起来」的确定信号。
	//
	// best-effort：失败只 warn，**绝不拒绝 hello**（节点能上线才有机会被派发）。
	// 顺序在 UpdateNodeStatus(online) 之前：先把钉子清掉，再宣布自己可接单。
	if _, err := st.ReclaimStaleSlots(ctx, node.NamespaceID, node.ID, store.Now()); err != nil {
		slog.Warn("reclaim stale slots failed",
			"event", "node.ws.hello.reclaim_failed",
			"node_id", node.ID,
			"error", err.Error(),
		)
	}

	// ④ 上线
	if err := st.UpdateNodeStatus(ctx, node.NamespaceID, node.ID, store.NodeStatusOnline); err != nil {
		return nil, nil, err
	}
	return applied, stale, nil
}

// instanceSpecs 把 hello 帧的实例申报转成 store 的形状（两处调用共用，避免漂移）。
func instanceSpecs(f *HelloFrame) []store.InstanceSpec {
	specs := make([]store.InstanceSpec, 0, len(f.Instances))
	for _, in := range f.Instances {
		specs = append(specs, store.InstanceSpec{Slot: in.Slot, Concurrency: in.Concurrency})
	}
	return specs
}

// reassertNodeState 把「这个节点现在在线、这些槽位活着」再写一遍（审查 n1）。
//
// 调用时机必须是 hub.Register **之后**、welcome **之前**（见 handleHello）。
// 为什么：applyHello 里的两条 node 级写（UpsertInstances alive=1 + online）发生在
// Register 之前，而那正是旧连接 cleanup 的 ② 唯一可能看见 current()==nil 的窗口——
// 旧连接完全可能把 alive/online 又写回 0/offline，而新连接之后再也不会重写
// （TouchNode 只写 last_seen_at，alive=1 只有 hello 写）→ 节点「在线却接不到活」
// 且不自愈（#218 的症状）。
//
// 为什么写在 Register 之后就能收住：Register 之后 current() 不再返回 nil，
// 旧连接 cleanup 的判据（cur == nil || cur == s）看到的是我 → 跳过 node 级写入。
// 这一次重写是幂等的：UpsertInstances 只把 alive/并发写回 1，**保留 busy_task**
// （正在跑的任务不会因为重连丢掉槽位）。
func reassertNodeState(ctx context.Context, st NodeStore, node *store.Node, f *HelloFrame) error {
	if _, err := st.UpsertInstances(ctx, node.NamespaceID, node.ID, instanceSpecs(f)); err != nil {
		if errors.Is(err, store.ErrTooManyInstances) {
			return &helloError{code: ErrCodeTooManyInstances, message: err.Error()}
		}
		return err
	}
	return st.UpdateNodeStatus(ctx, node.NamespaceID, node.ID, store.NodeStatusOnline)
}

// reconcileHelloApplied 用 hello.applied[] 重建这台节点的能力归属（契约 D4）。
//
// v2 的核心语义变化：节点不再「声明我会跑什么」，只陈述「我缓存了哪些产物」。
// 中枢据此做两件事：
//  1. 每个 applied 的 name@version 必须在中枢已定义，否则整帧 unknown_capability
//     （产物就是从中枢部署过去的，出现中枢不认识的能力说明状态已经不一致，
//     含糊过去只会让后面的派发变成「ack 之后 failed」）；
//  2. 全量重建 node_capabilities（老的 DELETE + INSERT 语义保留）。
//
// 注意 **不做的事**：不会因为 applied 里出现一条没部署过的能力就把归属给它。
// 唯一的写者是控制台的部署；这里只做对账，并把不一致的行打成 pending ——
// 随后 handleHello 会按中枢的期望重新下发（中枢覆盖，见 resendDeployments）。
//
// 返回值：capability_id → 节点实际缓存的 digest，供补推比对（不报 = 节点上没有）。
func reconcileHelloApplied(ctx context.Context, st NodeStore, node *store.Node, f *HelloFrame) (map[string]string, []store.AppliedRef, error) {
	refs := make([]store.AppliedRef, 0, len(f.Applied))
	for _, a := range f.Applied {
		refs = append(refs, store.AppliedRef{
			Name:    a.Capability,
			Version: a.Version,
			Digest:  a.Digest,
		})
	}
	ids, stale, err := st.ReconcileApplied(ctx, node.NamespaceID, node.ID, refs)
	if err != nil {
		// 只剩「形状坏掉」这一类（空能力名）→ bad_frame：那是节点 bug，必须响。
		var declErr *store.CapabilityDeclError
		if errors.As(err, &declErr) {
			return nil, nil, &helloError{code: ErrCodeBadFrame, message: err.Error()}
		}
		return nil, nil, err
	}
	applied := make(map[string]string, len(ids))
	for i, id := range ids {
		if id == "" {
			continue
		}
		applied[id] = refs[i].Digest
	}
	return applied, stale, nil
}

// welcomeFrameFor 构造握手成功的 welcome 帧。
//
// prune 是「节点缓存里有、中枢已经没有了」的能力引用：节点收到后删掉对应的
// 缓存目录与索引。空就不带这个键（老 agent 不认识它，带空的也没意义）。
func welcomeFrameFor(node *store.Node, prune []store.AppliedRef) *WelcomeFrame {
	w := &WelcomeFrame{
		Type:         NodeFrameWelcome,
		NodeID:       node.ID,
		Protocol:     ProtocolVersion,
		HeartbeatSec: store.HeartbeatSec,
		LeaseTTLSec:  store.LeaseTTLSec,
	}
	for _, p := range prune {
		w.Prune = append(w.Prune, WelcomePrune{Capability: p.Name, Version: p.Version})
	}
	return w
}
