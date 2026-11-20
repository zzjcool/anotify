package runner

import (
	"fmt"
	"testing"
	"time"
)

// testWaitLimit 是包内测试等「异步事实」的统一上限：goroutine 送出的 ACK/结果帧、
// 真实进程退出、真实 HTTP 请求到达、租约 goroutine 注册定时器。
//
// 为什么不是 2s —— anotify#478：这些等待等的是 goroutine + channel 上的异步帧，
// 不是「逻辑上应该立刻发生」的事；CI 上同一个 job 里还跑着 internal/server 等重负载
// 包，2s 硬墙钟会被吃掉，于是只改文档的 PR 也被合并闸随机拦红（连拦两次）。
// 这里放宽的只是 deadline（允许更慢的环境），断言一个字没放宽：没等到仍然 Fatal。
const testWaitLimit = 15 * time.Second

// waitTest 从 ch 收一帧，最多等 limit（封顶 testWaitLimit）。超时 t.Fatalf 并打印
// 描述、实际等待时长、以及调用方给出的最后观察到的状态（可选，拿得到就传）。
//
// 它只负责 deadline，不负责断言：收到了就把值原样返回，由调用方按原条件判断
// （例如「必须是那个 ACK」）；绝不把「超时」当成「通过」。
func waitTest[T any](t *testing.T, ch <-chan T, limit time.Duration, desc string, lastState ...func() string) T {
	t.Helper()
	if limit <= 0 || limit > testWaitLimit {
		limit = testWaitLimit
	}
	start := time.Now()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	var zero T
	select {
	case v := <-ch:
		return v
	case <-timer.C:
		state := "<调用方未提供>"
		if len(lastState) > 0 && lastState[0] != nil {
			state = lastState[0]()
		}
		t.Fatalf("等待 %s 超时：实际等待 %s（上限 %s）；最后观察到的状态：%s",
			desc, time.Since(start).Round(time.Millisecond), limit, state)
		return zero
	}
}

// describeChan 打印 channel 当前缓冲长度，供 waitTest 的 lastState 使用
// （不读走元素，纯观察）。
func describeChan[T any](name string, ch <-chan T) func() string {
	return func() string { return fmt.Sprintf("%s len=%d", name, len(ch)) }
}
