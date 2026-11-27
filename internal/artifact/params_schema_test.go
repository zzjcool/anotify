package artifact

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/schema"
)

func TestValidateRejectsUnusableParamSchema(t *testing.T) {
	cases := []struct {
		name   string
		params string
		field  string
	}{
		{
			name:   "顶层 type 不是 object",
			params: `{"type":"array","items":{"type":"string"}}`,
			field:  "spec.params.type",
		},
		{
			name:   "顶层 type 是字符串",
			params: `{"type":"string"}`,
			field:  "spec.params.type",
		},
		{
			name:   "属性不是对象",
			params: `{"type":"object","properties":{"a":3}}`,
			field:  "spec.params.properties.a",
		},
		{
			name:   "参数名就叫 pattern（属性值不是对象）",
			params: `{"type":"object","properties":{"pattern":3}}`,
			field:  "spec.params.properties.pattern",
		},
		{
			name:   "参数名以 .pattern 结尾",
			params: `{"type":"object","properties":{"x.pattern":3}}`,
			field:  "spec.params.properties.x.pattern",
		},
		{
			name:   "参数名就叫 pattern（漏写一层对象）",
			params: `{"type":"object","properties":{"pattern":"^x$"}}`,
			field:  "spec.params.properties.pattern",
		},
		{
			name:   "参数名就叫 pattern 且嵌套坏属性值",
			params: `{"type":"object","properties":{"pattern":{"properties":{"pattern":3}}}}`,
			field:  "spec.params.properties.pattern.properties.pattern",
		},
		{
			name:   "required 不是字符串数组",
			params: `{"type":"object","required":"a"}`,
			field:  "spec.params.required",
		},
		{
			name:   "未知 type 名（笔误）", // #580：静默放行会让类型判据不存在
			params: `{"type":"object","properties":{"a":{"type":"strin"}}}`,
			field:  "spec.params.properties.a.type",
		},
		{
			name:   "表外关键字 oneOf", // #580：收下但运行期不执行 = 假闸
			params: `{"type":"object","properties":{"a":{"oneOf":[{"type":"string"}]}}}`,
			field:  "spec.params.properties.a.oneOf",
		},
		{
			name:   "表外关键字 format",
			params: `{"type":"object","properties":{"a":{"type":"string","format":"uri"}}}`,
			field:  "spec.params.properties.a.format",
		},
		{
			name:   "enum 形状非法（字符串）", // #580 返工点：形状非法不能 201 收下后静默不生效
			params: `{"type":"object","properties":{"env":{"type":"string","enum":"dev"}}}`,
			field:  "spec.params.properties.env.enum",
		},
		{
			name:   "minLength 形状非法（负数）",
			params: `{"type":"object","properties":{"s":{"type":"string","minLength":-1}}}`,
			field:  "spec.params.properties.s.minLength",
		},
		{
			name:   "items 形状非法（元组写法）",
			params: `{"type":"object","properties":{"tags":{"type":"array","items":["string"]}}}`,
			field:  "spec.params.properties.tags.items",
		},
		{
			name:   "根 additionalProperties 子 schema 不会被应用",
			params: `{"type":"object","additionalProperties":{"type":"string"}}`,
			field:  "spec.params.additionalProperties",
		},
		{
			name:   "default 违反自身 schema",
			params: `{"type":"object","properties":{"n":{"type":"integer","default":"abc"}}}`,
			field:  "spec.params.properties.n.default",
		},
		{
			name:   "嵌套 default 不会被应用",
			params: `{"type":"object","properties":{"m":{"type":"object","properties":{"x":{"type":"string","default":"z"}}}}}`,
			field:  "spec.params.properties.m.properties.x.default",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(artifactWithParams(tc.params))
			if err == nil {
				t.Fatal("不可用的 params schema 必须让写入期校验失败")
			}
			if got := Code(err); got != CodeParamSchemaInvalid {
				t.Fatalf("Code = %q，want %q（error=%v）", got, CodeParamSchemaInvalid, err)
			}
			var artifactErr *Error
			if !errors.As(err, &artifactErr) {
				t.Fatalf("错误不是 *Error: %T", err)
			}
			if len(artifactErr.Fields) == 0 || artifactErr.Fields[0] != tc.field {
				t.Fatalf("Fields = %v，want[0] = %q", artifactErr.Fields, tc.field)
			}
		})
	}
}

func TestValidateParamSchemaVerdictMatchesRuntime(t *testing.T) {
	raws := []string{
		`{}`,
		`{"type":"object"}`,
		`{"type":["object","null"]}`,
		`{"type":"array"}`,
		`{"type":"string"}`,
		`{"type":"object","properties":{"a":{"type":"string","default":"x"}}}`,
		`{"type":"object","properties":{"a":{"type":"weird"}}}`,
		`{"type":"object","properties":{"a":{"type":"string","format":"date"}}}`,
		`{"type":"object","properties":{"a":{"enum":["x","y"],"minimum":1}}}`,
		`{"type":"object","properties":{"a":{"type":"string","minLength":2,"maxLength":4}}}`,
		`{"type":"object","items":{"type":"array","maxItems":2}}`,
		`{"type":"object","required":["a"],"additionalProperties":false,"properties":{"a":{"type":"string"}}}`,
		`{"type":"object","title":"输入","properties":{"a":{"type":"string","description":"名字"}}}`,
		`{"type":"object","properties":{"a":3}}`,
		`{"type":"object","properties":{"a":{"type":"string","pattern":"(["}}}`,
		`{"type":"object","properties":{"a":{"type":"string","pattern":7}}}`,
		`{"type":"object","properties":{"pattern":3}}`,
		`{"type":"object","properties":{"x.pattern":3}}`,
		`{"type":"object","properties":{"pattern":"^x$"}}`,
		`{"type":"object","properties":{"pattern":{"properties":{"pattern":3}}}}`,
		`{"type":"object","properties":{"pattern":{"type":"string","pattern":"(["}}}`,
		`{"type":"object","properties":{"pattern":{"pattern":"^x$"}}}`,
		`{"type":"object","required":7}`,
		`{"type":"object","required":["a"]}`,
	}
	for _, raw := range raws {
		t.Run(raw, func(t *testing.T) {
			_, problems := schema.ParseParams(json.RawMessage(raw))
			runtimeUsable := len(problems) == 0
			writeErr := Validate(artifactWithParams(raw))
			writeAccepts := writeErr == nil
			if writeAccepts != runtimeUsable {
				t.Fatalf("写入期接受=%v，运行期可用=%v（problems=%v，err=%v）",
					writeAccepts, runtimeUsable, problems, writeErr)
			}
		})
	}
}
