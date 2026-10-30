package server

import (
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
