/**
 * mock.js —— 纯前端预览用的 API 拦截层。
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
 * 数据是**内存里的可变状态**：新建/删除/触发都会真的改变它，
 * 所以能点着试完整流程。刷新页面回到初始状态。
 */
(function () {
	"use strict";

	// ---------- 基础工具 ----------
	const now = () => Math.floor(Date.now() / 1000);
	const uid = (p) => p + "_mock" + Math.random().toString(36).slice(2, 10);

	// traceSeq 给 trace id 用确定性的序号，不用随机。
	//
	// 为什么：mock 每次刷新都重新生成数据，随机 id 会让"分享/刷新 trace 页"
	// 永远 404（真实后端不会——trace_id 落库后就不变了）。
	// 用序号后，demo 里 trace_mock_t1 / t2 … 在刷新之间是稳定的，
	// trace.html?traceId=trace_mock_t5 可以反复打开。
	let traceSeq = 0;
	const nextTraceId = () => "trace_mock_t" + ++traceSeq;

	// taskSeq 同理：任务 id 也必须确定，否则「从 index 点开任务 → 刷新」必然 404。
	//
	// 任务详情页是 message.html?id=tsk_xxx（GET /v1/tasks/{id}）。种子任务的
	// 创建顺序在每次加载里是固定的，用递增序号就能得到跨刷新稳定的 id
	// （tsk_mock_1 / tsk_mock_2 …），深链可以反复打开。
	//
	// 运行期新建的任务（手动触发事件）排在种子之后，仍然唯一；它们本来
	// 就不在刷新后的数据里，序号是否延续不重要。
	let taskSeq = 0;
	const nextTaskId = () => "tsk_mock_" + ++taskSeq;
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
	const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

	// 演示用的固定组织/用户
	const ORG = "org_mock_demo";
	// 命名空间（设计见 docs/design/namespaces.md）：组织之下的隔离单位。
	// 既有演示数据默认落在 prod；staging / sandbox 由 initNamespaces 造。
	const NS = "ns_mock_prod";
	const ME = {
		id: "usr_mock_demo",
		username: "demo",
		displayName: "demo",
		createdAt: now() - 86400 * 30,
	};

	// ===== agent 自升级（CONTRACT-agent-upgrade.md §5.1）=====
	//
	// 中枢自己带的 agent 二进制版本。真实环境里来自 internal/version.String()
	// （`1.5.0+3fd5a59`）；演示数据里三台节点分别覆盖三种形态：
	//   laptop-worker 陈旧（1.3.0）/ old-laptop 从未上报（空）/ ops-box 已是最新。
	// 页面上的「目标版本」= 常量 AGENT_VERSION（GET /v1/nodes 顶层 targetAgentVersion）。
	const AGENT_VERSION = "1.5.0+3fd5a59";
	const AGENT_COMMIT = "3fd5a59";

	// ---------- 演示数据 ----------
	//
	// 刻意做得像真的：有扇出、有分叉的 out、有失败与剪枝，
	// 这样看页面时能看出各种状态长什么样，而不是一排"成功"。

	// 单出口：一个能力只有一个出口，内部名固定 "out"。
	//
	// 它是**路由键**，不是用户要表达的概念 —— 界面上只说「下游能力（候选）」。
	// 从来没配下游的能力就是终端能力：outputs 为 []。
	const SINGLE_OUT = "out";

	// normalizeOutputs 归一一次提交里的 outputs（只归一形状，不改语义）：
	//
	//   ["out"] / []     —— 表单提交的两种形状，原样收下（[] = 终端能力，不覆盖）
	//   逗号分隔的字符串  —— 手敲 curl 时最常见的写法
	//   **没给**但带了下游 —— 视为单出口（否则 links 一定过不了「出口必须在
	//                       outputs 里」这道校验，而用户其实已经表达了「有下游」）
	//
	// 多出口（历史数据 / 真需要第二种产出的能力）**原样保留**，不替用户合并成一个 ——
	// 那是破坏性改写。去重保序，空串丢掉。
	function normalizeOutputs(raw, hasLinks) {
		const given = raw !== undefined && raw !== null;
		const list = Array.isArray(raw)
			? raw.map((x) => String(x == null ? "" : x).trim())
			: typeof raw === "string"
				? raw.split(",").map((x) => x.trim())
				: [];
		const out = [];
		for (const name of list) {
			if (!name || out.includes(name)) continue;
			out.push(name);
		}
		if (!given && hasLinks) out.push(SINGLE_OUT);
		return out;
	}

	function cap(name, version, outputs, paramsSchema, source, displayName, balance) {
		// sticky 是「同一条能力尽量落到同一台机器」的提示，值是 params 里的**键名**
		// （internal/route/dispatch.go:stickySelectorValue）。演示数据里给两个能力开上，
		// 能力详情区才能同时看到「有值」和「空」两种形态。
		const STICKY = { "deploy-preview": "env", "analyze-pr": "worktree" };
		return {
			id: "cap_mock_" + name.replace(/\./g, "_") + "_v" + version,
			orgId: ORG,
			// 能力属于命名空间（I11）：同名能力在不同命名空间是两个不同定义
			namespaceId: NS,
			name: name,
			// 纯展示标签，空串 = 没取（前端回落到 name@version）
			displayName: displayName || "",
			version: version,
			outputs: outputs,
			// balance 是**能力版本上**的字段（契约 §3.3）：{出口: {mode}}，缺省 {}。
			// 出口名对用户不可见（单出口的内部名恒为 "out"），界面上只谈「下游」。
			// 缺省等价于每个出口都是 {mode:"priority"}；本期只实现 priority，
			// 其它模式在写入路径上就被 400 拦下（见 checkBalance），不静默降级。
			balance:
				balance && typeof balance === "object" && !Array.isArray(balance)
					? clone(balance)
					: {},
			paramsSchema: paramsSchema || { type: "object" },
			// lifecycle / sticky 是**能力定义的一部分**（参与 contentHash）。
			// 真实后端每条能力都有（未声明的时候是零值），所以 mock 也得给：
			// 不给的话详情区的「生命周期」永远是空的，看起来像功能没做。
			lifecycle: { expiresInSec: 3600, retryable: true, maxAttempts: 3 },
			sticky: STICKY[name] || "",
			source: source || "custom",
			contentHash: "hash" + Math.random().toString(16).slice(2, 10),
			createdAt: now() - 86400,
			kind: "pipe", // 会被 listCapabilities 重新推导
		};
	}

	const state = {
		// displayName 是纯展示标签（用户自己取的名字）。
		// 这里给几个能力填上，好让 demo 能看到"同名不同读法"的效果；
		// 留几个空的，用来验证没有显示名时回落到 name@version。
		capabilities: [
			// #110：github/cron/http/manual **不再是能力**。
			// 它们原来是不执行代码、不产生任务、也没地方存配置的空壳，
			// 唯一作用是给入口边当左端锚点。现在入口边左端直接是来源名
			// （fromIsSource: true），所以这里不该再有它们。
			//
			// 真实来源（GitHub webhook / 定时触发…）在「源」页面配置。
			// 中枢执行的终端能力：没有下游（outputs 空 = 终端能力，跑完就结束）。
			cap("notify.phone", 1, [], { type: "object" }, "builtin", "推送通知"),
			// 示例能力（新建表单的占位符里写的就是它）：一个出口 + 一段脚本。
			// 刻意**不接下游**：它同时是「跑完就结束」这条空态演示的样本。
			cap(
				"script.echo",
				1,
				["out"],
				{
					type: "object",
					properties: { message: { type: "string" } },
				},
				"custom",
				"回声（示例）",
			),
			// 用户能力：有参数、有下游（候选）
			cap(
				"analyze-pr",
				1,
				["out"],
				{
				type: "object",
				required: ["changedLines"],
				properties: {
					changedLines: { type: "integer", minimum: 0 },
					repo: { type: "string" },
				},
			}, "custom", "分析 PR"),
			cap(
				"apply-fix",
				1,
				["out"],
				{
					type: "object",
					properties: { scope: { type: "string", enum: ["lint", "test", "all"] } },
				},
				"custom",
				"应用修复",
			),
			// 一个出口、三条候选（运行期按 priority 选一条走）。
			cap(
				"deploy-preview",
				1,
				["out"],
				{
				type: "object",
				required: ["env"],
				properties: { env: { type: "string", enum: ["staging", "prod"] } },
			}, "custom", "部署预发"),
			cap(
				"report.send",
				1,
				["out"],
				{
				type: "object",
				required: ["channel", "title"],
				properties: {
					channel: { type: "string", enum: ["slack", "email"] },
					title: { type: "string" },
					urgent: { type: "boolean" },
					retries: { type: "integer", minimum: 0, maximum: 5 },
				},
			}, "custom", "发告警"),
			// 下面几个能力是为了让流向图有真实的"深度 + 分叉 + 汇聚"：
			// 一条链只有 3 个节点看不出布局和信息密度，太简单了。
			cap(
				"run-tests",
				1,
				["out"],
				{
				type: "object",
				properties: {
					suite: { type: "string", enum: ["unit", "integration", "e2e"] },
					shard: { type: "integer", minimum: 1 },
				},
			}, "custom", "跑测试"),
			cap(
				"lint-fix",
				1,
				["out"],
				{
				type: "object",
				properties: { scope: { type: "string" } },
			}, "custom", "清理构建缓存"),
			// 终端能力：没有下游（outputs 空），演示「跑完就结束」那一种形态。
			cap(
				"gen-changelog",
				1,
				[],
				{
				type: "object",
				properties: { since: { type: "string" } },
			}, "custom", "生成变更日志"),
			cap(
				"rollback",
				1,
				["out"],
				{
				type: "object",
				properties: { reason: { type: "string" } },
			}, "custom", "回滚"),
			// 出口声明了、但一条出边都没有 → 流向图上是个「没有下游」的灰点。
			// 这是最容易被忽略的配置遗漏，演示数据里必须留一个。
			cap(
				"alert.oncall",
				1,
				["out"],
				{
				type: "object",
				required: ["level"],
				properties: {
					level: { type: "string", enum: ["warn", "critical"] },
					page: { type: "boolean" },
				},
			}, "custom", "告警升级"),
			// 故意**没有任何节点声明**它（initNodes 里不引用）。
			//
			// 能力页要能演示最重要的那个运维信号：「这个能力定义好了，
			// 但没有任何机器能跑它」—— 没有这条演示数据，那个警告分支
			// 永远看不到，也就没人会去修。
			cap(
				"archive-logs",
				1,
				["out"],
				{
					type: "object",
					properties: { days: { type: "integer", minimum: 1 } },
				},
				"custom",
				"归档日志",
			),
		],
		edges: [],
		nodes: [],
		sources: [],
		events: [],
		tasks: [],
		// 每次 emit 的落库行（P1.6）：真后端是 emissions 表（迁移 22），
		// 按 (task_id, seq) 唯一。mock 里就是一个平表，由 seedEmissions() 播种。
		//
		// 一定要有种子数据：否则 trace 页的「产出（emits）」区永远只能看到
		// 空态，而空态看不出这个区该长什么样（产出徽章 / routed-pruned /
		// 可折叠的 data 一个都验不了）。
		emissions: [],
		keys: [],
		devices: [],
		// 产物与部署（CONTRACT-protocol §9）：真后端是两张表，mock 里
		// 只要够页面用。deployments 的行由 initDeployments() 造假数据。
		artifacts: [],
		deployments: [],
		// 运行期预算（契约 §2）：配置期不再判环，靠这两个兜底。
		//
		// 演示时可以原地改：window.MockApi.state.limits.maxHops = 0
		// → 下一次触发全部剪枝成 max_hops（budget 同理）。
		limits: { maxHops: 16, budget: 512 },
		// 中枢是否“带了 agent 二进制”。false = 服务端没带：
		//   · GET /v1/nodes 顶层 targetAgentVersion 为**空串**（控制台据此不给可点按钮）；
		//   · POST /v1/nodes/{id}/upgrade 回 501 agent_binary_unavailable。
		// 演示：window.MockApi.noAgentBinary() → 刷新节点页。
		agentBinary: true,
		// 让下一次 upgrade_ack 失败（need_root / bad_sha / download_failed / not_supported）。
		// 空串 = 正常成功（agentVersion 前进到目标版本）。演示：MockApi.failUpgrade("bad_sha")。
		upgradeFailCode: "",
	};

	// 辅助：按名字取能力（返回最高版本）
	function capByName(name) {
		const list = state.capabilities.filter((c) => c.name === name);
		if (!list.length) return null;
		return list.reduce((a, b) => (b.version > a.version ? b : a));
	}

	// ---------- 边的引用（P2.2）----------
	//
	// 与真后端**同形状**：边里只存**引用**（`name` 跟随 latest / `name@version` 钉死），
	// 入口边的左端是**来源名**。
	//
	// 为什么不能存能力 id：id 每一版都不一样，存了 id 之后能力发新版，
	// 原来那条边还指着旧 id —— 升级永远不生效。而「此刻解析到哪一版」是**派生量**，
	// 由 edgeView 现场算（和真后端一样，不落库），否则升级之后它对不上。
	//
	// 从**最后一个** '@' 拆：能力名里允许出现 '@'，而版本号一定是纯数字 ——
	// 与 Go 侧 store.SplitRef 同口径。
	function splitRef(ref) {
		const s = String(ref == null ? "" : ref).trim();
		const at = s.lastIndexOf("@");
		if (at <= 0 || at === s.length - 1) return { name: s, version: 0, pinned: false };
		const v = Number(s.slice(at + 1));
		if (!Number.isInteger(v) || v <= 0) return { name: s, version: 0, pinned: false };
		return { name: s.slice(0, at), version: v, pinned: true };
	}

	// resolveCapRef 把引用解析成能力定义；解析不到返回 null。
	//
	//   `name`     → 该名字的**最高版本**（= latest，升级自动生效）
	//   `name@ver` → 那一版（钉死）
	function resolveCapRef(ref, nsId) {
		const { name, version, pinned } = splitRef(ref);
		if (!name) return null;
		const pool = nsId
			? state.capabilities.filter((c) => !c.namespaceId || c.namespaceId === nsId)
			: state.capabilities;
		const list = pool.filter((c) => c.name === name);
		if (!list.length) return null;
		if (pinned) return list.find((c) => c.version === version) || null;
		return list.reduce((a, b) => (b.version > a.version ? b : a));
	}

	// capRef 是能力定义的引用写法（写边时用它，而不是 id）
	function capRef(c) {
		return c ? c.name + "@" + c.version : "";
	}

	// edgeView 是 GET /v1/edges 的一行：边本身 + 两端引用**此刻**解析到的能力 id。
	//
	// 入口边的 fromCapabilityId 恒为空串（左端是来源名，没有「哪一版」）；
	// 解析不到（能力被删）时也是空串 —— 一条坏边不该让整张表变成错误页。
	function edgeView(e) {
		const fromCap = e.fromIsSource ? null : resolveCapRef(e.fromRef, e.namespaceId);
		const toCap = resolveCapRef(e.toRef, e.namespaceId);
		return Object.assign(clone(e), {
			fromCapabilityId: fromCap ? fromCap.id : "",
			toCapabilityId: toCap ? toCap.id : "",
		});
	}

	// 配置期不再判环（契约 §2：靠 hop 预算 + trace 任务预算兜底）。
	//
	// 这里原来有个配置期环检测函数（与 store 那边同口径）：保存边时
	// 只要会成环就 400。契约 §2 明确删掉了静态环检测 —— `review → fix → review`
	// 是合法编排，真正的护栏是运行期的 max_hops / budget 两种剪枝（见 planDispatch）。

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
				outputs: [],
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
				outputs: [],
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
				outputs: [],
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
				outputs: [],
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
				outputs: [],
				timeoutSec: 120,
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
				outputs: [],
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
				outputs: [],
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
				outputs: [],
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
				outputs: [],
				timeoutSec: 3600,
			},
		];
	}

	// mock 的 digest：从正文算一个**稳定**的假 sha256。
	//
	// 同一份输入永远同一个值 —— 这样「改脚本 → digest 变了 → 节点上是本地变体」
	// 这条链路在演示里也讲得通；用随机值会让每次刷新都变成另一个 digest。
	function mockDigest(seed) {
		const s = String(seed || "");
		let h1 = 0x811c9dc5 >>> 0;
		let h2 = 0x1000193 >>> 0;
		for (let i = 0; i < s.length; i++) {
			const c = s.charCodeAt(i);
			h1 = ((h1 ^ c) * 0x01000193) >>> 0;
			h2 = (h2 + c * 31 + i * 7) >>> 0;
		}
		const hex = (n) => n.toString(16).padStart(8, "0");
		const parts = [
			h1,
			h2,
			(h1 ^ h2) >>> 0,
			(h1 + h2) >>> 0,
			(h1 * 3) >>> 0,
			(h2 * 7) >>> 0,
			(h1 ^ 0x5bf03635) >>> 0,
			(h2 ^ 0x9e3779b9) >>> 0,
		];
		return "sha256:" + parts.map(hex).join("");
	}

	// ===== kind 推导 =====
	//
	// 必须和后端 route.KindForCapability 一字不差，否则 mock 里看到的
	// 标签和真实产品对不上，等于在演示里撒谎。
	//
	// 后端真身（internal/route/kind.go:68）：
	//
	//	if 是内置能力:
	//	    Central(notify.phone)  → end      // 中枢执行，没有节点
	//	    Origin(github/cron/...) → origin   // 起点，不产生任务行
	//	return DeriveKind(outputs, declared)   // 其余一律按 outputs 推导
	//
	// 关键：`origin` 只给那 4 个内置起点（OriginCapability 的调用只有
	// github / cron / http / manual），**不是**"所有 builtin 都算起点"。
	// 之前这里写成 `c.source === "builtin" → origin`，导致 report.send
	// 这种用户能力因为 source 标记而被误标成 origin —— 它有一条出边
	// （→ alert.oncall）、也有节点声明，按规则该是 pipe。

	// 内置起点的名字（后端 store.BuiltinCapabilities 里 OriginCapability 的四个）
	const ORIGIN_NAMES = ["github", "cron", "http", "manual"];
	// 中枢执行的终端能力（BuiltinCapability.Central）
	const CENTRAL_NAMES = ["notify.phone"];

	function kindOf(c) {
		if (!c) return "origin";
		if (CENTRAL_NAMES.includes(c.name)) return "end";
		if (ORIGIN_NAMES.includes(c.name)) return "origin";
		// 其余按 DeriveKind：有节点声明 + 有出口 → pipe，否则 end/origin
		const declared = state.nodes.some((n) =>
			(n.capabilities || []).some((x) => x.id === c.id),
		);
		if (!declared) return "origin";
		return (c.outputs || []).some((o) => o) ? "pipe" : "end";
	}

	// ---------- 初始边 ----------
	(function initEdges() {
		// fromSource: 左端是**来源名**（入口边，#110），不是能力 id。
		// 旧模型靠一个叫 github@1 的空壳能力当锚点，那个空壳已经不存在了。
		// 右端是**引用**（P2.2）：演示数据一律钉死 `name@version`，
		// 让「同一张图、不同版本」在升级演示里看得见。
		// literals 是这条边的**常量覆盖**（P0-c，设计 §3.3）：默认 {}（= 没有覆盖）。
		// 两种引用形态（设计 §3.2），演示数据里**两种都要有**：
		//   PIN("x")    → `x@1` 钉死那一版（升级不动线）
		//   FOLLOW("x") → `x`   跟随 latest（升级自动生效）
		// 只有钉死引用的话，「当前 → name@version」这个提示在出口页永远不会出现，
		// 也就没人能看出这两种写法在界面上长得不一样。
		const PIN = (name) => capRef(capByName(name));
		const FOLLOW = (name) => name;
		// 工厂收的是**引用**（已经算好的字符串），不是能力名：
		// 这样调用点能自己决定这一端钉死还是跟随。
		const fromSource = (source, eventType, toRef, literals) => ({
			id: uid("edg"),
			orgId: ORG,
			fromIsSource: true,
			fromRef: source,
			// 入口边的 fromPort 是**事件类型**（外部系统决定，后端允许 '*'），
			// 与能力的出口无关 —— 所以它不参与「单出口」归一。
			fromPort: eventType,
			toRef,
			literals: literals || {},
			priority: 0,
			createdAt: now() - 3600,
		});
		const from = (fromRef, output, toRef, literals) => ({
			id: uid("edg"),
			orgId: ORG,
			fromRef,
			fromPort: output,
			toRef,
			literals: literals || {},
			priority: 0,
			createdAt: now() - 3600,
		});
		state.edges = [
			// 源也可以扇出：一个 PR 事件同时派发分析和通知两条链。
			// 这两条链互不等待（不做 join），各走各的。
			fromSource("github", "pull_request.opened", PIN("analyze-pr")),
			// 带常量覆盖的一条：告警一律标 urgent（常量在配置里，脚本不用管）。
			fromSource("github", "pull_request.opened", PIN("report.send"), { urgent: true }),
			fromSource("github", "pull_request.synchronize", PIN("analyze-pr")),
			fromSource("cron", "nightly", PIN("analyze-pr")),
			fromSource("http", "webhook.incoming", PIN("deploy-preview")),
			fromSource("manual", "manual.run", PIN("deploy-preview")),
			from(PIN("analyze-pr"), "out", PIN("lint-fix")),
			from(PIN("analyze-pr"), "out", PIN("run-tests")),

			// 两个分支各自往下走一步，然后在 deploy-preview 汇聚 ——
			// 汇聚是布局最容易画歪的形状（菱形），值得在演示数据里出现。
			from(PIN("lint-fix"), "out", PIN("run-tests")),
			from(PIN("lint-fix"), "out", PIN("report.send")),
			from(PIN("run-tests"), "out", PIN("deploy-preview")),

			// ===== 同一个出口、多条下游 = 候选 =====
			//
			// 单出口模型（内部名 "out"，对用户不可见）：拿 (能力, "out") 查表返回 N 行，
			// 运行期按 priority 升序取最小的一组、组内轮盘选**一条**（契约 §1）。
			// 界面上只说「下游能力（候选）」，候选集合正是这个形状。
			from(PIN("analyze-pr"), "out", PIN("run-tests")),
			from(PIN("apply-fix"), "out", PIN("deploy-preview")),

			// 测试挂了先尝试自动修，修完再测一次 —— 这是本仓库真实存在的
			// 环（run-tests → apply-fix → run-tests）。流向图必须画出环路，
			// 而"全是 DAG"的演示数据会让人以为这功能只能处理简单情况。
			from(PIN("run-tests"), "out", PIN("apply-fix")),

			// 部署之后：生成变更日志 / 回滚（同一出口的两条候选）
			//
			// 到 gen-changelog 这条路故意写成**跟随 latest**（FOLLOW）：
			// 能力发新版后它会自动走新版。出口页上会多一句「当前 → gen-changelog@1」，
			// 而钉死引用那一堆没有 —— 两种引用形态的区别必须在界面上看得见。
			from(PIN("deploy-preview"), "out", FOLLOW("gen-changelog")),
			from(PIN("deploy-preview"), "out", PIN("rollback")),

			// 一条真实存在的环：改了再测，测完再改。
			// 留着它是刻意的：流向图必须能画出环路，而"全是 DAG"的演示数据
			// 会让人以为这功能只能处理简单情况。
			from(PIN("apply-fix"), "out", PIN("run-tests")),

			// 兜底告警：report.send 和 rollback 都能触发值班
			from(PIN("rollback"), "out", PIN("alert.oncall")),
			from(PIN("report.send"), "out", PIN("alert.oncall")),
		];
	})();

	// ---------- 初始节点 ----------
	//
	// 三台机器按"谁干什么"分工，而不是随便挂几个能力。
	// 这件事不只是好看：**kind 是由"有没有节点声明"推导出来的**，一个能力
	// 没有节点声明就是 origin（表示"没人实现它，只有出边"）。如果 mock 里
	// 中间能力都没人声明，整张图会显示成一堆孤立的起点 —— 看起来像配置错误，
	// 其实只是演示数据没铺完。
	//
	// 所以这里让每个"应该有实现"的能力都被某台机器声明。
	(function initNodes() {
		const capRef = (n) => {
			const c = capByName(n);
			// 执行方式（run_spec）：节点侧 node.yaml 里声明的东西。
			// 中枢**只存不执行**（I1：中枢不是执行者），这里造数据也一样：
			// 只是给页面显示"这台机器上这个能力跑的什么命令"。
			const run = {
				runner: "bash",
				command: "./runners/" + n.replace(/\./g, "-") + ".sh",
				timeoutSec: 600,
				concurrency: 1,
				env: {},
			};
			// displayName 也带上：节点页要显示用户取的中文名。
			// kind 不在这里算 —— 它依赖"有没有节点声明"，而这段代码正在
			// 构造 state.nodes，算出来会是半成品。放到请求时算（nodeView）。
			return {
				id: c.id,
				name: c.name,
				version: c.version,
				run: run,
				displayName: c.displayName || "",
			};
		};

		state.nodes = [
			{
				// 主工作站：分析、修复、告警
				id: "nd_mock_laptop",
				orgId: ORG,
				name: "laptop-worker",
				status: "online",
				lastSeenAt: now() - 3,
				createdAt: now() - 86400,
				// 契约里 Node 一定有这两个硬顶；缺了页面会显示 undefined
				maxInstances: 20,
				maxConcurrency: 8,
				tags: ["local", "workstation"],
				// displayName 是纯展示标签（用户自己取的名字）。
		// 这里给几个能力填上，好让 demo 能看到"同名不同读法"的效果；
		// 留几个空的，用来验证没有显示名时回落到 name@version。
		capabilities: [
					"analyze-pr",
					"lint-fix",
					"apply-fix",
					"script.echo",
					"report.send",
					"alert.oncall",
					// 部署预发也放这台：源「GitHub PR」用它演示变量，
					// 挂在离线节点上会让那条示例每次都 pruned/no_node，
					// 用户点"立即运行"只能看到失败。
					"deploy-preview",
				].map(capRef),
				instances: [
					{ id: "nins_mock_0", index: 0, concurrency: 4, busy: 1 },
				],
				// agent 自升级（契约 §5.1）：陈旧的已知版本 + 从未尝试过升级。
				agentVersion: "1.3.0+9c1f2ab",
				agentCommit: "9c1f2ab",
				agentUpgrade: { status: "", targetVersion: "", error: "", at: 0 },
			},
			{
				// CI 机器：测试、部署、生成变更日志。离线是故意的（演示"节点掉线
				// 时任务保持 pending"这条路径）。
				id: "nd_mock_ci",
				orgId: ORG,
				name: "ci-runner",
				status: "offline",
				lastSeenAt: now() - 7200,
				createdAt: now() - 86400 * 3,
				// 契约里 Node 一定有这两个硬顶；缺了页面会显示 undefined
				maxInstances: 10,
				maxConcurrency: 4,
				tags: ["ci"],
				// displayName 是纯展示标签（用户自己取的名字）。
		// 这里给几个能力填上，好让 demo 能看到"同名不同读法"的效果；
		// 留几个空的，用来验证没有显示名时回落到 name@version。
		capabilities: ["run-tests", "gen-changelog"].map(capRef),
				instances: [{ id: "nins_mock_1", index: 0, concurrency: 2, busy: 0 }],
				// 离线 + 从未上报过版本：点升级会拿到 409 node_offline。
				agentVersion: "",
				agentCommit: "",
				agentUpgrade: { status: "", targetVersion: "", error: "", at: 0 },
			},
			{
				// 运维机：回滚。单独一台，方便演示"能力分散在不同机器上"，
				// 也让"按节点筛选"有东西可筛。
				id: "nd_mock_ops",
				orgId: ORG,
				name: "ops-box",
				status: "online",
				lastSeenAt: now() - 12,
				createdAt: now() - 86400 * 7,
				// 契约里 Node 一定有这两个硬顶；缺了页面会显示 undefined
				maxInstances: 5,
				maxConcurrency: 2,
				tags: ["ops"],
				// displayName 是纯展示标签（用户自己取的名字）。
		// 这里给几个能力填上，好让 demo 能看到"同名不同读法"的效果；
		// 留几个空的，用来验证没有显示名时回落到 name@version。
		capabilities: ["rollback"].map(capRef),
				instances: [{ id: "nins_mock_2", index: 0, concurrency: 1, busy: 0 }],
				// 已是最新：版本 == 目标版本 → 按钮直接显示「已是最新」（契约 §6）。
				agentVersion: AGENT_VERSION,
				agentCommit: AGENT_COMMIT,
				// ok 是上一次升级的结果：它会随刷新一起回来（已落库），
				// 所以刷新后仍然是「已是最新」而不是又变回「升级」。
				agentUpgrade: {
					status: "ok",
					targetVersion: AGENT_VERSION,
					error: "",
					at: now() - 3600,
				},
			},
			{
				// 老 agent：从来没在 hello 里上报过 agent_version（契约 §3）——
				// 页面显示「未知（老版本）」。它的升级永远失败在 need_root：
				// 演示「ack 失败落库、刷新后还能看到为什么没成」这条最关键的回路。
				id: "nd_mock_legacy",
				orgId: ORG,
				name: "old-laptop",
				status: "online",
				lastSeenAt: now() - 40,
				createdAt: now() - 86400 * 14,
				maxInstances: 4,
				maxConcurrency: 2,
				tags: ["legacy"],
				// 没声明任何能力：老 agent 还没部署过产物，这是真实形态。
				capabilities: [],
				instances: [{ id: "nins_mock_3", index: 0, concurrency: 1, busy: 0 }],
				agentVersion: "",
				agentCommit: "",
				agentUpgrade: {
					status: "failed",
					targetVersion: AGENT_VERSION,
					error: "need_root",
					at: now() - 1800,
				},
			},
		];
	})();

	// ---------- 初始产物（脚本）----------
	//
	// 用户的原话是「touchnow 我看不到具体什么脚本啊」——能力详情区现在会展示
	// 产物里的文件正文，那演示数据里就必须真的有脚本，否则每个能力都写
	// 「还没有产物」，那个功能在预览里等于不存在。
	//
	// 只给两条：
	//   analyze-pr    有脚本 + 有下游（完整形态：能看出它会 emit 什么）
	//   gen-changelog 有脚本 + **没有下游**（terminal 的那个形态，
	//                 正好演示详情里的「跑完就结束」空态）
	//   script.echo   有脚本 + 一个 message 参数（新建表单占位符里的那个例子）
	(function initArtifactScripts() {
		const scripts = {
			"analyze-pr": {
				requires: ["jq"],
				run: {
					entrypoint: "run.sh",
					timeoutSec: 600,
					workDir: "",
					env: {},
					files: {
						"run.sh": [
							"#!/bin/bash",
							"set -euo pipefail",
							'cd "$ANOTIFY_WORKDIR"',
							"",
							'lines="${ANOTIFY_PARAM_CHANGEDLINES:-0}"',
							'echo "changed lines: $lines"',
							"if [ \"$lines\" -gt 200 ]; then",
								"anotify-agent emit --data '{\"scope\":\"all\"}'",
							"else",
								'anotify-agent emit --data "{}"',
							"fi",
							"",
						].join("\n"),
						"lib/emit.sh": [
							"#!/bin/bash",
							"# emit 的薄封装：一个能力只有一个出口，--port 不用写（agent 自己填 out）",
							"set -euo pipefail",
							'exec "$ANOTIFY_AGENT_BIN" emit --data "${1:-{}}"',
						].join("\n"),
					},
				},
			},
			"gen-changelog": {
				requires: [],
				run: {
					entrypoint: "run.sh",
					timeoutSec: 300,
					workDir: "",
					env: {},
					files: {
						"run.sh": [
							"#!/bin/bash",
							"set -euo pipefail",
							'cd "$ANOTIFY_WORKDIR"',
							"",
							"# 没有下游 = 不发 emit（终端能力，合法）",
							'git log --oneline -20 > CHANGELOG.txt',
							'echo "已写入 CHANGELOG.txt"',
						].join("\n"),
					},
				},
			},
			// 示例能力的产物：读 message 参数、打印一份，不发 emit（没有下游）。
			"script.echo": {
				requires: [],
				run: {
					entrypoint: "run.sh",
					timeoutSec: 60,
					workDir: "",
					env: {},
					files: {
						"run.sh": [
							"#!/bin/bash",
							"set -euo pipefail",
							'cd "$ANOTIFY_WORKDIR"',
							"",
							'msg="${ANOTIFY_PARAM_MESSAGE:-hello}"',
							'echo "$msg" | tee echo.txt',
						].join("\n"),
					},
				},
			},
		};
		for (const [name, spec] of Object.entries(scripts)) {
			const c = state.capabilities.find((x) => x.name === name);
			if (!c) continue;
			c.requires = spec.requires.slice();
			c.run = spec.run;
			c.digest = mockDigest(JSON.stringify({ name: c.name, version: c.version, run: c.run }));
			state.artifacts.push({
				id: uid("art"),
				namespaceId: c.namespaceId,
				capabilityId: c.id,
				digest: c.digest,
				createdAt: now() - 3600,
			});
		}
	})();

	// ---------- 初始部署 ----------
	//
	// 造出四种状态各来几行，好让「能力页的部署列」与「节点页的已缓存产物」
	// 在演示里能看到真形态（而不是一排「未部署」）：
	//   applied 绿 / pending 黄 / failed 红（带 reason）/ local 黄（本地变体）
	(function initDeployments() {
		state.deployments = [];
		const byId = (id) => state.nodes.find((n) => n.id === id) || null;
		const seed = (nodeId, name, depState, reason) => {
			const node = byId(nodeId);
			const c = capByName(name);
			if (!node || !c) return;
			const digest = mockDigest(name + "@" + c.version);
			state.deployments.push({
				id: uid("dep"),
				namespaceId: NS,
				nodeId: node.id,
				artifactId: "art_" + digest.slice(7, 15),
				capabilityId: c.id,
				digest,
				state: depState,
				reason: reason || "",
				createdAt: now() - 3600,
				updatedAt: now() - 600,
			});
		};
		seed("nd_mock_laptop", "analyze-pr", "applied");
		seed("nd_mock_laptop", "lint-fix", "applied");
		// 手改过产物 → 中枢永不覆盖，只能标黄
		seed("nd_mock_laptop", "apply-fix", "local");
		// 常见失败：机器上没装 curl（reason 是机器可读短码 + 说明）
		seed("nd_mock_laptop", "report.send", "failed", "missing_requirement:curl");
		// ci-runner 离线 → 行照样创建，等它上线再由重连对账补推
		seed("nd_mock_ci", "run-tests", "pending");
		seed("nd_mock_ops", "rollback", "applied");
	})();

	// ---------- 初始源 ----------
	(function initSources() {
		const analyze = capByName("analyze-pr");
		const deploy = capByName("deploy-preview");
		const report = capByName("report.send");
		// 三个源刻意覆盖**变量用法的三种典型形态**，让变量功能打开就能看到：
		//
		//   每晚体检（cron）    变量全是固定值 —— 定时任务没有外部 payload
		//   GitHub PR（webhook）变量从请求体取 —— 真实场景的主力
		//   发布审批（webhook）一条边用变量、另一条边用字面量 —— 混用也很常见
		//
		// 如果这里全写成字面量，用户点开任何一个源都看不到变量长什么样，
		// 新功能等于没演示。
		state.sources = [
			{
				id: "src_mock_nightly",
				orgId: ORG,
				name: "每晚体检",
				kind: "cron",
				config: {
					expr: "0 3 * * *",
					tz: "Local",
				},
				enabled: true,
				lastFiredAt: now() - 3600 * 9,
				nextFireAt: now() + 3600 * 7,
				createdAt: now() - 86400 * 2,
				targets: [
					{
						capabilityId: analyze.id,
						params: { changedLines: 120, repo: "anotify" },
					},
				],
			},
			{
				id: "src_mock_hook",
				orgId: ORG,
				name: "GitHub PR",
				kind: "webhook",
				config: {
					token: "src_mocktoken_abc123",
					// 变量把 GitHub 那套深层路径收敛成几个短名字。
				},
				enabled: true,
				lastFiredAt: now() - 600,
				nextFireAt: null,
				createdAt: now() - 86400,
				targets: [
					{
						capabilityId: analyze.id,
						// 别名表已删除：参数直接写完整 payload 路径
						params: { changedLines: "{{pull_request.additions}}", repo: "{{repository.name}}" },
					},
					{ capabilityId: deploy.id, params: { env: "staging" } },
				],
			},
			{
				id: "src_mock_report",
				orgId: ORG,
				name: "发布审批",
				kind: "manual",
				config: {
					// 手动触发没有外部 payload，全用字面量。
				},
				enabled: false,
				lastFiredAt: null,
				nextFireAt: null,
				createdAt: now() - 3600 * 5,
				// 一条边用变量、一条边用字面量：混用是允许的，
				// 也正是真实配置最常见的样子（有些值随事件变，有些写死）。
				targets: [
					{
						capabilityId: report.id,
						params: {
							channel: "slack",
							title: "v1.4.0 发布",
							urgent: false,
							retries: 2,
						},
					},
				],
			},
		];
	})();

	// ---------- 初始事件与任务 ----------
	(function initEventsTasks() {
		// 演示数据刻意做得"有形状"，而不是一串整齐的 3 秒成功：
		//
		//   - 并行扇出：analyze-pr 之后同时跑 apply-fix 和 deploy-preview
		//   - 耗时差异：有 0.4s 的快任务，也有 12s 的慢任务
		//   - 三种终态：succeeded / failed / running（进行中，宽度算到"现在"）
		//   - 一个 pending（还没开始，只能画左边界细线）
		//
		// 全是"3 秒成功"的链会让瀑布图看起来像一堆等长方块，
		// 既看不出时序也验证不了 5 种边界情况。

		const mkTask = (o) => ({
			id: nextTaskId(),
			orgId: ORG,
			traceId: o.traceId,
			parentTaskId: o.parent || null,
			path: o.path || [],
			capabilityId: o.cap.id,
			capability: o.cap.name,
			version: o.cap.version,
			params: o.params || { mock: true },
			status: o.status || "succeeded",
			nodeId: o.nodeId !== undefined ? o.nodeId : state.nodes[0].id,
			instanceId: o.nodeId === null ? null : "nins_mock_0",
			leaseUntil: o.finishedAt ? null : o.createdAt + 30,
			attempts: o.attempts || 1,
			out: o.out || null,
			reported: o.status === "succeeded" || o.status === "failed",
			summary: o.summary || "",
			artifacts: [],
			error: o.error || "",
			contentHash: o.cap.contentHash,
			capHash: o.cap.contentHash,
			graphId: "grf_mock",
			createdAt: o.createdAt,
			startedAt: o.startedAt !== undefined ? o.startedAt : o.createdAt,
			finishedAt: o.finishedAt !== undefined ? o.finishedAt : null,
			notBefore: null,
		});

		// t 是这几条 demo 的**时间游标**，各段末尾用 `t += N` 往后推。
		//
		// 注意累加总量不能超过起点：曾经从 -180 起步、累加 210，
		// 结果最后一条落在 **+30s（未来）**，事件流里显示成"刚刚"，
		// 而且把真正想让人第一眼看到的那条挤到了下面。留着这个注释
		// 免得下次又往上加步长。
		let t = now() - 420;

		// ---- 1) 一条完整的修复链：analyze → lint-fix → run-tests → 部署 ----
		//
		// 这里**必须**符合真实派发语义：每个任务只回报一个 out
		// （store.Task.Out 是单值，不变量 I5），所以每个父任务
		// **最多只有一个子任务**。真实的执行路径永远是一条链，不是树。
		//
		// 想表现"并行"不能靠让一个父任务扇出 —— 那在模型里不存在。
		// 并行要靠两条独立的 trace 各自在跑（见下面第 3 条）。
		{
			const traceId = nextTraceId();
			state.events.push({
				id: uid("evt"), orgId: ORG, traceId,
				source: "GitHub PR", type: "pull_request.opened",
				payload: {
					pull_request: {
						number: 842, title: "修个错字", additions: 12, deletions: 3,
						user: { login: "alice" }, head: { ref: "fix/typo" }, base: { ref: "main" },
					},
					repository: { name: "anotify", full_name: "zzjcool/anotify" },
					action: "opened",
				}, createdAt: t,
			});
			const analyze = capByName("analyze-pr");
			const lintFix = capByName("lint-fix");
			const tests = capByName("run-tests");
			const deploy = capByName("deploy-preview");
			const report = capByName("report.send");

			// 逐跳串起来：每一跳走的都是同一个出口，下一跳由候选决定
			let prev = mkTask({
				traceId, cap: analyze, createdAt: t,
				startedAt: t + 1, finishedAt: t + 5,
				out: "out", summary: "发现 3 处 lint 问题",
			});
			state.tasks.push(prev);

			const steps = [
				{
					cap: lintFix, out: "out", summary: "自动修复 2 处，1 处需人工",
					start: t + 5, end: t + 6.4, node: state.nodes[0].id,
				},
				{
					cap: tests, out: "out", summary: "12 个用例全部通过",
					// 测试常跑在 CI 机器上，顺便让"按节点筛选"在演示数据里看得出差异
					start: t + 6.4, end: t + 18.6, node: state.nodes[1].id,
				},
				{
					cap: deploy, out: "out", summary: "预览环境已部署",
					start: t + 18.6, end: t + 21.2, node: state.nodes[1].id,
					params: { env: "staging" },
				},
			];

			for (const st of steps) {
				const child = mkTask({
					traceId, cap: st.cap, parent: prev.id,
					path: [analyze.id, st.cap.id],
					nodeId: st.node,
					createdAt: st.start, startedAt: st.start, finishedAt: st.end,
					out: st.out, summary: st.summary, params: st.params,
				});
				state.tasks.push(child);
				prev = child;
			}

			// 最后一跳失败，带 error —— 让链尾出现一个红节点
			state.tasks.push(mkTask({
				traceId, cap: report, parent: prev.id,
				path: [analyze.id, deploy.id, report.id],
				createdAt: t + 21.2,
				startedAt: t + 21.3, finishedAt: t + 21.7,
				params: { channel: "slack", title: "部署通知" },
				status: "failed", error: "slack: 403 invalid token",
			}));
			t += 40;
		}

		// ---- 1b) 主线：一条真正复杂的 trace（森林，不是单链）----
		//
		// 按 wiki 06「森林，不是树」的形状造：
		//
		//   根事件 pull_request.opened
		//     ├─ analyze-pr ─┬─ lint-fix ── run-tests(failed) ── apply-fix ── run-tests
		//     │              └─ run-tests(passed) ──────────────┐（analyze 的同一出口有 2 条候选，这次都走了）
		//     │                            └→ deploy-preview ─┬─ gen-changelog
		//     │                                                └─ alert.oncall
		//     └─ report.send（源扇出：同一个事件同时派发通知链）
		//
		// 同时含三种真实形状，一张图能验证完：
		//   ① 源扇出     —— 一个事件派两条互不等待的链
		//   ② 候选       —— analyze-pr 的出口有 2 条候选 → 这次建出 2 个子任务
		//   ③ 环         —— run-tests(failed) → apply-fix → run-tests(passed)
		//
		// 这是演示数据的核心：只有 3 个节点的链根本暴露不出布局问题、
		// 也验证不了"扇出的边怎么画"。
		{
			const traceId = nextTraceId();
			state.events.push({
				id: uid("evt"), orgId: ORG, traceId,
				source: "GitHub PR", type: "pull_request.reopened",
				payload: {
					pull_request: {
						number: 913, title: "重构参数映射", additions: 240, deletions: 88,
						user: { login: "bob" }, head: { ref: "refactor/params" }, base: { ref: "main" },
					},
					repository: { name: "anotify", full_name: "zzjcool/anotify" },
					action: "reopened",
				}, createdAt: t,
			});

			const analyze = capByName("analyze-pr");
			// 用「名字 + 起始偏移」建任务，方便表达扇出后的时间重叠
			const add = (name, parent, offset, dur, opts) => {
				const cap = capByName(name);
				const nodeIdx = opts.node != null ? opts.node : 0;
				const task = mkTask(Object.assign({
					traceId, cap,
					parent: parent ? parent.id : null,
					path: [analyze.id, cap.id],
					nodeId: state.nodes[nodeIdx].id,
					createdAt: t + offset,
					startedAt: t + offset,
					finishedAt: t + offset + dur,
					attempts: 1,
				}, opts));
				state.tasks.push(task);
				return task;
			};

			// ---- 根：analyze-pr（出口上有 2 条候选）----
			const a = add("analyze-pr", null, 0, 3.8, {
				out: "out", summary: "27 个文件，发现 6 处问题",
			});

			// ---- 候选第 1 支：lint-fix → run-tests(失败) ----
			const lintFix = add("lint-fix", a, 3.9, 2.1, {
				out: "out", summary: "自动修复 4 处，2 处需人工",
			});
			const testFail = add("run-tests", lintFix, 6.1, 34.2, {
				out: "out", status: "failed", node: 1,
				error: "e2e: 3 specs timed out (30s)",
			});
			// 失败后走环：改 → 再测（第二次带重试标记）
			const fix = add("apply-fix", testFail, 40.4, 12.6, {
				out: "out", node: 1, summary: "人工修复完成",
			});
			const testPass = add("run-tests", fix, 53.1, 41.3, {
				out: "out", node: 1, attempts: 2, summary: "全部 218 个用例通过",
			});

			// ---- 候选第 2 支：analyze-pr 的出口还直接连了 run-tests ----
			// （和上面那支同时开始 —— 演示数据里两条候选都走了，好把候选的形状画出来）
			const testDirect = add("run-tests", a, 3.9, 96.0, {
				out: "out", node: 1, summary: "全量回归通过",
			});

			// ---- 汇聚（不是 join，只是两条分支都指向同一个下游能力）----
			const deploy = add("deploy-preview", testPass, 94.5, 18.4, {
				out: "out", node: 1, params: { env: "staging" },
				summary: "staging 已就绪",
			});
			add("gen-changelog", deploy, 113, 1.2, { summary: "生成 14 条变更" });
			add("alert.oncall", deploy, 113, 0.3, { summary: "值班已确认" });

			// ---- 源扇出的另一支：通知链，与上面完全并行 ----
			add("report.send", null, 0.2, 0.9, {
				out: "out", params: { level: "warn", page: true },
				summary: "已通知 #ci-alerts",
			});

			t += 140;
		}

		// ---- 1c) 并行：两条独立的 trace 同时在跑 ----
		//
		// 想表现并行**不能**让一个父任务扇出（模型不存在那个状态，
		// Task.Out 是单值）。真实并行 = 两条 trace 各自在跑。
		// 事件流按源分组时，这两条会落在同一个源下面，瀑布图/图视图
		// 可以对比它们的耗时差异。
		{
			const branch = [
				{ src: "GitHub PR", type: "pull_request.opened", plan: [
					["analyze-pr", "out", "succeeded", 5.1, 0, "无问题"],
					["run-tests", "out", "succeeded", 22.7, 1, "218 用例通过"],
					["deploy-preview", "out", "succeeded", 15.2, 1, "staging 就绪"],
				]},
				{ src: "每晚体检", type: "nightly", plan: [
					["analyze-pr", "out", "succeeded", 2.2, 0, "基线正常"],
					["run-tests", "out", "succeeded", 9.4, 0, "冒烟通过"],
				]},
			];
			// 两条 **必须从完全相同的 createdAt 开始**，否则"并行"看不出来。
			// 取整到秒是刻意的：浮点毫秒会有微小差异，图上就有个错位。
			const baseClock = Math.floor(now() - 45);
			for (const br of branch) {
				const traceId = nextTraceId();
				state.events.push({
					id: uid("evt"), orgId: ORG, traceId,
					source: br.src, type: br.type, payload: {}, createdAt: baseClock,
				});
				let clock = baseClock;
				let prev = null;
				const rootId = capByName("analyze-pr").id;
				for (const [name, out, status, dur, nodeIdx, note] of br.plan) {
					const cap = capByName(name);
					const task = mkTask({
						traceId, cap,
						parent: prev ? prev.id : null,
						path: [rootId, cap.id],
						nodeId: state.nodes[nodeIdx].id,
						createdAt: clock, startedAt: clock, finishedAt: clock + dur,
						out: out || null, status, summary: note,
					});
					state.tasks.push(task);
					prev = task;
					clock += dur;
				}
			}
		}

		// ---- 2) 进行中：有一条任务还没结束（瀑布图宽度算到"现在"）----
		{
			const traceId = nextTraceId();
			state.events.push({
				id: uid("evt"), orgId: ORG, traceId,
				source: "每晚体检", type: "nightly",
				payload: {}, createdAt: now() - 6,
			});
			const analyze = capByName("analyze-pr");
			const a = mkTask({
				traceId, cap: analyze, createdAt: now() - 6,
				startedAt: now() - 5, finishedAt: null,
				status: "running", summary: "",
			});
			state.tasks.push(a);
		}

		// ---- 3) 待派发：还没被节点认领（只能画左边界细线）----
		{
			const traceId = nextTraceId();
			state.events.push({
				id: uid("evt"), orgId: ORG, traceId,
				source: "每日汇总", type: "manual.run",
				payload: {}, createdAt: now() - 30,
			});
			state.tasks.push(mkTask({
				traceId, cap: capByName("report.send"),
				createdAt: now() - 30, startedAt: null, finishedAt: null,
				status: "pending", nodeId: null,
				params: { channel: "email", title: "周报" },
			}));
		}

		// ---- 4) 快任务：0.4 秒，验证"零耗时也看得见" ----
		{
			const traceId = nextTraceId();
			state.events.push({
				id: uid("evt"), orgId: ORG, traceId,
				source: "GitHub PR", type: "pull_request.synchronize",
				payload: {
					pull_request: {
						number: 843, title: "加个测试", additions: 31, deletions: 0,
						user: { login: "carol" }, head: { ref: "test/more" }, base: { ref: "main" },
					},
					repository: { name: "anotify", full_name: "zzjcool/anotify" },
					action: "synchronize",
				}, createdAt: t,
			});
			state.tasks.push(mkTask({
				traceId, cap: capByName("deploy-preview"),
				nodeId: state.nodes[1].id,
				createdAt: t, startedAt: t + 1, finishedAt: t + 1.4,
				params: { env: "staging" }, summary: "已部署",
			}));
			t += 30;
		}

		// ---- 5) 重试过的任务：attempts=3，瀑布图要标出 ×3 ----
		{
			const traceId = nextTraceId();
			state.events.push({
				id: uid("evt"), orgId: ORG, traceId,
				source: "cron", type: "nightly",
				payload: {}, createdAt: t,
			});
			state.tasks.push(mkTask({
				traceId, cap: capByName("apply-fix"),
				createdAt: t, startedAt: t + 2, finishedAt: t + 9,
				attempts: 3, summary: "第 3 次尝试成功",
			}));
		}

		// ---- 6) 复杂链路：失败 → 重试 → 分支 → 汇聚 → 剪枝 全都占上 ----
		//
		// 前面每一条都只演示**一种**状态（纯成功链、并行对比、进行中、待派发、
		// 零耗时、重试过）。真实一次事故里这些是同时出现的：
		//
		//   analyze-pr ─→ lint-fix ─→ report.send ─→ alert.oncall
		//        │              └─→ （剪枝：run-tests 不被派发）
		//        └─→ run-tests ─→ apply-fix ─→ run-tests（×2 重试）
		//                          └────→ deploy-preview ─→ rollback ─→ alert.oncall
		//                                            （→gen-changelog 那条候选被剪枝）
		//
		// 这一条特意做了几件别的 trace 没做的事：
		//   · 同一个能力在一个 trace 里出现**多次**（run-tests 跑了 3 次、
		//     apply-fix 跑了 2 次）——瀑布图要能分清这是多次执行，不是重复渲染
		//   · 失败任务的 error 字段非空（UI 要展示错误文本）
		//   · 有 attempts=2 的重试标记
		//   · 有剪枝（另一条候选的 run-tests 没被派发，所以它不在任务里）
		//   · 时间跨度是前几条的两倍多（4 分半），瀑布图的刻度会换单位
		//
		// 注意仍然遵守 I5：每个任务只回报一个 out，所以是**一条链**在
		// 不同位置分叉，不是一棵同时展开的树。
		{
			const traceId = nextTraceId();
			// 刻意让这条**最新**：事件流按时间倒序，最近的一条排最上面。
			// 比"进行中"（-6s）和"零耗时"（t≈-120s）都新，这样打开 demo
			// 一眼就能看到"复杂链路"长什么样，不用往下翻找。
			const base = Math.floor(now() - 2);
			state.events.push({
				id: uid("evt"), orgId: ORG, traceId,
				source: "GitHub PR", type: "pull_request.synchronize",
				payload: {
					action: "synchronize",
					number: 844,
					pull_request: {
						number: 844,
						title: "重构参数解析（第 3 次推送）",
						additions: 486, deletions: 213, changed_files: 27,
						commits: 11, draft: false, merged: false,
						user: { login: "dave" },
						head: { ref: "refactor/param-parse", sha: "a1b2c3d4e5f6a7b8" },
						base: { ref: "main", sha: "9a8b7c6d5e4f3021" },
					},
					repository: {
						name: "anotify", full_name: "zzjcool/anotify",
						default_branch: "main", private: false,
					},
					sender: { login: "dave" },
				},
				createdAt: base,
			});

			const analyze = capByName("analyze-pr");
			const lintFix = capByName("lint-fix");
			const report = capByName("report.send");
			const alert = capByName("alert.oncall");
			const tests = capByName("run-tests");
			const fix = capByName("apply-fix");
			const deploy = capByName("deploy-preview");
			const rollback = capByName("rollback");
			const rootId = analyze.id;

			// 每跳的 [能力, 起点, 终点, out, 状态, 备注, nodeIdx, attempts]
			// nodeIdx 让瀑布图和节点页的"哪个实例跑的"对得上。
			// 重试**两轮**（而不是一轮）是刻意的：
			//
			// 环只绕一圈时，环上每条边都只被走了 1 次 —— "边上标注走过
			// 几次"这个功能就永远没有内容可显示。绕两圈才能看到
			// `failed ×2` / `fixed ×2`，也才像真实的重试。
			//
			// 时间跨度保持原来的 4m11s（把各段压缩），不把任务时间戳
			// 往未来推得更远。
			// 最后一列是**父任务在本数组里的下标**（null = 根任务）。
			//
			// 必须显式写，不能用"上一个元素"当父任务 —— 原来就是那么写的，
			// 结果算出 analyze→lint-fix→report→alert→**run-tests** 这种
			// 底图里根本不存在的父子对：alert 是终点，它没有出口能派发到
			// run-tests。连锁反应是"走过几次"全错位，
			// 真正走过的 analyze → run-tests 反而统计成 0 次，
			// 图上那条线就永远不动（issue #157）。
			const plan = [
				// [能力, 起点, 终点, 出口, 状态, 备注, nodeIdx, attempts, 父下标]
				[analyze, 0.0, 4.8, "out", "succeeded", "发现 1 个高危 + 27 处 lint", 0, 1, null],
				// 候选是 lint-fix 和 run-tests：演示数据里两条都走了，
				// 它们是兄弟，不是一条链上的前后。
				[lintFix, 4.8, 96.2, "out", "succeeded", "自动修了 26 处，1 处需人工", 0, 1, 0],
				[report, 96.2, 97.4, "out", "succeeded", "已通知值班人", 2, 1, 1],
				[alert, 97.4, 98.1, null, "succeeded", "电话已拨出", 2, 1, 2],
				[tests, 4.8, 48.3, "out", "failed", "3 个用例失败（parse_test.go）", 1, 1, 0],
				[fix, 48.3, 88.0, "out", "succeeded", "自动补了边界判断", 1, 2, 4],
				// 第二轮：修完还是不绿，再修一次才过
				[tests, 88.0, 125.0, "out", "failed", "还剩 1 个用例超时（TestParseTimeout）", 1, 1, 5],
				[fix, 125.0, 165.0, "out", "succeeded", "把超时阈值从 2s 提到 5s", 1, 2, 6],
				[tests, 165.0, 190.0, "out", "succeeded", "278 用例全部通过", 1, 1, 7],
				[deploy, 190.0, 231.5, "out", "failed", "健康检查 5 分钟未通过", 1, 1, 8],
				[rollback, 231.5, 250.8, "out", "succeeded", "已回滚到上一版", 1, 1, 9],
				[alert, 250.8, 251.4, null, "succeeded", "值班已收到升级", 2, 1, 10],
				// 注意这里**没有** gen-changelog：
				//
				// 配置里有 `deploy-preview → gen-changelog` 这条候选，
				// 而这次部署走的是另一条（→ rollback）。所以它根本没被派发，
				// 图视图上会以"剪枝"形式留在原地 —— 这正是底图的意义：
				// 让"配置里有这条路、但这次没走"变得可见。
				//
				// （之前让它"执行了"是错的：部署失败了哪来的预览地址去生成变更日志。）
			];

			const made = [];
			for (const [cap, t0, t1, out, status, note, nodeIdx, attempts, parentIdx] of plan) {
				const parent = parentIdx == null ? null : made[parentIdx];
				const clock = base + t0;
				const task = mkTask({
					traceId,
					cap,
					parent: parent ? parent.id : null,
					path: [rootId, cap.id],
					nodeId: state.nodes[nodeIdx].id,
					createdAt: clock,
					startedAt: clock,
					finishedAt: base + t1,
					out: out || null,
					status,
					summary: note,
					attempts,
					params:
						cap.name === "deploy-preview"
							? { env: "production", version: "v1.4.0-rc3" }
							: cap.name === "report.send"
								? { channel: "slack", title: "PR #844 需要人工确认" }
								: { mock: true },
					// 失败任务必须带错误文本：UI 上要能展开看原因，
					// 只有 status=failed 而没有 error 是"假装失败"。
					error:
						status === "failed"
							? cap.name === "run-tests"
								? "3 failures: TestParseNested / TestParseEmpty / TestParseLoop"
								: "healthcheck timed out after 300s (GET /health → 503)"
							: "",
				});
				state.tasks.push(task);
				made.push(task);
			}
		}
	})();

	// ---------- emit 种子数据（P1.6）----------
	//
	// 真后端的 emissions 是「每次 emit 一行」（迁移 22，PRIMARY KEY(task_id, seq)），
	// trace 页的「产出（emits）」区就是读它。这里给**主线 trace 的根任务**种三条，
	// 形状刻意覆盖三种结果：
	//
	//   #1 routed=1 pruned=1 —— 候选里派出去一条、另一条被剪
	//   #2 routed=0 pruned=1 —— 全被剪（routed=0 是合法结果）
	//   #3 routed=1 pruned=0 —— 正常派出
	//
	// 出口名一律是 "out"（单出口的内部名，对用户不可见）：emit 的出口必须在
	// 这个能力的 outputs 里（I2），mock 造一个不存在的出口就是在演示一个真后端
	// 会拒收的形状。同一个任务可以 emit 多次（seq 区分），出口不变。
	//
	// 为什么选主线 trace（trace_mock_t2）的 analyze-pr：它是事件流里点「查看链路」
	// 最常落到的那条（复杂、有扇出、有失败），打开就能看到产出区长什么样。
	// traceSeq 是确定性的（见 nextTraceId 的注释），所以 trace_mock_t2 跨刷新稳定。
	(function seedEmissions() {
		const host =
			state.tasks.find((t) => t.traceId === "trace_mock_t2" && t.capability === "analyze-pr") ||
			state.tasks[0];
		if (!host) return;
		// 时间贴着任务自身的时间轴：emit 发生在任务跑的过程中（startedAt..finishedAt），
		// 不是"任务结束后才冒出来"的一条。
		const base = host.startedAt != null ? host.startedAt : host.createdAt;
		const row = (seq, port, data, routed, pruned, offset, note) => ({
			taskId: host.id,
			seq,
			port,
			data,
			// 真后端的 emissions 表还没有 note 列（中枢收到帧后只记日志），
			// 所以这里也留空 —— mock 不能比真后端"多出"一个会误导人的字段值。
			note: note || "",
			routed,
			pruned,
			createdAt: Math.round(base + offset),
		});
		state.emissions.push(
			row(1, "out", { issue: "#913", files: 27, findings: 6, patch: "a1b2c3d" }, 1, 1, 0.4),
			row(2, "out", { issue: "#913", files: 3, findings: 2, patch: "a1b2c3d" }, 0, 1, 1.7),
			row(3, "out", { issue: "#913", files: 27, approved: false }, 1, 0, 3.1),
		);
	})();

	// ---------- 组织与命名空间 ----------
	//
	// 组织是身份边界；命名空间是**配置 + 运行面**的隔离边界：
	// sources / edges / events / tasks 属于命名空间，
	// capabilities / nodes 属于组织（能力和节点是共享资源池，见设计文档 §2）。
	//
	// 演示要求：切命名空间得能看出差别，否则这功能等于没有。所以
	//   prod    = 整套 t1~t12 演示数据（原来那些）
	//   staging = 一条独立的 cron 链（源 / 边 / trace 都和 prod 不同）
	//   sandbox = 故意留空（用来看空状态，也用来演示"非空不能删"）
	//   外加第二个组织 Acme，证明"换组织 = 换一整套命名空间"。
	(function initNamespaces() {
		const STAGING = "ns_mock_staging";
		const SANDBOX = "ns_mock_sandbox";
		const ACME = "org_mock_acme";
		const ACME_NS = "ns_mock_acme_default";

		state.orgs = [
			{ id: ORG, name: "演示组织", slug: "demo", createdAt: now() - 86400 * 40 },
			{ id: ACME, name: "Acme", slug: "acme", createdAt: now() - 86400 * 12 },
		];
		state.namespaces = [
			{ id: NS, orgId: ORG, slug: "prod", name: "生产", createdAt: now() - 86400 * 40 },
			{
				id: STAGING,
				orgId: ORG,
				slug: "staging",
				name: "预发",
				createdAt: now() - 86400 * 30,
			},
			{
				id: SANDBOX,
				orgId: ORG,
				slug: "sandbox",
				name: "沙箱",
				createdAt: now() - 86400 * 3,
			},
			{
				id: ACME_NS,
				orgId: ACME,
				slug: "default",
				name: "默认",
				createdAt: now() - 86400 * 12,
			},
		];

		// 已有演示数据统一归到 prod（不逐个改写种子，省得以后加种子又漏）
		for (const x of state.sources) x.namespaceId = NS;
		for (const x of state.edges) x.namespaceId = NS;
		for (const x of state.events) x.namespaceId = NS;
		for (const x of state.tasks) x.namespaceId = NS;
		for (const x of state.capabilities) x.namespaceId = NS;
		for (const x of state.nodes) x.namespaceId = NS;

		// ---- 内置能力：每个命名空间各有一份 ----
		//
		// 这是实现里最容易漏的一步：内置能力（notify.phone）是随项目发布的，
		// 不属于任何用户操作，所以新建命名空间时**必须同事务播种**，
		// 否则新命名空间里连一个 end 节点都声明不出来，推送直接失效。
		// demo 里 staging / sandbox 各有一份，用来验证这条规则。
		const builtinFor = (nsId) => {
			const c = cap("notify.phone", 1, [], { type: "object" }, "builtin", "推送通知");
			return Object.assign(c, {
				id: "cap_mock_" + nsId + "_notify_phone_v1",
				namespaceId: nsId,
				// 内容相同 → hash 相同（同一份二进制播种的），跨命名空间重复不算冲突
				contentHash: "hash_builtin_notify_phone_v1",
			});
		};
		state.capabilities.push(builtinFor(STAGING), builtinFor(SANDBOX));

		// ---- staging：一条和 prod 完全无关的链 ----
		//
		// 注意这里**故意不用 prod 的能力 id**：能力按命名空间隔离，
		// staging 的 analyze-pr@1 是另一个定义（另一个 id）。
		// 拿 prod 的 id 建边就是跨命名空间引用，I11 会拒。
		const stgCap = (name, version, outputs, display) => {
			const c = cap(name, version, outputs, { type: "object" }, "custom", display);
			return Object.assign(c, {
				id: "cap_mock_staging_" + name.replace(/\./g, "_") + "_v" + version,
				namespaceId: STAGING,
			});
		};
		const analyze = stgCap("analyze-pr", 1, ["out"], "分析 PR（预发）");
		const tests = stgCap("run-tests", 1, ["out"], "跑测试（预发）");
		const deploy = stgCap("deploy-preview", 1, ["out"], "部署预发（预发）");
		state.capabilities.push(analyze, tests, deploy);

		// staging 的机器：一台，只声明 staging 的三个能力
		state.nodes.push({
			id: "nd_mock_staging_runner",
			orgId: ORG,
			namespaceId: STAGING,
			name: "staging-runner",
			status: "online",
			lastSeenAt: now() - 12,
			createdAt: now() - 86400 * 20,
			maxInstances: 4,
			maxConcurrency: 2,
			tags: ["staging", "ci"],
			capabilities: [analyze, tests, deploy].map((c) => ({
				id: c.id,
				name: c.name,
				version: c.version,
				displayName: c.displayName,
			})),
			instances: [{ id: "nins_mock_stg_0", index: 0, concurrency: 2, busy: 0 }],
		});

		// 让"全就绪"的包真的存在：需要一个源把事件送进 lint-fix。
		// 否则每个包的触发器都是缺的，"就绪"这个状态在演示里永远看不到。
		const lintCap = state.capabilities.find(
			(c) => c.name === "lint-fix" && c.namespaceId === NS,
		);
		if (lintCap) {
			state.sources.push({
				id: "src_mock_lint",
				orgId: ORG,
				namespaceId: NS,
				name: "Lint 钩子",
				kind: "webhook",
				config: { token: "lint-demo" },
				enabled: true,
				lastFiredAt: now() - 3600 * 5,
				createdAt: now() - 86400 * 6,
				targets: [{ capabilityId: lintCap.id, params: { changedLines: 30 } }],
			});
		}

		const traceId = "trace_mock_staging_daily";
		const t0 = now() - 3600 * 6;

		state.sources.push({
			id: "src_mock_staging_cron",
			orgId: ORG,
			namespaceId: STAGING,
			name: "预发每日体检",
			kind: "cron",
			config: { expr: "0 4 * * *", tz: "Local" },
			enabled: true,
			lastFiredAt: t0,
			nextFireAt: t0 + 86400,
			createdAt: t0 - 86400 * 10,
			targets: [{ capabilityId: analyze.id, params: { changedLines: 12, repo: "anotify" } }],
		});

		const stgByName = { "analyze-pr": analyze, "run-tests": tests, "deploy-preview": deploy };
		const edge = (from, to, literals) => ({
			id: uid("edg"),
			orgId: ORG,
			namespaceId: STAGING,
			fromRef: capRef(stgByName[from]),
			fromPort: "out",
			toRef: capRef(stgByName[to]),
			literals: literals || {},
			priority: 0,
			createdAt: t0 - 86400 * 10,
		});
		state.edges.push(edge("analyze-pr", "run-tests"));
		state.edges.push(edge("run-tests", "deploy-preview"));

		state.events.push({
			id: uid("evt"),
			orgId: ORG,
			namespaceId: STAGING,
			traceId,
			source: "每晚体检",
			type: "nightly",
			payload: { env: "staging", repo: "anotify", commit: "9f3c1ab" },
			createdAt: t0,
		});

		const mkNsTask = (o) => ({
			id: nextTaskId(),
			orgId: ORG,
			namespaceId: STAGING,
			traceId,
			parentTaskId: o.parent || null,
			path: o.path || [],
			capabilityId: o.cap.id,
			capability: o.cap.name,
			version: o.cap.version,
			params: { mock: true },
			status: "succeeded",
			nodeId: state.nodes[1] ? state.nodes[1].id : null,
			instanceId: state.nodes[1] ? "nins_mock_0" : null,
			leaseUntil: null,
			attempts: 1,
			out: o.out !== undefined ? o.out : null,
			reported: true,
			summary: o.summary || "",
			artifacts: [],
			error: "",
			contentHash: o.cap.contentHash,
			capHash: o.cap.contentHash,
			graphId: "grf_mock",
			createdAt: o.at,
			startedAt: o.at,
			finishedAt: o.at + (o.secs || 20),
			notBefore: null,
		});
		const a = mkNsTask({ cap: analyze, at: t0 + 0.4, out: "out", secs: 18 });
		const t = mkNsTask({
			cap: tests,
			parent: a.id,
			path: [analyze.id, tests.id],
			at: t0 + 19,
			out: "out",
			secs: 63,
		});
		const d = mkNsTask({
			cap: deploy,
			parent: t.id,
			path: [analyze.id, tests.id, deploy.id],
			at: t0 + 83,
			out: null,
			secs: 41,
			summary: "预发部署完成",
		});
		state.tasks.push(a, t, d);

		// ---- Acme 组织：另一套完全独立的数据 ----
		//
		// 为什么要给第二个组织造全量数据：切过去如果整个产品是空的，
		// 看起来就像"mock 挂了 / 这功能坏了"，而不是"这是另一个组织"。
		// 换组织应该看到**同样完整、但内容完全不同**的东西。
		const acmeBuiltin = builtinFor(ACME_NS);
		state.capabilities.push(acmeBuiltin);

		const acmeCap = (name, version, outputs, display) => {
			const c = cap(name, version, outputs, { type: "object" }, "custom", display);
			return Object.assign(c, {
				id: "cap_mock_acme_" + name.replace(/\./g, "_") + "_v" + version,
				orgId: ACME,
				namespaceId: ACME_NS,
			});
		};
		const build = acmeCap("build-image", 1, ["out"], "构建镜像");
		const scan = acmeCap("scan-deps", 1, ["out"], "依赖扫描");
		const release = acmeCap("release", 1, ["out"], "发布");
		state.capabilities.push(build, scan, release);

		state.nodes.push({
			id: "nd_mock_acme_builder",
			orgId: ACME,
			namespaceId: ACME_NS,
			name: "acme-builder",
			status: "online",
			lastSeenAt: now() - 5,
			createdAt: now() - 86400 * 11,
			maxInstances: 8,
			maxConcurrency: 4,
			tags: ["acme", "builder"],
			capabilities: [build, scan, release, acmeBuiltin].map((c) => ({
				id: c.id,
				name: c.name,
				version: c.version,
				displayName: c.displayName,
			})),
			instances: [{ id: "nins_mock_acme_0", index: 0, concurrency: 4, busy: 0 }],
		});

		const acmeTrace = "trace_mock_acme_release";
		const at = now() - 3600 * 2;
		state.sources.push({
			id: "src_mock_acme_pr",
			orgId: ACME,
			namespaceId: ACME_NS,
			name: "Acme PR",
			kind: "webhook",
			config: { token: "acme-demo" },
			enabled: true,
			lastFiredAt: at,
			createdAt: now() - 86400 * 11,
			targets: [{ capabilityId: build.id, params: { repo: "acme/web", branch: "main" } }],
		});

		const acmeEdge = (from, to, caps) => ({
			id: uid("edg"),
			orgId: ACME,
			namespaceId: ACME_NS,
			fromRef: capRef(caps[from]),
			fromPort: "out",
			toRef: capRef(caps[to]),
			priority: 0,
			createdAt: now() - 86400 * 11,
		});
		const acmeCaps = { "build-image": build, "scan-deps": scan, release: release };
		state.edges.push(
			acmeEdge("build-image", "scan-deps", acmeCaps),
			acmeEdge("scan-deps", "release", acmeCaps),
		);

		state.events.push({
			id: uid("evt"),
			orgId: ACME,
			namespaceId: ACME_NS,
			traceId: acmeTrace,
			source: "Acme PR",
			type: "pull_request.opened",
			payload: { repository: { name: "acme/web" }, pull_request: { number: 17 } },
			createdAt: at,
		});

		const acmeTask = (o) => ({
			id: nextTaskId(),
			orgId: ACME,
			namespaceId: ACME_NS,
			traceId: acmeTrace,
			parentTaskId: o.parent || null,
			path: o.path || [],
			capabilityId: o.cap.id,
			capability: o.cap.name,
			version: o.cap.version,
			params: { mock: true },
			status: "succeeded",
			nodeId: "nd_mock_acme_builder",
			instanceId: "nins_mock_acme_0",
			leaseUntil: null,
			attempts: 1,
			out: o.out !== undefined ? o.out : null,
			reported: true,
			summary: o.summary || "",
			artifacts: [],
			error: "",
			contentHash: o.cap.contentHash,
			capHash: o.cap.contentHash,
			graphId: "grf_mock",
			createdAt: o.at,
			startedAt: o.at,
			finishedAt: o.at + (o.secs || 20),
			notBefore: null,
		});
		const ab = acmeTask({ cap: build, at: at + 0.3, out: "out", secs: 42 });
		const as = acmeTask({
			cap: scan,
			parent: ab.id,
			path: [build.id, scan.id],
			at: at + 43,
			out: "out",
			secs: 21,
		});
		const ar = acmeTask({
			cap: release,
			parent: as.id,
			path: [build.id, scan.id, release.id],
			at: at + 65,
			out: null,
			secs: 15,
			summary: "已发布 v1.4.0",
		});
		state.tasks.push(ab, as, ar);
	})();

	// ---------- 源的入口边（「源 → 出口能力」的唯一真相）----------
	//
	// 「这个源配了哪些出口能力」在模型里只有一份真相：**入口边**
	// （from_is_source=1、from_ref=源名、to_ref=能力引用）。后端建源/改源就是
	// 重建这批边，源详情的 targetCaps 也是从边读回来的。
	//
	// mock 之前缺了这一步：state.sources 里写着 targets，state.edges 里却只有几条
	// 左端写着 "github" / "cron" / "http" / "manual" 的装饰边 —— 和任何一个**源名**
	// 都对不上。后果是两个页面同时缺图：source.html 的下游流向图拿不到自己的
	// 入口边（图是空的），trace.html 的底图也找不到这个源的拓扑
	// （退化成"只画执行链"，看不出哪条分支被剪掉）。
	//
	// 这里按真实后端的口径补一遍：每个 target 一条入口边，出口是 '*'
	// （外部系统的事件类型不可能预先枚举），右端钉死当前版本
	// （源的目标在控制台上就是"选一个能力的某一版"）。
	(function initSourceEntryEdges() {
		for (const s of state.sources) {
			for (const t of s.targets || []) {
				// 能力 id 是命名空间内的定义 id，所以按 id 查不会拿错别人家的版本
				const c = state.capabilities.find((x) => x.id === t.capabilityId);
				// 能力被删了就不造边：真实后端里那种边解不出 toCapabilityId，
				// 图上会多一个"已删除"的假节点，而 mock 没必要演示坏数据
				if (!c) continue;
				state.edges.push({
					id: uid("edg"),
					orgId: ORG,
					namespaceId: s.namespaceId,
					fromIsSource: true,
					// 左端是**源名**（不是能力引用，也不是 kind）
					fromRef: s.name,
					fromPort: "*",
					toRef: capRef(c),
					// mapping 是入口边的参数模板；mock 的触发路径直接读 sources[].targets，
					// 这里留空不影响演示（两边的"参数"仍然指的是同一份 targets）。
					literals: {},
					mapping: {},
					priority: 0,
					createdAt: now() - 3600,
				});
			}
		}
	})();

	// ---------- 包（可安装的流程零件）----------
	//
	// 包本身不持有自己的图：`manifest` 是"期望长什么样"，
	// 真实状态永远从 capabilities / edges / sources / node_capabilities 里读。
	// 所以"装完之后手工改了一条边"，体检会如实反映 —— 两边不会各说各话。
	//
	// 四个包刻意覆盖四种总体状态，让人一眼看到每个状态长什么样：
	//   pr-review      needs-node（能力都有，但 run-tests 的机器离线了）
	//   nightly-audit  needs-node（archive-logs 定义了，没有任何节点声明它）
	//   release-train  blocked（notify.sms 连定义都没有）
	//   lint-guard     ready（能力 + 触发器都齐）
	const RUN = (name, extra) =>
		Object.assign(
			{
				runner: "bash",
				command: "./runners/" + name + ".sh",
				timeoutSec: 600,
				concurrency: 1,
				env: {},
			},
			extra || {},
		);

	state.playbooks = [
		{
			id: "pb_mock_pr_review",
			orgId: ORG,
			namespaceId: NS,
			slug: "pr-review",
			name: "PR 审查",
			description: "PR 一开就分析改动：要修先修，再跑测试。",
			createdAt: now() - 86400 * 9,
			manifest: {
				capabilities: [
					{
						name: "analyze-pr",
						version: 1,
						outputs: ["out"],
						runner: RUN("analyze-pr", { timeoutSec: 900, concurrency: 2 }),
					},
					{
						name: "lint-fix",
						version: 1,
						outputs: ["out"],
						runner: RUN("lint-fix"),
					},
					{
						name: "run-tests",
						version: 1,
						outputs: ["out"],
						runner: RUN("run-tests", { timeoutSec: 1800 }),
					},
				],
				edges: [
					{ from: "analyze-pr", fromPort: "out", to: "lint-fix" },
					{ from: "analyze-pr", fromPort: "out", to: "run-tests" },
					{ from: "lint-fix", fromPort: "out", to: "run-tests" },
				],
				triggers: [
					{
						kind: "webhook",
						types: ["pull_request.opened", "pull_request.synchronize"],
					},
				],
			},
		},
		{
			id: "pb_mock_nightly_audit",
			orgId: ORG,
			namespaceId: NS,
			slug: "nightly-audit",
			name: "夜间审计",
			description: "每晚归档日志，然后把结果发出来。",
			createdAt: now() - 86400 * 4,
			manifest: {
				capabilities: [
					{
						name: "archive-logs",
						version: 1,
						outputs: ["out"],
						runner: RUN("archive-logs", { timeoutSec: 3600 }),
					},
					{
						name: "report.send",
						version: 1,
						outputs: ["out"],
						runner: RUN("report.send"),
					},
				],
				edges: [{ from: "archive-logs", fromPort: "out", to: "report.send" }],
				// cron 的 types 就是 5 段表达式本身：安装源时要原样写进 config.expr
				triggers: [{ kind: "cron", types: ["0 3 * * *"] }],
			},
		},
		{
			id: "pb_mock_release_train",
			orgId: ORG,
			namespaceId: NS,
			slug: "release-train",
			name: "发布列车",
			description: "预先部署，失败就回滚，成功推一条短信。",
			createdAt: now() - 86400 * 2,
			manifest: {
				capabilities: [
					{
						name: "deploy-preview",
						version: 1,
						outputs: ["out"],
						runner: RUN("deploy-preview"),
					},
					{
						name: "rollback",
						version: 1,
						outputs: ["out"],
						runner: RUN("rollback"),
					},
					{
						// 故意没定义：演示 blocked（连契约都不全）
						name: "notify.sms",
						version: 1,
						outputs: [],
						runner: RUN("notify.sms"),
					},
				],
				edges: [
					{ from: "deploy-preview", fromPort: "out", to: "rollback" },
					{ from: "deploy-preview", fromPort: "out", to: "notify.sms" },
				],
				triggers: [{ kind: "webhook", types: ["release.published"] }],
			},
		},
		{
			id: "pb_mock_lint_guard",
			orgId: ORG,
			namespaceId: NS,
			slug: "lint-guard",
			name: "静态检查",
			description: "只有一条：改动进来就自动清理。",
			createdAt: now() - 86400,
			manifest: {
				capabilities: [
					{
						name: "lint-fix",
						version: 1,
						outputs: ["out"],
						runner: RUN("lint-fix"),
					},
				],
				edges: [],
				triggers: [{ kind: "webhook", types: ["push"] }],
			},
		},
	];

	// 就绪体检：全部是查询时算出来的（推导量不落库，I3 同源）
	//
	// 四类检查按依赖顺序：能力定义 → 节点实现 → 出口 → 触发器。
	// no-node 和 offline **必须分开**：前者要去写 YAML 装机器，
	// 后者只需要把机器连上。合成一个"未就绪"会让人做错事。
	function pbChecks(pb) {
		const ns = pb.namespaceId;
		const caps = state.capabilities.filter((c) => c.namespaceId === ns);
		const nodes = state.nodes.filter((n) => n.namespaceId === ns);
		const edges = state.edges.filter((e) => e.namespaceId === ns);
		const sources = state.sources.filter((x) => x.namespaceId === ns);
		const m = pb.manifest;
		const findCap = (name, version) =>
			caps.find((c) => c.name === name && (version == null || c.version === version));

		// ① 能力定义 + ② 节点实现（绑在同一行上，因为第二项依赖第一项）
		const capItems = (m.capabilities || []).map((c) => {
			const ref = c.name + "@" + c.version;
			const def = findCap(c.name, c.version);
			if (!def) {
				return {
					kind: "capability",
					ref: ref,
					status: "missing",
					outputs: c.outputs || [],
					runner: c.runner || null,
				};
			}
			const owners = nodes.filter((n) =>
				(n.capabilities || []).some((x) => x.id === def.id),
			);
			if (!owners.length) {
				return {
					kind: "capability",
					ref: ref,
					status: "no-node",
					outputs: def.outputs,
					capId: def.id,
					runner: c.runner || null,
				};
			}
			const online = owners.filter((n) => n.status === "online");
			return {
				kind: "capability",
				ref: ref,
				status: online.length ? "ok" : "offline",
				outputs: def.outputs,
				capId: def.id,
				runner: c.runner || null,
				nodes: owners.map((n) => ({ name: n.name, status: n.status })),
			};
		});

		// ③ 出口：两端能力都要有定义才能谈"这条边在不在"
		const edgeItems = (m.edges || []).map((e) => {
			const from = findCap(e.from);
			const to = findCap(e.to);
			const label = e.from + " --" + e.fromPort + "--> " + e.to;
			if (!from || !to) {
				return {
					kind: "edge",
					label: label,
					status: "missing",
					// 端点缺定义 → 这条边现在补不了，先补能力
					blockedByCap: !from ? e.from : e.to,
				};
			}
			// 「这条边在不在」比的是**两端解析到的能力**（引用 → 当前版本），
			// 不是引用字符串相等：预案声明的是「名字」，而库里可能是 `name@1`。
			const hit = edges.some(
				(x) =>
					!x.fromIsSource &&
					(resolveCapRef(x.fromRef, ns) || {}).id === from.id &&
					x.fromPort === e.fromPort &&
					(resolveCapRef(x.toRef, ns) || {}).id === to.id,
			);
			return {
				kind: "edge",
				label: label,
				status: hit ? "ok" : "missing",
				from: e.from,
				fromPort: e.fromPort,
				to: e.to,
				fromId: from.id,
				toId: to.id,
			};
		});

		// ④ 触发器：kind 匹配**且**这个源的目标里有本包的入口能力
		//
		// 只匹配 kind 会把"你有一个 webhook 源"当成"这条流程有触发器了"，
		// 而那个源可能指向完全无关的能力。
		const toSet = new Set((m.edges || []).map((e) => e.to));
		const entries = (m.capabilities || []).filter((c) => !toSet.has(c.name));
		const entryIds = entries.map((c) => findCap(c.name, c.version)).filter(Boolean).map((c) => c.id);
		const triggerItems = (m.triggers || []).map((t) => {
			const hit = sources.some(
				(x) =>
					x.kind === t.kind &&
					(x.targets || []).some((g) => entryIds.includes(g.capabilityId)),
			);
			return {
				kind: "trigger",
				label:
					t.kind + (t.types && t.types.length ? " · " + t.types.join(" / ") : ""),
				status: hit ? "ok" : "missing",
				entryCaps: entries.map((c) => c.name),
				declaredKind: t.kind,
			};
		});

		const items = capItems.concat(edgeItems, triggerItems);
		const capMissing = capItems.some((i) => i.status === "missing");
		const capNoNode = capItems.some((i) => i.status === "no-node" || i.status === "offline");
		const restBad =
			edgeItems.some((i) => i.status !== "ok") ||
			triggerItems.some((i) => i.status !== "ok");

		let status = "ready";
		if (capMissing) status = "blocked";
		else if (capNoNode) status = "needs-node";
		else if (restBad) status = "partial";

		return {
			status: status,
			// 「最该先做的一件事」= 按依赖顺序第一个没过的项
			next: items.find((i) => i.status !== "ok") || null,
			ready: items.filter((i) => i.status === "ok").length,
			total: items.length,
			counts: {
				capabilities: capItems.length,
				edges: edgeItems.length,
				triggers: triggerItems.length,
			},
			items: items,
		};
	}

	function pbView(pb, withItems) {
		const checks = pbChecks(pb);
		const view = {
			id: pb.id,
			orgId: pb.orgId,
			namespaceId: pb.namespaceId,
			slug: pb.slug,
			name: pb.name,
			description: pb.description,
			createdAt: pb.createdAt,
			status: checks.status,
			next: checks.next,
			ready: checks.ready,
			total: checks.total,
			counts: checks.counts,
			manifest: pb.manifest,
		};
		if (withItems) view.items = checks.items;
		return view;
	}

	// 幂等补齐。顺序必须是 能力 → 出口 → 源：
	// 出口两端没有定义就建不了，源没有入口能力就没人可派。
	function installPlaybook(pb) {
		const ns = pb.namespaceId;
		const m = pb.manifest || {};
		const created = { capabilities: [], edges: [], sources: [] };
		const skipped = { capabilities: [], edges: [], sources: [] };
		const find = (name) =>
			state.capabilities.find((c) => c.namespaceId === ns && c.name === name);

		for (const c of m.capabilities || []) {
			const ref = c.name + "@" + c.version;
			if (
				state.capabilities.some(
					(x) =>
						x.namespaceId === ns && x.name === c.name && x.version === c.version,
				)
			) {
				skipped.capabilities.push(ref);
				continue;
			}
			state.capabilities.push({
				id: "cap_" + ns + "_" + c.name.replace(/\./g, "_") + "_v" + c.version,
				orgId: pb.orgId,
				namespaceId: ns,
				name: c.name,
				displayName: "",
				version: c.version,
				outputs: c.outputs || [],
				paramsSchema: c.paramsSchema || { type: "object" },
				source: "custom",
				contentHash: "hash" + Math.random().toString(16).slice(2, 10),
				createdAt: now(),
			});
			created.capabilities.push(ref);
		}

		for (const e of m.edges || []) {
			const from = find(e.from);
			const to = find(e.to);
			const label = e.from + " --" + e.fromPort + "--> " + e.to;
			// 端点还缺定义：这一轮补不了，等能力补完再点一次（幂等所以无害）
			if (!from || !to) continue;
			if (
				state.edges.some(
					(x) =>
						x.namespaceId === ns &&
						(resolveCapRef(x.fromRef, ns) || {}).id === from.id &&
						x.fromPort === e.fromPort &&
						(resolveCapRef(x.toRef, ns) || {}).id === to.id,
				)
			) {
				skipped.edges.push(label);
				continue;
			}
			state.edges.push({
				id: uid("edg"),
				orgId: pb.orgId,
				namespaceId: ns,
				// 预案里写的是 `name@version`（与同一预案声明的能力定义对应）
				fromRef: capRef(from),
				fromPort: e.fromPort,
				toRef: capRef(to),
				// 预案里的边不带常量覆盖（'{}' = 没有覆盖），形状与新建边一致
				literals: {},
				createdAt: now(),
			});
			created.edges.push(label);
		}

		const toSet = new Set((m.edges || []).map((e) => e.to));
		const entries = (m.capabilities || []).filter((c) => !toSet.has(c.name));
		const entryCaps = entries.map((c) => find(c.name)).filter(Boolean);
		for (const t of m.triggers || []) {
			if (!entryCaps.length) continue;
			const hit = state.sources.some(
				(x) =>
					x.namespaceId === ns &&
					x.kind === t.kind &&
					(x.targets || []).some((g) =>
						entryCaps.some((c) => c.id === g.capabilityId),
					),
			);
			if (hit) {
				skipped.sources.push(t.kind);
				continue;
			}
			const config =
				t.kind === "cron"
					? { expr: (t.types && t.types[0]) || "0 3 * * *", tz: "Local" }
					: t.kind === "webhook" || t.kind === "github"
						? { token: "mock-" + Math.random().toString(36).slice(2, 8) }
						: {};
			const src = {
				id: uid("src"),
				orgId: pb.orgId,
				namespaceId: ns,
				name: pb.name + " · " + t.kind,
				kind: t.kind,
				config: config,
				enabled: true,
				lastFiredAt: null,
				createdAt: now(),
				targets: entryCaps.map((c) => ({ capabilityId: c.id, params: {} })),
			};
			state.sources.push(src);
			created.sources.push(src.name);
		}

		return { created, skipped };
	}

	// ---------- 初始 keys ----------
	//
	// 每条记录都必须带 `enabled: boolean`：真后端的 keyPublic（internal/server/handlers.go）
	// **总是**回这个字段，mock 种子漏掉就是形状漂移。前端 keys.html:756 的归一化是
	// `k.enabled !== false && k.enabled !== 0` —— 字段缺失时只能靠这个兜底当「启用」，
	// 一旦某条被停用（enabled:false）就会与缺失态混淆，所以源数据要把语义写全。
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
	function json(body, status) {
		return new Response(JSON.stringify(body), {
			status: status || 200,
			headers: { "Content-Type": "application/json; charset=utf-8" },
		});
	}
	function err(status, message, extra) {
		return json(Object.assign({ error: message }, extra || {}), status);
	}

	// 视图：能力列表带推导出的 kind
	// capView 与真实 handler（consoleHandler.capabilities）同形状：
	// 除了推导的 kind，还带"谁声明了它"。
	//
	// declaredBy 是**关键信息**：能力定义只是"能干什么"的白名单，真正让它
	// 跑得起来的是有机器声明它。空数组 = 这个能力永远跑不起来。
	function capView(c) {
		const declaredBy = state.nodes
			.filter((n) => (n.capabilities || []).some((x) => x.id === c.id))
			// 与后端排序一致：在线优先，然后按名字
			.sort((a, b) => {
				const rank = (st) => (st === "online" ? 0 : st === "draining" ? 1 : 2);
				return rank(a.status) - rank(b.status) || (a.name < b.name ? -1 : 1);
			})
			.map((n) => ({ id: n.id, name: n.name, status: n.status }));
		// 谁执行它：和真实 handler 一致
		const executedBy = CENTRAL_NAMES.includes(c.name)
			? "hub"
			: ORIGIN_NAMES.includes(c.name)
				? "none"
				: "node";
		return Object.assign(clone(c), { kind: kindOf(c), declaredBy, executedBy });
	}

	// 视图：源带出口能力信息 + hookUrl
	function sourceView(s) {
		const byID = new Map(state.capabilities.map((c) => [c.id, c]));
		const targetCaps = (s.targets || []).map((t) => {
			const c = byID.get(t.capabilityId);
			if (!c) return { id: t.capabilityId, missing: true, params: t.params || {} };
			return {
				id: c.id,
				name: c.name,
				displayName: c.displayName || "",
				version: c.version,
				params: t.params || {},
				paramsSchema: c.paramsSchema,
			};
		});
		const v = Object.assign(clone(s), { targetCaps });
		if ((s.kind === "webhook" || s.kind === "github") && s.config && s.config.token) {
			v.hookUrl = "/v1/hooks/" + s.config.token;
		}
		return v;
	}

	// ---------- 路由表 ----------
	//
	// 每个条目：(method, 正则) → handler(ctx) → Response
	// ctx 里有 json body、params、路径捕获。

	// 命名空间解析。
	//
	// 契约（CONTRACT §6.2）要求命名空间资源**必须显式**带 namespaceId，
	// 缺了返回 400。mock 这里故意宽松：缺省 = 默认命名空间。
	// 理由：mock 是给人点着看的，深链（trace.html?traceId=...）和
	// 手敲 curl 都不该因为少一个参数就白屏；而"UI 到底有没有带"
	// 由 E2E 断言盯（它检查页面请求的 query）。
	// `namespaceId=*` = 跨命名空间汇总（管理员视图）。
	function nsScope(q) {
		const v = q && q.get("namespaceId");
		if (v === "*") return null; // null = 不按命名空间过滤
		return v && v !== "" ? v : NS;
	}
	const inNs = (list, scope) =>
		scope === null ? list.slice() : list.filter((x) => x.namespaceId === scope);

	const routes = [];
	function on(method, pattern, handler) {
		routes.push({ method, pattern, handler });
	}

	// ===== 认证 =====
	on("GET", /^\/v1\/auth\/me$/, () => json(ME));
	on("POST", /^\/v1\/auth\/logout$/, () => json({ ok: true }));
	on("GET", /^\/v1\/vapid-public-key$/, () =>
		json({ publicKey: "BMockPublicKeyForPreviewOnly_NotARealVapidKey" }),
	);

	// ===== 组织 / 命名空间 =====
	//
	// 这两个端点在真实后端还没实现（设计先行，见 docs/design/namespaces.md），
	// 所以**故意不写进 api/openapi.yaml** —— check_openapi.py 要求 spec 与
	// mux.go 双向一致，先写 spec 没有 handler 会直接让 make contract 失败。
	on("GET", /^\/v1\/orgs$/, () => json({ orgs: clone(state.orgs) }));

	on("GET", /^\/v1\/namespaces$/, (ctx) => {
		const orgId = ctx.query.get("orgId") || ORG;
		return json({
			namespaces: clone(state.namespaces.filter((n) => n.orgId === orgId)),
		});
	});

	on("POST", /^\/v1\/namespaces$/, (ctx) => {
		const b = ctx.body || {};
		const slug = String(b.slug || "").trim();
		// slug 出现在 URL 里，只允许安全的字符集（和前端的校验一致）
		if (!/^[a-z0-9][a-z0-9-]{0,30}$/.test(slug)) {
			return err(400, "slug 只允许小写字母、数字、连字符，且以字母或数字开头");
		}
		const orgId = b.orgId || ORG;
		if (state.namespaces.some((n) => n.orgId === orgId && n.slug === slug)) {
			return err(409, "这个组织里已经有同名 slug 了");
		}
		const ns = {
			id: uid("ns"),
			orgId,
			slug,
			name: String(b.name || slug).trim() || slug,
			createdAt: now(),
		};
		state.namespaces.push(ns);
		return json({ namespace: clone(ns) });
	});

	// ===== 包 =====
	on("GET", /^\/v1\/playbooks$/, (ctx) => {
		const sc = nsScope(ctx.query);
		return json({
			playbooks: inNs(state.playbooks, sc).map((p) => pbView(p, false)),
		});
	});

	on("GET", /^\/v1\/playbooks\/([^/]+)$/, (ctx) => {
		const key = ctx.m[1];
		const pb = state.playbooks.find((p) => p.id === key || p.slug === key);
		if (!pb) return err(404, "包不存在");
		return json({ playbook: pbView(pb, true) });
	});

	// 幂等补齐：能力定义 → 出口 → 触发器，能补什么补什么。
	//
	// "缺节点"补不了：那是机器侧的事（要写 node.yaml 重启 agent），
	// 所以这个接口不碰节点 —— 也正是体检里那一项要单独给 YAML 片段的原因。
	on("POST", /^\/v1\/playbooks\/([^/]+)\/install$/, (ctx) => {
		const key = ctx.m[1];
		const pb = state.playbooks.find((p) => p.id === key || p.slug === key);
		if (!pb) return err(404, "包不存在");
		const res = installPlaybook(pb);
		return json({
			created: res.created,
			skipped: res.skipped,
			playbook: pbView(pb, true),
		});
	});

	// ===== 统计 =====
	on("GET", /^\/v1\/stats$/, (ctx) => {
		const sc = nsScope(ctx.query);
		const evs = inNs(state.events, sc);
		const tks = inNs(state.tasks, sc);
		const ok = tks.filter((t) => t.status === "succeeded").length;
		// 形状与口径都必须与真实 API 对齐（internal/store/stats.go）：
		//   total / today / daily 统计的是**事件**；delivered / deliveryTotal 是终态任务。
		// 早先是任务口径 —— 首页把 KPI 明确成「事件总数/今日事件」后，这里不改就会
		// 出现「总览说事件数、数字却是任务数」的静默漂移（#273 同类问题）。
		const today = new Date().toISOString().slice(0, 10);
		const devices = state.devices || [];
		return json({
			total: evs.length,
			today: evs.filter((e) => {
				const d =
					typeof e.createdAt === "number"
						? new Date(e.createdAt * 1000)
						: new Date(e.createdAt || 0);
				return d.toISOString().slice(0, 10) === today;
			}).length,
			delivered: tks.filter((t) => t.reported).length,
			deliveryTotal: tks.length,
			deviceCount: devices.filter((d) => d.enabled !== false).length,
			// 汇总字段（页面暂未用，但别丢：调试时有用）
			totalEvents: evs.length,
			succeeded: ok,
			failed: tks.filter((t) => t.status === "failed").length,
			running: tks.filter((t) =>
				["assigned", "acked", "running"].includes(t.status),
			).length,
			pending: tks.filter((t) => t.status === "pending").length,
			nodesOnline: state.nodes.filter((n) => n.status === "online").length,
			nodesTotal: state.nodes.length,
			// 热力图 / 趋势药丸：最近 7 天（**本地日历日**）每天的任务数。
			//
			// day 必须与前端同口径：index.html:775-780 的 dayKeyOf 用
			// getFullYear/getMonth/getDate 拼 `YYYY-MM-DD`（本地日），所以这里也用本地日。
			// 早先这里用 `toISOString().slice(0,10)` 取的是 **UTC 日**，UTC+8 下前端算出的
			// today/yesterday 根本查不到 → 趋势药丸退化成「无基线」或拿总数冒充增量。
			//
			// dev-only：今天取**真实**任务数；前 6 天取 max(真实, 按 SHAPE 比例回推)。
			// 为什么要回推：mock 的任务种子几乎全落在「今天」，按真实计数回推的话昨天=0，
			// 首页趋势药丸就会显示「↑ <今日总数>」——那个数字和 KPI 总数一模一样，
			// 看起来像拿总数冒充增量（#KPI 语义）。按比例回推后差值恒小于今日总量，
			// 既保留「7 天有差异」的形状，也不会出现「增量 > 总数」这种假象。
			// 真后端没有这层回推（internal/store/stats.go 只回真实计数）。
			daily: (() => {
				// i 为旧→新下标：0 = 6 天前，6 = 今天。今天固定 1（= 真实数）。
				const SHAPE = [0.4, 0.55, 0.5, 0.7, 0.6, 0.8, 1];
				const dayKeyOf = (d) => {
					const m = String(d.getMonth() + 1).padStart(2, "0");
					const dd = String(d.getDate()).padStart(2, "0");
					return d.getFullYear() + "-" + m + "-" + dd;
				};
				// 真实事件数按本地日聚合（numeric createdAt 是 unix 秒）
				const byDay = new Map();
				for (const e of evs) {
					const d =
						typeof e.createdAt === "number"
							? new Date(e.createdAt * 1000)
							: new Date(e.createdAt || 0);
					if (Number.isNaN(d.getTime())) continue;
					const k = dayKeyOf(d);
					byDay.set(k, (byDay.get(k) || 0) + 1);
				}
				const todayCount = byDay.get(dayKeyOf(new Date())) || 0;
				return Array.from({ length: 7 }, (_, i) => {
					// 按日历日回退（setDate 而不是减 86400e3：后者跨夏令时会漂一天）
					const d = new Date();
					d.setDate(d.getDate() - (6 - i));
					const day = dayKeyOf(d);
					const real = byDay.get(day) || 0;
					const count =
						i === 6
							? real
							: Math.max(real, Math.round(todayCount * SHAPE[i]));
					return { day, count };
				});
			})(),
		});
	});

	// ===== 任务 =====
	on("GET", /^\/v1\/tasks$/, (ctx) => {
		let list = inNs(state.tasks, nsScope(ctx.query));
		if (ctx.query.get("traceId")) {
			list = list.filter((t) => t.traceId === ctx.query.get("traceId"));
		}
		if (ctx.query.get("status")) {
			list = list.filter((t) => t.status === ctx.query.get("status"));
		}
		// nodeId 过滤：真实后端支持（console_handlers.go:96），
		// mock 之前漏了 —— 事件流"按节点筛选"于是永远筛不出东西。
		if (ctx.query.get("nodeId")) {
			const nid = ctx.query.get("nodeId");
			list = list.filter((t) => t.nodeId === nid);
		}
		const limit = Number(ctx.query.get("limit") || 50);
		list.sort((a, b) => b.createdAt - a.createdAt);
		return json({ tasks: clone(list.slice(0, limit)) });
	});

	on("GET", /^\/v1\/tasks\/([^/]+)$/, (ctx) => {
		const id = ctx.m[1];
		const task = state.tasks.find((t) => t.id === id);
		if (!task) return err(404, "任务不存在");
		const chain = state.tasks
			.filter((t) => t.traceId === task.traceId)
			.sort((a, b) => a.createdAt - b.createdAt);
		return json({ task: clone(task), chain: clone(chain) });
	});

	// ===== 产出（emits，P1.6）=====
	//
	// 真后端：`GET /v1/namespaces/{ns}/tasks/{taskId}/emissions`——emissions 是
	// 任务的**子资源**，命名空间长在路径上（不读 namespaceId query）。
	//
	// 三条口径与真后端逐条对齐，mock 才能当契约用：
	//   · 按 seq 升序（页面就是按这个顺序画的）
	//   · 空列表是 `[]` 不是 null（否则前端分不清「还没 emit」与「查不到」）
	//   · 跨命名空间一律 404，文案与「任务不存在」相同（不泄露存在性）
	on("GET", /^\/v1\/namespaces\/([^/]+)\/tasks\/([^/]+)\/emissions$/, (ctx) => {
		const nsId = decodeURIComponent(ctx.m[1]);
		const taskId = decodeURIComponent(ctx.m[2]);
		const task = state.tasks.find((t) => t.id === taskId);
		if (!task || (nsId !== "*" && task.namespaceId !== nsId)) {
			return err(404, "任务不存在");
		}
		const rows = state.emissions
			.filter((e) => e.taskId === taskId)
			.sort((a, b) => a.seq - b.seq);
		return json({ taskId: taskId, namespaceId: nsId, emissions: clone(rows) });
	});

	on("POST", /^\/v1\/tasks\/([^/]+)\/cancel$/, (ctx) => {
		const task = state.tasks.find((t) => t.id === ctx.m[1]);
		if (!task) return err(404, "任务不存在");
		if (!["pending", "assigned", "acked", "running"].includes(task.status)) {
			return err(409, "任务已到终态，不能取消");
		}
		task.status = "canceled";
		task.finishedAt = now();
		return json({ task: clone(task) });
	});

	// ===== 能力模板（CONTRACT-artifact §8 / CONTRACT-protocol §11） =====
	//
	// 只读清单，没有副作用。控制台拿它渲染「选模板」那一步，再按字段
	// 推导 paramsSchema —— 所以这个接口必须和真后端同形状。
	on("GET", /^\/v1\/capability-templates$/, () => json({ templates: mockTemplates() }));

	// ===== 能力与边 =====
	on("GET", /^\/v1\/capabilities$/, (ctx) =>
		json({
			capabilities: inNs(state.capabilities, nsScope(ctx.query)).map(capView),
		}),
	);

	// 最新产物的**解析后**内容（详情页展示脚本用）。
	//
	// 真后端从 artifacts.spec.run 里解出来；mock 的产物就是能力对象上的 run
	// （POST 时写进去的），所以直接投影即可 —— 但**形状必须一致**，否则控制台
	// 在预览里能跑、在真后端会拿不到脚本。
	//
	// 404 语义与真后端一样：“不属于本命名空间”与“没有产物”同一个回答。
	on("GET", /^\/v1\/capabilities\/([^/]+)\/artifact$/, (ctx) => {
		const scope = nsScope(ctx.query);
		const c = state.capabilities.find((x) => x.id === ctx.m[1]);
		if (!c || (scope !== null && c.namespaceId !== scope) || !c.run || !c.run.entrypoint) {
			return json({ error: "capability not found", code: "not_found" }, 404);
		}
		return json({
			digest:
				c.digest ||
				mockDigest(JSON.stringify({ name: c.name, version: c.version, run: c.run })),
			entrypoint: c.run.entrypoint,
			files: c.run.files || {},
			timeoutSec: c.run.timeoutSec || 0,
			workDir: c.run.workDir || "",
			requires: c.requires || [],
		});
	});

	// upgradeBlock 是升级闸门（P2.2）的**唯一**判定处：新版本成为 latest 且删了出口
	// → 返回要 409 出去的 body（带 upgradeImpact），否则返回 null = 放行。
	//
	// 影响面必须真的算出来（变了什么 + 哪几条 link 会断）：控制台的「仍然发布」
	// 按钮就是靠它让人做决定的，只给一句“冲突”等于没给。
	//
	// 只实现 removed_port 一类（删出口）：这是用户实际会踩到的那一条
	// （「出口清空 → 下游全断」），mock 不需要把整套契约比较重写一遍。
	//
	// 单建与批量（POST /v1/capabilities/batch）共用它：两份实现分开写，
	// 迟早会出现“单建拦住、批量却放过去”的不一致。
	function upgradeBlock(c, prev, force) {
		if (force === true) return null;
		if (!prev || c.version <= prev.version) return null;
		const nextPorts = new Set(c.outputs || []);
		const removed = (prev.outputs || []).filter((p) => !nextPorts.has(p));
		if (!removed.length) return null;
		const links = [];
		for (const e of state.edges) {
			if (e.fromIsSource) continue;
			if (!removed.includes(e.fromPort)) continue;
			const from = splitRef(e.fromRef);
			if (from.pinned || from.name !== c.name) continue;
			links.push({
				fromRef: e.fromRef,
				port: e.fromPort,
				toRef: e.toRef,
				reason: '出口 "' + e.fromPort + '" 在新版里被删掉了，这条 link 会断',
			});
		}
		return {
			error: "升级 " + c.name + "@" + c.version + " 会破坏已有的 name 型 link",
			code: "upgrade_breaking",
			upgradeImpact: {
				capability: c.name,
				fromVersion: prev.version,
				toVersion: c.version,
				breaking: true,
				changes: removed.map((p) => ({
					kind: "removed_port",
					detail: '出口 "' + p + '" 在新版里被删掉了',
				})),
				links: links,
			},
		};
	}

	on("POST", /^\/v1\/capabilities$/, (ctx) => {
		const b = ctx.body || {};
		if (!b.name) return err(400, "name is required");
		// 名字规则与 internal/artifact/artifact.go 的 namePattern 一致：
		// 前后端都要拦，不然演示里能建出真后端拒收的名字。
		if (!/^[a-z0-9][a-z0-9._-]{0,62}$/.test(b.name)) {
			return err(400, "invalid_name: 只能包含小写字母、数字、点、下划线、短横线，1–63 字符");
		}
		// 能力归属命名空间（缺省 = 当前范围）。真实后端这里必须是必填，
		// mock 宽松的理由见 nsScope 的注释。
		b.namespaceId = b.namespaceId || nsScope(ctx.query) || NS;
		const existing = state.capabilities.filter((c) => c.name === b.name);
		const nextVer = existing.length
			? Math.max(...existing.map((c) => c.version)) + 1
			: 1;
		// links 先算出来：outputs 的归一要看「这次有没有下游」
		// （没给 outputs 但带了 links = 单出口形态）。
		const links = Array.isArray(b.links) ? b.links : [];
		const c = cap(
			b.name,
			b.version || nextVer,
			normalizeOutputs(b.outputs, links.length > 0),
			b.paramsSchema,
		);
		if (b.displayName) c.displayName = String(b.displayName).trim();
		// sticky / lifecycle 是能力定义的一部分（参与 contentHash）。
		// 控制台发新版本时会把当前值原样带过来，预览里也得真的接住 ——
		// 否则「发一版就把粘性路由弄丢了」在预览里永远复现不了。
		if (typeof b.sticky === "string") c.sticky = b.sticky;
		if (b.lifecycle && typeof b.lifecycle === "object") c.lifecycle = b.lifecycle;
		// balance（契约 §3.3）：{出口: {mode}}，缺省 {} = 每个出口都是 priority。
		// 其它 mode 保存时就 400「尚未实现」——不静默降级（降级会让人以为
		// “选了 all 真的全派了”，而那正是此功能最容易造成的错觉）。
		const balRes = checkBalance(b.balance);
		if (balRes.error) {
			return err(400, balRes.error, balRes.code ? { code: balRes.code } : undefined);
		}
		c.balance = balRes.balance;

		// 升级闸门（P2.2）：新版本成为 latest 且删了出口 → 409 upgrade_breaking，
		// 除非显式传 force:true。判定与影响面计算在 upgradeBlock（单建/批量共用）。
		const prev = existing.length
			? existing.reduce((a, x) => (x.version > a.version ? x : a))
			: null;
		const blocked = upgradeBlock(c, prev, b.force);
		if (blocked) return json(blocked, 409);

		// links：能力 + 下游边**一次提交**（与真后端同语义：要么全成，要么全不成）。
		//
		// mock 没有事务，所以做法是**先全量校验、再统一写入**（见下面的 push 处）：
		// 任何一条不合法就 400，且能力/产物/边一样都不 push —— 真后端回滚的是整个事务，
		// mock 只能靠这个顺序模拟同一个不变量。
		//
		// 错误文案与 Go 侧同形（`links[i]: edge_rejected: …`）：控制台靠下标把错误
		// 映射回「第几条下游 → 哪个目标」，两份实现不能跑偏。
		const preparedLinks = [];
		// 边不合法的 400 body 与真后端同形：{ error, code: "edge_rejected" }
		// （控制台按 code 区分「边不合法」与其它 400，比如 invalid_name）
		const errCode = { code: "edge_rejected" };
		for (let i = 0; i < links.length; i++) {
			const spec = links[i] || {};
			if (!spec.toRef) {
				return err(400, `links[${i}]: edge_rejected: toRef is required`, errCode);
			}
			// 出口：界面上不出现，客户端可以不写（默认就是这个能力唯一的出口 "out"）。
			// 写了就照原样校验 —— 左端恒为**新能力的裸名字**（跟随 latest），
			// 出口必须在这个能力的 outputs 里（不变量 I2）。
			const fromOutput =
				spec.fromPort == null || spec.fromPort === ""
					? SINGLE_OUT
					: String(spec.fromPort).trim();
			if (!(c.outputs || []).includes(fromOutput)) {
				return err(
					400,
					`links[${i}]: edge_rejected: 出口 "${fromOutput}" 不在 ${c.name}@${c.version} 的 outputs ${JSON.stringify(c.outputs || [])} 里`,
					errCode,
				);
			}
			// 自连（出口接自己）：与 Go 侧同一个判据（**按名字**），而且必须在解析**之前**判 ——
			// 此刻这个能力还没进 state，裸名字解析不到，会被误报成「下游不存在」。
			if (splitRef(String(spec.toRef)).name === c.name) {
				return err(
					400,
					`links[${i}]: edge_rejected: self loop ${c.name} --${fromOutput}--> itself`,
					errCode,
				);
			}
			const to = resolveCapRef(spec.toRef, c.namespaceId);
			if (!to) {
				return err(400, `links[${i}]: edge_rejected: to link ref "${spec.toRef}": 下游能力不存在`, errCode);
			}
			const litRes = checkEdgeLiterals(to, spec.literals);
			if (litRes.error) {
				return err(400, `links[${i}]: edge_rejected: ${litRes.error}`, errCode);
			}
			// 配置期不再判环（契约 §2：靠 hop 预算 + trace 任务预算兜底）。
			// 自连上面已经拒了 —— 自己连自己是误操作，不是编排。
			const prioRes = edgePriority(spec.priority);
			if (prioRes.error) {
				return err(400, `links[${i}].priority: ${prioRes.error}`, errCode);
			}
			preparedLinks.push({
				id: uid("edg"),
				namespaceId: c.namespaceId,
				fromIsSource: false,
				// 裸名字 = 跟随 latest（设计 §3.2 默认形态）
				fromRef: c.name,
				fromPort: fromOutput,
				// 右端原样透传：裸名字跟随 latest / name@v 钉死
				toRef: String(spec.toRef).trim(),
				literals: litRes.literals || {},
				// priority：同一 (能力, 出口) 的候选按它**升序**选（小的先选，契约 §3）
				priority: prioRes.priority,
				createdAt: now(),
			});
		}

		// run（新增，CONTRACT-artifact §2）：入口文件 + 文件正文 + 超时 + env。
		// 有它才编译产物、算 digest；没有它就是「只定义了参数契约」的老用法。
		// mock 也走一遍 shebang 校验：控制台的「改脚本」必须能立刻看到这条错。
		const run = b.run || null;
		if (run && run.files) {
			const entry = run.entrypoint || "run.sh";
			const body = String((run.files || {})[entry] || "");
			if (!body.startsWith("#!")) {
				return err(400, "entrypoint_no_shebang: 入口文件必须以 #! 开头（节点直接 exec，不经 shell）");
			}
			c.requires = Array.isArray(b.requires) ? b.requires.slice() : [];
			c.run = {
				entrypoint: entry,
				files: run.files,
				timeoutSec: run.timeoutSec || 0,
				// workDir 属于能力定义（run.workDir，不是部署参数）。
				// 这里以前把它丢了 —— 于是「填了工作目录 → 详情里看不到」在预览里永远复现不了。
				workDir: run.workDir || "",
				env: run.env || {},
			};
			// digest 不存进文件，始终由内容算出来（CONTRACT-artifact §3）
			c.digest = mockDigest(JSON.stringify({ name: c.name, version: c.version, run: c.run }));
			state.artifacts.push({
				id: uid("art"),
				namespaceId: c.namespaceId,
				capabilityId: c.id,
				digest: c.digest,
				createdAt: now(),
			});
		}
		state.capabilities.push(c);
		// 边统一在这一刻写入（见上面「先全量校验、再统一写入」）
		for (const e of preparedLinks) state.edges.push(e);

		const resp = { capability: capView(c), kind: kindOf(c), digest: c.digest || "" };
		if (links.length) {
			// 建成的边（现有 edge 视图）—— 只有带 links 的请求才有这个键
			resp.links = preparedLinks.map(edgeView);
		}
		return json(resp, 201);
	});

	// 批量创建（契约 §4）：一次建多个能力 + 多条边，**要么全成、要么全不成**。
	//
	// mock 没有事务，做法与单建 links 完全一致：先把每个能力、每条边都算清楚、
	// 校验干净，最后才一起 push —— 任一 400/409 时 state.capabilities /
	// state.edges / state.artifacts **一个字节都不变**（真后端回滚的是同一个事务）。
	//
	// 与单建的差别只有三条（契约 §4）：
	//   ① capabilities[].name 批内不可重复；
	//   ② links[].fromRef/toRef 可以引用**本批**名字（它们还没进 state）；
	//   ③ 允许成环（§2：配置期不判环，靠 max_hops / budget 兜底）。
	on("POST", /^\/v1\/capabilities\/batch$/, (ctx) => {
		const b = ctx.body || {};
		const specs = b.capabilities;
		if (!Array.isArray(specs) || !specs.length) {
			return err(400, "capabilities is required");
		}
		// 命名空间规则与单建一致（契约 §4）
		const nsId = b.namespaceId || nsScope(ctx.query) || NS;
		const links = Array.isArray(b.links) ? b.links : [];

		// ① 逐条算新版本 + 校验（顺序与单建对齐：name 规则 → 版本自增 → balance）。
		const prepared = [];
		const byName = new Map();
		for (let i = 0; i < specs.length; i++) {
			const spec = specs[i] || {};
			const name = String(spec.name == null ? "" : spec.name).trim();
			if (!name) return err(400, `capabilities[${i}].name: is required`);
			// 名字规则与 internal/artifact/artifact.go 的 namePattern 一致
			if (!/^[a-z0-9][a-z0-9._-]{0,62}$/.test(name)) {
				return err(
					400,
					`capabilities[${i}].name: invalid_name: 只能包含小写字母、数字、点、下划线、短横线，1–63 字符`,
				);
			}
			if (byName.has(name)) {
				return err(400, `capabilities[${i}].name: duplicate name in batch`, {
					code: "duplicate_name",
				});
			}
			const balRes = checkBalance(spec.balance);
			if (balRes.error) {
				// 前缀 capabilities[i] 是给控制台定位「第几张卡」用的（契约 §3.5）
				return err(
					400,
					`capabilities[${i}]: ${balRes.error}`,
					balRes.code ? { code: balRes.code } : undefined,
				);
			}
			// 版本在同一个命名空间内自增（契约 §4.1；与单建一样按名字找上一版）
			const existing = state.capabilities.filter(
				(x) => x.name === name && (!x.namespaceId || x.namespaceId === nsId),
			);
			const prev = existing.length
				? existing.reduce((a, x) => (x.version > a.version ? x : a))
				: null;
			const nextVer = prev ? prev.version + 1 : 1;
			const c = cap(
				name,
				spec.version || nextVer,
				// 「这次提交里有没有从它出发的 link」按能力名判断（不能整批一概而论：
				// 同一批里另一个能力可能就是终端能力，它的 outputs [] 是用户明说的）。
				normalizeOutputs(
					spec.outputs,
					links.some((l) => splitRef(String((l || {}).fromRef || "")).name === name),
				),
				spec.paramsSchema,
				undefined,
				undefined,
				balRes.balance,
			);
			// cap() 的演示默认落在 NS；批量按请求的命名空间落库
			c.namespaceId = nsId;
			if (spec.displayName) c.displayName = String(spec.displayName).trim();
			if (typeof spec.sticky === "string") c.sticky = spec.sticky;
			if (spec.lifecycle && typeof spec.lifecycle === "object") c.lifecycle = spec.lifecycle;
			c.requires = Array.isArray(spec.requires) ? spec.requires.slice() : [];
			prepared.push({ c: c, spec: spec, prev: prev, artifact: null });
			byName.set(name, c);
		}

		// ② 升级闸门：任一条触发就把**整批**拦下（409 + upgradeImpact），
		//    force: true 越过。判定与影响面计算和单建共用 upgradeBlock。
		for (const p of prepared) {
			const blocked = upgradeBlock(p.c, p.prev, b.force);
			if (blocked) return json(blocked, 409);
		}

		// ③ run（CONTRACT-artifact §2）：与单建同一套校验与派生 ——
		//    入口文件必须以 #! 开头，digest 由内容算出来，命中就多一条产物行。
		for (let i = 0; i < prepared.length; i++) {
			const p = prepared[i];
			const run = p.spec.run || null;
			if (!run || !run.files) continue;
			const entry = run.entrypoint || "run.sh";
			const body = String((run.files || {})[entry] || "");
			if (!body.startsWith("#!")) {
				return err(
					400,
					`capabilities[${i}].run: entrypoint_no_shebang: 入口文件必须以 #! 开头（节点直接 exec，不经 shell）`,
				);
			}
			p.c.run = {
				entrypoint: entry,
				files: run.files,
				timeoutSec: run.timeoutSec || 0,
				workDir: run.workDir || "",
				env: run.env || {},
			};
			// digest 不存进文件，始终由内容算出来（CONTRACT-artifact §3）
			p.c.digest = mockDigest(
				JSON.stringify({ name: p.c.name, version: p.c.version, run: p.c.run }),
			);
			p.artifact = {
				id: uid("art"),
				namespaceId: p.c.namespaceId,
				capabilityId: p.c.id,
				digest: p.c.digest,
				createdAt: now(),
			};
		}

		// ④ links：与单建 links 同一套判据，只多了「可以引用本批名字」。
		//    错误文案带 links[i] 下标（控制台靠它把错映射回卡片/下游/目标）。
		const preparedLinks = [];
		// 边不合法的 400 body 与真后端同形：{ error, code: "edge_rejected" }
		const errCode = { code: "edge_rejected" };
		// 本批解析：裸名字 / name@version 都可以指向本批刚定义的能力
		const resolveBatchRef = (raw) => {
			const s = String(raw == null ? "" : raw).trim();
			if (!s) return null;
			if (byName.has(s)) return byName.get(s);
			const { name, version, pinned } = splitRef(s);
			const c = byName.get(name);
			if (!c) return null;
			return !pinned || c.version === version ? c : null;
		};
		for (let i = 0; i < links.length; i++) {
			const spec = links[i] || {};
			if (!spec.fromRef || !spec.toRef) {
				return err(
					400,
					`links[${i}]: edge_rejected: fromRef and toRef are both required`,
					errCode,
				);
			}
			// 出口默认就是单出口 "out"（界面上不出现，客户端通常不写）；
			// 写了就照原样校验，必须在上游的 outputs 里（不变量 I2）。
			const fromOutput =
				spec.fromPort == null || spec.fromPort === ""
					? SINGLE_OUT
					: String(spec.fromPort).trim();
			const fromCap = resolveBatchRef(spec.fromRef) || resolveCapRef(spec.fromRef, nsId);
			if (!fromCap) {
				return err(
					400,
					`links[${i}]: edge_rejected: fromRef "${spec.fromRef}": 上游能力不存在`,
					errCode,
				);
			}
			if (!(fromCap.outputs || []).includes(fromOutput)) {
				return err(
					400,
					`links[${i}]: edge_rejected: 出口 "${fromOutput}" 不在 ${fromCap.name}@${fromCap.version} 的 outputs ${JSON.stringify(fromCap.outputs || [])} 里`,
					errCode,
				);
			}
			const to = resolveBatchRef(spec.toRef) || resolveCapRef(spec.toRef, nsId);
			if (!to) {
				return err(
					400,
					`links[${i}]: edge_rejected: to link ref "${spec.toRef}": 下游能力不存在`,
					errCode,
				);
			}
			// 自连仍然拒绝（契约 §4.4）：那是误操作，不是编排；成环则放行。
			if (to.name === fromCap.name) {
				return err(
					400,
					`links[${i}]: edge_rejected: self loop ${fromCap.name} --${fromOutput}--> itself`,
					errCode,
				);
			}
			const prioRes = edgePriority(spec.priority);
			if (prioRes.error) {
				return err(400, `links[${i}].priority: ${prioRes.error}`, errCode);
			}
			const litRes = checkEdgeLiterals(to, spec.literals);
			if (litRes.error) {
				return err(400, `links[${i}]: edge_rejected: ${litRes.error}`, errCode);
			}
			preparedLinks.push({
				id: uid("edg"),
				namespaceId: nsId,
				fromIsSource: false,
				// 左端原样保存（裸名字 = 跟随 latest，name@v = 钉死）：意图由调用方决定
				fromRef: String(spec.fromRef).trim(),
				fromPort: fromOutput,
				toRef: String(spec.toRef).trim(),
				literals: litRes.literals || {},
				priority: prioRes.priority,
				createdAt: now(),
			});
		}

		// ⑤ 落库：能力行 → 产物行 → 边行（契约 §4「单事务」的形状）。
		//    走到这里不再有任何校验，所以这三段之间不会留下半成品。
		for (const p of prepared) {
			state.capabilities.push(p.c);
			if (p.artifact) state.artifacts.push(p.artifact);
		}
		for (const e of preparedLinks) state.edges.push(e);

		// 空列表给 []（不是 null），契约 §4
		return json(
			{
				capabilities: prepared.map((p) => capView(p.c)),
				edges: preparedLinks.map(edgeView),
				upgradeImpact: null,
			},
			201,
		);
	});

	// PATCH 只改 display_name（装饰字段），不产生新版本。
	// 真实后端同样如此：它不参与 contentHash（见 store.capHashInput 注释）。
	on("PATCH", /^\/v1\/capabilities$/, (ctx) => {
		const b = ctx.body || {};
		if (!b.id) return err(400, "id is required");
		const c = state.capabilities.find((x) => x.id === b.id);
		if (!c) return err(404, "capability not found");
		c.displayName = String(b.displayName || "").trim();
		return json({ capability: capView(c) });
	});

	// ===== 部署（CONTRACT-protocol §7 / §11） =====
	//
	// depView 按 **store.Deployment 的真实 JSON tag** 输出
	//（internal/store/deployment.go:47）：
	//   id / namespaceId / nodeId / artifactId / capabilityId / capability(名字) /
	//   version / digest / state / reason / createdAt / updatedAt
	//
	// 刻意**不**带 nodeName / capabilityName：真后端只给 id 与 capability 名，
	// 节点名要前端自己用 /v1/nodes 映射。mock 里多给这两个字段会让页面走一条
	// 生产环境不存在的好走的路，等真跑起来才发现名字全是 id。
	const depView = (d) => {
		const c = state.capabilities.find((x) => x.id === d.capabilityId);
		return {
			id: d.id,
			namespaceId: d.namespaceId,
			nodeId: d.nodeId,
			artifactId: d.artifactId || "art_" + String(d.digest || "").slice(7, 15),
			capabilityId: d.capabilityId,
			capability: c ? c.name : d.capabilityId,
			version: c ? c.version : 0,
			digest: d.digest,
			state: d.state,
			reason: d.reason || "",
			createdAt: d.createdAt || d.updatedAt || now(),
			updatedAt: d.updatedAt || now(),
		};
	};

	on("GET", /^\/v1\/deployments$/, (ctx) => {
		const sc = nsScope(ctx.query);
		const capId = ctx.query.get("capabilityId") || "";
		const nodeId = ctx.query.get("nodeId") || "";
		let list = inNs(state.deployments, sc);
		if (capId) list = list.filter((d) => d.capabilityId === capId);
		if (nodeId) list = list.filter((d) => d.nodeId === nodeId);
		// 与后端同样按「最近更新优先」排：面板上刚推的那条应该在最上面
		list = list.slice().sort((a, b) => (b.updatedAt || 0) - (a.updatedAt || 0));
		return json({ deployments: list.map(depView) });
	});

	on("POST", /^\/v1\/deployments$/, (ctx) => {
		const b = ctx.body || {};
		const nsId = b.namespaceId || nsScope(ctx.query) || NS;
		const c = state.capabilities.find((x) => x.id === b.capabilityId);
		if (!c) return err(400, "capability not found");
		const nodeIds = Array.isArray(b.nodeIds) ? b.nodeIds.map(String) : [];
		// 真后端两个选择器取并集（nodeIds ∪ tags）；UI 只发 nodeIds，这里也留 tags 支持，
		// 免得以后后端改了、mock 却没跟上。
		const wantTags = Array.isArray(b.tags) ? b.tags.map(String) : [];
		const picked = state.nodes.filter(
			(n) => nodeIds.includes(n.id) || (wantTags.length && (n.tags || []).some((tg) => wantTags.includes(tg))),
		);
		// 一台都没匹配上 → 400 no_target_node（真后端同款）
		if (!picked.length) {
			return json(
				{ error: "no node matched the selector", code: "no_target_node" },
				400,
			);
		}
		// digest 缺省取该能力最新产物（契约 §11）。没有产物就是「还没编译」——
		// 真后端也会拒；这里给一个确定性假值，好让演示能走完整个面板。
		const digest = b.digest || c.digest || mockDigest(c.name + "@" + c.version);

		const results = [];
		for (const node of picked) {
			const nodeId = node.id;
			// UNIQUE(node_id, artifact_id)：同一台机器再部署同一能力是**更新那一行**
			// （重新排队等节点确认），不是插一条新行 —— 否则列表里会出现重复。
			let row = state.deployments.find(
				(d) => d.nodeId === nodeId && d.capabilityId === c.id,
			);
			if (row) {
				row.digest = digest;
				row.state = "pending";
				row.reason = "";
				row.updatedAt = now();
			} else {
				row = {
					id: uid("dep"),
					namespaceId: nsId,
					nodeId,
					artifactId: "art_" + String(digest).slice(7, 15),
					capabilityId: c.id,
					digest,
					state: "pending",
					reason: "",
					createdAt: now(),
					updatedAt: now(),
				};
				state.deployments.push(row);
			}
			// 节点离线 → **行照样创建**（等它上线由重连对账补推），
			// 但 pushed=false：契约 §11 要求逐台如实回报，不能假装成功。
			// error 文案用后端原词 "node is offline"（internal/ws/node_hub.go:164 的
			// ErrNodeOffline），前端按 /offline/i 判定，别在这里改成 snake_case。
			const online = node.status === "online";
			results.push({
				nodeId,
				nodeName: node.name,
				deployId: row.id,
				pushed: online,
				error: online ? "" : "node is offline",
			});
		}
		// 响应形状对齐真后端：{artifact, deployments:[…]}（不是 {results}）
		return json(
			{
				artifact: {
					id: "art_" + String(digest).slice(7, 15),
					namespaceId: nsId,
					capabilityId: c.id,
					digest,
					spec: { name: c.name, version: c.version },
					createdAt: now(),
				},
				deployments: results,
			},
			200,
		);
	});

	// 撤销：真后端推 remove 帧后**直接删行**（不留 removed 态，契约 §7）
	//
	// 回 **204 空 body**（不是 200+JSON）：契约 §11 没规定状态码，而前端
	// `Anotify.api()` 对 ok 但空 body 的响应会抛 SyntaxError（`res.json()` 解析失败，
	// 且**没有** `.status`）。故意让演示走这条更难的路径 —— 否则「204 也算成功」
	// 那段判据在 mock/预览里永远没人执行，等真后端回 204 才发现是坏的。
	on("DELETE", /^\/v1\/deployments$/, (ctx) => {
		const id = ctx.query.get("id") || "";
		const before = state.deployments.length;
		state.deployments = state.deployments.filter((d) => d.id !== id);
		if (state.deployments.length === before) return err(404, "deployment not found");
		return new Response(null, { status: 204 });
	});

	on("GET", /^\/v1\/edges$/, (ctx) =>
		json({ edges: clone(inNs(state.edges, nsScope(ctx.query))).map(edgeView) }),
	);

	// edgeRefInput 把「用户给的字符串」归一成要存的**引用**（P2.2）。
	//
	// 与 Go 侧 store.resolveLinkRef 同口径：
	//   `name`         → 原样保存（跟随 latest）
	//   `name@version` → 原样保存（钉死）
	//   能力 id         → 转成 `name@version`（老客户端输入兼容，**绝不把 id 存进边**）
	function edgeRefInput(raw, isSource, nsId) {
		const s = String(raw == null ? "" : raw).trim();
		if (!s) return "";
		if (isSource) return s;
		if (resolveCapRef(s, nsId)) return s;
		const byId = state.capabilities.find((c) => c.id === s);
		return byId ? capRef(byId) : s;
	}

	on("POST", /^\/v1\/edges$/, (ctx) => {
		const b = ctx.body || {};
		const nsId = b.namespaceId || NS;
		// 新字段名优先、旧字段名兜底（与 Go 侧 edgeRequest.linkRefs 同口径）
		const fromRaw = b.fromRef || b.fromCapability;
		const toRaw = b.toRef || b.toCapability;

		const toRef = edgeRefInput(toRaw, false, nsId);
		const to = resolveCapRef(toRef, nsId);
		if (!to) return err(400, "下游能力不存在");

		// 常量覆盖（P0-c）：与 Go 侧 store.checkEdgeLiterals 同口径 —— 必须是对象、
		// 键必须在目标的 paramsSchema.properties 里、类型必须精确相等。
		// 配置期就拒掉，别让「拼错的键」等到派发时才静默失效。
		const litRes = checkEdgeLiterals(to, b.literals);
		if (litRes.error) return err(400, "edge_rejected: " + litRes.error);
		const literals = litRes.literals;

		// priority（契约 §1）：同一 (能力, 出口) 的候选按它**升序**选；缺省 0。
		const prioRes = edgePriority(b.priority);
		if (prioRes.error) return err(400, "priority: " + prioRes.error);

		// 入口边（#110）：左端是来源名，出口是事件类型，任意值。
		if (b.fromIsSource) {
			const fromRef = edgeRefInput(fromRaw, true, nsId);
			if (!fromRef) return err(400, "来源名不能为空");
			const e = {
				id: uid("edg"),
				orgId: ORG,
				namespaceId: nsId,
				fromIsSource: true,
				fromRef,
				fromPort: b.fromPort,
				toRef,
				literals,
				priority: prioRes.priority,
				createdAt: now(),
			};
			state.edges.push(e);
			return json({ edge: edgeView(e) }, 201);
		}

		const fromRef = edgeRefInput(fromRaw, false, nsId);
		const from = resolveCapRef(fromRef, nsId);
		if (!from) return err(400, "上游能力不存在");
		// 能力之间的边：fromPort 必须在该能力的 outputs 里（I2）
		if (!(from.outputs || []).includes(b.fromPort)) {
			return err(400, "fromPort 不在该能力的出口集合内");
		}
		const e = {
			id: uid("edg"),
			orgId: ORG,
			// 边一定属于某个命名空间（与后端同形状；漏了会让按命名空间的查询看不见它）
			namespaceId: nsId,
			fromRef,
			fromPort: b.fromPort,
			toRef,
			literals,
			priority: prioRes.priority,
			createdAt: now(),
		};
		state.edges.push(e);
		return json({ edge: edgeView(e) }, 201);
	});

	// 撤销一条边。
	//
	// 真后端按 (from, port, to, fromIsSource) 定位（`?id=` 不在契约里）；
	// 两种都认：有 from/port/to 就按它们匹配，否则退回按 id。
	// （控制台发的是 fromRef/fromPort/toRef —— 只认 id 的话预览里点删除永远是 404。）
	on("DELETE", /^\/v1\/edges$/, (ctx) => {
		const q = ctx.query;
		const id = q.get("id") || "";
		const from = q.get("fromRef") || q.get("fromCapability") || "";
		const to = q.get("toRef") || q.get("toCapability") || "";
		const port = q.get("fromPort") || "";
		const srcFlag = q.get("fromIsSource") || "";
		const wantSource = srcFlag === "1" || srcFlag === "true";
		const hit = state.edges.find((e) =>
			id
				? e.id === id
				: !!e.fromIsSource === wantSource &&
					e.fromRef === from &&
					e.fromPort === port &&
					e.toRef === to,
		);
		if (!hit) return err(404, "边不存在");
		state.edges = state.edges.filter((e) => e !== hit);
		return json({ deleted: hit.id });
	});

	// ===== 节点 =====
	// nodeView 与真实 handler（internal/server/console_handlers.go 的
	// consoleHandler.nodes）保持同形状：
	//
	//   capabilities  对象数组，带 displayName + 推导的 kind
	//   instances     申报的槽位**数量**（整数），不是槽位对象数组
	//   busy          正在占用的槽位数
	//
	// 之前这里直接 clone(state.nodes)，把内部的槽位数组原样吐出去，页面
	// 拿 instances 当数字用就渲染成「槽位 0/[object Object]」。
	function nodeView(n) {
		const slots = n.instances || [];
		return Object.assign(clone(n), {
			capabilities: (n.capabilities || []).map((ref) => Object.assign({}, ref)),
			instances: slots.length,
			busy: slots.filter((x) => (x.busy || 0) > 0).length,
			// agent 自升级（契约 §5.1）：这两个字段**恒存在**。
			// agentVersion 空串 = 老 agent 没在 hello 里上报（页面显示「未知（老版本）」）；
			// agentUpgrade 恒为对象（status 空串 = 从未尝试），别让页面四处判 undefined。
			agentVersion: String(n.agentVersion || ""),
			agentCommit: String(n.agentCommit || ""),
			agentUpgrade: Object.assign(
				{ status: "", targetVersion: "", error: "", at: 0 },
				clone(n.agentUpgrade) || {},
			),
		});
	}

	// 一次性明文令牌（注册节点时下发）。
	//
	// 真后端只把 sha256 存进 nodes.token_hash，明文**只在 201 响应体里出现这一次**，
	// 之后从任何接口都取不回来（契约 §10.3 / D3）。所以 mock 也每次现随机生成、
	// 绝不写进 state —— 否则前端「关掉令牌块就再也拿不到」这件事在预览里是假的。
	// 前缀 n_mock_ 让「这是 mock 造的」一眼可见，不会被误贴进真机的 node.yaml。
	function mockNodeToken() {
		const chunk = () =>
			Math.random().toString(36).slice(2) + Math.random().toString(36).slice(2);
		return ("n_mock_" + chunk() + chunk()).slice(0, 46);
	}

	on("GET", /^\/v1\/nodes$/, (ctx) =>
		json({
			nodes: inNs(state.nodes, nsScope(ctx.query)).map(nodeView),
			// 契约 §5.1：**顶层**目标版本（中枢当前带的 agent 二进制版本）。
			// 空串 = 服务端没带二进制 —— 控制台据此不给可点按钮（而不是点出一个 501）。
			targetAgentVersion: state.agentBinary ? AGENT_VERSION : "",
		}),
	);

	// 注册节点：POST /v1/nodes。
	//
	// 为什么 mock 要和真 handler（internal/server/console_handlers.go 的 createNode）
	// 同形状，而不是「随便回个 200」：前端注册成功后，令牌块与可直接粘的 node.yaml
	// 全靠这个 201 响应体里的 { node, token }。形状对不上就会出现「mock 里跑得通、
	// 接上真后端白屏」——#198 要的正是「纯 UI 把节点跑起来」，预览分支必须可信。
	//
	// 照着真后端抄的三条语义：
	//   1) 令牌只出现一次（见 mockNodeToken）；
	//   2) 名字规则与 store.ValidateNodeName 一致：/^[a-z0-9][a-z0-9._-]{0,62}$/，
	//      空/非法都归 400 invalid_name；**不 trim**（真后端也不 trim，" mbp" 这种
	//      带空白的名字必须当场拒绝，而不是静默改对后落库）；
	//   3) 同命名空间重名 → 409 name_taken（真后端靠 UNIQUE(namespace_id, name)）。
	//
	// instances 是要点：真后端只校验非负、**不落库**，槽位是节点在 hello 帧里申报的。
	// 所以新节点 instances 是空数组，不预创建 N 个槽位——否则页面会显示「这台机器
	// 已经有 2 个槽位」，而实际上根本没有进程连上来。
	on("POST", /^\/v1\/nodes$/, (ctx) => {
		const b = ctx.body || {};
		// 非字符串（数字/对象/缺省）一律当空名：真后端会把这类 JSON body 直接拒掉。
		const name = typeof b.name === "string" ? b.name : "";
		if (name.trim() === "") {
			return err(400, "name 必填（节点名不能为空或全为空白字符）", {
				code: "invalid_name",
			});
		}
		if (!/^[a-z0-9][a-z0-9._-]{0,62}$/.test(name)) {
			return err(
				400,
				"name「" +
					name +
					"」不合法：只能包含小写字母、数字、点（.）、下划线（_）、短横线（-），长度 1..63，且必须以字母或数字开头",
				{ code: "invalid_name" },
			);
		}
		// instances 负数在真 handler 里是 400（不落库但仍校验非负）。
		// 既然上面写了"与真 handler 同形状"，这一条也不能漏。
		if (typeof b.instances === "number" && b.instances < 0) {
			return err(400, "instances must be >= 0");
		}
		// 命名空间：与 mock 其它接口一样宽松，缺省 = 默认命名空间（`namespaceId=*`
		// 是只读汇总语义，写端点落回默认命名空间）。
		const namespaceId = nsScope(ctx.query) || NS;
		if (
			state.nodes.some((n) => n.namespaceId === namespaceId && n.name === name)
		) {
			return err(409, "同名节点已存在", { code: "name_taken" });
		}
		const node = {
			id: uid("nd"),
			orgId: ORG,
			namespaceId: namespaceId,
			name: name,
			// 刚注册还没连上来：offline、没见过、没有槽位。
			status: "offline",
			lastSeenAt: null,
			createdAt: now(),
			// 与既有演示节点同值的硬顶（真后端由 store 的 DefaultMax* 兜底）。
			maxInstances: 20,
			maxConcurrency: 8,
			tags: [],
			capabilities: [],
			instances: [],
			// 刚注册的节点还没连上来，自然也没上报过 agent 版本（契约 §5.1）。
			agentVersion: "",
			agentCommit: "",
			agentUpgrade: { status: "", targetVersion: "", error: "", at: 0 },
		};
		state.nodes.push(node);
		return json({ node: nodeView(node), token: mockNodeToken() }, 201);
	});

	// 节点详情：比列表多给每个槽位的具体情况（哪个槽在跑哪个任务）。
	// 与真实 handler（consoleHandler.nodeByID）同形状。
	on("GET", /^\/v1\/nodes\/([^/]+)$/, (ctx) => {
		const n = state.nodes.find((x) => x.id === ctx.m[1]);
		if (!n) return err(404, "节点不存在");
		// 让"正在跑"有东西可看：把该节点上 running 的任务挂到空闲槽位上。
		// 真实环境里是节点上报的，mock 只能自己推。
		const running = state.tasks.filter(
			(t) => t.nodeId === n.id && t.status === "running" && t.instanceId,
		);
		const instances = (n.instances || []).map((slot, i) => ({
			id: slot.id,
			nodeId: n.id,
			slot: i,
			concurrency: slot.concurrency,
			busyTask: running[i] ? running[i].id : null,
			alive: n.status !== "offline",
			updatedAt: n.lastSeenAt || n.createdAt,
		}));
		return json({ node: nodeView(n), instances });
	});

		on("POST", /^\/v1\/nodes\/([^/]+)\/drain$/, (ctx) => {
		const n = state.nodes.find((x) => x.id === ctx.m[1]);
		if (!n) return err(404, "节点不存在");
		n.status = "draining";
		return json({ node: nodeView(n) });
	});

	// 控制台下发升级：POST /v1/nodes/{id}/upgrade（契约 §5.1）。
	//
	// 四种回包都要能在预览里点到，否则 UI 的失败文案在 demo 里等于不存在：
	//   · 200 up_to_date                  —— 当前版本 == 目标版本（ops-box）
	//   · 409 node_offline                —— 离线节点（ci-runner）
	//   · 501 agent_binary_unavailable    —— 服务端没带二进制（MockApi.noAgentBinary()）
	//   · 200 sent + 延迟 ack             —— 正常下发；ack 成功把 agentVersion 推到目标版本，
	//                                        失败（need_root 等）落成 agentUpgrade.status=failed
	//
	// 端点自己只写 sent；ok/failed 由 upgrade_ack 覆盖（契约 §5.1）。mock 用 setTimeout
	// 演这条回程：600ms 足够页面把「升级中…」画出来，也不至于让 demo 卡着等。
	on("POST", /^\/v1\/nodes\/([^/]+)\/upgrade$/, (ctx) => {
		const n = state.nodes.find((x) => x.id === ctx.m[1]);
		if (!n) return err(404, "节点不存在");
		if (!state.agentBinary) return err(501, "agent_binary_unavailable");
		if (n.status === "offline") return err(409, "node_offline");
		const force = !!(ctx.body && ctx.body.force);
		if (!force && n.agentVersion === AGENT_VERSION) {
			// 同版本不推帧，直接告诉控制台「已是最新」（契约 §5）。
			return json({ status: "up_to_date", targetVersion: AGENT_VERSION });
		}
		n.agentUpgrade = { status: "sent", targetVersion: AGENT_VERSION, error: "", at: now() };
		// old-laptop 固定失败在 need_root（演示“落库后的失败”刷新后仍可见）；
		// 其它节点要演练失败分支就用 MockApi.failUpgrade("bad_sha")。
		const failCode =
			state.upgradeFailCode || (n.id === "nd_mock_legacy" ? "need_root" : "");
		setTimeout(() => {
			if (failCode) {
				n.agentUpgrade = {
					status: "failed",
					targetVersion: AGENT_VERSION,
					error: failCode,
					at: now(),
				};
				return;
			}
			n.agentUpgrade = { status: "ok", targetVersion: AGENT_VERSION, error: "", at: now() };
			n.agentVersion = AGENT_VERSION;
			n.agentCommit = AGENT_COMMIT;
			n.lastSeenAt = now(); // 重启后重新握手，心跳跟着刷新
		}, 600);
		return json({ status: "sent", targetVersion: AGENT_VERSION });
	});

	// ===== 源 =====
	on("GET", /^\/v1\/sources$/, (ctx) =>
		json({
			sources: inNs(state.sources, nsScope(ctx.query)).map(sourceView),
		}),
	);

	// 入口校验：事件 payload 对源声明的 resultSchema（设计 §4.2 / §7 P0.2）。
	//
	// 与后端 route.ValidateEventPayload 同口径（同一个受控子集：required + type
	// 逐层下渗）。mock 是 e2e 的参考实现，两边判定不一致的话 e2e 会直接报到
	// "试运行/真跑的剪枝原因不一样"。
	//
	// 返回 "" = 合规；否则是带字段路径的原因（哪个字段/期望什么/实得什么）。
	function validateEventPayload(schema, payload) {
		if (!schema || typeof schema !== "object" || Array.isArray(schema)) return "";
		const data = payload && typeof payload === "object" ? payload : {};
		return validateValueAt(schema, data, "payload");
	}

	// validateValueAt 是上面那个函数递归用的引擎：type / required / properties / items。
	function validateValueAt(schema, v, path) {
		if (!schema || typeof schema !== "object") return "";
		if (schema.type !== undefined) {
			const wanted = Array.isArray(schema.type) ? schema.type : [schema.type];
			if (!wanted.some((w) => typeMatches(w, v))) {
				return `${path}: want type ${wanted.join("|")}, got ${jsonTypeName(v)}`;
			}
		}
		if (v && typeof v === "object" && !Array.isArray(v)) {
			for (const k of schema.required || []) {
				if (v[k] === undefined) return `${path}: missing required field "${k}"`;
			}
			const props = schema.properties || {};
			for (const k of Object.keys(props)) {
				if (v[k] === undefined) continue;
				const msg = validateValueAt(props[k], v[k], path + "." + k);
				if (msg) return msg;
			}
		}
		if (Array.isArray(v) && schema.items) {
			for (let i = 0; i < v.length; i++) {
				const msg = validateValueAt(schema.items, v[i], `${path}[${i}]`);
				if (msg) return msg;
			}
		}
		return "";
	}

	function jsonTypeName(v) {
		if (v === null || v === undefined) return "null";
		if (Array.isArray(v)) return "array";
		if (typeof v === "number") return "number";
		return typeof v;
	}

	// typeMatches 与后端 typeMatches 对齐（integer 看"是不是整数"，不是看类型名）。
	function typeMatches(want, v) {
		switch (want) {
			case "object":
				return v !== null && typeof v === "object" && !Array.isArray(v);
			case "array":
				return Array.isArray(v);
			case "string":
				return typeof v === "string";
			case "boolean":
				return typeof v === "boolean";
			case "null":
				return v === null || v === undefined;
			case "number":
				return typeof v === "number";
			case "integer":
				return typeof v === "number" && Number.isInteger(v);
			default:
				return false; // 未知 type = 不匹配（与后端一致：声明错了就看得见）
		}
	}

	// resultSchemaError 是配置期校验：与后端 store.ResultSchemaError 同口径。
	//
	// 返回 "" = 合法。规则两条：
	//   1. 必须是 JSON 对象（数组/标量不是 schema）；
	//   2. 显式 null 合法 = "没有契约"（PATCH 用它清空）。
	function resultSchemaError(cfg) {
		if (!cfg || typeof cfg !== "object" || !("resultSchema" in cfg)) return "";
		const v = cfg.resultSchema;
		if (v === null || v === undefined) return "";
		if (typeof v !== "object" || Array.isArray(v)) {
			return 'config.resultSchema 必须是 JSON 对象（如 {"type":"object","required":["title"]}）';
		}
		return "";
	}

	// mergeResultSchema 是 PATCH 语义下的合并：
	//   没带这个键 → 继承旧值（否则控制台改个 cron 表达式就把用户写的契约抹掉了）；
	//   显式 null → 清除；
	//   对象 → 覆盖。
	// 与 Go 侧 store/server 的 inheritedConfigKeys 是同一套语义。
	function mergeResultSchema(oldSchema, bodyCfg) {
		const cfg = bodyCfg || {};
		if (!Object.prototype.hasOwnProperty.call(cfg, "resultSchema")) return oldSchema;
		const v = cfg.resultSchema;
		return v === null || v === undefined ? undefined : v;
	}

	// 参数校验：与后端 route.ValidateParams 同语义（mock 也要拦住，
	// 否则前端会因为"怎么填都能过"而看不出真实约束）
	function validateParams(schema, params) {
		if (!schema) return "";
		const props = schema.properties || {};
		const required = schema.required || [];
		for (const k of required) {
			if (params[k] === undefined || params[k] === null || params[k] === "") {
				return 'params: missing required field "' + k + '"';
			}
		}
		for (const [k, spec] of Object.entries(props)) {
			const v = params[k];
			if (v === undefined) continue;
			if (Array.isArray(spec.enum) && spec.enum.length && !spec.enum.includes(v)) {
				return `params.${k}: value ${v} not in enum [${spec.enum.join(" ")}]`;
			}
			if (spec.type === "integer" && !Number.isInteger(v)) {
				return `params.${k}: expected integer`;
			}
			if (spec.type === "number" && typeof v !== "number") {
				return `params.${k}: expected number`;
			}
			if (spec.type === "boolean" && typeof v !== "boolean") {
				return `params.${k}: expected boolean`;
			}
			if (spec.type === "string" && typeof v !== "string") {
				return `params.${k}: expected string`;
			}
			if (spec.minimum !== undefined && typeof v === "number" && v < spec.minimum) {
				return `params.${k}: ${v} < minimum ${spec.minimum}`;
			}
			if (spec.maximum !== undefined && typeof v === "number" && v > spec.maximum) {
				return `params.${k}: ${v} > maximum ${spec.maximum}`;
			}
		}
		return "";
	}

	// ---------- 边的常量覆盖（P0-c，设计 §3.3）----------
	//
	// 语义：进入下游的 params = data ⊎ literals，**字面量优先**，不做任何类型转换。
	// 与 Go 侧 store.checkEdgeLiterals / route.applyLiterals 同口径：
	//   ① 必须是 JSON 对象（null / undefined = 没有覆盖）
	//   ② 每个键必须出现在目标能力 paramsSchema.properties 里（拼错当场拒）
	//   ③ 值类型必须与声明精确相等（integer 不收 string，反之亦然）

	// jsonTypeName 给人看的类型名（错误文案与 Go 侧同形）。
	function jsonTypeName(v) {
		if (v === null) return "null";
		if (Array.isArray(v)) return "array";
		if (typeof v === "number") return "number";
		if (typeof v === "object") return "object";
		return typeof v;
	}

	// jsonTypeMatches 判定值是否属于某个 JSON Schema type（受控子集）。
	function jsonTypeMatches(want, v) {
		switch (want) {
			case "object":
				return v !== null && typeof v === "object" && !Array.isArray(v);
			case "array":
				return Array.isArray(v);
			case "string":
				return typeof v === "string";
			case "boolean":
				return typeof v === "boolean";
			case "null":
				return v === null;
			case "number":
				return typeof v === "number";
			case "integer":
				return typeof v === "number" && Number.isInteger(v);
		}
		return false;
	}

	// checkEdgeLiterals 校验并归一化一条边的常量覆盖。
	// 返回 { literals } 或 { error }（error 文案与后端一致，便于对照排查）。
	function checkEdgeLiterals(cap, literals) {
		if (literals === undefined || literals === null) return { literals: {} };
		if (typeof literals !== "object" || Array.isArray(literals)) {
			return { error: "literals must be a JSON object, got " + jsonTypeName(literals) };
		}
		const keys = Object.keys(literals);
		if (!keys.length) return { literals: {} };
		const props = (cap.paramsSchema && cap.paramsSchema.properties) || {};
		for (const k of keys.sort()) {
			if (!Object.prototype.hasOwnProperty.call(props, k)) {
				const declared = Object.keys(props).sort().join(" ");
				return {
					error:
						"literals." + k + ": " + cap.name + "@" + cap.version +
						" params_schema has no such field (declared: " + declared + ")",
				};
			}
			const spec = props[k];
			if (!spec || typeof spec !== "object" || spec.type === undefined) continue;
			const wanted = Array.isArray(spec.type) ? spec.type : [spec.type];
			if (wanted.some((w) => jsonTypeMatches(w, literals[k]))) continue;
			return {
				error:
					"literals." + k + ": want type " + wanted.join("|") +
					", got " + jsonTypeName(literals[k]),
			};
		}
		return { literals };
	}

	// checkBalance 校验并归一化能力版本上的 balance（契约 §3.3）。
	//
	// 形状：{出口: {"mode": "priority"}}；缺省（或空对象）= 每个出口都是 priority。
	// **本期只实现 priority**：all / random / least_loaded / hash 一律 400「尚未实现」，
	// 带上 code: "balance_unimplemented" 让前端能只拦住那一种错。
	// 不允许静默降级成 priority —— 用户选了 all 却只派了一个，比报错更伤人。
	function checkBalance(raw) {
		if (raw === undefined || raw === null) return { balance: {} };
		if (typeof raw !== "object" || Array.isArray(raw)) {
			return { error: "balance must be a JSON object, got " + jsonTypeName(raw) };
		}
		const out = {};
		for (const output of Object.keys(raw).sort()) {
			const spec = raw[output];
			// 出口写了但没写 mode（或给的不是对象）——当作缺省 priority。
			const mode = spec && typeof spec === "object" && !Array.isArray(spec) ? String(spec.mode || "priority") : "priority";
			if (mode !== "priority") {
				return {
					error: "balance[" + output + "]: mode " + mode + " 尚未实现",
					code: "balance_unimplemented",
				};
			}
			out[output] = { mode: "priority" };
		}
		return { balance: out };
	}

	// edgePriority 归一化一条边的 priority（契约 §1）：缺省 0，必须是非负整数。
	//
	// 数值**小的先选**（与 Go 侧 edges.priority 升序同口径）；负数 / 小数 / 非数字
	// 一律 400 —— 默默当 0 会让“我把它调成 -5 想插队”变成一个看不出来的谎言。
	function edgePriority(raw) {
		if (raw === undefined || raw === null || raw === "") return { priority: 0 };
		const n = typeof raw === "number" ? raw : Number(String(raw).trim());
		if (!Number.isInteger(n) || n < 0) {
			return {
				error: "must be a non-negative integer, got " + JSON.stringify(raw),
			};
		}
		return { priority: n };
	}

	// edgeSourceLiterals 取「这条 link 上挂的常量」（mock 的源路径用它）。
	//
	// **mock 的自觉简化**：Go 里 link 是 edges 行，源走自己的 targets（source_targets，
	// 常量写在 target.params 里）、入口边是另一条派发路径（POST /v1/events）。
	// mock 只有「源 → 目标」这一条派发路径，所以这里把**入口边**
	// （fromIsSource + 来源名）上的常量也叠进来——否则「在出口表上给某条边加了常量」
	// 这件事在演示里完全看不见。多条匹配时按 edges 顺序合并（后者覆盖前者）。
	function edgeSourceLiterals(ns, sourceName, toCapabilityId) {
		const out = {};
		for (const e of state.edges) {
			if (e.namespaceId !== ns || !e.fromIsSource) continue;
			if (e.fromRef !== sourceName) continue;
			// 右端是**引用**：比的是它此刻解析到的能力（带名字或 @版本 都行）
			const toCap = resolveCapRef(e.toRef, ns);
			if (!toCap || toCap.id !== toCapabilityId) continue;
			const lit = e.literals;
			if (!lit || typeof lit !== "object" || Array.isArray(lit)) continue;
			Object.assign(out, lit);
		}
		return out;
	}

	// 归一化 targets：后端同时接受 ["cap_a"] 与 [{capabilityId,params}]
	function normTargets(list) {
		return (list || [])
			.map((x) => (typeof x === "string" ? { capabilityId: x } : x))
			.filter((x) => x && x.capabilityId);
	}

	// ---------- 字段映射（params 模板）----------
	//
	// 一个出口的 params 里，每个值可以是两种形态：
	//
	//   字面量       "env": "prod"                     → 直接就是 prod
	//   映射         "title": { "from": "pr.title" }   → 从 payload 按路径取值
	//
	// **I6：这里不做表达式求值。**
	// 规则只有一条：按点号路径取一个已存在的值。没有拼接、没有运算、
	// 没有条件、没有函数调用。取不到时用 default（没有 default 就整字段不传）。
	//
	// 为什么坚持不引入表达式：中枢一旦求值用户提供的字符串，就多出
	// 模板注入 / 无限递归 / 超大表达式 DoS 三类攻击面，而收益只是"少一次
	// 节点侧拼装"。节点本来就能自己取数据（它才是知道数据形状的那一方）。

	// ---------- 模板引用 ----------
	//
	//   "{{pull_request.title}}"   → 取 payload.pull_request.title
	//   "PR #{{number}}: {{title}}" → 模板拼接
	//   "prod"                     → 字面量，原样传
	//
	// 支持**混写**（模板拼接）：文本里可以出现多个引用。
	//
	// 为什么这是安全的（I6「中枢不解释用户提供的逻辑」的边界）：
	//
	//   单趟扫描  → 逐个 {{x}} 替换 → **不再把替换结果当模板扫描**
	//
	// 危险的是"替换后再解析"（模板引擎的递归展开），那才能：
	//   - 嵌套/无限展开（{{a}} 展开出 {{b}} 展开出 ...）
	//   - 调用函数（{{toUpper(x)}}）
	//   - 访问外部资源
	// 单趟替换没有其中任何一条：它本质上是 `strings.Replace`，
	// 用户提供的是"文本 + 几个取值点"，不是"一段要被执行的程序"。
	//
	// 仍然**不做**的（这些才需要真正的解释器）：
	//   条件 {{#if x}}、循环 {{#each}}、函数调用、运算 {{a + b}}
	//
	// 占位符正则只允许「标识符 + 点号」，不允许括号、运算符、空格分隔的
	// 表达式 —— 这样即使有人写了 {{a + b}}，也不会被当成表达式求值，
	// 而是当成"查一个叫 `a + b` 的路径"（查不到 → param_missing）。
	// **语法上就没有运算的位置**，这是 I6 的结构性保证，不靠运行时检查。
	const PLACEHOLDER_RE = /\{\{\s*([A-Za-z_][A-Za-z0-9_.]*)\s*\}\}/g;

	// hasPlaceholder 字符串里是否含至少一个 {{x}}。
	function hasPlaceholder(v) {
		if (typeof v !== "string") return false;
		PLACEHOLDER_RE.lastIndex = 0;
		const found = PLACEHOLDER_RE.test(v);
		PLACEHOLDER_RE.lastIndex = 0;
		return found;
	}

	// wholeRef 取出"整个值就是一个引用"里的那个引用；不是则返回 null。
	function wholeRef(v) {
		if (typeof v !== "string") return null;
		const m = v.match(/^\{\{\s*([A-Za-z_][A-Za-z0-9_.]*)\s*\}\}$/);
		return m ? m[1] : null;
	}

	// renderTemplate 单趟替换。
	//
	// lookup(ref) 返回 {found, val}。任何取不到值的占位符都记进 missing，
	// **并且原样保留 {{x}} 文本**（让用户在报错里看到自己写了什么）。
	//
	// 关键：替换进去的值**不会被再次扫描**。用一次性遍历 + 拼接实现，
	// 而不是"替换后再 match 一遍"。
	function renderTemplate(str, lookup) {
		const missing = [];
		const out = str.replace(PLACEHOLDER_RE, (whole, ref) => {
			const r = lookup(ref);
			if (!r || !r.found) {
				missing.push(ref);
				return whole; // 原样留着，不丢信息
			}
			// 字符串化：对象/数组用 JSON，其它用 String。
			// 这里刻意不做 HTML/JSON 转义——值是要喂给能力的原始数据，
			// 中转擅自转义会让「路径里本来就有引号」变两次。
			if (r.val === null) return "";
			if (typeof r.val === "object") return JSON.stringify(r.val);
			return String(r.val);
		});
		PLACEHOLDER_RE.lastIndex = 0;
		return { text: out, missing };
	}
	// coerceValue 按目标类型转换一个求值结果；转不了返回 undefined（上层剪枝）。
	//
	// 为什么需要：整值引用保留原类型（数字还是数字），但混写模板
	// 一定是字符串。如果目标字段声明 integer，字符串 "31" 应该能转成
	// 31（模板拼出来的数字很正常），而 "abc" 转不了 —— 那种情况必须
	// 剪枝报错，不能静默传一个类型不对的值下去。
	function coerceValue(v, wantAs) {
		if (!wantAs) return v;
		if (wantAs === "integer" || wantAs === "number") {
			if (typeof v === "number") return v;
			if (typeof v === "string" && v.trim() !== "" && !Number.isNaN(Number(v))) {
				return Number(v);
			}
			// boolean/object 转数字没有意义，但也别一律拒绝：
			// true/false 在 JSON 里等价 1/0，是常见写法
			if (typeof v === "boolean") return v ? 1 : 0;
			return undefined;
		}
		if (wantAs === "boolean") {
			if (typeof v === "boolean") return v;
			if (v === "true" || v === 1) return true;
			if (v === "false" || v === 0) return false;
			return undefined;
		}
		if (wantAs === "string") {
			if (typeof v === "string") return v;
			if (v === null || v === undefined) return undefined;
			if (typeof v === "object") return JSON.stringify(v);
			return String(v);
		}
		// array / object：只能接受已经是该形状的值
		if (wantAs === "array") return Array.isArray(v) ? v : undefined;
		if (wantAs === "object") {
			return v && typeof v === "object" && !Array.isArray(v) ? v : undefined;
		}
		return v;
	}

	// schemaPropType 取某个字段在 params_schema 里声明的类型（取不到返回 undefined）。
	//
	// 参数值可能来自模板求值，类型是"求出来才知道"的：同一个
	// {{pull_request.additions}} 既可能是数字也可能被拼进字符串。
	// 目标字段需要什么类型，只能问 schema。
	function schemaPropType(cap, field) {
		const sc = cap && cap.paramsSchema;
		if (!sc || typeof sc !== "object") return undefined;
		const props = sc.properties;
		if (!props || typeof props !== "object") return undefined;
		const p = props[field];
		return p && typeof p === "object" ? p.type : undefined;
	}

	// getPath 按点号路径从对象取值（"a.b.0.c"），取不到返回 undefined。
	//
	// 只用 hasOwnProperty 判断存在性：`payload.constructor` / `__proto__`
	// 这类键不能从原型链上取到值（否则一个精心构造的 payload 能读到
	// Object.prototype 上的东西）。
	function getPath(obj, path) {
		if (!obj || typeof obj !== "object") return undefined;
		let cur = obj;
		for (const seg of String(path).split(".")) {
			if (cur == null) return undefined;
			if (Array.isArray(cur)) {
				const i = Number(seg);
				if (!Number.isInteger(i) || i < 0 || i >= cur.length) return undefined;
				cur = cur[i];
			} else if (typeof cur === "object") {
				if (!Object.prototype.hasOwnProperty.call(cur, seg)) return undefined;
				cur = cur[seg];
			} else {
				return undefined;
			}
		}
		return cur;
	}

	// resolveParams 把一份带映射的 params 按 payload 求值成实际参数。
	//
	// 返回 {params, used, missing}：missing 是"声明了映射但 payload 里没有、
	// 也没有 default"的字段名。上游（触发响应）会把它报出去，让用户知道
	// 哪个字段没取到值——静默丢掉是最难查的一种失败。
	function resolveParams(template, payload) {
		const out = {};
		const used = [];
		const missing = [];
		if (!template) return { params: out, used, missing };

		// lookup 解析一个引用：只有一种语法 —— 按点号路径从 payload 取。
		const lookup = (ref) => {
			const got = getPath(payload, String(ref).trim());
			return got === undefined ? { found: false } : { found: true, val: got };
		};

		for (const [k, v] of Object.entries(template)) {
			// ① 整个值就是一个引用 —— **保留原始类型**。
			//    "{{pull_request.additions}}" 拿到的是数字 77，不是 "77"。
			//    这对 integer/number 参数很重要：字符串 "77" 会被 schema 判错。
			const whole = wholeRef(v);
			if (whole) {
				const r = lookup(whole);
				if (r.found) {
					out[k] = clone(r.val);
					used.push({ field: k, from: "{{" + whole + "}}" });
				} else {
					missing.push({ field: k, from: "{{" + whole + "}}" });
				}
				continue;
			}

			// ② 含占位符的字符串 —— 单趟替换，结果一律是字符串。
			if (hasPlaceholder(v)) {
				const r = renderTemplate(v, lookup);
				out[k] = r.text;
				if (r.missing.length) {
					for (const m of r.missing) {
						missing.push({ field: k, from: "{{" + m + "}}" });
					}
				} else {
					used.push({ field: k, from: v });
				}
				continue;
			}

			// ③ 其余都是字面量（数字、布尔、对象…）—— 原样传。
			//
			// 已经没有 {from}/{var} 这种对象形态了：引用统一写成 {}
			// 里的路径文本。少一套语法，也少一处"两种写法怎么选"的困惑。
			out[k] = clone(v);
		}
		return { params: out, used, missing };
	}

	// validateMappingPaths 校验映射声明本身（配置期，不是触发期）。
	// 只查形式：from 必须是点号路径字符串，as 只能是 string/number/boolean/json。
	// validatePlaceholders 校验参数里的占位符**形式**（配置期）。
	//
	// 只查形式不查可达性：{{pull_request.title}} 无需任何声明就合法，
	// 拼错会在触发时以 param_missing 报出来（并原样回显用户写的引用）。
	function validatePlaceholders(template) {
		if (!template) return "";
		for (const [k, v] of Object.entries(template)) {
			if (typeof v !== "string") continue;
			// 闭合检查要**先于** hasPlaceholder：写了 "PR {{title" 这种
			// 半截占位符时，正则匹配不到任何完整占位符，hasPlaceholder
			// 返回 false，那个分支根本进不来 —— 用户会以为设置生效了。
			const opens = (v.match(/\{\{/g) || []).length;
			const closes = (v.match(/\}\}/g) || []).length;
			if (opens !== closes) {
				return `参数 ${k}：占位符没有闭合（{{ 与 }} 数量不一致）`;
			}
			if (!hasPlaceholder(v)) continue;
			PLACEHOLDER_RE.lastIndex = 0;
			let m;
			while ((m = PLACEHOLDER_RE.exec(v)) !== null) {
				const ref = m[1].trim();
				if (ref.includes("..") || ref.startsWith(".") || ref.endsWith(".")) {
					return `参数 ${k} 的路径不合法：「${ref}」`;
				}
			}
			PLACEHOLDER_RE.lastIndex = 0;
		}
		return "";
	}

	function validateTargets(targets) {
		const byID = new Map(state.capabilities.map((c) => [c.id, c]));
		for (const t of targets) {
			const c = byID.get(t.capabilityId);
			if (!c) return "出口能力 " + t.capabilityId + " 不存在";
			if (t.params && Object.keys(t.params).length) {
				// 先校验映射声明本身（形式对不对），再校验字面量的 schema。
				// 映射的值要触发时才有，这里不能拿它去做 schema 校验。
				const mmsg = validatePlaceholders(t.params);
				if (mmsg) return `${c.name}@${c.version}：${mmsg}`;
				const literals = {};
				for (const [k, v] of Object.entries(t.params)) {
					// 含占位符的值（无论是整值引用还是混写）都不是字面量：
					// 它的实际值要等触发时才知道，配置期不能拿字符串去校验
					// "expected integer"。混写一定产出字符串，所以它的类型
					// 由 "这个参数能不能收字符串" 决定 —— 那个判断在触发时
					// 由 coerceValue 做（转不了就 param_type 剪枝）。
					if (hasPlaceholder(v)) continue;
					literals[k] = v;
				}
				if (Object.keys(literals).length) {
					const msg = validateParams(c.paramsSchema, literals);
					if (msg) return `${c.name}@${c.version} 的参数不合法：${msg}`;
				}
			}
		}
		return "";
	}

	on("POST", /^\/v1\/sources$/, (ctx) => {
		const b = ctx.body || {};
		if (!b.name || !b.name.trim()) return err(400, "name is required");
		if (!["cron", "webhook", "github", "manual"].includes(b.kind)) {
			return err(400, "kind 必须是 cron / webhook / github / manual", {
				available: ["cron", "webhook", "github", "manual"],
			});
		}
		const targets = normTargets(b.targets);
		if (!targets.length) {
			return err(400, "targets 至少要选一个出口能力（否则这个源产生的事件永远派不出去）");
		}
		if (state.sources.some((s) => s.name === b.name.trim())) {
			return err(409, "同名源已存在，换个名字");
		}
		let cfg = b.config || {};

		const bad = validateTargets(targets);
		if (bad) return err(400, bad);

		// 事件 payload 契约的形式校验：必须是 JSON 对象（显式 null = 没有契约）。
		// 与后端 store.ResultSchemaError 同口径——mock 放过非对象的话，e2e 会
		// 以为"这能在真环境保存"，而真实后端会 400。
		const schemaErr = resultSchemaError(b.config);
		if (schemaErr) return err(400, schemaErr);

		if (b.kind === "cron") {
			const expr = (cfg.expr || "").trim();
			if (!expr) return err(400, "cron 需要 expr（五段表达式，如 0 3 * * *）");
			if (expr.split(/\s+/).length !== 5) {
				return err(400, "cron 表达式需要 5 段，得到 " + expr.split(/\s+/).length + " 段");
			}
			// 不再有 config.payload：它是"cron 也能带数据"的历史补丁，
			// 和常量直写参数重复。cron 的常量写在参数里就行。
			cfg = { expr: expr, tz: cfg.tz || "Local" };
		} else if (b.kind === "webhook" || b.kind === "github") {
			// github 就是"知道载荷长什么样"的 webhook：token/投递路径完全相同
			cfg = {
				token: cfg.token || "src_mock" + Math.random().toString(36).slice(2, 12),
			};
		} else {
			cfg = {};
		}

		// resultSchema 与 kind 无关（入口校验对每种源都生效）：上面按 kind 重建 cfg
		// 时会把用户刚写的契约丢掉，这里单独放回（显式 null = 不存这个键）。
		const rs = mergeResultSchema(undefined, b.config);
		if (rs !== undefined) cfg.resultSchema = rs;

		const s = {
			id: uid("src"),
			orgId: ORG,
			name: b.name.trim(),
			kind: b.kind,
			config: cfg,
			enabled: b.enabled !== false,
			lastFiredAt: null,
			nextFireAt: b.kind === "cron" ? now() + 3600 : null,
			createdAt: now(),
			targets: targets,
		};
		state.sources.push(s);
		return json({ source: sourceView(s) }, 201);
	});

	on("GET", /^\/v1\/sources\/([^/]+)$/, (ctx) => {
		const s = state.sources.find((x) => x.id === ctx.m[1]);
		if (!s) return err(404, "源不存在");
		return json({ source: sourceView(s) });
	});

	on("PATCH", /^\/v1\/sources\/([^/]+)$/, (ctx) => {
		const s = state.sources.find((x) => x.id === ctx.m[1]);
		if (!s) return err(404, "源不存在");
		const b = ctx.body || {};
		if (b.kind && b.kind !== s.kind) {
			return err(400, "update source: kind 不可修改（当前 " + s.kind + "）");
		}
		if (b.name !== undefined && b.name.trim()) {
			if (state.sources.some((x) => x.id !== s.id && x.name === b.name.trim())) {
				return err(409, "同名源已存在，换个名字");
			}
			s.name = b.name.trim();
		}
		if (b.targets && b.targets.length) {
			const targets = normTargets(b.targets);
			const bad = validateTargets(targets);
			if (bad) return err(400, bad);
			s.targets = targets;
		}
		if (b.config !== undefined) {
			const schemaErr = resultSchemaError(b.config);
			if (schemaErr) return err(400, schemaErr);
			// resultSchema 的合并语义：没带 → 继承旧值，显式 null → 清空。
			// 与 Go 侧 inheritedConfigKeys 一套口径（改个 cron 表达式不该把契约抹掉）。
			const rs = mergeResultSchema(s.config && s.config.resultSchema, b.config);
			if (s.kind === "cron") {
				const expr = (b.config.expr || "").trim();
				if (expr.split(/\s+/).length !== 5) {
					return err(400, "cron 表达式需要 5 段");
				}
				s.config = {
					expr: expr,
					tz: b.config.tz || "Local",
				};
			} else {
				// webhook / github / manual：config 整体替换（token 由服务端补）
				s.config = Object.assign({}, b.config);
			}
			if (rs !== undefined) s.config.resultSchema = rs;
			else delete s.config.resultSchema;
		}
		if (b.enabled !== undefined) s.enabled = !!b.enabled;
		return json({ source: sourceView(s) });
	});

	on("DELETE", /^\/v1\/sources\/([^/]+)$/, (ctx) => {
		const i = state.sources.findIndex((x) => x.id === ctx.m[1]);
		if (i < 0) return err(404, "源不存在");
		state.sources.splice(i, 1);
		return json({ deleted: ctx.m[1] });
	});

	// recordEventSchemaPrune 把入口剪枝写进 mock 的事件流。
	//
	// 与 Go 侧 source.Trigger.recordEventSchemaPrune 同形状：source='route'、
	// type='pruned.<reason>'、payload=PruneEventPayload（fromCapability/port/detail）、
	// 与事件同一个 traceId。否则"收到了但没派"在事件流/trace 里完全看不到，
	// 用户只能猜——而那正是 #142 与设计 §4.2 要消灭的东西。
	//
	// 只在**真触发**的两条路（webhook 投递 / 手动 fire）里调：试运行不落库，
	// 用户每试一次都多一条假历史是不能接受的（与 Go 侧 DryRun 不写剪枝同理）。
	function recordEventSchemaPrune(source, traceId, eventType, pruned) {
		for (const p of pruned || []) {
			if (p.reason !== "event_schema") continue;
			state.events.push({
				id: uid("evt"),
				orgId: ORG,
				namespaceId: source.namespaceId || NS,
				traceId: traceId,
				source: "route",
				type: "pruned.event_schema",
				payload: {
					fromCapability: source.name,
					port: eventType,
					detail: p.detail,
				},
				createdAt: now(),
			});
		}
	}

	// 手动触发：真的往 state 里加事件 + 任务，页面上能看到链长出来
	// planDispatch 算出"这条配置会不会真的跑起来"——**不产生任何副作用**。
	//
	// fire 和「试运行」共用它：fire 拿结果去建任务，试运行只展示。
	// 抽出来是因为两边必须给出**一致**的判定，否则用户会看到
	// "试运行说能跑，一点真的跑却剪枝了"这种最伤信任的差异。
	//
	// 返回 {tasks, pruned}：
	//   tasks  —— 会派发的出口（含求值后的参数）
	//   pruned —— 被跳过的出口及原因（event_schema / no_capability / max_hops /
	//             budget / param_missing / param_schema / param_type / no_node）
	//
	// event_schema 是**入口级**的：与逐出口的那几种不同，它一旦命中就一条都不派
	// （事件形状不对，不存在"这条出口能过"），所以直接返回空 tasks + 单条剪枝
	// ——与 Go 侧 source.Trigger.Fire 在调 Dispatch 之前拦下是同一形状。
	//
	// traceId：只给 budget 预算用（契约 §2「同一 trace_id 已建任务数 >= budget」）。
	// 试运行没有 trace 可算，传空即可（已建任务数为 0 → 不剪；想验预算就改
	// window.MockApi.state.limits.budget）。
	function planDispatch(source, payload, traceId) {
		const tasks = [];
		const pruned = [];

		const schema = source && source.config ? source.config.resultSchema : undefined;
		if (schema !== undefined && schema !== null) {
			const msg = validateEventPayload(schema, payload);
			if (msg) {
				return {
					tasks: [],
					pruned: [
						{
							reason: "event_schema",
							capabilityId: null,
							capability: null,
							detail: `payload 不符合源「${source.name}」的 resultSchema：${msg}`,
						},
					],
				};
			}
		}

		for (const t of source.targets) {
			const c = state.capabilities.find((x) => x.id === t.capabilityId);
			if (!c) {
				pruned.push({
					reason: "no_capability",
					capabilityId: t.capabilityId,
					capability: null,
					detail: "能力不存在（可能已被删除）",
				});
				continue;
			}

			// ① hop 预算（契约 §2）：配置期不再判环，深度就是这个预算兜的。
			//    口径与真后端一致：depth = len(入参 path)（子任务 path = 父 path + 本能力名，
			//    形状不变，所以入口第一跳的 depth 是 0），depth >= maxHops 就剪。
			const depth = payload && Array.isArray(payload.path) ? payload.path.length : 0;
			if (depth >= state.limits.maxHops) {
				pruned.push({
					reason: "max_hops",
					capabilityId: c.id,
					capability: null,
					detail: `depth ${depth} >= maxHops ${state.limits.maxHops}`,
				});
				continue;
			}

			// ② 本次 trace 的任务预算（契约 §2）：同一 traceId 已建任务数达到预算，
			//    后续出口一律剪掉 —— 环上无法收敛的编排就是在这里停下来的。
			const used = traceId ? state.tasks.filter((t2) => t2.traceId === traceId).length : 0;
			if (used >= state.limits.budget) {
				pruned.push({
					reason: "budget",
					capabilityId: c.id,
					capability: null,
					detail: `trace 任务数 ${used} >= budget ${state.limits.budget}`,
				});
				continue;
			}

			const resolved = resolveParams(t.params, payload);

			// 类型推断：含占位符的值按 schema 转（整值引用保留原类型，
			// 混写一定是字符串但目标字段可能是 integer）
			for (const [k, v] of Object.entries(resolved.params)) {
				const decl = (t.params || {})[k];
				const wantAs = hasPlaceholder(decl) ? schemaPropType(c, k) : undefined;
				const converted = coerceValue(v, wantAs);
				if (converted === undefined) {
					pruned.push({
						reason: "param_type",
						capabilityId: c.id,
						capability: c.name,
						field: k,
						detail: `参数 ${k} 期望 ${wantAs}，但求值得到 ${JSON.stringify(v)}，转不了`,
					});
					resolved.params = null;
					break;
				}
				resolved.params[k] = converted;
			}
			if (resolved.params === null) continue;

			for (const m of resolved.missing) {
				pruned.push({
					reason: "param_missing",
					capabilityId: c.id,
					capability: c.name,
					field: m.field,
					detail: `参数 ${m.field} 取不到 ${m.from}（payload 里没有这个字段）`,
				});
			}
			if (resolved.missing.length) continue;

			// 常量覆盖（P0-c）：字面量优先（同名键覆盖求值结果），**不做类型转换**。
			// 位置很关键：覆盖必须发生在 validateParams **之前**，这样类型写错的常量
			// 照样被 params_schema 挡住（配置期已拒过一道，这是第二道）。
			const lits = edgeSourceLiterals(source.namespaceId || NS, source.name, c.id);
			if (Object.keys(lits).length) Object.assign(resolved.params, lits);

			const msg = validateParams(c.paramsSchema, resolved.params);
			if (msg) {
				pruned.push({
					reason: "param_schema",
					capabilityId: c.id,
					capability: c.name,
					detail: `${c.name}@${c.version} params_schema: ${msg}`,
				});
				continue;
			}
			const node = state.nodes.find(
				(n) => n.status === "online" && (n.capabilities || []).some((x) => x.id === c.id),
			);
			if (!node) {
				pruned.push({
					reason: "no_node",
					capabilityId: c.id,
					capability: c.name,
					detail: `没有在线节点声明 ${c.name}@${c.version}`,
				});
				continue;
			}
			tasks.push({
				capabilityId: c.id,
				capability: c.name,
				version: c.version,
				params: resolved.params,
				node: node.name,
			});
		}
		return { tasks, pruned };
	}

	// Webhook 投递：外部系统 POST 到 /v1/hooks/{token}。
	//
	// 与 Go 侧同构，两种格式靠 kind 区分：
	//   github  —— GitHub 原生格式：body 就是 payload，事件名在
	//              X-GitHub-Event 头里（mock 用 body.__event 模拟，因为
	//              浏览器里没法设自定义头）
	//   webhook —— 本系统包装体 {"type","payload"}
	on("POST", /^\/v1\/hooks\/([^/]+)$/, (ctx) => {
		const token = ctx.m[1];
		const s = state.sources.find(
			(x) => (x.kind === "webhook" || x.kind === "github") && x.config && x.config.token === token,
		);
		if (!s) return err(404, "not found");
		if (!s.enabled) return err(404, "not found");

		const b = ctx.body || {};
		let payload;
		let eventType;
		if (s.kind === "github") {
			// 原生格式：整个 body 就是 payload
			eventType = b.__event || (s.config && s.config.event) || "pull_request";
			payload = Object.assign({}, b);
			delete payload.__event;
		} else {
			eventType = b.type || s.name;
			payload = b.payload && typeof b.payload === "object" ? b.payload : {};
		}

		const traceId = nextTraceId();
		const ev = {
			id: uid("evt"),
			orgId: ORG,
			traceId: traceId,
			source: s.name,
			type: eventType,
			payload: payload,
			createdAt: now(),
		};
		state.events.push(ev);

		// traceId 传下去是为了 budget 预算（同一 trace 已建任务数）
		const plan = planDispatch(s, payload, traceId);
		// 入口剪枝也留痕（与后端 InsertPruneEvent 对齐）：事件已入库，
		// 这里只多一条 pruned.event_schema 说明"为什么没派"。
		recordEventSchemaPrune(s, traceId, eventType, plan.pruned);
		const tasks = [];
		for (const p of plan.tasks) {
			const c = state.capabilities.find((x) => x.id === p.capabilityId);
			const tk = {
				id: nextTaskId(), orgId: ORG, traceId: traceId,
				parentTaskId: null, path: [p.capabilityId],
				capabilityId: p.capabilityId, capability: p.capability,
				params: p.params, version: p.version,
				status: "pending", nodeId: null, instanceId: null,
				leaseUntil: null, attempts: 0, out: null, reported: false,
				summary: "", artifacts: [], error: "",
				contentHash: c.contentHash, capHash: c.contentHash,
				graphId: "grf_mock", createdAt: now(),
				startedAt: null, finishedAt: null, notBefore: null,
			};
			state.tasks.push(tk);
			tasks.push(tk);
		}
		s.lastFiredAt = now();
		return json({ event: clone(ev), inserted: true, dispatch: { tasks: clone(tasks), pruned: plan.pruned } });
	});

	// 试运行：用给定 payload 算出"会派发什么、会剪枝什么"，**不落库**。
	//
	// 和 fire 共用 planDispatch，所以两边判定一定一致 —— 用户不会遇到
	// "试运行说能跑，真点却剪枝"这种最伤信任的差异。
	// 草稿试运行：弹窗里还没保存的配置也能先验一遍。
	// 请求体带 { source: {...}, payload: {...} }，与 /{id}/dry-run 共用 planDispatch。
	on("POST", /^\/v1\/sources\/dry-run$/, (ctx) => {
		const b = ctx.body || {};
		const draft = b.source;
		if (!draft || !Array.isArray(draft.targets)) return err(400, "source is required");
		// 草稿的 payload 契约也先过形式校验：与保存路径同一口径
		// （否则又会出现"试运行说能跑、点保存却 400"）。
		const schemaErr = resultSchemaError(draft.config);
		if (schemaErr) return err(400, schemaErr);
		const payload = b.payload && typeof b.payload === "object" ? b.payload : {};
		const plan = planDispatch(draft, payload);
		return json({ enabled: true, dispatch: plan });
	});

	on("POST", /^\/v1\/sources\/([^/]+)\/dry-run$/, (ctx) => {
		const s = state.sources.find((x) => x.id === ctx.m[1]);
		if (!s) return err(404, "源不存在");
		const b = ctx.body || {};
		const payload = b.payload && typeof b.payload === "object" ? b.payload : {};
		const plan = planDispatch(s, payload);
		return json({
			payload: clone(payload),
			enabled: !!s.enabled,
			dispatch: plan,
		});
	});

	on("POST", /^\/v1\/sources\/([^/]+)\/fire$/, (ctx) => {
		const s = state.sources.find((x) => x.id === ctx.m[1]);
		if (!s) return err(404, "源不存在");
		if (!s.enabled) return err(400, "source: " + s.name + " 已停用");

		const b = ctx.body || {};
		const traceId = nextTraceId();
		const ev = {
			id: uid("evt"),
			orgId: ORG,
			traceId: traceId,
			source: s.name,
			type: b.type || s.name,
			payload: b.payload || {},
			createdAt: now(),
		};
		state.events.push(ev);

		// 判定逻辑与「试运行」同一份实现
		const plan = planDispatch(s, ev.payload, traceId);
		recordEventSchemaPrune(s, traceId, ev.type, plan.pruned);
		const tasks = [];
		for (const p of plan.tasks) {
			const c = state.capabilities.find((x) => x.id === p.capabilityId);
			const tk = {
				id: nextTaskId(),
				orgId: ORG,
				traceId: traceId,
				parentTaskId: null,
				path: [p.capabilityId],
				capabilityId: p.capabilityId,
				capability: p.capability,
				params: p.params,
				version: p.version,
				status: "pending",
				nodeId: null,
				instanceId: null,
				leaseUntil: null,
				attempts: 0,
				out: null,
				reported: false,
				summary: "",
				artifacts: [],
				error: "",
				contentHash: c.contentHash,
				capHash: c.contentHash,
				graphId: "grf_mock",
				createdAt: now(),
				startedAt: null,
				finishedAt: null,
				notBefore: null,
			};
			state.tasks.push(tk);
			tasks.push(tk);
		}
		s.lastFiredAt = now();

		if (tasks.length) {
			setTimeout(() => {
				for (const tk of tasks) {
					tk.status = "succeeded";
					tk.nodeId = state.nodes[0].id;
					tk.instanceId = "nins_mock_0";
					tk.startedAt = now();
					tk.finishedAt = now() + 1;
					tk.reported = true;
					tk.attempts = 1;
					tk.summary = "已完成（mock）";
				}
			}, 1200);
		}

		return json({
			event: clone(ev),
			inserted: true,
			idempotent: false,
			dispatch: { tasks: clone(tasks), pruned: plan.pruned },
		});
	});

	// ===== 事件流 =====
	on("GET", /^\/v1\/console\/events$/, (ctx) => {
		const limit = Number(ctx.query.get("limit") || 50);
		const trace = ctx.query.get("traceId");
		// source 过滤：参数编辑器用它拿"这个源最近一条真实事件"当预览样本，
		// 让用户能看到 {{pull_request.title}} 到底会取到什么，而不是盲写路径。
		const source = ctx.query.get("source");
		let list = inNs(state.events, nsScope(ctx.query));
		if (trace) list = list.filter((e) => e.traceId === trace);
		if (source) list = list.filter((e) => e.source === source);
		list.sort((a, b) => b.createdAt - a.createdAt);
		return json({ events: clone(list.slice(0, limit)) });
	});

	on("POST", /^\/v1\/events$/, (ctx) => {
		const b = ctx.body || {};
		const ev = {
			id: uid("evt"),
			orgId: ORG,
			namespaceId: b.namespaceId || NS,
			traceId: nextTraceId(),
			source: b.source || "manual",
			type: b.type || "manual.run",
			payload: b.payload || {},
			createdAt: now(),
		};
		state.events.push(ev);
		return json({ event: clone(ev), inserted: true, dispatch: { tasks: [], pruned: [] } });
	});

	// ===== API Keys =====
	on("GET", /^\/v1\/keys$/, () => json({ keys: clone(state.keys) }));

	on("POST", /^\/v1\/keys$/, (ctx) => {
		const b = ctx.body || {};
		const k = {
			id: uid("key"),
			orgId: ORG,
			name: b.name || "未命名",
			scopes: b.scopes || ["send"],
			prefix: "ant_mock_" + Math.random().toString(36).slice(2, 10),
			createdAt: now(),
			lastUsedAt: null,
			// keys.html:756 归一化 `k.enabled !== false && k.enabled !== 0`；真后端 keyPublic
			// 必带 enabled:boolean，新建记录同样要把语义写全（缺字段只能靠前端兜底）。
			enabled: true,
			// revoked / orgId 保留：其它 mock 分支（revoke、nsScope）还在按这两个字段过滤
			revoked: false,
		};
		state.keys.push(k);
		// 对齐真后端 internal/server/handlers.go:295（HTTP 200）：
		//   { ok: true, key: "<明文串>", record: {…} } —— key 是**明文字符串**，record 是记录本体。
		// 前端 keys.html:1047 直接读 data.key 当字符串用（NEW_KEY_PLAIN = data.key || data.plaintext），
		// 早先 mock 把记录对象塞进 key、明文放 plaintext，页面上「明文」就成了一坨对象。
		// 明文只返回一次（与真实后端一致），沿用既有构造：prefix + "_" + 28 个 x。
		const plaintext = k.prefix + "_" + "x".repeat(28);
		return json({ ok: true, key: plaintext, record: clone(k) }, 200);
	});

	on("POST", /^\/v1\/keys\/([^/]+)\/revoke$/, (ctx) => {
		const k = state.keys.find((x) => x.id === ctx.m[1]);
		if (!k) return err(404, "key 不存在");
		// 真后端 RevokeKey 把 enabled 置 0（store/auth.go:396），列表里的 keyPublic
		// 于是回 enabled:false。只改 revoked 的话，刷新后这枚 Key 又变回「启用」。
		k.revoked = true;
		k.enabled = false;
		return json({ ok: true });
	});

	on("DELETE", /^\/v1\/keys\/([^/]+)$/, (ctx) => {
		const i = state.keys.findIndex((x) => x.id === ctx.m[1]);
		if (i < 0) return err(404, "key 不存在");
		state.keys.splice(i, 1);
		return json({ deleted: ctx.m[1] });
	});

	// ===== 设备（推送订阅）=====
	on("GET", /^\/v1\/devices$/, () => json({ devices: clone(state.devices) }));

	// ===== 测试通知 =====
	on("POST", /^\/v1\/test-notify$/, (ctx) => {
		const b = ctx.body || {};
		const ev = {
			id: uid("evt"),
			orgId: ORG,
			traceId: nextTraceId(),
			source: "manual",
			type: "test.notify",
			payload: { title: b.title, body: b.body },
			createdAt: now(),
		};
		state.events.push(ev);
		return json({ ok: true, event: clone(ev) });
	});

	// ===== 安全 / passkey / 会话 =====
	on("GET", /^\/v1\/auth\/passkeys$/, () => json({ passkeys: [] }));
	on("GET", /^\/v1\/auth\/sessions$/, () =>
		json({
			sessions: [
				{
					id: "sess_mock_current",
					deviceName: "Chrome on Linux",
					username: "demo",
					createdAt: now() - 3600,
					expiresAt: now() + 86400 * 7,
					lastSeen: now() - 60,
					current: true,
				},
			],
		}),
	);
	on("DELETE", /^\/v1\/auth\/sessions\/([^/]+)$/, () => json({ ok: true }));
	// 对齐真后端 internal/server/auth_handlers.go:546：返回 { unusedCount, hasCodes }。
	// security 页（web-src/pages/security.html:1833-1843）只读 data.unusedCount（number）
	// 来显示剩余数量；早先 mock 给的是 {enabled, remaining, total} → 读不到 → 页面只能
	// 走 recoveryDemo 分支显示「无法加载恢复码状态」。
	on("GET", /^\/v1\/auth\/recovery\/status$/, () =>
		json({ unusedCount: 8, hasCodes: true }),
	);
	// 对齐真后端 internal/server/auth_handlers.go:527（HTTP 200）：
	//   { ok: true, recoveryCodes: [...] } —— 明文字串数组只在这一次返回。
	// security 页（web-src/pages/security.html:1966）读的就是 `data.recoveryCodes`；
	// 早先 mock 回的是 `{ codes: [...] }` → 页面拿到空数组，重新生成后列表空白。
	on("POST", /^\/v1\/auth\/recovery\/regenerate$/, () =>
		json({
			ok: true,
			recoveryCodes: Array.from({ length: 10 }, () => "MOCK-CODE-" + Math.random().toString(36).slice(2, 8)),
		}),
	);

	// ===== 管理后台 =====
	on("GET", /^\/v1\/admin\/stats$/, () =>
		json({
			users: 3,
			orgs: 1,
			nodes: state.nodes.length,
			events: state.events.length,
			tasks: state.tasks.length,
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
					username: "demo",
					deviceName: "Chrome on Linux",
					createdAt: now() - 3600,
					expiresAt: now() + 86400 * 7,
					lastSeen: now() - 60,
				},
			],
		}),
	);
	on("GET", /^\/v1\/admin\/messages$/, () => json({ messages: [] }));

	// ===== CLI 设备授权 =====
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

	// ===== passkey 注册流程 =====
	on("POST", /^\/v1\/auth\/register\/options$/, () => err(400, "mock 模式不支持真实 passkey 注册"));
	on("POST", /^\/v1\/auth\/login\/options$/, () => err(400, "mock 模式不支持真实 passkey 登录"));

	// ---------- fetch 拦截 ----------
	const origFetch = window.fetch.bind(window);

	window.fetch = async function (input, init) {
		let url;
		let method = (init && init.method) || "GET";
		if (typeof input === "string") {
			url = input;
		} else if (input instanceof Request) {
			url = input.url;
			method = input.method || method;
		} else {
			url = String(input);
		}

		// 绝对 URL 只取 pathname + search 判断（同源才拦）
		let path = url;
		let query = new URLSearchParams();
		try {
			const u = new URL(url, location.href);
			if (u.origin !== location.origin && !/^https?:/.test(url)) {
				// 相对路径：已由 base 解析，继续
			}
			path = u.pathname + u.search;
			query = u.searchParams;
		} catch (e) {
			/* 保持原样 */
		}

		const pathOnly = path.split("?")[0];
		if (!pathOnly.startsWith("/v1/")) {
			return origFetch(input, init);
		}

		// 解析 body
		let body = null;
		if (init && init.body) {
			try {
				body = typeof init.body === "string" ? JSON.parse(init.body) : null;
			} catch (e) {
				body = null;
			}
		}

		// 300ms 左右的延迟：让 loading 态能被看到，也避免请求瞬间返回
		// 导致页面闪烁得"不像真的"
		await sleep(120 + Math.random() * 180);

		// ?mockFail=1 可以强制看错误态（调试用）
		if (query.get("mockFail") === "1") {
			return err(500, "mock 强制失败（?mockFail=1）");
		}

		for (const r of routes) {
			if (r.method !== method.toUpperCase()) continue;
			const m = r.pattern.exec(pathOnly);
			if (!m) continue;
			try {
				return r.handler({ m, query, body, path: pathOnly });
			} catch (e) {
				console.error("[mock] handler 抛错", r.pattern, e);
				return err(500, "mock handler error: " + e.message);
			}
		}

		// 没实现的接口：明确说出来，而不是静默返回 {} 让页面莫名其妙地空。
		//
		// 这条 warn 的**措辞是门禁契约的一部分**：scripts/e2e 的 fe-preview 套件只在
		// console **文本**里搜 "mock 未实现" 这个子串，用它判定「页面调的接口 mock 有没有实现」。
		// 所以这里**不能加 "[mock] " 前缀** —— `"[mock] 未实现：…".includes("mock 未实现")` 是 false
		//（`] ` 卡在中间），断言会永远绿。旧文案 "[mock] 未实现的接口:" 也是同一个毛病；
		// 501 响应体里的那行是对的，但响应体从不进 console（Anotify.api 只 throw，页面也不打日志）。
		console.warn("mock 未实现：" + method + " " + pathOnly);
		return err(501, "mock 未实现：" + method + " " + pathOnly);
	};

	// ---------- 预览提示条 ----------
	//
	// 让人一眼知道"这是假数据"，否则很容易误以为后端坏了。
	//
	// 位置改为**左下角**小卡片（早先是底部居中的黑影胶囊，会压在页面底部内容上）。
	// 颜色全走 web/tokens.css 的主题 token，不再硬编码 rgba/hex：
	// 旧的 #fbbf24 在亮色主题（--surface-1 接近白）上对比不足，且切主题时不变。
	// ≤639px 时左右贴边（8px），贴底占满一行，不居中遮挡内容。
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
		// 关过一次就永不再挂（刷新也不再复现）
		if (bannerDismissed()) return;
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

	// 挂到 window 上方便调试：MockApi.state / MockApi.reset()
	window.MockApi = {
		state,
		reset: () => location.reload(),
		capByName,
		// 升级演示控制（契约 §5.1）——两个都只改内存，刷 F5 就回初始值。
		//
		// noAgentBinary()：服务端没带 agent 二进制（传 false 恢复）。节点页版本照旧，
		//   但升级控件变成「服务端没带 agent 二进制」（不给可点按钮）。
		//   ⚠️ 生效时机是**下一次拉列表**（点页头「刷新」），不需要也不需要 F5——
		//   F5 会重建整个 mock state，这个开关就被丢掉了。
		noAgentBinary: (on) => {
			state.agentBinary = !(on === undefined ? true : on);
			return state.agentBinary;
		},
		// failUpgrade("need_root"|"bad_sha"|"download_failed"|"not_supported")：
		//   让下一次 upgrade_ack 失败；传空串恢复正常。
		failUpgrade: (code) => {
			state.upgradeFailCode = code || "";
			return state.upgradeFailCode;
		},
	};
	mountBanner();

	console.info(
		"[mock] 前端预览模式：所有 /v1/* 请求由本地 mock 应答。\n" +
			"  - 数据存在 window.MockApi.state，可改\n" +
			"  - 任意接口加 ?mockFail=1 可看错误态\n" +
			"  - 升级失败演练：MockApi.failUpgrade('bad_sha') / 无二进制：MockApi.noAgentBinary()\n" +
			"  - 刷新页面恢复初始数据",
	);
})();
