package server

import (
	"context"
	"fmt"
	"sync"

	"github.com/zzjcool/anotify/internal/store"
)

// artifactGCStore 是 resultspool.GC 与 store 之间的适配器（#332 / A1）。
//
// 为什么要这层适配而不是让 resultspool import store：
//   - resultspool 是**纯字节池**（只认不透明 id 字符串），它的测试用 fake 就能覆盖
//     全部错误分支，不需要真 sqlite；
//   - store.ArtifactObjectID 刻意**没有** string → ID 的构造函数（「身份只能由 store
//     铸造」），所以 ack 所需的 typed id 必须来自同一轮 Expire 的返回值。
//     本适配器把那一轮拿到的 typed id 暂存起来按字符串回查，过期条目在下一轮
//     Expire 会再次返回（purged_at 仍为 NULL），所以暂存丢失只会导致重试，不会漏删。
//
// 并发：Expire/Ack 都在 GC 的**单个** goroutine 里被调用，但仍然加锁——这个类型
// 是导出的接缝，将来若有多路径调用，静默 map 竞争比多一把 mutex 更贵。
type artifactGCStore struct {
	db *store.DB

	mu   sync.Mutex
	live map[string]store.ArtifactObjectID
}

func newArtifactGCStore(db *store.DB) *artifactGCStore {
	return &artifactGCStore{db: db, live: map[string]store.ArtifactObjectID{}}
}

// ExpireObjects 撤销一批已到期的字节身份，返回不透明 id 字符串。
func (a *artifactGCStore) ExpireObjects(ctx context.Context, now int64, limit int) ([]string, error) {
	ids, err := a.db.ExpireArtifactObjects(ctx, now, limit)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, id := range ids {
		a.live[id.String()] = id
		out = append(out, id.String())
	}
	return out, nil
}

// AckPurged 记录「私有系统文件已删除」；未在本轮 Expire 出现过的 id 一律拒绝，
// 避免把任意字符串当身份去 Ack（store 侧同样会以 invalid_transition 拒绝）。
func (a *artifactGCStore) AckPurged(ctx context.Context, id string, now int64) error {
	a.mu.Lock()
	typed, ok := a.live[id]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("artifact gc: ack unknown object %q: %w", id, store.ErrInvalidTransition)
	}
	if err := a.db.AckArtifactObjectPurged(ctx, typed, now); err != nil {
		return err
	}
	a.mu.Lock()
	delete(a.live, id)
	a.mu.Unlock()
	return nil
}
