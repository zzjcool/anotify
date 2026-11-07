package server

// 本文件是**测试夹具**（P1-b1 机械适配）：给 server 包的用例补一份能力正文。
//
// Capability Spec v3 起「能力 = 正文（spec）」，派生列由 store 在写入时从正文算出。
// 生产路径明令禁止从派生字段反推正文（契约 §4），但测试夹具的意图恰恰是
// 「给我一个 outputs=[a,b] 的能力」——在本包的用例里重复手写 spec 只会增加写错的
// 机会。已经带了实现（handler 或 run）的正文原样返回：显式写的 spec 优先。

import (
	"encoding/json"

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

// withMinimalRun 给请求体补一份最小 run（已有 run 的原样返回，不覆盖显式实现）。
//
// 用它可以把改动收在「请求体外面那一层」：用例本身关心的东西（闸门 / 幂等 / 过滤）
// 一个字都不用动。
func withMinimalRun(body map[string]any) map[string]any {
	if body == nil {
		body = map[string]any{}
	}
	if _, has := body["run"]; has {
		return body
	}
	body["run"] = minimalRunInput()
	return body
}

// withMinimalRunReq 是 withMinimalRun 的**类型化**版本（用 capabilityListRequest 造数的用例）。
//
// 两份形状（map 与结构体）在用例里都有人用；各留一个入口，免得每个文件自己拼 run。
func withMinimalRunReq(c capabilityListRequest) capabilityListRequest {
	if c.Run != nil {
		return c
	}
	c.Run = &capabilityRunInput{
		Entrypoint: "run.sh",
		Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
	}
	return c
}
