package auth

import (
	"testing"
)

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
