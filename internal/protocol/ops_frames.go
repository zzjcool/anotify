package protocol

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

// LogsErrNoLocalArchive 是 logs_response.error 的唯一取值：这台节点上没有那次
// 执行的档案（被 GC 了 / 老任务 / 不是这台机器跑的）。
const LogsErrNoLocalArchive = "no_local_archive"

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
}

// LogsResponseFrame 是节点对一次 logs_request 的回答。
//
// 三条纪律：
//   - 配对靠 requestId：中枢看不到的 requestId 一律丢弃（晚到的回包、过期请求）。
//   - error 非空 = 没取到（目前只有 LogsErrNoLocalArchive）；空 error + 两个空串是
//     「这次执行什么都没打印」，两者语义不同，不能合并。
//   - 编码后 ≤ LogsResponseMaxBytes：truncated=true 表示档案里还有更多（档案本身
//     完整，被截的是这次回包）。
type LogsResponseFrame struct {
	Type      string `json:"type"`
	TaskID    string `json:"taskId"`
	RequestID string `json:"requestId"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated"`
	Bytes     int64  `json:"bytes"`
	Error     string `json:"error,omitempty"`
}
