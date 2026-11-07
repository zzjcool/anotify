package main

// 本文件是 issue #319 §3 的**一致性守卫**：`anotify-agent schema capability` 与
// `internal/artifact` 的 Validate 是两份真理，最容易漂。
//
// 为什么必须有它：schema 是给人/coding agent 看的**承诺**。如果 schema 说「合法」而
// Validate 拒绝，作者会照着文档写出跑不起来的东西，而且**两处都不报错**（schema 不改
// 行为，Validate 不读 schema）——这种漂移只有用例能发现。
//
// 做法：写一个**最小 JSON Schema 求值器**（只认本仓库三份 schema 用到的关键字），
// 拿它跑一组「单点破坏」的输入，再拿 `artifact.Parse` 跑同一份输入，两边的判定必须
// 逐例一致。用例覆盖 issue 点名的三类约束：
//
//	required  缺 apiVersion / kind / metadata / spec / run / entrypoint / files / version
//	enum      apiVersion / kind 的 const 值
//	pattern   metadata.name / outputs[] / requires[] / run.env 的键
//
// 外加「不该被误判」的一侧：outputs 可以省略（Validate 允许空出口 = 终端能力），
// 所以 schema 不能把它写成 required——只测「schema 拒绝」会漏掉这种假阳性。
//
// 为什么自己写求值器而不是引一个 schema 库：issue 明确「不引新的 schema 生成库」，
// 而这不影响守卫的强度——求值的是**真实的 schema 文档**（从 renderSchema 的输出解回来），
// 不是另外手抄的一份规则。

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/ws"
	"gopkg.in/yaml.v3"
)

// ---------- 最小 JSON Schema 求值器 ----------

// schemaDocValue 取一份**实际输出**的 schema（走 renderSchema，与 `anotify-agent schema`
// 打出去的是同一份字节），返回解码后的文档树。
func schemaDocValue(t *testing.T, name string) any {
	t.Helper()
	out, err := renderSchema(schemaDoc[name])
	if err != nil {
		t.Fatalf("renderSchema(%s): %v", name, err)
	}
	var doc any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("schema %s 不是合法 JSON: %v\n%s", name, err, out)
	}
	return doc
}

// validateJSONSchema 判一份文档是否满足一份 schema，返回全部违规（空 = 满足）。
//
// 结果按字符串排序：map 迭代顺序是随机的，排序后失败信息才可复现、可 diff。
//
// 文档先过一道 JSON 往返：真实求值器看到的水远是 JSON（数字都是 float64），而夹具
// 是 Go 值（`version: 1` 是 int）。不归一的话 `"type": "integer"` 会把每个 int 都
// 判成类型错——那是求值器的 bug，不是 schema 的问题。
func validateJSONSchema(root any, doc any) []string {
	normalized, err := normalizeJSONValue(doc)
	if err != nil {
		return []string{"$: 文档不是合法 JSON: " + err.Error()}
	}
	var out []string
	checkAgainstSchema(root, root, normalized, "$", &out)
	sort.Strings(out)
	return out
}

// normalizeJSONValue 把任何 Go 值转成「JSON 解出来的形状」（map[string]any / []any /
// float64 / string / bool / nil）。
func normalizeJSONValue(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// checkAgainstSchema 是求值器的递归内核（root 用于解析 $ref）。
func checkAgainstSchema(root any, schema, doc any, path string, out *[]string) {
	obj, ok := schema.(map[string]any)
	if !ok {
		return // true / {} 之类：不约束
	}

	if ref, ok := obj["$ref"].(string); ok {
		target, found := resolveSchemaRef(root, ref)
		if !found {
			*out = append(*out, fmt.Sprintf("%s: 解析不了 $ref %q", path, ref))
			return
		}
		checkAgainstSchema(root, target, doc, path, out)
	}

	if t, ok := obj["type"]; ok && !matchesSchemaType(t, doc) {
		*out = append(*out, fmt.Sprintf("%s: 类型应为 %v，实际 %s", path, t, jsonTypeOf(doc)))
		return // 类型都不对，再报「缺必填键」只会制造噪音
	}

	switch d := doc.(type) {
	case map[string]any:
		props := map[string]any{}
		if p, ok := obj["properties"].(map[string]any); ok {
			props = p
		}
		if req, ok := obj["required"].([]any); ok {
			for _, r := range req {
				name, _ := r.(string)
				if _, present := d[name]; !present {
					*out = append(*out, fmt.Sprintf("%s: 缺少必填键 %q", path, name))
				}
			}
		}
		for _, name := range sortedMapKeys(props) {
			if v, present := d[name]; present {
				checkAgainstSchema(root, props[name], v, path+"."+name, out)
			}
		}
		if pn, ok := obj["propertyNames"]; ok {
			for _, name := range sortedMapKeys(d) {
				checkAgainstSchema(root, pn, name, path+"{"+name+"}", out)
			}
		}
		for _, name := range sortedMapKeys(d) {
			if _, declared := props[name]; declared {
				continue
			}
			switch rule := obj["additionalProperties"].(type) {
			case bool:
				if !rule {
					*out = append(*out, fmt.Sprintf("%s: 不允许的键 %q（additionalProperties: false）", path, name))
				}
			case map[string]any:
				checkAgainstSchema(root, rule, d[name], path+"."+name, out)
			}
		}
		if n, ok := schemaInt(obj, "minProperties"); ok && len(d) < n {
			*out = append(*out, fmt.Sprintf("%s: 属性只有 %d 个，至少要 %d 个", path, len(d), n))
		}
		if n, ok := schemaInt(obj, "maxProperties"); ok && len(d) > n {
			*out = append(*out, fmt.Sprintf("%s: 属性有 %d 个，最多 %d 个", path, len(d), n))
		}

	case []any:
		if items, ok := obj["items"]; ok {
			for i, v := range d {
				checkAgainstSchema(root, items, v, fmt.Sprintf("%s[%d]", path, i), out)
			}
		}
		if n, ok := schemaInt(obj, "minItems"); ok && len(d) < n {
			*out = append(*out, fmt.Sprintf("%s: 只有 %d 项，至少要 %d 项", path, len(d), n))
		}
		if n, ok := schemaInt(obj, "maxItems"); ok && len(d) > n {
			*out = append(*out, fmt.Sprintf("%s: 有 %d 项，最多 %d 项", path, len(d), n))
		}
		if uniq, _ := obj["uniqueItems"].(bool); uniq {
			seen := make(map[string]int, len(d))
			for i, v := range d {
				key := canonicalSchemaJSON(v)
				if first, dup := seen[key]; dup {
					*out = append(*out, fmt.Sprintf("%s[%d]: 与 [%d] 重复（uniqueItems: true）", path, i, first))
					continue
				}
				seen[key] = i
			}
		}

	case string:
		if pattern, ok := obj["pattern"].(string); ok {
			re, err := regexp.Compile(pattern)
			switch {
			case err != nil:
				*out = append(*out, fmt.Sprintf("%s: schema 的 pattern 编译不了（%v）：%q", path, err, pattern))
			case !re.MatchString(d):
				*out = append(*out, fmt.Sprintf("%s: 不满足 pattern %q（实际 %q）", path, pattern, d))
			}
		}
		// JSON Schema 的 length 按 Unicode 码点算，不是字节。
		if n, ok := schemaInt(obj, "minLength"); ok && utf8.RuneCountInString(d) < n {
			*out = append(*out, fmt.Sprintf("%s: 长度 %d < minLength %d", path, utf8.RuneCountInString(d), n))
		}
		if n, ok := schemaInt(obj, "maxLength"); ok && utf8.RuneCountInString(d) > n {
			*out = append(*out, fmt.Sprintf("%s: 长度 %d > maxLength %d", path, utf8.RuneCountInString(d), n))
		}

	case float64:
		if n, ok := schemaNumber(obj, "minimum"); ok && d < n {
			*out = append(*out, fmt.Sprintf("%s: %v < minimum %v", path, d, n))
		}
		if n, ok := schemaNumber(obj, "maximum"); ok && d > n {
			*out = append(*out, fmt.Sprintf("%s: %v > maximum %v", path, d, n))
		}
	}

	if c, ok := obj["const"]; ok && !schemaJSONEqual(c, doc) {
		*out = append(*out, fmt.Sprintf("%s: 必须恰好是 %v（实际 %v）", path, c, doc))
	}
	if e, ok := obj["enum"].([]any); ok {
		found := false
		for _, v := range e {
			if schemaJSONEqual(v, doc) {
				found = true
				break
			}
		}
		if !found {
			*out = append(*out, fmt.Sprintf("%s: %v 不在 enum %v 里", path, doc, e))
		}
	}
	if sub, ok := obj["not"]; ok {
		var inner []string
		checkAgainstSchema(root, sub, doc, path, &inner)
		if len(inner) == 0 {
			*out = append(*out, fmt.Sprintf("%s: 命中了 not 约束（这一项不允许）", path))
		}
	}
	if subs, ok := obj["allOf"].([]any); ok {
		for _, sub := range subs {
			checkAgainstSchema(root, sub, doc, path, out)
		}
	}
	if subs, ok := obj["anyOf"].([]any); ok {
		matched := false
		for _, sub := range subs {
			var inner []string
			checkAgainstSchema(root, sub, doc, path, &inner)
			if len(inner) == 0 {
				matched = true
				break
			}
		}
		if !matched {
			*out = append(*out, fmt.Sprintf("%s: 不满足 anyOf 的任何一支", path))
		}
	}
}

// resolveSchemaRef 解析本地 $ref（本仓库只用到 "#/$defs/x" 这一种）。
func resolveSchemaRef(root any, ref string) (any, bool) {
	const prefix = "#/"
	if !strings.HasPrefix(ref, prefix) {
		return nil, false
	}
	cur := root
	for _, seg := range strings.Split(ref[len(prefix):], "/") {
		seg = strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		next, ok := obj[seg]
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

// matchesSchemaType 判断文档是否属于 schema 声明的类型（string 或 string[]）。
func matchesSchemaType(t any, doc any) bool {
	switch tt := t.(type) {
	case string:
		return matchesSchemaTypeName(tt, doc)
	case []any:
		for _, v := range tt {
			if name, ok := v.(string); ok && matchesSchemaTypeName(name, doc) {
				return true
			}
		}
		return false
	}
	return true // 不认识的写法：不约束（宁可漏报，不要误报）
}

func matchesSchemaTypeName(name string, doc any) bool {
	switch name {
	case "object":
		_, ok := doc.(map[string]any)
		return ok
	case "array":
		_, ok := doc.([]any)
		return ok
	case "string":
		_, ok := doc.(string)
		return ok
	case "boolean":
		_, ok := doc.(bool)
		return ok
	case "null":
		return doc == nil
	case "number":
		_, ok := doc.(float64)
		return ok
	case "integer":
		f, ok := doc.(float64)
		return ok && f == math.Trunc(f)
	}
	return true
}

// jsonTypeOf 是失败信息里的类型名（decoded JSON 只有这几种）。
func jsonTypeOf(doc any) string {
	switch doc.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		return "number"
	}
	return fmt.Sprintf("%T", doc)
}

// schemaInt / schemaNumber 取数值关键字（JSON 解出来的数都是 float64）。
func schemaInt(obj map[string]any, key string) (int, bool) {
	f, ok := obj[key].(float64)
	if !ok {
		return 0, false
	}
	return int(f), true
}

func schemaNumber(obj map[string]any, key string) (float64, bool) {
	f, ok := obj[key].(float64)
	return f, ok
}

// schemaJSONEqual 比较两个 JSON 值（走 canonical JSON：map key 有序）。
func schemaJSONEqual(a, b any) bool {
	return canonicalSchemaJSON(a) == canonicalSchemaJSON(b)
}

func canonicalSchemaJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("!unmarshalable:%T", v)
	}
	return string(b)
}

// sortedMapKeys 取 map 的 key（有序：失败信息稳定）。
func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------- ① 能力 yaml：schema ↔ artifact.Validate ----------

// capabilityDoc 返回一份**合法**的能力产物（JSON 形状的 map），各用例在它上面做单点破坏。
//
// 用 map 而不是 YAML 字面量：破坏点是 `delete(doc["spec"].(map)["run"], "entrypoint")`
// 这种，改哪一处一眼可见；YAML 字符串拼接则会让「到底破坏了什么」变成猜。
func capabilityDoc() map[string]any {
	return map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Capability",
		"metadata": map[string]any{
			"name": "notify.phone", "version": 1,
			// displayName 在 metadata 里（v3 §3.2：不参与 digest）。
			"displayName": "写文档",
		},
		"spec": map[string]any{
			"params": map[string]any{
				"type":       "object",
				"properties": map[string]any{"repo": map[string]any{"type": "string"}},
			},
			"outputs":   []any{"fix"},
			"requires":  []any{"git"},
			"lifecycle": map[string]any{"retryable": true, "maxAttempts": 2},
			"sticky":    "worktree",
			"run": map[string]any{
				"entrypoint": "main.sh",
				"files":      map[string]any{"main.sh": "#!/bin/sh\necho hi\n"},
				"timeoutSec": 60,
				"env":        map[string]any{"LOG_LEVEL": "info"},
			},
		},
	}
}

// nested 取 doc 里的一个嵌套对象（路径写错就是测试自己坏了，直接 Fatal）。
func nested(t *testing.T, doc map[string]any, keys ...string) map[string]any {
	t.Helper()
	cur := doc
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			t.Fatalf("夹具里 %q 不是对象（路径 %v 写错了）", k, keys)
		}
		cur = next
	}
	return cur
}

// capabilitySchemaCase 是一条「单点破坏」用例。
type capabilitySchemaCase struct {
	name string
	// class 是这条用例钉的约束类别（required / enum / pattern；空 = 其他）。
	// 用于断言「三类约束都真的被求值器执行过」——只测「schema 拒绝」而三类一条没覆盖，
	// 守卫就退化成了一句空话。
	class    string
	wantOK   bool
	wantCode string // wantOK=false 时，artifact.Code 必须给出这个短码
	mutate   func(t *testing.T, doc map[string]any)
}

func TestCapabilitySchemaMatchesValidate(t *testing.T) {
	root := schemaDocValue(t, schemaCapability)

	cases := []capabilitySchemaCase{
		{name: "完整产物", wantOK: true},

		// ---- required ----
		{name: "缺 apiVersion", class: "required", wantCode: "unsupported_api_version",
			mutate: func(t *testing.T, d map[string]any) { delete(d, "apiVersion") }},
		{name: "缺 kind", class: "required", wantCode: "unsupported_kind",
			mutate: func(t *testing.T, d map[string]any) { delete(d, "kind") }},
		{name: "缺 metadata", class: "required", wantCode: "invalid_name",
			mutate: func(t *testing.T, d map[string]any) { delete(d, "metadata") }},
		{name: "缺 metadata.version", class: "required", wantCode: "invalid_version",
			mutate: func(t *testing.T, d map[string]any) {
				delete(nested(t, d, "metadata"), "version")
			}},
		// 「没有实现」是一种**硬错误**（Capability Spec v3 §3.1 / R2）：既不写 run 也不写
		// handler 就等于这份能力没有实现，而不是「还没配」。
		{name: "缺 spec", class: "required", wantCode: "impl_missing",
			mutate: func(t *testing.T, d map[string]any) { delete(d, "spec") }},
		{name: "缺 spec.run", class: "required", wantCode: "impl_missing",
			mutate: func(t *testing.T, d map[string]any) { delete(nested(t, d, "spec"), "run") }},
		{name: "缺 run.entrypoint", class: "required", wantCode: "entrypoint_missing",
			mutate: func(t *testing.T, d map[string]any) {
				delete(nested(t, d, "spec", "run"), "entrypoint")
			}},
		{name: "缺 run.files", class: "required", wantCode: "invalid_file_path",
			mutate: func(t *testing.T, d map[string]any) {
				delete(nested(t, d, "spec", "run"), "files")
			}},
		// 反向（假阳性）：Validate 允许零出口（终端能力），schema 就不能把 outputs 写成必填。
		{name: "省略 spec.outputs（终端能力）", class: "required", wantOK: true,
			mutate: func(t *testing.T, d map[string]any) { delete(nested(t, d, "spec"), "outputs") }},
		{name: "省略 spec.params/requires/env（都可选）", class: "required", wantOK: true,
			mutate: func(t *testing.T, d map[string]any) {
				spec := nested(t, d, "spec")
				delete(spec, "params")
				delete(spec, "requires")
				delete(nested(t, d, "spec", "run"), "env")
			}},

		// ---- enum / const ----
		{name: "apiVersion 换成 anotify/v0", class: "enum", wantCode: "unsupported_api_version",
			mutate: func(t *testing.T, d map[string]any) { d["apiVersion"] = "anotify/v0" }},
		{name: "kind 小写", class: "enum", wantCode: "unsupported_kind",
			mutate: func(t *testing.T, d map[string]any) { d["kind"] = "capability" }},
		{name: "kind 为空", class: "enum", wantCode: "unsupported_kind",
			mutate: func(t *testing.T, d map[string]any) { d["kind"] = "" }},

		// ---- pattern ----
		{name: "name 含大写", class: "pattern", wantCode: "invalid_name",
			mutate: func(t *testing.T, d map[string]any) { nested(t, d, "metadata")["name"] = "BadName" }},
		{name: "name 以点开头", class: "pattern", wantCode: "invalid_name",
			mutate: func(t *testing.T, d map[string]any) { nested(t, d, "metadata")["name"] = ".notify.phone" }},
		{name: "name 超过 63 字符", class: "pattern", wantCode: "invalid_name",
			mutate: func(t *testing.T, d map[string]any) {
				nested(t, d, "metadata")["name"] = strings.Repeat("a", 64)
			}},
		{name: "name 正好 63 字符", class: "pattern", wantOK: true,
			mutate: func(t *testing.T, d map[string]any) {
				nested(t, d, "metadata")["name"] = strings.Repeat("a", 63)
			}},
		{name: "outputs 含空格", class: "pattern", wantCode: "invalid_output",
			mutate: func(t *testing.T, d map[string]any) { nested(t, d, "spec")["outputs"] = []any{"bad port"} }},
		{name: "outputs 含非法字符", class: "pattern", wantCode: "invalid_output",
			mutate: func(t *testing.T, d map[string]any) { nested(t, d, "spec")["outputs"] = []any{"a/b"} }},
		{name: "requires 含斜杠", class: "pattern", wantCode: "invalid_requirement",
			mutate: func(t *testing.T, d map[string]any) { nested(t, d, "spec")["requires"] = []any{"gh/cli"} }},
		{name: "env 键以数字开头", class: "pattern", wantCode: "reserved_env",
			mutate: func(t *testing.T, d map[string]any) {
				nested(t, d, "spec", "run")["env"] = map[string]any{"1BAD": "x"}
			}},
		{name: "env 键覆盖 ANOTIFY_", class: "pattern", wantCode: "reserved_env",
			mutate: func(t *testing.T, d map[string]any) {
				nested(t, d, "spec", "run")["env"] = map[string]any{"ANOTIFY_TASKDIR": "/tmp/x"}
			}},

		// ---- Capability Spec v3：实现形式二选一（anyOf）----
		{name: "handler 形式（无 run）", wantOK: true,
			mutate: func(t *testing.T, d map[string]any) {
				spec := nested(t, d, "spec")
				delete(spec, "run")
				spec["handler"] = "noop"
			}},
		{name: "handler 与 run 都给", wantCode: "impl_ambiguous",
			mutate: func(t *testing.T, d map[string]any) { nested(t, d, "spec")["handler"] = "noop" }},
		{name: "run 与 handler 都不给", wantCode: "impl_missing",
			mutate: func(t *testing.T, d map[string]any) { delete(nested(t, d, "spec"), "run") }},
		{name: "handler 不在白名单", wantCode: "unknown_handler",
			mutate: func(t *testing.T, d map[string]any) {
				spec := nested(t, d, "spec")
				delete(spec, "run")
				spec["handler"] = "shell"
			}},
		// handler 形式里残留任何一个 run 侧字段都是「两种实现混在一份规格里」。
		{name: "handler 形式带 timeoutSec", wantCode: "impl_ambiguous",
			mutate: func(t *testing.T, d map[string]any) {
				spec := nested(t, d, "spec")
				run := nested(t, d, "spec", "run")
				delete(run, "entrypoint")
				delete(run, "files")
				delete(run, "env")
				spec["handler"] = "noop"
			}},
		// lifecycle 是**不透明透传**（内容归 store 校验）：非对象也必须两边都放行，
		// 否则 schema 就替 store 做了决定，而 artifact 层看不出它为什么该拒。
		{name: "lifecycle 不是对象（内容归 store）", wantOK: true,
			mutate: func(t *testing.T, d map[string]any) {
				nested(t, d, "spec")["lifecycle"] = []any{"x"}
			}},
		{name: "sticky 是 worktree", wantOK: true,
			mutate: func(t *testing.T, d map[string]any) { nested(t, d, "spec")["sticky"] = "worktree" }},
		// v3 把 displayName 搬到了 metadata：spec 里的那个已是未知字段。
		{name: "spec.displayName（v3 起在 metadata 里）", wantCode: "unknown_field",
			mutate: func(t *testing.T, d map[string]any) {
				nested(t, d, "spec")["displayName"] = "写文档"
			}},

		// ---- 其他（同样要与 Validate 一致，只是不属于那三类）----
		{name: "version 为 0", wantCode: "invalid_version",
			mutate: func(t *testing.T, d map[string]any) { nested(t, d, "metadata")["version"] = 0 }},
		{name: "timeoutSec 超过 24 小时", wantCode: "invalid_timeout",
			mutate: func(t *testing.T, d map[string]any) {
				nested(t, d, "spec", "run")["timeoutSec"] = 90000
			}},
		{name: "params 不是对象", wantCode: "invalid_params_schema",
			mutate: func(t *testing.T, d map[string]any) { nested(t, d, "spec")["params"] = []any{} }},
		{name: "files 为空对象", wantCode: "invalid_file_path",
			mutate: func(t *testing.T, d map[string]any) {
				nested(t, d, "spec", "run")["files"] = map[string]any{}
			}},
		{name: "outputs 重复", wantCode: "invalid_output",
			mutate: func(t *testing.T, d map[string]any) { nested(t, d, "spec")["outputs"] = []any{"fix", "fix"} }},
		{name: "未知顶层字段", wantCode: "unknown_field",
			mutate: func(t *testing.T, d map[string]any) { d["capabilities"] = []any{} }},
		{name: "未知 run 字段", wantCode: "unknown_field",
			mutate: func(t *testing.T, d map[string]any) {
				nested(t, d, "spec", "run")["timeout"] = 60
			}},
	}

	exercised := map[string]bool{}
	for _, c := range cases {
		doc := capabilityDoc()
		if c.mutate != nil {
			c.mutate(t, doc)
		}
		violations := validateJSONSchema(root, doc)
		_, err := artifact.Parse(mustJSON(t, doc))
		schemaOK := len(violations) == 0
		validateOK := err == nil
		if schemaOK != validateOK {
			t.Errorf("%s: schema 与 Validate 分叉——schema 判 %s，Validate 判 %s\n"+
				"  schema 违规: %v\n  Validate: %v\n  生效的 schema 约束类别: %s",
				c.name, okWord(schemaOK), okWord(validateOK), violations, err, c.class)
			continue
		}
		if validateOK != c.wantOK {
			t.Errorf("%s: Validate 判 %s，用例期望 %s（err: %v）",
				c.name, okWord(validateOK), okWord(c.wantOK), err)
		}
		if !validateOK && c.wantCode != "" {
			if got := artifact.Code(err); got != c.wantCode {
				t.Errorf("%s: artifact.Code = %q, want %q（Validate: %v）",
					c.name, got, c.wantCode, err)
			}
		}
		if !schemaOK && c.class != "" {
			exercised[c.class] = true
		}
	}

	// 覆盖度自检：三类约束都必须真的**拦住过**至少一个输入。
	// 少了这一条，一份没有 required/pattern 的 schema 也能让上面的用例全过。
	for _, class := range []string{"required", "enum", "pattern"} {
		if !exercised[class] {
			t.Errorf("没有一条 %q 类用例被 schema 拒绝过——schema 里那一类约束等于没生效", class)
		}
	}
}

// handlerFormDoc 返回一份 handler 形式（没有 run）的合法产物。
func handlerFormDoc(t *testing.T) map[string]any {
	t.Helper()
	doc := capabilityDoc()
	spec := nested(t, doc, "spec")
	delete(spec, "run")
	spec["handler"] = "notify-phone"
	return doc
}

// TestCapabilityCanonicalJSONSatisfiesSchema 钉住「自产自销」这一侧：
// artifact.CanonicalJSON 吐出来的**规范字节**（store 的 artifacts.spec 列、节点落盘的
// artifact.yaml 都是它）必须被这份 schema 接受。
//
// 只让「schema 与 Validate 逐例同判」是不够的：canonical JSON 是 Validate 的**输出**，
// 它要是被判非法，任何拿存下来的 spec 去过一遍 schema 的工具都会报假警——而 artifact
// 与 schema 两份真理各自都和 Validate 对齐，谁都看不出问题出在输出侧。
//
// handler 形式就是这个坑的现场：Run 是非指针 struct，encoding/json 不会 omitempty，
// 一不留神就会在 handler 形式里漏出一个 `"run":{"entrypoint":"","files":null}` 块，
// 那既违反空列表纪律，又正好撞上「handler 不得带 files/entrypoint」这条规则。
func TestCapabilityCanonicalJSONSatisfiesSchema(t *testing.T) {
	root := schemaDocValue(t, schemaCapability)

	cases := []struct {
		name string
		doc  map[string]any
	}{
		{"脚本形式", capabilityDoc()},
		{"handler 形式", handlerFormDoc(t)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := artifact.Parse(mustJSON(t, c.doc))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			canon, err := artifact.CanonicalJSON(a)
			if err != nil {
				t.Fatalf("CanonicalJSON: %v", err)
			}
			if v := validateJSONSchema(root, decodeJSON(t, canon)); len(v) != 0 {
				t.Fatalf("规范字节不满足它自己的 schema：\n  %v\n  %s", v, canon)
			}
		})
	}
}

// TestCapabilitySchemaRequiredKeys 直接钉 root 的 required 清单：
// 它是 issue 里点名的第一个验收点（缺 spec.run.entrypoint 要报出来），也是「哪些键
// 可以省略」的唯一说明位。
func TestCapabilitySchemaRequiredKeys(t *testing.T) {
	root, _ := schemaDocValue(t, schemaCapability).(map[string]any)
	got := stringList(t, root["required"])
	want := []string{"apiVersion", "kind", "metadata", "spec"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("root.required = %v, want %v", got, want)
	}

	spec := nested(t, root, "properties", "spec")
	// spec 自己**没有** required：实现形式是 run 与 handler 二选一，
	// 用 anyOf 表达（单靠 required 会把「合法的那一支」也一起要求）。
	if got := spec["required"]; got != nil {
		if list, ok := got.([]any); ok && len(list) > 0 {
			t.Fatalf("spec.required = %v，want 空（实现形式二选一由 anyOf 表达）", list)
		}
	}
	run := nested(t, spec, "properties", "run")
	got = stringList(t, run["required"])
	if strings.Join(got, ",") != "entrypoint,files" {
		t.Fatalf("spec.run.required = %v, want [entrypoint files]", got)
	}
	// handler 的白名单是 wire 契约（P1-b 的播种/迁移照它写），这里逐字钉住。
	handler := nested(t, spec, "properties", "handler")
	if got := stringList(t, handler["enum"]); strings.Join(got, ",") != "notify-phone,noop" {
		t.Fatalf("spec.handler.enum = %v, want [notify-phone noop]", got)
	}
	// anyOf 两支：脚本形式（run 且无 handler）/ handler 形式（handler 且无 run）。
	anyOf, ok := spec["anyOf"].([]any)
	if !ok || len(anyOf) != 2 {
		t.Fatalf("spec.anyOf = %v，want 两支（脚本 / handler）", spec["anyOf"])
	}
	// 三个 const：apiVersion / kind / pick 的取值是契约，不是"示例"。
	if v := root["properties"].(map[string]any)["apiVersion"].(map[string]any)["const"]; v != "anotify/v1" {
		t.Fatalf("apiVersion.const = %v", v)
	}
	if v := root["properties"].(map[string]any)["kind"].(map[string]any)["const"]; v != "Capability" {
		t.Fatalf("kind.const = %v", v)
	}
}

// ---------- ② node.yaml：schema ↔ nodeConfig.validate ----------

// TestNodeSchemaMatchesConfigValidate：node.yaml 的 schema 与 loadConfig（内含
// nodeConfig.validate）对同一份输入必须同判。
//
// 为什么这也值得钉：node.yaml 是运维手写的（systemd unit 里那份），schema 说
// 「server 这样写就行」而 agent 起不来，是运维最痛的那类「文档骗人」。
func TestNodeSchemaMatchesConfigValidate(t *testing.T) {
	root := schemaDocValue(t, schemaNode)

	cases := []struct {
		name   string
		doc    map[string]any
		wantOK bool
	}{
		{"完整四件", map[string]any{
			"name": "buildbox-1", "server": "https://anotify.example.com",
			"token": "nt_abc", "instances": 4,
		}, true},
		{"省略 name（只进日志）", map[string]any{
			"server": "https://anotify.example.com", "token": "nt_abc", "instances": 2,
		}, true},
		{"省略 instances（缺省 1）", map[string]any{
			"name": "buildbox-1", "server": "https://anotify.example.com", "token": "nt_abc",
		}, true},
		{"instances 为 0（归一为 1）", map[string]any{
			"server": "https://anotify.example.com", "token": "nt_abc", "instances": 0,
		}, true},
		{"ws:// 也认", map[string]any{
			"server": "ws://127.0.0.1:8080", "token": "nt_abc",
		}, true},
		{"缺 server", map[string]any{"token": "nt_abc"}, false},
		{"缺 token", map[string]any{"server": "https://anotify.example.com"}, false},
		{"token 是空串", map[string]any{
			"server": "https://anotify.example.com", "token": "",
		}, false},
		{"server 没有 scheme", map[string]any{
			"server": "anotify.example.com:8080", "token": "nt_abc",
		}, false},
		{"server 方案不支持", map[string]any{
			"server": "ftp://anotify.example.com", "token": "nt_abc",
		}, false},
		{"instances 为负", map[string]any{
			"server": "https://anotify.example.com", "token": "nt_abc", "instances": -1,
		}, false},
		{"未知键（v1 的 capabilities）", map[string]any{
			"server": "https://anotify.example.com", "token": "nt_abc",
			"capabilities": []any{},
		}, false},
	}

	for _, c := range cases {
		violations := validateJSONSchema(root, c.doc)
		schemaOK := len(violations) == 0

		data, err := yaml.Marshal(c.doc)
		if err != nil {
			t.Fatalf("%s: yaml.Marshal: %v", c.name, err)
		}
		path := filepath.Join(t.TempDir(), "node.yaml")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("%s: 写 node.yaml: %v", c.name, err)
		}
		_, cfgErr := loadConfig(path)
		cfgOK := cfgErr == nil

		if schemaOK != cfgOK {
			t.Errorf("%s: schema 判 %s，loadConfig 判 %s\n  schema 违规: %v\n  loadConfig: %v\n  yaml:\n%s",
				c.name, okWord(schemaOK), okWord(cfgOK), violations, cfgErr, data)
			continue
		}
		if cfgOK != c.wantOK {
			t.Errorf("%s: loadConfig 判 %s，用例期望 %s（err: %v）",
				c.name, okWord(cfgOK), okWord(c.wantOK), cfgErr)
		}
	}
}

// ---------- ③ 出口契约：schema ↔ emitContractJSON（线上字节） ----------

// TestEmitSchemaMatchesWireContract：拿**真的** emitContractJSON 输出对 schema 求值。
//
// 这条比「手写一个例子」强：它钉的是 runner.go 实际序列化出来的字节（字段名、总是
// 输出的键、null 与 [] 的区别）。谁把 targets[].capability 改名、或给 needs 加
// omitempty，这条用例立刻红。
func TestEmitSchemaMatchesWireContract(t *testing.T) {
	root := schemaDocValue(t, schemaEmit)

	empty, err := emitContractJSON(job{Ports: []string{}, Expects: nil})
	if err != nil {
		t.Fatalf("emitContractJSON(空): %v", err)
	}
	if len(validateJSONSchema(root, decodeJSON(t, empty))) != 0 {
		t.Fatalf("空出口契约 %s 不满足 schema（没有端口时应当是 []）", empty)
	}

	j := job{
		Ports: []string{"out", "fix"},
		Expects: map[string]ws.PortExpect{
			"fix": { // 有下游、有并集门槛、有两条候选
				Schema: json.RawMessage(`{"type":"object","required":["issue"]}`),
				Targets: []ws.ExpectTarget{
					{Capability: "notify.phone", Version: 2, Priority: 1,
						Needs: json.RawMessage(`{"type":"object"}`)},
					{Capability: "script.echo", Version: 1, Priority: 0},
				},
			},
			"out": {}, // 没有下游：schema 与 needs 都是显式 null
		},
	}
	raw, err := emitContractJSON(j)
	if err != nil {
		t.Fatalf("emitContractJSON: %v", err)
	}
	doc := decodeJSON(t, raw)
	if violations := validateJSONSchema(root, doc); len(violations) != 0 {
		t.Fatalf("线上出口契约不满足 emit schema:\n  %v\n  实际: %s", violations, raw)
	}

	// 负例：schema 必须真的拦得住形状漂移（否则上面那条「通过」没有意义）。
	t.Run("负例", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(item map[string]any)
		}{
			{"port 改名", func(item map[string]any) {
				item["output"] = item["port"]
				delete(item, "port")
			}},
			{"pick 不是 one", func(item map[string]any) { item["pick"] = "all" }},
			{"port 含空格", func(item map[string]any) { item["port"] = "bad port" }},
			{"targets 缺 needs", func(item map[string]any) {
				tgt := item["targets"].([]any)[0].(map[string]any)
				delete(tgt, "needs")
			}},
			{"schema 不是对象/null", func(item map[string]any) { item["schema"] = "object" }},
			{"未知键", func(item map[string]any) { item["extra"] = true }},
		}
		for _, c := range cases {
			bad := decodeJSON(t, raw)
			items, ok := bad.([]any)
			if !ok || len(items) == 0 {
				t.Fatalf("夹具不是数组：%T", bad)
			}
			// 只破坏第一个（按 port 升序是 "fix"）那一项。
			c.mutate(items[0].(map[string]any))
			if v := validateJSONSchema(root, bad); len(v) == 0 {
				t.Errorf("%s: schema 没拦住（这一条约束等于没写）", c.name)
			}
		}
	})
}

// TestEmitSchemaPortPatternMatchesOutputs：出口契约的 port 就是能力的 outputs[]，
// 两边必须同一套字符口径（否则「能力自己声明的出口」与「契约里列的出口」会漂）。
func TestEmitSchemaPortPatternMatchesOutputs(t *testing.T) {
	root := schemaDocValue(t, schemaEmit)

	contractFor := func(port string) []any {
		return []any{map[string]any{
			"port": port, "pick": "one", "schema": nil, "targets": []any{},
		}}
	}
	for _, port := range []string{"fix", "a.b-c_1", "out", "bad port", "a/b", "", strings.Repeat("a", 32), strings.Repeat("a", 33)} {
		schemaOK := len(validateJSONSchema(root, contractFor(port))) == 0

		doc := capabilityDoc()
		nested(t, doc, "spec")["outputs"] = []any{port}
		_, err := artifact.Parse(mustJSON(t, doc))
		outputsOK := err == nil

		if schemaOK != outputsOK {
			t.Errorf("port %q: emit schema 判 %s，artifact 的 outputs 判 %s（err: %v）",
				port, okWord(schemaOK), okWord(outputsOK), err)
		}
	}
}

// ---------- 小工具 ----------

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

func decodeJSON(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", b, err)
	}
	return v
}

func okWord(ok bool) string {
	if ok {
		return "合法"
	}
	return "非法"
}

func stringList(t *testing.T, v any) []string {
	t.Helper()
	items, ok := v.([]any)
	if !ok {
		t.Fatalf("不是字符串数组：%T", v)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("数组里有非字符串项：%T", item)
		}
		out = append(out, s)
	}
	return out
}
