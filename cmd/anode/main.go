// Command anode is the independent v4 node: serve, install and emit only.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zzjcool/anotify/cmd/anode/internal/app"
	"github.com/zzjcool/anotify/cmd/anode/internal/config"
	"github.com/zzjcool/anotify/cmd/anode/internal/hub"
	"github.com/zzjcool/anotify/cmd/anode/internal/install"
	"github.com/zzjcool/anotify/cmd/anode/internal/node"
	"github.com/zzjcool/anotify/internal/protocol"
)

// Set by the release builder; both hello and --version use exactly these facts.
var version = "dev"
var commit = "unknown"

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
func usage(w io.Writer) {
	fmt.Fprintln(w, "Usage: anode {serve|install|path|emit} [flags]\nanode --version\nUse anode <command> --help for flags.")
}
func run(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		usage(errOut)
		return 2
	}
	if len(args) == 1 && args[0] == "--version" {
		_ = json.NewEncoder(out).Encode(struct {
			Version string `json:"version"`
			Commit  string `json:"commit"`
		}{version, commit})
		return 0
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		usage(out)
		return 0
	}
	switch args[0] {
	case "emit":
		return emit(args[1:], out, errOut)
	case "serve":
		if err := serve(args[1:], out, errOut); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			// --check 走自己的前缀（anode check: <kind>: <detail>），不复用 serve 的。
			var fault *checkFault
			if errors.As(err, &fault) {
				fmt.Fprintf(errOut, "anode check: %s: %s\n", fault.kind, fault.detail)
				return 2
			}
			fmt.Fprintf(errOut, "anode serve: %v\n", err)
			return 2
		}
		return 0
	case "install":
		if err := installCLI(args[1:], out, errOut); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			fmt.Fprintf(errOut, "anode install: %v\n", err)
			return 2
		}
		return 0
	case "path":
		if err := pathCLI(args[1:], out, errOut); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			fmt.Fprintf(errOut, "anode path: %v\n", err)
			return 2
		}
		return 0
	default:
		fmt.Fprintf(errOut, "unknown command %q; expected serve, install or emit\n", args[0])
		return 2
	}
}
func serve(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(errOut)
	cfgPath := fs.String("config", "/etc/anode/anode.yaml", "strict anode.yaml path")
	dataDir := fs.String("data", "/var/lib/anode", "private data directory")
	check := fs.Bool("check", false, "validate node identity against the hub once, then exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("serve accepts flags only")
	}
	if *check {
		// 预检不写盘、不监听、不装配：只用到 config 与 dataDir（后者的工作目录路径作为事实上报）。
		return checkIdentity(*cfgPath, *dataDir, out)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	if err := install.EnsureDirectory(*dataDir, 0o700); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	if err := app.EnsureDirs(*dataDir); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	restart := make(chan struct{}, 1)
	// Capture the service's environment, NOT any task's run.env. Execution whitelist remains in runner.
	env := os.Environ()
	argv := append([]string(nil), os.Args...)
	logger := log.New(errOut, "anode: ", log.LstdFlags)
	err = app.Serve(ctx, app.Options{Config: cfg, DataDir: *dataDir, Version: version, Commit: commit, Logf: logger.Printf, Restart: func() {
		select {
		case restart <- struct{}{}:
		default:
		}
		cancel()
	}})
	if err != nil {
		return fmt.Errorf("node: %w", err)
	}
	select {
	case <-restart:
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("restart executable: %w", err)
		}
		if err := syscall.Exec(exe, argv, env); err != nil {
			return fmt.Errorf("restart (supervisor may retry): %w", err)
		}
	default:
	}
	return nil
}

// checkTimeout 是 `serve --check` 的固定上限（契约 §4：不新增 flag，脚本自己有外层 timeout）。
const checkTimeout = 10 * time.Second

// checkFault 是 `serve --check` 的诊断：kind 三选一 + 给人看的 detail。
//
// run() 用 errors.As 分流到自己的前缀（`anode check: <kind>: <detail>`），
// 不会出现 `anode serve: check: ...` 这种双前缀。
type checkFault struct {
	kind   string
	detail string
}

func (f *checkFault) Error() string { return f.kind + ": " + f.detail }

// checkIdentity 做一次性身份预检：严格加载 config → hello → welcome → 退出。
//
// 绝不写盘：不调用 install.EnsureDirectory / app.EnsureDirs / app.Assemble / app.Serve，
// 不监听、不启动 runner/emit 网关。stdout 只有一行 `check ok: node=<name>`（不含 token）。
func checkIdentity(cfgPath, dataDir string, out io.Writer) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return &checkFault{kind: "config", detail: err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	// hello 自己构造：事实与 app.Assemble 同源（workRoot = dataDir/work），但**不**读
	// 磁盘上的 applied/能力缓存（AppliedRevision/AppliedHash 留空 = 本机无 applied，中枢回全量 welcome）。
	hello := protocol.HelloFrame{
		Type:         protocol.FrameHello,
		Name:         cfg.Name,
		Token:        cfg.Token,
		Namespaces:   append([]string(nil), cfg.Namespaces...),
		LocalVars:    checkVars(cfg.Vars),
		LocalLimits:  cfg.LocalLimits(),
		AgentVersion: version,
		AgentCommit:  commit,
		Facts:        node.RuntimeFacts(filepath.Join(dataDir, "work"), ""),
		// 预检不探测能力/不申报槽位（那需要读本地缓存/账本）；空列表不是 null。
		Capabilities: []protocol.HelloCapability{},
		Instances:    []protocol.HelloInstance{},
	}
	if err := hub.Check(ctx, cfg.Server, cfg.Token, hello); err != nil {
		if errors.Is(err, hub.ErrAuthRejected) {
			return &checkFault{kind: "unauthorized", detail: err.Error()}
		}
		// 到期、连不上、被异常关闭：契约只冻结三种 kind（config/unauthorized/timeout），
		// 其余归入 timeout 诊断，detail 里带真实原因。
		return &checkFault{kind: "timeout", detail: err.Error()}
	}
	fmt.Fprintf(out, "check ok: node=%s\n", cfg.Name)
	return nil
}

// checkVars 拷贝本机 vars（hello 里的 localVars；空也送空对象而不是 null）。
func checkVars(vars map[string]string) map[string]string {
	out := make(map[string]string, len(vars))
	for k, v := range vars {
		out[k] = v
	}
	return out
}

func installCLI(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var o install.Options
	fs.StringVar(&o.Prefix, "prefix", "", "stage all defaults under an absolute non-root directory")
	fs.StringVar(&o.ConfigPath, "config", "", "anode.yaml destination")
	fs.StringVar(&o.BinPath, "bin", "", "anode binary destination")
	fs.StringVar(&o.UnitPath, "unit", "", "anode.service destination")
	fs.StringVar(&o.DataPath, "data", "", "private data directory")
	fs.StringVar(&o.Name, "name", "", "existing Node name (required)")
	fs.StringVar(&o.Server, "server", "", "ws(s) hub URL (required)")
	fs.StringVar(&o.Token, "token", "", "existing Node identity token (required; prefer --token-file)")
	tokenFile := fs.String("token-file", "", "read existing identity token from a protected file")
	fs.StringVar(&o.UnitUser, "unit-user", "", "run the service as this user (User= in the unit)")
	fs.StringVar(&o.UnitHome, "unit-home", "", "absolute HOME for the unit (Environment=HOME=)")
	fs.Func("unit-path", "absolute PATH entry for the unit (repeatable; joined with ':')", func(v string) error {
		o.UnitPathEnv = append(o.UnitPathEnv, v)
		return nil
	})
	fs.BoolVar(&o.UpdateToken, "update-token", false, "explicitly replace the saved token; preserve all local configuration")
	fs.StringVar(&o.PathSource, "path-source", "", "where the unit PATH came from (login-shell|flags|fallback); recorded in install.json")
	fs.StringVar(&o.PathShell, "path-shell", "", "shell that produced the unit PATH; recorded in install.json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("install accepts flags only")
	}
	if *tokenFile != "" {
		if o.Token != "" {
			return errors.New("use token OR token-file")
		}
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			return fmt.Errorf("token-file: %w", err)
		}
		o.Token = string(b)
		o.Token = trimToken(o.Token)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("binary source: %w", err)
	}
	o.Source = exe
	if err := install.Apply(o, out); err != nil {
		return fmt.Errorf("%w; have an administrator rerun anode install with these paths (no sudo is executed)", err)
	}
	return nil
}

// pathCLI 是 `anode path show|add`：查看/追加任务环境的 PATH 基线。
//
// PATH 的真相在 install.json（与 anode.yaml 同目录）：show 只读，add 追加目录并原子
// 重写 unit（守卫与 install 同口径）。刻意**不**在这里抓登录 shell：anode 常以 root
// 运行（写 /etc 需要），root 的登录 shell 不等于 User=<安装者> 的登录 shell —— 抓取由
// 安装脚本以服务用户身份完成（scripts/install-anode.sh 的 capture_login_path）。
func pathCLI(args []string, out, errOut io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		return pathUsage(out)
	}
	switch args[0] {
	case "show":
		return pathShow(args[1:], out, errOut)
	case "add":
		return pathAdd(args[1:], out, errOut)
	default:
		return fmt.Errorf("unknown subcommand %q; expected show or add", args[0])
	}
}

// pathUsage 打印 `anode path` 的用法：flag 列表来自**真的 FlagSet**（PrintDefaults），
// 不是手写文本 —— 手册 ↔ `-h` 的防漂移门禁（scripts/check_anode_skill.py）对账的就是它。
func pathUsage(out io.Writer) error {
	fs := flag.NewFlagSet("path", flag.ContinueOnError)
	fs.SetOutput(out)
	var o install.Options
	fs.StringVar(&o.ConfigPath, "config", "/etc/anode/anode.yaml", "strict anode.yaml path (the PATH record lives next to it)")
	fs.StringVar(&o.UnitPath, "unit", "", "anode.service path (add)")
	fs.StringVar(&o.Prefix, "prefix", "", "stage all defaults under an absolute non-root directory")
	fs.Bool("raw", false, "print only the recorded PATH string (show)")
	fmt.Fprintln(out, "Usage: anode path <show|add> [flags]")
	fmt.Fprintln(out, "  show          print the recorded PATH (which directories the node's tasks see)")
	fmt.Fprintln(out, "  add DIR...    append directories to the recorded PATH and rewrite the unit")
	fs.PrintDefaults()
	return flag.ErrHelp
}

func pathShow(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("path show", flag.ContinueOnError)
	fs.SetOutput(errOut)
	cfgPath := fs.String("config", "/etc/anode/anode.yaml", "strict anode.yaml path (the PATH record lives next to it)")
	raw := fs.Bool("raw", false, "print only the recorded PATH string (no labels)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("path show accepts flags only")
	}
	record, err := install.LoadPathRecord(*cfgPath)
	if err != nil {
		return err
	}
	if record == nil {
		if *raw {
			return nil // 没有记录 = 空输出（脚本用 -n 判断），不是错误
		}
		fmt.Fprintf(out, "no PATH record at %s\n", install.RecordFile(*cfgPath))
		return nil
	}
	if *raw {
		fmt.Fprintln(out, record.Path)
		return nil
	}
	fmt.Fprintf(out, "record: %s\n", install.RecordFile(*cfgPath))
	fmt.Fprintf(out, "path: %s\n", record.Path)
	fmt.Fprintf(out, "source: %s\n", emptyAs(record.Source, "unknown"))
	fmt.Fprintf(out, "shell: %s\n", emptyAs(record.Shell, "unknown"))
	fmt.Fprintf(out, "user: %s\n", emptyAs(record.User, "unknown"))
	fmt.Fprintf(out, "home: %s\n", emptyAs(record.Home, "unknown"))
	fmt.Fprintf(out, "captured: %s\n", record.CapturedAt.Format(time.RFC3339))
	return nil
}

func pathAdd(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("path add", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var o install.Options
	fs.StringVar(&o.Prefix, "prefix", "", "stage all defaults under an absolute non-root directory")
	fs.StringVar(&o.ConfigPath, "config", "", "anode.yaml path (the PATH record lives next to it)")
	fs.StringVar(&o.UnitPath, "unit", "", "anode.service path")
	fs.StringVar(&o.BinPath, "bin", "", "anode binary path")
	fs.StringVar(&o.DataPath, "data", "", "private data directory")
	fs.StringVar(&o.UnitUser, "unit-user", "", "run the service as this user (User= in the unit)")
	fs.StringVar(&o.UnitHome, "unit-home", "", "absolute HOME for the unit (Environment=HOME=)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("path add needs at least one directory")
	}
	for _, dir := range fs.Args() {
		if strings.HasPrefix(dir, "-") {
			return fmt.Errorf("flags must come before directories (got %q)", dir)
		}
	}
	return install.AddUnitPath(o, fs.Args(), out)
}

func emptyAs(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
