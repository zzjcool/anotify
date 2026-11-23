package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/playbook"
)

// 按快照重装（#561）：把一份**已卸载**实例照安装时快照（#551 的 pinned）装回去。与首次安装的差别只有两点：
// 正文来源是 pinned 版本（能力取 `(name, pinned.version)` 那行——卸载只 SET deleted_at、正文还在）；
// 源只能复用（源正文不在回执里，缺了就报错，不假装能重建）。单事务与回执形状逐字同首次安装。
var (
	// ErrPlaybookAlreadyInstalled 表示这一组参数当前仍在生效（回执未卸载）→ 409 already_installed。
	ErrPlaybookAlreadyInstalled = errors.New("store: playbook release values still installed")
	// ErrPlaybookPinnedGone 表示某个 pinned 能力版本的正文已被物理回收 → 409 pinned_gone。
	ErrPlaybookPinnedGone = errors.New("store: pinned capability version no longer exists")
	// ErrPlaybookRestoreConflict 表示同名同版本的能力已存在且正文不同 → 409 restore_conflict。
	ErrPlaybookRestoreConflict = errors.New("store: restore target exists with different content")
	// ErrPlaybookPinMissingSource 表示某个 pinned 源在命名空间里不存在 → 409 pin_missing_source。
	ErrPlaybookPinMissingSource = errors.New("store: pinned source is missing")
)

// PlaybookInstallAuditRestored 是「按快照重装」的审计动作（闭集：本表只放这一种）。
const PlaybookInstallAuditRestored = "install.restored"

// 回执 pinned 的 kind 取值（与 `pinnedRefsOn` 写入的两类一致）。
const (
	pinnedRefKindCapability = "capability"
	pinnedRefKindSource     = "source"
)

// PlaybookRestoreCause 是一条「哪个对象、为什么复原不了」：`reason` 与 409 的 reason 同字面，
// `version` 只有能力有（源没有版本概念）。
type PlaybookRestoreCause struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Version int    `json:"version,omitempty"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// PlaybookRestoreError 把「哪一类复原失败」与「逐对象的 causes」放在一起（只说 409 字面等于没有线索）。
type PlaybookRestoreError struct {
	Err    error
	Causes []PlaybookRestoreCause
}

func (e *PlaybookRestoreError) Error() string { return e.Err.Error() }

func (e *PlaybookRestoreError) Unwrap() error { return e.Err }

// PlaybookRestoreCounts 是一次复原里「重建了什么 / 复用了什么」（审计行与 `restores[]` 的读面）。
type PlaybookRestoreCounts struct {
	Capabilities []string `json:"capabilities"` // `name@version`（与回执 created / skipped 同形）
	Sources      []string `json:"sources"`
}

// PlaybookRestoreAuditDetail 是 `playbook_install_audit.detail` 的形状。
type PlaybookRestoreAuditDetail struct {
	RestoredFrom   string                `json:"restoredFrom"`
	Recreated      PlaybookRestoreCounts `json:"recreated"`
	Reused         PlaybookRestoreCounts `json:"reused"`
	PinnedVersions []playbook.PinnedRef  `json:"pinnedVersions"`
}

// PlaybookInstallAuditEntry 是 `playbook_install_audit` 的一行（append-only：没有改/删入口）。
type PlaybookInstallAuditEntry struct {
	ID           string
	NamespaceID  string
	PlaybookID   string
	PlaybookSlug string
	Release      string
	InstallID    string
	Action       string
	ActorID      string
	Detail       json.RawMessage
	CreatedAt    int64
}

// restoreCandidates 是一次复原的判定结果：要重建的能力行、要复用的对象、以及阻断写的那批问题。
type restoreCandidates struct {
	createCaps []*CapabilityVersion
	reused     playbook.Created
	gone       []PlaybookRestoreCause
	missing    []PlaybookRestoreCause
	conflicts  []PlaybookRestoreCause
}

// RestorePlaybookRelease 照 pinned 快照把一份已卸载实例装回去（单事务；冲突 / 缺失整包 0 写）。
// 顺序固定（判据先于写）：读包 → 取该参数组的回执 → already? → 取快照 → 逐对象判定 → 有问题就在
// 第一条 INSERT 之前返回 → 建能力 → 写回执 + 快照 → 写审计 → COMMIT。
func (d *DB) RestorePlaybookRelease(ctx context.Context, namespaceID, slug, release, valuesDigest, actorID string) (*playbook.Receipt, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, ErrNotFound
	}
	digest := strings.TrimSpace(valuesDigest)
	if digest == "" {
		return nil, fmt.Errorf("restore playbook %q: valuesDigest 为空（调用方必须先拒 400）", slug)
	}
	rel, err := playbook.NormalizeRelease(release, slug)
	if err != nil {
		return nil, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("restore playbook: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	row, err := getPlaybookOn(ctx, ex, namespaceID, slug)
	if err != nil {
		return nil, err
	}
	source, err := restoreSourceReceiptOn(ctx, ex, namespaceID, row.ID, rel, digest)
	if err != nil {
		return nil, err
	}
	pins, err := installPinnedRefsOn(ctx, ex, namespaceID, source.id)
	if err != nil {
		return nil, err
	}
	if len(pins) == 0 {
		// 迁移 8 之前的回执没有快照：明说，不猜（拿当前版本凑一份出来就是「假装是复原」）。
		return nil, fmt.Errorf("%w: release %q valuesDigest %s", ErrPlaybookSnapshotMissing, rel, digest)
	}
	cand, err := d.restoreCandidatesOn(ctx, ex, namespaceID, pins)
	if err != nil {
		return nil, err
	}
	if err := cand.err(rel); err != nil {
		return nil, err
	}

	nsSlug, err := getNamespaceSlugOn(ctx, ex, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("restore playbook: %w", err)
	}
	now := Now()
	installID := newID("ins")
	for _, snap := range cand.createCaps {
		a := snap.Artifact()
		if err := insertInstalledCapabilityOn(ctx, ex, namespaceID, installID, playbook.PlannedCapability{
			Action: playbook.ActionCreate, Artifact: &a,
		}); err != nil {
			return nil, fmt.Errorf("restore playbook %q: %w", slug, err)
		}
	}

	receipt := restoreReceiptOf(row, rel, digest, installID, now, nsSlug, source, cand, pins)
	if err := insertPlaybookInstallOn(ctx, ex, namespaceID, row, &receipt, now); err != nil {
		return nil, fmt.Errorf("restore playbook %q: %w", slug, err)
	}
	if err := insertPlaybookRestoreAuditOn(ctx, ex, namespaceID, row, rel, installID, actorID, now,
		restoreAuditDetail(cand, pins, source.id)); err != nil {
		return nil, fmt.Errorf("restore playbook %q: %w", slug, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("restore playbook: commit: %w", err)
	}
	return &receipt, nil
}

// restoreSource 是「照哪一次安装复原」：回执 id、原文与已存库的打码后 values。
type restoreSource struct {
	id      string
	receipt playbook.Receipt
}

// restoreSourceReceiptOn 取该参数组的来源回执：一组 values 装过多次时只有**最后一次**是「当时的样子」，
// 快照以它为准（旧回执的快照是旧版本，照旧的复原会漂移）。
func restoreSourceReceiptOn(ctx context.Context, ex queryExec, namespaceID, playbookID, release, digest string) (*restoreSource, error) {
	rows, err := ex.queryContext(ctx,
		`SELECT id, receipt, uninstalled_at FROM playbook_installs
		  WHERE namespace_id = ? AND playbook_id = ? AND release = ? AND values_digest = ?
		  ORDER BY created_at, rowid`, namespaceID, playbookID, release, digest)
	if err != nil {
		return nil, fmt.Errorf("restore playbook: list group receipts: %w", err)
	}
	defer rows.Close()
	var source *restoreSource
	for rows.Next() {
		var id, raw string
		var uninstalledAt int64
		if err := rows.Scan(&id, &raw, &uninstalledAt); err != nil {
			return nil, fmt.Errorf("restore playbook: scan group receipt: %w", err)
		}
		if uninstalledAt == 0 { // 还在生效：复原等于「再装一遍」，该由 install 端点承接
			return nil, fmt.Errorf("%w: release %q valuesDigest %s", ErrPlaybookAlreadyInstalled, release, digest)
		}
		var receipt playbook.Receipt
		if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
			return nil, fmt.Errorf("restore playbook: decode receipt %s: %w", id, err)
		}
		if receipt.Install.ID == "" {
			receipt.Install.ID = id
		}
		source = &restoreSource{id: id, receipt: receipt}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("restore playbook: iterate group receipts: %w", err)
	}
	if source == nil { // 组不存在与 release 不存在同一落点（与 YAML 按组导出同口径）
		return nil, fmt.Errorf("%w: release %q valuesDigest %s", ErrPlaybookReleaseNotFound, release, digest)
	}
	return source, nil
}

// installPinnedRefsOn 按 install id 读出安装时快照（seq 顺序，与回执 created.pinned 一致）。
func installPinnedRefsOn(ctx context.Context, ex queryExec, namespaceID, installID string) ([]playbook.PinnedRef, error) {
	rows, err := ex.queryContext(ctx,
		`SELECT ref_kind, name, obj_kind, version, digest
		   FROM playbook_install_pinned
		  WHERE namespace_id = ? AND install_id = ?
		  ORDER BY seq, rowid`, namespaceID, installID)
	if err != nil {
		return nil, fmt.Errorf("restore playbook: list pinned refs: %w", err)
	}
	defer rows.Close()
	out := []playbook.PinnedRef{}
	for rows.Next() {
		var pin playbook.PinnedRef
		if err := rows.Scan(&pin.Kind, &pin.Name, &pin.KindOf, &pin.Version, &pin.Digest); err != nil {
			return nil, fmt.Errorf("restore playbook: scan pinned ref: %w", err)
		}
		out = append(out, pin)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("restore playbook: iterate pinned refs: %w", err)
	}
	return out, nil
}

// restoreCandidatesOn 逐对象判定「重建 / 复用 / 报错」，**不写任何行**。
func (d *DB) restoreCandidatesOn(ctx context.Context, ex queryExec, namespaceID string, pins []playbook.PinnedRef) (*restoreCandidates, error) {
	cand := &restoreCandidates{reused: playbook.Created{Capabilities: []string{}, Sources: []string{}}}
	for _, pin := range pins {
		switch pin.Kind {
		case pinnedRefKindCapability:
			if err := cand.capabilityOn(ctx, ex, namespaceID, pin); err != nil {
				return nil, err
			}
		case pinnedRefKindSource:
			if err := cand.sourceOn(ctx, ex, namespaceID, pin); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("restore playbook: 快照里未知的 kind %q（%s）", pin.Kind, pin.Name)
		}
	}
	sort.Slice(cand.createCaps, func(i, j int) bool {
		if cand.createCaps[i].Name != cand.createCaps[j].Name {
			return cand.createCaps[i].Name < cand.createCaps[j].Name
		}
		return cand.createCaps[i].Version < cand.createCaps[j].Version
	})
	sort.Strings(cand.reused.Capabilities)
	sort.Strings(cand.reused.Sources)
	return cand, nil
}

// capabilityOn 判一个 pinned 能力：正文没了 → gone；可见行内容相同 → 复用；可见行同版本不同内容 → 冲突。
func (c *restoreCandidates) capabilityOn(ctx context.Context, ex queryExec, namespaceID string, pin playbook.PinnedRef) error {
	ref := FormatRef(pin.Name, pin.Version)
	body, err := capabilitySnapshotByVersionOn(ctx, ex, namespaceID, pin.Name, pin.Version)
	if errors.Is(err, ErrNotFound) {
		c.gone = append(c.gone, PlaybookRestoreCause{Kind: pin.Kind, Name: pin.Name, Version: pin.Version,
			Reason: "pinned_gone", Message: "kind=capability name=" + pin.Name + " version=" + itoa64(int64(pin.Version)) +
				"：正文已被硬删，无法照快照重建"})
		return nil
	}
	if err != nil {
		return err
	}
	live, err := getCapabilityOn(ctx, ex, namespaceID, pin.Name, pin.Version)
	switch {
	case errors.Is(err, ErrNotFound): // 只剩软删行（卸载过的正常情形）：正文逐字节相同
		c.createCaps = append(c.createCaps, body)
	case err != nil:
		return err
	case live.Digest == pin.Digest:
		c.reused.Capabilities = append(c.reused.Capabilities, ref)
	default:
		c.conflicts = append(c.conflicts, PlaybookRestoreCause{Kind: pin.Kind, Name: pin.Name, Version: pin.Version,
			Reason: "restore_conflict", Message: "kind=capability name=" + pin.Name + " version=" + itoa64(int64(pin.Version)) +
				"：同名同版本已存在且正文不同（库里 " + live.Digest + "，快照 " + pin.Digest + "）"})
	}
	return nil
}

// sourceOn 判一个 pinned 源：同名存在 → 复用（不比对内容：源名是身份）；不存在 → pin_missing_source。
func (c *restoreCandidates) sourceOn(ctx context.Context, ex queryExec, namespaceID string, pin playbook.PinnedRef) error {
	_, err := getSourceByNameOn(ctx, ex, namespaceID, pin.Name)
	switch {
	case errors.Is(err, ErrSourceNotFound):
		c.missing = append(c.missing, PlaybookRestoreCause{Kind: pin.Kind, Name: pin.Name,
			Reason: "pin_missing_source", Message: "kind=source name=" + pin.Name + "：源不存在，且源正文不在回执里（先建源再复原）"})
	case err != nil:
		return err
	default:
		c.reused.Sources = append(c.reused.Sources, pin.Name)
	}
	return nil
}

// err 把判定结果翻成一个 409：先报「数据没了」，再报「你得先建东西」，最后才是「内容对不上」。
func (c *restoreCandidates) err(release string) error {
	for _, group := range []struct {
		err    error
		causes []PlaybookRestoreCause
	}{
		{ErrPlaybookPinnedGone, c.gone},
		{ErrPlaybookPinMissingSource, c.missing},
		{ErrPlaybookRestoreConflict, c.conflicts},
	} {
		if len(group.causes) == 0 {
			continue
		}
		return &PlaybookRestoreError{Err: fmt.Errorf("%w: release %q", group.err, release), Causes: group.causes}
	}
	return nil
}

// capabilitySnapshotByVersionOn 按 name + version 取正文，**不过滤 deleted_at**（卸载只是打标记）；多行软删历史取最近创建的那行。
func capabilitySnapshotByVersionOn(ctx context.Context, ex queryExec, namespaceID, name string, version int) (*CapabilityVersion, error) {
	return scanCapabilityOn(ctx, ex,
		`SELECT `+capabilityColumns+` FROM capability_versions
		  WHERE namespace_id = ? AND name = ? AND version = ?
		  ORDER BY created_at DESC, rowid DESC LIMIT 1`, namespaceID, name, version)
}

// restoreReceiptOf 组装复原回执：与首次安装同形，多一个 restoredFrom；values / valuesDigest / playbook
// 身份沿用来源回执（复原不重新渲染，就没有第二份 values 真相），created 是重建的、skipped 是复用的。
func restoreReceiptOf(row *PlaybookRow, release, digest, installID string, now int64, nsSlug string,
	source *restoreSource, cand *restoreCandidates, pins []playbook.PinnedRef) playbook.Receipt {
	byRef := make(map[string]playbook.PinnedRef, len(pins))
	for _, pin := range pins {
		byRef[pinnedKey(pin.Kind, pin.Name, pin.Version)] = pin
	}
	values := map[string]any{}
	for k, v := range source.receipt.Values {
		values[k] = v
	}
	created := playbook.Created{Capabilities: []string{}, Sources: []string{}, Pinned: []playbook.PinnedRef{}}
	skipped := playbook.Created{Capabilities: []string{}, Sources: []string{}, Pinned: []playbook.PinnedRef{}}
	for _, snap := range cand.createCaps {
		ref := FormatRef(snap.Name, snap.Version)
		created.Capabilities = append(created.Capabilities, ref)
		created.Pinned = append(created.Pinned, playbook.PinnedRef{Kind: pinnedRefKindCapability,
			Name: snap.Name, Version: snap.Version, Digest: snap.Digest})
	}
	for _, ref := range cand.reused.Capabilities {
		name, version, _ := SplitRef(ref)
		skipped.Capabilities = append(skipped.Capabilities, ref)
		skipped.Pinned = append(skipped.Pinned, playbook.PinnedRef{Kind: pinnedRefKindCapability,
			Name: name, Version: version, Digest: byRef[pinnedKey(pinnedRefKindCapability, name, version)].Digest})
	}
	for _, name := range cand.reused.Sources {
		skipped.Sources = append(skipped.Sources, name)
		skipped.Pinned = append(skipped.Pinned, playbook.PinnedRef{Kind: pinnedRefKindSource, Name: name,
			KindOf: byRef[pinnedKey(pinnedRefKindSource, name, 0)].KindOf})
	}
	plan := &playbook.PlanResult{
		Playbook:     source.receipt.Playbook,
		Values:       values,
		ValuesDigest: digest,
		Release:      release,
		Created:      created,
		Skipped:      skipped,
		Dependencies: source.receipt.Dependencies,
	}
	receipt := playbook.BuildReceipt(plan, values, digest, playbook.InstallMeta{
		ID:        installID,
		At:        time.Unix(now, 0).UTC().Format(time.RFC3339),
		Namespace: nsSlug,
		Release:   release,
		Source:    row.Source,
	})
	receipt.RestoredFrom = source.id
	return receipt
}

// pinnedKey 是「按 (kind, name, version) 找快照项」的键（源的 version 恒为 0）。
func pinnedKey(kind, name string, version int) string {
	return kind + "\x00" + name + "\x00" + itoa64(int64(version))
}

// restoreAuditDetail 组装审计行的摘要（计数 + 对象名单，不含 values）。
func restoreAuditDetail(cand *restoreCandidates, pins []playbook.PinnedRef, restoredFrom string) PlaybookRestoreAuditDetail {
	detail := PlaybookRestoreAuditDetail{
		RestoredFrom:   restoredFrom,
		Recreated:      PlaybookRestoreCounts{Capabilities: []string{}, Sources: []string{}},
		Reused:         PlaybookRestoreCounts{Capabilities: cand.reused.Capabilities, Sources: cand.reused.Sources},
		PinnedVersions: pins,
	}
	for _, snap := range cand.createCaps {
		detail.Recreated.Capabilities = append(detail.Recreated.Capabilities, FormatRef(snap.Name, snap.Version))
	}
	if detail.Reused.Capabilities == nil {
		detail.Reused.Capabilities = []string{}
	}
	if detail.Reused.Sources == nil {
		detail.Reused.Sources = []string{}
	}
	if detail.PinnedVersions == nil {
		detail.PinnedVersions = []playbook.PinnedRef{}
	}
	return detail
}

// insertPlaybookRestoreAuditOn 落一行 `install.restored`：与复原同一个事务（被 409 拦下就一行都没有）。
func insertPlaybookRestoreAuditOn(ctx context.Context, ex queryExec, namespaceID string, row *PlaybookRow,
	release, installID, actorID string, now int64, detail PlaybookRestoreAuditDetail) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("restore audit: marshal detail: %w", err)
	}
	if _, err := ex.execContext(ctx,
		`INSERT INTO playbook_install_audit
		   (id, namespace_id, playbook_id, playbook_slug, release, install_id, action, actor_id, detail, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		newID("pau"), namespaceID, row.ID, row.Slug, release, installID,
		PlaybookInstallAuditRestored, actorID, string(raw), now); err != nil {
		return fmt.Errorf("restore audit: %w", err)
	}
	return nil
}

// ListPlaybookInstallAudit 读一个实例的审计行（按时间正序：复原次数是十位量级）。
func (d *DB) ListPlaybookInstallAudit(ctx context.Context, namespaceID, slug, release string) ([]*PlaybookInstallAuditEntry, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.queryContext(ctx,
		`SELECT id, namespace_id, playbook_id, playbook_slug, release, install_id, action, actor_id, detail, created_at
		   FROM playbook_install_audit
		  WHERE namespace_id = ? AND playbook_slug = ? AND release = ?
		  ORDER BY created_at, rowid`, namespaceID, slug, release)
	if err != nil {
		return nil, fmt.Errorf("list playbook install audit: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*PlaybookInstallAuditEntry{}
	for rows.Next() {
		var e PlaybookInstallAuditEntry
		var detail string
		if err := rows.Scan(&e.ID, &e.NamespaceID, &e.PlaybookID, &e.PlaybookSlug, &e.Release,
			&e.InstallID, &e.Action, &e.ActorID, &detail, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan playbook install audit: %w", err)
		}
		e.Detail = json.RawMessage(detail)
		if len(e.Detail) == 0 {
			e.Detail = json.RawMessage(`{}`)
		}
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate playbook install audit: %w", err)
	}
	return out, nil
}
