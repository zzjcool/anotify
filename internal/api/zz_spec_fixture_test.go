package api

// 本文件是**测试夹具**（P1-b1 机械适配）：给 api 包的用例补一份能力正文。
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
	// 把"占位实现"换成一份真的脚本实现（节点执行）。
	//
	// 为什么不能再留 handler: noop（P2b）：noop 是合法但**不可执行**的占位，
	// 派发遇到它会剪枝 pruned/no_impl（契约 §3.1 / R1，见 route.ReasonNoImpl）。
	// 夹具的意图是"给我一个能参与路由的能力"，占位实现恰好是最不该给的那种——
	// 任何一个派发用例都会因此一条任务都建不出来。脚本形式与真实用户写得出来的
	// 能力同形：入口 run.sh、内容以 #! 开头、由声明它的节点执行。
	c.Spec.Handler = ""
	c.Spec.Run = artifact.Run{
		Entrypoint: "run.sh",
		Files:      map[string]string{"run.sh": "#!/bin/bash\nexit 0\n"},
		TimeoutSec: 30,
	}
	return c
}
