package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestCapabilityWriteRejectsUnusableParamSchema(t *testing.T) {
	env := newCapTestEnv(t)
	cases := []struct {
		name   string
		params map[string]any
		field  string
		says   string
	}{
		{
			name:   "顶层 type 不是 object",
			params: map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			field:  "spec.params.type",
			says:   "schema.type 必须是 object",
		},
		{
			name:   "顶层 type 是字符串",
			params: map[string]any{"type": "string"},
			field:  "spec.params.type",
			says:   "schema.type 必须是 object",
		},
		{
			name: "属性不是对象",
			params: map[string]any{
				"type": "object", "properties": map[string]any{"a": 3},
			},
			field: "spec.params.properties.a",
			says:  "不是对象",
		},
		{
			name:   "required 不是字符串数组",
			params: map[string]any{"type": "object", "required": "a"},
			field:  "spec.params.required",
			says:   "required 必须是字符串数组",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := capabilityBodyWithParams(t, "bad-schema", "#!/bin/sh\necho hi\n", tc.params)
			rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "", ""), body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("不可用的 params schema 应 400，实际 %d body=%s", rec.Code, rec.Body.String())
			}
			doc := decodeCapJSON(t, rec)
			if got := capString(t, doc, "reason"); got != "param_schema" {
				t.Fatalf("reason = %q，want param_schema", got)
			}
			first := capObject(t, capArray(t, capObject(t, doc["details"], "details")["causes"], "details.causes")[0], "causes[0]")
			if field := capString(t, first, "field"); field != tc.field {
				t.Fatalf("causes[0].field = %q，want %q", field, tc.field)
			}
			if msg := capString(t, first, "message"); !strings.Contains(msg, tc.says) {
				t.Fatalf("causes[0].message 应含 %q，得到 %q", tc.says, msg)
			}
		})
	}
}
func TestCapabilityWriteAcceptsUsableParamSchema(t *testing.T) {
	env := newCapTestEnv(t)
	params := []map[string]any{
		{},
		{"type": "object", "properties": map[string]any{"diff": map[string]any{"type": "string"}}},
		{"type": []any{"object", "null"}},
		{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "weird"}}},
	}
	for i, p := range params {
		body := capabilityBodyWithParams(t, "good-schema", "#!/bin/sh\necho hi\n", p)
		rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "", ""), body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("#%d 合法 params schema 应 201，实际 %d body=%s", i, rec.Code, rec.Body.String())
		}
	}
}
