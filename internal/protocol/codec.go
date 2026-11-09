package protocol

import (
	"encoding/json"
	"fmt"
)

// 编解码统一入口。
//
// 为什么要一个 Decode：帧的 type 字段是唯一的判别位，节点与会话两侧都要按它分流。
// 各自写一份 switch 的后果是「新加的帧在一边被当成 unknown_type」——而两边都编译
// 通过、单测也各自绿。分流只在这里做一次。
//
// 未知 type 不是错误：返回 (*UnknownFrame, nil)，调用方按协议回
// ErrCodeUnknownType 而不是断开连接（一条老/新帧不该打断整条连接）。
// JSON 本身坏了才是错误（= ErrCodeBadFrame）。

// UnknownFrame 表示解析成功但 type 不在本包定义的集合里。
type UnknownFrame struct {
	Type string
}

// FrameTypeOf 从原始载荷里取出 type（不校验其它字段）。
func FrameTypeOf(data []byte) (string, error) {
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return "", fmt.Errorf("protocol: 帧不是合法 JSON: %w", err)
	}
	return env.Type, nil
}

// Encode 把任意帧编码成 JSON（统一入口，便于加统一字段/度量）。
func Encode(f any) ([]byte, error) {
	data, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("protocol: 编码帧: %w", err)
	}
	return data, nil
}

// Decode 把收到的 JSON 解码成对应类型的帧。
//
// 返回的具体类型是 *HelloFrame / *WelcomeFrame / … / *CancelAckFrame /
// *UpgradeFrame / *LogsResponseFrame；未知 type 返回 (*UnknownFrame, nil)。
func Decode(data []byte) (any, error) {
	frameType, err := FrameTypeOf(data)
	if err != nil {
		return nil, err
	}
	var target any
	switch frameType {
	// 配置 5 帧。
	case FrameHello:
		target = &HelloFrame{}
	case FrameWelcome:
		target = &WelcomeFrame{}
	case FrameConfigChanged:
		target = &ConfigChangedFrame{}
	case FrameConfigGet:
		target = &ConfigGetFrame{}
	case FrameConfigApplied:
		target = &ConfigAppliedFrame{}
	// 执行 10 帧。
	case FrameTaskDispatch:
		target = &TaskDispatchFrame{}
	case FrameTaskAccept:
		target = &TaskAcceptFrame{}
	case FrameTaskReject:
		target = &TaskRejectFrame{}
	case FrameLeaseGrant:
		target = &LeaseGrantFrame{}
	case FrameLeaseRenew:
		target = &LeaseRenewFrame{}
	case FrameLeaseAck:
		target = &LeaseAckFrame{}
	case FrameTaskResult:
		target = &TaskResultFrame{}
	case FrameResultAck:
		target = &ResultAckFrame{}
	case FrameTaskCancel:
		target = &TaskCancelFrame{}
	case FrameCancelAck:
		target = &CancelAckFrame{}
	// 保留运维 4 帧。
	case FrameUpgrade:
		target = &UpgradeFrame{}
	case FrameUpgradeAck:
		target = &UpgradeAckFrame{}
	case FrameLogsRequest:
		target = &LogsRequestFrame{}
	case FrameLogsResponse:
		target = &LogsResponseFrame{}
	default:
		return &UnknownFrame{Type: frameType}, nil
	}
	if err := json.Unmarshal(data, target); err != nil {
		return nil, fmt.Errorf("protocol: 解码 %s: %w", frameType, err)
	}
	return target, nil
}

// upstreamFrames 是节点 → 中枢的帧（含 hello/config.get/config.applied 与执行上行）。
var upstreamFrames = []string{
	FrameHello,
	FrameConfigGet,
	FrameConfigApplied,
	FrameTaskAccept,
	FrameTaskReject,
	FrameLeaseRenew,
	FrameTaskResult,
	FrameCancelAck,
	FrameUpgradeAck,
	FrameLogsResponse,
}

// downstreamFrames 是中枢 → 节点的帧。
var downstreamFrames = []string{
	FrameWelcome,
	FrameConfigChanged,
	FrameTaskDispatch,
	FrameLeaseGrant,
	FrameLeaseAck,
	FrameResultAck,
	FrameTaskCancel,
	FrameUpgrade,
	FrameLogsRequest,
}

// IsUpstream 判定帧类型是否是节点 → 中枢方向。
func IsUpstream(frameType string) bool {
	return containsString(upstreamFrames, frameType)
}

// IsDownstream 判定帧类型是否是中枢 → 节点方向。
func IsDownstream(frameType string) bool {
	return containsString(downstreamFrames, frameType)
}

// UpstreamFrames 返回全部上行帧类型（拷贝，调用方可安全修改）。
func UpstreamFrames() []string {
	out := make([]string, len(upstreamFrames))
	copy(out, upstreamFrames)
	return out
}

// DownstreamFrames 返回全部下行帧类型（拷贝）。
func DownstreamFrames() []string {
	out := make([]string, len(downstreamFrames))
	copy(out, downstreamFrames)
	return out
}
