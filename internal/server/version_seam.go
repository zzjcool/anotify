package server

// serverVersion 是中枢自己的版本号（agent 升级的**目标版本**，CONTRACT-agent-upgrade §1）。
//
// SEAM(A)：契约 §1 的 `internal/version` 包由 A 组创建（`version.Version` /
// `version.String()`），B 组不得修改 `internal/version/**`、也不改 Makefile（§7）。
// 本分支（B）与 A 并行开发，A 合入前 `internal/version` 还不存在，所以这里先返回
// 契约里的缺省值 "dev"。
//
// A 合入后把本文件替换成下面三行（调用点不用动）：
//
//	import "github.com/zzjcool/anotify/internal/version"
//
//	func serverVersion() string { return version.String() }
//
// 调用点只有 mux.go 的 loadAgentBinary（版本的第二个消费方是 GET /v1/nodes 的
// targetAgentVersion，它读的是同一个 agentBinary.version），所以这次替换是机械的。
func serverVersion() string { return "dev" }
