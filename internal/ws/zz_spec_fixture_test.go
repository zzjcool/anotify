package ws

// 本文件是**测试夹具**：给 ws 包的用例补一份能力正文。
//
// Capability Spec v3 起「能力 = 正文（spec）」，派生列由 store 在写入时从正文算出。
// 生产路径明令禁止从派生字段反推正文（契约 §4），但测试夹具的意图恰恰是
// 「给我一个 outputs=[a,b] 的能力」——在本包的用例里重复手写 spec 只会增加写错的
// 机会。已经带了实现（handler 或 run）的正文原样返回：显式写的 spec 优先。
//
// 默认给**脚本形式**（不是 handler: noop）：本包的部署用例要真的把产物推给节点，
// 而 handler 形式刻意不可部署（Capability Spec v3 §5）——给一份 noop 占位会让
// 「部署 → 推帧 → apply」这条主链在本包整体测不到。

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
	c.Spec = artifact.Spec{
		Params:    c.ParamsSchema,
		Outputs:   append([]string{}, c.Outputs...),
		Lifecycle: lc,
		Sticky:    c.Sticky,
		Run: artifact.Run{
			Entrypoint: "run.sh",
			Files:      map[string]string{"run.sh": "#!/bin/bash\nexit 0\n"},
		},
	}
	return c
}
