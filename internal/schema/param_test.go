package schema

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseParamsAvailability(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantPaths []string
	}{
		{name: "空（省略）", raw: ``, wantPaths: nil},
		{name: "null", raw: `null`, wantPaths: nil},
		{name: "空对象", raw: `{}`, wantPaths: nil},
		{
			name:      "合法子集",
			raw:       `{"type":"object","properties":{"repo":{"type":"string","pattern":"^[a-z]+$"}},"required":["repo"]}`,
			wantPaths: nil,
		},
		{name: "type 数组含 object", raw: `{"type":["object","null"]}`, wantPaths: nil},
		{
			name:      "未知类型名", // #580：笔误（strin）会让类型判定静默失效，不能 201 收下
			raw:       `{"type":"object","properties":{"x":{"type":"weird"}}}`,
			wantPaths: []string{"properties.x.type"},
		},
		{
			name:      "表外关键字（oneOf）",
			raw:       `{"type":"object","properties":{"x":{"oneOf":[{"type":"string"}]}}}`,
			wantPaths: []string{"properties.x.oneOf"},
		},
		{
			name:      "表外关键字（$ref）",
			raw:       `{"type":"object","properties":{"x":{"$ref":"#/defs/a"}}}`,
			wantPaths: []string{"properties.x.$ref"},
		},
		{
			name:      "注释类关键字不参与校验，接受",
			raw:       `{"type":"object","title":"输入","properties":{"x":{"type":"string","description":"名字","examples":["a"]}}}`,
			wantPaths: nil,
		},
		{name: "type 不是 object", raw: `{"type":"string"}`, wantPaths: []string{"type"}},
		{name: "type 数组不含 object", raw: `{"type":["string","null"]}`, wantPaths: []string{"type"}},
		{name: "type 是数字", raw: `{"type":7}`, wantPaths: []string{"type"}},
		{name: "不是 JSON 对象", raw: `[]`, wantPaths: []string{""}},
		{
			name:      "参数名就叫 pattern（属性值不是对象）",
			raw:       `{"type":"object","properties":{"pattern":3}}`,
			wantPaths: []string{"properties.pattern"},
		},
		{
			name:      "参数名以 .pattern 结尾",
			raw:       `{"type":"object","properties":{"x.pattern":3}}`,
			wantPaths: []string{"properties.x.pattern"},
		},
		{
			name:      "参数名就叫 pattern（漏写一层对象）",
			raw:       `{"type":"object","properties":{"pattern":"^x$"}}`,
			wantPaths: []string{"properties.pattern"},
		},
		{
			name:      "参数名就叫 pattern 且嵌套里有坏属性值",
			raw:       `{"type":"object","properties":{"pattern":{"properties":{"pattern":3}}}}`,
			wantPaths: []string{"properties.pattern.properties.pattern"},
		},
		{
			name:      "坏 pattern（RE2 编不过）",
			raw:       `{"type":"object","properties":{"repo":{"type":"string","pattern":"(["}}}`,
			wantPaths: []string{"properties.repo.pattern"},
		},
		{
			name:      "pattern 不是字符串",
			raw:       `{"type":"object","properties":{"repo":{"type":"string","pattern":7}}}`,
			wantPaths: []string{"properties.repo.pattern"},
		},
		{
			name:      "嵌套 properties 的坏 pattern",
			raw:       `{"type":"object","properties":{"meta":{"type":"object","properties":{"dir":{"type":"string","pattern":"("}}}}}`,
			wantPaths: []string{"properties.meta.properties.dir.pattern"},
		},
		{
			name:      "属性不是对象",
			raw:       `{"type":"object","properties":{"a":3}}`,
			wantPaths: []string{"properties.a"},
		},
		{
			name:      "properties 不是对象",
			raw:       `{"type":"object","properties":[1]}`,
			wantPaths: []string{"properties"},
		},
		{
			name:      "required 不是字符串数组",
			raw:       `{"type":"object","required":"a"}`,
			wantPaths: []string{"required"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, problems := ParseParams(json.RawMessage(tc.raw))
			if len(problems) != len(tc.wantPaths) {
				t.Fatalf("problem 数 = %d（%v），want %d（%v）",
					len(problems), problems, len(tc.wantPaths), tc.wantPaths)
			}
			for i, want := range tc.wantPaths {
				if problems[i].Path != want {
					t.Fatalf("problems[%d].Path = %q，want %q（全部：%v）", i, problems[i].Path, want, problems)
				}
				if strings.TrimSpace(problems[i].Msg) == "" {
					t.Fatalf("problems[%d] 的 Msg 不能为空（它是 400 message / 剪枝 detail 的人话）", i)
				}
			}
		})
	}
}

func TestParseParamsProblemsCarryKeyword(t *testing.T) {
	cases := []struct {
		raw     string
		path    string
		keyword string
	}{
		{`[]`, "", KeywordRoot},
		{`{"type":"string"}`, "type", KeywordType},
		{`{"properties":[1]}`, "properties", KeywordProperties},
		{`{"properties":{"a":3}}`, "properties.a", KeywordProperty},
		{`{"properties":{"a":{"pattern":"(["}}}`, "properties.a.pattern", KeywordPattern},
		{`{"properties":{"a":{"pattern":7}}}`, "properties.a.pattern", KeywordPattern},
		{`{"required":7}`, "required", KeywordRequired},
	}
	for _, tc := range cases {
		_, problems := ParseParams(json.RawMessage(tc.raw))
		if len(problems) == 0 {
			t.Fatalf("%s 应被判为不可用", tc.raw)
		}
		if problems[0].Path != tc.path || problems[0].Keyword != tc.keyword {
			t.Fatalf("%s 的首个问题 = {%q %q}，want {%q %q}（全部：%v）",
				tc.raw, problems[0].Path, problems[0].Keyword, tc.path, tc.keyword, problems)
		}
	}
}

func TestParseParamsProblemOrderIsStable(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{
		"z":{"type":"string","pattern":"(["},
		"b":3,
		"pattern":7,
		"a":{"pattern":"("},
		"m":{"type":"object","properties":{"pattern":3}}
	}}`)
	var first []Problem
	for i := 0; i < 32; i++ {
		_, problems := ParseParams(raw)
		if len(problems) == 0 {
			t.Fatal("坏 schema 必须报问题")
		}
		if i == 0 {
			first = problems
			continue
		}
		if !reflect.DeepEqual(problems, first) {
			t.Fatalf("第 %d 次的问题序列不同：\n第一次 %v\n这一次 %v", i, first, problems)
		}
	}
	if got := first[0].Path; got != "properties.a.pattern" {
		t.Fatalf("problems[0].Path = %q，want properties.a.pattern（属性名排序后的第一个坏属性）", got)
	}
}

func TestParseParamsDetailKeepsRuntimeWording(t *testing.T) {
	cases := []struct{ raw, says string }{
		{`{"type":"array"}`, "schema.type 必须是 object"},
		{`{"type":"object","properties":{"repo":{"pattern":"(["}}}`, "pattern"},
		{`{"type":"object","properties":{"repo":{"pattern":7}}}`, "pattern 必须是字符串"},
		{`{"type":"object","required":3}`, "required 必须是字符串数组"},
	}
	for _, tc := range cases {
		_, problems := ParseParams(json.RawMessage(tc.raw))
		if len(problems) == 0 {
			t.Fatalf("%s 应被判为不可用", tc.raw)
		}
		if !strings.Contains(problems[0].Msg, tc.says) {
			t.Fatalf("detail = %q，应包含 %q", problems[0].Msg, tc.says)
		}
	}
}

func TestParseParamsKeepsSubsetSemantics(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{
		"file":{"type":"string","default":"a.go"},
		"count":{"type":"integer"}
	},"required":["count"]}`)
	params, problems := ParseParams(raw)
	if len(problems) > 0 {
		t.Fatalf("合法 schema 不该有问题：%v", problems)
	}
	if params.Permissive {
		t.Fatal("有 properties 的 schema 不是 permissive")
	}
	if got := params.Properties["file"]; !got.HasDefault || got.Default != "a.go" {
		t.Fatalf("default 未解析出来：%+v", got)
	}
	if issue := params.Properties["count"].Check("count", "3"); issue == nil || issue.Field != "count" {
		t.Fatalf("类型不符应给出带路径的 issue，得到 %+v", issue)
	}
	pat, problems := ParseParams /* pattern 只约束字符串：非字符串值不因 pattern 被误判 */ (json.RawMessage(`{"type":"object","properties":{"p":{"pattern":"^x$"}}}`))
	if len(problems) > 0 {
		t.Fatalf("合法 pattern 不该有问题：%v", problems)
	}
	if issue := pat.Properties["p"].Check("p", 3); issue != nil {
		t.Fatalf("非字符串值不该被 pattern 判失败：%+v", issue)
	}
}

func TestParseParamsRejectsUnsupportedKeywordsAnywhere(t *testing.T) {
	cases := []struct{ name, raw, path string }{
		{"根节点", `{"type":"object","anyOf":[{"type":"object"}]}`, "anyOf"},
		{"属性", `{"type":"object","properties":{"a":{"type":"string","format":"uri"}}}`, "properties.a.format"},
		{"嵌套属性", `{"type":"object","properties":{"a":{"type":"object","properties":{"b":{"type":"number","multipleOf":2}}}}}`, "properties.a.properties.b.multipleOf"},
		{"items", `{"type":"object","properties":{"a":{"type":"array","items":{"type":"string","const":"x"}}}}`, "properties.a.items.const"},
		{"prefixItems", `{"type":"object","properties":{"a":{"type":"array","prefixItems":[{"type":"string","uniqueItems":true}]}}}`, "properties.a.prefixItems[0].uniqueItems"},
		{"additionalProperties 子 schema", `{"type":"object","additionalProperties":{"type":"string","then":{"type":"string"}}}`, "additionalProperties.then"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, problems := ParseParams(json.RawMessage(tc.raw))
			if len(problems) == 0 {
				t.Fatalf("%s 应被判为不可用", tc.raw)
			}
			if problems[0].Path != tc.path || problems[0].Keyword != KeywordUnsupported {
				t.Fatalf("首个问题 = {%q %q}，want {%q %q}（全部：%v）",
					problems[0].Path, problems[0].Keyword, tc.path, KeywordUnsupported, problems)
			}
			if !strings.Contains(problems[0].Msg, "enum") {
				t.Fatalf("400 message 应列出受支持的关键字（至少 enum），得到 %q", problems[0].Msg)
			}
		})
	}
}

func TestParseParamsAdditionalPropertiesFalse(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{`{"type":"object"}`, false},
		{`{"type":"object","additionalProperties":true}`, false},
		{`{"type":"object","additionalProperties":false}`, true},
	}
	for _, tc := range cases {
		params, problems := ParseParams(json.RawMessage(tc.raw))
		if len(problems) > 0 {
			t.Fatalf("%s 应是合法 schema：%v", tc.raw, problems)
		}
		if params.RejectsAdditional != tc.want {
			t.Fatalf("%s：RejectsAdditional = %v，want %v", tc.raw, params.RejectsAdditional, tc.want)
		}
	}
}

func TestParamPropertyCheckKeywords(t *testing.T) {
	params, problems := ParseParams(json.RawMessage(`{"type":"object","properties":{
		"n":{"type":"integer","minimum":1,"maximum":10},
		"env":{"type":"string","enum":["dev","prod"]},
		"name":{"type":"string","minLength":3,"maxLength":5},
		"tags":{"type":"array","maxItems":1,"items":{"type":"string"}},
		"meta":{"type":"object","additionalProperties":false,"properties":{"dir":{"type":"string"}}}
	}}`))
	if len(problems) > 0 {
		t.Fatalf("合法 schema 不应有问题：%v", problems)
	}
	bad := []struct {
		param string
		value any
	}{
		{"n", json.Number("100")},
		{"n", json.Number("0")},
		{"env", "staging"},
		{"name", "a"},
		{"name", "abcdefgh"},
		{"tags", []any{json.Number("1"), json.Number("2")}},
		{"tags", []any{json.Number("1")}},
		{"meta", map[string]any{"dir": "a", "typo": true}},
	}
	for _, tc := range bad {
		if issue := params.Properties[tc.param].CheckKeywords(tc.param, tc.value); issue == nil {
			t.Fatalf("参数 %s = %#v 应被关键字闸拒掉", tc.param, tc.value)
		} else if issue.Field != tc.param {
			t.Fatalf("参数 %s issue.Field = %q，want %q", tc.param, issue.Field, tc.param)
		}
	}
	good := []struct {
		param string
		value any
	}{
		{"n", json.Number("10")},
		{"env", "prod"},
		{"name", "abcd"},
		{"tags", []any{"x"}},
		{"meta", map[string]any{"dir": "a"}},
	}
	for _, tc := range good {
		if issue := params.Properties[tc.param].CheckKeywords(tc.param, tc.value); issue != nil {
			t.Fatalf("参数 %s = %#v 不该被拒：%+v", tc.param, tc.value, issue)
		}
	}
}
