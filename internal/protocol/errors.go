package protocol

// 帧级错误码（协议错误，不是任务失败码）。
//
// 三个错误域不要混用：这里是「这条帧本身有问题」；任务失败用 enums.go 的
// Reason*（进 Task.status.result.error）；公共 API 用 Status.reason。
const (
	// ErrCodeBadFrame 帧不是合法 JSON / 字段类型不对。
	ErrCodeBadFrame = "bad_frame"
	// ErrCodeUnknownType 帧能解析，但 type 不在本包定义的集合里。
	ErrCodeUnknownType = "unknown_type"
	// ErrCodeInvalidTransition 当前会话状态不允许这条帧（如未 accept 就 result）。
	ErrCodeInvalidTransition = "invalid_transition"
	// ErrCodeProtocolMismatch 节点 speak 的协议版本与服务端不同。
	//
	// 与 bad_frame 分开：这是版本不兼容，运维该去升级二进制，而不是查 JSON 结构。
	ErrCodeProtocolMismatch = "protocol_mismatch"
	// ErrCodeFrameTooLarge 上行帧超过服务端软上限（FrameSoftLimit）。
	ErrCodeFrameTooLarge = "frame_too_large"
	// ErrCodeParamsTooLarge 任务 params 超过中枢上限。
	//
	// 单独一个码是为了可诊断：并进 invalid_transition 会把「你的 params 太大」
	// 这个病因埋掉（症状是父任务反复重派后 abandoned）。
	ErrCodeParamsTooLarge = "params_too_large"
)

// frameErrorCodes 是帧级错误码的封闭集合。
var frameErrorCodes = []string{
	ErrCodeBadFrame,
	ErrCodeUnknownType,
	ErrCodeInvalidTransition,
	ErrCodeProtocolMismatch,
	ErrCodeFrameTooLarge,
	ErrCodeParamsTooLarge,
}

// IsFrameErrorCode 报告 code 是否是帧级错误码。
func IsFrameErrorCode(code string) bool {
	return containsString(frameErrorCodes, code)
}
