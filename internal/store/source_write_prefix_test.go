package store

// source_write_prefix_test.go 守 #644 第 4 条：store 的写路径包装不许再出现字面量。出现即说明有人
// 新写了 `fmt.Errorf("xxx source: …")` 绕过 `SourceWritePrefixes`，剪裁表就此静默失配（改一个空格就够）。

import (
	"os"
	"regexp"
	"testing"
)

func TestSourceWritePrefixWrapsUseConstants(t *testing.T) {
	raw, err := os.ReadFile("source.go")
	if err != nil {
		t.Fatalf("读不了 source.go：%v", err)
	}
	// `<动词> source: ` 形式的字面量包装。读路径（get/scan/list）的错误最终都落在 `not found` / 500，不要求走常量。
	literal := regexp.MustCompile(`fmt\.Errorf\("([a-z]+ source: )`)
	readOnly := map[string]bool{"get source: ": true, "scan source: ": true, "list source: ": true}
	var offenders []string
	for _, m := range literal.FindAllStringSubmatch(string(raw), -1) {
		if readOnly[m[1]] {
			continue
		}
		offenders = append(offenders, m[1])
	}
	if len(offenders) > 0 {
		t.Fatalf("source.go 里还有写路径前缀字面量 %v：请改用 SourceWritePrefix* 常量并加进 SourceWritePrefixes",
			offenders)
	}

	// 常量表自身必须齐全且互不重复（新增写操作忘了登记 = 用户会看到该前缀）。
	seen := map[string]bool{}
	for _, prefix := range SourceWritePrefixes {
		if prefix == "" || !regexp.MustCompile(`^[a-z]+ source: $`).MatchString(prefix) {
			t.Fatalf("SourceWritePrefixes 里有形状不对的条目 %q（want `<动词> source: `）", prefix)
		}
		if seen[prefix] {
			t.Fatalf("SourceWritePrefixes 里有重复条目 %q", prefix)
		}
		seen[prefix] = true
	}
	for _, prefix := range []string{
		SourceWritePrefixCreate, SourceWritePrefixUpdate,
		SourceWritePrefixDelete, SourceWritePrefixInsert,
	} {
		if !seen[prefix] {
			t.Fatalf("常量 %q 没有登记进 SourceWritePrefixes（server 就不会剪它）", prefix)
		}
	}
}
