package server

import "github.com/zzjcool/anotify/internal/version"

// serverVersion 是中枢自己的版本号，也是 agent 升级的**目标版本**
// （.pi-orchestrator/CONTRACT-agent-upgrade.md §1）。
//
// 为什么单独一个函数而不是到处 import：版本的唯一来源是构建期注入的
// internal/version，而这台机器的 /app/anode（= 升级源二进制，v3 的 /app/agent）必须与中枢是
// **同一份构建**——两边读同一个 version.String()，才不会出现「中枢说 1.5.0、
// 发出去的 agent 是 dev」这种升级死循环。
//
// 两个消费方：GET /v1/node/binary 的 X-Agent-Version、GET /v1/nodes 的
// targetAgentVersion（后者读 agentBinary.version，来源是同一个）。
func serverVersion() string { return version.String() }
