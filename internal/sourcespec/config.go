package sourcespec

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
)

// knownFields 是各 kind 认得的 config 字段（source.md §3）。
//
// `payload` 在 webhook 上也「认识」，但语义是**丢弃**（§3.2：载荷来自投递体，
// 存到 config 里没有意义）——所以它不在这里，而是在 webhook 分支里被显式跳过。
// 未知字段一律拒：拼错的键（`payloadScheme`）静默保留只会让人以为契约生效了。
var knownFields = map[string]map[string]bool{
	KindCron:    {"expr": true, "tz": true, "payload": true, "payloadSchema": true, "sample": true},
	KindWebhook: {"token": true, "secret": true, "payload": true, "payloadSchema": true, "sample": true},
	KindManual:  {"payload": true, "payloadSchema": true, "sample": true},
}

// cronConfig 是归一化后的 kind=cron 配置形状（字段顺序即输出顺序）。
type cronConfig struct {
	Expr          string          `json:"expr"`
	TZ            string          `json:"tz"`
	Payload       map[string]any  `json:"payload,omitempty"`
	PayloadSchema json.RawMessage `json:"payloadSchema,omitempty"`
	Sample        map[string]any  `json:"sample,omitempty"`
}

// webhookConfig 是归一化后的 kind=webhook 配置形状。
type webhookConfig struct {
	Token         string          `json:"token"`
	Secret        string          `json:"secret,omitempty"`
	PayloadSchema json.RawMessage `json:"payloadSchema,omitempty"`
	Sample        map[string]any  `json:"sample,omitempty"`
}

// manualConfig 是归一化后的 kind=manual 配置形状。
type manualConfig struct {
	Payload       map[string]any  `json:"payload,omitempty"`
	PayloadSchema json.RawMessage `json:"payloadSchema,omitempty"`
	Sample        map[string]any  `json:"sample,omitempty"`
}

// NormalizeConfig 归一化 config：只保留该 kind 认得的字段、补默认值、丢弃无意义字段。
//
// 归一化的意义是「库里存的就是规范形态」：创建与 PATCH 都过它，于是
//   - 同语义的两份 config（缺省 tz vs tz: Local）落库字节一致，比较不会假阴性；
//   - 坏形状（数组/标量/未知字段/坏 cron 表达式/payload 不是对象）在写路径当场拒。
//
// **不做**的事：不生成 token（空 token 原样保留，由 store 在一次性的创建路径上生成）、
// 不校验 targets（那是 ValidateSpec / ValidateTargets 的事）。
func NormalizeConfig(kind string, raw json.RawMessage) (json.RawMessage, error) {
	if !IsKind(kind) {
		return nil, errKind(kind)
	}
	fields, err := decodeFields(raw)
	if err != nil {
		return nil, err
	}
	for _, key := range sortedFieldKeys(fields) {
		if knownFields[kind][key] {
			continue
		}
		return nil, errf("%s 不认字段 %q（允许：%v）", kind, key, sortedKnown(kind))
	}

	switch kind {
	case KindCron:
		out := cronConfig{}
		if err := json.Unmarshal(nonNull(fields["expr"], "null"), &out.Expr); err != nil {
			return nil, errf("config.expr 必须是字符串")
		}
		out.Expr = strings.TrimSpace(out.Expr)
		if out.Expr == "" {
			return nil, errf("cron 需要 expr（五段表达式，如 0 3 * * *）")
		}
		// 创建时**真的解析一遍**：只查非空会放过 `61 * * * *` 这种永不触发的配置。
		if _, err := ParseCron(out.Expr); err != nil {
			return nil, errf("%v", err)
		}
		if err := json.Unmarshal(nonNull(fields["tz"], `""`), &out.TZ); err != nil {
			return nil, errf("config.tz 必须是字符串")
		}
		out.TZ = strings.TrimSpace(out.TZ)
		if out.TZ == "" {
			out.TZ = "Local"
		}
		if out.TZ != "Local" {
			if _, err := time.LoadLocation(out.TZ); err != nil {
				return nil, errf("未知时区 %q", out.TZ)
			}
		}
		if out.Payload, err = objectField(fields, "payload"); err != nil {
			return nil, err
		}
		if out.Sample, err = objectField(fields, "sample"); err != nil {
			return nil, err
		}
		if out.PayloadSchema, err = schemaField(fields); err != nil {
			return nil, err
		}
		return marshal(out)

	case KindWebhook:
		out := webhookConfig{}
		if err := json.Unmarshal(nonNull(fields["token"], `""`), &out.Token); err != nil {
			return nil, errf("config.token 必须是字符串")
		}
		if err := json.Unmarshal(nonNull(fields["secret"], `""`), &out.Secret); err != nil {
			return nil, errf("config.secret 必须是字符串")
		}
		if out.Sample, err = objectField(fields, "sample"); err != nil {
			return nil, err
		}
		if out.PayloadSchema, err = schemaField(fields); err != nil {
			return nil, err
		}
		// payload 被丢弃（不报错）：它是投递体的事，存进 config 只会让人以为生效了。
		return marshal(out)

	case KindManual:
		out := manualConfig{}
		if out.Payload, err = objectField(fields, "payload"); err != nil {
			return nil, err
		}
		if out.Sample, err = objectField(fields, "sample"); err != nil {
			return nil, err
		}
		if out.PayloadSchema, err = schemaField(fields); err != nil {
			return nil, err
		}
		return marshal(out)
	}
	// IsKind 已经挡住了不可能的分支；留一句明确的错误而不是 nil,nil。
	return nil, errKind(kind)
}

// MergeConfig 把 PATCH 语义应用到已有 config 上：**凭证字段「省略 = 保留已有」**。
//
// 为什么只有 token/secret 有这个例外：GET 永远不回显它们的原文（source.md §3.1：
// 只给 hookUrl / secretSet），所以客户端在 PATCH 里根本拿不到旧值可发。若按
// 「省略 = 清空」处理，任何一次「只改 payloadSchema」的 PATCH 都会顺手把 webhook
// 的 URL 换掉、把签名失效——那是数据丢失，不是编辑。
//
// 其它字段（payload / payloadSchema / sample）没有这个例外：读接口会原样回显它们，
// 客户端有能力重发；省略即清空/缺省。
func MergeConfig(kind string, existing, patch json.RawMessage) (json.RawMessage, error) {
	if !IsKind(kind) {
		return nil, errKind(kind)
	}
	if kind != KindWebhook {
		return NormalizeConfig(kind, patch)
	}
	fields, err := decodeFields(patch)
	if err != nil {
		return nil, err
	}
	cur, err := decodeFields(existing)
	if err != nil {
		// 老行坏掉不该让一次 PATCH 永远失败：当作空 config，由下面按「缺 = 生成」处理。
		cur = map[string]json.RawMessage{}
	}
	for _, key := range []string{"token", "secret"} {
		if _, ok := fields[key]; ok {
			continue
		}
		if v, ok := cur[key]; ok {
			fields[key] = v
		}
	}
	merged, err := marshal(fields)
	if err != nil {
		return nil, err
	}
	return NormalizeConfig(kind, merged)
}

// ValidateSpec 校验一份源 spec（kind / config / targets）。
//
// forWrite 区分两个时机的判据：
//   - true：创建/更新。webhook 的 token **可以**为空（服务端随后生成），
//     cron 的表达式必须当场解析得过，targets 的模板语法必须成立。
//   - false：读/比对。webhook 的 token 必须已经存在（一个没有 token 的 webhook
//     源在外面对不上任何 URL），其余与写路径一致。
//
// **空 targets 是合法的**（source.md §5：先把入口建起来、再接能力是正常顺序，
// 触发时留一条 no_target 剪枝痕），这里不判它。
func ValidateSpec(spec Spec, forWrite bool) error {
	if !IsKind(spec.Kind) {
		return errKind(spec.Kind)
	}
	if _, err := NormalizeConfig(spec.Kind, spec.Config); err != nil {
		return err
	}
	if !forWrite && spec.Kind == KindWebhook {
		if ParseConfig(spec.Kind, spec.Config).Token == "" {
			return errf("webhook 源必须有 token（空 = 建源时由服务端生成）")
		}
	}
	return ValidateTargets(spec.Targets)
}

// ValidateTargets 校验出口声明：引用非空、参数模板语法成立。
//
// 模板只校**语法**（`$(payload.a.b)` 的形状、被禁的根名），不校「这个路径取不取得到」
// ——那需要一条真实载荷，属于运行期（取不到 → param_missing 剪枝，source.md §4.1）。
func ValidateTargets(targets []Target) error {
	for i, t := range targets {
		if strings.TrimSpace(t.Capability) == "" {
			return errf("targets[%d].capability 不能为空", i)
		}
		if err := ValidateParamsTemplate(t.Params); err != nil {
			return errf("targets[%d]（%s）：%v", i, t.Capability, err)
		}
	}
	return nil
}

// MaskConfig 返回用于回显的 config：token / secret 脱敏。
//
// 保留后 4 位（`***ab12`）而不是全 `***`：运维要能分辨「我正在轮换的是哪一把」，
// 后 4 位不足以还原凭据。长度 ≤ 4 的串没有「安全的后 4 位」，整体打码。
//
// 注意这只是 store 层的**兜底**：source.md §3.1 规定 GET 给 `hookUrl` + `secretSet`
// 两个投影字段，handler 层（G3）按那条实现，本函数保证「万一把 config 原样回显了」
// 也不会漏凭据。
func MaskConfig(kind string, raw json.RawMessage) json.RawMessage {
	fields, err := decodeFields(raw)
	if err != nil {
		// 坏 config 也不该把原文字节漏出去：整个打码（调用方拿到的至少是合法 JSON）。
		return json.RawMessage(`{}`)
	}
	for _, key := range []string{"token", "secret"} {
		v, ok := fields[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			continue
		}
		masked, _ := marshal(maskSecret(s))
		fields[key] = masked
	}
	out, err := marshal(fields)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return out
}

// maskSecret 保留后 4 位，其余打码。
func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 4 {
		return "***"
	}
	return "***" + s[len(s)-4:]
}

// SameConfig 比较两份 config 是否**语义相同**（归一化后逐字节比）。
//
// 归一化先行：`{expr: X}` 与 `{expr: X, tz: "Local"}` 是同一份配置，
// 不归一会让安装包在「复用已有源」那一格上误判成「内容不同」。
func SameConfig(kind string, a, b json.RawMessage) bool {
	na, err := NormalizeConfig(kind, a)
	if err != nil {
		return false
	}
	nb, err := NormalizeConfig(kind, b)
	if err != nil {
		return false
	}
	return canonicalEqual(na, nb)
}

// SameTargets 比较两份 targets 是否语义相同。
//
// 顺序无关：扇出的求值顺序不是语义（每条 target 独立求值、独立剪枝，source.md §4.2），
// 所以 `[a,b]` 与 `[b,a]` 是同一份声明。参数按 canonical JSON 比（键序无关）。
func SameTargets(a, b []Target) bool {
	ka, ok := targetKeySet(a)
	if !ok {
		return false
	}
	kb, ok := targetKeySet(b)
	if !ok {
		return false
	}
	if len(ka) != len(kb) {
		return false
	}
	for i := range ka {
		if ka[i] != kb[i] {
			return false
		}
	}
	return true
}

// targetKeySet 把 targets 归一成**有序**的规范化键列表（用于集合比较）。
func targetKeySet(targets []Target) ([]string, bool) {
	keys := make([]string, 0, len(targets))
	for _, t := range targets {
		params, err := marshalCanonical(t.Params)
		if err != nil {
			return nil, false
		}
		keys = append(keys, strings.TrimSpace(t.Capability)+"\x00"+string(params))
	}
	sort.Strings(keys)
	return keys, true
}

// ---------- 内部工具 ----------

// errKind 造一个 kind 不合法错误（消息里带可选值清单，source.md §6）。
func errKind(kind string) error {
	return &kindError{kind: kind}
}

// kindError 带可选值清单（handler 可直接把它写进 400 的 message）。
type kindError struct{ kind string }

func (e *kindError) Error() string {
	return "invalid source kind: " + strconv.Quote(e.kind) + "（可选：" + strings.Join(Kinds(), " / ") + "）"
}

// Unwrap 让 errors.Is(err, ErrInvalidKind) 成立。
func (e *kindError) Unwrap() error { return ErrInvalidKind }

// sortedFieldKeys 返回 config 键表的键（升序）——错误消息与遍历的确定性来源。
func sortedFieldKeys(fields map[string]json.RawMessage) []string {
	out := make([]string, 0, len(fields))
	for k := range fields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedKnown 返回某 kind 认得的字段（升序，错误消息用）。
func sortedKnown(kind string) []string {
	out := make([]string, 0, len(knownFields[kind]))
	for k := range knownFields[kind] {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// decodeFields 把 config 解成键值表；空 / null → 空表，非对象 → 错误。
func decodeFields(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || isNull(raw) {
		return map[string]json.RawMessage{}, nil
	}
	var fields map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&fields); err != nil || fields == nil {
		return nil, errf("config 必须是 JSON 对象")
	}
	return fields, nil
}

// objectField 取一个「必须是 JSON 对象」的字段（缺省 / null / 空 → nil）。
func objectField(fields map[string]json.RawMessage, key string) (map[string]any, error) {
	raw, ok := fields[key]
	if !ok || isNull(raw) {
		return nil, nil
	}
	obj, isObj := decodeObject(raw)
	if !isObj {
		return nil, errf("config.%s 必须是 JSON 对象", key)
	}
	return obj, nil
}

// schemaField 取 payloadSchema（必须是 JSON 对象；缺省 / null → nil）。
//
// 显式 null 合法，含义是「没有契约」；数组/标量/字符串一律拒——那种值到投递时
// 只会变成一条莫名的 event_schema 剪枝，不如配置期说清楚。
func schemaField(fields map[string]json.RawMessage) (json.RawMessage, error) {
	raw, ok := fields["payloadSchema"]
	if !ok || isNull(raw) {
		return nil, nil
	}
	if _, isObj := decodeObject(raw); !isObj {
		return nil, errf(`config.payloadSchema 必须是 JSON 对象（如 {"type":"object","required":["environment"]}）`)
	}
	return append(json.RawMessage(nil), raw...), nil
}

// nonNull 返回 raw，或在缺省时返回 fallback 字节（json.Unmarshal 的默认值来源）。
func nonNull(raw json.RawMessage, fallback string) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 || isNull(raw) {
		return json.RawMessage(fallback)
	}
	return raw
}
