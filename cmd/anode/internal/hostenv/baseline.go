// Package hostenv 是守护进程启动时的宿主环境快照（capability.md §4.0 / anode.md §3.2）。
//
// 两条硬约束在这里落地：
//
//  1. **PATH 同源**：PATH 取 `anode` 启动那一刻的基线；`requires.commands` 探测
//     与任务初始 PATH 必须是同一份（本包只提供这一份，runtime 直接用它，不要
//     再读 os.Getenv("PATH")）。
//  2. **凭证只按名字白名单**：envPassthrough 里的名字在启动时快照住值；任务执行
//     时只用快照，不重新读进程环境、不把完整名单/值上报。名单本身只在本机。
package hostenv

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Baseline 是守护进程启动期的环境快照。
type Baseline struct {
	// Path 是启动时的 PATH（探测与执行共用）。
	Path string
	// Home 是启动时的 HOME（作为 status.homeDir 的兜底；不是任务 HOME）。
	Home string

	// passthrough 只包含**启动时确实存在**的白名单变量值。
	passthrough map[string]string
}

// Capture 从当前进程环境抓取 PATH/HOME 与白名单变量的值。
//
// 白名单里不存在于环境的变量不会被记录（运行时「缺即不存在」）。
func Capture(passthrough []string) Baseline {
	return CaptureWith(passthrough, os.LookupEnv)
}

// CaptureWith 用注入的 lookup 抓快照（测试与 systemd/容器接线共用同一逻辑）。
func CaptureWith(passthrough []string, lookup func(string) (string, bool)) Baseline {
	b := Baseline{passthrough: make(map[string]string, len(passthrough))}
	b.Path, _ = lookup("PATH")
	b.Home, _ = lookup("HOME")
	for _, name := range passthrough {
		if value, ok := lookup(name); ok {
			b.passthrough[name] = value
		}
	}
	return b
}

// PATH 返回启动基线 PATH（任务环境与命令探测的唯一来源）。
func (b Baseline) PATH() string { return b.Path }

// HOME 返回启动基线 HOME。
func (b Baseline) HOME() string { return b.Home }

// Passthrough 返回白名单变量在启动快照里的值；不在名单/启动时缺失 → false。
//
// 没有「列出全部名单」的方法：完整名单只在本机配置里，不上报、不进快照、
// 不进诊断（诊断只报本次实际用到的名字）。
func (b Baseline) Passthrough(name string) (string, bool) {
	value, ok := b.passthrough[name]
	return value, ok
}

// LookPath 用**启动基线 PATH** 探测一条命令（与 execution PATH 同源）。
func (b Baseline) LookPath(name string) (string, error) {
	return LookPath(b.Path, name)
}

// LookPath 在给定 PATH 字符串里查找可执行文件。
//
// 行为对齐 os/exec.LookPath 的 Unix 语义：名字含 '/' 直接按路径找；空 PATH 项
// 视为 "."；命中要求普通文件且有执行位。返回绝对路径（hello.capabilities 报
// 的是「命中的绝对路径」）。
func LookPath(pathEnv, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: 空命令名", exec.ErrNotFound)
	}
	if strings.ContainsRune(name, '/') {
		return findExecutable(name)
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		path := filepath.Join(dir, name)
		if abs, err := findExecutable(path); err == nil {
			return abs, nil
		}
	}
	return "", fmt.Errorf("%w: %s", exec.ErrNotFound, name)
}

// findExecutable 返回绝对路径；不是普通可执行文件就是 ErrNotFound。
func findExecutable(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: %s", exec.ErrNotFound, path)
	}
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s", exec.ErrNotFound, path)
		}
		return "", fmt.Errorf("%w: %s: %v", exec.ErrNotFound, path, err)
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%w: %s", exec.ErrNotFound, path)
	}
	return abs, nil
}
