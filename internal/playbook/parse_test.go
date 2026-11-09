package playbook

import (
	"strings"
	"testing"
)

// TestParseValidPackage 验一个合法包能被解析出全部身份字段（含未引用的 README.md 豁免）。
func TestParseValidPackage(t *testing.T) {
	b := newBundle().withStandard().extra("README.md", "# 说明\n")
	files := b.files()
	pkg, err := Parse(files)
	if err != nil {
		t.Fatalf("合法包不该被拒: %v", err)
	}
	if pkg.Name != "project-chain" {
		t.Fatalf("包名 = %q，期望 project-chain", pkg.Name)
	}
	if pkg.DisplayName != "单项目审查链" || pkg.Version != 3 || pkg.Description != "测试包" {
		t.Fatalf("metadata 解析丢了: %+v", pkg)
	}
	if got, want := len(pkg.CapabilityPaths), 2; got != want {
		t.Fatalf("能力清单 = %v，期望 %d 项", pkg.CapabilityPaths, want)
	}
	if got, want := len(pkg.SourcePaths), 1; got != want {
		t.Fatalf("源清单 = %v，期望 %d 项", pkg.SourcePaths, want)
	}
	// Files 必须原样保留（store 直接落库、includeBody 原样返回）。
	for path, body := range files {
		if pkg.Files[path] != body {
			t.Fatalf("Files[%q] 与上传原文不一致", path)
		}
	}
	if !strings.Contains(string(pkg.Values), `"properties"`) {
		t.Fatalf("spec.values 没解析进来: %s", pkg.Values)
	}
}

// TestParseNoValuesSchema 验 spec.values 省略 → `{}`（接受一切、无默认值）。
func TestParseNoValuesSchema(t *testing.T) {
	b := newBundle().setValues("")
	pkg := b.pkg(t)
	if strings.TrimSpace(string(pkg.Values)) != "{}" {
		t.Fatalf("spec.values 省略时应归一成 {}，实际 %s", pkg.Values)
	}
}

// TestParseEntryMissing 验缺入口 = entry_missing（一个包没有 playbook.yaml 就什么都不是）。
func TestParseEntryMissing(t *testing.T) {
	b := newBundle().withStandard()
	files := b.files()
	delete(files, EntryFile)
	_, err := Parse(files)
	assertCode(t, err, CodeEntryMissing)
}

// TestParseUnreferencedYAML 验包内未引用的 YAML = file_unreferenced（静默漏装是最怕的形态）。
func TestParseUnreferencedYAML(t *testing.T) {
	b := newBundle().withStandard().extra("capabilities/orphan.yaml", "apiVersion: anotify/v1\nkind: Capability\n")
	err := b.pkgErr(t)
	assertCode(t, err, CodeFileUnreferenced)
	if !strings.Contains(err.Error(), "capabilities/orphan.yaml") {
		t.Fatalf("错误消息应该点名路径: %v", err)
	}
}

// TestParseDuplicateReference 验同一文件被列两次 = file_duplicate。
func TestParseDuplicateReference(t *testing.T) {
	b := newBundle().cap("capabilities/fixer.yaml", fixerDoc)
	// 同一路径在 capabilities 里出现两次。
	b.entryOverride = "apiVersion: anotify/v1\nkind: Playbook\nmetadata:\n  name: project-chain\nspec:\n" +
		"  capabilities:\n    - capabilities/fixer.yaml\n    - capabilities/fixer.yaml\n"
	assertCode(t, b.pkgErr(t), CodeFileDuplicate)

	// 跨槽位重复同样拒（一个文件只能占一个槽位）。
	b2 := newBundle().cap("capabilities/fixer.yaml", fixerDoc)
	b2.entryOverride = "apiVersion: anotify/v1\nkind: Playbook\nmetadata:\n  name: project-chain\nspec:\n" +
		"  capabilities:\n    - capabilities/fixer.yaml\n" +
		"  sources:\n    - capabilities/fixer.yaml\n"
	assertCode(t, b2.pkgErr(t), CodeFileDuplicate)
}

// TestParseFilePathInvalid 验路径形态规则（§1.3：相对包根、无 ..、≤255、无 NUL）。
//
// 两个层次都测：`pathProblem` 的纯规则（含 NUL 这种 YAML 写不出来的字符）+ 走 Parse 的接线。
func TestParseFilePathInvalid(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"absolute", "/etc/passwd.yaml"},
		{"parent", "../evil.yaml"},
		{"nested_parent", "capabilities/../evil.yaml"},
		{"backslash", `capabilities\evil.yaml`},
		{"nul", "capabilities/a\x00.yaml"},
		{"empty_segment", "capabilities//a.yaml"},
		{"dot_segment", "capabilities/./a.yaml"},
		{"too_long", "capabilities/" + strings.Repeat("a", MaxPathLen) + ".yaml"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run("rule_"+tc.name, func(t *testing.T) {
			if msg := pathProblem(tc.path); msg == "" {
				t.Fatalf("路径 %q 应该被判非法", tc.path)
			}
		})
	}
	// 接线：清单里写了非法路径就是 file_path_invalid（而不是 file_missing / file_unreferenced）。
	for _, tc := range cases {
		if strings.ContainsRune(tc.path, 0) || tc.path == "" {
			continue // 含 NUL / 空路径没法写进 YAML 清单，规则层已覆盖
		}
		t.Run("parse_"+tc.name, func(t *testing.T) {
			b := newBundle()
			b.entryOverride = "apiVersion: anotify/v1\nkind: Playbook\nmetadata:\n  name: project-chain\nspec:\n" +
				"  capabilities:\n    - " + tc.path + "\n"
			files := b.files()
			files[tc.path] = "kind: Capability\n"
			_, err := Parse(files)
			assertCode(t, err, CodeFilePathInvalid)
		})
	}
}

// TestParseFileMissing 验引用了包里没有的文件 = file_missing。
func TestParseFileMissing(t *testing.T) {
	b := newBundle().cap("capabilities/review.yaml", reviewDoc)
	files := b.files()
	delete(files, "capabilities/review.yaml")
	_, err := Parse(files)
	assertCode(t, err, CodeFileMissing)
}

// TestParseNamespaceInPackage 验 metadata.namespace 在入口与被引用文档里都禁止。
func TestParseNamespaceInPackage(t *testing.T) {
	t.Run("entry", func(t *testing.T) {
		b := newBundle().withStandard()
		b.entryOverride = "apiVersion: anotify/v1\nkind: Playbook\nmetadata:\n  name: project-chain\n  namespace: prod\nspec:\n  capabilities:\n    - capabilities/review.yaml\n"
		err := b.pkgErr(t)
		assertCode(t, err, CodeNamespaceInPackage)
	})
	t.Run("capability", func(t *testing.T) {
		b := newBundle().cap("capabilities/review.yaml",
			"apiVersion: anotify/v1\nkind: Capability\nmetadata:\n  name: r\n  namespace: prod\nspec:\n  next: []\n")
		err := b.pkgErr(t)
		assertCode(t, err, CodeNamespaceInPackage)
	})
	t.Run("source", func(t *testing.T) {
		b := newBundle().source("sources/on-push.yaml",
			"metadata:\n  name: s\n  namespace: prod\nspec:\n  kind: manual\n")
		err := b.pkgErr(t)
		assertCode(t, err, CodeNamespaceInPackage)
	})
}

// TestParseSlotMismatch 验「同一个文件放错槽位 = invalid_playbook（消息指向路径）」。
func TestParseSlotMismatch(t *testing.T) {
	t.Run("source_in_capability_slot", func(t *testing.T) {
		b := newBundle().cap("capabilities/wrong.yaml",
			"kind: Source\nmetadata:\n  name: s\nspec:\n  kind: manual\n")
		err := b.pkgErr(t)
		assertCode(t, err, CodeInvalidPlaybook)
		if !strings.Contains(err.Error(), "capabilities/wrong.yaml") {
			t.Fatalf("错误消息应该点名路径: %v", err)
		}
	})
	t.Run("capability_in_source_slot", func(t *testing.T) {
		b := newBundle().source("sources/wrong.yaml",
			"kind: Capability\nmetadata:\n  name: c\nspec:\n  next: []\n")
		err := b.pkgErr(t)
		assertCode(t, err, CodeInvalidPlaybook)
	})
	t.Run("source_without_kind", func(t *testing.T) {
		b := newBundle().source("sources/wrong.yaml", "metadata:\n  name: s\nspec:\n  config: {}\n")
		err := b.pkgErr(t)
		assertCode(t, err, CodeInvalidPlaybook)
	})
}

// TestParseUnknownFields 验入口的未知字段是错误（拼错的键静默失效最难查）。
func TestParseUnknownFields(t *testing.T) {
	cases := map[string]string{
		"top_level": "apiVersion: anotify/v1\nkind: Playbook\nmetadata:\n  name: project-chain\nspec: {}\nextra: 1\n",
		"metadata":  "apiVersion: anotify/v1\nkind: Playbook\nmetadata:\n  name: project-chain\n  oops: 1\nspec: {}\n",
		"spec":      "apiVersion: anotify/v1\nkind: Playbook\nmetadata:\n  name: project-chain\nspec:\n  capability: []\n",
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			b := newBundle()
			b.entryOverride = entry
			err := b.pkgErr(t)
			assertCode(t, err, CodeInvalidPlaybook)
			if !strings.Contains(err.Error(), "未知字段") {
				t.Fatalf("错误消息应该点未知字段: %v", err)
			}
		})
	}
}

// TestParseSlugInvalid 验包名正则与长度（§1.1）。
func TestParseSlugInvalid(t *testing.T) {
	for _, slug := range []string{"Project", "pro_ject", "pro--ject", "-p", "p-", "", strings.Repeat("a", MaxNameLen+1)} {
		t.Run(slug, func(t *testing.T) {
			b := newBundle()
			b.slug = slug
			err := b.pkgErr(t)
			assertCode(t, err, CodeSlugInvalid)
		})
	}
}

// TestParseShapeErrors 验入口形状（values/capabilities/version 的类型）。
func TestParseShapeErrors(t *testing.T) {
	cases := []struct {
		name  string
		entry string
	}{
		{"values_not_object", "apiVersion: anotify/v1\nkind: Playbook\nmetadata:\n  name: p\nspec:\n  values: [1,2]\n"},
		{"capabilities_not_list", "apiVersion: anotify/v1\nkind: Playbook\nmetadata:\n  name: p\nspec:\n  capabilities: nope\n"},
		{"version_zero", "apiVersion: anotify/v1\nkind: Playbook\nmetadata:\n  name: p\n  version: 0\nspec: {}\n"},
		{"kind_mismatch", "apiVersion: anotify/v1\nkind: Source\nmetadata:\n  name: p\nspec: {}\n"},
		{"api_version_mismatch", "apiVersion: anotify/v9\nkind: Playbook\nmetadata:\n  name: p\nspec: {}\n"},
		{"no_metadata", "apiVersion: anotify/v1\nkind: Playbook\nspec: {}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBundle()
			b.entryOverride = tc.entry
			assertCode(t, b.pkgErr(t), CodeInvalidPlaybook)
		})
	}
}

// TestParseLimits 验 §1.5 的三条上界（文件数 / 单文件 / YAML 合计）与能力·源数量上限。
func TestParseLimits(t *testing.T) {
	t.Run("too_many_files", func(t *testing.T) {
		b := newBundle().cap("capabilities/a.yaml", fixerDoc)
		files := b.files()
		for i := 0; len(files) <= MaxFiles; i++ {
			files["notes/x"+string(rune('a'+i%26))+string(rune('a'+i/26))+".txt"] = "x"
		}
		_, err := Parse(files)
		assertCode(t, err, CodeTooManyItems)
	})
	t.Run("too_many_capabilities", func(t *testing.T) {
		b := newBundle()
		for i := 0; i <= MaxCapabilities; i++ {
			b.cap("capabilities/c"+string(rune('a'+i%26))+string(rune('a'+i/26))+".yaml", fixerDoc)
		}
		err := b.pkgErr(t)
		assertCode(t, err, CodeTooManyItems)
	})
	t.Run("too_many_sources", func(t *testing.T) {
		b := newBundle()
		for i := 0; i <= MaxSources; i++ {
			b.source("sources/s"+string(rune('a'+i%26))+string(rune('a'+i/26))+".yaml",
				"metadata:\n  name: s\nspec:\n  kind: manual\n")
		}
		err := b.pkgErr(t)
		assertCode(t, err, CodeTooManyItems)
	})
	t.Run("file_too_large", func(t *testing.T) {
		b := newBundle().cap("capabilities/a.yaml", fixerDoc)
		files := b.files()
		files["blob.txt"] = strings.Repeat("x", MaxFileBytes+1)
		_, err := Parse(files)
		assertCode(t, err, CodeInvalidPlaybook)
	})
	t.Run("yaml_too_large", func(t *testing.T) {
		b := newBundle()
		// 5 个 2 MiB 的文件：每个都没超单文件上限，但 YAML 合计超 8 MiB。
		for i := 0; i < 5; i++ {
			b.cap("capabilities/c"+string(rune('a'+i))+".yaml",
				fixerDoc+"# "+strings.Repeat("y", MaxFileBytes-200)+"\n")
		}
		err := b.pkgErr(t)
		assertCode(t, err, CodeInvalidPlaybook)
	})
}

// TestParseFileKeyPathEscape 验包体自身的键也是路径：非引用文件（.txt）的路径逃逸同样拒。
func TestParseFileKeyPathEscape(t *testing.T) {
	b := newBundle()
	files := b.files()
	files["../escape.txt"] = "x"
	_, err := Parse(files)
	assertCode(t, err, CodeFilePathInvalid)

	files2 := b.files()
	files2["notes\\windows.txt"] = "x"
	_, err = Parse(files2)
	assertCode(t, err, CodeFilePathInvalid)
}

// TestParseYAMLRequired 验被引用文件本身必须是合法 YAML 映射。
func TestParseYAMLRequired(t *testing.T) {
	b := newBundle().cap("capabilities/a.yaml", "\t- broken: [\n")
	assertCode(t, b.pkgErr(t), CodeInvalidPlaybook)
}

// TestParseCapabilityNameMayBeTemplate 验创建期**不**判名字正则（参数化包必须能存进去）。
func TestParseCapabilityNameMayBeTemplate(t *testing.T) {
	pkg := newBundle().withStandard().pkg(t)
	if len(pkg.CapabilityPaths) != 2 {
		t.Fatalf("能力清单 = %v", pkg.CapabilityPaths)
	}
}

// assertCode 断言错误的领域码（并打印真实错误，便于排查）。
func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误码 %s，实际没有错误", want)
	}
	if got := Code(err); got != want {
		t.Fatalf("错误码 = %q（%v），期望 %q", got, err, want)
	}
}
