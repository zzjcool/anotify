package ws

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/zzjcool/anotify/internal/store"
)

// NodeStore 是节点协议需要的最小 store 表面（便于测试注入）。
//
// *store.DB 满足它。
type NodeStore interface {
	GetNodeByTokenHash(ctx context.Context, tokenHash string) (*store.Node, error)
	UpdateNodeStatus(ctx context.Context, orgID, nodeID, status string) error
	TouchNode(ctx context.Context, orgID, nodeID string, at int64) error
	ConfirmCapabilities(ctx context.Context, orgID, nodeID string, caps []store.CapabilityRef) ([]string, error)
	// ConfirmCapabilityDecls 是带 run 声明的注册路径（hello 带 manifest）：
	// 逐条校验 run，并把 run_spec 与声明一起全量替换（契约 §3.3）。
	ConfirmCapabilityDecls(ctx context.Context, orgID, nodeID string, decls []store.CapabilityDecl) ([]string, error)
	UpsertInstances(ctx context.Context, orgID, nodeID string, specs []store.InstanceSpec) ([]*store.NodeInstance, error)
	MarkInstancesDead(ctx context.Context, nodeID string) error
	ListInstances(ctx context.Context, nodeID string) ([]*store.NodeInstance, error)
	GetTask(ctx context.Context, orgID, id string) (*store.Task, error)
	Transition(ctx context.Context, orgID, taskID, from, to string) error
	// ProgressTask 一个事务内完成 progress 帧的三件事：→running + started_at + 续租。
	ProgressTask(ctx context.Context, orgID, taskID string, leaseUntil int64) error
	// ReportResultAndCreateTasks 在**一个事务**里完成「父任务终态写入 + 下一跳子任务建行」
	// （#140）。next 为空 == 旧的 ReportResult 行为。
	//
	// 为什么必须同一个事务：拆成两步（先提交终态、后台 goroutine 再建行）时，
	// 进程在中间退出就会留下「父任务 succeeded + out=fix、子任务行不存在」的永久空洞——
	// ListRedispatchable 只捞已存在的 pending 行，捞不到一条不存在的行，链永久停。
	// 子任务插入失败 → 整体回滚（父任务不留在终态）。
	//
	// 补偿路径：本事务提交后，next 里的行已是 pending + node_id IS NULL + attempts=0，
	// 即使调用方的 publish 丢失/失败/随进程消失，store.ListRedispatchable
	// （internal/lease sweeper，每 5s tick，Redispatch = Dispatcher.RedispatchTask）
	// 会把它捞回来重派。所以 publish 不需要也不该进事务。
	ReportResultAndCreateTasks(ctx context.Context, orgID, taskID, status string, out *string,
		reported bool, summary string, artifacts []string, errMsg string, next []*store.Task) error
	ExtendLease(ctx context.Context, orgID, taskID string, leaseUntil int64) error
	ExtendLeasesForNode(ctx context.Context, orgID, nodeID string, leaseUntil int64) (int, error)
}

// 握手鉴权失败的错误类型（供 HTTP 层区分 401 / 403）。
var (
	// ErrMissingToken 请求没带 Authorization: Bearer。
	ErrMissingToken = errors.New("missing node token")
	// ErrBadToken token 无效（查不到节点）。
	ErrBadToken = errors.New("invalid node token")
	// ErrNodeDraining 节点在排空，拒绝接入新连接。
	ErrNodeDraining = errors.New("node is draining")
)

// AuthenticateNode 在 **Upgrade 之前**完成握手鉴权（契约 §6.1 / §10.3）。
//
// token 是 **opaque**（不是 JWT）：查 nodes.token_hash（sha256）得到 (node_id, org_id)。
// 拿到这两个值之后，该连接上所有帧都用它们，**不再查库**。
//
// 返回的错误：ErrMissingToken → 401；ErrBadToken / ErrNodeDraining → 403。
// 关键：鉴权失败时**绝不能**返回 101（升级成功后才发现没权限，
// 会让客户端以为连上了）。
func AuthenticateNode(ctx context.Context, st NodeStore, r *http.Request) (*store.Node, error) {
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
	if node.Status == store.NodeStatusDraining {
		return nil, ErrNodeDraining
	}
	return node, nil
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
	case errors.Is(err, ErrBadToken), errors.Is(err, ErrNodeDraining):
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

// applyHello 校验并落库 hello 帧带来的声明（契约 Q5 / 不变量 I2）。
//
// 两件事，任一失败都拒绝整帧：
//  1. 能力声明：name@version 必须在中枢已定义，否则 unknown_capability；
//     带 manifest 时 run 还要合法，否则 invalid_manifest（错误一次列全，全拒不留半份）
//  2. 实例申报：数量受 nodes.max_instances 硬顶约束，否则 too_many_instances
//
// 顺序有讲究：先校能力再落实例——一个声明了不存在能力的节点不该先占住槽位。
func applyHello(ctx context.Context, st NodeStore, node *store.Node, f *HelloFrame) error {
	// 协议版本
	if f.Protocol != 0 && f.Protocol != ProtocolVersion {
		return &helloError{
			code:    ErrCodeBadFrame,
			message: fmt.Sprintf("unsupported protocol %d (server speaks %d)", f.Protocol, ProtocolVersion),
		}
	}
	// 协议要求节点自报的身份与 token 对应的节点一致（防「拿 A 的 token 冒充 B」）
	if f.NodeID != "" && f.NodeID != node.ID {
		return &helloError{
			code:    ErrCodeBadFrame,
			message: fmt.Sprintf("hello node_id %q does not match the token's node %q", f.NodeID, node.ID),
		}
	}

	// ① 能力声明
	if err := confirmHelloCapabilities(ctx, st, node, f); err != nil {
		return err
	}

	// ② 实例申报
	specs := make([]store.InstanceSpec, 0, len(f.Instances))
	for _, in := range f.Instances {
		specs = append(specs, store.InstanceSpec{Slot: in.Slot, Concurrency: in.Concurrency})
	}
	if _, err := st.UpsertInstances(ctx, node.OrgID, node.ID, specs); err != nil {
		if errors.Is(err, store.ErrTooManyInstances) {
			return &helloError{code: ErrCodeTooManyInstances, message: err.Error()}
		}
		return err
	}

	// ③ 上线
	if err := st.UpdateNodeStatus(ctx, node.OrgID, node.ID, store.NodeStatusOnline); err != nil {
		return err
	}
	return nil
}

// confirmHelloCapabilities 落库 hello 里的能力声明（契约 §2 / §5）。
//
// 两条路径，**老路径零漂移**：
//   - 不带 manifest（老 agent）：原样走 ConfirmCapabilities，run_spec 一律 '{}'，
//     错误码映射也保持原样（只认 unknown_capability）；
//   - 带 manifest：先按契约 §2.1 合并成一份声明（run 原文跟着走），再走
//     ConfirmCapabilityDecls（store 侧一次校验全部条目、全拒不留半份）。
//
// 错误码映射**顺序敏感**（契约 §5）：混有未知能力时优先报 unknown_capability
// （根因），否则才是 invalid_manifest；其它错误（DB 故障等）原样返回 —— 那不是帧的错。
func confirmHelloCapabilities(ctx context.Context, st NodeStore, node *store.Node, f *HelloFrame) error {
	// 老路径：没有 manifest = 只报 name@version，行为与错误码一律不变。
	if len(f.Manifest) == 0 {
		refs := make([]store.CapabilityRef, 0, len(f.Capabilities))
		for _, c := range f.Capabilities {
			refs = append(refs, store.CapabilityRef{Name: c.Capability, Version: c.Version})
		}
		if _, err := st.ConfirmCapabilities(ctx, node.OrgID, node.ID, refs); err != nil {
			if errors.Is(err, store.ErrUnknownCapability) {
				return &helloError{code: ErrCodeUnknownCapability, message: err.Error()}
			}
			return err
		}
		return nil
	}

	// 新路径：capabilities + manifest 合并成一份带 run 的声明。
	decls := mergeDeclarations(f.Capabilities, f.Manifest)
	if _, err := st.ConfirmCapabilityDecls(ctx, node.OrgID, node.ID, decls); err != nil {
		// 顺序敏感：未知能力（可能混有 run 问题）优先 → unknown_capability。
		if errors.Is(err, store.ErrUnknownCapability) {
			return &helloError{code: ErrCodeUnknownCapability, message: err.Error()}
		}
		// 纯 run 问题 → invalid_manifest。message 用 err.Error()：
		// 它已经把**全部**问题项列全（; 分隔），节点侧排障一次到位。
		var declErr *store.CapabilityDeclError
		if errors.As(err, &declErr) {
			return &helloError{code: ErrCodeInvalidManifest, message: err.Error()}
		}
		return err
	}
	return nil
}

// mergeDeclarations 把 hello 的两份声明合成一份（契约 §2.1，纯函数、不碰 IO）。
//
//	caps:     hello.capabilities —— 只有 name@version，老 agent 的唯一声明来源
//	manifest: hello.manifest     —— 带 run 原文，信息更全
//
// 规则：
//  1. manifest 建索引，key = name@version（重复时后者胜，行为确定）；
//  2. 按 caps 顺序输出：命中索引的用 manifest 的 run 且 RunRequired=true；
//     未命中的是「声明了能力但没声明执行方式」（Run=nil → run_spec 落 '{}'）；
//  3. 按 manifest 顺序补上 caps 里没有的项（Run 原文，RunRequired=true）。
//
// 即 capabilities 的顺序优先，manifest 独有的项追加在后；同一 name@version 只出一条，
// 且一定用 manifest 的 run —— 两份声明冲突时以 manifest 为准（信息更全）。
func mergeDeclarations(caps []CapabilityDecl, manifest []ManifestDecl) []store.CapabilityDecl {
	index := make(map[string]ManifestDecl, len(manifest))
	order := make([]string, 0, len(manifest))
	for _, m := range manifest {
		key := declarationKey(m.Capability, m.Version)
		if _, seen := index[key]; !seen {
			order = append(order, key)
		}
		index[key] = m // 重复时后者胜
	}

	out := make([]store.CapabilityDecl, 0, len(caps)+len(order))
	emitted := make(map[string]struct{}, len(caps)+len(order))
	for _, c := range caps {
		key := declarationKey(c.Capability, c.Version)
		if _, dup := emitted[key]; dup {
			continue // 同一 name@version 只出一条
		}
		emitted[key] = struct{}{}
		decl := store.CapabilityDecl{Name: c.Capability, Version: c.Version}
		if m, ok := index[key]; ok {
			decl.Run = m.Run
			decl.RunRequired = true
		}
		out = append(out, decl)
	}
	for _, key := range order {
		if _, done := emitted[key]; done {
			continue
		}
		emitted[key] = struct{}{}
		m := index[key]
		out = append(out, store.CapabilityDecl{
			Name:        m.Capability,
			Version:     m.Version,
			Run:         m.Run,
			RunRequired: true,
		})
	}
	return out
}

// declarationKey 是 manifest 索引与去重的 key：name@version。
func declarationKey(name string, version int) string {
	return fmt.Sprintf("%s@%d", name, version)
}

// welcomeFrameFor 构造握手成功的 welcome 帧。
func welcomeFrameFor(node *store.Node) *WelcomeFrame {
	return &WelcomeFrame{
		Type:         NodeFrameWelcome,
		NodeID:       node.ID,
		Protocol:     ProtocolVersion,
		HeartbeatSec: store.HeartbeatSec,
		LeaseTTLSec:  store.LeaseTTLSec,
	}
}
