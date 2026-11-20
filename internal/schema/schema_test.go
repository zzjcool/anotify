package schema

import (
	"strings"
	"testing"
)

// TestCompilePatternCacheAndLimits 钉住编译入口的两条边界：同一个 pattern 第二次走缓存
// （返回同一个 *regexp.Regexp），坏正则与超长 pattern 都在编译期拒绝。
func TestCompilePatternCacheAndLimits(t *testing.T) {
	first, err := CompilePattern("^[a-z][a-z0-9-]*$")
	if err != nil {
		t.Fatalf("合法 pattern 编译失败: %v", err)
	}
	second, err := CompilePattern("^[a-z][a-z0-9-]*$")
	if err != nil {
		t.Fatalf("第二次编译失败: %v", err)
	}
	if first != second {
		t.Fatal("同一个 pattern 应命中编译缓存（返回同一个 *regexp.Regexp）")
	}

	if _, err := CompilePattern("["); err == nil {
		t.Fatal("坏正则必须在编译期被拒")
	}
	long := strings.Repeat("a", MaxPatternLen+1)
	if _, err := CompilePattern(long); err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("超长 pattern 应被长度上限拒绝，得到 %v", err)
	}
}

// TestValuePatternOnlyConstrainsStrings 钉住 JSON Schema 语义：pattern 只作用于字符串；
// 非字符串值由 type 负责，pattern 不越权。
func TestValuePatternOnlyConstrainsStrings(t *testing.T) {
	sch := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project": map[string]any{"type": "string", "pattern": "^[a-z]+$"},
			"count":   map[string]any{"pattern": "^n-[0-9]+$"},
		},
	}
	if msg := Value(sch, map[string]any{"project": "abc", "count": 7}, "params"); msg != "" {
		t.Fatalf("合法值与非字符串值都应通过，得到 %q", msg)
	}
	msg := Value(sch, map[string]any{"project": "a/b"}, "params")
	if !strings.Contains(msg, "pattern") || !strings.Contains(msg, "a/b") {
		t.Fatalf("不匹配时应同时给出 pattern 与实际值，得到 %q", msg)
	}
}
