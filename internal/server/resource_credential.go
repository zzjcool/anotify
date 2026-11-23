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

// 命名空间凭据资源面（#554）：值**只写不读**。读投影是派生视图（usedBy 来自源声明），所以不套信封（与 `/edges` 同类）。
const (
	reasonSecretInUse   = "secret_in_use"
	reasonSecretAbsent  = "secret_not_found"
	secretValueMaxBytes = 8 << 10
)

type namespaceSecretDoc struct {
	Name      string   `json:"name"`
	UpdatedAt string   `json:"updatedAt"`
	UsedBy    []string `json:"usedBy"`
}

type namespaceSecretListDoc struct {
	Items []namespaceSecretDoc `json:"items"`
}

type namespaceSecretWrite struct {
	Value *string `json:"value"`
}

type secretAPI struct{ d *serverDeps }

func newSecretAPI(d *serverDeps) *secretAPI { return &secretAPI{d: d} }

func (a *secretAPI) register(mux *http.ServeMux) {
	const base = "/v1/namespaces/{ns}/secrets"
	mux.Handle("GET "+base, noStore(a.d.session(http.HandlerFunc(a.list))))
	mux.Handle("GET "+base+"/{name}", noStore(a.d.session(http.HandlerFunc(a.get))))
	mux.Handle("PUT "+base+"/{name}", noStore(a.d.session(http.HandlerFunc(a.put))))
	mux.Handle("DELETE "+base+"/{name}", noStore(a.d.session(http.HandlerFunc(a.remove))))
}

func secretDocOf(f *store.NamespaceSecretFact) namespaceSecretDoc {
	usedBy := f.UsedBy
	if usedBy == nil {
		usedBy = []string{}
	}
	return namespaceSecretDoc{Name: f.Name, UpdatedAt: rfc3339(f.UpdatedAt), UsedBy: usedBy}
}

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
	if strings.ContainsAny(value, "\r\n") {
		return "", invalid("value 不能含 CR/LF")
	}
	if len(value) > secretValueMaxBytes {
		return "", invalid(fmt.Sprintf("value 最长 %d 字节", secretValueMaxBytes))
	}
	return value, nil
}

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
