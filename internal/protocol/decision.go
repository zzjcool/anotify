package protocol

import "time"

const ( // 决策凭据与端点（#522 / A3）：router 只选结局，平台按 outcomes 表执行
	DecisionPathPrefix  = "/v1/tasks/"
	DecisionPathSuffix  = "/decision"
	DecisionAuthScheme  = EmitAuthScheme
	DecisionAuthHeader  = EmitAuthHeader
	DecisionContentType = EmitContentType
	DecisionTokenTTL    = 10 * time.Minute // 凭据有效期
	DecisionMaxBody     = 1 << 20          // 请求体上限
)

// DecisionPatternPath 必须是常量：openapi 一致性检查静态求值 mux 的模式表达式。
const DecisionPatternPath = DecisionPathPrefix + "{taskId}" + DecisionPathSuffix

func DecisionPath(taskID string) string { return DecisionPathPrefix + taskID + DecisionPathSuffix }

type DecisionRequest struct {
	Outcome string         `json:"outcome"`
	Params  map[string]any `json:"params,omitempty"`
}

type DecisionResponse struct {
	TaskID  string `json:"taskId"`
	Outcome string `json:"outcome"`
	Actions int    `json:"actions"`
}
