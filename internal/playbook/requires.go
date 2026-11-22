package playbook

import (
	"fmt"
	"sort"

	"github.com/zzjcool/anotify/internal/artifact"
)

// MaxRequires 是 spec.requires 的条目上限（含）；同一 capability 出现两次也是 400。
const MaxRequires = 32

// Requirement 是 spec.requires 的一条声明（MinVersion 省略（0）= 任意已装版本）。
type Requirement struct {
	Capability string `json:"capability"`
	MinVersion int    `json:"minVersion,omitempty"`
}

// Dependency 是一条解析成功的依赖（进回执 dependencies[]）：能力名 + 安装时可见的最高版本。
type Dependency struct {
	Capability string `json:"capability"`
	Version    int    `json:"version"`
}

// Cause 是一条逐点校验结论（wire 上是 Status.details.causes[] 的一项，三键形状不扩）。
type Cause struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	Field   string `json:"field,omitempty"`
}

// 只认这两个键：capability 是字面量，不参与 `{{values.*}}` 渲染。
const (
	requireKeyCapability = "capability"
	requireKeyMinVersion = "minVersion"
)

// requireField 拼一条 cause 的字段路径（形如 spec.requires[0].capability）。
func requireField(i int, suffix string) string {
	return fmt.Sprintf("spec.requires[%d]%s", i, suffix)
}

// requiresError 把收集到的 causes 一次性报到 400 invalid_playbook 上。
func requiresError(where string, causes []Cause) *Error {
	return errf(CodeInvalidPlaybook, "%s 的 spec.requires 不合法（%d 条问题，见 details.causes）",
		where, len(causes)).withCauses(causes...)
}

// parseRequires 读 spec.requires 的**形状**（写入期，值无关）：全部问题收集成 causes 后一次报。
func parseRequires(spec map[string]any, where string) ([]Requirement, error) {
	raw, ok := spec["requires"]
	if !ok || raw == nil {
		return nil, nil
	}
	causes := make([]Cause, 0)
	list, isList := raw.([]any)
	if !isList {
		causes = append(causes, Cause{
			Reason:  CodeInvalidPlaybook,
			Field:   "spec.requires",
			Message: "spec.requires 必须是数组（- capability: <name>，可带 minVersion: <n>）",
		})
		return nil, requiresError(where, causes)
	}
	if len(list) > MaxRequires {
		causes = append(causes, Cause{
			Reason:  CodeInvalidPlaybook,
			Field:   "spec.requires",
			Message: fmt.Sprintf("spec.requires 有 %d 条，上限 %d（拆包）", len(list), MaxRequires),
		})
	}

	out := make([]Requirement, 0, len(list))
	seen := make(map[string]int, len(list))
	for i, item := range list {
		entry, isMap := item.(map[string]any)
		if !isMap {
			causes = append(causes, Cause{
				Reason:  CodeInvalidPlaybook,
				Field:   requireField(i, ""),
				Message: fmt.Sprintf("spec.requires[%d] 必须是对象（capability / minVersion）", i),
			})
			continue
		}
		problems := len(causes)
		unknown := make([]string, 0, len(entry))
		for k := range entry {
			if k != requireKeyCapability && k != requireKeyMinVersion {
				unknown = append(unknown, k)
			}
		}
		sort.Strings(unknown)
		for _, k := range unknown {
			causes = append(causes, Cause{
				Reason:  CodeInvalidPlaybook,
				Field:   requireField(i, "."+k),
				Message: fmt.Sprintf("spec.requires[%d] 有未知字段 %q（只认 capability / minVersion）", i, k),
			})
		}

		capability, isStr := entry[requireKeyCapability].(string)
		switch {
		case !isStr || capability == "":
			causes = append(causes, Cause{
				Reason:  CodeInvalidPlaybook,
				Field:   requireField(i, ".capability"),
				Message: fmt.Sprintf("spec.requires[%d].capability 必填且必须是非空字符串", i),
			})
		case !artifact.IsName(capability):
			causes = append(causes, Cause{
				Reason:  CodeInvalidPlaybook,
				Field:   requireField(i, ".capability"),
				Message: fmt.Sprintf("spec.requires[%d].capability %q 不是合法能力名（小写字母/数字/. _ -，首字符是字母或数字，≤63）", i, capability),
			})
		default:
			if prev, dup := seen[capability]; dup {
				causes = append(causes, Cause{
					Reason:  CodeInvalidPlaybook,
					Field:   requireField(i, ".capability"),
					Message: fmt.Sprintf("spec.requires[%d] 重复声明 %q（spec.requires[%d] 已经有一条）", i, capability, prev),
				})
			} else {
				seen[capability] = i
			}
		}

		minVersion := 0
		if v, ok := entry[requireKeyMinVersion]; ok && v != nil {
			n, isInt := toInt(v)
			if !isInt || n < 1 {
				causes = append(causes, Cause{
					Reason:  CodeInvalidPlaybook,
					Field:   requireField(i, ".minVersion"),
					Message: fmt.Sprintf("spec.requires[%d].minVersion 必须是 ≥1 的整数（省略 = 任意已装版本）", i),
				})
			} else {
				minVersion = n
			}
		}
		if len(causes) == problems {
			out = append(out, Requirement{Capability: capability, MinVersion: minVersion})
		}
	}
	if len(causes) > 0 {
		return nil, requiresError(where, causes)
	}
	return out, nil
}

// resolveRequires 解析声明侧依赖（安装期：**先自引用（400）、再逐条解析（409）**），
// 满足时按声明顺序返回 Dependency（版本 = 命名空间里当前可见的最高版本）。
func resolveRequires(reqs []Requirement, installed []*artifact.Artifact, existing Existing) ([]Dependency, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	selfMade := make(map[string]bool, len(installed))
	for _, a := range installed {
		if a != nil {
			selfMade[a.Metadata.Name] = true
		}
	}
	selfCauses := make([]Cause, 0)
	for i, req := range reqs {
		if !selfMade[req.Capability] {
			continue
		}
		selfCauses = append(selfCauses, Cause{
			Reason:  CodeInvalidPlaybook,
			Field:   requireField(i, ".capability"),
			Message: fmt.Sprintf("本包自己安装 %q：自己装的东西不用 require", req.Capability),
		})
	}
	if len(selfCauses) > 0 {
		return nil, errf(CodeInvalidPlaybook,
			"%s 的 spec.requires 点名了本包自己安装的能力（%d 条，见 details.causes）",
			EntryFile, len(selfCauses)).withCauses(selfCauses...)
	}

	deps := make([]Dependency, 0, len(reqs))
	causes := make([]Cause, 0)
	for i, req := range reqs {
		maxV, ok := existing.CapabilityMaxVersion(req.Capability)
		if !ok {
			causes = append(causes, Cause{
				Reason: CodeDependencyMissing,
				Field:  requireField(i, ".capability"),
				Message: fmt.Sprintf("依赖能力 %q 在命名空间里没有可见版本（need: %d, have: none）",
					req.Capability, req.MinVersion),
			})
			continue
		}
		if req.MinVersion > 0 && maxV < req.MinVersion {
			causes = append(causes, Cause{
				Reason: CodeDependencyMissing,
				Field:  requireField(i, ".capability"),
				Message: fmt.Sprintf("依赖能力 %q 的可见版本不够（need: %d, have: %d）",
					req.Capability, req.MinVersion, maxV),
			})
			continue
		}
		deps = append(deps, Dependency{Capability: req.Capability, Version: maxV})
	}
	if len(causes) > 0 {
		return nil, errf(CodeDependencyMissing,
			"预案的外部依赖未满足（%d 条）：安装被拒绝，一行不写", len(causes)).withCauses(causes...)
	}
	return deps, nil
}
