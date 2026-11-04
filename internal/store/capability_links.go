package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 本文件是「能力 + 下游边（+ 产物）一次提交」的原子写入入口。
//
// 为什么需要它：控制台新建能力时填的「出口 + 下游」以前是 1 + N 个请求
// （先 POST /v1/capabilities，再逐条 POST /v1/edges）。中间任何一条失败，
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
// 建完之后用 POST /v1/edges 单独建一条即可。
type LinkSpec struct {
	FromPort string          `json:"fromPort"`
	ToRef    string          `json:"toRef"` // name 或 name@version；空 = 解析失败
	Literals json.RawMessage `json:"literals,omitempty"`
}

// ArtifactBuilder 在事务内、能力行**归一化之后**被调用，返回要落库的产物行。
//
// 为什么是回调而不是直接传 *Artifact：content-hash 幂等会把「同内容但版本号不同」
// 的请求折叠到**已有的那一行**（append-only，不变量 I8）——归一化之后版本号可能变。
// 产物里嵌着 name@version（digest 也吃它），用请求里的版本号建产物，就会写出一份
// 元数据与能力定义不一致、digest 也算错的产物。所以产物必须在拿到权威行之后再构建。
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

// CapabilityWrite 是一次原子写入的回执：权威能力行 + 建成的边 + 产物 + 影响面。
//
// Capability 是**归一后**的行（幂等折叠时它的 version/id 来自已有那一行），
// 调用方必须用它（而不是请求里的那份）去算响应与产物。
type CapabilityWrite struct {
	Capability *CapabilityVersion
	// Edges 按 links 的顺序返回（每条都已归一：引用、literals 都是落库的形态）。
	Edges []*Edge
	// Artifact 是落库的产物行；没传 ArtifactBuilder 时为 nil。
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
//	③ 产物（可选，用归一后的能力行构建 → 同一事务落库）
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
		if art.NamespaceID == "" {
			art.NamespaceID = c.NamespaceID
		}
		if art.CapabilityID == "" {
			art.CapabilityID = c.ID
		}
		savedArtifact, berr = createArtifactOn(ctx, ex, art)
		if berr != nil {
			return nil, fmt.Errorf("create capability with links: %w", berr)
		}
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
