package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

type fixture struct {
	db   *store.DB
	ns   *store.Namespace
	svc  *Service
	now  time.Time
	runs *Poller
}

func newFixture(t *testing.T, cfg string) *fixture {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/poll.db")
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	ns, err := db.CreateNamespace(ctx, "default", "Default")
	if err != nil {
		t.Fatalf("建命名空间: %v", err)
	}
	if err := db.CreateCapability(ctx, &store.CapabilityVersion{
		NamespaceID: ns.ID, Name: "analyze", Version: 1,
		Spec: artifact.Spec{
			Params: json.RawMessage(`{"type":"object"}`),
			Run:    artifact.Run{Entrypoint: "run.sh", Files: map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"}},
		},
	}); err != nil {
		t.Fatalf("建能力: %v", err)
	}
	if err := db.CreateSource(ctx, &store.Source{
		NamespaceID: ns.ID, Name: "poll-1", Kind: sourcespec.KindPoll,
		Config:  json.RawMessage(cfg),
		Targets: []sourcespec.Target{{Capability: "analyze"}},
		Enabled: true,
	}); err != nil {
		t.Fatalf("建 poll 源: %v", err)
	}
	svc := New(db, route.New(db))
	f := &fixture{db: db, ns: ns, svc: svc, now: time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)}
	if next := f.source(t).NextFireAt; next != nil {
		f.now = time.Unix(*next, 0)
	}
	f.runs = NewPoller(db, svc, nil, WithClock(func() time.Time { return f.now }))
	return f
}

func (f *fixture) source(t *testing.T) *store.Source {
	t.Helper()
	src, err := f.db.GetSourceByName(context.Background(), f.ns.ID, "poll-1")
	if err != nil {
		t.Fatalf("回读源: %v", err)
	}
	return src
}

func (f *fixture) round(t *testing.T) {
	t.Helper()
	src := f.source(t)
	f.runs.round(context.Background(), src)
}

func (f *fixture) counts(t *testing.T) (events, tasks int) {
	t.Helper()
	for table, dst := range map[string]*int{"events": &events, "tasks": &tasks} {
		if err := f.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(dst); err != nil {
			t.Fatalf("统计 %s: %v", table, err)
		}
	}
	return events, tasks
}

func pollBody(ids ...string) string {
	items := make([]string, 0, len(ids))
	for _, id := range ids {
		items = append(items, fmt.Sprintf(`{"id":%q,"type":"advisory","title":"t-%s"}`, id, id))
	}
	return `[` + strings.Join(items, ",") + `]`
}

const pollEnvelopeCfg = `{"url":%q,"schedule":{"expr":"*/5 * * * *"},"allowInsecure":true,"maxItems":10,
	"envelope":{"items":".","type":{"pointer":"type"},"deliveryId":{"pointer":"id"},"payload":{"pointer":""}}}`

func TestPollRoundIngestsThenDedupes(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(pollBody("a", "b", "c")))
	}))
	defer server.Close()

	f := newFixture(t, fmt.Sprintf(pollEnvelopeCfg, server.URL))
	f.round(t)
	if events, tasks := f.counts(t); events != 3 || tasks != 3 {
		t.Fatalf("第一轮 events/tasks = %d/%d，want 3/3", events, tasks)
	}
	src := f.source(t)
	if src.ConsecutiveFailures != 0 || src.LastError != "" || src.LastPollAt == nil {
		t.Fatalf("成功后状态不对: %+v", src)
	}
	f.now = f.now.Add(5 * time.Minute)
	f.round(t)
	if events, tasks := f.counts(t); events != 3 || tasks != 3 {
		t.Fatalf("第二轮不建新任务：events/tasks = %d/%d，want 3/3（duplicate 3）", events, tasks)
	}
	if hits != 2 {
		t.Fatalf("第二轮应真的再抓一次（无 ETag → 无 304），hits=%d", hits)
	}
	if got := f.source(t).PollCursor; got != "" {
		t.Fatalf("响应没有 ETag / Last-Modified 时游标应保持为空，得到 %q", got)
	}
}

func TestPollCursorSurvivesRestartAndSends304(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("If-None-Match"))
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(pollBody("a")))
	}))
	defer server.Close()
	f := newFixture(t, fmt.Sprintf(pollEnvelopeCfg, server.URL))
	f.round(t)
	if got := f.source(t).PollCursor; got != `"v1"` {
		t.Fatalf("成功后游标应前进到 ETag，得到 %q", got)
	}
	restarted := NewPoller(f.db, f.svc, nil, WithClock(func() time.Time { return f.now.Add(5 * time.Minute) }))
	f.runs = restarted
	restarted.round(context.Background(), f.source(t))
	if len(seen) != 2 || seen[1] != `"v1"` {
		t.Fatalf("重启后的下一轮必须带上游标（If-None-Match），实际请求头 = %v", seen)
	}
	if events, tasks := f.counts(t); events != 1 || tasks != 1 {
		t.Fatalf("304 = 成功的空轮，不该新建事件/任务：%d/%d", events, tasks)
	}
	src := f.source(t)
	if src.ConsecutiveFailures != 0 || src.PollCursor != `"v1"` || src.LastPollAt == nil {
		t.Fatalf("304 必须算成功且游标不动: %+v", src)
	}
}

func TestPollFailureKeepsCursorAndBacksOff(t *testing.T) {
	var fail bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail {
			w.Header().Set("ETag", `"v2"`)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(pollBody("a")))
	}))
	defer server.Close()
	f := newFixture(t, fmt.Sprintf(pollEnvelopeCfg, server.URL))
	f.round(t)
	base := int64(5 * 60)
	fail = true

	f.now = f.now.Add(5 * time.Minute)
	f.round(t)
	src := f.source(t)
	if src.ConsecutiveFailures != 1 || src.PollCursor != `"v1"` || src.LastError == "" {
		t.Fatalf("失败必须留痕且不动游标: %+v", src)
	}
	if src.NextFireAt == nil {
		t.Fatal("失败后必须有 next_fire_at")
	}
	firstGap := *src.NextFireAt - f.now.Unix() // 抖动符号随命名空间 UUID 变 ⇒ 只钉 ±10% 区间（钉「≥ base」会按建库 UUID 半数概率红）
	if firstGap < base*9/10 || firstGap > base*11/10 {
		t.Fatalf("第一次失败后应等 base±10%%（%ds），实际 %ds（now=%d）", base, firstGap, f.now.Unix())
	}

	f.now = time.Unix(*f.source(t).NextFireAt, 0)
	f.round(t)
	src = f.source(t)
	if src.ConsecutiveFailures != 2 {
		t.Fatalf("第二次失败后 consecutiveFailures = %d，want 2", src.ConsecutiveFailures)
	}
	if gap := *src.NextFireAt - f.now.Unix(); gap < 2*firstGap-2 || gap > 2*firstGap+2 {
		t.Fatalf("第二次失败后应 ≈ base*2（%ds±2s，同一源抖动恒定只缩放不改倍率），实际 %ds", 2*firstGap, gap)
	}

	retry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer retry.Close()
	f2 := newFixture(t, fmt.Sprintf(pollEnvelopeCfg, retry.URL))
	f2.round(t)
	src2 := f2.source(t)
	if src2.ConsecutiveFailures != 1 {
		t.Fatalf("429 必须记失败: %+v", src2)
	}
	if gap := *src2.NextFireAt - f2.now.Unix(); gap != 7 {
		t.Fatalf("429 + Retry-After: 7 → 下一轮间隔 7s，实际 %ds", gap)
	}

	fail = false
	f.now = time.Unix(*f.source(t).NextFireAt, 0)
	f.round(t)
	src = f.source(t)
	if src.ConsecutiveFailures != 0 || src.LastError != "" {
		t.Fatalf("成功后失败计数与 lastError 必须清零: %+v", src)
	}
	if src.LastFiredAt == nil || *src.LastFiredAt != f.now.Unix() {
		t.Fatalf("last_fired_at 只在成功轮推进: %v", src.LastFiredAt)
	}
}

func TestPollOverlapSkippedNotQueued(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte(pollBody("a")))
	}))
	defer server.Close()
	f := newFixture(t, fmt.Sprintf(pollEnvelopeCfg, server.URL))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = f.runs.Run(ctx, 20*time.Millisecond) }()

	if !waitFor(3*time.Second, func() bool { return f.source(t).SkippedOverlaps >= 1 }) {
		t.Fatalf("上一轮挂着时必须记 skippedOverlaps，实际 %d", f.source(t).SkippedOverlaps)
	}
	once.Do(func() { close(release) })
	if !waitFor(3*time.Second, func() bool { _, tasks := f.counts(t); return tasks == 1 }) {
		t.Fatal("放行后应建出 1 个任务")
	}
	cancel()
	if _, tasks := f.counts(t); tasks != 1 {
		t.Fatalf("跳过的 tick 不该排队重放：tasks=%d want 1", tasks)
	}
}

func TestPollRuntimeBlocksResolvedPrivateIP(t *testing.T) {
	strict := `{"url":%q,"schedule":{"expr":"*/5 * * * *"},
		"envelope":{"items":".","type":{"pointer":"type"},"deliveryId":{"pointer":"id"}}}`
	f := newFixture(t, fmt.Sprintf(strict, "https://rebind.example.test/feed"))
	f.runs = NewPoller(f.db, f.svc, nil,
		WithClock(func() time.Time { return f.now }),
		WithResolver(func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}))
	f.round(t)
	src := f.source(t)
	if src.ConsecutiveFailures != 1 || !strings.Contains(src.LastError, "解析到私网地址") {
		t.Fatalf("解析到私网地址必须在运行期拒: %+v", src)
	}
	if src.PollCursor != "" {
		t.Fatalf("失败不该有游标: %+v", src)
	}
}

func TestPollLimitsFailLoudly(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		padding := strings.Repeat("x", PollMaxBodyBytes+16)
		_, _ = w.Write([]byte(`[{"id":"a","type":"t","blob":"` + padding + `"}]`))
	}))
	defer big.Close()
	f := newFixture(t, fmt.Sprintf(pollEnvelopeCfg, big.URL))
	f.round(t)
	if src := f.source(t); !strings.Contains(src.LastError, "超过上限") {
		t.Fatalf("超 4 MiB 必须失败并留痕: %+v", src)
	}

	many := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(pollBody("a", "b", "c", "d")))
	}))
	defer many.Close()
	cfg := `{"url":%q,"schedule":{"expr":"*/5 * * * *"},"allowInsecure":true,"maxItems":3,
		"envelope":{"items":".","type":{"pointer":"type"},"deliveryId":{"pointer":"id"}}}`
	f2 := newFixture(t, fmt.Sprintf(cfg, many.URL))
	f2.round(t)
	if src := f2.source(t); !strings.Contains(src.LastError, "maxItems") {
		t.Fatalf("item 超 maxItems 必须失败并留痕: %+v", src)
	}
	if events, tasks := f2.counts(t); events != 0 || tasks != 0 {
		t.Fatalf("超限那一轮不该部分入库：events/tasks = %d/%d", events, tasks)
	}

	notArray := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"items":[]}}`))
	}))
	defer notArray.Close()
	f3 := newFixture(t, fmt.Sprintf(cfg, notArray.URL))
	f3.round(t)
	if src := f3.source(t); !strings.Contains(src.LastError, "不是数组") {
		t.Fatalf("items 取不到数组 = 该轮失败（不是 0 条）: %+v", src)
	}
}

func TestPollSecretRefResolvesCredential(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(pollBody("a")))
	}))
	defer server.Close()
	cfg := `{"url":%q,"schedule":{"expr":"*/5 * * * *"},"allowInsecure":true,
		"secretRef":{"name":"gh-token","header":"Authorization","prefix":"Bearer "},
		"secret":"poll-fixture-token",
		"envelope":{"items":".","deliveryId":{"pointer":"id"}}}`
	f := newFixture(t, fmt.Sprintf(cfg, server.URL))
	src := f.source(t)
	if strings.Contains(string(src.Config), "poll-fixture-token") || !strings.Contains(string(src.Config), "gh-token") {
		t.Fatalf("config 只许有引用、不许有值: %s", src.Config)
	}
	f.round(t)
	if gotAuth != "Bearer poll-fixture-token" {
		t.Fatalf("运行期必须按 secretRef 注入头，实际 %q", gotAuth)
	}

	missing := `{"url":%q,"schedule":{"expr":"*/5 * * * *"},"allowInsecure":true,
		"secretRef":{"name":"nope","header":"Authorization"}}`
	f2 := newFixture(t, `{"url":"https://example.com/x","schedule":{"expr":"*/5 * * * *"}}`)
	err := f2.db.CreateSource(context.Background(), &store.Source{
		NamespaceID: f2.ns.ID, Name: "poll-missing", Kind: sourcespec.KindPoll,
		Config:  json.RawMessage(fmt.Sprintf(missing, server.URL)),
		Targets: []sourcespec.Target{{Capability: "analyze"}},
		Enabled: true,
	})
	if err == nil || !strings.Contains(err.Error(), "config.secretRef") {
		t.Fatalf("缺失凭据引用必须写入期 400（cause 带 config.secretRef）: %v", err)
	}
}

func TestBlockPrivateDialGuardsActualIP(t *testing.T) {
	for _, address := range []string{"127.0.0.1:80", "169.254.169.254:80", "10.1.2.3:443", "[fd00::1]:443", "[::1]:443"} {
		if err := blockPrivateDial("tcp", address, nil); err == nil {
			t.Errorf("dial 到 %s 必须被拒（DNS rebinding 的第二道闸）", address)
		}
	}
	if err := blockPrivateDial("tcp", "93.184.216.34:443", nil); err != nil {
		t.Errorf("公网地址必须放行: %v", err)
	}
}

func TestSameHostRedirectPolicy(t *testing.T) {
	req := func(host string) *http.Request {
		return &http.Request{URL: &url.URL{Scheme: "https", Host: host}}
	}
	if err := sameHostRedirect(req("api.example.com"), []*http.Request{req("api.example.com")}); err != nil {
		t.Fatalf("同主机重定向应放行: %v", err)
	}
	if err := sameHostRedirect(req("evil.example.com"), []*http.Request{req("api.example.com")}); err == nil {
		t.Fatal("跨主机重定向必须拒绝")
	}
	four := []*http.Request{req("a"), req("a"), req("a"), req("a")}
	if err := sameHostRedirect(req("a"), four); err == nil {
		t.Fatal("超过 3 跳必须拒绝")
	}
}

func waitFor(budget time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		<-time.After(10 * time.Millisecond)
	}
	return cond()
}

func TestPollBackoffIsMonotonicRegardlessOfPhase(t *testing.T) {
	runs := NewPoller(nil, nil, nil)
	capGap := int64((30 * time.Minute).Seconds())
	const interval = int64(60) // * * * * * → 相邻两格之差
	cfg := json.RawMessage(`{"url":"https://example.test/feed","schedule":{"expr":"* * * * *"}}`)
	minute := time.Date(2026, 11, 22, 0, 13, 0, 0, time.UTC)
	sources := []string{"feed", "feed-00", "feed-01", "feed-07", "feed-19", "feed-31"} // 覆盖抖动为正/为负两档：只测 p=0 的曲线时，抖动写错（甚至整行删掉）也验不出来
	for _, name := range sources {
		src := &store.Source{NamespaceID: "ns-633", Name: name, Config: cfg}
		gaps := make(map[int][]int64, 2)
		for _, second := range []int{2, 58} {
			now := minute.Add(time.Duration(second) * time.Second)
			for failures := 1; failures <= 6; failures++ {
				gap := runs.failureNextFire(src, now, failures, 0) - now.Unix()
				base := interval << (failures - 1)
				if base > capGap {
					base = capGap // 抖动作用在封顶后的基数上，不是翻倍后的原始值
				}
				lo, hi := base-base/10, base+base/10
				if hi > capGap {
					hi = capGap
				}
				prev := gaps[second]
				gaps[second] = append(prev, gap)
				if gap < lo || gap > hi {
					t.Fatalf("%s @xx:xx:%02d 第 %d 次失败等了 %ds，want ∈ [%d,%d]（基数 %ds ±10%%，封顶 %ds）",
						name, second, failures, gap, lo, hi, base, capGap)
				}
				if failures > 1 {
					last := prev[failures-2]
					if gap < last {
						t.Fatalf("%s @xx:xx:%02d 的退避不单调：第 %d 次 %ds < 第 %d 次 %ds",
							name, second, failures, gap, failures-1, last)
					}
					if base < capGap && (gap < 2*last-2 || gap > 2*last+2) {
						t.Fatalf("%s @xx:xx:%02d 第 %d 次失败 %ds，want 上次（%ds）的 2 倍（抖动按源恒定只缩放不改倍率；落库取整到秒容忍 ±2s）",
							name, second, failures, gap, last)
					}
				}
			}
		}
		for i, gap := range gaps[2] {
			if gaps[58][i] != gap {
				t.Fatalf("%s 退避不该取决于失败发生在哪一秒：第 %d 次 %ds（:02）vs %ds（:58）",
					name, i+1, gap, gaps[58][i])
			}
		}
	}
}

func TestFailureNextFireJitterIsPerSourceSymmetricWithin10Percent(t *testing.T) {
	runs := NewPoller(nil, nil, nil)
	now := time.Date(2026, 11, 22, 0, 13, 30, 0, time.UTC)
	cfg := json.RawMessage(`{"url":"https://example.test/feed","schedule":{"expr":"*/5 * * * *"}}`)
	const base = int64(300) // */5 → 基数 300s
	var below, above int
	distinct := map[int64]string{}
	for i := 0; i < 40; i++ {
		src := &store.Source{NamespaceID: "ns-633", Name: fmt.Sprintf("feed-%02d", i), Config: cfg}
		gap := runs.failureNextFire(src, now, 1, 0) - now.Unix()
		if gap < base*9/10 || gap > base*11/10 {
			t.Fatalf("%s: 抖动越界：%ds，want ∈ [%d,%d]", src.Name, gap, base*9/10, base*11/10)
		}
		if again := runs.failureNextFire(src, now, 1, 0) - now.Unix(); again != gap {
			t.Fatalf("%s: 同源两次算出 %ds / %ds——抖动必须按源确定，否则排期与测试都在抖", src.Name, gap, again)
		}
		distinct[gap] = src.Name
		switch {
		case gap < base:
			below++
		case gap > base:
			above++
		}
	}
	if len(distinct) < 2 {
		t.Fatalf("40 个源算出同一个间隔（%v）：抖动没生效", distinct)
	}
	if below == 0 || above == 0 {
		t.Fatalf("抖动应关于基数对称（允许负偏差）：低于 base 的源 %d 个、高于的 %d 个", below, above)
	}
}

func TestFailureNextFireJitterSurvivesCap(t *testing.T) {
	runs := NewPoller(nil, nil, nil)
	now := time.Date(2026, 11, 22, 0, 13, 30, 0, time.UTC)
	cfg := json.RawMessage(`{"url":"https://example.test/feed","schedule":{"expr":"*/5 * * * *"}}`)
	capGap := int64((30 * time.Minute).Seconds())
	distinct := map[int64]string{}
	for i := 0; i < 40; i++ {
		src := &store.Source{NamespaceID: "ns-633", Name: fmt.Sprintf("feed-%02d", i), Config: cfg}
		gap := runs.failureNextFire(src, now, 12, 0) - now.Unix() // 300s × 2^11 ≫ 封顶
		if gap > capGap {
			t.Fatalf("%s: 30min 是硬上限，实际 %ds > %ds", src.Name, gap, capGap)
		}
		if gap < capGap*9/10 {
			t.Fatalf("%s: 封顶档的抖动越界：%ds < %ds", src.Name, gap, capGap*9/10)
		}
		distinct[gap] = src.Name
	}
	if len(distinct) < 2 {
		t.Fatalf("封顶档 %d 个源全等（%v）：抖动被第二次 capDelay 裁掉了", 40, distinct)
	}
}

func TestFailureNextFireRetryAfterSkipsJitter(t *testing.T) {
	runs := NewPoller(nil, nil, nil)
	now := time.Date(2026, 11, 22, 0, 13, 30, 0, time.UTC)
	cfg := json.RawMessage(`{"url":"https://example.test/feed","schedule":{"expr":"*/5 * * * *"}}`)
	for i := 0; i < 10; i++ {
		src := &store.Source{NamespaceID: "ns-633", Name: fmt.Sprintf("feed-%02d", i), Config: cfg}
		if gap := runs.failureNextFire(src, now, 3, 7*time.Second) - now.Unix(); gap != 7 {
			t.Fatalf("%s: Retry-After 是上游口径，不该叠抖动：%ds want 7s", src.Name, gap)
		}
	}
}

func TestPollLastModifiedCursorUsesIfModifiedSince(t *testing.T) {
	const modified = "Wed, 25 Nov 2026 00:00:00 GMT"
	var noneMatch, ifModifiedSince []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noneMatch = append(noneMatch, r.Header.Get("If-None-Match"))
		ifModifiedSince = append(ifModifiedSince, r.Header.Get("If-Modified-Since"))
		if r.Header.Get("If-Modified-Since") == modified {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Last-Modified", modified)
		_, _ = w.Write([]byte(pollBody("a")))
	}))
	defer server.Close()

	f := newFixture(t, fmt.Sprintf(pollEnvelopeCfg, server.URL))
	f.round(t)
	if got := f.source(t).PollCursor; got != modified {
		t.Fatalf("没有 ETag 时游标应记 Last-Modified，得到 %q", got)
	}
	f.now = f.now.Add(5 * time.Minute)
	f.round(t)
	if len(ifModifiedSince) != 2 || ifModifiedSince[1] != modified {
		t.Fatalf("第二轮必须以 If-Modified-Since 发回游标，实际 %v", ifModifiedSince)
	}
	if noneMatch[1] != "" {
		t.Fatalf("Last-Modified 型游标不能进 If-None-Match（永远打不中），实际 %q", noneMatch[1])
	}
	if events, tasks := f.counts(t); events != 1 || tasks != 1 {
		t.Fatalf("304 = 成功的空轮，必须零入库：events/tasks = %d/%d", events, tasks)
	}
	src := f.source(t)
	if src.ConsecutiveFailures != 0 || src.PollCursor != modified || src.LastError != "" {
		t.Fatalf("304 必须算成功且游标不动: %+v", src)
	}
}

func TestSetConditionalPicksHeaderByCursorShape(t *testing.T) {
	cases := []struct{ cursor, wantHeader, wantValue string }{
		{`"v1"`, "If-None-Match", `"v1"`},
		{`W/"v1"`, "If-None-Match", `W/"v1"`},
		{"Sun, 06 Nov 1994 08:49:37 GMT", "If-Modified-Since", "Sun, 06 Nov 1994 08:49:37 GMT"},
		{"Sunday, 06-Nov-94 08:49:37 GMT", "If-Modified-Since", "Sunday, 06-Nov-94 08:49:37 GMT"},
		{"not-a-validator", "If-None-Match", "not-a-validator"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "https://example.test/feed", nil)
		setConditional(req, tc.cursor)
		if got := req.Header.Get(tc.wantHeader); got != tc.wantValue {
			t.Fatalf("游标 %q：%s = %q，want %q", tc.cursor, tc.wantHeader, got, tc.wantValue)
		}
		other := "If-None-Match"
		if tc.wantHeader == "If-None-Match" {
			other = "If-Modified-Since"
		}
		if got := req.Header.Get(other); got != "" {
			t.Fatalf("游标 %q 不该同时发 %s = %q", tc.cursor, other, got)
		}
	}
	empty := httptest.NewRequest(http.MethodGet, "https://example.test/feed", nil)
	setConditional(empty, "")
	if empty.Header.Get("If-None-Match") != "" || empty.Header.Get("If-Modified-Since") != "" {
		t.Fatalf("空游标不该发条件请求头: %v", empty.Header)
	}
}
