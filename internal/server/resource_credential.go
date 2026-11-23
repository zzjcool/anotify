package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

// 命名空间凭据资源面（#554）：值**只写不读**，读投影只有 name/updatedAt/usedBy。
//
// 为什么不做成信封资源：凭据没有 spec/status 两半（只有「一个值」这个写指令），
// 读面是派生视图（usedBy 来自源声明），与 `/edges`、`/capabilities/{n}/audit` 同类。
// 写入仍走 store 的同一份 upsert（老路径 config.secret + secretRef 用的就是它）。
const (
	reasonSecretInUse  = "secret_in_use"
	reasonSecretAbsent = "secret_not_found"
	// 名字由 sourcespec.ValidateSecretName 判（与 config.secretRef.name 同一个函数）；这里是路径参数缺失。
	secretValueMaxBytes = 8 << 10
)

// namespaceSecretDoc 是凭据读投影。**没有 value 字段** —— 值只写不读，#554 语义 1。
type namespaceSecretDoc struct {
	Name      string   `json:"name"`
	UpdatedAt string   `json:"updatedAt"`
	UsedBy    []string `json:"usedBy"`
}

// namespaceSecretListDoc 是非信封列表（`items` 恒为 `[]` 而不是 `null`）。
type namespaceSecretListDoc struct {
	Items []namespaceSecretDoc `json:"items"`
}

// namespaceSecretWrite 是 PUT body：`value` 是**唯一**可写字段。
type namespaceSecretWrite struct {
	Value *string `json:"value"`
}

// secretAPI 是凭据资源域的全部 handler。
type secretAPI struct{ d *serverDeps }

// newSecretAPI 构造凭据资源域。
func newSecretAPI(d *serverDeps) *secretAPI { return &secretAPI{d: d} }

// register 把凭据资源域挂到 mux 上（会话保护 + no-store：值不进任何缓存）。
func (a *secretAPI) register(mux *http.ServeMux) {
	const base = "/v1/namespaces/{ns}/secrets"
	mux.Handle("GET "+base, noStore(a.d.session(http.HandlerFunc(a.list))))
	mux.Handle("GET "+base+"/{name}", noStore(a.d.session(http.HandlerFunc(a.get))))
	mux.Handle("PUT "+base+"/{name}", noStore(a.d.session(http.HandlerFunc(a.put))))
	mux.Handle("DELETE "+base+"/{name}", noStore(a.d.session(http.HandlerFunc(a.remove))))
}

// ---------- 投影 ----------

// secretDocOf 把 store 事实折成读投影（时间戳秒 → RFC3339；usedBy 空 = `[]`）。
func secretDocOf(f *store.NamespaceSecretFact) namespaceSecretDoc {
	usedBy := f.UsedBy
	if usedBy == nil {
		usedBy = []string{}
	}
	return namespaceSecretDoc{Name: f.Name, UpdatedAt: rfc3339(f.UpdatedAt), UsedBy: usedBy}
}

// secretNameFromPath 取 `{name}` 并按**与 config.secretRef.name 相同的规则**校验。
//
// 非法名字是调用方输入错（400 + causes.field = name），不是「不存在」：否则一个
// 手误的名字会看起来像「凭据被删了」，而且两种面（源引用 / 新端点）对同一串给出不同结论。
func secretNameFromPath(r *http.Request) (string, error) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		return "", errStatus(http.StatusBadRequest, reasonBadRequest, "secret name is required")
	}
	if err := sourcespec.ValidateSecretName(name); err != nil {
		return "", &apiError{
			Code:    http.StatusBadRequest,
			Reason:  reasonValueInvalid,
			Message: "非法凭据名字：" + err.Error(),
			Details: &statusDetails{Causes: []statusCause{{
				Reason:  reasonValueInvalid,
				Message: err.Error(),
				Field:   "name",
			}}},
		}
	}
	return name, nil
}

// decodeSecretWrite 严格解码 PUT body：未知字段 → 400，`value` 缺失 → 400（causes.field = value）。
func decodeSecretWrite(r *http.Request) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return "", errStatus(http.StatusBadRequest, reasonBadRequest, "cannot read request body")
	}
	_ = r.Body.Close()
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(raw)))
	dec.DisallowUnknownFields()
	var body namespaceSecretWrite
	if err := dec.Decode(&body); err != nil {
		return "", unknownFieldError(err)
	}
	invalid := func(msg string) error {
		return &apiError{
			Code:    http.StatusBadRequest,
			Reason:  reasonValueInvalid,
			Message: msg,
			Details: &statusDetails{Causes: []statusCause{{Reason: reasonValueInvalid, Message: msg, Field: "value"}}},
		}
	}
	if body.Value == nil {
		return "", invalid("value 必填（轮换就写新值；删凭据用 DELETE）")
	}
	value := *body.Value
	if value == "" {
		return "", invalid("value 不能为空")
	}
	// 值最终进 HTTP 请求头（secretRef.header）：CR/LF 是头注入面，写入期就拒。
	if strings.ContainsAny(value, "\r\n") {
		return "", invalid("value 不能含 CR/LF")
	}
	if len(value) > secretValueMaxBytes {
		return "", invalid(fmt.Sprintf("value 最长 %d 字节", secretValueMaxBytes))
	}
	return value, nil
}

// ---------- handlers ----------

// list 处理 GET …/secrets。
func (a *secretAPI) list(w http.ResponseWriter, r *http.Request) {
	nsID, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	list, err := a.d.DB.ListNamespaceSecrets(r.Context(), nsID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	items := make([]namespaceSecretDoc, 0, len(list))
	for i := range list {
		items = append(items, secretDocOf(&list[i]))
	}
	writeJSON(w, http.StatusOK, namespaceSecretListDoc{Items: items})
}

// get 处理 GET …/secrets/{name}（同样不含值）。
func (a *secretAPI) get(w http.ResponseWriter, r *http.Request) {
	nsID, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	name, err := secretNameFromPath(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	fact, err := a.d.DB.GetNamespaceSecretMeta(r.Context(), nsID, name)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, secretDocOf(fact))
}

// put 处理 PUT …/secrets/{name}：新建或轮换（同一个 store 写函数 + 一条审计）。
//
// 轮换立刻生效：poll 每一轮都按 (ns, name) 重新读值（internal/source/poll_fetch.go），
// 这里不缓存、也不推送到节点 —— 「下一轮用新值」由读时取值保证。
func (a *secretAPI) put(w http.ResponseWriter, r *http.Request) {
	nsID, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	name, err := secretNameFromPath(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	value, err := decodeSecretWrite(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if _, err := a.d.DB.PutNamespaceSecret(r.Context(), nsID, name, value, auth.UserIDFromContext(r.Context())); err != nil {
		writeAPIError(w, err)
		return
	}
	fact, err := a.d.DB.GetNamespaceSecretMeta(r.Context(), nsID, name)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, secretDocOf(fact))
}

// remove 处理 DELETE …/secrets/{name}：被源引用 → 409 secret_in_use（不删）。
func (a *secretAPI) remove(w http.ResponseWriter, r *http.Request) {
	nsID, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	name, err := secretNameFromPath(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := a.d.DB.DeleteNamespaceSecret(r.Context(), nsID, name, auth.UserIDFromContext(r.Context())); err != nil {
		writeAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
