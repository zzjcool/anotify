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
hdrfile=""
cfg=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) out=$2; shift 2 ;;
    --dump-header) hdrfile=$2; shift 2 ;;
    --config) cfg=$2; shift 2 ;;
    --connect-timeout|--max-time|--write-out|--header) shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
# D9：下行令牌只能走 curl --config（不进 argv）。这里只验配置文件真带了 Bearer，
# 并把结论记成一行“curl-auth ok”——绝不把令牌本身写进调用日志。
if [ -n "$cfg" ]; then
  if grep -q "Authorization: Bearer ${FIXTURE_TOKEN:-}" "$cfg" 2>/dev/null; then
    printf 'curl-auth ok\n' >> "$FIXTURE_LOG"
  else
    printf 'curl-auth MISSING\n' >> "$FIXTURE_LOG"
  fi
fi
[ -n "$out" ] || { printf 'fake-curl: 没有 --output\n' >&2; exit 90; }
case "$url" in
  *sha256) cp "$FIXTURE_SUM" "$out" ;;
  *"${FIXTURE_BINARY_PATH:-/v1/node/binary}")
    if [ -n "$hdrfile" ]; then
      printf 'HTTP/1.1 200 OK\r\nX-Agent-Version: old-v1\r\nX-Agent-Sha256: %s\r\n\r\n' "$(cut -d' ' -f1 "$FIXTURE_SUM")" > "$hdrfile"
    fi
    cp "$FIXTURE_PAYLOAD" "$out"
    printf '200'
    ;;
  *) cp "$FIXTURE_PAYLOAD" "$out" ;;
esac
`

const fakeSystemctl = `#!/bin/sh
printf 'systemctl %s\n' "$*" >> "$FIXTURE_LOG"
case "$*" in
  *is-active*)
    # 所有权敏感模式（C1）：忠实模拟 systemd 的权限语义 —— unit 写的是 User=<安装者>，
    # 而 config 是 0600，属主不是那个用户时服务读不到配置、起不来，is-active 只能是非 active。
    # 所以这条分支只有在「chown 已经把 config 交给 unit 的 User=」之后才会绿。
    if [ "${FIXTURE_OWNERSHIP:-0}" = "1" ]; then
      want=$(sed -n 's/^User=//p' "$FIXTURE_UNIT_FILE" 2>/dev/null | head -n 1)
      got=$(cat "$FIXTURE_CONFIG_OWNER" 2>/dev/null || :)
      printf 'systemctl: ownership got=%s want=%s\n' "$got" "$want" >> "$FIXTURE_LOG"
      [ -n "$want" ] && [ "$got" = "$want" ] || exit 3
      exit 0
    fi
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

// 假 chown 记两件事：调用本身（顺序断言用），以及「谁被授权读 config」——后者写进
// $FIXTURE_CONFIG_OWNER，由假 systemctl 的 is-active 语义消费（见 FIXTURE_OWNERSHIP）。
// 不真 chown：夹具里的安装者（root 直跑时是 fixtureOwner）在这台机器上并不存在。
const fakeChown = `#!/bin/sh
printf 'chown %s\n' "$*" >> "$FIXTURE_LOG"
owner=""
for a in "$@"; do
  case "$a" in -*) continue ;; esac
  if [ -z "$owner" ]; then owner=$a; continue; fi
  if [ -n "${FIXTURE_CONFIG_PATH:-}" ] && [ "$a" = "$FIXTURE_CONFIG_PATH" ]; then
    printf '%s' "$owner" > "$FIXTURE_CONFIG_OWNER"
  fi
done
exit 0
`

// 假 id：让脚本以为自己是普通用户（1000）。`auto` 推导成 `user` 只在「有 systemctl、提不了权」
// 时发生，而开发机/CI 常常以 root 跑测试（root 一律走 system）。只回答脚本用到的 -u / -un，
// 其余透传真 id。
const fakeID = `#!/bin/sh
case "$1" in
  -u) echo 1000 ;;
  -un) echo fixturenode ;;
  *) for p in /usr/bin/id /bin/id; do [ -x "$p" ] && exec "$p" "$@"; done; exit 1 ;;
esac
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

	// ownershipCheck 打开「所有权敏感」夹具：假 systemctl 的 is-active 只在
	// 「config 属主 == unit 的 User=」时成功（C1 的回归门）。
	ownershipCheck bool
	// installExit 非空时，下载到的二进制在 `install` 这一步以这个码退出（失败码传播用例）。
	installExit string
	// pathOverride 非空时顶掉子进程的 PATH（模拟「换个终端/换 sudo env 重跑」）。
	pathOverride string
	// asNonRoot 让脚本以为自己是普通用户（假 id）且没有 sudo（ANODE_SUDO 指向不存在的命令）：
	// “auto 推导成 user”只在这种情况下发生，而开发机/CI 常常以 root 跑测试（root 一律走 system）。
	asNonRoot  bool
	nonRootBin string
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
	f.nonRootBin = filepath.Join(root, "nonroot-bin")
	for _, dir := range []string{f.binDir, f.tmpDir, f.homeDir, f.nonRootBin} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	put(t, filepath.Join(f.nonRootBin, "id"), fakeID, 0755)
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
	// FIXTURE_INSTALL_EXIT 让 `install` 这一步失败（exit 码原样带出）：用来验证脚本既不吞掉
	// 失败，也不让节点停在「服务已 stop」的状态。
	payload := fmt.Sprintf("#!/bin/sh\nprintf '%%s %%s\\n' \"$0\" \"$*\" >> \"${FIXTURE_LOG:-/dev/null}\" 2>/dev/null || true\nif [ \"${FIXTURE_INSTALL_EXIT:-}\" != \"\" ] && [ \"$1\" = \"install\" ]; then\n  printf 'anode install: fixture failure\\n' >&2\n  exit \"$FIXTURE_INSTALL_EXIT\"\nfi\nexec %q \"$@\"\n", f.realBin)
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
			// 接受集以 fixtureToken 为前缀（令牌轮换用例用 fixtureToken+"A"/"B"），
			// fixtureWrong（不带这个前缀）仍然被 1008 赶走 —— 预检失败用例靠它。
			authOK := strings.HasPrefix(r.Header.Get(protocol.EmitAuthHeader), protocol.EmitAuthScheme+" "+fixtureToken)
			if !strings.HasPrefix(hello.Token, fixtureToken) || !authOK {
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
	path := f.pathOverride
	if path == "" {
		path = f.binDir + ":/usr/local/bin:/usr/bin:/bin"
	}
	if f.asNonRoot {
		// 假 id 放在最前面：脚本的 platform/提权/落盘判定都会看到“普通用户”。
		path = f.nonRootBin + ":" + path
	}
	env := []string{
		"PATH=" + path,
		"HOME=" + f.homeDir,
		"TMPDIR=" + f.tmpDir,
		"TZ=UTC",
		"FIXTURE_LOG=" + f.logPath,
		"FIXTURE_ROOT=" + f.root,
		"FIXTURE_PAYLOAD=" + f.payload,
		"FIXTURE_SUM=" + f.sumPath,
		"FIXTURE_COUNTER=" + f.counter,
		fmt.Sprintf("FIXTURE_ISACTIVE_FAIL=%d", f.isActiveFail),
		"FIXTURE_UNIT_FILE=" + filepath.Join(f.stage, "etc/systemd/system/anode.service"),
		"FIXTURE_CONFIG_PATH=" + filepath.Join(f.stage, "etc/anode/anode.yaml"),
		"FIXTURE_CONFIG_OWNER=" + filepath.Join(f.root, "config.owner"),
		"FIXTURE_TOKEN=" + fixtureToken,
		"FIXTURE_BINARY_PATH=" + protocol.NodeBinaryPath,
	}
	if f.ownershipCheck {
		env = append(env, "FIXTURE_OWNERSHIP=1")
	}
	if f.installExit != "" {
		env = append(env, "FIXTURE_INSTALL_EXIT="+f.installExit)
	}
	if f.sudoUser != "" {
		env = append(env, "SUDO_USER="+f.sudoUser)
	}
	if f.asNonRoot {
		// 「这台机器没有 sudo」：ANODE_SUDO 指向不存在的命令（脚本用它探测提权）。
		env = append(env, "ANODE_SUDO="+filepath.Join(f.root, "no-such-sudo"))
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
	// 参数只转发一次、不被脚本改写：--server 归一化后的 ws:// 地址必须原样到 anode install
	// （旧用例里"--server 真的转发过"的那条覆盖）。
	if got := lineArg(installLine, "--server"); got != f.hubWS {
		t.Fatalf("--server 没转发给 anode install：got %q want %q\n%s", got, f.hubWS, installLine)
	}
	if got := lineArg(installLine, "--prefix"); got != f.stage {
		t.Fatalf("--prefix 没转发给 anode install：got %q want %q", got, f.stage)
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
	if code != 2 {
		t.Fatalf("校验和不匹配应该按契约退 2，实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
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
	if code != 2 {
		t.Fatalf("没有令牌应该按契约退 2，实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
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
	if strings.Contains(out+errOut, fixtureWrong) {
		t.Fatalf("令牌泄露到输出")
	}
}

// ---- 6. --service none：只落盘，一个 systemctl 都不调，但**照样预检**（契约 D5，无例外）----

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
	// 预检**不因 --service none 而跳过**（R1/M3）："只落盘"也要先证明这把令牌能用，
	// 否则错令牌会以 exit 0 “装成功”却永远上不了线。
	if hits := f.stream.Load(); hits != 1 {
		t.Fatalf("--service none 也应该预检 1 次（契约 D5），实际 %d", hits)
	}
	// 手工命令必须打印出来，否则用户不知道下一步做什么。
	if !strings.Contains(out, "手工启动") || !strings.Contains(out, "systemctl enable --now anode.service") {
		t.Fatalf("没有打印手工启动命令：%s", out)
	}
}

// ---- 6b. 错令牌 + --service none：必须非 0，而不是“落盘即成功”----

func TestInstallScriptServiceNoneRejectsWrongToken(t *testing.T) {
	f := newScriptFixture(t)
	code, out, errOut := f.run(append(f.baseArgs(), "--service", "none", "--token", fixtureWrong)...)
	if code != 2 {
		t.Fatalf("错令牌 + --service none 应该按契约退 2（预检拦下），实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if hits := f.stream.Load(); hits != 1 {
		t.Fatalf("预检应该连中枢 1 次，实际 %d", hits)
	}
	if !strings.Contains(errOut, "预检失败") {
		t.Fatalf("预检失败没给出可定位诊断：%s", errOut)
	}
	f.mustNotExist(f.stage)
	if countLines(f.calls(), func(l string) bool { return hasArg(l, "install") }) != 0 {
		t.Fatalf("预检失败却调用了 anode install：%v", f.calls())
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

// ---- 9b. --dry-run --service user：preview 不得真改系统（R1/M1）----
//
// `loginctl enable-linger` 会写 /var/lib/systemd/linger/<user>（真实的系统状态修改），
// 与“--dry-run 不碰网络、不写盘、不提权”直接冲突。所以预演下 systemctl/loginctl 一个都不许执行。
func TestInstallScriptDryRunUserServiceNoSystemChanges(t *testing.T) {
	f := newScriptFixture(t)
	// 清掉 SUDO_USER：root 直跑时脚本会 getent 查家目录（只读查询），这里要的是**完全没有**
	// 任何命令被真的执行——M1 要钉的是“预演不改系统状态”，同时把 getent 也排掉更纯粹。
	f.sudoUser = ""
	// 用户服务不能与 --prefix 一起用（unit 会落在 prefix 下，systemctl --user 发现不到；
	// R2-B 会明确拒绝）。这里换成合法的用户服务布局（$HOME/.anode）——M1 的断言一条不少。
	args := []string{
		"--name", fixtureName,
		"--server", f.hubWS,
		"--version", "old-v1",
		"--download-url", f.downloadURL,
		"--checksum-url", f.checksumURL,
		"--service", "user", "--dry-run", "--token", fixtureToken,
	}
	code, out, errOut := f.run(args...)
	if code != 0 {
		t.Fatalf("--dry-run --service user 应该退 0，实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if lines := f.calls(); len(lines) != 0 {
		t.Fatalf("--dry-run --service user 不该真的执行任何命令（含 loginctl/systemctl），实际：%v", lines)
	}
	f.mustNotExist(filepath.Join(f.homeDir, ".anode"))
	if entries, err := os.ReadDir(f.tmpDir); err != nil || len(entries) != 0 {
		t.Fatalf("--dry-run 不该往 TMPDIR 写东西：%v %v", entries, err)
	}
	// 预演给出的 unit 落点必须是 systemd 真的会看的地方（$HOME/.config/systemd/user）。
	wantUnit := filepath.Join(f.homeDir, ".config/systemd/user/anode.service")
	if !strings.Contains(out, wantUnit) {
		t.Fatalf("预演没有给出用户 unit 的正确落点 %s：%s", wantUnit, out)
	}
	// 预演仍要告诉用户“真跑会开 linger”（否则摘要与实际行为脱节）。
	if !strings.Contains(out, "enable-linger") {
		t.Fatalf("预演没有说明会执行 enable-linger：%s", out)
	}
}

// ---- 9c. --token-file 轮换：与 --token 同档，自动 --update-token（R1/M2）----
func TestInstallScriptTokenFileRotation(t *testing.T) {
	f := newScriptFixture(t)
	tokA := filepath.Join(f.root, "token-a")
	tokB := filepath.Join(f.root, "token-b")
	put(t, tokA, fixtureToken+"A\n", 0600)
	if code, out, errOut := f.run(append(f.baseArgs(), "--token-file", tokA)...); code != 0 {
		t.Fatalf("首次用 --token-file 安装失败 %d\n%s\n%s", code, out, errOut)
	}
	cfg := filepath.Join(f.stage, "etc/anode/anode.yaml")
	first, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("读配置: %v", err)
	}
	if !strings.Contains(string(first), "token: "+fixtureToken+"A") {
		t.Fatalf("首装没有用 --token-file 里的令牌：%s", first)
	}

	f.resetLog()
	put(t, tokB, fixtureToken+"B\n", 0600)
	code, out, errOut := f.run(append(f.baseArgs(), "--token-file", tokB)...)
	if code != 0 {
		t.Fatalf("--token-file 轮换令牌应该成功（文档承诺会自动 --update-token），实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	after, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("轮换后读配置: %v", err)
	}
	if !strings.Contains(string(after), "token: "+fixtureToken+"B") || strings.Contains(string(after), fixtureToken+"A") {
		t.Fatalf("轮换后配置里的令牌没换成新的：%s", after)
	}
	_, installLine := findLine(f.calls(), isAnodeInstallCall)
	if !hasArg(installLine, "--update-token") {
		t.Fatalf("轮换没有自动带 --update-token：%s", installLine)
	}
	if strings.Contains(strings.Join(f.calls(), "\n"), fixtureToken) {
		t.Fatalf("令牌泄露到调用日志")
	}
}

// ---- 9d. 系统服务的所有权顺序：chown 必须在 restart 之前（R1/C1）----
//
// 模拟 systemd 的权限语义：unit 写 User=<安装者>，而 config 是 0600；属主不是那个用户时
// 服务读不到配置、根本起不来（is-active 永远非 active）。先 restart 再 chown 的旧顺序在这套
// 夹具下必然 die（修复前：is-active 轮询 10 次全失败）；把 chown 提到 restart 之前才会绿。
func TestInstallScriptSystemServiceChownBeforeRestart(t *testing.T) {
	f := newScriptFixture(t)
	f.ownershipCheck = true
	code, out, errOut := f.run(append(f.baseArgs(), "--token", fixtureToken)...)
	if code != 0 {
		t.Fatalf("system 服务安装应该成功（config 属主先交给 unit 的 User=），实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}

	unit := filepath.Join(f.stage, "etc/systemd/system/anode.service")
	unitBody, err := os.ReadFile(unit)
	if err != nil {
		t.Fatalf("读 unit: %v", err)
	}
	user := ""
	for _, line := range strings.Split(string(unitBody), "\n") {
		if strings.HasPrefix(line, "User=") {
			user = strings.TrimPrefix(line, "User=")
		}
	}
	if user == "" {
		t.Fatalf("系统服务的 unit 没有 User=：%s", unitBody)
	}
	ownerRaw, err := os.ReadFile(filepath.Join(f.root, "config.owner"))
	if err != nil {
		t.Fatalf("没有任何 chown 把 config 交给服务用户（%v）：%v", err, f.calls())
	}
	if got := strings.TrimSpace(string(ownerRaw)); got != user {
		t.Fatalf("config 的属主 = %q，unit 的 User= = %q（服务读不到 0600 配置）", got, user)
	}

	lines := f.calls()
	chownAt, chownLine := findLine(lines, func(l string) bool { return strings.HasPrefix(l, "chown ") })
	restartAt, _ := findLine(lines, func(l string) bool { return isDirectSystemctl(l) && hasArg(l, "restart") })
	if chownLine == "" || restartAt < 0 {
		t.Fatalf("没有 chown 或没有 restart：%v", lines)
	}
	if chownAt > restartAt {
		t.Fatalf("chown（第 %d 行）在 restart（第 %d 行）之后：服务会先读到读不了的配置而失败\n%v", chownAt, restartAt, lines)
	}
	// chown 必须同时覆盖 config 与 data（data 也是服务要写的）。
	if !hasArg(chownLine, filepath.Join(f.stage, "etc/anode/anode.yaml")) || !hasArg(chownLine, filepath.Join(f.stage, "var/lib/anode")) {
		t.Fatalf("chown 没有同时覆盖 config 与 data：%s", chownLine)
	}
}

// ---- 9e. 起不来时恢复旧服务：升级不能把节点搞成“停在停止态”（R1/C1 补偿）----
func TestInstallScriptRestoresOldServiceWhenStartFails(t *testing.T) {
	f := newScriptFixture(t)
	if code, out, errOut := f.run(append(f.baseArgs(), "--token", fixtureToken)...); code != 0 {
		t.Fatalf("首次安装失败 %d\n%s\n%s", code, out, errOut)
	}
	f.resetLog()
	// 升级时新实例永远起不来（is-active 一直非 active）：脚本必须把刚 stop 的旧服务恢复回去。
	f.isActiveFail = 1000
	code, out, errOut := f.run([]string{
		"--version", "old-v1",
		"--download-url", f.downloadURL,
		"--checksum-url", f.checksumURL,
		"--prefix", f.stage,
	}...)
	if code != 2 {
		t.Fatalf("wait_active 失败应该按契约退 2，实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	lines := f.calls()
	stopAt, stopLine := findLine(lines, func(l string) bool { return isDirectSystemctl(l) && hasArg(l, "stop") })
	startAt, startLine := findLine(lines, func(l string) bool { return isDirectSystemctl(l) && hasArg(l, "start") })
	if stopLine == "" || startLine == "" {
		t.Fatalf("失败后没有恢复旧服务（stop/start 不全）：%v", lines)
	}
	if startAt < stopAt {
		t.Fatalf("恢复顺序不对：start（%d）在 stop（%d）之前\n%v", startAt, stopAt, lines)
	}
	if !strings.Contains(out+errOut, "恢复") {
		t.Fatalf("失败结论没说清旧服务已被恢复：stdout=%s stderr=%s", out, errOut)
	}
}

// ---- 9f. anode install 失败：不吞失败、不把节点留在停止态（R1/M4 + 旧用例“失败码传播”）----
func TestInstallScriptInstallFailureRestoresServiceAndKeepsCode(t *testing.T) {
	f := newScriptFixture(t)
	if code, out, errOut := f.run(append(f.baseArgs(), "--token", fixtureToken)...); code != 0 {
		t.Fatalf("首次安装失败 %d\n%s\n%s", code, out, errOut)
	}
	f.resetLog()
	f.installExit = "7"
	code, out, errOut := f.run([]string{
		"--version", "old-v1",
		"--download-url", f.downloadURL,
		"--checksum-url", f.checksumURL,
		"--prefix", f.stage,
	}...)
	// 契约 §3：脚本失败一律 2；但子进程的失败不能被吞——它必须出现在诊断里（旧用例的“失败码传播”）。
	if code != 2 {
		t.Fatalf("install 失败应该按契约退 2，实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "退出码 7") {
		t.Fatalf("install 的失败码没有传播到诊断里：%s", errOut)
	}
	if !strings.Contains(out+errOut, "anode install: fixture failure") {
		t.Fatalf("install 的原文输出没有透传：stdout=%s stderr=%s", out, errOut)
	}
	lines := f.calls()
	if countLines(lines, func(l string) bool { return isDirectSystemctl(l) && hasArg(l, "start") }) != 1 {
		t.Fatalf("install 失败后没有恢复旧服务：%v", lines)
	}
	// 失败发生在起服务之前：没有 restart，也没有把 unit 留在 enable 后死状态的内疚。
	if countLines(lines, func(l string) bool { return isDirectSystemctl(l) && hasArg(l, "restart") }) != 0 {
		t.Fatalf("install 失败却 restart 了服务：%v", lines)
	}
}

// ---- 9g. 不同 caller PATH 重跑 = 同一个 unit（R1/M4）----
//
// unit 里的 Environment=PATH 不能照抄安装时的 $PATH：否则换一个终端（direnv/nvm/不同 ssh）
// 重跑就会被 anode install 判为“unit 被改过”而拒绝，节点停留在“服务已 stop”。
func TestInstallScriptUpgradeStablePathAcrossCallerPATH(t *testing.T) {
	f := newScriptFixture(t)
	if code, out, errOut := f.run(append(f.baseArgs(), "--token", fixtureToken)...); code != 0 {
		t.Fatalf("首次安装失败 %d\n%s\n%s", code, out, errOut)
	}
	unit := filepath.Join(f.stage, "etc/systemd/system/anode.service")
	before, err := os.ReadFile(unit)
	if err != nil {
		t.Fatalf("读 unit: %v", err)
	}
	if strings.Contains(string(before), f.binDir) {
		t.Fatalf("unit 的 PATH 照抄了调用者 PATH（含夹具 bin 目录）：%s", before)
	}

	f.resetLog()
	// 换一个“终端”：PATH 的目录集合/顺序都变了（但假命令仍在最前面，脚本还能跑）。
	f.pathOverride = f.binDir + ":/opt/caller-only/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	code, out, errOut := f.run([]string{
		"--version", "old-v1",
		"--download-url", f.downloadURL,
		"--checksum-url", f.checksumURL,
		"--prefix", f.stage,
	}...)
	if code != 0 {
		t.Fatalf("不同 caller PATH 重跑应该等于同一个 unit（升级成功），实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	after, err := os.ReadFile(unit)
	if err != nil {
		t.Fatalf("升级后读 unit: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("caller PATH 变了就换了 unit（升级会拒绝覆盖）\nbefore:\n%s\nafter:\n%s", before, after)
	}
	// 稳定集合：$HOME/.local/bin + 固定系统路径，不照抄调用者 PATH。
	wantPath := ""
	if f.sudoUser != "" {
		wantPath = "/home/" + f.sudoUser + "/.local/bin"
	} else {
		wantPath = f.homeDir + "/.local/bin"
	}
	wantPath += ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	if !strings.Contains(string(after), "Environment=PATH="+wantPath+"\n") {
		t.Fatalf("unit 的 PATH 不是稳定集合\nwant Environment=PATH=%s\ngot unit:\n%s", wantPath, after)
	}
	if !strings.Contains(out, "升级完成") {
		t.Fatalf("升级结论没写清楚：%s", out)
	}
}

// ---- 9h. 显式 --service system + --prefix：unit 不在 systemd 搜索路径里，拦下来（R1/m2）----
func TestInstallScriptExplicitSystemWithPrefixRejected(t *testing.T) {
	f := newScriptFixture(t)
	code, out, errOut := f.run(append(f.baseArgs(), "--service", "system", "--token", fixtureToken)...)
	if code != 2 {
		t.Fatalf("--prefix + --service system 应该明确报错退 2（否则 systemctl enable 必失败），实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "--prefix") || !strings.Contains(errOut, "--service system") {
		t.Fatalf("报错没说清是哪两个参数冲突：%s", errOut)
	}
	f.mustNotExist(f.stage)
	if n := countLines(f.calls(), isSystemctlCall); n != 0 {
		t.Fatalf("参数冲突却已经动了服务：%v", f.calls())
	}
}

// ---- 9i. 中枢下载路径（UI 一条命令的主路径）：令牌只走 curl --config，不进 argv（R1/minor）----
func TestInstallScriptHubDownloadKeepsTokenOutOfArgv(t *testing.T) {
	f := newScriptFixture(t)
	// 不给 --download-url/--version：走中枢 GET /v1/node/binary（假 curl 按响应头回放）。
	code, out, errOut := f.run("--name", fixtureName, "--server", f.hubWS, "--token", fixtureToken, "--prefix", f.stage)
	if code != 0 {
		t.Fatalf("从中枢下载的安装应该成功，实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	lines := f.calls()
	if countLines(lines, func(l string) bool { return l == "curl-auth ok" }) != 1 {
		t.Fatalf("下载令牌没有经 curl --config 交给 curl：%v", lines)
	}
	binReq := countLines(lines, func(l string) bool {
		return strings.HasPrefix(l, "curl ") && strings.Contains(l, protocol.NodeBinaryPath)
	})
	if binReq != 1 {
		t.Fatalf("应该向中枢请求 %s 恰好 1 次，实际 %d：%v", protocol.NodeBinaryPath, binReq, lines)
	}
	for _, l := range lines {
		if strings.Contains(l, fixtureToken) {
			t.Fatalf("令牌进了 curl 的 argv（同机其他用户可从 /proc/<pid>/cmdline 读到）：%s", l)
		}
	}
	cfg, err := os.ReadFile(filepath.Join(f.stage, "etc/anode/anode.yaml"))
	if err != nil {
		t.Fatalf("读配置: %v", err)
	}
	if !strings.Contains(string(cfg), "token: "+fixtureToken) {
		t.Fatalf("配置里没有节点令牌：%s", cfg)
	}
	if strings.Contains(out+errOut, fixtureToken) {
		t.Fatalf("令牌泄露到输出：stdout=%s stderr=%s", out, errOut)
	}
}

// ---- 9j. 预检失败也要恢复旧服务：升级不能把节点留在停止态（R2-A）----
//
// 顺序是 stop 旧实例 → 预检。如果预检失败就在那里 die，旧节点就停在停止态；而旧文案还
// 声称“服务没有被改动”。这里钉两件事：stop 之后必须有 start（恢复），且诊断说真话。
func TestInstallScriptPreflightFailureRestoresOldService(t *testing.T) {
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

	// 升级 + 错令牌：stop 已经发生，预检必然失败 —— 脚本必须把旧实例拉回来再退出。
	code, out, errOut := f.run(append(f.baseArgs(), "--token", fixtureWrong)...)
	if code != 2 {
		t.Fatalf("错令牌重跑应该按契约退 2，实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	lines := f.calls()
	stopAt, stopLine := findLine(lines, func(l string) bool { return isDirectSystemctl(l) && hasArg(l, "stop") })
	if stopLine == "" {
		t.Fatalf("升级没有先停旧服务：%v", lines)
	}
	startAt, startLine := findLine(lines, func(l string) bool { return isDirectSystemctl(l) && hasArg(l, "start") })
	if startLine == "" {
		t.Fatalf("预检失败后旧服务没有被恢复（stop 之后没有 start/restart）：%v", lines)
	}
	if startAt < stopAt {
		t.Fatalf("恢复顺序不对：start（第 %d 行）在 stop（第 %d 行）之前\n%v", startAt, stopAt, lines)
	}
	// 诊断必须说真话：正式配置没动 + 旧服务已恢复。
	if !strings.Contains(errOut, "旧服务已恢复") {
		t.Fatalf("预检失败没有说明旧服务已恢复：%s", errOut)
	}
	if strings.Contains(errOut, "服务都没有被改动") || strings.Contains(errOut, "服务均未改动") {
		t.Fatalf("预检失败却声称服务没被改动（stop 是改动）：%s", errOut)
	}
	// 预检失败仍然不落盘、不 install。
	if countLines(lines, isAnodeInstallCall) != 0 {
		t.Fatalf("预检失败却调用了 anode install：%v", lines)
	}
	after, err := os.ReadFile(cfg)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("预检失败改动了正式配置（err=%v）\nbefore:\n%s\nafter:\n%s", err, before, after)
	}
	if strings.Contains(out+errOut, fixtureWrong) || strings.Contains(out+errOut, fixtureToken) {
		t.Fatalf("令牌泄露到输出")
	}
}

// ---- 9k. 没停过旧服务就不许凭空 start（R2-A 的另一半）----
func TestInstallScriptPreflightFailureNoPhantomStart(t *testing.T) {
	f := newScriptFixture(t)
	// 首装（没有旧 unit）+ 错令牌：预检失败，但没停过任何东西。
	code, out, errOut := f.run(append(f.baseArgs(), "--token", fixtureWrong)...)
	if code != 2 {
		t.Fatalf("预检失败的退出码应该是 2（契约），实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if n := countLines(f.calls(), isSystemctlCall); n != 0 {
		t.Fatalf("首次安装预检失败却动了 systemctl %d 次（没停过的东西不该 start）：%v", n, f.calls())
	}
	if strings.Contains(errOut, "旧服务已恢复") {
		t.Fatalf("没有旧服务却声称恢复了旧服务：%s", errOut)
	}
	if !strings.Contains(errOut, "无需恢复") {
		t.Fatalf("预检失败没有说清没有旧服务被停：%s", errOut)
	}
	f.mustNotExist(f.stage)
}

// ---- 9l. 显式 --prefix + --service user：unit 落在 prefix 下，systemctl --user 发现不到（R2-B）----
func TestInstallScriptExplicitUserWithPrefixRejected(t *testing.T) {
	f := newScriptFixture(t)
	code, out, errOut := f.run(append(f.baseArgs(), "--service", "user", "--token", fixtureToken)...)
	if code != 2 {
		t.Fatalf("--prefix + --service user 应该明确报错退 2（unit 不在 systemd 的发现路径），实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "--prefix") || !strings.Contains(errOut, "--service user") {
		t.Fatalf("报错没说清是哪两个参数冲突：%s", errOut)
	}
	// 出路必须是真的能走通的：只落盘用 none；要用户服务就去掉 --prefix。
	if !strings.Contains(errOut, "--service none") || !strings.Contains(errOut, "去掉 --prefix") {
		t.Fatalf("报错没有给出可行出路：%s", errOut)
	}
	// 参数冲突在任何副作用之前拦下：不落盘、不调 systemctl、也不连中枢。
	f.mustNotExist(f.stage)
	if n := countLines(f.calls(), isSystemctlCall); n != 0 {
		t.Fatalf("参数冲突却已经动了服务：%v", f.calls())
	}
	if hits := f.stream.Load(); hits != 0 {
		t.Fatalf("参数冲突却已经连了中枢 %d 次", hits)
	}
	// 与 system 口径一致：--dry-run 也不假装这条路能走（root 直跑时清掉 SUDO_USER，
	// 免得 resolve_runner 的只读 getent 混进“没有执行任何命令”的断言）。
	f.sudoUser = ""
	f.resetLog()
	code, out, errOut = f.run(append(f.baseArgs(), "--service", "user", "--dry-run", "--token", fixtureToken)...)
	if code != 2 {
		t.Fatalf("--prefix + --service user 在 --dry-run 下也应该退 2，实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if lines := f.calls(); len(lines) != 0 {
		t.Fatalf("参数冲突的预演不该执行任何命令：%v", lines)
	}
	f.mustNotExist(f.stage)
}

// ---- 9m. auto 推导出 user + --prefix：降级为只落盘，不静默装一个起不来的服务（R2-B）----
//
// auto → user 只在「有 systemctl、提不了权」时发生，而测试机常常就是 root。夹具用假 `id`
// 让脚本以为自己是普通用户，并把 ANODE_SUDO 指向不存在的命令（= 这台机器没有 sudo）。
func TestInstallScriptAutoUserWithPrefixDowngradesToNone(t *testing.T) {
	f := newScriptFixture(t)
	f.asNonRoot = true
	code, out, errOut := f.run(append(f.baseArgs(), "--token", fixtureToken)...)
	if code != 0 {
		t.Fatalf("降级为只落盘后应该装成功（exit 0），实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if !strings.Contains(out+errOut, "降级为只落盘") {
		t.Fatalf("auto 推导出的用户服务在 --prefix 下没有降级/没说明：stdout=%s stderr=%s", out, errOut)
	}
	if !strings.Contains(out, "未安装服务") {
		t.Fatalf("SERVICE 实际不是 none（摘要没说未安装服务）：%s", out)
	}
	if n := countLines(f.calls(), isSystemctlCall); n != 0 {
		t.Fatalf("降级成 none 却调用了 systemctl %d 次（装了一个 systemctl --user 发现不到的服务）：%v", n, f.calls())
	}
	// 只落盘仍然要落：二进制 + 配置在，令牌没泄露。
	f.assertPerm(filepath.Join(f.stage, "etc/anode/anode.yaml"), 0600)
	if _, err := os.Stat(filepath.Join(f.stage, "usr/local/bin/anode")); err != nil {
		t.Fatalf("二进制没落盘: %v", err)
	}
	if hits := f.stream.Load(); hits != 1 {
		t.Fatalf("只落盘也要照样预检 1 次，实际 %d", hits)
	}
	if strings.Contains(out+errOut, fixtureToken) {
		t.Fatalf("令牌泄露到输出")
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
