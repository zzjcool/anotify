package store

import (
	"testing"
)

// TestPasskey_BackupEligibleRoundtrip 验证 BackupEligible flag 的存取往返一致性。
// 这个测试存在的理由：go-webauthn 登录时校验"注册存的 BackupEligible"与"登录返回的"必须一致；
// 若 store 没持久化该字段（读回恒为 false），则真实同步型 Passkey（BackupEligible=true）
// 登录必报 "Backup Eligible flag inconsistency"。此测试用纯 store 逻辑（不依赖认证器）
// 100% 覆盖该不变量，弥补"虚拟认证器默认 false 掩盖 bug"的测试盲区。
func TestPasskey_BackupEligibleRoundtrip(t *testing.T) {
	for _, want := range []bool{false, true} {
		db := newTestDB(t)
		u := &User{ID: NewUserID(), Username: "be-" + map[bool]string{false: "f", true: "t"}[want], CreatedAt: Now()}
		if err := db.CreateUser(u); err != nil {
			t.Fatalf("create user: %v", err)
		}
		p := &Passkey{
			ID: "cred-be-" + map[bool]string{false: "f", true: "t"}[want],
			UserID: u.ID, PublicKey: []byte{1}, Name: "测", Transports: []string{"internal"},
			BackupEligible: want, CreatedAt: Now(),
		}
		if err := db.CreatePasskey(p); err != nil {
			t.Fatalf("create passkey: %v", err)
		}
		// GetPasskeyByID 往返
		got, err := db.GetPasskeyByID(p.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.BackupEligible != want {
			t.Errorf("GetPasskeyByID BackupEligible 往返不一致: got %v want %v", got.BackupEligible, want)
		}
		// ListPasskeysByUser 往返
		list, err := db.ListPasskeysByUser(u.ID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 1 || list[0].BackupEligible != want {
			t.Errorf("ListPasskeysByUser BackupEligible 往返不一致: got %+v want %v", list, want)
		}
	}
}
