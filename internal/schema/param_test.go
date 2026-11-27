package schema

import (
	"encoding/json"
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
			name:      "未知类型名", // 运行期放行（JSON Schema 扩展）；写入期不许更严
			raw:       `{"type":"object","properties":{"x":{"type":"weird"}}}`,
			wantPaths: nil,
		},
		{name: "type 不是 object", raw: `{"type":"string"}`, wantPaths: []string{"type"}},
		{name: "type 数组不含 object", raw: `{"type":["string","null"]}`, wantPaths: []string{"type"}},
		{name: "type 是数字", raw: `{"type":7}`, wantPaths: []string{"type"}},
		{name: "不是 JSON 对象", raw: `[]`, wantPaths: []string{""}},
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
