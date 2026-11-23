package server

// resource_playbook_release_yaml.go 是「按 release 导出实例装出的对象」的读面（#536）：
// 集合只由回执决定，文档复用单对象 /yaml 的投影与脱敏；`?as=installed`（#551）改走安装时快照。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/playbook"
	"github.com/zzjcool/anotify/internal/store"
)

// 导出视图的两态（#551 D4）：缺省与 current 逐字节等价，installed 走安装时快照。
const (
	releaseYAMLAsCurrent   = "current"
	releaseYAMLAsInstalled = "installed"
)

// pinnedRefKindCapability / pinnedRefKindSource 是回执 pinned 的 kind 取值（#551 D1 / playbook.PinnedRef）。
const (
	pinnedRefKindCapability = "capability"
	pinnedRefKindSource     = "source"
)

// releaseYAML 处理 GET …/playbooks/{slug}/releases/{release}/yaml。
func (a *playbookAPI) releaseYAML(w http.ResponseWriter, r *http.Request) {
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
	// `as` 选视图：空串与 current 同一条路径（逐字节不变）；键名必须是字面量 "as"（parity 按字面量收集）。
	as := strings.TrimSpace(r.URL.Query().Get("as"))
	if as != "" && as != releaseYAMLAsCurrent && as != releaseYAMLAsInstalled {
		writePlaybookAPIError(w, errStatus(http.StatusBadRequest, reasonValueInvalid, "as must be current or installed"))
		return
	}
	// `valuesDigest` 可选：把导出钉在一个参数组上（不带 = 各组并集）；两种 `as` 都先按它选回执。
	digest := strings.TrimSpace(r.URL.Query().Get("valuesDigest"))
	exp, err := a.d.DB.GetPlaybookReleaseExportGroup(r.Context(), ns, slug, release, digest)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	body, err := a.releaseYAMLView(r.Context(), ns, slug, exp, as == releaseYAMLAsInstalled)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	// 裸字节响应（不是 JSON）；错误分支仍是 JSON Status。
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// releaseYAMLView 按视图取正文：installed 不判 uninstalled（卸载只是 SET deleted_at）。
func (a *playbookAPI) releaseYAMLView(ctx context.Context, ns, slug string, exp *store.PlaybookReleaseExport, installed bool) ([]byte, error) {
	if installed {
		return a.releaseYAMLInstalledBody(ctx, ns, slug, exp)
	}
	if exp.Uninstalled {
		// 已卸载 = 对象已物理删除：与「release 不存在」同一个回答（as=current 的口径不变）。
		return nil, fmt.Errorf("%w: release %q", store.ErrPlaybookReleaseNotFound, exp.Release)
	}
	return a.releaseYAMLBody(ctx, ns, slug, exp)
}

// releaseYAMLBody 把回执名单解析成一组文档并拼成多文档 YAML。
func (a *playbookAPI) releaseYAMLBody(ctx context.Context, ns, slug string, exp *store.PlaybookReleaseExport) ([]byte, error) {
	caps, srcs := playbook.ReleaseObjects(exp.Receipts)
	capRows, err := a.d.DB.ListCapabilities(ctx, ns)
	if err != nil {
		return nil, err
	}
	latest := make(map[string]*store.CapabilityVersion, len(capRows))
	for _, row := range latestCapabilityVersions(capRows) {
		latest[row.Name] = row
	}
	srcRows, err := a.d.DB.ListSources(ctx, ns)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]*store.Source, len(srcRows))
	for _, src := range srcRows {
		byName[src.Name] = src
	}

	docs := make([][]byte, 0, len(caps)+len(srcs))
	for _, name := range caps {
		row := latest[name]
		if row == nil {
			// 回执记过、但行已删除：导出给的是当前生效版本，不编造。
			continue
		}
		text, err := artifact.MarshalYAML(capabilityArtifact(row))
		if err != nil {
			return nil, err
		}
		docs = append(docs, text)
	}
	for _, name := range srcs {
		src := byName[name]
		if src == nil {
			continue
		}
		text, err := srcYAMLDocument(src)
		if err != nil {
			return nil, err
		}
		docs = append(docs, text)
	}
	// 头注释取最新一条回执的安装身份。
	return releaseYAMLText(slug, exp.Release, exp.Receipts[len(exp.Receipts)-1], docs), nil
}

// releaseYAMLInstalledBody 是 `as=installed` 的正文（#551 D5/D6）：能力取钉住的那一版正文（含软删行），
// 源按名字取当前正文；取不到就计入缺失、跳过不编造（绝不退化成「取最新版」）。
func (a *playbookAPI) releaseYAMLInstalledBody(ctx context.Context, ns, slug string, exp *store.PlaybookReleaseExport) ([]byte, error) {
	merged, err := a.mergedInstallPinned(ctx, ns, exp)
	if err != nil {
		return nil, err
	}
	// 输出顺序：能力按 name 升序在前，源按 name 升序在后（与 created 的排序口径一致）。
	caps := make([]playbook.PinnedRef, 0, len(merged))
	srcs := make([]playbook.PinnedRef, 0, len(merged))
	for _, pin := range merged {
		switch pin.Kind {
		case pinnedRefKindCapability:
			caps = append(caps, pin)
		case pinnedRefKindSource:
			srcs = append(srcs, pin)
		}
	}
	byName := func(refs []playbook.PinnedRef) {
		sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	}
	byName(caps)
	byName(srcs)

	docs := make([][]byte, 0, len(merged))
	missing := 0
	for _, pin := range caps {
		row, err := a.d.DB.GetCapabilityForSnapshot(ctx, ns, pin.Name, pin.Version)
		switch {
		case errors.Is(err, store.ErrNotFound):
			missing++ // 行被物理回收：不假装知道它当时是什么，也绝不退化成「取最新版」。
			continue
		case err != nil:
			return nil, err
		}
		text, err := artifact.MarshalYAML(capabilityArtifact(row))
		if err != nil {
			return nil, err
		}
		docs = append(docs, text)
	}
	if len(srcs) > 0 {
		srcRows, err := a.d.DB.ListSources(ctx, ns)
		if err != nil {
			return nil, err
		}
		current := make(map[string]*store.Source, len(srcRows))
		for _, src := range srcRows {
			current[src.Name] = src
		}
		for _, pin := range srcs {
			src := current[pin.Name]
			if src == nil {
				missing++ // 卸载会物理删源行（源没有版本、也没有软删墓碑）。
				continue
			}
			text, err := srcYAMLDocument(src)
			if err != nil {
				return nil, err
			}
			docs = append(docs, text)
		}
	}
	return releaseYAMLInstalledText(slug, exp.Release, exp.Receipts[len(exp.Receipts)-1], docs, missing), nil
}

// mergedInstallPinned 按 (kind,name) 合并该 release 全部回执的快照（D5）：按安装顺序（老 → 新）遍历、
// 后一条覆盖前一条；合并后为空（回执都早于 #551）→ ErrPlaybookSnapshotMissing（409 no_snapshot）。
func (a *playbookAPI) mergedInstallPinned(ctx context.Context, ns string, exp *store.PlaybookReleaseExport) ([]playbook.PinnedRef, error) {
	ids := make([]string, 0, len(exp.Receipts))
	seen := make(map[string]bool, len(exp.Receipts))
	for _, rcpt := range exp.Receipts {
		id := strings.TrimSpace(rcpt.Install.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
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
	if len(merged) == 0 {
		return nil, fmt.Errorf("%w: release %q", store.ErrPlaybookSnapshotMissing, exp.Release)
	}
	out := make([]playbook.PinnedRef, 0, len(merged))
	for _, pin := range merged {
		out = append(out, pin)
	}
	return out, nil
}

// releaseYAMLText 拼出「头注释 + 逐文档」：每段与单对象端点输出逐字节相同。
func releaseYAMLText(slug, release string, last playbook.Receipt, docs [][]byte) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# playbook: %s@%d  release: %s  values-digest: %s\n",
		slug, last.Playbook.Version, release, last.ValuesDigest)
	fmt.Fprintf(&b, "# install: %s  installed-at: %s  objects: %d\n",
		last.Install.ID, last.Install.At, len(docs))
	b.WriteString("# 版本说明：以下对象为「当前生效版本」（不是安装那一刻的快照）\n")
	writeYAMLDocs(&b, docs)
	return b.Bytes()
}

// releaseYAMLInstalledText 拼出「安装时快照」的导出：头注释固定 4 行，有缺失时第 5 行（D8）；
// `objects` 只数真写进导出的文档，缺失数单独一行（让「少了什么」可见，而不是静默缩水）。
func releaseYAMLInstalledText(slug, release string, last playbook.Receipt, docs [][]byte, missing int) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# playbook: %s@%d  release: %s  values-digest: %s\n",
		slug, last.Playbook.Version, release, last.ValuesDigest)
	fmt.Fprintf(&b, "# install: %s  installed-at: %s  objects: %d\n",
		last.Install.ID, last.Install.At, len(docs))
	b.WriteString("# 版本说明：以下对象为「安装时快照」（安装那一刻记录的版本，不是当前生效版本）\n")
	b.WriteString("# 源无版本：源按回执记录的名字取当前正文（源没有版本概念）\n")
	if missing > 0 {
		fmt.Fprintf(&b, "# 快照缺失: %d 个对象（安装时记录的对象已不存在，未包含在本导出里）\n", missing)
	}
	writeYAMLDocs(&b, docs)
	return b.Bytes()
}

// writeYAMLDocs 按 `---` 分隔写出每个文档，并保证整个正文以恰好一个换行收尾。
func writeYAMLDocs(b *bytes.Buffer, docs [][]byte) {
	for _, doc := range docs {
		b.WriteString("---\n")
		b.Write(doc)
		if len(doc) == 0 || doc[len(doc)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
}
