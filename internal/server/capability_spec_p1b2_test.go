package server

// 本文件钉住 Capability Spec v3 §5 在 HTTP 层的三条行为（P1-b2）：
//
//	① 缺实现 → 400 `impl_missing`（单建与批量**每一项**同一口径）
//	② 列表里的 `spec` 剔除 files 正文（换成 {count, bytes}），旧 `run` 投影保留且同源
//	③ 详情（GET /v1/capabilities/{id}/artifact）给**完整** spec（含 files 正文）
//
// 为什么这些必须写在 server 包里而不是只靠 store 的单测：store 只认正文，它不知道
// 「HTTP 请求体有没有带 run」；而 impl_missing 恰恰是**端点**的语义（契约 §5 的
// 「唯一的行为收紧」）。

import (
	"net/http"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
)

// TestPostCapabilityImplMissing400 缺实现 → 400 impl_missing，且一行都不留下。
//
// 两条硬要求：
//   - 机器可读的 `code`（前端要据此高亮「实现」那一块，而不是弹一个通用错误）
//   - **能力不落库**：capability_versions 是 append-only，一条只建了一半的定义
//     永远改不掉、也永远跑不起来
func TestPostCapabilityImplMissing400(t *testing.T) {
	env := newConsoleEnv(t)

	rr := env.do(t, http.MethodPost, env.withNS("/v1/capabilities"), map[string]any{
		"name":    "no.impl",
		"outputs": []string{"done"},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（body=%s）", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	if m["code"] != "impl_missing" {
		t.Fatalf("code = %v, want impl_missing（body=%s）", m["code"], rr.Body.String())
	}
	msg, _ := m["error"].(string)
	// 文案必须告诉用户下一步做什么（这条 400 很容易被当成「JSON 写错了」）
	for _, want := range []string{"必须有实现", "run", "handler"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("400 的 error 里没有 %q: %q", want, msg)
		}
	}
	if _, err := env.db.GetCapability(t.Context(), env.nsID, "no.impl", 1); err == nil {
		t.Fatal("被拒的请求留下了能力定义")
	}
}

// TestPostCapabilityBatchImplMissing400 批量里**任意一项**缺实现 → 整批 400，错误带下标。
//
// 批量与单建必须同口径：否则「单建不行、批量偷偷给我一个 noop 占位」会让同一个
// 表单在两个入口产生两种落库结果，而列表页看起来一模一样。
func TestPostCapabilityBatchImplMissing400(t *testing.T) {
	env := newConsoleEnv(t)

	// 直连 env.do（不走 postCapabilityBatch：那个夹具会替每个元素补 run，
	// 而这条用例要测的正是「不补 run 会怎样」）。
	rr := env.do(t, http.MethodPost, "/v1/capabilities/batch", map[string]any{
		"namespaceId": env.nsID,
		"capabilities": []map[string]any{
			{"name": "batch.hasrun", "outputs": []string{"done"}, "run": minimalRunInput()},
			// 第二项没实现：整批不能成（第一项也不许落库）
			{"name": "batch.norun", "outputs": []string{"done"}},
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（body=%s）", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	if m["code"] != "impl_missing" {
		t.Fatalf("code = %v, want impl_missing（body=%s）", m["code"], rr.Body.String())
	}
	msg, _ := m["error"].(string)
	if !strings.Contains(msg, "capabilities[1]") {
		t.Fatalf("错误里没有下标（前端无法定位第几条）: %q", msg)
	}
	if _, err := env.db.GetCapability(t.Context(), env.nsID, "batch.hasrun", 1); err == nil {
		t.Fatal("整批被拒却写进了第一项")
	}
}

// TestCapabilityListSpecStripsFiles 列表里的 spec：**没有 files 正文**，只有 {count, bytes}；
// 旧的 `run` 投影仍然在，且与 spec 同源。
func TestCapabilityListSpecStripsFiles(t *testing.T) {
	env := newConsoleEnv(t)
	capObj := createCapWithRun(t, env, env.nsID, "spec.strip", []string{"echo"})
	capID, _ := capObj["id"].(string)

	rr := env.do(t, http.MethodGet, env.withNS("/v1/capabilities"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", rr.Code, rr.Body.String())
	}
	listBody := rr.Body.String()
	var got map[string]any
	for _, raw := range decode(t, rr)["capabilities"].([]any) {
		obj, _ := raw.(map[string]any)
		if obj["id"] == capID {
			got = obj
		}
	}
	if got == nil {
		t.Fatalf("列表里没有刚建的能力 %s", capID)
	}

	// ① 正文在，形状对齐 Capability Spec v3 §3
	spec, _ := got["spec"].(map[string]any)
	if spec == nil {
		t.Fatalf("列表项缺 spec: %v", got)
	}
	if spec["outputs"] == nil {
		t.Fatalf("spec.outputs = nil，want []（I9）: %v", spec)
	}
	if outs, _ := spec["outputs"].([]any); len(outs) != 1 || outs[0] != "echo" {
		t.Fatalf("spec.outputs = %v, want [echo]", spec["outputs"])
	}
	if req, _ := spec["requires"].([]any); len(req) != 1 || req[0] != "curl" {
		t.Fatalf("spec.requires = %v, want [curl]", spec["requires"])
	}
	// run 形式不该有 handler（两组恰好给一组，契约 §3.1）
	if h, has := spec["handler"]; has {
		t.Fatalf("run 形式的 spec 里不该有 handler: %v", h)
	}

	// ② files 必须是**摘要**：只有 count / bytes，一个正文都没有
	runSpec, _ := spec["run"].(map[string]any)
	if runSpec == nil {
		t.Fatalf("spec.run = %v, want 对象", spec["run"])
	}
	files, _ := runSpec["files"].(map[string]any)
	if files == nil {
		t.Fatalf("spec.run.files = %v, want {count, bytes} 摘要", runSpec["files"])
	}
	for k := range files {
		if k != "count" && k != "bytes" {
			t.Fatalf("spec.run.files 里多出了 %q（列表不许带文件正文）: %v", k, files)
		}
	}
	if c, _ := files["count"].(float64); int(c) != 2 {
		t.Fatalf("spec.run.files.count = %v, want 2", files["count"])
	}
	wantBytes := len(artifactScript) + len("#!/bin/sh\nsee_you_later\n")
	if b, _ := files["bytes"].(float64); int(b) != wantBytes {
		t.Fatalf("spec.run.files.bytes = %v, want %d", files["bytes"], wantBytes)
	}
	// 正文真的没漏进来：这个 marker 只在 lib/x.sh 里有，且不含换行（不会被转义藏起来）
	if strings.Contains(listBody, "see_you_later") {
		t.Fatal("列表响应里出现了文件正文（files 正文必须剔除）")
	}

	// ③ 旧投影保留（P4 才删），且与 spec 同源
	runOld, _ := got["run"].(map[string]any)
	if runOld == nil {
		t.Fatalf("旧 run 投影被删了（P4 才删）: %v", got["run"])
	}
	if runOld["entrypoint"] != runSpec["entrypoint"] {
		t.Fatalf("run.entrypoint = %v 与 spec.run.entrypoint = %v 不同源",
			runOld["entrypoint"], runSpec["entrypoint"])
	}
	if runOld["timeoutSec"] != runSpec["timeoutSec"] {
		t.Fatalf("run.timeoutSec = %v 与 spec.run.timeoutSec = %v 不同源",
			runOld["timeoutSec"], runSpec["timeoutSec"])
	}
	if runOld["workDir"] != "/srv/work" || runSpec["workDir"] != "/srv/work" {
		t.Fatalf("workDir 投影 = %v / %v, want /srv/work", runOld["workDir"], runSpec["workDir"])
	}

	// ④ env 是正文的一部分，摘要里保留（它决定「跑起来时环境里有什么」）。
	// 空 env 在线上形状里是**省略**（artifact.Run.Env 带 omitempty），所以这里用一份
	// 真的带 env 的能力来断言，而不是去争论 `{}` 与缺键的区别。
	withEnv := createCapWithEnv(t, env, "spec.strip.env", map[string]string{"LOG_LEVEL": "info"})
	list2 := env.do(t, http.MethodGet, env.withNS("/v1/capabilities"), nil)
	if list2.Code != http.StatusOK {
		t.Fatalf("list status = %d", list2.Code)
	}
	for _, raw := range decode(t, list2)["capabilities"].([]any) {
		obj, _ := raw.(map[string]any)
		if obj["id"] != withEnv {
			continue
		}
		s2, _ := obj["spec"].(map[string]any)
		r2, _ := s2["run"].(map[string]any)
		e2, _ := r2["env"].(map[string]any)
		if e2["LOG_LEVEL"] != "info" {
			t.Fatalf("spec.run.env = %v, want {LOG_LEVEL: info}", r2["env"])
		}
		return
	}
	t.Fatalf("列表里没有带 env 的能力 %s", withEnv)
}

// TestCapabilityListSpecHandlerForm handler 形式的能力：列表里 `handler` 有值、没有 `run`。
//
// 这是「一个能力永远有实现」（契约 R2）在列表上的可读性：前端据此把它标成
// 「由中枢执行」而不是「还没实现」。
func TestCapabilityListSpecHandlerForm(t *testing.T) {
	env := newConsoleEnv(t)
	noop := env.seedCapability(t, "handler.form", 1, []string{"out"})

	rr := env.do(t, http.MethodGet, env.withNS("/v1/capabilities"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d", rr.Code)
	}
	var got map[string]any
	for _, raw := range decode(t, rr)["capabilities"].([]any) {
		obj, _ := raw.(map[string]any)
		if obj["id"] == noop.ID {
			got = obj
		}
	}
	if got == nil {
		t.Fatalf("列表里没有 %s", noop.ID)
	}
	spec, _ := got["spec"].(map[string]any)
	if spec == nil {
		t.Fatalf("列表项缺 spec: %v", got)
	}
	if spec["handler"] != artifact.HandlerNoop {
		t.Fatalf("spec.handler = %v, want %q", spec["handler"], artifact.HandlerNoop)
	}
	if _, has := spec["run"]; has {
		t.Fatalf("handler 形式不该有 run 键: %v", spec["run"])
	}
	// 旧投影同样给 null（前端显示「—」，而不是藏掉这两行）
	if v, has := got["run"]; has && v != nil {
		t.Fatalf("handler 形式的旧 run 投影 = %v, want null/缺省", v)
	}
}

// TestCapabilityArtifactReturnsFullSpec 详情端点给**完整** spec（含 files 正文）。
//
// 与列表用例成对：同一份能力，列表里看不到 "see_you_later"，详情里必须看到。
// 「谁能看到正文」是这一对用例唯一在证明的事。
func TestCapabilityArtifactReturnsFullSpec(t *testing.T) {
	env := newConsoleEnv(t)
	capID := createCapWithEnv(t, env, "spec.detail", map[string]string{"LOG_LEVEL": "info"})

	rr := env.do(t, http.MethodGet, env.withNS("/v1/capabilities/"+capID+"/artifact"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	spec, _ := m["spec"].(map[string]any)
	if spec == nil {
		t.Fatalf("详情缺完整 spec: %v", m)
	}
	runSpec, _ := spec["run"].(map[string]any)
	files, _ := runSpec["files"].(map[string]any)
	if got, _ := files["lib/x.sh"].(string); !strings.Contains(got, "see_you_later") {
		t.Fatalf("详情里的 spec.run.files 没有文件正文: %v", files)
	}
	if got, _ := files["run.sh"].(string); got != artifactScript {
		t.Fatalf("spec.run.files[run.sh] = %q, want %q", got, artifactScript)
	}
	// env 也在（与 spec.run.env 同一份数据）
	envFlat, _ := m["env"].(map[string]any)
	specEnv, _ := runSpec["env"].(map[string]any)
	if envFlat == nil || specEnv == nil {
		t.Fatalf("env 投影缺失：扁平 %v / spec %v", m["env"], runSpec["env"])
	}
}

// createCapWithEnv 建一个带 run + env 的能力，返回 capabilityId。
//
// 与 capability_artifact_test.go 的 createCapWithRun 的差别只有一处：env 可给。
// 「env 进正文 / 进摘要 / 进详情」是 P1-b2 的三条断言，需要一份真的声明了 env 的样本。
func createCapWithEnv(t *testing.T, env *consoleEnv, name string, runEnv map[string]string) string {
	t.Helper()
	rr := env.do(t, http.MethodPost, env.withNS("/v1/capabilities"), map[string]any{
		"name":         name,
		"outputs":      []string{"echo"},
		"paramsSchema": map[string]any{"type": "object"},
		"run": map[string]any{
			"entrypoint": "run.sh",
			"files": map[string]string{
				"run.sh":   artifactScript,
				"lib/x.sh": "#!/bin/sh\nsee_you_later\n",
			},
			"timeoutSec": 42,
			"workDir":    "/srv/work",
			"env":        runEnv,
		},
		"requires": []string{"curl"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("建能力 %s 失败：%d %s", name, rr.Code, rr.Body.String())
	}
	capObj, _ := decode(t, rr)["capability"].(map[string]any)
	id, _ := capObj["id"].(string)
	if id == "" {
		t.Fatalf("响应缺 capability.id: %s", rr.Body.String())
	}
	return id
}
