package server

// #583：源的 YAML 导出必须保留 `targets[].match`（#519 条件派）与 `targets[].outcomes`
// （#522 router 结局表）。
//
// 契约（api/resource.yaml「保证这份文本可整份贴回 playbook 包」）要求 /yaml 是存储定义的
// **重建**，不是手挑字段的子集：挑字段就会在 sourcespec.Target 长出新字段时静默掉队。
// 后果不是报错，而是贴回一个「没有过滤条件」的源 —— 本该被 no_match 剪掉的事件全部派发，
// 结局表消失。本文件同时钉两个出口：
//
//	① GET …/sources/{name}/yaml（单对象读面）
//	② GET …/playbooks/{slug}/releases/{release}/yaml 的 as=current / as=installed
//	   （复用同一份 srcYAMLDocument，契约要求与①逐字节相同）

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

// srcYAMLTestCapability 建一个可解析的出口能力（outcomes 的动作必须 ∈ target 的 spec.next）。
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

// srcYAMLWireTargets 把 sourcespec.Target 编成写请求体里的 targets 形状。
//
// 刻意**不**在测试里复述字段名：字面量掉队正是 #583 的根因，这里直接走 struct 的
// json tag，测试与产品共用同一份形状定义（少一处会各自走样的手工同步）。
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

// srcYAMLTargetsOfJSON 从 JSON 响应体里取 spec.targets 的**保型**正文。
//
// UseNumber 是断言的一半：数字按字面量比较，`200` 变成 `"200"`（yaml.v3 把 json.Number
// 当字符串编）会在这里暴露，而不是等到贴回后落库字节不等。
func srcYAMLTargetsOfJSON(t *testing.T, body []byte) string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("响应不是 JSON 对象：%v（%s）", err, body)
	}
	return srcYAMLTargetsJSON(t, doc["spec"])
}

// srcYAMLTargetsJSON 把 spec 段里的 targets 编回 JSON 文本（键序由 encoding/json 固定）。
func srcYAMLTargetsJSON(t *testing.T, spec any) string {
	t.Helper()
	raw, err := json.Marshal(sourceResObj(t, spec)["targets"])
	if err != nil {
		t.Fatalf("marshal spec.targets: %v", err)
	}
	return string(raw)
}

// srcYAMLJSON 是断言里的比较用编码（回执里的数组逐字比）。
func srcYAMLJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// TestSourceYAMLKeepsMatchAndOutcomes 是 #583 的单对象回归：
//
//	① GET /yaml 的 spec.targets 与 GET JSON 的 spec.targets **深度相等**（含 match/outcomes，
//	   数字 200 仍是数字，空动作结局 drop: [] 仍是 []）；
//	② 把 /yaml 整份贴回 POST …/sources/yaml（仅改名）后，targets 依旧相等；
//	③ 贴回件的 fire 回执与原件相同（过滤还在 ⇒ 同一份载荷得到同一份 pruned）。
func TestSourceYAMLKeepsMatchAndOutcomes(t *testing.T) {
	env := newSourceResEnv(t)
	srcYAMLTestCapability(t, env, "router", []string{"apply-fix"})
	srcYAMLTestCapability(t, env, "apply-fix", nil)

	// 四种 target 形状（source.md §4 + #519 + #522）：纯派 / types / all+any+negate（含数字）
	// / router 的结局表（含空动作结局 —— `drop: []` 对应 outcome_dropped，不是「没写」）。
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
				"drop": {},
			},
		},
	}
	env.createSource(
		map[string]any{"kind": "manual", "targets": srcYAMLWireTargets(t, targets)},
		map[string]any{"name": "yaml-full"},
	)

	// ① JSON 读投影是真源（srcEnvelopeOf 直接投 sourcespec.Target）。
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
	// 两个最容易漂的写法单独钉一遍（深度相等已经覆盖，这里给出可读的失败原因）。
	if !strings.Contains(yamlTargets, `"value":200`) {
		t.Fatalf("match 叶子里的 200 变成了非数字（贴回去落库字节就变了）：%s", yamlTargets)
	}
	if !strings.Contains(yamlTargets, `"drop":[]`) {
		t.Fatalf("空动作结局 drop: [] 丢了（它对应 outcome_dropped，有语义）：%s", yamlTargets)
	}

	// ② 契约承诺「可整份贴回」：只改名字，其余原样贴回 POST …/sources/yaml。
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

	// ③ 行为面：同一份载荷下，原件与贴回件的回执必须一致（match 丢了 tasks 会从 1 变 4）。
	payload := map[string]any{"payload": map[string]any{"n": 1}}
	origFire := env.request(http.MethodPost, env.sourcePath("yaml-full")+"/fire", payload)
	if origFire.Code != http.StatusOK {
		t.Fatalf("fire 原件期望 200，得到 %d：%s", origFire.Code, origFire.Body.String())
	}
	copyFire := env.request(http.MethodPost, env.sourcePath("yaml-full-copy")+"/fire", payload)
	if copyFire.Code != http.StatusOK {
		t.Fatalf("fire 贴回件期望 200，得到 %d：%s", copyFire.Code, copyFire.Body.String())
	}
	// detail 里带「事件 type」（缺省就是源名），两份源的它必然不同，比较前剔掉：
	// 要钉的是「哪些 target 为什么被剪」，不是事件名。
	pruneRows := func(receipt map[string]any) string {
		rows := sourceResArr(t, receipt["pruned"])
		out := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			obj := sourceResObj(t, row)
			delete(obj, "detail")
			out = append(out, obj)
		}
		return srcYAMLJSON(t, out)
	}
	if origPruned, copyPruned := pruneRows(env.doc(origFire)), pruneRows(env.doc(copyFire)); origPruned != copyPruned {
		t.Fatalf("原件与贴回件的剪枝结果不同（过滤语义变了）：\n--- 原件 ---\n%s\n--- 贴回 ---\n%s",
			origPruned, copyPruned)
	}
	// 只有「恒命中」的那条 target 派发；其余三条被 no_match 剪掉。
	if tasks := sourceResAssertRawArray(t, origFire, "tasks"); len(tasks) != 1 {
		t.Fatalf("n=1 只该派给恒命中的 target（1 条任务），得到 %d：%s", len(tasks), origFire.Body.String())
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

// pbTestExportFilteredBundle 是「源的 targets 带 match + outcomes」的包（#583 的第二个出口）。
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

// TestPlaybookReleaseYAMLSourceKeepsMatchAndOutcomes 钉住 release 导出的两个视图：
// 源文档里 match/outcomes 在，且与单对象 /yaml 逐字节相同（契约 api/resource.yaml:1376）。
func TestPlaybookReleaseYAMLSourceKeepsMatchAndOutcomes(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.pbTestExportMux(t)
	slug := "pb-export-filtered"
	env.create(t, slug, pbTestExportFilteredBundle(slug))
	pbTestExportInstallOK(t, env, slug, "default")

	// 单对象读面先自证带了过滤条件（否则下面比的是「两份都缺」这类无意义的相等）。
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
		// 两种视图的头注释不同（as=installed 多一行「源无版本」说明），各自用自己的切分器。
		var docs []string
		if view == "?as=installed" {
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
