package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/resultspool"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 #332 的**真实 GC 驱动**测试：验证 Expire → 删私有文件 → Ack 这条链在
// 真 sqlite + 真托管目录上真的把字节清掉并记账，而不是只测「函数被调用过」。
//
// 保留期事实（7 天副本 / 30 天事实 / 0 永久 / 活跃 ticket 与 running attempt 保护）
// 全部在 store 侧（`ExpireArtifactObjects` 的谓词），这里通过真实驱动复验外部行为：
// 副本被删、manifest 元数据仍在（410 可诊断）、active 与永久条目一个字节都不许动。

// newTestArtifactGC 用 fixture 的库与真实 spool 建一个「时钟固定为 now」的 GC。
func newTestArtifactGC(t *testing.T, env *artifactEnv, now int64) *resultspool.GC {
	t.Helper()
	gc, err := resultspool.NewGC(newArtifactGCStore(env.db), env.spool, resultspool.GCOptions{
		Interval: time.Minute,
		Limit:    16,
		Now:      func() int64 { return now },
		Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	if err != nil {
		t.Fatalf("new gc: %v", err)
	}
	return gc
}

// objectIDOf 返回托管目录里唯一对象文件名（= store 铸造的对象 id）。
func objectIDOf(t *testing.T, env *artifactEnv) string {
	t.Helper()
	files := env.objectFiles(t)
	if len(files) != 1 {
		t.Fatalf("spool objects = %v, want exactly 1", files)
	}
	return files[0]
}

// TestArtifactGCPurgesExpiredCopyAndKeepsMetadata 是驱动的主路径：
// 7 天到期 → 字节被删 → object 记 purged_at → manifest 行保留且 ExpiredAt 非空 →
// 下载变成 410 artifact_expired（元数据可诊断，不再服务字节）。
func TestArtifactGCPurgesExpiredCopyAndKeepsMetadata(t *testing.T) {
	env := newArtifactEnv(t)
	ctx := context.Background()
	ref := env.seed(t, "reports/report.md", []byte("gc-purge-me"))
	env.complete(t, ref)
	objectID := objectIDOf(t, env)

	now := store.Now()
	expiredAt := now + store.DefaultRetentionResultTTLSec + 1
	gc := newTestArtifactGC(t, env, expiredAt)
	rep, err := gc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("gc run once: %v", err)
	}
	if rep.Expired < 1 || rep.Purged < 1 || rep.Failed != 0 {
		t.Fatalf("gc report = %+v, want Expired>=1 Purged>=1 Failed=0", rep)
	}
	if files := env.objectFiles(t); len(files) != 0 {
		t.Fatalf("expired copy still on disk: %v", files)
	}
	var purgedAt *int64
	if err := env.db.QueryRowContext(ctx, `SELECT purged_at FROM task_artifact_objects WHERE id=?`, objectID).Scan(&purgedAt); err != nil {
		t.Fatalf("read object purged_at: %v", err)
	}
	if purgedAt == nil {
		t.Fatal("object purged_at is NULL after successful purge+ack")
	}
	entries, err := env.db.ListArtifactManifest(ctx, env.ns.ID, env.task.ID, env.attempt)
	if err != nil || len(entries) != 1 || entries[0].ExpiredAt == nil {
		t.Fatalf("manifest metadata after purge = %+v (err=%v), want 1 expired row", entries, err)
	}
	if _, err := env.db.GetTaskArtifactDownload(ctx, env.ns.ID, env.task.ID, ref.SHA256, expiredAt); err == nil {
		t.Fatal("download still served after byte purge")
	} else if err != store.ErrArtifactExpired {
		t.Fatalf("download after purge = %v, want ErrArtifactExpired", err)
	}
}

// TestArtifactGCKeepsActiveAndPermanentCopies：活跃任务（未 reported）与
// `retentionResultTtlSec=0`（永久）的副本，即使时钟推到很远的未来也不许删。
func TestArtifactGCKeepsActiveAndPermanentCopies(t *testing.T) {
	t.Run("active task", func(t *testing.T) {
		env := newArtifactEnv(t)
		ctx := context.Background()
		env.seed(t, "reports/active.md", []byte("still-running"))
		// 不 complete：任务仍在 running（attempt 未 reap）→ 字节受保护。
		far := store.Now() + 10*365*86400
		gc := newTestArtifactGC(t, env, far)
		rep, err := gc.RunOnce(ctx)
		if err != nil {
			t.Fatalf("gc run once: %v", err)
		}
		if rep.Expired != 0 || rep.Purged != 0 {
			t.Fatalf("gc collected active task bytes: %+v", rep)
		}
		if files := env.objectFiles(t); len(files) != 1 {
			t.Fatalf("active copy removed: %v", files)
		}
	})

	t.Run("ttl zero is permanent", func(t *testing.T) {
		env := newArtifactEnv(t)
		ctx := context.Background()
		// 先把结果保留期设成 0（永久），再上传登记 → manifest.expires_at = 0。
		cfg, err := env.db.GetSystemConfig(ctx)
		if err != nil {
			t.Fatalf("get system config: %v", err)
		}
		cfg.RetentionResultTTLSec = 0
		if err := env.db.PutSystemConfig(ctx, cfg, cfg.ResourceVersion); err != nil {
			t.Fatalf("put system config: %v", err)
		}
		ref := env.seed(t, "reports/forever.md", []byte("keep-forever"))
		env.complete(t, ref)
		entries, err := env.db.ListArtifactManifest(ctx, env.ns.ID, env.task.ID, env.attempt)
		if err != nil || len(entries) != 1 || entries[0].ExpiresAt != 0 {
			t.Fatalf("manifest expires_at = %+v (err=%v), want 0 (permanent)", entries, err)
		}
		far := store.Now() + 10*365*86400
		gc := newTestArtifactGC(t, env, far)
		rep, err := gc.RunOnce(ctx)
		if err != nil {
			t.Fatalf("gc run once: %v", err)
		}
		if rep.Expired != 0 || rep.Purged != 0 {
			t.Fatalf("gc collected permanent copy: %+v", rep)
		}
		if files := env.objectFiles(t); len(files) != 1 {
			t.Fatalf("permanent copy removed: %v", files)
		}
	})
}

// TestArtifactGCAdapterRejectsUnknownAck：适配器拒绝 Ack 一个没在本轮 Expire 返回的 id
// （typed id 只能来自 store 铸造，不允许任意字符串冒充身份）。
func TestArtifactGCAdapterRejectsUnknownAck(t *testing.T) {
	env := newArtifactEnv(t)
	adapter := newArtifactGCStore(env.db)
	if err := adapter.AckPurged(context.Background(), "obj_not_minted", store.Now()); err == nil {
		t.Fatal("adapter acked an id that this pass never minted")
	}
}

// TestArtifactUploadUnavailableWhenSpoolMissing：托管池没配好时上传回 503
// `artifact_upload_unavailable`（诚实的部署错误，不是假成功），且不写任何事实。
func TestArtifactUploadUnavailableWhenSpoolMissing(t *testing.T) {
	env := newArtifactEnv(t)
	mux := http.NewServeMux()
	mux.Handle("PUT "+protocol.NodeTaskArtifactsPathPrefix+"{taskId}"+protocol.NodeTaskArtifactsPathInfix+"{sha256}",
		newArtifactUploadHandler(env.db, nil, env.deps.logger()))
	content := []byte("no-spool")
	rr := env.do(mux, http.MethodPut, env.uploadURL(artifactSHA(content)),
		nil, env.uploadHeaders("reports/x.md"))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rr.Code, rr.Body.String())
	}
	// 错误体必须是 A1 的动作文档形状（节点客户端只解析 {"error":…}）；只断言状态码
	// 会让「部署未配置」在节点侧退化成不可诊断的 transport。
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("503 body 不是 JSON 对象: %v (body=%s)", err, rr.Body.String())
	}
	if len(body) != 1 || body["error"] != artifactErrUnavailable {
		t.Fatalf("503 body = %v, want exactly {\"error\":%q}", body, artifactErrUnavailable)
	}
	env.assertNoArtifactWrites(t)
}

// TestArtifactSpoolAndGCAreWiredByNewApp：真实 NewApp 装配里 spool 真的建出来（0700）、
// 显式配置周期时 GC 真的被装配（不是死代码）。
func TestArtifactSpoolAndGCAreWiredByNewApp(t *testing.T) {
	dir := t.TempDir()
	cfg := FromEnv()
	cfg.DBPath = filepath.Join(dir, "app.db")
	cfg.AgentBin = filepath.Join(dir, "no-agent-binary")
	cfg.RPDisplay = "Anotify Artifact Test"
	cfg.RPID = "localhost"
	cfg.RPOrigin = "http://localhost"
	cfg.Pprof = false
	cfg.ArtifactSpoolDir = filepath.Join(dir, "spool")
	cfg.ArtifactGCIntervalSec = 300

	ctx, cancel := context.WithCancel(context.Background())
	app := NewApp(ctx, cfg)
	t.Cleanup(func() {
		cancel()
		// 先等后台 goroutine（GC/心跳等）退出再关库：#259 postclose-guard 会抓
		// “Close 之后仍用句柄”的竞态；只 cancel 不 Wait 会让测试在 -count=N 下偶发红。
		app.WaitBackground()
		_ = app.DB.Close()
	})
	if app.Spool == nil {
		t.Fatal("NewApp did not wire the artifact spool")
	}
	info, err := os.Stat(app.Spool.Root())
	if err != nil || !info.IsDir() {
		t.Fatalf("spool root = %v (err=%v), want an existing directory", app.Spool.Root(), err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("spool root perm = %o, want 0700", perm)
	}
	if app.ArtifactGC == nil {
		t.Fatal("NewApp did not wire the artifact GC for a positive interval")
	}

	// 托管根不可建时降级为 nil（产物端点回 503），不拖垮整个进程。
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	cfg.ArtifactSpoolDir = filepath.Join(blocked, "spool")
	cfg.ArtifactGCIntervalSec = 0
	ctx2, cancel2 := context.WithCancel(context.Background())
	app2 := NewApp(ctx2, cfg)
	t.Cleanup(func() {
		cancel2()
		app2.WaitBackground()
		_ = app2.DB.Close()
	})
	if app2.Spool != nil {
		t.Fatalf("spool = %v, want nil when the root cannot be created", app2.Spool.Root())
	}
	if app2.ArtifactGC != nil {
		t.Fatal("GC wired without a spool")
	}
}
