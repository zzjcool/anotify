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

// 预案（Playbook）领域（v4，playbook.md）：目录包 = 清单 + 回执，不是所有权；本文件是安装包在 store 的唯一落点。三条边界：
// ① 包体是「文件集合」原文：`playbooks.files` 存 `{相对路径: 正文}`，render / install 每次从它 Parse（不另存解析态）；
// ② 安装**只做加法**（I-P1）：只 INSERT 能力行 / 源行 / 边投影 / 回执行，想改已有东西的差异进回执 unapplied[]；
// ③ 回执是**审计**不是锁：每次普通 install 写一行（dryRun 不写），删包不删回执，回执里的 ns / 原 slug / 原包 id 是快照。 12 格动作矩阵与回执形状由纯包 `internal/playbook` 算（Plan / BuildReceipt），本文件只做「读快照 → INSERT create → 写回执」。
// 事务内读一律走 txExec（见 txexec.go）；任何读错误都不允许被当成「库是空的」。

// 预案写路径的哨兵错误。
var (
	// ErrSlugTaken 表示同一命名空间里已有同 slug 的预案（POST 应翻 409 slug_taken）。
	ErrSlugTaken = errors.New("store: playbook slug already used")
	// ErrPlaybookSlugMismatch 表示路径里的 slug 与包内 metadata.name 不一致：包名就在入口文档里（D8：slug 不渲染、不依赖 values），路径只是它的寻址；两者不一致时若照写，`GET /playbooks/<slug>` 读出来的包会自称另一个名字。
	ErrPlaybookSlugMismatch = errors.New("store: playbook slug does not match package metadata.name")
)

// DeletePlaybookNote 是删除响应的固定说明（playbook.md §7：清单不是所有权）。
const DeletePlaybookNote = "预案只是清单，不是所有权；要拆链请直接删能力 / 删源"

// PlaybookRow 是 playbooks 表的一行：一份目录包的清单 + 包体原文。
type PlaybookRow struct {
	ID          string `json:"id"`
	NamespaceID string `json:"namespaceId"`
	// Slug 是包名 / URL 里那段（= 包内 metadata.name；创建后不可改）。
	Slug string `json:"slug"`
	// Name 是 metadata.name（与 Slug 相同；保留两列以对齐 baseline 形状）。
	Name string `json:"name"`
	// DisplayName / Description / Version 是纯展示与溯源（不参与 digest / 幂等）。
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
	Version     int    `json:"version"`
	// Files 是 `{相对路径: 正文}` 的**原始未渲染**包体（含 playbook.yaml）。
	Files map[string]string `json:"files"`
	// Source 是 OCI 来源（`oci://…@sha256:…`）；手传包体时为空。
	Source string `json:"source,omitempty"`
	// ResourceVersion 是写并发令牌（PUT / DELETE 的 CAS）。
	ResourceVersion int64 `json:"resourceVersion"`
	CreatedAt       int64 `json:"createdAt"`
	UpdatedAt       int64 `json:"updatedAt"`
}

// PlaybookVersionEntry 是版本历史的一行（读模型：不回包体原文，只回身份 + 摘要）。 digest 与回执的 `playbook.filesDigest` 同一个算法（playbook.FilesDigest），所以 「这次装的是哪一版」能与版本列表逐字对上，不用人眼比文件。
type PlaybookVersionEntry struct {
	Version     int    `json:"version"`
	FilesDigest string `json:"filesDigest"`
	CreatedAt   string `json:"createdAt"`
	CreatedBy   string `json:"createdBy"`
	// Current 标记这一行就是当前版本（当前版本的真身是 playbooks.files，这张表只留旧的那几份）。
	Current bool `json:"current"`
}

// PlaybookVersionList 是 `GET …/playbooks/{slug}/versions` 的响应体（读模型，不建实例表）。
type PlaybookVersionList struct {
	// Total 是历史行数（= len(versions)；这里不截断，包的历史是十位量级）。
	Total int `json:"total"`
	// CurrentVersion 是当前版本号（`playbooks.version`，读路径仍以 `playbooks.files` 为准）。
	CurrentVersion int                    `json:"currentVersion"`
	Versions       []PlaybookVersionEntry `json:"versions"`
}

// ErrPlaybookVersionNotFound 表示要装的那个版本既不在历史里、也不是当前版本。
var ErrPlaybookVersionNotFound = errors.New("store: playbook version not found")

// PlaybookKept 是删除响应里「留下了什么」：能力写 `name@version`，源写名字。两个列表都来自 installed_from 反查（§7：比「再用 values 渲染一遍」精确）。
type PlaybookKept struct {
	Capabilities []string `json:"capabilities"`
	Sources      []string `json:"sources"`
}

// PlaybookDeleteResult 是 `DELETE …/playbooks/{slug}` 的响应体（§7）。
type PlaybookDeleteResult struct {
	Deleted bool         `json:"deleted"`
	Kept    PlaybookKept `json:"kept"`
	// Values 是这包装过的**有效 values 组**（打码后，按 valuesDigest 去重；来自回执历史）。
	Values []map[string]any `json:"values"`
	Note   string           `json:"note"`
}

// PlaybookInstallGroup 是实例清单（§4.10）的一个折叠组： `(ns, 原包 id, release, valuesDigest)`。一个组 = 一个 release 的一套 values；`attempts` 是这组 values 装了几次（组内回执行数），不是版本号。换一个 release（哪怕 values 一样）是另一套实例，单独立组。
type PlaybookInstallGroup struct {
	// Release 是这组的安装级身份（Helm 语义：一个 release 一套实例）。
	Release      string `json:"release"`
	ValuesDigest string `json:"valuesDigest"`
	// Values 是打码后的有效 values（与回执行一致：writeOnly → "***"）。
	Values map[string]any `json:"values"`
	// FirstAt / LastAt 是组内最早 / 最新一次 install 的时间（RFC3339）。
	FirstAt string `json:"firstAt"`
	LastAt  string `json:"lastAt"`
	// Attempts 是组内回执行数。
	Attempts int `json:"attempts"`
	// LastInstallID 是组内最新一次 install 的回执 id（单回执读取的入口）。
	LastInstallID string `json:"lastInstallId"`
	// Capabilities / Sources 是组内**所有** install id 反查 installed_from 的行的并集。
	Capabilities []string `json:"capabilities"`
	Sources      []string `json:"sources"`
}

// PlaybookInstallList 是 `GET …/installs` 的响应体（读模型，不建实例表）。
type PlaybookInstallList struct {
	// Total 是折叠组总数（可能大于返回条数：超过上限按 lastAt 截断）。
	Total int `json:"total"`
	// Installs 按 lastAt 降序；空列表是 [] 不是 null。
	Installs []PlaybookInstallGroup `json:"installs"`
}

// playbookInstallMaxGroups 是一次实例清单最多返回的组数（§4.10：实例数是十位量级）。
const playbookInstallMaxGroups = 200

// playbookColumns 是 playbooks 的统一列顺序，scanPlaybookRow 依赖它。
const playbookColumns = `id, namespace_id, slug, name, display_name, descr, version, files, source,
	resource_version, created_at, updated_at`

// scanPlaybookRow 把一行 playbooks 解析成 PlaybookRow（files 列解成 map）。
func scanPlaybookRow(row scanner) (*PlaybookRow, error) {
	var (
		p     PlaybookRow
		files string
	)
	if err := row.Scan(&p.ID, &p.NamespaceID, &p.Slug, &p.Name, &p.DisplayName, &p.Description,
		&p.Version, &files, &p.Source, &p.ResourceVersion, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(files), &p.Files); err != nil {
		return nil, fmt.Errorf("decode playbook files: %w", err)
	}
	if p.Files == nil {
		// 空列表纪律：包体永远是 map，不是 null。
		p.Files = map[string]string{}
	}
	return &p, nil
}

// CreatePlaybook 存一份包（POST 语义）：Parse 做值无关的形状校验，同 slug 已存在 → ErrSlugTaken。 slug 为空时取包内 metadata.name；两者都给时必须一致。存储的是 Parse 返回的**原文文件集合**（pkg.Files），render / install 每次重新 Parse。
func (d *DB) CreatePlaybook(ctx context.Context, namespaceID, slug string, files map[string]string, source string) (*PlaybookRow, error) {
	return d.CreatePlaybookAs(ctx, namespaceID, slug, files, source, "")
}

// CreatePlaybookAs 是带**创建者**的 CreatePlaybook（`created_by` 落进版本历史）。 包的第一次落库同时是历史的第 1 行：不清两份写，否则「历史为空 + 当前版本有内容」 这种状态只能靠读侧猜（而猜错的代价是「按当前版本装」被回 404）。
func (d *DB) CreatePlaybookAs(ctx context.Context, namespaceID, slug string, files map[string]string, source, createdBy string) (*PlaybookRow, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	pkg, err := playbook.Parse(files)
	if err != nil {
		return nil, err
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		slug = pkg.Name
	}
	if slug != pkg.Name {
		return nil, fmt.Errorf("%w: 路径 slug %q ≠ 包内 metadata.name %q", ErrPlaybookSlugMismatch, slug, pkg.Name)
	}
	rawFiles, err := json.Marshal(pkg.Files)
	if err != nil {
		return nil, fmt.Errorf("create playbook: marshal files: %w", err)
	}
	now := Now()
	row := &PlaybookRow{
		ID:              NewPlaybookID(),
		NamespaceID:     namespaceID,
		Slug:            slug,
		Name:            pkg.Name,
		DisplayName:     pkg.DisplayName,
		Description:     pkg.Description,
		Version:         pkg.Version,
		Files:           pkg.Files,
		Source:          source,
		ResourceVersion: 1,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("create playbook: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}
	if _, err := ex.execContext(ctx,
		`INSERT INTO playbooks (`+playbookColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		row.ID, row.NamespaceID, row.Slug, row.Name, row.DisplayName, row.Description, row.Version,
		string(rawFiles), row.Source, row.ResourceVersion, row.CreatedAt, row.UpdatedAt); err != nil {
		if isUniqueConstraintError(err) || isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: %q", ErrSlugTaken, slug)
		}
		return nil, fmt.Errorf("create playbook: %w", err)
	}
	if err := recordPlaybookVersionOn(ctx, ex, row.ID, row.Version, pkg.Files, createdBy, now); err != nil {
		return nil, fmt.Errorf("create playbook: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("create playbook: commit: %w", err)
	}
	return row, nil
}

// ListPlaybooksPage 是 ListPlaybooks 的 keyset 分页版（#377）：同一排序 (created_at ASC, id ASC)。`afterID == ""` 表示第一页。
func (d *DB) ListPlaybooksPage(ctx context.Context, namespaceID string, afterAt int64, afterID string, limit int) ([]*PlaybookRow, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	query := `SELECT ` + playbookColumns + ` FROM playbooks WHERE namespace_id = ?`
	args := []any{namespaceID}
	if afterID != "" {
		query += ` AND (created_at > ? OR (created_at = ? AND id > ?))`
		args = append(args, afterAt, afterAt, afterID)
	}
	query += ` ORDER BY created_at, id LIMIT ?`
	args = append(args, normalizePageLimit(limit))
	rows, err := d.cachedQueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list playbooks page: %w", err)
	}
	defer rows.Close()
	out := []*PlaybookRow{}
	for rows.Next() {
		p, err := scanPlaybookRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan playbook row: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPlaybook 按 (namespace, slug) 取一份包（含原始 files；是否存在当前清单由它回答）。未命中返回 ErrNotFound。名字里带 playbook 是为了与命名空间/能力的 Get* 区分。
func (d *DB) GetPlaybook(ctx context.Context, namespaceID, slug string) (*PlaybookRow, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	row := d.cachedQueryRowContext(ctx,
		`SELECT `+playbookColumns+` FROM playbooks WHERE namespace_id = ? AND slug = ?`,
		namespaceID, strings.TrimSpace(slug))
	p, err := scanPlaybookRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get playbook: %w", err)
	}
	return p, nil
}

// ListPlaybooks 列出命名空间内的全部包（按 created_at、id 升序；空列表返回 []）。
func (d *DB) ListPlaybooks(ctx context.Context, namespaceID string) ([]*PlaybookRow, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.cachedQueryContext(ctx,
		`SELECT `+playbookColumns+` FROM playbooks WHERE namespace_id = ?
		  ORDER BY created_at, id`, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("list playbooks: %w", err)
	}
	defer rows.Close()
	out := []*PlaybookRow{}
	for rows.Next() {
		p, err := scanPlaybookRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan playbook row: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ReplacePlaybook 整体替换包体（PUT 语义）：同一信封 + CAS，缺 rv / 旧 rv 稳定哨兵。
// **只换模板**：已装的能力 / 源一行不动，它们要等下一次 install 才会变化（§5 / §10）。
func (d *DB) ReplacePlaybook(ctx context.Context, namespaceID, slug string, expectedVersion string, files map[string]string, source string) (*PlaybookRow, error) {
	return d.ReplacePlaybookAs(ctx, namespaceID, slug, expectedVersion, files, source, "")
}

// ReplacePlaybookAs 是带**修改者**的 ReplacePlaybook。 换包体的同时管版本历史（同一个事务）：内容与库里那份**相同就不写**（反复 PUT 不长历史），不同才写一行（同版本号不同内容 → 覆盖这个版本号那一行，见 schema 块的注释）。
func (d *DB) ReplacePlaybookAs(ctx context.Context, namespaceID, slug string, expectedVersion string, files map[string]string, source, createdBy string) (*PlaybookRow, error) {
	if _, _, err := parseResourceVersion(expectedVersion); err != nil {
		return nil, err
	}
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	pkg, err := playbook.Parse(files)
	if err != nil {
		return nil, err
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		slug = pkg.Name
	}
	if slug != pkg.Name {
		return nil, fmt.Errorf("%w: 路径 slug %q ≠ 包内 metadata.name %q", ErrPlaybookSlugMismatch, slug, pkg.Name)
	}
	rawFiles, err := json.Marshal(pkg.Files)
	if err != nil {
		return nil, fmt.Errorf("replace playbook: marshal files: %w", err)
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("replace playbook: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	row, err := getPlaybookOn(ctx, ex, namespaceID, slug)
	if err != nil {
		return nil, err
	}
	if err := checkResourceVersion(expectedVersion, row.ID, row.ResourceVersion); err != nil {
		return nil, err
	}
	// 「内容变了」的判据是包体自己的 digest（与回执 / 版本列表同一个算法），不看 mtime / rv。
	changed := playbook.FilesDigest(row.Files) != playbook.FilesDigest(pkg.Files)
	now := Now()
	if _, err := ex.execContext(ctx,
		`UPDATE playbooks
		    SET name = ?, display_name = ?, descr = ?, version = ?, files = ?, source = ?,
		        resource_version = resource_version + 1, updated_at = ?
		  WHERE namespace_id = ? AND slug = ? AND id = ? AND resource_version = ?`,
		pkg.Name, pkg.DisplayName, pkg.Description, pkg.Version, string(rawFiles), source, now,
		namespaceID, slug, row.ID, row.ResourceVersion); err != nil {
		return nil, fmt.Errorf("replace playbook: %w", err)
	}
	if changed {
		if err := recordPlaybookVersionOn(ctx, ex, row.ID, pkg.Version, pkg.Files, createdBy, now); err != nil {
			return nil, fmt.Errorf("replace playbook: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("replace playbook: commit: %w", err)
	}
	row.Name = pkg.Name
	row.DisplayName = pkg.DisplayName
	row.Description = pkg.Description
	row.Version = pkg.Version
	row.Files = pkg.Files
	row.Source = source
	row.ResourceVersion++
	row.UpdatedAt = now
	return row, nil
}

//	RenderPlaybook 是 `render` 端点（§2.4）：只读包体 → Parse → Render。**纯只读**：不读安装冲突库、不写库、不产生回执 （D19：render 是 helm template，dryRun 才读库），所以它不可能因为库里已有同名能力而失败，也不会留下任何行；
//
// release 只影响模板渲染（`{{release.name}}`），不读库，所以 render 不判 release 冲突。
func (d *DB) RenderPlaybook(ctx context.Context, namespaceID, slug, release string, values map[string]any) (*playbook.RenderResult, error) {
	return d.RenderPlaybookVersion(ctx, namespaceID, slug, release, 0, values)
}

// RenderPlaybookVersion 是带**版本钉住**的 RenderPlaybook：version <= 0 = 当前版本（逐字同旧行为）。 钉住的版本与 install 走同一个 resolvePlaybookFiles，预览与真装不会用两份不同的文件。
func (d *DB) RenderPlaybookVersion(ctx context.Context, namespaceID, slug, release string, version int, values map[string]any) (*playbook.RenderResult, error) {
	row, err := d.GetPlaybook(ctx, namespaceID, slug)
	if err != nil {
		return nil, err
	}
	files, _, err := resolvePlaybookFilesOn(ctx, d, row, version)
	if err != nil {
		return nil, err
	}
	pkg, err := playbook.Parse(files)
	if err != nil {
		return nil, err
	}
	return playbook.RenderForRelease(pkg, values, release)
}

// InstallPlaybook 在**一个 BEGIN IMMEDIATE 事务**里完成一次安装（§4.4 / §4.6）。
// release 是安装级身份（Helm 语义）：空串 = 缺省 = 包名，同一个包可以有多个 release、各是一套实例；
// **要复用的行是别的 release 装出来的**时 Plan 给出 release_conflict，本函数在写之前就返回（0 写）。顺序固定：
// ① 读包体（事务内，走 tx；`version` 钉住就取历史那一份）
// ② Render + 读库快照（installSnapshot，六个只读查询也走 tx）→ Plan
// ③ 有 conflict → 返回 *playbook.Error（完整 details / valuesDigest / conflicts / hint），0 写
// ④ INSERT 计划里的 **create 条目**（能力行 + next 投影；新源行 + token + targets 投影）
// ⑤ INSERT 回执行（masked values + digest + 回执原文 + ns/原 slug/原包 id 快照）
// ⑥ COMMIT；dryRun 在 ⑤ 之后 Rollback（同一路径，0 持久写）
// 边界（卡上红线，写代码时不要越过去）：- Plan 说 reuse 的条目**一行都不碰**：不能调 CreateCapability / UpdateSource 的折叠更新路径，
// 否则会偷改已存行的 next / displayName / targets（I-P1）。 - 事务内的读错误必须让整次安装失败：把查询失败当「空库」会装出一批本不该存在的行。
// - 不写 deployments / tasks / events dispatch：安装不部署，保留人工部署动作。
func (d *DB) InstallPlaybook(ctx context.Context, namespaceID, slug, release string, values map[string]any, dryRun bool) (*playbook.Receipt, error) {
	return d.InstallPlaybookVersion(ctx, namespaceID, slug, release, 0, values, dryRun)
}

// InstallPlaybookVersion 是带**版本钉住**的 InstallPlaybook：version <= 0 = 当前版本。
// 回执里的 `playbook.version` / `playbook.filesDigest` 记的是**这次真的用的那一份**，
// 所以 v1/v2 各装一份后，两份回执能各自指回自己的版本，不靠人眼比文件。
func (d *DB) InstallPlaybookVersion(ctx context.Context, namespaceID, slug, release string, version int, values map[string]any, dryRun bool) (*playbook.Receipt, error) {
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
		return nil, fmt.Errorf("install playbook: begin tx: %w", err)
	}
	// 所有 return 路径都回滚；普通安装只在最后 Commit 成功后才离开。
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	row, err := getPlaybookOn(ctx, ex, namespaceID, slug)
	if err != nil {
		return nil, err
	}
	files, pinned, err := resolvePlaybookFilesOn(ctx, ex, row, version)
	if err != nil {
		return nil, err
	}
	pkg, err := playbook.Parse(files)
	if err != nil {
		return nil, err
	}
	if pkg.Name != row.Slug {
		return nil, fmt.Errorf("%w: 库里的包 %q 自述 metadata.name %q", ErrPlaybookSlugMismatch, row.Slug, pkg.Name)
	}

	// ② 快照：每个查询的 error 都记录在 installSnapshot 里，Plan 之后统一裁决。
	snap := newInstallSnapshot(ctx, ex, namespaceID)
	plan, planErr := playbook.PlanForRelease(pkg, values, rel, snap)
	if snap.err != nil {
		// 读失败优先于「冲突」：快照不可信时任何判定都不作数（更不能写）。
		return nil, fmt.Errorf("install playbook %q: read snapshot: %w", slug, snap.err)
	}
	if planErr != nil {
		return nil, planErr
	}
	// 回执的包身份记**这次真的用的那一版**（钉住的版本号 + 那份文件的 digest），
	// 而不是包当下的版本：否则按 v1 装的回执会自称 v2。
	plan.Playbook.Version = pinned

	nsSlug, err := getNamespaceSlugOn(ctx, ex, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("install playbook: %w", err)
	}
	now := Now()
	installID := newID("ins")

	// ④ 只写 create 条目：能力在前、源在后（源入口边只对新源写；能力边随行重建）。
	for _, pc := range plan.Capabilities {
		if pc.Action != playbook.ActionCreate {
			continue
		}
		if err := insertInstalledCapabilityOn(ctx, ex, namespaceID, installID, pc); err != nil {
			return nil, fmt.Errorf("install playbook %q: %w", slug, err)
		}
	}
	for _, ps := range plan.Sources {
		if ps.Action != playbook.ActionCreate {
			continue
		}
		if err := d.createInstalledSourceOn(ctx, ex, namespaceID, installID, ps, now); err != nil {
			return nil, fmt.Errorf("install playbook %q: %w", slug, err)
		}
	}

	// ⑤ 回执：values 打码、digest 对打码前算（BuildReceipt 的约定）。
	receipt := playbook.BuildReceipt(plan, plan.Values, plan.ValuesDigest, playbook.InstallMeta{
		ID:        installID,
		At:        time.Unix(now, 0).UTC().Format(time.RFC3339),
		DryRun:    dryRun,
		Namespace: nsSlug,
		Release:   plan.Release,
		Source:    row.Source,
	})
	if err := insertPlaybookInstallOn(ctx, ex, namespaceID, row, &receipt, now); err != nil {
		return nil, fmt.Errorf("install playbook %q: %w", slug, err)
	}

	if dryRun {
		// 同一路径已经执行到「写回执」：Rollback 由 defer 完成，所有表 0 持久写。
		return &receipt, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("install playbook: commit: %w", err)
	}
	return &receipt, nil
}

// DeletePlaybook 只删清单（§7）：CAS 校验在同一事务里，回执 / 已装对象一行不动。
// 响应告诉用户留下了什么：`kept` 由 installed_from 反查，`values` 从回执历史汇总 （都是**打码后**的有效 values，按 valuesDigest 去重）。
func (d *DB) DeletePlaybook(ctx context.Context, namespaceID, slug string, expectedVersion string) (*PlaybookDeleteResult, error) {
	if _, _, err := parseResourceVersion(expectedVersion); err != nil {
		return nil, err
	}
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	slug = strings.TrimSpace(slug)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("delete playbook: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	row, err := getPlaybookOn(ctx, ex, namespaceID, slug)
	if err != nil {
		return nil, err
	}
	if err := checkResourceVersion(expectedVersion, row.ID, row.ResourceVersion); err != nil {
		return nil, err
	}

	kept, err := keptPlaybookObjectsOn(ctx, ex, namespaceID, row.ID)
	if err != nil {
		return nil, err
	}
	values, err := playbookInstallValuesOn(ctx, ex, namespaceID, row.ID)
	if err != nil {
		return nil, err
	}
	if _, err := ex.execContext(ctx,
		`DELETE FROM playbooks WHERE namespace_id = ? AND slug = ? AND id = ? AND resource_version = ?`, namespaceID, slug, row.ID, row.ResourceVersion); err != nil {
		return nil, fmt.Errorf("delete playbook: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("delete playbook: commit: %w", err)
	}
	return &PlaybookDeleteResult{
		Deleted: true,
		Kept:    kept,
		Values:  values,
		Note:    DeletePlaybookNote,
	}, nil
}

// ListPlaybookInstalls 是实例清单（§4.10）：把回执行按 `(namespace_id, 原包 id, release, valuesDigest)` 折叠成「一套 values 装过几次」。
// 已卸载（uninstalled_at != 0）的回执不进清单；currentPlaybookID 是当前清单 id（同 slug 重建即新 id，新包看不到旧实例）；
// 组数上限 200、按 lastAt 降序截断，Total 是折叠组总数。
func (d *DB) ListPlaybookInstalls(ctx context.Context, namespaceID, currentPlaybookID string) (*PlaybookInstallList, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	out := &PlaybookInstallList{Installs: []PlaybookInstallGroup{}}
	if strings.TrimSpace(currentPlaybookID) == "" {
		return out, nil
	}
	rows, err := d.cachedQueryContext(ctx,
		`SELECT id, release, values_digest, "values", created_at, rowid
		   FROM playbook_installs
		  WHERE namespace_id = ? AND playbook_id = ? AND uninstalled_at = 0
		  ORDER BY created_at, rowid`, namespaceID, currentPlaybookID)
	if err != nil {
		return nil, fmt.Errorf("list playbook installs: %w", err)
	}
	defer rows.Close()

	type acc struct {
		at  int64
		seq int64
	}
	byGroup := map[string]*PlaybookInstallGroup{}
	order := make([]string, 0)
	ids := make([]string, 0)
	idGroup := make(map[string]string)
	accs := map[string]*acc{}
	for rows.Next() {
		var (
			id, release, digest, rawValues string
			createdAt, seq                 int64
		)
		if err := rows.Scan(&id, &release, &digest, &rawValues, &createdAt, &seq); err != nil {
			return nil, fmt.Errorf("scan playbook install: %w", err)
		}
		// 折叠键 = (release, valuesDigest)：同一套 values 装 N 次是一组；换 release 就是另一套实例（名字由 `{{release.name}}` 决定）。
		key := release + "\x00" + digest
		g, ok := byGroup[key]
		if !ok {
			parsed := map[string]any{}
			if trimmed := strings.TrimSpace(rawValues); trimmed != "" {
				if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
					return nil, fmt.Errorf("decode install values %s: %w", digest, err)
				}
				if parsed == nil {
					parsed = map[string]any{}
				}
			}
			g = &PlaybookInstallGroup{
				Release:      release,
				ValuesDigest: digest,
				Values:       parsed,
				FirstAt:      time.Unix(createdAt, 0).UTC().Format(time.RFC3339),
				LastAt:       time.Unix(createdAt, 0).UTC().Format(time.RFC3339),
				Capabilities: []string{},
				Sources:      []string{},
			}
			byGroup[key] = g
			order = append(order, key)
			accs[key] = &acc{at: createdAt, seq: seq}
		}
		g.Attempts++
		if createdAt >= accs[key].at {
			accs[key].at = createdAt
			accs[key].seq = seq
			g.LastAt = time.Unix(createdAt, 0).UTC().Format(time.RFC3339)
			g.LastInstallID = id
		}
		ids = append(ids, id)
		idGroup[id] = key
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate playbook installs: %w", err)
	}

	groups := make([]PlaybookInstallGroup, 0, len(order))
	for _, key := range order {
		groups = append(groups, *byGroup[key])
	}
	sort.SliceStable(groups, func(i, j int) bool {
		a := accs[groups[i].Release+"\x00"+groups[i].ValuesDigest]
		b := accs[groups[j].Release+"\x00"+groups[j].ValuesDigest]
		if a.at != b.at {
			return a.at > b.at
		}
		return a.seq > b.seq
	})
	out.Total = len(groups)
	if len(groups) > playbookInstallMaxGroups {
		groups = groups[:playbookInstallMaxGroups]
	}

	if len(ids) == 0 || len(groups) == 0 {
		out.Installs = groups
		return out, nil
	}

	// 只对本页返回的组反查 installed_from（上限 200 组，量是有界的）。
	keepGroup := make(map[string]bool, len(groups))
	groupOf := make(map[string]int, len(groups))
	for i := range groups {
		key := groups[i].Release + "\x00" + groups[i].ValuesDigest
		keepGroup[key] = true
		groupOf[key] = i
	}
	pageIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		if keepGroup[idGroup[id]] {
			pageIDs = append(pageIDs, id)
		}
	}

	// 能力：组内所有 install id 的 installed_from 并集（每个 create 行只记首次那一次）。
	capQuery := `SELECT installed_from, name, version FROM capability_versions
	              WHERE namespace_id = ? AND installed_from IN (` + placeholdersFor(len(pageIDs)) + `)
	              ORDER BY name, version`
	capRows, err := d.cachedQueryContext(ctx, capQuery, append([]any{namespaceID}, anyArgs(pageIDs)...)...)
	if err != nil {
		return nil, fmt.Errorf("list playbook install capabilities: %w", err)
	}
	for capRows.Next() {
		var from, name string
		var version int
		if err := capRows.Scan(&from, &name, &version); err != nil {
			capRows.Close()
			return nil, fmt.Errorf("scan install capability: %w", err)
		}
		if idx, ok := groupOf[idGroup[from]]; ok {
			groups[idx].Capabilities = append(groups[idx].Capabilities, FormatRef(name, version))
		}
	}
	if err := capRows.Err(); err != nil {
		capRows.Close()
		return nil, fmt.Errorf("iterate install capabilities: %w", err)
	}
	capRows.Close()

	srcQuery := `SELECT installed_from, name FROM sources
	              WHERE namespace_id = ? AND installed_from IN (` + placeholdersFor(len(pageIDs)) + `)
	              ORDER BY name`
	srcRows, err := d.cachedQueryContext(ctx, srcQuery, append([]any{namespaceID}, anyArgs(pageIDs)...)...)
	if err != nil {
		return nil, fmt.Errorf("list playbook install sources: %w", err)
	}
	for srcRows.Next() {
		var from, name string
		if err := srcRows.Scan(&from, &name); err != nil {
			srcRows.Close()
			return nil, fmt.Errorf("scan install source: %w", err)
		}
		if idx, ok := groupOf[idGroup[from]]; ok {
			groups[idx].Sources = append(groups[idx].Sources, name)
		}
	}
	if err := srcRows.Err(); err != nil {
		srcRows.Close()
		return nil, fmt.Errorf("iterate install sources: %w", err)
	}
	srcRows.Close()

	out.Installs = groups
	return out, nil
}

// ListPlaybookVersions 是版本历史读模型（`GET …/playbooks/{slug}/versions`）：只回身份与摘要，不回包体原文。
//
// currentVersion 是**当前清单**的版本号（playbooks.version）：当前版本的真身是 playbooks.files，
// 所以历史里那一行只做标记（current=true），读当前包体仍走 GetPlaybook（不 join 这张表）。
// 迁移前建的老包可能一行历史都没有：Total=0 是对的，当前版本依旧能装（resolvePlaybookFilesOn 的第三条分支）。
// 过滤带 namespace：历史表没有 ns 列，但读取仍必须被作用域约束（id 是从本 ns 的行上取的）。
func (d *DB) ListPlaybookVersions(ctx context.Context, namespaceID, playbookID string, currentVersion int) (*PlaybookVersionList, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	out := &PlaybookVersionList{CurrentVersion: currentVersion, Versions: []PlaybookVersionEntry{}}
	if strings.TrimSpace(playbookID) == "" {
		return out, nil
	}
	rows, err := d.cachedQueryContext(ctx,
		`SELECT v.version, v.digest, v.created_at, v.created_by
		   FROM playbook_versions v
		   JOIN playbooks p ON p.id = v.playbook_id AND p.namespace_id = ?
		  WHERE v.playbook_id = ?
		  ORDER BY v.version DESC`, namespaceID, playbookID)
	if err != nil {
		return nil, fmt.Errorf("list playbook versions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			e         PlaybookVersionEntry
			createdAt int64
		)
		if err := rows.Scan(&e.Version, &e.FilesDigest, &createdAt, &e.CreatedBy); err != nil {
			return nil, fmt.Errorf("scan playbook version: %w", err)
		}
		e.CreatedAt = time.Unix(createdAt, 0).UTC().Format(time.RFC3339)
		e.Current = e.Version == currentVersion
		out.Versions = append(out.Versions, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate playbook versions: %w", err)
	}
	out.Total = len(out.Versions)
	return out, nil
}

// placeholdersFor 返回 n 个逗号分隔的 `?`（IN (...) 用）；n<=0 时调用方不该拼 SQL。
func placeholdersFor(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// anyArgs 把驱动不认的 []string 摊成 []any（database/sql 只接受标量参数）。
func anyArgs(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// GetPlaybookInstall 按「历史 ns + 原 slug + installId」读取一次 install 的完整回执。
// **不要求当前清单存在**：回执行的 playbook_id / playbook_slug 是快照，删包之后单条回执仍可按旧 slug 直达（§4.10 / D21）；未命中返回 ErrNotFound。
func (d *DB) GetPlaybookInstall(ctx context.Context, namespaceID, slug, installID string) (*playbook.Receipt, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	var raw, release string
	err := d.cachedQueryRowContext(ctx,
		`SELECT receipt, release FROM playbook_installs
		  WHERE namespace_id = ? AND playbook_slug = ? AND id = ?`,
		namespaceID, strings.TrimSpace(slug), strings.TrimSpace(installID)).Scan(&raw, &release)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get playbook install: %w", err)
	}
	var receipt playbook.Receipt
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
		return nil, fmt.Errorf("decode playbook install receipt: %w", err)
	}
	if receipt.Install.Release == "" {
		// 迁移前写的回执原文里没有 release；列上已经回填成包名。回执是**审计快照**，不改它的原文，只在读的时候把「这次安装属于哪个 release」补齐——否则同一次 安装从实例清单（按列分组）点进来会显示成另一个身份。
		receipt.Install.Release = release
	}
	return &receipt, nil
}
