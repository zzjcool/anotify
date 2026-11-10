package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// goldenMachineHash 独立于实现算一遍期望值：喂**手写的** canonical JSON，
// 避免「实现自己算一遍再和自己比」的假绿。
func goldenMachineHash(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// TestMachineConfigHashGolden 钉死 canonical 文档形状与 golden hash。
//
// canonical JSON 只有两个顶层键 `vars` / `limits`；两级 map 的键按字节序排序；
// 空 map/nil 写成 `{}` 而不是 null；total 未设置归一成缺省 1。
func TestMachineConfigHashGolden(t *testing.T) {
	cases := []struct {
		name      string
		vars      map[string]string
		limits    Limits
		canonical string
	}{
		{
			name:      "nil vars + 零值 limits 归一",
			vars:      nil,
			limits:    Limits{},
			canonical: `{"vars":{},"limits":{"total":1,"perCapability":{}}}`,
		},
		{
			name:      "空 map 与 nil 等价",
			vars:      map[string]string{},
			limits:    Limits{Total: 1, PerCapability: map[string]int{}},
			canonical: `{"vars":{},"limits":{"total":1,"perCapability":{}}}`,
		},
		{
			name: "vars 与 perCapability 键排序",
			vars: map[string]string{"B": "2", "A": "1"},
			limits: Limits{
				Total:         8,
				PerCapability: map[string]int{"review": 2, "*": 1},
			},
			canonical: `{"vars":{"A":"1","B":"2"},"limits":{"total":8,"perCapability":{"*":1,"review":2}}}`,
		},
		{
			name:      "HTML 字符按 encoding/json 默认转义（两侧同字节）",
			vars:      map[string]string{"A": "a<b&c"},
			limits:    Limits{Total: 2},
			canonical: `{"vars":{"A":"a\u003cb\u0026c"},"limits":{"total":2,"perCapability":{}}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MachineConfigHash(tc.vars, tc.limits)
			if err != nil {
				t.Fatalf("MachineConfigHash: %v", err)
			}
			want := goldenMachineHash(tc.canonical)
			if got != want {
				t.Fatalf("hash = %s，期望 %s（canonical %s）", got, want, tc.canonical)
			}
			// 同时钉 canonical 字节本身：hash 对了但形状漂了同样是断约。
			raw, err := machineConfigCanonicalJSON(tc.vars, tc.limits)
			if err != nil {
				t.Fatalf("machineConfigCanonicalJSON: %v", err)
			}
			if string(raw) != tc.canonical {
				t.Fatalf("canonical JSON = %s，期望 %s", raw, tc.canonical)
			}
		})
	}
}

// TestMachineConfigHashDefaults 显式缺省值 == 未设置（total:0 归一成 1）。
func TestMachineConfigHashDefaults(t *testing.T) {
	unset, err := MachineConfigHash(nil, Limits{})
	if err != nil {
		t.Fatalf("MachineConfigHash: %v", err)
	}
	explicit, err := MachineConfigHash(nil, Limits{Total: DefaultLimitsTotal})
	if err != nil {
		t.Fatalf("MachineConfigHash: %v", err)
	}
	if unset != explicit {
		t.Fatalf("total 未设置与显式缺省值必须同 hash：%s != %s", unset, explicit)
	}
	if DefaultLimitsTotal != 1 {
		t.Fatalf("DefaultLimitsTotal = %d，期望 1", DefaultLimitsTotal)
	}
}

// TestMachineConfigHashCanonicalKeys 只含 vars/limits 两个顶层键，绝不夹带
// revision / ns 默认 / token / 凭证 / envPassthrough（那些不进机器配置内容）。
func TestMachineConfigHashCanonicalKeys(t *testing.T) {
	raw, err := machineConfigCanonicalJSON(
		map[string]string{"SOURCE_ROOT": "/srv/src"},
		Limits{Total: 4, PerCapability: map[string]int{"review": 2}},
	)
	if err != nil {
		t.Fatalf("machineConfigCanonicalJSON: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("canonical JSON 不可解析：%v", err)
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) != 2 || keys[0] != "limits" || keys[1] != "vars" {
		t.Fatalf("canonical 顶层键 = %v，期望只有 [limits vars]", keys)
	}
	var limits map[string]json.RawMessage
	if err := json.Unmarshal(top["limits"], &limits); err != nil {
		t.Fatalf("limits 不可解析：%v", err)
	}
	limitKeys := make([]string, 0, len(limits))
	for k := range limits {
		limitKeys = append(limitKeys, k)
	}
	sort.Strings(limitKeys)
	if len(limitKeys) != 2 || limitKeys[0] != "perCapability" || limitKeys[1] != "total" {
		t.Fatalf("limits 键 = %v，期望 [perCapability total]", limitKeys)
	}
	for _, banned := range []string{"revision", "namespace", "secret", "token", "envPassthrough", "credentials"} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("canonical JSON 夹带了 %q：%s", banned, raw)
		}
	}
}

// TestMachineConfigHashOrderInsensitive map 迭代顺序不得影响结果。
func TestMachineConfigHashOrderInsensitive(t *testing.T) {
	vars := map[string]string{"Z": "26", "A": "1", "M": "13", "B": "2"}
	limits := Limits{Total: 8, PerCapability: map[string]int{"review": 2, "fix": 1, "*": 3}}
	want, err := MachineConfigHash(vars, Limits{Total: 8, PerCapability: map[string]int{"review": 2, "fix": 1, "*": 3}})
	if err != nil {
		t.Fatalf("MachineConfigHash: %v", err)
	}
	for i := 0; i < 32; i++ {
		// 每次重建 map（Go 的 map 迭代顺序随机，重建后编码路径必须仍稳定）。
		v := map[string]string{}
		for k, val := range vars {
			v[k] = val
		}
		l := Limits{Total: limits.Total, PerCapability: map[string]int{}}
		for k, val := range limits.PerCapability {
			l.PerCapability[k] = val
		}
		got, err := MachineConfigHash(v, l)
		if err != nil {
			t.Fatalf("MachineConfigHash: %v", err)
		}
		if got != want {
			t.Fatalf("第 %d 次 hash 漂了：%s != %s", i, got, want)
		}
	}
}

// TestMachineConfigHashContentSensitive 内容变了 hash 必须变（否则 drift 判不出来）。
func TestMachineConfigHashContentSensitive(t *testing.T) {
	base, err := MachineConfigHash(map[string]string{"A": "1"}, Limits{Total: 4})
	if err != nil {
		t.Fatalf("MachineConfigHash: %v", err)
	}
	variants := []struct {
		name   string
		vars   map[string]string
		limits Limits
	}{
		{"变量值变", map[string]string{"A": "2"}, Limits{Total: 4}},
		{"变量名变", map[string]string{"B": "1"}, Limits{Total: 4}},
		{"多一个变量", map[string]string{"A": "1", "B": "1"}, Limits{Total: 4}},
		{"total 变", map[string]string{"A": "1"}, Limits{Total: 5}},
		{"perCapability 变", map[string]string{"A": "1"}, Limits{Total: 4, PerCapability: map[string]int{"review": 2}}},
	}
	for _, v := range variants {
		got, err := MachineConfigHash(v.vars, v.limits)
		if err != nil {
			t.Fatalf("%s: MachineConfigHash: %v", v.name, err)
		}
		if got == base {
			t.Fatalf("%s 之后 hash 没变（%s）", v.name, got)
		}
	}
}

// TestMachineConfigHashNegativeTotal 负 total 不是「未设置」，不静默归一。
func TestMachineConfigHashNegativeTotal(t *testing.T) {
	if _, err := MachineConfigHash(nil, Limits{Total: -1}); err == nil {
		t.Fatal("负 total 必须报错")
	}
}

// TestMachineConfigHashFormat 输出形如 sha256:<64 hex>（与 artifact digest 同前缀）。
func TestMachineConfigHashFormat(t *testing.T) {
	got, err := MachineConfigHash(map[string]string{"A": "1"}, Limits{Total: 2})
	if err != nil {
		t.Fatalf("MachineConfigHash: %v", err)
	}
	if !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("hash 前缀不对：%s", got)
	}
	hexPart := strings.TrimPrefix(got, "sha256:")
	if len(hexPart) != 64 {
		t.Fatalf("hash hex 长度 = %d，期望 64（%s）", len(hexPart), got)
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		t.Fatalf("hash 不是 hex：%v", err)
	}
	if len(got) != len("sha256:")+64 {
		t.Fatalf("hash 总长 = %d", len(got))
	}
}
