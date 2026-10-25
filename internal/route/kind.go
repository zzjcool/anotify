package route

import "github.com/zzjcool/anotify/internal/store"

// Kind 是能力在链上的角色（契约 §5）。**推导量，不落库**（不变量 I3）。
type Kind string

const (
	// KindOrigin 没有任何节点声明这个能力：只能当链的起点（触发器产出）。
	KindOrigin Kind = "origin"
	// KindEnd 有节点声明，且能力 outputs 全空：链到这里结束。
	KindEnd Kind = "end"
	// KindPipe 有节点声明，且 outputs 有非空项：链的中间段。
	KindPipe Kind = "pipe"
)

// DeriveKind 由声明推导能力的 kind（纯函数，不查库、不缓存）。
//
//	没有任何节点声明                 → origin
//	有节点声明，outputs 全空          → end
//	有节点声明，outputs 有任一非空项   → pipe
//
// 注意：中枢执行的内置能力（notify.phone）没有节点声明、outputs 也为空，
// 这个函数会把它算成 origin。那是不对的——它不接收事件，它是链的**终点**
// （中枢自己执行完就结束）。调用方应先用 CentralKind 覆盖，
// 见 KindForCapability。
//
// 多能力混挂：节点声明多个能力时，只要**有任一**能力的 outputs 非空 → 该节点是 pipe。
// 这个判定由调用方对节点的全部能力做「有任一非空」合并后再传入 declaredByAnyNode 与
// outputs 的并集即可（见 NodeKind）。
func DeriveKind(outputs []string, declaredByAnyNode bool) Kind {
	if !declaredByAnyNode {
		return KindOrigin
	}
	for _, o := range outputs {
		if o != "" {
			return KindPipe
		}
	}
	return KindEnd
}

// NodeKind 推导一个节点整体的 kind（多能力混挂的消歧规则）。
//
// 节点声明了多个能力时，只要**有任一**能力的 outputs 非空 → 整个节点 pipe；
// 都没声明任何能力 → origin；都声明了但 outputs 全空 → end。
func NodeKind(caps []*store.CapabilityVersion, declaredByAnyNode bool) Kind {
	if !declaredByAnyNode || len(caps) == 0 {
		return KindOrigin
	}
	var merged []string
	for _, c := range caps {
		merged = append(merged, c.Outputs...)
	}
	return DeriveKind(merged, true)
}

// KindForCapability 是整个仓库推导 kind 的**唯一入口**。
//
// 它在 DeriveKind 之上补两条内置能力的特例：
//
//	内置中山执行（notify.phone）→ end   中枢是它的"执行者"，不是"来源"
//	内置起点（github/cron/...） → origin 它没有节点，也不产生任务
//
// 为什么必须集中在这里：console 列表、能力详情、前端建边表单都各自
// 调过一次 DeriveKind，特例散落三处必然漂移（notify.phone 曾在能力页
// 显示成 origin，误导用户把它当事件来源）。
func KindForCapability(c *store.CapabilityVersion, declaredByAnyNode bool) Kind {
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
	return DeriveKind(c.Outputs, declaredByAnyNode)
}
