package source

// webhook_sign_test.go 是签名口径的**单元**判别性用例（source.md §3.3 / D7）。
//
// 与 internal/server 的端到端用例分工：
//   - 这里钉「格式判据」——哪一种 hex / 前缀 / 长度算合法，哪一种必须拒；
//   - 服务端用例钉「传输面后果」——401 + 不落任何行。
//
// 期望值用**测试自己的** HMAC 实现算（不调 SignHookBody）：实现若改口径（换算法、
// 换编码、对解析后的 JSON 签名），这里的用例必须跟着红。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// signFor 是测试侧的契约口径签名（hex(HMAC-SHA256(secret, body))，带 `sha256=` 前缀）。
func signFor(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyHookSignature(t *testing.T) {
	const secret = "s3cr3t"
	body := []byte("{\n  \"type\": \"ping\"\n}")
	good := signFor(secret, body)
	rawHex := strings.TrimPrefix(good, "sha256=")

	ok := []struct {
		why    string
		secret string
		body   []byte
		header string
	}{
		{"合法签名", secret, body, good},
		{"合法签名（头两侧空白由 HTTP 层归一）", secret, body, "  " + good + "  "},
		{"没有 secret 的源不校验（乱写的头也放行）", "", body, "nonsense"},
		{"没有 secret 的源不校验（连头都没有）", "", body, ""},
	}
	for _, tc := range ok {
		if err := VerifyHookSignature(tc.secret, tc.body, tc.header); err != nil {
			t.Fatalf("%s：期望通过，得到 %v", tc.why, err)
		}
	}

	bad := []struct {
		why    string
		body   []byte
		header string
	}{
		{"缺签名头", body, ""},
		{"只有前缀", body, "sha256="},
		{"裸 hex 没有前缀", body, rawHex},
		{"前缀大小写错", body, "SHA256=" + rawHex},
		{"前缀多一个空格", body, "sha256 = " + rawHex},
		{"其它算法", body, "sha512=" + rawHex},
		{"hex 截断", body, good[:len(good)-1]},
		{"hex 超长", body, good + "0"},
		{"hex 大写", body, "sha256=" + strings.ToUpper(rawHex)},
		{"hex 里有非 hex 字符", body, "sha256=" + strings.Repeat("z", len(rawHex))},
		{"签名对应另一份字节（多了尾随换行）", append(append([]byte{}, body...), '\n'), good},
		{"签名用了另一把 secret", body, signFor("other-secret", body)},
	}
	for _, tc := range bad {
		err := VerifyHookSignature(secret, tc.body, tc.header)
		if !errors.Is(err, ErrHookSignature) {
			t.Fatalf("%s：期望 ErrHookSignature，得到 %v", tc.why, err)
		}
	}
}

// TestSignHookBodyMatchesContract 钉住 SignHookBody 的输出形状：
// `sha256=` + 64 个小写 hex，且等于测试侧的独立实现（口径只有一处解释）。
func TestSignHookBodyMatchesContract(t *testing.T) {
	const secret = "rotate-me"
	body := []byte(`{"id":"evt-1"}`)
	got := SignHookBody(secret, body)
	if want := signFor(secret, body); got != want {
		t.Fatalf("SignHookBody = %q，want %q", got, want)
	}
	if !strings.HasPrefix(got, HookSignatureScheme) {
		t.Fatalf("SignHookBody 必须带 %q 前缀：%q", HookSignatureScheme, got)
	}
	if hexPart := strings.TrimPrefix(got, HookSignatureScheme); len(hexPart) != hookSignatureHexLen || !isLowerHex(hexPart) {
		t.Fatalf("SignHookBody 的 hex 段必须是 %d 个小写 hex：%q", hookSignatureHexLen, hexPart)
	}
	if HookSignatureHeader != "X-Anotify-Signature" {
		t.Fatalf("签名头名字是契约里逐字冻结的：%q", HookSignatureHeader)
	}
}
