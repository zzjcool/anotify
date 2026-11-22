package playbook

// #536：导出集合的纯函数（只由回执 created 决定 + 固定排序 + 名字去重）。

import (
	"reflect"
	"testing"
)

func TestReleaseObjectsOnlyCreatedRefs(t *testing.T) {
	receipts := []Receipt{
		{Created: Created{Capabilities: []string{"z-review@1", "a-fixer@2"}, Sources: []string{"m-push"}}},
		// 同名能力的后续版本（升级）与重复回执：按名字去重，仍然只有一份文档。
		{Created: Created{Capabilities: []string{"z-review@2"}, Sources: []string{"m-push", "b-push"}}},
		// skipped 不是 created：被复用的别人的行不进导出。
		{Skipped: Created{Capabilities: []string{"manual@1"}, Sources: []string{"manual-push"}}},
	}
	caps, srcs := ReleaseObjects(receipts)
	if want := []string{"a-fixer", "z-review"}; !reflect.DeepEqual(caps, want) {
		t.Fatalf("能力集合 = %v，期望 %v（按名字升序、去重、剥掉 @version）", caps, want)
	}
	if want := []string{"b-push", "m-push"}; !reflect.DeepEqual(srcs, want) {
		t.Fatalf("源集合 = %v，期望 %v（按名字升序）", srcs, want)
	}
}

func TestReleaseObjectsEmptyIsNotNil(t *testing.T) {
	caps, srcs := ReleaseObjects([]Receipt{{}})
	if caps == nil || srcs == nil {
		t.Fatalf("空集合必须是 [] 而不是 nil：caps=%v srcs=%v", caps, srcs)
	}
	if len(caps) != 0 || len(srcs) != 0 {
		t.Fatalf("空回执不该产生对象：%v / %v", caps, srcs)
	}
}
