package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
)

// 部署状态（deployments.state）。
//
// 写者纪律（CONTRACT-protocol §7）：
//   - 建行/删行只有控制台 API 会做；
//   - state / reason / updated_at 只有节点帧（apply）与 hello 对账会写。
const (
	// DeploymentPending 中枢想部署，还没收到节点确认。
	DeploymentPending = "pending"
	// DeploymentApplied 节点确认应用成功。
	DeploymentApplied = "applied"
	// DeploymentFailed 节点拒绝（reason 里是短码 + 说明）。
	DeploymentFailed = "failed"
	// DeploymentLocal 是**旧的**「本地变体」态：节点上的 digest 与中枢期望不一致，
	// 且中枢不敢覆盖。
	//
	// 现在不再产生这个态：中枢是部署的唯一写者，发现不一致就重新下发（覆盖），
	// 对账会把行打成 pending。保留它只为了让旧库里已有的行能被读出来（下一次 hello
	// 对账就会把它们改成 applied/pending）。UI 上的黄色“本地变体”也因此只会是历史残留。
	DeploymentLocal = "local"
)

// ErrArtifactNotFound 表示「这个能力此刻没有可部署/可引用的产物」。
//
// 语义在 P2 之后收窄成两种（artifact 表已删，产物是能力行的正文算出来的）：
//   - 能力不存在，或不属于该命名空间；
//   - 能力是 **handler 形式**（中枢内置执行器认领，没有 files）——它不该被部署到节点，
//     也不存在「一份可推下去的产物」。
//
// 名字沿用旧哨兵：handler 层靠它翻 404 artifact_not_found，改名字换不来任何好处。
var ErrArtifactNotFound = errors.New("store: artifact not found")

// ErrDeploymentExists 同一 (node, capability, digest) 已经部署过。
var ErrDeploymentExists = errors.New("store: deployment already exists")

// Artifact 是一份能力产物的**派生值**（Capability Spec v3 §11.2）。
//
// P2 之后它不再是表里的一行，而是「能力行的正文 + metadata」拼出来的整份产物：
// digest 是能力行的 content_hash（≡ 产物 digest），Spec 是整份 canonical JSON。
// 之所以还留着一个结构体而不是返回 (digest, spec)：deploy 帧、/v1/artifacts、撤销帧
// 三处都要同一份形状，拼三遍迟早会拼出三种 digest。
//
// 字段取舍（契约 §11.2 / P2a）：
//   - NamespaceID / CapabilityID / Digest / Spec 是内容的全部身份，都留着；
//   - CreatedAt 删掉：没有行了，就没有落库时间（它唯一的作用是「新的在前」排序，
//     而按能力取只有一份）。
type Artifact struct {
	// ID 是**历史字段**：没有行身份之后它永远是空串，因此不进 JSON（omitempty）。
	// 留在结构体里只为兼容在途调用方（internal/route 的 fake store 还在构造它，
	// 那个文件的所有权不在 P2a）。新代码不要读它。
	ID           string `json:"id,omitempty"`
	NamespaceID  string `json:"namespaceId"`
	CapabilityID string `json:"capabilityId"`
	Digest       string `json:"digest"`
	// Spec 是整份产物的 canonical JSON（CanonicalJSON 的结果，含 metadata 与 spec）。
	Spec []byte `json:"spec"`
}

// Deployment 是「某台节点应当拥有某份产物」+「节点回报的状态」。
//
// 引用的是**能力行**（capability_id），digest 是那次期望的内容标识。没有 artifact_id：
// 产物不再是一个可以单独引用的物体。
type Deployment struct {
	ID           string `json:"id"`
	NamespaceID  string `json:"namespaceId"`
	NodeID       string `json:"nodeId"`
	CapabilityID string `json:"capabilityId"`
	Capability   string `json:"capability"`
	Version      int    `json:"version"`
	Digest       string `json:"digest"`
	State        string `json:"state"`
	Reason       string `json:"reason"`
	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
}

// CurrentArtifact 返回某能力**当前产物**：digest = 能力行的 content_hash，
// Spec = 由 capability_versions.spec 拼出的整份 canonical JSON。
//
// 能力不存在、或它是 handler 形式（没有 files，不该出现在部署面板里）时返回
// ErrArtifactNotFound —— 「还没有产物」与「这个能力不该有产物」在**部署**这条路上
// 是同一个回答（handler 形式的那条更具体的 400 builtin_not_deployable 在 handler 层
// 先判，见 internal/server 的 createDeployments）。
func (d *DB) CurrentArtifact(ctx context.Context, namespaceID, capabilityID string) (*Artifact, error) {
	return currentArtifactOn(ctx, d, namespaceID, capabilityID)
}

// LatestArtifact 是 CurrentArtifact 的**旧名字**（P2 的读路径已经全部改读能力行）。
//
// 为什么名字还在：internal/route 的 Dispatcher 通过自己的 Store 接口声明了
// `LatestArtifact(...)`，而那个包的所有权不在 P2a（Capability Spec v3 §7 把 route 切到
// CurrentArtifact 排在 P2b）。语义与 CurrentArtifact 逐字相同，**不再**碰 artifacts 表。
//
// Deprecated: 新代码用 CurrentArtifact。
func (d *DB) LatestArtifact(ctx context.Context, namespaceID, capabilityID string) (*Artifact, error) {
	return d.CurrentArtifact(ctx, namespaceID, capabilityID)
}

// CurrentArtifact 的 ex 版（事务内读必须走它，否则看不到本事务刚写的行）。
func currentArtifactOn(ctx context.Context, ex queryExec, namespaceID, capabilityID string) (*Artifact, error) {
	c, err := getCapabilityByIDOn(ctx, ex, namespaceID, capabilityID)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrArtifactNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read capability for artifact: %w", err)
	}
	return artifactOf(c)
}

// deployable 判「这个能力有没有一份可推给节点的产物」。
//
// 可部署 ⟺ **有 run**（entrypoint + files）。两种「没有实现」的形态都必须挡住：
//   - handler 形式：实现编译在中枢里，没有 files 可推（Capability Spec v3 §6.1）；
//   - 纯定义（既没 handler 也没 run）：根本不是能跑的东西——迁移 8 之前建的老行、
//     事件起点类能力的残壳都属于这一类。
//
// 放过去的后果完全相同：它会出现在部署面板里，推下去节点只会拒。
func deployable(c *CapabilityVersion) error {
	if c.Spec.Handler != "" || c.Spec.Run.Entrypoint == "" {
		return ErrArtifactNotFound
	}
	return nil
}

// artifactOf 把一行能力拼成整份产物（**唯一**的拼装点）。
//
// 两件事必须一起做对，否则症状极难查：
//   - digest 取 `c.Digest()`（content_hash，契约 §4 的「content_hash ≡ digest」）。
//     它必须与节点从收到的字节重算出来的一致，否则部署永远收敛不了；
//   - Spec 取 `artifact.CanonicalJSON(c.Artifact())`。P1 期间写穿到 artifacts 表里的
//     就是这串字节，所以「产物内容」在 P2 前后逐字节相同（deploy 帧零改动）。
func artifactOf(c *CapabilityVersion) (*Artifact, error) {
	if err := deployable(c); err != nil {
		return nil, err
	}
	a := c.Artifact()
	spec, err := artifact.CanonicalJSON(&a)
	if err != nil {
		return nil, fmt.Errorf("canonical json for %s@%d: %w", c.Name, c.Version, err)
	}
	return &Artifact{
		NamespaceID:  c.NamespaceID,
		CapabilityID: c.ID,
		Digest:       c.Digest(),
		Spec:         spec,
	}, nil
}

// GetArtifactByDigest 按内容寻址标识取产物：digest 去掉 `sha256:` 前缀就是
// capability_versions.content_hash（契约 §11.2）。
//
// 内容寻址的含义没变：同一个 digest 在同一命名空间里最多命中一行能力
// （UNIQUE(namespace_id, content_hash)）。handler 形式命中时返回 ErrArtifactNotFound
// ——它没有可推的产物。
func (d *DB) GetArtifactByDigest(ctx context.Context, namespaceID, digest string) (*Artifact, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	hash := strings.TrimPrefix(digest, artifact.DigestPrefix)
	if hash == "" {
		return nil, ErrArtifactNotFound
	}
	c, err := getCapabilityByHashOn(ctx, d, namespaceID, hash)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrArtifactNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get artifact by digest: %w", err)
	}
	return artifactOf(c)
}

// ListArtifacts 返回某个能力的产物列表。
//
// **最多一条**（P2 起的语义，契约 §11.2）：产物不再是可累加的历史行，一个能力行
// = 一份内容 = 一个 digest。保留列表形状只为不动 `GET /v1/artifacts` 的响应契约
// （{artifacts:[…]}，e2e 与外部调用方都钉着它）。
//
// 能力不存在 / handler 形式 → 空列表（不是错误）：列表端点回答的是「有哪些」。
func (d *DB) ListArtifacts(ctx context.Context, namespaceID, capabilityID string) ([]*Artifact, error) {
	got, err := d.CurrentArtifact(ctx, namespaceID, capabilityID)
	if errors.Is(err, ErrArtifactNotFound) {
		return []*Artifact{}, nil
	}
	if err != nil {
		return nil, err
	}
	return []*Artifact{got}, nil
}

// CreateDeployment 建一条部署（state=pending）。
//
// 同一 (node, capability) **只保留一行**：部署是中枢对「这台机器该有哪份内容」的
// 期望状态，新期望直接取代旧的。
//
// 为什么不再把旧 digest 的行当“历史”留着：留着会让同一能力同时存在两条互斥的期望。
// hello 对账按行逐个比对 digest，于是旧行总是“与节点上那份一致”、新行总是“不一致”，
// 两者每次重连都互相推翻——节点会在两份内容之间来回被推。收敛的前提是
// 「一个 (机器, 能力) 只有一个期望」（见 markDeployedDigest）。
//
// 签名（契约 §11.2 / P2a）：(namespaceID, nodeID, **capabilityID**, digest)。
// digest 是「要部署的那份内容」的身份：
//   - 传空串 = 该能力行的当前内容；
//   - 传了就必须等于该能力行的 digest。传给别的能力的 digest 是自相矛盾的请求
//     （部署的期望内容与被引用的能力行不是一回事），按「找不到这份产物」拒绝，
//     而不是悄悄换一个能力去部署。
//
// 同一 (node, capability, digest) 已存在时是幂等 no-op：控制台重复点部署不该报错，
// 也不该重置已经 applied 的状态（否则一次误点会把绿标打回灰）。
func (d *DB) CreateDeployment(ctx context.Context, namespaceID, nodeID, capabilityID, digest string) (*Deployment, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if capabilityID == "" {
		return nil, fmt.Errorf("create deployment: empty capability_id")
	}
	now := Now()
	id := NewDeploymentID()

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin create deployment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 期望内容的唯一来源是能力行（digest 也是它算出来的）。
	capRow, err := getCapabilityByIDOn(ctx, txExec{tx: tx}, namespaceID, capabilityID)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrArtifactNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("resolve deployment capability: %w", err)
	}
	if derr := deployable(capRow); derr != nil {
		// 不可部署的形态（handler 形式 / 纯定义，见 deployable）。handler 层先判并回
		// 400 builtin_not_deployable；走到这里说明是别的调用方，同样不给建行。
		return nil, derr
	}
	digest = strings.TrimSpace(digest)
	if digest == "" {
		digest = capRow.Digest()
	}
	if digest != capRow.Digest() {
		return nil, ErrArtifactNotFound
	}

	// 取代范围只能是**同一能力**的旧行（别的能力不碰）。
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM deployments
		  WHERE namespace_id = ? AND node_id = ? AND capability_id = ? AND digest <> ?`,
		namespaceID, nodeID, capabilityID, digest); err != nil {
		return nil, fmt.Errorf("supersede previous deployment: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO deployments (id, namespace_id, node_id, capability_id, digest, state, reason, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,'',?,?)
		 ON CONFLICT(node_id, capability_id, digest) DO NOTHING`,
		id, namespaceID, nodeID, capabilityID, digest, DeploymentPending, now, now); err != nil {
		return nil, fmt.Errorf("create deployment: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit create deployment: %w", err)
	}
	return d.GetDeploymentByNodeCapability(ctx, namespaceID, nodeID, capabilityID)
}

// GetDeploymentByNodeCapability 取一条部署（同一 (node, capability) 至多一行）。
func (d *DB) GetDeploymentByNodeCapability(ctx context.Context, namespaceID, nodeID, capabilityID string) (*Deployment, error) {
	return d.scanDeployment(ctx,
		deploymentSelect+` WHERE d.namespace_id = ? AND d.node_id = ? AND d.capability_id = ?`,
		namespaceID, nodeID, capabilityID)
}

// GetDeployment 按 id 取一条部署。
func (d *DB) GetDeployment(ctx context.Context, namespaceID, id string) (*Deployment, error) {
	return d.scanDeployment(ctx, deploymentSelect+` WHERE d.namespace_id = ? AND d.id = ?`, namespaceID, id)
}

// deploymentDigestSQL 是「中枢期望的 digest」的**唯一**口径：能力行的 content_hash
// 加上标准前缀。不读 d.digest 列——列里那份是建行那一刻的副本（审计用），
// 而期望永远是「这个能力此刻的内容」（契约 §11.2：对账改从能力行取 digest）。
//
// 为什么要统一：节点算 digest 用的是它收到的产物字节，中枢若拿别的值去比，
// 症状是「每次连上来都判不一致、每次都重推一遍」——不影响正确性，但永远收敛不了。
const deploymentDigestSQL = `'` + artifact.DigestPrefix + `' || c.content_hash`

const deploymentSelect = `
SELECT d.id, d.namespace_id, d.node_id, d.capability_id, ` + deploymentDigestSQL + ` AS digest,
       d.state, d.reason, d.created_at, d.updated_at, c.name, c.version
  FROM deployments d
  JOIN capability_versions c ON c.id = d.capability_id`

func (d *DB) scanDeployment(ctx context.Context, query string, args ...any) (*Deployment, error) {
	var dp Deployment
	err := d.QueryRowContext(ctx, query, args...).Scan(
		&dp.ID, &dp.NamespaceID, &dp.NodeID, &dp.CapabilityID, &dp.Digest,
		&dp.State, &dp.Reason, &dp.CreatedAt, &dp.UpdatedAt,
		&dp.Capability, &dp.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get deployment: %w", err)
	}
	return &dp, nil
}

// DeploymentFilter 是部署列表的过滤条件（零值 = 不过滤）。
type DeploymentFilter struct {
	NodeID       string
	CapabilityID string
}

// ListDeployments 列出部署。
func (d *DB) ListDeployments(ctx context.Context, namespaceID string, f DeploymentFilter) ([]*Deployment, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	var where []string
	args := []any{namespaceID}
	where = append(where, "d.namespace_id = ?")
	if f.NodeID != "" {
		where = append(where, "d.node_id = ?")
		args = append(args, f.NodeID)
	}
	if f.CapabilityID != "" {
		where = append(where, "d.capability_id = ?")
		args = append(args, f.CapabilityID)
	}

	rows, err := d.QueryContext(ctx,
		deploymentSelect+` WHERE `+strings.Join(where, " AND ")+
			` ORDER BY d.created_at DESC, d.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	defer rows.Close()

	out := []*Deployment{}
	for rows.Next() {
		var dp Deployment
		if err := rows.Scan(
			&dp.ID, &dp.NamespaceID, &dp.NodeID, &dp.CapabilityID, &dp.Digest,
			&dp.State, &dp.Reason, &dp.CreatedAt, &dp.UpdatedAt,
			&dp.Capability, &dp.Version); err != nil {
			return nil, fmt.Errorf("scan deployment: %w", err)
		}
		out = append(out, &dp)
	}
	return out, rows.Err()
}

// DeleteDeployment 删一条部署，返回被删的行（供调用方推 remove 帧）。
func (d *DB) DeleteDeployment(ctx context.Context, namespaceID, id string) (*Deployment, error) {
	dp, err := d.GetDeployment(ctx, namespaceID, id)
	if err != nil {
		return nil, err
	}
	if _, err := d.ExecContext(ctx,
		`DELETE FROM deployments WHERE namespace_id = ? AND id = ?`, namespaceID, id); err != nil {
		return nil, fmt.Errorf("delete deployment: %w", err)
	}
	return dp, nil
}

// MarkDeployResult 落一条来自节点 apply 帧的结果（只碰 observed 三列）。
func (d *DB) MarkDeployResult(ctx context.Context, namespaceID, deployID, state, reason string) error {
	// pending 也是合法目标：中枢重连时把“要重新下发”的行标回 pending（中枢覆盖语义）。
	if state != DeploymentApplied && state != DeploymentFailed &&
		state != DeploymentLocal && state != DeploymentPending {
		return fmt.Errorf("mark deploy result: invalid state %q", state)
	}
	res, err := d.ExecContext(ctx,
		`UPDATE deployments SET state = ?, reason = ?, updated_at = ?
		  WHERE namespace_id = ? AND id = ?`,
		state, reason, Now(), namespaceID, deployID)
	if err != nil {
		return fmt.Errorf("mark deploy result: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// markDeployedDigest 是 hello 对账的入口：拿「节点实际缓存的 digest」和中枢的期望
// 逐能力比对。
//
// 语义（中枢覆盖）：
//   - digest 一致 → applied（绿勾）
//   - 不一致   → pending，reason 里两端 digest 都写上，回来后由 handleHello
//     把中枢期望的那份推下去（节点不需要知道自己错在哪里，它只要拿到对的东西）
//
// 期望值取**能力行**的 digest（deploymentDigestSQL），不是 deployments.digest 列的
// 副本：列里那份是建行那一刻的快照，而「这个能力现在的产物」只有能力行说了算
// （契约 §11.2）。老库里指向历史内容的行因此会自然收敛到当前内容。
//
// 早先这里标 `local`（“本地变体，中枢永不覆盖”，契约 §7 的旧口径），实践中行不通：
// 部署本来就是中枢下发的，节点上那份不一致是“还没收到”而不再是“用户改过”，
// 标黄只会让一条正常的部署永远停在黄标上，还得人去点一次。
func (d *DB) markDeployedDigest(ctx context.Context, tx *sql.Tx, nodeID, capabilityID, digest string) (int, error) {
	now := Now()
	rows, err := tx.QueryContext(ctx,
		`SELECT d.id, `+deploymentDigestSQL+`
		   FROM deployments d
		   JOIN capability_versions c ON c.id = d.capability_id
		  WHERE d.node_id = ? AND d.capability_id = ?`, nodeID, capabilityID)
	if err != nil {
		return 0, fmt.Errorf("query deployments for reconcile: %w", err)
	}
	defer rows.Close()

	type row struct{ id, digest string }
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.digest); err != nil {
			return 0, fmt.Errorf("scan deployment for reconcile: %w", err)
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	n := 0
	for _, r := range all {
		state, reason := DeploymentPending, ""
		if r.digest == digest {
			state = DeploymentApplied
		} else {
			reason = fmt.Sprintf("digest mismatch: node has %s, console expects %s — 重新下发",
				digest, r.digest)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE deployments SET state = ?, reason = ?, updated_at = ? WHERE id = ?`,
			state, reason, now, r.id); err != nil {
			return 0, fmt.Errorf("mark deployment: %w", err)
		}
		n++
	}
	return n, nil
}

// DeploymentDelivery 是「把一份产物推给某台节点」所需的全部信息（行 + 产物原文）。
//
// 为什么单独开一个形状：deploy 帧要的是 canonical JSON（整份产物），而 Deployment
// 是给控制台看的（不带产物正文）。两者混在一个结构里会让列表接口无端背上几 KB 正文。
//
// 两个字段都从**能力行**派生（P2 起）：Digest = content_hash（期望），Spec = 由
// capability_versions.spec 拼出的整份 canonical JSON。所以「推下去的字节」与
// 「中枢说的期望 digest」永远同源——这正是节点算出来的 digest 能与中枢对上的前提。
type DeploymentDelivery struct {
	DeploymentID string
	CapabilityID string
	Capability   string
	Version      int
	Digest       string
	Spec         []byte
}

// DeploymentsForNode 返回该节点当前的部署（每个能力最多一行，由 CreateDeployment 保证）。
//
// 调用方是握手结束后的补推：节点连上来时，它缓存的 digest 如果不是中枢期望的那份，
// 就把期望的那份推下去——部署是中枢下发的，这里不做“本地变体”协商，直接覆盖。
// 同时补上了「部署时节点离线」那条路径（原来只有注释说会补推，代码里没有）。
func (d *DB) DeploymentsForNode(ctx context.Context, namespaceID, nodeID string) ([]*DeploymentDelivery, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.QueryContext(ctx,
		`SELECT d.id, c.id, c.name, c.version, `+deploymentDigestSQL+`, c.spec
		   FROM deployments d
		   JOIN capability_versions c ON c.id = d.capability_id
		  WHERE d.namespace_id = ? AND d.node_id = ?
		  ORDER BY d.created_at DESC, d.id`, namespaceID, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list deployments for node: %w", err)
	}
	defer rows.Close()

	out := []*DeploymentDelivery{}
	for rows.Next() {
		var it DeploymentDelivery
		var spec string
		if err := rows.Scan(&it.DeploymentID, &it.CapabilityID, &it.Capability, &it.Version, &it.Digest, &spec); err != nil {
			return nil, fmt.Errorf("scan deployment for node: %w", err)
		}
		c := &CapabilityVersion{
			ID:          it.CapabilityID,
			NamespaceID: namespaceID,
			Name:        it.Capability,
			Version:     it.Version,
		}
		if trimmed := strings.TrimSpace(spec); trimmed != "" {
			if err := json.Unmarshal([]byte(trimmed), &c.Spec); err != nil {
				return nil, fmt.Errorf("decode capability spec for deployment %s: %w", it.DeploymentID, err)
			}
		}
		// handler 形式不可部署（建行时就拒了），真在库里出现说明行是历史残留：
		// 直接跳过而不是推一份没有 files 的产物下去（节点只会拒，日志还会骗人）。
		full, aerr := artifactOf(c)
		if errors.Is(aerr, ErrArtifactNotFound) {
			continue
		}
		if aerr != nil {
			return nil, aerr
		}
		it.Spec = full.Spec
		out = append(out, &it)
	}
	return out, rows.Err()
}

// deploymentRetryAfter 是节点离线时部署重推的退避（目前不落库，留给 sweeper）。
const deploymentRetryAfter = 30 * time.Second

var _ = deploymentRetryAfter // 保留给后续 sweeper 使用，避免误删常量
