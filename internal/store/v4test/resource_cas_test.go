package v4test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/zzjcool/anotify/internal/protocol"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

func casRace(t *testing.T, a, b func() error) {
	t.Helper()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, f := range []func() error{a, b} {
		wg.Add(1)
		go func(f func() error) { defer wg.Done(); <-start; results <- f() }(f)
	}
	close(start)
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, store.ErrResourceVersionConflict) {
			conflicts++
		} else {
			t.Errorf("unexpected race error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("same old RV: success=%d conflict=%d; want 1/1", successes, conflicts)
	}
}

func TestResourceCASSourceConcurrentProjection(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "cas-source")
	createCap(t, db, testCap(ns, "left", 1, nil))
	createCap(t, db, testCap(ns, "right", 1, nil))
	s := &store.Source{NamespaceID: ns, Name: "entry", Kind: store.SourceKindManual, Enabled: true, Config: json.RawMessage(`{}`)}
	if err := db.CreateSource(ctx, s); err != nil {
		t.Fatal(err)
	}
	a, b := *s, *s
	a.Targets = []sourcespec.Target{{Capability: "left"}}
	b.Targets = []sourcespec.Target{{Capability: "right"}}
	casRace(t, func() error { return db.UpdateSource(ctx, &a) }, func() error { return db.UpdateSource(ctx, &b) })
	got, err := db.GetSource(ctx, ns, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ResourceVersion != s.ResourceVersion+1 {
		t.Fatalf("RV=%d", got.ResourceVersion)
	}
	if len(got.Targets) != 1 || !reflect.DeepEqual(edgeRefsFrom(t, db, ns, s.Name), []string{got.Targets[0].Capability}) {
		t.Fatalf("torn source/edges: %+v", got)
	}
}

func TestResourceCASSourceMissingAndStale(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "cas-errors")
	s := &store.Source{NamespaceID: ns, Name: "entry", Kind: store.SourceKindManual, Enabled: true}
	if err := db.CreateSource(ctx, s); err != nil {
		t.Fatal(err)
	}
	missing := *s
	missing.ResourceVersion = 0
	if err := db.UpdateSource(ctx, &missing); !errors.Is(err, store.ErrResourceVersionRequired) {
		t.Fatalf("missing RV: %v", err)
	}
	first, stale := *s, *s
	if err := db.UpdateSource(ctx, &first); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSource(ctx, &stale); !errors.Is(err, store.ErrResourceVersionConflict) {
		t.Fatalf("stale RV: %v", err)
	}
}

func TestResourceCASCreateCannotMutateExisting(t *testing.T) {
	db := openTestDB(t)
	ns := newNS(t, db, "cas-create")
	first := createCap(t, db, testCap(ns, "review", 1, []string{"before"}))
	again := testCap(ns, "review", 1, []string{"after"})
	again.DisplayName = "unguarded"
	if err := db.CreateCapability(context.Background(), again); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetCapability(context.Background(), ns, "review", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.ResourceVersion != first.ResourceVersion || got.DisplayName != first.DisplayName || !reflect.DeepEqual(got.Spec.Next, first.Spec.Next) {
		t.Fatalf("POST reuse mutated existing resource without CAS: %+v", got)
	}
}

func TestResourceCASCapabilityLowerVersionCreateKeepsLatestProjection(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "create-lower-version")
	high := createCap(t, db, testCap(ns, "worker", 3, []string{"keep"}))
	before, err := db.GetCapabilityLatest(ctx, ns, high.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(edgeRefs(t, db, ns, false, high.Name), before.Spec.Next) {
		t.Fatal("latest projection fixture mismatch")
	}

	low := createCap(t, db, testCap(ns, high.Name, 1, []string{"low"}))
	storedLow, err := db.GetCapability(ctx, ns, high.Name, 1)
	if err != nil || !reflect.DeepEqual(storedLow, low) || countCapRows(t, db, ns, high.Name) != 2 {
		t.Fatalf("lower explicit version did not persist: %+v %v", storedLow, err)
	}
	latest, err := db.GetCapabilityLatest(ctx, ns, high.Name)
	if err != nil || !reflect.DeepEqual(latest, before) {
		t.Fatalf("lower creation changed latest spec/identity/revision: %+v %v", latest, err)
	}
	if got := edgeRefs(t, db, ns, false, high.Name); !reflect.DeepEqual(got, before.Spec.Next) {
		t.Fatalf("lower creation replaced latest projection: got %v, want %v", got, before.Spec.Next)
	}
}

func TestResourceCASSourceDisplayNameRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "display.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ns := newNS(t, db, "display")
	var s store.Source
	if err := json.Unmarshal([]byte(`{"name":"hook","kind":"webhook","enabled":true,"displayName":"展示","config":{"token":"src_fixed","secret":"secret","sample":{"message":"keep"}}}`), &s); err != nil {
		t.Fatal(err)
	}
	s.NamespaceID = ns
	if err := db.CreateSource(ctx, &s); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.GetSource(ctx, ns, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["displayName"] != "展示" {
		t.Fatalf("displayName lost through create/reopen: %s", raw)
	}
	if !reflect.DeepEqual(got.Config, s.Config) {
		t.Fatalf("config changed: %s", got.Config)
	}
}

func TestResourceCASNodeRootReadsConfigToken(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	n := newClusterNode(t, db, "shared")
	cfg := putNodeCfg(t, db, n.ID, nil, map[string]string{"KEEP": "value"}, nil)
	got, err := db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ResourceVersion != cfg.ResourceVersion {
		t.Fatalf("node RV=%d config RV=%d; root/config bypass", got.ResourceVersion, cfg.ResourceVersion)
	}
}

func requireConflictToken(t *testing.T, err error, uid string, rev int64) {
	t.Helper()
	var conflict *store.ResourceVersionConflictError
	if !errors.Is(err, store.ErrResourceVersionConflict) || !errors.As(err, &conflict) || conflict.CurrentResourceVersion != store.ResourceVersion(uid, rev) {
		t.Fatalf("conflict must carry current complete token: err=%v details=%+v", err, conflict)
	}
}

func TestResourceCASCapabilityDeletePinsAndRecreate(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "delete-pin")
	old := createCap(t, db, testCap(ns, "worker", 1, []string{"sink"}))
	latest := createCap(t, db, testCap(ns, "worker", 2, []string{"sink"}))
	upstream := createCap(t, db, testCap(ns, "upstream", 1, []string{"worker@1"}))
	source := newSource(t, db, ns, "entry", store.SourceKindManual, `{}`)
	targets := []sourcespec.Target{{Capability: "worker@1"}}
	if _, err := db.UpdateSourceByName(ctx, ns, source.Name, store.SourcePatch{Targets: &targets}, store.ResourceVersion(source.ID, source.ResourceVersion)); err != nil {
		t.Fatal(err)
	}
	task := newTask(t, db, ns, old, "pin-old", "")
	node := newClusterNode(t, db, "pinned")
	// S1-D7：显式部署必须先在事务里覆盖目标 ns 的授权。夹具节点默认没有申报任何
	// namespace，这里补真实的申报事实（不改任何期望 0 写/FK/digest 行为）。
	if _, err := db.Exec(`UPDATE nodes SET declared_namespaces = ? WHERE id = ?`, `["delete-pin"]`, node.ID); err != nil {
		t.Fatalf("declare node namespace: %v", err)
	}
	if _, err := db.CreateDeployments(ctx, ns, old.ID, old.Digest, []string{node.ID}); err != nil {
		t.Fatal(err)
	}
	var oldSpec string
	if err := db.QueryRow("SELECT spec FROM capability_versions WHERE id = ?", old.ID).Scan(&oldSpec); err != nil {
		t.Fatal(err)
	}
	token := store.ResourceVersion(latest.ID, latest.ResourceVersion)
	if err := db.DeleteCapability(ctx, ns, "worker", store.ResourceVersion(old.ID, old.ResourceVersion)); !errors.Is(err, store.ErrResourceVersionConflict) {
		t.Fatalf("stale latest identity delete: %v", err)
	}
	if err := db.DeleteCapability(ctx, ns, "worker", token); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"worker", "worker@1", "worker@2"} {
		if _, err := db.ResolveRef(ctx, ns, ref); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("deleted ref %s: %v", ref, err)
		}
	}
	if _, err := db.GetCapabilityByID(ctx, ns, old.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ordinary ID exposes tombstone: %v", err)
	}
	list, err := db.ListCapabilities(ctx, ns)
	if err != nil || len(list) != 1 || list[0].ID != upstream.ID {
		t.Fatalf("deleted public versions leaked: %+v %v", list, err)
	}
	if edges := edgeRefs(t, db, ns, false, "worker"); len(edges) != 0 {
		t.Fatalf("outgoing projection remains: %v", edges)
	}
	if edges := edgeRefs(t, db, ns, false, "upstream"); !reflect.DeepEqual(edges, []string{"worker@1"}) {
		t.Fatalf("incoming intent changed: %v", edges)
	}
	gotSource, err := db.GetSource(ctx, ns, source.ID)
	if err != nil || !reflect.DeepEqual(gotSource.Targets, targets) {
		t.Fatalf("incoming source changed: %+v %v", gotSource, err)
	}
	if _, err := db.CurrentArtifact(ctx, ns, old.ID); !errors.Is(err, store.ErrArtifactNotFound) {
		t.Fatalf("deleted deploy target remains: %v", err)
	}
	if _, err := db.CreateDeployments(ctx, ns, old.ID, old.Digest, []string{node.ID}); !errors.Is(err, store.ErrArtifactNotFound) {
		t.Fatalf("deleted target deployed: %v", err)
	}
	dps, err := db.ListDeployments(ctx, ns, store.DeploymentFilter{})
	if err != nil || len(dps) != 0 {
		t.Fatalf("public deleted deployment target: %+v %v", dps, err)
	}
	// Private delivery still has a real, retained deployment reference to the old UID.
	deliveries, err := db.DeploymentsForNode(ctx, ns, node.ID)
	if err != nil || len(deliveries) != 1 || deliveries[0].CapabilityID != old.ID {
		t.Fatalf("pinned deployment body lost: %+v %v", deliveries, err)
	}
	fresh := createCap(t, db, testCap(ns, "worker", 1, []string{"new-route"}))
	if fresh.ID == old.ID || fresh.Digest != old.Digest {
		t.Fatalf("recreate identity/digest: old=%+v new=%+v", old, fresh)
	}
	if err := db.DeleteCapability(ctx, ns, "worker", store.ResourceVersion(old.ID, old.ResourceVersion)); err == nil {
		t.Fatal("old UID deleted recreated row")
	} else {
		requireConflictToken(t, err, fresh.ID, fresh.ResourceVersion)
	}
	if _, err := db.UpdateCapability(ctx, ns, "worker", 1, store.CapabilityPatch{}, store.ResourceVersion(old.ID, old.ResourceVersion)); err == nil {
		t.Fatal("ABA update succeeded")
	} else {
		requireConflictToken(t, err, fresh.ID, fresh.ResourceVersion)
	}
	gotTask, err := db.GetTask(ctx, ns, task.ID)
	if err != nil || gotTask.CapabilityID != old.ID || gotTask.Digest != old.Digest || gotTask.Phase != store.TaskPending {
		t.Fatalf("delete rewrote/cancelled task: %+v %v", gotTask, err)
	}
	if _, _, err := db.EligibleNodesForTask(ctx, gotTask); err != nil {
		t.Fatalf("private pinned task read failed: %v", err)
	}
	setRetention(t, db, 1, 1, 1)
	if _, err := db.RunRetention(ctx, store.Now()+100000); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DeleteExpiredPlaybookInstalls(ctx, store.Now()+100000, 100); err != nil {
		t.Fatal(err)
	}
	var pinnedSpec string
	if err := db.QueryRow("SELECT spec FROM capability_versions WHERE id = ?", old.ID).Scan(&pinnedSpec); err != nil || pinnedSpec != oldSpec {
		t.Fatalf("pin body changed/GC loss: %s %v", pinnedSpec, err)
	}
	live, err := db.ResolveRef(ctx, ns, "worker@1")
	if err != nil || live.ID != fresh.ID {
		t.Fatalf("GC/recreate resolved wrong identity: %+v %v", live, err)
	}
	var digestOwners int
	if err := db.QueryRow("SELECT count(*) FROM capability_versions WHERE digest = ?", old.Digest).Scan(&digestOwners); err != nil || digestOwners != 2 {
		t.Fatalf("shared digest owner lost: %d %v", digestOwners, err)
	}
	if edges := edgeRefs(t, db, ns, false, "worker"); !reflect.DeepEqual(edges, []string{"new-route"}) {
		t.Fatalf("GC deleted new live edges: %v", edges)
	}
}

func TestResourceCASCapabilityConcurrentAndProjectionRollback(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "cap-cas-race")
	cap := createCap(t, db, testCap(ns, "worker", 1, nil))
	token := store.ResourceVersion(cap.ID, cap.ResourceVersion)
	left, right := []string{"left"}, []string{"right"}
	casRace(t, func() error {
		_, err := db.UpdateCapability(ctx, ns, cap.Name, 1, store.CapabilityPatch{Next: &left}, token)
		return err
	}, func() error {
		_, err := db.UpdateCapability(ctx, ns, cap.Name, 1, store.CapabilityPatch{Next: &right}, token)
		return err
	})
	got, err := db.GetCapability(ctx, ns, cap.Name, 1)
	if err != nil || got.ResourceVersion != cap.ResourceVersion+1 || got.Digest != cap.Digest || !reflect.DeepEqual(edgeRefs(t, db, ns, false, cap.Name), got.Spec.Next) {
		t.Fatalf("torn cap CAS: %+v %v", got, err)
	}
	var events int
	if err := db.QueryRow("SELECT count(*) FROM events WHERE type = ?", store.EventTypeCapabilityRoutingChanged).Scan(&events); err != nil || events != 1 {
		t.Fatalf("routing events=%d %v", events, err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_cap_edge BEFORE INSERT ON edges WHEN NEW.to_ref = 'blocked' BEGIN SELECT RAISE(ABORT, 'edge blocked'); END`); err != nil {
		t.Fatal(err)
	}
	blocked := []string{"blocked"}
	label := "must rollback"
	if _, err := db.UpdateCapability(ctx, ns, cap.Name, 1, store.CapabilityPatch{Next: &blocked, DisplayName: &label}, store.ResourceVersion(got.ID, got.ResourceVersion)); err == nil {
		t.Fatal("edge failure expected")
	}
	after, err := db.GetCapability(ctx, ns, cap.Name, 1)
	if err != nil || !reflect.DeepEqual(after, got) || !reflect.DeepEqual(edgeRefs(t, db, ns, false, cap.Name), got.Spec.Next) {
		t.Fatalf("partial spec/edge/RV: %+v %v", after, err)
	}
}

func TestResourceCASCapabilityDeleteUpdateLinearization(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "delete-race")
	cap := createCap(t, db, testCap(ns, "worker", 1, nil))
	token := store.ResourceVersion(cap.ID, cap.ResourceVersion)
	// The token loser either sees a conflict or the correctly absent live resource.
	start := make(chan struct{})
	results := make(chan error, 2)
	next := []string{"after"}
	go func() { <-start; results <- db.DeleteCapability(ctx, ns, cap.Name, token) }()
	go func() {
		<-start
		_, err := db.UpdateCapability(ctx, ns, cap.Name, 1, store.CapabilityPatch{Next: &next}, token)
		results <- err
	}()
	close(start)
	a, b := <-results, <-results
	if (a == nil) == (b == nil) {
		t.Fatalf("exactly one mutation required: %v / %v", a, b)
	}
	loser := a
	if loser == nil {
		loser = b
	}
	if !errors.Is(loser, store.ErrResourceVersionConflict) && !errors.Is(loser, store.ErrNotFound) {
		t.Fatalf("unexpected loser: %v", loser)
	}
}

func TestResourceCASSourceDisplayOnlyAndProjectionRollback(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "source-fields")
	createCap(t, db, testCap(ns, "worker", 1, nil))
	src := &store.Source{NamespaceID: ns, Name: "hook", DisplayName: "before", Kind: store.SourceKindWebhook, Enabled: true, Config: json.RawMessage(`{"token":"src_fixed","secret":"secret","sample":{"message":"keep"}}`), Targets: []sourcespec.Target{{Capability: "worker"}}}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	label := "changed"
	got, err := db.UpdateSourceByName(ctx, ns, src.Name, store.SourcePatch{DisplayName: &label}, store.ResourceVersion(src.ID, src.ResourceVersion))
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != label || got.Name != src.Name || got.Kind != src.Kind || !reflect.DeepEqual(got.Config, src.Config) || !reflect.DeepEqual(got.Targets, src.Targets) || got.Enabled != src.Enabled {
		t.Fatalf("display touched spec/credentials: %+v", got)
	}
	omitted, err := db.UpdateSourceByName(ctx, ns, src.Name, store.SourcePatch{}, store.ResourceVersion(got.ID, got.ResourceVersion))
	if err != nil || omitted.DisplayName != label {
		t.Fatalf("omission cleared display: %+v %v", omitted, err)
	}
	clear := ""
	cleared, err := db.UpdateSourceByName(ctx, ns, src.Name, store.SourcePatch{DisplayName: &clear}, store.ResourceVersion(omitted.ID, omitted.ResourceVersion))
	if err != nil || cleared.DisplayName != "" || !reflect.DeepEqual(cleared.Config, src.Config) {
		t.Fatalf("explicit clear failed: %+v %v", cleared, err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_source_edge BEFORE INSERT ON edges BEGIN SELECT RAISE(ABORT,'source edge blocked'); END`); err != nil {
		t.Fatal(err)
	}
	changedCfg := []byte(`{"secret":"must rollback"}`)
	if _, err := db.UpdateSourceByName(ctx, ns, src.Name, store.SourcePatch{Config: changedCfg, DisplayName: &label}, store.ResourceVersion(cleared.ID, cleared.ResourceVersion)); err == nil {
		t.Fatal("edge failure expected")
	}
	after, err := db.GetSource(ctx, ns, src.ID)
	if err != nil || !reflect.DeepEqual(after, cleared) || !reflect.DeepEqual(edgeRefsFrom(t, db, ns, src.Name), []string{"worker"}) {
		t.Fatalf("source partial rollback: %+v %v", after, err)
	}
}

func TestResourceCASSourceDeleteRecreateABA(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "source-aba")
	src := newSource(t, db, ns, "entry", store.SourceKindManual, `{}`)
	old := store.ResourceVersion(src.ID, src.ResourceVersion)
	if err := db.DeleteSourceByName(ctx, ns, src.Name, old); err != nil {
		t.Fatal(err)
	}
	fresh := newSource(t, db, ns, "entry", store.SourceKindManual, `{}`)
	if fresh.ID == src.ID {
		t.Fatal("UID reused")
	}
	if _, err := db.UpdateSourceByName(ctx, ns, src.Name, store.SourcePatch{}, old); err == nil {
		t.Fatal("old source token wrote replacement")
	} else {
		requireConflictToken(t, err, fresh.ID, fresh.ResourceVersion)
	}
	requireConflictToken(t, db.DeleteSourceByName(ctx, ns, src.Name, old), fresh.ID, fresh.ResourceVersion)
}

func TestResourceCASNodeRootConfigRaceAndABA(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	n := newClusterNode(t, db, "node-shared")
	cfg := putNodeCfg(t, db, n.ID, []string{"default"}, map[string]string{"KEEP": "value"}, nil)
	if err := db.RecordHello(ctx, n.ID, protocol.NodeFacts{Hostname: "host"}, nil, []string{"*"}, map[string]string{"LOCAL": "local"}, nil, "v4", "commit"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordApplied(ctx, n.ID, cfg.Revision, "hash"); err != nil {
		t.Fatal(err)
	}
	root, err := db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	token := store.ResourceVersion(root.ID, root.ResourceVersion)
	tags := []string{"pool"}
	other := *cfg
	other.Vars = map[string]string{"OTHER": "other"}
	casRace(t, func() error { _, err := db.UpdateNode(ctx, n.Name, store.NodePatch{Tags: &tags}, token); return err }, func() error { return db.PutNodeConfigByName(ctx, n.Name, &other, token) })
	rootAfter, err := db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	configAfter, err := db.GetNodeConfig(ctx, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rootAfter.ResourceVersion != cfg.ResourceVersion+1 || rootAfter.ResourceVersion != configAfter.ResourceVersion || rootAfter.Name != n.Name || rootAfter.Facts.Hostname != "host" || rootAfter.TokenHash != n.TokenHash || configAfter.LocalVars["LOCAL"] != "local" || configAfter.AppliedHash != "hash" {
		t.Fatalf("root/config/facts torn: %+v %+v", rootAfter, configAfter)
	}
	if (len(rootAfter.Tags) > 0) == (configAfter.Vars["OTHER"] != "") {
		t.Fatal("both spec winners committed")
	}
	latestToken := store.ResourceVersion(n.ID, rootAfter.ResourceVersion)
	if err := db.DeleteNodeByName(ctx, n.Name, latestToken); err != nil {
		t.Fatal(err)
	}
	fresh := newClusterNode(t, db, n.Name)
	if fresh.ID == n.ID {
		t.Fatal("node UID reused")
	}
	requireConflictToken(t, db.DeleteNodeByName(ctx, n.Name, latestToken), fresh.ID, fresh.ResourceVersion)
	if _, err := db.UpdateNode(ctx, n.Name, store.NodePatch{Tags: &tags}, latestToken); err == nil {
		t.Fatal("node ABA update")
	} else {
		requireConflictToken(t, err, fresh.ID, fresh.ResourceVersion)
	}
}

func TestResourceCASNodeEqualRevisionRecreateABA(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		mutate func(*store.DB, string, string) error
	}{
		{"UpdateNode", func(db *store.DB, name, token string) error {
			tags := []string{"stale"}
			_, err := db.UpdateNode(ctx, name, store.NodePatch{Tags: &tags, Vars: map[string]string{"STALE": "write"}}, token)
			return err
		}},
		{"PutNodeConfigByName", func(db *store.DB, name, token string) error {
			return db.PutNodeConfigByName(ctx, name, &store.NodeConfig{Namespaces: []string{"*"}, Vars: map[string]string{"STALE": "write"}}, token)
		}},
		{"DeleteNodeByName", func(db *store.DB, name, token string) error {
			return db.DeleteNodeByName(ctx, name, token)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			old := newClusterNode(t, db, "node-equal-revision")
			oldRoot, err := db.GetNode(ctx, old.ID)
			if err != nil {
				t.Fatal(err)
			}
			oldNodeToken := store.ResourceVersion(oldRoot.ID, oldRoot.ResourceVersion)
			if err := db.DeleteNodeByName(ctx, oldRoot.Name, oldNodeToken); err != nil {
				t.Fatal(err)
			}

			// Direct creation avoids putNodeCfg's implicit counter increment.
			fresh := &store.Node{Name: oldRoot.Name, Tags: []string{"keep"}, Facts: protocol.NodeFacts{Hostname: "replacement"}, MaxInstances: 7, MaxConcurrency: 3}
			if _, err := db.CreateNode(ctx, fresh); err != nil {
				t.Fatal(err)
			}
			rootBefore, err := db.GetNode(ctx, fresh.ID)
			if err != nil {
				t.Fatal(err)
			}
			configBefore, err := db.GetNodeConfig(ctx, fresh.ID)
			if err != nil {
				t.Fatal(err)
			}
			if oldRoot.ID == rootBefore.ID || oldRoot.ResourceVersion != 1 || rootBefore.ResourceVersion != oldRoot.ResourceVersion || configBefore.ResourceVersion != rootBefore.ResourceVersion {
				t.Fatalf("ABA fixture needs different UIDs at the same raw revision: old=%+v fresh=%+v config=%+v", oldRoot, rootBefore, configBefore)
			}

			requireConflictToken(t, tc.mutate(db, fresh.Name, oldNodeToken), rootBefore.ID, rootBefore.ResourceVersion)
			rootAfter, err := db.GetNode(ctx, fresh.ID)
			if err != nil || !reflect.DeepEqual(rootAfter, rootBefore) {
				t.Fatalf("rejected old UID changed replacement root/fields/revision: %+v %v", rootAfter, err)
			}
			configAfter, err := db.GetNodeConfig(ctx, fresh.ID)
			if err != nil || !reflect.DeepEqual(configAfter, configBefore) {
				t.Fatalf("rejected old UID changed replacement config/fields/revisions: %+v %v", configAfter, err)
			}
		})
	}
}

func TestResourceCASNamespaceRootConfigRaceAndABA(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id := newNS(t, db, "ns-shared")
	newNS(t, db, "keep-namespace")
	ns, err := db.GetNamespace(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	token := store.ResourceVersion(ns.ID, ns.ResourceVersion)
	casRace(t, func() error { _, err := db.UpdateNamespace(ctx, ns.Slug, "display", token); return err }, func() error {
		_, err := db.PutNamespaceConfigByName(ctx, ns.Slug, map[string]string{"VALUE": "keep"}, token)
		return err
	})
	root, err := db.GetNamespace(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := db.GetNamespaceConfig(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if root.ResourceVersion != ns.ResourceVersion+1 || root.ResourceVersion != cfg.ResourceVersion {
		t.Fatalf("namespace config bypass: %+v %+v", root, cfg)
	}
	if err := db.DeleteNamespaceByName(ctx, ns.Slug, true, store.ResourceVersion(root.ID, root.ResourceVersion)); err != nil {
		t.Fatal(err)
	}
	fresh, err := db.CreateNamespace(ctx, ns.Slug, "new")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateNamespace(ctx, ns.Slug, "old", token); err == nil {
		t.Fatal("namespace ABA")
	} else {
		requireConflictToken(t, err, fresh.ID, fresh.ResourceVersion)
	}
}

func TestResourceCASPlaybookRecreateABAAndRace(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-cas")
	p := newPBPackage("cas-pack").withDefault()
	row := pbCreate(t, db, ns, p)
	token := store.ResourceVersion(row.ID, row.ResourceVersion)
	casRace(t, func() error { _, err := db.ReplacePlaybook(ctx, ns, p.slug, token, p.build(), "one"); return err }, func() error { _, err := db.ReplacePlaybook(ctx, ns, p.slug, token, p.build(), "two"); return err })
	latest, err := db.GetPlaybook(ctx, ns, p.slug)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DeletePlaybook(ctx, ns, p.slug, store.ResourceVersion(latest.ID, latest.ResourceVersion)); err != nil {
		t.Fatal(err)
	}
	fresh := pbCreate(t, db, ns, p)
	if _, err := db.ReplacePlaybook(ctx, ns, p.slug, token, p.build(), ""); err == nil {
		t.Fatal("playbook ABA replace")
	} else {
		requireConflictToken(t, err, fresh.ID, fresh.ResourceVersion)
	}
	if _, err := db.DeletePlaybook(ctx, ns, p.slug, token); err == nil {
		t.Fatal("playbook ABA delete")
	} else {
		requireConflictToken(t, err, fresh.ID, fresh.ResourceVersion)
	}
}

func TestResourceCASTokenValidation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "token-validation")
	cap := createCap(t, db, testCap(ns, "worker", 1, nil))
	for _, bad := range []string{"1", "rv1::1", "rv1:" + cap.ID + ":0", "rv1:" + cap.ID + ":01", "rv1:" + cap.ID + ":-1", "rv1:" + cap.ID + ":9223372036854775808", "rv1:" + cap.ID + ":1:extra"} {
		if _, err := db.UpdateCapability(ctx, ns, cap.Name, 1, store.CapabilityPatch{}, bad); !errors.Is(err, store.ErrResourceVersionInvalid) {
			t.Fatalf("invalid token %q: %v", bad, err)
		}
	}
	if _, err := db.UpdateCapability(ctx, ns, cap.Name, 1, store.CapabilityPatch{}, ""); !errors.Is(err, store.ErrResourceVersionRequired) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := db.UpdateCapability(ctx, ns, "absent", 1, store.CapabilityPatch{}, store.ResourceVersion(cap.ID, 1)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("absent: %v", err)
	}
}

func TestResourceCASPinnedTaskUsesOldSpecAfterRecreate(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pin-body")
	definition := testCap(ns, "worker", 1, nil)
	definition.Spec.Requires.Values = []string{"vars.OLD"}
	old := createCap(t, db, definition)
	task := newTask(t, db, ns, old, "pin-body-task", "")
	node := newClusterNode(t, db, "pin-body-node")
	if err := db.RecordHello(ctx, node.ID, protocol.NodeFacts{}, nil, []string{"*"}, map[string]string{"OLD": "available"}, nil, "", ""); err != nil {
		t.Fatal(err)
	}
	nodeOnline(t, db, node.ID)
	deploys, err := db.CreateDeployments(ctx, ns, old.ID, old.Digest, []string{node.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkDeployResult(ctx, ns, deploys[0].ID, store.DeploymentApplied, ""); err != nil {
		t.Fatal(err)
	}
	before, _, err := db.EligibleNodesForTask(ctx, task)
	if err != nil || len(before) != 1 {
		t.Fatalf("pin test prerequisite: %+v %v", before, err)
	}
	if err := db.DeleteCapability(ctx, ns, old.Name, store.ResourceVersion(old.ID, old.ResourceVersion)); err != nil {
		t.Fatal(err)
	}
	replacement := testCap(ns, old.Name, 1, nil)
	replacement.Spec.Requires.Values = []string{"vars.NEW"}
	fresh := createCap(t, db, replacement)
	if fresh.ID == old.ID || fresh.Digest == old.Digest {
		t.Fatal("replacement must have distinct execution content")
	}
	after, reason, err := db.EligibleNodesForTask(ctx, task)
	if err != nil || len(after) != 1 || after[0].ID != node.ID {
		t.Fatalf("pinned reader followed new name/content: %+v %v %v", after, reason, err)
	}
}

func TestResourceCASConcurrentCreateAndDeleteRecreate(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "create-race")
	a, b := testCap(ns, "worker", 1, nil), testCap(ns, "worker", 1, nil)
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; results <- db.CreateCapability(ctx, a) }()
	go func() { <-start; results <- db.CreateCapability(ctx, b) }()
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if a.ID != b.ID || countCapRows(t, db, ns, "worker") != 1 {
		t.Fatalf("concurrent immutable create did not fold live: %+v %+v", a, b)
	}
	incoming := testCap(ns, "worker", 1, []string{"new-route"})
	start = make(chan struct{})
	go func() {
		<-start
		results <- db.DeleteCapability(ctx, ns, a.Name, store.ResourceVersion(a.ID, a.ResourceVersion))
	}()
	go func() { <-start; results <- db.CreateCapability(ctx, incoming) }()
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	live, err := db.GetCapabilityLatest(ctx, ns, a.Name)
	if incoming.ID == a.ID {
		if !errors.Is(err, store.ErrNotFound) || len(edgeRefs(t, db, ns, false, a.Name)) != 0 {
			t.Fatalf("create-before-delete not linearized: %+v %v", live, err)
		}
	} else {
		if err != nil || live.ID != incoming.ID || !reflect.DeepEqual(live.Spec.Next, []string{"new-route"}) {
			t.Fatalf("delete-before-create not linearized: %+v %v", live, err)
		}
	}
	var retained int
	if err := db.QueryRow("SELECT count(*) FROM capability_versions WHERE id = ? AND deleted_at IS NOT NULL", a.ID).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("old fact purged: %d %v", retained, err)
	}
}

func TestResourceCASSourceMetadataKeepsDeletedTargetIntent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "source-missing")
	cap := createCap(t, db, testCap(ns, "worker", 1, nil))
	src := &store.Source{NamespaceID: ns, Name: "entry", Kind: store.SourceKindManual, Enabled: true, Config: json.RawMessage(`{"payload":{"keep":true}}`), Targets: []sourcespec.Target{{Capability: "worker@1"}}}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteCapability(ctx, ns, cap.Name, store.ResourceVersion(cap.ID, cap.ResourceVersion)); err != nil {
		t.Fatal(err)
	}
	label := "missing target still declared"
	got, err := db.UpdateSourceByName(ctx, ns, src.Name, store.SourcePatch{DisplayName: &label}, store.ResourceVersion(src.ID, src.ResourceVersion))
	if err != nil {
		t.Fatalf("metadata mutation must keep incoming missing intent: %v", err)
	}
	if !reflect.DeepEqual(got.Targets, src.Targets) || !reflect.DeepEqual(got.Config, src.Config) || !reflect.DeepEqual(edgeRefsFrom(t, db, ns, src.Name), []string{"worker@1"}) {
		t.Fatalf("dangling target declaration changed: %+v", got)
	}
}

func TestResourceCASSourceDisplayKeepsCronFacts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "cron-display")
	src := newSource(t, db, ns, "clock", store.SourceKindCron, `{"expr":"0 6 * * *"}`)
	fired, next := store.Now()-100, store.Now()-30
	if err := db.MarkSourceFired(ctx, src.ID, fired, next); err != nil {
		t.Fatal(err)
	}
	before, err := db.GetSource(ctx, ns, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	label := "display only"
	got, err := db.UpdateSourceByName(ctx, ns, src.Name, store.SourcePatch{DisplayName: &label}, store.ResourceVersion(src.ID, src.ResourceVersion))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.LastFiredAt, before.LastFiredAt) || !reflect.DeepEqual(got.NextFireAt, before.NextFireAt) || !reflect.DeepEqual(got.Config, before.Config) {
		t.Fatalf("display-only rewrote cron facts: before=%+v after=%+v", before, got)
	}
}
