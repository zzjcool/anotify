package playbook

import (
	"strings"
	"testing"
)

// TestParseRejectsMultiDocument 守 #635：包内每个 YAML 文件只能一份文档（与 yamldoc 同口径）。
func TestParseRejectsMultiDocument(t *testing.T) {
	const secondCap = "\n---\napiVersion: anotify/v1\nkind: Capability\nmetadata: {name: second}\nspec: {}\n"
	cases := []struct {
		name string
		path string
		tail string
	}{
		{"入口", EntryFile, "\n---\nmetadata:\n  name: other\n"},
		{"能力", "capabilities/review.yaml", secondCap},
		{"源", "sources/on-push.yaml", secondCap},
		// 尾部分隔符同判据：作者写下 `---` 就是以为后面还有一份。
		{"能力：只有尾部分隔符", "capabilities/fixer.yaml", "\n---\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := newBundle().withStandard().files()
			files[c.path] += c.tail
			_, err := Parse(files)
			assertCode(t, err, CodeInvalidPlaybook)
			if !strings.Contains(err.Error(), c.path) {
				t.Fatalf("错误必须指名文件 %s，实际 %v", c.path, err)
			}
			if !strings.Contains(err.Error(), "一个文件只能一份文档") {
				t.Fatalf("错误必须说明「一个文件只能一份文档」，实际 %v", err)
			}
			if !strings.Contains(err.Error(), "行起含多份 YAML 文档") {
				t.Fatalf("错误必须给出第二份文档的起始行（照着改才知道拆哪），实际 %v", err)
			}
		})
	}
}

func TestParseRejectsMultiDocSecondDocSyntaxError(t *testing.T) {
	files := newBundle().withStandard().files()
	files["capabilities/review.yaml"] += "\n---\n[b: 2\n"
	_, err := Parse(files)
	assertCode(t, err, CodeInvalidPlaybook)
	if !strings.Contains(err.Error(), "capabilities/review.yaml") || !strings.Contains(err.Error(), "不是合法 YAML") {
		t.Fatalf("第二份文档的语法错必须报成 invalid_playbook（指名文件 + 语法错），实际 %v", err)
	}
}

func TestParseAcceptsSingleDocWithLeadingMarker(t *testing.T) {
	files := newBundle().withStandard().files()
	for _, p := range []string{EntryFile, "capabilities/review.yaml", "sources/on-push.yaml"} {
		files[p] = "---\n" + files[p]
	}
	if _, err := Parse(files); err != nil {
		t.Fatalf("开头 `---` 的单文档包不该被拒: %v", err)
	}
}
