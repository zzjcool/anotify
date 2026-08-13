package route

import (
	"testing"

	"github.com/zzjcool/anotify/internal/broker"
	"github.com/zzjcool/anotify/internal/store"
)

// benchDev 构造一台带给定标签的测试设备。
func benchDev(enabled bool, eventScope string, tags []string) *store.Device {
	return &store.Device{
		ID:          "dev_bench",
		Enabled:     enabled,
		EventScope:  eventScope,
		Tags:        tags,
	}
}

// benchmarkDevices 构造 n 台设备，其中一半命中 msgTags，一半不命中。
func benchmarkDevices(n int, msgTags []string) []*store.Device {
	out := make([]*store.Device, 0, n)
	for i := 0; i < n; i++ {
		devTags := []string{"miss"}
		if i%2 == 0 {
			devTags = msgTags
		}
		out = append(out, benchDev(true, "all", devTags))
	}
	return out
}

// BenchmarkFilterDevices 度量「按路由规则从候选设备中筛选命中设备」的吞吐。
// 这是每条消息 × 每用户投递路径上的核心热点（广播给多设备时被反复调用）。
func BenchmarkFilterDevices(b *testing.B) {
	msg := &broker.Message{
		ID:         "ntf_bench",
		UserID:     "usr_bench",
		AgentState:  broker.AgentStateDone,
		DeviceTags:  []string{"ops", "build"},
	}
	devices := benchmarkDevices(100, []string{"ops", "build"})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got := FilterDevices(devices, msg)
		if len(got) != 50 {
			b.Fatalf("FilterDevices 命中 %d 台，期望 50", len(got))
		}
	}
}

// BenchmarkFilterDevicesBroadcast 度量广播消息（无 deviceTags）命中大量设备的场景。
// 广播走最热路径：所有 enabled+status 匹配设备都命中。
func BenchmarkFilterDevicesBroadcast(b *testing.B) {
	msg := &broker.Message{ID: "ntf_bench", UserID: "usr", AgentState: broker.AgentStateDone}
	devices := make([]*store.Device, 100)
	for i := range devices {
		devices[i] = benchDev(true, "all", nil)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got := FilterDevices(devices, msg)
		if len(got) != 100 {
			b.Fatalf("广播命中 %d 台，期望 100", len(got))
		}
	}
}

// BenchmarkTagMatch 度量单设备 × 单消息的标签判定成本（过滤循环内最细粒度操作）。
func BenchmarkTagMatch(b *testing.B) {
	deviceTags := []string{"手机", "工作", "build", "ci", "prod"}
	msgTags := []string{"build", "release"}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !TagMatch(deviceTags, msgTags) {
			b.Fatal("TagMatch 应命中")
		}
	}
}

// BenchmarkShouldDeliver 度量综合判定（enabled + status + tag）的单设备成本。
func BenchmarkShouldDeliver(b *testing.B) {
	dev := benchDev(true, "final", []string{"ops"})
	msg := &broker.Message{ID: "ntf", AgentState: broker.AgentStateError, DeviceTags: []string{"ops"}}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !ShouldDeliver(dev, msg) {
			b.Fatal("ShouldDeliver 应命中")
		}
	}
}

// BenchmarkScopeMatch 度量状态过滤分支成本。
func BenchmarkScopeMatch(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !ScopeMatch("final", broker.AgentStateError) {
			b.Fatal("ScopeMatch 应命中")
		}
	}
}
