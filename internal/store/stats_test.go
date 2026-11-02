package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// 运行期统计维护（#265）的数据层验收。
//
// 这组用例守两件事：
//  1. 常态下 **不** 无条件 ANALYZE（成本远大于收益，大表上会挡住写）
//  2. 只执行 SQLite 自己给的 `ANALYZE ...` 建议，别的字符串一律不碰
//     （"把数据库返回的字符串当 SQL 执行"必须锁在最小面上）

// fakeStatsQueryer 让用例能指定 SQLite 的"建议"。
type fakeStatsQueryer struct {
	suggestions []string
	executed    []string
	execErr     error
}

func (f *fakeStatsQueryer) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	// maintainStatsOn 只查一条：PRAGMA optimize
	rows := &sql.Rows{} // 占位，真正的实现对 *sql.Rows 不可构造
	_ = rows
	return nil, errFakeNotConstructible
}

func (f *fakeStatsQueryer) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	f.executed = append(f.executed, q)
	return nil, f.execErr
}

var errFakeNotConstructible = sql.ErrConnDone

// TestMaintainStatsFreshDBIsQuiet 新库上没有漂移 → 不该执行任何 ANALYZE。
//
// 这是「不要无条件 ANALYZE」的护栏：真跑一遍 MaintainStats，断言 statements 为空。
func TestMaintainStatsFreshDBIsQuiet(t *testing.T) {
	db := newTestDB(t)
	ran, err := db.MaintainStats(context.Background())
	if err != nil {
		t.Fatalf("maintain stats: %v", err)
	}
	if len(ran) != 0 {
		t.Fatalf("新库不该需要 ANALYZE，实际执行了 %v", ran)
	}
}

// TestMaintainStatsOnlyRunsAnalyzeSuggestions 是对「建议校验」的守卫。
//
// 实现上 `maintainStatsOn` 接受一个最小接口，但 *sql.Rows 无法在测试里构造，
// 所以这里直接测**过滤函数**本身（把建议 → 是否放行 的逻辑抽出来验）。
func TestMaintainStatsSuggestionFilter(t *testing.T) {
	ok := []string{`ANALYZE "tasks"`, `analyze tasks`, `  ANALYZE sqlite_master `}
	for _, s := range ok {
		if !isAnalyzeSuggestion(s) {
			t.Errorf("%q 应该被放行", s)
		}
	}
	bad := []string{"", "DROP TABLE tasks", "DELETE FROM tasks", "PRAGMA ignore_check_constraints=1"}
	for _, s := range bad {
		if isAnalyzeSuggestion(s) {
			t.Errorf("%q 绝不能被当 SQL 执行", s)
		}
	}
}

// TestMaintainStatsDoesNotTouchData 统计维护不能改任何业务数据（它只该动 sqlite_stat*）。
func TestMaintainStatsDoesNotTouchData(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	u := &User{ID: NewUserID(), Username: "stats-user", CreatedAt: Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := db.MaintainStats(ctx); err != nil {
		t.Fatalf("maintain stats: %v", err)
	}
	got, err := db.GetUserByUsername("stats-user")
	if err != nil || got.ID != u.ID {
		t.Fatalf("统计维护动了业务数据：err=%v got=%+v", err, got)
	}
	_ = strings.TrimSpace("") // 保持 strings 依赖显式（过滤实现用它）
}
