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

func TestInstallRecordsPathAndReusesItOnRerun(t *testing.T) {
	o := fixture(t)
	o.UnitUser, o.UnitHome = "node-runner", "/home/node-runner"
	o.UnitPathEnv = []string{"/home/node-runner/.local/share/mise/shims", "/usr/local/bin", "/usr/bin"}
	o.PathSource, o.PathShell = "login-shell", "/bin/bash"
	if err := Apply(o, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := o.defaults(); err != nil {
		t.Fatal(err)
	}
	first := readUnit(t, o)
	record, err := LoadPathRecord(o.ConfigPath)
	if err != nil || record == nil {
		t.Fatalf("记录没落盘: record=%v err=%v", record, err)
	}
	if record.Path != strings.Join(o.UnitPathEnv, ":") || record.Source != "login-shell" || record.Shell != "/bin/bash" || record.User != "node-runner" {
		t.Fatalf("记录内容不对: %+v", record)
	}
	if st, err := os.Stat(RecordFile(o.ConfigPath)); err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("install.json 权限/存在性: %v %v", st, err)
	}

	// 不带 --unit-path 重跑：沿用记录（unit 逐字节一致，记录也不被重写）。
	bare := o
	bare.UnitPathEnv = nil
	if err := Apply(bare, &bytes.Buffer{}); err != nil {
		t.Fatalf("复用记录重跑失败: %v", err)
	}
	if got := readUnit(t, o); got != first {
		t.Fatalf("复用记录重跑改了 unit:\nfirst:\n%s\ngot:\n%s", first, got)
	}
	again, err := LoadPathRecord(o.ConfigPath)
	if err != nil || again == nil {
		t.Fatal(err)
	}
	if !again.CapturedAt.Equal(record.CapturedAt) || again.Source != record.Source {
		t.Fatalf("重跑改写了记录的来源/抓取时间: %+v → %+v", record, again)
	}
}

func TestInstallMigratesLegacyStablePathUnitOnce(t *testing.T) {
	o := fixture(t)
	o.UnitUser, o.UnitHome = "node-runner", "/home/node-runner"
	// 复刻老版本安装器的落盘结果：PATH = $HOME/.local/bin + 系统目录，且没有 install.json。
	o.UnitPathEnv = []string{legacyStablePath(o.UnitHome)}
	if err := Apply(o, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := o.defaults(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(RecordFile(o.ConfigPath)); err != nil {
		t.Fatal(err)
	}

	o.UnitPathEnv = []string{"/home/node-runner/.local/share/mise/shims", "/usr/local/bin", "/usr/bin"}
	o.PathSource = "login-shell"
	if err := Apply(o, &bytes.Buffer{}); err != nil {
		t.Fatalf("从固定集合迁移到记录制失败: %v", err)
	}
	want := "Environment=PATH=" + strings.Join(o.UnitPathEnv, ":") + "\n"
	if got := readUnit(t, o); !strings.Contains(got, want) {
		t.Fatalf("迁移后 unit 没写新 PATH:\nwant %q\ngot:\n%s", want, got)
	}
	if record, err := LoadPathRecord(o.ConfigPath); err != nil || record == nil {
		t.Fatalf("迁移没写记录: %v %v", record, err)
	}
}

func TestInstallRefusesHandEditedUnitEvenWithRecord(t *testing.T) {
	o := fixture(t)
	o.UnitUser = "node-runner"
	o.UnitPathEnv = []string{"/usr/local/bin", "/usr/bin"}
	if err := Apply(o, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := o.defaults(); err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(readUnit(t, o), "Restart=always", "Restart=always\nEnvironment=EXTRA=1", 1)
	if err := os.WriteFile(o.UnitPath, []byte(edited), 0644); err != nil {
		t.Fatal(err)
	}
	o.UnitPathEnv = []string{"/usr/local/bin", "/usr/bin", "/opt/x/bin"}
	err := Apply(o, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "existing unit is unknown/modified") {
		t.Fatalf("手改过的 unit 被覆盖了: %v", err)
	}
	if got := readUnit(t, o); got != edited {
		t.Fatal("被拒绝的安装改了 unit 文件")
	}
}

func TestAddUnitPathRewritesUnitAndRecord(t *testing.T) {
	o := fixture(t)
	o.UnitUser, o.UnitHome = "node-runner", "/home/node-runner"
	o.UnitPathEnv = []string{"/usr/local/bin", "/usr/bin"}
	if err := Apply(o, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := o.defaults(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := AddUnitPath(o, []string{"/opt/toolchain/bin"}, &out); err != nil {
		t.Fatal(err)
	}
	want := "/usr/local/bin:/usr/bin:/opt/toolchain/bin"
	if got := readUnit(t, o); !strings.Contains(got, "Environment=PATH="+want+"\n") {
		t.Fatalf("path add 没重写 unit:\n%s", got)
	}
	record, err := LoadPathRecord(o.ConfigPath)
	if err != nil || record == nil || record.Path != want {
		t.Fatalf("path add 没更新记录: %+v %v", record, err)
	}
	// 幂等：同一目录再 add 一次不改任何文件。
	before := readUnit(t, o)
	if err := AddUnitPath(o, []string{"/opt/toolchain/bin"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if readUnit(t, o) != before {
		t.Fatal("重复 add 改了 unit")
	}
	// 相对目录：形状校验在写盘之前拒绝。
	if err := AddUnitPath(o, []string{"opt/bin"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "path add") {
		t.Fatalf("相对目录被接受: %v", err)
	}
	// 手改过的 PATH 行（与记录不一致）：拒绝覆盖。注释/其它行的手工改动不影响（只改 PATH 行）。
	if err := os.WriteFile(o.UnitPath, []byte(strings.Replace(before, ":"+"/opt/toolchain/bin", ":/opt/hand-edited", 1)+"# hand edit\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := AddUnitPath(o, []string{"/opt/y/bin"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "differs from the record") {
		t.Fatalf("手改过的 PATH 行被 path add 覆盖了: %v", err)
	}
	// 没有 PATH 行：不猜，拒绝。
	noPath := "Type=simple\nExecStart=/usr/local/bin/anode serve\n"
	if err := os.WriteFile(o.UnitPath, []byte(noPath), 0644); err != nil {
		t.Fatal(err)
	}
	if err := AddUnitPath(o, []string{"/opt/y/bin"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "Environment=PATH=") {
		t.Fatalf("没有 PATH 行的 unit 被改写了: %v", err)
	}
	// 没有记录：要求先 install，而不是凭空造一条 PATH。
	if err := os.WriteFile(o.UnitPath, []byte(before), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(RecordFile(o.ConfigPath)); err != nil {
		t.Fatal(err)
	}
	if err := AddUnitPath(o, []string{"/opt/z/bin"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "no PATH record") {
		t.Fatalf("没有记录时 path add 没报错: %v", err)
	}
}
