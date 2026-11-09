package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// 本文件是「能力 + 下游边（+ 产物）一次提交」的原子写入入口。
//
// 为什么需要它：控制台新建能力时填的「出口 + 下游」以前是 1 + N 个请求
// （先 POST /v1/capabilities，再逐条 POST /v1/edges——能力边那条路径现在已封，
// 存量能力的下游走 PUT /v1/capabilities/{name}/downstreams 整体替换）。中间任何一条失败，
// 库里就留下「能力在、边缺」的半成品——append-only 的能力表撤不回来，
// 客户端只能在内存里记一份「待重试清单」（刷新就丢）。
//
// 目标语义：**要么全成，要么全不成**。所以能力行、产物行、边行写在**同一个事务**里，
// 任何一步失败整体回滚，并把「是第几条 link 出的问题」带回给调用方。

// LinkSpec 是一条待建的 link（能力定义时一起提交）。
//
// 字段口径与 Edge 的对应关系：
//
//	FromPort  → Edge.FromPort（必须 ∈ 这个新能力的 outputs，封闭集合 I2）
//	ToRef     → Edge.ToRef（`name` 跟随 latest / `name@version` 钉死；空 = 解析失败 → 400）
//	Literals  → Edge.Literals（常量覆盖，键与类型受目标能力 paramsSchema 约束）
//
// 左端恒为**裸名字**（= 这个刚建的能力名，跟随 latest）：它一定是最新版本，
// 用户在这里表达的语义就是「我这次建的这个能力，从这些出口接到那些下游」。
// 想钉死版本是 Edge 的语义，不是「定义能力时顺手建边」的语义——真要钉死，
// 建完之后用 PUT /v1/capabilities/{name}/downstreams 把那条引用换成 `name@version`。
type LinkSpec struct {
	FromPort string          `json:"fromPort"`
	ToRef    string          `json:"toRef"` // name 或 name@version；空 = 解析失败
	Literals json.RawMessage `json:"literals,omitempty"`
	// Priority 是这条候选的优先级（契约 §1：小的优先，同级轮盘）。
	// 单建时随 links[] 一起提交，批量走 BatchLinkSpec.Priority，两条路径落到同一列。
	Priority int `json:"priority,omitempty"`
}

// ArtifactBuilder 在事务内、能力行**归一化之后**被调用，返回这次创建对应的产物值。
//
// 为什么是回调而不是直接传 *Artifact：content-hash 幂等会把「同内容但版本号不同」
// 的请求折叠到**已有的那一行**（append-only，不变量 I8）——归一化之后版本号可能变。
// 产物里嵌着 name@version（digest 也吃它），用请求里的版本号建产物，就会得到一份
// 元数据与能力定义不一致、digest 也算错的产物。所以产物必须在拿到权威行之后再构建。
//
// P2 起产物**不落库**（artifacts 表已删）：回调的唯一作用是「用另一条路径算一遍，
// 在事务内与权威行对账」（见 verifyBuiltArtifact）；对不上就整体失败。
//
// 回调返回 nil 表示「这次没有产物」（等价于不传回调）。
// 构建失败会作为 *ArtifactBuildError 返回：整个事务回滚，能力一行都不会留下。
type ArtifactBuilder func(c *CapabilityVersion) (*Artifact, error)

// ArtifactBuildError 表示事务内的产物构建失败。
//
// 单独一个类型是为了让 handler 把「run 写错了」翻成 400 invalid_artifact，
// 而不是笼统的 500——它与 store 的其它错误（唯一键冲突、命名空间错）不是一回事。
type ArtifactBuildError struct {
	Err error
}

func (e *ArtifactBuildError) Error() string {
	if e == nil || e.Err == nil {
		return "build artifact failed"
	}
	return "build artifact: " + e.Err.Error()
}

func (e *ArtifactBuildError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// verifyBuiltArtifact 用 builder 的输出对账**权威能力行**（P2 起产物是派生值，不落库）。
//
// 为什么还要这个回调：构建发生在事务内、归一化之后，所以它算出的 digest 是
// 「这一行正文的 digest」的唯一独立算法。P1 期间它的产物被写进 artifacts 表；
// P2 删表之后它唯一的用处就是这个对账——两条路径算出不同 digest 的症状是
// 「响应里的 digest 与部署时钉住的不同」，只在某类请求上复现，事后极难查。
func verifyBuiltArtifact(c *CapabilityVersion, art *Artifact) error {
	if art == nil {
		return errors.New("artifact builder returned nil")
	}
	if art.NamespaceID == "" {
		art.NamespaceID = c.NamespaceID
	}
	if art.CapabilityID == "" {
		art.CapabilityID = c.ID
	}
	if art.NamespaceID != c.NamespaceID || art.CapabilityID != c.ID {
		return fmt.Errorf("artifact builder returned an artifact for %s/%s, want %s/%s",
			art.NamespaceID, art.CapabilityID, c.NamespaceID, c.ID)
	}
	if want := c.Digest; art.Digest != want {
		return fmt.Errorf("artifact digest %q != capability digest %q（构建路径与读取路径分叉了）",
			art.Digest, want)
	}
	return nil
}

// CapabilityWrite 是一次原子写入的回执：权威能力行 + 建成的边 + 产物 + 影响面。
//
// Capability 是**归一后**的行（幂等折叠时它的 version/id 来自已有那一行），
// 调用方必须用它（而不是请求里的那份）去算响应与产物。
type CapabilityWrite struct {
	Capability *CapabilityVersion
	// Edges 按 links 的顺序返回（每条都已归一：引用、literals 都是落库的形态）。
	Edges []*Edge
	// Artifact 是这次创建对应的产物值（由 builder 算出来，P2 起不落库）；
	// 没传 ArtifactBuilder 时为 nil。
	Artifact *Artifact
	// Impact 是本次「成为 latest」的影响面（空影响面也是空数组，不是 nil）。
	Impact *UpgradeImpact
}

// CreateCapabilityWithLinks 在一个事务里建能力 + 它的下游边，并返回升级影响面。
//
// 语义与 CreateCapabilityForce 完全一致（同一套校验、同一个闸门、同样的幂等折叠），
// 只是把「逐条建边」也纳进了这个事务：任何一条 link 不合法 → 整体回滚。
//
// 边不合法时返回的 error **带下标**（`links[1]: …`），且 errors.Is(err, ErrEdgeRejected)
// 成立（handler 据此翻 400 并定位是哪一条）：
//
//	links[0]: edge_rejected: to link ref "ghost": store: not found: edges.to_ref "ghost"
func (d *DB) CreateCapabilityWithLinks(ctx context.Context, c *CapabilityVersion, links []LinkSpec, force bool) (*UpgradeImpact, error) {
	res, err := d.CreateCapabilityWithLinksAndArtifact(ctx, c, links, nil, force)
	if err != nil {
		return nil, err
	}
	return res.Impact, nil
}

// CreateCapabilityWithLinksAndArtifact 是 CreateCapabilityWithLinks 的「带产物」版本。
//
// 事务里的顺序（每一步的读都必须走 tx，见 txexec.go）：
//
//	① 升级闸门 —— 在同一快照里算影响面（与别的写者交错算出来的影响面没有意义）
//	② 建能力（createCapabilityTx：闸门是权威拦截，幂等折叠也在这里）
//	③ 产物（可选，用归一后的能力行构建 → 与能力行对账，不落库）
//	④ 逐条建边（createEdgeTx：引用解析 / 端口封闭 / literals / 静态环检测）
//	⑤ 提交
//
// 任一步失败 → defer 里回滚，库里一行都不会留下。
func (d *DB) CreateCapabilityWithLinksAndArtifact(ctx context.Context, c *CapabilityVersion, links []LinkSpec, build ArtifactBuilder, force bool) (*CapabilityWrite, error) {
	if c == nil {
		return nil, fmt.Errorf("create capability with links: nil")
	}
	if c.NamespaceID == "" || c.NamespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("create capability with links: begin tx: %w", err)
	}
	// 早退 / panic / 构建失败都回滚：这个事务的意义就是「不留半成品」，
	// 任何一条路径漏掉回滚都会把目标语义变回「部分成功」。
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	// ① 影响面：必须在事务内算（闸门读的是「此刻的 latest」与「此刻的边」）。
	//
	// 这里算一遍是为了把影响面**总是**回给调用方（空影响面也是 []），与
	// handler 的 CheckUpgrade 预计算同一口径；真正的拦截在 ② 里（createCapabilityTx
	// 内部再判一次，那是写入路径的唯一权威——它还负责「同内容幂等折叠不过闸门」）。
	impact, err := checkUpgradeOn(ctx, ex, c.NamespaceID, c)
	if err != nil {
		if errors.Is(err, ErrNamespaceRequired) {
			return nil, err
		}
		return nil, fmt.Errorf("create capability with links: %w", err)
	}

	// ② 能力行（校验、闸门、幂等折叠都在这一处，与 CreateCapabilityForce 共用）
	if err := createCapabilityTx(ctx, ex, c, force); err != nil {
		return nil, err
	}

	// ③ 产物：用**归一后**的能力行构建（见 ArtifactBuilder 注释）
	var savedArtifact *Artifact
	if build != nil {
		art, berr := build(c)
		if berr != nil {
			return nil, &ArtifactBuildError{Err: berr}
		}
		if art == nil {
			return nil, &ArtifactBuildError{Err: errors.New("artifact builder returned nil")}
		}
		// 归属由这里兜底：产物必须挂在刚建的那一行能力上，调用方不必再拼一遍。
		if verr := verifyBuiltArtifact(c, art); verr != nil {
			return nil, &ArtifactBuildError{Err: verr}
		}
		// 不再落库（P2 删了 artifacts 表）：产物就是这一行正文算出来的东西。
		// 回执里带上它，是因为响应要回 digest 与整份产物（契约 §5）。
		savedArtifact = art
	}

	// ④ 边：逐条建，失败带下标整体回滚
	edges := make([]*Edge, 0, len(links))
	for i, spec := range links {
		if strings.TrimSpace(spec.FromPort) == "" || strings.TrimSpace(spec.ToRef) == "" {
			return nil, fmt.Errorf("links[%d]: %w: fromPort and toRef are both required", i, ErrEdgeRejected)
		}
		e := &Edge{
			NamespaceID: c.NamespaceID,
			// 能力边：左端是**裸名字**（= 刚建的这个能力，跟随 latest）。
			FromIsSource: false,
			FromRef:      c.Name,
			FromPort:     spec.FromPort,
			// 右端原样透传：`notify` 跟随 latest / `notify@2` 钉死（归一在 createEdgeTx 里）。
			ToRef:    spec.ToRef,
			Literals: spec.Literals,
			Priority: spec.Priority,
		}
		if err := createEdgeTx(ctx, ex, e); err != nil {
			return nil, fmt.Errorf("links[%d]: %w", i, err)
		}
		edges = append(edges, e)
	}

	// ⑤ 提交
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("create capability with links: commit: %w", err)
	}
	return &CapabilityWrite{Capability: c, Edges: edges, Artifact: savedArtifact, Impact: impact}, nil
}

// ---------- 批量创建（契约 §4）----------
//
// 「一个表单建多个能力」（a 和 b 一起定义，再互相接上）在单建路径上做不到：
// 能力表 append-only，a 建好了、b 失败，a 就永远留在库里；而 a→b 的边在 b 还不存在
// 时也建不出来。所以批量必须**整批一个事务**：全部能力行 + 全部产物行 + 全部边行，
// 任一步失败整体回滚。
//
// 边的两端是**引用**，可以先引用本批新建的名字：事务里先插全部能力行，
// 再逐条解析边（`createEdgeTx` 在同一快照里读得到刚写进去的行）。
//
// 环不在这里判（契约 §2 删掉了配置期静态环检测）：`review → fix → review` 是合法
// 编排，兜底是运行期的 hop 预算与 trace 预算。本函数**不做任何**环检测。

// ErrBatchEmpty 是「批量里没有任何能力」的哨兵（handler 据此回 400）。
var ErrBatchEmpty = errors.New("capability_batch_empty")

// ErrBatchDuplicateName 是「批内 name 重复」的哨兵（handler 据此回 400）。
//
// 批内重名必须硬拒：`links[].fromRef/toRef` 用的是**名字**，重名会让同一条边
// 指向两个不同的定义，而「谁是谁」在请求里根本没有表达方式。
var ErrBatchDuplicateName = errors.New("capability_batch_duplicate_name")

// BatchLinkSpec 是一条**批内** link：两端都是显式引用。
//
// 与 LinkSpec 的唯一差别：左端不再隐含「刚建的那个能力」，而是显式给 fromRef——
// 它可以是本批新建的名字、库里已有的名字，或 `name@version` 钉死。
type BatchLinkSpec struct {
	// FromRef / ToRef 是能力引用（`name` 跟随 latest / `name@version` 钉死）。
	FromRef  string `json:"fromRef"`
	FromPort string `json:"fromPort"`
	ToRef    string `json:"toRef"`
	// Literals 是这条边的常量覆盖（与 LinkSpec.Literals 同口径：原样落库）。
	Literals json.RawMessage `json:"literals,omitempty"`

	// Priority 是这条候选的优先级（契约 §1：小的优先，同级轮盘）。
	Priority int `json:"priority,omitempty"`
}

// CapabilityBatchWrite 是一次批量创建的回执。
type CapabilityBatchWrite struct {
	// Capabilities 与请求的能力一一对应、**同序**。每项都是归一后的权威行
	// （幂等折叠时它的 version/id 来自已有那一行）——调用方必须用它，而不是请求里那份。
	Capabilities []*CapabilityVersion
	// Edges 与请求的 links 一一对应、**同序**（每条都已归一：引用、literals 都是落库形态）。
	Edges []*Edge
	// Impact 是整批**合并**后的影响面（恒非 nil，空影响面也是 [] 而不是 null）。
	//
	// 整批只有一个影响面对象（与单建同一个形状，前端不必分叉），所以：
	//   - 没有破坏性变更时，Capability / FromVersion / ToVersion 取自**批内第一个**能力
	//     （与单建一样总有值，不是空对象）；
	//   - 有破坏性变更时，这三个字段换成**第一个破坏性**能力；
	//   - Changes / Links 是整批的**并集**（去重、稳定顺序），Change.Detail 带能力名前缀。
	// 单建时两者恒等，合并口径只在批量里有意义。
	Impact *UpgradeImpact
}

// CreateCapabilitiesBatch 在一个事务里建 N 个能力 + 它们的产物（派生值，不落库）+ M 条边。
//
// 事务里的顺序（每一步的读都必须走 tx，见 txexec.go）：
//
//	① 逐条算影响面并**合并**（整批 409 要报全部破坏性能力，而 createCapabilityTx
//	   的闸门只会报出第一个撞上的那一个）
//	② 逐条建能力（createCapabilityTx：校验 / 闸门 / 幂等折叠都在那一处）
//	③ 产物（可选，用归一后的能力行构建 → 与能力行对账，不落库）
//	④ 逐条建边（createEdgeTx：引用解析 / 端口封闭 / literals），此时 ② 建的行已经可见
//	⑤ 提交
//
// 任一步失败 → 整体回滚：能力 0 条、产物 0 条、边 0 条（契约 §4 的三条断言）。
//
// builders 与 caps 对齐：builders[i] 为 nil 表示第 i 个能力没有产物（整个切片可以是 nil）。
func (d *DB) CreateCapabilitiesBatch(ctx context.Context, caps []*CapabilityVersion, links []BatchLinkSpec, builders []ArtifactBuilder, force bool) (*CapabilityBatchWrite, error) {
	if len(caps) == 0 {
		return nil, fmt.Errorf("create capabilities batch: %w", ErrBatchEmpty)
	}
	if len(builders) != 0 && len(builders) != len(caps) {
		// 对齐错了会把 A 的产物挂到 B 上（digest 也错），这种编程错误必须在写入前炸。
		return nil, fmt.Errorf("create capabilities batch: builders length %d != capabilities %d", len(builders), len(caps))
	}
	nsID := caps[0].NamespaceID
	if nsID == "" || nsID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	seen := make(map[string]int, len(caps))
	for i, c := range caps {
		if c == nil {
			return nil, fmt.Errorf("capabilities[%d]: nil", i)
		}
		// 一批只能属于一个命名空间（与单建的命名空间规则同口径）：跨命名空间的一批
		// 既没有「一个表单」的语义，也会让下面的 nsID 变成「第一个的」。
		if c.NamespaceID != nsID {
			return nil, fmt.Errorf("capabilities[%d]: namespace mismatch (batch is %s, got %s)", i, nsID, c.NamespaceID)
		}
		name := strings.TrimSpace(c.Name)
		if name == "" {
			return nil, fmt.Errorf("capabilities[%d]: name is required", i)
		}
		if j, dup := seen[name]; dup {
			return nil, fmt.Errorf("capabilities[%d]: %w: %q 在批内重复（第一次出现在 capabilities[%d]）", i, ErrBatchDuplicateName, name, j)
		}
		seen[name] = i
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("create capabilities batch: begin tx: %w", err)
	}
	// 早退 / panic / 任何一步失败都回滚：这个事务的意义就是「不留半成品」。
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	// ① 影响面：整批合并后再判。
	//
	// 不能只靠 ② 里 createCapabilityTx 的那次闸门：它遇到第一个破坏性能力就返回，
	// 409 的 body 只会带那一个；用户拿不到「这一批里到底哪几个会改线」。
	merged := &UpgradeImpact{Changes: []UpgradeChange{}, Links: []UpgradeLink{}}
	for i, c := range caps {
		impact, ierr := checkUpgradeOn(ctx, ex, nsID, c)
		if ierr != nil {
			if errors.Is(ierr, ErrNamespaceRequired) {
				return nil, ierr
			}
			return nil, fmt.Errorf("create capabilities batch: capabilities[%d]: %w", i, ierr)
		}
		mergeUpgradeImpact(merged, impact)
	}
	if merged.Breaking && !force {
		return nil, &UpgradeBreakingError{Impact: merged}
	}

	// ② 能力行：**先插全部**，这样批内的边能引用本批新建的名字（③④ 才看得见它们）。
	created := make([]*CapabilityVersion, 0, len(caps))
	for i, c := range caps {
		if cerr := createCapabilityTx(ctx, ex, c, force); cerr != nil {
			// 带下标：一批几十条时，「第几条」是唯一有用的定位信息。
			// errors.Is/As 仍然穿透（%w）：UpgradeBreakingError / ErrEdgeRejected 都不变。
			return nil, fmt.Errorf("capabilities[%d]: %w", i, cerr)
		}
		created = append(created, c)
	}

	// ③ 产物：用**归一后**的能力行构建（见 ArtifactBuilder 的注释：
	//    幂等折叠会让版本号变，产物里嵌着 name@version，digest 也吃它）。
	for i, c := range caps {
		if len(builders) == 0 || builders[i] == nil {
			continue
		}
		art, berr := builders[i](c)
		if berr != nil {
			return nil, &ArtifactBuildError{Err: fmt.Errorf("capabilities[%d]: %w", i, berr)}
		}
		if art == nil {
			return nil, &ArtifactBuildError{Err: fmt.Errorf("capabilities[%d]: artifact builder returned nil", i)}
		}
		// 归属兜底 + 与权威行对账（P2 起产物不再落库，builder 的作用从「写入」变成
		// 「在事务内自检：构建路径与读取路径算出来的是同一份内容」）。
		if verr := verifyBuiltArtifact(c, art); verr != nil {
			return nil, &ArtifactBuildError{Err: fmt.Errorf("capabilities[%d]: %w", i, verr)}
		}
	}

	// ④ 边：逐条建（两端都是引用，本批新建的名字在这里解析得到）。
	edges := make([]*Edge, 0, len(links))
	for i, spec := range links {
		if strings.TrimSpace(spec.FromRef) == "" || strings.TrimSpace(spec.FromPort) == "" || strings.TrimSpace(spec.ToRef) == "" {
			return nil, fmt.Errorf("links[%d]: %w: fromRef, fromPort and toRef are all required", i, ErrEdgeRejected)
		}
		e := &Edge{
			NamespaceID: nsID,
			// 批内 link 只建**能力边**：入口边（FromIsSource）的左端是来源名，
			// 「一个表单建多个能力再接线」不会产生入口边（入口边由源页单独建）。
			FromIsSource: false,
			FromRef:      strings.TrimSpace(spec.FromRef),
			FromPort:     spec.FromPort,
			ToRef:        strings.TrimSpace(spec.ToRef),
			Literals:     spec.Literals,
			Priority:     spec.Priority,
		}
		if eerr := createEdgeTx(ctx, ex, e); eerr != nil {
			return nil, fmt.Errorf("links[%d]: %w", i, eerr)
		}
		edges = append(edges, e)
	}

	// ⑤ 提交
	if cerr := tx.Commit(); cerr != nil {
		return nil, fmt.Errorf("create capabilities batch: commit: %w", cerr)
	}
	return &CapabilityBatchWrite{Capabilities: created, Edges: edges, Impact: merged}, nil
}

// mergeUpgradeImpact 把一个能力的影响面并进整批的口径（去重、稳定顺序）。
//
// 身份字段（Capability / FromVersion / ToVersion）两次赋值的机会：第一次调用
// （整批的第一个能力）先填上，让「没有破坏性变更」的响应与单建一样带这三个值；
// 一旦某个能力是破坏性的，就换成它——409 的 body 必须说清是哪一个。
//
// Change.Detail 在并集里带**能力名前缀**：两个能力都删了同名出口时，说明文字
// 一字不差，不加前缀就会去重成一条——用户看到「1 处变化」而实际是两处，
// 也看不出是哪两条能力变的。Links 不用前缀：它自带 fromRef/toRef。
func mergeUpgradeImpact(dst, src *UpgradeImpact) {
	if dst == nil || src == nil {
		return
	}
	if dst.Capability == "" {
		dst.Capability, dst.FromVersion, dst.ToVersion = src.Capability, src.FromVersion, src.ToVersion
	}
	if src.Breaking && !dst.Breaking {
		dst.Capability, dst.FromVersion, dst.ToVersion = src.Capability, src.FromVersion, src.ToVersion
		dst.Breaking = true
	}
	seenChange := make(map[string]bool, len(dst.Changes)+len(src.Changes))
	for _, c := range dst.Changes {
		seenChange[string(c.Kind)+"\x00"+c.Detail] = true
	}
	for _, c := range src.Changes {
		detail := c.Detail
		if src.Capability != "" {
			detail = src.Capability + ": " + detail
		}
		key := string(c.Kind) + "\x00" + detail
		if seenChange[key] {
			continue
		}
		seenChange[key] = true
		dst.Changes = append(dst.Changes, UpgradeChange{Kind: c.Kind, Detail: detail})
	}
	// 同一条 link 可能被两个能力同时影响（from 删了 port、to 改了入参）：
	// 合并成一行、reason 拼起来，与单建里 upgradeAffectedLinks 的口径一致。
	idx := make(map[string]int, len(dst.Links))
	for i, l := range dst.Links {
		idx[l.FromRef+"\x00"+l.Port+"\x00"+l.ToRef] = i
	}
	for _, l := range src.Links {
		key := l.FromRef + "\x00" + l.Port + "\x00" + l.ToRef
		if i, ok := idx[key]; ok {
			if !strings.Contains(dst.Links[i].Reason, l.Reason) {
				dst.Links[i].Reason = dst.Links[i].Reason + "；" + l.Reason
			}
			continue
		}
		idx[key] = len(dst.Links)
		dst.Links = append(dst.Links, l)
	}
	sort.Slice(dst.Changes, func(i, j int) bool {
		if dst.Changes[i].Kind != dst.Changes[j].Kind {
			return dst.Changes[i].Kind < dst.Changes[j].Kind
		}
		return dst.Changes[i].Detail < dst.Changes[j].Detail
	})
	sort.Slice(dst.Links, func(i, j int) bool {
		if dst.Links[i].FromRef != dst.Links[j].FromRef {
			return dst.Links[i].FromRef < dst.Links[j].FromRef
		}
		if dst.Links[i].Port != dst.Links[j].Port {
			return dst.Links[i].Port < dst.Links[j].Port
		}
		return dst.Links[i].ToRef < dst.Links[j].ToRef
	})
}
