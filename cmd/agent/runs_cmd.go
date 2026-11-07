package main

// 本文件是节点侧的**执行回看子命令**：`anotify-agent runs` 与 `anotify-agent show`。
//
// 落盘那一半在 runstore.go（`<data-dir>/runs/`），这里只做「把盘上已有的东西讲给人听」：
//
//	anotify-agent runs [--data-dir DIR] [--limit N]   最近 N 条（缺省 20），时间倒序
//	anotify-agent runs --trace <traceId|前缀>         该 trace 的全部执行，按执行顺序
//	anotify-agent runs --capability <x@2>             该能力的历次执行，时间倒序
//	anotify-agent show <trace|前缀>/<capability>@<ver>/<seq>-a<n>
//
// # 为什么退出码是 1 而不是 0（空结果不是崩溃）
//
// 「没有执行记录」在 shell 里确实不是错误，但它几乎总是**用户判断错了**：目录写错、
// 任务根本没跑起来、`--trace` 抄错了 id。用 0 会让 `anotify-agent runs && ...` 在
// 什么都没找到时照样往下走，用户看到的是下一条命令的报错。所以：
//
//	找到 = 0；空结果 / ref 不存在 / 参数非法 = 1（并在 stderr 说清是哪种）。
//
// 两档退出码是这两条命令**自己的契约**（issue #318「不做」）：前缀歧义（kind=
// ambiguous_ref）也退 1，只有 kind 区分。多的那一档会撬开所有 `runs && …` 脚本。
//
// # 表格给人，JSON 给管道（#318 §1）
//
// 两种读者，两个问题：人问「这条 trace 跑过几次、都成没成」，表格一眼答完；机器要的
// 是能 `jq` 的形状与能 switch 的 kind。所以 TTY 上照旧打表格，stdout 不是终端（管道 /
// 重定向）时自动输出 `{"runs":[…]}`，`--json` 可以显式选（见 cliout.go）。
//
// `show` 的 input 段在文本模式下仍然是缩进 JSON：那一段的用途就是「原样看清当时
// 收到了什么」，转成表格反而会丢参数结构。
//
// # 上下文预算（#318 §3）
//
//   - 表格里 TRACE 只打前 8 位（shortTraceRef），完整 id 只在 `--json` 里；
//   - `show` / `runs --trace` 接受**唯一前缀**（最少 8 字符；撞了列候选、退 1）；
//   - `show --tail N` / `--no-stdout` / `--no-stderr`：单流上限是 8 MiB（runner.go），
//     一次 dump 就能把 agent 的上下文顶穿，窗口粒度是必需品而不是讲究。

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
	trace   string // --trace：完整 traceId 或唯一前缀（解析后换回完整 id）
	json    bool   // --json：机器可读（缺省由 stdout 是否终端决定，见 cliout.go）

	// 能力过滤：用户写 `x` 或 `x@2`，拆成名字 + 版本（version <= 0 = 不限版本，见 runFilter.Version）。
	rawCap     string // 用户原样给的字符串（错误信息与 footer 里照原样回显）
	capName    string
	capVersion int
}

// runsUsage 是 `runs` 的用法串（错误信息与 -h 都指向它）。
const runsUsage = "anotify-agent runs [--data-dir DIR] [--limit N] [--trace ID|前缀] " +
	"[--capability NAME[@VER]] [--json]"

// showUsage 是 `show` 的用法串。
const showUsage = "anotify-agent show <trace|前缀>/<capability>@<version>/<seq>-a<n> " +
	"[--data-dir DIR] [--tail N] [--no-stdout] [--no-stderr] [--json]"

// runsExitRows / showExitRows 是 `-h` 里的退出码表。
//
// 只有 0/1 两档（文件头的契约）：空结果、ref 不存在、前缀歧义都是 1，区别在 kind。
var runsExitRows = []exitRow{
	{code: "0", desc: "找到（至少一条执行）"},
	{code: "1", desc: "没找到 / 参数非法 / 数据目录不可用（kind 区分是哪一种）",
		kind: "not_found" + " / " + kindAmbiguousRef + " / " + kindUsage +
			" / " + kindConfigMissing + " / " + kindConfigInvalid},
}

var showExitRows = []exitRow{
	{code: "0", desc: "这次执行存在，已输出"},
	{code: "1", desc: "ref 不存在 / 形状不对 / 前缀歧义 / 数据目录不可用",
		kind: "not_found" + " / " + kindAmbiguousRef + " / " + kindUsage +
			" / " + kindConfigMissing + " / " + kindConfigInvalid},
}

// parseRunsFlags 解析 `runs` 子命令的参数。
//
// 与其它子命令同样用 ContinueOnError：退出码只由 runRuns 决定，flag 包只负责把
// usage 写到 stderr（`-h` 由调用方翻成 0）。
func parseRunsFlags(args []string, stderr io.Writer) (runsOptions, error) {
	fs := flag.NewFlagSet("agent runs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { writeSubcommandHelp(fs, runsUsage, runsExitRows) }

	dataDir := fs.String("data-dir", "", "数据目录（默认 ~/.anotify）：读它下面的 runs/")
	limit := fs.Int("limit", defaultRunsLimit, "最近多少条（0 = 不限；缺省 20）")
	trace := fs.String("trace", "", "只看这条 trace 的全部执行（完整 id 或唯一前缀，最少 8 字符）")
	capability := fs.String("capability", "",
		"只看这个能力的历次执行（<capability> 或 <capability>@<version>）")
	jsonOut := fs.Bool("json", false,
		"机器可读输出：{\"runs\":[…]}（缺省：stdout 不是终端时自动开；TRACE 列只打前 8 位）")

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
		json:       *jsonOut,
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
// 提示得不一样，用户才知道下一步该查哪里（hint 同理，否则「用 runs 列出有哪些」
// 在「盘上一条都没有」时就变成了循环）。
func (o runsOptions) emptyResultError(root string) error {
	hint := o.emptyHint()
	switch o.mode() {
	case runsModeTrace:
		return withKind(fmt.Errorf(
			"没有执行记录：trace %q 一次都没跑过（档案库 %s）", o.trace, root), kindNotFound, hint)
	case runsModeCapability:
		return withKind(fmt.Errorf(
			"没有执行记录：能力 %q 一次都没跑过（档案库 %s）", o.rawCap, root), kindNotFound, hint)
	default:
		return withKind(fmt.Errorf(
			"没有执行记录：%s 下还没有任何执行档案（节点跑过任务之后才会有）", root), kindNotFound, hint)
	}
}

// emptyHint 是「一条都没找到」时的下一步（按视图分开：要查的东西不同）。
func (o runsOptions) emptyHint() string {
	switch o.mode() {
	case runsModeTrace:
		return "先用 `anotify-agent runs --json` 看这台机器上有哪些 trace（完整 id 或前 8 位都行）"
	case runsModeCapability:
		return "先用 `anotify-agent runs` 看这台机器跑过哪些能力（名字是产物里的 capability）"
	default:
		return "先确认 --data-dir 指的就是跑过任务的那台机器/目录（缺省 ~/.anotify）"
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
		return fmt.Sprintf("（列出 %d 条，最近的在前 · TRACE 列是前 %d 位，完整 id 用 --json · "+
			"--limit 调条数，0 = 不限 · 档案库 %s）", n, traceRefWidth, root)
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
		return exitFailure, withKind(err, kindUsage, hintUsage)
	}
	mode := modeFor(stdout, opts.json)

	dataDir, err := resolveDataDir(opts.dataDir)
	if err != nil {
		return exitFailure, withKind(err, kindConfigInvalid, hintConfigInvalid)
	}
	store := newRunStore(dataDir, newLogger(stderr))

	// --trace 接受唯一前缀（#318 §3）：解析成完整 id 之后，下游（过滤/footer）只见
	// 一种形状。没匹配到就保持原样，让下面按「一次都没跑过」去报——那句话比
	// 「前缀没匹配」更贴用户的实际处境。
	if opts.trace != "" {
		full, rerr := store.ResolveTrace(opts.trace)
		switch {
		case rerr == nil:
			opts.trace = full
		case isAmbiguousTrace(rerr):
			return exitFailure, withKind(rerr, kindAmbiguousRef, hintAmbiguousRef)
		case isPrefixTooShort(rerr):
			return exitFailure, rerr // kind/hint 已钉在错误上
		}
	}

	entries, err := store.List(opts.filter())
	if err != nil {
		return exitFailure, err
	}
	opts.sortEntries(entries)

	if mode == outJSON {
		return finishRunsJSON(stdout, store, opts, entries)
	}
	if len(entries) == 0 {
		// 空列表不是崩溃：盘上确实没有，说清楚为什么，退出码 1（见文件头）。
		return exitFailure, opts.emptyResultError(store.Root())
	}

	printRunsTable(stdout, entries)
	fmt.Fprintf(stdout, "\n%s\n", opts.footer(len(entries), store.Root()))
	return exitOK, nil
}

// runsJSON 是 `runs --json` 的形状：`{"runs":[…]}`。
//
// 空结果必须是 `[]` 而不是 `null`（空列表返回 [] 是仓库的 API 契约）：agent 拿
// `jq -e '.runs | type=="array"'` 判形状时，null 会被判成「命令坏了」而不是「没有记录」。
type runsJSON struct {
	Runs []runJSONEntry `json:"runs"`
}

// runJSONEntry 是列表里的一条：ref + meta.json 的全部字段。
//
// **直接嵌入 runMeta**，不另造字段名（issue §1）：盘上的 meta.json、`show --json`
// 的 meta、这里的每一条，三处是同一份形状——多一套名字就迟早会分叉。
type runJSONEntry struct {
	Ref string `json:"ref"`
	runMeta
}

// finishRunsJSON 打 `{"runs":[…]}` 并决定退出码。
//
// 空结果仍然退 1（语义不变），但 stdout 上是**合法的空数组**：agent 因此能分清
// 「没有记录」（stdout 有形状、stderr kind=not_found）与「命令自己出错」
// （stdout 什么都没有、stderr 是另一类 kind）。
func finishRunsJSON(w io.Writer, store *runStore, opts runsOptions, entries []runEntry) (int, error) {
	out := runsJSON{Runs: make([]runJSONEntry, 0, len(entries))}
	for _, e := range entries {
		out.Runs = append(out.Runs, runJSONEntry{Ref: e.Ref, runMeta: e.Meta})
	}
	// 写 stdout 失败只可能是管道被关（`| head`）：查询已经成功，不应因此
	// 把退出码改成「没找到」的那个 1（两者对 agent 的含义完全不同）。
	_ = writeJSONLine(w, out)
	if len(entries) == 0 {
		return exitFailure, opts.emptyResultError(store.Root())
	}
	return exitOK, nil
}

// printRunsTable 打表格：表头 + 每行一条执行（tabwriter 对齐，宽度按内容自适应）。
//
// 列是 TIME TRACE CAPABILITY SEQ ATT STATUS（契约 §4）：
//   - TRACE 只打前 8 位（shortTraceRef）：完整 id 在 `--json` 里，表格里那 28 个
//     用不上的字符全是 agent 的上下文预算；
//   - SEQ 是 seqInTrace（同一 trace 同一能力的第几次执行），ATT 是派发世代；
//   - STATUS 为空时打 `-`（见 runStatusUnknown）；
//   - `show` 要的 ref 由表里三段拼成：TRACE + "/" + CAPABILITY + "/" + SEQ + "-a" + ATT
//     （前缀就是给 `show` 用的，footer 给了档案库根目录）。
func printRunsTable(w io.Writer, entries []runEntry) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tTRACE\tCAPABILITY\tSEQ\tATT\tSTATUS")
	for _, e := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%s@%d\t%d\t%d\t%s\n",
			formatRunTime(e.Started), shortTraceRef(e.TraceID), e.Capability, e.Version,
			e.Seq, e.Attempt, runStatusText(e.Meta.Status))
	}
	_ = tw.Flush() // 写 stdout 的失败只可能是管道被关（`| head`），不该变成退出码
}

// showOptions 是 `show` 子命令解析出来的参数。
type showOptions struct {
	dataDir  string // --data-dir
	tail     int    // --tail：只看最后 N 行（0 = 不限）
	noStdout bool   // --no-stdout：只看元信息与输入
	noStderr bool   // --no-stderr
	json     bool   // --json：机器可读
}

// parseShowFlags 解析 `show` 的参数，并取出位置参数 ref。
//
// ref 允许写在 flag 前后（与 `run` 子命令同一个做法：取出首个位置参数后重解析剩余）。
func parseShowFlags(args []string, stderr io.Writer) (showOptions, string, error) {
	fs := flag.NewFlagSet("agent show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { writeSubcommandHelp(fs, showUsage, showExitRows) }

	dataDir := fs.String("data-dir", "", "数据目录（默认 ~/.anotify）：读它下面的 runs/")
	tail := fs.Int("tail", 0, "只看每个流的最后 N 行（0 = 不限；上下文预算不够时的第一根杠杆）")
	noStdout := fs.Bool("no-stdout", false, "不输出 stdout 段（只看元信息与输入）")
	noStderr := fs.Bool("no-stderr", false, "不输出 stderr 段")
	jsonOut := fs.Bool("json", false,
		"机器可读输出：{\"ref\",\"meta\",\"input\",\"stdout\",\"stderr\"}（缺省：stdout 不是终端时自动开）")

	if err := fs.Parse(args); err != nil {
		return showOptions{}, "", err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return showOptions{}, "", fmt.Errorf("用法: %s（ref 可以直接从 `anotify-agent runs` 抄）", showUsage)
	}
	ref := rest[0]
	if len(rest) > 1 {
		if err := fs.Parse(rest[1:]); err != nil {
			return showOptions{}, "", err
		}
		if extra := fs.Args(); len(extra) > 0 {
			return showOptions{}, "", fmt.Errorf("多余的参数: %s（show 只收一个 ref）", strings.Join(extra, " "))
		}
	}
	if *tail < 0 {
		return showOptions{}, "", fmt.Errorf("--tail 不能为负数（0 = 不限）: %d", *tail)
	}
	return showOptions{
		dataDir:  *dataDir,
		tail:     *tail,
		noStdout: *noStdout,
		noStderr: *noStderr,
		json:     *jsonOut,
	}, ref, nil
}

// runShow 是 `anotify-agent show` 的实现：把一次执行的输入、两个流、meta 摘要打全。
func runShow(args []string, stdout, stderr io.Writer) (int, error) {
	opts, ref, err := parseShowFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, nil
		}
		return exitFailure, withKind(err, kindUsage, hintUsage)
	}
	mode := modeFor(stdout, opts.json)

	dir, err := resolveDataDir(opts.dataDir)
	if err != nil {
		return exitFailure, withKind(err, kindConfigInvalid, hintConfigInvalid)
	}
	store := newRunStore(dir, newLogger(stderr))

	// 短 ref（#318 §3）：第一段允许是唯一前缀。歧义时**不输出任何正文**——
	// 列候选是为了让下一次调用能一次选对，而不是先糊一屏日志再报错。
	ref, err = resolveShowRef(store, ref)
	if err != nil {
		return exitFailure, err
	}

	detail, err := store.Load(ref)
	if err != nil {
		if errors.Is(err, ErrRunNotFound) {
			// 两种「没找到」要说成两句不同的话：ref 形状不对是**用户抄错了**（改 ref），
			// 形状对但盘上没有是**这次执行不在**（换查法）。把 Load 的英文原因原样括在后面
			// 对用户没有帮助（它只是重复了一遍 ref），所以原因在这里自己判。
			if shapeHint := runRefShapeHint(ref); shapeHint != "" {
				return exitFailure, withKind(fmt.Errorf(
					"ref %q 形状不对（%s）——用 `anotify-agent runs` 抄一个", ref, shapeHint),
					kindUsage, hintUsage)
			}
			return exitFailure, withKind(fmt.Errorf(
				"没找到 %s（%s 下没有这次执行）——用 `anotify-agent runs` 列出有哪些",
				ref, store.Root()), kindNotFound, hintNotFound)
		}
		return exitFailure, err
	}

	if mode == outJSON {
		return finishShowJSON(stdout, detail, opts)
	}
	printRunDetail(stdout, detail, opts)
	return exitOK, nil
}

// resolveShowRef 把 ref 的第一段（trace）换成唯一前缀解析出的完整 id。
//
// 只动**第一段**：能力名与 `<seq>-a<n>` 又短又可辨，没有前缀化的价值。第一段不是
// 有效前缀时原样返回，交给 Load 去报「形状不对 / 没这条执行」——那两句比这里更
// 贴用户的实际错误。
func resolveShowRef(store *runStore, ref string) (string, error) {
	clean := strings.Trim(strings.TrimSpace(ref), "/")
	parts := strings.Split(clean, "/")
	if len(parts) != 3 {
		return ref, nil // 形状错：Load 会说得更准（三段 / 第二段 / 第三段）
	}
	full, err := store.ResolveTrace(parts[0])
	if err != nil {
		switch {
		case isAmbiguousTrace(err):
			return "", withKind(err, kindAmbiguousRef, hintAmbiguousRef)
		case isPrefixTooShort(err):
			return "", err // kind/hint 已钉在错误上
		}
		return ref, nil // 盘上没有这条 trace：交给 Load 报 not_found
	}
	parts[0] = full
	return strings.Join(parts, "/"), nil
}

// isAmbiguousTrace 报告错误是不是「前缀撞上多条 trace」。
func isAmbiguousTrace(err error) bool {
	var amb *ambiguousTraceError
	return errors.As(err, &amb)
}

// isPrefixTooShort 报告错误是不是「前缀太短」。
func isPrefixTooShort(err error) bool {
	k, _ := kindOf(err)
	return k == kindUsage
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

// showJSON 是 `show --json` 的形状（issue §1 的五个键，一个不多一个不少）。
//
// Meta 是 runMeta 本体：验收要求「与磁盘 meta.json 逐字段相等」，所以这里**不能**
// 另造一套字段（连省略号都不能有——omitempty 会让空字段从形状里消失）。
type showJSON struct {
	Ref    string   `json:"ref"`
	Meta   runMeta  `json:"meta"`
	Input  runInput `json:"input"`
	Stdout string   `json:"stdout"`
	Stderr string   `json:"stderr"`
}

// finishShowJSON 打 `show --json` 并套用读输出窗口。
//
// 窗口（--tail / --no-*）对两种形状同样生效：上下文预算的问题与读的人是机器还是人无关。
func finishShowJSON(w io.Writer, d *runDetail, opts showOptions) (int, error) {
	out := showJSON{Ref: d.Ref, Meta: d.Meta, Input: d.Input}
	if !opts.noStdout {
		out.Stdout, _ = tailLines(d.Stdout, opts.tail)
	}
	if !opts.noStderr {
		out.Stderr, _ = tailLines(d.Stderr, opts.tail)
	}
	// 写 stdout 失败只可能是管道被关（`| head`）：不改退出码（同 finishRunsJSON）。
	_ = writeJSONLine(w, out)
	return exitOK, nil
}

// printRunDetail 打一次执行的全部内容：
//
//	== <ref> ==
//
//	--- input ---
//	{ 缩进 JSON }
//
//	--- stdout ---
//	<原文（或 --tail 的窗口）>
//
//	--- stderr ---
//	<原文>
//
//	--- meta ---
//	时间 / 状态 / 退出码 / summary / emit 次数 / 字节数 / 截断提示
//
// 段之间用空行隔开：这个输出经常被整段贴进 issue，分隔清楚才读得懂哪段是什么。
func printRunDetail(w io.Writer, d *runDetail, opts showOptions) {
	fmt.Fprintf(w, "== %s ==\n", d.Ref)
	fmt.Fprintln(w)

	fmt.Fprintln(w, "--- input ---")
	if data, err := marshalIndent(d.Input); err != nil {
		fmt.Fprintf(w, "(input.json 读不出来: %v)\n", err)
	} else {
		_, _ = w.Write(data) // marshalIndent 自带末尾换行
	}
	fmt.Fprintln(w)

	writeRunStreamSection(w, "stdout", d.Stdout, opts.tail, opts.noStdout)
	writeRunStreamSection(w, "stderr", d.Stderr, opts.tail, opts.noStderr)

	writeRunMetaSummary(w, d)
}

// writeRunStreamSection 打一段流：段头 + 正文 + 窗口提示 + 空行。
//
// 三种「没有正文」必须分清，否则读的人会把它们当成同一件事：
//   - --no-* ：是我让你别看（明确说省略）；
//   - 空流   ：脚本一个字都没写（段头 + 空行就够，加字反而像有内容）；
//   - 窗口   ：正文被 --tail 截了（说清掉了多少行，并给出看全文的办法）。
func writeRunStreamSection(w io.Writer, name, body string, tail int, skip bool) {
	fmt.Fprintf(w, "--- %s ---\n", name)
	switch {
	case skip:
		fmt.Fprintf(w, "（--no-%s：已省略；完整内容仍在盘上，字节数见 meta）\n", name)
	case body == "":
		// 空流什么都不打（段头 + 空行已经表达了"这一段是空的"）。
	default:
		shown, dropped := tailLines(body, tail)
		writeRunStreamBody(w, shown)
		if dropped > 0 {
			fmt.Fprintf(w, "注意: --tail %d 只显示最后 %d 行（另有 %d 行未显示；--tail 0 看全文）\n",
				tail, tail, dropped)
		}
	}
	fmt.Fprintln(w)
}

// writeRunStreamBody 原样写一个流（不做任何改写：档案就是原始字节）。
//
// 唯一处理是补齐末尾换行：不补的话下一段的 `--- stderr ---` 会紧贴在最后一行输出后面，
// 看起来像脚本自己打的。
func writeRunStreamBody(w io.Writer, body string) {
	if body == "" {
		return
	}
	_, _ = io.WriteString(w, body)
	if !strings.HasSuffix(body, "\n") {
		_, _ = io.WriteString(w, "\n")
	}
}

// tailLines 取最后 n 行；返回 (窗口内正文, 被丢掉的整行数)。
//
// n <= 0 = 不截（与 --limit 0 同一个约定：0 表示不限）。窗口按**行**而不是字节：
// agent 的逻辑单位是「最后几条报错」，不是「最后 4 KiB」。
func tailLines(s string, n int) (string, int) {
	if n <= 0 || s == "" {
		return s, 0
	}
	lines := splitLinesKeep(s)
	if len(lines) <= n {
		return s, 0
	}
	return strings.Join(lines[len(lines)-n:], ""), len(lines) - n
}

// splitLinesKeep 按换行切成「行 + 各自的换行」：拼回去逐字节等于原文。
//
// 不用 strings.Split：末尾换行会切出一个空元素，"最后 5 行"于是变成 4 行半。
func splitLinesKeep(s string) []string {
	out := make([]string, 0, 16)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
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
