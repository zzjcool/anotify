package main

// 安装脚本（scripts/install-anode.sh）的集成测试 —— **普通用户可跑**：不需要 sudo / systemd /
// docker / 网络。脚本是 v4 唯一的安装入口（契约 D1），它做的事（下载 → 校验 → 预检 → 落盘 →
// 服务生命周期 → 卸载/升级）任何一层单测都替代不了：只有把**假命令 + 真 anode 二进制**拼起来
// 跑一遍，才能看见「它到底按什么顺序调用了什么」。
//
// 夹具（每个用例一份，全在 t.TempDir() 里；绝不碰真实 /etc、绝不真 sudo/systemctl）：
//   - 假 curl/systemctl/sudo/loginctl/chown/getent 放在一个假 bin 目录，用 PATH 注入：脚本用
//     `command -v` 探测命令，所以它看到的必须是假货。每个假命令把**参数原样**追加到同一个
//     调用日志（一行一次、含命令名），断言顺序就靠它。
//   - 假 rm 是**安全网**：只允许删夹具根目录内的路径，越界就拒绝并退出 1。卸载用例因此
//     不可能删掉这台机器上真正装着的 /usr/local/bin/anode 或 /etc/anode/anode.yaml。
//   - 真 anode 二进制：用 TestMain 构出的 builtOld（`--version` = old-v1/old-commit）当
//     "下载得到的二进制"；假 curl 把它复制到脚本要求的 --output 位置，checksum 文件里写它的
//     真 sha256（crypto/sha256）。脚本会核对 --version 前缀，所以传 --version old-v1。
//   - 预检要真 WS 中枢：httptest + protocol.NodeStreamPath，读一帧 hello 后回 welcome；
//     令牌不对就 close 1008（`anode serve --check` 把它翻译成 unauthorized）。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/protocol"
)

// put 写一个夹具文件（同包其它测试在用，签名别动）。
func put(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// 安装脚本集成夹具的事实（值本身不重要，重要的是它们都是"非法但可识别"的假值）。
const (
	fixtureName  = "node-a"
	fixtureToken = "fixtureTokenAb12cd34"
	fixtureWrong = "fixtureWrongTokenZz99"
	// fixtureOwner 只在 root 直跑时用：脚本从 SUDO_USER 还原"安装者"，unit 的 User= 必须是
	// 合法用户名（install 的 unitUserPattern `^[a-z_][a-z0-9_-]{0,31}$`）。
	fixtureOwner = "anodetest"
)

// ---- 假命令：一行一次追加到 $FIXTURE_LOG，含命令名，供断言顺序 ----
//
// 每个脚本都不 set -e：日志写完才做真正的判断，免得"没记上"被当成"没调用"。

const fakeCurl = `#!/bin/sh
printf 'curl %s\n' "$*" >> "$FIXTURE_LOG"
out=""
url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) out=$2; shift 2 ;;
    --connect-timeout|--max-time|--header|--dump-header|--write-out) shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
[ -n "$out" ] || { printf 'fake-curl: 没有 --output\n' >&2; exit 90; }
case "$url" in
  *sha256) cp "$FIXTURE_SUM" "$out" ;;
  *) cp "$FIXTURE_PAYLOAD" "$out" ;;
esac
`

const fakeSystemctl = `#!/bin/sh
printf 'systemctl %s\n' "$*" >> "$FIXTURE_LOG"
case "$*" in
  *is-active*)
    n=0
    [ -f "$FIXTURE_COUNTER" ] && n=$(cat "$FIXTURE_COUNTER")
    n=$((n + 1))
    printf '%s' "$n" > "$FIXTURE_COUNTER"
    [ "$n" -gt "${FIXTURE_ISACTIVE_FAIL:-0}" ] && exit 0
    exit 3
    ;;
esac
exit 0
`

// 假 sudo 是**透传**的：先记日志，再 exec 剩下的命令（-n/-v 这类开关吃掉；只剩开关就是
// `sudo -n true` 这种探测，直接成功）。这样非 root 跑集成测试时，`sudo -n <anode> install …`
// 仍然会真的落下文件，断言与 root 下一致。
const fakeSudo = `#!/bin/sh
printf 'sudo %s\n' "$*" >> "$FIXTURE_LOG"
while [ "$#" -gt 0 ]; do
  case "$1" in
    -*) shift ;;
    *) break ;;
  esac
done
[ "$#" -eq 0 ] && exit 0
exec "$@"
`

const fakeLoginctl = `#!/bin/sh
printf 'loginctl %s\n' "$*" >> "$FIXTURE_LOG"
exit 0
`

const fakeChown = `#!/bin/sh
printf 'chown %s\n' "$*" >> "$FIXTURE_LOG"
exit 0
`

// 假 getent 只回答 passwd（脚本用它查 unit 的 Environment=HOME）；其余退出 2（像真的一样"查不到"）。
const fakeGetent = `#!/bin/sh
printf 'getent %s\n' "$*" >> "$FIXTURE_LOG"
if [ "$1" = "passwd" ] && [ -n "${2:-}" ]; then
  printf '%s:x:1000:1000::/home/%s:/bin/sh\n' "$2" "$2"
  exit 0
fi
exit 2
`

// 假 rm 只删夹具根目录里的东西：越界一律拒绝（卸载用例会遍历 /usr/local/bin/anode、
// /etc/anode/anode.yaml、/var/lib/anode 这些真实落点，不能让集成测试真的动它们）。
const fakeRM = `#!/bin/sh
printf 'rm %s\n' "$*" >> "$FIXTURE_LOG"
for a in "$@"; do
  case "$a" in
    -*) continue ;;
  esac
  case "$a" in
    "$FIXTURE_ROOT"/*) ;;
    *) printf 'fake-rm: 拒绝删除夹具之外的路径：%s\n' "$a" >&2; exit 1 ;;
  esac
done
exec /bin/rm "$@"
`

// scriptFixture 是一份完整的安装脚本测试夹具（每用例一份，自带 t.TempDir()）。
type scriptFixture struct {
	t *testing.T

	root    string // 夹具根目录（假 rm 的删除边界）
	binDir  string // 假命令目录（PATH 的第一段）
	tmpDir  string // TMPDIR：脚本运行期创建的临时文件都在这里（跑完必须空）
	homeDir string // HOME：用户级落点的锚
	stage   string // --prefix 的目标（系统级落点全部落到它下面）

	logPath string // 调用日志（一行一次，含命令名）
	counter string // systemctl is-active 的调用计数
	realBin string // 真 anode 二进制（TestMain 构出的 builtOld）
	payload string // 假 curl "下载"下来的东西：一个只记日志、其余原样 exec realBin 的垫片
	sumPath string // payload 的真 sha256（sha256sum 两空格格式）

	hubWS       string // 假中枢的 ws:// 地址（--server）
	downloadURL string // --download-url（假 curl 认这两个 URL）
	checksumURL string // --checksum-url

	stream       atomic.Int64 // 假中枢收到的 WS 连接次数（预检证据）
	hubBinary    atomic.Int64 // 假中枢被要二进制的次数（--download-url 下必须是 0）
	sudoUser     string       // 有值就注入 SUDO_USER（root 直跑时才需要）
	isActiveFail int          // 假 systemctl 前 N 次 is-active 返回"未 active"
}

func newScriptFixture(t *testing.T) *scriptFixture {
	t.Helper()
	root := t.TempDir()
	f := &scriptFixture{
		t:            t,
		root:         root,
		binDir:       filepath.Join(root, "bin"),
		tmpDir:       filepath.Join(root, "run"),
		homeDir:      filepath.Join(root, "home"),
		stage:        filepath.Join(root, "stage"),
		logPath:      filepath.Join(root, "calls.log"),
		counter:      filepath.Join(root, "is-active.counter"),
		realBin:      filepath.Join(root, "payload", "anode-real"),
		payload:      filepath.Join(root, "payload", "anode-download"),
		sumPath:      filepath.Join(root, "payload", "anode.sha256"),
		downloadURL:  "fixture.test/anode",
		checksumURL:  "fixture.test/anode.sha256",
		isActiveFail: 2,
	}
	for _, dir := range []string{f.binDir, f.tmpDir, f.homeDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	// root 直跑时脚本拿不到"安装者"身份（SUDO_USER）就拒绝装系统服务 —— 这不是本测试要验证的
	// 东西，所以补一个假安装者 + 假 getent 给出的家目录；非 root 下脚本走 id -un，不需要它。
	if os.Getuid() == 0 {
		f.sudoUser = fixtureOwner
	}
	// 真二进制当"下载载荷"：脚本会跑它的 --version 核对发布版本，假二进制骗不过这一关。
	//
	// 载荷外面套一层**只记日志**的垫片（和假 curl 记日志是一回事）：`anode install` 这一步是
	// 脚本直接 exec 下载到的二进制，不走 PATH，没有垫片就看不见"它到底有没有被调用"。
	// 真正执行的永远是 builtOld（--version / install 都是它），垫片只多写一行调用日志。
	binaryBody, err := os.ReadFile(builtOld)
	if err != nil {
		t.Fatalf("读 builtOld: %v", err)
	}
	put(t, f.realBin, string(binaryBody), 0755)
	payload := fmt.Sprintf("#!/bin/sh\nprintf '%%s %%s\\n' \"$0\" \"$*\" >> \"${FIXTURE_LOG:-/dev/null}\" 2>/dev/null || true\nexec %q \"$@\"\n", f.realBin)
	put(t, f.payload, payload, 0755)
	sum := sha256.Sum256([]byte(payload))
	f.writeSum(hex.EncodeToString(sum[:]))
	for name, body := range map[string]string{
		"curl": fakeCurl, "systemctl": fakeSystemctl, "sudo": fakeSudo,
		"loginctl": fakeLoginctl, "chown": fakeChown, "getent": fakeGetent, "rm": fakeRM,
	} {
		put(t, filepath.Join(f.binDir, name), body, 0755)
	}
	srv := f.newFakeHub()
	f.hubWS = "ws" + strings.TrimPrefix(srv.URL, "http")
	return f
}

// writeSum 覆盖校验和文件（篡改用例写 64 个 0）。
func (f *scriptFixture) writeSum(digest string) {
	f.t.Helper()
	put(f.t, f.sumPath, digest+"  anode\n", 0600)
}

// newFakeHub 是"真 WS 中枢"的最小实现：鉴权头 + hello 令牌都对才回 welcome，否则 close 1008。
func (f *scriptFixture) newFakeHub() *httptest.Server {
	f.t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case protocol.NodeStreamPath:
			f.stream.Add(1)
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.CloseNow()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			frame, err := protocol.Decode(data)
			if err != nil {
				return
			}
			hello, ok := frame.(*protocol.HelloFrame)
			if !ok || hello.Type != protocol.FrameHello {
				_ = conn.Close(websocket.StatusPolicyViolation, "not hello")
				return
			}
			// 两条鉴权面都要对：握手头（Bearer）与 hello 里的令牌（节点身份）。
			authOK := r.Header.Get(protocol.EmitAuthHeader) == protocol.EmitAuthScheme+" "+fixtureToken
			if hello.Token != fixtureToken || !authOK {
				// 1008 = 中枢拒绝身份；`anode serve --check` 把它翻成 unauthorized。
				_ = conn.Close(websocket.StatusPolicyViolation, "nope")
				return
			}
			body, err := protocol.Encode(&protocol.WelcomeFrame{
				Type:           protocol.FrameWelcome,
				ConfigRevision: "fixture-r1",
				NodeVars:       map[string]string{},
				Namespaces:     []string{"default"},
			})
			if err != nil {
				return
			}
			if err := conn.Write(ctx, websocket.MessageText, body); err != nil {
				return
			}
			_ = conn.Close(websocket.StatusNormalClosure, "done")
		case protocol.NodeBinaryPath:
			f.hubBinary.Add(1)
			http.Error(w, "no node binary here", http.StatusNotImplemented)
		default:
			http.NotFound(w, r)
		}
	}))
	f.t.Cleanup(srv.Close)
	return srv
}

// env 是脚本子进程的**完整**环境：不继承 os.Environ()，免得开发机上的 ANODE_* 变量
// （或 SUDO_USER / TMPDIR）偷偷改变脚本行为。
func (f *scriptFixture) env() []string {
	env := []string{
		"PATH=" + f.binDir + ":/usr/local/bin:/usr/bin:/bin",
		"HOME=" + f.homeDir,
		"TMPDIR=" + f.tmpDir,
		"TZ=UTC",
		"FIXTURE_LOG=" + f.logPath,
		"FIXTURE_ROOT=" + f.root,
		"FIXTURE_PAYLOAD=" + f.payload,
		"FIXTURE_SUM=" + f.sumPath,
		"FIXTURE_COUNTER=" + f.counter,
		fmt.Sprintf("FIXTURE_ISACTIVE_FAIL=%d", f.isActiveFail),
	}
	if f.sudoUser != "" {
		env = append(env, "SUDO_USER="+f.sudoUser)
	}
	return env
}

// installScriptPath 是仓库里的安装脚本（测试的工作目录是包目录 cmd/anode）。
func installScriptPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "scripts", "install-anode.sh"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("找不到安装脚本 %s: %v", path, err)
	}
	return path
}

// run 跑一次脚本：退出码 + stdout + stderr（超时直接判失败，不留挂住的测试）。
func (f *scriptFixture) run(args ...string) (int, string, string) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", append([]string{installScriptPath(f.t)}, args...)...)
	cmd.Env = f.env()
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if ctx.Err() != nil {
		f.t.Fatalf("安装脚本超时：%v\nstdout=%s\nstderr=%s", args, out.String(), errOut.String())
	}
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			f.t.Fatalf("跑安装脚本失败：%v", err)
		}
		code = exit.ExitCode()
	}
	return code, out.String(), errOut.String()
}

// baseArgs 是"首次安装"的完整参数（令牌由调用方按用例补）：走 --download-url 分支，
// 因此假中枢只会被预检连，不会被要二进制。
func (f *scriptFixture) baseArgs() []string {
	return []string{
		"--name", fixtureName,
		"--server", f.hubWS,
		"--version", "old-v1",
		"--download-url", f.downloadURL,
		"--checksum-url", f.checksumURL,
		"--prefix", f.stage,
	}
}

// calls 读调用日志（一行一次，含命令名）；日志不存在 = 一个命令都没跑。
func (f *scriptFixture) calls() []string {
	f.t.Helper()
	raw, err := os.ReadFile(f.logPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		f.t.Fatalf("读调用日志: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// resetLog 清空调用日志（一个用例里跑多轮时，用来只看最后一轮）。
func (f *scriptFixture) resetLog() {
	f.t.Helper()
	if err := os.Remove(f.logPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		f.t.Fatalf("清空调用日志: %v", err)
	}
}

// assertPerm 断言文件的权限位（落盘三件套的 0755/0600/0644 是安全边界，不是样式）。
func (f *scriptFixture) assertPerm(path string, want os.FileMode) {
	f.t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		f.t.Fatalf("stat %s: %v", path, err)
	}
	if got := st.Mode().Perm(); got != want {
		f.t.Fatalf("%s 权限 = %04o，期望 %04o", path, got, want)
	}
}

// mustNotExist 断言路径不存在（"未落盘"的机械证据）。
func (f *scriptFixture) mustNotExist(path string) {
	f.t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		f.t.Fatalf("%s 不该存在（err=%v）", path, err)
	}
}

// ---- 调用日志断言helpers（不依赖"哪个用户跑的"：sudo 前缀也在同一行里） ----

func findLine(lines []string, pred func(string) bool) (int, string) {
	for i, line := range lines {
		if pred(line) {
			return i, line
		}
	}
	return -1, ""
}

func countLines(lines []string, pred func(string) bool) int {
	n := 0
	for _, line := range lines {
		if pred(line) {
			n++
		}
	}
	return n
}

// isSystemctlCall 认出 systemctl 调用，包括 `sudo -n systemctl …` 这种提权后的形状。
func isSystemctlCall(line string) bool { return hasArg(line, "systemctl") }

// isDirectSystemctl 只认真正执行 systemctl 的那一行（不是 `sudo -n systemctl …` 的提权包装）。
//
// 假 sudo 是透传的：非 root 下同一次调用在日志里会出现两行（`sudo -n systemctl enable …`
// 后面跟着被它 exec 的 `systemctl enable …`）。所以“调用了几次”要数直接执行的那一行；
// 而在 root 下根本没有包装行，同一个断言两边都成立。
func isDirectSystemctl(line string) bool {
	return isSystemctlCall(line) && !strings.HasPrefix(line, "sudo ")
}

// isAnodeInstallCall 认出"下载到的 anode 执行 install"（DL_BIN 的 basename 是 anode）。
func isAnodeInstallCall(line string) bool {
	return hasArg(line, "install") && strings.Contains(line, "/anode")
}

// hasArg 判断这一行里有没有**恰好等于** arg 的参数（避免 "…/anode" 被 "…/anode.sha256" 命中）。
func hasArg(line, arg string) bool {
	for _, field := range strings.Fields(line) {
		if field == arg {
			return true
		}
	}
	return false
}

// lineArg 取 `--flag <value>` 里的 value。
func lineArg(line, flag string) string {
	fields := strings.Fields(line)
	for i, field := range fields {
		if field == flag && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// ---- 1. 正常安装 ----

func TestInstallScriptHappyPath(t *testing.T) {
	f := newScriptFixture(t)
	code, out, errOut := f.run(append(f.baseArgs(), "--token", fixtureToken)...)
	if code != 0 {
		t.Fatalf("正常安装应该退 0，实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}

	bin := filepath.Join(f.stage, "usr/local/bin/anode")
	cfg := filepath.Join(f.stage, "etc/anode/anode.yaml")
	unit := filepath.Join(f.stage, "etc/systemd/system/anode.service")
	f.assertPerm(bin, 0755)
	f.assertPerm(cfg, 0600)
	f.assertPerm(unit, 0644)

	// 下载 + 校验真的走通了：落盘的二进制就是 builtOld（脚本跑过它的 --version 才放行）。
	vcode, vout, verr := binaryRun(t, bin, nil, "--version")
	if vcode != 0 || !strings.Contains(vout, "old-v1") {
		t.Fatalf("装出来的二进制不是下载的那个：%d %s %s", vcode, vout, verr)
	}
	configBody, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("读配置: %v", err)
	}
	if !strings.Contains(string(configBody), "token: "+fixtureToken) {
		t.Fatalf("配置里没有节点令牌：%s", configBody)
	}

	// 预检只连一次假中枢；走 --download-url 时不该去要中枢的二进制。
	if hits := f.stream.Load(); hits != 1 {
		t.Fatalf("预检应该连中枢 1 次，实际 %d", hits)
	}
	if hits := f.hubBinary.Load(); hits != 0 {
		t.Fatalf("--download-url 不该请求 %s（实际 %d 次）", protocol.NodeBinaryPath, hits)
	}

	lines := f.calls()
	// 下载：两个 URL 都拉了（二进制 + 校验和）。
	if _, line := findLine(lines, func(l string) bool { return hasArg(l, f.downloadURL) }); line == "" {
		t.Fatalf("没有下载二进制：%v", lines)
	}
	if _, line := findLine(lines, func(l string) bool { return hasArg(l, f.checksumURL) }); line == "" {
		t.Fatalf("没有下载校验和：%v", lines)
	}
	// 下载完先跑 --version 核对发布版本（D4），再 install —— 顺序反了就等于信没验过的包。
	versionAt, versionLine := findLine(lines, func(l string) bool { return hasArg(l, "--version") })
	if versionLine == "" {
		t.Fatalf("脚本没有核对下载二进制的 --version：%v", lines)
	}

	// D9：令牌只经临时 --token-file 传给 anode install，绝不进 argv/日志；退出前删掉。
	installAt, installLine := findLine(lines, isAnodeInstallCall)
	if installLine == "" {
		t.Fatalf("没有调用 anode install：%v", lines)
	}
	tokenFile := lineArg(installLine, "--token-file")
	if tokenFile == "" {
		t.Fatalf("anode install 没用 --token-file：%s", installLine)
	}
	if !strings.HasPrefix(tokenFile, f.tmpDir+string(os.PathSeparator)) || filepath.Base(tokenFile) != "token" {
		t.Fatalf("临时令牌文件不在 TMPDIR 里：%s", tokenFile)
	}
	if strings.Contains(installLine, fixtureToken) {
		t.Fatalf("令牌进了 argv：%s", installLine)
	}
	if _, err := os.Stat(tokenFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("临时令牌文件没被删掉：%s", tokenFile)
	}
	// 整个 TMPDIR 也是空的：临时配置/令牌/下载载荷都不留。
	if entries, err := os.ReadDir(f.tmpDir); err != nil || len(entries) != 0 {
		t.Fatalf("TMPDIR 应该被清空：%v %v", entries, err)
	}

	// 服务生命周期顺序：install → daemon-reload → enable → restart。
	reload, _ := findLine(lines, func(l string) bool { return isSystemctlCall(l) && hasArg(l, "daemon-reload") })
	enable, _ := findLine(lines, func(l string) bool { return isSystemctlCall(l) && hasArg(l, "enable") })
	restart, _ := findLine(lines, func(l string) bool { return isSystemctlCall(l) && hasArg(l, "restart") })
	if reload < 0 || enable < 0 || restart < 0 || !(reload < enable && enable < restart) {
		t.Fatalf("systemctl 顺序不对（reload=%d enable=%d restart=%d）：%v", reload, enable, restart, lines)
	}
	if !(versionAt < installAt && installAt < reload) {
		t.Fatalf("顺序不对：--version(%d) → install(%d) → daemon-reload(%d)\n%v", versionAt, installAt, reload, lines)
	}
	// is-active 是**轮询**：假 systemctl 前 2 次返回非 active，第 3 次才成功。
	// 只问一次就判定的话这里会失败 —— 成功本身就证明它在重试。
	polls := countLines(lines, func(l string) bool { return isDirectSystemctl(l) && hasArg(l, "is-active") })
	if polls < 3 {
		t.Fatalf("is-active 只问 %d 次（前两次返回非 active，应该轮询到第 3 次）", polls)
	}

	// 令牌不出现在任何人类可读输出里。
	if strings.Contains(out+errOut, fixtureToken) {
		t.Fatalf("令牌泄露到输出：stdout=%s stderr=%s", out, errOut)
	}
	if strings.Contains(strings.Join(lines, "\n"), fixtureToken) {
		t.Fatalf("令牌泄露到调用日志")
	}
}

// ---- 2. 篡改 sha256：中止且未落盘 ----

func TestInstallScriptRejectsBadChecksum(t *testing.T) {
	f := newScriptFixture(t)
	f.writeSum(strings.Repeat("0", 64)) // 摘要与真二进制不符（破坏/篡改）
	code, out, errOut := f.run(append(f.baseArgs(), "--token", fixtureToken)...)
	if code == 0 {
		t.Fatalf("校验和不匹配应该失败\nstdout:\n%s", out)
	}
	if !strings.Contains(out+errOut, "sha256") {
		t.Fatalf("报错没点名 sha256：stdout=%s stderr=%s", out, errOut)
	}
	// 什么都没落：staging 目录整体不存在（连 etc/ usr/ 都没建）。
	f.mustNotExist(f.stage)
	// 下载确实发生了（不是"参数错所以没跑"），但装机与服务一步都没走。
	lines := f.calls()
	if countLines(lines, func(l string) bool { return hasArg(l, f.downloadURL) }) != 1 {
		t.Fatalf("应该已经下载过二进制：%v", lines)
	}
	if countLines(lines, func(l string) bool { return hasArg(l, "install") }) != 0 {
		t.Fatalf("校验失败却调用了 anode install：%v", lines)
	}
	if n := countLines(lines, isSystemctlCall); n != 0 {
		t.Fatalf("校验失败却调用了 systemctl %d 次：%v", n, lines)
	}
	// 也没有预检（校验在预检之前）。
	if hits := f.stream.Load(); hits != 0 {
		t.Fatalf("校验失败却连了中枢 %d 次", hits)
	}
}

// ---- 3. 缺令牌（--token / --token-file 都不给，且没有已有配置）----

func TestInstallScriptRequiresToken(t *testing.T) {
	f := newScriptFixture(t)
	code, out, errOut := f.run(f.baseArgs()...)
	if code == 0 {
		t.Fatalf("没有令牌却装成功了\nstdout:\n%s", out)
	}
	if !strings.Contains(errOut, "--token") || !strings.Contains(errOut, "--token-file") {
		t.Fatalf("报错没点名 --token/--token-file：%s", errOut)
	}
	f.mustNotExist(f.stage)
	// 身份校验在下载之前：一个命令都没跑（连 curl 都没有）。
	if lines := f.calls(); countLines(lines, func(l string) bool { return strings.HasPrefix(l, "curl ") }) != 0 {
		t.Fatalf("缺令牌不该开始下载：%v", lines)
	}
	if n := countLines(f.calls(), isSystemctlCall); n != 0 {
		t.Fatalf("缺令牌却调用了 systemctl %d 次", n)
	}
}

// ---- 4. --no-preflight：跳过预检但照样装成 ----

func TestInstallScriptNoPreflight(t *testing.T) {
	f := newScriptFixture(t)
	// 故意给一个假中枢**不认**的令牌：预检一旦真的发生，不是 count+1 就是直接失败。
	code, out, errOut := f.run(append(f.baseArgs(), "--no-preflight", "--token", fixtureWrong)...)
	if code != 0 {
		t.Fatalf("--no-preflight 应该装成功，实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if hits := f.stream.Load(); hits != 0 {
		t.Fatalf("--no-preflight 了还连了中枢 %d 次", hits)
	}
	if !strings.Contains(out, "--no-preflight") {
		t.Fatalf("输出没有说明跳过了预检：%s", out)
	}
	// 跳过预检 ≠ 跳过服务：配置落盘 + restart 照旧。
	if _, err := os.Stat(filepath.Join(f.stage, "etc/anode/anode.yaml")); err != nil {
		t.Fatalf("配置没落盘: %v", err)
	}
	if countLines(f.calls(), func(l string) bool { return isDirectSystemctl(l) && hasArg(l, "restart") }) != 1 {
		t.Fatalf("服务没被 restart：%v", f.calls())
	}
}

// ---- 5. 预检失败（错令牌 → 1008）：中止且未落盘、未装服务 ----

func TestInstallScriptPreflightRejected(t *testing.T) {
	f := newScriptFixture(t)
	code, out, errOut := f.run(append(f.baseArgs(), "--token", fixtureWrong)...)
	if code != 2 {
		t.Fatalf("预检失败的退出码应该是 2（契约），实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if hits := f.stream.Load(); hits != 1 {
		t.Fatalf("预检应该连中枢 1 次，实际 %d", hits)
	}
	if !strings.Contains(errOut, "预检失败") || !strings.Contains(errOut, "令牌") {
		t.Fatalf("预检失败没给出可定位诊断：%s", errOut)
	}
	// 预检在落盘之前：配置/unit/二进制都不许出现，anode install 与 systemctl 一次都不许调。
	f.mustNotExist(filepath.Join(f.stage, "etc/anode/anode.yaml"))
	f.mustNotExist(filepath.Join(f.stage, "etc/systemd/system/anode.service"))
	f.mustNotExist(filepath.Join(f.stage, "usr/local/bin/anode"))
	lines := f.calls()
	if countLines(lines, func(l string) bool { return hasArg(l, "install") }) != 0 {
		t.Fatalf("预检失败却调用了 anode install：%v", lines)
	}
	if n := countLines(lines, isSystemctlCall); n != 0 {
		t.Fatalf("预检失败却调用了 systemctl %d 次：%v", n, lines)
	}
	if strings.Contains(out+errOut, fixtureWrong+" ") {
		t.Fatalf("令牌泄露到输出")
	}
}

// ---- 6. --service none：只落盘，一个 systemctl 都不调 ----

func TestInstallScriptServiceNone(t *testing.T) {
	f := newScriptFixture(t)
	code, out, errOut := f.run(append(f.baseArgs(), "--service", "none", "--token", fixtureToken)...)
	if code != 0 {
		t.Fatalf("--service none 应该成功，实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if n := countLines(f.calls(), isSystemctlCall); n != 0 {
		t.Fatalf("--service none 却调用了 systemctl %d 次：%v", n, f.calls())
	}
	// 只落盘：二进制 + 配置在，服务没起。
	f.assertPerm(filepath.Join(f.stage, "usr/local/bin/anode"), 0755)
	f.assertPerm(filepath.Join(f.stage, "etc/anode/anode.yaml"), 0600)
	// 没有服务可停/可起，预检也不做（脚本只在装服务时预检）。
	if hits := f.stream.Load(); hits != 0 {
		t.Fatalf("--service none 不该连中枢预检（实际 %d 次）", hits)
	}
	// 手工命令必须打印出来，否则用户不知道下一步做什么。
	if !strings.Contains(out, "手工启动") || !strings.Contains(out, "systemctl enable --now anode.service") {
		t.Fatalf("没有打印手工启动命令：%s", out)
	}
}

// ---- 7. 重跑 = 升级：无 token/server/name 参数，配置字节不变，restart 被调用 ----

func TestInstallScriptUpgradeKeepsConfig(t *testing.T) {
	f := newScriptFixture(t)
	if code, out, errOut := f.run(append(f.baseArgs(), "--token", fixtureToken)...); code != 0 {
		t.Fatalf("首次安装失败 %d\n%s\n%s", code, out, errOut)
	}
	cfg := filepath.Join(f.stage, "etc/anode/anode.yaml")
	before, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("读配置: %v", err)
	}
	f.resetLog()

	// 重跑只要"取二进制"的参数：name/server/token 全部从已有 anode.yaml 读回。
	upgrade := []string{
		"--version", "old-v1",
		"--download-url", f.downloadURL,
		"--checksum-url", f.checksumURL,
		"--prefix", f.stage,
	}
	code, out, errOut := f.run(upgrade...)
	if code != 0 {
		t.Fatalf("升级应该成功，实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	after, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("升级后读配置: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("升级改动了 anode.yaml（令牌/身份必须原样保留）\nbefore:\n%s\nafter:\n%s", before, after)
	}
	lines := f.calls()
	_, installLine := findLine(lines, isAnodeInstallCall)
	if installLine == "" {
		t.Fatalf("升级没有调用 anode install：%v", lines)
	}
	if lineArg(installLine, "--name") != fixtureName {
		t.Fatalf("升级没有从配置读回节点名：%s", installLine)
	}
	// 先停旧实例（同一个令牌只能有一条连接），再装，再 restart + 轮询 active。
	if countLines(lines, func(l string) bool { return isDirectSystemctl(l) && hasArg(l, "stop") }) != 1 {
		t.Fatalf("升级没有先停旧服务：%v", lines)
	}
	if countLines(lines, func(l string) bool { return isDirectSystemctl(l) && hasArg(l, "restart") }) != 1 {
		t.Fatalf("升级没有 restart：%v", lines)
	}
	if hits := f.stream.Load(); hits != 2 { // 首装 1 次 + 升级 1 次
		t.Fatalf("两次安装都应该预检一次（共 2 次），实际 %d", hits)
	}
	if !strings.Contains(out, "升级完成") {
		t.Fatalf("升级结论没写清楚：%s", out)
	}
}

// ---- 8. 卸载：默认 / --keep-config / --purge ----

func TestInstallScriptUninstallModes(t *testing.T) {
	f := newScriptFixture(t)
	bin := filepath.Join(f.stage, "usr/local/bin/anode")
	cfg := filepath.Join(f.stage, "etc/anode/anode.yaml")
	unit := filepath.Join(f.stage, "etc/systemd/system/anode.service")
	data := filepath.Join(f.stage, "var/lib/anode")
	marker := filepath.Join(data, "work", "marker")

	install := func() {
		t.Helper()
		if code, out, errOut := f.run(append(f.baseArgs(), "--token", fixtureToken)...); code != 0 {
			t.Fatalf("装一次（给卸载准备现场）失败 %d\n%s\n%s", code, out, errOut)
		}
		// 数据目录是"用户数据"：卸载默认必须保住它，只有 --purge 才删。
		put(t, marker, "keep me\n", 0600)
	}
	uninstall := func(extra ...string) (string, string) {
		t.Helper()
		f.resetLog()
		code, out, errOut := f.run(append([]string{"--uninstall", "--prefix", f.stage}, extra...)...)
		if code != 0 {
			t.Fatalf("卸载 %v 应该退 0，实际 %d\nstdout:\n%s\nstderr:\n%s", extra, code, out, errOut)
		}
		return out, errOut
	}

	// (a) 默认卸载：unit/二进制/配置都删，data 保留。
	install()
	out, _ := uninstall()
	f.mustNotExist(unit)
	f.mustNotExist(bin)
	f.mustNotExist(cfg)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("默认卸载不该删数据目录（%s）: %v", marker, err)
	}
	if !strings.Contains(out, "已移除") || !strings.Contains(out, "控制台") {
		t.Fatalf("卸载结论不完整：%s", out)
	}
	// 停服务是卸载的一部分（disable --now）。
	if countLines(f.calls(), func(l string) bool { return isSystemctlCall(l) && hasArg(l, "disable") }) == 0 {
		t.Fatalf("卸载没有停用服务：%v", f.calls())
	}

	// (b) --keep-config：配置保留（令牌还在里面，用户可能要备份），二进制/unit 照删。
	install()
	out, _ = uninstall("--keep-config")
	f.mustNotExist(bin)
	f.mustNotExist(unit)
	if _, err := os.Stat(cfg); err != nil {
		t.Fatalf("--keep-config 却删了配置: %v", err)
	}
	if !strings.Contains(out, "保留配置") {
		t.Fatalf("--keep-config 没说明配置被保留：%s", out)
	}

	// (c) --purge：连数据目录一起删。
	install()
	uninstall("--purge")
	f.mustNotExist(cfg)
	f.mustNotExist(bin)
	f.mustNotExist(data)
}

// ---- 9. --dry-run：不落盘、不提权、系统命令一个都没真跑 ----

func TestInstallScriptDryRun(t *testing.T) {
	f := newScriptFixture(t)
	// 预演不该去查 passwd（root + SUDO_USER 会触发 getent）：这里要的是**完全没有**系统调用。
	f.sudoUser = ""
	code, out, errOut := f.run(append(f.baseArgs(), "--dry-run", "--token", fixtureToken)...)
	if code != 0 {
		t.Fatalf("--dry-run 应该退 0，实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if lines := f.calls(); len(lines) != 0 {
		t.Fatalf("--dry-run 不该真的执行任何命令，实际：%v", lines)
	}
	f.mustNotExist(f.stage)
	if entries, err := os.ReadDir(f.tmpDir); err != nil || len(entries) != 0 {
		t.Fatalf("--dry-run 不该往 TMPDIR 写东西：%v %v", entries, err)
	}
	// 令牌在预演里是占位符：真值一个字都不出现。
	if !strings.Contains(out, "<token>") {
		t.Fatalf("预演没有用 <token> 占位：%s", out)
	}
	if strings.Contains(out+errOut, fixtureToken) {
		t.Fatalf("预演打印了真令牌：%s", out)
	}
	if !strings.Contains(out, "dry-run") {
		t.Fatalf("预演输出没有标明是 dry-run：%s", out)
	}
}

// ---- 10. 脚本与 web/install-anode.sh 字节一致（D2：静态服务匿名下发的就是它）----

func TestInstallScriptCopyMatchesWeb(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	source, err := os.ReadFile(filepath.Join(repoRoot, "scripts", "install-anode.sh"))
	if err != nil {
		t.Fatalf("读 scripts/install-anode.sh: %v", err)
	}
	served, err := os.ReadFile(filepath.Join(repoRoot, "web", "install-anode.sh"))
	if err != nil {
		t.Fatalf("读 web/install-anode.sh: %v", err)
	}
	if len(source) < 1024 {
		t.Fatalf("scripts/install-anode.sh 只有 %d 字节，不像是完整安装器", len(source))
	}
	if !bytes.Equal(source, served) {
		t.Fatalf("web/install-anode.sh 与 scripts/install-anode.sh 不一致（%d vs %d 字节）：`curl -fsSL <origin>/install-anode.sh | sh` 下发的必须是同一份脚本", len(source), len(served))
	}
}

// ---- 既有覆盖：anode install 自己不需要宿主服务 ----
//
// 脚本的落盘步骤是 `anode install`，它必须能在"PATH 里什么都没有"的机器上跑完
// （不 sudo、不 systemctl、不联网）。这条与安装脚本互补，保留。
func TestBinaryInstallOfflineStageNoHostServices(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "stage")
	env := []string{"PATH=" + t.TempDir()} // no systemctl/sudo/curl available; installer must not execute any.
	args := []string{"install", "--prefix", prefix, "--name", "node-a", "--server", "ws://127.0.0.1:1", "--token", "offline-token"}
	for i := 0; i < 2; i++ {
		code, out, stderr := binaryRun(t, builtOld, env, args...)
		if code != 0 || strings.Contains(out+stderr, "offline-token") {
			t.Fatalf("stage %d: %d %s %s", i, code, out, stderr)
		}
	}
	installed := filepath.Join(prefix, "usr/local/bin/anode")
	code, out, stderr := binaryRun(t, installed, nil, "--version")
	if code != 0 || !strings.Contains(out, "old-commit") {
		t.Fatalf("installed binary: %d %s %s", code, out, stderr)
	}
}
