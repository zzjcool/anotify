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
	applyFrame     = ws.ApplyFrame

	// appliedDecl 是「我缓存了什么」的陈述（v2 新增）。它**不是**能力声明：
	// 中枢只拿它对账已部署的产物，不会凭它把能力归属给这台机器（裁决 D4）。
	appliedDecl  = ws.AppliedDecl
	instanceDecl = ws.InstanceDecl
)

// ---------- 下行帧（中枢 → 节点）----------

// 同上，别名复用（tag 以 internal/ws 为准，docs/contracts/CONTRACT.md §6.1）。
type (
	welcomeFrame = ws.WelcomeFrame
	assignFrame  = ws.AssignFrame
	deployFrame  = ws.DeployFrame
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
	frameTypeApply     = ws.NodeFrameApply

	frameTypeWelcome = ws.NodeFrameWelcome
	frameTypeAssign  = ws.NodeFrameAssign
	frameTypeDeploy  = ws.NodeFrameDeploy
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
// out 是**两态**（契约 §2.5）：
//
//	键缺席 / null → 这次没选出口（到此为止）
//	"out":"fix"  → 走 fix 出口
//
// 缺省与 null 等价（服务端两者都解成 nil），所以这里的 omitempty 只是「不选出口时
// 不发脏字段」的书写习惯，不是语义区分。
//
// 不从 internal/ws 复用结构体：那边是**服务端解析**的边界（含帧常量/下行帧），
// agent 不该把服务端包拉进二进制。两份的字段名与 tag **必须一致**，由
// TestResultFrameTagsMatchServer 用反射逐项对齐，漂移即测试失败。
type resultFrame struct {
	Type      string   `json:"type"`
	TaskID    string   `json:"task_id"`
	Status    string   `json:"status"`
	Out       *string  `json:"out,omitempty"` // nil → 不发 out 键（没选出口）
	Summary   string   `json:"summary"`       // 成功=说明 / 失败=原因
	Artifacts []string `json:"artifacts"`     // 空时必须是 []，不是 null
	Error     string   `json:"error"`         // 兼容位：老代码只填 error 时服务端兜底
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
// 返回 *welcomeFrame / *assignFrame / *deployFrame / *cancelFrame / *errorFrame
// *别名* 指针（即 *ws.WelcomeFrame 等）；类型未知返回 (&unknownFrame{Type: t}, nil)；
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
	case frameTypeDeploy:
		var f deployFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode deploy: %w", err)
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
