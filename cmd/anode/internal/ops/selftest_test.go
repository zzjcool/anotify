package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
)

func TestUpgradeReportsExecutableVersionNotRequestedVersion(t *testing.T) {
	testSelfCheck(t, "#!/bin/sh\nprintf '%s\\n' '{\"version\":\"actual-v2\",\"commit\":\"actual-commit\"}'\n", true)
}

func TestUpgradeInvalidExecutablePreservesOldBinary(t *testing.T) {
	testSelfCheck(t, "this is not an executable", false)
}

func testSelfCheck(t *testing.T, binary string, valid bool) {
	t.Helper()
	sum := sha256.Sum256([]byte(binary))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer node-token" {
			t.Error("missing Node Bearer")
		}
		_, _ = w.Write([]byte(binary))
	}))
	defer srv.Close()
	target := filepath.Join(t.TempDir(), "anode")
	if err := os.WriteFile(target, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	a, _, _, restarted := testAgent(t, target)
	a.Upgrade.ServerURL = "ws" + strings.TrimPrefix(srv.URL, "http")
	a.Upgrade.NodeToken = "node-token"
	var ack *protocol.UpgradeAckFrame
	a.HandleUpgrade(context.Background(), &protocol.UpgradeFrame{Type: protocol.FrameUpgrade, TargetVersion: "untrusted-requested-version", SHA256: hex.EncodeToString(sum[:])}, func(f any) error { ack = f.(*protocol.UpgradeAckFrame); return nil })
	if ack == nil {
		t.Fatal("missing ACK")
	}
	if valid {
		if !ack.OK || ack.Version != "actual-v2" || !*restarted {
			t.Fatalf("not actual executable version: %+v restarted=%v", ack, *restarted)
		}
	} else {
		got, err := os.ReadFile(target)
		if err != nil || string(got) != "old" || ack.OK || *restarted {
			t.Fatalf("invalid executable replaced target: %q, %v, %+v", got, err, ack)
		}
	}
}
