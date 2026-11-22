package source

// webhook_sign.go 是 webhook 投递的签名口径（D7）。值 = `HMAC-SHA256(secret, 原始 body 字节)`
// 的小写十六进制；平台差异只到头名（`X-Anotify-Signature` / `X-Hub-Signature-256`）。校验对
// 原始字节做（解析后的 JSON 会因空白/键序假失败），缺失/格式错/不匹配 → 401 且不产生事件，
// 所以必须在落库与派发之前完成；比较走 `hmac.Equal`（逐字节短路 = 可爆破的预言机）。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

// HookSignatureHeader 是缺省口径（anotify-sha256）的头名（契约里逐字冻结）。
const HookSignatureHeader = "X-Anotify-Signature"

// HookSignatureGitHub 是 `config.signature.scheme: github-sha256` 的头名（算法同缺省口径）。
const HookSignatureGitHub = "X-Hub-Signature-256"

// HookSignatureScheme 是签名值的算法前缀（`sha256=<hex>`）。
//
// 只有这一种：不认识的算法一律按「不合法」拒，而不是回落到 sha256 或忽略前缀——
// 忽略前缀会让 `md5=...` 这种明显不是我们签的值看起来「校验通过」。
const HookSignatureScheme = "sha256="

// hookSignatureHexLen 是 hex 段的长度（sha256 = 32 字节 = 64 个 hex 字符）。
const hookSignatureHexLen = sha256.Size * 2

// ErrHookSignature 表示签名缺失 / 格式不合法 / 与 body 不匹配。
//
// 三种情况对外同码同文案（401，不区分）：区分它们只会告诉攻击者「你猜的格式对了」。
var ErrHookSignature = errors.New("source: webhook 签名缺失或不匹配")

// SignHookBody 计算契约口径的签名值（含 `sha256=` 前缀），供测试与调用方文档复用。
func SignHookBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return HookSignatureScheme + hex.EncodeToString(mac.Sum(nil))
}

// Scheme 返回该源的签名口径（github-sha256 / 缺省 anotify-sha256）。只有一处解释
// （sourcespec 的归一化形态）：handler 据此选头名，本包据此验签。
func HookScheme(src *store.Source) string {
	if src == nil || src.Kind != sourcespec.KindWebhook {
		return sourcespec.SchemeAnotifySHA256
	}
	return sourcespec.ParseConfig(src.Kind, src.Config).Scheme()
}

// VerifyHookSignature 校验一次投递的签名头。secret 为空 → 放行且**不看**头（契约：空 =
// 不校验，乱写的头不该让投递失败）。非空时任何一条不满足 → ErrHookSignature：头形如
// `sha256=<64 个小写 hex>`（前缀大小写、裸 hex、截断/超长、大写都拒），且 `hmac.Equal`。
func VerifyHookSignature(secret string, body []byte, header string) error {
	if secret == "" {
		return nil
	}
	value := strings.TrimSpace(header)
	if !strings.HasPrefix(value, HookSignatureScheme) {
		return ErrHookSignature
	}
	hexPart := value[len(HookSignatureScheme):]
	if len(hexPart) != hookSignatureHexLen || !isLowerHex(hexPart) {
		return ErrHookSignature
	}
	provided, err := hex.DecodeString(hexPart)
	if err != nil {
		return ErrHookSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), provided) {
		return ErrHookSignature
	}
	return nil
}

// isLowerHex 判断是否全为小写十六进制字符。不 `ToLower` 后放行：大小写都收会让轮换时的
// 手工排错变含糊（`sha256=5F1D…` 算不算同一把？）。
func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}
