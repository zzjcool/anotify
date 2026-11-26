package server

// openapi_bounds_test.go 是 #644 的静态边界对账：`TestOpenAPIParity` 只对账 properties ↔ json tag、
// **从不施加 `minimum`**（样本里的 version 带 omitempty、永不进 JSON），所以把下界改回去它仍然全绿；
// 函数名必须以 `TestOpenAPIParity` 开头，否则 `make contract` 的 -run 选不中它。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/sourcespec"
)

func TestOpenAPIParityRequestBounds(t *testing.T) {
	docs, problems := loadSpecDocs()
	if len(problems) != 0 {
		t.Fatalf("spec 载入有问题：%v", problems)
	}

	// ① 下界必须是 0：`minimum: 1` = 按 schema 生成的客户端本地就拒掉服务端明确接受的请求。
	for _, schemaName := range []string{"CapabilityTestRequest", "CapabilityDeployRequest"} {
		schema, ok := componentSchema(docs, "resource.yaml", schemaName)
		if !ok {
			t.Fatalf("resource.yaml 里找不到 schema %s", schemaName)
		}
		version := childSchema(t, schema, schemaName, "version")
		if got, ok := integerValue(version["minimum"]); !ok || got != 0 {
			t.Fatalf("%s.version.minimum = %#v，want 0（0 与省略同义 = live；只有负数才是 400）",
				schemaName, version["minimum"])
		}
		desc, _ := version["description"].(string)
		if desc == "" {
			t.Fatalf("%s.version 缺 description：必须写明「省略 / 0 = live」", schemaName)
		}
		if !strings.Contains(desc, "0 = ") {
			t.Fatalf("%s.version.description = %q，必须写明「0 = live/最新」口径", schemaName, desc)
		}
		if !strings.Contains(desc, "负数") {
			t.Fatalf("%s.version.description = %q，必须写明负数 → 400", schemaName, desc)
		}
	}

	// ② 区间必须由代码常量拼出（不许人肉复制），且不许写「夹到」：越界是 400 拒绝（#644 第 2 条）。
	raw, err := os.ReadFile(filepath.Join(specDir, "resource.yaml"))
	if err != nil {
		t.Fatalf("读不了 %s/resource.yaml：%v", specDir, err)
	}
	var pollLine string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "timeoutMs:") && strings.Contains(line, "默认") {
			pollLine = line
			break
		}
	}
	if pollLine == "" {
		t.Fatal("resource.yaml 里找不到 poll `timeoutMs` 的文档行（含「默认」注释的那行）")
	}
	wantBounds := fmt.Sprintf("[%d, %d]", sourcespec.PollTimeoutMinMs, sourcespec.PollTimeoutMaxMs)
	if !strings.Contains(pollLine, wantBounds) {
		t.Fatalf("poll timeoutMs 文档区间与代码常量不一致：want %s\n实际那行：%s", wantBounds, pollLine)
	}
	if strings.Contains(pollLine, "夹到") {
		t.Fatalf("文档不得写「夹到」：normalizePollConfig 对越界值是 400 拒绝（value_invalid），不是静默夹取\n实际那行：%s", pollLine)
	}
}

func childSchema(t *testing.T, schema map[string]any, schemaName, key string) map[string]any {
	t.Helper()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema %s 没有 properties", schemaName)
	}
	child, ok := props[key].(map[string]any)
	if !ok {
		t.Fatalf("schema %s 没有 properties.%s", schemaName, key)
	}
	return child
}

func integerValue(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if float64(int(n)) == n {
			return int(n), true
		}
	case uint64:
		return int(n), true
	}
	return 0, false
}
