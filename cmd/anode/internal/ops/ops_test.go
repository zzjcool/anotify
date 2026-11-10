package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/anotify/cmd/anode/internal/resultstore"
	"github.com/zzjcool/anotify/internal/protocol"
)

func testStore(t *testing.T) *resultstore.Store {
	t.Helper()
	dir := t.TempDir()
	return resultstore.New(filepath.Join(dir, "results"), filepath.Join(dir, "work"))
}

func writeLogs(t *testing.T, s *resultstore.Store, taskID string, attempt int, stdout, stderr string) {
	t.Helper()
	rec, err := s.Begin(resultstore.Record{
		TaskID: taskID, Node: "node-a", Attempt: attempt, Capability: "echo",
		Digest: "sha256:aa", TTL: 0, Phase: "succeeded",
		CreatedAt: time.Now().UTC(), Manifest: []resultstore.Manifest{},
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for name, text := range map[string]string{"stdout.log": stdout, "stderr.log": stderr} {
		f, err := rec.OpenLogFile(name)
		if err != nil {
			t.Fatalf("OpenLogFile: %v", err)
		}
		if _, err := f.WriteString(text); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
}

func TestLogsRequestReturnsExactAttempt(t *testing.T) {
	store := testStore(t)
	writeLogs(t, store, "t1", 1, "first-out", "first-err")
	writeLogs(t, store, "t1", 2, "second-out", "second-err")
	agent := &Agent{Results: store}

	resp := agent.HandleLogsRequest(context.Background(), &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: "t1", RequestID: "r1", Attempt: 1,
	})
	if resp == nil || resp.Error != "" || resp.Attempt != 1 ||
		resp.Stdout != "first-out" || resp.Stderr != "first-err" {
		t.Fatalf("精确 attempt 读档不对：%+v", resp)
	}
	// 省略 attempt = 最近一次有档案的 attempt。
	resp = agent.HandleLogsRequest(context.Background(), &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: "t1", RequestID: "r2",
	})
	if resp.Attempt != 2 || resp.Stdout != "second-out" {
		t.Fatalf("latest 读档不对：%+v", resp)
	}
	// 无档案：不能伪造 latest/空成功。
	resp = agent.HandleLogsRequest(context.Background(), &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: "missing", RequestID: "r3",
	})
	if resp.Error != protocol.LogsErrNoLocalArchive || resp.Attempt != 0 || resp.Stdout != "" {
		t.Fatalf("无档案必须是 no_local_archive：%+v", resp)
	}
}

func TestLogsResponseTruncatedWithinLimit(t *testing.T) {
	store := testStore(t)
	big := strings.Repeat("x", 2*protocol.LogsResponseMaxBytes)
	writeLogs(t, store, "t1", 1, big, "err-tail")
	agent := &Agent{Results: store}
	resp := agent.HandleLogsRequest(context.Background(), &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: "t1", RequestID: "r1", Attempt: 1,
	})
	if !resp.Truncated {
		t.Fatal("超过帧上限必须标记 truncated")
	}
	raw, err := protocol.Encode(resp)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(raw) > protocol.LogsResponseMaxBytes {
		t.Fatalf("回包必须 ≤ %d 字节，实际 %d", protocol.LogsResponseMaxBytes, len(raw))
	}
	if !strings.HasSuffix(resp.Stdout, "x") || !strings.HasSuffix(resp.Stderr, "err-tail") {
		t.Fatal("截断必须取尾部（最近的日志）")
	}
	if resp.Bytes != int64(len(resp.Stdout)+len(resp.Stderr)) {
		t.Fatalf("bytes 必须是实际返回的字节数：%d", resp.Bytes)
	}
	// 本机档案完整（截的只是回包）。
	_, stdout, _, found, err := store.Logs("t1", 1)
	if err != nil || !found || len(stdout) != len(big) {
		t.Fatalf("本机档案不能被截断：%d err=%v", len(stdout), err)
	}
}

type fakeDrainer struct {
	mu     sync.Mutex
	called bool
}

func (f *fakeDrainer) Drain(ctx context.Context) {
	f.mu.Lock()
	f.called = true
	f.mu.Unlock()
}

func testAgent(t *testing.T, target string) (*Agent, *fakeDrainer, *[]protocol.UpgradeAckFrame, *bool) {
	t.Helper()
	drainer := &fakeDrainer{}
	var acked []protocol.UpgradeAckFrame
	var restarted bool
	agent := &Agent{
		Results: testStore(t),
		Runner:  drainer,
		Upgrade: UpgradeConfig{TargetPath: target},
		Restart: func() { restarted = true },
	}
	return agent, drainer, &acked, &restarted
}

func TestUpgradeReplacesTargetAndRestarts(t *testing.T) {
	binary := []byte("#!/bin/sh\nprintf '%s\\n' '{\"version\":\"1.5.0\",\"commit\":\"c2\"}'\n")
	sum := sha256.Sum256(binary)
	sha := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/node/binary" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(binary)
	}))
	t.Cleanup(srv.Close)

	target := filepath.Join(t.TempDir(), "anode")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	agent, drainer, acked, restarted := testAgent(t, target)
	agent.Upgrade.ServerURL = "ws" + strings.TrimPrefix(srv.URL, "http")
	agent.Upgrade.NodeToken = "node-token"
	agent.Upgrade.HTTP = srv.Client()

	agent.HandleUpgrade(context.Background(), &protocol.UpgradeFrame{
		Type: protocol.FrameUpgrade, TargetVersion: "1.5.0", SHA256: sha,
	}, func(frame any) error {
		ack := frame.(*protocol.UpgradeAckFrame)
		*acked = append(*acked, *ack)
		return nil
	})
	if len(*acked) != 1 || !(*acked)[0].OK || (*acked)[0].Version != "1.5.0" || (*acked)[0].Commit != "c2" || (*acked)[0].Error != "" {
		t.Fatalf("upgrade_ack 不对：%+v", *acked)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != string(binary) {
		t.Fatalf("目标二进制必须被原子替换：%q err=%v", data, err)
	}
	if !*restarted {
		t.Fatal("替换成功后必须触发重启钩子")
	}
	if !drainer.called {
		t.Fatal("升级前必须先 drain（停止授权/回收进程组）")
	}
}

func TestUpgradeBadSHADoesNotReplaceOrRestart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("evil"))
	}))
	t.Cleanup(srv.Close)
	target := filepath.Join(t.TempDir(), "anode")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	agent, _, acked, restarted := testAgent(t, target)
	agent.Upgrade.ServerURL = "ws" + strings.TrimPrefix(srv.URL, "http")
	agent.Upgrade.HTTP = srv.Client()

	agent.HandleUpgrade(context.Background(), &protocol.UpgradeFrame{
		Type: protocol.FrameUpgrade, TargetVersion: "9.9.9",
		SHA256: strings.Repeat("ab", 32),
	}, func(frame any) error {
		ack := frame.(*protocol.UpgradeAckFrame)
		*acked = append(*acked, *ack)
		return nil
	})
	if len(*acked) != 1 || (*acked)[0].OK || (*acked)[0].Error != protocol.UpgradeErrBadSHA {
		t.Fatalf("sha 不符必须 bad_sha：%+v", *acked)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "old" {
		t.Fatalf("校验失败不能替换旧二进制：%q", data)
	}
	if *restarted {
		t.Fatal("失败不能重启")
	}
}

func TestUpgradeRejectsForeignOrigin(t *testing.T) {
	target := filepath.Join(t.TempDir(), "anode")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	agent, _, acked, restarted := testAgent(t, target)
	agent.Upgrade.ServerURL = "ws://127.0.0.1:1"
	agent.HandleUpgrade(context.Background(), &protocol.UpgradeFrame{
		Type: protocol.FrameUpgrade, TargetVersion: "2",
		SHA256: strings.Repeat("ab", 32), URLPath: "https://evil.example.com/bin",
	}, func(frame any) error {
		ack := frame.(*protocol.UpgradeAckFrame)
		*acked = append(*acked, *ack)
		return nil
	})
	if len(*acked) != 1 || (*acked)[0].OK || (*acked)[0].Error != protocol.UpgradeErrDownloadFailed {
		t.Fatalf("跨源下载必须拒绝：%+v", *acked)
	}
	if *restarted {
		t.Fatal("拒绝升级不能重启")
	}
}
