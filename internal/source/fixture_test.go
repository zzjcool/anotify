package source

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

func testCtx() context.Context { return context.Background() }

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/source.db")
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func testNamespace(t *testing.T, db *store.DB) *store.Namespace {
	t.Helper()
	ns, err := db.CreateNamespace(testCtx(), "default", "Default")
	if err != nil {
		t.Fatalf("建命名空间: %v", err)
	}
	return ns
}

func testCapability(t *testing.T, db *store.DB, namespaceID, name string, params string) *store.CapabilityVersion {
	t.Helper()
	if params == "" {
		params = `{"type":"object"}`
	}
	capability := &store.CapabilityVersion{
		NamespaceID: namespaceID, Name: name, Version: 1,
		Spec: artifact.Spec{
			Params: json.RawMessage(params),
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
			},
		},
	}
	if err := db.CreateCapability(testCtx(), capability); err != nil {
		t.Fatalf("建能力 %s: %v", name, err)
	}
	got, err := db.GetCapability(testCtx(), namespaceID, name, capability.Version)
	if err != nil {
		t.Fatalf("回读能力: %v", err)
	}
	return got
}

type sourceOpts struct {
	Kind    string
	Config  string
	Targets []sourcespec.Target
	Enabled bool
}

func testSource(t *testing.T, db *store.DB, namespaceID, name string, opts sourceOpts) *store.Source {
	t.Helper()
	targets := opts.Targets
	if targets == nil {
		targets = []sourcespec.Target{}
	}
	src := &store.Source{
		NamespaceID: namespaceID, Name: name, Kind: opts.Kind,
		Config: json.RawMessage(opts.Config), Targets: targets, Enabled: opts.Enabled,
	}
	if err := db.CreateSource(testCtx(), src); err != nil {
		t.Fatalf("建源 %s: %v", name, err)
	}
	got, err := db.GetSourceByName(testCtx(), namespaceID, name)
	if err != nil {
		t.Fatalf("回读源 %s: %v", name, err)
	}
	return got
}

func newService(t *testing.T, db *store.DB) *Service {
	t.Helper()
	return New(db, route.New(db))
}

func countRows(t *testing.T, db *store.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("统计 %s: %v", table, err)
	}
	return n
}
