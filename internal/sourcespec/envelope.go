package sourcespec

// 信封与签名（#513 A1 / design #512）：把「从哪取 type / deliveryId / payload」与「用哪种
// 签名口径」从 Go 搬到配置里。传输仍是 Go 的（信任边界不外包），适配是数据。
//
// 三条纪律：三个维度**各自独立缺省**（只写 `type` 时 `id`/`payload` 仍取今天的语义，于是
// 「不写 envelope」=「写一份全默认」）；指针与 params 模板**同一套方言**（共用 `walkPath`，
// 不新造第二种路径语言）；写入期拒未知键与非法组合（拼错的 `deliveryID` 静默变成「不去重」
// 是线上最难查的一类故障——事件照收，只是重复派发）。

import (
	"encoding/json"
	"strings"
)

// config.signature.scheme 的两个合法值（空 = 缺省 = anotify-sha256）。两者算法相同
// （HMAC-SHA256 over 原始字节），区别只有头名。
const (
	SchemeAnotifySHA256 = "anotify-sha256" // X-Anotify-Signature: sha256=<hex>
	SchemeGitHubSHA256  = "github-sha256"  // X-Hub-Signature-256: sha256=<hex>
)

// Envelope 是 config.envelope（三个维度各自可缺省）。
type Envelope struct {
	Type       *FieldSource   `json:"type,omitempty"`
	DeliveryID *FieldSource   `json:"deliveryId,omitempty"`
	Payload    *PayloadSource `json:"payload,omitempty"`
}

// FieldSource 是 type / deliveryId 的来源：header 与 pointer 二选一。
//
// JoinFrom 只对 type 有意义（`x-github-event` + `$.action` → `issues.opened`）：
// GitHub 的事件类型是「头 + body 字段」两段拼的，只读头会丢掉 action 维度。
type FieldSource struct {
	Header   string `json:"header,omitempty"`
	Pointer  string `json:"pointer,omitempty"`
	JoinFrom string `json:"joinFrom,omitempty"`
}

// PayloadSource 是 payload 的来源：whole（整个投递体）与 pointer 二选一。
type PayloadSource struct {
	Whole   bool   `json:"whole,omitempty"`
	Pointer string `json:"pointer,omitempty"`
}

// Signature 是 config.signature（scheme 空 = 缺省口径）。
type Signature struct {
	Scheme string `json:"scheme,omitempty"`
}

// Match 是 targets[].match（A1 只有 types；`cel` 属 A2）。Types 是精确、大小写敏感的事件
// 类型白名单——不写 match 才是「全部命中」。
type Match struct {
	Types []string `json:"types,omitempty"`
}

// Matches 报告一次事件类型是否命中声明；nil（没写 match）= 恒命中。
func (m *Match) Matches(eventType string) bool {
	if m == nil {
		return true
	}
	for _, want := range m.Types {
		if want == eventType {
			return true
		}
	}
	return false
}

// ParsePointer 归一一个指针：`$.a.b` 与 `a.b` 同义，返回不带 `$` 前缀的形式。段字符集与
// params 模板共用 validPathSegment（数字段 = 数组下标，连字符拒绝）。
func ParsePointer(raw string) (string, error) {
	path := strings.TrimPrefix(strings.TrimSpace(raw), "$")
	path = strings.TrimPrefix(path, ".")
	if path == "" {
		return "", errf("指针 %q 不合法（要形如 $.a.b 或 a.b）", raw)
	}
	for _, seg := range strings.Split(path, ".") {
		if !validPathSegment(seg) {
			return "", errf("指针 %q 的路径段 %q 不合法（只允许字母/数字/下划线，数字段表示数组下标）", raw, seg)
		}
	}
	return path, nil
}

// LookupPointer 按已归一的点号路径取值；任一段取不到 → ok=false。与模板的 lookupPath 共用
// walkPath：路径语言分叉的症状是「参数模板取得到、envelope 取不到」，而配置看起来一样。
func LookupPointer(root map[string]any, path string) (any, bool) {
	return walkPath(root, strings.Split(path, "."))
}

// ScalarText 把指针取到的值转成事件类型 / 投递 id 用的文本。只认标量：对象/数组当 type 是
// 配置写错了，按「取不到」处理（回落源名 / 不去重），而不是把一整段 JSON 塞进事件类型。
func ScalarText(v any) (string, bool) {
	switch val := v.(type) {
	case string:
		return strings.TrimSpace(val), true
	case json.Number:
		return val.String(), true
	case bool:
		if val {
			return "true", true
		}
		return "false", true
	default:
		return "", false
	}
}

// ---------- 归一化 ----------

// envelopeSources 解出 envelope 的三个维度（缺省 → nil）。
func envelopeSources(fields map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	raw, ok := fields["envelope"]
	if !ok || isNull(raw) {
		return nil, nil
	}
	obj, err := decodeFields(raw)
	if err != nil {
		return nil, errf("config.envelope 必须是 JSON 对象")
	}
	if len(obj) == 0 {
		return nil, errf("config.envelope 不能是空对象（要写 type / deliveryId / payload 之一）")
	}
	return obj, nil
}

// normalizeEnvelope 校验归一 config.envelope（缺省 / null → nil）。
func normalizeEnvelope(fields map[string]json.RawMessage) (*Envelope, error) {
	obj, err := envelopeSources(fields)
	if err != nil || obj == nil {
		return nil, err
	}
	out := &Envelope{}
	for _, key := range sortedFieldKeys(obj) {
		switch key {
		case "type":
			out.Type, err = normalizeFieldSource(obj[key], "type", true)
		case "deliveryId":
			out.DeliveryID, err = normalizeFieldSource(obj[key], "deliveryId", false)
		case "payload":
			out.Payload, err = normalizePayloadSource(obj[key])
		default:
			err = errf("config.envelope 不认字段 %q（允许：deliveryId / payload / type）", key)
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// normalizeFieldSource 校验「header 或 pointer」来源（type 额外允许 joinFrom）。
func normalizeFieldSource(raw json.RawMessage, name string, allowJoin bool) (*FieldSource, error) {
	obj, err := decodeFields(raw)
	if err != nil {
		return nil, errf("config.envelope.%s 必须是 JSON 对象", name)
	}
	out := &FieldSource{}
	for _, key := range sortedFieldKeys(obj) {
		var text string
		if err := json.Unmarshal(nonNull(obj[key], `""`), &text); err != nil {
			return nil, errf("config.envelope.%s.%s 必须是字符串", name, key)
		}
		switch key {
		case "header":
			out.Header = strings.TrimSpace(text)
		case "pointer":
			out.Pointer = strings.TrimSpace(text)
		case "joinFrom":
			if !allowJoin {
				return nil, errf("config.envelope.%s 不认字段 %q（只有 type 支持 joinFrom）", name, key)
			}
			out.JoinFrom = strings.TrimSpace(text)
		default:
			return nil, errf("config.envelope.%s 不认字段 %q（允许：header / pointer / joinFrom）", name, key)
		}
	}
	switch {
	case out.Header == "" && out.Pointer == "":
		return nil, errf("config.envelope.%s 要声明 header 或 pointer（二选一）", name)
	case out.Header != "" && out.Pointer != "":
		return nil, errf("config.envelope.%s 的 header 与 pointer 只能二选一", name)
	}
	if out.Pointer != "" {
		if out.Pointer, err = ParsePointer(out.Pointer); err != nil {
			return nil, errf("config.envelope.%s：%v", name, err)
		}
	}
	if out.JoinFrom != "" {
		if out.JoinFrom, err = ParsePointer(out.JoinFrom); err != nil {
			return nil, errf("config.envelope.%s.joinFrom：%v", name, err)
		}
	}
	return out, nil
}

// normalizePayloadSource 校验 payload 来源（whole / pointer 二选一）。
func normalizePayloadSource(raw json.RawMessage) (*PayloadSource, error) {
	obj, err := decodeFields(raw)
	if err != nil {
		return nil, errf("config.envelope.payload 必须是 JSON 对象")
	}
	out := &PayloadSource{}
	for _, key := range sortedFieldKeys(obj) {
		switch key {
		case "whole":
			var whole bool
			if err := json.Unmarshal(nonNull(obj[key], "null"), &whole); err != nil || !whole {
				return nil, errf("config.envelope.payload.whole 只能是 true（省略即不选它）")
			}
			out.Whole = true
		case "pointer":
			var text string
			if err := json.Unmarshal(nonNull(obj[key], `""`), &text); err != nil {
				return nil, errf("config.envelope.payload.pointer 必须是字符串")
			}
			out.Pointer = strings.TrimSpace(text)
		default:
			return nil, errf("config.envelope.payload 不认字段 %q（允许：pointer / whole）", key)
		}
	}
	switch {
	case out.Whole && out.Pointer != "":
		return nil, errf("config.envelope.payload 的 whole 与 pointer 只能二选一")
	case !out.Whole && out.Pointer == "":
		return nil, errf("config.envelope.payload 要声明 whole: true 或 pointer（二选一）")
	}
	if out.Pointer != "" {
		if out.Pointer, err = ParsePointer(out.Pointer); err != nil {
			return nil, errf("config.envelope.payload：%v", err)
		}
	}
	return out, nil
}

// normalizeSignature 校验归一 config.signature（缺省 / null / 只剩默认口径 → nil）。显式
// `anotify-sha256` 折叠成缺省：否则语义相同却落库字节不同，安装比较会把同一份配置判成漂移。
func normalizeSignature(fields map[string]json.RawMessage) (*Signature, error) {
	raw, ok := fields["signature"]
	if !ok || isNull(raw) {
		return nil, nil
	}
	obj, err := decodeFields(raw)
	if err != nil {
		return nil, errf("config.signature 必须是 JSON 对象")
	}
	out := &Signature{}
	for _, key := range sortedFieldKeys(obj) {
		if key != "scheme" {
			return nil, errf("config.signature 不认字段 %q（允许：scheme）", key)
		}
		if err := json.Unmarshal(nonNull(obj[key], `""`), &out.Scheme); err != nil {
			return nil, errf("config.signature.scheme 必须是字符串")
		}
		out.Scheme = strings.TrimSpace(out.Scheme)
	}
	switch out.Scheme {
	case "", SchemeAnotifySHA256:
		return nil, nil
	case SchemeGitHubSHA256:
		return out, nil
	}
	return nil, errf("config.signature.scheme %q 不合法（可选：%s / %s）",
		out.Scheme, SchemeAnotifySHA256, SchemeGitHubSHA256)
}

// normalizeMatch 校验 targets[].match（空 types / 空串 / 重复都拒）。
func normalizeMatch(m *Match) error {
	if m == nil {
		return nil
	}
	if len(m.Types) == 0 {
		return errf("targets[].match 至少要声明 types（不写 match 才是全部命中）")
	}
	seen := map[string]bool{}
	for i, want := range m.Types {
		trimmed := strings.TrimSpace(want)
		if trimmed == "" {
			return errf("targets[].match.types[%d] 不能为空", i)
		}
		if seen[trimmed] {
			return errf("targets[].match.types[%d] 与前面重复（%q）", i, trimmed)
		}
		seen[trimmed] = true
		m.Types[i] = trimmed
	}
	return nil
}
