package schema

import (
	"fmt"
	"sort"
	"strings"
)

type Problem struct { // 一处 schema 自身的写法问题
	Path string `json:"path"` // 出错关键字的 JSON 路径（如 properties.a.type）
	Msg  string `json:"msg"`  // 人话原因
}

func (p Problem) String() string { return p.Path + ": " + p.Msg }

var schemaTypeNames = []string{"string", "integer", "number", "boolean", "object", "array", "null"}

var numericKeywords = []string{ // 受支持的数值边界关键字：值不是数字 = 约束静默无效
	"minimum", "maximum",
	"minLength", "maxLength",
	"minItems", "maxItems",
	"minProperties", "maxProperties",
}

func ValidateSchema(sch map[string]any) []Problem { // 校验 schema 自身写得对不对（不校验值）
	var out []Problem // Value 对未知 type 只是「不匹配」，笔误会拖到每个值都失败才暴露，所以解析期先判
	validateSchemaNode(sch, "", &out)
	return out
}

func validateSchemaNode(sch map[string]any, at string, out *[]Problem) {
	if sch == nil {
		return
	}
	validateTypeKeyword(sch, at, out)
	if raw, ok := sch["required"]; ok && !isStringList(raw) {
		addProblem(out, joinAt(at, "required"), "required 必须是字符串数组")
	}
	if raw, ok := sch["enum"]; ok {
		list, isList := raw.([]any)
		switch {
		case !isList:
			addProblem(out, joinAt(at, "enum"), "enum 必须是数组")
		case len(list) == 0:
			addProblem(out, joinAt(at, "enum"), "enum 不能是空数组（没有任何值能通过）")
		}
	}
	if raw, ok := sch["pattern"]; ok {
		pat, isStr := raw.(string)
		if !isStr {
			addProblem(out, joinAt(at, "pattern"), "pattern 必须是字符串")
		} else if _, err := CompilePattern(pat); err != nil {
			addProblem(out, joinAt(at, "pattern"), "pattern %q 不可用: %v", pat, err)
		}
	}
	for _, key := range numericKeywords {
		raw, ok := sch[key]
		if !ok {
			continue
		}
		if _, isNum := toFloat(raw); !isNum {
			addProblem(out, joinAt(at, key), "%s 必须是数字", key)
		}
	}
	validateSchemaChildren(sch, at, out)
}

func validateTypeKeyword(sch map[string]any, at string, out *[]Problem) {
	raw, ok := sch["type"]
	if !ok {
		return
	}
	path := joinAt(at, "type")
	switch t := raw.(type) {
	case string:
		if !knownTypeName(t) {
			addProblem(out, path, "未知类型 %q（可选 %s）", t, strings.Join(schemaTypeNames, "/"))
		}
	case []any:
		if len(t) == 0 {
			addProblem(out, path, "type 数组不能为空")
		}
		for i, e := range t {
			name, isStr := e.(string)
			if !isStr {
				addProblem(out, fmt.Sprintf("%s[%d]", path, i), "type 数组的元素必须是字符串")
				continue
			}
			if !knownTypeName(name) {
				addProblem(out, fmt.Sprintf("%s[%d]", path, i), "未知类型 %q（可选 %s）", name, strings.Join(schemaTypeNames, "/"))
			}
		}
	default:
		addProblem(out, path, "type 必须是字符串或字符串数组")
	}
}

func validateSchemaChildren(sch map[string]any, at string, out *[]Problem) { // 递归受控子集的容器关键字，其余不求值
	if raw, ok := sch["properties"]; ok && raw != nil {
		props, isMap := raw.(map[string]any)
		if !isMap {
			addProblem(out, joinAt(at, "properties"), "properties 必须是对象（字段名 → schema）")
		} else {
			names := make([]string, 0, len(props))
			for name := range props {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				validateChildSchema(props[name], joinAt(at, "properties."+name), out)
			}
		}
	}
	for _, key := range []string{"additionalProperties", "items"} {
		raw, ok := sch[key]
		if !ok {
			continue
		}
		if _, isList := raw.([]any); isList {
			continue // draft-07 的元组 items 不在受控子集内：忽略，不假装支持
		}
		validateChildSchema(raw, joinAt(at, key), out)
	}
	if raw, ok := sch["prefixItems"]; ok {
		list, isList := raw.([]any)
		if !isList {
			addProblem(out, joinAt(at, "prefixItems"), "prefixItems 必须是数组（按位置的 schema）")
			return
		}
		for i, e := range list {
			validateChildSchema(e, fmt.Sprintf("%s[%d]", joinAt(at, "prefixItems"), i), out)
		}
	}
}

func validateChildSchema(raw any, at string, out *[]Problem) {
	switch child := raw.(type) {
	case nil, bool:
		return
	case map[string]any:
		validateSchemaNode(child, at, out)
	default:
		addProblem(out, at, "schema 必须是对象或布尔（true / false）")
	}
}

func addProblem(out *[]Problem, path, format string, args ...any) {
	*out = append(*out, Problem{Path: path, Msg: fmt.Sprintf(format, args...)})
}

func joinAt(at, seg string) string {
	if at == "" {
		return seg
	}
	return at + "." + seg
}

func knownTypeName(name string) bool {
	for _, known := range schemaTypeNames {
		if name == known {
			return true
		}
	}
	return false
}

func isStringList(raw any) bool {
	switch list := raw.(type) {
	case []string:
		return true
	case []any:
		for _, e := range list {
			if _, ok := e.(string); !ok {
				return false
			}
		}
		return true
	}
	return false
}
