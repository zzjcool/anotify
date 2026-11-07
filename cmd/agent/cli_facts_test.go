package main

// 本文件是**本机事实面**的单测（issue #320）：`capabilities` / `status` / `state.json`。
//
// 钉的是三条容易在半年后悄悄错掉的事：
//
//  1. `capabilities` 的**空结果是 0**（不是 1）：与 `runs` 的空结果语义刻意相反，
//     这条语义错了不会有人报 bug，只会有人在脚本里写 `capabilities && …` 然后纳闷；
//  2. 真实索引下的三列内容与排序（name, version），并且 outputs 与产物一致；
//  3. `status` 的 live / stale 两态：pid 不在、updatedAt 太旧都要退 1（“最后一次已知
//     状态”），而不是把一份陈旧的快照当成 live。
//
// 夹具用真实的 cache.put 落产物（而不是手写 index/），这样索引与 artifacts/ 的
// 形状永远与生产路径一致。

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
)

// factsPut 把一份产物经真实路径（cache.put）落进 dataDir，返回它的 digest。
func factsPut(t *testing.T, dataDir string, art *artifact.Artifact) string {
	t.Helper()
	digest := mustDigest(t, art)
	if err := newCache(dataDir).put(art, digest); err != nil {
		t.Fatalf("cache.put(%s@%d): %v", art.Metadata.Name, art.Metadata.Version, err)
	}
	return digest
}

// factsArtifact 造一份带指定出口的产物。
func factsArtifact(name string, version int, outputs ...string) *artifact.Artifact {
	art := testScriptArtifact(name, version, bashScript("echo ok\n"))
	art.Spec.Outputs = outputs
	return art
}

// ---------- capabilities ----------

// TestCapabilitiesEmptyDataDirExitsZero 是验收里的第一条：
//
//	空 data-dir：capabilities --json → {"capabilities":[]}，退出 0；runs 仍退出 1。
//
// 两条语义不能混——所以这条用例同时断言 `runs` 的 1。
func TestCapabilitiesEmptyDataDirExitsZero(t *testing.T) {
	dataDir := t.TempDir()

	var stdout, stderr bytes.Buffer
	code, err := runCapabilities([]string{"--data-dir", dataDir, "--json"}, &stdout, &stderr)
	if err != nil || code != exitOK {
		t.Fatalf("capabilities 空结果 = (%d, %v)，want (0, nil)（stderr=%s）", code, err, stderr.String())
	}
	// 逐字节断言：空列表必须是 []，不是 null（AGENTS.md 的空列表纪律）。
	if got := stdout.String(); got != "{\"capabilities\":[]}\n" {
		t.Errorf("stdout = %q，want %q", got, "{\"capabilities\":[]}\n")
	}

	// 对照：同一个空目录，runs 是 1（“没有执行记录”几乎总是用户判断错了）。
	var runsOut, runsErr bytes.Buffer
	if code, err := runRuns([]string{"--data-dir", dataDir, "--json"}, &runsOut, &runsErr); code != exitFailure || err == nil {
		t.Errorf("runs 空结果 = (%d, %v)，want (1, error)：两条命令的空结果语义必须保持不同", code, err)
	}
}

// TestCapabilitiesListsCachedArtifacts 是验收第二条：真实 index 下三条都列出来，
// outputs 与产物一致，排序稳定（name、再 version）。
func TestCapabilitiesListsCachedArtifacts(t *testing.T) {
	dataDir := t.TempDir()
	// 故意乱序写入：输出顺序必须由排序决定，而不是 ReadDir 的偶然顺序。
	waoo := factsArtifact("waoo", 1)                        // 没有出口
	scriptEcho := factsArtifact("script.echo", 1, "result") // 一个出口
	haha := factsArtifact("haha", 1, "out", "err")          // 两个出口
	factsPut(t, dataDir, waoo)
	factsPut(t, dataDir, scriptEcho)
	factsPut(t, dataDir, haha)

	var stdout, stderr bytes.Buffer
	code, err := runCapabilities([]string{"--data-dir", dataDir, "--json"}, &stdout, &stderr)
	if err != nil || code != exitOK {
		t.Fatalf("capabilities = (%d, %v)（stderr=%s）", code, err, stderr.String())
	}

	var got capabilitiesJSON
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("解析 JSON: %v（stdout=%q）", err, stdout.String())
	}
	if len(got.Capabilities) != 3 {
		t.Fatalf("列出 %d 个能力，want 3：%+v", len(got.Capabilities), got.Capabilities)
	}
	wantNames := []string{"haha", "script.echo", "waoo"}
	for i, want := range wantNames {
		if got.Capabilities[i].Name != want {
			t.Errorf("第 %d 条 = %q，want %q（排序必须是 name、再 version）",
				i, got.Capabilities[i].Name, want)
		}
		if got.Capabilities[i].Version != 1 {
			t.Errorf("%s 的版本 = %d，want 1", want, got.Capabilities[i].Version)
		}
	}
	// outputs 必须与产物一致（emit --port X 的 X 就来自这里）。
	for name, want := range map[string][]string{
		"haha":        {"out", "err"},
		"script.echo": {"result"},
		"waoo":        {},
	} {
		infos := got.Capabilities
		var found *capabilityInfo
		for i := range infos {
			if infos[i].Name == name {
				found = &infos[i]
			}
		}
		if found == nil {
			t.Fatalf("没有 %q", name)
		}
		if strings.Join(found.Outputs, ",") != strings.Join(want, ",") {
			t.Errorf("%s 的 outputs = %v，want %v", name, found.Outputs, want)
		}
		if found.Outputs == nil {
			t.Errorf("%s 的 outputs 是 null，必须是 []（空列表纪律）", name)
		}
		if !strings.HasPrefix(found.Digest, "sha256:") {
			t.Errorf("%s 的 digest = %q，want sha256:…", name, found.Digest)
		}
		// dir 指向盘上真实存在的产物目录（用户要去看/校验时从这里出发）。
		if st, err := os.Stat(found.Dir); err != nil || !st.IsDir() {
			t.Errorf("%s 的 dir = %q 不是目录（err=%v）", name, found.Dir, err)
		}
	}

	// 文本模式（stdout 不是 *os.File → 默认文本）：三列 + digest 只打 12 位。
	stdout.Reset()
	if code, err := runCapabilities([]string{"--data-dir", dataDir}, &stdout, &stderr); code != exitOK || err != nil {
		t.Fatalf("capabilities 文本模式 = (%d, %v)", code, err)
	}
	text := stdout.String()
	for _, want := range []string{"NAME@VER", "OUTPUTS", "DIGEST", "haha@1", "script.echo@1", "waoo@1", "out,err"} {
		if !strings.Contains(text, want) {
			t.Errorf("表格里没有 %q：\n%s", want, text)
		}
	}
	// 12 位（不是 64 位）：agent 的上下文预算。
	digestOfHaha := factsDigestOf(t, dataDir, "haha", 1)
	if strings.Contains(text, strings.TrimPrefix(digestOfHaha, "sha256:")) {
		t.Errorf("表格打出了完整 digest（应当只打前 12 位）：\n%s", text)
	}
	if !strings.Contains(text, shortDigest(digestOfHaha)) {
		t.Errorf("表格里没有短 digest %q：\n%s", shortDigest(digestOfHaha), text)
	}
	if shortDigest(digestOfHaha) != strings.TrimPrefix(digestOfHaha, "sha256:")[:12] {
		t.Errorf("shortDigest = %q，want 去掉 sha256: 前缀的前 12 位", shortDigest(digestOfHaha))
	}
}

// factsDigestOf 从索引里取一条记录的 digest（测试断言 12 位截断用）。
func factsDigestOf(t *testing.T, dataDir, name string, version int) string {
	t.Helper()
	digest, ok := newCache(dataDir).readIndex(name, version)
	if !ok {
		t.Fatalf("索引里没有 %s@%d", name, version)
	}
	return digest
}

// TestCapabilitiesHelpExplainsEmptyIsZero：`-h` 必须写出「空结果 = 0」的理由，
// 否则下一个人会把它当 bug「修」成 1（issue §1 明说这一点要进 help）。
func TestCapabilitiesHelpExplainsEmptyIsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code, err := runCapabilities([]string{"-h"}, &stdout, &stderr); code != exitOK || err != nil {
		t.Fatalf("capabilities -h = (%d, %v)", code, err)
	}
	help := stderr.String()
	for _, want := range []string{"空结果", "0", "runs"} {
		if !strings.Contains(help, want) {
			t.Errorf("capabilities -h 没有说明 %q：\n%s", want, help)
		}
	}
}

// ---------- status ----------

// factsWriteState 直接写一份 state.json（模拟 daemon 写过的最后一拍）。
func factsWriteState(t *testing.T, dataDir string, st agentState) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("建数据目录: %v", err)
	}
	data, err := marshalIndent(st)
	if err != nil {
		t.Fatalf("编码 state.json: %v", err)
	}
	if err := os.WriteFile(statePath(dataDir), data, 0o600); err != nil {
		t.Fatalf("写 state.json: %v", err)
	}
}

// factsLiveState 造一份「刚刚还活着」的快照（pid = 本进程，updatedAt = 现在）。
func factsLiveState() agentState {
	return agentState{
		PID:          os.Getpid(),
		StartedAt:    stateTime(time.Now().Add(-time.Minute)),
		Version:      "9.9.9",
		Commit:       "abc1234",
		Server:       "wss://example.invalid/v1/node/stream",
		Connected:    true,
		Slots:        stateSlots{Total: 4, Busy: 1},
		UpdatedAt:    stateTime(time.Now()),
		Sessions:     []stateSession{},
		HeartbeatSec: 10,
	}
}

// TestStatusLive：live 退 0，`--json` 是 state.json 全字段 + stale:false。
func TestStatusLive(t *testing.T) {
	dataDir := t.TempDir()
	factsWriteState(t, dataDir, factsLiveState())

	var stdout, stderr bytes.Buffer
	code, err := runStatus([]string{"--data-dir", dataDir, "--json"}, &stdout, &stderr)
	if code != exitOK || err != nil {
		t.Fatalf("status live = (%d, %v)（stderr=%s）", code, err, stderr.String())
	}
	var rep statusReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("解析 JSON: %v（stdout=%q）", err, stdout.String())
	}
	if rep.Stale {
		t.Error("stale = true，want false（pid 在、updatedAt 是现在）")
	}
	if !rep.Connected {
		t.Error("connected = false，want true")
	}
	if rep.PID != os.Getpid() {
		t.Errorf("pid = %d，want %d", rep.PID, os.Getpid())
	}
	if rep.Slots.Total != 4 || rep.Slots.Busy != 1 {
		t.Errorf("slots = %+v，want total=4 busy=1", rep.Slots)
	}
	if rep.Sessions == nil {
		t.Error("sessions 是 null，必须是 []（#321 预留字段也要满足空列表纪律）")
	}
	if rep.LastError != nil {
		t.Errorf("lastError = %+v，want null", rep.LastError)
	}

	// 文本模式：先给 STATE，再给细节，并写明「本机视角」。
	stdout.Reset()
	if code, err := runStatus([]string{"--data-dir", dataDir}, &stdout, &stderr); code != exitOK || err != nil {
		t.Fatalf("status 文本模式 = (%d, %v)", code, err)
	}
	text := stdout.String()
	for _, want := range []string{"STATE", "live", "CONNECTED", "true", "SLOTS", "本机视角"} {
		if !strings.Contains(text, want) {
			t.Errorf("status 文本里没有 %q：\n%s", want, text)
		}
	}
}

// TestStatusStaleWhenPIDGone：daemon 被 kill -9 之后 pid 不在了 → stale + 退 1。
func TestStatusStaleWhenPIDGone(t *testing.T) {
	dataDir := t.TempDir()
	st := factsLiveState()
	// 一个必然不存在的 pid：Linux 的 pid_max 远小于 1<<30。
	st.PID = 1 << 30
	factsWriteState(t, dataDir, st)

	var stdout, stderr bytes.Buffer
	code, err := runStatus([]string{"--data-dir", dataDir, "--json"}, &stdout, &stderr)
	if code != exitFailure {
		t.Fatalf("退出码 = %d，want 1（stale 是“最后一次已知状态”）", code)
	}
	if err == nil {
		t.Fatal("stale 必须返回一个说明性 error（stderr 上要有人话）")
	}
	if kind, _ := kindOf(err); kind != kindStale {
		t.Errorf("kind = %q，want %q", kind, kindStale)
	}
	var rep statusReport
	if uerr := json.Unmarshal(stdout.Bytes(), &rep); uerr != nil {
		t.Fatalf("解析 JSON: %v（stdout=%q）", uerr, stdout.String())
	}
	if !rep.Stale {
		t.Error("stale = false，want true（pid 不在了）")
	}
	// stdout 仍然给出完整形状：机器读得懂「这份快照不能当真」。
	if rep.PID != 1<<30 || rep.Slots.Total != 4 {
		t.Errorf("stdout 的形状不完整：%+v", rep.agentState)
	}
}

// TestStatusStaleWhenHeartbeatOld：pid 还在但 updatedAt 太旧（超过 3 个心跳周期）
// → 同样 stale。一个卡死却没退出的进程就长这样。
func TestStatusStaleWhenHeartbeatOld(t *testing.T) {
	dataDir := t.TempDir()
	st := factsLiveState()
	st.HeartbeatSec = 10
	st.UpdatedAt = stateTime(time.Now().Add(-time.Hour))
	factsWriteState(t, dataDir, st)

	var stdout, stderr bytes.Buffer
	code, err := runStatus([]string{"--data-dir", dataDir}, &stdout, &stderr)
	if code != exitFailure || err == nil {
		t.Fatalf("status 陈旧的快照 = (%d, %v)，want (1, error)（stderr=%s）", code, err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "stale") {
		t.Errorf("文本输出应当说清 stale：\n%s", stdout.String())
	}
}

// TestStatusMissingStateFile：「从没起过 daemon」也是一条本机事实：形状完整、
// stale=true、退 1，kind=config_missing（不是 not_found 也不是崩溃）。
func TestStatusMissingStateFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := runStatus([]string{"--data-dir", t.TempDir(), "--json"}, &stdout, &stderr)
	if code != exitFailure || err == nil {
		t.Fatalf("status 空数据目录 = (%d, %v)，want (1, error)", code, err)
	}
	if kind, _ := kindOf(err); kind != kindConfigMissing {
		t.Errorf("kind = %q，want %q", kind, kindConfigMissing)
	}
	var rep statusReport
	if uerr := json.Unmarshal(stdout.Bytes(), &rep); uerr != nil {
		t.Fatalf("解析 JSON: %v（stdout=%q）", uerr, stdout.String())
	}
	if !rep.Stale || rep.PID != 0 {
		t.Errorf("report = %+v，want stale=true pid=0", rep.agentState)
	}
	if rep.Sessions == nil {
		t.Error("sessions 是 null，必须是 []")
	}
}

// TestStatusHelpSaysLocalView：`-h` 必须说清「哪个 status」（本机视角 vs 中枢）
// 与 stale 判定（issue §2 要求这句话进 help 与 skill）。
func TestStatusHelpSaysLocalView(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code, err := runStatus([]string{"-h"}, &stdout, &stderr); code != exitOK || err != nil {
		t.Fatalf("status -h = (%d, %v)", code, err)
	}
	help := stderr.String()
	for _, want := range []string{"本机视角", "中枢", "stale", "3 个心跳周期"} {
		if !strings.Contains(help, want) {
			t.Errorf("status -h 没有说明 %q：\n%s", want, help)
		}
	}
}

// ---------- state.json 写者（daemon 侧） ----------

// TestStateWriterSnapshots 钉住写者落盘的每个字段与两态之间的迁移。
func TestStateWriterSnapshots(t *testing.T) {
	dataDir := t.TempDir()
	cfg := &nodeConfig{Name: "n1", Server: "wss://example.invalid/v1/node/stream", Instances: 3}
	w := newStateWriter(dataDir, cfg, runPersistLog())

	w.start()
	st, err := loadAgentState(dataDir)
	if err != nil {
		t.Fatalf("start 之后读 state.json: %v", err)
	}
	if st.PID != os.Getpid() || st.Slots.Total != 3 || st.Connected {
		t.Errorf("start 后的快照 = %+v，want pid=%d total=3 connected=false", st, os.Getpid())
	}
	if st.Server != cfg.Server {
		t.Errorf("server = %q，want %q", st.Server, cfg.Server)
	}
	if st.Sessions == nil {
		t.Error("sessions 是 null，必须是 []")
	}

	w.setBusy(2)
	w.setConnected(true, 5)
	st, err = loadAgentState(dataDir)
	if err != nil {
		t.Fatalf("读 state.json: %v", err)
	}
	if !st.Connected || st.HeartbeatSec != 5 || st.Slots.Busy != 2 {
		t.Errorf("快照 = %+v，want connected=true heartbeatSec=5 busy=2", st)
	}
	if stateIsStale(st, time.Now()) {
		t.Error("刚写完的快照被判定成 stale（pid 是本进程、updatedAt 是现在）")
	}

	// fail 是「节点级错误」：lastError 记下来，同时 connected 置 false（不能自相矛盾）。
	w.fail(kindCommFailed, "dial failed\nsecond line")
	st, err = loadAgentState(dataDir)
	if err != nil {
		t.Fatalf("读 state.json: %v", err)
	}
	if st.Connected {
		t.Error("fail 之后 connected 必须是 false")
	}
	if st.LastError == nil || st.LastError.Kind != kindCommFailed {
		t.Fatalf("lastError = %+v，want kind=%s", st.LastError, kindCommFailed)
	}
	if strings.Contains(st.LastError.Message, "\n") {
		t.Errorf("lastError.message 必须是单行：%q", st.LastError.Message)
	}

	w.stop()
	st, err = loadAgentState(dataDir)
	if err != nil {
		t.Fatalf("读 state.json: %v", err)
	}
	if st.Connected {
		t.Error("stop 之后 connected 必须为 false")
	}
	if st.LastError == nil || st.LastError.Kind != kindCommFailed {
		t.Error("stop 不该擦掉 lastError（正常退出不是错误）")
	}
}

// TestStateWriterFromDaemon：真的 serveNode 走一遍握手，state.json 里必须是
// connected=true、slots.total = 节点配置、pid = 本进程（验收第三条的前半句）。
func TestStateWriterFromDaemon(t *testing.T) {
	dataDir := t.TempDir()
	h := w4StartRawAt(t, w4Config(), dataDir, 100*time.Millisecond)
	h.welcome(1) // 心跳 1s 一次 → 很快能看到 updatedAt 前进

	// 握手是异步的：等 connected 真正落盘（测试不猜调度顺序）。
	var st *agentState
	var err error
	deadline := time.Now().Add(3 * time.Second)
	for {
		st, err = loadAgentState(dataDir)
		if err == nil && st.Connected && st.HeartbeatSec == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("握手成功后 state.json 没有变成 connected=true/heartbeatSec=1（st=%+v err=%v）", st, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st.Slots.Total != w4Config().slots() {
		t.Errorf("slots.total = %d，want %d", st.Slots.Total, w4Config().slots())
	}
	if st.PID != os.Getpid() {
		t.Errorf("pid = %d，want %d（本进程）", st.PID, os.Getpid())
	}
	firstUpdated := st.UpdatedAt
	if firstUpdated == "" {
		t.Fatal("updatedAt 是空的：stale 判定无从下手")
	}

	// 验收原话：daemon 起来后 `status --json` 里 connected: true。
	var statusOut, statusErr bytes.Buffer
	if code, err := runStatus([]string{"--data-dir", dataDir, "--json"}, &statusOut, &statusErr); code != exitOK || err != nil {
		t.Fatalf("daemon 在跑时 status = (%d, %v)（stderr=%s）", code, err, statusErr.String())
	}
	var rep statusReport
	if uerr := json.Unmarshal(statusOut.Bytes(), &rep); uerr != nil {
		t.Fatalf("解析 status JSON: %v", uerr)
	}
	if !rep.Connected || rep.Stale {
		t.Errorf("status --json = connected:%t stale:%t，want connected:true stale:false", rep.Connected, rep.Stale)
	}

	// 等一拍心跳：updatedAt 必须前进（心跳就是“它还活着”的证据）。
	deadline = time.Now().Add(3 * time.Second)
	for {
		st, err = loadAgentState(dataDir)
		if err != nil {
			t.Fatalf("读 state.json: %v", err)
		}
		if st.UpdatedAt != firstUpdated {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("心跳没有刷新 updatedAt（仍是 %s）", firstUpdated)
		}
		time.Sleep(50 * time.Millisecond)
	}

	h.stop() // 正常退出：connected 必须落回 false
	st, err = loadAgentState(dataDir)
	if err != nil {
		t.Fatalf("退出后读 state.json: %v", err)
	}
	if st.Connected {
		t.Error("进程退出后 connected 必须是 false")
	}
}
