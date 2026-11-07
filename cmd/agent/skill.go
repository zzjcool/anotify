package main

// 本文件是 `--skill`：**内嵌手册**（issue #317 §2）。
//
// 为什么要内嵌：agent 拿到一台机器上的二进制时，可能没有仓库、没有文档、也没有网络。
// 手册必须就在二进制里，而且不读 node.yaml / 不连网 / 不落盘 —— 这三条不是洁癖，
// 而是它「在任何环境都能跑」的全部理由：空目录 + 没有 ANOTIFY_TOKEN + 断网，照样
// 退出 0 并打出全文。
//
// 内容（cmd/agent/skill.md）是任务导向的，不是 flag 罗列：起节点 / 写能力与上报 /
// 排障 / 退出码表 / 不要做。flag 细节留在 `-h`（那份由 flag 包自己生成，不会漂移）。
//
// 防漂移：skill.md 里的 `anotify-agent <子命令>` 与 `-flag` 由
// scripts/check_agent_skill.py 逐条对账真实二进制的 `-h` 输出；首行版本与
// `--version` 同理（agent 可能拿着别的机器上缓存的旧手册，首行不给版本，它就会
// 用旧 flag 去调新二进制）。

import (
	_ "embed"
	"fmt"
	"io"
	"strings"

	"github.com/zzjcool/anotify/internal/version"
)

// skillDoc 是编进二进制的手册正文（`go:embed` 在编译期把它变成字符串常量）。
//
//go:embed skill.md
var skillDoc string

// skillVersionPlaceholder 是 skill.md 首行里的版本占位符。
//
// 为什么是占位符而不是把版本写死在文件里：版本是 `-ldflags -X` 在构建时注入的
// （Makefile 的 VERSION_LDFLAGS 给 server 与 agent 注同一份），文件里写死一个值
// 只会在下一个版本变成谎言。占位符由 renderSkill 在运行期替换，check_agent_skill.py
// 会用一个假的注入版本核对替换确实生效。
const skillVersionPlaceholder = "{{VERSION}}"

// fallbackUnknown 是「这一项没被注入」时的占位：与空串区分开——空串在终端里
// 看起来像「少打了一段」，而 unknown 如实回答「构建期没给」。
const fallbackUnknown = "unknown"

// buildVersion 返回语义化版本，未注入时是 "dev"（与 internal/version 的约定一致）。
func buildVersion() string {
	v := strings.TrimSpace(version.Version)
	if v == "" {
		return "dev"
	}
	return v
}

// buildCommit 返回 git short sha，未注入时是 unknown。
func buildCommit() string {
	c := strings.TrimSpace(version.Commit)
	if c == "" {
		return fallbackUnknown
	}
	return c
}

// buildBuiltAt 返回构建时间（RFC3339），未注入时是 unknown。
func buildBuiltAt() string {
	b := strings.TrimSpace(version.BuiltAt)
	if b == "" {
		return fallbackUnknown
	}
	return b
}

// versionLabel 是「版本 + commit」两段（`1.5.0 3fd5a59`），
// `--version` 与手册首行用的是同一份拼法（issue §4 要求两处一致）。
func versionLabel() string {
	return buildVersion() + " " + buildCommit()
}

// versionLine 是 `--version` 的那一行（issue §3）：
//
//	anotify-agent 1.5.0 (3fd5a59, 2026-11-05T10:00:00Z)
//
// 不读配置、不连网、不落盘：只回答「我在驱动哪一版」，这是 agent 在自己机器上
// 唯一的本地答案（此前版本只报在 hello 帧里，本地拿不到）。
func versionLine() string {
	return fmt.Sprintf("anotify-agent %s (%s, %s)", buildVersion(), buildCommit(), buildBuiltAt())
}

// renderSkill 把 skill.md 里的版本占位符换成当前二进制真实的版本。
//
// 纯字符串替换，不写任何文件：手册的输出可以被管道重定向，进程自己不产生副作用。
func renderSkill() string {
	return strings.ReplaceAll(skillDoc, skillVersionPlaceholder, versionLabel())
}

// runSkill 把手册打到 stdout。它**永远成功**：没有可失败的输入，这就是它的用途。
func runSkill(stdout io.Writer) {
	fmt.Fprint(stdout, renderSkill())
}
