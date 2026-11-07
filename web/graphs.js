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
 * 「源 → 能力 → 下游」的**拓扑推导**只有一份：sourceTopology。
 * 源详情页的流向图（capabilityFlow）与 trace 页的底图（taskChain 的 baseGraph）
 * 都从它来 —— 两个页面看到的图因此必然一致。要改推导逻辑就只改它一个地方；
 * 不要再在页面里写第三份（历史上写过两份，结果同一份配置在两页画出不同的图）。
 *
 * 两个图的语义差别（曾经混过一次，这里写死）：
 *
 *   流向图（capabilityFlow）  静态配置视图（**预览**）
 *     问"可能流向哪些能力"。数据来自 /v1/capabilities + /v1/edges，
 *     一次画出**所有**声明的出口和边，包括没走过的分支。
 *     所以它天然是 DAG（或有意保留的环），跟运行状态无关。
 *
 *     渲染器与 trace 页的链路图**是同一个**（flograph 的 Silk 舞台）：
 *     两页的图必须长得一样，用户才不会以为是两个功能。配置视图没有
 *     执行事实，但**预览也要看得出流向** —— 于是这里按拓扑顺序合成一
 *     组节拍（previewBeats，每条边一拍），交给同一套播放机器。
 *     见 capabilityFlow 里的 animate/beats/root/preview 四个参数。
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
	 *
	 * opts 原样透传（本文件用到 { withVersion: true } 来区分同名多版本）。
	 * **不要在这里再写一套显示名规则**：裁决点只有 Anotify.capLabel 一处。
	 */
	function capLabel(cap, opts) {
		const A = window.Anotify;
		if (A && typeof A.capLabel === "function") return A.capLabel(cap, opts);
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

	// 九态 → 颜色。与瀑布图、状态点用同一套（唯一来源是 tokens.css 的
	// --st-*），跨视图一致；亮/暗主题各自给值，切换主题时重新取色。
	//
	// 这里保留一份十六进制**兜底值**：file:// 直开或 CSS 还没加载时
	// getComputedStyle 拿不到变量，没有兜底图就会全白。
	const STATUS_VAR = {
		pending: "--st-pending",
		assigned: "--st-assigned",
		acked: "--st-acked",
		running: "--st-running",
		succeeded: "--st-succeeded",
		failed: "--st-failed",
		canceled: "--st-canceled",
		abandoned: "--st-abandoned",
		lease_expired: "--st-lease_expired",
	};
	const STATUS_FALLBACK = {
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

	/** 读一个 CSS 变量（带兜底），拿不到就返回 fallback。 */
	function cssVar(name, fallback) {
		try {
			const v = getComputedStyle(document.documentElement)
				.getPropertyValue(name)
				.trim();
			return v || fallback;
		} catch (e) {
			return fallback;
		}
	}

	/** 单个状态色：永远以 tokens.css 的 --st-* 为准。 */
	function statusColor(st) {
		const key = Object.hasOwn(STATUS_VAR, st) ? st : "pending";
		return cssVar(STATUS_VAR[key], STATUS_FALLBACK[key]);
	}

	// 对外导出的是**快照对象**（导出时是当前主题的值），
	// 主题切换后由 refreshStatusColors() 就地更新并重绘已挂载的图。
	const STATUS_COLOR = Object.assign({}, STATUS_FALLBACK);
	function refreshStatusColors() {
		for (const k of Object.keys(STATUS_VAR)) STATUS_COLOR[k] = statusColor(k);
	}
	refreshStatusColors();

	// 已挂载的图宿主：主题切换后需要按当前主题重新取色重绘。
	// 用 Set 而不是查 DOM，是因为宿主可能是页面里临时插入的子容器。
	const graphHosts = new Set();

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
	function render(host, spec, renderOpts) {
		if (!host || !host.isConnected) {
			throw new Error(
				"AnotifyGraphs: 宿主元素必须先插入文档再调 mount()" +
					"（脱离文档时 offsetWidth 为 0，dagre 会布局出重叠的图）",
			);
		}
		refreshStatusColors();
		// silk 是**纯渲染开关**，不是图的一部分。
		//
		// 为什么不写进 spec：trace 页与源详情页的 spec 必须逐字段相等
		// （e2e 的 checkSharedTopology 直接比对两页的 _graphSpec）。把
		// "这一页用哪种皮肤"塞进 spec，等价于把一个纯视觉选择伪装成
		// 拓扑事实 —— 两页 spec 立刻不等，而它们描述的数据其实一模一样。
		// 所以开关只走 mount 的 options，_graphSpec 永远是原始 spec。
		const opts = renderOpts || {};
		const mountSpec = opts.silk ? Object.assign({}, spec, { silk: true }) : spec;
		const graph = window.FlowGraph.mount(host, mountSpec);
		host._graph = graph;
		host._graphSpec = spec;
		graphHosts.add(host);
		return graph;
	}

	/**
	 * redrawAll —— 主题切换后重绘所有已挂载的图。
	 *
	 * 图里的状态色是**内联样式**（badge / accent 由 flograph 写成 style），
	 * 只换 CSS 变量不会让已经画出来的节点变色，所以必须按同一份 opts 重跑
	 * 入口函数。宿主已经不在文档里（切页/换视图）就顺手移出注册表。
	 */
	function redrawAll() {
		refreshStatusColors();
		for (const host of Array.from(graphHosts)) {
			if (!host || !host.isConnected) {
				graphHosts.delete(host);
				continue;
			}
			if (typeof host._graphRebuild !== "function") continue;
			try {
				host._graphRebuild();
			} catch (e) {
				// 重绘失败不该把页面搞坏：保留旧图即可。
				console.warn("[graphs] 主题切换重绘失败", e);
			}
		}
	}

	document.addEventListener("anotify:theme", redrawAll);

	// ==================== 拓扑推导（唯一一份） ====================

	// 深度上限：防数据异常（图上画出一条几千跳的链）卡死浏览器。
	// 20 是原来两页各自用的量级，这里保持一致。
	const TOPO_MAX_DEPTH = 20;

	/**
	 * sourceTopology —— 唯一一份「源 → 能力 → 下游」拓扑推导。
	 *
	 * 输入是**配置事实**（源 + 能力表 + 边表），输出是 FlowGraph 中立的 {nodes, edges}。
	 * 源详情页与 trace 页的底图都从这里来 —— 两个页面看到的图必然一致。
	 *
	 * 为什么必须只有一份：这两页原来各有一份推导（source.html 的 buildFlowTree
	 * 建一棵树 + 本文件的 capabilityFlow 把树摊平；trace.html 的 buildBaseGraph
	 * 自己摊平一张底图）。同一份数据、两份推导，结果是同一个源在两个页面画出
	 * 不同的图 —— 「源直接指向两个能力」那条 1:N 的线就是这么丢的：
	 *
	 *   老实现用「节点首次被访问时打 isEntry」来决定画不画「源 → 它」这条线，
	 *   而 addNode 遇到已存在的节点是**静默跳过**的。于是菱形（既能从源直达、
	 *   又能从别的分支到达）里，先遍历到的分支把节点以 isEntry=false 注册，
	 *   后处理 root 时被跳过 → 源 → 它 的那条线永远不画。
	 *
	 * 现在 1:N 由**构造**保证：入口边是唯一入口，每条入口边产出一个 entry 节点，
	 * 渲染层逐个连线 —— 不依赖任何「首次访问」式的遍历状态。
	 *
	 * @param {{src: Object, caps: Array, edges: Array}} input
	 *   src   源对象（只用 name：入口边的左端就是来源名）
	 *   caps  能力定义表（GET /v1/capabilities）
	 *   edges 边表（GET /v1/edges，含 fromIsSource/fromRef/fromCapabilityId/…）
	 * @returns {{nodes: Array, edges: Array, truncated: boolean}}
	 *   节点 {id, label, kind, entry, cycle, deadport, unknown}
	 *   边   {from, to, port, showPort, cycle, muted}
	 *        showPort 只表达**渲染意图**（这条边的出口名该不该画），
	 *        不是数据事实：单出口能力（内部名固定 out）在图上不再标注出口名。
	 */
	function sourceTopology(input) {
		const src = (input && input.src) || {};
		const caps = (input && input.caps) || [];
		const allEdges = (input && input.edges) || [];
		const capById = new Map(caps.map((c) => [c.id, c]));

		// 节点按 id 全局去重、边按 (from, port, to) 去重。
		// 用 Map 而不是 Set 是为了保住**插入序**（布局稳定，diff 也稳定）。
		const nodes = new Map();
		const edges = new Map();
		let truncated = false;

		/**
		 * 上游能力是不是真的有多出口（只可能是历史数据）。
		 *
		 * 一个能力现在只有一个出口，内部名固定 out —— 这个名字对读者是噪音，
		 * 所以单出口的边不画出口名标签。保留例外是因为老数据里可能有
		 * outputs 长度 > 1 的能力：那时两条边只靠名字区分，不标就分不清。
		 */
		const hasMultiOut = (capId) => {
			const cap = capById.get(capId);
			return !!(cap && Array.isArray(cap.outputs) && cap.outputs.length > 1);
		};

		const deadPortID = (capId, port) => "dead|" + capId + "|" + port;
		// 解析不到（引用指向的能力已被删）时节点 id 退回**引用**：
		// 不能拿空串当 id —— 那会把所有坏边并成一个假节点。
		const endID = (e) => e.toCapabilityId || "ref:" + (e.toRef || "");

		/**
		 * 注册一个能力节点（按 id 全局去重）。
		 * 已存在时**只补标记、不重建**：同一个节点可能先被某条下游边登记、
		 * 之后又被入口边登记（菱形），entry 标记必须能补上 —— 丢这个标记
		 * 正是「源 → 它」那条线消失的 bug。
		 */
		const ensureNode = (id, patch) => {
			let n = nodes.get(id);
			if (!n) {
				const cap = capById.get(id);
				n = {
					id: id,
					// 有显示名就用它。裁决点只有一处：Anotify.capLabel
					// （它活在 partials.js 的 IIFE 里，本文件是独立 IIFE，
					// 只能通过上文的代理访问，不能裸调）。
					label: cap ? capLabel(cap) : id.replace(/^ref:/, ""),
					kind: cap ? cap.kind || "pipe" : "unknown",
					entry: false,
					cycle: false,
					deadport: false,
					unknown: !cap,
				};
				nodes.set(id, n);
			}
			if (patch) Object.assign(n, patch);
			return n;
		};

		/**
		 * 画一条边（按 (from, port, to) 去重）。
		 * 已经画过的边再遇到时只**升级标记**（cycle / muted 取或），不新增一行：
		 * 一条边被多条路径走到时，标记可能是后到的那条路径才看出来的。
		 *
		 * 去重键里的 port **必须保留**：同一对节点之间的多条候选边就是靠它
		 * 区分的（合并了就变成一条自环）。port 留在数据里，只是不一定要画。
		 */
		const putEdge = (from, port, to, cycle, muted) => {
			const key = from + "|" + (port || "") + "|" + to;
			const hit = edges.get(key);
			if (hit) {
				if (cycle) hit.cycle = true;
				if (muted) hit.muted = true;
				return hit;
			}
			const e = {
				from: from,
				to: to,
				port: port || "",
				// 渲染意图：只有多出口能力才把出口名画到边上（见 hasMultiOut）
				showPort: hasMultiOut(from),
				cycle: !!cycle,
				muted: !!muted,
			};
			edges.set(key, e);
			return e;
		};

		// 能力边（fromIsSource === false）按上游 id 分桶。
		// 左端解析不到的边（引用指向已删能力）没有上游节点可挂，画不出来 —— 跳过。
		const capEdges = new Map();
		for (const e of allEdges) {
			if (e.fromIsSource) continue;
			const from = e.fromCapabilityId;
			if (!from) continue;
			if (!capEdges.has(from)) capEdges.set(from, []);
			capEdges.get(from).push(e);
		}

		// ---- ① 入口边是唯一入口 ----
		//
		// 只认 edges 里 fromIsSource 且 fromRef 等于本源名字的那些边。
		// **不要**再从 src.targetCaps 推一遍（那是同一件事的第二份真相：
		// 后端 DetailSource 的 targetCaps 本来就是入口边的投影），
		// 两份一起用就会出现「边上有一条入口边、targetCaps 里却没有」这种
		// 静默不一致，而且没人知道该信哪份。
		//
		// 首层节点打 entry: true —— 渲染层的「源 → 能力」线只认这个标记，
		// 于是 1:N 由**构造**保证：N 条入口边 ⇒ N 个 entry 节点 ⇒ N 条线。
		const entryIDs = [];
		for (const e of allEdges) {
			if (!e.fromIsSource || e.fromRef !== src.name) continue;
			const id = endID(e);
			ensureNode(id).entry = true;
			if (!entryIDs.includes(id)) entryIDs.push(id);
		}

		// ---- ② 沿能力边向下遍历 ----
		//
		// 环的判据是「目标是否在**当前这条路径**上」（path 集合）。它只用来打标记
		// （cycle），绝不用于剪掉节点/边：
		//   · 同一个节点被多条路径到达（菱形）→ 节点全局去重只画一次，
		//     但每条**进入它**的边都要画；
		//   · 环上的回边 → 边照画、节点照留，只标 cycle 且不再递归（否则走不到头）。
		//
		// walked 只挡「同一个节点重复展开」这一件事（不然稠密图会指数爆炸）。
		// 它挡不住边：每条边都由它自己的上游节点在其唯一一次展开里画出来。
		const walked = new Set();
		const walk = (id, path, depth) => {
			if (walked.has(id)) return;
			walked.add(id);
			if (depth > TOPO_MAX_DEPTH) {
				// 超过上限：只标记截断（不假装画全），由调用方决定是否提示
				truncated = true;
				return;
			}
			const cap = capById.get(id);
			if (!cap) return; // 坏引用：没有能力定义就没有出口、也没有下游可走

			const connected = new Set();
			for (const e of capEdges.get(id) || []) {
				const toID = endID(e);
				connected.add(e.fromPort || "");
				const onPath = path.has(toID);
				putEdge(id, e.fromPort, toID, onPath, false);
				const child = ensureNode(toID);
				if (onPath) {
					// 环：只标记。节点和边在上一行就已经落下去了 —— 不吞。
					child.cycle = true;
					continue;
				}
				const next = new Set(path);
				next.add(toID);
				walk(toID, next, depth + 1);
			}

			// 声明了但没接边的出口 → 灰色终点伪节点，让配置遗漏在图上可见。
			// label 是**出口名**（事实）；页面上显示成「（没有下游）」由渲染层决定
			// （见 topoNodeLabel），推导这一层不掺语言。
			for (const port of cap.outputs || []) {
				if (!port || connected.has(port)) continue;
				const did = deadPortID(id, port);
				ensureNode(did, { label: port, kind: "deadport", deadport: true, unknown: false });
				// 指向灰点的边一律弱化：它表示「这条路到不了任何地方」
				putEdge(id, port, did, false, true);
			}
		};

		// 每个入口节点各起一条路径。同一个能力有多条入口边时只走一遍
		// （entryIDs 已按节点去重，节点/边的画法不受影响）。
		for (const id of entryIDs) walk(id, new Set([id]), 0);

		// ---- ③ 同名多版本：同一张图里必须能区分 ----
		//
		// capLabel 默认把版本藏起来（显示名是「回声（示例）」而不是
		// 「回声（示例）@2」），因为界面上一个能力只该有一个名字。
		// 但同一张图里可能并排出现 touch@1 与 touch@2 —— 两行文字一模一样，
		// 读者分不清是两个版本还是画重了。所以只对**同名出现多次**的节点
		// 改用 capLabel(cap, { withVersion: true })；只出现一次时保持现状。
		//
		// 显示名规则仍然只在 Anotify.capLabel 里裁决，这里不自己拼。
		const nameCount = new Map();
		for (const n of nodes.values()) {
			const cap = capById.get(n.id);
			if (!cap) continue; // 灰点（出口伪节点）/ 坏引用没有能力定义
			nameCount.set(cap.name, (nameCount.get(cap.name) || 0) + 1);
		}
		for (const n of nodes.values()) {
			const cap = capById.get(n.id);
			if (!cap) continue;
			if ((nameCount.get(cap.name) || 0) > 1) {
				n.label = capLabel(cap, { withVersion: true });
			}
		}

		return {
			nodes: Array.from(nodes.values()),
			edges: Array.from(edges.values()),
			truncated: truncated,
		};
	}

	/**
	 * topoNodeLabel —— 拓扑节点在**图上**显示的名字。
	 *
	 * 拓扑本身不带语言（sourceTopology 只输出配置事实，deadport 的 label 是出口名），
	 * 所以「（没有下游）」这类人话留在这里翻译。两个渲染入口（capabilityFlow 与
	 * taskChain）都必须走这一个函数，否则同一个灰点在两页会显示成两个名字。
	 *
	 * 灰点现在表达的是「这个能力不会再触发任何能力」（终端能力本身不是问题，
	 * 是**声明了输出却没接**才画灰点），不是「哪条接线缺了」。
	 */
	function topoNodeLabel(n, t) {
		if (n && n.deadport) return t("sources.flow.unconnected", "（没有下游）");
		return (n && n.label) || "";
	}

	/**
	 * flowSub —— 源详情页节点的副标题。
	 *
	 * 这里只讲**结构身份**（终点 / 没有下游 / 已删除），不是执行状态：
	 * 源详情页没有执行事实，拿状态词上去就是编造。
	 */
	function flowSub(n, t) {
		if (n.deadport) return t("sources.flow.no_edge", "没有下游");
		if (n.unknown) return t("sources.flow.missing", "能力已删除");
		if (n.kind === "end") return t("sources.flow.terminal_short", "终点");
		return "";
	}

	// ==================== 流向图（静态配置） ====================

	/**
	 * flowLegend 构建图例。
	 *
	 * 两个视图（源详情页的拓扑图、trace 页的叠加图）共用一套编码：
	 *   · 图标 = 结构身份（源 / 管道 / 终点）
	 *   · 颜色 = 状态（仅 trace 页有执行数据）
	 *   · 深浅 = 这条路走了多少次
	 *
	 * 之前图例是 trace 页里硬拼的数组，源详情页压根没有 —— 同一个组件
	 * 两个页面长得不一样。现在按 scope 生成，加一条只改这一处。
	 *
	 * @param {"trace"|"source"} scope
	 */
	function flowLegend(scope, t) {
		// 没有底图时图上只有一个维度（执行状态），图例没有意义
		if (scope === "none") return [];
		const rows = [
			{ icon: "⚡", text: t("trace.legend_origin", "源 · 触发器（事件从这里进来）") },
			{ icon: "◆", text: t("trace.legend_pipe", "管道 · 会继续往下派发") },
			{ icon: "◉", text: t("trace.legend_end", "终点 · 到此结束，不再派发") },
		];
		if (scope === "trace") {
			rows.push(
				{ icon: "✓", cls: "lg-ok", text: t("trace.legend_ok", "执行成功") },
				{ icon: "✕", cls: "lg-bad", text: t("trace.legend_bad", "执行失败") },
				{
					icon: "○",
					cls: "lg-pruned",
					text: t("trace.legend_pruned", "未执行（配置里有、这次没走）"),
				},
			);
		} else {
			// 源详情页没有执行数据，但有"声明了输出却没接下游"这种配置问题
			rows.push({
				icon: "○",
				cls: "lg-pruned",
				text: t("sources.flow.no_edge", "没有下游"),
			});
			// 配置预览里没有「走了几次」这个维度（每条边只描出一遍），
			// 所以 ↺ 只解释环本身：图会绕回来。别把 trace 页的 "×N" 说法
			// 搬过来 —— 预览中没有数字，图例说了也是空谈。
			rows.push({
				icon: "↺",
				cls: "lg-hot",
				text: t("sources.flow.cycle", "环 · 这条边会绕回来"),
			});
			return rows;
		}
		rows.push({
			icon: "↺",
			cls: "lg-hot",
			text: t("trace.legend_hot", "线与数字越深 · 这条路走了很多次（重试/回环）"),
		});
		return rows;
	}

	/**
	 * renderFlow 装配 flograph 的渲染参数。
	 *
	 * **所有"图上该有什么"的开关只在这里决定**：图例、全屏入口、布局方向、
	 * 间距。两个视图都走这个函数 —— 否则 trace 页加了东西、源详情页忘了，
	 * 同一个组件在两个页面长得不一样（图例就是这么丢的）。
	 */
	// 入场动画每次**页面加载**只播一次。
	//
	// 用户要的是"打开的时候有动画"；每次在瀑布图/图之间来回切都重播
	// 2.4s，对比视图时会一直看到一片灰，反而是干扰。
	let enterAnimated = false;

	function renderFlow(host, spec, silk) {
		const t = spec.t;
		const nodeCount = (spec.nodes || []).length;
		const longChain = nodeCount > 7;
		return render(host, {
			nodes: spec.nodes,
			edges: spec.edges,
			legend: flowLegend(spec.scope || "trace", t),
			// 全屏入口每页只出现一次：有标题栏文字入口的页面不画工具栏图标
			showFullscreen: !!spec.showFullscreen,
			// 入场动画只对"有执行事实"的图有意义 —— 源详情页没有执行数据，
			// 但预览有自己的节拍（见 capabilityFlow），所以是否播放由调用方决定。
			animate: !!spec.animate,
			// 节拍（{cap, from, port}）与触发器节点 id：flograph 靠它决定
			// "第几拍点亮谁、什么时候画线"。trace 页来自任务时间，源详情页
			// 来自拓扑顺序（previewBeats）—— **同一套播放机器**。
			beats: spec.beats || [],
			root: spec.root || "",
			// preview = 配置预览（源详情页）：没有执行事实，文案与状态色
			// 不能借用 trace 的「成功/失败」。渲染机器仍是同一套。
			preview: !!spec.preview,
			rankdir: spec.rankdir || (longChain ? "TB" : "LR"),
			rankSep: spec.rankSep != null ? spec.rankSep : longChain ? 40 : 96,
			nodeSep: spec.nodeSep != null ? spec.nodeSep : longChain ? 22 : 26,
			t: t,
			onNodeClick: spec.onNodeClick,
		}, { silk: !!silk });
	}

	/**
	 * previewBeats —— 配置拓扑的**预览节拍**：把整张图按「从源出发的广度
	 * 顺序」走一遍，每条边一拍。
	 *
	 * 为什么不能复用 trace 的 beats：那是执行事实（任务的 startedAt），
	 * 源详情页没有任务，也不能编一个「发生过」的顺序。这里要的是
	 * **配置事实的阅读顺序**：源 → 首层 → 再下游，同层按边的插入序。
	 *
	 * 为什么每条边一拍而不是每个节点一拍：silk 的 drawIncoming 按
	 * (from,to,port) 匹配边，一个节点有多个上游时，只有逐边的节拍才能
	 * 保证**每条边都真的被描出来**（「流向所有的节点和边」）。节点在它
	 * 的第一条入边到达时点亮，之后再到达就脉冲一下 —— 和环上重访同一
	 * 语义。
	 *
	 * 广度（BFS）而不是深度（DFS）：图是「源往下游摊开」的形状，按层
	 * 推进才读得出「先到这一层、再到下一层」；DFS 会一条分支扎到底，
	 * 回来时读者已经忘了起点在哪。
	 *
	 * 兜底：不可达的子图（截断/坏数据）按 nodes 的插入序补一个起点，
	 * 保证「所有节点」都会被点亮，而不是静默留在灰态。
	 *
	 * @param {Array} nodes  {id}
	 * @param {Array} edges  {from,to,port}
	 * @param {string} rootId
	 * @returns {Array<{cap:string, from:string, port:string}>} 与 trace 的
	 *          beats **同一形状**（渲染层因此不需要第二套播放机器）。
	 */
	function previewBeats(nodes, edges, rootId) {
		const adj = new Map();
		for (const e of edges) {
			if (!adj.has(e.from)) adj.set(e.from, []);
			adj.get(e.from).push(e);
		}

		const beats = [];
		const seen = new Set();
		const walked = new Set(); // 边对象只走一次（稠密图里防重复拍）
		const queue = [];
		const enqueue = (id) => {
			if (!id || seen.has(id)) return;
			seen.add(id);
			queue.push(id);
		};

		enqueue(rootId);
		let i = 0;
		for (;;) {
			// queue 会在遍历中增长：i < queue.length 每轮重新求值，就是 BFS。
			while (i < queue.length) {
				const cur = queue[i++];
				for (const e of adj.get(cur) || []) {
					if (walked.has(e)) continue;
					walked.add(e);
					beats.push({ cap: e.to, from: e.from, port: e.port || "" });
					enqueue(e.to);
				}
			}
			// 不可达子图：换一个未访问的节点当起点继续走（不留灰块）。
			const next = nodes.find((n) => !seen.has(n.id));
			if (!next) break;
			enqueue(next.id);
		}
		return beats;
	}

	/**
	 * capabilityFlow —— 把「源 → 能力 → 下游」拓扑画成流向图
	 *
	 * **这是一个纯渲染层**：拓扑推导不在这里，在 sourceTopology（本文件顶部）。
	 * 它只做三件事：
	 *   ① 把中立拓扑映射成 FlowGraph 的节点/边形状（图标、图例、配色、文案）；
	 *   ② 补上源节点，并按 topology.nodes 的 entry 标记**逐个**连线；
	 *   ③ 交给 renderFlow（与 trace 页共用的渲染参数）与 **silk 渲染器**。
	 *
	 * 与 trace 的差别只有「节拍从哪来」：那边是任务时间，这里是拓扑顺序
	 * （previewBeats）。播放/描线/相机/回放条都是同一套。
	 *
	 * @param {HTMLElement} host        容器（会被清空）
	 * @param {Object} opts
	 * @param {Object} opts.src         源对象 { name, kind }
	 * @param {{nodes: Array, edges: Array, truncated?: boolean}} opts.topology
	 *                                  sourceTopology() 的产物
	 * @param {Function} [opts.t]        i18n
	 * @param {Function} [opts.onNodeClick]  回调参数是节点对象（不是 id）
	 * @param {number} [opts.limit]
	 * @param {boolean} [opts.showFullscreen]
	 */
	function capabilityFlow(host, opts) {
		const t = opts.t || ((k, d) => d || k);
		const src = opts.src || {};
		const topo = opts.topology || { nodes: [], edges: [] };
		const limit = opts.limit || 120;

		// 触发器节点（源本身）。它不属于拓扑推导：拓扑回答「源往下游怎么连」，
		// 而它是这个图的坐标系原点。
		const SRC_ID = "__source__";
		const kindLabel = {
			cron: t("sources.kind.cron", "定时器"),
			webhook: t("sources.kind.webhook", "Webhook"),
			manual: t("sources.kind.manual", "手动"),
		};

		const nodes = [
			{
				id: SRC_ID,
				label: src.name || t("sources.flow.source", "源"),
				sub: kindLabel[src.kind] || "",
				kind: "trigger",
				role: "trigger",
				root: true,
				icon: ICON.trigger,
			},
		];
		for (const n of topo.nodes) {
			nodes.push({
				id: n.id,
				// 图节点不带版本号：视觉空间有限，版本放悬停标题里（capLabel 已裁）
				// 例外：同名多版本同图时由 sourceTopology 改用 withVersion 重写 label
				// （否则两行字一模一样，读者分不清是哪个版本）。
				label: topoNodeLabel(n, t),
				sub: flowSub(n, t),
				kind: n.kind,
				// 源详情页画的**就是**结构身份（这里的 kind 不是执行状态），
				// 所以原样传成 role —— 和 trace 页的图共用同一套 role 样式，
				// 两处看起来才一致。
				role: n.kind,
				root: false,
				entry: !!n.entry,
				cycle: !!n.cycle,
				// deadport / unknown 是结构身份的一部分：图标和配色已经从 kind 里
				// 区分了，这两个布尔值原样带上（flograph 不读它们），是为了让
				// host._graphSpec 能如实回答"图上这个点是什么"（调试 / e2e）。
				deadport: !!n.deadport,
				unknown: !!n.unknown,
				icon: ICON[n.kind] || "◆",
			});
		}

		const edges = topo.edges.map((e) => ({
			from: e.from,
			to: e.to,
			// 出口名只在多出口（历史数据）时画；单出口的内部名（固定 out）
			// 对读者是噪音。topo 里的 port 仍保留（去重/调试需要），只是不传给渲染层。
			port: e.showPort ? e.port : "",
			cycle: !!e.cycle,
			// 环上的边绕出去画，和 trace 页同一个形状。
			// 不传这个的话回边就是两节点之间的一小段斜线，看着像画坏了。
			loopback: !!e.cycle,
			muted: !!e.muted,
		}));

		// 源节点 → 首层能力：topology 里每个 entry 节点**逐个**连一条。
		//
		// 不在遍历时判「谁是首层」（那种状态标记曾经在菱形上丢线），
		// 而是按节点身上的 entry 事实连 —— N 条入口边就是 N 条线，这是构造保证。
		for (const n of topo.nodes) {
			if (n.entry) edges.push({ from: SRC_ID, to: n.id, port: "" });
		}

		// 截断只影响显示（数据异常时防卡死），不改变推导结果
		const cut = truncate(nodes, edges, limit);

		// 预览节拍：从源出发、按广度顺序把**每一条边**都走一遍。
		// 这一步只回答「以什么顺序读这张图」，不是执行事实。
		const beats = previewBeats(cut.nodes, cut.edges, SRC_ID);

		const graph = renderFlow(
			host,
			{
				scope: "source",
				t: t,
				// 工具栏全屏图标：和 trace 页保持一致的位置与图标
				showFullscreen: !!opts.showFullscreen,
				nodes: cut.nodes,
				edges: cut.edges,
				// 配置视图也要有「流向」：节拍来自拓扑顺序（previewBeats），
				// 渲染层用与 trace 完全相同的播放机器放一遍。
				animate: true,
				beats: beats,
				root: SRC_ID,
				// preview 只影响**文案与状态色**（没有执行事实，不能说「全部成功」），
				// 不改变拓扑事实。
				preview: true,
				onNodeClick: (n) => {
					if (n.kind === "deadport" || n.kind === "unknown" || n.kind === "trigger") return;
					if (opts.onNodeClick) opts.onNodeClick(n);
				},
			},
			// **同一个渲染器**：silk 是 trace 页链路图的那一套（1:1 舞台 +
			// 丝带 + 回放条）。两页长得一样，用户才不会以为是两个功能。
			true,
		);

		if (host) host._graphRebuild = () => capabilityFlow(host, opts);
		// 两种截断合并上报：节点数硬顶（limit）与推导深度上限（TOPO_MAX_DEPTH）。
		// 调用方想提示就拿这个（现在两页都不提示，跟改动前一致）。
		return { graph, truncated: cut.truncated || !!topo.truncated };
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

		// 执行节拍：把这次执行**按真实时间**排成一串能力。
		//
		// 入场动画一拍一拍地播：触发器 → 它的线 → 第一个能力 → 它的线 →
		// 下一个能力…… 所以这里要的是"任务执行顺序"，不是"图上的节点顺序"。
		//
		// 用真实时间（startedAt）而不是数组下标：后端返回顺序不保证是
		// 执行顺序。同一能力出现多次时**保留多个节拍** —— 重试本来就该
		// 在动画里看得见（否则环上那三次 run-tests 只亮一次）。
		// 每拍还要带"把流程带到这里"的那条边：
		//   from = 父任务的能力，port = 父任务走的是哪个出口，cap = 本任务能力
		//
		// 有了这个，渲染层才能做到"先画出进入这个节点的那条线，再点亮它"。
		// 只知道节点顺序的话，一条边只能跟着它的**源**节点一起画 —— 像
		// run-tests→deploy-preview 就会在第一次跑测试时就画完，而那时
		// 部署还早着（issue #157 里"看不到线进入"的观感就是这么来的）。
		const sortedTasks = tasks.slice().sort(
			(a, b) =>
				(a.startedAt != null ? a.startedAt : a.createdAt) -
					(b.startedAt != null ? b.startedAt : b.createdAt) ||
				(a.id < b.id ? -1 : 1),
		);
		const capOfTaskID = new Map(tasks.map((tk) => [tk.id, tk.capabilityId]));
		const outOfTaskID = new Map(tasks.map((tk) => [tk.id, tk.out]));
		const beats = sortedTasks
			.filter((tk) => tk.capabilityId)
			.map((tk) => ({
				cap: tk.capabilityId,
				from: tk.parentTaskId ? capOfTaskID.get(tk.parentTaskId) || "" : "",
				port: tk.parentTaskId ? outOfTaskID.get(tk.parentTaskId) || "" : "",
			}));

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
					//
					// 灰点（deadport）不说「未执行」：它永远执行不了（没接下游）。
					return {
						id: n.id,
						// 底图节点来自 sourceTopology，label 是配置事实；
						// 「（没有下游）」这类人话统一在 topoNodeLabel 里翻译。
						label: topoNodeLabel(n, t),
						sub: n.deadport ? "" : t("trace.legend_pruned", "未执行"),
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
					label: topoNodeLabel(n, t),
					sub: bits.join(" · "),
					kind: kindForStatus(st),
					icon: roleIcon,
					role: n.kind,
					entry: n.entry,
					badge: STATUS_COLOR[st] || STATUS_COLOR.pending,
					accent: st === "failed" ? STATUS_COLOR.failed : null,
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
				const key = e.from + "\u0000" + e.to;
				const walked = walkCount.get(key) || 0;
				const isCycle = backEdges.has(key);
				return {
					from: e.from,
					to: e.to,
					port: e.showPort ? e.port : "",
					// 底图边来自 sourceTopology，和源页同一个开关：只有多出口能力
					// （历史数据）才把出口名画到边上。
					// **真实**遍历次数。边上要标 "×N"（多出口时才带名字），
					// 这个数必须是事实，不能掺"这是环所以 +1"的调味 —— 环上那条边
					// 可能就只走了一遍，标 ×2 是撒谎。
					times: walked,
					// 渲染档位（颜色深浅/粗细）是另一件事：在环上意味着
					// "这段路会回来"，所以即使只走一遍也该比直线更实。
					heat: walked + (isCycle ? 1 : 0),
					// 回边要绕出去画：否则它只是两个相邻节点之间的一小段，
					// 看不出"这里是绕回来的"。渲染层据此把路径鼓出弧线。
					loopback: isCycle,
					// 这次**没走过**的边也画成虚线灰掉，和剪枝节点同一个语义：
					// "配置里有这条路，这次没走"。
					//
					// 只靠"不参与动画"是不够的：入场动画一开始会把走过的元素
					// 压暗，而没走过的边如果保持正常实线，就会在那一瞬间显得
					// **比走过的还亮** —— 看着像"这条线一开始就点亮了"，还分
					// 不清哪条路这次真的用了（issue #157 第二张截图）。
					//
					// e.muted 来自拓扑推导：「这条边指向一个没接下游的出口」
					// —— 那是配置层面就到不了，与"这次没走"是两件事，都得灰。
					muted: !bothAlive || walked === 0 || !!e.muted,
				};
			});

			// ---- 源节点：这条 trace 是谁触发的 ----
			//
			// 没有它，图只是"一堆能力怎么连"，看不出这个 trace 的**起点** ——
			// 而"谁触发"恰恰是排查的第一步。
			if (opts.sourceLabel) {
				const SRC_NODE_ID = "__trigger__";
				// 本次真正的根任务（触发器直接拉起的能力）
				const rootCaps = new Set(
					tasks.filter((tk) => !tk.parentTaskId).map((tk) => tk.capabilityId),
				);
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
				const executedCaps = new Set(tasks.map((tk) => tk.capabilityId));
				for (const n of entries) {
					const isRoot = rootCaps.has(n.id);
					// 这次不是它起跑、但它确实执行了 —— 别硬连一条触发器过来。
					//
					// 它有自己的入边（谁派发的它，图上已经画了）。再补一条
					// 触发器→它 就是编造：比如部署预发这次是第 3 次跑测试
					// 派发的，跟"PR 同步事件"没关系。
					if (!isRoot && executedCaps.has(n.id)) continue;
					edges.push({
						from: SRC_NODE_ID,
						to: n.id,
						// 出口名只有**真被这次事件拉起**时才写。
						//
						// 触发器节点标的是具体事件（如 pull_request.synchronize），
						// 而"源也接了别的入口"（部署预发是被 http/manual 源接的）
						// 跟这个事件无关 —— 把 sourcePort 一律写上去等于说
						// "这个事件能触发它"，是假的。
						port: isRoot ? opts.sourcePort || "" : "",
						// 没被这次走用的入口要显式画成"未走"（虚线），
						// 不能只靠"不参与动画"来表达 —— 那会让它在整场
						// 入场动画里以满不透明度的实线亮着，看着像一开始
						// 就点亮了（issue #157 的第二张截图）。
						muted: !!n.dead || !isRoot,
						times: isRoot ? 1 : 0,
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
					badge: STATUS_COLOR[st] || STATUS_COLOR.pending,
					accent: st === "failed" ? STATUS_COLOR.failed : null,
					_task: tk,
				};
			});

			edges = [];
			for (const tk of tasks) {
				if (!tk.parentTaskId || !byId.has(tk.parentTaskId)) continue;
				edges.push({
					from: tk.parentTaskId,
					to: tk.id,
					// 无底图分支拿不到能力定义，判不了「多出口」与否 —— 所以**不标出口名**：
					// 单出口是常态，把 `out` 挂在线上对读者只是噪音（与有底图时同一口径）。
					port: "",
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
		// 布局方向由 renderFlow 统一按节点数决定，这里不再重复算一遍。
		const graph = renderFlow(host, {
			// 有底图时才有"执行状态"这一层可讲；只画执行链时图例没有意义
			scope: base && base.nodes && base.nodes.length ? "trace" : "none",
			// 首次进入图视图时按真实执行顺序点亮（排序信息来自任务时间）
			animate: !enterAnimated,
			beats: beats,
			root: opts.sourceLabel ? "__trigger__" : "",
			t: t,
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
			// ✅ 唯一一处 silk：**只有 trace 页的链路图**换成 Silk 皮肤。
			//
			// 为什么钉在这里：web/traceview.js 冻结了（不传任何皮肤参数），
			// 而 source 页的 capabilityFlow 走同一个 renderFlow —— 默认必须是
			// 原 FlowGraph，否则源详情页会跟着变样（那张图有它自己的 fit/
			// 坐标断言）。第三个参数就是"这一张图用哪种皮肤"的唯一切换点。
		}, true);

		// 真的播过了才置位：图还没数据（切页早于数据到）时不该把机会用掉。
		if (graph && graph._debug && graph._debug.animated && graph._debug.animated()) {
			enterAnimated = true;
		}

		if (host) host._graphRebuild = () => taskChain(host, opts);
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
		// 拓扑推导（唯一一份）：源详情页的流向图与 trace 页的底图都从这里来。
		// 导出它而不是只导渲染函数，是为了让两个页面真的调**同一个**函数
		// （否则下一个人又会"顺手"在页面里再写一份推导）。
		sourceTopology,
		// 预览节拍（唯一一份）：源详情页的「按顺序流过所有节点和边」就是它。
		// 导出给 E2E 直接验顺序（与页面上卡片步号对账），而不是靠截图猜。
		previewBeats,
		capabilityFlow,
		taskChain,
		fmtDur,
		// 供其它页面复用同一套状态配色（当前主题快照；主题切换后就地刷新）
		STATUS_COLOR,
		STATUS_ICON,
		// 主题切换后重绘所有已挂载的图（partials.js 派发 anotify:theme 时自动调用）
		redraw: redrawAll,
	};
})();
