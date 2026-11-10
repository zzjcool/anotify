package execenv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/cmd/anode/internal/hostenv"
	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

func testArtifact(t *testing.T, mutate func(*artifact.Artifact)) *artifact.Artifact {
	t.Helper()
	a := &artifact.Artifact{
		APIVersion: "anotify/v1",
		Kind:       "Capability",
		Metadata:   artifact.Metadata{Name: "echo", Version: 1},
		Spec: artifact.Spec{
			Params: json.RawMessage(`{}`),
			Next:   []string{"fixer"},
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\ntrue\n"},
			},
		},
	}
	if mutate != nil {
		mutate(a)
	}
	if err := artifact.Validate(a); err != nil {
		t.Fatalf("artifact.Validate: %v", err)
	}
	return a
}

func testBaseline(t *testing.T) hostenv.Baseline {
	t.Helper()
	dir := t.TempDir()
	return hostenv.CaptureWith([]string{"TEST_SECRET"}, func(key string) (string, bool) {
		switch key {
		case "PATH":
			return "/usr/bin:/bin", true
		case "HOME":
			return dir, true
		case "TEST_SECRET":
			return "top-secret-value", true
		}
		return "", false
	})
}

func TestResolveWhitelistContractVarsAndSources(t *testing.T) {
	dir := t.TempDir()
	a := testArtifact(t, func(a *artifact.Artifact) {
		a.Spec.Requires.Values = []string{"vars.REPO"}
		a.Spec.Run.Env = map[string]artifact.EnvValue{
			"REPO":  {FieldPath: "vars.REPO"},
			"NAMED": {FieldPath: "metadata.name"},
		}
	})
	in := Input{
		Facts:         protocol.NodeFacts{HomeDir: dir},
		Artifact:      a,
		LocalVars:     map[string]string{"REPO": "local-repo"},
		MachineVars:   map[string]string{"REPO": "node-repo", "UNUSED": "x"},
		NamespaceVars: map[string]string{"REPO": "ns-repo"},
		Passthrough:   []string{"TEST_SECRET"},
		Baseline:      testBaseline(t),
		Params:        json.RawMessage(`{}`),
		Endpoint:      "http://127.0.0.1:9",
		TaskToken:     "tok",
		TaskID:        "t1",
		ArtifactDir:   dir,
	}
	res, err := Resolve(in)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	env := envMap(res.Env())
	if env["REPO"] != "local-repo" {
		t.Fatalf("vars 优先级 local > node > ns 不对：%q", env["REPO"])
	}
	if env["HOME"] != dir {
		t.Fatalf("HOME 必须是基线里的 homeDir：%q", env["HOME"])
	}
	if env[protocol.EnvEmitEndpoint] != "http://127.0.0.1:9" ||
		env[protocol.EnvTaskToken] != "tok" ||
		env[protocol.EnvTaskID] != "t1" ||
		env[protocol.EnvArtifactDir] != dir {
		t.Fatalf("契约变量没注入：%v", env)
	}
	if env["TEST_SECRET"] != "top-secret-value" {
		t.Fatal("envPassthrough 白名单值必须注入")
	}
	if _, ok := env["UNUSED"]; ok {
		t.Fatal("没被引用的机器 vars 不能泄漏进环境")
	}
	// 诊断只有名字+来源，没有值。
	for _, d := range res.Diags {
		if strings.Contains(d.Name, "top-secret") || strings.Contains(d.Source, "top-secret") {
			t.Fatalf("诊断不能含值：%+v", d)
		}
	}
}

func TestResolveRejectsArtifactDirOverride(t *testing.T) {
	a := testArtifact(t, nil)
	// 绕过 artifact.Validate（它本来就拒绝 ANOTIFY_ 前缀）：验证 Resolve 自己的防线。
	a.Spec.Run.Env = map[string]artifact.EnvValue{
		protocol.EnvArtifactDir: {Value: "/tmp/evil"},
	}
	_, err := Resolve(Input{Artifact: a, Baseline: testBaseline(t), ArtifactDir: "/tmp/real"})
	if err == nil {
		t.Fatal("run.env 不能覆盖 ANOTIFY_ARTIFACT_DIR")
	}
	if ReasonOf(err) != protocol.ReasonMissingRequirement {
		t.Fatalf("覆盖契约变量必须是确定性 requires 类失败，实际 %q", ReasonOf(err))
	}
}

func TestResolveRejectsPassthroughOfContractVars(t *testing.T) {
	a := testArtifact(t, nil)
	_, err := Resolve(Input{
		Artifact:    a,
		Passthrough: []string{protocol.EnvTaskToken},
		Baseline:    testBaseline(t),
		TaskToken:   "tok",
	})
	if err == nil {
		t.Fatal("envPassthrough 不能继承 ANOTIFY_ 契约变量")
	}
}

func TestExpandWorkDirFieldPaths(t *testing.T) {
	a := testArtifact(t, func(a *artifact.Artifact) {
		a.Spec.Requires.Values = []string{"vars.ROOT"}
		a.Spec.Run.WorkDir = "$(vars.ROOT)"
		a.Spec.Run.Env = map[string]artifact.EnvValue{"ROOT": {FieldPath: "vars.ROOT"}}
	})
	res, err := Resolve(Input{
		Artifact:    a,
		MachineVars: map[string]string{"ROOT": "/srv/jobs"},
		Baseline:    testBaseline(t),
		Endpoint:    "http://127.0.0.1:9",
		TaskToken:   "tok",
		TaskID:      "t1",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got, err := ExpandWorkDir(a.Spec.Run.WorkDir, res)
	if err != nil {
		t.Fatalf("ExpandWorkDir: %v", err)
	}
	if got != "/srv/jobs" {
		t.Fatalf("workDir 展开 = %q", got)
	}
}

func TestValidateDirRejectsUnsafePaths(t *testing.T) {
	root := t.TempDir()
	systemRoot := filepath.Join(root, "work")
	resultsRoot := filepath.Join(root, "results")
	if err := os.MkdirAll(systemRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(resultsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	userDir := filepath.Join(root, "user")
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateDir(userDir, systemRoot, resultsRoot); err != nil {
		t.Fatalf("合法用户目录被拒：%v", err)
	}
	missing := filepath.Join(root, "missing")
	if _, err := ValidateDir(missing, systemRoot, resultsRoot); err == nil {
		t.Fatal("不存在的 workDir 必须被拒绝（不自动创建）")
	}
	if _, err := ValidateDir("relative/dir", systemRoot, resultsRoot); err == nil {
		t.Fatal("相对 workDir 必须被拒绝")
	}
	// 系统 GC 根的祖先/自身：会被 GC 误删 → 拒绝。
	for _, p := range []string{root, systemRoot, resultsRoot, filepath.Dir(systemRoot)} {
		if _, err := ValidateDir(p, systemRoot, resultsRoot); err == nil {
			t.Fatalf("与系统根重叠的 workDir 必须被拒绝：%s", p)
		}
	}
	// 软链逃逸。
	escape := filepath.Join(root, "escape")
	if err := os.Symlink("/", escape); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateDir(escape, systemRoot, resultsRoot); err == nil {
		t.Fatal("解软链后逃逸的 workDir 必须被拒绝")
	}
}

func TestMaterializeRejectsSymlinkAndEscape(t *testing.T) {
	root := t.TempDir()
	// 目标已存在且是软链：不能写穿。
	link := filepath.Join(root, "run.sh")
	if err := os.Symlink("/etc/passwd", link); err != nil {
		t.Fatal(err)
	}
	err := Materialize(root, map[string]string{"run.sh": "#!/bin/sh\ntrue\n"}, "run.sh")
	if err == nil || ReasonOf(err) != protocol.ReasonFilesEscape {
		t.Fatalf("软链目标必须 files_escape，实际 %v", err)
	}
	if _, err := os.Stat("/etc/passwd"); err != nil {
		t.Fatalf("/etc/passwd 不能被动过：%v", err)
	}
	// 路径里有 .. 或反斜杠：拒绝。
	for _, bad := range []string{"../escape.sh", "a\\b.sh", "/abs.sh"} {
		if err := Materialize(t.TempDir(), map[string]string{bad: "x"}, bad); err == nil {
			t.Fatalf("非法文件路径必须被拒绝：%q", bad)
		}
	}
	// 合法：写入 0600 + 入口 0700。
	dir := t.TempDir()
	if err := Materialize(dir, map[string]string{"run.sh": "#!/bin/sh\ntrue\n"}, "run.sh"); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "run.sh"))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("入口权限必须 0700：%v %v", info, err)
	}
}

func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}
