package decisions

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller 失败：定位不到测试文件")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func TestDecisionIDsAreWellFormed(t *testing.T) {
	const want = 6
	if len(Implementations) != want {
		t.Fatalf("决策索引应有 %d 条，实际 %d 条", want, len(Implementations))
	}
	for id := range Implementations {
		if !strings.HasPrefix(id, "V4-D") || len(id) != len("V4-D1") {
			t.Fatalf("决策号形状不是 V4-D<n>：%q", id)
		}
	}
}

func TestDecisionImplementationsExist(t *testing.T) {
	root := repoRoot(t)
	for id, rel := range Implementations {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Fatalf("%s 的执行者不存在：%s（%v）", id, rel, err)
		}
	}
}
