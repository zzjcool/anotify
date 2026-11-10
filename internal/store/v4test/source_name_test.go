package v4test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/zzjcool/anotify/internal/playbook"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

// sourceNameFacts compares every stored column, not just row counts. Values stay
// in memory: a failed assertion names the table without logging credentials.
func sourceNameFacts(t *testing.T, db *store.DB) map[string]string {
	t.Helper()
	facts := make(map[string]string, len(pbCountedTables))
	for _, table := range pbCountedTables {
		rows, err := db.Query("SELECT * FROM " + table + " ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		all := [][]any{}
		for rows.Next() {
			values := make([]any, len(cols))
			dest := make([]any, len(cols))
			for i := range values {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			all = append(all, values)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(all)
		if err != nil {
			t.Fatal(err)
		}
		facts[table] = string(body)
	}
	return facts
}

func requireSourceNameFacts(t *testing.T, db *store.DB, before map[string]string) {
	t.Helper()
	after := sourceNameFacts(t, db)
	for _, table := range pbCountedTables {
		if before[table] != after[table] {
			t.Fatalf("rejected source name changed persistent facts in %s", table)
		}
	}
}

func TestStoredSourceNameRenderDryRunInstallRejectCreateReuse(t *testing.T) {
	if store.ErrReservedEventSource != sourcespec.ErrReservedName {
		t.Fatal("original exported sentinel is not the shared pointer")
	}
	for _, tc := range []struct {
		label, template, name string
		want                  error
	}{
		{"literal_route", "route", "unused", store.ErrReservedEventSource},
		{"values_route", "{{values.name}}", "route", store.ErrReservedEventSource},
		{"trimmed_route", "{{values.name}}", " \troute\n ", store.ErrReservedEventSource},
		{"empty", "{{values.name}}", "", sourcespec.ErrEmptyName},
		{"whitespace", "{{values.name}}", " \t\n ", sourcespec.ErrEmptyName},
	} {
		for _, reuse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reuse=%t", tc.label, reuse), func(t *testing.T) {
				db := openTestDB(t)
				ctx := context.Background()
				ns := newNS(t, db, "source-names")
				if reuse {
					// Emulate a historical row; creation APIs must not create it.
					existing := &store.Source{NamespaceID: ns, Name: "historical", Kind: store.SourceKindWebhook,
						Config: json.RawMessage("{}"), Enabled: true, Targets: []sourcespec.Target{}}
					if err := db.CreateSource(ctx, existing); err != nil {
						t.Fatal(err)
					}
					actual := tc.name
					if tc.template == "route" {
						actual = "route"
					}
					if _, err := db.Exec("UPDATE sources SET name = ? WHERE id = ?", actual, existing.ID); err != nil {
						t.Fatal(err)
					}
				}
				p := newPBPackage("source-name").withDefault()
				p.values = pbValuesSchema + "\n      name: {type: string}"
				p.files["sources/on-push.yaml"] = fmt.Sprintf("metadata:\n  name: %q\nspec:\n  kind: webhook\n  config: {}\n  targets:\n    - capability: '{{values.project}}-review'\n", tc.template)
				row := pbCreate(t, db, ns, p)
				values := map[string]any{"project": "a", "name": tc.name}
				before := sourceNameFacts(t, db)
				if rendered, err := db.RenderPlaybook(ctx, ns, p.slug, values); !errors.Is(err, tc.want) || rendered != nil || !playbook.IsCode(err, playbook.CodeRenderNameInvalid) {
					t.Fatalf("stored render did not reject with shared identity/domain code: %v", err)
				}
				requireSourceNameFacts(t, db, before)
				for _, dryRun := range []bool{true, false} {
					r, err := db.InstallPlaybook(ctx, ns, p.slug, values, dryRun)
					if !errors.Is(err, tc.want) || r != nil || !playbook.IsCode(err, playbook.CodeRenderNameInvalid) {
						t.Fatalf("dryRun=%t accepted invalid name or lost error identity: %v", dryRun, err)
					}
					requireSourceNameFacts(t, db, before)
				}
				got, err := db.GetPlaybook(ctx, ns, p.slug)
				if err != nil || !reflect.DeepEqual(got.Files, row.Files) || values["name"] != tc.name || values["project"] != "a" {
					t.Fatal("stored raw package or caller values changed")
				}
			})
		}
	}
}

func TestInstallSourceLegalKindsNamesEmptyTargetsAndPending(t *testing.T) {
	for _, kind := range []string{sourcespec.KindWebhook, sourcespec.KindCron, sourcespec.KindManual} {
		for _, name := range []string{"github", "cron", "http", "manual", "Route", "route-a", " 每晚体检 "} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				db := openTestDB(t)
				ctx := context.Background()
				ns := newNS(t, db, "legal-source")
				p := newPBPackage("legal-source").withDefault()
				config := "{}"
				if kind == sourcespec.KindCron {
					config = "{expr: '0 3 * * *', tz: UTC}"
				}
				p.files["sources/on-push.yaml"] = fmt.Sprintf("metadata:\n  name: %q\nspec:\n  kind: %s\n  config: %s\n  targets: []\n", name, kind, config)
				row := pbCreate(t, db, ns, p)
				before := sourceNameFacts(t, db)
				dry, err := db.InstallPlaybook(ctx, ns, p.slug, pbValues("a"), true)
				if err != nil || dry == nil || !dry.Install.DryRun || len(dry.Created.Sources) != 1 || dry.Created.Sources[0] != name {
					t.Fatalf("legal empty-target dryRun rejected/renamed source: %v", err)
				}
				requireSourceNameFacts(t, db, before)
				r := pbInstall(t, db, ns, p.slug, pbValues("a"))
				src := pbSource(t, db, ns, name)
				if src.Name != name || src.Kind != kind || src.InstalledFrom != r.Install.ID || len(src.Targets) != 0 {
					t.Fatal("legal source identity/kind/empty targets/installedFrom changed")
				}
				before = sourceNameFacts(t, db)
				reused, err := db.InstallPlaybook(ctx, ns, p.slug, pbValues("a"), true)
				if err != nil || reused == nil || len(reused.Skipped.Sources) != 1 || reused.Skipped.Sources[0] != name {
					t.Fatalf("legal empty-target reuse rejected: %v", err)
				}
				requireSourceNameFacts(t, db, before)
				// New legal source with an unresolved target stays pending. The
				// install defense must not invoke direct CreateSource target lookup.
				p.files["sources/on-push.yaml"] = fmt.Sprintf("metadata:\n  name: %q\nspec:\n  kind: %s\n  config: %s\n  targets:\n    - capability: missing-target\n", name+"-pending", kind, config)
				pbReplace(t, db, ns, p, store.ResourceVersion(row.ID, row.ResourceVersion))
				pending := pbInstall(t, db, ns, p.slug, pbValues("a"))
				if len(pending.Pending) != 1 || pending.Pending[0] != "missing-target" {
					t.Fatalf("pending target behavior changed: %v", pending.Pending)
				}
				pendingSource := pbSource(t, db, ns, name+"-pending")
				if pendingSource.Name != name+"-pending" || len(pendingSource.Targets) != 1 || pendingSource.Targets[0].Capability != "missing-target" {
					t.Fatal("pending source input renamed or target lost")
				}
			})
		}
	}
}
