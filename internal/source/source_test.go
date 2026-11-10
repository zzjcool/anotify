package source

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

func TestIngestManualUnifiedReceiptAndTask(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "analyze", `{"type":"object","properties":{"env":{"type":"string"}},"required":["env"]}`)
	src := testSource(t, db, ns.ID, "manual-1", sourceOpts{
		Kind:   sourcespec.KindManual,
		Config: `{"payload":{"env":"prod"}}`,
		Targets: []sourcespec.Target{{
			Capability: "analyze",
			Params:     map[string]any{"env": "$(payload.env)"},
		}},
		Enabled: true,
	})
	receipt, err := newService(t, db).Ingest(testCtx(), Input{Source: src})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if receipt.EventID == "" || receipt.TraceID == "" {
		t.Fatalf("回执必须有 eventId/traceId: %+v", receipt)
	}
	if receipt.Duplicate {
		t.Fatal("首次投递不是重复")
	}
	if len(receipt.Tasks) != 1 || len(receipt.Pruned) != 0 {
		t.Fatalf("回执 tasks/pruned 不对: %+v", receipt)
	}
	event, err := db.GetEvent(testCtx(), ns.ID, receipt.EventID)
	if err != nil {
		t.Fatalf("读事件: %v", err)
	}
	if event.Source != "manual-1" || event.Type != "manual-1" || event.TraceID != receipt.TraceID {
		t.Fatalf("事件行不对: %+v", event)
	}
	task, err := db.GetTask(testCtx(), ns.ID, receipt.Tasks[0].TaskID)
	if err != nil {
		t.Fatalf("读任务: %v", err)
	}
	if task.Phase != store.TaskPending || task.ParentTaskID != nil || task.TraceID != receipt.TraceID {
		t.Fatalf("入口任务应 pending 且无父任务: %+v", task)
	}
	if task.Params == nil || string(task.Params) == "{}" {
		t.Fatalf("模板参数未落到任务: %s", task.Params)
	}
}

func TestIngestNoTargetPruneAndEvent(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	src := testSource(t, db, ns.ID, "manual-empty", sourceOpts{
		Kind: sourcespec.KindManual, Config: `{}`, Enabled: true,
	})
	receipt, err := newService(t, db).Ingest(testCtx(), Input{Source: src})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(receipt.Tasks) != 0 {
		t.Fatalf("空 targets 不该建任务: %+v", receipt.Tasks)
	}
	if len(receipt.Pruned) != 1 || receipt.Pruned[0].Reason != protocol.ReasonNoTarget {
		t.Fatalf("应留 no_target 剪枝回执: %+v", receipt.Pruned)
	}
	if countRows(t, db, "events") != 2 { // 1 条 ingested + 1 条 prune 留痕
		t.Fatalf("事件行数 = %d，want 2", countRows(t, db, "events"))
	}
}

func TestIngestParamPruneIsPerTarget(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "good", `{"type":"object"}`)
	testCapability(t, db, ns.ID, "needs-env", `{"type":"object","properties":{"env":{"type":"string"}},"required":["env"]}`)
	src := testSource(t, db, ns.ID, "manual-fanout", sourceOpts{
		Kind: sourcespec.KindManual, Config: `{}`,
		Targets: []sourcespec.Target{
			{Capability: "good"},
			{Capability: "needs-env", Params: map[string]any{"env": "$(payload.env)"}},
		},
		Enabled: true,
	})
	receipt, err := newService(t, db).Ingest(testCtx(), Input{Source: src})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(receipt.Tasks) != 1 || len(receipt.Pruned) != 1 {
		t.Fatalf("扇出应一条成功一条剪枝: %+v", receipt)
	}
	if receipt.Pruned[0].Reason != protocol.ReasonParamMissing {
		t.Fatalf("剪枝原因 = %q，want param_missing", receipt.Pruned[0].Reason)
	}
	if receipt.Tasks[0].Capability != "good" {
		t.Fatalf("成功的那条应是 good: %+v", receipt.Tasks[0])
	}
}

func TestIngestPayloadSchemaPrune(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "analyze", `{"type":"object"}`)
	src := testSource(t, db, ns.ID, "manual-schema", sourceOpts{
		Kind:    sourcespec.KindManual,
		Config:  `{"payloadSchema":{"type":"object","properties":{"environment":{"type":"string"}},"required":["environment"]}}`,
		Targets: []sourcespec.Target{{Capability: "analyze"}},
		Enabled: true,
	})
	receipt, err := newService(t, db).Ingest(testCtx(), Input{Source: src, Payload: json.RawMessage(`{"other":1}`)})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(receipt.Tasks) != 0 {
		t.Fatalf("载荷不合契约不得建任务: %+v", receipt.Tasks)
	}
	if len(receipt.Pruned) != 1 || receipt.Pruned[0].Reason != protocol.ReasonEventSchema {
		t.Fatalf("应剪枝 event_schema: %+v", receipt.Pruned)
	}
	if receipt.EventID == "" {
		t.Fatal("event_schema 剪枝仍必须返回合法 eventId（入库）")
	}
}

func TestIngestDuplicateDeliveryReturnsOriginal(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "analyze", `{"type":"object"}`)
	src := testSource(t, db, ns.ID, "hook-1", sourceOpts{
		Kind: sourcespec.KindWebhook, Config: `{}`,
		Targets: []sourcespec.Target{{Capability: "analyze"}},
		Enabled: true,
	})
	service := newService(t, db)
	first, err := service.Ingest(testCtx(), Input{Source: src, DeliveryID: "delivery-1", Payload: json.RawMessage(`{"a":1}`)})
	if err != nil {
		t.Fatalf("首次投递: %v", err)
	}
	again, err := service.Ingest(testCtx(), Input{Source: src, DeliveryID: "delivery-1", Payload: json.RawMessage(`{"a":1}`)})
	if err != nil {
		t.Fatalf("重复投递: %v", err)
	}
	if !again.Duplicate || again.EventID != first.EventID || again.TraceID != first.TraceID {
		t.Fatalf("窗口内重复投递应回原 event/trace: first=%+v again=%+v", first, again)
	}
	// 冻结语义：duplicate 返回第一次的完整 receipt（含同一批 task），不是空回执、不重算。
	if len(again.Tasks) != len(first.Tasks) || len(again.Pruned) != len(first.Pruned) {
		t.Fatalf("重复投递必须返回第一次的完整回执: first=%+v again=%+v", first, again)
	}
	if len(first.Tasks) != 1 || again.Tasks[0].TaskID != first.Tasks[0].TaskID {
		t.Fatalf("重复投递不得换 task id: first=%+v again=%+v", first.Tasks, again.Tasks)
	}
	if got := countRows(t, db, "tasks"); got != 1 {
		t.Fatalf("任务行数 = %d，want 1", got)
	}
}

func TestIngestManualPayloadOverridesConfigOneLevel(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "echo", `{"type":"object","properties":{"env":{"type":"string"},"x":{"type":"string"}}}`)
	src := testSource(t, db, ns.ID, "manual-defaults", sourceOpts{
		Kind:   sourcespec.KindManual,
		Config: `{"payload":{"env":"prod","x":"keep"}}`,
		Targets: []sourcespec.Target{{
			Capability: "echo",
			Params:     map[string]any{"env": "$(payload.env)", "x": "$(payload.x)"},
		}},
		Enabled: true,
	})
	receipt, err := newService(t, db).Ingest(testCtx(), Input{
		Source: src, Payload: json.RawMessage(`{"env":"stage"}`),
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(receipt.Tasks) != 1 {
		t.Fatalf("应建一条任务: %+v", receipt)
	}
	task, err := db.GetTask(testCtx(), ns.ID, receipt.Tasks[0].TaskID)
	if err != nil {
		t.Fatalf("读任务: %v", err)
	}
	var params map[string]any
	if err := json.Unmarshal(task.Params, &params); err != nil {
		t.Fatalf("解码 params: %v", err)
	}
	if params["env"] != "stage" || params["x"] != "keep" {
		t.Fatalf("一层覆盖未生效（env 应被投递体覆盖, x 应保留配置）: %v", params)
	}
}

func TestPreviewWritesNothing(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "analyze", `{"type":"object","properties":{"env":{"type":"string"}},"required":["env"]}`)
	// 未保存的源也能预览（source.md §5.3）：这里刻意在内存里放一个含不可解析
	// 目标的文档——保存路径会拒它，预览必须能说明「这一条会 missing」。
	src := &store.Source{
		NamespaceID: ns.ID, Name: "manual-preview", Kind: sourcespec.KindManual,
		Config: json.RawMessage(`{"payload":{"env":"prod"}}`),
		Targets: []sourcespec.Target{
			{Capability: "analyze", Params: map[string]any{"env": "$(payload.env)"}},
			{Capability: "ghost"},
		},
		Enabled: true,
	}
	before := map[string]int{
		"events": countRows(t, db, "events"), "tasks": countRows(t, db, "tasks"),
		"event_deliveries": countRows(t, db, "event_deliveries"),
	}
	preview, err := newService(t, db).PreviewSource(testCtx(), src, nil)
	if err != nil {
		t.Fatalf("PreviewSource: %v", err)
	}
	if len(preview.Targets) != 2 {
		t.Fatalf("预览应逐条列出 targets: %+v", preview.Targets)
	}
	if preview.Targets[0].State != "ready" || preview.Targets[1].State != "missing" {
		t.Fatalf("预览状态不对: %+v", preview.Targets)
	}
	if preview.Targets[1].Reason != protocol.ReasonDownstreamMissing {
		t.Fatalf("missing 目标原因 = %q，want downstream_missing", preview.Targets[1].Reason)
	}
	for table, want := range before {
		if got := countRows(t, db, table); got != want {
			t.Fatalf("dry-run 写了 %s：%d → %d", table, want, got)
		}
	}

	// 坏载荷预览：event_schema 剪枝，同样 0 写入。
	badPayload, err := newService(t, db).PreviewSource(testCtx(), src, json.RawMessage(`[1,2]`))
	if !errors.Is(err, ErrBadPayload) || badPayload != nil {
		t.Fatalf("非对象载荷预览应 ErrBadPayload，得到 %v/%v", badPayload, err)
	}
}

func TestHookDisabledIsNotFound(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	src := testSource(t, db, ns.ID, "hook-disabled", sourceOpts{
		Kind: sourcespec.KindWebhook, Config: `{}`, Enabled: false,
	})
	cfg := sourcespec.ParseConfig(src.Kind, src.Config)
	_, err := newService(t, db).Hook(testCtx(), cfg.Token, HookBody{})
	if !errors.Is(err, store.ErrSourceNotFound) {
		t.Fatalf("停用源的 hook 必须 404 语义（ErrSourceNotFound），得到 %v", err)
	}
}

func TestIngestRejectsNonObjectPayload(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	src := testSource(t, db, ns.ID, "manual-bad", sourceOpts{
		Kind: sourcespec.KindManual, Config: `{}`, Enabled: true,
	})
	_, err := newService(t, db).Ingest(testCtx(), Input{Source: src, Payload: json.RawMessage(`"text"`)})
	if !errors.Is(err, ErrBadPayload) {
		t.Fatalf("非对象载荷必须 ErrBadPayload，得到 %v", err)
	}
	if got := countRows(t, db, "events"); got != 0 {
		t.Fatalf("坏载荷不得落事件，events = %d", got)
	}
}

func TestFireDisabledSourceRejected(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	src := testSource(t, db, ns.ID, "manual-off", sourceOpts{
		Kind: sourcespec.KindManual, Config: `{}`, Enabled: false,
	})
	_, err := newService(t, db).Fire(testCtx(), ns.ID, src.Name, nil)
	if !errors.Is(err, ErrSourceDisabled) {
		t.Fatalf("停用源 fire 必须 ErrSourceDisabled，得到 %v", err)
	}
}

func TestIngestDeliveryIDScopeAndAbsence(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "analyze", `{"type":"object"}`)
	sourceA := testSource(t, db, ns.ID, "manual-a", sourceOpts{
		Kind: sourcespec.KindManual, Config: `{}`,
		Targets: []sourcespec.Target{{Capability: "analyze"}}, Enabled: true,
	})
	sourceB := testSource(t, db, ns.ID, "manual-b", sourceOpts{
		Kind: sourcespec.KindManual, Config: `{}`,
		Targets: []sourcespec.Target{{Capability: "analyze"}}, Enabled: true,
	})
	service := newService(t, db)
	// 没有 deliveryId：每次都是新事件（无自动去重）。
	if _, err := service.Ingest(testCtx(), Input{Source: sourceA}); err != nil {
		t.Fatalf("第一次无 id 投递: %v", err)
	}
	second, err := service.Ingest(testCtx(), Input{Source: sourceA})
	if err != nil {
		t.Fatalf("第二次无 id 投递: %v", err)
	}
	if second.Duplicate {
		t.Fatal("没有 deliveryId 不得按调用序号去重")
	}
	// 空串 deliveryId 等同没有：同样每次新事件。
	third, err := service.Ingest(testCtx(), Input{Source: sourceA, DeliveryID: ""})
	if err != nil || third.Duplicate {
		t.Fatalf("空 deliveryId 应按缺失处理: %v/%+v", err, third)
	}
	// 幂等域来自源：同 id 在另一个源不合并。
	other, err := service.Ingest(testCtx(), Input{Source: sourceB, DeliveryID: "dup-1"})
	if err != nil || other.Duplicate {
		t.Fatalf("幂等域必须按源隔离: %v/%+v", err, other)
	}
	again, err := service.Ingest(testCtx(), Input{Source: sourceB, DeliveryID: "dup-1"})
	if err != nil || !again.Duplicate {
		t.Fatalf("同源同 id 必须去重: %v/%+v", err, again)
	}
	if again.EventID != other.EventID {
		t.Fatalf("去重必须回原 event: %s != %s", again.EventID, other.EventID)
	}
}

// testCapabilityVersion 建一个指定 version 的能力（source 用例需要“两个版本”场景）。
func testCapabilityVersion(t *testing.T, db *store.DB, namespaceID, name string, version int, params string) *store.CapabilityVersion {
	t.Helper()
	if params == "" {
		params = `{"type":"object"}`
	}
	capability := &store.CapabilityVersion{
		NamespaceID: namespaceID, Name: name, Version: version,
		Spec: artifact.Spec{
			Params: json.RawMessage(params),
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
			},
		},
	}
	if err := db.CreateCapability(testCtx(), capability); err != nil {
		t.Fatalf("建能力 %s@%d: %v", name, version, err)
	}
	got, err := db.GetCapability(testCtx(), namespaceID, name, version)
	if err != nil {
		t.Fatalf("回读能力: %v", err)
	}
	return got
}

// TestIngestMissingTargetCreatesFailedTaskNotPrune 钉住 R1-C3：已保存源里指向后来被
// 删除能力的 target 解析不到时，事件接收成功、落**失败 Task（attempt 0）+ 错误事件**，
// 回执把它列在 tasks（真实请求 ref/声明 version），不是剪枝。
func TestIngestMissingTargetCreatesFailedTaskNotPrune(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	consumer := testCapabilityVersion(t, db, ns.ID, "consumer", 2, "")
	src := testSource(t, db, ns.ID, "manual-missing", sourceOpts{
		Kind: sourcespec.KindManual, Config: `{}`,
		Targets: []sourcespec.Target{{Capability: "consumer@2"}}, Enabled: true,
	})
	if err := db.DeleteCapability(testCtx(), ns.ID, "consumer",
		store.ResourceVersion(consumer.ID, consumer.ResourceVersion)); err != nil {
		t.Fatalf("删除能力制造 missing: %v", err)
	}
	receipt, err := newService(t, db).Ingest(testCtx(), Input{Source: src})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(receipt.Pruned) != 0 {
		t.Fatalf("downstream_missing 不得进入 pruned: %+v", receipt.Pruned)
	}
	if len(receipt.Tasks) != 1 {
		t.Fatalf("回执 tasks 必须列出真实建立的 failed Task: %+v", receipt.Tasks)
	}
	ref := receipt.Tasks[0]
	if ref.TaskID == "" || ref.Capability != "consumer@2" || ref.Version != 2 {
		t.Fatalf("failed Task 回执必须是真实请求 ref/声明 version: %+v", ref)
	}
	task, err := db.GetTask(testCtx(), ns.ID, ref.TaskID)
	if err != nil {
		t.Fatalf("读 failed task: %v", err)
	}
	if task.Phase != store.TaskFailed || task.Attempt != 0 || task.CapabilityID != "" ||
		task.Capability != "consumer@2" || task.Version != 2 || task.Digest != "" ||
		task.Error != protocol.ReasonDownstreamMissing || task.ParentTaskID != nil ||
		task.StartedAt != nil || task.ExitCode != nil {
		t.Fatalf("failed Task 形状不对（无 Node/过程事实）: %+v", task)
	}
	// 错误事件：真实请求 ref + taskId。
	events, err := db.ListEvents(testCtx(), ns.ID, receipt.TraceID, "", 50)
	if err != nil {
		t.Fatalf("列事件: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == protocol.ReasonDownstreamMissing && strings.Contains(string(ev.Payload), ref.TaskID) {
			found = true
		}
	}
	if !found {
		t.Fatalf("downstream_missing 错误事件缺真实 taskId: %+v", events)
	}
}

// TestIngestDuplicateReturnsFirstReceiptWithFailuresAndPrunes duplicate 必须返回第一次
// 的完整回执（含 failed Task 与纯剪枝），不新建任务/事件。
func TestIngestDuplicateReturnsFirstReceiptWithFailuresAndPrunes(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "good", "")
	testCapability(t, db, ns.ID, "needs-env", `{"type":"object","properties":{"env":{"type":"string"}},"required":["env"]}`)
	consumer := testCapabilityVersion(t, db, ns.ID, "consumer", 2, "")
	src := testSource(t, db, ns.ID, "hook-mixed", sourceOpts{
		Kind: sourcespec.KindWebhook, Config: `{}`,
		Targets: []sourcespec.Target{
			{Capability: "good"},
			{Capability: "needs-env", Params: map[string]any{"env": "$(payload.env)"}},
			{Capability: "consumer@2"},
		},
		Enabled: true,
	})
	if err := db.DeleteCapability(testCtx(), ns.ID, "consumer",
		store.ResourceVersion(consumer.ID, consumer.ResourceVersion)); err != nil {
		t.Fatalf("删除能力制造 missing: %v", err)
	}
	service := newService(t, db)
	first, err := service.Ingest(testCtx(), Input{
		Source: src, DeliveryID: "delivery-mixed", Payload: json.RawMessage(`{"a":1}`),
	})
	if err != nil {
		t.Fatalf("首次投递: %v", err)
	}
	if len(first.Tasks) != 2 || len(first.Pruned) != 1 ||
		first.Pruned[0].Reason != protocol.ReasonParamMissing {
		t.Fatalf("首次回执投影不对（正常 + failed + 纯剪枝）: %+v", first)
	}
	tasksBefore := countRows(t, db, "tasks")
	eventsBefore := countRows(t, db, "events")
	again, err := service.Ingest(testCtx(), Input{
		Source: src, DeliveryID: "delivery-mixed", Payload: json.RawMessage(`{"a":1}`),
	})
	if err != nil {
		t.Fatalf("重复投递: %v", err)
	}
	if !again.Duplicate || again.EventID != first.EventID || again.TraceID != first.TraceID {
		t.Fatalf("duplicate 必须回第一次 event/trace: first=%+v again=%+v", first, again)
	}
	if len(again.Tasks) != len(first.Tasks) || again.Tasks[0].TaskID != first.Tasks[0].TaskID ||
		again.Tasks[1].TaskID != first.Tasks[1].TaskID {
		t.Fatalf("duplicate 必须回第一次的同一批 task（含 failed）: first=%+v again=%+v", first.Tasks, again.Tasks)
	}
	if len(again.Pruned) != 1 || again.Pruned[0].Reason != protocol.ReasonParamMissing {
		t.Fatalf("duplicate 必须回第一次的剪枝回执: %+v", again.Pruned)
	}
	if countRows(t, db, "tasks") != tasksBefore || countRows(t, db, "events") != eventsBefore {
		t.Fatalf("duplicate 产生了新写: tasks %d→%d events %d→%d",
			tasksBefore, countRows(t, db, "tasks"), eventsBefore, countRows(t, db, "events"))
	}
}

// TestIngestSourceRevisionMismatchWritesNothing 源在计划与接收之间变了 → 明确错误 +
// 0 写（不吞错、不用陈旧参数接收）。
func TestIngestSourceRevisionMismatchWritesNothing(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "analyze", "")
	src := testSource(t, db, ns.ID, "manual-cas", sourceOpts{
		Kind: sourcespec.KindManual, Config: `{}`,
		Targets: []sourcespec.Target{{Capability: "analyze"}}, Enabled: true,
	})
	stale := *src
	updated := *src
	updated.DisplayName = "renamed"
	if err := db.UpdateSource(testCtx(), &updated); err != nil {
		t.Fatalf("更新源: %v", err)
	}
	eventsBefore := countRows(t, db, "events")
	tasksBefore := countRows(t, db, "tasks")
	_, err := newService(t, db).Ingest(testCtx(), Input{Source: &stale})
	if err == nil {
		t.Fatal("源 resource_version 失配必须报错（不能吞掉用陈旧源接收）")
	}
	if countRows(t, db, "events") != eventsBefore || countRows(t, db, "tasks") != tasksBefore {
		t.Fatalf("失配路径必须 0 写: events %d→%d tasks %d→%d",
			eventsBefore, countRows(t, db, "events"), tasksBefore, countRows(t, db, "tasks"))
	}
}

// TestPreviewPerTargetReasons dry-run 每个 target 必须带自身 Reason/Detail，
// 不能用 Pruned[0] 兜给所有 target（R1-C3）。
func TestPreviewPerTargetReasons(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "needs-env", `{"type":"object","properties":{"env":{"type":"string"}},"required":["env"]}`)
	testCapability(t, db, ns.ID, "needs-count", `{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"]}`)
	src := &store.Source{
		NamespaceID: ns.ID, Name: "manual-preview", Kind: sourcespec.KindManual,
		Targets: []sourcespec.Target{
			{Capability: "needs-env", Params: map[string]any{"env": "$(payload.env)"}},
			{Capability: "needs-count", Params: map[string]any{"count": "$(payload.text)"}},
		},
		Enabled: true,
	}
	preview, err := newService(t, db).PreviewSource(testCtx(), src, json.RawMessage(`{"text":"abc"}`))
	if err != nil {
		t.Fatalf("PreviewSource: %v", err)
	}
	if len(preview.Targets) != 2 {
		t.Fatalf("预览应逐条列出 target: %+v", preview.Targets)
	}
	if preview.Targets[0].State != route.StatePruned || preview.Targets[0].Reason != protocol.ReasonParamMissing {
		t.Fatalf("target[0] 原因 = %+v，want param_missing", preview.Targets[0])
	}
	if preview.Targets[1].State != route.StatePruned || preview.Targets[1].Reason != protocol.ReasonParamType {
		t.Fatalf("target[1] 原因 = %+v，want param_type（不能用 Pruned[0] 兜）", preview.Targets[1])
	}
	if len(preview.Pruned) != 2 || preview.Pruned[0].Reason != protocol.ReasonParamMissing ||
		preview.Pruned[1].Reason != protocol.ReasonParamType {
		t.Fatalf("aggregate pruned 与逐目标原因必须一致: %+v", preview.Pruned)
	}
}
