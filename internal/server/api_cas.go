package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

// CAS 令牌承载方式（W1 / api-conventions §3.4 / A9）。
//
// 只有一种承载方式，且 PUT/PATCH 与 DELETE 的位置不同：
//
//	PUT / PATCH 已有资源  → body `metadata.resourceVersion`
//	DELETE 已有资源       → 没有 body，唯一承载是 `?resourceVersion=<URL 编码后的字符串>`
//
// 不并存 If-Match 头，DELETE 也没有请求体——两套协议并存时，客户端与服务端对
// 「哪个令牌生效」永远各说一套，冲突时无法判定谁错了。

// casTokenFromQuery 取 DELETE 的 CAS 令牌。
//
// 缺失直接 400 resource_version_required：静默 last-write-wins 会让「两个人的删除
// 互相覆盖」变成正常现象，比报错危险得多。
func casTokenFromQuery(r *http.Request) (string, error) {
	token := strings.TrimSpace(r.URL.Query().Get("resourceVersion"))
	if token == "" {
		return "", errStatus(http.StatusBadRequest, reasonResourceVersionRequired,
			"resourceVersion query parameter is required for DELETE")
	}
	return token, nil
}

// casTokenFromMeta 取 PUT / PATCH 的 CAS 令牌（body metadata.resourceVersion）。
func casTokenFromMeta(meta objectMeta) (string, error) {
	token := strings.TrimSpace(meta.ResourceVersion)
	if token == "" {
		return "", errStatus(http.StatusBadRequest, reasonResourceVersionRequired,
			"metadata.resourceVersion is required")
	}
	return token, nil
}

// writeCASError 写 CAS 相关错误；返回 true 表示已写过响应。
//
// 只处理 CAS 家族（required / invalid / conflict），其余错误交回调用方走 writeAPIError，
// 避免这里悄悄吞掉一个真实 500。
func writeCASError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	var ae *apiError
	if asAPIError(err, &ae) {
		if ae.Reason == reasonResourceVersionRequired || ae.Reason == reasonResourceVersionInvalid ||
			ae.Reason == reasonResourceVersionConflict {
			writeAPIError(w, err)
			return true
		}
		return false
	}
	writeAPIError(w, err)
	return true
}

// asAPIError 是 errors.As 的本地薄封装，避免 api_cas.go 再 import errors 两次。
func asAPIError(err error, dst **apiError) bool {
	for err != nil {
		if ae, ok := err.(*apiError); ok {
			*dst = ae
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// cursorEnvelope 是不透明 continue 令牌的载荷。
//
// 契约要求（W6 / api-conventions §3.3 / §8.3）：cursor 必须绑定 namespace + 筛选条件，
// 不能拿另一个 ns 的 cursor 读越权记录。这里的做法是把 (binding, 位置) 一起编码进
// 令牌，服务端解码后**逐字比对** binding；不匹配就当坏令牌（400），不做「尽力解释」。
// 令牌里没有秘密，因此不需要签名：防的是误用与越权组合，不是伪造。
type cursorEnvelope struct {
	B string `json:"b"` // binding: scope + filter，逐字比对
	I string `json:"i"` // 上一页最后一行的 id（keyset 破平局用）
	T int64  `json:"t"` // 上一页最后一行的 created_at
}

// encodeCursor 生成不透明 continue 令牌。
func encodeCursor(binding, id string, at int64) string {
	raw, err := json.Marshal(cursorEnvelope{B: binding, I: id, T: at})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeCursor 解码并校验 binding（ns + 筛选条件）。
func decodeCursor(raw, binding string) (id string, at int64, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", 0, nil
	}
	decoded, decErr := base64.RawURLEncoding.DecodeString(raw)
	if decErr != nil {
		return "", 0, errStatus(http.StatusBadRequest, reasonBadRequest, "continue token is malformed")
	}
	var env cursorEnvelope
	if err := json.Unmarshal(decoded, &env); err != nil {
		return "", 0, errStatus(http.StatusBadRequest, reasonBadRequest, "continue token is malformed")
	}
	if env.B != binding {
		return "", 0, errStatus(http.StatusBadRequest, reasonBadRequest, "continue token does not belong to this query")
	}
	if env.I == "" || env.T <= 0 {
		return "", 0, errStatus(http.StatusBadRequest, reasonBadRequest, "continue token is malformed")
	}
	return env.I, env.T, nil
}

// eventCursorBinding 生成事件列表的 cursor binding（ns + 筛选条件）。
func eventCursorBinding(namespaceID, source, traceID string) string {
	return "events|" + namespaceID + "|" + source + "|" + traceID
}

// emissionCursorBinding 生成 emissions 列表的 cursor binding（ns + task）。
func emissionCursorBinding(namespaceID, taskID string) string {
	return "emissions|" + namespaceID + "|" + taskID
}
