package server

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/zzjcool/anotify/internal/store"
)

// 自助注册的入口闸门与策略查询（#574）：权威判定在 store 的 registerUser 事务里，这里只提前给干净的 403。

func (h *authHandler) registrationMode(ctx context.Context) (string, error) {
	if h.db.RegistrationPolicy() != store.RegistrationFirstUserOnly {
		return registrationOpen, nil
	}
	n, err := h.db.UserCount(ctx)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return registrationOpen, nil
	}
	return registrationClosed, nil
}

func (h *authHandler) registrationGate(w http.ResponseWriter, r *http.Request) bool {
	mode, err := h.registrationMode(r.Context())
	if err != nil {
		// 读不出计数就不放行：失败方向必须与闸门一致（宁可 500 也不开门）。
		slog.Error("registration policy check failed",
			"event", "auth.register.policy_error",
			"ip", clientIP(r),
			"error", err.Error(),
		)
		writeErr(w, 500, "registration policy check failed")
		return false
	}
	if mode != registrationOpen {
		slog.Info("register rejected",
			"event", "auth.register.closed",
			"ip", clientIP(r),
			"reason", "registration_closed",
		)
		writeErr(w, 403, "registration_closed")
		return false
	}
	return true
}

// writeRegistrationClosed 把事务层的拒绝翻成 403（否则会被包装成 400）。
func writeRegistrationClosed(w http.ResponseWriter, r *http.Request) {
	slog.Info("register rejected",
		"event", "auth.register.closed",
		"ip", clientIP(r),
		"reason", "registration_closed",
	)
	writeErr(w, 403, "registration_closed")
}

type registrationStatusResp struct {
	Registration string `json:"registration"`
}

func (h *authHandler) registrationStatus(w http.ResponseWriter, r *http.Request) {
	// 方法判定先于限流：非 GET 不该占用 120/min 的查询额度，也不该回 404。
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	if rateLimited(w, rlRegistrationStatusIP, clientIP(r)) {
		return
	}
	mode, err := h.registrationMode(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, registrationStatusResp{Registration: mode})
}
