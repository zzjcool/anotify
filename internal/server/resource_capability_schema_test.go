package server

import (
	"encoding/json"
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
			name: "参数名就叫 pattern（属性值不是对象）",
			params: map[string]any{
				"type": "object", "properties": map[string]any{"pattern": 3},
			},
			field: "spec.params.properties.pattern",
			says:  "不是对象",
		},
		{
			name: "参数名以 .pattern 结尾",
			params: map[string]any{
				"type": "object", "properties": map[string]any{"x.pattern": 3},
			},
			field: "spec.params.properties.x.pattern",
			says:  "不是对象",
		},
		{
			name:   "required 不是字符串数组",
			params: map[string]any{"type": "object", "required": "a"},
			field:  "spec.params.required",
			says:   "required 必须是字符串数组",
		},
		{
			name: "未知 type 名", // #580：笔误不能 201 收下（运行期会静默失效）
			params: map[string]any{"type": "object", "properties": map[string]any{
				"x": map[string]any{"type": "strin"},
			}},
			field: "spec.params.properties.x.type",
			says:  "未知类型",
		},
		{
			name: "表外关键字 oneOf", // #580：收下但不执行 = 假闸
			params: map[string]any{"type": "object", "properties": map[string]any{
				"x": map[string]any{"oneOf": []any{map[string]any{"type": "string"}}},
			}},
			field: "spec.params.properties.x.oneOf",
			says:  "不支持 schema 关键字",
		},
		{
			name: "受控关键字形状非法（enum 是字符串）", // #580 返工点：形状非法同样不能「收下但静默不生效」
			params: map[string]any{"type": "object", "properties": map[string]any{
				"env": map[string]any{"type": "string", "enum": "dev"},
			}},
			field: "spec.params.properties.env.enum",
			says:  "enum 必须是数组",
		},
		{
			name:   "根级 additionalProperties 子 schema 不会被应用",
			params: map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
			field:  "spec.params.additionalProperties",
			says:   "只支持布尔",
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

// seedLegacyParams 绕过产品校验写坏 params：造「升级前接受、升级后被 #612 拒」的存量行。
func seedLegacyParams(t *testing.T, env *capTestEnv, ns, name string, params map[string]any) {
	t.Helper()
	var raw string
	if err := env.db.QueryRow(`SELECT spec FROM capability_versions
		WHERE namespace_id = ? AND name = ? AND deleted_at IS NULL
		ORDER BY version DESC LIMIT 1`, ns, name).Scan(&raw); err != nil {
		t.Fatalf("读 %s 的 spec: %v", name, err)
	}
	spec := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatalf("解 %s 的 spec: %v", name, err)
	}
	spec["params"] = params
	bad, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("重新序列化 %s 的 spec: %v", name, err)
	}
	if _, err := env.db.Exec(`UPDATE capability_versions SET spec = ?
		WHERE namespace_id = ? AND name = ? AND deleted_at IS NULL`, string(bad), ns, name); err != nil {
		t.Fatalf("写回 %s 的 spec: %v", name, err)
	}
}

// TestCapabilityPatchMetadataOnlyOnLegacyParamSchema：#612 判据只判新写入的内容，存量行必须还能改名 / 改 next。
func TestCapabilityPatchMetadataOnlyOnLegacyParamSchema(t *testing.T) {
	env := newCapTestEnv(t)
	created := env.createCapability(t, env.nsA, "legacy-params", "#!/bin/sh\necho hi\n")
	meta := capObject(t, created["metadata"], "metadata")
	token := capString(t, meta, "resourceVersion")
	badParams := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	seedLegacyParams(t, env, env.nsA, "legacy-params", badParams)

	rec := env.do(t, http.MethodPatch, env.capPath(env.nsA, "legacy-params", ""), map[string]any{
		"metadata": map[string]any{"displayName": "存量坏行", "resourceVersion": token},
		"spec":     map[string]any{"next": []string{"leaf@2"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("存量坏行的 metadata-only PATCH 应 200，得到 %d：%s", rec.Code, rec.Body.String())
	}
	doc := decodeCapJSON(t, rec)
	if got := capString(t, capObject(t, doc["metadata"], "metadata"), "displayName"); got != "存量坏行" {
		t.Fatalf("displayName = %q，want 存量坏行", got)
	}
	next := capArray(t, capObject(t, doc["spec"], "spec")["next"], "spec.next")
	if len(next) != 1 || next[0] != "leaf@2" {
		t.Fatalf("spec.next = %#v，want [leaf@2]", next)
	}
	got := capObject(t, capObject(t, doc["spec"], "spec")["params"], "spec.params")
	if got["type"] != "array" {
		t.Fatalf("PATCH 不得改动存量 params，实际 %#v", got)
	}
}

func TestCapabilityWriteAcceptsUsableParamSchema(t *testing.T) {
	env := newCapTestEnv(t)
	params := []map[string]any{
		{},
		{"type": "object", "properties": map[string]any{"diff": map[string]any{"type": "string"}}},
		{"type": []any{"object", "null"}},
		{"type": "object", "properties": map[string]any{"diff": map[string]any{"type": "string", "enum": []any{"a", "b"}, "minLength": 1}}},
		{"type": "object", "additionalProperties": false, "properties": map[string]any{"n": map[string]any{"type": "integer", "minimum": 1, "maximum": 10}}},
		{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "array", "maxItems": 2, "items": map[string]any{"type": "string"}}}},
		{"type": "object", "title": "输入", "properties": map[string]any{"a": map[string]any{"type": "string", "description": "名字"}}},
	}
	for i, p := range params {
		body := capabilityBodyWithParams(t, "good-schema", "#!/bin/sh\necho hi\n", p)
		rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "", ""), body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("#%d 合法 params schema 应 201，实际 %d body=%s", i, rec.Code, rec.Body.String())
		}
	}
}
