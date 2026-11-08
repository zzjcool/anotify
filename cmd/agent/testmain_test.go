package main

// 包级测试临时目录的统一收口（#326）。
//
// 背景：本机 `/tmp` 是 tmpfs —— 临时目录漏出去吃的是**内存**，不是磁盘。
// buildAgentBinary 会在系统临时目录里编一份真的 agent 二进制（每份约 15MB），
// 原来没有任何 RemoveAll：`go test ./cmd/agent` 跑 N 次就往 /tmp 里留 N 份。
// 这类目录不属于任何单个用例（sync.Once + 包级共享），t.Cleanup 没有落点，
// 所以由 TestMain 统一持有、m.Run() 之后统一 RemoveAll。
//
// 与 t.TempDir() 的分工（别混用）：
//   - 单个用例自己的目录 → 继续用 t.TempDir()：testing 自己会删，且 -run 子集
//     下也能正确工作；
//   - 跨用例共享、生命周期到整包结束的产物 → mkTestTempDir() 登记到这里。
//
// 回归守卫：scripts/check-tmp-leaks.mjs 在 `make ci` 里比对运行前后的
// /tmp/anotify-* 集合，本次新增任何一个都会红（不能只看总量：本机可能同时
// 有别的 e2e / 测试在跑）。
import (
	"fmt"
	"os"
	"sync"
	"testing"
)

var (
	testTempMu   sync.Mutex
	testTempDirs []string
)

// mkTestTempDir 建一个包级临时目录并登记，由 TestMain 在整包跑完后统一删除。
func mkTestTempDir(prefix string) (string, error) {
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return "", err
	}
	testTempMu.Lock()
	testTempDirs = append(testTempDirs, dir)
	testTempMu.Unlock()
	return dir, nil
}

// isolateSystemTemp 把本用例看到的 os.TempDir() 指到 t.TempDir() 下。
//
// 用于覆盖「没有 --data-dir → 工作目录/数据目录兜底到系统临时目录」的用例：
// 兜底行为本身必须继续被验证，但测试不该往真实的 /tmp 里写 —— 本机 /tmp 是
// tmpfs（吃内存），而且超时/失败路径会故意**保留**整棵任务目录供人工翻现场。
// 走 t.Setenv 后，这些目录落在 testing 自己会删的 t.TempDir() 里。
//
// 注意：只能在非 parallel 的用例里用（t.Setenv 的限制）。
func isolateSystemTemp(t *testing.T) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
}

// TestMain 是整包唯一入口：先跑测试，再把包级临时目录删干净。
//
// 两个细节都是刻意的：
//  1. 用 os.Exit(code) 而不是自然返回 —— m.Run() 的结论必须原样变成进程退出码，
//     否则测试失败会被吞成 0；
//  2. 清理显式写在 m.Run() 之后 —— os.Exit 不跑 defer。清理失败不算测试失败
//     （只影响 /tmp 卫生），但必须打印出来：静默失败等于没人知道它又在漏。
func TestMain(m *testing.M) {
	code := m.Run()

	testTempMu.Lock()
	dirs := append([]string(nil), testTempDirs...)
	testTempDirs = nil
	testTempMu.Unlock()
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			fmt.Fprintf(os.Stderr, "[tmp-cleanup] 删除 %s 失败: %v\n", dir, err)
		}
	}

	os.Exit(code)
}
