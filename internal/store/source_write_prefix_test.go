package store

// source_write_prefix_test.go 守住 #644 第 4 条的「前缀只有一处定义」。
//
// 背景：server 的错误投影要剪掉写路径前缀（`create source: ` 这类定位信息），曾经是**手抄
// 字面量**——store 把 `create source: ` 改成 `create source:`（少一个空格）或新增
// `replace source: `，剪裁表静默失配，用户又开始看到内部前缀而测试仍绿。
//
// 现在两侧引用同一组常量，于是这里只需要守一件事：**source.go 的写路径包装不许再出现字面量**。
// 出现即说明有人新写了 `fmt.Errorf("xxx source: …")` 而绕过了 `SourceWritePrefixes`。

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
	// `<动词> source: ` 形式的字面量包装。读路径（get/scan/list）不进入用户可见的错误体
	// （它们的错误最终都落在 `not found` / 500），所以只要求**写路径**走常量。
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
