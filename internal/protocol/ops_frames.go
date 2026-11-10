package protocol

import (
	"errors"
	"fmt"
	"strings"
)

// 保留的运维帧：升级与节点日志。
//
// 它们不是 v4 重新设计的协议，而是既有功能的**唯一定义源搬迁**：帧从 internal/ws
// 搬到这里，字段名统一成 camelCase（wire 纪律），语义与行为不变。ws 只做传输与
// 状态机，不再自带一份帧结构。
const (
	// FrameUpgrade 让节点去下载并替换自己的二进制。
	FrameUpgrade = "upgrade"
	// FrameUpgradeAck 是节点对一次 upgrade 的回报。
	FrameUpgradeAck = "upgrade_ack"
	// FrameLogsRequest 让节点把本机某次执行的日志尾巴回给中枢。
	FrameLogsRequest = "logs_request"
	// FrameLogsResponse 是节点对 logs_request 的回答（按 requestId 配对）。
	FrameLogsResponse = "logs_response"
)

// 日志错误码是**封闭集合**：中枢按枚举做映射，新增码必须双方同时上。
const (
	// LogsErrNoLocalArchive：这台节点上没有那次执行的档案（被 GC 了 / 老任务 /
	// 不是这台机器跑的）。
	LogsErrNoLocalArchive = "no_local_archive"
	// LogsErrUnreadable：档案在，但结构化记录索引缺失/损坏/版本不支持/写入失败
	// 锁存——无法证明记录完整性。绝不能退化成一个空成功。
	LogsErrUnreadable = "logs_unreadable"
	// LogsErrIllegalRequest：请求本身不合法（空 taskId/requestId、attempt<0、
	// tailLines/sinceSeconds 越界）。
	LogsErrIllegalRequest = "illegal_log_request"
)

// 日志记录与查询的共享常量。
const (
	// LogStreamStdout / LogStreamStderr 是 LogLine.stream 的封闭取值。
	LogStreamStdout = "stdout"
	LogStreamStderr = "stderr"
	// LogLineTextMaxBytes 是单条记录文字段的上限（64 KiB）。超长行由节点按
	// 实际采集片段形成多条记录；这不代表脚本写过额外换行，原始字节仍以
	// stdout.log/stderr.log 为准。
	LogLineTextMaxBytes = 64 << 10
	// LogsTailLinesMax 是 logs_request.tailLines 的上限（0/省略 = 不限条数）。
	LogsTailLinesMax = 5000
)

// FrameSoftLimit 是逐帧软上限（1 MiB）。
//
// 在 protocol 里定义而不是 ws：logs_response 的编码上限与它是同一个数，节点侧
// 必须在编码**之后**量字节数并截断（见 LogsResponseMaxBytes）。两条常量分居两包
// 迟早会漂移，症状是「日志回包被当作超大帧拒掉」。
const FrameSoftLimit = 1 << 20

// LogsResponseMaxBytes 是**一帧 logs_response 编码后**的上限。
//
// 与 FrameSoftLimit 同值是刻意为之：服务端对超限上行帧的处置是先回
// frame_too_large 再 1009 关连接——一条「日志太大」把连接打断，比取不到日志严
// 重得多。所以节点侧必须自己先截断。
const LogsResponseMaxBytes = FrameSoftLimit

// UpgradeFrame 让节点把自己的二进制升级到目标版本。
//
// 帧里**不带二进制**：15 MB 的帧会撑爆心跳与背压。节点收到后用自己的节点 token
// 走普通 HTTP 下载，校验 sha256 后原子替换。
type UpgradeFrame struct {
	Type string `json:"type"`
	// TargetVersion 是中枢自己的版本（控制台展示的「已是最新」也按它比较）。
	TargetVersion string `json:"targetVersion"`
	// SHA256 是 64 位十六进制（不带 "sha256:" 前缀）——下载后必须逐字节对上。
	SHA256 string `json:"sha256"`
	// URLPath 是中枢上的下载路径（相对 server 根；节点自己拼）。
	URLPath string `json:"urlPath"`
}

// 节点升级失败的判别短码（upgrade_ack.error）。
//
// 它们会原样落库并出线给控制台，所以字符串是冻结契约的一部分。
const (
	UpgradeErrNeedRoot       = "need_root"
	UpgradeErrBadSHA         = "bad_sha"
	UpgradeErrDownloadFailed = "download_failed"
	UpgradeErrNotSupported   = "not_supported"
)

// NormalizeUpgradeError 把 ack 里的错误码收敛到契约枚举（未知码 → ""）。
//
// 为什么不让未知码原样落库：出线形状是封闭集合（控制台按枚举做映射），存一个 UI
// 不认识的码只会显示「升级失败（未知原因）」；原始值已经进了 WARN 日志。
func NormalizeUpgradeError(code string) string {
	switch code {
	case UpgradeErrNeedRoot, UpgradeErrBadSHA, UpgradeErrDownloadFailed, UpgradeErrNotSupported:
		return code
	}
	return ""
}

// UpgradeAckFrame 是节点对 upgrade 的回报。
//
// ok=false 时 Error 是可判别短码；中枢落库时成功写版本列、失败只写升级状态
// （节点上的二进制没换，版本列保持旧值才诚实）。
type UpgradeAckFrame struct {
	Type string `json:"type"`
	OK   bool   `json:"ok"`
	// Version / Commit 是升级后**实际生效**的版本；ok=false 时为空。
	Version string `json:"version,omitempty"`
	Commit  string `json:"commit,omitempty"`
	Error   string `json:"error"`
}

// NodeBinaryPath 是节点下载二进制的路径（GET 到中枢根上）。
//
// 与 UpgradeFrame.URLPath 共用同一个常量：改路径只有一处，帧和 HTTP 路由不会漂移。
const NodeBinaryPath = "/v1/node/binary"

// NodeStreamPath 是节点 WS 连接的路径（GET 升级到 WebSocket，拼在中枢根上）。
//
// 与 NodeBinaryPath 并列：地址字符串是协议的一部分，节点与中枢必须用同一个常量，
// 不要在两侧各写一份字面量。中枢侧的空 path 或 "/" 归一为本值；显式配置的
// 反代前缀原样使用（本常量只是标准默认值），不提供 legacy 别名。
const NodeStreamPath = "/v1/node/stream"

// LogsRequestFrame 让节点回报某条任务的日志尾巴。
//
// 帧里只有「问的是哪条任务的哪一次请求」：任务入参（params）属于内部输入快照，
// 不该为了让一次排障取日志而把可能敏感的入参搬过网络。
//
// requestId 由中枢生成（同一条连接上可能同时有多条请求在飞），节点原样回带，
// 中枢只做字符串相等比较——「A 的日志被安到 B 头上」这种错会静默发生，必须靠配对。
type LogsRequestFrame struct {
	Type      string `json:"type"`
	TaskID    string `json:"taskId"`
	RequestID string `json:"requestId"`
	// Attempt 是可选的目标 attempt（整数 ≥1）。省略（0）= 该逻辑 task 最近一次
	// 有档案的 attempt。跨节点重试时中枢必须按 attempt 的历史节点发请求，不能
	// 拿当前节点的日志冒充另一次执行。
	Attempt int `json:"attempt,omitempty"`
	// TailLines 是记录条数上限（0/省略 = 不限）。上限 LogsTailLinesMax；
	// 节点先按 sinceSeconds 过滤、再取最新 tailLines 条。
	TailLines int `json:"tailLines,omitempty"`
	// SinceSeconds 是时间窗（0/省略 = 不限）。节点用自己的当前时间算出阈值，
	// 只回采集时间不早于阈值的记录；中枢不按服务器时钟二次过滤。
	SinceSeconds int `json:"sinceSeconds,omitempty"`
}

// ValidateLogsRequest 校验 logs_request 的封闭参数域。
//
// 非法请求必须回 LogsErrIllegalRequest，而不是把它当成「0 = 最新」悄悄读档：
// 负数 attempt / 越界 tailLines 若被归一化，中枢看到的成功就无法与它发出的
// 请求一一对应。
func ValidateLogsRequest(f *LogsRequestFrame) error {
	if f == nil {
		return errors.New("protocol: logs_request 为空")
	}
	if f.Type != FrameLogsRequest {
		return fmt.Errorf("protocol: 帧类型 %q 不是 %s", f.Type, FrameLogsRequest)
	}
	if strings.TrimSpace(f.TaskID) == "" {
		return errors.New("protocol: logs_request.taskId 为空")
	}
	if strings.TrimSpace(f.RequestID) == "" {
		return errors.New("protocol: logs_request.requestId 为空")
	}
	if f.Attempt < 0 {
		return fmt.Errorf("protocol: logs_request.attempt 不能为负（%d）", f.Attempt)
	}
	if f.TailLines < 0 || f.TailLines > LogsTailLinesMax {
		return fmt.Errorf("protocol: logs_request.tailLines=%d 越界（0..%d）", f.TailLines, LogsTailLinesMax)
	}
	if f.SinceSeconds < 0 {
		return fmt.Errorf("protocol: logs_request.sinceSeconds 不能为负（%d）", f.SinceSeconds)
	}
	return nil
}

// LogLine 是一条真实采集到的日志记录。
//
// 恰有四个 wire 字段，不增不减：
//   - sequence：attempt 内跨 stdout/stderr 严格递增、从 1 开始。它描述**节点实际
//     观察并串行记录的顺序**，不是脚本跨流的因果先后，也不承诺两个 pipe 的写入顺序。
//   - stream：stdout / stderr。
//   - text：实际采集文字（LF 分行，EOF 残片是真实记录；单段 ≤ 64 KiB，超长行按
//     实际片段拆成多条）。无效 UTF-8 按标准 JSON 文字投影（替换字符），raw 档案
//     保持逐字节不变。
//   - at：节点采集该记录时的真实时间（RFC3339Nano）；不是 finishedAt，也不校正成
//     中枢时钟。
type LogLine struct {
	Sequence int    `json:"sequence"`
	Stream   string `json:"stream"`
	Text     string `json:"text"`
	At       string `json:"at"`
}

// LogsResponseFrame 是节点对一次 logs_request 的回答。
//
// 四条纪律：
//   - 配对靠 requestId：中枢看不到的 requestId 一律丢弃（晚到的回包、过期请求）。
//   - error 非空 = 没取到（封闭集合：no_local_archive / logs_unreadable /
//     illegal_log_request）；空 error + 零条记录是「这次执行什么都没打印」，
//     两者语义不同，不能合并。
//   - 编码后 ≤ LogsResponseMaxBytes：truncated=true 表示档案里还有更多（档案本身
//     完整，被截的是这次回包）。
//   - Lines 只在成功响应里出现：成功必须是非 null 数组（零条 = []），错误响应
//     不能伪造成功记录。用指针是为了让 wire 上「错误时没有 lines 键」与
//     「成功零条 = []」在同一个 struct 上都能精确表达。
type LogsResponseFrame struct {
	Type      string `json:"type"`
	TaskID    string `json:"taskId"`
	RequestID string `json:"requestId"`
	// Attempt 是**实际**读到的 attempt：成功时必填（≥1），错误/无档案时为空。
	// 错误响应不能伪造一个 latest 冒充请求的历史 attempt。
	Attempt   int        `json:"attempt,omitempty"`
	Stdout    string     `json:"stdout"`
	Stderr    string     `json:"stderr"`
	Truncated bool       `json:"truncated"`
	Bytes     int64      `json:"bytes"`
	Error     string     `json:"error,omitempty"`
	Lines     *[]LogLine `json:"lines,omitempty"`
}

// SetLogLines 设置成功响应的记录数组：nil 也编码成 []，而不是缺键/null。
func (f *LogsResponseFrame) SetLogLines(lines []LogLine) {
	if lines == nil {
		lines = []LogLine{}
	}
	f.Lines = &lines
}

// LogLines 返回记录数组（nil 安全；错误响应返回 nil）。
func (f *LogsResponseFrame) LogLines() []LogLine {
	if f == nil || f.Lines == nil {
		return nil
	}
	return *f.Lines
}
