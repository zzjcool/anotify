package source

import (
	"context"
	"encoding/json"
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
	inserted  bool
	insertErr error
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
func (f *fakeStore) GetSource(ctx context.Context, orgID, id string) (*store.Source, error) {
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

func TestFireUsesTargetsNotEdges(t *testing.T) {
	st := &fakeStore{inserted: true}
	disp := &fakeDisp{out: &route.DispatchOutput{Tasks: []*store.Task{{ID: "tsk_1"}}}}
	tr := &Trigger{Store: st, Dispatcher: disp}

	src := &store.Source{
		ID: "src_1", OrgID: "org_1", Name: "每晚体检", Kind: store.SourceKindCron,
		Enabled: true, Targets: []string{"cap_a", "cap_b"},
	}
	res, err := tr.Fire(context.Background(), src, "nightly", "", json.RawMessage(`{"k":"v"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(disp.got) != 1 {
		t.Fatalf("应派发 1 次, got %d", len(disp.got))
	}
	in := disp.got[0]
	// 关键断言：源走 Targets 直连，不查边（FromCap 为空）
	if len(in.Targets) != 2 || in.Targets[0] != "cap_a" {
		t.Errorf("Targets 未透传: %v", in.Targets)
	}
	if in.FromCap != "" {
		t.Errorf("源不应设置 FromCap（那是查边用的）: %q", in.FromCap)
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
	// payload 原样进 params（不变量 I6：不解释、不替换）
	if in.Params["k"] != "v" {
		t.Errorf("params 未透传: %v", in.Params)
	}
}

func TestFireDisabledSource(t *testing.T) {
	st := &fakeStore{inserted: true}
	tr := &Trigger{Store: st, Dispatcher: &fakeDisp{}}
	src := &store.Source{ID: "s", OrgID: "o", Name: "n", Enabled: false}
	if _, err := tr.Fire(context.Background(), src, "t", "", nil); err == nil {
		t.Error("停用的源不该能触发")
	}
}

func TestFireIdempotent(t *testing.T) {
	st := &fakeStore{inserted: false} // 模拟幂等命中
	disp := &fakeDisp{}
	tr := &Trigger{Store: st, Dispatcher: disp}
	src := &store.Source{ID: "s", OrgID: "o", Name: "n", Enabled: true, Targets: []string{"c"}}
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
			ID: "src_1", OrgID: "org_1", Name: "cron-1", Kind: store.SourceKindCron,
			Enabled: true, Config: json.RawMessage(`{"expr":"0 3 * * *"}`),
			NextFireAt: &next,
		}},
	}
	st.src = st.due[0]
	st.src.Targets = []string{"cap_a"}
	disp := &fakeDisp{out: &route.DispatchOutput{Tasks: []*store.Task{{ID: "t1"}}}}
	s := &Scheduler{Store: st, Dispatcher: disp, Now: func() int64 { return now }}
	s.Tick(context.Background())

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
