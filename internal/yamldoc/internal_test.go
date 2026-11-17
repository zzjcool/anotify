package yamldoc

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// 这个文件是白盒单测：只覆盖「从公开 API 到不了」的分支。
//
// 别名分支就是典型：YAML 规定锚点必须在别名之前出现，而 rejectAt 按文档顺序走，
// 所以 `&a … *a` 永远先撞到锚点。别名分支是防御性的（防 yaml.v3 顺序变化），
// 只能直接调 nodeReject 覆盖。

func TestNodeReject(t *testing.T) {
	cases := []struct {
		name    string
		node    *yaml.Node
		wantNil bool
		has     string
	}{
		{"别名", &yaml.Node{Kind: yaml.AliasNode, Line: 4}, false, "不支持别名"},
		{"锚点", &yaml.Node{Kind: yaml.ScalarNode, Tag: yamlStrTag, Anchor: "a", Line: 2}, false, "不支持锚点"},
		{"自定义 tag", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!Foo", Line: 3}, false, "不支持的自定义标签「!Foo」"},
		{"!!binary 也算自定义 tag", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!binary", Line: 3}, false, "!!binary"},
		{"内置 str", &yaml.Node{Kind: yaml.ScalarNode, Tag: yamlStrTag, Line: 1}, true, ""},
		{"内置 timestamp 放行（裸标量会被自动解析成时间）", &yaml.Node{Kind: yaml.ScalarNode, Tag: yamlTimestampTag, Line: 1}, true, ""},
		{"映射节点", &yaml.Node{Kind: yaml.MappingNode, Tag: yamlMapTag, Line: 1}, true, ""},
		{"文档节点 tag 为空", &yaml.Node{Kind: yaml.DocumentNode, Line: 1}, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nodeReject(tc.node)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("nodeReject() = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("nodeReject() = nil, want 问题")
			}
			if got.Reason != reasonInvalidYAML {
				t.Errorf("Reason = %q, want %q", got.Reason, reasonInvalidYAML)
			}
			if got.Line != tc.node.Line {
				t.Errorf("Line = %d, want %d", got.Line, tc.node.Line)
			}
			if !strings.Contains(got.Message, tc.has) {
				t.Errorf("Message = %q，缺少 %q", got.Message, tc.has)
			}
		})
	}
}

// marshalCanonical 的失败分支只能白盒测：数字在 scalarValue 里就过了 isJSONNumber /
// formatFloat，`.inf` / `.nan` 在 badNumber 就被拒，所以从公开 API 到不了这里。
// 但它必须「不可能静默」：带行号，且**绝不**转发 encoding/json 的错误文本——
// 那段文本带标量字面量（reviewer 抓到的 `json: invalid number literal "01067"` 就是它）。
func TestMarshalProblem(t *testing.T) {
	// 先证明「为什么不转发 err」：编码器的错误文本里就有字面量。
	_, encErr := marshalCanonical(json.Number("01067"))
	if encErr == nil {
		t.Fatal("encoding/json 应当拒绝 json.Number(\"01067\")")
	}
	if !strings.Contains(encErr.Error(), "01067") {
		t.Fatalf("前提变了：编码错误文本里没有字面量：%v", encErr)
	}

	e := marshalProblem(&yaml.Node{Kind: yaml.MappingNode, Line: 4})
	if len(e.Problems) != 1 {
		t.Fatalf("问题数 = %d", len(e.Problems))
	}
	p := e.Problems[0]
	if p.Reason != reasonValueInvalid {
		t.Errorf("Reason = %q, want %q", p.Reason, reasonValueInvalid)
	}
	// 文档级问题：Path 为空是 Problem 对这个形状的定义（编码失败的是整棵树）。
	if p.Path != "" {
		t.Errorf("Path = %q, want \"\"（文档级）", p.Path)
	}
	if p.Line != 4 {
		t.Errorf("Line = %d, want 4（根节点行）", p.Line)
	}
	if p.Message == "" {
		t.Error("Message 为空")
	}
	for _, bad := range []string{"01067", "invalid number literal", encErr.Error()} {
		if strings.Contains(p.Message, bad) {
			t.Errorf("Message 回显了编码器细节 %q：%q", bad, p.Message)
		}
	}
	// nil 根节点不得 panic（防御性：调用方将来可能换源）。
	if got := marshalProblem(nil); len(got.Problems) != 1 || got.Problems[0].Line != 0 {
		t.Errorf("marshalProblem(nil) = %#v", got)
	}
}

func TestIsJSONNumber(t *testing.T) {
	ok := []string{"0", "-0", "1", "-42", "1700000000", "1.5", "1.50", "-0.25", "1e9", "1e+09", "1E-9", "1.5e3"}
	bad := []string{"", "-", "+5", "0x1f", "1_000", ".5", "1.", "01", "1e", "1e+", ".inf", "1 2"}
	for _, s := range ok {
		if !isJSONNumber(s) {
			t.Errorf("isJSONNumber(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if isJSONNumber(s) {
			t.Errorf("isJSONNumber(%q) = true, want false", s)
		}
	}
}
