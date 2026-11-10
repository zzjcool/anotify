package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

type binaryFixture struct {
	t                            *testing.T
	ctx                          context.Context
	cancel                       context.CancelFunc
	conns                        chan *websocket.Conn
	server                       *httptest.Server
	binary, data, config, socket string
	done                         chan error
	logs                         *os.File
	ackResults                   bool
}

func realFixture(t *testing.T, a *artifact.Artifact, newBinary []byte) *binaryFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	root := t.TempDir()
	f := &binaryFixture{t: t, ctx: ctx, cancel: cancel, conns: make(chan *websocket.Conn, 4), binary: filepath.Join(root, "anode"), data: filepath.Join(root, "data"), config: filepath.Join(root, "anode.yaml"), socket: filepath.Join(root, "emit.sock"), done: make(chan error, 1), ackResults: true}
	b, err := os.ReadFile(builtOld)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.binary, b, 0755); err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer node-identity" {
			t.Error("missing Node Bearer")
			http.Error(w, "forbidden", 401)
			return
		}
		switch {
		case r.URL.Path == protocol.NodeStreamPath:
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			select {
			case f.conns <- conn:
			case <-ctx.Done():
				conn.CloseNow()
				return
			}
			<-ctx.Done()
			conn.CloseNow()
		case r.URL.Path == protocol.NodeBinaryPath:
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(newBinary)
		case strings.HasPrefix(r.URL.Path, "/v1/node/capabilities/"):
			if a == nil {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(a)
		default:
			http.NotFound(w, r)
		}
	}))
	put(t, f.config, "name: node-a\nserver: ws"+strings.TrimPrefix(f.server.URL, "http")+"\ntoken: node-identity\nlisten: unix://"+f.socket+"\n", 0600)
	f.logs, err = os.Create(filepath.Join(root, "node.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, f.binary, "serve", "--config", f.config, "--data", f.data)
	// Keep only service baseline; a random credential must NOT become a task variable.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + root, "N3_UNLISTED_SECRET=not-authorized"}
	cmd.Stdout = f.logs
	cmd.Stderr = f.logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { f.done <- cmd.Wait(); close(f.done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-f.done:
		case <-time.After(8 * time.Second):
			cancel()
			<-f.done
		}
		cancel()
		f.server.Close()
		f.logs.Close()
		if t.Failed() {
			b, _ := os.ReadFile(f.logs.Name())
			t.Logf("real node log:\n%s", b)
		}
	})
	return f
}
func (f *binaryFixture) conn() *websocket.Conn {
	f.t.Helper()
	select {
	case c := <-f.conns:
		return c
	case err := <-f.done:
		f.t.Fatalf("binary exited: %v", err)
	case <-f.ctx.Done():
		f.t.Fatal("real node connection timeout")
	}
	return nil
}
func (f *binaryFixture) write(c *websocket.Conn, v any) {
	f.t.Helper()
	b, err := protocol.Encode(v)
	if err == nil {
		err = c.Write(f.ctx, websocket.MessageText, b)
	}
	if err != nil {
		f.t.Fatalf("write frame: %v", err)
	}
}
func (f *binaryFixture) until(c *websocket.Conn, pred func(any) bool, hook func(any)) any {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
	defer cancel()
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			f.t.Fatalf("read frame: %v", err)
		}
		v, err := protocol.Decode(b)
		if err != nil {
			f.t.Fatalf("decode: %v: %s", err, b)
		}
		if renew, ok := v.(*protocol.LeaseRenewFrame); ok {
			f.write(c, &protocol.LeaseAckFrame{Type: protocol.FrameLeaseAck, TaskID: renew.TaskID, Attempt: renew.Attempt, LeaseID: renew.LeaseID, ExpiresAt: time.Now().Add(protocol.LeaseTTL)})
		}
		if result, ok := v.(*protocol.TaskResultFrame); ok && f.ackResults {
			f.write(c, &protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: result.TaskID, Attempt: result.Attempt, Accepted: true})
		}
		if hook != nil {
			hook(v)
		}
		if pred(v) {
			return v
		}
	}
}
func (f *binaryFixture) welcome(c *websocket.Conn) *protocol.HelloFrame {
	hello := f.until(c, func(v any) bool { _, ok := v.(*protocol.HelloFrame); return ok }, nil).(*protocol.HelloFrame)
	f.write(c, &protocol.WelcomeFrame{Type: protocol.FrameWelcome, ConfigRevision: "r1", Namespaces: []string{"default"}, NodeVars: map[string]string{}, NodeLimits: protocol.Limits{Total: 1}})
	f.until(c, func(v any) bool { _, ok := v.(*protocol.ConfigAppliedFrame); return ok }, nil)
	return hello
}
func makeCapability(t *testing.T, script string) (*artifact.Artifact, string) {
	t.Helper()
	zero, one := 0, 1
	a := &artifact.Artifact{APIVersion: "anotify/v1", Kind: "Capability", Metadata: artifact.Metadata{Name: "emitter", Version: 1}, Spec: artifact.Spec{Params: json.RawMessage(`{}`), Next: []string{"next"}, Run: artifact.Run{Entrypoint: "run.sh", Files: map[string]string{"run.sh": script}}, Lifecycle: artifact.Lifecycle{ResultTTLSec: &zero}, Concurrency: artifact.Concurrency{MaxPerNode: &one}}}
	if err := artifact.Validate(a); err != nil {
		t.Fatal(err)
	}
	digest, err := artifact.Digest(a)
	if err != nil {
		t.Fatal(err)
	}
	return a, digest
}
func (f *binaryFixture) deploy(c *websocket.Conn, digest string) {
	f.write(c, &protocol.CapabilityDeployFrame{Type: protocol.FrameCapabilityDeploy, RequestID: "deploy", DeployRefs: protocol.DeployRefs{Namespace: "default", Name: "emitter", Version: 1, Digest: digest}})
	deployed := f.until(c, func(v any) bool { _, ok := v.(*protocol.CapabilityDeployedFrame); return ok }, nil).(*protocol.CapabilityDeployedFrame)
	if deployed.Status != protocol.DeployStatusApplied {
		f.t.Fatalf("real GET deploy: %+v", deployed)
	}
}
func (f *binaryFixture) grant(c *websocket.Conn, digest string, attempt int) {
	id := fmt.Sprintf("assignment-%d", attempt)
	f.write(c, &protocol.TaskDispatchFrame{Type: protocol.FrameTaskDispatch, TaskID: "logical-task", AssignmentID: id, ProposedAttempt: attempt, Namespace: "default", Capability: protocol.CapabilityRef{Name: "emitter", Version: 1, Digest: digest}, Params: json.RawMessage(`{}`), ConfigRevision: "r1", AssignmentExpiresAt: time.Now().Add(30 * time.Second)})
	accepted := f.until(c, func(v any) bool { _, ok := v.(*protocol.TaskAcceptFrame); return ok }, nil).(*protocol.TaskAcceptFrame)
	if accepted.Slot == nil || *accepted.Slot != 0 {
		f.t.Fatal("missing real slot 0")
	}
	f.write(c, &protocol.LeaseGrantFrame{Type: protocol.FrameLeaseGrant, TaskID: "logical-task", Node: "node-a", Attempt: attempt, AssignmentID: id, Slot: accepted.Slot, LeaseID: fmt.Sprintf("lease-%d", attempt), AttemptToken: fmt.Sprintf("attempt-token-%d", attempt), ExpiresAt: time.Now().Add(protocol.LeaseTTL)})
}

// A real capability executes the built CLI, which goes through the Unix gateway and
// E1 WebSocket to a fake authoritative hub. No HTTP-only DTO or shell curl stub is used.
func TestRealCapabilityBinaryEmitWSAuthorityAcrossAttempts(t *testing.T) {
	// builtOld is a real Go binary; its path is embedded explicitly, not inherited via credentials/PATH.
	script := fmt.Sprintf(`#!/bin/sh
set -eu
[ -z "${N3_UNLISTED_SECRET:-}" ]
'%s' emit next --id task --data '{"value":"same"}'
'%s' emit next --id prune --data '{"value":"prune"}'
set +e
'%s' emit next --id task --data '{"value":"different"}'
code=$?
set -e
[ "$code" -eq 6 ]
set +e
ANOTIFY_TASK_TOKEN=attempt-token-1 '%s' emit next --id task --data '{"value":"same"}'
code=$?
set -e
# The old token is current on attempt 1; on attempt 2 it MUST be rejected before id lookup.
if [ "$ANOTIFY_TASK_TOKEN" = attempt-token-1 ]; then [ "$code" -eq 0 ]; else [ "$code" -eq 3 ]; fi
'%s' emit next --data '{}'
'%s' emit next --data '{}'
`, builtOld, builtOld, builtOld, builtOld, builtOld, builtOld)
	a, digest := makeCapability(t, script)
	f := realFixture(t, a, nil)
	c := f.conn()
	hello := f.welcome(c)
	if hello.AgentVersion != "old-v1" || hello.AgentCommit != "old-commit" {
		t.Fatal("hello differs from actual --version")
	}
	f.deploy(c, digest)
	type receipt struct {
		body     string
		response protocol.EmitResponse
	}
	ledger := map[string]receipt{}
	events := 0
	received := []*protocol.TaskEmitFrame{}
	ackEmit := func(v any) {
		e, ok := v.(*protocol.TaskEmitFrame)
		if !ok {
			return
		}
		received = append(received, e)
		ack := &protocol.TaskEmitAckFrame{Type: protocol.FrameTaskEmitAck, RequestID: e.RequestID, TaskID: e.TaskID, Attempt: e.Attempt, HTTPStatus: 200}
		// Authority before business id lookup. Never recover a receipt using stale authorization.
		if e.AttemptToken != fmt.Sprintf("attempt-token-%d", e.Attempt) || e.LeaseID != fmt.Sprintf("lease-%d", e.Attempt) {
			ack.HTTPStatus = 401
			ack.Error = &protocol.EmitError{Error: protocol.ReasonTokenInvalid}
			f.write(c, ack)
			return
		}
		body := e.To + "|" + string(e.Data)
		key := e.TaskID + "|" + e.ID
		if old, ok := ledger[key]; ok && e.ID != "" {
			if old.body != body {
				ack.HTTPStatus = 409
				ack.Error = &protocol.EmitError{Error: protocol.ReasonEmitIDConflict}
			} else {
				r := old.response
				ack.Response = &r
			}
		} else {
			events++
			taskID := fmt.Sprintf("down-%d", events)
			r := protocol.EmitResponse{TaskID: &taskID, EventID: fmt.Sprintf("event-%d", events), To: e.To, Version: 1}
			if e.ID == "prune" {
				r.TaskID = nil
				r.Pruned = &protocol.EmitPruned{Reason: protocol.ReasonParamType}
			}
			ack.Response = &r
			if e.ID != "" {
				ledger[key] = receipt{body: body, response: r}
			}
		}
		f.write(c, ack)
	}
	var attemptOutputs [][]map[string]any
	for attempt := 1; attempt <= 2; attempt++ {
		f.grant(c, digest, attempt)
		result := f.until(c, func(v any) bool { r, ok := v.(*protocol.TaskResultFrame); return ok && r.Attempt == attempt }, ackEmit).(*protocol.TaskResultFrame)
		if result.Phase != protocol.TaskPhaseSucceeded {
			t.Fatalf("capability emit script failed: %+v", result)
		}
		f.write(c, &protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: result.TaskID, Attempt: attempt, Accepted: true})
		f.write(c, &protocol.LogsRequestFrame{Type: protocol.FrameLogsRequest, TaskID: result.TaskID, Attempt: attempt, RequestID: fmt.Sprintf("logs-%d", attempt)})
		logs := f.until(c, func(v any) bool { r, ok := v.(*protocol.LogsResponseFrame); return ok && r.Attempt == attempt }, ackEmit).(*protocol.LogsResponseFrame)
		if logs.Error != "" {
			t.Fatalf("logs: %+v", logs)
		}
		lines := strings.Split(strings.TrimSpace(logs.Stdout), "\n")
		if len(lines) != 6 {
			t.Fatalf("actual CLI receipts: %d %s", len(lines), logs.Stdout)
		}
		var output []map[string]any
		for _, line := range lines {
			var r map[string]any
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				t.Fatal(err)
			}
			output = append(output, r)
		}
		if output[2]["error"] != "emit_id_conflict" || output[1]["taskId"] != nil || output[1]["pruned"] == nil {
			t.Fatalf("conflict/pruned receipts: %+v", output)
		}
		if attempt == 2 && output[3]["error"] != "token_invalid" {
			t.Fatal("old token bypassed current attempt authorization")
		}
		if output[4]["taskId"] == output[5]["taskId"] {
			t.Fatal("no-id calls deduplicated")
		}
		attemptOutputs = append(attemptOutputs, output)
	}
	if attemptOutputs[0][0]["taskId"] != attemptOutputs[1][0]["taskId"] || attemptOutputs[0][1]["eventId"] != attemptOutputs[1][1]["eventId"] {
		t.Fatal("cross-attempt logical id did not return original task/pruned event")
	}
	if events != 6 || len(received) != 11 {
		t.Fatalf("authority facts: created=%d WS calls=%d", events, len(received))
	}
	code, out, _ := binaryRun(t, builtOld, []string{"ANOTIFY_EMIT_ENDPOINT=unix://" + f.socket, "ANOTIFY_TASK_TOKEN=attempt-token-2"}, "emit", "next", "--id", "task")
	if code != 3 || !strings.Contains(out, "token_invalid") {
		t.Fatalf("late token must be refused locally: %d %s", code, out)
	}
	t.Logf("real binary → script → Unix gateway → E1 WS → authority → ACK: 2 attempts, 11 WS calls, 6 creations; old token rejected before ledger")
}

func TestRealCurrentBinaryUpgradeReplacesRestartsReconnectsAndReaps(t *testing.T) {
	newBytes, err := os.ReadFile(builtNew)
	if err != nil {
		t.Fatal(err)
	}
	a, digest := makeCapability(t, fmt.Sprintf("#!/bin/sh\nsleep 50 &\n'%s' emit next --id ready --data '{}'\nwait\n", builtOld))
	f := realFixture(t, a, newBytes)
	c := f.conn()
	f.welcome(c)
	f.deploy(c, digest)
	f.grant(c, digest, 1)
	// The script has spawned sleep and a real CLI in the same process group. Hold its ACK
	// so both are genuinely alive while upgrade drains, instead of racing grant vs spawn.
	ready := f.until(c, func(v any) bool { _, ok := v.(*protocol.TaskEmitFrame); return ok }, nil).(*protocol.TaskEmitFrame)
	if ready.ID != "ready" || ready.Attempt != 1 || ready.LeaseID != "lease-1" {
		t.Fatalf("running readiness: %+v", ready)
	}
	sum := sha256.Sum256(newBytes)
	f.write(c, &protocol.UpgradeFrame{Type: protocol.FrameUpgrade, TargetVersion: "requested-label-must-not-win", URLPath: protocol.NodeBinaryPath, SHA256: hex.EncodeToString(sum[:])})
	reaped := false
	ack := f.until(c, func(v any) bool { _, ok := v.(*protocol.UpgradeAckFrame); return ok }, func(v any) {
		if a, ok := v.(*protocol.CancelAckFrame); ok && a.TaskID == "logical-task" && a.LeaseID == "lease-1" && a.Slot != nil && *a.Slot == 0 && a.ProcessGroupReaped {
			reaped = true
		}
	}).(*protocol.UpgradeAckFrame)
	if !ack.OK || ack.Version != "actual-v2" || ack.Commit != "new-commit" {
		t.Fatalf("actual selftest ACK: %+v", ack)
	}
	got, err := os.ReadFile(f.binary)
	if err != nil || !bytes.Equal(got, newBytes) {
		t.Fatal("current on-disk binary not replaced")
	}
	next := f.conn()
	hello := f.until(next, func(v any) bool { _, ok := v.(*protocol.HelloFrame); return ok }, nil).(*protocol.HelloFrame)
	if hello.AgentVersion != "actual-v2" || hello.AgentCommit != "new-commit" {
		t.Fatalf("real restart not new binary: %+v", hello)
	}
	if !reaped {
		t.Fatal("upgrade lacked historical lease/slot processGroupReaped ACK before restart")
	}
	for _, slot := range hello.Instances {
		if slot.Busy == nil || *slot.Busy {
			t.Fatalf("restart reported unreaped/unknown slot: %+v", slot)
		}
	}
	t.Log("current executable replaced by raw new binary; actual version/commit ACK; exec restart reconnects with reaped idle slots")
}
func TestRealCurrentBinaryBadSHAKeepsOldProcessAndFile(t *testing.T) {
	newBytes, err := os.ReadFile(builtNew)
	if err != nil {
		t.Fatal(err)
	}
	f := realFixture(t, nil, newBytes)
	c := f.conn()
	f.welcome(c)
	before, err := os.ReadFile(f.binary)
	if err != nil {
		t.Fatal(err)
	}
	f.write(c, &protocol.UpgradeFrame{Type: protocol.FrameUpgrade, SHA256: strings.Repeat("0", 64), TargetVersion: "bad"})
	ack := f.until(c, func(v any) bool { _, ok := v.(*protocol.UpgradeAckFrame); return ok }, nil).(*protocol.UpgradeAckFrame)
	if ack.OK || ack.Error != protocol.UpgradeErrBadSHA {
		t.Fatalf("bad SHA: %+v", ack)
	}
	after, err := os.ReadFile(f.binary)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("bad SHA replaced current binary")
	}
	// Same old connection remains responsive; no restart/reconnect may be reported.
	f.write(c, &protocol.LogsRequestFrame{Type: protocol.FrameLogsRequest, TaskID: "none", RequestID: "after-bad"})
	f.until(c, func(v any) bool { r, ok := v.(*protocol.LogsResponseFrame); return ok && r.RequestID == "after-bad" }, nil)
	select {
	case <-f.conns:
		t.Fatal("bad SHA triggered restart")
	default:
	}
	code, out, _ := binaryRun(t, f.binary, nil, "--version")
	if code != 0 || !strings.Contains(out, "old-v1") {
		t.Fatalf("old binary lost: %d %s", code, out)
	}
	t.Log("bad SHA: ACK bad_sha, old executable unchanged, same old WS still responsive, no reconnect")
}
