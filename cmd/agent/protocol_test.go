package main

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/ws"
)

// 本文件的 golden 全部来自 docs/contracts/CONTRACT.md §6.1（内联常量，不读 testdata：
// 帧层测试不该依赖磁盘状态）。
//
// 重点不是「能解出字段」，而是**线上字节**与**服务端视角**：
// 上行帧的精确 JSON 由服务端 DecodeUpstream 回读验证，out 的三态由服务端
// ResultFrame.OutPresent/Reported() 判定——这样 tag 漂移会被服务端自己抓到。

func mustEncode(t *testing.T, f any) string {
	t.Helper()
	data, err := encodeFrame(f)
	if err != nil {
		t.Fatalf("encodeFrame(%T): %v", f, err)
	}
	return string(data)
}

// demoRun 是 node.yaml 里 run 块序列化后的紧凑 JSON（契约 §3.1.3：manifest.run = json.Marshal(runSpec)）。
const demoRun = `{"runner":"bash","command":"echo prep","timeoutSec":30,"concurrency":1,"env":{"LOG_LEVEL":"info"}}`

// demoHello 是契约 §4 的 hello golden（node.yaml 只有一条能力 demo.prep@1）。
func demoHello(t *testing.T) helloFrame {
	t.Helper()
	if !json.Valid([]byte(demoRun)) {
		t.Fatalf("demoRun is not valid JSON: %s", demoRun)
	}
	return helloFrame{
		Type:     frameTypeHello,
		NodeID:   "", // 空 = 由服务端按令牌认（node.yaml 不提供 node_id）
		Protocol: ws.ProtocolVersion,
		Capabilities: []capabilityDecl{
			{Capability: "demo.prep", Version: 1},
		},
		Manifest: []manifestDecl{
			{Capability: "demo.prep", Version: 1, Run: json.RawMessage(demoRun)},
		},
		Instances: []instanceDecl{{Slot: 0, Concurrency: 1}},
		Labels:    nil,
	}
}

// goldenHelloJSON 是 hello 的**精确**线上字节（字段顺序 = ws.HelloFrame 的字段顺序）。
//
// 注意两处容易写错但**按实际 tag** 必须如此的细节：
//   - node_id 没有 omitempty，空串照写 "node_id":""；
//   - labels 有 omitempty，nil 时整键不出现。
const goldenHelloJSON = `{"type":"hello","node_id":"","protocol":1,` +
	`"capabilities":[{"capability":"demo.prep","version":1}],` +
	`"manifest":[{"capability":"demo.prep","version":1,"run":` + demoRun + `}],` +
	`"instances":[{"slot":0,"concurrency":1}]}`

// ---------- W2 必测 1：三个上行帧 golden（精确字符串）----------

func TestFrameGoldenUpstream(t *testing.T) {
	tests := []struct {
		name string
		f    any
		want string
	}{
		{"hello", demoHello(t), goldenHelloJSON},
		{"ack", ackFrame{Type: frameTypeAck, TaskID: "tsk_x"},
			`{"type":"ack","task_id":"tsk_x"}`},
		{"heartbeat", heartbeatFrame{Type: frameTypeHeartbeat, TS: 1730000000},
			`{"type":"heartbeat","ts":1730000000}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mustEncode(t, tc.f)
			if got != tc.want {
				t.Fatalf("wire bytes mismatch\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// frameType* 必须**取值于** ws 的常量（不是本包重写的字面量），否则线上帧名与服务端对不上。
func TestFrameTypeConstantsMatchServer(t *testing.T) {
	tests := []struct{ name, got, want string }{
		{"hello", frameTypeHello, ws.NodeFrameHello},
		{"heartbeat", frameTypeHeartbeat, ws.NodeFrameHeartbeat},
		{"ack", frameTypeAck, ws.NodeFrameAck},
		{"progress", frameTypeProgress, ws.NodeFrameProgress},
		{"result", frameTypeResult, ws.NodeFrameResult},
		{"welcome", frameTypeWelcome, ws.NodeFrameWelcome},
		{"assign", frameTypeAssign, ws.NodeFrameAssign},
		{"cancel", frameTypeCancel, ws.NodeFrameCancel},
		{"error", frameTypeError, ws.NodeFrameError},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s frame type = %q, want %q (ws 常量)", tc.name, tc.got, tc.want)
		}
	}
	if statusSucceeded != "succeeded" || statusFailed != "failed" {
		t.Errorf("status constants = %q/%q, want succeeded/failed", statusSucceeded, statusFailed)
	}
}

func TestFrameEncodeError(t *testing.T) {
	if _, err := encodeFrame(make(chan int)); err == nil {
		t.Fatal("encodeFrame(chan) returned nil error, want an error")
	} else if !strings.Contains(err.Error(), "encode frame") {
		t.Fatalf("error = %q, want it to contain %q", err, "encode frame")
	}
}

// ---------- W2 必测 5：服务端可读性（hello）----------

// agent 造出来的 hello 必须能被服务端**自己的**解码器读回同构结构。
// 这是「tag 没漂」的最强证明：读的是 internal/ws 的 tag，不是本包的假设。
func TestHelloGoldenReadableByServer(t *testing.T) {
	raw := mustEncode(t, demoHello(t))
	got, err := ws.DecodeUpstream([]byte(raw))
	if err != nil {
		t.Fatalf("ws.DecodeUpstream(%s): %v", raw, err)
	}
	hf, ok := got.(*ws.HelloFrame)
	if !ok {
		t.Fatalf("decoded type = %T, want *ws.HelloFrame", got)
	}

	want := demoHello(t)
	if hf.Type != want.Type {
		t.Errorf("type = %q, want %q", hf.Type, want.Type)
	}
	if hf.NodeID != want.NodeID {
		t.Errorf("node_id = %q, want %q", hf.NodeID, want.NodeID)
	}
	if hf.Protocol != want.Protocol {
		t.Errorf("protocol = %d, want %d", hf.Protocol, want.Protocol)
	}
	if !reflect.DeepEqual(hf.Capabilities, want.Capabilities) {
		t.Errorf("capabilities = %+v, want %+v", hf.Capabilities, want.Capabilities)
	}
	if !reflect.DeepEqual(hf.Instances, want.Instances) {
		t.Errorf("instances = %+v, want %+v", hf.Instances, want.Instances)
	}
	if hf.Labels != nil {
		t.Errorf("labels = %+v, want nil (omitempty 键不出现)", hf.Labels)
	}

	// manifest 的 run 原文必须原样回来（中枢只存不执行，I1：字节不能被改写）
	if len(hf.Manifest) != len(want.Manifest) {
		t.Fatalf("manifest len = %d, want %d", len(hf.Manifest), len(want.Manifest))
	}
	for i, gotDecl := range hf.Manifest {
		wantDecl := want.Manifest[i]
		if gotDecl.Capability != wantDecl.Capability || gotDecl.Version != wantDecl.Version {
			t.Errorf("manifest[%d] = %s@%d, want %s@%d",
				i, gotDecl.Capability, gotDecl.Version, wantDecl.Capability, wantDecl.Version)
		}
		if !jsonEqual(t, gotDecl.Run, wantDecl.Run) {
			t.Errorf("manifest[%d].run = %s, want %s", i, gotDecl.Run, wantDecl.Run)
		}
	}

	// run 原文必须能被服务端侧的 runSpec 形状解码（runner=bash/command 非空，
	// 否则握手会被 invalid_manifest 拒掉）。
	var rs struct {
		Runner  string `json:"runner"`
		Command string `json:"command"`
	}
	if err := json.Unmarshal(hf.Manifest[0].Run, &rs); err != nil {
		t.Fatalf("manifest[0].run is not decodable: %v", err)
	}
	if rs.Runner != "bash" || rs.Command == "" {
		t.Errorf("manifest[0].run = %+v, want runner=bash with a non-empty command", rs)
	}
}

// ---------- W2 必测 2：resultFrame 的 out 三态（I5）----------

func TestResultFrameOutTriState(t *testing.T) {
	t.Run("out 缺席（没回报出口）", func(t *testing.T) {
		f := resultFrame{
			Type:    frameTypeResult,
			TaskID:  "tsk_x",
			Status:  statusSucceeded,
			Out:     nil,
			Summary: "ok",
			Error:   "",
		}
		got := mustEncode(t, f)
		want := `{"type":"result","task_id":"tsk_x","status":"succeeded","summary":"ok","artifacts":[],"error":""}`
		if got != want {
			t.Fatalf("wire bytes mismatch\n got: %s\nwant: %s", got, want)
		}
		if strings.Contains(got, `"out"`) {
			t.Fatalf("JSON must not contain an out key when Out==nil: %s", got)
		}
	})

	t.Run("out 有值", func(t *testing.T) {
		out := "fix"
		f := resultFrame{
			Type:      frameTypeResult,
			TaskID:    "tsk_x",
			Status:    statusSucceeded,
			Out:       &out,
			Summary:   "ok",
			Artifacts: []string{"tsk_8f21a3c0"},
			Error:     "",
		}
		got := mustEncode(t, f)
		want := `{"type":"result","task_id":"tsk_x","status":"succeeded","out":"fix",` +
			`"summary":"ok","artifacts":["tsk_8f21a3c0"],"error":""}`
		if got != want {
			t.Fatalf("wire bytes mismatch\n got: %s\nwant: %s", got, want)
		}
	})

	t.Run("artifacts 空列表", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			artifacts []string
		}{
			{"nil", nil},
			{"空切片", []string{}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := mustEncode(t, resultFrame{
					Type: frameTypeResult, TaskID: "tsk_x", Status: statusFailed,
					Summary: "", Artifacts: tc.artifacts, Error: "boom",
				})
				if !strings.Contains(got, `"artifacts":[]`) {
					t.Fatalf("artifacts must serialize as [] (never null): %s", got)
				}
				if strings.Contains(got, `"artifacts":null`) {
					t.Fatalf("artifacts serialized as null: %s", got)
				}
			})
		}
	})
}

// ---------- W2 必测 3：anti-drift 反射测试 ----------

// resultFrame 是唯一本地定义的帧，它的 json tag 集合必须与 ws.ResultFrame 完全一致，
// 且**唯一**允许的差异是 out 的 omitempty（本地要能省略键，服务端要能区分缺失/null）。
func TestResultFrameTagsMatchServer(t *testing.T) {
	local := jsonTaggedFields(t, reflect.TypeOf(resultFrame{}), "resultFrame")
	server := jsonTaggedFields(t, reflect.TypeOf(ws.ResultFrame{}), "ws.ResultFrame")

	var localNames, serverNames []string
	for name := range local {
		localNames = append(localNames, name)
	}
	for name := range server {
		serverNames = append(serverNames, name)
	}
	sort.Strings(localNames)
	sort.Strings(serverNames)
	if !reflect.DeepEqual(localNames, serverNames) {
		t.Fatalf("json tag name set drifted from internal/ws\ngot:  %v\nwant: %v\n"+
			"帧结构复用 internal/ws 是硬约束：两边 tag 名必须一字不差", localNames, serverNames)
	}

	for name, localField := range local {
		serverField := server[name]
		if localField.typ != serverField.typ {
			t.Errorf("%q field type = %v, want %v (与 ws.ResultFrame 同类型)", name, localField.typ, serverField.typ)
		}
		if name == "out" {
			// 唯一允许的差异：本地 omitempty（I5 三态的编码侧表达）
			if !localField.omitempty {
				t.Errorf(`resultFrame.Out must be "out,omitempty" (Out==nil → 键缺席)`)
			}
			if serverField.omitempty {
				t.Errorf("ws.ResultFrame.Out unexpectedly has omitempty: 契约认为服务端永远写 out")
			}
			continue
		}
		if localField.tag != serverField.tag {
			t.Errorf("%q tag = %q, want %q", name, localField.tag, serverField.tag)
		}
	}
}

// ---------- W2 必测 6：result JSON 用服务端解码器判定 I5 ----------

func TestResultFrameReadableByServer(t *testing.T) {
	t.Run("out 缺席 → 未回报出口", func(t *testing.T) {
		raw := mustEncode(t, resultFrame{
			Type: frameTypeResult, TaskID: "tsk_x", Status: statusSucceeded, Summary: "ok",
		})
		rf := decodeResultOnServer(t, raw)
		if rf.OutPresent {
			t.Errorf("OutPresent = true, want false (out 键没有出现过)")
		}
		if rf.Reported() {
			t.Errorf("Reported() = true, want false")
		}
		if rf.Out != nil {
			t.Errorf("Out = %q, want nil", *rf.Out)
		}
		if rf.Artifacts == nil {
			t.Errorf("Artifacts = nil, want []（服务端不该看到 null）")
		}
		if rf.Type != frameTypeResult || rf.TaskID != "tsk_x" || rf.Status != statusSucceeded || rf.Summary != "ok" {
			t.Errorf("decoded result = %+v, fields drifted", rf)
		}
	})

	t.Run("out 有值 → 已回报出口", func(t *testing.T) {
		out := "fix"
		raw := mustEncode(t, resultFrame{
			Type: frameTypeResult, TaskID: "tsk_x", Status: statusSucceeded,
			Out: &out, Summary: "发现 2 处类型错误", Artifacts: []string{"tsk_8f21a3c0"},
		})
		rf := decodeResultOnServer(t, raw)
		if !rf.OutPresent {
			t.Fatalf("OutPresent = false, want true (out 键出现过)")
		}
		if !rf.Reported() {
			t.Fatalf("Reported() = false, want true")
		}
		if rf.Out == nil || *rf.Out != "fix" {
			t.Fatalf("Out = %v, want \"fix\"", rf.Out)
		}
		if !reflect.DeepEqual(rf.Artifacts, []string{"tsk_8f21a3c0"}) {
			t.Errorf("Artifacts = %v, want [tsk_8f21a3c0]", rf.Artifacts)
		}
		if rf.Summary != "发现 2 处类型错误" {
			t.Errorf("Summary = %q, want 发现 2 处类型错误", rf.Summary)
		}
	})
}

// ---------- W2 必测 4：decodeDownstream ----------

func TestDecodeDownstream(t *testing.T) {
	t.Run("welcome", func(t *testing.T) {
		got, err := decodeDownstream([]byte(
			`{"type":"welcome","node_id":"nd_x","protocol":1,"heartbeat_sec":10,"lease_ttl_sec":30}`))
		if err != nil {
			t.Fatalf("decodeDownstream: %v", err)
		}
		f, ok := got.(*welcomeFrame)
		if !ok {
			t.Fatalf("decoded type = %T, want *welcomeFrame", got)
		}
		want := ws.WelcomeFrame{Type: frameTypeWelcome, NodeID: "nd_x", Protocol: 1, HeartbeatSec: 10, LeaseTTLSec: 30}
		if *f != want {
			t.Fatalf("welcome = %+v, want %+v", *f, want)
		}
	})

	t.Run("assign", func(t *testing.T) {
		got, err := decodeDownstream([]byte(`{"type":"assign","task_id":"tsk_x",` +
			`"capability":"review","version":1,"params":{"diff":"a b"},"lease_until":1730000030,` +
			`"max_duration_sec":3600}`))
		if err != nil {
			t.Fatalf("decodeDownstream: %v", err)
		}
		f, ok := got.(*assignFrame)
		if !ok {
			t.Fatalf("decoded type = %T, want *assignFrame", got)
		}
		if f.Type != frameTypeAssign || f.TaskID != "tsk_x" || f.Capability != "review" ||
			f.Version != 1 || f.LeaseUntil != 1730000030 || f.MaxDuration != 3600 {
			t.Fatalf("assign = %+v, fields drifted", *f)
		}
		if !reflect.DeepEqual(f.Params, map[string]any{"diff": "a b"}) {
			t.Fatalf("params = %+v, want map[diff:a b]", f.Params)
		}
		// assign 载荷就是中枢能表达的全部：没有 argv / command（I1）
		if strings.Contains(strings.ToLower(string(gotJSON(t, got))), "command") {
			t.Fatal("assign frame carries a command/argv field: 中枢不执行、不下发命令（I1）")
		}
	})

	t.Run("cancel", func(t *testing.T) {
		got, err := decodeDownstream([]byte(`{"type":"cancel","task_id":"tsk_x","reason":"superseded"}`))
		if err != nil {
			t.Fatalf("decodeDownstream: %v", err)
		}
		f, ok := got.(*cancelFrame)
		if !ok {
			t.Fatalf("decoded type = %T, want *cancelFrame", got)
		}
		want := ws.CancelFrame{Type: frameTypeCancel, TaskID: "tsk_x", Reason: "superseded"}
		if *f != want {
			t.Fatalf("cancel = %+v, want %+v", *f, want)
		}
	})

	t.Run("error", func(t *testing.T) {
		got, err := decodeDownstream([]byte(`{"type":"error","code":"unknown_capability",` +
			`"message":"no such capability"}`))
		if err != nil {
			t.Fatalf("decodeDownstream: %v", err)
		}
		f, ok := got.(*errorFrame)
		if !ok {
			t.Fatalf("decoded type = %T, want *errorFrame", got)
		}
		if f.Type != frameTypeError || f.Code != ws.ErrCodeUnknownCapability ||
			f.Message != "no such capability" || f.TaskID != "" {
			t.Fatalf("error = %+v, fields drifted", *f)
		}
	})

	t.Run("未知 type 不算错误", func(t *testing.T) {
		got, err := decodeDownstream([]byte(`{"type":"nope","task_id":"tsk_x"}`))
		if err != nil {
			t.Fatalf("decodeDownstream: err = %v, want nil（未知 type 要回 unknown_type 错误帧，不断连接）", err)
		}
		f, ok := got.(*unknownFrame)
		if !ok {
			t.Fatalf("decoded type = %T, want *unknownFrame", got)
		}
		if f.Type != "nope" {
			t.Fatalf("unknownFrame.Type = %q, want nope", f.Type)
		}
	})

	t.Run("坏 JSON 是错误", func(t *testing.T) {
		for _, raw := range []string{`{"type":`, `not json`, ``} {
			if _, err := decodeDownstream([]byte(raw)); err == nil {
				t.Fatalf("decodeDownstream(%q) returned nil error, want bad_frame", raw)
			}
		}
	})
}

// ---------- 小工具 ----------

// decodeResultOnServer 用**服务端**的解码器读 agent 发出去的 result 字节。
func decodeResultOnServer(t *testing.T, raw string) *ws.ResultFrame {
	t.Helper()
	got, err := ws.DecodeUpstream([]byte(raw))
	if err != nil {
		t.Fatalf("ws.DecodeUpstream(%s): %v", raw, err)
	}
	rf, ok := got.(*ws.ResultFrame)
	if !ok {
		t.Fatalf("decoded type = %T, want *ws.ResultFrame", got)
	}
	return rf
}

func gotJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(%T): %v", v, err)
	}
	return data
}

// jsonEqual 比较两段 JSON 的**语义**（键序/空白无关）。
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", a, err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", b, err)
	}
	return reflect.DeepEqual(av, bv)
}

// jsonField 是从 struct tag 里拆出来的判定信息。
type jsonField struct {
	tag       string // 完整 tag 值，如 "task_id" / "out,omitempty"
	omitempty bool
	typ       reflect.Type
}

// jsonTaggedFields 收集结构体的 json 字段（按**tag 名**索引），跳过 `json:"-"`。
func jsonTaggedFields(t *testing.T, rt reflect.Type, label string) map[string]jsonField {
	t.Helper()
	if rt.Kind() != reflect.Struct {
		t.Fatalf("%s: kind = %v, want struct", label, rt.Kind())
	}
	out := make(map[string]jsonField, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" || tag == "" {
			continue // 内部字段 / 非 JSON 字段
		}
		name, opts, _ := strings.Cut(tag, ",")
		out[name] = jsonField{
			tag:       tag,
			omitempty: strings.Contains(opts, "omitempty"),
			typ:       f.Type,
		}
	}
	return out
}
