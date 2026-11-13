package store

// 本文件覆盖 #377 的存储侧：八个列表的 keyset 翻页查询。
//
// 守的是三条性质（比「翻两页能跑」更硬）：
//
//  1. **不漏不重**：按页拿到的行拼起来，与一次性全量列表在顺序、集合上逐条一致；
//  2. **并列排序键稳定**：created_at / name 相同的行靠 id 破平局，页边界不抖动
//     （同秒批量落库是常态，只按时间比较会在边界丢行或重复行）；
//  3. **跨页写操作可预期**：翻页途中插入/删除只影响「排序位置在游标之后」的行——
//     已翻过的行不会重复出现，其余行不会凭空消失。
//
// 边界口径也在这里钉住：limit <= 0 → 50；limit > 501 → 501（handler 多取一行的上限）。

import (
	"context"
	"fmt"
	"sort"
	"testing"
)

// pageTestSetCreatedAt 把一行的 created_at 改成确定的秒数。
//
// 为什么要改：CreateXxx 一律落 Now()，同一测试里的行会挤在同一秒，
// 想构造「不同秒 + 同秒并列」必须显式设时钟。table/id 都是测试常量，不来自外部。
func pageTestSetCreatedAt(t *testing.T, db *DB, table, id string, at int64) {
	t.Helper()
	if _, err := db.Exec(`UPDATE `+table+` SET created_at = ? WHERE id = ?`, at, id); err != nil {
		t.Fatalf("set %s.created_at(%s)=%d: %v", table, id, at, err)
	}
}

// pageTestIDs 把一组行 id 化，便于整段比较。
func pageTestIDs[T any](rows []T, id func(T) string) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, id(row))
	}
	return out
}

func pageTestEqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestListSourcesPageNoDupNoGap 覆盖 DESC 排序 + created_at 并列：两组各 6 条同秒的源，
// limit 交错翻页后必须与 ListSources 完全一致。
func TestListSourcesPageNoDupNoGap(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	var want []string
	for group, at := range []int64{400, 300, 200, 100} {
		var groupIDs []string
		for i := 0; i < 3; i++ {
			src := &Source{
				NamespaceID: nsID,
				Name:        fmt.Sprintf("group%d-src%d", group, i),
				Kind:        SourceKindWebhook,
			}
			if err := db.CreateSource(ctx, src); err != nil {
				t.Fatalf("create source: %v", err)
			}
			pageTestSetCreatedAt(t, db, "sources", src.ID, at)
			groupIDs = append(groupIDs, src.ID)
		}
		// 同秒并列按 id DESC 破平局（ORDER BY created_at DESC, id DESC）。
		sort.Sort(sort.Reverse(sort.StringSlice(groupIDs)))
		want = append(want, groupIDs...)
	}

	full, err := db.ListSources(ctx, nsID)
	if err != nil {
		t.Fatalf("list sources: %v", err)
	}
	if got := pageTestIDs(full, func(s *Source) string { return s.ID }); !pageTestEqualStrings(got, want) {
		t.Fatalf("前置：ListSources 顺序与夹具预期不一致\n got=%v\nwant=%v", got, want)
	}

	var got []string
	var afterAt int64
	afterID := ""
	for page := 0; ; page++ {
		if page > 50 {
			t.Fatal("翻页不收敛（游标没有前进）")
		}
		rows, err := db.ListSourcesPage(ctx, nsID, afterAt, afterID, 5)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(rows) == 0 {
			break
		}
		if len(rows) > 5 {
			t.Fatalf("page %d 返回 %d 行，超过 limit=5", page, len(rows))
		}
		for _, row := range rows {
			got = append(got, row.ID)
		}
		last := rows[len(rows)-1]
		afterAt, afterID = last.CreatedAt, last.ID
	}
	if !pageTestEqualStrings(got, want) {
		t.Fatalf("翻页结果与全量列表不一致（丢行/重复/顺序错）\n got=%v\nwant=%v", got, want)
	}
}

// TestListSourcesPageInsertDeleteAcrossPages 覆盖跨页插入/删除：
//   - 插到游标**之前**（更新）的行：已翻过的位置，后续页不能再出现；
//   - 插到游标**之后**（更旧）的行：必须出现在后续页；
//   - 删除已翻过的行：不影响剩余页；
//   - 删除还没翻到的行：只少这一行，其余不动。
func TestListSourcesPageInsertDeleteAcrossPages(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	ids := map[int64]string{}
	for _, at := range []int64{900, 800, 700, 600, 500, 400, 300, 200, 100} {
		src := &Source{NamespaceID: nsID, Name: fmt.Sprintf("src-%d", at), Kind: SourceKindWebhook}
		if err := db.CreateSource(ctx, src); err != nil {
			t.Fatalf("create source: %v", err)
		}
		pageTestSetCreatedAt(t, db, "sources", src.ID, at)
		ids[at] = src.ID
	}

	// 第一页：900 / 800 / 700（created_at DESC）。
	first, err := db.ListSourcesPage(ctx, nsID, 0, "", 3)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if got := pageTestIDs(first, func(s *Source) string { return s.ID }); !pageTestEqualStrings(got, []string{ids[900], ids[800], ids[700]}) {
		t.Fatalf("第一页顺序不对: %v", got)
	}
	afterAt, afterID := first[len(first)-1].CreatedAt, first[len(first)-1].ID

	// 游标之后（更旧）：必须出现在后续页。
	newer := &Source{NamespaceID: nsID, Name: "inserted-after-cursor", Kind: SourceKindWebhook}
	if err := db.CreateSource(ctx, newer); err != nil {
		t.Fatalf("create source: %v", err)
	}
	pageTestSetCreatedAt(t, db, "sources", newer.ID, 50)

	// 游标之前（更新）：已翻过的位置，后续页不能再出现。
	older := &Source{NamespaceID: nsID, Name: "inserted-before-cursor", Kind: SourceKindWebhook}
	if err := db.CreateSource(ctx, older); err != nil {
		t.Fatalf("create source: %v", err)
	}
	pageTestSetCreatedAt(t, db, "sources", older.ID, 850)

	// 删一行已翻过的（900）与一行还没翻到的（400）。
	if err := db.DeleteSource(ctx, nsID, ids[900], 1); err != nil {
		t.Fatalf("delete seen source: %v", err)
	}
	if err := db.DeleteSource(ctx, nsID, ids[400], 1); err != nil {
		t.Fatalf("delete unseen source: %v", err)
	}

	want := []string{ids[600], ids[500], ids[300], ids[200], ids[100], newer.ID}
	var got []string
	for page := 0; ; page++ {
		if page > 50 {
			t.Fatal("翻页不收敛")
		}
		rows, err := db.ListSourcesPage(ctx, nsID, afterAt, afterID, 2)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			got = append(got, row.ID)
		}
		last := rows[len(rows)-1]
		afterAt, afterID = last.CreatedAt, last.ID
	}
	if !pageTestEqualStrings(got, want) {
		t.Fatalf("跨页插入/删除后的剩余页不对\n got=%v\nwant=%v", got, want)
	}
	for _, id := range got {
		if id == older.ID {
			t.Fatal("游标之前插入的行不应出现在后续页（会让翻页重复）")
		}
	}
}

// TestListCapabilitiesPageCompositeKey 覆盖 (name ASC, version DESC) 复合排序键：
// 分组边界上同一 name 的多个版本不能跨页丢/重。
func TestListCapabilitiesPageCompositeKey(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	for _, v := range []int{1, 2, 3} {
		v4Cap(t, db, nsID, "alpha", v)
	}
	for _, v := range []int{1, 2} {
		v4Cap(t, db, nsID, "beta", v)
	}

	var want []string
	for _, row := range orderedCapsForPageTest(t, db, nsID) {
		want = append(want, row.ID)
	}

	var got []string
	afterName, afterVersion := "", 0
	for page := 0; ; page++ {
		if page > 50 {
			t.Fatal("翻页不收敛")
		}
		rows, err := db.ListCapabilitiesPage(ctx, nsID, afterName, afterVersion, 2)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			got = append(got, row.ID)
		}
		last := rows[len(rows)-1]
		afterName, afterVersion = last.Name, last.Version
	}
	if !pageTestEqualStrings(got, want) {
		t.Fatalf("能力翻页与全量不一致\n got=%v\nwant=%v", got, want)
	}
	// 翻页途中插入同名更高版本：它排在游标之前，不能再出现在后续页。
	if len(got) == 0 {
		t.Fatal("夹具应当有行")
	}
	newVersion := v4Cap(t, db, nsID, "alpha", 4)
	rest, err := db.ListCapabilitiesPage(ctx, nsID, afterName, afterVersion, 100)
	if err != nil {
		t.Fatalf("tail page: %v", err)
	}
	for _, row := range rest {
		if row.ID == newVersion.ID {
			t.Fatal("翻页途中插入的更高版本排在游标之前，不应出现在后续页")
		}
	}
}

// orderedCapsForPageTest 返回对外顺序（name ASC、version DESC），不走 SQL。
func orderedCapsForPageTest(t *testing.T, db *DB, nsID string) []*CapabilityVersion {
	t.Helper()
	rows, err := db.ListCapabilities(context.Background(), nsID)
	if err != nil {
		t.Fatalf("list capabilities: %v", err)
	}
	out := make([]*CapabilityVersion, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- { // store 是 (name asc, version asc) → 反着收一遍
		out = append(out, rows[i])
	}
	// 重新按 (name asc, version desc) 排（上面的反序只对同 name 有效）。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			if a.Name < b.Name || (a.Name == b.Name && a.Version > b.Version) {
				break
			}
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// TestListPlaybooksPageAscendingWithTies 覆盖 ASC 排序 + 同 created_at 并列。
func TestListPlaybooksPageAscendingWithTies(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	var created []string
	for i := 0; i < 6; i++ {
		row := pbCreate(t, db, nsID, fmt.Sprintf("pb-%d", i), pbEmpty(fmt.Sprintf("pb-%d", i)))
		pageTestSetCreatedAt(t, db, "playbooks", row.ID, 1000) // 全部同秒 → id ASC 破平局
		created = append(created, row.ID)
	}
	full, err := db.ListPlaybooks(ctx, nsID)
	if err != nil {
		t.Fatalf("list playbooks: %v", err)
	}
	want := pageTestIDs(full, func(p *PlaybookRow) string { return p.ID })

	var got []string
	afterAt, afterID := int64(0), ""
	for page := 0; ; page++ {
		if page > 50 {
			t.Fatal("翻页不收敛")
		}
		rows, err := db.ListPlaybooksPage(ctx, nsID, afterAt, afterID, 4)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			got = append(got, row.ID)
		}
		last := rows[len(rows)-1]
		afterAt, afterID = last.CreatedAt, last.ID
	}
	if !pageTestEqualStrings(got, want) {
		t.Fatalf("包翻页与全量不一致\n got=%v\nwant=%v", got, want)
	}
}

// TestListNodesPage 覆盖 (name, id) 排序键。
func TestListNodesPage(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	names := []string{"node-d", "node-b", "node-a", "node-c", "node-e"}
	byName := map[string]string{}
	for _, name := range names {
		n := &Node{Name: name}
		if _, err := db.CreateNode(ctx, n); err != nil {
			t.Fatalf("create node: %v", err)
		}
		byName[name] = n.ID
	}
	sort.Strings(names)
	want := make([]string, 0, len(names))
	for _, name := range names {
		want = append(want, byName[name])
	}
	full, err := db.ListNodes(ctx)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if got := pageTestIDs(full, func(n *Node) string { return n.ID }); !pageTestEqualStrings(got, want) {
		t.Fatalf("ListNodes 顺序应为 name 升序，got=%v", got)
	}

	var got []string
	afterName, afterID := "", ""
	for page := 0; ; page++ {
		if page > 50 {
			t.Fatal("翻页不收敛")
		}
		rows, err := db.ListNodesPage(ctx, afterName, afterID, 2)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			got = append(got, row.ID)
		}
		last := rows[len(rows)-1]
		afterName, afterID = last.Name, last.ID
	}
	if !pageTestEqualStrings(got, want) {
		t.Fatalf("节点翻页与全量不一致\n got=%v\nwant=%v", got, want)
	}
}

// TestListUsersKeysPasskeysSessionsPage 覆盖剩余四个 (created_at, id) 列表。
func TestListUsersKeysPasskeysSessionsPage(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// ---- admin users：ASC + 同秒并列 ----
	var userIDs []string
	for i := 0; i < 5; i++ {
		id, err := db.InsertUser(ctx, "", fmt.Sprintf("user-%d", i), fmt.Sprintf("User %d", i))
		if err != nil {
			t.Fatalf("insert user: %v", err)
		}
		pageTestSetCreatedAt(t, db, "users", id, 2000)
		userIDs = append(userIDs, id)
	}
	// created_at 全同 → 期望顺序是**插入序**（rowid）：id 是 crypto/rand，拿它破平局
	// 会让「首注册用户排最前」变成掷硬币（#377 复审：TestAdmin_ListUsers 42.5% 随机红）。
	wantUsers := append([]string(nil), userIDs...)
	var gotUsers []string
	afterAt, afterID := int64(0), ""
	for page := 0; ; page++ {
		if page > 50 {
			t.Fatal("用户翻页不收敛")
		}
		rows, err := db.ListUsersWithStatsPage(ctx, afterAt, afterID, 2)
		if err != nil {
			t.Fatalf("users page %d: %v", page, err)
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			gotUsers = append(gotUsers, row.ID)
		}
		last := rows[len(rows)-1]
		afterAt, afterID = last.CreatedAt, last.ID
	}
	if !pageTestEqualStrings(gotUsers, wantUsers) {
		t.Fatalf("用户翻页与全量不一致（同秒并列必须按 id 稳定）\n got=%v\nwant=%v", gotUsers, wantUsers)
	}

	// ---- api keys：DESC + 同秒并列 ----
	uid := userIDs[0]
	var keyIDs []string
	for i := 0; i < 5; i++ {
		k := &APIKey{ID: fmt.Sprintf("key_%d", i), UserID: uid, Name: fmt.Sprintf("key-%d", i), KeyHash: "hash", Prefix: "ant_", CreatedAt: 3000}
		if err := db.CreateAPIKey(k); err != nil {
			t.Fatalf("create key: %v", err)
		}
		keyIDs = append(keyIDs, k.ID)
	}
	// created_at 全同 → 期望顺序是 id DESC（与 ListAPIKeysByUser 的 created_at DESC 同向补破平局）。
	wantKeys := append([]string(nil), keyIDs...)
	sort.Sort(sort.Reverse(sort.StringSlice(wantKeys)))
	var gotKeys []string
	afterAt, afterID = 0, ""
	for page := 0; ; page++ {
		if page > 50 {
			t.Fatal("key 翻页不收敛")
		}
		rows, err := db.ListAPIKeysByUserPage(ctx, uid, afterAt, afterID, 2)
		if err != nil {
			t.Fatalf("keys page %d: %v", page, err)
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			gotKeys = append(gotKeys, row.ID)
		}
		last := rows[len(rows)-1]
		afterAt, afterID = last.CreatedAt, last.ID
	}
	if !pageTestEqualStrings(gotKeys, wantKeys) {
		t.Fatalf("key 翻页与全量不一致\n got=%v\nwant=%v", gotKeys, wantKeys)
	}

	// ---- passkeys：ASC ----
	var pkIDs []string
	for i := 0; i < 4; i++ {
		p := &Passkey{ID: fmt.Sprintf("pk_%d", i), UserID: uid, PublicKey: []byte{byte(i)}, Name: fmt.Sprintf("pk-%d", i), Transports: []string{}, CreatedAt: 4000}
		if err := db.CreatePasskey(p); err != nil {
			t.Fatalf("create passkey: %v", err)
		}
		pkIDs = append(pkIDs, p.ID)
	}
	// created_at 全同 → 期望顺序是 id ASC。
	wantPks := append([]string(nil), pkIDs...)
	sort.Strings(wantPks)
	var gotPks []string
	afterAt, afterID = 0, ""
	for page := 0; ; page++ {
		if page > 50 {
			t.Fatal("passkey 翻页不收敛")
		}
		rows, err := db.ListPasskeysByUserPage(ctx, uid, afterAt, afterID, 3)
		if err != nil {
			t.Fatalf("passkeys page %d: %v", page, err)
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			gotPks = append(gotPks, row.ID)
		}
		last := rows[len(rows)-1]
		afterAt, afterID = last.CreatedAt, last.ID
	}
	if !pageTestEqualStrings(gotPks, wantPks) {
		t.Fatalf("passkey 翻页与全量不一致\n got=%v\nwant=%v", gotPks, wantPks)
	}

	// ---- sessions：DESC + 只含未过期 ----
	now := Now()
	var liveIDs []string
	for i := 0; i < 5; i++ {
		s := &Session{ID: NewSessionID(), UserID: uid, DeviceName: fmt.Sprintf("dev-%d", i), CreatedAt: 5000 + int64(i), ExpiresAt: now + 3600, LastSeen: now}
		if err := db.CreateSession(s); err != nil {
			t.Fatalf("create session: %v", err)
		}
		liveIDs = append(liveIDs, s.ID)
	}
	for i := 0; i < 2; i++ {
		s := &Session{ID: NewSessionID(), UserID: uid, DeviceName: fmt.Sprintf("expired-%d", i), CreatedAt: 6000 + int64(i), ExpiresAt: now - 60, LastSeen: now - 60}
		if err := db.CreateSession(s); err != nil {
			t.Fatalf("create expired session: %v", err)
		}
	}

	var gotSessions []string
	afterAt, afterID = 0, ""
	for page := 0; ; page++ {
		if page > 50 {
			t.Fatal("session 翻页不收敛")
		}
		rows, err := db.ListSessionsByUserPage(ctx, uid, afterAt, afterID, 2)
		if err != nil {
			t.Fatalf("sessions page %d: %v", page, err)
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			gotSessions = append(gotSessions, row.ID)
			if row.ExpiresAt <= now {
				t.Fatalf("过期会话不该出现在列表里: %+v", row)
			}
		}
		last := rows[len(rows)-1]
		afterAt, afterID = last.CreatedAt, last.ID
	}
	// DESC：createdAt 大的在前。
	wantSessions := []string{liveIDs[4], liveIDs[3], liveIDs[2], liveIDs[1], liveIDs[0]}
	if !pageTestEqualStrings(gotSessions, wantSessions) {
		t.Fatalf("会话翻页应只含未过期行且按 createdAt 倒序\n got=%v\nwant=%v", gotSessions, wantSessions)
	}
}

// TestPageLimitNormalization 钉住 store 侧兜底夹取：<=0 → 50，>501 → 501。
func TestPageLimitNormalization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.CreateNode(ctx, &Node{Name: "n1"}); err != nil {
		t.Fatalf("create node: %v", err)
	}

	cases := []struct {
		name  string
		limit int
		want  int
	}{
		{"zero-defaults-to-50", 0, pageDefaultLimit},
		{"negative-defaults-to-50", -3, pageDefaultLimit},
		{"over-fetch-cap", 10_000, pageFetchCap},
	}
	for _, c := range cases {
		rows, err := db.ListNodesPage(ctx, "", "", c.limit)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		_ = rows
		if got := normalizePageLimit(c.limit); got != c.want {
			t.Fatalf("%s: normalizePageLimit(%d)=%d want %d", c.name, c.limit, got, c.want)
		}
	}
	if pageDefaultLimit != 50 || pageFetchCap != 501 {
		t.Fatalf("默认/多取一行的上限被改了：%d/%d（handler 的 50/500 与它耦合）", pageDefaultLimit, pageFetchCap)
	}
}
