package sourcespec

import (
	"errors"
	"strings"
	"testing"
)

func TestOutcomeNamePattern(t *testing.T) {
	ok := []string{"close", "triage", "a", "a1", "a-b", "a_b", "0lead", strings.Repeat("a", 32)}
	for _, name := range ok {
		if !IsOutcomeName(name) {
			t.Fatalf("%q 应该是合法结局名", name)
		}
	}
	bad := []string{"", "Close", "has space", "dot.name", "-lead", "_lead", "a/b", strings.Repeat("a", 33), "参数"}
	for _, name := range bad {
		if IsOutcomeName(name) {
			t.Fatalf("%q 不该是合法结局名（小写字母/数字/下划线/连字符，首字符字母或数字，≤32）", name)
		}
	}
}

func TestValidateOutcomesLegalShapes(t *testing.T) {
	cases := map[string]map[string][]OutcomeAction{
		"空表":       {},
		"drop 空动作": {"drop": {}},
		"单动作":      {"close": {{Capability: "issue-close"}}},
		"decision 根名": {"close": {{Capability: "issue-close",
			Params: map[string]any{"reason": "$(decision.reason)"}}}},
		"payload + decision 混写": {"close": {{Capability: "issue-close",
			Params: map[string]any{"title": "$(payload.issue.title)/$(decision.reason)"}}}},
	}
	for name, outcomes := range cases {
		if causes := ValidateOutcomes(outcomes, "targets[0]"); len(causes) != 0 {
			t.Fatalf("%s：应合法，得到 %v", name, causes)
		}
	}
}

func TestValidateOutcomesCauses(t *testing.T) {
	tooMany := map[string][]OutcomeAction{}
	for i := 0; i < MaxOutcomes+1; i++ {
		tooMany[string(rune('a'+i%26))+strings.Repeat("x", i/26)] = nil
	}
	tooManyActions := make([]OutcomeAction, MaxOutcomeActions+1)
	for i := range tooManyActions {
		tooManyActions[i] = OutcomeAction{Capability: "c"}
	}
	cases := []struct {
		name     string
		outcomes map[string][]OutcomeAction
		wantSub  string
	}{
		{"结局太多", tooMany, "targets[0].outcomes"},
		{"结局名非法", map[string][]OutcomeAction{"Close": nil}, "targets[0].outcomes"},
		{"动作太多", map[string][]OutcomeAction{"close": tooManyActions}, "targets[0].outcomes.close"},
		{"动作缺能力", map[string][]OutcomeAction{"close": {{Capability: " "}}}, "targets[0].outcomes.close[0].capability"},
		{"模板根名写坏", map[string][]OutcomeAction{"close": {{Capability: "c",
			Params: map[string]any{"r": "$(decision.a-b)"}}}}, "targets[0].outcomes.close[0].params"},
		{"target 参数不许用 decision", map[string][]OutcomeAction{"close": {{Capability: "c",
			Params: map[string]any{"r": "$(decision.reason)"}}}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			causes := ValidateOutcomes(tc.outcomes, "targets[0]")
			if tc.wantSub == "" {
				if len(causes) != 0 {
					t.Fatalf("应合法：%v", causes)
				}
				return
			}
			if len(causes) == 0 {
				t.Fatalf("应报 cause（want field 前缀 %q）", tc.wantSub)
			}
			found := false
			for _, cause := range causes {
				if cause.Field == tc.wantSub {
					found = true
				}
				if cause.Reason != CauseReasonValueInvalid {
					t.Fatalf("cause.reason = %q，want value_invalid（封闭词表）", cause.Reason)
				}
				if cause.Message == "" {
					t.Fatalf("cause.Message 不能为空（人要看得懂）")
				}
			}
			if !found {
				t.Fatalf("没有 field == %q 的 cause：%v", tc.wantSub, causes)
			}
		})
	}
}

func TestValidateTargetsMergesOutcomeCauses(t *testing.T) {
	targets := []Target{{
		Capability: "router",
		Match:      &Match{All: []MatchLeaf{{Field: "a", Op: "~="}}},
		Outcomes:   map[string][]OutcomeAction{"Close": nil},
	}}
	err := ValidateTargets(targets)
	var causes *CausesError
	if !errors.As(err, &causes) {
		t.Fatalf("应返回 CausesError，得到 %v", err)
	}
	fields := map[string]bool{}
	for _, cause := range causes.Causes {
		fields[cause.Field] = true
	}
	if !fields["targets[0].outcomes"] {
		t.Fatalf("outcomes 的 cause 丢了：%v", causes.Causes)
	}
	if len(fields) < 2 {
		t.Fatalf("match 与 outcomes 的问题应一起报：%v", causes.Causes)
	}
}

func TestDeclaredRefUniqueness(t *testing.T) {
	cases := []struct {
		next []string
		to   string
		want string
		ok   bool
	}{
		{[]string{"a"}, "a", "a", true},
		{[]string{"a@2"}, "a", "a@2", true},
		{[]string{"a", "a@2"}, "a", "", false},   // 裸名 + 钉版混写：无法唯一选择
		{[]string{"a@1", "a@2"}, "a", "", false}, // 同名多钉版：不偷偷挑
		{[]string{"a@1", "a@2"}, "a@2", "a@2", true},
		{[]string{"a"}, "a@2", "", false}, // 显式钉版不能因有裸名就放行
		{[]string{"a"}, "a@0", "", false}, // 坏版本写法永不匹配
		{[]string{"a@0"}, "a", "", false}, // 声明侧的坏写法也不匹配裸名
		{[]string{"b"}, "a", "", false},
	}
	for _, tc := range cases {
		got, ok := DeclaredRef(tc.next, tc.to)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("DeclaredRef(%v, %q) = (%q,%v)，want (%q,%v)", tc.next, tc.to, got, ok, tc.want, tc.ok)
		}
	}
}

func TestRenderTargetTwoPhase(t *testing.T) {
	target := Target{
		Capability: "router",
		Params:     map[string]any{"event": "$(payload)"},
		Outcomes: map[string][]OutcomeAction{
			"close": {{
				Capability: "issue-close",
				Params: map[string]any{
					"reason": "$(decision.reason)",
					"title":  "$(payload.issue.title)",
					"count":  "$(decision.count)",
					"fixed":  "x",
				},
			}},
		},
	}
	payload := map[string]any{"issue": map[string]any{"title": "spam"}}
	params, outcomes, missing := RenderTarget(target, payload)
	if len(missing) != 0 {
		t.Fatalf("不该缺参数：%v", missing)
	}
	if got := params["event"]; got == nil {
		t.Fatalf("整值 $(payload) 应保留原对象：%v", params)
	}
	action := outcomes["close"][0]
	if action.Params["title"] != "spam" {
		t.Fatalf("payload 引用应在建任务时求值：%v", action.Params)
	}
	if action.Params["reason"] != "$(decision.reason)" {
		t.Fatalf("decision 引用必须原样保留（判决还没发生）：%v", action.Params)
	}
	if action.Params["fixed"] != "x" {
		t.Fatalf("常量丢了：%v", action.Params)
	}

	rendered, gaps := RenderDecisionParams(action.Params, map[string]any{"reason": "广告", "count": 3})
	if len(gaps) != 0 {
		t.Fatalf("decision 参数齐全时不该缺：%v", gaps)
	}
	if rendered["reason"] != "广告" {
		t.Fatalf("decision.reason 没展开：%v", rendered)
	}
	if _, isString := rendered["count"].(string); isString {
		t.Fatalf("整值引用必须保留原类型：%v (%T)", rendered["count"], rendered["count"])
	}
	if _, gaps := RenderDecisionParams(action.Params, map[string]any{"reason": "x"}); len(gaps) != 1 || gaps[0] != "count" {
		t.Fatalf("缺 decision 参数必须报出来：%v", gaps)
	}

	_, _, missing = RenderTarget(target, map[string]any{"issue": map[string]any{}})
	if len(missing) != 1 || missing[0] != "outcomes.close[0].params.title" {
		t.Fatalf("missing = %v，want [outcomes.close[0].params.title]", missing)
	}
}
