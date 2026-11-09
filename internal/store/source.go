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

	"github.com/zzjcool/anotify/internal/sourcespec"
)

// 源的种类（source.md §1）：注册表里有什么就是什么，本期只有三个内置项。
//
// kind 是开放集合，判定一律走 sourcespec.IsKind（查表），不写「不是这三个就拒」。
const (
	SourceKindCron    = sourcespec.KindCron
	SourceKindWebhook = sourcespec.KindWebhook
	SourceKindManual  = sourcespec.KindManual
)

// SourceKinds 返回全部合法种类（拷贝；供校验与前端下拉复用）。
func SourceKinds() []string { return sourcespec.Kinds() }

// IsValidSourceKind 判断种类是否合法。
func IsValidSourceKind(k string) bool { return sourcespec.IsKind(k) }

// 源相关的哨兵错误。
//
// 前两个别名到 sourcespec 的同名错误：校验只有一份实现，`errors.Is` 在 store 与
// sourcespec 两侧都能命中（避免 handler 为了判错误而 import 两个包）。
var (
	ErrInvalidSourceKind   = sourcespec.ErrInvalidKind
	ErrInvalidSourceConfig = sourcespec.ErrInvalidConfig
)

var (
	// ErrSourceNotFound 表示源不存在（也覆盖「hook token 找不到 / 源已停用」，不区分）。
	ErrSourceNotFound = errors.New("source not found")
	// ErrSourceNameTaken 表示命名空间内重名。
	ErrSourceNameTaken = errors.New("source name already used")
	// ErrSourceKindImmutable 表示试图改 kind（换语义只能删了重建）。
	ErrSourceKindImmutable = errors.New("source_kind_immutable")
	// ErrSourceNameImmutable 表示试图改名（换名等于断掉入口边，只能删了重建）。
	ErrSourceNameImmutable = errors.New("source_name_immutable")
)

// Source 是一个「源」实例：事件的入口。
//
// 它**不是能力**：能力由节点声明、中枢派任务给它；源自己产生事件、不干活。
// Targets 是入口边的原始事实（`sources.targets` 列），edges 是它的派生投影
// （`from_is_source=1, from_ref=<源名>`）——两者在同一事务里一起写。
type Source struct {
	ID          string `json:"id"`
	NamespaceID string `json:"namespaceId"`
	// Name 是命名空间内唯一的源名，也是事件的 source（入口边左端）——创建后不可改。
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Config json.RawMessage `json:"config"`
	// Targets 是出口声明：能力引用 + 参数模板（source.md §4）。多行 = 扇出。
	Targets []sourcespec.Target `json:"targets"`
	Enabled bool                `json:"enabled"`
	// LastFiredAt / NextFireAt 只有 cron 有意义（其它 kind 为 nil）。
	LastFiredAt *int64 `json:"lastFiredAt"`
	NextFireAt  *int64 `json:"nextFireAt"`
	// InstalledFrom 是首次安装来源（安装回执 id；空 = 手建，复用不覆盖）。
	InstalledFrom string `json:"installedFrom"`
	// ResourceVersion 是写并发令牌（CAS），与内容版本无关。
	ResourceVersion int64 `json:"resourceVersion"`
	CreatedAt       int64 `json:"createdAt"`
}

// sourceColumns 是统一的列顺序，scanSource 依赖它。
const sourceColumns = `id, namespace_id, name, kind, config, targets, enabled,
	last_fired_at, next_fire_at, installed_from, resource_version, created_at`

// scanSource 从一行结果解析 Source（targets 列在这里解成结构化声明）。
func scanSource(rows *sql.Rows) (*Source, error) {
	var (
		s       Source
		cfg     string
		targets string
		enabled int
		last    sql.NullInt64
		next    sql.NullInt64
	)
	if err := rows.Scan(&s.ID, &s.NamespaceID, &s.Name, &s.Kind, &cfg, &targets, &enabled,
		&last, &next, &s.InstalledFrom, &s.ResourceVersion, &s.CreatedAt); err != nil {
		return nil, fmt.Errorf("scan source: %w", err)
	}
	s.Config = json.RawMessage(cfg)
	if len(s.Config) == 0 {
		s.Config = json.RawMessage("{}")
	}
	s.Targets = []sourcespec.Target{}
	if trimmed := strings.TrimSpace(targets); trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &s.Targets); err != nil {
			return nil, fmt.Errorf("decode source targets: %w", err)
		}
	}
	if s.Targets == nil {
		s.Targets = []sourcespec.Target{}
	}
	s.Enabled = enabled != 0
	if last.Valid {
		v := last.Int64
		s.LastFiredAt = &v
	}
	if next.Valid {
		v := next.Int64
		s.NextFireAt = &v
	}
	return &s, nil
}

// NewSourceToken 生成 webhook 的路径 token：src_<base64url(16B)>。
//
// 用随机串而不是自增 ID：URL 一旦泄漏就等于开放写入口，必须不可枚举、
// 且能通过「重新生成」立刻失效。
func NewSourceToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate source token: %w", err)
	}
	return "src_" + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// CreateSource 建一个源：写 sources 行 + 同事务重建入口边。
//
// 校验顺序（全部在开事务之前，坏输入不该留下一行半成品）：
//  1. 命名空间/名字/kind；
//  2. config 归一化（kind 专属字段、cron 表达式当场解析、webhook token 空则生成）；
//  3. targets 模板语法 + 能力引用在**同命名空间**内可解析。
//
// **空 targets 合法**（source.md §5）：先把入口建起来、再接能力是正常顺序，
// 触发时留一条 no_target 剪枝痕。
func (d *DB) CreateSource(ctx context.Context, s *Source) error {
	if s == nil {
		return fmt.Errorf("create source: nil")
	}
	if s.NamespaceID == "" || s.NamespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	s.Name = strings.TrimSpace(s.Name)
	if s.Name == "" {
		return fmt.Errorf("create source: empty name")
	}
	// 保留来源名：叫 route 的源产生的事件会被事件流过滤掉（静默消失），
	// 所以要在**配置期**就报错，而不是等事件没了再去猜。
	if s.Name == EventPruneSource {
		return fmt.Errorf("create source: %w", ErrReservedEventSource)
	}
	cfg, err := d.prepareSourceConfig(s.Kind, s.Config)
	if err != nil {
		return fmt.Errorf("create source: %w", err)
	}
	s.Config = cfg
	if err := d.checkSourceTargets(ctx, s); err != nil {
		return fmt.Errorf("create source: %w", err)
	}
	if s.ID == "" {
		s.ID = NewSourceID()
	}
	if s.CreatedAt == 0 {
		s.CreatedAt = Now()
	}
	if s.ResourceVersion <= 0 {
		s.ResourceVersion = 1
	}
	nextFire, err := sourceNextFire(s, s.CreatedAt)
	if err != nil {
		return fmt.Errorf("create source: %w", err)
	}
	s.NextFireAt = nextFire

	targetsJSON, err := json.Marshal(s.Targets)
	if err != nil {
		return fmt.Errorf("create source: marshal targets: %w", err)
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create source: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sources (`+sourceColumns+`)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.ID, s.NamespaceID, s.Name, s.Kind, string(s.Config), string(targetsJSON),
		boolToInt(s.Enabled), nil, nullableUnix(nextFire), s.InstalledFrom, s.ResourceVersion,
		s.CreatedAt); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: %q", ErrSourceNameTaken, s.Name)
		}
		return fmt.Errorf("create source: %w", err)
	}
	if err := replaceSourceEdgesOn(ctx, txExec{tx: tx}, s.NamespaceID, s.Name, targetRefs(s)); err != nil {
		return fmt.Errorf("create source: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create source: commit: %w", err)
	}
	return nil
}

// prepareSourceConfig 归一化 config，并在需要时生成 webhook token。
//
// token 生成是**一次性的创建期副作用**：空 token 进来 → 服务端补一个随机串。
// 之后的任何 PATCH 都不会再生成（MergeConfig 保住旧值），所以 URL 不会因为
// 「改了一下 payloadSchema」而换掉。
func (d *DB) prepareSourceConfig(kind string, raw json.RawMessage) (json.RawMessage, error) {
	cfg, err := sourcespec.NormalizeConfig(kind, raw)
	if err != nil {
		return nil, err
	}
	if kind != SourceKindWebhook {
		return cfg, nil
	}
	if sourcespec.ParseConfig(kind, cfg).Token != "" {
		return cfg, nil
	}
	token, err := NewSourceToken()
	if err != nil {
		return nil, err
	}
	fields := map[string]any{}
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &fields); err != nil {
			return nil, fmt.Errorf("%w: config 必须是 JSON 对象", ErrInvalidSourceConfig)
		}
	}
	fields["token"] = token
	withToken, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("create source: marshal config: %w", err)
	}
	return sourcespec.NormalizeConfig(kind, withToken)
}

// checkSourceTargets 校验 targets 并保证每个引用在**同命名空间**内可解析。
//
// 跨命名空间引用一律报错（不静默忽略）：静默会让人以为配置生效了，而事件永远派不出去。
// 引用本身原样保留（`name` 跟随 latest、`name@version` 钉死），不做版本替换——
// targets 是用户写的声明，投影必须与它一字不差。
func (d *DB) checkSourceTargets(ctx context.Context, s *Source) error {
	if err := sourcespec.ValidateTargets(s.Targets); err != nil {
		return err
	}
	for _, t := range s.Targets {
		ref := strings.TrimSpace(t.Capability)
		if _, err := d.ResolveRef(ctx, s.NamespaceID, ref); err != nil {
			if errors.Is(err, ErrNotFound) {
				return fmt.Errorf("出口能力 %s 不存在", ref)
			}
			return fmt.Errorf("出口能力 %s: %w", ref, err)
		}
	}
	return nil
}

// targetRefs 取 targets 里的引用列表（入口边右端）。
func targetRefs(s *Source) []string {
	refs := make([]string, 0, len(s.Targets))
	for _, t := range s.Targets {
		if ref := strings.TrimSpace(t.Capability); ref != "" {
			refs = append(refs, ref)
		}
	}
	return refs
}

// sourceNextFire 算 cron 源的下次触发时刻；非 cron 或未启用 → nil。
//
// 建源时就算好，否则要等到下次进程重启才被调度到。
func sourceNextFire(s *Source, after int64) (*int64, error) {
	if s.Kind != SourceKindCron || !s.Enabled {
		return nil, nil
	}
	t, err := sourcespec.NextCronTime(s.Config, time.Unix(after, 0))
	if err != nil {
		return nil, err
	}
	if t.IsZero() {
		// 表达式永不成立（如 2 月 30 日）：不排期，但建源仍然成功——
		// 调度器看到 nil 就知道这条永远不会自己触发。
		return nil, nil
	}
	unix := t.Unix()
	return &unix, nil
}

// nullableUnix 把 *int64 转成 SQL 参数（nil → NULL）。
func nullableUnix(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// ListSources 列出命名空间内的全部源（按创建时间倒序）；空列表返回 []。
func (d *DB) ListSources(ctx context.Context, namespaceID string) ([]*Source, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.cachedQueryContext(ctx,
		`SELECT `+sourceColumns+` FROM sources WHERE namespace_id = ? ORDER BY created_at DESC, id DESC`,
		namespaceID)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()
	out := []*Source{}
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetSource 按 (namespaceID, id) 取源；未命中返回 ErrSourceNotFound。
func (d *DB) GetSource(ctx context.Context, namespaceID, id string) (*Source, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.cachedQueryContext(ctx,
		`SELECT `+sourceColumns+` FROM sources WHERE namespace_id = ? AND id = ?`, namespaceID, id)
	if err != nil {
		return nil, fmt.Errorf("get source: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get source: %w", err)
		}
		return nil, ErrSourceNotFound
	}
	return scanSource(rows)
}

// GetSourceByName 按 (namespaceID, name) 取源；未命中返回 ErrSourceNotFound。
//
// 入口边的左端是**源名**（没有 source_id 可 join），所以按名字取是触发路径的常态。
func (d *DB) GetSourceByName(ctx context.Context, namespaceID, name string) (*Source, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.cachedQueryContext(ctx,
		`SELECT `+sourceColumns+` FROM sources WHERE namespace_id = ? AND name = ?`, namespaceID, name)
	if err != nil {
		return nil, fmt.Errorf("get source by name: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get source by name: %w", err)
		}
		return nil, ErrSourceNotFound
	}
	return scanSource(rows)
}

// LookupHook 按 webhook token 找源（外部 POST 进来时用）。
//
// **停用的源返回 ErrSourceNotFound，不区分「token 不存在」与「源已停用」**：
// 状态码差异会变成探测 token 是否有效的旁路（source.md §5）。
func (d *DB) LookupHook(ctx context.Context, token string) (*Source, error) {
	if strings.TrimSpace(token) == "" {
		return nil, ErrSourceNotFound
	}
	rows, err := d.cachedQueryContext(ctx,
		`SELECT `+sourceColumns+`
		   FROM sources
		  WHERE kind = ? AND enabled = 1
		    AND json_extract(config, '$.token') = ?`,
		SourceKindWebhook, token)
	if err != nil {
		return nil, fmt.Errorf("lookup hook: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("lookup hook: %w", err)
		}
		return nil, ErrSourceNotFound
	}
	return scanSource(rows)
}

// UpdateSource 更新可改字段（config / enabled / targets），kind 与 name 不可改。
//
// s.Config 按 **PATCH 语义**解释：webhook 的 token/secret 省略 = 保留已有
// （GET 从不回显它们，客户端没有旧值可发），其余字段省略即缺省/清空。
//
// targets 变更与入口边重建在同一个事务里：投影永远跟着声明走，
// 「改了 targets 但边还是旧的」这个形状不会出现。
func (d *DB) UpdateSource(ctx context.Context, s *Source) error {
	if s == nil || s.ID == "" {
		return fmt.Errorf("update source: missing id")
	}
	if s.NamespaceID == "" || s.NamespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	cur, err := d.GetSource(ctx, s.NamespaceID, s.ID)
	if err != nil {
		return err
	}
	if s.Kind == "" {
		s.Kind = cur.Kind
	}
	if s.Kind != cur.Kind {
		return fmt.Errorf("%w: 源 %s 是 %s，不能改成 %s", ErrSourceKindImmutable, cur.Name, cur.Kind, s.Kind)
	}
	name := strings.TrimSpace(s.Name)
	if name == "" {
		name = cur.Name
	}
	if name != cur.Name {
		return fmt.Errorf("%w: 源名 %s 创建后不可修改（请删除后重建）", ErrSourceNameImmutable, cur.Name)
	}
	cfg, err := sourcespec.MergeConfig(s.Kind, cur.Config, s.Config)
	if err != nil {
		return fmt.Errorf("update source: %w", err)
	}
	s.Config = cfg
	if s.Kind == SourceKindWebhook && sourcespec.ParseConfig(s.Kind, s.Config).Token == "" {
		// 老行的 config 坏掉、读不出 token：补一个，否则这个源再也对不上任何 URL。
		cfg, err = d.prepareSourceConfig(s.Kind, s.Config)
		if err != nil {
			return fmt.Errorf("update source: %w", err)
		}
		s.Config = cfg
	}
	if err := d.checkSourceTargets(ctx, s); err != nil {
		return fmt.Errorf("update source: %w", err)
	}
	nextFire, err := sourceNextFire(&Source{Kind: s.Kind, Enabled: s.Enabled, Config: s.Config}, Now())
	if err != nil {
		return fmt.Errorf("update source: %w", err)
	}
	s.NextFireAt = nextFire
	targetsJSON, err := json.Marshal(s.Targets)
	if err != nil {
		return fmt.Errorf("update source: marshal targets: %w", err)
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("update source: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE sources
		    SET config = ?, targets = ?, enabled = ?, next_fire_at = ?, resource_version = resource_version + 1
		  WHERE namespace_id = ? AND id = ?`,
		string(s.Config), string(targetsJSON), boolToInt(s.Enabled), nullableUnix(nextFire),
		s.NamespaceID, s.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: %q", ErrSourceNameTaken, name)
		}
		return fmt.Errorf("update source: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSourceNotFound
	}
	if err := replaceSourceEdgesOn(ctx, txExec{tx: tx}, s.NamespaceID, cur.Name, targetRefs(s)); err != nil {
		return fmt.Errorf("update source: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("update source: commit: %w", err)
	}
	s.Name = cur.Name
	return nil
}

// DeleteSource 删除源，并把它名下的入口边一起删掉。
//
// 为什么必须显式删边：入口边的左端是**来源名**（from_ref），没有外键指向 sources，
// 删了源之后那些边会变成「找不到来源的边」，下次同名的源建出来时会凭空继承旧模板。
func (d *DB) DeleteSource(ctx context.Context, namespaceID, id string) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	cur, err := d.GetSource(ctx, namespaceID, id)
	if err != nil {
		return err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete source: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM edges WHERE namespace_id = ? AND from_is_source = 1 AND from_ref = ?`,
		namespaceID, cur.Name); err != nil {
		return fmt.Errorf("delete source: clear entry edges: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM sources WHERE namespace_id = ? AND id = ?`, namespaceID, id); err != nil {
		return fmt.Errorf("delete source: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete source: commit: %w", err)
	}
	return nil
}

// ListDueSources 取出「到点该触发」的 cron 源（跨命名空间，调度器用）。
//
// now 由调用方传入（便于测试注入）；只取 enabled=1 且 next_fire_at <= now 的。
func (d *DB) ListDueSources(ctx context.Context, now int64, limit int) ([]*Source, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.cachedQueryContext(ctx,
		`SELECT `+sourceColumns+`
		   FROM sources
		  WHERE kind = ? AND enabled = 1
		    AND next_fire_at IS NOT NULL AND next_fire_at <= ?
		  ORDER BY next_fire_at, id
		  LIMIT ?`, SourceKindCron, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list due sources: %w", err)
	}
	defer rows.Close()
	out := []*Source{}
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// MarkSourceFired 记录一次触发，并把下次触发时间推到后面。
//
// 用 nextFire 作为参数而不是「从现在算」：调度器可能延迟醒来，
// 从「上次计划时刻」往后推才不会漏掉或堆积。
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
