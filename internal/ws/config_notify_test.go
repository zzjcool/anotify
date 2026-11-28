package ws

// config_notify_test.go 是 #569 的回归：写配置后中枢必须推 config.changed。

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
)

func bumpNodeConfig(t *testing.T, e *env, vars map[string]string) int64 {
	t.Helper()
	ctx := context.Background()
	cfg, err := e.db.GetNodeConfig(ctx, e.node.ID)
	if err != nil {
		t.Fatalf("读节点配置: %v", err)
	}
	cfg.Vars = vars
	if err := e.db.PutNodeConfig(ctx, cfg); err != nil {
		t.Fatalf("写节点配置: %v", err)
	}
	fresh, err := e.db.GetNodeConfig(ctx, e.node.ID)
	if err != nil {
		t.Fatalf("回读节点配置: %v", err)
	}
	return fresh.Revision
}

func TestConfigPutPushesConfigChangedToOnlineNode(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	conn, _ := e.dialCaptureWelcome(t)

	capability := e.capability(t, "notify-cap", "", true, nil)
	e.deployApplied(t, capability)
	task := e.pendingTask(t, capability)

	revision := bumpNodeConfig(t, e, map[string]string{"FOO": "bar"})
	if err := e.hub.NotifyConfigChanged(ctx, e.node.ID); err != nil {
		t.Fatalf("通知 config.changed: %v", err)
	}

	frame, _, err := e.readRaw(t, conn, time.Second)
	if err != nil {
		t.Fatalf("配置 revision 变化后 1s 内没有收到 config.changed（中枢没有 producer）: %v", err)
	}
	changed, ok := frame.(*protocol.ConfigChangedFrame)
	if !ok {
		t.Fatalf("配置变更后第一帧应是 config.changed，得到 %T", frame)
	}
	if changed.Revision != strconv.FormatInt(revision, 10) {
		t.Fatalf("config.changed.revision = %q, want %d", changed.Revision, revision)
	}

	if n, err := e.hub.DispatchOnce(ctx, 10); err != nil || n != 0 {
		t.Fatalf("配置未应用时 DispatchOnce = %d, %v；want 0, nil（ConfigPending 不是候选）", n, err)
	}
	if got := e.task(t, task.ID); got.Phase != "pending" {
		t.Fatalf("任务应停在 pending，得到 %s", got.Phase)
	}

	e.send(t, conn, &protocol.ConfigGetFrame{Type: protocol.FrameConfigGet})
	frame = e.read(t, conn)
	welcome, ok := frame.(*protocol.WelcomeFrame)
	if !ok {
		t.Fatalf("config.get 的应答应是 welcome，得到 %T", frame)
	}
	if welcome.ConfigRevision != changed.Revision {
		t.Fatalf("welcome.configRevision = %q, want %q（与 config.changed 同源）",
			welcome.ConfigRevision, changed.Revision)
	}
	if welcome.NodeVars["FOO"] != "bar" {
		t.Fatalf("welcome.nodeVars 没带新值：%v", welcome.NodeVars)
	}

	hash, err := protocol.MachineConfigHash(welcome.NodeVars, welcome.NodeLimits)
	if err != nil {
		t.Fatalf("按共享规则算机器配置 hash: %v", err)
	}
	e.send(t, conn, &protocol.ConfigAppliedFrame{
		Type: protocol.FrameConfigApplied, Revision: welcome.ConfigRevision, Hash: hash,
	})
	if !waitFor(t, time.Second, func() bool {
		cfg, err := e.db.GetNodeConfig(ctx, e.node.ID)
		return err == nil && cfg.AppliedRevision == cfg.Revision
	}) {
		t.Fatalf("节点回报 config.applied 后 appliedRevision 没有追上 revision")
	}

	if n, err := e.hub.DispatchOnce(ctx, 10); err != nil || n != 1 {
		t.Fatalf("配置对齐后 DispatchOnce = %d, %v；want 1, nil", n, err)
	}
	frame = e.read(t, conn)
	dispatch, ok := frame.(*protocol.TaskDispatchFrame)
	if !ok {
		t.Fatalf("配置对齐后应收到 task.dispatch，得到 %T", frame)
	}
	if dispatch.TaskID != task.ID {
		t.Fatalf("派发的任务 = %q, want %q", dispatch.TaskID, task.ID)
	}
}

func TestNotifyConfigChangedSkipsInSyncNode(t *testing.T) {
	e := newEnv(t)
	conn, welcome := e.dialCaptureWelcome(t)
	if welcome.ConfigRevision == "" {
		t.Fatal("welcome 没带 configRevision")
	}
	if err := e.hub.NotifyConfigChanged(context.Background(), e.node.ID); err != nil {
		t.Fatalf("已对齐的节点通知不该报错: %v", err)
	}
	if frame, _, err := e.readRaw(t, conn, 300*time.Millisecond); err == nil {
		t.Fatalf("已对齐的节点不该收到任何帧，得到 %T", frame)
	}
}

func TestNotifyConfigChangedOfflineNodeIsNoop(t *testing.T) {
	e := newEnv(t)
	bumpNodeConfig(t, e, map[string]string{"FOO": "bar"})
	if e.hub.Online(e.node.ID) {
		t.Fatal("夹具前提：这条用例要求节点不在线")
	}
	if err := e.hub.NotifyConfigChanged(context.Background(), e.node.ID); err != nil {
		t.Fatalf("离线节点的通知应是 no-op，得到错误: %v", err)
	}
	if err := e.hub.NotifyConfigChanged(context.Background(), "node_missing"); err != nil {
		t.Fatalf("未知节点的通知应是 no-op，得到错误: %v", err)
	}
}

func TestNudgeStaleConfigsPushesMissedNotification(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	conn, _ := e.dialCaptureWelcome(t)

	revision := bumpNodeConfig(t, e, map[string]string{"FOO": "bar"}) // 不调通知：模拟丢失

	e.hub.nudgeStaleConfigs(ctx)
	frame, _, err := e.readRaw(t, conn, time.Second)
	if err != nil {
		t.Fatalf("兜底补推没有发出 config.changed: %v", err)
	}
	changed, ok := frame.(*protocol.ConfigChangedFrame)
	if !ok {
		t.Fatalf("兜底补推的第一帧应是 config.changed，得到 %T", frame)
	}
	if changed.Revision != strconv.FormatInt(revision, 10) {
		t.Fatalf("兜底补推的 revision = %q, want %d", changed.Revision, revision)
	}

	// 节点仍未 apply：节流窗口内的第二轮不该再发（否则故障节点被每 250ms 打一次）。
	e.hub.nudgeStaleConfigs(ctx)
	if frame, _, err := e.readRaw(t, conn, 300*time.Millisecond); err == nil {
		t.Fatalf("节流窗口内的第二轮补推不该再发帧，得到 %T", frame)
	}
}
