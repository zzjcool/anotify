package server

import (
	"testing"
	"time"
)

// ANOTIFY_STATS_INTERVAL_SEC 的解析语义（#265）。
//
// 三态必须可区分，否则「关掉周期 ANALYZE」在运维侧不可达：
//
//	未设置 / 非法 → 0  （用 lease 默认 6h）
//	"0"           → -1 （禁用）
//	正数           → 该秒数（上限 7 天）
func TestStatsIntervalFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  string // "" = 不设置
		want int
	}{
		{"未设置 → 默认", "", 0},
		{"显式 0 → 禁用", "0", -1},
		{"负数 → 禁用", "-5", -1},
		{"正数 → 原值", "900", 900},
		{"非法 → 默认（拼错不该关掉维护）", "abc", 0},
		{"超出上限 → 钳到 7 天", "99999999", 7 * 24 * 3600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == "" {
				t.Setenv("ANOTIFY_STATS_INTERVAL_SEC", "")
			} else {
				t.Setenv("ANOTIFY_STATS_INTERVAL_SEC", tc.env)
			}
			if got := statsIntervalFromEnv("ANOTIFY_STATS_INTERVAL_SEC"); got != tc.want {
				t.Fatalf("statsIntervalFromEnv(%q) = %d, want %d", tc.env, got, tc.want)
			}
		})
	}
}

// TestConfigStatsIntervalMapping 锁定 Config → lease 的三态映射。
func TestConfigStatsIntervalMapping(t *testing.T) {
	if d := (Config{}).StatsInterval(); d != 0 {
		t.Fatalf("未设置应返回 0（= lease 用默认），实际 %v", d)
	}
	if d := (Config{StatsIntervalSec: 120}).StatsInterval(); d != 2*time.Minute {
		t.Fatalf("正数应换算成 Duration，实际 %v", d)
	}
	if d := (Config{StatsIntervalSec: -1}).StatsInterval(); d != -1 {
		t.Fatalf("禁用应返回 -1，实际 %v", d)
	}
}
