package server

// release 详情读面（#548）：一份实例装出了什么，一次拿全。归属规则只有一条 ——
// **对象集合由这个 release 的回执 created 决定**，不按名字前缀、不查全命名空间；
// 语义与字段说明的真源在 api/resource.yaml 的该 operation 上。

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/zzjcool/anotify/internal/playbook"
	"github.com/zzjcool/anotify/internal/store"
)

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
	groups, err := a.d.DB.GetPlaybookReleaseGroups(r.Context(), ns, slug, release)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	if groups.Uninstalled {
		writePlaybookAPIError(w, fmt.Errorf("%w: release %q", store.ErrPlaybookReleaseNotFound, groups.Release))
		return
	}
	doc, err := a.releaseDetail(r.Context(), ns, groups)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// releaseDetail 把回执折叠组投影成详情文档（对象取当前生效版本，与按 release 导出 YAML 同口径）。
func (a *playbookAPI) releaseDetail(ctx context.Context, ns string, groups *store.PlaybookReleaseGroups) (playbookReleaseDetail, error) {
	doc := playbookReleaseDetail{
		Playbook:     playbookReleaseRef{Name: groups.PlaybookName, Version: groups.PlaybookVersion},
		Release:      groups.Release,
		Groups:       make([]playbookReleaseParamGroup, 0, len(groups.Groups)),
		Dependencies: []playbook.Dependency{},
		Objects:      playbookReleaseObjects{Capabilities: []playbookReleaseCapObject{}, Sources: []playbookReleaseSourceObject{}},
	}
	caps, err := a.d.DB.ListCapabilities(ctx, ns)
	if err != nil {
		return doc, err
	}
	liveCap := make(map[string]*store.CapabilityVersion, len(caps))
	for _, row := range latestCapabilityVersions(caps) {
		liveCap[row.Name] = row
	}
	srcs, err := a.d.DB.ListSources(ctx, ns)
	if err != nil {
		return doc, err
	}
	liveSrc := make(map[string]*store.Source, len(srcs))
	for _, src := range srcs {
		liveSrc[src.Name] = src
	}
	for i := range groups.Groups {
		group := groups.Groups[i]
		objects, deps := releaseObjectsOf(group.Receipts, liveCap, liveSrc)
		at, lastID := group.LastAt, group.LastInstallID
		doc.Groups = append(doc.Groups, playbookReleaseParamGroup{
			ValuesDigest:  group.ValuesDigest,
			Values:        group.Values,
			Attempts:      group.Attempts,
			InstalledAt:   at,
			LastInstallID: lastID,
			Objects:       objects,
			Dependencies:  deps,
		})
	}

	primary := doc.Groups[0]
	doc.ValuesDigest = primary.ValuesDigest
	doc.InstalledAt = primary.InstalledAt
	doc.LastInstallID = primary.LastInstallID
	doc.Objects = primary.Objects
	doc.Dependencies = primary.Dependencies
	doc.Params = playbookReleaseParams{Digest: primary.ValuesDigest, Groups: len(doc.Groups)}
	return doc, nil
}

func releaseObjectsOf(receipts []playbook.Receipt, caps map[string]*store.CapabilityVersion,
	srcs map[string]*store.Source) (playbookReleaseObjects, []playbook.Dependency) {
	out := playbookReleaseObjects{Capabilities: []playbookReleaseCapObject{}, Sources: []playbookReleaseSourceObject{}}
	capNames, srcNames := playbook.ReleaseObjects(receipts)
	for _, name := range capNames {
		row := caps[name]
		if row == nil {
			continue
		}
		out.Capabilities = append(out.Capabilities, playbookReleaseCapObject{Name: row.Name, Version: row.Version, Digest: row.Digest})
	}
	for _, name := range srcNames {
		row := srcs[name]
		if row == nil {
			continue
		}
		out.Sources = append(out.Sources, playbookReleaseSourceObject{Name: row.Name, Kind: row.Kind})
	}
	deps := []playbook.Dependency{}
	if len(receipts) > 0 {
		last := receipts[len(receipts)-1]
		if last.Dependencies != nil {
			deps = last.Dependencies
		}
	}
	return out, deps
}

type playbookReleaseDetail struct {
	Playbook      playbookReleaseRef          `json:"playbook"`
	Release       string                      `json:"release"`
	ValuesDigest  string                      `json:"valuesDigest"`
	InstalledAt   string                      `json:"installedAt"`
	LastInstallID string                      `json:"lastInstallId"`
	Params        playbookReleaseParams       `json:"params"`
	Objects       playbookReleaseObjects      `json:"objects"`
	Dependencies  []playbook.Dependency       `json:"dependencies"`
	Groups        []playbookReleaseParamGroup `json:"groups"`
}

type playbookReleaseRef struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

type playbookReleaseParams struct {
	Digest string `json:"digest"`
	Groups int    `json:"groups"`
}

type playbookReleaseObjects struct {
	Capabilities []playbookReleaseCapObject    `json:"capabilities"`
	Sources      []playbookReleaseSourceObject `json:"sources"`
}

type playbookReleaseCapObject struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
	Digest  string `json:"digest"`
}

type playbookReleaseSourceObject struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type playbookReleaseParamGroup struct {
	ValuesDigest  string                 `json:"valuesDigest"`
	Values        map[string]any         `json:"values"`
	Attempts      int                    `json:"attempts"`
	InstalledAt   string                 `json:"installedAt"`
	LastInstallID string                 `json:"lastInstallId"`
	Objects       playbookReleaseObjects `json:"objects"`
	Dependencies  []playbook.Dependency  `json:"dependencies"`
}
