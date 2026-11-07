package main

// 本文件是**离线书写面**的第二条命令：`anotify-agent schema [node|capability|emit]`
// （issue #319 §2）。
//
// 它回答的问题与 check 互补：check 说「你写错了什么」，schema 说「本来该写成什么」。
// 在此之前那个答案只存在于 40 KB 的 DEVELOPMENT.md 与源码 struct 里，而 agent 拿到
// 一台机器上的二进制时两样都没有。
//
// 三条纪律：
//
//	只吐 JSON      stdout 是**恰好一份** JSON Schema（jq / ajv / python3 -m json.tool 直接吃），
//	               不加标题、不加提示语——那会把它变成「差不多能解析」的文本；
//	逐字节稳定     同一份代码两次输出相同（json.MarshalIndent 会排序 key），可以在 CI 里 diff；
//	不读不写       不读 node.yaml、不读数据目录、不连网、不落盘（与 --skill / --version 同一类开关）。
//
// schema 文档本身在 schema_doc.go（手写 + 一致性守卫），这里只做「选一份、美化、打出去」。

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// schemaUsage 是 `schema` 的用法串。
//
// 三个名字各是谁在这里就说清（而不是只列名字）：`schema node` 与 `schema capability`
// 的区别是「运维的 node.yaml」与「作者的能力 yaml」，选错一次就会照着错的文档写。
const schemaUsage = `anotify-agent schema [node|capability|emit] [--json]

  node      node.yaml 的形状：name / server / token / instances
  capability  能力 yaml 的形状：apiVersion / kind / metadata / spec（不给参数时的缺省）
  emit      出口契约的形状：$ANOTIFY_EMIT_PORTS / $ANOTIFY_TASKDIR/emit.json`

// schemaExitRows 是 `schema -h` 的退出码表。
var schemaExitRows = []exitRow{
	{code: "0", desc: "已输出（stdout 是一份 JSON Schema；两次输出逐字节相同）"},
	{code: "1", desc: "schema 名不认识 / 多余的位置参数", kind: kindUsage},
}

// schemaOptions 是 `schema` 解析出来的参数。
type schemaOptions struct {
	name string // node / capability / emit（空 = 缺省 capability，在 parse 里定好）
}

// parseSchemaFlags 解析 `schema` 的参数（0 或 1 个位置参数）。
//
// 位置参数允许写在 flag 前后（与 `check` / `run` / `show` 同一做法：取出首个位置参数
// 后再解析剩下的）：`anotify-agent schema node --json` 是最自然的写法，不能因为它报了
// 「多余参数」就让人以为 --json 坏了（全局开关写在子命令前后都得能用）。
//
// 注册一个**无效**的 --json 是刻意的：全局开关可能写在子命令前面
// （`anotify-agent --json schema node`，见 subcommandAt）。schema 的输出本来就是 JSON，
// 所以它没有额外语义，但必须能接受。
func parseSchemaFlags(args []string, stderr io.Writer) (schemaOptions, error) {
	fs := flag.NewFlagSet("agent schema", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { writeSubcommandHelp(fs, schemaUsage, schemaExitRows) }

	_ = fs.Bool("json", false, "对 schema 无额外效果：输出本来就是 JSON（只为兼容写在子命令前的全局 --json）")

	if err := fs.Parse(args); err != nil {
		return schemaOptions{}, err
	}
	rest := fs.Args()
	if len(rest) > 1 {
		if err := fs.Parse(rest[1:]); err != nil {
			return schemaOptions{}, err
		}
		if extra := fs.Args(); len(extra) > 0 {
			return schemaOptions{}, fmt.Errorf("多余的参数: %s（用法: %s）",
				strings.Join(extra, " "), schemaUsage)
		}
		rest = rest[:1]
	}

	opts := schemaOptions{name: schemaCapability}
	if len(rest) == 1 {
		opts.name = strings.TrimSpace(rest[0])
	}
	// 名字必须完全对上（不做前缀/大小写容忍）：`node` 与 `nodes` 差一个字母就是
	// 另一份形状，猜错了作者会照着错的文档写 yaml。
	if _, ok := schemaDoc[opts.name]; !ok {
		return schemaOptions{}, fmt.Errorf("不认识的 schema 名 %q（可用: %s）",
			opts.name, strings.Join(schemaNames(), " / "))
	}
	return opts, nil
}

// runSchema 是 `anotify-agent schema` 的实现。
func runSchema(args []string, stdout, stderr io.Writer) (int, error) {
	opts, err := parseSchemaFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, nil // -h：usage 已由 flag 包写到 stderr
		}
		return exitFailure, withKind(err, kindUsage, hintUsage)
	}

	out, err := renderSchema(schemaDoc[opts.name])
	if err != nil {
		// 内嵌文档本身坏了（改 schema_doc.go 时的低级错误）：这是**我们**的问题，
		// 不能让用户以为自己的 yaml 有问题，所以直说「内部 schema 损坏」。
		return exitFailure, withKind(fmt.Errorf("内嵌 schema %q 损坏: %w", opts.name, err),
			kindFailed, "这是 agent 自身的 bug（改 schema_doc.go 之后没跑测试），不是你的 yaml 有问题")
	}
	if _, err := stdout.Write(out); err != nil {
		// 管道被关（`| head`）：内容已经完整给过一遍，不改退出码（同 runs/show）。
		return exitOK, nil
	}
	return exitOK, nil
}

// renderSchema 把原始 JSON 规范成「缩进 2 空格、key 有序、末尾一个换行」的输出。
//
// 走 Unmarshal → Encoder 而不是直接把原始串打出去：后者依赖源文件里的空白与
// key 顺序，而「两次输出逐字节相同」是验收项（CI 里 diff schema 的人不该被格式化差异
// 糊住）。代价是输出的 key 顺序是字典序，与源码里的书写顺序无关——schema 的 key 顺序
// 不影响语义，稳定性更重要。
//
// SetEscapeHTML(false) 是为了 description 里的 `>=` / `~` 不被写成 `\u003e`：这份输出
// 是给人读的文档，不是 HTML 里嵌的数据。
func renderSchema(raw string) ([]byte, error) {
	var doc any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
