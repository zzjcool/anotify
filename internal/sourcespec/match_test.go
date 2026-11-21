package sourcespec

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func matchTargets(t *testing.T, raw string) []Target {
	t.Helper()
	var targets []Target
	if err := json.Unmarshal([]byte(raw), &targets); err != nil {
		t.Fatalf("解码 targets 失败: %v（%s）", err, raw)
	}
	return targets
}

func matchCauses(t *testing.T, raw string) map[string]Cause {
	t.Helper()
	return matchCausesOf(t, "["+raw+"]")
}

func matchCausesOf(t *testing.T, raw string) map[string]Cause {
	t.Helper()
	err := ValidateTargets(matchTargets(t, raw))
	if err == nil {
		t.Fatalf("应被拒，得到 nil（%s）", raw)
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("必须是 ErrInvalidConfig，得到 %v", err)
	}
	var causes *CausesError
	if !errors.As(err, &causes) {
		t.Fatalf("错误应带结构化 causes，得到 %T: %v", err, err)
	}
	out := map[string]Cause{}
	for _, cause := range causes.Causes {
		if cause.Message == "" {
			t.Fatalf("cause %q 没有 message", cause.Field)
		}
		out[cause.Field] = cause
	}
	return out
}

func TestMatchTypes(t *testing.T) {
	var none *Match
	if hit, detail := none.Matches("anything", map[string]any{}); !hit || detail != "" {
		t.Fatal("没写 match = 全部命中")
	}
	m := &Match{Types: []string{"issues.opened", "issue_comment.created"}}
	for eventType, want := range map[string]bool{
		"issues.opened": true, "Issues.Opened": false, "issues": false, "": false,
	} {
		if hit, _ := m.Matches(eventType, map[string]any{}); hit != want {
			t.Fatalf("match.types 必须精确且大小写敏感：%q → %v", eventType, hit)
		}
	}
}

// TestMatchLeafOps 逐条钉住叶子的比较表。
func TestMatchLeafOps(t *testing.T) {
	payload := map[string]any{
		"issue": map[string]any{
			"title":  "URGENT: disk full",
			"number": json.Number("7"),
			"draft":  false,
			"labels": []any{map[string]any{"name": "bug"}, map[string]any{"name": "p1"}},
			"body":   nil,
			"assignee": map[string]any{
				"login": "alice",
			},
		},
		"repository": map[string]any{"full_name": "me/infra"},
	}
	cases := []struct {
		name string
		leaf MatchLeaf
		want bool
	}{
		{"== 字符串命中", MatchLeaf{Field: "repository.full_name", Op: MatchOpEQ, Value: "me/infra"}, true},
		{"== 字符串不命中也算判完（false 不是错）", MatchLeaf{Field: "repository.full_name", Op: MatchOpEQ, Value: "me/other"}, false},
		{"== 布尔", MatchLeaf{Field: "issue.draft", Op: MatchOpEQ, Value: false}, true},
		{"== null 与显式 null 相等", MatchLeaf{Field: "issue.body", Op: MatchOpEQ, Value: nil}, true},
		{"!= 不等为真", MatchLeaf{Field: "issue.title", Op: MatchOpNE, Value: "x"}, true},
		{"!= 相等为假", MatchLeaf{Field: "issue.title", Op: MatchOpNE, Value: "URGENT: disk full"}, false},
		{"数组下标路径 + in 命中", MatchLeaf{Field: "issue.labels.0.name", Op: MatchOpIn, Value: []any{"bug", "regression"}}, true},
		{"数组下标路径 + in 不命中", MatchLeaf{Field: "issue.labels.1.name", Op: MatchOpIn, Value: []any{"bug", "regression"}}, false},
		{"数字按值比（==）", MatchLeaf{Field: "issue.number", Op: MatchOpEQ, Value: json.Number("7.0")}, true},
		{"> 数值", MatchLeaf{Field: "issue.number", Op: MatchOpGT, Value: json.Number("3")}, true},
		{"<= 边界", MatchLeaf{Field: "issue.number", Op: MatchOpLE, Value: json.Number("7")}, true},
		{"< 不成立", MatchLeaf{Field: "issue.number", Op: MatchOpLT, Value: json.Number("7")}, false},
		{"matches 命中", MatchLeaf{Field: "issue.title", Op: MatchOpMatches, Value: "^URGENT"}, true},
		{"matches 不命中", MatchLeaf{Field: "issue.title", Op: MatchOpMatches, Value: "^WIP"}, false},
		{"contains 大小写敏感", MatchLeaf{Field: "issue.title", Op: MatchOpContains, Value: "disk"}, true},
		{"contains 大小写不同即不命中", MatchLeaf{Field: "issue.title", Op: MatchOpContains, Value: "DISK"}, false},
		{"exists 存在", MatchLeaf{Field: "issue.assignee.login", Op: MatchOpExists}, true},
		{"exists 缺失", MatchLeaf{Field: "issue.milestone", Op: MatchOpExists}, false},
		{"缺失字段 + negate → 成立（!= 做不到）", MatchLeaf{Field: "issue.assignee.login", Op: MatchOpEQ, Value: "x", Negate: true}, true},
		{"缺失字段本身判 false（不是错）", MatchLeaf{Field: "issue.assignee.login", Op: MatchOpEQ, Value: "x"}, false},
		{"缺失字段 + != 仍是 false（文档语义）", MatchLeaf{Field: "issue.milestone", Op: MatchOpNE, Value: "x"}, false},
		{"类型不符（数字 > 字符串）→ false", MatchLeaf{Field: "repository.full_name", Op: MatchOpGT, Value: json.Number("3")}, false},
		{"类型不符（对象 contains）→ false", MatchLeaf{Field: "issue.assignee", Op: MatchOpContains, Value: "ali"}, false},
		{"negate 翻转类型不符", MatchLeaf{Field: "issue.assignee", Op: MatchOpContains, Value: "ali", Negate: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &Match{All: []MatchLeaf{tc.leaf}}
			hit, detail := store.Matches("anything", payload)
			if hit != tc.want {
				t.Fatalf("求值 %+v = %v，want %v（detail=%q）", tc.leaf, hit, tc.want, detail)
			}
			if !hit && detail == "" {
				t.Fatal("不命中必须给出可读 detail（回执里就靠它解释为什么没派发）")
			}
		})
	}
}

func TestMatchCombinatorSemantics(t *testing.T) {
	prIssue := map[string]any{"issue": map[string]any{
		"title": "URGENT", "number": json.Number("9"),
		"pull_request": map[string]any{"url": "https://api.github.com/pulls/1"},
	}}
	plainIssue := map[string]any{"issue": map[string]any{"title": "URGENT: hello", "number": json.Number("2")}}

	// 验收 1 的 PR-as-issue 判据：pull_request 存在即 PR（negate 后不成立）。
	prGate := MatchLeaf{Field: "issue.pull_request", Op: MatchOpExists, Negate: true}
	urgent := MatchLeaf{Field: "issue.title", Op: MatchOpMatches, Value: "^URGENT"}
	m := &Match{Types: []string{"issues.labeled"}, All: []MatchLeaf{prGate}, Any: []MatchLeaf{urgent}}
	if hit, detail := m.Matches("issues.labeled", prIssue); hit {
		t.Fatalf("PR-as-issue 必须被 all 挡住，detail=%q", detail)
	}
	if hit, _ := m.Matches("issues.labeled", plainIssue); !hit {
		t.Fatal("非 PR + any 命中 → 应派发")
	}
	// types 与 all/any 是 AND：事件类型不在白名单里 → 不命中（哪怕字段全中）。
	if hit, detail := m.Matches("issues.opened", plainIssue); hit || !strings.Contains(detail, "issues.labeled") {
		t.Fatalf("types 与 all/any 必须是 AND，detail=%q hit=%v", detail, hit)
	}
	// any 是 OR：任一行成立即可（第二行命中、第一行不命中）。
	orMatch := &Match{Any: []MatchLeaf{
		{Field: "issue.title", Op: MatchOpMatches, Value: "^WIP"},
		{Field: "issue.number", Op: MatchOpGT, Value: json.Number("5")},
	}}
	if hit, _ := orMatch.Matches("x", prIssue); !hit {
		t.Fatal("any 内是 OR：第二行命中就该派发")
	}
	// any 全部不成立 → 不命中，detail 点名行数（不回显 32 行全文）。
	if hit, detail := orMatch.Matches("x", plainIssue); hit || detail == "" {
		t.Fatalf("any 全不成立应剪枝，detail=%q hit=%v", detail, hit)
	}
	// 只有 any 命中、但 all 不成立 → 不派发（验收 5 的「只有 any 命中的用例」）。
	onlyAny := &Match{All: []MatchLeaf{prGate}, Any: []MatchLeaf{urgent}}
	if hit, _ := onlyAny.Matches("issues.labeled", prIssue); hit {
		t.Fatal("any 命中不能替代 all 的 AND")
	}
}

func TestMatchRuntimeToleratesBadStoredPattern(t *testing.T) {
	m := &Match{All: []MatchLeaf{{Field: "issue.title", Op: MatchOpMatches, Value: "("}}}
	if hit, _ := m.Matches("x", map[string]any{"issue": map[string]any{"title": "a"}}); hit {
		t.Fatal("编不过的 pattern 在运行期按不命中处理")
	}
}

func TestNormalizeMatchRejects(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		field  string
		reason string
		says   string
	}{
		{"空 match 对象", `{"capability":"x","match":{}}`, "targets[0].match", CauseReasonValueInvalid, "types / all / any"},
		{"types 空数组", `{"capability":"x","match":{"types":[]}}`, "targets[0].match.types", CauseReasonValueInvalid, "空数组"},
		{"types 空串", `{"capability":"x","match":{"types":[""]}}`, "targets[0].match.types[0]", CauseReasonValueInvalid, "不能为空"},
		{"types 重复（去空白后）", `{"capability":"x","match":{"types":["a"," a "]}}`, "targets[0].match.types[1]", CauseReasonValueInvalid, "重复"},
		{"空组合子 all", `{"capability":"x","match":{"all":[]}}`, "targets[0].match.all", CauseReasonValueInvalid, "空组合子"},
		{"空组合子 any", `{"capability":"x","match":{"any":[]}}`, "targets[0].match.any", CauseReasonValueInvalid, "空组合子"},
		{"未知键（顶层）", `{"capability":"x","match":{"type":["a"]}}`, "targets[0].match.type", CauseReasonUnknownField, "all / any / types"},
		{"未知键（叶子）", `{"capability":"x","match":{"all":[{"field":"a","op":"exists","valuee":1}]}}`, "targets[0].match.all[0].valuee", CauseReasonUnknownField, "field / negate / op / value"},
		{"非法 op", `{"capability":"x","match":{"all":[{"field":"a","op":"=~","value":"x"}]}}`, "targets[0].match.all[0].op", CauseReasonValueInvalid, "不认识的 op"},
		{"op 缺失", `{"capability":"x","match":{"all":[{"field":"a"}]}}`, "targets[0].match.all[0].op", CauseReasonValueInvalid, "必填"},
		{"正则编不过", `{"capability":"x","match":{"all":[{"field":"a.b","op":"matches","value":"(unclosed"}]}}`, "targets[0].match.all[0].value", CauseReasonValueInvalid, "RE2"},
		{"matches 的 value 不是字符串", `{"capability":"x","match":{"all":[{"field":"a","op":"matches","value":3}]}}`, "targets[0].match.all[0].value", CauseReasonValueInvalid, "必须是字符串"},
		{"路径语法非法", `{"capability":"x","match":{"all":[{"field":"issue.labels.0-name","op":"exists"}]}}`, "targets[0].match.all[0].field", CauseReasonValueInvalid, "路径段"},
		{"field 缺失", `{"capability":"x","match":{"all":[{"op":"exists"}]}}`, "targets[0].match.all[0].field", CauseReasonValueInvalid, "必填"},
		{"value 缺失", `{"capability":"x","match":{"all":[{"field":"a","op":"=="}]}}`, "targets[0].match.all[0].value", CauseReasonValueInvalid, "需要 value"},
		{"in 的 value 不是数组", `{"capability":"x","match":{"any":[{"field":"a","op":"in","value":"x"}]}}`, "targets[0].match.any[0].value", CauseReasonValueInvalid, "必须是数组"},
		{"in 的 value 空数组", `{"capability":"x","match":{"any":[{"field":"a","op":"in","value":[]}]}}`, "targets[0].match.any[0].value", CauseReasonValueInvalid, "空数组"},
		{"比较 op 的 value 不是数字", `{"capability":"x","match":{"all":[{"field":"a","op":">","value":"3"}]}}`, "targets[0].match.all[0].value", CauseReasonValueInvalid, "必须是数字"},
		{"contains 的 value 不是字符串", `{"capability":"x","match":{"all":[{"field":"a","op":"contains","value":1}]}}`, "targets[0].match.all[0].value", CauseReasonValueInvalid, "必须是字符串"},
		{"negate 不是布尔", `{"capability":"x","match":{"all":[{"field":"a","op":"exists","negate":"yes"}]}}`, "targets[0].match.all[0].negate", CauseReasonValueInvalid, "布尔"},
		{"叶子不是对象", `{"capability":"x","match":{"all":[1]}}`, "targets[0].match.all[0]", CauseReasonValueInvalid, "必须是对象"},
		{"match 不是对象", `{"capability":"x","match":[1]}`, "targets[0].match", CauseReasonValueInvalid, "JSON 对象"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			causes := matchCauses(t, tc.raw)
			cause, ok := causes[tc.field]
			if !ok {
				t.Fatalf("causes 里没有 %q，实际 %v", tc.field, causes)
			}
			if cause.Reason != tc.reason {
				t.Fatalf("%q 的 reason = %q，want %q", tc.field, cause.Reason, tc.reason)
			}
			if !strings.Contains(cause.Message, tc.says) {
				t.Fatalf("%q 的 message 应含 %q，得到 %q", tc.field, tc.says, cause.Message)
			}
		})
	}
}

func TestNormalizeMatchRowLimit(t *testing.T) {
	row := `{"field":"issue.title","op":"exists"}`
	rows := func(n int) string {
		out := make([]string, n)
		for i := range out {
			out[i] = row
		}
		return strings.Join(out, ",")
	}
	for _, key := range []string{"all", "any"} {
		atLimit := `[{"capability":"x","match":{"` + key + `":[` + rows(MatchMaxRows) + `]}}]`
		if err := ValidateTargets(matchTargets(t, atLimit)); err != nil {
			t.Fatalf("%s 恰好 %d 行应通过，得到 %v", key, MatchMaxRows, err)
		}
		causes := matchCauses(t, `{"capability":"x","match":{"`+key+`":[`+rows(MatchMaxRows+1)+`]}}`)
		cause, found := causes["targets[0].match."+key]
		if !found || !strings.Contains(cause.Message, "32") {
			t.Fatalf("%s 超限应 400 且文案含上限 32，得到 %v", key, causes)
		}
	}
}

func TestValidateTargetsReportsAllTargets(t *testing.T) {
	raw := `[
		{"capability":"a","match":{"all":[{"field":"x","op":"nope","value":1}]}},
		{"capability":"","match":{"types":[]}},
		{"capability":"b","match":{"any":[{"field":"x","op":"exists","negate":1}]}}
	]`
	causes := matchCausesOf(t, raw)
	for _, field := range []string{
		"targets[0].match.all[0].op",
		"targets[1].capability",
		"targets[1].match.types",
		"targets[2].match.any[0].negate",
	} {
		if _, ok := causes[field]; !ok {
			t.Fatalf("缺 cause %q，实际 %v", field, causes)
		}
	}
}

func TestMatchNormalizationRoundTrip(t *testing.T) {
	targets := matchTargets(t, `[{"capability":"x","match":{"types":[" issues.opened "],
		"all":[{"field":"$.issue.pull_request","op":"exists","negate":true}],
		"any":[{"field":"issue.labels.0.name","op":"in","value":["bug"]}]}}]`)
	if err := ValidateTargets(targets); err != nil {
		t.Fatalf("首轮校验失败: %v", err)
	}
	match := targets[0].Match
	if match.Types[0] != "issues.opened" || match.All[0].Field != "issue.pull_request" {
		t.Fatalf("归一没生效（types 去空白 / field 去 $.）：%+v", match)
	}
	encoded, err := json.Marshal(targets)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(encoded), `"raw"`) || strings.Contains(string(encoded), "$.") {
		t.Fatalf("落库字节里不该有内部字段或未归一的路径：%s", encoded)
	}
	reloaded := matchTargets(t, string(encoded))
	if err := ValidateTargets(reloaded); err != nil {
		t.Fatalf("往返后校验失败: %v", err)
	}
	if reloaded[0].Match.All[0].Negate != true || reloaded[0].Match.Any[0].Value.([]any)[0] != "bug" {
		t.Fatalf("往返丢了配置：%+v", reloaded[0].Match)
	}
	// null match 与「不写 match」同义（= 恒命中）。
	if err := ValidateTargets(matchTargets(t, `[{"capability":"x","match":null}]`)); err != nil {
		t.Fatalf("match: null 应等同于不写，得到 %v", err)
	}
}
