package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zzjcool/anotify/internal/sourcespec"
)

// 命名空间凭据（#532）：poll 源的 secretRef 指向这里的一行。
//
// 值只写不读 —— 它不出现在 sources.config、不进 GET 回显、不进日志。写入口只有 poll 源的
// write-only config.secret（与 webhook 的 token/secret 同一纪律），因此不需要新的 HTTP 端点。
var ErrNamespaceSecretNotFound = errors.New("store: 命名空间凭据不存在")

// UpsertNamespaceSecret 写入或更新一份命名空间凭据。
func (d *DB) UpsertNamespaceSecret(ctx context.Context, namespaceID, name, value string) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	if name == "" || value == "" {
		return fmt.Errorf("upsert namespace secret: name/value 不能为空")
	}
	now := Now()
	if _, err := d.ExecContext(ctx,
		`INSERT INTO namespace_secrets (namespace_id, name, value, created_at, updated_at)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(namespace_id, name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		namespaceID, name, value, now, now); err != nil {
		return fmt.Errorf("upsert namespace secret: %w", err)
	}
	return nil
}

// GetNamespaceSecret 取凭据值（运行期唯一读点）。
func (d *DB) GetNamespaceSecret(ctx context.Context, namespaceID, name string) (string, error) {
	var value string
	err := d.QueryRowContext(ctx,
		`SELECT value FROM namespace_secrets WHERE namespace_id = ? AND name = ?`, namespaceID, name).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %q", ErrNamespaceSecretNotFound, name)
	}
	if err != nil {
		return "", fmt.Errorf("get namespace secret: %w", err)
	}
	return value, nil
}

// pollSecretOf 取出 poll config 里 write-only 的 `secret`。
//
// 必须在归一化**之前**取：NormalizeConfig 的输出不含它（它不属于落库的 config 字节）。
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

// resolveSourceSecret 把本轮写入的 secret 落到 namespace_secrets，并保证 secretRef 能解析。
//
// 两条判据：#532 验收 7 的「缺失凭据引用 → 写入期 400」。没给 secret 又查不到 → 400，
// 带上 `config.secretRef` 路径（前端点得到那一行）。
func (d *DB) resolveSourceSecret(ctx context.Context, namespaceID, kind string, cfg json.RawMessage, secret string) error {
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
		return d.UpsertNamespaceSecret(ctx, namespaceID, ref.Name, secret)
	}
	if _, err := d.GetNamespaceSecret(ctx, namespaceID, ref.Name); err != nil {
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
