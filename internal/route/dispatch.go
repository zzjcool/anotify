// Package route 实现事件链的路由判定：给定上游「能力 + 出口名」，算出要建哪些任务。
//
// 它是本仓库唯一一个知道「怎么从出口表走到任务」的地方：
//
//	① edges 查表 → N 个**候选**（预算先判：max_hops / budget）
//	② 解析参数模板（param_missing / param_type）+ 校参数（各自 params_schema）
//	   + 跨组织用户校验 → 滤掉不可达的候选
//	③ node_capabilities 查节点（同样只留可达候选）
//	④ 候选选择：最小 priority 组内轮盘挑**一个**；按负载（或粘性钉住）挑机器
//	⑤ 建任务
//
// 前三步都在建任务**之前**，所以剪枝不产生任务行（契约 §4）。
//
// 实现上分成两段：①–③（含构造 pending 任务）是 **plan**——只读，不落库、
// 不占槽位、不推帧；④–⑤ 是 **launch**——占槽位 + 推 assign 帧。拆开是 #140 的要求：
// 「子任务建行 + 写 emission」必须在同一个事务里（见 route.Emit），而 publish 是网络
// 操作、不能进事务，所以 plan 必须能在事务之前单独跑。
//
// 下一跳的唯一入口是 Emit（emit 帧 → plan → 一个事务写 emission + 建子任务行 →
// launch）。旧的两步接力入口（`result.out` 先算计划、再由后台任务补推 assign）已随
// P1.5 整条删除，`tasks.out` 列本身也在迁移 28 删掉。
//
// 但剪枝**一律留痕**：每一条剪枝都写一行 events（source='route'，#142），
// 否则「这条边为什么没走」在触发那一刻之后就永远查不到了。
// 本包不写 SQL：所有查表/落库都走 Store 接口，便于用 mock 覆盖全部分支。
package route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/zzjcool/anotify/internal/store"
)

// ---------- 派发 ----------

// DispatchInput 是一次派发的输入。
type DispatchInput struct {
	// NamespaceID 是这次派发的**隔离维度**（契约 §3 / 设计文档 §5）。
	//
	// 派发四步里 ① 查出口表、③ 找节点、⑤ 建任务都按它过滤/落库；② 校参数、
	// ④ 挑机器与命名空间无关，完全不变。
	//
	// 谁来给：根任务由 handler 给、来源触发由调用方从 src.NamespaceID 传进来、
	// 下一跳（emit）由父任务的 NamespaceID 继承（见 route.Dispatcher.upstreamKey）。
	//
	// 空值是**错误**，不是「回退默认命名空间」（见 checkInput）：回退会让
	// 「调用方忘了带 namespace」变成「悄悄写进了 default」，是最难查的那类串数据。
	NamespaceID  string `json:"namespaceId"`
	TraceID      string
	ParentTaskID string
	// FromCap 是上游标识：默认是上游能力的**名字**（P2.2：link 的左端是引用，
	// `analyze` / `analyze@1` 里的名字部分；tasks.capability 那一列）；
	// FromIsSource 为 true 时它是**来源名**（入口边，如 "github"）。
	//
	// 为什么是名字不是 id：link 存的是引用，同一条 link 在升级前后必须能被同一跳查到。
	// 按 id 查意味着「升级之后这条 link 就查不到了」——那正是 P2.2 要修的东西。
	//
	// 兼容：ws 的接力派发仍然传 task.CapabilityID（那一面不在本波范围），
	// plan 会把 `cap_…` 形状的 id 换成名字（见 Dispatcher.upstreamKey）。
	FromCap      string
	FromIsSource bool
	// Out 是上游**能力**回报的出口名。"" 表示上游没回报——此时不派发、也不记剪枝。
	// 入口边不看它（事件类型在 EntryFromPort 里）。
	Out string
	// To 是 emit 里脚本显式点名的**下游能力**（`name` / `name@version`）。
	//
	// 只有 emit 会给它（正常派发没有"指定下游"这件事，入口边更是扇出语义）。
	// 非空时候选集先被它过滤：点名的那个不在候选里 → 整个 emit 被拒
	// （ErrEmitToNotCandidate），**不改投**——改投等于"看起来成功了却发给了别人"。
	To string

	// EntryFromKey / EntryFromPort 是**入口边**的左端与端口（来源名 + 事件类型）。
	// FromIsSource=true 时必须给，否则 plan 拿不到要查的边。
	//
	// 为什么不复用 FromCap / Out：Out 的语义是「上游能力回报的出口名」，而入口边没有
	// 上游能力、事件类型也不是任何能力的出口。混在一个字段里，「没回报出口就结束」
	// 这条早退会悄悄地作用在源路径上，而那条路径上的「没出口」其实是配置错误。
	EntryFromKey  string
	EntryFromPort string

	// Params 是**能力边**的目标参数（emit 的 data 原样）。
	//
	// 入口边**不看**它：入口边的参数来自边上的模板（edges.mapping），由事件 payload
	// 求值——外部世界（GitHub / 定时器）不会替你拼参数，而能力边的参数由实现自己
	// emit 出来（P2.1 把「源」那条旁路删掉之后，这里只剩能力边一种语义）。
	Params map[string]any

	// Payload 是本次事件的原始 payload（入口边用它解析 edges.mapping 里的 {{路径}}）。
	// 能力→能力递归派发时为 nil（那里不做解析）。
	Payload map[string]any
	// VarValues 是源声明变量（config.vars）的求值结果（别名表），供 {{name}} 引用。
	VarValues map[string]any

	// DryRun 为 true 时只算"会派发什么、会剪枝什么"，**不落库、不推送**。
	//
	// 给「试运行」用：用户改完参数，保存前想看到"这条配置到底能不能跑"。
	// 复用 Dispatch 的同一套判定，所以试运行和真实触发不会出现分歧
	// （那种"试运行说能跑、真点却剪枝"的差异最伤信任）。
	//
	// DryRun 下 Tasks 里的任务只有能力/参数/节点信息，ID 等字段为空。
	DryRun bool

	// Path 是从根到本任务的能力序列（含本任务的能力）。
	//
	// 它现在是 **hop 预算** 的输入（契约 §2）：depth = len(Path)，depth >= max_hops
	// 就剪枝。环本身是合法编排（`review → fix → review`），所以这里不再做
	// 「同一个名字出现过就剪」的判重 —— 那会把一条**只走了一圈**的合法链也拒掉。
	Path []string
}

// fromKeyPort 返回这次派发的 (左端, 端口)：
//
//	入口边（FromIsSource）：来源名 + 事件类型
//	能力边：     上游能力**名字** + 它回报的出口名
func (in DispatchInput) fromKeyPort() (key, port string) {
	if in.FromIsSource {
		return in.EntryFromKey, in.EntryFromPort
	}
	return in.FromCap, in.Out
}

// upstream 返回剪枝记录里要回显的上游标识（入口边是来源名，能力边是能力名）。
func (in DispatchInput) upstream() string {
	if in.FromIsSource {
		return in.EntryFromKey
	}
	return in.FromCap
}

// DispatchOutput 是一次派发的结果：建成 N 个任务，或 N 条剪枝记录。
//
// 两者可以同时非空：扇出里某条边参数不过、另一条正常建任务，是合法结果（契约 §4 ②）。
type DispatchOutput struct {
	// json tag 必须写：这个结构会直接出现在 HTTP 响应里
	// （/v1/events、/v1/sources/{id}/fire、/v1/hooks/{token}）。
	// 不写的话 Go 默认用字段名 "Tasks"/"Pruned"，前端按 camelCase 读会得到
	// undefined——曾经就是这样：派发明明成功、前端却显示"0 个任务"。
	Tasks  []*store.Task  `json:"tasks"`
	Pruned []PrunedRecord `json:"pruned"`
}

// Store 是派发需要的最小 store 表面（便于 mock 测试，也便于将来换实现）。
//
// *store.DB 满足这个接口。
type Store interface {
	// EdgesFrom 是派发第 ① 步的唯一查表入口。
	//
	// fromIsSource 决定 fromKey 的解释（见 store.Edge.FromIsSource）：
	// false → 上游能力**名字**（P2.2：link 的左端是引用）；true → 来源名（入口边）。
	//
	// 命名空间维度是必须的：同名能力可以在两个命名空间里各有一条**不同的**出口边，
	// 少这个参数就会把别人的边当自己的（契约 §3 第 ① 步）。
	//
	// 返回的是**边目标**（目标能力的引用 + 该条边的字面量覆盖），不是 id：
	// 派发要把 literals 叠到目标参数上（设计 §3.3），分两次查会多一次往返，
	// 也让「一条边」这个概念在两个包里各有一半。目标引用要在 plan 里解析成
	// 具体版本（`name` 跟随 latest → 这次派发时刻的最新版）。
	EdgesFrom(ctx context.Context, namespaceID string, fromIsSource bool, fromKey, fromPort string) ([]store.LinkTarget, error)
	// ResolveRef 把 link 上的引用解析成能力定义（设计 §3.2）：
	// `name` → latest（**每次派发重新解析**，所以升级下一次派发就生效）；
	// `name@version` → 那一版（钉死，升级不影响）。
	//
	// 未命中返回 store.ErrNotFound（plan 把它翻成 prune/param_schema 之类可见失败，
	// 而不是让整次派发 500：一条指向已删能力的 link 不该拖垮整个扇出）。
	ResolveRef(ctx context.Context, namespaceID, ref string) (*store.CapabilityVersion, error)
	GetCapabilityByID(ctx context.Context, namespaceID, id string) (*store.CapabilityVersion, error)
	// NodesForCapability 只返回**本命名空间**里声明了该能力的节点（契约 §3 第 ③ 步）。
	//
	// 能力 id 全局唯一，所以这里天然只会匹配到一个命名空间的声明；仍然按
	// namespace_id 过滤一遍，防的是**数据被写坏**（见 store I11）。
	NodesForCapability(ctx context.Context, namespaceID, capabilityID string) ([]string, error)
	ListInstances(ctx context.Context, nodeID string) ([]*store.NodeInstance, error)
	// FreeSlotsByNode 一次拿到多个节点的空闲槽位数（#145：派发不能被 N+1 拖慢）
	FreeSlotsByNode(ctx context.Context, nodeIDs []string) (map[string]int, error)
	// LeaseAndAssign 在一个事务里完成「占槽位 + pending→assigned + 写归属/attempts」（#141）。
	//
	// 没有空闲槽位返回 store.ErrNoCapacity 且**什么都没写**（事务回滚）；
	// 其他错误同样带事务回滚，调用方不需要再补一次 ReleaseInstance。
	LeaseAndAssign(ctx context.Context, namespaceID, taskID, nodeID string, leaseUntil int64) (string, error)
	ReleaseInstance(ctx context.Context, nodeID, instanceID string) error
	ReleaseInstanceByTask(ctx context.Context, nodeID, taskID string) error
	GetPin(ctx context.Context, stickyKey string) (*store.NodePin, error)
	PutPin(ctx context.Context, p *store.NodePin) error
	CreateTasks(ctx context.Context, ts []*store.Task) error
	// ExtendLease 把租约推到指定时刻（派发失败回滚时用来"提前过期"）。
	ExtendLease(ctx context.Context, namespaceID, taskID string, leaseUntil int64) error
	// SweepExpiredLeases 回收过期租约（契约 §3.3）。
	//
	// 派发失败回滚复用它，而不是自己拼一套"退回 pending + 清归属"的 SQL：
	// 那样会出现第二个改任务归属的地方，与「唯一入口」的约束冲突。
	SweepExpiredLeases(ctx context.Context, now int64) ([]string, error)
	EnsureGraphVersion(ctx context.Context, namespaceID string) (*store.GraphVersion, error)
	// GetUserByID 查用户是否存在（契约 §10.1.1 的用户维度校验用）。
	//
	// #204 之后没有组织层，判据从「是不是本组织成员」收窄成「本部署里有没有这个人」：
	// 命名空间是唯一隔离边界，本部署任何已登录用户都能访问任何存在的命名空间。
	GetUserByID(id string) (*store.User, error)

	// CountTasksByTrace 返回同一个 trace **已经建出**的任务数（契约 §2 的 trace 任务预算）。
	//
	// 必须是**真查库**（不是进程内累加）：预算要挡住的是「同一条 trace 疯狂扇出」，
	// 而那种扇出往往来自并发到达的帧 —— 缓存计数会把别人刚建的行漏掉。
	// 口径：只数 tasks 表里已存在的行（本次派发即将建的行不算，所以 budget 就是上限）。
	CountTasksByTrace(ctx context.Context, namespaceID, traceID string) (int, error)

	// InsertPruneEvent 落一条剪枝事件（#142）。
	//
	// 它是 d.prune() 的唯一落库出口：剪枝的「产生」与「留痕」不能分叉，
	// 否则总会有某个分支只进了 HTTP 响应、没进库——那就又是一个查不到的剪枝。
	// 语义（幂等、idem_key 恒 NULL）由 store 实现，本包不关心细节。
	InsertPruneEvent(ctx context.Context, e *store.Event) (bool, error)

	// LatestArtifact 返回该能力**最新登记的产物**（P0-a：派发时把它的 digest 钉在任务行上）。
	//
	// 为什么是「最新一份」而不是「本次部署的那份」：任务行只钉 name@version（契约 I8），
	// 派发这一步看不到「哪台机器上的哪份部署」——那要等 ④ 挑完机器才知道（而任务行
	// 在建任务时就要落库）。最新产物是此刻这个能力要被执行的权威内容，也是节点缓存
	// 里「name@version → digest」索引的指向；两者对齐，事后能对上账。
	//
	// 没有产物时返回 store.ErrArtifactNotFound——那不是错误（见 plan 里的 pinDigest）。
	LatestArtifact(ctx context.Context, namespaceID, capabilityID string) (*store.Artifact, error)

	// ---- emit（P1.1，docs/design/emit-protocol.md §4 / §7）----
	//
	// emit 与派发共用同一套判定（plan），多出来的只有三件事：
	// 任务行必须可 emit（非终态）、seq 必须连续且幂等、以及「emission 与子任务行
	// 同事务」。下面四个方法就是这三件事需要的 store 表面。

	// GetTask 读任务行。emit 的第一件事就是拿它：状态决定收不收、
	// TraceID/Path/CapabilityID 决定新派发的形状（子任务只继承父任务的事实）。
	GetTask(ctx context.Context, namespaceID, id string) (*store.Task, error)
	// InsertEmissionAndCreateTasks 在**一个事务**里写 emission 并建子任务行（幂等）。
	//
	// 为什么不是两个方法：拆开就有「emission 已提交、下游行没有」的窗口——
	// 节点已拿到 ack 不会重发，下游就永远不存在（emit-protocol §7）。
	// 返回 first=false 表示重复 seq（什么都没写，调用方不得重复派发）。
	InsertEmissionAndCreateTasks(ctx context.Context, e *store.Emission, ts []*store.Task) (first bool, err error)
	// GetEmission 按 (task_id, seq) 取首次结果；未命中返回 store.ErrNotFound。
	// 幂等路径靠它把首次的 routed/pruned 原样回给节点。
	GetEmission(ctx context.Context, taskID string, seq int64) (*store.Emission, error)
	// EmissionSeqBefore 返回 seq < before 的已落库条数与最大 seq（缺口判定用）。
	// 「条数 == n-1 且最大 seq == n-1」等价于「{1..n-1} 齐全」。
	EmissionSeqBefore(ctx context.Context, taskID string, before int64) (count int, maxSeq int64, err error)
}

// Dispatcher 按派发四步算出要建的任务。
type Dispatcher struct {
	Store Store

	// UserScoped 是「params.to 必须是本部署里存在的用户」的能力名集合（契约 §10.1.1）。
	//
	// 这是 params_schema 管不到的业务规则：schema 只能说「to 是非空字符串」，
	// 「这个人在本部署里存在吗」要单独查 users 表（见 checkUserScoped）。
	// 每加一个按人投递（推送/私信）能力，都要在这里补一条。
	UserScoped map[string]bool

	// Now 返回当前 unixepoch 秒（测试可注入）。
	Now func() int64

	// MaxHops 是 hop 预算的上限（契约 §2，默认 16，来自 ANOTIFY_MAX_HOPS）。
	//
	// 语义：depth = len(入参 path)，depth >= MaxHops → 剪 max_hops。
	// 环是合法编排，靠它兜底：没有预算的话 `review → fix → review` 会一直跑下去。
	// <= 0 表示用默认值（见 maxHops）。
	MaxHops int

	// TraceTaskBudget 是**同一个 trace** 的任务总数上限（契约 §2，默认 512，
	// 来自 ANOTIFY_TRACE_TASK_BUDGET）。
	//
	// 语义：同一 trace 已建任务数 >= TraceTaskBudget → 剪 budget。
	// hop 预算只按深度算，「同一个 trace 里每秒 emit 一次的链」深度很浅却能把
	// 任务表撑爆，所以两个预算都要。<= 0 表示用默认值（见 traceTaskBudget）。
	TraceTaskBudget int

	// cursor 是**同级轮盘**的游标（契约 §3）：map[(namespace, cap, port, priority)] → 下一个下标。
	//
	// 为什么在进程内、不持久化：用户明确要求。它的语义只是「同级候选轮流来」，
	// 重启后从头开始是完全可以接受的（不是账，是调度偏好）；落库反而会引入
	// 「谁去更新这一行」的并发写热点，而它连审计价值都没有。
	//
	// cursorMu 保护 cursor：派发是并发的（多个 emit 同时进来）。
	cursorMu sync.Mutex
	cursor   map[cursorKey]int

	// Publisher 把刚派发的任务推给节点（assign 帧）。
	//
	// 为 nil 时只落库不推送（测试、以及"仅配置不跑"的场景）。
	// **非 nil 时推送失败会让 Dispatch 返回错误**：任务已经 assigned 在库里、
	// 但节点不知道有活干，这种状态必须让调用方知道（否则任务会静默卡到租约过期）。
	Publisher Publisher
}

// Publisher 把一条已派发的任务推给目标节点。
//
// 由 ws 层实现（NodeHub）。抽成接口是为了让 route 包不依赖 ws 包。
type Publisher interface {
	// PublishAssign 向 nodeID 推送 assign 帧；节点不在线返回 error。
	//
	// 作用域是**命名空间**（不是组织）：一个 WS 连接 = 一个节点 = 一个命名空间，
	// hub 只把帧推给同命名空间的连接（#162 / I10）。按组织路由会把 staging 的
	// 任务推给 prod 的机器 —— 这正是命名空间要消灭的那类串台。
	//
	// 返回 error 时派发器会把任务退回 pending（并释放槽位），
	// 让下一次派发能立刻重试——而不是让它卡到租约过期。
	PublishAssign(ctx context.Context, namespaceID, nodeID string, task *store.Task) error
}

// 两个预算的默认值（契约 §2）：hop 深度 16、单 trace 任务数 512。
//
// 为什么是这两个数：16 层足够表达现实里的编排（review/fix/test 来回几圈），
// 512 是一个 trace 的正常规模上界（几十个任务），同时远低于「配置/实现写错
// 导致无限扇出」的量级。两个都可以用环境变量覆盖（ANOTIFY_MAX_HOPS /
// ANOTIFY_TRACE_TASK_BUDGET，见 internal/server/config.go）。
const (
	DefaultMaxHops         = 16
	DefaultTraceTaskBudget = 512
)

// New 构造一个 Dispatcher，默认把 notify.phone 视为用户维度能力。
//
// 两个预算取默认值；接线层（server）会用环境变量覆盖它们。字段 <= 0 时
// 仍然回落到默认值（见 maxHops / traceTaskBudget），所以「忘了赋值」不会
// 把预算关掉——一个被关掉的预算等于没有防线，而那不会以错误的形式暴露。
func New(st Store) *Dispatcher {
	return &Dispatcher{
		Store:           st,
		UserScoped:      DefaultUserScoped(),
		Now:             store.Now,
		MaxHops:         DefaultMaxHops,
		TraceTaskBudget: DefaultTraceTaskBudget,
	}
}

// maxHops 返回生效的 hop 预算（<= 0 → 默认值）。
func (d *Dispatcher) maxHops() int {
	if d.MaxHops > 0 {
		return d.MaxHops
	}
	return DefaultMaxHops
}

// traceTaskBudget 返回生效的 trace 任务预算（<= 0 → 默认值）。
func (d *Dispatcher) traceTaskBudget() int {
	if d.TraceTaskBudget > 0 {
		return d.TraceTaskBudget
	}
	return DefaultTraceTaskBudget
}

// DefaultUserScoped 返回默认的用户维度能力集合。
func DefaultUserScoped() map[string]bool {
	return map[string]bool{
		// notify.phone 的 params.to 是用户 ID（契约 Q1：推送目标是用户，不是组织）
		"notify.phone": true,
	}
}

// candidate 是一条「已通过全部判定、等待建任务/派发」的目标。
//
// 它是 plan 与 launch 之间的唯一交接物：plan 只算候选（纯读），launch 只负责
// 占槽位/推帧。#140 要求「终态 + 建行」同事务、publish 在事务外，
// 所以这两半必须能分开调用（事务之前只能跑 plan 那一半）。
type candidate struct {
	cap    *store.CapabilityVersion
	params map[string]any
	nodes  []string
	// toRef 是这条边的目标**引用**（`name` / `name@version`），来自 edges.to_ref。
	// 候选排序的第一关键字（契约 §3：to_ref 字典序 → 保证确定）。
	toRef string
	// priority 是这条边的候选优先级（edges.priority，小的优先）。
	// 候选排序的第二关键字，也是「最小 priority 组」的判据。
	priority int
	// central 表示这个能力由**中枢自己执行**，不找节点。
	// 典型是 notify.phone：没人会写一个"发推送节点"。
	central bool
}

// Dispatch 执行派发四步（契约 §4），返回要建的任务或剪枝记录。
//
// 顺序（前三步都在建任务之前）：
//
//	① edges 查表 → N 个候选；0 行 → pruned/no_edge，结束；
//	   预算先判：depth >= max_hops → pruned/max_hops、
//	   trace 已有任务数 >= budget → pruned/budget（都是整次派发级别的）
//	② 逐条候选校验：解析参数模板(param_missing / param_type)
//	   → 参数(param_schema) → 跨组织用户(foreign_user)
//	③ node_capabilities 查节点；0 行 → pruned/no_node（这一条候选不可达）
//	④ 候选选择：最小 priority 组内轮盘选**一个**（契约 §3），
//	   再按负载或粘性钉住挑一台；挑不到容量就先把任务留 pending（不剪枝）
//	⑤ 事务内批量建任务 + 逐条派发
//
// 实现是 plan（①②③ + 候选选择 + 试运行早退 + 构造 pending 任务，只读）→ CreateTasks →
// launch（④⑤ 占槽位/推帧）三段。对外签名、返回值语义、剪枝留痕与拆分前一致；
// 唯一的变化是语义：一个 (能力, 端口) 从「全部下游各建一个任务（扇出）」变成
// 「候选集合里选一个」（契约 §3，全派需要 balance.mode=all，本期不实现）。
func (d *Dispatcher) Dispatch(ctx context.Context, in DispatchInput) (*DispatchOutput, error) {
	if err := d.checkInput(in); err != nil {
		return nil, err
	}

	out := &DispatchOutput{Tasks: []*store.Task{}, Pruned: []PrunedRecord{}}

	tasks, cands, err := d.plan(ctx, in, out)
	if err != nil {
		return nil, err
	}
	// 没有候选（链到头 / 全被剪枝）或试运行：plan 已把结果
	//（剪枝记录 / 试运行任务）填进 out，这里直接返回。
	if len(cands) == 0 || in.DryRun {
		return out, nil
	}

	// 先落库（原子），再派发。落库失败什么都没发生；
	// 派发失败的任务留在 pending，等后续调度（不会丢任务）。
	if err := d.Store.CreateTasks(ctx, tasks); err != nil {
		return nil, fmt.Errorf("route: create tasks: %w", err)
	}

	if err := d.launch(ctx, in, tasks, cands, out); err != nil {
		return nil, err
	}
	return out, nil
}

// checkInput 是 Dispatch / Emit 共用的前置校验（两者都直接调 plan）。
//
// 抽出来的理由：Emit 绕过了 Dispatch 直接调 plan，若不校验，
// Store 为 nil 时会在 plan 里 panic（Dispatch 本来就有这层校验）。
//
// 为什么这里不要求组织：组织层已在迁移 14 删除，命名空间是唯一隔离维度，
// 派发四步的每一次 store 调用都只吃 namespaceID。除了 store 的作用域参数，
// 派发不需要任何更粗的归属。
func (d *Dispatcher) checkInput(in DispatchInput) error {
	if d.Store == nil {
		return fmt.Errorf("route: dispatcher has no store")
	}
	if d.Now == nil {
		d.Now = store.Now
	}
	// 入口边的两个字段必填：少了左端，EdgesFrom 会拿空 key 去查、结果是「no_edge」——
	// 一个本该是**调用方 bug** 的形状被伪装成「用户没配边」；少了端口同理。
	if in.FromIsSource && in.EntryFromKey == "" {
		return fmt.Errorf("route: from_is_source requires entry_from_key (source name)")
	}
	if in.FromIsSource && in.EntryFromPort == "" {
		return fmt.Errorf("route: from_is_source requires entry_from_port (event type)")
	}
	// 命名空间必填：这里**不**回退默认命名空间（契约 §3 / §4）。
	//
	// 复用 store 的哨兵错误（ErrNamespaceRequired）是有意的：handler 只要
	// errors.Is(err, store.ErrNamespaceRequired) 就能把这个失败翻成 400，
	// 不必为「路由层少传了 namespace」再造一个只有它自己认识的错误类型。
	if in.NamespaceID == "" {
		return fmt.Errorf("route: empty namespace_id: %w", store.ErrNamespaceRequired)
	}
	if in.TraceID == "" {
		return fmt.Errorf("route: empty trace_id")
	}
	return nil
}

// plan 是派发的**只读**前半段：① 出口早退 + ② 查出口表 + ③ 逐条边判定
// （环/参数/跨组织用户/找节点）+ DryRun 早退 + 构造 pending 任务。
//
// 它**不落库、不占槽位、不推帧**（任务只有 ID/字段，尚未写进 tasks 表）。
// 唯一的写入是两处既有副作用，都不属于「派发结果」：
//   - EnsureGraphVersion：按需落一次图版本（契约 §10.2）；
//   - InsertPruneEvent：剪枝留痕（#142，best-effort、事务外，见 d.prune）。
//
// 为什么 plan 能同步跑（#140）：plan 全是只读查询（出口表/能力/节点），
// 把真正的网络操作（publish）留在后台就够了。而「必须先有计划才能进
// 『父任务终态 + 子任务建行』的事务」，所以 plan 挪到了节点的读循环里同步执行。
//
// 返回的 tasks 与 cands **一一对应、顺序一致**（launch 依赖这个不变量）。
// DryRun 时把试运行任务填进 out.Tasks，并返回 (nil, nil, nil)。
func (d *Dispatcher) plan(ctx context.Context, in DispatchInput, out *DispatchOutput) (tasks []*store.Task, cands []candidate, err error) {
	// ① 取「从哪来 / 哪个口」。
	//
	// 两类上游在这里汇成一对 (fromKey, fromPort)：
	//   - 入口边：来源名 + 事件类型（事件类型由外部系统决定，可以是任意值）
	//   - 能力边：上游能力**名字** + 它回报的出口名（link 的左端就是名字）
	//
	// 能力没回报出口 = 正常结束的分支，不查表、不剪枝（契约 §7 第 ① 行）。
	// （入口边一定有时刻：checkInput 已要求 EntryFromPort 非空。）
	fromKey, fromPort := in.fromKeyPort()
	if fromPort == "" {
		return nil, nil, nil
	}
	if !in.FromIsSource {
		// 兼容老调用方（ws 的接力派发传的是 task.CapabilityID）：换成名字再查。
		// 换成之后剪枝/留痕里的上游标识也统一是名字，trace 里不会一半 id 一半名字。
		name, err := d.upstreamKey(ctx, in.NamespaceID, fromKey)
		if err != nil {
			return nil, nil, err
		}
		fromKey = name
	}

	// ② 查出口表。**只剩这一条路径**（P2.1）：源不再有旁路，它的目标就是入口边。
	//
	// 入口边按 `from_port = ? OR from_port = '*'` 匹配（源能收任意事件类型），
	// 能力边严格按端口——那是能力声明的封闭契约（I2），不能凭空发明出口。
	// 只查**本命名空间**的边（契约 §3 第 ① 步）：别的命名空间里
	// 同名能力/同来源名的边不参与派发。
	targets, err := d.Store.EdgesFrom(ctx, in.NamespaceID, in.FromIsSource, fromKey, fromPort)
	if err != nil {
		return nil, nil, fmt.Errorf("route: lookup edges: %w", err)
	}
	if len(targets) == 0 {
		d.prune(ctx, in, out, PrunedRecord{
			Reason: ReasonNoEdge,
			Port:   fromPort,
			Detail: fmt.Sprintf("no edge from %s port %q", fromKey, fromPort),
		})
		return nil, nil, nil
	}

	// ---------- 预算（契约 §2）：hop 深度 + trace 任务数 ----------
	//
	// 两个预算都在**建任何东西之前**判，且都是**整次派发**级别的：它们回答的是
	// 「这条链还能不能继续」，与「这次的目标是哪个能力」无关。
	//
	// 顺序是先 hop 再 budget：hop 是纯内存判断（path 就在入参里），budget 要查一次库。
	// 两者都在 no_edge 之后：没有下游时「这条链到头了」比「预算用尽了」更准确。
	if maxHops := d.maxHops(); len(in.Path) >= maxHops {
		d.prune(ctx, in, out, PrunedRecord{
			Reason: ReasonMaxHops,
			Port:   fromPort,
			Detail: fmt.Sprintf("hop depth %d >= max_hops %d (path %s)",
				len(in.Path), maxHops, formatPath(in.Path)),
		})
		return nil, nil, nil
	}
	// trace 任务预算：**每次派发重新查库**（不缓存、不用进程内计数器）。
	//
	// 口径（与 store.CountTasksByTrace 同一份）：
	//   - 数的是 tasks 表里**已经建出**的行，本次派发即将建的行还不存在 → 不把
	//     「当前这次」算进去，所以 budget 本身就是上限（不是 budget-1）；
	//   - 不缓存：并发到达的两个 emit 各自读到的是库里真实行数，缓存会把
	//     「别人刚建的任务」漏掉，预算于是形同虚设。
	//
	// 残留窗口（明确写出来，别以为它是原子闸门）：plan 与建行不在同一个事务里，
	// 所以两个**同时**读到 511 的派发会各自建一个任务，trace 最终到 513。
	// 这个预算防的是「无限扇出的量级」，不是「精确到 512 的硬账」；要精确到行
	// 就得把 COUNT 挪进建行事务（emit 的 InsertEmissionAndCreateTasks），那会把
	// 剪枝留痕也拖进事务，代价远大于收益。
	budget := d.traceTaskBudget()
	built, err := d.Store.CountTasksByTrace(ctx, in.NamespaceID, in.TraceID)
	if err != nil {
		return nil, nil, fmt.Errorf("route: count tasks for trace %s: %w", in.TraceID, err)
	}
	if built >= budget {
		d.prune(ctx, in, out, PrunedRecord{
			Reason: ReasonBudget,
			Port:   fromPort,
			Detail: fmt.Sprintf("trace %s already has %d tasks >= budget %d", in.TraceID, built, budget),
		})
		return nil, nil, nil
	}

	// 图版本只落一次（按需落版，契约 §10.2）。图版本属于命名空间（edges 按 namespace 重建）。
	graph, err := d.Store.EnsureGraphVersion(ctx, in.NamespaceID)
	if err != nil {
		return nil, nil, fmt.Errorf("route: pin graph version: %w", err)
	}

	// ③ 逐条边：解析引用 → 参数 → 跨组织用户 → 找节点（都通过的进 cands）
	//
	// 解析引用（P2.2，设计 §3.2）集中在**这一处**：`name` 解析成**本次派发时刻的
	// latest**（升级下一次派发就生效），`name@version` 解析成那一版（钉死）。
	// resolveCache 只活一次 plan：同一条被扇出多次引用的能力只解析一次，
	// 但**不会**跨派发复用——缓存跨派发就等于把 latest 钉在第一次解析的那一刻，
	// 升级又永远不生效了。
	cache := newRefCache()

	// 脚本点名了具体下游（emit --to）→ **先**把边收敛到它，再进逐条校验。
	//
	// 顺序很关键：`--to` 的语义是「这次只发给它」。把别的边也校验一遍，会在 trace 里
	// 留下 `pruned: param_schema(别人)` —— 用户从没打算发给它，那是纯噪音；更糟的是
	// 「别人的必填字段配不上」看起来像这次调用失败了。同一出口的多个下游是**候选**
	// （运行期只走一个），不是扇出，所以没被点名的那几条本来就不该参与判定。
	//
	// 收敛判据用**解析后的**能力名（`name` / `name@version`），与节点侧 CLI 的
	// resolveEmitPort 同一口径；解析结果进 cache，主循环不会付第二次开销。
	if in.To != "" && !in.FromIsSource {
		narrowed, err := d.narrowTargetsTo(ctx, in.NamespaceID, targets, in.To, cache)
		if err != nil {
			return nil, nil, err
		}
		targets = narrowed
	}

	for _, tgt := range targets {
		capDef, err := d.resolveRef(ctx, in.NamespaceID, tgt.ToRef, cache)
		if err != nil {
			// 引用解析不到（能力被删 / 版本不存在）：只剪这一条边，不整次失败。
			// 一条坏 link 不该把扇出里其他健康的边一起卖掉（与坏 literals 同口径）。
			d.prune(ctx, in, out, PrunedRecord{
				Reason: ReasonParamSchema,
				Port:   fromPort,
				Detail: fmt.Sprintf("link target %q: %v", tgt.ToRef, err),
			})
			continue
		}
		targetID := capDef.ID

		// 这里**没有**「目标已在 path 里」的判重（契约 §2）：环是合法编排，
		// `review → fix → review` 必须能跑起来。兜底是上面那道 hop 预算 ——
		// 它按**实际走过的**深度剪枝，而不是按「链上出现过这个名字」。
		//
		// 该目标实际使用的参数，按**边的类型**分两种（P2.1）：
		//
		//  入口边：参数 = ResolveParams(这条边的 mapping, 事件 payload, 源变量)。
		//    外部世界不会替我们拼参数（GitHub 不会把 pull_request.title 换成
		//    下游的 params.title），所以引用只能挂在入口边上（设计 §11.5 选项 A）。
		//    求值只发生在这里——能力→能力递归派发不带 mapping，因此不会对上游
		//    结果再解析一次（契约 §2.5 的单趟边界）。
		//
		//  能力边：参数 = in.Params（emit 的 data 原样），**不求值**。
		//    参数怎么拼是实现的内部事务（设计 §8 已否决能力边的引用映射）。
		targetParams := in.Params
		if in.FromIsSource {
			mapping, err := decodeEdgeMapping(tgt.Mapping)
			if err != nil {
				// 库里的 mapping 坏了（手改 / 旧数据）：fail closed，只剪这一条边。
				// 不整次派发失败：一条坏边不应把扇出里其他健康的边一起卖掉。
				d.prune(ctx, in, out, PrunedRecord{
					Reason:       ReasonParamSchema,
					CapabilityID: targetID,
					Port:         fromPort,
					Detail:       fmt.Sprintf("edge mapping: %v", err),
				})
				continue
			}
			// 空 mapping = **不裁剪**：事件 payload 原样就是目标参数（恒等）。
			//
			// 为什么要有这条默认：多数入口边并不需要改字段名（事件里本来就有
			// title/body，下游要的也是它们）。要求每条入口边都写一遍
			// {"title":"{{title}}"} 只是仪式，且会让「配了源却跑不起来」
			// （payload 明明有值，参数却是空的）这种失败变得很难懂。
			// 这也是 P2.1 之前在 /v1/events 上一直成立的行为。
			if len(mapping) == 0 {
				// 恒等：事件原样就是目标参数。
				if in.Payload == nil {
					targetParams = map[string]any{}
				} else {
					targetParams = in.Payload
				}
			} else {
				resolved, misses, typeErr := ResolveParams(mapping, in.Payload, in.VarValues, capDef.ParamsSchema)
				// 先判 missing 再判 typeErr：缺字段时不拿"类型转不了"盖住真正的病因
				if len(misses) > 0 {
					d.prune(ctx, in, out, PrunedRecord{
						Reason: ReasonParamMissing, CapabilityID: targetID, Port: fromPort,
						Detail: formatParamMissing(capDef, misses),
					})
					continue
				}
				if typeErr != nil {
					d.prune(ctx, in, out, PrunedRecord{
						Reason: ReasonParamType, CapabilityID: targetID, Port: fromPort,
						Detail: formatParamType(capDef, *typeErr),
					})
					continue
				}
				targetParams = resolved
			}
		}

		// 字面量覆盖（P0-c，设计 §3.3）：目标实际 params = data ⊎ literals，**字面量优先**，
		// 所以 data 里不必再带这些键。
		//
		// 两个要点：
		//   ① 覆盖必须发生在 validateParams **之前** —— 字面量也是参数的一部分，
		//      类型写错了就要被 params_schema 拦住（配置期已经拦过一道，这是第二道，
		//      手改库/旧数据也要能挡住）。
		//   ② 这一层未来会被「emit 的 data」接管（P1.3：data ⊎ literals → ValidateParams），
		//      但**字面量覆盖的位置不变**：它永远在「拿到目标参数」与「校验参数」之间。
		//
		// 没有字面量的边（literals = {}）在这里就是原封不动地把 targetParams 往下走，
		// 行为与加这一列之前完全一致。
		merged, mergeErr := applyLiterals(targetParams, tgt.Literals)
		if mergeErr != nil {
			// 库里的 literals 坏了（手改 / 旧数据）：fail closed，只剪这一条边。
			// 不整次派发失败：一条坏边不应把扇出里其他健康的边一起卖掉。
			d.prune(ctx, in, out, PrunedRecord{
				Reason:       ReasonParamSchema,
				CapabilityID: targetID,
				Port:         fromPort,
				Detail:       fmt.Sprintf("edge literals: %v", mergeErr),
			})
			continue
		}
		targetParams = merged

		// 参数校验：不过只跳过这一条边，不影响其他边（扇出可部分成功）
		if msg := validateParams(capDef.ParamsSchema, targetParams); msg != "" {
			d.prune(ctx, in, out, PrunedRecord{
				Reason:       ReasonParamSchema,
				CapabilityID: targetID,
				Port:         fromPort,
				Detail:       fmt.Sprintf("%s@%d params_schema: %s", capDef.Name, capDef.Version, msg),
			})
			continue
		}

		// 跨组织用户校验（params_schema 管不到的业务规则）
		//
		// 查库失败必须**整个派发失败**（fail closed）：这是"不许推给外组织用户"
		// 的闸门，放行等于在最该拦住的时候放行。宁可让这一次 trace 失败并留错，
		// 也不能把消息推给不属于本组织的人。
		pruned, rec, err := d.checkUserScoped(ctx, capDef, targetParams)
		if err != nil {
			return nil, nil, err
		}
		if pruned {
			rec.Port = fromPort // 留痕要有完整证据：哪条边、哪个出口
			d.prune(ctx, in, out, rec)
			continue
		}

		// 内置中枢能力（如 notify.phone）：**跳过找节点**。
		//
		// 这类能力没有节点执行者——推送是中枢自己的能力。若照常走
		// node_capabilities 查询必然是 0 行，于是 pruned/no_node，
		// 推送永远发不出去。这是「内置能力」与「自定义能力」唯一的分岔点。
		if bc, ok := store.IsBuiltinCapability(capDef.Name, capDef.Version); ok && bc.Central {
			cands = append(cands, candidate{
				cap: capDef, params: targetParams, central: true,
				toRef: tgt.ToRef, priority: tgt.Priority,
			})
			continue
		}

		// 找节点（只在本命名空间的在线节点里找，契约 §3 第 ③ 步）
		nodes, err := d.Store.NodesForCapability(ctx, in.NamespaceID, targetID)
		if err != nil {
			return nil, nil, fmt.Errorf("route: nodes for capability %s: %w", targetID, err)
		}
		if len(nodes) == 0 {
			d.prune(ctx, in, out, PrunedRecord{
				Reason:       ReasonNoNode,
				CapabilityID: targetID,
				Port:         fromPort,
				Detail:       fmt.Sprintf("no node declares %s@%d", capDef.Name, capDef.Version),
			})
			continue
		}

		// 可以派：记成**候选**（不是「要建的任务」）。选谁由下面的候选选择决定。
		cands = append(cands, candidate{
			cap: capDef, params: targetParams, nodes: nodes,
			toRef: tgt.ToRef, priority: tgt.Priority,
		})
	}

	if len(cands) == 0 {
		return nil, nil, nil
	}

	// ---------- 候选选择（契约 §3） ----------
	//
	// 走到这里的 cands 已经只剩**可达**的候选（不可达的在上面逐条剪掉，各有理由）。
	// 一个 (能力, 端口) 默认只派**一个**下游（1:1）：取最小 priority 组，组内轮盘选一个。
	// 想「全派」（扇出到全部候选）需要 balance.mode=all，本期不实现（保存时已 400）。
	//
	// **入口边不参与候选选择**：一个源喂多个能力是「源扇出」（设计 §1 的静态 1:N），
	// 不是「端口候选」。两者的区别是语义的，不是实现细节：
	//
	//   · 源扇出：一盆水同时倒给 n 个能力 —— 每个能力都是**独立的一路**触发；
	//   · 端口候选：同一类产出可能去哪儿 —— 运行期只走**一个**（1:1），
	//     走哪个由 priority + 同级轮盘定。
	//
	// 把入口边也按候选选，症状是「一个源配了 2 个目标，实际只喂了 1 个」——
	// e2e routing A6 抓到过这个（配置对、任务只建一条，极难从日志看出来）。
	if !in.FromIsSource {
		// 端口平衡策略先看一眼：库被手改出 all / random 之类时按 priority 处理
		// （**不扇出**：没实现的语义不能靠猜），同时留一条 WARN。
		if mode := d.portBalanceMode(ctx, in, fromKey, fromPort, cache); mode != store.BalanceModePriority {
			warnUnsupportedBalanceMode(in.NamespaceID, fromKey, fromPort, mode)
		}
		// `--to` 的收敛已在 ③ 之前做完（连参数校验都不该看见没被点名的边）。
		chosen := d.pickCandidate(in.NamespaceID, fromKey, fromPort, cands)
		// 没被选中的候选**不是剪枝**：它们只是这一圈没轮到（下一次 emit 的轮盘会轮到别人）。
		// 所以这里不产生 pruned 记录，返回值的 Tasks 也只有一条。
		cands = []candidate{chosen}
	}

	// DryRun 在这里返回：判定已经做完（目标存在、参数过、节点在线），
	// 但**不占槽位、不建任务、不推送**。这正好是"试运行"要的信息。
	if in.DryRun {
		for _, c := range cands {
			// 预览里带**解析后**的参数：用户改完模板点"试运行"，最想知道的
			// 就是「{{x}} 到底取到了什么」。用的正是真派发那份 c.params
			// （求值/校验已经做完），不在这里重新解析一次——否则预览与真跑
			// 会分叉成两套判定。
			paramsJSON, err := marshalParams(c.params)
			if err != nil {
				return nil, nil, fmt.Errorf("route: marshal params: %w", err)
			}
			out.Tasks = append(out.Tasks, &store.Task{
				CapabilityID: c.cap.ID,
				Capability:   c.cap.Name,
				Version:      c.cap.Version,
				Params:       paramsJSON,
				Status:       store.TaskPending,
			})
		}
		return nil, nil, nil
	}

	// 构造 pending 任务：只算字段（ID 在这里生成），**不落库**。
	// marshalParams 会把空/null 归一成 {}，避免下游把 nil 当成"没有参数"。
	tasks = make([]*store.Task, 0, len(cands))
	for _, c := range cands {
		paramsJSON, err := marshalParams(c.params)
		if err != nil {
			return nil, nil, fmt.Errorf("route: marshal params: %w", err)
		}
		// path 存**能力名**（P2.2）：它是 hop 预算的输入（depth = len(path)），
		// 也是事后看「这条链是怎么走到这里的」的唯一依据 —— 名字比 id 可读，
		// 而且 `analyze@1` / `analyze@2` 在链上是同一个名字，深度才是真正的判据。
		path := append(append([]string{}, in.Path...), c.cap.Name)
		task := &store.Task{
			ID:           store.NewTaskID(),
			NamespaceID:  in.NamespaceID, // ⑤ 根任务用入参；子任务由 store I11④ 强制继承父任务
			TraceID:      in.TraceID,
			ParentTaskID: parentPtr(in.ParentTaskID),
			Path:         path,
			CapabilityID: c.cap.ID,
			Capability:   c.cap.Name,
			Version:      c.cap.Version,
			Params:       paramsJSON,
			Status:       store.TaskPending,
			CapHash:      c.cap.ContentHash,
			Digest:       d.pinDigest(ctx, in.NamespaceID, c.cap.ID),
			GraphID:      graph.ID,
		}
		tasks = append(tasks, task)
	}

	return tasks, cands, nil
}

// capabilityIDPrefix 是 capability_versions.id 的固定前缀（契约 §2.2：`cap_<秒>_<hash8>_<随机>`）。
const capabilityIDPrefix = "cap_"

// upstreamKey 把接力派发的上游标识归一成**能力名**（P2.2：link 的左端是名字）。
//
// 为什么需要兼容：ws 的接力派发仍按老形状传 `task.CapabilityID`（那一面由
// 另一个 worker 负责，本波不动它）。id 的形状是契约定死的 `cap_` 前缀，
// 所以先用前缀做一次**零成本**判定，只有真的像 id 才去查一次库；
// 万一有个能力名字恰好以 `cap_` 开头，查不到就按名字原样用（不会解析到别处）。
func (d *Dispatcher) upstreamKey(ctx context.Context, namespaceID, key string) (string, error) {
	if !strings.HasPrefix(key, capabilityIDPrefix) {
		return key, nil
	}
	c, err := d.Store.GetCapabilityByID(ctx, namespaceID, key)
	if errors.Is(err, store.ErrNotFound) {
		return key, nil
	}
	if err != nil {
		return "", fmt.Errorf("route: resolve upstream %s: %w", key, err)
	}
	return c.Name, nil
}

// refCache 是**一次 plan** 内的引用解析缓存（不跨派发复用）。
//
// 为什么需要：扇出时同一条 link 目标可能被多条边引用（1:N 扇出里同一个下游接到
// 两个出口上），解析两次就是两次往返。为什么不能跨派发：缓存跨派发等于把
// `name` 型 link 的 latest 钉死在第一次解析的那一刻——升级又永远不生效了
// （这正是 P2.2 要消灭的失败模式）。
type refCache map[string]*store.CapabilityVersion

func newRefCache() refCache { return make(refCache, 4) }

// resolveRef 是派发侧解析 link 引用的**唯一**入口（store.ResolveRef 的带缓存包装）。
//
// 解析失败（能力被删 / 版本不存在）返回 error，由调用方只剪那一条边。
func (d *Dispatcher) resolveRef(ctx context.Context, namespaceID, ref string, cache refCache) (*store.CapabilityVersion, error) {
	if c, ok := cache[ref]; ok && c != nil {
		return c, nil
	}
	c, err := d.Store.ResolveRef(ctx, namespaceID, ref)
	if err != nil {
		return nil, err
	}
	cache[ref] = c
	return c, nil
}

// narrowTargetsTo 把**边**（不是已经过完校验的候选）收敛到脚本点名的那一个（emit --to）。
//
// 为什么在参数/节点校验之前做：`--to` 说的是「这次只发给它」，其余候选不参与本次
// 判定。放在校验之后，没被点名的边会各自留下一条 `pruned: param_schema` —— 那是
// 用户从没打算发的边，trace 里凭空多出一条“失败”，最容易被当成“这条链挂了”。
//
// 匹配规则：`name` 匹配该能力名（不论版本，取这条边实际指向的版本）；`name@ver`
// 精确匹配那个版本。**不用 id 匹配**：id 会随升级变化，而脚本里写的是名字（与 link
// 的引用语义一致），这也正是节点侧 CLI resolveEmitPort 的口径 —— 两边判据不同会
// 得到「节点放行、中枢说不是候选」这种两边都看不出自己错在哪的失败。
//
// 一条都没匹配上时返回 ErrEmitToNotCandidate，并列出**本出口的候选**：脚本写错名字
// 要当场说清「可选的其实有这些」，不能默默发 0 条（那会变成静默丢产出）。
//
// 解析不到的边这一步**不剪枝**：剪枝与留痕统一由主循环做（理由只有一处），这里只
// 负责回答「谁是我点名的那个」。
func (d *Dispatcher) narrowTargetsTo(ctx context.Context, namespaceID string, targets []store.LinkTarget, to string, cache refCache) ([]store.LinkTarget, error) {
	name, ver := to, 0
	if at := strings.LastIndex(to, "@"); at > 0 {
		name = to[:at]
		if n, err := strconv.Atoi(to[at+1:]); err == nil {
			ver = n
		}
	}
	kept := make([]store.LinkTarget, 0, 1)
	names := make([]string, 0, len(targets))
	for _, tgt := range targets {
		c, err := d.resolveRef(ctx, namespaceID, tgt.ToRef, cache)
		if err != nil {
			continue
		}
		names = append(names, fmt.Sprintf("%s@%d", c.Name, c.Version))
		if c.Name != name {
			continue
		}
		if ver != 0 && c.Version != ver {
			continue
		}
		kept = append(kept, tgt)
	}
	if len(kept) == 0 {
		sort.Strings(names)
		return nil, fmt.Errorf("%w: %s（本出口的候选：%s）",
			ErrEmitToNotCandidate, to, strings.Join(names, ", "))
	}
	return kept, nil
}

// ---------- 候选选择（契约 §3） ----------

// cursorKey 是同级轮盘游标的键：一个 (命名空间, 上游能力, 端口, priority) 一个游标。
//
// 为什么 priority 也要进键：不同的 priority 组是**不同的轮盘**。让它们共用一个
// 下标，低优先级组的长度就会带偏高优先级组的轮换（只在「高优先级全都不可达、
// 落到低优先级组」时才看得出来，是最难查的那类偏差）。
type cursorKey struct {
	namespaceID string
	capability  string
	port        string
	priority    int
}

// pickCandidate 从可达候选里选一个（契约 §3 的第 2–4 步）。
//
// 顺序就是语义：
//  1. 不可达的候选已经在 plan 里逐条剪掉了（各有自己的 reason），这里不重复判；
//  2. 剩余按 **to_ref 字典序**（保证确定），再按 **priority 升序**；
//  3. 取**最小 priority 组**（同 priority 的整组）；
//  4. 组内**轮盘**选一个（游标见 cursorKey / rotate）。
//
// 为什么组内顺序必须是确定顺序：游标只是一个下标，「+1」在两个进程里若指向
// 不同的候选，就不叫轮盘、叫随机——而随机是另一种模式（预留的 random），
// 不是 priority 的实现方式。

func (d *Dispatcher) pickCandidate(namespaceID, capability, port string, cands []candidate) candidate {
	sorted := make([]candidate, len(cands))
	copy(sorted, cands)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].priority != sorted[j].priority {
			return sorted[i].priority < sorted[j].priority
		}
		return sorted[i].toRef < sorted[j].toRef
	})
	// 排序后最小 priority 组就在前面，长度按 priority 相等的连续段算。
	n := 0
	for n < len(sorted) && sorted[n].priority == sorted[0].priority {
		n++
	}
	group := sorted[:n]
	return group[d.rotate(cursorKey{
		namespaceID: namespaceID,
		capability:  capability,
		port:        port,
		priority:    sorted[0].priority,
	}, len(group))]
}

// rotate 返回该游标的下一个下标并推进它（进程内，不持久化）。
//
// 首次使用随机初始化：不然每个进程都从候选 0 开始，「轮盘」就退化成
// 「所有节点都优先打第一个下游」——那是负载最不均衡的一种形状。
// 只有一个候选时不占游标：随机初始化一个没意义的下标只会让「后来多了一个
// 候选」的那一刻从一个奇怪的起点开始。
func (d *Dispatcher) rotate(key cursorKey, n int) int {
	if n <= 1 {
		return 0
	}
	d.cursorMu.Lock()
	defer d.cursorMu.Unlock()
	if d.cursor == nil {
		d.cursor = make(map[cursorKey]int, 8)
	}
	idx, ok := d.cursor[key]
	if !ok {
		idx = rand.IntN(n)
	}
	// 组长度可能变（加了/删了一条边）：取模保证下标永远有效。
	d.cursor[key] = (idx + 1) % n
	return idx % n
}

// portBalanceMode 取上游能力在这个端口上的 balance.mode（缺省 = priority）。
//
// 入口边没有上游能力（来源不是能力定义），一律 priority：源的「全派」不是一个
// 端口级配置，没有对应概念。解析不到上游能力（能力刚被删 / 名字不是有效引用）
// 也回落到 priority：balance 只决定「多个候选里选几个」，缺了它按 1:1 走是安全方向。
func (d *Dispatcher) portBalanceMode(ctx context.Context, in DispatchInput, fromKey, fromPort string, cache refCache) string {
	if in.FromIsSource {
		return store.BalanceModePriority
	}
	up, err := d.resolveRef(ctx, in.NamespaceID, fromKey, cache)
	if err != nil || up == nil {
		return store.BalanceModePriority
	}
	return up.BalanceModeOf(fromPort)
}

// balanceWarnOnce 让「未实现的 balance 模式」在进程里只 WARN 一次。
//
// 每次派发都打会把这个 WARN 变成噪音（热路径上每个任务一条），而它表达的是
// 一个**部署级**事实（库里有本版本不认识的配置）。
var balanceWarnOnce sync.Once

// warnUnsupportedBalanceMode 记录一次「库里有未实现的 balance 模式」。
//
// 保存路径已经在 store 层拒掉这些值（ErrBalanceRejected → 400），所以走到这里
// 只可能是手改库。按 priority 处理（1:1），**绝不扇出**：没实现的语义不能靠猜。
func warnUnsupportedBalanceMode(namespaceID, capability, port, mode string) {
	balanceWarnOnce.Do(func() {
		slog.Warn("balance mode not implemented, falling back to priority",
			"event", "route.balance.unsupported_mode",
			"namespace_id", namespaceID,
			"capability", capability,
			"port", port,
			"mode", mode,
			"note", "本期只实现 priority；该端口的候选按最小 priority 组轮盘选一个（不扇出）",
		)
	})
}

// formatPath 把 path 渲染成给人看的箭头链（剪枝 Detail 用）。
func formatPath(path []string) string {
	if len(path) == 0 {
		return "[]"
	}
	return strings.Join(path, " → ")
}

// pinDigest 取「这次派发要钉在任务行上的产物内容 digest」（P0-a）。
//
// 取不到就返回空串，**不是错误**：
//   - 中枢执行的内置能力（notify.phone 之类）没有节点产物；
//   - 纯配置能力 / 刚建好还没登记产物的能力也没有。
//
// 这里刻意不让任何错误外溢（包括 DB 错误）：digest 是**审计字段**，
// 钉不住最多是「事后查不到哪份字节」；而把整次派发停下来的代价是这条链直接丢。
// 两者不在一个量级，所以宁可降级为不钉（行为与加这一列之前完全一致）。
func (d *Dispatcher) pinDigest(ctx context.Context, namespaceID, capabilityID string) string {
	a, err := d.Store.LatestArtifact(ctx, namespaceID, capabilityID)
	if err != nil || a == nil {
		return ""
	}
	return a.Digest
}

// launch 是派发的后半段：④ 挑机器/占槽位 + ⑤ 推 assign 帧，最后算出 out.Tasks。
//
// 入参 tasks 必须**已落库**（Dispatch 里由 CreateTasks 完成），且与 cands 一一对应。
// 返回值语义与拆分前完全一致：
//   - 容量不足留在 pending 的 → **出现在 out.Tasks 里**（任务确实建出来了，在等调度）；
//   - 推帧失败被退回 pending 的 → **不出现在 out.Tasks 里**（它没派出去，是回滚）。
func (d *Dispatcher) launch(ctx context.Context, in DispatchInput, tasks []*store.Task, cands []candidate, out *DispatchOutput) error {
	published := make([]bool, len(tasks))
	centralTasks := make([]*store.Task, 0)
	for i, task := range tasks {
		c := cands[i]

		// 中枢执行的候选：不挑节点、不占槽位、不设租约。
		// 任务保持 pending，由中枢执行器（internal/executor）领取并推进状态机。
		// 用 pending 而不是新状态：九态是契约定的，不能为内部实现加状态。
		if c.central {
			centralTasks = append(centralTasks, task)
			continue
		}

		// 挑一台：粘性优先，否则负载最小
		nodeID, stickySelector, err := d.pickNode(ctx, in, c.cap, c.params, c.nodes)
		if err != nil {
			return err
		}
		// 租约必须在 LeaseAndAssign **之前**算好：占槽位 / 改状态 / 写归属是一个事务，
		// lease_until 是它的入参（#141）。
		lease := d.Now() + store.LeaseTTLSec
		// 派发后 20s 内必须 ack，所以初始租约取 min(TTL, AckWait)——
		// 等 ack 的是 20s 窗口，ack 之后才开始按心跳续 30s（契约 §3.3）。
		if ackLease := d.Now() + store.AckWaitSec; ackLease < lease {
			lease = ackLease
		}
		// 占槽位 + pending→assigned + 写归属/attempts = **一次事务**（#141）。
		// 任何一步失败都回滚，槽位不会泄漏——所以这里**没有**（也不需要）
		// 失败后补一次 ReleaseInstance 的语句。
		instanceID, err := d.Store.LeaseAndAssign(ctx, in.NamespaceID, task.ID, nodeID, lease)
		if errors.Is(err, store.ErrNoCapacity) {
			// 有节点声明但暂时没空：不是永久剪枝，任务留 pending 等调度。
			// （剪枝语义是「这条链在这里断了」，容量不够不是断链。）
			continue
		}
		if err != nil {
			return fmt.Errorf("route: lease+assign on %s: %w", nodeID, err)
		}
		published[i] = true // 成功占用槽位并派发
		// 读回派发后的状态，让调用方拿到最新值
		task.Status = store.TaskAssigned
		task.NodeID = &nodeID
		task.InstanceID = &instanceID
		task.LeaseUntil = &lease
		task.Attempts = 1

		// 推 assign 帧。**必须在 LeaseAndAssign 之后**：节点收到帧就会开始干活，
		// 若此时库里还不是 assigned，它回报的 ack/result 会因为状态不匹配被拒。
		//
		// 推送失败（节点掉线/连接忙）时要把任务**退回 pending**并释放槽位：
		// 否则库里的状态是 assigned、节点却根本不知道有活干，
		// 这条任务会一直卡到租约过期才被巡检回收（30s 起，重试还要再来一轮）。
		// 退回 pending 让它能被下一次派发立刻捡起。
		if d.Publisher != nil {
			if err := d.Publisher.PublishAssign(ctx, in.NamespaceID, nodeID, task); err != nil {
				d.requeueAfterPublishFailure(ctx, in.NamespaceID, task, nodeID, instanceID)
				// 从返回值里剔除：它已退回 pending，不算"派发出去了"。
				// （仍留在库里，会被后续调度捡起。）
				published[i] = false
				continue
			}
		}

		// 粘性钉住：能力开了 sticky 且是第一次落到这台机器时写 pin
		// （pin 钉的是「哪个命名空间里的哪台机器」，所以带 NamespaceID）。
		if stickySelector != nil {
			key := store.StickyKey(in.TraceID, c.cap.ID, *stickySelector)
			if err := d.Store.PutPin(ctx, &store.NodePin{
				StickyKey:   key,
				NamespaceID: in.NamespaceID,
				NodeID:      nodeID,
				Selector:    *stickySelector,
			}); err != nil {
				return fmt.Errorf("route: put pin: %w", err)
			}
		}
	}

	// 返回值语义：
	//   - 容量不足留在 pending 的 → **要返回**（任务确实建出来了，在等调度；
	//     隐藏它会让调用方以为"这个出口什么都没触发"）
	//   - 推送失败被退回 pending 的 → **不返回**（它没派出去，是回滚）
	//
	// 两者在库里都是 pending，区别只在"这次派发有没有成功"。
	out.Tasks = make([]*store.Task, 0, len(tasks))
	for i, t := range tasks {
		if !published[i] && t.Status == store.TaskPending && t.Attempts > 0 {
			continue // 推送失败回滚的那条（attempts 已被 LeaseAndAssign +1）
		}
		out.Tasks = append(out.Tasks, t)
	}
	return nil
}

// formatParamMissing 组装 param_missing 的 detail。
//
// 一条目标一条记录，多个未命中用「；」连接；Ref 已由 ResolveParams 渲染成
// {{x}} / payload.a.b / $var 三种形态，据此区分「payload 结构不对」与
// 「变量/别名的出处不对」：前者是 payload 里没这个字段，后者是变量没声明
// 或它指向的字段不在 payload 里——两种病因完全不同，混成一句话就查不下去了。
//
// 文案逐字照契约 §2.2，前缀挂能力名，与 param_schema 的
// `%s@%d params_schema: %s` 风格一致。capDef 可能为 nil（测试/异常路径），
// 此时只省掉前缀，不 panic。
//
// 前缀**不能省**：扇出时一条 trace 里多个目标各自求值，没有前缀就答不出
// 「是哪个目标的参数出了问题」。所以 dispatch_test.go 里除了按子串断言正文，
// 还有一条**等值**断言把 `能力名@版本 ` 前缀一起钉死（契约 §8.1 A2）。
func formatParamMissing(capDef *store.CapabilityVersion, misses []ParamMiss) string {
	parts := make([]string, 0, len(misses))
	for _, m := range misses {
		if strings.HasPrefix(m.Ref, "$") {
			// {var:"name"}：显式引用变量，**只**查别名表（不会回退 payload 路径）
			parts = append(parts, fmt.Sprintf(
				"参数 %s 取不到变量 %s（源未声明该变量，或它指向的字段在 payload 里没有）",
				m.Field, m.Ref))
			continue
		}
		// {{x}} / {from:"a.b"}：Ref 已经是 {{x}} 或 payload.a.b
		parts = append(parts, fmt.Sprintf(
			"参数 %s 取不到 %s（payload 里没有这个字段）", m.Field, m.Ref))
	}
	return capDetailPrefix(capDef) + strings.Join(parts, "；")
}

// formatParamType 组装 param_type 的 detail（契约 §2.2）。
//
// Got 是**转换前**的原值，用 JSON 表示：`"abc"` 带引号才能和字符串 `abc`
// 之外的类型（数字/布尔/对象）区分开；Marshal 失败（不可达）退回 %v。
func formatParamType(capDef *store.CapabilityVersion, e ParamTypeErr) string {
	got := fmt.Sprintf("%v", e.Got)
	if b, err := json.Marshal(e.Got); err == nil {
		got = string(b)
	}
	return capDetailPrefix(capDef) + fmt.Sprintf(
		"参数 %s 期望 %s，但求值得到 %s，转不了", e.Field, e.Want, got)
}

// capDetailPrefix 是剪枝 detail 的能力前缀（`name@version `）。
//
// 有了它，一条多出口扇出的剪枝记录才能回答「是哪个目标的参数出了问题」；
// capDef 为 nil 时返回空串（宁可少一段前缀，也不能 panic 在派发热路径上）。
func capDetailPrefix(capDef *store.CapabilityVersion) string {
	if capDef == nil {
		return ""
	}
	return fmt.Sprintf("%s@%d ", capDef.Name, capDef.Version)
}

// marshalParams 把一份参数序列化成任务里的 params（null/空 → "{}"）。
//
// 真派发与 DryRun 预览**共用**它：否则预览里的参数形状（null vs {}）
// 会和真跑的不一样，白让用户怀疑一次。
func marshalParams(params map[string]any) (json.RawMessage, error) {
	b, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 || string(b) == "null" {
		b = []byte("{}")
	}
	return json.RawMessage(b), nil
}

// prune 记录一条剪枝：先进返回值，再留痕落库（#142）。
//
// 唯一入口——「产生」与「留痕」不允许分叉（漏一处就是又一个查不到的剪枝），
// 所以 plan 里 9 个剪枝产生点（no_edge / max_hops / budget / param_missing /
// param_type / param_schema / foreign_user / no_node，外加解析失败与坏 literals
// 这两条归到 param_schema 的路径）全部走这里，不再直接 append out.Pruned。
//
// 三条规则（契约 §4.3）：
//
//  1. 返回值语义不变：out.Pruned 照旧给 HTTP 响应（调用方当场就要看到）；
//  2. DryRun 不落库：试运行不能污染 events（否则用户每试一次就多一条假历史）；
//  3. 落库失败只记日志，**不向上抛错**——剪枝留痕是诊断，
//     不能反过来打断派发/建任务。
func (d *Dispatcher) prune(ctx context.Context, in DispatchInput, out *DispatchOutput, p PrunedRecord) {
	out.Pruned = append(out.Pruned, p)

	if in.DryRun {
		return
	}

	payload, err := json.Marshal(PruneEventPayload{
		FromCapability:   in.upstream(),
		Port:             p.Port,
		TargetCapability: p.CapabilityID,
		Detail:           p.Detail,
	})
	if err != nil {
		// 四个字段全是 string，理论上不可达；保底不 panic、不中断派发。
		slog.Warn("marshal prune event payload failed",
			"event", "route.prune.record_failed",
			"reason", string(p.Reason),
			"trace_id", in.TraceID,
			"error", err.Error())
		return
	}

	// 幂等/去重由 store 层负责（同 namespace + 同 trace + 同原因 + 同 payload 只落一行）。
	// NamespaceID 必填：events 是命名空间资源，缺它 store 会直接报 ErrNamespaceRequired，
	// 于是剪枝会「静默地写失败」——那就又把 #142 要消灭的「查不到的剪枝」还回来了。
	if _, err := d.Store.InsertPruneEvent(ctx, &store.Event{
		NamespaceID: in.NamespaceID,
		TraceID:     in.TraceID,
		Source:      PruneEventSource,
		Type:        PruneEventType(p.Reason),
		Payload:     payload,
		CreatedAt:   d.now(),
	}); err != nil {
		slog.Warn("record prune event failed",
			"event", "route.prune.record_failed",
			"reason", string(p.Reason),
			"trace_id", in.TraceID,
			"capability_id", p.CapabilityID,
			"error", err.Error())
	}
}

// requeueAfterPublishFailure 把「已 assigned 但推不出去」的任务退回 pending。
//
// 关键是**复用租约巡检**而不是自己拼一套回滚：
//
//  1. ReleaseInstance 释放派发时占住的槽位
//  2. ExtendLease(…, 0) 把租约推到"已过期"
//  3. SweepExpiredLeases 走同一条回收路径（改状态 + 清归属 + 释放槽位）
//
// 这样"清理任务归属"的逻辑只存在一处（sweepOne），不会出现第二份实现。
// 失败只记日志：最坏情况是等 30s 后巡检自然兜底，结果一样。
func (d *Dispatcher) requeueAfterPublishFailure(ctx context.Context, namespaceID string, task *store.Task, nodeID, instanceID string) {
	if err := d.Store.ReleaseInstance(ctx, nodeID, instanceID); err != nil {
		slog.Warn("release instance after publish failure failed",
			"event", "route.publish.requeue",
			"task_id", task.ID, "node_id", nodeID, "error", err.Error())
	}
	// 把租约推到过去，然后走**同一条**巡检路径回收。
	//
	// 为什么不直接 Transition：租约巡检除了改状态，还要清空
	// node_id/instance_id/lease_until 并释放实例槽位。自己再写一遍
	// 就等于出现第二个改归属的地方，且容易漏步骤（漏了 node_id 会让
	// 下次派发以为任务还挂在旧节点上）。
	//
	// 代价是这中间有极短的时间窗任务仍是 assigned（租约已过期），
	// 但巡检的 CAS 保证只有一个执行者能改它。
	if err := d.Store.ExtendLease(ctx, namespaceID, task.ID, 0); err != nil {
		slog.Warn("expire lease after publish failure failed",
			"event", "route.publish.requeue",
			"task_id", task.ID, "error", err.Error())
		return
	}
	if _, err := d.Store.SweepExpiredLeases(ctx, d.now()); err != nil {
		slog.Warn("sweep after publish failure failed",
			"event", "route.publish.requeue",
			"task_id", task.ID, "error", err.Error())
		return
	}
	task.Status = store.TaskPending
	task.NodeID = nil
	task.InstanceID = nil
	task.LeaseUntil = nil
}

// now 返回当前 unixepoch 秒。
func (d *Dispatcher) now() int64 {
	if d.Now != nil {
		return d.Now()
	}
	return store.Now()
}

// checkUserScoped 校验 params.to 指向的用户在本部署里存在（契约 §10.1.1）。
//
// **#204 之后判据变了（明确写在这里以免误读）**：组织层删除，命名空间成为唯一
// 隔离边界，本部署任何已登录用户都能访问任何存在的命名空间 —— 所以「把消息推给
// 外组织用户」这个威胁不存在了。这条规则仍然有意义，只是收窄成「收件人得是个
// 真人」：推给一个不存在的 userID，任务一定发不出去、而且会静默消失，不如在派发
// 时就剪掉并留痕（PruneReason 仍叫 foreign_user，wire 值不变）。
//
// 返回的 error 必须由调用方**继续往上抛**：查库失败时既不能判"用户不存在"
// （那是把故障当成攻击，消息静默消失），也不能判"通过"（那是 fail open）。
// 以前这里只返回 (bool, PrunedRecord)，注释写着"向上抛"却无处可抛 ——
// 于是查库失败等价于放行。
func (d *Dispatcher) checkUserScoped(ctx context.Context, capDef *store.CapabilityVersion, params map[string]any) (bool, PrunedRecord, error) {
	if !d.UserScoped[capDef.Name] {
		return false, PrunedRecord{}, nil
	}
	to, _ := params["to"].(string)
	if to == "" {
		// schema 已经要求 to 非空；走到这里说明 schema 没声明要求，
		// 保守处理：没有目标用户就不能推（不然会变成广播）。
		return true, PrunedRecord{
			Reason:       ReasonForeignUser,
			CapabilityID: capDef.ID,
			Detail:       fmt.Sprintf("%s requires a non-empty params.to", capDef.Name),
		}, nil
	}
	if _, err := d.Store.GetUserByID(to); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return true, PrunedRecord{
				Reason:       ReasonForeignUser,
				CapabilityID: capDef.ID,
				Detail:       fmt.Sprintf("params.to=%q does not exist in this deployment", to),
			}, nil
		}
		// 查库失败：既不能剪枝（会静默丢消息），也不能放行
		return false, PrunedRecord{}, fmt.Errorf("route: check user %q: %w", to, err)
	}
	return false, PrunedRecord{}, nil
}

// pickNode 挑一台节点：粘性钉住优先，否则按负载（空闲槽位最多）挑。
//
// 返回 (nodeID, stickySelector, error)。stickySelector 非 nil 表示该能力开了 sticky，
// 调用方应写 pin。粘性命中时沿用钉住的那台（哪怕负载更高）。
func (d *Dispatcher) pickNode(ctx context.Context, in DispatchInput, capDef *store.CapabilityVersion, params map[string]any, candidates []string) (string, *string, error) {
	var selector *string
	if capDef.Sticky != "" {
		// 用**该目标实际的参数**算粘性选择值：多目标各有各的 params，
		// 拿公共 Params 会算出错的 key，钉到不该钉的节点上。
		sel := stickySelectorValue(capDef, params)
		selector = &sel
		key := store.StickyKey(in.TraceID, capDef.ID, sel)
		pin, err := d.Store.GetPin(ctx, key)
		if err == nil && pin != nil && containsStr(candidates, pin.NodeID) {
			if err := d.Store.PutPin(ctx, &store.NodePin{
				StickyKey:   key,
				NamespaceID: pin.NamespaceID, // 刷新沿用库里那条 pin 的命名空间
				NodeID:      pin.NodeID,
				Selector:    pin.Selector,
				CreatedAt:   pin.CreatedAt,
				LastUsedAt:  d.Now(),
			}); err != nil {
				return "", nil, fmt.Errorf("route: refresh pin: %w", err)
			}
			return pin.NodeID, nil, nil // 已钉住，不再重复写
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return "", nil, fmt.Errorf("route: read pin: %w", err)
		}
	}

	// 负载最小：空闲（alive 且 busy_task IS NULL）槽位最多的那台
	//
	// 一次聚合查询查完所有候选，而不是每个节点查一次 ListInstances：
	// 这段在**派发热路径**上，每个任务都要付一次（接力派发还会再乘 T 次）。
	// 节点数一多，N+1 就是吞吐的主要瓶颈，也是当前 node 数量上限的真正原因（#145）。
	freeSlots, err := d.Store.FreeSlotsByNode(ctx, candidates)
	if err != nil {
		return "", nil, fmt.Errorf("route: free slots by node: %w", err)
	}
	best := ""
	bestFree := -1
	for _, nodeID := range candidates {
		// 完全没有槽位申报的节点也算候选（它可能会在下一次 hello 申报），
		// 但优先级最低（free=0 已经体现了这一点）。
		if free := freeSlots[nodeID]; free > bestFree {
			best, bestFree = nodeID, free
		}
	}
	if best == "" {
		best = candidates[0]
	}
	return best, selector, nil
}

// stickySelectorValue 取粘性选择器的值：sticky 字段通常就是 params 里那个键名
// （如 sticky="worktree" → params["worktree"]），没有则用空串（单一桶）。
func stickySelectorValue(capDef *store.CapabilityVersion, params map[string]any) string {
	if v, ok := params[capDef.Sticky]; ok {
		return fmt.Sprintf("%v", v)
	}
	return ""
}

// ValidateParams 用 params_schema 校验参数，返回不合法原因（"" = 通过）。
//
// 导出供控制台在建源/改源时**立即**校验源配置的参数：
// 用户填完表单就该知道填错了，而不是等触发时被剪枝才发现。
// 与派发期用的是同一个函数——两边口径必须一致，否则会出现
// "建源说没问题、触发被剪枝"这种最让人困惑的情况。
func ValidateParams(schema json.RawMessage, params map[string]any) string {
	return validateParams(schema, params)
}

// validateParams 用 params_schema 校验参数，返回不合法原因（"" = 通过）。
func validateParams(schema json.RawMessage, params map[string]any) string {
	if len(schema) == 0 {
		return ""
	}
	var root map[string]any
	if err := json.Unmarshal(schema, &root); err != nil {
		// schema 本身坏了：拒绝派发（不能拿坏 schema 放行任何参数）
		return "invalid params_schema in capability definition"
	}
	if len(root) == 0 {
		return ""
	}
	if params == nil {
		params = map[string]any{}
	}
	return validateValue(root, params, "params")
}

// ---------- 小工具 ----------

// decodeEdgeMapping 把一条入口边上的 mapping（JSON 对象字节）解成参数模板。
//
// 三种输入都是「没有模板」：空字节（老库）、JSON null、空对象 → 返回空 map
// （ResolveParams 对空模板返回空 map，调用方不必再判空）。
//
// 坏字节返回 error，由调用方**只剪这一条边**（fail closed，与 literals 同口径）：
// 配置期已经拒过一份坏模板（store.checkEdgeMapping），能走到这里只能是手改库。
func decodeEdgeMapping(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if m == nil {
		return map[string]any{}, nil
	}
	return m, nil
}

// applyLiterals 把一条边的字面量覆盖叠到目标参数上（P0-c，设计 §3.3）。
//
// 语义只有一条：**字面量优先**（同名键由 literals 覆盖），**不做任何类型转换** ——
// 因此 data 里同名字段的类型必须与下游 params_schema 一致（否则调用方的
// validateParams 会把它剪成 param_schema，这正是派发期那道校验的职责）。
//
// 两个必须守住的细节：
//   - 返回的是**新 map**：in.Params 会被同一轮的所有边共用，就地改会让 A 边的常量
//     漏进 B 边的参数里（扇出时最隐蔽的一类串参数）。
//   - 空字面量（nil / {}）原样返回入参：没有覆盖的边行为必须与加这一列之前一模一样。
func applyLiterals(params map[string]any, literals json.RawMessage) (map[string]any, error) {
	if len(literals) == 0 {
		return params, nil
	}
	var raw any
	if err := json.Unmarshal(literals, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if raw == nil {
		return params, nil // JSON null = 没有覆盖（与 store 的配置期口径一致）
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("want a JSON object, got %s", store.JSONTypeName(raw))
	}
	if len(obj) == 0 {
		return params, nil
	}
	merged := make(map[string]any, len(params)+len(obj))
	for k, v := range params {
		merged[k] = v
	}
	for k, v := range obj {
		merged[k] = v
	}
	return merged, nil
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func parentPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// RedispatchTask 尝试把一条已存在的 pending 任务重新派给节点。
//
// 与 Dispatch 的区别：Dispatch 是「从出口表算出要建哪些任务」，
// 本方法是「任务已经建好了，只是还没找到机器」——典型场景是租约过期后
// 被巡检退回 pending，或派发时容量不足。
//
// 不重新走 ①②（出口表/参数校验）：那些在建模时就过了，重走一遍只会
// 因为图变了而产生不一致（任务已存在，却不该存在——那是另一个问题）。
//
// 返回 (true, nil) 表示派发出去了；false 表示没派成（没有可用节点、
// 容量不足），调用方应保持 pending 并在退避后重试。
func (d *Dispatcher) RedispatchTask(ctx context.Context, task *store.Task) (bool, error) {
	if task == nil {
		return false, fmt.Errorf("route: redispatch nil task")
	}
	if task.Status != store.TaskPending {
		// 不是 pending 说明已被别人处理（派发/取消/回收），不是错误。
		return false, nil
	}
	// 重派也必须知道隔离维度（它决定节点列表与租约落在哪个命名空间）。
	// 库里不存在这种行（tasks.namespace_id 是必填）；真出现只能是调用方自己拼了一个，
	// 那宁可报错也不要拿空 namespace 去查：查不到节点会让任务默默卡在 pending。
	if task.NamespaceID == "" {
		return false, fmt.Errorf("route: redispatch task %s: %w", task.ID, store.ErrNamespaceRequired)
	}

	nodes, err := d.Store.NodesForCapability(ctx, task.NamespaceID, task.CapabilityID)
	if err != nil {
		return false, fmt.Errorf("route: redispatch nodes for %s: %w", task.CapabilityID, err)
	}
	if len(nodes) == 0 {
		// 没有节点声明这个能力。不是错误——节点可能临时全下线，
		// 保持 pending，等节点回来再派。
		return false, nil
	}

	// 复用派发第 ④ 步的挑机器逻辑（粘性 + 负载）。
	capDef, err := d.Store.GetCapabilityByID(ctx, task.NamespaceID, task.CapabilityID)
	if err != nil {
		return false, fmt.Errorf("route: redispatch load capability %s: %w", task.CapabilityID, err)
	}
	// 重派时用**任务自己存的 params**算粘性（不能空着，否则粘性形同虚设）
	var taskParams map[string]any
	if len(task.Params) > 0 {
		_ = json.Unmarshal(task.Params, &taskParams)
	}
	// stickySelector 非 nil 表示「该能力开了 sticky 且库里还没钉住」，派发成功后**必须**写 pin。
	// 不能像以前那样用 `_` 丢掉：接力派发（#140）不再经过 launch，这里是它唯一的
	// 写 pin 的地方（见下方 PutPin 段）。
	// 粘性/负载与命名空间无关（派发第 ④ 步不变），但 pin 要能追溯归属，
	// 所以把任务自己的 namespace 带进去。
	nodeID, stickySelector, err := d.pickNode(ctx, DispatchInput{NamespaceID: task.NamespaceID, TraceID: task.TraceID}, capDef, taskParams, nodes)
	if err != nil {
		return false, err
	}

	// 租约在调用前算好（LeaseAndAssign 的入参），算法与 Dispatch 一致。
	lease := d.now() + store.LeaseTTLSec
	if ackLease := d.now() + store.AckWaitSec; ackLease < lease {
		lease = ackLease
	}
	// 占槽位 + pending→assigned + 写归属/attempts = 一次事务（#141）。
	// 失败即回滚，槽位不会泄漏，所以不需要再调 ReleaseInstance。
	instanceID, err := d.Store.LeaseAndAssign(ctx, task.NamespaceID, task.ID, nodeID, lease)
	if errors.Is(err, store.ErrNoCapacity) {
		// 有节点但满了：不是永久失败，下轮再试。
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("route: redispatch lease+assign: %w", err)
	}

	task.Status = store.TaskAssigned
	task.NodeID = &nodeID
	task.InstanceID = &instanceID
	task.LeaseUntil = &lease
	task.Attempts++
	// 派出去了，清掉退避时间（下次若再失败会重新设）
	task.NotBefore = nil

	if d.Publisher != nil {
		if err := d.Publisher.PublishAssign(ctx, task.NamespaceID, nodeID, task); err != nil {
			d.requeueAfterPublishFailure(ctx, task.NamespaceID, task, nodeID, instanceID)
			return false, nil // 推不出去 = 这次没派成，下轮重试
		}
	}

	// 粘性钉住：这是 launch 第 ④⑤ 步里 PutPin 那一段的**同构副本**。
	//
	// 为什么必须在这里再写一遍：重派（巡检捞回 / emit 的后台派发）走的是
	// RedispatchTask 而不是 launch，若不在这里接住 pickNode 返回的 selector，
	// 重派链上就永远不会有 pin——
	// 而 pickNode 只在「库里还没有 pin」时才返回 selector（已钉住返回 nil），
	// 于是 hop≥2 的 sticky 静默失效：第二次派发/巡检重派会按负载换机器，
	// 同一 trace 的同一 worktree 落到另一台节点上（对 sticky="worktree" 这类
	// 能力是结果可能不正确，不只是慢）。
	//
	// 位置与 launch 一致，两条都是硬要求：
	//   - 必须在 PublishAssign **成功之后**：上面那个 return false 的分支
	//     （推帧失败退回 pending）绝不能写 pin，否则重派会被钉死在一台
	//     刚刚推不出去的机器上；
	//   - Publisher == nil（只落库不推送）时也要写，与 launch 同口径。
	//
	// 写失败只记日志、不影响返回值：帧已经发出去了，这里报错也无法反悔；
	// 后果仅是这条 trace 的粘性退化为「按负载挑」（下一跳可能换机器），
	// 不影响任务本身已经派发成功的事实。
	if stickySelector != nil {
		key := store.StickyKey(task.TraceID, capDef.ID, *stickySelector)
		if err := d.Store.PutPin(ctx, &store.NodePin{
			StickyKey:   key,
			NamespaceID: task.NamespaceID,
			NodeID:      nodeID,
			Selector:    *stickySelector,
		}); err != nil {
			slog.Warn("put sticky pin after redispatch failed",
				"event", "route.redispatch.put_pin_failed",
				"namespace_id", task.NamespaceID,
				"task_id", task.ID,
				"trace_id", task.TraceID,
				"capability_id", task.CapabilityID,
				"node_id", nodeID,
				"error", err.Error())
		}
	}
	return true, nil
}
