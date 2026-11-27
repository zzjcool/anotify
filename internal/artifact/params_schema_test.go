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
			name:   "required 不是字符串数组",
			params: `{"type":"object","required":"a"}`,
			field:  "spec.params.required",
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
		`{"type":"object","properties":{"a":3}}`,
		`{"type":"object","properties":{"a":{"type":"string","pattern":"(["}}}`,
		`{"type":"object","properties":{"a":{"type":"string","pattern":7}}}`,
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
