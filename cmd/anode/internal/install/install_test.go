package install

import (
	"bytes"
	"os"
	"path/filepath"
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
