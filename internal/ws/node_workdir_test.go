package ws

// 能力工作目录（迁移 19）在**派发帧**上的验收：中枢必须把部署记录里的 work_dir
// 带给节点，且查不到时不许编一个值出来（空 = 用默认的每任务目录）。

import (
	"context"
	"errors"
	"testing"
)

// fakeWorkDirs 是可编程的 WorkDirResolver。
type fakeWorkDirs struct {
	dir string
	err error
}

func (f fakeWorkDirs) DeploymentWorkDir(ctx context.Context, namespaceID, nodeID, capabilityID string) (string, error) {
	return f.dir, f.err
}

// TestAssignFrameCarriesWorkDir：部署里配了目录 → 帧里带 work_dir。
func TestAssignFrameCarriesWorkDir(t *testing.T) {
	f := newNodeFixture(t)
	f.handler.hub().WorkDirs = fakeWorkDirs{dir: "/home/me/proj"}
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	task := f.makeAssignedTask(t, "trace_workdir_1")
	publishTo(t, f, f.node.ID, task.ID)

	frame := readUntil(t, conn, NodeFrameAssign)
	if got := frame["work_dir"]; got != "/home/me/proj" {
		t.Fatalf("assign.work_dir = %v, want /home/me/proj（部署里配的目录要带给节点）", got)
	}
}

// TestAssignFrameWithoutWorkDir：没有部署记录 / 解析失败 → 帧里**不出现**该字段，
// 节点因此走默认的每任务目录（与加这一列之前逐字节一致）。
func TestAssignFrameWithoutWorkDir(t *testing.T) {
	cases := []struct {
		name     string
		resolver WorkDirResolver
	}{
		{"没有解析器", nil},
		{"查不到部署", fakeWorkDirs{dir: ""}},
		{"查询失败", fakeWorkDirs{err: errors.New("db is busy")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newNodeFixture(t)
			if tc.resolver != nil {
				f.handler.hub().WorkDirs = tc.resolver
			}
			conn := f.mustDial(t)
			hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
				[]InstanceDecl{{Slot: 0, Concurrency: 1}})

			task := f.makeAssignedTask(t, "trace_workdir_absent")
			publishTo(t, f, f.node.ID, task.ID)

			frame := readUntil(t, conn, NodeFrameAssign)
			// omitempty：空值不出现在帧里，节点解码得到零值 = 默认目录。
			if v, ok := frame["work_dir"]; ok && v != "" {
				t.Fatalf("不该带 work_dir，实际 %v", v)
			}
		})
	}
}

// TestAssignFrameWorkDirQueryFailureDoesNotBlockDispatch：解析失败只降级，
// **不能**让任务派不出去（那是加这一列之前从没发生过的事）。
func TestAssignFrameWorkDirQueryFailureDoesNotBlockDispatch(t *testing.T) {
	f := newNodeFixture(t)
	f.handler.hub().WorkDirs = fakeWorkDirs{err: errors.New("db is busy")}
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	task := f.makeAssignedTask(t, "trace_workdir_degrade")
	if err := f.handler.hub().PublishAssign(context.Background(), f.nsID, f.node.ID, task); err != nil {
		t.Fatalf("work_dir 查询失败不该阻断派发：%v", err)
	}
	readUntil(t, conn, NodeFrameAssign)
}
