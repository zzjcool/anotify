/**
 * Anotify 图适配层 —— 在 FlowGraph 之上，把 **v4** 数据形状映射成图
 *
 * 分两层是故意的：
 *
 *   web/flograph.js  通用渲染器（布局 / 节点 / 连线 / 缩放），与业务无关
 *   web/graphs.js    本模块：能力与源 → 流向图；任务链 → 拓扑图
 *
 * 这样"画一个图"的能力只有一份（flograph.js），而"我们的数据怎么变成图"
 * 也只有一份（这里）。页面只负责：取数据 → 调这里的一个函数。
 *
 * v4 口径（`docs/design/v4/capability.md` / `source.md` / `api-conventions.md`）：
 *
 *   · 能力只有 `spec.next`（裸名或 `name@version`）：**没有端口、没有优先级、
 *     没有候选、没有下游实体**。终端能力 = 空 next。
 *   · 源入口来自 `source.spec.targets`，能力下游来自 `capability.spec.next`；
 *     `GET .../edges` 是它们的**只读投影** `{from, to, kind}`（v4 CapabilityEdge；
 *     kind ∈ capability|source，没有 port/priority/position）。
 *     图上的边因此没有"出口名"要标 —— 那条视觉编码整个删掉了（flograph 的
 *     `port` 允许省略/空串）。
 *   · 任务字段走 v4：`metadata.name` / `status.phase`（小写六态）/ `status.attempt` /
 *     `spec.parentTaskId` / `spec.capability.name`。v3 的 `out` / 九态 / `capabilityId`
 *     不再存在，本模块也不再读它们。
 *   · 未解析的引用（`next` 指向一个不存在的能力）画成 `unknown` 节点 —— 那是
 *     `DownstreamResolved=False` 在图上的样子，不是"出口没接下游"。
 *
 * 两个图的语义差别（曾经混过一次，这里写死）：
 *
 *   流向图（capabilityFlow）  静态配置视图（**预览**）
 *     问"可能流向哪些能力"。一次画出**所有**声明的 next / targets，包括没走过的分支。
 *     渲染器与 trace 页的链路图**是同一个**（flograph 的 Silk 舞台），
 *     节拍按拓扑顺序合成（previewBeats），走同一套播放机器。
 *
 *   任务链图（taskChain）     动态运行视图
 *     问"这次执行实际走了哪条路径"。数据来自 trace 的 `tasks[]`。
 *
 *     扇出依然合法：一个任务 emit 两次就是两个子任务，它们的 `parentTaskId`
 *     都指向同一个父任务。因此一条 trace 的数据形状是**森林/树**，不是单链。
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
	};

	// v4 六个 phase → 颜色。与瀑布图、状态点用同一套（唯一来源是 tokens.css 的
	// --st-*），跨视图一致；亮/暗主题各自给值，切换主题时重新取色。
	//
	// 这里保留一份十六进制**兜底值**：file:// 直开或 CSS 还没加载时
	// getComputedStyle 拿不到变量，没有兜底图就会全白。
	//
	// ⚠️ 拼写：v4 的 phase 是 `cancelled`（两个 l），而 tokens.css / ui.css 里的
	// 变量与类名是历史的 `canceled`（一个 l）。映射只在这里做一处 ——
	// 不去 tokens.css 里加一个几乎同名的变量（两份迟早漂移）。
	const STATUS_VAR = {
		pending: "--st-pending",
		running: "--st-running",
		succeeded: "--st-succeeded",
		failed: "--st-failed",
		cancelled: "--st-canceled",
		abandoned: "--st-abandoned",
	};
	const STATUS_FALLBACK = {
		pending: "#71717a",
		running: "#22c55e",
		succeeded: "#10b981",
		failed: "#ef4444",
		cancelled: "#a1a1aa",
		abandoned: "#f97316",
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
		running: "▶",
		succeeded: "✓",
		failed: "✕",
		cancelled: "⊘",
		abandoned: "⊘",
	};

	// v4 phase → **CSS 类名后缀**（ui.css 的 .st-* / .b-* 用的是历史拼写）。
	// 与 STATUS_VAR 一一对应；导出给 traceview.js 用，免得两处长成两份。
	const STATUS_CLASS = {
		pending: "pending",
		running: "running",
		succeeded: "succeeded",
		failed: "failed",
		cancelled: "canceled",
		abandoned: "abandoned",
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

	/** 资源信封的 metadata.name（v4 的名字在 metadata 里，不在一级字段）。 */
	function metaName(res) {
		return res && res.metadata && res.metadata.name ? String(res.metadata.name) : "";
	}

	/** `name@version` → name：图上的节点 id 用**名字**（版本只影响解析结果）。 */
	function refName(ref) {
		const s = String(ref == null ? "" : ref).trim();
		const at = s.lastIndexOf("@");
		if (at <= 0) return s;
		const v = Number(s.slice(at + 1));
		return Number.isInteger(v) && v > 0 ? s.slice(0, at) : s;
	}

	function nextOf(cap) {
		const next = cap && cap.spec ? cap.spec.next : null;
		return Array.isArray(next) ? next : [];
	}
	function targetsOf(src) {
		const targets = src && src.spec ? src.spec.targets : null;
		return Array.isArray(targets) ? targets : [];
	}
	/** sources 入参既可以是数组，也可以只给一个源（详情页）。 */
	function normSources(input) {
		if (Array.isArray(input.sources)) {
			return input.sources.filter((s) => s && s.metadata);
		}
		if (input.source && typeof input.source === "object") return [input.source];
		return [];
	}

	/**
	 * sourceTopology —— 唯一一份「源 → 能力 → 下游」拓扑推导（v4）。
	 *
	 * 输入是**配置事实**（源 + 能力表 + 可选边表），输出是 FlowGraph 中立的 {nodes, edges}。
	 * 源详情页的流向图与 trace 页的底图都从这里来 —— 两个页面看到的图必然一致。
	 * 为什么必须只有一份：这两页曾经各自写过一份推导，同一个源在两页画出不同的图。
	 *
	 * 1:N 由**构造**保证：每条入口边产出一个 entry 节点，渲染层逐个连线 ——
	 * 不依赖任何「首次访问」式的遍历状态（那是老实现丢线的根因）。
	 *
	 * @param {{sources?: Array, source?: Object|string, capabilities?: Array, edges?: Array}} input
	 *   sources       源资源数组（取 `spec.targets` 当入口边；也用来认名字）
	 *   source        可选：只画这一个源的入口（资源对象或 name）
	 *   capabilities  能力资源数组（取 `spec.next` 当能力边）
	 *   edges         可选：`GET .../edges` 的 items（**只**用来取 position；
	 *                 拓扑事实仍然只有 spec 一份，不引入第二份真相）
	 * @returns {{nodes: Array, edges: Array, truncated: boolean}}
	 *   节点 {id, label, kind(pipe|end|unknown), entry, cycle, unknown}
	 *   边   {from, to, port:"", cycle, muted}
	 */
	function sourceTopology(input) {
		const o = input || {};
		const sources = normSources(o);
		const caps = (o.capabilities || []).filter((c) => c && c.metadata);
		const capByName = new Map(caps.map((c) => [metaName(c), c]));
		const only =
			o.source && typeof o.source === "string" ? o.source : o.source ? metaName(o.source) : "";
		const edgeList = Array.isArray(o.edges) ? o.edges : [];

		// 节点按名字全局去重、边按 (from,to) 去重。用 Map 是为了保住**插入序**
		//（布局稳定，diff 也稳定）。
		const nodes = new Map();
		const edges = new Map();
		let truncated = false;

		const ensureNode = (name, patch) => {
			let n = nodes.get(name);
			if (!n) {
				const cap = capByName.get(name);
				n = {
					id: name,
					label: cap ? capLabel(cap) : name,
					// 结构身份：终端能力（空 next）= end；未解析的引用 = unknown
					kind: !cap ? "unknown" : nextOf(cap).length ? "pipe" : "end",
					entry: false,
					cycle: false,
					unknown: !cap,
				};
				nodes.set(name, n);
			}
			if (patch) Object.assign(n, patch);
			return n;
		};

		/**
		 * 画一条边（按 (from,to) 去重）。已画过的边再遇到时只**升级标记**。
		 * v4 没有端口：`port` 恒为空串（flograph 允许省略/空串，不画节拍）。
		 */
		const putEdge = (from, to, cycle, muted) => {
			const key = from + "|" + to;
			const hit = edges.get(key);
			if (hit) {
				if (cycle) hit.cycle = true;
				if (muted) hit.muted = true;
				return hit;
			}
			const e = { from: from, to: to, port: "", cycle: !!cycle, muted: !!muted };
			edges.set(key, e);
			return e;
		};

		// ---- ① 收集边：能力下游来自 spec.next，源入口来自 spec.targets ----
		// 边的 wire 形状是 v4 `{from, to, kind}`：没有 position/port 字段，
		// 所以布局位置只按推导顺序给（稳定，不读服务端没给的字段）。
		const pairs = [];
		for (const cap of caps) {
			for (const ref of nextOf(cap)) {
				if (!ref) continue;
				pairs.push({ fromIsSource: false, fromRef: metaName(cap), toRef: String(ref) });
			}
		}
		for (const src of sources) {
			const name = metaName(src);
			if (only && name !== only) continue;
			for (const tgt of targetsOf(src)) {
				if (!tgt || !tgt.capability) continue;
				pairs.push({ fromIsSource: true, fromRef: name, toRef: String(tgt.capability) });
			}
		}
		// 只拿到边表、没有源资源时（页面可能只查 edges），入口边也认
		if (!pairs.some((p) => p.fromIsSource)) {
			for (const e of edgeList) {
				if (!e || e.kind !== "source") continue;
				if (only && String(e.from) !== only) continue;
				pairs.push({
					fromIsSource: true,
					fromRef: String(e.from),
					toRef: String(e.to),
					position: e.position,
				});
			}
		}
		// position：推导顺序（v4 的边投影不携带 position，别假装有）
		pairs.forEach((p, i) => {
			p.position = i + 1;
		});
		pairs.sort((a, b) => (a.position || 0) - (b.position || 0));

		// ---- ② 入口节点：每条入口边一个 entry 标记（N 条边 ⇒ N 条线）----
		const entryNames = [];
		for (const p of pairs) {
			if (!p.fromIsSource) continue;
			const name = refName(p.toRef);
			if (!name) continue;
			ensureNode(name).entry = true;
			if (entryNames.indexOf(name) < 0) entryNames.push(name);
		}

		// ---- ③ 沿 next 向下遍历 ----
		// 环的判据是「目标是否在**当前这条路径**上」（path 集合）：只打标记、不剪节点/边。
		// walked 只挡「同一个节点重复展开」（不然稠密图会指数爆炸），挡不住边。
		const outEdges = new Map();
		for (const p of pairs) {
			if (p.fromIsSource) continue;
			if (!outEdges.has(p.fromRef)) outEdges.set(p.fromRef, []);
			outEdges.get(p.fromRef).push(p);
		}
		const walked = new Set();
		const walk = (name, path, depth) => {
			if (walked.has(name)) return;
			walked.add(name);
			if (depth > TOPO_MAX_DEPTH) {
				truncated = true;
				return;
			}
			if (!capByName.has(name)) return; // 未解析的引用：节点已以 unknown 留下，没有下游
			for (const p of outEdges.get(name) || []) {
				const to = refName(p.toRef);
				if (!to) continue;
				const onPath = path.has(to);
				putEdge(name, to, onPath, false);
				const child = ensureNode(to);
				if (onPath) {
					// 环：只标记。节点和边都已经落下了 —— 不吞。
					child.cycle = true;
					continue;
				}
				const nextPath = new Set(path);
				nextPath.add(to);
				walk(to, nextPath, depth + 1);
			}
		};
		// 每个入口节点各起一条路径（entryNames 已按名字去重）
		for (const name of entryNames) walk(name, new Set([name]), 0);

		return {
			nodes: Array.from(nodes.values()),
			edges: Array.from(edges.values()),
			truncated: truncated,
		};
	}

	/**
	 * topoNodeLabel —— 拓扑节点在**图上**显示的名字。
	 *
	 * 拓扑本身不带语言（sourceTopology 只输出配置事实），所以这里只负责取名字；
	 * 结构身份的人话留给 flowSub。两个渲染入口（capabilityFlow 与 taskChain）
	 * 都必须走这一个函数，否则同一个节点在两页会显示成两个名字。
	 */
	function topoNodeLabel(n) {
		return (n && n.label) || "";
	}

	/**
	 * flowSub —— 节点副标题里的**结构身份**（终点 / 未解析 / 其他）。
	 *
	 * 这里不是执行状态：源详情页没有执行事实，拿状态词上去就是编造。
	 * v4 没有"声明了出口却没接下游"这种状态 —— 空 next 就是终端能力，
	 * 那是正常形状，不该画灰点。
	 */
	function flowSub(n, t) {
		if (n && n.unknown) return t("sources.flow.missing", "下游能力不存在");
		if (n && n.kind === "end") return t("sources.flow.terminal_short", "终点");
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
			// 源详情页没有执行数据。v4 里"声明了出口却没接下游"不存在（空 next
			// 就是终端能力），所以图例只需要解释环。
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
			// idle：只摆成"一拍还没开始"的定格、不自动播。首页那张卡在折叠线
			// 以下，自动播等于在没人看的时候放完；它自己按"进入视口"再 replay。
			idle: !!spec.idle,
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
				// 图节点不带版本号：视觉空间有限。v4 里一个名字只有一版，
				// 所以这里不需要"同名多版本"消歧义。
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
				// unknown（引用解析不到）是结构身份的一部分：图标和配色已经从
				// kind 里区分了，这个布尔值原样带上（flograph 不读它），是为了让
				// host._graphSpec 能如实回答"图上这个点是什么"（调试 / e2e）。
				unknown: !!n.unknown,
				icon: ICON[n.kind] || "◆",
			});
		}

		const edges = topo.edges.map((e) => ({
			from: e.from,
			to: e.to,
			// v4 没有端口：边上一律不标出口名（flograph 允许 port 为空串）
			port: "",
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
					if (n.kind === "unknown" || n.kind === "trigger") return;
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

	/** 时间戳统一成 Unix 秒：v4 wire 是 RFC3339 字符串，图里算宽度要数字。 */
	function epochSec(v) {
		if (v == null || v === "") return null;
		if (typeof v === "number") return v;
		const ms = Date.parse(String(v));
		return Number.isFinite(ms) ? Math.floor(ms / 1000) : null;
	}

	function isObjLike(v) {
		return !!v && typeof v === "object";
	}

	/**
	 * taskOf —— 把一条 trace 里的任务摊平（资源信封或 `model.task()` 的行都认）。
	 *
	 * **只读 v4 字段**：metadata.name / status.phase（小写）/ status.attempt /
	 * spec.parentTaskId / spec.capability。v3 的 `out`、九态、`capabilityId`
	 * 一个都不读 —— 图上的"出口名"编码已经随端口一起删掉了。
	 */
	function taskOf(tk) {
		const r = tk || {};
		const A = window.Anotify;
		const flat =
			A && A.model && typeof A.model.task === "function" && r.metadata
				? A.model.task(r)
				: r;
		const meta = r.metadata || {};
		const spec = isObjLike(r.spec) ? r.spec : {};
		const st = isObjLike(r.status) ? r.status : {};
		const capRaw = spec.capability || (isObjLike(r.capability) ? r.capability : null);
		const capability = capRaw
			? { name: capRaw.name || null, version: capRaw.version == null ? null : capRaw.version }
			: null;
		const phaseRaw =
			flat.phase !== undefined ? flat.phase : st.phase !== undefined ? st.phase : null;
		const started = flat.startedAt !== undefined ? flat.startedAt : st.startedAt;
		const finished = flat.finishedAt !== undefined ? flat.finishedAt : st.finishedAt;
		const result = isObjLike(flat.result) ? flat.result : isObjLike(st.result) ? st.result : {};
		// 已经摊平过的行（上一次 taskOf 的产物）直接复用，不重复解析时间戳
		const startedSec = flat.startedSec !== undefined ? flat.startedSec : epochSec(started);
		const finishedSec = flat.finishedSec !== undefined ? flat.finishedSec : epochSec(finished);
		// 还没开始的任务（pending / waiting_*）没有 startedAt：排序要退到 createdAt，
		// 退到 0 的话它们会全部挤到最前面（"还没跑的先执行"是假的阅读顺序）。
		const createdSec =
			flat.createdAt !== undefined
				? epochSec(flat.createdAt)
				: epochSec(flat.creationTimestamp !== undefined ? flat.creationTimestamp : meta.creationTimestamp);
		return {
			name: flat.name || meta.name || null,
			phase: phaseRaw == null ? null : String(phaseRaw).toLowerCase(),
			attempt: flat.attempt !== undefined ? flat.attempt : st.attempt,
			parentTaskId:
				flat.parentTaskId !== undefined ? flat.parentTaskId : spec.parentTaskId || null,
			parentRef: flat.parentRef !== undefined ? flat.parentRef : spec.parentRef || null,
			capability: capability,
			nodeName: flat.nodeName !== undefined ? flat.nodeName : st.nodeName || null,
			startedSec: startedSec,
			finishedSec: finishedSec,
			createdSec: createdSec,
			// 排序用：开始时间 → 结束时间 → 创建时间（都拿不到给 0）
			sortAt:
				startedSec != null
					? startedSec
					: finishedSec != null
						? finishedSec
						: createdSec != null
							? createdSec
							: 0,
			errorReason: result.error ? result.error.reason : null,
			summary: result.summary || "",
		};
	}

	/**
	 * taskChain —— 把一条 trace 的任务列表画成拓扑图
	 *
	 * @param {HTMLElement} host
	 * @param {Object} opts
	 * @param {Array}  opts.tasks     trace 的 tasks[]（资源信封或 model.task 行）
	 * @param {Function} opts.t       i18n
	 * @param {Function} [opts.onNodeClick]
	 * @param {number} [opts.limit]
	 *
	 * 出口：v4 没有 `out`（一次 emit 只交给一个下游，下游由能力的 next 决定），
	 * 所以父子任务之间的边**不标出口名** —— 这是删掉端口概念后的直接结果，
	 * 不是信息丢失。父 → 子关系只由 `spec.parentTaskId` 表达。
	 */
	function taskChain(host, opts) {
		const t = opts.t || ((k, d) => d || k);
		// 不是 v4 任务形状（没有任务名）的行直接丢掉：宁可图空着，也不要
		// 把 null id 塞给 dagre（那会在布局里抛异常、把整页拖白）。
		const rows = (opts.tasks || []).map(taskOf).filter((tk) => tk.name);
		const limit = opts.limit || 120;
		if (!rows.length) {
			// 空 trace：不画图，返回空结果。调用方（traceview / 首页）自己显示空态。
			if (host) host._graphRebuild = () => taskChain(host, opts);
			return { graph: null, truncated: 0 };
		}

		// 底图：这个 trace 所属源在「源」页配置的静态拓扑。
		//
		// 为什么用底图而不是只画执行过的那条链：只画链的话，"为什么这条
		// 分支没走"这个问题图上根本看不见 —— 剪枝的分支压根不出现，
		// 用户看到的永远是一条顺顺利利的直线。用配置拓扑当底、执行状态
		// 当覆盖层，被剪枝的分支会以灰色留在原地。
		const base = opts.baseGraph || null;

		// ------- 执行状态索引：能力名 → 该能力本条 trace 的任务列表 -------
		// 同一个能力可能执行多次（失败重试后再次进入同一步），所以是数组。
		const byCapability = new Map();
		for (const tk of rows) {
			if (!tk.capability || !tk.capability.name) continue;
			const key = tk.capability.name;
			if (!byCapability.has(key)) byCapability.set(key, []);
			byCapability.get(key).push(tk);
		}
		// 任务名 → 规范化 Task：**没有底图**时节点 id 就是任务名（taskChain 自己的
		// id 空间），点击必须能直接命中那一次任务，而不是去能力索引里空找。
		const byTaskName = new Map(rows.map((tk) => [tk.name, tk]));

		// 状态严重度：节点汇总时取最坏的那个（failed 比 succeeded 更该被看见）
		const SEVERITY = {
			failed: 5,
			abandoned: 4,
			cancelled: 3,
			running: 3,
			pending: 2,
			succeeded: 1,
		};
		const worst = (list) => {
			let best = null;
			let bestScore = -1;
			for (const tk of list) {
				const sc = SEVERITY[tk.phase] || 0;
				if (sc > bestScore) {
					bestScore = sc;
					best = tk;
				}
			}
			return best;
		};

		// 执行节拍：按真实时间（startedAt）排成一串能力。
		// 同一能力出现多次时**保留多个节拍** —— 重试本来就该在动画里看得见。
		// ★ 节拍必须和**节点 id 用同一套 id 空间**：有底图时节点 id 是能力名，
		//   没有底图（首页那种）时节点 id 是任务名。
		const sortedRows = rows
			.slice()
			.sort((a, b) => a.sortAt - b.sortAt || (a.name < b.name ? -1 : 1));
		const useTaskIds = !(base && base.nodes && base.nodes.length);
		const capOfTask = new Map(
			rows.map((tk) => [tk.name, tk.capability && tk.capability.name ? tk.capability.name : ""]),
		);
		const beats = sortedRows
			.filter((tk) => tk.capability)
			.map((tk) => ({
				cap: useTaskIds ? tk.name : tk.capability.name,
				from: tk.parentTaskId
					? useTaskIds
						? tk.parentTaskId
						: capOfTask.get(tk.parentTaskId) || ""
					: "",
				// v4 没有出口名：节拍不带 port（渲染层把空串当"不画"）
				port: "",
			}));

		let nodes;
		let edges;

		if (base && base.nodes && base.nodes.length) {
			// ------- 有底图：配置拓扑 + 执行染色 -------
			nodes = base.nodes.map((n) => {
				// 图标永远表达**结构身份**（pipe / end / unknown），
				// 颜色和徽章表达**执行状态**。
				const roleIcon = ICON[n.kind] || ICON.pipe;
				const hits = byCapability.get(n.id);
				if (!hits || !hits.length) {
					// 剪枝：这条分支这次没走。
					// unknown（引用解析不到）不该说"未执行"：它根本执行不了，
					// 副标题走 flowSub —— 和源详情页同一条推导。
					return {
						id: n.id,
						label: topoNodeLabel(n),
						sub: n.unknown
							? flowSub(n, t)
							: t("trace.legend_pruned", "未执行"),
						kind: "pruned",
						icon: roleIcon,
						role: n.kind,
						entry: n.entry,
						dead: true,
					};
				}
				const tk = worst(hits);
				const st = tk.phase || "pending";
				const dur =
					tk.startedSec != null && tk.finishedSec != null
						? (tk.finishedSec - tk.startedSec) * 1000
						: null;
				const bits = [st];
				if (dur != null) bits.push(fmtDur(dur));
				if (hits.length > 1) bits.push("×" + hits.length);
				// 结构身份也要在场：终端能力（空 next）旁边没有别的节点，
				// 只写执行状态的话，读者分不清"这条链到头了"和"这次刚好没往下走"。
				const struct = flowSub(n, t);
				if (struct) bits.push(struct);
				return {
					id: n.id,
					label: topoNodeLabel(n),
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

			// 每条边**实际被走过几次**（纯执行事实：重试/回环会让同一段路走多遍）
			const walkCount = new Map();
			for (const tk of rows) {
				if (!tk.parentTaskId || !tk.capability || !tk.capability.name) continue;
				const fromCap = capOfTask.get(tk.parentTaskId);
				if (!fromCap) continue;
				const key = fromCap + "\u0000" + tk.capability.name;
				walkCount.set(key, (walkCount.get(key) || 0) + 1);
			}

			// 回边：环上的边（"这条路会回来"，所以它天然该比直线更实）
			const backEdges = findBackEdges(
				base.nodes.map((n) => n.id),
				base.edges,
			);

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
					// v4 没有出口名要标：port 恒为空串
					port: "",
					// **真实**遍历次数（边上标 ×N 用），不掺"这是环所以 +1"的调味
					times: walked,
					heat: walked + (isCycle ? 1 : 0),
					loopback: isCycle,
					// 这次**没走过**的边画成虚线灰掉，和剪枝节点同一个语义
					muted: !bothAlive || walked === 0 || !!e.muted,
				};
			});

			// ---- 源节点：这条 trace 是谁触发的 ----
			if (opts.sourceLabel) {
				const SRC_NODE_ID = "__trigger__";
				// 本次真正的根任务（触发器直接拉起的能力）
				const rootCaps = new Set(
					rows
						.filter((tk) => !tk.parentTaskId && tk.capability)
						.map((tk) => tk.capability.name),
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
				// 连向所有"源直接指向"的能力（没有 entry 标记的老底图退回无入边节点）
				const hasIncoming = new Set(base.edges.map((e) => e.to));
				const entries = base.nodes.filter((n) =>
					n.entry !== undefined ? n.entry : !hasIncoming.has(n.id),
				);
				const executedCaps = new Set(
					rows.filter((tk) => tk.capability).map((tk) => tk.capability.name),
				);
				for (const n of entries) {
					const isRoot = rootCaps.has(n.id);
					// 这次不是它起跑、但它确实执行了 —— 别硬连一条触发器过来
					if (!isRoot && executedCaps.has(n.id)) continue;
					edges.push({
						from: SRC_NODE_ID,
						to: n.id,
						// v4 没有出口名：入口边一律不带 port
						port: "",
						// 没被这次走用的入口要显式画成"未走"（虚线）
						muted: !!n.dead || !isRoot,
						times: isRoot ? 1 : 0,
					});
				}
			}
		} else {
			// ------- 没有底图：退回"只画执行链" -------
			// 拿不到源配置时（源已删除、trace 来自未知源）仍然要能看图。
			const byId = new Map(rows.map((x) => [x.name, x]));
			nodes = rows.map((tk) => {
				const st = tk.phase || "pending";
				const dur =
					tk.startedSec != null && tk.finishedSec != null
						? (tk.finishedSec - tk.startedSec) * 1000
						: null;
				const bits = [st];
				if (dur != null) bits.push(fmtDur(dur));
				if (tk.attempt > 1) bits.push("×" + tk.attempt);
				return {
					id: tk.name,
					label: tk.capability
						? tk.capability.name + "@" + (tk.capability.version == null ? "?" : tk.capability.version)
						: tk.name,
					sub: bits.join(" · "),
					kind: kindForStatus(st),
					// 没有底图时不知道结构身份，只能用状态图标
					icon: STATUS_ICON[st] || ICON.pipe,
					badge: STATUS_COLOR[st] || STATUS_COLOR.pending,
					accent: st === "failed" ? STATUS_COLOR.failed : null,
					_task: tk,
				};
			});

			edges = [];
			for (const tk of rows) {
				if (!tk.parentTaskId || !byId.has(tk.parentTaskId)) continue;
				edges.push({
					from: tk.parentTaskId,
					to: tk.name,
					// v4 没有出口名：这条边不标 port
					port: "",
					cycle: false,
					// ★ 这条边是父子任务之间的**真实遍历**，必须写 times ——
					// flograph 的判据是 `walked = !muted && times > 0`；写 0 会
					// 变成灰色虚线且不参与入场动画（连线从头到尾一动不动）。
					times: 1,
					heat: 1,
					muted: false,
				});
			}
		}

		const cut = truncate(nodes, edges, limit);

		// 布局方向按**节点数**自适应（由 renderFlow 统一决定，这里不再算一遍）。
		const graph = renderFlow(
			host,
			{
				// 有底图时才有"执行状态"这一层可讲；只画执行链时图例没有意义
				scope: base && base.nodes && base.nodes.length ? "trace" : "none",
				// 首次进入图视图时按真实执行顺序点亮（排序信息来自任务时间）
				animate: opts.animate === false ? false : !enterAnimated,
				// 首页：先定格在"第 0 拍"，等它真被看见再播
				idle: opts.idle === true,
				beats: beats,
				root: opts.sourceLabel ? "__trigger__" : "",
				t: t,
				nodes: cut.nodes.map((n) => {
					const out = Object.assign({}, n);
					delete out._task;
					return out;
				}),
				edges: cut.edges,
				// 链路图这一页没有标题栏的「全屏」入口，所以需要工具栏版本
				showFullscreen: !!opts.showFullscreen,
				onNodeClick: (n) => {
					if (!opts.onNodeClick) return;
					// 节点 id 空间随图模式而变：没底图时是任务名，有底图时是能力名。
					// 回调统一交出**规范化 Task**（字段是 `name`；Task 没有 `id`），
					// 调用方不该猜当前图的 id 空间。
					if (useTaskIds) {
						const task = byTaskName.get(n.id);
						if (task) opts.onNodeClick(task);
						return;
					}
					// 有底图时节点 id 是能力名，映射回该能力的一次任务
					const hit = byCapability.get(n.id);
					if (hit && hit.length) opts.onNodeClick(hit[0]);
				},
			},
			// ✅ 唯一一处 silk：**只有 trace 页的链路图**换成 Silk 皮肤。
			// 源详情页的 capabilityFlow 走同一个 renderFlow —— 默认必须是原 FlowGraph，
			// 否则那张图会跟着变样（它有自己的一套 fit/坐标断言）。
			true,
		);

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
	// v4 六态 → 视觉分组。注意这一层**不是** i18n 文案，是 CSS 类名后缀，
	// ui.css 里要有对应的 .wf-node.is-* 才能上色（is-pipe / is-end /
	// is-running / is-failed / is-unknown 就这几个）。
	//
	// failed 与 unknown 分开：那个类的语义是"配置里指向一个不存在的能力"，
	// 于是一次执行失败和"配置里指向一个不存在的能力"长得一模一样。
	// cancelled（两个 l）= 用户取消，与 abandoned 一样是"终态且没成功"，
	// 但图标不同（⊘），颜色共用失败红 —— 它不该看起来像成功。
	function kindForStatus(st) {
		if (st === "failed" || st === "abandoned" || st === "cancelled") return "failed";
		if (st === "succeeded") return "end";
		if (st === "running") return "running";
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

	/**
	 * loadBaseGraph —— 取某个源（按名字）的静态拓扑底图，给 taskChain 当底。
	 *
	 * **取数与推导都只有这一份**：trace 页和首页都调它。v4 的三张表都走
	 * ns 路径（`Anotify.nsPath`）；信封用 `items`。拿不到（源已删、没权限、
	 * 后端刚起来）返回 null，调用方退回"只画执行链"——图库不该把页面卡死。
	 *
	 * @param {{source: string, fetchJson?: Function}} opts
	 *   source    源名（`metadata.name`）
	 *   fetchJson 可选：注入的取数函数（默认 `Anotify.api`）
	 */
	async function loadBaseGraph(opts) {
		const o = opts || {};
		const A = window.Anotify;
		if (!o.source || !A || typeof A.nsPath !== "function") return null;
		const fj = o.fetchJson || ((u) => A.api(u));
		const items = (res) => (res && Array.isArray(res.items) ? res.items : []);
		let srcRes;
		let capRes;
		let edgeRes;
		try {
			const got = await Promise.all([
				fj(A.nsPath("sources")),
				fj(A.nsPath("capabilities")),
				fj(A.nsPath("edges")),
			]);
			srcRes = got[0];
			capRes = got[1];
			edgeRes = got[2];
		} catch (e) {
			// 取数失败 = 没有底图（调用方退回执行链），不要在页面里抛未捕获异常
			return null;
		}
		const srcObj = items(srcRes).find((x) => metaName(x) === o.source);
		if (!srcObj) return null;
		const caps = items(capRes);
		if (!caps.length) return null;
		const topo = sourceTopology({
			sources: [srcObj],
			source: srcObj,
			capabilities: caps,
			edges: items(edgeRes),
		});
		return topo && topo.nodes && topo.nodes.length ? topo : null;
	}

	window.AnotifyGraphs = {
		// 拓扑推导（唯一一份）：源详情页的流向图与 trace 页的底图都从这里来。
		// 导出它而不是只导渲染函数，是为了让两个页面真的调**同一个**函数
		// （否则下一个人又会"顺手"在页面里再写一份推导）。
		sourceTopology,
		// 底图取数（唯一一份）：trace 页与首页共用，别在页面里再写一遍
		loadBaseGraph,
		// 预览节拍（唯一一份）：源详情页的「按顺序流过所有节点和边」就是它。
		// 导出给 E2E 直接验顺序（与页面上卡片步号对账），而不是靠截图猜。
		previewBeats,
		capabilityFlow,
		taskChain,
		// 任务摊平（v4 字段）：trace 页/首页若自己算时间/排序也会用同一套
		taskOf,
		fmtDur,
		// 供其它页面复用同一套状态配色（当前主题快照；主题切换后就地刷新）
		STATUS_COLOR,
		STATUS_ICON,
		// v4 phase → CSS 类名后缀（`cancelled` → 历史的 `canceled`）。
		// traceview.js 从这里取，两处不长成两份。
		STATUS_CLASS,
		// 主题切换后重绘所有已挂载的图（partials.js 派发 anotify:theme 时自动调用）
		redraw: redrawAll,
	};
})();
