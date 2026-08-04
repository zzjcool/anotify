package auth

import (
	"fmt"
	"regexp"
)

// 用户名规则（产品级，WebAuthn 友好）：
//   - 长度 2–32
//   - 字符：字母 / 数字 / _ / - / .（ASCII；显示名 DisplayName 才允许中文等任意字符）
//   - 不能以 _ - . 开头或结尾（避免混淆与路径/子域歧义）
//
// 用户名是登录标识（challenge 的 key、唯一约束），需稳定可输入；
// 显示名仅用于展示，不做字符限制。
var usernameRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,30}[A-Za-z0-9])?$`)

// ValidateUsername 校验用户名是否合法，非法时返回带原因的错误。
func ValidateUsername(username string) error {
	if username == "" {
		return fmt.Errorf("用户名不能为空")
	}
	if len(username) < 2 {
		return fmt.Errorf("用户名至少 2 个字符")
	}
	if len(username) > 32 {
		return fmt.Errorf("用户名最多 32 个字符")
	}
	if !usernameRe.MatchString(username) {
		return fmt.Errorf("用户名只能包含字母、数字、_、-、.，且不能以 _ - . 开头或结尾")
	}
	return nil
}
