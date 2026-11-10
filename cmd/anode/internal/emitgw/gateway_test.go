package emitgw

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/anotify/cmd/anode/internal/runner"
	"github.com/zzjcool/anotify/internal/protocol"
)

type fakeLookup struct {
	mu       sync.Mutex
	bindings map[string]runner.EmitBinding
}

func (f *fakeLookup) LookupEmit(token string) (runner.EmitBinding, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.bindings[token]
	return b, ok
}

type fakeSender struct {
	mu      sync.Mutex
	frames  []*protocol.TaskEmitFrame
	offline bool
	onSend  func(*protocol.TaskEmitFrame)
}

func (s *fakeSender) Send(frame any) error {
	s.mu.Lock()
	if s.offline {
		s.mu.Unlock()
		return io.ErrClosedPipe
	}
	f, _ := frame.(*protocol.TaskEmitFrame)
	if f != nil {
		s.frames = append(s.frames, f)
	}
	onSend := s.onSend
	s.mu.Unlock()
	if onSend != nil && f != nil {
		go onSend(f)
	}
	return nil
}

func newTestGateway(t *testing.T) (*Gateway, *fakeSender, *httptest.Server) {
	t.Helper()
	lookup := &fakeLookup{bindings: map[string]runner.EmitBinding{
		"tok-1": {
			TaskID: "t1", Attempt: 1, LeaseID: "lease-1", Token: "tok-1",
			Next: []string{"fixer", "reviewer@2"},
		},
	}}
	sender := &fakeSender{}
	gw := New(sender, lookup)
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return gw, sender, srv
}

func postEmit(t *testing.T, srv *httptest.Server, token, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+protocol.EmitPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set(protocol.EmitAuthHeader, protocol.EmitAuthScheme+" "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if len(data) > 0 {
		_ = json.Unmarshal(data, &decoded)
	}
	return resp, decoded
}

func TestEmitSuccessCorrelatesByRequestID(t *testing.T) {
	gw, sender, srv := newTestGateway(t)
	// 中枢回执：requestId 从节点帧里取，身份核对 task/attempt。
	sender.onSend = func(f *protocol.TaskEmitFrame) {
		id := "task-downstream"
		gw.HandleEmitAck(&protocol.TaskEmitAckFrame{
			Type: protocol.FrameTaskEmitAck, RequestID: f.RequestID,
			TaskID: f.TaskID, Attempt: f.Attempt, HTTPStatus: 200,
			Response: &protocol.EmitResponse{TaskID: &id, EventID: "evt-1", To: f.EmitRequest.To, Version: 1},
		})
	}
	resp, body := postEmit(t, srv, "tok-1", `{"to":"fixer","id":"evt-1","data":{"sha":"abc"}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body=%v", resp.StatusCode, body)
	}
	if body["taskId"] != "task-downstream" || body["eventId"] != "evt-1" {
		t.Fatalf("成功回执不对：%v", body)
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.frames) != 1 {
		t.Fatalf("应转发 1 条 task.emit：%d", len(sender.frames))
	}
	f := sender.frames[0]
	if f.TaskID != "t1" || f.Attempt != 1 || f.LeaseID != "lease-1" || f.AttemptToken != "tok-1" {
		t.Fatalf("帧身份字段不对：%+v", f)
	}
	if f.EmitRequest.To != "fixer" || f.EmitRequest.ID != "evt-1" {
		t.Fatalf("转发内容不对：%+v", f.EmitRequest)
	}
	if len(f.RequestID) < 16 {
		t.Fatalf("requestId 必须是高熵关联 id：%q", f.RequestID)
	}
}

func TestEmitLocalPrechecks(t *testing.T) {
	_, _, srv := newTestGateway(t)
	cases := []struct {
		name   string
		token  string
		body   string
		status int
		code   string
	}{
		{"缺令牌", "", `{"to":"fixer"}`, 401, protocol.ReasonTokenInvalid},
		{"未知令牌", "tok-x", `{"to":"fixer"}`, 401, protocol.ReasonTokenInvalid},
		{"缺目标", "tok-1", `{}`, 400, protocol.ReasonMissingTarget},
		{"目标未申报", "tok-1", `{"to":"deployer"}`, 400, protocol.ReasonTargetNotDeclared},
		{"data 非对象", "tok-1", `{"to":"fixer","data":[1]}`, 400, protocol.ReasonParamType},
		{"坏 JSON", "tok-1", `{`, 400, protocol.ReasonParamType},
	}
	for _, tc := range cases {
		resp, body := postEmit(t, srv, tc.token, tc.body)
		if resp.StatusCode != tc.status || body["error"] != tc.code {
			t.Fatalf("%s: status=%d body=%v（期望 %d/%s）", tc.name, resp.StatusCode, body, tc.status, tc.code)
		}
	}
}

func TestEmitAckErrorsMapToStatus(t *testing.T) {
	gw, sender, srv := newTestGateway(t)
	sender.onSend = func(f *protocol.TaskEmitFrame) {
		gw.HandleEmitAck(&protocol.TaskEmitAckFrame{
			Type: protocol.FrameTaskEmitAck, RequestID: f.RequestID,
			TaskID: f.TaskID, Attempt: f.Attempt, HTTPStatus: 409,
			Error: &protocol.EmitError{Error: protocol.ReasonEmitIDConflict, Message: "conflict"},
		})
	}
	resp, body := postEmit(t, srv, "tok-1", `{"to":"fixer","id":"dup"}`)
	if resp.StatusCode != 409 || body["error"] != protocol.ReasonEmitIDConflict {
		t.Fatalf("中枢 409 必须原样回：status=%d body=%v", resp.StatusCode, body)
	}
}

func TestEmitOfflineIsUnknownNotSuccess(t *testing.T) {
	_, sender, srv := newTestGateway(t)
	sender.offline = true
	resp, body := postEmit(t, srv, "tok-1", `{"to":"fixer"}`)
	if resp.StatusCode != http.StatusBadGateway || body["error"] != protocol.ReasonEmitUnreachable {
		t.Fatalf("离线必须是 emit_unreachable（结果未知）：%d %v", resp.StatusCode, body)
	}
}

func TestEmitTimeoutIsUnreachableAndLateAckDropped(t *testing.T) {
	gw, sender, srv := newTestGateway(t)
	gw.Timeout = 50 * time.Millisecond
	sent := make(chan *protocol.TaskEmitFrame, 1)
	sender.onSend = func(f *protocol.TaskEmitFrame) { sent <- f }
	resp, body := postEmit(t, srv, "tok-1", `{"to":"fixer"}`)
	if resp.StatusCode != http.StatusGatewayTimeout || body["error"] != protocol.ReasonEmitUnreachable {
		t.Fatalf("超时必须 emit_unreachable：%d %v", resp.StatusCode, body)
	}
	if gw.Inflight() != 0 {
		t.Fatalf("超时后等待表必须清空：%d", gw.Inflight())
	}
	var captured *protocol.TaskEmitFrame
	select {
	case captured = <-sent:
	case <-time.After(time.Second):
		t.Fatal("emit was never forwarded before timeout")
	}
	// 迟到的 ACK：丢弃，不 panic、不派给别的请求。
	gw.HandleEmitAck(&protocol.TaskEmitAckFrame{
		Type: protocol.FrameTaskEmitAck, RequestID: captured.RequestID,
		TaskID: "t1", Attempt: 1, HTTPStatus: 200,
		Response: &protocol.EmitResponse{To: "fixer"},
	})
}

func TestEmitAckWithWrongIdentityIsDropped(t *testing.T) {
	gw, sender, srv := newTestGateway(t)
	gw.Timeout = 80 * time.Millisecond
	sender.onSend = func(f *protocol.TaskEmitFrame) {
		// 身份不符（另一个 attempt）的 ACK：不能交给当前 HTTP 请求。
		gw.HandleEmitAck(&protocol.TaskEmitAckFrame{
			Type: protocol.FrameTaskEmitAck, RequestID: f.RequestID,
			TaskID: f.TaskID, Attempt: f.Attempt + 1, HTTPStatus: 200,
			Response: &protocol.EmitResponse{To: f.EmitRequest.To},
		})
	}
	resp, body := postEmit(t, srv, "tok-1", `{"to":"fixer"}`)
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("错身份的 ACK 不能让请求成功：%d %v", resp.StatusCode, body)
	}
}

func TestEmitBodyTooLargeIsNotTruncated(t *testing.T) {
	_, _, srv := newTestGateway(t)
	huge := `{"to":"fixer","data":{"x":"` + strings.Repeat("a", protocol.FrameSoftLimit) + `"}}`
	resp, body := postEmit(t, srv, "tok-1", huge)
	if resp.StatusCode != http.StatusRequestEntityTooLarge || body["error"] != CodePayloadTooLarge {
		t.Fatalf("超上限必须 413 且不截断：%d %v", resp.StatusCode, body)
	}
}

func TestEmitRejectsNonPost(t *testing.T) {
	_, _, srv := newTestGateway(t)
	resp, err := srv.Client().Get(srv.URL + protocol.EmitPath)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("非 POST 必须 405：%d", resp.StatusCode)
	}
}
