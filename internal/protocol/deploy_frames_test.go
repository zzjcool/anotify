package protocol

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestDeployFrameNamesAndDirection 钉死部署两帧的 wire 名与方向：
// capability.deploy 中枢 → 节点（下行），capability.deployed 节点 → 中枢（上行）。
func TestDeployFrameNamesAndDirection(t *testing.T) {
	if FrameCapabilityDeploy != "capability.deploy" {
		t.Fatalf("FrameCapabilityDeploy = %q", FrameCapabilityDeploy)
	}
	if FrameCapabilityDeployed != "capability.deployed" {
		t.Fatalf("FrameCapabilityDeployed = %q", FrameCapabilityDeployed)
	}
	if !IsDownstream(FrameCapabilityDeploy) {
		t.Fatal("capability.deploy 必须是中枢 → 节点（下行）")
	}
	if IsUpstream(FrameCapabilityDeploy) {
		t.Fatal("capability.deploy 不可能是上行")
	}
	if !IsUpstream(FrameCapabilityDeployed) {
		t.Fatal("capability.deployed 必须是节点 → 中枢（上行）")
	}
	if IsDownstream(FrameCapabilityDeployed) {
		t.Fatal("capability.deployed 不可能是下行")
	}
	// 两帧必须都进方向表（Decode 的 type 集合 = 上行 ∪ 下行）。
	if !containsString(UpstreamFrames(), FrameCapabilityDeployed) {
		t.Fatal("上行方向表缺 capability.deployed")
	}
	if !containsString(DownstreamFrames(), FrameCapabilityDeploy) {
		t.Fatal("下行方向表缺 capability.deploy")
	}
}

// deployKeys 把帧编码后的顶层 wire 键取出来排序。
func deployKeys(t *testing.T, frame any) []string {
	t.Helper()
	raw, err := Encode(frame)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal %T: %v", frame, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestCapabilityDeployFrameWire capability.deploy 只带 requestId + refs 四元组：
// 不夹带 Artifact 包体（files）、不夹带执行/租约字段。
func TestCapabilityDeployFrameWire(t *testing.T) {
	frame := &CapabilityDeployFrame{
		Type:      FrameCapabilityDeploy,
		RequestID: "dep_1",
		DeployRefs: DeployRefs{
			Namespace: "default",
			Name:      "review",
			Version:   3,
			Digest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
	}
	gotKeys := deployKeys(t, frame)
	wantKeys := []string{"digest", "name", "namespace", "requestId", "type", "version"}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("capability.deploy 键 = %v，期望 %v", gotKeys, wantKeys)
	}
	raw, err := Encode(frame)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// Artifact 整份文档不走 WS：包里没有 files / script / artifacts 这类字段。
	for _, banned := range []string{`"files"`, `"script"`, `"artifacts"`, `"yaml"`} {
		if bytes.Contains(raw, []byte(banned)) {
			t.Fatalf("capability.deploy 不该带 %s：%s", banned, raw)
		}
	}

	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got, ok := decoded.(*CapabilityDeployFrame)
	if !ok {
		t.Fatalf("Decode 返回 %T，期望 *CapabilityDeployFrame", decoded)
	}
	if !reflect.DeepEqual(*got, *frame) {
		t.Fatalf("往返丢了字段：\n got %+v\nwant %+v", *got, *frame)
	}
}

// TestCapabilityDeployedFrameWire capability.deployed 的 refs 与请求同形，
// 另带 status（applied|failed）与可选 error（空时省略）。
func TestCapabilityDeployedFrameWire(t *testing.T) {
	refs := DeployRefs{Namespace: "team-a", Name: "fixer", Version: 2, Digest: "sha256:abcd"}
	failed := &CapabilityDeployedFrame{
		Type:       FrameCapabilityDeployed,
		RequestID:  "dep_1",
		DeployRefs: refs,
		Status:     DeployStatusFailed,
		Error:      "digest mismatch",
	}
	gotKeys := deployKeys(t, failed)
	wantKeys := []string{"digest", "error", "name", "namespace", "requestId", "status", "type", "version"}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("capability.deployed 键 = %v，期望 %v", gotKeys, wantKeys)
	}
	raw, err := Encode(failed)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got, ok := decoded.(*CapabilityDeployedFrame)
	if !ok {
		t.Fatalf("Decode 返回 %T，期望 *CapabilityDeployedFrame", decoded)
	}
	if !reflect.DeepEqual(*got, *failed) {
		t.Fatalf("往返丢了字段：\n got %+v\nwant %+v", *got, *failed)
	}

	// 成功回执省略 error（wire 上真的没有这个键）。
	applied := &CapabilityDeployedFrame{
		Type:       FrameCapabilityDeployed,
		RequestID:  "dep_2",
		DeployRefs: refs,
		Status:     DeployStatusApplied,
	}
	appliedRaw, err := Encode(applied)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if bytes.Contains(appliedRaw, []byte(`"error"`)) {
		t.Fatalf("applied 回执不该带 error：%s", appliedRaw)
	}
	gotKeys = deployKeys(t, applied)
	wantKeys = []string{"digest", "name", "namespace", "requestId", "status", "type", "version"}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("applied 回执键 = %v，期望 %v", gotKeys, wantKeys)
	}
}

// TestDeployStatus 状态封闭集合：只有 applied / failed。
func TestDeployStatus(t *testing.T) {
	if DeployStatusApplied != "applied" || DeployStatusFailed != "failed" {
		t.Fatalf("部署状态 wire 值漂了：%q / %q", DeployStatusApplied, DeployStatusFailed)
	}
	for _, s := range []DeployStatus{DeployStatusApplied, DeployStatusFailed} {
		if !s.Valid() {
			t.Fatalf("%q 应当合法", s)
		}
	}
	for _, s := range []DeployStatus{"", "applying", "APPLIED", "success", "ok"} {
		if s.Valid() {
			t.Fatalf("%q 不该通过 Valid()", s)
		}
	}
}

// TestDeployFramesShareRefs 请求与回执的 refs 必须逐字段相同（同一四元组），
// requestId 也是同一个：中枢据此把回执配回部署意图，不靠 name 猜版本。
func TestDeployFramesShareRefs(t *testing.T) {
	refs := DeployRefs{
		Namespace: "default",
		Name:      "review",
		Version:   5,
		Digest:    "sha256:0123",
	}
	req := &CapabilityDeployFrame{Type: FrameCapabilityDeploy, RequestID: "dep_9", DeployRefs: refs}
	ack := &CapabilityDeployedFrame{Type: FrameCapabilityDeployed, RequestID: "dep_9", DeployRefs: refs, Status: DeployStatusApplied}

	reqRaw, err := Encode(req)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	ackRaw, err := Encode(ack)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	reqDecoded, err := Decode(reqRaw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	ackDecoded, err := Decode(ackRaw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	reqGot := reqDecoded.(*CapabilityDeployFrame)
	ackGot := ackDecoded.(*CapabilityDeployedFrame)
	if reqGot.DeployRefs != ackGot.DeployRefs {
		t.Fatalf("请求/回执 refs 不一致：%+v vs %+v", reqGot.DeployRefs, ackGot.DeployRefs)
	}
	if reqGot.RequestID != ackGot.RequestID {
		t.Fatalf("请求/回执 requestId 不一致：%q vs %q", reqGot.RequestID, ackGot.RequestID)
	}
	if !strings.Contains(string(reqRaw), `"digest":"sha256:0123"`) {
		t.Fatalf("digest 必须原样出现在 wire 上：%s", reqRaw)
	}
}
