package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// 能力来源（契约 §2.2 source 列）。
const (
	CapSourceBuiltin = "builtin"
	CapSourceCustom  = "custom"
)

// Lifecycle 是能力定义里的生命周期参数（JSON 存在 lifecycle 列）。
type Lifecycle struct {
	ExpiresInSec int  `json:"expiresInSec,omitempty"`
	Retryable    bool `json:"retryable,omitempty"`
	MaxAttempts  int  `json:"maxAttempts,omitempty"`
	ResultTTLSec int  `json:"resultTtlSec,omitempty"`
	// Idempotent 为 false 表示重放不安全：租约过期后直接 abandoned，不回 pending（契约 §3.2）。
	Idempotent *bool `json:"idempotent,omitempty"`
}

// IsIdempotent 默认 true：没声明就当可重试（与契约 §3.2 的 "实现 idempotent:false → abandoned" 对应）。
func (l Lifecycle) IsIdempotent() bool {
	if l.Idempotent == nil {
		return true
	}
	return *l.Idempotent
}

// CapabilityVersion 是 capability_versions 表的一行：一个 name@version 的不可变定义（不变量 I8）。
//
// Outputs 是出口名的唯一来源，同时是 edges.from_port 的封闭集合（不变量 I2）。
// 没有 Kind 字段——kind 由 DeriveKind 推导，不落库（不变量 I3）。
type CapabilityVersion struct {
	ID           string          `json:"id"`
	OrgID        string          `json:"orgId"`
	Name         string          `json:"name"`
	Version      int             `json:"version"`
	ParamsSchema json.RawMessage `json:"paramsSchema"`
	Outputs      []string        `json:"outputs"`
	Lifecycle    Lifecycle       `json:"lifecycle"`
	Sticky       string          `json:"sticky"`
	Source       string          `json:"source"`
	ContentHash  string          `json:"contentHash"`
	CreatedAt    int64           `json:"createdAt"`
}

// CreateCapability 追加一个能力版本。
//
// 同内容（content_hash 相同）不产生第二行：返回 nil 且把 c 填成已有那行——
// 这是「幂等创建」，不是错误。
func (d *DB) CreateCapability(ctx context.Context, c *CapabilityVersion) error {
	return ErrNotImplemented
}

// GetCapability 按 name + version 取能力定义；未命中返回 ErrNotFound。
func (d *DB) GetCapability(ctx context.Context, orgID, name string, version int) (*CapabilityVersion, error) {
	return nil, ErrNotImplemented
}

// GetCapabilityByID 按 ID 取能力定义；未命中返回 ErrNotFound。
func (d *DB) GetCapabilityByID(ctx context.Context, orgID, id string) (*CapabilityVersion, error) {
	return nil, ErrNotImplemented
}

// ListCapabilities 列出组织内全部能力定义（按 name、version 升序；空列表返回 []）。
func (d *DB) ListCapabilities(ctx context.Context, orgID string) ([]*CapabilityVersion, error) {
	return nil, ErrNotImplemented
}

// NextCapabilityVersion 返回某 name 的下一个版本号（无定义时返回 1）。
func (d *DB) NextCapabilityVersion(ctx context.Context, orgID, name string) (int, error) {
	return 0, ErrNotImplemented
}

// NormalizeCapability 计算 content_hash：把 params_schema / outputs / lifecycle / sticky
// 规范化（键排序、稳定序列化）后取 sha256。同内容必须得同 hash。
func NormalizeCapability(c *CapabilityVersion) (string, error) {
	return "", fmt.Errorf("NormalizeCapability: %w", ErrNotImplemented)
}
