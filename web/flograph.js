/**
 * FlowGraph —— 工作流图渲染器（HTML 节点 + SVG 连线 + dagre 布局）
 *
 * 为什么自己渲染节点而不用图库：
 *   图库（Cytoscape / G6 / vis）的节点本质是"一个背景色 + 一条边框 + 一段
 *   文本"。做不出卡片节点（图标 + 标题 + 副标题 + 状态徽章 + 悬浮操作），
 *   也做不出"每个能力自定义外观"这种需求。要做成卡片就必须用真正的 DOM。
 *
 * 分工：
 *   dagre  → 只算坐标（分层、排序、减少交叉）
 *   HTML   → 画节点（任意结构、任意样式、可换肤）
 *   SVG    → 画连线（贝塞尔 + 箭头）
 *
 * 用法：
 *   const g = FlowGraph.mount(el, {
 *     nodes: [{ id, label, sub, kind, root, icon, accent, badge }],
 *     edges: [{ from, to, port, cycle, muted }],
 *     onNodeClick: (node) => {},
 *   });
 *   g.fit();      // 缩放至全景
 *   g.destroy();  // 清理（含 window 监听）
 *
 * 自定义节点外观：
 *   默认按 kind 上色（见 ui.css 的 .wf-node.is-*）。要覆盖单个节点，
 *   在 node 上给 { accent: "#f00", icon: "🔥" } 即可 —— 这些会变成内联
 *   样式，优先级高于 kind 类。
 *
 * 依赖：vendor/dagre.min.js（全局 dagre）
 */
(function () {
	"use strict";

	// SVG 命名空间：createElementNS 才是创建 SVG 元素的正确方式，
	// createElement("path") 会得到一个 HTMLUnknownElement，什么也不画。
	const SVG_NS = "http://www.w3.org/2000/svg";

	function svg(tag, attrs) {
		const n = document.createElementNS(SVG_NS, tag);
		if (attrs) for (const k in attrs) n.setAttribute(k, attrs[k]);
		return n;
	}

	function div(cls, text) {
		const n = document.createElement("div");
		if (cls) n.className = cls;
		if (text != null) n.textContent = String(text);
		return n;
	}

	/**
	 * 控件图标（24×24 线性图标，stroke=currentColor）。
	 *
	 * 为什么不用 Unicode 字符：⛶ / ⤢ 这类符号在字体缺字时会被渲染成「豆腐块」
	 * （DejaVu / 部分 Linux Chrome 就没有 ⤢），一个方块按钮看起来像坏了。
	 * 内联 SVG 不依赖字体，永远画得出来。
	 */
	function ctrlIcon(name) {
		const paths = {
			// 加 / 减
			"zoom-in": ["M12 5v14", "M5 12h14"],
			"zoom-out": ["M5 12h14"],
			// 适应视图：四角**向内**的折角。
			//
			// 原本和 expand（四角向外）只差箭头方向，在 14px 图标尺寸下
			// 几乎看不出区别，用户会以为是重复按钮。现在改成"中间一个框 +
			// 四角折角"，轮廓差异明显。
			fit: [
				"M8 8h8v8H8z",
				"M4 8V4h4",
				"M16 4h4v4",
				"M4 16v4h4",
				"M20 16v4h-4",
			],
			// 四角向外的箭头：全屏 / 退出全屏
			expand: ["M4 9V4h5", "M20 9V4h-5", "M4 15v5h5", "M20 15v5h-5"],
			collapse: ["M9 4v5H4", "M15 4v5h5", "M9 20v-5H4", "M15 20v-5h5"],
		};
		const ns = "http://www.w3.org/2000/svg";
		const el = document.createElementNS(ns, "svg");
		el.setAttribute("viewBox", "0 0 24 24");
		el.setAttribute("fill", "none");
		el.setAttribute("stroke", "currentColor");
		el.setAttribute("stroke-width", "1.7");
		el.setAttribute("stroke-linecap", "round");
		el.setAttribute("stroke-linejoin", "round");
		el.setAttribute("aria-hidden", "true");
		el.classList.add("flograph-ctrl-icon");
		for (const d of paths[name] || []) {
			const p = document.createElementNS(ns, "path");
			p.setAttribute("d", d);
			el.appendChild(p);
		}
		return el;
	}

	/**
	 * 连线路径：从 (x1,y1) 到 (x2,y2) 的三次贝塞尔。
	 *
	 * 控制点用垂直偏移（TB 布局下边从底部出、顶部进），这样曲线是
	 * "先往下走再拐过去"，跟 n8n / 流程图工具的观感一致。
	 * 偏移量取两点垂直距离的一半（有上下限），距离近时不要画成直线。
	 */
	function edgePath(x1, y1, x2, y2, horizontal) {
		if (horizontal) {
			// 横向：控制点沿 x 方向偏移，曲线"先往右走再拐过去"
			const dx = Math.max(Math.abs(x2 - x1) * 0.5, 20);
			return `M ${x1} ${y1} C ${x1 + dx} ${y1}, ${x2 - dx} ${y2}, ${x2} ${y2}`;
		}
		const dy = Math.max(Math.abs(y2 - y1) * 0.5, 18);
		return `M ${x1} ${y1} C ${x1} ${y1 + dy}, ${x2} ${y2 - dy}, ${x2} ${y2}`;
	}

	/**
	 * 边的修饰类。cycle（环路）优先于 muted（弱化/虚线）。
	 *
	 * 抽成函数是因为原来用了嵌套三元（e.cycle ? ... : e.muted ? ... : ""），
	 * 三处调用各写一遍既重复又难读。
	 *
	 * @param {boolean} withSpace 拼进已有 class 字符串时要带前导空格
	 */
	function edgeClass(e, withSpace) {
		let cls = "";
		if (e.cycle) cls = "is-cycle";
		else if (e.muted) cls = "is-muted";
		if (cls && withSpace) return " " + cls;
		return cls;
	}

	/** 贝塞尔在 t 处的点（用来把出口名标签放到曲线中点） */
	function bezierAt(x1, y1, x2, y2, t, horizontal) {
		let p1;
		let p2;
		if (horizontal) {
			const dx = Math.max(Math.abs(x2 - x1) * 0.5, 20);
			p1 = [x1 + dx, y1];
			p2 = [x2 - dx, y2];
		} else {
			const dy = Math.max(Math.abs(y2 - y1) * 0.5, 18);
			p1 = [x1, y1 + dy];
			p2 = [x2, y2 - dy];
		}
		const mt = 1 - t;
		const a = mt * mt * mt;
		const b = 3 * mt * mt * t;
		const c = 3 * mt * t * t;
		const d = t * t * t;
		return [
			a * x1 + b * p1[0] + c * p2[0] + d * x2,
			a * y1 + b * p1[1] + c * p2[1] + d * y2,
		];
	}

	function mount(container, opts) {
		const options = opts || {};
		const nodes = (options.nodes || []).map((n) => Object.assign({}, n));
		const edges = (options.edges || []).map((e) => Object.assign({}, e));

		// 本模块自己的文案（缩放/全屏按钮 title）。
		//
		// 为什么不直接读 window.AnotifyI18n：sitegen 把翻译表注入成
		// `window.AnotifyI18n[lang].键` 的平铺字典，而这里也可能被调用方
		// 通过 options.t 传入（页面里拿的是同一个字典的闭包）。两条都支持，
		// 都拿不到就用中文默认值 —— 图上的控件没字也不会崩。
		const tt = (k, d) => {
			if (typeof options.t === "function") {
				const v = options.t(k, d);
				if (v && v !== k) return v;
			}
			const dict = window.AnotifyI18n;
			if (dict && dict[k]) return dict[k];
			return d || k;
		};

		container.replaceChildren();
		container.classList.add("flograph");

		const viewport = div("flograph-viewport");
		const svgRoot = svg("svg", { class: "flograph-svg" });

		// 箭头标记：一个 svg 只需要一份 defs
		const defs = svg("defs");
		for (const [id, color] of [
			["arrow", "currentColor"],
			["arrow-cycle", "#f97316"],
		]) {
			const marker = svg("marker", {
				id: "fg-" + id,
				viewBox: "0 0 8 8",
				refX: "7",
				refY: "4",
				markerWidth: "6",
				markerHeight: "6",
				orient: "auto-start-reverse",
			});
			marker.appendChild(svg("path", { d: "M 0 0 L 8 4 L 0 8 z", fill: color }));
			defs.appendChild(marker);
		}
		svgRoot.appendChild(defs);

		viewport.appendChild(svgRoot);
		container.appendChild(viewport);

		// ---------- 1. 先建 DOM 节点并测量 ----------
		//
		// dagre 需要每个节点的宽高才能布局。CSS 里宽度是 min/max 区间、
		// 高度随内容变化，所以必须先挂到 DOM 实测，不能瞎猜。
		// 先放到 viewport 里（还没定位），量完再排。
		const nodeEls = new Map();
		for (const n of nodes) {
			const el = div("wf-node");
			el.dataset.nodeId = n.id;

			// kind 类：决定默认配色
			el.classList.add("is-" + (n.kind || "pipe"));
			if (n.root) el.classList.add("is-root");
			if (n.cycle) el.classList.add("is-cycle");
			if (n.seen) el.classList.add("is-seen");

			// 单个节点的自定义外观（覆盖 kind 默认色）
			if (n.accent) {
				el.style.borderColor = n.accent;
			}

			const icon = div("wf-node-icon", n.icon || "◆");
			if (n.accent) {
				// 用 accent 的 16% 透明度做底，图标本身用 accent
				icon.style.background = hexA(n.accent, 0.16);
				icon.style.color = n.accent;
			}
			el.appendChild(icon);

			const body = div("wf-node-body");
			body.appendChild(div("wf-node-title", n.label || n.id));
			if (n.sub) body.appendChild(div("wf-node-sub", n.sub));
			el.appendChild(body);

			if (n.badge) {
				if (typeof n.badge === "string") {
					const b = div("wf-node-dot");
					b.style.background = n.badge;
					el.appendChild(b);
				} else {
					el.appendChild(n.badge);
				}
			}

			if (options.onNodeClick) {
				el.style.cursor = "pointer";
				el.addEventListener("click", (ev) => {
					// 拖拽平移时不要误触发点击
					if (moved) return;
					ev.stopPropagation();
					options.onNodeClick(n, ev);
				});
			}
			if (options.renderNode) options.renderNode(el, n);

			viewport.appendChild(el);
			nodeEls.set(n.id, el);
		}

		// 测量节点尺寸给 dagre 用。
		//
		// **必须用 offsetWidth/offsetHeight，不能用 getBoundingClientRect()**：
		// 节点在一个被 transform:scale() 的 viewport 里，rect 拿到的是**缩放后**
		// 的尺寸。首次 fit 前 scale=1 时两者相同，但 ResizeObserver 触发
		// fit() 把 scale 提到 1.15 之后，rect 就变成 0.87 倍——用那个值重算
		// 布局会让 dagre 以为节点更窄，于是节点重叠。
		// offsetWidth 是布局尺寸，不受 transform 影响。
		//
		// 另：节点是 position:absolute，先给临时定位让它在确定的上下文里测量。
		for (const n of nodes) {
			const el = nodeEls.get(n.id);
			el.style.left = "0px";
			el.style.top = "0px";
		}
		void viewport.offsetHeight; // 强制回流，确保样式应用完

		for (const n of nodes) {
			const el = nodeEls.get(n.id);
			n._w = el.offsetWidth;
			n._h = el.offsetHeight;
		}

		// ---------- 2. dagre 布局 ----------
		const g = new dagre.graphlib.Graph({ multigraph: true });
		g.setGraph({
			rankdir: options.rankdir || "TB",
			// 间距给足：卡片节点比图库的方块大得多，挤在一起连线会糊成一团
			nodesep: options.nodeSep != null ? options.nodeSep : 64,
			ranksep: options.rankSep != null ? options.rankSep : 78,
			marginx: 20,
			marginy: 20,
		});
		g.setDefaultEdgeLabel(() => ({}));

		for (const n of nodes) {
			g.setNode(n.id, { width: n._w, height: n._h });
		}
		// multigraph 要 edge name：同一对节点之间可能有多条边（不同出口）
		edges.forEach((e, i) => {
			if (g.hasNode(e.from) && g.hasNode(e.to)) {
				g.setEdge(e.from, e.to, {}, "e" + i);
			}
		});

		dagre.layout(g);

		// ---------- 3. 定位节点 ----------
		for (const n of nodes) {
			const p = g.node(n.id);
			if (!p) continue;
			const el = nodeEls.get(n.id);
			// dagre 给的是中心点，CSS 定位要给左上角
			el.style.left = p.x - n._w / 2 + "px";
			el.style.top = p.y - n._h / 2 + "px";
		}

		// ---------- 4. 画连线 ----------
		//
		// 边的进出点取决于布局方向：TB 从上到下（底出顶进），
		// LR 从左到右（右出左进）。写死一边会让另一个方向的连线穿过节点。
		const horizontal = (options.rankdir || "TB") === "LR";
		const labelEls = [];
		const edgePos = new Map();
		edges.forEach((e, i) => {
			const src = g.node(e.from);
			const dst = g.node(e.to);
			if (!src || !dst) return;

			// 出/进点按布局方向取边中点
			const x1 = horizontal ? src.x + src.width / 2 : src.x;
			const y1 = horizontal ? src.y : src.y + src.height / 2;
			const x2 = horizontal ? dst.x - dst.width / 2 : dst.x;
			const y2 = horizontal ? dst.y : dst.y - dst.height / 2;

			const path = svg("path", { d: edgePath(x1, y1, x2, y2, horizontal) });
			path.setAttribute("marker-end", e.cycle ? "url(#fg-arrow-cycle)" : "url(#fg-arrow)");
			const cls = edgeClass(e);
			if (cls) path.setAttribute("class", cls);
			svgRoot.appendChild(path);

			edgePos.set(i, { x1, y1, x2, y2 });

			// 出口名：放在曲线上 42% 处（稍微偏向出发端，读起来像"从这个口出"）
			if (e.port) {
				const [lx, ly] = bezierAt(x1, y1, x2, y2, 0.42, horizontal);
				const lb = div("flograph-edge-label" + edgeClass(e, true), e.port);
				lb.style.left = lx + "px";
				lb.style.top = ly + "px";
				viewport.appendChild(lb);
				labelEls.push(lb);
			}
		});

		// ---------- 5. 视图变换（缩放 + 平移） ----------
		const graphInfo = g.graph();
		const contentW = Math.max(graphInfo.width || 0, 1);
		const contentH = Math.max(graphInfo.height || 0, 1);

		let scale = 1;
		let tx = 0;
		let ty = 0;

		function applyTransform() {
			viewport.style.transform = `translate(${tx}px, ${ty}px) scale(${scale})`;
		}

		function fit(padding) {
			const pad = padding != null ? padding : 24;
			const box = container.getBoundingClientRect();
			if (!box.width || !box.height) return;
			const sx = (box.width - pad * 2) / contentW;
			const sy = (box.height - pad * 2) / contentH;
			// 不放大超过 1.15：小图放到最大会很怪
			//
			// 缩放下限 0.42：图大时如果一路缩到底，字会小到读不了。
			// 宁可让它溢出（用户能拖/滚），也不要给出"看得见但看不清"的全景。
			scale = Math.min(sx, sy, 1.15);
			if (!Number.isFinite(scale) || scale <= 0) scale = 1;
			scale = Math.max(scale, 0.42);
			tx = (box.width - contentW * scale) / 2;
			ty = (box.height - contentH * scale) / 2;
			applyTransform();
		}

		function zoomBy(factor, cx, cy) {
			const box = container.getBoundingClientRect();
			const px = cx != null ? cx : box.width / 2;
			const py = cy != null ? cy : box.height / 2;
			const next = Math.min(2.5, Math.max(0.25, scale * factor));
			// 以鼠标位置为锚点缩放：先算该点在内容坐标下的位置，缩放后保持不动
			const innerX = (px - tx) / scale;
			const innerY = (py - ty) / scale;
			scale = next;
			tx = px - innerX * scale;
			ty = py - innerY * scale;
			applyTransform();
		}

		// ---- 拖拽平移 ----
		let dragging = false;
		let moved = false;
		let lastX = 0;
		let lastY = 0;

		function onDown(ev) {
			// 只响应左键；点在节点上也算拖拽（但要记住没移动，避免误触点击）
			if (ev.button !== 0) return;
			dragging = true;
			moved = false;
			lastX = ev.clientX;
			lastY = ev.clientY;
			container.classList.add("is-panning");
		}
		function onMove(ev) {
			if (!dragging) return;
			const dx = ev.clientX - lastX;
			const dy = ev.clientY - lastY;
			if (Math.abs(dx) > 2 || Math.abs(dy) > 2) moved = true;
			tx += dx;
			ty += dy;
			lastX = ev.clientX;
			lastY = ev.clientY;
			applyTransform();
		}
		function onUp() {
			dragging = false;
			container.classList.remove("is-panning");
		}

		container.addEventListener("mousedown", onDown);
		// move/up 挂 window：拖出容器外也要跟手
		window.addEventListener("mousemove", onMove);
		window.addEventListener("mouseup", onUp);

		// ---- 滚轮缩放 ----
		//
		// 原来每个 wheel 事件一律 ×1.12，只看 deltaY 的正负。这手感很差：
		//
		//   - 触控板一次双指手势会连发几十个 wheel 事件（deltaY 只有几像素），
		//     每个都乘 1.12 → 瞬间顶到上限，用户根本控制不住；
		//   - 鼠标滚轮 deltaY 常是 100 或 120，一次就跳 12%；
		//   - 而平滑滚轮（Firefox、部分 macOS）deltaY 是 1~3，得狂滚。
		//
		// 正确做法是**按滚动量连续缩放**：把 deltaY 归一化到"行"，
		// 每行缩放 exp 曲线的固定比例。这样：
		//   - 同样的物理滚动距离，得到同样的缩放幅度（设备无关）
		//   - 触控板的连续小事件累加后正好等于一次大滚动，不会失控
		//
		// 还要设一个上限：单个事件的 deltaY 可能极大（触控板甩一下 300+），
		// 直接指数化会把 scale 一次打到底。
		// 每"行"滚动的缩放强度（指数）：exp(0.12) ≈ 1.127，
		// 也就是滚一格 ≈ 12.7%，和原来 1.12 的手感接近，但**由滚动量决定**。
		const ZOOM_PER_LINE = 0.12;
		const MAX_LINES_PER_EVENT = 12; // 单事件最多折算 12 行

		// wheelLines 把 deltaY 折算成"行数"。
		//
		// deltaMode：0=像素，1=行，2=页（浏览器按设备/系统设置给不同单位，
		// 不归一化的话同一个手势在不同机器上差几十倍）。
		function wheelLines(ev) {
			let dy = ev.deltaY;
			if (ev.deltaMode === 1) dy *= 16; // 行 → 像素（约 16px/行）
			else if (ev.deltaMode === 2) dy *= container.clientHeight || 800; // 页 → 像素
			let lines = dy / 100; // 100px 约等于"一行"
			if (lines > MAX_LINES_PER_EVENT) lines = MAX_LINES_PER_EVENT;
			if (lines < -MAX_LINES_PER_EVENT) lines = -MAX_LINES_PER_EVENT;
			return lines;
		}

		function onWheel(ev) {
			// 只拦纵向滚动来做缩放；横向滚轮（shift+滚轮、触控板横滑）留给浏览器
			if (Math.abs(ev.deltaY) < Math.abs(ev.deltaX)) return;
			ev.preventDefault();
			const lines = wheelLines(ev);
			if (!lines) return;
			const box = container.getBoundingClientRect();
			// 指数映射：滚 −1 行 → ×exp(0.12)，连续且可逆
			// （滚回同样距离能精确回到原来的比例，线性乘法做不到）
			const factor = Math.exp(-lines * ZOOM_PER_LINE);
			zoomBy(factor, ev.clientX - box.left, ev.clientY - box.top);
		}
		container.addEventListener("wheel", onWheel, { passive: false });

		// ---- 右下角缩放控制 ----
		//
		// 全屏按钮放这里而不是让各页面自己在外层包一个容器：
		// 全屏要重排**这一个**容器（见 setFullscreen），页面拿不到内部
		// 的 fit()/scale，自己实现必然要复制一份状态机。
		const ctrl = div("flograph-ctrl");
		for (const [key, icon, title, fn] of [
			["zoom-in", "zoom-in", tt("graph.zoom_in", "放大"), () => zoomBy(1.25)],
			["zoom-out", "zoom-out", tt("graph.zoom_out", "缩小"), () => zoomBy(1 / 1.25)],
			["fit", "fit", tt("graph.fit", "适应视图"), () => fit()],
			// 全屏默认**不放进工具栏**：调用方通常在卡片标题上还有一个
			// 带文字的「全屏」入口（更容易发现）。两个都放会看到两个
			// 形状相近的图标挨在一起，反而让人以为是重复按钮。
			// 需要工具栏版本时传 opts.showFullscreen。
			["fullscreen", "expand", tt("graph.fullscreen", "全屏"), () => toggleFullscreen()],
		]) {
			if (key === "fullscreen" && !options.showFullscreen) continue;
			const b = document.createElement("button");
			b.type = "button";
			b.title = title;
			b.dataset.fgAction = key;
			// aria-label：按钮里只有图标，读屏软件需要一个名字
			b.setAttribute("aria-label", title);
			b.appendChild(ctrlIcon(icon));
			b.addEventListener("click", (ev) => {
				ev.stopPropagation();
				fn();
			});
			ctrl.appendChild(b);
		}
		container.appendChild(ctrl);

		// 全屏时左下角给一行操作提示。只在全屏显示：卡片里的日常状态不需要解释，
		// 而全屏后页面其它元素都被遮住了，没有提示用户只能靠猜（尤其 Esc 退出）。
		const fsHint = div("flograph-fullscreen-hint", tt("graph.fullscreen_hint", "滚轮缩放 · 拖动平移 · Esc 退出"));
		container.appendChild(fsHint);

		// ---- 全屏 ----
		//
		// 为什么不直接用 Fullscreen API：
		//
		//   ① 它是**异步且可被拒绝**的（用户手势外调用 / iframe 缺 allowfullscreen /
		//      iOS Safari 对非 video 元素直接不支持），失败时要回落到别的方案；
		//   ② 全屏后 Esc 由浏览器接管，退出事件不保证经过我们的代码。
		//
		// 所以主路径是「伪全屏」：把容器从文档里**摘出来**挂到 body 上放满视口，
		// 关掉时按占位符的位置放回去。摘出来而不是只加 position:fixed，是因为
		// 祖先节点只要有一个 transform / filter / contain，fixed 的包含块就变成
		// 那个祖先，容器会被裁在卡片里（.reveal 块就带 transform + opacity 动画，
		// 这种坑在真实页面里非常容易踩到）。
		const fsPlaceholder = document.createElement("div");
		fsPlaceholder.className = "flograph-placeholder";
		fsPlaceholder.hidden = true;

		let fullscreen = false;
		// 退出全屏后要还原的样式与位置。容器平时是页面的静态结构，
		// 只有全屏期间才会被摘出去，所以必须把内联样式改回原样（不是清空：
		// 调用方可能给容器设过高度）。
		let fsRestore = null;

		function setFullscreen(on) {
			if (on === fullscreen) return;
			fullscreen = on;
			if (on) {
				// **先量再改样式**：容器已经在文档里，此刻它的 getBoundingClientRect()
				// 是卡片里的真实尺寸。要是先加 .is-fullscreen（position:fixed +
				// inset:0），再量就是整个视口的高度——占位符会变成一整屏高，
				// 退出全屏时页面会“多出一大块”（真实踩过）。
				const box = container.getBoundingClientRect();
				fsRestore = {
					parent: container.parentNode,
					next: container.nextSibling,
					style: container.getAttribute("style"),
					w: Math.round(box.width),
					h: Math.round(box.height),
				};
				// 占位符撑住原来的位置：容器被摘走后卡片会塌成 0 高，
				// 关掉全屏时页面就会“跳”到别处
				fsPlaceholder.style.width = fsRestore.w + "px";
				fsPlaceholder.style.height = fsRestore.h + "px";
				fsPlaceholder.hidden = false;
				if (container.parentNode) {
					container.parentNode.insertBefore(fsPlaceholder, container);
				}
				container.classList.add("is-fullscreen");
				document.body.appendChild(container);
				document.documentElement.classList.add("flograph-fullscreen-open");
			} else {
				document.documentElement.classList.remove("flograph-fullscreen-open");
				container.classList.remove("is-fullscreen");
				const r = fsRestore;
				if (r && r.parent) {
					if (r.next && r.next.parentNode === r.parent) {
						r.parent.insertBefore(container, r.next);
					} else {
						r.parent.appendChild(container);
					}
				}
				if (r && r.style == null) container.removeAttribute("style");
				else if (r) container.setAttribute("style", r.style);
				fsPlaceholder.remove();
				fsPlaceholder.hidden = true;
				fsRestore = null;
			}
			// 容器尺寸彻底变了（520px 卡片 → 视口大小），必须重排一次。
			// 放在下一帧：样式刚改，这一帧量到的还是旧尺寸。
			//
			// 全屏是「要看全景」的意图，所以**强制** fit（忽略 userAdjusted），
			// 顺便把 userAdjusted 重置，否则退出全屏后 ResizeObserver 也不再自动适配。
			requestAnimationFrame(() => {
				if (!fullscreen && !container.isConnected) return;
				userAdjusted = false;
				fit();
				onFullscreenChange(fullscreen);
			});
		}

		function toggleFullscreen() {
			setFullscreen(!fullscreen);
		}

		function onFullscreenChange(v) {
			// 按钮和提示必须**分别**处理：工具栏全屏按钮可能不存在
			// （调用方在标题栏有自己的入口，见 options.showFullscreen），
			// 早期写法是 `if (!b) return;` —— 那一句会把下面的提示也跳过，
			// 于是没有工具栏按钮时，全屏后左下角的「Esc 退出」永远不出现。
			const b = ctrl.querySelector('[data-fg-action="fullscreen"]');
			if (b) {
				const title = v
					? tt("graph.exit_fullscreen", "退出全屏（Esc）")
					: tt("graph.fullscreen", "全屏");
				b.title = title;
				b.setAttribute("aria-label", title);
				// 图标也跟着换：全屏中显示「向内收」，提示点一下会退回去
				b.replaceChildren(ctrlIcon(v ? "collapse" : "expand"));
				b.classList.toggle("is-on", v);
			}
			if (fsHint) fsHint.classList.toggle("is-on", v);
		}

		// Esc 退出。keydown 挂 window：全屏时容器是 body 的直接子节点，
		// 但焦点可能在任何地方（点过按钮、选中过文字）。
		function onKeydown(ev) {
			if (!fullscreen) return;
			if (ev.key === "Escape") {
				ev.preventDefault();
				setFullscreen(false);
			}
		}
		window.addEventListener("keydown", onKeydown);

		// 首次适配。等一帧：容器可能刚插入 DOM，尺寸还没算出来
		requestAnimationFrame(() => fit());

		// 容器尺寸变化（窗口缩放 / 侧栏折叠）时重新适配
		let ro = null;
		if (typeof ResizeObserver === "function") {
			ro = new ResizeObserver(() => {
				// 只在用户没手动调整过时自动 fit，否则会打断操作
				if (!userAdjusted) fit();
			});
			ro.observe(container);
		}

		// 用户一旦手动缩放/平移，就不再自动 fit
		let userAdjusted = false;
		container.addEventListener("wheel", () => {
			userAdjusted = true;
		});
		container.addEventListener("mousedown", () => {
			userAdjusted = true;
		});

		return {
			fit,
			zoomBy,
			// 全屏开关：页面也可以用（例如卡片标题上的「全屏」按钮），
			// 但主入口是右下角控件里的那一个按钮。
			setFullscreen,
			isFullscreen: () => fullscreen,
			destroy() {
				// 销毁时如果在全屏状态，先把容器放回去：否则节点被清空、
				// 容器却还挂在 body 上占着整个视口，页面上会多出一块黑底。
				if (fullscreen) setFullscreen(false);
				window.removeEventListener("keydown", onKeydown);
				window.removeEventListener("mousemove", onMove);
				window.removeEventListener("mouseup", onUp);
				container.removeEventListener("wheel", onWheel);
				container.removeEventListener("mousedown", onDown);
				if (ro) ro.disconnect();
				container.replaceChildren();
				container.classList.remove("flograph");
			},
			// 暴露内部以便调试与 e2e 断言
			_debug: {
				nodes: () => nodes.map((n) => ({ id: n.id, label: n.label, kind: n.kind })),
				edges: () => edges.map((e) => ({ from: e.from, to: e.to, port: e.port })),
				fullscreen: () => fullscreen,
				scale: () => scale,
				contentSize: () => ({ w: contentW, h: contentH }),
			},
		};
	}

	/** 十六进制色 + 透明度 → rgba()。支持 #rgb / #rrggbb。 */
	function hexA(hex, alpha) {
		const h = String(hex).replace("#", "");
		const full =
			h.length === 3
				? h[0] + h[0] + h[1] + h[1] + h[2] + h[2]
				: h.padEnd(6, "0").slice(0, 6);
		const r = parseInt(full.slice(0, 2), 16);
		const gg = parseInt(full.slice(2, 4), 16);
		const b = parseInt(full.slice(4, 6), 16);
		return `rgba(${r}, ${gg}, ${b}, ${alpha})`;
	}

	window.FlowGraph = { mount };
})();
