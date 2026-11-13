// Package install is the single non-interactive installation source for anode.
// It never registers a Node, deploys capabilities, removes data or invokes sudo/systemctl.
package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zzjcool/anotify/cmd/anode/internal/config"
	"gopkg.in/yaml.v3"
)

type Options struct {
	Prefix, ConfigPath, BinPath, UnitPath, DataPath string
	Name, Server, Token, Source                     string
	UpdateToken                                     bool
	// UnitUser 是 unit 里的 User=（空 = 不输出，服务以启动者身份运行）。
	UnitUser string
	// UnitHome 是 unit 里的 Environment=HOME=（空 = 不输出；有值必须是安全的绝对路径）。
	UnitHome string
	// UnitPathEnv 是 unit 里的 Environment=PATH= 的逐项值（空 = 沿用记录 / 不输出；按给出顺序用 : 连接）。
	UnitPathEnv []string
	// PathSource / PathShell 只进 install.json：记录这条 PATH 是怎么来的（login-shell /
	// flags / fallback）以及抓取它的 shell，方便事后判断"为什么任务里没有某个命令"。
	PathSource string
	PathShell  string
}

// PathRecordVersion 是 install.json 的格式版本。
const PathRecordVersion = 1

// PathRecord 是"任务环境 PATH 从哪来"的落盘记录（与 anode.yaml 同目录的 install.json）。
//
// 存在的理由：重跑 `anode install` 必须渲染出**同一份 unit**（D8 重跑=升级，否则会被
// "existing unit is unknown/modified" 拒掉、节点停在停止态），而 PATH 又要跟着服务用户的
// 登录 shell 走。记录是这两件事的交点：
//
//	抓取只发生在"没有记录"或显式 --refresh-path；其余一律复用记录值 → unit 逐字节一致。
//
// 记录本身不含秘密（PATH 是公开信息），0644 即可。
//
// 为什么不让 `anode install` 自己去抓：系统安装要写 /etc，`anode install` 常以 root 运行，
// 此时它的登录 shell 是 root 的，抓出来的 PATH 对 User=<安装者> 的服务毫无意义 —— 抓取必须
// 由安装脚本以**服务用户**身份完成（scripts/install-anode.sh 的 capture_login_path）。
//
// 运行时不可能等于"你的终端"：终端 PATH 是（用户, shell, cwd, 目录级配置）的函数，而守护
// 进程没有 shell 启动过程。能对齐且应该对齐的是**安装那一刻的登录 shell**；direnv/`.nvmrc`/
// `mise.toml` 这类按目录切换的版本不会生效（能力要锁版本就写绝对路径或 `mise exec`）。
type PathRecord struct {
	Version    int       `json:"version"`
	User       string    `json:"user,omitempty"`
	Home       string    `json:"home,omitempty"`
	Shell      string    `json:"shell,omitempty"`
	Source     string    `json:"source,omitempty"`
	Path       string    `json:"path"`
	CapturedAt time.Time `json:"capturedAt"`
}

// Entries 把记录的 PATH 拆成逐项（空串/相对项在写入前就已被拒，这里只还原顺序）。
func (r *PathRecord) Entries() []string {
	if r == nil || strings.TrimSpace(r.Path) == "" {
		return nil
	}
	return strings.Split(r.Path, ":")
}

// RecordFile 返回记录文件路径：与 anode.yaml 同目录（--prefix staging 下也自动跟随）。
func RecordFile(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "install.json")
}

// LoadPathRecord 读记录；不存在返回 (nil, nil) —— "没有记录"是正常状态（首次安装/老版本）。
func LoadPathRecord(configPath string) (*PathRecord, error) {
	data, err := os.ReadFile(RecordFile(configPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", RecordFile(configPath), err)
	}
	var rec PathRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("parse %s: %w", RecordFile(configPath), err)
	}
	if rec.Version != PathRecordVersion {
		return nil, fmt.Errorf("unsupported %s version %d", RecordFile(configPath), rec.Version)
	}
	return &rec, nil
}

// unitUserPattern 是 systemd User= 允许的形状（也是常见 POSIX 用户名）。
var unitUserPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

func (o *Options) defaults() error {
	if err := o.validateUnitEnv(); err != nil {
		return err
	}
	if o.Prefix != "" {
		if err := safePath(o.Prefix); err != nil {
			return fmt.Errorf("prefix: %w", err)
		}
	}
	defaults := []string{"/etc/anode/anode.yaml", "/usr/local/bin/anode", "/etc/systemd/system/anode.service", "/var/lib/anode"}
	paths := []*string{&o.ConfigPath, &o.BinPath, &o.UnitPath, &o.DataPath}
	for i, p := range paths {
		if *p == "" {
			*p = defaults[i]
			if o.Prefix != "" {
				*p = filepath.Join(o.Prefix, defaults[i])
			}
		}
		if err := safePath(*p); err != nil {
			return err
		}
		if o.Prefix != "" {
			rel, err := filepath.Rel(o.Prefix, *p)
			if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
				return errors.New("path escapes staging prefix")
			}
		}
	}
	if filepath.Base(o.ConfigPath) != "anode.yaml" || filepath.Base(o.BinPath) != "anode" || filepath.Base(o.UnitPath) != "anode.service" {
		return errors.New("destinations must be named anode.yaml, anode, anode.service")
	}
	for i, a := range paths {
		for j, b := range paths {
			if i != j && (*a == *b || strings.HasPrefix(*a, *b+string(os.PathSeparator))) {
				return errors.New("installation destinations overlap")
			}
		}
	}
	return nil
}

// validateUnitEnv 校验 --unit-user / --unit-home / --unit-path。
//
// 这些值原样进 unit 文件（systemd 不做 shell 转义），所以只放行可证明安全的值；
// 非法值必须在**任何写盘之前**失败（defaults 是 Apply 的第一道门）。
func (o *Options) validateUnitEnv() error {
	if o.UnitUser != "" && !unitUserPattern.MatchString(o.UnitUser) {
		return fmt.Errorf("unit-user %q is not a valid user name (%s)", o.UnitUser, unitUserPattern.String())
	}
	if o.UnitHome != "" {
		if err := unitEnvPath("unit-home", o.UnitHome); err != nil {
			return err
		}
	}
	return validatePathEntries("unit-path", o.UnitPathEnv)
}

// validatePathEntries 逐个校验 PATH 项（也用于 `anode path add` 的追加目录）。
func validatePathEntries(flag string, entries []string) error {
	for _, entry := range entries {
		if err := unitEnvPath(flag, entry); err != nil {
			return err
		}
	}
	return nil
}

// legacyStablePath 是老版本安装器写进 unit 的固定集合（$HOME/.local/bin + 系统目录）。
// 仅用于"从老 unit 一次性迁移到记录制"的判定，不参与任何新写入。
func legacyStablePath(home string) string {
	const system = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	if home == "" {
		return system
	}
	return filepath.Join(home, ".local/bin") + ":" + system
}

// unitEnvPath 要求绝对、clean、非 / 且不含会破坏 unit 行的字符。
//
// 这里**不**走 safePath：home/PATH 目录允许是符号链接（如 /var/home），也不是本工具创建的。
func unitEnvPath(flag, value string) error {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value || value == "/" {
		return fmt.Errorf("%s %q: absolute clean non-root path required", flag, value)
	}
	if strings.ContainsAny(value, "\x00\r\n\"%$`") {
		return fmt.Errorf("%s %q: contains characters unsafe in a systemd unit (quote, %%, $, backtick, newline)", flag, value)
	}
	return nil
}

// safePath rejects symlinks and special files in every existing path component.
// Directories must be administrator-controlled: this is not a hostile-user sandbox.
func safePath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return fmt.Errorf("unsafe path %q: absolute clean non-root path required", path)
	}
	if strings.ContainsAny(path, "\x00\r\n\"%\\'$`") {
		return fmt.Errorf("unsafe path %q", path)
	}
	p := "/"
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		p = filepath.Join(p, part)
		st, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect %s: %w", p, err)
		}
		if st.Mode()&os.ModeSymlink != 0 || (!st.IsDir() && !st.Mode().IsRegular()) {
			return fmt.Errorf("unsafe symlink/special file %s", p)
		}
		if p != path && !st.IsDir() {
			return fmt.Errorf("parent is not a directory: %s", p)
		}
	}
	return nil
}

// EnsureDirectory 确保目录存在，**不存在时**按 mode 新建。
//
// 已存在的目录不改权限：那是管理员的选择（脚本侧的 0755 修复是显式的，见
// scripts/install-anode.sh 的 start_service）。
func EnsureDirectory(path string, mode os.FileMode) error {
	if err := safePath(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return fmt.Errorf("create %s (administrator permissions may be required): %w", path, err)
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("not a directory: %s", path)
	}
	return nil
}

// unitReplaceable 判定既有 unit 能不能被这次参数覆盖（守卫的全部放行条件都在这里）。
//
//   - 与本次渲染逐字节相同：无变化（重跑=升级），放行；
//   - 与"用 install.json 记录渲染"的 unit 相同：同一套安装、只是本次要改 PATH
//     （--refresh-path / anode path add），放行；
//   - 没有记录、但等于老版本固定集合渲染的 unit：从固定集合一次性迁移到记录制，放行。
//     之后记录就存在了，再改 PATH 必须走上一行，不会持续放宽。
//
// User=/HOME=/ExecStart 任何一项不一致都不满足以上条件 → 仍然拒绝（保护手改过的 unit）。
func unitReplaceable(o Options, oldUnit, expected []byte, record *PathRecord) bool {
	if bytes.Equal(oldUnit, expected) {
		return true
	}
	if record != nil {
		return bytes.Equal(oldUnit, unitWithPath(o, record.Entries()))
	}
	legacy := [][]string{{legacyStablePath(o.UnitHome)}}
	if o.UnitHome == "" {
		legacy = append(legacy, nil)
	}
	for _, entries := range legacy {
		if bytes.Equal(oldUnit, unitWithPath(o, entries)) {
			return true
		}
	}
	return false
}

func unit(o Options) []byte { return unitWithPath(o, o.UnitPathEnv) }

// unitWithPath 用给定的 PATH 项渲染 unit（apply / path add / 守卫比对共用同一模板）。
func unitWithPath(o Options, entries []string) []byte {
	var b strings.Builder
	b.WriteString("# Managed by anode install; do not replace with an unrelated service.\n[Unit]\nDescription=Anotify v4 node\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nType=simple\n")
	// 只在有值时输出对应行；值的合法性已由 defaults 保证（这里是纯模板）。
	if o.UnitUser != "" {
		fmt.Fprintf(&b, "User=%s\n", o.UnitUser)
	}
	if o.UnitHome != "" {
		fmt.Fprintf(&b, "Environment=HOME=%s\n", o.UnitHome)
	}
	if len(entries) > 0 {
		fmt.Fprintf(&b, "Environment=PATH=%s\n", strings.Join(entries, ":"))
	}
	fmt.Fprintf(&b, "ExecStart=\"%s\" serve --config \"%s\" --data \"%s\"\nRestart=always\nRestartSec=2\nUMask=0077\n\n[Install]\nWantedBy=multi-user.target\n", o.BinPath, o.ConfigPath, o.DataPath)
	return []byte(b.String())
}

// Apply performs all validation before any writes; each installed file is atomic.
// Existing strict local configuration is retained; a different token needs UpdateToken.
func Apply(o Options, out io.Writer) (retErr error) {
	if err := o.defaults(); err != nil {
		return err
	}
	if strings.TrimSpace(o.Name) == "" || strings.TrimSpace(o.Server) == "" || strings.TrimSpace(o.Token) == "" {
		return errors.New("existing Node name, server and token are required (no registration is performed)")
	}
	// nil slices marshal to [], which would incorrectly cancel the namespace default.
	cfg := &config.Config{Name: o.Name, Server: o.Server, Token: o.Token, Namespaces: []string{config.DefaultNamespace}}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if _, err := config.Parse(raw); err != nil {
		return fmt.Errorf("identity/configuration: %w", err)
	}
	// 有效的 PATH 优先级：--unit-path（显式）> install.json 记录 > 都没有（unit 不写 PATH 行）。
	// 复用记录是"重跑=升级"能逐字节一致的关键：路径不是每次从当前 shell 重新取。
	record, err := LoadPathRecord(o.ConfigPath)
	if err != nil {
		return err
	}
	entries := o.UnitPathEnv
	fromRecord := false
	if len(entries) == 0 && record != nil {
		entries = record.Entries()
		fromRecord = true
	}
	if err := validatePathEntries("unit-path", entries); err != nil {
		return err
	}
	expectedUnit := unitWithPath(o, entries)
	oldConfig, ce := os.ReadFile(o.ConfigPath)
	oldUnit, ue := os.ReadFile(o.UnitPath)
	if ce != nil && !errors.Is(ce, os.ErrNotExist) {
		return fmt.Errorf("read configuration: %w", ce)
	}
	if ue != nil && !errors.Is(ue, os.ErrNotExist) {
		return fmt.Errorf("read unit: %w", ue)
	}
	if ue == nil && !unitReplaceable(o, oldUnit, expectedUnit, record) {
		return errors.New("existing unit is unknown/modified; refusing to overwrite")
	}
	if ce == nil {
		saved, err := config.Parse(oldConfig)
		if err != nil {
			return errors.New("existing configuration is invalid/unknown; refusing to overwrite")
		}
		if saved.Name != o.Name || saved.Server != o.Server {
			return errors.New("existing Node identity/server differs; refusing to overwrite")
		}
		if saved.Token != o.Token && !o.UpdateToken {
			return errors.New("existing token differs; use --update-token explicitly to rotate only the token")
		}
		cfg = saved
		if saved.Token == o.Token {
			raw = oldConfig
		} else {
			cfg.Token = o.Token
			raw, err = yaml.Marshal(cfg)
			if err != nil {
				return err
			}
		}
	}
	// PATH 记录：显式给了 --unit-path 才重写（记录是"抓取结果"，不是每次重算的常量）；
	// 复用记录时不动 install.json —— capturedAt/source 保持原值，重跑本身也不产生变化。
	var recordBytes []byte
	if len(entries) > 0 && !fromRecord {
		source := o.PathSource
		if source == "" {
			source = "flags"
		}
		rec := PathRecord{
			Version: PathRecordVersion, User: o.UnitUser, Home: o.UnitHome, Shell: o.PathShell,
			Source: source, Path: strings.Join(entries, ":"), CapturedAt: time.Now().UTC(),
		}
		recordBytes, err = marshalPathRecord(rec)
		if err != nil {
			return err
		}
	}
	source, err := os.ReadFile(o.Source)
	if err != nil {
		return fmt.Errorf("read binary: %w", err)
	}
	st, err := os.Stat(o.Source)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode()&0111 == 0 {
		return errors.New("source must be an executable regular file")
	}
	if len(source) == 0 {
		return errors.New("source binary is empty")
	}
	if existing, err := os.Stat(o.BinPath); err == nil {
		if !existing.Mode().IsRegular() || ce != nil || ue != nil {
			return errors.New("existing binary is not an established anode installation; refusing overwrite")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// A config/unit leaf must not be a directory, even if it is empty.
	for _, p := range []string{o.ConfigPath, o.UnitPath, o.BinPath} {
		if st, err := os.Lstat(p); err == nil && !st.Mode().IsRegular() {
			return fmt.Errorf("not a regular file: %s", p)
		}
	}
	// 目录模式分两档（R3）：配置/unit/二进制的父目录必须让**非 root 的服务用户**能穿越
	// （配置文件的 0600 才是秘密边界）；数据目录是节点私有的，保持 0700。
	//
	// 历史上这里一律用 0700：/etc/anode 被 root 建成 0700，而安装器只 chown 了配置文件本身
	// → 以安装者身份跑的 systemd 服务连目录都进不去，读配置即 perm denied、exit 2，
	// 于是进入 Restart=always 的崩溃循环。
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{
		{filepath.Dir(o.ConfigPath), 0o755},
		{filepath.Dir(o.UnitPath), 0o755},
		{filepath.Dir(o.BinPath), 0o755},
		{o.DataPath, 0o700},
	} {
		if err := EnsureDirectory(d.path, d.mode); err != nil {
			return err
		}
	}
	// Roll back only leaves demonstrably created by this invocation. Never
	// claim an unknown partial install or remove preexisting files/directories.
	type createdFile struct {
		info os.FileInfo
		data []byte
	}
	created := make(map[string]createdFile)
	defer func() {
		if retErr == nil {
			return
		}
		for path, file := range created {
			current, err := os.Lstat(path)
			if err != nil || !current.Mode().IsRegular() || !os.SameFile(file.info, current) {
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, file.data) {
				continue
			}
			if err := os.Remove(path); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("rollback newly created %s: %w", path, err))
			}
		}
	}()
	files := []struct {
		path string
		data []byte
		mode os.FileMode
	}{{o.BinPath, source, 0755}, {o.ConfigPath, raw, 0600}, {o.UnitPath, expectedUnit, 0644}}
	if recordBytes != nil {
		files = append(files, struct {
			path string
			data []byte
			mode os.FileMode
		}{RecordFile(o.ConfigPath), recordBytes, 0644})
	}
	for _, file := range files {
		_, err := os.Lstat(file.path)
		newFile := errors.Is(err, os.ErrNotExist)
		if err := atomicFile(file.path, file.data, file.mode); err != nil {
			return err
		}
		if newFile {
			info, err := os.Lstat(file.path)
			if err != nil {
				return fmt.Errorf("inspect newly installed %s: %w", file.path, err)
			}
			created[file.path] = createdFile{info: info, data: file.data}
		}
	}
	fmt.Fprintf(out, "Installed anode (no registration, deployment or service start).\nConfiguration: %s (0600); data: %s\n", o.ConfigPath, o.DataPath)
	if len(entries) > 0 {
		fmt.Fprintf(out, "PATH record: %s (%s)\n", RecordFile(o.ConfigPath), strings.Join(entries, ":"))
	}
	fmt.Fprintln(out, "Administrator steps:")
	if o.UnitPath != "/etc/systemd/system/anode.service" {
		fmt.Fprintf(out, "install -m 0644 '%s' /etc/systemd/system/anode.service\n", o.UnitPath)
	}
	fmt.Fprintln(out, "systemctl daemon-reload\nsystemctl enable --now anode.service")
	if o.UpdateToken {
		fmt.Fprintln(out, "Explicit token update requested; local vars/namespaces/limits/envPassthrough preserved. Restart the service to use the new token.")
	}
	return nil
}

// AddUnitPath 往 install.json 记录的 PATH 末尾追加目录，并重写 unit 的 PATH 行。
//
// 用途：存量节点补工具链目录（例：`anode path add /home/u/.local/share/mise/shims`），
// 不必 --uninstall 重装（那会在升级窗口里让节点离线）。
//
// 只改那一条 `Environment=PATH=`，unit 的其它行逐字节保留 —— 所以不需要（也不接受）在这里
// 重新声明 User=/HOME=/ExecStart，更不会因为参数没带全就把它们抹掉。
//
// 守卫：unit 里的 PATH 必须与记录**完全一致**（不一致 = 手改过，拒绝覆盖）。
func AddUnitPath(o Options, dirs []string, out io.Writer) error {
	if len(dirs) == 0 {
		return errors.New("path add needs at least one directory")
	}
	if err := o.defaults(); err != nil {
		return err
	}
	if err := validatePathEntries("path add", dirs); err != nil {
		return err
	}
	record, err := LoadPathRecord(o.ConfigPath)
	if err != nil {
		return err
	}
	if record == nil {
		return fmt.Errorf("no PATH record at %s: install first, or pass explicit --unit-path to anode install", RecordFile(o.ConfigPath))
	}
	unit, err := os.ReadFile(o.UnitPath)
	if err != nil {
		return fmt.Errorf("read unit: %w", err)
	}
	value, line, ok := unitPathLine(unit)
	if !ok {
		return fmt.Errorf("%s 有零条或多条 Environment=PATH= 行；不猜，拒绝改写", o.UnitPath)
	}
	if value != record.Path {
		return errors.New("existing unit PATH differs from the record (hand-edited unit?); refusing to overwrite")
	}
	prev := record.Entries()
	next := append([]string(nil), prev...)
	for _, dir := range dirs {
		if !containsString(next, dir) {
			next = append(next, dir)
		}
	}
	if len(next) == len(prev) {
		fmt.Fprintf(out, "PATH unchanged: %s\n", strings.Join(prev, ":"))
		return nil
	}
	lines := strings.Split(string(unit), "\n")
	lines[line] = unitPathPrefix + strings.Join(next, ":")
	rec := *record
	rec.Path = strings.Join(next, ":")
	data, err := marshalPathRecord(rec)
	if err != nil {
		return err
	}
	if err := atomicFile(o.UnitPath, []byte(strings.Join(lines, "\n")), 0644); err != nil {
		return err
	}
	if err := atomicFile(RecordFile(o.ConfigPath), data, 0644); err != nil {
		return err
	}
	fmt.Fprintf(out, "PATH record: %s\n", rec.Path)
	fmt.Fprintln(out, "Administrator steps:\nsystemctl daemon-reload\nsystemctl restart anode.service")
	return nil
}

// unitPathPrefix 是 unit 里 PATH 基线的唯一形态（模板与 path add 共用）。
const unitPathPrefix = "Environment=PATH="

// unitPathLine 找唯一的一条 Environment=PATH=（行号 0 基）；零条或多条都算"不是我们写的"。
func unitPathLine(unit []byte) (value string, line int, ok bool) {
	line = -1
	for i, text := range strings.Split(string(unit), "\n") {
		if !strings.HasPrefix(text, unitPathPrefix) {
			continue
		}
		if ok {
			return "", 0, false
		}
		value, line, ok = strings.TrimPrefix(text, unitPathPrefix), i, true
	}
	return value, line, ok
}

func marshalPathRecord(rec PathRecord) ([]byte, error) {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode PATH record: %w", err)
	}
	return append(data, '\n'), nil
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// chmodTemporary is the file-permission operation (fault-injected by package
// tests); installation still executes the real atomic write flow.
var chmodTemporary = func(f *os.File, mode os.FileMode) error { return f.Chmod(mode) }

func atomicFile(path string, data []byte, mode os.FileMode) error {
	if err := safePath(path); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".anode-install-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = chmodTemporary(f, mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = f.Close()
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("atomic write %s (administrator permissions may be required): %w", path, err)
	}
	return nil
}
