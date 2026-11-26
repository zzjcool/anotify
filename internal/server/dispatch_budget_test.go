package server

// 派发预算（ANOTIFY_MAX_HOPS / ANOTIFY_TRACE_TASK_BUDGET）的配置层验收。
//
// 为什么恢复这组用例（m4-2）：review 指出 v3 的三条派发预算用例被退役，理由是
// 「env 解析的对象不存在了」——但 `Config.MaxHops/TraceTaskBudget` 仍在 `FromEnv`
// 解析、`Validate` 启动校验、`mux.go` 启动日志打印，退役后配置面完全裸奔。
// 消费方是 `route.Router.checkBudget`（每次派发都判，见 internal/route/budget.go）。
//
// 断言逐条沿用 v3：
//   - 不配 env → 默认值（16 / 512，引 route 的常量）；
//   - 正整数原样使用；0 / 负数 / 非数字 → 哨兵 0，由 Validate 拒绝（启动即失败）。

import (
	"strings"
	"testing"
)

// TestDispatchBudgetDefaults 不配 env 时，配置层的值 = 本包默认值（不是 0）。
func TestDispatchBudgetDefaults(t *testing.T) {
	t.Setenv("ANOTIFY_MAX_HOPS", "")
	t.Setenv("ANOTIFY_TRACE_TASK_BUDGET", "")
	cfg := FromEnv()
	if cfg.MaxHops != defaultMaxHops || cfg.TraceTaskBudget != defaultTraceTaskBudget {
		t.Fatalf("默认预算 = (%d, %d), want (%d, %d)",
			cfg.MaxHops, cfg.TraceTaskBudget, defaultMaxHops, defaultTraceTaskBudget)
	}
}

// TestDispatchBudgetEnvParsing 正整数原样使用；其它值留给 Validate 拒绝（哨兵 0）。
func TestDispatchBudgetEnvParsing(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"未设置", "", defaultMaxHops},
		{"正整数原样使用", "7", 7},
		{"0 非法", "0", 0},
		{"负数非法", "-3", 0},
		{"非数字非法", "sixteen", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("ANOTIFY_MAX_HOPS", c.raw)
			t.Setenv("ANOTIFY_TRACE_TASK_BUDGET", "")
			if got := FromEnv().MaxHops; got != c.want {
				t.Errorf("ANOTIFY_MAX_HOPS=%q → %d, want %d", c.raw, got, c.want)
			}
		})
	}
}

// TestValidateRejectsBadDispatchBudgets 非法预算 ⇒ 启动即失败（不静默用默认）。
func TestValidateRejectsBadDispatchBudgets(t *testing.T) {
	base := Config{
		MaxHops: defaultMaxHops, TraceTaskBudget: defaultTraceTaskBudget,
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("合法配置被拒: %v", err)
	}

	for _, tc := range []struct {
		name string
		key  string
		raw  string
	}{
		{"hop=0", "ANOTIFY_MAX_HOPS", "0"},
		{"hop=负数", "ANOTIFY_MAX_HOPS", "-1"},
		{"budget=非数字", "ANOTIFY_TRACE_TASK_BUDGET", "lots"},
		{"budget=0", "ANOTIFY_TRACE_TASK_BUDGET", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.raw)
			cfg := FromEnv()
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("%s=%q 被接受了：非法预算必须让启动失败", tc.key, tc.raw)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("错误里没说是哪个 env: %v", err)
			}
		})
	}
}
