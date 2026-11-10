package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 #332 与 #340（生产时钟单位）的**接缝回归**。
//
// 背景（真实踩过的坑）：`serverDeps.Now` 的契约单位是**毫秒**（`nowMilli`），而
// `store.GetTaskArtifactDownload` 的 `now` 参数与 `task_artifact_manifests.expires_at`
// 是**秒**。直接把 `d.now()` 传给 store 会让「毫秒数」当成秒用——结果是所有 live 副本
// 都被判过期（一律 410）；更阴险的是**单域测试若把 Now 注入成秒级**，折算后落在 1970 年，
// 于是所有条目都 live，缺陷在测试里完全隐形。
//
// 所以这里用**毫秒**注入，并且做成双向判据：同一份 live 副本在毫秒时钟下必须 200，
// 推到 8 天后必须 410。少了任何一边，单位错了都不会红。

// downloadMuxFor 建一个与生产接线一致的下载 mux（会话中间件 + no-store）。
func downloadMuxFor(env *artifactEnv) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /v1/namespaces/{ns}/tasks/{taskId}/artifacts/{sha256}",
		noStore(env.deps.session(http.HandlerFunc(newArtifactDownloadAPI(env.deps).get))))
	return mux
}

// TestArtifactDownloadMillisecondClockKeepsLiveCopyLive：毫秒时钟下 live 副本必须 200。
//
// 若 handler 把毫秒直接当秒传给 store，`expires_at`（≈ now+7d 的秒）会远小于
// `now`（毫秒），这条用例立刻 410——正是 #340 那类单位缺陷的判别器。
func TestArtifactDownloadMillisecondClockKeepsLiveCopyLive(t *testing.T) {
	env := newArtifactEnv(t)
	content := []byte("live-under-millisecond-clock")
	ref := env.seed(t, "reports/live.md", content)
	env.complete(t, ref)

	entries, err := env.db.ListArtifactManifest(env.ctx, env.ns.ID, env.task.ID, env.attempt)
	if err != nil || len(entries) != 1 {
		t.Fatalf("manifest = %+v (err=%v)", entries, err)
	}
	// 前提校验：默认 TTL 让这条副本真的会过期（否则这条用例证明不了单位问题）。
	if entries[0].ExpiresAt <= store.Now() {
		t.Fatalf("fixture entry already expired (expiresAt=%d)", entries[0].ExpiresAt)
	}

	env.deps.Now = nowMilli // 生产口径：毫秒
	rr := env.do(downloadMuxFor(env), http.MethodGet,
		env.downloadURL(env.ns.Slug, env.task.ID, ref.SHA256), nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（毫秒时钟被当成秒 = 所有 live 副本误判过期）body=%s",
			rr.Code, rr.Body.String())
	}
}

// TestArtifactDownloadMillisecondClockExpiresPastTTL：同一机制的反向判据——
// 把毫秒时钟推到 8 天后，7 天 TTL 的副本必须 410（证明时钟真的参与了判定）。
func TestArtifactDownloadMillisecondClockExpiresPastTTL(t *testing.T) {
	env := newArtifactEnv(t)
	ref := env.seed(t, "reports/aged.md", []byte("expire-me-later"))
	env.complete(t, ref)

	future := time.Now().Add(8 * 24 * time.Hour).UnixMilli()
	env.deps.Now = func() int64 { return future }
	rr := env.do(downloadMuxFor(env), http.MethodGet,
		env.downloadURL(env.ns.Slug, env.task.ID, ref.SHA256), nil, nil)
	if rr.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410（8 天 > 7 天 TTL；body=%s）", rr.Code, rr.Body.String())
	}
	mustContainString(t, rr.Body.String(), reasonArtifactExpired, "410 reason")
}

// TestArtifactDownloadMissingClockFallsBackToStoreNow：deps.Now 未注入时回落 store.Now
// （秒），折算不得把当前时间除成 0 而让判定失效。
func TestArtifactDownloadMissingClockFallsBackToStoreNow(t *testing.T) {
	env := newArtifactEnv(t)
	ref := env.seed(t, "reports/fallback.md", []byte("clock-fallback"))
	env.complete(t, ref)
	env.deps.Now = nil
	rr := env.do(downloadMuxFor(env), http.MethodGet,
		env.downloadURL(env.ns.Slug, env.task.ID, ref.SHA256), nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with store.Now fallback (body=%s)", rr.Code, rr.Body.String())
	}
}
