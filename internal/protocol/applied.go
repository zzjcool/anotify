package protocol

// applied 文件（D-I4）。
//
// 文件在 `<dataDir>/anode.applied.yaml`，0600，写临时文件 + rename 原子写；
// 内容是**机器级合并结果**（本机值 ⊕ 中枢节点默认值），任务执行读它。
//
// 它只存机器级：namespace 默认值只随任务在内存里传递，不写 applied，也不进快照。
// 文件形体用 JSON 编码（JSON 是 YAML 1.2 的子集，读的人与工具都不用换解析器），
// 字段名与 wire 一致，中枢/节点/人读的是同一份形状。
const (
	// AppliedFileName 是 applied 文件名（放在 dataDir 下）。
	AppliedFileName = "anode.applied.yaml"
	// AppliedFileMode 是文件权限：人不要手改它；手改会判 drift。
	AppliedFileMode = 0o600
)

// AppliedConfig 是 anode.applied.yaml 的内容（`{revision,hash,vars,limits}`）。
//
//   - revision：本次生效的节点默认值 revision（hello 上报给中枢）；
//   - hash：内容归一化哈希（中枢据此判 drift；节点也用它对账）；
//   - vars：机器级生效变量（local ⊕ node），不含 ns 默认值；
//   - limits：机器级生效并发预算（local ⊕ node，逐叶合并）。
type AppliedConfig struct {
	// Revision 是节点默认值 revision。
	Revision string `json:"revision"`
	// Hash 是内容归一化哈希。
	Hash string `json:"hash"`
	// Vars 是机器级生效变量。
	Vars map[string]string `json:"vars"`
	// Limits 是机器级生效并发预算。
	Limits Limits `json:"limits"`
}
