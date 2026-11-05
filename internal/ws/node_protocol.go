package ws

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion 是本实现支持的节点协议版本（hello / welcome 帧里交换）。
//
// v2（console-declares）：hello 不再自报能力（capabilities/manifest 删除），
// 改为上报「本机缓存了哪些产物」；新增 deploy（下行）/ apply（上行）两帧。
const ProtocolVersion = 2

// 十三帧的帧类型名。字符串本身是协议的一部分，不能随意改。
//
// 节点 → 中枢：hello / heartbeat / ack / progress / result / apply / upgrade_ack
// 中枢 → 节点：welcome / assign / deploy / cancel / error / upgrade
const (
	// 节点 → 中枢
	NodeFrameHello     = "hello"
	NodeFrameHeartbeat = "heartbeat"
	NodeFrameAck       = "ack"
	NodeFrameProgress  = "progress"
	NodeFrameResult    = "result"
	NodeFrameApply     = "apply"
	// NodeFrameEmit 是运行中的一次产出上报（docs/design/emit-protocol.md §3.1）。
	//
	// 它是**唯一**的产出通道：一次性返回只是「只 emit 一次」的特例。
	// result 帧因此不再承担路由语义（**out 已退役**，见 ResultFrame）。
	NodeFrameEmit = "emit"

	// 中枢 → 节点
	NodeFrameWelcome = "welcome"
	NodeFrameAssign  = "assign"
	NodeFrameDeploy  = "deploy"
	NodeFrameCancel  = "cancel"
	NodeFrameError   = "error"
	// NodeFrameEmitAck 是对一次 emit 的确认（幂等：重复 seq 也回 ok）。
	NodeFrameEmitAck = "emit_ack"
	// NodeFrameUpgrade 让节点去下载并替换自己的二进制（CONTRACT-agent-upgrade §5）。
	//
	// 帧里**不带二进制**：15 MB 的帧会撑爆心跳与背压。节点收到后用自己的
	// node token 请求 UpgradeFrame.URLPath 下载，校验 sha256 后原子替换。
	NodeFrameUpgrade = "upgrade"
	// NodeFrameUpgradeAck 是节点对一次 upgrade 的回报（成功 / 失败 + 可判别短码）。
	NodeFrameUpgradeAck = "upgrade_ack"
)

// 错误码（契约 §6.1 表格）。
const (
	ErrCodeBadFrame          = "bad_frame"
	ErrCodeUnknownType       = "unknown_type"
	ErrCodeInvalidTransition = "invalid_transition"
	ErrCodeLeaseExpired      = "lease_expired"
	// ErrCodeProtocolMismatch 节点 speak 的协议版本与服务端不同。
	//
	// 与 bad_frame 分开的理由：这是**版本不兼容**，不是「帧写坏了」。节点/运维
	// 看到这个码应该去升级二进制，而不是去查 JSON 结构（契约 §1）。
	ErrCodeProtocolMismatch = "protocol_mismatch"
	// ErrCodeNodeDraining 节点在排空，不再接新任务。
	ErrCodeNodeDraining = "node_draining"
	// ErrCodeTooManyInstances 节点申报的实例数超过中枢允许的上限（契约 Q5）。
	ErrCodeTooManyInstances = "too_many_instances"
	// ErrCodeInvalidManifest 已随 hello.manifest 一起下线（v2 不再自报执行方式）。
	//
	// 部署产物不合法走 apply 帧的 reason 短码（见 ApplyFrame），不占用错误码空间：
	// 那是**部署**失败，不是**协议**错误。
	//
	// ErrCodeUnauthorized 握手鉴权失败（HTTP 层用，不会作为帧下发）。
	ErrCodeUnauthorized = "unauthorized"
	// ErrCodeFrameTooLarge 上行帧超过服务端软上限（#221）。
	//
	// 下发它是为了**可诊断**：没有它时 coder/websocket 会在硬上限处静默
	// 用 1009 关连接，节点侧只看到「连接断了」，不知道是自己的帧太大。
	// 节点收到这个码可以把大 result/artifacts 截断重发。
	ErrCodeFrameTooLarge = "frame_too_large"
	// ErrCodeParamsTooLarge 任务 params 超过 store.MaxTaskParamsBytes（#221 / 审查 m5）。
	//
	// 典型场景：一条**下一跳子任务**的 params 超限。store 侧的闸门在
	// createTasksTx 里，所以子任务被拒会让**父任务的终态整体回滚**（同一个事务）——
	// 父任务留在 running → 租约过期重派 → 节点把同一份昂贵工作再跑一遍 → 返回同样的
	// 超限子任务，直到 attempts 用尽变 abandoned。
	// 把这种错误映射成 invalid_transition 就把「你的 params 太大」这个病因埋了；
	// 给它一个独立的码，节点/运维一眼能看出是参数体积问题（应该是去分片或改用引用）。
	ErrCodeParamsTooLarge = "params_too_large"
)

// 上行帧（节点 → 中枢）──────────────────────────────

// HelloFrame 是节点连上后的第一帧：自报身份、协议版本、并发槽位、本机缓存了什么。
//
// # v2 的语义变化（console-declares，契约 D4）
//
// 旧版用 capabilities[] + manifest[] **自报能力**，中枢只能被动接受；
// v2 把它们删掉，改为 applied[]：节点陈述「我缓存了哪些产物」，
// 而「这个能力归谁」完全由中枢的部署决定。
//
// 差别很重要：能力归属从此只有一个写者（控制台），节点不能再单方面声称
// 「我会跑 X」——否则中枢数据库会静默地撒谎（页面显示能跑，实际跑到就 failed）。
//
// applied 里的能力必须已在中枢定义（否则 unknown_capability）。这条保留：
// 产物本身就是从中枢部署过去的，不可能出现中枢不认识的能力。
type HelloFrame struct {
	Type      string         `json:"type"`
	NodeID    string         `json:"node_id"`
	Protocol  int            `json:"protocol"`
	Instances []InstanceDecl `json:"instances"`
	Applied   []AppliedDecl  `json:"applied"`
	Labels    map[string]any `json:"labels,omitempty"`
	// AgentVersion 是节点二进制的版本（internal/version.String()："1.5.0+3fd5a59" / "dev"）。
	//
	// 老 agent 不带这个键 → 空串，中枢按「未知（老版本）」展示，不报错（契约 §3）。
	// 它只用于**观测与升级决策**（控制台据此提示过期），不参与调度判定。
	AgentVersion string `json:"agent_version,omitempty"`
}

// AppliedDecl 是节点对「我缓存了什么」的陈述。
//
// 它**不是**能力声明：中枢只会用它核对已部署的东西（对账 + 发现不一致后重新下发），
// 不会因为出现一条没部署过的 applied 就把能力归属给这台机器。
type AppliedDecl struct {
	Capability string `json:"capability"`
	Version    int    `json:"version"`
	Digest     string `json:"digest"`
}

// DeployFrame 下发或撤销一份产物。
//
// Artifact 是 CONTRACT-artifact §2 定义的 canonical JSON。节点负责校验、
// 探测 requires、原子落盘，然后回一帧 ApplyFrame。
type DeployFrame struct {
	Type     string          `json:"type"`
	DeployID string          `json:"deploy_id"`
	Action   string          `json:"action"` // apply | remove
	Artifact json.RawMessage `json:"artifact,omitempty"`
}

// 部署动作。
const (
	DeployActionApply  = "apply"
	DeployActionRemove = "remove"
)

// 部署结果状态（ApplyFrame.State）。
const (
	ApplyStateApplied = "applied"
	ApplyStateFailed  = "failed"
)

// 部署失败短码（ApplyFrame.Reason 的前缀，`;` 分隔多个）。
const (
	ApplyReasonInvalidArtifact    = "invalid_artifact"
	ApplyReasonMissingRequirement = "missing_requirement"
	ApplyReasonWriteFailed        = "write_failed"
	ApplyReasonUnsupportedAction  = "unsupported_action"
)

// ApplyFrame 是节点对一次部署的回报。
type ApplyFrame struct {
	Type       string `json:"type"`
	DeployID   string `json:"deploy_id"`
	Capability string `json:"capability,omitempty"`
	Version    int    `json:"version,omitempty"`
	Digest     string `json:"digest,omitempty"`
	State      string `json:"state"` // applied | failed
	Reason     string `json:"reason,omitempty"`
}

// InstanceDecl 是节点申报的并发槽位。
type InstanceDecl struct {
	Slot        int `json:"slot"`
	Concurrency int `json:"concurrency"`
}

// HeartbeatFrame 是心跳：任何上行帧都续命，心跳是最常见的那种。
type HeartbeatFrame struct {
	Type string `json:"type"`
	TS   int64  `json:"ts"`
}

// AckFrame 表示节点接单，租约开始计时。
type AckFrame struct {
	Type   string `json:"type"`
	TaskID string `json:"task_id"`
}

// ProgressFrame 推进任务到 running，可带一句进度说明。
type ProgressFrame struct {
	Type   string `json:"type"`
	TaskID string `json:"task_id"`
	Note   string `json:"note,omitempty"`
}

// ResultFrame 是任务结果。
//
// # out 已退役（P1.5，硬切）
//
// 这里曾经有一个 `Out *string`，带着 I5 的「两态」（有值 = 走那个出口 / 缺省 = 到此为止）。
// 现在**没有这个字段**：产出只有一个通道 —— emit（typed-pipeline §2），路由只看
// emit 的 `port`（I4 的形式保留：与 status 无关，失败前 emit 过照样继续）。
//
// 所以「一个任务走到哪里」不再由任何 result 字段表达：
//   - 想要下游 → 运行中 emit（多次、多端口都行）
//   - 不想走 → 不 emit（0 次 emit 是合法的，到此为止）
//   - 进程死/空转 → 退出码 / 信号 / 租约（状态来自进程，不来自端口）
//
// **老帧里多出来的 `"out":"fix"` 不报错**：encoding/json 对结构体里不存在的
// 字段一律忽略（这里没有 DisallowUnknownFields）。这是有意的——一条老实现发的帧
// 不应该因为一个已经退役的键就整帧被拒（bad_frame），它的效果是「没有下游」，
// 正好等于新语义下「没 emit」的语义。
type ResultFrame struct {
	Type   string `json:"type"`
	TaskID string `json:"task_id"`
	Status string `json:"status"` // succeeded | failed
	// Summary 是**唯一的**结果说明：成功=做了什么，失败=为什么失败。
	// 契约要求失败时非空（节点用 exit code / signal 兜底填），库里不再有第二个
	// "error" 字段去装同一句话。
	Summary   string   `json:"summary"`
	Artifacts []string `json:"artifacts"`
	// Error 是**兼容位**：老节点只填 error 不填 summary 时用它兜底 Summary。
	Error string `json:"error"`
}

// EmitFrame 是节点 → 中枢的一次**中途产出**（emit-protocol.md §3.1）。
//
// 语义要点：
//   - seq 在**一个任务内**单调递增、从 1 开始；中枢靠 `UNIQUE(task_id, seq)` 去重，
//     重发不会重复派发下游。缺口是丢消息的证据，中枢会明确回 error 而不是跳过。
//   - port 必须是本次 assign.ports 之一（I2 的封闭集合，提前到 emit 时判）。
//   - Data 是 JSON 对象（可缺省 = {}）；它**直接就是下游的 params**，不做字段搬运
//     （link 只有连接 + 可选字面量）。
//   - Note 只给人看，不参与路由。
type EmitFrame struct {
	Type   string          `json:"type"`
	TaskID string          `json:"task_id"`
	Seq    int64           `json:"seq"`
	Port   string          `json:"port"`
	Data   json.RawMessage `json:"data,omitempty"`
	Note   string          `json:"note,omitempty"`
}

// EmitAckFrame 是中枢 → 节点对 emit 的确认（emit-protocol.md §3.2）。
//
// Error 非空 = 拒收（seq 缺口 / seq 内容冲突 / 任务非运行态）。
// 重复 seq 一律回 ok，Routed/Pruned 取**首次**结果——重发不会重复派发。
type EmitAckFrame struct {
	Type   string `json:"type"`
	TaskID string `json:"task_id"`
	Seq    int64  `json:"seq"`
	// Routed 是派发出去几条下游；Pruned 是被剪枝条数（no_node / foreign_user / path）。
	// 0/0 不是错误：一个出口可以没接下游（到此为止）。
	Routed int    `json:"routed"`
	Pruned int    `json:"pruned,omitempty"`
	Error  string `json:"error,omitempty"`
}

// UpgradeFrame 让节点把自己的二进制升级到目标版本（CONTRACT-agent-upgrade §5）。
//
// 为什么不把二进制塞进帧里：帧上限 1 MiB（NodeFrameSoftLimit），而 agent 是 15 MB
// 量级；一条 15 MB 的帧会阻塞同一条连接上的心跳与 assign，节点侧读超时 = 掉线。
// 所以帧里只有「去哪拿、拿到后该是什么」三件事，下载走普通 HTTP（节点自己的 token 鉴权）。
type UpgradeFrame struct {
	Type string `json:"type"`
	// TargetVersion 是中枢自己的版本（控制台展示的「已是最新」也按它比较）。
	TargetVersion string `json:"target_version"`
	// SHA256 是 64 位十六进制（不带 "sha256:" 前缀）——下载后必须逐字节对上。
	SHA256 string `json:"sha256"`
	// URLPath 是中枢上的下载路径（相对，节点自己拼 server 根；见 NodeBinaryPath）。
	URLPath string `json:"url_path"`
}

// agent 升级失败的判别短码（upgrade_ack.error，CONTRACT-agent-upgrade §5）。
//
// 它们会原样落进 nodes.agent_upgrade.error 并出线给控制台（§5.1），所以字符串是
// 冻结契约的一部分，不能改。控制台按它做失败映射：need_root → 「去节点跑 sudo」。
const (
	UpgradeErrNeedRoot       = "need_root"
	UpgradeErrBadSHA         = "bad_sha"
	UpgradeErrDownloadFailed = "download_failed"
	UpgradeErrNotSupported   = "not_supported"
)

// NormalizeUpgradeError 把 ack 里的错误码收敛到契约枚举（未知码 → ""）。
//
// 为什么不让未知码原样落库：出线形状在 §5.1 里是**封闭集合**（控制台按枚举做
// 映射），存一个 UI 不认识的码只会让它显示「升级失败（未知原因）」——原始值已经
// 进了 WARN 日志，排障信息没有丢。
func NormalizeUpgradeError(code string) string {
	switch code {
	case UpgradeErrNeedRoot, UpgradeErrBadSHA, UpgradeErrDownloadFailed, UpgradeErrNotSupported:
		return code
	}
	return ""
}

// UpgradeAckFrame 是节点对 upgrade 的回报（CONTRACT-agent-upgrade §5）。
//
// ok=false 时 Error 是可判别短码：bad_sha / need_root / download_failed / not_supported。
// 中枢落库时**成功写版本列、失败只写 agent_upgrade 状态**（CONTRACT-agent-upgrade §5.1）：
// 节点上的二进制没换，版本列保持旧值才诚实；失败原因另存一列供控制台刷新后仍能映射。
type UpgradeAckFrame struct {
	Type string `json:"type"`
	OK   bool   `json:"ok"`
	// Version / Commit 是升级后**实际生效**的版本；ok=false 时为空。
	Version string `json:"version,omitempty"`
	Commit  string `json:"commit,omitempty"`
	Error   string `json:"error"`
}

// NodeBinaryPath 是节点下载 agent 二进制的路径（GET 到中枢根上）。
//
// 与 UpgradeFrame.URLPath 共用同一个常量：改路径只有一处，帧和 HTTP 路由不会漂移。
const NodeBinaryPath = "/v1/node/binary"

// helloVersion 是 hello 帧里中枢要落库、但**不在 HelloFrame 上**的字段（契约 §3）。
//
// 为什么不直接读 HelloFrame.AgentVersion：那个字段由 A 组加（契约 §7 明确
// 「B 只解析 + 落库 + 视图，不得再改这个 struct」）。这里对同一份原始 JSON
// 再解一次同一个键——两个解析器读的键逐字相同，A 合入后两处的值必然一致。
type helloVersion struct {
	AgentVersion string
}

// parseHelloVersion 从 hello 的原始载荷里取 agent_version。
//
// 载荷已由 DecodeUpstream 证明是合法 JSON，所以这里的失败只可能是「类型不对」；
// 一律当「没带这个字段」处理（版本是信息性数据，不值得因此拒掉一次握手）。
func parseHelloVersion(raw []byte) helloVersion {
	var v struct {
		AgentVersion string `json:"agent_version"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return helloVersion{}
	}
	return helloVersion{AgentVersion: v.AgentVersion}
}

// 下行帧（中枢 → 节点）──────────────────────────────

// WelcomeFrame 是握手成功后的第一帧：告诉节点按什么节奏发心跳。
type WelcomeFrame struct {
	Type         string `json:"type"`
	NodeID       string `json:"node_id"`
	Protocol     int    `json:"protocol"`
	HeartbeatSec int    `json:"heartbeat_sec"`
	LeaseTTLSec  int    `json:"lease_ttl_sec"`
	// Prune 列出「节点缓存里有、中枢已经不认」的能力：节点应当删掉它们。
	//
	// 为什么需要它：中枢删掉一个能力之后，节点缓存里必然还留着（缓存是派生物）。
	// 早先中枢对这种情况直接拒握手（unknown_capability），而 agent 视之为致命错误
	// → systemd 重启 → 再拒，节点永久崩溃循环。现在改成「认下认识的、把不认识的
	// 交回节点删掉」，这一帧就是那个「交回」。
	//
	// 老 agent 不认识这个键（json 忽略未知字段）→ 缓存留着但握手成功，无害。
	Prune []WelcomePrune `json:"prune,omitempty"`
}

// WelcomePrune 是一条要删掉的陈旧缓存。只带 name@version：digest 由节点自己从
// 索引里查，中枢不猜节点磁盘上有什么。
type WelcomePrune struct {
	Capability string `json:"capability"`
	Version    int    `json:"version"`
}

// AssignFrame 是中枢能表达的全部：{capability, version, params} + 租约。
//
// 仍然**不含任何「要执行什么」的内容**：节点自己按 capability@version（或 digest）
// 找到缓存里的产物。Digest 与 TraceID 是 v2 新增的旁路信息：
//
//   - Digest 让本次执行钉在一个确定的产物上（审计 + ANOTIFY_DIGEST）
//   - TraceID 让脚本能把日志串回链路（旧协议一直没有，issue #184）
type AssignFrame struct {
	Type        string         `json:"type"`
	TaskID      string         `json:"task_id"`
	Capability  string         `json:"capability"`
	Version     int            `json:"version"`
	Params      map[string]any `json:"params"`
	LeaseUntil  int64          `json:"lease_until"`
	MaxDuration int            `json:"max_duration_sec"`
	Digest      string         `json:"digest,omitempty"`
	TraceID     string         `json:"trace_id,omitempty"`

	// Ports 是本次运行**允许上报的出口名**（能力的 ports 快照）。
	//
	// 它是 I2「出口是封闭集合」的降级点：以前只能等进程退出、看到 `.anotify-out`
	// 才发现出口名写错了（而那条通道已经删了）；有了它，emit 当时就能拒。
	Ports []string `json:"ports,omitempty"`

	// Expects 是「这个端口的 data 要被怎样消费」，按 port 名索引。
	//
	// 它是**推导量**：中枢在派发那一刻从 link 算出来（各下游 params_schema 减去
	// 各自 literals 的并集 + 给人看的 targets），不落库。节点只持有本次任务的份，
	// 任务结束就丢——所以节点始终不掌握拓扑（只能知道自己的出边去哪、要什么）。
	Expects map[string]PortExpect `json:"expects,omitempty"`

	// Token 是该任务的一次性令牌：本地 emit socket 用它鉴权。
	//
	// 为什么需要它：socket 是父子进程共享的，同一台机器上可能有多个任务的进程；
	// 没有令牌，一个任务的脚本可以替另一个任务 emit（伪造产出）。
	Token string `json:"token,omitempty"`
}

// PortExpect 是一个端口的产出要求（emit 时校验）。
//
// Schema 是**机器门槛**：
// 该端口所有下游要求的并集（各自 params_schema 减去 literals）。
// 为空 = 没有下游（到此为止），此时只做结构校验。
//
// Targets 是**给人/LLM 看的**：这个口能去哪、各自要什么。
// 实现据此主动拼对参数，而不是被拒了再猜。
type PortExpect struct {
	Schema  json.RawMessage `json:"schema,omitempty"`
	Targets []ExpectTarget  `json:"targets,omitempty"`
}

// ExpectTarget 描述一个下游：谁、哪些版本、它需要哪些字段（去掉被 literals 覆盖的）。
type ExpectTarget struct {
	Capability string `json:"capability"`
	Version    int    `json:"version,omitempty"`
	// Priority 是这条边上的候选优先级（小的优先；0 = 默认，省略）。
	//
	// 由中枢从 edges.priority 读出来填好（route.BuildExpects），节点侧只**透传/展示**，
	// 不自己算：优先级是配置，不是执行时的判断。
	Priority int             `json:"priority,omitempty"`
	Needs    json.RawMessage `json:"needs,omitempty"`
}

// CancelFrame 要求节点停止某任务。
type CancelFrame struct {
	Type   string `json:"type"`
	TaskID string `json:"task_id"`
	Reason string `json:"reason,omitempty"`
}

// ErrorFrame 是中枢下发的错误（不关连接，除非是协议级错误）。
type ErrorFrame struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	TaskID  string `json:"task_id,omitempty"`
}

// ---------- 编解码 ----------

// envelope 只取帧类型，用于先分流再精确解码。
type envelope struct {
	Type string `json:"type"`
}

// UnknownFrame 表示解析成功但类型未知（调用方应回 unknown_type 错误帧）。
type UnknownFrame struct{ Type string }

// DecodeUpstream 把收到的 JSON 解码成对应类型的上行帧。
//
// 返回的具体类型是 *HelloFrame / *HeartbeatFrame / *AckFrame / *ProgressFrame / *ResultFrame；
// 类型未知时返回 (*UnknownFrame, nil)——这不是错误：协议要求回一个 error 帧
// （code=unknown_type）而不是断开连接。
// JSON 本身坏了返回 error（对应 code=bad_frame）。
func DecodeUpstream(data []byte) (any, error) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("bad frame json: %w", err)
	}
	switch env.Type {
	case NodeFrameHello:
		var f HelloFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode hello: %w", err)
		}
		return &f, nil
	case NodeFrameHeartbeat:
		var f HeartbeatFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode heartbeat: %w", err)
		}
		return &f, nil
	case NodeFrameAck:
		var f AckFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode ack: %w", err)
		}
		return &f, nil
	case NodeFrameProgress:
		var f ProgressFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode progress: %w", err)
		}
		return &f, nil
	case NodeFrameResult:
		// 注意：这里**不做**任何「容忍/拒收 out」的分支。老帧里多出来的 `"out"`
		// 是未知字段，json.Unmarshal 直接忽略（不报错）。刻意不给它一个
		// 「out is retired」的报错：那会把一条本来无害的老帧变成 bad_frame，
		// 让节点陷入重发→又被拒→重连的漩涡，而它们的路由结果本来就一样（无下游）。
		var f ResultFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode result: %w", err)
		}
		return &f, nil
	case NodeFrameApply:
		var f ApplyFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode apply: %w", err)
		}
		return &f, nil
	case NodeFrameEmit:
		var f EmitFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode emit: %w", err)
		}
		return &f, nil
	case NodeFrameUpgradeAck:
		var f UpgradeAckFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("decode upgrade_ack: %w", err)
		}
		return &f, nil
	default:
		return &UnknownFrame{Type: env.Type}, nil
	}
}

// Encode 把下行帧编码成 JSON（统一入口，便于加统一字段）。
func Encode(f any) ([]byte, error) {
	data, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("encode frame: %w", err)
	}
	return data, nil
}

// IsDownstream 判定一个帧类型是否是中枢 → 节点方向（用于断言节点没有发下行帧）。
func IsDownstream(frameType string) bool {
	switch frameType {
	case NodeFrameWelcome, NodeFrameAssign, NodeFrameDeploy, NodeFrameCancel, NodeFrameError,
		NodeFrameEmitAck, NodeFrameUpgrade:
		return true
	}
	return false
}

// IsUpstream 判定一个帧类型是否是节点 → 中枢方向。
func IsUpstream(frameType string) bool {
	switch frameType {
	case NodeFrameHello, NodeFrameHeartbeat, NodeFrameAck, NodeFrameProgress, NodeFrameResult,
		NodeFrameApply, NodeFrameEmit, NodeFrameUpgradeAck:
		return true
	}
	return false
}
