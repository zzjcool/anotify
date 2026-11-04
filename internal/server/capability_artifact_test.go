package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// 本文件覆盖 GET /v1/capabilities/{id}/artifact（详情页展示脚本用的新端点）。
//
// 四条硬要求（少一条都会让「看脚本」变成不可用的功能）：
//  1. 返回**解析后**的文件正文 —— 不是 base64 的 spec 原文（那正是旧端点不好用的原因）
//  2. 没有产物 → 404（刚定义、还没填脚本是正常状态，不是 500）
//  3. 跨命名空间 → 404，且与「不存在」**逐字节相同**（否则 body 就能当 id 探测器）
//  4. 未登录 → 401（sessMW 在 handler 之前就挡住）

// artifactScript 是测试用的最小合法脚本（shebang 是产物的硬要求）。
const artifactScript = "#!/bin/sh\nset -eu\necho hello\n"

// createCapWithRun 用真 handler 建一个带产物的能力（走 POST /v1/capabilities）。
func createCapWithRun(t *testing.T, env *consoleEnv, nsID, name string, outputs []string) map[string]any {
	t.Helper()
	rr := env.do(t, http.MethodPost, withNSID("/v1/capabilities", nsID), map[string]any{
		"name":         name,
		"outputs":      outputs,
		"paramsSchema": map[string]any{"type": "object"},
		"run": map[string]any{
			"entrypoint": "run.sh",
			"files": map[string]string{
				"run.sh":   artifactScript,
				"lib/x.sh": "#!/bin/sh\nsee_you_later\n",
			},
			"timeoutSec": 42,
			"workDir":    "/srv/work",
			"env":        map[string]string{},
		},
		"requires": []string{"curl"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("建能力 %s 失败：%d %s", name, rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	capObj, _ := m["capability"].(map[string]any)
	if capObj == nil {
		t.Fatalf("响应缺 capability: %v", m)
	}
	return capObj
}

// TestCapabilityArtifactReturnsParsedFiles：①返回解析后的 files（不是 base64）。
func TestCapabilityArtifactReturnsParsedFiles(t *testing.T) {
	env := newConsoleEnv(t)
	capObj := createCapWithRun(t, env, env.nsID, "touchnow", []string{"echo"})
	capID, _ := capObj["id"].(string)

	rr := env.do(t, http.MethodGet, withNSID("/v1/capabilities/"+capID+"/artifact", env.nsID), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)

	// files 是**普通文本**：如果误把 spec 的 base64 透出来，这里会是不可读的串。
	files, _ := m["files"].(map[string]any)
	if files == nil {
		t.Fatalf("响应缺 files: %v", m)
	}
	if got, _ := files["run.sh"].(string); got != artifactScript {
		t.Fatalf("files[run.sh] = %q, want %q", got, artifactScript)
	}
	if got, _ := files["lib/x.sh"].(string); !strings.Contains(got, "see_you_later") {
		t.Fatalf("files[lib/x.sh] = %q, want 含 see_you_later（多文件必须全给）", got)
	}
	if got, _ := m["entrypoint"].(string); got != "run.sh" {
		t.Fatalf("entrypoint = %q, want run.sh", got)
	}
	if got, _ := m["timeoutSec"].(float64); got != 42 {
		t.Fatalf("timeoutSec = %v, want 42", m["timeoutSec"])
	}
	if got, _ := m["workDir"].(string); got != "/srv/work" {
		t.Fatalf("workDir = %q, want /srv/work", got)
	}
	reqArr, _ := m["requires"].([]any)
	if len(reqArr) != 1 || reqArr[0] != "curl" {
		t.Fatalf("requires = %v, want [curl]", m["requires"])
	}
	if digest, _ := m["digest"].(string); !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("digest = %q, want sha256:…", digest)
	}
	// 响应里不该出现 base64 的 spec 原文（那会让「解析后」这个承诺变假）
	if _, has := m["spec"]; has {
		t.Fatalf("响应不该带 spec 原文: %v", m)
	}
}

// TestCapabilityArtifactNoArtifact404：②没有产物 → 404（不是 500、不是空 200）。
func TestCapabilityArtifactNoArtifact404(t *testing.T) {
	env := newConsoleEnv(t)
	c := env.seedCapability(t, "no.script", 1, []string{"done"})

	rr := env.do(t, http.MethodGet, withNSID("/v1/capabilities/"+c.ID+"/artifact", env.nsID), nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404（body=%s）", rr.Code, rr.Body.String())
	}
}

// TestCapabilityArtifactCrossNamespace404Identical：③跨命名空间 404 与「不存在」逐字节相同。
func TestCapabilityArtifactCrossNamespace404Identical(t *testing.T) {
	env := newConsoleEnv(t)
	other := env.seedNamespace(t, "other", "另一个")
	// 能力 + 产物都在**另一个**命名空间里
	capObj := createCapWithRun(t, env, other.ID, "elsewhere", []string{"echo"})
	capID, _ := capObj["id"].(string)

	// 用默认命名空间去要别人家的能力
	cross := env.do(t, http.MethodGet, withNSID("/v1/capabilities/"+capID+"/artifact", env.nsID), nil)
	if cross.Code != http.StatusNotFound {
		t.Fatalf("跨命名空间 status = %d, want 404（body=%s）", cross.Code, cross.Body.String())
	}
	// 一个**根本不存在**的能力 id（形状与真 id 同构，防止「只有格式不对才 404」）
	missing := env.do(t, http.MethodGet, withNSID("/v1/capabilities/cap_does_not_exist/artifact", env.nsID), nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("不存在 status = %d, want 404（body=%s）", missing.Code, missing.Body.String())
	}
	if cross.Body.String() != missing.Body.String() {
		t.Fatalf("两种 404 body 必须逐字节相同（否则就是 id 探测器）：\n跨库=%q\n不存在=%q",
			cross.Body.String(), missing.Body.String())
	}
	// 再钉一次「没有产物」也走同一个 body：三种情形同一个回答
	noArt := env.seedCapability(t, "no.artifact", 1, nil)
	empty := env.do(t, http.MethodGet, withNSID("/v1/capabilities/"+noArt.ID+"/artifact", env.nsID), nil)
	if empty.Code != http.StatusNotFound || empty.Body.String() != missing.Body.String() {
		t.Fatalf("没有产物也应与「不存在」同一回答：status=%d body=%q want %q",
			empty.Code, empty.Body.String(), missing.Body.String())
	}
}

// TestCapabilityArtifactUnauthenticated401：④未登录 → 401。
func TestCapabilityArtifactUnauthenticated401(t *testing.T) {
	env := newConsoleEnv(t)
	c := env.seedCapability(t, "auth.only", 1, nil)

	rr := env.doNoCookie(t, http.MethodGet, withNSID("/v1/capabilities/"+c.ID+"/artifact", env.nsID))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401（body=%s）", rr.Code, rr.Body.String())
	}
}

// TestCapabilityArtifactEmptyLists：空 requires 是 [] 而不是 null（不变量 I9）。
func TestCapabilityArtifactEmptyLists(t *testing.T) {
	env := newConsoleEnv(t)
	rr := env.do(t, http.MethodPost, env.withNS("/v1/capabilities"), map[string]any{
		"name":         "no.requires",
		"outputs":      []string{},
		"paramsSchema": map[string]any{"type": "object"},
		"run": map[string]any{
			"entrypoint": "run.sh",
			"files":      map[string]string{"run.sh": artifactScript},
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("建能力失败：%d %s", rr.Code, rr.Body.String())
	}
	capObj, _ := decode(t, rr)["capability"].(map[string]any)
	capID, _ := capObj["id"].(string)

	got := env.do(t, http.MethodGet, env.withNS("/v1/capabilities/"+capID+"/artifact"), nil)
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body=%s）", got.Code, got.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(raw["requires"]) != "[]" {
		t.Fatalf("requires = %s, want []（空列表不是 null）", raw["requires"])
	}
	// requires 之外，空 outputs 与空 workDir 也必须是「有键的空值」，不是缺键
	if _, has := raw["files"]; !has {
		t.Fatalf("响应缺 files 键: %s", got.Body.String())
	}
}
