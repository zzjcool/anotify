package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// A1 日志 attempt 补丁的具名回归。
//
// 语义（父 ARTIFACT-HANDOFF §1）：
//   - logs_request.attempt 可选，整数 ≥1；省略 = 该逻辑 task 最近一次有档案的 attempt；
//   - logs_response.attempt 是**实际**读到的 attempt：成功必填，错误/无档案为空；
//   - requestId 仍然只做请求配对，不承担 attempt 选择。

// TestLogsRequestAttemptOmittedMeansLatest 钉「省略」的 wire 形状：键不出现（不是 0）。
func TestLogsRequestAttemptOmittedMeansLatest(t *testing.T) {
	raw, err := Encode(&LogsRequestFrame{Type: FrameLogsRequest, TaskID: "tsk_1", RequestID: "r1"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["attempt"]; ok {
		t.Fatalf("省略 attempt 时不应出现 wire 键：%s", raw)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	req, ok := decoded.(*LogsRequestFrame)
	if !ok {
		t.Fatalf("Decode 返回 %T", decoded)
	}
	if req.Attempt != 0 {
		t.Fatalf("省略 attempt 应解码为 0（= 最新），实际 %d", req.Attempt)
	}
}

// TestLogsRequestAttemptExplicit 钉显式 attempt 的 wire 值与往返。
func TestLogsRequestAttemptExplicit(t *testing.T) {
	raw, err := Encode(&LogsRequestFrame{Type: FrameLogsRequest, TaskID: "tsk_1", RequestID: "r1", Attempt: 3})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(string(raw), `"attempt":3`) {
		t.Fatalf("显式 attempt 必须是整数键：%s", raw)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := decoded.(*LogsRequestFrame).Attempt; got != 3 {
		t.Fatalf("attempt 往返丢了：%d", got)
	}
}

// TestLogsResponseAttemptActual 钉「成功回实际 attempt」的形状：错误响应可以没有 attempt。
func TestLogsResponseAttemptActual(t *testing.T) {
	okRaw, err := Encode(&LogsResponseFrame{
		Type: FrameLogsResponse, TaskID: "tsk_1", RequestID: "r1", Attempt: 2,
		Stdout: "o", Stderr: "e", Bytes: 2,
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(okRaw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(m["attempt"]) != "2" {
		t.Fatalf("成功响应必须回实际 attempt：%s", okRaw)
	}
	decoded, err := Decode(okRaw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := decoded.(*LogsResponseFrame).Attempt; got != 2 {
		t.Fatalf("响应 attempt 往返丢了：%d", got)
	}

	errRaw, err := Encode(&LogsResponseFrame{
		Type: FrameLogsResponse, TaskID: "tsk_1", RequestID: "r1", Error: LogsErrNoLocalArchive,
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if strings.Contains(string(errRaw), `"attempt"`) {
		t.Fatalf("无档案的错误响应不能伪造 attempt：%s", errRaw)
	}
}

// TestLogsAttemptDoesNotChangeOtherFrames 确认 A1 补丁只动日志两帧。
func TestLogsAttemptDoesNotChangeOtherFrames(t *testing.T) {
	for _, name := range []string{FrameUpgradeAck, FrameCancelAck, FrameLeaseAck} {
		if strings.Contains(name, "logs") {
			t.Fatalf("测试自身写错了：%s", name)
		}
	}
	// 已冻结的 23 帧集合没有因为 A1 增加/减少帧。
	if got := len(UpstreamFrames()) + len(DownstreamFrames()); got != 23 {
		t.Fatalf("帧总数必须仍是 23，实际 %d", got)
	}
}
