package server

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// gzipTestJSON 造一份 >gzipMinBytes 的 JSON：形状照 list 类响应（重复键值多），
// 这样"压完必须明显更小"是硬结论而不是巧合。
func gzipTestJSON(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"items":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":"%04d","name":"node-%04d","status":"online"}`, i, i)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// gzipTestHandler 造一个和真实 handler 写形状一致的 endpoint：
// 先设头、WriteHeader、再把 body 分两段写（逼中间件走"累积"路径，
// 而不是单次大 Write 恰好越阈值的巧合）。
func gzipTestHandler(tc gzipTestCase) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tc.contentType != "" {
			w.Header().Set("Content-Type", tc.contentType)
		}
		for _, h := range tc.headers {
			w.Header().Set(h[0], h[1])
		}
		w.WriteHeader(tc.status)
		if len(tc.body) == 0 {
			return
		}
		half := len(tc.body) / 2
		_, _ = w.Write(tc.body[:half])
		_, _ = w.Write(tc.body[half:])
	})
}

type gzipTestCase struct {
	name        string
	method      string
	path        string
	acceptEnc   string
	status      int
	contentType string
	// headers 是 handler 自己额外设的头（每项 [name, value]）。
	headers [][2]string
	body    []byte

	wantStatus int
	wantGzip   bool
	// wantVary 表示响应头必须含 Vary: Accept-Encoding。
	wantVary bool
	// wantVaryExtra 是 Vary 里必须同时保留的其它 token（验证"追加不是覆盖"）。
	wantVaryExtra string
	// wantContentEncoding 非空时断言响应头的 Content-Encoding 恰好是它。
	wantContentEncoding string
	// wantNoContentLength：压缩后长度未知，必须没有 Content-Length。
	wantNoContentLength bool
}

func TestGzipJSON(t *testing.T) {
	bigBody := gzipTestJSON(200)
	if len(bigBody) < gzipMinBytes {
		t.Fatalf("测试载荷 %d 字节，不足以越过 gzipMinBytes=%d", len(bigBody), gzipMinBytes)
	}
	smallBody := []byte(`{"ok":true}`) // 远低于阈值

	cases := []gzipTestCase{
		{
			// 主用例：能压就压，且解压后与 handler 写的字节**逐字节等价**。
			name: "GET_v1_大JSON_含gzip_压缩", method: http.MethodGet, path: "/v1/events",
			acceptEnc: "gzip", status: http.StatusOK,
			contentType: "application/json; charset=utf-8",
			// handler 自己声明了 Content-Length（有些实现会），压缩后必须删掉。
			headers: [][2]string{{"Content-Length", strconv.Itoa(len(bigBody))}},
			body:    bigBody,

			wantStatus: http.StatusOK, wantGzip: true, wantVary: true,
			wantContentEncoding: "gzip", wantNoContentLength: true,
		},
		{
			// 客户端没说要 gzip：一个字节都不许改，但缓存分桶的 Vary 仍要在。
			name: "GET_v1_大JSON_无AcceptEncoding_明文", method: http.MethodGet, path: "/v1/events",
			status: http.StatusOK, contentType: "application/json",
			body: bigBody,

			wantStatus: http.StatusOK, wantGzip: false, wantVary: true,
		},
		{
			// gzip;q=0 是明确的"不接受"：协商结果优先，胜过我们省的字节。
			name: "GET_v1_gzip_q0_不压", method: http.MethodGet, path: "/v1/events",
			acceptEnc: "gzip;q=0, identity", status: http.StatusOK,
			contentType: "application/json", body: bigBody,

			wantStatus: http.StatusOK, wantGzip: false, wantVary: true,
		},
		{
			// 大小写不敏感 + q>0 仍接受。
			name: "GET_v1_GZIP_大写含q1_压缩", method: http.MethodGet, path: "/v1/events",
			acceptEnc: "GZIP;q=1.0", status: http.StatusOK,
			contentType: "application/json", body: bigBody,

			wantStatus: http.StatusOK, wantGzip: true, wantVary: true,
			wantContentEncoding: "gzip", wantNoContentLength: true,
		},
		{
			// 防双重压缩：handler 已经编码过，我们既不能重编也不能改它。
			name: "GET_v1_已有ContentEncoding_原样直通", method: http.MethodGet, path: "/v1/events",
			acceptEnc: "gzip", status: http.StatusOK,
			contentType: "application/json",
			headers:     [][2]string{{"Content-Encoding", "br"}},
			body:        bigBody,

			wantStatus: http.StatusOK, wantGzip: false, wantVary: true,
			wantContentEncoding: "br",
		},
		{
			// 小块不值得压：gzip 头 + 预热表的成本比省下的字节还大。
			name: "GET_v1_小JSON_不压", method: http.MethodGet, path: "/v1/vapid-public-key",
			acceptEnc: "gzip", status: http.StatusOK,
			contentType: "application/json", body: smallBody,

			wantStatus: http.StatusOK, wantGzip: false, wantVary: true,
		},
		{
			// 非 /v1/ 完全不在范围内：连 Vary 都不该出现（/health 由探针高频调用）。
			name: "GET_health_不压也不带Vary", method: http.MethodGet, path: "/health",
			acceptEnc: "gzip", status: http.StatusOK,
			contentType: "application/json", body: bigBody,

			wantStatus: http.StatusOK, wantGzip: false, wantVary: false,
		},
		{
			// 只压 GET：POST 的响应体（含事件回执）保持原样，行为与没这层一致。
			name: "POST_v1_不压", method: http.MethodPost, path: "/v1/events",
			acceptEnc: "gzip", status: http.StatusOK,
			contentType: "application/json", body: bigBody,

			wantStatus: http.StatusOK, wantGzip: false, wantVary: false,
		},
		{
			// 204 没有 body：不许凭空写 Content-Encoding / Vary / Content-Length。
			name: "GET_v1_204_无body", method: http.MethodGet, path: "/v1/devices/abc",
			acceptEnc: "gzip", status: http.StatusNoContent,

			wantStatus: http.StatusNoContent, wantGzip: false, wantVary: false,
		},
		{
			// text/* 可压（控制台可能回纯文本导出的 JSON 片段）。
			name: "GET_v1_text_plain_压缩", method: http.MethodGet, path: "/v1/export",
			acceptEnc: "gzip", status: http.StatusOK,
			contentType: "text/plain; charset=utf-8", body: bigBody,

			wantStatus: http.StatusOK, wantGzip: true, wantVary: true,
			wantContentEncoding: "gzip", wantNoContentLength: true,
		},
		{
			// 可压类型之外的（二进制/图片）别压：压了通常更大。
			name: "GET_v1_不可压类型_不压也不带Vary", method: http.MethodGet, path: "/v1/blob",
			acceptEnc: "gzip", status: http.StatusOK,
			contentType: "application/octet-stream", body: bigBody,

			wantStatus: http.StatusOK, wantGzip: false, wantVary: false,
		},
		{
			// handler 已经声明过 Vary：必须追加而不是覆盖（否则缓存分桶会丢维度）。
			name: "GET_v1_已有Vary_追加不覆盖", method: http.MethodGet, path: "/v1/events",
			acceptEnc: "gzip", status: http.StatusOK,
			contentType: "application/json",
			headers:     [][2]string{{"Vary", "Origin"}},
			body:        bigBody,

			wantStatus: http.StatusOK, wantGzip: true, wantVary: true,
			wantVaryExtra: "Origin", wantContentEncoding: "gzip", wantNoContentLength: true,
		},
		{
			// HEAD 不压（没有 body），但**必须**带 Vary：缓存会用 HEAD 的响应头
			// 去更新已存的 GET 响应（RFC 9111 §4.3.5），缺了就把分桶维度丢了。
			name: "HEAD_v1_不压但带Vary", method: http.MethodHead, path: "/v1/events",
			acceptEnc: "gzip", status: http.StatusOK,
			contentType: "application/json", body: bigBody,

			wantStatus: http.StatusOK, wantGzip: false, wantVary: true,
		},
		{
			// 304 无 body，但它验证的那份 200 是带 Vary 的：304 不带就会让缓存
			// 下次自己决定拿哪个变体。
			name: "GET_v1_304_无body但带Vary", method: http.MethodGet, path: "/v1/events",
			acceptEnc: "gzip", status: http.StatusNotModified,
			contentType: "application/json",

			wantStatus: http.StatusNotModified, wantGzip: false, wantVary: true,
		},
		{
			// `*` **不**当 gzip：我们是“省带宽”的一方，不能押注客户端真能解。
			// 这条钉的是有意选择，不是漏写。
			name: "GET_v1_AcceptEncoding星号_不压", method: http.MethodGet, path: "/v1/events",
			acceptEnc: "*", status: http.StatusOK,
			contentType: "application/json", body: bigBody,

			wantStatus: http.StatusOK, wantGzip: false, wantVary: true,
		},
		{
			// 声明了 200 + JSON 却一个字节没写：是“空响应”，仍要带 Vary。
			name: "GET_v1_空body_200_带Vary不压", method: http.MethodGet, path: "/v1/events",
			acceptEnc: "gzip", status: http.StatusOK,
			contentType: "application/json",

			wantStatus: http.StatusOK, wantGzip: false, wantVary: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := gzipJSON(gzipTestHandler(tc))
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.acceptEnc != "" {
				req.Header.Set("Accept-Encoding", tc.acceptEnc)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			res := rec.Result()
			defer res.Body.Close()

			got, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatalf("读响应体: %v", err)
			}

			if res.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", res.StatusCode, tc.wantStatus)
			}

			// Content-Encoding：不压时必须是空（或 handler 自己设的那个）。
			if gotCE := res.Header.Get("Content-Encoding"); gotCE != tc.wantContentEncoding {
				t.Errorf("Content-Encoding = %q, want %q", gotCE, tc.wantContentEncoding)
			}

			// Vary：可压类型必带 Accept-Encoding；不在范围内的路径/方法必须完全没有。
			if tc.wantVary {
				if !headerHasToken(res.Header, "Vary", "Accept-Encoding") {
					t.Errorf("Vary = %q, want 含 Accept-Encoding", res.Header.Values("Vary"))
				}
			} else if headerHasToken(res.Header, "Vary", "Accept-Encoding") {
				t.Errorf("Vary = %q, 不该含 Accept-Encoding", res.Header.Values("Vary"))
			}
			if tc.wantVaryExtra != "" && !headerHasToken(res.Header, "Vary", tc.wantVaryExtra) {
				t.Errorf("Vary = %q, 丢了原有的 %s", res.Header.Values("Vary"), tc.wantVaryExtra)
			}

			if tc.wantNoContentLength {
				// 压缩后长度未知：留着 Content-Length 会让客户端按原文长度截断压缩流。
				if cl := res.Header.Get("Content-Length"); cl != "" {
					t.Errorf("Content-Length = %q, 压缩响应必须没有它", cl)
				}
			}

			// body 双侧硬断言：压过的必须解出原文，没压的必须逐字节原样。
			if tc.wantGzip {
				if !strings.EqualFold(res.Header.Get("Content-Encoding"), "gzip") {
					t.Fatalf("期望 gzip，实际 Content-Encoding=%q", res.Header.Get("Content-Encoding"))
				}
				zr, err := gzip.NewReader(bytes.NewReader(got))
				if err != nil {
					t.Fatalf("响应体不是合法 gzip 流: %v", err)
				}
				defer zr.Close()
				plain, err := io.ReadAll(zr)
				if err != nil {
					t.Fatalf("解压失败: %v", err)
				}
				if !bytes.Equal(plain, tc.body) {
					t.Fatalf("解压后与原文不等（got %d 字节 / want %d 字节）", len(plain), len(tc.body))
				}
				if len(got) >= len(tc.body) {
					t.Errorf("压完 %d 字节 >= 原文 %d 字节，说明这份载荷根本不该走压缩路径", len(got), len(tc.body))
				}
				return
			}
			if !bytes.Equal(got, tc.body) {
				t.Fatalf("body = %q, want %q", got, tc.body)
			}
		})
	}
}

// TestGzipJSONFlush 覆盖流式语义：Flush 之后已经写的字节必须真的出去了，
// 不能被"攒到阈值再定局"卡在缓冲区里。
func TestGzipJSONFlush(t *testing.T) {
	first := []byte("第一段")
	second := []byte("第二段")

	h := gzipJSON(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(first)
		// 攒着的时候 Flush：必须把缓冲定局并刷给客户端（SSE/进度流靠这个）。
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		} else {
			t.Errorf("wrapper 丢了 Flush")
		}
		_, _ = w.Write(second)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/console/events", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !rec.Flushed {
		t.Error("Flushed = false, Flush 没有透传到底层")
	}
	if got, want := rec.Body.Bytes(), append(append([]byte{}, first...), second...); !bytes.Equal(got, want) {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if ce := rec.Result().Header.Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, 攒不够阈值的流不该被压", ce)
	}
}

// TestGzipJSONHijackPassthrough 是 WS（/v1/node/stream）的生命线：
// hijack 之后连接归 handler 独占，客户端拿到的必须是**未经压缩的原始字节**。
//
// 走的是生产同款链路 gzipJSON(accessLog(h))：hijack 要逐层透传
// （handler → statusWriter → gzipResponseWriter → 裸 conn），少一层就会断。
func TestGzipJSONHijackPassthrough(t *testing.T) {
	payload := []byte("RAW-HIJACKED-BYTES: 这不是 gzip，客户端必须原样收到\n")

	h := gzipJSON(accessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			// 在服务端 goroutine 里，不能用 Fatalf（会杀测试主 goroutine 之外的路径）。
			t.Errorf("wrapper 丢了 Hijack：WS 升级会直接失败")
			return
		}
		conn, brw, err := hj.Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = brw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: " +
			strconv.Itoa(len(payload)) + "\r\n\r\n")
		_, _ = brw.Write(payload)
		_ = brw.Flush()
	})))
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/node/stream", nil)
	if err != nil {
		t.Fatalf("造请求: %v", err)
	}
	// 显式要 gzip：中间件"想压"的意愿最强，此时仍必须透明。
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("读响应体: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("hijack 后 body = %q, want %q（被中间件改写/压缩了）", body, payload)
	}
	if ce := res.Header.Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, hijack 之后不许加编码", ce)
	}
}

// TestGzipJSONEndToEnd 走真 server + 真 client：浏览器的 fetch 与 Go 的
// 自动解压都必须对 gzip 无感（契约 §1 门禁：e2e 会真读响应体）。
func TestGzipJSONEndToEnd(t *testing.T) {
	want := gzipTestJSON(200)

	srv := httptest.NewServer(gzipJSON(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(want)
	})))
	defer srv.Close()

	// 不显式设 Accept-Encoding：由 transport 自己加并自动解压，等价于浏览器行为。
	res, err := srv.Client().Get(srv.URL + "/v1/events")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("读响应体: %v", err)
	}
	if !bytes.Equal(body, want) {
		t.Fatalf("客户端拿到的 body 与原文不等（got %d 字节 / want %d 字节）", len(body), len(want))
	}
	if !res.Uncompressed {
		t.Error("res.Uncompressed = false，说明响应其实没被 gzip 压过")
	}
}

// headerHasToken 判断多值头里是否含某个 token（Vary 那种逗号分隔语义）。
func headerHasToken(h http.Header, key, token string) bool {
	for _, v := range h.Values(key) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// TestGzipJSONKeepsStatusWriter 钉死中间件顺序：gzipJSON 必须在 accessLog **外侧**。
//
// ratelimit.go 的 writeRateLimited 用一条类型断言把限流维度递给最外层日志：
//
//	if sw, ok := w.(*statusWriter); ok { sw.retryAfter = secs; sw.rlScope = scope }
//
// 一旦 gzip 被套到 accessLog 内侧，handler 收到的 w 就是 *gzipResponseWriter 而不是
// *statusWriter，这条断言**静默**失败 → issue #245 费力修好的「429 日志带
// retry_after / rate_limit」当场回归，且不会有任何既有测试变红。
// 所以这里在真链路（gzipJSON(accessLog(h))，与 mux.go 的 return 同序）上把它钉死。
func TestGzipJSONKeepsStatusWriter(t *testing.T) {
	old := slog.Default()
	rec := &recordingHandler{} // 复用 hook_ratelimit_test.go 的 slog 夹具
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(old) })

	const retryAfterSecs = 7
	h := gzipJSON(accessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(*statusWriter); !ok {
			// 这就是 writeRateLimited 那条断言失败时的现场。
			t.Errorf("handler 收到的 w 是 %T，不是 *statusWriter：限流维度会静默丢失", w)
		}
		// 直接走生产限流器的写出口，连带验证它塞进 statusWriter 的两个字段。
		writeRateLimited(w, "key", retryAfterSecs*time.Second)
	})))

	req := httptest.NewRequest(http.MethodGet, "/v1/cli-auth/sessions/by-code", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rr.Code)
	}
	// 小响应（远低于 gzipMinBytes）：编码决策与中间件顺序无关，也不许被压。
	if ce := rr.Result().Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("Content-Encoding = %q, 小响应不该被压", ce)
	}

	var scope string
	var loggedRetryAfter, seen int
	for _, r := range rec.snapshot() {
		if r.msg != "request" {
			continue
		}
		if status, ok := logAttrInt(r.attrs, "status"); ok && status == http.StatusTooManyRequests {
			seen++
			loggedRetryAfter, _ = logAttrInt(r.attrs, "retry_after")
			if s, ok := r.attrs["rate_limit"].(string); ok {
				scope = s
			}
		}
	}
	if seen != 1 {
		t.Fatalf("429 日志 %d 条，want 恰好 1 条", seen)
	}
	if scope != "key" {
		t.Fatalf("429 日志的 rate_limit = %q, want \"key\"（writeRateLimited 的类型断言静默失败了）", scope)
	}
	if loggedRetryAfter != retryAfterSecs {
		t.Fatalf("429 日志的 retry_after = %d, want %d", loggedRetryAfter, retryAfterSecs)
	}
}
