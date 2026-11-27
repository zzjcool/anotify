package sourcespec

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/zzjcool/anotify/internal/schema"
)

// outcomes 的纯判据（#522）：名字/上限/模板语法；引用解析转发到 schema（节点侧也复算同一份）。

var OutcomeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func IsOutcomeName(name string) bool { return OutcomeNamePattern.MatchString(name) }

func ValidateOutcomes(outcomes map[string][]OutcomeAction, prefix string) []Cause {
	if len(outcomes) == 0 {
		return nil
	}
	causes := []Cause{}
	if len(outcomes) > MaxOutcomes {
		causes = append(causes, Cause{
			Field: prefix + ".outcomes", Reason: CauseReasonValueInvalid,
			Message: fmt.Sprintf("结局最多 %d 个（实际 %d 个）", MaxOutcomes, len(outcomes)),
		})
	}
	names := make([]string, 0, len(outcomes))
	for name := range outcomes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		actions := outcomes[name]
		if !IsOutcomeName(name) {
			causes = append(causes, Cause{
				Field: prefix + ".outcomes", Reason: CauseReasonValueInvalid,
				Message: fmt.Sprintf("结局名 %q 不合法（只允许小写字母/数字/下划线/连字符，首字符是字母或数字，最长 32）", name),
			})
			continue
		}
		field := prefix + ".outcomes." + name
		if len(actions) > MaxOutcomeActions {
			causes = append(causes, Cause{
				Field: field, Reason: CauseReasonValueInvalid,
				Message: fmt.Sprintf("结局 %q 最多 %d 个动作（实际 %d 个）", name, MaxOutcomeActions, len(actions)),
			})
		}
		for i, action := range actions {
			actionField := field + "[" + strconv.Itoa(i) + "]"
			if strings.TrimSpace(action.Capability) == "" {
				causes = append(causes, Cause{
					Field: actionField + ".capability", Reason: CauseReasonValueInvalid, Message: "不能为空",
				})
				continue
			}
			if err := ValidateActionParamsTemplate(action.Params); err != nil {
				causes = append(causes, Cause{
					Field: actionField + ".params", Reason: CauseReasonValueInvalid, Message: err.Error(),
				})
			}
		}
	}
	return causes
}

func DeclaredRef(next []string, to string) (string, bool) {
	return schema.DeclaredRef(next, to)
}

func SplitRef(ref string) (string, int, bool) {
	return schema.SplitRef(ref)
}

func RefName(ref string) string {
	name, _, _ := SplitRef(ref)
	return name
}

func DeclaredVersion(ref string) int {
	_, ver, pinned := SplitRef(ref)
	if !pinned || ver < 0 {
		return 0
	}
	return ver
}
