/**
 * mock.js —— 纯前端预览用的 API 拦截层。
 *
 * 为什么需要它：
 *   看前端效果不该先把 server、tunnel、devseed 示例数据、WS worker 全跑起来。
 *   那条链路对"我只想看看页面长什么样"来说太重了，而且任何一步失败
 *   （端口被占、tunnel 不通、worker 掉线）看到的都是"页面空着"。
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
	const ME = {
		id: "usr_mock_demo",
		username: "demo",
		displayName: "demo",
		createdAt: now() - 86400 * 30,
	};

	// ---------- 演示数据 ----------
	//
	// 刻意做得像真的：有扇出、有分叉的 out、有失败与剪枝，
	// 这样看页面时能看出各种状态长什么样，而不是一排"成功"。

	function cap(name, version, outputs, paramsSchema, source, displayName) {
		return {
			id: "cap_mock_" + name.replace(/\./g, "_") + "_v" + version,
			orgId: ORG,
			name: name,
			// 纯展示标签，空串 = 没取（前端回落到 name@version）
			displayName: displayName || "",
			version: version,
			outputs: outputs,
			paramsSchema: paramsSchema || { type: "object" },
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
			// 中枢执行的终端能力
			cap("notify.phone", 1, [], { type: "object" }, "builtin", "推送通知"),
			// 用户能力：有参数、有分叉
			cap(
				"analyze-pr",
				1,
				["needs-fix", "approved"],
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
				["fixed"],
				{
					type: "object",
					properties: { scope: { type: "string", enum: ["lint", "test", "all"] } },
				},
				"custom",
				"应用修复",
			),
			// outputs 里刻意留一个没接边的端口（preview.ready）：
			// 流向图要能显示"这条出口走不通"，那是最容易被忽略的配置遗漏
			// outputs 里刻意留一个没接边的端口（manual.approval）：
			// 流向图要能显示"这条出口走不通"，那是最容易被忽略的配置遗漏。
			cap(
				"deploy-preview",
				1,
				["failed", "preview.ready", "manual.approval"],
				{
				type: "object",
				required: ["env"],
				properties: { env: { type: "string", enum: ["staging", "prod"] } },
			}, "custom", "部署预发"),
			cap(
				"report.send",
				1,
				["escalate"],
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
				["passed", "failed"],
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
				["clean", "remaining"],
				{
				type: "object",
				properties: { scope: { type: "string" } },
			}, "custom", "清理构建缓存"),
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
				["done"],
				{
				type: "object",
				properties: { reason: { type: "string" } },
			}, "custom", "回滚"),
			cap(
				"alert.oncall",
				1,
				[],
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
				["archived"],
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
		keys: [],
		devices: [],
	};

	// 辅助：按名字取能力（返回最高版本）
	function capByName(name) {
		const list = state.capabilities.filter((c) => c.name === name);
		if (!list.length) return null;
		return list.reduce((a, b) => (b.version > a.version ? b : a));
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
	// 这种用户能力因为 source 标记而被误标成 origin —— 它有 escalate
	// 出口、也有节点声明，按规则该是 pipe。

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
		const fromSource = (source, port, to) => ({
			id: uid("edg"),
			orgId: ORG,
			fromIsSource: true,
			fromCapability: source,
			fromPort: port,
			toCapability: capByName(to).id,
			toCapabilityName: to,
			createdAt: now() - 3600,
		});
		const from = (name, port, to) => ({
			id: uid("edg"),
			orgId: ORG,
			fromCapability: capByName(name).id,
			fromCapabilityName: name,
			fromPort: port,
			toCapability: capByName(to).id,
			toCapabilityName: to,
			createdAt: now() - 3600,
		});
		state.edges = [
			// 源也可以扇出：一个 PR 事件同时派发分析和通知两条链。
			// 这两条链互不等待（不做 join），各走各的。
			fromSource("github", "pull_request.opened", "analyze-pr"),
			fromSource("github", "pull_request.opened", "report.send"),
			fromSource("github", "pull_request.synchronize", "analyze-pr"),
			fromSource("cron", "nightly", "analyze-pr"),
			fromSource("http", "webhook.incoming", "deploy-preview"),
			fromSource("manual", "manual.run", "deploy-preview"),
			from("analyze-pr", "needs-fix", "lint-fix"),
			from("analyze-pr", "approved", "run-tests"),

			// 两个分支各自往下走一步，然后在 deploy-preview 汇聚 ——
			// 汇聚是布局最容易画歪的形状（菱形），值得在演示数据里出现。
			from("lint-fix", "clean", "run-tests"),
			from("lint-fix", "remaining", "report.send"),
			from("run-tests", "passed", "deploy-preview"),

			// ===== 扇出（一个出口多条边）=====
			//
			// 这是模型的核心形状之一（wiki 02「一个出口可以有多条边」）：
			// 拿 (review@1, needs-fix) 查表返回 2 行 → 建 2 个任务，
			// 它们的 parent_task_id 是同一个。**不是择一，是全部下发。**
			//
			// 之前的 mock 完全没有扇出，等于把产品最重要的形状藏起来了。
			from("analyze-pr", "needs-fix", "run-tests"),
			from("apply-fix", "fixed", "deploy-preview"),

			// 测试挂了先尝试自动修，修完再测一次 —— 这是本仓库真实存在的
			// 环（run-tests → apply-fix → run-tests）。流向图必须画出环路，
			// 而"全是 DAG"的演示数据会让人以为这功能只能处理简单情况。
			from("run-tests", "failed", "apply-fix"),

			// 部署之后：成功就发通知 + 生成变更日志（一对多分叉）
			from("deploy-preview", "preview.ready", "gen-changelog"),
			from("deploy-preview", "failed", "rollback"),

			// 一条真实存在的环：改了再测，测完再改。
			// 留着它是刻意的：流向图必须能画出环路，而"全是 DAG"的演示数据
			// 会让人以为这功能只能处理简单情况。
			from("apply-fix", "fixed", "run-tests"),

			// 兜底告警：report.send 和 rollback 都能触发值班
			from("rollback", "done", "alert.oncall"),
			from("report.send", "escalate", "alert.oncall"),
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
			// displayName 也带上：节点页要显示用户取的中文名。
			// kind 不在这里算 —— 它依赖"有没有节点声明"，而这段代码正在
			// 构造 state.nodes，算出来会是半成品。放到请求时算（nodeView）。
			return {
				id: c.id,
				name: c.name,
				version: c.version,
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
			},
		];
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
			id: uid("tsk"),
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

			// 逐跳串起来：每一跳的 out 决定下一跳是谁
			let prev = mkTask({
				traceId, cap: analyze, createdAt: t,
				startedAt: t + 1, finishedAt: t + 5,
				out: "needs-fix", summary: "发现 3 处 lint 问题",
			});
			state.tasks.push(prev);

			const steps = [
				{
					cap: lintFix, out: "clean", summary: "自动修复 2 处，1 处需人工",
					start: t + 5, end: t + 6.4, node: state.nodes[0].id,
				},
				{
					cap: tests, out: "passed", summary: "12 个用例全部通过",
					// 测试常跑在 CI 机器上，顺便让"按节点筛选"在演示数据里看得出差异
					start: t + 6.4, end: t + 18.6, node: state.nodes[1].id,
				},
				{
					cap: deploy, out: "preview.ready", summary: "预览环境已部署",
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
		//     │              └─ run-tests(passed) ──────────────┐（analyze 的 needs-fix 扇出 2 条）
		//     │                            └→ deploy-preview ─┬─ gen-changelog
		//     │                                                └─ alert.oncall
		//     └─ report.send（源扇出：同一个事件同时派发通知链）
		//
		// 同时含三种真实形状，一张图能验证完：
		//   ① 源扇出     —— 一个事件派两条互不等待的链
		//   ② 出口扇出   —— analyze-pr 的 needs-fix 有 2 条边 → 2 个子任务
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

			// ---- 根：analyze-pr（报告 needs-fix，出口上有 2 条边）----
			const a = add("analyze-pr", null, 0, 3.8, {
				out: "needs-fix", summary: "27 个文件，发现 6 处问题",
			});

			// ---- 扇出第 1 支：lint-fix → run-tests(失败) ----
			const lintFix = add("lint-fix", a, 3.9, 2.1, {
				out: "remaining", summary: "自动修复 4 处，2 处需人工",
			});
			const testFail = add("run-tests", lintFix, 6.1, 34.2, {
				out: "failed", status: "failed", node: 1,
				error: "e2e: 3 specs timed out (30s)",
			});
			// 失败后走环：改 → 再测（第二次带重试标记）
			const fix = add("apply-fix", testFail, 40.4, 12.6, {
				out: "fixed", node: 1, summary: "人工修复完成",
			});
			const testPass = add("run-tests", fix, 53.1, 41.3, {
				out: "passed", node: 1, attempts: 2, summary: "全部 218 个用例通过",
			});

			// ---- 扇出第 2 支：analyze-pr 的 needs-fix 还直接连了 run-tests ----
			// （和上面那支同时开始 —— 这是"同一个出口两条边各派一个任务"的直接后果）
			const testDirect = add("run-tests", a, 3.9, 96.0, {
				out: "passed", node: 1, summary: "全量回归通过",
			});

			// ---- 汇聚（不是 join，只是两条分支都指向同一个下游能力）----
			const deploy = add("deploy-preview", testPass, 94.5, 18.4, {
				out: "preview.ready", node: 1, params: { env: "staging" },
				summary: "staging 已就绪",
			});
			add("gen-changelog", deploy, 113, 1.2, { summary: "生成 14 条变更" });
			add("alert.oncall", deploy, 113, 0.3, { summary: "值班已确认" });

			// ---- 源扇出的另一支：通知链，与上面完全并行 ----
			add("report.send", null, 0.2, 0.9, {
				out: "escalate", params: { level: "warn", page: true },
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
					["analyze-pr", "approved", "succeeded", 5.1, 0, "无问题"],
					["run-tests", "passed", "succeeded", 22.7, 1, "218 用例通过"],
					["deploy-preview", "preview.ready", "succeeded", 15.2, 1, "staging 就绪"],
				]},
				{ src: "每晚体检", type: "nightly", plan: [
					["analyze-pr", "approved", "succeeded", 2.2, 0, "基线正常"],
					["run-tests", "passed", "succeeded", 9.4, 0, "冒烟通过"],
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
		//   analyze-pr ─needs-fix→ lint-fix ─remaining→ report.send ─escalate→ alert.oncall
		//        │                      └─clean→ （剪枝：run-tests 不被派发）
		//        └─approved→ run-tests ─failed→ apply-fix ─fixed→ run-tests（第 2 次，成功）
		//                                              └────passed→ deploy-preview ─failed→ rollback ─done→ alert.oncall
		//                                                          （preview.ready→gen-changelog 分支被剪枝）
		//
		// 这一条特意做了几件别的 trace 没做的事：
		//   · 同一个能力在一个 trace 里出现**两次**（run-tests 第 1 次失败、
		//     第 2 次成功）——瀑布图要能分清这是两次执行，不是重复渲染
		//   · 失败任务的 error 字段非空（UI 要展示错误文本）
		//   · 有 attempts=2 的重试标记
		//   · 有剪枝（clean 分支的 run-tests 没被派发，所以它不在任务里）
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
			const plan = [
				[analyze, 0.0, 4.8, "needs-fix", "succeeded", "发现 1 个高危 + 27 处 lint", 0, 1],
				[lintFix, 4.8, 96.2, "remaining", "succeeded", "自动修了 26 处，1 处需人工", 0, 1],
				[report, 96.2, 97.4, "escalate", "succeeded", "已通知值班人", 2, 1],
				[alert, 97.4, 98.1, null, "succeeded", "电话已拨出", 2, 1],
				[tests, 4.8, 71.3, "failed", "failed", "3 个用例失败（parse_test.go）", 1, 1],
				[fix, 71.3, 132.6, "fixed", "succeeded", "自动补了边界判断", 1, 2],
				[tests, 132.6, 178.9, "passed", "succeeded", "278 用例全部通过", 1, 1],
				[deploy, 178.9, 231.5, "failed", "failed", "健康检查 5 分钟未通过", 1, 1],
				[rollback, 231.5, 250.8, "done", "succeeded", "已回滚到上一版", 1, 1],
				[alert, 250.8, 251.4, null, "succeeded", "值班已收到升级", 2, 1],
				// 注意这里**没有** gen-changelog：
				//
				// 配置里它是 `deploy-preview --preview.ready--> gen-changelog`，
				// 而这次部署走的是 `failed` 那条边。所以它根本没被派发，
				// 图视图上会以"剪枝"形式留在原地 —— 这正是底图的意义：
				// 让"配置里有这条路、但这次没走"变得可见。
				//
				// （之前让它"执行了"是错的：部署失败了哪来的预览地址去生成变更日志。）
			];

			let prev = null;
			for (const [cap, t0, t1, out, status, note, nodeIdx, attempts] of plan) {
				const clock = base + t0;
				const task = mkTask({
					traceId,
					cap,
					parent: prev ? prev.id : null,
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
				prev = task;
			}
		}
	})();

	// ---------- 初始 keys ----------
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

	// ===== 统计 =====
	on("GET", /^\/v1\/stats$/, () => {
		const done = state.tasks.filter((t) => t.status === "succeeded").length;
		const failed = state.tasks.filter((t) => t.status === "failed").length;
		return json({
			totalEvents: state.events.length,
			totalTasks: state.tasks.length,
			succeeded: done,
			failed: failed,
			running: state.tasks.filter((t) =>
				["assigned", "acked", "running"].includes(t.status),
			).length,
			pending: state.tasks.filter((t) => t.status === "pending").length,
			nodesOnline: state.nodes.filter((n) => n.status === "online").length,
			nodesTotal: state.nodes.length,
			// 热力图：最近 7 天每天的任务数
			daily: Array.from({ length: 7 }, (_, i) => {
				const day = now() - (6 - i) * 86400;
				return {
					ts: day - (day % 86400),
					count: Math.max(0, 3 + ((i * 7) % 5) - 2),
				};
			}),
		});
	});

	// ===== 任务 =====
	on("GET", /^\/v1\/tasks$/, (ctx) => {
		let list = state.tasks.slice();
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

	// ===== 能力与边 =====
	on("GET", /^\/v1\/capabilities$/, () =>
		json({ capabilities: state.capabilities.map(capView) }),
	);

	on("POST", /^\/v1\/capabilities$/, (ctx) => {
		const b = ctx.body || {};
		if (!b.name) return err(400, "name is required");
		const existing = state.capabilities.filter((c) => c.name === b.name);
		const nextVer = existing.length
			? Math.max(...existing.map((c) => c.version)) + 1
			: 1;
		const c = cap(b.name, b.version || nextVer, b.outputs || [], b.paramsSchema);
		state.capabilities.push(c);
		return json({ capability: capView(c) }, 201);
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

	on("GET", /^\/v1\/edges$/, () => json({ edges: clone(state.edges) }));

	on("POST", /^\/v1\/edges$/, (ctx) => {
		const b = ctx.body || {};
		const to = state.capabilities.find((c) => c.id === b.toCapability);
		if (!to) return err(400, "下游能力不存在");

		// 入口边（#110）：左端是来源名，出口是事件类型，任意值。
		if (b.fromIsSource) {
			if (!b.fromCapability) return err(400, "来源名不能为空");
			const e = {
				id: uid("edg"),
				orgId: ORG,
				fromIsSource: true,
				fromCapability: String(b.fromCapability),
				fromPort: b.fromPort,
				toCapability: to.id,
				toCapabilityName: to.name,
				createdAt: now(),
			};
			state.edges.push(e);
			return json({ edge: clone(e) }, 201);
		}

		const from = state.capabilities.find((c) => c.id === b.fromCapability);
		if (!from) return err(400, "上游能力不存在");
		// 能力之间的边：fromPort 必须在该能力的 outputs 里（I2）
		if (!(from.outputs || []).includes(b.fromPort)) {
			return err(400, "fromPort 不在该能力的出口集合内");
		}
		const e = {
			id: uid("edg"),
			orgId: ORG,
			fromCapability: from.id,
			fromCapabilityName: from.name,
			fromPort: b.fromPort,
			toCapability: to.id,
			toCapabilityName: to.name,
			createdAt: now(),
		};
		state.edges.push(e);
		return json({ edge: clone(e) }, 201);
	});

	on("DELETE", /^\/v1\/edges$/, (ctx) => {
		const id = ctx.query.get("id");
		const before = state.edges.length;
		state.edges = state.edges.filter((e) => e.id !== id);
		if (state.edges.length === before) return err(404, "边不存在");
		return json({ deleted: id });
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
		});
	}

	on("GET", /^\/v1\/nodes$/, () =>
		json({ nodes: state.nodes.map(nodeView) }),
	);

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

	// ===== 源 =====
	on("GET", /^\/v1\/sources$/, () =>
		json({ sources: state.sources.map(sourceView) }),
	);

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

	// 手动触发：真的往 state 里加事件 + 任务，页面上能看到链长出来
	// planDispatch 算出"这条配置会不会真的跑起来"——**不产生任何副作用**。
	//
	// fire 和「试运行」共用它：fire 拿结果去建任务，试运行只展示。
	// 抽出来是因为两边必须给出**一致**的判定，否则用户会看到
	// "试运行说能跑，一点真的跑却剪枝了"这种最伤信任的差异。
	//
	// 返回 {tasks, pruned}：
	//   tasks  —— 会派发的出口（含求值后的参数）
	//   pruned —— 被跳过的出口及原因（no_capability / param_missing /
	//             param_schema / param_type / no_node）
	function planDispatch(source, payload) {
		const tasks = [];
		const pruned = [];

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

		const plan = planDispatch(s, payload);
		const tasks = [];
		for (const p of plan.tasks) {
			const c = state.capabilities.find((x) => x.id === p.capabilityId);
			const tk = {
				id: uid("tsk"), orgId: ORG, traceId: traceId,
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
		const plan = planDispatch(s, ev.payload);
		const tasks = [];
		for (const p of plan.tasks) {
			const c = state.capabilities.find((x) => x.id === p.capabilityId);
			const tk = {
				id: uid("tsk"),
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
		let list = state.events.slice();
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
			revoked: false,
		};
		state.keys.push(k);
		// 明文只返回一次（与真实后端一致）
		return json({ key: clone(k), plaintext: k.prefix + "_" + "x".repeat(28) }, 201);
	});

	on("POST", /^\/v1\/keys\/([^/]+)\/revoke$/, (ctx) => {
		const k = state.keys.find((x) => x.id === ctx.m[1]);
		if (!k) return err(404, "key 不存在");
		k.revoked = true;
		return json({ key: clone(k) });
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
	on("GET", /^\/v1\/auth\/recovery\/status$/, () =>
		json({ enabled: true, remaining: 8, total: 10 }),
	);
	on("POST", /^\/v1\/auth\/recovery\/regenerate$/, () =>
		json({ codes: Array.from({ length: 10 }, () => "MOCK-CODE-" + Math.random().toString(36).slice(2, 8)) }),
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

		// 没实现的接口：明确说出来，而不是静默返回 {} 让页面莫名其妙地空
		console.warn("[mock] 未实现的接口:", method, pathOnly);
		return err(501, "mock 未实现：" + method + " " + pathOnly);
	};

	// ---------- 预览提示条 ----------
	//
	// 让人一眼知道"这是假数据"，否则很容易误以为后端坏了。
	function mountBanner() {
		const css = document.createElement("style");
		css.textContent = `
			#mock-banner {
				position: fixed;
				left: 50%;
				bottom: 12px;
				transform: translateX(-50%);
				z-index: 999999;
				display: flex;
				align-items: center;
				gap: 8px;
				padding: 7px 14px;
				border-radius: 9999px;
				background: rgba(24, 24, 27, 0.94);
				border: 1px solid rgba(251, 191, 36, 0.45);
				color: #fbbf24;
				font-size: 12px;
				font-weight: 500;
				font-family: ui-sans-serif, system-ui, sans-serif;
				box-shadow: 0 8px 24px rgba(0, 0, 0, 0.45);
				backdrop-filter: blur(8px);
				cursor: pointer;
				user-select: none;
			}
			#mock-banner .dot {
				width: 6px; height: 6px; border-radius: 9999px;
				background: #fbbf24;
				box-shadow: 0 0 8px #fbbf24;
			}
			#mock-banner .x {
				margin-left: 4px; opacity: 0.6; font-size: 14px; line-height: 1;
			}
			#mock-banner:hover .x { opacity: 1; }
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
		bar.addEventListener("click", () => bar.remove());

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
	};
	mountBanner();

	console.info(
		"[mock] 前端预览模式：所有 /v1/* 请求由本地 mock 应答。\n" +
			"  - 数据存在 window.MockApi.state，可改\n" +
			"  - 任意接口加 ?mockFail=1 可看错误态\n" +
			"  - 刷新页面恢复初始数据",
	);
})();
