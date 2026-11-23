package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

const auditBeforePreEpoch = int64(-1)

func auditLimitQuery(r *http.Request) (int, error) {
	limit, err := taskQueryInt(r, "limit", store.AuditDefaultLimit)
	if err != nil {
		return 0, err
	}
	if limit < 1 || limit > store.AuditMaxLimit {
		return 0, errStatus(http.StatusBadRequest, reasonBadRequest,
			fmt.Sprintf("limit must be between 1 and %d", store.AuditMaxLimit))
	}
	return limit, nil
}

func auditBeforeQuery(r *http.Request) (int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("before"))
	if raw == "" {
		return 0, nil
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return 0, errStatus(http.StatusBadRequest, reasonBadRequest, "before must be an RFC3339 timestamp")
	}
	if sec := at.Unix(); sec > 0 {
		return sec, nil
	}
	return auditBeforePreEpoch, nil
}
