package main

// 本文件是节点侧的**执行回看子命令**：`anotify-agent runs` 与 `anotify-agent show`。
//
// 落盘那一半在 runstore.go（`<data-dir>/runs/`），这里只做「把盘上已有的东西讲给人听」：
//
//	anotify-agent runs [--data-dir DIR] [--limit N]   最近 N 条（缺省 20），时间倒序
//	anotify-agent runs --trace <traceId>              该 trace 的全部执行，按执行顺序
//	anotify-agent runs --capability <x@2>             该能力的历次执行，时间倒序
//	anotify-agent show <trace>/<capability>@<ver>/<seq>-a<n>
//
// # 为什么退出码是 1 而不是 0（空结果不是崩溃）
//
// 「没有执行记录」在 shell 里确实不是错误，但它几乎总是**用户判断错了**：目录写错、
// 任务根本没跑起来、`--trace` 抄错了 id。用 0 会让 `anotify-agent runs && ...` 在
// 什么都没找到时照样往下走，用户看到的是下一条命令的报错。所以：
//
//	找到 = 0；空结果 / ref 不存在 / 参数非法 = 1（并在 stderr 说清是哪种）。
//
// # 为什么是表格而不是 JSON
//
// 这个命令的读者是人（运维、能力作者、被叫来排障的人）。他们的问题是「这条 trace
// 跑过几次、都成没成」，表格一眼答完；JSON 要再配一次 jq。机器读取的场景（脚本要
// 判断成败）由**退出码**回答，不需要把输出做成两种形状。
//
// `show` 里 input 段仍然是缩进 JSON：那一段的用途就是「原样看清当时收到了什么」，
// 转成表格反而会丢参数结构。

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

const (
	// defaultRunsLimit 是 `runs` 的缺省条数：「最近 20 条」够回答「刚才那几次跑得怎么样」，
	// 又不至于把 2000 个档案一次性糊满终端。
	// （子命令名 runsCommand / showCommand 在 main.go 里，与其它子命令放在一起。）
	defaultRunsLimit = 20

	// runTimeLayout 是给人看的时间格式（**本地**时区）：精确到秒就够——
	// 同一 trace 内的先后由 SEQ 列回答，不靠时间戳去猜。
	runTimeLayout = "2006-01-02 15:04:05"

	// runStatusUnknown 是 meta.json 读不出来时的 STATUS 占位。
	// 它有意义：一条执行落了半份（写盘中途崩了）就是这样子，看到 `-` 该去 show 里看提示。
	runStatusUnknown = "-"
)

// runsMode 是 `runs` 的三种视图（决定排序、是否截断、footer 措辞）。
type runsMode int

const (
	// runsModeRecent：不带过滤 = 最近 N 条，时间倒序。
	runsModeRecent runsMode = iota
	// runsModeTrace：--trace = 这条 trace 的全部执行，**时间正序**（按执行顺序读）。
	runsModeTrace
	// runsModeCapability：--capability = 这个能力的历次执行，时间倒序。
	runsModeCapability
)

// runsOptions 是 `runs` 子命令解析出来的参数。
type runsOptions struct {
	dataDir string // --data-dir：缺省空串 = ~/.anotify（resolveDataDir 归一）
	limit   int    // --limit：0 = 不限条数
	trace   string // --trace：精确匹配 traceId

	// 能力过滤：用户写 `x` 或 `x@2`，拆成名字 + 版本（version <= 0 = 不限版本，见 runFilter.Version）。
	rawCap     string // 用户原样给的字符串（错误信息与 footer 里照原样回显）
	capName    string
	capVersion int
}

// parseRunsFlags 解析 `runs` 子命令的参数。
//
// 与其它子命令同样用 ContinueOnError：退出码只由 runRuns 决定，flag 包只负责把
// usage 写到 stderr（`-h` 由调用方翻成 0）。
func parseRunsFlags(args []string, stderr io.Writer) (runsOptions, error) {
	fs := flag.NewFlagSet("agent runs", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dataDir := fs.String("data-dir", "", "数据目录（默认 ~/.anotify）：读它下面的 runs/")
	limit := fs.Int("limit", defaultRunsLimit, "最近多少条（0 = 不限；缺省 20）")
	trace := fs.String("trace", "", "只看这条 trace 的全部执行（按执行顺序，从早到晚）")
	capability := fs.String("capability", "",
		"只看这个能力的历次执行（<capability> 或 <capability>@<version>）")

	if err := fs.Parse(args); err != nil {
		return runsOptions{}, err
	}
	if extra := fs.Args(); len(extra) > 0 {
		return runsOptions{}, fmt.Errorf("多余的参数: %s（用法: %s）",
			strings.Join(extra, " "), runsUsage)
	}
	if *limit < 0 {
		// 负数没有合理解释（0 已经表示"不限"），当参数非法处理，别默默当成 0。
		return runsOptions{}, fmt.Errorf("--limit 不能为负数（0 = 不限）: %d", *limit)
	}

	out := runsOptions{
		dataDir:    *dataDir,
		limit:      *limit,
		trace:      strings.TrimSpace(*trace),
		capVersion: -1,
	}
	raw := strings.TrimSpace(*capability)
	if raw == "" {
		return out, nil
	}
	name, version, ok := parseCapVersion(raw)
	switch {
	case ok:
		out.rawCap, out.capName, out.capVersion = raw, name, version
	case strings.Contains(raw, "@"):
		// `x@` / `x@abc` 这类：形状错，别当成"没有 2 版"静默返回空列表。
		return runsOptions{}, fmt.Errorf(
			"--capability %q 的版本部分不是非负整数（形状: <capability> 或 <capability>@<version>）", raw)
	default:
		// 不带 @ = 这个能力的**所有**版本（版本过滤关闭）。
		out.rawCap, out.capName, out.capVersion = raw, raw, -1
	}
	return out, nil
}

// runsUsage 是 `runs` / `show` 的用法串（错误信息与 -h 都指向它）。
const runsUsage = "anotify-agent runs [--data-dir DIR] [--limit N] [--trace ID] [--capability NAME[@VER]]" +
	" / anotify-agent show <trace>/<capability>@<version>/<seq>-a<n> [--data-dir DIR]"

// mode 判断这次是哪种视图。
//
// 两个过滤同时给（`--trace X --capability Y`）按 trace 视图处理：它是交集，
// 且「按执行顺序读一条 trace」是更需要顺序的那一种。
func (o runsOptions) mode() runsMode {
	switch {
	case o.trace != "":
		return runsModeTrace
	case o.capName != "":
		return runsModeCapability
	default:
		return runsModeRecent
	}
}

// filter 把参数翻成 runStore.List 的过滤条件。
//
// Version 的零值是 0（= 只要 0 版），所以**必须**显式写 -1：漏了它就会把
// 「不带 --capability 的 runs」静默过滤成"只看 0 版"，症状是空列表。
func (o runsOptions) filter() runFilter {
	f := runFilter{
		TraceID:    o.trace,
		Capability: o.capName,
		Version:    -1,
	}
	if o.capName != "" {
		f.Version = o.capVersion
	}
	// --limit 只作用于**倒序**视图（"最近 N 条"）。trace 视图要的是这条 trace 的全部
	// 执行，截断只会让人以为后面没有了（那正是排障时最不该发生的误导）。
	if o.mode() != runsModeTrace {
		f.Limit = o.limit
	}
	return f
}

// sortEntries 把 List 的结果调整成该视图的顺序。
func (o runsOptions) sortEntries(entries []runEntry) {
	if o.mode() != runsModeTrace {
		return // List 已经是时间倒序（最近的在前），正是"最近 N 条"要的形状
	}
	// trace 视图：时间正序。同一秒内的扇出（几路同时开跑）按 SEQ 排，
	// 否则「第 1 次」可能排到「第 2 次」后面，读起来就是错的。
	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].Started.Equal(entries[j].Started) {
			return entries[i].Started.Before(entries[j].Started)
		}
		if entries[i].Seq != entries[j].Seq {
			return entries[i].Seq < entries[j].Seq
		}
		return entries[i].Ref < entries[j].Ref
	})
}

// emptyResultError 是「一条都没找到」时的说明（退出码 1 的**理由**）。
//
// 三种视图分开说话：`--trace` 抄错 id 与「这台机器还没跑过任何任务」是两回事，
// 提示得不一样，用户才知道下一步该查哪里。
func (o runsOptions) emptyResultError(root string) error {
	switch o.mode() {
	case runsModeTrace:
		return fmt.Errorf("没有执行记录：trace %q 一次都没跑过（档案库 %s）", o.trace, root)
	case runsModeCapability:
		return fmt.Errorf("没有执行记录：能力 %q 一次都没跑过（档案库 %s）", o.rawCap, root)
	default:
		return fmt.Errorf("没有执行记录：%s 下还没有任何执行档案（节点跑过任务之后才会有）", root)
	}
}

// footer 是表格下面那一行说明（表格 → 空行 → footer，见 runRuns）。
//
// 措辞用「列出」而不是「共」：--limit 会截断，说「共 20 条」会把“还有更多”说成“就这些”。
func (o runsOptions) footer(n int, root string) string {
	switch o.mode() {
	case runsModeTrace:
		return fmt.Sprintf("（列出 %d 条，按执行顺序 · trace %s · 档案库 %s）", n, o.trace, root)
	case runsModeCapability:
		return fmt.Sprintf("（列出 %d 条，最近的在前 · 能力 %s · 档案库 %s）", n, o.rawCap, root)
	default:
		return fmt.Sprintf("（列出 %d 条，最近的在前 · --limit 调条数，0 = 不限 · 档案库 %s）", n, root)
	}
}

// runRuns 是 `anotify-agent runs` 的实现。
//
// 返回 (退出码, error)：error 只用于**说明**（mainWith 打到 stderr 一次），
// 退出码本身已经表达「找到没找到」。
func runRuns(args []string, stdout, stderr io.Writer) (int, error) {
	opts, err := parseRunsFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, nil // -h：usage 已由 flag 包写到 stderr
		}
		return exitFailure, err
	}

	dataDir, err := resolveDataDir(opts.dataDir)
	if err != nil {
		return exitFailure, err
	}
	store := newRunStore(dataDir, newLogger(stderr))

	entries, err := store.List(opts.filter())
	if err != nil {
		return exitFailure, err
	}
	if len(entries) == 0 {
		// 空列表不是崩溃：盘上确实没有，说清楚为什么，退出码 1（见文件头）。
		return exitFailure, opts.emptyResultError(store.Root())
	}

	opts.sortEntries(entries)
	printRunsTable(stdout, entries)
	fmt.Fprintf(stdout, "\n%s\n", opts.footer(len(entries), store.Root()))
	return exitOK, nil
}

// printRunsTable 打表格：表头 + 每行一条执行（tabwriter 对齐，宽度按内容自适应）。
//
// 列是 TIME TRACE CAPABILITY SEQ ATT STATUS（契约 §4）：
//   - SEQ 是 seqInTrace（同一 trace 同一能力的第几次执行），ATT 是派发世代；
//   - STATUS 为空时打 `-`（见 runStatusUnknown）；
//   - 目录名里的 ref 不打成单独一列（太长），但 `show` 要的 ref 正好由表里三段拼成：
//     TRACE + "/" + CAPABILITY + "/" + SEQ + "-a" + ATT（footer 给了档案库根目录）。
func printRunsTable(w io.Writer, entries []runEntry) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tTRACE\tCAPABILITY\tSEQ\tATT\tSTATUS")
	for _, e := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%s@%d\t%d\t%d\t%s\n",
			formatRunTime(e.Started), e.TraceID, e.Capability, e.Version,
			e.Seq, e.Attempt, runStatusText(e.Meta.Status))
	}
	_ = tw.Flush() // 写 stdout 的失败只可能是管道被关（`| head`），不该变成退出码
}

// runShow 是 `anotify-agent show` 的实现：把一次执行的输入、两个流、meta 摘要打全。
func runShow(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("agent show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data-dir", "", "数据目录（默认 ~/.anotify）：读它下面的 runs/")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, nil
		}
		return exitFailure, err
	}
	// ref 允许写在 flag 前后（与 `run` 子命令同一个做法：取出首个位置参数后重解析剩余）。
	rest := fs.Args()
	if len(rest) == 0 {
		return exitFailure, fmt.Errorf("用法: %s（ref 可以直接从 `anotify-agent runs` 抄）", runsUsage)
	}
	ref := rest[0]
	if len(rest) > 1 {
		if err := fs.Parse(rest[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return exitOK, nil
			}
			return exitFailure, err
		}
		if extra := fs.Args(); len(extra) > 0 {
			return exitFailure, fmt.Errorf("多余的参数: %s（show 只收一个 ref）", strings.Join(extra, " "))
		}
	}

	dir, err := resolveDataDir(*dataDir)
	if err != nil {
		return exitFailure, err
	}
	store := newRunStore(dir, newLogger(stderr))

	detail, err := store.Load(ref)
	if err != nil {
		if errors.Is(err, ErrRunNotFound) {
			// 两种「没找到」要说成两句不同的话：ref 形状不对是**用户抄错了**（改 ref），
			// 形状对但盘上没有是**这次执行不在**（换查法）。把 Load 的英文原因原样括在后面
			// 对用户没有帮助（它只是重复了一遍 ref），所以原因在这里自己判。
			if hint := runRefShapeHint(ref); hint != "" {
				return exitFailure, fmt.Errorf("ref %q 形状不对（%s）——用 `anotify-agent runs` 抄一个", ref, hint)
			}
			return exitFailure, fmt.Errorf("没找到 %s（%s 下没有这次执行）——用 `anotify-agent runs` 列出有哪些",
				ref, store.Root())
		}
		return exitFailure, err
	}

	printRunDetail(stdout, detail)
	return exitOK, nil
}

// runRefShapeHint 判断用户给的 ref 是不是 `<trace>/<capability>@<version>/<seq>-a<n>` 形状。
//
// 返回空串 = 形状对（那就是真的没这条执行）；否则返回一句“哪一段不对”。
// 真正的路径安全闸仍然是 Load（safeJoin + checkRelPath）——这里只负责把错误说得像人话，
// 不代替它做任何放行决定。
func runRefShapeHint(ref string) string {
	parts := strings.Split(strings.Trim(strings.TrimSpace(ref), "/"), "/")
	if len(parts) != 3 {
		return "应为 <trace>/<capability>@<version>/<seq>-a<n> 三段"
	}
	if _, _, ok := parseCapVersion(parts[1]); !ok {
		return "第二段应为 <capability>@<version>"
	}
	if _, _, ok := parseRunDirName(parts[2]); !ok {
		return "第三段应为 <seq>-a<attempt>"
	}
	return ""
}

// printRunDetail 打一次执行的全部内容：
//
//	== <ref> ==
//
//	--- input ---
//	{ 缩进 JSON }
//
//	--- stdout ---
//	<原文>
//
//	--- stderr ---
//	<原文>
//
//	--- meta ---
//	时间 / 状态 / 退出码 / summary / emit 次数 / 字节数 / 截断提示
//
// 段之间用空行隔开：这个输出经常被整段贴进 issue，分隔清楚才读得懂哪段是什么。
func printRunDetail(w io.Writer, d *runDetail) {
	fmt.Fprintf(w, "== %s ==\n", d.Ref)
	fmt.Fprintln(w)

	fmt.Fprintln(w, "--- input ---")
	if data, err := marshalIndent(d.Input); err != nil {
		fmt.Fprintf(w, "(input.json 读不出来: %v)\n", err)
	} else {
		_, _ = w.Write(data) // marshalIndent 自带末尾换行
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "--- stdout ---")
	writeRunStreamBody(w, d.Stdout)
	fmt.Fprintln(w)

	fmt.Fprintln(w, "--- stderr ---")
	writeRunStreamBody(w, d.Stderr)
	fmt.Fprintln(w)

	writeRunMetaSummary(w, d)
}

// writeRunStreamBody 原样写一个流（不做任何改写：档案就是原始字节）。
//
// 唯一处理是补齐末尾换行：不补的话下一段的 `--- stderr ---` 会紧贴在最后一行输出后面，
// 看起来像脚本自己打的。空流什么都不打（段头 + 空行已经表达了"这一段是空的"）。
func writeRunStreamBody(w io.Writer, body string) {
	if body == "" {
		return
	}
	_, _ = io.WriteString(w, body)
	if !strings.HasSuffix(body, "\n") {
		_, _ = io.WriteString(w, "\n")
	}
}

// writeRunMetaSummary 打 meta 摘要（时间/状态/退出码/summary/emit/字节数/截断提示）。
func writeRunMetaSummary(w io.Writer, d *runDetail) {
	m := d.Meta
	fmt.Fprintln(w, "--- meta ---")
	fmt.Fprintf(w, "时间: %s\n", formatRunTimeRange(d))
	if runMetaKnown(m) {
		fmt.Fprintf(w, "状态: %s（退出码 %d）\n", runStatusText(m.Status), m.ExitCode)
	} else {
		// 没有 meta.json = 这次执行没写完（Save 最后才写它），别把零值当成"退出码 0、成功"。
		fmt.Fprintln(w, "状态: 未知（meta.json 缺失或读不出来：这次执行可能没写完）")
	}
	fmt.Fprintf(w, "能力: %s@%d  seq=%d  attempt=%d\n", d.Capability, d.Version, d.Seq, d.Attempt)
	if taskID := strings.TrimSpace(m.TaskID); taskID != "" {
		fmt.Fprintf(w, "任务: %s\n", taskID)
	}
	if s := strings.TrimSpace(m.Summary); s != "" {
		fmt.Fprintf(w, "summary: %s\n", s)
	}
	if e := strings.TrimSpace(m.Error); e != "" {
		fmt.Fprintf(w, "错误: %s\n", e)
	}
	fmt.Fprintf(w, "emit: %d 次\n", m.Emits)

	// 字节数取 meta 里的**真实**值（截断前）；meta 缺失时退到盘上内容长度，
	// 至少不让人看到"0 字节"却明明有一屏输出。
	stdoutBytes, stderrBytes := m.StdoutBytes, m.StderrBytes
	if stdoutBytes == 0 && len(d.Stdout) > 0 {
		stdoutBytes = int64(len(d.Stdout))
	}
	if stderrBytes == 0 && len(d.Stderr) > 0 {
		stderrBytes = int64(len(d.Stderr))
	}
	fmt.Fprintf(w, "字节: stdout %d · stderr %d\n", stdoutBytes, stderrBytes)

	// 截断提示必须显眼：盘上只有头部，读的人若不知道，会把"少了一段"当成"脚本没输出"。
	if d.StdoutTruncated {
		fmt.Fprintf(w, "注意: stdout 已截断（盘上只有头部，完整输出共 %d 字节）\n", m.StdoutBytes)
	}
	if d.StderrTruncated {
		fmt.Fprintf(w, "注意: stderr 已截断（盘上只有头部，完整输出共 %d 字节）\n", m.StderrBytes)
	}
}

// runMetaKnown 报告 meta.json 是否读到了（身份字段或时间戳任一存在即可）。
func runMetaKnown(m runMeta) bool {
	return strings.TrimSpace(m.StartedAt) != "" || strings.TrimSpace(m.Status) != "" ||
		strings.TrimSpace(m.TraceID) != ""
}

// formatRunTime 归一成给人看的本地时间；零值打 `-`（不猜）。
func formatRunTime(v time.Time) string {
	if v.IsZero() {
		return "-"
	}
	return v.Local().Format(runTimeLayout)
}

// formatRunTimeRange 渲染「开始 → 结束（耗时）」。
//
// 结束时间只从 meta.json 取（目录 mtime 是"这个目录最后被碰过"，不等于执行结束），
// 取不到就打 `-`；开始时间退到目录 mtime（Load/List 已经这么兜过）。
func formatRunTimeRange(d *runDetail) string {
	start := formatRunTime(d.Started)
	finish := "-"
	if ts, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(d.Meta.FinishedAt)); err == nil {
		finish = formatRunTime(ts)
	}
	out := start + " → " + finish
	if d.Meta.DurationMs > 0 {
		out += fmt.Sprintf("（耗时 %dms）", d.Meta.DurationMs)
	}
	return out
}

// runStatusText 把 meta 里的状态翻成表格/摘要里那一列。
func runStatusText(status string) string {
	if s := strings.TrimSpace(status); s != "" {
		return s
	}
	return runStatusUnknown
}
