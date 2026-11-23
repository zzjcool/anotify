package source

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pollNowCfg 的 envelope.items 指向响应对象的 `items` 键（本文件要能同时造「对」与「写错指针」的响应）。
const pollNowCfg = `{"url":%q,"schedule":{"expr":"*/5 * * * *"},"allowInsecure":true,"maxItems":10,
	"envelope":{"items":".items","type":{"pointer":"type"},"deliveryId":{"pointer":"id"},"payload":{"pointer":""}}}`

func pollWrapped(ids ...string) string { return `{"items":` + pollBody(ids...) + `}` }

// TestPollOnceReportsCountsAndDedupes 首轮全新建，同 deliveryId 的第二轮全命中去重窗口。
func TestPollOnceReportsCountsAndDedupes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"e1"`)
		_, _ = w.Write([]byte(pollWrapped("a", "b")))
	}))
	defer server.Close()

	f := newFixture(t, fmt.Sprintf(pollNowCfg, server.URL))
	ctx := context.Background()

	res, err := f.runs.PollOnce(ctx, f.source(t))
	if err != nil {
		t.Fatalf("首轮立即拉取失败: %v", err)
	}
	if res.Items != 2 || res.Created != 2 || res.Duplicates != 0 {
		t.Fatalf("首轮计数 = items %d / created %d / duplicates %d，want 2/2/0", res.Items, res.Created, res.Duplicates)
	}
	if !res.CursorAdvanced || res.Cursor != `"e1"` {
		t.Fatalf("首轮游标 = %q（advanced=%v），want \"e1\" 且前进", res.Cursor, res.CursorAdvanced)
	}
	if events, tasks := f.counts(t); events != 2 || tasks != 2 {
		t.Fatalf("首轮入库 events/tasks = %d/%d，want 2/2", events, tasks)
	}

	again, err := f.runs.PollOnce(ctx, f.source(t))
	if err != nil {
		t.Fatalf("第二轮立即拉取失败: %v", err)
	}
	if again.Items != 2 || again.Created != 0 || again.Duplicates != 2 {
		t.Fatalf("第二轮计数 = items %d / created %d / duplicates %d，want 2/0/2", again.Items, again.Created, again.Duplicates)
	}
	if again.CursorAdvanced {
		t.Fatalf("同一份 ETag 的第二轮不该报告游标前进（cursor=%q）", again.Cursor)
	}
	if events, _ := f.counts(t); events != 2 {
		t.Fatalf("去重后的 events = %d，want 2", events)
	}
}

// TestPollOnceFailureKeepsCursorAndSchedule 失败不前进游标，且 nextFireAt 与触发前逐字相同。
func TestPollOnceFailureKeepsCursorAndSchedule(t *testing.T) {
	var round2 atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if round2.Load() {
			// 合法 JSON + 新 ETag，但 envelope.items 指的地方不是数组 → 本轮失败。
			w.Header().Set("ETag", `"e2"`)
			_, _ = w.Write([]byte(`{"feed":{"items":[]}}`))
			return
		}
		w.Header().Set("ETag", `"e1"`)
		_, _ = w.Write([]byte(pollWrapped("a")))
	}))
	defer server.Close()

	f := newFixture(t, fmt.Sprintf(pollNowCfg, server.URL))
	ctx := context.Background()
	if _, err := f.runs.PollOnce(ctx, f.source(t)); err != nil {
		t.Fatalf("首轮成功轮: %v", err)
	}
	before := f.source(t)
	if before.PollCursor != `"e1"` || before.NextFireAt == nil {
		t.Fatalf("首轮后的事实不对: cursor=%q nextFireAt=%v", before.PollCursor, before.NextFireAt)
	}

	round2.Store(true)
	res, err := f.runs.PollOnce(ctx, f.source(t))
	if err == nil {
		t.Fatalf("envelope.items 写错必须让本轮失败，实际成功（res=%+v）", res)
	}
	after := f.source(t)
	if after.PollCursor != before.PollCursor {
		t.Fatalf("失败轮把游标推进了：%q → %q（失败不前进游标）", before.PollCursor, after.PollCursor)
	}
	if after.NextFireAt == nil || *after.NextFireAt != *before.NextFireAt {
		t.Fatalf("手动拉取改了排期：%v → %v（不重置 nextFireAt）", before.NextFireAt, after.NextFireAt)
	}
	if after.ConsecutiveFailures != 1 || after.LastError == "" {
		t.Fatalf("失败事实没落库: failures=%d lastError=%q", after.ConsecutiveFailures, after.LastError)
	}
}

// TestPollOnceAndSweepShareOneLock 手动与定时共用同一把锁：在飞时第二次触发拿 ErrPollInFlight。
func TestPollOnceAndSweepShareOneLock(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseNow := func() { releaseOnce.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 只挂第一轮：后续请求立刻返回，绕过锁的变异才会当场得到错误结论而不是把测试挂死。
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(pollWrapped("a")))
	}))
	defer server.Close()
	// 放行的 defer 必须比 server.Close 更晚注册（LIFO 先跑它）：否则断言失败时 Close 会等那条挂住的连接。
	defer releaseNow()

	f := newFixture(t, fmt.Sprintf(pollNowCfg, server.URL))
	ctx := context.Background()
	first := make(chan error, 1)
	go func() {
		_, err := f.runs.PollOnce(ctx, f.source(t))
		first <- err
	}()
	<-started

	if _, err := f.runs.PollOnce(ctx, f.source(t)); !errors.Is(err, ErrPollInFlight) {
		t.Fatalf("在飞时的第二次手动拉取 = %v，want ErrPollInFlight", err)
	}
	if started, err := f.runs.Sweep(ctx); err != nil || started != 0 {
		t.Fatalf("在飞时的 Sweep = (%d, %v)，want (0, nil)", started, err)
	}
	if got := f.source(t).SkippedOverlaps; got != 1 {
		t.Fatalf("重叠 tick 的 skippedOverlaps = %d，want 1", got)
	}

	releaseNow()
	if err := <-first; err != nil {
		t.Fatalf("放行后的第一轮应该成功: %v", err)
	}
}

// TestPollOnceNotModifiedKeepsCursor 304：计数全 0、游标不动，但整轮仍算成功。
func TestPollOnceNotModifiedKeepsCursor(t *testing.T) {
	var tagged atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if tagged.Load() && r.Header.Get("If-None-Match") == `"e1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"e1"`)
		_, _ = w.Write([]byte(pollWrapped("a")))
	}))
	defer server.Close()

	f := newFixture(t, fmt.Sprintf(pollNowCfg, server.URL))
	ctx := context.Background()
	if _, err := f.runs.PollOnce(ctx, f.source(t)); err != nil {
		t.Fatalf("首轮: %v", err)
	}
	tagged.Store(true)
	res, err := f.runs.PollOnce(ctx, f.source(t))
	if err != nil {
		t.Fatalf("304 不该算失败: %v", err)
	}
	if !res.NotModified || res.Items != 0 || res.Created != 0 || res.Duplicates != 0 || res.CursorAdvanced {
		t.Fatalf("304 的计数/游标 = %+v，want 全 0 且没前进", res)
	}
	if got := f.source(t).PollCursor; got != `"e1"` {
		t.Fatalf("304 后游标 = %q，want \"e1\"", got)
	}
}

// TestPollOnceRefusesPrivateHostAtRuntime 手动路径不是绕过口：私网同样是一轮失败（游标不动）。
func TestPollOnceRefusesPrivateHostAtRuntime(t *testing.T) {
	strict := `{"url":%q,"schedule":{"expr":"*/5 * * * *"},
		"envelope":{"items":".","type":{"pointer":"type"},"deliveryId":{"pointer":"id"}}}`
	f := newFixture(t, fmt.Sprintf(strict, "https://rebind.example.test/feed"))
	f.runs = NewPoller(f.db, f.svc, nil,
		WithClock(func() time.Time { return f.now }),
		WithResolver(func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}))
	ctx := context.Background()
	_, err := f.runs.PollOnce(ctx, f.source(t))
	if err == nil {
		t.Fatal("手动拉取解析到私网地址必须在运行期被拒")
	}
	src := f.source(t)
	if src.ConsecutiveFailures != 1 || !strings.Contains(src.LastError, "解析到私网地址") {
		t.Fatalf("私网拒绝没落成失败事实: %+v", src)
	}
	if src.PollCursor != "" {
		t.Fatalf("被拒的一轮不该有游标: %q", src.PollCursor)
	}
	if events, _ := f.counts(t); events != 0 {
		t.Fatalf("被拒的一轮不该产生事件: %d", events)
	}
}
