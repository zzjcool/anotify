package server

// 本文件钉住 P4 的**请求形状**（Capability Spec v3 §5 的 P4 列）与 I1 红线：
//
//	① POST /v1/capabilities 的请求体是 `{name, version?, displayName?, spec, force?, links?}`
//	   —— 旧的扁平字段（paramsSchema / outputs / run / requires / sticky / lifecycle）全部删除
//	② `spec.handler` 非空一律 400 `handler_not_allowed`（I1：API 永不接受 handler，
//	   否则任何人能把任意能力挂到中枢执行）
//	③ spec 原样进库、原样从详情读回（多文件 + env 一个字节都不能丢）
//
// 为什么这些必须写在 server 包里：store 只认正文，它不知道「请求体是哪一层形状」；
// 而「API 不接受 handler」恰恰是**端点**的语义（契约 §3.1 的规则 2 在 HTTP 上的投影）。

import (
	"net/http"
	"strings"
	"testing"
)

// TestPostCapabilityRejectsHandler 是 I1 红线：spec.handler 非空 → 400 handler_not_allowed。
//
// 为什么连**白名单里**的 handler（noop / notify-phone）也必须拒：白名单只约束
// 「中枢能跑哪些内置动作」，而「谁有权声明一个 handler 能力」是另一件事 ——
// 一旦 HTTP 能写 handler，任何登录用户都能把一个能力挂到中枢进程里执行，I1 直接破掉。
// handler 只能由内置播种 / 回填 / 预案（服务端代码路径）产生。
func TestPostCapabilityRejectsHandler(t *testing.T) {
	env := newConsoleEnv(t)

	for _, handler := range []string{"noop", "notify-phone", "custom-handler"} {
		t.Run(handler, func(t *testing.T) {
			name := "p4.handler." + strings.ReplaceAll(handler, "-", "_")
			rr := env.doRaw(t, http.MethodPost, env.withNS("/v1/capabilities"), map[string]any{
				"name": name,
				"spec": map[string]any{
					"outputs": []string{},
					"handler": handler,
				},
			})
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("handler=%q status = %d, want 400（body=%s）", handler, rr.Code, rr.Body.String())
			}
			m := decode(t, rr)
			if m["code"] != "handler_not_allowed" {
				t.Fatalf("code = %v, want handler_not_allowed（body=%s）", m["code"], rr.Body.String())
			}
			msg, _ := m["error"].(string)
			// 文案必须告诉用户「正确做法是给 run」——否则这条 400 会被当成「JSON 写错了」
			for _, want := range []string{"handler", "run"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("400 的 error 里没有 %q: %q", want, msg)
				}
			}
			// 能力不落库（append-only：一条半成品永远撤不回来）
			if _, err := env.db.GetCapability(t.Context(), env.nsID, name, 1); err == nil {
				t.Fatal("被拒的 handler 能力留下了能力定义")
			}
		})
	}
}

// TestPostCapabilityBatchRejectsHandler 批量里任意一项带 handler → 整批 400，错误带下标。
//
// 单建与批量必须同一条口径：否则「单建拒、批量偷偷接受」会让 handler 从旁边的门进来。
func TestPostCapabilityBatchRejectsHandler(t *testing.T) {
	env := newConsoleEnv(t)

	rr := env.doRaw(t, http.MethodPost, "/v1/capabilities/batch", map[string]any{
		"namespaceId": env.nsID,
		"capabilities": []map[string]any{
			{"name": "p4.batch.ok", "spec": map[string]any{"run": minimalRunInput()}},
			{"name": "p4.batch.handler", "spec": map[string]any{"handler": "noop"}},
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（body=%s）", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	if m["code"] != "handler_not_allowed" {
		t.Fatalf("code = %v, want handler_not_allowed（body=%s）", m["code"], rr.Body.String())
	}
	msg, _ := m["error"].(string)
	if !strings.Contains(msg, "capabilities[1]") {
		t.Fatalf("错误里没有下标（前端无法定位第几张卡）: %q", msg)
	}
	if _, err := env.db.GetCapability(t.Context(), env.nsID, "p4.batch.ok", 1); err == nil {
		t.Fatal("整批被拒却写进了第一项")
	}
}

// TestPostCapabilityLegacyFlatBodyRejected 旧扁平请求体**不再被接受**（P4 是破坏性切换）。
//
// 这条用例的意义是「失败要响」：如果畸形请求体被静默当成「没有实现」，客户端会以为
// 自己建成功了；如果被静默兼容，旧字段与 spec 就会变成两份真相。所以钉住的是
// 「旧形状 → 400 impl_missing」，而不是「悄悄兼容」。
func TestPostCapabilityLegacyFlatBodyRejected(t *testing.T) {
	env := newConsoleEnv(t)

	rr := env.doRaw(t, http.MethodPost, env.withNS("/v1/capabilities"), map[string]any{
		"name":         "p4.legacy",
		"outputs":      []string{"out"},
		"paramsSchema": map[string]any{"type": "object"},
		"run":          minimalRunInput(),
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（扁平字段已删除）body=%s", rr.Code, rr.Body.String())
	}
	if got := decode(t, rr)["code"]; got != "impl_missing" {
		t.Fatalf("code = %v, want impl_missing（body=%s）", got, rr.Body.String())
	}
}

// TestPostCapabilitySpecRoundTrip 是请求形状的核心验收：spec 原样进库、原样读回。
//
// 三件事必须成立（否则就是前端那两条数据丢失 bug 的服务端版本）：
//   - **多文件**全部保留（不是只有 entrypoint）
//   - **env** 全部保留，且空值合法（`GH_TOKEN: ""` = 由节点环境提供）
//   - 列表只把 files 换成 {count, bytes}，其余字段（params/outputs/requires/
//     lifecycle/sticky/run 的其它键）一个不少
func TestPostCapabilitySpecRoundTrip(t *testing.T) {
	env := newConsoleEnv(t)

	rr := env.doRaw(t, http.MethodPost, env.withNS("/v1/capabilities"), map[string]any{
		"name":        "p4.roundtrip",
		"displayName": "  P4 往返  ",
		"spec": map[string]any{
			"params": map[string]any{
				"type":       "object",
				"properties": map[string]any{"repo": map[string]any{"type": "string"}},
			},
			"outputs":   []string{"needs-fix", "approved"},
			"requires":  []string{"git", "jq"},
			"lifecycle": map[string]any{"retryable": true, "maxAttempts": 2, "resultTtlSec": 3600},
			"sticky":    "worktree",
			"run": map[string]any{
				"entrypoint": "run.sh",
				"files": map[string]string{
					"run.sh":        "#!/bin/sh\nset -eu\nsh lib/helper.sh\n",
					"lib/helper.sh": "#!/bin/sh\necho helper\n",
				},
				"timeoutSec": 900,
				"env":        map[string]string{"GH_TOKEN": "", "LOG_LEVEL": "info"},
				"workDir":    "~/project",
			},
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	capObj, _ := m["capability"].(map[string]any)
	capID, _ := capObj["id"].(string)
	if capID == "" {
		t.Fatalf("响应里没有 capability.id: %v", m)
	}
	if d, _ := m["digest"].(string); !strings.HasPrefix(d, "sha256:") {
		t.Fatalf("digest = %q, want sha256:<64 hex>", d)
	}
	if capObj["displayName"] != "P4 往返" {
		t.Fatalf("displayName = %v, want 去空格后的值", capObj["displayName"])
	}

	// ① 详情：完整 spec（含 files 正文与 env）
	art := env.do(t, http.MethodGet, env.withNS("/v1/capabilities/"+capID+"/artifact"), nil)
	if art.Code != http.StatusOK {
		t.Fatalf("artifact status = %d, body=%s", art.Code, art.Body.String())
	}
	am := decode(t, art)
	files, _ := am["files"].(map[string]any)
	if len(files) != 2 {
		t.Fatalf("详情 files = %v, want 2 个文件（多文件不能被压扁）", files)
	}
	if got, _ := files["lib/helper.sh"].(string); !strings.Contains(got, "echo helper") {
		t.Fatalf("详情 files[lib/helper.sh] = %q, want 含 echo helper", got)
	}
	envMap, _ := am["env"].(map[string]any)
	if v, ok := envMap["GH_TOKEN"]; !ok || v != "" {
		t.Fatalf("详情 env = %v, want GH_TOKEN:\"\"（空值合法，不能被吞掉）", am["env"])
	}
	if envMap["LOG_LEVEL"] != "info" {
		t.Fatalf("详情 env = %v, want LOG_LEVEL=info", am["env"])
	}
	if am["workDir"] != "~/project" || am["timeoutSec"] != float64(900) {
		t.Fatalf("详情 run 字段 = %v/%v, want ~/project/900", am["workDir"], am["timeoutSec"])
	}

	spec, _ := am["spec"].(map[string]any)
	if spec == nil {
		t.Fatalf("详情缺 spec: %v", am)
	}
	if got, _ := spec["sticky"].(string); got != "worktree" {
		t.Fatalf("spec.sticky = %v, want worktree", spec["sticky"])
	}
	if outs, _ := spec["outputs"].([]any); len(outs) != 2 {
		t.Fatalf("spec.outputs = %v, want 2 个", spec["outputs"])
	}
	if reqs, _ := spec["requires"].([]any); len(reqs) != 2 {
		t.Fatalf("spec.requires = %v, want 2 个", spec["requires"])
	}
	lc, _ := spec["lifecycle"].(map[string]any)
	if lc["retryable"] != true || lc["maxAttempts"] != float64(2) {
		t.Fatalf("spec.lifecycle = %v, want retryable/maxAttempts", spec["lifecycle"])
	}
	if params, _ := spec["params"].(map[string]any); params["type"] != "object" {
		t.Fatalf("spec.params = %v, want type=object", spec["params"])
	}

	// ② 列表：files 换成摘要，其余字段原样
	listBody := env.do(t, http.MethodGet, env.withNS("/v1/capabilities"), nil).Body.String()
	var listed map[string]any
	for _, raw := range decode(t, env.do(t, http.MethodGet, env.withNS("/v1/capabilities"), nil))["capabilities"].([]any) {
		obj, _ := raw.(map[string]any)
		if obj["id"] == capID {
			listed = obj
		}
	}
	if listed == nil {
		t.Fatalf("列表里没有刚建的能力 %s", capID)
	}
	ls, _ := listed["spec"].(map[string]any)
	lrun, _ := ls["run"].(map[string]any)
	lfiles, _ := lrun["files"].(map[string]any)
	if lfiles["count"] != float64(2) {
		t.Fatalf("列表 spec.run.files = %v, want {count:2}", lrun["files"])
	}
	if _, hasBody := lfiles["run.sh"]; hasBody {
		t.Fatalf("列表里出现了文件正文: %v", lfiles)
	}
	if lenv, _ := lrun["env"].(map[string]any); lenv["GH_TOKEN"] != "" {
		t.Fatalf("列表 spec.run.env = %v, want GH_TOKEN:\"\"", lrun["env"])
	}
	if strings.Contains(listBody, "echo helper") {
		t.Fatal("列表响应里出现了文件正文（files 正文必须剔除）")
	}
}
