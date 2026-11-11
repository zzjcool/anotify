package source

// webhook_sign.go 是 webhook 投递的**签名口径**（source.md §3.3 / D7）。
//
// 契约：配了 `config.secret` 的源，投递必须带
//
//	X-Anotify-Signature: sha256=<hex>
//
// 值等于 `HMAC-SHA256(secret, 原始 body 字节)` 的小写十六进制；校验对**原始字节**做
// （不是解析后的 JSON），因为「签名 → 再序列化」与「解析 → 重新序列化」两条路径都会
// 因空白 / 键序差异产生假失败（契约点名了这个坑）。缺失 / 格式错 / 不匹配 → 401，
// 且**不产生事件**——所以校验必须在落库与派发之前完成。
//
// 两个设计约束：
//
//  1. 不引入任何「某个外部平台的签名格式」知识：这里的协议与调用方无关（§3.3）。
//  2. 比较走 `hmac.Equal`（常量时间）：逐字节短路比较会把「前几个 hex 对了」变成
//     可测量的时间差，等于把 secret 变成可爆破的预言机。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// HookSignatureHeader 是签名头的名字（契约里逐字冻结）。
const HookSignatureHeader = "X-Anotify-Signature"

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

// VerifyHookSignature 校验一次投递的签名头。
//
// secret 为空（源没配 secret）→ 直接放行，**不看**签名头：没配就是没配，
// 此时一个乱写的头不该让投递失败（契约：`secret` 空 = 不校验）。
//
// 非空 secret 时的判据（任何一条不满足 → ErrHookSignature）：
//   - 头存在且形如 `sha256=<64 个小写 hex>`（前缀大小写、裸 hex、截断/超长、大写 hex 都拒）；
//   - `hmac.Equal` 与 `HMAC-SHA256(secret, body)` 相等（body 是**原始字节**）。
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

// isLowerHex 判断是否全为小写十六进制字符。
//
// 为什么不 `strings.ToLower` 后放行：契约写的是 `<hex>`（小写），而一个「大小写都收」
// 的实现会让轮换时的**手工排错**变得含糊（`sha256=5F1D…` 到底算不算同一把？）。
// 严格一种形态，错就 401，调用方看文档即可。
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
