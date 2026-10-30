package store

import (
	"crypto/rand"
	"encoding/binary"
	"math/big"
	"time"
)

// newID 生成一个带前缀的、时间有序的唯一 ID。
// 格式：<prefix>_<base62(8B 毫秒时间戳 + 8B 随机)>，时间有序、全局唯一足够。
//
// 为什么不返回 error：ID 生成器被 38 个包当构造函数用，签名定死为返回 string。
// 而 crypto/rand 失败意味着系统熵源不可用——此时没有任何「安全退路」可走
// （回退到 math/rand 或时间戳会静默生成可预测的、可能碰撞的 ID，比崩掉危险得多），
// 所以这里 panic 是刻意选择：让进程尽早、响亮地死掉，而不是带着坏 ID 继续跑。
func newID(prefix string) string {
	var b [16]byte
	ms := time.Now().UnixMilli()
	binary.BigEndian.PutUint64(b[0:8], uint64(ms))
	if _, err := rand.Read(b[8:]); err != nil {
		//nolint:panic // 见上方注释：熵源不可用时不提供不安全的降级路径
		panic("store: crypto/rand failed: " + err.Error())
	}
	return prefix + "_" + base62Encode(b[:])
}

const b62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// base62Encode 把字节串按大端解释成大整数后转 base62（真 base62，62 个字符都用满）。
//
// 旧实现是「每字节拆成两个 base62 字符」（c/62, c%62），首字符只会落在 b62[0..4]，
// 既不是真 base62，也浪费了 ID 的字符空间。
func base62Encode(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	n := new(big.Int).SetBytes(data)
	base := big.NewInt(62)
	mod := new(big.Int)
	var out []byte
	for n.Sign() > 0 {
		n.DivMod(n, base, mod)
		out = append(out, b62[mod.Int64()])
	}
	if len(out) == 0 {
		return "0"
	}
	// DivMod 从低位往高位产出，反转
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

// 实体 ID 生成器。前缀语义见 docs/contracts/CONTRACT.md。
func NewUserID() string         { return newID("usr") }
func NewDeviceID() string       { return newID("dev") }
func NewSessionID() string      { return newID("sess") }
func NewEventID() string        { return newID("evt") }
func NewCliAuthID() string      { return newID("cas") }
func NewRecoveryCodeID() string { return newID("rc") }

// NewMessageID 生成旧通知模型（messages 表）的 ID。
//
// Deprecated: messages 表属于被 events/tasks 取代的旧模型，
// 等 #7 把 /v1/notify、/v1/notifications、浏览器 WS 一起下线后删除。
func NewMessageID() string { return newID("ntf") }

// 重构新增（契约 §0 的实体）。
func NewOrgID() string { return newID("org") }

// NewNamespaceID 生成命名空间 ID：ns_<base62>（契约 §2.1.1）。
func NewNamespaceID() string { return newID("ns") }
func NewNodeID() string      { return newID("nd") }
func NewInstanceID() string  { return newID("nins") }
func NewSourceID() string    { return newID("src") }
func NewTaskID() string      { return newID("tsk") }

// NewPlaybookID 生成预案 ID：pb_<base62>（#193，与 mock / 设计稿的 pb_ 前缀一致）。
func NewPlaybookID() string { return newID("pb") }

// NewCapabilityID 生成能力版本 ID：cap_<UTC 秒>_<hash8>（契约 §2.2 的形态）。
//
// 形态里有时间戳和内容 hash 前 8 位——但这两者都**不足以保证唯一**：
//   - 同一秒内为不同组织创建同名同内容能力时，时间戳与 hash8 完全相同
//   - 不同内容也可能撞 hash 前 8 位（4 字节，生日碰撞在万级数量就可能出）
//
// 而 id 是 PRIMARY KEY，冲突直接报错。所以尾部再加 4 字节随机熵，
// 保持契约里「可读的时间 + hash 前缀」形态，同时真正唯一。
func NewCapabilityID(contentHash string) string {
	return "cap_" + itoa64(time.Now().Unix()) + "_" + idHashSuffix(contentHash) + "_" + randomSuffix()
}

// NewGraphVersionID 生成图快照 ID：grf_<UTC 秒>_<hash8>（同 NewCapabilityID 的唯一性考量）。
func NewGraphVersionID(contentHash string) string {
	return "grf_" + itoa64(time.Now().Unix()) + "_" + idHashSuffix(contentHash) + "_" + randomSuffix()
}

// randomSuffix 返回 6 位 base62 随机后缀（约 35 bit 熵）。
func randomSuffix() string {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 同 newID：熵源不可用时不提供不安全的降级路径
		//nolint:panic // 见 newID 注释
		panic("store: crypto/rand failed: " + err.Error())
	}
	return base62Encode(b[:])
}

// itoa64 是无 strconv 依赖的十进制转换（避免 ids.go 拉入额外包）。
func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
