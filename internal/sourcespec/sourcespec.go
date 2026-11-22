// Package sourcespec 是源（Source / 触发器）文档与配置的唯一纯校验源：同一套判据要在 store
// 写路径 / playbook 安装比较 / 拒绝理由展示三处用，抄两遍必然漂移。所以只做纯函数（不 import
// 本项目任何包、不碰 IO/环境/随机数），config 进出都是 `json.RawMessage`。语义基线是 `api/` 与测试。
package sourcespec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// 校验错误域。调用方（store / server）把它们翻译成 `invalid_source_kind` /
// `invalid_source_config`（source.md §6 的封闭错误码）。
var (
	// ErrInvalidKind 表示 kind 不在注册表里。
	ErrInvalidKind = errors.New("invalid source kind")
	// ErrInvalidConfig 表示 kind/config/targets 的形状不合法。
	ErrInvalidConfig = errors.New("invalid source config")
)

// 本期 kind 注册表的四个内置项。kind 是**开放集合**：合法值 = 注册表里有的项，判定一律走
// IsKind（查表），不写「不是这四个就拒」——那样开注册表时要逐处翻。
const (
	KindCron    = "cron"
	KindWebhook = "webhook"
	KindManual  = "manual"
	KindPoll    = "poll"
)

// kindRegistry 是本期注册表快照（只读）。
var kindRegistry = []string{KindCron, KindWebhook, KindManual, KindPoll}

// Kinds 返回合法 kind（拷贝；调用方可安全修改）。
func Kinds() []string {
	out := make([]string, len(kindRegistry))
	copy(out, kindRegistry)
	return out
}

// IsKind 报告 kind 是否在注册表里。
func IsKind(kind string) bool {
	for _, k := range kindRegistry {
		if k == kind {
			return true
		}
	}
	return false
}

// Target 是一条「事件送给哪个能力」的声明（source.md §4）。
//
// Capability 是**引用**：能力名（跟随 latest）或 `name@version`（钉死某版）。Params 是参数模板：常量、`$(payload.路径)` 引用，或两者混写（§4.1）。
type Target struct {
	Capability string         `json:"capability"`
	Params     map[string]any `json:"params,omitempty"`
	// Match 是条件派：不填 = 恒命中（现状）；写了任意一组（types / all / any）就按它判。
	Match *Match `json:"match,omitempty"`
	// Outcomes 是 router 的结局表（#522）：非空 = 只判断、只选一个结局名，平台按表执行。
	Outcomes map[string][]OutcomeAction `json:"outcomes,omitempty"`
}

// OutcomeAction 是 outcomes 表里的一条动作；capability 必须是 `spec.next` 里的下游。params 模板可写 `$(payload.路径)`（建任务时求值）与 `$(decision.路径)`（判决执行时求值）。
type OutcomeAction struct {
	Capability string         `json:"capability"`
	Params     map[string]any `json:"params,omitempty"`
}

// outcomes 表的两条硬上限，与 spec.next 的 32 条同一数量级：白名单要可审查。
const (
	// MaxOutcomes 是一个 target 的结局名上限。
	MaxOutcomes = 32
	// MaxOutcomeActions 是单个结局的动作上限。
	MaxOutcomeActions = 16
)

// Spec 是一份源文档的 spec 段（source.md §2）。
//
// Config 保持原始字节：本包只判形状与默认值，归一化后的字节由 NormalizeConfig 产出，
// 调用方拿到什么就存什么——中途重新编码会把用户写的 schema 改写成另一种字节等价形式。
type Spec struct {
	Kind    string          `json:"kind"`
	Enabled bool            `json:"enabled"`
	Config  json.RawMessage `json:"config"`
	Targets []Target        `json:"targets"`
}

// Config 是按 kind 解释后的 config（归一化形态）。
//
// 未在本 kind 上有意义的字段恒为零值：`token`/`secret`/`envelope`/`signature` 只有
// webhook 有，`expr`/`tz` 只有 cron 有（source.md §3.1/§3.2）。
type Config struct {
	// Expr / TZ 是 kind=cron 的调度参数（tz 空 = Local）。
	Expr string
	TZ   string
	// Token / Secret 是 webhook 的入口凭据（token 空 = 待服务端生成）。
	Token  string
	Secret string
	// Payload 是静态打底数据（三个 kind 同语义：入站一层覆盖）。
	Payload map[string]any
	// PayloadSchema 是载荷契约（缺省/null = 不校验）。
	PayloadSchema json.RawMessage
	// Sample 是编辑器提示用的示意载荷（不参与派发）。
	Sample map[string]any
	// Envelope / Signature 是 webhook 的适配层（#513）：从哪取三元组、用哪种签名口径。
	Envelope  *Envelope
	Signature *Signature
	// URL / Headers / SecretRef / Schedule / TimeoutMs / MaxItems / AllowInsecure 是
	// kind=poll 的拉取参数（#532），缺省已在此处补齐。
	URL           string
	Headers       map[string]string
	SecretRef     *SecretRef
	Schedule      PollSchedule
	TimeoutMs     int
	MaxItems      int
	AllowInsecure bool
}

// ParseConfig 把 config 字节按 kind 解释成结构化形态（坏字节 → 零值，不报错）。
//
// 它是**读侧**入口：库里一条手改坏的数据不该让整个源列表打不开。校验在 ValidateSpec / NormalizeConfig 里做（写侧）。
func ParseConfig(kind string, raw json.RawMessage) Config {
	var cfg Config
	if len(raw) == 0 {
		return cfg
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return cfg
	}
	if v, ok := fields["expr"]; ok {
		_ = json.Unmarshal(v, &cfg.Expr)
	}
	if v, ok := fields["tz"]; ok {
		_ = json.Unmarshal(v, &cfg.TZ)
	}
	if v, ok := fields["token"]; ok {
		_ = json.Unmarshal(v, &cfg.Token)
	}
	if v, ok := fields["secret"]; ok {
		_ = json.Unmarshal(v, &cfg.Secret)
	}
	if v, ok := fields["payloadSchema"]; ok && !isNull(v) {
		if _, isObj := decodeObject(v); isObj {
			cfg.PayloadSchema = append(json.RawMessage(nil), v...)
		}
	}
	if v, ok := fields["payload"]; ok {
		if obj, isObj := decodeObject(v); isObj {
			cfg.Payload = obj
		}
	}
	if v, ok := fields["sample"]; ok {
		if obj, isObj := decodeObject(v); isObj {
			cfg.Sample = obj
		}
	}
	if kind == KindWebhook {
		if env, err := normalizeEnvelope(fields, false); err == nil {
			cfg.Envelope = env
		}
		if sig, err := normalizeSignature(fields); err == nil {
			cfg.Signature = sig
		}
	}
	if kind == KindPoll {
		_ = json.Unmarshal(fields["url"], &cfg.URL)
		if v, ok := fields["headers"]; ok {
			_ = json.Unmarshal(v, &cfg.Headers)
		}
		if v, ok := fields["secretRef"]; ok {
			var ref SecretRef
			if json.Unmarshal(v, &ref) == nil && ref.Name != "" {
				cfg.SecretRef = &ref
			}
		}
		if v, ok := fields["schedule"]; ok {
			_ = json.Unmarshal(v, &cfg.Schedule)
		}
		_ = json.Unmarshal(fields["timeoutMs"], &cfg.TimeoutMs)
		_ = json.Unmarshal(fields["maxItems"], &cfg.MaxItems)
		_ = json.Unmarshal(fields["allowInsecure"], &cfg.AllowInsecure)
		if cfg.TimeoutMs <= 0 {
			cfg.TimeoutMs = DefaultPollTimeoutMs
		}
		if cfg.MaxItems <= 0 {
			cfg.MaxItems = DefaultPollMaxItems
		}
		if env, err := normalizeEnvelope(fields, true); err == nil {
			cfg.Envelope = env
		}
	}
	return cfg
}

// Scheme 返回该源的签名口径（缺省 = anotify-sha256）。调用方不自己解 config：「用哪种口径」只有一处解释，否则「验签看 A、文档写 B」这种分叉没法查。
func (c Config) Scheme() string {
	if c.Signature == nil || c.Signature.Scheme == "" {
		return SchemeAnotifySHA256
	}
	return c.Signature.Scheme
}

// isNull 判断 raw 是否就是 JSON 的 null（显式 null 与「没写」在配置期同义）。
func isNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// decodeObject 把 raw 解成 JSON 对象；不存在/不是对象（含 null、数组、标量）→ false。
func decodeObject(raw json.RawMessage) (map[string]any, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false
	}
	var probe any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&probe); err != nil {
		return nil, false
	}
	obj, ok := probe.(map[string]any)
	if !ok {
		return nil, false
	}
	return obj, true
}

// SortedKeys 返回 map 的键（升序）——遍历参数/map 时的确定性来源。
func SortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// errf 造一个带 ErrInvalidConfig 的、指向具体字段的错误。
func errf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, args...))
}

// Cause 是一条**带位置**的校验失败（服务端原样翻成 `Status.details.causes[]`）：Field 相对
// `spec`，Reason 复用状态码封闭词表（`unknown_field` / `value_invalid`），Message 给人看。
type Cause struct {
	Field   string
	Reason  string
	Message string
}

// cause 的两个短码（都是 `Status.reason` 已有的词，不新造）。
const (
	CauseReasonUnknownField = "unknown_field"
	CauseReasonValueInvalid = "value_invalid"
)

// CausesError 是一次写入期校验里**所有**问题的集合：`details.causes[].field` 让前端把红点
// 点到具体那行配置上（拼进 message 只能靠人眼找）。errors.Is(ErrInvalidConfig) 仍成立。
type CausesError struct{ Causes []Cause }

func (e *CausesError) Error() string {
	parts := make([]string, 0, len(e.Causes))
	for _, cause := range e.Causes {
		if cause.Field == "" {
			parts = append(parts, cause.Message)
			continue
		}
		parts = append(parts, cause.Field+"："+cause.Message)
	}
	return ErrInvalidConfig.Error() + ": " + strings.Join(parts, "；")
}

// Unwrap 让 errors.Is(err, ErrInvalidConfig) 成立（与 errf 同一错误域）。
func (e *CausesError) Unwrap() error { return ErrInvalidConfig }
