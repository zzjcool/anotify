package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
)

// 本文件覆盖产物缓存（cache.go）的往返与安全边界。
//
// 缓存的语义只有一条：**磁盘上的内容就是唯一事实**。所以每个用例都直接从磁盘
// 上验收（目录布局、文件内容、权限、索引文本），而不是只看函数返回值——
// 「put 说成功了但文件没落下去」正是这类代码最容易出的错。

// ---------- 夹具 ----------

// testScriptArtifact 造一份最小可运行产物：一个 #!/bin/bash 的 run.sh。
//
// 直接构造结构体而不走 YAML：cache 层的输入就是 *artifact.Artifact，
// 绕开解析能精确构造出「artifact.Parse 本来会拒绝」的畸形输入（见路径安全用例）。
func testScriptArtifact(name string, version int, script string) *artifact.Artifact {
	return &artifact.Artifact{
		APIVersion: artifact.APIVersion,
		Kind:       artifact.KindCapability,
		Metadata:   artifact.Metadata{Name: name, Version: version},
		Spec: artifact.Spec{
			Outputs: []string{},
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": script},
			},
		},
	}
}

// mustDigest 算产物的内容 digest（失败即 Fatal）。
func mustDigest(t *testing.T, a *artifact.Artifact) string {
	t.Helper()
	d, err := artifact.Digest(a)
	if err != nil {
		t.Fatalf("artifact.Digest: %v", err)
	}
	return d
}

// mustArtifactYAML 把产物序列化成落盘用的 YAML（失败即 Fatal）。
func mustArtifactYAML(t *testing.T, a *artifact.Artifact) []byte {
	t.Helper()
	b, err := artifact.MarshalYAML(a)
	if err != nil {
		t.Fatalf("artifact.MarshalYAML: %v", err)
	}
	return b
}

// ---------- put / get 往返 ----------

// TestCachePutGetRoundTrip：put 之后磁盘布局合规、get 能原样取回、applied 里出现一条。
func TestCachePutGetRoundTrip(t *testing.T) {
	c := newCache(t.TempDir())
	a := testScriptArtifact("demo.prep", 3, "#!/bin/bash\necho prep\n")
	digest := mustDigest(t, a)

	if err := c.put(a, digest); err != nil {
		t.Fatalf("put: %v", err)
	}

	// 磁盘布局（契约 §5）：artifacts/<digest>/capability.yaml + files/<rel-path>
	artifactDir := filepath.Join(c.root, "artifacts", digest)
	yamlPath := filepath.Join(artifactDir, "capability.yaml")
	if _, err := os.Stat(yamlPath); err != nil {
		t.Fatalf("产物原文没落盘 %s: %v", yamlPath, err)
	}
	filePath := filepath.Join(artifactDir, "files", "run.sh")
	st, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("展开的文件没落盘 %s: %v", filePath, err)
	}
	if perm := st.Mode().Perm(); perm != 0o755 {
		t.Errorf("展开的文件权限 = %o, want 755（直接 exec，靠 shebang）", perm)
	}
	body, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("读展开的文件: %v", err)
	}
	if string(body) != "#!/bin/bash\necho prep\n" {
		t.Errorf("展开的文件内容 = %q, want 原样", body)
	}
	// 索引：单行文本 = digest
	idx, err := os.ReadFile(c.indexPath("demo.prep", 3))
	if err != nil {
		t.Fatalf("索引没落盘: %v", err)
	}
	if strings.TrimSpace(string(idx)) != digest {
		t.Errorf("索引内容 = %q, want %q", idx, digest)
	}

	// get 往返：内容一致、digest 一致。
	got, gotDigest, ok := c.get("demo.prep", 3)
	if !ok {
		t.Fatal("get(demo.prep, 3) = false, want true")
	}
	if gotDigest != digest {
		t.Errorf("get digest = %q, want %q", gotDigest, digest)
	}
	if !reflect.DeepEqual(got, a) {
		t.Errorf("get 回来的产物 = %#v, want %#v", got, a)
	}

	// 按 digest 取（assign 帧带 digest 时走这条路）。
	byDigest, d2, ok := c.getByDigest(digest)
	if !ok || d2 != digest || !reflect.DeepEqual(byDigest, a) {
		t.Fatalf("getByDigest = (%#v, %q, %v), want 与 get 一致", byDigest, d2, ok)
	}

	// applied：一条，且 digest 是内容算出来的那个。
	want := []appliedDecl{{Capability: "demo.prep", Version: 3, Digest: digest}}
	if got := c.applied(); !reflect.DeepEqual(got, want) {
		t.Errorf("applied() = %#v, want %#v", got, want)
	}

	// 取不到的组合：版本不同 / 名字不同 / 非法 digest。
	if _, _, ok := c.get("demo.prep", 2); ok {
		t.Error("get(demo.prep, 2) = true, want false（版本不同）")
	}
	if _, _, ok := c.get("nope", 3); ok {
		t.Error("get(nope, 3) = true, want false")
	}
	if _, _, ok := c.getByDigest("../etc/passwd"); ok {
		t.Error("getByDigest(../etc/passwd) = true, want false（非法 digest）")
	}
}

// TestCachePutIdempotent：同一 digest 重复 put 是幂等的（内容寻址）。
//
// 这不仅「不该报错」：它必须**不重写**目录（否则每次重部署都会留下一堆 .tmp 或者
// 把正在被任务读取的文件换掉），索引也必须仍然指向同一个 digest。
func TestCachePutIdempotent(t *testing.T) {
	c := newCache(t.TempDir())
	a := testScriptArtifact("demo.prep", 1, "#!/bin/bash\necho v1\n")
	digest := mustDigest(t, a)

	if err := c.put(a, digest); err != nil {
		t.Fatalf("第一次 put: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(c.root, "artifacts"))
	if err != nil {
		t.Fatalf("读 artifacts 目录: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("artifacts 目录里有 %d 项, want 1: %v", len(entries), entries)
	}

	if err := c.put(a, digest); err != nil {
		t.Fatalf("重复 put: %v", err)
	}
	entries, err = os.ReadDir(filepath.Join(c.root, "artifacts"))
	if err != nil {
		t.Fatalf("读 artifacts 目录: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("重复 put 之后 artifacts 目录里有 %d 项, want 1（幂等）: %v", len(entries), entries)
	}
	if got := len(c.applied()); got != 1 {
		t.Errorf("applied() 有 %d 条, want 1", got)
	}

	// 不同内容 → 不同 digest → 两条独立的缓存项（同一 name@version 的索引指向后者）。
	b := testScriptArtifact("demo.prep", 1, "#!/bin/bash\necho v2\n")
	digest2 := mustDigest(t, b)
	if digest2 == digest {
		t.Fatal("不同内容的 digest 相同：内容寻址失效")
	}
	if err := c.put(b, digest2); err != nil {
		t.Fatalf("put v2: %v", err)
	}
	_, gotDigest, ok := c.get("demo.prep", 1)
	if !ok || gotDigest != digest2 {
		t.Fatalf("get(demo.prep, 1) = (%q, %v), want v2 的 digest %q", gotDigest, ok, digest2)
	}
	// 旧产物目录保留（内容寻址下可能被别的 name@version 复用，GC 留后续）。
	if _, err := os.Stat(filepath.Join(c.root, "artifacts", digest)); err != nil {
		t.Errorf("旧产物目录不该被删: %v", err)
	}
}

// TestCachePutDigestMismatch：调用方给的 digest 与内容不符时必须拒绝。
//
// 目录名是内容的哈希，这是整个审计链的根；允许「名不副实」就等于关掉了它。
func TestCachePutDigestMismatch(t *testing.T) {
	c := newCache(t.TempDir())
	a := testScriptArtifact("demo.prep", 1, "#!/bin/bash\necho hi\n")
	wrong := "sha256:" + strings.Repeat("0", 64)

	if err := c.put(a, wrong); err == nil {
		t.Fatal("put(digest 与内容不符) 返回 nil, want error")
	} else if !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("error = %q, want 含 digest mismatch", err)
	}
	if _, _, ok := c.get("demo.prep", 1); ok {
		t.Error("拒绝之后缓存里不该有这条索引")
	}
	// 非法 digest（路径拼接风险）也必须拒。
	if err := c.put(a, "../../etc"); err == nil {
		t.Fatal("put(非法 digest) 返回 nil, want error")
	}
}

// TestCachePathTraversalRejected：产物里的 ../ 路径必须落盘前就被拒。
//
// artifact.Parse 会先拦一道，但这里**绕过 Parse 直接构造**畸形产物：cache 是
// 落盘前的最后一道闸，不能假设上游一定校验过（standalone 模式、以后的导入路径
// 都会直接喂结构体）。
func TestCachePathTraversalRejected(t *testing.T) {
	for _, rel := range []string{"../escape.sh", "a/../../escape.sh", "/abs.sh", "a//b.sh", "./a.sh", `a\b.sh`} {
		t.Run(rel, func(t *testing.T) {
			c := newCache(t.TempDir())
			a := testScriptArtifact("demo.prep", 1, "#!/bin/bash\n")
			a.Spec.Run.Files = map[string]string{rel: "#!/bin/bash\necho pwn\n"}
			digest := mustDigest(t, a)

			if err := c.put(a, digest); err == nil {
				t.Fatalf("put(文件路径 %q) 返回 nil, want error", rel)
			}
			// 逃出去的文件绝不能出现（parent of root 就是那次 put 的攻击目标）。
			escaped := filepath.Join(filepath.Dir(c.root), "escape.sh")
			if _, err := os.Stat(escaped); err == nil {
				t.Fatalf("文件逃出了缓存根目录：%s 被写出来了", escaped)
			}
		})
	}

	// 入口文件本身用 ../ 也要被拒（entrypoint 也拼路径）。
	c := newCache(t.TempDir())
	a := testScriptArtifact("demo.prep", 1, "#!/bin/bash\n")
	a.Spec.Run.Entrypoint = "../run.sh"
	if err := c.put(a, mustDigest(t, a)); err == nil {
		t.Fatal("put(entrypoint 含 ..) 返回 nil, want error")
	}
}

// TestCacheRemove：remove 摘索引（幂等），产物目录保留，applied 随之消失。
func TestCacheRemove(t *testing.T) {
	c := newCache(t.TempDir())
	a := testScriptArtifact("demo.prep", 1, "#!/bin/bash\necho hi\n")
	digest := mustDigest(t, a)
	if err := c.put(a, digest); err != nil {
		t.Fatalf("put: %v", err)
	}

	if err := c.remove("demo.prep", 1); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, _, ok := c.get("demo.prep", 1); ok {
		t.Error("remove 之后 get 仍然成功，索引没被摘掉")
	}
	if got := c.applied(); len(got) != 0 {
		t.Errorf("remove 之后 applied() = %#v, want 空", got)
	}
	// 产物目录保留（可能还有在跑的任务在读它 / 被别的索引项复用）。
	if _, err := os.Stat(filepath.Join(c.root, "artifacts", digest)); err != nil {
		t.Errorf("remove 不该删产物目录: %v", err)
	}
	// 幂等：再 remove 一次仍然成功（中枢重推不该变成 write_failed）。
	if err := c.remove("demo.prep", 1); err != nil {
		t.Errorf("重复 remove = %v, want nil（幂等）", err)
	}
	// 非法键要拒（名字直接进路径）。
	if err := c.remove("../etc", 1); err == nil {
		t.Error("remove(非法名字) = nil, want error")
	}
	if err := c.remove("demo.prep", 0); err == nil {
		t.Error("remove(version 0) = nil, want error")
	}
}

// TestCacheAppliedReportsContentDigest：用户手改 capability.yaml 之后，
// applied 报的必须是**内容算出来的** digest（本地变体的判据，契约 §7）。
func TestCacheAppliedReportsContentDigest(t *testing.T) {
	c := newCache(t.TempDir())
	a := testScriptArtifact("demo.prep", 1, "#!/bin/bash\necho managed\n")
	digest := mustDigest(t, a)
	if err := c.put(a, digest); err != nil {
		t.Fatalf("put: %v", err)
	}

	// 模拟「用户 ssh 上去手改了产物」：目录名（旧 digest）不动，内容变了。
	edited := testScriptArtifact("demo.prep", 1, "#!/bin/bash\necho LOCALLY EDITED\n")
	editedDigest := mustDigest(t, edited)
	if editedDigest == digest {
		t.Fatal("编辑后 digest 没变：内容寻址失效")
	}
	if err := os.WriteFile(filepath.Join(c.root, "artifacts", digest, "capability.yaml"),
		mustArtifactYAML(t, edited), 0o644); err != nil {
		t.Fatalf("改写 capability.yaml: %v", err)
	}

	got := c.applied()
	if len(got) != 1 {
		t.Fatalf("applied() = %#v, want 1 条", got)
	}
	if got[0].Digest != editedDigest {
		t.Errorf("applied[0].Digest = %q, want 内容 digest %q（index 里还是 %q）",
			got[0].Digest, editedDigest, digest)
	}
	// 中枢靠这个差值判「本地变体、永不覆盖」。
	if got[0].Digest == digest {
		t.Error("applied 把 index 里的旧 digest 报了出去：中枢将以为这份产物仍是托管态")
	}
}

// TestCacheAppliedOrderAndSkipsGarbage：applied 排序稳定、坏索引项被跳过。
func TestCacheAppliedOrderAndSkipsGarbage(t *testing.T) {
	c := newCache(t.TempDir())

	b := testScriptArtifact("demo.b", 1, "#!/bin/bash\necho b\n")
	a2 := testScriptArtifact("demo.a", 2, "#!/bin/bash\necho a2\n")
	a1 := testScriptArtifact("demo.a", 1, "#!/bin/bash\necho a1\n")
	for _, art := range []*artifact.Artifact{b, a2, a1} {
		if err := c.put(art, mustDigest(t, art)); err != nil {
			t.Fatalf("put %s@%d: %v", art.Metadata.Name, art.Metadata.Version, err)
		}
	}
	// 垃圾索引项：点名不存在 / 内容坏 / 文件名不合法 → 全部跳过（节点确实跑不了它们）。
	if err := os.WriteFile(filepath.Join(c.indexDir(), "ghost@1"),
		[]byte("sha256:"+strings.Repeat("c", 64)+"\n"), 0o644); err != nil {
		t.Fatalf("写垃圾索引: %v", err)
	}
	if err := os.WriteFile(filepath.Join(c.indexDir(), "not-a-key"), []byte("x"), 0o644); err != nil {
		t.Fatalf("写垃圾索引: %v", err)
	}

	got := c.applied()
	wantNames := []string{"demo.a@1", "demo.a@2", "demo.b@1"}
	if len(got) != len(wantNames) {
		t.Fatalf("applied() = %#v, want %v", got, wantNames)
	}
	for i, want := range wantNames {
		if key := got[i].Capability + "@" + strconv.Itoa(got[i].Version); key != want {
			t.Errorf("applied[%d] = %s, want %s（排序应稳定）", i, key, want)
		}
	}
}

// TestCacheAppliedEmptyIsNotNil：没部署过任何东西时 applied 返回 []（不是 nil）。
func TestCacheAppliedEmptyIsNotNil(t *testing.T) {
	c := newCache(filepath.Join(t.TempDir(), "does-not-exist-yet"))
	got := c.applied()
	if got == nil {
		t.Fatal("applied() = nil, want 空切片（空列表纪律）")
	}
	if len(got) != 0 {
		t.Fatalf("applied() = %#v, want 空", got)
	}
}

// TestSafeJoin：路径闸门的表格测试（与 artifact 的校验规则同口径）。
func TestSafeJoin(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	ok := map[string]string{
		"run.sh":         filepath.Join(root, "run.sh"),
		"a/b/c.sh":       filepath.Join(root, "a", "b", "c.sh"),
		"a.b/c-d_1.sh":   filepath.Join(root, "a.b", "c-d_1.sh"),
		"deep/er/x.txt ": filepath.Join(root, "deep", "er", "x.txt "),
	}
	for rel, want := range ok {
		got, err := safeJoin(root, rel)
		if err != nil {
			t.Errorf("safeJoin(%q) 意外失败: %v", rel, err)
			continue
		}
		if got != want {
			t.Errorf("safeJoin(%q) = %q, want %q", rel, got, want)
		}
	}
	for _, rel := range []string{"", ".", "..", "../x", "a/../x", "a//b", "/x", `a\b`, "a/b/", "a/./b", "a\x00b"} {
		if got, err := safeJoin(root, rel); err == nil {
			t.Errorf("safeJoin(%q) = %q, want error", rel, got)
		}
	}
}
