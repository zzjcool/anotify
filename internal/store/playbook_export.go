package store

// 预案实例的导出事实（#536）：按 release 取「它装出了什么」的回执读数；与卸载同一判据。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zzjcool/anotify/internal/playbook"
)

// PlaybookReleaseExport 是一个 release 的导出输入：回执（含 created 名单）+ 是否已全部卸载。
type PlaybookReleaseExport struct {
	Release  string
	Receipts []playbook.Receipt
	// Uninstalled：回执都在 uninstalled_at != 0（对象已删，导出端回与「不存在」同一个 404）。
	Uninstalled bool
}

// GetPlaybookReleaseExport 按 (ns, slug, release) 读出全部回执（安装顺序）。
// release 归一化与形状校验走 playbook.NormalizeRelease（与 install / uninstall 同口径）。
func (d *DB) GetPlaybookReleaseExport(ctx context.Context, namespaceID, slug, release string) (*PlaybookReleaseExport, error) {
	return d.playbookReleaseExport(ctx, namespaceID, slug, release, "")
}

// GetPlaybookReleaseExportGroup 只取一个参数组（valuesDigest）的回执：页面按分组展示时，
// 导出的 YAML 必须与所看的那一组同一份（不带摘要的导出是各组回执的并集）。
func (d *DB) GetPlaybookReleaseExportGroup(ctx context.Context, namespaceID, slug, release, valuesDigest string) (*PlaybookReleaseExport, error) {
	digest := strings.TrimSpace(valuesDigest)
	if digest == "" {
		return d.playbookReleaseExport(ctx, namespaceID, slug, release, "")
	}
	return d.playbookReleaseExport(ctx, namespaceID, slug, release, digest)
}

// playbookReleaseExport 是全量 / 按组导出的共读路径（valuesDigest 为空 = 不筛）。
func (d *DB) playbookReleaseExport(ctx context.Context, namespaceID, slug, release, valuesDigest string) (*PlaybookReleaseExport, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	row, err := d.GetPlaybook(ctx, namespaceID, slug)
	if err != nil {
		return nil, err
	}
	rel, err := playbook.NormalizeRelease(release, row.Slug)
	if err != nil {
		return nil, err
	}
	query := `SELECT receipt, release, uninstalled_at FROM playbook_installs
		  WHERE namespace_id = ? AND playbook_id = ? AND release = ?`
	args := []any{namespaceID, row.ID, rel}
	if valuesDigest != "" {
		query += ` AND values_digest = ?`
		args = append(args, valuesDigest)
	}
	rows, err := d.cachedQueryContext(ctx, query+` ORDER BY created_at, rowid`, args...)
	if err != nil {
		return nil, fmt.Errorf("playbook release export: list receipts: %w", err)
	}
	defer rows.Close()
	exp := &PlaybookReleaseExport{Release: rel, Receipts: []playbook.Receipt{}, Uninstalled: true}
	for rows.Next() {
		var raw, rowRelease string
		var uninstalledAt int64
		if err := rows.Scan(&raw, &rowRelease, &uninstalledAt); err != nil {
			return nil, fmt.Errorf("playbook release export: scan receipt: %w", err)
		}
		var receipt playbook.Receipt
		if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
			return nil, fmt.Errorf("playbook release export: decode receipt: %w", err)
		}
		if receipt.Dependencies == nil {
			receipt.Dependencies = []playbook.Dependency{}
		}
		normalizePinned(&receipt)
		if receipt.Install.Release == "" {
			// 迁移前的回执原文没有 release；列上已回填（见 GetPlaybookInstall 的同一处理）。
			receipt.Install.Release = rowRelease
		}
		if uninstalledAt == 0 {
			exp.Uninstalled = false
		}
		exp.Receipts = append(exp.Receipts, receipt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("playbook release export: %w", err)
	}
	if len(exp.Receipts) == 0 {
		return nil, fmt.Errorf("%w: release %q", ErrPlaybookReleaseNotFound, rel)
	}
	return exp, nil
}

// PlaybookInstallPinned 按 (install_id, seq) 读出安装时快照（#551），返回每个 install 的有序切片（顺序就是回执 created.pinned 的顺序，不重排）；老回执没有行 → 该 install 不在 map 里（调用方按 ErrPlaybookSnapshotMissing 处理）。
func (d *DB) PlaybookInstallPinned(ctx context.Context, namespaceID string, installIDs []string) (map[string][]playbook.PinnedRef, error) {
	out := map[string][]playbook.PinnedRef{}
	if len(installIDs) == 0 {
		return out, nil // 不拼 `IN ()`
	}
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.cachedQueryContext(ctx,
		`SELECT install_id, ref_kind, name, obj_kind, version, digest
		   FROM playbook_install_pinned
		  WHERE namespace_id = ? AND install_id IN (`+placeholdersFor(len(installIDs))+`)
		  ORDER BY install_id, seq, ref_kind, name`,
		append([]any{namespaceID}, anyArgs(installIDs)...)...)
	if err != nil {
		return nil, fmt.Errorf("playbook install pinned: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var installID string
		var pin playbook.PinnedRef
		if err := rows.Scan(&installID, &pin.Kind, &pin.Name, &pin.KindOf, &pin.Version, &pin.Digest); err != nil {
			return nil, fmt.Errorf("playbook install pinned: scan: %w", err)
		}
		out[installID] = append(out[installID], pin)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("playbook install pinned: %w", err)
	}
	return out, nil
}
