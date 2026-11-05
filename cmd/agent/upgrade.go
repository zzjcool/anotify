package main

// 本文件是 `anotify-agent upgrade` 子命令（契约 §4）：把**节点上的这个二进制**
// 换成中枢当前带着的那份。
//
// 为什么升级源是中枢自己、而不是让节点去 GitHub 下 release：
//
//   - 内网/离线机器根本连不上 GitHub（而它们恰恰是最需要"有一条升级路"的机器）；
//   - 「中枢跑的是哪一版」才是节点该对齐的事实：升级源与中控同源，才不会出现
//     「中枢 1.5、节点 1.4、手册对不上」这种三方各说各话。
//
// # 一次升级按顺序做这几件事，任何一步失败都不许动目标文件
//
//  1. 读 node.yaml（与主命令同一套解析：loadConfigFrom）拿 server + token；
//  2. GET {server}/v1/node/binary（**node token** 鉴权，不是会话令牌）→ 二进制字节
//     + X-Agent-Version + X-Agent-Sha256；
//  3. 目标已同 sha256 → 什么都不做（`--force` 才继续覆盖）；
//  4. 下载到**目标同目录**的临时文件 → 校验 sha256（不符即删临时文件并失败）
//     → chmod 0755 → rename 原子替换；
//  5. 替换成功 → 打印「重启后生效」；`--restart` 时执行
//     `systemctl restart anotify-agent`（失败**只告警**：二进制已经换好了）。
//
// 「同目录临时文件 + rename」不是讲究，是正确性：/tmp 与 /usr/local/bin 常常不是
// 同一个挂载，跨设备 rename 会退化成「先删后拷」——那正好是「升级过程中断，
// 机器上留下半截二进制」的来源。也不需要停下来问人：rename 是原子的，
// 要么旧的全在，要么新的全在。
//
// # 退出码（契约 §4 的表，逐条对应）
//
//	0 成功 / 已最新 · 1 已最新（仅 --check）· 2 服务端未启用（501）
//	3 权限不足（需要 root）· 4 校验失败（sha256 不符）· 5 网络/其他错误
//
// 脚本据退出码决定下一步：3 去要 root（提示里给了能直接粘的一行），
// 4 是「服务端二进制被改了 / 传输坏了，别原样重试」，5 可以原样重跑。
// 0 与 1 分开是为了让 `--check` 能当「有更新吗」用在 cron 里。

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/version"
)

// upgradeCommand 是自升级子命令名：`anotify-agent upgrade`。
const upgradeCommand = "upgrade"

// upgrade 子命令专属的退出码（契约 §4）。0 复用进程级的 exitOK。
const (
	// exitUpgradeUpToDate：`--check` 且本地已是最新（**只有** --check 用它）。
	exitUpgradeUpToDate = 1
	// exitUpgradeUnavailable：服务端没有 agent 二进制（501 agent_binary_unavailable）。
	exitUpgradeUnavailable = 2
	// exitUpgradeNeedRoot：目标不可写（EACCES/EPERM），需要 root。
	exitUpgradeNeedRoot = 3
	// exitUpgradeBadSHA：下载内容与 X-Agent-Sha256 不符。
	exitUpgradeBadSHA = 4
	// exitUpgradeFailure：网络/其他错误（以及参数、配置错误）。
	exitUpgradeFailure = 5
)

const (
	// binaryEndpointPath 是中枢提供 agent 二进制的端点（契约 §2）。
	binaryEndpointPath = "/v1/node/binary"
	// headerAgentVersion / headerAgentSHA256 是那条响应上的两个元数据头。
	headerAgentVersion = "X-Agent-Version"
	headerAgentSHA256  = "X-Agent-Sha256"
	// agentServiceName 是 systemd 单元名（`--restart` 时用；与 install-agent.sh 一致）。
	agentServiceName = "anotify-agent"
	// upgradeTmpPrefix 是下载临时文件的前缀（契约 §4.6）：建在**目标同目录**，
	// 以 `.` 开头是为了不在 `ls` 里晃眼睛（也顺手避开 shell glob）。
	upgradeTmpPrefix = ".anotify-agent.tmp-"
	// defaultAgentBinaryName 是回落目标目录（~/.anotify/bin）里的文件名。
	defaultAgentBinaryName = "anotify-agent"
	// upgradeDownloadTimeout 是整次下载的上限：15 MB 的二进制 + 慢链路上也要够用，
	// 同时不让「连上了但永远不结束」把进程挂死。
	upgradeDownloadTimeout = 5 * time.Minute
)

// upgradeOptions 是 `upgrade` 子命令解析出来的参数。
type upgradeOptions struct {
	configPath string // -f：node.yaml 路径（与主命令同一个 flag）
	check      bool   // --check：只比较，不下载不替换
	force      bool   // --force：已是最新也强制覆盖
	server     string // --server：覆盖 node.yaml 里的中枢地址
	installTo  string // --install-to：替换哪个文件
	restart    bool   // --restart：替换后 systemctl restart
}

// parseUpgradeFlags 解析 `upgrade` 子命令的参数。
//
// 与 emit / run 子命令同一套风格：ContinueOnError + 每个子命令自己的 FlagSet
// （不碰全局 flag.CommandLine），退出码只由 runUpgrade 决定，flag 包只负责把
// usage 写到 stderr。
func parseUpgradeFlags(args []string, stderr io.Writer) (upgradeOptions, error) {
	fs := flag.NewFlagSet("agent upgrade", flag.ContinueOnError)
	fs.SetOutput(stderr)

	configPath := fs.String("f", "",
		"node.yaml 路径（不给则先看 ANOTIFY_TOKEN 等环境变量，再退到 ./node.yaml；与主命令一致）")
	check := fs.Bool("check", false,
		"只比较：有更新退 0、已是最新退 1、服务端未启用退 2（不下载、不替换）")
	force := fs.Bool("force", false, "已是最新也强制下载覆盖（--check 时无效果）")
	server := fs.String("server", "", "覆盖 node.yaml 里的 server（换了中枢地址时用）")
	installTo := fs.String("install-to", "",
		"替换哪个文件（缺省 = 当前运行的可执行，解 symlink；判不出来时用 ~/.anotify/bin/anotify-agent）")
	restart := fs.Bool("restart", false,
		"替换成功后执行 systemctl restart "+agentServiceName+"（失败只告警，退出码仍是 0）")

	if err := fs.Parse(args); err != nil {
		return upgradeOptions{}, err
	}
	if extra := fs.Args(); len(extra) > 0 {
		return upgradeOptions{}, fmt.Errorf("多余的参数: %s", strings.Join(extra, " "))
	}
	return upgradeOptions{
		configPath: *configPath,
		check:      *check,
		force:      *force,
		server:     *server,
		installTo:  *installTo,
		restart:    *restart,
	}, nil
}

// upgradeMain 是 mainWith 里 upgrade 分支的全部逻辑：跑一次 + 把失败原因打出来。
//
// 抽出来是为了单测能一口气断言「退出码 + stdout + stderr」——尤其是 3/4 这两档
// 靠的**就是**那句人话（`sudo install …` / 「校验失败」），只断言退出码等于
// 什么都没测。
func upgradeMain(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	code, err := runUpgrade(args, stdout, stderr, getenv)
	if err != nil {
		// 只在这里打一次：runUpgrade 只返回、不打印错误（避免同一句话出现两遍）。
		fmt.Fprintf(stderr, "anotify-agent upgrade: %s\n", err)
	}
	return code
}

// runUpgrade 是 `anotify-agent upgrade` 的实现。
//
// 返回 (退出码, error)：error 只用于**说明**（upgradeMain 打到 stderr 一次），
// 退出码本身已经表达了「脚本该做什么」。stdout/stderr/getenv 都是参数：
// 这条路径要被单测逐条钉住退出码表，而 os.Stdout / os.Getenv 是全局状态，测不动。
func runUpgrade(args []string, stdout, stderr io.Writer, getenv func(string) string) (int, error) {
	opts, err := parseUpgradeFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, nil // -h：usage 已由 flag 包写到 stderr，退出码 0
		}
		return exitUpgradeFailure, err
	}

	cfg, err := upgradeConfig(opts.configPath, opts.server, getenv)
	if err != nil {
		return exitUpgradeFailure, err
	}
	endpoint, err := binaryEndpoint(cfg.Server)
	if err != nil {
		return exitUpgradeFailure, err
	}

	resp, err := requestBinary(endpoint, cfg.Token)
	if err != nil {
		return exitUpgradeFailure, err
	}
	defer resp.Body.Close()

	// 501 = 服务端**没带** agent 二进制（契约 §2）。必须与网络错误分开说：
	// 前者是「这台中枢的镜像里没有 /app/agent」，重试一百次也不会好。
	if resp.StatusCode == http.StatusNotImplemented {
		return exitUpgradeUnavailable, errors.New(
			"服务端没有 agent 二进制（501 agent_binary_unavailable）：" +
				"这台中枢没带 agent 升级源（镜像里缺 /app/agent，或 ANOTIFY_AGENT_BIN 指的文件不存在）。" +
				"换一个带 agent 的镜像，或用 --install-to 自己装")
	}
	if resp.StatusCode != http.StatusOK {
		return exitUpgradeFailure, fmt.Errorf("GET %s 返回 %d（期望 200）", endpoint, resp.StatusCode)
	}

	remoteSHA := strings.ToLower(strings.TrimSpace(resp.Header.Get(headerAgentSHA256)))
	if remoteSHA == "" {
		// 没有校验和就不许替换：这条路径是「把远端字节写进你的 /usr/local/bin」，
		// 除了 TLS 之外没有别的完整性保证（契约 §4.6 的全部意义）。
		return exitUpgradeFailure, errors.New(
			"服务端没回 " + headerAgentSHA256 + "，拒绝在没有校验和的情况下替换二进制")
	}
	remoteVersion := strings.TrimSpace(resp.Header.Get(headerAgentVersion))
	if remoteVersion == "" {
		remoteVersion = "unknown"
	}

	target, err := resolveTarget(opts.installTo, os.Executable, stdout)
	if err != nil {
		return exitUpgradeFailure, err
	}
	currentSHA, err := fileSHA256(target)
	if err != nil {
		return exitUpgradeFailure, fmt.Errorf("读取目标 %s: %w", target, err)
	}
	upToDate := currentSHA != "" && currentSHA == remoteSHA

	// --check：只回答「有没有更新」，一个字节都不下载（契约 §4.3）。
	if opts.check {
		if upToDate {
			fmt.Fprintf(stdout, "已是最新：%s（sha256 %s，%s）\n",
				version.String(), shortSHA(currentSHA), target)
			return exitUpgradeUpToDate, nil
		}
		fmt.Fprintf(stdout, "可升级：当前 %s（sha256 %s）→ 目标 %s（sha256 %s），目标路径 %s\n",
			version.String(), shortSHA(currentSHA), remoteVersion, shortSHA(remoteSHA), target)
		return exitOK, nil
	}
	// 同 sha 已最新（契约 §4.4）：不动文件。--force 是给「二进制看着对但想重装」用的。
	if upToDate && !opts.force {
		fmt.Fprintf(stdout, "already up to date: %s（sha256 %s，%s）\n",
			version.String(), shortSHA(remoteSHA), target)
		return exitOK, nil
	}

	// ---- 下载 → 校验 → 原子替换 ----

	tmp, placed, err := openDownloadFile(target)
	if err != nil {
		return exitUpgradeFailure, err
	}
	// 临时文件的生命周期只有一个方向：要么 rename 成目标（此时它已经不是"临时"
	// 了），要么被删掉。需要 root 的那一档是唯一例外——那个路径要交给用户去
	// sudo install，删了就等于让他重新下载一遍。
	keepTmp := false
	defer func() {
		if !keepTmp {
			_ = os.Remove(tmp.Name())
		}
	}()

	gotSHA, err := downloadTo(tmp, resp.Body)
	if err != nil {
		return exitUpgradeFailure, fmt.Errorf("下载中断（%s 未改动）: %w", target, err)
	}
	if gotSHA != remoteSHA {
		// 契约 §4.6：不符即删临时文件并失败。**绝不**把它装上去再"看看好不好用"。
		return exitUpgradeBadSHA, fmt.Errorf(
			"校验失败：下载内容 sha256 = %s，服务端声明 %s；已丢弃下载内容，%s 未改动"+
				"（服务端二进制可能被改动，或传输被截断）",
			gotSHA, remoteSHA, target)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return exitUpgradeFailure, fmt.Errorf("设置权限 0755: %w", err)
	}

	if !placed {
		// 目标目录一开始就不可写：**不静默降级**到 $HOME 之类的地方（契约 §4.7）。
		// 静默换路径的症状是「升级报成功，跑的却还是旧那个」——比失败难查得多。
		// 这里把已校验的二进制留在临时路径，给出能直接粘的一行，退 3。
		keepTmp = true
		fmt.Fprint(stderr, needRootHint(tmp.Name(), target))
		return exitUpgradeNeedRoot, fmt.Errorf("目标目录不可写，需要 root：%s", filepath.Dir(target))
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		if isPermissionError(err) {
			keepTmp = true
			fmt.Fprint(stderr, needRootHint(tmp.Name(), target))
			return exitUpgradeNeedRoot, fmt.Errorf("替换 %s 需要 root: %w", target, err)
		}
		return exitUpgradeFailure, fmt.Errorf("替换 %s: %w", target, err)
	}
	// 已经把临时文件的 inode 挪成目标了：别再按老路径去删（那里可能很快被
	// 别人建出同名的东西）。
	keepTmp = true

	fmt.Fprintf(stdout, "已更新 %s：%s → %s（sha256 %s），重启后生效\n",
		target, version.String(), remoteVersion, shortSHA(remoteSHA))

	if opts.restart {
		if err := restartAgentService(); err != nil {
			// 契约 §4.8：二进制已经换好了，重启失败**不能**改退出码——
			// 否则脚本会以为"没升级"，于是再装一遍。
			fmt.Fprintf(stderr,
				"注意：重启服务失败（二进制已经换好了）：%s\n  手动执行：sudo systemctl restart %s\n",
				err, agentServiceName)
		} else {
			fmt.Fprintf(stdout, "已重启 %s\n", agentServiceName)
		}
	}
	return exitOK, nil
}

// upgradeConfig 读 node.yaml（与主命令同一套优先级与校验），可选覆盖 server。
//
// 为什么不在这里自己解析一遍 YAML：`-f` / 环境变量 / `./node.yaml` 的优先级、
// 未知字段报错、server 的 scheme 规则都只有**一份**事实（config.go）。
// 两条路径各写一份解析，迟早会出现「主命令能起、upgrade 说配置非法」。
func upgradeConfig(configPath, serverOverride string, getenv func(string) string) (*nodeConfig, error) {
	cfg, err := loadConfigFrom(configPath, getenv)
	if err != nil {
		return nil, err
	}
	if s := strings.TrimSpace(serverOverride); s != "" {
		cfg.Server = s
		if err := cfg.validate(); err != nil {
			return nil, fmt.Errorf("--server %q: %w", s, err)
		}
	}
	return cfg, nil
}

// binaryEndpoint 把 node.yaml 里的 server 映射到 GET /v1/node/binary（契约 §2）。
//
// 规则与 streamURL 一致（必须带 scheme；http/ws、https/wss 各成一组），区别只在
// **换协议**而不是换路径：node.yaml 的 server 是给 WebSocket 用的，自升级走的是
// 普通 HTTP 下载。所以 ws:// 要变成 http://，否则这里会拿一个 WS 客户端去见文件。
func binaryEndpoint(server string) (string, error) {
	raw := strings.TrimSpace(server)
	if raw == "" {
		return "", errors.New("server is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("server %q is not a valid URL: %w", raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "ws":
		u.Scheme = "http"
	case "https", "wss":
		u.Scheme = "https"
	default:
		return "", fmt.Errorf("server %q is not a valid URL (scheme must be http, https, ws or wss)", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("server %q is not a valid URL (missing host)", raw)
	}
	path := strings.TrimSuffix(u.Path, "/")
	if !strings.HasSuffix(path, binaryEndpointPath) {
		path += binaryEndpointPath
	}
	u.Path = path
	return u.String(), nil
}

// requestBinary 发那个 GET（node token 鉴权）。
//
// 令牌走 Authorization 头而不是 query：进 URL 的令牌会被反代/日志记下来
// （与 dialNode 同一条纪律）。
func requestBinary(endpoint, token string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求 %s: %w", endpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	// 明说「要二进制」：反代/网关据此决定按流式转发而不是 text 处理。
	req.Header.Set("Accept", "application/octet-stream")

	client := &http.Client{Timeout: upgradeDownloadTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连不上 %s: %w", endpoint, err)
	}
	return resp, nil
}

// resolveTarget 按契约 §4.5 的优先级决定替换哪个文件：
//
//	--install-to > 当前运行的可执行（os.Executable 解 symlink）> ~/.anotify/bin/anotify-agent
//
// 回落**只**发生在「当前可执行判不出来」时（os.Executable 报错 / 返回空，比如
// 某些容器里 /proc 挂得奇怪，或二进制已被删除）。刻意**不**因为「目标目录不可写」
// 就悄悄换成 $HOME：那是契约 §4.7 明令禁止的静默降级（症状：升级报成功、
// 实际跑的仍是旧那份），那种情况由 runUpgrade 报「需要 root」并退 3。
//
// exe 参数是「取当前可执行」的方法，参数化只为单测能模拟 os.Executable 失败。
func resolveTarget(explicit string, exe func() (string, error), stdout io.Writer) (string, error) {
	if p := strings.TrimSpace(explicit); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", fmt.Errorf("--install-to %q: %w", p, err)
		}
		return abs, nil
	}

	if exe != nil {
		if p, err := exe(); err == nil && strings.TrimSpace(p) != "" {
			// Linux 在二进制已被删除时返回 "/path/anotify-agent (deleted)"：
			// 直接拿去当目标会创建一个叫 "anotify-agent (deleted)" 的文件。
			p = strings.TrimSuffix(p, " (deleted)")
			// 解 symlink：install-agent.sh 装的 /usr/local/bin/anotify-agent 可能
			// 是指向别处的软链。替换软链本身就等于「什么都没换」——内核 exec 的是
			// 链的另一头，用户重跑的还是旧二进制。
			if resolved, rerr := filepath.EvalSymlinks(p); rerr == nil {
				p = resolved
			}
			return p, nil
		}
	}

	// 回落：~/.anotify/bin/anotify-agent。刻意与 node.yaml 同属 ~/.anotify：
	// 那里是用户自己的目录，不需要 root 就能自升级（容器里 /usr/local/bin 只读时
	// 这是唯一活得下去的路径）。
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("既判不出当前可执行，也定位不到家目录: %w", err)
	}
	dir := filepath.Join(home, ".anotify", "bin")
	fallback := filepath.Join(dir, defaultAgentBinaryName)
	// 目录可能还不存在（第一次自升级）：创建它，否则 rename 会因为没有父目录失败。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建 %s: %w", dir, err)
	}
	fmt.Fprintf(stdout, "注意：判不出当前运行的可执行，改用 %s（--install-to 可显式指定）\n", fallback)
	return fallback, nil
}

// openDownloadFile 在**目标同目录**建下载临时文件（契约 §4.6）。
//
// 返回 placed=false 表示目标目录不可写，文件建在了系统临时目录：这不是降级，
// 而是「让后面那条 sudo install 有东西可装」——用户拿到的仍然是一个已经校验
// 通过的二进制，而不是一句「你自己再去下吧」。替换动作仍然只发生在目标路径上。
func openDownloadFile(target string) (f *os.File, placed bool, err error) {
	dir := filepath.Dir(target)
	tmp, cerr := os.CreateTemp(dir, upgradeTmpPrefix)
	if cerr == nil {
		return tmp, true, nil
	}
	if !isPermissionError(cerr) {
		return nil, false, fmt.Errorf("在 %s 建临时文件: %w", dir, cerr)
	}
	tmp, terr := os.CreateTemp("", upgradeTmpPrefix)
	if terr != nil {
		return nil, false, fmt.Errorf(
			"目标目录 %s 不可写（%w），系统临时目录也建不了文件: %w", dir, cerr, terr)
	}
	return tmp, false, nil
}

// downloadTo 把响应体写进临时文件，边写边算 sha256，返回十六进制摘要。
//
// 边写边算（io.MultiWriter）而不是"写完再读一遍"：15 MB 不必读第二遍，
// 也顺手避免了「校验的是另一份文件」这种把自己的错当成别人的错的写法。
// 无论成功失败都关掉文件（后面 rename 要拿它当 inode 用）。
func downloadTo(f *os.File, body io.Reader) (string, error) {
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), body)
	closeErr := f.Close()
	if copyErr != nil {
		return "", fmt.Errorf("已写 %d 字节后中断: %w", n, copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("关闭临时文件: %w", closeErr)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fileSHA256 算目标文件当前的 sha256；文件不存在返回 ("", nil)。
//
// 「目标还没有」不是错误：它是「第一次装」的正常前提（也就自然落到"有更新"）。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// isPermissionError 判「需要 root」这一类错误（EACCES / EPERM，含被包装的情况）。
//
// 单拎出来是因为它有专门的退出码（3）：把它和「磁盘满了」「路径不存在」混成 5，
// 脚本就没法自动决定「去申请 sudo」。
func isPermissionError(err error) bool {
	return errors.Is(err, fs.ErrPermission)
}

// shortSHA 取前 12 位当展示口径（契约 §4.3）；空值返回「未知」。
//
// 前 12 位是「一眼能对、又不至于糊满一行」的长度；完整值仍然在文件上，
// 需要核对时用 sha256sum 比。
func shortSHA(sha string) string {
	if sha == "" {
		return "未知"
	}
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

// needRootHint 是「需要 root」时那条**能直接粘**的命令（契约 §4.7）。
//
// 路径做 shell 引用：这行是要原样粘进 shell 的，路径里有空格时没引号会把文件
// 装到另一个地方（而 `install` 不会因此报错）。
func needRootHint(tmpPath, target string) string {
	return fmt.Sprintf(
		"目标 %s 需要 root 才能替换（已下载并校验通过的二进制在 %s）。直接粘这一行：\n"+
			"  sudo install -m 0755 %s %s\n",
		target, tmpPath, shellQuote(tmpPath), shellQuote(target))
}

// shellQuote 把一个路径包成 shell 里安全的单参数（只在必要时加引号）。
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"\\$`&|;<>()*?[]{}~!#") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// restartAgentService 执行 `systemctl restart anotify-agent`（契约 §4.8）。
//
// 失败**不算升级失败**：二进制已经原子替换好了，重启只是让它立刻生效。
// 把退出码改成非 0 会让脚本以为「没升级成功」，于是去重装一遍——
// 一次正常的「这台机器没有 systemd」会被放大成一次重装。
func restartAgentService() error {
	out, err := exec.Command("systemctl", "restart", agentServiceName).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return fmt.Errorf("%w（systemctl 输出：%s）", err, msg)
		}
		return err
	}
	return nil
}
