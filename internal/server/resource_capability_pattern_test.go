package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestCapabilityWriteRejectsBadParamPattern(t *testing.T) {
	env := newCapTestEnv(t)
	cases := []struct {
		name   string
		field  string
		params map[string]any
	}{
		{
			name:  "顶层",
			field: "spec.params.properties.project.pattern",
			params: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"project": map[string]any{"type": "string", "pattern": "["},
				},
			},
		},
		{
			name:  "嵌套",
			field: "spec.params.properties.meta.properties.dir.pattern",
			params: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"meta": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"dir": map[string]any{"type": "string", "pattern": "("},
						},
					},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := capabilityBodyWithParams(t, "pattern-write", "#!/bin/sh\necho hi\n", tc.params)
			rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "", ""), body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("坏 pattern 应 400，实际 %d body=%s", rec.Code, rec.Body.String())
			}
			doc := decodeCapJSON(t, rec)
			if got := capString(t, doc, "reason"); got != "param_pattern_invalid" {
				t.Fatalf("reason = %q，want param_pattern_invalid", got)
			}
			details := capObject(t, doc["details"], "details")
			causes := capArray(t, details["causes"], "details.causes")
			if len(causes) == 0 {
				t.Fatalf("details.causes 为空: %s", rec.Body.String())
			}
			first := capObject(t, causes[0], "causes[0]")
			if field := capString(t, first, "field"); field != tc.field {
				t.Fatalf("causes[0].field = %q，want %q", field, tc.field)
			}
			msg := capString(t, first, "message")
			if !strings.Contains(msg, "RE2") || !strings.Contains(msg, tc.field) {
				t.Fatalf("message 应同时给正则方言与字段路径，得到 %q", msg)
			}
		})
	}
}

func TestCapabilityTestParamPatternIs400WithExactField(t *testing.T) {
	env := newCapTestEnv(t)
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project": map[string]any{"type": "string", "pattern": "^[a-z][a-z0-9-]*$"},
			"meta": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"dir": map[string]any{"type": "string", "pattern": "^[a-z]+$"},
				},
			},
			"tree": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"root": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name": map[string]any{"type": "string", "pattern": "^[a-z]+$"},
						},
					},
				},
			},
			"count": map[string]any{"type": []string{"integer", "string"}, "pattern": "^n-[0-9]+$"},
		},
	}
	body := capabilityBodyWithParams(t, "pattern-test", "#!/bin/sh\necho hi\n", schema)
	if rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "", ""), body); rec.Code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", rec.Code, rec.Body.String())
	}

	bad := []struct {
		name   string
		params map[string]any
		field  string
		actual string
	}{
		{"顶层", map[string]any{"project": "a/b"}, "params.project", "a/b"},
		{"嵌套", map[string]any{"meta": map[string]any{"dir": "A"}}, "params.meta.dir", "A"},
		{"深嵌套", map[string]any{"tree": map[string]any{"root": map[string]any{"name": "9x"}}}, "params.tree.root.name", "9x"},
		{"字符串值的 pattern", map[string]any{"count": "x"}, "params.count", "x"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "pattern-test", "test"),
				map[string]any{"params": tc.params})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("pattern 不匹配应 400，实际 %d body=%s", rec.Code, rec.Body.String())
			}
			doc := decodeCapJSON(t, rec)
			if got := capString(t, doc, "reason"); got != "param_type" {
				t.Fatalf("reason = %q，want param_type", got)
			}
			details := capObject(t, doc["details"], "details")
			causes := capArray(t, details["causes"], "details.causes")
			if len(causes) == 0 {
				t.Fatalf("details.causes 为空: %s", rec.Body.String())
			}
			first := capObject(t, causes[0], "causes[0]")
			if field := capString(t, first, "field"); field != tc.field {
				t.Fatalf("causes[0].field = %q，want %q", field, tc.field)
			}
			msg := capString(t, first, "message")
			if !strings.Contains(msg, "pattern") || !strings.Contains(msg, tc.actual) {
				t.Fatalf("message 应给期望 pattern 与实际值 %q，得到 %q", tc.actual, msg)
			}
		})
	}

	var rejected int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE namespace_id = ?`, env.nsA).Scan(&rejected); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if rejected != 0 {
		t.Fatalf("参数被拒时不应建任务，实际 %d 条", rejected)
	}

	valid := []map[string]any{
		{"project": "a-b1", "meta": map[string]any{"dir": "abc"}, "tree": map[string]any{"root": map[string]any{"name": "abc"}}, "count": 7},
		{"count": "n-7"},
	}
	for i, params := range valid {
		rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "pattern-test", "test"),
			map[string]any{"params": params})
		if rec.Code != http.StatusOK {
			t.Fatalf("合法参数 #%d 应 200，实际 %d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	var accepted int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE namespace_id = ?`, env.nsA).Scan(&accepted); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if accepted != len(valid) {
		t.Fatalf("合法参数应各建一条任务，实际 %d 条（want %d）", accepted, len(valid))
	}
}
