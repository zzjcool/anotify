package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/zzjcool/anotify/internal/artifact"
)

// 本文件是节点的**产物缓存层**：把中枢部署下来的产物落盘、按 name@version 或
// digest 取回、维护一份索引。
//
// 目录布局（契约 §5，可用 --data-dir 改根）：
//
//	<root>/artifacts/<digest>/capability.yaml     ← 产物原文（YAML）
//	<root>/artifacts/<digest>/files/<rel-path>    ← 展开的文件（全部 0755）
//	<root>/index/<name>@<version>                 ← 单行文本：digest
//
// # 为什么产物目录用 digest 命名
//
// digest 是内容的哈希（artifact.Digest），所以目录名本身就是一次审计：
// 同一份产物在任何机器上必然同名、内容必然相同；用户手改了 capability.yaml，
// 目录名不变但重新算出的 digest 会变——那正是「本地变体」的判据（契约 §7）。
// 也因此 put 是**天然幂等**的：同一个 digest 重复部署只是复用已有目录。
//
// # 为什么不删产物目录
//
// remove 只摘索引（name@version → digest）。产物目录故意留着，两个原因：
//  1. 内容寻址下同一 digest 可能被别的 name@version 复用，删一个就会伤到另一个；
//  2. 失败保留的任务工作目录、正在跑的任务都可能还在读它。
//
// GC（按「不在索引里 + 不年轻于 N 天」回收）留后续，这里只保证不泄漏索引。
//
// # 并发
//
// put / remove / get 只从读循环（deploy 帧）与任务 goroutine 调用；读是只读、
// 写只有读循环一条线，所以这里不加锁。真要做并行部署再加（接口不变）。

const (
	// artifactsDirName / indexDirName / workSubdirName 是数据目录下的三个子目录。
	artifactsDirName = "artifacts"
	indexDirName     = "index"
	workSubdirName   = "work"
	// artifactFileName 是产物原文在 <digest>/ 下的固定文件名（契约 §5）。
	artifactFileName = "capability.yaml"
	// filesDirName 是展开文件的固定子目录（契约 §5）。
	filesDirName = "files"
)

var (
	// digestPattern 是内容寻址标识的形状：sha256: + 64 位小写十六进制。
	// 校验它不只是「好看」：digest 直接拼进路径，不校验就等于把路径拼接交给对端。
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// indexNamePattern 与 internal/artifact 的能力名规则一致（同一条正则的含义）。
	indexNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
)

// forkLock 串行化「写文件（此刻持有写 fd）」与「fork/exec 子进程」两件事。
//
// 为什么必须有它（实测踩到）：fork 出来的子进程在 exec 之前会继承父进程**全部** fd
// ——包括另一个 goroutine 正在写的那个文件。就算那个文件随后被 rename 成 run.sh，
// 子进程手里的写 fd 仍指向同一个 inode，于是「另一个进程正开着它写」→ 那个 run.sh
// 的 exec 直接失败 ETXTBSY（"text file busy"）。8 个任务并发各写各的 run.sh 时，
// 两个并发任务就能稳定复现（见 runner_test.go 的工作目录隔离用例）。
//
// 之所以临时文件 + rename **不够**：rename 只换名字不换 inode，问题依然存在。
// 唯一简单可靠的做法是让「写」与「fork」互斥。锁只覆盖一次小文件写入 / 一次 fork，
// 都是微秒级，不会成为吞吐瓶颈。
var forkLock sync.Mutex

// cache 是节点侧的产物缓存（root 通常是 ~/.anotify 或 --data-dir）。
type cache struct {
	root string
}

// newCache 建一个指向 root 的缓存。目录不在这里创建：第一次 put 才建，
// 一个只跑 standalone / 只连中枢但还没部署过东西的节点不该在磁盘上留空壳。
func newCache(root string) *cache { return &cache{root: root} }

func (c *cache) artifactsDir() string { return filepath.Join(c.root, artifactsDirName) }
func (c *cache) indexDir() string     { return filepath.Join(c.root, indexDirName) }
func (c *cache) artifactDir(d string) string {
	return filepath.Join(c.artifactsDir(), d)
}

// workRoot 是任务工作目录的父目录：每次任务在它下面建 <task_id>/（见 runner.go）。
func (c *cache) workRoot() string { return filepath.Join(c.root, workSubdirName) }

// indexPath 是 <name>@<version> 索引项的文件路径。
func (c *cache) indexPath(name string, version int) string {
	return filepath.Join(c.indexDir(), fmt.Sprintf("%s@%d", name, version))
}

// put 把一份产物落进缓存，并更新索引（原子写 + 内容寻址幂等）。
//
// 顺序是「先落产物、后落索引」：索引项是「这份产物可用」的**承诺**，所以它必须
// 最后写。反过来的话，一次断电就会留下一个指向不存在目录的索引 —— 节点会声称
// 自己能跑一个其实跑不了的能力（正是本 epic 要消灭的「两个真相源」）。
func (c *cache) put(a *artifact.Artifact, digest string) error {
	if a == nil {
		return errors.New("cache.put: nil artifact")
	}
	if err := validateDigest(digest); err != nil {
		return err
	}
	// 再算一遍：目录名必须是**内容**的哈希，不能是调用方嘴里的字符串。
	// 少了这一步，一个错 digest 就会把两份不同的产物永久钉在同一个目录里。
	got, err := artifact.Digest(a)
	if err != nil {
		return fmt.Errorf("digest artifact: %w", err)
	}
	if got != digest {
		return fmt.Errorf("digest mismatch: 传入 %s，内容算出 %s", digest, got)
	}

	// 入口路径也要过一遍闸门：它在执行时会被拼成绝对路径直接 exec，
	// 一条 "../.." 就能把「跑产物入口」变成「跑机器上任意文件」。
	if err := checkRelPath(a.Spec.Run.Entrypoint); err != nil {
		return fmt.Errorf("run.entrypoint %q: %w", a.Spec.Run.Entrypoint, err)
	}

	final := c.artifactDir(digest)
	if st, err := os.Stat(final); err == nil && st.IsDir() {
		// 内容寻址天然幂等：同一 digest 的目录内容必然相同，直接复用。
		return c.writeIndex(a.Metadata.Name, a.Metadata.Version, digest)
	}

	if err := os.MkdirAll(c.artifactsDir(), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", c.artifactsDir(), err)
	}
	// 先写到同级临时目录，fsync 之后 rename：中途断电最多留下一个 .tmp-* 残骸，
	// 永远不会留下「半个产物」被当成可用（rename 在同一文件系统内是原子的）。
	tmp, err := os.MkdirTemp(c.artifactsDir(), ".tmp-"+strings.ReplaceAll(digest, ":", "-")+"-")
	if err != nil {
		return fmt.Errorf("mkdir temp artifact dir: %w", err)
	}
	// 成功 rename 之后 tmp 已经不存在，RemoveAll 是幂等的（失败路径靠它收尸）。
	defer os.RemoveAll(tmp)

	yamlBytes, err := artifact.MarshalYAML(a)
	if err != nil {
		return fmt.Errorf("marshal artifact yaml: %w", err)
	}
	if err := writeFileSync(filepath.Join(tmp, artifactFileName), yamlBytes, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", artifactFileName, err)
	}
	if err := expandArtifactFiles(tmp, a); err != nil {
		return err
	}
	if err := syncDir(tmp); err != nil {
		return fmt.Errorf("sync %s: %w", tmp, err)
	}

	if err := os.Rename(tmp, final); err != nil {
		// 目标已存在（并发 put / 上一次失败留下的半成品）：内容寻址下等价，复用即可。
		if st, statErr := os.Stat(final); statErr == nil && st.IsDir() {
			return c.writeIndex(a.Metadata.Name, a.Metadata.Version, digest)
		}
		return fmt.Errorf("rename %s -> %s: %w", tmp, final, err)
	}
	if err := syncDir(c.artifactsDir()); err != nil {
		return fmt.Errorf("sync %s: %w", c.artifactsDir(), err)
	}
	return c.writeIndex(a.Metadata.Name, a.Metadata.Version, digest)
}

// expandArtifactFiles 把产物的文件按相对路径展开进 <artifactDir>/files/。
//
// 全部 0755：执行方式是直接 exec 入口文件（靠 shebang），被入口调用的辅助脚本
// 也常常需要可执行位；产物里的文件内容本身就是能力作者写的代码，给执行位不扩大
// 任何权限面（它本来就会以节点的身份跑）。
func expandArtifactFiles(artifactDir string, a *artifact.Artifact) error {
	root := filepath.Join(artifactDir, filesDirName)
	// files/ 先建出来：没有它连"零个文件"的产物（理论上不该有）也不会 sync 失败。
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", root, err)
	}
	names := make([]string, 0, len(a.Spec.Run.Files))
	for name := range a.Spec.Run.Files {
		names = append(names, name)
	}
	sort.Strings(names) // 落盘顺序确定，便于排障（内容不受顺序影响）

	for _, rel := range names {
		dst, err := safeJoin(root, rel)
		if err != nil {
			return fmt.Errorf("文件 %q: %w", rel, err)
		}
		// 子目录按需创建（a/b/c.sh 这种嵌套路径要先把 a 建出来）。
		if dir := filepath.Dir(dst); dir != root {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("mkdir for %q: %w", rel, err)
			}
		}
		if err := writeFileSync(dst, []byte(a.Spec.Run.Files[rel]), 0o755); err != nil {
			return fmt.Errorf("写文件 %q: %w", rel, err)
		}
	}
	if err := syncDir(root); err != nil {
		return fmt.Errorf("sync %s: %w", root, err)
	}
	return nil
}

// writeIndex 原子地写一条 <name>@<version> → digest 索引项。
//
// 索引也要 tmp + rename：索引项被读到一半（空文件 / 半个 digest）会让 get 失败，
// 而节点在 hello 里报「我没有它」→ 中枢重推 → 用户看到莫名其妙的反复部署。
func (c *cache) writeIndex(name string, version int, digest string) error {
	if err := validateIndexKey(name, version); err != nil {
		return err
	}
	if err := validateDigest(digest); err != nil {
		return err
	}
	if err := os.MkdirAll(c.indexDir(), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", c.indexDir(), err)
	}

	if err := writeFileSync(c.indexPath(name, version), []byte(digest+"\n"), 0o644); err != nil {
		return fmt.Errorf("写索引 %s@%d: %w", name, version, err)
	}
	if err := syncDir(c.indexDir()); err != nil {
		return fmt.Errorf("sync %s: %w", c.indexDir(), err)
	}
	return nil
}

// get 按 name@version 取产物：读索引 → digest → 读 capability.yaml → Decode。
//
// 返回的 digest 是**内容算出来的**（见 getByDigest），所以拿它和中枢的 desired
// digest 比就能发现本地变体。
func (c *cache) get(name string, version int) (*artifact.Artifact, string, bool) {
	digest, ok := c.readIndex(name, version)
	if !ok {
		return nil, "", false
	}
	return c.getByDigest(digest)
}

// getByDigest 按 digest 取产物（assign 帧带了 digest 时走这条路）。
//
// 第二个返回值是**重新算出来**的 digest，不是传进来的那个：用户手改过
// capability.yaml / files 之后，目录名（旧 digest）与内容（新 digest）会对不上，
// 而中枢正是靠这个差值判定「本地变体、永不覆盖」（契约 §7）。
func (c *cache) getByDigest(digest string) (*artifact.Artifact, string, bool) {
	if err := validateDigest(digest); err != nil {
		return nil, "", false
	}
	data, err := os.ReadFile(filepath.Join(c.artifactDir(digest), artifactFileName))
	if err != nil {
		return nil, "", false
	}
	a, got, err := artifact.Decode(data)
	if err != nil {
		// 缓存里躺着一份坏产物：当作没有（中枢会重推）。损坏的原因留给运维查。
		return nil, "", false
	}
	return a, got, true
}

// readIndex 读一条索引项，返回它指向的 digest。
func (c *cache) readIndex(name string, version int) (string, bool) {
	if err := validateIndexKey(name, version); err != nil {
		return "", false
	}
	data, err := os.ReadFile(c.indexPath(name, version))
	if err != nil {
		return "", false
	}
	digest := strings.TrimSpace(string(data))
	if err := validateDigest(digest); err != nil {
		return "", false
	}
	return digest, true
}

// remove 摘掉一条索引项（产物目录保留，见文件头注释）。
//
// 幂等：索引项不存在就是「本来就不在缓存里」，按成功处理——remove 是从中枢来的
// 最终一致指令，重复投递不该报错（否则一次重连就会打出一条 write_failed）。
func (c *cache) remove(name string, version int) error {
	if err := validateIndexKey(name, version); err != nil {
		return err
	}
	if err := os.Remove(c.indexPath(name, version)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove index %s@%d: %w", name, version, err)
	}
	if err := syncDir(c.indexDir()); err != nil {
		return fmt.Errorf("sync %s: %w", c.indexDir(), err)
	}
	return nil
}

// applied 扫索引目录，返回本机**已缓存**的全部产物（hello.applied 的内容）。
//
// 排序让同一个缓存状态每次发出的 hello 逐字节一致（便于排障 + 测试可断言）。
// 所有坏项都跳过：解码失败的索引项节点确实跑不了，如实不报，中枢会重推。
func (c *cache) applied() []appliedDecl {
	entries, err := os.ReadDir(c.indexDir())
	if err != nil {
		// 还没部署过任何东西（或索引目录没建）：空列表，不是 nil（空列表纪律）。
		return []appliedDecl{}
	}
	out := make([]appliedDecl, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name, version, ok := parseIndexName(e.Name())
		if !ok {
			continue
		}
		a, digest, ok := c.get(name, version)
		if !ok {
			continue
		}
		out = append(out, appliedDecl{
			Capability: a.Metadata.Name,
			Version:    a.Metadata.Version,
			Digest:     digest,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Capability != out[j].Capability {
			return out[i].Capability < out[j].Capability
		}
		return out[i].Version < out[j].Version
	})
	return out
}

// parseIndexName 把索引文件名拆回 (name, version)。
func parseIndexName(file string) (string, int, bool) {
	at := strings.LastIndexByte(file, '@')
	if at <= 0 || at == len(file)-1 {
		return "", 0, false
	}
	name := file[:at]
	version, err := strconv.Atoi(file[at+1:])
	if err != nil {
		return "", 0, false
	}
	if err := validateIndexKey(name, version); err != nil {
		return "", 0, false
	}
	return name, version, true
}

// validateIndexKey 校验索引键：名字规则与 internal/artifact 一致（同一条正则的含义），
// 版本 >= 1。名字直接进路径，所以必须校验——不校验就等于把路径拼接交给对端。
func validateIndexKey(name string, version int) error {
	if !indexNamePattern.MatchString(name) {
		return fmt.Errorf("非法能力名 %q（只能小写字母/数字/点/下划线/短横线，1..63，数字或字母开头）", name)
	}
	if version < 1 {
		return fmt.Errorf("非法版本 %d（必须 >= 1）", version)
	}
	return nil
}

// validateDigest 校验 digest 形状（sha256: + 64 位小写十六进制）。
func validateDigest(digest string) error {
	if !digestPattern.MatchString(digest) {
		return fmt.Errorf("非法 digest %q（应为 sha256:<64 位小写十六进制>）", digest)
	}
	return nil
}

// checkRelPath 校验产物里的相对路径形状：空 / 绝对 / .. / 空段 / 反斜杠 / NUL 全拒。
//
// 规则与 internal/artifact 的校验一致（那边是同一条规则的另一份实现：它校验的是
// 「从网上收到的产物」，这里是「即将落盘 / 即将执行」的最后一道）。
func checkRelPath(rel string) error {
	if rel == "" {
		return errors.New("路径为空")
	}
	if strings.ContainsRune(rel, 0) {
		return errors.New("路径含 NUL 字节")
	}
	if strings.Contains(rel, `\`) {
		return errors.New("路径含反斜杠（只接受 / 分隔的 POSIX 相对路径）")
	}
	if path.IsAbs(rel) {
		return fmt.Errorf("路径 %q 必须是相对路径", rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("路径 %q 含非法路径段 %q", rel, seg)
		}
	}
	return nil
}

// safeJoin 把产物里的相对路径拼到 root 之下，并确认结果**仍在 root 内**。
//
// 规则与 internal/artifact 的校验一致（绝对路径 / .. / 空段 / 反斜杠 / NUL 全拒），
// 但这里是落盘前的最后一道闸：artifact 包校验的是「从网上收到的产物」，而 cache
// 也会被 standalone 模式（读本地 YAML）和以后可能的导入路径调用——路径安全不该
// 只依赖上游做过校验。filepath.Clean 之后再比前缀，是防 `a/../../b` 这类穿法。
func safeJoin(root, rel string) (string, error) {
	if err := checkRelPath(rel); err != nil {
		return "", err
	}

	rootClean := filepath.Clean(root)
	joined := filepath.Clean(filepath.Join(rootClean, filepath.FromSlash(rel)))
	if joined != rootClean && !strings.HasPrefix(joined, rootClean+string(filepath.Separator)) {
		return "", fmt.Errorf("路径 %q 逃出了根目录（%q）", rel, rootClean)
	}
	return joined, nil
}

// writeFileSync 原子地写一个文件：先写同目录临时文件 → fsync → rename 到目标。
//
// 为什么不用「O_CREATE|O_TRUNC 直接写目标」：两个理由，第二个是真踩过的。
//
//  1. 原子性：目标路径上永远不会出现「写了一半的内容」（断电 / 进程被杀）。
//  2. **避免 ETXTBSY**：如果我们正在往 X 写（持有 X 的写 fd），而另一个 goroutine
//     此刻 fork 出一个准备 exec Y 的子进程，那个子进程在 exec 之前会继承这个写 fd
//     ——于是「另一个进程正把 Y 打开着写」，Y 的 exec 就会失败（text file busy）。
//     8 并发任务各写各的 run.sh 时这是必然会发生的一次竞态。改成「写 .tmp-* →
//     rename」之后，写 fd 永远落在 .tmp-* 上，而 .tmp-* 从不被 exec。
func writeFileSync(path string, data []byte, perm os.FileMode) error {
	// 写期间持有 forkLock：不能让别的 goroutine 在这个窗口里 fork（见 forkLock 注释）。
	forkLock.Lock()
	defer forkLock.Unlock()

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-write-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// rename 成功之后 tmpName 已不存在；失败路径靠它收尸。
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// CreateTemp 建的是 0600，显式 chmod 才能得到产物要求的权限
	// （展开的文件要 0755——入口靠 shebang 直接 exec）。
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	// fsync 之后才允许 rename：否则断电后可能 rename 出一个内容为空的文件。
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// syncDir 把目录项也 fsync（rename 的原子性只保证「名字」，
// 内容能不能活过断电要靠文件和它的父目录都 sync 过）。
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
