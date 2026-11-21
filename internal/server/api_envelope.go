package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"

	"github.com/zzjcool/anotify/internal/yamldoc"
)

// 轻量资源信封（api-conventions §3.1–§3.3 / @decision V4-D1）。
//
// 只保留四个顶层键 + 可选 warnings：没有 labels / annotations / generation /
// finalizers / deletionTimestamp（V4-D1「刻意不做」）。status 只读且必须恒存在
// （无事实时 `{}` 而不是 null），空列表恒 `[]`。

// includeBodyQuery 是读取正文全文的统一判据（Capability versions / Playbook 详情）。
//
// 契约（api-conventions §7.1/§7.3）写的是 `?includeBody=true`：两个域必须逐字一致。
// 之前 Capability 只认字面 "true"、Playbook 另认 "1"，同一个前端参数在两个域给出
// 不同结果（一个回正文、一个静默回摘要）——静默退化比 400 更难查。
//
// `1` 作为 v3 前端已发布的写法保留：它是对契约 `true` 的等价别名，不是第四种语义。
func includeBodyQuery(r *http.Request) bool {
	if r == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("includeBody"))) {
	case "true", "1":
		return true
	}
	return false
}

// objectMeta 是信封 metadata（§3.2）。没有 uid 列的资源（playbook 以外的多数资源
// 都有 id 列）仍回填 UID：它是不透明身份，来自 store 行的 id。
type objectMeta struct {
	Name              string `json:"name"`
	Namespace         string `json:"namespace,omitempty"`
	UID               string `json:"uid,omitempty"`
	Version           *int   `json:"version,omitempty"` // 仅 Capability / Playbook
	ResourceVersion   string `json:"resourceVersion,omitempty"`
	CreationTimestamp string `json:"creationTimestamp,omitempty"`
	DisplayName       string `json:"displayName,omitempty"`
}

type apiEnvelope struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   objectMeta `json:"metadata"`
	Spec       any        `json:"spec"`
	Status     any        `json:"status"`
}

type listMeta struct {
	Continue string `json:"continue,omitempty"`
}

type apiList struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   listMeta `json:"metadata"`
	Items      any      `json:"items"`
}

// metaVersion 把整数版本装成指针（Capability / Playbook 的 metadata.version）。
func metaVersion(v int) *int { return &v }

// newEnvelope 组装信封，并保证 spec/status 非 nil（§3.1 E1：无事实给 `{}`）。
func newEnvelope(kind string, meta objectMeta, spec, status any) apiEnvelope {
	if spec == nil {
		spec = struct{}{}
	}
	if status == nil {
		status = struct{}{}
	}
	return apiEnvelope{APIVersion: apiVersionV1, Kind: kind, Metadata: meta, Spec: spec, Status: status}
}

// writeEnvelope 写信封响应。
func writeEnvelope(w http.ResponseWriter, code int, v any) {
	writeJSON(w, code, v)
}

// writeList 写资源列表响应：items 恒为数组（nil → `[]`，§3.1 E7 / 不变量 I12）。
func writeList(w http.ResponseWriter, code int, kind string, meta listMeta, items any) {
	writeJSON(w, code, apiList{APIVersion: apiVersionV1, Kind: kind, Metadata: meta, Items: nonNilSlice(items)})
}

// nonNilSlice 把 nil 切片换成同类型的空切片，保证 JSON 是 `[]` 而不是 `null`。
//
// 用反射而不是让每个调用点手写 `if xs == nil { xs = []T{} }`：漏一个就会有一个
// 端点回 `null`，而前端把它当"未连接"。这里是唯一收口。
func nonNilSlice(v any) any {
	if v == nil {
		return []any{}
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Slice && rv.IsNil() {
		return reflect.MakeSlice(rv.Type(), 0, 0).Interface()
	}
	return v
}

// writeEnvelopeStrict 用于写请求的解码结果便于统一报错；见 decodeWriteEnvelope。
var errEmptyBody = errors.New("request body is required")

// writeBodyLimit 是资源写请求的请求体上限（8 MiB）。
//
// 两个入口共用这一个数字：JSON 信封（decodeWriteEnvelope）与「JSON 里装一份 YAML」
// （decodeYAMLRequestBody）。分成两个数字迟早一个改了另一个没改。
const writeBodyLimit = 8 << 20

// writeMeta 顶层允许的键（写请求）。
type writeRequestMeta struct {
	APIVersion *string         `json:"apiVersion"`
	Kind       *string         `json:"kind"`
	Metadata   json.RawMessage `json:"metadata"`
	Spec       json.RawMessage `json:"spec"`
	Status     json.RawMessage `json:"status"`
}

// decodeWriteEnvelope 严格解码资源写请求（POST / PATCH / PUT 资源信封）。
//
// 契约要求（§3.1 E2/E4/E5/E6）：
//   - 未知顶层字段 → 400 unknown_field（不静默忽略：静默忽略会让人以为字段生效了）
//   - 写请求带非空 status → 400 status_not_writable
//   - kind 与路径不一致 → 400 kind_mismatch
//   - spec 未知字段 → 400 unknown_field
//
// metaKeys 是「该 kind 额外允许的 metadata 键」（例如 capability/playbook 允许 version）。
// name / displayName 恒允许；uid / creationTimestamp 等服务端字段一律拒。
func decodeWriteEnvelope(r *http.Request, wantKind string, specDst any, metaKeys ...string) (objectMeta, error) {
	var meta objectMeta
	if r.Body == nil {
		return meta, errStatus(http.StatusBadRequest, reasonBadRequest, errEmptyBody.Error())
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, writeBodyLimit))
	if err != nil {
		return meta, errStatus(http.StatusBadRequest, reasonBadRequest, "cannot read request body")
	}
	_ = r.Body.Close()
	return decodeWriteEnvelopeBytes(r, raw, wantKind, specDst, metaKeys...)
}

// decodeWriteEnvelopeBytes 是 decodeWriteEnvelope 的「body 已经读过」版本。
//
// 存在的理由只有一条：`POST …/capabilities/yaml` 的正文要先过 `yamldoc.Decode`
// 变成 canonical JSON 才能走同一条严格解码，而那条路径没有「原始 HTTP body」可读。
// 语义与 decodeWriteEnvelope 完全一致（后者只是读完 body 再调这里），所以未知顶层
// 字段 / status 只读 / kind 不一致 / metadata 白名单的判据不会分叉成两份。
func decodeWriteEnvelopeBytes(r *http.Request, raw []byte, wantKind string, specDst any, metaKeys ...string) (objectMeta, error) {
	var meta objectMeta
	if len(bytes.TrimSpace(raw)) == 0 {
		return meta, errStatus(http.StatusBadRequest, reasonBadRequest, errEmptyBody.Error())
	}

	var top writeRequestMeta
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&top); err != nil {
		return meta, unknownFieldError(err)
	}

	if top.Status != nil && !isEmptyJSON(top.Status) {
		return meta, errStatus(http.StatusBadRequest, reasonStatusNotWritable, "status is read-only")
	}
	if top.Kind != nil && *top.Kind != "" && *top.Kind != wantKind {
		return meta, errStatus(http.StatusBadRequest, reasonKindMismatch,
			fmt.Sprintf("kind %q does not match %q", *top.Kind, wantKind))
	}
	if top.APIVersion != nil && *top.APIVersion != "" && *top.APIVersion != apiVersionV1 {
		return meta, errStatus(http.StatusBadRequest, reasonBadRequest,
			fmt.Sprintf("apiVersion %q is not supported", *top.APIVersion))
	}

	allowed := writeMetaAllowList(r, metaKeys...)
	if len(top.Metadata) > 0 && !isEmptyJSON(top.Metadata) {
		decoded, err := decodeObjectMeta(top.Metadata, allowed)
		if err != nil {
			return meta, err
		}
		meta = decoded
	}

	if specDst != nil {
		if len(top.Spec) == 0 || isEmptyJSON(top.Spec) {
			// 允许空 spec（例如 Namespace 的 `spec: {}`）。
			return meta, nil
		}
		d := json.NewDecoder(bytes.NewReader(top.Spec))
		d.DisallowUnknownFields()
		if err := d.Decode(specDst); err != nil {
			return meta, unknownFieldError(err)
		}
	}
	return meta, nil
}

// writeMetaAllowList 是写请求允许的 metadata 键白名单（唯一一份）。
//
// 两个消费点共用它：JSON 信封解码（decodeWriteEnvelopeBytes 交给 decodeObjectMeta）
// 与 YAML 文档形状（writeDocShape 的 Meta）。抄两份的症状是「YAML 里能写、JSON 里
// 不能写」这种只在一条路径上出现的 400。
//
// 两个「按位置生效」的键（§3.4 / §3.2）：
//   - metadata.resourceVersion：只在 PUT/PATCH 是合法令牌；POST 创建时带了它是
//     调用方误解了协议（创建不套 CAS），必须 400 而不是静默丢弃。
//   - metadata.namespace：只在 namespaced 路径下有意义（可省略；显式值与 path
//     不同 → namespace_mismatch）。cluster 级资源（Node/Namespace）带它就是写错。
func writeMetaAllowList(r *http.Request, metaKeys ...string) map[string]bool {
	allowed := map[string]bool{"name": true, "displayName": true}
	if r.Method == http.MethodPatch || r.Method == http.MethodPut {
		allowed["resourceVersion"] = true
	}
	if strings.TrimSpace(r.PathValue("ns")) != "" {
		allowed["namespace"] = true
	}
	for _, k := range metaKeys {
		allowed[k] = true
	}
	return allowed
}

// writeDocShape 描述一份写请求 YAML 文档允许的形状（`POST …/yaml` 用）。
//
// 三条纪律决定了它必须从现有类型推导、而不是另写一张表：
//   - 顶层键只认 writeRequestMeta 的 json tag（多一个少一个都是双写规则）；
//   - metadata 键只认 writeMetaAllowList（安全边界，与 JSON 路径同一份）；
//   - spec 的类型由调用方给（能力 artifact.Spec / 源 srcSpecDoc），这里不猜。
//
// status 的类型是 nil：它**允许出现**（由后续的严格 JSON 解码按 status_not_writable
// 拒掉），形状校验本身不对它做类型检查 —— 报「status 类型不对」而不是「status 只读」
// 会把调用方引向错误的心智模型。
func writeDocShape(r *http.Request, specType reflect.Type, metaKeys ...string) yamldoc.Shape {
	metaType := reflect.TypeOf(objectMeta{})
	allow := writeMetaAllowList(r, metaKeys...)
	meta := make(map[string]reflect.Type, len(allow))
	for i := 0; i < metaType.NumField(); i++ {
		f := metaType.Field(i)
		name := jsonFieldName(f)
		if name == "" || !allow[name] {
			continue
		}
		meta[name] = f.Type
	}

	requestType := reflect.TypeOf(writeRequestMeta{})
	root := make(map[string]reflect.Type, requestType.NumField())
	for i := 0; i < requestType.NumField(); i++ {
		f := requestType.Field(i)
		name := jsonFieldName(f)
		if name == "" {
			continue
		}
		switch f.Name {
		case "APIVersion", "Kind":
			// 保持 writeRequestMeta 的原始类型（*string → yamldoc 走 indirect）。
			root[name] = f.Type
		case "Metadata":
			// RawMessage 是 opaque，会跳过键校验；这里必须换成 objectMeta（键集由 Meta 裁剪）。
			root[name] = metaType
		case "Spec":
			root[name] = specType
		default:
			// status：存在但不检查类型（见函数注释）。
			root[name] = nil
		}
	}
	return yamldoc.Shape{Root: root, Meta: meta, Required: []string{"metadata.name"}}
}

// jsonFieldName 取结构体字段的 JSON 键名（与 encoding/json 同规则：tag 名优先、
// `json:"-"` 跳过、无 tag 用字段名）。
func jsonFieldName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if name == "-" {
		return ""
	}
	if name == "" {
		return f.Name
	}
	return name
}

// decodeObjectMeta 严格解码写请求的 metadata：只认白名单键。
func decodeObjectMeta(raw json.RawMessage, allowed map[string]bool) (objectMeta, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return objectMeta{}, errStatus(http.StatusBadRequest, reasonBadRequest, "metadata must be an object")
	}
	for k := range m {
		if !allowed[k] {
			return objectMeta{}, errStatus(http.StatusBadRequest, reasonUnknownField,
				fmt.Sprintf("metadata.%s is not writable", k))
		}
	}
	var meta objectMeta
	if v, ok := m["name"]; ok {
		if err := json.Unmarshal(v, &meta.Name); err != nil {
			return objectMeta{}, errStatus(http.StatusBadRequest, reasonBadRequest, "metadata.name must be a string")
		}
	}
	if v, ok := m["displayName"]; ok {
		if err := json.Unmarshal(v, &meta.DisplayName); err != nil {
			return objectMeta{}, errStatus(http.StatusBadRequest, reasonBadRequest, "metadata.displayName must be a string")
		}
	}
	if v, ok := m["namespace"]; ok {
		if err := json.Unmarshal(v, &meta.Namespace); err != nil {
			return objectMeta{}, errStatus(http.StatusBadRequest, reasonBadRequest, "metadata.namespace must be a string")
		}
	}
	if v, ok := m["version"]; ok {
		var ver int
		if err := json.Unmarshal(v, &ver); err != nil {
			return objectMeta{}, errStatus(http.StatusBadRequest, reasonBadRequest, "metadata.version must be an integer")
		}
		meta.Version = &ver
	}
	if v, ok := m["resourceVersion"]; ok {
		if err := json.Unmarshal(v, &meta.ResourceVersion); err != nil {
			return objectMeta{}, errStatus(http.StatusBadRequest, reasonBadRequest, "metadata.resourceVersion must be a string")
		}
	}
	return meta, nil
}

// isEmptyJSON 判断一段 JSON 是否为 `{}` / `null` / 空白。
func isEmptyJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("{}")) {
		return true
	}
	return false
}

// unknownFieldError 把 encoding/json 的严格解码错误翻成 400。
//
// Go 的错误文本形如 `json: unknown field "foo"` / `json: unknown field "spec.bar"`；
// 契约要的是机器码 unknown_field + 可读 message，所以这里统一包装而不是把原始
// 错误文本直接回给客户端（原始文本会随 Go 版本漂移，客户端不该依赖它）。
func unknownFieldError(err error) error {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return errStatus(http.StatusBadRequest, reasonBadRequest,
			fmt.Sprintf("field %q must be %s", typeErr.Field, typeErr.Type.String()))
	}
	return errStatus(http.StatusBadRequest, reasonUnknownField, err.Error())
}

// checkMetaNamespace 校验 body 里的 metadata.namespace 与路径/上下文一致（§3.2）。
//
// 显式值与 path 不同 → 400 namespace_mismatch；两边都给不出来由调用方决定（通常路径
// 一定给了，所以这里只在显式冲突时报错）。
func checkMetaNamespace(meta objectMeta, pathNS string) error {
	if meta.Namespace == "" || pathNS == "" {
		return nil
	}
	if meta.Namespace != pathNS {
		return errStatus(http.StatusBadRequest, reasonNamespaceMismatch,
			fmt.Sprintf("metadata.namespace %q does not match path namespace %q", meta.Namespace, pathNS))
	}
	return nil
}

// requireMetaName 校验创建请求必须带 metadata.name（§3.4 POST）。
func requireMetaName(meta objectMeta) (string, error) {
	if meta.Name == "" {
		return "", errStatus(http.StatusBadRequest, reasonBadRequest, "metadata.name is required")
	}
	return meta.Name, nil
}
