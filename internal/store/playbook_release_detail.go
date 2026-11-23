package store

// 预案实例的分组读数（#548）。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/playbook"
)

type PlaybookReleaseGroup struct {
	ValuesDigest  string
	Values        map[string]any
	Attempts      int
	FirstAt       string
	LastAt        string
	LastInstallID string
	Receipts      []playbook.Receipt
}

type PlaybookReleaseGroups struct {
	PlaybookName    string
	PlaybookVersion int
	Release         string
	Groups          []PlaybookReleaseGroup
	Uninstalled     bool
}

// GetPlaybookReleaseGroups 按 (ns, slug, release) 读回执并折叠成参数组。
// 没有回执 → ErrPlaybookReleaseNotFound；全部已卸载 → Uninstalled。排序键取 (created_at, rowid)。
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
	out := &PlaybookReleaseGroups{PlaybookName: row.Slug, PlaybookVersion: row.Version, Release: rel,
		Groups: []PlaybookReleaseGroup{}, Uninstalled: true}
	byDigest := map[string]int{}
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

type playbookReleaseReceiptRow struct {
	receipt     playbook.Receipt
	values      map[string]any
	createdAt   int64
	seq         int64
	uninstalled bool
}

// playbookReleaseReceipts 取一个 release 的全部回执（安装顺序，含已卸载的）。
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
		normalizePinned(&receipt)
		if receipt.Install.Release == "" {
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
