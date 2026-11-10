package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zzjcool/anotify/cmd/anode/internal/capability"
	"github.com/zzjcool/anotify/cmd/anode/internal/clock"
	"github.com/zzjcool/anotify/cmd/anode/internal/execenv"
	"github.com/zzjcool/anotify/cmd/anode/internal/hostenv"
	"github.com/zzjcool/anotify/cmd/anode/internal/resultstore"
	"github.com/zzjcool/anotify/cmd/anode/internal/upload"
	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

// ---------- 测试夹具 ----------

type fakeSender struct {
	mu       sync.Mutex
	frames   []any
	offline  bool
	cond     *sync.Cond
	onResult func(*protocol.TaskResultFrame)
}

func newFakeSender() *fakeSender {
	s := &fakeSender{}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *fakeSender) Send(frame any) error {
	s.mu.Lock()
	if s.offline {
		s.mu.Unlock()
		return errors.New("fake: offline")
	}
	s.frames = append(s.frames, frame)
	s.cond.Broadcast()
	ack := s.onResult
	s.mu.Unlock()
	// Existing fixtures model a real server ACK, not "send result = idle".
	if result, ok := frame.(*protocol.TaskResultFrame); ok && ack != nil {
		ack(result)
	}
	return nil
}

// SendGuarded models a legal serialized transport boundary. Guard evaluation
// and recording happen under the sender mutex; receiver ACK happens only after
// releasing it, and no legacy Send fallback is permitted for results.
func (s *fakeSender) SendGuarded(ctx context.Context, frame any, guard func() bool) (bool, error) {
	s.mu.Lock()
	if s.offline {
		s.mu.Unlock()
		return false, errors.New("fake: offline")
	}
	if ctx.Err() != nil || guard == nil || !guard() {
		s.mu.Unlock()
		return false, nil
	}
	s.frames = append(s.frames, frame)
	s.cond.Broadcast()
	ack := s.onResult
	s.mu.Unlock()
	if result, ok := frame.(*protocol.TaskResultFrame); ok && ack != nil {
		ack(result)
	}
	return true, nil
}

func (s *fakeSender) setOffline(v bool) {
	s.mu.Lock()
	s.offline = v
	s.mu.Unlock()
}

func (s *fakeSender) snapshot() []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]any(nil), s.frames...)
}

func (s *fakeSender) find(pred func(any) bool) any {
	for _, f := range s.snapshot() {
		if pred(f) {
			return f
		}
	}
	return nil
}

type harness struct {
	t         *testing.T
	dir       string
	workRoot  string
	cache     *capability.Cache
	results   *resultstore.Store
	r         *Runner
	sender    *fakeSender
	clk       *clock.Fake
	baseline  hostenv.Baseline
	upload    *upload.Client
	mu        sync.Mutex
	present   bool
	rev       string
	vars      map[string]string
	limits    protocol.Limits
	localVars map[string]string
	ns        map[string]bool
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	workRoot := filepath.Join(dir, "work")
	resultsRoot := filepath.Join(dir, "results")
	h := &harness{
		t:         t,
		dir:       dir,
		workRoot:  workRoot,
		cache:     capability.NewCache(filepath.Join(dir, "capabilities")),
		results:   resultstore.New(resultsRoot, workRoot),
		sender:    newFakeSender(),
		clk:       clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		present:   true,
		rev:       "rev-1",
		vars:      map[string]string{"SOURCE_ROOT": "/srv/src"},
		limits:    protocol.Limits{Total: 2, PerCapability: map[string]int{"echo": 2}},
		localVars: map[string]string{},
		ns:        map[string]bool{"default": true},
	}
	h.baseline = hostenv.CaptureWith([]string{"TEST_SECRET"}, func(key string) (string, bool) {
		switch key {
		case "PATH":
			return "/usr/bin:/bin", true
		case "HOME":
			return dir, true
		case "TEST_SECRET":
			return "top-secret-value", true
		}
		return "", false
	})
	r, err := New(Options{
		NodeName:    "node-a",
		LocalVars:   h.localVars,
		Passthrough: []string{"TEST_SECRET"},
		Baseline:    h.baseline,
		Facts: protocol.NodeFacts{
			Hostname: "host-a", OS: "linux", Arch: "amd64", CPU: 4,
			WorkRoot: workRoot, HomeDir: dir,
		},
		Applied: func() (string, map[string]string, protocol.Limits, bool) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.rev, cloneMap(h.vars), h.limits, h.present
		},
		AllowsNamespace: func(ns string) bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.ns[ns]
		},
		LoadArtifact: func(digest string) (*artifact.Artifact, error) {
			entry, err := h.cache.Load(digest)
			if err != nil {
				return nil, err
			}
			return entry.Artifact, nil
		},
		Results:      h.results,
		EmitEndpoint: "http://127.0.0.1:12345",
		WorkRoot:     workRoot,
		Clock:        h.clk,
		Logf:         t.Logf,
		// 生产默认：TTL60 / renew20 / grace5（不缩到测试速度；测试推进假时钟）。
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	h.r = r
	h.sender.onResult = func(result *protocol.TaskResultFrame) {
		r.HandleResultAck(&protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: result.TaskID, Attempt: result.Attempt, Accepted: true})
	}
	r.SetSender(h.sender)
	// 默认接一个通用假上传端点（A1 语义：201 + 回显四元组）；需要断言的测试可覆盖。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		sum := sha256.Sum256(body)
		sha := hex.EncodeToString(sum[:])
		parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/v1/node/tasks/"), "/artifacts/")
		if len(parts) != 2 || parts[1] != sha {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "artifact_hash_mismatch"})
			return
		}
		attempt := 0
		fmt.Sscanf(req.Header.Get(protocol.ArtifactHeaderAttempt), "%d", &attempt)
		path, _ := protocol.DecodeArtifactPathHeader(req.Header.Get(protocol.ArtifactHeaderArtifactPath))
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"taskId": parts[0], "attempt": attempt, "path": path,
			"sha256": sha, "size": len(body),
		})
	}))
	t.Cleanup(srv.Close)
	r.opts.Upload = &upload.Client{BaseURL: srv.URL, NodeToken: "node-token", HTTP: srv.Client(), Retries: 0}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		r.Drain(ctx)
		r.Close(ctx)
	})
	return h
}

func (h *harness) artifact(mutate func(*artifact.Artifact)) (*artifact.Artifact, string) {
	h.t.Helper()
	a := &artifact.Artifact{
		APIVersion: "anotify/v1",
		Kind:       "Capability",
		Metadata:   artifact.Metadata{Name: "echo", Version: 1},
		Spec: artifact.Spec{
			Params: json.RawMessage(`{}`),
			Next:   []string{"fixer"},
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\necho hi\n"},
			},
			Lifecycle:   artifact.Lifecycle{ResultTTLSec: intPtr(0)},
			Concurrency: artifact.Concurrency{MaxPerNode: intPtr(1)},
		},
	}
	if mutate != nil {
		mutate(a)
	}
	if err := artifact.Validate(a); err != nil {
		h.t.Fatalf("artifact.Validate: %v", err)
	}
	digest, err := artifact.Digest(a)
	if err != nil {
		h.t.Fatalf("artifact.Digest: %v", err)
	}
	if _, err := h.cache.Store(a, digest); err != nil {
		h.t.Fatalf("cache.Store: %v", err)
	}
	return a, digest
}

func (h *harness) dispatch(taskID string, attempt int, a *artifact.Artifact, digest string) {
	h.r.HandleDispatch(&protocol.TaskDispatchFrame{
		Type:            protocol.FrameTaskDispatch,
		TaskID:          taskID,
		AssignmentID:    "assign-" + taskID,
		ProposedAttempt: attempt,
		Namespace:       "default",
		Capability: protocol.CapabilityRef{
			Name: a.Metadata.Name, Version: a.Metadata.Version, Digest: digest,
		},
		Params:                json.RawMessage(`{}`),
		NamespaceVars:         map[string]string{},
		NamespaceVarsRevision: "nsrev-1",
		ConfigRevision:        "rev-1",
		AssignmentExpiresAt:   h.clk.Now().Add(30 * time.Second),
	})
}

// acceptFor 取指定 task/attempt 的 slot 与 assignmentId（离线时 accept 帧发不出去，
// 所以优先读 runner 内部的真实预留/运行/历史账本）。
func (h *harness) acceptFor(taskID string, attempt int) (*int, string) {
	h.r.mu.Lock()
	defer h.r.mu.Unlock()
	if res, ok := h.r.reservations[taskID]; ok && res.proposed == attempt {
		slot := res.slot
		return &slot, res.assignmentID
	}
	if att, ok := h.r.running[taskID]; ok && att.attempt == attempt {
		slot := att.slot
		return &slot, att.assignmentID
	}
	if hist, ok := h.r.history[resultKey(taskID, attempt)]; ok {
		slot := hist.slot
		return &slot, ""
	}
	return nil, ""
}

func (h *harness) grant(taskID string, attempt int, leaseID, token string) {
	slot, assignment := h.acceptFor(taskID, attempt)
	h.r.HandleLeaseGrant(&protocol.LeaseGrantFrame{
		Type: protocol.FrameLeaseGrant, TaskID: taskID, Attempt: attempt,
		Node: "node-a", LeaseID: leaseID, AttemptToken: token,
		ExpiresAt:    h.clk.Now().Add(protocol.LeaseTTL),
		AssignmentID: assignment, Slot: slot,
	})
}

// cancel 按 (task/attempt/leaseId/slot) 的历史绑定发取消。
func (h *harness) cancel(taskID string, attempt int, leaseID string) {
	slot, _ := h.acceptFor(taskID, attempt)
	h.r.HandleCancel(&protocol.TaskCancelFrame{
		Type: protocol.FrameTaskCancel, TaskID: taskID, Attempt: attempt,
		LeaseID: leaseID, Slot: slot, Reason: protocol.CancelReasonUser,
	})
}

func (h *harness) waitFrame(t *testing.T, pred func(any) bool, timeout time.Duration) any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f := h.sender.find(pred); f != nil {
			return f
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待帧超时；已收帧：%v", frameTypes(h.sender.snapshot()))
	return nil
}

// waitFrameAdvancing 在等待帧时推进假时钟（租约/超时/宽限都由 clock 驱动）。
func (h *harness) waitFrameAdvancing(t *testing.T, pred func(any) bool, step time.Duration, timeout time.Duration) any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f := h.sender.find(pred); f != nil {
			return f
		}
		h.clk.Advance(step)
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("推进时钟等待帧超时；已收帧：%v", frameTypes(h.sender.snapshot()))
	return nil
}

func (h *harness) waitUntil(t *testing.T, what string, pred func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时", what)
}

func frameTypes(frames []any) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, fmt.Sprintf("%T", f))
	}
	return out
}

func findResult(frames []any, taskID string, attempt int) *protocol.TaskResultFrame {
	for _, f := range frames {
		if r, ok := f.(*protocol.TaskResultFrame); ok && r.TaskID == taskID && r.Attempt == attempt {
			return r
		}
	}
	return nil
}

func hasAccept(frames []any, taskID string) bool {
	for _, f := range frames {
		if a, ok := f.(*protocol.TaskAcceptFrame); ok && a.TaskID == taskID {
			return true
		}
	}
	return false
}

func countAccepts(frames []any, taskID string) int {
	n := 0
	for _, f := range frames {
		if a, ok := f.(*protocol.TaskAcceptFrame); ok && a.TaskID == taskID {
			n++
		}
	}
	return n
}

// ---------- dispatch / grant 边界 ----------

func TestDispatchReservesOnlyNoExecutionWithoutGrant(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(nil)
	h.dispatch("t1", 1, a, digest)

	h.waitFrame(t, func(f any) bool {
		acc, ok := f.(*protocol.TaskAcceptFrame)
		return ok && acc.TaskID == "t1" && acc.Node == "node-a"
	}, 2*time.Second)

	// dispatch 没有 token、不执行、不计 attempt：没有 result、没有 attempt 目录。
	time.Sleep(100 * time.Millisecond)
	if r := findResult(h.sender.snapshot(), "t1", 1); r != nil {
		t.Fatalf("没有 grant 不能产生 result：%+v", r)
	}
	if _, err := os.Stat(execenv.SystemWorkDir(h.workRoot, "t1", 1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("没有 grant 不能创建执行目录（err=%v）", err)
	}
}

func TestStaleConfigRejectsBeforeAcceptAndDoesNotCountAttempt(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(nil)
	h.mu.Lock()
	h.rev = "rev-2"
	h.mu.Unlock()
	h.dispatch("t1", 1, a, digest)

	rej := h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskRejectFrame)
		return ok && r.TaskID == "t1"
	}, 2*time.Second).(*protocol.TaskRejectFrame)
	if rej.Reason != protocol.ReasonStaleConfig {
		t.Fatalf("reject reason 只能 stale_config，实际 %q", rej.Reason)
	}
	if hasAccept(h.sender.snapshot(), "t1") {
		t.Fatal("stale_config 不能先 accept")
	}
	if r := findResult(h.sender.snapshot(), "t1", 1); r != nil {
		t.Fatalf("stale_config 不计 attempt、不产生 result：%+v", r)
	}
}

func TestColdStartWithoutAppliedRejectsStale(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(nil)
	h.mu.Lock()
	h.present = false
	h.mu.Unlock()
	h.dispatch("t1", 1, a, digest)
	rej := h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskRejectFrame)
		return ok && r.TaskID == "t1"
	}, 2*time.Second).(*protocol.TaskRejectFrame)
	if rej.Reason != protocol.ReasonStaleConfig {
		t.Fatalf("冷启动必须是 stale_config，实际 %q", rej.Reason)
	}
	if hasAccept(h.sender.snapshot(), "t1") {
		t.Fatal("冷启动不能假在线 accept")
	}
}

func TestGrantWithoutReservedDispatchDoesNotStart(t *testing.T) {
	h := newHarness(t)
	h.grant("ghost", 1, "lease-1", "token-1")
	time.Sleep(100 * time.Millisecond)
	if r := findResult(h.sender.snapshot(), "ghost", 1); r != nil {
		t.Fatalf("没有 reserved dispatch 的 grant 不能执行：%+v", r)
	}
	if _, err := os.Stat(execenv.SystemWorkDir(h.workRoot, "ghost", 1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("不能创建执行目录")
	}
}

func TestGrantForWrongNodeIsIgnored(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(nil)
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(f any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.r.HandleLeaseGrant(&protocol.LeaseGrantFrame{
		Type: protocol.FrameLeaseGrant, TaskID: "t1", Attempt: 1, Node: "node-b",
		LeaseID: "l", AttemptToken: "t", ExpiresAt: h.clk.Now().Add(time.Minute),
	})
	time.Sleep(100 * time.Millisecond)
	if r := findResult(h.sender.snapshot(), "t1", 1); r != nil {
		t.Fatalf("node 不符的 grant 不能执行：%+v", r)
	}
}

func TestDuplicateDispatchAndGrantDoNotDoubleExecute(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\necho x >> counter\n"
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.dispatch("t1", 1, a, digest)
	h.grant("t1", 1, "lease-1", "token-1")
	h.grant("t1", 1, "lease-1", "token-1")

	h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "t1"
	}, 5*time.Second)
	time.Sleep(100 * time.Millisecond)
	// A3：重复 dispatch 是幂等 re-accept（回同一个 slot），不是第二份预留。
	var seenSlots []int
	for _, f := range h.sender.snapshot() {
		if acc, ok := f.(*protocol.TaskAcceptFrame); ok && acc.TaskID == "t1" && acc.Slot != nil {
			seenSlots = append(seenSlots, *acc.Slot)
		}
	}
	if len(seenSlots) < 1 {
		t.Fatal("应至少 accept 一次")
	}
	for _, slot := range seenSlots {
		if slot != seenSlots[0] {
			t.Fatalf("重复 dispatch 必须回同一个 slot：%v", seenSlots)
		}
	}
	data, err := os.ReadFile(filepath.Join(execenv.SystemWorkDir(h.workRoot, "t1", 1), "counter"))
	if err != nil {
		t.Fatalf("读 counter: %v", err)
	}
	if strings.Count(string(data), "x") != 1 {
		t.Fatalf("重复 dispatch/grant 不能重复执行：counter=%q", data)
	}
}

// ---------- 启动前确定性失败（已计 attempt） ----------

func TestMissingRequirementFailsAfterGrantAndCountsAttempt(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Requires.Commands = []string{"anotify-command-does-not-exist-xyz"}
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")

	res := h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "t1"
	}, 3*time.Second).(*protocol.TaskResultFrame)
	if res.Phase != protocol.TaskPhaseFailed || res.Result.Error == nil ||
		res.Result.Error.Reason != protocol.ReasonMissingRequirement {
		t.Fatalf("缺命令必须是 failed/missing_requirement：%+v", res)
	}
	if res.Attempt != 1 || res.Node != "node-a" || res.LeaseID != "lease-1" {
		t.Fatalf("result 身份字段不对：%+v", res)
	}
	// 槽位已释放：下一个任务能 accept。
	h.dispatch("t2", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t2") }, 2*time.Second)
}

func TestScriptExit5IsScriptFailedNotInfra(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nexit 5\n"
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")

	res := h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "t1"
	}, 5*time.Second).(*protocol.TaskResultFrame)
	if res.Phase != protocol.TaskPhaseFailed || res.Result.Error == nil ||
		res.Result.Error.Reason != protocol.ReasonScriptFailed {
		t.Fatalf("exit 5 必须是 script_failed：%+v", res)
	}
	if res.ExitCode == nil || *res.ExitCode != 5 {
		t.Fatalf("必须保留退出码 5：%+v", res.ExitCode)
	}
	if protocol.IsRetryableInfra(res.Result.Error.Reason) {
		t.Fatal("exit 5 不能被当作 infra 事实")
	}
	if res.Result.Artifacts == nil || len(res.Result.Artifacts) != 0 {
		t.Fatalf("artifacts 必须是空数组（非 nil）：%#v", res.Result.Artifacts)
	}
}

// ---------- 租约 / 假时钟 ----------

func TestLeaseACKLossReapsGroupAndFreesSlot(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		// 忽略 TERM：验证 5s 宽限后会 KILL 整个进程组。
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\ntrap '' TERM\nsleep 30\n"
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")
	h.r.mu.Lock()
	att := h.r.running["t1"]
	h.r.mu.Unlock()

	// 20s 续租一次（没有 ACK）。
	h.waitFrameAdvancing(t, func(f any) bool {
		r, ok := f.(*protocol.LeaseRenewFrame)
		return ok && r.TaskID == "t1" && r.LeaseID == "lease-1"
	}, time.Second, 3*time.Second)

	// 推进过 60s deadline：本地判失联 → TERM（被忽略）→ 5s 后 KILL。
	h.waitFrameAdvancing(t, func(f any) bool {
		ack, ok := f.(*protocol.CancelAckFrame)
		return ok && ack.TaskID == "t1" && ack.Attempt == 1 && ack.LeaseID == "lease-1" && ack.Slot != nil && *ack.Slot == att.slot && ack.ProcessGroupReaped
	}, time.Second, 10*time.Second)
	rework2AssertExpiredArchive(t, h, att)

	// 进程组真的没了、槽位已释放（能再 accept 一个任务）。
	h.waitUntil(t, "槽位释放", func() bool {
		h.dispatch("t2", 1, a, digest)
		return hasAccept(h.sender.snapshot(), "t2")
	}, 3*time.Second)
}

func TestLeaseAckExtendsDeadline(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nsleep 30\n"
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")
	h.r.mu.Lock()
	att := h.r.running["t1"]
	h.r.mu.Unlock()

	// 推进到 50s，给一个 ACK：deadline 变成 50s+60s=110s，不能在第 60s 被判失联。
	h.clk.Advance(50 * time.Second)
	h.r.HandleLeaseAck(&protocol.LeaseAckFrame{
		Type: protocol.FrameLeaseAck, TaskID: "t1", Attempt: 1, LeaseID: "lease-1",
		ExpiresAt: h.clk.Now().Add(protocol.LeaseTTL),
	})
	h.clk.Advance(20 * time.Second) // t=70s > 原始 60s deadline
	time.Sleep(100 * time.Millisecond)
	if r := findResult(h.sender.snapshot(), "t1", 1); r != nil || !att.authorized() || !h.r.ActiveTasks()["t1"] {
		t.Fatalf("收到 ACK 后不能按旧 deadline 杀任务：%+v", r)
	}
	// 旧 lease 的 ACK 不刷新当前 deadline。
	h.r.HandleLeaseAck(&protocol.LeaseAckFrame{
		Type: protocol.FrameLeaseAck, TaskID: "t1", Attempt: 99, LeaseID: "lease-1",
	})
	h.clk.Advance(60 * time.Second)
	h.waitFrameAdvancing(t, func(f any) bool {
		ack, ok := f.(*protocol.CancelAckFrame)
		return ok && ack.TaskID == "t1" && ack.Attempt == 1 && ack.LeaseID == "lease-1" && ack.Slot != nil && *ack.Slot == att.slot && ack.ProcessGroupReaped
	}, time.Second, 10*time.Second)
	rework2AssertExpiredArchive(t, h, att)
}

func TestTimeoutKillsGroupAndIsFailedTimeout(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nsleep 30\n"
		a.Spec.Lifecycle.TimeoutSec = 2
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")

	res := h.waitFrameAdvancing(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "t1"
	}, time.Second, 8*time.Second).(*protocol.TaskResultFrame)
	if res.Phase != protocol.TaskPhaseFailed || res.Result.Error == nil ||
		res.Result.Error.Reason != protocol.ReasonTimeout {
		t.Fatalf("超时必须是 failed/timeout：%+v", res)
	}
}

// ---------- cancel / 子孙回收 ----------

func TestCancelReapsGrandchildHoldingPipesAndReleasesSlot(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		// leader 立刻退出，孙进程继续持有 stdout/stderr（父退出但孙仍活的场景）。
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nsleep 30 &\necho $! > child.pid\n"
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")

	workDir := execenv.SystemWorkDir(h.workRoot, "t1", 1)
	h.waitUntil(t, "孙进程 pid 文件", func() bool {
		_, err := os.Stat(filepath.Join(workDir, "child.pid"))
		return err == nil
	}, 3*time.Second)
	pidData, _ := os.ReadFile(filepath.Join(workDir, "child.pid"))
	var childPID int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(pidData)), "%d", &childPID); err != nil || childPID <= 0 {
		t.Fatalf("解析孙进程 pid 失败：%q", pidData)
	}

	h.cancel("t1", 1, "lease-1")
	ack := h.waitFrame(t, func(f any) bool {
		c, ok := f.(*protocol.CancelAckFrame)
		return ok && c.TaskID == "t1"
	}, 5*time.Second).(*protocol.CancelAckFrame)
	if !ack.ProcessGroupReaped {
		t.Fatal("进程组真正回收后才能回 processGroupReaped=true")
	}
	h.waitUntil(t, "孙进程退出", func() bool {
		return errors.Is(syscall.Kill(childPID, 0), syscall.ESRCH)
	}, 3*time.Second)
	if r := findResult(h.sender.snapshot(), "t1", 1); r != nil {
		t.Fatalf("取消走 cancel.ack，不应再发 task.result：%+v", r)
	}
	// 槽位已释放。
	h.dispatch("t2", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t2") }, 3*time.Second)
}

func TestStaleCancelDoesNotKillNewAttempt(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\necho x >> counter\nsleep 0.4\n"
	})
	// attempt 1：cancel 掉。
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")
	h.waitUntil(t, "attempt1 启动", func() bool {
		_, err := os.Stat(filepath.Join(execenv.SystemWorkDir(h.workRoot, "t1", 1), "counter"))
		return err == nil
	}, 3*time.Second)
	h.cancel("t1", 1, "lease-1")
	h.waitFrame(t, func(f any) bool {
		c, ok := f.(*protocol.CancelAckFrame)
		return ok && c.TaskID == "t1" && c.Attempt == 1
	}, 5*time.Second)

	// attempt 2（同逻辑 task）：旧 attempt 的 cancel 不能误停它。
	h.dispatch("t1", 2, a, digest)
	h.waitUntil(t, "attempt2 accept", func() bool {
		return countAccepts(h.sender.snapshot(), "t1") >= 2
	}, 3*time.Second)
	h.grant("t1", 2, "lease-2", "token-2")
	h.waitUntil(t, "attempt2 启动", func() bool {
		_, err := os.Stat(filepath.Join(execenv.SystemWorkDir(h.workRoot, "t1", 2), "counter"))
		return err == nil
	}, 3*time.Second)

	h.cancel("t1", 1, "lease-1")
	// 旧 attempt 的取消只回自己的 ack（不释放 attempt2 的槽位/不杀它）。
	res := h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "t1" && r.Attempt == 2
	}, 5*time.Second).(*protocol.TaskResultFrame)
	if res.Phase != protocol.TaskPhaseSucceeded {
		t.Fatalf("attempt2 应正常成功（没被旧 cancel 误杀）：%+v", res)
	}
}

// ---------- 结果重发 / 产物上传 ----------

func TestPendingResultResentOnSessionUpWithoutReexec(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\necho x >> counter\n"
	})
	h.sender.setOffline(true)
	h.dispatch("t1", 1, a, digest)
	if hasAccept(h.sender.snapshot(), "t1") {
		t.Fatal("离线时 accept 不应被记录")
	}
	// accept 也发不出去：直接发 grant 不会启动（没有 reserved dispatch？dispatch 已建预留）。
	h.grant("t1", 1, "lease-1", "token-1")
	h.waitUntil(t, "结果进入待确认", func() bool { return h.r.pendingResultCount() == 1 }, 5*time.Second)

	h.sender.setOffline(false)
	h.r.SessionUp()
	h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "t1"
	}, 3*time.Second)
	data, err := os.ReadFile(filepath.Join(execenv.SystemWorkDir(h.workRoot, "t1", 1), "counter"))
	if err != nil || strings.Count(string(data), "x") != 1 {
		t.Fatalf("重连只能重发事实，不能重新执行：data=%q err=%v", data, err)
	}
}

type uploadRecord struct {
	path, token, attempt, lease, artifactPath string
	body                                      []byte
}

func newUploadServer(t *testing.T, status int, mutate func(resp map[string]any, rec *uploadRecord)) (*httptest.Server, *[]uploadRecord) {
	t.Helper()
	var mu sync.Mutex
	var records []uploadRecord
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 0)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		rec := uploadRecord{
			path:         r.URL.Path,
			token:        r.Header.Get("Authorization"),
			attempt:      r.Header.Get(protocol.ArtifactHeaderAttempt),
			lease:        r.Header.Get(protocol.ArtifactHeaderLeaseID),
			artifactPath: r.Header.Get(protocol.ArtifactHeaderArtifactPath),
			body:         body,
		}
		mu.Lock()
		records = append(records, rec)
		mu.Unlock()
		sum := sha256.Sum256(body)
		resp := map[string]any{
			"taskId":  "t1",
			"attempt": 1,
			"path":    mustPath(rec.artifactPath),
			"sha256":  hex.EncodeToString(sum[:]),
			"size":    len(body),
		}
		if mutate != nil {
			mutate(resp, &rec)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &records
}

func mustPath(encoded string) string {
	decoded, err := protocol.DecodeArtifactPathHeader(encoded)
	if err != nil {
		return encoded
	}
	return decoded
}

func TestArtifactsCopiedUploadedThenResult(t *testing.T) {
	srv, records := newUploadServer(t, http.StatusCreated, nil)
	h := newHarness(t)
	client := &upload.Client{BaseURL: srv.URL, NodeToken: "node-token", HTTP: srv.Client(), Retries: 0}
	h.upload = client
	// 重新装配 runner，注入上传客户端。
	h.r.opts.Upload = client

	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nmkdir -p \"$ANOTIFY_ARTIFACT_DIR/out\"\nprintf 'hello' > \"$ANOTIFY_ARTIFACT_DIR/out/report.md\"\n"
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-9", "token-9")

	res := h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "t1"
	}, 5*time.Second).(*protocol.TaskResultFrame)
	if res.Phase != protocol.TaskPhaseSucceeded {
		t.Fatalf("应成功：%+v", res)
	}
	sum := sha256.Sum256([]byte("hello"))
	wantSHA := hex.EncodeToString(sum[:])
	if len(res.Result.Artifacts) != 1 || res.Result.Artifacts[0].Path != "artifacts/out/report.md" ||
		res.Result.Artifacts[0].SHA256 != wantSHA {
		t.Fatalf("result.artifacts 应是托管副本清单：%+v", res.Result.Artifacts)
	}
	recs := *records
	if len(recs) != 1 {
		t.Fatalf("应上传 1 个文件，实际 %d", len(recs))
	}
	rec := recs[0]
	if string(rec.body) != "hello" {
		t.Fatalf("上传 body 必须是原始字节：%q", rec.body)
	}
	if rec.path != "/v1/node/tasks/t1/artifacts/"+wantSHA {
		t.Fatalf("上传路径不对：%s", rec.path)
	}
	if rec.token != "Bearer node-token" || rec.attempt != "1" ||
		rec.lease != "lease-9" || rec.artifactPath != "artifacts%2Fout%2Freport.md" {
		t.Fatalf("上传身份/路径 header 不对：%+v", rec)
	}
	// 托管副本是独立副本（结果目录里那份），用户 workDir 原文件内容未被改动。
	copyPath := filepath.Join(h.results.Root, hexNameForTest("t1"), "1", "artifacts", "out", "report.md")
	data, err := os.ReadFile(copyPath)
	if err != nil || string(data) != "hello" {
		t.Fatalf("独立托管副本缺失/内容不对：%q err=%v", data, err)
	}
}

func TestArtifactUploadFailureIsStructured(t *testing.T) {
	srv, _ := newUploadServer(t, http.StatusConflict, nil)
	h := newHarness(t)
	client := &upload.Client{BaseURL: srv.URL, NodeToken: "t", HTTP: srv.Client(), Retries: 0}
	h.r.opts.Upload = client

	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nprintf 'x' > \"$ANOTIFY_ARTIFACT_DIR/x.txt\"\n"
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")

	res := h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "t1"
	}, 5*time.Second).(*protocol.TaskResultFrame)
	if res.Phase != protocol.TaskPhaseFailed || res.Result.Error == nil ||
		res.Result.Error.Reason != protocol.ReasonArtifactUploadFailed {
		t.Fatalf("上传失败必须是 artifact_upload_failed：%+v", res)
	}
	if res.Result.Artifacts == nil {
		t.Fatal("artifacts 不能是 nil")
	}
}

// ---------- workDir / 导出入口安全 ----------

func TestUserWorkDirAncestorOfSystemRootRejected(t *testing.T) {
	h := newHarness(t)
	userDir := filepath.Join(h.dir, "user")
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// 用户 workDir 是系统 workRoot 的祖先（GC 会误删）→ workdir_unavailable。
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.WorkDir = h.dir
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")
	res := h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "t1"
	}, 3*time.Second).(*protocol.TaskResultFrame)
	if res.Result.Error == nil || res.Result.Error.Reason != protocol.ReasonWorkDirUnavailable {
		t.Fatalf("与系统根重叠必须是 workdir_unavailable：%+v", res)
	}
}

func TestFinishReleasesSlotAfterGroupReaped(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(nil)
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")
	h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "t1"
	}, 5*time.Second)
	h.r.mu.Lock()
	busy := 0
	for _, s := range h.r.slots {
		if s != nil && s.taskID != "" {
			busy++
		}
	}
	h.r.mu.Unlock()
	if busy != 0 {
		t.Fatalf("终态后槽位必须释放，仍有 %d 个占用", busy)
	}
}

func hexNameForTest(s string) string {
	const digits = "0123456789abcdef"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		b.WriteByte(digits[s[i]>>4])
		b.WriteByte(digits[s[i]&0x0f])
	}
	return b.String()
}

// ---------- A2：每 attempt 系统导出目录 ----------

func TestEmptyExportDirIsValidEmptyArtifacts(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(nil) // 脚本只 echo hi，不写导出目录
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")

	res := h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "t1"
	}, 5*time.Second).(*protocol.TaskResultFrame)
	if res.Phase != protocol.TaskPhaseSucceeded {
		t.Fatalf("空导出目录是真空，应成功：%+v", res)
	}
	if res.Result.Artifacts == nil || len(res.Result.Artifacts) != 0 {
		t.Fatalf("空 artifacts 必须是 []（非 nil）：%#v", res.Result.Artifacts)
	}
}

func TestExportDirPerAttemptWithSharedUserWorkDir(t *testing.T) {
	h := newHarness(t)
	userDir := filepath.Join(h.dir, "shared-user-dir")
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.WorkDir = userDir
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\n" +
			"echo \"$ANOTIFY_ARTIFACT_DIR\" >> dirs.txt\n" +
			"echo \"$ANOTIFY_TASK_ID\" > \"$ANOTIFY_ARTIFACT_DIR/out.txt\"\n"
	})
	run := func(taskID string) *protocol.TaskResultFrame {
		h.dispatch(taskID, 1, a, digest)
		h.waitUntil(t, taskID+" accept", func() bool { return hasAccept(h.sender.snapshot(), taskID) }, 3*time.Second)
		h.grant(taskID, 1, "lease-"+taskID, "token-"+taskID)
		return h.waitFrame(t, func(f any) bool {
			r, ok := f.(*protocol.TaskResultFrame)
			return ok && r.TaskID == taskID
		}, 5*time.Second).(*protocol.TaskResultFrame)
	}
	r1 := run("t1")
	r2 := run("t2")
	if r1.Phase != protocol.TaskPhaseSucceeded || r2.Phase != protocol.TaskPhaseSucceeded {
		t.Fatalf("两个任务都应成功：%+v / %+v", r1, r2)
	}
	// 各自只收集自己的导出文件（内容=taskID），目录不同。
	for _, r := range []*protocol.TaskResultFrame{r1, r2} {
		if len(r.Result.Artifacts) != 1 || r.Result.Artifacts[0].Path != "artifacts/out.txt" {
			t.Fatalf("每个 attempt 只应收集自己的导出：%s → %+v", r.TaskID, r.Result.Artifacts)
		}
	}
	if r1.Result.Artifacts[0].SHA256 == r2.Result.Artifacts[0].SHA256 {
		t.Fatal("两个任务共享 workDir 时不能收集到同一份内容（导出目录必须隔离）")
	}
	data, err := os.ReadFile(filepath.Join(userDir, "dirs.txt"))
	if err != nil {
		t.Fatalf("读 dirs.txt: %v", err)
	}
	lines := strings.Fields(string(data))
	if len(lines) != 2 || lines[0] == lines[1] {
		t.Fatalf("两个 attempt 的 ANOTIFY_ARTIFACT_DIR 必须不同：%q", lines)
	}
	// 用户 workDir 里不创建专用导出目录，原文件也不被删。
	if _, err := os.Stat(filepath.Join(userDir, "dirs.txt")); err != nil {
		t.Fatalf("用户 workDir 原文件不能被删：%v", err)
	}
}

func TestRetryAttemptDoesNotMixOldExport(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\n" +
			"echo \"$ANOTIFY_TASK_ID-$ANOTIFY_ARTIFACT_DIR\" > \"$ANOTIFY_ARTIFACT_DIR/attempt.txt\"\n"
	})
	run := func(attempt int) *protocol.TaskResultFrame {
		h.dispatch("t1", attempt, a, digest)
		h.waitUntil(t, "attempt2 accept", func() bool {
			return countAccepts(h.sender.snapshot(), "t1") >= attempt
		}, 3*time.Second)
		h.grant("t1", attempt, "lease-"+fmt.Sprint(attempt), "token-"+fmt.Sprint(attempt))
		return h.waitFrame(t, func(f any) bool {
			r, ok := f.(*protocol.TaskResultFrame)
			return ok && r.TaskID == "t1" && r.Attempt == attempt
		}, 5*time.Second).(*protocol.TaskResultFrame)
	}
	r1 := run(1)
	r2 := run(2)
	if r1.Phase != protocol.TaskPhaseSucceeded || r2.Phase != protocol.TaskPhaseSucceeded {
		t.Fatalf("两次 attempt 都应成功：%+v / %+v", r1, r2)
	}
	if len(r1.Result.Artifacts) != 1 || len(r2.Result.Artifacts) != 1 {
		t.Fatalf("每次 attempt 应各有 1 个导出：%+v / %+v", r1.Result.Artifacts, r2.Result.Artifacts)
	}
	if r1.Result.Artifacts[0].SHA256 == r2.Result.Artifacts[0].SHA256 {
		t.Fatal("同 task 重试不能混用旧 attempt 的导出目录")
	}
}

func TestExportDirSymlinkEscapeFails(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nln -s /etc/passwd \"$ANOTIFY_ARTIFACT_DIR/leak\"\n"
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")
	res := h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "t1"
	}, 5*time.Second).(*protocol.TaskResultFrame)
	if res.Phase != protocol.TaskPhaseFailed || res.Result.Error == nil ||
		res.Result.Error.Reason != protocol.ReasonArtifactExportInvalid {
		t.Fatalf("导出目录里的符号链接必须明确 failed/artifact_export_invalid：%+v", res)
	}
	if len(res.Result.Artifacts) != 0 {
		t.Fatalf("失败时不能登记符号链接内容：%+v", res.Result.Artifacts)
	}
}

func TestDualStreamLogsCaptured(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\necho out-marker\necho err-marker >&2\n"
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")
	res := h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "t1"
	}, 5*time.Second).(*protocol.TaskResultFrame)
	if res.Phase != protocol.TaskPhaseSucceeded {
		t.Fatalf("双流脚本应成功：%+v", res)
	}
	dir := filepath.Join(h.results.Root, hexNameForTest("t1"), "1", "logs")
	stdout, err := os.ReadFile(filepath.Join(dir, "stdout.log"))
	if err != nil || !strings.Contains(string(stdout), "out-marker") || strings.Contains(string(stdout), "err-marker") {
		t.Fatalf("stdout.log 不对：%q err=%v", stdout, err)
	}
	stderr, err := os.ReadFile(filepath.Join(dir, "stderr.log"))
	if err != nil || !strings.Contains(string(stderr), "err-marker") || strings.Contains(string(stderr), "out-marker") {
		t.Fatalf("stderr.log 不对：%q err=%v", stderr, err)
	}
}
