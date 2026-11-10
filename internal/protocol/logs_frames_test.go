package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// L1-R3 日志两帧补口的 wire 回归：request 新增 tailLines/sinceSeconds（camelCase、
// omitempty），response 新增 lines（仅成功出现，零条 = []），LogLine 恰四字段。

// TestLogsRequestTailAndSinceWireShape 钉新字段的 wire 形状与往返。
func TestLogsRequestTailAndSinceWireShape(t *testing.T) {
	raw, err := Encode(&LogsRequestFrame{
		Type: FrameLogsRequest, TaskID: "tsk_1", RequestID: "lreq_1", Attempt: 2,
		TailLines: 120, SinceSeconds: 3600,
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for _, want := range []string{`"tailLines":120`, `"sinceSeconds":3600`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("wire 缺 %s：%s", want, raw)
		}
	}
	silent, err := Encode(&LogsRequestFrame{Type: FrameLogsRequest, TaskID: "tsk_1", RequestID: "lreq_1"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for _, key := range []string{"tailLines", "sinceSeconds"} {
		if strings.Contains(string(silent), `"`+key+`"`) {
			t.Fatalf("0/省略的 %s 不应出现 wire 键：%s", key, silent)
		}
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	req := decoded.(*LogsRequestFrame)
	if req.TailLines != 120 || req.SinceSeconds != 3600 {
		t.Fatalf("新字段往返丢了：%+v", req)
	}
}

// TestValidateLogsRequestClosedDomain 钉请求参数的封闭域：非法值必须拒绝，
// 不能被归一化后静默当成「0 = 最新 / 0 = 不限」。
func TestValidateLogsRequestClosedDomain(t *testing.T) {
	valid := []*LogsRequestFrame{
		{Type: FrameLogsRequest, TaskID: "t1", RequestID: "r1"},
		{Type: FrameLogsRequest, TaskID: "t1", RequestID: "r1", Attempt: 1, TailLines: LogsTailLinesMax, SinceSeconds: 86400},
	}
	for i, f := range valid {
		if err := ValidateLogsRequest(f); err != nil {
			t.Fatalf("合法请求 #%d 被拒：%v", i, err)
		}
	}
	illegal := []*LogsRequestFrame{
		nil,
		{Type: FrameLogsResponse, TaskID: "t1", RequestID: "r1"},
		{Type: FrameLogsRequest, RequestID: "r1"},
		{Type: FrameLogsRequest, TaskID: "t1"},
		{Type: FrameLogsRequest, TaskID: "t1", RequestID: "r1", Attempt: -1},
		{Type: FrameLogsRequest, TaskID: "t1", RequestID: "r1", TailLines: -1},
		{Type: FrameLogsRequest, TaskID: "t1", RequestID: "r1", TailLines: LogsTailLinesMax + 1},
		{Type: FrameLogsRequest, TaskID: "t1", RequestID: "r1", SinceSeconds: -1},
	}
	for i, f := range illegal {
		if err := ValidateLogsRequest(f); err == nil {
			t.Fatalf("非法请求 #%d 必须被拒：%+v", i, f)
		}
	}
}

// TestLogLineExactlyFourWireFields 钉 LogLine 恰有 sequence/stream/text/at。
func TestLogLineExactlyFourWireFields(t *testing.T) {
	raw, err := Encode(LogLine{Sequence: 7, Stream: LogStreamStderr, Text: "hello", At: "2026-11-10T10:00:00.123456789Z"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{"sequence", "stream", "text", "at"}
	if len(m) != len(want) {
		t.Fatalf("LogLine 只能有四个字段：%s", raw)
	}
	for _, key := range want {
		if _, ok := m[key]; !ok {
			t.Fatalf("LogLine 缺字段 %s：%s", key, raw)
		}
	}
	var round LogLine
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("unmarshal LogLine: %v", err)
	}
	if round != (LogLine{Sequence: 7, Stream: LogStreamStderr, Text: "hello", At: "2026-11-10T10:00:00.123456789Z"}) {
		t.Fatalf("LogLine 往返丢了：%+v", round)
	}
}

// TestLogsResponseLinesSuccessArrayErrorAbsent 钉 lines 的两种合法形状：
// 成功（含零条）必须是数组，错误响应不能出现成功记录数组。
func TestLogsResponseLinesSuccessArrayErrorAbsent(t *testing.T) {
	empty := &LogsResponseFrame{Type: FrameLogsResponse, TaskID: "t1", RequestID: "r1", Attempt: 1}
	empty.SetLogLines(nil)
	raw, err := Encode(empty)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(string(raw), `"lines":[]`) {
		t.Fatalf("成功零条必须编码成 lines: []：%s", raw)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	lines := decoded.(*LogsResponseFrame).LogLines()
	if lines == nil || len(lines) != 0 {
		t.Fatalf("成功零条解码必须是非 nil 空数组：%#v", lines)
	}

	withLines := &LogsResponseFrame{Type: FrameLogsResponse, TaskID: "t1", RequestID: "r1", Attempt: 1}
	withLines.SetLogLines([]LogLine{{Sequence: 1, Stream: LogStreamStdout, Text: "a", At: "2026-11-10T10:00:00Z"}})
	raw, err = Encode(withLines)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(string(raw), `"lines":[{"sequence":1`) {
		t.Fatalf("记录必须按序数组编码：%s", raw)
	}

	errResp := &LogsResponseFrame{Type: FrameLogsResponse, TaskID: "t1", RequestID: "r1", Error: LogsErrUnreadable}
	raw, err = Encode(errResp)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if strings.Contains(string(raw), `"lines"`) {
		t.Fatalf("错误响应不能携带成功记录数组：%s", raw)
	}
	if strings.Contains(string(raw), `"attempt"`) {
		t.Fatalf("错误响应不能伪造 attempt：%s", raw)
	}
}

// TestLogsClosedErrorSet 钉新增错误码的字面值（中枢按闭集映射，漂一个就是协议错）。
func TestLogsClosedErrorSet(t *testing.T) {
	cases := map[string]string{
		LogsErrNoLocalArchive: "no_local_archive",
		LogsErrUnreadable:     "logs_unreadable",
		LogsErrIllegalRequest: "illegal_log_request",
		LogStreamStdout:       "stdout",
		LogStreamStderr:       "stderr",
	}
	for got, want := range cases {
		if got != want {
			t.Fatalf("日志闭集常量漂了：got %q want %q", got, want)
		}
	}
	if LogLineTextMaxBytes != 64<<10 {
		t.Fatalf("单条文字段上限必须是 64 KiB，实际 %d", LogLineTextMaxBytes)
	}
	if LogsTailLinesMax != 5000 {
		t.Fatalf("tailLines 上限必须是 5000，实际 %d", LogsTailLinesMax)
	}
}
