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
//   - 例外表（`securityProbeExceptions` / onlyGo / onlySchema / `wideSchemas` /
//     `unpairedSchemas` / 三张 status 例外表）**过期即红**：某条例外不再需要（两边都有了 /
//     都没有了 / spec 里那条 operation 没了 / 基线期望已经与例外同向）就失败，防止例外变成
//     永久后门。
//   - **每个 component schema 都有归属**（F1）：228 个 schema 必须要么在 `parityPairs` 里，
//     要么被某对 pair 顺着 `$ref` 递归比对到（`schemaReachability` 的真实可达集），要么在
//     `unpairedSchemas` 里（理由 + 源码证人）。Go 侧形状可以直接反射具名类型，也可以从
//     handler 源码里读（`goWire`：`writeJSON` 的 map 字面量 / 函数内的匿名 struct）。
//   - **宽投影不许静默退出比对**（F3）：`additionalProperties` 的 schema 必须登记在
//     `wideSchemas`（理由 + 证人），未登记 = 红；证人符号消失 = 红。
//   - **注册语句里有真实中间件**（F4）：adminSession / consoleKey / nodeToken 三种方案靠
//     匿名探针分不出来（都回 401），所以判据落在 mux 注册语句的中间件链上（+ adminMW 的
//     定义、handler 里的鉴权调用），每条都带真实符号证人。
//   - **裸挂载的 method 有源码判据**（F5）：每个 op 都带一条 method 分派 token
//     （`case http.MethodPatch:` 这类），token 的方法必须与表里的 method 一致。
//   - **responses 与参数也在门禁内**（F2）：query 参数双向对账、requestBody → 400、
//     CAS DELETE → 400+409、security → 401、裸挂载 handler 会 405 就必须声明 405。
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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/playbook"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/source"
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
	// repoRoot 是仓库根（本包的测试工作目录是 internal/server）。
	// 源码证人（unpairedSchemas / wideSchemas / securityRegistration）都按
	// `相对仓库根的路径:标识符` 登记，所以这里要能从包目录回到仓库根。
	repoRoot = "../.."
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

// 实现现状快照（**改动必须同步**）。
//
// 这几个数字不是“随手挑的下限”，而是当前实现的真实数量：门槛低于实际值 = 削掉几十条
// 仍然绿（F10）。所以它们是 ratchet：删一条 pair / 删一条裸挂载 / 删一条真实路由 /
// 删一个 schema，都必须同时改这里的数字，并在 PR 里写清为什么。
const (
	// parityPairsFloor 是 parityPairs() 的对数（含 auth/ingress/agent 的全部 DTO 与协议帧）。
	parityPairsFloor = 191
	// bareMountFloor 是 bareMountSpec() 的条目数。
	bareMountFloor = 26
	// muxPatternCount 是静态收集到的真实 mux pattern 总数（含裸挂载与兜底）。
	muxPatternCount = 112
	// bareOpFloor 是裸挂载表登记的 spec operation 总数。
	bareOpFloor = 39
	// specSchemaFloor 是五份 spec 的 components.schemas 总数。
	specSchemaFloor = 228
)

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
	// 只判 len>0 等于没判：收集器漏收一半仍然绿。这里是现状快照（见常量注释）。
	if len(patterns) != muxPatternCount {
		t.Fatalf("静态收集到 %d 条 mux pattern，快照是 %d —— 新增/删除路由必须同步 spec 与这个数字"+
			"（若是收集器漏收，说明 AST 求值退化了）", len(patterns), muxPatternCount)
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

	fatalIfProblems(t, "裸挂载表", bareMountProblems(patterns, ops, bareMountSpec()))

	// 削表守卫：下面的数字是现状快照（见常量注释），删条目必须同步。
	table := bareMountSpec()
	if len(table) < bareMountFloor {
		t.Fatalf("裸挂载表只剩 %d 条（快照下限 %d）—— 被削过了", len(table), bareMountFloor)
	}
	opsN := 0
	for _, entry := range table {
		opsN += len(entry.Ops)
	}
	if opsN < bareOpFloor {
		t.Fatalf("裸挂载表只登记了 %d 条 operation（快照下限 %d）—— 被削过了", opsN, bareOpFloor)
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
	fatalIfProblems(t, "security 实测", securityProblems(ops, observed, securityProbeExceptions))

	// spec 声明的方案必须有真实中间件接住（匿名探针分辨不出 adminSession / consoleKey /
	// nodeToken —— 它们都回 401，所以必须直接从**注册语句**上找证人）。
	patterns, patternProblems := collectMuxPatterns(serverPkgDir)
	fatalIfProblems(t, "静态收集 mux pattern", patternProblems)
	fatalIfProblems(t, "security 注册语句", securityRegistrationProblems(patterns, ops))
}

// TestOpenAPIParityDTOFields 守 spec ↔ Go 结构体的**双向**字段集：Go 有而 schema 无 = 红，
// schema 有而 Go 无 = 红（例外必须写理由 + 可过期）。
func TestOpenAPIParityDTOFields(t *testing.T) {
	docs, problems := loadSpecDocs()
	fatalIfProblems(t, "载入五份 spec", problems)

	pairs := parityPairs()
	// 削表守卫：这些数字是**实现现状的快照**，改动必须同步（删 pair / 删裸挂载 = 悄悄
	// 放宽门禁，而那正是本门禁要防的事）。
	if len(pairs) < parityPairsFloor {
		t.Fatalf("parityPairs 只剩 %d 对（快照下限 %d：覆盖全部 kind + 请求体/回执 + auth/ingress/agent 帧；删行必须同时改这个数字并写理由）",
			len(pairs), parityPairsFloor)
	}
	fatalIfProblems(t, "DTO 双向字段", dtoProblems(docs, pairs, wideSchemas))

	// 宽投影白名单自身：schema 仍存在、仍是宽的、证人真实（F3）。
	fatalIfProblems(t, "宽投影白名单", wideSchemaProblems(docs, wideSchemas))

	// 完整性门禁（F1）：五份 spec 的**每一个** component schema 都必须有归属。
	coverageProblems, stats := schemaCoverageProblems(docs, pairs)
	fatalIfProblems(t, "schema 完整性", coverageProblems)
	t.Logf("schema 覆盖：total=%d paired=%d reachable=%d unpaired=%d（pairs=%d，宽投影豁免=%d）",
		stats.total, stats.paired, stats.reachable, stats.unpaired, len(pairs), len(wideSchemas))
	if stats.total < specSchemaFloor {
		t.Fatalf("五份 spec 的 component schema 只剩 %d 个（快照下限 %d）—— 少 schema 要么是 spec 被削了，要么是解析坏了",
			stats.total, specSchemaFloor)
	}
	if stats.paired < parityPairsFloor {
		t.Fatalf("只有 %d 个 schema 进了字段级 pair（快照下限 %d）—— auth/ingress/agent 的 DTO 又掉出门禁了",
			stats.paired, parityPairsFloor)
	}
	if stats.unpaired > 16 {
		t.Fatalf("unpairedSchemas 涨到 %d 条（快照上限 16）—— 每一对都是「没门禁」的口子，加条必须写理由并改这个数字", stats.unpaired)
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

	// 方向唯一的判据串：这两个方向此前用「Go 的」「不存在」这种子串，会**同时**命中
	// 反方向的错误消息（F8）——变异是否真的被抓到就无从判断了。
	const (
		dirGoOnly     = "在 Go 的 wire 形状里存在，但" // Go 有、schema 无
		dirSchemaOnly = "声明了这个字段，但 Go 的"      // schema 有、Go 无
		dirBrokenRef  = "的 $ref 解析失败"
	)

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

	t.Run("同一端点换占位符写法（{ns} vs {namespace}）→ 归属键必须按 canonical path 判重", func(t *testing.T) {
		docs := mustReloadDocs(t)
		addPathOp(t, docs, "resource.yaml", "/v1/namespaces/{ns}/capabilities/{name}", "get", map[string]any{
			"operationId": "getCapabilityPlaceholderVariant",
			"security":    []any{map[string]any{"session": []any{}}},
		})
		ops, _ := collectSpecOps(docs)
		problems := specIntegrityProblems(ops)
		requireProblem(t, "占位符变体重复", problems, "归属重复")
	})

	t.Run("把一条 session operation 的 security 改成 [] → security 实测必须红", func(t *testing.T) {
		observed := observeSecurityStatuses(t, baseOps)
		docs := mustReloadDocs(t)
		setSecurity(t, docs, "resource.yaml", "/v1/namespaces/{namespace}/capabilities", "get", []any{})
		ops, _ := collectSpecOps(docs)
		if len(ops) == 0 {
			t.Fatalf("变异「security 改成匿名」：变异后解析出 0 条 operation")
		}
		problems := securityProblems(ops, observed, securityProbeExceptions)
		requireProblem(t, "security 改成匿名", problems, "spec 声明匿名但实测 401")
	})

	t.Run("security 例外已过期（Want 与 spec 基线同向）→ 必须红", func(t *testing.T) {
		// 给一条**已有安全方案**的 operation 登记 Want=401 的例外：基线的要求就是 401，
		// 这条例外没有任何必要 —— 正是「例外变成永久后门」的样子。
		observed := observeSecurityStatuses(t, baseOps)
		exceptions := map[string]securityProbeException{}
		for k, v := range securityProbeExceptions {
			exceptions[k] = v
		}
		exceptions["GET /v1/namespaces/{namespace}/capabilities"] = securityProbeException{
			Want: http.StatusUnauthorized, Reason: "变异用：多余的例外",
		}
		problems := securityProblems(baseOps, observed, exceptions)
		requireProblem(t, "security 例外已过期", problems, "security 例外表已过期")
	})

	t.Run("把 POST /v1/auth/login 的 security 改成 {session} → 它的例外必须立刻过期", func(t *testing.T) {
		// 这条例外存在的唯一理由就是「匿名端点自己回 401」。spec 一旦声明 session，
		// 401 变成基线行为，例外必须删掉（F7 要求的过期检测）。
		observed := observeSecurityStatuses(t, baseOps)
		docs := mustReloadDocs(t)
		setSecurity(t, docs, "auth.yaml", "/v1/auth/login", "post", []any{map[string]any{"session": []any{}}})
		ops, _ := collectSpecOps(docs)
		problems := securityProblems(ops, observed, securityProbeExceptions)
		requireProblem(t, "login 例外过期", problems, "security 例外表已过期")
	})

	t.Run("从 schema 删一个字段（Go 有、schema 无）→ DTO 必须红（只命中该方向）", func(t *testing.T) {
		docs := mustReloadDocs(t)
		deleteSchemaProperty(t, docs, "resource.yaml", "CapabilityNodeState", "state")
		problems := dtoProblems(docs, parityPairs(), wideSchemas)
		requireOnlyProblem(t, "schema 删字段", problems, dirGoOnly, []string{dirSchemaOnly})
	})

	t.Run("给 schema 加一个 Go 没有的字段 → DTO 必须红（只命中该方向）", func(t *testing.T) {
		docs := mustReloadDocs(t)
		addSchemaProperty(t, docs, "resource.yaml", "CapabilityNodeState", "ghostField")
		problems := dtoProblems(docs, parityPairs(), wideSchemas)
		requireOnlyProblem(t, "schema 加幽灵字段", problems, dirSchemaOnly, []string{dirGoOnly})
	})

	t.Run("断掉一条跨文件 $ref → 加载/解析必须报错（只命中 ref 方向）", func(t *testing.T) {
		docs := mustReloadDocs(t)
		setRef(t, docs, "events.yaml", "components.schemas.TraceView.properties.tasks.items",
			"./resource.yaml#/components/schemas/NoSuchSchema")
		problems := refIntegrityProblems(docs)
		requireOnlyProblem(t, "断跨文件 $ref", problems, dirBrokenRef, []string{dirGoOnly, dirSchemaOnly})
	})

	t.Run("宽投影未登记（从 wideSchemas 删一条）→ DTO 必须红", func(t *testing.T) {
		docs := mustReloadDocs(t)
		wide := map[string]schemaWitness{}
		for k, v := range wideSchemas {
			wide[k] = v
		}
		delete(wide, "resource.yaml:RenderedCapability")
		problems := dtoProblems(docs, parityPairs(), wide)
		requireProblem(t, "宽投影未登记", problems, "宽投影")
	})

	t.Run("宽投影表过期（schema 不再是宽的）→ wideSchemaProblems 必须红", func(t *testing.T) {
		docs := mustReloadDocs(t)
		schema := schemaForMutation(t, docs, "resource.yaml", "PlaybookUpgradedEntry")
		delete(schema, "additionalProperties")
		schema["properties"] = map[string]any{"name": map[string]any{"type": "string"}}
		problems := wideSchemaProblems(docs, wideSchemas)
		requireProblem(t, "宽投影表过期", problems, "wideSchemas 已过期")
	})

	t.Run("宽投影的证人符号不存在 → 必须红（证人失效即过期）", func(t *testing.T) {
		docs := mustReloadDocs(t)
		wide := map[string]schemaWitness{}
		for k, v := range wideSchemas {
			wide[k] = v
		}
		wide["resource.yaml:RenderedCapability"] = schemaWitness{
			Reason:  "变异用：证人指向一个不存在的符号",
			Witness: "internal/playbook/playbook.go:NoSuchSymbolAnyMore",
		}
		problems := wideSchemaProblems(docs, wide)
		requireProblem(t, "证人失效", problems, "证人")
	})

	t.Run("裸挂载表反向多写（表里多一条 spec 没有的 op）→ 必须红", func(t *testing.T) {
		table := bareMountSpec()
		appended := false
		for i := range table {
			if table[i].Pattern == "/v1/devices" {
				table[i].Ops = append(table[i].Ops, bareOp{
					Method: "PUT", Path: "/v1/devices/{ghost}",
					Dispatch: "case http.MethodPut:", Literal: `strings.TrimPrefix(r.URL.Path, "/v1/devices")`,
				})
				appended = true
			}
		}
		if !appended {
			t.Fatalf("变异失败：裸挂载表里没有 /v1/devices")
		}
		problems := bareMountProblems(patterns, baseOps, table)
		requireProblem(t, "裸挂载反向多写", problems, "在五份 spec 里找不到")
	})

	t.Run("裸挂载的 method 分派 token 与表里的 method 不一致 → 必须红", func(t *testing.T) {
		table := bareMountSpec()
		mutated := false
		for i := range table {
			for j := range table[i].Ops {
				op := &table[i].Ops[j]
				if op.Method == "PATCH" && op.Path == "/v1/devices/{id}" {
					// 真实回归就是这一种：`case http.MethodPatch:` 被改成别的 method。
					op.Dispatch = `case http.MethodPut:`
					mutated = true
				}
			}
		}
		if !mutated {
			t.Fatalf("变异失败：裸挂载表里没有 PATCH /v1/devices/{id}")
		}
		problems := bareMountProblems(patterns, baseOps, table)
		requireProblem(t, "method token 不一致", problems, "Dispatch 指向 PUT")
		requireProblem(t, "method token 不一致（handler 里也没有）", problems, "方法分派判据")
	})

	t.Run("无法静态求值的 pattern 表达式 → 收集器必须报 problem（不许静默跳过）", func(t *testing.T) {
		dir := t.TempDir()
		src := `package server

import "net/http"

func registerGhost(mux *http.ServeMux) {
	mux.Handle(dynamicPattern(), http.NotFoundHandler())
}

func dynamicPattern() string { return "/v1/ghost" }
`
		if err := os.WriteFile(filepath.Join(dir, "ghost.go"), []byte(src), 0o600); err != nil {
			t.Fatalf("变异失败：写不了临时源码：%v", err)
		}
		built, problems := collectMuxPatterns(dir)
		requireProblem(t, "AST 求值失败", problems, "无法静态求值的 pattern 表达式")
		if len(built) != 0 {
			t.Fatalf("变异失败：不可求值的表达式居然被收进了 %v", built)
		}
	})

	t.Run("包级 FuncLit 里的路由注册 → 必须被收进表（F6）", func(t *testing.T) {
		dir := t.TempDir()
		src := `package server

import "net/http"

var registerPackageLevel = func(mux *http.ServeMux) {
	mux.Handle("GET /v1/package-level", http.NotFoundHandler())
}
`
		if err := os.WriteFile(filepath.Join(dir, "ghost.go"), []byte(src), 0o600); err != nil {
			t.Fatalf("变异失败：写不了临时源码：%v", err)
		}
		built, problems := collectMuxPatterns(dir)
		fatalIfProblems(t, "包级闭包收集", problems)
		if len(built) != 1 || built[0].Raw != "GET /v1/package-level" {
			t.Fatalf("包级 FuncLit 里的注册被静默漏收了：%v", built)
		}
	})

	t.Run("F2: spec 把 tailLines 写成 tail（实现只读 tailLines）→ query 正向必须红", func(t *testing.T) {
		docs := mustReloadDocs(t)
		ops, _ := collectSpecOps(docs)
		details, _ := collectSpecDetails(docs)
		literals, _ := stringLiterals(serverPkgDir)
		usage, _ := queryParamUsage(serverPkgDir)
		const logsOp = "GET /v1/namespaces/{namespace}/tasks/{taskId}/logs"
		detail, ok := details[logsOp]
		if !ok {
			t.Fatalf("变异失败：spec 里没有 %s", logsOp)
		}
		detail.QueryParams = replaceString(detail.QueryParams, "tailLines", "tail")
		details[logsOp] = detail
		problems := responsesAndParamsProblems(ops, details, literals, usage, patterns, statusExceptionTables{
			RequestBodyNo400: requestBodyNo400Exceptions,
			SecurityNo401:    securityNo401Exceptions,
			BareMount405:     bareMount405Exceptions,
		})
		requireProblem(t, "spec 参数名漂移（tail）", problems, "query 参数正向")
	})

	t.Run("F2: 实现读了 spec 没声明的 query 参数 → query 反向必须红", func(t *testing.T) {
		docs := mustReloadDocs(t)
		ops, _ := collectSpecOps(docs)
		details, _ := collectSpecDetails(docs)
		literals, _ := stringLiterals(serverPkgDir)
		usage, _ := queryParamUsage(serverPkgDir)
		usage["ghostParam"] = []string{"handlers.go:1"}
		problems := responsesAndParamsProblems(ops, details, literals, usage, patterns, statusExceptionTables{
			RequestBodyNo400: requestBodyNo400Exceptions,
			SecurityNo401:    securityNo401Exceptions,
			BareMount405:     bareMount405Exceptions,
		})
		requireProblem(t, "实现读了未声明参数", problems, "query 参数反向")
	})

	t.Run("F2: 405 例外表清空 → 裸挂载下的 405 缺口必须暴露（证明例外在承重）", func(t *testing.T) {
		docs := mustReloadDocs(t)
		ops, _ := collectSpecOps(docs)
		details, _ := collectSpecDetails(docs)
		literals, _ := stringLiterals(serverPkgDir)
		usage, _ := queryParamUsage(serverPkgDir)
		problems := responsesAndParamsProblems(ops, details, literals, usage, patterns, statusExceptionTables{
			RequestBodyNo400: requestBodyNo400Exceptions,
			SecurityNo401:    securityNo401Exceptions,
			BareMount405:     map[string]statusException{},
		})
		requireProblem(t, "405 例外清空", problems, "没声明 405")
	})

	t.Run("F2: requestBody 的 400 例外清空 → POST /v1/auth/login 必须暴露", func(t *testing.T) {
		docs := mustReloadDocs(t)
		ops, _ := collectSpecOps(docs)
		details, _ := collectSpecDetails(docs)
		literals, _ := stringLiterals(serverPkgDir)
		usage, _ := queryParamUsage(serverPkgDir)
		problems := responsesAndParamsProblems(ops, details, literals, usage, patterns, statusExceptionTables{
			RequestBodyNo400: map[string]statusException{},
			SecurityNo401:    securityNo401Exceptions,
			BareMount405:     bareMount405Exceptions,
		})
		requireProblem(t, "requestBody 400 例外清空", problems, "却没声明 400")
	})

	t.Run("F4: admin 路由的中间件掉成纯会话 → 注册语句必须红", func(t *testing.T) {
		mutated := make([]routePattern, 0, len(patterns))
		hits := 0
		for _, p := range patterns {
			if p.Raw == "/v1/admin/" {
				// 真实回归：adminMW（会话 + 超管两层）被换成单层会话。
				p.Reg = `mux.Handle("/v1/admin/", noStore(sessMW(adminHandler)))`
				hits++
			}
			mutated = append(mutated, p)
		}
		if hits == 0 {
			t.Fatalf("变异失败：静态 pattern 里没有 /v1/admin/")
		}
		problems := securityRegistrationProblems(mutated, baseOps)
		requireProblem(t, "admin 中间件掉层", problems, "没有 adminMW")
	})

	t.Run("F4: nodeToken 路由换成会话 handler → 注册语句必须红", func(t *testing.T) {
		mutated := make([]routePattern, 0, len(patterns))
		hits := 0
		for _, p := range patterns {
			if p.Raw == "GET /v1/node/binary" {
				p.Reg = `mux.Handle("GET /v1/node/binary", noStore(sessMW(someSessionHandler)))`
				hits++
			}
			mutated = append(mutated, p)
		}
		if hits == 0 {
			t.Fatalf("变异失败：静态 pattern 里没有 GET /v1/node/binary")
		}
		problems := securityRegistrationProblems(mutated, baseOps)
		requireProblem(t, "nodeToken 换成会话", problems, "没有 \"agentBinaryHandler\"")
	})

	t.Run("F6: 包级闭包用非 mux 参数名注册 → 必须响亮失败", func(t *testing.T) {
		dir := t.TempDir()
		src := `package server

import "net/http"

var registerWithOtherName = func(m *http.ServeMux) {
	m.Handle("GET /v1/other-name", http.NotFoundHandler())
}
`
		if err := os.WriteFile(filepath.Join(dir, "ghost.go"), []byte(src), 0o600); err != nil {
			t.Fatalf("变异失败：写不了临时源码：%v", err)
		}
		_, problems := collectMuxPatterns(dir)
		requireProblem(t, "包级闭包参数名不是 mux", problems, "不在静态门禁的收集范围内")
	})

	t.Run("F6: range 表里注册两个字段 → 两个都要收（不许只记最后一个）", func(t *testing.T) {
		dir := t.TempDir()
		src := `package server

import "net/http"

func registerTwo(mux *http.ServeMux) {
	routes := []struct {
		pattern   string
		patternV2 string
	}{
		{pattern: "GET /v1/two/a", patternV2: "GET /v1/two/b"},
	}
	for _, rt := range routes {
		mux.Handle(rt.pattern, http.NotFoundHandler())
		mux.Handle(rt.patternV2, http.NotFoundHandler())
	}
}
`
		if err := os.WriteFile(filepath.Join(dir, "ghost.go"), []byte(src), 0o600); err != nil {
			t.Fatalf("变异失败：写不了临时源码：%v", err)
		}
		built, problems := collectMuxPatterns(dir)
		fatalIfProblems(t, "range 多字段收集", problems)
		got := []string{}
		for _, p := range built {
			got = append(got, p.Raw)
		}
		sort.Strings(got)
		want := []string{"GET /v1/two/a", "GET /v1/two/b"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("range 循环体里的第二个字段被静默漏收了：got=%v want=%v", got, want)
		}
	})

	t.Run("F1: 从 parityPairs 删掉一个 schema → 完整性门禁必须红", func(t *testing.T) {
		docs := mustReloadDocs(t)
		var pairs []parityPair
		removed := false
		for _, pair := range parityPairs() {
			if pair.File == "auth.yaml" && pair.Schema == "AdminIndex" {
				removed = true
				continue
			}
			pairs = append(pairs, pair)
		}
		if !removed {
			t.Fatalf("变异失败：parityPairs 里没有 auth.yaml:AdminIndex")
		}
		problems, _ := schemaCoverageProblems(docs, pairs)
		requireProblem(t, "schema 掉出门禁", problems, "auth.yaml:AdminIndex 既不在 parityPairs")
	})

	t.Run("F4: adminMW 的定义少一层 → 必须红", func(t *testing.T) {
		problems := adminMiddlewareProblems("adminMW := sessMW(adminH)")
		requireProblem(t, "admin 少一层（无 AdminMiddleware）", problems, "AdminMiddleware")
		problems = adminMiddlewareProblems("adminMW := authSvc.AdminMiddleware(h)")
		requireProblem(t, "admin 少一层（无会话）", problems, "sessMW")
	})

	t.Run("FakeGreenGuard 自身：改名 → 必须红", func(t *testing.T) {
		src, err := os.ReadFile(selfFileName)
		if err != nil {
			t.Fatalf("读不了本文件：%v", err)
		}
		mutated := strings.Replace(string(src),
			"func TestOpenAPIParitySecurity(t *testing.T) {",
			"func TestParitySecurityRenamed(t *testing.T) {", 1)
		if mutated == string(src) {
			t.Fatalf("变异失败：本文件里没有 TestOpenAPIParitySecurity")
		}
		problems := fakeGreenGuardProblems([]byte(mutated))
		requireProblem(t, "门禁改名", problems, "本文件里没有 TestOpenAPIParitySecurity")
	})

	t.Run("FakeGreenGuard 自身：空函数体 → 必须红", func(t *testing.T) {
		src, err := os.ReadFile(selfFileName)
		if err != nil {
			t.Fatalf("读不了本文件：%v", err)
		}
		// 追加一个同名空壳：解析器允许重复声明，byName 取到的是空体那个。
		mutated := string(src) + "\nfunc TestOpenAPIParitySecurity(t *testing.T) {}\n"
		problems := fakeGreenGuardProblems([]byte(mutated))
		requireProblem(t, "空函数体", problems, "函数体是空的")
	})
}

func TestOpenAPIParityFakeGreenGuard(t *testing.T) {
	src, err := os.ReadFile(selfFileName)
	if err != nil {
		t.Fatalf("读不了本文件 %s：%v（本门禁必须能从 internal/server 目录跑）", selfFileName, err)
	}
	fatalIfProblems(t, "假绿守卫自扫", fakeGreenGuardProblems(src))
}

// fakeGreenGuardProblems 是假绿/反 skip 守卫的**纯函数**实现（把语料换成参数，
// 这样守卫自身也能被变异测试打红 —— 见 TestOpenAPIParityMutations 里的「空函数体」）。
func fakeGreenGuardProblems(src []byte) []string {
	var problems []string
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, selfFileName, src, 0)
	if err != nil {
		return []string{fmt.Sprintf("解析本文件失败：%v", err)}
	}
	byName := map[string]*ast.FuncDecl{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "TestOpenAPIParity") {
			continue
		}
		byName[fn.Name.Name] = fn
	}
	// 门禁一个都不能少：改名 = -run 匹配不到 = exit 0 假绿。
	want := []string{
		"TestOpenAPIParitySpecLoadAndRefs",
		"TestOpenAPIParityMuxToSpec",
		"TestOpenAPIParitySpecToMux",
		"TestOpenAPIParityBareMounts",
		"TestOpenAPIParitySecurity",
		"TestOpenAPIParityDTOFields",
		"TestOpenAPIParityResponsesAndParams",
		"TestOpenAPIParityMutations",
		"TestOpenAPIParityFakeGreenGuard",
	}
	for _, name := range want {
		fn, ok := byName[name]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"本文件里没有 %s（改名会让 `go test -run TestOpenAPIParity` 以 exit 0 假绿）", name))
			continue
		}
		// 空函数体 = 只占名字、什么都不检查。这是比改名更隐蔽的假绿：
		// `-run` 仍然匹配、测试仍然 PASS，但门禁已经不存在了。
		if fn.Body == nil || len(fn.Body.List) == 0 {
			problems = append(problems, fmt.Sprintf(
				"%s 的函数体是空的 —— 占着名字不检查东西（假绿）", name))
			continue
		}
		body := string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
		if !strings.Contains(body, "fatalIfProblems(") && !strings.Contains(body, "t.Fatal") &&
			!strings.Contains(body, "t.Error") && !strings.Contains(body, "Problems(") {
			problems = append(problems, fmt.Sprintf(
				"%s 的函数体里没有任何断言/门禁调用（fatalIfProblems / t.Fatal / t.Error / ...Problems(）", name))
		}
	}
	// 反 skip：本文件不许出现 skip 调用（写成拼接是为了不自匹配）。
	for _, banned := range []string{"t." + "Skip", "Skip" + "Now()", "Skip" + "f("} {
		if strings.Contains(string(src), banned) {
			problems = append(problems, fmt.Sprintf("本文件出现 %q —— 门禁里不允许 skip/放宽断言", banned))
		}
	}
	// 削表守卫（与 TestOpenAPIParityDTOFields 里那条互为双保险）。
	if len(parityPairs()) < parityPairsFloor {
		problems = append(problems, fmt.Sprintf("parityPairs 只剩 %d 对（快照下限 %d）", len(parityPairs()), parityPairsFloor))
	}
	if len(bareMountSpec()) < bareMountFloor {
		problems = append(problems, fmt.Sprintf("bareMountSpec 只剩 %d 条（快照下限 %d）", len(bareMountSpec()), bareMountFloor))
	}
	return problems
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
//
// 判重**必须用 canonical path**：`{ns}` 与 `{namespace}` 是两个不同的原始 path，却是同一个
// 归属（`internal/server` 里只有一份路由）。按原始 path 判重会漏掉
// 「同一端点两种占位符写法」这种重复——spec 里两条 operation、mux 上一条路由。
func specIntegrityProblems(ops []specOp) []string {
	var problems []string
	byKey := map[string]specOp{}
	byID := map[string]specOp{}
	for _, op := range ops {
		key := op.Method + " " + op.Canonical
		if prev, dup := byKey[key]; dup {
			problems = append(problems, fmt.Sprintf("归属重复：%s 同时出现在 %s %s（operationId=%s）与 %s %s（operationId=%s）—— 归属键 = method + canonical path（%s）",
				key, prev.File, prev.Path, prev.OperationID, op.File, op.Path, op.OperationID, op.Canonical))
		}
		byKey[key] = op
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
	// Reg 是**注册语句原文**（`mux.Handle(...)` 那一次调用的完整源码，多行已拼接、
	// 括号平衡由 AST 位置保证）。它带着整条中间件链，是 security 方案对账的唯一证据面。
	Reg string
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
	// 第二遍 a：每个具名函数/方法体。
	for _, f := range files {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			c.scanFunc(fd)
		}
	}
	// 第二遍 b：**包级 FuncLit**（`var registerRoutes = func(mux *http.ServeMux) {…}`）。
	// 只遍历 FuncDecl 会让这种注册既不进表也不报错（F6 的静默漏收）；这里按函数体扫描。
	// 参数名不是 mux 的包级闭包由第三遍兜底报错（X.Handle 的 X 不是 mux 即 problem）。
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, nm := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.FuncLit)
					if !ok || lit.Body == nil {
						continue
					}
					if !takesServeMux(lit.Type) {
						continue
					}
					c.scanBody("包级闭包 "+nm.Name, lit.Body)
				}
			}
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

// takesServeMux 报告一个函数类型是否接收 *http.ServeMux（包级闭包 = 注册器的判据）。
func takesServeMux(ft *ast.FuncType) bool {
	if ft == nil || ft.Params == nil {
		return false
	}
	for _, param := range ft.Params.List {
		star, ok := param.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		id, ok := sel.X.(*ast.Ident)
		if ok && id.Name == "http" && sel.Sel.Name == "ServeMux" {
			return true
		}
	}
	return false
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
	if fd.Body == nil {
		return
	}
	c.scanBody(fd.Name.Name, fd.Body)
}

// scanBody 扫一段函数体（具名函数/方法，以及**包级 FuncLit**）里的全部路由注册形态：
//
//	① 直写：            mux.Handle("GET " + base, h)
//	② 包装闭包：        route := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, …) }
//	                    route("GET "+nodesPath, a.list)
//	③ 表驱动 + range：  routes := []struct{pattern string; h http.HandlerFunc}{…}
//	                    for _, rt := range routes { mux.Handle(rt.pattern, …) }
//
// ② ③ 是真实代码的写法（resource_node/resource_task/retained_reads/capability/playbook）；
// 不展开它们就等于漏掉那五个资源域的全部路由 —— 所以这里必须理解它们，而不是“跳过”。
//
// 每个 pattern 都附带**注册语句原文**（`Reg`）：这是 F4 判「spec 的 security 方案有没有
// 真实中间件接住」的唯一证据面（匿名探针分不出 adminSession / consoleKey / nodeToken，
// 它们都回 401）。
func (c *patternCollector) scanBody(where string, body *ast.BlockStmt) {
	scopes := []map[string]ast.Expr{c.localConsts(body)}
	wrappers, wrapperParams := c.wrappers(body)
	rangeVars := c.rangeVarNames(body)
	rangeTables := c.rangeHandles(where, body)

	// ① 直写形态（跳过 ② ③ 的“参数位”）。
	ast.Inspect(body, func(n ast.Node) bool {
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
		c.record(arg, scopes, c.text(call))
		return true
	})
	// ② 包装闭包的调用点：注册语句取**闭包里那次 mux.Handle 的调用**（中间件链在那里）。
	for _, name := range sortedKeys(wrappers) {
		w := wrappers[name]
		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			fid, ok := call.Fun.(*ast.Ident)
			if !ok || fid.Name != name {
				return true
			}
			c.record(call.Args[0], scopes, w.regCall)
			return true
		})
	}
	// ③ 表驱动：把复合字面量里每个元素的 pattern 字段求值。
	for _, table := range rangeTables {
		c.recordRangeTable(where, body, table, scopes)
	}
}

// wrapper 是一个「把 pattern 当第一个参数转手给 mux.Handle」的闭包。
type wrapper struct {
	// param 是闭包的第一个参数名（调用点上第一个实参就是它）。
	param string
	// regCall 是闭包里那次 mux.Handle 调用的源码原文（注册语句 = 中间件证据）。
	regCall string
}

// rangeHandle 描述一处「range 一张局部的 struct 切片表」。
//
// 每个被注册的字段各占一条：一个 range 体里注册**两个**字段（pattern / patternV2）时，
// 只记最后一个等于静默漏掉另一批路由（F6）。这里按字段逐条登记。
type rangeHandle struct {
	sliceVar string
	field    string
	regCall  string
}

// localConsts 收函数体内的 `const x = "..."`（`const base = "/v1/namespaces/{ns}/…"` 就是它）。
func (c *patternCollector) localConsts(body *ast.BlockStmt) map[string]ast.Expr {
	out := map[string]ast.Expr{}
	ast.Inspect(body, func(n ast.Node) bool {
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
func (c *patternCollector) wrappers(body *ast.BlockStmt) (map[string]wrapper, map[string]bool) {
	names := map[string]wrapper{}
	params := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
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
		regCall := ""
		ast.Inspect(lit.Body, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok || !isMuxHandleCall(call) {
				return true
			}
			if id, ok := call.Args[0].(*ast.Ident); ok && id.Name == pname {
				uses = true
				regCall = c.text(call)
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
		names[name.Name] = wrapper{param: pname, regCall: regCall}
		params[pname] = true
		return true
	})
	return names, params
}

// rangeVarNames 收 `for _, x := range …` 的 value 变量名（用来在直写扫描里跳过 `x.pattern`）。
func (c *patternCollector) rangeVarNames(body *ast.BlockStmt) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
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
//
// 循环体里注册了**多个**字段时逐个登记（只记最后一个 = 静默漏路由）。
func (c *patternCollector) rangeHandles(where string, body *ast.BlockStmt) []rangeHandle {
	var out []rangeHandle
	ast.Inspect(body, func(n ast.Node) bool {
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
		fields := map[string]string{}
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
			fields[sel.Sel.Name] = c.text(call)
			return true
		})
		if len(fields) == 0 {
			return true
		}
		id, ok := rs.X.(*ast.Ident)
		if !ok {
			c.problems = append(c.problems, fmt.Sprintf("%s：range 的源不是局部变量，无法静态展开表驱动 pattern", c.pos(rs)))
			return true
		}
		for _, field := range sortedKeys(fields) {
			out = append(out, rangeHandle{sliceVar: id.Name, field: field, regCall: fields[field]})
		}
		return true
	})
	return out
}

// recordRangeTable 把 `routes := []struct{pattern string; …}{…}` 的每个元素展开。
func (c *patternCollector) recordRangeTable(where string, body *ast.BlockStmt, table rangeHandle, scopes []map[string]ast.Expr) {
	var lit *ast.CompositeLit
	ast.Inspect(body, func(n ast.Node) bool {
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
			"无法静态求值的 pattern 表达式：range 表 %q 在 %s 里找不到复合字面量定义",
			table.sliceVar, where))
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
		c.record(pick, scopes, table.regCall)
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
func (c *patternCollector) record(e ast.Expr, scopes []map[string]ast.Expr, reg string) {
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
	c.out = append(c.out, routePattern{Method: method, Path: path, Raw: raw, Pos: c.pos(e), Reg: reg})
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
	// Dispatch 是**方法分派**的源码 token（`case http.MethodPatch:` /
	// `if r.Method != http.MethodPost` / `r.Method == http.MethodPost`）。
	//
	// 为什么不能只靠 Literal（F5）：`case http.MethodPatch:` 改成 `case http.MethodPut:`
	// 时，路径判据（`h.patch(w, r, rest)`）一个字都没变，门禁仍然全绿 —— 而真实行为已经
	// 变成「PATCH 404 / PUT 打到 patch」。所以每个 op 都要有一条**方法级**判据，并且
	// token 里的方法必须与 Method 一致（不一致即红）。
	Dispatch string
	// NoDispatch 只在「该 handler 根本没有方法分派」时出现（写清理由）；
	// 此时 Dispatch 必须为空，且 HandlerFile 的源码里**不得**有 405 分支（否则就是在
	// 说「方法不对」却没有按方法分派，表必须补 Dispatch）。
	NoDispatch string
	// Literal 是**路径/子路由**判据：必须能在 HandlerFile 的源码里找到的字面量（含引号）。
	// 找不到 = 表和 handler 已经脱节（比如 handler 的 switch 改了分支名），直接红。
	Literal string
}

// dispatchMethod 从一条 method 分派 token 里取方法名（`http.MethodPatch` / `"PATCH"`）。
func dispatchMethod(token string) (string, bool) {
	if m := regexp.MustCompile(`http\.Method([A-Za-z]+)`).FindStringSubmatch(token); m != nil {
		return strings.ToUpper(m[1]), true
	}
	if m := regexp.MustCompile(`"(GET|PUT|POST|DELETE|PATCH|HEAD|OPTIONS)"`).FindStringSubmatch(token); m != nil {
		return m[1], true
	}
	return "", false
}

// methodNotAllowedBranch 报告 handler 源码里是否有「方法不对 → 405」的分支。
func methodNotAllowedBranch(src string) bool {
	return strings.Contains(src, "writeErr(w, 405") ||
		strings.Contains(src, "StatusMethodNotAllowed") ||
		strings.Contains(src, "methodNotAllowed")
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
				{"POST", "/v1/auth/register/options", `case http.MethodPost:`, "", `"register/options"`},
				{"POST", "/v1/auth/register", `case http.MethodPost:`, "", `"register"`},
				{"POST", "/v1/auth/login/options", `case http.MethodPost:`, "", `"login/options"`},
				{"POST", "/v1/auth/login", `case http.MethodPost:`, "", `"login"`},
				{"POST", "/v1/auth/logout", `case http.MethodPost:`, "", `"logout"`},
				{"POST", "/v1/auth/recovery/login", `case http.MethodPost:`, "", `"recovery/login"`},
				// password/login 有显式路由（更具体，见下一条），这里只是 authHandler 的兜底；
				// 两条都登记，反向检查按真实匹配优先级判（不会重复报）。
				{"POST", "/v1/auth/password/login", `case http.MethodPost:`, "", `"password/login"`},
			},
		},
		{
			Pattern:     "/v1/auth/password/login",
			HandlerFile: "auth_handlers.go",
			Ops: []bareOp{
				{"POST", "/v1/auth/password/login", `case http.MethodPost:`, "", `func (h *authHandler) passwordLogin(w http.ResponseWriter`},
			},
		},
		{
			Pattern:     "/v1/auth/password/register",
			HandlerFile: "auth_handlers.go",
			Ops: []bareOp{
				{"POST", "/v1/auth/password/register", `case http.MethodPost:`, "", `func (h *authHandler) passwordRegister(w http.ResponseWriter`},
			},
		},
		{
			Pattern:     "/v1/auth/password",
			HandlerFile: "auth_handlers.go",
			Ops: []bareOp{
				{"GET", "/v1/auth/password", `case http.MethodGet:`, "", `"password"`},
				{"POST", "/v1/auth/password", `case http.MethodPost:`, "", `"password"`},
				{"DELETE", "/v1/auth/password", `case http.MethodDelete:`, "", `"password"`},
			},
		},
		{
			Pattern:     "/v1/auth/recovery/regenerate",
			HandlerFile: "auth_handlers.go",
			Ops: []bareOp{
				{"POST", "/v1/auth/recovery/regenerate", `case http.MethodPost:`, "", `func (h *authHandler) recoveryRegenerate(w http.ResponseWriter`},
			},
		},
		{
			Pattern:     "/v1/auth/recovery/status",
			HandlerFile: "auth_handlers.go",
			Ops: []bareOp{
				{"GET", "/v1/auth/recovery/status", `case http.MethodGet:`, "", `func (h *authHandler) recoveryStatus(w http.ResponseWriter`},
			},
		},
		{
			Pattern:     "/v1/auth/me",
			HandlerFile: "auth_handlers.go",
			Ops: []bareOp{
				{"GET", "/v1/auth/me", `case http.MethodGet:`, "", `case "me":`},
			},
		},
		{
			Pattern:     "/v1/auth/sessions",
			HandlerFile: "auth_handlers.go",
			Ops: []bareOp{
				{"GET", "/v1/auth/sessions", `case http.MethodGet:`, "", `case "sessions":`},
			},
		},
		{
			Pattern:     "/v1/auth/passkeys",
			HandlerFile: "auth_handlers.go",
			Ops: []bareOp{
				{"GET", "/v1/auth/passkeys", `case http.MethodGet:`, "", `func (h *authHandler) listPasskeys(w http.ResponseWriter`},
			},
		},
		{
			Pattern:     "/v1/auth/passkeys/",
			HandlerFile: "auth_handlers.go",
			Ops: []bareOp{
				{"POST", "/v1/auth/passkeys/register/options", `sub == "register/options" && r.Method == http.MethodPost`, "", `sub == "register/options"`},
				{"POST", "/v1/auth/passkeys/register", `sub == "register" && r.Method == http.MethodPost`, "", `sub == "register"`},
				{"PATCH", "/v1/auth/passkeys/{id}", `if r.Method != http.MethodPatch`, "", `func (h *authHandler) renamePasskey(w http.ResponseWriter`},
				{"DELETE", "/v1/auth/passkeys/{id}", `case r.Method == http.MethodDelete:`, "", `func (h *authHandler) deletePasskey(w http.ResponseWriter`},
			},
		},
		{
			Pattern:     "/v1/vapid-public-key",
			HandlerFile: "mux.go",
			Ops: []bareOp{
				{"GET", "/v1/vapid-public-key", "", "内联闭包没有 method 分派：任何方法都回同一个公钥 JSON", `"/v1/vapid-public-key"`},
			},
		},
		{
			Pattern:     "/v1/devices",
			HandlerFile: "handlers.go",
			Ops: []bareOp{
				{"GET", "/v1/devices", `case http.MethodGet:`, "", `strings.TrimPrefix(r.URL.Path, "/v1/devices")`},
				{"POST", "/v1/devices", `case http.MethodPost:`, "", `strings.TrimPrefix(r.URL.Path, "/v1/devices")`},
			},
		},
		{
			Pattern:     "/v1/devices/",
			HandlerFile: "handlers.go",
			Ops: []bareOp{
				{"PATCH", "/v1/devices/{id}", `case http.MethodPatch:`, "", `h.patch(w, r, rest)`},
				{"DELETE", "/v1/devices/{id}", `case http.MethodDelete:`, "", `h.remove(w, r, rest)`},
			},
		},
		{
			Pattern:     "/v1/keys",
			HandlerFile: "handlers.go",
			Ops: []bareOp{
				{"GET", "/v1/keys", `case http.MethodGet:`, "", `strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/keys"), "/")`},
				{"POST", "/v1/keys", `case http.MethodPost:`, "", `strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/keys"), "/")`},
			},
		},
		{
			Pattern:     "/v1/keys/",
			HandlerFile: "handlers.go",
			Ops: []bareOp{
				{"POST", "/v1/keys/{id}/revoke", `case http.MethodPost:`, "", `"/revoke"`},
			},
		},
		{
			Pattern:     "/v1/admin",
			HandlerFile: "admin_handlers.go",
			Ops: []bareOp{
				{"GET", "/v1/admin", "", `case "": 这条分支没有方法守卫（任何方法都回端点清单）`, `strings.TrimPrefix(r.URL.Path, "/v1/admin")`},
			},
		},
		{
			Pattern:     "/v1/admin/",
			HandlerFile: "admin_handlers.go",
			Ops: []bareOp{
				{"GET", "/v1/admin/stats", `case http.MethodGet:`, "", `case "stats":`},
				{"GET", "/v1/admin/users", `case http.MethodGet:`, "", `case "users":`},
				{"GET", "/v1/admin/users/{id}", `case http.MethodGet:`, "", `strings.HasPrefix(rest, "users/")`},
				{"PATCH", "/v1/admin/users/{id}", `case http.MethodPatch:`, "", `strings.HasPrefix(rest, "users/")`},
				{"PATCH", "/v1/admin/users/{id}/role", `case http.MethodPatch:`, "", `action == "role"`},
				{"POST", "/v1/admin/users/{id}/disable", `r.Method == http.MethodPost`, "", `action == "disable"`},
				{"POST", "/v1/admin/users/{id}/enable", `r.Method == http.MethodPost`, "", `action == "enable"`},
				{"GET", "/v1/admin/messages", `case http.MethodGet:`, "", `case "messages":`},
				{"GET", "/v1/admin/sessions", `case http.MethodGet:`, "", `case "sessions":`},
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
func bareMountProblems(patterns []routePattern, ops []specOp, table []bareMountEntry) []string {
	var problems []string
	astBare := map[string]bool{}
	for _, p := range patterns {
		if p.Method == "" {
			astBare[p.Path] = true
		}
	}
	byPath := map[string]bareMountEntry{}
	for _, entry := range table {
		if _, dup := byPath[entry.Pattern]; dup {
			problems = append(problems, fmt.Sprintf("裸挂载表重复登记 %q", entry.Pattern))
		}
		byPath[entry.Pattern] = entry
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
			// 方法级判据（F5）：token 的方法必须与表里的 Method 一致，且真的出现在 handler 里。
			if bo.Dispatch == "" {
				if bo.NoDispatch == "" {
					problems = append(problems, fmt.Sprintf(
						"裸挂载表 %q 的 %s %s 没有方法分派判据（Dispatch 必填；确实没有分派就写 NoDispatch 理由）",
						entry.Pattern, bo.Method, bo.Path))
				}
			} else {
				if bo.NoDispatch != "" {
					problems = append(problems, fmt.Sprintf(
						"裸挂载表 %q 的 %s %s 同时写了 Dispatch 与 NoDispatch，二者只能有其一",
						entry.Pattern, bo.Method, bo.Path))
				}
				gotMethod, ok := dispatchMethod(bo.Dispatch)
				if !ok {
					problems = append(problems, fmt.Sprintf(
						"裸挂载表 %q 的 %s %s 的 Dispatch %q 里看不出方法（必须含 http.MethodX 或 \"METHOD\"）",
						entry.Pattern, bo.Method, bo.Path, bo.Dispatch))
				} else if gotMethod != bo.Method {
					problems = append(problems, fmt.Sprintf(
						"裸挂载表 %q 的 %s %s 的 Dispatch 指向 %s —— 方法 token 与表里的 method 不一致",
						entry.Pattern, bo.Method, bo.Path, gotMethod))
				}
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
					"裸挂载表与其 handler 代码脱节：%s %s 的路径判据 %q 在 %s 里找不到",
					bo.Method, bo.Path, bo.Literal, entry.HandlerFile))
			}
			if bo.Dispatch != "" && !strings.Contains(handlerSrc, bo.Dispatch) {
				problems = append(problems, fmt.Sprintf(
					"裸挂载表与其 handler 代码脱节：%s %s 的方法分派判据 %q 在 %s 里找不到"+
						"（dispatch 改成分支别的 method 了 → 这里必须红）",
					bo.Method, bo.Path, bo.Dispatch, entry.HandlerFile))
			}
			// 声称「没有方法分派」只有在**这条裸 pattern 只承载一个 method** 时才成立：
			// 两种 method 落在同一个无分派的 handler 上，等于两种方法互相顶（必须补 Dispatch）。
			if bo.Dispatch == "" && bo.NoDispatch != "" && len(entry.Ops) > 1 {
				problems = append(problems, fmt.Sprintf(
					"裸挂载表 %q 声称「没有方法分派」，但它承载了 %d 条 operation —— "+
						"无分派 handler 下不可能有多个 method，必须补 Dispatch",
					entry.Pattern, len(entry.Ops)))
			}
		}
	}
	// 反向 ①：mux 里的裸 pattern 必须都在表里。
	for _, p := range patterns {
		if p.Method != "" {
			continue
		}
		if _, ok := byPath[p.Path]; !ok {
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
		entry, ok := byPath[pat]
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
func securityProblems(ops []specOp, observed map[string]int, exceptions map[string]securityProbeException) []string {
	var problems []string
	for _, op := range ops {
		code, ok := observed[op.Key()]
		if !ok {
			problems = append(problems, fmt.Sprintf("security 实测缺数据：%s %s 没有发过请求", op.Method, op.Path))
			continue
		}
		if ex, has := exceptions[op.Key()]; has {
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
	for key := range exceptions {
		method, path, ok := strings.Cut(key, " ")
		if !ok {
			problems = append(problems, fmt.Sprintf("security 例外表的键 %q 不是 \"METHOD /path\" 形态", key))
			continue
		}
		op, found := findOp(ops, method, path)
		if !found {
			problems = append(problems, fmt.Sprintf(
				"security 例外表已过期：spec 里已经没有 %s（%s）", key, exceptions[key].Reason))
			continue
		}
		// 过期检测的第二层（只看「operation 还在不在」是不够的）：按 spec 的 security
		// 重新推一遍**基线期望**。例外的 Want 一旦与基线期望同向，说明这条例外已经没必要
		// ——spec 的 security 变了（匿名 → 有方案 / 反之），或者 handler 的鉴权变了。
		// 例：把 POST /v1/auth/login 的 security 从 [] 改成 {session}，它的 401 就不再是
		// 「handler 自己回的业务 401」，而是中间件拦的，例外必须删掉。
		if exceptionIsRedundant(op, exceptions[key].Want) {
			problems = append(problems, fmt.Sprintf(
				"security 例外表已过期：%s 的基线期望 = %s（spec security=%v），与例外的 Want=%d 同向 —— 这条例外已经没有存在理由（%s）",
				key, baselineDescription(op), op.Security, exceptions[key].Want,
				exceptions[key].Reason))
		}
	}
	return problems
}

// exceptionIsRedundant 报告「按 spec 的 security 推出的基线期望」与例外的 Want 是否同向。
//
//	有安全方案 → 基线是「必须 401」；Want == 401 时例外是多余的。
//	匿名       → 基线是「不得 401」；Want != 401 时例外是多余的。
func exceptionIsRedundant(op specOp, want int) bool {
	wantIs401 := want == http.StatusUnauthorized
	return wantIs401 == !op.Anonymous()
}

// baselineDescription 给失败信息用：说清「spec 现在要求什么」。
func baselineDescription(op specOp) string {
	if op.Anonymous() {
		return "匿名（不得 401）"
	}
	return "需要鉴权 " + fmt.Sprint(op.Security) + "（必须 401）"
}

// ============================================================================
// 5.5 security 方案的**真实中间件**证据（F4）
// ============================================================================

// nodeTokenRegistration 是 nodeToken 三个端点各自的**鉴权入口**。
//
// 这三个端点都挂在资源域之外、都由 handler 自己验 node token，所以「注册语句里有这个
// handler」+「这个 handler 的源码里真的按 token 查节点」两条都要成立。
var nodeTokenRegistration = map[string]struct {
	// Handler 是注册语句里必须出现的标识（handler 构造点）。
	Handler string
	// AuthFile 是真正做 node token 校验的文件（相对仓库根）。
	AuthFile string
	// Witness 是 AuthFile 里必须存在的符号（token → 节点的那一步）。
	Witness string
}{
	"GET /v1/node/stream": {
		Handler:  "hub",
		AuthFile: "internal/ws/hub.go",
		Witness:  "internal/ws/hub.go:GetNodeByTokenHash",
	},
	"GET /v1/node/binary": {
		Handler:  "agentBinaryHandler",
		AuthFile: "internal/server/agent_binary.go",
		Witness:  "internal/server/agent_binary.go:GetNodeByTokenHash",
	},
	"PUT /v1/node/tasks/{taskId}/artifacts/{sha256}": {
		Handler:  "newArtifactUploadHandler",
		AuthFile: "internal/server/artifact_upload.go",
		Witness:  "internal/server/artifact_upload.go:GetNodeByTokenHash",
	},
}

// consoleKeyRegistration 是 consoleKey 两个端点各自的**鉴权入口**。
//
// consoleKey 没有中间件：鉴权在 handler 里（Bearer → ValidateKey → scopes）。所以判据是
// 「注册语句指向这个 handler」+「这个 handler 的源码里真的有 key 校验调用」。
var consoleKeyRegistration = map[string]struct {
	// Handler 是注册语句里必须出现的标识。
	Handler string
	// AuthFile 是真正做 API Key 校验的文件（相对仓库根）。
	AuthFile string
	// Tokens 是 AuthFile 里必须出现的校验调用/标识。
	Tokens []string
}{
	"GET /v1/keys/self": {
		Handler:  "keysSelf",
		AuthFile: "internal/server/cli_auth_handlers.go",
		Tokens:   []string{"ValidateKey", "BearerToken"},
	},
	"POST /v1/namespaces/{namespace}/events": {
		Handler:  "eventH",
		AuthFile: "internal/api/events.go",
		Tokens:   []string{"Authenticate", "ScopeNotifySend"},
	},
}

// securitySchemeWitness 是「方案 → 真实符号」的固定证人（符号改名即红）。
var securitySchemeWitness = map[string][]string{
	"session":      {"internal/auth/session.go:Middleware"},
	"adminSession": {"internal/auth/admin.go:AdminMiddleware", "internal/auth/session.go:Middleware"},
	"consoleKey":   {"internal/auth/apikey.go:ValidateKey", "internal/api/events.go:EventHandler"},
	"nodeToken":    {"internal/authn/authn.go:BearerToken"},
}

// securityRegistrationProblems 对账「spec 的 security 方案」与「注册语句里出现的鉴权标识」。
//
// 为什么匿名探针不够（F4）：adminSession 掉成 session、consoleKey 换成会话、nodeToken 换成
// 随便什么 handler，**匿名请求都仍然 401**，securityProblems 全绿。方案之间的区别只写在
// 注册语句的中间件链里，所以判据必须落在那里。
func securityRegistrationProblems(patterns []routePattern, ops []specOp) []string {
	var problems []string
	byRaw := map[string]routePattern{}
	for _, p := range patterns {
		byRaw[p.Raw] = p
	}
	mux, muxProblems := newProbeMux(patterns)
	problems = append(problems, muxProblems...)

	// 裸挂载上的 op：注册语句取**所属裸 pattern** 的那条（中间件挂在裸 pattern 上）。
	regFor := func(op specOp) (routePattern, bool) {
		pat := probeMatch(mux, op.Method, samplePath(op.Path))
		if pat == "" {
			return routePattern{}, false
		}
		p, ok := byRaw[pat]
		return p, ok
	}

	// admin 的两层中间件写在 `adminMW := sessMW(authSvc.AdminMiddleware(...))` 的定义里，
	// 注册语句只引用 adminMW —— 所以两层要分别证明。
	adminDef, adminErr := assignmentText("internal/server/mux.go", "adminMW")
	if adminErr != nil {
		problems = append(problems, fmt.Sprintf("admin 中间件证人失效：%v", adminErr))
	}

	// 固定证人：符号改名/删除即红。
	for _, scheme := range sortedKeys(securitySchemeWitness) {
		for _, witness := range securitySchemeWitness[scheme] {
			problems = append(problems, witnessProblems("security 方案 "+scheme, witness)...)
		}
	}

	usedNodeToken := map[string]bool{}
	usedConsoleKey := map[string]bool{}
	for _, op := range ops {
		if len(op.Security) == 0 {
			continue
		}
		reg, ok := regFor(op)
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"security 注册语句：%s %s 没有任何真实 pattern 接住它（无法取注册语句）", op.Method, op.Path))
			continue
		}
		for _, scheme := range op.Security {
			switch scheme {
			case "session":
				if !strings.Contains(reg.Reg, "sessMW") && !strings.Contains(reg.Reg, ".session(") {
					problems = append(problems, fmt.Sprintf(
						"security 注册语句：%s %s 声明 session，但注册语句里没有会话中间件（sessMW / .session(）：%s（%s）",
						op.Method, op.Path, firstLine(reg.Reg), reg.Pos))
				}
			case "adminSession":
				if !strings.Contains(reg.Reg, "adminMW") {
					problems = append(problems, fmt.Sprintf(
						"security 注册语句：%s %s 声明 adminSession，但注册语句里没有 adminMW（admin 中间件）：%s（%s）",
						op.Method, op.Path, firstLine(reg.Reg), reg.Pos))
				}
				if adminErr == nil {
					problems = append(problems, adminMiddlewareProblems(adminDef)...)
				}
			case "consoleKey":
				spec, ok := consoleKeyRegistration[op.Key()]
				if !ok {
					problems = append(problems, fmt.Sprintf(
						"security 注册语句：%s %s 声明 consoleKey，但 consoleKeyRegistration 里没有它的鉴权入口登记", op.Method, op.Path))
					continue
				}
				usedConsoleKey[op.Key()] = true
				if !strings.Contains(reg.Reg, spec.Handler) {
					problems = append(problems, fmt.Sprintf(
						"security 注册语句：%s %s 声明 consoleKey，但注册语句没指向做 API Key 校验的 handler（%s）：%s（%s）",
						op.Method, op.Path, spec.Handler, firstLine(reg.Reg), reg.Pos))
				}
				for _, token := range spec.Tokens {
					if ok, err := fileMustContain(spec.AuthFile, token); err != nil || !ok {
						problems = append(problems, fmt.Sprintf(
							"security 注册语句：%s %s 的 consoleKey 鉴权调用不见了 —— %s 里没有 %q（err=%v）",
							op.Method, op.Path, spec.AuthFile, token, err))
					}
				}
			case "nodeToken":
				spec, ok := nodeTokenRegistration[op.Key()]
				if !ok {
					problems = append(problems, fmt.Sprintf(
						"security 注册语句：%s %s 声明 nodeToken，但 nodeTokenRegistration 里没有它的鉴权入口登记", op.Method, op.Path))
					continue
				}
				usedNodeToken[op.Key()] = true
				if !strings.Contains(reg.Reg, spec.Handler) {
					problems = append(problems, fmt.Sprintf(
						"security 注册语句：%s %s 声明 nodeToken，但注册语句里没有 %q（节点鉴权入口）：%s（%s）",
						op.Method, op.Path, spec.Handler, firstLine(reg.Reg), reg.Pos))
				}
				if ok, err := fileMustContain(spec.AuthFile, "GetNodeByTokenHash"); err != nil || !ok {
					problems = append(problems, fmt.Sprintf(
						"security 注册语句：%s %s 的节点鉴权在 %s 里找不到 GetNodeByTokenHash（token → 节点那一步没了；err=%v）",
						op.Method, op.Path, spec.AuthFile, err))
				}
				problems = append(problems, witnessProblems("nodeToken 的 "+op.Key(), spec.Witness)...)
			default:
				problems = append(problems, fmt.Sprintf(
					"security 注册语句：%s %s 用了未知方案 %q（securitySchemeWitness 里没有对应证人）",
					op.Method, op.Path, scheme))
			}
		}
	}
	// 登记表过期即红：两条登记表里的每个 key 都必须是**真实存在**的对应方案 op。
	for _, key := range sortedKeys(nodeTokenRegistration) {
		if usedNodeToken[key] {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"nodeTokenRegistration 已过期：spec 里没有声明 nodeToken 的 %s（或方案换了），请从表里删掉", key))
	}
	for _, key := range sortedKeys(consoleKeyRegistration) {
		if usedConsoleKey[key] {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"consoleKeyRegistration 已过期：spec 里没有声明 consoleKey 的 %s（或方案换了），请从表里删掉", key))
	}
	return problems
}

// adminMiddlewareProblems 检查 adminMW 的定义是否**同时**套了会话（sessMW）与超管
// （AdminMiddleware）两层。少任何一层 = 「adminSession」名不副实（F4）：
// 只套 sessMW = 任何登录用户都能进后台；只套 AdminMiddleware = 没有会话可判。
func adminMiddlewareProblems(def string) []string {
	var problems []string
	for _, token := range []string{"sessMW", "AdminMiddleware"} {
		if !strings.Contains(def, token) {
			problems = append(problems, fmt.Sprintf(
				"security 注册语句：admin 中间件少了一层 —— adminMW 的定义里没有 %q（定义原文：%s）",
				token, firstLine(def)))
		}
	}
	return problems
}

// firstLine 把多行注册语句压成一行（失败信息里只留开头的可读片段）。
func firstLine(s string) string {
	line := strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	if len(line) > 160 {
		line = line[:160] + "…"
	}
	return line
}

// ============================================================================
// 6. DTO 双向字段
// ============================================================================

// timeType 是 time.Time：wire 上是 RFC3339 字符串（MarshalJSON），反射里却是 struct。
// 不特判的话，协议帧里每个 `*At` / `expiresAt` 都会误报「openapi 说 string、Go 是 object」。
var timeType = reflect.TypeOf(time.Time{})

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
	// Wire 是「Go 侧形状来自产品源码」的另一种取法（匿名 struct / writeJSON 的 map 字面量）。
	// 与 Sample 二选一：Sample 用于具名类型，Wire 用于 handler 里就地定义的形状。
	Wire *goWire
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

		// ---- auth.yaml：身份 / 设备 / Key / 会话 / 管理后台（F1 的核心缺口）----
		// 具名类型的直接 reflect；handler 里就地定义的形状走 Wire（读产品源码，不抄一份）。
		{GoLabel: "store.User", File: "auth.yaml", Schema: "User", Sample: store.User{}},
		{GoLabel: "store.Device", File: "auth.yaml", Schema: "Device", Sample: store.Device{}},
		{GoLabel: "deviceUpsertReq", File: "auth.yaml", Schema: "DeviceUpsertRequest", Sample: deviceUpsertReq{}},
		{GoLabel: "devicePatchReq", File: "auth.yaml", Schema: "DevicePatchRequest", Sample: devicePatchReq{}},
		{GoLabel: "devicesHandler.upsert", File: "auth.yaml", Schema: "DeviceResult",
			Wire: &goWire{File: "handlers.go", Func: "devicesHandler.upsert", Kind: "response", Select: -1}},
		{GoLabel: "devicesHandler.list", File: "auth.yaml", Schema: "DeviceList",
			Wire: &goWire{File: "handlers.go", Func: "devicesHandler.list", Kind: "response", Select: -1}},
		{GoLabel: "keyPublic", File: "auth.yaml", Schema: "APIKey", Sample: keyPublic{}},
		{GoLabel: "keyCreateReq", File: "auth.yaml", Schema: "APIKeyCreateRequest", Sample: keyCreateReq{}},
		{GoLabel: "keysHandler.create", File: "auth.yaml", Schema: "APIKeyCreateResult",
			Wire: &goWire{File: "handlers.go", Func: "keysHandler.create", Kind: "response", Select: -1}},
		{GoLabel: "keysHandler.list", File: "auth.yaml", Schema: "APIKeyList",
			Wire: &goWire{File: "handlers.go", Func: "keysHandler.list", Kind: "response", Select: -1}},
		{GoLabel: "cliAuthHandler.keysSelf", File: "auth.yaml", Schema: "KeySelf",
			Wire: &goWire{File: "cli_auth_handlers.go", Func: "cliAuthHandler.keysSelf", Kind: "response", Select: -1}},
		{GoLabel: "store.Session", File: "auth.yaml", Schema: "Session", Sample: store.Session{}},
		{GoLabel: "authHandler.sessions", File: "auth.yaml", Schema: "SessionList",
			Wire: &goWire{File: "auth_handlers.go", Func: "authHandler.sessions", Kind: "response", Select: -1}},
		{GoLabel: "passkeyOut", File: "auth.yaml", Schema: "Passkey", Sample: passkeyOut{}},
		{GoLabel: "authHandler.listPasskeys", File: "auth.yaml", Schema: "PasskeyList",
			Wire: &goWire{File: "auth_handlers.go", Func: "authHandler.listPasskeys", Kind: "response", Select: -1}},
		{GoLabel: "authHandler.renamePasskey", File: "auth.yaml", Schema: "PasskeyRenameRequest",
			Wire: &goWire{File: "auth_handlers.go", Func: "authHandler.renamePasskey", Kind: "request", Select: -1}},
		{GoLabel: "usernameReq", File: "auth.yaml", Schema: "UsernameRequest", Sample: usernameReq{}},
		{GoLabel: "authHandler.passwordLogin", File: "auth.yaml", Schema: "PasswordLoginRequest",
			Wire: &goWire{File: "auth_handlers.go", Func: "authHandler.passwordLogin", Kind: "request", Select: -1}},
		{GoLabel: "authHandler.passwordRegister", File: "auth.yaml", Schema: "PasswordRegisterRequest",
			Wire: &goWire{File: "auth_handlers.go", Func: "authHandler.passwordRegister", Kind: "request", Select: -1}},
		{GoLabel: "authHandler.passwordStatus", File: "auth.yaml", Schema: "PasswordStatus",
			Wire: &goWire{File: "auth_handlers.go", Func: "authHandler.passwordStatus", Kind: "response", Select: -1}},
		{GoLabel: "authHandler.passwordSet", File: "auth.yaml", Schema: "PasswordSetRequest",
			Wire: &goWire{File: "auth_handlers.go", Func: "authHandler.passwordSet", Kind: "request", Select: -1}},
		{GoLabel: "authHandler.passwordSet", File: "auth.yaml", Schema: "PasswordSetResult",
			Wire: &goWire{File: "auth_handlers.go", Func: "authHandler.passwordSet", Kind: "response", Select: -1}},
		{GoLabel: "authHandler.passwordRemove", File: "auth.yaml", Schema: "PasswordRemoveRequest",
			Wire: &goWire{File: "auth_handlers.go", Func: "authHandler.passwordRemove", Kind: "request", Select: -1}},
		{GoLabel: "authHandler.passwordRemove", File: "auth.yaml", Schema: "PasswordRemoveResult",
			Wire: &goWire{File: "auth_handlers.go", Func: "authHandler.passwordRemove", Kind: "response", Select: -1}},
		{GoLabel: "authHandler.recoveryLogin", File: "auth.yaml", Schema: "RecoveryLoginRequest",
			Wire: &goWire{File: "auth_handlers.go", Func: "authHandler.recoveryLogin", Kind: "request", Select: -1}},
		{GoLabel: "authHandler.recoveryRegenerate", File: "auth.yaml", Schema: "RecoveryCodesResult",
			Wire: &goWire{File: "auth_handlers.go", Func: "authHandler.recoveryRegenerate", Kind: "response", Select: -1}},
		{GoLabel: "authHandler.recoveryStatus", File: "auth.yaml", Schema: "RecoveryStatus",
			Wire: &goWire{File: "auth_handlers.go", Func: "authHandler.recoveryStatus", Kind: "response", Select: -1}},
		{GoLabel: "authHandler.logout", File: "auth.yaml", Schema: "OkResult",
			Wire: &goWire{File: "auth_handlers.go", Func: "authHandler.logout", Kind: "response", Select: -1}},
		{GoLabel: "NewApp(vapid)", File: "auth.yaml", Schema: "VAPIDPublicKey",
			Wire: &goWire{File: "mux.go", Func: "NewApp", Kind: "response", Select: -1}},
		{GoLabel: "adminUserOut", File: "auth.yaml", Schema: "AdminUser", Sample: adminUserOut{}},
		{GoLabel: "adminHandler.patchUser", File: "auth.yaml", Schema: "AdminUserPatchRequest",
			Wire: &goWire{File: "admin_handlers.go", Func: "adminHandler.patchUser", Kind: "request", Select: -1}},
		{GoLabel: "adminHandler.changeRole", File: "auth.yaml", Schema: "AdminUserRoleRequest",
			Wire: &goWire{File: "admin_handlers.go", Func: "adminHandler.changeRole", Kind: "request", Select: -1}},
		{GoLabel: "adminHandler.listUsers", File: "auth.yaml", Schema: "AdminUserList",
			Wire: &goWire{File: "admin_handlers.go", Func: "adminHandler.listUsers", Kind: "response", Select: -1}},
		{GoLabel: "store.AdminSessionRow", File: "auth.yaml", Schema: "AdminSessionRow", Sample: store.AdminSessionRow{}},
		{GoLabel: "adminHandler.listSessions", File: "auth.yaml", Schema: "AdminSessionList",
			Wire: &goWire{File: "admin_handlers.go", Func: "adminHandler.listSessions", Kind: "response", Select: -1}},
		{GoLabel: "store.AdminGlobalEvent", File: "auth.yaml", Schema: "AdminEvent", Sample: store.AdminGlobalEvent{}},
		{GoLabel: "adminHandler.listMessages", File: "auth.yaml", Schema: "AdminEventList",
			Wire: &goWire{File: "admin_handlers.go", Func: "adminHandler.listMessages", Kind: "response", Select: -1}},
		{GoLabel: "store.SystemStats", File: "auth.yaml", Schema: "AdminSystemStats", Sample: store.SystemStats{}},
		{GoLabel: "store.DayCount(admin)", File: "auth.yaml", Schema: "AdminDayCount", Sample: store.DayCount{}},
		{GoLabel: "store.UserMetric", File: "auth.yaml", Schema: "AdminUserMetric", Sample: store.UserMetric{}},
		{GoLabel: "adminHandler.ServeHTTP", File: "auth.yaml", Schema: "AdminIndex",
			Wire: &goWire{File: "admin_handlers.go", Func: "adminHandler.ServeHTTP", Kind: "response", Select: -1}},
		{GoLabel: "createSessionReq", File: "auth.yaml", Schema: "CLIAuthSessionCreateRequest", Sample: createSessionReq{}},
		{GoLabel: "createSessionResp", File: "auth.yaml", Schema: "CLIAuthSessionCreated", Sample: createSessionResp{}},
		{GoLabel: "sessionView", File: "auth.yaml", Schema: "CLIAuthSessionView", Sample: sessionView{}},
		{GoLabel: "pollResp", File: "auth.yaml", Schema: "CLIAuthPollResult", Sample: pollResp{}},
		{GoLabel: "approveReq", File: "auth.yaml", Schema: "CLIAuthApproveRequest", Sample: approveReq{}},
		{GoLabel: "cliAuthHandler.approve", File: "auth.yaml", Schema: "CLIAuthStatusResult",
			Wire: &goWire{File: "cli_auth_handlers.go", Func: "cliAuthHandler.approve", Kind: "response", Select: -1}},
		{GoLabel: "enrollCreateReq", File: "auth.yaml", Schema: "PasskeyEnrollCreateRequest", Sample: enrollCreateReq{}},
		{GoLabel: "enrollCreateResp", File: "auth.yaml", Schema: "PasskeyEnrollCreated", Sample: enrollCreateResp{}},
		{GoLabel: "enrollAnonView", File: "auth.yaml", Schema: "PasskeyEnrollAnonView", Sample: enrollAnonView{}},
		{GoLabel: "enrollWatchResp", File: "auth.yaml", Schema: "PasskeyEnrollWatchView", Sample: enrollWatchResp{}},
		{GoLabel: "enrollPollResp", File: "auth.yaml", Schema: "PasskeyEnrollPollResult", Sample: enrollPollResp{}},
		{GoLabel: "enrollRequestReq", File: "auth.yaml", Schema: "PasskeyEnrollKnockRequest", Sample: enrollRequestReq{}},
		{GoLabel: "enrollRequestResp", File: "auth.yaml", Schema: "PasskeyEnrollKnockResult", Sample: enrollRequestResp{}},
		{GoLabel: "passkeyEnrollHandler.complete", File: "auth.yaml", Schema: "PasskeyEnrollCompleteResult",
			Wire: &goWire{File: "passkey_enroll_handlers.go", Func: "passkeyEnrollHandler.complete", Kind: "response", Select: -1}},
		{GoLabel: "passkeyEnrollHandler.cancel", File: "auth.yaml", Schema: "PasskeyEnrollStatusResult",
			Wire: &goWire{File: "passkey_enroll_handlers.go", Func: "passkeyEnrollHandler.cancel", Kind: "response", Select: -1}},

		// ---- ingress.yaml：投递体 / 内联预览（读 resource_source.go 的产品类型）----
		{GoLabel: "source.HookBody", File: "ingress.yaml", Schema: "HookBody", Sample: source.HookBody{}},
		{GoLabel: "srcPayloadBody", File: "ingress.yaml", Schema: "PayloadBody", Sample: srcPayloadBody{}},
		{GoLabel: "srcInlinePreviewBody", File: "ingress.yaml", Schema: "InlinePreviewBody", Sample: srcInlinePreviewBody{}},
		{GoLabel: "srcSpecView", File: "ingress.yaml", Schema: "InlineSourceSpec", Sample: srcSpecView{}},

		// ---- events.yaml 的保留读模型 ----
		{GoLabel: "taskTraceView", File: "events.yaml", Schema: "TraceView", Sample: taskTraceView{}},
		{GoLabel: "retainedReadAPI.orphans", File: "events.yaml", Schema: "Orphans",
			Wire: &goWire{File: "retained_reads.go", Func: "retainedReadAPI.orphans", Kind: "response", Select: -1}},

		// ---- resource.yaml 的写信封之外的部分 ----
		{GoLabel: "writeErr", File: "resource.yaml", Schema: "AuthError",
			Wire: &goWire{File: "handlers.go", Func: "writeErr", Kind: "response", Select: -1}},
		{GoLabel: "backpressureBody", File: "resource.yaml", Schema: "BackpressureError", Sample: backpressureBody{}},
		{GoLabel: "nodeAPI.upgradeNode", File: "resource.yaml", Schema: "NodeUpgradeResult",
			Wire: &goWire{File: "resource_node.go", Func: "nodeAPI.upgradeNode", Kind: "response", Select: 0}},
		{GoLabel: "playbookActionBody", File: "resource.yaml", Schema: "PlaybookActionRequest", Sample: playbookActionBody{}},
		{GoLabel: "taskActionReason", File: "resource.yaml", Schema: "TaskActionRequest",
			Wire: &goWire{File: "resource_task.go", Func: "taskActionReason", Kind: "request", Select: -1}},
		{GoLabel: "artifact.Template", File: "resource.yaml", Schema: "CapabilityTemplate", Sample: artifact.Template{}},
		{GoLabel: "artifact.Field", File: "resource.yaml", Schema: "CapabilityTemplateField", Sample: artifact.Field{}},
		{GoLabel: "capabilityAPI.templates", File: "resource.yaml", Schema: "CapabilityTemplateList",
			Wire: &goWire{File: "resource_capability.go", Func: "capabilityAPI.templates", Kind: "response", Select: -1}},
		{GoLabel: "retainedReadAPI.deployments", File: "resource.yaml", Schema: "DeploymentList",
			Wire: &goWire{File: "retained_reads.go", Func: "retainedReadAPI.deployments", Kind: "response", Select: -1}},
		{GoLabel: "capabilityAPI.edges", File: "resource.yaml", Schema: "EdgeList",
			Wire: &goWire{File: "resource_capability.go", Func: "capabilityAPI.edges", Kind: "response", Select: -1}},

		// ---- agent.yaml：37 个协议帧全部对上 internal/protocol（F1 第 4 条）----
		{GoLabel: "protocol.Limits", File: "agent.yaml", Schema: "Limits", Sample: protocol.Limits{}},
		{GoLabel: "protocol.NodeFacts", File: "agent.yaml", Schema: "NodeFacts", Sample: protocol.NodeFacts{}},
		{GoLabel: "protocol.HelloCommand", File: "agent.yaml", Schema: "HelloCommand", Sample: protocol.HelloCommand{}},
		{GoLabel: "protocol.HelloCapability", File: "agent.yaml", Schema: "HelloCapability", Sample: protocol.HelloCapability{}},
		{GoLabel: "protocol.HelloInstance", File: "agent.yaml", Schema: "HelloInstance", Sample: protocol.HelloInstance{}},
		{GoLabel: "protocol.HelloFrame", File: "agent.yaml", Schema: "HelloFrame", Sample: protocol.HelloFrame{}},
		{GoLabel: "protocol.WelcomeFrame", File: "agent.yaml", Schema: "WelcomeFrame", Sample: protocol.WelcomeFrame{}},
		{GoLabel: "protocol.ConfigChangedFrame", File: "agent.yaml", Schema: "ConfigChangedFrame", Sample: protocol.ConfigChangedFrame{}},
		{GoLabel: "protocol.ConfigGetFrame", File: "agent.yaml", Schema: "ConfigGetFrame", Sample: protocol.ConfigGetFrame{}},
		{GoLabel: "protocol.ConfigAppliedFrame", File: "agent.yaml", Schema: "ConfigAppliedFrame", Sample: protocol.ConfigAppliedFrame{}},
		{GoLabel: "protocol.CapabilityRef", File: "agent.yaml", Schema: "CapabilityRef", Sample: protocol.CapabilityRef{}},
		{GoLabel: "protocol.TaskDispatchFrame", File: "agent.yaml", Schema: "TaskDispatchFrame", Sample: protocol.TaskDispatchFrame{}},
		{GoLabel: "protocol.TaskAcceptFrame", File: "agent.yaml", Schema: "TaskAcceptFrame", Sample: protocol.TaskAcceptFrame{}},
		{GoLabel: "protocol.TaskRejectFrame", File: "agent.yaml", Schema: "TaskRejectFrame", Sample: protocol.TaskRejectFrame{}},
		{GoLabel: "protocol.LeaseGrantFrame", File: "agent.yaml", Schema: "LeaseGrantFrame", Sample: protocol.LeaseGrantFrame{}},
		{GoLabel: "protocol.LeaseRenewFrame", File: "agent.yaml", Schema: "LeaseRenewFrame", Sample: protocol.LeaseRenewFrame{}},
		{GoLabel: "protocol.LeaseAckFrame", File: "agent.yaml", Schema: "LeaseAckFrame", Sample: protocol.LeaseAckFrame{}},
		{GoLabel: "protocol.TaskResult", File: "agent.yaml", Schema: "TaskResultBody", Sample: protocol.TaskResult{}},
		{GoLabel: "protocol.TaskResultFrame", File: "agent.yaml", Schema: "TaskResultFrame", Sample: protocol.TaskResultFrame{}},
		{GoLabel: "protocol.ResultAckFrame", File: "agent.yaml", Schema: "ResultAckFrame", Sample: protocol.ResultAckFrame{}},
		{GoLabel: "protocol.TaskCancelFrame", File: "agent.yaml", Schema: "TaskCancelFrame", Sample: protocol.TaskCancelFrame{}},
		{GoLabel: "protocol.CancelAckFrame", File: "agent.yaml", Schema: "CancelAckFrame", Sample: protocol.CancelAckFrame{}},
		{GoLabel: "protocol.TaskEmitFrame", File: "agent.yaml", Schema: "TaskEmitFrame", Sample: protocol.TaskEmitFrame{}},
		{GoLabel: "protocol.TaskEmitAckFrame", File: "agent.yaml", Schema: "TaskEmitAckFrame", Sample: protocol.TaskEmitAckFrame{}},
		{GoLabel: "protocol.EmitPruned", File: "agent.yaml", Schema: "EmitPruned", Sample: protocol.EmitPruned{}},
		{GoLabel: "protocol.EmitResponse", File: "agent.yaml", Schema: "EmitResponse", Sample: protocol.EmitResponse{}},
		{GoLabel: "protocol.EmitError", File: "agent.yaml", Schema: "EmitError", Sample: protocol.EmitError{}},
		{GoLabel: "protocol.CapabilityDeployFrame", File: "agent.yaml", Schema: "CapabilityDeployFrame", Sample: protocol.CapabilityDeployFrame{}},
		{GoLabel: "protocol.CapabilityDeployedFrame", File: "agent.yaml", Schema: "CapabilityDeployedFrame", Sample: protocol.CapabilityDeployedFrame{}},
		{GoLabel: "protocol.UpgradeFrame", File: "agent.yaml", Schema: "UpgradeFrame", Sample: protocol.UpgradeFrame{}},
		{GoLabel: "protocol.UpgradeAckFrame", File: "agent.yaml", Schema: "UpgradeAckFrame", Sample: protocol.UpgradeAckFrame{}},
		{GoLabel: "protocol.LogsRequestFrame", File: "agent.yaml", Schema: "LogsRequestFrame", Sample: protocol.LogsRequestFrame{}},
		{GoLabel: "protocol.LogLine", File: "agent.yaml", Schema: "LogLine", Sample: protocol.LogLine{}},
		{GoLabel: "protocol.LogsResponseFrame", File: "agent.yaml", Schema: "LogsResponseFrame", Sample: protocol.LogsResponseFrame{}},
		{GoLabel: "protocol.ArtifactUploadResponse", File: "agent.yaml", Schema: "ArtifactUploadResponse", Sample: protocol.ArtifactUploadResponse{}},
		{GoLabel: "protocol.TaskArtifact", File: "agent.yaml", Schema: "TaskArtifact", Sample: protocol.TaskArtifact{}},
	}
}

// dtoProblems 是纯函数：逐对核对 Go 结构体与 schema（双向 + 递归 + 例外过期检测）。
//
// 例外表按 **Go 类型**登记（不是按「对」）：`statusDetails` 既出现在 `Status.details`
// 也会经 `NodeConfigBatchItem.error.details` 被递归到，例外必须对所有出现位置生效。
func dtoProblems(docs specDocs, pairs []parityPair, wide map[string]schemaWitness) []string {
	c := &dtoChecker{
		docs:       docs,
		wide:       wide,
		exclusions: map[reflect.Type]dtoExclusionSet{},
		wires:      map[string]reflect.Type{},
	}
	// 先把 Wire 型 pair 的 Go 形状从产品源码里解出来（失败即 problem，绝不静默跳过）。
	for _, pair := range pairs {
		if pair.Wire == nil {
			continue
		}
		if _, done := c.wires[pair.Wire.String()]; done {
			continue
		}
		t, problems := wireType(pair.Wire)
		c.problems = append(c.problems, problems...)
		if t != nil {
			c.wires[pair.Wire.String()] = t
		}
	}
	for _, pair := range pairs {
		var sample any = pair.Sample
		if sample == nil && pair.Wire != nil {
			sample = c.wires[pair.Wire.String()]
		}
		if sample == nil {
			continue
		}
		t := reflect.TypeOf(sample)
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
	docs specDocs
	// wide 是宽投影白名单（键 `文件:schema`）。未登记的宽 schema = problem。
	wide       map[string]schemaWitness
	exclusions map[reflect.Type]dtoExclusionSet
	// wires 是 `goWire.String()` → 从产品源码里解出来的 wire 类型。
	wires    map[string]reflect.Type
	problems []string
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

// comparePair 核对一对（Go 类型, schema）。Go 侧可以来自 `Sample`（具名类型）或 `Wire`
// （从产品源码里读出来的匿名 struct / map 字面量）。
func (c *dtoChecker) comparePair(pair parityPair) {
	var goType reflect.Type
	switch {
	case pair.Sample != nil:
		goType = reflect.TypeOf(pair.Sample)
	case pair.Wire != nil:
		goType = c.wires[pair.Wire.String()]
		if goType == nil {
			// wireType 已经把失败原因写进 problems 了（函数不存在 / 形状提取不出来）。
			return
		}
	default:
		c.addf("DTO 对照表里的 %s 既没有 Sample 也没有 Wire（无法取到 Go 类型）", pair.GoLabel)
		return
	}
	schema, file, ok := c.lookupSchema(pair.File, pair.Schema)
	if !ok {
		return
	}
	c.checkExclusions(pair, goType, schema)
	c.compareStruct(goType, schema, file, pair.GoLabel, "components.schemas."+pair.Schema,
		pair.File+":"+pair.Schema)
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
//
// refKey 是「这份 schema 的 component 名」（`文件:名字`）；只有 $ref 解析到这里时才有值。
// 宽投影的**白名单判定**靠它 —— 见 wideSchemas 的注释（F3）。
func (c *dtoChecker) compareStruct(goType reflect.Type, schema map[string]any, file, goLabel, schemaPath, refKey string) {
	for goType.Kind() == reflect.Ptr {
		goType = goType.Elem()
	}
	if goType.Kind() != reflect.Struct {
		c.addf("%s：Go 侧不是 struct（%s），无法做字段级比对", goLabel, goType.Kind())
		return
	}
	// 宽投影（additionalProperties 且没有 properties）：字段集无从核对。
	//
	// 但**不允许**静默 return（F3）：任何 schema 只要被改成 `additionalProperties: true`
	// 就能让这一对零检查。所以宽投影必须出现在 wideSchemas 白名单里（理由 + 证人），
	// 否则这里就是一条 problem。
	if wideSchema(schema) {
		key := refKey
		if key == "" {
			// 内联宽投影（没有 component 名）：按**路径**登记（路径变了 = 过期，必须同步）。
			key = file + ":path:" + schemaPath
		}
		if _, ok := c.wide[key]; !ok {
			c.addf("%s：%s 是宽投影（additionalProperties，没有 properties），字段集完全没被检查 —— "+
				"要么把它写成可比的 schema，要么在 wideSchemas 里登记理由与证人（键 %q）",
				goLabel, schemaPath, refKey)
		}
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
	// 记下「这份 schema 的 component 名」：宽投影白名单按名字判断（见 wideSchemas）。
	refKey := ""
	if ref, ok := schema["$ref"].(string); ok {
		if target, name, err := schemaRefTarget(file, ref); err == nil {
			refKey = target + ":" + name
		}
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
	if goType == timeType {
		return // wire 上是字符串（RFC3339）：类型由 time.Time 自己的 MarshalJSON 决定
	}
	switch goType.Kind() {
	case reflect.Struct:
		c.checkType(schema, "object", goPath)
		c.compareStruct(goType, schema, file, goPath, schemaPath, refKey)
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
// 6.5 Go 侧 wire 形状（从产品源码里读，不在测试里抄一份）
// ============================================================================

// anyType 是 `any` / `interface{}` / 未知外部类型的反射形态（compareValue 对它不做类型判定）。
var anyType = reflect.TypeOf((*any)(nil)).Elem()

// goWire 指向产品源码里的一段**可直接读**的 wire 形状。
//
// 为什么需要它：auth.yaml / ingress.yaml 里大量 DTO 在 handler 里是就地定义的匿名
// struct（`var req struct{…}`）或 `writeJSON(w, 200, map[string]any{…})`。在测试里抄一份
// 同名结构体等于把「Go 侧事实」复制成第二份真源——copy 一旦漂移，门禁就变成自欺。
// 所以这里**解析产品源码**取形状：handler 改了字段，门禁立刻看见。
type goWire struct {
	// File 是 internal/server 下的非测试文件名（如 "handlers.go"）。
	File string
	// Func 是具名函数/方法（方法写成 "接收者.方法"，如 "cliAuthHandler.keysSelf"）。
	// 包级闭包写在某个具名函数里时，用那个具名函数（如 "NewApp"）。
	Func string
	// Kind 是 "response"（writeJSON 的 map 字面量）或 "request"（函数内的匿名 struct）。
	Kind string
	// Select 是同类形状里的第几个（0 起；-1 = 最后一个）。
	Select int
}

func (w *goWire) String() string {
	return fmt.Sprintf("%s:%s:%s:%d", w.File, w.Func, w.Kind, w.Select)
}

func (w *goWire) label() string {
	return fmt.Sprintf("%s 里 %s 的 %s#%d", w.File, w.Func, w.Kind, w.Select)
}

// wireType 把 goWire 解析成 reflect.Type；任何一步失败都返回 problems（绝不静默跳过）。
func wireType(w *goWire) (reflect.Type, []string) {
	var problems []string
	if w.Kind != "response" && w.Kind != "request" {
		return nil, []string{fmt.Sprintf("goWire.%s 的 Kind=%q 不认识（只支持 response / request）", w.label(), w.Kind)}
	}
	rel := filepath.ToSlash(filepath.Join("internal/server", w.File))
	parsed := parseRepoGoFile(rel)
	if parsed.err != nil {
		return nil, []string{fmt.Sprintf("goWire.%s 的源码读不了：%v", w.label(), parsed.err)}
	}
	fd := findNamedFunc(parsed.file, w.Func)
	if fd == nil || fd.Body == nil {
		return nil, []string{fmt.Sprintf(
			"goWire.%s 失效：%s 里没有函数 %q（函数改名/删除 → 这条 pair 的 Go 侧依据不再存在）",
			w.label(), w.File, w.Func)}
	}
	var cands []ast.Expr
	switch w.Kind {
	case "response":
		cands = responseMapLiterals(fd.Body)
	case "request":
		cands = requestStructExprs(fd.Body)
	}
	if len(cands) == 0 {
		return nil, []string{fmt.Sprintf(
			"goWire.%s 失效：%s 的 %q 里找不到 %s 形状（handler 换成别的写法了，pair 必须同步）",
			w.label(), w.File, w.Func, w.Kind)}
	}
	pick := w.Select
	if pick < 0 {
		pick = len(cands) - 1
	}
	if pick < 0 || pick >= len(cands) {
		return nil, []string{fmt.Sprintf("goWire.%s 失效：只有 %d 个 %s 形状候选", w.label(), len(cands), w.Kind)}
	}
	t, err := typeOfWireExpr(cands[pick])
	if err != nil {
		return nil, []string{fmt.Sprintf("goWire.%s 无法转成反射类型：%v", w.label(), err)}
	}
	return t, problems
}

// findNamedFunc 按 "name" 或 "recv.name" 找具名函数/方法。
func findNamedFunc(f *ast.File, want string) *ast.FuncDecl {
	recv, name := "", want
	if head, tail, ok := strings.Cut(want, "."); ok {
		recv, name = head, tail
	}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != name {
			continue
		}
		gotRecv := ""
		if fd.Recv != nil && len(fd.Recv.List) == 1 {
			t := fd.Recv.List[0].Type
			for {
				star, ok := t.(*ast.StarExpr)
				if !ok {
					break
				}
				t = star.X
			}
			if id, ok := t.(*ast.Ident); ok {
				gotRecv = id.Name
			}
		}
		if gotRecv == recv {
			return fd
		}
	}
	return nil
}

// responseMapLiterals 取函数体里所有 `writeJSON(w, code, map[string]X{…})` 的 map 字面量。
func responseMapLiterals(body *ast.BlockStmt) []ast.Expr {
	var out []ast.Expr
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 3 {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != "writeJSON" {
			return true
		}
		lit, ok := call.Args[2].(*ast.CompositeLit)
		if !ok {
			return true
		}
		if _, ok := lit.Type.(*ast.MapType); !ok {
			return true
		}
		out = append(out, lit)
		return true
	})
	return out
}

// requestStructExprs 取函数体里「作为根形状」的匿名 struct：`var req struct{…}` 与
// `req := struct{…}{…}`。嵌套在里面的 struct 不算（那是对应字段的递归比对）。
func requestStructExprs(body *ast.BlockStmt) []ast.Expr {
	var out []ast.Expr
	add := func(e ast.Expr) {
		if st, ok := e.(*ast.StructType); ok && st != nil {
			out = append(out, st)
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.ValueSpec:
			if x.Type != nil {
				add(x.Type)
				return true
			}
			for _, v := range x.Values {
				if lit, ok := v.(*ast.CompositeLit); ok {
					add(lit.Type)
				}
			}
		case *ast.AssignStmt:
			for _, rhs := range x.Rhs {
				if lit, ok := rhs.(*ast.CompositeLit); ok {
					add(lit.Type)
				}
			}
		}
		return true
	})
	return out
}

// typeOfWireExpr 把「响应 map 字面量」或「匿名 struct 类型」转成反射类型。
func typeOfWireExpr(e ast.Expr) (reflect.Type, error) {
	switch x := e.(type) {
	case *ast.StructType:
		return structTypeOf(x)
	case *ast.CompositeLit:
		mt, ok := x.Type.(*ast.MapType)
		if !ok {
			return nil, fmt.Errorf("复合字面量的类型不是 map（%T）", x.Type)
		}
		elem, err := typeOfASTExpr(mt.Value)
		if err != nil {
			return nil, err
		}
		var fields []reflect.StructField
		seen := map[string]bool{}
		for _, elt := range x.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				return nil, fmt.Errorf("map 字面量的元素不是 key:value（%T）—— 无法静态取字段名", elt)
			}
			key, ok := kv.Key.(*ast.BasicLit)
			if !ok || key.Kind != token.STRING {
				return nil, fmt.Errorf("map 字面量的键不是字符串字面量（%T）—— 无法静态取字段名", kv.Key)
			}
			name, err := strconv.Unquote(key.Value)
			if err != nil {
				return nil, fmt.Errorf("map 键解析失败：%v", err)
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			fields = append(fields, reflect.StructField{
				Name: fmt.Sprintf("F%d", len(fields)),
				Type: elem,
				Tag:  reflect.StructTag(fmt.Sprintf("json:%q", name)),
			})
		}
		if len(fields) == 0 {
			return nil, fmt.Errorf("map 字面量是空的 —— 没有可核对的字段")
		}
		return reflect.StructOf(fields), nil
	}
	return nil, fmt.Errorf("不支持的 wire 表达式形态 %T", e)
}

// structTypeOf 把 AST 里的 struct 类型转成 reflect.Type（字段名/类型/json tag 照抄）。
func structTypeOf(st *ast.StructType) (reflect.Type, error) {
	var fields []reflect.StructField
	seen := map[string]bool{}
	add := func(name, tag string, typ reflect.Type) error {
		if name == "" || !ast.IsExported(name) {
			return nil // encoding/json 看不见未导出字段
		}
		if seen[name] {
			return nil
		}
		seen[name] = true
		fields = append(fields, reflect.StructField{Name: name, Type: typ, Tag: reflect.StructTag(tag)})
		return nil
	}
	for _, f := range st.Fields.List {
		tag := ""
		if f.Tag != nil {
			tag = strings.Trim(f.Tag.Value, "`")
		}
		typ, err := typeOfASTExpr(f.Type)
		if err != nil {
			return nil, err
		}
		if len(f.Names) == 0 {
			// 内嵌字段：按 encoding/json 的规则提升（本仓库的 handler 里没有这种写法，
			// 出现即报错——不猜）。
			return nil, fmt.Errorf("内嵌字段（%T）暂不支持静态展开", f.Type)
		}
		for _, nm := range f.Names {
			if err := add(nm.Name, tag, typ); err != nil {
				return nil, err
			}
		}
	}
	return reflect.StructOf(fields), nil
}

// typeOfASTExpr 把类型表达式转成反射类型。
//
// 本包内/外部包的自定义类型 → `any`：**键集仍然被核**，只是不做类型判定。
// 这不是静默放宽——之所以能接受，是因为这几对 pair 要守的是「字段集」（F1 的核心）。
func typeOfASTExpr(e ast.Expr) (reflect.Type, error) {
	switch x := e.(type) {
	case *ast.Ident:
		switch x.Name {
		case "string":
			return reflect.TypeOf(""), nil
		case "bool":
			return reflect.TypeOf(true), nil
		case "int":
			return reflect.TypeOf(int(0)), nil
		case "int8":
			return reflect.TypeOf(int8(0)), nil
		case "int16":
			return reflect.TypeOf(int16(0)), nil
		case "int32":
			return reflect.TypeOf(int32(0)), nil
		case "int64":
			return reflect.TypeOf(int64(0)), nil
		case "uint", "uint8", "uint16", "uint32", "uint64":
			return reflect.TypeOf(uint64(0)), nil
		case "float32":
			return reflect.TypeOf(float32(0)), nil
		case "float64":
			return reflect.TypeOf(float64(0)), nil
		case "any", "interface":
			return anyType, nil
		}
		return anyType, nil
	case *ast.InterfaceType:
		return anyType, nil
	case *ast.StarExpr:
		elem, err := typeOfASTExpr(x.X)
		if err != nil {
			return nil, err
		}
		return reflect.PointerTo(elem), nil
	case *ast.ArrayType:
		elem, err := typeOfASTExpr(x.Elt)
		if err != nil {
			return nil, err
		}
		return reflect.SliceOf(elem), nil
	case *ast.MapType:
		key, err := typeOfASTExpr(x.Key)
		if err != nil {
			return nil, err
		}
		elem, err := typeOfASTExpr(x.Value)
		if err != nil {
			return nil, err
		}
		return reflect.MapOf(key, elem), nil
	case *ast.StructType:
		return structTypeOf(x)
	case *ast.SelectorExpr:
		return anyType, nil
	case *ast.Ellipsis:
		elem, err := typeOfASTExpr(x.Elt)
		if err != nil {
			return nil, err
		}
		return reflect.SliceOf(elem), nil
	}
	return nil, fmt.Errorf("不支持的类型表达式 %T", e)
}

// ============================================================================
// 7. 源码证人（AST）：宽投影表 / 未配对表 / 注册语句，全部要能追到真实符号
// ============================================================================

// schemaWitness 是「这条声明凭什么成立」的机械证据。
//
// Witness 的形态是 `相对仓库根的 .go 文件:标识符`（如
// `internal/server/handlers.go:writeJSON`）。它必须**真实存在**：符号改名/删除 →
// 证人失效 → 门禁红。这是「过期即红」在文档层之外的第二种体现。
type schemaWitness struct {
	// Reason 是给人看的依据（必填）。
	Reason string
	// Witness 是机械证据（必填）。
	Witness string
}

// parsedGoFile 是「解析过的源码文件」缓存（同一文件被多处证人引用时只解析一次）。
type parsedGoFile struct {
	src  []byte
	fset *token.FileSet
	file *ast.File
	err  error
}

var goFileCache = map[string]*parsedGoFile{}

// parseRepoGoFile 解析一个相对仓库根路径的 Go 文件（带缓存；解析失败也缓存）。
func parseRepoGoFile(rel string) *parsedGoFile {
	if cached, ok := goFileCache[rel]; ok {
		return cached
	}
	out := &parsedGoFile{}
	out.src, out.err = os.ReadFile(filepath.Join(repoRoot, rel))
	if out.err != nil {
		out.err = fmt.Errorf("读不了 %s：%v", rel, out.err)
		goFileCache[rel] = out
		return out
	}
	out.fset = token.NewFileSet()
	out.file, out.err = parser.ParseFile(out.fset, rel, out.src, 0)
	if out.err != nil {
		out.err = fmt.Errorf("解析 %s 失败：%v", rel, out.err)
	}
	goFileCache[rel] = out
	return out
}

// symbolInFile 报告 rel 里是否存在名为 ident 的声明（函数/方法/类型/变量/常量）。
//
// 只认**声明**，不认「注释里提了一句」：证人必须是代码事实。
func symbolInFile(rel, ident string) (bool, error) {
	parsed := parseRepoGoFile(rel)
	if parsed.err != nil {
		return false, parsed.err
	}
	found := false
	for _, decl := range parsed.file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name.Name == ident {
				found = true
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					if sp.Name.Name == ident {
						found = true
					}
				case *ast.ValueSpec:
					for _, nm := range sp.Names {
						if nm.Name == ident {
							found = true
						}
					}
				}
			}
		}
	}
	return found, nil
}

// identUsedInFile 报告 rel 里是否**在代码里**用到过这个标识符（注释不算）。
func identUsedInFile(rel, ident string) (bool, error) {
	parsed := parseRepoGoFile(rel)
	if parsed.err != nil {
		return false, parsed.err
	}
	used := false
	ast.Inspect(parsed.file, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == ident {
			used = true
		}
		return !used
	})
	return used, nil
}

// witnessProblems 校验一条 `文件:标识符` 证人；格式不对 / 文件读不了 / 符号不存在都是 problem。
//
// 证人允许两种形态：① 文件里**声明**了它（类型/函数/变量/常量）；② 文件里**在代码里用到**
// 了它（调用点）。两种都是「这个文件真的负责这件事」的证据；注释里提一句不算（走 AST）。
func witnessProblems(where, witness string) []string {
	file, ident, ok := strings.Cut(witness, ":")
	if !ok || file == "" || ident == "" {
		return []string{fmt.Sprintf("%s 的证人 %q 不是 `相对路径:标识符` 形态", where, witness)}
	}
	declared, err := symbolInFile(file, ident)
	if err != nil {
		return []string{fmt.Sprintf("%s 的证人 %q 失效：%v", where, witness, err)}
	}
	if declared {
		return nil
	}
	used, err := identUsedInFile(file, ident)
	if err != nil {
		return []string{fmt.Sprintf("%s 的证人 %q 失效：%v", where, witness, err)}
	}
	if !used {
		return []string{fmt.Sprintf("%s 的证人 %q 失效：%s 里既没有声明也没有使用 %q（符号改名/删除 → 这条依据不再成立）",
			where, witness, file, ident)}
	}
	return nil
}

// fileMustContain 报告 rel 的源码里是否出现 want（源码事实，不是注释——注释也算在源码里，
// 所以这里只用于「函数体里出现了这个调用」这类高置信度的判据）。
func fileMustContain(rel, want string) (bool, error) {
	parsed := parseRepoGoFile(rel)
	if parsed.err != nil {
		return false, parsed.err
	}
	return strings.Contains(string(parsed.src), want), nil
}

// assignmentText 返回 srcFile 里 `name := <expr>` / `name = <expr>` 的源码原文。
//
// 用途：admin 中间件那两层（sessMW + AdminMiddleware）写在 `adminMW := sessMW(...)`
// 的定义里，而不是每条 mux.Handle 的注册语句里 —— 判据必须能读到那段定义。
func assignmentText(srcFile, name string) (string, error) {
	parsed := parseRepoGoFile(filepath.ToSlash(srcFile))
	if parsed.err != nil {
		return "", parsed.err
	}
	out := ""
	ast.Inspect(parsed.file, func(n ast.Node) bool {
		if out != "" {
			return false
		}
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 {
			return true
		}
		id, ok := as.Lhs[0].(*ast.Ident)
		if !ok || id.Name != name {
			return true
		}
		p, e := parsed.fset.Position(as.Pos()), parsed.fset.Position(as.End())
		if p.Offset >= 0 && e.Offset <= len(parsed.src) && e.Offset > p.Offset {
			out = string(parsed.src[p.Offset:e.Offset])
		}
		return false
	})
	if out == "" {
		return "", fmt.Errorf("%s 里没有 %s 的赋值语句", srcFile, name)
	}
	return out, nil
}

// ============================================================================
// 8. 宽投影（additionalProperties）白名单
// ============================================================================

// wideSchemas 是**唯一允许**成为「宽投影」（`additionalProperties` 且没有 properties）的
// schema 名单。键有两种形态：
//
//	`文件:schema 名`                    —— components.schemas 里的具名 schema
//	`文件:path:<点号路径>`              —— 具名 schema 内部的**内联**宽投影（没有名字可登记）
//
// 为什么必须有这张表（F3）：`compareStruct` 遇到宽 schema 会直接 return —— 那是**零检查**。
// 于是「把任意 schema 改成 `additionalProperties: true`」就能让那一对 DTO 静默退出比对。
// 现在宽投影必须显式登记（理由 + 证人），未登记的宽 schema 一律红；表里的条目还要证明
// 两件事：① 它现在**仍然**是宽的（长出 properties = 变成可比的，必须删掉这条免）；
// ② 证人（产生这个宽投影的 Go 类型/函数）真实存在。
var wideSchemas = map[string]schemaWitness{
	"resource.yaml:RenderedCapability": {
		Reason: "渲染后的能力文档 = 用户 playbook 里的任意 YAML（字段集由用户的模板决定，不是中枢的类型）；" +
			"中枢只做 JSON 层面的归一化，所以 schema 只能声明 additionalProperties",
		Witness: "internal/playbook/playbook.go:RenderResult",
	},
	"resource.yaml:RenderedSource": {
		Reason:  "渲染后的源文档 = 用户 playbook 里的任意 YAML（同 RenderedCapability）；字段集由模板决定",
		Witness: "internal/playbook/playbook.go:RenderResult",
	},
	"resource.yaml:path:components.schemas.NodeStatus.properties.effectiveVars.additionalProperties": {
		Reason: "effectiveVars 的 value 是 {value,source}，但 spec 用 additionalProperties: true 表达" +
			"（有效值离开命名空间没有意义，中枢不做形状承诺）；这是 spec 的**宽松**选择，不是漏写",
		Witness: "internal/server/resource_node.go:EffectiveVars",
	},
	"resource.yaml:PlaybookUpgradedEntry": {
		Reason: "「已存在且被本次安装升级」的一条 = {name@version} 的任意投影（同 Rendered*：由用户包决定）；" +
			"Go 侧是 []map[string]any / []string 的并集，没有固定字段",
		Witness: "internal/playbook/playbook.go:Receipt",
	},
}

// wideSchemaProblems 校验宽投影白名单自身：schema 存在、现在仍是宽的、证人真实存在。
func wideSchemaProblems(docs specDocs, wide map[string]schemaWitness) []string {
	var problems []string
	checker := &dtoChecker{docs: docs}
	for _, key := range sortedKeys(wide) {
		entry := wide[key]
		file, rest, ok := strings.Cut(key, ":")
		if !ok || file == "" || rest == "" {
			problems = append(problems, fmt.Sprintf("wideSchemas 的键 %q 不是 `文件:...` 形态", key))
			continue
		}
		if strings.TrimSpace(entry.Reason) == "" {
			problems = append(problems, fmt.Sprintf("wideSchemas 的 %s 没有写理由（宽投影必须说明为什么无从逐字段比对）", key))
		}
		problems = append(problems, witnessProblems("wideSchemas 的 "+key, entry.Witness)...)
		var schema map[string]any
		var found bool
		if path, isPath := strings.CutPrefix(rest, "path:"); isPath {
			schema, found = schemaAtDottedPath(docs, file, path)
		} else {
			schema, _, found = checker.lookupSchema(file, rest)
		}
		if !found {
			problems = append(problems, fmt.Sprintf("wideSchemas 的 %s 指向了不存在的 schema（路径/名字变了 → 过期）", key))
			continue
		}
		if !wideSchema(schema) {
			problems = append(problems, fmt.Sprintf(
				"wideSchemas 已过期：%s 现在**不是**宽投影了（有 properties/allOf），请从表里删掉它并让它进字段级比对", key))
		}
	}
	return problems
}

// ============================================================================
// 9. 未配对 schema 的显式清单 + 完整性门禁（F1）
// ============================================================================

// unpairedSchemas 是「不做字段级 pair」的**显式**清单。键是 `文件:schema 名`。
//
// 完整性门禁（schemaCoverageProblems）要求五份 spec 的**每一个** component schema 都
// 满足其一：① 在 parityPairs 里；② 被某对 pair 顺着 `$ref` **递归比对**到（
// schemaReachability 算出来的真实可达集）；③ 在这张表里（理由 + 证人）。
// 三者都不满足 = 红 —— 这就是「227 个 schema 只有 84 个有门禁」那个假绿面的封堵。
//
// 表里的条目也**会过期**：某个 schema 一旦进了 pair 或变得可达，这行就必须删掉。
var unpairedSchemas = map[string]schemaWitness{
	// ---- resource.yaml：写请求信封 ----
	// 九个 *Write / *Patch 都是「信封 + 调用方各自的 spec 类型」：`decodeWriteEnvelope`
	// 把 apiVersion/kind/metadata 解到 objectMeta、spec 解到各域的 **typed** 目标，
	// Go 侧从来没有一个「写信封」的具名类型可反射 → 只能显式免检（形状在 decode 的校验里）。
	"resource.yaml:CapabilityPatch": {
		Witness: "internal/server/api_envelope.go:decodeWriteEnvelope",
		Reason:  "写信封（apiVersion/kind/metadata/spec）由 decodeWriteEnvelope 解到各域自己的 spec 类型，Go 侧没有统一的具名结构体",
	},
	"resource.yaml:CapabilityWrite": {
		Witness: "internal/server/api_envelope.go:decodeWriteEnvelope",
		Reason:  "写信封（同上）：spec 落到 capabilitySpec 的 Go 类型，metadata 落到 objectMeta",
	},
	"resource.yaml:NamespaceConfigWrite": {
		Witness: "internal/server/api_envelope.go:decodeWriteEnvelope",
		Reason:  "写信封（同上）：spec 落到 namespaceConfigDoc",
	},
	"resource.yaml:NamespaceWrite": {
		Witness: "internal/server/api_envelope.go:decodeWriteEnvelope",
		Reason:  "写信封（同上）：spec 落到 namespaceSpec",
	},
	"resource.yaml:NodeConfigWrite": {
		Witness: "internal/server/api_envelope.go:decodeWriteEnvelope",
		Reason:  "写信封（同上）：spec 落到 nodeConfigWrapper",
	},
	"resource.yaml:NodeWrite": {
		Witness: "internal/server/api_envelope.go:decodeWriteEnvelope",
		Reason:  "写信封（同上）：spec 落到 nodeSpec",
	},
	"resource.yaml:PlaybookWrite": {
		Witness: "internal/server/api_envelope.go:decodeWriteEnvelope",
		Reason:  "写信封（同上）：spec 落到 playbook 包的包类型",
	},
	"resource.yaml:SourceWrite": {
		Witness: "internal/server/api_envelope.go:decodeWriteEnvelope",
		Reason:  "写信封（同上）：spec 落到 srcSpecDoc",
	},
	"resource.yaml:SystemConfigWrite": {
		Witness: "internal/server/api_envelope.go:decodeWriteEnvelope",
		Reason:  "写信封（同上）：两个键（metadata + retention）分别落到 objectMeta / retentionDoc",
	},

	// ---- resource.yaml：两个 oneOf / 无 JSON 写出的错误体 ----
	"resource.yaml:AuthFlow": {
		Witness: "internal/server/cli_auth_handlers.go:writeApproveErr",
		Reason: "409 的两种形状（{status} 或 {error}）是 `oneOf`，而 Go 侧是两个独立的写出点" +
			"（写 {status} 的 writeJSON 与写 {error} 的 writeErr），没有单一具名类型可比",
	},
	"resource.yaml:NodeUploadError": {
		Witness: "internal/server/artifact_upload.go:reject",
		Reason:  `节点产物上传的封闭错误体由 reject/rejectError 逐处写出（{"error":"<code>"}），没有具名结构体`,
	},

	// ---- agent.yaml ----
	"agent.yaml:ArtifactUploadErrorBody": {
		Witness: "internal/server/artifact_upload.go:reject",
		Reason:  "上传失败的 4xx body 由中枢的 reject 写出（同 NodeUploadError），协议包里没有对应类型",
	},

	// ---- ingress.yaml ----
	"ingress.yaml:EventIngestRequest": {
		Witness: "internal/api/events.go:eventIngestRequest",
		Reason:  "投递体是 internal/api 里的未导出类型（跨包无法反射），字段集由 api 包的严格解码保证",
	},

	// ---- auth.yaml：go-webauthn 库的结构体 ----
	"auth.yaml:CredentialAssertionResponse": {
		Witness: "internal/auth/webauthn.go:FinishLogin",
		Reason:  "WebAuthn 断言响应的形状由 go-webauthn 的 protocol 包定义（第三方库），本仓没有自己的类型",
	},
	"auth.yaml:CredentialCreationResponse": {
		Witness: "internal/auth/webauthn.go:FinishRegister",
		Reason:  "WebAuthn 注册响应的形状由 go-webauthn 的 protocol 包定义（第三方库），本仓没有自己的类型",
	},
	"auth.yaml:WebAuthnRequestOptions": {
		Witness: "internal/auth/webauthn.go:BeginLogin",
		Reason:  "登录选项（publicKey 挑战）直接序列化 go-webauthn 的 protocol.CredentialAssertion，本仓没有自己的类型",
	},
}

// schemaCoverageStats 是完整性门禁的自检输出（防止「0 项检查也绿」）。
type schemaCoverageStats struct {
	total, paired, reachable, unpaired int
}

// schemaAtDottedPath 按点号路径从某个文档的根开始下钻取 schema（内联宽投影登记用）。
func schemaAtDottedPath(docs specDocs, file, path string) (map[string]any, bool) {
	doc, ok := docs[file]
	if !ok {
		return nil, false
	}
	var node any = doc
	for _, seg := range strings.Split(path, ".") {
		m, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := m[seg]
		if !ok {
			return nil, false
		}
		node = v
	}
	schema, ok := node.(map[string]any)
	return schema, ok
}

// componentSchema 取某文件 components.schemas 下的一个 schema。
func componentSchema(docs specDocs, file, name string) (map[string]any, bool) {
	schemas, ok := nestedMap(docs[file], "components", "schemas")
	if !ok {
		return nil, false
	}
	schema, ok := schemas[name].(map[string]any)
	return schema, ok
}

// schemaRefTarget 把一条 `$ref` 解析成 (目标文件, components.schemas 下的名字)。
func schemaRefTarget(currentFile, ref string) (string, string, error) {
	filePart, pointer, ok := strings.Cut(ref, "#")
	target := currentFile
	if filePart != "" {
		target = strings.TrimPrefix(filePart, "./")
	}
	const prefix = "/components/schemas/"
	if !ok || !strings.HasPrefix(pointer, prefix) {
		return "", "", fmt.Errorf("$ref 不指向 components.schemas：%s", ref)
	}
	name := strings.TrimPrefix(pointer, prefix)
	if name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("$ref 的 schema 名不合法：%s", ref)
	}
	return target, name, nil
}

// schemaReachability 从每一对 pair 的 schema 出发，沿 `$ref` 走一遍 schema 图。
//
// 走到的集合 = 「递归比对真的会看到的 schema」：compareStruct/compareValue 会顺着
// properties/items/additionalProperties/allOf 下钻，所以这条遍历与它是同一张图。
func schemaReachability(docs specDocs, pairs []parityPair) (map[string]bool, []string) {
	var problems []string
	reach := map[string]bool{}
	var walk func(file string, node any)
	walk = func(file string, node any) {
		switch v := node.(type) {
		case map[string]any:
			if ref, ok := v["$ref"].(string); ok {
				target, name, err := schemaRefTarget(file, ref)
				if err != nil {
					problems = append(problems, fmt.Sprintf("DTO 可达性遍历：%v（当前文件 %s）", err, file))
					return
				}
				key := target + ":" + name
				if reach[key] {
					return
				}
				schema, ok := componentSchema(docs, target, name)
				if !ok {
					problems = append(problems, fmt.Sprintf("DTO 可达性遍历：$ref 指向不存在的 schema %s", key))
					return
				}
				reach[key] = true
				walk(target, schema)
				return
			}
			for _, k := range sortedKeys(v) {
				walk(file, v[k])
			}
		case []any:
			for _, item := range v {
				walk(file, item)
			}
		}
	}
	for _, pair := range pairs {
		key := pair.File + ":" + pair.Schema
		schema, ok := componentSchema(docs, pair.File, pair.Schema)
		if !ok {
			problems = append(problems, fmt.Sprintf("parityPairs 的 %s 指向不存在的 schema %s", pair.GoLabel, key))
			continue
		}
		reach[key] = true
		walk(pair.File, schema)
	}
	return reach, problems
}

// schemaCoverageProblems 是「227 个 component schema 每一个都有归属」的那道门禁。
func schemaCoverageProblems(docs specDocs, pairs []parityPair) ([]string, schemaCoverageStats) {
	var problems []string
	var stats schemaCoverageStats
	reach, walkProblems := schemaReachability(docs, pairs)
	problems = append(problems, walkProblems...)
	paired := map[string]bool{}
	for _, pair := range pairs {
		paired[pair.File+":"+pair.Schema] = true
	}
	for _, file := range specFileNames {
		schemas, ok := nestedMap(docs[file], "components", "schemas")
		if !ok {
			problems = append(problems, fmt.Sprintf("%s 没有 components.schemas", file))
			continue
		}
		for _, name := range sortedKeys(schemas) {
			key := file + ":" + name
			stats.total++
			entry, listed := unpairedSchemas[key]
			switch {
			case paired[key]:
				stats.paired++
				if listed {
					problems = append(problems, fmt.Sprintf(
						"unpairedSchemas 已过期：%s 已经进了 parityPairs（字段级比对在做），请从表里删掉", key))
				}
			case reach[key] && !listed:
				stats.reachable++
			case listed:
				stats.unpaired++
				if reach[key] {
					problems = append(problems, fmt.Sprintf(
						"unpairedSchemas 已过期：%s 已经被某对 pair 递归比对覆盖，请从表里删掉", key))
				}
				if strings.TrimSpace(entry.Reason) == "" {
					problems = append(problems, fmt.Sprintf("unpairedSchemas 的 %s 没写理由", key))
				}
				problems = append(problems, witnessProblems("unpairedSchemas 的 "+key, entry.Witness)...)
			default:
				problems = append(problems, fmt.Sprintf(
					"schema 没有任何门禁：%s 既不在 parityPairs、也不被任何 pair 递归覆盖、也不在 unpairedSchemas —— "+
						"每个 component schema 都必须有归属（被比对或被显式免检）", key))
			}
		}
	}
	return problems, stats
}

// ============================================================================
// 10. responses 与参数（F2）：spec 的「声明」必须与实现的「读取/回答」对账
// ============================================================================

// specOpDetail 是 operation 上与 F2 相关的细节（query 参数 / 响应码 / 有没有请求体）。
type specOpDetail struct {
	QueryParams []string
	Responses   []string
	HasBody     bool
}

// collectSpecDetails 走一遍五份 spec，把每条 operation 的参数与响应码摊平成表。
//
// 参数要**解 $ref**（`./resource.yaml#/components/parameters/LimitQuery` 这种跨文件引用
// 在 events/ingress 里到处都是）：不解就是漏检。
func collectSpecDetails(docs specDocs) (map[string]specOpDetail, []string) {
	var problems []string
	out := map[string]specOpDetail{}
	addParams := func(file, where string, raw any, detail *specOpDetail) {
		list, ok := raw.([]any)
		if !ok {
			return
		}
		for _, item := range list {
			param, ok := item.(map[string]any)
			if !ok {
				problems = append(problems, fmt.Sprintf("%s：%s 的 parameters 元素不是对象", file, where))
				continue
			}
			if ref, ok := param["$ref"].(string); ok {
				node, _, err := docs.resolveRefFrom(file, ref)
				if err != nil {
					problems = append(problems, fmt.Sprintf("%s：%s 的参数 $ref 解析失败：%v", file, where, err))
					continue
				}
				resolved, ok := node.(map[string]any)
				if !ok {
					problems = append(problems, fmt.Sprintf("%s：%s 的参数 $ref 不是对象：%s", file, where, ref))
					continue
				}
				param = resolved
			}
			if in, _ := param["in"].(string); in != "query" {
				continue
			}
			name, _ := param["name"].(string)
			if name == "" {
				problems = append(problems, fmt.Sprintf("%s：%s 有一个没有 name 的 query 参数", file, where))
				continue
			}
			detail.QueryParams = append(detail.QueryParams, name)
		}
	}
	for _, file := range specFileNames {
		doc, ok := docs[file]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s 没有载入", file))
			continue
		}
		paths, _ := doc["paths"].(map[string]any)
		for _, rawPath := range sortedKeys(paths) {
			item, ok := paths[rawPath].(map[string]any)
			if !ok {
				continue
			}
			if ref, ok := item["$ref"].(string); ok {
				node, _, err := docs.resolveRefFrom(file, ref)
				if err != nil {
					problems = append(problems, fmt.Sprintf("%s：path %q 的 pathItem $ref 解析失败：%v", file, rawPath, err))
					continue
				}
				item, _ = node.(map[string]any)
			}
			for _, method := range specMethods {
				node, ok := item[method]
				if !ok {
					continue
				}
				op, ok := node.(map[string]any)
				if !ok {
					continue
				}
				key := strings.ToUpper(method) + " " + rawPath
				detail := out[key]
				addParams(file, key, item["parameters"], &detail)
				addParams(file, key, op["parameters"], &detail)
				if _, ok := op["requestBody"]; ok {
					detail.HasBody = true
				}
				if responses, ok := op["responses"].(map[string]any); ok {
					detail.Responses = sortedKeys(responses)
				}
				out[key] = detail
			}
		}
	}
	return out, problems
}

// queryParamUsage 收集 internal/server 非测试源码里**真的被读出来**的 query 参数名。
//
// 三种形态（本仓库全部覆盖）：
//
//	taskQueryInt/Bool/String(r, "x")   —— resource_task.go 的严格解码器
//	r.URL.Query().Get("x") / .Has("x") —— 直接读
//	q.Get("x")                         —— q := r.URL.Query() 之后再读
func queryParamUsage(dir string) (map[string][]string, []string) {
	var problems []string
	usage := map[string][]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []string{fmt.Sprintf("读不了 %s：%v", dir, err)}
	}
	record := func(name, pos string) {
		usage[name] = append(usage[name], pos)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("读不了 %s：%v", path, err))
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			problems = append(problems, fmt.Sprintf("解析 %s 失败：%v", path, err))
			continue
		}
		pos := func(n ast.Node) string {
			p := fset.Position(n.Pos())
			return fmt.Sprintf("%s:%d", filepath.Base(p.Filename), p.Line)
		}
		// 每个文件一份：哪些局部变量来自 r.URL.Query()（`q := r.URL.Query()`）。
		queryValuesVars := map[string]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			// q := r.URL.Query() 之后 q.Get("x") 也算读 query。
			if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == 1 && len(as.Rhs) == 1 {
				if id, ok := as.Lhs[0].(*ast.Ident); ok && expressionIsURLQuery(as.Rhs[0]) {
					queryValuesVars[id.Name] = true
				}
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			// taskQueryInt 是**包级函数**（call.Fun 是 Ident），Get/Has 是方法（SelectorExpr）：
			// 两种形态都要认，漏掉前者就是「tailLines/sinceSeconds/previous 三个参数静默漏检」。
			var sel *ast.SelectorExpr
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				sel = &ast.SelectorExpr{X: fun, Sel: fun}
				switch fun.Name {
				case "taskQueryInt", "taskQueryBool", "taskQueryString":
					if len(call.Args) >= 2 {
						if lit, ok := call.Args[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if v, err := strconv.Unquote(lit.Value); err == nil {
								record(v, pos(call))
							}
						}
					}
				}
				return true
			case *ast.SelectorExpr:
				sel = fun
			default:
				return true
			}
			switch sel.Sel.Name {
			case "taskQueryInt", "taskQueryBool", "taskQueryString":
				if len(call.Args) >= 2 {
					if lit, ok := call.Args[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if v, err := strconv.Unquote(lit.Value); err == nil {
							record(v, pos(call))
						}
					}
				}
			case "Get", "Has":
				if len(call.Args) != 1 {
					return true
				}
				lit, ok := call.Args[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				receiverIsQuery := expressionIsURLQuery(sel.X)
				if id, ok := sel.X.(*ast.Ident); ok && queryValuesVars[id.Name] {
					receiverIsQuery = true
				}
				if !receiverIsQuery {
					return true
				}
				if v, err := strconv.Unquote(lit.Value); err == nil {
					record(v, pos(call))
				}
			}
			return true
		})
	}
	return usage, problems
}

// expressionIsURLQuery 报告表达式是否形如 `X.URL.Query()`。
func expressionIsURLQuery(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Query" {
		return false
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	if !ok || inner.Sel.Name != "URL" {
		return false
	}
	return true
}

// stringLiterals 收集 dir 下非测试源码里出现的**全部**字符串字面量（AST 取值，不含注释）。
func stringLiterals(dir string) (map[string]string, []string) {
	var problems []string
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []string{fmt.Sprintf("读不了 %s：%v", dir, err)}
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("读不了 %s：%v", path, err))
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			problems = append(problems, fmt.Sprintf("解析 %s 失败：%v", path, err))
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if _, seen := out[v]; !seen {
				p := fset.Position(lit.Pos())
				out[v] = fmt.Sprintf("%s:%d", filepath.Base(p.Filename), p.Line)
			}
			return true
		})
	}
	return out, problems
}

// unreadQueryParamWhitelist 是「spec 声明了、实现却没读」的显式白名单（理由 + 证人）。
//
// 现在的 spec 里**没有**这种参数（`tail` 那个漂移已经被修成 tailLines）；这张表是为了
// 让「将来真的需要声明一个不读的参数」这件事必须走显式登记，而不是静默留一个死参数。
var unreadQueryParamWhitelist = map[string]schemaWitness{}

// undeclaredQueryParamWhitelist 是「实现读了、spec 没声明」的显式白名单。
// 内部诊断参数（?debug=1 这类）才允许进来；普通参数必须补进 spec。
var undeclaredQueryParamWhitelist = map[string]schemaWitness{}

// statusException 是「响应码声明」的显式例外（理由 + 证人 + 会过期）。
type statusException struct {
	Reason  string
	Witness string
}

// requestBodyNo400Exceptions：声明了 requestBody 却**没有** 400 的 operation。
var requestBodyNo400Exceptions = map[string]statusException{
	"POST /v1/auth/login": {
		Reason: "WebAuthn 断言校验的**所有**失败都在 auth.FinishLogin 里归一成 401" +
			"（api/auth.yaml 的 login 自己的约定：「失败统一 401，不区分用户不存在 / 断言无效 / 账户禁用」），" +
			"requestBody 解析失败也走这条路 —— 所以实现里没有 400 这个分支",
		Witness: "internal/server/auth_handlers.go:login",
	},
}

// securityNo401Exceptions：声明了 security 却**没有** 401 的 operation。
var securityNo401Exceptions = map[string]statusException{
	"GET /v1/node/stream": {
		Reason: "WebSocket：spec 只声明 101（升级成功）。无凭据的普通 HTTP 请求连握手都过不了（426），" +
			"真正的鉴权发生在升级后的首帧 hello —— 401 在这个端点上不是一个会出现的响应码",
		Witness: "internal/ws/hub.go:GetNodeByTokenHash",
	},
}

// bareMount405Exceptions：这些 handler 文件**确实**会用 405 回答「方法不对」，
// 但对应的 spec operation 还没声明 405 —— 这是 spec 的已知缺口（不在本次改动所有权内），
// 按文件登记以便**过期即红**：一旦 spec 补上 405，这条就用不上了，必须删掉。
var bareMount405Exceptions = map[string]statusException{
	"admin_handlers.go": {
		Reason: "多个 /v1/admin/* 的 spec 都没声明 405，而 adminHandler 有两种缺口形态：" +
			"`/v1/admin/users` 与 `/v1/admin/users/{id}` 的分派对其它方法 writeErr(w, 405)；" +
			"`/v1/admin` 与 `/v1/admin/stats` 则**没有方法守卫**（任何方法都进 handler，连 405 都不回）—— " +
			"两者都是 spec 与实现的缺口（已上报；本 worker 不改 api/**，补 405 时请一并给 stats 补方法判定）",
		Witness: "internal/server/admin_handlers.go:adminHandler",
	},
}

// TestOpenAPIParityResponsesAndParams 是 F2：responses 与参数此前完全不在门禁内。
//
// 机械对账四件事：
//  1. spec 声明的每个 query 参数名，必须在 internal/server 的非测试源码里作为字符串字面量
//     出现（真有人读它）——反例就是被 review 抓到的 `tail`（spec 写 tail、实现只读 tailLines）。
//  2. 实现里真的读出来的每个 query 名，必须被至少一条 operation 声明。
//  3. 状态码正向（有限但真实）：requestBody → 400；CAS DELETE → 400+409；
//     security 非空 → 401；落在裸挂载下且 handler 有 405 分支 → 405。
//  4. 检查数量必须非零（防「0 项检查也绿」）。
func TestOpenAPIParityResponsesAndParams(t *testing.T) {
	docs, problems := loadSpecDocs()
	fatalIfProblems(t, "载入五份 spec", problems)
	ops, specProblems := collectSpecOps(docs)
	fatalIfProblems(t, "解析 operation", specProblems)
	details, detailProblems := collectSpecDetails(docs)
	fatalIfProblems(t, "解析参数/响应码", detailProblems)

	literals, literalProblems := stringLiterals(serverPkgDir)
	fatalIfProblems(t, "收集字符串字面量", literalProblems)
	usage, usageProblems := queryParamUsage(serverPkgDir)
	fatalIfProblems(t, "收集 query 参数读取点", usageProblems)
	patterns, patternProblems := collectMuxPatterns(serverPkgDir)
	fatalIfProblems(t, "静态收集 mux pattern", patternProblems)

	all := responsesAndParamsProblems(ops, details, literals, usage, patterns, statusExceptionTables{
		RequestBodyNo400: requestBodyNo400Exceptions,
		SecurityNo401:    securityNo401Exceptions,
		BareMount405:     bareMount405Exceptions,
	})
	declaredCount := 0
	for _, op := range ops {
		declaredCount += len(details[op.Key()].QueryParams)
	}
	t.Logf("F2 对账：operation=%d，query 声明=%d（去重 %d），实现读取=%d，例外表=%d/%d/%d",
		len(ops), declaredCount, len(queryParamNames(ops, details)), len(usage),
		len(requestBodyNo400Exceptions), len(securityNo401Exceptions), len(bareMount405Exceptions))
	fatalIfProblems(t, "responses 与参数", all)
}

// queryParamNames 是 spec 里声明过的 query 参数名集合（失败信息与自检日志用）。
func queryParamNames(ops []specOp, details map[string]specOpDetail) []string {
	seen := map[string]bool{}
	for _, op := range ops {
		for _, name := range details[op.Key()].QueryParams {
			seen[name] = true
		}
	}
	return sortedKeys(seen)
}

// statusExceptionTables 是 F2 的三张例外表（参数化是为了让变异测试能「换成空表」验证它们
// 确实在承重，而不是永久后门）。
type statusExceptionTables struct {
	RequestBodyNo400 map[string]statusException
	SecurityNo401    map[string]statusException
	BareMount405     map[string]statusException
}

// responsesAndParamsProblems 是 F2 的纯函数实现（docs 侧的输入全部显式传入）。
func responsesAndParamsProblems(ops []specOp, details map[string]specOpDetail,
	literals map[string]string, usage map[string][]string, patterns []routePattern,
	exceptions statusExceptionTables) []string {
	var all []string
	// ① query 参数正向：spec 声明了就一定有人读。
	declared := map[string][]string{}
	declaredCount := 0
	for _, op := range ops {
		detail, ok := details[op.Key()]
		if !ok {
			all = append(all, fmt.Sprintf("参数对账缺数据：%s 没有 detail（collectSpecDetails 漏了）", op.Key()))
			continue
		}
		for _, param := range detail.QueryParams {
			declaredCount++
			declared[param] = append(declared[param], op.Key())
			if _, ok := literals[param]; ok {
				continue
			}
			if entry, listed := unreadQueryParamWhitelist[param]; listed {
				all = append(all, witnessProblems("unreadQueryParamWhitelist 的 "+param, entry.Witness)...)
				continue
			}
			all = append(all, fmt.Sprintf(
				"query 参数正向：%s 声明的 `%s` 在 internal/server/*.go（非测试）里根本没有作为字符串字面量出现 —— "+
					"没人读它（review 抓到的 `tail` 就是这样：spec 写 tail、实现只读 tailLines）", op.Key(), param))
		}
	}
	if declaredCount == 0 {
		all = append(all, "query 参数正向检查了 0 个参数 —— 解析器坏了（不许 0 项检查也绿）")
	}
	// ② query 参数反向：实现了就一定有 spec 声明。
	usageCount := 0
	for _, name := range sortedKeys(usage) {
		usageCount++
		if _, ok := declared[name]; ok {
			continue
		}
		if entry, listed := undeclaredQueryParamWhitelist[name]; listed {
			all = append(all, witnessProblems("undeclaredQueryParamWhitelist 的 "+name, entry.Witness)...)
			continue
		}
		all = append(all, fmt.Sprintf(
			"query 参数反向：实现读了 query 参数 `%s`（%s），但五份 spec 里没有任何 operation 声明它",
			name, usage[name][0]))
	}
	if usageCount == 0 {
		all = append(all, "query 参数反向检查了 0 个参数 —— 收集器坏了（不许 0 项检查也绿）")
	}

	// ③ 状态码正向。
	mux, muxProblems := newProbeMux(patterns)
	all = append(all, muxProblems...)
	bareHandlers := map[string]string{} // 裸 pattern → handler 文件
	for _, entry := range bareMountSpec() {
		bareHandlers[entry.Pattern] = entry.HandlerFile
	}
	used405 := map[string]bool{}
	opCount := 0
	for _, op := range ops {
		opCount++
		detail := details[op.Key()]
		has := func(code string) bool { return containsString(detail.Responses, code) }
		if detail.HasBody && !has("400") {
			if ex, ok := exceptions.RequestBodyNo400[op.Key()]; ok {
				all = append(all, witnessProblems("requestBodyNo400Exceptions 的 "+op.Key(), ex.Witness)...)
			} else {
				all = append(all, fmt.Sprintf(
					"状态码：%s 声明了 requestBody，却没声明 400（解码失败没有归宿）—— 响应码=%v",
					op.Key(), detail.Responses))
			}
		}
		// CAS 资源：DELETE 的 CAS 令牌只能走 ?resourceVersion=（没有 body），
		// 所以 400（令牌缺失/格式非法）与 409（令牌过期）都必须声明。
		if op.Method == "DELETE" && containsString(detail.QueryParams, "resourceVersion") {
			for _, code := range []string{"400", "409"} {
				if !has(code) {
					all = append(all, fmt.Sprintf(
						"状态码：CAS 资源的 %s 没声明 %s（DELETE 的 CAS 令牌只能走 ?resourceVersion=）—— 响应码=%v",
						op.Key(), code, detail.Responses))
				}
			}
		}
		if len(op.Security) > 0 && !has("401") {
			if ex, ok := exceptions.SecurityNo401[op.Key()]; ok {
				all = append(all, witnessProblems("securityNo401Exceptions 的 "+op.Key(), ex.Witness)...)
			} else {
				all = append(all, fmt.Sprintf(
					"状态码：%s 声明了 security=%v，却没声明 401 —— 响应码=%v", op.Key(), op.Security, detail.Responses))
			}
		}
		// 落在裸挂载下的 op：handler 真的按方法分派（有 405 分支）时，spec 必须声明 405。
		pat := probeMatch(mux, op.Method, samplePath(op.Path))
		handlerFile, isBare := bareHandlers[pat]
		if !isBare || has("405") || handlerFile == "" {
			continue
		}
		src, err := os.ReadFile(filepath.Join(serverPkgDir, handlerFile))
		if err != nil || !methodNotAllowedBranch(string(src)) {
			continue
		}
		if _, ok := exceptions.BareMount405[handlerFile]; ok {
			used405[handlerFile] = true
			continue
		}
		all = append(all, fmt.Sprintf(
			"状态码：%s 落在裸挂载 %q 下，%s 对其它方法回 405，但 spec 没声明 405 —— 响应码=%v",
			op.Key(), pat, handlerFile, detail.Responses))
	}
	// 例外过期即红：这轮一条都没用上的例外说明 spec 已经补上了 405。
	for _, file := range sortedKeys(exceptions.BareMount405) {
		if used405[file] {
			continue
		}
		all = append(all, fmt.Sprintf(
			"bareMount405Exceptions 已过期：%s 下没有任何「handler 会 405 但 spec 未声明 405」的 operation 了，请从表里删掉", file))
	}
	if opCount == 0 {
		all = append(all, "状态码检查了 0 条 operation —— 解析器坏了（不许 0 项检查也绿）")
	}
	return all
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

// requireOnlyProblem 断言「want 命中 ≥1 条」**且**「反方向一条都不命中」。
//
// 为什么需要它（F8）：只用 `strings.Contains(p, "Go 的")` 判「变异被抓到」时，反方向的
// 错误消息也含同样的子串 —— 变异方向是否正确根本没被验证。
func requireOnlyProblem(t *testing.T, mutation string, problems []string, want string, forbidden []string) {
	t.Helper()
	hits := 0
	for _, p := range problems {
		if strings.Contains(p, want) {
			hits++
		}
	}
	if hits == 0 {
		t.Fatalf("变异「%s」没有被抓到：期望出现包含 %q 的失败，实际 problems=%v", mutation, want, problems)
	}
	for _, bad := range forbidden {
		for _, p := range problems {
			if strings.Contains(p, bad) {
				t.Fatalf("变异「%s」的失败方向不唯一：期望只命中 %q，实际还命中了反方向的 %q：%s",
					mutation, want, bad, p)
			}
		}
	}
}

// replaceString 把列表里的 old 换成 new（变异辅助）。
func replaceString(list []string, old, new string) []string {
	out := make([]string, len(list))
	for i, v := range list {
		if v == old {
			out[i] = new
			continue
		}
		out[i] = v
	}
	return out
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
