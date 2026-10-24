package store

import (
	"strings"
	"testing"
	"time"
)

// TestBase62EncodeIsRealBase62 修复 scout R12：旧实现把每个字节拆成 c/62 与 c%62，
// 首字符只会落在 b62[0..4]（因为 c/62 ≤ 4），不是真 base62。
func TestBase62EncodeIsRealBase62(t *testing.T) {
	// 高位字节 0xFF → 旧实现首字符 b62[255/62]=b62[4]='4'，真 base62 应该是 '[' 之外的值。
	enc := base62Encode([]byte{0xFF, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06})
	if enc == "" {
		t.Fatal("base62Encode returned empty string")
	}
	for _, r := range enc {
		if !strings.ContainsRune(b62, r) {
			t.Fatalf("base62Encode produced non-base62 rune %q in %q", r, enc)
		}
	}
	// 旧实现的「每字节两字符」长度特征：8 字节会产出 16 字符。
	// 真 base62 编 64 bit 约 11 字符（log62(2^64) ≈ 10.7），明显更短。
	if len(enc) != 11 {
		t.Errorf("base62Encode(8B) length = %d, want 11 (real base62 of 64-bit value)", len(enc))
	}
}

// TestBase62EncodePreservesOrder 时间序：更大的数值得到字典序更大的编码。
func TestBase62EncodePreservesOrder(t *testing.T) {
	for _, pair := range [][2][]byte{
		{{0, 0, 0, 0, 0, 0, 0, 1}, {0, 0, 0, 0, 0, 0, 0, 2}},
		{{0, 0, 0, 0, 0, 0, 1, 0}, {0, 0, 0, 0, 0, 0, 2, 0}},
		{{1, 0, 0, 0, 0, 0, 0, 0}, {1, 0, 0, 0, 0, 0, 0, 1}},
	} {
		lo, hi := base62Encode(pair[0]), base62Encode(pair[1])
		if !(lo < hi) {
			t.Errorf("base62 order broken: %q should sort before %q", lo, hi)
		}
	}
}

// TestBase62EncodeZero 边界：全零字节不应产出空串。
func TestBase62EncodeZero(t *testing.T) {
	if got := base62Encode([]byte{0, 0}); got != "0" {
		t.Errorf("base62Encode([0 0]) = %q, want \"0\"", got)
	}
	if got := base62Encode(nil); got != "" {
		t.Errorf("base62Encode(nil) = %q, want empty", got)
	}
}

// TestNewIDPrefixAndUniqueness 生成器的前缀与唯一性。
func TestNewIDPrefixAndUniqueness(t *testing.T) {
	cases := []struct {
		prefix string
		gen    func() string
	}{
		{"usr_", NewUserID},
		{"dev_", NewDeviceID},
		{"sess_", NewSessionID},
		{"evt_", NewEventID},
		{"cas_", NewCliAuthID},
		{"rc_", NewRecoveryCodeID},
		{"org_", NewOrgID},
		{"nd_", NewNodeID},
		{"nins_", NewInstanceID},
		{"tsk_", NewTaskID},
	}
	for _, c := range cases {
		seen := make(map[string]bool, 200)
		for i := 0; i < 200; i++ {
			id := c.gen()
			if !strings.HasPrefix(id, c.prefix) {
				t.Fatalf("%s: id %q missing prefix %q", c.prefix, id, c.prefix)
			}
			if seen[id] {
				t.Fatalf("%s: duplicate id %q", c.prefix, id)
			}
			seen[id] = true
		}
	}
}

// TestNewIDTimeOrdered 同一前缀下，**跨毫秒**生成的 ID 必须字典序递增（可用于按 ID 排序）。
//
// 同一毫秒内的顺序由随机后缀决定，不保证递增——ID 的时间序粒度是毫秒，
// 这也是 KSUID 的语义（1ms 内多 ID 属于同一时间桶）。
func TestNewIDTimeOrdered(t *testing.T) {
	prev := NewTaskID()
	// 3 次 × 2ms 足以跨毫秒边界，总开销可忽略；
	// 排序性本身由 TestBase62EncodePreservesOrder 在用函数层直接验证。
	for i := 0; i < 3; i++ {
		time.Sleep(2 * time.Millisecond)
		cur := NewTaskID()
		if cur < prev {
			t.Fatalf("ids not time-ordered across ms: %q then %q", prev, cur)
		}
		prev = cur
	}
}

// TestCapabilityAndGraphIDShape cap_<UTC>_<hash8> / grf_<UTC>_<hash8>。
func TestCapabilityAndGraphIDShape(t *testing.T) {
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	capID := NewCapabilityID(hash)
	parts := strings.Split(capID, "_")
	if len(parts) != 3 {
		t.Fatalf("capability id %q should have 3 underscore-separated parts", capID)
	}
	if parts[0] != "cap" || parts[2] != hash[:8] {
		t.Fatalf("capability id %q malformed (want cap_<utc>_%s)", capID, hash[:8])
	}

	grfID := NewGraphVersionID(hash)
	parts = strings.Split(grfID, "_")
	if len(parts) != 3 || parts[0] != "grf" || parts[2] != hash[:8] {
		t.Fatalf("graph version id %q malformed", grfID)
	}
}

// TestIDHashSuffix 短 hash 不越界。
func TestIDHashSuffix(t *testing.T) {
	if got := idHashSuffix("abc"); got != "abc" {
		t.Errorf("idHashSuffix(\"abc\") = %q, want \"abc\"", got)
	}
	if got := idHashSuffix(""); got != "" {
		t.Errorf("idHashSuffix(\"\") = %q, want \"\"", got)
	}
	if got := idHashSuffix("0123456789"); got != "01234567" {
		t.Errorf("idHashSuffix long = %q, want 01234567", got)
	}
}

// TestItoa64 十进制转换。
func TestItoa64(t *testing.T) {
	cases := map[int64]string{0: "0", 1: "1", 9: "9", 10: "10", 1730000000: "1730000000"}
	for in, want := range cases {
		if got := itoa64(in); got != want {
			t.Errorf("itoa64(%d) = %q, want %q", in, got, want)
		}
	}
}
