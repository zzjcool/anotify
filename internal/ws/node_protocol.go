package ws

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion 是本实现支持的节点协议版本（hello / welcome 帧里交换）。
//
// v2（console-declares）：hello 不再自报能力（capabilities/manifest 删除），
// 改为上报「本机缓存了哪些产物」；新增 deploy（下行）/ apply（上行）两帧。
const ProtocolVersion = 2

// 十一帧的帧类型名。字符串本身是协议的一部分，不能随意改。
//
// 节点 → 中枢：hello / heartbeat / ack / progress / result / apply
// 中枢 → 节点：welcome / assign / deploy / cancel / error
const (
	// 节点 → 中枢
	NodeFrameHello     = "hello"
	NodeFrameHeartbeat = "heartbeat"
	NodeFrameAck       = "ack"
	NodeFrameProgress  = "progress"
	NodeFrameResult    = "result"
	NodeFrameApply     = "apply"

	// 中枢 → 节点
	NodeFrameWelcome = "welcome"
	NodeFrameAssign  = "assign"
	NodeFrameDeploy  = "deploy"
	NodeFrameCancel  = "cancel"
	NodeFrameError   = "error"
)

// 错误码（契约 §6.1 表格）。
const (
	ErrCodeUnknownCapability = "unknown_capability"
	ErrCodeBadFrame          = "bad_frame"
	ErrCodeUnknownType       = "unknown_type"
	ErrCodeInvalidTransition = "invalid_transition"
	ErrCodeLeaseExpired      = "lease_expired"
	// ErrCodeProtocolMismatch 节点 speak 的协议版本与服务端不同。
	//
	// 与 bad_frame 分开的理由：这是**版本不兼容**，不是「帧写坏了」。节点/运维
	// 看到这个码应该去升级二进制，而不是去查 JSON 结构（契约 §1）。
	ErrCodeProtocolMismatch = "protocol_mismatch"
	// ErrCodeNodeDraining 节点在排空，不再接新任务。
	ErrCodeNodeDraining = "node_draining"
	// ErrCodeTooManyInstances 节点申报的实例数超过中枢允许的上限（契约 Q5）。
	ErrCodeTooManyInstances = "too_many_instances"
	// ErrCodeInvalidManifest 已随 hello.manifest 一起下线（v2 不再自报执行方式）。
	//
	// 部署产物不合法走 apply 帧的 reason 短码（见 ApplyFrame），不占用错误码空间：
	// 那是**部署**失败，不是**协议**错误。
	//
	// ErrCodeUnauthorized 握手鉴权失败（HTTP 层用，不会作为帧下发）。
	ErrCodeUnauthorized = "unauthorized"
	// ErrCodeFrameTooLarge 上行帧超过服务端软上限（#221）。
	//
	// 下发它是为了**可诊断**：没有它时 coder/websocket 会在硬上限处静默
	// 用 1009 关连接，节点侧只看到「连接断了」，不知道是自己的帧太大。
	// 节点收到这个码可以把大 result/artifacts 截断重发。
	ErrCodeFrameTooLarge = "frame_too_large"
	// ErrCodeParamsTooLarge 任务 params 超过 store.MaxTaskParamsBytes（#221 / 审查 m5）。
	//
	// 典型场景：一条**下一跳子任务**的 params 超限。store 侧的闸门在
	// createTasksTx 里，所以子任务被拒会让**父任务的终态整体回滚**（同一个事务）——
	// 父任务留在 running → 租约过期重派 → 节点把同一份昂贵工作再跑一遍 → 返回同样的
	// 超限子任务，直到 attempts 用尽变 abandoned。
	// 把这种错误映射成 invalid_transition 就把「你的 params 太大」这个病因埋了；
	// 给它一个独立的码，节点/运维一眼能看出是参数体积问题（应该是去分片或改用引用）。
	ErrCodeParamsTooLarge = "params_too_large"
)

// 上行帧（节点 → 中枢）──────────────────────────────

// HelloFrame 是节点连上后的第一帧：自报身份、协议版本、并发槽位、本机缓存了什么。
//
// # v2 的语义变化（console-declares，契约 D4）
//
// 旧版用 capabilities[] + manifest[] **自报能力**，中枢只能被动接受；
// v2 把它们删掉，改为 applied[]：节点陈述「我缓存了哪些产物」，
// 而「这个能力归谁」完全由中枢的部署决定。
//
// 差别很重要：能力归属从此只有一个写者（控制台），节点不能再单方面声称
// 「我会跑 X」——否则中枢数据库会静默地撒谎（页面显示能跑，实际跑到就 failed）。
//
// applied 里的能力必须已在中枢定义（否则 unknown_capability）。这条保留：
// 产物本身就是从中枢部署过去的，不可能出现中枢不认识的能力。
type HelloFrame struct {
	Type      string         `json:"type"`
	NodeID    string         `json:"node_id"`
	Protocol  int            `json:"protocol"`
	Instances []InstanceDecl `json:"instances"`
	Applied   []AppliedDecl  `json:"applied"`
	Labels    map[string]any `json:"labels,omitempty"`
}

// AppliedDecl 是节点对「我缓存了什么」的陈述。
//
// 它**不是**能力声明：中枢只会用它核对已部署的东西（对账 + 发现本地变体），
// 不会因为出现一条没部署过的 applied 就把能力归属给这台机器。
type AppliedDecl struct {
	Capability string `json:"capability"`
	Version    int    `json:"version"`
	Digest     string `json:"digest"`
}

// DeployFrame 下发或撤销一份产物。
//
// Artifact 是 CONTRACT-artifact §2 定义的 canonical JSON。节点负责校验、
// 探测 requires、原子落盘，然后回一帧 ApplyFrame。
type DeployFrame struct {
	Type     string          `json:"type"`
	DeployID string          `json:"deploy_id"`
	Action   string          `json:"action"` // apply | remove
	Artifact json.RawMessage `json:"artifact,omitempty"`
}

// 部署动作。
const (
	DeployActionApply  = "apply"
	DeployActionRemove = "remove"
)

// 部署结果状态（ApplyFrame.State）。
const (
	ApplyStateApplied = "applied"
	ApplyStateFailed  = "failed"
)

// 部署失败短码（ApplyFrame.Reason 的前缀，`;` 分隔多个）。
const (
	ApplyReasonInvalidArtifact    = "invalid_artifact"
	ApplyReasonMissingRequirement = "missing_requirement"
	ApplyReasonWriteFailed        = "write_failed"
	ApplyReasonUnsupportedAction  = "unsupported_action"
)

// ApplyFrame 是节点对一次部署的回报。
type ApplyFrame struct {
	Type       string `json:"type"`
	DeployID   string `json:"deploy_id"`
	Capability string `json:"capability,omitempty"`
	Version    int    `json:"version,omitempty"`
	Digest     string `json:"digest,omitempty"`
	State      string `json:"state"` // applied | failed
	Reason     string `json:"reason,omitempty"`
}

// InstanceDecl 是节点申报的并发槽位。
type InstanceDecl struct {
	Slot        int `json:"slot"`
	Concurrency int `json:"concurrency"`
}

// HeartbeatFrame 是心跳：任何上行帧都续命，心跳是最常见的那种。
type HeartbeatFrame struct {
	Type string `json:"type"`
	TS   int64  `json:"ts"`
}

// AckFrame 表示节点接单，租约开始计时。
type AckFrame struct {
	Type   string `json:"type"`
	TaskID string `json:"task_id"`
}

// ProgressFrame 推进任务到 running，可带一句进度说明。
type ProgressFrame struct {
	Type   string `json:"type"`
	TaskID string `json:"task_id"`
	Note   string `json:"note,omitempty"`
}

// ResultFrame 是任务结果。
//
// Out 必须是 **nil（缺字段）或 NULL（明确到头）或字符串**三态，
// 这是不变量 I5 的协议层表达：
//
//	字段缺失   → Reported=false（节点没回报出口）
//	"out":null → Reported=true, Out=nil（明确到头）
//	"out":"fix"→ Reported=true, Out=&"fix"`
//
// Go 的 encoding/json 无法直接区分「字段缺失」与「显式 null」——
// 两者都让 *string 保持 nil。所以本结构体带一个**自定义 UnmarshalJSON**
// （见下方），用 json.RawMessage 先看字段在不在。
type ResultFrame struct {
	Type      string   `json:"type"`
	TaskID    string   `json:"task_id"`
	Status    string   `json:"status"` // succeeded | failed
	Out       *string  `json:"out"`
	Summary   string   `json:"summary"`
	Artifacts []string `json:"artifacts"`
	Error     string   `json:"error"`

	// OutPresent 记录 JSON 里是否出现了 "out" 键（无论值是不是 null）。
	// 这是「缺失 vs null」的唯一可靠判据，不导出到 JSON。
	OutPresent bool `json:"-"`
}

// UnmarshalJSON 实现「字段缺失」与「显式 null」的区分（不变量 I5 的关键）。
func (f *ResultFrame) UnmarshalJSON(data []byte) error {
	// 用一个同构的临时结构体接收常规字段，避免递归调用自己
	type alias struct {
		Type      string   `json:"type"`
		TaskID    string   `json:"task_id"`
		Status    string   `json:"status"`
		Out       *string  `json:"out"`
		Summary   string   `json:"summary"`
		Artifacts []string `json:"artifacts"`
		Error     string   `json:"error"`
	}
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return fmt.Errorf("decode result frame: %w", err)
	}
	f.Type = a.Type
	f.TaskID = a.TaskID
	f.Status = a.Status
	f.Out = a.Out
	f.Summary = a.Summary
	f.Artifacts = a.Artifacts
	f.Error = a.Error

	// 再看 "out" 键在不在
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("decode result frame keys: %w", err)
	}
	_, f.OutPresent = probe["out"]
	return nil
}

// MarshalJSON 让 OutPresent 不影响出站 JSON（节点侧不该看到这个内部字段）。
func (f ResultFrame) MarshalJSON() ([]byte, error) {
	type alias struct {
		Type      string   `json:"type"`
		TaskID    string   `json:"task_id"`
		Status    string   `json:"status"`
		Out       *string  `json:"out"`
		Summary   string   `json:"summary"`
		Artifacts []string `json:"artifacts"`
		Error     string   `json:"error"`
	}
	return json.Marshal(alias{
		Type: f.Type, TaskID: f.TaskID, Status: f.Status, Out: f.Out,
		Summary: f.Summary, Artifacts: f.Artifacts, Error: f.Error,
	})
}

// Reported 判定这次结果是否「回报了出口」（不变量 I5）。
//
// 只要 out 键出现过（哪怕是 null），就是已回报。
func (f *ResultFrame) Reported() bool { return f.OutPresent }

// 下行帧（中枢 → 节点）──────────────────────────────

// WelcomeFrame 是握手成功后的第一帧：告诉节点按什么节奏发心跳。
type WelcomeFrame struct {
	Type         string `json:"type"`
	NodeID       string `json:"node_id"`
	Protocol     int    `json:"protocol"`
	HeartbeatSec int    `json:"heartbeat_sec"`
	LeaseTTLSec  int    `json:"lease_ttl_sec"`
}

// AssignFrame 是中枢能表达的全部：{capability, version, params} + 租约。
//
// 仍然**不含任何「要执行什么」的内容**：节点自己按 capability@version（或 digest）
// 找到缓存里的产物。Digest 与 TraceID 是 v2 新增的旁路信息：
//
//   - Digest 让本次执行钉在一个确定的产物上（审计 + ANOTIFY_DIGEST）
//   - TraceID 让脚本能把日志串回链路（旧协议一直没有，issue #184）
type AssignFrame struct {
	Type        string         `json:"type"`
	TaskID      string         `json:"task_id"`
	Capability  string         `json:"capability"`
	Version     int            `json:"version"`
	Params      map[string]any `json:"params"`
	LeaseUntil  int64          `json:"lease_until"`
	MaxDuration int            `json:"max_duration_sec"`
	Digest      string         `json:"digest,omitempty"`
	TraceID     string         `json:"trace_id,omitempty"`
}

// CancelFrame 要求节点停止某任务。
type CancelFrame struct {
	Type   string `json:"type"`
	TaskID string `json:"task_id"`
	Reason string `json:"reason,omitempty"`
}

// ErrorFrame 是中枢下发的错误（不关连接，除非是协议级错误）。
type ErrorFrame struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	TaskID  string `json:"task_id,omitempty"`
}

// ---------- 编解码 ----------

// envelope 只取帧类型，用于先分流再精确解码。
type envelope struct {
	Type string `json:"type"`
}

// UnknownFrame 表示解析成功但类型未知（调用方应回 unknown_type 错误帧）。
type UnknownFrame struct{ Type string }

// DecodeUpstream 把收到的 JSON 解码成对应类型的上行帧。
//
// 返回的具体类型是 *HelloFrame / *HeartbeatFrame / *AckFrame / *ProgressFrame / *ResultFrame；
// 类型未知时返回 (*UnknownFrame, nil)——这不是错误：协议要求回一个 error 帧
// （code=unknown_type）而不是断开连接。
// JSON 本身坏了返回 error（对应 code=bad_frame）。
func DecodeUpstream(data []byte) (any, error) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("bad frame json: %w", err)
	}
	switch env.Type {
	case NodeFrameHello:
		var f HelloFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode hello: %w", err)
		}
		return &f, nil
	case NodeFrameHeartbeat:
		var f HeartbeatFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode heartbeat: %w", err)
		}
		return &f, nil
	case NodeFrameAck:
		var f AckFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode ack: %w", err)
		}
		return &f, nil
	case NodeFrameProgress:
		var f ProgressFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode progress: %w", err)
		}
		return &f, nil
	case NodeFrameResult:
		var f ResultFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode result: %w", err)
		}
		return &f, nil
	case NodeFrameApply:
		var f ApplyFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode apply: %w", err)
		}
		return &f, nil
	default:
		return &UnknownFrame{Type: env.Type}, nil
	}
}

// Encode 把下行帧编码成 JSON（统一入口，便于加统一字段）。
func Encode(f any) ([]byte, error) {
	data, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("encode frame: %w", err)
	}
	return data, nil
}

// IsDownstream 判定一个帧类型是否是中枢 → 节点方向（用于断言节点没有发下行帧）。
func IsDownstream(frameType string) bool {
	switch frameType {
	case NodeFrameWelcome, NodeFrameAssign, NodeFrameDeploy, NodeFrameCancel, NodeFrameError:
		return true
	}
	return false
}

// IsUpstream 判定一个帧类型是否是节点 → 中枢方向。
func IsUpstream(frameType string) bool {
	switch frameType {
	case NodeFrameHello, NodeFrameHeartbeat, NodeFrameAck, NodeFrameProgress, NodeFrameResult, NodeFrameApply:
		return true
	}
	return false
}
