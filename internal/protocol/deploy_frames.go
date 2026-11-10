package protocol

// 部署接缝的 2 个帧（父 CONTRACT rev2 §7；设计文档尚无独立章节，见 PROTOCOL-HANDOFF）。
//
// 能力投递是**人工部署动作**的传输接缝，不是自收敛控制器：中枢只把「部署意图」
// 发给一台在线节点；节点按自己的身份认证，用
//
//	GET /v1/node/capabilities/{digest}   （Authorization: Bearer <node token>）
//
// 取完整、不可变的 Artifact 文档，校验 spec / digest / 路径后原子缓存，再回
// capability.deployed。**Artifact 整份文档不进 WS 帧**：帧有 1 MiB 软上限，脚本
// 包体不该挤进受软上限约束的通道；WS 只传递「要哪个 digest」和「结果如何」。
//
// 两帧的 refs 是同一组四元组 {namespace,name,version,digest}（共享 DeployRefs，
// 不是两份各自手写的 tag）：中枢据此把回执配回部署意图，不靠 name 猜版本。
// 离线节点在重连时重放**已明确请求**的未完成部署由中枢负责；不新增帧、不做
// 通用 reconcile。
const (
	// FrameCapabilityDeploy 是中枢 → 节点的部署通知（只带 refs，不带包体）。
	FrameCapabilityDeploy = "capability.deploy"
	// FrameCapabilityDeployed 是节点 → 中枢的部署回执（applied / failed）。
	FrameCapabilityDeployed = "capability.deployed"
)

// DeployStatus 是 capability.deployed 的状态（封闭集合）。
type DeployStatus string

const (
	// DeployStatusApplied 表示节点已获取、校验并缓存该 digest。
	DeployStatusApplied DeployStatus = "applied"
	// DeployStatusFailed 表示获取/校验/缓存失败（error 带人话原因）。
	DeployStatusFailed DeployStatus = "failed"
)

// Valid 报告状态是否是封闭集合里的值。
func (s DeployStatus) Valid() bool {
	switch s {
	case DeployStatusApplied, DeployStatusFailed:
		return true
	}
	return false
}

// DeployRefs 是部署意图/回执共享的身份四元组。
//
// 匿名嵌入两帧，wire 上**平铺**成 `namespace` / `name` / `version` / `digest`
// 四个顶层键（与父契约的字段表逐字对应），Go 侧只有一份定义，避免「两个帧各写
// 一遍 tag，改一处漏一处」。
type DeployRefs struct {
	// Namespace 是能力所属命名空间（中枢只能部署该节点被授权且明确指向的 ns）。
	Namespace string `json:"namespace"`
	// Name 是能力名（谱系，不是某个版本）。
	Name string `json:"name"`
	// Version 是解析到的版本号（省略 version 的能力由内容寻址分配）。
	Version int `json:"version"`
	// Digest 是部署身份（artifact.Digest，`sha256:<hex>`）：节点按它 GET 包体，
	// 拿到后必须自己重算并比对，不信任传输内容。
	Digest string `json:"digest"`
}

// CapabilityDeployFrame 是中枢 → 节点的部署请求。
//
// 它是在线节点上的一次显式人工部署；离线节点不靠这一帧，重连后由中枢重放
// 已请求的部署。**不带 Artifact 内容**（也没有 files 字段）。
type CapabilityDeployFrame struct {
	Type string `json:"type"`
	// RequestID 是部署请求 id（中枢生成；回执按它配对，同一次重放保持同一个）。
	RequestID string `json:"requestId"`
	// DeployRefs 平铺为 namespace/name/version/digest。
	DeployRefs
}

// CapabilityDeployedFrame 是节点 → 中枢的部署回执。
//
// refs 必须与请求**完全相同**（Namespace/Name/Version/Digest 四元组）；
// 中枢按 requestId 配对并用 refs 校验没有被篡改的部署目标。
type CapabilityDeployedFrame struct {
	Type string `json:"type"`
	// RequestID 对应 capability.deploy.requestId。
	RequestID string `json:"requestId"`
	// DeployRefs 与请求同一组 refs。
	DeployRefs
	// Status 是 applied（已获取、校验并缓存）或 failed。
	Status DeployStatus `json:"status"`
	// Error 是失败的人话原因（status=failed 时非空；成功时省略，不进任何判断）。
	Error string `json:"error,omitempty"`
}
