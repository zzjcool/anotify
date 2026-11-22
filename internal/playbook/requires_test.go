package playbook

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
)

func requireCausesOf(t *testing.T, err error) []Cause {
	t.Helper()
	if err == nil {
		t.Fatalf("期望 requires 类错误，实际没有错误")
	}
	var pbErr *Error
	if !errors.As(err, &pbErr) {
		t.Fatalf("期望 *playbook.Error，实际 %T（%v）", err, err)
	}
	return pbErr.Causes
}

func TestParseRequiresWriteTime(t *testing.T) {
	tooMany := &strings.Builder{}
	for i := 0; i <= MaxRequires; i++ {
		fmt.Fprintf(tooMany, "- capability: cap-%02d\n", i)
	}
	cases := []struct {
		name     string
		requires string
		code     string
		field    string
	}{
		{
			name:     "unknown_key",
			requires: "- capability: inventory\n  minimum: 2\n",
			code:     CodeInvalidPlaybook,
			field:    "spec.requires[0].minimum",
		},
		{
			name:     "not_array",
			requires: "inventory\n",
			code:     CodeInvalidPlaybook,
			field:    "spec.requires",
		},
		{
			name:     "entry_not_object",
			requires: "- inventory\n",
			code:     CodeInvalidPlaybook,
			field:    "spec.requires[0]",
		},
		{
			name:     "capability_name_invalid",
			requires: "- capability: Inventory_2\n",
			code:     CodeInvalidPlaybook,
			field:    "spec.requires[0].capability",
		},
		{
			name:     "capability_missing",
			requires: "- minVersion: 2\n",
			code:     CodeInvalidPlaybook,
			field:    "spec.requires[0].capability",
		},
		{
			name:     "min_version_zero",
			requires: "- capability: inventory\n  minVersion: 0\n",
			code:     CodeInvalidPlaybook,
			field:    "spec.requires[0].minVersion",
		},
		{
			name:     "duplicate",
			requires: "- capability: inventory\n- capability: inventory\n",
			code:     CodeInvalidPlaybook,
			field:    "spec.requires[1].capability",
		},
		{
			name:     "too_many",
			requires: tooMany.String(),
			code:     CodeInvalidPlaybook,
			field:    "spec.requires",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := newBundle().setRequires(tc.requires).pkgErr(t)
			assertCode(t, err, tc.code)
			causes := requireCausesOf(t, err)
			if len(causes) == 0 {
				t.Fatalf("错误应该带 causes: %v", err)
			}
			if causes[0].Field != tc.field {
				t.Fatalf("causes[0].Field = %q，期望 %q（全部 causes: %+v）",
					causes[0].Field, tc.field, causes)
			}
			if causes[0].Reason != tc.code {
				t.Fatalf("causes[0].Reason = %q，期望 %q", causes[0].Reason, tc.code)
			}
			if !strings.Contains(err.Error(), "spec.requires 不合法") {
				t.Fatalf("错误消息应该点名 spec.requires: %v", err)
			}
		})
	}
}

func TestParseRequiresCollectsAllProblems(t *testing.T) {
	err := newBundle().setRequires("- capability: Bad\n  minimum: 1\n- capability: ok-cap\n  minVersion: 0\n").pkgErr(t)
	assertCode(t, err, CodeInvalidPlaybook)
	causes := requireCausesOf(t, err)
	got := make([]string, 0, len(causes))
	for _, c := range causes {
		got = append(got, c.Field)
	}
	want := []string{
		"spec.requires[0].minimum",
		"spec.requires[0].capability",
		"spec.requires[1].minVersion",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("causes 的字段路径 = %v，期望 %v", got, want)
	}
}

func TestParseRequiresValid(t *testing.T) {
	pkg := newBundle().setRequires("- capability: inventory\n  minVersion: 2\n- capability: audit\n").pkg(t)
	want := []Requirement{{Capability: "inventory", MinVersion: 2}, {Capability: "audit"}}
	if !reflect.DeepEqual(pkg.Requires, want) {
		t.Fatalf("Package.Requires = %+v，期望 %+v", pkg.Requires, want)
	}
}

func TestParseRequiresOmitted(t *testing.T) {
	if pkg := newBundle().withStandard().pkg(t); pkg.Requires != nil {
		t.Fatalf("没有 spec.requires 的包不该解析出 Requires: %+v", pkg.Requires)
	}
	if pkg := newBundle().setRequires("[]\n").pkg(t); len(pkg.Requires) != 0 {
		t.Fatalf("spec.requires: [] 不该长出依赖: %+v", pkg.Requires)
	}
}

func TestResolveRequires(t *testing.T) {
	t.Run("missing_capability", func(t *testing.T) {
		_, err := resolveRequires([]Requirement{{Capability: "inventory"}}, nil, newFakeExisting())
		assertCode(t, err, CodeDependencyMissing)
		causes := requireCausesOf(t, err)
		if len(causes) != 1 {
			t.Fatalf("causes = %+v，期望 1 条", causes)
		}
		if causes[0].Field != "spec.requires[0].capability" || causes[0].Reason != CodeDependencyMissing {
			t.Fatalf("causes[0] = %+v", causes[0])
		}
		if !strings.Contains(causes[0].Message, "have: none") {
			t.Fatalf("不存在的能力必须写 have: none: %q", causes[0].Message)
		}
		if !strings.Contains(causes[0].Message, "need: 0") {
			t.Fatalf("省略 minVersion 的 need 是 0: %q", causes[0].Message)
		}
	})

	t.Run("version_too_low", func(t *testing.T) {
		ex := newFakeExisting().withCap("inventory", 2, "sha256:f2", "sha256:d2")
		_, err := resolveRequires([]Requirement{{Capability: "inventory", MinVersion: 3}}, nil, ex)
		assertCode(t, err, CodeDependencyMissing)
		causes := requireCausesOf(t, err)
		if len(causes) != 1 {
			t.Fatalf("causes = %+v，期望 1 条", causes)
		}
		for _, want := range []string{"need: 3", "have: 2"} {
			if !strings.Contains(causes[0].Message, want) {
				t.Fatalf("版本不足的 message 必须含 %q: %q", want, causes[0].Message)
			}
		}
	})

	t.Run("satisfied_uses_max_version_in_declaration_order", func(t *testing.T) {
		ex := newFakeExisting().
			withCap("inventory", 1, "sha256:f1", "sha256:d1").
			withCap("inventory", 2, "sha256:f2", "sha256:d2").
			withCap("audit", 1, "sha256:f3", "sha256:d3")
		deps, err := resolveRequires([]Requirement{
			{Capability: "audit"},
			{Capability: "inventory", MinVersion: 2},
		}, nil, ex)
		if err != nil {
			t.Fatalf("满足的依赖不该报错: %v", err)
		}
		want := []Dependency{{Capability: "audit", Version: 1}, {Capability: "inventory", Version: 2}}
		if !reflect.DeepEqual(deps, want) {
			t.Fatalf("dependencies = %+v，期望 %+v（顺序 = 声明顺序，版本 = 可见最高版本）", deps, want)
		}
	})

	t.Run("self_reference", func(t *testing.T) {
		installed := []*artifact.Artifact{{Metadata: artifact.Metadata{Name: "inventory"}}}
		ex := newFakeExisting().withCap("other", 1, "sha256:f1", "sha256:d1")
		_, err := resolveRequires([]Requirement{{Capability: "inventory"}}, installed, ex)
		assertCode(t, err, CodeInvalidPlaybook)
		causes := requireCausesOf(t, err)
		if len(causes) != 1 || causes[0].Field != "spec.requires[0].capability" {
			t.Fatalf("自引用的 causes = %+v", causes)
		}
		if !strings.Contains(causes[0].Message, "inventory") {
			t.Fatalf("自引用的 message 应该点名能力: %q", causes[0].Message)
		}
	})

	t.Run("self_reference_wins_over_missing", func(t *testing.T) {
		installed := []*artifact.Artifact{{Metadata: artifact.Metadata{Name: "audit"}}}
		_, err := resolveRequires([]Requirement{
			{Capability: "audit"},
			{Capability: "inventory", MinVersion: 9},
		}, installed, newFakeExisting())
		assertCode(t, err, CodeInvalidPlaybook)
	})
}
