package server

// resource_source_poll_test.go 是「立即拉取一次」（#555）的 handler 级验收。
//
// 三件事必须在这里钉死，因为它们是 issue 的语义而不是投影细节：
//   - 与定时轮询共用同一把 in-flight 锁（在飞 → 409 poll_in_progress，不排队）；
//   - 本轮失败 → 502 poll_failed，且 status.cursor 不动；
//   - 非 poll 源 → 409 not_poll_source。
//
// feed 用 httptest（源 config 的 allowInsecure 让它走运行期的那条 transport）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// pollNowSourceSpec 造一份 kind=poll 的 spec（envelope 指向响应对象的 items 键）。
func pollNowSourceSpec(url string, enabled bool) map[string]any {
	return map[string]any{
		"kind":    "poll",
		"enabled": enabled,
		"config": map[string]any{
			"url":           url,
			"schedule":      map[string]any{"expr": "*/5 * * * *"},
			"allowInsecure": true,
			"maxItems":      10,
			"envelope": map[string]any{
				"items":      "items",
				"type":       map[string]any{"pointer": "type"},
				"deliveryId": map[string]any{"pointer": "id"},
				"payload":    map[string]any{"pointer": ""},
			},
		},
		"targets": []map[string]any{{"capability": "notify"}},
	}
}

// pollFeed 起一个可控的 feed：mode=0 返回两条 item（带 ETag），mode=1 返回「items 不是数组」。
func pollFeed(t *testing.T, mode *atomic.Int32, gate <-chan struct{}, hit chan<- struct{}) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hit != nil {
			select {
			case hit <- struct{}{}:
			default:
			}
		}
		if gate != nil {
			<-gate
		}
		w.Header().Set("Content-Type", "application/json")
		if mode.Load() == 1 {
			w.Header().Set("ETag", `"e2"`)
			_, _ = w.Write([]byte(`{"feed":{"items":[]}}`))
			return
		}
		w.Header().Set("ETag", `"e1"`)
		_, _ = w.Write([]byte(`{"items":[{"id":"a","type":"advisory"},{"id":"b","type":"advisory"}]}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// TestSourcePollNowCountsAndAudit 200 的五个字段、真的入库、以及一行审计（actor + 计数）。
func TestSourcePollNowCountsAndAudit(t *testing.T) {
	e := newSourceResEnv(t)
	var mode atomic.Int32
	feed := pollFeed(t, &mode, nil, nil)
	e.createSource(pollNowSourceSpec(feed.URL, true), map[string]any{"name": "poll.now"})

	rr := e.request(http.MethodPost, e.sourcePath("poll.now")+"/poll", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("立即拉取期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	doc := e.doc(rr)
	if doc["items"] != float64(2) || doc["created"] != float64(2) || doc["duplicates"] != float64(0) {
		t.Fatalf("计数不对：%v", doc)
	}
	if doc["cursorAdvanced"] != true {
		t.Fatalf("首轮游标应前进：%v", doc)
	}
	if _, ok := doc["durationMs"].(float64); !ok {
		t.Fatalf("durationMs 必须是数字：%v", doc["durationMs"])
	}
	if got := e.count("events"); got != 2 {
		t.Fatalf("立即拉取的事件数 = %d，want 2", got)
	}

	proj := e.doc(e.request(http.MethodGet, e.sourcePath("poll.now"), nil))
	status := sourceResObj(t, proj["status"])
	if status["cursor"] != `"e1"` {
		t.Fatalf("status.cursor = %v，want \"e1\"", status["cursor"])
	}
	if _, ok := status["lastPollAt"]; !ok {
		t.Fatalf("立即拉取后必须有 status.lastPollAt：%v", status)
	}

	var (
		action, actor, detail string
	)
	if err := e.db.QueryRow(`SELECT action, actor_id, detail FROM source_audit`).Scan(&action, &actor, &detail); err != nil {
		t.Fatalf("读审计行：%v", err)
	}
	if action != "source.polled_manually" || actor == "" {
		t.Fatalf("审计行 action/actor = %q/%q", action, actor)
	}
	var counts map[string]any
	if err := json.Unmarshal([]byte(detail), &counts); err != nil {
		t.Fatalf("审计 detail 不是 JSON：%v", err)
	}
	if counts["ok"] != true || counts["items"] != float64(2) || counts["created"] != float64(2) {
		t.Fatalf("审计 detail 的计数不对：%v", counts)
	}

	again := e.doc(e.request(http.MethodPost, e.sourcePath("poll.now")+"/poll", nil))
	if again["created"] != float64(0) || again["duplicates"] != float64(2) {
		t.Fatalf("第二轮应全部命中去重：%v", again)
	}
	if again["cursorAdvanced"] != false {
		t.Fatalf("同一份 ETag 的第二轮不该报游标前进：%v", again)
	}
}

// TestSourcePollNowRejectsNonPollAndDisabled 两道前置拒绝：非 poll 与停用都是 409（不是 404）。
func TestSourcePollNowRejectsNonPollAndDisabled(t *testing.T) {
	e := newSourceResEnv(t)
	e.createSource(map[string]any{
		"kind": "manual", "enabled": true,
		"targets": []map[string]any{{"capability": "notify"}},
	}, map[string]any{"name": "manual.one"})
	var mode atomic.Int32
	feed := pollFeed(t, &mode, nil, nil)
	e.createSource(pollNowSourceSpec(feed.URL, false), map[string]any{"name": "poll.off"})

	for _, tc := range []struct{ name, reason string }{
		{"manual.one", "not_poll_source"},
		{"poll.off", "source_disabled"},
	} {
		rr := e.request(http.MethodPost, e.sourcePath(tc.name)+"/poll", nil)
		if rr.Code != http.StatusConflict {
			t.Fatalf("%s 立即拉取期望 409，得到 %d：%s", tc.name, rr.Code, rr.Body.String())
		}
		if reason := e.doc(rr)["reason"]; reason != tc.reason {
			t.Fatalf("%s 的 reason = %v，want %s", tc.name, reason, tc.reason)
		}
	}
}

// TestSourcePollNowInFlightConflict 第一轮还在飞时第二次触发 → 409 poll_in_progress，放行后成功。
func TestSourcePollNowInFlightConflict(t *testing.T) {
	e := newSourceResEnv(t)
	var mode atomic.Int32
	gate := make(chan struct{})
	hit := make(chan struct{}, 4)
	feed := pollFeed(t, &mode, gate, hit)
	e.createSource(pollNowSourceSpec(feed.URL, true), map[string]any{"name": "poll.slow"})

	first := make(chan int, 1)
	go func() {
		first <- e.request(http.MethodPost, e.sourcePath("poll.slow")+"/poll", nil).Code
	}()
	<-hit // 第一轮已经进了 feed（= 锁已持有）

	rr := e.request(http.MethodPost, e.sourcePath("poll.slow")+"/poll", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("在飞时的第二次触发期望 409，得到 %d：%s", rr.Code, rr.Body.String())
	}
	if reason := e.doc(rr)["reason"]; reason != "poll_in_progress" {
		t.Fatalf("reason = %v，want poll_in_progress", reason)
	}

	close(gate)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("放行后的第一轮 = %d，want 200", code)
	}
	// 放行后再点一次必须能成功（锁已释放，不是永久占用）。
	done := make(chan int, 1)
	go func() { done <- e.request(http.MethodPost, e.sourcePath("poll.slow")+"/poll", nil).Code }()
	if code := <-done; code != http.StatusOK {
		t.Fatalf("锁释放后的再次触发 = %d，want 200", code)
	}
}

// TestSourcePollNowFailureKeepsCursor 失败 → 502 poll_failed + 错误原文，且 status.cursor 不动。
func TestSourcePollNowFailureKeepsCursor(t *testing.T) {
	e := newSourceResEnv(t)
	var mode atomic.Int32
	feed := pollFeed(t, &mode, nil, nil)
	e.createSource(pollNowSourceSpec(feed.URL, true), map[string]any{"name": "poll.bad"})

	if rr := e.request(http.MethodPost, e.sourcePath("poll.bad")+"/poll", nil); rr.Code != http.StatusOK {
		t.Fatalf("首轮期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	mode.Store(1) // envelope.items 指向的位置不是数组
	rr := e.request(http.MethodPost, e.sourcePath("poll.bad")+"/poll", nil)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("失败轮期望 502，得到 %d：%s", rr.Code, rr.Body.String())
	}
	body := e.doc(rr)
	if body["reason"] != "poll_failed" {
		t.Fatalf("reason = %v，want poll_failed", body["reason"])
	}
	if msg, _ := body["message"].(string); msg == "" {
		t.Fatalf("502 必须带错误原文：%v", body)
	}
	proj := e.doc(e.request(http.MethodGet, e.sourcePath("poll.bad"), nil))
	status := sourceResObj(t, proj["status"])
	if status["cursor"] != `"e1"` {
		t.Fatalf("失败轮推进了游标：%v", status["cursor"])
	}
	if status["consecutiveFailures"] != float64(1) {
		t.Fatalf("失败事实没落库：%v", status)
	}
}
