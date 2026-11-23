package server

// release 详情读面（#548）：**对象集合由这个 release 的回执 created 决定**，不按名字前缀、
// 不查全命名空间；语义与字段说明的真源在 api/resource.yaml 的该 operation 上。

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

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
	// include=uninstalled 让已卸载的实例可读（#553）：缺省仍 404（与导出 / 卸载同一口径）。
	withGone, err := includeUninstalledQuery(r)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	if groups.Uninstalled && !withGone {
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
// 已卸载的实例（#553）：对象已删 → objects 如实为空 + objectsGone，另给卸载时刻与安装时快照。
func (a *playbookAPI) releaseDetail(ctx context.Context, ns string, groups *store.PlaybookReleaseGroups) (playbookReleaseDetail, error) {
	doc := playbookReleaseDetail{
		Playbook:     playbookReleaseRef{Name: groups.PlaybookName, Version: groups.PlaybookVersion},
		Release:      groups.Release,
		Groups:       make([]playbookReleaseParamGroup, 0, len(groups.Groups)),
		Dependencies: []playbook.Dependency{},
		Objects:      playbookReleaseObjects{Capabilities: []playbookReleaseCapObject{}, Sources: []playbookReleaseSourceObject{}},
	}
	var liveCap map[string]*store.CapabilityVersion
	var liveSrc map[string]*store.Source
	if groups.Uninstalled {
		doc.ObjectsGone = true
		doc.UninstalledAt = time.Unix(groups.UninstalledAt, 0).UTC().Format(time.RFC3339)
		pinned, err := a.releasePinnedRefs(ctx, ns, groups.Receipts)
		if err != nil {
			return doc, err
		}
		doc.Pinned = pinned
	} else {
		caps, err := a.d.DB.ListCapabilities(ctx, ns)
		if err != nil {
			return doc, err
		}
		liveCap = make(map[string]*store.CapabilityVersion, len(caps))
		for _, row := range latestCapabilityVersions(caps) {
			liveCap[row.Name] = row
		}
		srcs, err := a.d.DB.ListSources(ctx, ns)
		if err != nil {
			return doc, err
		}
		liveSrc = make(map[string]*store.Source, len(srcs))
		for _, src := range srcs {
			liveSrc[src.Name] = src
		}
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

// releasePinnedRefs 合并全部回执的安装时快照（与 `as=installed` 同口径，#551 D5：同一 (kind,name)
// 后一条覆盖前一条；能力按 name 升序在前、源在后）。老回执 → 空切片：详情是只读视图，不 409。
func (a *playbookAPI) releasePinnedRefs(ctx context.Context, ns string, receipts []playbook.Receipt) ([]playbook.PinnedRef, error) {
	ids := make([]string, 0, len(receipts))
	seen := make(map[string]bool, len(receipts))
	for _, rcpt := range receipts {
		id := strings.TrimSpace(rcpt.Install.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return []playbook.PinnedRef{}, nil
	}
	byInstall, err := a.d.DB.PlaybookInstallPinned(ctx, ns, ids)
	if err != nil {
		return nil, err
	}
	merged := make(map[string]playbook.PinnedRef, len(ids))
	for _, id := range ids {
		for _, pin := range byInstall[id] {
			merged[pin.Kind+"\x00"+pin.Name] = pin
		}
	}
	caps := make([]playbook.PinnedRef, 0, len(merged))
	srcs := make([]playbook.PinnedRef, 0, len(merged))
	for _, pin := range merged {
		if pin.Kind == pinnedRefKindSource {
			srcs = append(srcs, pin)
			continue
		}
		caps = append(caps, pin)
	}
	byName := func(refs []playbook.PinnedRef) {
		sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	}
	byName(caps)
	byName(srcs)
	return append(caps, srcs...), nil
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
	// 下面三个键只在已卸载视图（#553）出现：生效中实例的响应因此逐字节不变。
	UninstalledAt string               `json:"uninstalledAt,omitempty"`
	Pinned        []playbook.PinnedRef `json:"pinned,omitempty"`
	ObjectsGone   bool                 `json:"objectsGone,omitempty"`
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
