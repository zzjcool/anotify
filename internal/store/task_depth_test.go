package store

import (
	"context"
	"testing"
)

func TestAncestorDepth(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "depth-cap", 1)

	var parent *string
	ids := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		task := mkTask(nsID, "tr_depth", c)
		task.ParentTaskID = parent
		if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
			t.Fatalf("建第 %d 层: %v", i+1, err)
		}
		if i == 0 {
			if n, err := db.CountTasksByTrace(ctx, nsID, "tr_depth"); err != nil || n != 1 {
				t.Fatalf("根任务数 = %d（err=%v），want 1", n, err)
			}
		}
		id := task.ID
		parent = &id
		ids = append(ids, id)
	}

	for want, id := range ids {
		got, err := db.AncestorDepth(ctx, nsID, id)
		if err != nil {
			t.Fatalf("depth(%s): %v", id, err)
		}
		if got != want+1 {
			t.Fatalf("depth(%s) = %d，want %d", id, got, want+1)
		}
	}
	if got, err := db.AncestorDepth(ctx, nsID, "tsk_missing"); err != nil || got != 0 {
		t.Fatalf("不存在的任务 = %d（err=%v），want 0", got, err)
	}
	if _, err := db.AncestorDepth(ctx, "", ids[0]); err == nil {
		t.Fatal("缺命名空间必须报错（读路径也遵守命名空间边界）")
	}
}
