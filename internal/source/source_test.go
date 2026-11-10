package source

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
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
	if len(again.Tasks) != 0 {
		t.Fatalf("重复投递不得重复派发: %+v", again.Tasks)
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
