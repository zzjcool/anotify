package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
)

// 轻量资源信封（api-conventions §3.1–§3.3 / V4-D1）。
//
// 只保留四个顶层键 + 可选 warnings：没有 labels / annotations / generation /
// finalizers / deletionTimestamp（V4-D1「刻意不做」）。status 只读且必须恒存在
// （无事实时 `{}` 而不是 null），空列表恒 `[]`。

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
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return meta, errStatus(http.StatusBadRequest, reasonBadRequest, "cannot read request body")
	}
	_ = r.Body.Close()
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

	allowed := map[string]bool{"name": true, "displayName": true}
	for _, k := range metaKeys {
		allowed[k] = true
	}
	if len(top.Metadata) > 0 && !isEmptyJSON(top.Metadata) {
		meta, err = decodeObjectMeta(top.Metadata, allowed)
		if err != nil {
			return meta, err
		}
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
