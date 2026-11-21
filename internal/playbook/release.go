package playbook

import "regexp"

// release 是**安装级身份**（Helm 语义）：同一个包可以有多个 release，各一套名字与
// values；要复用的行属于另一个 release 时整包拒绝（409），绝不静默认领。
// 缺省 = 包名，所以老包逐字不变。语义全貌见 docs/reports/playbook-release.md。
const ReleaseRef = "release.name"

// releasePattern 是 release 的合法形状，与节点名（store 的 nodeNamePattern）同形：
// 1..63 个字符，首字符是小写字母或数字，其余只允许小写字母、数字、`.`、`_`、`-`。
var releasePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// NormalizeRelease 把请求里可选的 release 归一成本次安装的身份：空串 = 包名。
// 空串刻意不复查正则（包名上限 64 > release 的 63，复查会让合法老包突然装不上）；
// 显式输入不 trim，形状错当场拒绝（与节点名同一口径）。
func NormalizeRelease(explicit, slug string) (string, error) {
	if explicit == "" {
		if slug == "" {
			return "", errf(CodeInvalidPlaybook, "release 缺省时要有包名可退（slug 为空）")
		}
		return slug, nil
	}
	if !releasePattern.MatchString(explicit) {
		return "", errf(CodeInvalidPlaybook,
			"release %q 不合法：必须匹配 %s（1..63 个字符，首字符是小写字母或数字，其余只允许小写字母、数字、`.`、`_`、`-`）",
			explicit, releasePattern.String()).withDetails(map[string]any{
			"field":   "release",
			"pattern": releasePattern.String(),
		})
	}
	return explicit, nil
}

// ReleaseConflict 是「这条要复用的行是别的 release 装出来的」的一条认领冲突，
// 也是 409 `release_conflict` 的 details 形状：四个字段都要能指认到「哪一行、
// 哪个对象、谁的 release、哪次安装」，否则用户不知道该换 release 还是改名字模板。
type ReleaseConflict struct {
	Name           string `json:"name"`
	Kind           string `json:"kind"`
	OwnerRelease   string `json:"ownerRelease"`
	OwnerInstallID string `json:"ownerInstallId"`
}

// releaseConflictHint 是 409 的处置提示（两条出路，缺一条就是死路）。
const releaseConflictHint = "把 release 写进名字模板（{{release.name}}），让每份实例有自己的名字；" +
	"或者换一个 release——不同 release 的实例互不认领，同一个 release 才是升级"

// IsReleaseConflict 报告 err 是否是 release 认领冲突（409）。
func IsReleaseConflict(err error) bool { return IsCode(err, CodeReleaseConflict) }
