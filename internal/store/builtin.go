package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zzjcool/anotify/internal/artifact"
)

// 内置能力清单（契约设计见 design/wiki/08-scope.html）。
//
// 内置能力与自定义能力走同一套表、同一套版本化，唯一区别是 `source` 列。
// 但有一类内置能力**没有节点执行者**：`notify.phone` 这类终端能力是中枢
// 自己的能力——没人会写一个"发推送节点"。所以派发时它们跳过「找节点」，
// 由中枢执行器执行（见 route.Dispatcher 与 internal/executor）。
//
// 为什么放在 store 而不是 route：这是**能力定义数据**，和 params_schema
// 一样属于"什么是一个合法能力"，不是路由算法。
type BuiltinCapability struct {
	Name string
	// DisplayName 是内置能力**预先取好的**显示名（「GitHub webhook」这种）。
	//
	// 用户不需要给内置能力取名 —— 它们随项目发布，名字由我们定。
	// 用户仍然可以改（不满意就改掉），但默认得有个能读的名字，
	// 否则能力页一打开满屏 github@1 / notify.phone@1。
	DisplayName  string
	Version      int
	ParamsSchema string // JSON
	Outputs      []string
	// Artifact 是随能力一起播种的产物（v2 之后节点只跑产物，没有产物 = 部署不了）。
	//
	// 大部分内置能力不需要它（handler 形式的由中枢执行、origin 的不产生任务），
	// 但「示例能力」必须带：它的全部价值就在于「注册完就能部署、能跑通一条链」。
	Artifact *artifact.Artifact
	// Handler 是「中枢内置执行器」形式的实现（Capability Spec v3 §3.1），
	// 与 Artifact 恰好给一个。
	//
	// 为什么要这个字段：notify.phone 由中枢执行、**没有 files**。给它在 artifacts 表里
	// 造一行会让它出现在部署面板里，被推给节点后节点只会拒（一份没有 files 的产物
	// 跑不起来）——契约 §6.1 把这个坑点名列了出来。所以 handler 形式只写
	// capability_versions.spec，artifacts 一行都不建。
	//
	// **它是「谁执行这个能力」的唯一来源**（P2b / 契约 §11.3）：这里填了可执行的
	// handler，这个能力就是中枢能力；中枢认领看的是库里的 spec.handler，不是这张表。
	// （曾经的 Central 布尔已删：它只是把这张内存表喂给一条硬编码名字表，而那张
	// 名字表会让「用户自定义的 handler 能力」永远不被认领。）
	Handler string
	// Origin 表示它是**事件起点**：没有节点执行者，也不产生任务，
	// 只提供「事件从这里进来」的命名锚点 + 任意事件类型作为出口。
	//
	// 与 handler 形式的区别：handler 能力会派发成中枢执行的任务；Origin 不建任务行，
	// 它是链的 0 号位（trigger 按 source 找到它，再用 type 当出口派发）。
	Origin bool
}

// NotifyPhoneCapability 是 `notify.phone@1` 的定义。
//
// params 就是通知内容本身——**没有模板替换、没有表达式求值**（不变量 I6）：
// 上游事件带什么就推什么。`to` 走 foreign_user 校验（契约 §10.1.1）。
var NotifyPhoneCapability = BuiltinCapability{
	Name:        "notify.phone",
	DisplayName: "推送通知",
	Version:     1,
	ParamsSchema: `{
		"type": "object",
		"required": ["to", "title"],
		"properties": {
			"to": {"type": "string"},
			"title": {"type": "string"},
			"body": {"type": "string"},
			"link": {"type": "string"},
			"agentState": {"type": "string"},
			"deviceTags": {"type": "array", "items": {"type": "string"}}
		}
	}`,
	Outputs: []string{},
	// 实现形式是**中枢内置执行器**：没有 files，也没有 artifacts 行（契约 §6.1）。
	// Handler 非空 = 由中枢执行（不再需要一个并行的 Central 布尔）。
	Handler: artifact.HandlerNotifyPhone,
}

// OriginCapability 构造一个内置起点能力。
//
// outputs 保持为空（不是 ["*"]）。原因：
//
//   - `EdgesFrom` 是精确匹配（`WHERE from_port = ?`），通配符在 SQL 里配不上
//     具体事件类型，写 "*" 只会让用户在建边下拉里看到一个用不了的选项；
//   - kind 推导对「无节点声明」的能力直接返回 origin，也不需要非空 outputs。
//
// 通配语义放在**建边校验**侧：内置起点的出口允许任意非空事件类型
// （见 store.CreateEdge）。这样用户建边时填的是真实事件名（github 的
// `pull_request.opened`），派发时也能精确命中。
func OriginCapability(name, displayName string) BuiltinCapability {
	return BuiltinCapability{
		Name:         name,
		DisplayName:  displayName,
		Version:      1,
		ParamsSchema: `{"type": "object"}`,
		Outputs:      []string{},
		Origin:       true,
	}
}

// BuiltinCapabilities 是随项目发布的全部内置能力。
//
// 两类：
//   - 中枢执行（填了 Handler）：notify.phone —— 没有节点会去"发推送"
//   - 事件起点（Origin）：github/cron/http/manual —— 没有节点会去"当来源"
//
// 「谁由中枢执行」不再由这张表回答：运行时认领看的是库里的 spec.handler（P2b /
// 契约 §11.3）。表里只需要把 Handler 填对，播种后会自然成为中枢能力。
//
// 起点做成内置的收益：用户接入一个 GitHub webhook 时，**不需要先伪造一个
// 叫 github、没有出口、没有节点的"能力"**，直接在能力页建边即可。
// 这是把「入口」和「能干活的东西」两个概念分开，而不是绑死。
//
// 自定义来源依然支持：用户可以自己定义一个能力（比如 jira@1），
// source = "jira" 一样能找到它——内置只是把常见来源预先备好。
// ExampleCapability 是**给新命名空间的示例能力**（#282）。
//
// 为什么需要它：v2 之后新账号里唯一的可见能力曾是 notify.phone，而它（a）被
// 手机推送的开关隐去（b）由中枢执行、没有产物、部署不了。结果就是一个全新账号
// 打开能力页什么都没有 —— 用户以为坏了，而这只是「还没有人定义过能力」。
//
// 设计约束（每一条都是为了「第一次就能跑通」）：
//   - **零依赖**：只用 bash 内建 + echo，任何节点都跑得起来（不需要 git/curl/jq）
//   - **带产物**：有 artifact 才能「部署 → 节点应用 → 执行」这条链路完整可见
//   - **有参数**：能力页的参数表单不是空的，用户看得见「参数怎么传进脚本」
//   - **有出口**：outputs=["echoed"]，而且脚本真的会走这个出口（任务会成功）——
//     可以拿它当一条链的上游做实验
//   - **不占名字**：叫 script.echo 而不是 demo.xxx —— 它是能用的东西，不是演示
var ExampleCapability = BuiltinCapability{
	Name:        "script.echo",
	DisplayName: "回声（示例）",
	Version:     1,
	ParamsSchema: `{
		"type": "object",
		"properties": {
			"message": {"type": "string", "title": "消息", "description": "会被原样打印出来"}
		}
	}`,
	Outputs: []string{"echoed"},
	Artifact: &artifact.Artifact{
		APIVersion: artifact.APIVersion,
		Kind:       artifact.KindCapability,
		Metadata:   artifact.Metadata{Name: "script.echo", Version: 1, DisplayName: "回声（示例）"},
		Spec: artifact.Spec{
			Params: json.RawMessage(`{
				"type": "object",
				"properties": {
					"message": {"type": "string", "title": "消息", "description": "会被原样打印出来"}
				}
			}`),
			Outputs: []string{"echoed"},
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files: map[string]string{
					"run.sh": `#!/bin/bash
# 示例能力：把参数原样打印出来，然后从固定出口 echoed 走下一跳。
# 这不是玩具——它同时演示了三件事：参数怎么进脚本（ANOTIFY_PARAM_* / ANOTIFY_PARAMS）、
# 输出怎么回中枢（stdout）、产出怎么走下一跳（anotify-agent emit）。
#
# 产出**只有一条通道**：emit（P1.5 起 .anotify-out / $ANOTIFY_OUT_FILE 已退役）。
# ANOTIFY_EMIT_SOCK / ANOTIFY_TASK_ID / ANOTIFY_TASK_TOKEN 由 agent 注入；
# ANOTIFY_AGENT_BIN 是 agent 自己的路径（取不到就回退到 PATH 上的 anotify-agent）。
#
# 端口是**封闭集合**（不变量 I2）：emit 一个没声明过的端口会被 node 当场拒
# （CLI 退出码 3），而不是“这个任务的结果叫 hello”。所以这里写死 echoed。
# （这里曾把 "$msg" 写进出口文件，于是任何 message 都跑不过——见
#  TestExampleBuiltinScriptEmitsDeclaredPort。）
set -euo pipefail

msg="${ANOTIFY_PARAM_MESSAGE:-hello from Anotify}"
echo "$msg"

# 没有 emit socket = 本地 standalone 跑（没有中枢，emit 无处可去）：跳过而不是
# 把任务跑成失败。这是 emit-protocol §1 「本地模式无中枢」那条分支的用法示例。
if [ -n "${ANOTIFY_EMIT_SOCK:-}" ]; then
  agent_bin="${ANOTIFY_AGENT_BIN:-anotify-agent}"
  # 把**入参原样**交给下游（ANOTIFY_PARAMS 由 agent 注入，始终是合法 JSON 对象），
  # 免得在零依赖 bash 里手拼 JSON（那才是转义 bug 的温床）。
  # 注意别写成 ${ANOTIFY_PARAMS:-{}}：bash 会在第一个未配对的 } 处截断展开，
  # 结果是多出一个字面量 } → 非法 JSON → emit 退出码 2。
  "$agent_bin" emit --data "$ANOTIFY_PARAMS"
fi
`,
				},
				TimeoutSec: 30,
			},
		},
	},
}

var BuiltinCapabilities = []BuiltinCapability{
	NotifyPhoneCapability,
	// 示例能力：放在清单里意味着**每个新命名空间都会播种它**，
	// 已有命名空间在启动时由 BackfillBuiltinCapabilities 补上（幂等）。
	ExampleCapability,
	// github/cron/http/manual **不再是能力**（契约 §2.4 / #110）。
	//
	// 它们原来是以「内置起点能力」的形式存在的：不执行代码、不产生任务、
	// 也没地方存配置（cron 表达式之类）。唯一作用是给入口边当左端锚点。
	// 代价是能力列表上多出 4 个用户看得见却用不上的条目。
	//
	// 现在入口边的左端直接是来源名（edges.from_is_source=1），
	// 用户视角的「事件从哪来」完全由 sources 承担。
}

// BackfillBuiltinCapabilities 给本部署**所有命名空间**补齐内置能力（幂等），
// 返回处理过的命名空间数。启动时跑一遍。
//
// 语义在 #162 之后从「一个组织一份」变成「每个命名空间一份」：能力属于命名空间，
// `notify.phone` 在 prod 与 staging 是两行不同的定义（各自 namespace_id）；
// 组织层已删，所以这里直接遍历 namespaces。
//
// 为什么不只在新命名空间播种：内置能力清单会随版本升级增加（比如以后加
// notify.webhook）。那时已有命名空间也需要补上——启动时全量跑一遍比"只在
// 创建时播种"可靠（少一条路径就是一次线上事故）。
func (d *DB) BackfillBuiltinCapabilities(ctx context.Context) (int, error) {
	namespaces, err := d.ListNamespaces(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ns := range namespaces {
		if err := d.SeedBuiltinCapabilities(ctx, ns.ID); err != nil {
			return n, fmt.Errorf("backfill builtin capabilities for namespace %s: %w", ns.ID, err)
		}
		n++
	}
	return n, nil
}

// SeedBuiltinCapabilities 给一个命名空间播种内置能力（自带事务的便捷包装）。
func (d *DB) SeedBuiltinCapabilities(ctx context.Context, namespaceID string) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin seed builtin capabilities: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := SeedBuiltinCapabilitiesTx(ctx, tx, namespaceID); err != nil {
		return err
	}
	return tx.Commit()
}

// SeedBuiltinCapabilitiesTx 在调用方事务内给一个命名空间播种全部内置能力。
//
// 两条必须都走这条路：
//  1. 新建命名空间（注册 / 建命名空间的 default）—— 否则新命名空间里连一个
//     end 节点（notify.phone）都声明不出来，推送直接失效；
//  2. 内置能力升版 —— 遍历所有命名空间各插一行（append-only，不改旧行）。
func SeedBuiltinCapabilitiesTx(ctx context.Context, tx *sql.Tx, namespaceID string) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	for _, bc := range BuiltinCapabilities {
		if err := seedOneBuiltinTx(ctx, tx, namespaceID, bc); err != nil {
			return err
		}
	}
	return nil
}

// seedOneBuiltinTx 在某个命名空间里播种一个内置能力（已存在则只补显示名）。
//
// 已有行只补**空**的 display_name：预设名是给老库用的（这一列是后加的），
// 而用户改过的名字不能被启动流程改回去。
func seedOneBuiltinTx(ctx context.Context, tx *sql.Tx, namespaceID string, bc BuiltinCapability) error {
	var existingID, existingDisplay string
	err := tx.QueryRowContext(ctx,
		`SELECT id, display_name FROM capability_versions
		  WHERE namespace_id = ? AND name = ? AND version = ?`,
		namespaceID, bc.Name, bc.Version).Scan(&existingID, &existingDisplay)
	if err == nil {
		// 能力已存在（老库升级 / 重复播种）：产物可能还没建过，仍要补一次。
		if err := syncBuiltinDeploymentsTx(ctx, tx, namespaceID, existingID); err != nil {
			return err
		}
		if bc.DisplayName != "" && existingDisplay == "" {
			if _, uerr := tx.ExecContext(ctx,
				`UPDATE capability_versions SET display_name = ? WHERE id = ?`,
				bc.DisplayName, existingID); uerr != nil {
				return fmt.Errorf("backfill display name for builtin %s@%d: %w", bc.Name, bc.Version, uerr)
			}
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("probe builtin capability %s@%d: %w", bc.Name, bc.Version, err)
	}

	spec, err := builtinSpec(bc)
	if err != nil {
		return err
	}
	c := &CapabilityVersion{
		NamespaceID: namespaceID,
		Name:        bc.Name,
		DisplayName: bc.DisplayName,
		Version:     bc.Version,
		Source:      CapSourceBuiltin,
		Spec:        spec,
	}
	// 派生列由正文算出（与 createCapabilityTx 同一条路：deriveFromSpec 是唯一实现）。
	if err := deriveFromSpec(c); err != nil {
		return fmt.Errorf("seed builtin capability %s@%d: %w", bc.Name, bc.Version, err)
	}
	hash, err := NormalizeCapability(c)
	if err != nil {
		return fmt.Errorf("seed builtin capability %s@%d: %w", bc.Name, bc.Version, err)
	}
	specJSON, err := json.Marshal(c.Spec)
	if err != nil {
		return fmt.Errorf("marshal builtin spec %s@%d: %w", bc.Name, bc.Version, err)
	}
	schema := string(c.ParamsSchema)
	if schema == "" {
		schema = "{}"
	}
	outputsJSON, err := json.Marshal(c.Outputs)
	if err != nil {
		return fmt.Errorf("marshal builtin outputs %s@%d: %w", bc.Name, bc.Version, err)
	}
	lifecycleJSON, err := json.Marshal(c.Lifecycle)
	if err != nil {
		return fmt.Errorf("marshal builtin lifecycle %s@%d: %w", bc.Name, bc.Version, err)
	}
	capID := NewCapabilityID(hash)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO capability_versions
		   (id, namespace_id, name, display_name, version, params_schema, outputs,
		    lifecycle, sticky, source, content_hash, created_at, spec)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		capID, namespaceID, c.Name, c.DisplayName, c.Version, schema,
		string(outputsJSON), string(lifecycleJSON), c.Sticky, CapSourceBuiltin, hash, Now(),
		string(specJSON)); err != nil {
		return fmt.Errorf("seed builtin capability %s@%d: %w", bc.Name, bc.Version, err)
	}
	return syncBuiltinDeploymentsTx(ctx, tx, namespaceID, capID)
}

// builtinSpec 返回内置能力播种时要写进 capability_versions.spec 的正文。
//
// 两种实现形式恰好给一个（Capability Spec v3 §3.1）：
//   - Handler 形式：中枢内置执行器（notify.phone）。artifacts 一行都不建（§6.1）。
//   - Artifact 形式：正文直接取产物里的 spec（要归一成 canonical —— 产物里的
//     params 可能带缩进，而 spec 列与 digest 吃的是同一串字节）。
//
// 两者都没有的条目（历史遗留的 origin 类）给 `handler: noop`：契约 R2 说能力永远
// 有实现，占位用平台认领的 noop 明确表达「还没实现」，而不是伪造一份用户代码。
func builtinSpec(bc BuiltinCapability) (artifact.Spec, error) {
	switch {
	case bc.Handler != "" && bc.Artifact != nil:
		return artifact.Spec{}, fmt.Errorf("builtin capability %s@%d: handler 与 artifact 只能给一个", bc.Name, bc.Version)
	case bc.Handler != "":
		return HandlerSpec(bc.Handler, json.RawMessage(bc.ParamsSchema), bc.Outputs, nil), nil
	case bc.Artifact != nil:
		spec, _, err := canonicalizeSpec(bc.Artifact)
		if err != nil {
			return artifact.Spec{}, fmt.Errorf("builtin capability %s@%d: %w", bc.Name, bc.Version, err)
		}
		return spec, nil
	default:
		return HandlerSpec(artifact.HandlerNoop, json.RawMessage(bc.ParamsSchema), bc.Outputs, nil), nil
	}
}

// syncBuiltinDeploymentsTx 让已有的部署行跟着内置能力的**当前内容**走（幂等）。
//
// 内置能力的内容是**平台自己的**：修了示例里的 bug 之后，部署不该等人去控制台
// 逐台重点一次。部署是中枢下发的期望状态，平台换了内容，期望就该跟着换 —— 这里把
// 该能力的部署行打回 pending（reason 说明是内置更新），帧由节点连接时的
// ws.resendDeployments 补发（播种发生在启动阶段，那时还没有连接可推）。
//
// P2 之后没有 artifacts 表可指：内容标识就是**能力行**的 content_hash
// （≡ digest，契约 §4），所以这里读回那一行、把 digest 列对齐、状态打回 pending。
//
// 两步都不能省：
//   - 先收敛（同一 (node, capability) 只留最新那条）：旧版允许「旧 digest 的行保留为
//     历史」，多行并存时 UPDATE 会撞 UNIQUE(node_id, capability_id, digest)；
//   - 再打回 pending：内容变了而状态还是 applied，控制台上就是一个假的绿勾。
func syncBuiltinDeploymentsTx(ctx context.Context, tx *sql.Tx, namespaceID, capabilityID string) error {
	var contentHash, spec string
	err := tx.QueryRowContext(ctx,
		`SELECT content_hash, spec FROM capability_versions WHERE id = ? AND namespace_id = ?`,
		capabilityID, namespaceID).Scan(&contentHash, &spec)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // 行不见了（并发删库？）：没有可同步的部署
	}
	if err != nil {
		return fmt.Errorf("read builtin capability %s: %w", capabilityID, err)
	}
	// handler 形式（中枢内置执行器）不可部署：不碰部署行，也不要造出「待推送」的假象。
	var sp artifact.Spec
	if strings.TrimSpace(spec) != "" {
		if jerr := json.Unmarshal([]byte(spec), &sp); jerr != nil {
			return fmt.Errorf("decode builtin spec %s: %w", capabilityID, jerr)
		}
	}
	if sp.Handler != "" {
		return nil
	}
	digest := artifact.DigestPrefix + contentHash

	// ① 收敛成一行：**保留最新那条**（它代表最近一次部署意图），其余删掉。
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM deployments
		  WHERE namespace_id = ?1
		    AND capability_id = ?2
		    AND id NOT IN (
		      SELECT id FROM (
		        SELECT d.id AS id, ROW_NUMBER() OVER (PARTITION BY d.node_id ORDER BY d.created_at DESC, d.id DESC) AS rn
		          FROM deployments d
		         WHERE d.namespace_id = ?1 AND d.capability_id = ?2
		      ) WHERE rn = 1)`,
		namespaceID, capabilityID); err != nil {
		return fmt.Errorf("dedupe builtin deployment %s: %w", capabilityID, err)
	}
	// ② 对齐 digest 并打回 pending（内容没变时是 0 行更新，纯幂等）。
	if _, err := tx.ExecContext(ctx,
		`UPDATE deployments
		    SET digest = ?, state = ?, reason = ?, updated_at = ?
		  WHERE namespace_id = ? AND capability_id = ? AND digest <> ?`,
		digest, DeploymentPending, "builtin artifact updated", Now(),
		namespaceID, capabilityID, digest); err != nil {
		return fmt.Errorf("repoint builtin deployment %s: %w", capabilityID, err)
	}
	return nil
}

// IsBuiltinCapability 判断某个 name@version 是不是内置能力（中枢执行或起点）。
//
// 注意：「是不是内置能力」与「由谁执行」是两件事。内置能力也可能由节点执行
// （script.echo），起点能力则根本不产生任务。执行者看 **spec.handler**
// （artifact.IsExecutableHandler），不要问这张表。
func IsBuiltinCapability(name string, version int) (BuiltinCapability, bool) {
	for _, bc := range BuiltinCapabilities {
		if bc.Name == name && bc.Version == version {
			return bc, true
		}
	}
	return BuiltinCapability{}, false
}

// IsBuiltinCapabilityName 按名字判断是不是内置能力（不看版本）。
//
// 用于「名字是否保留」的判定：内置起点的名字不能被用户开新版本接管，
// 否则 findOrigin 取最高版本会让 source 解析到自定义版本上，
// 而自定义版本不享受起点的任意出口——失败得极其隐蔽。
func IsBuiltinCapabilityName(name string) (BuiltinCapability, bool) {
	for _, bc := range BuiltinCapabilities {
		if bc.Name == name {
			return bc, true
		}
	}
	return BuiltinCapability{}, false
}

// CentralCapabilityNames 返回由中枢执行的能力名（ClaimCentralTasks 的唯一认领依据）。
//
// **数据驱动**（Capability Spec v3 §11.3）：从 `capability_versions.spec` 里读
// handler，而不是遍历 BuiltinCapabilities 筛一个布尔。判据就一条—— spec.handler
// 落在 artifact.ExecutableHandlers() 里（handler 白名单里可执行的那一部分）：
//
//	SELECT DISTINCT name FROM capability_versions
//	 WHERE json_extract(spec, '$.handler') IN (…)
//
// 为什么必须这样：旧实现是一张内存硬编码名字表（`Central: true`），于是用户自定义的
// `handler: notify-phone` 能力**永远不会被认领**（派发把它当节点能力，查 node_capabilities
// 必然 0 行 → pruned/no_node），而这张表也就成了与库内容并行的第二真相。
//
// 跨命名空间（不过滤 namespace_id）：中枢任务没有节点，命名空间不参与「谁能执行」的
// 判定；每条任务自己带 NamespaceID，后续写入仍按它生效（I10）。这也是旧实现的口径。
//
// **有意不做缓存**：缓存必须配写入失效，而写入点散在 createCapabilityTx / 播种 /
// 迁移回填三处以上，漏一个就静默变成第二真相（且症状是“新能力不被认领”这种
// 最难查的形态）。这条查询由执行器按 1s 轮询的频率付，表是每个命名空间个位数行，
// 不值得为它引入一份失效风险。
func (d *DB) CentralCapabilityNames(ctx context.Context) ([]string, error) {
	handlers := artifact.ExecutableHandlers()
	if len(handlers) == 0 {
		return []string{}, nil
	}
	placeholders := make([]string, len(handlers))
	args := make([]any, len(handlers))
	for i, h := range handlers {
		placeholders[i] = "?"
		args[i] = h
	}
	rows, err := d.QueryContext(ctx,
		`SELECT DISTINCT name FROM capability_versions
		  WHERE json_extract(spec, '$.handler') IN (`+strings.Join(placeholders, ",")+`)
		  ORDER BY name`, args...)
	if err != nil {
		return nil, fmt.Errorf("query central capability names: %w", err)
	}
	defer rows.Close()

	// 空结果给 []（不是 nil）：空列表纪律（不变量 I9）。
	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan central capability name: %w", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate central capability names: %w", err)
	}
	return out, nil
}

// ClaimCentralTasks 领取中枢执行的 pending 任务，并推进到 running。
//
// 为什么跳过 assigned/acked：那两个状态的含义是「已派给节点，等它接单/开始」。
// 中枢执行没有节点参与，走那两个状态会让状态机语义失真（也会让租约巡检
// 去扫它们——而中枢任务没有租约，扫不到就会一直挂着）。
//
// 并发安全：用 transitionGuarded 的 CAS（status=pending 作为守卫），
// 多实例部署时只有一个能领到同一条任务。
func (d *DB) ClaimCentralTasks(ctx context.Context, limit int) ([]*Task, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	// 认领名单来自**数据**（spec.handler），不是一个包级变量：查库失败必须向上抛，
	// 而不是静默当成「一个中枢能力都没有」（那会让推送任务永远停在 pending，
	// 而日志里只会看到“没活干”）。
	names, err := d.CentralCapabilityNames(ctx)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return []*Task{}, nil
	}

	// 先查候选（pending + 中枢能力名）
	placeholders := make([]string, len(names))
	args := make([]any, 0, len(names)+limit)
	for i, n := range names {
		placeholders[i] = "?"
		args = append(args, n)
	}
	q := `SELECT ` + taskColumns + ` FROM tasks
	      WHERE status = ? AND node_id IS NULL AND capability IN (` +
		strings.Join(placeholders, ",") + `)
	      ORDER BY created_at, id LIMIT ?`
	queryArgs := append([]any{TaskPending}, args...)
	queryArgs = append(queryArgs, limit)

	rows, err := d.QueryContext(ctx, q, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("query central tasks: %w", err)
	}
	candidates := []*Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate central tasks: %w", err)
	}
	rows.Close()

	// 逐条 CAS 推进到 running（pending → running 不在合法迁移表里，
	// 所以走 pending → assigned → running：assigned 在这里只表示"被中枢认领"）。
	claimed := make([]*Task, 0, len(candidates))
	for _, t := range candidates {
		// 作用域是行自己的 namespace_id（#162：任务的隔离维度是命名空间，
		// 中枢任务也属于某个命名空间）。
		if err := d.transitionGuarded(ctx, nil, t.NamespaceID, t.ID, TaskPending, TaskAssigned, ""); err != nil {
			if errors.Is(err, ErrTransitionGuardFailed) || errors.Is(err, ErrInvalidTransition) {
				continue // 别人先领了
			}
			return claimed, fmt.Errorf("claim central task %s: %w", t.ID, err)
		}
		if err := d.transitionGuarded(ctx, nil, t.NamespaceID, t.ID, TaskAssigned, TaskRunning, ""); err != nil {
			return claimed, fmt.Errorf("start central task %s: %w", t.ID, err)
		}
		if _, err := d.ExecContext(ctx,
			`UPDATE tasks SET started_at = COALESCE(started_at, ?) WHERE namespace_id = ? AND id = ?`,
			Now(), t.NamespaceID, t.ID); err != nil {
			return claimed, fmt.Errorf("stamp central task start: %w", err)
		}
		t.Status = TaskRunning
		claimed = append(claimed, t)
	}
	return claimed, nil
}

// FinishTask 把 running 的中枢任务推进到终态并写回报。
//
// 与 ReportResult 的区别：ReportResult 是**节点**回报路径（要求任务有
// node_id 归属、会校验租约）。中枢任务没有归属，所以走这条。
//
// 没有出口参数：P1.5 之后 `tasks.out` 整列已删（baseline 里没有这一列），路由只由 emission 表达。
func (d *DB) FinishTask(ctx context.Context, namespaceID, taskID, status string, summary, errMsg string) error {
	if status != TaskSucceeded && status != TaskFailed {
		return fmt.Errorf("%w: finish status must be succeeded|failed, got %q", ErrInvalidTransition, status)
	}

	// 与节点回报路径共用同一套「说明/原因」归并与派生规则。
	summary, errMsg = normalizeResultMessage(status, summary, errMsg)

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin finish task: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// running → succeeded|failed（走唯一写入口，守卫 status=running）
	if err := d.transitionGuarded(ctx, tx, namespaceID, taskID, TaskRunning, status, ""); err != nil {
		return fmt.Errorf("finish central task %s: %w", taskID, err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET reported = ?, summary = ?, error = ? WHERE namespace_id = ? AND id = ?`,
		1, summary, errMsg, namespaceID, taskID); err != nil {
		return fmt.Errorf("write central task result: %w", err)
	}
	return tx.Commit()
}
