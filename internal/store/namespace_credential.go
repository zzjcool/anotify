package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

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
