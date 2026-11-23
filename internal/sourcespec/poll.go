package sourcespec

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultPollTimeoutMs = 10000
	PollTimeoutMinMs     = 1000
	PollTimeoutMaxMs     = 120000
	DefaultPollMaxItems  = 100
	PollMaxItemsCap      = 500
	pollHeaderMaxCount   = 20
	pollSecretNameMax    = 64
)

// SecretRef 指向命名空间凭据：值不进 config，运行期按 (namespace, name) 取。
type SecretRef struct {
	Name   string `json:"name"`
	Header string `json:"header"`
	Prefix string `json:"prefix,omitempty"`
}

type PollSchedule struct {
	Expr string `json:"expr"`
	TZ   string `json:"tz,omitempty"`
}

// pollConfig 的 `secret` 故意不在输出里：它是 write-only 指令，落库目标是 namespace_secrets。
type pollConfig struct {
	URL           string            `json:"url"`
	Headers       map[string]string `json:"headers,omitempty"`
	SecretRef     *SecretRef        `json:"secretRef,omitempty"`
	Schedule      PollSchedule      `json:"schedule"`
	TimeoutMs     int               `json:"timeoutMs"`
	MaxItems      int               `json:"maxItems"`
	AllowInsecure bool              `json:"allowInsecure,omitempty"`
	Envelope      *Envelope         `json:"envelope,omitempty"`
	Payload       map[string]any    `json:"payload,omitempty"`
	PayloadSchema json.RawMessage   `json:"payloadSchema,omitempty"`
	Sample        map[string]any    `json:"sample,omitempty"`
}

func normalizePollConfig(fields map[string]json.RawMessage) (json.RawMessage, error) {
	out := pollConfig{}
	causes := []Cause{}
	relaxPollPayloadPointer(fields)
	add := func(field, message string) {
		causes = append(causes, Cause{Field: "config." + field, Reason: CauseReasonValueInvalid, Message: message})
	}

	if err := json.Unmarshal(nonNull(fields["allowInsecure"], "false"), &out.AllowInsecure); err != nil {
		add("allowInsecure", "必须是布尔")
	}
	rawURL, ok := fields["url"]
	if !ok || isNull(rawURL) {
		add("url", "必填（如 https://api.github.com/advisories）")
	} else {
		var text string
		if err := json.Unmarshal(rawURL, &text); err != nil {
			add("url", "必须是字符串")
		} else if norm, err := CheckPollURL(text, out.AllowInsecure); err != nil {
			add("url", err.Error())
		} else {
			out.URL = norm
		}
	}
	if raw, ok := fields["headers"]; ok && !isNull(raw) {
		var headers map[string]string
		if err := json.Unmarshal(raw, &headers); err != nil {
			add("headers", "必须是字符串到字符串的对象")
		} else if len(headers) > pollHeaderMaxCount {
			add("headers", fmt.Sprintf("最多 %d 项，得到 %d", pollHeaderMaxCount, len(headers)))
		} else {
			for _, key := range SortedKeys(anyMap(headers)) {
				if err := checkHeaderText(key, true); err != nil {
					add("headers", fmt.Sprintf("头名 %q 不合法：%v", key, err))
					continue
				}
				if err := checkHeaderText(headers[key], false); err != nil {
					add("headers", fmt.Sprintf("头 %q 的值不合法：%v", key, err))
				}
			}
			if len(headers) > 0 {
				out.Headers = headers
			}
		}
	}
	if raw, ok := fields["secretRef"]; ok && !isNull(raw) {
		if ref, err := normalizeSecretRef(raw); err != nil {
			add("secretRef", err.Error())
		} else {
			out.SecretRef = ref
		}
	}
	if raw, ok := fields["secret"]; ok && !isNull(raw) {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			add("secret", "必须是字符串")
		}
	}
	if raw, ok := fields["schedule"]; ok && !isNull(raw) {
		if sched, err := normalizePollSchedule(raw); err != nil {
			add("schedule", err.Error())
		} else {
			out.Schedule = sched
		}
	} else {
		add("schedule", "必填（如 {expr: \"*/5 * * * *\", tz: UTC}）")
	}
	out.TimeoutMs = DefaultPollTimeoutMs
	if raw, ok := fields["timeoutMs"]; ok && !isNull(raw) {
		var v int
		if err := json.Unmarshal(raw, &v); err != nil {
			add("timeoutMs", "必须是整数毫秒")
		} else if v < PollTimeoutMinMs || v > PollTimeoutMaxMs {
			add("timeoutMs", fmt.Sprintf("必须在 [%d, %d] 毫秒内，得到 %d", PollTimeoutMinMs, PollTimeoutMaxMs, v))
		} else {
			out.TimeoutMs = v
		}
	}
	out.MaxItems = DefaultPollMaxItems
	if raw, ok := fields["maxItems"]; ok && !isNull(raw) {
		var v int
		if err := json.Unmarshal(raw, &v); err != nil {
			add("maxItems", "必须是整数")
		} else if v < 1 || v > PollMaxItemsCap {
			add("maxItems", fmt.Sprintf("必须在 [1, %d] 内，得到 %d", PollMaxItemsCap, v))
		} else {
			out.MaxItems = v
		}
	}
	if env, err := normalizeEnvelope(fields, true); err != nil {
		add("envelope", strings.TrimPrefix(err.Error(), ErrInvalidConfig.Error()+": "))
	} else if env != nil {
		if env.Items == "" {
			env.Items = "."
		}
		for _, field := range []struct {
			name string
			src  *FieldSource
		}{{"type", env.Type}, {"deliveryId", env.DeliveryID}} {
			if field.src != nil && field.src.Header != "" {
				add("envelope", field.name+".header 只对 webhook 有意义（poll 没有请求头，请用 pointer）")
			}
		}
		out.Envelope = env
	}
	var err error
	if out.Payload, err = objectField(fields, "payload"); err != nil {
		return nil, err
	}
	if out.Sample, err = objectField(fields, "sample"); err != nil {
		return nil, err
	}
	if out.PayloadSchema, err = schemaField(fields); err != nil {
		return nil, err
	}
	if len(causes) > 0 {
		return nil, &CausesError{Causes: causes}
	}
	return marshal(out)
}

// relaxPollPayloadPointer 让 `payload: {pointer: ""}` 等价于 `{whole: true}`（空指针 = 整个 item）。
func relaxPollPayloadPointer(fields map[string]json.RawMessage) {
	raw, ok := fields["envelope"]
	if !ok || isNull(raw) {
		return
	}
	env, err := decodeFields(raw)
	if err != nil {
		return
	}
	payload, ok := env["payload"]
	if !ok || isNull(payload) {
		return
	}
	src, err := decodeFields(payload)
	if err != nil {
		return
	}
	pointer, ok := src["pointer"]
	if !ok {
		return
	}
	var text string
	if err := json.Unmarshal(pointer, &text); err != nil || strings.TrimSpace(text) != "" {
		return
	}
	src["whole"] = json.RawMessage("true")
	delete(src, "pointer")
	rebuilt, err := marshal(src)
	if err != nil {
		return
	}
	env["payload"] = rebuilt
	if outer, err := marshal(env); err == nil {
		fields["envelope"] = outer
	}
}

func anyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func checkHeaderText(text string, isName bool) error {
	if strings.ContainsAny(text, "\r\n") {
		return errors.New("不能含 CR/LF")
	}
	if !isName {
		return nil
	}
	if text == "" {
		return errors.New("不能为空")
	}
	for _, r := range text {
		if r <= ' ' || r == ':' || r >= 0x7f {
			return errors.New("只允许可见 ASCII，且不含空格与冒号")
		}
	}
	return nil
}

func ValidateSecretName(name string) error {
	if name == "" {
		return errors.New("name 必填（命名空间凭据的名字）")
	}
	if len(name) > pollSecretNameMax {
		return fmt.Errorf("name 最长 %d 字节", pollSecretNameMax)
	}
	for i, r := range name {
		ok := r == '.' || r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok || (i == 0 && (r == '.' || r == '_' || r == '-')) {
			return fmt.Errorf("name %q 只允许字母数字与 . _ -，且首字符为字母数字", name)
		}
	}
	return nil
}

func normalizeSecretRef(raw json.RawMessage) (*SecretRef, error) {
	obj, err := decodeFields(raw)
	if err != nil {
		return nil, errors.New("必须是 JSON 对象（{name, header, prefix}）")
	}
	out := &SecretRef{}
	for _, key := range sortedFieldKeys(obj) {
		var text string
		if err := json.Unmarshal(nonNull(obj[key], `""`), &text); err != nil {
			return nil, fmt.Errorf("%s 必须是字符串", key)
		}
		switch key {
		case "name":
			out.Name = strings.TrimSpace(text)
		case "header":
			out.Header = strings.TrimSpace(text)
		case "prefix":
			out.Prefix = text
		default:
			return nil, fmt.Errorf("不认字段 %q（允许：header / name / prefix）", key)
		}
	}
	if err := ValidateSecretName(out.Name); err != nil {
		return nil, err
	}
	if out.Header == "" {
		return nil, errors.New("header 必填（如 Authorization）")
	}
	if err := checkHeaderText(out.Header, true); err != nil {
		return nil, fmt.Errorf("header 不合法：%v", err)
	}
	if strings.ContainsAny(out.Prefix, "\r\n") {
		return nil, errors.New("prefix 不能含 CR/LF")
	}
	return out, nil
}

func normalizePollSchedule(raw json.RawMessage) (PollSchedule, error) {
	obj, err := decodeFields(raw)
	if err != nil {
		return PollSchedule{}, errors.New("必须是 JSON 对象（{expr, tz}）")
	}
	out := PollSchedule{}
	for _, key := range sortedFieldKeys(obj) {
		var text string
		if err := json.Unmarshal(nonNull(obj[key], `""`), &text); err != nil {
			return PollSchedule{}, fmt.Errorf("%s 必须是字符串", key)
		}
		switch key {
		case "expr":
			out.Expr = strings.TrimSpace(text)
		case "tz":
			out.TZ = strings.TrimSpace(text)
		default:
			return PollSchedule{}, fmt.Errorf("不认字段 %q（允许：expr / tz）", key)
		}
	}
	if out.Expr == "" {
		return PollSchedule{}, errors.New("expr 必填（五段表达式，如 */5 * * * *）")
	}
	if _, err := ParseCron(out.Expr); err != nil {
		return PollSchedule{}, err
	}
	if out.TZ == "" {
		out.TZ = "Local"
	}
	if out.TZ != "Local" {
		if _, err := time.LoadLocation(out.TZ); err != nil {
			return PollSchedule{}, fmt.Errorf("未知时区 %q", out.TZ)
		}
	}
	return out, nil
}

// NextPollTime 把 schedule 拼回 cron 形状后委托 NextCronTime —— 排期只有一份实现。
func NextPollTime(config json.RawMessage, after time.Time) (time.Time, error) {
	cfg := ParseConfig(KindPoll, config)
	if strings.TrimSpace(cfg.Schedule.Expr) == "" {
		return time.Time{}, errf("poll 需要 schedule.expr（五段表达式，如 */5 * * * *）")
	}
	raw, err := marshal(cronConfig{Expr: cfg.Schedule.Expr, TZ: cfg.Schedule.TZ})
	if err != nil {
		return time.Time{}, err
	}
	return NextCronTime(raw, after)
}

// ParsePointerOrRoot 把根写法（空 / `.` / `$` / `$.`）归一成 `"."`，其余走 ParsePointer。
func ParsePointerOrRoot(raw string) (string, error) {
	switch strings.TrimSpace(raw) {
	case "", ".", "$", "$.":
		return ".", nil
	}
	return ParsePointer(raw)
}

// LookupIn 是 walkPath 的 any 入口（item 可能不是对象）；LookupItems 在它之上要数组。
func LookupIn(doc any, path string) (any, bool) {
	if path == "." || path == "" {
		return doc, true
	}
	return walkPath(doc, strings.Split(path, "."))
}

func LookupItems(doc any, path string) ([]any, bool) {
	value, ok := LookupIn(doc, path)
	if !ok {
		return nil, false
	}
	items, ok := value.([]any)
	return items, ok
}

// CheckPollURL 是写入期 SSRF 判据：只允许 https（http 要 allowInsecure + 测试 host），
// 拒 userinfo、非公网 IP 字面量、localhost、.internal/.local。allowInsecure 跳过私网判据。
func CheckPollURL(raw string, allowInsecure bool) (string, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "", errors.New("不能为空")
	}
	u, err := url.Parse(text)
	if err != nil {
		return "", fmt.Errorf("无法解析：%v", err)
	}
	host := strings.ToLower(u.Hostname())
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !allowInsecure {
			return "", errors.New("只允许 https（http 需显式 allowInsecure: true 且 host 为 localhost / *.test）")
		}
		if !isTestHost(host) {
			return "", fmt.Errorf("http 只允许测试 host（localhost / *.test / 环回字面量），得到 %q", host)
		}
	default:
		return "", fmt.Errorf("只允许 https，得到 scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("必须是绝对地址（https://host/path）")
	}
	if u.User != nil {
		return "", errors.New("不能带 userinfo —— 凭据只走 config.secretRef")
	}
	if allowInsecure {
		return text, nil
	}
	if reason := blockedHostReason(host); reason != "" {
		return "", fmt.Errorf("%s %q 不是公网地址（SSRF 默认拒；测试请显式 allowInsecure: true）", reason, host)
	}
	return text, nil
}

func isTestHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost" || strings.HasSuffix(host, ".test")
}

func blockedHostReason(host string) string {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	switch {
	case h == "":
		return "空 host"
	case h == "localhost":
		return "环回地址"
	case strings.HasSuffix(h, ".internal"), strings.HasSuffix(h, ".local"):
		return "内部域名"
	}
	if ip := net.ParseIP(h); ip != nil {
		if kind := BlockedIPKind(ip); kind != "" {
			return kind + "地址"
		}
	}
	return ""
}

// BlockedIPKind 是非公网 IP 的唯一分类器：写入期与运行期（解析后 / dial 时）共用同一份判断。
func BlockedIPKind(ip net.IP) string {
	if ip == nil {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	switch {
	case ip.IsUnspecified():
		return "未指定"
	case ip.IsLoopback():
		return "环回"
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return "链路本地"
	case ip.IsMulticast():
		return "多播"
	case ip.IsPrivate():
		return "私网"
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return "运营商级 NAT"
	}
	return ""
}
