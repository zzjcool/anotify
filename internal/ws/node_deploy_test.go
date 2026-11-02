package ws

// v2 新增的两帧（deploy 下行 / apply 上行）与部署推送通道的协议层验收。
//
// 契约见 .pi-orchestrator/console-declares/CONTRACT-protocol.md：
//   §2 帧清单把总帧数从 9 扩到 11（+ deploy / + apply）；
//   §4 deploy 携带 canonical JSON 产物（action=apply|remove）；
//   §5 apply 回报部署结果（state=applied|failed + reason 短码）；
//   §6 assign 增加 digest / trace_id 两个可选旁路字段。
//
// 这里只测**协议层**（编解码 / 方向表 / 推送通道）。「apply 落库成 deployments.state」
// 属于 handler 层的接线，不在本文件（见报告里的产品偏差）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestDecodeUpstreamApplyFrame apply 是上行帧：DecodeUpstream 必须解出全部字段。
//
// 为什么这条重要：帧类型字符串在 switch 里漏一个 case 时，行为不是报错而是
// 返回 *UnknownFrame（协议设计如此：未知类型回 unknown_type 而不是断连），
// 于是「节点回报的部署结果被静默丢掉」——这必须有测试钉住。
func TestDecodeUpstreamApplyFrame(t *testing.T) {
	raw := `{"type":"apply","deploy_id":"dp_1","capability":"review","version":3,` +
		`"digest":"sha256:abc","state":"failed","reason":"missing_requirement:claude"}`
	got, err := DecodeUpstream([]byte(raw))
	if err != nil {
		t.Fatalf("DecodeUpstream: %v", err)
	}
	f, ok := got.(*ApplyFrame)
	if !ok {
		t.Fatalf("DecodeUpstream 返回 %T, want *ApplyFrame", got)
	}
	if f.Type != NodeFrameApply || f.DeployID != "dp_1" || f.Capability != "review" || f.Version != 3 ||
		f.Digest != "sha256:abc" || f.State != ApplyStateFailed ||
		f.Reason != ApplyReasonMissingRequirement+":claude" {
		t.Fatalf("apply 帧字段解析不对: %+v", f)
	}
}

// TestDecodeUpstreamApplyMinimal 只有必填字段的 apply 也要能解（节点想省字段时不该解不开）。
func TestDecodeUpstreamApplyMinimal(t *testing.T) {
	got, err := DecodeUpstream([]byte(`{"type":"apply","deploy_id":"dp_2","state":"applied"}`))
	if err != nil {
		t.Fatalf("DecodeUpstream: %v", err)
	}
	f, ok := got.(*ApplyFrame)
	if !ok {
		t.Fatalf("DecodeUpstream 返回 %T, want *ApplyFrame", got)
	}
	if f.State != ApplyStateApplied || f.DeployID != "dp_2" || f.Reason != "" {
		t.Fatalf("最小 apply 帧解析不对: %+v", f)
	}
}

// TestDeployFrameRoundTrip deploy 帧（含产物原文）编码后再解码，产物字节不变。
//
// 为什么要比字节：产物是内容寻址的（digest = sha256(canonical JSON)），中途中转
// 只要多/少一个字节，节点算出来的 digest 就与中枢的不一致 → 每次部署都被判成
// 「本地变体」。所以「原样转发」是这个帧唯一的正确性要求。
func TestDeployFrameRoundTrip(t *testing.T) {
	artifact := `{"apiVersion":"anotify/v1","kind":"Capability",` +
		`"metadata":{"name":"review","version":1},"spec":{"outputs":[],"run":{"entrypoint":"run.sh","files":{"run.sh":"#!/bin/bash\necho hi\n"}}}}`
	f := &DeployFrame{
		Type:     NodeFrameDeploy,
		DeployID: "dp_rt",
		Action:   DeployActionApply,
		Artifact: json.RawMessage(artifact),
	}
	data, err := Encode(f)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// 中枢侧没有下行解码器（解码方是节点），所以这里用与节点侧等价的 json 解码。
	var back DeployFrame
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("解码 deploy 帧: %v", err)
	}
	if back.DeployID != "dp_rt" || back.Action != DeployActionApply {
		t.Fatalf("deploy 帧字段丢了: %+v", back)
	}
	if !bytes.Equal(back.Artifact, f.Artifact) {
		t.Fatalf("产物原文被改写了:\n want: %s\n  got: %s", f.Artifact, back.Artifact)
	}
	// remove 也必须带产物（契约 §4 定稿：DeployFrame 没有 capability/version 字段，
	// 节点只能靠产物里的 metadata 知道要摘掉哪一条缓存索引）。
	//
	// 为什么这条断言值得写：`Artifact` 的 json tag 是 omitempty，一旦有人把
	// remove 路径的产物填成 nil，帧照样能编码、节点照样收到 —— 然后它只能回
	// apply{failed, reason="invalid_artifact"}（不猜名字去删）。这里钉住「线上形状
	// 里 artifact 在场」，让这类回归在协议层就红掉。
	rm := &DeployFrame{
		Type: NodeFrameDeploy, DeployID: "dp_rm", Action: DeployActionRemove,
		Artifact: json.RawMessage(artifact),
	}
	data, err = Encode(rm)
	if err != nil {
		t.Fatalf("Encode remove: %v", err)
	}
	var rmBack DeployFrame
	if err := json.Unmarshal(data, &rmBack); err != nil {
		t.Fatalf("解码 remove 帧: %v", err)
	}
	if rmBack.Action != DeployActionRemove {
		t.Fatalf("remove 帧 action = %q, want %q", rmBack.Action, DeployActionRemove)
	}
	if !bytes.Equal(rmBack.Artifact, json.RawMessage(artifact)) {
		t.Fatalf("remove 帧没带（或被改写了）产物，节点无从定位要摘掉的缓存:\n want: %s\n  got: %s",
			artifact, rmBack.Artifact)
	}
	// 产物里的 metadata 是节点唯一的定位依据，必须解析得出来
	var meta struct {
		Metadata struct {
			Name    string `json:"name"`
			Version int    `json:"version"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(rmBack.Artifact, &meta); err != nil {
		t.Fatalf("产物 metadata 解不出来: %v", err)
	}
	if meta.Metadata.Name != "review" || meta.Metadata.Version != 1 {
		t.Fatalf("产物 metadata = %+v, want review@1", meta.Metadata)
	}
}

// TestFrameDirectionTable 方向表必须与帧清单一一对应（11 帧，每帧恰好一个方向）。
//
// 方向表是「节点没有发下行帧」这条断言的判据（nodeSession 用它决定是否回
// unknown_type）。漏一帧的后果是：该帧被当成方向错误 → 合法的上行帧被拒。
func TestFrameDirectionTable(t *testing.T) {
	upstream := []string{
		NodeFrameHello, NodeFrameHeartbeat, NodeFrameAck,
		NodeFrameProgress, NodeFrameResult, NodeFrameApply,
	}
	downstream := []string{
		NodeFrameWelcome, NodeFrameAssign, NodeFrameDeploy,
		NodeFrameCancel, NodeFrameError,
	}
	if got := len(upstream) + len(downstream); got != 11 {
		t.Fatalf("帧清单 = %d 帧, want 11（契约 §2）", got)
	}
	for _, f := range upstream {
		if !IsUpstream(f) {
			t.Errorf("%q 不是上行帧", f)
		}
		if IsDownstream(f) {
			t.Errorf("%q 同时被算成下行帧（方向表重叠）", f)
		}
	}
	for _, f := range downstream {
		if !IsDownstream(f) {
			t.Errorf("%q 不是下行帧", f)
		}
		if IsUpstream(f) {
			t.Errorf("%q 同时被算成上行帧（方向表重叠）", f)
		}
	}
	// v2 的两个新帧各就各位
	if !IsUpstream(NodeFrameApply) {
		t.Error("apply 必须是上行帧（节点 → 中枢）")
	}
	if !IsDownstream(NodeFrameDeploy) {
		t.Error("deploy 必须是下行帧（中枢 → 节点）")
	}
	if IsUpstream("apply ") || IsDownstream("deploy ") {
		t.Error("方向表对带空格的名字也返回 true（帧名比较写错了）")
	}
}

// TestAssignFrameCarriesDigestAndTraceID v2 给 assign 加了两个可选旁路字段（契约 §6）。
//
// 空的 digest / trace_id 必须**不出现**在线上 JSON 里：老节点按 capability@version
// 找缓存，多出来的空字段只会让日志更难读。
func TestAssignFrameCarriesDigestAndTraceID(t *testing.T) {
	f := &AssignFrame{
		Type: NodeFrameAssign, TaskID: "tsk_1", Capability: "review", Version: 1,
		Params: map[string]any{}, LeaseUntil: 100, MaxDuration: 60,
		Digest: "sha256:abc", TraceID: "trc_1",
	}
	data, err := Encode(f)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for _, want := range []string{`"digest":"sha256:abc"`, `"trace_id":"trc_1"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("assign 帧缺 %s: %s", want, data)
		}
	}
	// 缺省（老派发路径不传）时两个字段一个字节都不出现
	bare, err := Encode(&AssignFrame{Type: NodeFrameAssign, TaskID: "tsk_2", Capability: "review", Version: 1})
	if err != nil {
		t.Fatalf("Encode bare: %v", err)
	}
	if strings.Contains(string(bare), "digest") || strings.Contains(string(bare), "trace_id") {
		t.Fatalf("空的 digest/trace_id 被序列化出来了: %s", bare)
	}
}

// TestPublishDeployOfflineNode 节点没有活跃连接 → ErrNodeOffline（调用方据此标「已排队」）。
func TestPublishDeployOfflineNode(t *testing.T) {
	f := newNodeFixture(t)

	err := f.handler.hub().PublishDeploy(f.nsID, f.node.ID, &DeployFrame{
		Type: NodeFrameDeploy, DeployID: "dp_offline", Action: DeployActionApply,
	})
	if !errors.Is(err, ErrNodeOffline) {
		t.Fatalf("离线节点推部署 err = %v, want ErrNodeOffline", err)
	}
	// 不存在的节点同样是 ErrNodeOffline（不 panic、不静默成功）
	if err := f.handler.hub().PublishDeploy(f.nsID, "nd_does_not_exist", &DeployFrame{
		Type: NodeFrameDeploy, DeployID: "dp_ghost", Action: DeployActionApply,
	}); !errors.Is(err, ErrNodeOffline) {
		t.Fatalf("未知节点推部署 err = %v, want ErrNodeOffline", err)
	}
}

// TestPublishDeployCrossNamespaceRejected 绝不跨命名空间推部署（不变量 I10/I11）。
//
// 跨命名空间推成功 = 把 A 隔离域的产物内容送进 B 隔离域，是隔离边界的直接破口。
func TestPublishDeployCrossNamespaceRejected(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	otherNS := newOtherNamespace(t, f)

	err := f.handler.hub().PublishDeploy(otherNS, f.node.ID, &DeployFrame{
		Type: NodeFrameDeploy, DeployID: "dp_cross", Action: DeployActionApply,
	})
	if err == nil {
		t.Fatal("跨命名空间推部署成功了（隔离边界破了）")
	}
	if !strings.Contains(err.Error(), "another namespace") {
		t.Fatalf("错误信息 = %q, want 说明是跨命名空间", err.Error())
	}
	// 连接上不该出现任何帧（连错误帧都不该有：这是调用方的参数错误，不是协议错误）
	assertNoFrame(t, conn, 300*time.Millisecond, "跨命名空间的部署不该送到这条连接上")
}

// TestPublishDeployDeliversFrame 在线节点收到 deploy 帧：type / action / deploy_id /
// 产物原文全部原样到达。
//
// 这是「控制台点部署 → 节点真的收到」这条链路上唯一一段在中枢侧可测的部分
// （节点侧的应用逻辑在 cmd/agent）。
func TestPublishDeployDeliversFrame(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	artifact := `{"apiVersion":"anotify/v1","kind":"Capability","metadata":{"name":"review","version":1}}`
	if err := f.handler.hub().PublishDeploy(f.nsID, f.node.ID, &DeployFrame{
		Type:     NodeFrameDeploy,
		DeployID: "dp_live",
		Action:   DeployActionApply,
		Artifact: json.RawMessage(artifact),
	}); err != nil {
		t.Fatalf("PublishDeploy: %v", err)
	}

	frame := readUntil(t, conn, NodeFrameDeploy)
	if frame["deploy_id"] != "dp_live" {
		t.Fatalf("deploy_id = %v, want dp_live", frame["deploy_id"])
	}
	if frame["action"] != DeployActionApply {
		t.Fatalf("action = %v, want %q", frame["action"], DeployActionApply)
	}
	// artifact 在帧里是**对象**（RawMessage 原样嵌入），不是被转义的字符串
	art, ok := frame["artifact"].(map[string]any)
	if !ok {
		t.Fatalf("artifact 不是对象（被当成字符串转义了）: %T %v", frame["artifact"], frame["artifact"])
	}
	if md, _ := art["metadata"].(map[string]any); md["name"] != "review" {
		t.Fatalf("artifact 内容被改写: %v", art)
	}
}

// newOtherNamespace 在同一个部署里再建一个命名空间（跨命名空间用例）。
func newOtherNamespace(t *testing.T, f *nodeFixture) string {
	t.Helper()
	ns, err := f.db.CreateNamespace(context.Background(), "staging", "staging")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return ns.ID
}
