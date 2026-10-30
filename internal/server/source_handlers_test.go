package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// TestWriteSourceErrReservedNameIs400 保留来源名的错误必须映射成 400。
//
// 为什么值得单独测：这是本单新增的**用户可见状态码**。它落到 default 就是 500，
// 而 500 会让用户以为中枢坏了（实际改个名字就好），且 mux 级测试没有
// /v1/sources 的 harness（console_test.go 不挂它），e2e 里的源名也全是正常名
// —— 没有这几行，这条映射零覆盖（变异验证已证明：删掉那个 case 全绿）。
//
// 直接调 writeSourceErr 而不起 mux：它只依赖 w/err/op，零依赖，
// 所以 `&sourceHandler{}` 就够；顺带把「其它错误仍走 500」一并钉住
// （避免将来把别的 store 错误误吞成 400，那会把服务端故障伪装成用户输入问题）。
func TestWriteSourceErrReservedNameIs400(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"两层包装的保留名错误", fmt.Errorf("create source: %w", store.ErrReservedEventSource), http.StatusBadRequest},
		{"改名路径的保留名错误", fmt.Errorf("update source: %w", store.ErrReservedEventSource), http.StatusBadRequest},
		{"其它错误仍走 default（500）", fmt.Errorf("create source: 磁盘满了"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h := &sourceHandler{}
			h.writeSourceErr(rr, c.err, "create")
			if rr.Code != c.want {
				t.Fatalf("status = %d, want %d (body=%s)", rr.Code, c.want, rr.Body.String())
			}
		})
	}

	// 400 的 body 要能让人知道「改个名字就好」，所以必须带上保留名说明
	t.Run("400 的 body 含保留名说明", func(t *testing.T) {
		rr := httptest.NewRecorder()
		(&sourceHandler{}).writeSourceErr(rr, fmt.Errorf("create source: %w", store.ErrReservedEventSource), "create")
		if body := rr.Body.String(); !strings.Contains(body, "route") {
			t.Fatalf("body = %s, want 含 \"route\" 的说明", body)
		}
	})
}

// TestValidateTargetParamsConfigTimeRules 是配置期的三道闸（契约 §4 第 21 项、§2.4）。
//
// 为什么走方法级测试而不是 mux 级：本包没有 /v1/sources 的 harness
// （console_test.go 只挂 console 路由），而这三条判定的全部复杂性都在
// validateTargetParams 里，差一个 mux 不会多验证任何东西。所以直接用真库
// （store.Open）+ 手建的能力定义调方法：真 schema 校验是真的跑了一遗，
// 而不是拿 fake 对赌。
//
// 四条必须同时成立（少一条就是把错误推到触发期）：
//
//  1. `{var:"titel"}` 未声明 → 错误含变量名（issue 的"坑①"）；
//  2. 半截占位符 `"PR {{title"` → 错误（不能当字面量存下来）；
//  3. 字面量 `"abc"` 对 integer schema → 错误（既有行为不回归）；
//  4. 只含占位符的 `"{{n}}"` 对 integer schema → **不**报错（类型要求值后才知道）。
func TestValidateTargetParamsConfigTimeRules(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	uid, err := db.InsertUser(ctx, "", "srcval", "Src Val")
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	org, err := db.EnsureOrgForUser(ctx, uid, "srcval")
	if err != nil {
		t.Fatalf("ensure org: %v", err)
	}

	capDef := &store.CapabilityVersion{
		OrgID: org.ID, Name: "analyze", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`),
		Source:       store.CapSourceCustom,
	}
	if err := db.CreateCapability(ctx, capDef); err != nil {
		t.Fatalf("create capability: %v", err)
	}

	h := &sourceHandler{db: db}
	targets := func(params map[string]any) []store.SourceTarget {
		return []store.SourceTarget{{CapabilityID: capDef.ID, Params: params}}
	}

	cases := []struct {
		name         string
		params       map[string]any
		declaredVars []string
		wantErr      []string // 空 = 必须通过
	}{
		{
			name:    "引用未声明的变量",
			params:  map[string]any{"title": map[string]any{"var": "titel"}},
			wantErr: []string{"titel"}, // 错误必须含变量名，用户才知道该声明什么
		},
		{
			name:    "半截占位符",
			params:  map[string]any{"title": "PR {{title"},
			wantErr: []string{"占位符没有闭合"},
		},
		{
			name:    "字面量类型错（既有行为不回归）",
			params:  map[string]any{"n": "abc"},
			wantErr: []string{"params.n", "integer"},
		},
		{
			name:   "只含占位符的参数不破 schema 校验",
			params: map[string]any{"n": "{{n}}"},
		},
		{
			name:   "合法的字面量",
			params: map[string]any{"n": 42},
		},
		{
			name:   "映射声明的 as 不报错",
			params: map[string]any{"n": map[string]any{"from": "lines", "as": "number"}},
		},
		{
			name:         "已声明的变量可以被 {var} 引用",
			params:       map[string]any{"title": map[string]any{"var": "titel"}},
			declaredVars: []string{"titel"},
		},
		{
			name:    "from 路径形态不合法",
			params:  map[string]any{"title": map[string]any{"from": "a..b"}},
			wantErr: []string{"from 路径不合法"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := h.validateTargetParams(ctx, org.ID, targets(c.params), c.declaredVars)
			if len(c.wantErr) == 0 {
				if err != nil {
					t.Fatalf("不该报错, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应报错（含 %v），但通过了", c.wantErr)
			}
			for _, want := range c.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("错误 %q 不含 %q", err.Error(), want)
				}
			}
		})
	}

	// 能力不存在时仍要报「不存在」，不能因为新增了模板校验就把这条丢了
	t.Run("出口能力不存在", func(t *testing.T) {
		err := h.validateTargetParams(ctx, org.ID, []store.SourceTarget{{
			CapabilityID: "cap_not_exist",
			Params:       map[string]any{"n": 1},
		}}, nil)
		if err == nil || !strings.Contains(err.Error(), "不存在") {
			t.Fatalf("err = %v, want 含「不存在」", err)
		}
	})

	// 没配参数（或空对象）直接跳过：schema 校验交给派发期
	t.Run("空参数不校验", func(t *testing.T) {
		if err := h.validateTargetParams(ctx, org.ID, targets(nil), nil); err != nil {
			t.Fatalf("空参数不该报错: %v", err)
		}
		if err := h.validateTargetParams(ctx, org.ID, targets(map[string]any{}), nil); err != nil {
			t.Fatalf("空对象不该报错: %v", err)
		}
	})
}

// TestValidateTargetParamsRequiredWithPlaceholder 是 A14 的方法级覆盖：
//
// 只含占位符的参数**不该**因为 required 被误拒，真缺键 / 字面量类型错仍要报。
// 这条钉的是 validateTargetParams 的口径（调用 route.ValidateLiteralParams），
// HTTP 级见 TestCreateSourceRequiredParamsWithPlaceholderHTTP。
func TestValidateTargetParamsRequiredWithPlaceholder(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	uid, err := db.InsertUser(ctx, "", "srcreq", "Src Req")
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	org, err := db.EnsureOrgForUser(ctx, uid, "srcreq")
	if err != nil {
		t.Fatalf("ensure org: %v", err)
	}

	// required 两个键、都是 string：剔掉占位符的实现会在第一条用例上误拒
	capDef := &store.CapabilityVersion{
		OrgID: org.ID, Name: "notify", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object","required":["to","title"],
			"properties":{"to":{"type":"string"},"title":{"type":"string"}}}`),
		Source: store.CapSourceCustom,
	}
	if err := db.CreateCapability(ctx, capDef); err != nil {
		t.Fatalf("create capability: %v", err)
	}

	h := &sourceHandler{db: db}
	targets := func(params map[string]any) []store.SourceTarget {
		return []store.SourceTarget{{CapabilityID: capDef.ID, Params: params}}
	}

	cases := []struct {
		name    string
		params  map[string]any
		wantErr []string // 空 = 必须通过
	}{
		{
			// 修前（剔掉占位符再跑 ValidateParams）会报 missing required "to"
			name:   "占位符占着 required 的键也算满足",
			params: map[string]any{"to": "{{pull_request.user.login}}", "title": "PR 已合并"},
		},
		{
			name:   "映射声明占着 required 的键也算满足",
			params: map[string]any{"to": map[string]any{"from": "pull_request.user.login"}, "title": "x"},
		},
		{
			name:    "真缺 required 的键",
			params:  map[string]any{"title": "hi"},
			wantErr: []string{"missing required field", "to"},
		},
		{
			name:    "字面量类型错仍当场拦",
			params:  map[string]any{"to": 123, "title": "hi"},
			wantErr: []string{"params.to"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := h.validateTargetParams(ctx, org.ID, targets(c.params), nil)
			if len(c.wantErr) == 0 {
				if err != nil {
					t.Fatalf("不该报错, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应报错（含 %v），但通过了", c.wantErr)
			}
			for _, want := range c.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("错误 %q 不含 %q", err.Error(), want)
				}
			}
		})
	}
}

// TestMergeSourceConfig 钉住 PATCH 的 config 合并语义（issue #114 A6、契约 §2.4 ④）。
//
// 这是纯函数层：不碰 HTTP、不碰库，只回答一个问题——「新配置没带的键，旧值还在吗」。
// 为什么要这个函数：config.vars / config.payload 是**另一个入口**写入的声明，
// 控制台的「保存 expr / token」不会带上它们；整份替换等于静默丢数据，
// 而且紧接着会用被清空的配置去校验既有 targets（改个 cron 表达式都会 400）。
//
// 边界同样重要：只继承 vars / payload 两键（契约 §2.4 ④ 明确点了两个键），
// 其余键（expr / tz / token / event）必须被新配置覆盖，否则「改时区」会改不动。
func TestMergeSourceConfig(t *testing.T) {
	const (
		curVars    = `"vars":[{"name":"n","from":"lines"}]`
		curPayload = `"payload":{"region":"cn"}`
		curJSON    = `{"expr":"0 3 * * *","tz":"Local",` + curVars + `,` + curPayload + `}`
	)
	cur := json.RawMessage(curJSON)

	fields := func(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
		t.Helper()
		if !json.Valid(raw) {
			t.Fatalf("合并结果不是合法 JSON: %s", raw)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("合并结果不是 JSON 对象: %s (%v)", raw, err)
		}
		return m
	}

	t.Run("next 不带 vars/payload → 从 cur 继承", func(t *testing.T) {
		got := fields(t, mergeSourceConfig(cur, json.RawMessage(`{"expr":"0 4 * * *","tz":"UTC"}`)))
		if string(got["expr"]) != `"0 4 * * *"` {
			t.Fatalf("expr 该用新值, got %s", got["expr"])
		}
		if string(got["tz"]) != `"UTC"` {
			t.Fatalf("tz 该用新值, got %s", got["tz"])
		}
		if v := string(got["vars"]); !strings.Contains(v, `"from":"lines"`) {
			t.Fatalf("vars 该被继承, got %q", v)
		}
		if v := string(got["payload"]); !strings.Contains(v, `"region":"cn"`) {
			t.Fatalf("payload 该被继承, got %q", v)
		}
	})

	// 这条同时是任务书里「next 只有 {"expr":...} 时 cur 的未知键不丢」的验收点：
	// 对 handler 而言 vars / payload 就是「它不认识、但绝不能丢」的键。
	t.Run("next 只有 expr 时 cur 的声明键不丢", func(t *testing.T) {
		got := fields(t, mergeSourceConfig(cur, json.RawMessage(`{"expr":"0 4 * * *"}`)))
		if _, ok := got["vars"]; !ok {
			t.Fatalf("vars 丢了: %s", got["vars"])
		}
		if _, ok := got["payload"]; !ok {
			t.Fatalf("payload 丢了: %s", got["payload"])
		}
	})

	t.Run("显式 vars: [] 是真的清空", func(t *testing.T) {
		got := fields(t, mergeSourceConfig(cur, json.RawMessage(`{"expr":"0 4 * * *","vars":[]}`)))
		if v := string(got["vars"]); v != `[]` {
			t.Fatalf("显式空数组该原样保留, got %q", v)
		}
		// 只清 vars，payload 不受影响（两个键各自独立）
		if _, ok := got["payload"]; !ok {
			t.Fatalf("payload 不该被 vars:[] 连坐清掉")
		}
	})

	t.Run("显式 payload: {} 是真的清空", func(t *testing.T) {
		got := fields(t, mergeSourceConfig(cur, json.RawMessage(`{"payload":{}}`)))
		if v := string(got["payload"]); v != `{}` {
			t.Fatalf("显式空对象该原样保留, got %q", v)
		}
		if _, ok := got["vars"]; !ok {
			t.Fatalf("vars 不该被 payload:{} 连坐清掉")
		}
	})

	t.Run("next 带新 vars → 用新的（不是两份相加）", func(t *testing.T) {
		got := fields(t, mergeSourceConfig(cur, json.RawMessage(`{"vars":[{"name":"x","value":"1"}]}`)))
		v := string(got["vars"])
		if !strings.Contains(v, `"x"`) || strings.Contains(v, `"lines"`) {
			t.Fatalf("该整份使用新 vars, got %q", v)
		}
	})

	t.Run("next 非法 JSON → 原样返回 cur", func(t *testing.T) {
		for _, bad := range []string{`{"expr":`, `not json`, ``} {
			if got := string(mergeSourceConfig(cur, json.RawMessage(bad))); got != curJSON {
				t.Fatalf("next=%q 时应原样返回 cur, got %s", bad, got)
			}
		}
	})

	t.Run("next 不是 JSON 对象 → 原样返回 cur", func(t *testing.T) {
		for _, bad := range []string{`null`, `[1,2]`, `"str"`, `42`} {
			if got := string(mergeSourceConfig(cur, json.RawMessage(bad))); got != curJSON {
				t.Fatalf("next=%s 时应原样返回 cur, got %s", bad, got)
			}
		}
	})

	t.Run("next 自己的未知键原样保留（不重新编码整个 config）", func(t *testing.T) {
		got := fields(t, mergeSourceConfig(cur, json.RawMessage(
			`{"expr":"0 4 * * *","tz":"Asia/Shanghai","custom":{"deep":[1,2]}}`)))
		if v := string(got["custom"]); v != `{"deep":[1,2]}` {
			t.Fatalf("未知键 custom 该原样保留, got %q", v)
		}
		if v := string(got["tz"]); v != `"Asia/Shanghai"` {
			t.Fatalf("tz 该保留, got %q", v)
		}
	})

	t.Run("两键之外不继承（契约 §2.4 只点了 vars/payload）", func(t *testing.T) {
		got := fields(t, mergeSourceConfig(cur, json.RawMessage(`{"expr":"0 4 * * *"}`)))
		if v, ok := got["tz"]; ok {
			t.Fatalf("cur 的 tz 不该被继承（那是新配置的事）, got %s", v)
		}
	})

	t.Run("cur 为空时正常放行 next", func(t *testing.T) {
		next := `{"expr":"0 4 * * *","vars":[{"name":"n","from":"a"}]}`
		if got := string(mergeSourceConfig(nil, json.RawMessage(next))); got != next {
			t.Fatalf("cur 为空应原样返回 next, got %s", got)
		}
	})
}

// sourceHTTPEnv 是 /v1/sources 的 HTTP 级测试环境（真库 + 真 handler）。
//
// 为什么值得多起一层：A6 的 bug 症状是「用户点保存拿到 400」，能看见它的
// 唯一位置是 HTTP 状态码和 body。方法级测试（上面的 TestValidateTargetParams…）
// 直接喂 params，永远看不到「PATCH 把 vars 抹掉之后再校验」这条路径。
//
// resolveOrg 是唯一的注入点（真实实现要会话中间件），替成一个固定 org 即可；
// 其余全是生产代码路径：解析请求体 → 校验 → 落库 → 重读 → 组装视图。
type sourceHTTPEnv struct {
	db    *store.DB
	ctx   context.Context
	h     *sourceHandler
	orgID string
	capID string
}

// sourceHTTPDefaultSchema 是 sourceHTTPEnv 的默认能力 schema：
// 一个「要 integer + 有 string」的 schema，配置期校验才有东西可判。
const sourceHTTPDefaultSchema = `{"type":"object","properties":{"n":{"type":"integer"},"t":{"type":"string"}}}`

func newSourceHTTPEnv(t *testing.T) *sourceHTTPEnv {
	t.Helper()
	return newSourceHTTPEnvWithSchema(t, sourceHTTPDefaultSchema)
}

// newSourceHTTPEnvWithSchema 同上，但指定能力 schema（A14 的 required 用例需要它）。
func newSourceHTTPEnvWithSchema(t *testing.T, paramsSchema string) *sourceHTTPEnv {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	uid, err := db.InsertUser(ctx, "", "srchttp", "Src HTTP")
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	org, err := db.EnsureOrgForUser(ctx, uid, "srchttp")
	if err != nil {
		t.Fatalf("ensure org: %v", err)
	}

	capDef := &store.CapabilityVersion{
		OrgID: org.ID, Name: "analyze", Version: 1,
		ParamsSchema: json.RawMessage(paramsSchema),
		Source:       store.CapSourceCustom,
	}
	if err := db.CreateCapability(ctx, capDef); err != nil {
		t.Fatalf("create capability: %v", err)
	}

	h := &sourceHandler{
		db: db,
		resolveOrg: func(w http.ResponseWriter, r *http.Request, requested string) (string, bool) {
			return org.ID, true
		},
	}
	return &sourceHTTPEnv{db: db, ctx: ctx, h: h, orgID: org.ID, capID: capDef.ID}
}

// expand 把用例模板里的占位符换成本环境的真实 ID / 唯一源名。
func (e *sourceHTTPEnv) expand(tmpl, name string) string {
	return strings.NewReplacer("__NAME__", name, "__CAP__", e.capID).Replace(tmpl)
}

func (e *sourceHTTPEnv) create(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	e.h.create(rr, httptest.NewRequest(http.MethodPost, "/v1/sources", strings.NewReader(body)))
	return rr
}

func (e *sourceHTTPEnv) patch(t *testing.T, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	e.h.update(rr, httptest.NewRequest(http.MethodPatch, "/v1/sources/"+id, strings.NewReader(body)), id)
	return rr
}

// sourceIDFrom 从创建回包{"source":{"id":...}}里取 id。
func sourceIDFrom(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var resp struct {
		Source struct {
			ID string `json:"id"`
		} `json:"source"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析创建回包失败: %v (body=%s)", err, rr.Body.String())
	}
	if resp.Source.ID == "" {
		t.Fatalf("创建回包没有 source.id: %s", rr.Body.String())
	}
	return resp.Source.ID
}

// configFields 把一段 config 解成键值表，断言它是 JSON 对象。
func configFields(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	if !json.Valid(raw) {
		t.Fatalf("config 不是合法 JSON: %s", raw)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("config 不是 JSON 对象: %s (%v)", raw, err)
	}
	return m
}

// TestCreateSourceConfigTimeErrorsHTTP 是配置期三道闸的 HTTP 级覆盖（契约 §4 第 21 项）。
//
// 与 TestValidateTargetParamsConfigTimeRules 的分工：那个测的是校验函数本身，
// 这个测的是「用户 POST /v1/sources 时会看到什么」——400 + 能读懂的原因。
// 少了这层，handler 里把错误吞掉 / 换成 500 / 丢掉变量名都不会被发现。
func TestCreateSourceConfigTimeErrorsHTTP(t *testing.T) {
	e := newSourceHTTPEnv(t)

	cases := []struct {
		name     string
		tmpl     string
		wantCode int
		wantBody []string // body 必须含的子串（按子串断言：错误正文会随上下文略变）
	}{
		{
			// issue 的"坑①"：{var} 是显式引用，拼错必须当场 400，且错误里要带变量名
			name:     "params 引用未声明的变量",
			tmpl:     `{"name":"__NAME__","kind":"manual","targets":[{"capabilityId":"__CAP__","params":{"t":{"var":"titel"}}}]}`,
			wantCode: http.StatusBadRequest,
			wantBody: []string{"titel"},
		},
		{
			// 半截占位符绝不能因为正则匹配不到就被当字面量存下来
			name:     "半截占位符",
			tmpl:     `{"name":"__NAME__","kind":"manual","targets":[{"capabilityId":"__CAP__","params":{"t":"PR {{title"}}]}`,
			wantCode: http.StatusBadRequest,
			wantBody: []string{"占位符没有闭合"},
		},
		{
			// 契约 §2.4 第二行：含占位符的值类型要触发时才知道，配置期不许拿 schema 误拒
			name:     "只含占位符的 integer 参数不因 schema 被拒",
			tmpl:     `{"name":"__NAME__","kind":"manual","targets":[{"capabilityId":"__CAP__","params":{"n":"{{n}}"}}]}`,
			wantCode: http.StatusCreated,
		},
		{
			// 既有行为不回归：纯字面量的类型错仍要在配置期拦住
			name:     "字面量类型错",
			tmpl:     `{"name":"__NAME__","kind":"manual","targets":[{"capabilityId":"__CAP__","params":{"n":"abc"}}]}`,
			wantCode: http.StatusBadRequest,
			wantBody: []string{"params.n", "integer"},
		},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := e.create(t, e.expand(c.tmpl, fmt.Sprintf("http-src-%d", i)))
			if rr.Code != c.wantCode {
				t.Fatalf("status = %d, want %d (body=%s)", rr.Code, c.wantCode, rr.Body.String())
			}
			for _, want := range c.wantBody {
				if !strings.Contains(rr.Body.String(), want) {
					t.Fatalf("body 不含 %q: %s", want, rr.Body.String())
				}
			}
		})
	}
}

// TestPatchSourceKeepsConfigVarsHTTP 是 A6 的验收用例：PATCH 只改 expr 不许抹掉 vars。
//
// bug 原文（issue #114 复审 A6）：PATCH 走的是 `cur.Config = req.Config` 整份替换。
// 于是「控制台把 cron 从 0 3 * * * 改成 0 4 * * *」这一次保存：
//  1. 把已存的 config.vars / config.payload 静默抹掉；
//  2. 紧接着用被清空的 VarNames() 校验既有 targets，含 {var:"x"} 的源直接 400
//     ——用户只是想改个时间，却被告知"引用了未声明的变量"。
//
// 所以这里的第一断言是**状态码 200**（bug 时是 400），第二断言是库里/回包里
// vars 与 payload 都还在（bug 时被抹掉），第三断言是"显式清空仍然有效"
// （不能为了修 bug 把清空语义也一起弄没了）。
func TestPatchSourceKeepsConfigVarsHTTP(t *testing.T) {
	e := newSourceHTTPEnv(t)

	// 建一个「vars 声明 + payload 打底 + 出口用 {var} 引用」的 cron 源：
	// 三个要素齐了，才能同时覆盖"丢数据"和"校验被连带击穿"两个症状。
	createBody := e.expand(`{
		"name": "cron-vars",
		"kind": "cron",
		"config": {
			"expr": "0 3 * * *",
			"vars": [{"name": "n", "from": "lines"}],
			"payload": {"region": "cn"}
		},
		"targets": [{"capabilityId": "__CAP__", "params": {"n": {"var": "n"}}}]
	}`, "cron-vars")
	rr := e.create(t, createBody)
	if rr.Code != http.StatusCreated {
		t.Fatalf("建源 status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	id := sourceIDFrom(t, rr)

	// ① 只改 expr（控制台保存 cron 的主路径）：必须 200，且 vars/payload 不被抹掉
	patchBody := `{"config":{"expr":"0 4 * * *"}}`
	rr = e.patch(t, id, patchBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH 只改 expr 应 200（A6：整份替换会清空 vars，{var} 引用随即 400），"+
			"got %d body=%s", rr.Code, rr.Body.String())
	}

	checkConfig := func(t *testing.T, raw json.RawMessage, where string) {
		t.Helper()
		fields := configFields(t, raw)
		if v := string(fields["expr"]); v != `"0 4 * * *"` {
			t.Fatalf("%s: expr 该被 PATCH 改成 0 4 * * *, got %s", where, v)
		}
		if v := string(fields["vars"]); !strings.Contains(v, `"from":"lines"`) {
			t.Fatalf("%s: config.vars 被 PATCH 抹掉了, got %q", where, v)
		}
		if v := string(fields["payload"]); !strings.Contains(v, `"region":"cn"`) {
			t.Fatalf("%s: config.payload 被 PATCH 抹掉了, got %q", where, v)
		}
	}

	var resp struct {
		Source struct {
			Config json.RawMessage `json:"config"`
		} `json:"source"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析 PATCH 回包失败: %v (body=%s)", err, rr.Body.String())
	}
	checkConfig(t, resp.Source.Config, "PATCH 回包")

	// ② 光回包好看不够：库里也必须真的存着（下次重启/重读就靠它）
	stored, err := e.db.GetSource(e.ctx, e.orgID, id)
	if err != nil {
		t.Fatalf("重读源: %v", err)
	}
	checkConfig(t, stored.Config, "库里的源")

	// ③ 显式清空仍然有效：传 vars:[] 才是“真的清空”，不能被合并逻辑吞掉。
	//
	// 用另一个源：上面那个的出口用 {var:"n"} 引用着 n，清掉 vars 本身就该 400
	// （那是正确的连带报错，不是本用例要测的）。这个源的出口不带参数。
	rr = e.create(t, e.expand(`{
		"name": "cron-plain",
		"kind": "cron",
		"config": {"expr": "0 3 * * *", "vars": [{"name": "n", "from": "lines"}], "payload": {"region": "cn"}},
		"targets": ["__CAP__"]
	}`, "cron-plain"))
	if rr.Code != http.StatusCreated {
		t.Fatalf("建源 cron-plain status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	plainID := sourceIDFrom(t, rr)

	rr = e.patch(t, plainID, `{"config":{"expr":"0 4 * * *","vars":[]}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("显式清空 vars 应 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	stored, err = e.db.GetSource(e.ctx, e.orgID, plainID)
	if err != nil {
		t.Fatalf("重读源: %v", err)
	}
	fields := configFields(t, stored.Config)
	if v := string(fields["vars"]); v != `[]` {
		t.Fatalf("显式 vars:[] 该在库里是空数组, got %q", v)
	}
	if v := string(fields["payload"]); !strings.Contains(v, `"region":"cn"`) {
		t.Fatalf("只清 vars 不该连坐 payload, got %q", v)
	}
}

// TestCreateSourceRequiredParamsWithPlaceholderHTTP 是 A14 的验收用例（最重要的一条）。
//
// bug 原文（第二轮复审 A14）：validateTargetParams 把「剔掉占位符/映射后的字面子集」
// 直接跑 route.ValidateParams，于是 schema 的 required 按**子集**判——
// 能力 schema `required:["to","title"]` 配上
// `{"to":"{{pull_request.user.login}}","title":"PR 已合并"}`：剔完只剩 title，
// 当场报 `params: missing required field "to"` → 400。
// 可这条配置在派发时求值后完全合法（dry-run 能过）。用户点保存却拿 400，
// 而且提示的字段是他明明填了的 —— 最伤信任的一种不一致。
//
// 所以第一断言必须是 **201**（bug 时是 400）。同时不能把闸门整体拆了：
// 真缺 required 键仍要 400（且说清哪个字段），字面量类型错仍要 400（带 params.to 路径）。
func TestCreateSourceRequiredParamsWithPlaceholderHTTP(t *testing.T) {
	// required 两个都是 string：分别是「占位符占着键」和「真字面量」的场景
	e := newSourceHTTPEnvWithSchema(t,
		`{"type":"object","required":["to","title"],`+
			`"properties":{"to":{"type":"string"},"title":{"type":"string"}}}`)

	cases := []struct {
		name     string
		params   string // 出口 params 的 JSON 字面量
		wantCode int
		wantBody []string // 空 = 不检查 body
	}{
		{
			// 修前：字面子集只有 title → missing required "to" → 400
			name:     "占位符占着 required 的键 → 201",
			params:   `{"to":"{{pull_request.user.login}}","title":"PR 已合并"}`,
			wantCode: http.StatusCreated,
		},
		{
			// 映射声明同理：值要触发时才有，不能拿它去凑 required
			name:     "映射声明占着 required 的键 → 201",
			params:   `{"to":{"from":"pull_request.user.login"},"title":"PR 已合并"}`,
			wantCode: http.StatusCreated,
		},
		{
			// 闸门不能拆：真没写 to 就还是 400，并且说清缺哪个字段
			name:     "真缺 required 的键 → 400",
			params:   `{"title":"hi"}`,
			wantCode: http.StatusBadRequest,
			wantBody: []string{"missing required field", "to"},
		},
		{
			// 既有行为不回归：字面量类型错仍旧当场拦住，错误带 params.to 路径
			name:     "字面量类型错 → 400 且带字段路径",
			params:   `{"to":123,"title":"hi"}`,
			wantCode: http.StatusBadRequest,
			wantBody: []string{"params.to"},
		},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := e.expand(`{"name":"__NAME__","kind":"manual","targets":[{"capabilityId":"__CAP__","params":`+
				c.params+`}]}`, fmt.Sprintf("req-src-%d", i))
			rr := e.create(t, body)
			if rr.Code != c.wantCode {
				t.Fatalf("status = %d, want %d (body=%s)", rr.Code, c.wantCode, rr.Body.String())
			}
			for _, want := range c.wantBody {
				if !strings.Contains(rr.Body.String(), want) {
					t.Fatalf("body 不含 %q: %s", want, rr.Body.String())
				}
			}
		})
	}
}

// TestPatchSourceNullConfigIsNoopHTTP 是 A18 的验收用例：`{"config": null}` = 本次不改配置。
//
// 解码成 json.RawMessage 的 null 不是 nil（是字面量 "null"），光靠 `req.Config != nil`
// 拦不住它；若哪天有人把 null 当"清空 config"实现，一次带 null 的保存就会连带抹掉
// config.vars / config.payload（即 A6 那个坑的另一种触发路径）。所以这里断言：
// 状态码 200、请求里的其它字段照常生效、库里的 config 一字未变。
func TestPatchSourceNullConfigIsNoopHTTP(t *testing.T) {
	e := newSourceHTTPEnv(t)

	createBody := e.expand(`{
		"name": "null-cfg",
		"kind": "cron",
		"config": {"expr": "0 3 * * *", "tz": "Local", "vars": [{"name": "n", "from": "lines"}], "payload": {"region": "cn"}},
		"targets": ["__CAP__"]
	}`, "null-cfg")
	rr := e.create(t, createBody)
	if rr.Code != http.StatusCreated {
		t.Fatalf("建源 status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	id := sourceIDFrom(t, rr)

	before, err := e.db.GetSource(e.ctx, e.orgID, id)
	if err != nil {
		t.Fatalf("重读源: %v", err)
	}

	// 同时改 name：证明这次请求本身不是被整体忽略，只有 config 那一段是 no-op
	rr = e.patch(t, id, `{"name":"null-cfg-renamed","config":null}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH config:null 应 200（A18：视为不改 config）, got %d body=%s", rr.Code, rr.Body.String())
	}

	after, err := e.db.GetSource(e.ctx, e.orgID, id)
	if err != nil {
		t.Fatalf("重读源: %v", err)
	}
	if after.Name != "null-cfg-renamed" {
		t.Fatalf("name 该被改掉, got %q", after.Name)
	}
	if string(after.Config) != string(before.Config) {
		t.Fatalf("config:null 不该动库里的 config:\n before=%s\n after =%s", before.Config, after.Config)
	}
	// 逐键再钉一遍：哪怕将来 config 被规范化重编码，vars/payload/expr 也必须等价
	fields := configFields(t, after.Config)
	if v := string(fields["expr"]); v != `"0 3 * * *"` {
		t.Fatalf("expr 该原样保留, got %s", v)
	}
	if v := string(fields["vars"]); !strings.Contains(v, `"from":"lines"`) {
		t.Fatalf("config.vars 被 config:null 抹掉了, got %q", v)
	}
	if v := string(fields["payload"]); !strings.Contains(v, `"region":"cn"`) {
		t.Fatalf("config.payload 被 config:null 抹掉了, got %q", v)
	}
}
