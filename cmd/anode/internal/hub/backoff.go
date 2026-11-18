// Package hub 是 anode 侧的节点连接客户端：把配置里的 server 解析成
// /v1/node/stream、用 WebSocket 收发 protocol 帧（含 ping 保活），并在断线后按
// 有界指数退避重连；下行帧的业务处理由 Handler 的实现方（internal/node）承担。
//
// 它只管连接与收发：派发、租约、结果、emit 的权威判定都在中枢侧 route/store，
// hub 不重算任何一条（重算就是两侧判定分叉的起点）。
package hub

import "time"

// 重连退避（D-I11 的节点侧）：有界指数退避，封顶后保持上限，不无限增大。
const (
	// DefaultBackoffBase 是首次重连等待。
	DefaultBackoffBase = time.Second
	// DefaultBackoffMax 是重连等待上限。
	DefaultBackoffMax = 30 * time.Second
	// DefaultBackoffFactor 是每次失败后的倍数。
	DefaultBackoffFactor = 2
)

// Backoff 是有界指数退避。零值等价于默认参数。
type Backoff struct {
	Base   time.Duration
	Max    time.Duration
	Factor int

	next  time.Duration
	first bool
}

// Next 返回这次应该等待的时长，并推进到下一次。
func (b *Backoff) Next() time.Duration {
	base, max, factor := b.params()
	if !b.first || b.next == 0 {
		b.first = true
		b.next = base
	}
	out := b.next
	if out > max {
		out = max
	}
	b.next = out * time.Duration(factor)
	if b.next > max {
		b.next = max
	}
	if b.next < base {
		b.next = base
	}
	return out
}

// Reset 在连接成功（收到 welcome）后重置退避。
func (b *Backoff) Reset() {
	b.next = 0
	b.first = false
}

func (b *Backoff) params() (base, max time.Duration, factor int) {
	base, max, factor = b.Base, b.Max, b.Factor
	if base <= 0 {
		base = DefaultBackoffBase
	}
	if max <= 0 {
		max = DefaultBackoffMax
	}
	if max < base {
		max = base
	}
	if factor < 2 {
		factor = DefaultBackoffFactor
	}
	return base, max, factor
}
