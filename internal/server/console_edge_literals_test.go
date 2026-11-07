package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// ---------- 边的字面量覆盖（P0-c，设计 §3.3 / §7 P0.3） ----------

// seedTargetWithSchema 建一个**声明了字段字典**的下游能力（字面量按它校验）。
func seedTargetWithSchema(t *testing.T, env *consoleEnv, name, schema string) *store.CapabilityVersion {
	t.Helper()
	c := &store.CapabilityVersion{
		NamespaceID:  env.nsID,
		Name:         name,
		Version:      1,
		ParamsSchema: json.RawMessage(schema),
		Source:       store.CapSourceBuiltin,
	}
	if err := env.db.CreateCapability(context.Background(), withSpec(c)); err != nil {
		t.Fatalf("seed capability %s: %v", name, err)
	}
	return c
}

// edgeLiteralTargetSchema 是这些用例用的目标契约：to 必填（string），tries 是 integer。
const edgeLiteralTargetSchema = `{"type":"object","required":["to"],
	"properties":{"to":{"type":"string"},"tries":{"type":"integer"}}}`

// TestEdgeCreateWithLiterals 带字面量的下游：创建成功，响应与列表**原样**回显。
//
// **改写说明**：造数从 `POST /v1/edges` 换成
// `PUT /v1/capabilities/{name}/downstreams`（能力边的唯一写入口）。
// 断言不变：字面量在写回执与列表里都原样可见。
func TestEdgeCreateWithLiterals(t *testing.T) {
	env := newConsoleEnv(t)
	env.seedCapability(t, "review", 1, []string{"out"})
	seedTargetWithSchema(t, env, "notify", edgeLiteralTargetSchema)

	rr := env.replaceDownstreams(t, "review", []map[string]any{{
		"toRef":    "notify",
		"priority": 0,
		"literals": json.RawMessage(`{"to":"u_123"}`),
	}})
	if rr.Code != http.StatusOK {
		t.Fatalf("create status = %d, body=%s", rr.Code, rr.Body.String())
	}
	links, _ := decode(t, rr)["links"].([]any)
	if len(links) != 1 {
		t.Fatalf("links = %v, want 1", links)
	}
	lit, _ := links[0].(map[string]any)["literals"].(map[string]any)
	if lit["to"] != "u_123" {
		t.Fatalf("创建响应里 links[0].literals = %v, want {to:u_123}", links[0])
	}

	// 列表也原样带回（控制台要显示这个徽章）
	rr = env.do(t, http.MethodGet, env.withNS("/v1/edges"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d", rr.Code)
	}
	edges, _ := decode(t, rr)["edges"].([]any)
	if len(edges) != 1 {
		t.Fatalf("edges = %d, want 1", len(edges))
	}
	listLit, _ := edges[0].(map[string]any)["literals"].(map[string]any)
	if listLit["to"] != "u_123" {
		t.Fatalf("列表里 edge.literals = %v, want {to:u_123}", edges[0])
	}
}

// TestEdgeCreateWithoutLiterals 不传 literals：默认 {}（老客户端行为不变）。
func TestEdgeCreateWithoutLiterals(t *testing.T) {
	env := newConsoleEnv(t)
	env.seedCapability(t, "review", 1, []string{"out"})
	seedTargetWithSchema(t, env, "notify", edgeLiteralTargetSchema)

	rr := env.replaceDownstreams(t, "review", []map[string]any{{"toRef": "notify"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("create status = %d, body=%s", rr.Code, rr.Body.String())
	}
	links, _ := decode(t, rr)["links"].([]any)
	if len(links) != 1 {
		t.Fatalf("links = %v, want 1", links)
	}
	lit, ok := links[0].(map[string]any)["literals"].(map[string]any)
	if !ok || len(lit) != 0 {
		t.Fatalf("缺省 literals = %v, want {}", links[0])
	}
}

// TestEdgeLiteralsRejected 非法字面量 → 400 + edge_rejected（原因透传，客户端可修）。
//
// **改写说明**：造数换成 PUT downstreams；机器码仍是 `edge_rejected`（与 POST /v1/edges
// 的入口边同源，同一份 checkEdgeLiterals），错误文案里的具体原因（哪个键不存在 /
// 期望什么类型）原样透传——断言不变。
func TestEdgeLiteralsRejected(t *testing.T) {
	cases := []struct {
		name     string
		literals string
		wantMsg  string
	}{
		{"不是对象", `[1,2]`, "must be a JSON object"},
		{"键不在目标 schema", `{"nope":"x"}`, "has no such field"},
		{"类型不符", `{"to":123}`, "want type string, got number"},
		{"integer 收 string", `{"tries":"3"}`, "want type integer, got string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newConsoleEnv(t)
			env.seedCapability(t, "review", 1, []string{"out"})
			seedTargetWithSchema(t, env, "notify", edgeLiteralTargetSchema)

			rr := env.replaceDownstreams(t, "review", []map[string]any{{
				"toRef":    "notify",
				"literals": json.RawMessage(tc.literals),
			}})
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("literals=%s status = %d, want 400; body=%s", tc.literals, rr.Code, rr.Body.String())
			}
			m := decode(t, rr)
			if m["code"] != "edge_rejected" {
				t.Fatalf("code = %v, want edge_rejected", m["code"])
			}
			msg, _ := m["error"].(string)
			if !strings.Contains(msg, tc.wantMsg) {
				t.Fatalf("error = %q, want it to contain %q（原因必须透传，否则用户不知道改哪）", msg, tc.wantMsg)
			}
			// 被拒的边不能落库
			rr = env.do(t, http.MethodGet, env.withNS("/v1/edges"), nil)
			if edges, _ := decode(t, rr)["edges"].([]any); len(edges) != 0 {
				t.Fatalf("被拒的边还是落库了: %v", edges)
			}
		})
	}
}
