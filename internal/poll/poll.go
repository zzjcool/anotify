// Package poll 执行 kind=poll 源的轮询：抓取 → 逐条入站 → 写回游标/退避/跳过计数。
package poll

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"log/slog"

	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

const (
	DefaultTick      = 5 * time.Second
	DefaultBatch     = 100
	backoffCap       = 30 * time.Minute
	neverFireHorizon = 366 * 24 * time.Hour
)

// Runner 是唯一的轮询执行器（一个进程一份）。in-flight 集合只存活在内存里：
// 不重叠是**进程内**纪律（本仓没有 leader 选举，单实例部署）。
type Runner struct {
	db       *store.DB
	sources  *source.Service
	log      *slog.Logger
	now      func() time.Time
	resolve  resolverFunc
	strict   *http.Client
	insecure *http.Client

	mu       sync.Mutex
	inflight map[string]struct{}
}

type Option func(*Runner)

func WithClock(now func() time.Time) Option {
	return func(r *Runner) { r.now = now }
}

func WithResolver(fn func(ctx context.Context, host string) ([]net.IP, error)) Option {
	return func(r *Runner) { r.resolve = fn }
}

func New(db *store.DB, sources *source.Service, log *slog.Logger, opts ...Option) *Runner {
	if log == nil {
		log = slog.Default()
	}
	r := &Runner{
		db:       db,
		sources:  sources,
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

func (r *Runner) Run(ctx context.Context, tick time.Duration) error {
	if tick <= 0 {
		tick = DefaultTick
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

// Sweep 取一轮到点的 poll 源，逐个起 goroutine（tick 不被慢源卡住 —— 卡住的下个 tick
// 记一次 skippedOverlaps，而不是排队）。
func (r *Runner) Sweep(ctx context.Context) (int, error) {
	sources, err := r.db.ListDueSourcesOfKind(ctx, store.SourceKindPoll, r.now().Unix(), DefaultBatch)
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

func (r *Runner) begin(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, busy := r.inflight[id]; busy {
		return false
	}
	r.inflight[id] = struct{}{}
	return true
}

func (r *Runner) done(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inflight, id)
}

func (r *Runner) round(ctx context.Context, src *store.Source) {
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

func (r *Runner) successNextFire(src *store.Source, now time.Time) int64 {
	next, err := sourcespec.NextPollTime(src.Config, now)
	if err != nil || next.IsZero() {
		return now.Add(neverFireHorizon).Unix()
	}
	return next.Unix()
}

// failureNextFire 的间隔是 min(base << (failures-1), 30min)，429 的 Retry-After 优先。
func (r *Runner) failureNextFire(src *store.Source, now time.Time, failures int, retry time.Duration) int64 {
	delay := retry
	if delay <= 0 {
		base := backoffCap
		if next, err := sourcespec.NextPollTime(src.Config, now); err == nil && next.After(now) {
			base = next.Sub(now)
		}
		if base <= 0 || base > backoffCap {
			base = backoffCap
		}
		delay = base
		for i := 1; i < failures && delay < backoffCap; i++ {
			delay *= 2
		}
	}
	if delay > backoffCap {
		delay = backoffCap
	}
	return now.Add(delay).Unix()
}

func (r *Runner) items(cfg sourcespec.Config, doc any) ([]any, error) {
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

func (r *Runner) ingest(ctx context.Context, src *store.Source, cfg sourcespec.Config, items []any) error {
	for i, item := range items {
		in, err := inputFor(src, cfg, item)
		if err != nil {
			return fmt.Errorf("poll: 第 %d 条 item 不合法: %w", i, err)
		}
		if _, err := r.sources.Ingest(ctx, in); err != nil {
			return fmt.Errorf("poll: 第 %d 条 item 入站失败: %w", i, err)
		}
	}
	return nil
}
