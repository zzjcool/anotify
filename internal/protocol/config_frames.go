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
}

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
}
