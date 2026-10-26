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
				root: !!n.root,
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
		const tasks = opts.tasks || [];
		const limit = opts.limit || 120;

		const byId = new Map(tasks.map((x) => [x.id, x]));

		const nodes = tasks.map((tk) => {
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
				icon: STATUS_ICON[st] || "◆",
				badge: STATUS_COLOR[st] || "#71717a",
				accent: st === "failed" ? "#ef4444" : null,
				_task: tk,
			};
		});

		const edges = [];
		for (const tk of tasks) {
			if (!tk.parentTaskId || !byId.has(tk.parentTaskId)) continue;
			const parent = byId.get(tk.parentTaskId);
			edges.push({
				from: tk.parentTaskId,
				to: tk.id,
				// 出口名取**父任务回报的那个 out**。
				//
				// 扇出时这个 out 对每条边都成立 —— 一个任务只回报一个 out，
				// 拿它查表返回 N 行，就是从这个出口同时走向 N 个能力。
				// 所以多子时标同一个名字是**正确的**，不是"无法区分"。
				//
				// （要精确到"哪条边是第几行"没有意义：库里 edges 只是
				//  (from_capability, from_port) → to_capability 的集合，
				//   同一个出口的多条边本来就并列，没有先后。）
				port: parent.out || "",
				// 失败的分支用橙色虚线，顺着线能看出是哪一步挂了
				cycle: tk.status === "failed",
			});
		}

		const cut = truncate(nodes, edges, limit);

		const graph = render(host, {
			// 横向布局：时间从左往右推进，和瀑布图的读法一致。
			// 竖向排会变成一个细长条（尤其纯链的情况），把容器宽度浪费掉。
			//
			// rankSep 是"相邻两层之间的水平间隙"；扇出时同一层的兄弟节点
			// 靠 nodeSep 拉开垂直距离，太小会挤在一起。
			rankdir: "LR",
			rankSep: 96,
			nodeSep: 26,
			nodes: cut.nodes.map((n) => {
				const out = Object.assign({}, n);
				delete out._task;
				return out;
			}),
			edges: cut.edges,
			onNodeClick: (n) => {
				if (opts.onNodeClick) opts.onNodeClick(n);
			},
		});

		return { graph, truncated: cut.truncated };
	}

	/**
	 * 状态 → 节点主题色组（kind）。
	 *
	 * 只有三种视觉分组：失败（红）／成功（绿）／进行中（默认蓝）。
	 * 用 if/else 而不是嵌套三元 —— 三元套三元在这里已经读不动了。
	 */
	function kindForStatus(st) {
		if (st === "failed") return "unknown";
		if (st === "succeeded") return "end";
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
