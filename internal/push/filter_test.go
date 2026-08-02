package push

import (
	"testing"

	"github.com/anotify/anotify/internal/broker"
	"github.com/anotify/anotify/internal/route"
	"github.com/anotify/anotify/internal/store"
)

// dev 构造一台测试设备。
func dev(enabled bool, statusFilter string, tags ...string) *store.Device {
	return &store.Device{
		ID:           "dev_test",
		Enabled:      enabled,
		StatusFilter: statusFilter,
		Tags:         tags,
	}
}

func msg(status string, tags ...string) *broker.Message {
	return &broker.Message{ID: "ntf_test", Status: status, DeviceTags: tags}
}

// TestStatusMatch 覆盖 statusMatch 全部分支。
func TestStatusMatch(t *testing.T) {
	cases := []struct {
		name      string
		filter    string
		msgStatus string
		want      bool
	}{
		{"all 放行 success", "all", broker.StatusSuccess, true},
		{"all 放行 error", "all", broker.StatusError, true},
		{"all 放行 interrupted", "all", broker.StatusInterrupted, true},
		{"all 放行 info", "all", broker.StatusInfo, true},
		{"all 放行 warning", "all", broker.StatusWarning, true},
		{"空 filter 视为 all", "", broker.StatusSuccess, true},

		{"error 过滤放行 error", "error", broker.StatusError, true},
		{"error 过滤拒绝 success", "error", broker.StatusSuccess, false},
		{"error 过滤拒绝 interrupted", "error", broker.StatusInterrupted, false},
		{"error 过滤拒绝 info", "error", broker.StatusInfo, false},
		{"error 过滤拒绝 warning", "error", broker.StatusWarning, false},

		{"success 过滤放行 success", "success", broker.StatusSuccess, true},
		{"success 过滤拒绝 error", "success", broker.StatusError, false},
		{"success 过滤拒绝 interrupted", "success", broker.StatusInterrupted, false},
		{"success 过滤拒绝 info", "success", broker.StatusInfo, false},

		{"未知 filter 拒绝一切", "bogus", broker.StatusSuccess, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := route.StatusMatch(c.filter, c.msgStatus); got != c.want {
				t.Fatalf("StatusMatch(%q,%q)=%v, want %v", c.filter, c.msgStatus, got, c.want)
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

// TestShouldDeliver 覆盖三元判定：enabled AND statusMatch AND tagMatch。
func TestShouldDeliver(t *testing.T) {
	cases := []struct {
		name string
		dev  *store.Device
		msg  *broker.Message
		want bool
	}{
		{"禁用设备一律不投递", dev(false, "all", "手机"), msg("success"), false},
		{"广播消息+enabled+all", dev(true, "all"), msg("success"), true},
		{"广播消息+error过滤+success消息", dev(true, "error"), msg("success"), false},
		{"广播消息+error过滤+error消息", dev(true, "error"), msg("error"), true},
		{"定向消息命中设备tag", dev(true, "all", "手机"), msg("success", "手机"), true},
		{"定向消息未命中设备tag", dev(true, "all", "手机"), msg("success", "工作"), false},
		{"定向消息+catch-all设备", dev(true, "all"), msg("success", "工作"), true},
		{"定向命中但status过滤拒绝", dev(true, "error", "手机"), msg("success", "手机"), false},
		{"status与tag都满足", dev(true, "error", "手机"), msg("error", "手机"), true},
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
		{ID: "a", Enabled: true, StatusFilter: "all", Tags: []string{"手机"}},
		{ID: "b", Enabled: true, StatusFilter: "error", Tags: []string{"工作"}},
		{ID: "c", Enabled: true, StatusFilter: "all", Tags: nil}, // catch-all
		{ID: "d", Enabled: false, StatusFilter: "all", Tags: nil},
	}

	// 广播 success：a✓ b✗(status) c✓ d✗(disabled)
	got := route.FilterDevices(devices, msg("success"))
	wantIDs := map[string]bool{"a": true, "c": true}
	assertIDs(t, got, wantIDs)

	// 定向 error 到 工作：b✓(tag+status) c✓(catch-all) a✗(tag不命中)
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
