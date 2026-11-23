package server

// resource_playbook_release.go 是「一个实例（release）的详情」的读面（#548）：包 → 实例两级化的第二级。
//
// 四个口径：身份取最新一条回执；对象归属只认回执 id（不按名字前缀）；分组按 valuesDigest、最近安装的组在前；
// dependencies 去重取最高 version。口径真源是 api/resource.yaml 的 PlaybookRelease schema description。

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/zzjcool/anotify/internal/playbook"
	"github.com/zzjcool/anotify/internal/store"
)

// getRelease 处理 GET …/playbooks/{slug}/releases/{release}（读模型，不套四键信封，与 listInstalls 同口径）。
func (a *playbookAPI) getRelease(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	slug := strings.TrimSpace(r.PathValue("slug"))
	release := strings.TrimSpace(r.PathValue("release"))
	if slug == "" || release == "" {
		writePlaybookAPIError(w, errStatus(http.StatusBadRequest, reasonBadRequest, "playbook slug and release are required"))
		return
	}
	exp, err := a.d.DB.GetPlaybookReleaseExport(r.Context(), ns, slug, release)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	if exp.Uninstalled || len(exp.Receipts) == 0 {
		writePlaybookAPIError(w, fmt.Errorf("%w: release %q", store.ErrPlaybookReleaseNotFound, exp.Release))
		return
	}
	detail, err := a.releaseDetail(r.Context(), ns, exp)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// releaseDetail 把回执历史 + 存活行投影成详情读模型。
func (a *playbookAPI) releaseDetail(ctx context.Context, ns string, exp *store.PlaybookReleaseExport) (PlaybookRelease, error) {
	if exp == nil || len(exp.Receipts) == 0 {
		return PlaybookRelease{}, fmt.Errorf("%w: release %q", store.ErrPlaybookReleaseNotFound, releaseOf(exp))
	}
	idx, err := a.releaseObjects(ctx, ns)
	if err != nil {
		return PlaybookRelease{}, err
	}
	last := exp.Receipts[len(exp.Receipts)-1]
	groups := releaseGroups(exp.Receipts)
	out := PlaybookRelease{
		Release:       exp.Release,
		ValuesDigest:  last.ValuesDigest,
		InstalledAt:   last.Install.At,
		LastInstallID: last.Install.ID,
		Objects:       idx.objectsOf(exp.Receipts),
		Dependencies:  releaseDependencies(exp.Receipts),
		Params:        PlaybookReleaseParams{Digest: last.ValuesDigest, Count: len(groups)},
		Groups:        make([]PlaybookReleaseGroup, 0, len(groups)),
		Receipt:       last,
	}
	for _, members := range groups {
		newest := members[len(members)-1]
		out.Groups = append(out.Groups, PlaybookReleaseGroup{
			ValuesDigest:  newest.ValuesDigest,
			InstalledAt:   newest.Install.At,
			LastInstallID: newest.Install.ID,
			Attempts:      len(members),
			Objects:       idx.objectsOf(members),
			Dependencies:  releaseDependencies(members),
		})
	}
	return out, nil
}

func releaseOf(exp *store.PlaybookReleaseExport) string {
	if exp == nil {
		return ""
	}
	return exp.Release
}

type releaseObjectIndex struct {
	capRows []*store.CapabilityVersion
	latest  map[string]*store.CapabilityVersion
	srcRows []*store.Source
}

func (a *playbookAPI) releaseObjects(ctx context.Context, ns string) (releaseObjectIndex, error) {
	capRows, err := a.d.DB.ListCapabilities(ctx, ns)
	if err != nil {
		return releaseObjectIndex{}, err
	}
	latest := make(map[string]*store.CapabilityVersion, len(capRows))
	for _, row := range latestCapabilityVersions(capRows) {
		latest[row.Name] = row
	}
	srcRows, err := a.d.DB.ListSources(ctx, ns)
	if err != nil {
		return releaseObjectIndex{}, err
	}
	return releaseObjectIndex{capRows: capRows, latest: latest, srcRows: srcRows}, nil
}

// objectsOf 取出「这批回执装出的对象」：判据是 installed_from ∈ 回执的 install.id，空集合是 []。
func (idx releaseObjectIndex) objectsOf(receipts []playbook.Receipt) PlaybookReleaseObjects {
	owners := make(map[string]bool, len(receipts))
	for i := range receipts {
		if id := receipts[i].Install.ID; id != "" {
			owners[id] = true
		}
	}
	names := map[string]bool{}
	for _, row := range idx.capRows {
		if row == nil || !owners[row.InstalledFrom] {
			continue
		}
		names[row.Name] = true
	}
	out := PlaybookReleaseObjects{
		Capabilities: make([]PlaybookReleaseCapability, 0, len(names)),
		Sources:      make([]PlaybookReleaseSource, 0, len(idx.srcRows)),
	}
	for name := range names {
		row := idx.latest[name]
		if row == nil {
			continue
		}
		out.Capabilities = append(out.Capabilities, PlaybookReleaseCapability{
			Name: row.Name, Version: row.Version, Digest: row.Digest,
		})
	}
	sort.Slice(out.Capabilities, func(i, j int) bool {
		if out.Capabilities[i].Name != out.Capabilities[j].Name {
			return out.Capabilities[i].Name < out.Capabilities[j].Name
		}
		return out.Capabilities[i].Version < out.Capabilities[j].Version
	})
	seen := map[string]bool{}
	for _, row := range idx.srcRows {
		if row == nil || !owners[row.InstalledFrom] || seen[row.Name] {
			continue
		}
		seen[row.Name] = true
		out.Sources = append(out.Sources, PlaybookReleaseSource{Name: row.Name, Kind: row.Kind})
	}
	sort.Slice(out.Sources, func(i, j int) bool { return out.Sources[i].Name < out.Sources[j].Name })
	return out
}

// releaseGroups 按 valuesDigest 分组、最近安装的组在前；组内回执保持安装顺序（升序）。
func releaseGroups(receipts []playbook.Receipt) [][]playbook.Receipt {
	groups := make([][]playbook.Receipt, 0, len(receipts))
	index := make(map[string]int, len(receipts))
	for i := len(receipts) - 1; i >= 0; i-- {
		digest := receipts[i].ValuesDigest
		if at, ok := index[digest]; ok {
			groups[at] = append(groups[at], receipts[i])
			continue
		}
		index[digest] = len(groups)
		groups = append(groups, []playbook.Receipt{receipts[i]})
	}
	for _, members := range groups {
		for i, j := 0, len(members)-1; i < j; i, j = i+1, j-1 {
			members[i], members[j] = members[j], members[i]
		}
	}
	return groups
}

// releaseDependencies 把回执里的依赖按 capability 去重取**最高 version**，按 capability 升序；空集合是 []。
func releaseDependencies(receipts []playbook.Receipt) []playbook.Dependency {
	best := map[string]int{}
	for i := range receipts {
		for _, dep := range receipts[i].Dependencies {
			if version, ok := best[dep.Capability]; !ok || dep.Version > version {
				best[dep.Capability] = dep.Version
			}
		}
	}
	out := make([]playbook.Dependency, 0, len(best))
	for name, version := range best {
		out = append(out, playbook.Dependency{Capability: name, Version: version})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Capability < out[j].Capability })
	return out
}

// PlaybookRelease 是 `GET …/releases/{release}` 的响应体（读模型；六个口径见 api/resource.yaml）。
type PlaybookRelease struct {
	Release       string                 `json:"release"`
	ValuesDigest  string                 `json:"valuesDigest"`
	InstalledAt   string                 `json:"installedAt"`
	LastInstallID string                 `json:"lastInstallId"`
	Objects       PlaybookReleaseObjects `json:"objects"`
	Dependencies  []playbook.Dependency  `json:"dependencies"`
	Params        PlaybookReleaseParams  `json:"params"`
	Groups        []PlaybookReleaseGroup `json:"groups"`
	Receipt       playbook.Receipt       `json:"receipt"`
}

type PlaybookReleaseObjects struct {
	Capabilities []PlaybookReleaseCapability `json:"capabilities"`
	Sources      []PlaybookReleaseSource     `json:"sources"`
}

type PlaybookReleaseCapability struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
	Digest  string `json:"digest"`
}

type PlaybookReleaseSource struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type PlaybookReleaseParams struct {
	Digest string `json:"digest"`
	Count  int    `json:"count"`
}

type PlaybookReleaseGroup struct {
	ValuesDigest  string                 `json:"valuesDigest"`
	InstalledAt   string                 `json:"installedAt"`
	LastInstallID string                 `json:"lastInstallId"`
	Attempts      int                    `json:"attempts"`
	Objects       PlaybookReleaseObjects `json:"objects"`
	Dependencies  []playbook.Dependency  `json:"dependencies"`
}
