package server

// hook_signature_test.go 覆盖 source.md §3.3 / D7 的 webhook 验签契约（#335 抓到的真产品缺口）：
// 配了 `secret` 的源，投递必须带 `X-Anotify-Signature: sha256=<hex>`，值是
// `HMAC-SHA256(secret, **原始 body 字节**)` 的小写十六进制；缺失 / 格式错 / 不匹配 → `401`
// 且**不产生事件**（不落事件 / 不落去重窗 / 不建任务）。没配 secret 的源行为不变。
//
// 判别性（每条都对应一次真实的红）：
//   - 结论落在**真实 HTTP 状态码 + 表行数**上，不是只测一个纯函数；
//   - 签名口径必须对**原始字节**：同一份 JSON 只改空白就必须失效（证明实现不是
//     「解析后再序列化」，那条路径在 §3.3 里被点名为假失败来源）；
//   - 401 路径数 events / event_deliveries / tasks 三张表，证明真的没落任何行；
//   - 轮换：旧 secret 立刻 401、新 secret 200（§3.1 的轮换语义）。

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// hookSigHeader 是契约里的签名头名字（source.md §3.3）。
const hookSigHeader = "X-Anotify-Signature"

// hookSign 用**测试自己的** HMAC 实现算契约口径的签名。
//
// 故意不调产品代码：签名算法若被改坏（改用解析后的 JSON、换编码、换算法），
// 用例必须跟着红——用实现证明实现的用例永远绿。
func hookSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// hookPost 发一次**原始字节**的投递（sig 为空 = 不带签名头）。
func hookPost(t *testing.T, env *sourceResEnv, token string, raw []byte, sig string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/"+token, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set(hookSigHeader, sig)
	}
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	return rr
}

// hookRowCounts 是「有没有产生事件」的三张表快照：
// events（事件行，含剪枝留痕）、event_deliveries（去重窗）、tasks（派发产物）。
type hookRowCounts struct{ events, deliveries, tasks int }

func hookSnapshot(t *testing.T, env *sourceResEnv) hookRowCounts {
	t.Helper()
	return hookRowCounts{
		events:     env.count("events"),
		deliveries: env.count("event_deliveries"),
		tasks:      env.count("tasks"),
	}
}

// hookAssertNoRows 在失败投递后断言 0 副作用（401 不许留任何行）。
func hookAssertNoRows(t *testing.T, want, got hookRowCounts, where string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s：失败投递留下了行（events %d→%d，event_deliveries %d→%d，tasks %d→%d）",
			where, want.events, got.events, want.deliveries, got.deliveries, want.tasks, got.tasks)
	}
}

// hookCreateSecretSource 建一个配了 secret 的 webhook 源（token 就是 URL 段）。
func hookCreateSecretSource(t *testing.T, env *sourceResEnv, name, token, secret string) {
	t.Helper()
	cfg := map[string]any{"token": token}
	if secret != "" {
		cfg["secret"] = secret
	}
	env.createSource(
		map[string]any{
			"kind":    "webhook",
			"config":  cfg,
			"targets": []any{map[string]any{"capability": "notify"}},
		},
		map[string]any{"name": name},
	)
}

// TestHookSignatureRequiredWhenSecretSet 是 #335 的核心用例：配了 secret 就必须验签。
//
// 六种不合法的签名形态一律 401（且 0 行），合法签名 200 且真的派发。
func TestHookSignatureRequiredWhenSecretSet(t *testing.T) {
	env := newSourceResEnv(t)
	const (
		name   = "signed-hook"
		token  = "src_signed_TOKEN"
		secret = "signed-hook-secret"
	)
	hookCreateSecretSource(t, env, name, token, secret)

	raw := []byte(`{"type":"deploy.succeeded","id":"evt-1","payload":{"environment":"prod"}}`)
	good := hookSign(secret, raw)

	cases := []struct {
		why string
		sig string
	}{
		{"无签名头", ""},
		{"签名不匹配（换了一把 secret）", hookSign("wrong-secret", raw)},
		{"前缀不是 sha256= ", "sha512=" + strings.TrimPrefix(good, "sha256=")},
		{"没有算法前缀（裸 hex）", strings.TrimPrefix(good, "sha256=")},
		{"hex 截断（少一位）", good[:len(good)-1]},
		{"hex 大写", "sha256=" + strings.ToUpper(strings.TrimPrefix(good, "sha256="))},
		{"前缀大小写错（SHA256=）", "SHA256=" + strings.TrimPrefix(good, "sha256=")},
	}
	for _, tc := range cases {
		before := hookSnapshot(t, env)
		rr := hookPost(t, env, token, raw, tc.sig)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s：期望 401，得到 %d：%s", tc.why, rr.Code, rr.Body.String())
		}
		if reason := sourceResString(t, env.doc(rr)["reason"]); reason != reasonUnauthorized {
			t.Fatalf("%s：reason 期望 %q，得到 %q", tc.why, reasonUnauthorized, reason)
		}
		if strings.Contains(rr.Body.String(), secret) {
			t.Fatalf("%s：401 的错误体里出现了 secret 原文：%s", tc.why, rr.Body.String())
		}
		hookAssertNoRows(t, before, hookSnapshot(t, env), tc.why)
	}

	// 合法签名 → 200 + 真的产生事件与任务（不是「401 一切通过」的空壳）。
	before := hookSnapshot(t, env)
	rr := hookPost(t, env, token, raw, good)
	if rr.Code != http.StatusOK {
		t.Fatalf("合法签名期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	doc := env.doc(rr)
	if eventID := sourceResString(t, doc["eventId"]); eventID == "" {
		t.Fatalf("合法投递必须有 eventId：%v", doc)
	}
	after := hookSnapshot(t, env)
	if after.events != before.events+1 {
		t.Fatalf("合法投递应产生 1 条事件：%d → %d", before.events, after.events)
	}
	if after.deliveries != before.deliveries+1 {
		t.Fatalf("合法投递应落 1 条去重窗：%d → %d", before.deliveries, after.deliveries)
	}
	if after.tasks != before.tasks+1 {
		t.Fatalf("合法投递应建 1 个任务：%d → %d", before.tasks, after.tasks)
	}
}

// TestHookSignatureCoversRawBytes 验「签名对**原始字节**，不是解析后的 JSON」。
//
// 同一份语义、不同空白的两个 body 是两个不同的签名输入：这就是「调用方签名后
// 再序列化 → 假失败」与「我们解析后重新序列化 → 假通过」两种错误的照妖镜。
func TestHookSignatureCoversRawBytes(t *testing.T) {
	env := newSourceResEnv(t)
	const (
		token  = "src_raw_TOKEN"
		secret = "raw-bytes-secret"
	)
	hookCreateSecretSource(t, env, "raw-hook", token, secret)

	pretty := []byte("{\n  \"type\": \"ping\",\n  \"payload\": {\"k\": 1}\n}\n")
	compact := []byte(`{"type":"ping","payload":{"k":1}}`)

	// ① 对 pretty 签名、投 pretty → 200。
	if rr := hookPost(t, env, token, pretty, hookSign(secret, pretty)); rr.Code != http.StatusOK {
		t.Fatalf("对原始字节签名（带换行/缩进）期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}

	// ② 对 compact 签名、投 pretty → 401（等价 JSON 不等于等价字节）。
	before := hookSnapshot(t, env)
	rr := hookPost(t, env, token, pretty, hookSign(secret, compact))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("同一份 JSON 的紧凑形态签名投递到 pretty 字节上必须 401，得到 %d：%s", rr.Code, rr.Body.String())
	}
	hookAssertNoRows(t, before, hookSnapshot(t, env), "签名输入与投递字节不同")

	// ③ 只差一个尾随换行也必须失效（证明实现没有 trim / 归一化 body）。
	before = hookSnapshot(t, env)
	rr = hookPost(t, env, token, pretty, hookSign(secret, append(append([]byte{}, pretty...), '\n')))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("body 末尾多一个换行必须 401，得到 %d：%s", rr.Code, rr.Body.String())
	}
	hookAssertNoRows(t, before, hookSnapshot(t, env), "签名输入多一个尾随换行")

	// ④ compact 对 compact 签名 → 200（同一口径的另一侧）。
	if rr := hookPost(t, env, token, compact, hookSign(secret, compact)); rr.Code != http.StatusOK {
		t.Fatalf("compact 对 compact 签名期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}

	// ⑤ 空 body 也是合法投递（§3.3：有些系统只发 ping），但对它签名必须签**空字节**。
	before = hookSnapshot(t, env)
	rr = hookPost(t, env, token, nil, hookSign(secret, []byte("{}")))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("空 body 上用非空字节的签名必须 401，得到 %d：%s", rr.Code, rr.Body.String())
	}
	hookAssertNoRows(t, before, hookSnapshot(t, env), "空 body 的签名口径")

	if rr := hookPost(t, env, token, nil, hookSign(secret, nil)); rr.Code != http.StatusOK {
		t.Fatalf("空 body + 空字节目签名期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
}

// TestHookSignatureRotation 验轮换语义（§3.1：换 secret 旧签名立刻失效）。
func TestHookSignatureRotation(t *testing.T) {
	env := newSourceResEnv(t)
	const (
		name  = "rotate-hook"
		token = "src_rotate_TOKEN"
		oldSk = "old-secret"
		newSk = "new-secret"
	)
	hookCreateSecretSource(t, env, name, token, oldSk)

	raw := []byte(`{"type":"ping","payload":{"n":1}}`)
	if rr := hookPost(t, env, token, raw, hookSign(oldSk, raw)); rr.Code != http.StatusOK {
		t.Fatalf("轮换前旧 secret 期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}

	// 只改 secret（PATCH 显式写新值）。
	rv := env.resourceVersion(name)
	rr := env.request(http.MethodPatch, env.sourcePath(name), map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   map[string]any{"name": name, "resourceVersion": rv},
		"spec": map[string]any{
			"config": map[string]any{"secret": newSk},
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("轮换 PATCH 期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	if cfg := sourceResObj(t, sourceResObj(t, env.doc(rr)["spec"])["config"]); cfg["secretSet"] != true {
		t.Fatalf("轮换后 secretSet 必须仍为 true：%v", cfg)
	}

	// 旧签名立刻 401（且 0 行）。
	before := hookSnapshot(t, env)
	if rr := hookPost(t, env, token, raw, hookSign(oldSk, raw)); rr.Code != http.StatusUnauthorized {
		t.Fatalf("轮换后旧 secret 期望 401，得到 %d：%s", rr.Code, rr.Body.String())
	}
	hookAssertNoRows(t, before, hookSnapshot(t, env), "轮换后的旧 secret")

	// 新签名 200。
	if rr := hookPost(t, env, token, raw, hookSign(newSk, raw)); rr.Code != http.StatusOK {
		t.Fatalf("轮换后新 secret 期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
}

// TestHookSignatureNotRequiredWithoutSecret 是无 secret 源的回归：行为必须与从前一致
// （不带签名 / 带一个乱写的签名头都 200，签名只在配了 secret 时才是判据）。
func TestHookSignatureNotRequiredWithoutSecret(t *testing.T) {
	env := newSourceResEnv(t)
	const token = "src_open_TOKEN"
	hookCreateSecretSource(t, env, "open-hook", token, "")

	raw := []byte(`{"type":"ping","payload":{"n":1}}`)
	for _, sig := range []string{"", "sha256=deadbeef", "完全不是签名"} {
		if rr := hookPost(t, env, token, raw, sig); rr.Code != http.StatusOK {
			t.Fatalf("无 secret 源（签名头 %q）期望 200，得到 %d：%s", sig, rr.Code, rr.Body.String())
		}
	}
	// 读投影仍要诚实：没配就是 secretSet=false。
	cfg := sourceResObj(t, sourceResObj(t, env.doc(env.request(http.MethodGet, env.sourcePath("open-hook"), nil))["spec"])["config"])
	if cfg["secretSet"] != false {
		t.Fatalf("无 secret 源的 secretSet 必须 false：%v", cfg)
	}
}

// TestSourceResourceSecretExplicitClear 验「显式写空串 = 清空」的写路径语义。
//
// §3.1 只禁止把只读投影 `secretSet: false` 当作清空；PATCH 显式写已定义的
// `config.secret` 才是意图来源（与「省略 = 保留」互为两侧）。
func TestSourceResourceSecretExplicitClear(t *testing.T) {
	env := newSourceResEnv(t)
	const (
		name   = "clear-hook"
		token  = "src_clear_TOKEN"
		secret = "clear-me-secret"
	)
	hookCreateSecretSource(t, env, name, token, secret)
	raw := []byte(`{"type":"ping"}`)

	if rr := hookPost(t, env, token, raw, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("清空前无签名必须 401，得到 %d：%s", rr.Code, rr.Body.String())
	}

	rv := env.resourceVersion(name)
	rr := env.request(http.MethodPatch, env.sourcePath(name), map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   map[string]any{"name": name, "resourceVersion": rv},
		"spec":       map[string]any{"config": map[string]any{"secret": ""}},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("显式清空 secret 期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	if cfg := sourceResObj(t, sourceResObj(t, env.doc(rr)["spec"])["config"]); cfg["secretSet"] != false {
		t.Fatalf("显式清空后 secretSet 必须 false：%v", cfg)
	}

	// 清空后回到「不校验」语义：不带签名也 200（并且真的落行）。
	before := hookSnapshot(t, env)
	if rr := hookPost(t, env, token, raw, ""); rr.Code != http.StatusOK {
		t.Fatalf("清空 secret 后无签名期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	if after := hookSnapshot(t, env); after.events != before.events+1 {
		t.Fatalf("清空 secret 后的投递必须产生事件：%d → %d", before.events, after.events)
	}

	// 清空是显式意图，不是「省略」：再 PATCH 一个无关字段，secret 不会自己回来。
	rv = env.resourceVersion(name)
	rr = env.request(http.MethodPatch, env.sourcePath(name), map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   map[string]any{"name": name, "resourceVersion": rv},
		"spec":       map[string]any{"config": map[string]any{"sample": map[string]any{"k": "v"}}},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("清空后改 sample 期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	if cfg := sourceResObj(t, sourceResObj(t, env.doc(rr)["spec"])["config"]); cfg["secretSet"] != false {
		t.Fatalf("清空后的 secret 不该被后续 PATCH 复活：%v", cfg)
	}
}

// TestHookSignatureDoesNotLeakStoreError 是边界：签名错与「token 不存在」必须分开。
//
// 400/401/404 都在这一条端点上，别让验签把 404 变成 401（那会让「token 是否存在」
// 从响应码上消失，反过来也破坏 §5 的 404 不区分停用/不存在口径）。
func TestHookSignatureStatusPrecedence(t *testing.T) {
	env := newSourceResEnv(t)
	const token = "src_prec_TOKEN"
	hookCreateSecretSource(t, env, "prec-hook", token, "prec-secret")

	// 不存在的 token：仍然是 404（不是 401），哪怕带了乱签名。
	rr := hookPost(t, env, "src_nope_TOKEN", []byte(`{"type":"ping"}`), "sha256=00")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("不存在的 token 期望 404，得到 %d：%s", rr.Code, rr.Body.String())
	}

	// body 不是合法 JSON，但签名也不合法 → 401 先行（不给未签名方解析细节）。
	before := hookSnapshot(t, env)
	rr = hookPost(t, env, token, []byte(`not-json`), "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("非法 body + 无签名期望 401（验签先于解析），得到 %d：%s", rr.Code, rr.Body.String())
	}
	hookAssertNoRows(t, before, hookSnapshot(t, env), "非法 body + 无签名")

	// 签名合法但 body 不是 JSON → 400（回到既有解析口径，不产生事件）。
	before = hookSnapshot(t, env)
	bad := []byte(`not-json`)
	rr = hookPost(t, env, token, bad, hookSign("prec-secret", bad))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("合法签名 + 非法 body 期望 400，得到 %d：%s", rr.Code, rr.Body.String())
	}
	hookAssertNoRows(t, before, hookSnapshot(t, env), "合法签名 + 非法 body")

	// 超过 4 MiB → 仍然 413（回归：验签改造不能把上限吃掉）。
	big := make([]byte, srcHookBodyLimit+1)
	for i := range big {
		big[i] = 'a'
	}
	if rr := hookPost(t, env, token, big, hookSign("prec-secret", big)); rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超过 4 MiB 期望 413，得到 %d：%s", rr.Code, rr.Body.String())
	}
}
