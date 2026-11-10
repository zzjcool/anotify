package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func put(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}
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
func TestBootstrapFakeCurlSingleSourceReinstallAndFailure(t *testing.T) {
	root := t.TempDir()
	fakeDir := filepath.Join(root, "bin")
	payload := filepath.Join(root, "payload")
	sumPath := filepath.Join(root, "sum")
	logPath := filepath.Join(root, "invocations")
	put(t, payload, "#!/bin/sh\nset -eu\nif [ \"$1\" = --version ]; then echo '{\"version\":\"fixture-v1\",\"commit\":\"fixture\"}'; exit 0; fi\n[ \"$1\" = install ] || exit 99\nprintf '%s\\n' \"$@\" >> \"$FIXTURE_LOG\"\nexit \"${FIXTURE_EXIT:-0}\"\n", 0700)
	b, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	put(t, sumPath, hex.EncodeToString(sum[:])+"  binary\n", 0600)
	// This fake curl actually copies payload/checksum to the wrapper's requested output.
	put(t, filepath.Join(fakeDir, "curl"), "#!/bin/sh\nset -eu\nout=\nwhile [ \"$#\" -gt 0 ]; do case \"$1\" in --output) out=$2; shift 2;; --connect-timeout|--max-time) shift 2;; --fail|--location) shift;; *) url=$1; shift;; esac; done\ncase \"$url\" in fixture-binary) cp \"$FIXTURE_PAYLOAD\" \"$out\";; fixture-sum) cp \"$FIXTURE_SUM\" \"$out\";; *) exit 90;; esac\n", 0700)
	script, err := filepath.Abs("../../scripts/install-anode.sh")
	if err != nil {
		t.Fatal(err)
	}
	run := func(exit string) (int, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/sh", script, "--version", "fixture-v1", "--arch", "amd64", "--download-url", "fixture-binary", "--checksum-url", "fixture-sum", "--", "--prefix", filepath.Join(root, "stage"), "--name", "node-a", "--server", "ws://127.0.0.1:1", "--token", "fixture-token")
		cmd.Env = append(os.Environ(), "PATH="+fakeDir+":/usr/bin:/bin", "FIXTURE_PAYLOAD="+payload, "FIXTURE_SUM="+sumPath, "FIXTURE_LOG="+logPath, "FIXTURE_EXIT="+exit)
		b, err := cmd.CombinedOutput()
		if err == nil {
			return 0, string(b)
		}
		if e, ok := err.(*exec.ExitError); ok {
			return e.ExitCode(), string(b)
		}
		t.Fatal(err)
		return -1, ""
	}
	for i := 0; i < 2; i++ {
		code, out := run("0")
		if code != 0 {
			t.Fatalf("bootstrap reinstall %d: %d %s", i, code, out)
		}
	}
	code, out := run("7")
	if code != 7 {
		t.Fatalf("install failure not propagated: %d %s", code, out)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(log), "install\n") != 3 || strings.Count(string(log), "--prefix\n") != 3 || strings.Count(string(log), "fixture-token\n") != 3 {
		t.Fatalf("single-source forwarding: %s", log)
	}
	put(t, sumPath, strings.Repeat("0", 64)+"\n", 0600)
	code, out = run("0")
	if code == 0 {
		t.Fatalf("bad checksum accepted: %s", out)
	}
	after, _ := os.ReadFile(logPath)
	if string(after) != string(log) {
		t.Fatal("bad checksum invoked installer")
	}
	// Wrapper must never contain a second YAML/service installation source.
	raw, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"sudo ", "systemctl ", "anode.yaml", "[Service]", "read -p"} {
		if strings.Contains(string(raw), bad) {
			t.Fatal(fmt.Sprintf("duplicate/interactive installer: %s", bad))
		}
	}
}
