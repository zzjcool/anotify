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
