package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/zzjcool/anotify/internal/authn"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

type taskDecisionHandler struct {
	db  *store.DB
	log *slog.Logger
}

func newTaskDecisionHandler(db *store.DB, log *slog.Logger) *taskDecisionHandler {
	if log == nil {
		log = slog.Default()
	}
	return &taskDecisionHandler{db: db, log: log}
}

func (h *taskDecisionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	taskID := r.PathValue("taskId")
	token := authn.BearerToken(r)
	if token == "" {
		writeStatus(w, http.StatusUnauthorized, protocol.ReasonTokenInvalid,
			"缺少 Authorization: Bearer <decision token>", nil)
		return
	}
	body := http.MaxBytesReader(w, r.Body, protocol.DecisionMaxBody)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	var req protocol.DecisionRequest
	if err := dec.Decode(&req); err != nil {
		writeStatus(w, http.StatusBadRequest, reasonBadRequest, "决策体必须是 JSON：{\"outcome\":…,\"params\":{…}}", nil)
		return
	}
	record, err := h.db.RecordDecision(ctx, store.DecisionInput{
		TaskID: taskID, Token: token, Outcome: req.Outcome, Params: req.Params,
	})
	if err != nil {
		h.writeDecisionError(w, taskID, err)
		return
	}
	writeJSON(w, http.StatusOK, protocol.DecisionResponse{
		TaskID: record.TaskID, Outcome: record.Outcome, Actions: record.Actions,
	})
}

func (h *taskDecisionHandler) writeDecisionError(w http.ResponseWriter, taskID string, err error) {
	switch {
	case errors.Is(err, store.ErrAttemptTokenInvalid):
		writeStatus(w, http.StatusUnauthorized, protocol.ReasonTokenInvalid,
			"决策凭据不可用（不存在 / 不匹配 / 已过期）", nil)
	case errors.Is(err, store.ErrOutcomeNotDeclared):
		writeStatus(w, http.StatusForbidden, protocol.ReasonOutcomeNotDeclared, err.Error(), nil)
	case errors.Is(err, store.ErrDecisionAlreadyTaken):
		writeStatus(w, http.StatusConflict, protocol.ReasonDecisionAlreadyTaken,
			"同一个任务的判决只能提交一次", nil)
	case errors.Is(err, store.ErrTaskNotRunning):
		writeStatus(w, http.StatusConflict, protocol.ReasonTaskNotRunning,
			"任务已结束，迟到的判决永远不会被执行", nil)
	case errors.Is(err, store.ErrDecisionNotRouter), errors.Is(err, store.ErrDecisionInvalid):
		writeStatus(w, http.StatusBadRequest, reasonBadRequest, err.Error(), nil)
	default:
		h.log.Warn("decision rejected", "task_id", taskID, "err", err)
		writeStatus(w, http.StatusInternalServerError, reasonInternal, err.Error(), nil)
	}
}
