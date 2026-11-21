// Package decisions 是 v4 决策索引的代码侧落点：编号即外键（`@decision` 按它解析），每条指向执行者。
package decisions

const (
	LightweightResourceAPI = "V4-D1"
	NamespaceVarsScopes    = "V4-D2"
	InstallAdditiveOnly    = "V4-D3"
	SafeExecution          = "V4-D4"
	RuntimeEnvAndCreds     = "V4-D5"
	BoundedRetention       = "V4-D6"
)

// Implementations 把决策号映射到执行它的文件（相对仓库根），由 decisions_test.go 核对存在。
var Implementations = map[string]string{
	LightweightResourceAPI: "internal/server/api_envelope.go",
	NamespaceVarsScopes:    "internal/store/config.go",
	InstallAdditiveOnly:    "internal/playbook/plan.go",
	SafeExecution:          "internal/lease/lease.go",
	RuntimeEnvAndCreds:     "cmd/anode/internal/execenv/execenv.go",
	BoundedRetention:       "internal/server/capability_gc.go",
}
