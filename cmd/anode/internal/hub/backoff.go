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
