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

// 预案（Playbook）领域（v4，playbook.md）：目录包 = 清单 + 回执，不是所有权。
//
// 本文件是安装包在 store 的**唯一**落点，三条边界：
//
//	① 包体是「文件集合」的原文：`playbooks.files` 存 `{相对路径: 正文}`（含 playbook.yaml），
//	   `includeBody` 原样返回它；render / install 每次都从这份原文 Parse（不另存解析态）。
//	② 安装**只做加法**（I-P1）：只 INSERT 能力行 / 源行 / 边投影 / 回执行，不 UPDATE、
//	   不 DELETE 任何已有行。任何「想改已有东西」的差异进回执的 unapplied[]。
//	③ 回执是**审计**不是锁：每次普通 install 写一行（dryRun 一行不写），删包不删回执，
//	   回执里的 namespace / 原 slug / 原包 id 是快照（playbooks 行消失后仍可定位）。
//
// 12 格动作矩阵与回执形状由纯包 `internal/playbook` 算（Plan / BuildReceipt）；
// 本文件只负责「同一事务里读快照 → INSERT 计划里的 create 条目 → 写回执」。
// 事务内读一律走 txExec（见 txexec.go）；任何读错误都不允许被当成「库是空的」。

// 预案写路径的哨兵错误。
var (
	// ErrSlugTaken 表示同一命名空间里已有同 slug 的预案（POST 应翻 409 slug_taken）。
	ErrSlugTaken = errors.New("store: playbook slug already used")
	// ErrPlaybookSlugMismatch 表示路径里的 slug 与包内 metadata.name 不一致。
	//
	// 包名就在入口文档里（D8：slug 不渲染、不依赖 values），路径只是它的寻址。
	// 两者不一致时若照写，`GET /playbooks/<slug>` 读出来的包会自称另一个名字。
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

// PlaybookKept 是删除响应里「留下了什么」：能力写 `name@version`，源写名字。
//
// 两个列表都来自 installed_from 反查（§7：比「再用 values 渲染一遍」精确）。
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

// PlaybookInstallGroup 是实例清单（§4.10）的一个折叠组：`(ns, 原包 id, valuesDigest)`。
//
// 一个组 = 一套 values；`attempts` 是这组 values 装了几次（组内回执行数），不是版本号。
type PlaybookInstallGroup struct {
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

// CreatePlaybook 存一份包（POST 语义）：Parse 做值无关的形状校验，同 slug 已存在 → ErrSlugTaken。
//
// slug 为空时取包内 metadata.name（"若无 slug 传递从 Parse 给"）；两者都给时必须一致。
// 存储的是 Parse 返回的**原文文件集合**（pkg.Files），render / install 每次重新 Parse。
func (d *DB) CreatePlaybook(ctx context.Context, namespaceID, slug string, files map[string]string, source string) (*PlaybookRow, error) {
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
	if _, err := d.ExecContext(ctx,
		`INSERT INTO playbooks (`+playbookColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		row.ID, row.NamespaceID, row.Slug, row.Name, row.DisplayName, row.Description, row.Version,
		string(rawFiles), row.Source, row.ResourceVersion, row.CreatedAt, row.UpdatedAt); err != nil {
		if isUniqueConstraintError(err) || isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: %q", ErrSlugTaken, slug)
		}
		return nil, fmt.Errorf("create playbook: %w", err)
	}
	return row, nil
}

// GetPlaybook 按 (namespace, slug) 取一份包（含原始 files；是否存在当前清单由它回答）。
//
// 未命中返回 ErrNotFound。名字里带 playbook 是为了与命名空间/能力的 Get* 区分。
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
//
// **只换模板**：已装的能力 / 源一行不动，它们要等下一次 install 才会变化（§5 / §10）。
func (d *DB) ReplacePlaybook(ctx context.Context, namespaceID, slug string, expectedVersion string, files map[string]string, source string) (*PlaybookRow, error) {
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

// RenderPlaybook 是 `render` 端点（§2.4）：只读包体 → Parse → Render。
//
// **纯只读**：不读安装冲突库、不写库、不产生回执（D19：render 是 helm template，
// dryRun 才读库）。所以它不可能因为库里已有同名能力而失败，也不会留下任何行。
func (d *DB) RenderPlaybook(ctx context.Context, namespaceID, slug string, values map[string]any) (*playbook.RenderResult, error) {
	row, err := d.GetPlaybook(ctx, namespaceID, slug)
	if err != nil {
		return nil, err
	}
	pkg, err := playbook.Parse(row.Files)
	if err != nil {
		return nil, err
	}
	return playbook.Render(pkg, values)
}

// InstallPlaybook 在**一个 BEGIN IMMEDIATE 事务**里完成一次安装（§4.4 / §4.6）。
//
// 顺序固定：
//
//	① 读包体（事务内，走 tx）
//	② Render + 读库快照（installSnapshot，六个只读查询也走 tx）→ Plan
//	③ 有 conflict → 返回 *playbook.Error（完整 details / valuesDigest / conflicts / hint），0 写
//	④ INSERT 计划里的 **create 条目**（能力行 + next 投影；新源行 + token + targets 投影）
//	⑤ INSERT 回执行（masked values + digest + 回执原文 + ns/原 slug/原包 id 快照）
//	⑥ COMMIT；dryRun 在 ⑤ 之后 Rollback（同一路径，0 持久写）
//
// 边界（卡上红线，写代码时不要越过去）：
//   - Plan 说 reuse 的条目**一行都不碰**：不能调 CreateCapability / UpdateSource 的折叠更新路径，
//     否则会偷改已存行的 next / displayName / targets（I-P1）。
//   - 事务内的读错误必须让整次安装失败：把查询失败当「空库」会装出一批本不该存在的行。
//   - 不写 deployments / tasks / events dispatch：安装不部署，保留人工部署动作。
func (d *DB) InstallPlaybook(ctx context.Context, namespaceID, slug string, values map[string]any, dryRun bool) (*playbook.Receipt, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, ErrNotFound
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
	pkg, err := playbook.Parse(row.Files)
	if err != nil {
		return nil, err
	}
	if pkg.Name != row.Slug {
		return nil, fmt.Errorf("%w: 库里的包 %q 自述 metadata.name %q", ErrPlaybookSlugMismatch, row.Slug, pkg.Name)
	}

	// ② 快照：每个查询的 error 都记录在 installSnapshot 里，Plan 之后统一裁决。
	snap := newInstallSnapshot(ctx, ex, namespaceID)
	plan, planErr := playbook.Plan(pkg, values, snap)
	if snap.err != nil {
		// 读失败优先于「冲突」：快照不可信时任何判定都不作数（更不能写）。
		return nil, fmt.Errorf("install playbook %q: read snapshot: %w", slug, snap.err)
	}
	if planErr != nil {
		return nil, planErr
	}

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
//
// 响应告诉用户留下了什么：`kept` 由 installed_from 反查，`values` 从回执历史汇总
// （都是**打码后**的有效 values，按 valuesDigest 去重）。
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

// ListPlaybookInstalls 是实例清单（§4.10）：把回执行按
// `(namespace_id, 原包 id, valuesDigest)` 折叠成「一套 values 装过几次」。
//
// currentPlaybookID 是**当前清单**的 id：同 slug 删掉重建后是一份新清单（新 id），
// 旧回执仍引用旧 id —— 所以新包看不到旧实例，也不会把旧 group 的来源认成自己的。
// 组数上限 200，按 lastAt 降序截断；Total 是折叠组总数。
func (d *DB) ListPlaybookInstalls(ctx context.Context, namespaceID, currentPlaybookID string) (*PlaybookInstallList, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	out := &PlaybookInstallList{Installs: []PlaybookInstallGroup{}}
	if strings.TrimSpace(currentPlaybookID) == "" {
		return out, nil
	}
	rows, err := d.cachedQueryContext(ctx,
		`SELECT id, values_digest, "values", created_at, rowid
		   FROM playbook_installs
		  WHERE namespace_id = ? AND playbook_id = ?
		  ORDER BY created_at, rowid`, namespaceID, currentPlaybookID)
	if err != nil {
		return nil, fmt.Errorf("list playbook installs: %w", err)
	}
	defer rows.Close()

	type acc struct {
		at  int64
		seq int64
	}
	byDigest := map[string]*PlaybookInstallGroup{}
	order := make([]string, 0)
	ids := make([]string, 0)
	idDigest := make(map[string]string)
	accs := map[string]*acc{}
	for rows.Next() {
		var (
			id, digest, rawValues string
			createdAt, seq        int64
		)
		if err := rows.Scan(&id, &digest, &rawValues, &createdAt, &seq); err != nil {
			return nil, fmt.Errorf("scan playbook install: %w", err)
		}
		g, ok := byDigest[digest]
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
				ValuesDigest: digest,
				Values:       parsed,
				FirstAt:      time.Unix(createdAt, 0).UTC().Format(time.RFC3339),
				LastAt:       time.Unix(createdAt, 0).UTC().Format(time.RFC3339),
				Capabilities: []string{},
				Sources:      []string{},
			}
			byDigest[digest] = g
			order = append(order, digest)
			accs[digest] = &acc{at: createdAt, seq: seq}
		}
		g.Attempts++
		if createdAt >= accs[digest].at {
			accs[digest].at = createdAt
			accs[digest].seq = seq
			g.LastAt = time.Unix(createdAt, 0).UTC().Format(time.RFC3339)
			g.LastInstallID = id
		}
		ids = append(ids, id)
		idDigest[id] = digest
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate playbook installs: %w", err)
	}

	groups := make([]PlaybookInstallGroup, 0, len(order))
	for _, digest := range order {
		groups = append(groups, *byDigest[digest])
	}
	sort.SliceStable(groups, func(i, j int) bool {
		a, b := accs[groups[i].ValuesDigest], accs[groups[j].ValuesDigest]
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
	keepDigest := make(map[string]bool, len(groups))
	groupOf := make(map[string]int, len(groups))
	for i := range groups {
		keepDigest[groups[i].ValuesDigest] = true
		groupOf[groups[i].ValuesDigest] = i
	}
	pageIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		if keepDigest[idDigest[id]] {
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
		if idx, ok := groupOf[idDigest[from]]; ok {
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
		if idx, ok := groupOf[idDigest[from]]; ok {
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
//
// **不要求当前清单存在**：回执行的 playbook_id / playbook_slug 是快照，删包之后
// 单条回执仍可按旧 slug 直达（§4.10 / D21）。未命中返回 ErrNotFound。
func (d *DB) GetPlaybookInstall(ctx context.Context, namespaceID, slug, installID string) (*playbook.Receipt, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	var raw string
	err := d.cachedQueryRowContext(ctx,
		`SELECT receipt FROM playbook_installs
		  WHERE namespace_id = ? AND playbook_slug = ? AND id = ?`,
		namespaceID, strings.TrimSpace(slug), strings.TrimSpace(installID)).Scan(&raw)
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
	return &receipt, nil
}
