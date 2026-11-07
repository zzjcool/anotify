package main

// 本文件是 `anotify-agent capabilities`：**本机能跑什么**（issue #320 §1）。
//
// 能力的唯一定义在中枢（裁决 D4），节点只知道「我缓存了哪些产物」——就是
// `<data-dir>/index/<name>@<version>`（cache.go）与它指到的
// `<data-dir>/artifacts/<digest>/`。这条命令把那份索引讲成一张表：
//
//	NAME@VER  OUTPUTS  DIGEST(12)
//
// 它**只读**两个目录：不连中枢、不开 socket、不校验产物内容（校验是 #319 的
// `check` 的事）。读的失败（权限、目录是一个文件）算查询失败退 1；**空结果是 0**：
// 「这台机器还什么都不能跑」是一条合法事实，不是查询失败——与 `runs` 的空结果
// 语义（=1）刻意不同。两条命令的空结果含义不同这一点，`-h` 与 `--skill` 都写了，
// 否则下一个人会来「修」它。
//
// # 为什么 OUTPUTS 直接取 spec.outputs
//
// 能力脚本里 `anotify-agent emit --port X` 的 X 必须落在这个集合里（出口契约由
// agent 注入）。agent 写一个能力/调一次 emit 之前，第一件要问的就是「这个能力
// 声明了哪些出口」——它就在产物里，没有必要让 agent 去解 YAML。
//
// # 为什么 DIGEST 只打 12 位
//
// 排障时人只用它做肉眼比对（日志里的 digest=sha256:…、中枢上的版本）。64 位十六
// 进制在终端里全是 token，完整值在 `--json` 里。表里打的是 digest **去掉
// "sha256:" 前缀后的前 12 位**：前缀是常量，占位不携带任何信息。

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
)

// capabilitiesUsage 是 `capabilities` 的用法串。
const capabilitiesUsage = "anotify-agent capabilities [--data-dir DIR] [--json]"

// capabilitiesHelpNote 是 `-h` 里那句「空结果为什么是 0」的说明。
//
// 它不是装饰：不看这句的人会把「空列表 + 退出码 0」当成 bug 改掉（issue §1 明确
// 要求把理由写进 help，否则下一个人会来「修」它）。
const capabilitiesHelpNote = `空结果是退出码 0（不是 1）：这台机器什么都能跑是一条合法事实，不是查询失败。
这一点与 runs 刻意相反——runs 空结果 = 1，因为「没有执行记录」几乎总是目录/id 写错。
只读 <data-dir>/index 与 <data-dir>/artifacts：不连中枢、不校验产物内容（校验用 check）。`

// capabilitiesExitRows 是 `-h` 的退出码表。
var capabilitiesExitRows = []exitRow{
	{code: "0", desc: "查询成功（**空结果也是 0**：没有缓存产物也是合法事实）"},
	{code: "1", desc: "数据目录 / 索引目录读不出来（--data-dir 写错、权限不对）",
		kind: kindConfigInvalid},
}

// capabilityInfo 是表里的一行，也是 `--json` 里的一条。
//
// 字段名就是 issue §1 要求的五个：name / version / outputs / digest / dir。
// Outputs 恒为非 nil（空列表纪律：`[]` 而不是 `null`）。
type capabilityInfo struct {
	Name    string   `json:"name"`
	Version int      `json:"version"`
	Outputs []string `json:"outputs"`
	// Digest 是**按内容重新算出**的 digest（与 hello.applied 同源）：索引里记的值
	// 可能已经被手改过的产物落在后面，中枢靠这个差值决定重推。
	Digest string `json:"digest"`
	// Dir 是这份产物在盘上的目录（artifacts/<digest>）：用户要去看/改/校验时从这里出发。
	Dir string `json:"dir"`
}

// capabilitiesJSON 是 `--json` 的形状：`{"capabilities":[…]}`（空时是 `[]`）。
type capabilitiesJSON struct {
	Capabilities []capabilityInfo `json:"capabilities"`
}

// capabilitiesOptions 是 `capabilities` 解析出来的参数。
type capabilitiesOptions struct {
	dataDir string // --data-dir：缺省空串 = ~/.anotify（resolveDataDir 归一）
	json    bool   // --json：机器可读
}

// parseCapabilitiesFlags 解析 `capabilities` 的参数（与其它子命令同一个模式）。
func parseCapabilitiesFlags(args []string, stderr io.Writer) (capabilitiesOptions, error) {
	fs := flag.NewFlagSet("agent capabilities", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { writeSubcommandHelp(fs, capabilitiesUsage, capabilitiesExitRows, capabilitiesHelpNote) }

	dataDir := fs.String("data-dir", "", "数据目录（默认 ~/.anotify）：读它下面的 index/ 与 artifacts/")
	jsonOut := fs.Bool("json", false,
		"机器可读输出：{\"capabilities\":[{name,version,outputs,digest,dir}]}")

	if err := fs.Parse(args); err != nil {
		return capabilitiesOptions{}, err
	}
	if extra := fs.Args(); len(extra) > 0 {
		return capabilitiesOptions{}, fmt.Errorf("多余的参数: %s（用法: %s）",
			strings.Join(extra, " "), capabilitiesUsage)
	}
	return capabilitiesOptions{dataDir: *dataDir, json: *jsonOut}, nil
}

// runCapabilities 是 `anotify-agent capabilities` 的实现。
//
// 返回 (退出码, error)：error 只用于**说明**（mainWith 打到 stderr 一次）。
// 空结果既不是 error 也不是失败：退出码 0。
func runCapabilities(args []string, stdout, stderr io.Writer) (int, error) {
	opts, err := parseCapabilitiesFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, nil // -h：usage 已由 flag 包写到 stderr
		}
		return exitFailure, withKind(err, kindUsage, hintUsage)
	}
	mode := modeFor(stdout, opts.json)

	dataDir, err := resolveDataDir(opts.dataDir)
	if err != nil {
		return exitFailure, withKind(err, kindConfigInvalid, hintConfigInvalid)
	}
	infos, err := listCapabilities(dataDir)
	if err != nil {
		return exitFailure, withKind(err, kindConfigInvalid, hintConfigInvalid)
	}

	if mode == outJSON {
		// 写 stdout 失败只可能是管道被关（`| head`）：查询已经成功，不改退出码。
		_ = writeJSONLine(stdout, capabilitiesJSON{Capabilities: infos})
		return exitOK, nil
	}
	printCapabilitiesTable(stdout, infos, dataDir)
	return exitOK, nil
}

// listCapabilities 扫索引目录，返回本机缓存的全部产物（按 name、再 version 排序）。
//
// 口径与 cache.applied 一致：**解码不出来的索引项直接跳过**（节点确实跑不了它，
// 中枢会按 hello.applied 的差值重推）。索引目录不存在 = 空列表（不是错误）：
// 一个从没部署过东西的节点就是这种状态。
func listCapabilities(dataDir string) ([]capabilityInfo, error) {
	c := newCache(dataDir)
	entries, err := os.ReadDir(c.indexDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []capabilityInfo{}, nil
		}
		return nil, fmt.Errorf("读索引目录 %s: %w", c.indexDir(), err)
	}

	out := make([]capabilityInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue // index/ 下只认普通文件（名字就是 <name>@<version>）
		}
		name, version, ok := parseIndexName(e.Name())
		if !ok {
			continue
		}
		digest, ok := c.readIndex(name, version)
		if !ok {
			continue
		}
		art, contentDigest, ok := c.getByDigest(digest)
		if !ok {
			// 索引指着的产物读不出来/解不开：如实不列（与 applied 同一口径）。
			continue
		}
		out = append(out, capabilityInfo{
			Name:    art.Metadata.Name,
			Version: art.Metadata.Version,
			Outputs: append([]string{}, art.Spec.Outputs...),
			Digest:  contentDigest,
			Dir:     c.artifactDir(digest),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Version < out[j].Version
	})
	return out, nil
}

// shortDigest 是表里那一列：去掉 "sha256:" 前缀后的前 12 位十六进制。
//
// 完整值在 `--json` 里；表里的用途只是「与日志/中枢上的那一版肉眼对上」。
func shortDigest(digest string) string {
	s := strings.TrimPrefix(strings.TrimSpace(digest), "sha256:")
	if s == "" {
		return "-"
	}
	if len(s) > 12 {
		s = s[:12]
	}
	return s
}

// capabilityOutputs 把 outputs 渲染成表格里的一格：逗号分隔，空集合打 `-`。
func capabilityOutputs(outputs []string) string {
	if len(outputs) == 0 {
		return "-"
	}
	return strings.Join(outputs, ",")
}

// printCapabilitiesTable 打三列表：NAME@VER / OUTPUTS / DIGEST。
//
// 空结果也打一段说明（而不是什么都不输出）：人看到空屏会以为命令坏了，
// 而这条命令的语义正是「空是合法答案」。
func printCapabilitiesTable(w io.Writer, infos []capabilityInfo, dataDir string) {
	if len(infos) == 0 {
		fmt.Fprintf(w, "（没有缓存的产物：%s 下还没有任何索引条目）\n", newCache(dataDir).indexDir())
		fmt.Fprintln(w, "（这是空结果，不是查询失败：退出码 0；连上中枢部署过能力之后才会有。）")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME@VER\tOUTPUTS\tDIGEST")
	for _, info := range infos {
		fmt.Fprintf(tw, "%s@%d\t%s\t%s\n",
			info.Name, info.Version, capabilityOutputs(info.Outputs), shortDigest(info.Digest))
	}
	_ = tw.Flush() // 写 stdout 的失败只可能是管道被关（`| head`），不该变成退出码

	fmt.Fprintf(w, "\n（列出 %d 个能力 · 产物目录 %s · DIGEST 是内容哈希前 12 位，"+
		"完整值用 --json）\n", len(infos), newCache(dataDir).artifactsDir())
}
