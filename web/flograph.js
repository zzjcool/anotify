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
	 * 连线路径：从 (x1,y1) 到 (x2,y2) 的三次贝塞尔。
	 *
	 * 控制点用垂直偏移（TB 布局下边从底部出、顶部进），这样曲线是
	 * "先往下走再拐过去"，跟 n8n / 流程图工具的观感一致。
	 * 偏移量取两点垂直距离的一半（有上下限），距离近时不要画成直线。
	 */
	function edgePath(x1, y1, x2, y2) {
		const dy = Math.max(Math.abs(y2 - y1) * 0.5, 18);
		return `M ${x1} ${y1} C ${x1} ${y1 + dy}, ${x2} ${y2 - dy}, ${x2} ${y2}`;
	}

	/** 贝塞尔在 t 处的点（用来把出口名标签放到曲线中点） */
	function bezierAt(x1, y1, x2, y2, t) {
		const dy = Math.max(Math.abs(y2 - y1) * 0.5, 18);
		const p1 = [x1, y1 + dy];
		const p2 = [x2, y2 - dy];
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

		// 测量（此刻元素已在 DOM，offsetWidth/Height 可用）
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
		const labelEls = [];
		const edgePos = new Map();
		edges.forEach((e, i) => {
			const src = g.node(e.from);
			const dst = g.node(e.to);
			if (!src || !dst) return;

			// TB 布局：从下边中点出发，到上边中点
			const x1 = src.x;
			const y1 = src.y + src.height / 2;
			const x2 = dst.x;
			const y2 = dst.y - dst.height / 2;

			const path = svg("path", { d: edgePath(x1, y1, x2, y2) });
			path.setAttribute(
				"marker-end",
				e.cycle ? "url(#fg-arrow-cycle)" : "url(#fg-arrow)",
			);
			const cls = e.cycle ? "is-cycle" : e.muted ? "is-muted" : "";
			if (cls) path.setAttribute("class", cls);
			svgRoot.appendChild(path);

			edgePos.set(i, { x1, y1, x2, y2 });

			// 出口名：放在曲线上 42% 处（稍微偏向出发端，读起来像"从这个口出"）
			if (e.port) {
				const [lx, ly] = bezierAt(x1, y1, x2, y2, 0.42);
				const lb = div(
					"flograph-edge-label" + (e.cycle ? " is-cycle" : e.muted ? " is-muted" : ""),
					e.port,
				);
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
			scale = Math.min(sx, sy, 1.15);
			if (!isFinite(scale) || scale <= 0) scale = 1;
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
			const ix = (px - tx) / scale;
			const iy = (py - ty) / scale;
			scale = next;
			tx = px - ix * scale;
			ty = py - iy * scale;
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
		function onWheel(ev) {
			ev.preventDefault();
			const box = container.getBoundingClientRect();
			zoomBy(ev.deltaY < 0 ? 1.12 : 1 / 1.12, ev.clientX - box.left, ev.clientY - box.top);
		}
		container.addEventListener("wheel", onWheel, { passive: false });

		// ---- 右下角缩放控制 ----
		const ctrl = div("flograph-ctrl");
		for (const [label, title, fn] of [
			["+", "放大", () => zoomBy(1.25)],
			["−", "缩小", () => zoomBy(1 / 1.25)],
			["⛶", "适应视图", () => fit()],
		]) {
			const b = document.createElement("button");
			b.type = "button";
			b.textContent = label;
			b.title = title;
			b.addEventListener("click", (ev) => {
				ev.stopPropagation();
				fn();
			});
			ctrl.appendChild(b);
		}
		container.appendChild(ctrl);

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
			destroy() {
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
