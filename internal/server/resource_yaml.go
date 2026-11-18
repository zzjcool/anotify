package server

// resource_yaml.go 是两条「从 YAML 创建资源」端点的共用零件：
//
//	POST /v1/namespaces/{ns}/capabilities/yaml
//	POST /v1/namespaces/{ns}/sources/yaml
//
// 请求形状是**JSON 里装一份 YAML 正文**（`{"yaml": "<全文>"}`），不是
// `Content-Type: application/yaml`：仓库里 YAML 正文一律作为字符串装在 JSON 里
// （playbook 的 `spec.files`），而只读端点已经返回同一个 `ResourceYAML{yaml:string}`
// shape —— 写端点复用它，「取 → 改 → 贴回」才是一次 schema 复用的往返。
//
// 三个纪律：
//   - 请求体严格 JSON：未知键 → 400 unknown_field（与 decodeWriteEnvelope 同口径，
//     它们最后都会落到同一个严格解码上）；
//   - YAML 层的问题（语法/未知字段/类型/必填）一律 400 + `details.causes[]`，带行号与
//     字段路径 —— 这是本端点相对 JSON 创建多出来的那部分价值；
//   - 语义校验（artifact / sourcespec / store）**不在这里**：本文件只把文档变成
//     canonical JSON，之后走的就是既有 JSON 创建的同一条路。

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/zzjcool/anotify/internal/yamldoc"
)

// yamlDocTextLimit 是 `yaml` 文本的上限（2 MiB，与 playbook 单文件上界一致）。
//
// 请求体本身的上限是 writeBodyLimit（8 MiB，与其它写端点一致）：JSON 转义会
// 把正文撑大，所以两个上限是两件事，不是同一个数字写两遍。
const yamlDocTextLimit = 2 << 20

// decodeYAMLRequestBody 严格解出 `{"yaml": "…"}` 里的 YAML 正文。
//
// 复用 resource_capability.go 的 yamlDoc（只读端点的响应体）：**同一个 struct 既当
// 读形状又当写形状**，字段名与 schema 就不可能分叉。
func decodeYAMLRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, errStatus(http.StatusBadRequest, reasonBadRequest, "yaml 文档为空")
	}
	var doc yamlDoc
	// 显式传 nil 而不是 w：MaxBytesReader 在超限时会自己给 w 写 413 头，之后我们的
	// 400 Status 体就会变成「413 头 + 400 体」——本仓的 statusWriter 只认第一次
	// WriteHeader（middleware.go），访问日志和响应体会不一致。这里要自己掌握唯一的响应。
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, writeBodyLimit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, errStatus(http.StatusBadRequest, reasonBadRequest, "请求体超过 8 MiB 上限")
		}
		return nil, unknownFieldError(err)
	}
	_ = r.Body.Close()
	text := doc.YAML
	if strings.TrimSpace(text) == "" {
		// 缺失与空串是同一件事：都没有可解析的文档（契约 §1.3）。
		return nil, errStatus(http.StatusBadRequest, reasonBadRequest, "yaml 文档为空")
	}
	if len(text) > yamlDocTextLimit {
		return nil, errStatus(http.StatusBadRequest, reasonBadRequest, "yaml 文本超过 2 MiB 上限")
	}
	return []byte(text), nil
}

// writeYAMLError 把 yamldoc 的问题落成 400 + `details.causes[]`；其它错误走统一映射。
//
// 顶层 `reason` = 第一条 cause 的 reason、`message` = 第一条问题的人话（含行号与字段
// 路径，与 Problem.String() 同形），这样「只看 message 的前端」也能定位到行。
// details.causes[].message 已带「第 N 行：」前缀、字段路径在 Field 里（不重复拼）。
func writeYAMLError(w http.ResponseWriter, err error) {
	var ye *yamldoc.Error
	if !errors.As(err, &ye) || len(ye.Problems) == 0 {
		writeAPIError(w, err)
		return
	}
	causes := ye.Causes()
	out := make([]statusCause, 0, len(causes))
	for _, c := range causes {
		out = append(out, statusCause{Reason: c.Reason, Field: c.Field, Message: c.Message})
	}
	writeStatus(w, http.StatusBadRequest, ye.Reason(), ye.Problems[0].String(), &statusDetails{Causes: out})
}
