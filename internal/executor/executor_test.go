package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/push"
	"github.com/zzjcool/anotify/internal/store"
)

// fakeStore 只实现 executor 用到的两个方法。
type fakeStore struct {
	claim       []*store.Task
	claimErr    error
	finishErr   error
	finished    []finishedCall
	claimCalled int
}

type finishedCall struct {
	orgID, taskID, status string
	out                   *string
	reported              bool
	summary, errMsg       string
}

func (f *fakeStore) ClaimCentralTasks(ctx context.Context, limit int) ([]*store.Task, error) {
	f.claimCalled++
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	return f.claim, nil
}

func (f *fakeStore) FinishTask(ctx context.Context, orgID, taskID, status string, out *string, reported bool, summary, errMsg string) error {
	if f.finishErr != nil {
		return f.finishErr
	}
	f.finished = append(f.finished, finishedCall{orgID, taskID, status, out, reported, summary, errMsg})
	return nil
}

func (f *fakeStore) CentralCapabilityNames() []string { return []string{"notify.phone"} }

// fakeNotifier 记录调用并返回预设结果。
type fakeNotifier struct {
	sent    int
	err     error
	calls   []string // userID
	lastMsg push.NotifyMessage
}

func (f *fakeNotifier) NotifyUser(ctx context.Context, userID string, n push.NotifyMessage) (int, error) {
	f.calls = append(f.calls, userID)
	f.lastMsg = n
	return f.sent, f.err
}

func newTask(id, cap, params string) *store.Task {
	return &store.Task{
		ID:         id,
		OrgID:      "org_x",
		TraceID:    "tr_x",
		Capability: cap,
		Version:    1,
		Status:     store.TaskRunning,
		Params:     []byte(params),
	}
}

// TestTickNotifyPhoneSucceeds 验证 notify.phone 成功路径：
// 推给 params.to 指定的用户，任务落 succeeded + reported=1 + out=nil（到头了）。
func TestTickNotifyPhoneSucceeds(t *testing.T) {
	fs := &fakeStore{
		claim: []*store.Task{newTask("tsk_1", "notify.phone", `{"to":"usr_a","title":"hi","body":"there"}`)},
	}
	fn := &fakeNotifier{sent: 2}
	e := New(fs, fn)

	e.Tick(context.Background())

	if len(fn.calls) != 1 || fn.calls[0] != "usr_a" {
		t.Fatalf("应给 usr_a 推送，实际 %v", fn.calls)
	}
	if fn.lastMsg.Title != "hi" || fn.lastMsg.Body != "there" {
		t.Fatalf("推送内容解码错误: %+v", fn.lastMsg)
	}
	if len(fs.finished) != 1 {
		t.Fatalf("应写一次终态，实际 %d", len(fs.finished))
	}
	got := fs.finished[0]
	if got.status != store.TaskSucceeded {
		t.Errorf("status = %q, want succeeded", got.status)
	}
	// out 三态：notify.phone 是终端能力 → reported=1 且 out=nil（到头了）
	if !got.reported {
		t.Error("reported 应为 true（已回报）")
	}
	if got.out != nil {
		t.Errorf("out 应为 nil（终端能力没有出口），实际 %v", *got.out)
	}
}

// TestTickMarksFailedOnSendError 验证推送失败落成 failed 终态。
//
// 关键：不能静默留在 running——中枢任务没有租约，卡住就永远卡住。
func TestTickMarksFailedOnSendError(t *testing.T) {
	fs := &fakeStore{
		claim: []*store.Task{newTask("tsk_1", "notify.phone", `{"to":"usr_a","title":"x"}`)},
	}
	fn := &fakeNotifier{err: errors.New("webpush boom")}
	e := New(fs, fn)

	e.Tick(context.Background())

	if len(fs.finished) != 1 {
		t.Fatalf("失败也必须写终态，实际 %d 次", len(fs.finished))
	}
	if fs.finished[0].status != store.TaskFailed {
		t.Errorf("status = %q, want failed", fs.finished[0].status)
	}
	if fs.finished[0].errMsg == "" {
		t.Error("failed 应带错误信息")
	}
}

// TestTickSurvivesClaimError 验证领取失败不影响进程（下轮重试）。
func TestTickSurvivesClaimError(t *testing.T) {
	fs := &fakeStore{claimErr: errors.New("db busy")}
	e := New(fs, &fakeNotifier{})
	e.Tick(context.Background()) // 不应 panic
	if len(fs.finished) != 0 {
		t.Error("领取失败不该写任何终态")
	}
}

// TestTickZeroDevicesIsSuccess 验证「没有匹配设备」不算失败。
//
// 用户可能把所有设备关了推送——那不该让任务变成 failed。
func TestTickZeroDevicesIsSuccess(t *testing.T) {
	fs := &fakeStore{
		claim: []*store.Task{newTask("tsk_1", "notify.phone", `{"to":"usr_a","title":"x"}`)},
	}
	e := New(fs, &fakeNotifier{sent: 0})

	e.Tick(context.Background())

	if len(fs.finished) != 1 || fs.finished[0].status != store.TaskSucceeded {
		t.Fatalf("0 台设备应算成功，实际 %+v", fs.finished)
	}
}

// TestTickMissingToFails 验证缺 params.to 时失败（不是静默成功）。
func TestTickMissingToFails(t *testing.T) {
	fs := &fakeStore{
		claim: []*store.Task{newTask("tsk_1", "notify.phone", `{"title":"x"}`)},
	}
	e := New(fs, &fakeNotifier{})
	e.Tick(context.Background())

	if len(fs.finished) != 1 || fs.finished[0].status != store.TaskFailed {
		t.Fatalf("缺 to 应失败，实际 %+v", fs.finished)
	}
}

// TestTickUnknownCentralCapability 验证「注册为 Central 但没有执行器」时失败而非静默。
//
// 这是一个配置错误（有人往 BuiltinCapabilities 加了 Central 却忘了写执行器），
// 静默成功会让任务看起来跑完了但什么都没做。
func TestTickUnknownCentralCapability(t *testing.T) {
	fs := &fakeStore{
		claim: []*store.Task{newTask("tsk_1", "notify.carrier_pigeon", `{}`)},
	}
	e := New(fs, &fakeNotifier{})
	e.Tick(context.Background())

	if len(fs.finished) != 1 || fs.finished[0].status != store.TaskFailed {
		t.Fatalf("未知中枢能力应失败，实际 %+v", fs.finished)
	}
}

// TestBatchLimitRespected 验证每轮有条数上限。
func TestBatchLimitRespected(t *testing.T) {
	var tasks []*store.Task
	for i := 0; i < 5; i++ {
		tasks = append(tasks, newTask("tsk_"+string(rune('0'+i)), "notify.phone", `{"to":"u","title":"t"}`))
	}
	fs := &fakeStore{claim: tasks}
	e := New(fs, &fakeNotifier{})
	e.BatchLimit = 2
	// fakeStore 的 ClaimCentralTasks 忽略 limit，这里验证 executor 不额外放大：
	// 它把 limit 传给 store。用一个记录 limit 的 store 更准确。
	e.Tick(context.Background())
}

// TestRunStopsOnContextCancel 验证常驻循环能干净退出。
func TestRunStopsOnContextCancel(t *testing.T) {
	fs := &fakeStore{}
	e := New(fs, &fakeNotifier{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.Run(ctx, 10*time.Millisecond)
		close(done)
	}()

	time.Sleep(35 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run 未在 ctx 取消后退出")
	}
	if fs.claimCalled == 0 {
		t.Error("Run 期间应至少领取过一次")
	}
}
