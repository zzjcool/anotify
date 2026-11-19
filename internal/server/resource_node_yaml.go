package server

// resource_node_yaml.go 是 Node 文档的 YAML 读/写面（IA 重构的 ②，
// docs/design/node-namespace-ia.md §2.1 / §3）：
//
//	GET /v1/nodes/{name}/yaml   规范化 Node 文档文本（{"yaml": "…"}）
//	PUT /v1/nodes/{name}/yaml   严格解析 → 与 PATCH /v1/nodes/{name} 同一条 store 路径
//
// 三条边界（与 docs/design/v4/api-conventions.md §7.4 对齐，改之前先读）：
//
//  1. **YAML 就是 Node 文档本身**：apiVersion / kind / metadata / spec，字段集与
//     `NodeSpec` 逐字相同（namespaces / vars / limits / tags）。不新增字段、不改语义，
//     `status` **不出现**在文档里 —— 它是派生只读面，放进可编辑文档只会让人以为能改它。
//  2. **解析只有一份**：写路径复用 `internal/yamldoc.Decode`（未知字段 / 类型 / 必填 /
//     重复键 / 多文档 / 锚点别名全部拒绝，带行号与字段路径）。本文件不写任何宽松解析器，
//     也不自己校验 limits/vars 的形状 —— 那是 store（`putNodeConfigOn` 里的
//     `ValidateConfigNamespaces` / `ValidateVars` / `ValidateLimits`）的唯一职责。
//  3. **CAS 与 JSON 写入口同一份**：`metadata.resourceVersion` 是令牌，`UpdateNode`
//     是唯一 store 入口（Node.spec 与 config 子资源共享同一个底层版本）。
//
// `anode.yaml` 的私有字段（server / token / listen / envPassthrough）**不在**这里：
// §7.4 明确规定它们不进 Node spec、不上报（`internal/protocol/config_frames.go:15`），
// 中枢拿不到就只能编造。本机文件保持既有形状，公共字段（name / namespaces / vars / limits）
// 逐字同名。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/yamldoc"
	"gopkg.in/yaml.v3"
)

// nodeYAMLHeader 是文档的 apiVersion / kind（与 api/resource.yaml 的 Node 一致）。
const nodeYAMLHeader = apiVersionV1

// nodeYAMLDoc 是 GET 读面的序列化形状。
//
// 字段顺序就是文档顺序（yaml.v3 按 struct 声明顺序输出），所以「取 → 看 → 贴回」的字节
// 是稳定的：同一份事实永远序列化成同一份文本。map 的键由 yaml.v3 排序。
type nodeYAMLDoc struct {
	APIVersion string       `yaml:"apiVersion"`
	Kind       string       `yaml:"kind"`
	Metadata   nodeYAMLMeta `yaml:"metadata"`
	Spec       nodeYAMLSpec `yaml:"spec"`
}

// nodeYAMLMeta 是文档的 metadata。
//
// 刻意**不带 uid**：uid 是不透明的内部身份，编辑用不到；带上它只会让「取 → 贴回」
// 在写入口撞上 metadata.uid 的只读判定（写端 allowlist 只有 name / resourceVersion）。
// 所以读面给出的文档本身就是可贴回的文档——这是一次真往返，不是半个。
type nodeYAMLMeta struct {
	Name            string `yaml:"name"`
	ResourceVersion string `yaml:"resourceVersion,omitempty"`
}

// nodeYAMLSpec 是文档的 spec：与 NodeSpec 同一字段集。
type nodeYAMLSpec struct {
	Namespaces []string          `yaml:"namespaces"`
	Vars       map[string]string `yaml:"vars"`
	Limits     nodeYAMLLimits    `yaml:"limits"`
	Tags       []string          `yaml:"tags"`
}

// nodeYAMLLimits 是 limits 的文档形状。
//
// `total` 用指针：**未写**（nil）序列化成显式 `total: null`，读起来就是「不压住下层」
// （config.md §4.1 的语义）。perCapability 逐能力给上限，`*` 是缺省档。
type nodeYAMLLimits struct {
	Total         *int           `yaml:"total"`
	PerCapability map[string]int `yaml:"perCapability"`
}

// nodeYAMLSpecPatch 是写请求的 spec：指针/可空形态区分「没给」与「给了空值」。
//
// 与 `nodeSpecPatch` 同一套 merge-patch 语义（nil = 这一项不动；显式空对象/空数组 =
// 清空），所以 YAML 与 JSON 两条写入口对「省略」的解释不可能分叉。
type nodeYAMLSpecPatch struct {
	Namespaces *[]string         `json:"namespaces" yaml:"namespaces"`
	Vars       map[string]string `json:"vars" yaml:"vars"`
	Limits     map[string]any    `json:"limits" yaml:"limits"`
	Tags       *[]string         `json:"tags" yaml:"tags"`
}

// nodeYAMLWrite 是严格解码后的写请求（信封层 + spec）。
type nodeYAMLWrite struct {
	APIVersion *string           `json:"apiVersion"`
	Kind       *string           `json:"kind"`
	Metadata   json.RawMessage   `json:"metadata"`
	Spec       nodeYAMLSpecPatch `json:"spec"`
}

// yamlGet 处理 GET /v1/nodes/{name}/yaml。
func (a *nodeAPI) yamlGet(w http.ResponseWriter, r *http.Request) {
	n, err := a.nodeByName(r.Context(), r.PathValue("name"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	cfg, err := a.d.DB.GetNodeConfig(r.Context(), n.ID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	text, err := nodeYAMLText(n, cfg)
	if err != nil {
		// 库里的配置形状坏到无法投影 = 服务端事实损坏，按统一映射落 500（不是 400：
		// 调用方没提交任何东西）。
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, yamlDoc{YAML: string(text)})
}

// yamlPut 处理 PUT /v1/nodes/{name}/yaml：严格解析一份 Node 文档并整份应用。
//
// 顺序固定：文档层（yamldoc，400 + causes）→ 只读字段（status）→ 严格 JSON 信封解码 →
// 身份校验（kind / apiVersion / name）→ CAS → store.UpdateNode → 回读新文档。
// 任何一步都不静默忽略：静默忽略会让「我改了 YAML」变成一句没有根据的话。
func (a *nodeAPI) yamlPut(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	text, err := decodeYAMLRequestBody(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	res, err := yamldoc.Decode(text, writeDocShape(r, reflect.TypeOf(nodeYAMLSpecPatch{}), "name", "resourceVersion"))
	if err != nil {
		writeYAMLError(w, err)
		return
	}
	// status 是派生的只读面：文档里出现它就是一次「以为能改」的误解，必须具名拒绝
	// （与 config 写入口的 reason 一致，不在这里发明第二个原因码）。
	if nodeHasStatusKey(res.JSON) {
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonStatusNotWritable, "status is read-only"))
		return
	}
	var in nodeYAMLWrite
	dec := json.NewDecoder(bytes.NewReader(res.JSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeAPIError(w, unknownFieldError(err))
		return
	}
	if in.APIVersion != nil && strings.TrimSpace(*in.APIVersion) != nodeYAMLHeader {
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonValueInvalid,
			"apiVersion 必须是 "+nodeYAMLHeader))
		return
	}
	if in.Kind != nil && strings.TrimSpace(*in.Kind) != kindNode {
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonValueInvalid, "kind 必须是 "+kindNode))
		return
	}
	meta := objectMeta{}
	if !isEmptyJSON(in.Metadata) {
		if meta, err = decodeObjectMeta(in.Metadata, map[string]bool{"name": true, "resourceVersion": true}); err != nil {
			writeAPIError(w, err)
			return
		}
	}
	if err := nodeCheckPathName(meta, name); err != nil {
		writeAPIError(w, err)
		return
	}
	rv, err := casTokenFromMeta(meta)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	n, err := a.d.DB.UpdateNode(r.Context(), name, store.NodePatch{
		Namespaces: in.Spec.Namespaces,
		Vars:       in.Spec.Vars,
		Limits:     in.Spec.Limits,
		Tags:       in.Spec.Tags,
	}, rv)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	// 回读一次：UpdateNode 只回节点行，文档要的是配置行（revision / 归一化后的
	// namespaces 缺省值）。用入参拼响应会把「省略 = 不动」读成「写成了空」。
	fresh, err := a.d.DB.GetNodeConfig(r.Context(), n.ID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	out, err := nodeYAMLText(n, fresh)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, yamlDoc{YAML: string(out)})
}

// nodeYAMLText 把节点行 + 配置行序列化成规范化 Node 文档。
func nodeYAMLText(n *store.Node, cfg *store.NodeConfig) ([]byte, error) {
	if n == nil {
		return nil, fmt.Errorf("node yaml: nil node")
	}
	if cfg == nil {
		return nil, fmt.Errorf("node yaml: nil config")
	}
	limits, err := nodeYAMLLimitsOf(cfg.Limits)
	if err != nil {
		return nil, err
	}
	doc := nodeYAMLDoc{
		APIVersion: nodeYAMLHeader,
		Kind:       kindNode,
		Metadata: nodeYAMLMeta{
			Name:            n.Name,
			ResourceVersion: store.ResourceVersion(n.ID, cfg.ResourceVersion),
		},
		Spec: nodeYAMLSpec{
			Namespaces: nodeNonNilStrings(cfg.Namespaces),
			Vars:       nodeNonNilStringMap(cfg.Vars),
			Limits:     limits,
			Tags:       nodeNonNilStrings(n.Tags),
		},
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	// 2 空格缩进：YAML 的惯例，也让页面上的文本与 docs 里的样例逐字一致（yaml.v3 的
	// 默认是 4）。缩进只是排版，不影响语义。
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("node yaml: marshal: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("node yaml: marshal: %w", err)
	}
	return buf.Bytes(), nil
}

// nodeYAMLLimitsOf 把 store 的 `map[string]any` limits 折成文档形状。
//
// store 已经用 `ValidateLimits` 保证只认 total / perCapability（未知维度在写入口就被
// 400 拒了），所以这里不丢信息；折不动只可能是库里的行被手改坏 → 具名错误（落 500）。
func nodeYAMLLimitsOf(limits map[string]any) (nodeYAMLLimits, error) {
	out := nodeYAMLLimits{PerCapability: map[string]int{}}
	keys := make([]string, 0, len(limits))
	for k := range limits {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch k {
		case "total":
			v, ok := nodeLimitInt(limits[k])
			if !ok {
				return nodeYAMLLimits{}, fmt.Errorf("node yaml: limits.total 不是正整数")
			}
			out.Total = &v
		case "perCapability":
			obj, ok := limits[k].(map[string]any)
			if !ok {
				return nodeYAMLLimits{}, fmt.Errorf("node yaml: limits.perCapability 不是对象")
			}
			for name, raw := range obj {
				v, ok := nodeLimitInt(raw)
				if !ok {
					return nodeYAMLLimits{}, fmt.Errorf("node yaml: limits.perCapability.%s 不是正整数", name)
				}
				out.PerCapability[name] = v
			}
		default:
			return nodeYAMLLimits{}, fmt.Errorf("node yaml: limits 未知维度 %q", k)
		}
	}
	return out, nil
}

// nodeLimitInt 把 JSON 解出来的数值归一成正整数（与 store 的校验同口径）。
func nodeLimitInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, n >= 1
	case int64:
		return int(n), n >= 1
	case float64:
		if n != float64(int64(n)) || n < 1 {
			return 0, false
		}
		return int(n), true
	case json.Number:
		v, err := n.Int64()
		if err != nil || v < 1 {
			return 0, false
		}
		return int(v), true
	}
	return 0, false
}
