package ws

import (
	"sync"
	"time"
)

// 本文件是 internal/ws 夹具**测试专用**的会话收尾屏障（#525，test hook）：等「hub 的会话
// 真的退完」而不赌时间。根因（httptest.Server.Close 不等被 hijack 的 WS 会话；detach
// **先**摘注册表**再**写库；DB.Close 不等在飞的查询）与实测见
// docs/reports/ws-fixture-flake.md，形状同 internal/server 的 hub_fixture_test.go。
type sessionSettle struct {
	mu      sync.Mutex
	pending int
	done    chan struct{} // 归零时关闭；只在 0 → 1 的跃迁上重建，等待方不会拿到过期通道
}

// newSessionSettle 造一个起步即「已归零」的屏障。
func newSessionSettle() *sessionSettle {
	s := &sessionSettle{done: make(chan struct{})}
	close(s.done)
	return s
}

func (s *sessionSettle) enter() {
	s.mu.Lock()
	if s.pending == 0 {
		s.done = make(chan struct{})
	}
	s.pending++
	s.mu.Unlock()
}

func (s *sessionSettle) exit() {
	s.mu.Lock()
	s.pending--
	if s.pending == 0 {
		close(s.done)
	}
	s.mu.Unlock()
}

// wait 等计数归零，最多 d；返回超时那一刻仍未返回的 handler 条数。
func (s *sessionSettle) wait(d time.Duration) (int, bool) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		s.mu.Lock()
		pending, done := s.pending, s.done
		s.mu.Unlock()
		if pending == 0 {
			return 0, true
		}
		select {
		case <-done:
		case <-timer.C:
			s.mu.Lock()
			pending = s.pending
			s.mu.Unlock()
			return pending, pending == 0
		}
	}
}
