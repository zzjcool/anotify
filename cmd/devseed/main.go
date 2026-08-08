// devseed 是开发/测试辅助工具：直接操作 store 播种用户 + API Key + 测试会话。
// 仅用于本地开发与集成测试，不随生产二进制分发（或保留但文档标注仅限自托管管理员）。
//
// 用法：
//
//	go run ./cmd/devseed -db ./anotify.db -username demo
//
// 输出：SEND_KEY / RECV_KEY / SESSION（可直接作为 Cookie 注入做 Web Push E2E）
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/anotify/anotify/internal/auth"
	"github.com/anotify/anotify/internal/store"
)

func main() {
	dbPath := flag.String("db", "./anotify.db", "SQLite 路径")
	username := flag.String("username", "demo", "用户名")
	admin := flag.Bool("admin", false, "设为超管角色")
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
}
