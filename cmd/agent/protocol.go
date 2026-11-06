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

	// emitFrame 是一次**中途产出**（emit-protocol.md §3.1）：能力进程经本地 socket
	// 交给 agent，agent 原样发给中枢。它与 result 帧的区别是「跑完之前也能发」，
	// 而且它是**唯一**的产出通道（一次性返回只是「只 emit 一次」的特例）。
	emitFrame = ws.EmitFrame

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

	// upgradeFrame / upgradeAckFrame 是中枢下发升级与节点的回报
	// （CONTRACT-agent-upgrade §5）。下载走普通 HTTP，帧里只有「去哪拿、该是什么」。
	upgradeFrame    = ws.UpgradeFrame
	upgradeAckFrame = ws.UpgradeAckFrame

	// emitAckFrame 是中枢对一次 emit 的确认（emit-protocol.md §3.2）。
	// error 非空 = 拒收；重复 seq 一律回 ok（幂等，不重复派发下游）。
	emitAckFrame = ws.EmitAckFrame
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
	frameTypeEmit      = ws.NodeFrameEmit

	frameTypeWelcome = ws.NodeFrameWelcome
	frameTypeAssign  = ws.NodeFrameAssign
	frameTypeDeploy  = ws.NodeFrameDeploy
	frameTypeCancel  = ws.NodeFrameCancel
	frameTypeError   = ws.NodeFrameError
	// frameTypeUpgrade / frameTypeUpgradeAck：中枢下发升级、节点回报结果
	// （CONTRACT-agent-upgrade §5）。漏了 frameTypeUpgrade 的后果特别隐蔽——
	// 控制台显示「已下发」，节点却当作未知帧丢掉，永远等不到 ack。
	frameTypeUpgrade    = ws.NodeFrameUpgrade
	frameTypeUpgradeAck = ws.NodeFrameUpgradeAck
	// frameTypeEmitAck 必须在这里**显式**认出来：漏了它，中枢的每次确认都会落到
	// unknownFrame 分支——emit 永远等不到 ack，--wait-routed 必然超时（§5）。
	frameTypeEmitAck = ws.NodeFrameEmitAck

	// frameTypeLogsRequest / frameTypeLogsResponse：中枢按需取日志的一对帧
	// （CONTRACT-logs-on-node §1）。漏了 logs_request 的后果是控制台永远只看到
	// 「节点离线 / 超时」——而登录机器 `cat runs/...` 明明有内容。
	frameTypeLogsRequest  = ws.NodeFrameLogsRequest
	frameTypeLogsResponse = ws.NodeFrameLogsResponse
)

// 升级路径里的常量别名（不是类型，所以单独一个 const 块）。
const (
	// nodeBinaryPath 是中枢上 agent 二进制的下载路径（帧没给 url_path 时用它）。
	nodeBinaryPath = ws.NodeBinaryPath

	// 契约 §5 的失败短码：会原样出线给控制台，字符串是冻结的。
	upgradeErrNeedRoot       = ws.UpgradeErrNeedRoot
	upgradeErrBadSHA         = ws.UpgradeErrBadSHA
	upgradeErrDownloadFailed = ws.UpgradeErrDownloadFailed
	upgradeErrNotSupported   = ws.UpgradeErrNotSupported
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
// **没有 out 字段**：P1.5 硬切之后出口只在 emit 帧里（typed-pipeline §2）。
// 这不只是「不带这个键」，而是服务端结构体里也没有它（ws.ResultFrame）：
// 老帧里多出来的 `"out":"fix"` 会被 encoding/json 当成未知字段忽略（不报错、
// 不生效），所以「写了一张老产物、脚本没改成 emit」的现场表现是**没有下游**，
// 而不是一条神秘错误。
//
// 不从 internal/ws 复用结构体：那边是**服务端解析**的边界（含帧常量/下行帧），
// agent 不该把服务端包拉进二进制。两份的字段名与 tag **必须一致**，由
// TestResultFrameTagsMatchServer 用反射逐项对齐，漂移即测试失败。
type resultFrame struct {
	Type      string   `json:"type"`
	TaskID    string   `json:"task_id"`
	Status    string   `json:"status"`
	Summary   string   `json:"summary"`   // 成功=说明 / 失败=原因
	Artifacts []string `json:"artifacts"` // 空时必须是 []，不是 null
	Error     string   `json:"error"`     // 兼容位：老代码只填 error 时服务端兜底
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
// 返回 *welcomeFrame / *assignFrame / *deployFrame / *cancelFrame / *errorFrame /
// *emitAckFrame / *logsRequestFrame *别名* 指针（即 *ws.WelcomeFrame 等）；类型未知返回
// (&unknownFrame{Type: t}, nil)；JSON 本身坏了返回 error（对应 code=bad_frame 的接收侧处理）。
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
	case frameTypeUpgrade:
		var f upgradeFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode upgrade: %w", err)
		}
		return &f, nil
	case frameTypeEmitAck:
		var f emitAckFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode emit_ack: %w", err)
		}
		return &f, nil
	case frameTypeLogsRequest:
		var f logsRequestFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode logs_request: %w", err)
		}
		return &f, nil
	default:
		return &unknownFrame{Type: env.Type}, nil
	}
}
