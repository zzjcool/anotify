package server

// 按快照重装（#561）：语义在 store.RestorePlaybookRelease（单事务、整包 0 写），这里收参数 + 翻错误码。

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/store"
)

func (a *playbookAPI) restore(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writePlaybookRestoreError(w, err)
		return
	}
	slug := strings.TrimSpace(r.PathValue("slug"))
	release := strings.TrimSpace(r.PathValue("release"))
	if slug == "" || release == "" {
		writePlaybookRestoreError(w, errStatus(http.StatusBadRequest, reasonBadRequest,
			"playbook slug and release are required"))
		return
	}
	body, err := decodePlaybookRestore(r)
	if err != nil {
		writePlaybookRestoreError(w, err)
		return
	}
	digest := strings.TrimSpace(body.ValuesDigest)
	if digest == "" {
		writePlaybookRestoreError(w, errStatus(http.StatusBadRequest, reasonValueInvalid,
			"valuesDigest 必填（要复原哪一组参数）"))
		return
	}
	receipt, err := a.d.DB.RestorePlaybookRelease(r.Context(), ns, slug, release, digest,
		auth.UserIDFromContext(r.Context()))
	if err != nil {
		writePlaybookRestoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

type playbookRestoreBody struct {
	ValuesDigest string `json:"valuesDigest"`
}

func decodePlaybookRestore(r *http.Request) (playbookRestoreBody, error) {
	var body playbookRestoreBody
	if r == nil || r.Body == nil {
		return body, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return body, errStatus(http.StatusBadRequest, reasonBadRequest, "cannot read request body")
	}
	_ = r.Body.Close()
	if len(bytes.TrimSpace(raw)) == 0 {
		return body, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return body, unknownFieldError(err)
	}
	return body, nil
}

// writePlaybookRestoreError 是本端点的 409 出口；其它错误（404 / 400 / 500）走 Playbook 域的统一映射。
func writePlaybookRestoreError(w http.ResponseWriter, err error) {
	var restoreErr *store.PlaybookRestoreError
	switch {
	case errors.As(err, &restoreErr):
		writeStatus(w, http.StatusConflict, restoreReasonOf(restoreErr.Err), restoreErr.Error(),
			&statusDetails{Causes: restoreCausesOf(restoreErr.Causes)})
	case errors.Is(err, store.ErrPlaybookAlreadyInstalled): // 还在生效：该由 install 端点承接
		writeStatus(w, http.StatusConflict, "already_installed", err.Error(), nil)
	case errors.Is(err, store.ErrPlaybookSnapshotMissing):
		writeStatus(w, http.StatusConflict, "no_snapshot",
			"这次安装没有留下安装时快照（迁移 8 之前装的），无法按快照复原", &statusDetails{
				Causes: []statusCause{{Reason: "no_snapshot", Field: "release",
					Message: "该参数组的最新回执没有 created.pinned；先按当前版本重新安装，或手工重建"}},
			})
	default:
		writePlaybookAPIError(w, err)
	}
}

// restoreReasonOf 把 store 的分类哨兵翻成 409 的稳定 reason 字面。
func restoreReasonOf(err error) string {
	switch {
	case errors.Is(err, store.ErrPlaybookPinnedGone):
		return "pinned_gone"
	case errors.Is(err, store.ErrPlaybookPinMissingSource):
		return "pin_missing_source"
	case errors.Is(err, store.ErrPlaybookRestoreConflict):
		return "restore_conflict"
	case errors.Is(err, store.ErrPlaybookAlreadyInstalled):
		return "already_installed"
	default:
		return reasonConflict
	}
}

// restoreCausesOf 把逐对象原因搬进 Status.causes[]（field 用 `pinned.capabilities[i]` / `pinned.sources[i]`）。
func restoreCausesOf(causes []store.PlaybookRestoreCause) []statusCause {
	out := make([]statusCause, 0, len(causes))
	caps, srcs := 0, 0
	for _, c := range causes {
		field := "pinned.sources[" + strconv.Itoa(srcs) + "]"
		if c.Kind == "capability" {
			field = "pinned.capabilities[" + strconv.Itoa(caps) + "]"
			caps++
		} else {
			srcs++
		}
		out = append(out, statusCause{Reason: c.Reason, Field: field, Message: c.Message})
	}
	return out
}
