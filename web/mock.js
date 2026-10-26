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
			// 内置起点（source=builtin，outputs 空，任意事件类型）
			cap("github", 1, [], { type: "object" }, "builtin"),
			cap("cron", 1, [], { type: "object" }, "builtin"),
			cap("http", 1, [], { type: "object" }, "builtin"),
			cap("manual", 1, [], { type: "object" }, "builtin"),
			// 中枢执行的终端能力
			cap("notify.phone", 1, [], { type: "object" }, "builtin"),
			// 用户能力：有参数、有分叉
			cap("analyze-pr", 1, ["needs-fix", "approved"], {
				type: "object",
				required: ["changedLines"],
				properties: {
					changedLines: { type: "integer", minimum: 0 },
					repo: { type: "string" },
				},
			}),
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
			cap("deploy-preview", 1, ["failed", "preview.ready", "manual.approval"], {
				type: "object",
				required: ["env"],
				properties: { env: { type: "string", enum: ["staging", "prod"] } },
			}),
			cap("report.send", 1, ["escalate"], {
				type: "object",
				required: ["channel", "title"],
				properties: {
					channel: { type: "string", enum: ["slack", "email"] },
					title: { type: "string" },
					urgent: { type: "boolean" },
					retries: { type: "integer", minimum: 0, maximum: 5 },
				},
			}),
			// 下面几个能力是为了让流向图有真实的"深度 + 分叉 + 汇聚"：
			// 一条链只有 3 个节点看不出布局和信息密度，太简单了。
			cap("run-tests", 1, ["passed", "failed"], {
				type: "object",
				properties: {
					suite: { type: "string", enum: ["unit", "integration", "e2e"] },
					shard: { type: "integer", minimum: 1 },
				},
			}),
			cap("lint-fix", 1, ["clean", "remaining"], {
				type: "object",
				properties: { scope: { type: "string" } },
			}),
			cap("gen-changelog", 1, [], {
				type: "object",
				properties: { since: { type: "string" } },
			}),
			cap("rollback", 1, ["done"], {
				type: "object",
				properties: { reason: { type: "string" } },
			}),
			cap("alert.oncall", 1, [], {
				type: "object",
				required: ["level"],
				properties: {
					level: { type: "string", enum: ["warn", "critical"] },
					page: { type: "boolean" },
				},
			}),
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
			from("github", "pull_request.opened", "analyze-pr"),
			from("github", "pull_request.opened", "report.send"),
			from("github", "pull_request.synchronize", "analyze-pr"),
			from("cron", "nightly", "analyze-pr"),
			from("http", "webhook.incoming", "deploy-preview"),
			from("manual", "manual.run", "deploy-preview"),
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
			return { id: c.id, name: c.name, version: c.version };
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
				// displayName 是纯展示标签（用户自己取的名字）。
		// 这里给几个能力填上，好让 demo 能看到"同名不同读法"的效果；
		// 留几个空的，用来验证没有显示名时回落到 name@version。
		capabilities: ["analyze-pr", "lint-fix", "apply-fix", "report.send", "alert.oncall"].map(capRef),
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
				// displayName 是纯展示标签（用户自己取的名字）。
		// 这里给几个能力填上，好让 demo 能看到"同名不同读法"的效果；
		// 留几个空的，用来验证没有显示名时回落到 name@version。
		capabilities: ["deploy-preview", "run-tests", "gen-changelog"].map(capRef),
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
		state.sources = [
			{
				id: "src_mock_nightly",
				orgId: ORG,
				name: "每晚体检",
				kind: "cron",
				config: { expr: "0 3 * * *", tz: "Local" },
				enabled: true,
				lastFiredAt: now() - 3600 * 9,
				nextFireAt: now() + 3600 * 7,
				createdAt: now() - 86400 * 2,
				targets: [
					{ capabilityId: analyze.id, params: { changedLines: 120, repo: "anotify" } },
				],
			},
			{
				id: "src_mock_hook",
				orgId: ORG,
				name: "GitHub PR",
				kind: "webhook",
				config: { token: "src_mocktoken_abc123" },
				enabled: true,
				lastFiredAt: now() - 600,
				nextFireAt: null,
				createdAt: now() - 86400,
				targets: [
					{ capabilityId: analyze.id, params: { changedLines: 350 } },
					{ capabilityId: deploy.id, params: { env: "staging" } },
				],
			},
			{
				id: "src_mock_report",
				orgId: ORG,
				name: "每日汇总",
				kind: "manual",
				config: {},
				enabled: false,
				lastFiredAt: null,
				nextFireAt: null,
				createdAt: now() - 3600 * 5,
				targets: [
					{
						capabilityId: report.id,
						params: { channel: "slack", title: "每日汇总", urgent: false, retries: 2 },
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

		let t = now() - 180;

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
				payload: { pr: 842, repo: "anotify" }, createdAt: t,
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
				payload: { pr: 913, repo: "anotify", files: 27 }, createdAt: t,
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
				payload: { pr: 843 }, createdAt: t,
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
	function capView(c) {
		return Object.assign(clone(c), { kind: kindOf(c) });
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
		if (s.kind === "webhook" && s.config && s.config.token) {
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
		const from = state.capabilities.find((c) => c.id === b.fromCapability);
		const to = state.capabilities.find((c) => c.id === b.toCapability);
		if (!from || !to) return err(400, "能力不存在");
		// 非起点能力：fromPort 必须在该能力的 outputs 里（I2）
		const isOrigin = kindOf(from) === "origin";
		if (!isOrigin && !(from.outputs || []).includes(b.fromPort)) {
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
	on("GET", /^\/v1\/nodes$/, () => json({ nodes: clone(state.nodes) }));

	on("POST", /^\/v1\/nodes\/([^/]+)\/drain$/, (ctx) => {
		const n = state.nodes.find((x) => x.id === ctx.m[1]);
		if (!n) return err(404, "节点不存在");
		n.status = "draining";
		return json({ node: clone(n) });
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

	function validateTargets(targets) {
		const byID = new Map(state.capabilities.map((c) => [c.id, c]));
		for (const t of targets) {
			const c = byID.get(t.capabilityId);
			if (!c) return "出口能力 " + t.capabilityId + " 不存在";
			if (t.params && Object.keys(t.params).length) {
				const msg = validateParams(c.paramsSchema, t.params);
				if (msg) return `${c.name}@${c.version} 的参数不合法：${msg}`;
			}
		}
		return "";
	}

	on("POST", /^\/v1\/sources$/, (ctx) => {
		const b = ctx.body || {};
		if (!b.name || !b.name.trim()) return err(400, "name is required");
		if (!["cron", "webhook", "manual"].includes(b.kind)) {
			return err(400, "kind 必须是 cron / webhook / manual", {
				available: ["cron", "webhook", "manual"],
			});
		}
		const targets = normTargets(b.targets);
		if (!targets.length) {
			return err(400, "targets 至少要选一个出口能力（否则这个源产生的事件永远派不出去）");
		}
		if (state.sources.some((s) => s.name === b.name.trim())) {
			return err(409, "同名源已存在，换个名字");
		}
		const bad = validateTargets(targets);
		if (bad) return err(400, bad);

		let cfg = b.config || {};
		if (b.kind === "cron") {
			const expr = (cfg.expr || "").trim();
			if (!expr) return err(400, "cron 需要 expr（五段表达式，如 0 3 * * *）");
			if (expr.split(/\s+/).length !== 5) {
				return err(400, "cron 表达式需要 5 段，得到 " + expr.split(/\s+/).length + " 段");
			}
			cfg = { expr: expr, tz: cfg.tz || "Local" };
		} else if (b.kind === "webhook") {
			cfg = { token: cfg.token || "src_mock" + Math.random().toString(36).slice(2, 12) };
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
				s.config = { expr: expr, tz: b.config.tz || "Local" };
			} else {
				s.config = b.config;
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

		const tasks = [];
		const pruned = [];
		for (const t of s.targets) {
			const c = state.capabilities.find((x) => x.id === t.capabilityId);
			if (!c) {
				pruned.push({ reason: "no_capability", capabilityId: t.capabilityId, detail: "能力不存在" });
				continue;
			}
			const params = t.params || {};
			const msg = validateParams(c.paramsSchema, params);
			if (msg) {
				pruned.push({
					reason: "param_schema",
					capabilityId: c.id,
					detail: `${c.name}@${c.version} params_schema: ${msg}`,
				});
				continue;
			}
			const hasNode = state.nodes.some(
				(n) => n.status === "online" && (n.capabilities || []).some((x) => x.id === c.id),
			);
			if (!hasNode) {
				pruned.push({
					reason: "no_node",
					capabilityId: c.id,
					detail: `no node declares ${c.name}@${c.version}`,
				});
				continue;
			}
			const tk = {
				id: uid("tsk"),
				orgId: ORG,
				traceId: traceId,
				parentTaskId: null,
				path: [c.id],
				capabilityId: c.id,
				capability: c.name,
				version: c.version,
				params: clone(params),
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

		// 演示"任务跑完"：1.2s 后把刚建的任务标成功，刷新就能看到结果
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
			dispatch: { tasks: clone(tasks), pruned: pruned },
		});
	});

	// ===== 事件流 =====
	on("GET", /^\/v1\/console\/events$/, (ctx) => {
		const limit = Number(ctx.query.get("limit") || 50);
		const trace = ctx.query.get("traceId");
		let list = state.events.slice();
		if (trace) list = list.filter((e) => e.traceId === trace);
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
