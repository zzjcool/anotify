package v4test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

func namespaceWholeFacts(t *testing.T, db *store.DB) [][][]any {
	t.Helper()
	out := registryFacts(t, db)
	for _, q := range []string{`SELECT * FROM namespaces ORDER BY id`, `SELECT * FROM namespace_configs ORDER BY namespace_id`, `SELECT * FROM capability_versions ORDER BY id`, `SELECT * FROM deployments ORDER BY id`, `SELECT * FROM playbooks ORDER BY id`, `SELECT * FROM playbook_installs ORDER BY id`, `SELECT * FROM events ORDER BY id`, `SELECT * FROM event_deliveries ORDER BY namespace_id,source,delivery_id`, `SELECT * FROM edges ORDER BY namespace_id,from_is_source,from_ref,to_ref`, `SELECT * FROM sources ORDER BY id`, `SELECT * FROM nodes ORDER BY id`, `SELECT * FROM node_configs ORDER BY node_id`, `SELECT * FROM emits ORDER BY id`} {
		out = append(out, wireRows(t, db, q))
	}
	return out
}

func TestC3bNamespaceForcePhysicalOccupancyWholeFacts(t *testing.T) {
	for _, fault := range []string{"granted-unspawned", "terminal-unreaped", "offline-shrink", "busy-unknown", "reservation"} {
		t.Run(fault, func(t *testing.T) {
			db, ns, cap, node, task, auth := uploadFixture(t)
			ctx := context.Background()
			newNS(t, db, "keep")
			if _, err := db.CreateDeployments(ctx, ns, cap.ID, "", []string{node.ID}); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "terminal-unreaped":
				if err := db.RequestAbandon(ctx, task.ID, protocol.ReasonAbandonedByUser, auth.Now); err != nil {
					t.Fatal(err)
				}
				if _, _, err := db.RecoverLostLeases(ctx, auth.Now+60); err != nil {
					t.Fatal(err)
				}
			case "offline-shrink":
				if err := db.MarkNodeOffline(ctx, node.ID, auth.Now); err != nil {
					t.Fatal(err)
				}
				if _, err := db.UpsertInstances(ctx, node.ID, []store.InstanceSpec{{Slot: 1}}); err != nil {
					t.Fatal(err)
				}
			case "busy-unknown":
				if _, err := db.ReapTaskAttempt(ctx, store.ReapedInputs{TaskID: task.ID, Attempt: auth.Attempt, NodeID: node.ID, NodeTokenHash: node.TokenHash, LeaseID: auth.LeaseID, Slot: 0, Now: auth.Now}); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`UPDATE node_instances SET busy_task=?,busy_attempt=999,busy_lease_id='unknown' WHERE node_id=? AND slot=0`, task.ID, node.ID); err != nil {
					t.Fatal(err)
				}
			case "reservation":
				if _, err := db.ReapTaskAttempt(ctx, store.ReapedInputs{TaskID: task.ID, Attempt: auth.Attempt, NodeID: node.ID, NodeTokenHash: node.TokenHash, LeaseID: auth.LeaseID, Slot: 0, Now: auth.Now}); err != nil {
					t.Fatal(err)
				}
				queued := newTask(t, db, ns, cap, "reservation", "")
				if err := db.AssignTaskAt(ctx, queued.ID, node.ID, "reserved", 1, auth.Now); err != nil {
					t.Fatal(err)
				}
				observeReservation(t, db, ns, node.ID, queued.ID, 0)
			}
			before := namespaceWholeFacts(t, db)
			err := db.DeleteNamespaceByName(ctx, "uploads", true, store.ResourceVersion(ns, 1))
			var blocked *store.NamespaceBlockedError
			if !errors.As(err, &blocked) || !strings.Contains(err.Error(), "reap") {
				t.Fatalf("force must existing409 human-readable reap conflict: %v", err)
			}
			if !reflect.DeepEqual(before, namespaceWholeFacts(t, db)) {
				t.Fatal("force conflict changed any related facts")
			}
		})
	}
}

func TestC3bNamespaceForceAfterReapAndLatePendingGrant(t *testing.T) {
	db, ns, cap, node, task, auth := uploadFixture(t)
	ctx := context.Background()
	otherNS := newNS(t, db, "other")
	sharedInput := testCap(otherNS, cap.Name, cap.Version, nil)
	sharedInput.Spec = cap.Spec
	shared := createCap(t, db, sharedInput)
	if shared.Digest != cap.Digest {
		t.Fatal("shared digest fixture must be identical")
	}
	if _, err := db.CreateDeployments(ctx, ns, cap.ID, "", []string{node.ID}); err != nil {
		t.Fatal(err)
	}
	if err := db.RequestAbandon(ctx, task.ID, protocol.ReasonAbandonedByUser, auth.Now); err != nil {
		t.Fatal(err)
	}
	if err := db.AckCancelAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, store.ReapedInputs{TaskID: task.ID, Attempt: auth.Attempt, LeaseID: auth.LeaseID, Slot: 0, Now: auth.Now}, true); err != nil {
		t.Fatal(err)
	}
	pending := newTask(t, db, ns, cap, "pending", "")
	if err := db.AssignTaskAt(ctx, pending.ID, node.ID, "late", 1, auth.Now); err != nil {
		t.Fatal(err)
	}
	outsideBefore := wireRows(t, db, `SELECT * FROM capability_versions WHERE namespace_id=?`, otherNS)
	if err := db.DeleteNamespaceByName(ctx, "uploads", true, store.ResourceVersion(ns, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetNode(ctx, node.ID); err != nil {
		t.Fatalf("global Node lost: %v", err)
	}
	if !reflect.DeepEqual(outsideBefore, wireRows(t, db, `SELECT * FROM capability_versions WHERE namespace_id=?`, otherNS)) {
		t.Fatal("shared digest outside owner modified")
	}
	before := namespaceWholeFacts(t, db)
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, pending.ID, "late", 1, 0, auth.Now); err == nil {
		t.Fatal("late grant resurrected deleted task")
	}
	if !reflect.DeepEqual(before, namespaceWholeFacts(t, db)) {
		t.Fatal("late grant wrote")
	}
	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, auth.Attempt, auth.LeaseID, auth.Now, store.TaskResult{Phase: store.TaskSucceeded}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("late result: %v", err)
	}
	fresh, err := db.CreateNamespace(ctx, "uploads", "recreated")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID == ns {
		t.Fatal("UID reused")
	}
	before = namespaceWholeFacts(t, db)
	err = db.DeleteNamespaceByName(ctx, "uploads", true, store.ResourceVersion(ns, 1))
	var conflict *store.ResourceVersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("old UID CAS: %v", err)
	}
	if !reflect.DeepEqual(before, namespaceWholeFacts(t, db)) {
		t.Fatal("old UID deleted recreation")
	}
}

func TestC3bNamespaceForceExternalReferencesNoCascade(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "owned")
	other := newNS(t, db, "other")
	cap := createCap(t, db, testCap(ns, "run", 1, nil))
	// Retained externally-owned ID reference fixture: a single-ID task FK
	// cannot express namespace ownership. Force must still not destroy it.
	outside := newTask(t, db, other, cap, "outside", "")
	before := namespaceWholeFacts(t, db)
	err := db.DeleteNamespaceByName(ctx, "owned", true, store.ResourceVersion(ns, 1))
	var blocked *store.NamespaceBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("external UID reference cascaded: %v", err)
	}
	if !reflect.DeepEqual(before, namespaceWholeFacts(t, db)) {
		t.Fatal("external reference rejection wrote")
	}
	if got, err := db.GetTask(ctx, other, outside.ID); err != nil || got.CapabilityID != cap.ID {
		t.Fatalf("external pin lost: %+v %v", got, err)
	}
}

func TestC3bCapabilityPhysicalGCRealReferencesAndRecreation(t *testing.T) {
	db, ns, cap, node, task, auth := uploadFixture(t)
	ctx := context.Background()
	dep, err := db.CreateDeployments(ctx, ns, cap.ID, "", []string{node.ID})
	if err != nil {
		t.Fatal(err)
	}
	refs := registerTestArtifact(t, db, auth, "out", 1)
	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, auth.Attempt, auth.LeaseID, auth.Now, store.TaskResult{Phase: store.TaskSucceeded, Artifacts: refs}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteCapability(ctx, ns, cap.Name, store.ResourceVersion(cap.ID, cap.ResourceVersion)); err != nil {
		t.Fatal(err)
	}
	recreatedCap := testCap(ns, cap.Name, cap.Version, nil)
	recreatedCap.Spec = cap.Spec
	fresh := createCap(t, db, recreatedCap)
	if fresh.ID == cap.ID || fresh.Digest != cap.Digest {
		t.Fatal("new UID/shared digest not actual")
	}
	oldSpec := wireRows(t, db, `SELECT * FROM capability_versions WHERE id=?`, cap.ID)
	if n, err := db.PurgeDeletedCapabilities(ctx, 20); err != nil || n != 0 {
		t.Fatalf("task/attempt/manifest/deploy refs: %d %v", n, err)
	}
	if !reflect.DeepEqual(oldSpec, wireRows(t, db, `SELECT * FROM capability_versions WHERE id=?`, cap.ID)) {
		t.Fatal("GC changed pinned original spec")
	}
	if n, err := db.DeleteExpiredTasks(ctx, auth.Now+1, 20); err != nil || n != 1 {
		t.Fatalf("task TTL: %d %v", n, err)
	}
	if n, err := db.PurgeDeletedCapabilities(ctx, 20); err != nil || n != 0 {
		t.Fatalf("actual deployment alone must protect: %d %v", n, err)
	}
	// C2 deliberately filters ordinary deployment reads for deleted targets.
	// This GC test retires the exact persisted reference as a trusted control-plane
	// fault fixture, not via the live-resource HTTP/ordinary DeleteDeployment API.
	if _, err := db.Exec(`DELETE FROM deployments WHERE id=? AND capability_id=?`, dep[0].ID, cap.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := db.PurgeDeletedCapabilities(ctx, 20); err != nil || n != 1 {
		t.Fatalf("unreferenced tombstone not purged: %d %v", n, err)
	}
	live, err := db.ResolveRef(ctx, ns, cap.Name)
	if err != nil || live.ID != fresh.ID {
		t.Fatalf("recreated live owner lost: %+v %v", live, err)
	}
	if got, err := db.GetCapabilityByID(ctx, ns, fresh.ID); err != nil || got.Digest != cap.Digest {
		t.Fatalf("shared digest live code lost: %+v %v", got, err)
	}
}

func TestC3bCapabilityGCInstallReceiptAnchor(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "receipt-gc")
	p := newPBPackage("pkg").withDefault()
	if _, err := db.CreatePlaybook(ctx, ns, p.slug, p.build(), "test"); err != nil {
		t.Fatal(err)
	}
	receipt, err := db.InstallPlaybook(ctx, ns, p.slug, map[string]any{"project": "demo"}, false)
	if err != nil {
		t.Fatal(err)
	}
	cap, err := db.ResolveRef(ctx, ns, "demo-review")
	if err != nil {
		t.Fatal(err)
	}
	if cap.InstalledFrom == "" {
		t.Fatalf("actual receipt missing: %+v", receipt)
	}
	if err := db.DeleteCapability(ctx, ns, cap.Name, store.ResourceVersion(cap.ID, cap.ResourceVersion)); err != nil {
		t.Fatal(err)
	}
	if n, err := db.PurgeDeletedCapabilities(ctx, 20); err != nil || n != 0 {
		t.Fatalf("retained install receipt must protect tombstone: %d %v", n, err)
	}
	if !rowExists(t, db, "capability_versions", cap.ID) || !rowExists(t, db, "playbook_installs", cap.InstalledFrom) {
		t.Fatal("receipt/pin lost")
	}
}
