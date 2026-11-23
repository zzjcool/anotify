package server

// resource_playbook_release_yaml.go 是「按 release 导出实例装出的对象」的读面（#536）：
// 集合只由回执的 created 决定，每个文档复用单对象 /yaml 端点的投影与脱敏。

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/playbook"
	"github.com/zzjcool/anotify/internal/store"
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
	// `valuesDigest` 可选：把导出钉在某个参数组上（同一 release 装过两套 values 时，
	// 不带筛选的导出是两组的并集，按组导出只给这一组装出的对象）。
	digest := strings.TrimSpace(r.URL.Query().Get("valuesDigest"))
	exp, err := a.d.DB.GetPlaybookReleaseExportGroup(r.Context(), ns, slug, release, digest)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	if exp.Uninstalled {
		// 已卸载 = 对象已物理删除：与「release 不存在」同一个回答。
		writePlaybookAPIError(w, fmt.Errorf("%w: release %q", store.ErrPlaybookReleaseNotFound, exp.Release))
		return
	}
	body, err := a.releaseYAMLBody(r.Context(), ns, slug, exp)
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

// releaseYAMLText 拼出「头注释 + 逐文档」：每段与单对象端点输出逐字节相同。
func releaseYAMLText(slug, release string, last playbook.Receipt, docs [][]byte) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# playbook: %s@%d  release: %s  values-digest: %s\n",
		slug, last.Playbook.Version, release, last.ValuesDigest)
	fmt.Fprintf(&b, "# install: %s  installed-at: %s  objects: %d\n",
		last.Install.ID, last.Install.At, len(docs))
	b.WriteString("# 版本说明：以下对象为「当前生效版本」（不是安装那一刻的快照）\n")
	for _, doc := range docs {
		b.WriteString("---\n")
		b.Write(doc)
		if len(doc) == 0 || doc[len(doc)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}
