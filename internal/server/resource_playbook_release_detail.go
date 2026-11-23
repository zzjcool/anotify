package server

// release 详情读面（#548）：一份实例装出了什么，一次拿全。
//
// 为什么要有它：页面原来把「包的状态 / 版本历史 / 按参数折叠的安装记录」混在一页，
// 而一份实例的身份（release + 参数）与它装出的对象之间隔着三个端点（installs +
// capabilities + sources）。前端拼三个请求换来的是一份「名字对、版本可能不对」的视图，
// 并且每个页面的拼法都要各自复述一遍归属规则。归属规则只有一条：
// **对象集合由这个 release 的回执 created 决定**，不按名字前缀、不查全命名空间。

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/zzjcool/anotify/internal/playbook"
	"github.com/zzjcool/anotify/internal/store"
)

// getRelease 处理 GET …/playbooks/{slug}/releases/{release}。
//
// 已卸载（回执都打了 uninstalled_at）与「从未存在」回同一个 404 release_not_found
// （与导出 / 卸载同一口径）：对象已经不在了，给页面一个「有身份但空」的 200 只会让它
// 渲染出一份不存在的实例。
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
	// 读模型（不是资源信封）：它没有 uid / resourceVersion，也没有可写的 spec。
	writeJSON(w, http.StatusOK, doc)
}

// releaseDetail 把回执折叠组投影成详情文档。
//
// 对象取的是**当前生效版本**（与按 release 导出 YAML 同一个口径）：回执记的是安装那一刻
// 的引用，行被升级后详情页与导出必须给出同一份名字 + 版本，否则两处看到的实例不一样。
// 行已经不存在的引用不出现在列表里（不编造一个 version: 0 的幽灵行）。
func (a *playbookAPI) releaseDetail(ctx context.Context, ns string, groups *store.PlaybookReleaseGroups) (playbookReleaseDetail, error) {
	doc := playbookReleaseDetail{
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
	// 顶层字段描述「现在在跑的那一组」（分组按最近安装降序，第一组就是它）。
	// 单组 release 因此与分组明细逐字段一致，页面可以只读顶层。
	primary := doc.Groups[0]
	doc.ValuesDigest = primary.ValuesDigest
	doc.InstalledAt = primary.InstalledAt
	doc.LastInstallID = primary.LastInstallID
	doc.Objects = primary.Objects
	doc.Dependencies = primary.Dependencies
	doc.Params = playbookReleaseParams{Digest: primary.ValuesDigest, Groups: len(doc.Groups)}
	return doc, nil
}

// releaseObjectsOf 把一组回执的 created 名单解析成「此刻真实存在的对象」。
//
// 依赖取组内**最后一条**回执的 dependencies：那是这套 values 最近一次安装时解析到的版本，
// 与单次回执端点读到的一致（历史回执的依赖只在回执里，不再叠加）。
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

// ---------- 响应形状 ----------

// playbookReleaseDetail 是 `GET …/releases/{release}` 的响应体。
//
// 顶层字段是「主分组」（最近安装的那一组），groups 是全部参数组的明细：
// 同一 release 用两套参数装过时，页面用 groups 分组展示，单组时两者一致。
type playbookReleaseDetail struct {
	Release       string                      `json:"release"`
	ValuesDigest  string                      `json:"valuesDigest"`
	InstalledAt   string                      `json:"installedAt"`
	LastInstallID string                      `json:"lastInstallId"`
	Params        playbookReleaseParams       `json:"params"`
	Objects       playbookReleaseObjects      `json:"objects"`
	Dependencies  []playbook.Dependency       `json:"dependencies"`
	Groups        []playbookReleaseParamGroup `json:"groups"`
}

// playbookReleaseParams 是参数面的一行摘要：主分组摘要 + 组数。
type playbookReleaseParams struct {
	Digest string `json:"digest"`
	Groups int    `json:"groups"`
}

// playbookReleaseObjects 是这份实例装出的对象（能力按名字升序，其后源按名字升序）。
type playbookReleaseObjects struct {
	Capabilities []playbookReleaseCapObject    `json:"capabilities"`
	Sources      []playbookReleaseSourceObject `json:"sources"`
}

// playbookReleaseCapObject 是一个能力的当前生效身份（version / digest 与单对象端点同源）。
type playbookReleaseCapObject struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
	Digest  string `json:"digest"`
}

// playbookReleaseSourceObject 是一个源的当前身份（kind 决定它在页面上的跳转与图标）。
type playbookReleaseSourceObject struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// playbookReleaseParamGroup 是一套 values 装出的那一份明细。
//
// values 是回执里那份**已打码**的有效 values（writeOnly → "***"），与实例清单同源。
type playbookReleaseParamGroup struct {
	ValuesDigest  string                 `json:"valuesDigest"`
	Values        map[string]any         `json:"values"`
	Attempts      int                    `json:"attempts"`
	InstalledAt   string                 `json:"installedAt"`
	LastInstallID string                 `json:"lastInstallId"`
	Objects       playbookReleaseObjects `json:"objects"`
	Dependencies  []playbook.Dependency  `json:"dependencies"`
}
