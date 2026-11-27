package sourcespec

import (
	"strings"
	"testing"
)

func derivedMatch(t *testing.T, raw string) *Match {
	t.Helper()
	targets := matchTargets(t, raw)
	if err := ValidateTargets(targets); err != nil {
		t.Fatalf("应通过校验，得到 %v", err)
	}
	return targets[0].Match
}

func TestDerivedIsPullRequest(t *testing.T) {
	m := derivedMatch(t, `[{"capability":"x","match":{"all":[{"field":"_derived.isPullRequest","op":"==","value":true}]}}]`)
	cases := map[string]struct {
		payload map[string]any
		hit     bool
	}{
		"issue_comment 在 PR 上": {map[string]any{"issue": map[string]any{"pull_request": map[string]any{"url": "u"}}}, true},
		"issues 事件里的普通 issue":  {map[string]any{"issue": map[string]any{"number": 1}}, false},
		"pull_request 事件自身":    {map[string]any{"pull_request": map[string]any{"number": 2}}, true},
		"非 GitHub 载荷":          {map[string]any{"kind": "ping"}, false},
		"空载荷":                  {map[string]any{}, false},
		"别处的 pull_request 键不算": {map[string]any{"repo": map[string]any{"pull_request": map[string]any{"number": 3}}}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if hit, detail := m.Matches("x", tc.payload); hit != tc.hit {
				t.Fatalf("命中 = %v，want %v（detail %q）", hit, tc.hit, detail)
			}
		})
	}
}

func TestDerivedLinksCountsBodyOnly(t *testing.T) {
	cases := map[string]struct {
		payload map[string]any
		want    int
	}{
		"body 里 http 与 https 各算一条": {map[string]any{"issue": map[string]any{
			"body": "看 http://a.example 与 https://b.example",
		}}, 2},
		"html_url / avatar_url / diff_url 不算": {map[string]any{"issue": map[string]any{
			"body":       "https://a.example",
			"html_url":   "https://b.example https://c.example",
			"avatar_url": "https://d.example",
			"diff_url":   "https://e.example",
		}}, 1},
		"text / content 键不算（只认 body）": {map[string]any{"issue": map[string]any{
			"body":    "https://a.example",
			"text":    "https://b.example https://c.example",
			"content": "https://d.example",
		}}, 1},
		"嵌套层的非 body 键也不算": {map[string]any{"issue": map[string]any{
			"body": "https://a.example",
			"user": map[string]any{"avatar_url": "https://b.example", "note": "https://c.example"},
		}}, 1},
		"没有 body": {map[string]any{"kind": "ping"}, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := (&derived{payload: tc.payload}).countLinks(); got != tc.want {
				t.Fatalf("链接数 = %d，want %d（只数键名 body 的字符串值）", got, tc.want)
			}
		})
	}
	m := derivedMatch(t, `[{"capability":"x","match":{"all":[{"field":"_derived.links","op":">=","value":2}]}}]`)
	spam := map[string]any{"issue": map[string]any{
		"body":     "看 https://a.example 与 http://b.example 还有 https://c.example",
		"html_url": "https://d.example https://e.example https://f.example",
	}}
	if hit, detail := m.Matches("x", spam); !hit {
		t.Fatalf("正文 3 条链接应命中 >= 2（detail %q）", detail)
	}
	one := map[string]any{"issue": map[string]any{
		"body":       "只有 https://a.example",
		"html_url":   "https://b.example https://c.example",
		"avatar_url": "https://d.example https://e.example",
	}}
	if hit, detail := m.Matches("x", one); hit {
		t.Fatalf("正文 1 条 + 平台 URL 4 条不该命中 >= 2（detail %q）", detail)
	}
	if hit, _ := m.Matches("x", map[string]any{"kind": "ping"}); hit {
		t.Fatal("没有 body 的载荷算 0 条")
	}
}

func TestDerivedLinksSumsNestedBodies(t *testing.T) {
	d := &derived{payload: map[string]any{
		"issue":   map[string]any{"body": "https://a.example"},
		"comment": map[string]any{"body": "https://b.example https://c.example"},
		"items": []any{
			map[string]any{"body": "http://d.example"},
			"https://ignored.example",
			map[string]any{"text": "https://also-ignored.example", "meta": map[string]any{"html_url": "https://still-ignored.example"}},
		},
	}}
	if got := d.countLinks(); got != 4 {
		t.Fatalf("链接数 = %d，want 4（任意深度的 body 都算）", got)
	}
	if again := d.countLinks(); again != 4 {
		t.Fatalf("第二次求值 = %d，want 4（算一次就够）", again)
	}
}

func TestDerivedLinksRuleTable(t *testing.T) {
	m := derivedMatch(t, `[{"capability":"x","match":{"all":[
		{"field":"_derived.links","op":">","value":3},
		{"field":"issue.title","op":"contains","value":"URGENT"}]}}]`)
	payload := map[string]any{"issue": map[string]any{
		"title": "URGENT 招人",
		"body":  "https://a https://b https://c https://d",
	}}
	if hit, detail := m.Matches("x", payload); !hit {
		t.Fatalf("4 条链接 + URGENT 应命中（detail %q）", detail)
	}
	payload["issue"].(map[string]any)["body"] = "https://a https://b https://c"
	if hit, detail := m.Matches("x", payload); hit || !strings.Contains(detail, "_derived.links") {
		t.Fatalf("3 条链接不该命中 > 3，且 detail 要点出这一行（得到 %q）", detail)
	}
}

func TestDerivedFieldAcceptsDollarPrefix(t *testing.T) {
	m := derivedMatch(t, `[{"capability":"x","match":{"all":[{"field":"$._derived.links","op":"==","value":1}]}}]`)
	if m.All[0].Field != "_derived.links" {
		t.Fatalf("归一后的 field = %q，want _derived.links", m.All[0].Field)
	}
	if hit, detail := m.Matches("x", map[string]any{"issue": map[string]any{"body": "https://a"}}); !hit {
		t.Fatalf("$._derived.links 应与 _derived.links 同义（detail %q）", detail)
	}
}

func TestDerivedExistsIsAlwaysTrue(t *testing.T) {
	m := derivedMatch(t, `[{"capability":"x","match":{"all":[{"field":"_derived.links","op":"exists"}]}}]`)
	if hit, detail := m.Matches("x", map[string]any{}); !hit {
		t.Fatalf("派生字段恒可取（0 条也是值），exists 应为真（detail %q）", detail)
	}
}

func TestDerivedPrefixWinsOverPayloadKey(t *testing.T) {
	payload := map[string]any{
		"_derived": map[string]any{"links": 99},
		"issue":    map[string]any{"body": "https://a.example"},
	}
	one := derivedMatch(t, `[{"capability":"x","match":{"all":[{"field":"_derived.links","op":"==","value":1}]}}]`)
	if hit, detail := one.Matches("x", payload); !hit {
		t.Fatalf("_derived.links 应算派生值 1，不是载荷里的 99（detail %q）", detail)
	}
	ninety := derivedMatch(t, `[{"capability":"x","match":{"all":[{"field":"_derived.links","op":"==","value":99}]}}]`)
	if hit, detail := ninety.Matches("x", payload); hit {
		t.Fatalf("载荷里真有 _derived 键也不该被读到（detail %q）", detail)
	}
}

func TestDerivedPrefixNeedsDotBoundary(t *testing.T) {
	m := derivedMatch(t, `[{"capability":"x","match":{"all":[{"field":"_derivedlike.links","op":"==","value":2}]}}]`)
	payload := map[string]any{"_derivedlike": map[string]any{"links": 2}}
	if hit, detail := m.Matches("x", payload); !hit {
		t.Fatalf("保留前缀是 _derived. 带点，_derivedlike 应按载荷指针取到（detail %q）", detail)
	}
}

func TestDerivedUnknownNameOnReadPathIsFalse(t *testing.T) {
	m := &Match{All: []MatchLeaf{{Field: "_derived.ageDays", Op: MatchOpExists}}}
	payload := map[string]any{"_derived": map[string]any{"ageDays": 30}}
	if hit, detail := m.Matches("x", payload); hit {
		t.Fatalf("读路径上不认识的派生名按「取不到」处理，exists 应为假（detail %q）", detail)
	}
}

func TestDerivedIsPinnedToReplayablePayload(t *testing.T) {
	payload := map[string]any{"issue": map[string]any{"body": "https://a https://b"}}
	m := derivedMatch(t, `[{"capability":"x","match":{"any":[{"field":"_derived.links","op":"==","value":2}]}}]`)
	for i := 0; i < 3; i++ {
		if hit, detail := m.Matches("x", payload); !hit {
			t.Fatalf("第 %d 次求值变了（detail %q）—— 派生值必须是载荷的纯函数", i, detail)
		}
	}
}

func TestNormalizeMatchRejectsDerived(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		field string
		says  string
	}{
		{"不认识的派生字段", `{"capability":"x","match":{"all":[{"field":"_derived.ageDays","op":">","value":7}]}}`,
			"targets[0].match.all[0].field", "isPullRequest"},
		{"嵌套的派生路径", `{"capability":"x","match":{"any":[{"field":"_derived.isPullRequest.x","op":"exists"}]}}`,
			"targets[0].match.any[0].field", "不认识的派生字段"},
		{"只写根名", `{"capability":"x","match":{"all":[{"field":"_derived","op":"exists"}]}}`,
			"targets[0].match.all[0].field", "不认识的派生字段"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cause, ok := matchCauses(t, tc.raw)[tc.field]
			if !ok {
				t.Fatalf("causes 里没有 %q", tc.field)
			}
			if cause.Reason != CauseReasonValueInvalid || !strings.Contains(cause.Message, tc.says) {
				t.Fatalf("cause = %+v，want reason %q 含 %q", cause, CauseReasonValueInvalid, tc.says)
			}
		})
	}
}
