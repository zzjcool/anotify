package ws

import (
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
)

// TestRehelloReclaimUsesOneClockSample 钉死「一条 hello 事实只允许一个时钟采样点」。
//
// applyInstances 先把申报的 slot upsert 成真实槽位（写 node_instances.updated_at），
// 再拿观察里的 ObservedAt 去 ReclaimStaleSlots 回收旧绑定。回收守则是
// `绑定的 updated_at 晚于 ObservedAt ⇒ 这条事实没看见它 ⇒ 跳过`。两处若各采一次
// 时钟，节点**明确回报的空闲**就会被当成旧快照丢掉：生产窗口是微秒级（真实丢回收，
// 中枢以为槽仍然占着），测试里假时钟冻结在用例开始那一秒，窗口被放大成「整场用例
// 是否跨秒」，于是同一条用例按秒边界随机红。
//
// 这里把 hub 时钟固定成比真实墙钟早 5s —— 两个采样点必然不同（不靠秒边界的运气），
// 正确实现只会产生一个时刻：
//   - upsert 写下的 updated_at 必须就是 hub 这次采样的 now（不是 store 内部另采的墙钟）；
//   - 带相符历史引用的 idle 必须真的回收。
//
// 时钟只在**会话建立前**改一次（此刻没有任何 hub goroutine 在读 e.now）；不搞「跑到
// 一半把假时钟拨到真实时间」那套——那是数据竞争（-race 会在 fixture/hub 的时钟读取上
// 报竞态），也不是这条缺陷的真实形态。
func TestRehelloReclaimUsesOneClockSample(t *testing.T) {
	e := newEnv(t)
	// hub 的采样点：比真实墙钟早 5s，与 store 内部 Now() 必然不同一个时刻。
	e.now = time.Now().Unix() - 5
	capability := e.capability(t, "analyze", "", false, nil)
	conn := e.dial(t)
	task, dispatch := e.dispatchOne(t, conn, capability)
	e.send(t, conn, &protocol.TaskAcceptFrame{
		Type: protocol.FrameTaskAccept, TaskID: task.ID, AssignmentID: dispatch.AssignmentID,
		ProposedAttempt: dispatch.ProposedAttempt, Node: "node-1", Slot: intPtr(0),
	})
	grant, ok := e.read(t, conn).(*protocol.LeaseGrantFrame)
	if !ok {
		t.Fatal("应收到 lease.grant")
	}
	rehello := func(instances []protocol.HelloInstance) {
		e.send(t, conn, &protocol.HelloFrame{
			Type: protocol.FrameHello, Name: "node-1", Token: e.token,
			Namespaces: []string{"default"}, Instances: instances,
		})
		if _, ok := e.read(t, conn).(*protocol.WelcomeFrame); !ok {
			t.Fatal("rehello 应回 welcome")
		}
	}

	// ① 只重报 slot（不给 busy 三态）：这条路径**只有** upsert 会写 updated_at。
	//    写下的时刻必须是 hub 的采样点，不能是 store 内部另采的那一次。
	rehello([]protocol.HelloInstance{{Slot: intPtr(0)}})
	if inst := e.instances(t); len(inst) != 1 || inst[0].UpdatedAt != e.now {
		t.Fatalf("node_instances.updated_at 必须是本次 hello 的采样点 %d（两处各采一次时钟就漂移）: %s",
			e.now, instFacts(inst))
	}

	// ② 匹配引用的 idle：回收判据必须落在**同一个**采样点上，否则
	//    updated_at(另采点) > ObservedAt(本采样点) 会把一条真实的空闲事实当成旧快照丢掉。
	rehello([]protocol.HelloInstance{{
		Slot: intPtr(0), Busy: boolPtr(false),
		TaskID: task.ID, Attempt: 1, LeaseID: grant.LeaseID,
	}})
	if inst := e.instances(t); len(inst) != 1 || inst[0].BusyTask != nil {
		t.Fatalf("匹配引用的 idle 应回收旧槽（回收判据与 upsert 必须是同一个采样点）: %s", instFacts(inst))
	}
}
