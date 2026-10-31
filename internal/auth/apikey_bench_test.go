package auth

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// benchKeyManager 为基准准备「真 store + 一把已签发的 key」。
// 用真 store 而不是桩：要量的就是「缓存命中能省掉多少 DB + argon2 开销」，
// 桩会把这块成本从分母里抹掉。
func benchKeyManager(b *testing.B, cacheTTL time.Duration) (*KeyManager, string) {
	b.Helper()
	db, err := store.Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatalf("open db: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })
	km := NewKeyManagerTuned(db, cacheTTL)
	uid := store.NewUserID()
	if err := db.CreateUser(&store.User{ID: uid, Username: "bench", CreatedAt: store.Now()}); err != nil {
		b.Fatalf("create user: %v", err)
	}
	plaintext, _, err := km.CreateKey(uid, "bench-key", []string{ScopeNotifySend})
	if err != nil {
		b.Fatalf("create key: %v", err)
	}
	return km, plaintext
}

// BenchmarkValidateKeyCacheHit 度量**命中路径**的单请求成本（#227 的核心数字）：
// 应为「一次 sha256 + 一次读锁 + 一次 scopes 复制」，不碰 DB、不跑 argon2，
// 所以 B/op 必须是几百字节量级而不是 64MB。
func BenchmarkValidateKeyCacheHit(b *testing.B) {
	km, plaintext := benchKeyManager(b, defaultKeyCacheTTL)
	// 预热：第一次是未命中（DB + argon2），不能算进命中路径。
	if _, _, err := km.ValidateKey(plaintext); err != nil {
		b.Fatalf("预热校验失败: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := km.ValidateKey(plaintext); err != nil {
			b.Fatalf("命中路径校验失败: %v", err)
		}
	}
}

// BenchmarkValidateKeyCacheMiss 度量**未命中路径**（对照）的成本：
// 每轮先 InvalidateAll 保证真的回源，价格 = DB 前缀查 + argon2(64MB) + 写 last_used_at。
// InvalidateAll 自身（加写锁 + 清表）相对 argon2 可忽略，但确实是分母的一部分。
func BenchmarkValidateKeyCacheMiss(b *testing.B) {
	km, plaintext := benchKeyManager(b, defaultKeyCacheTTL)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		km.InvalidateAll()
		if _, _, err := km.ValidateKey(plaintext); err != nil {
			b.Fatalf("回源校验失败: %v", err)
		}
	}
}

// BenchmarkHashKey 度量签发 API Key 时的 argon2id 哈希成本。
// 注意：argon2 参数为 m=64MB/t=1/p=4，单次开销较大，基准单位设为较小迭代。
func BenchmarkHashKey(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h, err := hashKey("ant_full_bench_secret")
		if err != nil {
			b.Fatalf("hashKey: %v", err)
		}
		_ = h
	}
}

// BenchmarkVerifyKey 度量登录校验时的常量时间比对成本（含重新计算 argon2id）。
func BenchmarkVerifyKey(b *testing.B) {
	plaintext := "ant_full_bench_secret"
	hash, err := hashKey(plaintext)
	if err != nil {
		b.Fatalf("hashKey: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ok, err := verifyKey(plaintext, hash)
		if err != nil || !ok {
			b.Fatalf("verifyKey: ok=%v err=%v", ok, err)
		}
	}
}

// BenchmarkScopeLabel 度量 scope 推断标签的成本（签发 Key 时调用）。
func BenchmarkScopeLabel(b *testing.B) {
	scopes := []string{ScopeNotifySend, ScopeNotifyReceive}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if scopeLabel(scopes) != "full" {
			b.Fatal("scopeLabel 应返回 full")
		}
	}
}
