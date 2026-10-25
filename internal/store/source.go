package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 源的种类（契约 §5.4）。新增种类时这里和 sourceKinds 一起改。
const (
	SourceKindCron    = "cron"    // 定时器：按五段表达式到点触发
	SourceKindWebhook = "webhook" // HTTP 回调：外部系统 POST 到唯一 URL
	SourceKindManual  = "manual"  // 手动：页面上点一下就跑
)

// SourceKinds 是全部合法种类（供校验与前端下拉复用）。
var SourceKinds = []string{SourceKindCron, SourceKindWebhook, SourceKindManual}

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
	ID      string          `json:"id"`
	OrgID   string          `json:"orgId"`
	Name    string          `json:"name"`
	Kind    string          `json:"kind"`
	Config  json.RawMessage `json:"config"`
	Enabled bool            `json:"enabled"`

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
func ValidateSourceConfig(kind string, raw json.RawMessage) (json.RawMessage, error) {
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
		return b, nil
	case SourceKindWebhook:
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
		return json.Marshal(c)
	case SourceKindManual:
		return json.RawMessage(`{}`), nil
	}
	return nil, fmt.Errorf("%w: %q", ErrInvalidSourceKind, kind)
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

const sourceColumns = `id, org_id, name, kind, config, enabled, last_fired_at, next_fire_at, created_at`

func scanSource(r scanner) (*Source, error) {
	var (
		s       Source
		cfg     string
		enabled int
		last    sql.NullInt64
		next    sql.NullInt64
	)
	if err := r.Scan(&s.ID, &s.OrgID, &s.Name, &s.Kind, &cfg, &enabled,
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
	if s.OrgID == "" {
		return fmt.Errorf("create source: empty org_id")
	}
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("create source: empty name")
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

	// 出口能力必须都存在且同组织（不变量 I10：一切过 org_id）
	for _, t := range s.Targets {
		if _, err := d.GetCapabilityByID(ctx, s.OrgID, t.CapabilityID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return fmt.Errorf("create source: 出口能力 %s 不存在", t.CapabilityID)
			}
			return fmt.Errorf("create source: 查能力 %s: %w", t.CapabilityID, err)
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
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		s.ID, s.OrgID, s.Name, s.Kind, string(s.Config), enabled,
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
			`INSERT INTO source_targets (source_id, capability_id, params, created_at) VALUES (?,?,?,?)`,
			s.ID, t.CapabilityID, paramsJSON, s.CreatedAt); err != nil {
			return fmt.Errorf("create source target: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create source: commit: %w", err)
	}
	return nil
}

// ListSources 列出组织的全部源（含出口列表）。按创建时间倒序。
func (d *DB) ListSources(ctx context.Context, orgID string) ([]*Source, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT `+sourceColumns+` FROM sources WHERE org_id = ? ORDER BY created_at DESC`, orgID)
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
		   JOIN sources s ON s.id = st.source_id
		  WHERE s.org_id = ?
		  ORDER BY st.capability_id`, orgID)
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
func (d *DB) GetSource(ctx context.Context, orgID, id string) (*Source, error) {
	row := d.QueryRowContext(ctx,
		`SELECT `+sourceColumns+` FROM sources WHERE org_id = ? AND id = ?`, orgID, id)
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
func (d *DB) GetSourceByWebhookToken(ctx context.Context, token string) (*Source, error) {
	row := d.QueryRowContext(ctx,
		`SELECT `+sourceColumns+`
		   FROM sources
		  WHERE kind = ? AND enabled = 1
		    AND json_extract(config, '$.token') = ?`,
		SourceKindWebhook, token)
	s, err := scanSource(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSourceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get source by token: %w", err)
	}
	return d.GetSource(ctx, s.OrgID, s.ID)
}

// UpdateSource 更新可改字段（name / config / enabled / targets）。
//
// 不可改 kind：换种类等于换语义（cron 表达式对 webhook 无意义），
// 让用户删了重建，代价很小、心智负担更小。
func (d *DB) UpdateSource(ctx context.Context, s *Source) error {
	if s == nil || s.ID == "" || s.OrgID == "" {
		return fmt.Errorf("update source: missing id/org_id")
	}
	cur, err := d.GetSource(ctx, s.OrgID, s.ID)
	if err != nil {
		return err
	}
	if s.Kind == "" {
		s.Kind = cur.Kind
	}
	if s.Kind != cur.Kind {
		return fmt.Errorf("update source: kind 不可修改（当前 %s）", cur.Kind)
	}
	if len(s.Targets) == 0 {
		return fmt.Errorf("update source: 至少要选一个出口能力")
	}
	cfg, err := ValidateSourceConfig(s.Kind, s.Config)
	if err != nil {
		return err
	}
	for _, t := range s.Targets {
		if _, err := d.GetCapabilityByID(ctx, s.OrgID, t.CapabilityID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return fmt.Errorf("update source: 出口能力 %s 不存在", t.CapabilityID)
			}
			return err
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
		  WHERE org_id = ? AND id = ?`,
		strings.TrimSpace(s.Name), string(cfg), enabled, nextFire, s.OrgID, s.ID); err != nil {
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
			`INSERT INTO source_targets (source_id, capability_id, params, created_at) VALUES (?,?,?,?)`,
			s.ID, t.CapabilityID, paramsJSON, Now()); err != nil {
			return fmt.Errorf("update source: insert target: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("update source: commit: %w", err)
	}
	return nil
}

// DeleteSource 删除源（source_targets 由外键级联删除）。
func (d *DB) DeleteSource(ctx context.Context, orgID, id string) error {
	res, err := d.ExecContext(ctx, `DELETE FROM sources WHERE org_id = ? AND id = ?`, orgID, id)
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
