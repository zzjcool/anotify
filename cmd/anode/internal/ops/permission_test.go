package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
)

func TestUpgradeDirectoryPermissionReportsNeedRoot(t *testing.T) {
	if os.Getenv("ANODE_PERMISSION_FIXTURE") == "child" {
		a := &Agent{Upgrade: UpgradeConfig{TargetPath: os.Getenv("ANODE_PERMISSION_TARGET"), ServerURL: os.Getenv("ANODE_PERMISSION_SERVER")}, Restart: func() { t.Fatal("permission failure restarted") }}
		ack := a.apply(context.Background(), &protocol.UpgradeFrame{Type: protocol.FrameUpgrade, SHA256: strings.Repeat("0", 64)})
		if ack.OK || ack.Error != protocol.UpgradeErrNeedRoot {
			t.Fatalf("permission denied must be need_root, not not_supported: %+v", ack)
		}
		return
	}
	root, err := os.MkdirTemp("", "anode-permission-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(root, "helper")
	if err := os.WriteFile(helper, b, 0755); err != nil {
		t.Fatal(err)
	}
	targetDir := filepath.Join(root, "readonly")
	if err := os.Mkdir(targetDir, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(targetDir, "anode")
	if err := os.WriteFile(target, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(targetDir, 0555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(targetDir, 0755)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("new")) }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, helper, "-test.run=^TestUpgradeDirectoryPermissionReportsNeedRoot$", "-test.v")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + root, "ANODE_PERMISSION_FIXTURE=child", "ANODE_PERMISSION_TARGET=" + target, "ANODE_PERMISSION_SERVER=ws" + strings.TrimPrefix(srv.URL, "http")}
	if os.Getuid() == 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("real permission fixture: %v\n%s", err, out)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "old" {
		t.Fatal("permission failure changed binary")
	}
	t.Logf("real read-only directory upgrade under non-root identity:\n%s", out)
}

func TestUpgradeRejectsNetworkPathAndCrossOriginRedirect(t *testing.T) {
	target := filepath.Join(t.TempDir(), "anode")
	if err := os.WriteFile(target, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	foreignCalls := 0
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreignCalls++; w.Write([]byte("evil")) }))
	defer foreign.Close()
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL+"/binary", http.StatusFound)
	}))
	defer home.Close()
	for _, path := range []string{"//" + strings.TrimPrefix(foreign.URL, "http://") + "/binary", "/redirect"} {
		a := &Agent{Upgrade: UpgradeConfig{TargetPath: target, ServerURL: "ws" + strings.TrimPrefix(home.URL, "http")}}
		sum := sha256.Sum256([]byte("evil"))
		ack := a.apply(context.Background(), &protocol.UpgradeFrame{Type: protocol.FrameUpgrade, URLPath: path, SHA256: hex.EncodeToString(sum[:])})
		if ack.OK || ack.Error != protocol.UpgradeErrDownloadFailed {
			t.Fatalf("cross-origin upgrade accepted: %+v", ack)
		}
	}
	if foreignCalls != 0 {
		t.Fatal("Node credential escaped to foreign origin")
	}
}
