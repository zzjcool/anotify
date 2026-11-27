package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type Params struct {
	Permissive        bool // 「{} / 省略 / null」：怎么传都过
	Properties        map[string]ParamProperty
	Required          map[string]bool
	Order             []string
	RejectsAdditional bool // 顶层 additionalProperties:false：未声明输入键不是「忽略」而是拒绝（#580）
}

type ParamProperty struct {
	Types       []string // 空 = 不限
	Pattern     *regexp.Regexp
	PatternText string
	Nested      *Params
	Default     any
	HasDefault  bool           // 区分「没写 default」与「default: null」
	Schema      map[string]any // enum / minimum / items … 由 schema.Value 执行（#580），Check 只管自己那几条
}

type Issue struct {
	Field  string // 相对参数根（`repo` / `meta.dir`）
	Detail string
}

const (
	KeywordRoot        = "root"        // 整份 schema 不是 JSON 对象（Path 为空）
	KeywordType        = "type"        // type 写坏 / 不接受 object / 类型名未知
	KeywordProperties  = "properties"  // properties 不是对象
	KeywordProperty    = "property"    // properties.<name> 的值不是对象
	KeywordPattern     = "pattern"     // pattern 不是字符串 / 编译不过
	KeywordDefault     = "default"     // default 不是合法 JSON
	KeywordRequired    = "required"    // required 不是字符串数组
	KeywordUnsupported = "unsupported" // 不在受控子集里的关键字（#580）
)

// paramsSchemaKeywords 是受控子集白名单（#580）：表外写入期 400 / 运行期 param_schema；纯注解不求值。
var paramsSchemaKeywords = map[string]struct{}{
	"type": {}, "properties": {}, "required": {}, "additionalProperties": {},
	"items": {}, "prefixItems": {}, "enum": {}, "pattern": {},
	"minLength": {}, "maxLength": {}, "minimum": {}, "maximum": {},
	"minItems": {}, "maxItems": {}, "minProperties": {}, "maxProperties": {},
	"default": {},
	"title":   {}, "description": {}, "examples": {}, "example": {},
	"$comment": {}, "$schema": {}, "$id": {},
	"deprecated": {}, "readOnly": {}, "writeOnly": {},
}

var paramsSchemaKeywordList = []string{ // 400 消息里展示的支持列表（顺序固定）
	"type", "properties", "required", "additionalProperties", "items", "prefixItems",
	"enum", "pattern", "minLength", "maxLength", "minimum", "maximum",
	"minItems", "maxItems", "minProperties", "maxProperties", "default",
}

// ParseParams 是 #612 的唯一判据：问题非空 ⇔ 运行期不可用（写入期 400 / 运行期剪枝）。
func ParseParams(raw json.RawMessage) (*Params, []Problem) {
	s := &Params{Properties: map[string]ParamProperty{}, Required: map[string]bool{}}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" || string(trimmed) == "{}" {
		s.Permissive = true
		return s, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &doc); err != nil || doc == nil {
		return nil, []Problem{{Keyword: KeywordRoot, Msg: "参数 schema 不是 JSON 对象"}}
	}
	var problems []Problem // 白名单/未知类型名（#580）：写入期 400、运行期 param_schema 剪枝
	var generic map[string]any
	if err := json.Unmarshal(trimmed, &generic); err == nil && generic != nil {
		problems = append(problems, checkParamKeywords(generic, "", true)...)
		if raw, ok := generic["additionalProperties"]; ok {
			if allow, isBool := raw.(bool); isBool && !allow {
				s.RejectsAdditional = true
			}
		}
	}
	if rawType, ok := doc["type"]; ok {
		if !isObjectType(rawType) {
			problems = append(problems, Problem{Path: "type", Keyword: KeywordType, Msg: "参数 schema.type 必须是 object"})
		}
	}
	if rawProps, ok := doc["properties"]; ok {
		var props map[string]json.RawMessage
		if err := json.Unmarshal(rawProps, &props); err != nil || props == nil {
			problems = append(problems, Problem{Path: "properties", Keyword: KeywordProperties,
				Msg: "参数 schema.properties 不是对象"})
		} else {
			names := make([]string, 0, len(props))
			for name := range props {
				names = append(names, name)
			}
			sort.Strings(names) // 遍历顺序固定：problems[0] / causes[0] / 剪枝 detail 不随进程变化
			for _, name := range names {
				prop, propProblems := parseProperty(name, props[name])
				for _, problem := range propProblems {
					problem.Path = joinPath("properties."+name, problem.Path)
					problems = append(problems, problem)
				}
				s.Properties[name] = prop
			}
			s.Order = names
		}
	}
	if rawRequired, ok := doc["required"]; ok {
		var names []string
		if err := json.Unmarshal(rawRequired, &names); err != nil {
			problems = append(problems, Problem{Path: "required", Keyword: KeywordRequired,
				Msg: "参数 schema.required 必须是字符串数组"})
		}
		for _, name := range names {
			s.Required[name] = true
		}
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return s, nil
}

func parseProperty(name string, raw json.RawMessage) (ParamProperty, []Problem) {
	var prop ParamProperty
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return prop, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &doc); err != nil || doc == nil {
		return prop, []Problem{{Keyword: KeywordProperty, Msg: fmt.Sprintf("参数 %s 的 schema 不是对象", name)}}
	}
	if generic := map[string]any(nil); json.Unmarshal(trimmed, &generic) == nil { // 整份属性 schema 留给 schema.Value
		prop.Schema = generic
	}
	var problems []Problem
	if rawType, ok := doc["type"]; ok {
		types, terr := paramTypes(rawType)
		if terr != nil {
			problems = append(problems, Problem{Path: "type", Keyword: KeywordType,
				Msg: fmt.Sprintf("参数 %s：%v", name, terr)})
		}
		prop.Types = types
	}
	if rawPattern, ok := doc["pattern"]; ok {
		var pattern string
		if err := json.Unmarshal(rawPattern, &pattern); err != nil {
			problems = append(problems, Problem{Path: "pattern", Keyword: KeywordPattern,
				Msg: fmt.Sprintf("参数 %s 的 pattern 必须是字符串", name)})
		} else if re, perr := CompilePattern(pattern); perr != nil {
			problems = append(problems, Problem{Path: "pattern", Keyword: KeywordPattern,
				Msg: fmt.Sprintf("参数 %s 的 pattern %q 不可用：%v", name, pattern, perr)})
		} else {
			prop.Pattern = re
			prop.PatternText = pattern
		}
	}
	if rawDefault, ok := doc["default"]; ok {
		dec := json.NewDecoder(bytes.NewReader(rawDefault))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			problems = append(problems, Problem{Path: "default", Keyword: KeywordDefault,
				Msg: fmt.Sprintf("参数 %s 的 default 不是合法 JSON", name)})
		} else {
			prop.Default = v
			prop.HasDefault = true
		}
	}
	if rawProps, ok := doc["properties"]; ok {
		wrapped, err := json.Marshal(map[string]json.RawMessage{
			"type":       json.RawMessage(`"object"`),
			"properties": rawProps,
		})
		if err == nil {
			if rawRequired, rok := doc["required"]; rok {
				var merged map[string]json.RawMessage
				if json.Unmarshal(wrapped, &merged) == nil {
					merged["required"] = rawRequired
					if again, merr := json.Marshal(merged); merr == nil {
						wrapped = again
					}
				}
			}
			nested, nestedProblems := ParseParams(wrapped)
			problems = append(problems, nestedProblems...)
			prop.Nested = nested
		}
	}
	if len(problems) > 0 {
		return prop, problems
	}
	return prop, nil
}

func (p ParamProperty) Check(path string, value any) *Issue {
	if len(p.Types) > 0 {
		matched := false
		for _, t := range p.Types {
			if valueMatchesType(t, value) {
				matched = true
				break
			}
		}
		if !matched {
			return &Issue{Field: path,
				Detail: fmt.Sprintf("参数 %s 类型应为 %s，实际 %s", path, strings.Join(p.Types, "|"), jsonKind(value))}
		}
	}
	if p.Pattern != nil {
		if s, ok := value.(string); ok && !p.Pattern.MatchString(s) {
			return &Issue{Field: path,
				Detail: fmt.Sprintf("参数 %s 应匹配 pattern %q，实际 %q", path, p.PatternText, s)}
		}
	}
	if p.Nested != nil {
		if obj, ok := value.(map[string]any); ok {
			if issue := p.Nested.checkObject(obj, path); issue != nil {
				return issue
			}
		}
	}
	return nil
}

func (s *Params) checkObject(obj map[string]any, prefix string) *Issue {
	if s.Permissive {
		return nil
	}
	missing := []string{}
	for _, name := range s.Order {
		prop := s.Properties[name]
		value, ok := obj[name]
		if !ok {
			if s.Required[name] {
				missing = append(missing, prefix+"."+name)
			}
			continue
		}
		if issue := prop.Check(prefix+"."+name, value); issue != nil {
			return issue
		}
	}
	if len(missing) > 0 {
		return &Issue{Field: missing[0], Detail: "缺少 " + strings.Join(missing, ", ")}
	}
	return nil
}

func (p ParamProperty) CheckKeywords(path string, value any) *Issue {
	if p.Schema == nil {
		return nil
	}
	if msg := Value(p.Schema, value, path); msg != "" {
		return &Issue{Field: path, Detail: msg}
	}
	return nil
}

// checkParamKeywords 判白名单与 type 名（#580）；根节点的 type 由 ParseParams 自己判，不重复报。
func checkParamKeywords(doc map[string]any, at string, isRoot bool) []Problem {
	var out []Problem
	keys := make([]string, 0, len(doc))
	for key := range doc {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, ok := paramsSchemaKeywords[key]; ok {
			continue
		}
		out = append(out, Problem{Path: joinPath(at, key), Keyword: KeywordUnsupported,
			Msg: fmt.Sprintf("不支持 schema 关键字 %q（受支持：%s）", key, strings.Join(paramsSchemaKeywordList, " / "))})
	}
	if raw, ok := doc["type"]; ok && !isRoot {
		if types, err := genericTypes(raw); err == nil {
			for _, name := range types {
				if !knownTypeName(name) {
					out = append(out, Problem{Path: joinPath(at, "type"), Keyword: KeywordType,
						Msg: fmt.Sprintf("未知类型 %q（可选 %s）", name, strings.Join(schemaTypeNames, "/"))})
				}
			}
		}
	}
	if props, ok := doc["properties"].(map[string]any); ok {
		names := make([]string, 0, len(props))
		for name := range props {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if child, ok := props[name].(map[string]any); ok {
				out = append(out, checkParamKeywords(child, joinPath(at, "properties."+name), false)...)
			}
		}
	}
	for _, key := range []string{"items", "additionalProperties"} {
		if child, ok := doc[key].(map[string]any); ok {
			out = append(out, checkParamKeywords(child, joinPath(at, key), false)...)
		}
	}
	if prefix, ok := doc["prefixItems"].([]any); ok {
		for i, item := range prefix {
			if child, ok := item.(map[string]any); ok {
				out = append(out, checkParamKeywords(child, fmt.Sprintf("%s[%d]", joinPath(at, "prefixItems"), i), false)...)
			}
		}
	}
	return out
}

func genericTypes(raw any) ([]string, error) { // 泛型解码的 type 值 → 类型名
	switch t := raw.(type) {
	case string:
		if t == "" {
			return nil, fmt.Errorf("type 为空")
		}
		return []string{t}, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok || s == "" {
				return nil, fmt.Errorf("type 必须是字符串或字符串数组")
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("type 必须是字符串或字符串数组")
}

func isObjectType(raw json.RawMessage) bool {
	types, err := paramTypes(raw)
	if err != nil {
		return false
	}
	for _, t := range types {
		if t == "object" {
			return true
		}
	}
	return false
}

func paramTypes(raw json.RawMessage) ([]string, error) {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if single == "" {
			return nil, fmt.Errorf("type 为空")
		}
		return []string{single}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, fmt.Errorf("type 必须是字符串或字符串数组")
	}
	for _, t := range many {
		if t == "" {
			return nil, fmt.Errorf("type 数组里有空值")
		}
	}
	return many, nil
}

func valueMatchesType(want string, value any) bool {
	switch want {
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		switch value.(type) {
		case json.Number, float64, float32, int, int64, int32:
			return true
		}
		return false
	case "integer":
		switch v := value.(type) {
		case json.Number:
			return !strings.ContainsAny(string(v), ".eE")
		case float64:
			return v == float64(int64(v))
		case int, int64, int32:
			return true
		}
		return false
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "null":
		return value == nil
	}
	return false // 未知类型名：解析期已拒绝（checkParamKeywords），这里 fail-closed
}

func jsonKind(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case json.Number, float64:
		return "number"
	}
	return "unknown"
}

func joinPath(base, seg string) string {
	if seg == "" {
		return base
	}
	if base == "" {
		return seg
	}
	return base + "." + seg
}
