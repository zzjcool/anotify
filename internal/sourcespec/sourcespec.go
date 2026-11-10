// Package sourcespec 是 Anotify v4 **源（Source / 触发器）**文档与配置的唯一纯校验源。
//
// 为什么单独一个包：源的校验要在三个地方用同一套判据——store 的写路径（建/改源）、
// playbook 的 12 格安装比较（G1-5）、以及将来的拒绝理由展示。把它们抄两遍必然漂移
// （症状是「保存说没问题、安装说不一样」），所以本包只做**纯函数**：
//
//   - 不 import 任何本项目包（store/route/server 都不行）：校验规则不依赖数据库；
//   - 不碰 IO、不读环境、不生成随机数（token 的生成是 store 的事，本包只判「空 = 待生成」）；
//   - 输入的 config 一律是 `json.RawMessage`，输出也是——调用方拿到的字节就是落库的字节。
//
// 语义基线是 `docs/design/v4/source.md`（§1 kind 注册表、§2 文档形状、§3 config 字段、
// §4 targets 与模板语法、§6 校验与错误）。主文档与实现冲突时以主文档为准。
package sourcespec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// 校验错误域。调用方（store / server）把它们翻译成 `invalid_source_kind` /
// `invalid_source_config`（source.md §6 的封闭错误码）。
var (
	// ErrInvalidKind 表示 kind 不在注册表里。
	ErrInvalidKind = errors.New("invalid source kind")
	// ErrInvalidConfig 表示 kind/config/targets 的形状不合法。
	ErrInvalidConfig = errors.New("invalid source config")
)

// 本期 kind 注册表的三个内置项（source.md §1 / §7 D0）。
//
// kind 是**开放集合**：合法值 = 注册表里有的项。判定一律走 IsKind（查表），
// 不写「不是这三个就拒」——那样开注册表时要逐处翻。
const (
	KindCron    = "cron"
	KindWebhook = "webhook"
	KindManual  = "manual"
)

// kindRegistry 是本期注册表快照（只读）。
var kindRegistry = []string{KindCron, KindWebhook, KindManual}

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
// Capability 是**引用**：能力名（跟随 latest）或 `name@version`（钉死某版）。
// Params 是参数模板：常量、`$(payload.路径)` 引用，或两者混写（§4.1）。
type Target struct {
	Capability string         `json:"capability"`
	Params     map[string]any `json:"params,omitempty"`
}

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
// 未在本 kind 上有意义的字段恒为零值：`payload` 只有 cron/manual 保留，
// `token`/`secret` 只有 webhook 有（source.md §3.1/§3.2）。
type Config struct {
	// Expr / TZ 是 kind=cron 的调度参数（tz 空 = Local）。
	Expr string
	TZ   string
	// Token / Secret 是 kind=webhook 的入口凭据（token 空 = 待服务端生成）。
	Token  string
	Secret string
	// Payload 是 cron/manual 的静态打底数据（webhook 的载荷来自投递体，这里恒为 nil）。
	Payload map[string]any
	// PayloadSchema 是载荷契约（缺省/null = 不校验）。
	PayloadSchema json.RawMessage
	// Sample 是编辑器提示用的示意载荷（不参与派发）。
	Sample map[string]any
}

// ParseConfig 把 config 字节按 kind 解释成结构化形态（坏字节 → 零值，不报错）。
//
// 它是**读侧**入口：库里一条手改坏的数据不该让整个源列表打不开。
// 校验在 ValidateSpec / NormalizeConfig 里做（写侧）。
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
	if v, ok := fields["payload"]; ok && keepsPayload(kind) {
		if obj, isObj := decodeObject(v); isObj {
			cfg.Payload = obj
		}
	}
	if v, ok := fields["sample"]; ok {
		if obj, isObj := decodeObject(v); isObj {
			cfg.Sample = obj
		}
	}
	return cfg
}

// keepsPayload 报告该 kind 是否把 `config.payload` 当「静态打底数据」保留。
//
// cron/manual 的载荷由配置/人给，要存；webhook 的载荷是投递进来的请求体，
// 存在 config 里没有意义（source.md §3.2：给了也丢弃）。
func keepsPayload(kind string) bool { return kind == KindCron || kind == KindManual }

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
