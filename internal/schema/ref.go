package schema

import (
	"fmt"
	"strings"
)

// 能力引用的解析与「是否落在 next 声明里」判据：artifact 与 sourcespec 共用，节点侧也复算。

// SplitRef 拆成 (名字, 版本, 是否钉死)；钉死但版本写法非法时 version = -1（fail closed）。
func SplitRef(ref string) (name string, version int, pinned bool) {
	ref = strings.TrimSpace(ref)
	name, verRaw, found := strings.Cut(ref, "@")
	if !found {
		return name, 0, false
	}
	version, ok := parsePinnedVersion(verRaw)
	if !ok {
		return name, -1, true
	}
	return name, version, true
}

// DeclaredRef 回答「to 是否落在 next 声明里」；歧义即拒（裸名+钉版混写、同名多钉版）。
func DeclaredRef(next []string, to string) (string, bool) {
	toName, toVer, toPinned := SplitRef(to)
	if toName == "" || (toPinned && toVer < 0) {
		return "", false
	}
	unique := make(map[string]bool)
	for _, ref := range next {
		name, ver, pinned := SplitRef(ref)
		if name != toName {
			continue
		}
		if toPinned {
			if pinned && ver == toVer {
				return fmt.Sprintf("%s@%d", name, ver), true
			}
			continue
		}
		if pinned {
			if ver < 0 {
				continue // 坏的钉版写法永不匹配（fail closed）
			}
			unique[fmt.Sprintf("%s@%d", name, ver)] = true
		} else {
			unique[name] = true
		}
	}
	if len(unique) != 1 {
		return "", false
	}
	for ref := range unique {
		return ref, true
	}
	return "", false
}

func parsePinnedVersion(raw string) (int, bool) {
	if raw == "" || len(raw) > 9 {
		return 0, false
	}
	if len(raw) > 1 && raw[0] == '0' {
		return 0, false
	}
	n := 0
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	if n <= 0 {
		return 0, false
	}
	return n, true
}
