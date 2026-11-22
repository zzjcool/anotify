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

// kind=poll（#532）的拉取参数。这里只有**形状**与安全判据：真正发请求的在 internal/poll，
// 本包仍然纯函数、不碰 IO（DNS 也在运行期，写入期只判字面量）。
const (
	DefaultPollTimeoutMs = 10000
	PollTimeoutMinMs     = 1000
	PollTimeoutMaxMs     = 120000
	DefaultPollMaxItems  = 100
	PollMaxItemsCap      = 500
	pollHeaderMaxCount   = 20
	pollSecretNameMax    = 64
)

// SecretRef 指向命名空间凭据：值不进 config，只在运行期按 (namespace, name) 取。
type SecretRef struct {
	Name   string `json:"name"`
	Header string `json:"header"`
	Prefix string `json:"prefix,omitempty"`
}

// PollSchedule 是 poll 的排期（表达式与时区与 cron 同一套）。
type PollSchedule struct {
	Expr string `json:"expr"`
	TZ   string `json:"tz,omitempty"`
}

// pollConfig 是归一化后的 kind=poll 形状（字段顺序即落库字节顺序）。`secret` 故意不在
// 这里：它是 write-only 的写入指令，落库目标是 namespace_secrets（见 store）。
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

// normalizePollConfig 校验并归一 kind=poll 的 config。字段级问题**一次性全报**（带
// `config.<键>` 路径），因为写路径把 causes 直接翻成 details.causes[]。
func normalizePollConfig(fields map[string]json.RawMessage) (json.RawMessage, error) {
	out := pollConfig{}
	causes := []Cause{}
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

func anyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// checkHeaderText 拒 CR/LF（头注入）；头名额外要求无空白与冒号。
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
	if out.Name == "" {
		return nil, errors.New("name 必填（命名空间凭据的名字）")
	}
	if len(out.Name) > pollSecretNameMax {
		return nil, fmt.Errorf("name 最长 %d 字节", pollSecretNameMax)
	}
	for i, r := range out.Name {
		ok := r == '.' || r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok || (i == 0 && (r == '.' || r == '_' || r == '-')) {
			return nil, fmt.Errorf("name %q 只允许字母数字与 . _ -，且首字符为字母数字", out.Name)
		}
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

// NextPollTime 是 poll 的下次触发时刻：把 schedule 拼回 cron 形状后**委托** NextCronTime，
// 排期只留一份实现。
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

// ParsePointerOrRoot 是 items 指针的方言：根写法（空 / `.` / `$` / `$.`）归一成 `"."`，
// 其余与 #515 的指针逐字同一套（共用 ParsePointer）。
func ParsePointerOrRoot(raw string) (string, error) {
	switch strings.TrimSpace(raw) {
	case "", ".", "$", "$.":
		return ".", nil
	}
	return ParsePointer(raw)
}

// LookupItems 在响应文档里取事件数组：`"."` = 根即数组，否则走同一份 walkPath。
// 取不到或不是数组 → false（调用方记该轮失败，不当成「0 条」）。
func LookupItems(doc any, path string) ([]any, bool) {
	if path == "." || path == "" {
		items, ok := doc.([]any)
		return items, ok
	}
	value, ok := walkPath(doc, strings.Split(path, "."))
	if !ok {
		return nil, false
	}
	items, ok := value.([]any)
	return items, ok
}

// CheckPollURL 是 poll 的**写入期** SSRF 判据：只允许 https（http 要 allowInsecure 且 host 是
// localhost/*.test），拒 userinfo、IP 字面量落在非公网段、localhost、.internal/.local。
// allowInsecure = 显式逃生口（e2e/测试）：跳过私网判据，仅此一处放宽。
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

// isTestHost 是 allowInsecure 下允许走明文 http 的 host：本机名、*.test、环回字面量。
func isTestHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost" || strings.HasSuffix(host, ".test")
}

// blockedHostReason 判定 host 名/字面量是否落在非公网（写入期，不解析 DNS）。
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

// BlockedIPKind 返回非公网 IP 的类别（"" = 可放行）。运行期（解析后、dial 时）复用它，
// 免得两处各写一套网段判断。
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
