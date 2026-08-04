package auth

import "testing"

func TestValidateUsername(t *testing.T) {
	valid := []string{
		"zheng", "ab", "user123", "a_b", "a-b", "a.b",
		"User.Name-1_2", "x9", "abc.def_ghi-jkl",
		"a123456789012345678901234567890b", // 32 字符
	}
	for _, u := range valid {
		if err := ValidateUsername(u); err != nil {
			t.Errorf("ValidateUsername(%q) 应合法，得到错误 %v", u, err)
		}
	}

	invalid := map[string]string{
		"":            "空",
		"a":           "太短（<2）",
		"-abc":        "- 开头",
		"abc-":        "- 结尾",
		"_abc":        "_ 开头",
		".abc":        ". 开头",
		"abc.":        ". 结尾",
		"a b":         "含空格",
		"a/b":         "含斜杠",
		"a@b":         "含 @",
		"中文":          "非 ASCII",
		"a中文":         "含中文",
		"a12345678901234567890123456789012": "超长（33）",
	}
	for u, why := range invalid {
		if err := ValidateUsername(u); err == nil {
			t.Errorf("ValidateUsername(%q) 应非法（%s），却通过", u, why)
		}
	}
}
