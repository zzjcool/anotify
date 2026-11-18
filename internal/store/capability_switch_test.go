package store

// capability_switch_test.go 覆盖能力启停的四条底线（capability-lifecycle §10 store 清单）：
//
//   - 无开关行 = enabled（读路径**不写行**，老库升级零回填）；
//   - 写 → 读往返一致（enabled 0/1、reason、updated_by、updated_at 存什么读什么）；
//   - enable/disable 幂等：重复调用不报错、覆盖最近一次操作、**每次都留一条审计**；
//   - 审计 append-only + 倒序 + limit 夹取；命名空间删除后开关行与审计行一起消失。
//
// 「改回去即红」的判据：把 GetCapabilitySwitch 的 ErrNoRows 分支改成 ErrNotFound，
// 或把 SetCapabilitySwitch 里的审计写入删掉，这里的用例立刻红。

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestCapabilitySwitchDefaultEnabled 断言「无行 = enabled」且读不写行。
func TestCapabilitySwitchDefaultEnabled(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	sw, err := db.GetCapabilitySwitch(ctx, nsID, "review")
	if err != nil {
		t.Fatalf("get default switch: %v", err)
	}
	if !sw.Enabled {
		t.Fatalf("无开关行必须等于 enabled，得到 %+v", sw)
	}
	if sw.Reason != "" || sw.UpdatedAt != 0 || sw.UpdatedBy != "" {
		t.Fatalf("默认开关不应带任何操作事实，得到 %+v", sw)
	}
	if enabled, err := db.IsCapabilityEnabled(ctx, nsID, "review"); err != nil || !enabled {
		t.Fatalf("IsCapabilityEnabled 默认应为 (true, nil)，得到 (%v, %v)", enabled, err)
	}

	// 读路径不许写库：没有行才是默认值的唯一表达。
	var rows int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM capability_switches WHERE namespace_id = ?`, nsID).Scan(&rows); err != nil {
		t.Fatalf("count switches: %v", err)
	}
	if rows != 0 {
		t.Fatalf("读开关不应写行，实际 %d 行", rows)
	}
}

// TestCapabilitySwitchRoundtripAndIdempotent 断言写读往返 + 幂等 + 每次留审计。
func TestCapabilitySwitchRoundtripAndIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, userID := newTestNS(t, db)
	base := Now()

	// disable（带 reason）→ 读回 0 + reason + 操作者。
	if _, err := db.SetCapabilitySwitch(ctx, CapabilitySwitchInput{
		NamespaceID: nsID, Name: "review", Enabled: false,
		Reason: "上游在改接口", ActorID: userID, Now: base,
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	sw, err := db.GetCapabilitySwitch(ctx, nsID, "review")
	if err != nil {
		t.Fatalf("get switch: %v", err)
	}
	if sw.Enabled || sw.Reason != "上游在改接口" || sw.UpdatedBy != userID || sw.UpdatedAt != base {
		t.Fatalf("disable 往返不一致：%+v", sw)
	}

	// 幂等：再调一次 disable（不同 reason/时刻）→ 不报错，覆盖最近一次操作。
	if _, err := db.SetCapabilitySwitch(ctx, CapabilitySwitchInput{
		NamespaceID: nsID, Name: "review", Enabled: false,
		Reason: "再确认一次", ActorID: userID, Now: base + 10,
	}); err != nil {
		t.Fatalf("disable again: %v", err)
	}
	sw, err = db.GetCapabilitySwitch(ctx, nsID, "review")
	if err != nil {
		t.Fatalf("get switch after repeat: %v", err)
	}
	if sw.Enabled || sw.Reason != "再确认一次" || sw.UpdatedAt != base+10 {
		t.Fatalf("重复 disable 应覆盖最近一次操作：%+v", sw)
	}

	// enable 回到默认语义（行仍在，但 enabled=1）。
	if _, err := db.SetCapabilitySwitch(ctx, CapabilitySwitchInput{
		NamespaceID: nsID, Name: "review", Enabled: true, ActorID: userID, Now: base + 20,
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	sw, err = db.GetCapabilitySwitch(ctx, nsID, "review")
	if err != nil {
		t.Fatalf("get switch after enable: %v", err)
	}
	if !sw.Enabled || sw.Reason != "" {
		t.Fatalf("enable 后应 enabled 且 reason 清空：%+v", sw)
	}

	// 三次操作 → 三条审计（幂等**不**跳过审计）。
	entries, err := db.ListCapabilityAudit(ctx, nsID, "review", 10)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("三次启停应留三条审计，得到 %d：%+v", len(entries), entries)
	}
	// 倒序：最新在前。
	want := []string{CapabilityAuditEnable, CapabilityAuditDisable, CapabilityAuditDisable}
	for i, e := range entries {
		if e.Action != want[i] {
			t.Fatalf("审计第 %d 条 action = %q, want %q（%+v）", i, e.Action, want[i], entries)
		}
		if e.ActorID != userID {
			t.Fatalf("审计第 %d 条 actor = %q, want %q", i, e.ActorID, userID)
		}
	}
	// detail 只存 reason 摘要。
	if got := string(entries[0].Detail); got != `{}` {
		t.Fatalf("enable 的 detail 应为空摘要 {}，得到 %s", got)
	}
	if got := string(entries[1].Detail); got != `{"reason":"再确认一次"}` {
		t.Fatalf("disable 的 detail 应含 reason，得到 %s", got)
	}
}

// TestCapabilitySwitchPerNamespaceAndName 断言开关按 (ns, name) 独立：改一个不波及其它。
func TestCapabilitySwitchPerNamespaceAndName(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsA, userID := newTestNS(t, db)
	nsB := newTestExtraNS(t, db, "other")

	if _, err := db.SetCapabilitySwitch(ctx, CapabilitySwitchInput{
		NamespaceID: nsA, Name: "review", Enabled: false, ActorID: userID, Now: Now(),
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	// 同 ns 的另一个名字、另一个 ns 的同名能力都不受影响。
	for _, probe := range []struct{ ns, name string }{
		{nsA, "clean"},
		{nsB, "review"},
	} {
		enabled, err := db.IsCapabilityEnabled(ctx, probe.ns, probe.name)
		if err != nil {
			t.Fatalf("probe %s/%s: %v", probe.ns, probe.name, err)
		}
		if !enabled {
			t.Fatalf("%s/%s 被误伤成 disabled", probe.ns, probe.name)
		}
	}
}

// TestCapabilityAuditLimit 断言 limit 边界（1 / 200 / 越界）与空列表形状。
func TestCapabilityAuditLimit(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, userID := newTestNS(t, db)

	// 空审计 → []（不是 nil / null）。
	empty, err := db.ListCapabilityAudit(ctx, nsID, "review", 10)
	if err != nil {
		t.Fatalf("list empty audit: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("空审计必须是 len 0 的非 nil 切片，得到 %#v", empty)
	}

	base := Now()
	for i := 0; i < 205; i++ {
		if _, err := db.AppendCapabilityAudit(ctx, CapabilityAuditInput{
			NamespaceID: nsID, Name: "review", Action: CapabilityAuditTest,
			ActorID: userID, Detail: []byte(`{"traceId":"t"}`), Now: base + int64(i),
		}); err != nil {
			t.Fatalf("append audit %d: %v", i, err)
		}
	}
	cases := []struct{ limit, want int }{
		{0, CapabilityAuditDefaultLimit}, // 0 → 默认 50
		{1, 1},
		{50, 50},
		{200, 200},
		{201, CapabilityAuditMaxLimit}, // 越界 → 夹到 200
		{100000, CapabilityAuditMaxLimit},
	}
	for _, c := range cases {
		got, err := db.ListCapabilityAudit(ctx, nsID, "review", c.limit)
		if err != nil {
			t.Fatalf("list audit limit %d: %v", c.limit, err)
		}
		if len(got) != c.want {
			t.Fatalf("limit %d → %d 条, want %d", c.limit, len(got), c.want)
		}
	}

	// 倒序且稳定（同 created_at 用 id 破平局，不丢不重）。
	all, err := db.ListCapabilityAudit(ctx, nsID, "review", CapabilityAuditMaxLimit)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	seen := map[string]bool{}
	for i, e := range all {
		if seen[e.ID] {
			t.Fatalf("审计行重复出现：%s", e.ID)
		}
		seen[e.ID] = true
		if i > 0 && e.CreatedAt > all[i-1].CreatedAt {
			t.Fatalf("审计未按时间倒序：%d > %d", e.CreatedAt, all[i-1].CreatedAt)
		}
	}
}

// TestCapabilityAuditActionClosedSet 断言 action 封闭集合（非法值当场报错，不落行）。
func TestCapabilityAuditActionClosedSet(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	if _, err := db.AppendCapabilityAudit(ctx, CapabilityAuditInput{
		NamespaceID: nsID, Name: "review", Action: "pause", Now: Now(),
	}); err == nil {
		t.Fatal("action=pause 不在封闭集合内，必须报错")
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM capability_audit WHERE namespace_id = ?`, nsID).Scan(&rows); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if rows != 0 {
		t.Fatalf("被拒的审计不应落行，实际 %d 行", rows)
	}
}

// TestCapabilitySwitchCascadeOnNamespaceDelete 断言删命名空间后开关行与审计行都消失。
func TestCapabilitySwitchCascadeOnNamespaceDelete(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	_, userID := newTestNS(t, db) // 注册者：默认 ns 只用来确有一个管理员存在
	nsID := newTestExtraNS(t, db, "victim")

	if _, err := db.SetCapabilitySwitch(ctx, CapabilitySwitchInput{
		NamespaceID: nsID, Name: "review", Enabled: false, ActorID: userID, Now: Now(),
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	ns, err := db.GetNamespace(ctx, nsID)
	if err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	if err := db.DeleteNamespace(ctx, nsID, true, ns.ResourceVersion); err != nil {
		t.Fatalf("delete namespace: %v", err)
	}
	for _, table := range []string{"capability_switches", "capability_audit"} {
		var rows int
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE namespace_id = ?`, nsID).Scan(&rows); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if rows != 0 {
			t.Fatalf("%s 在命名空间删除后仍有 %d 行", table, rows)
		}
	}
}

// TestCapabilitySwitchUnknownNamespace 断言不存在命名空间 → ErrNamespaceNotFound（审计不悬空）。
func TestCapabilitySwitchUnknownNamespace(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.SetCapabilitySwitch(ctx, CapabilitySwitchInput{
		NamespaceID: "ns_ghost", Name: "review", Enabled: false, Now: time.Now().Unix(),
	}); !errors.Is(err, ErrNamespaceNotFound) {
		t.Fatalf("不存在的 ns 应报 ErrNamespaceNotFound，得到 %v", err)
	}
}
