package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/resultspool"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是**真实装配**的接线路由门禁（fresh review R2 的第一条未覆盖项）。
//
// 为什么必须有：本卡所有 handler 测试都自己 new 一个 ServeMux，`mux.go` / `resource_task.go`
// 里那两行生产注册即使漏挂、被更具体的 pattern 遮蔽、或忘了包会话中间件，全部用例
// 依然绿——症状要到线上才出现（`/v1/node/...` 落到静态兜底 = SPA 404，下载变成
// 「未登录也 404」）。所以这里直接打 `NewApp().Handler`：
//   - 未登录下载 → **401**（而不是 404）：同时证明「路由已注册」+「会话中间件在包它」；
//   - 无凭证上传 → **401 + {"error":"unauthorized"}**（而不是 404）；
//   - 带真会话的未知 ns/未知 task → 404（证明 handler 真的被执行到了）。

// artifactWiringSHA 是一个形状合法的 64 位小写 hex（内容哈希，不是 spec digest）。
var artifactWiringSHA = strings.Repeat("a", 64)

func TestArtifactRoutesAreWiredOnProductionMux(t *testing.T) {
	app := newProductionAppForTest(t)
	if app.Spool == nil {
		t.Fatal("production app has no artifact spool")
	}
	downloadPath := "/v1/namespaces/default/tasks/tsk_wiring/artifacts/" + artifactWiringSHA
	uploadPath := protocol.NodeTaskArtifactsPathPrefix + "tsk_wiring" + protocol.NodeTaskArtifactsPathInfix + artifactWiringSHA

	t.Run("download route is registered and session-wrapped", func(t *testing.T) {
		rr := appDo(t, app, http.MethodGet, downloadPath, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated download status = %d, want 401（404 说明路由没挂上，落到了静态兜底；body=%s）",
				rr.Code, rr.Body.String())
		}
		if cc := rr.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
			t.Fatalf("Cache-Control = %q, want no-store（noStore 包装层）", cc)
		}
	})

	t.Run("upload route is registered without session middleware", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, uploadPath, strings.NewReader("x"))
		rr := httptest.NewRecorder()
		app.Handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("credential-less upload status = %d, want 401（404 = 路由没挂上；405 = 只挂了别的 method；body=%s）",
				rr.Code, rr.Body.String())
		}
		var body map[string]string
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || len(body) != 1 || body["error"] != "unauthorized" {
			t.Fatalf("upload 401 body = %s (err=%v), want exactly {\"error\":\"unauthorized\"}", rr.Body.String(), err)
		}
	})

	t.Run("authenticated download reaches the handler", func(t *testing.T) {
		// 真用户 + 真会话：走完整的 Cookie → SessionManager → authorizePathNamespace 链路。
		user := &store.User{ID: store.NewUserID(), Username: "artifact-wiring", CreatedAt: store.Now()}
		ns, err := app.DB.RegisterUser(t.Context(), user)
		if err != nil {
			t.Fatalf("register user: %v", err)
		}
		sess, err := app.AuthSvc.Sessions().Create(user.ID, "artifact wiring")
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		cookie := &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID}

		// ① ns 存在、task 不存在、hash 合法 → 404（不是 401/500）。这条同时证明
		//    handler 跑通了 authorizePathNamespace + GetTaskArtifactDownload 的前半段。
		rr := appDo(t, app, http.MethodGet, "/v1/namespaces/"+ns.Slug+"/tasks/tsk_missing/artifacts/"+artifactWiringSHA, cookie)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("unknown task download status = %d, want 404（body=%s）", rr.Code, rr.Body.String())
		}
		// ② 未登记的 ns → 404（ns 解析在 handler 内，不是中间件里）。
		rr = appDo(t, app, http.MethodGet, "/v1/namespaces/no-such-ns/tasks/tsk_missing/artifacts/"+artifactWiringSHA, cookie)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("unknown namespace download status = %d, want 404（body=%s）", rr.Code, rr.Body.String())
		}
	})

}

// TestArtifactUploadInternalErrorShape：非 A1 / 非授权类的真故障必须是
// 500 + {"error":"internal"}，并且**补偿仍然生效**（ticket 撤回、不留字节、不落 manifest）。
// fresh review R2 指出这条 error 分支覆盖率 0；它也是「补偿不能把确定的失败变成另一种失败」的底线。
//
// 制造手法：托管池的临时目录被移走 → `CreateDraft` 失败。此时节点身份与授权预检**已经通过**，
// 所以错误既不是 401/403 也不是 A1 四码。不用「关库」：那条路会在第一步节点查询就 401，
// 测不到 500 分支。
func TestArtifactUploadInternalErrorShape(t *testing.T) {
	env := newArtifactEnv(t)
	mux := http.NewServeMux()
	mux.Handle("PUT "+protocol.NodeTaskArtifactsPathPrefix+"{taskId}"+protocol.NodeTaskArtifactsPathInfix+"{sha256}",
		newArtifactUploadHandler(env.db, env.spool, env.deps.logger()))

	if err := os.RemoveAll(filepath.Join(env.spool.Root(), "tmp")); err != nil {
		t.Fatalf("remove spool tmp: %v", err)
	}
	content := []byte("no-tmp-dir")
	rr := env.do(mux, http.MethodPut, env.uploadURL(artifactSHA(content)), bytes.NewReader(content), env.uploadHeaders("reports/x.md"))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500（body=%s）", rr.Code, rr.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || len(body) != 1 || body["error"] != artifactErrInternal {
		t.Fatalf("500 body = %s (err=%v), want exactly {\"error\":%q}", rr.Body.String(), err, artifactErrInternal)
	}
	// 补偿生效：Begin 建出的 ticket 被撤回，托管目录没有对象文件，manifest 0 行。
	env.assertNoArtifactWrites(t)
}

// TestArtifactSpoolRootSymlinkIsRejectedWithoutChmod：root 配成符号链接时，
// resultspool 必须**拒绝**且**不改链接目标权限**（fresh review R1 的 minor：
// 先 Chmod 再 Lstat 会去改 root 之外的 inode）。
func TestArtifactSpoolRootSymlinkIsRejectedWithoutChmod(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real-root")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	// 目标目录先放宽到 0755：若实现先 Chmod 再 Lstat，这里会被改成 0700。
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatalf("chmod target: %v", err)
	}
	link := filepath.Join(dir, "spool-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := resultspool.New(link); err == nil {
		t.Fatal("resultspool.New accepted a symlinked root")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Fatalf("symlink target perm = %o, want 0755（拒绝之前不得改动 root 外的 inode）", perm)
	}
}
