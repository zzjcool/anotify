package protocol

// 配置子协议的 5 个帧（config.md §4.2）。所有同步动作由 anode 发起，中枢只回应：
//
//	anode ── hello {name, token, namespaces, localVars, localLimits,
//	                appliedRevision, appliedHash} ──► 中枢
//	anode ◄── welcome {configRevision, nodeVars, nodeLimits, namespaces} ── 中枢
//	anode ── config.applied {revision, hash, machineVars} ──► 中枢
//	anode ◄── config.changed {revision} ── 中枢（只是提示，不是命令）
//	anode ── config.get {appliedRevision, appliedHash} ──► 中枢
//	anode ◄── welcome（同形复用，实现裁决 D-I2）
//
// 帧里带的是**机器级**快照：本机值（local）+ 中枢节点默认值（node）合并后的生效值。
// ns 默认值不在这里——它只随 task.dispatch 在内存里下发，不落 applied、不上报。
// envPassthrough 的名单与值都不上报（anode.md §3.3）。
const (
	// FrameHello 是节点每次连上的第一帧：注册 + 上报 + 拉默认值（一个动作）。
	FrameHello = "hello"
	// FrameWelcome 是中枢对 hello 的应答；config.get 的应答**复用同形**（D-I2）。
	FrameWelcome = "welcome"
	// FrameConfigChanged 只带 revision：节点自己决定什么时候 config.get。
	FrameConfigChanged = "config.changed"
	// FrameConfigGet 是节点主动拉取节点默认值（D-I2：请求 appliedRevision/appliedHash）。
	FrameConfigGet = "config.get"
	// FrameConfigApplied 是节点合并落盘并生效后的回报。
	FrameConfigApplied = "config.applied"
)

// HelloFrame 是节点连上后的第一帧（config.md §4.2 / anode.md §3.3）。
//
// 它同时是「注册」「上报」「拉默认值」三件事，因为中枢从不主动问节点：一次握手
// 就把状态对齐，重连即全量重报。帧里**不带 namespace**：节点令牌是集群身份，
// 不绑 ns；带的是 namespaces（本机申报列表），与中枢授权取交集生效。
type HelloFrame struct {
	Type string `json:"type"`
	// Name 是节点名（集群内唯一；换名字 = 换一台机器）。
	Name string `json:"name"`
	// Token 是节点身份凭据（不绑 ns）。
	Token string `json:"token"`
	// Namespaces 是本机**申报**的 ns 列表（缺省 ["default"]，["*"] = 全部）。
	// 生效 = 申报 ∩ 中枢授权（welcome.namespaces）；它不是配置，不走 revision。
	Namespaces []string `json:"namespaces"`
	// LocalVars 是 anode.yaml 里的本机值（跨 ns 共用，同键压住节点默认值）。
	LocalVars map[string]string `json:"localVars"`
	// LocalLimits 是本机并发预算（逐叶覆盖节点默认值）。
	LocalLimits Limits `json:"localLimits"`
	// AppliedRevision 是 anode.applied.yaml 里的机器级配置 revision。
	AppliedRevision string `json:"appliedRevision"`
	// AppliedHash 是 applied 文件的归一化哈希（中枢据此判定 drift）。
	AppliedHash string `json:"appliedHash"`
	// AgentVersion / AgentCommit 是节点自报的版本事实（D-I13；空串 = 未知）。
	// 它们不进 digest、不进配置 revision：只用于 NodesAvailable / Node.status / upgrade。
	AgentVersion string `json:"agentVersion"`
	AgentCommit  string `json:"agentCommit"`
	// Facts 是节点内置事实（anode.md §3.6）：能力用 status.* 引用它们，
	// 中枢用它回答 Node.status 与「这台机器能不能跑」。它们只走 hello/welcome，
	// 不新增帧、也不参与节点匹配的配置 revision。
	Facts NodeFacts `json:"facts"`
	// Capabilities 是本机的能力探测结果（D-I13）：每条带 requires.commands 的
	// 命中情况（LookPath 结果）与可执行能力名。中枢据此回答 NodesAvailable
	// 的 CommandMissing，不需要再问节点。
	Capabilities []HelloCapability `json:"capabilities"`
	// Instances 是本机申报的并发槽位（slot / concurrency / busy，D-I13）。
	// 槽位事实只在 hello 上报；派发授槽仍由中枢的 lease 协议决定。
	Instances []HelloInstance `json:"instances"`
}

// NodeFacts 是节点内置事实（anode.md §3.6 的 status.* 那一组）。
//
// 它们是「机器说了算」的值：不进 vars、不进配置 revision、不上报值来源，
// 每个节点都有。字段名是 wire 名（camelCase，D-I13）。
type NodeFacts struct {
	HostIP   string `json:"hostIP"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	// CPU 是逻辑核数（注入任务环境时是字符串，wire 上是数字）。
	CPU int `json:"cpu"`
	// WorkRoot 是任务目录的父目录；HomeDir 是 anode 用户家目录（都是目录类事实）。
	WorkRoot string `json:"workRoot"`
	HomeDir  string `json:"homeDir"`
}

// HelloCapability 是 hello.capabilities[] 的一条：能力名 + 它的命令探测结果。
//
// 为什么把「能力名」与「命令探测」放在同一条：中枢要回答的是
// 「这台机器能不能跑这个能力」，缺的往往是 requires.commands 里的一条命令；
// 分开两条会让这个判断变成一次跨数组 join。
type HelloCapability struct {
	// Name 是能力名（与 spec.metadata.name 同一套正则）。
	Name string `json:"name"`
	// Version / Digest 是这台机器上持有的那份（0 / "" = 未知或未部署）。
	Version int    `json:"version"`
	Digest  string `json:"digest"`
	// Commands 是 requires.commands 的逐条探测结果。
	Commands []HelloCommand `json:"commands"`
}

// HelloCommand 是一条命令探测结果：查到了给路径，没查到 Path 为空。
type HelloCommand struct {
	// Name 是命令名（requires.commands 的原样值）。
	Name string `json:"name"`
	// Found 是 LookPath 是否命中。
	Found bool `json:"found"`
	// Path 是命中的绝对路径（Found=false 时为空串）。
	Path string `json:"path"`
}

// HelloInstance 是 hello.instances[] / config.applied.instances[] 的一条**物理槽位**事实
// （A3）。
//
// presence 是这一节的硬要求：slot 用 *int（0 合法、缺失无效），busy 用 *bool
// （缺失 = 未知，**不能**当成空闲）。缺 instances / 缺 busy 都表示「没有新断言」，
// 中枢不能据此把旧绑定当作已退出。
type HelloInstance struct {
	// Slot 是 0..N-1 的槽号（0 合法）。
	Slot *int `json:"slot"`
	// Busy 是这条槽位的物理占用断言：true = 有进程/预留；false = 本机明确空闲。
	// 缺失（nil）= 未知，不能当 idle。
	Busy *bool `json:"busy,omitempty"`
	// 已执行的占用给 taskId/attempt/leaseId；尚未 grant 的 reservation 给
	// taskId/assignmentId/proposedAttempt，**不伪造** leaseId。
	TaskID          string `json:"taskId,omitempty"`
	Attempt         int    `json:"attempt,omitempty"`
	LeaseID         string `json:"leaseId,omitempty"`
	AssignmentID    string `json:"assignmentId,omitempty"`
	ProposedAttempt int    `json:"proposedAttempt,omitempty"`
}

// ValidSlot 的语义见 exec_frames.go（*int、≥0、0 合法）。

// WelcomeFrame 是中枢对 hello（以及 config.get）的应答。
//
// config.get 的应答复用本类型（D-I2）：文档只给了 config.get 帧名，没有定义第二个
// 应答形状；同形复用的好处是节点只有一条「合并 → 落盘 → config.applied」代码路径。
type WelcomeFrame struct {
	Type string `json:"type"`
	// ConfigRevision 是中枢侧节点默认值的 revision（节点据此判 stale_config）。
	ConfigRevision string `json:"configRevision"`
	// NodeVars 是中枢给这台机器存的默认值（本机没写的键才用它）。
	NodeVars map[string]string `json:"nodeVars"`
	// NodeLimits 是中枢给这台机器存的默认预算（逐叶被本机覆盖）。
	NodeLimits Limits `json:"nodeLimits"`
	// Namespaces 是中枢**授权**列表（与本机申报取交集，只能收紧）。
	Namespaces []string `json:"namespaces"`
}

// ConfigChangedFrame 是「这台的默认值变了」的提示，不带值也不带命令。
//
// 节点丢了这条提示也不会漂移：下次 hello 会自己对齐。中枢不做「下推配置」。
type ConfigChangedFrame struct {
	Type string `json:"type"`
	// Revision 是新的节点默认值 revision。
	Revision string `json:"revision"`
}

// ConfigGetFrame 让节点主动拉取当前节点默认值（D-I2）。
//
// 请求带 appliedRevision / appliedHash：中枢可以据此回「你没变」的应答，也可以在
// 节点手改过 applied 文件（hash 对不上）时把它拉回真相。应答是 WelcomeFrame。
type ConfigGetFrame struct {
	Type string `json:"type"`
	// AppliedRevision 是本机当前生效的 revision（空 = 还没有 applied 文件）。
	AppliedRevision string `json:"appliedRevision"`
	// AppliedHash 是本机当前 applied 文件的哈希（空 = 还没有 applied 文件）。
	AppliedHash string `json:"appliedHash"`
}

// ConfigAppliedFrame 是节点合并落盘并生效后的回报。
//
// machineVars = 本机值 ⊕ 节点默认值。ns 默认值不在里面：它按任务所属 ns 解析、
// 随任务内存下发（config.md §2.4）。limits 也不在里面——中枢从 hello 的
// localLimits + welcome 的 nodeLimits 就能算出机器级预算，再传一份就是第三份真相。
type ConfigAppliedFrame struct {
	Type string `json:"type"`
	// Revision 是本次生效的节点默认值 revision。
	Revision string `json:"revision"`
	// Hash 是 applied 文件内容的归一化哈希（中枢用它判 drift）。
	Hash string `json:"hash"`
	// MachineVars 是机器级生效值（含来源 local / node 的合并结果，不含值来源标记）。
	MachineVars map[string]string `json:"machineVars"`
	// Instances 可附本机真实槽位快照（A3）：配置应用后预算变化也要能上报。
	// 未附（nil）不更新实例账本。
	Instances []HelloInstance `json:"instances,omitempty"`
}
