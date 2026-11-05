package main

// 本文件钉住 `anotify-agent upgrade` 的契约 §4 行为：退出码表、原子替换、
// 校验失败不落盘、需要 root 时不静默降级。
//
// 全部用「真 HTTP 服务 + 真文件系统」：这条子命令的价值就在「把远端字节变成
// 本地一个能 exec 的文件」这一件事上，用 fake HTTP 客户端测等于跳过唯一值得测的
// 那段（rename 是否真的原子、失败的路径有没有动过目标、临时文件有没有漏）。
//
// 断言口径是**外部可见的东西**：退出码、stdout/stderr 里人能看到的话、目标文件的
// 内容与权限位、目录里有没有残留——而不是内部函数调用过几次。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	// testNodeToken 是假中枢认的节点令牌（契约 §2：自升级用 node token 鉴权）。
	testNodeToken = "nt_upgrade_test_token"
	// 旧/新二进制内容：只要字节不同就能验出「换没换」。
	oldBinaryBody = "old-agent-binary\n"
	newBinaryBody = "new-agent-binary\n"
	// testRemoteVersion 是服务端声明的版本（会原样出现在输出里）。
	testRemoteVersion = "1.5.0+3fd5a59"
)

// sha256Hex 算十六进制摘要（和子命令内部用的口径一致）。
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// writeUpgradeNodeYAML 写一份最小的 node.yaml（-f 指过来，绝不依赖工作目录里的
// ./node.yaml —— 开发机上的那个文件长什么样与测试无关）。
func writeUpgradeNodeYAML(t *testing.T, server, token string) string {
	t.Helper()
	// 复用 config_test 里那个「写任意 body」的 helper，形状只维护一份。
	return writeNodeYAML(t, fmt.Sprintf("name: test-node\nserver: %s\ntoken: %s\n", server, token))
}

// binaryServer 起一个假的 GET /v1/node/binary（契约 §2）：
// 200 + X-Agent-Version + X-Agent-Sha256 + 二进制原文，并核对 node token。
//
// declaredSHA 传空 = 用 body 的真实摘要（正常路径）；传别的值 = 模拟
// 「服务端核到的字节和它自己声明的摘要不符」（被改动 / 传输坏了）。
func binaryServer(t *testing.T, body []byte, version, declaredSHA string) *httptest.Server {
	t.Helper()
	if declaredSHA == "" {
		declaredSHA = sha256Hex(body)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testNodeToken {
			t.Errorf("Authorization = %q, want %q（契约 §2：node token 鉴权，不是会话令牌）",
				got, "Bearer "+testNodeToken)
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if r.URL.Path != binaryEndpointPath {
			t.Errorf("请求路径 = %q, want %q", r.URL.Path, binaryEndpointPath)
			http.NotFound(w, r)
			return
		}
		w.Header().Set(headerAgentVersion, version)
		w.Header().Set(headerAgentSHA256, declaredSHA)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(body); err != nil {
			// 客户端在 --check 分支会提前关掉 body：这不是测试失败。
			t.Logf("写响应失败（客户端可能提前关闭）: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runUpgradeCLI 跑一次 `anotify-agent upgrade …`（就是 mainWith 那条分支）。
//
// getenv 恒返回空串：配置只从 -f 指的文件来，测试不受开发机上 ANOTIFY_* 影响。
func runUpgradeCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code = upgradeMain(args, &out, &errBuf, func(string) string { return "" })
	return code, out.String(), errBuf.String()
}

// writeTarget 造一个「已经装好的旧二进制」。
func writeTarget(t *testing.T, dir, body string, perm os.FileMode) string {
	t.Helper()
	target := filepath.Join(dir, defaultAgentBinaryName)
	if err := os.WriteFile(target, []byte(body), perm); err != nil {
		t.Fatalf("写目标文件: %v", err)
	}
	return target
}

// assertNoTempFiles 断言目录里没留下下载临时文件（失败路径要清干净）。
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录 %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), upgradeTmpPrefix) {
			t.Errorf("目录 %s 里残留临时文件 %q（契约 §4.6：校验失败/中断必须删掉，且不许动目标）",
				dir, e.Name())
		}
	}
}

// assertTarget 断言目标文件的内容与权限位（失败时一并打印 stdout/stderr）。
func assertTarget(t *testing.T, target, wantBody string, wantPerm os.FileMode, stdout, stderr string) {
	t.Helper()
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读目标 %s: %v（stdout=%q stderr=%q）", target, err, stdout, stderr)
	}
	if string(got) != wantBody {
		t.Fatalf("目标内容 = %q, want %q（stdout=%q stderr=%q）", got, wantBody, stdout, stderr)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat %s: %v", target, err)
	}
	if perm := info.Mode().Perm(); perm != wantPerm {
		t.Fatalf("目标权限 = %04o, want %04o", perm, wantPerm)
	}
}

// TestUpgradeReplacesBinary：正常路径——下载、校验、chmod 0755、rename 原子替换。
func TestUpgradeReplacesBinary(t *testing.T) {
	srv := binaryServer(t, []byte(newBinaryBody), testRemoteVersion, "")
	cfgPath := writeUpgradeNodeYAML(t, srv.URL, testNodeToken)

	dir := t.TempDir()
	// 旧二进制是 0700：换完必须变成 0755（否则可能变成不可执行 → 服务起不来）。
	target := writeTarget(t, dir, oldBinaryBody, 0o700)

	code, stdout, stderr := runUpgradeCLI(t, "-f", cfgPath, "--install-to", target)
	if code != exitOK {
		t.Fatalf("退出码 = %d, want 0（stdout=%q stderr=%q）", code, stdout, stderr)
	}
	assertTarget(t, target, newBinaryBody, 0o755, stdout, stderr)

	// 输出要说清「换了什么 → 什么」，并交代重启结论（契约 §4.8）。
	// 测试环境里没有运行中的 agent，所以结论必须是"没有检测到"——这比笼统的
	// "重启后生效"更诚实：它同时告诉用户"这次不需要你做任何事"。
	for _, want := range []string{target, testRemoteVersion, "没有检测到运行中的 agent"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout 里没有 %q：%q", want, stdout)
		}
	}
	assertNoTempFiles(t, dir)
}

// TestUpgradeCheckModes：`--check` 只比较、不下载、不动文件；
// 已最新退 **1**（别的退出码都是「能继续做点什么」），有更新退 0（契约 §4.3）。
func TestUpgradeCheckModes(t *testing.T) {
	t.Run("有更新", func(t *testing.T) {
		srv := binaryServer(t, []byte(newBinaryBody), testRemoteVersion, "")
		cfgPath := writeUpgradeNodeYAML(t, srv.URL, testNodeToken)
		dir := t.TempDir()
		target := writeTarget(t, dir, oldBinaryBody, 0o755)

		code, stdout, stderr := runUpgradeCLI(t, "-f", cfgPath, "--install-to", target, "--check")
		if code != exitOK {
			t.Fatalf("退出码 = %d, want 0（有更新；stdout=%q stderr=%q）", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "可升级") {
			t.Errorf("stdout 里没有「可升级」：%q", stdout)
		}
		// 「当前 X → 目标 Y（sha 前 12 位）」（契约 §4.3）。
		if !strings.Contains(stdout, testRemoteVersion) {
			t.Errorf("stdout 里没有目标版本 %q：%q", testRemoteVersion, stdout)
		}
		if want := shortSHA(sha256Hex([]byte(newBinaryBody))); !strings.Contains(stdout, want) {
			t.Errorf("stdout 里没有目标 sha 前 12 位 %q：%q", want, stdout)
		}
		// --check 一个字节都不许落地。
		assertTarget(t, target, oldBinaryBody, 0o755, stdout, stderr)
		assertNoTempFiles(t, dir)
	})

	t.Run("已是最新", func(t *testing.T) {
		srv := binaryServer(t, []byte(newBinaryBody), testRemoteVersion, "")
		cfgPath := writeUpgradeNodeYAML(t, srv.URL, testNodeToken)
		dir := t.TempDir()
		target := writeTarget(t, dir, newBinaryBody, 0o600)

		code, stdout, stderr := runUpgradeCLI(t, "-f", cfgPath, "--install-to", target, "--check")
		if code != exitUpgradeUpToDate {
			t.Fatalf("退出码 = %d, want %d（--check 且已最新；stdout=%q stderr=%q）",
				code, exitUpgradeUpToDate, stdout, stderr)
		}
		if !strings.Contains(stdout, "已是最新") {
			t.Errorf("stdout 里没有「已是最新」：%q", stdout)
		}
		// 权限还是 0600 = 文件没被重写。
		assertTarget(t, target, newBinaryBody, 0o600, stdout, stderr)
		assertNoTempFiles(t, dir)
	})
}

// TestUpgradeSkipsWhenUpToDate：不带 --check 时「已最新」退 **0**（契约 §4.4）。
//
// 与 --check 的区别就是这条退出码：cron 用它判断「要不要接着往下做」，
// 普通调用则把「已最新」当成功。
func TestUpgradeSkipsWhenUpToDate(t *testing.T) {
	srv := binaryServer(t, []byte(newBinaryBody), testRemoteVersion, "")
	cfgPath := writeUpgradeNodeYAML(t, srv.URL, testNodeToken)
	dir := t.TempDir()
	target := writeTarget(t, dir, newBinaryBody, 0o600)

	code, stdout, stderr := runUpgradeCLI(t, "-f", cfgPath, "--install-to", target)
	if code != exitOK {
		t.Fatalf("退出码 = %d, want 0（已最新不是错误；stdout=%q stderr=%q）", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "already up to date") {
		t.Errorf("stdout 里没有 already up to date：%q", stdout)
	}
	// 0600 还在 = 没有覆盖写（覆盖会 chmod 到 0755）。
	assertTarget(t, target, newBinaryBody, 0o600, stdout, stderr)
	assertNoTempFiles(t, dir)
}

// TestUpgradeForceOverwrites：同 sha + --force 仍然覆盖（契约 §4.4）。
func TestUpgradeForceOverwrites(t *testing.T) {
	srv := binaryServer(t, []byte(newBinaryBody), testRemoteVersion, "")
	cfgPath := writeUpgradeNodeYAML(t, srv.URL, testNodeToken)
	dir := t.TempDir()
	target := writeTarget(t, dir, newBinaryBody, 0o600)

	code, stdout, stderr := runUpgradeCLI(t, "-f", cfgPath, "--install-to", target, "--force")
	if code != exitOK {
		t.Fatalf("退出码 = %d, want 0（stdout=%q stderr=%q）", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "已更新") {
		t.Errorf("stdout 里没有「已更新」（--force 必须真的覆盖）：%q", stdout)
	}
	// 0600 → 0755 就是「文件确实被换过」的证据。
	assertTarget(t, target, newBinaryBody, 0o755, stdout, stderr)
}

// TestUpgradeBadSHADoesNotReplace：sha256 不符 → 退 4、目标**一个字节都不动**、
// 临时文件删掉（契约 §4.6）。
func TestUpgradeBadSHADoesNotReplace(t *testing.T) {
	tamperedSHA := sha256Hex([]byte("something-else-entirely"))
	srv := binaryServer(t, []byte(newBinaryBody), testRemoteVersion, tamperedSHA)
	cfgPath := writeUpgradeNodeYAML(t, srv.URL, testNodeToken)
	dir := t.TempDir()
	target := writeTarget(t, dir, oldBinaryBody, 0o755)

	code, stdout, stderr := runUpgradeCLI(t, "-f", cfgPath, "--install-to", target)
	if code != exitUpgradeBadSHA {
		t.Fatalf("退出码 = %d, want %d（校验失败）（stdout=%q stderr=%q）",
			code, exitUpgradeBadSHA, stdout, stderr)
	}
	if !strings.Contains(stderr, "校验失败") {
		t.Errorf("stderr 没说清是校验失败：%q", stderr)
	}
	// 旧内容必须还在：把可疑字节装上去再"看看好不好用"是最坏的一种失败。
	assertTarget(t, target, oldBinaryBody, 0o755, stdout, stderr)
	assertNoTempFiles(t, dir)
}

// TestUpgradeDownloadInterruptedKeepsTarget：下载中途断掉 → 退 5、目标不动、
// 临时文件清干净。与 sha 不符分开测：两者的清理路径相同，但错误分类不同（5 vs 4）。
func TestUpgradeDownloadInterruptedKeepsTarget(t *testing.T) {
	full := []byte(newBinaryBody)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerAgentVersion, testRemoteVersion)
		w.Header().Set(headerAgentSHA256, sha256Hex(full))
		w.Header().Set("Content-Type", "application/octet-stream")
		// Content-Length 说全量，实际只写一半就掐断：客户端必然拿到 unexpected EOF。
		w.Header().Set("Content-Length", strconv.Itoa(len(full)))
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(full[:4]); err != nil {
			t.Logf("写半截失败: %v", err)
			return
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// ErrAbortHandler 会真的断掉连接（不打栈），模拟中途掉线。
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(srv.Close)

	cfgPath := writeUpgradeNodeYAML(t, srv.URL, testNodeToken)
	dir := t.TempDir()
	target := writeTarget(t, dir, oldBinaryBody, 0o755)

	code, stdout, stderr := runUpgradeCLI(t, "-f", cfgPath, "--install-to", target)
	if code != exitUpgradeFailure {
		t.Fatalf("退出码 = %d, want %d（网络/其他错误）（stdout=%q stderr=%q）",
			code, exitUpgradeFailure, stdout, stderr)
	}
	if !strings.Contains(stderr, "下载中断") {
		t.Errorf("stderr 没说是下载中断：%q", stderr)
	}
	assertTarget(t, target, oldBinaryBody, 0o755, stdout, stderr)
	assertNoTempFiles(t, dir)
}

// TestUpgradeServerNotEnabled：501 = 服务端没带 agent 二进制 → 退 2（契约 §2/§4）。
//
// 与网络错误分开的用处：2 说「这台中枢压根没有升级源」，重试一百次也不会好。
func TestUpgradeServerNotEnabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		fmt.Fprint(w, `{"error":"agent_binary_unavailable"}`)
	}))
	t.Cleanup(srv.Close)

	cfgPath := writeUpgradeNodeYAML(t, srv.URL, testNodeToken)
	dir := t.TempDir()
	target := writeTarget(t, dir, oldBinaryBody, 0o755)

	code, stdout, stderr := runUpgradeCLI(t, "-f", cfgPath, "--install-to", target)
	if code != exitUpgradeUnavailable {
		t.Fatalf("退出码 = %d, want %d（服务端未启用）（stdout=%q stderr=%q）",
			code, exitUpgradeUnavailable, stdout, stderr)
	}
	for _, want := range []string{"501", "agent_binary_unavailable"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr 里没有 %q（要让人一眼看出是「中枢没带二进制」）：%q", want, stderr)
		}
	}
	assertTarget(t, target, oldBinaryBody, 0o755, stdout, stderr)
	assertNoTempFiles(t, dir)
}

// TestUpgradeNeedRoot：目标目录不可写 → 退 3、**不静默降级**、
// 提示里给一条能直接粘的 `sudo install -m 0755 <path> <target>`（契约 §4.7）。
//
// 顺带验证那条命令真的能用：提示里的源文件必须存在、内容就是服务端那份、
// 权限已经是 0755——否则「可直接粘」是假承诺。
func TestUpgradeNeedRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视目录权限位，这条用例只能以普通用户跑")
	}

	srv := binaryServer(t, []byte(newBinaryBody), testRemoteVersion, "")
	cfgPath := writeUpgradeNodeYAML(t, srv.URL, testNodeToken)

	dir := t.TempDir()
	target := writeTarget(t, dir, oldBinaryBody, 0o755)
	// 0555 = 能读能进、不能新建（CreateTemp 必然 EACCES）。
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod 目录: %v", err)
	}
	// 让 t.TempDir 的清理能删掉目录。
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	code, stdout, stderr := runUpgradeCLI(t, "-f", cfgPath, "--install-to", target)
	if code != exitUpgradeNeedRoot {
		t.Fatalf("退出码 = %d, want %d（需要 root）（stdout=%q stderr=%q）",
			code, exitUpgradeNeedRoot, stdout, stderr)
	}
	if !strings.Contains(stderr, "sudo install") {
		t.Fatalf("stderr 里没有可直接粘的 sudo install 命令：%q", stderr)
	}
	// 不给 root 就不许动目标（也不许把文件悄悄装到 $HOME 去）。
	assertTarget(t, target, oldBinaryBody, 0o755, stdout, stderr)

	src, dst := parseInstallHint(t, stderr)
	if dst != target {
		t.Errorf("提示里的目标 = %q, want %q", dst, target)
	}
	t.Cleanup(func() { _ = os.Remove(src) }) // 别在 /tmp 里留一份二进制
	got, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("提示里的源文件 %s 读不了（用户粘那行会失败）: %v", src, err)
	}
	if string(got) != newBinaryBody {
		t.Errorf("提示里的源文件内容 = %q, want %q（应当是服务端那份）", got, newBinaryBody)
	}
	if info, err := os.Stat(src); err != nil {
		t.Fatalf("stat %s: %v", src, err)
	} else if perm := info.Mode().Perm(); perm != 0o755 {
		t.Errorf("提示里的源文件权限 = %04o, want 0755（否则 install 后不可执行）", perm)
	}
}

// parseInstallHint 从 stderr 里抽出 needRootHint 打出来的那行命令的 (src, dst)。
//
// 解析而不是只断言「包含 sudo install」：那条命令要真能跑，路径必须是实际存在的文件。
func parseInstallHint(t *testing.T, stderr string) (src, dst string) {
	t.Helper()
	for _, line := range strings.Split(stderr, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 6 && fields[0] == "sudo" && fields[1] == "install" &&
			fields[2] == "-m" && fields[3] == "0755" {
			return strings.Trim(fields[4], "'"), strings.Trim(fields[5], "'")
		}
	}
	t.Fatalf("stderr 里没有 `sudo install -m 0755 <src> <dst>` 形状的命令：%q", stderr)
	return "", ""
}

// TestUpgradeRestartFailureStillZero：`--restart` 失败只告警，退出码仍是 0
// （契约 §4.8：二进制已经换好了，不该让脚本以为「没升级」）。
func TestUpgradeRestartFailureStillZero(t *testing.T) {
	srv := binaryServer(t, []byte(newBinaryBody), testRemoteVersion, "")
	cfgPath := writeUpgradeNodeYAML(t, srv.URL, testNodeToken)
	dir := t.TempDir()
	target := writeTarget(t, dir, oldBinaryBody, 0o755)

	// PATH 置空 → exec.Command("systemctl") 找不到可执行文件：
	// 确定性的失败，且绝不碰开发机上的真 systemd。
	t.Setenv("PATH", "")

	code, stdout, stderr := runUpgradeCLI(t, "-f", cfgPath, "--install-to", target, "--restart")
	if code != exitOK {
		t.Fatalf("退出码 = %d, want 0（重启失败只告警）（stdout=%q stderr=%q）", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "重启服务失败") {
		t.Errorf("stderr 里没有重启失败的告警：%q", stderr)
	}
	// 二进制仍然是换好的那一份。
	assertTarget(t, target, newBinaryBody, 0o755, stdout, stderr)
}

// TestUpgradeServerOverride：`--server` 覆盖 node.yaml 里的地址（换了中枢时用）。
func TestUpgradeServerOverride(t *testing.T) {
	srv := binaryServer(t, []byte(newBinaryBody), testRemoteVersion, "")
	// node.yaml 里指向一个必然连不上的地址（127.0.0.1:1）。
	cfgPath := writeUpgradeNodeYAML(t, "http://127.0.0.1:1", testNodeToken)
	dir := t.TempDir()
	target := writeTarget(t, dir, oldBinaryBody, 0o755)

	if code, stdout, stderr := runUpgradeCLI(t, "-f", cfgPath, "--install-to", target); code != exitUpgradeFailure {
		t.Fatalf("不带 --server 时退出码 = %d, want %d（连不上）（stdout=%q stderr=%q）",
			code, exitUpgradeFailure, stdout, stderr)
	}
	code, stdout, stderr := runUpgradeCLI(t, "-f", cfgPath, "--install-to", target, "--server", srv.URL)
	if code != exitOK {
		t.Fatalf("带 --server 时退出码 = %d, want 0（stdout=%q stderr=%q）", code, stdout, stderr)
	}
	assertTarget(t, target, newBinaryBody, 0o755, stdout, stderr)
}

// TestUpgradeHelpExitsZero：-h 走 flag.ErrHelp → 退出码 0（与主命令同一约定）。
func TestUpgradeHelpExitsZero(t *testing.T) {
	code, _, stderr := runUpgradeCLI(t, "-h")
	if code != exitOK {
		t.Fatalf("退出码 = %d, want 0（-h 是用户主动要帮助）", code)
	}
	if !strings.Contains(stderr, "-install-to") {
		t.Errorf("usage 里没有 -install-to：%q", stderr)
	}
}

// TestMainWithUpgradeDispatch：`upgrade` 子命令必须由 mainWith 分派过去。
//
// 漏了分派的表现很隐蔽：参数会被当成节点模式解析，于是 `anotify-agent upgrade`
// 启动一个节点（或报「./node.yaml 不存在」），而脚本只看到退出码 1。
func TestMainWithUpgradeDispatch(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := mainWith([]string{upgradeCommand, "-h"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("mainWith([upgrade -h]) = %d, want 0（没分派到 upgrade 分支？stderr=%q）",
			code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "agent upgrade") {
		t.Errorf("usage 里没有子命令名（没走到 upgrade 的 FlagSet）：%q", stderr.String())
	}
}

// TestResolveTarget 钉住契约 §4.5 的优先级与「解 symlink」这条细节。
func TestResolveTarget(t *testing.T) {
	t.Run("--install-to 优先", func(t *testing.T) {
		want := filepath.Join(t.TempDir(), "custom-name")
		got, err := resolveTarget(want, func() (string, error) {
			t.Fatalf("给了 --install-to 就不该再去问 os.Executable")
			return "", errors.New("unreachable")
		}, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("resolveTarget: %v", err)
		}
		if got != want {
			t.Errorf("目标 = %q, want %q", got, want)
		}
	})

	t.Run("当前可执行要解 symlink", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "real-anotify-agent")
		if err := os.WriteFile(real, []byte("x"), 0o755); err != nil {
			t.Fatalf("写真实文件: %v", err)
		}
		link := filepath.Join(dir, "anotify-agent")
		if err := os.Symlink(real, link); err != nil {
			t.Fatalf("建软链: %v", err)
		}

		got, err := resolveTarget("", func() (string, error) { return link, nil }, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("resolveTarget: %v", err)
		}
		// 替换软链本身 = 什么都没换（内核 exec 的是链的另一头）。
		if got != real {
			t.Errorf("目标 = %q, want %q（要解 symlink）", got, real)
		}
	})

	t.Run("os.Executable 失败回落 ~/.anotify/bin", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		var stdout bytes.Buffer
		got, err := resolveTarget("", func() (string, error) { return "", errors.New("boom") }, &stdout)
		if err != nil {
			t.Fatalf("resolveTarget: %v", err)
		}
		want := filepath.Join(home, ".anotify", "bin", defaultAgentBinaryName)
		if got != want {
			t.Errorf("目标 = %q, want %q", got, want)
		}
		// 回落目录要建出来，否则后面的 rename 会因为父目录不存在失败。
		if info, err := os.Stat(filepath.Dir(want)); err != nil {
			t.Errorf("回落目录没建出来: %v", err)
		} else if !info.IsDir() {
			t.Errorf("%s 不是目录", filepath.Dir(want))
		}
		if !strings.Contains(stdout.String(), "注意") {
			t.Errorf("回落时应当打印提示（静默换路径最难查）：%q", stdout.String())
		}
	})

	t.Run("Linux 的 (deleted) 后缀要剥掉", func(t *testing.T) {
		got, err := resolveTarget("", func() (string, error) {
			return "/usr/local/bin/anotify-agent (deleted)", nil
		}, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("resolveTarget: %v", err)
		}
		if got != "/usr/local/bin/anotify-agent" {
			t.Errorf("目标 = %q, want /usr/local/bin/anotify-agent（别造一个带 (deleted) 的文件）", got)
		}
	})
}

// TestOpenDownloadFilePlacesTempBesideTarget：临时文件必须建在**目标同目录**
// （契约 §4.6）——跨挂载 rename 会退化成「先删后拷」，那正是半截二进制的来源。
func TestOpenDownloadFilePlacesTempBesideTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, defaultAgentBinaryName)

	f, placed, err := openDownloadFile(target)
	if err != nil {
		t.Fatalf("openDownloadFile: %v", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	defer f.Close()

	if !placed {
		t.Fatalf("placed = false, want true（目录可写时应当就地建临时文件）")
	}
	if got := filepath.Dir(f.Name()); got != dir {
		t.Errorf("临时文件目录 = %q, want %q（必须与目标同目录）", got, dir)
	}
	if !strings.HasPrefix(filepath.Base(f.Name()), upgradeTmpPrefix) {
		t.Errorf("临时候选名 %q 没有前缀 %q（排查时认不出来）", filepath.Base(f.Name()), upgradeTmpPrefix)
	}
}

// TestBinaryEndpoint 钉住 server → GET /v1/node/binary 的映射（契约 §2）。
//
// 要点是**换协议**（ws/wss 是 node.yaml 的写法，升级走普通 HTTP）而不是换路径，
// 以及「已经带了路径就别重复追加」。
func TestBinaryEndpoint(t *testing.T) {
	cases := []struct {
		name    string
		server  string
		want    string
		wantErr bool
	}{
		{name: "https 原样", server: "https://anotify.example.com", want: "https://anotify.example.com" + binaryEndpointPath},
		{name: "http 原样", server: "http://127.0.0.1:8080", want: "http://127.0.0.1:8080" + binaryEndpointPath},
		{name: "ws 换成 http", server: "ws://example.com", want: "http://example.com" + binaryEndpointPath},
		{name: "wss 换成 https", server: "wss://example.com", want: "https://example.com" + binaryEndpointPath},
		{name: "去尾斜杠", server: "https://example.com/", want: "https://example.com" + binaryEndpointPath},
		{name: "子路径保留", server: "https://example.com/anotify/", want: "https://example.com/anotify" + binaryEndpointPath},
		{name: "已带路径不重复", server: "https://example.com" + binaryEndpointPath, want: "https://example.com" + binaryEndpointPath},
		{name: "缺 scheme 报错", server: "example.com:8080", wantErr: true},
		{name: "空串报错", server: "", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := binaryEndpoint(tc.server)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("binaryEndpoint(%q) = %q, want error", tc.server, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("binaryEndpoint(%q): %v", tc.server, err)
			}
			if got != tc.want {
				t.Errorf("binaryEndpoint(%q) = %q, want %q", tc.server, got, tc.want)
			}
		})
	}
}

// TestRestartCandidates 是"升级完能不能自动重启"的判据。
//
// 每一条排除都对应一个真实误伤：
//   - 前台跑的 agent 没有 supervisor，杀它就是停用户的服务；
//   - 别的路径上的副本不是我们升级的那份，重启它毫无意义；
//   - 自己是祖先（任务脚本里调 upgrade）时杀掉 = 把当前任务腰斩。
func TestRestartCandidates(t *testing.T) {
	target := "/home/u/.anotify/bin/anotify-agent"
	procs := []procInfo{
		{pid: 100, exe: target, supervised: true},                         // 要的
		{pid: 101, exe: target, supervised: false},                        // 前台跑，不能杀
		{pid: 102, exe: "/usr/local/bin/anotify-agent", supervised: true}, // 别的副本
		{pid: 200, exe: target, supervised: true},                         // 自己的祖先
		{pid: 300, exe: target, supervised: true},                         // 自己
	}
	got := restartCandidates(procs, target, 300, map[int]bool{200: true, 250: true})
	if len(got) != 1 || got[0] != 100 {
		t.Fatalf("restartCandidates = %v, want [100]", got)
	}
}
