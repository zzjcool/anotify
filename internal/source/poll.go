package source

import (
	"context"
	"fmt"
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

// Poller 是唯一的轮询执行器，靠进程内 in-flight 集合保证不重叠（单实例部署，无 leader 选举）。
//
// 三条不变量：① 游标只在整轮成功后前进（失败沿用旧游标，下一轮重抓同一批、靠 deliveryId
// 幂等去重）；② 上一轮没结束的 tick 只记 skippedOverlaps，不排队不并发；③ 失败退避
// min(base << (failures-1), 30min)，429 的 Retry-After 优先，成功清零。
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

func (r *Poller) round(ctx context.Context, src *store.Source) {
	started := r.now()
	cfg := sourcespec.ParseConfig(sourcespec.KindPoll, src.Config)
	out := store.PollOutcome{PolledAt: started.Unix(), Cursor: src.PollCursor}
	res, err := r.fetch(ctx, src, cfg)
	if err == nil && !res.notModified {
		var items []any
		if items, err = r.items(cfg, res.doc); err == nil {
			err = r.ingest(ctx, src, cfg, items)
		}
	}
	if err != nil {
		out.Failures = src.ConsecutiveFailures + 1
		out.Error = err.Error()
		out.NextFire = r.failureNextFire(src, started, out.Failures, res.retryAfter)
		r.log.Warn("poll failed", "source", src.Name, "namespace", src.NamespaceID, "failures", out.Failures, "err", err)
	} else {
		out.Success = true
		out.Cursor = res.cursor
		out.NextFire = r.successNextFire(src, started)
	}
	if err := r.db.MarkSourcePolled(ctx, src.ID, out); err != nil {
		r.log.Warn("poll status write failed", "source", src.Name, "err", err)
	}
}

func (r *Poller) successNextFire(src *store.Source, now time.Time) int64 {
	next, err := sourcespec.NextPollTime(src.Config, now)
	if err != nil || next.IsZero() {
		return now.Add(pollNeverFire).Unix()
	}
	return next.Unix()
}

func (r *Poller) failureNextFire(src *store.Source, now time.Time, failures int, retry time.Duration) int64 {
	delay := retry
	if delay <= 0 {
		base := pollBackoffCap
		if next, err := sourcespec.NextPollTime(src.Config, now); err == nil && next.After(now) {
			base = next.Sub(now)
		}
		if base <= 0 || base > pollBackoffCap {
			base = pollBackoffCap
		}
		delay = base
		for i := 1; i < failures && delay < pollBackoffCap; i++ {
			delay *= 2
		}
	}
	if delay > pollBackoffCap {
		delay = pollBackoffCap
	}
	return now.Add(delay).Unix()
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

func (r *Poller) ingest(ctx context.Context, src *store.Source, cfg sourcespec.Config, items []any) error {
	for i, item := range items {
		in, err := inputFor(src, cfg, item)
		if err != nil {
			return fmt.Errorf("poll: 第 %d 条 item 不合法: %w", i, err)
		}
		if _, err := r.services.Ingest(ctx, in); err != nil {
			return fmt.Errorf("poll: 第 %d 条 item 入站失败: %w", i, err)
		}
	}
	return nil
}
