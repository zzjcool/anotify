package server

// 生产装配的时钟单位（#340 r2 review M-1）。
//
// 为什么需要这一条：资源域的单测都自己构造 serverDeps 并注入**毫秒** now，各 handler 的
// 换算（`taskNowSeconds` = `d.now()/1000`）因此总是被喂正确单位。一旦**生产装配**
// （`mux.go` 的 NewApp）把 `store.Now`（秒）塞进 `serverDeps.Now`（毫秒契约，见
// `api_deps.go` 的 nowMilli 注释），单域测试仍然全绿，而线上静默算错：
//
//   - `retained_reads.go` 的 stats 窗口起点 = `taskNowSeconds(d)-371*86400`；
//     秒被再除 1000 → 1.7e6（1970-01-21）→ `since` 变负数 → `created_at >= since` 恒真
//     → 热力图把全部历史都算进桶里；
//   - `store.RequestCancel` 的 now 落成 1970。
//
// 所以单位对齐是**装配的责任**，必须在真 NewApp 上钉住。本文件刻意不复用 `taskResEnv`
// 之类的裸 mux 夹具：那些夹具自带 now 注入，会把这类错位遮住（那正是 M-1 被发现的原因）。

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/store"
)

// prodClockSession 建一个真实用户（注册即建 default 命名空间）+ 会话。
//
// 返回会话 Cookie 与 default 命名空间 id（写库要 id，请求路径要 slug）。
func prodClockSession(t *testing.T, app *App) (*http.Cookie, string) {
	t.Helper()
	ctx := context.Background()
	user := &store.User{
		ID: store.NewUserID(), Username: "clock-unit", DisplayName: "时钟单位",
		Role: store.RoleAdmin, CreatedAt: store.Now(),
	}
	if _, err := app.DB.RegisterUser(ctx, user); err != nil {
		t.Fatalf("register user: %v", err)
	}
	sess, err := app.AuthSvc.Sessions().Create(user.ID, "production clock unit")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	nss, err := app.DB.ListNamespaces(ctx)
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	for _, ns := range nss {
		if ns.Slug == "default" {
			return &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID}, ns.ID
		}
	}
	t.Fatalf("注册后没有 default 命名空间（namespaces=%d）", len(nss))
	return nil, ""
}

// prodClockSeedEvent 插一条事件行（stats 的事实源）。
func prodClockSeedEvent(t *testing.T, app *App, nsID string, createdAt int64) {
	t.Helper()
	if _, err := app.DB.Exec(
		`INSERT INTO events (id, namespace_id, trace_id, source, type, payload, reason, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		store.NewEventID(), nsID, "trace_clock", "route", "clock.window", `{}`,
		store.EventReasonIngested, createdAt); err != nil {
		t.Fatalf("seed event (created_at=%d): %v", createdAt, err)
	}
}

// prodClockSeedPendingTask 插一条 pending 任务行（cancel_requested_at 的写入目标）并返回 id。
//
// 列集与 `internal/server/resource_task_test.go` 的 insertTask 保持一致；这里只需要一条
// 「可被取消」的最小行，不重复那边的可调字段矩阵。
func prodClockSeedPendingTask(t *testing.T, app *App, nsID string) string {
	t.Helper()
	capID := "capv_clock_unit"
	if _, err := app.DB.Exec(
		`INSERT INTO capability_versions (id, namespace_id, name, version, spec, digest, fold_key, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		capID, nsID, "clock", 1, `{}`, "sha256:clock-unit", "fold_"+capID, store.Now()); err != nil {
		t.Fatalf("seed capability: %v", err)
	}
	taskID := store.NewTaskID()
	if _, err := app.DB.Exec(
		`INSERT INTO tasks
		   (id, namespace_id, trace_id, parent_task_id, capability_id, capability, version, digest, params,
		    phase, attempt, node_id, node_name, config_revision, max_attempts, lease_until,
		    waiting_reason, next_attempt_at, summary, artifacts, error, created_at, started_at, finished_at,
		    namespace_vars_revision, result_json)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,NULL,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		taskID, nsID, "trace_clock", nil, capID, "clock", 1, "sha256:clock-unit", `{}`,
		store.TaskPending, 0, "", int64(0), 3, nil,
		"", nil, "", "[]", "", store.Now(), nil, nil,
		int64(0), ""); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return taskID
}

// TestProductionMuxStatsWindowUsesMillisecondClock 钉住 stats 的 371 天窗口在真实装配上生效。
//
// 判据：窗口只影响 `daily`（热力图），不影响 `total`（全量数）。插两条事件——400 天前
// （窗口外）与现在——正确装配下 daily 只有一个桶、合计 1；若 Now 注入成秒，窗口起点变
// 负数，两条都进桶（两个桶、合计 2）。
func TestProductionMuxStatsWindowUsesMillisecondClock(t *testing.T) {
	app := newProductionAppForTest(t)
	cookie, nsID := prodClockSession(t, app)

	now := store.Now()
	prodClockSeedEvent(t, app, nsID, now-400*86400) // 371 天窗口之外
	prodClockSeedEvent(t, app, nsID, now)           // 今天

	rr := appDo(t, app, http.MethodGet, "/v1/stats?namespace=default", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v1/stats = %d, want 200（body=%s）", rr.Code, rr.Body.String())
	}
	var stats struct {
		Total int `json:"total"`
		Daily []struct {
			Day   string `json:"day"`
			Count int    `json:"count"`
		} `json:"daily"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &stats); err != nil {
		t.Fatalf("stats 响应不是 JSON: %v（%s）", err, rr.Body.String())
	}

	// total 是「该命名空间的事件总数」，不经过窗口——两条都算。
	if stats.Total != 2 {
		t.Fatalf("stats.total = %d, want 2（body=%s）", stats.Total, rr.Body.String())
	}

	// daily 是 371 天窗口内的按天分桶：400 天前那条必须被过滤掉。
	bucketSum := 0
	for _, dc := range stats.Daily {
		bucketSum += dc.Count
	}
	if len(stats.Daily) != 1 || bucketSum != 1 {
		t.Fatalf("stats.daily = %+v（桶数 %d、合计 %d），want 恰好 1 桶、合计 1："+
			"生产装配 serverDeps.Now 是毫秒契约（api_deps.go/D1），若 mux.go 注入 store.Now（秒），"+
			"taskNowSeconds 把秒再除 1000 得到 1.7e6（1970-01-21）→ since 变负数 → 371 天窗口失效、全部历史进桶",
			stats.Daily, len(stats.Daily), bucketSum)
	}
}

// TestProductionMuxCancelRequestedAtIsRealTime 钉住 cancel_requested_at 是真时间（秒）。
//
// 判据：取消意图的时间来自 `taskNowSeconds(d)`。装配注入秒时该值落成 1970-01-21，
// 读回来的 CancelRequestedAt 会远离现在——这条用例只认「现在 ±120 秒」。
func TestProductionMuxCancelRequestedAtIsRealTime(t *testing.T) {
	app := newProductionAppForTest(t)
	cookie, nsID := prodClockSession(t, app)
	ctx := context.Background()

	taskID := prodClockSeedPendingTask(t, app, nsID)

	rr := appDo(t, app, http.MethodPost, "/v1/namespaces/default/tasks/"+taskID+"/cancel", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST cancel = %d, want 200（body=%s）", rr.Code, rr.Body.String())
	}

	task, err := app.DB.GetTask(ctx, nsID, taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.CancelRequestedAt == nil {
		t.Fatalf("cancel_requested_at = nil, want 真时间（任务相位=%s）", task.Phase)
	}
	at := *task.CancelRequestedAt
	// 先取 now 再取 now 的边界：两次调用最多差毫秒级，120 秒余量足够。
	lo, hi := store.Now()-120, store.Now()+120
	if at < lo || at > hi {
		t.Fatalf("cancel_requested_at = %d（%s），want 落在 [%d,%d]（unix 秒）："+
			"生产装配 serverDeps.Now 是毫秒契约（api_deps.go/D1），若 mux.go 注入 store.Now（秒），"+
			"taskNowSeconds 把秒再除 1000 → 取消意图落成 1970-01-21",
			at, time.Unix(at, 0).UTC().Format(time.RFC3339), lo, hi)
	}
}
