package server

// #583：/yaml 是存储定义的重建（契约 api/resource.yaml：可整份贴回 playbook 包）—— 手工挑
// 字段会让 sourcespec.Target 的新字段静默掉队（#519 的 match / #522 的 outcomes 就没进过导出）。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
	"gopkg.in/yaml.v3"
)

func srcYAMLTestCapability(t *testing.T, env *sourceResEnv, name string, next []string) {
	t.Helper()
	err := env.db.CreateCapability(context.Background(), &store.CapabilityVersion{
		NamespaceID: env.nsID,
		Name:        name,
		Version:     1,
		Spec: artifact.Spec{
			Params: json.RawMessage(`{"type":"object"}`),
			Next:   next,
			Run:    artifact.Run{Entrypoint: "run.sh", Files: map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"}},
		},
	})
	if err != nil {
		t.Fatalf("create capability %q: %v", name, err)
	}
}

func srcYAMLWireTargets(t *testing.T, targets []sourcespec.Target) []any {
	t.Helper()
	raw, err := json.Marshal(targets)
	if err != nil {
		t.Fatalf("marshal targets: %v", err)
	}
	var out []any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode targets: %v", err)
	}
	return out
}

func srcYAMLTargetsOfJSON(t *testing.T, body []byte) string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // 数字按字面量比：200 写成 "200" 要在这里就暴露
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("响应不是 JSON 对象：%v（%s）", err, body)
	}
	return srcYAMLTargetsJSON(t, doc["spec"])
}

func srcYAMLTargetsJSON(t *testing.T, spec any) string {
	t.Helper()
	raw, err := json.Marshal(sourceResObj(t, spec)["targets"])
	if err != nil {
		t.Fatalf("marshal spec.targets: %v", err)
	}
	return string(raw)
}

func srcYAMLJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func TestSourceYAMLKeepsMatchAndOutcomes(t *testing.T) {
	env := newSourceResEnv(t)
	srcYAMLTestCapability(t, env, "router", []string{"apply-fix"})
	srcYAMLTestCapability(t, env, "apply-fix", nil)

	targets := []sourcespec.Target{
		{Capability: "notify"},
		{
			Capability: "notify",
			Params:     map[string]any{"text": "push", "n": 3},
			Match:      &sourcespec.Match{Types: []string{"push"}},
		},
		{
			Capability: "notify",
			Match: &sourcespec.Match{
				All: []sourcespec.MatchLeaf{{Field: "n", Op: ">", Value: 200}},
				Any: []sourcespec.MatchLeaf{{Field: "repo.private", Op: "exists", Negate: true}},
			},
		},
		{
			Capability: "router",
			Match:      &sourcespec.Match{All: []sourcespec.MatchLeaf{{Field: "n", Op: ">", Value: 200}}},
			Outcomes: map[string][]sourcespec.OutcomeAction{
				"fix":  {{Capability: "apply-fix", Params: map[string]any{"repo": "$(decision.repo)"}}},
				"drop": {}, // 空动作结局 = outcome_dropped，不是「没写」
			},
		},
	}
	env.createSource(
		map[string]any{"kind": "manual", "targets": srcYAMLWireTargets(t, targets)},
		map[string]any{"name": "yaml-full"},
	)

	got := env.request(http.MethodGet, env.sourcePath("yaml-full"), nil)
	if got.Code != http.StatusOK {
		t.Fatalf("GET 源期望 200，得到 %d：%s", got.Code, got.Body.String())
	}
	wantTargets := srcYAMLTargetsOfJSON(t, got.Body.Bytes())

	rr := env.request(http.MethodGet, env.sourcePath("yaml-full")+"/yaml", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /yaml 期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	text := sourceResString(t, env.doc(rr)["yaml"])
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("/yaml 正文必须是合法 YAML 文档：%v\n%s", err, text)
	}
	yamlTargets := srcYAMLTargetsJSON(t, doc["spec"])
	if yamlTargets != wantTargets {
		t.Fatalf("#583：/yaml 的 spec.targets 与 JSON 读投影不相等（match/outcomes 掉了就长这样）\n"+
			"--- JSON 读投影 ---\n%s\n--- /yaml 重建 ---\n%s\n--- 原文 ---\n%s",
			wantTargets, yamlTargets, text)
	}
	if !strings.Contains(yamlTargets, `"value":200`) {
		t.Fatalf("match 叶子里的 200 变成了非数字（贴回去落库字节就变了）：%s", yamlTargets)
	}
	if !strings.Contains(yamlTargets, `"drop":[]`) {
		t.Fatalf("空动作结局 drop: [] 丢了（它对应 outcome_dropped，有语义）：%s", yamlTargets)
	}

	rawDoc, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal /yaml 解析结果: %v", err)
	}
	var copyDoc map[string]any
	if err := json.Unmarshal(rawDoc, &copyDoc); err != nil {
		t.Fatalf("decode /yaml 解析结果: %v", err)
	}
	sourceResObj(t, copyDoc["metadata"])["name"] = "yaml-full-copy"
	pasted, err := yaml.Marshal(copyDoc)
	if err != nil {
		t.Fatalf("marshal 贴回件: %v", err)
	}
	rec := env.request(http.MethodPost, env.sourcesPath()+"/yaml", yamlBody(string(pasted)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("贴回 /yaml 期望 201，得到 %d：%s\n%s", rec.Code, rec.Body.String(), pasted)
	}
	back := env.request(http.MethodGet, env.sourcePath("yaml-full-copy"), nil)
	if back.Code != http.StatusOK {
		t.Fatalf("读取贴回件期望 200，得到 %d：%s", back.Code, back.Body.String())
	}
	if gotTargets := srcYAMLTargetsOfJSON(t, back.Body.Bytes()); gotTargets != wantTargets {
		t.Fatalf("贴回件的 targets 与原件不等：\n--- 原件 ---\n%s\n--- 贴回 ---\n%s", wantTargets, gotTargets)
	}

	// 下面钉行为面：match 生效 ⇒ 1 条任务 + 3 条 no_match。outcomes 只钉结构面（深度相等 +
	// drop: []），router 判决链由 route 包自己的测试付 —— 别把这理解成「行为面也钉到了」。
	payload := map[string]any{"payload": map[string]any{"n": 1}}
	origFire := env.request(http.MethodPost, env.sourcePath("yaml-full")+"/fire", payload)
	if origFire.Code != http.StatusOK {
		t.Fatalf("fire 原件期望 200，得到 %d：%s", origFire.Code, origFire.Body.String())
	}
	copyFire := env.request(http.MethodPost, env.sourcePath("yaml-full-copy")+"/fire", payload)
	if copyFire.Code != http.StatusOK {
		t.Fatalf("fire 贴回件期望 200，得到 %d：%s", copyFire.Code, copyFire.Body.String())
	}
	pruneRows := func(receipt map[string]any) string {
		rows := sourceResArr(t, receipt["pruned"])
		out := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			obj := sourceResObj(t, row)
			delete(obj, "detail") // 里面带事件 type（缺省=源名），两份源必然不同
			out = append(out, obj)
		}
		return srcYAMLJSON(t, out)
	}
	if origPruned, copyPruned := pruneRows(env.doc(origFire)), pruneRows(env.doc(copyFire)); origPruned != copyPruned {
		t.Fatalf("原件与贴回件的剪枝结果不同（过滤语义变了）：\n--- 原件 ---\n%s\n--- 贴回 ---\n%s",
			origPruned, copyPruned)
	}
	if tasks := sourceResAssertRawArray(t, origFire, "tasks"); len(tasks) != 1 {
		t.Fatalf("n=1 只该派给恒命中的 target（1 条任务），得到 %d：%s", len(tasks), origFire.Body.String())
	}
	// 贴回件也直接断言：只靠 pruned 相等去推，「既不派发也不进 pruned」的 target 会被放行。
	if tasks := sourceResAssertRawArray(t, copyFire, "tasks"); len(tasks) != 1 {
		t.Fatalf("贴回件的 n=1 同样只该派 1 条任务，得到 %d：%s", len(tasks), copyFire.Body.String())
	}
	pruned := sourceResAssertRawArray(t, origFire, "pruned")
	if len(pruned) != 3 {
		t.Fatalf("另外 3 条 target 应被 no_match 剪掉，得到 %v", pruned)
	}
	for _, row := range pruned {
		if reason := sourceResObj(t, row)["reason"]; reason != "no_match" {
			t.Fatalf("剪枝理由期望 no_match，得到 %v", reason)
		}
	}
}

func pbTestExportFilteredBundle(slug string) map[string]string {
	return map[string]string{
		"playbook.yaml": pbTestEntryDoc(slug, 1,
			[]string{"capabilities/router.yaml", "capabilities/fixer.yaml"}, []string{"sources/filtered.yaml"}),
		"capabilities/router.yaml": pbTestCapabilityDoc("{{values.project}}-router", []string{"{{values.project}}-fixer"}),
		"capabilities/fixer.yaml":  pbTestCapabilityDoc("{{values.project}}-fixer", nil),
		"sources/filtered.yaml": "metadata:\n  name: '{{values.project}}-filtered'\n" +
			"spec:\n  kind: webhook\n  enabled: true\n  config: {}\n  targets:\n" +
			"    - capability: '{{values.project}}-router'\n" +
			"      params:\n        repo: '{{values.project}}'\n" +
			"      match:\n        all:\n          - {field: n, op: '>', value: 200}\n" +
			"      outcomes:\n" +
			"        fix:\n          - capability: '{{values.project}}-fixer'\n" +
			"            params:\n              repo: '$(decision.repo)'\n" +
			"        drop: []\n",
	}
}

func TestPlaybookReleaseYAMLSourceKeepsMatchAndOutcomes(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.pbTestExportMux(t)
	slug := "pb-export-filtered"
	env.create(t, slug, pbTestExportFilteredBundle(slug))
	pbTestExportInstallOK(t, env, slug, "default")

	want := pbTestExportYAMLOf(t, env, "/v1/namespaces/"+env.nsID+"/sources/demo-filtered/yaml")
	var wantDoc map[string]any
	if err := yaml.Unmarshal([]byte(want), &wantDoc); err != nil {
		t.Fatalf("单对象 /yaml 不是合法 YAML：%v\n%s", err, want)
	}
	wantTargets := srcYAMLTargetsJSON(t, wantDoc["spec"])
	if !strings.Contains(wantTargets, `"match"`) || !strings.Contains(wantTargets, `"outcomes"`) {
		t.Fatalf("单对象 /yaml 的 targets 缺 match/outcomes：%s\n%s", wantTargets, want)
	}
	if !strings.Contains(wantTargets, `"drop":[]`) {
		t.Fatalf("单对象 /yaml 丢了空的 drop 结局：%s", wantTargets)
	}

	for _, view := range []string{"", "?as=current", "?as=installed"} {
		rec := env.do(t, "GET", env.path("/"+slug+"/releases/default/yaml")+view, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("导出 %q 应 200，得到 %d：%s", view, rec.Code, rec.Body.String())
		}
		var docs []string
		if view == "?as=installed" { // 该视图多一行「源无版本」头注释，切分器不同
			_, docs = pbTestInstalledDocs(t, rec.Body.String(), 0)
		} else {
			_, docs = pbTestExportDocs(t, rec.Body.String())
		}
		found := false
		for _, entry := range docs {
			if entry == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("导出 %q 的源文档与单对象 /yaml 不逐字节相同（#583：release 导出复用同一份重建）\n"+
				"--- 单对象 /yaml ---\n%s\n--- 导出 ---\n%s", view, want, rec.Body.String())
		}
	}
}
