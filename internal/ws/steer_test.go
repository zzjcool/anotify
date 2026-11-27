package ws

import (
	"context"
	"testing"
)

func TestSlotSteerWindowExpires(t *testing.T) { // 30s 窗口直测：只靠 reject→换台的间接路径抓不到它。
	h := &Hub{}
	h.markSlotFull("node-1", "ns-1", "triage", 100)
	if !h.slotFullRecently("node-1", "ns-1", "triage", 100+slotSteerWindowSec-1) {
		t.Fatalf("窗口内（距离开记 < %ds，与 assignment_expires_at 同形）建议必须生效", slotSteerWindowSec)
	}
	if h.slotFullRecently("node-1", "ns-1", "triage", 100+slotSteerWindowSec) {
		t.Fatalf("%ds 到点建议必须失效（否则被拒过的机器长期排在后面）", slotSteerWindowSec)
	}
	if n := len(h.steer.until); n != 0 {
		t.Fatalf("过期建议应顺手回收，仍留着 %d 条", n)
	}
	h.markSlotFull("node-1", "ns-1", "triage", 200)
	if h.slotFullRecently("node-2", "ns-1", "triage", 200) ||
		h.slotFullRecently("node-1", "ns-2", "triage", 200) ||
		h.slotFullRecently("node-1", "ns-1", "other", 200) {
		t.Fatalf("建议必须按 node/ns/能力 精确匹配")
	}
}

func TestSlotSteerClearedByGrant(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	task := e.pendingTask(t, e.pinnedCapability(t, "triage-clear", 1))

	e.hub.noteSlotFull(ctx, e.node.ID, task.ID, e.now)
	if !e.hub.slotFullRecently(e.node.ID, e.ns.ID, "triage-clear", e.now) {
		t.Fatalf("noteSlotFull 应记下 (node, ns, 能力) 建议")
	}
	e.hub.clearSlotFullSteer(ctx, e.node.ID, task.ID)
	if e.hub.slotFullRecently(e.node.ID, e.ns.ID, "triage-clear", e.now) {
		t.Fatalf("授出一次租约后建议必须清除")
	}
	e.hub.clearSlotFullSteer(ctx, e.node.ID, task.ID) // 没有建议时走内存快筛，不读库也不报错
	if n := len(e.hub.steer.until); n != 0 {
		t.Fatalf("清除后表里不该有残留，仍有 %d 条", n)
	}
}
