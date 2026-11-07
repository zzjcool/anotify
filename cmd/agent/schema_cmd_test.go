package main

// 本文件钉住**离线书写面**的第二条命令：`anotify-agent schema [node|capability|emit]` 的
// 输出纪律（issue #319 §2）：能直接喂给 jq / python3 -m json.tool，且**两次输出逐字节相同**
// （否则没法在 CI 里 diff，也没法确认「agent 看的就是这一版」）。
//
// 内容是否正确不在这里（那是 schema_consistency_test.go 的活：与 artifact.Validate /
// emitContractJSON 双向钉住）；这里只管「形状、稳定性、错误与发现路径」。

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// schemaRun 跑一次 `schema <name>`（args 原样给，便于测缺省与多余参数）。
func schemaRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return cliTextInvoke(t, append([]string{schemaCommand}, args...)...)
}

// TestSchemaOutputStableAndParseable：三份 schema（+ 缺省）都能被 JSON 解析，且
// **两次运行 / 带不带 --json 都逐字节相同**。
func TestSchemaOutputStableAndParseable(t *testing.T) {
	t.Parallel()

	for _, name := range append([]string{""}, schemaNames()...) {
		name := name
		label := name
		if label == "" {
			label = "（缺省）"
		}
		t.Run(label, func(t *testing.T) {
			args := []string{}
			if name != "" {
				args = []string{name}
			}

			code1, out1, err1 := schemaRun(t, args...)
			if code1 != exitOK {
				t.Fatalf("schema %s 退出码 = %d, want 0（stderr: %s）", label, code1, err1)
			}
			if err1 != "" {
				t.Fatalf("schema %s 不该往 stderr 写东西：%q", label, err1)
			}

			var doc any
			if err := json.Unmarshal([]byte(out1), &doc); err != nil {
				t.Fatalf("schema %s 的输出不是合法 JSON: %v\n%s", label, err, out1)
			}
			if _, ok := doc.(map[string]any); !ok {
				t.Fatalf("schema %s 的根应当是 JSON 对象，实际 %T", label, doc)
			}
			if !strings.HasSuffix(out1, "}\n") {
				t.Fatalf("schema %s 的输出应当以一个换行结束（管道友好）：%q", label, out1)
			}

			// 两次运行逐字节相同（json.MarshalIndent 排序 key，map 迭代顺序不影响结果）。
			code2, out2, _ := schemaRun(t, args...)
			if code2 != exitOK || out1 != out2 {
				t.Fatalf("schema %s 两次输出不同（%d vs %d 字节）", label, len(out1), len(out2))
			}

			// --json 是全局开关的兼容位：输出必须与不带它时**逐字节相同**。
			code3, out3, _ := schemaRun(t, append(args, "--json")...)
			if code3 != exitOK || out1 != out3 {
				t.Fatalf("schema %s --json 与不带 --json 的输出不同（--json 对 schema 应当无额外效果）", label)
			}
		})
	}
}

// TestSchemaDefaultIsCapability：不给名字 = 能力 yaml 的形状（写 yaml 的人占绝大多数）。
func TestSchemaDefaultIsCapability(t *testing.T) {
	t.Parallel()

	_, def, _ := schemaRun(t)
	_, explicit, _ := schemaRun(t, schemaCapability)
	if def != explicit {
		t.Fatalf("缺省输出 != schema %s", schemaCapability)
	}
	// 抽查一个只属于能力 schema 的键，确认缺省没选错。
	if !strings.Contains(def, `"apiVersion"`) {
		t.Fatalf("缺省输出看起来不是能力 yaml 的 schema：\n%s", def[:min(len(def), 200)])
	}
}

// TestSchemaUnknownNameIsUsageError：拼错的名字要**明确拒绝**并列出可用的三个
// （不做前缀/大小写容忍：猜错了作者会照着错的形状写 yaml）。
func TestSchemaUnknownNameIsUsageError(t *testing.T) {
	t.Parallel()

	code, stdout, stderr := cliJSONInvoke(t, schemaCommand, "nodes")
	if code != exitFailure {
		t.Fatalf("退出码 = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("报错时 stdout 应为空：%q", stdout)
	}
	apiErr := cliDecodeError(t, stderr)
	if apiErr.Kind != kindUsage {
		t.Fatalf("kind = %q, want %q", apiErr.Kind, kindUsage)
	}
	for _, name := range schemaNames() {
		if !strings.Contains(apiErr.Message, name) {
			t.Errorf("message 没列出可用的 %q：%s", name, apiErr.Message)
		}
	}
}

// TestSchemaExtraArgsIsUsageError：schema 只收一个名字。
func TestSchemaExtraArgsIsUsageError(t *testing.T) {
	t.Parallel()

	code, _, stderr := cliJSONInvoke(t, schemaCommand, "node", "capability")
	if code != exitFailure {
		t.Fatalf("退出码 = %d, want 1", code)
	}
	if got := cliDecodeError(t, stderr).Kind; got != kindUsage {
		t.Fatalf("kind = %q, want %q", got, kindUsage)
	}
}

// TestSchemaHelpListsThreeNames：`schema -h` 要把三个名字的含义写出来
// （选错一次就会照着错的文档写 yaml，所以不能只列名字）。
func TestSchemaHelpListsThreeNames(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if code := mainWith([]string{schemaCommand, "-h"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("schema -h 退出码 = %d, want 0", code)
	}
	help := stderr.String()
	if help == "" {
		help = stdout.String()
	}
	for _, want := range []string{schemaNode, schemaCapability, schemaEmit, "node.yaml", "能力 yaml", "出口契约"} {
		if !strings.Contains(help, want) {
			t.Errorf("schema -h 缺 %q：\n%s", want, help)
		}
	}
}
