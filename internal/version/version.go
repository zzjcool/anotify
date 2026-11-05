// Package version 是**构建期注入的版本元数据**（契约 §1）。
//
// 三个变量都由 `-ldflags -X` 注入（Makefile 的 VERSION_LDFLAGS 给 server 与 agent
// 两个二进制都注入同一份）：
//
//	go build -ldflags "-X github.com/zzjcool/anotify/internal/version.Version=1.5.0 \
//	                   -X .../internal/version.Commit=3fd5a59 \
//	                   -X .../internal/version.BuiltAt=2026-11-05T10:00:00Z"
//
// 为什么是包级 var 而不是常量：linker 的 `-X` 只能改「字符串类型的包级变量」，
// 常量改不动。为什么单独一个包：server 与 agent 两个二进制都要报同一份事实
// （agent 把它报进 hello 帧，见 internal/ws.HelloFrame.AgentVersion），
// 放在任何一侧都会让另一侧反向依赖。
//
// 缺省值就是「没注入」的样子：Version="dev"、Commit/BuiltAt 为空。零配置
// `go run ./cmd/agent` 也能如实回答「我是 dev 构建，不是某个正式版本」。
package version

import "strings"

var (
	// Version 是语义化版本（如 "1.5.0"），未注入时是 "dev"。
	//
	// 约定：**不带空格**。它会进 -ldflags，而 linker 按空格拆参数，
	// 带空格的值会把后面的注入项挤成另一个参数（构建失败或静默少注入一项）。
	// 构建时间因此单独走 BuiltAt，不跟在这里。
	Version = "dev"
	// Commit 是 git short sha（如 "3fd5a59"），未注入时为空。
	Commit = ""
	// BuiltAt 是构建时间（RFC3339，UTC），未注入时为空。
	//
	// 刻意**不**在运行期填一个"当前时间"：那等于撒谎——排障时会把
	// 「刚 build 的」和「三年前的」看成一回事，而这恰恰是版本号要回答的问题。
	BuiltAt = ""
)

// String 把版本与 commit 拼成给人看的一行（契约 §1）：
//
//	Version="1.5.0", Commit="3fd5a59"  → "1.5.0+3fd5a59"
//	Version="dev",   Commit=""         → "dev"
//
// 只在 Commit 非空时加 "+<sha>"：空 commit 还拼一个 "+" 会得到 "dev+"，
// 既难看也是假信息（那一位什么都没说）。Version 被注入成空串时按 "dev" 处理，
// 不让「忘了给 -X」变成「版本号是空字符串」——空串在控制台里等于「老版本」，
// 与「这是 dev 构建」是两件事。
func String() string {
	v := strings.TrimSpace(Version)
	if v == "" {
		v = "dev"
	}
	c := strings.TrimSpace(Commit)
	if c == "" {
		return v
	}
	return v + "+" + c
}
