package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/route"
)

// 本文件是契约 §1（capability_versions.balance）与 §2（两个派发预算）在**接线层**的验收：
//   - balance 里出现未实现的 mode → 保存时 400（"尚未实现"），**不静默降级**；
//   - ANOTIFY_MAX_HOPS / ANOTIFY_TRACE_TASK_BUDGET 非法（非正整数）→ 启动即失败；
//   - 两个默认值与 route 包的默认值字面量一致（不配 env 时预算不会变成另一个数）。

// TestPostCapabilityBalanceUnimplemented400 预留模式（all/random/least_loaded/hash）→ 400。
func TestPostCapabilityBalanceUnimplemented400(t *testing.T) {
	for _, mode := range []string{"all", "random", "least_loaded", "hash"} {
		t.Run(mode, func(t *testing.T) {
			env := newConsoleEnv(t)
			baseCaps := countIn(t, env, `SELECT count(*) FROM capability_versions`)

			res := postCapabilityLinks(t, env, map[string]any{
				"namespaceId": env.nsID,
				"name":        "fanout.wannabe",
				"outputs":     []string{"fix"},
				"balance":     map[string]any{"fix": map[string]any{"mode": mode}},
			})
			if res.code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400（body=%s）", res.code, res.body)
			}
			m := res.json(t)
			if m["code"] != "balance_rejected" {
				t.Fatalf("code = %v, want balance_rejected（body=%s）", m["code"], res.body)
			}
			errText, _ := m["error"].(string)
			if !strings.Contains(errText, "尚未实现") {
				t.Fatalf("400 的 error 里没有「尚未实现」: %q", errText)
			}
			// 拒了就是什么都没建（能力表是 append-only，半条数据撤不回来）。
			if n := countIn(t, env, `SELECT count(*) FROM capability_versions`); n != baseCaps {
				t.Fatalf("被拒的请求留下了 %d 行能力（基线 %d）", n, baseCaps)
			}
		})
	}
}

// TestPostCapabilityBalanceStored 实现的模式（priority，或缺省）照常落库并回显。
func TestPostCapabilityBalanceStored(t *testing.T) {
	env := newConsoleEnv(t)
	res := postCapabilityLinks(t, env, map[string]any{
		"namespaceId": env.nsID,
		"name":        "review.withbalance",
		"outputs":     []string{"fix"},
		"balance":     map[string]any{"fix": map[string]any{"mode": "priority"}},
	})
	if res.code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body=%s）", res.code, res.body)
	}
	m := res.json(t)
	capObj, _ := m["capability"].(map[string]any)
	balance, _ := capObj["balance"].(map[string]any)
	fix, _ := balance["fix"].(map[string]any)
	if fix == nil || fix["mode"] != "priority" {
		t.Fatalf("响应里的 balance = %v, want {fix:{mode:priority}}", capObj["balance"])
	}
	// 缺省（不传 balance）也要回 `{}` 而不是 null（不变量 I9）。
	env.seedCapability(t, "review.plain", 1, nil)
	got := env.mustGetCapability(t, "review.plain", 1)
	if got.Balance == nil {
		t.Fatal("没配 balance 的能力读回来是 nil（JSON 会变成 null）")
	}
}

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
