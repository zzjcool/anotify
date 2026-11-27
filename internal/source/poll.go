package source

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"net/http"
	"sync"
	"time"

	"log/slog"

	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

const (
	PollDefaultTick  = 5 * time.Second
	PollDefaultBatch = 100
	pollBackoffCap   = 30 * time.Minute
	pollNeverFire    = 366 * 24 * time.Hour
)

// Poller 是唯一的轮询执行器（单实例、无 leader 选举），靠进程内 in-flight 集合保证不重叠。
// 三条不变量：① 游标只在整轮成功后前进（失败沿用旧游标，下一轮重抓同一批、靠 deliveryId 幂等去重）；
// ② 上一轮没结束的 tick 只记 skippedOverlaps，不排队不并发；③ 退避 min(间隔<<(failures-1), 30min) 叠 ±10% 抖动再裁到 30min，Retry-After 优先。
type Poller struct {
	db       *store.DB
	services *Service
	log      *slog.Logger
	now      func() time.Time
	resolve  resolverFunc
	strict   *http.Client
	insecure *http.Client

	mu       sync.Mutex
	inflight map[string]struct{}
}

type Option func(*Poller)

func WithClock(now func() time.Time) Option {
	return func(r *Poller) { r.now = now }
}

func WithResolver(fn func(ctx context.Context, host string) ([]net.IP, error)) Option {
	return func(r *Poller) { r.resolve = fn }
}

func NewPoller(db *store.DB, services *Service, log *slog.Logger, opts ...Option) *Poller {
	if log == nil {
		log = slog.Default()
	}
	r := &Poller{
		db:       db,
		services: services,
		log:      log,
		now:      time.Now,
		resolve:  defaultResolver,
		strict:   &http.Client{Transport: strictTransport(), CheckRedirect: sameHostRedirect},
		insecure: &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: sameHostRedirect},
		inflight: map[string]struct{}{},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func defaultResolver(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		out = append(out, addr.AsSlice())
	}
	return out, nil
}

func (r *Poller) Run(ctx context.Context, tick time.Duration) error {
	if tick <= 0 {
		tick = PollDefaultTick
	}
	timer := time.NewTicker(tick)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			if _, err := r.Sweep(ctx); err != nil {
				r.log.Warn("poll sweep failed", "err", err)
			}
		}
	}
}

// Sweep 取到点的 poll 源逐个起 goroutine：tick 不被慢源卡住，卡住的下个 tick 记 skippedOverlaps。
func (r *Poller) Sweep(ctx context.Context) (int, error) {
	sources, err := r.db.ListDueSourcesOfKind(ctx, store.SourceKindPoll, r.now().Unix(), PollDefaultBatch)
	if err != nil {
		return 0, err
	}
	started := 0
	for _, src := range sources {
		if !r.begin(src.ID) {
			if err := r.db.BumpSourceSkippedOverlaps(ctx, src.ID); err != nil {
				r.log.Warn("poll skip record failed", "source", src.Name, "err", err)
			}
			continue
		}
		started++
		go func(src *store.Source) {
			defer r.done(src.ID)
			r.round(ctx, src)
		}(src)
	}
	return started, nil
}

func (r *Poller) begin(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, busy := r.inflight[id]; busy {
		return false
	}
	r.inflight[id] = struct{}{}
	return true
}

func (r *Poller) done(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inflight, id)
}

// ErrPollInFlight 表示该源已有一轮在飞（调度器或另一次手动触发）：本轮未执行，也不排队。
var ErrPollInFlight = errors.New("source: 该源已有一轮在飞")

// PollResult 是一轮拉取的可观测事实（「立即拉取一次」的 200 回执就投影它）。
type PollResult struct {
	NotModified    bool
	Items          int
	Created        int
	Duplicates     int
	DurationMs     int64
	Cursor         string
	CursorAdvanced bool
}

// PollOnce 同步跑一轮「立即拉取一次」：**同一把** in-flight 锁（在飞 → ErrPollInFlight，
// 不排队）+ **同一份** roundOnce（本轮失败 → error，游标不动）。
func (r *Poller) PollOnce(ctx context.Context, src *store.Source) (PollResult, error) {
	if src == nil {
		return PollResult{}, fmt.Errorf("source: 缺少源行")
	}
	if !r.begin(src.ID) {
		return PollResult{}, ErrPollInFlight
	}
	defer r.done(src.ID)
	return r.roundOnce(ctx, src, true)
}

func (r *Poller) round(ctx context.Context, src *store.Source) {
	_, _ = r.roundOnce(ctx, src, false)
}

// roundOnce 是一轮拉取的**唯一实现**（定时轮询与「立即拉取一次」都走这里）；manual 只改
// 一件事：不写 next_fire_at（排期不被手动触发重置，成功与失败都是）。
func (r *Poller) roundOnce(ctx context.Context, src *store.Source, manual bool) (PollResult, error) {
	started := r.now()
	cfg := sourcespec.ParseConfig(sourcespec.KindPoll, src.Config)
	out := store.PollOutcome{PolledAt: started.Unix(), Cursor: src.PollCursor, KeepNextFire: manual}
	res := PollResult{}
	fetched, err := r.fetch(ctx, src, cfg)
	res.NotModified = fetched.notModified
	if err == nil && !fetched.notModified {
		var items []any
		if items, err = r.items(cfg, fetched.doc); err == nil {
			res.Items = len(items)
			res.Created, res.Duplicates, err = r.ingest(ctx, src, cfg, items)
		}
	}
	if err != nil {
		out.Failures = src.ConsecutiveFailures + 1
		out.Error = err.Error()
		out.NextFire = r.failureNextFire(src, started, out.Failures, fetched.retryAfter)
		r.log.Warn("poll failed", "source", src.Name, "namespace", src.NamespaceID, "failures", out.Failures, "err", err)
	} else {
		out.Success = true
		out.Cursor = fetched.cursor
		out.NextFire = r.successNextFire(src, started)
		res.Cursor = fetched.cursor
		res.CursorAdvanced = fetched.cursor != src.PollCursor
	}
	res.DurationMs = r.now().Sub(started).Milliseconds()
	if err := r.db.MarkSourcePolled(ctx, src.ID, out); err != nil {
		r.log.Warn("poll status write failed", "source", src.Name, "err", err)
	}
	return res, err
}

func (r *Poller) successNextFire(src *store.Source, now time.Time) int64 {
	next, err := sourcespec.NextPollTime(src.Config, now)
	if err != nil || next.IsZero() {
		return now.Add(pollNeverFire).Unix()
	}
	return next.Unix()
}

func (r *Poller) failureNextFire(src *store.Source, now time.Time, failures int, retry time.Duration) int64 {
	if retry > 0 {
		return now.Add(capDelay(retry)).Unix()
	}
	delay := pollInterval(src, now)
	for i := 1; i < failures && delay < pollBackoffCap; i++ {
		delay *= 2
	}
	delay = capDelay(delay) // 30min 先作用于基数，否则失败次数继续翻倍后 ±10% 也拉不回封顶以内（抖动在封顶档全被裁掉）
	delay += time.Duration(int64(delay) * pollJitterPermille(src) / 1000)
	return now.Add(capDelay(delay)).Unix()
}

func capDelay(d time.Duration) time.Duration {
	if d <= 0 || d > pollBackoffCap {
		return pollBackoffCap
	}
	return d
}

func pollInterval(src *store.Source, now time.Time) time.Duration {
	next, err := sourcespec.NextPollTime(src.Config, now)
	if err != nil || !next.After(now) {
		return pollBackoffCap
	}
	after, err := sourcespec.NextPollTime(src.Config, next)
	if err != nil || !after.After(next) {
		return pollBackoffCap
	}
	return capDelay(after.Sub(next))
}

// pollJitterPermille：按「命名空间+源名」确定的 ±10%（-100..+100‰）；同源恒定 ⇒ 等比曲线的倍率不变（单调性不破），异源错开。
func pollJitterPermille(src *store.Source) int64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(src.NamespaceID + "\x00" + src.Name))
	return int64(h.Sum32()%201) - 100
}

func (r *Poller) items(cfg sourcespec.Config, doc any) ([]any, error) {
	path := "."
	if cfg.Envelope != nil && cfg.Envelope.Items != "" {
		path = cfg.Envelope.Items
	}
	items, ok := sourcespec.LookupItems(doc, path)
	if !ok {
		return nil, fmt.Errorf("poll: 响应里的 %q 不是数组（envelope.items 写错，或该轮没有事件）", path)
	}
	if len(items) > cfg.MaxItems {
		return nil, fmt.Errorf("poll: item 数 %d 超过 maxItems %d（不截断，本轮失败）", len(items), cfg.MaxItems)
	}
	return items, nil
}

func (r *Poller) ingest(ctx context.Context, src *store.Source, cfg sourcespec.Config, items []any) (created, duplicates int, err error) {
	for i, item := range items {
		in, err := inputFor(src, cfg, item)
		if err != nil {
			return created, duplicates, fmt.Errorf("poll: 第 %d 条 item 不合法: %w", i, err)
		}
		receipt, err := r.services.Ingest(ctx, in)
		if err != nil {
			return created, duplicates, fmt.Errorf("poll: 第 %d 条 item 入站失败: %w", i, err)
		}
		if receipt != nil && receipt.Duplicate { // 去重口径以入库回执为准
			duplicates++
		} else {
			created++
		}
	}
	return created, duplicates, nil
}
