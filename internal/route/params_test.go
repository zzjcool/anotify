package route

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// 本文件覆盖契约 §4 的必测矩阵 1~10 项（#114「源变量与参数变量引用」）。
//
// 断言口径一律以 docs/design/param-mapping.md 的 D 章（最新）为准：
// 单趟替换、不做模板语法、别名表降级为纯别名。

// ---------- 1. RenderTemplate：单趟替换 + 未命中保留原文 ----------

func TestRenderTemplate(t *testing.T) {
	vals := map[string]any{
		"a": float64(1),
		"b": float64(2),
		"n": nil,
		"o": map[string]any{"k": "v"},
	}
	lookup := func(ref string) (any, bool) {
		v, ok := vals[ref]
		return v, ok
	}

	cases := []struct {
		name        string
		in          string
		wantText    string
		wantMissing []string
	}{
		{"多个占位符全部命中", "PR: {{a}} by {{b}}", "PR: 1 by 2", nil},
		{"未命中保留原文并记 missing", "hello {{nope}}", "hello {{nope}}", []string{"nope"}},
		{"占位符允许内部空格", "x={{ a }}", "x=1", nil},
		{"混合命中与未命中", "{{a}}-{{x}}", "1-{{x}}", []string{"x"}},
		// 这一层是纯渲染器：lookup 说 found=true 就渲染。nil → "" 只服务于这种低层用法；
		// 求值链路上 ResolveParams 的 lookup 把 nil 归为未命中（契约 §8.5 A16），
		// 所以 `"v={{x}}"` + null 走的是下面 TestResolveParamsExplicitNullIsMiss 的 param_missing。
		{"nil 值字符串化为空", "n=[{{n}}]", "n=[]", nil},
		{"对象值字符串化为 JSON", "o={{o}}", `o={"k":"v"}`, nil},
		{"没有占位符原样返回", "pure text", "pure text", nil},
		{"半截占位符不是占位符", "PR {{title", "PR {{title", nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text, missing := RenderTemplate(c.in, lookup)
			if text != c.wantText {
				t.Fatalf("RenderTemplate(%q) = %q, want %q", c.in, text, c.wantText)
			}
			if !reflect.DeepEqual(missing, c.wantMissing) {
				t.Fatalf("missing = %v, want %v", missing, c.wantMissing)
			}
		})
	}
}

// ---------- 2. 禁止递归展开（单趟替换的硬指标） ----------

func TestRenderTemplateSinglePass(t *testing.T) {
	// 替换进去的值**绝不再被扫描**：值里的 {{b}} 必须原样留在输出里。
	lookup := func(ref string) (any, bool) {
		if ref == "a" {
			return "{{b}}", true
		}
		if ref == "b" {
			return "B", true
		}
		return nil, false
	}

	if text, _ := RenderTemplate("{{a}}", lookup); text != "{{b}}" {
		t.Fatalf("递归展开了：RenderTemplate({{a}}) = %q, want %q", text, "{{b}}")
	}
	if text, _ := RenderTemplate("x={{a}}", lookup); text != "x={{b}}" {
		t.Fatalf("递归展开了：RenderTemplate(x={{{{a}}}}) = %q, want %q", text, "x={{b}}")
	}
	// 同一个值里两次引用同一个占位符，也要各自独立判定
	if text, _ := RenderTemplate("{{a}}{{b}}", lookup); text != "{{b}}B" {
		t.Fatalf("RenderTemplate({{a}}{{b}}) = %q, want %q", text, "{{b}}B")
	}
}

func TestResolveParamsNoRecursion(t *testing.T) {
	payload := map[string]any{"a": "{{b}}", "b": "B"}

	params, missing, typeErr := ResolveParams(map[string]any{
		"whole": "{{a}}",
		"mixed": "x={{a}}",
	}, payload, nil, nil)

	if len(missing) != 0 || typeErr != nil {
		t.Fatalf("不该有 missing/typeErr：missing=%v typeErr=%v", missing, typeErr)
	}
	// 整值引用：取到的是字面 "{{b}}"，不是 "B"
	if got := params["whole"]; got != "{{b}}" {
		t.Fatalf(`params["whole"] = %v, want "{{b}}"（不得递归展开成 "B"）`, got)
	}
	// 混写：结果一律字符串，同样不再展开
	if got := params["mixed"]; got != "x={{b}}" {
		t.Fatalf(`params["mixed"] = %v, want "x={{b}}"`, got)
	}
}

// ---------- 3. 整值引用保留类型 ----------

func TestResolveParamsWholeRefKeepsType(t *testing.T) {
	payload := map[string]any{
		"n": float64(42),
		"b": true,
		"o": map[string]any{"k": "v"},
	}
	// schema 声明 num 是 integer：整值引用拿到 float64(42) 原样通过
	schema := json.RawMessage(`{"type":"object","properties":{"num":{"type":"integer"}}}`)

	params, missing, typeErr := ResolveParams(map[string]any{
		"num":   "{{n}}",
		"flag":  "{{b}}",
		"obj":   "{{o}}",
		"mixed": "n={{n}}",
	}, payload, nil, schema)

	if len(missing) != 0 || typeErr != nil {
		t.Fatalf("不该有 missing/typeErr：missing=%v typeErr=%v", missing, typeErr)
	}
	if got, ok := params["num"].(float64); !ok || got != 42 {
		t.Fatalf("num = %#v, want float64(42)（整值引用保留原类型）", params["num"])
	}
	if got, ok := params["flag"].(bool); !ok || !got {
		t.Fatalf("flag = %#v, want bool(true)", params["flag"])
	}
	wantObj := map[string]any{"k": "v"}
	if !reflect.DeepEqual(params["obj"], wantObj) {
		t.Fatalf("obj = %#v, want %#v", params["obj"], wantObj)
	}
	if got, ok := params["mixed"].(string); !ok || got != "n=42" {
		t.Fatalf("mixed = %#v, want string \"n=42\"（混写一律字符串）", params["mixed"])
	}
}

// ---------- 4. 嵌套路径 + 数组下标 ----------

func TestGetPath(t *testing.T) {
	payload := map[string]any{
		"items": []any{
			map[string]any{"name": "first"},
			map[string]any{"name": "second"},
		},
		"a":       map[string]any{"b": map[string]any{"c": float64(1)}},
		"s":       "scalar",
		"n":       float64(5),
		"nothing": nil, // 显式 null（JSON 里 `{"nothing":null}`）
	}

	cases := []struct {
		name    string
		path    string
		want    any
		wantHit bool
	}{
		{"顶层键", "n", float64(5), true},
		{"嵌套对象", "a.b.c", float64(1), true},
		{"数组下标", "items.0.name", "first", true},
		{"数组下标第二项", "items.1.name", "second", true},
		{"下标带小数点 miss", "items.0.0", nil, false},
		{"全数字路径 0.0 miss", "0.0", nil, false},
		// 契约 §8.3 A12：下标必须匹配 ^(0|[1-9][0-9]*)$（先过正则再 Atoi）
		{"正号下标 miss", "items.+1", nil, false},
		{"前导零下标 miss", "items.01", nil, false},
		{"负下标 miss", "items.-1", nil, false},
		{"科学计数法下标 miss", "items.1e2", nil, false},
		{"越界下标 miss", "items.9", nil, false},
		{"中间是标量 miss", "s.x", nil, false},
		{"末尾还想要子键 miss", "n.x", nil, false},
		{"键不存在 miss", "nope", nil, false},
		// 显式 null：路径**存在**，值空。GetPath 是纯行走器，老实报 (nil,true)；
		// “nil 一律当未命中”在求值层（ResolveParams 的 lookup）实现（契约 §8.5 A16），
		// 见 TestResolveParamsExplicitNullIsMiss。
		{"显式 null 命中但值空", "nothing", nil, true},
		{"空路径 miss", "", nil, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, hit := GetPath(payload, c.path)
			if hit != c.wantHit {
				t.Fatalf("GetPath(%q) hit = %v, want %v", c.path, hit, c.wantHit)
			}
			if c.wantHit && !reflect.DeepEqual(got, c.want) {
				t.Fatalf("GetPath(%q) = %#v, want %#v", c.path, got, c.want)
			}
		})
	}

	t.Run("nil payload 一律 miss", func(t *testing.T) {
		if _, hit := GetPath(nil, "a"); hit {
			t.Fatal("nil payload 必须 miss")
		}
	})
}

// ---------- 5. 未命中 → ParamMiss 的三种 Ref 形态 ----------

func TestResolveParamsMissingRefs(t *testing.T) {
	payload := map[string]any{"a": map[string]any{}}
	vars := map[string]any{"declared": "V"}

	params, missing, typeErr := ResolveParams(map[string]any{
		"title": "{{x}}",                         // 占位符：原样回显
		"body":  map[string]any{"from": "a.b.c"}, // from：payload 路径
		"tag":   map[string]any{"var": "titel"},  // var：$ 变量名
	}, payload, vars, nil)

	if typeErr != nil {
		t.Fatalf("不该有 typeErr：%+v", typeErr)
	}
	if len(params) != 0 {
		t.Fatalf("未命中的字段不该出现在 params 里：%v", params)
	}

	got := map[string]string{}
	for _, m := range missing {
		got[m.Field] = m.Ref
	}
	want := map[string]string{
		"title": "{{x}}",
		"body":  "payload.a.b.c",
		"tag":   "$titel",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("missing = %v, want %v", got, want)
	}
	t.Run("missing 顺序按字段名稳定", func(t *testing.T) {
		if len(missing) != 3 || missing[0].Field != "body" || missing[1].Field != "tag" || missing[2].Field != "title" {
			t.Fatalf("missing 顺序不稳定：%+v", missing)
		}
	})
}

// ---------- 6. 配置期：闭合检查先于 hasPlaceholder ----------

func TestValidateParamTemplatesPlaceholderForms(t *testing.T) {
	cases := []struct {
		name     string
		template map[string]any
		wantErr  string // "" = 通过；非空 = 必须包含的子串
	}{
		{"合法整值引用", map[string]any{"title": "{{pull_request.title}}"}, ""},
		{"合法混写", map[string]any{"title": "PR #{{pull_request.number}} 已合并"}, ""},
		{"合法占位符带空格", map[string]any{"title": "{{ pull_request.title }}"}, ""},
		{"半截占位符必须拒绝", map[string]any{"title": "PR {{title"}, "占位符没有闭合"},
		{"多一个左括号必须拒绝", map[string]any{"title": "PR {{x}} {{"}, "占位符没有闭合"},
		{"拼接表达式必须拒绝", map[string]any{"title": "{{a}}{{b}} + 1{{"}, "占位符没有闭合"},
		// 数量相等但中间不是合法占位符：原因不同，文案必须不同（契约 §8.3 A11）
		{"条件语法必须拒绝", map[string]any{"title": "{{#if x}}"}, "占位符不合法"},
		{"运算表达式必须拒绝", map[string]any{"title": "{{x + 1}}"}, "占位符不合法"},
		{"函数调用必须拒绝", map[string]any{"title": "{{f(x)}}"}, "占位符不合法"},
		{"路径以点开头", map[string]any{"title": "{{.a}}"}, "占位符不合法"},
		{"$ 变量语法必须拒绝", map[string]any{"title": "{{$x}}"}, "占位符不合法"},
		{"路径含连续点号", map[string]any{"title": "{{a..b}}"}, "的路径不合法"},
		{"路径以点结尾", map[string]any{"title": "{{a.}}"}, "的路径不合法"},
		// 只含 }} 不含 {{ 的是普通字面量（JSON 片段），必须能保存
		{"含 }} 的 JSON 字面量必须放行", map[string]any{"body": `{"a":{"b":1}}`}, ""},
		{"含单个 } 的普通字符串", map[string]any{"title": "PR {x} 已合并"}, ""},
		{"纯字面量字符串", map[string]any{"title": "没有占位符"}, ""},
		{"字面量对象不是映射声明", map[string]any{"extra": map[string]any{"k": "v"}}, ""},
		{"空模板", map[string]any{}, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := ValidateParamTemplates(c.template, nil)
			if c.wantErr == "" {
				if msg != "" {
					t.Fatalf("期望通过，得到错误：%s", msg)
				}
				return
			}
			if msg == "" {
				t.Fatalf("期望被拒（含 %q），却静默放过", c.wantErr)
			}
			if !strings.Contains(msg, c.wantErr) {
				t.Fatalf("错误消息 %q 不含 %q", msg, c.wantErr)
			}
		})
	}
}

func TestValidateParamTemplatesMappings(t *testing.T) {
	cases := []struct {
		name     string
		template map[string]any
		declared []string
		wantErr  string
	}{
		{"from 合法", map[string]any{"t": map[string]any{"from": "a.b"}}, nil, ""},
		{"from + as 合法", map[string]any{"t": map[string]any{"from": "a.b", "as": "number"}}, nil, ""},
		{"var 已声明", map[string]any{"t": map[string]any{"var": "env"}}, []string{"env"}, ""},
		{"var + default 已声明", map[string]any{"t": map[string]any{"var": "env", "default": "prod"}}, []string{"env"}, ""},
		{"var 未声明", map[string]any{"t": map[string]any{"var": "titel"}}, []string{"title"}, "未声明的变量「titel」"},
		{"var 为空", map[string]any{"t": map[string]any{"var": ""}}, []string{"env"}, "变量名为空"},
		{"from 与 var 并存", map[string]any{"t": map[string]any{"from": "a", "var": "env"}}, []string{"env"}, "只能选一个"},
		{"as 不在枚举内", map[string]any{"t": map[string]any{"from": "a", "as": "foo"}}, nil, "as 只能是 string/number/boolean/json"},
		{"as 不是字符串", map[string]any{"t": map[string]any{"from": "a", "as": 1}}, nil, "as 只能是"},
		{"as 为 integer 也算非法（配置期只认四个）", map[string]any{"t": map[string]any{"from": "a", "as": "integer"}}, nil, "as 只能是"},
		{"from 含连续点号", map[string]any{"t": map[string]any{"from": "a..b"}}, nil, "from 路径不合法"},
		{"from 以点开头", map[string]any{"t": map[string]any{"from": ".a"}}, nil, "from 路径不合法"},
		{"from 以点结尾", map[string]any{"t": map[string]any{"from": "a."}}, nil, "from 路径不合法"},
		// A15：多一个键 / from 不是字符串 → 不是映射声明，按字面量放过（派发期同样当字面量）
		{"from 非字符串 + 多键按字面量放过", map[string]any{"t": map[string]any{"from": 1, "y": 2}}, nil, ""},
		{"只写 from 但值不是字符串", map[string]any{"t": map[string]any{"from": 1}}, nil, ""},
		{"只写可选键缺 from/var", map[string]any{"t": map[string]any{"as": "string"}}, nil, "缺少 from 或 var"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := ValidateParamTemplates(c.template, c.declared)
			if c.wantErr == "" {
				if msg != "" {
					t.Fatalf("期望通过，得到错误：%s", msg)
				}
				return
			}
			if !strings.Contains(msg, c.wantErr) {
				t.Fatalf("错误消息 %q 不含 %q", msg, c.wantErr)
			}
		})
	}
}

// ---------- A15：配置期与派发期用同一个 IsMapping 口径 ----------

func TestValidateParamTemplatesMappingOrLiteral(t *testing.T) {
	// 口径必须与 ResolveParams 的映射分支一致：那边用 IsMapping(m) 判定，
	// 不是映射声明就当字面量原样传。配置期若“见到 from 键就当映射”，
	// `{"from":1,"y":2}` 就会出现「配置期 400、派发期能传」的分裂（A15）。
	t.Run("多键对象按字面量放过（且派发期确实当字面量传）", func(t *testing.T) {
		v := map[string]any{"from": 1, "y": 2}
		if msg := ValidateParamTemplates(map[string]any{"t": v}, nil); msg != "" {
			t.Fatalf("配置期应放过字面量对象，得到错误：%s", msg)
		}
		params, missing, typeErr := ResolveParams(map[string]any{"t": v}, nil, nil, nil)
		if len(missing) != 0 || typeErr != nil {
			t.Fatalf("派发期不该当映射：missing=%v typeErr=%v", missing, typeErr)
		}
		if !reflect.DeepEqual(params["t"], v) {
			t.Fatalf("t = %#v，want 原样字面量 %#v", params["t"], v)
		}
	})

	t.Run("空对象字面量不被误判成缺引用的映射", func(t *testing.T) {
		// `{"opts": {}}` 是合法字面量：派发期 IsMapping({})==false → 原样传，
		// 配置期必须同样放行（否则就是「配置期 400、派发期能传」的分裂）。
		empty := map[string]any{}
		if msg := ValidateParamTemplates(map[string]any{"opts": empty}, nil); msg != "" {
			t.Fatalf("空对象应放行，得到错误：%s", msg)
		}
	})

	t.Run("只写可选键的对象仍提示缺 from/var", func(t *testing.T) {
		msg := ValidateParamTemplates(map[string]any{"t": map[string]any{"default": 1, "as": "string"}}, nil)
		if !strings.Contains(msg, "缺少 from 或 var") {
			t.Fatalf("msg = %q，期望含「缺少 from 或 var」", msg)
		}
	})

	t.Run("from 与 var 并存仍报互斥", func(t *testing.T) {
		msg := ValidateParamTemplates(map[string]any{"t": map[string]any{"var": "x", "from": "y"}}, []string{"x"})
		if !strings.Contains(msg, "只能选一个") {
			t.Fatalf("msg = %q，期望含「只能选一个」", msg)
		}
	})

	t.Run("var 未声明仍报错", func(t *testing.T) {
		msg := ValidateParamTemplates(map[string]any{"t": map[string]any{"var": "x"}}, []string{"y"})
		if !strings.Contains(msg, "未声明的变量「x」") {
			t.Fatalf("msg = %q，期望含「未声明的变量「x」」", msg)
		}
	})
}

// ---------- A14：配置期只校验“能确定的那些参数值” ----------

func TestValidateLiteralParams(t *testing.T) {
	// 典型的能力 params_schema：to/title 都必填，n 是整数，另有一个可选嵌套对象。
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"to": {"type": "string"},
			"title": {"type": "string"},
			"n": {"type": "integer"},
			"cfg": {
				"type": "object",
				"properties": {"url": {"type": "string"}, "retries": {"type": "integer"}}
			}
		},
		"required": ["to", "title"]
	}`)

	t.Run("核心回归：required 按模板键集合判（字面量+模板混用不被误拒）", func(t *testing.T) {
		// to 的值是占位符——类型要触发时才知道，但键已经写了，必填已满足。
		if msg := ValidateLiteralParams(schema, map[string]any{"to": "{{a}}", "title": "hi"}); msg != "" {
			t.Fatalf("含占位符的必填字段不该被 required 误拒，得到错误：%s", msg)
		}
	})

	t.Run("缺必填字段报 missing required field", func(t *testing.T) {
		msg := ValidateLiteralParams(schema, map[string]any{"title": "hi"})
		if !strings.Contains(msg, `missing required field "to"`) {
			t.Fatalf("msg = %q，期望含 missing required field \"to\"", msg)
		}
	})

	t.Run("字面量类型错照样当场报", func(t *testing.T) {
		msg := ValidateLiteralParams(schema, map[string]any{"to": "a", "title": "hi", "n": "abc"})
		if !strings.Contains(msg, "params.n") {
			t.Fatalf("msg = %q，期望路径含 params.n", msg)
		}
	})

	t.Run("additionalProperties=false 拒未声明键", func(t *testing.T) {
		strict := json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":false}`)
		msg := ValidateLiteralParams(strict, map[string]any{"b": "{{x}}"})
		if !strings.Contains(msg, `unexpected field "b"`) || !strings.Contains(msg, "additionalProperties=false") {
			t.Fatalf("msg = %q，期望 unexpected field \"b\"", msg)
		}
	})

	t.Run("嵌套未解析子树整体跳过（含内层 required）", func(t *testing.T) {
		nested := json.RawMessage(`{
			"type": "object",
			"properties": {"cfg": {
				"type": "object",
				"properties": {"url": {"type": "string"}, "retries": {"type": "integer"}},
				"required": ["url"]
			}}
		}`)
		// url 是占位符：内层 required 由派发期兜底，配置期跳整棵 cfg 子树。
		if msg := ValidateLiteralParams(nested, map[string]any{
			"cfg": map[string]any{"url": "{{u}}", "retries": float64(3)},
		}); msg != "" {
			t.Fatalf("含未解析内容的子树必须整体跳过，得到错误：%s", msg)
		}
	})

	t.Run("嵌套字面量字段类型错报内层路径", func(t *testing.T) {
		msg := ValidateLiteralParams(schema, map[string]any{
			"to": "a", "title": "hi",
			"cfg": map[string]any{"url": "https://x", "retries": "abc"},
		})
		if !strings.Contains(msg, "params.cfg.") {
			t.Fatalf("msg = %q，期望路径含 params.cfg.", msg)
		}
	})

	t.Run("空 schema / 非法 schema 一律放过", func(t *testing.T) {
		if msg := ValidateLiteralParams(nil, map[string]any{"x": "abc"}); msg != "" {
			t.Fatalf("空 schema 应放过，得到：%s", msg)
		}
		if msg := ValidateLiteralParams(json.RawMessage(`{not json`), map[string]any{"x": "abc"}); msg != "" {
			t.Fatalf("非法 schema 应放过（派发期报），得到：%s", msg)
		}
	})
}

// ---------- 7. IsMapping / IsVarRef ----------

func TestIsMapping(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want bool
	}{
		{"from 形态", map[string]any{"from": "x"}, true},
		{"var 形态", map[string]any{"var": "x"}, true},
		{"from 与 var 并存不是映射", map[string]any{"var": "x", "from": "y"}, false},
		{"var 值不是字符串", map[string]any{"var": 1}, false},
		{"多一个键就不是映射", map[string]any{"from": 1, "y": 2}, false},
		{"带 default 与 as", map[string]any{"from": "x", "default": 1, "as": "string"}, true},
		{"只有 default 不是映射", map[string]any{"default": 1}, false},
		{"字符串不是映射", "{{x}}", false},
		{"nil 不是映射", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsMapping(c.in); got != c.want {
				t.Fatalf("IsMapping(%#v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestIsVarRef(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want bool
	}{
		{"非空变量名", map[string]any{"var": "env"}, true},
		{"空变量名", map[string]any{"var": ""}, false},
		{"from 形态不是 var 引用", map[string]any{"from": "a"}, false},
		{"变量名不是字符串", map[string]any{"var": 1}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsVarRef(c.in); got != c.want {
				t.Fatalf("IsVarRef(%#v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// ---------- 8. CoerceValue ----------

func TestCoerceValue(t *testing.T) {
	cases := []struct {
		name   string
		in     any
		want   string
		wantV  any
		wantOK bool
	}{
		{"数字字符串转 integer", "42", "integer", float64(42), true},
		{"非数字字符串转 integer 失败", "abc", "integer", nil, false},
		{"字符串 true 转 boolean", "true", "boolean", true, true},
		{"小数转 integer 失败", "4.5", "integer", nil, false},
		{"整值 number 保持原样", float64(42), "integer", float64(42), true},
		{"无 want 原样返回 nil", nil, "", nil, true},
		{"小数转 number 成功", "4.5", "number", 4.5, true},
		{"非整数的 float 转 integer 失败", float64(4.5), "integer", nil, false},
		{"bool true 转 number 为 1", true, "number", float64(1), true},
		{"bool false 转 number 为 0", false, "number", float64(0), true},
		{"字符串 false 转 boolean", "false", "boolean", false, true},
		{"数字 1 转 boolean", float64(1), "boolean", true, true},
		{"数字 0 转 boolean", float64(0), "boolean", false, true},
		{"数字 2 转 boolean 失败", float64(2), "boolean", nil, false},
		{"字符串 1 转 boolean 失败", "1", "boolean", nil, false},
		{"对象转 string 为 JSON", map[string]any{"k": "v"}, "string", `{"k":"v"}`, true},
		{"数组转 string 为 JSON", []any{float64(1)}, "string", `[1]`, true},
		{"数字转 string", float64(42), "string", "42", true},
		{"null 转 string 失败", nil, "string", nil, false},
		{"对象转 object 原样", map[string]any{"k": "v"}, "object", map[string]any{"k": "v"}, true},
		{"数组转 object 失败", []any{}, "object", nil, false},
		{"数组转 array 原样", []any{}, "array", []any{}, true},
		{"字符串转 array 失败", "abc", "array", nil, false},
		{"未知目标原样直传", "abc", "json", "abc", true},
		{"空 want 不做转换", float64(7), "", float64(7), true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := CoerceValue(c.in, c.want)
			if ok != c.wantOK {
				t.Fatalf("CoerceValue(%#v, %q) ok = %v, want %v", c.in, c.want, ok, c.wantOK)
			}
			if c.wantOK && !reflect.DeepEqual(got, c.wantV) {
				t.Fatalf("CoerceValue(%#v, %q) = %#v, want %#v", c.in, c.want, got, c.wantV)
			}
		})
	}
}

// ---------- 9. SchemaPropType ----------

func TestSchemaPropType(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"lines":{"type":"integer"},"title":{"type":"string"},"weird":{}}}`)

	cases := []struct {
		name   string
		schema json.RawMessage
		field  string
		want   string
	}{
		{"integer 字段", schema, "lines", "integer"},
		{"string 字段", schema, "title", "string"},
		{"声明里没有 type", schema, "weird", ""},
		{"schema 里没有该字段", schema, "nope", ""},
		{"nil schema", nil, "lines", ""},
		{"坏 schema", json.RawMessage(`{`), "lines", ""},
		{"没有 properties", json.RawMessage(`{"type":"object"}`), "lines", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SchemaPropType(c.schema, c.field); got != c.want {
				t.Fatalf("SchemaPropType(%s, %q) = %q, want %q", c.schema, c.field, got, c.want)
			}
		})
	}
}

// ---------- 10. ResolveVars ----------

func TestResolveVars(t *testing.T) {
	payload := map[string]any{
		"pull_request": map[string]any{"number": float64(7)},
	}

	t.Run("from 命中进表", func(t *testing.T) {
		got := ResolveVars([]store.VarDef{{Name: "pr", From: "pull_request.number"}}, payload)
		if !reflect.DeepEqual(got, map[string]any{"pr": float64(7)}) {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("from 未命中不进表", func(t *testing.T) {
		got := ResolveVars([]store.VarDef{{Name: "x", From: "nope.nope"}}, payload)
		if len(got) != 0 {
			t.Fatalf("未命中不该进表：%#v", got)
		}
	})
	t.Run("常量原样进表", func(t *testing.T) {
		got := ResolveVars([]store.VarDef{{Name: "env", Value: "prod", HasValue: true}}, payload)
		if !reflect.DeepEqual(got, map[string]any{"env": "prod"}) {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("空 defs 得到空表", func(t *testing.T) {
		got := ResolveVars(nil, payload)
		if got == nil || len(got) != 0 {
			t.Fatalf("got %#v, want 非 nil 空表", got)
		}
	})
	t.Run("混合：常量 + from + 未命中", func(t *testing.T) {
		got := ResolveVars([]store.VarDef{
			{Name: "env", Value: "prod", HasValue: true},
			{Name: "pr", From: "pull_request.number"},
			{Name: "missing", From: "nope"},
		}, payload)
		want := map[string]any{"env": "prod", "pr": float64(7)}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	})
}

// ---------- 补充：ResolveParams 的映射/default/别名优先 ----------

func TestResolveParamsMappings(t *testing.T) {
	payload := map[string]any{
		"a": map[string]any{"b": float64(1)},
		"n": float64(31),
		"v": "from-payload",
	}
	vars := map[string]any{"v": "from-vars"}

	t.Run("from 命中", func(t *testing.T) {
		params, missing, typeErr := ResolveParams(map[string]any{"x": map[string]any{"from": "a.b"}}, payload, vars, nil)
		if len(missing) != 0 || typeErr != nil {
			t.Fatalf("missing=%v typeErr=%v", missing, typeErr)
		}
		if !reflect.DeepEqual(params["x"], float64(1)) {
			t.Fatalf("x = %#v", params["x"])
		}
	})
	t.Run("from 未命中用 default", func(t *testing.T) {
		params, missing, _ := ResolveParams(map[string]any{
			"x": map[string]any{"from": "nope", "default": "dflt"},
		}, payload, vars, nil)
		if len(missing) != 0 {
			t.Fatalf("有 default 不该报 missing：%v", missing)
		}
		if params["x"] != "dflt" {
			t.Fatalf("x = %#v, want dflt", params["x"])
		}
	})
	t.Run("var 命中别名表", func(t *testing.T) {
		params, _, _ := ResolveParams(map[string]any{"x": map[string]any{"var": "v"}}, payload, vars, nil)
		if params["x"] != "from-vars" {
			t.Fatalf("x = %#v, want from-vars", params["x"])
		}
	})
	t.Run("var 未命中不回退 payload", func(t *testing.T) {
		params, missing, _ := ResolveParams(map[string]any{"x": map[string]any{"var": "n"}}, payload, map[string]any{}, nil)
		if len(params) != 0 {
			t.Fatalf("var 不该回退 payload 路径：%#v", params)
		}
		if len(missing) != 1 || missing[0].Ref != "$n" {
			t.Fatalf("missing = %+v, want Ref=$n", missing)
		}
	})
	t.Run("名字冲突时别名优先", func(t *testing.T) {
		params, _, _ := ResolveParams(map[string]any{"x": "{{v}}", "y": "x={{v}}"}, payload, vars, nil)
		if params["x"] != "from-vars" {
			t.Fatalf("x = %#v, want from-vars（别名优先）", params["x"])
		}
		if params["y"] != "x=from-vars" {
			t.Fatalf("y = %#v", params["y"])
		}
	})
	t.Run("from 带 $ 前缀（兼容老写法）", func(t *testing.T) {
		// 旧文档把变量写成 `$name`，所以 `{from:"$v"}` 会由 lookup 剥掉 `$` 再查别名表。
		// 这是 **lookup 的防御性兼容**，不是「支持 {{$x}}」：`{{$x}}` 在配置期就被拒
		// （见 TestValidateParamTemplatesDollarRefRejected），两件事互不相干。
		params, _, _ := ResolveParams(map[string]any{"x": map[string]any{"from": "$v"}}, payload, vars, nil)
		if params["x"] != "from-vars" {
			t.Fatalf("x = %#v, want from-vars", params["x"])
		}
	})
	t.Run("映射的 as 优先于 schema", func(t *testing.T) {
		schema := json.RawMessage(`{"properties":{"x":{"type":"string"}}}`)
		params, _, typeErr := ResolveParams(map[string]any{
			"x": map[string]any{"from": "n", "as": "number"},
		}, payload, nil, schema)
		if typeErr != nil {
			t.Fatalf("typeErr = %+v", typeErr)
		}
		if !reflect.DeepEqual(params["x"], float64(31)) {
			t.Fatalf("x = %#v, want float64(31)（as=number 优先）", params["x"])
		}
	})
	t.Run("别名表里是 nil 视为未命中并降级 payload", func(t *testing.T) {
		params, _, _ := ResolveParams(map[string]any{"x": "{{v}}"}, map[string]any{"v": "P"}, map[string]any{"v": nil}, nil)
		if params["x"] != "P" {
			t.Fatalf("x = %#v, want P", params["x"])
		}
	})
}

func TestResolveParamsTypeError(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"lines":{"type":"integer"},"title":{"type":"string"}}}`)

	t.Run("混写结果转不成 integer", func(t *testing.T) {
		params, _, typeErr := ResolveParams(map[string]any{"lines": "加 {{n}} 行"},
			map[string]any{"n": "abc"}, nil, schema)
		if params != nil {
			t.Fatalf("类型错时 params 应为 nil，得到 %#v", params)
		}
		if typeErr == nil {
			t.Fatal("期望 ParamTypeErr")
		}
		if typeErr.Field != "lines" || typeErr.Want != "integer" {
			t.Fatalf("typeErr = %+v", typeErr)
		}
		if got := stringifyValue(typeErr.Got); got != "加 abc 行" {
			t.Fatalf("Got = %q, want 原值「加 abc 行」", got)
		}
	})
	t.Run("整值引用字符串按 schema 转 integer", func(t *testing.T) {
		params, _, typeErr := ResolveParams(map[string]any{"lines": "{{n}}"},
			map[string]any{"n": "31"}, nil, schema)
		if typeErr != nil {
			t.Fatalf("typeErr = %+v", typeErr)
		}
		if !reflect.DeepEqual(params["lines"], float64(31)) {
			t.Fatalf("lines = %#v, want float64(31)", params["lines"])
		}
	})
	t.Run("字面量不做类型转换", func(t *testing.T) {
		// schema 说 lines 是 integer，但用户写的就是字面量 "abc"：
		// 尊重用户写的，类型错由 ValidateParams 报 param_schema。
		params, missing, typeErr := ResolveParams(map[string]any{"lines": "abc"}, nil, nil, schema)
		if len(missing) != 0 || typeErr != nil {
			t.Fatalf("missing=%v typeErr=%v", missing, typeErr)
		}
		if params["lines"] != "abc" {
			t.Fatalf("lines = %#v, want abc（字面量不转换）", params["lines"])
		}
	})
	t.Run("含未命中占位符时不报类型错", func(t *testing.T) {
		// 值本身不完整：先报 param_missing，别用 param_type 盖住真正的病因
		params, missing, typeErr := ResolveParams(map[string]any{"lines": "{{n}}"},
			map[string]any{}, nil, schema)
		if typeErr != nil {
			t.Fatalf("不该有 typeErr：%+v", typeErr)
		}
		if len(params) != 0 || len(missing) != 1 || missing[0].Ref != "{{n}}" {
			t.Fatalf("params=%#v missing=%+v", params, missing)
		}
	})
}

func TestResolveParamsEmptyTemplate(t *testing.T) {
	params, missing, typeErr := ResolveParams(nil, nil, nil, nil)
	if params == nil {
		t.Fatal("空模板要返回空 map（不是 nil）")
	}
	if len(params) != 0 || len(missing) != 0 || typeErr != nil {
		t.Fatalf("params=%#v missing=%v typeErr=%v", params, missing, typeErr)
	}
}

func TestResolveParamsLiteralClone(t *testing.T) {
	obj := map[string]any{"k": "v"}
	arr := []any{"a"}
	params, _, _ := ResolveParams(map[string]any{"o": obj, "a": arr, "n": float64(5), "b": true}, nil, nil, nil)

	if !reflect.DeepEqual(params["o"], obj) || !reflect.DeepEqual(params["a"], arr) {
		t.Fatalf("字面量要原样克隆：%#v", params)
	}
	// 克隆必须是深拷贝：改输出不能影响模板
	params["o"].(map[string]any)["k"] = "changed"
	if obj["k"] != "v" {
		t.Fatal("字面量对象被共享了（不是克隆）")
	}
	if params["n"] != float64(5) || params["b"] != true {
		t.Fatalf("字面量标量要原样：%#v", params)
	}
}

// ---------- 11. A4：只含 `}}` 的合法字面量必须能保存 ----------

func TestValidateParamTemplatesLiteralBraces(t *testing.T) {
	t.Run("只含 }} 的 JSON 片段可以保存", func(t *testing.T) {
		// 用户往参数里塞一段 JSON 文本：解码后是 {"a":{"b":1}}。
		// 结尾的 }} 是 JSON 的收尾括号，不是占位符的残骸——没有 {{ 就没有占位符
		// 可言，闭合检查必须直接放行（契约 §8.1 A4）。
		var template map[string]any
		if err := json.Unmarshal([]byte(`{"body": "{\"a\":{\"b\":1}}"}`), &template); err != nil {
			t.Fatalf("测试数据必须能解码：%v", err)
		}
		body, _ := template["body"].(string)
		if body != `{"a":{"b":1}}` {
			t.Fatalf("解码结果 = %q，测试数据不对", body)
		}
		if msg := ValidateParamTemplates(template, nil); msg != "" {
			t.Fatalf("只含 }} 的字面量必须放行，得到错误：%s", msg)
		}
	})

	t.Run("只含单个 } 的普通字符串可以保存", func(t *testing.T) {
		if msg := ValidateParamTemplates(map[string]any{"title": "PR {x} 已合并"}, nil); msg != "" {
			t.Fatalf("单个 } 是普通字符，必须放行，得到错误：%s", msg)
		}
	})

	t.Run("出现 {{ 却写不全时仍然拒绝", func(t *testing.T) {
		msg := ValidateParamTemplates(map[string]any{"title": "PR {{title"}, nil)
		if !strings.Contains(msg, "占位符没有闭合") {
			t.Fatalf("msg = %q，期望含「占位符没有闭合」", msg)
		}
	})
}

// ---------- 12. A11：闭合检查的文案分级 ----------

func TestValidateParamTemplatesFormTiers(t *testing.T) {
	// 同样是错，原因不同 → 文案必须不同（契约 §8.3 A11）：
	//   ① {{ 与 }} 数量不等（没写全）→「没有闭合」
	//   ② 数量相等但残留（写了但不合法）→「不合法」，并指出不支持运算符/括号/函数/$
	t.Run("数量不等走没有闭合", func(t *testing.T) {
		for _, s := range []string{"PR {{x", "PR {{x}} {{", "{{"} {
			msg := ValidateParamTemplates(map[string]any{"title": s}, nil)
			if !strings.Contains(msg, "占位符没有闭合") {
				t.Fatalf("ValidateParamTemplates(%q) = %q，期望含「占位符没有闭合」", s, msg)
			}
		}
	})

	t.Run("数量相等但形式不合法走不合法文案", func(t *testing.T) {
		for _, s := range []string{"{{x + 1}}", "{{f(x)}}", "{{#if x}}", "{{.a}}", "{{$x}}"} {
			msg := ValidateParamTemplates(map[string]any{"title": s}, nil)
			if !strings.Contains(msg, "占位符不合法") {
				t.Fatalf("ValidateParamTemplates(%q) = %q，期望含「占位符不合法」", s, msg)
			}
			if strings.Contains(msg, "没有闭合") {
				t.Fatalf("ValidateParamTemplates(%q) = %q：数量是 1:1，不该说没闭合", s, msg)
			}
		}
	})

	t.Run("完整占位符不受影响", func(t *testing.T) {
		if msg := ValidateParamTemplates(map[string]any{"title": "PR #{{n}} 已合并"}, nil); msg != "" {
			t.Fatalf("合法混写必须放行，得到：%s", msg)
		}
	})
}

// ---------- 13. A1：{{$name}} 不再识别（配置期拒，存量值派发期当字面量） ----------

func TestValidateParamTemplatesDollarRefRejected(t *testing.T) {
	t.Run("{{$env}} 配置期被拒且文案是不合法", func(t *testing.T) {
		// 占位符正则以标识符字符起始，`$` 不是合法字符（契约 §2.1 裁决）。
		// 即使 env 已声明也只能用 {var:"env"} 引用，不能写成 {{$env}}。
		msg := ValidateParamTemplates(map[string]any{"title": "{{$env}}"}, []string{"env"})
		if msg == "" {
			t.Fatal("{{$env}} 必须被拒，却静默放过")
		}
		if !strings.Contains(msg, "占位符不合法") {
			t.Fatalf("msg = %q，期望含「占位符不合法」", msg)
		}
	})

	t.Run("混写里的 {{$env}} 同样被拒", func(t *testing.T) {
		msg := ValidateParamTemplates(map[string]any{"title": "env={{$env}}"}, []string{"env"})
		if !strings.Contains(msg, "占位符不合法") {
			t.Fatalf("msg = %q，期望含「占位符不合法」", msg)
		}
	})

	t.Run("存量 {{$x}} 值在派发期按字面量原样透传", func(t *testing.T) {
		// 历史配置已经存进去了：派发期不能报错、不能展开，原样当文本传。
		params, missing, typeErr := ResolveParams(map[string]any{
			"whole": "{{$x}}",
			"mixed": "v={{$x}}",
		}, map[string]any{"x": "X"}, nil, nil)
		if len(missing) != 0 || typeErr != nil {
			t.Fatalf("存量值不该报 missing/typeErr：missing=%v typeErr=%v", missing, typeErr)
		}
		if params["whole"] != "{{$x}}" {
			t.Fatalf("whole = %#v，want 字面量 \"{{$x}}\"", params["whole"])
		}
		if params["mixed"] != "v={{$x}}" {
			t.Fatalf("mixed = %#v，want 字面量 \"v={{$x}}\"", params["mixed"])
		}
	})
}

// ---------- 14. 非二次展开：映射形态（from / var / default）同样单趟 ----------

func TestResolveParamsMappingNoRecursion(t *testing.T) {
	payload := map[string]any{"a": "{{b}}", "b": "B"}
	vars := map[string]any{"v": "{{b}}"}

	t.Run("{from} 取到的值里的占位符不再展开", func(t *testing.T) {
		params, missing, typeErr := ResolveParams(map[string]any{
			"x": map[string]any{"from": "a"},
		}, payload, vars, nil)
		if len(missing) != 0 || typeErr != nil {
			t.Fatalf("missing=%v typeErr=%v", missing, typeErr)
		}
		if params["x"] != "{{b}}" {
			t.Fatalf("x = %#v，want \"{{b}}\"（不得展开成 \"B\"）", params["x"])
		}
	})

	t.Run("{var} 取到的值里的占位符不再展开", func(t *testing.T) {
		params, missing, typeErr := ResolveParams(map[string]any{
			"x": map[string]any{"var": "v"},
		}, payload, vars, nil)
		if len(missing) != 0 || typeErr != nil {
			t.Fatalf("missing=%v typeErr=%v", missing, typeErr)
		}
		if params["x"] != "{{b}}" {
			t.Fatalf("x = %#v，want \"{{b}}\"", params["x"])
		}
	})

	t.Run("default 里的占位符不再展开", func(t *testing.T) {
		// payload 里没有 a：走 default。default 是**字面值**，不是模板。
		params, missing, typeErr := ResolveParams(map[string]any{
			"x": map[string]any{"from": "a", "default": "{{b}}"},
		}, map[string]any{"b": "B"}, vars, nil)
		if len(missing) != 0 || typeErr != nil {
			t.Fatalf("missing=%v typeErr=%v", missing, typeErr)
		}
		if params["x"] != "{{b}}" {
			t.Fatalf("x = %#v，want \"{{b}}\"（default 不展开）", params["x"])
		}
	})

	t.Run("{var} 命中时不看 default", func(t *testing.T) {
		params, _, _ := ResolveParams(map[string]any{
			"x": map[string]any{"var": "v", "default": "D"},
		}, payload, vars, nil)
		if params["x"] != "{{b}}" {
			t.Fatalf("x = %#v，want \"{{b}}\"", params["x"])
		}
	})
}

// ---------- 15. A7：{var} 取到 nil 等于未命中（与 {{x}} 口径一致） ----------

func TestResolveParamsVarNilIsMiss(t *testing.T) {
	t.Run("nil 变量用 default 兜底", func(t *testing.T) {
		params, missing, typeErr := ResolveParams(map[string]any{
			"title": map[string]any{"var": "title", "default": "(无标题)"},
		}, nil, map[string]any{"title": nil}, nil)
		if len(missing) != 0 || typeErr != nil {
			t.Fatalf("有 default 不该报 missing：missing=%v typeErr=%v", missing, typeErr)
		}
		if params["title"] != "(无标题)" {
			t.Fatalf("title = %#v，want (无标题)", params["title"])
		}
	})

	t.Run("nil 变量无 default 报 $title", func(t *testing.T) {
		params, missing, typeErr := ResolveParams(map[string]any{
			"title": map[string]any{"var": "title"},
		}, nil, map[string]any{"title": nil}, nil)
		if typeErr != nil {
			t.Fatalf("不该有 typeErr：%+v", typeErr)
		}
		if len(params) != 0 {
			t.Fatalf("nil 变量不该出现在 params 里：%#v", params)
		}
		if len(missing) != 1 || missing[0].Field != "title" || missing[0].Ref != "$title" {
			t.Fatalf("missing = %+v，want Field=title Ref=$title", missing)
		}
	})

	t.Run("{var} 与 {{x}} 对 nil 的口径一致", func(t *testing.T) {
		vars := map[string]any{"title": nil}
		mappingParams, mappingMiss, _ := ResolveParams(map[string]any{
			"t": map[string]any{"var": "title"},
		}, nil, vars, nil)
		placeholderParams, placeholderMiss, _ := ResolveParams(map[string]any{
			"t": "{{title}}",
		}, nil, vars, nil)

		if len(mappingParams) != 0 || len(placeholderParams) != 0 {
			t.Fatalf("nil 不该被当成取到了值：mapping=%#v placeholder=%#v", mappingParams, placeholderParams)
		}
		if len(mappingMiss) != 1 || len(placeholderMiss) != 1 {
			t.Fatalf("两种写法都该各报一条 missing：mapping=%+v placeholder=%+v", mappingMiss, placeholderMiss)
		}
		if mappingMiss[0].Ref != "$title" || placeholderMiss[0].Ref != "{{title}}" {
			t.Fatalf("Ref 形态不对：mapping=%q placeholder=%q", mappingMiss[0].Ref, placeholderMiss[0].Ref)
		}
	})
}

// ---------- 16. A16：nil 一律视为未命中（payload 显式 null 也一样） ----------

func TestResolveParamsExplicitNullIsMiss(t *testing.T) {
	// JSON 里的显式 `null` 与「键不存在」在用户眼里是同一件事：
	// 这次事件没有这个值。静默当成「取到了 null」会把混写拼成空串
	// （`"v={{x}}"` → `"v="`），这是无提示的数据错误（契约 §8.5 A16）。
	payload := map[string]any{
		"x": nil,
		"a": map[string]any{"b": nil},
	}

	t.Run("整值引用 {{x}} 取到 null → ParamMiss", func(t *testing.T) {
		params, missing, typeErr := ResolveParams(map[string]any{"t": "{{x}}"}, payload, nil, nil)
		if typeErr != nil {
			t.Fatalf("不该有 typeErr：%+v", typeErr)
		}
		if len(params) != 0 {
			t.Fatalf("null 不该出现在 params 里：%#v", params)
		}
		if len(missing) != 1 || missing[0].Field != "t" || missing[0].Ref != "{{x}}" {
			t.Fatalf("missing = %+v，want Field=t Ref={{x}}", missing)
		}
	})

	t.Run("混写 v={{x}} 取到 null → ParamMiss，不得静默拼成 \"v=\"", func(t *testing.T) {
		// 混写有未命中时，值保留**原文**（`v={{x}}`，占位符不被吞掉）并记 missing，
		// 上游按 param_missing 剪枝；关键是绝不能静默变成 `"v="`。
		params, missing, typeErr := ResolveParams(map[string]any{"t": "v={{x}}"}, payload, nil, nil)
		if typeErr != nil {
			t.Fatalf("不该有 typeErr：%+v", typeErr)
		}
		if len(missing) != 1 || missing[0].Ref != "{{x}}" {
			t.Fatalf("missing = %+v，want Ref={{x}}", missing)
		}
		if params["t"] == "v=" {
			t.Fatalf("null 被静默拼成空串：%#v", params["t"])
		}
		if params["t"] != "v={{x}}" {
			t.Fatalf("t = %#v，want 原文 \"v={{x}}\"", params["t"])
		}
	})

	t.Run("嵌套路径的末段是 null → 同样算未命中", func(t *testing.T) {
		params, missing, _ := ResolveParams(map[string]any{"t": map[string]any{"from": "a.b"}}, payload, nil, nil)
		if len(params) != 0 {
			t.Fatalf("null 不该出现在 params 里：%#v", params)
		}
		if len(missing) != 1 || missing[0].Ref != "payload.a.b" {
			t.Fatalf("missing = %+v，want Ref=payload.a.b", missing)
		}
	})

	t.Run("{from} 取到 null 且有 default → 用 default", func(t *testing.T) {
		params, missing, typeErr := ResolveParams(map[string]any{
			"t": map[string]any{"from": "x", "default": "D"},
		}, payload, nil, nil)
		if len(missing) != 0 || typeErr != nil {
			t.Fatalf("有 default 不该报 missing：missing=%+v typeErr=%v", missing, typeErr)
		}
		if params["t"] != "D" {
			t.Fatalf("t = %#v，want D", params["t"])
		}
	})
}

// ---------- 补充：剪枝原因常量（W4 在 dispatch 里用） ----------

func TestParamPruneReasons(t *testing.T) {
	if ReasonParamMissing != "param_missing" {
		t.Fatalf("ReasonParamMissing = %q", ReasonParamMissing)
	}
	if ReasonParamType != "param_type" {
		t.Fatalf("ReasonParamType = %q", ReasonParamType)
	}
	if got := PruneEventType(ReasonParamMissing); got != "pruned.param_missing" {
		t.Fatalf("PruneEventType(param_missing) = %q", got)
	}
	if got := PruneEventType(ReasonParamType); got != "pruned.param_type" {
		t.Fatalf("PruneEventType(param_type) = %q", got)
	}
}
