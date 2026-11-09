package protocol

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// ---------- task.emit / task.emit.ack ----------

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func mustMarshal(t *testing.T, f any) []byte {
	t.Helper()
	raw, err := Encode(f)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return raw
}

// TestTaskEmitFrameNamesAndDirection 钉 wire 名与方向：上行只 task.emit，下行只 ack。
func TestTaskEmitFrameNamesAndDirection(t *testing.T) {
	if FrameTaskEmit != "task.emit" {
		t.Fatalf("FrameTaskEmit = %q，期望 task.emit", FrameTaskEmit)
	}
	if FrameTaskEmitAck != "task.emit.ack" {
		t.Fatalf("FrameTaskEmitAck = %q，期望 task.emit.ack", FrameTaskEmitAck)
	}
	if !IsUpstream(FrameTaskEmit) || IsDownstream(FrameTaskEmit) {
		t.Fatal("task.emit 必须是节点 → 中枢（上行），不是下行")
	}
	if !IsDownstream(FrameTaskEmitAck) || IsUpstream(FrameTaskEmitAck) {
		t.Fatal("task.emit.ack 必须是中枢 → 节点（下行），不是上行")
	}
}

// TestTaskEmitFrameWire 钉 task.emit 的 wire 键快照与字段往返。
//
// requestId 与业务 id 是两个字段：前者每次 RPC 新生成，后者是跨 attempt 的业务幂等键。
func TestTaskEmitFrameWire(t *testing.T) {
	frame := &TaskEmitFrame{
		Type: FrameTaskEmit, RequestID: "ereq_1", TaskID: "tsk_1", Attempt: 2,
		LeaseID: "lease_1", AttemptToken: "tok_1",
		EmitRequest: EmitRequest{To: "fixer", ID: "review-a.go", Data: json.RawMessage(`{"file":"a.go"}`)},
	}
	raw := mustMarshal(t, frame)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{"attempt", "attemptToken", "data", "id", "leaseId", "requestId", "taskId", "to", "type"}
	if got := sortedKeys(m); !reflect.DeepEqual(got, want) {
		t.Fatalf("task.emit wire 键 = %v，期望 %v", got, want)
	}
	// 平铺：不许出现嵌套的 "EmitRequest" / "emitRequest" 包装。
	for _, k := range []string{"EmitRequest", "emitRequest", "namespace", "node", "seq"} {
		if _, ok := m[k]; ok {
			t.Fatalf("task.emit 不应出现 %q 键", k)
		}
	}

	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got, ok := decoded.(*TaskEmitFrame)
	if !ok {
		t.Fatalf("Decode 返回 %T，期望 *TaskEmitFrame", decoded)
	}
	if !reflect.DeepEqual(got, frame) {
		t.Fatalf("往返丢字段:\n got %#v\nwant %#v", got, frame)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("合法帧 Validate: %v", err)
	}

	// 可选字段省略：没有 id/data 时 wire 上不出现这两个键（不是空串/null）。
	minimal := &TaskEmitFrame{
		Type: FrameTaskEmit, RequestID: "ereq_2", TaskID: "tsk_1", Attempt: 1,
		LeaseID: "lease_1", AttemptToken: "tok_1",
		EmitRequest: EmitRequest{To: "fixer"},
	}
	minimalRaw := mustMarshal(t, minimal)
	var mm map[string]json.RawMessage
	if err := json.Unmarshal(minimalRaw, &mm); err != nil {
		t.Fatalf("unmarshal minimal: %v", err)
	}
	wantMinimal := []string{"attempt", "attemptToken", "leaseId", "requestId", "taskId", "to", "type"}
	if got := sortedKeys(mm); !reflect.DeepEqual(got, wantMinimal) {
		t.Fatalf("最小 task.emit wire 键 = %v，期望 %v", got, wantMinimal)
	}
}

// TestTaskEmitFrameEmbeddedEmitRequest 证明 WS 帧与 HTTP 入口是**同一个** payload 解释：
// to/id/data 由匿名嵌入的 EmitRequest 提供，往返后整体相等。
func TestTaskEmitFrameEmbeddedEmitRequest(t *testing.T) {
	payload := EmitRequest{To: "fixer", ID: "evt-1", Data: json.RawMessage(`{"file":"a.go","n":1}`)}
	frame := &TaskEmitFrame{
		Type: FrameTaskEmit, RequestID: "ereq_1", TaskID: "tsk_1", Attempt: 1,
		LeaseID: "lease_1", AttemptToken: "tok_1", EmitRequest: payload,
	}
	decoded, err := Decode(mustMarshal(t, frame))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got := decoded.(*TaskEmitFrame)
	if !reflect.DeepEqual(got.EmitRequest, payload) {
		t.Fatalf("EmitRequest 往返 = %#v，期望 %#v", got.EmitRequest, payload)
	}
	// 同一个 EmitRequest 直接作为 HTTP body 编码出的键，必须都在 WS 帧里。
	httpRaw := mustMarshal(t, payload)
	var hmap map[string]json.RawMessage
	if err := json.Unmarshal(httpRaw, &hmap); err != nil {
		t.Fatalf("unmarshal http body: %v", err)
	}
	var wmap map[string]json.RawMessage
	if err := json.Unmarshal(mustMarshal(t, frame), &wmap); err != nil {
		t.Fatalf("unmarshal ws frame: %v", err)
	}
	for k, v := range hmap {
		wv, ok := wmap[k]
		if !ok {
			t.Fatalf("HTTP EmitRequest 键 %q 在 WS 帧里丢了", k)
		}
		if !bytes.Equal(wv, v) {
			t.Fatalf("键 %q 的值不同：ws=%s http=%s", k, wv, v)
		}
	}
}

// TestTaskEmitUnknownFieldsRejected 未知顶层字段让 Decode 直接失败。
// E1 两帧是父批准的严格字段帧：namespace/node/seq 这类「偷偷加」的键不能被
// 静默忽略——否则调用方会误以为自己的覆盖生效，或中枢误读一条伪造帧。
func TestTaskEmitUnknownFieldsRejected(t *testing.T) {
	control := []byte(`{"type":"task.emit","requestId":"ereq_1","taskId":"tsk_1","attempt":2,"leaseId":"lease_1","attemptToken":"tok_1","to":"fixer","data":{"file":"a.go"}}`)
	if _, err := Decode(control); err != nil {
		t.Fatalf("合法帧必须能 Decode：%v", err)
	}
	extras := map[string]string{
		"namespace":    `"namespace":"attacker-ns"`,
		"node":         `"node":"other-node"`,
		"seq":          `"seq":99`,
		"leaseExpires": `"leaseExpiresAt":"2030-01-01T00:00:00Z"`,
	}
	for name, extra := range extras {
		t.Run(name, func(t *testing.T) {
			decoded, err := Decode(withUnknownField(control, extra))
			if err == nil {
				t.Fatalf("未知字段 %s 必须被拒绝，实际返回 %#v", extra, decoded)
			}
			if !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("应当是严格字段拒绝，实际错误：%v", err)
			}
		})
	}
}

// TestTaskEmitDecodeEnforcesValidate task.emit 的 Validate 在接收路径生效：
// 关联身份必填、attempt≥1、data 必须是对象；这些不在调用方「记得调」的层面。
func TestTaskEmitDecodeEnforcesValidate(t *testing.T) {
	legal := `{"type":"task.emit","requestId":"ereq_1","taskId":"tsk_1","attempt":1,"leaseId":"lease_1","attemptToken":"tok_1","to":"fixer"}`
	if _, err := Decode([]byte(legal)); err != nil {
		t.Fatalf("合法帧必须能 Decode：%v", err)
	}
	cases := map[string]string{
		"missing requestId": `{"type":"task.emit","taskId":"tsk_1","attempt":1,"leaseId":"lease_1","attemptToken":"tok_1","to":"fixer"}`,
		"missing taskId":    `{"type":"task.emit","requestId":"ereq_1","attempt":1,"leaseId":"lease_1","attemptToken":"tok_1","to":"fixer"}`,
		"attempt zero":      `{"type":"task.emit","requestId":"ereq_1","taskId":"tsk_1","leaseId":"lease_1","attemptToken":"tok_1","to":"fixer"}`,
		"missing leaseId":   `{"type":"task.emit","requestId":"ereq_1","taskId":"tsk_1","attempt":1,"attemptToken":"tok_1","to":"fixer"}`,
		"missing token":     `{"type":"task.emit","requestId":"ereq_1","taskId":"tsk_1","attempt":1,"leaseId":"lease_1","to":"fixer"}`,
		"non-object data":   `{"type":"task.emit","requestId":"ereq_1","taskId":"tsk_1","attempt":1,"leaseId":"lease_1","attemptToken":"tok_1","to":"fixer","data":[1,2]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			decoded, err := Decode([]byte(raw))
			if err == nil {
				t.Fatalf("%s 必须被 Decode 拒绝，实际返回 %#v", name, decoded)
			}
			if !strings.Contains(err.Error(), "task.emit") {
				t.Fatalf("错误应指出 task.emit：%v", err)
			}
		})
	}
}

// withUnknownField 在顶层对象末尾插入一个额外键（只用于测试构造非法帧）。
func withUnknownField(control []byte, extra string) []byte {
	out := make([]byte, 0, len(control)+len(extra)+2)
	out = append(out, control[:len(control)-1]...)
	out = append(out, ',')
	out = append(out, extra...)
	out = append(out, '}')
	return out
}

// TestTaskEmitFrameValidate 覆盖关联身份必填与 attempt 合法性。
func TestTaskEmitFrameValidate(t *testing.T) {
	base := func() *TaskEmitFrame {
		return &TaskEmitFrame{
			Type: FrameTaskEmit, RequestID: "ereq_1", TaskID: "tsk_1", Attempt: 1,
			LeaseID: "lease_1", AttemptToken: "tok_1",
			EmitRequest: EmitRequest{To: "fixer", Data: json.RawMessage(`{"a":1}`)},
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("合法帧 Validate: %v", err)
	}
	cases := map[string]func(*TaskEmitFrame){
		"missing requestId": func(f *TaskEmitFrame) { f.RequestID = "" },
		"missing taskId":    func(f *TaskEmitFrame) { f.TaskID = "" },
		"attempt zero":      func(f *TaskEmitFrame) { f.Attempt = 0 },
		"attempt negative":  func(f *TaskEmitFrame) { f.Attempt = -1 },
		"missing leaseId":   func(f *TaskEmitFrame) { f.LeaseID = "" },
		"missing token":     func(f *TaskEmitFrame) { f.AttemptToken = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := base()
			mutate(f)
			if err := f.Validate(); err == nil {
				t.Fatalf("%s 应当被 Validate 拒绝", name)
			}
		})
	}
}

// TestTaskEmitNonObjectData 非对象 data（数组/标量/字符串/null）不能默默接受；
// 缺 data 则是合法的（空对象语义）。
func TestTaskEmitNonObjectData(t *testing.T) {
	newFrame := func(data string) *TaskEmitFrame {
		f := &TaskEmitFrame{
			Type: FrameTaskEmit, RequestID: "ereq_1", TaskID: "tsk_1", Attempt: 1,
			LeaseID: "lease_1", AttemptToken: "tok_1", EmitRequest: EmitRequest{To: "fixer"},
		}
		if data != "" {
			f.EmitRequest.Data = json.RawMessage(data)
		}
		return f
	}
	if err := newFrame(`{"file":"a.go"}`).Validate(); err != nil {
		t.Fatalf("对象 data 应通过：%v", err)
	}
	if err := newFrame("").Validate(); err != nil {
		t.Fatalf("缺 data 应通过（空对象语义）：%v", err)
	}
	for _, raw := range []string{`[1,2]`, `"str"`, `42`, `true`, `null`} {
		if err := newFrame(raw).Validate(); err == nil {
			t.Fatalf("data=%s 必须被拒绝（非 JSON 对象）", raw)
		}
	}
}

// TestTaskEmitMissingTargetKeepsHTTPSemantics 缺 to 不是协议错误：Decode/Validate
// 都放行，让中枢按 HTTP 语义回 missing_target 400 / CLI 2。
func TestTaskEmitMissingTargetKeepsHTTPSemantics(t *testing.T) {
	raw := []byte(`{
		"type":"task.emit","requestId":"ereq_1","taskId":"tsk_1","attempt":1,
		"leaseId":"lease_1","attemptToken":"tok_1","data":{}
	}`)
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("缺 to 不该让 Decode 失败（否则中枢无法回 HTTP 语义）：%v", err)
	}
	frame := decoded.(*TaskEmitFrame)
	if frame.EmitRequest.To != "" {
		t.Fatalf("缺 to 解码后应当是空串，实际 %q", frame.EmitRequest.To)
	}
	if err := frame.Validate(); err != nil {
		t.Fatalf("缺 to 不该让 Validate 失败：%v", err)
	}
	if got := EmitHTTPStatus(ReasonMissingTarget); got != 400 {
		t.Fatalf("missing_target HTTP 状态 = %d，期望 400", got)
	}
	if got := EmitExitCode(ReasonMissingTarget); got != EmitExitBadRequest {
		t.Fatalf("missing_target CLI 退出码 = %d，期望 %d", got, EmitExitBadRequest)
	}
}

// TestTaskEmitAckFrameWire 钉两种 ack 形态的 wire 键：成功只有 response，错误只有
// error；都不回显 attemptToken/leaseId。
func TestTaskEmitAckFrameWire(t *testing.T) {
	success := &TaskEmitAckFrame{
		Type: FrameTaskEmitAck, RequestID: "ereq_1", TaskID: "tsk_1", Attempt: 2, HTTPStatus: 200,
		Response: &EmitResponse{TaskID: testStr("tsk_2"), EventID: "evt_1", To: "fixer", Version: 2},
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(mustMarshal(t, success), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantSuccess := []string{"attempt", "httpStatus", "requestId", "response", "taskId", "type"}
	if got := sortedKeys(m); !reflect.DeepEqual(got, wantSuccess) {
		t.Fatalf("成功 ack wire 键 = %v，期望 %v", got, wantSuccess)
	}
	for _, k := range []string{"attemptToken", "leaseId", "namespace", "error", "data", "to", "id", "seq"} {
		if _, ok := m[k]; ok {
			t.Fatalf("成功 ack 不应出现 %q 键", k)
		}
	}

	failure := &TaskEmitAckFrame{
		Type: FrameTaskEmitAck, RequestID: "ereq_1", TaskID: "tsk_1", Attempt: 2, HTTPStatus: 409,
		Error: &EmitError{Error: ReasonEmitIDConflict, Message: "同 id 不同请求"},
	}
	var fm map[string]json.RawMessage
	if err := json.Unmarshal(mustMarshal(t, failure), &fm); err != nil {
		t.Fatalf("unmarshal failure: %v", err)
	}
	wantFailure := []string{"attempt", "error", "httpStatus", "requestId", "taskId", "type"}
	if got := sortedKeys(fm); !reflect.DeepEqual(got, wantFailure) {
		t.Fatalf("错误 ack wire 键 = %v，期望 %v", got, wantFailure)
	}
	if _, ok := fm["response"]; ok {
		t.Fatal("错误 ack 不应出现 response 键")
	}
	for _, k := range []string{"attemptToken", "leaseId", "namespace", "seq"} {
		if _, ok := fm[k]; ok {
			t.Fatalf("错误 ack 不应出现 %q 键", k)
		}
	}

	// 两种形态都必须能走统一 Decode 并往返。
	for _, frame := range []*TaskEmitAckFrame{success, failure} {
		decoded, err := Decode(mustMarshal(t, frame))
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		got, ok := decoded.(*TaskEmitAckFrame)
		if !ok {
			t.Fatalf("Decode 返回 %T，期望 *TaskEmitAckFrame", decoded)
		}
		if !reflect.DeepEqual(got, frame) {
			t.Fatalf("ack 往返丢字段：\n got %#v\nwant %#v", got, frame)
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	}
}

// TestTaskEmitAckMutualExclusion 成功只 response、错误只 error：同时出现或都不出现
// 都不是可信 ack。
func TestTaskEmitAckMutualExclusion(t *testing.T) {
	base := func() *TaskEmitAckFrame {
		return &TaskEmitAckFrame{
			Type: FrameTaskEmitAck, RequestID: "ereq_1", TaskID: "tsk_1", Attempt: 1, HTTPStatus: 200,
		}
	}
	withResponse := base()
	withResponse.Response = &EmitResponse{TaskID: testStr("tsk_2"), To: "fixer", Version: 1}
	if err := withResponse.Validate(); err != nil {
		t.Fatalf("只有 response 应通过：%v", err)
	}
	withError := base()
	withError.HTTPStatus = 400
	withError.Error = &EmitError{Error: ReasonMissingTarget}
	if err := withError.Validate(); err != nil {
		t.Fatalf("只有 error 应通过：%v", err)
	}
	both := base()
	both.Response = &EmitResponse{TaskID: testStr("tsk_2"), To: "fixer", Version: 1}
	both.Error = &EmitError{Error: ReasonMissingTarget}
	if err := both.Validate(); err == nil {
		t.Fatal("response 与 error 同时出现必须被拒绝")
	}
	neither := base()
	if err := neither.Validate(); err == nil {
		t.Fatal("既无 response 又无 error 必须被拒绝")
	}
}

// TestTaskEmitAckStatusMapping ack 的 httpStatus 必须与错误码/成功一致。
func TestTaskEmitAckStatusMapping(t *testing.T) {
	codes := map[string]int{
		ReasonTargetNotDeclared: 400,
		ReasonMissingTarget:     400,
		ReasonTokenInvalid:      401,
		ReasonTaskNotRunning:    409,
		ReasonEmitIDConflict:    409,
		ReasonEmitUnreachable:   500,
	}
	for code, wantStatus := range codes {
		f := &TaskEmitAckFrame{
			Type: FrameTaskEmitAck, RequestID: "ereq_1", TaskID: "tsk_1", Attempt: 1,
			HTTPStatus: wantStatus, Error: &EmitError{Error: code},
		}
		if err := f.Validate(); err != nil {
			t.Fatalf("错误码 %q + %d 应通过：%v", code, wantStatus, err)
		}
		if got := EmitHTTPStatus(code); got != wantStatus {
			t.Fatalf("EmitHTTPStatus(%q) = %d，期望 %d", code, got, wantStatus)
		}
		f.HTTPStatus = wantStatus + 1
		if err := f.Validate(); err == nil {
			t.Fatalf("错误码 %q 配错状态必须被拒绝", code)
		}
	}

	// 成功回执固定 200（剪枝也是 200，不是 4xx）。
	ok := &TaskEmitAckFrame{
		Type: FrameTaskEmitAck, RequestID: "ereq_1", TaskID: "tsk_1", Attempt: 1, HTTPStatus: 200,
		Response: &EmitResponse{TaskID: testStr("tsk_2"), To: "fixer", Version: 1},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("成功 ack 应通过：%v", err)
	}
	badSuccess := *ok
	badSuccess.HTTPStatus = 409
	if err := badSuccess.Validate(); err == nil {
		t.Fatal("成功 ack 的 httpStatus 不是 200 必须被拒绝")
	}

	// 未知错误码不当作可信 ack（应走传输层结果未知，不伪造业务结论）。
	unknown := &TaskEmitAckFrame{
		Type: FrameTaskEmitAck, RequestID: "ereq_1", TaskID: "tsk_1", Attempt: 1, HTTPStatus: 500,
		Error: &EmitError{Error: "boom"},
	}
	if err := unknown.Validate(); err == nil {
		t.Fatal("未知错误码必须被拒绝")
	}
	if !IsEmitErrorCode(ReasonMissingTarget) || IsEmitErrorCode("boom") {
		t.Fatal("IsEmitErrorCode 的封闭集合不对")
	}
}

// TestTaskEmitAckUnknownFieldsRejected ack 顶层未知字段同样被拒绝：
// ACK 是中枢的权威结论，多出来的键不能静默漂入节点。
func TestTaskEmitAckUnknownFieldsRejected(t *testing.T) {
	control := []byte(`{"type":"task.emit.ack","requestId":"ereq_1","taskId":"tsk_1","attempt":2,"httpStatus":200,"response":{"taskId":"tsk_2","to":"fixer","version":2}}`)
	if _, err := Decode(control); err != nil {
		t.Fatalf("合法 ack 必须能 Decode：%v", err)
	}
	extras := map[string]string{
		"namespace": `"namespace":"attacker-ns"`,
		"seq":       `"seq":3`,
		"status":    `"status":"ok"`,
	}
	for name, extra := range extras {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(withUnknownField(control, extra)); err == nil {
				t.Fatalf("ack 未知字段 %s 必须被拒绝", extra)
			} else if !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("应当是严格字段拒绝，实际错误：%v", err)
			}
		})
	}
}

// TestTaskEmitAckNestedUnknownFieldsRejected 严格字段递归到嵌套 DTO：
// response / response.pruned / error 多一个键都拒——不给伪造字段或第二个
// Status 信封任何入口。
func TestTaskEmitAckNestedUnknownFieldsRejected(t *testing.T) {
	cases := map[string]string{
		"response": `{"type":"task.emit.ack","requestId":"ereq_1","taskId":"tsk_1","attempt":1,"httpStatus":200,` +
			`"response":{"taskId":"tsk_2","to":"fixer","version":1,"status":"ok"}}`,
		"pruned": `{"type":"task.emit.ack","requestId":"ereq_1","taskId":"tsk_1","attempt":1,"httpStatus":200,` +
			`"response":{"taskId":null,"eventId":"evt_1","to":"fixer","version":2,` +
			`"pruned":{"reason":"param_missing","code":"param_type"}}}`,
		"error": `{"type":"task.emit.ack","requestId":"ereq_1","taskId":"tsk_1","attempt":1,"httpStatus":400,` +
			`"error":{"error":"missing_target","status":400}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(raw)); err == nil {
				t.Fatalf("嵌套未知字段（%s）必须被拒绝", name)
			} else if !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("应当是严格字段拒绝，实际错误：%v", err)
			}
		})
	}
}

// TestTaskEmitAckCredentialInjectionRejected ack 里注入 attemptToken/leaseId/
// token/namespace 不能被静默忽略：严格字段帧直接拒整条 ack，伪造与日志泄漏面
// 都不接收。
func TestTaskEmitAckCredentialInjectionRejected(t *testing.T) {
	base := `{"type":"task.emit.ack","requestId":"ereq_1","taskId":"tsk_1","attempt":1,"httpStatus":200,"response":{"taskId":"tsk_2","to":"fixer","version":1}`
	for _, extra := range []string{
		`"attemptToken":"tok_1"`,
		`"leaseId":"lease_1"`,
		`"token":"tok_1"`,
		`"namespace":"attacker-ns"`,
	} {
		if _, err := Decode([]byte(base + "," + extra + `}`)); err == nil {
			t.Fatalf("ack 注入 %s 必须被拒绝", extra)
		}
	}
}

// TestTaskEmitAckDecodeEnforcesValidate ack 的互斥 / 状态对应 / 关联身份必填
// 也在接收路径生效：不是只直接调 Validate 才拒。
func TestTaskEmitAckDecodeEnforcesValidate(t *testing.T) {
	legal := `{"type":"task.emit.ack","requestId":"ereq_1","taskId":"tsk_1","attempt":1,"httpStatus":200,"response":{"taskId":"tsk_2","to":"fixer","version":1}}`
	if _, err := Decode([]byte(legal)); err != nil {
		t.Fatalf("合法成功 ack 必须能 Decode：%v", err)
	}
	legalErr := `{"type":"task.emit.ack","requestId":"ereq_1","taskId":"tsk_1","attempt":1,"httpStatus":400,"error":{"error":"missing_target"}}`
	if _, err := Decode([]byte(legalErr)); err != nil {
		t.Fatalf("合法错误 ack 必须能 Decode：%v", err)
	}
	cases := map[string]string{
		"missing requestId": `{"type":"task.emit.ack","taskId":"tsk_1","attempt":1,"httpStatus":200,"response":{"taskId":"tsk_2","to":"fixer","version":1}}`,
		"missing taskId":    `{"type":"task.emit.ack","requestId":"ereq_1","attempt":1,"httpStatus":200,"response":{"taskId":"tsk_2","to":"fixer","version":1}}`,
		"attempt zero":      `{"type":"task.emit.ack","requestId":"ereq_1","taskId":"tsk_1","httpStatus":200,"response":{"taskId":"tsk_2","to":"fixer","version":1}}`,
		"both response and error": `{"type":"task.emit.ack","requestId":"ereq_1","taskId":"tsk_1","attempt":1,"httpStatus":200,` +
			`"response":{"taskId":"tsk_2","to":"fixer","version":1},"error":{"error":"missing_target"}}`,
		"neither response nor error": `{"type":"task.emit.ack","requestId":"ereq_1","taskId":"tsk_1","attempt":1,"httpStatus":200}`,
		"success status not 200": `{"type":"task.emit.ack","requestId":"ereq_1","taskId":"tsk_1","attempt":1,"httpStatus":409,` +
			`"response":{"taskId":"tsk_2","to":"fixer","version":1}}`,
		"unknown error code": `{"type":"task.emit.ack","requestId":"ereq_1","taskId":"tsk_1","attempt":1,"httpStatus":500,` +
			`"error":{"error":"boom"}}`,
		"error status mismatch": `{"type":"task.emit.ack","requestId":"ereq_1","taskId":"tsk_1","attempt":1,"httpStatus":409,` +
			`"error":{"error":"missing_target"}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			decoded, err := Decode([]byte(raw))
			if err == nil {
				t.Fatalf("%s 必须被 Decode 拒绝，实际返回 %#v", name, decoded)
			}
			if !strings.Contains(err.Error(), "task.emit.ack") {
				t.Fatalf("错误应指出 task.emit.ack：%v", err)
			}
		})
	}
}

// TestTaskEmitRequestIDIsCorrelationOnly requestId 只是 RPC 关联；业务幂等键是显式 id。
// 两者在 wire 上并存且不互相替代；ack 只有 requestId，没有业务 id/seq。
func TestTaskEmitRequestIDIsCorrelationOnly(t *testing.T) {
	frame := &TaskEmitFrame{
		Type: FrameTaskEmit, RequestID: "ereq_retry_2", TaskID: "tsk_1", Attempt: 2,
		LeaseID: "lease_1", AttemptToken: "tok_1",
		EmitRequest: EmitRequest{To: "fixer", ID: "review-a.go", Data: json.RawMessage(`{"file":"a.go"}`)},
	}
	decoded, err := Decode(mustMarshal(t, frame))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got := decoded.(*TaskEmitFrame)
	if got.RequestID == got.EmitRequest.ID {
		t.Fatal("requestId 与业务 id 必须是两个字段")
	}
	if got.RequestID != "ereq_retry_2" || got.EmitRequest.ID != "review-a.go" {
		t.Fatalf("requestId/id 被串了：%#v", got)
	}

	ackRaw := []byte(`{
		"type":"task.emit.ack","requestId":"ereq_retry_2","taskId":"tsk_1","attempt":2,
		"httpStatus":200,"response":{"taskId":"tsk_2","to":"fixer","version":2}
	}`)
	ack := mustDecodeAck(t, ackRaw)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(mustMarshal(t, ack), &m); err != nil {
		t.Fatalf("unmarshal ack: %v", err)
	}
	for _, k := range []string{"id", "seq"} {
		if _, ok := m[k]; ok {
			t.Fatalf("ack 不应带业务键 %q（requestId 只做关联）", k)
		}
	}
}

func mustDecodeAck(t *testing.T, raw []byte) *TaskEmitAckFrame {
	t.Helper()
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode ack: %v", err)
	}
	ack, ok := decoded.(*TaskEmitAckFrame)
	if !ok {
		t.Fatalf("Decode 返回 %T，期望 *TaskEmitAckFrame", decoded)
	}
	return ack
}

// TestTaskEmitAckPrunedSuccess 剪枝也是成功 200：response.taskId 是 null（不是省略、
// 不是空串），带 pruned 原因。
func TestTaskEmitAckPrunedSuccess(t *testing.T) {
	frame := &TaskEmitAckFrame{
		Type: FrameTaskEmitAck, RequestID: "ereq_1", TaskID: "tsk_1", Attempt: 1, HTTPStatus: 200,
		Response: &EmitResponse{
			TaskID: nil, EventID: "evt_1", To: "fixer", Version: 2,
			Pruned: &EmitPruned{Reason: ReasonParamMissing, Detail: "需要 file"},
		},
	}
	if err := frame.Validate(); err != nil {
		t.Fatalf("剪枝回执应通过 Validate：%v", err)
	}
	raw := mustMarshal(t, frame)
	if !bytes.Contains(raw, []byte(`"taskId":null`)) {
		t.Fatalf("剪枝回执必须写 \"taskId\":null：%s", raw)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got := decoded.(*TaskEmitAckFrame)
	if got.Response == nil || got.Response.TaskID != nil {
		t.Fatalf("剪枝回执往返丢了 null taskId：%#v", got.Response)
	}
	if got.Response.Pruned == nil || got.Response.Pruned.Reason != ReasonParamMissing {
		t.Fatalf("剪枝原因丢了：%#v", got.Response)
	}
}
