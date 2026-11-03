package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// 本文件是 /v1/stats 时区口径（P0-TZ）的验收：Today 与热力图日桶都按
// **服务端本地时区**切日界，且结果只由传入的 now 决定。
//
// 纪律：全部用例用 time.FixedZone，绝不依赖 time.Local / TZ 环境变量，
// 也不用 time.Sleep —— 依赖真实时钟或机器时区的用例在 CI 与开发机上行为
// 分叉，恰恰是这类时区 bug 藏身的地方。

// tzFixtureLocations 是时区口径用例的固定时区夹具：东八区、西五区、东五点五区。
// 一东一西保证「有人把口径改回 UTC」时至少有一边必挂；UTC+5:30 是半小时
// 偏移的时区（`Unix() - Hour()*3600` 这类算分界的写法会在它上面算错）。
func tzFixtureLocations() []*time.Location {
	return []*time.Location{
		time.FixedZone("UTC+8", 8*3600),
		time.FixedZone("UTC-5", -5*3600),
		time.FixedZone("UTC+5:30", 5*3600+1800),
	}
}

// localDay 返回 loc 时区里 2026-05-20 当天的某个时刻。
func localDay(loc *time.Location, hour, min, sec int) time.Time {
	return time.Date(2026, 5, 20, hour, min, sec, 0, loc)
}

// insertEventAt 在 nsID 下插一条 created_at 由用例给定的事件。
//
// 统计只关心时间（InsertEvent 只要求 source/type 非空）；trace_id 由调用方给出，
// InsertEvent 强制非空（event.go:56-58），这里自己造一个唯一值。
func insertEventAt(t *testing.T, db *DB, nsID string, at time.Time) {
	t.Helper()
	if _, err := db.InsertEvent(context.Background(), &Event{
		NamespaceID: nsID,
		TraceID:     "tr_" + NewEventID(),
		Source:      "test",
		Type:        "ping",
		CreatedAt:   at.Unix(),
	}); err != nil {
		t.Fatalf("insert event at %v: %v", at, err)
	}
}

// statsTestCapability 取一个**真实**的能力定义行当 CreateTasks 的 capability_id。
//
// 不能伪造 id：capability_id 是外键（schema.sql `capability_id TEXT NOT NULL
// REFERENCES capability_versions(id)`，外键是开着的）。newTestNS 会把内置能力
// 播进默认命名空间，所以先按契约给的 "review@1" 试（内置清单里没有它，会
// ErrNotFound），再退回列表第一个。
func statsTestCapability(t *testing.T, db *DB, nsID string) *CapabilityVersion {
	t.Helper()
	ctx := context.Background()
	if c, err := db.GetCapability(ctx, nsID, "review", 1); err == nil && c != nil {
		return c
	}
	list, err := db.ListCapabilities(ctx, nsID)
	if err != nil {
		t.Fatalf("list capabilities: %v", err)
	}
	if len(list) == 0 {
		t.Fatalf("命名空间 %q 里没有任何能力定义，CreateTasks 缺外键建不了任务", nsID)
	}
	return list[0]
}

// TestStartOfLocalDayUnix 守「今日」日界函数本身：取本地 00:00:00，不是 UTC 0 点。
func TestStartOfLocalDayUnix(t *testing.T) {
	for _, loc := range tzFixtureLocations() {
		t.Run(loc.String(), func(t *testing.T) {
			now := localDay(loc, 0, 0, 30) // 本地 00:00:30
			want := localDay(loc, 0, 0, 0).Unix()
			got := startOfLocalDayUnix(now)
			if got != want {
				t.Fatalf("startOfLocalDayUnix(%v) = %d, want %d（本地 00:00:00）", now, got, want)
			}
			// 守回归：日界必须是「本地 0 点」，不是 UTC 0 点。
			if utcMidnight := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC).Unix(); got == utcMidnight {
				t.Fatalf("startOfLocalDayUnix 退化成 UTC 0 点口径：%d", got)
			}
		})
	}
}

// TestMessageStatsTodayUsesLocalMidnight 是 Today 的主验收：本地前一日 23:59:59
// 不计入、本地当日 00:00:00 起计入。
//
// 两个时区各跑一遍：把口径改回 UTC 0 点时两边都会数成 3（本地 day-1 23:59:59
// 在 UTC 下仍落在「今天」），所以这个用例对回归是红的。
func TestMessageStatsTodayUsesLocalMidnight(t *testing.T) {
	for _, loc := range tzFixtureLocations() {
		t.Run(loc.String(), func(t *testing.T) {
			db := newTestDB(t)
			ctx := context.Background()
			nsID, userID := newTestNS(t, db)

			midnight := localDay(loc, 0, 0, 0)
			insertEventAt(t, db, nsID, midnight.Add(-time.Second)) // 本地前一日 23:59:59
			insertEventAt(t, db, nsID, midnight)                   // 本地当日 00:00:00
			insertEventAt(t, db, nsID, midnight.Add(time.Hour))    // 本地当日 01:00:00

			now := localDay(loc, 0, 0, 30) // 本地 00:00:30
			s, err := db.messageStats(ctx, nsID, userID, 0, now)
			if err != nil {
				t.Fatalf("messageStats: %v", err)
			}
			if s.Total != 3 {
				t.Errorf("Total = %d, want 3（总数与日界无关）", s.Total)
			}
			if s.Today != 2 {
				t.Errorf("Today = %d, want 2（本地 00:00 起算；UTC 0 点口径会数成 3）", s.Today)
			}
		})
	}
}

// TestMessageStatsDailyUsesLocalDay 是热力图日桶的验收：分桶用与 Today 同一个
// 本地时区，日期字符串是本地日期（不是 UTC 日期），顺序按日期升序；
// 同时锁住 sinceSec 下界（窗口外的老事件不能出现在 Daily 里）。
func TestMessageStatsDailyUsesLocalDay(t *testing.T) {
	for _, loc := range tzFixtureLocations() {
		t.Run(loc.String(), func(t *testing.T) {
			db := newTestDB(t)
			ctx := context.Background()
			nsID, userID := newTestNS(t, db)

			today := localDay(loc, 0, 0, 0)      // 本地当日 00:00:00
			yesterday := today.AddDate(0, 0, -1) // 本地前一日 00:00:00

			// 这条落在 sinceSec（本地 day-1 00:00）之前，必须被下界过滤掉：
			// 把 daily SQL 的 `AND created_at >= ?` 删掉后它就会冒出来，want 必炸。
			insertEventAt(t, db, nsID, yesterday.Add(-time.Second)) // 本地 day-2 23:59:59，窗口外
			insertEventAt(t, db, nsID, today.Add(-time.Second))     // 本地 day-1 23:59:59
			insertEventAt(t, db, nsID, today)                       // 本地 day 00:00:00
			insertEventAt(t, db, nsID, today.Add(12*time.Hour))     // 本地 day 12:00:00

			now := today.Add(30 * time.Second) // 本地 day 00:00:30
			s, err := db.messageStats(ctx, nsID, userID, yesterday.Unix(), now)
			if err != nil {
				t.Fatalf("messageStats: %v", err)
			}

			// 期望日期从 FixedZone 时间算出来，不硬编码 UTC 日期。
			want := []DayCount{
				{Day: yesterday.Format("2006-01-02"), Count: 1},
				{Day: today.Format("2006-01-02"), Count: 2},
			}
			if !slices.Equal(s.Daily, want) {
				t.Fatalf("Daily = %v, want %v（本地时区分桶 + 升序）", s.Daily, want)
			}
		})
	}
}

// TestMessageStatsTotalsDeliveryAndDevices 守重构的「搬运无损」：把 MessageStats
// 的实现体搬进 messageStats 后，总数 / 任务成功率 / 启用设备数三块仍然对。
func TestMessageStatsTotalsDeliveryAndDevices(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, userID := newTestNS(t, db)
	loc := tzFixtureLocations()[0] // 本用例不关心日界，挑一个固定时区即可

	day := localDay(loc, 9, 0, 0)
	insertEventAt(t, db, nsID, day.AddDate(0, 0, -1)) // 前一日
	insertEventAt(t, db, nsID, day)                   // 当日
	insertEventAt(t, db, nsID, day.Add(time.Hour))    // 当日

	// 5 个任务 → 终态 4 个（1 成功），1 个仍 pending（不计入分母）。
	cap := statsTestCapability(t, db, nsID)
	tasks := make([]*Task, 0, 5)
	for i := 0; i < 5; i++ {
		tasks = append(tasks, mkTask(nsID, "tr_"+NewEventID(), cap))
	}
	if err := db.CreateTasks(ctx, tasks); err != nil {
		t.Fatalf("create tasks: %v", err)
	}
	// CreateTasks 只能落在 pending（契约 §3.2），终态直接 SQL 改（tasks 无触发器）。
	for i, status := range []string{TaskSucceeded, TaskFailed, TaskCanceled, TaskAbandoned, TaskPending} {
		mustExec(t, db, `UPDATE tasks SET status=? WHERE id=?`, status, tasks[i].ID)
	}

	// 设备：启用 1 台、停用 1 台 → DeviceCount 只数启用的。
	mustExec(t, db,
		`INSERT INTO devices (id,user_id,name,platform,enabled,endpoint,p256dh,auth,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		NewDeviceID(), userID, "enabled", "ios", 1,
		"https://push.example/enabled", "p256dh-enabled", "auth-enabled", Now())
	mustExec(t, db,
		`INSERT INTO devices (id,user_id,name,platform,enabled,endpoint,p256dh,auth,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		NewDeviceID(), userID, "disabled", "ios", 0,
		"https://push.example/disabled", "p256dh-disabled", "auth-disabled", Now())

	s, err := db.messageStats(ctx, nsID, userID, 0, day)
	if err != nil {
		t.Fatalf("messageStats: %v", err)
	}
	if s.Total != 3 {
		t.Errorf("Total = %d, want 3", s.Total)
	}
	if s.Delivered != 1 {
		t.Errorf("Delivered = %d, want 1（只有 succeeded 计成功）", s.Delivered)
	}
	if s.DeliveryTotal != 4 {
		t.Errorf("DeliveryTotal = %d, want 4（pending 不进分母）", s.DeliveryTotal)
	}
	if s.DeviceCount != 1 {
		t.Errorf("DeviceCount = %d, want 1（只数 enabled=1）", s.DeviceCount)
	}
}

// TestMessageStatsPublicUsesRealClock 守公开入口确实把**真实时钟**交给 messageStats。
//
// 抓的是「MessageStats 里传了零值 time.Time」这类重构事故：零值的日界会退到
// 公元 1 年，连 1970 年那条老事件都会被数进 Today（Today 会数成 2 而不是 1）。
func TestMessageStatsPublicUsesRealClock(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, userID := newTestNS(t, db)

	// 不能用 time.Unix(0, 0)：InsertEvent 把 CreatedAt==0 当「未设置」，会重写成 Now()。
	insertEventAt(t, db, nsID, time.Unix(1, 0))     // 1970 年的老事件，绝不可能算「今日」
	insertEventAt(t, db, nsID, time.Unix(Now(), 0)) // 真实当前时刻

	s, err := db.MessageStats(ctx, nsID, userID, 0) // 公开入口：now 只可能是真实时钟
	if err != nil {
		t.Fatalf("MessageStats: %v", err)
	}
	if s.Total != 2 {
		t.Errorf("Total = %d, want 2", s.Total)
	}
	if s.Today != 1 {
		t.Errorf("Today = %d, want 1（零值 time.Time 会让日界退到公元 1 年，今日会数成 2）", s.Today)
	}
}

// TestMessageStatsRejectsEmptyNamespace 守命名空间守卫（契约 §6.2：缺 namespaceId
// 一律 ErrNamespaceRequired，绝不回退默认命名空间）。重构搬实现体时最容易丢它。
func TestMessageStatsRejectsEmptyNamespace(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	_, userID := newTestNS(t, db)

	for _, nsID := range []string{"", AllNamespaces} {
		if _, err := db.messageStats(ctx, nsID, userID, 0, localDay(tzFixtureLocations()[0], 0, 0, 30)); !errors.Is(err, ErrNamespaceRequired) {
			t.Fatalf("messageStats(namespaceID=%q) err = %v, want ErrNamespaceRequired", nsID, err)
		}
	}
	// 公开入口同样不许吞掉这个哨兵。
	if _, err := db.MessageStats(ctx, "", userID, 0); !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("MessageStats(namespaceID=\"\") err = %v, want ErrNamespaceRequired", err)
	}
}
