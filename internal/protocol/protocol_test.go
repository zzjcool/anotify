package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// ---------- 帧名快照 ----------

// TestFrameNamesAreWireValues 钉死帧类型名：字符串本身就是协议，改一个字就是断连。
func TestFrameNamesAreWireValues(t *testing.T) {
	want := map[string]string{
		"hello":               FrameHello,
		"welcome":             FrameWelcome,
		"config.changed":      FrameConfigChanged,
		"config.get":          FrameConfigGet,
		"config.applied":      FrameConfigApplied,
		"task.dispatch":       FrameTaskDispatch,
		"task.accept":         FrameTaskAccept,
		"task.reject":         FrameTaskReject,
		"lease.grant":         FrameLeaseGrant,
		"lease.renew":         FrameLeaseRenew,
		"lease.ack":           FrameLeaseAck,
		"task.result":         FrameTaskResult,
		"result.ack":          FrameResultAck,
		"task.cancel":         FrameTaskCancel,
		"cancel.ack":          FrameCancelAck,
		"upgrade":             FrameUpgrade,
		"upgrade_ack":         FrameUpgradeAck,
		"logs_request":        FrameLogsRequest,
		"logs_response":       FrameLogsResponse,
		"capability.deploy":   FrameCapabilityDeploy,
		"capability.deployed": FrameCapabilityDeployed,
		"task.emit":           FrameTaskEmit,
		"task.emit.ack":       FrameTaskEmitAck,
	}
	for wire, got := range want {
		if got != wire {
			t.Fatalf("帧类型常量 = %q，期望 %q", got, wire)
		}
	}
	// 23 帧一个不多一个不少：上行 ∪ 下行 = 全集，且两边不相交。
	up, down := UpstreamFrames(), DownstreamFrames()
	if len(up)+len(down) != len(want) {
		t.Fatalf("上行 %d + 下行 %d 帧，期望共 %d", len(up), len(down), len(want))
	}
	seen := map[string]bool{}
	for _, f := range append(append([]string{}, up...), down...) {
		if seen[f] {
			t.Fatalf("帧 %q 同时出现在上行与下行", f)
		}
		seen[f] = true
		if _, ok := want[f]; !ok {
			t.Fatalf("方向表里出现了未登记的帧 %q", f)
		}
	}
	for wire := range want {
		if !seen[wire] {
			t.Fatalf("帧 %q 没有方向", wire)
		}
	}
	// 上行/下行判定与方向表一致。
	for _, f := range up {
		if !IsUpstream(f) || IsDownstream(f) {
			t.Fatalf("帧 %q 的上行判定错了", f)
		}
	}
	for _, f := range down {
		if !IsDownstream(f) || IsUpstream(f) {
			t.Fatalf("帧 %q 的下行判定错了", f)
		}
	}
}

// ---------- 往返 + 字段名快照 ----------

// frameFixture 是一帧「所有字段都填满」的样本：只有填满，字段名快照才能抓住
// 漏写/改名的 wire 键。
type frameFixture struct {
	name     string
	frame    any
	wantKeys []string
}

func utc(y int, mo time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, mo, d, h, mi, s, 0, time.UTC)
}

func exitCode(v int) *int { return &v }

func testStr(v string) *string { return &v }

func frameFixtures() []frameFixture {
	expires := utc(2026, 11, 10, 12, 0, 30)
	started := utc(2026, 11, 10, 11, 59, 0)
	finished := utc(2026, 11, 10, 12, 0, 5)
	return []frameFixture{
		{
			name: "hello",
			frame: &HelloFrame{
				Type:            FrameHello,
				Name:            "node-a",
				Token:           "tok",
				Namespaces:      []string{"default", "team-a"},
				LocalVars:       map[string]string{"SOURCE_ROOT": "/srv/src"},
				LocalLimits:     Limits{Total: 8, PerCapability: map[string]int{"review": 2}},
				AppliedRevision: "7",
				AppliedHash:     "abc123",
				AgentVersion:    "1.4.0",
				AgentCommit:     "3fd5a59",
				Facts: NodeFacts{
					HostIP: "10.0.0.7", Hostname: "node-a", OS: "linux", Arch: "amd64",
					CPU: 8, WorkRoot: "/var/lib/anode/runs", HomeDir: "/home/anode",
				},
				Capabilities: []HelloCapability{{
					Name: "review", Version: 2, Digest: "sha256:abc",
					Commands: []HelloCommand{{Name: "git", Found: true, Path: "/usr/bin/git"}},
				}},
				Instances: []HelloInstance{{Slot: intPtrTest(0), Busy: boolPtrTest(true), TaskID: "tsk_1", Attempt: 2, LeaseID: "lease_1"}},
			},
			wantKeys: []string{
				"agentCommit", "agentVersion", "appliedHash", "appliedRevision",
				"capabilities", "facts", "instances", "localLimits", "localVars",
				"name", "namespaces", "token", "type",
			},
		},
		{
			name: "welcome",
			frame: &WelcomeFrame{
				Type:           FrameWelcome,
				ConfigRevision: "7",
				NodeVars:       map[string]string{"TOOLCHAIN": "/opt/go"},
				NodeLimits:     Limits{Total: 4, PerCapability: map[string]int{"*": 2}},
				Namespaces:     []string{"default"},
			},
			wantKeys: []string{"configRevision", "namespaces", "nodeLimits", "nodeVars", "type"},
		},
		{
			name:     "config.changed",
			frame:    &ConfigChangedFrame{Type: FrameConfigChanged, Revision: "8"},
			wantKeys: []string{"revision", "type"},
		},
		{
			name:     "config.get",
			frame:    &ConfigGetFrame{Type: FrameConfigGet, AppliedRevision: "7", AppliedHash: "abc123"},
			wantKeys: []string{"appliedHash", "appliedRevision", "type"},
		},
		{
			name:     "config.applied",
			frame:    &ConfigAppliedFrame{Type: FrameConfigApplied, Revision: "7", Hash: "abc123", MachineVars: map[string]string{"SOURCE_ROOT": "/srv/src"}},
			wantKeys: []string{"hash", "machineVars", "revision", "type"},
		},
		{
			name: "task.dispatch",
			frame: &TaskDispatchFrame{
				Type:                  FrameTaskDispatch,
				TaskID:                "tsk_1",
				AssignmentID:          "asg_1",
				ProposedAttempt:       2,
				Namespace:             "default",
				Capability:            CapabilityRef{Name: "review", Version: 3, Digest: "sha256:aa"},
				Params:                json.RawMessage(`{"repo":"a"}`),
				NamespaceVars:         map[string]string{"NOTIFY_BASE": "https://n"},
				NamespaceVarsRevision: "12",
				ConfigRevision:        "7",
				AssignmentExpiresAt:   utc(2026, 11, 10, 12, 1, 0),
			},
			wantKeys: []string{"assignmentExpiresAt", "assignmentId", "capability", "configRevision", "namespace", "namespaceVars", "namespaceVarsRevision", "params", "proposedAttempt", "taskId", "type"},
		},
		{
			name:     "task.accept",
			frame:    &TaskAcceptFrame{Type: FrameTaskAccept, TaskID: "tsk_1", AssignmentID: "asg_1", ProposedAttempt: 2, Node: "node-a", Slot: intPtrTest(0)},
			wantKeys: []string{"assignmentId", "node", "proposedAttempt", "slot", "taskId", "type"},
		},
		{
			name:     "task.reject",
			frame:    &TaskRejectFrame{Type: FrameTaskReject, TaskID: "tsk_1", AssignmentID: "asg_1", ProposedAttempt: 2, Node: "node-a", Reason: ReasonStaleConfig},
			wantKeys: []string{"assignmentId", "node", "proposedAttempt", "reason", "taskId", "type"},
		},
		{
			name:     "lease.grant",
			frame:    &LeaseGrantFrame{Type: FrameLeaseGrant, TaskID: "tsk_1", Attempt: 2, Node: "node-a", LeaseID: "lease_1", AttemptToken: "tok_1", ExpiresAt: expires, AssignmentID: "asg_1", Slot: intPtrTest(0)},
			wantKeys: []string{"assignmentId", "attempt", "attemptToken", "expiresAt", "leaseId", "node", "slot", "taskId", "type"},
		},
		{
			name:     "lease.renew",
			frame:    &LeaseRenewFrame{Type: FrameLeaseRenew, TaskID: "tsk_1", Attempt: 2, LeaseID: "lease_1"},
			wantKeys: []string{"attempt", "leaseId", "taskId", "type"},
		},
		{
			name:     "lease.ack",
			frame:    &LeaseAckFrame{Type: FrameLeaseAck, TaskID: "tsk_1", Attempt: 2, LeaseID: "lease_1", ExpiresAt: expires},
			wantKeys: []string{"attempt", "expiresAt", "leaseId", "taskId", "type"},
		},
		{
			name: "task.result",
			frame: &TaskResultFrame{
				Type:       FrameTaskResult,
				TaskID:     "tsk_1",
				Attempt:    2,
				Node:       "node-a",
				LeaseID:    "lease_1",
				Phase:      TaskPhaseFailed,
				ExitCode:   exitCode(1),
				Result:     TaskResult{Summary: "boom", Artifacts: []TaskArtifact{{Path: "artifacts/report.md", SHA256: "ff"}}, Error: &TaskResultError{Reason: ReasonScriptFailed, Message: "exit 1"}},
				StartedAt:  started,
				FinishedAt: finished,
			},
			wantKeys: []string{"attempt", "exitCode", "finishedAt", "leaseId", "node", "phase", "result", "startedAt", "taskId", "type"},
		},
		{
			name:     "result.ack",
			frame:    &ResultAckFrame{Type: FrameResultAck, TaskID: "tsk_1", Attempt: 2, Accepted: true},
			wantKeys: []string{"accepted", "attempt", "taskId", "type"},
		},
		{
			name:     "task.cancel",
			frame:    &TaskCancelFrame{Type: FrameTaskCancel, TaskID: "tsk_1", Attempt: 2, Reason: CancelReasonUser, GraceSec: 5, LeaseID: "lease_1", Slot: intPtrTest(0)},
			wantKeys: []string{"attempt", "graceSec", "leaseId", "reason", "slot", "taskId", "type"},
		},
		{
			name:     "cancel.ack",
			frame:    &CancelAckFrame{Type: FrameCancelAck, TaskID: "tsk_1", Attempt: 2, ProcessGroupReaped: true, LeaseID: "lease_1", Slot: intPtrTest(0)},
			wantKeys: []string{"attempt", "leaseId", "processGroupReaped", "slot", "taskId", "type"},
		},
		{
			name:     "upgrade",
			frame:    &UpgradeFrame{Type: FrameUpgrade, TargetVersion: "1.6.0", SHA256: "abc", URLPath: NodeBinaryPath},
			wantKeys: []string{"sha256", "targetVersion", "type", "urlPath"},
		},
		{
			name:     "upgrade_ack",
			frame:    &UpgradeAckFrame{Type: FrameUpgradeAck, OK: true, Version: "1.6.0", Commit: "deadbee", Error: ""},
			wantKeys: []string{"commit", "error", "ok", "type", "version"},
		},
		{
			name:     "logs_request",
			frame:    &LogsRequestFrame{Type: FrameLogsRequest, TaskID: "tsk_1", RequestID: "lreq_1", Attempt: 2},
			wantKeys: []string{"attempt", "requestId", "taskId", "type"},
		},
		{
			name: "logs_response",
			frame: &LogsResponseFrame{
				Type: FrameLogsResponse, TaskID: "tsk_1", RequestID: "lreq_1", Attempt: 2,
				Stdout: "out", Stderr: "err", Truncated: true, Bytes: 6, Error: LogsErrNoLocalArchive,
			},
			wantKeys: []string{"attempt", "bytes", "error", "requestId", "stderr", "stdout", "taskId", "truncated", "type"},
		},
		{
			name: "capability.deploy",
			frame: &CapabilityDeployFrame{
				Type: FrameCapabilityDeploy, RequestID: "dep_1",
				DeployRefs: DeployRefs{Namespace: "default", Name: "review", Version: 3, Digest: "sha256:aa"},
			},
			wantKeys: []string{"digest", "name", "namespace", "requestId", "type", "version"},
		},
		{
			name: "capability.deployed",
			frame: &CapabilityDeployedFrame{
				Type: FrameCapabilityDeployed, RequestID: "dep_1",
				DeployRefs: DeployRefs{Namespace: "default", Name: "review", Version: 3, Digest: "sha256:aa"},
				Status:     DeployStatusApplied,
				Error:      "",
			},
			wantKeys: []string{"digest", "name", "namespace", "requestId", "status", "type", "version"},
		},
		{
			name: "task.emit",
			frame: &TaskEmitFrame{
				Type: FrameTaskEmit, RequestID: "ereq_1", TaskID: "tsk_1", Attempt: 2,
				LeaseID: "lease_1", AttemptToken: "tok_1",
				EmitRequest: EmitRequest{To: "fixer", ID: "review-a.go", Data: json.RawMessage(`{"file":"a.go"}`)},
			},
			wantKeys: []string{"attempt", "attemptToken", "data", "id", "leaseId", "requestId", "taskId", "to", "type"},
		},
		{
			name: "task.emit.ack",
			frame: &TaskEmitAckFrame{
				Type: FrameTaskEmitAck, RequestID: "ereq_1", TaskID: "tsk_1", Attempt: 2, HTTPStatus: 200,
				Response: &EmitResponse{TaskID: testStr("tsk_2"), EventID: "evt_1", To: "fixer", Version: 2},
			},
			wantKeys: []string{"attempt", "httpStatus", "requestId", "response", "taskId", "type"},
		},
	}
}

// TestFrameRoundTripAndKeys 是协议层最重要的一条回归：
// 每一帧 marshal → Decode → marshal 必须逐字节相同（字段一个都不丢），
// 且顶层 wire 键名必须与快照一致（camelCase 不许漂）。
func TestFrameRoundTripAndKeys(t *testing.T) {
	for _, fx := range frameFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			raw, err := Encode(fx.frame)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}

			// 顶层键名快照。
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("unmarshal keys: %v", err)
			}
			gotKeys := make([]string, 0, len(m))
			for k := range m {
				gotKeys = append(gotKeys, k)
			}
			sort.Strings(gotKeys)
			if !reflect.DeepEqual(gotKeys, fx.wantKeys) {
				t.Fatalf("wire 键名漂了：\n got %v\nwant %v", gotKeys, fx.wantKeys)
			}

			// type 必须是帧名本身。
			if string(m["type"]) != `"`+fx.wantKeys[0]+`"` {
				// type 的键名未必排第一，单独校值。
				if !bytes.Contains(raw, []byte(`"type":"`+fx.name+`"`)) {
					t.Fatalf("type 字段不是帧名：%s", raw)
				}
			}

			// 往返：字段一个不丢。
			decoded, err := Decode(raw)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if reflect.TypeOf(decoded) != reflect.TypeOf(fx.frame) {
				t.Fatalf("Decode 返回 %T，期望 %T", decoded, fx.frame)
			}
			again, err := Encode(decoded)
			if err != nil {
				t.Fatalf("re-Encode: %v", err)
			}
			if !bytes.Equal(raw, again) {
				t.Fatalf("往返后字节不同：\n first %s\nsecond %s", raw, again)
			}
		})
	}
}

// TestLegacyFramesKeepLenientDecode 只有 task.emit / task.emit.ack 是父批准的
// 严格字段帧；其余 **21 帧保持既有宽松语义**——一条老/新帧多一个键不该整帧被拒
// （不断连）。这条和两条 emit 的严格测试成对，防止解 E1 时顺手改全包语义。
func TestLegacyFramesKeepLenientDecode(t *testing.T) {
	checked := 0
	for _, fx := range frameFixtures() {
		if fx.name == FrameTaskEmit || fx.name == FrameTaskEmitAck {
			continue
		}
		raw, err := Encode(fx.frame)
		if err != nil {
			t.Fatalf("%s: Encode: %v", fx.name, err)
		}
		withUnknown := make([]byte, 0, len(raw)+32)
		withUnknown = append(withUnknown, raw[:len(raw)-1]...)
		withUnknown = append(withUnknown, []byte(`,"futureField":{"nested":true}}`)...)
		if _, err := Decode(withUnknown); err != nil {
			t.Fatalf("%s: 未知字段不该让 Decode 失败：%v", fx.name, err)
		}
		checked++
	}
	if want := len(UpstreamFrames()) + len(DownstreamFrames()) - 2; checked != want {
		t.Fatalf("宽松帧覆盖 = %d，期望全部 %d 帧（23 帧 - 2 严格帧）", checked, want)
	}
}

// TestHelloFactsAndProbesKeys 钉 D-I13 的嵌套 wire 字段名：
// facts{hostIP,hostname,os,arch,cpu,workRoot,homeDir}、
// capabilities[]{name,version,digest,commands[]{name,found,path}}、
// instances[]{slot,concurrency,busy}。
//
// 这些字段是「中枢回答 NodesAvailable 的唯一输入」，键名漂一个就是节点报的东西
// 中枢读不到——而两侧都编译通过。所以单独一条快照，不复用顶层帧快照。
func TestHelloFactsAndProbesKeys(t *testing.T) {
	hello := &HelloFrame{
		Type:         FrameHello,
		Name:         "node-a",
		Token:        "tok",
		AgentVersion: "1.4.0",
		AgentCommit:  "3fd5a59",
		Facts: NodeFacts{
			HostIP: "10.0.0.7", Hostname: "node-a", OS: "linux", Arch: "amd64",
			CPU: 8, WorkRoot: "/var/lib/anode/runs", HomeDir: "/home/anode",
		},
		Capabilities: []HelloCapability{{
			Name: "review", Version: 2, Digest: "sha256:abc",
			Commands: []HelloCommand{{Name: "git", Found: true, Path: "/usr/bin/git"}},
		}},
		Instances: []HelloInstance{{Slot: intPtrTest(1), Busy: boolPtrTest(false)}},
	}
	raw, err := Encode(hello)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// facts 的键集合（全部必填：节点事实是定长的一组，不是可选扩展）。
	var facts map[string]json.RawMessage
	if err := json.Unmarshal(m["facts"], &facts); err != nil {
		t.Fatalf("facts 不是对象：%s", m["facts"])
	}
	wantFacts := []string{"arch", "cpu", "homeDir", "hostIP", "hostname", "os", "workRoot"}
	gotFacts := make([]string, 0, len(facts))
	for k := range facts {
		gotFacts = append(gotFacts, k)
	}
	sort.Strings(gotFacts)
	if !reflect.DeepEqual(gotFacts, wantFacts) {
		t.Fatalf("facts 键 = %v，期望 %v", gotFacts, wantFacts)
	}

	// capabilities[] 的元素键 + commands[] 的元素键。
	var caps []map[string]json.RawMessage
	if err := json.Unmarshal(m["capabilities"], &caps); err != nil || len(caps) != 1 {
		t.Fatalf("capabilities 形状不对：%s", m["capabilities"])
	}
	for _, k := range []string{"name", "version", "digest", "commands"} {
		if _, ok := caps[0][k]; !ok {
			t.Fatalf("capabilities[0] 缺字段 %q", k)
		}
	}
	var cmds []map[string]json.RawMessage
	if err := json.Unmarshal(caps[0]["commands"], &cmds); err != nil || len(cmds) != 1 {
		t.Fatalf("commands 形状不对：%s", caps[0]["commands"])
	}
	for _, k := range []string{"name", "found", "path"} {
		if _, ok := cmds[0][k]; !ok {
			t.Fatalf("commands[0] 缺字段 %q", k)
		}
	}

	// instances[] 的元素键。
	var insts []map[string]json.RawMessage
	if err := json.Unmarshal(m["instances"], &insts); err != nil || len(insts) != 1 {
		t.Fatalf("instances 形状不对：%s", m["instances"])
	}
	for _, k := range []string{"slot", "busy"} {
		if _, ok := insts[0][k]; !ok {
			t.Fatalf("instances[0] 缺字段 %q", k)
		}
	}

	// 反序列化必须把嵌套结构完整还原（round-trip 的深一层校验）。
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got, ok := decoded.(*HelloFrame)
	if !ok {
		t.Fatalf("Decode 返回 %T", decoded)
	}
	if got.Facts.HostIP != hello.Facts.HostIP || got.Facts.CPU != hello.Facts.CPU {
		t.Fatalf("facts 往返丢了：%+v", got.Facts)
	}
	if len(got.Capabilities) != 1 || len(got.Capabilities[0].Commands) != 1 ||
		!got.Capabilities[0].Commands[0].Found {
		t.Fatalf("capabilities 往返丢了：%+v", got.Capabilities)
	}
	if len(got.Instances) != 1 || got.Instances[0].Slot == nil || *got.Instances[0].Slot != 1 ||
		got.Instances[0].Busy == nil || *got.Instances[0].Busy {
		t.Fatalf("instances 往返丢了：%+v", got.Instances)
	}
}

// TestTaskResultPayloadKeys 单独钉 result{summary,artifacts,error} 与
// error{reason,message} 的 wire 键（它们是任务结果的对外形状）。
func TestTaskResultPayloadKeys(t *testing.T) {
	raw, err := Encode(&TaskResult{
		Summary:   "s",
		Artifacts: []TaskArtifact{{Path: "p", SHA256: "h"}},
		Error:     &TaskResultError{Reason: ReasonScriptFailed, Message: "m"},
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"summary", "artifacts", "error"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("result 缺字段 %q：%s", k, raw)
		}
	}
	var errObj map[string]json.RawMessage
	if err := json.Unmarshal(m["error"], &errObj); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	for _, k := range []string{"reason", "message"} {
		if _, ok := errObj[k]; !ok {
			t.Fatalf("result.error 缺字段 %q", k)
		}
	}
	var artifact map[string]json.RawMessage
	var arts []map[string]json.RawMessage
	if err := json.Unmarshal(m["artifacts"], &arts); err != nil || len(arts) != 1 {
		t.Fatalf("artifacts 形状不对：%s", m["artifacts"])
	}
	artifact = arts[0]
	for _, k := range []string{"path", "sha256"} {
		if _, ok := artifact[k]; !ok {
			t.Fatalf("artifacts 元素缺字段 %q", k)
		}
	}
}

// TestDecodeUnknownType 未知 type 不是错误：返回 UnknownFrame，由调用方回错误帧。
func TestDecodeUnknownType(t *testing.T) {
	got, err := Decode([]byte(`{"type":"task.assign","task_id":"x"}`))
	if err != nil {
		t.Fatalf("Decode 未知 type 不该报错：%v", err)
	}
	unknown, ok := got.(*UnknownFrame)
	if !ok {
		t.Fatalf("返回 %T，期望 *UnknownFrame", got)
	}
	if unknown.Type != "task.assign" {
		t.Fatalf("UnknownFrame.Type = %q", unknown.Type)
	}
	if IsUpstream(unknown.Type) || IsDownstream(unknown.Type) {
		t.Fatal("未知帧不该有方向")
	}
}

// TestDecodeBadJSON 坏 JSON 是错误（对应 bad_frame）。
func TestDecodeBadJSON(t *testing.T) {
	if _, err := Decode([]byte(`{"type":`)); err == nil {
		t.Fatal("坏 JSON 必须报错")
	}
	if _, err := FrameTypeOf([]byte(`not json`)); err == nil {
		t.Fatal("FrameTypeOf 坏 JSON 必须报错")
	}
}

// ---------- emit 网关 ----------

// TestEmitPrunedRoundTrip 剪枝回执必须写 taskId:null（不是省略、不是空串）。
func TestEmitPrunedRoundTrip(t *testing.T) {
	raw, err := Encode(&EmitResponse{TaskID: nil, EventID: "evt_1", To: "fixer", Version: 2,
		Pruned: &EmitPruned{Reason: ReasonParamMissing, Detail: "需要 file"}})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"taskId":null`)) {
		t.Fatalf("剪枝回执必须带 taskId:null：%s", raw)
	}
	var back EmitResponse
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.TaskID != nil {
		t.Fatalf("剪枝回执的 taskId 应为 nil，实际 %q", *back.TaskID)
	}
	if back.Pruned == nil || back.Pruned.Reason != ReasonParamMissing {
		t.Fatalf("剪枝原因丢了：%+v", back.Pruned)
	}

	ok, err := Encode(&EmitResponse{TaskID: testStr("tsk_1"), To: "fixer", Version: 2})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Contains(ok, []byte(`"taskId":"tsk_1"`)) {
		t.Fatalf("已派发回执必须有 taskId：%s", ok)
	}
	if bytes.Contains(ok, []byte("pruned")) {
		t.Fatalf("已派发回执不该带 pruned：%s", ok)
	}
}

// TestEmitRequestShape 请求只有 {to,id?,data}：凭据走头，不进 payload。
func TestEmitRequestShape(t *testing.T) {
	raw, err := Encode(&EmitRequest{To: "fixer", ID: "review-a.go", Data: json.RawMessage(`{"file":"a.go"}`)})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"data", "id", "to"}) {
		t.Fatalf("emit 请求键 = %v，期望 [data id to]", keys)
	}
	if strings.Contains(string(raw), "token") {
		t.Fatalf("凭据不许进 payload：%s", raw)
	}

	// 不带 id / data 时只剩 to。
	bare, err := Encode(&EmitRequest{To: "fixer"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if string(bare) != `{"to":"fixer"}` {
		t.Fatalf("不带 id/data 的请求 = %s", bare)
	}
}

// TestEmitExitCodes 退出码判据（capability.md §4.1）。
func TestEmitExitCodes(t *testing.T) {
	cases := map[string]int{
		"":                      EmitExitOK,
		ReasonTargetNotDeclared: EmitExitBadRequest,
		ReasonMissingTarget:     EmitExitBadRequest,
		ReasonTokenInvalid:      EmitExitTokenInvalid,
		ReasonTaskNotRunning:    EmitExitTaskNotRunning,
		ReasonEmitIDConflict:    EmitExitIDConflict,
		ReasonEmitUnreachable:   EmitExitUnknown,
		"something_unexpected":  EmitExitUnknown,
	}
	for reason, want := range cases {
		if got := EmitExitCode(reason); got != want {
			t.Fatalf("EmitExitCode(%q) = %d，期望 %d", reason, got, want)
		}
	}
	// 退出码本身是文档判据，值不能漂。
	if EmitExitOK != 0 || EmitExitBadRequest != 2 || EmitExitTokenInvalid != 3 ||
		EmitExitTaskNotRunning != 4 || EmitExitUnknown != 5 || EmitExitIDConflict != 6 {
		t.Fatal("emit 退出码常量漂了")
	}

	status := map[string]int{
		"":                      200,
		ReasonTargetNotDeclared: 400,
		ReasonMissingTarget:     400,
		ReasonTokenInvalid:      401,
		ReasonTaskNotRunning:    409,
		ReasonEmitIDConflict:    409,
	}
	for reason, want := range status {
		if got := EmitHTTPStatus(reason); got != want {
			t.Fatalf("EmitHTTPStatus(%q) = %d，期望 %d", reason, got, want)
		}
	}
}

// TestEmitAuthWire 凭据走 Authorization: Bearer。
func TestEmitAuthWire(t *testing.T) {
	if EmitAuthHeader != "Authorization" || EmitAuthScheme != "Bearer" || EmitPath != "/emit" {
		t.Fatalf("emit 网关 wire 常量漂了：%s / %s / %s", EmitAuthHeader, EmitAuthScheme, EmitPath)
	}
}

// ---------- 枚举与封闭集合 ----------

// TestTaskPhases 相位小写、六态、终态判定。
func TestTaskPhases(t *testing.T) {
	phases := []TaskPhase{TaskPhasePending, TaskPhaseRunning, TaskPhaseSucceeded, TaskPhaseFailed, TaskPhaseCancelled, TaskPhaseAbandoned}
	want := []string{"pending", "running", "succeeded", "failed", "cancelled", "abandoned"}
	for i, p := range phases {
		if string(p) != want[i] {
			t.Fatalf("相位 %d = %q，期望 %q（wire 必须小写）", i, p, want[i])
		}
		if !p.Valid() {
			t.Fatalf("%q 应当是合法相位", p)
		}
	}
	if TaskPhase("running ").Valid() || TaskPhase("").Valid() || TaskPhase("Succeeded").Valid() {
		t.Fatal("Valid() 放过了非法相位")
	}
	if TaskPhasePending.Terminal() || TaskPhaseRunning.Terminal() {
		t.Fatal("pending/running 不是终态")
	}
	for _, p := range []TaskPhase{TaskPhaseSucceeded, TaskPhaseFailed, TaskPhaseCancelled, TaskPhaseAbandoned} {
		if !p.Terminal() {
			t.Fatalf("%q 应当是终态", p)
		}
	}
}

// TestWaitingReasons 等待态封闭集合。
func TestWaitingReasons(t *testing.T) {
	want := []string{"waiting_for_slot", "retry_backoff", "waiting_for_config"}
	got := []WaitingReason{WaitingForSlot, WaitingRetryBackoff, WaitingForConfig}
	for i, w := range got {
		if string(w) != want[i] {
			t.Fatalf("等待原因 %d = %q，期望 %q", i, w, want[i])
		}
		if !w.Valid() {
			t.Fatalf("%q 应当合法", w)
		}
	}
	if WaitingReason("waiting").Valid() {
		t.Fatal("Valid() 放过了非法等待原因")
	}
}

// TestCancelReasons 取消原因三类。
func TestCancelReasons(t *testing.T) {
	if string(CancelReasonUser) != ReasonCancelledByUser {
		t.Fatalf("用户取消 reason = %q", CancelReasonUser)
	}
	if string(CancelReasonAbandonedByUser) != ReasonAbandonedByUser {
		t.Fatalf("用户放弃 reason = %q", CancelReasonAbandonedByUser)
	}
	if string(CancelReasonInfra) != "abandoned" {
		t.Fatalf("基础设施放弃 reason = %q", CancelReasonInfra)
	}
	for _, c := range []CancelReason{CancelReasonUser, CancelReasonAbandonedByUser, CancelReasonInfra} {
		if !c.Valid() {
			t.Fatalf("%q 应当合法", c)
		}
	}
	if CancelReason("abandoned_by_user ").Valid() || CancelReason("").Valid() {
		t.Fatal("Valid() 放过了非法取消原因")
	}
}

// TestConditionReasons 四个 condition type 与各自的 reason 词表（封闭集合）。
func TestConditionReasons(t *testing.T) {
	want := map[string][]string{
		ConditionImplemented:        {"Valid", "Invalid"},
		ConditionNodesAvailable:     {"Valid", "NoMatchingNode", "CommandMissing", "ValueMissing", "ConfigPending", "NodeOffline", "NamespaceUnauthorized"},
		ConditionDownstreamResolved: {"Valid", "NotFound"},
		ConditionDeployed:           {"Valid", "NotDeployed", "Stale"},
	}
	if len(want) != 4 {
		t.Fatal("condition 只有四个 type")
	}
	for typ, reasons := range want {
		got := ConditionReasons(typ)
		if !reflect.DeepEqual(got, reasons) {
			t.Fatalf("%s 的 reason 词表 = %v，期望 %v", typ, got, reasons)
		}
		for _, r := range reasons {
			if !IsConditionReason(typ, r) {
				t.Fatalf("%s/%s 应当合法", typ, r)
			}
		}
	}
	// 跨 type 的 reason 不许串用。
	if IsConditionReason(ConditionImplemented, ConditionReasonNotFound) {
		t.Fatal("Implemented 不该接受 NotFound")
	}
	if IsConditionReason("Unknown", ConditionReasonValid) {
		t.Fatal("未知 condition type 不该接受任何 reason")
	}
	if IsConditionReason(ConditionDeployed, "NotReady") {
		t.Fatal("Deployed 不该接受 NotReady")
	}
	if ConditionStatusTrue != "True" || ConditionStatusFalse != "False" {
		t.Fatal("condition status 是字符串 \"True\"/\"False\"")
	}
}

// TestReasonSets 失败 / 剪枝 / 重试 / reject 的封闭集合关系。
func TestReasonSets(t *testing.T) {
	for _, r := range PruneReasons() {
		if !IsPruneReason(r) {
			t.Fatalf("%q 应当出现在剪枝集合里", r)
		}
	}
	// no_target 只表示源没有 targets；emit 缺 to 是 missing_target（不是剪枝）。
	if IsPruneReason(ReasonMissingTarget) {
		t.Fatal("missing_target 不是剪枝原因")
	}
	if IsPruneReason(ReasonStaleConfig) {
		t.Fatal("stale_config 不是剪枝原因")
	}
	for _, r := range []string{ReasonNodeLost, ReasonLeaseExpired, ReasonInfraTransient} {
		if !IsRetryableInfra(r) {
			t.Fatalf("%q 应当是可重试的短暂基础设施失败", r)
		}
	}
	// 脚本非零退出、超时、取消、确定性配置错误都不自动重试。
	for _, r := range []string{ReasonScriptFailed, ReasonFilesEscape, ReasonWorkDirUnavailable,
		ReasonHomeUnavailable, ReasonMissingRequirement, ReasonCancelledByUser,
		ReasonAbandonedByUser, ReasonMaxAttemptsExceeded, ReasonNoTarget} {
		if IsRetryableInfra(r) {
			t.Fatalf("%q 不应当被自动重试", r)
		}
	}
	// task.reject 的理由只有 stale_config。
	if !IsTaskRejectReason(ReasonStaleConfig) {
		t.Fatal("stale_config 是 task.reject 的合法理由")
	}
	for _, r := range []string{ReasonNodeLost, ReasonMissingRequirement, ConditionReasonValueMissing} {
		if IsTaskRejectReason(r) {
			t.Fatalf("%q 不该出现在 task.reject 里", r)
		}
	}
}

// TestLeaseConstants 租约 60/20/5 全目录一致。
func TestLeaseConstants(t *testing.T) {
	if LeaseTTL != 60*time.Second {
		t.Fatalf("LeaseTTL = %v，期望 60s", LeaseTTL)
	}
	if LeaseRenewInterval != 20*time.Second {
		t.Fatalf("LeaseRenewInterval = %v，期望 20s", LeaseRenewInterval)
	}
	if LeaseTermGrace != 5*time.Second {
		t.Fatalf("LeaseTermGrace = %v，期望 5s", LeaseTermGrace)
	}
	if LeaseRenewInterval >= LeaseTTL {
		t.Fatal("续租间隔必须小于租约时长")
	}
	if RetryBackoffBase != 5*time.Second || RetryBackoffMax != 5*time.Minute || RetryBackoffFactor != 2 {
		t.Fatal("attempt 退避参数（5s 起、×2、上限 5m）漂了")
	}
	if StaleConfigBackoffBase != 5*time.Second || StaleConfigBackoffMax != 60*time.Second {
		t.Fatal("stale_config 退避参数（5s 起、上限 60s）漂了")
	}
}

// TestFrameErrorCodes 帧级错误码是封闭集合，且与任务失败码不混用。
func TestFrameErrorCodes(t *testing.T) {
	for _, c := range []string{ErrCodeBadFrame, ErrCodeUnknownType, ErrCodeInvalidTransition,
		ErrCodeProtocolMismatch, ErrCodeFrameTooLarge, ErrCodeParamsTooLarge} {
		if !IsFrameErrorCode(c) {
			t.Fatalf("%q 应当是帧级错误码", c)
		}
	}
	if IsFrameErrorCode(ReasonScriptFailed) {
		t.Fatal("script_failed 是任务失败码，不是帧级错误码")
	}
	if FrameSoftLimit != 1<<20 || LogsResponseMaxBytes != FrameSoftLimit {
		t.Fatal("帧软上限必须是 1 MiB，且 logs_response 与它同值")
	}
}

// TestUpgradeErrorNormalize ack 里的未知码收敛成空（封闭集合出线）。
func TestUpgradeErrorNormalize(t *testing.T) {
	for _, c := range []string{UpgradeErrNeedRoot, UpgradeErrBadSHA, UpgradeErrDownloadFailed, UpgradeErrNotSupported} {
		if NormalizeUpgradeError(c) != c {
			t.Fatalf("%q 应当原样保留", c)
		}
	}
	if NormalizeUpgradeError("whatever") != "" {
		t.Fatal("未知升级错误码必须收敛为空")
	}
	if NodeBinaryPath == "" {
		t.Fatal("NodeBinaryPath 不能为空")
	}
}

// ---------- applied 文件形状 ----------

// TestAppliedConfigShape D-I4：{revision,hash,vars,limits}，0600，文件名固定。
// 字段名就是 wire 名（JSON 是 YAML 1.2 子集，写出来仍是合法 YAML）。
func TestAppliedConfigShape(t *testing.T) {
	cfg := &AppliedConfig{
		Revision: "7",
		Hash:     "abc123",
		Vars:     map[string]string{"SOURCE_ROOT": "/srv/src"},
		Limits:   Limits{Total: 8, PerCapability: map[string]int{"review": 2}},
	}
	raw, err := Encode(cfg)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"hash", "limits", "revision", "vars"}) {
		t.Fatalf("applied 文件键 = %v，期望 [hash limits revision vars]", keys)
	}
	if AppliedFileName != "anode.applied.yaml" {
		t.Fatalf("applied 文件名 = %q", AppliedFileName)
	}
	if AppliedFileMode != 0o600 {
		t.Fatalf("applied 文件权限 = %#o，期望 0600", AppliedFileMode)
	}
}

// ---------- 零依赖 ----------

// TestNoProjectDependencies 是 D-I10 的守卫：protocol 只用 stdlib。
//
// 用 `go list -deps` 而不是人工看 import：节点二进制不能链 sqlite，而这条约束一旦
// 被一次「顺手 import store」破掉，症状是节点体积暴涨 + 交叉编译需要 CGO 工具链。
func TestNoProjectDependencies(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("找不到 go（协议零依赖检查需要它）：%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, goBin, "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	const self = "github.com/zzjcool/anotify/internal/protocol"
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if dep == "" || dep == self {
			continue
		}
		if strings.HasPrefix(dep, "github.com/zzjcool/anotify") {
			t.Fatalf("internal/protocol 不许 import 本项目其它包，实际依赖 %q", dep)
		}
		if strings.Contains(dep, "sqlite") {
			t.Fatalf("internal/protocol 不许链 sqlite，实际依赖 %q", dep)
		}
	}
}

// intPtrTest / boolPtrTest 是 presence 字段的测试夹具（0 / false 都是合法值）。
func intPtrTest(v int) *int    { return &v }
func boolPtrTest(v bool) *bool { return &v }
