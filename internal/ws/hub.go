// Package ws 是 v4 中枢 ↔ 节点的 WebSocket 接线层：认证会话、配置子协议、执行
// 子协议（dispatch / accept / grant / renew / result / cancel）、emit 转发、历史
// 日志请求与人工部署通知。
//
// 三条与 store 的分工不能越界：
//
//   - **会话只证明「这是哪台机器」**：连接第一次 hello 里的 token 换成当前
//     nodes.token_hash，NodeAuthorization{NodeID, NodeTokenHash} 从这一刻起固定；
//     之后每个授权写入口（grant/renew/result/cancel-ack/emit）都把它交给 store 的
//     *Authorized 事务重新比对库里的当前哈希与 ns 授权。会话不做 TOCTOU 预检后
//     直接写库，也不从请求体取 caller 自报的 nodeId。
//   - **dispatch 不是授权**：派发帧只带 assignmentId / proposedAttempt / 30s 窗口；
//     lease/token 只由 GrantLeaseAuthorized 生成，slot 只能回带节点 accept 的同一个。
//   - **物理回收只认事实**：缺 busy / 空 instances / 未知都**不是** idle；rehello 与
//     断连都不假释放 busy，只有 cancel.ack 的 processGroupReaped=true 或明确的
//     SlotObservation 才落 reaped。
//
// 共享 wire 定义只在 internal/protocol（T3 唯一作者）。本包不复制镜像结构、不写
// 第二个 Decode，也不向 wire 增加私有字段；未知帧按协议忽略，方向错误的帧断开连接
// （没有 T3 定义过的 error 帧可回，不发明一个）。
package ws

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
)

// Clock 是秒级时钟（测试注入）。
type Clock func() int64

// 会话层错误（供 G3 的 HTTP 层翻成 4xx/5xx；不冒充 store 哨兵）。
var (
	// ErrNodeOffline 表示目标节点当前没有已认证连接（不是「结果为空」）。
	ErrNodeOffline = errors.New("ws: 节点不在线")
	// ErrNoAttempt 表示任务没有可定位的执行 attempt（未 grant 或历史行不存在）。
	ErrNoAttempt = errors.New("ws: 没有可定位的执行 attempt")
	// ErrLogsTimeout 表示节点在期限内没有回 logs_response。
	ErrLogsTimeout = errors.New("ws: 等待日志回包超时")
	// ErrLogsCorrelation 表示回包的 requestId/taskId/attempt 与请求不匹配。
	ErrLogsCorrelation = errors.New("ws: 日志回包与请求不匹配")
	// ErrSessionClosed 表示连接已经关闭（写失败的直接原因）。
	ErrSessionClosed = errors.New("ws: 会话已关闭")
	// errProtocolViolation 表示对端发了方向错误的帧（断开连接）。
	errProtocolViolation = errors.New("ws: 协议违例")
)

// Option 是 Hub 的可选配置。
type Option func(*Hub)

// WithLogger 注入结构化 logger（默认 slog.Default()）。
func WithLogger(logger *slog.Logger) Option {
	return func(h *Hub) {
		if logger != nil {
			h.log = logger
		}
	}
}

// WithClock 注入秒级时钟（测试/回放用）。
func WithClock(now Clock) Option {
	return func(h *Hub) {
		if now != nil {
			h.now = now
		}
	}
}

// WithHelloTimeout 覆盖首帧 hello 的等待上限（默认 10s）。
func WithHelloTimeout(d time.Duration) Option {
	return func(h *Hub) {
		if d > 0 {
			h.helloTimeout = d
		}
	}
}

// WithWriteTimeout 覆盖单帧写上限（默认 10s）。
func WithWriteTimeout(d time.Duration) Option {
	return func(h *Hub) {
		if d > 0 {
			h.writeTimeout = d
		}
	}
}

// logRequest 是一条在飞的 logs 请求（按 requestId 配对）。
//
// ch 容量 1 + 「交给 waiter 前先从 map 移除」保证每个请求最多被交付一次：
// 重复/晚到的回包只能命中「没有登记」分支，不会在 cap1 通道上阻塞读循环。
type logResponse struct {
	frame *protocol.LogsResponseFrame
	err   error
}

type logRequest struct {
	taskID  string
	attempt int
	nodeID  string
	ch      chan logResponse
}

// Hub 是节点会话的注册表与运行期编排入口。
type Hub struct {
	db     *store.DB
	router *route.Router
	log    *slog.Logger
	now    Clock

	helloTimeout time.Duration
	writeTimeout time.Duration

	mu       sync.Mutex
	sessions map[string]*Session // nodeID → session（同一节点只保留最新连接）
	logs     map[string]*logRequest
}

// NewHub 构造 Hub（不启动后台循环；调度由调用方驱动 DispatchOnce 或 RunDispatchLoop）。
func NewHub(db *store.DB, router *route.Router, opts ...Option) *Hub {
	h := &Hub{
		db: db, router: router, log: slog.Default(),
		now:          func() int64 { return store.Now() },
		helloTimeout: 10 * time.Second,
		writeTimeout: 10 * time.Second,
		sessions:     map[string]*Session{},
		logs:         map[string]*logRequest{},
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// DB 返回底层 store。
func (h *Hub) DB() *store.DB { return h.db }

// Router 返回共享路由器。
func (h *Hub) Router() *route.Router { return h.router }

// Now 返回当前秒级时刻。
func (h *Hub) Now() int64 {
	if h.now == nil {
		return store.Now()
	}
	return h.now()
}

// ServeHTTP 把一条 HTTP 升级请求接成节点会话。
//
// 认证在**首帧 hello** 完成（协议规定的握手）：token 换成 nodes.token_hash 后，
// 会话身份固定；失败即断开（没有「匿名连接」这种状态）。
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// 节点不是浏览器：不依赖 Origin。Node 身份由 hello token 决定。
		InsecureSkipVerify: true,
	})
	if err != nil {
		h.log.Warn("node stream upgrade failed", "err", err)
		return
	}
	sess := &Session{hub: h, conn: conn}
	if err := h.handshake(r.Context(), sess); err != nil {
		h.log.Warn("node handshake failed", "err", err)
		_ = conn.Close(websocket.StatusPolicyViolation, "handshake failed")
		return
	}
	h.attach(sess)
	h.log.Info("node connected", "node_id", sess.nodeID, "node", sess.nodeName)
	h.serveSession(r.Context(), sess)
}

// attach 注册会话：同一节点的新连接顶掉旧连接（旧连接随后写失败并 detach）。
func (h *Hub) attach(sess *Session) {
	h.mu.Lock()
	old := h.sessions[sess.nodeID]
	h.sessions[sess.nodeID] = sess
	h.mu.Unlock()
	if old != nil && old != sess {
		old.close(websocket.StatusPolicyViolation, "superseded by a newer connection")
	}
}

// detach 把会话从注册表移除并标记节点 offline（busy 账本保留：断连 != 进程已回收）。
//
// 只有**当前**会话的退出才改节点状态：同一节点的新连接顶掉旧连接时，旧读循环随后
// 退出，不能把刚上线的新会话又标成 offline。
func (h *Hub) detach(sess *Session) {
	h.mu.Lock()
	current := h.sessions[sess.nodeID]
	if current == sess {
		delete(h.sessions, sess.nodeID)
	}
	h.mu.Unlock()
	sess.closedFlag()
	if current != sess {
		// 被新连接顶掉的旧读循环：不唤醒日志 waiter（它们属于当前会话），
		// 也不把刚上线的新会话标 offline。
		return
	}
	// 断开即唤醒该节点上所有在飞的日志请求（真实错误，不是等超时）。
	h.wakeLogsForNode(sess.nodeID, ErrNodeOffline)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.db.MarkNodeOffline(ctx, sess.nodeID, h.Now()); err != nil {
		h.log.Warn("mark node offline failed", "node_id", sess.nodeID, "err", err)
	}
	if err := h.db.MarkInstancesDead(ctx, sess.nodeID); err != nil {
		h.log.Warn("mark instances dead failed", "node_id", sess.nodeID, "err", err)
	}
	h.log.Info("node disconnected", "node_id", sess.nodeID, "node", sess.nodeName)
}

// session 返回某节点当前的已认证会话；没有则 nil。
func (h *Hub) session(nodeID string) *Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[nodeID]
}

// Online 报告节点当前是否有已认证连接。
func (h *Hub) Online(nodeID string) bool { return h.session(nodeID) != nil }

// handshake 完成首帧 hello 的认证与状态落库，然后回 welcome。
func (h *Hub) handshake(ctx context.Context, sess *Session) error {
	readCtx, cancel := context.WithTimeout(ctx, h.helloTimeout)
	_, data, err := sess.conn.Read(readCtx)
	cancel()
	if err != nil {
		return err
	}
	frame, err := protocol.Decode(data)
	if err != nil {
		return err
	}
	hello, ok := frame.(*protocol.HelloFrame)
	if !ok {
		return errors.New("首帧必须是 hello")
	}
	if err := h.authenticate(ctx, sess, hello); err != nil {
		return err
	}
	if err := h.applyHello(ctx, sess, hello); err != nil {
		return err
	}
	if err := h.sendWelcome(ctx, sess); err != nil {
		return err
	}
	// 重放在 welcome 之后：节点先完成配置对齐，再收到「中枢希望它持有的部署」。
	return h.replayDeployments(ctx, sess)
}

// authenticate 把 hello.token 换成本次会话的固定身份。
//
// 只按 token hash 查库：请求体里的 name 必须与 token 所属节点一致，否则拒绝——
// 不允许「拿 A 的 token 报 B 的名字」。
func (h *Hub) authenticate(ctx context.Context, sess *Session, hello *protocol.HelloFrame) error {
	if strings.TrimSpace(hello.Token) == "" {
		return errors.New("hello 缺少 token")
	}
	if strings.TrimSpace(hello.Name) == "" {
		return errors.New("hello 缺少 name")
	}
	tokenHash := store.HashNodeToken(hello.Token)
	node, err := h.db.GetNodeByTokenHash(ctx, tokenHash)
	if err != nil {
		return err
	}
	if node.Name != hello.Name {
		return errors.New("hello.name 与 token 所属节点不一致")
	}
	sess.nodeID = node.ID
	sess.nodeName = node.Name
	sess.tokenHash = tokenHash
	sess.auth = store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: tokenHash}
	return nil
}

// applyHello 落一次 hello 上报（facts/能力/ns/本机配置）并处理槽位事实。
//
// rehello 走同一条路径：只更新事实，**不**因为重连而假定旧占用已回收
// （旧 busy 只能被明确的 SlotObservation 或 cancel.ack 清掉）。
func (h *Hub) applyHello(ctx context.Context, sess *Session, hello *protocol.HelloFrame) error {
	if err := h.db.RecordHello(ctx, sess.nodeID, hello.Facts, hello.Capabilities,
		hello.Namespaces, hello.LocalVars, limitsToMap(hello.LocalLimits),
		hello.AgentVersion, hello.AgentCommit); err != nil {
		return err
	}
	if err := h.db.UpdateNodeStatus(ctx, sess.nodeID, store.NodeStatusOnline); err != nil {
		return err
	}
	if hello.AppliedRevision != "" {
		revision, err := strconv.ParseInt(strings.TrimPrefix(hello.AppliedRevision, "rev-"), 10, 64)
		if err == nil && revision >= 0 {
			if err := h.db.RecordApplied(ctx, sess.nodeID, revision, hello.AppliedHash); err != nil {
				return err
			}
		}
	}
	if err := h.applyInstances(ctx, sess.nodeID, hello.Instances); err != nil {
		return err
	}
	return nil
}

// applyInstances 处理 hello / config.applied 附带的物理槽位事实。
//
// 规则（A3）：
//   - slot 必须 presence 且 ≥0（0 合法），缺失/负数/重复项不更新账本；
//   - 申报的 slot 集合 upsert 成真实槽位（这是「本机有几格」，不是「哪格空闲」）；
//   - Busy 三态原样交给 store.ReclaimStaleSlots：nil/缺 instances 都是未知，
//     绝不折算成 idle。
func (h *Hub) applyInstances(ctx context.Context, nodeID string, instances []protocol.HelloInstance) error {
	if len(instances) == 0 {
		return nil
	}
	now := h.Now()
	specs := make([]store.InstanceSpec, 0, len(instances))
	observations := make([]store.SlotObservation, 0, len(instances))
	seen := map[int]bool{}
	for _, inst := range instances {
		if inst.Slot == nil || *inst.Slot < 0 || seen[*inst.Slot] {
			continue
		}
		seen[*inst.Slot] = true
		specs = append(specs, store.InstanceSpec{Slot: *inst.Slot})
		observations = append(observations, store.SlotObservation{
			Slot: *inst.Slot, Busy: inst.Busy,
			TaskID: inst.TaskID, Attempt: inst.Attempt, LeaseID: inst.LeaseID,
			AssignmentID: inst.AssignmentID, ProposedAttempt: inst.ProposedAttempt,
			ObservedAt: now,
		})
	}
	if len(specs) == 0 {
		return nil
	}
	if _, err := h.db.UpsertInstances(ctx, nodeID, specs); err != nil {
		return err
	}
	if _, err := h.db.ReclaimStaleSlots(ctx, nodeID, observations, now); err != nil {
		return err
	}
	return nil
}

// sendWelcome 回 welcome（config.get 复用同形，D-I2）。
func (h *Hub) sendWelcome(ctx context.Context, sess *Session) error {
	cfg, err := h.db.GetNodeConfig(ctx, sess.nodeID)
	if err != nil {
		return err
	}
	namespaces, err := h.db.AuthorizedNamespaces(ctx, sess.nodeID)
	if err != nil {
		return err
	}
	limits, err := store.TypedMachineLimits(cfg.Limits)
	if err != nil {
		return err
	}
	return sess.Send(ctx, &protocol.WelcomeFrame{
		Type:           protocol.FrameWelcome,
		ConfigRevision: strconv.FormatInt(cfg.Revision, 10),
		NodeVars:       cfg.Vars,
		NodeLimits:     limits,
		Namespaces:     namespaces,
	})
}

// DispatchOnce 扫描一批可派发任务，为每条任务**稳定**记录一个候选（assignment）
// 并把 task.dispatch 推给在线节点。
//
// 安全性/稳定性（R1-C2）：
//   - 候选写入走 store.ProposeTaskAssignment：未到期的同 Task 提案原样返回（多
//     sweep 不 churn）；只有到期才换绑定。派发**不授执行权**：没有 lease / attempt /
//     token / slot，节点 accept 之后才走 GrantLeaseAuthorized。
//   - ConfigRevision 与 welcome 同源 —— 真实 node_configs.revision（不是
//     nodes.resource_version）：真实 anode 用它做 stale_config 判定，取错会让所有
//     派发永远 stale。
func (h *Hub) DispatchOnce(ctx context.Context, limit int) (int, error) {
	now := h.Now()
	tasks, err := h.db.ListDispatchableTasks(ctx, now, limit)
	if err != nil {
		return 0, err
	}
	dispatched := 0
	configRevisions := map[string]string{}
	for _, task := range tasks {
		if task.CancelRequestedAt != nil {
			continue
		}
		nodes, _, err := h.db.EligibleNodesForTask(ctx, task)
		if err != nil {
			h.log.Warn("eligible nodes failed", "task_id", task.ID, "err", err)
			continue
		}
		if len(nodes) == 0 {
			continue
		}
		free, err := h.db.FreeSlotsByNode(ctx, nodeIDs(nodes))
		if err != nil {
			h.log.Warn("free slots failed", "task_id", task.ID, "err", err)
			continue
		}
		nsVars := h.namespaceVarsFor(ctx, task)
		proposed := task.Attempt + 1
		for _, node := range nodes {
			if free[node.ID] <= 0 {
				continue
			}
			sess := h.session(node.ID)
			if sess == nil {
				continue
			}
			proposal, perr := h.db.ProposeTaskAssignment(ctx, store.AssignmentProposalInputs{
				TaskID: task.ID, NodeID: node.ID, ProposedAttempt: proposed, Now: now,
			})
			if perr != nil {
				h.log.Warn("propose assignment failed", "task_id", task.ID, "node_id", node.ID, "err", perr)
				continue
			}
			if proposal.NodeID != node.ID {
				// 未到期提案属于另一节点：本轮不换绑定（等它到期或对方报事实）。
				h.log.Debug("task already proposed to another node",
					"task_id", task.ID, "held_by", proposal.NodeID, "candidate", node.ID)
				continue
			}
			revision, ok := configRevisions[node.ID]
			if !ok {
				cfg, cerr := h.db.GetNodeConfig(ctx, node.ID)
				if cerr != nil {
					h.log.Warn("read node config failed", "node_id", node.ID, "err", cerr)
					continue
				}
				revision = strconv.FormatInt(cfg.Revision, 10)
				configRevisions[node.ID] = revision
			}
			frame := &protocol.TaskDispatchFrame{
				Type:            protocol.FrameTaskDispatch,
				TaskID:          task.ID,
				AssignmentID:    proposal.AssignmentID,
				ProposedAttempt: proposal.ProposedAttempt,
				Namespace:       h.namespaceSlug(ctx, task.NamespaceID),
				Capability: protocol.CapabilityRef{
					Name: task.Capability, Version: task.Version, Digest: task.Digest,
				},
				Params:                task.Params,
				NamespaceVars:         nsVars,
				NamespaceVarsRevision: strconv.FormatInt(task.NamespaceVarsRevision, 10),
				ConfigRevision:        revision,
				AssignmentExpiresAt:   timeUnix(proposal.ExpiresAt),
			}
			if err := sess.Send(ctx, frame); err != nil {
				h.log.Warn("send dispatch failed", "task_id", task.ID, "node_id", node.ID, "err", err)
				h.detachSessionOnWriteFailure(sess, err)
				continue
			}
			dispatched++
			break
		}
	}
	return dispatched, nil
}

// RunDispatchLoop 按 interval 周期扫描派发，直到 ctx 结束。
func (h *Hub) RunDispatchLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := h.DispatchOnce(ctx, 100); err != nil {
				h.log.Warn("dispatch sweep failed", "err", err)
			}
		}
	}
}

// CancelTask 记录取消意图，并在任务仍在跑时按**完整历史绑定**把 task.cancel 推给
// 执行它的节点（taskId+attempt+leaseId+slot）。
//
// pending 的取消由 store 直接终结（没有执行者，不需要 ack）；running 的终态由
// cancel.ack（processGroupReaped=true）裁定——这里不假装释放槽位。
func (h *Hub) CancelTask(ctx context.Context, namespaceID, taskID, reason string, graceSec int) error {
	if !protocol.CancelReason(reason).Valid() {
		reason = string(protocol.CancelReasonUser)
	}
	if graceSec <= 0 {
		graceSec = int(protocol.LeaseTermGrace / time.Second)
	}
	if err := h.db.RequestCancel(ctx, taskID, reason, h.Now()); err != nil {
		return err
	}
	task, err := h.db.GetTask(ctx, namespaceID, taskID)
	if err != nil {
		return err
	}
	if task.Phase != store.TaskRunning || task.NodeID == nil || task.Attempt <= 0 {
		// 已经终结（pending 直接取消）或没有执行绑定：没有帧可发。
		return nil
	}
	attempt, err := h.db.GetTaskAttempt(ctx, taskID, task.Attempt)
	if err != nil {
		return err
	}
	sess := h.session(*task.NodeID)
	if sess == nil {
		// 节点离线：取消意图已落库，节点回来补报 cancel.ack 即可；不伪造回收。
		return nil
	}
	slot := attempt.Slot
	return sess.Send(ctx, &protocol.TaskCancelFrame{
		Type:     protocol.FrameTaskCancel,
		TaskID:   taskID,
		Attempt:  task.Attempt,
		LeaseID:  attempt.LeaseID,
		Slot:     &slot,
		Reason:   protocol.CancelReason(reason),
		GraceSec: graceSec,
	})
}

// DeployOutcome 是一次人工部署里单节点的结果。
type DeployOutcome struct {
	Deployment *store.Deployment
	RequestID  string
	// Sent 表示通知帧已经写进该节点的在线会话；false = 离线（pending 意图保留，
	// 重连时由 replayDeployments 重放）。
	Sent bool
}

// Deploy 执行一次**显式人工部署**：写 deployments 事实并通知在线节点。
//
// 它不做 install 自动部署、不做自收敛 reconcile、不把整份脚本塞进 WS（节点按
// digest 走受鉴权的 HTTP 内容通道取包体）。nodeIDs 为空时按现有部署策略选候选
// （EligibleNodes：在线 + ns 授权 + 配置 applied + 已有该 digest？——这里用
// 能力身份与 spec 判定的合格节点集合；没有候选就是诚实的空结果，不是错误）。
func (h *Hub) Deploy(ctx context.Context, namespaceID, name string, version int, nodeIDs []string) ([]DeployOutcome, error) {
	if version <= 0 {
		resolved, err := h.db.ResolveRef(ctx, namespaceID, name)
		if err != nil {
			return nil, err
		}
		version = resolved.Version
	}
	capability, err := h.db.GetCapability(ctx, namespaceID, name, version)
	if err != nil {
		return nil, err
	}
	namespace, err := h.db.GetNamespace(ctx, namespaceID)
	if err != nil {
		return nil, err
	}
	artifact, err := h.db.CurrentArtifact(ctx, namespaceID, capability.ID)
	if err != nil {
		return nil, err
	}
	targets := append([]string(nil), nodeIDs...)
	if len(targets) == 0 {
		nodes, _, err := h.db.EligibleNodes(ctx, namespaceID, capability.Spec, capability.Digest)
		if err != nil {
			return nil, err
		}
		for _, node := range nodes {
			targets = append(targets, node.ID)
		}
	}
	if len(targets) == 0 {
		return []DeployOutcome{}, nil
	}
	rows, err := h.db.CreateDeployments(ctx, namespaceID, capability.ID, artifact.Digest, targets)
	if err != nil {
		return nil, err
	}
	out := make([]DeployOutcome, 0, len(rows))
	for _, row := range rows {
		outcome := DeployOutcome{Deployment: row, RequestID: deployRequestID(row.ID)}
		sess := h.session(row.NodeID)
		if sess == nil {
			out = append(out, outcome)
			continue
		}
		frame := &protocol.CapabilityDeployFrame{
			Type:      protocol.FrameCapabilityDeploy,
			RequestID: outcome.RequestID,
			DeployRefs: protocol.DeployRefs{
				Namespace: namespace.Slug,
				Name:      row.Capability,
				Version:   row.Version,
				Digest:    row.Digest,
			},
		}
		if err := sess.Send(ctx, frame); err != nil {
			h.detachSessionOnWriteFailure(sess, err)
			out = append(out, outcome)
			continue
		}
		outcome.Sent = true
		out = append(out, outcome)
	}
	return out, nil
}

// replayDeployments 在节点（重）连后重放**已明确请求**的未完成部署。
//
// 只发 state=pending 的行：这是「部署动作已经发生、节点当时离线」的补推，不是
// 通用 reconciler（applied 不发、failed 不自动重试、也不因为 digest 不一致自己
// 建意图）。
func (h *Hub) replayDeployments(ctx context.Context, sess *Session) error {
	authorized, err := h.db.AuthorizedNamespaces(ctx, sess.nodeID)
	if err != nil {
		return err
	}
	namespaces, err := h.db.ListNamespaces(ctx)
	if err != nil {
		return err
	}
	for _, namespace := range namespaces {
		if !namespaceAllowed(authorized, namespace.Slug) {
			continue
		}
		rows, err := h.db.ListDeployments(ctx, namespace.ID, store.DeploymentFilter{NodeID: sess.nodeID})
		if err != nil {
			h.log.Warn("list deployments failed", "node_id", sess.nodeID, "namespace", namespace.Slug, "err", err)
			continue
		}
		for _, row := range rows {
			if row.State != store.DeploymentPending {
				continue
			}
			frame := &protocol.CapabilityDeployFrame{
				Type:      protocol.FrameCapabilityDeploy,
				RequestID: deployRequestID(row.ID),
				DeployRefs: protocol.DeployRefs{
					Namespace: namespace.Slug,
					Name:      row.Capability,
					Version:   row.Version,
					Digest:    row.Digest,
				},
			}
			if err := sess.Send(ctx, frame); err != nil {
				return err
			}
		}
	}
	return nil
}

// LogsResult 是一次历史日志请求的结果。
type LogsResult struct {
	Frame *protocol.LogsResponseFrame
	// Empty 表示「没有可定位的历史 attempt」（previous 且没有上一次执行）：
	// 这是真空结果，不是「离线/档案过期」的错误成功。
	Empty bool
	// Structured 表示这是一条真实的逐条记录响应（success + lines 非 null）：
	// sequence/stream/text/at 来自节点真实采集，中枢不重编号/不补时间。
	Structured bool
}

// RequestLogs 向执行过目标 attempt 的节点要一次日志回包。
//
// attempt>0 = 指定历史 attempt；attempt==0 = 任务当前 attempt；previous=true 时忽略
// attempt 并取「上一次执行」（没有上一次 → Empty=true 的真空结果）。节点按
// task_attempts 的历史 node 定位，不拿当前节点冒充另一次执行。
//
// tailLines/sinceSeconds 是**节点侧**的真实查询参数（共享 LogsRequestFrame 字段）：
// 中枢只转发，不拼 raw、不重编号、不按 server 时钟二次过滤。
func (h *Hub) RequestLogs(ctx context.Context, namespaceID, taskID string, attempt int, previous bool, tailLines, sinceSeconds int, timeout time.Duration) (*LogsResult, error) {
	task, err := h.db.GetTask(ctx, namespaceID, taskID)
	if err != nil {
		return nil, err
	}
	if previous {
		prev, perr := h.db.PreviousTaskAttempt(ctx, taskID)
		if errors.Is(perr, store.ErrNotFound) {
			return &LogsResult{
				Frame: &protocol.LogsResponseFrame{Type: protocol.FrameLogsResponse, TaskID: taskID},
				Empty: true,
			}, nil
		}
		if perr != nil {
			return nil, perr
		}
		attempt = prev.Attempt
	}
	if attempt <= 0 {
		attempt = task.Attempt
	}
	if attempt <= 0 {
		return nil, ErrNoAttempt
	}
	historical, err := h.db.GetTaskAttempt(ctx, taskID, attempt)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNoAttempt
		}
		return nil, err
	}
	sess := h.session(historical.NodeID)
	if sess == nil {
		return nil, ErrNodeOffline
	}
	requestID := newRequestID("lreq")
	waiter := &logRequest{
		taskID: taskID, attempt: attempt, nodeID: historical.NodeID,
		ch: make(chan logResponse, 1),
	}
	h.mu.Lock()
	h.logs[requestID] = waiter
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if h.logs[requestID] == waiter {
			delete(h.logs, requestID)
		}
		h.mu.Unlock()
	}()
	if err := sess.Send(ctx, &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: taskID, RequestID: requestID, Attempt: attempt,
		TailLines: tailLines, SinceSeconds: sinceSeconds,
	}); err != nil {
		h.detachSessionOnWriteFailure(sess, err)
		return nil, err
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, ErrLogsTimeout
	case res := <-waiter.ch:
		if res.err != nil {
			return nil, res.err
		}
		if res.frame == nil {
			return nil, ErrLogsCorrelation
		}
		return &LogsResult{
			Frame:      res.frame,
			Structured: res.frame.Error == "" && res.frame.Lines != nil,
		}, nil
	}
}

// deliverLogs 把一条 logs_response 交给对应 waiter。
//
// 有界交付（R1-C5）：命中 waiter 时**先把它从登记表移除**再非阻塞投递（cap 1），
// 所以同一请求最多被交付一次；重复/晚到的回包只能走「没有登记」分支，绝不能在
// 读循环上阻塞一个 send。配对失败（requestId/taskId/attempt）交付一个明确错误，
// 不把 A 的日志安到 B 头上。
func (h *Hub) deliverLogs(sess *Session, frame *protocol.LogsResponseFrame) {
	h.mu.Lock()
	waiter := h.logs[frame.RequestID]
	if waiter != nil {
		delete(h.logs, frame.RequestID)
	}
	h.mu.Unlock()
	if waiter == nil {
		h.log.Warn("logs response without request", "request_id", frame.RequestID, "task_id", frame.TaskID)
		return
	}
	if waiter.nodeID != sess.nodeID || waiter.taskID != frame.TaskID {
		h.log.Warn("logs response correlation mismatch",
			"request_id", frame.RequestID, "want_task", waiter.taskID, "got_task", frame.TaskID)
		h.deliverLogsOnce(waiter, logResponse{err: ErrLogsCorrelation})
		return
	}
	if err := validateLogsResponse(frame, waiter.attempt); err != nil {
		h.log.Warn("logs response invalid", "request_id", frame.RequestID,
			"want_attempt", waiter.attempt, "got_attempt", frame.Attempt, "err", err)
		h.deliverLogsOnce(waiter, logResponse{err: ErrLogsCorrelation})
		return
	}
	h.deliverLogsOnce(waiter, logResponse{frame: frame})
}

// deliverLogsOnce 向已从登记表移除的 waiter 做一次非阻塞交付。
//
// ch 容量 1 且 waiter 只可能被移除一次：这里永不阻塞；即使极端情况下通道已满，
// 也只是丢弃一条已经无主的回包，不会卡住读循环。
func (h *Hub) deliverLogsOnce(waiter *logRequest, res logResponse) {
	select {
	case waiter.ch <- res:
	default:
	}
}

// wakeLogsForNode 在会话断开时唤醒该节点所有在飞的日志请求（真实错误，不是等超时）。
func (h *Hub) wakeLogsForNode(nodeID string, err error) {
	h.mu.Lock()
	waiters := make([]*logRequest, 0)
	for requestID, waiter := range h.logs {
		if waiter.nodeID != nodeID {
			continue
		}
		delete(h.logs, requestID)
		waiters = append(waiters, waiter)
	}
	h.mu.Unlock()
	for _, waiter := range waiters {
		h.deliverLogsOnce(waiter, logResponse{err: err})
	}
}

// validateLogsResponse 严格校验一条 logs_response 的真实性与关联（R1-C5）。
//
//   - 成功：Attempt 必须是**实际**请求的 attempt；lines 必须是非 null 数组；每条
//     sequence ≥1 且严格递增、stream ∈ stdout/stderr、at 是 RFC3339Nano。
//   - 错误：错误码 ∈ 封闭集合；不得携带成功 lines；不得用一个错误声称别的
//     attempt（Attempt 为空或等于请求的 attempt，不伪造 latest）。
func validateLogsResponse(frame *protocol.LogsResponseFrame, wantAttempt int) error {
	if frame == nil {
		return ErrLogsCorrelation
	}
	if frame.Error != "" {
		if frame.Lines != nil {
			return ErrLogsCorrelation
		}
		if frame.Attempt != 0 && frame.Attempt != wantAttempt {
			return ErrLogsCorrelation
		}
		switch frame.Error {
		case protocol.LogsErrNoLocalArchive, protocol.LogsErrUnreadable, protocol.LogsErrIllegalRequest:
			return nil
		default:
			return ErrLogsCorrelation
		}
	}
	if frame.Attempt != wantAttempt {
		return ErrLogsCorrelation
	}
	if frame.Lines == nil {
		return ErrLogsCorrelation
	}
	last := 0
	for _, line := range *frame.Lines {
		if line.Sequence < 1 || line.Sequence <= last {
			return ErrLogsCorrelation
		}
		if line.Stream != protocol.LogStreamStdout && line.Stream != protocol.LogStreamStderr {
			return ErrLogsCorrelation
		}
		if _, terr := time.Parse(time.RFC3339Nano, line.At); terr != nil {
			return ErrLogsCorrelation
		}
		last = line.Sequence
	}
	return nil
}

// detachSessionOnWriteFailure 在写失败时立刻下线该会话（读循环随后也会退出）。
func (h *Hub) detachSessionOnWriteFailure(sess *Session, err error) {
	if errors.Is(err, ErrSessionClosed) {
		return
	}
	sess.close(websocket.StatusInternalError, "write failed")
}

// namespaceVarsFor 取派发帧要携带的 ns 默认值：**只含该能力 requires.values 点名的名字**。
//
// 任务行上钉住的是接受时的完整 ns 快照（D2）；帧里只给被引用的键，避免把整份
// ns 默认值广播给节点。
func (h *Hub) namespaceVarsFor(ctx context.Context, task *store.Task) map[string]string {
	out := map[string]string{}
	capability, err := h.db.GetCapabilityByID(ctx, task.NamespaceID, task.CapabilityID)
	if err != nil {
		return out
	}
	for _, name := range capability.Spec.Requires.Values {
		if !strings.HasPrefix(name, "vars.") {
			continue
		}
		key := strings.TrimPrefix(name, "vars.")
		if value, ok := task.NamespaceVars[key]; ok {
			out[key] = value
		}
	}
	return out
}

// namespaceSlug 把 ns id 翻成 wire 上的 ns 名（失败时退化为 id，仅供诊断）。
func (h *Hub) namespaceSlug(ctx context.Context, namespaceID string) string {
	namespace, err := h.db.GetNamespace(ctx, namespaceID)
	if err != nil {
		return namespaceID
	}
	return namespace.Slug
}

// limitsToMap 把协议 Limits 转成 config 的 map 形状（空 perCapability 不写）。
func limitsToMap(limits protocol.Limits) map[string]any {
	out := map[string]any{}
	if limits.Total != 0 {
		out["total"] = limits.Total
	}
	if len(limits.PerCapability) > 0 {
		perCapability := make(map[string]any, len(limits.PerCapability))
		for name, value := range limits.PerCapability {
			perCapability[name] = value
		}
		out["perCapability"] = perCapability
	}
	return out
}

// nodeIDs 提取节点 id 列表。
func nodeIDs(nodes []*store.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, node.ID)
	}
	return out
}

// namespaceAllowed 判断授权列表（可含 "*"）是否覆盖某 ns 名。
func namespaceAllowed(authorized []string, slug string) bool {
	for _, ns := range authorized {
		if ns == store.NamespaceWildcard || ns == slug {
			return true
		}
	}
	return false
}

// deployRequestID 是部署请求的稳定 id：同一次重放保持同一个值（部署行 id 派生）。
func deployRequestID(deploymentID string) string { return "dep_" + deploymentID }

// newRequestID 生成 `<prefix>_<base64url(12B)>` 形式的关联 id。
func newRequestID(prefix string) string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return prefix + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(buf)
}

// timeUnix 把秒级时刻转成 UTC 的 time.Time（wire 上的时间字段一律 UTC，不带本地时区）。
func timeUnix(sec int64) time.Time { return time.Unix(sec, 0).UTC() }
