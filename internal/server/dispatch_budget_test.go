package server

import (
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/route"
)

// 本文件是**两个派发预算**（ANOTIFY_MAX_HOPS / ANOTIFY_TRACE_TASK_BUDGET）在配置层的验收：
//   - 不配 env 时用 route 包的默认值（不抄第二份）；
//   - 非法（非正整数字面量）→ 启动即失败，不静默用默认。
//
// 这三条原在 capability_balance_test.go 里（那个文件是为了 balance 建的）。P3 删掉
// balance 后，与它有关系的两条用例随之消失，预算这三条与 balance 毫无关系，搬到这里
// 继续守着——「删一个字段」不该顺手放掉另一个功能的门禁。

// TestDispatchBudgetDefaults 不配 env 时，配置层的值 = route 包的默认值。
func TestDispatchBudgetDefaults(t *testing.T) {
	// 不配 env 时 FromEnv 给出默认值（不是 0，也不是别的数）。
	// 默认值只有一份（route.Default*），所以这里同时守住「别在配置层抄第二份」。
	t.Setenv("ANOTIFY_MAX_HOPS", "")
	t.Setenv("ANOTIFY_TRACE_TASK_BUDGET", "")
	cfg := FromEnv()
	if cfg.MaxHops != route.DefaultMaxHops || cfg.TraceTaskBudget != route.DefaultTraceTaskBudget {
		t.Fatalf("默认预算 = (%d, %d), want (%d, %d)",
			cfg.MaxHops, cfg.TraceTaskBudget, route.DefaultMaxHops, route.DefaultTraceTaskBudget)
	}
}

// TestDispatchBudgetEnvParsing 正整数原样使用；其它值留给 Validate 拒绝（哨兵 0）。
func TestDispatchBudgetEnvParsing(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"未设置", "", route.DefaultMaxHops},
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

// TestValidateRejectsBadDispatchBudgets 非法预算 ⇒ **启动即失败**（不静默用默认）。
func TestValidateRejectsBadDispatchBudgets(t *testing.T) {
	base := Config{
		VAPIDPublic: "pub", VAPIDPriv: "priv",
		MaxHops: route.DefaultMaxHops, TraceTaskBudget: route.DefaultTraceTaskBudget,
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
			cfg.VAPIDPublic, cfg.VAPIDPriv = "pub", "priv" // VAPID 不是本用例的关注点
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
