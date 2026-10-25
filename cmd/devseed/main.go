// devseed 是开发/测试辅助工具：直接操作 store 播种用户 + API Key + 测试会话。
// 仅用于本地开发与集成测试，不随生产二进制分发（或保留但文档标注仅限自托管管理员）。
//
// 用法：
//
//	go run ./cmd/devseed -db ./anotify.db -username demo
//
// 输出：SEND_KEY / RECV_KEY / SESSION（可直接作为 Cookie 注入做 Web Push E2E）
//
// 加 -demo 时额外造一份「真跑过」的示例数据（见 demo.go）：
//
//	go run ./cmd/devseed -db ./anotify.db -username demo -demo
//
// 它会建能力图、注册一个节点、用真实 WS 握手把它挂上线，然后发一批事件，
// 让节点接单、ack、回报——所以前端看到的是走完整条派发链路的数据，
// 而不是手工塞进库里的状态行。
//
// 加 -autologin-file 时额外写一个自动登录页（见 scripts/dev.sh）：
//
//	go run ./cmd/devseed -db ./anotify.db -username demo \
//	  -autologin-file ./web/__dev_login.html
//
// 页面作用是「把 session 变成 cookie 并跳首页」——WebAuthn 的 RP_ID 必须
// 与域名严格一致，换域名（trycloudflare 随机域名等）就注册不了 passkey，
// 有个免 passkey 的入口能省掉一堆手工操作。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/store"
)

func main() {
	dbPath := flag.String("db", "./anotify.db", "SQLite 路径")
	username := flag.String("username", "demo", "用户名")
	admin := flag.Bool("admin", false, "设为超管角色")
	autoLoginFile := flag.String("autologin-file", "", "写一个自动登录页到指定路径（开发用；空=不写）")
	demo := flag.Bool("demo", false, "造一份示例数据：能力图 + 节点 + 真跑过的事件链（幂等，可重复跑）")
	demoURL := flag.String("demo-url", "http://localhost:8080", "-demo 时节点要连的服务地址（走真实 WS 握手）")
	demoFollow := flag.Int("demo-follow-sec", 25, "-demo 时保持节点在线多少秒（期间事件链会真的跑完）")
	flag.Parse()

	db, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()

	// 幂等：用户名已存在则复用（并按 -admin 提权）
	var uid string
	if u, err := db.GetUserByUsername(*username); err == nil && u != nil {
		uid = u.ID
		log.Printf("用户 %s 已存在: %s", *username, uid)
		if *admin && u.Role != store.RoleAdmin {
			if _, err := db.UpdateUserRole(ctx, uid, store.RoleAdmin); err != nil {
				log.Fatal(err)
			}
			log.Printf("用户 %s 已提权为 admin", *username)
		}
	} else {
		role := store.RoleMember
		if *admin {
			role = store.RoleAdmin
		}
		u := &store.User{ID: store.NewUserID(), Username: *username, DisplayName: *username, Role: role, CreatedAt: store.Now()}
		if err := db.CreateUser(u); err != nil {
			log.Fatal(err)
		}
		uid = u.ID
	}
	// 注册即得组织（契约 A1）。seed 工具也走同一条规则，别绕开。
	org, err := db.EnsureOrgForUser(ctx, uid, *username)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("组织就绪: %s (%s)", org.Name, org.ID)

	km := auth.NewKeyManager(db)
	sendPlain, _, err := km.CreateKey(uid, "dev-send", []string{"notify:send"})
	if err != nil {
		log.Fatal(err)
	}
	recvPlain, _, err := km.CreateKey(uid, "dev-recv", []string{"notify:receive"})
	if err != nil {
		log.Fatal(err)
	}

	sm := auth.NewSessionManager(db, 0, false)
	sess, err := sm.Create(uid, "devseed · CLI")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("UID=%s\n", uid)
	fmt.Printf("SEND_KEY=%s\n", sendPlain)
	fmt.Printf("RECV_KEY=%s\n", recvPlain)
	fmt.Printf("SESSION=%s\n", sess.ID)
	if u, err := db.GetUserByID(uid); err == nil {
		fmt.Printf("ROLE=%s\n", u.Role)
	}

	if *demo {
		// 注意：必须在 sendPlain 生成之后——事件通过 HTTP + API Key 投递
		if err := runDemo(ctx, db, org.ID, *demoURL, sendPlain, *demoFollow); err != nil {
			log.Fatalf("-demo 失败: %v", err)
		}
	}

	if *autoLoginFile != "" {
		if err := writeAutoLoginPage(*autoLoginFile, sess.ID, *username); err != nil {
			log.Fatalf("写自动登录页失败: %v", err)
		}
		log.Printf("自动登录页已写入: %s", *autoLoginFile)
	}
}

// autoLoginPage 是自动登录页的模板。
//
// 设计要点：
//   - session **内嵌在页面里**，不经过 URL/日志/Referer——它是个长期凭据，
//     放到 query 上会漏进 access log 和 Referer 头。
//   - 用 location.replace 而不是 href：不在浏览器历史里留一条能回退的登录页。
//   - 页面只在开发模式存在（dev.sh 生成、.gitignore 忽略、退出时删除）。
const autoLoginPage = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>开发登录 · Anotify</title>
<style>
  :root { color-scheme: dark; }
  body { margin:0; min-height:100vh; display:grid; place-items:center;
         background:#0a0a0b; color:#e4e4e7;
         font:14px/1.6 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif; }
  .card { max-width:26rem; padding:2rem; border-radius:1rem;
          background:#131316; box-shadow:0 0 0 1px rgba(255,255,255,.06); }
  h1 { margin:0 0 .5rem; font-size:1.05rem; font-weight:600; color:#fafafa; }
  p { margin:.5rem 0; color:#a1a1aa; }
  code { color:#a5b4fc; font-family:ui-monospace,SFMono-Regular,Menlo,monospace; }
  .ok { color:#4ade80; }
  .err { color:#f87171; }
  .bar { height:3px; border-radius:2px; background:#27272a; overflow:hidden; margin-top:1rem; }
  .bar > i { display:block; height:100%; width:0; background:#6366f1; transition:width .3s; }
</style>
</head>
<body>
<div class="card">
  <h1>开发模式自动登录</h1>
  <p>用户 <code>__USERNAME__</code> · 即将跳转工作台…</p>
  <p id="status">正在写入会话…</p>
  <div class="bar"><i id="bar"></i></div>
  <noscript><p class="err">需要启用 JavaScript。</p></noscript>
</div>
<script>
(function () {
  "use strict";
  var SESSION = "__SESSION__";
  var bar = document.getElementById("bar");
  var status = document.getElementById("status");

  // 跟 dev.sh 保持同一个跳转目标（默认总览）
  var next = new URLSearchParams(location.search).get("next") || "/index.html";

  try {
    // httpOnly 拿不到，但这里只是开发页；Secure 由 https 决定，
    // http://localhost 下写 Secure cookie 浏览器会静默丢弃，所以按协议判断。
    var attrs = "; path=/; samesite=lax";
    if (location.protocol === "https:") attrs += "; secure";
    document.cookie = "anotify_session=" + encodeURIComponent(SESSION) + attrs;
    bar.style.width = "100%";
    status.textContent = "会话已写入，跳转中…";
    status.className = "ok";
    setTimeout(function () { location.replace(next); }, 250);
  } catch (e) {
    status.textContent = "写入失败：" + e.message;
    status.className = "err";
  }
})();
</script>
</body>
</html>
`

// writeAutoLoginPage 把自动登录页写到指定路径（开发辅助）。
func writeAutoLoginPage(path, session, username string) error {
	html := strings.NewReplacer(
		"__SESSION__", session,
		"__USERNAME__", username,
	).Replace(autoLoginPage)
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	// 0600：里面含会话凭据，别让同机其他用户读到
	return os.WriteFile(path, []byte(html), 0o600)
}
