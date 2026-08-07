package auth

import (
	"testing"
	"time"
)

func TestDeviceNameFromUA(t *testing.T) {
	tests := []struct {
		ua   string
		want string
	}{
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36", "Chrome · macOS"},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1", "Safari · iOS"},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0", "Edge · Windows"},
		{"Mozilla/5.0 (Linux; Android 13) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36", "Chrome · Android"},
		{"Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0", "Firefox · Linux"},
		{"", "Browser · Unknown OS"},
	}
	for _, tt := range tests {
		got := DeviceNameFromUA(tt.ua)
		if got != tt.want {
			t.Errorf("DeviceNameFromUA(%q) = %q, want %q", tt.ua, got, tt.want)
		}
	}
}

func TestEnroll_CreateSession_StoresUserID(t *testing.T) {
	db, svc, uid := newEnrollTestDB(t)
	clock := &fakeClock{t: time.Unix(1000000, 0)}
	m := newEnrollMgrWithClock(db, svc, clock)

	created, err := m.CreateSession(uid, "my-iphone")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	got, _ := db.GetCliAuthSession(created.SessionID)
	if got.UserID != uid {
		t.Errorf("session.UserID = %q, want %q (发起者 user_id 应在建会话时存入)", got.UserID, uid)
	}
}
