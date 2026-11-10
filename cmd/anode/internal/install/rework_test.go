package install

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Run the actual Apply flow unprivileged so directory permission failures remain
// real even in root CI. No host services, sudo or inherited environment.
func TestReworkInstallPermissionHelper(t *testing.T) {
	if os.Getenv("ANODE_INSTALL_FAULT_HELPER") != "1" {
		return
	}
	o := Options{Prefix: os.Getenv("STAGE"), Source: os.Getenv("SOURCE"), Name: "node-a", Server: "ws://127.0.0.1:1", Token: "fixture-credential"}
	if err := Apply(o, io.Discard); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(17)
	}
}
func installUnprivileged(t *testing.T, o Options) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Go build directories are private to root; copy only the test executable
	// into this test-owned, traversable fixture rather than changing those paths.
	helper := filepath.Join(filepath.Dir(o.Source), "install-test-helper")
	executable, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helper, executable, 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, helper, "-test.run=^TestReworkInstallPermissionHelper$", "-test.timeout=4s")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "ANODE_INSTALL_FAULT_HELPER=1", "STAGE=" + o.Prefix, "SOURCE=" + o.Source}
	if os.Geteuid() == 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	}
	return cmd.CombinedOutput()
}
func TestReworkInstallPartialFailureCanSafelyRerun(t *testing.T) {
	for _, fault := range []string{"config", "unit", "unit-with-user-config"} {
		t.Run(fault, func(t *testing.T) {
			o := fixture(t)
			if err := o.defaults(); err != nil {
				t.Fatal(err)
			}
			// t.TempDir's parent is also owned by this test. Make only this safe
			// temporary tree traversable for the helper's dropped credentials.
			for _, p := range []string{filepath.Dir(filepath.Dir(o.Source)), filepath.Dir(o.Source)} {
				if err := os.Chmod(p, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(o.Source, 0755); err != nil {
				t.Fatal(err)
			}
			for _, p := range []string{filepath.Dir(o.BinPath), filepath.Dir(o.ConfigPath), filepath.Dir(o.UnitPath), o.DataPath} {
				if err := os.MkdirAll(p, 0777); err != nil {
					t.Fatal(err)
				}
			}
			// Every staging ancestor must be writable/traversable, not host paths.
			if err := filepath.WalkDir(o.Prefix, func(p string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					return os.Chmod(p, 0777)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var userConfig []byte
			if fault == "unit-with-user-config" {
				userConfig = []byte("name: node-a\nserver: ws://127.0.0.1:1\ntoken: fixture-credential\nnamespaces: [custom]\nvars: {USER: preserved}\n")
				if err := os.WriteFile(o.ConfigPath, userConfig, 0644); err != nil {
					t.Fatal(err)
				}
			}
			blocked := filepath.Dir(o.ConfigPath)
			if fault != "config" {
				blocked = filepath.Dir(o.UnitPath)
			}
			if err := os.Chmod(blocked, 0555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(blocked, 0777) })
			output, err := installUnprivileged(t, o)
			if err == nil || !strings.Contains(string(output), "create temporary file") || !strings.Contains(string(output), blocked) {
				t.Fatalf("did not hit real post-binary %s write fault: %v\n%s", fault, err, output)
			}
			t.Logf("actual Apply fault after binary write: %v\n%s", err, output)
			if err := os.Chmod(blocked, 0777); err != nil {
				t.Fatal(err)
			}
			output, err = installUnprivileged(t, o)
			if err != nil {
				t.Fatalf("fixed permissions must allow safe reinstall after partial %s failure: %v\n%s", fault, err, output)
			}
			for _, p := range []string{o.BinPath, o.ConfigPath, o.UnitPath} {
				if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
					t.Fatalf("rerun missing installed file %s: %v", p, err)
				}
			}
			if userConfig != nil {
				got, err := os.ReadFile(o.ConfigPath)
				if err != nil || !bytes.Equal(got, userConfig) {
					t.Fatalf("rollback deleted/changed preexisting config: %q %v", got, err)
				}
			}
			// Apply must never delete staging directories/ancestors on rollback.
			for _, p := range []string{o.Prefix, o.DataPath, blocked} {
				if st, err := os.Stat(p); err != nil || !st.IsDir() {
					t.Fatalf("rollback removed directory %s: %v", p, err)
				}
			}
		})
	}
}
