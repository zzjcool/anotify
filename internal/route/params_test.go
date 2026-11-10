package route

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

func TestPrepareParamsPermissiveSchema(t *testing.T) {
	params, prune := PrepareParams(nil, map[string]any{"anything": "x", "n": 1})
	if prune != nil {
		t.Fatalf("空 schema 应该怎么传都过，得到剪枝 %+v", prune)
	}
	var got map[string]any
	if err := json.Unmarshal(params, &got); err != nil {
		t.Fatalf("params 不是合法 JSON: %v", err)
	}
	if got["anything"] != "x" {
		t.Fatalf("空 schema 应原样透传，得到 %v", got)
	}
}

func TestPrepareParamsDefaultsRequiredAndExtra(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{"file":{"type":"string"},"mode":{"type":"string","default":"fast"}},
		"required":["file"]
	}`)
	params, prune := PrepareParams(schema, map[string]any{"file": "a.go", "extra": "ignored"})
	if prune != nil {
		t.Fatalf("合法输入不该被剪枝: %+v", prune)
	}
	var got map[string]any
	_ = json.Unmarshal(params, &got)
	if got["file"] != "a.go" || got["mode"] != "fast" {
		t.Fatalf("default 未补齐或 extra 未忽略: %v", got)
	}
	if _, ok := got["extra"]; ok {
		t.Fatalf("payload 多余字段必须忽略（同名直取），得到 %v", got)
	}
}

func TestPrepareParamsMissingRequired(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"file":{"type":"string"}},"required":["file"]}`)
	if _, prune := PrepareParams(schema, map[string]any{}); prune == nil {
		t.Fatal("缺必填参数必须剪枝")
	} else if prune.Reason != protocol.ReasonParamMissing {
		t.Fatalf("剪枝 reason = %q，want param_missing", prune.Reason)
	}
}

func TestPrepareParamsTypeMismatch(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"]}`)
	_, prune := PrepareParams(schema, map[string]any{"count": "not-a-number"})
	if prune == nil || prune.Reason != protocol.ReasonParamType {
		t.Fatalf("类型不符应剪枝 param_type，得到 %+v", prune)
	}
}

func TestPrepareParamsBadSchema(t *testing.T) {
	_, prune := PrepareParams(json.RawMessage(`[]`), map[string]any{})
	if prune == nil || prune.Reason != protocol.ReasonParamSchema {
		t.Fatalf("坏 schema 应剪枝 param_schema，得到 %+v", prune)
	}
}

func TestValidatePayloadSchema(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"environment":{"type":"string"}},"required":["environment"]}`)
	if prune := ValidatePayload(schema, map[string]any{"environment": "prod"}); prune != nil {
		t.Fatalf("合法载荷不该剪枝: %+v", prune)
	}
	prune := ValidatePayload(schema, map[string]any{"environment": 3})
	if prune == nil || prune.Reason != protocol.ReasonEventSchema {
		t.Fatalf("载荷类型不符应剪枝 event_schema，得到 %+v", prune)
	}
}

func TestDeclaresTarget(t *testing.T) {
	next := []string{"fixer", "review@2"}
	cases := []struct {
		to   string
		want bool
	}{
		{"fixer", true},
		{"fixer@3", true}, // 裸名跟随 latest：任何版本都算声明
		{"review", true},  // 裸 to 命中钉版条目
		{"review@2", true},
		{"review@1", false}, // 钉版不符
		{"other", false},
	}
	for _, tc := range cases {
		if got := declaresTarget(next, tc.to); got != tc.want {
			t.Errorf("declaresTarget(%q) = %v，want %v", tc.to, got, tc.want)
		}
	}
}

func TestDecodePayloadRejectsNonObject(t *testing.T) {
	if _, ok := DecodePayload(json.RawMessage(`[1,2]`)); ok {
		t.Fatal("数组不是合法 params 对象")
	}
	if _, ok := DecodePayload(json.RawMessage(`"text"`)); ok {
		t.Fatal("字符串不是合法 params 对象")
	}
	if got, ok := DecodePayload(nil); !ok || len(got) != 0 {
		t.Fatalf("nil 载荷应解成空对象，得到 %v/%v", got, ok)
	}
}

// 编译期断言：参数子集使用的剪枝码必须来自共享封闭词表。
var _ = func() bool {
	for _, reason := range []string{
		protocol.ReasonParamMissing, protocol.ReasonParamSchema, protocol.ReasonParamType,
		protocol.ReasonEventSchema,
	} {
		if !protocol.IsPruneReason(reason) {
			panic("route: 非剪枝词表原因码 " + reason)
		}
	}
	_ = context.Background
	_ = artifact.DefaultMaxAttempts
	return true
}
