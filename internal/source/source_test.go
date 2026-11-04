package source

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
)

// fakeStore 只实现 Trigger/Scheduler 需要的那几个方法。
type fakeStore struct {
	src    *store.Source
	due    []*store.Source
	events []*store.Event
	fired  []struct {
		ID            string
		FiredAt, Next int64
	}
	// getSourceNS 记录 GetSource 收到的命名空间（调度器必须用 src.NamespaceID）。
	getSourceNS []string
	inserted    bool
	insertErr   error
	// prunes 记录 InsertPruneEvent 落下的剪枝事件（入口校验留痕）。
	prunes []*store.Event
}

func (f *fakeStore) InsertPruneEvent(ctx context.Context, e *store.Event) (bool, error) {
	f.prunes = append(f.prunes, e)
	return true, nil
}

func (f *fakeStore) DueCronSources(ctx context.Context, now int64, limit int) ([]*store.Source, error) {
	return f.due, nil
}
func (f *fakeStore) MarkSourceFired(ctx context.Context, id string, firedAt, nextFire int64) error {
	f.fired = append(f.fired, struct {
		ID            string
		FiredAt, Next int64
	}{id, firedAt, nextFire})
	return nil
}
func (f *fakeStore) GetSource(ctx context.Context, namespaceID, id string) (*store.Source, error) {
	f.getSourceNS = append(f.getSourceNS, namespaceID)
	if f.src != nil {
		return f.src, nil
	}
	return nil, store.ErrSourceNotFound
}
func (f *fakeStore) InsertEvent(ctx context.Context, e *store.Event) (bool, error) {
	if f.insertErr != nil {
		return false, f.insertErr
	}
	f.events = append(f.events, e)
	return f.inserted, nil
}

type fakeDisp struct {
	got []route.DispatchInput
	out *route.DispatchOutput
}

func (f *fakeDisp) Dispatch(ctx context.Context, in route.DispatchInput) (*route.DispatchOutput, error) {
	f.got = append(f.got, in)
	if f.out != nil {
		return f.out, nil
	}
	return &route.DispatchOutput{}, nil
}

// TestFireUsesEntryEdge 源触发只给「来源名 + 事件类型」，目标与参数模板都在入口边上。
//
// P2.1 删掉了 Targets/TargetParams 旁路：源触发与 POST /v1/events 现在是**同一条路**
// （edges.from_is_source=1 的入口边）。所以 DispatchInput 里只剩入口边的两个字段，
// 参数模板由 route.plan 用 Payload/VarValues 对边上的 mapping 求值。
func TestFireUsesEntryEdge(t *testing.T) {
	st := &fakeStore{inserted: true}
	disp := &fakeDisp{out: &route.DispatchOutput{Tasks: []*store.Task{{ID: "tsk_1"}}}}
	tr := &Trigger{Store: st, Dispatcher: disp}

	src := &store.Source{
		ID: "src_1", NamespaceID: "ns_1", Name: "每晚体检", Kind: store.SourceKindCron,
		Enabled: true, Targets: []store.SourceTarget{{CapabilityID: "cap_a"}, {CapabilityID: "cap_b"}},
	}
	res, err := tr.Fire(context.Background(), src, "nightly", "", json.RawMessage(`{"k":"v"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(disp.got) != 1 {
		t.Fatalf("应派发 1 次, got %d", len(disp.got))
	}
	in := disp.got[0]
	// 命名空间随源走：派发与事件都必须是源所在的命名空间（契约 §3）。
	// 命名空间是唯一的隔离维度（org 层已随迁移 14 删除），源带的 ns 必须原样传下去；
	// 少了它或换成别的维度（如默认命名空间）这里立即变红。
	if in.NamespaceID != src.NamespaceID {
		t.Errorf("DispatchInput.NamespaceID = %q, want %q（源触发必须带命名空间）", in.NamespaceID, src.NamespaceID)
	}
	if res.Event.NamespaceID != src.NamespaceID {
		t.Errorf("event.NamespaceID = %q, want %q", res.Event.NamespaceID, src.NamespaceID)
	}
	// 关键断言：源走**入口边**（来源名 + 事件类型），没有第二条取目标的路径。
	// FromCap 是能力边的左端（能力 id），入口边不该用它——否则「从哪来」有两份真相。
	if !in.FromIsSource {
		t.Error("源触发必须标记 FromIsSource（否则 route 会按能力 id 去查边）")
	}
	if in.EntryFromKey != src.Name {
		t.Errorf("EntryFromKey = %q, want %q（入口边的左端是来源名）", in.EntryFromKey, src.Name)
	}
	if in.EntryFromPort != "nightly" {
		t.Errorf("EntryFromPort = %q, want %q（入口边的端口是事件类型）", in.EntryFromPort, "nightly")
	}
	if in.FromCap != "" || in.Out != "" {
		t.Errorf("入口边不该再填 FromCap/Out（那是能力边的字段）: %q/%q", in.FromCap, in.Out)
	}
	if len(res.Dispatch.Tasks) != 1 {
		t.Errorf("应返回 1 个任务, got %d", len(res.Dispatch.Tasks))
	}
	if res.Event.Source != src.Name {
		t.Errorf("事件来源应是源的名字, got %q", res.Event.Source)
	}
	if res.Event.Type != "nightly" {
		t.Errorf("事件类型错了: %q", res.Event.Type)
	}
	// payload 作为**求值来源**原样透传（不变量 I6：不解释、不替换），
	// 但不再当成 params 直接塞给下游——入口边的参数来自边上的模板。
	if in.Payload["k"] != "v" {
		t.Errorf("payload 未透传: %v", in.Payload)
	}
	if in.Params != nil {
		t.Errorf("入口边的参数不该由源塞整份 payload（会盖住边上的模板）: %v", in.Params)
	}
}

func TestFireDisabledSource(t *testing.T) {
	st := &fakeStore{inserted: true}
	tr := &Trigger{Store: st, Dispatcher: &fakeDisp{}}
	src := &store.Source{ID: "s", NamespaceID: "ns_1", Name: "n", Enabled: false}
	if _, err := tr.Fire(context.Background(), src, "t", "", nil); err == nil {
		t.Error("停用的源不该能触发")
	}
}

func TestFireIdempotent(t *testing.T) {
	st := &fakeStore{inserted: false} // 模拟幂等命中
	disp := &fakeDisp{}
	tr := &Trigger{Store: st, Dispatcher: disp}
	src := &store.Source{ID: "s", NamespaceID: "ns_1", Name: "n", Enabled: true, Targets: []store.SourceTarget{{CapabilityID: "c"}}}
	res, err := tr.Fire(context.Background(), src, "t", "k1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Idempotent {
		t.Error("应标记幂等命中")
	}
	if len(disp.got) != 0 {
		t.Error("幂等命中不该重复派发")
	}
}

func TestSchedulerTickFiresDueCron(t *testing.T) {
	now := int64(1792925804)
	next := now // 已到点
	st := &fakeStore{
		inserted: true,
		// scheduler 先用 DueCronSources 拿列表，再 GetSource 拿全量
		due: []*store.Source{{
			ID: "src_1", NamespaceID: "ns_1", Name: "cron-1", Kind: store.SourceKindCron,
			Enabled: true, Config: json.RawMessage(`{"expr":"0 3 * * *"}`),
			NextFireAt: &next,
		}},
	}
	st.src = st.due[0]
	st.src.Targets = []store.SourceTarget{{CapabilityID: "cap_a"}}
	disp := &fakeDisp{out: &route.DispatchOutput{Tasks: []*store.Task{{ID: "t1"}}}}
	s := &Scheduler{Store: st, Dispatcher: disp, Now: func() int64 { return now }}
	s.Tick(context.Background())

	// 回查全量源时用的是行上自带的命名空间（不是默认命名空间兜底）
	if len(st.getSourceNS) != 1 || st.getSourceNS[0] != "ns_1" {
		t.Fatalf("GetSource 的命名空间 = %v, want [ns_1]（应传 src.NamespaceID）", st.getSourceNS)
	}
	// 派发与事件同样带上这个命名空间
	if len(disp.got) != 1 || disp.got[0].NamespaceID != "ns_1" {
		t.Fatalf("派发的 NamespaceID 不是 ns_1: %+v", disp.got)
	}
	if len(st.events) != 1 || st.events[0].NamespaceID != "ns_1" {
		t.Fatalf("事件的 NamespaceID 不是 ns_1: %+v", st.events)
	}

	if len(st.fired) != 1 {
		t.Fatalf("应标记 1 次触发, got %d", len(st.fired))
	}
	// next 必须被推进（否则这个源会每分钟重复触发）
	if st.fired[0].Next <= now {
		t.Errorf("nextFire 应被推进到未来, got %d (now=%d)", st.fired[0].Next, now)
	}
	if len(st.events) != 1 {
		t.Fatalf("应写 1 条事件, got %d", len(st.events))
	}
	// cron 的幂等键必须确定性（含计划时刻），否则重启会重复触发
	if st.events[0].IdemKey == nil || *st.events[0].IdemKey == "" {
		t.Error("cron 触发应带幂等键")
	}
	want := "cron:src_1:" + itoa(now)
	if got := *st.events[0].IdemKey; got != want {
		t.Errorf("幂等键 = %q, want %q", got, want)
	}
}

func TestSchedulerSkipsNotDue(t *testing.T) {
	st := &fakeStore{inserted: true, due: nil}
	disp := &fakeDisp{}
	s := &Scheduler{Store: st, Dispatcher: disp, Now: func() int64 { return 100 }}
	s.Tick(context.Background())
	if len(disp.got) != 0 {
		t.Error("没有到点的源时不该派发")
	}
}

// ---------- #114：payload 交给参数解析 + cron payload 打底 ----------

// TestFirePassesPayloadAndVars Fire 把展开后的 payload 与变量表一起递进派发（契约 §4 第 19 项）。
//
// 没有这两个字段，派发层就只能拿到「参数模板」，`{{x}}` / {var} 全部取不到
// → 每条源路径都被 param_missing 剪掉。所以这里把它们当成协议的一部分钉住。
func TestFirePassesPayloadAndVars(t *testing.T) {
	st := &fakeStore{inserted: true}
	disp := &fakeDisp{out: &route.DispatchOutput{}}
	tr := &Trigger{Store: st, Dispatcher: disp}

	src := &store.Source{
		ID: "src_1", Name: "hook", Kind: store.SourceKindWebhook,
		Enabled: true,
		Config: json.RawMessage(`{"vars":[` +
			`{"name":"prTitle","from":"pull_request.title"},` +
			`{"name":"team","value":"infra"}]}`),
		Targets: []store.SourceTarget{{
			CapabilityID: "cap_a",
			Params:       map[string]any{"title": "{{prTitle}}", "channel": "slack"},
		}},
	}
	if _, err := tr.Fire(context.Background(), src, "opened", "",
		json.RawMessage(`{"pull_request":{"title":"修个 bug"},"n":42}`)); err != nil {
		t.Fatal(err)
	}
	if len(disp.got) != 1 {
		t.Fatalf("应派发 1 次, got %d", len(disp.got))
	}
	in := disp.got[0]

	// ① Payload 是展开后的事件 payload（map 形式，供 {{路径}} 取值）
	if got := in.Payload["n"]; got != float64(42) {
		t.Errorf("in.Payload[n] = %#v, want 42", got)
	}
	pr, ok := in.Payload["pull_request"].(map[string]any)
	if !ok || pr["title"] != "修个 bug" {
		t.Errorf("in.Payload 少了嵌套对象: %#v", in.Payload["pull_request"])
	}

	// ② VarValues 是 config.vars 的求值结果：from 命中 + 常量都在表里
	if got := in.VarValues["prTitle"]; got != "修个 bug" {
		t.Errorf("in.VarValues[prTitle] = %#v, want 从 payload 取到的标题", got)
	}
	if got := in.VarValues["team"]; got != "infra" {
		t.Errorf("in.VarValues[team] = %#v, want 常量 infra", got)
	}

	// ③ P2.1 之后**没有**公共 params 回退：参数只从入口边的 mapping 求值。
	// 再传一份整份 payload 会变成第二条口径（模板说的与 payload 给的谁优先？）。
	if in.Params != nil {
		t.Errorf("in.Params = %#v, want nil（入口边的参数只来自边上的模板）", in.Params)
	}

	// ④ 写进 events 的 payload 与求值同源（同一份合并结果）
	if len(st.events) != 1 {
		t.Fatalf("应写 1 条事件, got %d", len(st.events))
	}
	if got := extractParams(st.events[0].Payload); got["n"] != float64(42) {
		t.Errorf("事件 payload 与 in.Payload 不同源: %#v", got)
	}
}

// TestFireConfigPayloadIsBaseConfigPayload 是打底：config.payload 打底、事件 payload 覆盖同名键（契约 §2.5）。
//
// 典型场景：cron/manual 源把「环境/仓库」写在 config.payload 里，每次触发
// 再带上本次的具体字段。打底数据必须能被事件覆盖，否则同一个源永远只能用
// 第一份值。
func TestFireConfigPayloadIsBaseConfigPayload(t *testing.T) {
	st := &fakeStore{inserted: true}
	disp := &fakeDisp{out: &route.DispatchOutput{}}
	tr := &Trigger{Store: st, Dispatcher: disp}

	src := &store.Source{
		ID: "src_1", Name: "man", Kind: store.SourceKindManual,
		Enabled: true,
		Config:  json.RawMessage(`{"payload":{"env":"prod","team":"old","region":"cn"}}`),
		Targets: []store.SourceTarget{{CapabilityID: "cap_a"}},
	}
	if _, err := tr.Fire(context.Background(), src, "t", "",
		json.RawMessage(`{"team":"new"}`)); err != nil {
		t.Fatal(err)
	}
	in := disp.got[0]

	if got := in.Payload["env"]; got != "prod" {
		t.Errorf("打底字段丢了: env = %#v", got)
	}
	if got := in.Payload["region"]; got != "cn" {
		t.Errorf("打底字段丢了: region = %#v", got)
	}
	if got := in.Payload["team"]; got != "new" {
		t.Errorf("事件 payload 应覆盖同名键: team = %#v, want new", got)
	}
	// events 里的字节也必须是合并后的（否则 pruned 的取值来源与事件体矛盾）
	evPayload := extractParams(st.events[0].Payload)
	if evPayload["env"] != "prod" || evPayload["team"] != "new" {
		t.Errorf("事件 payload 不是合并后的结果: %#v", evPayload)
	}
}

// TestSchedulerTickCronPayloadIncludesConfig cron 的 payload 带 config.payload 的用户字段，
// 且 source/kind/firedAt 仍在（契约 §4 第 20 项）。
func TestSchedulerTickCronPayloadIncludesConfig(t *testing.T) {
	now := int64(1792925804)
	next := now
	st := &fakeStore{
		inserted: true,
		due: []*store.Source{{
			ID: "src_1", Name: "cron-1", Kind: store.SourceKindCron,
			Enabled: true,
			// config.payload 是用户写的打底数据（config.vars 同理，两者都要保留）
			Config:     json.RawMessage(`{"expr":"0 3 * * *","payload":{"requestedBy":"ops"}}`),
			NextFireAt: &next,
		}},
	}
	st.src = st.due[0]
	st.src.Targets = []store.SourceTarget{{CapabilityID: "cap_a"}}
	disp := &fakeDisp{out: &route.DispatchOutput{}}
	s := &Scheduler{Store: st, Dispatcher: disp, Now: func() int64 { return now }}
	s.Tick(context.Background())

	if len(disp.got) != 1 {
		t.Fatalf("应派发 1 次, got %d", len(disp.got))
	}
	in := disp.got[0]
	if got := in.Payload["requestedBy"]; got != "ops" {
		t.Errorf("cron payload 少了 config.payload 的用户字段: %#v", in.Payload)
	}
	if got := in.Payload["source"]; got != "cron-1" {
		t.Errorf("系统字段 source 丢了: %#v", got)
	}
	if got := in.Payload["kind"]; got != store.SourceKindCron {
		t.Errorf("系统字段 kind 丢了: %#v", got)
	}
	if got := in.Payload["firedAt"]; got != float64(now) {
		t.Errorf("系统字段 firedAt 丢了: %#v, want %d", got, now)
	}
}

// TestSchedulerTickCronPayloadSystemFields 没有 config.payload 时就是那三个系统字段（旧行为不变）。
func TestSchedulerTickCronPayloadSystemFields(t *testing.T) {
	now := int64(1792925804)
	next := now
	st := &fakeStore{
		inserted: true,
		due: []*store.Source{{
			ID: "src_1", Name: "cron-1", Kind: store.SourceKindCron,
			Enabled:    true,
			Config:     json.RawMessage(`{"expr":"0 3 * * *"}`),
			NextFireAt: &next,
		}},
	}
	st.src = st.due[0]
	st.src.Targets = []store.SourceTarget{{CapabilityID: "cap_a"}}
	disp := &fakeDisp{out: &route.DispatchOutput{}}
	s := &Scheduler{Store: st, Dispatcher: disp, Now: func() int64 { return now }}
	s.Tick(context.Background())

	if len(disp.got) != 1 {
		t.Fatalf("应派发 1 次, got %d", len(disp.got))
	}
	in := disp.got[0]
	if got := in.Payload["firedAt"]; got != float64(now) {
		t.Errorf("firedAt = %#v, want %d", got, now)
	}
	if len(in.Payload) != 3 {
		t.Errorf("payload 应只有 {source,kind,firedAt}, got %#v", in.Payload)
	}
}

// TestSchedulerTickCronPayloadUserFieldsWin 钉住 A3 的冲突方向：cron 的
// config.payload 里写了与系统字段同名的键时，**用户字段赢**（设计 §3.3）。
//
// 方向搞反过一次（契约初稿写成"系统字段覆盖用户字段"），后果是用户显式写的
// source/kind/firedAt 被静默盖掉 —— 参数模板里 `{{source}}` 取到的与用户在
// 配置里看到的不一样，且没有任何可观测信号。所以这里逐键断言方向。
func TestSchedulerTickCronPayloadUserFieldsWin(t *testing.T) {
	now := int64(1792925804)
	next := now
	st := &fakeStore{
		inserted: true,
		due: []*store.Source{{
			ID: "src_1", Name: "cron-1", Kind: store.SourceKindCron,
			Enabled: true,
			// 四个键全部与系统字段同名（firedAt 是数字）+ 一个独占键
			Config: json.RawMessage(`{"expr":"0 3 * * *","payload":{` +
				`"source":"custom","kind":"custom-kind","firedAt":123,"team":"t"}}`),
			NextFireAt: &next,
		}},
	}
	st.src = st.due[0]
	st.src.Targets = []store.SourceTarget{{CapabilityID: "cap_a"}}
	disp := &fakeDisp{out: &route.DispatchOutput{}}
	s := &Scheduler{Store: st, Dispatcher: disp, Now: func() int64 { return now }}
	s.Tick(context.Background())

	if len(disp.got) != 1 {
		t.Fatalf("应派发 1 次, got %d", len(disp.got))
	}
	in := disp.got[0]
	// ① 冲突键：用户的值必须赢（不是系统默认值）
	if got := in.Payload["source"]; got != "custom" {
		t.Errorf("source = %#v, want custom（用户字段应覆盖系统默认值）", got)
	}
	if got := in.Payload["kind"]; got != "custom-kind" {
		t.Errorf("kind = %#v, want custom-kind（用户字段应覆盖系统默认值）", got)
	}
	if got := in.Payload["firedAt"]; got != float64(123) {
		t.Errorf("firedAt = %#v, want 123（用户字段应覆盖系统默认值）", got)
	}
	// ② 非冲突键照常在
	if got := in.Payload["team"]; got != "t" {
		t.Errorf("team = %#v, want t", got)
	}
	// ③ events 里落库的字节必须是同一份（否则事后复盘看到的 payload
	//    与派发时求值用的不是同一份）
	if len(st.events) != 1 {
		t.Fatalf("应写 1 条事件, got %d", len(st.events))
	}
	ev := extractParams(st.events[0].Payload)
	if ev["source"] != "custom" || ev["kind"] != "custom-kind" ||
		ev["firedAt"] != float64(123) || ev["team"] != "t" {
		t.Errorf("落库的事件 payload 与求值不同源: %#v", ev)
	}
}

// TestFireWebhookIgnoresConfigPayload 是 A13 的回归：webhook / github 的
// payload 来自外部投递体，config 里残留的 `payload` 键（历史行）不得被
// 复活成事件字段。
//
// 否则同一份投递在两版实现下会产出不同 payload：参数模板 `{{env}}` 突然取
// 得到值了、events 里多出投递方从没发过的字段，复盘时根本对不上账。
func TestFireWebhookIgnoresConfigPayload(t *testing.T) {
	for _, kind := range []string{store.SourceKindWebhook, store.SourceKindGitHub} {
		t.Run(kind, func(t *testing.T) {
			st := &fakeStore{inserted: true}
			disp := &fakeDisp{out: &route.DispatchOutput{}}
			tr := &Trigger{Store: st, Dispatcher: disp}

			src := &store.Source{
				ID: "src_1", Name: "hook", Kind: kind,
				Enabled: true,
				// 历史行残留：早期 ValidateSourceConfig 各 kind 都保存 payload
				Config: json.RawMessage(`{"token":"tok","event":"push",` +
					`"payload":{"env":"prod","team":"infra"}}`),
				Targets: []store.SourceTarget{{CapabilityID: "cap_a"}},
			}
			raw := json.RawMessage(`{"action":"opened"}`)
			if _, err := tr.Fire(context.Background(), src, "push", "", raw); err != nil {
				t.Fatal(err)
			}
			if len(disp.got) != 1 {
				t.Fatalf("应派发 1 次, got %d", len(disp.got))
			}
			in := disp.got[0]
			for _, k := range []string{"env", "team"} {
				if v, ok := in.Payload[k]; ok {
					t.Errorf("%s 源不得合并 config.payload 的键 %q（got %#v）", kind, k, v)
				}
			}
			if got := in.Payload["action"]; got != "opened" {
				t.Errorf("投递体字段丢了: action = %#v", got)
			}
			// events 里必须是投递体原样字节（没打底 → 不重新 marshal）
			if len(st.events) != 1 {
				t.Fatalf("应写 1 条事件, got %d", len(st.events))
			}
			if got := string(st.events[0].Payload); got != string(raw) {
				t.Errorf("事件体 = %s, want 原样投递字节 %s", got, raw)
			}
			if _, ok := extractParams(st.events[0].Payload)["env"]; ok {
				t.Errorf("落库的事件 payload 被残留 config.payload 污染: %s", st.events[0].Payload)
			}

			// 试运行与真触发必须同判定：也不合并
			if _, err := tr.DryRun(context.Background(), src, "push", raw); err != nil {
				t.Fatal(err)
			}
			if len(disp.got) != 2 {
				t.Fatalf("试运行应派发 1 次, got %d", len(disp.got)-1)
			}
			if v, ok := disp.got[1].Payload["env"]; ok {
				t.Errorf("试运行也不得合并 config.payload: env = %#v", v)
			}
		})
	}
}

// TestEventPayloadKindScope 直接钉住 eventPayload 的 kind 范围（A13）。
//
// cron / manual 打底；webhook / github / 未知 kind 一律不合并 —— 未知 kind
// 按"不合并"处理是保守选择：宁可少给字段（触发时 param_missing 可观测），
// 也不要把历史残留字段悄悄塞进 payload。
func TestEventPayloadKindScope(t *testing.T) {
	raw := json.RawMessage(`{"a":1}`)
	cfg := json.RawMessage(`{"payload":{"env":"prod"}}`)

	for _, kind := range []string{store.SourceKindCron, store.SourceKindManual} {
		src := &store.Source{Kind: kind, Config: cfg}
		m, ev := eventPayload(src, raw)
		if m["env"] != "prod" {
			t.Errorf("%s 源应合并 config.payload 打底: %#v", kind, m)
		}
		if m["a"] != float64(1) {
			t.Errorf("%s 源丢了事件 payload 字段: %#v", kind, m)
		}
		if got := extractParams(ev); got["env"] != "prod" {
			t.Errorf("%s 源的事件字节不含打底: %s", kind, ev)
		}
	}

	for _, kind := range []string{store.SourceKindWebhook, store.SourceKindGitHub, "", "future-kind"} {
		src := &store.Source{Kind: kind, Config: cfg}
		m, ev := eventPayload(src, raw)
		if _, ok := m["env"]; ok {
			t.Errorf("kind=%q 不该合并 config.payload: %#v", kind, m)
		}
		if m["a"] != float64(1) {
			t.Errorf("kind=%q 丢了事件 payload 字段: %#v", kind, m)
		}
		if string(ev) != string(raw) {
			t.Errorf("kind=%q 的事件字节应原样保留: %s", kind, ev)
		}
	}
}

// TestDryRunPassesPayloadAndVars 试运行与真实触发用同一套 payload/变量求值（契约 §2.5）。
//
// 两者不同源，就会出现"试运行说取得到、真触发 param_missing"这种无法自圆的差异。
func TestDryRunPassesPayloadAndVars(t *testing.T) {
	st := &fakeStore{inserted: true}
	disp := &fakeDisp{out: &route.DispatchOutput{}}
	tr := &Trigger{Store: st, Dispatcher: disp}

	src := &store.Source{
		ID: "src_1", Name: "man", Kind: store.SourceKindManual,
		Config: json.RawMessage(`{"vars":[{"name":"who","from":"user.login"}],` +
			`"payload":{"env":"prod"}}`),
		Targets: []store.SourceTarget{{CapabilityID: "cap_a"}},
	}
	if _, err := tr.DryRun(context.Background(), src, "t", json.RawMessage(`{"user":{"login":"ada"}}`)); err != nil {
		t.Fatal(err)
	}
	in := disp.got[0]
	if !in.DryRun {
		t.Error("DryRun 标志没传下去")
	}
	if got := in.Payload["env"]; got != "prod" {
		t.Errorf("试运行没拿到 config.payload 打底: %#v", in.Payload)
	}
	if got := in.VarValues["who"]; got != "ada" {
		t.Errorf("试运行没求值变量: %#v", in.VarValues)
	}
	// 试运行不落库（不能污染 events）
	if len(st.events) != 0 {
		t.Errorf("试运行写了 %d 条事件, want 0", len(st.events))
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := make([]byte, 0, 20)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		return "-" + string(buf)
	}
	return string(buf)
}

// 保证 fakeStore 满足接口（编译期）
var _ Store = (*fakeStore)(nil)
var _ Dispatcher = (*fakeDisp)(nil)

// 让 time 被用到（NextCronTime 相关断言在上面用不到 time 时避免 unused import）
var _ = time.Now

// TestFireDoesNotFlattenParams 每个出口的模板由**各自的入口边**携带，不在派发输入里合并。
//
// 这是「源可以带参数」的关键语义：曾经源只有一份公共 params，同时喂两个
// params_schema 不同的能力时必然对不上至少一边，触发时被 param_schema 静默剪枝。
// P2.1 之后这份"按出口分别的参数"住在 edges.mapping 上（一个出口一条边），
// 所以 Trigger 这一层**不该**再把它们合成一份 map 传下去——它只给来源名 + 事件类型。
//
// 按出口分别求值的覆盖点：
//   - store：CreateSource 把每个 target 的 params 写成独立的入口边 mapping
//     （source_test.go 的 TestSourceTargetsRoundTripFromEdges）；
//   - route：plan 逐条边求值、逐条剪枝（dispatch_test.go 的扇出用例）。
func TestFireDoesNotFlattenParams(t *testing.T) {
	st := &fakeStore{inserted: true}
	disp := &fakeDisp{out: &route.DispatchOutput{}}
	tr := &Trigger{Store: st, Dispatcher: disp}

	src := &store.Source{
		ID: "src_1", NamespaceID: "ns_1", Name: "多出口", Kind: store.SourceKindManual,
		Enabled: true,
		Targets: []store.SourceTarget{
			{CapabilityID: "cap_analyze", Params: map[string]any{"changedLines": float64(350)}},
			{CapabilityID: "cap_deploy", Params: map[string]any{"env": "staging"}},
			{CapabilityID: "cap_noargs"},
		},
	}
	if _, err := tr.Fire(context.Background(), src, "t", "", nil); err != nil {
		t.Fatal(err)
	}
	if len(disp.got) != 1 {
		t.Fatalf("应派发 1 次, got %d", len(disp.got))
	}
	in := disp.got[0]

	// 派发输入里只该有入口边的标识：合并成一份 map 就又会串味/串 schema。
	if in.EntryFromKey != src.Name || in.EntryFromPort != "t" {
		t.Fatalf("入口边标识 = %q/%q, want %q/%q", in.EntryFromKey, in.EntryFromPort, src.Name, "t")
	}
	if in.Params != nil {
		t.Fatalf("in.Params = %v, want nil（按出口的参数在各自入口边的 mapping 上）", in.Params)
	}
}

// ---------- 入口校验：resultSchema（设计 §4.2 / §7 P0.2）----------

// prSchemaSrc 是一个「声明了 payload 契约」的 github 源。
//
// 契约故意写得浅（顶层 required + 一个嵌套 string）：要覆盖的是
// 「合规则派 / 不合规则留痕」这两条路，不是 schema 引擎本身
// （引擎由 internal/route 的 params_test.go 覆盖）。
func prSchemaSrc() *store.Source {
	return &store.Source{
		ID: "src_schema", NamespaceID: "ns_1", Name: "pr-hook", Kind: store.SourceKindGitHub,
		Enabled: true,
		Config: json.RawMessage(`{"token":"tok","event":"pull_request","resultSchema":` +
			`{"type":"object","required":["pull_request"],` +
			`"properties":{"pull_request":{"type":"object",` +
			`"properties":{"title":{"type":"string"}}}}}}`),
		Targets: []store.SourceTarget{{CapabilityID: "cap_a"}},
	}
}

// TestFireDispatchesWhenPayloadSatisfiesResultSchema 是验收 ①：合法 payload 照常派发。
//
// 合规则路径必须完全不产生 event_schema 剪枝也不写剪枝事件——否则「配了契约
// 但 payload 是对的」会看起来像被拦了，用户一定会先把契约删掉。
func TestFireDispatchesWhenPayloadSatisfiesResultSchema(t *testing.T) {
	st := &fakeStore{inserted: true}
	disp := &fakeDisp{out: &route.DispatchOutput{Tasks: []*store.Task{{ID: "tsk_1"}}}}
	tr := &Trigger{Store: st, Dispatcher: disp}

	res, err := tr.Fire(context.Background(), prSchemaSrc(), "pull_request.opened", "",
		json.RawMessage(`{"pull_request":{"title":"修个 bug"},"action":"opened"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(disp.got) != 1 {
		t.Fatalf("合规 payload 应派发 1 次, got %d", len(disp.got))
	}
	if len(res.Dispatch.Tasks) != 1 || len(res.Dispatch.Pruned) != 0 {
		t.Fatalf("合规时不该有剪枝: tasks=%d pruned=%v",
			len(res.Dispatch.Tasks), res.Dispatch.Pruned)
	}
	if len(st.prunes) != 0 {
		t.Fatalf("合规时不该写剪枝事件, got %d", len(st.prunes))
	}
}

// TestFirePrunesEventSchemaViolation 是验收 ②：不合规 → 零任务 + prune(event_schema)
// + **事件仍落库**。
//
// 三条断言缺一不可：
//   - 不派发（disp 没被调用）：不合规的事件不能进链路；
//   - 事件在库里（st.events 有 1 条，payload 原样）：外部系统不重试，
//     400 丢弃等于把数据丢了（设计 §4.2 的第一句话）；
//   - 剪枝留痕（st.prunes 1 条、source=route、type=pruned.event_schema、
//     与事件同 trace）：否则「收到了但没派」这件事在事后无法查清。
func TestFirePrunesEventSchemaViolation(t *testing.T) {
	cases := []struct {
		name       string
		payload    string
		wantDetail string // detail 里必须出现（哪个字段 + 期望什么/实得什么）
	}{
		{
			name:       "缺必填字段",
			payload:    `{"action":"opened"}`,
			wantDetail: `missing required field "pull_request"`,
		},
		{
			name:       "字段类型不符",
			payload:    `{"pull_request":{"title":42}}`,
			wantDetail: "payload.pull_request.title: want type string, got number",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &fakeStore{inserted: true}
			disp := &fakeDisp{out: &route.DispatchOutput{Tasks: []*store.Task{{ID: "tsk_1"}}}}
			tr := &Trigger{Store: st, Dispatcher: disp, Now: func() int64 { return 1700000000 }}

			res, err := tr.Fire(context.Background(), prSchemaSrc(), "pull_request.opened", "",
				json.RawMessage(c.payload))
			if err != nil {
				t.Fatalf("入口不合规不是错误（事件照样入库）: %v", err)
			}

			// ① 零任务：一条都不派
			if len(disp.got) != 0 {
				t.Fatalf("不合规的事件不该派发, dispatch 被调了 %d 次", len(disp.got))
			}
			if res.Dispatch == nil {
				t.Fatal("res.Dispatch 不能是 nil（前端要读它展示剪枝）")
			}
			if len(res.Dispatch.Tasks) != 0 {
				t.Fatalf("不合规时不该有任务: %v", res.Dispatch.Tasks)
			}
			if res.Dispatch.Tasks == nil {
				t.Error("空任务列表必须是 [] 而不是 null（前端会把它当成未连接）")
			}

			// ② prune(event_schema)，detail 指明字段与期望
			if len(res.Dispatch.Pruned) != 1 {
				t.Fatalf("应恰好 1 条剪枝, got %v", res.Dispatch.Pruned)
			}
			p := res.Dispatch.Pruned[0]
			if p.Reason != route.ReasonEventSchema {
				t.Fatalf("reason = %q, want %q", p.Reason, route.ReasonEventSchema)
			}
			if !strings.Contains(p.Detail, c.wantDetail) {
				t.Fatalf("detail 应含 %q（哪个字段/期望什么/实得什么）, got %q", c.wantDetail, p.Detail)
			}
			if p.Port != "pull_request.opened" {
				t.Errorf("port 应是事件类型（trace 里对得上 \"哪次触发\"）: %q", p.Port)
			}

			// ③ 事件仍落库，payload 原样
			if len(st.events) != 1 {
				t.Fatalf("事件必须照常落库（丢了就没了）, got %d", len(st.events))
			}
			ev := st.events[0]
			if string(ev.Payload) != c.payload {
				t.Errorf("事件 payload 被改动: %s, want %s", ev.Payload, c.payload)
			}

			// ④ 剪枝留痕：与派发期同一张表 / 同一套 source、type 约定
			if len(st.prunes) != 1 {
				t.Fatalf("应写 1 条剪枝事件, got %d", len(st.prunes))
			}
			pe := st.prunes[0]
			if pe.Source != route.PruneEventSource {
				t.Errorf("剪枝事件 source = %q, want %q", pe.Source, route.PruneEventSource)
			}
			if pe.Type != route.PruneEventType(route.ReasonEventSchema) {
				t.Errorf("剪枝事件 type = %q, want pruned.event_schema", pe.Type)
			}
			if pe.TraceID != ev.TraceID {
				t.Errorf("剪枝事件必须与事件同 trace（否则 trace 视图里看不到）: %q vs %q",
					pe.TraceID, ev.TraceID)
			}
			if pe.NamespaceID != ev.NamespaceID {
				t.Errorf("剪枝事件命名空间 = %q, want %q", pe.NamespaceID, ev.NamespaceID)
			}
			if pe.CreatedAt != 1700000000 {
				t.Errorf("剪枝事件时间应由 Trigger.Now 给: %d", pe.CreatedAt)
			}
			var body route.PruneEventPayload
			if err := json.Unmarshal(pe.Payload, &body); err != nil {
				t.Fatalf("剪枝事件 payload 不是合法 JSON: %v (%s)", err, pe.Payload)
			}
			if body.FromCapability != "pr-hook" {
				t.Errorf("fromCapability 应是源名: %q", body.FromCapability)
			}
			if body.Detail == "" {
				t.Error("剪枝事件的 detail 不能为空（它是唯一的证据）")
			}
		})
	}
}

// TestFireWithoutResultSchemaUnchanged 是验收 ④：没声明契约时行为与今天完全一致。
//
// 老源（库里没有 resultSchema 键）不该因为这次改动多出任何校验：任意 payload
// 都派发、不剪枝、不写剪枝事件。这条一旦变红，等于给所有已有源加了静默的闸门。
func TestFireWithoutResultSchemaUnchanged(t *testing.T) {
	st := &fakeStore{inserted: true}
	disp := &fakeDisp{out: &route.DispatchOutput{Tasks: []*store.Task{{ID: "tsk_1"}}}}
	tr := &Trigger{Store: st, Dispatcher: disp}

	// config 里完全没有 resultSchema（老数据的长相）
	src := &store.Source{
		ID: "src_old", NamespaceID: "ns_1", Name: "老源", Kind: store.SourceKindManual,
		Enabled: true, Config: json.RawMessage(`{"payload":{"env":"prod"}}`),
		Targets: []store.SourceTarget{{CapabilityID: "cap_a"}},
	}
	res, err := tr.Fire(context.Background(), src, "t", "", json.RawMessage(`{"whatever":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(disp.got) != 1 {
		t.Fatalf("未声明契约时必须照常派发, got %d", len(disp.got))
	}
	if len(res.Dispatch.Pruned) != 0 {
		t.Fatalf("未声明契约时不该有剪枝: %v", res.Dispatch.Pruned)
	}
	if len(st.prunes) != 0 {
		t.Fatalf("未声明契约时不该写剪枝事件, got %d", len(st.prunes))
	}

	// 脏数据（resultSchema 不是对象）：读侧容忍成"没声明"，不能因一条坏行
	// 让这个源的所有事件都派不出去。
	for _, bad := range []string{`{"resultSchema":[1,2]}`, `{"resultSchema":"x"}`, `{"resultSchema":null}`} {
		st := &fakeStore{inserted: true}
		disp := &fakeDisp{out: &route.DispatchOutput{Tasks: []*store.Task{{ID: "t"}}}}
		tr := &Trigger{Store: st, Dispatcher: disp}
		dirty := *src
		dirty.Config = json.RawMessage(bad)
		if _, err := tr.Fire(context.Background(), &dirty, "t", "", json.RawMessage(`{"a":1}`)); err != nil {
			t.Fatalf("%s: %v", bad, err)
		}
		if len(disp.got) != 1 {
			t.Errorf("%s: 脏 resultSchema 应退化成不校验（照常派发）, got %d", bad, len(disp.got))
		}
	}
}

// TestDryRunAppliesResultSchema 试运行必须给出与真触发**同一条**判定。
//
// 「试运行说能跑、真点却剪枝」是最伤信任的差异（仓库里所有 dry-run 注释都在说
// 这件事）。所以试运行也要跑入口校验，而且——它不落库：不写事件、不写剪枝。
func TestDryRunAppliesResultSchema(t *testing.T) {
	// 不合规：只回判定，不派发、不落库
	st := &fakeStore{inserted: true}
	disp := &fakeDisp{}
	tr := &Trigger{Store: st, Dispatcher: disp}
	out, err := tr.DryRun(context.Background(), prSchemaSrc(), "pull_request.opened",
		json.RawMessage(`{"action":"opened"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(disp.got) != 0 {
		t.Fatalf("不合规的 payload 试运行也不该派发, got %d", len(disp.got))
	}
	if len(out.Tasks) != 0 {
		t.Fatalf("不合规时 dry-run 不该给任务: %v", out.Tasks)
	}
	if len(out.Pruned) != 1 || out.Pruned[0].Reason != route.ReasonEventSchema {
		t.Fatalf("dry-run 应把 event_schema 剪枝回给用户: %v", out.Pruned)
	}
	if len(st.events) != 0 || len(st.prunes) != 0 {
		t.Errorf("试运行不能落库: events=%d prunes=%d", len(st.events), len(st.prunes))
	}

	// 合规：照常进 Dispatch（DryRun 标志照旧传下去）
	st2 := &fakeStore{inserted: true}
	disp2 := &fakeDisp{out: &route.DispatchOutput{}}
	tr2 := &Trigger{Store: st2, Dispatcher: disp2}
	if _, err := tr2.DryRun(context.Background(), prSchemaSrc(), "pull_request.opened",
		json.RawMessage(`{"pull_request":{"title":"ok"}}`)); err != nil {
		t.Fatal(err)
	}
	if len(disp2.got) != 1 || !disp2.got[0].DryRun {
		t.Fatalf("合规 payload 的试运行应带 DryRun 进派发: %+v", disp2.got)
	}
}
