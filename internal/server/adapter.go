package server

import (
	"context"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/authn"
)

// keyValidatorAdapter 把 auth.KeyManager（ValidateKey(plaintext) 无 ctx）
// 适配为 authn.KeyValidator（ValidateKey(ctx, key)）。
type keyValidatorAdapter struct {
	km *auth.KeyManager
}

func (a keyValidatorAdapter) ValidateKey(_ context.Context, key string) (string, []string, error) {
	return a.km.ValidateKey(key)
}

// asKeyValidator 适配。
func asKeyValidator(km *auth.KeyManager) authn.KeyValidator {
	return keyValidatorAdapter{km: km}
}
