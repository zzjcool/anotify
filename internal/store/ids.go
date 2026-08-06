package store

import (
	"crypto/rand"
	"encoding/binary"
	"time"
)

// newID 生成一个带前缀的、时间有序的唯一 ID（简化版 KSUID）。
// 格式：<prefix>_<base62(timestamp_ms + random)>，时间有序、全局唯一足够。
func newID(prefix string) string {
	var b [12]byte
	ms := time.Now().UnixMilli()
	binary.BigEndian.PutUint64(b[0:8], uint64(ms))
	rand.Read(b[8:12])
	return prefix + "_" + base62Encode(b[:])
}

const b62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func base62Encode(data []byte) string {
	// 简单起见用 hex-ish 的 base62 折叠（避免大数运算），保持时间有序前缀。
	// 前 8 字节是毫秒时间戳，逐字节映射保证大致有序。
	out := make([]byte, 0, len(data)*2)
	for _, c := range data {
		out = append(out, b62[c/62], b62[c%62])
	}
	return string(out)
}

// NewUserID / NewMessageID / NewDeviceID / NewSessionID / NewKeyID 生成各实体 ID。
func NewUserID() string    { return newID("usr") }
func NewMessageID() string { return newID("ntf") }
func NewDeviceID() string  { return newID("dev") }
func NewSessionID() string { return newID("sess") }
func NewEventID() string   { return newID("evt") }
func NewCliAuthID() string { return newID("cas") }
