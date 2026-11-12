// Package install is the single non-interactive installation source for anode.
// It never registers a Node, deploys capabilities, removes data or invokes sudo/systemctl.
package install

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

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
	// UnitPathEnv 是 unit 里的 Environment=PATH= 的逐项值（空 = 不输出；按给出顺序用 : 连接）。
	UnitPathEnv []string
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
	for _, entry := range o.UnitPathEnv {
		if err := unitEnvPath("unit-path", entry); err != nil {
			return err
		}
	}
	return nil
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

func EnsureDirectory(path string) error {
	if err := safePath(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
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

func unit(o Options) []byte {
	var b strings.Builder
	b.WriteString("# Managed by anode install; do not replace with an unrelated service.\n[Unit]\nDescription=Anotify v4 node\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nType=simple\n")
	// 只在有值时输出对应行；值的合法性已由 defaults 保证（这里是纯模板）。
	if o.UnitUser != "" {
		fmt.Fprintf(&b, "User=%s\n", o.UnitUser)
	}
	if o.UnitHome != "" {
		fmt.Fprintf(&b, "Environment=HOME=%s\n", o.UnitHome)
	}
	if len(o.UnitPathEnv) > 0 {
		fmt.Fprintf(&b, "Environment=PATH=%s\n", strings.Join(o.UnitPathEnv, ":"))
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
	expectedUnit := unit(o)
	oldConfig, ce := os.ReadFile(o.ConfigPath)
	oldUnit, ue := os.ReadFile(o.UnitPath)
	if ce != nil && !errors.Is(ce, os.ErrNotExist) {
		return fmt.Errorf("read configuration: %w", ce)
	}
	if ue != nil && !errors.Is(ue, os.ErrNotExist) {
		return fmt.Errorf("read unit: %w", ue)
	}
	if ue == nil && !bytes.Equal(oldUnit, expectedUnit) {
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
	for _, p := range []string{filepath.Dir(o.ConfigPath), filepath.Dir(o.UnitPath), filepath.Dir(o.BinPath), o.DataPath} {
		if err := EnsureDirectory(p); err != nil {
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
	for _, file := range []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{o.BinPath, source, 0755}, {o.ConfigPath, raw, 0600}, {o.UnitPath, expectedUnit, 0644},
	} {
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
	fmt.Fprintf(out, "Installed anode (no registration, deployment or service start).\nConfiguration: %s (0600); data: %s\nAdministrator steps:\n", o.ConfigPath, o.DataPath)
	if o.UnitPath != "/etc/systemd/system/anode.service" {
		fmt.Fprintf(out, "install -m 0644 '%s' /etc/systemd/system/anode.service\n", o.UnitPath)
	}
	fmt.Fprintln(out, "systemctl daemon-reload\nsystemctl enable --now anode.service")
	if o.UpdateToken {
		fmt.Fprintln(out, "Explicit token update requested; local vars/namespaces/limits/envPassthrough preserved. Restart the service to use the new token.")
	}
	return nil
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
