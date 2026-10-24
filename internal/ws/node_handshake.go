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
	UpsertInstances(ctx context.Context, orgID, nodeID string, specs []store.InstanceSpec) ([]*store.NodeInstance, error)
	MarkInstancesDead(ctx context.Context, nodeID string) error
	ListInstances(ctx context.Context, nodeID string) ([]*store.NodeInstance, error)
	GetTask(ctx context.Context, orgID, id string) (*store.Task, error)
	Transition(ctx context.Context, orgID, taskID, from, to string) error
	StartTask(ctx context.Context, orgID, taskID string) error
	ReportResult(ctx context.Context, orgID, taskID, status string, out *string, reported bool, summary string, artifacts []string, errMsg string) error
	ExtendLease(ctx context.Context, orgID, taskID string, leaseUntil int64) error
	ExtendLeasesForNode(ctx context.Context, orgID, nodeID string, leaseUntil int64) (int, error)
	ReleaseInstanceByTask(ctx context.Context, nodeID, taskID string) error
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
//  1. 能力声明：name@version 必须在中枢已定义，否则 unknown_capability
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
