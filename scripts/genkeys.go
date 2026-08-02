// Anotify VAPID 密钥生成工具（替代原型 genkeys.js）。
// 用法：go run ./scripts/genkeys.go
// 输出：打印 ANOTIFY_VAPID_PUBLIC / ANOTIFY_VAPID_PRIVATE 环境变量
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
)

func main() {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	pub := elliptic.Marshal(elliptic.P256(), priv.PublicKey.X, priv.PublicKey.Y)
	// 私钥取 32 字节标量
	d := priv.D.Bytes()
	privBytes := make([]byte, 32)
	copy(privBytes[32-len(d):], d)

	fmt.Println("# 将以下两行加入环境（或 .env）")
	fmt.Printf("ANOTIFY_VAPID_PUBLIC=%s\n", base64.RawURLEncoding.EncodeToString(pub))
	fmt.Printf("ANOTIFY_VAPID_PRIVATE=%s\n", base64.RawURLEncoding.EncodeToString(privBytes))
}
