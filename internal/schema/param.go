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
	Permissive bool // 「{} / 省略 / null」：怎么传都过
	Properties map[string]ParamProperty
	Required   map[string]bool
	Order      []string
}

type ParamProperty struct {
	Types       []string // 空 = 不限
	Pattern     *regexp.Regexp
	PatternText string
	Nested      *Params
	Default     any
	HasDefault  bool // 区分「没写 default」与「default: null」
}

type Issue struct {
	Field  string // 相对参数根（`repo` / `meta.dir`）
	Detail string
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
		return nil, []Problem{{Msg: "参数 schema 不是 JSON 对象"}}
	}
	var problems []Problem
	if rawType, ok := doc["type"]; ok {
		if !isObjectType(rawType) {
			problems = append(problems, Problem{Path: "type", Msg: "参数 schema.type 必须是 object"})
		}
	}
	if rawProps, ok := doc["properties"]; ok {
		var props map[string]json.RawMessage
		if err := json.Unmarshal(rawProps, &props); err != nil || props == nil {
			problems = append(problems, Problem{Path: "properties", Msg: "参数 schema.properties 不是对象"})
		} else {
			for name, rawProp := range props {
				prop, propProblems := parseProperty(name, rawProp)
				for _, problem := range propProblems {
					problem.Path = joinPath("properties."+name, problem.Path)
					problems = append(problems, problem)
				}
				s.Properties[name] = prop
				s.Order = append(s.Order, name)
			}
			sort.Strings(s.Order)
		}
	}
	if rawRequired, ok := doc["required"]; ok {
		var names []string
		if err := json.Unmarshal(rawRequired, &names); err != nil {
			problems = append(problems, Problem{Path: "required", Msg: "参数 schema.required 必须是字符串数组"})
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
		return prop, []Problem{{Msg: fmt.Sprintf("参数 %s 的 schema 不是对象", name)}}
	}
	var problems []Problem
	if rawType, ok := doc["type"]; ok {
		types, terr := paramTypes(rawType)
		if terr != nil {
			problems = append(problems, Problem{Path: "type",
				Msg: fmt.Sprintf("参数 %s：%v", name, terr)})
		}
		prop.Types = types
	}
	if rawPattern, ok := doc["pattern"]; ok {
		var pattern string
		if err := json.Unmarshal(rawPattern, &pattern); err != nil {
			problems = append(problems, Problem{Path: "pattern",
				Msg: fmt.Sprintf("参数 %s 的 pattern 必须是字符串", name)})
		} else if re, perr := CompilePattern(pattern); perr != nil {
			problems = append(problems, Problem{Path: "pattern",
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
			problems = append(problems, Problem{Path: "default",
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
	return true // 未知类型名（JSON Schema 扩展）：不做判定，放行而不是误剪
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
