package playbook

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/sourcespec"
)

// 测试纪律：不 skip、不弱化断言。每条用例验一条 v4 契约（playbook.md §1 / §2 / §4.2 / §4.5），
// 断言落在**导出行为**上（渲染产物、计划、回执），不靠读私有字段证明。

// testValues 是测试包的 spec.values：覆盖默认值、必填、enum、writeOnly、嵌套对象、数组、
// additionalProperties: false 这七件事。
const testValues = `
    type: object
    additionalProperties: false
    required: [project]
    properties:
      project:
        type: string
        pattern: '^[a-z0-9][a-z0-9-]{0,20}$'
        description: 项目 slug
      env:  { type: string, enum: [dev, staging, prod], default: prod }
      repo: { type: string, default: '' }
      slackToken: { type: string, writeOnly: true }
      replicas: { type: integer, default: 2 }
      nested:
        type: object
        properties:
          cpu: { type: integer, default: 1 }
          mem: { type: string, default: 512Mi }
      tags:
        type: array
        items: { type: string, default: leaked }
`

// reviewDoc 是一份模板化的能力文档（名字/正文/并发都引用 values）。
const reviewDoc = `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: '{{values.project}}-review'
  displayName: '{{values.project}} 审查'
spec:
  params:
    type: object
    properties: { repo: { type: string } }
  next: ['{{values.project}}-fixer']
  run:
    entrypoint: run.sh
    files:
      run.sh: |
        #!/usr/bin/env bash
        echo "project={{values.project}} env={{values.env}}"
        echo "$HOME ${VAR} $(date) {{payload.x}}"
  concurrency: { maxPerNode: '{{values.replicas}}' }
`

// fixerDoc 是终端能力（next: []）。
const fixerDoc = `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: '{{values.project}}-fixer'
spec:
  next: []
  run:
    entrypoint: run.sh
    files:
      run.sh: "#!/usr/bin/env bash\ntrue\n"
`

// onPushDoc 是一份 webhook 源：名字/targets/config 参数都引用 values。
const onPushDoc = `
metadata:
  name: '{{values.project}}-on-push'
spec:
  kind: webhook
  enabled: true
  config:
    token: ''
    payloadSchema:
      type: object
      properties: { repo: { type: string } }
  targets:
    - capability: '{{values.project}}-review'
      params:
        repo: '{{values.repo}}'
`

// bundle 是测试用的包体构造器：把「入口 + 能力/源清单」的拼装收在一处。
type bundle struct {
	slug   string
	values string
	caps   map[string]string
	srcs   map[string]string
	extras map[string]string
	// entryOverride 非空时整份替换 playbook.yaml（测入口形状错误用）。
	entryOverride string
	rawEntry      map[string]string
}

func newBundle() *bundle {
	return &bundle{
		slug:   "project-chain",
		values: testValues,
		caps:   map[string]string{},
		srcs:   map[string]string{},
		extras: map[string]string{},
	}
}

// withStandard 填一份"两能力 + 一源"的标准包（与设计文档 §1 的例子同形）。
func (b *bundle) withStandard() *bundle {
	b.caps["capabilities/review.yaml"] = reviewDoc
	b.caps["capabilities/fixer.yaml"] = fixerDoc
	b.srcs["sources/on-push.yaml"] = onPushDoc
	return b
}

func (b *bundle) cap(path, body string) *bundle    { b.caps[path] = body; return b }
func (b *bundle) source(path, body string) *bundle { b.srcs[path] = body; return b }
func (b *bundle) extra(path, body string) *bundle  { b.extras[path] = body; return b }
func (b *bundle) setValues(values string) *bundle  { b.values = values; return b }

// files 拼出 `{相对路径: 正文}`（含 playbook.yaml）。
func (b *bundle) files() map[string]string {
	out := make(map[string]string, len(b.caps)+len(b.srcs)+len(b.extras)+1)
	for k, v := range b.caps {
		out[k] = v
	}
	for k, v := range b.srcs {
		out[k] = v
	}
	for k, v := range b.extras {
		out[k] = v
	}
	out[EntryFile] = b.entry()
	return out
}

// entry 生成 playbook.yaml（键序固定，便于断言）。
func (b *bundle) entry() string {
	if b.entryOverride != "" {
		return b.entryOverride
	}
	var sb strings.Builder
	sb.WriteString("apiVersion: anotify/v1\nkind: Playbook\nmetadata:\n")
	fmt.Fprintf(&sb, "  name: %s\n", b.slug)
	sb.WriteString("  displayName: 单项目审查链\n  version: 3\n  description: 测试包\n")
	sb.WriteString("spec:\n")
	if b.values != "" {
		sb.WriteString("  values:\n")
		sb.WriteString(indent(b.values, "    "))
	} else {
		sb.WriteString("  values: {}\n")
	}
	for _, key := range []string{"capabilities", "sources"} {
		list := b.capPaths()
		if key == "sources" {
			list = b.srcPaths()
		}
		if len(list) == 0 {
			continue
		}
		fmt.Fprintf(&sb, "  %s:\n", key)
		for _, p := range list {
			fmt.Fprintf(&sb, "    - %s\n", p)
		}
	}
	return sb.String()
}

func (b *bundle) capPaths() []string { return sortedKeys(b.caps) }
func (b *bundle) srcPaths() []string { return sortedKeys(b.srcs) }

// pkg 解析包体，失败即 Fatal。
func (b *bundle) pkg(t *testing.T) *Package {
	t.Helper()
	pkg, err := Parse(b.files())
	if err != nil {
		t.Fatalf("解析包失败: %v", err)
	}
	return pkg
}

// pkgErr 解析包体并返回错误（用于拒绝码断言）。
func (b *bundle) pkgErr(t *testing.T) error {
	t.Helper()
	_, err := Parse(b.files())
	return err
}

// sortedKeys 返回 map 的键（升序）。
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// indent 给一段多行文本加前缀（拼 YAML 用）。
func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		if line == "" {
			lines[i] = ""
			continue
		}
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n") + "\n"
}

// mustRender 渲染，失败即 Fatal。
func mustRender(t *testing.T, pkg *Package, values map[string]any) *RenderResult {
	t.Helper()
	out, err := Render(pkg, values)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	return out
}

// renderErr 渲染并返回错误。
func renderErr(t *testing.T, pkg *Package, values map[string]any) error {
	t.Helper()
	_, err := Render(pkg, values)
	return err
}

// baseValues 是标准包"能装"的那组 values。
func baseValues() map[string]any {
	return map[string]any{"project": "a"}
}

// capNames 取渲染后的能力名（升序）。
func capNames(caps []*artifact.Artifact) []string {
	out := make([]string, 0, len(caps))
	for _, a := range caps {
		out = append(out, a.Metadata.Name)
	}
	sort.Strings(out)
	return out
}

// findCap 按名字找渲染后的能力。
func findCap(caps []*artifact.Artifact, name string) *artifact.Artifact {
	for _, a := range caps {
		if a.Metadata.Name == name {
			return a
		}
	}
	return nil
}

// findSource 按名字找渲染后的源。
func findSource(srcs []Source, name string) *Source {
	for i := range srcs {
		if srcs[i].Name == name {
			return &srcs[i]
		}
	}
	return nil
}

// fakeExisting 是内存版库快照（store 的实现见 G1-5b；这里只喂 Plan 需要的六个查询）。
type fakeExisting struct {
	caps     []*CapabilitySnapshot
	sources  []*SourceSnapshot
	edges    map[string]int
	srcRefs  map[string]int
	upstream map[string]bool
}

func newFakeExisting() *fakeExisting {
	return &fakeExisting{edges: map[string]int{}, srcRefs: map[string]int{}, upstream: map[string]bool{}}
}

// withCap 加一条能力行（foldKey / digest 直接照库里存的样子给）。
func (f *fakeExisting) withCap(name string, version int, foldKey, digest string, next ...string) *fakeExisting {
	f.caps = append(f.caps, &CapabilitySnapshot{
		Name: name, Version: version, FoldKey: foldKey, Digest: digest, Next: next,
	})
	return f
}

// withSource 加一条源行。
func (f *fakeExisting) withSource(name, kind string, cfg string, targets ...sourcespec.Target) *fakeExisting {
	f.sources = append(f.sources, &SourceSnapshot{
		Name: name, Kind: kind, Enabled: true, Config: json.RawMessage(cfg), Targets: targets,
	})
	return f
}

// withAffects 设置裸名引用计数（upgraded[].affects）。
func (f *fakeExisting) withAffects(name string, edges, sources int) *fakeExisting {
	f.edges[name] = edges
	f.srcRefs[name] = sources
	return f
}

// withUpstream 标记库里已有指向 name 的引用。
func (f *fakeExisting) withUpstream(name string) *fakeExisting {
	f.upstream[name] = true
	return f
}

func (f *fakeExisting) CapabilityByFoldKey(name, foldKey string) (*CapabilitySnapshot, bool) {
	for _, c := range f.caps {
		if c.Name == name && c.FoldKey == foldKey {
			return c, true
		}
	}
	return nil, false
}

func (f *fakeExisting) CapabilityByNameVersion(name string, version int) (*CapabilitySnapshot, bool) {
	for _, c := range f.caps {
		if c.Name == name && c.Version == version {
			return c, true
		}
	}
	return nil, false
}

func (f *fakeExisting) CapabilityMaxVersion(name string) (int, bool) {
	max := 0
	for _, c := range f.caps {
		if c.Name == name && c.Version > max {
			max = c.Version
		}
	}
	return max, max > 0
}

func (f *fakeExisting) SourceByName(name string) (*SourceSnapshot, bool) {
	for _, s := range f.sources {
		if s.Name == name {
			return s, true
		}
	}
	return nil, false
}

func (f *fakeExisting) BareNameRefs(name string) (int, int) { return f.edges[name], f.srcRefs[name] }

func (f *fakeExisting) HasUpstream(name string) bool { return f.upstream[name] }

// foldOf 算一份产物在指定版本下的 foldKey（测试里要和库里的行对齐）。
func foldOf(t *testing.T, a *artifact.Artifact) string {
	t.Helper()
	key, err := artifact.FoldKey(a)
	if err != nil {
		t.Fatalf("算 foldKey 失败: %v", err)
	}
	return key
}

// digestOf 算一份产物在指定版本下的 digest。
func digestOf(t *testing.T, a *artifact.Artifact, version int) string {
	t.Helper()
	clone := *a
	clone.Metadata.Version = version
	d, err := artifact.Digest(&clone)
	if err != nil {
		t.Fatalf("算 digest 失败: %v", err)
	}
	return d
}

// ts 是测试里固定的时间戳（回执 at）。
const ts = "2026-11-08T13:20:00Z"
