package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/zzjcool/anotify/internal/sourcespec"
)

// 命名空间凭据（#532）：值只写不读（不进 config、不进回显、不进日志），写入口只有 poll 的 config.secret。
var ErrNamespaceSecretNotFound = errors.New("store: 命名空间凭据不存在")

func (d *DB) UpsertNamespaceSecret(ctx context.Context, namespaceID, name, value string) error {
	return upsertNamespaceSecretOn(ctx, d, namespaceID, name, value)
}

func upsertNamespaceSecretOn(ctx context.Context, ex queryExec, namespaceID, name, value string) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	if name == "" || value == "" {
		return fmt.Errorf("upsert namespace secret: name/value 不能为空")
	}
	now := Now()
	if _, err := ex.execContext(ctx,
		`INSERT INTO namespace_secrets (namespace_id, name, value, created_at, updated_at)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(namespace_id, name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		namespaceID, name, value, now, now); err != nil {
		return fmt.Errorf("upsert namespace secret: %w", err)
	}
	return nil
}

func (d *DB) GetNamespaceSecret(ctx context.Context, namespaceID, name string) (string, error) {
	return getNamespaceSecretOn(ctx, d, namespaceID, name)
}

func getNamespaceSecretOn(ctx context.Context, ex queryExec, namespaceID, name string) (string, error) {
	var value string
	err := ex.queryRowContext(ctx,
		`SELECT value FROM namespace_secrets WHERE namespace_id = ? AND name = ?`, namespaceID, name).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %q", ErrNamespaceSecretNotFound, name)
	}
	if err != nil {
		return "", fmt.Errorf("get namespace secret: %w", err)
	}
	return value, nil
}

// pollSecretOf 必须在归一化之前取：NormalizeConfig 的输出不含 secret（它不属于落库的 config）。
func pollSecretOf(kind string, raw json.RawMessage) string {
	if kind != SourceKindPoll || len(raw) == 0 {
		return ""
	}
	var fields struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ""
	}
	return fields.Secret
}

// resolveSourceSecretOn 落本轮 secret 并保证 secretRef 可解析：没给值又查不到 → 400
// （cause 带 config.secretRef，前端点得到那一行）。
func resolveSourceSecretOn(ctx context.Context, ex queryExec, namespaceID, kind string, cfg json.RawMessage, secret string) error {
	if kind != SourceKindPoll {
		return nil
	}
	ref := sourcespec.ParseConfig(kind, cfg).SecretRef
	if ref == nil {
		if secret != "" {
			return &sourcespec.CausesError{Causes: []sourcespec.Cause{{
				Field:   "config.secret",
				Reason:  sourcespec.CauseReasonValueInvalid,
				Message: "写了 secret 就必须同时写 secretRef（凭据要有名字才能被引用）",
			}}}
		}
		return nil
	}
	if secret != "" {
		return upsertNamespaceSecretOn(ctx, ex, namespaceID, ref.Name, secret)
	}
	if _, err := getNamespaceSecretOn(ctx, ex, namespaceID, ref.Name); err != nil {
		if errors.Is(err, ErrNamespaceSecretNotFound) {
			return &sourcespec.CausesError{Causes: []sourcespec.Cause{{
				Field:   "config.secretRef",
				Reason:  sourcespec.CauseReasonValueInvalid,
				Message: fmt.Sprintf("命名空间凭据 %q 不存在（首次引用时同时给 config.secret）", ref.Name),
			}}}
		}
		return err
	}
	return nil
}

// ---------- 凭据资源面（#554）：CRUD + 引用查询 + 审计 ----------

// NamespaceSecretFact 是凭据的读投影：**没有 value**（值只写不读）。
type NamespaceSecretFact struct {
	NamespaceID string
	Name        string
	CreatedAt   int64
	UpdatedAt   int64
	UsedBy      []string
}

// 审计动作的闭集（与 schema.sql 的 CHECK 同口径）。
const (
	NamespaceSecretCreated = "created"
	NamespaceSecretRotated = "rotated"
	NamespaceSecretDeleted = "deleted"
)

// NamespaceSecretAuditEntry 是 namespace_secret_audit 的一行（append-only，不含值）。
type NamespaceSecretAuditEntry struct {
	ID          string
	NamespaceID string
	Name        string
	Action      string
	ActorID     string
	CreatedAt   int64
}

// NamespaceSecretInUseError：被源引用的凭据**不删**（409 secret_in_use）。
type NamespaceSecretInUseError struct {
	Name    string
	Sources []string
}

func (e *NamespaceSecretInUseError) Error() string {
	return fmt.Sprintf("命名空间凭据 %q 还被 %d 个源引用（%s），先改那些源的 secretRef",
		e.Name, len(e.Sources), strings.Join(e.Sources, ", "))
}

// ListNamespaceSecrets 列本 ns 的凭据读投影（按名字排序，UsedBy 已算好）。
func (d *DB) ListNamespaceSecrets(ctx context.Context, namespaceID string) ([]NamespaceSecretFact, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	usage, err := secretUsageOn(ctx, d, namespaceID)
	if err != nil {
		return nil, err
	}
	rows, err := d.queryContext(ctx,
		`SELECT name, created_at, updated_at FROM namespace_secrets WHERE namespace_id = ? ORDER BY name`,
		namespaceID)
	if err != nil {
		return nil, fmt.Errorf("list namespace secrets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []NamespaceSecretFact{}
	for rows.Next() {
		f := NamespaceSecretFact{NamespaceID: namespaceID}
		if err := rows.Scan(&f.Name, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan namespace secret: %w", err)
		}
		f.UsedBy = usedByOf(usage, f.Name)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list namespace secrets: %w", err)
	}
	return out, nil
}

// GetNamespaceSecretMeta 读单条凭据的读投影（不含值）。
func (d *DB) GetNamespaceSecretMeta(ctx context.Context, namespaceID, name string) (*NamespaceSecretFact, error) {
	return getNamespaceSecretMetaOn(ctx, d, namespaceID, name)
}

func getNamespaceSecretMetaOn(ctx context.Context, ex queryExec, namespaceID, name string) (*NamespaceSecretFact, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	f := &NamespaceSecretFact{NamespaceID: namespaceID, Name: name}
	err := ex.queryRowContext(ctx,
		`SELECT created_at, updated_at FROM namespace_secrets WHERE namespace_id = ? AND name = ?`,
		namespaceID, name).Scan(&f.CreatedAt, &f.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %q", ErrNamespaceSecretNotFound, name)
	}
	if err != nil {
		return nil, fmt.Errorf("get namespace secret meta: %w", err)
	}
	usage, err := secretUsageOn(ctx, ex, namespaceID)
	if err != nil {
		return nil, err
	}
	f.UsedBy = usedByOf(usage, name)
	return f, nil
}

// secretUsageOn 扫本 ns 的源，按 config.secretRef.name 归组（Go 侧解析，不依赖 SQLite json1）。
func secretUsageOn(ctx context.Context, ex queryExec, namespaceID string) (map[string][]string, error) {
	rows, err := ex.queryContext(ctx, `SELECT name, kind, config FROM sources WHERE namespace_id = ?`, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("secret usage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	usage := map[string][]string{}
	for rows.Next() {
		var srcName, kind, cfg string
		if err := rows.Scan(&srcName, &kind, &cfg); err != nil {
			return nil, fmt.Errorf("secret usage: %w", err)
		}
		ref := sourcespec.ParseConfig(kind, json.RawMessage(cfg)).SecretRef
		if ref == nil || ref.Name == "" {
			continue
		}
		usage[ref.Name] = append(usage[ref.Name], srcName)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("secret usage: %w", err)
	}
	for _, names := range usage {
		sort.Strings(names)
	}
	return usage, nil
}

// usedByOf 取引用者（空列表是 [] 不是 nil：空列表纪律）。
func usedByOf(usage map[string][]string, name string) []string {
	if names := usage[name]; len(names) > 0 {
		return names
	}
	return []string{}
}

// PutNamespaceSecret 新建或轮换：值写入走 upsertNamespaceSecretOn（与老路径**同一条语句**），
// 同事务补一条审计。created 由事务内「行在不在」判（并发两次不会都报 created）。
func (d *DB) PutNamespaceSecret(ctx context.Context, namespaceID, name, value, actorID string) (bool, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return false, ErrNamespaceRequired
	}
	if err := sourcespec.ValidateSecretName(name); err != nil {
		return false, fmt.Errorf("put namespace secret: %w", err)
	}
	if value == "" {
		return false, fmt.Errorf("put namespace secret: value 不能为空")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("put namespace secret: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	var probe int
	err = ex.queryRowContext(ctx,
		`SELECT 1 FROM namespace_secrets WHERE namespace_id = ? AND name = ?`, namespaceID, name).Scan(&probe)
	created := errors.Is(err, sql.ErrNoRows)
	if err != nil && !created {
		return false, fmt.Errorf("put namespace secret: probe: %w", err)
	}
	if err := upsertNamespaceSecretOn(ctx, ex, namespaceID, name, value); err != nil {
		return false, err
	}
	action := NamespaceSecretRotated
	if created {
		action = NamespaceSecretCreated
	}
	if err := appendNamespaceSecretAuditOn(ctx, ex, namespaceID, name, action, actorID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("put namespace secret: commit: %w", err)
	}
	return created, nil
}

// DeleteNamespaceSecret 删除一个未被引用的凭据。被任一源引用 → *NamespaceSecretInUseError（不删）。
func (d *DB) DeleteNamespaceSecret(ctx context.Context, namespaceID, name, actorID string) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	if err := sourcespec.ValidateSecretName(name); err != nil {
		return fmt.Errorf("delete namespace secret: %w", err)
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete namespace secret: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	var probe int
	err = ex.queryRowContext(ctx,
		`SELECT 1 FROM namespace_secrets WHERE namespace_id = ? AND name = ?`, namespaceID, name).Scan(&probe)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %q", ErrNamespaceSecretNotFound, name)
	}
	if err != nil {
		return fmt.Errorf("delete namespace secret: probe: %w", err)
	}
	usage, err := secretUsageOn(ctx, ex, namespaceID)
	if err != nil {
		return err
	}
	if names := usage[name]; len(names) > 0 {
		return &NamespaceSecretInUseError{Name: name, Sources: names}
	}
	if _, err := ex.execContext(ctx,
		`DELETE FROM namespace_secrets WHERE namespace_id = ? AND name = ?`, namespaceID, name); err != nil {
		return fmt.Errorf("delete namespace secret: %w", err)
	}
	if err := appendNamespaceSecretAuditOn(ctx, ex, namespaceID, name, NamespaceSecretDeleted, actorID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete namespace secret: commit: %w", err)
	}
	return nil
}

// appendNamespaceSecretAuditOn 是凭据审计的唯一写入实现：只记 name/action/actor，**不记值**。
func appendNamespaceSecretAuditOn(ctx context.Context, ex queryExec, namespaceID, name, action, actorID string) error {
	switch action {
	case NamespaceSecretCreated, NamespaceSecretRotated, NamespaceSecretDeleted:
	default:
		return fmt.Errorf("namespace secret audit: action %q 不在闭集内", action)
	}
	if _, err := ex.execContext(ctx,
		`INSERT INTO namespace_secret_audit (id, namespace_id, name, action, actor_id, created_at)
		 VALUES (?,?,?,?,?,?)`,
		NewNamespaceSecretAuditID(), namespaceID, name, action, actorID, Now()); err != nil {
		return fmt.Errorf("namespace secret audit: %w", err)
	}
	return nil
}

// ListNamespaceSecretAudit 读审计（最新在前；name 为空 = 本 ns 全部）。
func (d *DB) ListNamespaceSecretAudit(ctx context.Context, namespaceID, name string, limit int) ([]NamespaceSecretAuditEntry, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	q := `SELECT id, name, action, actor_id, created_at FROM namespace_secret_audit WHERE namespace_id = ?`
	args := []any{namespaceID}
	if name != "" {
		q += ` AND name = ?`
		args = append(args, name)
	}
	// 同一秒内多次操作要按插入顺序（rowid）决出先后：id 是随机 base62，排序不可用。
	q += ` ORDER BY created_at DESC, rowid DESC LIMIT ?`
	args = append(args, limit)
	rows, err := d.queryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list namespace secret audit: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []NamespaceSecretAuditEntry{}
	for rows.Next() {
		e := NamespaceSecretAuditEntry{NamespaceID: namespaceID}
		if err := rows.Scan(&e.ID, &e.Name, &e.Action, &e.ActorID, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan namespace secret audit: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list namespace secret audit: %w", err)
	}
	return out, nil
}
