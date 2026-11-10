package server

// 本文件是 #333（exact-five OpenAPI）的 **Go 侧门禁**：五份 spec 与真实 mux /
// 真实中间件 / 真实 Go 结构体的双向对账 + 负向变异自测。
//
// # 为什么是 Go 而不是 python
//
// `scripts/check_openapi.py` 只能看 spec 自己（exact-five、归属、operationId、security 方案、
// 错误具名族）。它**看不见 handler**。历史上的漂移都是这一半造成的：
//   - mux 上加了一条路由、spec 没跟上 → 文档少一条端点（按文档写的客户端永远调不到）；
//   - spec 写了一条 mux 上不存在的端点 → 文档在骗人（客户端 404，而门禁全绿）；
//   - spec 声明 `security: [session]`、真实路由却忘了包 session 中间件 → **匿名可写**；
//   - schema 少一个字段 → 按 schema 生成的客户端永远提交不了实现。
//
// 所以这里不「读 spec 和自己比」，而是四份**独立事实**互相对账：
//  1. 静态 AST：`internal/server/*.go`（排除 `_test.go`）里所有 `mux.Handle/HandleFunc`
//     的第一个实参，按 Go 的常量规则求值成真实 pattern；
//  2. 真实 `http.ServeMux`：把上面的 pattern 装成一个探针 mux，用 spec 的
//     (method, path) 去问「谁接住了这个请求」；
//  3. 真实装配：`newProductionAppForTest` + 无凭据请求，实测 spec 声明的鉴权是不是真的；
//  4. 真实 Go 结构体：`reflect` 取 json tag，与 schema 的 properties **双向**比。
//
// # 纪律
//
//   - 静态求值**失败即 Fatal**（"无法静态求值的 pattern 表达式"）：静默跳过一条 pattern
//     就等于给「新增路由永不进 spec」开后门。
//   - 裸 pattern（`/v1/auth/` 这种不带方法的）走**显式表** `bareMountSpec()`：method 由
//     handler 内部 switch 决定，AST 里根本看不到；表里每条都要有「handler 源码里的字符串
//     判据」，判据找不到 = 表和 handler 已经脱节，红。
//   - 例外表（`securityProbeExceptions` / onlyGo / onlySchema）**过期即红**：某条例外不再
//     需要（两边都有了 / 都没有了 / spec 里那条 operation 没了）就失败，防止例外变成永久后门。
//   - 负向变异（`TestOpenAPIParityMutations`）把每一层门禁都打一遍：删 operation、加幽灵
//     operation、复制 operationId、把 session 改成匿名、给 schema 删/加字段、断跨文件 $ref
//     都必须**报错**。变异只在内存里做，绝不写盘。
//   - 本文件里**不许出现 skip**：`TestOpenAPIParityFakeGreenGuard` 会扫自己（见该函数）。
//
// 所有测试函数名都以 `TestOpenAPIParity` 开头：`make contract` 用
// `go test ./internal/server -run TestOpenAPIParity`，一旦改名，"no tests to run"
// 会以 exit 0 静默变绿——FakeGreenGuard 就是为这个存在的。

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/zzjcool/anotify/internal/playbook"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

// ---------- 路径常量 ----------

const (
	// specDir 是五份真源所在目录（相对本包的测试工作目录 internal/server）。
	specDir = "../../api"
	// oldSingleSpec 是 v3 的单文件真源：**必须不存在**（exact-five 的兜底守卫）。
	oldSingleSpec = "../../api/openapi.yaml"
	// serverPkgDir 是静态 AST 扫描目录（就是本包目录）。
	serverPkgDir = "."
	// selfFileName 是本文件（FakeGreenGuard 自扫用）。
	selfFileName = "openapi_parity_test.go"
)

// specFileNames 是 exact-five 的**唯一**合法集合（顺序即失败信息里的顺序）。
var specFileNames = []string{
	"resource.yaml",
	"events.yaml",
	"agent.yaml",
	"ingress.yaml",
	"auth.yaml",
}

// maxRefDepth 是 $ref 链的环上限（真实 spec 里层层数 ≤ 3；死循环比报错贵得多）。
const maxRefDepth = 8

// ---------- 测试入口 ----------

// TestOpenAPIParitySpecLoadAndRefs 守 exact-five 本身：五份文件都能解析出 operation、
// `$ref`（含跨文件与 components.pathItems）全都能解开、旧单文件不存在。
func TestOpenAPIParitySpecLoadAndRefs(t *testing.T) {
	docs, problems := loadSpecDocs()
	fatalIfProblems(t, "载入五份 spec", problems)

	if _, err := os.Stat(oldSingleSpec); !os.IsNotExist(err) {
		t.Fatalf("exact-five 兜底守卫：%s 必须不存在（v3 单文件真源已删除；err=%v）", oldSingleSpec, err)
	}

	// 每份文件的基本骨架：info / servers / tags / paths / components。
	for _, file := range specFileNames {
		doc, ok := docs[file]
		if !ok {
			t.Fatalf("%s 没载入", file)
		}
		for _, key := range []string{"info", "servers", "tags", "paths", "components"} {
			if _, ok := doc[key]; !ok {
				t.Errorf("%s 缺顶层 %q —— 五份真源都必须有 info/servers/tags/paths/components", file, key)
			}
		}
	}

	fatalIfProblems(t, "解析 operation", specProblems(docs))
	fatalIfProblems(t, "解析 $ref（含跨文件）", refIntegrityProblems(docs))

	ops, _ := collectSpecOps(docs)
	if len(ops) != 124 {
		t.Errorf("五份 spec 的 operation 总数 = %d，契约（docs/v4/g3c-spec-inventory.md）冻结的是 124", len(ops))
	}
	// 至少一条来自 components.pathItems（agent.yaml 三条 + events/ingress 的共享 pathItem）。
	fromPathItems := 0
	for _, op := range ops {
		if op.FromPathItem {
			fromPathItems++
		}
	}
	if fromPathItems == 0 {
		t.Error("没有任何 operation 来自 components.pathItems —— 说明 pathItem $ref 解析没生效（静默丢 operation）")
	}
	// 匿名 operation 必须显式写 security: []（otherwise 「忘了写」与「匿名」不可区分）。
	for _, op := range ops {
		if !op.SecurityDeclared {
			t.Errorf("%s %s（%s，operationId=%s）没有显式声明 security —— 契约要求每条 operation 都写（匿名写 security: []）",
				op.Method, op.Path, op.File, op.OperationID)
		}
	}
}

// TestOpenAPIParityMuxToSpec 守「mux 有、spec 缺」：静态收集全部真实注册，
// 带方法的 pattern 必须在 spec 里恰好出现一次；裸 pattern 交给 bareMountSpec。
func TestOpenAPIParityMuxToSpec(t *testing.T) {
	patterns, problems := collectMuxPatterns(serverPkgDir)
	fatalIfProblems(t, "静态收集 mux pattern（求值失败必须报错，不允许跳过）", problems)
	if len(patterns) == 0 {
		t.Fatal("静态收集到 0 条 pattern —— AST 收集器本身坏了")
	}
	docs, loadProblems := loadSpecDocs()
	fatalIfProblems(t, "载入五份 spec", loadProblems)
	ops, specProblems := collectSpecOps(docs)
	fatalIfProblems(t, "解析 operation", specProblems)

	fatalIfProblems(t, "mux → spec 方向", muxToSpecProblems(patterns, ops))

	// 探针自检：上面那张表必须真的能装进 http.ServeMux（重复 pattern 会 panic）。
	if _, p := newProbeMux(patterns); len(p) > 0 {
		t.Fatalf("静态收集的 pattern 装不进真实 ServeMux：%v", p)
	}
}

// TestOpenAPIParitySpecToMux 守「spec 有、mux 无」：每个 spec operation 用样本值打进
// 探针 mux，命中的 pattern 必须就是它自己（或它在 bareMountSpec 里登记的裸挂载）。
func TestOpenAPIParitySpecToMux(t *testing.T) {
	patterns, problems := collectMuxPatterns(serverPkgDir)
	fatalIfProblems(t, "静态收集 mux pattern", problems)
	docs, loadProblems := loadSpecDocs()
	fatalIfProblems(t, "载入五份 spec", loadProblems)
	ops, specProblems := collectSpecOps(docs)
	fatalIfProblems(t, "解析 operation", specProblems)

	fatalIfProblems(t, "spec → mux 方向", specToMuxProblems(patterns, ops))

	// 特别覆盖：跨文件同 path 不同 method 的两条 operation 必须各自命中、互不顶掉。
	for _, key := range [][2]string{
		{"GET", "/v1/namespaces/{namespace}/events"},
		{"POST", "/v1/namespaces/{namespace}/events"},
		{"PUT", "/v1/node/tasks/{taskId}/artifacts/{sha256}"},
		{"GET", "/v1/node/stream"},
	} {
		mux, _ := newProbeMux(patterns)
		pat := probeMatch(mux, key[0], samplePath(key[1]))
		if pat == "" {
			t.Fatalf("特别覆盖失败：%s %s 没有命中任何 pattern", key[0], key[1])
		}
		op, ok := findOp(ops, key[0], key[1])
		if !ok {
			t.Fatalf("特别覆盖失败：spec 里没有 %s %s", key[0], key[1])
		}
		if m, p, err := splitPattern(pat); err != nil {
			t.Fatalf("特别覆盖失败：命中的 pattern %q 不合法：%v", pat, err)
		} else if m == "" {
			if !bareEntryHas(pat, op) {
				t.Fatalf("特别覆盖失败：%s %s 命中了裸挂载 %q，但表里没登记", key[0], key[1], pat)
			}
		} else if canonicalPath(p) != op.Canonical || m != op.Method {
			t.Fatalf("特别覆盖失败：%s %s 命中了 %q（期望 method+canonical 一致）", key[0], key[1], pat)
		}
	}
}

// TestOpenAPIParityBareMounts 守裸挂载这张显式表：pattern 在 AST 里存在、表里的
// (method, path) 都在 spec 里、判据在 handler 源码里真的出现过、反向不漏（多写的 spec 也红）。
func TestOpenAPIParityBareMounts(t *testing.T) {
	patterns, problems := collectMuxPatterns(serverPkgDir)
	fatalIfProblems(t, "静态收集 mux pattern", problems)
	docs, loadProblems := loadSpecDocs()
	fatalIfProblems(t, "载入五份 spec", loadProblems)
	ops, specProblems := collectSpecOps(docs)
	fatalIfProblems(t, "解析 operation", specProblems)

	fatalIfProblems(t, "裸挂载表", bareMountProblems(patterns, ops))

	// 表本身不能为空（削表守卫）。
	if len(bareMountSpec()) < 20 {
		t.Fatalf("裸挂载表只剩 %d 条 —— 被削过了（真实 mux 的裸 pattern 20 条以上）", len(bareMountSpec()))
	}
}

// TestOpenAPIParitySecurity 守「spec 声明的鉴权 = 真实中间件」：对每条 operation 发一次
// **无凭据**请求，匿名端点不得 401、其余必须 401（无法满足的走显式例外表）。
func TestOpenAPIParitySecurity(t *testing.T) {
	docs, problems := loadSpecDocs()
	fatalIfProblems(t, "载入五份 spec", problems)
	ops, specProblems := collectSpecOps(docs)
	fatalIfProblems(t, "解析 operation", specProblems)

	observed := observeSecurityStatuses(t, ops)
	fatalIfProblems(t, "security 实测", securityProblems(ops, observed))
}

// TestOpenAPIParityDTOFields 守 spec ↔ Go 结构体的**双向**字段集：Go 有而 schema 无 = 红，
// schema 有而 Go 无 = 红（例外必须写理由 + 可过期）。
func TestOpenAPIParityDTOFields(t *testing.T) {
	docs, problems := loadSpecDocs()
	fatalIfProblems(t, "载入五份 spec", problems)

	pairs := parityPairs()
	// 削表守卫：删一对就是把门禁悄悄放宽，而那正是本门禁要防的事。
	if len(pairs) < 30 {
		t.Fatalf("parityPairs 只剩 %d 对（契约要求 ≥30：全部 kind + 全部请求体/回执）", len(pairs))
	}
	fatalIfProblems(t, "DTO 双向字段", dtoProblems(docs, pairs))

	// 宽投影的两个 schema（additionalProperties: true，没有 properties）必须真的是宽的：
	// 一旦它们长出 properties，字段比对就会真的开始跑（这里不需要改代码，红就是提醒）。
	for _, name := range []string{"RenderedCapability", "RenderedSource"} {
		schema, _, ok := (&dtoChecker{docs: docs}).lookupSchema("resource.yaml", name)
		if !ok {
			t.Fatalf("resource.yaml 里没有 %s", name)
		}
		if !wideSchema(schema) {
			t.Errorf("%s 不再是宽投影（有 properties 了）：请把它纳入字段级比对，而不是继续用 additionalProperties 兜住", name)
		}
	}
}

// TestOpenAPIParityMutations 是负向自测：每条门禁都要能被一个具体的错误**打红**。
//
// 变异全部在内存里做（重新 Unmarshal 一份），绝不写盘；每条变异的断言消息里都带
// 变异名，失败时能直接看出“哪条变异没被抓到”。
func TestOpenAPIParityMutations(t *testing.T) {
	patterns, problems := collectMuxPatterns(serverPkgDir)
	fatalIfProblems(t, "静态收集 mux pattern", problems)

	baseDocs, loadProblems := loadSpecDocs()
	fatalIfProblems(t, "载入五份 spec", loadProblems)
	baseOps, specProblems := collectSpecOps(baseDocs)
	fatalIfProblems(t, "解析 operation", specProblems)

	t.Run("删掉一条 spec operation → mux 有、spec 缺", func(t *testing.T) {
		docs := mustReloadDocs(t)
		deletePathOp(t, docs, "resource.yaml", "/v1/namespaces/{namespace}/capabilities", "get")
		ops, _ := collectSpecOps(docs)
		problems := muxToSpecProblems(patterns, ops)
		requireProblem(t, "删掉 spec operation", problems, "mux 有、spec 缺")
	})

	t.Run("加一条不存在的 spec operation → spec 有、mux 无", func(t *testing.T) {
		docs := mustReloadDocs(t)
		addPathOp(t, docs, "resource.yaml", "/v1/namespaces/{namespace}/ghosts", "get", map[string]any{
			"operationId": "ghostOperation",
			"security":    []any{},
		})
		ops, _ := collectSpecOps(docs)
		problems := specToMuxProblems(patterns, ops)
		requireProblem(t, "加幽灵 spec operation", problems, "spec 有、mux 无")
	})

	t.Run("复制一个 operationId → 必须报重复", func(t *testing.T) {
		docs := mustReloadDocs(t)
		setOperationID(t, docs, "resource.yaml", "/v1/namespaces/{namespace}/edges", "get", "listCapabilities")
		ops, _ := collectSpecOps(docs)
		problems := specIntegrityProblems(ops)
		requireProblem(t, "复制 operationId", problems, "operationId 重复")
	})

	t.Run("把一条 session operation 的 security 改成 [] → security 实测必须红", func(t *testing.T) {
		observed := observeSecurityStatuses(t, baseOps)
		docs := mustReloadDocs(t)
		setSecurity(t, docs, "resource.yaml", "/v1/namespaces/{namespace}/capabilities", "get", []any{})
		ops, _ := collectSpecOps(docs)
		if len(ops) == 0 {
			t.Fatalf("变异「security 改成匿名」：变异后解析出 0 条 operation")
		}
		problems := securityProblems(ops, observed)
		requireProblem(t, "security 改成匿名", problems, "spec 声明匿名但实测 401")
	})

	t.Run("从 schema 删一个字段（Go 有、schema 无）→ DTO 必须红", func(t *testing.T) {
		docs := mustReloadDocs(t)
		deleteSchemaProperty(t, docs, "resource.yaml", "CapabilityNodeState", "state")
		problems := dtoProblems(docs, parityPairs())
		requireProblem(t, "schema 删字段", problems, "properties 里没有")
	})

	t.Run("给 schema 加一个 Go 没有的字段 → DTO 必须红", func(t *testing.T) {
		docs := mustReloadDocs(t)
		addSchemaProperty(t, docs, "resource.yaml", "CapabilityNodeState", "ghostField")
		problems := dtoProblems(docs, parityPairs())
		requireProblem(t, "schema 加幽灵字段", problems, "Go 的")
	})

	t.Run("断掉一条跨文件 $ref → 加载/解析必须报错", func(t *testing.T) {
		docs := mustReloadDocs(t)
		setRef(t, docs, "events.yaml", "components.schemas.TraceView.properties.tasks.items",
			"./resource.yaml#/components/schemas/NoSuchSchema")
		problems := refIntegrityProblems(docs)
		requireProblem(t, "断跨文件 $ref", problems, "不存在")
	})
}

// TestOpenAPIParityFakeGreenGuard 是 `-run TestOpenAPIParity` 的假绿守卫 + 反 skip 守卫。
//
// `go test -run TestOpenAPIParity` 在没有匹配时打印 "no tests to run" 但 **exit 0**：
// 一次改名（TestParity…）就能让 `make contract` 静默变绿。所以这里解析本文件自己，
// 数一遍 TestOpenAPIParity* 函数，要求它们都在、且一个都没被 skip 掉。
func TestOpenAPIParityFakeGreenGuard(t *testing.T) {
	src, err := os.ReadFile(selfFileName)
	if err != nil {
		t.Fatalf("读不了本文件 %s：%v（本门禁必须能从 internal/server 目录跑）", selfFileName, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, selfFileName, src, 0)
	if err != nil {
		t.Fatalf("解析本文件失败：%v", err)
	}
	names := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "TestOpenAPIParity") {
			continue
		}
		names[fn.Name.Name] = true
	}
	// 七个门禁一个都不能少：改名 = -run 匹配不到 = exit 0 假绿。
	want := []string{
		"TestOpenAPIParitySpecLoadAndRefs",
		"TestOpenAPIParityMuxToSpec",
		"TestOpenAPIParitySpecToMux",
		"TestOpenAPIParityBareMounts",
		"TestOpenAPIParitySecurity",
		"TestOpenAPIParityDTOFields",
		"TestOpenAPIParityMutations",
		"TestOpenAPIParityFakeGreenGuard",
	}
	for _, name := range want {
		if !names[name] {
			t.Errorf("本文件里没有 %s（改名会让 `go test -run TestOpenAPIParity` 以 exit 0 假绿）", name)
		}
	}
	// 反 skip：本文件不许出现 skip 调用（写成拼接是为了不自匹配）。
	for _, banned := range []string{"t." + "Skip", "Skip" + "Now()", "Skip" + "f("} {
		if strings.Contains(string(src), banned) {
			t.Errorf("本文件出现 %q —— 门禁里不允许 skip/放宽断言", banned)
		}
	}
	// 削表守卫（与 TestOpenAPIParityDTOFields 里那条互为双保险）。
	if len(parityPairs()) < 30 {
		t.Errorf("parityPairs 只剩 %d 对（< 30）", len(parityPairs()))
	}
	if len(bareMountSpec()) < 20 {
		t.Errorf("bareMountSpec 只剩 %d 条（< 20）", len(bareMountSpec()))
	}
}

// ============================================================================
// 1. spec 载入 + $ref 解析
// ============================================================================

// specDocs 是「文件名 → 解析后的 YAML 文档」。跨文件 $ref 靠它定位目标文件。
type specDocs map[string]map[string]any

// loadSpecDocs 读并解析五份真源；exact-five 违规、文件缺失、YAML 坏都变成 problems。
func loadSpecDocs() (specDocs, []string) {
	var problems []string
	entries, err := os.ReadDir(specDir)
	if err != nil {
		return nil, []string{fmt.Sprintf("读不了 %s 目录：%v", specDir, err)}
	}
	found := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		found[e.Name()] = true
	}
	for _, name := range specFileNames {
		if !found[name] {
			problems = append(problems, fmt.Sprintf("exact-five：缺少 %s/%s", specDir, name))
		}
	}
	for name := range found {
		if !containsString(specFileNames, name) {
			problems = append(problems, fmt.Sprintf("exact-five：%s/%s 不是五份真源之一（api/ 下第六个 .yaml 即失败）", specDir, name))
		}
	}

	docs := specDocs{}
	for _, name := range specFileNames {
		raw, err := os.ReadFile(filepath.Join(specDir, name))
		if err != nil {
			problems = append(problems, fmt.Sprintf("读不了 %s/%s：%v", specDir, name, err))
			continue
		}
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			problems = append(problems, fmt.Sprintf("%s/%s 不是合法 YAML：%v", specDir, name, err))
			continue
		}
		if doc == nil {
			problems = append(problems, fmt.Sprintf("%s/%s 解析成空文档", specDir, name))
			continue
		}
		docs[name] = doc
	}
	return docs, problems
}

// resolveRefFrom 解开一条 `$ref`：`#/components/...`（同文件）或 `./x.yaml#/components/...`
// （跨文件）。禁止 remote / 绝对路径 / 逃逸 api/ 目录；指针逐段下钻。
func (d specDocs) resolveRefFrom(currentFile, ref string) (any, string, error) {
	filePart, pointer, hasPointer := strings.Cut(ref, "#")
	targetFile := currentFile
	if filePart != "" {
		if strings.Contains(filePart, "://") {
			return nil, currentFile, fmt.Errorf("禁止 remote $ref：%s", ref)
		}
		if strings.HasPrefix(filePart, "/") {
			return nil, currentFile, fmt.Errorf("禁止绝对路径 $ref：%s", ref)
		}
		name := strings.TrimPrefix(filePart, "./")
		if strings.Contains(name, "/") || name == ".." || name == "." {
			return nil, currentFile, fmt.Errorf("$ref 不得逃逸 %s 目录：%s", specDir, ref)
		}
		if _, ok := d[name]; !ok {
			return nil, currentFile, fmt.Errorf("$ref 指向不存在的文件：%s（当前文件 %s）", ref, currentFile)
		}
		targetFile = name
	}
	root, ok := d[targetFile]
	if !ok {
		return nil, currentFile, fmt.Errorf("$ref 的目标文件 %s 没有载入：%s", targetFile, ref)
	}
	if !hasPointer || pointer == "" {
		return root, targetFile, nil
	}
	var node any = root
	for _, seg := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		seg = strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")
		m, ok := node.(map[string]any)
		if !ok {
			return nil, targetFile, fmt.Errorf("$ref 指针 %q 走到了非对象节点：%s", seg, ref)
		}
		v, ok := m[seg]
		if !ok {
			return nil, targetFile, fmt.Errorf("$ref 指针段 %q 不存在：%s", seg, ref)
		}
		node = v
	}
	return node, targetFile, nil
}

// resolveSchemaNode 沿着 `$ref` 链下钻（环上限 maxRefDepth）并合并 `allOf` 的 properties。
//
// `allOf` 是 OpenAPI 3.0 里「给 $ref 加兄弟键」（`nullable: true` 就是这么写的）的标准
// 写法；不合并就会读到一份没有 properties 的 schema，那种失败信息会把人指向错误方向。
func (d specDocs) resolveSchemaNode(file string, schema map[string]any) (map[string]any, string, error) {
	seen := map[string]bool{}
	for i := 0; i < maxRefDepth; i++ {
		ref, _ := schema["$ref"].(string)
		if ref == "" {
			break
		}
		if seen[ref] {
			return nil, file, fmt.Errorf("$ref 成环：%s", ref)
		}
		seen[ref] = true
		node, nextFile, err := d.resolveRefFrom(file, ref)
		if err != nil {
			return nil, file, err
		}
		m, ok := node.(map[string]any)
		if !ok {
			return nil, file, fmt.Errorf("$ref 的目标不是 schema 对象：%s", ref)
		}
		schema, file = m, nextFile
	}
	if ref, _ := schema["$ref"].(string); ref != "" {
		return nil, file, fmt.Errorf("$ref 链超过 %d 层：%s", maxRefDepth, ref)
	}

	parts, ok := schema["allOf"].([]any)
	if !ok {
		return schema, file, nil
	}
	merged := map[string]any{}
	props := map[string]any{}
	for _, part := range parts {
		pm, _ := part.(map[string]any)
		if pm == nil {
			return nil, file, fmt.Errorf("allOf 里有非对象元素")
		}
		sub, _, err := d.resolveSchemaNode(file, pm)
		if err != nil {
			return nil, file, err
		}
		for k, v := range sub {
			if k == "properties" {
				pm, ok := v.(map[string]any)
				if !ok {
					continue
				}
				for pk, pv := range pm {
					props[pk] = pv
				}
				continue
			}
			merged[k] = v
		}
	}
	for k, v := range schema {
		if k == "allOf" || k == "properties" {
			continue
		}
		if _, exists := merged[k]; !exists {
			merged[k] = v
		}
	}
	if len(props) > 0 {
		merged["properties"] = props
	}
	return merged, file, nil
}

// refIntegrityProblems 解开**所有**文件里出现的每一个 `$ref`（不限于被 parityPairs 用到的
// 那几个）：跨文件断链、remote、绝对路径、逃逸目录都在这里一次性暴露。
func refIntegrityProblems(docs specDocs) []string {
	var problems []string
	for _, file := range specFileNames {
		doc, ok := docs[file]
		if !ok {
			continue
		}
		walkRefs(doc, file, "", func(currentFile, path, ref string) {
			if _, _, err := docs.resolveRefFrom(currentFile, ref); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %s 的 $ref 解析失败：%v", currentFile, path, err))
			}
		})
	}
	return problems
}

// walkRefs 深度遍历文档，对每个 `$ref` 键回调（path 是便于定位的点号路径）。
func walkRefs(node any, currentFile, path string, visit func(currentFile, path, ref string)) {
	switch v := node.(type) {
	case map[string]any:
		for _, key := range sortedKeys(v) {
			if key == "$ref" {
				if ref, ok := v[key].(string); ok {
					visit(currentFile, path, ref)
				}
				continue
			}
			walkRefs(v[key], currentFile, path+"."+key, visit)
		}
	case []any:
		for i, item := range v {
			walkRefs(item, currentFile, fmt.Sprintf("%s[%d]", path, i), visit)
		}
	}
}

// ============================================================================
// 2. spec 的 operation 集合
// ============================================================================

// specMethods 是 OpenAPI 里可搭载 operation 的方法键（顺序固定，失败信息稳定）。
var specMethods = []string{"get", "put", "post", "delete", "patch", "head", "options"}

// specOp 是 spec 里的一条 operation（pathItem 为 $ref 时 FromPathItem=true）。
type specOp struct {
	File             string
	Method           string
	Path             string
	Canonical        string
	OperationID      string
	Security         []string
	SecurityDeclared bool
	FromPathItem     bool
}

// Anonymous 报告这条 operation 是否匿名（`security: []`）。
func (o specOp) Anonymous() bool { return len(o.Security) == 0 }

// Key 是 (method, 原始 spec path) —— 与实测状态码表、例外表的键一致。
func (o specOp) Key() string { return o.Method + " " + o.Path }

// collectSpecOps 收集五份文件里的全部 operation；pathItem 为 `$ref` 时解开后再取方法。
func collectSpecOps(docs specDocs) ([]specOp, []string) {
	var problems []string
	var ops []specOp
	for _, file := range specFileNames {
		doc, ok := docs[file]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s 没有载入", file))
			continue
		}
		paths, ok := doc["paths"].(map[string]any)
		if !ok {
			problems = append(problems, fmt.Sprintf("%s 没有 paths 段", file))
			continue
		}
		for _, rawPath := range sortedKeys(paths) {
			item, ok := paths[rawPath].(map[string]any)
			if !ok {
				problems = append(problems, fmt.Sprintf("%s: path %q 不是对象", file, rawPath))
				continue
			}
			fromPathItem := false
			if ref, _ := item["$ref"].(string); ref != "" {
				node, _, err := docs.resolveRefFrom(file, ref)
				if err != nil {
					problems = append(problems, fmt.Sprintf("%s: path %q 的 pathItem $ref 解析失败：%v", file, rawPath, err))
					continue
				}
				resolved, ok := node.(map[string]any)
				if !ok {
					problems = append(problems, fmt.Sprintf("%s: path %q 的 pathItem $ref 不是对象", file, rawPath))
					continue
				}
				item = resolved
				fromPathItem = true
			}
			for _, method := range specMethods {
				node, ok := item[method]
				if !ok {
					continue
				}
				opNode, ok := node.(map[string]any)
				if !ok {
					problems = append(problems, fmt.Sprintf("%s: %s %s 的 operation 不是对象", file, strings.ToUpper(method), rawPath))
					continue
				}
				op := specOp{
					File:         file,
					Method:       strings.ToUpper(method),
					Path:         rawPath,
					Canonical:    canonicalPath(rawPath),
					FromPathItem: fromPathItem,
				}
				op.OperationID, _ = opNode["operationId"].(string)
				if op.OperationID == "" {
					problems = append(problems, fmt.Sprintf("%s: %s %s 没有 operationId", file, op.Method, rawPath))
				}
				sec, declared := opNode["security"]
				op.SecurityDeclared = declared
				if declared {
					schemes, err := securitySchemeNames(sec)
					if err != nil {
						problems = append(problems, fmt.Sprintf("%s: %s %s 的 security 形状不合法：%v", file, op.Method, rawPath, err))
					}
					op.Security = schemes
				}
				ops = append(ops, op)
			}
		}
	}
	sort.SliceStable(ops, func(i, j int) bool {
		if ops[i].Method != ops[j].Method {
			return ops[i].Method < ops[j].Method
		}
		return ops[i].Path < ops[j].Path
	})
	return ops, problems
}

// securitySchemeNames 把 `security: [{session: []}, {consoleKey: []}]` 摊平成方案名。
func securitySchemeNames(v any) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("security 必须是数组")
	}
	var out []string
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("security 的元素必须是对象")
		}
		for name := range m {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// specIntegrityProblems 守 operation 的身份：同一 (method, canonical path) 不得重复，
// operationId 跨五文件全局唯一（归属键 = method + canonical path）。
func specIntegrityProblems(ops []specOp) []string {
	var problems []string
	byKey := map[string]specOp{}
	byID := map[string]specOp{}
	for _, op := range ops {
		if prev, dup := byKey[op.Key()]; dup {
			problems = append(problems, fmt.Sprintf("归属重复：%s 同时出现在 %s（operationId=%s）与 %s（operationId=%s）",
				op.Key(), op.File, op.OperationID, prev.File, prev.OperationID))
		}
		byKey[op.Key()] = op
		if op.OperationID == "" {
			continue
		}
		if prev, dup := byID[op.OperationID]; dup {
			problems = append(problems, fmt.Sprintf("operationId 重复：%q 同时出现在 %s %s 与 %s %s",
				op.OperationID, prev.File, prev.Path, op.File, op.Path))
		}
		byID[op.OperationID] = op
	}
	return problems
}

// specProblems 是 operation 层的全部问题（解析 + 身份）。
func specProblems(docs specDocs) []string {
	ops, problems := collectSpecOps(docs)
	return append(problems, specIntegrityProblems(ops)...)
}

// canonicalPath 把占位符归一成 `{}`（归属键的可比形态：`{ns}` 与 `{namespace}` 等价）。
func canonicalPath(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if len(seg) >= 2 && strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			segs[i] = "{}"
		}
	}
	return strings.Join(segs, "/")
}

// samplePath 把 spec 路径里的占位符换成样本值（探针请求用）。
func samplePath(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if len(seg) >= 2 && strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			switch strings.Trim(seg, "{}") {
			case "namespace":
				segs[i] = "default"
			case "sha256":
				segs[i] = strings.Repeat("a", 64)
			default:
				segs[i] = "x"
			}
		}
	}
	return strings.Join(segs, "/")
}

// ============================================================================
// 3. 静态收集真实 mux pattern（go/parser，排 _test.go）
// ============================================================================

// routePattern 是一条真实注册的路由。Method=="" 即裸前缀/裸路径挂载。
type routePattern struct {
	Method string
	Path   string
	Raw    string
	Pos    string
}

// Canonical 是归一后的路径（`{ns}` → `{}`）。
func (p routePattern) Canonical() string { return canonicalPath(p.Path) }

// protocolPathConsts 是**唯一**允许出现在 pattern 表达式里的外部常量小表。
//
// 为什么只允许这张表：pattern 是「协议的一部分」，只能来自 protocol 包里那几个被节点共享
// 的常量。放开任意 selector 等于让 pattern 变成运行期算出来的值，静态门禁就彻底失效。
var protocolPathConsts = map[string]string{
	"NodeTaskArtifactsPathPrefix": protocol.NodeTaskArtifactsPathPrefix,
	"NodeTaskArtifactsPathInfix":  protocol.NodeTaskArtifactsPathInfix,
	"NodeStreamPath":              protocol.NodeStreamPath,
	"NodeBinaryPath":              protocol.NodeBinaryPath,
}

// patternCollector 是一次静态扫描的状态。
type patternCollector struct {
	fset        *token.FileSet
	src         map[string][]byte
	pkgConsts   map[string]ast.Expr
	pkgConstPos map[string]string
	problems    []string
	out         []routePattern
	seen        map[string]string
}

// collectMuxPatterns 扫描 dir 下的非测试 Go 源码，收集所有 `mux.Handle/handleFunc` 的
// 第一个实参并求值成 pattern。**求值失败一律记成 problem**（上层 Fatal），绝不跳过。
func collectMuxPatterns(dir string) ([]routePattern, []string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []string{fmt.Sprintf("读不了 %s：%v", dir, err)}
	}
	c := &patternCollector{
		fset:        token.NewFileSet(),
		src:         map[string][]byte{},
		pkgConsts:   map[string]ast.Expr{},
		pkgConstPos: map[string]string{},
		seen:        map[string]string{},
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			c.problems = append(c.problems, fmt.Sprintf("读不了 %s：%v", path, err))
			continue
		}
		f, err := parser.ParseFile(c.fset, path, src, 0)
		if err != nil {
			c.problems = append(c.problems, fmt.Sprintf("解析 %s 失败：%v", path, err))
			continue
		}
		c.src[path] = src
		files = append(files, f)
	}
	// 第一遍：包级 const/var 的字符串值（表驱动 pattern 依赖 `const base = "..."` 与
	// 包级 `tasksPath` 等）。
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, nm := range vs.Names {
					if i < len(vs.Values) {
						c.pkgConsts[nm.Name] = vs.Values[i]
						c.pkgConstPos[nm.Name] = c.pos(vs.Values[i])
					}
				}
			}
		}
	}
	// 第二遍：每个函数体（路由注册只发生在函数里）。
	for _, f := range files {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			c.scanFunc(fd)
		}
	}
	// 第三遍：`Handle/HandleFunc` 只在 mux 上调用（别的 receiver 会让门禁漏掉路由）。
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || id.Name != "mux" {
				c.problems = append(c.problems, fmt.Sprintf(
					"%s：%s 上的 %s 调用不在静态门禁的收集范围内（路由必须走 mux.Handle/mux.HandleFunc）",
					c.pos(call), exprTextOr(c, call.Fun), sel.Sel.Name))
			}
			return true
		})
	}
	sort.SliceStable(c.out, func(i, j int) bool {
		if c.out[i].Path != c.out[j].Path {
			return c.out[i].Path < c.out[j].Path
		}
		return c.out[i].Method < c.out[j].Method
	})
	return c.out, c.problems
}

// scanFunc 处理一个函数体里的全部路由注册形态：
//
//	① 直写：            mux.Handle("GET " + base, h)
//	② 包装闭包：        route := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, …) }
//	                    route("GET "+nodesPath, a.list)
//	③ 表驱动 + range：  routes := []struct{pattern string; h http.HandlerFunc}{…}
//	                    for _, rt := range routes { mux.Handle(rt.pattern, …) }
//
// ② ③ 是真实代码的写法（resource_node/resource_task/retained_reads/capability/playbook）；
// 不展开它们就等于漏掉那五个资源域的全部路由 —— 所以这里必须理解它们，而不是“跳过”。
func (c *patternCollector) scanFunc(fd *ast.FuncDecl) {
	scopes := []map[string]ast.Expr{c.localConsts(fd)}
	wrappers, wrapperParams := c.wrappers(fd)
	rangeVars := c.rangeVarNames(fd)
	rangeTables := c.rangeHandles(fd)

	// ① 直写形态（跳过 ② ③ 的“参数位”）。
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isMuxHandleCall(call) {
			return true
		}
		arg := call.Args[0]
		if id, ok := arg.(*ast.Ident); ok && wrapperParams[id.Name] {
			return true
		}
		if sel, ok := arg.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && rangeVars[id.Name] {
				return true
			}
		}
		c.record(arg, scopes)
		return true
	})
	// ② 包装闭包的调用点。
	for name := range wrappers {
		wrapperName := name
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			fid, ok := call.Fun.(*ast.Ident)
			if !ok || fid.Name != wrapperName {
				return true
			}
			c.record(call.Args[0], scopes)
			return true
		})
	}
	// ③ 表驱动：把复合字面量里每个元素的 pattern 字段求值。
	for _, table := range rangeTables {
		c.recordRangeTable(fd, table, scopes)
	}
}

// rangeHandle 描述一处「range 一张局部的 struct 切片表」。
type rangeHandle struct {
	sliceVar string
	field    string
}

// localConsts 收函数体内的 `const x = "..."`（`const base = "/v1/namespaces/{ns}/…"` 就是它）。
func (c *patternCollector) localConsts(fd *ast.FuncDecl) map[string]ast.Expr {
	out := map[string]ast.Expr{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		ds, ok := n.(*ast.DeclStmt)
		if !ok {
			return true
		}
		gd, ok := ds.Decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			return true
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, nm := range vs.Names {
				if i < len(vs.Values) {
					out[nm.Name] = vs.Values[i]
				}
			}
		}
		return true
	})
	return out
}

// wrappers 找出「把 pattern 当第一个参数转手给 mux.Handle 的闭包」及其调用名。
func (c *patternCollector) wrappers(fd *ast.FuncDecl) (map[string]string, map[string]bool) {
	names := map[string]string{}
	params := map[string]bool{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		lit, ok := as.Rhs[0].(*ast.FuncLit)
		if !ok || lit.Type.Params == nil || len(lit.Type.Params.List) == 0 {
			return true
		}
		first := lit.Type.Params.List[0]
		if len(first.Names) == 0 {
			return true
		}
		pname := first.Names[0].Name
		uses := false
		ast.Inspect(lit.Body, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok || !isMuxHandleCall(call) {
				return true
			}
			if id, ok := call.Args[0].(*ast.Ident); ok && id.Name == pname {
				uses = true
			}
			return true
		})
		if !uses {
			return true
		}
		name, ok := as.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		names[name.Name] = pname
		params[pname] = true
		return true
	})
	return names, params
}

// rangeVarNames 收 `for _, x := range …` 的 value 变量名（用来在直写扫描里跳过 `x.pattern`）。
func (c *patternCollector) rangeVarNames(fd *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		rs, ok := n.(*ast.RangeStmt)
		if !ok {
			return true
		}
		if id, ok := rs.Value.(*ast.Ident); ok && id.Name != "_" {
			out[id.Name] = true
		}
		return true
	})
	return out
}

// rangeHandles 找「range 一张局部表、循环体里 mux.Handle(x.<field>, …)」的形态。
func (c *patternCollector) rangeHandles(fd *ast.FuncDecl) []rangeHandle {
	var out []rangeHandle
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		rs, ok := n.(*ast.RangeStmt)
		if !ok {
			return true
		}
		valName := ""
		if id, ok := rs.Value.(*ast.Ident); ok && id.Name != "_" {
			valName = id.Name
		}
		if valName == "" {
			return true
		}
		field := ""
		ast.Inspect(rs.Body, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok || !isMuxHandleCall(call) {
				return true
			}
			sel, ok := call.Args[0].(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || id.Name != valName {
				return true
			}
			field = sel.Sel.Name
			return true
		})
		if field == "" {
			return true
		}
		id, ok := rs.X.(*ast.Ident)
		if !ok {
			c.problems = append(c.problems, fmt.Sprintf("%s：range 的源不是局部变量，无法静态展开表驱动 pattern", c.pos(rs)))
			return true
		}
		out = append(out, rangeHandle{sliceVar: id.Name, field: field})
		return true
	})
	return out
}

// recordRangeTable 把 `routes := []struct{pattern string; …}{…}` 的每个元素展开。
func (c *patternCollector) recordRangeTable(fd *ast.FuncDecl, table rangeHandle, scopes []map[string]ast.Expr) {
	var lit *ast.CompositeLit
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if lit != nil {
			return false
		}
		switch x := n.(type) {
		case *ast.AssignStmt:
			if len(x.Lhs) == 1 && len(x.Rhs) == 1 {
				if id, ok := x.Lhs[0].(*ast.Ident); ok && id.Name == table.sliceVar {
					if cl, ok := x.Rhs[0].(*ast.CompositeLit); ok {
						lit = cl
					}
				}
			}
		case *ast.DeclStmt:
			gd, ok := x.Decl.(*ast.GenDecl)
			if !ok {
				return true
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, nm := range vs.Names {
					if nm.Name != table.sliceVar || i >= len(vs.Values) {
						continue
					}
					if cl, ok := vs.Values[i].(*ast.CompositeLit); ok {
						lit = cl
					}
				}
			}
		}
		return true
	})
	if lit == nil {
		c.problems = append(c.problems, fmt.Sprintf(
			"无法静态求值的 pattern 表达式：range 表 %q 在函数 %s 里找不到复合字面量定义",
			table.sliceVar, fd.Name.Name))
		return
	}
	fieldNames := compositeFieldNames(lit.Type)
	idx := -1
	for i, name := range fieldNames {
		if name == table.field {
			idx = i
		}
	}
	for _, elt := range lit.Elts {
		el, ok := elt.(*ast.CompositeLit)
		if !ok {
			c.problems = append(c.problems, fmt.Sprintf(
				"无法静态求值的 pattern 表达式：range 表 %q 的元素不是复合字面量（%s）", table.sliceVar, c.pos(elt)))
			continue
		}
		var pick ast.Expr
		for _, e := range el.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == table.field {
				pick = kv.Value
			}
		}
		if pick == nil {
			if idx < 0 || idx >= len(el.Elts) {
				c.problems = append(c.problems, fmt.Sprintf(
					"无法静态求值的 pattern 表达式：range 表 %q 的元素没有 %q 字段（%s）",
					table.sliceVar, table.field, c.pos(el)))
				continue
			}
			pick = el.Elts[idx]
		}
		c.record(pick, scopes)
	}
}

// compositeFieldNames 取 `[]struct{...}{}` 里结构体的字段名顺序（位置式字面量靠它对齐）。
func compositeFieldNames(t ast.Expr) []string {
	at, ok := t.(*ast.ArrayType)
	if !ok {
		return nil
	}
	st, ok := at.Elt.(*ast.StructType)
	if !ok {
		return nil
	}
	var out []string
	for _, f := range st.Fields.List {
		for _, nm := range f.Names {
			out = append(out, nm.Name)
		}
	}
	return out
}

// record 求值一个 pattern 表达式并登记（求值失败 → problem，带源码位置与表达式原文）。
func (c *patternCollector) record(e ast.Expr, scopes []map[string]ast.Expr) {
	raw, err := c.eval(e, scopes, 0)
	if err != nil {
		c.problems = append(c.problems, fmt.Sprintf(
			"无法静态求值的 pattern 表达式：%s（%s）：%v", c.text(e), c.pos(e), err))
		return
	}
	method, path, err := splitPattern(raw)
	if err != nil {
		c.problems = append(c.problems, fmt.Sprintf("%s（%s）：%v", raw, c.pos(e), err))
		return
	}
	if prev, dup := c.seen[raw]; dup {
		c.problems = append(c.problems, fmt.Sprintf("pattern %q 被注册了两次（%s 与 %s）", raw, prev, c.pos(e)))
	}
	c.seen[raw] = c.pos(e)
	c.out = append(c.out, routePattern{Method: method, Path: path, Raw: raw, Pos: c.pos(e)})
}

// eval 按 Go 的常量规则求值 pattern 表达式（深度上限防常量自引用）。
func (c *patternCollector) eval(e ast.Expr, scopes []map[string]ast.Expr, depth int) (string, error) {
	if depth > 16 {
		return "", fmt.Errorf("表达式层级过深（疑似常量自引用）")
	}
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", fmt.Errorf("非字符串字面量 %s", x.Value)
		}
		s, err := strconv.Unquote(x.Value)
		if err != nil {
			return "", fmt.Errorf("字符串字面量解析失败：%v", err)
		}
		return s, nil
	case *ast.ParenExpr:
		return c.eval(x.X, scopes, depth+1)
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", fmt.Errorf("只支持 + 拼接（见到 %s）", x.Op)
		}
		left, err := c.eval(x.X, scopes, depth+1)
		if err != nil {
			return "", err
		}
		right, err := c.eval(x.Y, scopes, depth+1)
		if err != nil {
			return "", err
		}
		return left + right, nil
	case *ast.Ident:
		for i := len(scopes) - 1; i >= 0; i-- {
			if v, ok := scopes[i][x.Name]; ok {
				return c.eval(v, scopes, depth+1)
			}
		}
		if v, ok := c.pkgConsts[x.Name]; ok {
			return c.eval(v, scopes, depth+1)
		}
		return "", fmt.Errorf("标识符 %q 不是可求值的字符串常量", x.Name)
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		if !ok || id.Name != "protocol" {
			return "", fmt.Errorf("只允许 protocol.<受限常量>（见到 %s）", c.text(x))
		}
		v, ok := protocolPathConsts[x.Sel.Name]
		if !ok {
			return "", fmt.Errorf("protocol.%s 不在允许的常量小表里（新增请先扩 protocolPathConsts）", x.Sel.Name)
		}
		return v, nil
	}
	return "", fmt.Errorf("不支持的表达式形态 %T", e)
}

// pos 返回 "file:line" 形态的位置（失败信息照着实操就能定位）。
func (c *patternCollector) pos(n ast.Node) string {
	p := c.fset.Position(n.Pos())
	return fmt.Sprintf("%s:%d", filepath.Base(p.Filename), p.Line)
}

// text 返回表达式的源码原文（失败信息里直接看到写的是什么）。
func (c *patternCollector) text(n ast.Node) string {
	return exprTextOr(c, n)
}

func exprTextOr(c *patternCollector, n ast.Node) string {
	p := c.fset.Position(n.Pos())
	end := c.fset.Position(n.End())
	src, ok := c.src[p.Filename]
	if !ok || p.Offset < 0 || end.Offset > len(src) || end.Offset <= p.Offset {
		return "<expr>"
	}
	return strings.TrimSpace(string(src[p.Offset:end.Offset]))
}

// isMuxHandleCall 报告调用是否是 `mux.Handle(...)` / `mux.HandleFunc(...)`。
func isMuxHandleCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != "mux" {
		return false
	}
	return len(call.Args) >= 1
}

// splitPattern 把 "METHOD /path" 拆成 (method, path)；不带方法即裸挂载（method==""）。
func splitPattern(raw string) (method, path string, err error) {
	fields := strings.Fields(raw)
	switch len(fields) {
	case 1:
		if !strings.HasPrefix(fields[0], "/") {
			return "", "", fmt.Errorf("pattern 必须以 / 开头：%q", raw)
		}
		return "", fields[0], nil
	case 2:
		m := strings.ToUpper(fields[0])
		if !containsString(specMethods, strings.ToLower(m)) {
			return "", "", fmt.Errorf("pattern 的方法 %q 不认识：%q", fields[0], raw)
		}
		if !strings.HasPrefix(fields[1], "/") {
			return "", "", fmt.Errorf("pattern 的路径必须以 / 开头：%q", raw)
		}
		return m, fields[1], nil
	}
	return "", "", fmt.Errorf("pattern 形状不合法：%q", raw)
}

// ============================================================================
// 4. 双向比对：mux → spec / spec → mux / 裸挂载
// ============================================================================

// muxToSpecProblems 是「mux 有、spec 缺」：带方法的 pattern 必须在 spec operation 集合里
// **恰好出现一次**（多一次 = 归属重复，specIntegrityProblems 也会报）。
func muxToSpecProblems(patterns []routePattern, ops []specOp) []string {
	var problems []string
	counts := map[string]int{}
	for _, op := range ops {
		counts[op.Method+" "+op.Canonical]++
	}
	for _, p := range patterns {
		if p.Method == "" {
			continue // 裸挂载由 bareMountProblems 管
		}
		switch counts[p.Method+" "+p.Canonical()] {
		case 1:
			// ok
		case 0:
			problems = append(problems, fmt.Sprintf(
				"mux 有、spec 缺：%s %s（真实注册在 %s，canonical=%s）", p.Method, p.Path, p.Pos, p.Canonical()))
		default:
			problems = append(problems, fmt.Sprintf(
				"mux 的 %s %s 在 spec 里出现了 %d 次（归属键 %s 必须唯一）",
				p.Method, p.Path, counts[p.Method+" "+p.Canonical()], p.Method+" "+p.Canonical()))
		}
	}
	return problems
}

// specToMuxProblems 是「spec 有、mux 无」：把 spec 的 (method, path) 打进探针 mux，
// 命中的 pattern 必须就是它自己（或它在 bareMountSpec 里登记的裸挂载）。
func specToMuxProblems(patterns []routePattern, ops []specOp) []string {
	mux, problems := newProbeMux(patterns)
	for _, op := range ops {
		pat := probeMatch(mux, op.Method, samplePath(op.Path))
		if pat == "" {
			problems = append(problems, fmt.Sprintf(
				"spec 有、mux 无：%s %s（%s，operationId=%s）没有任何真实 pattern 接住它",
				op.Method, op.Path, op.File, op.OperationID))
			continue
		}
		method, path, err := splitPattern(pat)
		if err != nil {
			problems = append(problems, fmt.Sprintf("spec 有、mux 无：%s %s 命中的 pattern %q 不合法：%v",
				op.Method, op.Path, pat, err))
			continue
		}
		if method != "" && method == op.Method && canonicalPath(path) == op.Canonical {
			continue
		}
		if method == "" {
			entry, known := bareEntry(pat)
			if !known {
				problems = append(problems, fmt.Sprintf(
					"spec 有、mux 无：%s %s 只落到了未登记的裸 pattern %q", op.Method, op.Path, pat))
				continue
			}
			if len(entry.Ops) == 0 {
				// 兜底 pattern（静态资源 / 健康检查 / 显式 405）。spec 里的端点落在这里 =
				// 真实 mux 没为它注册路由，等于「spec 有、mux 无」。
				problems = append(problems, fmt.Sprintf(
					"spec 有、mux 无：%s %s 没有任何真实路由接住它，请求只会落到兜底 pattern %q（%s）",
					op.Method, op.Path, pat, entry.Reason))
				continue
			}
			if bareEntryHas(pat, op) {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"spec 的 %s %s 落到了裸挂载 %q，但该裸挂载的表条目里没有这条 operation（裸挂载是 method 的黑盒，必须显式登记）",
				op.Method, op.Path, pat))
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"spec 的 %s %s 命中了别的 pattern %q（方法或路径不一致；method=%q canonical=%q）",
			op.Method, op.Path, pat, method, canonicalPath(path)))
	}
	return problems
}

// newProbeMux 把静态收集到的 pattern 全部装进真实 `http.ServeMux`（handler 只是探针）。
//
// 用真实 ServeMux 而不是自己写匹配器：Go 1.22 的优先级（最长匹配 / 方法优先 / 裸前缀的
// 405 语义）本身就是 spec 归属判断的一部分，自己实现一遍必然与生产行为分叉。
func newProbeMux(patterns []routePattern) (*http.ServeMux, []string) {
	mux := http.NewServeMux()
	var problems []string
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // 418：探针只有 pattern 有意义
	})
	for _, p := range patterns {
		func(raw, pos string) {
			defer func() {
				if r := recover(); r != nil {
					problems = append(problems, fmt.Sprintf("pattern %q（%s）装不进真实 ServeMux：%v", raw, pos, r))
				}
			}()
			mux.Handle(raw, probe)
		}(p.Raw, p.Pos)
	}
	return mux, problems
}

// probeMatch 问探针 mux：这个 (method, path) 谁接住了？返回命中的注册 pattern。
func probeMatch(mux *http.ServeMux, method, path string) string {
	req := httptest.NewRequest(method, path, nil)
	_, pattern := mux.Handler(req)
	return pattern
}

// ---------- 裸挂载的显式表 ----------

// bareOp 是裸挂载负责的一条 spec operation。
type bareOp struct {
	Method string
	Path   string // spec 路径（占位符写法与 spec 一致）
	// Literal 是**判定依据**：必须能在 HandlerFile 的源码里找到的字面量（含引号）。
	// 找不到 = 表和 handler 已经脱节（比如 handler 的 switch 改了分支名），直接红。
	Literal string
}

// bareMountEntry 是一条裸 pattern（AST 里 method 为空的注册）。
type bareMountEntry struct {
	Pattern     string
	HandlerFile string
	Ops         []bareOp
	// Reason 只在「该裸 pattern 不承载任何 spec operation」时出现（405 兜底 / 静态兜底）。
	Reason string
}

// bareMountSpec 是裸挂载的**唯一**事实来源。
//
// 为什么需要这张表：mux 上的 `/v1/auth/`、`/v1/devices` 这类注册不带方法——method 由
// handler 内部的 switch 决定，静态 AST 里根本看不到。spec 却必须写具体 method。于是
// 「mux 的裸 pattern」与「spec 的 (method, path)」之间必须有一份人工维护的映射，而这份
// 映射必须**可被机械验证**（每条都带 handler 源码里的字面量判据 + 反向不漏）。
func bareMountSpec() []bareMountEntry {
	return []bareMountEntry{
		{
			Pattern:     "/v1/auth/",
			HandlerFile: "auth_handlers.go",
			Ops: []bareOp{
				{"POST", "/v1/auth/register/options", `"register/options"`},
				{"POST", "/v1/auth/register", `"register"`},
				{"POST", "/v1/auth/login/options", `"login/options"`},
				{"POST", "/v1/auth/login", `"login"`},
				{"POST", "/v1/auth/logout", `"logout"`},
				{"POST", "/v1/auth/recovery/login", `"recovery/login"`},
				// password/login 有显式路由（更具体，见下一条），这里只是 authHandler 的兜底；
				// 两条都登记，反向检查按真实匹配优先级判（不会重复报）。
				{"POST", "/v1/auth/password/login", `"password/login"`},
			},
		},
		{
			Pattern:     "/v1/auth/password/login",
			HandlerFile: "auth_handlers.go",
			Ops: []bareOp{
				{"POST", "/v1/auth/password/login", `func (h *authHandler) passwordLogin(w http.ResponseWriter`},
			},
		},
		{
			Pattern:     "/v1/auth/password/register",
			HandlerFile: "auth_handlers.go",
			Ops: []bareOp{
				{"POST", "/v1/auth/password/register", `func (h *authHandler) passwordRegister(w http.ResponseWriter`},
			},
		},
		{
			Pattern:     "/v1/auth/password",
			HandlerFile: "auth_handlers.go",
			Ops: []bareOp{
				{"GET", "/v1/auth/password", `"password"`},
				{"POST", "/v1/auth/password", `"password"`},
				{"DELETE", "/v1/auth/password", `"password"`},
			},
		},
		{
			Pattern:     "/v1/auth/recovery/regenerate",
			HandlerFile: "auth_handlers.go",
			Ops: []bareOp{
				{"POST", "/v1/auth/recovery/regenerate", `func (h *authHandler) recoveryRegenerate(w http.ResponseWriter`},
			},
		},
		{
			Pattern:     "/v1/auth/recovery/status",
			HandlerFile: "auth_handlers.go",
			Ops: []bareOp{
				{"GET", "/v1/auth/recovery/status", `func (h *authHandler) recoveryStatus(w http.ResponseWriter`},
			},
		},
		{
			Pattern:     "/v1/auth/me",
			HandlerFile: "auth_handlers.go",
			Ops: []bareOp{
				{"GET", "/v1/auth/me", `case "me":`},
			},
		},
		{
			Pattern:     "/v1/auth/sessions",
			HandlerFile: "auth_handlers.go",
			Ops: []bareOp{
				{"GET", "/v1/auth/sessions", `case "sessions":`},
			},
		},
		{
			Pattern:     "/v1/auth/passkeys",
			HandlerFile: "auth_handlers.go",
			Ops: []bareOp{
				{"GET", "/v1/auth/passkeys", `func (h *authHandler) listPasskeys(w http.ResponseWriter`},
			},
		},
		{
			Pattern:     "/v1/auth/passkeys/",
			HandlerFile: "auth_handlers.go",
			Ops: []bareOp{
				{"POST", "/v1/auth/passkeys/register/options", `sub == "register/options"`},
				{"POST", "/v1/auth/passkeys/register", `sub == "register"`},
				{"PATCH", "/v1/auth/passkeys/{id}", `func (h *authHandler) renamePasskey(w http.ResponseWriter`},
				{"DELETE", "/v1/auth/passkeys/{id}", `func (h *authHandler) deletePasskey(w http.ResponseWriter`},
			},
		},
		{
			Pattern:     "/v1/vapid-public-key",
			HandlerFile: "mux.go",
			Ops: []bareOp{
				{"GET", "/v1/vapid-public-key", `"/v1/vapid-public-key"`},
			},
		},
		{
			Pattern:     "/v1/devices",
			HandlerFile: "handlers.go",
			Ops: []bareOp{
				{"GET", "/v1/devices", `strings.TrimPrefix(r.URL.Path, "/v1/devices")`},
				{"POST", "/v1/devices", `strings.TrimPrefix(r.URL.Path, "/v1/devices")`},
			},
		},
		{
			Pattern:     "/v1/devices/",
			HandlerFile: "handlers.go",
			Ops: []bareOp{
				{"PATCH", "/v1/devices/{id}", `h.patch(w, r, rest)`},
				{"DELETE", "/v1/devices/{id}", `h.remove(w, r, rest)`},
			},
		},
		{
			Pattern:     "/v1/keys",
			HandlerFile: "handlers.go",
			Ops: []bareOp{
				{"GET", "/v1/keys", `strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/keys"), "/")`},
				{"POST", "/v1/keys", `strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/keys"), "/")`},
			},
		},
		{
			Pattern:     "/v1/keys/",
			HandlerFile: "handlers.go",
			Ops: []bareOp{
				{"POST", "/v1/keys/{id}/revoke", `"/revoke"`},
			},
		},
		{
			Pattern:     "/v1/admin",
			HandlerFile: "admin_handlers.go",
			Ops: []bareOp{
				{"GET", "/v1/admin", `strings.TrimPrefix(r.URL.Path, "/v1/admin")`},
			},
		},
		{
			Pattern:     "/v1/admin/",
			HandlerFile: "admin_handlers.go",
			Ops: []bareOp{
				{"GET", "/v1/admin/stats", `case "stats":`},
				{"GET", "/v1/admin/users", `case "users":`},
				{"GET", "/v1/admin/users/{id}", `strings.HasPrefix(rest, "users/")`},
				{"PATCH", "/v1/admin/users/{id}", `strings.HasPrefix(rest, "users/")`},
				{"PATCH", "/v1/admin/users/{id}/role", `action == "role"`},
				{"POST", "/v1/admin/users/{id}/disable", `action == "disable"`},
				{"POST", "/v1/admin/users/{id}/enable", `action == "enable"`},
				{"GET", "/v1/admin/messages", `case "messages":`},
				{"GET", "/v1/admin/sessions", `case "sessions":`},
			},
		},
		{
			// 只读投影：非 GET 走显式 405 兜底（methodNotAllowedAllowGet），没有别的 spec operation。
			Pattern:     "/v1/namespaces/{ns}/edges",
			HandlerFile: "resource_capability.go",
			Reason:      "只读投影：GET 由更具体的 " + "`GET /v1/namespaces/{ns}/edges` 接住，这条裸 pattern 只回 405（methodNotAllowedAllowGet），不承载任何 spec operation",
		},
		{
			Pattern:     "/v1/namespaces/{ns}/deployments",
			HandlerFile: "retained_reads.go",
			Reason:      "只读投影：GET 由更具体的 " + "`GET /v1/namespaces/{ns}/deployments` 接住，这条裸 pattern 只回 405，不承载任何 spec operation",
		},
		{
			Pattern: "/health",
			Reason:  "显式排除（g3c §5.5）：健康检查不在五份 spec 内（不属于 /v1 API 面）",
		},
		{
			Pattern: "/debug/pprof/",
			Reason:  "显式排除（g3c §5.5）：pprof 只在 ANOTIFY_PPROF=1 时存在，不是 API",
		},
		{Pattern: "/debug/pprof/cmdline", Reason: "显式排除（g3c §5.5）：pprof"},
		{Pattern: "/debug/pprof/profile", Reason: "显式排除（g3c §5.5）：pprof"},
		{Pattern: "/debug/pprof/symbol", Reason: "显式排除（g3c §5.5）：pprof"},
		{Pattern: "/debug/pprof/trace", Reason: "显式排除（g3c §5.5）：pprof"},
		{
			Pattern: "/",
			Reason:  "显式排除（g3c §5.5）：静态资源兜底（SPA/内嵌资产），不是 API；它接住一切未注册路径，所以不参与前缀覆盖判定",
		},
	}
}

// bareMountProblems 检查裸挂载表与真实 mux / spec 的双向一致。
func bareMountProblems(patterns []routePattern, ops []specOp) []string {
	var problems []string
	astBare := map[string]bool{}
	for _, p := range patterns {
		if p.Method == "" {
			astBare[p.Path] = true
		}
	}
	table := map[string]bareMountEntry{}
	for _, entry := range bareMountSpec() {
		if _, dup := table[entry.Pattern]; dup {
			problems = append(problems, fmt.Sprintf("裸挂载表重复登记 %q", entry.Pattern))
		}
		table[entry.Pattern] = entry
		if !astBare[entry.Pattern] {
			problems = append(problems, fmt.Sprintf(
				"裸挂载表登记了 %q，但 mux 里没有这个裸 pattern（表和 mux 脱节）", entry.Pattern))
		}
		if len(entry.Ops) == 0 && entry.Reason == "" {
			problems = append(problems, fmt.Sprintf("裸挂载 %q 没有任何 operation，却没写 Reason", entry.Pattern))
		}
		if len(entry.Ops) > 0 && entry.Reason != "" {
			problems = append(problems, fmt.Sprintf("裸挂载 %q 既有 operation 又写了 Reason，二者只能有其一", entry.Pattern))
		}
		var handlerSrc string
		handlerLoaded := false
		for _, bo := range entry.Ops {
			if _, ok := findOp(ops, bo.Method, bo.Path); !ok {
				problems = append(problems, fmt.Sprintf(
					"裸挂载表 %q 里的 %s %s 在五份 spec 里找不到（表比 spec 多）", entry.Pattern, bo.Method, bo.Path))
			}
			if bo.Literal == "" || entry.HandlerFile == "" {
				problems = append(problems, fmt.Sprintf(
					"裸挂载表 %q 的 %s %s 没有 handler 判据（HandlerFile/Literal 必填）", entry.Pattern, bo.Method, bo.Path))
				continue
			}
			if !handlerLoaded {
				src, err := os.ReadFile(filepath.Join(serverPkgDir, entry.HandlerFile))
				if err != nil {
					problems = append(problems, fmt.Sprintf("裸挂载表 %q 的 handler 文件读不了：%v", entry.Pattern, err))
					handlerLoaded = true
					handlerSrc = ""
				} else {
					handlerSrc, handlerLoaded = string(src), true
				}
			}
			if !strings.Contains(handlerSrc, bo.Literal) {
				problems = append(problems, fmt.Sprintf(
					"裸挂载表与其 handler 代码脱节：%s %s 的判据 %q 在 %s 里找不到",
					bo.Method, bo.Path, bo.Literal, entry.HandlerFile))
			}
		}
	}
	// 反向 ①：mux 里的裸 pattern 必须都在表里。
	for _, p := range patterns {
		if p.Method != "" {
			continue
		}
		if _, ok := table[p.Path]; !ok {
			problems = append(problems, fmt.Sprintf(
				"mux 的裸 pattern %q（%s）不在裸挂载表里 —— 它带的 method 无人登记，spec 无法与它对齐", p.Path, p.Pos))
		}
	}
	// 反向 ②：任何 spec operation 若**真的**落到了某个裸 pattern 上，必须在那条表条目里。
	//
	// 用探针 mux 判「真的落到哪」而不是裸的前缀匹配：Go 1.22 是最长匹配优先，
	// `/v1/auth/password/login` 会被更具体的显式路由接住，不该再算到 `/v1/auth/` 头上。
	mux, muxProblems := newProbeMux(patterns)
	problems = append(problems, muxProblems...)
	for _, op := range ops {
		pat := probeMatch(mux, op.Method, samplePath(op.Path))
		if pat == "" {
			continue // 由 specToMuxProblems 报
		}
		if m, _, err := splitPattern(pat); err != nil || m != "" {
			continue
		}
		entry, ok := table[pat]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"spec 的 %s %s 被裸挂载 %q 接住，但该 pattern 不在裸挂载表里", op.Method, op.Path, pat))
			continue
		}
		listed := false
		for _, bo := range entry.Ops {
			if bo.Method == op.Method && canonicalPath(bo.Path) == op.Canonical {
				listed = true
			}
		}
		if !listed {
			problems = append(problems, fmt.Sprintf(
				"spec 的 %s %s 被裸挂载 %q 接住，但表里没登记（多写的 spec 也红）", op.Method, op.Path, pat))
		}
	}
	return problems
}

// bareEntryHas 报告某条裸 pattern 的表条目里是否登记了这个 operation。
func bareEntryHas(pattern string, op specOp) bool {
	entry, ok := bareEntry(pattern)
	if !ok {
		return false
	}
	for _, bo := range entry.Ops {
		if bo.Method == op.Method && canonicalPath(bo.Path) == op.Canonical {
			return true
		}
	}
	return false
}

// bareEntry 按 pattern 取裸挂载表的一条条目。
func bareEntry(pattern string) (bareMountEntry, bool) {
	for _, entry := range bareMountSpec() {
		if entry.Pattern == pattern {
			return entry, true
		}
	}
	return bareMountEntry{}, false
}

// findOp 按 (method, 原始 spec path) 找 operation。
func findOp(ops []specOp, method, path string) (specOp, bool) {
	for _, op := range ops {
		if op.Method == method && op.Path == path {
			return op, true
		}
	}
	return specOp{}, false
}

// ============================================================================
// 5. security 实测
// ============================================================================

// securityProbeException 是「spec 声明与实测状态码不一致」的**显式**例外。
//
// 禁止宽泛条件（比如“401/403 都行”）：每条例外都要写清理由 + 断言一个真实状态码。
type securityProbeException struct {
	Want   int
	Reason string
}

// securityProbeExceptions 是唯一允许偏离「有安全方案 = 401 / 匿名 = 非 401」的清单。
var securityProbeExceptions = map[string]securityProbeException{
	"GET /v1/node/stream": {
		Want: http.StatusUpgradeRequired,
		Reason: "WS 升级失败在鉴权之前：登录发生在升级后的首帧 hello（internal/ws/hub.go）；" +
			"无凭据的普通 HTTP 请求连握手都过不了（426 Upgrade Required），所以拿不到 401。" +
			"带 token 的节点同样先升级再校验。",
	},
	"POST /v1/auth/login": {
		Want: http.StatusUnauthorized,
		Reason: "匿名登录端点（**不**包 sessMW），但这个 401 是 handler 自己回的 **WebAuthn 断言校验失败**" +
			"（凭据错），不是中间件拦的。这也是 api/auth.yaml:138 自己写下的约定（“失败统一 401，" +
			"不区分用户不存在 / 断言无效 / 账户禁用”），所以它不是 spec 与实现的漂移，只是不能套用" +
			"“匿名 → 不得 401”那条默认规则。识别方式：同为匿名的 POST /v1/auth/register 空 body 回 400，" +
			"不会被拦成 401。",
	},
}

// observeSecurityStatuses 用**真实装配**（newProductionAppForTest）对每条 operation 发一次
// 无凭据请求，返回 (method, path) → 状态码。
func observeSecurityStatuses(t *testing.T, ops []specOp) map[string]int {
	t.Helper()
	app := newProductionAppForTest(t)
	observed := make(map[string]int, len(ops))
	for _, op := range ops {
		rr := appDo(t, app, op.Method, samplePath(op.Path), nil)
		observed[op.Key()] = rr.Code
	}
	return observed
}

// securityProblems 是纯函数：把 spec 声明的鉴权与实测状态码对账。
func securityProblems(ops []specOp, observed map[string]int) []string {
	var problems []string
	for _, op := range ops {
		code, ok := observed[op.Key()]
		if !ok {
			problems = append(problems, fmt.Sprintf("security 实测缺数据：%s %s 没有发过请求", op.Method, op.Path))
			continue
		}
		if ex, has := securityProbeExceptions[op.Key()]; has {
			if code != ex.Want {
				problems = append(problems, fmt.Sprintf(
					"security 例外 %s 的实测状态码 %d 与登记的 %d 不一致（理由：%s）",
					op.Key(), code, ex.Want, ex.Reason))
			}
			continue
		}
		if op.Anonymous() {
			if code == http.StatusUnauthorized {
				problems = append(problems, fmt.Sprintf(
					"spec 声明匿名但实测 401：%s %s（%s，security: []）—— 要么补 security，要么中间件挂错了",
					op.Method, op.Path, op.File))
			}
			continue
		}
		if code != http.StatusUnauthorized {
			problems = append(problems, fmt.Sprintf(
				"spec 声明需要鉴权 %v，但无凭据请求实测 %d（不是 401）：%s %s（%s，operationId=%s）—— 真实中间件与文档不一致",
				op.Security, code, op.Method, op.Path, op.File, op.OperationID))
		}
	}
	// 例外表过期即红：例外指向的 operation 必须真的存在。
	for key := range securityProbeExceptions {
		method, path, ok := strings.Cut(key, " ")
		if !ok {
			problems = append(problems, fmt.Sprintf("security 例外表的键 %q 不是 \"METHOD /path\" 形态", key))
			continue
		}
		if _, found := findOp(ops, method, path); !found {
			problems = append(problems, fmt.Sprintf(
				"security 例外表已过期：spec 里已经没有 %s（%s）", key, securityProbeExceptions[key].Reason))
		}
	}
	return problems
}

// ============================================================================
// 6. DTO 双向字段
// ============================================================================

// rawMessageType 是「不透明透传」标记：形状由用户/实现单方面决定，只核存在性。
var rawMessageType = reflect.TypeOf(json.RawMessage{})

// dtoExclusion 是一条例外（必须写理由，且必须可过期）。
type dtoExclusion struct {
	// Reason 是给人看的依据（必填）。
	Reason string
	// Witness 是 onlySchema 的机械证据：某个「在别处把该键发出去」的 Go 值。
	// 例：StatusDetails 的 valuesDigest/conflicts/hint 由 playbookConflictDetails 发出。
	Witness any
}

// parityPair 是「一个 Go 类型 ↔ 一份文件里的一层 schema」。
type parityPair struct {
	// GoLabel 是失败信息里的人类可读名。
	GoLabel string
	// File 是 schema 所在文件（五份真源之一）。
	File string
	// Schema 是 components.schemas 下的名字（支持点号下钻）。
	Schema string
	// Sample 是该类型的零值（用 reflect.TypeOf 取类型；匿名类型也能这么拿）。
	Sample any
	// OnlyGo / OnlySchema 是例外表：键是 json 字段名。
	OnlyGo     map[string]dtoExclusion
	OnlySchema map[string]dtoExclusion
}

// parityPairs 是本门禁的对照表：覆盖全部 kind 的信封/状态面 + 全部请求体/回执/例外文档。
//
// 加一行就能覆盖一对；加错了这里立刻会红。**删行要同时改 TestOpenAPIParityDTOFields 的下限**
// （削表守卫），理由必须写进 PR。
func parityPairs() []parityPair {
	return []parityPair{
		// ---- 信封与公共件（resource.yaml）----
		{GoLabel: "objectMeta", File: "resource.yaml", Schema: "ObjectMeta", Sample: objectMeta{}},
		{GoLabel: "listMeta", File: "resource.yaml", Schema: "ListMeta", Sample: listMeta{}},
		{GoLabel: "apiList", File: "resource.yaml", Schema: "CapabilityList", Sample: apiList{}},
		{GoLabel: "apiList", File: "resource.yaml", Schema: "SourceList", Sample: apiList{}},
		{GoLabel: "apiList", File: "resource.yaml", Schema: "NodeList", Sample: apiList{}},
		{GoLabel: "apiList", File: "resource.yaml", Schema: "TaskList", Sample: apiList{}},
		{GoLabel: "apiList", File: "resource.yaml", Schema: "PlaybookList", Sample: apiList{}},
		{GoLabel: "apiList", File: "resource.yaml", Schema: "NamespaceList", Sample: apiList{}},
		{GoLabel: "statusDoc", File: "resource.yaml", Schema: "Status", Sample: statusDoc{}},
		{
			GoLabel: "statusDetails", File: "resource.yaml", Schema: "StatusDetails", Sample: statusDetails{},
			OnlySchema: map[string]dtoExclusion{
				"valuesDigest": {Reason: "playbook_conflict 的 400 专用 details（W3 冻结形状），由 playbookConflictDetails 发出", Witness: playbookConflictDetails{}},
				"conflicts":    {Reason: "playbook_conflict 的逐条冲突矩阵，由 playbookConflictDetails 发出", Witness: playbookConflictDetails{}},
				"hint":         {Reason: "playbook_conflict 的处置提示，由 playbookConflictDetails 发出", Witness: playbookConflictDetails{}},
			},
		},
		{GoLabel: "statusCause", File: "resource.yaml", Schema: "StatusCause", Sample: statusCause{}},

		// ---- 全部 kind 的信封（apiEnvelope = {apiVersion,kind,metadata,spec,status}）----
		// 信封本身只有一个 Go 类型（apiEnvelope），六份 kind schema 必须与它同形；
		// spec/status 各自有专表（下面），这里只錨住四个公共键。
		{GoLabel: "apiEnvelope", File: "resource.yaml", Schema: "Capability", Sample: apiEnvelope{}},
		{GoLabel: "apiEnvelope", File: "resource.yaml", Schema: "Source", Sample: apiEnvelope{}},
		{GoLabel: "apiEnvelope", File: "resource.yaml", Schema: "Playbook", Sample: apiEnvelope{}},
		{GoLabel: "apiEnvelope", File: "resource.yaml", Schema: "Namespace", Sample: apiEnvelope{}},
		{GoLabel: "apiEnvelope", File: "resource.yaml", Schema: "Task", Sample: apiEnvelope{}},
		// Node 是唯一带顶层 context 的 kind（§7.4「有效值必须带 ns 上下文」）。
		{GoLabel: "nodeReadEnvelope", File: "resource.yaml", Schema: "Node", Sample: nodeReadEnvelope{}},
		// Namespace.spec 当前是空对象（v4 不接 seedBuiltinCapabilities）。
		{GoLabel: "namespaceSpec", File: "resource.yaml", Schema: "NamespaceSpec", Sample: namespaceSpec{}},

		// ---- capability ----
		{GoLabel: "capabilityStatus", File: "resource.yaml", Schema: "CapabilityStatus", Sample: capabilityStatus{}},
		{GoLabel: "capabilityCondition", File: "resource.yaml", Schema: "CapabilityCondition", Sample: capabilityCondition{}},
		{GoLabel: "capabilityNodeState", File: "resource.yaml", Schema: "CapabilityNodeState", Sample: capabilityNodeState{}},
		{GoLabel: "capabilityResolved", File: "resource.yaml", Schema: "CapabilityResolved", Sample: capabilityResolved{}},
		{GoLabel: "capabilityResolvedNext", File: "resource.yaml", Schema: "CapabilityResolvedNext", Sample: capabilityResolvedNext{}},
		{GoLabel: "capabilityDeployRequest", File: "resource.yaml", Schema: "CapabilityDeployRequest", Sample: capabilityDeployRequest{}},
		{GoLabel: "capabilityDeployResponse", File: "resource.yaml", Schema: "CapabilityDeployResponse", Sample: capabilityDeployResponse{}},
		{GoLabel: "capabilityDeployOutcome", File: "resource.yaml", Schema: "CapabilityDeployOutcome", Sample: capabilityDeployOutcome{}},
		{GoLabel: "capabilityEdgeView", File: "resource.yaml", Schema: "CapabilityEdge", Sample: capabilityEdgeView{}},

		// ---- source ----
		{GoLabel: "srcSpecView", File: "resource.yaml", Schema: "SourceSpec", Sample: srcSpecView{}},
		{GoLabel: "srcStatusView", File: "resource.yaml", Schema: "SourceStatus", Sample: srcStatusView{}},
		{GoLabel: "sourcespec.Target", File: "resource.yaml", Schema: "SourceTarget", Sample: sourcespec.Target{}},
		{GoLabel: "srcReceiptDoc", File: "resource.yaml", Schema: "SourceReceipt", Sample: srcReceiptDoc{}},
		{GoLabel: "srcReceiptTask", File: "resource.yaml", Schema: "SourceReceiptTask", Sample: srcReceiptTask{}},
		{GoLabel: "route.Prune", File: "resource.yaml", Schema: "PruneFact", Sample: route.Prune{}},
		{GoLabel: "srcPreviewDoc", File: "resource.yaml", Schema: "SourcePreview", Sample: srcPreviewDoc{}},
		{GoLabel: "srcPreviewTarget", File: "resource.yaml", Schema: "SourcePreviewTarget", Sample: srcPreviewTarget{}},

		// ---- playbook ----
		{GoLabel: "playbookStatus", File: "resource.yaml", Schema: "PlaybookStatus", Sample: playbookStatus{}},
		{GoLabel: "playbookCondition", File: "resource.yaml", Schema: "PlaybookCondition", Sample: playbookCondition{}},
		{GoLabel: "playbookItem", File: "resource.yaml", Schema: "PlaybookItem", Sample: playbookItem{}},
		{GoLabel: "playbookSourceStatus", File: "resource.yaml", Schema: "PlaybookSourceStatus", Sample: playbookSourceStatus{}},
		{GoLabel: "playbook.UnappliedEntry", File: "resource.yaml", Schema: "PlaybookUnappliedEntry", Sample: playbook.UnappliedEntry{}},
		{GoLabel: "playbook.Receipt", File: "resource.yaml", Schema: "PlaybookReceipt", Sample: playbook.Receipt{}},
		{GoLabel: "playbook.RenderResult", File: "resource.yaml", Schema: "PlaybookRenderResult", Sample: playbook.RenderResult{}},
		{GoLabel: "store.PlaybookInstallList", File: "resource.yaml", Schema: "PlaybookInstallList", Sample: store.PlaybookInstallList{}},
		{GoLabel: "store.PlaybookInstallGroup", File: "resource.yaml", Schema: "PlaybookInstallGroup", Sample: store.PlaybookInstallGroup{}},
		{GoLabel: "store.PlaybookDeleteResult", File: "resource.yaml", Schema: "PlaybookDeleteResult", Sample: store.PlaybookDeleteResult{}},
		{GoLabel: "store.PlaybookKept", File: "resource.yaml", Schema: "PlaybookKept", Sample: store.PlaybookKept{}},

		// ---- namespace / system config ----
		{GoLabel: "namespaceCounts", File: "resource.yaml", Schema: "NamespaceCounts", Sample: namespaceCounts{}},
		{GoLabel: "namespaceAggregate", File: "resource.yaml", Schema: "NamespaceAggregate", Sample: namespaceAggregate{}},
		{GoLabel: "namespaceStatus", File: "resource.yaml", Schema: "NamespaceStatus", Sample: namespaceStatus{}},
		{GoLabel: "namespaceConfigDoc", File: "resource.yaml", Schema: "NamespaceConfig", Sample: namespaceConfigDoc{}},
		{GoLabel: "systemConfigDoc", File: "resource.yaml", Schema: "SystemConfig", Sample: systemConfigDoc{}},
		{GoLabel: "retentionDoc", File: "resource.yaml", Schema: "Retention", Sample: retentionDoc{}},

		// ---- node ----
		{GoLabel: "nodeSpec", File: "resource.yaml", Schema: "NodeSpec", Sample: nodeSpec{}},
		{GoLabel: "nodeStatus", File: "resource.yaml", Schema: "NodeStatus", Sample: nodeStatus{}},
		{GoLabel: "nodeCapacity", File: "resource.yaml", Schema: "NodeCapacity", Sample: nodeCapacity{}},
		{GoLabel: "nodeInstanceRef", File: "resource.yaml", Schema: "NodeInstanceRef", Sample: nodeInstanceRef{}},
		{GoLabel: "nodeConfigWrapper", File: "resource.yaml", Schema: "NodeConfig", Sample: nodeConfigWrapper{}},
		{GoLabel: "nodeTokenDoc", File: "resource.yaml", Schema: "NodeToken", Sample: nodeTokenDoc{}},
		{GoLabel: "nodeConfigBatchRequest", File: "resource.yaml", Schema: "NodeConfigBatchRequest", Sample: nodeConfigBatchRequest{}},
		{GoLabel: "nodeConfigBatchUpdate", File: "resource.yaml", Schema: "NodeConfigBatchUpdate", Sample: nodeConfigBatchUpdate{}},
		{GoLabel: "nodeConfigBatchResponse", File: "resource.yaml", Schema: "NodeConfigBatchResponse", Sample: nodeConfigBatchResponse{}},
		{GoLabel: "nodeConfigBatchItem", File: "resource.yaml", Schema: "NodeConfigBatchItem", Sample: nodeConfigBatchItem{}},

		// ---- task ----
		{GoLabel: "taskSpecDoc", File: "resource.yaml", Schema: "TaskSpec", Sample: taskSpecDoc{}},
		{GoLabel: "taskCapabilityRef", File: "resource.yaml", Schema: "TaskCapabilityRef", Sample: taskCapabilityRef{}},
		{GoLabel: "taskStatusDoc", File: "resource.yaml", Schema: "TaskStatus", Sample: taskStatusDoc{}},
		{GoLabel: "taskResultDoc", File: "resource.yaml", Schema: "TaskResult", Sample: taskResultDoc{}},
		{GoLabel: "taskResultErrorDoc", File: "resource.yaml", Schema: "TaskResultError", Sample: taskResultErrorDoc{}},
		{GoLabel: "taskEnvDiagnostic", File: "resource.yaml", Schema: "TaskEnvDiagnostic", Sample: taskEnvDiagnostic{}},
		{GoLabel: "taskLogsDoc", File: "resource.yaml", Schema: "TaskLogs", Sample: taskLogsDoc{}},
		{GoLabel: "protocol.LogLine", File: "resource.yaml", Schema: "TaskLogLine", Sample: protocol.LogLine{}},
		{GoLabel: "taskEmissionsList", File: "resource.yaml", Schema: "TaskEmissions", Sample: taskEmissionsList{}},
		{GoLabel: "taskEmissionItem", File: "resource.yaml", Schema: "TaskEmissionItem", Sample: taskEmissionItem{}},
		{GoLabel: "protocol.TaskArtifact", File: "resource.yaml", Schema: "TaskArtifactRef", Sample: protocol.TaskArtifact{}},

		// ---- 事件域（events.yaml）----
		{GoLabel: "apiEnvelope", File: "events.yaml", Schema: "Event", Sample: apiEnvelope{}},
		{GoLabel: "apiList", File: "events.yaml", Schema: "EventList", Sample: apiList{}},
		{GoLabel: "eventSpecDoc", File: "events.yaml", Schema: "EventSpec", Sample: eventSpecDoc{}},
		{GoLabel: "eventObjectRef", File: "events.yaml", Schema: "EventObjectRef", Sample: eventObjectRef{}},
		{GoLabel: "eventStatusDoc", File: "events.yaml", Schema: "EventStatus", Sample: eventStatusDoc{}},
		{GoLabel: "pruneFactDoc", File: "events.yaml", Schema: "EventPruneFact", Sample: pruneFactDoc{}},
		{GoLabel: "store.Stats", File: "events.yaml", Schema: "Stats", Sample: store.Stats{}},
		{GoLabel: "store.DayCount", File: "events.yaml", Schema: "DayCount", Sample: store.DayCount{}},
		{GoLabel: "store.OrphanPendingTask", File: "events.yaml", Schema: "OrphanTask", Sample: store.OrphanPendingTask{}},
		{GoLabel: "store.Deployment", File: "resource.yaml", Schema: "Deployment", Sample: store.Deployment{}},

		// ---- agent.yaml 的已冻结帧/DTO（协议动作）----
		{GoLabel: "protocol.LogLine", File: "agent.yaml", Schema: "LogLine", Sample: protocol.LogLine{}},
		{GoLabel: "protocol.TaskArtifact", File: "agent.yaml", Schema: "TaskArtifact", Sample: protocol.TaskArtifact{}},
	}
}

// dtoProblems 是纯函数：逐对核对 Go 结构体与 schema（双向 + 递归 + 例外过期检测）。
//
// 例外表按 **Go 类型**登记（不是按「对」）：`statusDetails` 既出现在 `Status.details`
// 也会经 `NodeConfigBatchItem.error.details` 被递归到，例外必须对所有出现位置生效。
func dtoProblems(docs specDocs, pairs []parityPair) []string {
	c := &dtoChecker{docs: docs, exclusions: map[reflect.Type]dtoExclusionSet{}}
	for _, pair := range pairs {
		if pair.Sample == nil {
			continue
		}
		t := reflect.TypeOf(pair.Sample)
		for t.Kind() == reflect.Ptr {
			t = t.Elem()
		}
		if len(pair.OnlyGo) > 0 || len(pair.OnlySchema) > 0 {
			c.exclusions[t] = dtoExclusionSet{onlyGo: pair.OnlyGo, onlySchema: pair.OnlySchema}
		}
	}
	for _, pair := range pairs {
		c.comparePair(pair)
	}
	return c.problems
}

// dtoExclusionSet 是某个 Go 类型上的例外（onlyGo / onlySchema）。
type dtoExclusionSet struct {
	onlyGo     map[string]dtoExclusion
	onlySchema map[string]dtoExclusion
}

type dtoChecker struct {
	docs       specDocs
	exclusions map[reflect.Type]dtoExclusionSet
	problems   []string
}

func (c *dtoChecker) addf(format string, args ...any) {
	c.problems = append(c.problems, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

// lookupSchema 取 `file` 的 components.schemas 下一层（支持点号下钻）并解开 $ref/allOf。
func (c *dtoChecker) lookupSchema(file, name string) (map[string]any, string, bool) {
	doc, ok := c.docs[file]
	if !ok {
		c.addf("DTO 对照表里的 %s 指向了没载入的文件 %s", name, file)
		return nil, file, false
	}
	schemas, ok := nestedMap(doc, "components", "schemas")
	if !ok {
		c.addf("%s 没有 components.schemas", file)
		return nil, file, false
	}
	var node any = schemas
	for _, part := range strings.Split(name, ".") {
		m, ok := node.(map[string]any)
		if !ok {
			c.addf("%s 的 components.schemas.%s 走到了非对象节点", file, name)
			return nil, file, false
		}
		v, ok := m[part]
		if !ok {
			c.addf("%s 的 components.schemas.%s 不存在", file, name)
			return nil, file, false
		}
		node = v
	}
	schema, ok := node.(map[string]any)
	if !ok {
		c.addf("%s 的 components.schemas.%s 不是 schema 对象", file, name)
		return nil, file, false
	}
	resolved, resolvedFile, err := c.docs.resolveSchemaNode(file, schema)
	if err != nil {
		c.addf("%s 的 components.schemas.%s 解析 $ref 失败：%v", file, name, err)
		return nil, file, false
	}
	return resolved, resolvedFile, true
}

// comparePair 核对一对（Go 类型, schema）。
func (c *dtoChecker) comparePair(pair parityPair) {
	goType := reflect.TypeOf(pair.Sample)
	if goType == nil {
		c.addf("DTO 对照表里的 %s 没有 Sample（无法取到 Go 类型）", pair.GoLabel)
		return
	}
	schema, file, ok := c.lookupSchema(pair.File, pair.Schema)
	if !ok {
		return
	}
	c.checkExclusions(pair, goType, schema)
	c.compareStruct(goType, schema, file, pair.GoLabel, "components.schemas."+pair.Schema)
}

// checkExclusions 做**过期检测**：例外一旦不再需要就红。
func (c *dtoChecker) checkExclusions(pair parityPair, goType reflect.Type, schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	fields := jsonFields(goType)

	for _, name := range sortedKeys(pair.OnlyGo) {
		ex := pair.OnlyGo[name]
		if ex.Reason == "" {
			c.addf("%s：onlyGo 例外 %q 没写理由（例外必须逐条写清依据）", pair.GoLabel, name)
		}
		if _, exists := props[name]; exists {
			c.addf("%s：onlyGo 例外已过期 —— schema 现在**已经**声明了 `%s`，请从 onlyGo 里删掉", pair.GoLabel, name)
		}
		if _, exists := fields[name]; !exists {
			c.addf("%s：onlyGo 例外已过期 —— Go 里已经没有 json tag `%s`，请从 onlyGo 里删掉", pair.GoLabel, name)
		}
	}

	for _, name := range sortedKeys(pair.OnlySchema) {
		ex := pair.OnlySchema[name]
		if ex.Reason == "" {
			c.addf("%s：onlySchema 例外 %q 没写理由（例外必须逐条写清依据）", pair.GoLabel, name)
		}
		if _, exists := fields[name]; exists {
			c.addf("%s：onlySchema 例外已过期 —— Go 现在把 `%s` 放上 wire 了，请从 onlySchema 里删掉", pair.GoLabel, name)
			continue
		}
		if hasInternalField(goType, name) {
			continue
		}
		if ex.Witness != nil {
			if _, ok := jsonFields(reflect.TypeOf(ex.Witness))[name]; ok {
				continue
			}
			c.addf("%s：onlySchema 例外 %q 的 Witness 结构体里没有这个 json tag —— 依据不成立", pair.GoLabel, name)
			continue
		}
		c.addf("%s：onlySchema 例外 %q 没有依据 —— Go 里既没有同名 json tag，也没有 json:\"-\" 的同名字段，还没有 Witness（理由写的是：%s）",
			pair.GoLabel, name, ex.Reason)
	}
}

// hasInternalField 报告 t 里是否有被显式标成 `json:"-"` 的同名字段（内部字段的合法依据）。
func hasInternalField(t reflect.Type, name string) bool {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return false
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !strings.EqualFold(f.Name, name) {
			continue
		}
		if strings.Split(f.Tag.Get("json"), ",")[0] == "-" {
			return true
		}
	}
	return false
}

// compareStruct 核对一个结构体与一份 schema 对象（双向 + 递归）。
func (c *dtoChecker) compareStruct(goType reflect.Type, schema map[string]any, file, goLabel, schemaPath string) {
	for goType.Kind() == reflect.Ptr {
		goType = goType.Elem()
	}
	if goType.Kind() != reflect.Struct {
		c.addf("%s：Go 侧不是 struct（%s），无法做字段级比对", goLabel, goType.Kind())
		return
	}
	// 宽投影（additionalProperties 且没有 properties）：形状由实现单方面决定，
	// 字段集无从核对。这里不“跳过整对” —— 调用方（TestOpenAPIParityDTOFields）会断言
	// 这两个 schema 确实还是宽的；一旦它们长出 properties，比对就会真的开始跑。
	if wideSchema(schema) {
		return
	}
	excl := c.exclusions[goType]
	props, _ := schema["properties"].(map[string]any)
	fields := jsonFields(goType)

	for _, req := range stringList(schema["required"]) {
		if _, ok := props[req]; !ok {
			c.addf("%s 的 required 里列了 `%s`，但 properties 里没有它 —— 文档要求发一个它自己没定义的字段", schemaPath, req)
		}
	}

	// 方向 A：Go 有 → schema 必须有。
	for _, name := range sortedKeys(fields) {
		prop, ok := props[name]
		if !ok {
			if _, excluded := excl.onlyGo[name]; excluded {
				continue
			}
			c.addf("%s.%s 在 Go 的 wire 形状里存在，但 %s.properties 里没有 —— 按 schema 生成的客户端永远提交不了它",
				goLabel, name, schemaPath)
			continue
		}
		propSchema, _ := prop.(map[string]any)
		c.compareValue(fields[name], propSchema, file, goLabel+"."+name, schemaPath+".properties."+name)
	}

	// 方向 B：schema 有 → Go 必须有。
	for _, name := range sortedKeys(props) {
		if _, ok := fields[name]; ok {
			continue
		}
		if _, excluded := excl.onlySchema[name]; excluded {
			continue
		}
		c.addf("%s.properties.%s 声明了这个字段，但 Go 的 %s 里没有对应的 json tag —— 文档在骗人：照它写的客户端会读一个永远不出现的键",
			schemaPath, name, goLabel)
	}
}

// compareValue 按 Go 的类型种类继续下钻。
func (c *dtoChecker) compareValue(goType reflect.Type, schema map[string]any, file, goPath, schemaPath string) {
	if schema == nil {
		return // 方向 A 已经报过
	}
	resolved, resolvedFile, err := c.docs.resolveSchemaNode(file, schema)
	if err != nil {
		c.addf("%s 的 schema $ref 解析失败：%v", schemaPath, err)
		return
	}
	schema, file = resolved, resolvedFile
	for goType.Kind() == reflect.Ptr {
		goType = goType.Elem()
	}
	if goType == rawMessageType {
		return // 不透明透传：只核存在性
	}
	switch goType.Kind() {
	case reflect.Struct:
		c.checkType(schema, "object", goPath)
		c.compareStruct(goType, schema, file, goPath, schemaPath)
	case reflect.Map:
		c.checkType(schema, "object", goPath)
		if _, fixed := schema["properties"]; fixed {
			c.addf("%s：Go 那边是 map（键由调用方决定），schema 却写死了 properties —— 文档声明了一批并不存在固定字段", goPath)
		}
		if ap, ok := schema["additionalProperties"].(map[string]any); ok {
			c.compareValue(goType.Elem(), ap, file, goPath+"{}", schemaPath+".additionalProperties")
		}
	case reflect.Slice, reflect.Array:
		c.checkType(schema, "array", goPath)
		items, ok := schema["items"].(map[string]any)
		if !ok {
			c.addf("%s：schema 说是 array 却没有 items —— 元素形状没有声明", goPath)
			return
		}
		c.compareValue(goType.Elem(), items, file, goPath+"[]", schemaPath+".items")
	case reflect.String:
		c.checkType(schema, "string", goPath)
	case reflect.Bool:
		c.checkType(schema, "boolean", goPath)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		c.checkType(schema, "integer", goPath)
	case reflect.Float32, reflect.Float64:
		c.checkType(schema, "number", goPath)
	case reflect.Interface:
		// any / interface{}：形状由文档单方面描述，无从核对。
	}
}

// checkType 核对 `type`（文档没写 type 时不判：本仓库有几处宽松写法）。
func (c *dtoChecker) checkType(schema map[string]any, want, goPath string) {
	got, declared := schema["type"].(string)
	if !declared || got == want {
		return
	}
	c.addf("%s：openapi 声明 type=%s，Go 那边是 %s —— 两边说的不是同一种值", goPath, got, want)
}

// wideSchema 报告 schema 是否用 additionalProperties 表达了「宽投影」（没有 properties）。
func wideSchema(schema map[string]any) bool {
	if _, hasProps := schema["properties"]; hasProps {
		return false
	}
	if _, hasAllOf := schema["allOf"]; hasAllOf {
		return false
	}
	switch v := schema["additionalProperties"].(type) {
	case bool:
		return v
	case map[string]any:
		return true
	}
	return false
}

// jsonFields 按 encoding/json 的规则把一个结构体摊成 wire 字段表。
//
//   - `json:"-"` → 跳过（显式不上 wire 的内部字段）
//   - 没有 tag → 用 Go 字段名（encoding/json 的回落规则）
//   - 匿名内嵌结构体 → 拍平（promoted fields）
//   - `omitempty` → **不影响**：可以缺省不等于可以不声明
//   - 未导出字段 → encoding/json 根本看不到
func jsonFields(t reflect.Type) map[string]reflect.Type {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	out := map[string]reflect.Type{}
	if t.Kind() != reflect.Struct {
		return out
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			// 未导出字段：encoding/json 看不见 —— 唯一例外是**匿名的未导出结构体**：
			// 内嵌类型的名字小写、但它自己的字段是导出的，那些字段仍会被提升
			// （nodeReadEnvelope 内嵌 apiEnvelope 就是这种形状）。
			embedded := f.Type
			for embedded.Kind() == reflect.Ptr {
				embedded = embedded.Elem()
			}
			if !(f.Anonymous && embedded.Kind() == reflect.Struct) {
				continue
			}
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if f.Anonymous && name == "" {
			embedded := f.Type
			for embedded.Kind() == reflect.Ptr {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				for k, v := range jsonFields(embedded) {
					if _, dup := out[k]; !dup {
						out[k] = v
					}
				}
				continue
			}
		}
		if name == "" {
			name = f.Name
		}
		if _, dup := out[name]; dup {
			continue
		}
		out[name] = f.Type
	}
	return out
}

// ============================================================================
// 变异辅助（只改内存里的 doc，绝不写盘）
// ============================================================================

// mustReloadDocs 重新从磁盘载入一份**独立**的文档树（变异用；互不影响）。
func mustReloadDocs(t *testing.T) specDocs {
	t.Helper()
	docs, problems := loadSpecDocs()
	fatalIfProblems(t, "变异前重新载入 spec", problems)
	return docs
}

// mustPathItem 取某文件里的一条 path item（不存在就 Fatal）。
func mustPathItem(t *testing.T, docs specDocs, file, path string) map[string]any {
	t.Helper()
	doc, ok := docs[file]
	if !ok {
		t.Fatalf("变异失败：%s 没载入", file)
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatalf("变异失败：%s 没有 paths", file)
	}
	item, ok := paths[path].(map[string]any)
	if !ok {
		t.Fatalf("变异失败：%s 里没有 path %q", file, path)
	}
	return item
}

func deletePathOp(t *testing.T, docs specDocs, file, path, method string) {
	t.Helper()
	item := mustPathItem(t, docs, file, path)
	if _, ok := item[method]; !ok {
		t.Fatalf("变异失败：%s %s %s 本来就不存在", file, method, path)
	}
	delete(item, method)
}

func addPathOp(t *testing.T, docs specDocs, file, path, method string, op map[string]any) {
	t.Helper()
	item := mustPathItemOrCreate(t, docs, file, path)
	item[method] = op
}

func mustPathItemOrCreate(t *testing.T, docs specDocs, file, path string) map[string]any {
	t.Helper()
	doc := docs[file]
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatalf("变异失败：%s 没有 paths", file)
	}
	item, ok := paths[path].(map[string]any)
	if !ok {
		item = map[string]any{}
		paths[path] = item
	}
	return item
}

func setOperationID(t *testing.T, docs specDocs, file, path, method, id string) {
	t.Helper()
	item := mustPathItem(t, docs, file, path)
	op, ok := item[method].(map[string]any)
	if !ok {
		t.Fatalf("变异失败：%s 里没有 %s %s", file, method, path)
	}
	op["operationId"] = id
}

func setSecurity(t *testing.T, docs specDocs, file, path, method string, sec []any) {
	t.Helper()
	item := mustPathItem(t, docs, file, path)
	op, ok := item[method].(map[string]any)
	if !ok {
		t.Fatalf("变异失败：%s 里没有 %s %s", file, method, path)
	}
	op["security"] = sec
}

func schemaForMutation(t *testing.T, docs specDocs, file, name string) map[string]any {
	t.Helper()
	doc, ok := docs[file]
	if !ok {
		t.Fatalf("变异失败：%s 没载入", file)
	}
	schemas, ok := nestedMap(doc, "components", "schemas")
	if !ok {
		t.Fatalf("变异失败：%s 没有 components.schemas", file)
	}
	schema, ok := schemas[name].(map[string]any)
	if !ok {
		t.Fatalf("变异失败：%s 没有 schema %s", file, name)
	}
	return schema
}

func deleteSchemaProperty(t *testing.T, docs specDocs, file, schema, prop string) {
	t.Helper()
	props, ok := schemaForMutation(t, docs, file, schema)["properties"].(map[string]any)
	if !ok {
		t.Fatalf("变异失败：%s 的 %s 没有 properties", file, schema)
	}
	if _, ok := props[prop]; !ok {
		t.Fatalf("变异失败：%s 的 %s 本来就没有 %q", file, schema, prop)
	}
	delete(props, prop)
}

func addSchemaProperty(t *testing.T, docs specDocs, file, schema, prop string) {
	t.Helper()
	s := schemaForMutation(t, docs, file, schema)
	props, ok := s["properties"].(map[string]any)
	if !ok {
		props = map[string]any{}
		s["properties"] = props
	}
	props[prop] = map[string]any{"type": "string"}
}

// setRef 沿点号路径找到一层 schema 对象并改写它的 `$ref`。
func setRef(t *testing.T, docs specDocs, file, path, ref string) {
	t.Helper()
	doc := docs[file]
	var node any = doc
	parts := strings.Split(path, ".")
	for _, part := range parts {
		m, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("变异失败：%s 的 %s 走到了非对象节点（段 %q）", file, path, part)
		}
		v, ok := m[part]
		if !ok {
			t.Fatalf("变异失败：%s 的 %s 在段 %q 处不存在", file, path, part)
		}
		node = v
	}
	m, ok := node.(map[string]any)
	if !ok {
		t.Fatalf("变异失败：%s 的 %s 不是对象", file, path)
	}
	if _, had := m["$ref"]; !had {
		t.Fatalf("变异失败：%s 的 %s 本来没有 $ref", file, path)
	}
	m["$ref"] = ref
}

// requireProblem 断言 problems 里有一条包含 wantSubstr 的消息（变异必须被打红）。
func requireProblem(t *testing.T, mutation string, problems []string, wantSubstr string) {
	t.Helper()
	for _, p := range problems {
		if strings.Contains(p, wantSubstr) {
			return
		}
	}
	t.Fatalf("变异「%s」没有被抓到：期望出现包含 %q 的失败，实际 problems=%v",
		mutation, wantSubstr, problems)
}

// fatalIfProblems 把 problems 一次性打出来（不是遇到第一条就 Fatal：修一次能修一片）。
func fatalIfProblems(t *testing.T, stage string, problems []string) {
	t.Helper()
	if len(problems) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s：%d 条问题", stage, len(problems))
	for _, p := range problems {
		b.WriteString("\n  - ")
		b.WriteString(p)
	}
	t.Fatal(b.String())
}

// ============================================================================
// 通用小工具
// ============================================================================

// nestedMap 沿键路径取嵌套 map。
func nestedMap(root map[string]any, keys ...string) (map[string]any, bool) {
	var node any = root
	for _, k := range keys {
		m, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := m[k]
		if !ok {
			return nil, false
		}
		node = v
	}
	m, ok := node.(map[string]any)
	return m, ok
}

// stringList 把 YAML 里的字符串序列取出来。
func stringList(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// sortedKeys 让所有失败信息的顺序稳定（map 遍历顺序随机，不稳定就不好 diff）。
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// containsString 是小集合的线性查找（避免为一行判断引入 slices 依赖）。
func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
