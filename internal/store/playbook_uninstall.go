package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/playbook"
)

// 预案卸载（#510）：按 release 撤掉**它装出来的**行，回执只打标记、不删。归属判据是
// installed_from：手工行、别的 release 的行、仍被另一个 release 复用的行都不删，进 kept[]。
var (
	// ErrPlaybookReleaseNotFound 表示这个包名下没有该 release 的回执（404 release_not_found）。
	ErrPlaybookReleaseNotFound = errors.New("store: playbook release not found")
	// ErrPlaybookSnapshotMissing 表示选中的回执没有任何安装时快照行（老回执；导出端翻 409 no_snapshot）。
	ErrPlaybookSnapshotMissing = errors.New("store: playbook install snapshot missing")
	// ErrPlaybookInstallMismatch 表示 installId 不在该 release 的回执里（409 install_conflict）。
	ErrPlaybookInstallMismatch = errors.New("store: install id does not belong to this release")
)

// PlaybookUninstallNote 是卸载响应的固定说明（回执是审计，不是所有权）。
const PlaybookUninstallNote = "已删除该 release 装出的对象；安装回执保留作为审计，他人创建的对象未删除"

// kept[] 的封闭原因码（契约 §2）：手工行 / 别的安装装出来的行 / 被另一个生效中的 release 复用。
const (
	uninstallReasonManual = "manual"
	uninstallReasonOther  = "other_release"
	uninstallReasonShared = "shared_by_release"
)

// PlaybookUninstallKept 是 kept[] 的一条：留下了哪一行、为什么留（码表见上）。
type PlaybookUninstallKept struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Version 只有能力有（源名在命名空间内唯一）。
	Version int    `json:"version,omitempty"`
	Reason  string `json:"reason"`
	// OwnerRelease 只在 other_release / shared_by_release 时给出。
	OwnerRelease string `json:"ownerRelease,omitempty"`
}

// PlaybookUninstallResult 是 `DELETE …/playbooks/{slug}/releases/{release}` 的响应体（动作文档，不套资源信封）。
type PlaybookUninstallResult struct {
	Release string `json:"release"`
	// AlreadyUninstalled 为真时本次 0 写：removed 两个列表与 kept 都是空。
	AlreadyUninstalled bool                    `json:"alreadyUninstalled"`
	UninstalledAt      string                  `json:"uninstalledAt"`
	InstallIDs         []string                `json:"installIds"`
	Removed            PlaybookKept            `json:"removed"`
	Kept               []PlaybookUninstallKept `json:"kept"`
	Note               string                  `json:"note"`
}

// UninstallPlaybookRelease 在一个事务里撤掉该 release 装出的对象（契约 §2/§3）。
//
// 顺序固定：读回执 → already?（0 写返回）→ 找候选行 → 算 kept → 逐行 CAS 软删/删行 →
// 重算受影响名字的边投影 → 给回执行打 uninstalled_at → COMMIT。pinInstallID 非空时必须
// 落在该 release 的回执里，否则 409（「我看到的那次安装」已被别人换掉）。
func (d *DB) UninstallPlaybookRelease(ctx context.Context, namespaceID, slug, release, pinInstallID string) (*PlaybookUninstallResult, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, ErrNotFound
	}
	// release 的归一化放在事务之前：形状错是调用方输入错误（400），不必开事务。
	rel, err := playbook.NormalizeRelease(release, slug)
	if err != nil {
		return nil, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("uninstall playbook: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	row, err := getPlaybookOn(ctx, ex, namespaceID, slug)
	if err != nil {
		return nil, err
	}
	receipts, err := releaseReceiptsOn(ctx, ex, namespaceID, row.ID, rel)
	if err != nil {
		return nil, err
	}
	if len(receipts) == 0 {
		return nil, fmt.Errorf("%w: release %q", ErrPlaybookReleaseNotFound, rel)
	}
	ids := make([]string, 0, len(receipts))
	uninstalled, lastAt := true, int64(0)
	for _, r := range receipts {
		ids = append(ids, r.id)
		if r.uninstalledAt == 0 {
			uninstalled = false
		} else if r.uninstalledAt > lastAt {
			lastAt = r.uninstalledAt
		}
	}
	if pin := strings.TrimSpace(pinInstallID); pin != "" && !stringInSlice(ids, pin) {
		return nil, fmt.Errorf("%w: %s", ErrPlaybookInstallMismatch, pin)
	}
	if uninstalled {
		// 幂等的落点：一个字都不写（回滚由 defer 完成），两个列表与 kept 都是空。
		return &PlaybookUninstallResult{
			Release:            rel,
			AlreadyUninstalled: true,
			UninstalledAt:      time.Unix(lastAt, 0).UTC().Format(time.RFC3339),
			InstallIDs:         ids,
			Removed:            PlaybookKept{Capabilities: []string{}, Sources: []string{}},
			Kept:               []PlaybookUninstallKept{},
			Note:               PlaybookUninstallNote,
		}, nil
	}

	caps, err := uninstallCandidateCapabilitiesOn(ctx, ex, namespaceID, ids)
	if err != nil {
		return nil, err
	}
	srcs, err := uninstallCandidateSourcesOn(ctx, ex, namespaceID, ids)
	if err != nil {
		return nil, err
	}
	sharedCaps, sharedSrcs, err := sharedReleaseRefsOn(ctx, ex, namespaceID, ids)
	if err != nil {
		return nil, err
	}

	now := Now()
	kept := []PlaybookUninstallKept{}
	added := map[string]bool{}
	addKept := func(e PlaybookUninstallKept) {
		key := fmt.Sprintf("%s\x00%s\x00%d\x00%s\x00%s", e.Kind, e.Name, e.Version, e.Reason, e.OwnerRelease)
		if added[key] {
			return
		}
		added[key] = true
		kept = append(kept, e)
	}
	owners := map[string]string{}
	ownerOn := func(installID string) (string, error) {
		if installID == "" {
			return "", nil
		}
		if rel, ok := owners[installID]; ok {
			return rel, nil
		}
		var rel string
		err := ex.queryRowContext(ctx,
			`SELECT release FROM playbook_installs WHERE namespace_id = ? AND id = ?`,
			namespaceID, installID).Scan(&rel)
		if errors.Is(err, sql.ErrNoRows) {
			// 悬挂引用（回执被显式 GC 过）：当无主，不编造 ownerRelease。
			rel = ""
		} else if err != nil {
			return "", fmt.Errorf("uninstall playbook: release of install %s: %w", installID, err)
		}
		owners[installID] = rel
		return rel, nil
	}

	// 同名但**不是本 release 装的**未删行：手工行记 manual，别的安装记 other_release。
	capNames := []string{}
	for _, c := range caps {
		if len(capNames) == 0 || capNames[len(capNames)-1] != c.name {
			capNames = append(capNames, c.name)
		}
	}
	for _, name := range capNames {
		foreign, err := foreignCapabilityRowsOn(ctx, ex, namespaceID, name, ids)
		if err != nil {
			return nil, err
		}
		for _, f := range foreign {
			reason, owner := uninstallReasonManual, ""
			if f.installedFrom != "" {
				reason = uninstallReasonOther
				if owner, err = ownerOn(f.installedFrom); err != nil {
					return nil, err
				}
			}
			addKept(PlaybookUninstallKept{Kind: "capability", Name: name, Version: f.version, Reason: reason, OwnerRelease: owner})
		}
	}
	for _, s := range srcs {
		foreign, err := foreignSourceRowsOn(ctx, ex, namespaceID, s.Name, ids)
		if err != nil {
			return nil, err
		}
		for _, from := range foreign {
			reason, owner := uninstallReasonManual, ""
			if from != "" {
				reason = uninstallReasonOther
				if owner, err = ownerOn(from); err != nil {
					return nil, err
				}
			}
			addKept(PlaybookUninstallKept{Kind: "source", Name: s.Name, Reason: reason, OwnerRelease: owner})
		}
	}
	// 本 release 自己建的行，被另一个仍生效的 release 记在 skipped（复用）里：留下它。
	for _, c := range caps {
		if owner, ok := sharedCaps[FormatRef(c.name, c.version)]; ok {
			addKept(PlaybookUninstallKept{Kind: "capability", Name: c.name, Version: c.version, Reason: uninstallReasonShared, OwnerRelease: owner})
		}
	}
	for _, s := range srcs {
		if owner, ok := sharedSrcs[s.Name]; ok {
			addKept(PlaybookUninstallKept{Kind: "source", Name: s.Name, Reason: uninstallReasonShared, OwnerRelease: owner})
		}
	}

	// 删前记住每个名字的可见最高版本：只有它被删掉时才需要重算这个名字的边投影。
	maxVisible := map[string]int{}
	for _, name := range capNames {
		if maxVisible[name], err = maxVisibleCapabilityVersionOn(ctx, ex, namespaceID, name); err != nil {
			return nil, err
		}
	}
	removedCaps := []string{}
	deleted := map[string]map[int]bool{}
	for _, c := range caps {
		if _, ok := sharedCaps[FormatRef(c.name, c.version)]; ok {
			continue
		}
		res, err := ex.execContext(ctx,
			`UPDATE capability_versions SET deleted_at = ?, resource_version = resource_version + 1
			  WHERE namespace_id = ? AND name = ? AND id = ? AND resource_version = ? AND deleted_at IS NULL`,
			now, namespaceID, c.name, c.id, c.rv)
		if err != nil {
			return nil, fmt.Errorf("uninstall playbook: delete capability %s@%d: %w", c.name, c.version, err)
		}
		if err := requireCASRow(res, c.id, c.rv); err != nil {
			return nil, err
		}
		removedCaps = append(removedCaps, FormatRef(c.name, c.version))
		if deleted[c.name] == nil {
			deleted[c.name] = map[int]bool{}
		}
		deleted[c.name][c.version] = true
	}
	for _, name := range capNames {
		if !deleted[name][maxVisible[name]] {
			continue
		}
		latest, err := getCapabilityLatestOn(ctx, ex, namespaceID, name)
		switch {
		case errors.Is(err, ErrNotFound):
			if _, err := ex.execContext(ctx,
				`DELETE FROM edges WHERE namespace_id = ? AND from_is_source = 0 AND from_ref = ?`,
				namespaceID, name); err != nil {
				return nil, fmt.Errorf("uninstall playbook: drop projection of %q: %w", name, err)
			}
		case err != nil:
			return nil, err
		default:
			if err := rebuildCapabilityEdgesOn(ctx, ex, namespaceID, name, latest.Spec.Next); err != nil {
				return nil, err
			}
		}
	}

	removedSrcs := []string{}
	for _, s := range srcs {
		if _, ok := sharedSrcs[s.Name]; ok {
			continue
		}
		if err := deleteSourceOn(ctx, ex, s); err != nil {
			return nil, err
		}
		removedSrcs = append(removedSrcs, s.Name)
	}

	if _, err := ex.execContext(ctx,
		`UPDATE playbook_installs SET uninstalled_at = ?
		  WHERE namespace_id = ? AND playbook_id = ? AND release = ? AND uninstalled_at = 0`,
		now, namespaceID, row.ID, rel); err != nil {
		return nil, fmt.Errorf("uninstall playbook: mark receipts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("uninstall playbook: commit: %w", err)
	}

	sort.Strings(removedCaps)
	sort.Strings(removedSrcs)
	sort.SliceStable(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Version < b.Version
	})
	return &PlaybookUninstallResult{
		Release:            rel,
		AlreadyUninstalled: false,
		UninstalledAt:      time.Unix(now, 0).UTC().Format(time.RFC3339),
		InstallIDs:         ids,
		Removed:            PlaybookKept{Capabilities: removedCaps, Sources: removedSrcs},
		Kept:               kept,
		Note:               PlaybookUninstallNote,
	}, nil
}

// playbookReleaseReceipt 是一行回执的卸载事实（判 already 只看这两列）。
type playbookReleaseReceipt struct {
	id            string
	uninstalledAt int64
}

func releaseReceiptsOn(ctx context.Context, ex queryExec, namespaceID, playbookID, release string) ([]playbookReleaseReceipt, error) {
	rows, err := ex.queryContext(ctx,
		`SELECT id, uninstalled_at FROM playbook_installs
		  WHERE namespace_id = ? AND playbook_id = ? AND release = ?
		  ORDER BY created_at, rowid`, namespaceID, playbookID, release)
	if err != nil {
		return nil, fmt.Errorf("uninstall playbook: list release receipts: %w", err)
	}
	defer rows.Close()
	out := []playbookReleaseReceipt{}
	for rows.Next() {
		var r playbookReleaseReceipt
		if err := rows.Scan(&r.id, &r.uninstalledAt); err != nil {
			return nil, fmt.Errorf("uninstall playbook: scan release receipt: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// uninstallCapabilityRow 是候选能力行（删它要 CAS，所以带上 resource_version 与 installed_from）。
type uninstallCapabilityRow struct {
	id            string
	name          string
	version       int
	installedFrom string
	rv            int64
}

func uninstallCandidateCapabilitiesOn(ctx context.Context, ex queryExec, namespaceID string, installIDs []string) ([]uninstallCapabilityRow, error) {
	rows, err := ex.queryContext(ctx,
		`SELECT id, name, version, installed_from, resource_version FROM capability_versions
		  WHERE namespace_id = ? AND deleted_at IS NULL
		    AND installed_from IN (`+placeholdersFor(len(installIDs))+`)
		  ORDER BY name, version`, append([]any{namespaceID}, anyArgs(installIDs)...)...)
	if err != nil {
		return nil, fmt.Errorf("uninstall playbook: list release capabilities: %w", err)
	}
	return scanUninstallCapabilityRows(rows)
}

func foreignCapabilityRowsOn(ctx context.Context, ex queryExec, namespaceID, name string, installIDs []string) ([]uninstallCapabilityRow, error) {
	rows, err := ex.queryContext(ctx,
		`SELECT id, name, version, installed_from, resource_version FROM capability_versions
		  WHERE namespace_id = ? AND name = ? AND deleted_at IS NULL
		    AND installed_from NOT IN (`+placeholdersFor(len(installIDs))+`)
		  ORDER BY version`, append([]any{namespaceID, name}, anyArgs(installIDs)...)...)
	if err != nil {
		return nil, fmt.Errorf("uninstall playbook: list other capabilities of %q: %w", name, err)
	}
	return scanUninstallCapabilityRows(rows)
}

func scanUninstallCapabilityRows(rows *sql.Rows) ([]uninstallCapabilityRow, error) {
	defer rows.Close()
	out := []uninstallCapabilityRow{}
	for rows.Next() {
		var r uninstallCapabilityRow
		if err := rows.Scan(&r.id, &r.name, &r.version, &r.installedFrom, &r.rv); err != nil {
			return nil, fmt.Errorf("uninstall playbook: scan capability row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func uninstallCandidateSourcesOn(ctx context.Context, ex queryExec, namespaceID string, installIDs []string) ([]*Source, error) {
	rows, err := ex.queryContext(ctx,
		`SELECT `+sourceColumns+` FROM sources
		  WHERE namespace_id = ? AND installed_from IN (`+placeholdersFor(len(installIDs))+`)
		  ORDER BY name`, append([]any{namespaceID}, anyArgs(installIDs)...)...)
	if err != nil {
		return nil, fmt.Errorf("uninstall playbook: list release sources: %w", err)
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

// foreignSourceRowsOn 返回同名源的 installed_from（源名唯一，最多一行）。
func foreignSourceRowsOn(ctx context.Context, ex queryExec, namespaceID, name string, installIDs []string) ([]string, error) {
	rows, err := ex.queryContext(ctx,
		`SELECT installed_from FROM sources
		  WHERE namespace_id = ? AND name = ? AND installed_from NOT IN (`+placeholdersFor(len(installIDs))+`)`,
		append([]any{namespaceID, name}, anyArgs(installIDs)...)...)
	if err != nil {
		return nil, fmt.Errorf("uninstall playbook: list other sources of %q: %w", name, err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var from string
		if err := rows.Scan(&from); err != nil {
			return nil, fmt.Errorf("uninstall playbook: scan source row: %w", err)
		}
		out = append(out, from)
	}
	return out, rows.Err()
}

// sharedReleaseRefsOn 收集「别的仍生效的 release 记为 skipped（复用）」的引用 → 那个 release。
//
// 只读 uninstalled_at = 0 的回执，并排除本 release 自己的那批：同 release 后续安装记下的 skipped 是复用自己上一轮的行，撤销它正是本次要做的。
func sharedReleaseRefsOn(ctx context.Context, ex queryExec, namespaceID string, installIDs []string) (map[string]string, map[string]string, error) {
	caps, srcs := map[string]string{}, map[string]string{}
	rows, err := ex.queryContext(ctx,
		`SELECT release, receipt FROM playbook_installs
		  WHERE namespace_id = ? AND uninstalled_at = 0
		    AND id NOT IN (`+placeholdersFor(len(installIDs))+`)`, append([]any{namespaceID}, anyArgs(installIDs)...)...)
	if err != nil {
		return nil, nil, fmt.Errorf("uninstall playbook: list other release receipts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var release, raw string
		if err := rows.Scan(&release, &raw); err != nil {
			return nil, nil, fmt.Errorf("uninstall playbook: scan other release receipt: %w", err)
		}
		var receipt playbook.Receipt
		if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
			return nil, nil, fmt.Errorf("uninstall playbook: decode install receipt: %w", err)
		}
		normalizePinned(&receipt)
		for _, ref := range receipt.Skipped.Capabilities {
			if ref = strings.TrimSpace(ref); ref != "" {
				caps[ref] = release
			}
		}
		for _, name := range receipt.Skipped.Sources {
			if name = strings.TrimSpace(name); name != "" {
				srcs[name] = release
			}
		}
	}
	return caps, srcs, rows.Err()
}

// maxVisibleCapabilityVersionOn 取某名字当前可见的最高版本（无可见行时 0）。
func maxVisibleCapabilityVersionOn(ctx context.Context, ex queryExec, namespaceID, name string) (int, error) {
	var v sql.NullInt64
	if err := ex.queryRowContext(ctx,
		`SELECT MAX(version) FROM capability_versions
		  WHERE namespace_id = ? AND name = ? AND deleted_at IS NULL`, namespaceID, name).Scan(&v); err != nil {
		return 0, fmt.Errorf("uninstall playbook: max version of %q: %w", name, err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

// stringInSlice 判断字符串在切片里（回执 id 的 pin 校验）。
func stringInSlice(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
