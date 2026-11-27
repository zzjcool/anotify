package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestSourceWriteRejectsUnusablePayloadSchema(t *testing.T) {
	env := newSourceResEnv(t)
	cases := []struct {
		name   string
		schema map[string]any
		field  string
		says   string
	}{
		{
			name:   "顶层 type 不是 object",
			schema: map[string]any{"type": "string"},
			field:  "spec.config.payloadSchema.type",
			says:   "schema.type 必须是 object",
		},
		{
			name: "坏 pattern（RE2 编不过）",
			schema: map[string]any{
				"type": "object", "properties": map[string]any{
					"repo": map[string]any{"type": "string", "pattern": "(["},
				},
			},
			field: "spec.config.payloadSchema.properties.repo.pattern",
			says:  "pattern",
		},
		{
			name: "pattern 不是字符串",
			schema: map[string]any{
				"type": "object", "properties": map[string]any{
					"repo": map[string]any{"type": "string", "pattern": 7},
				},
			},
			field: "spec.config.payloadSchema.properties.repo.pattern",
			says:  "pattern 必须是字符串",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := env.request(http.MethodPost, env.sourcesPath(), map[string]any{
				"apiVersion": "anotify/v1",
				"kind":       "Source",
				"metadata":   map[string]any{"name": "bad.schema"},
				"spec": map[string]any{
					"kind":   "manual",
					"config": map[string]any{"payloadSchema": tc.schema},
				},
			})
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("不可用的 payloadSchema 期望 400，得到 %d：%s", rr.Code, rr.Body.String())
			}
			doc := env.doc(rr)
			if got := doc["reason"]; got != "invalid_source_config" {
				t.Fatalf("reason = %v，want invalid_source_config（body=%s）", got, rr.Body.String())
			}
			cause := firstCause(t, doc)
			if got := cause["field"]; got != tc.field {
				t.Fatalf("causes[0].field = %v，want %s", got, tc.field)
			}
			if msg, _ := cause["message"].(string); !strings.Contains(msg, tc.says) {
				t.Fatalf("causes[0].message 应含 %q，得到 %q", tc.says, msg)
			}
			if got := env.count("sources"); got != 0 {
				t.Fatalf("被拒绝的创建不得落行，sources = %d", got)
			}
		})
	}
}

// TestSourceDraftPreviewPrunesUnusablePayloadSchema：dry-run 不跑写入期判据，只按 event_schema 剪枝。
func TestSourceDraftPreviewPrunesUnusablePayloadSchema(t *testing.T) {
	env := newSourceResEnv(t)
	rr := env.request(http.MethodPost, env.sourcesPath()+"/dry-run", map[string]any{
		"source": map[string]any{
			"apiVersion": "anotify/v1",
			"kind":       "Source",
			"metadata":   map[string]any{"name": "draft-bad-schema"},
			"spec": map[string]any{
				"kind":    "manual",
				"config":  map[string]any{"payloadSchema": map[string]any{"type": "string"}},
				"targets": []any{},
			},
		},
		"payload": map[string]any{"a": 1},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("草稿预览不校验写入期判据，应 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	pruned := sourceResAssertRawArray(t, rr, "pruned")
	if len(pruned) != 1 {
		t.Fatalf("应恰好 1 条剪枝，得到 %#v", pruned)
	}
	first := sourceResObj(t, pruned[0])
	if got := first["reason"]; got != "event_schema" {
		t.Fatalf("pruned[0].reason = %v，want event_schema", got)
	}
	detail, _ := first["detail"].(string)
	if !strings.Contains(detail, "payloadSchema 不可用") {
		t.Fatalf("pruned[0].detail 应点明 payloadSchema 不可用，得到 %q", detail)
	}
}

func TestSourceWriteAcceptsUsablePayloadSchema(t *testing.T) {
	env := newSourceResEnv(t)
	schemas := []map[string]any{
		{"type": "object", "properties": map[string]any{"repo": map[string]any{"type": "string", "pattern": "^[a-z]+$"}}},
		{"type": []any{"object", "null"}},
		{"required": []any{"action"}},
		{"type": "object", "properties": map[string]any{"repo": map[string]any{"type": "weird"}}},
	}
	for i, schema := range schemas {
		name := fmt.Sprintf("good.schema%d", i)
		rr := env.request(http.MethodPost, env.sourcesPath(), map[string]any{
			"apiVersion": "anotify/v1",
			"kind":       "Source",
			"metadata":   map[string]any{"name": name},
			"spec": map[string]any{
				"kind":   "manual",
				"config": map[string]any{"payloadSchema": schema},
			},
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("#%d 合法 payloadSchema 应 201，得到 %d：%s", i, rr.Code, rr.Body.String())
		}
	}
}
