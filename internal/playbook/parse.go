package playbook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// slugPattern 是包名（metadata.name）的正则（playbook.md §1.1：命名空间内唯一、URL 里那段）。
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Parse 解析一个包的文件集合，做**创建期**校验（值无关；playbook.md §9.1 / I-P2）。
//
// 创建期只判"与 values 无关"的东西：
//
//	入口存在、引用闭合（存在 / 不重复 / 无未引用 YAML）、路径合法、
//	包的形状（entry / metadata / spec.values / 引用清单）、槽位形状、
//	metadata.namespace 禁止、计数与体积上界。
//
// **刻意不做**的事（写了就是 bug，见 §9.1 的硬约束）：不跑单建校验、不判名字正则、
// 不解析 next ——此刻能力名可能还是 `{{values.project}}-review`，任何"当文档校验"的
// 逻辑都会让**所有**参数化包存不进来。
//
// files 的语义是 `{相对路径: 正文}`；返回的 Package 保留 Files 原文（store 直接落库）。
func Parse(files map[string]string) (*Package, error) {
	if files == nil {
		files = map[string]string{}
	}
	entry, ok := files[EntryFile]
	if !ok {
		return nil, errf(CodeEntryMissing,
			"包入口 %s 不存在：一个包 = 一个目录 + 唯一入口 %s + 被引用的能力/源文件",
			EntryFile, EntryFile)
	}
	if err := checkBundleLimits(files); err != nil {
		return nil, err
	}

	tree, err := decodeDoc(entry, EntryFile)
	if err != nil {
		return nil, err
	}
	pkg := &Package{Files: copyFiles(files), CapabilityPaths: []string{}, SourcePaths: []string{}}
	if err := applyEntry(pkg, tree); err != nil {
		return nil, err
	}
	if err := checkReferences(pkg); err != nil {
		return nil, err
	}
	if err := checkSlots(pkg); err != nil {
		return nil, err
	}
	return pkg, nil
}

// checkBundleLimits 是 §1.5 的体积与文件数上界。
//
// 体积超限没有专属错误码（§9.2 没有"文件太大"这一行），统一报 `invalid_playbook`
// 并在消息里点名文件与建议（拆能力 / 拆包）——比杜撰一个新码好。
func checkBundleLimits(files map[string]string) error {
	if len(files) > MaxFiles {
		return errf(CodeTooManyItems, "包内有 %d 个文件，上限 %d（含 %s）",
			len(files), MaxFiles, EntryFile)
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var yamlTotal int
	for _, p := range paths {
		// 文件集合的键本身就是路径：包体可能来自 tar（OCI），路径逃逸在写库之前就该拒。
		// 引用清单里的路径在 checkReferences 里再校验一次（两处都要过）。
		if msg := pathProblem(p); msg != "" {
			return errf(CodeFilePathInvalid, "包内文件路径 %q 不合法：%s", p, msg)
		}
		body := files[p]
		if len(body) > MaxFileBytes {
			return errf(CodeInvalidPlaybook, "文件 %q 有 %d 字节，单文件上限 %d（拆能力）",
				p, len(body), MaxFileBytes)
		}
		if isYAMLPath(p) {
			yamlTotal += len(body)
		}
	}
	if yamlTotal > MaxYAMLBytes {
		return errf(CodeInvalidPlaybook, "包内 YAML 合计 %d 字节，上限 %d（拆包）",
			yamlTotal, MaxYAMLBytes)
	}
	return nil
}

// applyEntry 把入口文档的字段读进 Package，并校验包的形状。
func applyEntry(pkg *Package, tree map[string]any) error {
	if err := checkKeys(tree, []string{"apiVersion", "kind", "metadata", "spec"}, EntryFile, "入口"); err != nil {
		return err
	}
	if err := checkDocKind(tree, KindPlaybook, EntryFile); err != nil {
		return err
	}

	meta, err := mappingField(tree, "metadata", EntryFile)
	if err != nil {
		return err
	}
	if meta == nil {
		return errf(CodeInvalidPlaybook, "%s 缺 metadata（包名 metadata.name 必填）", EntryFile)
	}
	if err := checkKeys(meta, []string{"name", "displayName", "version", "description", "namespace"},
		EntryFile, "metadata"); err != nil {
		return err
	}
	if ns, ok := meta["namespace"]; ok {
		// 空串等同于没写；非字符串或非空值都是「包里写死了命名空间」。
		s, isStr := ns.(string)
		if !isStr || s != "" {
			return errf(CodeNamespaceInPackage,
				"%s 的 metadata.namespace 禁止：包是命名空间内的输入，装进哪个 ns 由安装上下文决定",
				EntryFile)
		}
	}

	name, _ := meta["name"].(string)
	if name == "" {
		return errf(CodeSlugInvalid, "%s 的 metadata.name 必填（包名 slug）", EntryFile)
	}
	if !slugPattern.MatchString(name) || len(name) > MaxNameLen {
		return errf(CodeSlugInvalid,
			"包名 %q 不合法：必须匹配 ^[a-z0-9]+(-[a-z0-9]+)*$（同一段不能有下划线/大写/连续短横线）且 ≤%d 字符",
			name, MaxNameLen)
	}
	pkg.Name = name
	if s, ok := meta["displayName"]; ok {
		str, isStr := s.(string)
		if !isStr {
			return errf(CodeInvalidPlaybook, "%s 的 metadata.displayName 必须是字符串", EntryFile)
		}
		pkg.DisplayName = str
	}
	if s, ok := meta["description"]; ok {
		str, isStr := s.(string)
		if !isStr {
			return errf(CodeInvalidPlaybook, "%s 的 metadata.description 必须是字符串", EntryFile)
		}
		pkg.Description = str
	}
	if v, ok := meta["version"]; ok {
		n, isInt := toInt(v)
		if !isInt || n < 1 {
			return errf(CodeInvalidPlaybook,
				"%s 的 metadata.version 必须是 ≥1 的整数（省略 = 不参与任何身份）", EntryFile)
		}
		pkg.Version = n
	}

	spec, err := mappingField(tree, "spec", EntryFile)
	if err != nil {
		return err
	}
	if spec == nil {
		return errf(CodeInvalidPlaybook, "%s 缺 spec（values 与引用清单的家）", EntryFile)
	}
	if err := checkKeys(spec, []string{"values", "capabilities", "sources"}, EntryFile, "spec"); err != nil {
		return err
	}

	// spec.values：JSON Schema 对象（省略 = {} = 接受一切）。
	values := json.RawMessage(`{}`)
	if raw, ok := spec["values"]; ok && raw != nil {
		canon, err := canonicalJSON(raw)
		if err != nil {
			return errf(CodeInvalidPlaybook, "%s 的 spec.values 不是合法 JSON: %v", EntryFile, err)
		}
		var probe map[string]any
		if err := json.Unmarshal(canon, &probe); err != nil || probe == nil {
			return errf(CodeInvalidPlaybook,
				"%s 的 spec.values 必须是 JSON Schema 对象（{type: object, properties: …}）", EntryFile)
		}
		values = canon
	}
	pkg.Values = values

	caps, err := stringList(spec, "capabilities", EntryFile)
	if err != nil {
		return err
	}
	srcs, err := stringList(spec, "sources", EntryFile)
	if err != nil {
		return err
	}
	pkg.CapabilityPaths = caps
	pkg.SourcePaths = srcs
	return nil
}

// checkReferences 校验引用清单：路径合法、文件存在、不重复、无未引用 YAML、计数上界。
func checkReferences(pkg *Package) error {
	if len(pkg.CapabilityPaths) > MaxCapabilities {
		return errf(CodeTooManyItems, "包内有 %d 个能力，上限 %d", len(pkg.CapabilityPaths), MaxCapabilities)
	}
	if len(pkg.SourcePaths) > MaxSources {
		return errf(CodeTooManyItems, "包内有 %d 个源，上限 %d", len(pkg.SourcePaths), MaxSources)
	}

	seen := make(map[string]string, len(pkg.CapabilityPaths)+len(pkg.SourcePaths))
	for _, list := range []struct {
		slot  string
		paths []string
	}{{"capabilities", pkg.CapabilityPaths}, {"sources", pkg.SourcePaths}} {
		for i, p := range list.paths {
			if msg := pathProblem(p); msg != "" {
				return errf(CodeFilePathInvalid, "%s[%d] 的路径 %q 不合法：%s", list.slot, i, p, msg)
			}
			if prev, ok := seen[p]; ok {
				return errf(CodeFileDuplicate,
					"同一文件 %q 被重复引用（%s 与 %s）：一个文件只能占一个槽位", p, prev, list.slot)
			}
			seen[p] = list.slot
			if _, ok := pkg.Files[p]; !ok {
				return errf(CodeFileMissing, "%s[%d] 引用的文件 %q 不在包内", list.slot, i, p)
			}
		}
	}

	// 未引用的 YAML：包内 `.yaml` / `.yml` 必须全被引用（`.md` / `.txt` 豁免）。
	orphans := make([]string, 0)
	for p := range pkg.Files {
		if p == EntryFile || !isYAMLPath(p) {
			continue
		}
		if _, ok := seen[p]; !ok {
			orphans = append(orphans, p)
		}
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		return errf(CodeFileUnreferenced,
			"包内 YAML %s 没被 spec.capabilities / spec.sources 引用：静默漏装是最坏的失败形态（.md/.txt 不强制登记）",
			strings.Join(orphans, "、"))
	}
	return nil
}

// checkSlots 校验每个被引用文件的**形状**（不跑单建校验，见 Parse 的说明）。
//
// 槽位由清单决定：`capabilities[]` 里的文件必须是 Capability 文档，`sources[]` 里的必须是
// Source 文档——放错槽位 = `invalid_playbook`（消息指向路径）。
func checkSlots(pkg *Package) error {
	for _, p := range pkg.CapabilityPaths {
		tree, err := decodeDoc(pkg.Files[p], p)
		if err != nil {
			return err
		}
		if err := checkDocKind(tree, KindCapability, p); err != nil {
			return err
		}
		if err := checkDocShell(tree, p); err != nil {
			return err
		}
	}
	for _, p := range pkg.SourcePaths {
		tree, err := decodeDoc(pkg.Files[p], p)
		if err != nil {
			return err
		}
		if err := checkDocKind(tree, KindSource, p); err != nil {
			return err
		}
		if err := checkDocShell(tree, p); err != nil {
			return err
		}
		spec, err := mappingField(tree, "spec", p)
		if err != nil {
			return err
		}
		// source.md §2：spec.kind 必填。这里只看"有没有这个键、是不是字符串"，
		// 值是不是注册表里的 kind 由渲染期（sourcespec.ValidateSpec）判。
		if _, ok := spec["kind"].(string); !ok {
			return errf(CodeInvalidPlaybook,
				"%s 放在 sources 槽位，但 spec.kind 缺失或不是字符串（cron / webhook / manual）", p)
		}
	}
	return nil
}

// checkDocShell 校验被引用文档的公共外壳：metadata.name 存在、metadata.namespace 禁止、spec 是映射。
//
// 这些是"与 values 无关"的形状：name 可以是模板（`{{values.project}}-review`），
// 所以这里只要求"有个非空字符串"，不判正则（正则属于渲染期）。
func checkDocShell(tree map[string]any, where string) error {
	meta, err := mappingField(tree, "metadata", where)
	if err != nil {
		return err
	}
	if meta == nil {
		return errf(CodeInvalidPlaybook, "%s 缺 metadata（能力/源文档必须有 metadata.name）", where)
	}
	if _, ok := meta["name"].(string); !ok {
		return errf(CodeInvalidPlaybook, "%s 的 metadata.name 必填且必须是字符串", where)
	}
	if ns, ok := meta["namespace"]; ok {
		if s, isStr := ns.(string); !isStr || s != "" {
			return errf(CodeNamespaceInPackage,
				"%s 的 metadata.namespace 禁止：命名空间由安装上下文决定，包里写死会让同一个包只能装进一个 ns",
				where)
		}
	}
	spec, err := mappingField(tree, "spec", where)
	if err != nil {
		return err
	}
	if spec == nil {
		return errf(CodeInvalidPlaybook, "%s 缺 spec", where)
	}
	return nil
}

// decodeDoc 把一份 YAML 正文解成 `map[string]any`（顶层必须是映射）。
func decodeDoc(body, where string) (map[string]any, error) {
	var raw any
	if err := yaml.Unmarshal([]byte(body), &raw); err != nil {
		return nil, errf(CodeInvalidPlaybook, "%s 不是合法 YAML: %v", where, err)
	}
	tree, ok := normalizeTree(raw).(map[string]any)
	if !ok {
		return nil, errf(CodeInvalidPlaybook, "%s 的顶层必须是映射（apiVersion/kind/metadata/spec）", where)
	}
	return tree, nil
}

// normalizeTree 把 yaml.v3 可能产出的 `map[any]any` 递归换成 `map[string]any`。
func normalizeTree(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalizeTree(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[fmt.Sprint(k)] = normalizeTree(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = normalizeTree(val)
		}
		return out
	default:
		return v
	}
}

// checkDocKind 校验 kind / apiVersion（**写了就必须对**；没写不看——被引用文档允许省略）。
//
// 值是模板（含 `{{`）时跳过：`kind: '{{values.k}}'` 在渲染期才知道是什么。
func checkDocKind(tree map[string]any, wantKind, where string) error {
	if v, ok := tree["apiVersion"]; ok {
		s, isStr := v.(string)
		if !isStr {
			return errf(CodeInvalidPlaybook, "%s 的 apiVersion 必须是字符串", where)
		}
		if !strings.Contains(s, "{{") && s != APIVersion {
			return errf(CodeInvalidPlaybook, "%s 的 apiVersion 期望 %q，实际 %q", where, APIVersion, s)
		}
	}
	if v, ok := tree["kind"]; ok {
		s, isStr := v.(string)
		if !isStr {
			return errf(CodeInvalidPlaybook, "%s 的 kind 必须是字符串", where)
		}
		if strings.Contains(s, "{{") {
			return nil
		}
		if s != wantKind {
			return errf(CodeInvalidPlaybook,
				"%s 的 kind 是 %q，但这个槽位要求 %q（放错槽位 = invalid_playbook）", where, s, wantKind)
		}
	}
	return nil
}

// checkKeys 拒未知字段（拼错的键静默失效比解析失败更难查）。
func checkKeys(m map[string]any, allowed []string, where, section string) error {
	allow := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		allow[k] = true
	}
	unknown := make([]string, 0)
	for k := range m {
		if !allow[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return errf(CodeInvalidPlaybook, "%s 的 %s 有未知字段 %s（允许：%s）",
		where, section, strings.Join(unknown, "、"), strings.Join(allowed, " / "))
}

// mappingField 取一个"必须是映射"的字段（缺省 → nil, nil）。
func mappingField(m map[string]any, key, where string) (map[string]any, error) {
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil, nil
	}
	child, isMap := raw.(map[string]any)
	if !isMap {
		return nil, errf(CodeInvalidPlaybook, "%s 的 %s 必须是映射", where, key)
	}
	return child, nil
}

// stringList 取一个"字符串数组"的字段（缺省 → 空切片）。
func stringList(m map[string]any, key, where string) ([]string, error) {
	raw, ok := m[key]
	if !ok || raw == nil {
		return []string{}, nil
	}
	list, isList := raw.([]any)
	if !isList {
		return nil, errf(CodeInvalidPlaybook, "%s 的 %s 必须是字符串数组（相对路径清单）", where, key)
	}
	out := make([]string, 0, len(list))
	for i, item := range list {
		s, isStr := item.(string)
		if !isStr {
			return nil, errf(CodeInvalidPlaybook, "%s 的 %s[%d] 不是字符串", where, key, i)
		}
		out = append(out, s)
	}
	return out, nil
}

// canonicalJSON 把任意值归一成 key 有序的紧凑 JSON。
func canonicalJSON(v any) (json.RawMessage, error) {
	out, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var probe any
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	if err := dec.Decode(&probe); err != nil {
		return nil, err
	}
	return json.RawMessage(out), nil
}

// toInt 把 YAML/JSON 解出的数字转 int（非整数/非数字 → false）。
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	}
	return 0, false
}

// pathProblem 校验引用路径的形状，返回空串表示合法。
//
// 规则照 §1.3：相对包根、`/` 分隔、不以 `/` 开头、不含 `..` 段、≤255 字符、不含 NUL。
// 另外拒反斜杠、空段与 `.` 段——它们在不同操作系统上会指向同一个名字以外的东西，
// 让"包里有哪几个文件"这件事变得不可审计（D27：路径是寻址）。
func pathProblem(p string) string {
	if p == "" {
		return "路径不能为空"
	}
	if len(p) > MaxPathLen {
		return fmt.Sprintf("超过 %d 字符", MaxPathLen)
	}
	if strings.ContainsRune(p, 0) {
		return "含 NUL 字节"
	}
	if strings.Contains(p, `\`) {
		return "含反斜杠（只接受 / 分隔的 POSIX 相对路径）"
	}
	if path.IsAbs(p) {
		return "必须是相对包根的路径"
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "":
			return "含空路径段"
		case ".", "..":
			return fmt.Sprintf("含 %q 路径段", seg)
		}
	}
	return ""
}

// isYAMLPath 报告路径是否是 YAML 文件（`.yaml` / `.yml`，大小写敏感——路径本身不渲染）。
func isYAMLPath(p string) bool {
	return strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml")
}

// copyFiles 拷一份文件集合（调用方之后改自己的 map 不影响 Package）。
func copyFiles(files map[string]string) map[string]string {
	out := make(map[string]string, len(files))
	for k, v := range files {
		out[k] = v
	}
	return out
}
