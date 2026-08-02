package auth

import (
	"encoding/base64"
	"fmt"
)

// encodeCredID 把 credential 的原始字节 ID 编码为 base64url 字符串（作表主键）。
func encodeCredID(id []byte) string {
	return base64.RawURLEncoding.EncodeToString(id)
}

// decodeCredID 把 base64url 字符串还原为原始字节。
func decodeCredID(s string) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode credential id: %w", err)
	}
	return b, nil
}
