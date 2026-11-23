package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/sourcespec"
)

func TestNamespaceSecretPutListGetRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	if list, err := db.ListNamespaceSecrets(ctx, nsID); err != nil || list == nil || len(list) != 0 {
		t.Fatalf("空列表必须是 [] 不是 nil: %v %#v", err, list)
	}

	created, err := db.PutNamespaceSecret(ctx, nsID, "gh-token", "v1-SECRET", "usr_actor")
	if err != nil || !created {
		t.Fatalf("首次 PUT 必须是 created: %v %v", created, err)
	}
	if value, err := db.GetNamespaceSecret(ctx, nsID, "gh-token"); err != nil || value != "v1-SECRET" {
		t.Fatalf("值往返不一致: %q %v", value, err)
	}

	meta, err := db.GetNamespaceSecretMeta(ctx, nsID, "gh-token")
	if err != nil {
		t.Fatalf("读单条: %v", err)
	}
	if meta.Name != "gh-token" || meta.UpdatedAt == 0 || meta.CreatedAt == 0 {
		t.Fatalf("读投影缺字段: %+v", meta)
	}
	if meta.UsedBy == nil || len(meta.UsedBy) != 0 {
		t.Fatalf("未引用的凭据 usedBy 必须是 []: %#v", meta.UsedBy)
	}

	rotated, err := db.PutNamespaceSecret(ctx, nsID, "gh-token", "v2-SECRET", "usr_actor")
	if err != nil || rotated {
		t.Fatalf("同名 PUT 必须是 rotated: %v %v", rotated, err)
	}
	if value, _ := db.GetNamespaceSecret(ctx, nsID, "gh-token"); value != "v2-SECRET" {
		t.Fatalf("轮换后必须读回新值: %q", value)
	}
	audit, err := db.ListNamespaceSecretAudit(ctx, nsID, "gh-token", 10)
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	if len(audit) != 2 || audit[0].Action != NamespaceSecretRotated || audit[1].Action != NamespaceSecretCreated {
		t.Fatalf("创建/轮换各一条审计（最新在前）: %+v", audit)
	}
	for _, e := range audit {
		if e.ActorID != "usr_actor" || e.Name != "gh-token" {
			t.Fatalf("审计必须带 name 与 actor: %+v", e)
		}
		if strings.Contains(e.Action, "SECRET") {
			t.Fatalf("审计行不许含值: %+v", e)
		}
	}
}

func TestNamespaceSecretUsageAndDeleteGuard(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	sourceFixtureCap(t, db, nsID, "demo.polsec")

	if _, err := db.PutNamespaceSecret(ctx, nsID, "gh-token", "tok", ""); err != nil {
		t.Fatalf("建凭据: %v", err)
	}
	cfg := `{"url":"https://example.com/feed","schedule":{"expr":"*/5 * * * *"},
		"secretRef":{"name":"gh-token","header":"Authorization","prefix":"Bearer "}}`
	for _, name := range []string{"src-b", "src-a"} {
		if err := db.CreateSource(ctx, pollSrc(nsID, name, cfg, "demo.polsec")); err != nil {
			t.Fatalf("按 secretRef 建源 %s: %v", name, err)
		}
	}

	list, err := db.ListNamespaceSecrets(ctx, nsID)
	if err != nil || len(list) != 1 {
		t.Fatalf("列凭据: %v %#v", err, list)
	}
	if got := strings.Join(list[0].UsedBy, ","); got != "src-a,src-b" {
		t.Fatalf("usedBy 必须是引用它的源名（排序）: %q", got)
	}

	err = db.DeleteNamespaceSecret(ctx, nsID, "gh-token", "")
	var inUse *NamespaceSecretInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("被引用必须拒删: %v", err)
	}
	if len(inUse.Sources) != 2 || inUse.Sources[0] != "src-a" || inUse.Sources[1] != "src-b" {
		t.Fatalf("拒删要列出引用者: %+v", inUse.Sources)
	}
	if _, err := db.GetNamespaceSecret(ctx, nsID, "gh-token"); err != nil {
		t.Fatalf("拒删不得删掉它: %v", err)
	}

	if _, err := db.PutNamespaceSecret(ctx, nsID, "other", "tok2", ""); err != nil {
		t.Fatalf("建替代凭据: %v", err)
	}
	for _, name := range []string{"src-a", "src-b"} {
		src, err := db.GetSourceByName(ctx, nsID, name)
		if err != nil {
			t.Fatalf("读源 %s: %v", name, err)
		}
		newCfg := strings.Replace(string(src.Config),
			`"secretRef":{"name":"gh-token","header":"Authorization","prefix":"Bearer "}`,
			`"secretRef":{"name":"other","header":"Authorization"}`, 1)
		if newCfg == string(src.Config) {
			t.Fatalf("夹具没换掉 secretRef: %s", src.Config)
		}
		src.Config = json.RawMessage(newCfg)
		if err := db.UpdateSource(ctx, src); err != nil {
			t.Fatalf("改源 %s 的 secretRef: %v", name, err)
		}
	}
	if err := db.DeleteNamespaceSecret(ctx, nsID, "gh-token", "usr_actor"); err != nil {
		t.Fatalf("没人引用后必须能删: %v", err)
	}
	if _, err := db.GetNamespaceSecret(ctx, nsID, "gh-token"); !errors.Is(err, ErrNamespaceSecretNotFound) {
		t.Fatalf("删除后必须 404: %v", err)
	}
	if err := db.DeleteNamespaceSecret(ctx, nsID, "gh-token", "usr_actor"); !errors.Is(err, ErrNamespaceSecretNotFound) {
		t.Fatalf("删不存在的必须是 not found: %v", err)
	}
	audit, err := db.ListNamespaceSecretAudit(ctx, nsID, "gh-token", 10)
	if err != nil || len(audit) != 2 || audit[0].Action != NamespaceSecretDeleted {
		t.Fatalf("删除也要留一条审计: %v %+v", err, audit)
	}
}

func TestNamespaceSecretNameRuleIsShared(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	sourceFixtureCap(t, db, nsID, "demo.polsec")

	for _, name := range []string{"", "-bad", ".bad", "_bad", "has space", "有中文", strings.Repeat("a", 65)} {
		if err := sourcespec.ValidateSecretName(name); err == nil {
			t.Fatalf("%q 必须是非法名字", name)
		}
		if _, err := db.PutNamespaceSecret(ctx, nsID, name, "v", ""); err == nil {
			t.Fatalf("新面必须拒 %q", name)
		}
		cfg := `{"url":"https://example.com/x","schedule":{"expr":"*/5 * * * *"},
			"secretRef":{"name":` + jsonQuote(name) + `,"header":"Authorization"}}`
		if err := db.CreateSource(ctx, pollSrc(nsID, "src-"+NewSourceID(), cfg, "demo.polsec")); err == nil {
			t.Fatalf("老路径必须用同一规则拒 %q", name)
		}
	}
	for _, name := range []string{"a", "gh-token", "a.b_c-D9", strings.Repeat("a", 64)} {
		if err := sourcespec.ValidateSecretName(name); err != nil {
			t.Fatalf("%q 必须合法: %v", name, err)
		}
	}
}

func TestNamespaceSecretLegacySourcePathUnchanged(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	sourceFixtureCap(t, db, nsID, "demo.polsec")

	cfg := `{"url":"https://example.com/feed","schedule":{"expr":"*/5 * * * *"},
		"secretRef":{"name":"gh","header":"Authorization","prefix":"Bearer "},"secret":"legacy-tok"}`
	if err := db.CreateSource(ctx, pollSrc(nsID, "legacy", cfg, "demo.polsec")); err != nil {
		t.Fatalf("老路径必须仍能一次创建: %v", err)
	}
	if value, err := db.GetNamespaceSecret(ctx, nsID, "gh"); err != nil || value != "legacy-tok" {
		t.Fatalf("老路径必须落同一个值: %q %v", value, err)
	}
	audit, err := db.ListNamespaceSecretAudit(ctx, nsID, "", 10)
	if err != nil || len(audit) != 0 {
		t.Fatalf("老路径不该写资源面审计（行为不变）: %v %+v", err, audit)
	}
	list, err := db.ListNamespaceSecrets(ctx, nsID)
	if err != nil || len(list) != 1 || strings.Join(list[0].UsedBy, ",") != "legacy" {
		t.Fatalf("老路径建的凭据也要出现在读面并带引用者: %v %#v", err, list)
	}
}
