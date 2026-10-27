/**
 * Anotify 图适配层 —— 在 FlowGraph 之上，把我们的数据形状映射成图
 *
 * 分两层是故意的：
 *
 *   web/flograph.js  通用渲染器（布局 / 节点 / 连线 / 缩放），与业务无关
 *   web/graphs.js    本模块：能力和边 → 流向图；任务链 → 拓扑图
 *
 * 这样"画一个图"的能力只有一份（flograph.js），而"我们的数据怎么变成图"
 * 也只有一份（这里）。页面只负责：取数据 → 调这里的一个函数。
 *
 * 两个图的语义差别（曾经混过一次，这里写死）：
 *
 *   流向图（capabilityFlow）  静态配置视图
 *     问"可能流向哪些能力"。数据来自 /v1/capabilities + /v1/edges，
 *     一次画出**所有**声明的出口和边，包括没走过的分支。
 *     所以它天然是 DAG（或有意保留的环），跟运行状态无关。
 *
 *   任务链图（taskChain）     动态运行视图
 *     问"这次执行实际走了哪条路径"。数据来自 /v1/tasks，每个任务一行。
 *
 *     **扇出是合法的**：一个任务回报一个 out，拿这个 (能力, 出口) 去查
 *     edges 会返回 N 行（同一个出口可以有多条边），于是建 N 个子任务，
 *     它们的 parent_task_id 都是同一个。wiki 02「一个出口可以有多条边」
 *     与 e2e 契约 A6 都以此为准。
 *
 *     所以一条 trace 的数据形状是**森林/树**，不是单链：多个根（源扇出）
 *     + 中间分叉（出口扇出）。每条分支各自到底，不做收集（join）。
 *
 * 依赖：vendor/dagre.min.js + web/flograph.js（全局 FlowGraph）
 */
(function () {
	"use strict";

	/**
	 * capLabel 是本模块对 Anotify.capLabel 的薄代理。
	 *
	 * 为什么不能裸调：graphs.js 与 partials.js 是两个独立的 IIFE，
	 * 后者的局部函数在前者作用域里不存在 —— 裸调会在**渲染时**抛
	 * ReferenceError（不是加载时，所以更容易漏到线上）。
	 *
	 * 为什么要有代理而不是到处写 window.Anotify.capLabel：加载顺序
	 * 不保证（base.html 里两个都是普通 script），代理能在调用时才解析，
	 * 而且 capLabel 缺失时给出可读的回落而不是崩掉整张图。
	 */
	function capLabel(cap) {
		const A = window.Anotify;
		if (A && typeof A.capLabel === "function") return A.capLabel(cap);
		// 回落：至少别崩，也别显示 "undefined"
		if (!cap) return "";
		return String(cap.name || "") + (cap.version != null ? "@" + cap.version : "");
	}

	// ==================== 内部工具 ====================

	const ICON = {
		trigger: "⚡",
		origin: "⚡",
		pipe: "◆",
		end: "◉",
		unknown: "✕",
		deadport: "○",
	};

	// 九态 → 颜色。与瀑布图、状态点用同一套，跨视图一致。
	const STATUS_COLOR = {
		pending: "#71717a",
		assigned: "#3b82f6",
		acked: "#06b6d4",
		running: "#22c55e",
		succeeded: "#10b981",
		failed: "#ef4444",
		canceled: "#a1a1aa",
		abandoned: "#f97316",
		lease_expired: "#eab308",
	};

	const STATUS_ICON = {
		pending: "◆",
		assigned: "◆",
		acked: "◆",
		running: "▶",
		succeeded: "✓",
		failed: "✕",
		canceled: "⊘",
		abandoned: "⊘",
		lease_expired: "⏱",
	};

	/** 图太大时截断（防数据异常卡死浏览器）。返回实际使用的节点/边。 */
	function truncate(nodes, edges, limit) {
		if (nodes.length <= limit) return { nodes, edges, truncated: 0 };
		const useNodes = nodes.slice(0, limit);
		const keep = new Set(useNodes.map((n) => n.id));
		return {
			nodes: useNodes,
			edges: edges.filter((e) => keep.has(e.from) && keep.has(e.to)),
			truncated: nodes.length,
		};
	}

	/**
	 * 把节点和边交给 FlowGraph；顺便挂 _graph 引用方便调试与测试。
	 *
	 * 宿主**必须已经在文档里**：FlowGraph 靠 offsetWidth/offsetHeight 测量
	 * 节点尺寸喂给 dagre，脱离文档的元素量出来是 0（或一个极小的默认值），
	 * 布局会算成一团、节点全叠在一起。这里显式检查，把"看不见的布局错误"
	 * 变成一眼能看出的异常。
	 */
	function render(host, spec) {
		if (!host || !host.isConnected) {
			throw new Error(
				"AnotifyGraphs: 宿主元素必须先插入文档再调 mount()" +
					"（脱离文档时 offsetWidth 为 0，dagre 会布局出重叠的图）",
			);
		}
		const graph = window.FlowGraph.mount(host, spec);
		host._graph = graph;
		return graph;
	}

	// ==================== 流向图（静态配置） ====================

	/**
	 * capabilityFlow —— 从"源"出发画配置好的下游流向
	 *
	 * @param {HTMLElement} host        容器（会被清空）
	 * @param {Object} opts
	 * @param {Object} opts.src         源对象 { name, kind }
	 * @param {Array}  opts.tree        buildFlow() 的产物（树形，含 cycle/seen/dead 标记）
	 * @param {Function} opts.t        i18n
	 * @param {Function} [opts.onNodeClick]
	 * @param {number} [opts.limit]
	 */
	function capabilityFlow(host, opts) {
		const t = opts.t || ((k, d) => d || k);
		const tree = opts.tree || [];
		const src = opts.src || {};
		const limit = opts.limit || 120;

		// 1. 树 → 扁平的 nodes/edges
		const nodes = new Map();
		const edges = new Map();
		const addNode = (id, data) => {
			if (!nodes.has(id)) nodes.set(id, Object.assign({ id }, data));
		};
		const addEdge = (from, to, port, cycle) => {
			const key = from + "|" + port + "|" + to;
			if (edges.has(key)) {
				if (cycle) edges.get(key).cycle = true;
				return;
			}
			edges.set(key, { from, to, port: port || "", cycle: !!cycle });
		};
		const deadPortID = (capId, port) => "dead|" + capId + "|" + port;

		(function walk(list, parentId) {
			for (const n of list) {
				const unknown = n.kind === "unknown";
				addNode(n.capId, {
					// 有显示名就用它。裁决点只有一处：Anotify.capLabel
					// （它活在 partials.js 的 IIFE 里，本文件是独立 IIFE，
					// 只能通过 window.Anotify 访问，不能裸调）。
					// 图节点不带版本号：视觉空间有限，版本放悬停标题里。
					label: unknown ? n.capId : capLabel(n.cap),
					kind: unknown ? "unknown" : n.cap.kind || "pipe",
					// "源直接指向"这个信息不用于高亮（图上唯一的根是触发器），
					// 但下面要用它来连"源 → 首层能力"的边
					isEntry: parentId == null,
					cycle: n.kind === "cycle",
					seen: n.kind === "seen",
					deadport: false,
				});
				if (parentId != null) addEdge(parentId, n.capId, n.viaPort, n.kind === "cycle");

				// 声明了但没接边的出口：画灰色终点伪节点，让配置遗漏在图上可见
				for (const port of n.unconnected || []) {
					const did = deadPortID(n.capId, port);
					addNode(did, { label: t("sources.flow.unconnected", "未接下游"), kind: "deadport", deadport: true });
					addEdge(n.capId, did, port);
				}
				if (n.children && n.children.length) walk(n.children, n.capId);
			}
		})(tree, null);

		// 2. 加触发器节点（源本身），连到所有首层能力
		const SRC_ID = "__source__";
		const kindLabel = {
			cron: t("sources.kind.cron", "定时器"),
			webhook: t("sources.kind.webhook", "Webhook"),
			manual: t("sources.kind.manual", "手动"),
		};
		const all = [...nodes.values()];
		all.unshift({
			id: SRC_ID,
			label: src.name || t("sources.flow.source", "源"),
			kind: "trigger",
			root: true,
		});
		const allEdges = [...edges.values()];
		for (const n of nodes.values()) {
			if (n.isEntry) allEdges.push({ from: SRC_ID, to: n.id, port: "" });
		}

		// 3. 截断 + 映射成 FlowGraph 的节点形状
		const cut = truncate(all, allEdges, limit);

		const subOf = (n) => {
			if (n.id === SRC_ID) return kindLabel[src.kind] || "";
			if (n.kind === "deadport") return t("sources.flow.no_edge", "没有接下游");
			if (n.kind === "unknown") return t("sources.flow.missing", "能力已删除");
			if (n.kind === "end") return t("sources.flow.terminal_short", "终点");
			return "";
		};

		const graph = render(host, {
			nodes: cut.nodes.map((n) => ({
				id: n.id,
				label: n.label,
				sub: subOf(n),
				kind: n.kind,
				// 源详情页画的**就是**结构身份（这里的 kind 不是执行状态），
				// 所以原样传成 role —— 和 trace 页的图共用同一套 role 样式，
				// 两处看起来才一致。
				role: n.kind,
				root: !!n.root,
				entry: !!n.isEntry,
				cycle: n.cycle,
				seen: n.seen,
				icon: ICON[n.kind] || "◆",
				accent: n.accent,
			})),
			edges: cut.edges.map((e) => ({
				from: e.from,
				to: e.to,
				port: e.port,
				cycle: e.cycle,
				muted: cut.nodes.some((n) => n.id === e.to && n.kind === "deadport"),
			})),
			onNodeClick: (n) => {
				if (n.kind === "deadport" || n.kind === "unknown" || n.kind === "trigger") return;
				if (opts.onNodeClick) opts.onNodeClick(n);
			},
		});

		return { graph, truncated: cut.truncated };
	}

	// ==================== 任务链图（动态运行） ====================

	/**
	 * taskChain —— 把一条 trace 的任务列表画成拓扑图
	 *
	 * @param {HTMLElement} host
	 * @param {Object} opts
	 * @param {Array}  opts.tasks     任务列表（Task 结构）
	 * @param {Function} opts.t       i18n
	 * @param {Function} [opts.onNodeClick]
	 * @param {number} [opts.limit]
	 *
	 * 出口名的取法（这里曾经绕过一次弯）：
	 *
	 *   每个任务只有一个 out（不变量 I5），所以"父 → 子"这条边对应的出口
	 *   **就是父任务的 out**。真实性是：一个父任务只沿它汇报的那一个出口
	 *   派发一次，因此正常情况下每个父任务只会有一个子任务。
	 *
	 *   如果数据里出现"一个父任务多个子任务"，那不是模型能表达的状态
	 *   （说明数据是伪造的或历史遗留），此时 out 无法区分是哪条边，
	 *   多子一律不标。
	 */
	function taskChain(host, opts) {
		// 这里不用 i18n：副标题显示的是九态原始名（succeeded / failed …），
		// 它们是协议里的状态标识，翻译反而对不上日志和 API 响应。
		const t = opts.t || ((k, d) => d || k);
		const tasks = opts.tasks || [];
		const limit = opts.limit || 120;

		// 底图：这个 trace 所属源在「源」页配置的静态拓扑。
		//
		// 为什么用底图而不是只画执行过的那条链：只画链的话，"为什么这条
		// 分支没走"这个问题图上根本看不见 —— 剪枝的分支压根不出现，
		// 用户看到的永远是一条顺顺利利的直线。用配置拓扑当底、执行状态
		// 当覆盖层，被剪枝的分支会以灰色留在原地，一眼看出"这里本来有
		// 条路，这次没走"。
		const base = opts.baseGraph || null;

		// ------- 执行状态索引：能力 id → 该能力本条 trace 的任务列表 -------
		//
		// 同一个能力可能执行多次（失败重试后再次进入同一步），所以是数组。
		// 底图的一个节点用一个徽章汇总：n 次执行里"最坏"的那个状态露出来，
		// 次数标在旁边。
		const byCapability = new Map();
		for (const tk of tasks) {
			const cid = tk.capabilityId;
			if (!cid) continue;
			if (!byCapability.has(cid)) byCapability.set(cid, []);
			byCapability.get(cid).push(tk);
		}

		// 状态严重度：节点汇总时取最坏的那个（failed 比 succeeded 更该被看见）
		const SEVERITY = {
			failed: 5,
			abandoned: 4,
			running: 3,
			assigned: 3,
			leased: 3,
			pending: 2,
			succeeded: 1,
		};
		const worst = (list) => {
			let best = null;
			let bestScore = -1;
			for (const tk of list) {
				const sc = SEVERITY[tk.status] || 0;
				if (sc > bestScore) {
					bestScore = sc;
					best = tk;
				}
			}
			return best;
		};

		let nodes;
		let edges;

		if (base && base.nodes && base.nodes.length) {
			// ------- 有底图：配置拓扑 + 执行染色 -------
			nodes = base.nodes.map((n) => {
				// 图标永远表达**结构身份**（origin / pipe / end），
				// 颜色和徽章表达**执行状态**。
				//
				// 这两件事必须同时可见：图标若换成状态图标，
				// "这个节点本来是终点"就没了；只靠颜色表达结构的话，
				// 失败/剪枝又看不出来。
				const roleIcon = ICON[n.kind] || ICON.pipe;

				const hits = byCapability.get(n.id);
				if (!hits || !hits.length) {
					// 剪枝：这条分支这次没走。
					//
					// 用独立 kind（不是简单的变灰），让它能同时表达
					// "这是配置里真实存在的路"和"这次没走"。
					return {
						id: n.id,
						label: n.label,
						sub: t("trace.graph.pruned", "未执行"),
						kind: "pruned",
						icon: roleIcon,
						role: n.kind,
						entry: n.entry,
						dead: true,
					};
				}
				const tk = worst(hits);
				const st = tk.status || "pending";
				const dur =
					tk.startedAt != null && tk.finishedAt != null
						? (tk.finishedAt - tk.startedAt) * 1000
						: null;
				const bits = [st];
				if (dur != null) bits.push(fmtDur(dur));
				if (hits.length > 1) bits.push("×" + hits.length);
				return {
					id: n.id,
					label: n.label,
					sub: bits.join(" · "),
					kind: kindForStatus(st),
					icon: roleIcon,
					role: n.kind,
					entry: n.entry,
					badge: STATUS_COLOR[st] || "#71717a",
					accent: st === "failed" ? "#ef4444" : null,
					_task: tk,
				};
			});

			// 每条边**实际被走过几次**。
			//
			// 这不是"拓扑上有没有环"的判断，而是纯执行事实：
			// "跑测试 --failed--> 应用修复 --fixed--> 跑测试" 这条环走一遍，
			// 其中 apply-fix→run-tests 这条边就被**再走一次**。
			//
			// 原来把回边涂成橙色虚线，语义是"这里有环 = 危险"。但环本身
			// 不危险 —— 它就是这个工作流的样子（失败了就修、修完再测）。
			// 真正想表达的是"这条边走过很多次"，所以改成：**次数越多颜色越深**，
			// 和普通边同一个色系，只是更实、更粗。
			const walkCount = new Map();
			const capOfTask = new Map();
			for (const tk of tasks) capOfTask.set(tk.id, tk.capabilityId);
			for (const tk of tasks) {
				if (!tk.parentTaskId) continue;
				const fromCap = capOfTask.get(tk.parentTaskId);
				if (!fromCap) continue;
				const key = fromCap + "\u0000" + tk.capabilityId;
				walkCount.set(key, (walkCount.get(key) || 0) + 1);
			}

			// 回边：环上的边。
			//
			// 判据不是"危险"，而是"这条路会回来"—— 环意味着同一段路
			// 会被走第二遍、第三遍。所以它天然比直线更深。
			const backEdges = findBackEdges(base.nodes.map((n) => n.id), base.edges);

			edges = base.edges.map((e) => {
				const fromNode = nodes.find((n) => n.id === e.from);
				const toNode = nodes.find((n) => n.id === e.to);
				const bothAlive = !(fromNode && fromNode.dead) && !(toNode && toNode.dead);
				const walked = walkCount.get(e.from + "\u0000" + e.to) || 0;
				const isCycle = backEdges.has(e.from + "\u0000" + e.to);
				return {
					from: e.from,
					to: e.to,
					port: e.port,
					// "这条路被走过几遍"：
					//   真实遍历次数 + 环（在环上意味着还会绕回来）
					// 两个信号合并成一个数，渲染层只需要一个档位。
					times: walked + (isCycle ? 1 : 0),
					// 回边要绕出去画：否则它只是两个相邻节点之间的一小段，
					// 看不出"这里是绕回来的"。渲染层据此把路径鼓出弧线。
					loopback: isCycle,
					// 剪枝的边用虚线灰掉：和"配置里没有这条边"区分开
					muted: !bothAlive,
				};
			});

			// ---- 源节点：这条 trace 是谁触发的 ----
			//
			// 没有它，图只是"一堆能力怎么连"，看不出这个 trace 的**起点** ——
			// 而"谁触发"恰恰是排查的第一步。
			if (opts.sourceLabel) {
				const SRC_NODE_ID = "__trigger__";
				nodes.unshift({
					id: SRC_NODE_ID,
					label: opts.sourceLabel,
					sub: opts.sourceSub || "",
					kind: "trigger",
					icon: ICON.trigger,
					role: "origin",
					root: true,
				});
				// 连向所有"源直接指向"的能力。没有 entry 标记时
				// （老数据 / 底图不完整）就连到所有无入边的节点。
				const hasIncoming = new Set(base.edges.map((e) => e.to));
				const entries = base.nodes.filter((n) =>
					n.entry !== undefined ? n.entry : !hasIncoming.has(n.id),
				);
				for (const n of entries) {
					edges.push({
						from: SRC_NODE_ID,
						to: n.id,
						port: opts.sourcePort || "",
						muted: !!n.dead,
					});
				}
			}
		} else {
			// ------- 没有底图：退回"只画执行链"（原行为） -------
			//
			// 拿不到源配置时（源已删除、trace 来自未知源）仍然要能看图。
			const byId = new Map(tasks.map((x) => [x.id, x]));
			nodes = tasks.map((tk) => {
				const st = tk.status || "pending";
				const dur =
					tk.startedAt != null && tk.finishedAt != null
						? (tk.finishedAt - tk.startedAt) * 1000
						: null;
				const bits = [st];
				if (dur != null) bits.push(fmtDur(dur));
				if (tk.attempts > 1) bits.push("×" + tk.attempts);
				return {
					id: tk.id,
					label: (tk.capability || "?") + "@" + (tk.version ?? "?"),
					sub: bits.join(" · "),
					kind: kindForStatus(st),
					// 没有底图时不知道结构身份，只能用状态图标
					icon: STATUS_ICON[st] || "◆",
					badge: STATUS_COLOR[st] || "#71717a",
					accent: st === "failed" ? "#ef4444" : null,
					_task: tk,
				};
			});

			edges = [];
			for (const tk of tasks) {
				if (!tk.parentTaskId || !byId.has(tk.parentTaskId)) continue;
				const parent = byId.get(tk.parentTaskId);
				edges.push({
					from: tk.parentTaskId,
					to: tk.id,
					port: parent.out || "",
					cycle: tk.status === "failed",
				});
			}
		}

		const cut = truncate(nodes, edges, limit);

		// 布局方向按**节点数**自适应。
		//
		// 原先固定横向（LR）：节点少时确实好读（时间从左往右，和瀑布图一致）。
		// 但长链会退化 —— 10 个节点的链横向排开要 2000px 以上，520px 宽的
		// 卡片里只能缩到 0.2 倍，字完全看不清，而容器高度还空着大半。
		//
		// 超过 7 个节点改用纵向（TB）：链路图天生是**一条**链，
		// 纵向排正好顺着容器的高度走，宽高比正常。
		//
		// 有底图时反而是横着的（源流向图就是横向，扇出往右铺），
		// 所以那边按宽高比判断更合适 —— 但节点数少时横向仍然更好读。
		const longChain = cut.nodes.length > 7;
		// 图例：只在有底图（也就是有两种编码要解释）时给。
		// 没有底图时只有一个维度（状态），图上本来就说得清。
		const legend = [];
		if (base && base.nodes && base.nodes.length) {
			legend.push(
				{ icon: "⚡", text: t("trace.legend.origin", "源 · 触发器（事件从这里进来）") },
				{ icon: "◆", text: t("trace.legend.pipe", "管道 · 会继续往下派发") },
				{ icon: "◉", text: t("trace.legend.end", "终点 · 到此结束，不再派发") },
				{ icon: "✓", cls: "lg-ok", text: t("trace.legend.ok", "执行成功") },
				{ icon: "✕", cls: "lg-bad", text: t("trace.legend.bad", "执行失败") },
				{ icon: "○", cls: "lg-pruned", text: t("trace.legend.pruned", "未执行（配置里有、这次没走）") },
				{ icon: "↺", cls: "lg-hot", text: t("trace.legend.hot", "线与数字越深 · 这条路走了很多次（重试/回环）") },
			);
		}

		const graph = render(host, {
			legend: legend,
			rankdir: longChain ? "TB" : "LR",
			rankSep: longChain ? 40 : 96,
			nodeSep: longChain ? 22 : 26,
			nodes: cut.nodes.map((n) => {
				const out = Object.assign({}, n);
				delete out._task;
				return out;
			}),
			edges: cut.edges,
			// 透传给 flograph：链路图这一页没有标题栏的「全屏」入口，
			// 所以需要工具栏版本；源流向图那边有，默认不画避免重复。
			showFullscreen: !!opts.showFullscreen,
			onNodeClick: (n) => {
				if (!opts.onNodeClick) return;
				// 有底图时节点 id 是能力 id，映射回该能力最"严重"的那次任务
				const hit = byCapability.get(n.id);
				if (hit && hit.length) opts.onNodeClick(hit[0]);
			},
		});

		return { graph, truncated: cut.truncated };
	}

	/**
	 * findBackEdges 用 DFS 找"回边"（指向当前递归栈上节点的边）。
	 *
	 * 一条边 (u→v) 是回边，当且仅当 v 还在 u 的递归栈上 —— 也就是
	 * 沿着 v ⇝ u 已经有一条路径，加上这条就闭环了。
	 *
	 * 用来判断"这条路会回来"：环上的边天然比直线走得更多次，
	 * 图上应该更深一些。
	 */
	function findBackEdges(nodeIds, edges) {
		const out = new Map();
		for (const id of nodeIds) out.set(id, []);
		for (const e of edges) {
			if (out.has(e.from)) out.get(e.from).push(e.to);
		}

		const WHITE = 0;
		const GRAY = 1;
		const BLACK = 2;
		const color = new Map();
		for (const id of nodeIds) color.set(id, WHITE);

		const back = new Set();
		const visit = (u) => {
			color.set(u, GRAY);
			for (const v of out.get(u) || []) {
				if (!color.has(v)) continue;
				const c = color.get(v);
				if (c === GRAY) back.add(u + "\u0000" + v);
				else if (c === WHITE) visit(v);
			}
			color.set(u, BLACK);
		};

		for (const id of nodeIds) {
			if (color.get(id) === WHITE) visit(id);
		}
		return back;
	}

	/**
	 * 状态 → 节点主题色组（kind）。
	 *
	 * 只有三种视觉分组：失败（红）／成功（绿）／进行中（默认蓝）。
	 * 用 if/else 而不是嵌套三元 —— 三元套三元在这里已经读不动了。
	 */
	// 九态 → 视觉分组。注意这一层**不是** i18n 文案，是 CSS 类名后缀，
	// ui.css 里要有对应的 .wf-node.is-* 才能上色。
	//
	// 曾经 failed 映射到 "unknown" —— 那个类的语义是"能力已被删除"，
	// 于是一次执行失败和"配置里指向一个不存在的能力"长得一模一样。
	// 现在分开：failed 有独立的红色，真·unknown 仍是红色但配 ✕ 图标。
	function kindForStatus(st) {
		if (st === "failed" || st === "abandoned") return "failed";
		if (st === "succeeded") return "end";
		if (st === "running" || st === "assigned" || st === "leased") return "running";
		return "pipe";
	}

	/** 毫秒 → "1.2s" / "340ms" / "1m12s"。瀑布图和这里共用同一种写法。 */
	function fmtDur(ms) {
		if (ms == null || !Number.isFinite(ms) || ms < 0) return "";
		if (ms < 1000) return Math.round(ms) + "ms";
		if (ms < 60000) return (ms / 1000).toFixed(ms < 10000 ? 1 : 0) + "s";
		const m = Math.floor(ms / 60000);
		const s = Math.round((ms % 60000) / 1000);
		return m + "m" + s + "s";
	}

	window.AnotifyGraphs = {
		capabilityFlow,
		taskChain,
		fmtDur,
		// 供其它页面复用同一套状态配色
		STATUS_COLOR,
		STATUS_ICON,
	};
})();
