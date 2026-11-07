package ws

// 本文件是**测试夹具**（P1-b1 机械适配）：给 ws 包的用例补一份能力正文。
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
