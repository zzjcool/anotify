package store

// 预案实例的分组读数（#548）：一个 release 的折叠组，每组带回它自己的回执原文。
//
// 与 ListPlaybookInstalls 同一条折叠键 `(release, valuesDigest)`，区别有两点：
// 按 release 收窄，且把回执本身交出去 —— release 详情页要按参数分组列出「这一组装出了
// 什么、依赖当时解析到什么」，而折叠后的名字并集已经把「哪一组装出了什么」抹平了。
//
// 集合只由回执决定（与导出 / 卸载同一判据）：没有回执 → ErrPlaybookReleaseNotFound；
// 回执全部已标记卸载 → Uninstalled（调用方据此回与「不存在」同一个回答）。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/playbook"
)

// PlaybookReleaseGroup 是一个参数组：同一个 release 用同一套 values 装过的那些回执。
type PlaybookReleaseGroup struct {
	// ValuesDigest 是这组 values 的摘要（折叠键的后半）。
	ValuesDigest string
	// Values 是回执里那份打码后的有效 values（writeOnly 已被替换成 "***"）。
	Values map[string]any
	// Attempts 是组内回执数（装了几次，不是版本号）。
	Attempts int
	// FirstAt / LastAt 是组内最早 / 最近一次安装的时刻（RFC3339）。
	FirstAt string
	LastAt  string
	// LastInstallID 是组内最新一次 install 的回执 id。
	LastInstallID string
	// Receipts 是组内回执原文（安装顺序）；对象与依赖都从这里派生。
	Receipts []playbook.Receipt
}

// PlaybookReleaseGroups 是 `GET …/releases/{release}` 的事实输入。
type PlaybookReleaseGroups struct {
	Release string
	// Groups 按 LastAt 降序（与实例清单同序：第一组是最新在跑的那一套）。
	Groups []PlaybookReleaseGroup
	// Uninstalled 为真表示每一条回执都已被标记卸载（对象已删）。
	Uninstalled bool
}

// GetPlaybookReleaseGroups 按 (ns, slug, release) 读回执并折叠成参数组。
// release 归一化与形状校验走 playbook.NormalizeRelease（与 install / uninstall / export 同口径）。
func (d *DB) GetPlaybookReleaseGroups(ctx context.Context, namespaceID, slug, release string) (*PlaybookReleaseGroups, error) {
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
	receipts, err := d.playbookReleaseReceipts(ctx, namespaceID, row.ID, rel)
	if err != nil {
		return nil, err
	}
	if len(receipts) == 0 {
		return nil, fmt.Errorf("%w: release %q", ErrPlaybookReleaseNotFound, rel)
	}
	out := &PlaybookReleaseGroups{Release: rel, Groups: []PlaybookReleaseGroup{}, Uninstalled: true}
	byDigest := map[string]int{}
	// 每组最近一次安装的排序键：(created_at, rowid)。同一秒内的两次安装 created_at 相等，
	// 只比时间戳会让「哪一组是现在在跑的那套」随机化 —— 与实例清单同一条 tie-break。
	lastKey := map[string][2]int64{}
	for _, rc := range receipts {
		if !rc.uninstalled {
			out.Uninstalled = false
		}
		at := time.Unix(rc.createdAt, 0).UTC().Format(time.RFC3339)
		idx, ok := byDigest[rc.receipt.ValuesDigest]
		if !ok {
			byDigest[rc.receipt.ValuesDigest] = len(out.Groups)
			lastKey[rc.receipt.ValuesDigest] = [2]int64{rc.createdAt, rc.seq}
			out.Groups = append(out.Groups, PlaybookReleaseGroup{
				ValuesDigest:  rc.receipt.ValuesDigest,
				Values:        rc.values,
				FirstAt:       at,
				LastAt:        at,
				LastInstallID: rc.receipt.Install.ID,
				Receipts:      []playbook.Receipt{rc.receipt},
			})
			continue
		}
		g := &out.Groups[idx]
		g.LastAt = at
		g.LastInstallID = rc.receipt.Install.ID
		lastKey[rc.receipt.ValuesDigest] = [2]int64{rc.createdAt, rc.seq}
		g.Receipts = append(g.Receipts, rc.receipt)
	}
	for i := range out.Groups {
		out.Groups[i].Attempts = len(out.Groups[i].Receipts)
	}
	// 按最近一次安装降序：第一组就是「现在在跑的那一套」，页面默认展开它。
	order := make([]int, len(out.Groups))
	for i := range order {
		order[i] = i
	}
	key := func(i int) [2]int64 { return lastKey[out.Groups[i].ValuesDigest] }
	sort.SliceStable(order, func(i, j int) bool {
		a, b := key(order[i]), key(order[j])
		if a[0] != b[0] {
			return a[0] > b[0]
		}
		return a[1] > b[1]
	})
	sorted := make([]PlaybookReleaseGroup, 0, len(order))
	for _, i := range order {
		sorted = append(sorted, out.Groups[i])
	}
	out.Groups = sorted
	return out, nil
}

// playbookReleaseReceiptRow 是一条回执行的读结果（回执 + 折叠/排序需要的列）。
type playbookReleaseReceiptRow struct {
	receipt     playbook.Receipt
	values      map[string]any
	createdAt   int64
	seq         int64
	uninstalled bool
}

// playbookReleaseReceipts 按 (ns, 包 id, release) 取全部回执（安装顺序，含已卸载的）。
//
// 已卸载的行**不能**在这里过滤掉：全部已卸载要能与「一条回执都没有」区分开
// （前者是 404 release_not_found 的第二种来源，后者也是 404，但语义不同）。
func (d *DB) playbookReleaseReceipts(ctx context.Context, namespaceID, playbookID, release string) ([]playbookReleaseReceiptRow, error) {
	rows, err := d.cachedQueryContext(ctx,
		`SELECT receipt, "values", created_at, rowid, uninstalled_at FROM playbook_installs
		  WHERE namespace_id = ? AND playbook_id = ? AND release = ?
		  ORDER BY created_at, rowid`,
		namespaceID, playbookID, release)
	if err != nil {
		return nil, fmt.Errorf("playbook release detail: list receipts: %w", err)
	}
	defer rows.Close()
	out := []playbookReleaseReceiptRow{}
	for rows.Next() {
		var (
			raw, rawValues string
			createdAt, seq int64
			uninstalledAt  int64
		)
		if err := rows.Scan(&raw, &rawValues, &createdAt, &seq, &uninstalledAt); err != nil {
			return nil, fmt.Errorf("playbook release detail: scan receipt: %w", err)
		}
		var receipt playbook.Receipt
		if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
			return nil, fmt.Errorf("playbook release detail: decode receipt: %w", err)
		}
		if receipt.Dependencies == nil {
			receipt.Dependencies = []playbook.Dependency{}
		}
		if receipt.Install.Release == "" {
			// 迁移前的回执原文没有 release；列上已回填（与 GetPlaybookInstall 同一处理）。
			receipt.Install.Release = release
		}
		values := map[string]any{}
		if trimmed := strings.TrimSpace(rawValues); trimmed != "" {
			if err := json.Unmarshal([]byte(trimmed), &values); err != nil {
				return nil, fmt.Errorf("playbook release detail: decode values: %w", err)
			}
			if values == nil {
				values = map[string]any{}
			}
		}
		out = append(out, playbookReleaseReceiptRow{
			receipt:     receipt,
			values:      values,
			createdAt:   createdAt,
			seq:         seq,
			uninstalled: uninstalledAt != 0,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("playbook release detail: iterate receipts: %w", err)
	}
	return out, nil
}
