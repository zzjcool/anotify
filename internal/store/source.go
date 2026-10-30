package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// 源的种类（契约 §5.4）。新增种类时这里和 sourceKinds 一起改。
const (
	SourceKindCron    = "cron"    // 定时器：按五段表达式到点触发
	SourceKindWebhook = "webhook" // 通用 HTTP 回调：外部系统 POST 到唯一 URL
	SourceKindGitHub  = "github"  // GitHub webhook：形状已知，可给字段清单/一键配置
	SourceKindManual  = "manual"  // 手动：页面上点一下就跑
)

// SourceKinds 是全部合法种类（供校验与前端下拉复用）。
var SourceKinds = []string{SourceKindCron, SourceKindWebhook, SourceKindGitHub, SourceKindManual}

// IsValidSourceKind 判断种类是否合法。
func IsValidSourceKind(k string) bool {
	for _, v := range SourceKinds {
		if v == k {
			return true
		}
	}
	return false
}

// 源相关的哨兵错误。
var (
	ErrSourceNotFound      = errors.New("source not found")
	ErrSourceNameTaken     = errors.New("source name already used")
	ErrInvalidSourceKind   = errors.New("invalid source kind")
	ErrInvalidSourceConfig = errors.New("invalid source config")
)

// Source 是一个「源」实例：事件的入口。
//
// 它**不是能力**：能力由节点声明、中枢派任务给它；源自己产生事件、不干活。
// 这个区分是本表存在的理由（见 schema.sql 迁移 7 的注释）。
type Source struct {
	ID string `json:"id"`
	// NamespaceID 是隔离维度（I10/I11）：源属于某个命名空间。
	NamespaceID string          `json:"namespaceId"`
	OrgID       string          `json:"orgId"`
	Name        string          `json:"name"`
	Kind        string          `json:"kind"`
	Config      json.RawMessage `json:"config"`
	Enabled     bool            `json:"enabled"`

	// 调度元数据（只有 cron 有意义；其它 kind 为 nil）
	LastFiredAt *int64 `json:"lastFiredAt"`
	NextFireAt  *int64 `json:"nextFireAt"`

	CreatedAt int64 `json:"createdAt"`

	// Targets 是出口能力（多行 = 扇出），每个带自己的参数模板。
	Targets []SourceTarget `json:"targets"`
}

// SourceTarget 是一个出口：能力 + 该能力专属的参数模板。
//
// 为什么参数按出口分别存：每个能力的 params_schema 各不相同（字段名、
// 必填项都不同），一份公共参数必然对不上至少一边，触发时被剪枝却无从察觉。
type SourceTarget struct {
	CapabilityID string `json:"capabilityId"`
	// Params 是这个出口的默认参数（触发时按目标能力分别传给 Dispatcher）。
	// 空对象 = 不额外传参。
	Params map[string]any `json:"params,omitempty"`
}

// TargetIDs 返回出口能力 ID 列表（顺序与 Targets 一致）。
//
// 单独一个方法是因为"只要 ID"的地方很多（校验、派发 Targets），
// 而拿 ID 的调用方不该关心参数。
func (s *Source) TargetIDs() []string {
	out := make([]string, 0, len(s.Targets))
	for _, t := range s.Targets {
		out = append(out, t.CapabilityID)
	}
	return out
}

// TargetParamsMap 返回 capabilityID → params 的映射（派发时用）。
func (s *Source) TargetParamsMap() map[string]map[string]any {
	if len(s.Targets) == 0 {
		return nil
	}
	out := make(map[string]map[string]any, len(s.Targets))
	for _, t := range s.Targets {
		if len(t.Params) > 0 {
			out[t.CapabilityID] = t.Params
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// CronConfig 是 kind=cron 的配置。
//
// 只读这两个字段就够：expr 是标准五段表达式，tz 是 IANA 时区名
// （空 = Local）。**不做表达式求值以外的任何解释**——不变量 I6：
// 中枢不理解业务语义，更不能执行用户写的命令。
type CronConfig struct {
	Expr string `json:"expr"`
	TZ   string `json:"tz,omitempty"`
}

// WebhookConfig 是 kind=webhook 的配置。
//
// Token 用来拼出唯一 URL（/v1/hooks/<token>）。它本身是凭据：
// 谁拿到这个 URL 谁就能往里发事件，所以创建时随机生成、可轮换。
type WebhookConfig struct {
	Token string `json:"token"`

	// Event 只对 kind=github 有意义：这个源订阅的 GitHub 事件类型
	// （pull_request / push / issues / issue_comment / release / workflow_run …）。
	//
	// 它决定两件事：
	//   1. 前端给哪一份字段清单（各事件的 payload 结构完全不同）
	//   2. 收到投递时用哪个事件名（真实 GitHub 把它放在 X-GitHub-Event 头里）
	//
	// 为空表示"接受所有事件"，事件名以请求头为准。
	Event string `json:"event,omitempty"`
}

// 常用的 GitHub 事件类型。**不是白名单**：GitHub 会新增事件类型，
// 中枢不该拦（I6 只约束"推导量"，不约束外部世界的枚举）。这个列表
// 只用于前端下拉的默认项，未知值一律允许。
var GitHubEvents = []string{
	"pull_request",
	"push",
	"issues",
	"issue_comment",
	"pull_request_review",
	"release",
	"workflow_run",
}

// varNamePattern 是变量名的合法形态（配置期校验用）。
//
// 与参数模板占位符正则可取的字符集对齐（点号留给 payload 路径），所以变量名
// 里不许有点：`{{a.b}}` 才会稳定地被理解成「payload 路径」而不是「变量 a 的 b」。
var varNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// VarDef 是源声明的一个变量：给 payload 里的某个路径（或一个常量）取个短名字，
// 之后参数模板里就能写 {{name}} / {var:"name"}。
//
// 为什么存「名字 + 来源」而不是「名字 + 取值」：源拿到的事件 payload 每次投递
// 都不一样（webhook 的请求体本身就是 payload），变量只能在**触发时**求值。
// From 是 payload 的点号路径；Value/HasValue 是旧形态 {name,value} 的常量支持
// （HasValue 单独一个字段是因为常量允许是 null，nil 分不清「没写」和「写了 null」）。
type VarDef struct {
	Name, From string
	Value      any
	HasValue   bool
}

// UnmarshalJSON 认两种形态：{name,from}（引用 payload 路径）与 {name,value}（常量）。
//
// 为什么手写而不是给字段加 tag：`value` 可以合法地是 null，而 `Value == nil`
// 无法区分「没写 value」和「写了 value:null」——两者语义完全不同（后者是常量
// null，前者是没声明来源，配置期必须报错）。要知道键**是否存在**，只能下去读
// map[string]json.RawMessage。
func (d *VarDef) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return errors.New("变量项必须是对象")
	}
	if raw == nil { // `null` 解进 map 不报错，但一个键都没有
		return errors.New("变量项必须是对象")
	}
	*d = VarDef{}
	if v, ok := raw["name"]; ok {
		if err := json.Unmarshal(v, &d.Name); err != nil {
			return errors.New("变量名必须是字符串")
		}
	}
	if v, ok := raw["from"]; ok {
		if err := json.Unmarshal(v, &d.From); err != nil {
			return errors.New("变量的 from 必须是字符串")
		}
	}
	if v, ok := raw["value"]; ok {
		d.HasValue = true
		if err := json.Unmarshal(v, &d.Value); err != nil {
			return errors.New("变量的 value 无法解析")
		}
	}
	return nil
}

// VarDefs 解析源声明的变量（config.vars 数组）。
//
// 坏数据（空 config / 非法 JSON / vars 不是数组）返回 nil：读侧容忍，一条手改
// 坏的数据不该让整个源列表打不开（与 unmarshalTargetParams 同一口径）。触发时
// 取不到的变量会变成 param_missing 剪枝，该被看见的仍然会被看见。
func (s *Source) VarDefs() []VarDef {
	if s == nil || len(s.Config) == 0 {
		return nil
	}
	var cfg struct {
		Vars []VarDef `json:"vars"`
	}
	if err := json.Unmarshal(s.Config, &cfg); err != nil {
		return nil
	}
	if len(cfg.Vars) == 0 {
		return nil
	}
	return cfg.Vars
}

// VarNames 返回变量名列表（顺序与 config.vars 一致）。
//
// 只给「声明了哪些变量」：参数模板校验要判断 {var:"x"} 是否已声明，
// 它不需要（也不该）关心每个变量从哪取值。
func (s *Source) VarNames() []string {
	defs := s.VarDefs()
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

// ConfigPayload 返回 cron/manual 源用来给事件打底的 config.payload。
//
// 必须是 JSON 对象：数组/标量/null 一律 nil（读侧容忍；会保留它的 kind 在配置期已经
// 拒过一次，webhook/github 则根本不保留 payload）。
func (s *Source) ConfigPayload() map[string]any {
	if s == nil || len(s.Config) == 0 {
		return nil
	}
	var cfg struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(s.Config, &cfg); err != nil {
		return nil
	}
	m, ok := decodeJSONObject(cfg.Payload)
	if !ok {
		return nil
	}
	return m
}

// decodeJSONObject 把 raw 解成 JSON 对象；不存在/不是对象（含 null、数组、标量）→ false。
func decodeJSONObject(raw json.RawMessage) (map[string]any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, false
	}
	m, ok := probe.(map[string]any)
	if !ok {
		return nil, false
	}
	return m, true
}

// ValidateVarDefs 校验变量声明："" 表示合法，否则是给用户看的中文错误。
//
// 这些检查必须在**配置期**做完：变量名拼错、from 与 value 同时写、路径写成
// `a..b`，到触发时都只会变成「参数取不到」的剪枝——用户配完以为好了、实际
// 一次都不生效。配置期 400 是唯一能把问题当场说清楚的位置。
func ValidateVarDefs(defs []VarDef) string {
	seen := make(map[string]bool, len(defs))
	for _, d := range defs {
		// 名字按**原样**校验（不 trim 后再过正则）：` a ` 这种带空白名字能过
		// 形式校验，却在触发时永远匹配不上 {var:"a"}，属于静默失效。
		name := d.Name
		if strings.TrimSpace(name) == "" {
			return "每个变量都要有名字"
		}
		if !varNamePattern.MatchString(name) {
			return fmt.Sprintf("变量名「%s」不合法（只能是字母/数字/下划线，且不以数字开头）", name)
		}
		if seen[name] {
			return fmt.Sprintf("变量名「%s」重复", name)
		}
		seen[name] = true

		from := strings.TrimSpace(d.From)
		if from != "" && d.HasValue {
			return fmt.Sprintf("变量「%s」不能同时有 from 和 value", name)
		}
		if from == "" && !d.HasValue {
			return fmt.Sprintf("变量「%s」需要 from（payload 路径）", name)
		}
		if from == "" { // 常量：没有路径可查
			continue
		}
		// 只接受点号分段的 payload 路径：空段（`a..b`）与首尾的点都会让
		// GetPath 永远取不到值，是纯粹的输入错误，不该等到触发时才发现。
		if strings.Contains(from, "..") || strings.HasPrefix(from, ".") || strings.HasSuffix(from, ".") {
			return fmt.Sprintf("变量「%s」的 from 路径不合法：「%s」", name, from)
		}
	}
	return ""
}

// ParseCronConfig 解析并校验 cron 配置。
func ParseCronConfig(raw json.RawMessage) (CronConfig, error) {
	var c CronConfig
	if len(raw) == 0 {
		return c, fmt.Errorf("%w: empty cron config", ErrInvalidSourceConfig)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("%w: %v", ErrInvalidSourceConfig, err)
	}
	c.Expr = strings.TrimSpace(c.Expr)
	if c.Expr == "" {
		return c, fmt.Errorf("%w: cron 需要 expr（五段表达式，如 0 3 * * *）", ErrInvalidSourceConfig)
	}
	if c.TZ != "" {
		if _, err := time.LoadLocation(c.TZ); err != nil {
			return c, fmt.Errorf("%w: 未知时区 %q", ErrInvalidSourceConfig, c.TZ)
		}
	}
	return c, nil
}

// ValidateSourceConfig 按 kind 校验配置，顺带补默认值。
//
// kind 专属字段在这里生成/规范化，但 `vars`（变量声明，kind 无关）与
// cron/manual 的 `payload`（事件打底数据）必须**原样保留**：整份重写 config
// 会把用户写的这两个字段静默抹掉，等于变量/打底数据从来没存下来过，而且
// 没有任何报错可以追。所以做法是：先把 config 读成键值表，按 kind 规范出
// 专属字段，再把 vars/payload 原样放回。
func ValidateSourceConfig(kind string, raw json.RawMessage) (json.RawMessage, error) {
	if !IsValidSourceKind(kind) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidSourceKind, kind)
	}

	// 用户写的 config 先读成键值表。解析失败当空 map：token 等字段由服务端补，
	// 坏 JSON 不该在这里变成一个不带 ErrInvalidSourceConfig 的内部错误。
	fields := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &fields); err != nil {
			fields = map[string]json.RawMessage{}
		}
	}

	// vars 是 kind 无关的变量声明：逐项形式校验（错误消息带变量名，前端能
	// 直接指出是哪一项），通过后原样放回（不重新编码，保持用户写的形态）。
	if rawVars, ok := fields["vars"]; ok {
		var items []json.RawMessage
		if err := json.Unmarshal(rawVars, &items); err != nil {
			return nil, fmt.Errorf("%w: config.vars 必须是数组", ErrInvalidSourceConfig)
		}
		defs := make([]VarDef, 0, len(items))
		for _, item := range items {
			var d VarDef
			if err := json.Unmarshal(item, &d); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidSourceConfig, err)
			}
			defs = append(defs, d)
		}
		if msg := ValidateVarDefs(defs); msg != "" {
			return nil, fmt.Errorf("%w: %v", ErrInvalidSourceConfig, msg)
		}
	}

	// payload 只有在**会被保留**的 kind（cron/manual）上才有意义，所以也只有它们
	// 需要对象性校验：数组/标量/null 都不是「事件打底数据」，放进去只会在触发时
	// 变成一句莫名的报错，不如配置期直接拒。webhook/github 的 payload 直接丢弃
	// （投递体才是真正的 payload），连校验都不做——否则历史行里残留一个坏 payload，
	// 任何一次 PATCH 都会莫名 400。校验与保留必须共用同一个判据。
	if rawPayload, ok := fields["payload"]; ok && keepsConfigPayload(kind) {
		if _, isObj := decodeJSONObject(rawPayload); !isObj {
			return nil, fmt.Errorf("%w: config.payload 必须是 JSON 对象", ErrInvalidSourceConfig)
		}
	}

	out := map[string]json.RawMessage{}
	switch kind {
	case SourceKindCron:
		c, err := ParseCronConfig(raw)
		if err != nil {
			return nil, err
		}
		// 必须真的解析一遍表达式：只查"非空"会放过 `61 * * * *` 这种
		// 永远不会触发的配置，用户配完以为好了、实际一次都不跑。
		if _, err := ParseCron(c.Expr); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidSourceConfig, err)
		}
		if c.TZ == "" {
			c.TZ = "Local"
		}
		b, err := json.Marshal(c)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &out); err != nil {
			return nil, err
		}
	case SourceKindWebhook, SourceKindGitHub:
		// github 就是一个"知道载荷长什么样"的 webhook：投递路径、
		// token、签名处理完全相同，差别只在前端能给字段清单和一键配置。
		// 后端把两者当同一种东西校验，避免多一份会分叉的校验逻辑。
		var c WebhookConfig
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &c) // 忽略解析错误：token 由服务端生成
		}
		if c.Token == "" {
			t, err := NewSourceToken()
			if err != nil {
				return nil, err
			}
			c.Token = t
		}
		b, err := json.Marshal(c)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &out); err != nil {
			return nil, err
		}
	case SourceKindManual:
		// manual 没有 kind 专属字段，配置里只有 payload/vars（下面统一放回）。
	}

	// vars 各 kind 都保留；payload 只有 cron/manual 保留（webhook/github 的
	// payload 是投递进来的请求体，存在 config 里没有意义，也不该被当成打底数据）。
	if rawVars, ok := fields["vars"]; ok {
		out["vars"] = rawVars
	}
	if rawPayload, ok := fields["payload"]; ok && keepsConfigPayload(kind) {
		out["payload"] = rawPayload
	}
	return json.Marshal(out)
}

// keepsConfigPayload 报告该 kind 是否会把 config.payload **原样保留**下来。
//
// cron/manual 的 payload 是用户建源时写的「事件打底数据」，要存要读；webhook/github
// 的 payload 是投递进来的请求体，config 里留它没有意义、一律丢弃。校验（对象性）与
// 保留必须用这一个判据，否则会出现「校验了却丢掉」：历史 webhook 行里残留一个非对象
// payload 时，任何一次 PATCH 都会莫名 400。
func keepsConfigPayload(kind string) bool {
	return kind == SourceKindCron || kind == SourceKindManual
}

// NewSourceToken 生成 webhook 的路径 token：src_<base62(16B)>。
//
// 用随机串而不是自增 ID：URL 一旦泄漏就等于开放写入口，
// 必须不可枚举、且能通过"重新生成"立刻失效。
func NewSourceToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate source token: %w", err)
	}
	return "src_" + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

const sourceColumns = `id, namespace_id, org_id, name, kind, config, enabled, last_fired_at, next_fire_at, created_at`

func scanSource(r scanner) (*Source, error) {
	var (
		s       Source
		cfg     string
		enabled int
		last    sql.NullInt64
		next    sql.NullInt64
	)
	if err := r.Scan(&s.ID, &s.NamespaceID, &s.OrgID, &s.Name, &s.Kind, &cfg, &enabled,
		&last, &next, &s.CreatedAt); err != nil {
		return nil, err
	}
	s.Config = json.RawMessage(cfg)
	s.Enabled = enabled != 0
	if last.Valid {
		v := last.Int64
		s.LastFiredAt = &v
	}
	if next.Valid {
		v := next.Int64
		s.NextFireAt = &v
	}
	s.Targets = []SourceTarget{}
	return &s, nil
}

// CreateSource 建一个源，并写入它的出口能力。
//
// targets 至少一个：没有出口的源产生的事件永远派不出去，
// 那正是我们要消灭的"静默失败"。空列表直接拒绝。
func (d *DB) CreateSource(ctx context.Context, s *Source) error {
	if s == nil {
		return fmt.Errorf("create source: nil")
	}
	if s.NamespaceID == "" || s.NamespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("create source: empty name")
	}
	// 保留来源名（#142）：源名就是事件的 source（入口边左端 / POST /v1/events）。
	// 叫 route 的源产生的事件会被事件流过滤掉、在 trace 视图里被当成剪枝，
	// 所以要在**配置期**就报错，而不是等事件静默消失后再去猜。
	if strings.TrimSpace(s.Name) == "route" {
		return fmt.Errorf("create source: %w", ErrReservedEventSource)
	}
	if !IsValidSourceKind(s.Kind) {
		return fmt.Errorf("%w: %q", ErrInvalidSourceKind, s.Kind)
	}
	if len(s.Targets) == 0 {
		return fmt.Errorf("create source: 至少要选一个出口能力（否则事件永远派不出去）")
	}
	cfg, err := ValidateSourceConfig(s.Kind, s.Config)
	if err != nil {
		return err
	}
	if s.ID == "" {
		s.ID = NewSourceID()
	}
	if s.CreatedAt == 0 {
		s.CreatedAt = Now()
	}
	s.Config = cfg

	// org_id 从命名空间派生（I10：namespace.org_id == org_id 由构造保证）。
	orgID, err := d.NamespaceOrgID(ctx, s.NamespaceID)
	if err != nil {
		return fmt.Errorf("create source: %w", err)
	}
	s.OrgID = orgID

	// I11 第 ② 条：出口能力必须属于**同一个命名空间**。
	// 违反一律报错（400），不静默忽略——静默忽略会让人以为配置生效了。
	for _, t := range s.Targets {
		if err := d.checkCapabilityInNamespace(ctx, s.NamespaceID, t.CapabilityID, "source_targets"); err != nil {
			if errors.Is(err, ErrNotFound) {
				return fmt.Errorf("create source: 出口能力 %s 不存在", t.CapabilityID)
			}
			return fmt.Errorf("create source: %w", err)
		}
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create source: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// cron 的首次触发时刻在建源时就算好，否则要等到下次进程重启才调度
	var nextFire any
	if s.Kind == SourceKindCron {
		if t, err := NextCronTime(s.Config, time.Unix(s.CreatedAt, 0)); err == nil && !t.IsZero() {
			unix := t.Unix()
			nextFire = unix
			s.NextFireAt = &unix
		}
	}

	enabled := 0
	if s.Enabled {
		enabled = 1
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sources (`+sourceColumns+`)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		s.ID, s.NamespaceID, s.OrgID, s.Name, s.Kind, string(s.Config), enabled,
		nil, nextFire, s.CreatedAt); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: %q", ErrSourceNameTaken, s.Name)
		}
		return fmt.Errorf("create source: %w", err)
	}
	for _, t := range s.Targets {
		paramsJSON, err := marshalTargetParams(t.Params)
		if err != nil {
			return fmt.Errorf("create source target params: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO source_targets (source_id, capability_id, namespace_id, params, created_at)
			 VALUES (?,?,?,?,?)`,
			s.ID, t.CapabilityID, s.NamespaceID, paramsJSON, s.CreatedAt); err != nil {
			return fmt.Errorf("create source target: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create source: commit: %w", err)
	}
	return nil
}

// ListSources 列出**命名空间内**的全部源（含出口列表）。按创建时间倒序。
func (d *DB) ListSources(ctx context.Context, namespaceID string) ([]*Source, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.QueryContext(ctx,
		`SELECT `+sourceColumns+` FROM sources WHERE namespace_id = ? ORDER BY created_at DESC`, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()

	out := []*Source{}
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}

	// 出口单独取一次，避免 N+1（源数量不大，一次全查更简单）
	if len(out) == 0 {
		return out, nil
	}
	trows, err := d.QueryContext(ctx,
		`SELECT st.source_id, st.capability_id, st.params
		   FROM source_targets st
		  WHERE st.namespace_id = ?
		  ORDER BY st.capability_id`, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("list source targets: %w", err)
	}
	defer trows.Close()
	bySrc := map[string][]SourceTarget{}
	for trows.Next() {
		var sid, cid, pj string
		if err := trows.Scan(&sid, &cid, &pj); err != nil {
			return nil, fmt.Errorf("scan source target: %w", err)
		}
		bySrc[sid] = append(bySrc[sid], SourceTarget{
			CapabilityID: cid, Params: unmarshalTargetParams(pj),
		})
	}
	if err := trows.Err(); err != nil {
		return nil, fmt.Errorf("list source targets: %w", err)
	}
	for _, s := range out {
		if t, ok := bySrc[s.ID]; ok {
			s.Targets = t
		}
	}
	return out, nil
}

// GetSource 取单个源。
func (d *DB) GetSource(ctx context.Context, namespaceID, id string) (*Source, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	row := d.QueryRowContext(ctx,
		`SELECT `+sourceColumns+` FROM sources WHERE namespace_id = ? AND id = ?`, namespaceID, id)
	s, err := scanSource(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSourceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get source: %w", err)
	}
	rows, err := d.QueryContext(ctx,
		`SELECT capability_id, params FROM source_targets
		  WHERE source_id = ? ORDER BY capability_id`, id)
	if err != nil {
		return nil, fmt.Errorf("get source targets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, pj string
		if err := rows.Scan(&cid, &pj); err != nil {
			return nil, fmt.Errorf("scan source target: %w", err)
		}
		s.Targets = append(s.Targets, SourceTarget{
			CapabilityID: cid, Params: unmarshalTargetParams(pj),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get source targets: %w", err)
	}
	return s, nil
}

// GetSourceByWebhookToken 按 webhook token 找源（外部 POST 进来时用）。
//
// 返回 ErrSourceNotFound 也覆盖「找到了但已停用」：停用的源不该接受事件。
//
// kind 是 IN 而不是 =：webhook 和 github 共用同一套投递路径（后者只是
// "知道载荷长什么样"），这里若只认 webhook，github 源的回调地址永远 404。
func (d *DB) GetSourceByWebhookToken(ctx context.Context, token string) (*Source, error) {
	row := d.QueryRowContext(ctx,
		`SELECT `+sourceColumns+`
		   FROM sources
		  WHERE kind IN (?, ?) AND enabled = 1
		    AND json_extract(config, '$.token') = ?`,
		SourceKindWebhook, SourceKindGitHub, token)
	s, err := scanSource(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSourceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get source by token: %w", err)
	}
	return d.GetSource(ctx, s.NamespaceID, s.ID)
}

// UpdateSource 更新可改字段（name / config / enabled / targets）。
//
// 不可改 kind：换种类等于换语义（cron 表达式对 webhook 无意义），
// 让用户删了重建，代价很小、心智负担更小。
func (d *DB) UpdateSource(ctx context.Context, s *Source) error {
	if s == nil || s.ID == "" {
		return fmt.Errorf("update source: missing id")
	}
	if s.NamespaceID == "" || s.NamespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	orgID, err := d.NamespaceOrgID(ctx, s.NamespaceID)
	if err != nil {
		return fmt.Errorf("update source: %w", err)
	}
	s.OrgID = orgID
	cur, err := d.GetSource(ctx, s.NamespaceID, s.ID)
	if err != nil {
		return err
	}
	if s.Kind == "" {
		s.Kind = cur.Kind
	}
	if s.Kind != cur.Kind {
		return fmt.Errorf("update source: kind 不可修改（当前 %s）", cur.Kind)
	}
	// 改名同样要过保留名水闸：CreateSource 挡住的，UpdateSource 不能放进来
	// （否则「建完再改名」就能把源变成 route，它的事件会被事件流静默吞掉）。
	//
	// 关于空名：UpdateSource 无条件写 name（没有「空 = 不改名」语义，那在
	// handler 层）；唯一调用方（PATCH /v1/sources/{id}）总是从 GetSource
	// 带上当前名并只在 req.Name != "" 时覆盖，所以这里不会把空名误判成保留名。
	if strings.TrimSpace(s.Name) == "route" {
		return fmt.Errorf("update source: %w", ErrReservedEventSource)
	}
	if len(s.Targets) == 0 {
		return fmt.Errorf("update source: 至少要选一个出口能力")
	}
	cfg, err := ValidateSourceConfig(s.Kind, s.Config)
	if err != nil {
		return err
	}
	// I11 第 ② 条同样适用于更新路径（建完再改一样能引入跨命名空间引用）。
	for _, t := range s.Targets {
		if err := d.checkCapabilityInNamespace(ctx, s.NamespaceID, t.CapabilityID, "source_targets"); err != nil {
			if errors.Is(err, ErrNotFound) {
				return fmt.Errorf("update source: 出口能力 %s 不存在", t.CapabilityID)
			}
			return fmt.Errorf("update source: %w", err)
		}
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("update source: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	enabled := 0
	if s.Enabled {
		enabled = 1
	}
	// 重算下次触发：改了表达式或重新启用后，旧的时间点已经不适用
	var nextFire any
	if s.Kind == SourceKindCron && s.Enabled {
		if t, err := NextCronTime(cfg, time.Now()); err == nil && !t.IsZero() {
			nextFire = t.Unix()
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sources
		    SET name = ?, config = ?, enabled = ?, next_fire_at = ?
		  WHERE namespace_id = ? AND id = ?`,
		strings.TrimSpace(s.Name), string(cfg), enabled, nextFire, s.NamespaceID, s.ID); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: %q", ErrSourceNameTaken, s.Name)
		}
		return fmt.Errorf("update source: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM source_targets WHERE source_id = ?`, s.ID); err != nil {
		return fmt.Errorf("update source: clear targets: %w", err)
	}
	for _, t := range s.Targets {
		paramsJSON, err := marshalTargetParams(t.Params)
		if err != nil {
			return fmt.Errorf("update source: target params: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO source_targets (source_id, capability_id, namespace_id, params, created_at)
			 VALUES (?,?,?,?,?)`,
			s.ID, t.CapabilityID, s.NamespaceID, paramsJSON, Now()); err != nil {
			return fmt.Errorf("update source: insert target: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("update source: commit: %w", err)
	}
	return nil
}

// DeleteSource 删除源（source_targets 由外键级联删除）。
func (d *DB) DeleteSource(ctx context.Context, namespaceID, id string) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	res, err := d.ExecContext(ctx, `DELETE FROM sources WHERE namespace_id = ? AND id = ?`, namespaceID, id)
	if err != nil {
		return fmt.Errorf("delete source: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrSourceNotFound
	}
	return nil
}

// DueCronSources 取出「到点该触发」的 cron 源（跨组织，调度器用）。
//
// now 由调用方传入（便于测试注入）；只取 enabled=1 的。
func (d *DB) DueCronSources(ctx context.Context, now int64, limit int) ([]*Source, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.QueryContext(ctx,
		`SELECT `+sourceColumns+`
		   FROM sources
		  WHERE kind = ? AND enabled = 1
		    AND next_fire_at IS NOT NULL AND next_fire_at <= ?
		  ORDER BY next_fire_at
		  LIMIT ?`, SourceKindCron, now, limit)
	if err != nil {
		return nil, fmt.Errorf("due cron sources: %w", err)
	}
	defer rows.Close()
	out := []*Source{}
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, fmt.Errorf("scan due source: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("due cron sources: %w", err)
	}
	return out, nil
}

// MarkSourceFired 记录一次触发，并把下次触发时间推到后面。
//
// 用 nextFire 作为参数而不是"从现在算"：调度器可能延迟醒来，
// 从"上次计划时刻"往后推才不会漏掉或堆积。
func (d *DB) MarkSourceFired(ctx context.Context, id string, firedAt, nextFire int64) error {
	if _, err := d.ExecContext(ctx,
		`UPDATE sources SET last_fired_at = ?, next_fire_at = ? WHERE id = ?`,
		firedAt, nextFire, id); err != nil {
		return fmt.Errorf("mark source fired: %w", err)
	}
	return nil
}

// isUniqueViolation 判断是否唯一约束冲突（modernc.org/sqlite 的错误文本）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE constraint failed") || strings.Contains(s, "constraint failed: UNIQUE")
}

// marshalTargetParams 把参数序列化成 JSON（nil → "{}"）。
func marshalTargetParams(p map[string]any) (string, error) {
	if len(p) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("marshal params: %w", err)
	}
	return string(b), nil
}

// unmarshalTargetParams 反序列化参数（坏数据 → nil，不阻塞读取）。
//
// 坏数据不报错是有意的：一条来源不明的手改数据不该让整个源列表打不开。
// 触发时这些参数会走 params_schema 校验，该被拦的仍会被拦。
func unmarshalTargetParams(s string) map[string]any {
	if s == "" || s == "{}" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil || len(m) == 0 {
		return nil
	}
	return m
}
