package push

import (
	"testing"

	"github.com/zzjcool/anotify/internal/broker"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
)

// dev 构造一台测试设备。
func dev(enabled bool, eventScope string, tags ...string) *store.Device {
	return &store.Device{
		ID:          "dev_test",
		Enabled:     enabled,
		EventScope:  eventScope,
		Tags:        tags,
	}
}

func msg(agentState string, tags ...string) *broker.Message {
	return &broker.Message{ID: "ntf_test", AgentState: agentState, DeviceTags: tags}
}

// TestScopeMatch 覆盖 scopeMatch 全部分支。
func TestScopeMatch(t *testing.T) {
	cases := []struct {
		name       string
		filter     string
		agentState string
		want       bool
	}{
		// all 档：全放行
		{"all 放行 done", "all", broker.AgentStateDone, true},
		{"all 放行 error", "all", broker.AgentStateError, true},
		{"all 放行 interrupted", "all", broker.AgentStateInterrupted, true},
		{"all 放行 working", "all", broker.AgentStateWorking, true},
		{"all 放行 blocked", "all", broker.AgentStateBlocked, true},
		{"空 filter 视为 all", "", broker.AgentStateDone, true},

		// final 档：仅终态放行
		{"final 放行 done（终态）", "final", broker.AgentStateDone, true},
		{"final 放行 error（终态）", "final", broker.AgentStateError, true},
		{"final 放行 interrupted（终态）", "final", broker.AgentStateInterrupted, true},
		{"final 拒绝 working（非终态）", "final", broker.AgentStateWorking, false},
		{"final 拒绝 blocked（非终态）", "final", broker.AgentStateBlocked, false},

		// 未知 filter 拒绝一切
		{"未知 filter 拒绝一切", "bogus", broker.AgentStateDone, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := route.ScopeMatch(c.filter, c.agentState); got != c.want {
				t.Fatalf("ScopeMatch(%q,%q)=%v, want %v", c.filter, c.agentState, got, c.want)
			}
		})
	}
}

// TestTagMatch 覆盖 tagMatch 的核心规则。
func TestTagMatch(t *testing.T) {
	cases := []struct {
		name       string
		deviceTags []string
		msgTags    []string
		want       bool
	}{
		{"消息无tag→广播匹配所有设备", []string{"手机"}, nil, true},
		{"消息无tag→匹配无tag设备", nil, nil, true},
		{"设备无tag→catch-all接收带tag消息", nil, []string{"手机"}, true},
		{"双方有交集（ANY）", []string{"手机", "工作"}, []string{"工作"}, true},
		{"双方多对多任一命中", []string{"手机", "工作"}, []string{"生活", "工作"}, true},
		{"双方无交集→拒绝", []string{"手机"}, []string{"工作"}, false},
		{"大小写不敏感命中", []string{"Phone"}, []string{"phone"}, true},
		{"设备无tag且消息无tag", []string{}, []string{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := route.TagMatch(c.deviceTags, c.msgTags); got != c.want {
				t.Fatalf("TagMatch(%v,%v)=%v, want %v", c.deviceTags, c.msgTags, got, c.want)
			}
		})
	}
}

// TestShouldDeliver 覆盖三元判定：enabled AND scopeMatch AND tagMatch。
func TestShouldDeliver(t *testing.T) {
	cases := []struct {
		name string
		dev  *store.Device
		msg  *broker.Message
		want bool
	}{
		{"禁用设备一律不投递", dev(false, "all", "手机"), msg("done"), false},
		{"广播消息+enabled+all", dev(true, "all"), msg("done"), true},
		{"广播终态+final设备→放行", dev(true, "final"), msg("done"), true},
		{"广播非终态+final设备→拒绝", dev(true, "final"), msg("working"), false},
		{"定向消息命中设备tag", dev(true, "all", "手机"), msg("done", "手机"), true},
		{"定向消息未命中设备tag", dev(true, "all", "手机"), msg("done", "工作"), false},
		{"定向消息+catch-all设备", dev(true, "all"), msg("done", "工作"), true},
		{"定向命中但scope过滤拒绝（非终态+final）", dev(true, "final", "手机"), msg("working", "手机"), false},
		{"scope与tag都满足（终态+final+tag命中）", dev(true, "final", "手机"), msg("done", "手机"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := route.ShouldDeliver(c.dev, c.msg); got != c.want {
				t.Fatalf("ShouldDeliver()=%v, want %v", got, c.want)
			}
		})
	}
}

// TestFilterDevices 验证从候选集中筛出命中设备。
func TestFilterDevices(t *testing.T) {
	devices := []*store.Device{
		{ID: "a", Enabled: true, EventScope: "all", Tags: []string{"手机"}},
		{ID: "b", Enabled: true, EventScope: "final", Tags: []string{"工作"}},
		{ID: "c", Enabled: true, EventScope: "all", Tags: nil}, // catch-all
		{ID: "d", Enabled: false, EventScope: "all", Tags: nil},
	}

	// 广播 done（终态）：a✓ b✓(终态+final) c✓ d✗(disabled)
	got := route.FilterDevices(devices, msg("done"))
	wantIDs := map[string]bool{"a": true, "b": true, "c": true}
	assertIDs(t, got, wantIDs)

	// 广播 working（非终态）：a✓ b✗(final拒绝非终态) c✓ d✗(disabled)
	got = route.FilterDevices(devices, msg("working"))
	wantIDs = map[string]bool{"a": true, "c": true}
	assertIDs(t, got, wantIDs)

	// 定向 error 到 工作：b✓(tag+终态+final) c✓(catch-all) a✗(tag不命中)
	got = route.FilterDevices(devices, msg("error", "工作"))
	wantIDs = map[string]bool{"b": true, "c": true}
	assertIDs(t, got, wantIDs)
}

func assertIDs(t *testing.T, got []*store.Device, want map[string]bool) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("FilterDevices 命中 %d 台, want %d (%v)", len(got), len(want), ids(got))
	}
	for _, d := range got {
		if !want[d.ID] {
			t.Fatalf("FilterDevices 意外命中 %s", d.ID)
		}
	}
}

func ids(devs []*store.Device) []string {
	var out []string
	for _, d := range devs {
		out = append(out, d.ID)
	}
	return out
}
