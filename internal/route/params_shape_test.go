package route

import (
	"encoding/json"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
)

func TestPrepareParamsShapeInvalidSchemaIsParamSchema(t *testing.T) {
	schemas := []struct{ name, raw string }{
		{"enum 是字符串", `{"type":"object","properties":{"env":{"type":"string","enum":"dev"}}}`},
		{"minimum 是字符串", `{"type":"object","properties":{"n":{"type":"integer","minimum":"1"}}}`},
		{"minLength 是负数", `{"type":"object","properties":{"s":{"type":"string","minLength":-1}}}`},
		{"items 是元组数组", `{"type":"object","properties":{"tags":{"type":"array","items":["string"]}}}`},
		{"根 additionalProperties 是对象", `{"type":"object","additionalProperties":{"type":"string"}}`},
		{"default 违反自身 schema", `{"type":"object","properties":{"n":{"type":"integer","default":"abc"}}}`},
		{"嵌套 default 不会被应用", `{"type":"object","properties":{"m":{"type":"object","properties":{"x":{"type":"string","default":"z"}}}}}`},
	}
	for _, tc := range schemas {
		t.Run(tc.name, func(t *testing.T) {
			params, prune := PrepareParams(json.RawMessage(tc.raw), map[string]any{})
			if prune == nil {
				t.Fatalf("形状非法的 schema 必须剪枝，实际建出 %s", params)
			}
			if prune.Reason != protocol.ReasonParamSchema {
				t.Fatalf("reason = %q，want param_schema（detail=%q）", prune.Reason, prune.Detail)
			}
		})
	}
}

func TestPrepareParamsEnforcesRootKeywords(t *testing.T) {
	minProps := json.RawMessage(`{"type":"object","minProperties":2,
		"properties":{"a":{"type":"string"},"b":{"type":"string"}}}`)
	if _, prune := PrepareParams(minProps, map[string]any{"a": "x"}); prune == nil ||
		prune.Reason != protocol.ReasonParamType || prune.Field != "params" {
		t.Fatalf("minProperties 未满足应剪枝 param_type（field=params），得到 %+v", prune)
	}
	if _, prune := PrepareParams(minProps, map[string]any{"a": "x", "b": "y"}); prune != nil {
		t.Fatalf("满足 minProperties 的输入不该被拒：%+v", prune)
	}

	maxProps := json.RawMessage(`{"type":"object","maxProperties":0,"properties":{"a":{"type":"string"}}}`)
	if _, prune := PrepareParams(maxProps, map[string]any{"a": "x"}); prune == nil ||
		prune.Reason != protocol.ReasonParamType || prune.Field != "params" {
		t.Fatalf("maxProperties 超限应剪枝 param_type（field=params），得到 %+v", prune)
	}

	rootEnum := json.RawMessage(`{"type":"object","enum":[{}],"properties":{"a":{"type":"string"}}}`)
	if _, prune := PrepareParams(rootEnum, map[string]any{"a": "x"}); prune == nil ||
		prune.Reason != protocol.ReasonParamType || prune.Field != "params" {
		t.Fatalf("根 enum 应真的执行（field=params），得到 %+v", prune)
	}
	if _, prune := PrepareParams(rootEnum, map[string]any{}); prune != nil {
		t.Fatalf("命中根 enum 的输入不该被拒：%+v", prune)
	}
}

func TestPrepareParamsRejectsUndeclaredRequired(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","required":["ghost"],"properties":{"a":{"type":"string"}}}`)
	_, prune := PrepareParams(raw, map[string]any{"a": "x"})
	if prune == nil || prune.Reason != protocol.ReasonParamMissing || prune.Field != "params.ghost" {
		t.Fatalf("未声明的 required 应剪枝 param_missing（field=params.ghost），得到 %+v", prune)
	}
}

func TestValidatePayloadEnforcesRootKeywords(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","minProperties":2,
		"properties":{"env":{"type":"string"}}}`)
	if prune := ValidatePayload(raw, map[string]any{"env": "dev"}); prune == nil ||
		prune.Reason != protocol.ReasonEventSchema || prune.Field != "payload" {
		t.Fatalf("payloadSchema 的 minProperties 应剪枝 event_schema（field=payload），得到 %+v", prune)
	}
	if prune := ValidatePayload(raw, map[string]any{"env": "dev", "extra": 1}); prune != nil {
		t.Fatalf("满足 minProperties 的载荷不该被拒：%+v", prune)
	}
}
