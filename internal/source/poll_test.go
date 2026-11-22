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
	if src.NextFireAt == nil || *src.NextFireAt < f.now.Add(time.Duration(base)*time.Second).Unix() {
		t.Fatalf("第一次失败后应等 base（%ds），next_fire_at=%v now=%d", base, src.NextFireAt, f.now.Unix())
	}
	firstGap := *src.NextFireAt - f.now.Unix()

	f.now = time.Unix(*f.source(t).NextFireAt, 0)
	f.round(t)
	src = f.source(t)
	if src.ConsecutiveFailures != 2 {
		t.Fatalf("第二次失败后 consecutiveFailures = %d，want 2", src.ConsecutiveFailures)
	}
	if gap := *src.NextFireAt - f.now.Unix(); gap < 2*firstGap {
		t.Fatalf("第二次失败后应 ≥ base*2（%ds），实际 %ds", 2*firstGap, gap)
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

	time.Sleep(300 * time.Millisecond)
	src := f.source(t)
	if src.SkippedOverlaps < 1 {
		t.Fatalf("上一轮挂着时必须记 skippedOverlaps，实际 %d", src.SkippedOverlaps)
	}
	once.Do(func() { close(release) })
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, tasks := f.counts(t); tasks == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
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
