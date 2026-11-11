/**
 * mock.js —— 纯前端预览用的 API 拦截层（**v4 形状**）。
 *
 * 为什么需要它：
 *   看前端效果不该先把 server、tunnel、devseed 示例数据、WS worker 全跑起来。
 *   那条链路对"我只想看看页面长什么样"来说太重了，而且任何一步失败
 *   （服务起不来、tunnel 不通、worker 掉线）看到的都是"页面空着"。
 *
 * 做法：在 <head> 里最早加载本脚本，包住 window.fetch。
 *   路径以 /v1/ 开头的请求**不发出网络请求**，直接返回这里构造的数据；
 *   其它请求（html/css/js/svg）照常走网络。
 *
 * 为什么包 fetch 而不是 Service Worker：
 *   SW 需要 https 或 localhost、还要注册/激活/刷新一次才生效，
 *   而预览场景常常是 file:// 或局域网 IP 直开。包 fetch 没有这些前置条件，
 *   打开就能用。代价是不能拦截 <img>/<link> 这类不走 fetch 的请求——
 *   而我们只需要拦 API。
 *
 * v4 口径（`docs/design/v4/api-conventions.md`）：
 *   · 资源端点一律 `{apiVersion,kind,metadata,spec,status}` 信封，列表是 `KindList.items`；
 *   · 命名空间私有资源走 `/v1/namespaces/{namespace}/{plural}[/{name}]`；
 *   · 写已有资源必须带 `metadata.resourceVersion`（CAS），`status` 只读；
 *   · 错误体统一 `Status`（`reason` 用封闭词表）；
 *   · **status 不落库**：能力/源/节点的 status 全部在**读时**从 mock 的事实
 *     （capabilities / sources / deployments / tasks / events / nodes）现算 —— 与真后端
 *     「status 从事实派生、不落库」同一口径。所以这份文件里没有一个 `row.status`
 *     被写过。
 *
 * **不做 v3 兼容**：v3 的扁平资源路径（`/v1/capabilities`）、`PUT .../downstreams`、
 * `POST/DELETE /v1/edges`、`POST/DELETE /v1/deployments`、`namespaceId` query 全部
 * 走 REMOVED 表回 404 `Status`。没实现的**新**接口才回 501（并保持既有的
 * `console.warn("mock 未实现：…")` 文案 —— 那是 e2e 门禁的一部分）。
 *
 * 数据是**内存里的可变状态**：新建/删除/触发都会真的改变它，
 * 所以能点着试完整流程。刷新页面回到初始状态。
 */
(function () {
	"use strict";

	// ---------- 基础工具 ----------
	const now = () => Math.floor(Date.now() / 1000);
	/** Unix 秒 → RFC3339（v4 wire 用字符串时间戳，不是数字）。 */
	const iso = (sec) =>
		new Date((sec == null ? now() : sec) * 1000)
			.toISOString()
			.replace(/\.\d{3}Z$/, "Z");
	const isoNow = () => iso(now());

	// clone 深拷贝演示数据，避免调用方改到 state 里的原对象。
	//
	// JSON.parse 会抛（带 undefined/循环引用时），而这个函数在每个 mock
	// handler 里都用——一处抛错就整页白屏。降级成原值比整个页面挂掉好：
	// 数据是假的，能看就行。
	const clone = (v) => {
		try {
			return JSON.parse(JSON.stringify(v));
		} catch (e) {
			console.warn("[mock] clone 失败，返回原值", e);
			return v;
		}
	};

	// mock 的 digest：从正文算一个**稳定**的假 sha256。
	//
	// 同一份输入永远同一个值 —— 这样「改脚本 → digest 变了 → 节点上是旧 digest」
	// 这条派生链在演示里也讲得通；用随机值会让每次刷新都变成另一个 digest。
	function mockDigest(seed) {
		const s = String(seed || "");
		let h1 = 0x811c9dc5 >>> 0;
		let h2 = 0x1000193 >>> 0;
		for (let i = 0; i < s.length; i++) {
			const c = s.charCodeAt(i);
			h1 = ((h1 ^ c) * 0x01000193) >>> 0;
			h2 = (h2 + c * 31 + i * 7) >>> 0;
		}
		const hex = (n) => (n >>> 0).toString(16).padStart(8, "0");
		const parts = [
			h1,
			h2,
			h1 ^ h2,
			h1 + h2,
			h1 * 3,
			h2 * 7,
			h1 ^ 0x5bf03635,
			h2 ^ 0x9e3779b9,
		];
		return "sha256:" + parts.map(hex).join("");
	}

	// 资源 id / 资源名也要**确定性**：种子顺序在每次加载里固定，深链
	// （trace.html?traceUID=… / message.html?id=…）刷新后必须还能打开。
	// 运行期新建的资源用递增序号，不参与随机。
	const API_VERSION = "anotify/v1";
	const ME = {
		id: "usr_mock_demo",
		username: "demo",
		displayName: "demo",
		role: "admin",
		disabled: false,
		createdAt: now() - 86400 * 30,
	};
	const ORG = "org_mock_demo";

	// resourceVersion 是**写并发令牌**（不是 metadata.version，也不是 configRevision）。
	// 种子给固定值，运行期每次写 +1；seed 从 100 起，避免和种子的 "1".."20" 撞。
	const nextRV = () => String(++state.allocationCounters[0].rvSeq);
	// 独立的创建序号在删除/快照恢复后仍保留；只在成功创建/commit 时分配。
	const nextUID = (kind) => kind + "_mock_created_" + (++state.allocationCounters[0].uidSeq);

	// 命名空间（cluster-scoped）。三个：默认 / 生产 / 团队 A。
	const NS_DEFAULT = "default";

	// agent 自升级：中枢自己带的 agent 二进制版本（真环境来自 internal/version.String()）。
	const AGENT_VERSION = "1.5.0+3fd5a59";

	// ---------- 演示数据 ----------
	//
	// 刻意做得像真的：能力有扇出/终端/缺节点/旧 digest，源三种 kind 各一，
	// 任务覆盖六个 phase 与三个 waitingReason，事件四种 reason。
	// 这样看页面时能看出各种状态长什么样，而不是一排"成功"。

	/**
	 * state —— mock 的唯一事实源。
	 *
	 * 这里只存**事实**（能力定义的 spec、源的真实 token、任务行、事件行、
	 * 部署事实、节点上报），status 一概不存：读的时候按 v4 规则现算
	 * （见 deriveCapabilityStatus / deriveNodeStatus / edgeList …）。
	 */
	const state = {
		// Cluster-scoped 分配事实随整个快照恢复，不属于任何 namespace 的资源事实。
		// 两个内部 counter 都不从公共 resourceVersion 反算。
		allocationCounters: [{ ns: null, rvSeq: 100, uidSeq: 0 }],
		namespaces: [],
		// ns 默认值（`GET/PUT /v1/namespaces/{name}/config` 的事实）：F2c 用。
		nsVars: {},
		nsConfig: {},
		systemConfig: { rv: "system-1", retention: { resultTtlSec: 604800, eventTtlSec: 2592000, taskTtlSec: 2592000 } },
		capabilities: [],
		sources: [],
		playbooks: [],
		playbookInstalls: [],
		tasks: [],
		events: [],
		nodes: [],
		// 部署事实：{ns, capability, version, digest, node, updatedAt}
		deployments: [],
		// 派发事实：任务派出了什么 / 剪掉了什么
		emissions: [],
		prunes: [],
		taskLogs: [],
		keys: [],
		devices: [],
		// passkey 设备注册会话（保留域；F2c 的 passkey-enroll 页消费）
		passkeyEnroll: [],
		auth: { passwordHash: mockDigest("mock-only-password"), passwordUpdatedAt: now() - 86400, loggedOut: typeof sessionStorage !== "undefined" && sessionStorage.getItem("anotify.mock.loggedOut") === "1" },
		// 中枢是否"带了 agent 二进制"（noAgentBinary() 演示开关；F2c 节点页消费）
		agentBinary: true,
		// 让下一次 upgrade_ack 失败（F2c 节点页消费）
		upgradeFailCode: "",
		// mock 响应延迟（毫秒）。120 ~ 300 让人看得见 loading 态；
		// 批量断言/截图时可 `MockApi.latency(0)` 关掉。
		latencyMs: 120,
	};

	// 辅助：按名字取能力（最高版本）—— MockApi.capByName 用它。
	function capByName(name, ns) {
		const list = state.capabilities.filter(
			(c) => c.name === name && (ns == null || c.ns === ns),
		);
		if (!list.length) return null;
		return list.reduce((a, b) => (b.version > a.version ? b : a));
	}

	// 从**最后一个** '@' 拆 `name@version`（与后端 SplitRef 同口径）：
	// 能力名里允许出现 '@'，而版本号一定是纯整数。
	function splitRef(ref) {
		const s = String(ref == null ? "" : ref).trim();
		const at = s.lastIndexOf("@");
		if (at <= 0 || at === s.length - 1) return { name: s, version: null };
		const v = Number(s.slice(at + 1));
		if (!Number.isInteger(v) || v <= 0) return { name: s, version: null };
		return { name: s.slice(0, at), version: v };
	}

	// 把引用解析成能力行；解析不到返回 null。
	//
	//   `name`     → 该名字的最高版本（= latest，升级自动生效）
	//   `name@ver` → 那一版（钉死）
	//
	// 版本历史在 mock 里存于 `row.versions`（归档行），所以钉死旧版本也要能解析：
	// 归档行返回一个只含 `{ns,name,version,digest,archived}` 的视图 —— 它有
	// 部署身份（digest），但没有当前版本才有的 spec/status。
	function resolveCapRef(ref, ns) {
		const r = splitRef(ref);
		const list = state.capabilities.filter((c) => c.ns === ns && c.name === r.name);
		if (!list.length) return null;
		if (r.version == null) return list.reduce((a, b) => (b.version > a.version ? b : a));
		const row = list[0];
		if (row.version === r.version) return row;
		const old = (row.versions || []).find((v) => v.version === r.version);
		if (!old) return null;
		return {
			ns: row.ns,
			name: row.name,
			version: old.version,
			digest: old.digest,
			archived: true,
		};
	}

	// ---------- 种子：命名空间 ----------
	(function initNamespaces() {
		const seed = [
			{ name: "default", displayName: "默认命名空间" },
			{ name: "prod", displayName: "生产" },
			{ name: "team-a", displayName: "团队 A" },
		];
		state.namespaces = seed.map((n, i) => ({
			name: n.name,
			displayName: n.displayName,
			uid: "ns_mock_" + n.name,
			rv: String(i + 1),
			createdAt: iso(now() - 86400 * 60),
		}));
		state.nsVars = {
			default: { NOTIFY_URL: "https://hooks.example/notify/default", SLACK_CHANNEL: "#general" },
			prod: { NOTIFY_URL: "https://hooks.example/notify/prod", SLACK_CHANNEL: "#prod" },
			"team-a": { NOTIFY_URL: "https://hooks.example/notify/team-a" },
		};
	})();

	for (const n of state.namespaces) state.nsConfig[n.name] = { rv: "ns-config-" + n.name, revision: "1" };

	// ---------- 种子：节点（cluster-scoped）----------
	//
	// `reports` 是**节点上报**的 mock 内部表示（真后端是 anode 的注册帧）：
	//   声明了哪些命令、能解析哪些值。它们不直接上 wire —— Node.status 只有
	//   契约里那些字段（见 deriveNodeStatus）。
	(function initNodes() {
		state.nodes = [
			{
				name: "anode-1",
				displayName: "构建机 1",
				spec: {
					// 中枢**授权**（管理员的意愿）
					namespaces: ["default", "prod"],
					vars: { TOOLCHAIN: "/opt/go1.25" },
					limits: { total: 4, perCapability: { "*": 2, fixer: 1 } },
					tags: ["build", "prod"],
				},
				// 本机申报（anode.yaml.namespaces）；生效 = 与 spec.namespaces 取交集
				declared: ["default", "prod"],
				facts: {
					hostIP: "10.0.0.7",
					hostname: "anode-1",
					os: "linux",
					arch: "amd64",
					capacity: { cpu: "8" },
					workRoot: "/var/lib/anode/work",
					homeDir: "/home/anode",
					localVars: { SOURCE_ROOT: "/srv/src" },
					agentVersion: AGENT_VERSION,
					instances: [{ slot: 0, taskId: "tsk_mock_2", attempt: 1, leaseId: "lease_mock_2", busy: true }],
					lastSeenAt: iso(now() - 15),
				},
				configRevision: "7",
				appliedRevision: "7",
				rv: "1",
				reports: {
					commands: ["git", "bash", "curl"],
					values: ["vars.SOURCE_ROOT", "vars.TOOLCHAIN"],
				},
			},
			{
				name: "anode-2",
				displayName: "构建机 2",
				spec: {
					namespaces: ["default"],
					vars: { TOOLCHAIN: "/opt/go1.25" },
					limits: { total: 2, perCapability: { "*": 1 } },
					tags: ["build"],
				},
				declared: ["default"],
				facts: {
					hostIP: "10.0.0.8",
					hostname: "anode-2",
					os: "linux",
					arch: "amd64",
					capacity: { cpu: "4" },
					workRoot: "/var/lib/anode/work",
					homeDir: "/home/anode2",
					localVars: { SOURCE_ROOT: "/srv/src" },
					agentVersion: "1.4.9",
					instances: [],
					lastSeenAt: iso(now() - 40),
				},
				// ConfigPending 的来源：configRevision 已经 +1，节点还没回报 applied
				configRevision: "8",
				appliedRevision: "7",
				rv: "2",
				reports: { commands: ["bash", "jq"], values: ["vars.SOURCE_ROOT"] },
			},
			{
				name: "anode-3",
				displayName: "团队 A 的机器",
				spec: {
					// 授权 team-a，但本机只申报了 default → 生效命名空间为空
					namespaces: ["team-a"],
					vars: {},
					limits: { total: 2, perCapability: { "*": 1 } },
					tags: ["team-a"],
				},
				declared: ["default"],
				facts: {
					hostIP: "10.0.0.9",
					hostname: "anode-3",
					os: "linux",
					arch: "arm64",
					capacity: { cpu: "4" },
					workRoot: "/var/lib/anode/work",
					homeDir: "/home/anode3",
					localVars: { SOURCE_ROOT: "/srv/src" },
					agentVersion: "1.4.9",
					instances: [],
					lastSeenAt: iso(now() - 90),
				},
				configRevision: "5",
				appliedRevision: "5",
				rv: "3",
				reports: { commands: ["git", "bash"], values: ["vars.SOURCE_ROOT"] },
			},
			{
				name: "anode-laptop",
				displayName: "笔记本（旧配置机）",
				spec: {
					namespaces: ["default"],
					vars: {},
					limits: { total: 1, perCapability: { "*": 1 } },
					tags: ["dev"],
				},
				declared: ["default"],
				facts: {
					hostIP: "10.0.0.10",
					hostname: "anode-laptop",
					os: "linux",
					arch: "amd64",
					capacity: { cpu: "4" },
					workRoot: "/home/anode/work",
					homeDir: "/home/anode",
					localVars: {},
					agentVersion: "1.4.9",
					instances: [],
					lastSeenAt: iso(now() - 120),
				},
				configRevision: "6",
				appliedRevision: "6",
				rv: "4",
				reports: { commands: ["git", "bash", "rsync"], values: ["vars.SOURCE_ROOT"] },
			},
			{
				name: "anode-offline",
				displayName: "生产机（离线）",
				spec: {
					namespaces: ["prod"],
					vars: {},
					limits: { total: 4, perCapability: { "*": 2 } },
					tags: ["prod"],
				},
				declared: ["prod"],
				facts: {
					hostIP: "10.0.0.11",
					hostname: "anode-offline",
					os: "linux",
					arch: "amd64",
					capacity: { cpu: "8" },
					workRoot: "/var/lib/anode/work",
					homeDir: "/home/anode",
					localVars: { SOURCE_ROOT: "/srv/src" },
					agentVersion: "1.4.9",
					instances: [],
					// 3 小时没心跳 → NodeOffline
					lastSeenAt: iso(now() - 10800),
				},
				configRevision: "4",
				appliedRevision: "4",
				rv: "5",
				reports: { commands: ["git", "bash", "docker"], values: ["vars.SOURCE_ROOT"] },
			},
		];
	})();

	// ---------- 种子：能力 ----------
	//
	// 内部行只存**定义**（spec + 展示字段 + digest）；status 读时派生。
	// `spec` 就是 v4 Capability Spec（params / next / run / requires / lifecycle / concurrency）。
	(function initCapabilities() {
		const run = (entrypoint, files, extra) =>
			Object.assign(
				{
					entrypoint: entrypoint,
					files: files || {},
				},
				extra || {},
			);
		const sh = (body) =>
			[
				"#!/usr/bin/env bash",
				"set -euo pipefail",
				"",
				'echo "capability: $ANOTIFY_CAPABILITY"',
			]
				.concat(body)
				.join("\n");

		const seeds = [
			{
				ns: "default",
				name: "fixer",
				version: 1,
				displayName: "修复入口",
				spec: {
					params: {
						type: "object",
						required: ["repo"],
						properties: {
							repo: { type: "string" },
							environment: { type: "string", default: "prod" },
						},
					},
					// 多下游（扇出）：一次 emit 只交给一个下游，两条下游就 emit 两次
					next: ["analyze-pr", "lint-fix@1"],
					run: run("run.sh", {
						"run.sh": sh(['anode emit analyze-pr --data \'{"repo":"x"}\'']),
					}),
					requires: { commands: ["git"], values: [] },
					lifecycle: { idempotent: false, maxAttempts: 3, resultTtlSec: 604800, timeoutSec: 900 },
					concurrency: { maxPerNode: 1 },
				},
				rv: "11",
			},
			{
				ns: "default",
				name: "analyze-pr",
				version: 2,
				displayName: "分析 PR",
				spec: {
					params: {
						type: "object",
						required: ["repo"],
						properties: {
							repo: { type: "string" },
							changedLines: { type: "integer", minimum: 0 },
						},
					},
					next: ["lint-fix", "run-tests"],
					run: run(
						"main.sh",
						{ "main.sh": sh(["./analyze --repo \"$ANOTIFY_PARAM_REPO\""]) },
						{
							env: {
								GIT_TERMINAL_PROMPT: "0",
								SOURCE_ROOT: { valueFrom: { fieldRef: { fieldPath: "vars.SOURCE_ROOT" } } },
							},
							workDir: "$(vars.SOURCE_ROOT)",
						},
					),
					requires: { commands: ["git"], values: ["vars.SOURCE_ROOT"] },
					lifecycle: { idempotent: false, maxAttempts: 3, resultTtlSec: 604800, timeoutSec: 1800 },
					concurrency: { maxPerNode: 2 },
				},
				rv: "12",
				// 安装来源：只记初次来源（复用/升级不覆写）；页面必须显示它
				installedFrom: "ins_mock_7f3a",
				// 版本历史：v1 的正文留在 versions 里（旧 digest 在节点上）
				versions: [
					{
						version: 1,
						digest: mockDigest("analyze-pr@1"),
						at: iso(now() - 86400 * 9),
						spec: { run: run("main.sh", { "main.sh": "#!/usr/bin/env bash\necho v1\n" }) },
					},
				],
			},
			{
				ns: "default",
				name: "lint-fix",
				version: 1,
				displayName: "应用修复",
				spec: {
					params: {
						type: "object",
						properties: { scope: { type: "string", enum: ["lint", "test", "all"] } },
					},
					next: ["run-tests"],
					run: run("main.sh", { "main.sh": sh(["./fix --scope \"${ANOTIFY_PARAM_SCOPE:-lint}\""]) }),
					// 只有 anode-2 装了 jq，而它配置待生效 → NodesAvailable=ConfigPending
					requires: { commands: ["jq"], values: [] },
					lifecycle: { idempotent: true, maxAttempts: 2, resultTtlSec: 604800, timeoutSec: 900 },
					concurrency: { maxPerNode: 1 },
				},
				rv: "13",
			},
			{
				ns: "default",
				name: "run-tests",
				version: 3,
				displayName: "跑测试",
				spec: {
					params: {
						type: "object",
						properties: {
							suite: { type: "string", enum: ["unit", "integration", "e2e"], default: "unit" },
							shard: { type: "integer", minimum: 1 },
						},
					},
					next: ["deploy-preview"],
					run: run("main.sh", { "main.sh": sh(["make test"]) }),
					requires: { commands: ["curl"], values: [] },
					lifecycle: { idempotent: true, maxAttempts: 3, resultTtlSec: 604800, timeoutSec: 1800 },
					concurrency: { maxPerNode: 1 },
				},
				rv: "14",
			},
			{
				ns: "default",
				name: "deploy-preview",
				version: 1,
				displayName: "部署预发",
				spec: {
					params: {
						type: "object",
						required: ["env"],
						properties: { env: { type: "string", enum: ["staging", "prod"] } },
					},
					// 终端能力：next 为空 = 跑完就结束
					next: [],
					run: run("main.sh", { "main.sh": sh(["./deploy \"$ANOTIFY_PARAM_ENV\""]) }),
					// ns 默认值里没有 SLACK_WEBHOOK → NodesAvailable=ValueMissing
					requires: { commands: [], values: ["vars.SLACK_WEBHOOK"] },
					lifecycle: { idempotent: false, maxAttempts: 1, resultTtlSec: 604800, timeoutSec: 600 },
					concurrency: { maxPerNode: 1 },
				},
				rv: "15",
			},
			{
				ns: "default",
				name: "notify",
				version: 1,
				displayName: "通知",
				spec: {
					params: {
						type: "object",
						required: ["text"],
						properties: { text: { type: "string" }, channel: { type: "string" } },
					},
					next: [],
					run: run("main.sh", { "main.sh": sh(["curl -fsS -X POST --data '{}' \"$NOTIFY_URL\""]) }),
					requires: { commands: ["curl"], values: [] },
					lifecycle: { idempotent: true, maxAttempts: 3, resultTtlSec: 604800, timeoutSec: 120 },
					concurrency: { maxPerNode: 1 },
				},
				rv: "16",
			},
			{
				ns: "default",
				name: "archive",
				version: 2,
				displayName: "归档产物",
				spec: {
					params: { type: "object", properties: {} },
					next: [],
					run: run("main.sh", { "main.sh": sh(["7z a out.7z artifacts/"]) }),
					// 没有节点同时装 rsync 与 7z → NodesAvailable=CommandMissing
					requires: { commands: ["rsync", "7z"], values: [] },
					lifecycle: { idempotent: true, maxAttempts: 1, resultTtlSec: 0, timeoutSec: 0 },
					concurrency: { maxPerNode: 1 },
				},
				rv: "17",
			},
			{
				ns: "default",
				name: "sketch",
				version: 1,
				displayName: "草稿（还没写脚本）",
				spec: {
					params: {},
					next: [],
					// 没有 entrypoint → Implemented=False / Invalid
					run: {},
					requires: {},
					lifecycle: {},
					concurrency: {},
				},
				rv: "18",
			},
			{
				ns: "prod",
				name: "report.send",
				version: 1,
				displayName: "发告警",
				spec: {
					params: {
						type: "object",
						required: ["channel", "title"],
						properties: {
							channel: { type: "string", enum: ["slack", "email"] },
							title: { type: "string" },
						},
					},
					// 指向一个不存在的能力 → DownstreamResolved=False / NotFound
					next: ["ghost-cap"],
					run: run("main.sh", { "main.sh": sh(["./report"]) }),
					requires: { commands: [], values: [] },
					lifecycle: { idempotent: true, maxAttempts: 3, resultTtlSec: 604800, timeoutSec: 120 },
					concurrency: { maxPerNode: 1 },
				},
				rv: "19",
			},
			{
				ns: "prod",
				name: "prod-deploy",
				version: 2,
				displayName: "生产部署",
				spec: {
					params: { type: "object", properties: { tag: { type: "string" } } },
					next: [],
					run: run("main.sh", { "main.sh": sh(["docker compose up -d"]) }),
					// prod 只有一台装了 docker 的节点，而它离线 → NodesAvailable=NodeOffline
					requires: { commands: ["docker"], values: [] },
					lifecycle: { idempotent: false, maxAttempts: 2, resultTtlSec: 604800, timeoutSec: 1800 },
					concurrency: { maxPerNode: 1 },
				},
				rv: "20",
			},
			{
				ns: "team-a",
				name: "team-task",
				version: 1,
				displayName: "团队任务",
				spec: {
					params: { type: "object", properties: {} },
					next: [],
					run: run("main.sh", { "main.sh": sh(["echo team"]) }),
					requires: { commands: ["git"], values: [] },
					lifecycle: { idempotent: false, maxAttempts: 3, resultTtlSec: 604800, timeoutSec: 900 },
					concurrency: { maxPerNode: 1 },
				},
				rv: "21",
			},
			{
				ns: "team-a",
				name: "team-missing",
				version: 1,
				displayName: "要一个没装的命令",
				spec: {
					params: { type: "object", properties: {} },
					next: [],
					run: run("main.sh", { "main.sh": sh(["terraform apply"]) }),
					// 没有任何节点装 terraform → NodesAvailable=NoMatchingNode
					requires: { commands: ["terraform"], values: [] },
					lifecycle: { idempotent: false, maxAttempts: 1, resultTtlSec: 604800, timeoutSec: 900 },
					concurrency: { maxPerNode: 1 },
				},
				rv: "22",
			},
		];

		const createdAt = iso(now() - 86400 * 30);
		state.capabilities = seeds.map((s) => ({
			ns: s.ns,
			name: s.name,
			displayName: s.displayName || "",
			version: s.version,
			spec: s.spec,
			digest: mockDigest(s.name + "@" + s.version),
			rv: s.rv,
			createdAt: createdAt,
			// versionAt = **当前版本**的创建时间（归档时写进行版本历史）
			versionAt: createdAt,
			versions: s.versions || [],
			installedFrom: s.installedFrom || null,
			uid: "cap_mock_" + s.ns + "_" + s.name,
		}));
	})();

	// ---------- 种子：部署事实 ----------
	//
	// 「某节点持有某 digest」—— 能力的 nodes[].state 与 Deployed 条件都从这里派生。
	// 刻意造三种：当前 digest（deployed）、旧 digest（stale）、没有行（missing）。
	(function initDeployments() {
		const dep = (ns, name, version, node, digestOverride) => {
			const cap = byVersion(ns, name, version);
			if (!cap) return;
			state.deployments.push({
				ns: ns,
				capability: name,
				version: version,
				digest: digestOverride || cap.digest,
				node: node,
				updatedAt: iso(now() - 3600),
			});
		};
		function byVersion(ns, name, version) {
			return state.capabilities.find(
				(c) => c.ns === ns && c.name === name && c.version === version,
			);
		}

		// fixer：anode-1 / anode-2 是当前 digest，anode-laptop 是旧 digest → Stale
		dep("default", "fixer", 1, "anode-1");
		dep("default", "fixer", 1, "anode-2");
		dep("default", "fixer", 1, "anode-laptop", mockDigest("fixer@1-old"));
		// analyze-pr：只有 anode-1 是当前版，anode-laptop 上还是 v1 → Stale
		dep("default", "analyze-pr", 2, "anode-1");
		const analyzeV1 = (byVersion("default", "analyze-pr", 2).versions || [])[0];
		dep(
			"default",
			"analyze-pr",
			2,
			"anode-laptop",
			analyzeV1 ? analyzeV1.digest : mockDigest("analyze-pr@1"),
		);
		// run-tests：三台候选节点都是当前 digest → Deployed=Valid
		dep("default", "run-tests", 3, "anode-1");
		dep("default", "run-tests", 3, "anode-2");
		dep("default", "run-tests", 3, "anode-laptop");
		// lint-fix / notify / archive / deploy-preview：只有部分节点
		dep("default", "lint-fix", 1, "anode-1");
		dep("default", "notify", 1, "anode-laptop");
		dep("default", "archive", 2, "anode-laptop", mockDigest("archive@1"));
		dep("default", "deploy-preview", 1, "anode-1");
		// prod
		dep("prod", "report.send", 1, "anode-1");
		dep("prod", "prod-deploy", 2, "anode-offline", mockDigest("prod-deploy@1"));
	})();

	// ---------- 种子：源 ----------
	//
	// 三种 kind 各一个（webhook 带 hookUrl/secretSet + targets 模板、cron 带表达式、
	// manual 带静态载荷）+ 一个停用的 + 一个没有 target 的（剪枝 no_target）。
	// token / secret 只在 mock 内部存：GET 投影只给 hookUrl + secretSet。
	(function initSources() {
		const src = (o) =>
			Object.assign(
				{
					ns: NS_DEFAULT,
					enabled: true,
					installedFrom: null,
					lastFiredAt: null,
					lastError: null,
					unapplied: false,
					createdAt: iso(now() - 86400 * 20),
				},
				o,
			);

		state.sources = [
			src({
				ns: "default",
				name: "deploy-hook",
				displayName: "部署成功回调",
				kind: "webhook",
				rv: "31",
				config: {
					token: "src_mock_deploy_hook",
					secret: "hmac_mock_secret_deploy_hook",
					payloadSchema: {
						type: "object",
						required: ["repo"],
						properties: {
							repo: { type: "string" },
							environment: { type: "string", enum: ["staging", "prod"] },
							commit: { type: "string" },
						},
					},
					sample: { repo: "acme/api", environment: "prod", commit: "9f2a1b3" },
				},
				targets: [
					{
						capability: "fixer",
						params: {
							repo: "$(payload.repo)",
							environment: "$(payload.environment)",
						},
					},
				],
				lastFiredAt: iso(now() - 600),
			}),
			src({
				ns: "default",
				name: "nightly",
				displayName: "每晚回归",
				kind: "cron",
				rv: "32",
				config: {
					expr: "0 3 * * *",
					tz: "UTC",
					payloadSchema: { type: "object", properties: { suite: { type: "string" } } },
					payload: { suite: "e2e" },
				},
				targets: [{ capability: "run-tests", params: { suite: "$(payload.suite)" } }],
				nextFireAt: iso(now() + 3600),
				lastFiredAt: iso(now() - 86400),
			}),
			src({
				ns: "default",
				name: "manual-smoke",
				displayName: "手动冒烟",
				kind: "manual",
				rv: "33",
				config: { payload: { repo: "acme/api" } },
				targets: [{ capability: "analyze-pr", params: { repo: "$(payload.repo)" } }],
				lastFiredAt: iso(now() - 1800),
			}),
			src({
				ns: "default",
				name: "paused-hook",
				displayName: "已停用的入口",
				kind: "webhook",
				enabled: false,
				rv: "34",
				config: { token: "src_mock_paused_hook", payloadSchema: null, sample: null },
				targets: [{ capability: "notify", params: { text: "$(payload.text)" } }],
			}),
			src({
				ns: "prod",
				name: "prod-hook",
				displayName: "生产告警入口",
				kind: "manual",
				rv: "36",
				config: { payload: { channel: "slack", title: "生产告警" } },
				// 目标能力存在，而它的 next 指向一个不存在的能力 ——
				// 图上会多一个 unknown 节点（DownstreamResolved=False 的可视化）
				targets: [
					{ capability: "report.send", params: { channel: "$(payload.channel)", title: "$(payload.title)" } },
				],
				lastFiredAt: iso(now() - 3000),
			}),
			src({
				ns: "prod",
				name: "prod-noop",
				displayName: "占位入口（还没接能力）",
				kind: "manual",
				rv: "35",
				config: { payload: {} },
				// 没有 target：事件照常入库，留一条 no_target 的剪枝痕
				targets: [],
			}),
		];
	})();

	// ---------- 种子：任务 ----------
	//
	// 六个 phase 全覆盖（pending / running / succeeded / failed / cancelled / abandoned）
	// + 三个 waitingReason（waiting_for_slot / retry_backoff / waiting_for_config）
	// + attempt>1 与 attempts[] 历史 + envDiagnostics 五种来源（只有名字，没有值）。
	// 时间戳一律 RFC3339 字符串（v4 wire）。
	(function initTasks() {
		const T1 = "trace_mock_t1";
		const T2 = "trace_mock_t2";
		const t = (sec) => iso(now() - sec);

		const add = (tk) =>
			state.tasks.push(
				Object.assign(
					{
						ns: "default",
						traceUID: T1,
						sequence: 1,
						sourceRef: null,
						parentRef: null,
						parentTaskId: null,
						attempt: 1,
						maxAttempts: 3,
						waitingReason: null,
						nextAttemptAt: null,
						nodeName: null,
						instanceId: null,
						leaseExpiresAt: null,
						configRevision: "7",
						namespaceVarsRevision: "12",
						envDiagnostics: [],
						result: { summary: null, artifacts: [], error: null },
						startedAt: null,
						finishedAt: null,
						attempts: [],
					},
					tk,
				),
			);

		// ---- trace 1：webhook → fixer 扇出 → analyze-pr / lint-fix → run-tests → deploy-preview ----
		add({
			name: "tsk_mock_1",
			sequence: 1,
			sourceRef: "deploy-hook",
			capability: "fixer",
			capabilityVersion: 1,
			params: { repo: "acme/api", environment: "prod" },
			phase: "succeeded",
			nodeName: "anode-1",
			instanceId: 1,
			startedAt: t(620),
			finishedAt: t(600),
			createdAt: t(630),
			// envDiagnostics：只记名字 + 来源（local / node / namespace / passthrough / builtin）
			envDiagnostics: [
				{ name: "SOURCE_ROOT", source: "local" },
				{ name: "TOOLCHAIN", source: "node" },
				{ name: "NOTIFY_URL", source: "namespace" },
				{ name: "GIT_SSH_KEY", source: "passthrough" },
				{ name: "PATH", source: "builtin" },
			],
			result: { summary: "已派发 2 条下游", artifacts: [], error: null },
		});
		add({
			name: "tsk_mock_2",
			sequence: 2,
			sourceRef: "deploy-hook",
			parentRef: "fixer",
			parentTaskId: "tsk_mock_1",
			capability: "analyze-pr",
			capabilityVersion: 2,
			params: { repo: "acme/api" },
			phase: "succeeded",
			nodeName: "anode-1",
			instanceId: 1,
			startedAt: t(598),
			finishedAt: t(560),
			createdAt: t(599),
			envDiagnostics: [
				{ name: "SOURCE_ROOT", source: "local" },
				{ name: "GIT_TERMINAL_PROMPT", source: "node" },
			],
			result: {
				summary: "发现 3 处问题",
				artifacts: [{ path: "artifacts/report.md", sha256: "fca851ec8492b3d9385bbc0b0c0ebc7583f456ae247b3a572a1c102dd16f2199" }],
				error: null,
			},
		});
		add({
			name: "tsk_mock_3",
			sequence: 2,
			sourceRef: "deploy-hook",
			parentRef: "fixer",
			parentTaskId: "tsk_mock_1",
			capability: "lint-fix",
			capabilityVersion: 1,
			params: { scope: "lint" },
			phase: "succeeded",
			nodeName: "anode-1",
			instanceId: 1,
			startedAt: t(598),
			finishedAt: t(570),
			createdAt: t(599),
			envDiagnostics: [{ name: "SOURCE_ROOT", source: "local" }],
			result: { summary: "已修 3 处", artifacts: [], error: null },
		});
		add({
			name: "tsk_mock_4",
			sequence: 3,
			sourceRef: "deploy-hook",
			parentRef: "analyze-pr@2",
			parentTaskId: "tsk_mock_2",
			capability: "run-tests",
			capabilityVersion: 3,
			params: { suite: "unit" },
			phase: "failed",
			attempt: 3,
			maxAttempts: 3,
			nodeName: "anode-1",
			instanceId: 1,
			startedAt: t(556),
			finishedAt: t(520),
			createdAt: t(558),
			envDiagnostics: [{ name: "SOURCE_ROOT", source: "local" }],
			result: {
				summary: "3 次尝试都失败",
				artifacts: [{ path: "artifacts/tests.txt", sha256: "c8fb72b784c34532bd3addcc67a6a78c0d6d47280ee3e9d23b70f40aa72635a6" }],
				error: { reason: "max_attempts_exceeded", message: "script_failed ×3" },
			},
			attempts: [
				{
					attempt: 1,
					nodeName: "anode-1",
					configRevision: "7",
					phase: "failed",
					startedAt: t(556),
					finishedAt: t(548),
					reason: "script_failed",
				},
				{
					attempt: 2,
					nodeName: "anode-1",
					configRevision: "7",
					phase: "failed",
					startedAt: t(540),
					finishedAt: t(530),
					reason: "script_failed",
				},
				{
					attempt: 3,
					nodeName: "anode-1",
					configRevision: "7",
					phase: "failed",
					startedAt: t(526),
					finishedAt: t(520),
					reason: "script_failed",
				},
			],
		});
		add({
			name: "tsk_mock_5",
			sequence: 3,
			sourceRef: "deploy-hook",
			parentRef: "analyze-pr@2",
			parentTaskId: "tsk_mock_2",
			capability: "run-tests",
			capabilityVersion: 3,
			params: { suite: "integration" },
			phase: "pending",
			attempt: 2,
			maxAttempts: 3,
			waitingReason: "retry_backoff",
			nextAttemptAt: iso(now() + 45),
			createdAt: t(500),
			envDiagnostics: [],
			attempts: [
				{
					attempt: 1,
					nodeName: "anode-1",
					configRevision: "7",
					phase: "failed",
					startedAt: t(498),
					finishedAt: t(490),
					reason: "infra_transient",
				},
			],
		});
		add({
			name: "tsk_mock_6",
			sequence: 4,
			sourceRef: "deploy-hook",
			parentRef: "lint-fix@1",
			parentTaskId: "tsk_mock_3",
			capability: "deploy-preview",
			capabilityVersion: 1,
			params: { env: "staging" },
			phase: "running",
			nodeName: "anode-1",
			instanceId: 1,
			leaseExpiresAt: iso(now() + 90),
			startedAt: t(480),
			createdAt: t(482),
			envDiagnostics: [{ name: "NOTIFY_URL", source: "namespace" }],
		});
		add({
			name: "tsk_mock_7",
			sequence: 5,
			sourceRef: "deploy-hook",
			parentRef: "deploy-preview@1",
			parentTaskId: "tsk_mock_6",
			capability: "notify",
			capabilityVersion: 1,
			params: { text: "部署完成", channel: "slack" },
			phase: "pending",
			waitingReason: "waiting_for_slot",
			nextAttemptAt: iso(now() + 10),
			createdAt: t(478),
			envDiagnostics: [],
		});

		// ---- trace 2：prod 夜间回归（succeeded / waiting_for_config / cancelled / abandoned）----
		add({
			name: "tsk_mock_8",
			ns: "prod",
			traceUID: T2,
			sequence: 1,
			sourceRef: "nightly",
			capability: "report.send",
			capabilityVersion: 1,
			params: { channel: "slack", title: "夜间回归" },
			phase: "succeeded",
			nodeName: "anode-1",
			instanceId: 1,
			startedAt: t(3600),
			finishedAt: t(3590),
			createdAt: t(3601),
			envDiagnostics: [{ name: "SLACK_CHANNEL", source: "namespace" }],
			result: { summary: "已发送", artifacts: [], error: null },
		});
		add({
			name: "tsk_mock_9",
			ns: "prod",
			traceUID: T2,
			sequence: 2,
			sourceRef: "nightly",
			parentRef: "report.send@1",
			parentTaskId: "tsk_mock_8",
			capability: "prod-deploy",
			capabilityVersion: 2,
			params: { tag: "v1.4.0" },
			phase: "pending",
			waitingReason: "waiting_for_config",
			nextAttemptAt: iso(now() + 120),
			configRevision: "8",
			createdAt: t(3588),
			envDiagnostics: [{ name: "SOURCE_ROOT", source: "local" }],
		});
		add({
			name: "tsk_mock_10",
			ns: "prod",
			traceUID: T2,
			sequence: 2,
			sourceRef: "nightly",
			parentRef: "report.send@1",
			parentTaskId: "tsk_mock_8",
			capability: "prod-deploy",
			capabilityVersion: 2,
			params: { tag: "v1.4.1" },
			phase: "cancelled",
			nodeName: "anode-offline",
			instanceId: 1,
			startedAt: t(3580),
			finishedAt: t(3560),
			createdAt: t(3581),
			envDiagnostics: [],
			result: {
				summary: "被用户取消",
				artifacts: [],
				error: { reason: "cancelled_by_user", message: "用户取消" },
			},
		});
		add({
			name: "tsk_mock_11",
			ns: "prod",
			traceUID: T2,
			sequence: 2,
			sourceRef: "nightly",
			parentRef: "report.send@1",
			parentTaskId: "tsk_mock_8",
			capability: "report.send",
			capabilityVersion: 1,
			params: { channel: "email", title: "补充说明" },
			phase: "abandoned",
			nodeName: "anode-1",
			instanceId: 1,
			startedAt: t(3550),
			finishedAt: t(3540),
			createdAt: t(3551),
			envDiagnostics: [],
			result: {
				summary: "已被放弃",
				artifacts: [],
				error: { reason: "abandoned_by_user", message: "用户放弃" },
			},
			attempts: [
				{
					attempt: 1,
					nodeName: "anode-1",
					configRevision: "7",
					phase: "abandoned",
					startedAt: t(3550),
					finishedAt: t(3540),
					reason: "abandoned_by_user",
				},
			],
		});

		// Task result bytes are managed copies, not node/user workDir URLs.
		state.taskArtifacts = [
			{ ns: "default", task: "tsk_mock_2", sha256: "fca851ec8492b3d9385bbc0b0c0ebc7583f456ae247b3a572a1c102dd16f2199", content: "Report: 3 issues found\n", expired: false },
			{ ns: "default", task: "tsk_mock_4", sha256: "c8fb72b784c34532bd3addcc67a6a78c0d6d47280ee3e9d23b70f40aa72635a6", content: "Expired unit test report\n", expired: true },
		];

		// W6 logs retain separate attempt archives. Waiting is not a new log run.
		state.taskLogs = [
			{
				task: "tsk_mock_2", attempt: 1, truncated: true,
				lines: [
					{ sequence: 1, stream: "stdout", text: "$ ./analyze --repo acme/api", at: t(598) },
					{ sequence: 2, stream: "stderr", text: "warning: shallow clone", at: t(590) },
					{ sequence: 3, stream: "stdout", text: "3 issues found", at: t(560) },
				],
			},
			{
				task: "tsk_mock_5", attempt: 1, truncated: true,
				lines: [
					{ sequence: 1, stream: "stdout", text: "integration attempt 1", at: t(498) },
					{ sequence: 2, stream: "stderr", text: "infra_transient: runner disconnected", at: t(490) },
				],
			},
			{ task: "tsk_mock_4", attempt: 2, truncated: false, lines: [
				{ sequence: 1, stream: "stderr", text: "attempt 2: script_failed", at: t(530) },
			] },
			{ task: "tsk_mock_4", attempt: 3, truncated: false, lines: [
				{ sequence: 1, stream: "stderr", text: "attempt 3: max_attempts_exceeded", at: t(520) },
			] },
			{ task: "tsk_mock_10", attempt: 1, unavailable: "node_offline" },
		];

		// Accepted emit facts: eventId identifies an independent call. fields
		// stays internal for Capability observed, never in W6 summary responses.
		state.emissions = [
			{ ns: "default", task: "tsk_mock_1", eventId: "evt_mock_2", to: "analyze-pr", version: 2, taskId: "tsk_mock_2", id: "review", pruned: null, fields: ["repo"], at: t(599) },
			{ ns: "default", task: "tsk_mock_1", eventId: "evt_emit_lint", to: "lint-fix", version: 1, taskId: "tsk_mock_3", pruned: null, fields: ["scope"], at: t(599) },
			{ ns: "default", task: "tsk_mock_2", eventId: "evt_emit_tests", to: "run-tests", version: 3, taskId: "tsk_mock_4", pruned: null, fields: ["suite"], at: t(558) },
			{ ns: "default", task: "tsk_mock_2", eventId: "evt_emit_integration", to: "run-tests", version: 3, taskId: "tsk_mock_5", id: "integration", pruned: null, fields: ["suite"], at: t(500) },
			{ ns: "default", task: "tsk_mock_2", eventId: "evt_mock_3", to: "run-tests", version: 3, taskId: null, pruned: { reason: "param_schema", detail: "run-tests@3 params.suite: expected unit|integration|e2e" }, fields: ["suite"], at: t(480) },
		];
		state.prunes = [
			{
				ns: "default",
				traceUID: T1,
				task: "tsk_mock_2",
				event: "evt_mock_3",
				to: "run-tests",
				reason: "param_schema",
				detail: "run-tests@3 params.suite: 期望 unit|integration|e2e",
			},
			{
				ns: "prod",
				traceUID: T2,
				// 挂在 prod-deploy 的**最新**任务上，这样 capability.status.observed.pruned
				// 也能看到它（observed 只看最近一个逻辑 task）
				task: "tsk_mock_10",
				event: "evt_mock_6",
				to: "prod-deploy",
				reason: "param_missing",
				detail: "需要 tag（payload 里没有这个字段）",
			},
		];
	})();

	// ---------- 种子：事件 ----------
	//
	// 四种 reason 全覆盖：Ingested / Emitted / Pruned / RoutingChanged（+ ConfigApplied）。
	// 事件是**只读事实**：spec 不可变，status 也是从派发事实汇总的。
	(function initEvents() {
		const T1 = "trace_mock_t1";
		const T2 = "trace_mock_t2";
		const t = (sec) => iso(now() - sec);

		const ev = (o) =>
			Object.assign(
				{
					ns: "default",
					type: "Normal",
					involvedObject: { kind: "Source", name: "deploy-hook", uid: "src_mock_deploy_hook" },
					traceUID: T1,
					deliveryId: null,
					payload: {},
					signature: null,
					dispatched: [],
					prunedCount: 0,
					deliveredAt: null,
				},
				o,
			);

		state.events = [
			ev({
				name: "evt_mock_1",
				reason: "Ingested",
				eventType: "deploy.succeeded",
				deliveryId: "evt-9f2a1b3c",
				involvedObject: { kind: "Source", name: "deploy-hook", uid: "src_mock_deploy_hook" },
				payload: { repo: "acme/api", environment: "prod", commit: "9f2a1b3" },
				signature: { algorithm: "hmac-sha256", verified: true },
				dispatched: [{ task: "tsk_mock_1", capability: "fixer" }],
				deliveredAt: t(630),
				createdAt: t(630),
			}),
			ev({
				name: "evt_mock_2",
				reason: "Emitted",
				eventType: "capability.run",
				deliveryId: "emit-tsk_mock_1-1",
				involvedObject: { kind: "Task", name: "tsk_mock_1", uid: "tsk_mock_1" },
				payload: { to: "analyze-pr", fields: ["repo"] },
				dispatched: [
					{ task: "tsk_mock_2", capability: "analyze-pr" },
					{ task: "tsk_mock_3", capability: "lint-fix" },
				],
				deliveredAt: t(599),
				createdAt: t(599),
			}),
			ev({
				name: "evt_mock_3",
				reason: "Pruned",
				type: "Warning",
				eventType: "capability.emit",
				deliveryId: "emit-tsk_mock_2-3",
				involvedObject: { kind: "Task", name: "tsk_mock_2", uid: "tsk_mock_2" },
				payload: { to: "run-tests", suite: "smoke" },
				prunedCount: 1,
				deliveredAt: t(560),
				createdAt: t(560),
			}),
			ev({
				name: "evt_mock_4",
				reason: "RoutingChanged",
				eventType: "source.targets",
				involvedObject: { kind: "Source", name: "deploy-hook", uid: "src_mock_deploy_hook" },
				traceUID: null,
				payload: { from: 1, to: 1, detail: "targets 重建入口边" },
				deliveredAt: t(4000),
				createdAt: t(4000),
			}),
			ev({
				name: "evt_mock_5",
				ns: "prod",
				reason: "Ingested",
				eventType: "nightly",
				traceUID: T2,
				deliveryId: "cron-nightly-1",
				involvedObject: { kind: "Source", name: "nightly", uid: "src_mock_nightly" },
				payload: { suite: "e2e" },
				dispatched: [{ task: "tsk_mock_8", capability: "report.send" }],
				deliveredAt: t(3601),
				createdAt: t(3601),
			}),
			ev({
				name: "evt_mock_6",
				ns: "prod",
				reason: "Pruned",
				type: "Warning",
				eventType: "capability.emit",
				traceUID: T2,
				deliveryId: "emit-tsk_mock_10-1",
				involvedObject: { kind: "Task", name: "tsk_mock_10", uid: "tsk_mock_10" },
				payload: { to: "prod-deploy" },
				prunedCount: 1,
				deliveredAt: t(3588),
				createdAt: t(3588),
			}),
			ev({
				name: "evt_mock_7",
				ns: "prod",
				reason: "ConfigApplied",
				eventType: "namespace.config",
				involvedObject: { kind: "Namespace", name: "prod", uid: "ns_mock_prod" },
				traceUID: null,
				payload: { namespaceVarsRevision: "12" },
				deliveredAt: t(7200),
				createdAt: t(7200),
			}),
		];
	})();

	// ---------- 种子：API Keys / 设备（保留域，形状不动）----------
	//
	// 每条记录都必须带 `enabled: boolean`：真后端 keyPublic **总是**回这个字段，
	// 种子漏掉就是形状漂移（页面靠 `k.enabled !== false` 兜底会与缺失态混淆）。
	(function initKeys() {
		state.keys = [
			{
				id: "key_mock_send",
				orgId: ORG,
				name: "webhook 发送",
				scopes: ["send"],
				prefix: "ant_send_mock",
				createdAt: now() - 86400,
				lastUsedAt: now() - 300,
				enabled: true,
				revoked: false,
			},
			{
				id: "key_mock_recv",
				orgId: ORG,
				name: "节点接收",
				scopes: ["recv"],
				prefix: "ant_recv_mock",
				createdAt: now() - 86400 * 2,
				lastUsedAt: null,
				enabled: true,
				revoked: false,
			},
		];
	})();

	// ---------- 响应构造 ----------
	//
	// Response 在浏览器里恒有，但 node:vm 单测环境可能没有 —— 没有时降级成
	// 一个形状相同的最小对象（ok/status/headers/json/text）。页面只读这些。
	function json(body, status) {
		const code = status || 200;
		if (typeof Response === "function") {
			return new Response(JSON.stringify(body), {
				status: code,
				headers: { "Content-Type": "application/json; charset=utf-8" },
			});
		}
		return {
			ok: code >= 200 && code < 300,
			status: code,
			headers: { get: () => "application/json; charset=utf-8" },
			json: async () => body,
			text: async () => JSON.stringify(body),
		};
	}

	// 错误体统一 `Status`（api-conventions §4）。`details.current.resourceVersion`
	// 只在 409 冲突里出现 —— 前端 `api()` 靠它提示「别人改过了」。
	function statusErr(code, reason, message, details) {
		return json(
			{
				apiVersion: API_VERSION,
				kind: "Status",
				status: "Failure",
				code: code,
				reason: reason,
				message: message,
				details: Object.assign({ causes: [] }, details || {}),
			},
			code,
		);
	}

	const notFound = (what) => statusErr(404, "NotFound", what + "不存在");
	const badRequest = (reason, message) => statusErr(400, reason || "BadRequest", message);
	const conflict = (message, row) =>
		statusErr(409, "resource_version_conflict", message || "resourceVersion 已过期，请刷新后重试", {
			current: { resourceVersion: row && row.rv != null ? String(row.rv) : null },
		});

	// 列表信封：`KindList.items` + 可选顶层 `warnings[]`（与四键并列）。
	function listOf(kind, items, warnings) {
		const out = { apiVersion: API_VERSION, kind: kind, metadata: {}, items: items };
		if (warnings && warnings.length) out.warnings = warnings;
		return out;
	}

	// 写请求的公共校验（v4 纪律）：
	//   ① 顶层只认四个键（+ warnings），未知字段 → 400 unknown_field；
	//   ② `status` 只读 → 400 status_not_writable；
	//   ③ kind 必须匹配 → 400 kind_mismatch；
	//   ④ metadata.namespace 与路径不符 → 400 namespace_mismatch。
	const ENVELOPE_KEYS = ["apiVersion", "kind", "metadata", "spec", "status"];
	function validateWriteBody(body, kind, ns) {
		if (!body || typeof body !== "object") return badRequest("BadRequest", "请求体必须是 JSON 对象");
		for (const k of Object.keys(body)) {
			if (ENVELOPE_KEYS.indexOf(k) < 0 && k !== "warnings") {
				return badRequest("unknown_field", "未知顶层字段 " + k);
			}
		}
		if (body.status != null) {
			return badRequest(
				"status_not_writable",
				"status 只读：它由服务端从事实派生，写请求必须省略",
			);
		}
		if (body.kind != null && body.kind !== kind) {
			return badRequest("kind_mismatch", "kind 必须是 " + kind + "（收到 " + body.kind + "）");
		}
		if (ns && body.metadata && body.metadata.namespace && body.metadata.namespace !== ns) {
			return badRequest(
				"namespace_mismatch",
				"metadata.namespace 与路径里的命名空间不一致",
			);
		}
		return null;
	}

	// PATCH/PUT 的 CAS：缺失 400 resource_version_required，旧值 409 + details.current。
	function checkCas(body, row) {
		const rv = body && body.metadata ? body.metadata.resourceVersion : null;
		if (rv == null || rv === "") {
			return badRequest(
				"resource_version_required",
				"写已有资源必须带 metadata.resourceVersion（先刷新再改）",
			);
		}
		if (String(rv) !== String(row.rv)) return conflict("resourceVersion 已过期，请刷新后重试", row);
		return null;
	}

	// DELETE 的 CAS 传输走 `?resourceVersion=`（契约 §2.8.2 假设）。
	function checkCasQuery(query, row) {
		const rv = query.get("resourceVersion");
		if (rv == null || rv === "") {
			return badRequest(
				"resource_version_required",
				"删除必须带 ?resourceVersion=（先刷新再删）",
			);
		}
		if (String(rv) !== String(row.rv)) return conflict("resourceVersion 已过期，请刷新后重试", row);
		return null;
	}

	// ---------- 派生：节点 ----------
	//
	// 生效命名空间 = 「本机申报」（anode.yaml）∩「中枢授权」（Node.spec.namespaces）；
	// 任一侧 `"*"` = 全部。授权只能收紧，不能放宽 —— 所以交集为空是**合法**结果
	// （这台机器对该 ns 就是不可用），不是数据错误。
	function nodeEffectiveNamespaces(row) {
		const all = state.namespaces.map((n) => n.name);
		const expand = (list) =>
			!Array.isArray(list) || !list.length
				? []
				: list.indexOf("*") >= 0
					? all.slice()
					: list.slice();
		const declared = expand(row.declared || []);
		const authorized = expand((row.spec && row.spec.namespaces) || ["*"]);
		return declared.filter((ns) => authorized.indexOf(ns) >= 0);
	}

	// 心跳 5 分钟内算在线（与真后端的离线判定同一量级）。
	function nodeOnline(row) {
		const seen = row.facts && row.facts.lastSeenAt ? Date.parse(row.facts.lastSeenAt) : NaN;
		if (!Number.isFinite(seen)) return false;
		return Date.now() - seen <= 5 * 60 * 1000;
	}

	// 配置是否已生效：appliedRevision 落后 configRevision 就是 ConfigPending。
	function nodeSynced(row) {
		return String(row.appliedRevision) === String(row.configRevision);
	}

	function deriveNodeStatus(row, nsContext) {
		const f = row.facts || {};
		const localVars = clone(f.localVars || {});
		const nodeVars = clone((row.spec && row.spec.vars) || {});
		const machineVars = clone(f.machineVars || Object.assign({}, nodeVars, localVars));
		const st = {
			hostIP: f.hostIP || "",
			hostname: f.hostname || row.name,
			os: f.os || "",
			arch: f.arch || "",
			capacity: clone(f.capacity || {}),
			workRoot: f.workRoot || "",
			homeDir: f.homeDir || "",
			online: nodeOnline(row),
			draining: row.draining === true,
			localLimits: clone(f.localLimits || {}),
			machineLimits: clone(f.machineLimits || { total: f.localLimits?.total ?? row.spec?.limits?.total ?? 1, perCapability: { ...(row.spec?.limits?.perCapability || {}), ...(f.localLimits?.perCapability || {}) } }),
			overrideStale: row.overrideStale === true,
			localVars: localVars,
			nodeVars: nodeVars,
			// 机器级合并结果：**没有**一份扁平 effectiveVars 假装覆盖全部 ns
			machineVars: machineVars,
			configRevision: String(row.configRevision),
			appliedRevision: String(row.appliedRevision),
			appliedHash: mockDigest("applied-" + row.name + "-" + row.appliedRevision),
			effectiveNamespaces: nodeEffectiveNamespaces(row),
			agentVersion: f.agentVersion || "",
			...(Array.isArray(f.instances) ? { instances: clone(f.instances) } : {}),
			lastSeenAt: f.lastSeenAt || null,
		};
		// `?namespace=` 才给 ns 上下文：三层配置的第三层与合并结果。
		if (nsContext) {
			const nsVars = clone(state.nsVars[nsContext] || {});
			st.namespaceVars = nsVars;
			st.effectiveVars = Object.assign({}, nsVars, nodeVars, localVars);
			st.namespaceVarsRevision = state.nsConfig[nsContext]?.revision ?? null;
			st.context = { namespace: nsContext };
		}
		return st;
	}

	function nodeResource(row, nsContext) {
		return {
			apiVersion: API_VERSION,
			kind: "Node",
			metadata: {
				name: row.name,
				uid: row.uid || "node_mock_" + row.name,
				resourceVersion: String(row.rv),
				creationTimestamp: row.createdAt || iso(now() - 86400 * 45),
				displayName: row.displayName || "",
			},
			spec: {
				namespaces: clone((row.spec && row.spec.namespaces) || []),
				vars: clone((row.spec && row.spec.vars) || {}),
				limits: clone((row.spec && row.spec.limits) || {}),
				tags: clone((row.spec && row.spec.tags) || []),
			},
			status: deriveNodeStatus(row, nsContext),
		};
	}

	function namespaceResource(row) {
		const count = (list) => list.filter((x) => x.ns === row.name).length;
		return {
			apiVersion: API_VERSION,
			kind: "Namespace",
			metadata: {
				name: row.name,
				uid: row.uid,
				resourceVersion: String(row.rv),
				creationTimestamp: row.createdAt,
				displayName: row.displayName || "",
			},
			spec: {},
			status: {
				counts: {
					capabilities: count(state.capabilities),
					sources: count(state.sources),
					tasks: count(state.tasks),
					nodes: state.nodes.filter((n) => nodeEffectiveNamespaces(n).indexOf(row.name) >= 0)
						.length,
				},
			},
		};
	}

	// ---------- 派生：能力 status ----------
	//
	// 四个 condition 的 reason 用 api-conventions §4.3 的封闭词表：
	//   Implemented        Valid | Invalid
	//   NodesAvailable     Valid | NoMatchingNode | CommandMissing | ValueMissing | ConfigPending | NodeOffline | NamespaceUnauthorized
	//   DownstreamResolved Valid | NotFound
	//   Deployed           Valid | NotDeployed | Stale
	//
	// 全部在**读时**从事实算：capabilities.spec / nodes 上报 / deployments / tasks / events。
	function capCandidates(ns) {
		return state.nodes.filter((n) => nodeEffectiveNamespaces(n).indexOf(ns) >= 0);
	}

	function nodeHas(node, required) {
		const have = (node.reports && node.reports.commands) || [];
		return required.every((c) => have.indexOf(c) >= 0);
	}
	function nodeHasValues(node, required) {
		const have = (node.reports && node.reports.values) || [];
		return required.every((v) => have.indexOf(v) >= 0);
	}
	function requiredParams(cap) {
		const params = (cap.spec && cap.spec.params) || {};
		return Array.isArray(params.required) ? params.required.slice() : [];
	}

	function deriveCapabilityStatus(row) {
		const ns = row.ns;
		const candidates = capCandidates(ns);
		const req = (row.spec && row.spec.requires) || {};
		const needCmds = Array.isArray(req.commands) ? req.commands : [];
		const needVals = Array.isArray(req.values) ? req.values : [];

		const conditions = [];

		// ① Implemented：有没有可执行入口（v4 的能力声明 spec.run.entrypoint）
		const entry = row.spec && row.spec.run ? row.spec.run.entrypoint : "";
		const implemented = !!(entry && String(entry).trim());
		conditions.push({
			type: "Implemented",
			status: implemented ? "True" : "False",
			reason: implemented ? "Valid" : "Invalid",
			message: implemented ? "" : "spec.run.entrypoint 未声明，节点上跑不起来",
		});

		// ② NodesAvailable：能不能找到**现在就能跑**的节点
		const missingCmds = (n) => needCmds.filter((c) => !nodeHas(n, [c]));
		const missingVals = (n) => needVals.filter((v) => !nodeHasValues(n, [v]));
		const usable = candidates.filter(
			(n) => missingCmds(n).length === 0 && missingVals(n).length === 0,
		);
		const ready = usable.filter((n) => nodeOnline(n) && nodeSynced(n));
		let nodesCond;
		if (ready.length) {
			nodesCond = { status: "True", reason: "Valid", message: "" };
		} else if (usable.length) {
			if (usable.every((n) => !nodeOnline(n))) {
				nodesCond = {
					status: "False",
					reason: "NodeOffline",
					message: "候选节点全部离线：" + usable.map((n) => n.name).join("、"),
				};
			} else {
				const pendingNodes = usable.filter((n) => !nodeSynced(n)).map((n) => n.name);
				nodesCond = {
					status: "False",
					reason: "ConfigPending",
					message: "配置还没生效（appliedRevision 落后）：" + pendingNodes.join("、"),
				};
			}
		} else if (!candidates.length) {
			const anyNodeFits = state.nodes.some(
				(n) => missingCmds(n).length === 0 && missingVals(n).length === 0,
			);
			nodesCond = anyNodeFits
				? {
						status: "False",
						reason: "NamespaceUnauthorized",
						message: "有节点能满足要求，但都没有授权到命名空间 " + ns,
					}
				: {
						status: "False",
						reason: "NoMatchingNode",
						message: "没有节点声明这些要求：" + needCmds.concat(needVals).join("、"),
					};
		} else if (candidates.some((n) => missingCmds(n).length)) {
			const miss = [];
			for (const n of candidates) for (const c of missingCmds(n)) if (miss.indexOf(c) < 0) miss.push(c);
			nodesCond = {
				status: "False",
				reason: "CommandMissing",
				message: "没有节点同时装了这些命令：" + miss.join("、"),
			};
		} else {
			const miss = [];
			for (const n of candidates) for (const v of missingVals(n)) if (miss.indexOf(v) < 0) miss.push(v);
			nodesCond = {
				status: "False",
				reason: "ValueMissing",
				message: "没有节点能解析这些值：" + miss.join("、"),
			};
		}
		conditions.push(
			Object.assign({ type: "NodesAvailable" }, nodesCond),
		);

		// ③ DownstreamResolved：spec.next 的每个引用能不能解析
		const next = Array.isArray(row.spec && row.spec.next) ? row.spec.next : [];
		const resolvedNext = next.map((ref) => {
			const cap = resolveCapRef(ref, ns);
			const parsed = splitRef(ref);
			if (!cap) {
				return {
					name: parsed.name,
					version: parsed.version,
					missing: true,
					reason: "NotFound",
					ref: ref,
				};
			}
			return {
				name: cap.name,
				version: cap.version,
				digest: cap.digest,
				ready: true,
				missing: false,
				params: { required: requiredParams(cap) },
				ref: ref,
			};
		});
		const missingNext = resolvedNext.filter((r) => r.missing);
		conditions.push({
			type: "DownstreamResolved",
			status: missingNext.length ? "False" : "True",
			reason: missingNext.length ? "NotFound" : "Valid",
			message: missingNext.length
				? "下游解析不到：" + missingNext.map((r) => r.name).join("、")
				: "",
		});

		// ④ Deployed：候选节点手上是不是**当前 digest**
		const deps = state.deployments.filter(
			(d) => d.ns === ns && d.capability === row.name,
		);
		const nodeStates = candidates.map((n) => {
			const d = deps.find((x) => x.node === n.name);
			const state = !d ? "missing" : d.digest === row.digest ? "deployed" : "stale";
			return { name: n.name, state: state };
		});
		const stale = nodeStates.filter((s) => s.state === "stale");
		const deployedN = nodeStates.filter((s) => s.state === "deployed").length;
		let depCond;
		if (!nodeStates.length) {
			depCond = { status: "False", reason: "NotDeployed", message: "没有候选节点" };
		} else if (stale.length) {
			depCond = {
				status: "False",
				reason: "Stale",
				message: stale.length + " 台节点持有旧 digest（" + stale.map((s) => s.name).join("、") + "）",
			};
		} else if (deployedN === nodeStates.length) {
			depCond = { status: "True", reason: "Valid", message: "" };
		} else {
			depCond = {
				status: "False",
				reason: "NotDeployed",
				message: nodeStates.length - deployedN + " 台节点还没有这个版本",
			};
		}
		conditions.push(Object.assign({ type: "Deployed" }, depCond));

		// observed：最近一个逻辑 task 的 emits / pruned + 任务计数（明细数组保留，不只给计数）
		const tasks = state.tasks.filter((t) => t.ns === ns && t.capability === row.name);
		const latest = tasks
			.slice()
			.sort((a, b) => String(b.createdAt).localeCompare(String(a.createdAt)))[0];
		const emits = latest ? state.emissions.filter((e) => e.task === latest.name) : [];
		const prunes = latest ? state.prunes.filter((p) => p.task === latest.name) : [];

		return {
			ready: conditions.every((c) => c.status === "True"),
			version: row.version,
			digest: row.digest,
			conditions: conditions,
			nodes: nodeStates,
			// resolved 的每条都是「解析结果」，不是原始引用：`ref` 只是内部拼装字段
			resolved: {
				next: resolvedNext.map((r) => {
					const out = Object.assign({}, r);
					delete out.ref;
					return out;
				}),
			},
			observed: {
				taskId: latest ? latest.name : null,
				emits: emits.map((e) => ({ to: e.to, fields: clone(e.fields || []), ok: true })),
				pruned: prunes.map((p) => ({ to: p.to, reason: p.reason, detail: p.detail })),
				tasks: {
					total: tasks.length,
					failed: tasks.filter((t) => t.phase === "failed").length,
				},
			},
			installedFrom: row.installedFrom || null,
		};
	}

	function capabilityResource(row) {
		return {
			apiVersion: API_VERSION,
			kind: "Capability",
			metadata: {
				name: row.name,
				namespace: row.ns,
				uid: row.uid,
				version: row.version,
				resourceVersion: String(row.rv),
				creationTimestamp: row.createdAt,
				displayName: row.displayName || "",
			},
			spec: clone(row.spec || {}),
			status: deriveCapabilityStatus(row),
		};
	}

	// ---------- 派生：源 status ----------
	//
	// **GET 绝不回显 token / secret**：只给 `config.hookUrl`（授权读）与
	// `config.secretSet`（布尔）。投影是白名单式的（逐字段复制），不是
	// "删掉敏感字段" —— 后者漏一个键就泄漏。
	function sourceHookUrl(row) {
		if (row.kind !== "webhook") return null;
		const token = row.config && row.config.token;
		return token ? "/v1/hooks/" + token : null;
	}

	function sourceResolvedTargets(row) {
		return (row.targets || []).map((t) => {
			const cap = resolveCapRef(t.capability, row.ns);
			const parsed = splitRef(t.capability);
			if (!cap) {
				return {
					capability: t.capability,
					name: parsed.name,
					version: parsed.version,
					missing: true,
					reason: "NotFound",
					params: clone(t.params || {}),
				};
			}
			return {
				capability: t.capability,
				name: cap.name,
				version: cap.version,
				digest: cap.digest,
				missing: false,
				params: clone(t.params || {}),
			};
		});
	}

	function deriveSourceStatus(row) {
		const targets = sourceResolvedTargets(row);
		let health = "ok";
		if (!row.enabled) health = "disabled";
		else if (!targets.length) health = "blocked";
		else if (targets.some((t) => t.missing)) health = "blocked";
		else if (row.unapplied) health = "unapplied";
		return {
			hookUrl: sourceHookUrl(row),
			lastFiredAt: row.lastFiredAt || null,
			nextFireAt: row.nextFireAt || null,
			enabled: !!row.enabled,
			health: health,
			// 契约 api-conventions §7.2 定的是 **boolean**：所有 target 都解析到才 true。
			// 没有任何 target 不算“解析完成”（这种源是 health=blocked），明细不在这里。
			targetsResolved: targets.length > 0 && targets.every((t) => !t.missing),
			lastError: row.lastError || null,
			installedFrom: row.installedFrom || null,
		};
	}

	function sourceResource(row) {
		const c = row.config || {};
		const cfg = {};
		if (row.kind === "webhook") {
			cfg.hookUrl = sourceHookUrl(row);
			cfg.secretSet = !!(c.secret && String(c.secret).length);
		}
		if (row.kind === "cron") {
			cfg.expr = c.expr || "";
			cfg.tz = c.tz || "UTC";
		}
		if (c.payloadSchema != null) cfg.payloadSchema = clone(c.payloadSchema);
		if (c.sample != null) cfg.sample = clone(c.sample);
		if (c.payload != null) cfg.payload = clone(c.payload);
		return {
			apiVersion: API_VERSION,
			kind: "Source",
			metadata: {
				name: row.name,
				namespace: row.ns,
				uid: row.uid || "src_mock_" + row.name,
				resourceVersion: String(row.rv),
				creationTimestamp: row.createdAt,
				displayName: row.displayName || "",
			},
			spec: {
				kind: row.kind,
				enabled: !!row.enabled,
				config: cfg,
				targets: clone(row.targets || []),
			},
			status: deriveSourceStatus(row),
		};
	}

	// ---------- 派生：任务 / 事件 / trace / edges / deployments ----------
	function capDigestOf(row) {
		const cap = state.capabilities.find(
			(c) => c.ns === row.ns && c.name === row.capability,
		);
		return cap ? cap.digest : null;
	}

	function taskStatus(row) {
		return {
			phase: String(row.phase || "pending").toLowerCase(),
			waitingReason: row.waitingReason || null,
			nextAttemptAt: row.nextAttemptAt || null,
			nodeName: row.nodeName || null,
			instanceId: row.instanceId == null ? null : row.instanceId,
			attempt: row.attempt == null ? 1 : row.attempt,
			maxAttempts: row.maxAttempts == null ? 3 : row.maxAttempts,
			leaseExpiresAt: row.leaseExpiresAt || null,
			startedAt: row.startedAt || null,
			finishedAt: row.finishedAt || null,
			configRevision: row.configRevision == null ? null : String(row.configRevision),
			namespaceVarsRevision:
				row.namespaceVarsRevision == null ? null : String(row.namespaceVarsRevision),
			// 只记名字 + 来源：值只在本机，中枢拿不到（也不需要）
			envDiagnostics: (row.envDiagnostics || []).map((e) => ({
				name: e.name,
				source: e.source,
			})),
			result: {
				summary:
					row.result && row.result.summary != null ? row.result.summary : null,
				artifacts: clone((row.result && row.result.artifacts) || []),
				error: row.result && row.result.error ? clone(row.result.error) : null,
			},
			attempts: clone(row.attempts || []),
		};
	}

	function taskResource(row) {
		return {
			apiVersion: API_VERSION,
			kind: "Task",
			metadata: {
				name: row.name,
				namespace: row.ns,
				uid: "tsk_mock_uid_" + row.name,
				resourceVersion: String(row.rv || "1"),
				creationTimestamp: row.createdAt,
			},
			spec: {
				traceUID: row.traceUID || null,
				sequence: row.sequence == null ? 1 : row.sequence,
				sourceRef: row.sourceRef || null,
				parentRef: row.parentRef || null,
				parentTaskId: row.parentTaskId || null,
				capability: {
					name: row.capability,
					version: row.capabilityVersion,
					digest: capDigestOf(row),
				},
				params: clone(row.params || {}),
			},
			status: taskStatus(row),
		};
	}

	function eventResource(ev) {
		return {
			apiVersion: API_VERSION,
			kind: "Event",
			metadata: {
				name: ev.name,
				namespace: ev.ns,
				uid: "evt_mock_uid_" + ev.name,
				resourceVersion: "1",
				creationTimestamp: ev.createdAt,
			},
			spec: {
				reason: ev.reason,
				type: ev.type || "Normal",
				involvedObject: clone(ev.involvedObject || {}),
				traceUID: ev.traceUID || null,
				eventType: ev.eventType || null,
				deliveryId: ev.deliveryId || null,
				payload: clone(ev.payload || {}),
				signature: ev.signature ? clone(ev.signature) : null,
			},
			status: {
				dispatched: clone(ev.dispatched || []),
				pruned: ev.prunedCount ? (() => {
					const facts = state.prunes.filter(p => p.ns === ev.ns && p.event === ev.name);
					return facts.length ? { reason: facts[0].reason, detail: facts[0].detail } : null;
				})() : null,
				deliveredAt: ev.deliveredAt || null,
			},
		};
	}

	function traceDoc(uid, ns) {
		const tasks = state.tasks.filter((t) => t.traceUID === uid && t.ns === ns);
		const events = state.events.filter((e) => e.traceUID === uid && e.ns === ns);
		if (!tasks.length && !events.length) return null;
		const pruned = state.prunes
			.filter((p) => p.traceUID === uid && p.ns === ns)
			.map((p) => ({
				to: p.to,
				reason: p.reason,
				detail: p.detail,
				event: p.event,
				task: p.task,
			}));
		return {
			apiVersion: API_VERSION,
			kind: "Trace",
			traceUID: uid,
			namespace: ns,
			events: events.map(eventResource),
			tasks: tasks.map(taskResource),
			pruned: pruned,
		};
	}

	// edges 只读投影：能力边来自 `spec.next`，源入口边来自 `source.spec.targets`。
	// **没有 port / priority / mapping**：v4 没有端口，也没有候选排序。
	function edgeItems(ns) {
		const items = [];
		for (const cap of state.capabilities.filter((c) => c.ns === ns)) {
			const next = Array.isArray(cap.spec && cap.spec.next) ? cap.spec.next : [];
			next.forEach((ref, i) => {
				items.push({
					fromIsSource: false,
					fromRef: cap.name,
					toRef: ref,
					position: i + 1,
				});
			});
		}
		for (const src of state.sources.filter((s) => s.ns === ns)) {
			(src.targets || []).forEach((t, i) => {
				items.push({
					fromIsSource: true,
					fromRef: src.name,
					toRef: t.capability,
					position: i + 1,
				});
			});
		}
		return items;
	}

	// deployments 只读事实：一行 = 「某节点持有某 digest」。
	// 页面要读的 `capability / capabilityId / name / version / digest / node / state`
	// 都是 v4 可用字段（没有 v3 的 port / status 九态）。
	function deploymentItems(ns) {
		return state.deployments
			.filter((d) => d.ns === ns)
			.map((d) => {
				const cap = state.capabilities.find(
					(c) => c.ns === ns && c.name === d.capability && c.version === d.version,
				);
				const stateStr = !cap ? "missing" : cap.digest === d.digest ? "deployed" : "stale";
				return {
					apiVersion: API_VERSION,
					kind: "Deployment",
					metadata: {
						name: "dep_mock_" + d.capability + "_" + d.node,
						namespace: ns,
						creationTimestamp: d.updatedAt,
					},
					spec: {
						capability: { name: d.capability, version: d.version, digest: d.digest },
						node: d.node,
					},
					status: { updatedAt: d.updatedAt, current: stateStr === "deployed" },
					// 展平读数（页面按行读；都是 v4 事实，不是 v3 别名）
					capability: d.capability,
					capabilityId: d.capability,
					name: d.capability + "/" + d.node,
					version: d.version,
					digest: d.digest,
					node: d.node,
					updatedAt: d.updatedAt,
					state: stateStr,
				};
			});
	}

	// ---------- Stats（现有读模型形状，namespace 显式过滤）----------
	function statsDoc(ns) {
		const inNs = (list) => list.filter((x) => ns === "*" || x.ns === ns);
		const evs = inNs(state.events);
		const tks = inNs(state.tasks);
		const dayKey = (d) => {
			const m = String(d.getMonth() + 1).padStart(2, "0");
			const dd = String(d.getDate()).padStart(2, "0");
			return d.getFullYear() + "-" + m + "-" + dd;
		};
		const today = dayKey(new Date());
		const byPhase = (p) => tks.filter((t) => t.phase === p).length;
		const daily = (() => {
			const counts = new Map();
			for (const e of evs) {
				const d = new Date(e.createdAt);
				if (Number.isNaN(d.getTime())) continue;
				const k = dayKey(d);
				counts.set(k, (counts.get(k) || 0) + 1);
			}
			const out = [];
			for (let i = 0; i < 7; i++) {
				const d = new Date();
				d.setDate(d.getDate() - (6 - i));
				const key = dayKey(d);
				const real = counts.get(key) || 0;
				out.push({
					day: key,
					count: real,
				});
			}
			return out;
		})();
		const nodesInScope =
			ns === "*"
				? state.nodes.slice()
				: state.nodes.filter((n) => nodeEffectiveNamespaces(n).indexOf(ns) >= 0);
		return {
			total: evs.length,
			today: evs.filter((e) => {
				const d = new Date(e.createdAt);
				return !Number.isNaN(d.getTime()) && dayKey(d) === today;
			}).length,
			// No device delivery facts in this preview, never count terminal tasks as push success.
			delivered: 0,
			deliveryTotal: 0,
			deviceCount: (state.devices || []).filter((d) => d.enabled !== false).length,
			totalEvents: evs.length,
			succeeded: byPhase("succeeded"),
			failed: byPhase("failed"),
			running: byPhase("running"),
			pending: byPhase("pending"),
			nodesOnline: nodesInScope.filter((n) => nodeOnline(n)).length,
			nodesTotal: nodesInScope.length,
			daily: daily,
		};
	}

	// ---------- 源的模板求值 / 派发计划 ----------
	//
	// `targets[].params` 只认 `$(payload)` / `$(payload.a.b)`（source.md §4.1）：
	//   · 整个值**恰好**是一个引用 → 保留原类型；
	//   · 掺了任何常量（哪怕只是一个空格）→ 字符串拼接；
	//   · 取不到 → 剪枝 `param_missing`（不静默给空）；
	//   · `$(vars.…)` / `$(params.…)` 在源参数里是**配置错误**（400 invalid_source_config）。
	const REF_RE = /\$\(([^)]*)\)/g;

	function templateRefs(value) {
		const src = String(value == null ? "" : value);
		const out = [];
		REF_RE.lastIndex = 0;
		let m;
		while ((m = REF_RE.exec(src))) out.push(m[1]);
		return out;
	}

	// 配置期校验：引用写成什么样才算合法（§4.1 / §6）。
	function templateError(value) {
		if (value && typeof value === "object") {
			for (const v of Object.values(value)) { const err = templateError(v); if (err) return err; }
			return null;
		}
		if (typeof value !== "string") return null;
		if (/\$\((?:vars|params)(?:\.|\))/.test(value)) return "vars/params references are invalid in source params";
		for (const match of value.matchAll(/\$\(payload[^)]*(?:\)|$)/g)) {
			if (!/^\$\(payload(?:\.(?:[A-Za-z_][A-Za-z0-9_]*|[0-9]+))*\)$/.test(match[0])) return "Invalid payload reference: " + match[0];
		}
		return null;
	}

	function lookupPayload(payload, path) {
		const segs = path.split(".").slice(1); // 去掉根名 payload
		let cur = payload;
		for (const s of segs) {
			if (cur == null || typeof cur !== "object" || !(s in cur)) {
				return { found: false };
			}
			cur = cur[s];
		}
		return { found: true, value: cur };
	}

	// 求值一个模板值 → {value} 或 {missing:"payload.xxx"}。
	function evalTemplate(value, payload) {
		if (value && typeof value === "object") {
			const result = Array.isArray(value) ? [] : {};
			for (const key of Object.keys(value)) {
				const out = evalTemplate(value[key], payload);
				if (out.missing) return out;
				result[key] = out.value;
			}
			return {value: result};
		}
		if (typeof value !== "string") return {value};
		const pattern = /\$\((payload(?:\.(?:[A-Za-z_][A-Za-z0-9_]*|[0-9]+))*)\)/g;
		const refs = [...value.matchAll(pattern)];
		if (!refs.length) return {value};
		if (refs.length === 1 && refs[0][0] === value) {
			const hit = lookupPayload(payload, refs[0][1]);
			return hit.found ? {value: clone(hit.value)} : {missing: refs[0][1]};
		}
		let missing;
		const result = value.replace(pattern, (_, path) => {
			const hit = lookupPayload(payload, path);
			if (!hit.found) {missing = path; return "";}
			return hit.value && typeof hit.value === "object" ? JSON.stringify(hit.value) : String(hit.value);
		});
		return missing ? {missing} : {value: result};
	}

	// 极简 JSON Schema 校验（只覆盖 payloadSchema / params 的常用子集：
	// type / required / properties / items / enum）。返回错误消息或 null。
	function validateSchema(schema, value, path) {
		if (!schema || typeof schema !== "object") return null;
		const p = path || "payload";
		if (schema.type) {
			const t = schema.type;
			const ok =
				(t === "object" && value && typeof value === "object" && !Array.isArray(value)) ||
				(t === "array" && Array.isArray(value)) ||
				(t === "string" && typeof value === "string") ||
				(t === "integer" && Number.isInteger(value)) ||
				(t === "number" && typeof value === "number") ||
				(t === "boolean" && typeof value === "boolean") ||
				(t === "null" && value === null);
			if (!ok) return p + " 期望 " + t;
		}
		if (Array.isArray(schema.enum) && schema.enum.indexOf(value) < 0) {
			return p + " 只允许 " + schema.enum.join("|");
		}
		if (schema.type === "object" || (schema.properties && typeof value === "object" && value)) {
			for (const key of schema.required || []) {
				if (value == null || !(key in value)) return p + " 缺少必填字段 " + key;
			}
			const props = schema.properties || {};
			for (const key of Object.keys(value || {})) {
				if (props[key]) {
					const msg = validateSchema(props[key], value[key], p + "." + key);
					if (msg) return msg;
				}
			}
		}
		return null;
	}

	function schemaPropType(cap, field) {
		const props = (cap.spec && cap.spec.params && cap.spec.params.properties) || {};
		return props[field] && props[field].type ? props[field].type : null;
	}

	function coerceTo(v, type) {
		if (!type) return { ok: true, value: v };
		if (type === "integer") {
			const n = typeof v === "string" && v.trim() !== "" ? Number(v) : v;
			if (Number.isInteger(n)) return { ok: true, value: n };
			return { ok: false };
		}
		if (type === "number") {
			const n = typeof v === "string" && v.trim() !== "" ? Number(v) : v;
			if (typeof n === "number" && Number.isFinite(n)) return { ok: true, value: n };
			return { ok: false };
		}
		if (type === "string") {
			return { ok: true, value: v == null ? "" : String(v) };
		}
		if (type === "boolean") {
			if (typeof v === "boolean") return { ok: true, value: v };
			if (v === "true" || v === "false") return { ok: true, value: v === "true" };
			return { ok: false };
		}
		return { ok: true, value: v };
	}

	/**
	 * planDispatch —— 算出「这条配置会派发什么、会剪什么」，**不产生副作用**。
	 *
	 * fire 与 dry-run（集合 / 单源）共用它：两边必须给出**一致**的判定，
	 * 否则用户会看到"试运行说能跑、一点真跑却剪枝了"这种最伤信任的差异。
	 *
	 * 剪枝词表（source.md §6，封闭集合）：`no_target` / `event_schema` /
	 * `param_missing` / `param_schema` / `param_type`。
	 */
	function planDispatch(source, payload) {
		const tasks = [];
		const pruned = [];
		const targets = source.targets || [];

		// 没有 target：事件照常入库，留一条 no_target（合法状态，不是错误）
		if (!targets.length) {
			return {
				tasks: [],
				pruned: [{ to: null, reason: "no_target", detail: "源没有配置 targets" }],
			};
		}

		// 入口级：载荷不符合 payloadSchema 就一条都不派（不是逐 target 的事）
		const schema = source.config && source.config.payloadSchema;
		if (schema) {
			const msg = validateSchema(schema, payload || {}, "payload");
			if (msg) {
				return {
					tasks: [],
					pruned: [{ to: null, reason: "event_schema", detail: msg }],
				};
			}
		}

		for (const t of targets) {
			const cap = resolveCapRef(t.capability, source.ns);
			if (!cap) {
				// 目标解析不到 = 这条入口到不了任何地方；剪枝词表里没有专门的码，
				// 用 no_target 记录并把人话放进 detail（源体检同时会报 blocked）。
				pruned.push({
					to: t.capability,
					reason: "no_target",
					detail: "目标能力 " + t.capability + " 不存在（配置未生效）",
				});
				continue;
			}
			const params = {};
			let dropped = false;
			for (const key of Object.keys(t.params || {})) {
				const raw = t.params[key];
				const out = evalTemplate(raw, payload || {});
				if (out.missing) {
					pruned.push({
						to: t.capability,
						reason: "param_missing",
						field: key,
						detail: "参数 " + key + " 取不到 " + out.missing,
					});
					dropped = true;
					break;
				}
				const want = typeof raw === "string" && /\$\(payload/.test(raw) ? schemaPropType(cap, key) : null;
				const conv = coerceTo(out.value, want);
				if (!conv.ok) {
					pruned.push({
						to: t.capability,
						reason: "param_type",
						field: key,
						detail: "参数 " + key + " 期望 " + want + "，求值得到 " + JSON.stringify(out.value),
					});
					dropped = true;
					break;
				}
				params[key] = conv.value;
			}
			if (dropped) continue;

			for (const [key, prop] of Object.entries((cap.spec.params && cap.spec.params.properties) || {})) {
				if (!(key in params) && prop.default !== undefined) params[key] = clone(prop.default);
			}
			const msg = validateParams(cap, params);
			if (msg) {
				pruned.push({
					to: t.capability,
					reason: "param_schema",
					detail: cap.name + "@" + cap.version + " " + msg,
				});
				continue;
			}
			tasks.push({
				capability: cap.name,
				version: cap.version,
				digest: cap.digest,
				params: params,
			});
		}
		return { tasks: tasks, pruned: pruned };
	}

	function validateParams(cap, params) {
		const schema = (cap.spec && cap.spec.params) || {};
		for (const key of schema.required || []) {
			if (!(key in (params || {}))) return "params." + key + " 必填";
		}
		const props = schema.properties || {};
		for (const key of Object.keys(params || {})) {
			if (!props[key]) continue;
			const msg = validateSchema(props[key], params[key], "params." + key);
			if (msg) return msg;
		}
		return null;
	}

	// ===== 能力模板（保留域：`GET /v1/capability-templates` 的 cluster 读模型）=====
	//
	// 9 个内置模板：5 个 script + 4 个 agent。字段（key/label/type/required/
	// options/help）必须和真后端 `internal/artifact/templates.go` 一字不差 ——
	// 控制台要按字段**推导 paramsSchema**，字段错了演示出来的参数契约就是假的。
	//
	// v4 起模板不再有 `outputs`（没有端口这个概念），只剩 script / requires。
	// script 正文这里做了精简（真后端给的是完整正文）：模板脚本在控制台里
	// 是原样展示 + 允许改的东西，精简不影响页面行为，但保留 shebang 和
	// ANOTIFY_PARAM_* 的用法，好让「改脚本」演示出来的东西仍然像真的。
	// ===== 能力模板（CONTRACT-artifact §8）=====
	//
	// 9 个内置模板：5 个 script + 4 个 agent。字段（key/label/type/required/
	// options/help）必须和真后端 `internal/artifact/templates.go` 一字不差 ——
	// 控制台要按字段**推导 paramsSchema**，字段错了演示出来的参数契约就是假的。
	//
	// script 正文这里做了精简（真后端给的是完整正文）：模板脚本在控制台里
	// 是原样展示 + 允许改的东西，精简不影响页面行为，但保留 shebang 和
	// ANOTIFY_PARAM_* 的用法，好让「改脚本」演示出来的东西仍然像真的。
	function mockTemplates() {
		const agentPrompt = (label, help) => ({
			key: "prompt",
			label: label || "提示词",
			type: "text",
			required: true,
			default: "",
			help: help || "",
		});
		const sh = (body) => ["#!/bin/bash", "set -euo pipefail", 'cd "$ANOTIFY_WORKDIR"', ""]
			.concat(body)
			.concat([""])
			.join("\n");

		return [
			{
				id: "script.blank",
				displayName: "空白脚本",
				category: "script",
				description: "从零开始写一段 bash。参数通过 ANOTIFY_PARAM_* 环境变量读取。",
				fields: [],
				script: sh([
					'echo "capability: $ANOTIFY_CAPABILITY"',
					'echo "digest:     $ANOTIFY_DIGEST"',
					'echo "params:     $ANOTIFY_PARAMS"',
				]),
				requires: [],
				timeoutSec: 900,
			},
			{
				id: "script.http",
				displayName: "HTTP 请求",
				category: "script",
				description: "按参数发一个 HTTP 请求，输出响应体。",
				fields: [
					{ key: "url", label: "URL", type: "string", required: true, default: "", help: "完整地址，含 scheme" },
					{
						key: "method",
						label: "方法",
						type: "select",
						required: false,
						default: "POST",
						options: ["GET", "POST", "PUT", "PATCH", "DELETE"],
						help: "",
					},
					{ key: "body", label: "请求体", type: "text", required: false, default: "", help: "JSON 文本；留空发送 {}" },
				],
				script: sh([
					'curl -fsS -X "${ANOTIFY_PARAM_METHOD:-POST}" \\',
					"  -H 'content-type: application/json' \\",
					'  --data "${ANOTIFY_PARAM_BODY:-{}}" \\',
					'  "$ANOTIFY_PARAM_URL"',
				]),
				requires: ["curl"],
				timeoutSec: 120,
			},
			{
				id: "script.git",
				displayName: "拉取 Git 仓库",
				category: "script",
				description: "浅克隆一个仓库到工作目录并输出最近一次提交。",
				fields: [
					{ key: "repo", label: "仓库地址", type: "string", required: true, default: "", help: "https 或 ssh 地址" },
					{ key: "ref", label: "分支 / 标签", type: "string", required: false, default: "", help: "留空用默认分支" },
				],
				script: sh([
					"args=(--depth 1)",
					'if [ -n "${ANOTIFY_PARAM_REF:-}" ]; then',
					'  args+=(--branch "$ANOTIFY_PARAM_REF")',
					"fi",
					'git clone "${args[@]}" "$ANOTIFY_PARAM_REPO" work',
					"cd work",
					"git log -1 --stat",
				]),
				requires: ["git"],
				timeoutSec: 900,
			},
			{
				id: "script.test",
				displayName: "跑测试",
				category: "script",
				description: "在工作目录里执行一条测试命令。退出码非 0 视为任务失败。",
				fields: [
					{ key: "command", label: "测试命令", type: "string", required: true, default: "make test", help: "" },
				],
				script: sh(['bash -c "$ANOTIFY_PARAM_COMMAND"']),
				requires: [],
				timeoutSec: 1800,
			},
			{
				id: "script.notify",
				displayName: "回调 Webhook",
				category: "script",
				description: "把标题和正文 POST 到一个 webhook 地址。",
				fields: [
					{ key: "webhook_url", label: "Webhook 地址", type: "string", required: true, default: "", help: "" },
					{ key: "title", label: "标题", type: "string", required: false, default: "", help: "" },
					{ key: "body", label: "正文", type: "text", required: false, default: "", help: "" },
				],
				script: sh([
					"payload=$(jq -n \\",
					'  --arg t "${ANOTIFY_PARAM_TITLE:-}" \\',
					'  --arg b "${ANOTIFY_PARAM_BODY:-}" \\',
					"  '{title: \$t, body: \$b}')",
					'curl -fsS -X POST -H \'content-type: application/json\' --data "$payload" \\',
					'  "$ANOTIFY_PARAM_WEBHOOK_URL"',
				]),
				requires: ["curl", "jq"],
				timeoutSec: 120,
			},
			{
				id: "agent.tui",
				displayName: "hc 托管 TUI Agent",
				category: "agent",
				description: "把一个任务交给 hc 托管的 TUI agent（pi / claude / codex …），agent 自己按提示词里的 emit 契约上报。",
				fields: [
					{ key: "agent", label: "Agent", type: "select", required: false, default: "pi", options: ["pi", "claude", "codex", "gemini", "cursor", "opencode"], help: "必须是 hc 认得的 agent（它靠命令行认 agent）" },
					{ key: "agent_args", label: "Agent 参数", type: "string", required: false, default: "", help: "原样拼到命令行：--model gpt-6.1-sol --thinking medium" },
					{ key: "ports", label: "产出说明", type: "text", required: false, default: "", help: "每行一条：先写什么时候 emit，再写内容期望" },
					{ key: "space", label: "空间（标签）", type: "string", required: false, default: "default", help: "label anotify.space=<值>" },
					{ key: "worktree", label: "Worktree", type: "select", required: false, default: "off", options: ["off", "new", "existing"], help: "new = 节点自己 git worktree add 再交给 agent" },
					{ key: "branch", label: "分支", type: "string", required: false, default: "anotify/{{trace8}}-{{seq}}", help: "仅 worktree=new；由节点在任务期展开" },
					{ key: "base", label: "起点", type: "string", required: false, default: "HEAD", help: "仅 worktree=new" },
					{ key: "worktree_path", label: "Worktree 路径", type: "string", required: false, default: "", help: "existing 必填" },
					{ key: "worktree_cleanup", label: "结束后清理", type: "select", required: false, default: "remove", options: ["remove", "keep"], help: "仅 worktree=new 且任务成功时" },
					agentPrompt("任务正文", "交给 agent 的提示词"),
					{ key: "timeout_sec", label: "任务上限（秒）", type: "number", required: false, default: "3600", help: "整个任务的上限" },
				],
				script: sh([
					"hc server status >/dev/null || { echo \"hc_core_unreachable: hc core 不可用\" >&2; exit 1; }",
					'task_json=".anotify/task.json"; chmod 600 "$task_json"',
					'hc run --bg --outlive-parent --keep --no-notify \\',
					'  --name "anotify-${ANOTIFY_TRACE_ID:-local}" --label "anotify.space=${ANOTIFY_PARAM_SPACE:-default}" \\',
					'  --cwd "$PWD" -- "${ANOTIFY_PARAM_AGENT:-pi}" "$ANOTIFY_PARAM_PROMPT"',
				]),
				requires: ["hc", "git"],
				timeoutSec: 3600,
			},
			{
				id: "agent.claude",
				displayName: "Claude Code",
				category: "agent",
				description: "用 Claude Code 执行一段提示词。密钥留在机器上（机器需已装 llm 与登录态）。",
				fields: [
					agentPrompt(),
					{ key: "model", label: "模型", type: "string", required: false, default: "", help: "留空用默认" },
					{ key: "tools", label: "允许的工具", type: "string", required: false, default: "", help: "逗号分隔，留空用默认" },
				],
				script: sh([
					"args=(--print)",
					'if [ -n "${ANOTIFY_PARAM_MODEL:-}" ]; then',
					'  args+=(--model "$ANOTIFY_PARAM_MODEL")',
					"fi",
					'if [ -n "${ANOTIFY_PARAM_TOOLS:-}" ]; then',
					'  args+=(--allowedTools "$ANOTIFY_PARAM_TOOLS")',
					"fi",
					'claude "${args[@]}" "$ANOTIFY_PARAM_PROMPT"',
				]),
				requires: ["claude"],
				timeoutSec: 3600,
			},
			{
				id: "agent.codex",
				displayName: "Codex",
				category: "agent",
				description: "用 Codex CLI 执行一段提示词。密钥留在机器上。",
				fields: [agentPrompt()],
				script: sh(['codex exec "$ANOTIFY_PARAM_PROMPT"']),
				requires: ["codex"],
				timeoutSec: 3600,
			},
			{
				id: "agent.pi",
				displayName: "PI",
				category: "agent",
				description: "用 PI 执行一段提示词。密钥留在机器上。",
				fields: [
					agentPrompt(),
					{ key: "model", label: "模型", type: "string", required: false, default: "", help: "留空用默认" },
				],
				script: sh([
					'if [ -n "${ANOTIFY_PARAM_MODEL:-}" ]; then',
					'  pi --print -m "$ANOTIFY_PARAM_MODEL" -p "$ANOTIFY_PARAM_PROMPT"',
					"else",
					'  pi --print -p "$ANOTIFY_PARAM_PROMPT"',
					"fi",
				]),
				requires: ["pi"],
				timeoutSec: 3600,
			},
			{
				id: "agent.custom",
				displayName: "自定义 Agent CLI",
				category: "agent",
				description: "调用任意已安装在机器上的 Agent 命令行。",
				fields: [
					{
						key: "agent_command",
						label: "命令名",
						type: "string",
						required: true,
						default: "",
						help: "例如 aider / amp / cursor-agent",
					},
					agentPrompt(),
				],
				script: sh(['"$ANOTIFY_PARAM_AGENT_COMMAND" "$ANOTIFY_PARAM_PROMPT"']),
				requires: [],
				timeoutSec: 3600,
			},
		];
	}

	// ---------- 路由表 ----------
	//
	// 每个条目：(method, 正则) → handler(ctx) → Response
	// ctx 里：m（正则捕获）、query（.get）、body（JSON）、path。
	const routes = [];
	function on(method, pattern, handler) {
		routes.push({ method: method, pattern: pattern, handler: handler });
	}

	// 命名空间存在性：v4 的 ns 私有资源路径里 ns 是**定位器**，不存在就 404。
	function nsRow(name) {
		return state.namespaces.find((n) => n.name === name) || null;
	}
	function nsMissing(name) {
		return name === "*" ? null : nsRow(name) ? null : notFound("命名空间 ");
	}

	// `namespace` query 必填（stats / console.events 不再默认全 ns）。
	function requireNamespace(query) {
		const ns = query.get("namespace");
		if (ns == null || ns === "") {
			return {
				err: badRequest(
					"BadRequest",
					"namespace query 必填：v4 没有默认的全命名空间返回",
				),
			};
		}
		if (ns !== "*" && !nsRow(ns)) return { err: notFound("命名空间 ") };
		return { ns: ns };
	}

	const findCap = (ns, name) =>
		state.capabilities.find((c) => c.ns === ns && c.name === name) || null;
	const findSource = (ns, name) =>
		state.sources.find((s) => s.ns === ns && s.name === name) || null;
	const findTask = (ns, name) =>
		state.tasks.find((t) => t.ns === ns && t.name === name) || null;

	// 名称规则与 capability.md §2 的 metadata.name 一致（源/能力共用）。
	const NAME_RE = /^[a-z0-9][a-z0-9._-]{0,62}$/;

	// AUTH1: retained identity errors are {error}, never resource Status.
	// This is a private, known-user preview fixture, NOT backend authentication.
	const mockSession = "mock-preview-demo-session";
	const loginAttempts = new Map();
	function authResponse(body, code = 200, plainText = false) {
		const text = JSON.stringify(body) + (plainText ? "\n" : "");
		const type = (plainText ? "text/plain" : "application/json") + "; charset=utf-8";
		if (typeof Response === "function") return new Response(text, { status: code, headers: { "Content-Type": type } });
		const headers = new Map([["content-type", type]]);
		return { status: code, ok: code >= 200 && code < 300, headers: { get: k => headers.get(k.toLowerCase()) || null, set: (k, v) => headers.set(k.toLowerCase(), v) }, json: async () => body, text: async () => text };
	}
	const identityError = (code, error) => authResponse({ error }, code);
	const anonymous = () => authResponse({ error: "未登录" }, 401, true);
	const hasSession = () => !state.auth.loggedOut && String(document.cookie || "").split(";").some(c => c.trim() === "anotify_session=" + mockSession);
	function sessionCookie(clear = false) {
		const expires = new Date(clear ? 0 : Date.now() + 7 * 86400000).toUTCString();
		const cookie = "anotify_session=" + (clear ? "" : mockSession) + "; Path=/; SameSite=Lax; Expires=" + expires;
		document.cookie = cookie; // JS cookie only in explicit preview; real server owns HttpOnly.
		return cookie.replace("; SameSite", "; HttpOnly; SameSite");
	}
	if (!state.auth.loggedOut) sessionCookie();
	function loginLimited(key) {
		const time = Date.now(), old = loginAttempts.get(key);
		const bucket = old && old.until > time ? old : { count: 0, until: time + 60000 };
		loginAttempts.set(key, bucket);
		if (++bucket.count <= 10) return null;
		const res = identityError(429, "请求过于频繁，请稍后再试");
		res.headers.set("Retry-After", String(Math.max(1, Math.ceil((bucket.until - time) / 1000))));
		return res;
	}
	on("GET", /^\/v1\/auth\/me$/, () => hasSession() ? json(ME) : anonymous());
	on("POST", /^\/v1\/auth\/password\/login$/, ctx => {
		const ipLimit = loginLimited("preview-ip"); if (ipLimit) return ipLimit;
		const { username, password } = ctx.body || {};
		if (typeof username !== "string" || !username.trim() || typeof password !== "string" || !password) return identityError(400, "缺少 username 或 password");
		const userLimit = loginLimited("user:" + username.trim().toLowerCase()); if (userLimit) return userLimit;
		if (username.trim() !== ME.username || !state.auth.passwordHash || mockDigest(password) !== state.auth.passwordHash) return identityError(401, "Username or password is incorrect");
		state.auth.loggedOut = false;
		if (typeof sessionStorage !== "undefined") sessionStorage.removeItem("anotify.mock.loggedOut");
		const res = authResponse({ ok: true }); res.headers.set("Set-Cookie", sessionCookie()); return res;
	});
	on("POST", /^\/v1\/auth\/logout$/, () => {
		state.auth.loggedOut = true;
		if (typeof sessionStorage !== "undefined") sessionStorage.setItem("anotify.mock.loggedOut", "1");
		const res = authResponse({ ok: true }); res.headers.set("Set-Cookie", sessionCookie(true)); return res;
	});
	on("GET", /^\/v1\/vapid-public-key$/, () =>
		json({ publicKey: "BMockPublicKeyForPreviewOnly_NotARealVapidKey" }),
	);

	// ===== 命名空间（cluster，读 + 建）=====
	//
	// 读要 `NamespaceList` 信封（`ensureScope` 靠 items[].metadata.name 选默认范围），
	// 建也是信封进、信封出。v3 的 `{namespaces:[{id,slug,name}]}` 已删除。
	on("GET", /^\/v1\/namespaces$/, () =>
		json(listOf("NamespaceList", state.namespaces.map(namespaceResource))),
	);

	on("POST", /^\/v1\/namespaces$/, (ctx) => {
		const body = ctx.body;
		const derr = validateWriteBody(body, "Namespace", null);
		if (derr) return derr;
		const name = String((body.metadata && body.metadata.name) || "").trim();
		if (!NAME_RE.test(name)) {
			return badRequest("BadRequest", "metadata.name 只允许小写字母/数字/._-，且以字母或数字开头");
		}
		if (nsRow(name)) return statusErr(409, "AlreadyExists", "命名空间 " + name + " 已存在");
		const row = {
			name: name,
			displayName: (body.metadata && body.metadata.displayName) || name,
			uid: "ns_mock_" + name,
			rv: nextRV(),
			createdAt: isoNow(),
		};
		state.namespaces.push(row);
		state.nsVars[name] = clone((body.spec && body.spec.vars) || {});
		return json(namespaceResource(row), 201);
	});

	on("GET", /^\/v1\/namespaces\/([^/]+)$/, (ctx) => {
		const name = decodeURIComponent(ctx.m[1]);
		const row = nsRow(name);
		if (!row) return notFound("命名空间 ");
		return json(namespaceResource(row));
	});

	// ===== Node/config (W4/W5/A3): one fact and CAS per node =====
	const configObject = v => !!v && typeof v === "object" && !Array.isArray(v);
	const findNode = name => state.nodes.find(n => n.name === name);
	function configFields(body, allowed) {
		if (!configObject(body)) return badRequest("BadRequest", "Expected configuration object");
		if (Object.keys(body).some(k => !allowed.includes(k))) return badRequest("unknown_field", "Only writable configuration fields are accepted");
		if (body.metadata && (!configObject(body.metadata) || Object.keys(body.metadata).some(k => k !== "resourceVersion"))) return badRequest("unknown_field", "Only metadata.resourceVersion is writable");
		return null;
	}
	function validateNodeConfig(body) {
		if (body.namespaces != null && (!Array.isArray(body.namespaces) || !body.namespaces.length || new Set(body.namespaces).size !== body.namespaces.length || body.namespaces.some(n => n !== "*" && (typeof n !== "string" || !NAME_RE.test(n))))) return badRequest("namespace_invalid", "Nonempty unique namespace names or * required");
		if (body.vars != null && (!configObject(body.vars) || Object.entries(body.vars).some(([k,v]) => !/^[A-Z][A-Z0-9_]{0,63}$/.test(k) || typeof v !== "string" || !v || v.includes("\0") || unescape(encodeURIComponent(v)).length > 4096))) return badRequest("vars_invalid", "vars must contain nonempty, nonsecret strings with uppercase names");
		const limits = body.limits;
		if (limits != null && (!configObject(limits) || Object.keys(limits).some(k => !["total", "perCapability"].includes(k)) || (limits.total != null && (!Number.isInteger(limits.total) || limits.total < 1)) || (limits.perCapability != null && (!configObject(limits.perCapability) || Object.entries(limits.perCapability).some(([k,v]) => (k !== "*" && !NAME_RE.test(k)) || !Number.isInteger(v) || v < 1))))) return badRequest("limits_invalid", "limits.total/perCapability leaves must be positive integers; no namespace limits");
		return null;
	}
	function nodeConfig(row) {
		return { metadata: { resourceVersion: String(row.rv) }, namespaces: clone(row.spec.namespaces), vars: clone(row.spec.vars), limits: clone(row.spec.limits), configRevision: String(row.configRevision), appliedRevision: String(row.appliedRevision), appliedHash: mockDigest("applied-" + row.name + "-" + row.appliedRevision) };
	}
	function updateNodeConfig(row, body) {
		const invalid = configFields(body, ["metadata", "namespaces", "vars", "limits"]) || checkCas(body, row) || validateNodeConfig(body);
		if (invalid) return invalid;
		// Keep the last machine-applied report honest while defaults are pending.
		const old = deriveNodeStatus(row);
		row.facts.machineVars = clone(old.machineVars); row.facts.machineLimits = clone(old.machineLimits);
		if (body.vars) row.overrideStale = Object.keys(row.facts.localVars || {}).some(k => body.vars[k] !== row.spec.vars[k]);
		for (const k of ["namespaces", "vars", "limits"]) if (body[k] != null) row.spec[k] = clone(body[k]);
		row.rv = nextRV(); if (row.rv === body.metadata.resourceVersion) row.rv = nextRV();
		row.configRevision = String(Number(row.configRevision) + 1);
		return json(nodeConfig(row));
	}
	on("GET", /^\/v1\/nodes$/, ctx => {
		const ns = ctx.query.get("namespace"); if (ns && nsMissing(ns)) return nsMissing(ns);
		return json(listOf("NodeList", state.nodes.map(n => nodeResource(n, ns || null))));
	});
	on("POST", /^\/v1\/nodes$/, ctx => {
		const body = ctx.body; const invalid = validateWriteBody(body, "Node", null);
		if (invalid) return invalid;
		if (body.apiVersion !== API_VERSION || body.kind !== "Node") return badRequest("kind_mismatch", "Node envelope required");
		const name = body.metadata?.name;
		if (!NAME_RE.test(name || "")) return badRequest("name_invalid", "metadata.name required");
		if (body.metadata.namespace != null) return badRequest("namespace_mismatch", "Node is cluster scoped");
		if (findNode(name)) return statusErr(409, "AlreadyExists", "Cluster node name already exists");
		const spec = { namespaces: ["*"], vars: {}, limits: { total: 1, perCapability: {} }, tags: [], ...(body.spec || {}) };
		if (Object.keys(spec).some(k => !["namespaces", "vars", "limits", "tags"].includes(k))) return badRequest("unknown_field", "Node spec contains an unknown field");
		const err = validateNodeConfig(spec); if (err) return err;
		const row = { name, uid: nextUID("node"), displayName: body.metadata.displayName || "", spec, declared: [], facts: {}, rv: nextRV(), configRevision: "1", appliedRevision: "", createdAt: isoNow() };
		state.nodes.push(row); return json(nodeResource(row), 201);
	});
	on("GET", /^\/v1\/nodes\/([^/]+)$/, ctx => {
		const row = findNode(decodeURIComponent(ctx.m[1])); if (!row) return notFound("Node ");
		const ns = ctx.query.get("namespace"); if (ns && nsMissing(ns)) return nsMissing(ns);
		return json(nodeResource(row, ns || null));
	});
	on("PUT", /^\/v1\/nodes\/([^/]+)$/, ctx => {
		const row = findNode(decodeURIComponent(ctx.m[1])); if (!row) return notFound("Node ");
		const invalid = validateWriteBody(ctx.body, "Node", null); if (invalid) return invalid;
		const saved = updateNodeConfig(row, { metadata: { resourceVersion: ctx.body.metadata?.resourceVersion }, ...(ctx.body.spec || {}) });
		return saved.status === 200 ? json(nodeResource(row)) : saved;
	});
	on("POST", /^\/v1\/nodes\/([^/]+)\/tokens$/, ctx => {
		const row = findNode(decodeURIComponent(ctx.m[1])); if (!row) return notFound("Node ");
		if (ctx.body != null && (!configObject(ctx.body) || Object.keys(ctx.body).length)) return badRequest("unknown_field", "tokens accepts an empty body");
		row.identityToken = "ant_node_mock_" + row.name + "_" + nextRV();
		// Identity rotation invalidates existing connected authorization; no auto hello.
		row.facts.lastSeenAt = null;
		return json({ apiVersion: API_VERSION, kind: "NodeToken", node: { name: row.name }, token: row.identityToken });
	});
	on("GET", /^\/v1\/nodes\/([^/]+)\/config$/, ctx => {
		const row = findNode(decodeURIComponent(ctx.m[1])); return row ? json(nodeConfig(row)) : notFound("Node ");
	});
	on("PUT", /^\/v1\/nodes\/([^/]+)\/config$/, ctx => {
		const row = findNode(decodeURIComponent(ctx.m[1])); return row ? updateNodeConfig(row, ctx.body) : notFound("Node ");
	});
	on("POST", /^\/v1\/nodes\/config\/batch$/, async ctx => {
		if (!configObject(ctx.body) || !Array.isArray(ctx.body.updates) || Object.keys(ctx.body).some(k => k !== "updates")) return badRequest("BadRequest", "updates array required");
		const items = [];
		for (const update of ctx.body.updates) {
			const { name, ...body } = configObject(update) ? update : {};
			const row = findNode(name); const response = row ? updateNodeConfig(row, body) : notFound("Node ");
			const value = await response.json();
			items.push(response.status === 200 ? { name, ok: true, config: value } : { name: name || "", ok: false, error: value });
		}
		return json({ items });
	});
	function namespaceConfig(name) {
		const row = state.nsConfig[name] ||= { rv: "ns-config-" + name, revision: "1" };
		return { metadata: { resourceVersion: row.rv }, vars: clone(state.nsVars[name] || {}), namespaceVarsRevision: row.revision };
	}
	on("GET", /^\/v1\/namespaces\/([^/]+)\/config$/, ctx => {
		const name = decodeURIComponent(ctx.m[1]); return nsMissing(name) || json(namespaceConfig(name));
	});
	on("PUT", /^\/v1\/namespaces\/([^/]+)\/config$/, ctx => {
		const name = decodeURIComponent(ctx.m[1]); if (nsMissing(name)) return nsMissing(name);
		namespaceConfig(name); const row = state.nsConfig[name];
		const invalid = configFields(ctx.body, ["metadata", "vars"]) || checkCas(ctx.body, row) || validateNodeConfig(ctx.body);
		if (invalid) return invalid;
		if (ctx.body.vars != null) state.nsVars[name] = clone(ctx.body.vars);
		row.rv = nextRV(); if (row.rv === ctx.body.metadata.resourceVersion) row.rv = nextRV(); row.revision = String(Number(row.revision) + 1);
		return json(namespaceConfig(name));
	});
	const systemConfig = () => ({ metadata: { resourceVersion: state.systemConfig.rv }, retention: clone(state.systemConfig.retention) });
	on("GET", /^\/v1\/system\/config$/, () => ME.role === "admin" ? json(systemConfig()) : statusErr(403, "Forbidden", "Administrator required"));
	on("PUT", /^\/v1\/system\/config$/, ctx => {
		if (ME.role !== "admin") return statusErr(403, "Forbidden", "Administrator required");
		const invalid = configFields(ctx.body, ["metadata", "retention"]) || checkCas(ctx.body, state.systemConfig); if (invalid) return invalid;
		if (!configObject(ctx.body.retention) || Object.keys(ctx.body.retention).some(k => !["resultTtlSec", "eventTtlSec", "taskTtlSec"].includes(k)) || Object.values(ctx.body.retention).some(v => !Number.isInteger(v) || v < 0)) return badRequest("retention_invalid", "TTL must be nonnegative integer seconds; 0 means permanent");
		state.systemConfig.retention = { ...state.systemConfig.retention, ...clone(ctx.body.retention) };
		state.systemConfig.rv = nextRV(); if (state.systemConfig.rv === ctx.body.metadata.resourceVersion) state.systemConfig.rv = nextRV();
		return json(systemConfig());
	});
	on("POST", /^\/v1\/nodes\/([^/]+)\/drain$/, ctx => {
		const row = findNode(decodeURIComponent(ctx.m[1])); if (!row) return notFound("Node ");
		if (!nodeOnline(row)) return statusErr(409, "node_offline", "Node is offline");
		row.draining = true; return json({ status: "draining" });
	});
	on("POST", /^\/v1\/nodes\/([^/]+)\/upgrade$/, ctx => {
		const row = findNode(decodeURIComponent(ctx.m[1])); if (!row) return notFound("Node ");
		if (!state.agentBinary) return statusErr(501, "agent_binary_unavailable", "Server has no node binary; install locally");
		if (!nodeOnline(row)) return statusErr(409, "node_offline", "Node is offline");
		if (state.upgradeFailCode) return statusErr(500, state.upgradeFailCode, "Node upgrade failed");
		return statusErr(501, "agent_binary_unavailable", "Preview has no downloadable verified binary; no upgrade was sent");
	});

	// ===== Playbook: raw package -> pure render -> atomic additive plan =====
	const PB_LIST = /^\/v1\/namespaces\/([^/]+)\/playbooks$/;
	const PB_ONE = /^\/v1\/namespaces\/([^/]+)\/playbooks\/([^/]+)$/;
	const pbObject = v => !!v && typeof v === "object" && !Array.isArray(v);
	const pbFail = (reason, message) => { throw Object.assign(new Error(message), { reason }); };
	const pbParse = (text, path) => {
		try { return window.Anotify.model.playbook.parseDocument(text); }
		catch (error) { return pbFail("invalid_playbook", path + ": " + error.message); }
	};
	function pbPackage(files, slug) {
		if (!pbObject(files)) pbFail("invalid_playbook", "spec.files must be a relative-path -> string map");
		if (!("playbook.yaml" in files)) pbFail("entry_missing", "playbook.yaml is required");
		const keys = Object.keys(files);
		if (keys.length > 64) pbFail("too_many_items", "At most 64 files");
		let bytes = 0;
		for (const path of keys) {
			if (!path || path.startsWith("/") || path.includes("\\") || path.split("/").some(s => s === ".." || s === "." || !s) || path.length > 255 || path.includes("\0") || path.includes("{{")) {
				pbFail("file_path_invalid", "Invalid package path: " + path);
			}
			if (typeof files[path] !== "string") pbFail("invalid_playbook", path + ": expected string content");
			const size = unescape(encodeURIComponent(files[path])).length;
			if (size > 2 * 1024 * 1024) pbFail("invalid_playbook", path + ": file exceeds 2 MiB");
			if (/\.ya?ml$/i.test(path)) bytes += size;
		}
		if (bytes > 8 * 1024 * 1024) pbFail("invalid_playbook", "YAML package exceeds 8 MiB");
		const root = pbParse(files["playbook.yaml"], "playbook.yaml");
		if (!pbObject(root) || !pbObject(root.metadata) || !pbObject(root.spec)) pbFail("invalid_playbook", "playbook.yaml needs metadata and spec");
		if (!/^[a-z0-9]+(-[a-z0-9]+)*$/.test(root.metadata.name || "") || root.metadata.name.length > 64) pbFail("slug_invalid", "Invalid package slug");
		if (root.metadata.name !== slug) pbFail("invalid_playbook", "metadata.name must match playbook.yaml slug");
		if (root.metadata.namespace != null) pbFail("namespace_in_package", "playbook.yaml: namespace is installation context");
		if (root.metadata.version != null && (!Number.isInteger(root.metadata.version) || root.metadata.version < 1)) pbFail("invalid_playbook", "Package version must be a positive integer");
		if (root.spec.values != null && !pbObject(root.spec.values)) pbFail("invalid_playbook", "spec.values must be JSON Schema");
		if (["edges", "triggers"].some(k => k in root.spec)) pbFail("invalid_playbook", "v3 package shape is not supported");
		const seen = new Set(["playbook.yaml"]);
		const documents = { capabilities: [], sources: [] };
		for (const slot of Object.keys(documents)) {
			const refs = root.spec[slot] || [];
			if (!Array.isArray(refs)) pbFail("invalid_playbook", "spec." + slot + " must be relative path references");
			if (refs.length > 32) pbFail("too_many_items", slot + ": at most 32 items");
			for (const path of refs) {
				if (typeof path !== "string" || !path || path.startsWith("/") || path.split("/").includes("..") || path.length > 255 || path.includes("\0") || path.includes("{{")) pbFail("file_path_invalid", "Invalid reference path");
				if (seen.has(path)) pbFail("file_duplicate", path + ": repeated reference");
				if (!(path in files)) pbFail("file_missing", path + ": referenced file missing");
				seen.add(path);
				const doc = pbParse(files[path], path);
				if (!pbObject(doc) || !pbObject(doc.metadata) || typeof doc.metadata.name !== "string" || !pbObject(doc.spec)) pbFail("invalid_playbook", path + ": metadata.name and spec required");
				if (doc.metadata.namespace != null) pbFail("namespace_in_package", path + ": namespace forbidden");
				delete doc.status;
				if (slot === "capabilities" && (!pbObject(doc.spec.run) || !pbObject(doc.spec.run.files) || typeof doc.spec.run.entrypoint !== "string")) pbFail("invalid_playbook", path + ": Capability requires run/files/entrypoint");
				if (slot === "sources" && !["manual", "webhook", "cron"].includes(doc.spec.kind)) pbFail("invalid_playbook", path + ": Source kind required");
				documents[slot].push({ path, doc });
			}
		}
		for (const path of keys) if (/\.ya?ml$/i.test(path) && !seen.has(path)) pbFail("file_unreferenced", path + ": unreferenced YAML");
		return { root, documents };
	}
	function pbDefaults(schema, value) {
		if (value === undefined && schema && "default" in schema) value = clone(schema.default);
		if (pbObject(value)) {
			value = clone(value);
			for (const [key, sub] of Object.entries(schema?.properties || {})) {
				const result = pbDefaults(sub, value[key]);
				if (result !== undefined) value[key] = result;
			}
		}
		return value;
	}
	function pbValidateValues(schema, value, path = "values") {
		if (!schema || typeof schema !== "object") return;
		const message = validateSchema(schema, value, path);
		if (message) pbFail(message.includes("缺少必填") ? "values_missing" : "values_invalid", path + ": schema validation failed");
		if (typeof value === "string") {
			if (schema.pattern && !new RegExp(schema.pattern).test(value)) pbFail("values_invalid", path + ": pattern does not match");
			if (schema.minLength != null && value.length < schema.minLength || schema.maxLength != null && value.length > schema.maxLength) pbFail("values_invalid", path + ": invalid length");
		}
		if (typeof value === "number" && (schema.minimum != null && value < schema.minimum || schema.maximum != null && value > schema.maximum)) pbFail("values_invalid", path + ": out of bounds");
		if (pbObject(value)) for (const [key, v] of Object.entries(value)) {
			if (!schema.properties?.[key] && schema.additionalProperties === false) pbFail("values_invalid", path + "." + key + ": unknown field");
			pbValidateValues(schema.properties?.[key], v, path + "." + key);
		}
		if (Array.isArray(value)) value.forEach((v, i) => pbValidateValues(schema.items, v, path + "." + i));
	}
	function pbInterpolate(value, values, path) {
		if (Array.isArray(value)) return value.map((v, i) => pbInterpolate(v, values, path + "." + i));
		if (pbObject(value)) return Object.fromEntries(Object.entries(value).map(([k, v]) => [k, pbInterpolate(v, values, path + "." + k)]));
		if (typeof value !== "string") return value;
		const get = key => {
			let result = values;
			for (const part of key.split(".")) {
				if (result == null || !Object.prototype.hasOwnProperty.call(result, part)) pbFail("values_missing", path + ": values." + key + " missing; supply a value or schema default");
				result = result[part];
			}
			return clone(result);
		};
		const whole = /^\{\{values\.([\w.-]+)\}\}$/.exec(value);
		if (whole) return get(whole[1]);
		return value.replace(/\{\{values\.([\w.-]+)\}\}/g, (_, key) => {
			const v = get(key); return typeof v === "string" ? v : JSON.stringify(v);
		});
	}
	function pbSecrets(schema, value, list = [], inherited = false) {
		const secret = inherited || schema?.writeOnly;
		if (secret && typeof value === "string" && value) list.push(value);
		if (pbObject(value)) for (const [k, v] of Object.entries(value)) pbSecrets(schema?.properties?.[k], v, list, secret);
		if (Array.isArray(value)) value.forEach(v => pbSecrets(schema?.items, v, list, secret));
		return list;
	}
	function pbMask(value, secrets = [], schema) {
		if (schema?.writeOnly) return "***";
		if (Array.isArray(value)) return value.map((v, i) => pbMask(v, secrets, schema?.prefixItems?.[i] || schema?.items));
		if (pbObject(value)) return Object.fromEntries(Object.entries(value).map(([k, v]) => [k, /^(token|secret)$/i.test(k) ? "***" : pbMask(v, secrets, schema?.properties?.[k])]));
		if (typeof value === "string" && secrets.some(s => s && value.includes(s))) return "***";
		return value;
	}
	// A policy contains only schema/template provenance, never submitted values.
	// It travels with an applied mock resource so an older actual route is masked
	// by its OWN template even after a package update changes reference locations.
	function pbSchemaPolicy(schema) {
		if (schema?.writeOnly) return { writeOnly: true };
		return { ...(schema?.properties ? { properties: Object.fromEntries(Object.entries(schema.properties).map(([k, v]) => [k, pbSchemaPolicy(v)])) } : {}),
			...(schema?.items ? { items: pbSchemaPolicy(schema.items) } : {}) };
	}
	function pbReferencePolicy(schema, key) {
		for (const part of key.split(".")) {
			if (schema?.writeOnly) return { writeOnly: true };
			schema = /^\d+$/.test(part) ? schema?.items : schema?.properties?.[part];
		}
		return pbSchemaPolicy(schema);
	}
	function pbPolicySecret(policy) {
		if (!policy) return false;
		return !!policy.writeOnly || Object.values(policy?.properties || {}).some(pbPolicySecret) || pbPolicySecret(policy?.items);
	}
	function pbTemplatePolicy(template, schema) {
		if (Array.isArray(template)) return { prefixItems: template.map(v => pbTemplatePolicy(v, schema)) };
		if (pbObject(template)) return { properties: Object.fromEntries(Object.entries(template).map(([k, v]) => [k, pbTemplatePolicy(v, schema)])) };
		if (typeof template !== "string") return {};
		const whole = /^\{\{values\.([\w.-]+)\}\}$/.exec(template);
		if (whole) return pbReferencePolicy(schema, whole[1]);
		return [...template.matchAll(/\{\{values\.([\w.-]+)\}\}/g)].some(m => pbPolicySecret(pbReferencePolicy(schema, m[1]))) ? { writeOnly: true } : {};
	}
	function pbDisplaySecrets(raw, masked, secrets = []) {
		if (typeof raw === "string" && raw !== masked && raw) secrets.push(raw);
		if (raw && typeof raw === "object") for (const [k, v] of Object.entries(raw)) pbDisplaySecrets(v, masked?.[k], secrets);
		return secrets;
	}
	function pbRender(row, input) {
		const pkg = pbPackage(row.files, row.name);
		if (input != null && !pbObject(input)) pbFail("values_invalid", "values must be an object");
		const values = pbDefaults(pkg.root.spec.values || {}, input || {});
		pbValidateValues(pkg.root.spec.values, values);
		const documents = { capabilities: [], sources: [] }; const files = clone(row.files);
		const errorSecrets = pbSecrets(pkg.root.spec.values, values);
		try {
		for (const [slot, entries] of Object.entries(pkg.documents)) {
			const names = new Set();
			for (const { path, doc } of entries) {
				const rendered = pbInterpolate(doc, values, path);
				pbDisplaySecrets(rendered, pbMask(rendered, [], pbTemplatePolicy(doc, pkg.root.spec.values)), errorSecrets);
				if (typeof rendered.metadata.name !== "string") pbFail("render_type_mismatch", path + ": metadata.name must render to string");
				if (!NAME_RE.test(rendered.metadata.name)) pbFail("render_name_invalid", path + ": " + doc.metadata.name + " rendered to " + rendered.metadata.name + "; tighten values schema pattern");
				if (names.has(rendered.metadata.name)) pbFail(slot === "capabilities" ? "duplicate_capability" : "duplicate_source", path + ": duplicate rendered name");
				names.add(rendered.metadata.name);
				if (slot === "capabilities") {
					const spec = rendered.spec; const run = spec.run;
					if (typeof run.entrypoint !== "string" || !pbObject(run.files) || typeof run.files[run.entrypoint] !== "string" || !run.files[run.entrypoint].startsWith("#!")) pbFail("entrypoint_no_shebang", path + ": entrypoint must start with #!");
					for (const [file, content] of Object.entries(run.files)) if (typeof content !== "string") pbFail("render_type_mismatch", path + ": run.files." + file + " requires string");
					if (!Array.isArray(spec.next || []) || (spec.next || []).some(ref => typeof ref !== "string" || !NAME_RE.test(splitRef(ref).name))) pbFail("invalid_playbook", path + ": invalid next reference");
					if (rendered.metadata.version != null && (!Number.isInteger(rendered.metadata.version) || rendered.metadata.version < 1)) pbFail("invalid_playbook", path + ": version must be a positive integer");
					// Shared single-create semantic checks, without database next lookup.
					const basic = validateNewCapability(Object.assign({}, spec, { next: [] }), row.ns);
					if (basic) pbFail("invalid_playbook", path + ": invalid capability contract");
				} else {
					const err = validateSourceSpec(rendered.spec); if (err) pbFail(err.reason, path + ": " + err.message);
				}
				documents[slot].push(rendered); files[path] = JSON.stringify(rendered, null, 2);
			}
		}
		const byName = new Map(documents.capabilities.map(d => [d.metadata.name, d]));
		const visiting = new Set(); const done = new Set();
		const visit = name => {
			if (visiting.has(name)) pbFail("cycle", "Package next cycle at " + name);
			if (done.has(name)) return;
			visiting.add(name);
			for (const ref of byName.get(name)?.spec.next || []) if (byName.has(splitRef(ref).name)) visit(splitRef(ref).name);
			visiting.delete(name); done.add(name);
		};
		for (const name of byName.keys()) visit(name);
		return { values, valuesDigest: mockDigest(stableStringify(values)), files, documents };
		} catch (error) { if (error.reason) error.message = pbMask(error.message, errorSecrets); throw error; }
	}
	function pbVersions(row) {
		return row ? [row].concat((row.versions || []).map(v => Object.assign({}, row, v, { receiptPolicy: clone(v.receiptPolicy || {}), receiptRef: v.receiptRef, versions: [] }))) : [];
	}
	function pbOwned(row) {
		const installs = state.playbookInstalls.filter(i => i.ns === row.ns && i.playbookUid === row.uid);
		const ids = new Set(installs.map(i => i.id));
		return { installs, capabilities: state.capabilities.filter(c => c.ns === row.ns).flatMap(pbVersions).filter(c => ids.has(c.installedFrom)), sources: state.sources.filter(s => s.ns === row.ns && ids.has(s.installedFrom)) };
	}
	function pbStatus(row) {
		const owned = pbOwned(row); const last = owned.installs.at(-1)?.receipt;
		const unapplied = clone(last?.unapplied || []);
		const caps = owned.capabilities.map(c => ({ row: c, status: deriveCapabilityStatus(c) }));
		const metadataPolicy = c => c.receiptPolicy?.properties?.metadata?.properties;
		const conditionMessage = (c, condition) => {
			if (!condition?.message) return "";
			const policy = c.receiptPolicy?.properties?.spec?.properties;
			if (condition.type === "DownstreamResolved") {
				const next = c.spec.next || [], masked = pbMask(next, [], policy?.next);
				// This message derives missing ref NAMES, not the whole name@version.
				if (next.some((ref, i) => !resolveCapRef(ref, c.ns) && (masked === "***" || masked[i] !== ref))) return "***";
			} else if (condition.type === "NodesAvailable" && ["CommandMissing", "ValueMissing", "NoMatchingNode"].includes(condition.reason)) {
				const fields = condition.reason === "CommandMissing" ? ["commands"] : condition.reason === "ValueMissing" ? ["values"] : ["commands", "values"];
				const raw = Object.fromEntries(fields.map(k => [k, c.spec.requires?.[k] || []]));
				return pbMask(condition.message, pbDisplaySecrets(raw, pbMask(raw, [], policy?.requires)));
			}
			return condition.message;
		};
		const conditions = ["Implemented", "NodesAvailable", "DownstreamResolved", "Deployed"].map(type => {
			const bad = caps.filter(c => c.status.conditions.find(x => x.type === type)?.status !== "True");
			return { type, status: caps.length && !bad.length ? "True" : "False", reason: !caps.length ? "NotInstalled" : bad[0]?.status.conditions.find(x => x.type === type)?.reason || "Valid",
				members: bad.map(c => pbMask(c.row.name, [], metadataPolicy(c.row)?.name)), message: bad.map(c => conditionMessage(c.row, c.status.conditions.find(x => x.type === type))).filter(Boolean).join("; ") };
		});
		const items = caps.map(c => ({ kind: "capability", name: pbMask(c.row.name, [], metadataPolicy(c.row)?.name), version: pbMask(c.row.version, [], metadataPolicy(c.row)?.version), state: c.status.conditions.find(x => x.type === "NodesAvailable")?.status === "True" ? "ok" : "no_node" }));
		for (const s of owned.sources) {
			const differing = unapplied.some(u => u.kind === "source" && u.ref === s.name);
			const state = differing ? "unapplied" : deriveSourceStatus(s).health;
			const targetsPolicy = s.receiptPolicy?.properties?.spec?.properties?.targets;
			// Mask the display copy before selecting leaves, retaining array/object ancestors.
			const displayTargets = pbMask(s.targets || [], [], targetsPolicy);
			items.push({ kind: "source", name: pbMask(s.name, [], metadataPolicy(s)?.name), state, targets: (s.targets || []).map((t, i) => displayTargets === "***" || displayTargets[i] === "***" ? "***" : displayTargets[i].capability) });
		}
		const sources = { total: owned.sources.length, enabled: owned.sources.filter(s => s.enabled).length, blocked: 0, disabled: 0, unapplied: 0 };
		for (const i of items.filter(i => i.kind === "source")) if (i.state in sources) sources[i.state]++;
		const bad = conditions.find(c => c.status !== "True");
		const blocker = unapplied.length ? { type: unapplied[0].reason, capability: unapplied[0].ref, action: unapplied[0].kind === "source" ? "edit_source" : "edit_capability" }
			: bad ? { type: bad.type, capability: bad.members[0] || null, action: { Implemented: "edit_capability", NodesAvailable: "open_node_page", DownstreamResolved: "create_capability", Deployed: "deploy" }[bad.type] } : null;
		// Mask only provenance-bearing display leaves above, not fixed action/type
		// identities by a substring sweep of the entire status object.
		return pbMask({ ready: caps.length > 0 && !bad && !unapplied.length, unapplied, total: items.length, conditions, blocker, items, sources, lastInstallId: last?.install.id || null, ...(row.source ? { source: row.source } : {}) });
	}
	function pbResource(row, body = false) {
		return { apiVersion: API_VERSION, kind: "Playbook", metadata: { name: row.name, namespace: row.ns, uid: row.uid, resourceVersion: row.rv, creationTimestamp: row.createdAt, displayName: row.root.metadata.displayName || row.name, description: row.root.metadata.description || "", ...(row.root.metadata.version ? { version: row.root.metadata.version } : {}) },
			spec: body ? { files: clone(row.files) } : {}, status: pbStatus(row) };
	}
	function pbSourceComparable(spec, existing) {
		const cfg = sourceWriteConfig(spec.config, spec.kind);
		if (spec.kind === "webhook") {
			if (!("token" in cfg) && existing?.config.token) cfg.token = existing.config.token;
			if (!("secret" in cfg)) cfg.secret = "";
		} else {
			if (!("payload" in cfg)) cfg.payload = {};
			if (spec.kind === "cron" && !cfg.tz) cfg.tz = "UTC";
		}
		return { kind: spec.kind, enabled: spec.enabled !== false, config: cfg };
	}
	function pbDiff(a, b, path = "") {
		if (stableStringify(a) === stableStringify(b)) return [];
		if (pbObject(a) && pbObject(b)) return [...new Set([...Object.keys(a), ...Object.keys(b)])].flatMap(k => pbDiff(a[k], b[k], path ? path + "." + k : k));
		return [path];
	}
	function pbPlan(row, rendered, dryRun) {
		const id = dryRun ? null : "ins_mock_" + (state.playbookInstalls.length + 1);
		const receipt = { apiVersion: API_VERSION, kind: "InstallReceipt", playbook: { name: row.name, version: row.root.metadata.version || null }, ...(row.source ? { source: row.source } : {}), install: { id, at: isoNow(), dryRun }, namespace: row.ns,
			values: rendered.values, valuesDigest: rendered.valuesDigest, created: { capabilities: [], sources: [] }, skipped: { capabilities: [], sources: [] }, upgraded: [], unapplied: [], external: [], pending: [], notes: [] };
		const additions = { capabilities: [], sources: [] }; const conflicts = [];
		const templates = pbPackage(row.files, row.name).documents;
		const policies = new Map();
		const secrets = pbSecrets(row.root.spec.values, rendered.values);
		const display = (value, policy) => {
			const masked = pbMask(value, [], policy); pbDisplaySecrets(value, masked, secrets); return masked;
		};
		for (const slot of ["capabilities", "sources"]) rendered.documents[slot].forEach((d, i) => {
			const policy = pbTemplatePolicy(templates[slot][i].doc, row.root.spec.values);
			policies.set(d, policy);
			const masked = display(d, policy);
			if (d.metadata.version != null && masked.metadata.version !== d.metadata.version) secrets.push(d.metadata.name + "@" + d.metadata.version);
		});
		const note = (code, ref, message) => receipt.notes.push({ code, ...(ref ? { ref } : {}), message });
		for (const d of rendered.documents.capabilities) {
			const name = d.metadata.name; const current = findCap(row.ns, name); const versions = pbVersions(current);
			const explicit = d.metadata.version; const key = capabilityContentKey(d.spec);
			const existing = explicit == null ? versions.find(v => capabilityContentKey(v.spec) === key) : versions.find(v => v.version === explicit);
			if (existing && capabilityContentKey(existing.spec) !== key) {
				conflicts.push({ kind: "capability", ref: name + "@" + explicit, reason: "version_content_immutable", declaredDigest: capabilityDigest(name, explicit, d.spec), actualDigest: existing.digest, fields: pbDiff(capabilityContent(d.spec), capabilityContent(existing.spec), "spec") });
				continue;
			}
			if (existing) {
				const ref = name + "@" + existing.version;
				display({ metadata: { name, version: existing.version } }, existing.receiptPolicy);
				if (existing.receiptRef && existing.receiptRef !== ref || existing.receiptPolicy?.properties?.metadata?.properties?.version?.writeOnly) secrets.push(ref);
				receipt.skipped.capabilities.push(ref);
				if (stableStringify([...(d.spec.next || [])].sort()) !== stableStringify([...(existing.spec.next || [])].sort())) {
					receipt.unapplied.push({ kind: "capability", ref, field: "next", reason: "routing_unapplied", declared: display(d.spec.next || [], policies.get(d).properties?.spec?.properties?.next), actual: display(existing.spec.next || [], (existing.receiptPolicy || policies.get(d)).properties?.spec?.properties?.next) });
					note("routing_unapplied", ref, "Declared next was not applied; edit the existing capability explicitly.");
				}
			} else {
				const version = explicit == null ? Math.max(0, ...versions.map(v => v.version)) + 1 : explicit;
				additions.capabilities.push({ ns: row.ns, name, version, receiptPolicy: policies.get(d), displayName: d.metadata.displayName || "", spec: clone(d.spec), digest: capabilityDigest(name, version, d.spec), installedFrom: id });
				receipt.created.capabilities.push(name + "@" + version);
				if (current && version > current.version) receipt.upgraded.push({ capability: name, from: display(current.version, current.receiptPolicy?.properties?.metadata?.properties?.version), to: display(version, policies.get(d).properties?.metadata?.properties?.version), affects: {
					edges: state.capabilities.filter(c => c.ns === row.ns).flatMap(pbVersions).reduce((n, c) => n + (c.spec.next || []).filter(ref => ref === name).length, 0),
					sources: state.sources.filter(s => s.ns === row.ns && (s.targets || []).some(t => t.capability === name)).length,
				} });
			}
		}
		for (const d of rendered.documents.sources) {
			const name = d.metadata.name; const existing = findSource(row.ns, name);
			const declared = pbSourceComparable(d.spec, existing);
			if (existing) {
				const actual = pbSourceComparable(existing);
				const fields = pbDiff(declared, actual);
				if (fields.length) { conflicts.push({ kind: "source", ref: name, reason: "config_differs", declaredDigest: mockDigest(stableStringify(declared)), actualDigest: mockDigest(stableStringify(actual)), fields }); continue; }
				display({ metadata: { name } }, existing.receiptPolicy);
				if (existing.receiptRef && existing.receiptRef !== name) secrets.push(name);
				receipt.skipped.sources.push(name);
				const targets = ts => (ts || []).map(t => Object.assign({}, t, { params: t.params || {} })).sort((a, b) => stableStringify(a).localeCompare(stableStringify(b)));
				if (stableStringify(targets(d.spec.targets)) !== stableStringify(targets(existing.targets))) {
					receipt.unapplied.push({ kind: "source", ref: name, field: "targets", reason: "targets_unapplied", declared: display(d.spec.targets || [], policies.get(d).properties?.spec?.properties?.targets), actual: display(existing.targets || [], (existing.receiptPolicy || policies.get(d)).properties?.spec?.properties?.targets) });
					note("targets_unapplied", name, "Declared targets/params were not applied; edit the existing source explicitly.");
				}
			} else {
				const config = sourceWriteConfig(d.spec.config, d.spec.kind);
				if (d.spec.kind === "webhook" && !config.token) config.token = "src_mock_" + row.ns + "_" + name;
				additions.sources.push({ ns: row.ns, name, displayName: d.metadata.displayName || "", kind: d.spec.kind, enabled: d.spec.enabled !== false, config, receiptPolicy: policies.get(d), targets: clone(d.spec.targets || []), installedFrom: id, lastFiredAt: null, nextFireAt: null, lastError: null, unapplied: false });
				receipt.created.sources.push(name);
			}
			if (d.spec.enabled === false) note("source_disabled", name, "Source is disabled.");
		}
		if (conflicts.length) return { error: statusErr(400, "playbook_conflict", "Whole package rejected; no resources or receipt written", pbMask({ valuesDigest: rendered.valuesDigest, conflicts, hint: "Choose a new explicit capability version or edit the existing source separately; install never overwrites existing definitions." }, secrets)) };
		const inPackage = new Set(rendered.documents.capabilities.map(d => d.metadata.name));
		const refs = rendered.documents.capabilities.flatMap(d => d.spec.next || []).concat(rendered.documents.sources.flatMap(d => (d.spec.targets || []).map(t => t.capability)));
		for (const ref of [...new Set(refs)]) {
			const parsed = splitRef(ref);
			const added = additions.capabilities.find(c => c.name === parsed.name && (parsed.version == null || c.version === parsed.version));
			const existing = resolveCapRef(ref, row.ns);
			if (!added && !existing) { receipt.pending.push(ref); note("pending_ref", ref, "Reference does not resolve; add the missing capability."); }
			else if (!inPackage.has(parsed.name) && existing) {
				const actualRef = existing.name + "@" + existing.version;
				// The execution resolver deliberately returns a minimal archived deployment view.
				// Read ONLY the selected version's original display provenance separately.
				const identity = pbVersions(findCap(row.ns, existing.name)).find(c => c.version === existing.version);
				display({ metadata: { name: existing.name, version: existing.version } }, identity?.receiptPolicy);
				if (identity?.receiptRef && identity.receiptRef !== actualRef || identity?.receiptPolicy?.properties?.metadata?.properties?.version?.writeOnly) secrets.push(actualRef);
				receipt.external.push(actualRef);
			}
		}
		// Reachability uses ACTUAL reused routes, not the unapplied declaration.
		const actualCaps = rendered.documents.capabilities.map(d => additions.capabilities.find(c => c.name === d.metadata.name) || pbVersions(findCap(row.ns, d.metadata.name)).find(c => receipt.skipped.capabilities.includes(c.name + "@" + c.version)));
		const actualSources = rendered.documents.sources.map(d => additions.sources.find(s => s.name === d.metadata.name) || findSource(row.ns, d.metadata.name));
		const incoming = new Set(actualCaps.flatMap(c => c?.spec.next || []).concat(actualSources.flatMap(s => (s?.targets || []).map(t => t.capability))).map(ref => splitRef(ref).name));
		for (const c of actualCaps) if (c && !incoming.has(c.name)) note("unreachable", c.name + "@" + c.version, "No package upstream/source points to this capability.");
		if (actualCaps.length && !actualCaps.some(c => deriveCapabilityStatus(c).conditions.find(x => x.type === "NodesAvailable")?.status === "True")) note("no_node", null, "No package capability can run on the current node pool.");
		secrets.push(...rendered.documents.sources.flatMap(d => [d.spec.config?.token, d.spec.config?.secret]).filter(v => typeof v === "string" && v));
		// 脱敏分两层：schema 的 writeOnly 路径按**字段身份**遮（数字/布尔/对象/数组
		// 同样遮得住，不依赖“secret 是不是字符串”），字符串包含匹配只用于 schema
		// 之外的值（source token/secret 等）。应用值（写进资源的 config）与
		// valuesDigest 都不受影响 —— 脱敏只改回执/历史的展示副本。
		receipt.values = pbMask(receipt.values, secrets, row.root.spec.values);
		return { receipt: pbMask(receipt, secrets), additions };
	}
	function pbCommit(row, plan) {
		for (const [index, added] of plan.additions.capabilities.entries()) {
			const old = findCap(row.ns, added.name);
			const time = isoNow(); const fresh = Object.assign({}, added, { uid: old ? old.uid : nextUID("cap"), receiptRef: plan.receipt.created.capabilities[index], rv: nextRV(), createdAt: time, versionAt: time, versions: [] });
			if (!old) state.capabilities.push(fresh);
			else {
				const all = pbVersions(old).concat(fresh).sort((a, b) => b.version - a.version);
				const latest = Object.assign({}, all[0], { versions: all.slice(1).map(v => ({ version: v.version, spec: clone(v.spec), digest: v.digest, at: v.versionAt || v.createdAt, installedFrom: v.installedFrom, receiptPolicy: clone(v.receiptPolicy || {}), receiptRef: v.receiptRef })) });
				state.capabilities.splice(state.capabilities.indexOf(old), 1, latest);
			}
		}
		for (const [index, added] of plan.additions.sources.entries()) state.sources.push(Object.assign({}, added, { uid: nextUID("src"), receiptRef: plan.receipt.created.sources[index], rv: nextRV(), createdAt: isoNow() }));
		const receipt = plan.receipt;
		state.playbookInstalls.push({ id: receipt.install.id, ns: row.ns, playbookName: row.name, playbookUid: row.uid, valuesDigest: receipt.valuesDigest, values: clone(receipt.values), source: row.source || null, receipt: clone(receipt) });
	}
	function pbRequest(handler) {
		return ctx => {
			try { return handler(ctx); }
			catch (error) { if (error.reason) return statusErr(400, error.reason, error.message); throw error; }
		};
	}
	const pbFind = (ns, name) => state.playbooks.find(p => p.ns === ns && p.name === name);
	function pbSave(ctx, updating) {
		const ns = decodeURIComponent(ctx.m[1]); const miss = nsMissing(ns); if (miss) return miss;
		const body = ctx.body; const invalid = validateWriteBody(body, "Playbook", ns); if (invalid) return invalid;
		const name = body.metadata?.name;
		if (typeof name !== "string" || !/^[a-z0-9]+(-[a-z0-9]+)*$/.test(name) || name.length > 64) return badRequest("slug_invalid", "metadata.name must be a package slug");
		const existing = pbFind(ns, updating ? decodeURIComponent(ctx.m[2]) : name);
		if (updating && !existing) return notFound("Playbook ");
		if (updating && existing.name !== name) return badRequest("name_immutable", "metadata.name is immutable");
		if (!updating && existing) return statusErr(409, "slug_taken", "Package exists; use PUT with its current resourceVersion to replace the template");
		if (updating) { const cas = checkCas(body, existing); if (cas) return cas; }
		const spec = body.spec;
		if (!pbObject(spec) || Object.keys(spec).some(k => !["files", "source"].includes(k)) || ("files" in spec) === ("source" in spec)) return badRequest("invalid_playbook", "spec.files OR spec.source, never a second root manifest");
		// No fake OCI success: the in-browser mock has no registry credentials/network.
		if ("source" in spec) return badRequest(typeof spec.source === "string" && spec.source.startsWith("oci://") ? "oci_pull_failed" : "oci_unsupported", "Mock cannot pull an OCI registry; upload a self-contained file map or use the real server");
		const pkg = pbPackage(spec.files, name);
		const row = Object.assign({}, existing || {}, { ns, name, root: pkg.root, files: clone(spec.files), source: null, rv: nextRV(), uid: existing?.uid || nextUID("pb"), createdAt: existing?.createdAt || isoNow() });
		if (existing) state.playbooks.splice(state.playbooks.indexOf(existing), 1, row); else state.playbooks.push(row);
		return json(pbResource(row), updating ? 200 : 201);
	}
	on("POST", PB_LIST, pbRequest(ctx => pbSave(ctx, false)));
	on("PUT", PB_ONE, pbRequest(ctx => pbSave(ctx, true)));
	on("GET", PB_LIST, ctx => {
		const ns = decodeURIComponent(ctx.m[1]); const miss = nsMissing(ns); if (miss) return miss;
		return json(listOf("PlaybookList", state.playbooks.filter(p => p.ns === ns).map(p => pbResource(p))));
	});
	on("GET", PB_ONE, ctx => {
		const ns = decodeURIComponent(ctx.m[1]); const miss = nsMissing(ns); if (miss) return miss;
		const row = pbFind(ns, decodeURIComponent(ctx.m[2])); return row ? json(pbResource(row, ctx.query.get("includeBody") === "true")) : notFound("Playbook ");
	});
	for (const action of ["render", "install"]) on("POST", new RegExp("^/v1/namespaces/([^/]+)/playbooks/([^/]+)/" + action + "$"), pbRequest(ctx => {
		const ns = decodeURIComponent(ctx.m[1]); const miss = nsMissing(ns); if (miss) return miss;
		const row = pbFind(ns, decodeURIComponent(ctx.m[2])); if (!row) return notFound("Playbook ");
		if (!pbObject(ctx.body || {}) || Object.keys(ctx.body || {}).some(k => !["values", ...(action === "install" ? ["dryRun"] : [])].includes(k))) return badRequest("unknown_field", "Only values and install.dryRun are accepted");
		const rendered = pbRender(row, ctx.body?.values);
		if (action === "render") return json(rendered);
		const dryRun = ctx.body?.dryRun === true;
		const plan = pbPlan(row, rendered, dryRun); if (plan.error) return plan.error;
		if (!dryRun) pbCommit(row, plan);
		return json(plan.receipt);
	}));
	on("GET", /^\/v1\/namespaces\/([^/]+)\/playbooks\/([^/]+)\/installs(?:\/([^/]+))?$/, ctx => {
		const ns = decodeURIComponent(ctx.m[1]); const miss = nsMissing(ns); if (miss) return miss;
		const name = decodeURIComponent(ctx.m[2]); const row = pbFind(ns, name);
		const records = state.playbookInstalls.filter(i => i.ns === ns && i.playbookName === name && (!row || i.playbookUid === row.uid));
		if (ctx.m[3]) {
			const record = records.find(i => i.id === decodeURIComponent(ctx.m[3])); return record ? json(clone(record.receipt)) : notFound("InstallReceipt ");
		}
		if (!row) return notFound("Playbook ");
		const groups = new Map();
		for (const i of records) {
			let g = groups.get(i.valuesDigest);
			if (!g) { g = { valuesDigest: i.valuesDigest, values: clone(i.values), firstAt: i.receipt.install.at, lastAt: i.receipt.install.at, attempts: 0, lastInstallId: i.id, capabilities: [], sources: [] }; groups.set(i.valuesDigest, g); }
			g.attempts++; g.lastAt = i.receipt.install.at; g.lastInstallId = i.id;
			const ids = new Set(records.filter(r => r.valuesDigest === i.valuesDigest).map(r => r.id));
			g.capabilities = [...new Set(state.capabilities.filter(c => c.ns === ns).flatMap(pbVersions).filter(c => ids.has(c.installedFrom)).map(c => c.receiptRef || c.name + "@" + c.version))];
			g.sources = state.sources.filter(s => s.ns === ns && ids.has(s.installedFrom)).map(s => s.receiptRef || s.name);
		}
		return json({ total: groups.size, installs: [...groups.values()].sort((a, b) => b.lastAt.localeCompare(a.lastAt)).slice(0, 200) });
	});
	on("DELETE", PB_ONE, ctx => {
		const ns = decodeURIComponent(ctx.m[1]); const miss = nsMissing(ns); if (miss) return miss;
		const row = pbFind(ns, decodeURIComponent(ctx.m[2])); if (!row) return notFound("Playbook ");
		const cas = checkCasQuery(ctx.query, row); if (cas) return cas;
		const owned = pbOwned(row);
		state.playbooks.splice(state.playbooks.indexOf(row), 1);
		return json({ deleted: true, kept: { capabilities: [...new Set(owned.capabilities.map(c => c.name + "@" + c.version))], sources: owned.sources.map(s => s.name) }, values: [...new Map(owned.installs.map(i => [i.valuesDigest, clone(i.values)])).values()], note: "Only the manifest was removed. Resources and receipts remain; remove capabilities/sources separately if needed." });
	});

	// ===== 能力（ns 私有）=====
	//
	// 写入期的版本规则按 capability.md §3：
	//   foldKey/digest 都**不含** `spec.next` 与 `metadata.displayName` —— 改路由不产生
	//   新版本、digest 也不变；改 params/requires/lifecycle/concurrency/run 一定产生
	//   新版本；显式版本的内容不可变（同版本正文不同 → 400 version_content_immutable）。
	// mock 里同样实现，页面才能演示“改 run 出新版本 / 只改 next 不换版本”。
	function stableStringify(v) {
		if (v === null || typeof v !== "object") return JSON.stringify(v);
		if (Array.isArray(v)) return "[" + v.map(stableStringify).join(",") + "]";
		const keys = Object.keys(v).sort();
		return (
			"{" +
			keys.map((k) => JSON.stringify(k) + ":" + stableStringify(v[k])).join(",") +
			"}"
		);
	}

	// 参与「是不是同一版内容」的部分：next/displayName 刻意排除。
	function capabilityContent(spec) {
		const s = spec && typeof spec === "object" ? spec : {};
		return {
			params: s.params === undefined ? {} : s.params,
			requires: s.requires === undefined ? {} : s.requires,
			lifecycle: s.lifecycle === undefined ? {} : s.lifecycle,
			concurrency: s.concurrency === undefined ? {} : s.concurrency,
			run: s.run === undefined ? {} : s.run,
		};
	}
	function capabilityContentKey(spec) {
		return stableStringify(capabilityContent(spec));
	}
	function capabilityDigest(name, version, spec) {
		return mockDigest("cap:" + name + "@" + version + ":" + capabilityContentKey(spec));
	}

	// 单建（POST）的写入期校验（capability.md §6.1 的常用项）。
	// 只做能在 mock 里确定性回答的几条；执行期才知道的事不在此列。
	function validateNewCapability(spec, ns) {
		const s = spec && typeof spec === "object" ? spec : {};
		const run = s.run && typeof s.run === "object" ? s.run : {};
		if (!run.entrypoint) {
			return badRequest("impl_missing", "没有 run.entrypoint：能力的实现只有脚本一种");
		}
		if (!run.files || !Object.prototype.hasOwnProperty.call(run.files, run.entrypoint)) {
			return badRequest(
				"entrypoint_no_shebang",
				"entrypoint " + run.entrypoint + " 不在 run.files 里（入口必须是文件里的一个路径）",
			);
		}
		const next = Array.isArray(s.next) ? s.next : [];
		const unknown = next.filter((ref) => !resolveCapRef(ref, ns)).map((ref) => splitRef(ref).name);
		if (unknown.length) {
			return badRequest(
				"unknown_target",
				"next 里的能力在当前命名空间解析不到：" + unknown.join("、") +
					"（单建严格；先建下游，或用安装包补齐）",
			);
		}
		const requires = s.requires && typeof s.requires === "object" ? s.requires : {};
		const declaredValues = Array.isArray(requires.values) ? requires.values : [];
		const env = run.env && typeof run.env === "object" ? run.env : {};
		const missing = [];
		for (const key of Object.keys(env)) {
			const val = env[key];
			const path =
				val && typeof val === "object" && val.valueFrom && val.valueFrom.fieldRef
					? String(val.valueFrom.fieldRef.fieldPath || "")
					: "";
			if (path.indexOf("vars.") === 0 && declaredValues.indexOf(path) < 0) missing.push(path);
		}
		if (missing.length) {
			return badRequest(
				"undeclared_need",
				"run.env 引用了没声明进 spec.requires.values 的值：" + missing.join("、"),
			);
		}
		const conc = s.concurrency && typeof s.concurrency === "object" ? s.concurrency : {};
		if (conc.maxPerNode != null) {
			const n = Number(conc.maxPerNode);
			if (!Number.isInteger(n) || n < 1) {
				return badRequest("concurrency_invalid", "spec.concurrency.maxPerNode 必须是 ≥1 的整数");
			}
		}
		return null;
	}

	// 版本历史的有序视图（新→旧）：当前版本在最前，归档版本按版本号倒序。
	// 版本历史 = `CapabilityList.items`，每项都是**同一种 Capability 信封**
	// （api-conventions §5.3：versions 不是独立 kind，也没有自造 item.current）。
	// “当前版本”由调用方按 max(metadata.version) 判定，不写进 wire。
	function versionSpec(spec, includeBody) {
		const out = clone(spec || {});
		if (!includeBody && out.run && typeof out.run === "object") delete out.run.files;
		return out;
	}
	function archivedVersionResource(row, v, includeBody) {
		return {
			apiVersion: API_VERSION,
			kind: "Capability",
			metadata: {
				name: row.name,
				namespace: row.ns,
				uid: row.uid,
				version: v.version,
				displayName: row.displayName || "",
				creationTimestamp: v.at,
			},
			spec: versionSpec(v.spec, includeBody),
			// 历史版本只给身份/摘要字段：conditions/nodes 是“当前事实”的派生，
			// 不能伪造到旧版本上（版本化 status 语义未冻结）。
			status: { version: v.version, digest: v.digest },
		};
	}
	function currentVersionResource(row, includeBody) {
		const res = capabilityResource(row);
		if (!includeBody) {
			const copy = clone(res);
			if (copy.spec && copy.spec.run && typeof copy.spec.run === "object") delete copy.spec.run.files;
			return copy;
		}
		return res;
	}
	// 列表只带摘要（`?includeBody=true` 才把 run.files 正文带上）。
	function capabilityVersionList(row) {
		return [currentVersionResource(row, false)].concat(
			(row.versions || [])
				.slice()
				.sort((a, b) => b.version - a.version)
				.map((v) => archivedVersionResource(row, v, false)),
		);
	}

	const CAPS = /^\/v1\/namespaces\/([^/]+)\/capabilities$/;
	const CAP_ONE = /^\/v1\/namespaces\/([^/]+)\/capabilities\/([^/]+)$/;

	on("GET", CAPS, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		return json(
			listOf(
				"CapabilityList",
				state.capabilities.filter((c) => c.ns === ns).map(capabilityResource),
			),
		);
	});

	on("POST", CAPS, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		const body = ctx.body;
		const derr = validateWriteBody(body, "Capability", ns);
		if (derr) return derr;
		const name = String((body.metadata && body.metadata.name) || "").trim();
		if (!NAME_RE.test(name)) {
			return badRequest("BadRequest", "metadata.name 只允许小写字母/数字/._-，且以字母或数字开头");
		}
		if (findCap(ns, name)) {
			return statusErr(409, "AlreadyExists", "能力 " + name + " 已存在（升级请用 PATCH）");
		}
		const spec = clone(body.spec || {});
		// 单建严格（capability.md §6.3）：next 解析不到 / env 用了没声明的值都拒。
		const verr = validateNewCapability(spec, ns);
		if (verr) return verr;
		const version = Number((body.metadata && body.metadata.version) || 1) || 1;
		const row = {
			ns: ns,
			name: name,
			displayName: (body.metadata && body.metadata.displayName) || "",
			version: version,
			spec: spec,
			digest: capabilityDigest(name, version, spec),
			rv: nextRV(),
			createdAt: isoNow(),
			versionAt: isoNow(),
			versions: [],
			installedFrom: null,
			uid: nextUID("cap"),
		};
		state.capabilities.push(row);
		return json(capabilityResource(row), 201);
	});

	on("GET", CAP_ONE, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		const row = findCap(ns, decodeURIComponent(ctx.m[2]));
		if (!row) return notFound("能力 ");
		return json(capabilityResource(row));
	});

	on("PATCH", CAP_ONE, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		const row = findCap(ns, decodeURIComponent(ctx.m[2]));
		if (!row) return notFound("能力 ");
		const body = ctx.body;
		const derr = validateWriteBody(body, "Capability", ns);
		if (derr) return derr;
		const cas = checkCas(body, row);
		if (cas) return cas;

		// name / namespace 不可变（改名 = 另建一个）
		if (body.metadata && body.metadata.name && body.metadata.name !== row.name) {
			return badRequest("name_immutable", "metadata.name 不可变");
		}
		// 版本规则（capability.md §3）：改 next/displayName 原地生效；改正文出新版本。
		const nextSpec = body.spec ? clone(body.spec) : clone(row.spec);
		const contentChanged = capabilityContentKey(nextSpec) !== capabilityContentKey(row.spec);
		const explicitVersion =
			body.metadata && body.metadata.version != null && body.metadata.version !== ""
				? Number(body.metadata.version)
				: null;
		if (explicitVersion != null && (!Number.isInteger(explicitVersion) || explicitVersion < 1)) {
			return badRequest("BadRequest", "metadata.version 必须是 ≥1 的整数");
		}
		if (contentChanged) {
			// 版本号唯一 + 正文不可变：显式版本只要**已存在**（当前行或归档行）
			// 就不能换正文。只比 `row.version` 会让归档版本被覆盖，历史与
			// “当前 = max(metadata.version)”两个不变式一起被打破。
			const archived = (row.versions || []).slice();
			const known = [row.version].concat(archived.map((v) => v.version));
			const maxVersion = Math.max.apply(null, known);
			if (explicitVersion != null && (explicitVersion === row.version || archived.some((v) => v.version === explicitVersion))) {
				return badRequest(
					"version_content_immutable",
					"显式版本 " + explicitVersion +
						" 的执行正文不可变：改 run/params/requires/lifecycle/concurrency 必须换一个 version（next/displayName 例外）",
				);
			}
			// 显式版本必须比所有已知版本更新：否则“当前版本”会低于历史最大值，
			// 页面的 latest=max 投影会指向一个旧正文。
			if (explicitVersion != null && explicitVersion < maxVersion) {
				return badRequest(
					"version_not_increasing",
					"显式版本 " + explicitVersion + " 小于已存在的最大版本 " + maxVersion + "：当前版本必须 = max(metadata.version)",
				);
			}
			// 归档旧版（digest/正文留着：节点可能还拿着旧 digest）。
			row.versions = archived.concat([
				{
					version: row.version,
					digest: row.digest,
					at: row.versionAt || row.createdAt,
					spec: clone(row.spec || {}),
					installedFrom: row.installedFrom,
					receiptPolicy: clone(row.receiptPolicy || {}),
					receiptRef: row.receiptRef,
				},
			]);
			// 省略 version = 内容寻址：没有可复用的同一正文行，分配下一个版本号。
			row.version = explicitVersion != null ? explicitVersion : maxVersion + 1;
			row.versionAt = isoNow();
			row.digest = capabilityDigest(row.name, row.version, nextSpec);
		}
		row.spec = nextSpec;
		if (body.metadata && "displayName" in body.metadata) {
			row.displayName = body.metadata.displayName || "";
		}
		row.rv = nextRV();
		return json(capabilityResource(row));
	});

	on("DELETE", CAP_ONE, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		const row = findCap(ns, decodeURIComponent(ctx.m[2]));
		if (!row) return notFound("能力 ");
		const cas = checkCasQuery(ctx.query, row);
		if (cas) return cas;
		// 允许直接删：不因 next / targets 阻塞（引用方由 status 报缺失）
		state.capabilities.splice(state.capabilities.indexOf(row), 1);
		state.deployments = state.deployments.filter(
			(d) => !(d.ns === ns && d.capability === row.name),
		);
		return json({ apiVersion: API_VERSION, kind: "Status", status: "Success", code: 200 });
	});

	// 人工部署：把目标版本的 digest 写成 deployments 事实（install 不会自动调用它）。
	on("POST", /^\/v1\/namespaces\/([^/]+)\/capabilities\/([^/]+)\/deploy$/, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		const row = findCap(ns, decodeURIComponent(ctx.m[2]));
		if (!row) return notFound("能力 ");
		const body = ctx.body || {};
		if (body.status != null) return badRequest("status_not_writable", "deploy 不接受 status");
		const version = body.version == null ? row.version : Number(body.version);
		if (!Number.isInteger(version) || version < 1) {
			return badRequest("BadRequest", "version 必须是 ≥1 的整数");
		}
		// 目标版本可以比当前版本旧（节点上还拿着旧 digest 时手动回滚）；只在历史里查到也算。
		const target = resolveCapRef(row.name + "@" + version, ns);
		if (!target) {
			return badRequest("BadRequest", "能力 " + row.name + " 没有版本 " + version);
		}
		const nodes =
			Array.isArray(body.nodes) && body.nodes.length
				? body.nodes.map(String)
				: capCandidates(ns).map((n) => n.name);
		for (const node of nodes) {
			const hit = state.deployments.find(
				(d) => d.ns === ns && d.capability === row.name && d.node === node,
			);
			const fact = {
				ns: ns,
				capability: row.name,
				version: target.version,
				digest: target.digest,
				node: node,
				updatedAt: isoNow(),
			};
			if (hit) Object.assign(hit, fact);
			else state.deployments.push(fact);
		}
		return json(
			listOf(
				"DeploymentList",
				deploymentItems(ns).filter((d) => d.capability === row.name),
			),
			201,
		);
	});

	// 版本历史（只读列表：CapabilityList.items，每项同 Capability 信封）。
	on("GET", /^\/v1\/namespaces\/([^/]+)\/capabilities\/([^/]+)\/versions$/, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		const row = findCap(ns, decodeURIComponent(ctx.m[2]));
		if (!row) return notFound("能力 ");
		return json(listOf("CapabilityList", capabilityVersionList(row)));
	});

	// 单版本 = 同一个 Capability 信封；`?includeBody=true` 才把 run.files 正文带上。
	on(
		"GET",
		/^\/v1\/namespaces\/([^/]+)\/capabilities\/([^/]+)\/versions\/([^/]+)$/,
		(ctx) => {
			const ns = decodeURIComponent(ctx.m[1]);
			const miss = nsMissing(ns);
			if (miss) return miss;
			const row = findCap(ns, decodeURIComponent(ctx.m[2]));
			if (!row) return notFound("能力 ");
			const want = Number(decodeURIComponent(ctx.m[3]));
			const includeBody = ctx.query.get("includeBody") === "true";
			if (want === row.version) return json(currentVersionResource(row, includeBody));
			const v = (row.versions || []).find((x) => x.version === want);
			if (!v) return notFound("版本 ");
			return json(archivedVersionResource(row, v, includeBody));
		},
	);

	// ===== 源（ns 私有）=====
	const SRC = /^\/v1\/namespaces\/([^/]+)\/sources$/;
	const SRC_ONE = /^\/v1\/namespaces\/([^/]+)\/sources\/([^/]+)$/;
	const SOURCE_KINDS = ["cron", "webhook", "manual"];

	// 配置期校验（source.md §6）：cron 五段、payloadSchema 形状、模板引用合法性。
	function validateSourceSpec(spec) {
		if (!["cron", "webhook", "manual"].includes(spec && spec.kind)) return {reason:"invalid_source_kind",message:"kind: cron | webhook | manual"};
		const invalid = message => ({reason:"invalid_source_config",message});
		const obj = v => v && typeof v === "object" && !Array.isArray(v);
		const cfg = spec.config || {};
		if (!obj(cfg)) return invalid("config must be an object");
		if (cfg.payloadSchema != null && !obj(cfg.payloadSchema)) return invalid("config.payloadSchema must be an object or null");
		if (cfg.sample != null && !obj(cfg.sample)) return invalid("config.sample must be an object");
		if (cfg.payload != null && !obj(cfg.payload)) return invalid("config.payload must be an object");
		if (spec.kind === "cron") {
			const fields = String(cfg.expr || "").trim().split(/\s+/);
			const bounds = [[0,59],[0,23],[1,31],[1,12],[0,6]];
			if (fields.length !== 5 || fields.some((f,i) => f.split(",").some(part => {
				const m = /^(\*|\d+(?:-\d+)?)(?:\/(\d+))?$/.exec(part);
				if (!m || (m[2] && Number(m[2]) < 1)) return true;
				if (m[1] === "*") return false;
				const nums = m[1].split("-").map(Number);
				return nums.some(n=>n<bounds[i][0] || n>bounds[i][1]) || (nums.length===2 && nums[0]>nums[1]);
			}))) return invalid("config.expr: invalid five-field cron expression");
			if (cfg.tz && cfg.tz !== "Local") { try { new Intl.DateTimeFormat("en",{timeZone:cfg.tz}); } catch { return invalid("config.tz: invalid IANA timezone"); } }
		}
		if (!Array.isArray(spec.targets || [])) return invalid("spec.targets must be an array");
		for (const target of spec.targets || []) {
			if (!target || typeof target.capability !== "string" || !target.capability) return invalid("targets[].capability required");
			if (target.params != null && !obj(target.params)) return invalid("targets[].params must be an object");
			const err = templateError(target.params || {}); if (err) return invalid(err);
		}
		return null;
	}

	// Read-only config projections never become stored credentials/default webhook payload.
	function sourceWriteConfig(config, kind) {
		const cfg = clone(config || {});
		delete cfg.hookUrl; delete cfg.secretSet;
		if (kind === "webhook") delete cfg.payload;
		else { delete cfg.token; delete cfg.secret; delete cfg.sample; }
		return cfg;
	}
	function sourcePayload(row, body) {
		return Object.assign({}, row.kind === "webhook" ? {} : row.config.payload || {}, body.payload || {});
	}
	function previewSource(row, payload) {
		const plan = planDispatch(row, payload);
		return {
			targets: (row.targets || []).map(t => {
				const cap = resolveCapRef(t.capability, row.ns);
				const single = planDispatch(Object.assign({},row,{targets:[t]}), payload);
				const ready = single.tasks[0]; const prune = single.pruned[0];
				return Object.assign({capability:t.capability,params:ready ? ready.params : {},state:!cap ? "missing" : ready ? "ready" : "pruned"},
					cap ? {version:cap.version} : {}, prune ? {reason:prune.reason,detail:prune.detail} : {});
			}),
			pruned: plan.pruned.map(p=>Object.assign(p.to ? {capability:p.to} : {},{reason:p.reason,detail:p.detail}))
		};
	}

	on("GET", SRC, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		return json(
			listOf(
				"SourceList",
				state.sources.filter((s) => s.ns === ns).map(sourceResource),
			),
		);
	});

	on("POST", SRC, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		const body = ctx.body;
		const derr = validateWriteBody(body, "Source", ns);
		if (derr) return derr;
		const name = String((body.metadata && body.metadata.name) || "").trim();
		if (!NAME_RE.test(name)) {
			return badRequest("BadRequest", "metadata.name 只允许小写字母/数字/._-，且以字母或数字开头");
		}
		const spec = clone(body.spec || {});
		const verr = validateSourceSpec(spec);
		if (verr) return statusErr(400, verr.reason, verr.message);
		if (findSource(ns, name)) {
			return statusErr(409, "source_name_used", "源 " + name + " 已存在");
		}
		const cfg = sourceWriteConfig(spec.config, spec.kind);
		// webhook：没给 token 就服务端生成（GET 只回 hookUrl，不回 token 本身）
		if (spec.kind === "webhook" && !cfg.token) cfg.token = "src_mock_" + name + (ns === "default" ? "" : "_" + ns);
		const row = {
			ns: ns,
			name: name,
			displayName: (body.metadata && body.metadata.displayName) || "",
			kind: spec.kind,
			enabled: spec.enabled !== false,
			config: cfg,
			targets: clone(spec.targets || []),
			rv: nextRV(),
			createdAt: isoNow(),
			lastFiredAt: null,
			nextFireAt: null,
			lastError: null,
			installedFrom: null,
			unapplied: false,
			uid: nextUID("src"),
		};
		state.sources.push(row);
		return json(sourceResource(row), 201);
	});

	on("GET", SRC_ONE, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		const row = findSource(ns, decodeURIComponent(ctx.m[2]));
		if (!row) return notFound("源 ");
		return json(sourceResource(row));
	});

	on("PATCH", SRC_ONE, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		const row = findSource(ns, decodeURIComponent(ctx.m[2]));
		if (!row) return notFound("源 ");
		const body = ctx.body;
		const derr = validateWriteBody(body, "Source", ns);
		if (derr) return derr;
		const cas = checkCas(body, row);
		if (cas) return cas;
		if (body.metadata && body.metadata.name && body.metadata.name !== row.name) {
			return badRequest("name_immutable", "metadata.name 不可变");
		}
		const spec = clone(body.spec || {});
		if (spec.kind && spec.kind !== row.kind) {
			return statusErr(400, "source_kind_immutable", "spec.kind 不可变（换 kind 请删了重建）");
		}
		const merged = {
			kind: row.kind,
			enabled: spec.enabled === undefined ? row.enabled : spec.enabled !== false,
			config: Object.assign({}, row.config, sourceWriteConfig(spec.config, row.kind)),
			targets: spec.targets === undefined ? row.targets : spec.targets,
		};
		const verr = validateSourceSpec(merged);
		if (verr) return statusErr(400, verr.reason, verr.message);
		// 写请求省略 token / secret = **保留已有**（前端用空串表示"没改"）
		const cfg = merged.config;
		if (!cfg.token) cfg.token = row.config.token;
		if (!cfg.secret) cfg.secret = row.config.secret;
		if (merged.kind === "webhook" && !cfg.token) cfg.token = "src_mock_" + row.name;
		row.enabled = merged.enabled;
		row.config = cfg;
		row.targets = clone(merged.targets || []);
		if (body.metadata && "displayName" in body.metadata) {
			row.displayName = body.metadata.displayName || "";
		}
		row.rv = nextRV();
		return json(sourceResource(row));
	});

	on("DELETE", SRC_ONE, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		const row = findSource(ns, decodeURIComponent(ctx.m[2]));
		if (!row) return notFound("源 ");
		const cas = checkCasQuery(ctx.query, row);
		if (cas) return cas;
		state.sources.splice(state.sources.indexOf(row), 1);
		return json({ apiVersion: API_VERSION, kind: "Status", status: "Success", code: 200 });
	});

	// 事件入库 + 派发：fire 与 ingress 共用（去重、剪枝留痕、建任务）。
	let taskSeq = 12;
	function nextTaskName() {
		return "tsk_mock_" + ++taskSeq;
	}
	function recordEvent(ev) {
		state.events.push(ev);
		return ev;
	}
	// 派发计划落地：建 pending 任务 + 剪枝事件（剪枝**永远留痕**）。
	function applyPlan(row, ev, plan, payload) {
		const created = [];
		for (const p of plan.tasks) {
			const tk = {
				ns: row.ns,
				name: nextTaskName(),
				traceUID: ev.traceUID,
				sequence: created.length + 2,
				sourceRef: row.name,
				parentRef: null,
				parentTaskId: null,
				capability: p.capability,
				capabilityVersion: p.version,
				params: clone(p.params),
				phase: "pending",
				attempt: 0,
				maxAttempts: 3,
				waitingReason: "waiting_for_slot",
				nextAttemptAt: iso(now() + 5),
				createdAt: isoNow(),
				envDiagnostics: [],
				result: { summary: null, artifacts: [], error: null },
				attempts: [],
			};
			state.tasks.push(tk);
			created.push({ name: tk.name, capability: tk.capability, version: tk.capabilityVersion });
		}
		if (plan.pruned.length) {
			const pruneEv = {
				ns: row.ns,
				name: "evt_mock_" + (state.events.length + 1) + "_prune",
				reason: "Pruned",
				type: "Warning",
				involvedObject: { kind: "Source", name: row.name, uid: "src_mock_" + row.name },
				traceUID: ev.traceUID,
				eventType: ev.eventType,
				deliveryId: null,
				payload: clone(payload || {}),
				signature: null,
				dispatched: [],
				prunedCount: plan.pruned.length,
				deliveredAt: isoNow(),
				createdAt: isoNow(),
			};
			recordEvent(pruneEv);
			for (const p of plan.pruned) {
				state.prunes.push({
					ns: row.ns,
					traceUID: ev.traceUID,
					task: created.length ? created[0].name : null,
					event: pruneEv.name,
					to: p.to || null,
					reason: p.reason,
					detail: p.detail,
				});
			}
		}
		return created;
	}

	// 一次投递的公共路径（fire / ingress 共用）：入库 → 去重 → 派发 → 回执。
	function deliver(row, opts) {
		const o = opts || {};
		const payload = clone(o.payload || {});
		const deliveryId = o.deliveryId || null;
		// (namespace, source, deliveryId) 去重30天；接受不是任务成功。
		if (deliveryId) {
			const dup = state.events.find(
				(e) => e.ns === row.ns && e.involvedObject.name === row.name && e.deliveryId === deliveryId && e.reason === "Ingested" && Date.parse(e.createdAt) >= Date.now() - 2592000000,
			);
			if (dup) return Object.assign({}, clone(dup.receipt || {eventId:dup.name,traceId:dup.traceUID,tasks:[],pruned:[]}), {duplicate:true});
		}
		const ev = {
			ns: row.ns,
			name: "evt_mock_" + (state.events.length + 1),
			reason: "Ingested",
			type: "Normal",
			involvedObject: { kind: "Source", name: row.name, uid: "src_mock_" + row.name },
			traceUID: "trace_mock_run" + (state.events.length + 1),
			eventType: o.eventType || row.name,
			deliveryId: deliveryId,
			payload: payload,
			signature: o.signature || null,
			dispatched: [],
			prunedCount: 0,
			deliveredAt: isoNow(),
			createdAt: isoNow(),
		};
		recordEvent(ev);
		const plan = planDispatch(row, payload);
		const created = applyPlan(row, ev, plan, payload);
		ev.dispatched = created.map((c) => ({ task: c.name, capability: c.capability }));
		ev.prunedCount = plan.pruned.length;
		row.lastFiredAt = isoNow();
		const receipt = {
			eventId: ev.name, traceId: ev.traceUID, duplicate: false,
			tasks: created.map(c=>({taskId:c.name,capability:c.capability,version:c.version})),
			pruned: plan.pruned.map(p=>Object.assign(p.to ? {capability:p.to} : {}, {reason:p.reason,detail:p.detail}))
		};
		ev.receipt = clone(receipt);
		return receipt;
	}

	// 手动触发：真的往 state 里加事件 + 任务，页面上能看到链长出来。
	on("POST", /^\/v1\/namespaces\/([^/]+)\/sources\/([^/]+)\/fire$/, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		const row = findSource(ns, decodeURIComponent(ctx.m[2]));
		if (!row) return notFound("源 ");
		if (!row.enabled) {
			return badRequest("BadRequest", "源 " + row.name + " 已停用，不能触发");
		}
		const body = ctx.body || {};
		// cron / manual：config.payload 打底，本次输入一层覆盖（深合并不做）
		const base = row.kind === "webhook" ? {} : clone(row.config.payload || {});
		const out = deliver(row, {
			payload: Object.assign(base, body.payload || {}),
			eventType: row.name,
		});
		return json(out);
	});

	// 试运行：算出「会派发什么、会剪什么」，**不落库**（草稿也能先验一遍）。
	on("POST", /^\/v1\/namespaces\/([^/]+)\/sources\/dry-run$/, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		const body = ctx.body || {};
		const draft = body.source;
		if (!draft || typeof draft !== "object") {
			return badRequest("BadRequest", "body.source 必填（草稿源）");
		}
		const derr = validateWriteBody(draft, "Source", ns);
		if (derr) return derr;
		const spec = draft.spec || {};
		if (!Array.isArray(spec.targets)) {
			return badRequest("BadRequest", "source.spec.targets 必须是数组");
		}
		const verr = validateSourceSpec(spec);
		if (verr) return statusErr(400, verr.reason, verr.message);
		const row = {
			ns: ns,
			name: (draft.metadata && draft.metadata.name) || "draft",
			kind: spec.kind,
			enabled: spec.enabled !== false,
			config: clone(spec.config || {}),
			targets: clone(spec.targets || []),
		};
		return json(previewSource(row, sourcePayload(row, body)));
	});

	on("POST", /^\/v1\/namespaces\/([^/]+)\/sources\/([^/]+)\/dry-run$/, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		const row = findSource(ns, decodeURIComponent(ctx.m[2]));
		if (!row) return notFound("源 ");
		const body = ctx.body || {};
		return json(previewSource(row, sourcePayload(row, body)));
	});

	// 外部投递入口（webhook 免登录；token 定位 ns）。停用的源一律 404 ——
	// 不区分"token 不存在"与"源已停用"，免得用状态码差异探测 token。
	on("POST", /^\/v1\/hooks\/([^/]+)$/, async (ctx) => {
		const token = decodeURIComponent(ctx.m[1]);
		const row = state.sources.find(
			(s) => s.kind === "webhook" && s.config && s.config.token === token,
		);
		if (!row || !row.enabled) return notFound("源 ");
		if (row.config.secret) {
			const headers = ctx.headers || {};
			const signature = typeof headers.get === "function" ? headers.get("X-Anotify-Signature") : Object.entries(headers).find(([k])=>k.toLowerCase()==="x-anotify-signature")?.[1];
			if (!/^sha256=[0-9a-f]{64}$/.test(signature || "") || !window.crypto?.subtle) return statusErr(401,"Unauthorized","Invalid HMAC signature");
			const key = await window.crypto.subtle.importKey("raw",new TextEncoder().encode(row.config.secret),{name:"HMAC",hash:"SHA-256"},false,["sign"]);
			const signed = await window.crypto.subtle.sign("HMAC",key,new TextEncoder().encode(ctx.rawBody || ""));
			const expected = "sha256=" + [...new Uint8Array(signed)].map(x=>x.toString(16).padStart(2,"0")).join("");
			if (signature !== expected) return statusErr(401,"Unauthorized","Invalid HMAC signature");
		}
		if (ctx.rawBody && ctx.body === null) return badRequest("BadRequest","Invalid JSON");
		const body = ctx.body || {};
		const out = deliver(row, {
			payload: body.payload && typeof body.payload === "object" ? body.payload : {},
			eventType: body.type || row.name,
			deliveryId: body.id || null,
			signature: row.config.secret
				? { algorithm: "hmac-sha256", verified: true }
				: null,
		});
		return json(out);
	});

	const continuations = new Map();
	let continuationId = 0;
	function pageFacts(list, query, binding) {
		const raw = query.get("limit");
		const limit = raw == null ? 50 : Number(raw);
		if (!Number.isInteger(limit) || limit < 1 || limit > 100) {
			return { err: badRequest("invalid_limit", "limit must be 1..100") };
		}
		let offset = 0;
		const token = query.get("continue");
		if (token) {
			const cursor = continuations.get(token);
			if (!cursor || cursor.binding !== binding) {
				return { err: badRequest("invalid_continue", "Continuation does not match this namespace/filter") };
			}
			offset = cursor.offset;
		}
		const items = list.slice(offset, offset + limit);
		let next = "";
		if (offset + limit < list.length) {
			next = "page_" + (++continuationId).toString(36) + "_" + mockDigest(binding).slice(-12);
			continuations.set(next, { binding, offset: offset + limit });
		}
		return { items, metadata: { continue: next } };
	}

	// ===== 任务（ns 私有，只读 + 动作）=====
	const TASKS = /^\/v1\/namespaces\/([^/]+)\/tasks$/;
	const TASK_ONE = /^\/v1\/namespaces\/([^/]+)\/tasks\/([^/]+)$/;

	on("GET", TASKS, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		let list = state.tasks.filter((t) => t.ns === ns);
		const trace = ctx.query.get("traceUID");
		if (trace) list = list.filter((t) => t.traceUID === trace);
		const phase = ctx.query.get("phase");
		if (phase) list = list.filter((t) => t.phase === phase);
		const cap = ctx.query.get("capability");
		if (cap) list = list.filter((t) => t.capability === cap);
		const node = ctx.query.get("nodeName");
		if (node) list = list.filter((t) => t.nodeName === node);
		const limit = Number(ctx.query.get("limit") || 100);
		list = list
			.slice()
			.sort((a, b) => String(b.createdAt).localeCompare(String(a.createdAt)))
			.slice(0, limit);
		return json(listOf("TaskList", list.map(taskResource)));
	});

	// Task 只读：不能 POST / DELETE（只由派发器 / ingress 产生）。
	on("POST", TASKS, () => statusErr(405, "BadRequest", "Task 只读：只能由派发器/投递入口产生"));

	on("GET", TASK_ONE, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		const row = findTask(ns, decodeURIComponent(ctx.m[2]));
		if (!row) return notFound("任务 ");
		return json(taskResource(row));
	});

	on("GET", /^\/v1\/namespaces\/([^/]+)\/tasks\/([^/]+)\/logs$/, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		const row = findTask(ns, decodeURIComponent(ctx.m[2]));
		if (!row) return notFound("任务 ");
		const previous = ctx.query.get("previous") === "true";
		const history = (row.attempts || []).filter(a => a.attempt < row.attempt).sort((a, b) => b.attempt - a.attempt);
		const archives = state.taskLogs.filter(l => l.task === row.name).sort((a, b) => b.attempt - a.attempt);
		const attempt = previous ? (history[0]?.attempt || 0) : (archives[0]?.attempt || row.attempt);
		if (previous && !attempt) return json({ taskId: row.name, attempt: 0, lines: [], truncated: false });
		const log = archives.find(l => l.attempt === attempt);
		if (log && log.unavailable) return statusErr(503, log.unavailable, "Node archive is unavailable");
		if (!log) return statusErr(410, "logs_expired", "Task log archive is unavailable or expired");
		let lines = log ? clone(log.lines || []) : [];
		const since = ctx.query.get("sinceSeconds");
		if (since != null) {
			if (!Number.isFinite(Number(since)) || Number(since) < 0) return badRequest("invalid_since_seconds", "sinceSeconds must be nonnegative");
			lines = lines.filter(l => Date.parse(l.at) >= Date.now() - Number(since) * 1000);
		}
		const tail = ctx.query.get("tailLines");
		let truncated = !!(log && log.truncated);
		if (tail != null) {
			if (!Number.isInteger(Number(tail)) || Number(tail) < 0) return badRequest("invalid_tail_lines", "tailLines must be nonnegative");
			truncated = truncated || lines.length > Number(tail);
			lines = Number(tail) === 0 ? [] : lines.slice(-Number(tail));
		}
		return json({ taskId: row.name, attempt, lines, truncated });
	});

	on("GET", /^\/v1\/namespaces\/([^/]+)\/tasks\/([^/]+)\/emissions$/, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		const row = findTask(ns, decodeURIComponent(ctx.m[2]));
		if (!row) return notFound("任务 ");
		const unique = new Map();
		state.emissions.filter(e => e.ns === ns && e.task === row.name)
			.sort((a, b) => String(b.at).localeCompare(String(a.at)))
			.forEach(e => { if (!unique.has(e.eventId)) unique.set(e.eventId, e); });
		const paged = pageFacts([...unique.values()], ctx.query, JSON.stringify(["emissions", ns, row.name]));
		if (paged.err) return paged.err;
		return json({ metadata: paged.metadata, items: paged.items.map(e => {
			const fact = { eventId: e.eventId, to: e.to, version: e.version, taskId: e.taskId, pruned: clone(e.pruned) };
			if (e.id != null) fact.id = e.id;
			return fact;
		}) });
	});

	// A1: task manifest authorization comes before looking up managed bytes.
	on("GET", /^\/v1\/namespaces\/([^/]+)\/tasks\/([^/]+)\/artifacts\/([a-f0-9]{64})$/, ctx => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		const row = findTask(ns, decodeURIComponent(ctx.m[2]));
		const hash = ctx.m[3];
		const artifact = row?.result?.artifacts?.find(a => a.sha256 === hash);
		if (!artifact) return notFound("artifact reference ");
		const bytes = state.taskArtifacts.find(a => a.ns === ns && a.task === row.name && a.sha256 === hash);
		if (!bytes || bytes.expired) return statusErr(410, "artifact_expired", "Managed artifact expired; task manifest retained");
		const basename = artifact.path.split("/").pop().replace(/[\r\n]/g, "");
		const headers = { "Content-Type": "application/octet-stream", "Content-Disposition": 'attachment; filename="' + basename + '"' };
		if (typeof Response === "function") return new Response(bytes.content, { status: 200, headers });
		return { ok: true, status: 200, headers: { get: key => headers[key] || null }, text: async () => bytes.content };
	});

	// 取消 / 放弃：终态任务不能再动（409 Conflict）。
	const TASK_ACTIONS = {
		cancel: { phase: "cancelled", reason: "cancelled_by_user", message: "用户取消" },
		abandon: { phase: "abandoned", reason: "abandoned_by_user", message: "用户放弃" },
	};
	const TERMINAL = ["succeeded", "failed", "cancelled", "abandoned"];
	for (const action of Object.keys(TASK_ACTIONS)) {
		on(
			"POST",
			new RegExp("^/v1/namespaces/([^/]+)/tasks/([^/]+)/" + action + "$"),
			(ctx) => {
				const ns = decodeURIComponent(ctx.m[1]);
				const miss = nsMissing(ns);
				if (miss) return miss;
				const row = findTask(ns, decodeURIComponent(ctx.m[2]));
				if (!row) return notFound("任务 ");
				if (TERMINAL.indexOf(row.phase) >= 0) {
					return statusErr(409, "Conflict", "任务已到终态（" + row.phase + "），不能再" + action);
				}
				const spec = TASK_ACTIONS[action];
				row.phase = spec.phase;
				row.waitingReason = null;
				row.nextAttemptAt = null;
				row.finishedAt = isoNow();
				row.result = {
					summary: spec.message,
					artifacts: [],
					error: { reason: spec.reason, message: spec.message },
				};
				return json(taskResource(row));
			},
		);
	}

	// ===== 事件（ns 私有，只读资源 + 投递入口）=====
	on("GET", /^\/v1\/namespaces\/([^/]+)\/events$/, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		const limit = Number(ctx.query.get("limit") || 100);
		const list = state.events
			.filter((e) => e.ns === ns)
			.sort((a, b) => String(b.createdAt).localeCompare(String(a.createdAt)))
			.slice(0, limit);
		return json(listOf("EventList", list.map(eventResource)));
	});

	// 投递入口：body `{source, type?, id?, payload}`（Bearer key 鉴权，mock 不校验）。
	on("POST", /^\/v1\/namespaces\/([^/]+)\/events$/, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		const body = ctx.body || {};
		const row = findSource(ns, String(body.source || ""));
		if (!row) return notFound("源 ");
		const out = deliver(row, {
			payload: body.payload && typeof body.payload === "object" ? body.payload : {},
			eventType: body.type || row.name,
			deliveryId: body.id || null,
		});
		return json(out);
	});

	// ===== Trace（只读虚拟 kind：`{events,tasks,pruned}`）=====
	on("GET", /^\/v1\/namespaces\/([^/]+)\/traces\/([^/]+)$/, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		const doc = traceDoc(decodeURIComponent(ctx.m[2]), ns);
		if (!doc) return notFound("trace ");
		return json(doc);
	});

	// ===== edges / deployments（只读投影）=====
	on("GET", /^\/v1\/namespaces\/([^/]+)\/edges$/, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		return json(listOf("EdgeList", edgeItems(ns)));
	});

	on("GET", /^\/v1\/namespaces\/([^/]+)\/deployments$/, (ctx) => {
		const ns = decodeURIComponent(ctx.m[1]);
		const miss = nsMissing(ns);
		if (miss) return miss;
		return json(listOf("DeploymentList", deploymentItems(ns)));
	});

	// ===== 统计 / 事件流读模型（保留形状；namespace 必填）=====
	on("GET", /^\/v1\/stats$/, (ctx) => {
		const want = requireNamespace(ctx.query);
		if (want.err) return want.err;
		return json(statsDoc(want.ns));
	});

	on("GET", /^\/v1\/console\/events$/, (ctx) => {
		const want = requireNamespace(ctx.query);
		if (want.err) return want.err;
		if (!nsRow(want.ns)) return badRequest("namespace_required", "Select one namespace");
		const trace = ctx.query.get("traceId") || "";
		const source = ctx.query.get("source") || "";
		let list = state.events.filter(e => e.ns === want.ns);
		if (trace) list = list.filter(e => e.traceUID === trace);
		if (source) list = list.filter(e => e.involvedObject && e.involvedObject.name === source);
		list.sort((a, b) => String(b.createdAt).localeCompare(String(a.createdAt)) || b.name.localeCompare(a.name));
		const page = pageFacts(list, ctx.query, JSON.stringify(["events", want.ns, source, trace]));
		if (page.err) return page.err;
		return json(Object.assign(listOf("EventList", page.items.map(eventResource)), { metadata: page.metadata }));
	});

	// 能力模板（cluster 读模型，公共模板不承载 ns 私有数据）
	on("GET", /^\/v1\/capability-templates$/, () => json({ templates: mockTemplates() }));

	// ===== API Keys（保留域）=====
	on("GET", /^\/v1\/keys$/, () => json({ keys: clone(state.keys) }));

	on("POST", /^\/v1\/keys$/, (ctx) => {
		const b = ctx.body || {};
		const k = {
			id: "key_mock_" + (state.keys.length + 1),
			orgId: ORG,
			name: b.name || "未命名",
			scopes: b.scopes || ["send"],
			prefix: "ant_mock_" + (state.keys.length + 1),
			createdAt: now(),
			lastUsedAt: null,
			enabled: true,
			revoked: false,
		};
		state.keys.push(k);
		// 与真后端一致：`key` 是**明文字符串**（只返回一次），record 是记录本体。
		const plaintext = k.prefix + "_" + "x".repeat(28);
		return json({ ok: true, key: plaintext, record: clone(k) }, 200);
	});

	on("POST", /^\/v1\/keys\/([^/]+)\/revoke$/, (ctx) => {
		const k = state.keys.find((x) => x.id === ctx.m[1]);
		if (!k) return notFound("key ");
		k.revoked = true;
		k.enabled = false;
		return json({ ok: true });
	});

	on("DELETE", /^\/v1\/keys\/([^/]+)$/, (ctx) => {
		const i = state.keys.findIndex((x) => x.id === ctx.m[1]);
		if (i < 0) return notFound("key ");
		state.keys.splice(i, 1);
		return json({ deleted: ctx.m[1] });
	});

	// ===== 设备（推送订阅，保留域）=====
	on("GET", /^\/v1\/devices$/, () => json({ devices: clone(state.devices) }));
	on("PATCH", /^\/v1\/devices\/([^/]+)$/, ctx => {
		if (!hasSession()) return anonymous();
		const device = state.devices.find(d => d.id === decodeURIComponent(ctx.m[1]) && d.userId === ME.id);
		if (!device) return identityError(404, "Device not found");
		const patch = ctx.body;
		if (!pbObject(patch) || Object.keys(patch).some(k => !["enabled", "name", "eventScope", "tags"].includes(k)) ||
			(patch.enabled != null && typeof patch.enabled !== "boolean") ||
			(patch.name != null && typeof patch.name !== "string") ||
			(patch.eventScope != null && typeof patch.eventScope !== "string") ||
			(patch.tags != null && (!Array.isArray(patch.tags) || patch.tags.some(t => typeof t !== "string")))) return identityError(400, "Invalid device update");
		for (const key of ["enabled", "name", "eventScope", "tags"]) if (patch[key] != null) device[key] = clone(patch[key]);
		return json({ ok: true, device: clone(device) });
	});

	// ===== 安全 / passkey / 会话（保留域）=====
	on("GET", /^\/v1\/auth\/passkeys$/, () => json({ passkeys: [] }));
	on("GET", /^\/v1\/auth\/password$/, () => json({ hasPassword: !!state.auth.passwordHash, updatedAt: state.auth.passwordUpdatedAt }));
	on("POST", /^\/v1\/auth\/password$/, ctx => {
		if (state.auth.passwordHash && mockDigest(ctx.body?.currentPassword || "") !== state.auth.passwordHash) return badRequest("current_password_invalid", "Current password is incorrect");
		const value = ctx.body?.newPassword;
		if (typeof value !== "string" || value.length < 8 || value.length > 256 || !value.trim()) return badRequest("password_invalid", "New password must be 8–256 characters");
		state.auth.passwordHash = mockDigest(value); state.auth.passwordUpdatedAt = now(); return json({ ok: true });
	});
	on("DELETE", /^\/v1\/auth\/password$/, ctx => {
		if (mockDigest(ctx.body?.currentPassword || "") !== state.auth.passwordHash) return badRequest("current_password_invalid", "Current password is incorrect");
		return statusErr(409, "last_login_method", "Keep at least one login method: add a Passkey before removing the password");
	});

	on("GET", /^\/v1\/auth\/sessions$/, () =>
		json({
			sessions: [
				{
					id: "sess_mock_current",
					deviceName: "Chrome on Linux",
					username: ME.username,
					createdAt: now() - 3600,
					expiresAt: now() + 86400 * 7,
					lastSeen: now() - 60,
					current: true,
				},
			],
		}),
	);
	on("DELETE", /^\/v1\/auth\/sessions\/([^/]+)$/, () => json({ ok: true }));
	on("GET", /^\/v1\/auth\/recovery\/status$/, () => json({ unusedCount: 8, hasCodes: true }));
	on("POST", /^\/v1\/auth\/recovery\/regenerate$/, () =>
		json({
			ok: true,
			recoveryCodes: Array.from({ length: 10 }, (_, i) => "MOCK-CODE-" + (i + 1)),
		}),
	);

	// ===== 管理后台（保留域）=====
	on("GET", /^\/v1\/admin\/stats$/, () =>
		json({
			users: 3,
			orgs: 1,
			nodes: state.nodes.length,
			events: state.events.length,
			tasks: state.tasks.length,
			userCount: 3, adminCount: 1, disabledUsers: 0,
			messageCount: 0, todayMessages: 0,
			deviceCount: state.devices.length, enabledDevices: state.devices.filter(d => d.enabled).length,
			keyCount: state.keys.length, deliveredCount: 0, deliveryTotal: 0, topUsers: [],
		}),
	);
	on("GET", /^\/v1\/admin\/users$/, () =>
		json({
			users: [
				Object.assign(clone(ME), { role: "admin", lastSeenAt: now() - 60 }),
				{
					id: "usr_mock_b",
					username: "alice",
					displayName: "Alice",
					role: "member",
					createdAt: now() - 86400 * 5,
					lastSeenAt: now() - 3600,
				},
			],
		}),
	);
	on("GET", /^\/v1\/admin\/sessions$/, () =>
		json({
			sessions: [
				{
					id: "sess_mock_current",
					username: ME.username,
					deviceName: "Chrome on Linux",
					createdAt: now() - 3600,
					expiresAt: now() + 86400 * 7,
					lastSeen: now() - 60,
				},
			],
		}),
	);
	on("GET", /^\/v1\/admin\/messages$/, () => json({ messages: [] }));

	// ===== CLI 设备授权（保留域）=====
	on("POST", /^\/v1\/cli-auth\/sessions$/, () =>
		json(
			{
				id: "cas_mock_1",
				code: "MOCK-1234",
				url: "/cli-auth.html?code=MOCK-1234",
				expiresAt: now() + 600,
			},
			201,
		),
	);
	on("GET", /^\/v1\/cli-auth\/sessions\/by-code$/, () =>
		json({
			session: {
				id: "cas_mock_1",
				code: "MOCK-1234",
				deviceName: "anotify-cli on laptop",
				status: "pending",
				expiresAt: now() + 600,
			},
		}),
	);

	// ===== Passkey 设备注册会话（保留域）=====
	//
	// 状态机与真后端一致：pending → requested → approved / denied / expired。
	//
	// **刻意不伪造 WebAuthn**：mock 永远不返回 attestationOptions，所以页面会停在
	// "等待批准"而不是假装完成了一次真实的 passkey 创建 —— 预览里那一步（平台
	// 认证器）本来就跑不了，假装成功反而会让人以为绑定成功了。
	const ENROLL = "/v1/passkey-enroll/sessions";

	function enrollView(row) {
		const out = { sessionId: row.id, status: row.status, expiresAt: row.expiresAt };
		if (row.initiatorName) out.initiatorName = row.initiatorName;
		if (row.deviceHint) out.deviceHint = row.deviceHint;
		return out;
	}

	const findEnroll = (id) =>
		state.passkeyEnroll.find((x) => x.id === decodeURIComponent(id)) || null;

	on("POST", /^\/v1\/passkey-enroll\/sessions$/, (ctx) => {
		const b = ctx.body || {};
		const n = state.passkeyEnroll.length + 1;
		const row = {
			id: "pes_mock_" + n,
			secret: "pes_secret_mock_" + n,
			userCode: "MOCK-" + String(1000 + n),
			status: "pending",
			deviceName: b.deviceName || "anotify-cli",
			deviceHint: "",
			initiatorName: "anotify-cli (mock)",
			expiresAt: now() + 600,
		};
		state.passkeyEnroll.push(row);
		return json(
			{
				sessionId: row.id,
				secret: row.secret,
				userCode: row.userCode,
				authUrl: "/passkey-enroll.html?s=" + row.id,
				pollInterval: 2,
				expiresAt: row.expiresAt,
				kind: "passkey",
			},
			201,
		);
	});

	// 按 code 查（CLI 显示的四位码）—— 必须比 `{id}` 先注册，否则 "by-code" 会被当成 id
	on("GET", /^\/v1\/passkey-enroll\/sessions\/by-code$/, (ctx) => {
		const code = String(ctx.query.get("code") || "").toUpperCase();
		const row = state.passkeyEnroll.find((x) => String(x.userCode).toUpperCase() === code);
		if (!row) return notFound("注册会话 ");
		return json(enrollView(row));
	});

	on("GET", /^\/v1\/passkey-enroll\/sessions\/([^/]+)$/, (ctx) => {
		const row = findEnroll(ctx.m[1]);
		if (!row) return notFound("注册会话 ");
		return json(enrollView(row));
	});

	on("POST", /^\/v1\/passkey-enroll\/sessions\/([^/]+)\/request$/, (ctx) => {
		const row = findEnroll(ctx.m[1]);
		if (!row) return notFound("注册会话 ");
		if (row.status !== "pending") {
			return statusErr(409, "Conflict", "会话已经是 " + row.status + "，不能再请求");
		}
		const b = ctx.body || {};
		row.status = "requested";
		row.deviceHint = b.deviceHint || "未知设备";
		// secret 只发给**发起请求的设备**（真后端同此口径）
		return json(Object.assign(enrollView(row), { secret: row.secret }));
	});

	on("GET", /^\/v1\/passkey-enroll\/sessions\/([^/]+)\/poll$/, (ctx) => {
		const row = findEnroll(ctx.m[1]);
		const secret = ctx.query.get("secret");
		if (!row || !secret || secret !== row.secret) return notFound("注册会话 ");
		return json({ status: row.status, expiresAt: row.expiresAt });
	});

	on("POST", /^\/v1\/passkey-enroll\/sessions\/([^/]+)\/approve$/, (ctx) => {
		const row = findEnroll(ctx.m[1]);
		if (!row) return notFound("注册会话 ");
		if (row.status === "consumed" || row.status === "denied") {
			return statusErr(409, "Conflict", "会话已经是 " + row.status);
		}
		row.status = "approved";
		return json(enrollView(row));
	});

	on("POST", /^\/v1\/passkey-enroll\/sessions\/([^/]+)\/complete$/, (ctx) => {
		const row = findEnroll(ctx.m[1]);
		if (!row) return notFound("注册会话 ");
		row.status = "consumed";
		return json({ ok: true, status: row.status, name: ctx.query.get("name") || null });
	});

	// ===== passkey 注册流程（mock 模式不支持真实 WebAuthn）=====
	on("POST", /^\/v1\/auth\/register\/options$/, () =>
		badRequest("BadRequest", "mock 模式不支持真实 passkey 注册"),
	);
	on("POST", /^\/v1\/auth\/login\/options$/, () =>
		badRequest("BadRequest", "mock 模式不支持真实 passkey 登录"),
	);

	// ---------- 已删除的 v3 端点 ----------
	//
	// v4 明确移除：扁平资源路径、`PUT .../downstreams`、Edge / Deployment 写端点。
	// 这些必须回 **404 Status**（不是 501）：501 留给"mock 还没实现的新接口"，
	// 混在一起会让"这个端点已经不存在了"和"这个端点我还没写"看起来一样。
	//
	// 只在**没有路由命中**时才查这张表（所以 GET /v1/namespaces/x/edges 正常返回，
	// 而 POST 同一个路径落到这里 → 404）。
	const REMOVED = [
		{
			re: /^\/v1\/capabilities(\/.*)?$/,
			msg: "v3 扁平能力路径已删除：用 /v1/namespaces/{namespace}/capabilities",
		},
		{
			re: /^\/v1\/sources(\/.*)?$/,
			msg: "v3 扁平源路径已删除：用 /v1/namespaces/{namespace}/sources",
		},
		{
			re: /^\/v1\/tasks(\/.*)?$/,
			msg: "v3 扁平任务路径已删除：用 /v1/namespaces/{namespace}/tasks",
		},
		{
			re: /^\/v1\/events$/,
			msg: "v3 投递入口已删除：用 POST /v1/namespaces/{namespace}/events",
		},
		{
			re: /^\/v1\/playbooks(\/.*)?$/,
			msg: "v3 扁平安装包路径已删除：用 /v1/namespaces/{namespace}/playbooks",
		},
		{
			re: /^\/v1\/edges(\/.*)?$/,
			msg: "Edge 没有写端点；读走 GET /v1/namespaces/{namespace}/edges",
		},
		{
			re: /^\/v1\/deployments(\/.*)?$/,
			msg: "Deployment 只读；部署走 POST .../capabilities/{name}/deploy",
		},
		{
			re: /^\/v1\/traces(\/.*)?$/,
			msg: "trace 挂在命名空间下：GET /v1/namespaces/{namespace}/traces/{traceUID}",
		},
		{ re: /^\/v1\/orgs(\/.*)?$/, msg: "v3 组织端点已删除（v4 用 /v1/namespaces）" },
		{
			re: /^\/v1\/(namespaces\/[^/]+\/)?capabilities\/[^/]+\/downstreams$/,
			msg: "PUT .../downstreams 已删除：下游写在 spec.next 里（PATCH 能力）",
		},
		{
			re: /^\/v1\/namespaces\/[^/]+\/capabilities\/[^/]+\/artifact$/,
			msg: "artifact 已删除：脚本全文走 GET .../versions/{version}?includeBody=true",
		},
		{
			re: /^\/v1\/namespaces\/[^/]+\/capabilities\/[^/]+\/revisions(\/.*)?$/,
			msg: "revisions 已删除（v4 没有 CapabilityRevision）：用 .../versions",
		},
		{
			re: /^\/v1\/namespaces\/[^/]+\/edges$/,
			msg: "Edge 没有写端点（POST/DELETE 已删除）：边是 spec.next / spec.targets 的投影",
		},
		{
			re: /^\/v1\/namespaces\/[^/]+\/deployments(\/.*)?$/,
			msg: "Deployment 只读（POST/DELETE 已删除）：部署走 POST .../capabilities/{name}/deploy",
		},
	];

	// ---------- fetch 拦截 ----------
	//
	// 自己解析 URL（不用 `new URL`）：mock 也会在 node:vm 单测里加载，
	// 那里没有 URL / URLSearchParams 这两个全局。手写解析足够——
	// 我们只关心 pathname 与 query。
	const origFetch =
		typeof window.fetch === "function" ? window.fetch.bind(window) : null;

	function makeQuery(search) {
		const map = new Map();
		const s = String(search == null ? "" : search).replace(/^\?/, "");
		if (!s) return { get: () => null, has: () => false };
		for (const part of s.split("&")) {
			if (!part) continue;
			const i = part.indexOf("=");
			const k = decodeURIComponent((i < 0 ? part : part.slice(0, i)).replace(/\+/g, " "));
			const v = i < 0 ? "" : decodeURIComponent(part.slice(i + 1).replace(/\+/g, " "));
			if (!map.has(k)) map.set(k, v);
		}
		return {
			get: (k) => (map.has(k) ? map.get(k) : null),
			has: (k) => map.has(k),
		};
	}

	function parseTarget(input) {
		let href =
			typeof input === "string"
				? input
				: input && input.url
					? String(input.url)
					: String(input);
		// 绝对 URL → 只留 pathname + search（预览场景就是同源，不需要 origin）
		const abs = /^[a-z][a-z0-9+.-]*:\/\/[^/]*(\/.*)?$/i.exec(href);
		if (abs) href = abs[1] || "/";
		const qi = href.indexOf("?");
		return {
			pathname: qi >= 0 ? href.slice(0, qi) : href,
			query: makeQuery(qi >= 0 ? href.slice(qi + 1) : ""),
		};
	}

	const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

	window.fetch = async function (input, init) {
		const opts = init || {};
		const target = parseTarget(input);
		const pathOnly = target.pathname;
		if (pathOnly.indexOf("/v1/") !== 0) {
			if (!origFetch) throw new Error("mock: 没有可用的网络层（非 /v1/ 请求）");
			return origFetch(input, init);
		}

		const method = String(
			(opts.method || (input && input.method) || "GET").toUpperCase(),
		);

		// 解析 body（只认 JSON 字符串；其它形态在 mock 里用不到）
		let body = null;
		if (opts.body != null) {
			if (typeof opts.body === "string") {
				try {
					body = JSON.parse(opts.body);
				} catch (e) {
					body = null;
				}
			}
		}

		// 延迟：让 loading 态能被看到，也避免请求瞬间返回让页面闪烁得"不像真的"
		if (state.latencyMs > 0) await sleep(state.latencyMs + Math.random() * 180);

		// ?mockFail=1 强制看错误态（调试用；所有 /v1/ 请求一致）
		if (target.query.get("mockFail") === "1") {
			return statusErr(500, "BadRequest", "mock 强制失败（?mockFail=1）");
		}

		for (const r of routes) {
			if (r.method !== method) continue;
			const m = r.pattern.exec(pathOnly);
			if (!m) continue;
			try {
				return r.handler({ m: m, query: target.query, body: body, path: pathOnly, rawBody: opts.body, headers: opts.headers });
			} catch (e) {
				console.error("[mock] handler 抛错", r.pattern, e);
				return statusErr(500, "BadRequest", "mock handler error: " + e.message);
			}
		}

		// 已删除的 v3 端点：404（不是 501 —— 见 REMOVED 的注释）
		for (const gone of REMOVED) {
			if (!gone.re.test(pathOnly)) continue;
			return statusErr(404, "NotFound", gone.msg);
		}

		// 没实现的接口：明确说出来，而不是静默返回 {} 让页面莫名其妙地空。
		//
		// 这条 warn 的**措辞是门禁契约的一部分**：scripts/e2e 的 fe-preview 套件只在
		// console **文本**里搜 "mock 未实现" 这个子串，用它判定「页面调的接口 mock 有没有实现」。
		// 所以这里**不能加 "[mock] " 前缀** —— `"[mock] 未实现：…".includes("mock 未实现")` 是 false
		//（`] ` 卡在中间），断言会永远绿。
		console.warn("mock 未实现：" + method + " " + pathOnly);
		return statusErr(501, "BadRequest", "mock 未实现：" + method + " " + pathOnly);
	};

	// ---------- 预览提示条 ----------
	//
	// 让人一眼知道"这是假数据"，否则很容易误以为后端坏了。
	// 位置是**左下角**小卡片（早先是底部居中的黑影胶囊，会压在页面底部内容上）。
	// 颜色全走 web/tokens.css 的主题 token，不硬编码 rgba/hex。
	const BANNER_DISMISSED_KEY = "anotify-mock-banner";

	// localStorage 在隐私模式 / 被禁 / 部分 iframe 沙箱里读会抛异常：
	// 读不到就当"没关过"，写不进也只影响刷新后的复现，不能把整个 mock 拖挂。
	function bannerDismissed() {
		try {
			return window.localStorage.getItem(BANNER_DISMISSED_KEY) === "1";
		} catch (e) {
			return false;
		}
	}

	function mountBanner() {
		if (bannerDismissed()) return;
		if (!document || !document.createElement) return;
		const css = document.createElement("style");
		css.textContent = `
			#mock-banner {
				position: fixed;
				left: 12px;
				bottom: 12px;
				z-index: 999999;
				display: flex;
				align-items: center;
				gap: 8px;
				padding: 6px 10px;
				border-radius: var(--r-md);
				background: var(--panel-overlay);
				border: 1px solid var(--line);
				color: var(--warn);
				font-size: 11.5px;
				font-weight: 500;
				font-family: ui-sans-serif, system-ui, sans-serif;
				box-shadow: var(--shadow-2);
				backdrop-filter: blur(8px);
				cursor: pointer;
				user-select: none;
			}
			#mock-banner .dot {
				width: 6px; height: 6px; border-radius: 9999px;
				background: var(--warn);
				box-shadow: 0 0 8px var(--warn);
			}
			#mock-banner .x {
				margin-left: 4px; opacity: 0.6; font-size: 14px; line-height: 1;
			}
			#mock-banner:hover .x { opacity: 1; }
			@media (max-width: 639px) {
				#mock-banner { left: 8px; right: 8px; bottom: 8px; }
			}
		`;
		document.head.appendChild(css);

		const bar = document.createElement("div");
		bar.id = "mock-banner";
		bar.title = "点击关闭提示（数据仍是 mock 的）";
		const dot = document.createElement("span");
		dot.className = "dot";
		const txt = document.createElement("span");
		txt.textContent = "前端预览 · 数据均为 mock（接口未真正调用）";
		const x = document.createElement("span");
		x.className = "x";
		x.textContent = "✕";
		bar.appendChild(dot);
		bar.appendChild(txt);
		bar.appendChild(x);
		bar.addEventListener("click", () => {
			// 写标记再移除：下次加载（含刷新）就不再挂
			try {
				window.localStorage.setItem(BANNER_DISMISSED_KEY, "1");
			} catch (e) {
				/* localStorage 不可用：本次会话照样关掉就行 */
			}
			bar.remove();
		});

		const attach = () => document.body && document.body.appendChild(bar);
		if (document.readyState === "loading") {
			document.addEventListener("DOMContentLoaded", attach);
		} else {
			attach();
		}
	}

	// 挂到 window 上方便调试与演示：MockApi.state / MockApi.reset()
	window.MockApi = {
		state: state,
		reset: () => window.location.reload(),
		// 按名字取能力（最高版本；可带命名空间）
		capByName: capByName,
		// 升级演示控制（F2c 节点页消费）——两个都只改内存，刷 F5 就回初始值。
		noAgentBinary: (on) => {
			state.agentBinary = !(on === undefined ? true : on);
			return state.agentBinary;
		},
		failUpgrade: (code) => {
			state.upgradeFailCode = code || "";
			return state.upgradeFailCode;
		},
		// mock 响应延迟（毫秒）。0 = 立即返回（截图 / 批量断言用）。
		latency: (ms) => {
			state.latencyMs = Math.max(0, Number(ms) || 0);
			return state.latencyMs;
		},
	};
	mountBanner();

	console.info(
		"[mock] 前端预览模式（v4）：所有 /v1/* 请求由本地 mock 应答。\n" +
			"  - 数据存在 window.MockApi.state，可改\n" +
			"  - 任意接口加 ?mockFail=1 可看错误态\n" +
			"  - 去掉响应延迟：MockApi.latency(0)\n" +
			"  - 刷新页面恢复初始数据",
	);
})();
