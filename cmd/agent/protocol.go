package main

import (
	"encoding/json"
	"fmt"

	"github.com/zzjcool/anotify/internal/ws"
)

// 本文件是 agent 侧的**帧层**：类型、常量、编解码。
//
// 帧结构一律复用 internal/ws 的导出类型（`type x = ws.X` 是**别名**，不是新类型），
// 这样 JSON tag 只有一处定义，编译器就挡住了 tag 漂移——agent 侧改不动服务端的 tag，
// 也就不用维护第二份定义。唯一的例外是 resultFrame（见下）。

// ---------- 上行帧（节点 → 中枢）----------

// 全部是 internal/ws 的类型**别名**：tag / 字段名 / 顺序与服务端同源。
type (
	helloFrame     = ws.HelloFrame
	ackFrame       = ws.AckFrame
	heartbeatFrame = ws.HeartbeatFrame
	progressFrame  = ws.ProgressFrame

	capabilityDecl = ws.CapabilityDecl
	manifestDecl   = ws.ManifestDecl
	instanceDecl   = ws.InstanceDecl
)

// ---------- 下行帧（中枢 → 节点）----------

// 同上，别名复用（tag 以 internal/ws 为准，docs/contracts/CONTRACT.md §6.1）。
type (
	welcomeFrame = ws.WelcomeFrame
	assignFrame  = ws.AssignFrame
	cancelFrame  = ws.CancelFrame
	errorFrame   = ws.ErrorFrame
)

// ---------- 常量 ----------

// 帧类型名取自 internal/ws 的常量（字符串本身是协议的一部分，不在本包重写一遍）。
const (
	frameTypeHello     = ws.NodeFrameHello
	frameTypeHeartbeat = ws.NodeFrameHeartbeat
	frameTypeAck       = ws.NodeFrameAck
	frameTypeProgress  = ws.NodeFrameProgress
	frameTypeResult    = ws.NodeFrameResult

	frameTypeWelcome = ws.NodeFrameWelcome
	frameTypeAssign  = ws.NodeFrameAssign
	frameTypeCancel  = ws.NodeFrameCancel
	frameTypeError   = ws.NodeFrameError
)

// result.status 的两个取值（契约 §6.1）。internal/ws 只在注释里写了
// `// succeeded | failed`（node_protocol.go ResultFrame.Status），没有导出常量，
// 所以这里只能写字面量：这是协议的**取值**，不是帧类型名。
const (
	statusSucceeded = "succeeded"
	statusFailed    = "failed"
)

// ---------- resultFrame（本地定义，唯一不复用的一帧）----------

// resultFrame 是本 agent 发出的 result 帧。
//
// 为什么不复用 ws.ResultFrame：I5 要求 out 能表达三态
//
//	键缺席   → 没回报出口（没声明 run.out，或任务没成功：失败/超时/取消）
//	"out":null → 明确到头
//	"out":"fix"→ 出口名
//
// 而 ws.ResultFrame.MarshalJSON 无条件写 "out"，编码侧无法表达「缺席」；
// internal/ws 是硬边界（不改），所以在 agent 侧本地定义一份。
//
// 与 ws.ResultFrame 的差异**只有** out 的 omitempty：字段名/tag 集合由
// TestResultFrameTagsMatchServer 用反射逐项对齐，漂移即测试失败。
type resultFrame struct {
	Type      string   `json:"type"`
	TaskID    string   `json:"task_id"`
	Status    string   `json:"status"`
	Out       *string  `json:"out,omitempty"` // nil → JSON 里没有 out 键（没回报出口）
	Summary   string   `json:"summary"`
	Artifacts []string `json:"artifacts"` // 空时必须是 []，不是 null
	Error     string   `json:"error"`
}

// MarshalJSON 只做一件事：把 nil 的 Artifacts 归一成空切片。
//
// 契约要求「空列表返回 [] 而不是 null」（服务端/前端都把 null 当异常）。
// 调用方（runner outcome）本来就该给非 nil 切片，但帧层自己兜住更省事：
// 用一个无方法的别名类型转一次，字段 tag 与结构体完全一致，不存在第二份定义。
func (f resultFrame) MarshalJSON() ([]byte, error) {
	type alias resultFrame // 丢掉方法集，字段与 tag 原样保留
	a := alias(f)
	if a.Artifacts == nil {
		a.Artifacts = []string{}
	}
	return json.Marshal(a)
}

// ---------- 编解码 ----------

// encodeFrame 是上行帧的统一出口（json.Marshal 包装）。
func encodeFrame(f any) ([]byte, error) {
	data, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("encode frame: %w", err)
	}
	return data, nil
}

// unknownFrame 表示「JSON 解析成功但 type 不认」。
//
// 与 internal/ws 的处理口径一致：这不是错误，协议要求回一个 error 帧
// （code=unknown_type）而不是断开连接，所以 decodeDownstream 返回它 + nil error。
type unknownFrame struct{ Type string }

// decodeEnvelope 只取帧类型，用于先分流再精确解码。
type decodeEnvelope struct {
	Type string `json:"type"`
}

// decodeDownstream 把中枢发来的 JSON 解码成对应的下行帧。
//
// 返回 *welcomeFrame / *assignFrame / *cancelFrame / *errorFrame *别名* 指针
// （即 *ws.WelcomeFrame 等）；类型未知返回 (&unknownFrame{Type: t}, nil)；
// JSON 本身坏了返回 error（对应 code=bad_frame 的接收侧处理）。
func decodeDownstream(data []byte) (any, error) {
	var env decodeEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("bad frame json: %w", err)
	}
	switch env.Type {
	case frameTypeWelcome:
		var f welcomeFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode welcome: %w", err)
		}
		return &f, nil
	case frameTypeAssign:
		var f assignFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode assign: %w", err)
		}
		return &f, nil
	case frameTypeCancel:
		var f cancelFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode cancel: %w", err)
		}
		return &f, nil
	case frameTypeError:
		var f errorFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode error: %w", err)
		}
		return &f, nil
	default:
		return &unknownFrame{Type: env.Type}, nil
	}
}
