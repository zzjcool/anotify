package yamldoc

import (
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
