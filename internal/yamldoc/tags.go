package yamldoc

import "regexp"

// yaml.v3 没有导出 tag 常量，这里按 `Node.ShortTag()` 返回的短名抄一份
// （短名形如 `!!str`，长名是 `tag:yaml.org,2002:str`）。
const (
	yamlStrTag       = "!!str"
	yamlIntTag       = "!!int"
	yamlFloatTag     = "!!float"
	yamlBoolTag      = "!!bool"
	yamlNullTag      = "!!null"
	yamlMapTag       = "!!map"
	yamlSeqTag       = "!!seq"
	yamlTimestampTag = "!!timestamp"
	yamlMergeTag     = "!!merge"
)

var (
	// linePattern 从 yaml.v3 的错误文本里取行号（`yaml: line 3: did not find expected key`）。
	linePattern = regexp.MustCompile(`line (\d+)`)
	// linePrefixPattern 去掉错误文本里重复的行号前缀（行号已进 Problem.Line）。
	linePrefixPattern = regexp.MustCompile(`^line \d+: `)
	// backtickPattern 抹掉反引号里的片段（yaml.v3 的 TypeError 会带标量值）。
	backtickPattern = regexp.MustCompile("`[^`]*`")
	// plainIntPattern 是「十进制整数字面量」——只有它才原文照抄进 JSON。
	plainIntPattern = regexp.MustCompile(`^-?[0-9]+$`)
)
