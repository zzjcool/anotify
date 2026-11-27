package artifact

import (
	"strings"
	"testing"
)

func parseWithLifecycle(t *testing.T, lifecycle string) (*Artifact, error) {
	t.Helper()
	doc := strings.Replace(minimalYAML, "spec:\n", "spec:\n  next: [issue-record]\n"+lifecycle, 1)
	return Parse([]byte(doc))
}

func TestLifecycleIdleTimeoutDefaultsAndBounds(t *testing.T) {
	a, err := parseWithLifecycle(t, "")
	if err != nil {
		t.Fatalf("省略 idleTimeoutSec 应当合法：%v", err)
	}
	if a.Spec.Lifecycle.IdleTimeoutSec != nil {
		t.Fatalf("省略时不该被补一个默认值进来：%v", *a.Spec.Lifecycle.IdleTimeoutSec)
	}
	if got := a.Spec.Lifecycle.EffectiveIdleTimeoutSec(600); got != 600 {
		t.Fatalf("省略 → 节点默认 600，实际 %d", got)
	}

	for _, doc := range []string{
		"  lifecycle: {idleTimeoutSec: 0}\n",
		"  lifecycle: {idleTimeoutSec: 86400}\n",
	} {
		if _, err := parseWithLifecycle(t, doc); err != nil {
			t.Fatalf("边界值应当合法（%q）：%v", doc, err)
		}
	}
	a, err = parseWithLifecycle(t, "  lifecycle: {idleTimeoutSec: 0}\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Spec.Lifecycle.EffectiveIdleTimeoutSec(600); got != 0 {
		t.Fatalf("显式 0 必须是关闭，实际 %d", got)
	}

	for _, doc := range []string{
		"  lifecycle: {idleTimeoutSec: -1}\n",
		"  lifecycle: {idleTimeoutSec: 86401}\n",
	} {
		_, err := parseWithLifecycle(t, doc)
		if err == nil {
			t.Fatalf("%q 应当被拒", doc)
		}
		if got := codeOf(t, err); got != CodeBadRequest {
			t.Fatalf("%q 期望 %q，实际 %q", doc, CodeBadRequest, got)
		}
	}
}

func TestLifecycleOnFailureRoundTrip(t *testing.T) {
	a, err := parseWithLifecycle(t, `  lifecycle:
    timeoutSec: 3600
    onFailure:
      to: issue-record
      params: {outcome: timeout, findings: 因超时终止}
`)
	if err != nil {
		t.Fatalf("合法的 onFailure 应当被接受：%v", err)
	}
	if a.Spec.Lifecycle.OnFailure == nil || a.Spec.Lifecycle.OnFailure.To != "issue-record" {
		t.Fatalf("onFailure 没有被解析出来：%+v", a.Spec.Lifecycle.OnFailure)
	}
	if got := a.Spec.Lifecycle.OnFailure.Params["outcome"]; got != "timeout" {
		t.Fatalf("onFailure.params.outcome 期望 timeout，实际 %v", got)
	}

	withFallback, err := Digest(withVersion(t, a))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := parseWithLifecycle(t, "  lifecycle:\n    timeoutSec: 3600\n")
	if err != nil {
		t.Fatal(err)
	}
	withoutFallback, err := Digest(withVersion(t, plain))
	if err != nil {
		t.Fatal(err)
	}
	if withFallback == withoutFallback {
		t.Fatal("onFailure 必须参与 digest：它是执行策略的一部分")
	}
}

// withVersion 给产物钉一个版本（算 digest 的前置条件）。
func withVersion(t *testing.T, a *Artifact) *Artifact {
	t.Helper()
	out, err := Normalize(a)
	if err != nil {
		t.Fatalf("归一化: %v", err)
	}
	out.Metadata.Version = 1
	return out
}

func TestLifecycleOnFailureRejectedShapes(t *testing.T) {
	cases := map[string]string{
		"to 为空":       "  lifecycle:\n    onFailure: {to: \"\"}\n",
		"to 不在 next":  "  lifecycle:\n    onFailure: {to: issue-unrelated}\n",
		"to 写模板":      "  lifecycle:\n    onFailure: {to: \"$(vars.X)\"}\n",
		"未知子键":        "  lifecycle:\n    onFailure: {to: issue-record, params: {}, x: 1}\n",
		"params 写模板":  "  lifecycle:\n    onFailure: {to: issue-record, params: {findings: \"$(payload.x)\"}}\n",
		"params 不是对象": "  lifecycle:\n    onFailure: {to: issue-record, params: 3}\n",
	}
	for name, field := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseWithLifecycle(t, field); err == nil {
				t.Fatal("应当被拒")
			}
		})
	}
}
