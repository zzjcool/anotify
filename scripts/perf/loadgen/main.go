// loadgen 是一个只用标准库的 HTTP 压测工具。
//
// 为什么不用 hey / wrk / vegeta：本机装不上（网络取不到依赖），而性能评估必须
// 能**被别人一模一样复现**。所以自己写一个零依赖的，`go run` 就能跑，
// 只依赖 Go 本身。
//
// 用法：
//
//	go run ./scripts/perf/loadgen -url http://127.0.0.1:7900/health -c 32 -d 10s
//	go run ./scripts/perf/loadgen -url .../v1/hooks/<token> -method POST \
//	    -body-file /tmp/evt.json -header "content-type: application/json" -c 16 -d 30s
//
// 报告：总请求、RPS、状态码分布、延迟分位（p50/p90/p95/p99/max）、
// 以及**失败明细按错误类别归并**（超时/连接拒绝/非 2xx/非 5xx）。
//
// 刻意不做的事：不做连接池调优、不复用 Cookie 做复杂会话流程。
// 它量的是「服务端在这条路径上的极限」，不是「浏览器端体验」——
// 后者用 scripts/perf/README.md 里的 playwright 方式量。
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// multiFlag 让 -header 可以重复传：-header "a: b" -header "c: d"
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

type result struct {
	status  int
	latency time.Duration
	err     error
}

func main() {
	url := flag.String("url", "", "目标 URL（必填）")
	method := flag.String("method", "GET", "HTTP 方法")
	conc := flag.Int("c", 16, "并发数")
	dur := flag.Duration("d", 10*time.Second, "持续时间")
	bodyFile := flag.String("body-file", "", "请求体文件（POST 用）")
	timeout := flag.Duration("timeout", 10*time.Second, "单请求超时")
	noKeepAlive := flag.Bool("no-keepalive", false, "禁用连接复用（模拟每次新建连接）")
	var headerFlags multiFlag
	flag.Var(&headerFlags, "header", `请求头，可重复："Cookie: anotify_session=..."`)
	flag.Parse()

	if *url == "" {
		fmt.Fprintln(os.Stderr, "缺少 -url")
		os.Exit(2)
	}
	var body []byte
	if *bodyFile != "" {
		b, err := os.ReadFile(*bodyFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "读 body 失败:", err)
			os.Exit(2)
		}
		body = b
	}
	headers := map[string]string{}
	for _, kv := range headerFlags {
		if i := strings.Index(kv, ":"); i > 0 {
			headers[strings.TrimSpace(kv[:i])] = strings.TrimSpace(kv[i+1:])
		}
	}

	tr := &http.Transport{
		MaxIdleConns:        *conc * 2,
		MaxIdleConnsPerHost: *conc * 2,
		IdleConnTimeout:     30 * time.Second,
		DisableKeepAlives:   *noKeepAlive,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	client := &http.Client{Transport: tr, Timeout: *timeout}

	ctx, cancel := context.WithTimeout(context.Background(), *dur)
	defer cancel()

	results := make(chan result, 4096)
	var wg sync.WaitGroup

	start := time.Now()
	for i := 0; i < *conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				var rdr io.Reader
				if body != nil {
					rdr = bytes.NewReader(body)
				}
				req, err := http.NewRequestWithContext(ctx, *method, *url, rdr)
				if err != nil {
					results <- result{err: err}
					return
				}
				for k, v := range headers {
					req.Header.Set(k, v)
				}
				t0 := time.Now()
				resp, err := client.Do(req)
				lat := time.Since(t0)
				if err != nil {
					// ctx 到期属于正常收尾，不算失败
					if ctx.Err() != nil {
						return
					}
					results <- result{err: err, latency: lat}
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				results <- result{status: resp.StatusCode, latency: lat}
			}
		}()
	}

	go func() { wg.Wait(); close(results) }()

	var (
		n         int
		latencies []time.Duration
		byStatus  = map[int]int{}
		byErr     = map[string]int{}
	)
	for r := range results {
		n++
		latencies = append(latencies, r.latency)
		if r.err != nil {
			byErr[errClass(r.err)]++
			continue
		}
		byStatus[r.status]++
	}
	elapsed := time.Since(start)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	pct := func(p float64) time.Duration {
		if len(latencies) == 0 {
			return 0
		}
		idx := int(float64(len(latencies)-1) * p)
		return latencies[idx]
	}

	fmt.Printf("目标      %s %s\n", *method, *url)
	fmt.Printf("并发/时长  %d / %s\n", *conc, elapsed.Round(time.Millisecond))
	fmt.Printf("总请求    %d\n", n)
	fmt.Printf("RPS       %.1f\n", float64(n)/elapsed.Seconds())
	if len(byStatus) > 0 {
		var parts []string
		for code, c := range byStatus {
			parts = append(parts, fmt.Sprintf("%d×%d", code, c))
		}
		sort.Strings(parts)
		fmt.Printf("状态码    %s\n", strings.Join(parts, " "))
	}
	if len(byErr) > 0 {
		var parts []string
		for k, c := range byErr {
			parts = append(parts, fmt.Sprintf("%s×%d", k, c))
		}
		sort.Strings(parts)
		fmt.Printf("失败      %s\n", strings.Join(parts, " "))
	}
	fmt.Printf("延迟      p50=%s p90=%s p95=%s p99=%s max=%s\n",
		pct(0.50).Round(time.Microsecond), pct(0.90).Round(time.Microsecond),
		pct(0.95).Round(time.Microsecond), pct(0.99).Round(time.Microsecond),
		pct(1.0).Round(time.Microsecond))
}

// errClass 把错误归并成人类能读的类别：
// 「每个请求一条 timeout 消息」没有信息量，需要知道的是哪一类占了多少。
func errClass(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "context deadline exceeded"), strings.Contains(s, "Client.Timeout"):
		return "timeout"
	case strings.Contains(s, "connection refused"):
		return "conn_refused"
	case strings.Contains(s, "connection reset"):
		return "conn_reset"
	case strings.Contains(s, "no such host"):
		return "dns"
	case strings.Contains(s, "EOF"):
		return "eof"
	default:
		if len(s) > 60 {
			s = s[:60]
		}
		return "other: " + s
	}
}
