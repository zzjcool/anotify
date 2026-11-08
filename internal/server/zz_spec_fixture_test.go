package server

// 本文件是**测试夹具**（P1-b1 机械适配 + P4 请求形状收口）：给 server 包的用例补一份
// 能力正文，并把**旧扁平形状**的请求体翻译成 P4 的 `spec` 形状。
//
// Capability Spec v3 起「能力 = 正文（spec）」，派生列由 store 在写入时从正文算出。
// 生产路径明令禁止从派生字段反推正文（契约 §4），但测试夹具的意图恰恰是
// 「给我一个 outputs=[a,b] 的能力」——在本包的用例里重复手写 spec 只会增加写错的
// 机会。已经带了实现（handler 或 run）的正文原样返回：显式写的 spec 优先。
//
// P4 把 HTTP 请求体切到 `spec` 形状（契约 §5）。本包的几百条用例关心的从来不是
// 「字段摆在哪一层」，而是闸门 / 幂等 / 过滤 / 边引用这些行为，所以**翻译收在这一层**：
// 造数仍然可以按旧扁平形状写（paramsSchema / outputs / run / requires / sticky /
// lifecycle），`withMinimalRun` 把她们搬进 `spec`。想钉新形状本身的用例（handler
// 拒绝、spec 往返）直接发 `spec`，翻译器看到 `spec` 就原样放过。

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/store"
)

func withSpec(c *store.CapabilityVersion) *store.CapabilityVersion {
	if c == nil {
		return nil
	}
	if c.Spec.Handler != "" || c.Spec.Run.Entrypoint != "" || len(c.Spec.Run.Files) > 0 {
		return c
	}
	var lc json.RawMessage
	if c.Lifecycle != (store.Lifecycle{}) {
		b, err := json.Marshal(c.Lifecycle)
		if err != nil {
			panic(err)
		}
		lc = b
	}
	c.Spec = store.HandlerSpec(artifact.HandlerNoop, c.ParamsSchema, c.Outputs, lc)
	c.Spec.Sticky = c.Sticky
	return c
}

// minimalRunInput 是 HTTP 造数用的**最小实现**（入口脚本 = 什么都没做，退出码 0）。
//
// 为什么每个建能力的用例都得带上它：P1-b2 起 POST /v1/capabilities **必须**带实现
// （缺 run → 400 `impl_missing`，契约 §5），所以「建个能力」不再是一份只有
// name/outputs 的请求体。这里给的是**请求体里真的带来的实现**（不是服务端替用户编的
// 占位——那正是 P1-b2 删掉的兜底）。
func minimalRunInput() map[string]any {
	return map[string]any{
		"entrypoint": "run.sh",
		"files":      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
	}
}

// minimalSpecInput 是 minimalRunInput 的 spec 内层（与上面的口径逐字一致）。
func minimalSpecInput() map[string]any {
	return map[string]any{"run": minimalRunInput()}
}

// flatSpecKeys 是 P4 从请求体**删掉**的那些扁平字段（契约 §5 的 P4 列）。
//
// 留在这里当一份可执行的清单：谁哪天又往请求结构里加回一个扁平字段，翻译器不会
// 搬它，用例会立刻红（而不是「翻译器顺手兼容了」，让旧形状偷偷活下来）。
var flatSpecKeys = []string{"paramsSchema", "outputs", "requires", "sticky", "lifecycle", "run", "balance", "source"}

// bodyToSpec 把**旧扁平形状**的请求体翻成 P4 的 `spec` 形状（原地改并返回）。
//
// 已有 `spec` 的请求原样返回（新形状的用例不走翻译）；扁平字段被搬进 spec 后删除。
// addRun 为 true 时补一份最小 run（P1-b2 起所有造数都要做的事）；为 false 时**不补**
// ——「缺实现 → 400」的用例要的正是没有实现的那一份请求。
func bodyToSpec(body map[string]any, addRun bool) map[string]any {
	if body == nil {
		body = map[string]any{}
	}
	if spec, ok := body["spec"].(map[string]any); ok {
		if addRun {
			if _, hasRun := spec["run"]; !hasRun {
				spec["run"] = minimalRunInput()
			}
		}
		for _, k := range flatSpecKeys {
			delete(body, k)
		}
		return body
	}
	if _, ok := body["spec"]; ok {
		// 显式给了 spec（哪怕类型不对）：原样放过，让被测端点自己判。
		return body
	}
	spec := map[string]any{}
	for _, k := range flatSpecKeys {
		v, ok := body[k]
		if !ok {
			continue
		}
		switch k {
		case "paramsSchema":
			spec["params"] = v
		case "source":
			// source 不再属于请求体（store 按 custom 落库）；静默丢掉。
		default:
			spec[k] = v
		}
		delete(body, k)
	}
	if addRun {
		if _, ok := spec["run"]; !ok {
			spec["run"] = minimalRunInput()
		}
	}
	body["spec"] = spec
	return body
}

// withMinimalRun 把请求体翻成 spec 形状，并保证里面有一份最小实现。
func withMinimalRun(body map[string]any) map[string]any {
	return bodyToSpec(body, true)
}

// adaptCapabilityBody 是 env.do 的请求体翻译入口：只对两个**建能力**端点生效。
//
// 批量端点逐元素翻译（不补 run：补不补是实现语义，由调用方通过
// postCapabilityBatch / withMinimalRun 显式决定）。
func adaptCapabilityBody(method, path string, body any) any {
	if method != http.MethodPost {
		return body
	}
	base := path
	if i := strings.IndexByte(base, '?'); i >= 0 {
		base = base[:i]
	}
	m, ok := body.(map[string]any)
	if !ok {
		return body
	}
	switch base {
	case "/v1/capabilities":
		return bodyToSpec(m, false)
	case "/v1/capabilities/batch":
		items, ok := m["capabilities"].([]map[string]any)
		if !ok {
			return m
		}
		for i, item := range items {
			items[i] = bodyToSpec(item, false)
		}
		return m
	default:
		return body
	}
}

// withMinimalRunReq 是 withMinimalRun 的**类型化**版本（用 capabilityListRequest 造数的用例）。
//
// 两份形状（map 与结构体）在用例里都有人用；各留一个入口，免得每个文件自己拼 run。
func withMinimalRunReq(c capabilityListRequest) capabilityListRequest {
	if c.Spec != nil {
		if c.Spec.Run.Entrypoint == "" && len(c.Spec.Run.Files) == 0 && c.Spec.Handler == "" {
			c.Spec.Run = artifact.Run{Entrypoint: "run.sh", Files: map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"}}
		}
		return c
	}
	spec := &artifact.Spec{}
	spec.Run = artifact.Run{Entrypoint: "run.sh", Files: map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"}}
	c.Spec = spec
	return c
}
