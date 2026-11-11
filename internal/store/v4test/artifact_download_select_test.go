package v4test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 #332（G3-D4）的**直接边界测试**：同一 sha256 在当前 reported result 里
// 被多个逻辑 path 引用时，下载选择的判定（live 优先 / 全过期 410 / 无引用 404）。
//
// 为什么单独一份：`TestC3b*` 钉的是「引用必须来自当前 task+attempt+namespace」与
// 「过期元数据保留」，没有覆盖「首个 expired 遮蔽其后 live」这个真实缺陷；修
// GetTaskArtifactDownload 的选择逻辑必须有判别性用例，否则回归无法被发现。
//
// 夹具沿用 artifact_registry_test.go 的 uploadFixture/registerInput 与 g14 的
// newNode/newTask/grant（真 sqlite 文件库 + 真授权链）。

// d4Fixture 建一条已 grant 的任务，并把同一个 sha256 登记到多个逻辑 path 上。
// 返回的 auth 可用于后续 result 提交（结果引用这些 path）。
type d4Fixture struct {
	db   *store.DB
	ns   string
	cap  *store.CapabilityVersion
	node *store.Node
	task *store.Task
	auth store.EmitAuth
	sha  string
}

// registerSameSHA 把同一个内容哈希登记到多个逻辑 path（每个 path 一条 manifest 行）。
func (f *d4Fixture) registerSameSHA(t *testing.T, paths ...string) map[string]store.ArtifactObjectID {
	t.Helper()
	out := map[string]store.ArtifactObjectID{}
	for _, p := range paths {
		in := protocol.ArtifactUploadResponse{TaskID: f.auth.TaskID, Attempt: f.auth.Attempt, Path: p, SHA256: f.sha, Size: 7}
		out[p] = registerInput(t, f.db, f.auth, in).ObjectID()
	}
	return out
}

// complete 让当前 attempt 以「引用这些 path」的结果终态化（写 result_ref=1 与 reported=1）。
func (f *d4Fixture) complete(t *testing.T, paths ...string) {
	t.Helper()
	refs := make([]protocol.TaskArtifact, 0, len(paths))
	for _, p := range paths {
		refs = append(refs, protocol.TaskArtifact{Path: p, SHA256: f.sha})
	}
	result := store.TaskResult{Phase: store.TaskSucceeded, Artifacts: refs}
	if err := f.db.CompleteTaskAuthorized(context.Background(),
		store.NodeAuthorization{NodeID: f.node.ID, NodeTokenHash: f.node.TokenHash},
		f.task.ID, f.auth.Attempt, f.auth.LeaseID, f.auth.Now, result); err != nil {
		t.Fatalf("complete task %s: %v", f.task.ID, err)
	}
}

// expire 把某条 manifest 行钉成「已过期」。mode=expiredAt 走字节撤销事实，
// mode=expiresAt 走可信时间边界（两者都必须被判 expired，且有判别性）。
func (f *d4Fixture) expire(t *testing.T, path, mode string, now int64) {
	t.Helper()
	var err error
	switch mode {
	case "expiredAt":
		_, err = f.db.Exec(`UPDATE task_artifact_manifests SET expired_at=? WHERE task_id=? AND attempt=? AND path=?`,
			now-1, f.task.ID, f.auth.Attempt, path)
	case "expiresAt":
		_, err = f.db.Exec(`UPDATE task_artifact_manifests SET expires_at=? WHERE task_id=? AND attempt=? AND path=?`,
			now-1, f.task.ID, f.auth.Attempt, path)
	default:
		t.Fatalf("unknown expire mode %q", mode)
	}
	if err != nil {
		t.Fatalf("expire %s: %v", path, err)
	}
}

func newD4Fixture(t *testing.T) *d4Fixture {
	t.Helper()
	db, ns, capability, node, task, auth := uploadFixture(t)
	return &d4Fixture{db: db, ns: ns, cap: capability, node: node, task: task, auth: auth, sha: strings.Repeat("a", 64)}
}

// TestD4SameSHALiveWinsOverEarlierExpired 是最小回归判别：path 字典序在前的那条已过期，
// 后面那条仍 live → 必须 200 且返回 live 那一行（修复前会直接 410）。
func TestD4SameSHALiveWinsOverEarlierExpired(t *testing.T) {
	for _, mode := range []string{"expiredAt", "expiresAt"} {
		t.Run(mode, func(t *testing.T) {
			f := newD4Fixture(t)
			ctx := context.Background()
			objs := f.registerSameSHA(t, "a-old", "b-live")
			f.complete(t, "a-old", "b-live")
			now := f.auth.Now
			f.expire(t, "a-old", mode, now)

			got, err := f.db.GetTaskArtifactDownload(ctx, f.ns, f.task.ID, f.sha, now)
			if err != nil {
				t.Fatalf("live sibling masked by earlier expired path: %v", err)
			}
			if got.Path != "b-live" {
				t.Fatalf("selected path = %q, want b-live", got.Path)
			}
			if got.ObjectID.String() != objs["b-live"].String() {
				t.Fatalf("selected object = %q, want live object %q", got.ObjectID.String(), objs["b-live"].String())
			}
			if got.ExpiredAt != nil {
				t.Fatal("live entry carries expired_at")
			}
		})
	}
}

// TestD4LiveFirstStaysLive：live 在前、expired 在后，行为不变（不得因为存在过期行而收紧）。
func TestD4LiveFirstStaysLive(t *testing.T) {
	f := newD4Fixture(t)
	ctx := context.Background()
	f.registerSameSHA(t, "a-live", "z-old")
	f.complete(t, "a-live", "z-old")
	now := f.auth.Now
	f.expire(t, "z-old", "expiredAt", now)

	got, err := f.db.GetTaskArtifactDownload(ctx, f.ns, f.task.ID, f.sha, now)
	if err != nil {
		t.Fatalf("live-first download: %v", err)
	}
	if got.Path != "a-live" {
		t.Fatalf("selected path = %q, want a-live", got.Path)
	}
}

// TestD4MultipleLivePicksLexicographicFirst：多条 live 时给 path 排序首条（下载 basename 确定）。
func TestD4MultipleLivePicksLexicographicFirst(t *testing.T) {
	f := newD4Fixture(t)
	ctx := context.Background()
	f.registerSameSHA(t, "b-live", "c-live", "a-dead")
	f.complete(t, "b-live", "c-live", "a-dead")
	now := f.auth.Now
	f.expire(t, "a-dead", "expiredAt", now)

	got, err := f.db.GetTaskArtifactDownload(ctx, f.ns, f.task.ID, f.sha, now)
	if err != nil {
		t.Fatalf("multi live download: %v", err)
	}
	if got.Path != "b-live" {
		t.Fatalf("selected path = %q, want first live b-live", got.Path)
	}
}

// TestD4AllExpiredIs410：所有引用都过期 → 410（元数据保留，但副本不可得）。
func TestD4AllExpiredIs410(t *testing.T) {
	f := newD4Fixture(t)
	ctx := context.Background()
	f.registerSameSHA(t, "a-one", "b-two")
	f.complete(t, "a-one", "b-two")
	now := f.auth.Now
	f.expire(t, "a-one", "expiredAt", now)
	f.expire(t, "b-two", "expiresAt", now)

	if _, err := f.db.GetTaskArtifactDownload(ctx, f.ns, f.task.ID, f.sha, now); !errors.Is(err, store.ErrArtifactExpired) {
		t.Fatalf("all expired download = %v, want ErrArtifactExpired", err)
	}
}

// TestD4HistoricalAttemptIs404：旧 attempt 的 manifest 不能服务当前 attempt 的下载。
// 序列与 C3 一致：attempt1 只上传（不提交结果）→ 租约回收/收束 → attempt2 提交**无产物**
// 的结果；此时 sha 只存在于历史 attempt 的 manifest 里，当前 reported attempt 无引用 → 404。
func TestD4HistoricalAttemptIs404(t *testing.T) {
	f := newD4Fixture(t)
	ctx := context.Background()
	f.registerSameSHA(t, "a-old-attempt")
	now := f.auth.Now

	if _, _, err := f.db.RecoverLostLeases(ctx, now+60); err != nil {
		t.Fatalf("recover leases: %v", err)
	}
	if _, err := f.db.ReapTaskAttempt(ctx, store.ReapedInputs{
		TaskID: f.task.ID, Attempt: f.auth.Attempt, NodeID: f.node.ID,
		NodeTokenHash: f.node.TokenHash, LeaseID: f.auth.LeaseID, Slot: 0, Now: now + 60,
	}); err != nil {
		t.Fatalf("reap attempt: %v", err)
	}
	if err := f.db.AssignTaskAt(ctx, f.task.ID, f.node.ID, "again", 2, now+65); err != nil {
		t.Fatalf("reassign: %v", err)
	}
	nextLease, _, nextAttempt, _, err := f.db.GrantLeaseAuthorized(ctx,
		store.NodeAuthorization{NodeID: f.node.ID, NodeTokenHash: f.node.TokenHash},
		f.task.ID, "again", 2, 0, now+65)
	if err != nil {
		t.Fatalf("grant next attempt: %v", err)
	}
	if err := f.db.CompleteTaskAuthorized(ctx,
		store.NodeAuthorization{NodeID: f.node.ID, NodeTokenHash: f.node.TokenHash},
		f.task.ID, nextAttempt, nextLease, now+66, store.TaskResult{Phase: store.TaskSucceeded}); err != nil {
		t.Fatalf("complete next attempt: %v", err)
	}
	if nextAttempt == f.auth.Attempt {
		t.Fatalf("fixture did not advance attempt: %d", nextAttempt)
	}
	if _, err := f.db.GetTaskArtifactDownload(ctx, f.ns, f.task.ID, f.sha, now+66); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("historical attempt download = %v, want ErrNotFound", err)
	}
}

// TestD4CrossNamespaceOtherTaskAndUnknownHashAre404：只有「当前 task 当前 attempt 的
// 结果引用」能定位字节，跨 ns / 别 task / 未知 hash 一律 404（不泄露存在性）。
func TestD4CrossNamespaceOtherTaskAndUnknownHashAre404(t *testing.T) {
	f := newD4Fixture(t)
	ctx := context.Background()
	f.registerSameSHA(t, "a")
	f.complete(t, "a")

	otherNS := newNS(t, f.db, "d4-other")
	if _, err := f.db.GetTaskArtifactDownload(ctx, otherNS, f.task.ID, f.sha, f.auth.Now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross namespace = %v, want ErrNotFound", err)
	}
	// 别 task 的同 hash live 副本（同一个 node 的第二个槽 → 真 grant）。
	secondCap := lifeCycleCap(t, f.db, f.ns, "d4-second", true, 2)
	second := newTask(t, f.db, f.ns, secondCap, "d4-second", "")
	lease, token, attempt := grant(t, f.db, second.ID, f.node.ID, "d4-second-asg")
	secondAuth := store.EmitAuth{TaskID: second.ID, NodeID: f.node.ID, NodeTokenHash: f.node.TokenHash, Attempt: attempt, LeaseID: lease, AttemptToken: token, Now: store.Now()}
	registerInput(t, f.db, secondAuth, protocol.ArtifactUploadResponse{TaskID: second.ID, Attempt: attempt, Path: "a", SHA256: "b" + strings.Repeat("b", 63), Size: 3})
	if _, err := f.db.GetTaskArtifactDownload(ctx, f.ns, f.task.ID, "b"+strings.Repeat("b", 63), f.auth.Now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other task hash = %v, want ErrNotFound", err)
	}
	if _, err := f.db.GetTaskArtifactDownload(ctx, f.ns, f.task.ID, strings.Repeat("f", 64), f.auth.Now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown hash = %v, want ErrNotFound", err)
	}
	if _, err := f.db.GetTaskArtifactDownload(ctx, f.ns, f.task.ID, "not-a-hash", f.auth.Now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("malformed hash = %v, want ErrNotFound", err)
	}
}

// TestD4ExpiredOwnerCannotBorrowLiveBytes：A 的引用全过期时，即使别的 task 有同 hash 的
// live 字节，A 也只能 410——字节身份按 (task, current attempt, result_ref) 定位。
func TestD4ExpiredOwnerCannotBorrowLiveBytes(t *testing.T) {
	f := newD4Fixture(t)
	ctx := context.Background()
	f.registerSameSHA(t, "a")
	f.complete(t, "a")
	now := f.auth.Now
	f.expire(t, "a", "expiredAt", now)

	// 同一份字节在另一个 task 里仍 live。
	holderCap := lifeCycleCap(t, f.db, f.ns, "d4-live-holder", true, 2)
	holder := newTask(t, f.db, f.ns, holderCap, "d4-holder", "")
	lease, token, attempt := grant(t, f.db, holder.ID, f.node.ID, "d4-holder-asg")
	holderAuth := store.EmitAuth{TaskID: holder.ID, NodeID: f.node.ID, NodeTokenHash: f.node.TokenHash, Attempt: attempt, LeaseID: lease, AttemptToken: token, Now: store.Now()}
	registerInput(t, f.db, holderAuth, protocol.ArtifactUploadResponse{TaskID: holder.ID, Attempt: attempt, Path: "a", SHA256: f.sha, Size: 7})
	holderResult := store.TaskResult{Phase: store.TaskSucceeded, Artifacts: []protocol.TaskArtifact{{Path: "a", SHA256: f.sha}}}
	if err := f.db.CompleteTaskAuthorized(ctx,
		store.NodeAuthorization{NodeID: f.node.ID, NodeTokenHash: f.node.TokenHash},
		holder.ID, attempt, lease, holderAuth.Now, holderResult); err != nil {
		t.Fatalf("complete holder: %v", err)
	}

	if _, err := f.db.GetTaskArtifactDownload(ctx, f.ns, f.task.ID, f.sha, now); !errors.Is(err, store.ErrArtifactExpired) {
		t.Fatalf("expired owner borrowed live bytes: %v, want ErrArtifactExpired", err)
	}
	got, err := f.db.GetTaskArtifactDownload(ctx, f.ns, holder.ID, f.sha, now)
	if err != nil {
		t.Fatalf("live holder download: %v", err)
	}
	if got.TaskID != holder.ID {
		t.Fatalf("live holder served %q, want %q", got.TaskID, holder.ID)
	}
}

// TestD4UnreportedTaskIs404：结果还没上报（reported=0）时既没有「当前 ResultRef」，
// 不能按已登记 manifest 提供下载。
func TestD4UnreportedTaskIs404(t *testing.T) {
	f := newD4Fixture(t)
	ctx := context.Background()
	f.registerSameSHA(t, "a")
	if _, err := f.db.GetTaskArtifactDownload(ctx, f.ns, f.task.ID, f.sha, f.auth.Now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unreported task download = %v, want ErrNotFound", err)
	}
	// 未引用的 manifest 行（result_ref=0）同样不提供下载。
	f.complete(t)
	if _, err := f.db.GetTaskArtifactDownload(ctx, f.ns, f.task.ID, f.sha, f.auth.Now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unreferenced manifest download = %v, want ErrNotFound", err)
	}
}
