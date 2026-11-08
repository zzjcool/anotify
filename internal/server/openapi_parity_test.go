package server

// 本文件是 Capability Spec v3 §7 **P6** 的契约门禁：`api/openapi.yaml` 与 Go 结构体的
// **字段级**双向核对。
//
// # 为什么需要它
//
// `scripts/check_openapi.py` 只核两件事：path 存在 + 每个 operation 有 security。
// 它**不看 schema**。代价在 Capability Spec v3 这一轮里反复付过账：
// `run`（5 个字段）/ `requires` / `env` 都曾经「Go 读了/发了，openapi 里根本没声明」，
// 而且漏了好几个月没人发现 —— 症状是**按 schema 生成的客户端永远提交不了实现**，
// 中枢那边一切正常，没有任何东西会红。P1-b2 手工补过一次，但没有东西阻止下一次漏。
//
// # 两个方向
//
//	A（漏声明）：Go 结构体里有的 json tag → schema 里必须有对应 property
//	B（幽灵字段）：schema 里声明的 property → Go 结构体里必须有对应 tag
//
// 两个方向都要，缺一半就有一半的漂移看不见：只有 A 时「文档多写一个字段」查不出来
// （按文档写客户端的人会拿到一个永远不出现的键），只有 B 时正是上面那类漏声明。
//
// # 递归与命名
//
//   - 嵌套逐层下钻（`spec` → `run` → `files`），`$ref` 在 components.schemas 内解开
//   - Go 侧按 **encoding/json 的规则**取 wire 字段：`json:"-"` 跳过（内部字段）、
//     空 tag 回落成 Go 字段名、匿名内嵌结构体拍平；`omitempty` **不是**排除理由
//     （带 omitempty 的字段仍然是 wire 形状的一部分，只是可以缺省）
//   - 失败信息指名道姓到「哪个类型的哪个字段 ↔ openapi 的哪一层 properties」，
//     照着实操就能修，不需要再去反推这个门禁在说什么
//
// # 排除表
//
// 排除必须**逐条写明理由**，并且带**过期检测**：排除项一旦不再需要（schema 已经声明了 /
// Go 已经补上了），这里会红。否则排除表会慢慢变成一个什么都放行的后门。
//
// 主体是**纯静态**核对（不碰数据库、不启服务），跟 `go test ./internal/server/...` 一起跑。
// 唯一一条 HTTP 用例（TestOpenAPIParityZeroCollectionsMarshal 的 ③）用包内已有的
// newConsoleEnv 夹具，因为「空集合给 [] 而不是 null」这条纪律在详情端点是**内联**实现的
// （capabilityArtifact 里三行），除了打真请求没有别的核对办法。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/store"
)

// openapiSpecPath 是 spec 相对本包测试工作目录（internal/server）的路径。
const openapiSpecPath = "../../api/openapi.yaml"

// rawMessageType 是「不透明透传」的标记类型。
//
// `params` / `lifecycle` / manifest 里的 `spec` / `runner` / `config` 都是 `json.RawMessage`：
// 中枢不解释它们（I1），形状由**用户自己**决定。对这类字段只核「存在性」（方向 A/B），
// 不再下钻（下钻等于要求 Go 里有一个它根本没有的结构体）。
var rawMessageType = reflect.TypeOf(json.RawMessage{})

// openapiDoc 是解析后的 spec（只读，多个用例共用一份）。
var (
	openapiLoadOnce sync.Once
	openapiDoc      map[string]any
	openapiLoadErr  error
)

// loadOpenAPIDoc 读并缓存 api/openapi.yaml。
func loadOpenAPIDoc(t *testing.T) map[string]any {
	t.Helper()
	openapiLoadOnce.Do(func() {
		path, err := filepath.Abs(openapiSpecPath)
		if err != nil {
			openapiLoadErr = err
			return
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			openapiLoadErr = err
			return
		}
		openapiLoadErr = yaml.Unmarshal(raw, &openapiDoc)
	})
	if openapiLoadErr != nil {
		t.Fatalf("读不了 %s：%v（本门禁要在 internal/server 目录下跑）", openapiSpecPath, openapiLoadErr)
	}
	if openapiDoc == nil {
		t.Fatalf("%s 是空的", openapiSpecPath)
	}
	return openapiDoc
}

// ---------- 对照表 ----------

// parityPair 是「一个 Go 类型 ↔ openapi 里的一层 schema」。
type parityPair struct {
	// goLabel 是失败信息里的人类可读名（类型名 + 它出现在哪个端点上）。
	goLabel string
	// schemaPath 是 components.schemas 下的点号路径，如 "CapabilitySpecSummary.properties.run"。
	schemaPath string
	// goSample 是该类型的**零值**：类型从 reflect.TypeOf 拿（匿名/无名类型也能这么拿，
	// 不必为了测试去改生产类型名）。
	goSample any
	// onlyGo / onlySchema 是排除表：键是 json 字段名，值**必须**写清理由。
	onlyGo     map[string]string
	onlySchema map[string]string
}

// parityPairs 是本次改造的核心面（Capability Spec v3 §3/§5/§12）。其余 schema 可以后续补：
// 加一行就能覆盖一对，加错了这里立刻会红。
func parityPairs() []parityPair {
	return []parityPair{
		{
			// 任务卡里写作 capabilityCreateRequest，仓库里的真名是 capabilityListRequest
			// （GET/POST/PATCH 共用一个 handler，decode 结构只服务 POST）。
			goLabel:    "capabilityListRequest（POST /capabilities 的请求体）",
			schemaPath: "CapabilityInput",
			goSample:   capabilityListRequest{},
		},
		{
			// 能力行的**读投影**：列表项（CapabilityWithKind）内嵌的就是它。
			// 唯一排除项 `spec` 的理由见下（它是 json:"-" 的内部字段，由端点另给）。
			goLabel:    "store.CapabilityVersion（能力行的读投影）",
			schemaPath: "Capability",
			goSample:   store.CapabilityVersion{},
			onlySchema: map[string]string{
				"spec": `store.CapabilityVersion.Spec 是 json:"-" 的内部字段（能力行这个中间结构不该把 1 MiB` +
					` 的文件正文塞进每条列表响应）。响应里的 spec 由 listCapabilities 用 specSummary()` +
					` 压成列表投影后单独给，那份形状由下面 CapabilitySpecSummary / CapabilitySpec 两对负责 ——` +
					` 所以这里的排除**不是**漏掉核对，而是换了核对对象`,
			},
		},
		{
			goLabel:    "artifact.Spec（能力正文的全文）",
			schemaPath: "CapabilitySpec",
			goSample:   artifact.Spec{},
		},
		{
			goLabel:    "artifact.Run（能力正文里的 spec.run）",
			schemaPath: "CapabilitySpec.properties.run",
			goSample:   artifact.Run{},
		},
		{
			goLabel:    "capabilitySpecView（列表里的 spec 投影）",
			schemaPath: "CapabilitySpecSummary",
			goSample:   capabilitySpecView{},
		},
		{
			goLabel:    "capabilityRunSpecView（列表里的 spec.run 投影）",
			schemaPath: "CapabilitySpecSummary.properties.run",
			goSample:   capabilityRunSpecView{},
		},
		{
			goLabel:    "capabilityFilesSummary（列表里的 files 摘要）",
			schemaPath: "CapabilityFilesSummary",
			goSample:   capabilityFilesSummary{},
		},
		{
			// 任务卡里写作 store.Artifact，那是 store 层的**存储结构**
			// （{namespaceId, capabilityId, digest, spec}，json tag 服务于内部/CLI 的序列化）。
			// 详情端点（GET /capabilities/{id}/artifact）的响应投影是 capabilityArtifactView ——
			// 与 CapabilityArtifact 逐字段对应的是它。拿 store.Artifact 去比一定会全红，
			// 那种红不是漏声明（两个类型说的根本不是同一件事）。
			goLabel:    "capabilityArtifactView（GET /capabilities/{id}/artifact 的响应）",
			schemaPath: "CapabilityArtifact",
			goSample:   capabilityArtifactView{},
		},
		{
			// 批量端点复用了单建的 decode 结构（capabilityListRequest），但**外层**是另一个
			// 形状（capabilityBatchRequest）。两对都列：drift 恰恰容易长在「单建生效、
			// 批量静默丢弃」这类地方（CapabilityLinkInput 少 priority 而 BatchLinkInput 有，
			// 就是这次当场查出来的那一条）。
			goLabel:    "capabilityBatchRequest（POST /capabilities/batch 的请求体）",
			schemaPath: "CapabilityBatchInput",
			goSample:   capabilityBatchRequest{},
		},
		{
			goLabel:    "store.BatchLinkSpec（批量 links[] 的一条边）",
			schemaPath: "BatchLinkInput",
			goSample:   store.BatchLinkSpec{},
		},
		{
			goLabel:    "store.PlaybookManifest（预案清单 v2）",
			schemaPath: "PlaybookManifest",
			goSample:   store.PlaybookManifest{},
		},
		{
			goLabel:    "store.ManifestCapability（manifest 里的一条能力声明）",
			schemaPath: "PlaybookManifestCapability",
			goSample:   store.ManifestCapability{},
		},
		{
			goLabel:    "store.ManifestEdge（manifest 里的一条边）",
			schemaPath: "PlaybookManifestEdge",
			goSample:   store.ManifestEdge{},
		},
		{
			// ManifestTriggerTarget 没有**独立的** schema（它的形状写在
			// PlaybookManifestTrigger.properties.targets.items 里），所以不在这里列一对 ——
			// 它由下面这一对的递归覆盖（targets 是 []ManifestTriggerTarget）。
			goLabel:    "store.ManifestTrigger（manifest 里的一个触发器）",
			schemaPath: "PlaybookManifestTrigger",
			goSample:   store.ManifestTrigger{},
		},
	}
}

// ---------- 核对引擎 ----------

// parityChecker 持有 spec 文档与收集到的问题。
type parityChecker struct {
	schemas map[string]any
	problem []string
}

func (c *parityChecker) addf(format string, args ...any) {
	c.problem = append(c.problem, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

// resolve 把 `$ref` 解开，并把 `allOf` 的 properties 合并进来。
//
// `allOf` 在 OpenAPI 3.0 里是「给 $ref 加兄弟键」的标准写法（本仓库的
// CapabilityWithKind / PlaybookView 都用它），不合并就会读到一份没有 properties 的 schema，
// 那种失败信息会把人指向错误的方向。
func (c *parityChecker) resolve(schema map[string]any) map[string]any {
	for i := 0; i < 8; i++ { // 防 $ref 环（真实 spec 里没有，但死循环比报错贵得多）
		ref, _ := schema["$ref"].(string)
		if ref == "" {
			break
		}
		target, ok := c.schemaByName(strings.TrimPrefix(ref, "#/components/schemas/"))
		if !ok {
			c.addf("openapi 的 $ref 指向了不存在的 schema：%s", ref)
			return schema
		}
		schema = target
	}
	parts, ok := schema["allOf"].([]any)
	if !ok {
		return schema
	}
	merged := map[string]any{}
	props := map[string]any{}
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		for k, v := range c.resolve(pm) {
			if k == "properties" {
				sub, _ := v.(map[string]any)
				for pk, pv := range sub {
					props[pk] = pv
				}
				continue
			}
			merged[k] = v
		}
	}
	if len(props) > 0 {
		merged["properties"] = props
	}
	return merged
}

// schemaByName 取 components.schemas 下的一层。
func (c *parityChecker) schemaByName(name string) (map[string]any, bool) {
	node, ok := c.schemas[name]
	if !ok {
		return nil, false
	}
	m, ok := node.(map[string]any)
	return m, ok
}

// lookup 沿点号路径取 schema（如 "CapabilitySpecSummary.properties.run"）。
func (c *parityChecker) lookup(path string) (map[string]any, bool) {
	parts := strings.Split(path, ".")
	schema, ok := c.schemaByName(parts[0])
	if !ok {
		return nil, false
	}
	for _, p := range parts[1:] {
		node, ok := schema[p]
		if !ok {
			return nil, false
		}
		m, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		schema = m
	}
	return c.resolve(schema), true
}

// comparePair 核对一对（Go 类型, schema）。
func (c *parityChecker) comparePair(pair parityPair) {
	goType := reflect.TypeOf(pair.goSample)
	schema, ok := c.lookup(pair.schemaPath)
	if !ok {
		c.addf("对照表里的 %s 在 openapi 里不存在：components.schemas.%s",
			pair.goLabel, pair.schemaPath)
		return
	}
	c.checkExclusions(pair, goType, schema)
	c.compareStruct(goType, schema, pair.goLabel, "components.schemas."+pair.schemaPath,
		exclusion{onlyGo: pair.onlyGo, onlySchema: pair.onlySchema})
}

// exclusion 是某一层的排除表（只在一对的**顶层**生效，嵌套层一律空）。
type exclusion struct {
	onlyGo     map[string]string
	onlySchema map[string]string
}

// checkExclusions 做**过期检测**：排除项一旦不再需要，这里就红。
//
// 没有这一步，排除表会慢慢变成什么都放行的后门：加一条「这个字段先不比」，
// 三个月后那个字段真的补上了，排除却还在，于是下一次它再漏掉也没人知道。
func (c *parityChecker) checkExclusions(pair parityPair, goType reflect.Type, schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	fields := jsonFields(goType)

	// onlyGo：说的是「Go 有、schema 不该有」。schema 一旦有了，这句判断就过期了。
	for _, name := range sortedKeys(pair.onlyGo) {
		if _, exists := props[name]; exists {
			c.addf("%s：排除表已过期 —— openapi 现在**已经**声明了 `%s`，请把它从 onlyGo 里删掉", pair.goLabel, name)
		}
		if _, exists := fields[name]; !exists {
			c.addf("%s：排除表已过期 —— Go 里已经没有 json tag `%s` 了，请把它从 onlyGo 里删掉", pair.goLabel, name)
		}
	}

	// onlySchema：说的是「schema 有、Go 行的 wire 上没有」。
	// 只接受一种依据：Go 那边有一个同名字段被显式标成 json:"-"（内部字段）。
	for _, name := range sortedKeys(pair.onlySchema) {
		if _, exists := fields[name]; exists {
			c.addf("%s：排除表已过期 —— Go 现在把 `%s` 放上 wire 了，请把它从 onlySchema 里删掉", pair.goLabel, name)
			continue
		}
		if !hasInternalField(goType, name) {
			c.addf("%s：排除表没有依据 —— Go 里既没有 json tag `%s`，也没有被标成 json:\"-\" 的同名字段（理由写的是：%s）",
				pair.goLabel, name, pair.onlySchema[name])
		}
	}
}

// hasInternalField 报告 t 里是否有一个**被显式标成 json:"-"** 的字段，名字（忽略大小写）是 name。
//
// 这是 onlySchema 排除的合法依据：字段存在，但**故意不上 wire**（内部字段）。
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
func (c *parityChecker) compareStruct(goType reflect.Type, schema map[string]any, goLabel, schemaPath string, excl exclusion) {
	schema = c.resolve(schema)
	props, _ := schema["properties"].(map[string]any)
	fields := jsonFields(goType)

	// schema 自己的内部一致性：required 里写了的键必须在 properties 里存在。
	// 缺失时的症状是「文档要求调用方必须发一个它没定义的字段」—— 调用方无从下手。
	for _, req := range stringList(schema["required"]) {
		if _, ok := props[req]; !ok {
			c.addf("%s 的 required 里列了 `%s`，但 properties 里没有它 —— 文档要求发一个它自己没定义的字段", schemaPath, req)
		}
	}

	// 方向 A：Go 有 → schema 必须有
	for _, name := range sortedKeys(fields) {
		prop, ok := props[name]
		if !ok {
			if _, excluded := excl.onlyGo[name]; excluded {
				// 排除表说「schema 不该声明它」：跳过（过期检测已确认这条排除仍然有意义）
				continue
			}
			c.addf("%s.%s 在 API 里存在，但 openapi 的 %s.properties 里没有 —— "+
				"按 schema 生成的客户端永远提交不了它（读响应的人也永远看不到这个字段）",
				goLabel, name, schemaPath)
			continue
		}
		propSchema, _ := prop.(map[string]any)
		c.compareValue(fields[name], propSchema, goLabel+"."+name, schemaPath+".properties."+name)
	}

	// 方向 B：schema 有 → Go 必须有
	for _, name := range sortedKeys(props) {
		if _, ok := fields[name]; ok {
			continue
		}
		if _, excluded := excl.onlySchema[name]; excluded {
			// 排除表说「这个键由别处提供 / Go 那边是内部字段」：跳过（过期检测已确认）
			continue
		}
		c.addf("openapi 的 %s.properties.%s 声明了这个字段，但 Go 的 %s 里没有对应的 json tag —— "+
			"文档在骗人：照它写的客户端会去读一个永远不会出现的键",
			schemaPath, name, goLabel)
	}
}

// compareValue 按 Go 的类型种类继续下钻。
func (c *parityChecker) compareValue(goType reflect.Type, schema map[string]any, goPath, schemaPath string) {
	if schema == nil {
		// 方向 A 已经报过「schema 里没有这个字段」，这里再报一次只是噪声。
		return
	}
	for goType.Kind() == reflect.Ptr {
		goType = goType.Elem()
	}
	if goType == rawMessageType {
		// 不透明透传（params / lifecycle / 各类 manifest 自由形状）：只核存在性。
		return
	}
	switch goType.Kind() {
	case reflect.Struct:
		c.checkType(schema, "object", goPath, schemaPath)
		c.compareStruct(goType, schema, goPath, schemaPath, exclusion{})
	case reflect.Map:
		c.checkType(schema, "object", goPath, schemaPath)
		if _, fixed := schema["properties"]; fixed {
			c.addf("%s：Go 那边是 map（键由调用方决定），openapi 却写死了 properties —— "+
				"文档声明了一批并不存在固定字段", goPath)
		}
		if ap, ok := schema["additionalProperties"].(map[string]any); ok {
			c.compareValue(goType.Elem(), ap, goPath+"{}", schemaPath+".additionalProperties")
		}
	case reflect.Slice:
		c.checkType(schema, "array", goPath, schemaPath)
		if items, ok := schema["items"].(map[string]any); ok {
			c.compareValue(goType.Elem(), items, goPath+"[]", schemaPath+".items")
		} else if _, typed := schema["type"]; typed {
			c.addf("%s：openapi 说是 array 却没有 items —— 元素形状没有声明", goPath)
		}
	case reflect.String:
		c.checkType(schema, "string", goPath, schemaPath)
	case reflect.Bool:
		c.checkType(schema, "boolean", goPath, schemaPath)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		c.checkType(schema, "integer", goPath, schemaPath)
	case reflect.Float32, reflect.Float64:
		c.checkType(schema, "number", goPath, schemaPath)
	case reflect.Interface:
		// any / interface{}：形状由文档单方面描述，无从核对
	}
}

// checkType 核对 `type`。文档没写 type 时不判（本仓库有几处宽松写法只写
// deprecated/description，如 PlaybookManifestCapability.properties.runner）——
// 那些字段的存在性仍由方向 A/B 保证，漏声明照样会红。
func (c *parityChecker) checkType(schema map[string]any, want, goPath, schemaPath string) {
	got, declared := schema["type"].(string)
	if !declared || got == want {
		return
	}
	c.addf("%s：openapi 声明 type=%s，Go 那边是 %s —— 两边说的不是同一种值", goPath, got, want)
}

// ---------- 用例 ①：双向字段核对 ----------

// TestOpenAPIParityStructFields 是 P6 的主体：逐对核对 Go 结构体与 openapi schema。
//
// 它红的时候一次把所有问题都列出来（不是遇到第一条就 Fatal）：
// 补声明通常是一次补好几条，逐条跑会变成「改一条跑一次」的磨洋工。
func TestOpenAPIParityStructFields(t *testing.T) {
	doc := loadOpenAPIDoc(t)
	schemas, ok := nestedMap(doc, "components", "schemas")
	if !ok {
		t.Fatal("openapi 里没有 components.schemas —— 本门禁的前提没了，先确认 spec 结构")
	}
	c := &parityChecker{schemas: schemas}

	pairs := parityPairs()
	// 下限是**削表守卫**：删掉一对（比如「这个类型先不管」）就是把门禁悄悄放宽，而那正是
	// 本门禁要防的事。加类型随时；减必须同时改这里并写清理由。
	if len(pairs) < 14 {
		t.Fatalf("对照表只有 %d 对，被削过了（P6 要求覆盖 §3 的正文形状 + §5 的端点投影 + §12 的 manifest）", len(pairs))
	}
	for _, pair := range pairs {
		if pair.goSample == nil {
			t.Fatalf("对照表里 %s 的 goSample 是 nil", pair.goLabel)
		}
		c.comparePair(pair)
	}

	if len(c.problem) > 0 {
		t.Fatalf("openapi 与 Go 结构体不一致（%d 条）：\n  - %s",
			len(c.problem), strings.Join(c.problem, "\n  - "))
	}
}

// ---------- 用例 ②：数组纪律 ----------

// TestOpenAPIParityArraysNeverNull 钉住不变量 I9 的 **schema 一半**：空集合给 `[]` 而不是 `null`。
//
// 两条：
//   - 凡是数组（有 items），就必须 `type: array`；反过来 `type: array` 就必须有 items
//   - 数组**不得**声明 `nullable` —— 那等于允许 `null`，而 Go 侧（I9）保证给 `[]`。
//     两者同时存在时，按 schema 生成的客户端会为一个永远不会到来的 null 写判空分支。
func TestOpenAPIParityArraysNeverNull(t *testing.T) {
	doc := loadOpenAPIDoc(t)
	var problems []string
	walkOpenAPISchemas(doc, "openapi", func(path string, node map[string]any) {
		_, hasItems := node["items"]
		typ, _ := node["type"].(string)
		if hasItems && typ != "array" {
			problems = append(problems, fmt.Sprintf(
				"%s 有 items 但 type=%q（必须是 array）", path, typ))
		}
		if typ == "array" && !hasItems {
			problems = append(problems, fmt.Sprintf("%s 是 array 但没有 items（元素形状没声明）", path))
		}
		if typ == "array" {
			if nullable, _ := node["nullable"].(bool); nullable {
				problems = append(problems, fmt.Sprintf(
					"%s 是 array 且声明了 nullable: true —— 数组的零值是 [] 不是 null（不变量 I9）", path))
			}
		}
	})
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("openapi 的数组声明违反了 I9（%d 条）：\n  - %s",
			len(problems), strings.Join(problems, "\n  - "))
	}
}

// ---------- 用例 ③：零值序列化 ----------

// TestOpenAPIParityZeroCollectionsMarshal 钉住 I9 的 **Go 一半**：端点输出的数组与 map
// 字段，空集合序列化成 `[]` / `{}`，不是 `null`。
//
// 分两层：
//
//   - ① ② 直接核对两个**端点真正调用的归一化函数**（specSummary / fullSpec）。核对函数而
//     不是结构体零值：零值本来就是 nil，重要的是「交给 JSON 之前有没有归一」
//   - ③ 走**真 HTTP**（建一份没有 requires / env / outputs 的能力，再读详情）：
//     详情端点顶层的 files / requires / env 是 handler 里的内联归一，那三行不是函数，
//     没法直接调；而它们一旦没归一，响应就是 `null`，与 schema 的 description 矛盾。
func TestOpenAPIParityZeroCollectionsMarshal(t *testing.T) {
	// ① 列表投影：outputs 空 → []；requires 空 → 省略（schema 的 description 就是这么写的）
	list := marshalJSON(t, specSummary(artifact.Spec{}))
	if got := jsonFieldOf(t, list, "outputs"); got != "[]" {
		t.Fatalf("specSummary(零值).outputs = %s，want []（I9：空集合给 [] 不是 null）", got)
	}
	if strings.Contains(list, ":null") {
		t.Fatalf("列表投影里出现了 null：%s", list)
	}

	// ② 全文投影：outputs 空 → []，run.files 空 → {}（详情端点给的就是这份）
	full := marshalJSON(t, fullSpec(artifact.Spec{}))
	if got := jsonFieldOf(t, full, "outputs"); got != "[]" {
		t.Fatalf("fullSpec(零值).outputs = %s，want []（I9）", got)
	}
	run, ok := decodeObject(t, full)["run"].(map[string]any)
	if !ok {
		t.Fatalf("fullSpec(零值) 的 run 不是对象：%s", full)
	}
	if got, ok := run["files"].(map[string]any); !ok || len(got) != 0 {
		t.Fatalf("fullSpec(零值).run.files = %v，want {}（I9：空 map 给 {} 不是 null）", run["files"])
	}
	if strings.Contains(full, ":null") {
		t.Fatalf("全文投影里出现了 null：%s", full)
	}

	// ③ 详情端点的**真响应**：requires / env / spec.outputs 都必须是空集合而不是 null。
	//
	// 用类型断言判而不是比字符串：`null` 解出来是 nil（断言失败），`[]` / `{}` 解出来是
	// 长度为 0 的集合（断言成功）—— 这一行同时钉住「不是 null」与「不发明默认值」。
	env := newConsoleEnv(t)
	created := env.do(t, http.MethodPost, env.withNS("/v1/capabilities"), map[string]any{
		"name":    "parity.empty.collections",
		"outputs": []string{},
		"run": map[string]any{
			"entrypoint": "run.sh",
			"files":      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
		},
	})
	if created.Code != http.StatusOK {
		t.Fatalf("建能力失败：%d %s", created.Code, created.Body.String())
	}
	capObj, _ := decode(t, created)["capability"].(map[string]any)
	capID, _ := capObj["id"].(string)
	if capID == "" {
		t.Fatalf("建能力响应里没有 capability.id：%s", created.Body.String())
	}
	detail := env.do(t, http.MethodGet, env.withNS("/v1/capabilities/"+capID+"/artifact"), nil)
	if detail.Code != http.StatusOK {
		t.Fatalf("详情 status = %d, body=%s", detail.Code, detail.Body.String())
	}
	body := detail.Body.String()
	m := decode(t, detail)
	if req, ok := m["requires"].([]any); !ok || len(req) != 0 {
		t.Fatalf("详情.requires = %v，want []（I9：空集合给 [] 不是 null）", m["requires"])
	}
	if runEnv, ok := m["env"].(map[string]any); !ok || len(runEnv) != 0 {
		t.Fatalf("详情.env = %v，want {}（I9：空 map 给 {} 不是 null）", m["env"])
	}
	detailSpec, _ := m["spec"].(map[string]any)
	if out, ok := detailSpec["outputs"].([]any); !ok || len(out) != 0 {
		t.Fatalf("详情.spec.outputs = %v，want []（I9）", detailSpec["outputs"])
	}
	if strings.Contains(body, ":null") {
		t.Fatalf("详情响应里出现了 null（schema 里没有任何字段声明 nullable）：%s", body)
	}
}

// ---------- 小工具 ----------

// jsonFields 按 encoding/json 的规则把一个结构体摊成 wire 字段表。
//
//   - `json:"-"` → 跳过（显式不上 wire 的内部字段）
//   - 没有 tag → 用 Go 字段名（encoding/json 的回落规则）
//   - 匿名内嵌结构体 → 拍平到外层（encoding/json 的 promoted fields）
//   - `omitempty` → **不影响**：可以缺省不等于可以不声明
//
// 返回 wire 字段名 → 字段类型。
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
		if f.PkgPath != "" { // 未导出：encoding/json 根本看不到
			continue
		}
		tag := f.Tag.Get("json")
		name := strings.Split(tag, ",")[0]
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
			continue // 同名字段：encoding/json 取最外层（先到先得即最外层）
		}
		out[name] = f.Type
	}
	return out
}

// walkOpenAPISchemas 深度遍历文档，对**每一个 schema 节点**回调（path → node）。
//
// 不能「遍历每一层 map」：`properties` 的**容器本身**不是 schema，而它完全可能有一个
// 叫 `items` 的键（PlaybookDetail.properties.items 就是——体检项数组）。把容器当 schema
// 判会得到一个假的「有 items 但 type 不是 array」。
//
// 同理，`example` / `enum` / `default` 里是**用户数据**，不是 schema 关键字。
func walkOpenAPISchemas(node any, path string, visit func(path string, node map[string]any)) {
	switch v := node.(type) {
	case map[string]any:
		visit(path, v)
		for _, key := range sortedKeys(v) {
			switch key {
			case "example", "examples", "enum", "default":
				continue
			case "properties":
				props, ok := v[key].(map[string]any)
				if !ok {
					continue
				}
				for _, name := range sortedKeys(props) {
					walkOpenAPISchemas(props[name], path+".properties."+name, visit)
				}
			default:
				walkOpenAPISchemas(v[key], path+"."+key, visit)
			}
		}
	case []any:
		for i, item := range v {
			walkOpenAPISchemas(item, fmt.Sprintf("%s[%d]", path, i), visit)
		}
	}
}

// nestedMap 沿键路径取嵌套 map。
func nestedMap(root map[string]any, keys ...string) (map[string]any, bool) {
	node := any(root)
	for _, k := range keys {
		m, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		node, ok = m[k]
		if !ok {
			return nil, false
		}
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

// sortedKeys 让所有失败信息的顺序稳定（map 遍历顺序是随机的，不稳定就不好对照 diff）。
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// marshalJSON 把值序列化成字符串（失败信息里能直接看到形状）。
func marshalJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	return string(b)
}

// decodeObject 把 JSON 对象解成 map（用例里要看嵌套字段）。
func decodeObject(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("反序列化失败：%v（raw=%s）", err, raw)
	}
	return out
}

// jsonFieldOf 取顶层字段的**原始 JSON 文本**（用来区分 `[]` 与 `null`）。
func jsonFieldOf(t *testing.T, raw, key string) string {
	t.Helper()
	start := strings.Index(raw, `"`+key+`":`)
	if start < 0 {
		t.Fatalf("JSON 里没有 %q：%s", key, raw)
	}
	rest := raw[start+len(key)+3:]
	end := strings.IndexAny(rest, ",}")
	if end < 0 {
		end = len(rest)
	}
	return strings.TrimSpace(rest[:end])
}
