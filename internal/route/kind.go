package route

import "github.com/zzjcool/anotify/internal/store"

// Kind 是能力在链上的角色（契约 §5）。**推导量，不落库**（不变量 I3）。
type Kind string

const (
	// KindOrigin 有出边、没有入边：链的起点（只产出，没人喂它）。
	KindOrigin Kind = "origin"
	// KindEnd 没有出边：链到这里结束。
	KindEnd Kind = "end"
	// KindPipe 有出边、也有入边：链的中间段。
	KindPipe Kind = "pipe"
)

// DeriveKind 由**边的接线事实**推导能力在链上的角色（纯函数，不查库、不缓存）。
//
//	没有出边              → end      链到这里结束
//	有出边、没有入边       → origin   链的起点
//	有出边、有入边         → pipe     链的中间段
//
// 为什么看边而不是看 outputs（#383）：`outputs` 是**产物**里的端口声明，它曾经
// 同时被当成拓扑真相 —— 于是「建时声明 outputs: [] 之后又加了下游」的能力在页面上
// 一直显示「链到这里结束」，而那条边其实已经生效。拓扑的真相是边（与 assign.ports
// 的 MergePorts 同一条口径）。
//
// 注意：中枢执行的内置能力（notify.phone）走不到这里 —— 它由 Central 特例覆盖，
// 见 KindForCapability。调用方**不要**直接调这个函数，用 KindForCapability。
func DeriveKind(hasOutEdges, hasInEdges bool) Kind {
	if !hasOutEdges {
		return KindEnd
	}
	if !hasInEdges {
		return KindOrigin
	}
	return KindPipe
}

// NodeKind 推导一个节点整体的 kind（多能力混挂的消歧规则）。
//
// 节点声明了多个能力时，只要**任一**能力有出边 → 节点有出口；任一能力有入边 →
// 节点有入口，合并后再走同一条规则（DeriveKind）。空形状（没有能力可推导）算 end。
//
// 输入是逐能力的 store.EdgeShape（由调用方按名字各读一份）：这样节点视图与能力视图
// 不会各自演化出第二条推导规则。
func NodeKind(shapes []*store.EdgeShape) Kind {
	hasOut, hasIn := false, false
	for _, s := range shapes {
		if s == nil {
			continue
		}
		if s.HasOut() {
			hasOut = true
		}
		if s.HasIn {
			hasIn = true
		}
	}
	return DeriveKind(hasOut, hasIn)
}

// KindForCapability 是整个仓库推导 kind 的**唯一入口**。
//
// 它在 DeriveKind 之上补两条内置能力的特例：
//
//	内置中枢执行（notify.phone）→ end   中枢是它的"执行者"，执行完就结束，
//	                                    不产生下游任务（executor 不 emit）
//	内置起点（github/cron/...） → origin 它不接收事件、也不产生任务
//
// 为什么必须集中在这里：console 列表、能力详情、建能力的响应都各自推导过一次
// kind，特例散落三处必然漂移（notify.phone 曾在能力页显示成 origin，误导用户把它
// 当事件来源）。
//
// shape 传 nil 是允许的（读边失败的降级路径）：当作「没有任何边」。
func KindForCapability(c *store.CapabilityVersion, shape *store.EdgeShape) Kind {
	if c == nil {
		return KindOrigin
	}
	if bc, ok := store.IsBuiltinCapability(c.Name, c.Version); ok {
		if bc.Central {
			return KindEnd
		}
		if bc.Origin {
			return KindOrigin
		}
	}
	return DeriveKind(shape.HasOut(), shape != nil && shape.HasIn)
}
