package artifact

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/schema"
)

// artifactWithParams 造一份只关心 spec.params 的最小合法能力（run 只要 entrypoint + 文件）。
func artifactWithParams(params string) *Artifact {
	return &Artifact{
		APIVersion: APIVersion,
		Kind:       KindCapability,
		Metadata:   Metadata{Name: "pattern-check", Version: 1},
		Spec: Spec{
			Params: json.RawMessage(params),
			Run: Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\necho hi\n"},
			},
		},
	}
}

// TestValidateRejectsInvalidParamPattern 钉住「坏正则写入期就拒绝，不存进库等执行期炸」：
// 短码固定为 param_pattern_invalid，Fields 给出精确的 JSON 路径（前端/YAML 定位用）。
func TestValidateRejectsInvalidParamPattern(t *testing.T) {
	long := strings.Repeat("a", schema.MaxPatternLen+1)
	longJSON, err := json.Marshal(long)
	if err != nil {
		t.Fatalf("marshal long pattern: %v", err)
	}
	cases := []struct {
		name   string
		params string
		field  string
	}{
		{
			name:   "顶层",
			params: `{"type":"object","properties":{"project":{"type":"string","pattern":"["}}}`,
			field:  "spec.params.properties.project.pattern",
		},
		{
			name:   "嵌套",
			params: `{"type":"object","properties":{"meta":{"type":"object","properties":{"dir":{"pattern":"("}}}}}`,
			field:  "spec.params.properties.meta.properties.dir.pattern",
		},
		{
			name:   "数组元素",
			params: `{"type":"object","properties":{"list":{"type":"array","items":{"pattern":"["}}}}`,
			field:  "spec.params.properties.list.items.pattern",
		},
		{
			name:   "pattern 不是字符串",
			params: `{"type":"object","properties":{"project":{"pattern":3}}}`,
			field:  "spec.params.properties.project.pattern",
		},
		{
			name:   "超过长度上限",
			params: fmt.Sprintf(`{"type":"object","properties":{"project":{"pattern":%s}}}`, longJSON),
			field:  "spec.params.properties.project.pattern",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(artifactWithParams(tc.params))
			if err == nil {
				t.Fatal("坏 pattern 必须让写入期校验失败")
			}
			if got := Code(err); got != CodeParamPatternInvalid {
				t.Fatalf("Code = %q，want %q（error=%v）", got, CodeParamPatternInvalid, err)
			}
			var artifactErr *Error
			if !errors.As(err, &artifactErr) {
				t.Fatalf("错误不是 *Error: %T", err)
			}
			if len(artifactErr.Fields) == 0 || artifactErr.Fields[0] != tc.field {
				t.Fatalf("Fields = %v，want[0] = %q", artifactErr.Fields, tc.field)
			}
			if !strings.Contains(artifactErr.Problems[0], tc.field) {
				t.Fatalf("问题文本也必须带字段路径 %q，得到 %q", tc.field, artifactErr.Problems[0])
			}
		})
	}
}

func TestValidateAcceptsValidParamPattern(t *testing.T) {
	params := `{"type":"object","properties":{"project":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"}}}`
	if err := Validate(artifactWithParams(params)); err != nil {
		t.Fatalf("合法 pattern 不该让写入期校验失败: %v", err)
	}
}

// TestValidateDoesNotTreatDataPatternAsKeyword 钉住遍历边界：default 里的 "pattern"
// 是用户数据，不是 schema 关键字；递归它会把合法能力误拒。
func TestValidateDoesNotTreatDataPatternAsKeyword(t *testing.T) {
	params := `{"type":"object","properties":{"cfg":{"type":"object","default":{"pattern":"["}}}}`
	if err := Validate(artifactWithParams(params)); err != nil {
		t.Fatalf("default 数据里的 pattern 不该被当成关键字: %v", err)
	}
}

// TestParamPatternLenMatchesSharedSchemaLimit 钉住写入期与派发期/值校验的长度上限不漂移。
// 测试文件可以 import internal/schema（A5 只检查非测试依赖）：产品代码不能 import，
// 否则 cmd/anode 会间接依赖中枢包。
func TestParamPatternLenMatchesSharedSchemaLimit(t *testing.T) {
	if maxParamPatternLen != schema.MaxPatternLen {
		t.Fatalf("写入期上限 %d != internal/schema 上限 %d", maxParamPatternLen, schema.MaxPatternLen)
	}
}
