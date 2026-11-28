package store

import "errors"

// RegistrationPolicy 是自助注册闸门（#574）：库非空时还允不允许新账号自建；零值 = Open。
// 放在 store 是因为判定必须与 INSERT 同事务（见 identity.go 的 registerUser）。
type RegistrationPolicy int32

const (
	RegistrationOpen RegistrationPolicy = iota
	// RegistrationFirstUserOnly 仅当 users 为空时允许注册（首个用户成为 admin）。
	RegistrationFirstUserOnly
)

func (p RegistrationPolicy) String() string {
	switch p {
	case RegistrationOpen:
		return "open"
	case RegistrationFirstUserOnly:
		return "first-user-only"
	default:
		// 不认识的取值**不**降级成 open：这条策略的失效方向必须是「更严」。
		return "invalid"
	}
}

// ErrRegistrationClosed 是闸门拒绝注册的哨兵（errors.Is 判，翻成 403）。
var ErrRegistrationClosed = errors.New("store: registration is closed")

func (d *DB) SetRegistrationPolicy(p RegistrationPolicy) {
	d.registration.Store(int32(p))
}

func (d *DB) RegistrationPolicy() RegistrationPolicy {
	return RegistrationPolicy(d.registration.Load())
}
