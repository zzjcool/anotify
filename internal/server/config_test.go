package server

import (
	"testing"
	"time"
)

// TestEventRetentionDaysEnvParsing 覆盖 ANOTIFY_EVENT_RETENTION_DAYS 的五种输入（契约 §1.3.1 / A1）。
//
// 语义：未设 / 空 → 30；"0" → 0（永久保留）；负数 → 0；非法 → 30；正常 → 原值。
//
// 只断言新增的两个字段：FromEnv 会读一堆别的 env（VAPID / 限流 / 缓存 TTL），
// 那些的默认值不是本任务的验收面，混进来只会让这条测试在别人改默认值时无辜变红。
func TestEventRetentionDaysEnvParsing(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"未设置", "", defaultEventRetentionDays},
		{"显式 0 = 永久保留", "0", 0},
		{"负数归一到永久保留", "-7", 0},
		{"非法值回落默认", "abc", defaultEventRetentionDays},
		{"正常值原样使用", "90", 90},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 空串等价于「未设置」：os.Getenv 对「未设」与「设为空」返回的都是 ""，
			// 用 t.Setenv 而不是 os.Unsetenv 才能让测试自己不污染别的用例（Cleanup 复位）。
			t.Setenv("ANOTIFY_EVENT_RETENTION_DAYS", c.raw)
			// 另一个旋钮显式钉住，否则外部环境里有 ANOTIFY_PENDING_MAX_AGE_SEC 时会串味。
			t.Setenv("ANOTIFY_PENDING_MAX_AGE_SEC", "")
			if got := FromEnv().EventRetentionDays; got != c.want {
				t.Errorf("ANOTIFY_EVENT_RETENTION_DAYS=%q → %d, want %d", c.raw, got, c.want)
			}
		})
	}
}

// TestPendingMaxAgeSecEnvParsing 覆盖 ANOTIFY_PENDING_MAX_AGE_SEC 的五种输入（契约 §1.3.1 / B1）。
//
// 语义：未设 / 空 → 120；"0" → 0（不启用）；负数 → 0（不启用）；非法 → 120；正常 → 原值。
func TestPendingMaxAgeSecEnvParsing(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"未设置", "", defaultPendingMaxAgeSec},
		{"显式 0 = 不启用", "0", 0},
		{"负数归一到不启用", "-1", 0},
		{"非法值回落默认", "twelve", defaultPendingMaxAgeSec},
		{"正常值原样使用", "300", 300},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("ANOTIFY_PENDING_MAX_AGE_SEC", c.raw)
			t.Setenv("ANOTIFY_EVENT_RETENTION_DAYS", "")
			if got := FromEnv().PendingMaxAgeSec; got != c.want {
				t.Errorf("ANOTIFY_PENDING_MAX_AGE_SEC=%q → %d, want %d", c.raw, got, c.want)
			}
		})
	}
}

// TestEventRetentionSecConversion 钉住 Config.EventRetentionSec() 的换算口径（契约 §1.3.1）。
//
// 关键不是 30*86400 这个算术，而是 **0 必须保持 0**：store 侧 0 = EventRetentionDisabled
// = 永久保留。任何「0 → 补成默认天数」的实现都会让「运维显式选择永久保留」变成
// 「不可逆地删 30 天前的事件」。
func TestEventRetentionSecConversion(t *testing.T) {
	cases := []struct {
		days int
		want int
	}{
		{0, 0},  // 永久保留：必须原样透传 0
		{-3, 0}, // 负数（直接写结构体的路径）也归一成 0
		{1, 86400},
		{30, 30 * 86400},
		{90, 90 * 86400},
	}
	for _, c := range cases {
		if got := (Config{EventRetentionDays: c.days}).EventRetentionSec(); got != c.want {
			t.Errorf("EventRetentionDays=%d → EventRetentionSec()=%d, want %d", c.days, got, c.want)
		}
	}
}

// TestRetentionAndBackpressureOverflowClamp 钉住两个「乘常数」旋钮的防溢出钳制。
//
// 为什么值得单独一条（#228 复审 MINOR）：两个字段都会被乘上常数再转成整数：
//
//	EventRetentionDays * 86400       → int
//	PendingMaxAgeSec   * time.Second → time.Duration（int64 纳秒）
//
// `strconv.Atoi` 在 64 位上能吃下到 9.2e18，乘完会**回绕**，而且回绕方向极其危险：
//
//	保留天数回绕成小正数  → cutoff = now - 小正数 → 一次删掉几乎全库（不可逆）；
//	背压阈值回绕成 ~290ms → 全站新事件立刻 503。
//
// 手滑多打几个 0 是最常见的事故来源，所以这里必须**确定性**地证明：超大输入不会
// 变成「很小的数」，而是回落到默认（解析层）或钳到上限（结构体直写路径）。
func TestRetentionAndBackpressureOverflowClamp(t *testing.T) {
	t.Run("解析层：超大天数钳到上限而不是回绕", func(t *testing.T) {
		// 这个值 × 86400 回绕后会变成约 169 秒 —— 也就是「保留期 = 3 分钟」。
		t.Setenv("ANOTIFY_EVENT_RETENTION_DAYS", "106751991167301")
		t.Setenv("ANOTIFY_PENDING_MAX_AGE_SEC", "")
		cfg := FromEnv()
		// 钳到上限而不是回落 30 天：填天文数字的人想的是「几乎永不删」，
		// 回落成 30 天等于加速删，方向反了（见 maxEventRetentionDays 注释）。
		if cfg.EventRetentionDays != maxEventRetentionDays {
			t.Fatalf("超大天数应钳到上限 %d，实际 %d", maxEventRetentionDays, cfg.EventRetentionDays)
		}
		if sec := cfg.EventRetentionSec(); sec != maxEventRetentionDays*86400 {
			t.Fatalf("EventRetentionSec 应是上限的秒数，实际 %d", sec)
		}
	})

	t.Run("解析层：超大背压秒数钳到上限", func(t *testing.T) {
		t.Setenv("ANOTIFY_PENDING_MAX_AGE_SEC", "9223372036854775807")
		t.Setenv("ANOTIFY_EVENT_RETENTION_DAYS", "")
		cfg := FromEnv()
		// 同理：填天文数字的人想的是「几乎不触发」，回落成 120 秒等于变严。
		if cfg.PendingMaxAgeSec != maxPendingMaxAgeSec {
			t.Fatalf("超大秒数应钳到上限 %d，实际 %d", maxPendingMaxAgeSec, cfg.PendingMaxAgeSec)
		}
		if d := cfg.PendingMaxAge(); d != time.Duration(maxPendingMaxAgeSec)*time.Second {
			t.Fatalf("PendingMaxAge 应是上限时长，实际 %v", d)
		}
	})

	t.Run("结构体直写路径也要钳住", func(t *testing.T) {
		// 绕过 FromEnv（测试/内嵌部署会这样写）：钳到上限，且绝不能变成负数或小数。
		big := Config{EventRetentionDays: 1 << 40, PendingMaxAgeSec: 1 << 40}
		if sec := big.EventRetentionSec(); sec != maxEventRetentionDays*86400 {
			t.Fatalf("超大天数应钳到上限 %d 秒，实际 %d", maxEventRetentionDays*86400, sec)
		}
		if d := big.PendingMaxAge(); d != time.Duration(maxPendingMaxAgeSec)*time.Second {
			t.Fatalf("超大秒数应钳到上限 %v，实际 %v", time.Duration(maxPendingMaxAgeSec)*time.Second, d)
		}
		// 钳位之后仍然必须是正数：负数会被 backpressure 当成「不启用」，
		// 正数极小则会让全站立刻 503 —— 两种回绕都不可接受。
		if big.EventRetentionSec() <= 0 || big.PendingMaxAge() <= 0 {
			t.Fatalf("钳位结果必须为正：sec=%d age=%v", big.EventRetentionSec(), big.PendingMaxAge())
		}
	})

	t.Run("0 与负数仍然是「不启用 / 永久保留」，不被钳位改写", func(t *testing.T) {
		c := Config{EventRetentionDays: 0, PendingMaxAgeSec: -5}
		if got := c.EventRetentionSec(); got != 0 {
			t.Fatalf("0 天必须是永久保留（0），实际 %d", got)
		}
		if got := c.PendingMaxAge(); got != 0 {
			t.Fatalf("负数必须是不启用（0），实际 %v", got)
		}
	})
}
