package ws

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion 是本实现支持的节点协议版本（hello / welcome 帧里交换）。
const ProtocolVersion = 1

// 九帧的帧类型名（契约 §6.1）。字符串本身是协议的一部分，不能随意改。
//
// 常量带 Node 前缀，与旧浏览器协议的 Frame*（hello/ack/error 同名）区分开——
// 旧协议待 #7 下线后删除，届时可考虑去掉前缀。
//
// 节点 → 中枢：hello / heartbeat / ack / progress / result
// 中枢 → 节点：welcome / assign / cancel / error
const (
	// 节点 → 中枢
	NodeFrameHello     = "hello"
	NodeFrameHeartbeat = "heartbeat"
	NodeFrameAck       = "ack"
	NodeFrameProgress  = "progress"
	NodeFrameResult    = "result"

	// 中枢 → 节点
	NodeFrameWelcome = "welcome"
	NodeFrameAssign  = "assign"
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
	// ErrCodeNodeDraining 节点在排空，不再接新任务。
	ErrCodeNodeDraining = "node_draining"
	// ErrCodeTooManyInstances 节点申报的实例数超过中枢允许的上限（契约 Q5）。
	ErrCodeTooManyInstances = "too_many_instances"
	// ErrCodeInvalidManifest 节点上报的 manifest 不合法（run 缺失 / runner 不支持 /
	// command 为空 / concurrency 越界）。与 too_many_instances 同构：每种握手拒绝原因一个码，
	// 节点侧不必解析人类可读的 message。
	ErrCodeInvalidManifest = "invalid_manifest"
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

// HelloFrame 是节点连上后的第一帧：自报身份、协议版本、能力与实例。
//
// Capabilities 只有 name + version —— 没有命令、没有出口（不变量 I1/I2）。
// Instances 由节点申报（契约 §10 Q5），中枢 upsert 到 node_instances。
//
// # Manifest 与 Capabilities 的优先级（issue #168，语义必须写死）
//
//   - 缺省 Manifest（老 agent）= 老行为：只报 name@version，run_spec 落 '{}'。
//   - Capabilities 与 Manifest 同时出现时**以 manifest 为准**：manifest 信息更全
//     （它带 run），同一 name@version 只落一条，用 manifest 里那份 run。
//   - manifest 里没写的 capabilities 项 = 「声明了能力但没声明执行方式」：
//     能力照旧声明（参与派发选点），run_spec 落 '{}'。
//   - **不改 protocol 版本号**：manifest 是同版本内新增的**可选**字段，
//     Go 的 json 解码对缺省字段宽容，老节点不受影响。
//
// manifest 里的 run 是节点**自己的执行方式**：中枢只校验形状、只存不执行（I1 边界，
// docs/design/playbooks.md §4.2），所以这里用 json.RawMessage 原样承载。
type HelloFrame struct {
	Type         string           `json:"type"`
	NodeID       string           `json:"node_id"`
	Protocol     int              `json:"protocol"`
	Capabilities []CapabilityDecl `json:"capabilities"`
	Manifest     []ManifestDecl   `json:"manifest,omitempty"` // 新增，可选
	Instances    []InstanceDecl   `json:"instances"`
	Labels       map[string]any   `json:"labels,omitempty"`
}

// CapabilityDecl 是节点声明「我有 review@1」——只有名字与版本。
type CapabilityDecl struct {
	Capability string `json:"capability"`
	Version    int    `json:"version"`
}

// ManifestDecl 是 manifest 的一条：能力 + 它**怎么跑**的声明原文。
//
// Run 用 json.RawMessage 原样承载、**不解析结构**：中枢只校验形状、只存不执行
// （I1 边界，docs/design/playbooks.md §4.2）。runner 以后会长出新形状
// （agent 的 prompt/model/tools），在这里定死字段等于每加一种执行器改一次协议。
// 字段名用 capability（与 hello.capabilities[].capability 一致），
// 节点实现者不需要为同一个概念记两个名字（playbooks §4.2 的 name 是缩写示意）。
type ManifestDecl struct {
	Capability string          `json:"capability"`
	Version    int             `json:"version"`
	Run        json.RawMessage `json:"run"`
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
// **只有能力名、版本、参数与租约**：不含任何「要执行什么」的内容（不变量 I1）。
// 节点自己知道怎么执行这个能力，中枢不告诉它跑什么。

type AssignFrame struct {
	Type        string         `json:"type"`
	TaskID      string         `json:"task_id"`
	Capability  string         `json:"capability"`
	Version     int            `json:"version"`
	Params      map[string]any `json:"params"`
	LeaseUntil  int64          `json:"lease_until"`
	MaxDuration int            `json:"max_duration_sec"`
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
	case NodeFrameWelcome, NodeFrameAssign, NodeFrameCancel, NodeFrameError:
		return true
	}
	return false
}

// IsUpstream 判定一个帧类型是否是节点 → 中枢方向。
func IsUpstream(frameType string) bool {
	switch frameType {
	case NodeFrameHello, NodeFrameHeartbeat, NodeFrameAck, NodeFrameProgress, NodeFrameResult:
		return true
	}
	return false
}
