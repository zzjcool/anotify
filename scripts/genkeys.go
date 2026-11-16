// Anotify VAPID 密钥生成工具（替代原型 genkeys.js）。
// 用法：go run ./scripts/genkeys.go
// 输出：打印 ANOTIFY_VAPID_PUBLIC_KEY / ANOTIFY_VAPID_PRIVATE_KEY 环境变量
package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
)

// VAPID 两种键的形状是 wire 契约，不是实现细节（webpush-go v1.4.0 vapid.go 的
// GenerateVAPIDKeys / generateVAPIDHeaderKeys 就是按这两种形状解析的）：
//
//   - 公钥：P-256 未压缩点 = 65 字节，首字节 0x04，其后 X(32) || Y(32)；
//   - 私钥：32 字节大端标量；
//   - 编码：base64url 无填充（RawURLEncoding）。
//
// crypto/ecdh 的 Bytes() 正好就是这两种形状，所以不再需要 elliptic.Marshal。
const (
	publicKeyLen  = 65
	privateKeyLen = 32
	uncompressed  = 0x04
)

func main() {
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	pub := priv.PublicKey().Bytes()
	privBytes := priv.Bytes()
	// 生成结果要直接喂给 webpush-go，形状不对就当场报错，而不是等到发推送时
	// 才发现 JWT/加密失败（那时的报错离这里很远）。
	if len(pub) != publicKeyLen || pub[0] != uncompressed {
		log.Fatalf("公钥不是 P-256 未压缩点：len=%d 首字节=0x%02x", len(pub), pub[0])
	}
	if len(privBytes) != privateKeyLen {
		log.Fatalf("私钥不是 32 字节标量：len=%d", len(privBytes))
	}

	fmt.Println("# 将以下两行加入环境（或 .env）")
	fmt.Printf("ANOTIFY_VAPID_PUBLIC_KEY=%s\n", base64.RawURLEncoding.EncodeToString(pub))
	fmt.Printf("ANOTIFY_VAPID_PRIVATE_KEY=%s\n", base64.RawURLEncoding.EncodeToString(privBytes))
}
