package store

// 预案实例的导出事实（#536）：按 release 取「它装出了什么」的回执读数。
// 集合只由回执决定（不按前缀匹配、不从 installedFrom 反查）；与卸载同一判据：
// 没有回执 → ErrPlaybookReleaseNotFound；回执全部已标记卸载 → Uninstalled。

import (
	"context"
	"encoding/json"
	"fmt"

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
	rows, err := d.cachedQueryContext(ctx,
		`SELECT receipt, release, uninstalled_at FROM playbook_installs
		  WHERE namespace_id = ? AND playbook_id = ? AND release = ?
		  ORDER BY created_at, rowid`,
		namespaceID, row.ID, rel)
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
