package main

// 中枢下发升级（CONTRACT-agent-upgrade §5）的用例。
//
// 这一组测的是「中枢推一帧 upgrade 之后，节点到底做了什么」——它是整条控制台
// 升级路径上**唯一没有 supervisor 兜底**的一段：下载、校验、替换写错了，症状是
// 「控制台显示已最新，节点跑的还是旧的」或者「节点把自己搞没了」。所以这里用真
// HTTP 服务 + 真文件系统，逐条钉住结论码与文件状态。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// upgradeTestAgent 造一个只够跑 handleUpgrade 的 agent（不连 WS）。
func upgradeTestAgent(t *testing.T, serverURL string) *nodeAgent {
	t.Helper()
	cfg := &nodeConfig{Server: serverURL, Token: "nt_test", Name: "test-node"}
	conn := &captureConn{}
	return newNodeAgent(t.Context(), cfg, conn, t.TempDir(), slog.New(slog.NewTextHandler(os.Stderr, nil)))
}

// captureConn 记录 agent 写出去的帧（handleUpgrade 只写一帧 upgrade_ack）。
type captureConn struct {
	frames [][]byte
}

func (c *captureConn) read(_ context.Context) ([]byte, error) { return nil, errNodeConnClosed }
func (c *captureConn) write(_ context.Context, b []byte) error {
	c.frames = append(c.frames, append([]byte(nil), b...))
	return nil
}
func (c *captureConn) close() error { return nil }

// ackOf 解析最后一帧 upgrade_ack。
func (c *captureConn) ackOf(t *testing.T) *upgradeAckFrame {
	t.Helper()
	if len(c.frames) == 0 {
		t.Fatal("agent 没有写任何帧（期望一帧 upgrade_ack）")
	}
	var f upgradeAckFrame
	if err := json.Unmarshal(c.frames[len(c.frames)-1], &f); err != nil {
		t.Fatalf("最后一帧不是 upgrade_ack: %v (%s)", err, c.frames[len(c.frames)-1])
	}
	if f.Type != frameTypeUpgradeAck {
		t.Fatalf("最后一帧 type = %q, want %q", f.Type, frameTypeUpgradeAck)
	}
	return &f
}

// TestHandleUpgradeRejectsBadSHA 中枢声明的 sha 形状不合法 → 不下载、回 bad_sha。
//
// 「不下载」是重点：形状坏掉的校验和意味着帧本身有问题（版本不对/被改成别的
// 东西），此时去下载只是浪费带宽并扩大攻击面。
func TestHandleUpgradeRejectsBadSHA(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte("should-not-be-fetched"))
	}))
	defer srv.Close()

	a := upgradeTestAgent(t, srv.URL)
	a.handleUpgrade(&upgradeFrame{SHA256: "not-a-sha", URLPath: nodeBinaryPath, TargetVersion: "x"})

	if calls != 0 {
		t.Fatalf("sha 形状坏掉却发起了 %d 次下载，want 0", calls)
	}
	ack := a.conn.(*captureConn).ackOf(t)
	if ack.OK || ack.Error != upgradeErrBadSHA {
		t.Fatalf("ack = %+v, want ok=false error=bad_sha", ack)
	}
}

// TestHandleUpgradeRefusesForeignHost 帧里的绝对 URL 跑到了别的 host → 拒绝。
//
// 不校验同源的话，「谁能让中枢推一帧」就等于「谁能让每台节点以自���权限执行任意
// 二进制」。相对路径拼中枢；绝对地址只允许同源。
func TestHandleUpgradeRefusesForeignHost(t *testing.T) {
	fetched := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched = true
	}))
	defer srv.Close()

	a := upgradeTestAgent(t, "https://hub.example.com")
	sum := sha256.Sum256([]byte("x"))
	a.handleUpgrade(&upgradeFrame{
		SHA256: hex.EncodeToString(sum[:]),
		// 指向测试服务器的绝对地址（与中枢不同源）。
		URLPath:       srv.URL + "/v1/node/binary",
		TargetVersion: "1.2.3",
	})

	if fetched {
		t.Fatal("跨源的 url_path 被真的去下载了")
	}
	ack := a.conn.(*captureConn).ackOf(t)
	if ack.OK || ack.Error != upgradeErrDownloadFailed {
		t.Fatalf("ack = %+v, want ok=false error=download_failed", ack)
	}
}

// TestHandleUpgradeBadSHAOnDownload 下载内容与声明不符 → bad_sha，且**目标文件不动**。
//
// 这是整条路径上最要命的一种失败：把校验失败静默当成成功，等于把任意字节写进
// 以用户权限运行的二进制。
func TestHandleUpgradeBadSHAOnDownload(t *testing.T) {
	body := []byte("#!/bin/sh\necho hijacked\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	a := upgradeTestAgent(t, srv.URL)
	target := filepath.Join(t.TempDir(), "anotify-agent")
	if err := os.WriteFile(target, []byte("old-bytes"), 0o755); err != nil {
		t.Fatalf("write target: %v", err)
	}

	sum := sha256.Sum256([]byte("different content"))
	// 直接把目标路径塞进 resolveTarget 的返回：生产路径用的是 os.Executable，
	// 测试里不能替换自己，所以走 installTo 覆盖（与 CLI 的 --install-to 同一入口）。
	a.installToForTest = target
	a.handleUpgrade(&upgradeFrame{
		SHA256:        hex.EncodeToString(sum[:]),
		URLPath:       nodeBinaryPath,
		TargetVersion: "1.2.3",
	})

	ack := a.conn.(*captureConn).ackOf(t)
	if ack.OK || ack.Error != upgradeErrBadSHA {
		t.Fatalf("ack = %+v, want ok=false error=bad_sha", ack)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !bytes.Equal(got, []byte("old-bytes")) {
		t.Fatalf("校验失败却改动了目标文件：%q", got)
	}
	// 临时文件必须清掉（下载不得在目标目录里留垃圾）。
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(target) {
			t.Fatalf("目标目录残留临时文件：%s", e.Name())
		}
	}
}

// TestHandleUpgradeSuccess 下载 + 校验 + 原子替换 + 回 ok。
//
// 注意：成功路径在生产实现里会 os.Exit(0)（让 supervisor 拉起新版本）。这里用
// exitHook 把它拦下来，才能断言「替换真的发生了」与「ack 先发出去了」。
func TestHandleUpgradeSuccess(t *testing.T) {
	body := []byte("#!/bin/sh\necho v2\n")
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != nodeBinaryPath {
			t.Errorf("节点请求的路径 = %q, want %q", r.URL.Path, nodeBinaryPath)
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("下载没有带 node token：%q", got)
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	a := upgradeTestAgent(t, srv.URL)
	target := filepath.Join(t.TempDir(), "anotify-agent")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	a.installToForTest = target

	var exited int
	a.exitHook = func(code int) { exited = code }

	a.handleUpgrade(&upgradeFrame{
		SHA256:        hex.EncodeToString(sum[:]),
		URLPath:       nodeBinaryPath,
		TargetVersion: "1.5.0+abc",
	})

	ack := a.conn.(*captureConn).ackOf(t)
	if !ack.OK || ack.Version != "1.5.0+abc" || ack.Error != "" {
		t.Fatalf("ack = %+v, want ok=true version=1.5.0+abc", ack)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("目标文件内容 = %q, want %q", got, body)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("替换后的权限 = %o, want 0755", info.Mode().Perm())
	}
	if exited != 0 {
		t.Fatalf("成功替换后没有退出（exitHook 收到 %d，want 0）", exited)
	}

	// 顺序：ack 必须**先**写出去，再退出。反过来的话中枢永远收不到结果。
	if len(a.conn.(*captureConn).frames) != 1 {
		t.Fatalf("写了 %d 帧，want 恰好 1 帧 upgrade_ack", len(a.conn.(*captureConn).frames))
	}
}

// TestHandleUpgradeNeedRootDoesNotExit 目标目录不可写 → 回 need_root，**不退出**。
//
// 退出等于节点掉线，而用户此刻正需要看到「为什么没升上去」（要人去跑 sudo）。
func TestHandleUpgradeNeedRootDoesNotExit(t *testing.T) {
	body := []byte("#!/bin/sh\necho v2\n")
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	target := filepath.Join(dir, "anotify-agent")

	a := upgradeTestAgent(t, srv.URL)
	a.installToForTest = target
	exited := false
	a.exitHook = func(int) { exited = true }

	a.handleUpgrade(&upgradeFrame{
		SHA256:        hex.EncodeToString(sum[:]),
		URLPath:       nodeBinaryPath,
		TargetVersion: "1.5.0",
	})

	ack := a.conn.(*captureConn).ackOf(t)
	if ack.OK || ack.Error != upgradeErrNeedRoot {
		t.Fatalf("ack = %+v, want ok=false error=need_root", ack)
	}
	if exited {
		t.Fatal("need_root 时进程退出了（节点会掉线，用户也看不到原因）")
	}
}
