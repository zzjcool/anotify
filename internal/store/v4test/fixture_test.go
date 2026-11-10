package v4test

// fixture_test.go 是 v4test（外部测试包）的**共享 v4 夹具门面**（G1-6 A 轮交付）。
//
// 既有 helpers_test.go / g14_helpers_test.go 是分任务长出来的：G1-2/3 一份、G1-4
// 一份，命名口径（newTask/grant/testCap）已经散在几十个用例里。本文件只补两类东西：
//
//  1. 后续白盒（B）与迁移用例反复需要的**参数化构造器**（lifecycle/concurrency/
//     resultTtl 一次写清），避免每个用例手拼 artifact.Spec 时写错一个字段；
//  2. 需要「跨包可见」的夹具入口：store 包内测试不能 import 本包的 _test 符号，
//     所以这里导出的形状必须只依赖 store 的**公开 API**（本来就是外部测试包的纪律）。
//
// 纪律与其它夹具一致：不提供任何绕过认证的钩子，grant 走真实 assign → 认证身份
// + 真实 slot；不造 fake alias 兼容旧 v3 字段。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// fixtureCapOpts 是 fixtureCap 的可选内容字段（零值 = 用 v4 默认）。
type fixtureCapOpts struct {
	Params       string
	Next         []string
	Script       string
	Idempotent   bool
	MaxAttempts  int
	ResultTTLSec int
	MaxPerNode   int
	TimeoutSec   int
}

// fixtureSpec 用真实正文造一份归一化后的 artifact.Spec（脚本是唯一实现形式）。
func fixtureSpec(opts fixtureCapOpts) artifact.Spec {
	params := opts.Params
	if params == "" {
		params = `{"type":"object"}`
	}
	script := opts.Script
	if script == "" {
		script = "#!/bin/sh\nexit 0\n"
	}
	spec := artifact.Spec{
		Params: json.RawMessage(params),
		Next:   append([]string{}, opts.Next...),
		Run: artifact.Run{
			Entrypoint: "run.sh",
			Files:      map[string]string{"run.sh": script},
		},
		Lifecycle: artifact.Lifecycle{
			Idempotent: opts.Idempotent,
			TimeoutSec: opts.TimeoutSec,
		},
	}
	if opts.MaxAttempts > 0 {
		n := opts.MaxAttempts
		spec.Lifecycle.MaxAttempts = &n
	}
	if opts.ResultTTLSec > 0 {
		n := opts.ResultTTLSec
		spec.Lifecycle.ResultTTLSec = &n
	}
	if opts.MaxPerNode > 0 {
		n := opts.MaxPerNode
		spec.Concurrency.MaxPerNode = &n
	}
	return spec
}

// fixtureCap 建一个内容受控的能力行（version>0 = 显式版本，0 = 内容寻址）。
func fixtureCap(t *testing.T, db *store.DB, nsID, name string, version int, opts fixtureCapOpts) *store.CapabilityVersion {
	t.Helper()
	return createCap(t, db, &store.CapabilityVersion{
		NamespaceID: nsID,
		Name:        name,
		Version:     version,
		Spec:        fixtureSpec(opts),
	})
}

// fixtureEventAt 造一条 created_at/时间窗都显式的事件（保留期与分页用例需要）。
//
// now 是「投递被接收的时刻」，CreatedAt 默认跟随 now；windowSec 是去重窗口
// （0 = 无窗口，仍要求 DeliveryID 才会去重）。
func fixtureEventAt(nsID, source, typ string, at int64) *store.Event {
	return &store.Event{
		NamespaceID: nsID,
		TraceID:     "tr_" + store.NewEventID(),
		Source:      source,
		Type:        typ,
		CreatedAt:   at,
	}
}

// fixtureComplete 以当前认证身份 + 当前 attempt/lease 提交一次完整结果事实。
//
// 结果里的时间戳是真实纳秒时间（不是借来的 startedAt），exitCode 用指针表达
// 「Node 明确报了 0」与「Node 没报」的区别。
func fixtureComplete(t *testing.T, db *store.DB, nsID, nodeID, taskID, leaseID string, attempt int, exitCode *int) store.TaskResultDisposition {
	t.Helper()
	start := time.Now().UTC().Truncate(time.Microsecond)
	finished := start.Add(2 * time.Second)
	result := store.TaskResult{
		Phase:      store.TaskSucceeded,
		Summary:    "done",
		Artifacts:  []protocol.TaskArtifact{},
		ExitCode:   exitCode,
		StartedAt:  start,
		FinishedAt: finished,
	}
	disp, err := db.CompleteTaskClassifiedAuthorized(context.Background(), nodeAuthByID(t, db, nodeID),
		taskID, attempt, leaseID, store.Now(), result)
	if err != nil {
		t.Fatalf("complete task %s: %v", taskID, err)
	}
	return disp
}

// fixtureGrantThenComplete 是「建任务 → 派发 → 授权 → 完成」的整条真实链路，
// 返回租约与结果分类，供跨层用例（event/retention/deployment/download）复用。
func fixtureGrantThenComplete(t *testing.T, db *store.DB, nsID, nodeID, traceID string,
	c *store.CapabilityVersion, exitCode *int) (taskID, leaseID string, disp store.TaskResultDisposition) {
	t.Helper()
	task := newTask(t, db, nsID, c, traceID, "")
	leaseID, _, attempt := grant(t, db, task.ID, nodeID, "asg-"+task.ID)
	disp = fixtureComplete(t, db, nsID, nodeID, task.ID, leaseID, attempt, exitCode)
	return task.ID, leaseID, disp
}
