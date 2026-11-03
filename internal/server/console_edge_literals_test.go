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
	if err := env.db.CreateCapability(context.Background(), c); err != nil {
		t.Fatalf("seed capability %s: %v", name, err)
	}
	return c
}

// edgeLiteralTargetSchema 是这些用例用的目标契约：to 必填（string），tries 是 integer。
const edgeLiteralTargetSchema = `{"type":"object","required":["to"],
	"properties":{"to":{"type":"string"},"tries":{"type":"integer"}}}`

// TestEdgeCreateWithLiterals 带字面量的边：创建成功，响应与列表**原样**回显。
func TestEdgeCreateWithLiterals(t *testing.T) {
	env := newConsoleEnv(t)
	from := env.seedCapability(t, "review", 1, []string{"fix"})
	to := seedTargetWithSchema(t, env, "notify", edgeLiteralTargetSchema)

	rr := env.do(t, http.MethodPost, "/v1/edges", edgeRequest{
		NamespaceID:    env.nsID,
		FromCapability: from.ID,
		FromPort:       "fix",
		ToCapability:   to.ID,
		Literals:       json.RawMessage(`{"to":"u_123"}`),
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("create status = %d, body=%s", rr.Code, rr.Body.String())
	}
	edgeObj, _ := decode(t, rr)["edge"].(map[string]any)
	lit, _ := edgeObj["literals"].(map[string]any)
	if lit["to"] != "u_123" {
		t.Fatalf("创建响应里 edge.literals = %v, want {to:u_123}", edgeObj["literals"])
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
	from := env.seedCapability(t, "review", 1, []string{"fix"})
	to := seedTargetWithSchema(t, env, "notify", edgeLiteralTargetSchema)

	rr := env.do(t, http.MethodPost, "/v1/edges", edgeRequest{
		NamespaceID:    env.nsID,
		FromCapability: from.ID,
		FromPort:       "fix",
		ToCapability:   to.ID,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("create status = %d, body=%s", rr.Code, rr.Body.String())
	}
	edgeObj, _ := decode(t, rr)["edge"].(map[string]any)
	lit, ok := edgeObj["literals"].(map[string]any)
	if !ok || len(lit) != 0 {
		t.Fatalf("缺省 literals = %v, want {}", edgeObj["literals"])
	}
}

// TestEdgeLiteralsRejected 三类非法字面量 → 400 + edge_rejected（原因透传，客户端可修）。
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
			from := env.seedCapability(t, "review", 1, []string{"fix"})
			to := seedTargetWithSchema(t, env, "notify", edgeLiteralTargetSchema)

			rr := env.do(t, http.MethodPost, "/v1/edges", edgeRequest{
				NamespaceID:    env.nsID,
				FromCapability: from.ID,
				FromPort:       "fix",
				ToCapability:   to.ID,
				Literals:       json.RawMessage(tc.literals),
			})
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
