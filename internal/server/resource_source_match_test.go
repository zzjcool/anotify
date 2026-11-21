package server

// targets[].match 组合子（#519）的 HTTP 面契约：写入期 400 带 causes[].field，读投影原样回显。

import (
	"net/http"
	"strings"
	"testing"
)

// matchWriteRejects：每条钉住 reason 与 causes[].field。
var matchWriteRejects = []struct {
	name   string
	match  any
	reason string
	field  string
	says   string
}{
	{
		name:   "未知键（单个 → 沿用文档层的 unknown_field）",
		match:  map[string]any{"type": []any{"issues.opened"}},
		reason: "unknown_field", field: "spec.targets[0].match.type", says: "all / any / types",
	},
	{
		name:   "叶子未知键",
		match:  map[string]any{"all": []any{map[string]any{"field": "a", "op": "exists", "values": 1}}},
		reason: "unknown_field", field: "spec.targets[0].match.all[0].values", says: "field / negate / op / value",
	},
	{
		name:   "空组合子",
		match:  map[string]any{"all": []any{}},
		reason: "invalid_source_config", field: "spec.targets[0].match.all", says: "空组合子",
	},
	{
		name:   "非法 op",
		match:  map[string]any{"any": []any{map[string]any{"field": "a", "op": "=~", "value": "x"}}},
		reason: "invalid_source_config", field: "spec.targets[0].match.any[0].op", says: "不认识的 op",
	},
	{
		name:   "RE2 编不过",
		match:  map[string]any{"all": []any{map[string]any{"field": "issue.title", "op": "matches", "value": "(unclosed"}}},
		reason: "invalid_source_config", field: "spec.targets[0].match.all[0].value", says: "RE2",
	},
	{
		name:   "路径语法非法",
		match:  map[string]any{"all": []any{map[string]any{"field": "issue.labels.0-name", "op": "exists"}}},
		reason: "invalid_source_config", field: "spec.targets[0].match.all[0].field", says: "路径段",
	},
	{
		name:   "value 形状与 op 不符",
		match:  map[string]any{"all": []any{map[string]any{"field": "issue.number", "op": ">", "value": "3"}}},
		reason: "invalid_source_config", field: "spec.targets[0].match.all[0].value", says: "必须是数字",
	},
}

// TestSourceMatchWriteRejects 钉住「写入期 400 + 定位到具体那行」。
func TestSourceMatchWriteRejects(t *testing.T) {
	env := newSourceResEnv(t)
	for _, tc := range matchWriteRejects {
		t.Run(tc.name, func(t *testing.T) {
			rr := env.request(http.MethodPost, env.sourcesPath(), map[string]any{
				"apiVersion": "anotify/v1",
				"kind":       "Source",
				"metadata":   map[string]any{"name": "bad.match"},
				"spec": map[string]any{
					"kind":    "manual",
					"targets": []any{map[string]any{"capability": "notify", "match": tc.match}},
				},
			})
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("期望 400，得到 %d：%s", rr.Code, rr.Body.String())
			}
			doc := env.doc(rr)
			if got := doc["reason"]; got != tc.reason {
				t.Fatalf("reason = %v，want %s（body=%s）", got, tc.reason, rr.Body.String())
			}
			cause := firstCause(t, doc)
			if got := cause["field"]; got != tc.field {
				t.Fatalf("causes[0].field = %v，want %s", got, tc.field)
			}
			if msg, _ := cause["message"].(string); !strings.Contains(msg, tc.says) {
				t.Fatalf("causes[0].message 应含 %q，得到 %q", tc.says, msg)
			}
			if got := env.count("sources"); got != 0 {
				t.Fatalf("被拒的创建不得落行，sources = %d", got)
			}
		})
	}
}

// TestSourceMatchRowLimitIs400：33 行 → 400，文案带上限 32。
func TestSourceMatchRowLimitIs400(t *testing.T) {
	env := newSourceResEnv(t)
	row := map[string]any{"field": "issue.title", "op": "exists"}
	rows := make([]any, 33)
	for i := range rows {
		rows[i] = row
	}
	rr := env.request(http.MethodPost, env.sourcesPath(), map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   map[string]any{"name": "too-many-rows"},
		"spec": map[string]any{
			"kind":    "manual",
			"targets": []any{map[string]any{"capability": "notify", "match": map[string]any{"all": rows}}},
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("33 行期望 400，得到 %d：%s", rr.Code, rr.Body.String())
	}
	doc := env.doc(rr)
	if msg, _ := doc["message"].(string); !strings.Contains(msg, "32") {
		t.Fatalf("message 应给出上限（32），得到 %q", msg)
	}
	cause := firstCause(t, doc)
	if got := cause["field"]; got != "spec.targets[0].match.all" {
		t.Fatalf("causes[0].field = %v，want spec.targets[0].match.all", got)
	}
}

// TestSourceMatchReadProjection 钉住读投影原样回显 + PATCH 坏 match 是 400 且不改库。
func TestSourceMatchReadProjection(t *testing.T) {
	env := newSourceResEnv(t)
	prGate := map[string]any{"field": "issue.pull_request", "op": "exists", "negate": true}
	labels := map[string]any{"field": "issue.labels.0.name", "op": "in", "value": []any{"bug", "regression"}}
	env.createSource(map[string]any{
		"kind": "manual",
		"targets": []any{map[string]any{
			"capability": "notify",
			"match":      map[string]any{"types": []any{"issues.labeled"}, "all": []any{prGate}, "any": []any{labels}},
		}},
	}, map[string]any{"name": "match-projection"})

	rr := env.request(http.MethodGet, env.sourcePath("match-projection"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("读取期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	targets := sourceResArr(t, sourceResObj(t, env.doc(rr)["spec"])["targets"])
	match := sourceResObj(t, sourceResObj(t, targets[0])["match"])
	if got := sourceResArr(t, match["types"])[0]; got != "issues.labeled" {
		t.Fatalf("types 没原样回显：%v", got)
	}
	all := sourceResObj(t, sourceResArr(t, match["all"])[0])
	if all["field"] != "issue.pull_request" || all["op"] != "exists" || all["negate"] != true {
		t.Fatalf("all[0] 没原样回显（negate 丢了就是行为变化）：%v", all)
	}
	anyRow := sourceResObj(t, sourceResArr(t, match["any"])[0])
	if anyRow["op"] != "in" || len(sourceResArr(t, anyRow["value"])) != 2 {
		t.Fatalf("any[0].value 没回显：%v", anyRow)
	}

	rv := sourceResString(t, sourceResObj(t, env.doc(rr)["metadata"])["resourceVersion"])
	bad := env.request(http.MethodPatch, env.sourcePath("match-projection"), map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   map[string]any{"name": "match-projection", "resourceVersion": rv},
		"spec": map[string]any{
			"targets": []any{map[string]any{
				"capability": "notify",
				"match":      map[string]any{"all": []any{map[string]any{"field": "a", "op": "nope", "value": 1}}},
			}},
		},
	})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("PATCH 坏 match 期望 400，得到 %d：%s", bad.Code, bad.Body.String())
	}
	if got := firstCause(t, env.doc(bad))["field"]; got != "spec.targets[0].match.all[0].op" {
		t.Fatalf("PATCH causes[0].field = %v", got)
	}
	after := env.request(http.MethodGet, env.sourcePath("match-projection"), nil)
	kept := sourceResObj(t, sourceResObj(t, sourceResArr(t, sourceResObj(t, env.doc(after)["spec"])["targets"])[0])["match"])
	if _, has := kept["types"]; !has {
		t.Fatalf("被拒的 PATCH 改动了库里的 match：%v", kept)
	}
}

// TestSourceMatchYAMLWrite 钉住 YAML 编辑面：all/any 能建，叶子未知键仍 400 且带位置。
func TestSourceMatchYAMLWrite(t *testing.T) {
	env := newSourceResEnv(t)
	header := "apiVersion: anotify/v1\nkind: Source\nmetadata:\n  name: match-yaml\nspec:\n  kind: manual\n  targets:\n    - capability: notify\n      match:\n"
	good := header +
		"        all:\n" +
		"          - {field: issue.pull_request, op: exists, negate: true}\n" +
		"        any:\n" +
		"          - {field: issue.labels.0.name, op: in, value: [bug]}\n"
	rec := env.request(http.MethodPost, env.sourcesPath()+"/yaml", yamlBody(good))
	if rec.Code != http.StatusCreated {
		t.Fatalf("YAML 建带组合子的源期望 201，得到 %d：%s", rec.Code, rec.Body.String())
	}
	targets := sourceResArr(t, sourceResObj(t, env.doc(rec)["spec"])["targets"])
	match := sourceResObj(t, sourceResObj(t, targets[0])["match"])
	if sourceResObj(t, sourceResArr(t, match["all"])[0])["negate"] != true {
		t.Fatalf("YAML 建出来的 negate 丢了：%v", match)
	}

	bad := header + "        all:\n          - {field: a, op: exists, values: 1}\n"
	rejected := env.request(http.MethodPost, env.sourcesPath()+"/yaml", yamlBody(bad))
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("叶子未知键期望 400，得到 %d：%s", rejected.Code, rejected.Body.String())
	}
	if got := firstCause(t, env.doc(rejected))["field"]; got != "spec.targets[0].match.all[0].values" {
		t.Fatalf("causes[0].field = %v，want spec.targets[0].match.all[0].values", got)
	}
}
