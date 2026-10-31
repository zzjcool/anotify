package server

import (
	"bufio"
	"compress/gzip"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/logging"
)

// statusWriter 包装 http.ResponseWriter 捕获 status code。
type statusWriter struct {
	http.ResponseWriter
	status int

	// retryAfter / rlScope 是限流器写 429 时塞进来的回边信息（见 writeRateLimited）。
	//
	// 为什么不直接改 http.ResponseWriter 签名：限流器在 handler 深处，
	// 把日志需要的上下文沿调用链手递太脏；statusWriter 本来就在最外层，
	// 用类型断言“有就写”是最小侵入。默认零值 = “不是限流器产生的 429”。
	retryAfter int
	rlScope    string
}

// WriteHeader 记状态码并透传。
//
// 只认**第一次** WriteHeader：net/http 的语义就是「第一次赢」（后续会记一行
// superfluous 并忽略），而旧实现无论第几次都覆盖 status，导致访问日志把
// 「201 之后又写了个 500」的响应记成 500（响应体其实是 201）。
func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write 也触发 status（确保 200 默认值正确）。
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.ResponseWriter.Write(b)
}

// Hijack 实现 http.Hijacker，委托给底层 ResponseWriter。
// WebSocket 升级需要 Hijack；若不透传，accessLog 中间件会破坏 WS 连接。
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("ResponseWriter does not implement Hijacker")
	}
	return h.Hijack()
}

// Flush 实现 http.Flusher，委托给底层 ResponseWriter（SSE/WS 需要）。
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能穿到真正的连接（Go 1.20+ 的透传约定）。
//
// 为什么需要它：本仓的中间件链是 `gzipJSON(accessLog(mux))`，handler 拿到的永远是
// *statusWriter；ResponseController 遇到不认它的 writer 就报 ErrNotSupported，
// SetWriteDeadline / EnableFullDuplex 这类能力就全没了。补上 Unwrap 后这些能力
// 能逐层穿透（statusWriter → gzipResponseWriter → 底层）。
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// accessLog 是请求级日志中间件，包在最外层覆盖所有路由。
// 记录 method/path(剥离 query)/status/latency_ms/ip/request_id/user_id。
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := logging.NewRequestID()
		r = r.WithContext(logging.WithRequestID(r.Context(), rid))

		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 0}

		next.ServeHTTP(sw, r)

		if sw.status == 0 {
			sw.status = 200
		}

		latency := time.Since(start).Microseconds()
		// r.URL.Path 天然不含 query（AC-3: secret/registrationToken 走 query 传输，不会泄露）
		path := r.URL.Path

		args := []any{
			"request_id", rid,
			"method", r.Method,
			"path", path,
			"status", sw.status,
			"latency_ms", latency,
			"ip", clientIP(r),
		}
		if uid := auth.UserIDFromContext(r.Context()); uid != "" {
			args = append(args, "user_id", uid)
		}

		// 健康检查降级为 DEBUG（避免探针刷屏）
		if path == "/health" {
			slog.Debug("request", args...)
			return
		}

		switch {
		case sw.status >= 500:
			slog.Error("request", args...)
		case sw.status == 429 || sw.status == 401 || sw.status == 403:
			// 429 的日志**只在这里写一条**（限流器内不再自己写）：
			// 双写会在打满时把日志盘刷爆（issue #245：c=16 时 75MB/10s）。
			// retry_after / rate_limit 让运维仍然能分清是哪个维度拒的。
			if sw.status == 429 && (sw.retryAfter > 0 || sw.rlScope != "") {
				args = append(args, "retry_after", sw.retryAfter, "rate_limit", sw.rlScope)
			}
			slog.Warn("request", args...)
		default:
			slog.Info("request", args...)
		}
	})
}

// ---------- gzip 响应编码（#243 / 契约 §3.3） ----------

const (
	// gzipMinBytes 是启动压缩的累积响应体阈值。
	//
	// 低于它的块压完常常更大（gzip 头 + 动态哈夫曼表预热 ~20B），
	// 省下的字节还不够付 flate 的 CPU —— 而 #243 的瓶颈恰恰是 CPU 秒/千请求。
	gzipMinBytes = 1024

	// gzipLevel 取 BestSpeed。
	//
	// /v1/ 的响应是 JSON，BestSpeed 与 DefaultCompression 的压缩比只差几个
	// 百分点，CPU 却差一个量级；想换压缩比只改这一行。
	gzipLevel = gzip.BestSpeed
)

// gzipWriterPool 复用 gzip.Writer。
//
// 每个 writer 自带 ~32KB 的滑动窗口与哈希表，每条响应新建一次，
// QPS 上千时 GC 压力比压缩本身还大（#243 要降的是 CPU 秒/千请求）。
var gzipWriterPool = sync.Pool{
	New: func() any {
		zw, err := gzip.NewWriterLevel(io.Discard, gzipLevel)
		if err != nil {
			// 级别是编译期常量，构造失败只可能是代码写错了。
			panic("server: gzip.NewWriterLevel: " + err.Error())
		}
		return zw
	},
}

// gzipJSON 给 /v1/ 的 JSON GET 响应加 gzip（#243，契约 §3.3）。
//
// 只作用于 GET /v1/*：静态资源（/）靠内容指纹 + ETag 做条件请求，
// pprof（/debug/）是运维排障用的一次性大文本，两者都不该被这里改写编码 ——
// 这是刻意画的边界，不是漏了。非 GET / 非 /v1/ 时**完全不包装**：不包装就没有
// 任何改写，行为与没有该中间件时逐字节一致（包括 Hijack 路径）。
//
// 客户端不接受 gzip 时仍要包一层：可压类型上必须带 Vary: Accept-Encoding
// （契约 §3.3 头语义），否则共享缓存会把某个变体发给不匹配的请求。
// 这种情形下不缓冲、不压缩，只补个头就透传（见 gzipResponseWriter.canCompress）。
func gzipJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead || !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		// HEAD 也要包一层：它没有 body，但它的**头**会被缓存拿去更新已存的 GET 响应
		// （RFC 9111 §4.3.5），缺 Vary: Accept-Encoding 会丢掉分桶维度。
		// 所以 HEAD 只做“补 Vary + 透传”，永远不压（canCompress=false）。
		canCompress := r.Method == http.MethodGet && acceptsGzip(r.Header.Get("Accept-Encoding"))
		gw := &gzipResponseWriter{
			ResponseWriter: w,
			canCompress:    canCompress,
			// 小响应（绝大多数 /v1/ 控制面响应）在 512B 内就结束：给个在手容量
			// 免掉 append 反复扩张；**不**预分配到 gzipMinBytes —— 大 body 走的是
			// Write 里的“先定局再直写”分支（免掉一次 w.buf 整块拷贝），用不上 w.buf。
			buf: make([]byte, 0, 512),
		}
		// 收尾必须挂 defer：handler 写一半就 return（或 panic 到 server 兜底）时
		// 也要把 gzip 尾（CRC32 + 原始长度）刷出去，否则客户端拿到截断的流。
		defer gw.finish()
		next.ServeHTTP(gw, r)
	})
}

// gzipResponseWriter 延迟定局：先把体攒着，越过阈值（或请求结束）再决定压不压。
//
// 为什么要延迟：Content-Encoding / Content-Length 一旦随头发出去就改不了了，
// 而"值不值得压"只能看到 body 才知道。
type gzipResponseWriter struct {
	http.ResponseWriter

	// canCompress 是请求侧结论（客户端接受 gzip）；false 时只补 Vary，不缓冲。
	canCompress bool
	// status 是 handler 声明的状态码；0 表示还没写过（net/http 会补 200）。
	status int
	// wroteHeader 表示最终状态头已经透传下去了。
	wroteHeader bool
	// decided 表示"压 or 不压"已经定了，且不会再变。
	decided bool
	// hijacked 表示连接已被 handler 接管：此后一律透明转发，不压不缓冲。
	hijacked bool
	// buf 是阈值以下的暂存体。
	buf []byte
	// gz 非 nil 表示正在压缩。
	gz *gzip.Writer
}

// WriteHeader 只记状态码，不立刻透传（理由见 gzipResponseWriter 的类型注释）。
func (w *gzipResponseWriter) WriteHeader(code int) {
	if w.hijacked {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if code < http.StatusOK {
		// 1xx 是中途信号（101 就是 WS 升级）：它不结束响应，缓冲它会把升级握手
		// 卡在一个永远不会到来的 body 上。直通，不动 w.status —— 最终状态还没来。
		w.ResponseWriter.WriteHeader(code)
		if code == http.StatusSwitchingProtocols {
			// 101 之后不再有自己的响应体：剩余一律透明转发。
			w.decided, w.wroteHeader = true, true
		}
		return
	}
	if w.wroteHeader {
		// 与 net/http 一致：重复写头交给底层处理（它会忽略并记一行 superfluous 日志）。
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if w.status == 0 {
		w.status = code
	}
	if !bodyAllowed(code) {
		// 204/304 没有 body：没有可压的字节，攒着只多一层延迟，立刻定局透传。
		// 但仍要补 Vary：304 是“你缓存里的那份仍然有效”，它在语义上必须带与 200
		// 相同的分桶维度，否则缓存下次就可能拿错变体。
		if isCompressibleContentType(w.Header().Get("Content-Type")) {
			appendVaryAcceptEncoding(w.Header())
		}
		w.decided, w.wroteHeader = true, true
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if !w.canCompress {
		// 客户端不接受 gzip：没有"要不要压"的悬念，立即定局（只补 Vary）。
		// 不把响应多攒一层——不压的时候缓冲纯是白付延迟。
		w.commit(false)
	}
}

// Write 在阈值以下先攒着，越阈值就定局。
func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if !w.canCompress && !w.hijacked && !w.decided {
		w.commit(false)
	}
	if w.hijacked || w.decided {
		if w.gz != nil {
			return w.gz.Write(b)
		}
		return w.ResponseWriter.Write(b)
	}
	if len(w.buf)+len(b) >= gzipMinBytes {
		// 越过阈值：先定局（把阈值以下攒下的 ≤1KB 冲出去），再**直接把这一批字节**
		// 写到 gzip/底层。绝不把大块 body 先 append 进 w.buf —— json.Encoder 对
		// 209KB 的 /v1/console/events 响应就是一次 Write，先攒后写等于每请求白付
		// 一次 209KB 分配 + 拷贝（#243 要降的正是这个）。
		w.commit(w.compressible())
		if w.gz != nil {
			return w.gz.Write(b)
		}
		return w.ResponseWriter.Write(b)
	}
	w.buf = append(w.buf, b...)
	// 攒进缓冲区就算收下了：定局时如果写底层失败，handler 已经拿不到这个错误了
	// （缓冲型 writer 的通用代价，和 bufio.Writer 一致）。
	return len(b), nil
}

// Flush 实现 http.Flusher。
//
// 语义要求"已经写的必须真的出去"：先把缓冲定局（不够阈值就明文发，绝不为了
// 压缩比把流卡在缓冲区里），再冲 gzip 的压缩缓冲，最后冲底层连接。
func (w *gzipResponseWriter) Flush() {
	if !w.hijacked && !w.decided {
		if w.status == 0 {
			w.status = http.StatusOK
		}
		w.commit(false)
	}
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack 实现 http.Hijacker：WS 升级（GET /v1/node/stream）靠它拿裸连接。
//
// hijack 之后这条连接归 handler 独占，net/http 不会再替我们写任何东西，
// 所以这里必须立刻退化成透明转发；缓冲里的字节（升级前正常不会有）已无处可写。
// 也正因如此，压缩路径绝不能先于 hijack 头部触发 —— 否则 handler 拿到的是
// 一条被我们写过头和 gzip 流的连接。
func (w *gzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("ResponseWriter does not implement http.Hijacker")
	}
	w.hijacked = true
	w.decided = true
	w.wroteHeader = true
	w.buf = nil
	return hj.Hijack()
}

// Unwrap 让 http.ResponseController 能穿过这层找到底层的 Hijack/Flush/
// SetWriteDeadline（Go 1.20+ 的透传约定，websocket.Accept 与 pprof 都可能用到）。
func (w *gzipResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// finish 收尾：把还没定局的响应冲出去，并归还 gzip writer。
func (w *gzipResponseWriter) finish() {
	if !w.decided {
		if w.status == 0 && len(w.buf) == 0 {
			// handler 一个字节都没写：交给 net/http 自己发它认为合适的空响应
			// （或干脆不发）。凭空写头会把"没响应"变成"空响应"。
			return
		}
		// 没攒够阈值：不压，但攒下的字节照样要发出去。
		// 注意 status != 0 时也要进这一步：handler 已经声明了 200 + Content-Type
		// 却没写 body 时，它是“一个空的 JSON 响应”，应该拿上 Vary（同一条 URL
		// 的变体仍随 Accept-Encoding 变化），而不是被当成“什么都没发生”。
		w.commit(false)
	}
	if w.gz != nil {
		// 只有 Close 才会写出 gzip 尾（CRC32 + 原始长度），漏了这一步
		// 客户端会拿到 truncated 流（解压报 unexpected EOF）。
		_ = w.gz.Close()
		// 放回池前丢掉对连接的引用，否则这条连接的 bufio.Reader 会被池里的
		// writer 一直挂着（内存泄漏 + 误写）。
		w.gz.Reset(io.Discard)
		gzipWriterPool.Put(w.gz)
		w.gz = nil
	}
}

// commit 定局：写头，必要时连攒着的 body 一起冲出去。
func (w *gzipResponseWriter) commit(compress bool) {
	if w.decided {
		return
	}
	w.decided = true
	w.wroteHeader = true

	h := w.Header()
	if isCompressibleContentType(h.Get("Content-Type")) {
		// 不论压不压，同一条 URL 的字节数都随 Accept-Encoding 变，必须让缓存分桶。
		appendVaryAcceptEncoding(h)
	}
	if compress {
		// 旧 Content-Length 是**原文**长度，必须删：留着它客户端会按原文字长去截
		// 压缩流，解压必然失败。删掉之后由 net/http 重新决定——压缩体塞得进 2KiB
		// bufio 时它会算出**压缩后**的正确 CL，更大的响应则走 chunked。
		h.Del("Content-Length")
		h.Set("Content-Encoding", "gzip")
	}
	// ETag / Last-Modified / Range 一概不碰：本中间件只覆盖 /v1/ 的动态响应，
	// 条件请求与断点续传语义留给静态资源那条路（刻意边界）。
	w.ResponseWriter.WriteHeader(w.status)

	if !compress {
		if len(w.buf) > 0 {
			_, _ = w.ResponseWriter.Write(w.buf)
			w.buf = nil
		}
		return
	}
	// 压缩定局后 gzip writer **必须**立刻建：否则头里已经写了 Content-Encoding: gzip，
	// 后续字节却会明文落连接（客户端 “invalid header”）。
	w.gz = gzipWriterPool.Get().(*gzip.Writer)
	w.gz.Reset(w.ResponseWriter)
	if len(w.buf) > 0 {
		_, _ = w.gz.Write(w.buf)
		w.buf = nil
	}
}

// compressible 判断当前响应头允不允许压（契约 §3.3 第 4/5 条）。
//
// 第 5 条（不二次压缩）必须在这里：handler 自己做过编码（br / gzip / …）时
// 我们既不知道原长度也不能重编，再包一层只会让客户端解出乱码。
func (w *gzipResponseWriter) compressible() bool {
	h := w.Header()
	if h.Get("Content-Encoding") != "" {
		return false
	}
	return isCompressibleContentType(h.Get("Content-Type"))
}

// acceptsGzip 解析 Accept-Encoding，判断客户端是否接受 gzip（契约 §3.3 第 3 条）。
//
// 只认显式的 gzip token：把 `*` 当 gzip 要付"客户端其实解不开"的代价，
// 而我们省的只是带宽，不值这个风险；`gzip;q=0` 是明确的"不接受"。
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		if qValueIsZero(params) {
			continue
		}
		return true
	}
	return false
}

// qValueIsZero 报告参数串里是否出现 q=0（含 0.0 / 0.000 / 大小写 Q）。
func qValueIsZero(params string) bool {
	for _, p := range strings.Split(params, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "q") {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return err == nil && f <= 0
	}
	return false
}

// isCompressibleContentType 判断这份 Content-Type 值不值得压（契约 §3.3 第 4 条）。
func isCompressibleContentType(ct string) bool {
	// 去掉 "; charset=utf-8" 之类的参数再比对。
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.ToLower(strings.TrimSpace(ct))
	switch ct {
	case "application/json", "application/javascript", "image/svg+xml":
		return true
	}
	return strings.HasPrefix(ct, "text/")
}

// bodyAllowed 报告该状态码是否可能有响应体（契约 §3.3 第 6 条）。
func bodyAllowed(code int) bool {
	if code >= 100 && code < 200 {
		return false
	}
	return code != http.StatusNoContent && code != http.StatusNotModified
}

// appendVaryAcceptEncoding 追加（而非覆盖）Vary: Accept-Encoding。
//
// 为什么必须带：同一条 URL 的响应字节数随 Accept-Encoding 变化，
// 中间缓存不分桶就会把 gzip 字节喂给不支持的客户端。
func appendVaryAcceptEncoding(h http.Header) {
	for _, v := range h.Values("Vary") {
		for _, token := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "accept-encoding") {
				// 已经有了：别叠第二遍（多值 Vary 语义等价，但会让日志/断言难读）。
				return
			}
		}
	}
	h.Add("Vary", "Accept-Encoding")
}
