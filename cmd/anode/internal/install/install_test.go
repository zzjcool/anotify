package install

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/cmd/anode/internal/config"
)

func fixture(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return Options{Prefix: filepath.Join(root, "stage"), Name: "node-a", Server: "ws://127.0.0.1:1", Token: "fixture-credential", Source: source}
}
func TestInstallSingleSourceReinstallPreservesLocalConfigAndData(t *testing.T) {
	o := fixture(t)
	var out bytes.Buffer
	if err := Apply(o, &out); err != nil {
		t.Fatal(err)
	}
	if err := o.defaults(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), o.Token) || !strings.Contains(out.String(), "systemctl daemon-reload") {
		t.Fatal("secret leaked or admin steps missing")
	}
	local := []byte("name: node-a\nserver: ws://127.0.0.1:1\ntoken: fixture-credential\nnamespaces: [production]\nvars: {LOCAL: value}\nlimits: {total: 3, perCapability: {echo: 2}}\nenvPassthrough: [API_KEY]\nlisten: tcp://127.0.0.1:0\n")
	if err := os.WriteFile(o.ConfigPath, local, 0600); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(o.DataPath, "user-data")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Apply(o, &out); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(o.ConfigPath)
	if err != nil || !bytes.Equal(got, local) {
		t.Fatalf("reinstall changed config: %s %v", got, err)
	}
	o.Token = "rotated-secret"
	if err := Apply(o, &out); err == nil || !strings.Contains(err.Error(), "--update-token") {
		t.Fatal("implicit token change accepted")
	}
	o.UpdateToken = true
	if err := Apply(o, &out); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(o.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != o.Token || cfg.Namespaces[0] != "production" || cfg.Vars["LOCAL"] != "value" || *cfg.Limits.Total != 3 || cfg.Limits.PerCapability["echo"] != 2 || cfg.EnvPassthrough[0] != "API_KEY" {
		t.Fatalf("local state lost: %+v", cfg)
	}
	got, err = os.ReadFile(sentinel)
	if err != nil || string(got) != "keep" {
		t.Fatal("user data changed")
	}
	for path, mode := range map[string]os.FileMode{o.ConfigPath: 0600, o.BinPath: 0755, o.UnitPath: 0644, o.DataPath: 0700} {
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != mode {
			t.Fatalf("%s: %v mode=%v", path, err, st)
		}
	}
}
func TestInstallConfigDirectoryIsTraversableByServiceUser(t *testing.T) {
	// R3 回归：配置目录若建成 0700（root），以安装者身份跑 systemd 服务时连目录都进不去
	// ——读 0600 配置即 perm denied、exit 2、Restart=always 崩溃循环，而安装器还报成功。
	// 秘密边界是**文件**的 0600；父目录必须可穿越。数据目录仍是节点私有的 0700（R3）。
	o := fixture(t)
	if err := o.defaults(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Apply(o, &out); err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Dir(o.ConfigPath)
	st, err := os.Stat(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o755 {
		t.Fatalf("配置目录 %s 权限 %#o，期望 0755（非 root 服务要能穿越）", cfgDir, perm)
	}
	dst, err := os.Stat(o.DataPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dst.Mode().Perm(); perm != 0o700 {
		t.Fatalf("数据目录 %s 权限 %#o，期望 0700（私有）", o.DataPath, perm)
	}
}

func TestInstallRejectsUnknownIdentityConfigurationAndUnit(t *testing.T) {
	for _, kind := range []string{"config", "unit", "binary", "identity"} {
		t.Run(kind, func(t *testing.T) {
			o := fixture(t)
			var out bytes.Buffer
			if err := o.defaults(); err != nil {
				t.Fatal(err)
			}
			if kind != "binary" {
				if err := Apply(o, &out); err != nil {
					t.Fatal(err)
				}
			}
			path := o.ConfigPath
			contents := "unknown: data\n"
			switch kind {
			case "unit":
				path = o.UnitPath
				contents = "unrelated service"
			case "binary":
				path = o.BinPath
				contents = "foreign binary"
			case "identity":
				o.Name = "other-node"
			}
			if kind != "identity" {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(path)
			if err := Apply(o, &out); err == nil {
				t.Fatal("unknown existing state accepted")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("unknown state overwritten")
			}
		})
	}
}
func TestInstallRejectsRootTraversalSymlinkSpecialAndPrefixEscape(t *testing.T) {
	for _, kind := range []string{"root", "traversal", "symlink", "special", "escape", "overlap"} {
		t.Run(kind, func(t *testing.T) {
			o := fixture(t)
			switch kind {
			case "root":
				o.Prefix = "/"
			case "traversal":
				o.Prefix = o.Prefix + "/../escape"
			case "symlink":
				if err := os.Symlink(t.TempDir(), o.Prefix); err != nil {
					t.Fatal(err)
				}
			case "special":
				o.ConfigPath = "/dev/null"
			case "escape":
				o.ConfigPath = filepath.Join(t.TempDir(), "anode.yaml")
			case "overlap":
				o.DataPath = filepath.Join(o.Prefix, "etc/anode/anode.yaml")
			}
			if err := Apply(o, &bytes.Buffer{}); err == nil {
				t.Fatal("unsafe install accepted")
			}
		})
	}
}
func TestInstallMissingIdentityDoesNotWrite(t *testing.T) {
	for _, field := range []string{"name", "server", "token"} {
		o := fixture(t)
		switch field {
		case "name":
			o.Name = ""
		case "server":
			o.Server = ""
		case "token":
			o.Token = ""
		}
		if err := Apply(o, &bytes.Buffer{}); err == nil {
			t.Fatal("missing identity accepted")
		}
		if _, err := os.Stat(o.Prefix); !os.IsNotExist(err) {
			t.Fatal("failed install wrote files")
		}
	}
}

// readUnit 落盘后读回 unit 文件内容。
func readUnit(t *testing.T, o Options) string {
	t.Helper()
	data, err := os.ReadFile(o.UnitPath)
	if err != nil {
		t.Fatalf("read unit %s: %v", o.UnitPath, err)
	}
	return string(data)
}

func TestInstallUnitUserHomeAndPathTemplate(t *testing.T) {
	o := fixture(t)
	o.UnitUser = "node-runner"
	o.UnitHome = "/home/node-runner"
	o.UnitPathEnv = []string{"/opt/node/bin", "/usr/local/bin", "/usr/bin"}
	var out bytes.Buffer
	if err := Apply(o, &out); err != nil {
		t.Fatal(err)
	}
	if err := o.defaults(); err != nil {
		t.Fatal(err)
	}
	got := readUnit(t, o)
	// 三行都紧跟 Type=simple，且 PATH 用 : 按给出顺序连接。
	want := "Type=simple\nUser=node-runner\nEnvironment=HOME=/home/node-runner\nEnvironment=PATH=/opt/node/bin:/usr/local/bin:/usr/bin\nExecStart="
	if !strings.Contains(got, want) {
		t.Fatalf("unit 模板不含预期行:\nwant substring: %q\ngot:\n%s", want, got)
	}
	if strings.Contains(out.String(), o.Token) {
		t.Fatal("token leaked to stdout")
	}

	// 没设三个 flag：一个都不输出（unit 与加上之前的形状一致）。
	bare := fixture(t)
	if err := Apply(bare, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := bare.defaults(); err != nil {
		t.Fatal(err)
	}
	plain := readUnit(t, bare)
	for _, line := range []string{"User=", "Environment=HOME=", "Environment=PATH="} {
		if strings.Contains(plain, line) {
			t.Fatalf("未设 flag 却输出了 %q:\n%s", line, plain)
		}
	}
	if !strings.Contains(plain, "Type=simple\nExecStart=") {
		t.Fatalf("无 flag 时 unit 形状变了:\n%s", plain)
	}
}

func TestInstallRejectsInvalidUnitEnvFlags(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Options)
		flag   string
		value  string
	}{
		{"user-uppercase", func(o *Options) { o.UnitUser = "Node" }, "unit-user", "Node"},
		{"user-leading-digit", func(o *Options) { o.UnitUser = "1node" }, "unit-user", "1node"},
		{"user-space", func(o *Options) { o.UnitUser = "node runner" }, "unit-user", "node runner"},
		{"user-shell-injection", func(o *Options) { o.UnitUser = "root;" }, "unit-user", "root;"},
		{"user-too-long", func(o *Options) { o.UnitUser = strings.Repeat("a", 33) }, "unit-user", strings.Repeat("a", 33)},
		{"home-relative", func(o *Options) { o.UnitHome = "home/node" }, "unit-home", "home/node"},
		{"home-root", func(o *Options) { o.UnitHome = "/" }, "unit-home", "/"},
		{"home-unclean", func(o *Options) { o.UnitHome = "/home/../root" }, "unit-home", "/home/../root"},
		{"home-dollar", func(o *Options) { o.UnitHome = "/home/$USER" }, "unit-home", "/home/$USER"},
		{"home-newline", func(o *Options) { o.UnitHome = "/home/x\nUser=root" }, "unit-home", "/home/x\nUser=root"},
		{"path-percent", func(o *Options) { o.UnitPathEnv = []string{"/usr/bin/%X"} }, "unit-path", "/usr/bin/%X"},
		{"path-dollar", func(o *Options) { o.UnitPathEnv = []string{"/usr/bin", "/opt/$USER/bin"} }, "unit-path", "/opt/$USER/bin"},
		{"path-backtick", func(o *Options) { o.UnitPathEnv = []string{"/usr/bin/`id`"} }, "unit-path", "/usr/bin/`id`"},
		{"path-relative", func(o *Options) { o.UnitPathEnv = []string{"usr/bin"} }, "unit-path", "usr/bin"},
		{"path-root", func(o *Options) { o.UnitPathEnv = []string{"/"} }, "unit-path", "/"},
		{"path-unclean", func(o *Options) { o.UnitPathEnv = []string{"/usr//bin"} }, "unit-path", "/usr//bin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := fixture(t)
			c.mutate(&o)
			err := Apply(o, &bytes.Buffer{})
			if err == nil {
				t.Fatalf("非法 %s %q 被接受", c.flag, c.value)
			}
			// 错误必须指出哪个 flag 的哪个值。
			if !strings.Contains(err.Error(), c.flag) || !strings.Contains(err.Error(), strconv.Quote(c.value)) {
				t.Fatalf("错误信息没点名 %s %q: %v", c.flag, c.value, err)
			}
			// 校验在任何写盘之前：staging 目录一个字节都不该出现。
			if _, statErr := os.Stat(o.Prefix); !os.IsNotExist(statErr) {
				t.Fatal("非法 flag 仍写了盘")
			}
		})
	}
}

func TestInstallUnitEnvRerunByteEqualAndModifiedUnitRefused(t *testing.T) {
	o := fixture(t)
	o.UnitUser = "node-runner"
	o.UnitHome = "/home/node-runner"
	o.UnitPathEnv = []string{"/usr/local/bin", "/usr/bin"}
	if err := Apply(o, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := o.defaults(); err != nil {
		t.Fatal(err)
	}
	first := readUnit(t, o)

	// 同一组 flag 重跑：unit 必须 byte-equal 且成功（不触发"unknown/modified"）。
	if err := Apply(o, &bytes.Buffer{}); err != nil {
		t.Fatalf("同参数重跑失败: %v", err)
	}
	if second := readUnit(t, o); second != first {
		t.Fatalf("同参数重跑 unit 变了:\nfirst:\n%s\nsecond:\n%s", first, second)
	}

	// 改 unit-user 重跑：模板不同 → 命中既有的"未知/被改过"拒绝。
	o.UnitUser = "other-runner"
	err := Apply(o, &bytes.Buffer{})
	if err == nil {
		t.Fatal("改了 unit-user 却覆盖了既有 unit")
	}
	if !strings.Contains(err.Error(), "existing unit is unknown/modified") {
		t.Fatalf("错误信息不是既有的拒绝文案: %v", err)
	}
	if after := readUnit(t, o); after != first {
		t.Fatal("被拒绝的安装改了 unit 文件")
	}
}
