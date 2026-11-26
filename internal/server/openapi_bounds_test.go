package server

// openapi_bounds_test.go 是 #644 的**静态边界门禁**：把 `api/resource.yaml` 里与 handler
// 判据耦合的边界值（`version` 的下界、poll `timeoutMs` 的区间）钉成断言。
//
// 为什么必须有这道门禁（`TestOpenAPIParity` 管不到）：parity 只对账「schema 的 properties ↔
// Go struct 的 json tag」以及形状/可达性，**从不施加 `minimum`**——样本里的 `version` 带
// `omitempty`，永远是 zero value、不进 JSON，所以把 `minimum: 0` 改回 `1` 时 parity 仍然全绿；
// poll `timeoutMs` 的上界更是只写在 description 文本里，退回 60000 不会有任何测试变红。
// #644 第 1 条验收句写的就是「通过 openapi_parity_test.go」，而那一半当时是空过。
//
// 两个用途：
//  1. schema 的下界必须与 handler 的判据同向（0 = live 是合法输入，负数才 400）；
//  2. 文档里的区间必须由代码常量拼出来，不写死人肉复制的数字。
//
// 所有测试函数名以 `TestOpenAPIParity` 开头：`make contract` 用
// `go test ./internal/server -run TestOpenAPIParity` 跑这道门禁，改名会静默变绿。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/sourcespec"
)

// TestOpenAPIParityRequestBounds 钉住「动作 body 的 version 下界 = 0」与「poll timeoutMs 区间」。
func TestOpenAPIParityRequestBounds(t *testing.T) {
	docs, problems := loadSpecDocs()
	if len(problems) != 0 {
		t.Fatalf("spec 载入有问题：%v", problems)
	}

	// ① 两个动作 body 的 `version`：0 与省略同义（live），所以 schema 下界必须是 0。
	//    回退成 `minimum: 1` = 严格按 schema 生成的客户端本地就拒掉服务端明确接受的请求，
	//    也就是 #644 第 1 条（试跑）与第 3 条（deploy）那对矛盾。
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

	// ② poll `timeoutMs` 的文档区间必须等于代码常量（`sourcespec.PollTimeoutMinMs/MaxMs`），
	//    并且不许写「夹到」：越界是 400 拒绝，不是静默夹取（#644 第 2 条）。
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

// childSchema 取 schema.properties[key] 这个子 schema（缺失即 Fatal）。
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

// integerValue 把 YAML 解析出来的数字归一成 int（gopkg.in/yaml.v3 给 int，保守再认几种）。
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
