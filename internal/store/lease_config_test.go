package store

import (
	"context"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
)

func TestLeaseTTLAppliesToGrantAndRenew(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "lease-ttl", 1)
	node := v4Node(t, db, "ttl-node", 1)
	now := Now()

	if got := db.LeaseTTL(); got != protocol.LeaseTTL {
		t.Fatalf("未配置时 LeaseTTL = %v，want 协议下限 %v", got, protocol.LeaseTTL)
	}
	db.SetLeaseTTL(protocol.DefaultLeaseTTL)
	if got := db.LeaseTTL(); got != protocol.DefaultLeaseTTL {
		t.Fatalf("注入后 LeaseTTL = %v，want %v", got, protocol.DefaultLeaseTTL)
	}

	task := v4Task(t, db, nsID, c, "tr_lease_ttl", "")
	leaseID, _, attempt := v4Grant(t, db, task.ID, node.ID, "asg-ttl")
	running := v4MustPhase(t, db, nsID, task.ID, TaskRunning)
	want := now + int64(protocol.DefaultLeaseTTL/time.Second)
	if running.LeaseUntil == nil || *running.LeaseUntil < want-2 || *running.LeaseUntil > want+2 {
		t.Fatalf("grant 的 lease_until = %v，want ≈%d（now+180s）", running.LeaseUntil, want)
	}

	ttl := int64(protocol.DefaultLeaseTTL / time.Second)
	renewAt := now + ttl - 60
	renewed, err := db.RenewLeaseAuthorized(ctx, v4Auth(t, db, node.ID), task.ID, attempt, leaseID, renewAt)
	if err != nil {
		t.Fatalf("续租: %v", err)
	}
	if want := renewAt + ttl; renewed < want-2 || renewed > want+2 {
		t.Fatalf("续租到期 = %d，want ≈%d（renewAt + 180s）", renewed, want)
	}
}
