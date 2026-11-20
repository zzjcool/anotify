package server

// resource_namespace_yaml.go 是 Namespace 与 System 文档的 YAML 面
// （IA 重构的 ③ / ④，docs/design/node-namespace-ia.md §2.2 / §3.1）：
//
//	GET  /v1/namespaces/{name}/yaml  规范化 Namespace 文档文本（{"yaml": "…"}）
//	PUT  /v1/namespaces/{name}/yaml  严格解析 → PutNamespaceConfigByName（CAS）
//	GET  /v1/system/yaml             规范化 SystemConfig 文档文本（{"yaml": "…"}，只读）
//
// 四条边界（改之前先读）：
//
//  1. **YAML 是唯一编辑面**：命名空间 vars 的写入口就是这份文档本身
//     （`PUT /v1/namespaces/{name}/yaml`，CAS = `metadata.resourceVersion`，即命名空间
//     vars 的 CAS 令牌 —— 与 `GET …/config` 给出的是同一个值）。
//     `PUT /v1/namespaces/{name}/config` 作为同一份事实的 JSON wrapper 保留，两条入口
//     共用**同一个** store 写路径（`PutNamespaceConfigByName`），不并存两套语义。
//     系统面（保留期）仍是只读：写入口是 `PUT /v1/system/config`，没有 `PUT …/yaml`。
//  2. **YAML 就是资源本身**：apiVersion / kind / metadata / spec，字段集分别与
//     Namespace（slug + 展示名 + vars）和 system_config（三条保留期）逐字相同。
//     `status`（派生计数）、`metadata.uid`（不透明内部身份）**不出现**：它们对
//     「看配置」没有信息量，却会让人以为能改。uid 正是 Node YAML 读面刻意省掉的
//     那个字段（写端白名单只有 name / resourceVersion），这里保持同一口径。
//  3. **解析只有一份**：本文件不写任何宽松解析器。读面在返回前把**自己生成的文本**
//     过一遍 `internal/yamldoc.Decode`（同样的严格形状），坏文本按 500 具名失败 ——
//     否则「服务端吐出来的 YAML 客户端贴不回去」会变成一条没人发现的回归。
//  4. **系统面是管理员面**：`/v1/system/*` 与 mock / 页面文案一致要求 role=admin
//     （非 admin 403 `forbidden`），不是「登录即可读」。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/yamldoc"
	"gopkg.in/yaml.v3"
)

const (
	// namespaceYAMLHeader 是 Namespace 文档的 apiVersion（与 api/resource.yaml 一致）。
	namespaceYAMLHeader = apiVersionV1
	// systemYAMLKind 是系统配置文档的 kind。
	//
	// 用 `SystemConfig` 而不是编一个 `System` 资源：system_config 是**单行配置表**
	// （没有 uid、没有 cluster 对象语义），它的 JSON wrapper 也叫 SystemConfig
	// （`decodeConfigWrite(r, "SystemConfig", …)`）。文档 kind 与 wire 上的 kind 同一份。
	systemYAMLKind = "SystemConfig"
)

// namespaceYAMLDoc 是 GET 读面的序列化形状（字段顺序 = 文档顺序）。
type namespaceYAMLDoc struct {
	APIVersion string            `yaml:"apiVersion" json:"apiVersion"`
	Kind       string            `yaml:"kind" json:"kind"`
	Metadata   namespaceYAMLMeta `yaml:"metadata" json:"metadata"`
	Spec       namespaceYAMLSpec `yaml:"spec" json:"spec"`
}

// namespaceYAMLMeta 是文档的 metadata：名字（slug）+ vars 写入的 CAS 令牌。
//
// 刻意**不带 uid / creationTimestamp**（理由见文件头第 2 条）。resourceVersion 就是
// **命名空间 vars 写入的 CAS 令牌**：store 的 `PutNamespaceConfigByName` 拿它跟
// `namespaces.resource_version` 逐字比（`checkResourceVersion`），而
// `GetNamespaceConfig` 把同一列投影成 `cfg.ResourceVersion` —— 所以
// `GET /v1/namespaces/{name}/config` 与 `GET …/yaml` 给出的是**同一个值**，
// 文档里这一行可以拿去写任一条入口（同一个事实的两种序列化，不是两个计数器）。
//
// displayName 也不进文档：它是**用户数据**（可以是任意语言），而这份文本是复制走
// 的运维工件 —— 页面侧对用户数据只有「只含文本的 SPAN + data-raw-content」这一种
// 豁免，`<pre>` 里的多语言文本会让 en/es 的 i18n 残留扫描把用户的展示名当成漏翻。
// 需要 displayName 时读 JSON 读面（它在 metadata.displayName 里）。
type namespaceYAMLMeta struct {
	Name            string `yaml:"name" json:"name"`
	ResourceVersion string `yaml:"resourceVersion,omitempty" json:"resourceVersion,omitempty"`
}

// namespaceYAMLSpec 是文档的 spec：命名空间默认 vars。
//
// 用 `*yaml.Node` 而不是 `map[string]string`：样例要求 vars 内部带一行**解释性注释**
// （「命名空间默认值；节点可覆盖」）。注释是文档的一部分，而 encoding/json→YAML 的
// 纯 map 序列化表达不了它。空 vars 显式写成 `vars: {}`（不是省略），让「没有默认值」
// 与「字段不存在」在文本上可区分。
//
// 注释用**英文**：这份文本是服务端按事实生成的、与 locale 无关（页面不翻译它），
// 而 en/es 的 i18n 残留扫描（f3 的 chrome residue）会把可见 DOM 里的中文当成漏翻。
// 中文解释属于页面文案（`namespace.v4.yaml_hint` / `effective_hint`）。
// 同样的理由适用于文件头的两段 prelude。
type namespaceYAMLSpec struct {
	Vars *yaml.Node `yaml:"vars" json:"vars"`
}

// namespaceYAMLShapeSpec 是严格自校验用的 spec 形状（与上面的编码类型同字段集）。
//
// 为什么另外声明一个类型：编码侧需要 `yaml.Node` 才能写注释，而严格解析器是按
// **Go 类型**逐键校验的 —— 把 `yaml.Node` 交给它是把实现的内部结构当成契约
// （yaml.Node 自己有 Kind/Content/HeadComment 等字段）。这里只描述契约：一个
// `vars: {名字→字符串}` 映射。两个字段名必须同时改，否则自校验失败。
type namespaceYAMLShapeSpec struct {
	Vars map[string]string `json:"vars"`
}

// namespaceYAMLShape 是读面自校验（以及测试）使用的严格形状。
var namespaceYAMLShape = yamldoc.Shape{
	Root: map[string]reflect.Type{
		"apiVersion": reflect.TypeOf(""),
		"kind":       reflect.TypeOf(""),
		"metadata":   reflect.TypeOf(namespaceYAMLMeta{}),
		"spec":       reflect.TypeOf(namespaceYAMLShapeSpec{}),
	},
	Meta: map[string]reflect.Type{
		"name":            reflect.TypeOf(""),
		"resourceVersion": reflect.TypeOf(""),
	},
	Required: []string{"apiVersion", "kind", "metadata.name"},
}

// namespaceYAMLSpecPatch 是写请求的 spec：只有 vars（Namespace 的 spec 是空对象；vars 是配置子资源）。
//
// 字段类型是 `map[string]string` 而不是指针：YAML 是**整份替换**（不是 merge-patch），
// 所以「没写 vars」与「写了 {}」必须由调用方分开表达 —— nil 是前者（400，见
// putNamespaceYAML），空 map 是后者（清空）。这与 JSON 写入口 `PUT …/config` 的判据一致。
type namespaceYAMLSpecPatch struct {
	Vars map[string]string `json:"vars"`
}

// namespaceYAMLWrite 是严格解码后的写请求（信封层 + spec），与 nodeYAMLWrite 同构。
//
// Metadata 用 RawMessage：它要先过 decodeObjectMeta 的白名单（name / resourceVersion），
// 直接解进 objectMeta 会把 uid / creationTimestamp 这些只读键静默吞掉。
type namespaceYAMLWrite struct {
	APIVersion *string                `json:"apiVersion"`
	Kind       *string                `json:"kind"`
	Metadata   json.RawMessage        `json:"metadata"`
	Spec       namespaceYAMLSpecPatch `json:"spec"`
}

// systemYAMLDoc 是 GET /v1/system/yaml 的序列化形状。
//
// metadata / retention 用**文档专用的**结构而不是复用 wire 的 configMeta / retentionDoc：
// 后两者只有 json tag（yaml.v3 会把字段名全小写成 `resourceversion`），而严格解析器
// 是按 json tag 校验键名的 —— 复用它们会让自校验报「未知字段 resourceversion」。
type systemYAMLDoc struct {
	APIVersion string         `yaml:"apiVersion" json:"apiVersion"`
	Kind       string         `yaml:"kind" json:"kind"`
	Metadata   systemYAMLMeta `yaml:"metadata" json:"metadata"`
	Spec       systemYAMLSpec `yaml:"spec" json:"spec"`
}

// systemYAMLMeta 是系统配置文档的 metadata（只有 CAS 令牌，没有 uid）。
type systemYAMLMeta struct {
	ResourceVersion string `yaml:"resourceVersion" json:"resourceVersion"`
}

// systemYAMLSpec 是系统配置的 spec：三条保留期。
type systemYAMLSpec struct {
	Retention systemYAMLRetention `yaml:"retention" json:"retention"`
}

// systemYAMLRetention 是保留期的文档形状（字段名与 JSON wrapper 逐字相同）。
type systemYAMLRetention struct {
	EventTTLSec  int64 `yaml:"eventTtlSec" json:"eventTtlSec"`
	TaskTTLSec   int64 `yaml:"taskTtlSec" json:"taskTtlSec"`
	ResultTTLSec int64 `yaml:"resultTtlSec" json:"resultTtlSec"`
}

// systemYAMLShape 是系统文档的严格形状（字段集与 retentionDoc 同一份）。
var systemYAMLShape = yamldoc.Shape{
	Root: map[string]reflect.Type{
		"apiVersion": reflect.TypeOf(""),
		"kind":       reflect.TypeOf(""),
		"metadata":   reflect.TypeOf(systemYAMLMeta{}),
		"spec":       reflect.TypeOf(systemYAMLSpec{}),
	},
	Meta: map[string]reflect.Type{
		"resourceVersion": reflect.TypeOf(""),
	},
	Required: []string{"apiVersion", "kind", "metadata.resourceVersion"},
}

// namespaceYAMLPrelude / systemYAMLPrelude 是文档头部的声明。
//
// 为什么写在文本里而不是只写页面上：YAML 会被复制走（贴进 wiki、贴进 PR、贴给同事），
// 离开页面之后「谁是可写的、写入入口在哪」这条信息必须还在文档自己身上。
const (
	namespaceYAMLPrelude = `# Namespace document. spec.vars are namespace defaults; a node may override them
# (merge order: local > node > namespace). Edit this document with
# PUT /v1/namespaces/{name}/yaml (metadata.resourceVersion = the CAS token of the
# namespace vars).
`
	systemYAMLPrelude = `# SystemConfig document (read-only). retention holds three TTLs in seconds; 0 means
# keep forever. Writes go through PUT /v1/system/config (CAS) - this endpoint
# deliberately has no PUT.
`
)

// getNamespaceYAML 处理 GET /v1/namespaces/{name}/yaml。
func (a *namespaceAPI) getNamespaceYAML(w http.ResponseWriter, r *http.Request) {
	ns, err := a.resolve(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	cfg, err := a.d.DB.GetNamespaceConfig(r.Context(), ns.ID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	text, err := namespaceYAMLText(ns, cfg)
	if err != nil {
		// 读面没有调用方输入：文本坏 = 服务端事实坏 → 500（与 Node YAML 读面同判据）。
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, yamlDoc{YAML: string(text)})
}

// putNamespaceYAML 处理 PUT /v1/namespaces/{name}/yaml：严格解析一份 Namespace 文档并整份替换 spec.vars。
//
// 顺序与 nodeAPI.yamlPut 一一对应（文档层 → 只读字段 → 严格 JSON → 身份 → 载荷 → CAS →
// store → 回读），只把最后一步换成 `PutNamespaceConfigByName`（`PUT …/config` 用的就是它）——
// 所以 YAML 与 JSON 两条写入口对「缺 vars」「旧令牌」「非法 vars」的解释不可能分叉。
// 任何一步都不静默忽略：静默忽略会让「我改了 YAML」变成一句没有根据的话。
func (a *namespaceAPI) putNamespaceYAML(w http.ResponseWriter, r *http.Request) {
	// 先 resolve：401（未登录）/ 404（不存在 / 无权）要在读 body 之前就落定，
	// 并且后面的身份校验比的是行上的 slug（路径可以写成 id）。
	ns, err := a.resolve(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	text, err := decodeYAMLRequestBody(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	res, err := yamldoc.Decode(text, writeDocShape(r, reflect.TypeOf(namespaceYAMLSpecPatch{}), "name", "resourceVersion"))
	if err != nil {
		writeYAMLError(w, err)
		return
	}
	// status 是派生的只读面：文档里出现它就是一次「以为能改」的误解，必须具名拒绝
	// （与 PUT …/config、Node 写入口同一个 reason，不在这里发明第二个原因码）。
	if namespaceYAMLHasStatusKey(res.JSON) {
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonStatusNotWritable, "status is read-only"))
		return
	}
	var in namespaceYAMLWrite
	dec := json.NewDecoder(bytes.NewReader(res.JSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeAPIError(w, unknownFieldError(err))
		return
	}
	if in.APIVersion != nil && strings.TrimSpace(*in.APIVersion) != namespaceYAMLHeader {
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonValueInvalid,
			"apiVersion 必须是 "+namespaceYAMLHeader))
		return
	}
	if in.Kind != nil && strings.TrimSpace(*in.Kind) != kindNamespace {
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonValueInvalid, "kind 必须是 "+kindNamespace))
		return
	}
	meta := objectMeta{}
	if !isEmptyJSON(in.Metadata) {
		if meta, err = decodeObjectMeta(in.Metadata, map[string]bool{"name": true, "resourceVersion": true}); err != nil {
			writeAPIError(w, err)
			return
		}
	}
	// metadata.name 是身份（URL + 令牌作用域）：与路径不符就 400，而不是静默按路径改。
	// 路径可以写成 id（resolve 两种都认），所以比的是行上的 slug，不是路径原文。
	if meta.Name != "" && meta.Name != ns.Slug {
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonValueInvalid,
			fmt.Sprintf("metadata.name %q does not match path namespace %q", meta.Name, ns.Slug)))
		return
	}
	if in.Spec.Vars == nil {
		// PUT 是**整份替换**（不是 merge）：`spec.vars` 缺席会被读成「清空所有 vars」，
		// 与「忘了写」无法区分。文案与 PUT …/config 同一口径，只把键名换成本文档的路径。
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonValueInvalid,
			"spec.vars is required (PUT replaces the whole namespace vars map; send {} to clear)"))
		return
	}
	token, err := casTokenFromMeta(meta)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if _, err := a.d.DB.PutNamespaceConfigByName(r.Context(), ns.Slug, in.Spec.Vars, token); err != nil {
		writeAPIError(w, err) // 非法 vars 400 value_invalid / 旧令牌 409 + details.current / 不存在 404
		return
	}
	// 回读一次：文档的 CAS 令牌与 vars 都来自**写后**的事实。
	// 令牌就是 `namespace_configs` 的写入口比的那一列（`namespaces.resource_version`，
	// `GetNamespaceConfig` 会把它投影到 `cfg.ResourceVersion`）—— 用入参里的旧值拼
	// 响应等于把一个刚过期的令牌回给客户端，「取 → 贴回」的下一跳立刻 409。
	cfg, err := a.d.DB.GetNamespaceConfig(r.Context(), ns.ID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	out, err := namespaceYAMLText(ns, cfg)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, yamlDoc{YAML: string(out)})
}

// namespaceYAMLHasStatusKey 在写请求里找顶层 `status`（照 nodeHasStatusKey 的写法）。
//
// 只看顶层键：spec 深处的同名键不属于写语义（那里会被 yamldoc 按未知字段拒掉），
// 而顶层 `status` 是派生只读面 —— 报文里出现它就必须报 status_not_writable，
// 而不是「未知字段」（那会把调用方引向错误的心智模型）。
func namespaceYAMLHasStatusKey(raw []byte) bool {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return false // 坏 JSON 交给 strict 解码报可理解的 400
	}
	_, ok := top["status"]
	return ok
}

// getSystemYAML 处理 GET /v1/system/yaml（管理员面）。
func (a *namespaceAPI) getSystemYAML(w http.ResponseWriter, r *http.Request) {
	if err := requireAdminUser(r.Context(), a.d.DB); err != nil {
		writeAPIError(w, err)
		return
	}
	cfg, err := a.d.DB.GetSystemConfig(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	text, err := systemYAMLText(cfg)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, yamlDoc{YAML: string(text)})
}

// requireAdminUser 是 `/v1/system/*` 的角色闸门：登录（401）→ 存在（401）→ 未禁用（401）
// → role=admin（403）。
//
// 为什么不直接复用 `auth.AdminMiddleware`：那个中间件挂在 mux 的 `/v1/admin/*` 上，
// 而这里返回的错误要走 v4 的 Status 体（reason=forbidden），前端才能把「无权限」与
// 「网络坏了」区分开（AdminMiddleware 回的是明文 `{"error":…}`）。判据与中间件逐条一致：
// 未登录/用户不存在/已禁用都是 401（视同未认证），只有角色不足是 403。
func requireAdminUser(ctx context.Context, db *store.DB) error {
	uid, err := requireUser(ctx)
	if err != nil {
		return err
	}
	u, err := db.GetUserByID(uid)
	if err != nil || u == nil {
		return store.ErrAuthRequired
	}
	if u.Disabled {
		return store.ErrAuthRequired
	}
	if u.Role != store.RoleAdmin {
		return errStatus(http.StatusForbidden, reasonForbidden, "administrator required")
	}
	return nil
}

// namespaceYAMLText 把命名空间行 + 配置行序列化成规范化 Namespace 文档。
func namespaceYAMLText(ns *store.Namespace, cfg *store.NamespaceConfig) ([]byte, error) {
	if ns == nil {
		return nil, fmt.Errorf("namespace yaml: nil namespace")
	}
	if cfg == nil {
		return nil, fmt.Errorf("namespace yaml: nil config")
	}
	vars := cfg.Vars
	if vars == nil {
		vars = map[string]string{}
	}
	varsNode := &yaml.Node{}
	if err := varsNode.Encode(vars); err != nil {
		return nil, fmt.Errorf("namespace yaml: encode vars: %w", err)
	}
	if len(vars) == 0 {
		// 空 map 用流式 `{}`；没有条目就没有地方挂注释（yaml.v3 会把 HeadComment 丢掉）。
		varsNode.Style = yaml.FlowStyle
	} else {
		varsNode.HeadComment = "namespace defaults; a node may override them (see the per-node effective values table)"
	}
	doc := namespaceYAMLDoc{
		APIVersion: namespaceYAMLHeader,
		Kind:       kindNamespace,
		Metadata: namespaceYAMLMeta{
			Name:            ns.Slug,
			ResourceVersion: store.ResourceVersion(ns.ID, cfg.ResourceVersion),
		},
		Spec: namespaceYAMLSpec{Vars: varsNode},
	}
	return enqueueYAMLDoc(namespaceYAMLPrelude, doc, namespaceYAMLShape)
}

// systemYAMLText 把系统配置行序列化成规范化 SystemConfig 文档。
func systemYAMLText(cfg *store.SystemConfig) ([]byte, error) {
	if cfg == nil {
		return nil, fmt.Errorf("system yaml: nil config")
	}
	doc := systemYAMLDoc{
		APIVersion: namespaceYAMLHeader,
		Kind:       systemYAMLKind,
		Metadata:   systemYAMLMeta{ResourceVersion: systemResourceVersion(cfg.ResourceVersion)},
		Spec: systemYAMLSpec{Retention: systemYAMLRetention{
			EventTTLSec:  cfg.RetentionEventTTLSec,
			TaskTTLSec:   cfg.RetentionTaskTTLSec,
			ResultTTLSec: cfg.RetentionResultTTLSec,
		}},
	}
	return enqueueYAMLDoc(systemYAMLPrelude, doc, systemYAMLShape)
}

// enqueueYAMLDoc 把「只读声明 + 文档」编码成文本，并用严格解析器**校验自己**。
//
// 2 空格缩进与 Node YAML 读面一致；键序由 struct 声明固定、map 键由 yaml.v3 排序，
// 所以同一份事实永远序列化成同一份文本（两次 GET 字节一致，页面不会每次刷新都跳）。
// 自校验失败是**服务端事实损坏**（500），不是调用方的输入错误。
func enqueueYAMLDoc(prelude string, doc any, shape yamldoc.Shape) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(prelude)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("yaml: marshal: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("yaml: marshal: %w", err)
	}
	text := buf.Bytes()
	if _, err := yamldoc.Decode(text, shape); err != nil {
		return nil, fmt.Errorf("yaml: 自校验失败: %w", err)
	}
	return text, nil
}
