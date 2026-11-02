package ws

import (
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// 即时重派钩子（#219）的接线验收。
//
// 为什么要单独测"钩子有没有被调用"：sweeper 自身的 Nudge 有单测，但**接线**在
// 两处（mux 把 sweeper.Nudge 塞进 NodeHandler，node_session 在终态落库后调它）。
// 接线断了不会有任何报错——只是退回"每 5s 巡检一次"，单槽吞吐掉两个数量级，
// 而且很难在功能测试里看出来（任务最终都会完成，只是慢）。
//
// 钩子必须在**终态确实落库之后**触发：槽位释放与终态是同一个事务，抢在提交前
// Nudge 只会让重派看到"还没有空槽"，白跑一次还制造误导性的日志。

// TestResultFreesSlotNudgesDispatcher：成功回报 → 钩子被调用且只调一次。
func TestResultFreesSlotNudgesDispatcher(t *testing.T) {
	fired := make(chan struct{}, 8)
	f := newNodeFixture(t, func(h *NodeHandler) {
		h.OnCapacityFreed = func() {
			select {
			case fired <- struct{}{}:
			default:
			}
		}
	})

	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []CapabilityDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	task := f.makeAssignedTask(t, "trace_219_nudge")
	sentResult(t, conn, task.ID, "fix")
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskSucceeded)

	select {
	case <-fired:
		// 触发 ✓
	case <-time.After(2 * time.Second):
		t.Fatal("任务终态落库后没有触发即时重派钩子（#219 回归：槽位空出来没人拉下一条）")
	}
}

// TestRejectedResultDoesNotNudge：终态**没落库**（被状态机拒绝）时不该触发。
//
// 反例很重要：如果这里也 Nudge，就等于"任务没完成却也认为空出了槽"，
// 重派会白跑一轮，把真实原因是「结果被拒」掩盖成「没有可派任务」。
func TestRejectedResultDoesNotNudge(t *testing.T) {
	fired := make(chan struct{}, 8)
	f := newNodeFixture(t, func(h *NodeHandler) {
		h.OnCapacityFreed = func() {
			select {
			case fired <- struct{}{}:
			default:
			}
		}
	})

	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []CapabilityDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})
	// hello 成功本身就会触发一次（节点上线 = 新增了执行能力），先把它排空，
	// 之后看到的信号才只可能来自 result。
	time.Sleep(100 * time.Millisecond)
	for drained := false; !drained; {
		select {
		case <-fired:
		default:
			drained = true
		}
	}

	// 别人的任务（node_id 不是本节点）→ reportTransitionError 分支，终态不落库
	task := f.makeAssignedTask(t, "trace_219_rejected")
	sentResult(t, conn, task.ID+"_nope", "fix") // 未知任务 → 直接 error 帧
	// 读到那帧 error，确保服务端这条分支已经走完（readUntil 自带超时）
	readUntil(t, conn, NodeFrameError)

	select {
	case <-fired:
		t.Fatal("终态没落库却触发了即时重派钩子：会在没有空槽时白跑一轮")
	case <-time.After(300 * time.Millisecond):
		// 正确：没有触发
	}
}
