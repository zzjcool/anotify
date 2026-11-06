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
			// 重播：逆时针的环绕箭头。用它而不是三角形 play，
			// 是因为这块是**图**不是播放器，"回到起点再来一遍"更准确。
			replay: ["M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8", "M3 3v5h5"],
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
	 * pickLabelSpot 给边标签找一个不压节点、不压别的标签的落点。
	 *
	 * 标签是绝对定位的 DOM，和 SVG 路径不在一个坐标系（zoom/pan 只作用于
	 * 共同的 viewport），所以必须自己算避让 —— 之前固定放 42% 处，
	 * 那个点可能正好落在某个节点矩形里，看上去就是"文字乱标"。
	 *
	 * 候选顺序从路径中点向两边扩散（0.5 → 0.42/0.58 → 0.34/0.66 …），
	 * 取第一个既不撞节点也不撞已放标签的。全撞就退回中点：
	 * 挤一点也比丢失"这条路走过几次"要好。
	 */
	function pickLabelSpot(points, x1, y1, x2, y2, el, nodeBoxes, labelBoxes) {
		// 先量一下标签尺寸（还没定位置就能量：绝对定位 + 空 left/top 也对）
		const w = el.offsetWidth || 40;
		const h = el.offsetHeight || 16;

		const hitsNode = (cx, cy) => {
			const l = cx - w / 2;
			const r = cx + w / 2;
			const t = cy - h / 2;
			const b = cy + h / 2;
			for (const n of nodeBoxes) {
				// 留 3px 余量，避免"紧贴边框"看着也像压着
				if (l < n.right + 3 && r > n.left - 3 && t < n.bottom + 3 && b > n.top - 3) {
					return true;
				}
			}
			return false;
		};
		const hitsLabel = (cx, cy) => {
			const l = cx - w / 2;
			const r = cx + w / 2;
			const t = cy - h / 2;
			const b = cy + h / 2;
			for (const o of labelBoxes) {
				if (l < o.right + 2 && r > o.left - 2 && t < o.bottom + 2 && b > o.top - 2) {
					return true;
				}
			}
			return false;
		};

		// 中点优先，然后左右交替扩散
		const cands = [0.5];
		for (let k = 1; k <= 5; k++) {
			cands.push(0.5 - k * 0.08, 0.5 + k * 0.08);
		}

		for (const t of cands) {
			if (t <= 0.06 || t >= 0.94) continue;
			const [cx, cy] = pointAlongPath(points, x1, y1, x2, y2, t);
			if (!hitsNode(cx, cy) && !hitsLabel(cx, cy)) {
				return { x: cx, y: cy, w, h };
			}
		}
		const [mx, my] = pointAlongPath(points, x1, y1, x2, y2, 0.5);
		return { x: mx, y: my, w, h };
	}

	/**
	 * snapEnd 把路径末端吸附到目标节点边界外 gap 像素处。
	 *
	 * 要同时满足两件事：
	 *   1. **贴住节点** —— 离远了看上去就是"线断了"
	 *   2. **别压进节点** —— 节点画在 SVG 上层，箭头有一像素落进节点矩形
	 *      就被盖住，看上去像"箭头缺了一半"
	 *
	 * 做法：找"最后一个还在【外扩 gap 后的框】外面"的折线顶点，从它朝
	 * 节点中心打射线，取与这个外扩框的**第一个**交点。
	 *
	 * 两个坑都踩过：
	 *
	 * ① 不能拿"折线与框的交点"当端点：dagre 的折线末点本身就在节点边界
	 *    上（它会补一个 node-intersect 点），据此求交会偏出去 17~27px，
	 *    渲染出来就是箭头悬空、看起来没连上。
	 *
	 * ② 反向搜索必须用**外扩后的框**，不能用原节点框。因为边界上的点
	 *    不算"在节点内部"，会被当成合格的起点；从边界朝中心打射线就会
	 *    **穿过整个节点**、从对面穿出去 —— 实测虚线直穿
	 *    「生成变更日志」，箭头落在节点下方的外面。
	 */
	function snapEnd(points, x1, y1, x2, y2, box, gap) {
		const full = [{ x: x1, y: y1 }].concat(points || [], [{ x: x2, y: y2 }]);
		if (!box) return { pts: points || [], x: x2, y: y2 };

		const l = box.x - box.width / 2 - gap;
		const r = box.x + box.width / 2 + gap;
		const t = box.y - box.height / 2 - gap;
		const b = box.y + box.height / 2 + gap;
		const outside = (q) => q.x <= l || q.x >= r || q.y <= t || q.y >= b;

		// 最后一个还在外扩框外面的顶点
		let j = -1;
		for (let k = full.length - 1; k >= 0; k--) {
			if (outside(full[k])) {
				j = k;
				break;
			}
		}

		// 与外扩框求**进入点**（沿 from → 节点中心方向的射线）。
		//
		// 必须用 slab 法（射线与 AABB 求交），不能"分别和四条边所在的直线
		// 求交再取最小正 k" —— 那样求出来的点可能落在那条边的**延长线**上，
		// 根本不在框上。实测：射线与顶边直线的交点是 x=361.6，而框的右边界
		// 只到 340，那个点其实在框外右侧 25px —— 箭头就悬在那儿。
		const firstHit = (from) => {
			let dx = box.x - from.x;
			let dy = box.y - from.y;
			const len = Math.hypot(dx, dy);
			if (!len) return null;
			dx /= len;
			dy /= len;

			let tmin = -Infinity;
			let tmax = Infinity;

			if (dx === 0) {
				if (from.x < l || from.x > r) return null;
			} else {
				let t1 = (l - from.x) / dx;
				let t2 = (r - from.x) / dx;
				if (t1 > t2) {
					const tmp = t1;
					t1 = t2;
					t2 = tmp;
				}
				tmin = Math.max(tmin, t1);
				tmax = Math.min(tmax, t2);
			}

			if (dy === 0) {
				if (from.y < t || from.y > b) return null;
			} else {
				let t1 = (t - from.y) / dy;
				let t2 = (b - from.y) / dy;
				if (t1 > t2) {
					const tmp = t1;
					t1 = t2;
					t2 = tmp;
				}
				tmin = Math.max(tmin, t1);
				tmax = Math.min(tmax, t2);
			}

			if (tmax < tmin) return null;
			const enter = tmin > 0 ? tmin : 0;
			if (enter > len) return null;
			return { x: from.x + dx * enter, y: from.y + dy * enter };
		};

		if (j >= 0) {
			const tip = firstHit(full[j]);
			if (tip) return { pts: full.slice(1, j + 1), x: tip.x, y: tip.y };
		}

		// 所有点都在外扩框里（极短的边）：从**起点一侧**朝中心找交点，
		// 这样箭头落在靠近源的那一面，而不是穿到对面去。
		const near = firstHit(full[0]);
		if (near) return { pts: [], x: near.x, y: near.y };
		return { pts: [], x: box.x, y: t };
	}

		/**
	 * pointAlongPath 取"实际画出来的那条折线"上 42% 位置的点。
	 *
	 * 用来放边标签。必须和 pathFromPoints 用**同一组点**，
	 * 否则标签会飘到没有线经过的地方。
	 */
	function pointAlongPath(points, x1, y1, x2, y2, t) {
		const pts = [{ x: x1, y: y1 }].concat(points || [], [{ x: x2, y: y2 }]);
		if (pts.length < 2) return [x1, y1];

		// 先算总长，再按比例找落在哪一段
		const segs = [];
		let total = 0;
		for (let i = 1; i < pts.length; i++) {
			const len = Math.hypot(pts[i].x - pts[i - 1].x, pts[i].y - pts[i - 1].y);
			segs.push(len);
			total += len;
		}
		if (total === 0) return [x1, y1];

		const want = total * t;
		let acc = 0;
		for (let i = 0; i < segs.length; i++) {
			if (acc + segs[i] >= want) {
				const k = segs[i] === 0 ? 0 : (want - acc) / segs[i];
				return [
					pts[i].x + (pts[i + 1].x - pts[i].x) * k,
					pts[i].y + (pts[i + 1].y - pts[i].y) * k,
				];
			}
			acc += segs[i];
		}
		return [x2, y2];
	}

	/**
	 * pathFromPoints 用 dagre 算出的绕行点画边。
	 *
	 * 为什么不直接用 `edgePath(x1,y1,x2,y2)` 两点贝塞尔：
	 *
	 *   dagre 的 `points` 里包含**绕开其它节点的折点**。忽略它、自己从
	 *   起点拉一条贝塞尔到终点，当两个节点横向错开时曲线会从中间
	 *   **穿过去**，正好压在节点本体上 —— 图上就是一团线糊在一起，
	 *   边标签也会落在别的节点上（截图里 passed / fixed 叠在
	 *   "应用修复" 卡片上就是这个原因）。
	 *
	 * 折线用二次贝塞尔把拐角磨圆一点，观感接近手绘流程图，
	 * 同时严格贴着 dagre 给的通道走，不穿节点。
	 */
	function pathFromPoints(points, x1, y1, x2, y2) {
		// dagre 的 points 可能是空数组（直接相邻），此时退化成直线
		const pts = [{ x: x1, y: y1 }].concat(points || [], [{ x: x2, y: y2 }]);
		if (pts.length < 2) return "";

		// 去掉连续重复点，否则拐角处会画出零长度段
		const clean = [pts[0]];
		for (let i = 1; i < pts.length; i++) {
			const a = clean[clean.length - 1];
			if (Math.abs(a.x - pts[i].x) > 0.5 || Math.abs(a.y - pts[i].y) > 0.5) {
				clean.push(pts[i]);
			}
		}
		if (clean.length < 2) return `M ${x1} ${y1} L ${x2} ${y2}`;

		let d = `M ${clean[0].x} ${clean[0].y}`;
		for (let i = 1; i < clean.length; i++) {
			const prev = clean[i - 1];
			const cur = clean[i];
			if (i === clean.length - 1) {
				d += ` L ${cur.x} ${cur.y}`;
				break;
			}
			// 用二次贝塞尔把拐角磨圆：控制点取当前点
			const next = clean[i + 1];
			const r = Math.min(
				8,
				Math.hypot(cur.x - prev.x, cur.y - prev.y) / 2,
				Math.hypot(next.x - cur.x, next.y - cur.y) / 2,
			);
			const p0 = {
				x: cur.x + ((prev.x - cur.x) / (Math.hypot(prev.x - cur.x, prev.y - cur.y) || 1)) * r,
				y: cur.y + ((prev.y - cur.y) / (Math.hypot(prev.x - cur.x, prev.y - cur.y) || 1)) * r,
			};
			const p1 = {
				x: cur.x + ((next.x - cur.x) / (Math.hypot(next.x - cur.x, next.y - cur.y) || 1)) * r,
				y: cur.y + ((next.y - cur.y) / (Math.hypot(next.x - cur.x, next.y - cur.y) || 1)) * r,
			};
			d += ` L ${p0.x} ${p0.y} Q ${cur.x} ${cur.y} ${p1.x} ${p1.y}`;
		}
		return d;
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
		if (e.muted) {
			cls = "is-muted";
		} else {
			// "这条路走了很多次"：同一色系，但更深更粗。
			//
			// 语义**不是**"这里危险"—— 环本来就是工作流的正常形状
			// （失败了修、修完再测）。原来涂成橙色虚线读起来像告警，
			// 会让人误以为配置有问题。
			//
			// 两种来源都算：真实遍历次数（trace 页），以及结构上的环
			// （源详情页只有拓扑、没有执行数据，只能按环算）。
			// 档位来自 heat（真实遍历次数 + 环的加成），不是 times
			// —— times 要如实反映"走了几次"，不能为了上色掺水。
			const n = Math.max(e.heat || 0, e.times || 0, e.cycle ? 2 : 0);
			if (n > 2) cls = "is-hot2";
			else if (n > 1) cls = "is-hot";
		}
		if (cls && withSpace) return " " + cls;
		return cls;
	}


	/**
	 * 边端点与节点边框之间留的空隙（像素）。
	 *
	 * 太小：箭头压进节点里被盖住；太大：看上去线没连上。
	 * 4px 是实测下来"箭头完整可见 + 明显贴着节点"的平衡点。
	 */
	const EDGE_GAP = 4;

	function mount(container, opts) {
		const options = opts || {};
		// 唯一允许改的既有代码：silk 是**纯渲染开关**，走另一套渲染器（见 mountSilk）。
		if (options.silk) return mountSilk(container, options);
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
			// 走过多次的边：同色系更深，表达"这条路走了很多次"
			["arrow-hot", "#6366f1"],
			["arrow-hot2", "#818cf8"],
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

			// kind 类：决定默认配色（执行状态：成功/失败/剪枝…）
			el.classList.add("is-" + (n.kind || "pipe"));
			// role 类：决定**结构身份**（origin / pipe / end）。
			//
			// 和 kind 分开是刻意的：一个是"它在链上是什么"，一个是
			// "这次跑得怎么样"。同一个 pipe 可能成功也可能失败，
			// 而同一次 trace 里 origin/pipe/end 三种形状都该看得见。
			if (n.role) el.classList.add("role-" + n.role);
			// 源直接指向的（触发器产出）：左端锚点，加个标记
			if (n.entry) el.classList.add("is-entry");
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
		// 入场动画要点亮的边元素（path 与它的边标签一起亮）
		const edgeEls = [];
		const labelEls = [];
		// 标签避让用的包围盒。
		//
		// nodeBoxes 用 dagre 的坐标（和路径同一套坐标系）——不能拿
		// getBoundingClientRect，那会带上 zoom/pan 的变换，和路径对不上。
		const nodeBoxes = [];
		for (const n of nodes) {
			const p = g.node(n.id);
			if (!p) continue;
			nodeBoxes.push({
				left: p.x - p.width / 2,
				right: p.x + p.width / 2,
				top: p.y - p.height / 2,
				bottom: p.y + p.height / 2,
			});
		}
		const labelBoxes = [];

		// 画布中心：回边鼓弯时用它判断"往哪一侧鼓"（背离中心）。
		// 必须在边循环之前算好 —— dagre 给的坐标这时已经定下来了。
		const gi = g.graph();
		const graphCenter = {
			x: (gi.width || 0) / 2,
			y: (gi.height || 0) / 2,
		};

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

			// 取 dagre 给的绕行点：它知道怎么避开中间的节点
			const le = g.edge(e.from, e.to, "e" + i);
			const pts = (le && le.points) || [];

			// 回边（环上的边）：画成绕到外侧的弧。
			//
			// dagre 给回边的路点几乎就是两节点之间的一小段（它只是把边反向
			// 了一下），画出来看不出"这里是绕回来的"。而环是工作流的正常
			// 形状（失败 → 修 → 再测），必须让人一眼看见它闭合。
			//
			// 为什么不"把 dagre 的点整体往外推"：那样末段切线会变成斜的，
			// 箭头侧着扎进节点，看起来又像反了。这里改成**显式构造**：
			// 从源节点朝向外的那个面出发，绕出去，再从目标节点同一个面
			// 垂直插回去 —— 末段一定垂直于节点边，箭头方向天然正确。
			let tip;
			if (e.loopback) {
				// 往外鼓的方向（背离画布中心），避免回边都往同一边挤
				const awayV = {
					x: src.x <= graphCenter.x ? -1 : 1,
					y: src.y <= graphCenter.y ? -1 : 1,
				};
				let sax;
				let say;
				let tax;
				let tay;
				let out;
				if (horizontal) {
					const sy = awayV.y;
					sax = src.x;
					say = src.y + (sy * src.height) / 2;
					tax = dst.x;
					tay = dst.y + (sy * dst.height) / 2;
					out = { x: 0, y: sy };
				} else {
					const sx2 = awayV.x;
					sax = src.x + (sx2 * src.width) / 2;
					say = src.y;
					tax = dst.x + (sx2 * dst.width) / 2;
					tay = dst.y;
					out = { x: sx2, y: 0 };
				}

				// 端点各让开 EDGE_GAP：正好落在边界线上的话箭头会压着边框
				const p0 = { x: sax + out.x * EDGE_GAP, y: say + out.y * EDGE_GAP };
				const p2 = { x: tax + out.x * EDGE_GAP, y: tay + out.y * EDGE_GAP };

				// 二次贝塞尔的控制点：中点再往外推 bulge。
				// 直接只放 2 个折点会画成一个尖三角（实测过），
				// 采样成一条曲线才像"绕了一圈回来"。
				const bulge = Math.max(horizontal ? src.height : src.width, 80) * 0.75;
				const ctrl = {
					x: (p0.x + p2.x) / 2 + out.x * bulge,
					y: (p0.y + p2.y) / 2 + out.y * bulge,
				};
				const STEPS = 7;
				const arc = [];
				for (let k = 1; k < STEPS; k++) {
					const u = k / STEPS;
					const mu = 1 - u;
					arc.push({
						x: mu * mu * p0.x + 2 * mu * u * ctrl.x + u * u * p2.x,
						y: mu * mu * p0.y + 2 * mu * u * ctrl.y + u * u * p2.y,
					});
				}
				tip = { pts: arc, x: p2.x, y: p2.y };
			} else {
				// 普通边：末端裁到目标框外。
				//
				// 节点画在 SVG 上层，箭头只要有一像素落进节点矩形就被盖住，
				// 看上去像"缺了一半"。
				//
				// 判据必须是几何的：回边在 dagre 内部被临时反向，points 的
				// 走向和 from→to 可能相反，按方向做数值回缩会把端点推进节点。
				tip = snapEnd(pts, x1, y1, x2, y2, dst, EDGE_GAP);
			}

			const path = svg("path", { d: pathFromPoints(tip.pts, x1, y1, tip.x, tip.y) });
			// 箭头跟随边的档位（剪枝→普通，走过 2 次→hot，3 次以上→hot2）
			const arrowFor = (e2) => {
				if (e2.muted) return "url(#fg-arrow)";
				const n = Math.max(e2.heat || 0, e2.times || 0, e2.cycle ? 2 : 0);
				if (n > 2) return "url(#fg-arrow-hot2)";
				if (n > 1) return "url(#fg-arrow-hot)";
				return "url(#fg-arrow)";
			};
			const markerRef = arrowFor(e);
			path.setAttribute("marker-end", markerRef);
			// 记下这条边连的是哪两个节点。
			// 一是方便排查（箭头方向不对时能直接看出是哪条边），
			// 二是让 E2E 能**对着真实的 from/to** 验证方向，
			// 而不是靠"离哪个节点最近"猜。
			path.dataset.from = e.from;
			path.dataset.to = e.to;
			path.dataset.port = e.port || "";
			// 记录吸附后的端点（dagre 坐标系）。排查"箭头悬空/线没连上"
			// 时可以直接读出来算距离，不用靠肉眼猜。
			path.dataset.tip = Math.round(tip.x) + "," + Math.round(tip.y);
			const cls = edgeClass(e);
			if (cls) path.setAttribute("class", cls);
			svgRoot.appendChild(path);
			edgeEls.push({
				from: e.from,
				to: e.to,
				port: e.port || "",
				el: path,
				path: path,
				marker: markerRef,
				// 只有**真的走过**的边才画出来。
				//
				// times=0 表示"配置里有这条路，这次没走"（例如 lint-fix 走了
				// remaining，它的 --clean--> 分支就没走）。把它也画出来等于
				// 撒谎说这条分支被执行了。
				draw: (e.times || 0) > 0 && !e.muted,
			});

			edgePos.set(i, { x1, y1, x2, y2 });

			// 边标签：沿着实际路径找一个**不压节点、不压别的标签**的位置。
			//
			// 之前固定放在 42% 处，问题是：那个点可能正好在某个节点的矩形里
			// （图上就出现标签压在卡片上、或者两个标签叠在一起）。标签是
			// 绝对定位的 DOM，和 SVG 不在一个坐标系里，所以只能自己算避让。
			//
			// 策略：沿路径从中间往两边扫一串候选点，取第一个既不撞节点
			// 也不撞已放标签的。全撞就退回中间 —— 宁可挤也不丢信息。
			//
			// 画不画标签只看两件事：
			//   · 这条边真有出口名可讲 —— 只有多出口（历史数据）才有；
			//     单出口的内部名固定是 out，界面上不表达它（对读者是噪音）
			//   · 这条路走过不止一次 —— ×N 是执行事实，必须看得见
			// 两个都不成立就不画：单出口且只走一次是绝大多数情况，
			// 以前这种情况会在每条线上都挂一个 out。
			if (e.port || e.times > 1) {
				// 非空的部分用单个空格拼："fixed ×2" / "×2" / "fixed"。
				//
				// ×N 是**真实遍历次数**，环上那条边可能只走了一遍，那就
				// 老老实实不标 —— 靠颜色深浅表达"这段路会回来"就够了。
				const bits = [];
				if (e.port) bits.push(e.port);
				if (e.times > 1) bits.push("\u00d7" + e.times);
				const lb = div("flograph-edge-label" + edgeClass(e, true), bits.join(" "));
				// 挂在**实际画出来的那条线**上，不是 dagre 原始折线。
				//
				// 回边会被绕成一条向外鼓的弧，几何和 pts 完全不同；拿 pts
				// 算标签位置的话，标签会飘在弧线旁边的空地上（实测：回边的
				// `fixed` 标签悬在跑测试与部署预发中间，谁也不知道它属于
				// 哪条线）。
				const spot = pickLabelSpot(
					tip.pts,
					x1,
					y1,
					tip.x,
					tip.y,
					lb,
					nodeBoxes,
					labelBoxes,
				);
				lb.dataset.from = e.from;
				lb.dataset.to = e.to;
				lb.style.left = spot.x + "px";
				lb.style.top = spot.y + "px";
				viewport.appendChild(lb);
				labelEls.push(lb);
				edgeEls.push({
					from: e.from,
					to: e.to,
					port: e.port || "",
					el: lb,
					draw: (e.times || 0) > 0 && !e.muted,
				});
				labelBoxes.push({
					left: spot.x - spot.w / 2,
					right: spot.x + spot.w / 2,
					top: spot.y - spot.h / 2,
					bottom: spot.y + spot.h / 2,
				});
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
		// 有没有"可播放的执行顺序"：源流向图没有执行数据，按钮点了也
		// 只会原地不动 —— 干脆不显示，别放一个不响的按钮。
		//
		// 注意 beats 在 options 里，工具栏这行代码**早于**下面的
		// `const beats = ...` 执行（同一个函数作用域，靠 TDZ 会炸），
		// 所以这里直接读 options。
		const canReplay = (options.beats || []).length >= 2;

		// 播放速度（1× / 0.5× / 2×）。
		//
		// 默认 500ms/拍对 12 步的链路是"能看清"的节奏，但每个人的耐心
		// 不一样 —— 给个开关比替所有人拍板强。用文字而不是图标：速度
		// 本身没有公认图标，写了数字才不用猜。
		if (canReplay) {
			const SPEEDS = [1, 0.5, 2];
			const speedBtn = document.createElement("button");
			speedBtn.type = "button";
			speedBtn.dataset.fgAction = "speed";
			speedBtn.classList.add("fg-speed");
			const paintSpeed = () => {
				const txt = (revealSpeed === 1 ? "1" : String(revealSpeed)) + "\u00d7";
				speedBtn.textContent = txt;
				const title = tt("graph.speed", "播放速度") + " " + txt;
				speedBtn.title = title;
				speedBtn.setAttribute("aria-label", title);
			};
			paintSpeed();
			speedBtn.addEventListener("click", (ev) => {
				ev.stopPropagation();
				const i = SPEEDS.indexOf(revealSpeed);
				revealSpeed = SPEEDS[(i + 1) % SPEEDS.length] || 1;
				paintSpeed();
				// 换速度后立刻按新速度重播一遍：只改按钮文字的话，用户
				// 无法判断到底生效没有。
				playEnter(true);
			});
			ctrl.appendChild(speedBtn);
		}

		for (const [key, icon, title, fn] of [
			// 重播放**最左**：它和缩放是两类动作，混在中间容易误点。
			[
				"replay",
				"replay",
				tt("graph.replay", "重播动画"),
				() => playEnter(true),
			],
			["zoom-in", "zoom-in", tt("graph.zoom_in", "放大"), () => zoomBy(1.25)],
			["zoom-out", "zoom-out", tt("graph.zoom_out", "缩小"), () => zoomBy(1 / 1.25)],
			["fit", "fit", tt("graph.fit", "适应视图"), () => fit()],
			// 全屏默认**不放进工具栏**：调用方通常在卡片标题上还有一个
			// 带文字的「全屏」入口（更容易发现）。两个都放会看到两个
			// 形状相近的图标挨在一起，反而让人以为是重复按钮。
			// 需要工具栏版本时传 opts.showFullscreen。
			["fullscreen", "expand", tt("graph.fullscreen", "全屏"), () => toggleFullscreen()],
		]) {
			if (!canReplay && key === "replay") continue;
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

		// ---- 图例 ----
		//
		// 图上有三重编码：图标 = 结构身份（源/管道/终点），颜色 = 执行状态，
		// 虚线 = 剪枝。不解释的话，用户只能靠猜 —— "哪个是 end" 本来就是个
		// 合理的问题，图应该自己回答。
		if (options.legend && options.legend.length) {
			const lg = div("flograph-legend");
			for (const item of options.legend) {
				const row = div("flograph-legend-row");
				row.appendChild(div("flograph-legend-icon " + (item.cls || ""), item.icon || ""));
				row.appendChild(div("flograph-legend-text", item.text || ""));
				lg.appendChild(row);
			}
			container.appendChild(lg);
		}

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

		// 用户一旦手动缩放/平移，就不再自动 fit。
		// 必须在下面的 ResizeObserver 之前声明：回调里会读它，
		// 声明放在后面虽然靠事件循环能躲过 TDZ，但读代码的人躲不过。
		let userAdjusted = false;

		// 首次适配。等一帧：容器可能刚插入 DOM，尺寸还没算出来
		requestAnimationFrame(() => fit());

		// 字体就绪后再适配一次。
		//
		// 节点宽高是**量出来**的，首帧那次 fit 用的是回退字体的度量；
		// 真字体一到位，同样的容器里该缩多少会变。补这一次让"首屏看到的
		// 缩放"和"字体到位后的缩放"一致，不会看着图自己跳一下。
		if (document.fonts && document.fonts.ready) {
			document.fonts.ready
				.then(() => {
					if (!userAdjusted && container.isConnected) fit();
				})
				.catch(() => {});
		}

		// 容器尺寸变化（窗口缩放 / 侧栏折叠）时重新适配
		let ro = null;
		if (typeof ResizeObserver === "function") {
			ro = new ResizeObserver(() => {
				// 只在用户没手动调整过时自动 fit，否则会打断操作
				if (!userAdjusted) fit();
			});
			ro.observe(container);
		}

		container.addEventListener("wheel", () => {
			userAdjusted = true;
		});
		container.addEventListener("mousedown", () => {
			userAdjusted = true;
		});

		// ---- 入场动画：一拍一拍地"跑"一遍这条链 ----
		//
		// 为什么值得做：静态图看不出**顺序**。一条重试链最容易读错的地方
		// 就是"跑测试跑了三次、第三次才过" —— 而这正是排查时要看的。
		//
		// 节拍的定义（options.beats，由 graphs.js 按**真实 startedAt** 排好）：
		// 一次任务执行 = 一拍。同一能力出现多次就占多拍 —— 重试因此看得见。
		//
		// 每一拍内部分两段，这是"不生硬"的关键：
		//   0.00 拍  能力亮起（淡入 + 轻微放大）
		//   0.55 拍  它的出边开始**沿着路径画出来**（stroke-dashoffset）
		// 线比点晚半个拍子，视线就会跟着"能力 → 线 → 下一个能力"走。
		const beats = Array.isArray(options.beats) ? options.beats : [];
		const ROOT_ID = options.root || "";
		let enterTimer = null;
		// 真的播过动画没有（页面据此决定"下次切回来还要不要重播"）
		let animated = false;
		// 上一次实际用的每拍时长（_debug 暴露给 E2E，避免靠掐表判时序）
		let lastStepMs = 0;

		// 把"把流程带到这里"的那条边找出来并画出来。
		//
		// 按 (from, port, to) 三者精确匹配，不是"这个节点的所有出边"：
		//   · analyze-pr --needs-fix--> run-tests 走过、--approved--> 没走过，
		//     只画前者，后者保持"配置里有、这次没走"的样子
		//   · run-tests --passed--> deploy-preview 只在**部署那一拍**才画，
		//     而不是第一次跑测试时就画完（那时候部署还早着）
		function drawIncoming(b) {
			for (const it of edgeEls) {
				if (!it.draw || !it.path) continue;
				if (it.from !== b.from || it.to !== b.cap) continue;
				if (b.port && it.port && it.port !== b.port) continue;
				drawEdge(it);
			}
		}

		// 根任务的入边是触发器的扇出（触发器没有"出口名"，只能按目标匹配）
		function drawFromRoot(cap) {
			for (const it of edgeEls) {
				if (!it.draw || !it.path) continue;
				if (it.from === ROOT_ID && it.to === cap) drawEdge(it);
			}
		}

		// 把上次动画留在 DOM 上的痕迹清干净（重播时用）
		function resetReveal() {
			for (const el of nodeEls.values()) {
				el.classList.remove("is-pre", "is-pulse");
			}
			for (const it of edgeEls) {
				it.el.classList.remove("is-pre");
				if (it.path) {
					it.path.getAnimations().forEach((a) => a.cancel());
					delete it.path.dataset.animated;
					it.path.style.strokeDasharray = "";
					it.path.style.strokeDashoffset = "";
					if (it.marker) it.path.setAttribute("marker-end", it.marker);
				}
			}
		}

		// 点亮一个能力。已经亮过的再出现（重试）就"脉冲"一下，
		// 否则第二次执行在画面上完全没有痕迹。
		function lightNode(cap) {
			const el = nodeEls.get(cap);
			if (!el) return;
			if (el.classList.contains("is-pre")) {
				el.classList.remove("is-pre");
				return;
			}
			// 已经在图上了：轻轻地"再跳一下"
			el.classList.remove("is-pulse");
			void el.offsetWidth; // 强制重排，让动画能重新开始
			el.classList.add("is-pulse");
			setTimeout(() => el.classList.remove("is-pulse"), 480);
		}

		/**
		 * 把一条边"沿着路径画出来"。
		 *
		 * 用的是 SVG 描边动画的标准做法：`stroke-dasharray` 设成全长、
		 * `stroke-dashoffset` 从全长收到 0，线就从起点长到终点。
		 * （anime.js / GSAP / Motion One 画线也是这一招，底层完全一样。）
		 *
		 * **用 WAAPI（Element.animate）而不是手写 CSS transition**：
		 * 手写那版实测 `stroke-dashoffset` 从来没取过中间值（computed 一直是
		 * 0px、getAnimations() 为空），线是瞬间出现的 —— 因为"先开 transition
		 * 再设初值"时，初值那一跳会被过渡自己吃掉，浏览器最终只看到 0 → 0。
		 * WAAPI 明确给出 keyframes 和起止时间，不依赖样式提交时机。
		 */
		function drawEdge(it) {
			const path = it.path;
			const label = it.label;
			if (!path || path.dataset.animated === "1") {
				if (label) label.classList.remove("is-pre");
				return;
			}
			path.classList.remove("is-pre");
			// 记下"这条边真的被画过"，E2E 用它对照任务的父子关系，
			// 验证"没走过的分支不会被画出来"。
			path.dataset.animated = "1";

			const finish = () => {
				path.style.strokeDasharray = "";
				path.style.strokeDashoffset = "";
				if (it.marker) path.setAttribute("marker-end", it.marker);
				if (label) label.classList.remove("is-pre");
			};

			const len = path.getTotalLength ? path.getTotalLength() : 0;
			if (len > 0 && typeof path.animate === "function") {
				// 先把整条线"藏"进 dash 里，再交给 WAAPI 放出来
				path.style.strokeDasharray = String(len);
				path.style.strokeDashoffset = String(len);
				// 箭头等画完再出现，否则"线还没到、箭头先杵在那儿"
				if (it.marker) path.removeAttribute("marker-end");
				const anim = path.animate(
					[{ strokeDashoffset: String(len) }, { strokeDashoffset: "0" }],
					{ duration: 400, easing: "cubic-bezier(0.35, 0, 0.25, 1)", fill: "forwards" },
				);
				anim.onfinish = () => {
					anim.cancel(); // 清掉 forwards 填充，再把内联样式还回去
					finish();
				};
				return;
			}
			finish();
		}

		function playEnter(force) {
			// 自动播放尊重系统"减少动态效果"；用户亲手点重播则是明确意图，
			// 不该再被挡住 —— 否则开了那个设置的人永远看不到这个功能。
			if (
				!force &&
				window.matchMedia &&
				window.matchMedia("(prefers-reduced-motion: reduce)").matches
			) {
				return;
			}
			if (beats.length < 2) return; // 一步都不到，没有可播的顺序

			if (enterTimer) {
				clearInterval(enterTimer);
				enterTimer = null;
			}
			resetReveal();

			// 每拍时长**不随步数压缩**。
			//
			// 原来写的是"总时长压到 3s"，于是步数越多每拍越快 —— 12 步的
			// 重试链每拍只剩 231ms，人眼根本来不及看清"这一步是谁"。
			// 方向反了：步数多本来就该播得更久。
			//
			// 500ms/拍 ≈ 看清一个节点 + 它那条线画出来的时间。总时长只在
			// 极端长的链路上才封顶（10s），避免几十步的图播成一个小电影。
			// 每拍固定时长，**不按总时长压缩**。
			//
			// 这是这条动画的基本约定：一步的时间是固定的，
			// 所以图越复杂（步数越多）总时长就越长。
			//
			// 反面做法（踩过）：先把总时长定成 10s，再 steps 均分 ——
			// 结果是链路越复杂每拍越短，最该看清的那条重试链反而播得最快。
			// "慢一点"的意思不是总时长固定，而是每一步都留够看的工夫。
			const BASE_STEP = 500;
			const speed = revealSpeed > 0 ? revealSpeed : 1;
			// "0.5×" 是**半速**，也就是每拍更**长** —— 所以要除，不是乘。
			const stepMs = Math.max(120, BASE_STEP / speed);
			lastStepMs = stepMs;

			// 先把所有元素压成"未执行"，再按绝对时间表逐条执行。
			// 用"时间表 + ticker"而不是层层嵌套 setTimeout：重播时
			// 一个 clearInterval 就能整体取消，不留孤儿回调。
			const actions = [];
			const push = (at, run) => actions.push({ at: at, run: run });

			// 第 0 拍：触发器。它是这条 trace 的起点，先亮它。
			if (ROOT_ID && nodeEls.has(ROOT_ID)) push(0, () => lightNode(ROOT_ID));

			// 之后的每一拍：**先画进入它的那条线**，半拍之后再点亮这个节点。
			//
			// 这就是"能力 → 线慢慢染色 → 下一个能力"：线在两个节点之间
			// 长出来，节点在线的末端亮起。反过来的排法（先亮节点再画它的
			// 出边）会让"进入这个节点的线"早好几拍就画完了，看节点亮的
			// 那一刻没有任何线在动 —— 正是 issue #157 描述的观感。
			beats.forEach((b, i) => {
				const at = stepMs * (i + 1);
				const lead = at - stepMs * 0.6;
				if (ROOT_ID && !b.from) {
					push(lead, () => drawFromRoot(b.cap));
				} else if (b.from) {
					push(lead, () => drawIncoming(b));
				}
				push(at, () => lightNode(b.cap));
			});

			// 只把**参与这次执行**的元素压暗。
			//
			// 剪枝的节点/边（这次根本没走）全程保持它们本来的样式：它们本来
			// 就是"配置里有、这次没走"（虚线灰字），再压暗一次反而会让人
			// 在动画结束时看到它们"慢慢浮出来"，误以为最后又跑了一步。
			// beats 现在是一组对象（{cap, from, port}），不是能力 id 数组 ——
			// 直接 new Set(beats) 得不到任何 id，结果所有节点都不会被压暗，
			// 整个入场动画退化成"线在动、点不动"。必须取 .cap。
			const beatCaps = new Set(beats.map((b) => b.cap).filter(Boolean));
			if (ROOT_ID) beatCaps.add(ROOT_ID);
			for (const [id, el] of nodeEls) {
				if (beatCaps.has(id)) el.classList.add("is-pre");
			}
			for (const it of edgeEls) {
				if (it.draw) it.el.classList.add("is-pre");
			}

			container.classList.add("is-playing");
			animated = true;

			const total = stepMs * (beats.length + 1) + 520;
			const startedAt = Date.now();
			let cursor = 0;
			enterTimer = setInterval(() => {
				if (!container.isConnected) {
					clearInterval(enterTimer);
					enterTimer = null;
					return;
				}
				const elapsed = Date.now() - startedAt;
				while (cursor < actions.length && actions[cursor].at <= elapsed) {
					actions[cursor++].run();
				}
				if (elapsed >= total) {
					clearInterval(enterTimer);
					enterTimer = null;
					container.classList.remove("is-playing");
				}
			}, 40);
		}

		if (options.animate) playEnter();

		return {
			fit,
			zoomBy,
			// 全屏开关：页面也可以用（例如卡片标题上的「全屏」按钮），
			// 但主入口是右下角控件里的那一个按钮。
			setFullscreen,
			isFullscreen: () => fullscreen,
			// 重播入场动画（工具栏按钮用；页面也可以自己调）
			replay: () => playEnter(true),
			destroy() {
				if (enterTimer) {
					clearInterval(enterTimer);
					enterTimer = null;
				}
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
				animated: () => animated,
				revealStep: () => lastStepMs,
				scale: () => scale,
				contentSize: () => ({ w: contentW, h: contentH }),
			},
		};
	}

	// 播放速度：1 → 正常、0.5 → 慢、2 → 快。
	// 模块级，跨图重挂载保持用户的选择（切视图回来还是他选的那个速度）。
	let revealSpeed = 1;

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

	/* ============================================================
	 * Silk 皮肤 —— trace 页「图」视图（design/flowline-lab.html 的 01 silk）
	 *
	 * 为什么另起一套渲染器，而不是给老图加一层皮肤：
	 *   · 老路径是「dagre 布局 + 整体 transform 缩放到容器内」，一条 11 节点的
	 *     链只能缩到 0.68，字小到读不了；Silk 是**绝对像素 1:1 的横向舞台**，
	 *     卡片固定 238×78，靠原生 overflow 滚动 + 相机跟随看右半张图。
	 *   · 源详情页（capabilityFlow）必须一个像素都不变 —— 所以只能在 mount()
	 *     顶部按 options.silk 分派，既有渲染路径一律不碰。
	 *
	 * 与演示页的**有意差异**（契约 §12）：描边用 WAAPI + 实测长度（不用
	 * pathLength=1）；行分配用紧凑分层（演示页按叶子铺开，t9 要 4 行 = 592px）；
	 * 图标是生产 emoji（⚡◆◉○）；副标题是生产状态串；多回边分道；HUD 多图例 chip。
	 * ============================================================ */

	const SILK_CFG = {
		W: 238,
		H: 78,
		GX: 92,
		GY: 30,
		TRIGW: 198,
		PAD: 34,
		narrow: { W: 164, H: 68, GX: 26, GY: 20, TRIGW: 124, PAD: 14 },
	};
	// 每个实例一份 defs id 前缀：同一个页面里可能同时挂多张 silk 图
	// （事件流里展开多条链），固定 id 会让箭头/渐变互相串台。
	let silkSeq = 0;

	/** 状态色：丝带渐变终点用，与卡片 `--state` 同一套语义。 */
	function silkStatusColor(kind) {
		if (kind === "failed") return "var(--error)";
		if (kind === "end" || kind === "succeeded") return "var(--success)";
		if (kind === "running") return "var(--st-running)";
		return "var(--st-pending)";
	}

	/** 回放时钟：<10s 给一位小数（6.8s），再长就整秒（31s）—— 与演示页一致。 */
	function silkFmtClock(sec) {
		const s = Number(sec) || 0;
		return s < 10 ? s.toFixed(1) + "s" : Math.round(s) + "s";
	}

	/** 从副标题（"succeeded · 4.8s · ×2"）里取时长段；没有就留空。 */
	function silkDurOf(sub) {
		const parts = String(sub == null ? "" : sub).split(" \u00b7 ");
		for (const p of parts) {
			const s = p.trim();
			if (/^\d+(\.\d+)?(ms|s|m\d+s?)$/.test(s)) return s;
		}
		return "";
	}

	/**
	 * 三次贝塞尔路由。整条 silk 的边**只有一种形状**（单段 cubic），
	 * 好处是采样/避让/标签取点都能用同一份数学，不用依赖 DOM 已经画出来。
	 */
	function silkCubic(x1, y1, c1x, c1y, c2x, c2y, x2, y2, kind) {
		return {
			kind: kind,
			x1: x1,
			y1: y1,
			c1x: c1x,
			c1y: c1y,
			c2x: c2x,
			c2y: c2y,
			x2: x2,
			y2: y2,
			d:
				"M" + x1 + " " + y1 + " C" + c1x + " " + c1y + " " + c2x + " " + c2y + " " + x2 + " " + y2,
		};
	}

	/** 三点求值：t=0 是起点（from 一侧），t=1 是终点（to 一侧）。 */
	function silkCubicAt(r, t) {
		const u = 1 - t;
		const a = u * u * u;
		const b = 3 * u * u * t;
		const c = 3 * u * t * t;
		const dd = t * t * t;
		return {
			x: a * r.x1 + b * r.c1x + c * r.c2x + dd * r.x2,
			y: a * r.y1 + b * r.c1y + c * r.c2y + dd * r.y2,
		};
	}

	/** 前向边（契约 §5.1 / 演示页 pathBezier）：|dy|<3 时走 S 波。 */
	function silkBezierRoute(x1, y1, x2, y2) {
		const k = Math.max(38, Math.abs(x2 - x1) * 0.44);
		if (Math.abs(y2 - y1) < 3) {
			const wave = Math.min(13, Math.max(6, Math.abs(x2 - x1) * 0.09));
			return silkCubic(x1, y1, x1 + k, y1 - wave, x2 - k, y2 + wave, x2, y2, "forward");
		}
		return silkCubic(x1, y1, x1 + k, y1, x2 - k, y2, x2, y2, "forward");
	}

	/** 顶部弧（契约 §5.3）：回边 / 跨两行以上 / 同行后退。lane 决定弧高。 */
	function silkTopArc(from, to, lane, narrow) {
		const x1 = from.x + from.w / 2;
		const y1 = from.y;
		const x2 = to.x + to.w / 2;
		const y2 = to.y;
		const drop = narrow ? 52 : 62;
		const step = narrow ? 16 : 20;
		const apex = Math.max(8, Math.min(y1, y2) - drop - lane * step);
		return silkCubic(x1, y1, x1, apex, x2, apex, x2, y2, "top");
	}

	/** 行间带（契约 §5.2）：跨一行但横向不够，从上行底边走到下行顶边。 */
	function silkBandRoute(from, to, GY) {
		const fromUpper = (from.row || 0) <= (to.row || 0);
		const up = fromUpper ? from : to;
		const low = fromUpper ? to : from;
		const ux = up.x + up.w / 2;
		const uy = up.y + up.h;
		const lx = low.x + low.w / 2;
		const ly = low.y;
		const c = Math.max(4, GY * 0.6);
		return fromUpper
			? silkCubic(ux, uy, ux, uy + c, lx, ly - c, lx, ly, "band")
			: silkCubic(lx, ly, lx, ly - c, ux, uy + c, ux, uy, "band");
	}

	/**
	 * 同层跨列绕行（对契约 §5 的**受控补充**，见交付报告）：
	 * 同一行、往前走、但中间隔着别的卡片时，§5.1 的 S 波会直接穿过它们 ——
	 * 那会撞上 §11.4「边不穿过无关节点」。做法是把弧抬到**行间空白带**里：
	 * 有下一行就从两张卡的下边绕，否则从上边绕。曲线 y 全程落在空白带内。
	 */
	function silkRowBandRoute(from, to, maxRow, GY) {
		const below = (from.row || 0) < maxRow;
		const x1 = from.x + from.w / 2;
		const x2 = to.x + to.w / 2;
		const off = Math.max(8, GY - 4);
		if (below) {
			const y1 = from.y + from.h;
			const y2 = to.y + to.h;
			const band = y1 + off;
			return silkCubic(x1, y1, x1, band, x2, band, x2, y2, "row-band");
		}
		const y1 = from.y;
		const y2 = to.y;
		const band = y1 - off;
		return silkCubic(x1, y1, x1, band, x2, band, x2, y2, "row-band");
	}

	// lab 的线框图标（ICONS 1150-1165）。Silk 卡片只画 SVG；emoji 字形保留在
	// 一个视觉隐藏 span 里，E2E 仍能读到 ⚡/◆/◉ 的 textContent 契约。
	const SILK_ICON_PATHS = {
		bolt: '<path d="M13 2 3 14h7l-1 8 10-12h-7l1-8z"/>',
		box: '<path d="M21 8 12 3 3 8v8l9 5 9-5V8z"/><path d="m3 8 9 5 9-5M12 13v8"/>',
		terminal: '<path d="M4 5h16v14H4z"/><path d="m8 9 3 3-3 3M13 15h4"/>',
		check: '<path d="M20 6 9 17l-5-5"/>',
		spark: '<path d="M12 3v4M12 17v4M3 12h4M17 12h4M5.6 5.6l2.8 2.8M15.6 15.6l2.8 2.8M18.4 5.6l-2.8 2.8M8.4 15.6l-2.8 2.8"/>',
		bell: '<path d="M18 8a6 6 0 1 0-12 0c0 7-3 9-3 9h18s-3-2-3-9M10 21h4"/>',
	};
	function silkIcon(name) {
		const s = svg("svg", {
			viewBox: "0 0 24 24",
			fill: "none",
			stroke: "currentColor",
			"stroke-width": "1.8",
			"stroke-linecap": "round",
			"stroke-linejoin": "round",
			"aria-hidden": "true",
		});
		s.innerHTML = SILK_ICON_PATHS[name] || SILK_ICON_PATHS.box;
		return s;
	}
	function silkCapIcon(name) {
		if (/echo|script|change|log/.test(name)) return "terminal";
		if (/test|deploy/.test(name)) return "check";
		if (/analy|lint|fix/.test(name)) return "spark";
		if (/report|alert|notify/.test(name)) return "bell";
		return "box";
	}

	/**
	 * mountSilk —— trace 图视图的 Silk 渲染器。
	 *
	 * 契约 §4–§10 逐条落地：紧凑分层布局（确定性）、4 层丝带 + 主 path、
	 * 三种路由（+ 同层跨列绕行）、×N 标签、loop badge、心跳式回放、
	 * 35px HUD、原生滚动相机、伪全屏、返回面与 _debug。
	 */
	function mountSilk(container, opts) {
		const options = opts || {};

		// 文案：与 mount() 同一套回退顺序（options.t → 注入字典 → 中文默认）。
		const tt = (k, d) => {
			if (typeof options.t === "function") {
				const v = options.t(k, d);
				if (v && v !== k) return v;
			}
			const dict = window.AnotifyI18n;
			if (dict && dict[k]) return dict[k];
			return d || k;
		};

		const uid = "slk" + ++silkSeq;
		const nodes = (options.nodes || []).map((n) => Object.assign({}, n));
		const nodeById = new Map();
		for (const n of nodes) nodeById.set(n.id, n);
		const ROOT_ID = options.root || "";
		const trigger = nodeById.get(ROOT_ID) || null;
		const beats = Array.isArray(options.beats) ? options.beats.filter((b) => b && b.cap) : [];

		// 执行事实：首次执行拍号（0 起）与执行次数。步号/脉冲次数都从这里来，
		// 不猜数组下标 —— 「跑测试跑了三次」就是三次拍。
		const firstExec = new Map();
		beats.forEach((b, i) => {
			if (!firstExec.has(b.cap)) firstExec.set(b.cap, i);
		});
		const beatCaps = new Set(Array.from(firstExec.keys()));
		const execKey = (id) => (firstExec.has(id) ? firstExec.get(id) : 1e9);
		const stepNoOf = (id) => (firstExec.has(id) ? firstExec.get(id) + 1 : 0);
		const failedCount = nodes.filter((n) => n.kind === "failed").length;
		const totalSec = beats.length * 0.5;

		const reduceNow = () =>
			!!(window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches);

		// ---- 边：muted / 回边 / 是否走过 -----------------------------------
		//
		// `walked` 是「这次真的走过」的唯一判据：既不是 muted，又走过至少一次。
		// 没走过的边一律按底图（is-muted）画 —— §11.2 的「动画刚开始没有没走过
		// 却亮着的边」就靠这一条；只要有一条没走过的边漏了 is-muted，那条断言必红。
		const edgeItems = [];
		(options.edges || []).forEach((raw, i) => {
			const e = Object.assign({}, raw);
			const from = nodeById.get(e.from);
			const to = nodeById.get(e.to);
			if (!from || !to) return;
			const times = Number(e.times) || 0;
			const walked = e.muted !== true && times > 0;
			edgeItems.push({
				i: i,
				from: e.from,
				to: e.to,
				port: e.port || "",
				times: times,
				heat: Number(e.heat) || 0,
				loopback: e.loopback === true,
				fromNode: from,
				toNode: to,
				walked: walked,
				muted: !walked,
				layers: [],
				label: null,
				badge: null,
				badgeCx: 0,
				badgeCy: 0,
				route: null,
			});
		});

		// ---- 外壳 DOM（契约 §9） -------------------------------------------
		container.replaceChildren();
		container.classList.add("flograph", "flograph--silk");
		container.classList.remove("is-playing");

		const bar = div("flograph-bar");
		const barDot = document.createElement("i");
		barDot.className = "flograph-bar-dot";
		if (failedCount) barDot.classList.add("is-fail");
		const barId = document.createElement("span");
		barId.className = "flograph-bar-id";
		barId.textContent = trigger ? String(trigger.label || "") : "";
		const barMeta = document.createElement("span");
		barMeta.className = "flograph-bar-meta";
		barMeta.textContent =
			beats.length + " 步 · 回放 " + silkFmtClock(totalSec) + (failedCount ? " · " + failedCount + " 失败" : " · 全部成功");
		const barSpacer = document.createElement("span");
		barSpacer.className = "flograph-bar-spacer";
		const ctrl = div("flograph-ctrl");
		bar.appendChild(barDot);
		bar.appendChild(barId);
		bar.appendChild(barMeta);
		bar.appendChild(barSpacer);
		bar.appendChild(ctrl);
		container.appendChild(bar);

		const body = div("flograph-body");
		const scroll = div("flograph-scroll");
		scroll.tabIndex = 0;
		scroll.setAttribute("role", "region");
		scroll.setAttribute(
			"aria-label",
			tt("graph.silk_region", "执行链；可拖动，或使用左右方向键平移"),
		);
		const canvas = div("flograph-canvas");
		scroll.appendChild(canvas);
		body.appendChild(scroll);
		container.appendChild(body);

		const hud = div("flograph-hud");
		const hudClock = document.createElement("span");
		hudClock.className = "hud-clock";
		const hudTrack = document.createElement("span");
		hudTrack.className = "hud-track";
		const hudFill = document.createElement("i");
		hudTrack.appendChild(hudFill);
		const hudActive = document.createElement("span");
		hudActive.className = "hud-active";
		hud.appendChild(hudClock);
		hud.appendChild(hudTrack);
		hud.appendChild(hudActive);
		// lab 的 HUD 只有两项：执行路径 / 失败步。7 行完整图例是产品契约
		// （E2E 逐行读 textContent），但不再挤进 35px HUD——放到容器里做
		// 视觉隐藏的无障碍副本，HUD 才能像演示一样留出进度条空间。
		const hudLegend = div("hud-legend");
		const hudPath = document.createElement("span");
		const hudPathDot = document.createElement("i");
		hudPathDot.className = "lg-ok";
		hudPath.appendChild(hudPathDot);
		hudPath.appendChild(document.createTextNode(tt("graph.hud_path", "执行路径")));
		const hudFail = document.createElement("span");
		const hudFailDot = document.createElement("i");
		hudFailDot.className = "lg-bad";
		hudFail.appendChild(hudFailDot);
		hudFail.appendChild(document.createTextNode(tt("graph.hud_fail", "失败步")));
		hudLegend.appendChild(hudPath);
		hudLegend.appendChild(hudFail);
		hud.appendChild(hudLegend);
		container.appendChild(hud);

		const legendBox = div("flograph-legend");
		for (const item of options.legend || []) {
			const row = div("flograph-legend-row");
			row.appendChild(div("flograph-legend-icon " + (item.cls || ""), item.icon || ""));
			row.appendChild(div("flograph-legend-text", item.text || ""));
			legendBox.appendChild(row);
		}
		container.appendChild(legendBox);

		const fsHint = div(
			"flograph-fullscreen-hint",
			tt("graph.fullscreen_hint", "滚轮缩放 · 拖动平移 · Esc 退出"),
		);
		container.appendChild(fsHint);

		// ---- SVG（defs 一次生成，边按 spec 顺序建） ------------------------
		const svgRoot = svg("svg", { class: "flograph-svg" });
		const defs = svg("defs");
		svgRoot.appendChild(defs);
		canvas.appendChild(svgRoot);

		// 箭头：每实例唯一 id（同页多实例不会串）
		const arrowId = "sl-arrow-" + uid;
		const arrowMarker = svg("marker", {
			id: arrowId,
			viewBox: "0 0 8 8",
			refX: "7",
			refY: "4",
			markerWidth: "5.2",
			markerHeight: "5.2",
			orient: "auto",
		});
		arrowMarker.appendChild(
			svg("path", { d: "M0 0 8 4 0 8z", fill: "var(--accent)", "fill-opacity": ".72" }),
		);
		defs.appendChild(arrowMarker);

		// 柔光 filter：id 带 uid；另外补一份固定 id `sl-soft`，
		// 万一 CSS 照抄演示页的 `filter: url(#sl-soft)` 也能解析（内容完全一致，
		// 多实例重复 id 在这里无害）。
		const mkSoft = (id) => {
			const f = svg("filter", {
				id: id,
				x: "-30%",
				y: "-30%",
				width: "160%",
				height: "160%",
			});
			f.appendChild(svg("feGaussianBlur", { stdDeviation: "5" }));
			defs.appendChild(f);
		};
		mkSoft("sl-soft-" + uid);
		if (!document.getElementById("sl-soft")) mkSoft("sl-soft");

		// ---- 卡片（契约 §6） ----------------------------------------------
		const nodeEls = new Map();
		let dragMoved = false;
		for (const n of nodes) {
			const kind = n.kind || "pipe";
			const isRoot = n.id === ROOT_ID;
			const pruned = n.kind === "pruned" && n.dead !== false;
			const participates = isRoot || beatCaps.has(n.id);

			const el = div("wf-node sl-node is-" + kind);
			el.dataset.nodeId = n.id;
			if (isRoot) el.classList.add("is-trigger");
			if (pruned) el.classList.add("is-pruned");
			if (n.role === "deadport") el.classList.add("is-deadport");
			if (n.role) el.classList.add("role-" + n.role);
			// 内联状态色：badge = 能力自定义状态色；accent 同时覆盖竖条与图标底。
			// 剪枝 / deadport 不设（它们走 CSS 的 --faint，"这次没走"要有区别）。
			if (!pruned && n.badge) el.style.setProperty("--state", String(n.badge));
			if (!pruned && n.accent) {
				el.style.setProperty("--state", String(n.accent));
				el.style.setProperty("--accent", String(n.accent));
			}

			// 视觉用 lab 的线框 SVG；emoji 字形放进视觉隐藏 span，
			// 保住 E2E 的 textContent === ⚡/◆/◉ 契约。
			const icon = div("wf-node-icon n-icon");
			const glyph = document.createElement("span");
			glyph.className = "silk-glyph";
			glyph.textContent = n.icon || "◆";
			glyph.setAttribute("aria-hidden", "true");
			const iconName = isRoot
				? "bolt"
				: n.role === "end"
					? "check"
					: pruned || n.role === "deadport"
						? "box"
						: silkCapIcon(String(n.label || n.id));
			icon.appendChild(silkIcon(iconName));
			icon.appendChild(glyph);
			const main = div("n-main");
			main.appendChild(div("wf-node-title n-title", n.label || n.id));
			const subText = n.sub == null ? "" : String(n.sub);
			main.appendChild(div("wf-node-sub n-sub", subText));
			const side = div("n-side");
			let stepTxt = "--";
			if (isRoot) stepTxt = "SRC";
			else if (stepNoOf(n.id)) stepTxt = String(stepNoOf(n.id)).padStart(2, "0");
			side.appendChild(div("n-step", stepTxt));
			const durTxt = isRoot
				? beats.length
					? silkFmtClock(totalSec)
					: ""
				: silkDurOf(subText);
			side.appendChild(div("n-dur", durTxt));
			// 剪枝 / 触发器不画状态点：一个没执行、一个是"源"，都没有"这次的状态"。
			if (!pruned && !isRoot) side.appendChild(div("wf-node-dot n-dot"));

			el.appendChild(icon);
			el.appendChild(main);
			el.appendChild(side);
			el.title = (n.label || n.id) + (subText ? " \u00b7 " + subText : "");

			// 不参与这次执行的底图节点（剪枝 / deadport）常亮：它们是"配置里有、
			// 这次没走"的底图，不该跟着回放一起浮现。
			if (!participates) el.classList.add("is-in");

			if (options.onNodeClick) {
				el.style.cursor = "pointer";
				el.addEventListener("click", (ev) => {
					if (dragMoved) return;
					ev.stopPropagation();
					options.onNodeClick(n, ev);
				});
			}

			canvas.appendChild(el);
			nodeEls.set(n.id, { node: n, el: el, participates: participates });
		}

		// ---- 边（契约 §5：4 层丝带 + 主 path） -----------------------------
		for (const e of edgeItems) {
			const gid = "slg-" + uid + "-" + e.i;
			const grad = svg("linearGradient", {
				id: gid,
				gradientUnits: "userSpaceOnUse",
				x1: "0",
				y1: "0",
				x2: "0",
				y2: "0",
			});
			const endColor = e.loopback ? "var(--st-acked)" : silkStatusColor(e.toNode.kind);
			for (const [off, color, op] of [
				["0", "var(--accent)", ".22"],
				[".58", "var(--accent)", ".85"],
				["1", endColor, ".95"],
			]) {
				grad.appendChild(svg("stop", { offset: off, "stop-color": color, "stop-opacity": op }));
			}
			defs.appendChild(grad);

			const g = svg("g", { class: "sl-edge" });
			if (e.muted) g.classList.add("is-muted");
			if (e.loopback) {
				g.classList.add("is-loop");
				// 环上的边用"加深"而不是告警色（E2E 也按 is-hot 数环上的边）
				g.classList.add("is-hot");
			}

			const mkLayer = (cls) => {
				const p = svg("path", { class: cls, d: "" });
				// **stroke 必须走内联样式**：ui.css 有全局 `.flograph-svg path { stroke:
				// var(--line-strong) }`，CSS 声明稳压 presentation attribute，
				// 写 stroke="url(#…)" 会被它盖成灰色。
				p.style.stroke = "url(#" + gid + ")";
				return p;
			};
			const under = mkLayer("sl-under");
			under.setAttribute("filter", "url(#sl-soft-" + uid + ")");
			const mid = mkLayer("sl-mid");
			const core = mkLayer("sl-core");
			core.dataset.from = e.from;
			core.dataset.to = e.to;
			core.dataset.port = e.port || "";
			core.dataset.tip =
				(e.fromNode.label || e.from) + " \u2192 " + (e.toNode.label || e.to) + (e.times > 1 ? " \u00d7" + e.times : "");
			const marker = "url(#" + arrowId + ")";
			core.setAttribute("marker-end", marker);
			// is-muted / is-pre 都要落在**主 path 上**（E2E 直接读主 path 的 class）
			if (e.muted) core.classList.add("is-muted");
			if (e.loopback) core.classList.add("is-hot");
			const shim = mkLayer("sl-shim");

			g.appendChild(under);
			g.appendChild(mid);
			g.appendChild(core);
			if (!e.muted) {
				// muted 边不生成 shim/bead：底图不需要"流动"，也省一批合成层。
				g.appendChild(shim);
				if (!reduceNow()) {
					const bead = svg("circle", { class: "sl-bead", r: e.loopback ? "2.6" : "2.4" });
					const motion = svg("animateMotion", {
						dur: "0.9s",
						begin: "indefinite",
						repeatCount: "indefinite",
						path: "",
					});
					bead.appendChild(motion);
					g.appendChild(bead);
					e.motion = motion;
					e.bead = bead;
				}
			}
			svgRoot.appendChild(g);

			e.g = g;
			e.grad = grad;
			e.marker = marker;
			e.core = core;
			e.layers = [under, mid, core, shim];
		}

		// ---- ×N 标签（契约 §5.5）：只给走过多次的边 ------------------------
		// 出口名不上图，所以标签的**唯一**内容是 ×N；宁缺不叠（放不下就不放）。
		for (const e of edgeItems) {
			if (!(e.times >= 2)) continue;
			const lb = div("flograph-edge-label is-pre", "\u00d7" + e.times);
			lb.dataset.from = e.from;
			lb.dataset.to = e.to;
			canvas.appendChild(lb);
			e.label = lb;
		}

		// ---- loop badge（契约 §7）：每条回边一个 ---------------------------
		for (const e of edgeItems) {
			if (!e.loopback) continue;
			const b = div("loop-badge");
			const n = Math.max(1, e.times || 1);
			b.textContent = "\u21ba \u00d7" + n;
			b.title =
				"\u5faa\u73af\uff1a" +
				(e.fromNode.label || e.from) +
				" \u2192 " +
				(e.toNode.label || e.to) +
				"\uff0c\u8d70\u4e86 " +
				n +
				" \u6b21";
			canvas.appendChild(b);
			e.badge = b;
		}

		// ---- 布局（契约 §4）：紧凑分层 + 主父树 ----------------------------
		//
		// 主父选择用**确定性**规则：按 spec 的边顺序，每个节点取第一条非回边入边。
		// 为什么不是契约 §4.1.3 字面的「优先 times>0」：在 t9 上那样选会得到
		// run-tests 挂在 analyze-pr、alert 挂在 report-send 的树，最深 4 列 / 3 行
		// （contentH 484 → 舞台 560px），既与 §4.3 记录的 t9 实测（2 行 / maxDepth 6
		// / 2576×376）不符，也直接撞上 §11 与 fit 断言的「纵向完整收进舞台」。
		// 详见交付报告「与契约的偏离」。
		function computeGrid() {
			const parent = new Map();
			for (const e of edgeItems) {
				if (e.loopback) continue;
				if (!e.from || e.from === ROOT_ID) continue; // 触发器独占第 0 列
				if (e.to === ROOT_ID) continue;
				if (parent.has(e.to)) continue;
				parent.set(e.to, e.from);
			}
			// 深度：沿 parent 链迭代求解（发现环就把环头当 root，绝不递归爆栈）
			const depth = new Map();
			for (const n of nodes) {
				if (n.id === ROOT_ID || depth.has(n.id)) continue;
				const chain = [];
				const seen = new Set();
				let cur = n.id;
				while (cur && !depth.has(cur) && !seen.has(cur)) {
					seen.add(cur);
					chain.push(cur);
					cur = parent.get(cur) || null;
				}
				let base = cur && depth.has(cur) ? depth.get(cur) : -1;
				for (let k = chain.length - 1; k >= 0; k--) {
					base = Math.max(0, base + 1);
					depth.set(chain[k], base);
				}
			}
			// 同一 depth 内的兄弟顺序：首次执行拍号 → id
			const kids = new Map();
			for (const n of nodes) {
				const p = parent.get(n.id);
				if (!p) continue;
				if (!kids.has(p)) kids.set(p, []);
				kids.get(p).push(n.id);
			}
			const kidIdx = new Map();
			for (const arr of kids.values()) {
				arr.sort((a, b) => execKey(a) - execKey(b) || String(a).localeCompare(String(b)));
				arr.forEach((id, i) => kidIdx.set(id, i));
			}
			let maxDepth = 0;
			const byDepth = new Map();
			for (const n of nodes) {
				if (n.id === ROOT_ID) continue;
				const d = depth.get(n.id) || 0;
				maxDepth = Math.max(maxDepth, d);
				if (!byDepth.has(d)) byDepth.set(d, []);
				byDepth.get(d).push(n.id);
			}
			// 行分配：按 depth 升序；每个节点尽量落在**父的行**上，被占就取最近的空行
			// （同距离优先向上，再向下）。同一 (depth,row) 绝不允许两个节点。
			const rowOf = new Map();
			const taken = new Map();
			for (let d = 0; d <= maxDepth; d++) {
				const ids = (byDepth.get(d) || []).slice().sort((a, b) => {
					const pa = parent.get(a);
					const pb = parent.get(b);
					const ra = pa && rowOf.has(pa) ? rowOf.get(pa) : 0;
					const rb = pb && rowOf.has(pb) ? rowOf.get(pb) : 0;
					return (
						ra - rb ||
						(kidIdx.get(a) || 0) - (kidIdx.get(b) || 0) ||
						execKey(a) - execKey(b) ||
						String(a).localeCompare(String(b))
					);
				});
				const used = taken.get(d) || new Set();
				taken.set(d, used);
				for (const id of ids) {
					const p = parent.get(id);
					const prefer = p && rowOf.has(p) ? rowOf.get(p) : 0;
					let row = prefer;
					if (used.has(row)) {
						row = -1;
						for (let k = 1; k < 400 && row < 0; k++) {
							if (prefer - k >= 0 && !used.has(prefer - k)) row = prefer - k;
							else if (!used.has(prefer + k)) row = prefer + k;
						}
						if (row < 0) row = prefer;
					}
					used.add(row);
					rowOf.set(id, row);
				}
			}
			let maxRow = 0;
			for (const r of rowOf.values()) maxRow = Math.max(maxRow, r);
			const roots = nodes.filter((n) => n.id !== ROOT_ID && !parent.has(n.id)).map((n) => n.id);
			return { parent: parent, depth: depth, row: rowOf, maxDepth: maxDepth, maxRow: maxRow, roots: roots };
		}
		const G = computeGrid();

		let geo = null;
		let zoom = 1;

		/** 坐标（契约 §4.4）：x 按列、y 按行，触发器独占第 0 列。 */
		function computeGeo(TOP, narrow) {
			const nm = narrow ? SILK_CFG.narrow : SILK_CFG;
			const P = narrow ? SILK_CFG.narrow.PAD : SILK_CFG.PAD;
			const W = nm.W;
			const H = nm.H;
			const GX = nm.GX;
			const GY = nm.GY;
			const TRIGW = nm.TRIGW;
			for (const n of nodes) {
				if (n.id === ROOT_ID) continue;
				const d = G.depth.get(n.id) || 0;
				const r = G.row.get(n.id) || 0;
				n.depth = d;
				n.row = r;
				n.x = P + TRIGW + GX + d * (W + GX);
				n.y = TOP + r * (H + GY);
				n.w = W;
				n.h = H;
			}
			const rootRows = G.roots.map((id) => G.row.get(id) || 0);
			const trigRow = narrow
				? rootRows[0] || 0
				: rootRows.length
					? rootRows.reduce((a, b) => a + b, 0) / rootRows.length
					: 0;
			if (trigger) {
				trigger.depth = -1;
				trigger.row = trigRow;
				trigger.x = P;
				trigger.w = TRIGW;
				trigger.h = H;
				trigger.y = TOP + trigRow * (H + GY);
			}
			const contentW = P * 2 + TRIGW + GX + (G.maxDepth + 1) * W + G.maxDepth * GX;
			const contentH = TOP * 2 + (G.maxRow + 1) * H + G.maxRow * GY + (narrow ? 0 : 14);
			return {
				narrow: narrow,
				P: P,
				W: W,
				H: H,
				GX: GX,
				GY: GY,
				TRIGW: TRIGW,
				TOP: TOP,
				contentW: contentW,
				contentH: contentH,
			};
		}

		/** 碰撞检测：沿曲线采样，看是否落进**无关**节点矩形（内缩 1px，同 E2E）。 */
		function routeHitsNodes(route, fromId, toId) {
			for (let k = 1; k < 40; k++) {
				const p = silkCubicAt(route, k / 40);
				for (const n of nodes) {
					if (n.id === fromId || n.id === toId) continue;
					if (p.x > n.x + 1 && p.x < n.x + n.w - 1 && p.y > n.y + 1 && p.y < n.y + n.h - 1) {
						return true;
					}
				}
			}
			return false;
		}

		/**
		 * 路由选择：每条边按"候选顺序"取第一个**不穿过无关节点**的形状。
		 *
		 * 候选顺序 = 契约 §5 的三种路由（前向 / 行间带 / 顶部弧），第 3 种之前
		 * 插一条「同层跨列绕行」：同行、往前、中间隔着别的卡片时，§5.1 的 S 波
		 * 会横穿那些卡片（t9 的 analyze-pr→run-tests 与 report-send→alert 就是）。
		 * 顶部弧按需分道：只有真的走了顶部空隙的边才占一道，TOP 跟着道数长。
		 */
		function routeAll(g) {
			const routes = [];
			let lane = 0;
			for (const e of edgeItems) {
				const from = e.fromNode;
				const to = e.toNode;
				const rowDiff = Math.abs((to.row || 0) - (from.row || 0));
				const x1 = from.x + from.w;
				const y1 = from.y + from.h / 2;
				const x2 = to.x - 4;
				const y2 = to.y + to.h / 2;
				const forward = x2 - x1 >= 40 && rowDiff <= 1;
				const cands = [];
				if (e.loopback) {
					cands.push(silkTopArc(from, to, lane, g.narrow));
				} else {
					if (forward) cands.push(silkBezierRoute(x1, y1, x2, y2));
					if (rowDiff === 1) cands.push(silkBandRoute(from, to, g.GY));
					// 同层跨列：优先走顶部弧（lab 的形态），长横带只做
					// 顶部被占满时的 fallback；TOP 会随 lane 数增长。
					if (rowDiff === 0 && e.from !== ROOT_ID) {
						cands.push(silkTopArc(from, to, lane, g.narrow));
					}
					if (rowDiff === 0) cands.push(silkRowBandRoute(from, to, G.maxRow, g.GY));
					cands.push(silkTopArc(from, to, lane, g.narrow));
				}
				let chosen = cands[0];
				for (const c of cands) {
					if (c && !routeHitsNodes(c, e.from, e.to)) {
						chosen = c;
						break;
					}
				}
				if (chosen && chosen.kind === "top") lane += 1;
				routes.push(chosen);
			}
			return { routes: routes, lanes: lane };
		}

		let baseH = 480;

		function measureBaseHeight() {
			const prev = container.style.getPropertyValue("--silk-h");
			if (prev) container.style.removeProperty("--silk-h");
			const h = container.getBoundingClientRect().height || 480;
			if (prev) container.style.setProperty("--silk-h", prev);
			return h;
		}

		/**
		 * 舞台高度收敛（契约 §9）：先量旧的 clamp 值，再取 min(需要的高度, 旧值)。
		 * 图比舞台矮就不留白（silk 的舞台永远是"刚好装下这张图"），
		 * 比舞台高就保持原高度、由滚动条兜底。
		 */
		function updateStageHeight() {
			if (!geo) return;
			const need = geo.contentH + 39 + 35 + 2;
			// 顶部弧会让内容比旧舞台高；允许舞台跟着长到 560，
			// 否则 480 卡片里会出现 12px 纵向滚动条（E2E 纵向完整断言）。
			const want = Math.round(Math.min(need, Math.max(baseH, 560)));
			const cur = container.style.getPropertyValue("--silk-h");
			if (cur !== want + "px") container.style.setProperty("--silk-h", want + "px");
		}

		function applyZoom() {
			if (!geo) return;
			canvas.style.width = Math.round(geo.contentW * zoom) + "px";
			canvas.style.height = Math.round(geo.contentH * zoom) + "px";
			if (zoom === 1) {
				canvas.style.transform = "";
				canvas.style.transformOrigin = "";
			} else {
				canvas.style.transformOrigin = "0 0";
				canvas.style.transform = "scale(" + zoom + ")";
			}
		}

		function overlap2(a, b, pad) {
			const p = pad || 0;
			return a.l < b.r + p && a.r > b.l - p && a.t < b.b + p && a.b > b.t - p;
		}

		/** ×N 标签落点（契约 §5.5）：中点为先，按固定序列避让；全撞就跳过。 */
		function placeLabels() {
			const nodeRects = nodes.map((n) => ({ l: n.x, r: n.x + n.w, t: n.y, b: n.y + n.h }));
			const badgeRects = [];
			for (const e of edgeItems) {
				if (!e.badge) continue;
				const w = e.badge.offsetWidth || 44;
				const h = e.badge.offsetHeight || 18;
				badgeRects.push({
					l: e.badgeCx - w / 2,
					r: e.badgeCx + w / 2,
					t: e.badgeCy - h / 2,
					b: e.badgeCy + h / 2,
				});
			}
			const placed = [];
			const cands = [0.5, 0.58, 0.42, 0.66, 0.34, 0.74, 0.26, 0.82, 0.18];
			for (const e of edgeItems) {
				if (!e.label) continue;
				const path = e.core;
				const len = path.getTotalLength ? path.getTotalLength() : 0;
				const w = e.label.offsetWidth || 40;
				const h = e.label.offsetHeight || 16;
				let spot = null;
				for (const t of cands) {
					if (!len) break;
					const pt = path.getPointAtLength(len * t);
					const rc = { l: pt.x - w / 2, r: pt.x + w / 2, t: pt.y - h / 2, b: pt.y + h / 2 };
					if (nodeRects.some((r) => overlap2(rc, r, 2))) continue;
					if (placed.some((r) => overlap2(rc, r, 2))) continue;
					if (badgeRects.some((r) => overlap2(rc, r, 2))) continue;
					spot = { x: pt.x, y: pt.y, rc: rc };
					break;
				}
				if (!spot) {
					e.label.style.display = "none";
					continue;
				}
				e.label.style.display = "";
				e.label.style.left = spot.x + "px";
				e.label.style.top = spot.y + "px";
				placed.push(spot.rc);
			}
		}

		function placeBadges() {
			for (const e of edgeItems) {
				if (!e.badge || !e.route) continue;
				const r = e.route;
				const cx = (r.x1 + r.x2) / 2;
				const cy = r.kind === "top" ? Math.min(r.c1y, r.c2y) : Math.min(r.y1, r.y2) - 12;
				e.badgeCx = cx;
				e.badgeCy = cy;
				e.badge.style.left = cx + "px";
				e.badge.style.top = cy + "px";
			}
		}

		function applyGeometry() {
			const g = geo;
			svgRoot.setAttribute("width", String(g.contentW));
			svgRoot.setAttribute("height", String(g.contentH));
			svgRoot.setAttribute("viewBox", "0 0 " + g.contentW + " " + g.contentH);
			// 1:1 用户单位：viewBox 与 CSS px 尺寸必须一致，否则线会被二次缩放。
			svgRoot.style.width = g.contentW + "px";
			svgRoot.style.height = g.contentH + "px";
			applyZoom();
			for (const n of nodes) {
				const rec = nodeEls.get(n.id);
				if (!rec) continue;
				rec.el.style.left = n.x + "px";
				rec.el.style.top = n.y + "px";
				rec.el.style.width = n.w + "px";
				rec.el.style.height = n.h + "px";
			}
			for (const e of edgeItems) {
				const r = e.route;
				if (!r) continue;
				for (const p of e.layers) p.setAttribute("d", r.d);
				// 渐变跟着端点走（userSpaceOnUse）：重排/断点切换后颜色方向不能歪。
				e.grad.setAttribute("x1", String(r.x1));
				e.grad.setAttribute("y1", String(r.y1));
				e.grad.setAttribute("x2", String(r.x2));
				e.grad.setAttribute("y2", String(r.y2));
				if (e.motion) e.motion.setAttribute("path", r.d);
			}
			placeBadges();
			placeLabels();
			updateStageHeight();
		}

		function relayout() {
			const narrow = isNarrow();
			// 起手 TOP：有回边就先给 88（narrow 56），没有就 34（narrow 14）；
			// 路由完再按"真的占了几道顶部空隙"收敛，最多迭代 3 次。
			let TOP = edgeItems.some((e) => e.loopback)
				? narrow
					? 56
					: 88
				: narrow
					? 14
					: 34;
			let routes = null;
			for (let iter = 0; iter < 3; iter++) {
				geo = computeGeo(TOP, narrow);
				const res = routeAll(geo);
				routes = res.routes;
				const next =
					res.lanes === 0
						? narrow
							? 14
							: 34
						: narrow
							? 56 + (res.lanes - 1) * 16
							: 88 + (res.lanes - 1) * 20;
				if (next === TOP) break;
				TOP = next;
			}
			edgeItems.forEach((e, i) => {
				e.route = routes[i] || null;
			});
			applyGeometry();
		}

		function isNarrow() {
			let w = scroll.clientWidth || 0;
			if (!w) w = container.clientWidth || 0;
			if (!w) w = 910;
			return w < 620;
		}

		baseH = measureBaseHeight();
		relayout();

		// ---- HUD 初值 ------------------------------------------------------
		function setProgress(sec) {
			const p = totalSec > 0 ? Math.max(0, Math.min(1, sec / totalSec)) : 0;
			hudFill.style.width = (p * 100).toFixed(1) + "%";
			hudClock.textContent =
				"\u56de\u653e " + silkFmtClock(Math.min(sec, totalSec)) + " / " + silkFmtClock(totalSec);
		}
		const doneLabel = () =>
			beats.length + " \u6b65\u5b8c\u6210" + (failedCount ? " \u00b7 " + failedCount + " \u5931\u8d25" : "");
		setProgress(0);
		hudActive.textContent = tt("graph.silk_ready", "\u51c6\u5907\u56de\u653e");

		// ---- 相机（原生滚动，不用 transform） -----------------------------
		let followOff = false;
		function followTo(node) {
			if (followOff || !node || node.id === ROOT_ID) return;
			const max = Math.max(0, scroll.scrollWidth - scroll.clientWidth);
			const target = node.x + node.w / 2 - scroll.clientWidth / 2;
			scroll.scrollTo({
				left: Math.max(0, Math.min(max, target)),
				behavior: reduceNow() ? "auto" : "smooth",
			});
		}
		function resetCamera() {
			followOff = false;
			scroll.scrollLeft = 0;
		}

		function onScrollKey(ev) {
			const k = ev.key;
			if (k !== "ArrowRight" && k !== "ArrowLeft" && k !== "Home" && k !== "End") return;
			ev.preventDefault();
			followOff = true;
			const behavior = reduceNow() ? "auto" : "smooth";
			if (k === "Home") scroll.scrollTo({ left: 0, behavior: behavior });
			else if (k === "End") scroll.scrollTo({ left: scroll.scrollWidth, behavior: behavior });
			else scroll.scrollBy({ left: k === "ArrowRight" ? 140 : -140, behavior: behavior });
		}

		function onPointerDown(ev) {
			followOff = true;
			dragMoved = false;
			if (ev.pointerType !== "mouse") return;
			scroll.classList.add("is-dragging");
			const startX = ev.clientX;
			const startLeft = scroll.scrollLeft;
			const move = (e2) => {
				const dx = e2.clientX - startX;
				if (Math.abs(dx) > 3) dragMoved = true;
				scroll.scrollLeft = startLeft - dx;
			};
			const up = () => {
				scroll.classList.remove("is-dragging");
				scroll.removeEventListener("pointermove", move);
				scroll.removeEventListener("pointerup", up);
				scroll.removeEventListener("pointercancel", up);
			};
			if (scroll.setPointerCapture) {
				try {
					scroll.setPointerCapture(ev.pointerId);
				} catch (err) {
					/* 指针已经抬起 / 不支持的实现：忽略 */
				}
			}
			scroll.addEventListener("pointermove", move);
			scroll.addEventListener("pointerup", up);
			scroll.addEventListener("pointercancel", up);
			ev.preventDefault();
		}
		const onStopFollow = () => {
			followOff = true;
		};

		scroll.addEventListener("pointerdown", onPointerDown, { passive: false });
		scroll.addEventListener("wheel", onStopFollow, { passive: true });
		scroll.addEventListener("touchstart", onStopFollow, { passive: true });
		scroll.addEventListener("keydown", onScrollKey);

		// ---- 控件（契约 §9）：speed / replay / zoom / fit / fullscreen -----
		const canReplay = beats.length >= 2;
		const SPEEDS = [1, 0.5, 2];
		function paintSpeed(btn) {
			const txt = (revealSpeed === 1 ? "1" : String(revealSpeed)) + "\u00d7";
			btn.textContent = txt;
			const title = tt("graph.speed", "\u64ad\u653e\u901f\u5ea6") + " " + txt;
			btn.title = title;
			btn.setAttribute("aria-label", title);
		}
		if (canReplay) {
			const speedBtn = document.createElement("button");
			speedBtn.type = "button";
			speedBtn.dataset.fgAction = "speed";
			speedBtn.classList.add("fg-speed");
			paintSpeed(speedBtn);
			speedBtn.addEventListener("click", (ev) => {
				ev.stopPropagation();
				const i = SPEEDS.indexOf(revealSpeed);
				revealSpeed = SPEEDS[(i + 1) % SPEEDS.length] || 1;
				paintSpeed(speedBtn);
				playEnter(true);
			});
			ctrl.appendChild(speedBtn);
		}
		for (const [key, icon, title, fn] of [
			["replay", "replay", tt("graph.replay", "\u91cd\u64ad\u52a8\u753b"), () => playEnter(true)],
			["zoom-in", "zoom-in", tt("graph.zoom_in", "\u653e\u5927"), () => zoomBy(1.25)],
			["zoom-out", "zoom-out", tt("graph.zoom_out", "\u7f29\u5c0f"), () => zoomBy(1 / 1.25)],
			["fit", "fit", tt("graph.fit", "\u9002\u5e94\u89c6\u56fe"), () => fit()],
			["fullscreen", "expand", tt("graph.fullscreen", "\u5168\u5c4f"), () => toggleFullscreen()],
		]) {
			if (!canReplay && key === "replay") continue;
			if (key === "fullscreen" && !options.showFullscreen) continue;
			const b = document.createElement("button");
			b.type = "button";
			b.title = title;
			b.dataset.fgAction = key;
			b.setAttribute("aria-label", title);
			b.appendChild(ctrlIcon(icon));
			b.addEventListener("click", (ev) => {
				ev.stopPropagation();
				fn();
			});
			ctrl.appendChild(b);
		}

		function zoomBy(factor) {
			const next = Math.max(1, Math.min(3, zoom * factor));
			if (Math.abs(next - zoom) < 0.001) return;
			zoom = next;
			applyZoom();
		}
		function fit() {
			zoom = 1;
			applyZoom();
			resetCamera();
		}

		// ---- 伪全屏（与既有 FlowGraph 同一套做法） -------------------------
		const fsPlaceholder = document.createElement("div");
		fsPlaceholder.className = "flograph-placeholder";
		fsPlaceholder.hidden = true;
		let fullscreen = false;
		let fsRestore = null;

		function onFullscreenChange(v) {
			const b = ctrl.querySelector('[data-fg-action="fullscreen"]');
			if (b) {
				const title = v
					? tt("graph.exit_fullscreen", "\u9000\u51fa\u5168\u5c4f\uff08Esc\uff09")
					: tt("graph.fullscreen", "\u5168\u5c4f");
				b.title = title;
				b.setAttribute("aria-label", title);
				b.replaceChildren(ctrlIcon(v ? "collapse" : "expand"));
				b.classList.toggle("is-on", v);
			}
			fsHint.classList.toggle("is-on", v);
		}

		function setFullscreen(on) {
			if (on === fullscreen) return;
			fullscreen = on;
			if (on) {
				const box = container.getBoundingClientRect();
				fsRestore = {
					parent: container.parentNode,
					next: container.nextSibling,
					style: container.getAttribute("style"),
					w: Math.round(box.width),
					h: Math.round(box.height),
				};
				fsPlaceholder.style.width = fsRestore.w + "px";
				fsPlaceholder.style.height = fsRestore.h + "px";
				fsPlaceholder.hidden = false;
				if (container.parentNode) container.parentNode.insertBefore(fsPlaceholder, container);
				container.classList.add("is-fullscreen");
				document.body.appendChild(container);
				document.documentElement.classList.add("flograph-fullscreen-open");
			} else {
				document.documentElement.classList.remove("flograph-fullscreen-open");
				container.classList.remove("is-fullscreen");
				const r = fsRestore;
				if (r && r.parent) {
					if (r.next && r.next.parentNode === r.parent) r.parent.insertBefore(container, r.next);
					else r.parent.appendChild(container);
				}
				if (r && r.style == null) container.removeAttribute("style");
				else if (r) container.setAttribute("style", r.style);
				fsPlaceholder.remove();
				fsPlaceholder.hidden = true;
				fsRestore = null;
			}
			// 容器尺寸彻底变了：下一帧重排（含 narrow 断点与 --silk-h）
			requestAnimationFrame(() => {
				if (!fullscreen && !container.isConnected) return;
				baseH = measureBaseHeight();
				relayout();
				onFullscreenChange(fullscreen);
			});
		}
		function toggleFullscreen() {
			setFullscreen(!fullscreen);
		}
		function onFsKey(ev) {
			if (!fullscreen) return;
			if (ev.key === "Escape") {
				ev.preventDefault();
				setFullscreen(false);
			}
		}
		window.addEventListener("keydown", onFsKey);

		// ---- 尺寸变化：narrow 变了才重排（避免抖动），高度总是跟着重算 -----
		let ro = null;
		let rafResize = 0;
		function onResize() {
			if (rafResize) return;
			rafResize = requestAnimationFrame(() => {
				rafResize = 0;
				if (!container.isConnected) return;
				baseH = measureBaseHeight();
				if (isNarrow() !== geo.narrow) relayout();
				else updateStageHeight();
			});
		}
		if (typeof ResizeObserver === "function") {
			ro = new ResizeObserver(onResize);
			ro.observe(scroll);
		}
		window.addEventListener("resize", onResize);

		// ---- 动画 / 状态机（契约 §8） -------------------------------------
		let animated = false;
		let lastStepMs = 0;
		let ticker = null;
		const timers = new Set();

		function clearTimers() {
			for (const id of Array.from(timers)) clearTimeout(id);
			timers.clear();
			if (ticker) {
				clearInterval(ticker);
				ticker = null;
			}
		}
		function later(fn, ms) {
			const id = setTimeout(() => {
				timers.delete(id);
				fn();
			}, ms);
			timers.add(id);
			return id;
		}

		/**
		 * 主线画完后的**无缝循环**流光：shim 每次位移一个 dash 周期，
		 * bead 同步循环；终点相位与起点一致，不会跳到中间。
		 */
		function startFlow(e, len, drawMs) {
			if (!e.g || !e.core) return;
			// shim 的循环速度跟着拍长走（0.5× 更慢），但 loop 本身是**无缝**的：
			// @keyframes 每次正好位移一个 dash 周期 168px，终点相位 = 起点相位。
			const flowMs = Math.max(1600, Math.min(3600, Math.round((lastStepMs || 900) * 2.2)));
			e.g.style.setProperty("--silk-flow", (flowMs / 1000).toFixed(3) + "s");
			e.g.classList.add("is-flow");
			// 整条边只保留一个 block：dash=24、gap=整条路径长度。
			// offset 从 0 走到 -(len+24)，block 从源点进入、从终点离开，再回到源点。
			const shim = e.g.querySelector(".sl-shim");
			if (shim && len > 0) {
				if (e.shimAnim) e.shimAnim.cancel();
				const block = 24;
				shim.style.strokeDasharray = `${block} ${len + block}`;
				shim.style.strokeDashoffset = "0";
				e.shimAnim = shim.animate(
					[{ strokeDashoffset: "0" }, { strokeDashoffset: String(-(len + block)) }],
					{ duration: flowMs, iterations: Infinity, easing: "linear" },
				);
			}
			if (e.motion) {
				e.motion.setAttribute("dur", (flowMs / 1000).toFixed(3) + "s");
				if (e.motion.beginElement) e.motion.beginElement();
			}
		}

		function resetReveal() {
			clearTimers();
			container.classList.remove("is-playing");
			for (const rec of nodeEls.values()) {
				rec.el.classList.remove("is-pre", "is-hot", "is-pulse");
				if (!rec.participates) rec.el.classList.add("is-in");
			}
			for (const e of edgeItems) {
				e.g.classList.remove("is-on", "is-loop", "is-flow");
				if (e.core) {
					e.core.classList.remove("is-pre");
					if (e.core.getAnimations) e.core.getAnimations().forEach((a) => a.cancel());
					delete e.core.dataset.animated;
					e.core.style.strokeDasharray = "";
					e.core.style.strokeDashoffset = "";
					e.core.setAttribute("marker-end", e.marker);
				}
				for (const p of e.g.querySelectorAll(".sl-under, .sl-mid")) {
					if (p.getAnimations) p.getAnimations().forEach((a) => a.cancel());
					p.style.strokeDasharray = "";
					p.style.strokeDashoffset = "";
				}
				if (e.label) e.label.classList.add("is-pre");
				if (e.badge) e.badge.classList.remove("is-loop");
				if (e.bead) e.bead.style.opacity = "";
				if (e.shimAnim) {
					e.shimAnim.cancel();
					e.shimAnim = null;
				}
				const shim = e.g.querySelector(".sl-shim");
				if (shim) {
					shim.style.strokeDasharray = "";
					shim.style.strokeDashoffset = "";
				}
			}
			setProgress(0);
			hudActive.textContent = tt("graph.silk_ready", "\u51c6\u5907\u56de\u653e");
		}

		/**
		 * 把一条边"沿着路径画出来"。
		 *
		 * 演示页只有 core 有 dashoffset，under/mid 是跟着整组 opacity 淡入的；
		 * 丝带加粗后那样会像"整条线一起淡入"。这里 under/mid/core 三层都用实测
		 * 长度做 dasharray/dashoffset，从源点一起长出来，才是真正的"流向流出"。
		 *
		 * 仍用实测长度而不是 pathLength=1：E2E 的「逐段描出」断言要求
		 * computedDash > 1 且 0 < |offset| < dash-0.5。
		 */
		function drawEdge(e) {
			const path = e.core;
			if (!path) return;
			if (path.dataset.animated === "1") {
				if (e.label) e.label.classList.remove("is-pre");
				return;
			}
			path.classList.remove("is-pre");
			path.dataset.animated = "1";
			e.g.classList.add("is-on");

			const ribbon = [e.g.querySelector(".sl-under"), e.g.querySelector(".sl-mid")].filter(Boolean);

			const finish = () => {
				path.style.strokeDasharray = "";
				path.style.strokeDashoffset = "";
				path.setAttribute("marker-end", e.marker);
				for (const p of ribbon) {
					p.style.strokeDasharray = "";
					p.style.strokeDashoffset = "";
				}
				if (e.label) e.label.classList.remove("is-pre");
			};

			const len = path.getTotalLength ? path.getTotalLength() : 0;
			if (len > 0 && typeof path.animate === "function") {
				path.style.strokeDasharray = String(len);
				path.style.strokeDashoffset = String(len);
				for (const p of ribbon) {
					p.style.strokeDasharray = String(len);
					p.style.strokeDashoffset = String(len);
				}
				// 箭头等画完再出现，否则"线还没到、箭头先杵在那儿"
				path.removeAttribute("marker-end");
				// 描出时长跟着拍长走：0.5× 时不是"拍子慢了、线还是 400ms 唰一下"。
				const drawMs = Math.max(300, Math.min(1200, (lastStepMs || 900) * 0.7));
				const ribbonAnims = ribbon.map((p) =>
					p.animate(
						[{ strokeDashoffset: String(len) }, { strokeDashoffset: "0" }],
						{ duration: drawMs, easing: "cubic-bezier(0.35, 0, 0.25, 1)", fill: "forwards" },
					),
				);
				const anim = path.animate(
					[{ strokeDashoffset: String(len) }, { strokeDashoffset: "0" }],
					{ duration: drawMs, easing: "cubic-bezier(0.35, 0, 0.25, 1)", fill: "forwards" },
				);
				anim.onfinish = () => {
					anim.cancel();
					for (const a of ribbonAnims) a.cancel();
					finish();
					startFlow(e, len, drawMs);
				};
				return;
			}
			finish();
		}

		/** 点亮一个能力：第一次去 is-pre（+ is-in + is-hot）；再来一次就脉冲 480ms。 */
		function lightNode(id) {
			const rec = nodeEls.get(id);
			if (!rec) return;
			const el = rec.el;
			if (el.classList.contains("is-pre")) {
				el.classList.remove("is-pre");
				el.classList.add("is-in", "is-hot");
				later(() => el.classList.remove("is-hot"), 950);
				// 该节点是某条回边的 from → 弧线与 badge 一起亮
				for (const e of edgeItems) {
					if (!e.loopback || e.from !== id) continue;
					e.g.classList.add("is-loop");
					if (e.badge) e.badge.classList.add("is-loop");
				}
				return;
			}
			el.classList.remove("is-pulse");
			void el.offsetWidth; // 强制重排，让动画能重新开始
			el.classList.add("is-pulse");
			later(() => el.classList.remove("is-pulse"), 480);
		}

		/** 画"进入这一拍"的边：按 (from,to[,port]) 匹配，触发器那条按 to 匹配。 */
		function drawIncoming(b) {
			const from = b.from || "";
			for (const e of edgeItems) {
				if (!e.walked) continue;
				if (from) {
					if (e.from !== from || e.to !== b.cap) continue;
					if (b.port && e.port && e.port !== b.port) continue;
				} else if (e.from !== ROOT_ID || e.to !== b.cap) {
					continue;
				}
				drawEdge(e);
			}
		}

		function showFinal() {
			resetReveal();
			for (const rec of nodeEls.values()) rec.el.classList.add("is-in");
			for (const e of edgeItems) {
				if (!e.walked) continue;
				e.g.classList.add("is-on");
				const len = e.core.getTotalLength ? e.core.getTotalLength() : 0;
				if (len > 0) startFlow(e, len, 0);
				e.core.classList.remove("is-pre");
				e.core.dataset.animated = "1";
				e.core.setAttribute("marker-end", e.marker);
				if (e.label) e.label.classList.remove("is-pre");
				if (e.loopback) {
					e.g.classList.add("is-loop");
					if (e.badge) e.badge.classList.add("is-loop");
				}
			}
			setProgress(totalSec);
			hudActive.textContent = doneLabel();
		}

		function playEnter(force) {
			// 自动播放尊重系统"减少动态效果"；用户亲手点重播是明确意图，不该再挡。
			if (!force && reduceNow()) {
				showFinal();
				return;
			}
			if (beats.length < 2) {
				showFinal();
				return;
			}
			resetReveal();

			const speed = revealSpeed > 0 ? revealSpeed : 1;
			// 900ms/拍：12 步约 10.8s；0.5× = 1800ms/拍（约 24s），
			// 让"看清一步"优先于"快点放完"。
			const stepMs = Math.max(180, 900 / speed);
			lastStepMs = stepMs;

			// 只压暗**参与这次执行**的元素：剪枝节点与 muted 边本来就"没走"，
			// 再压暗一次会在动画结束时"慢慢浮出来"，看起来像最后又跑了一步。
			for (const rec of nodeEls.values()) {
				if (rec.participates) rec.el.classList.add("is-pre");
			}
			for (const e of edgeItems) {
				if (e.walked) e.core.classList.add("is-pre");
			}
			container.classList.add("is-playing");
			animated = true;

			// 绝对时刻表 + 40ms ticker：重播时一个 clearInterval 就收干净，
			// 不会留孤儿子回调（层层 setTimeout 的经典漏法）。
			const actions = [];
			const push = (at, run) => actions.push({ at: at, run: run });
			if (trigger) {
				push(0, () => {
					lightNode(ROOT_ID);
					hudActive.textContent = tt("graph.silk_source", "\u6e90") + " \u00b7 " + (trigger.label || ROOT_ID);
				});
			}
			beats.forEach((b, i) => {
				const at = stepMs * (i + 1);
				const lead = at - stepMs * 0.6;
				push(lead, () => {
					drawIncoming(b);
					const rec = nodeEls.get(b.cap);
					if (rec) followTo(rec.node);
				});
				push(at, () => {
					lightNode(b.cap);
					const rec = nodeEls.get(b.cap);
					const no = stepNoOf(b.cap);
					hudActive.textContent =
						(no ? String(no).padStart(2, "0") : "--") +
						" \u00b7 " +
						((rec && rec.node.label) || b.cap);
				});
			});

			const totalMs = stepMs * (beats.length + 1) + 520;
			const startedAt = Date.now();
			let cursor = 0;
			ticker = setInterval(() => {
				if (!container.isConnected) {
					clearTimers();
					return;
				}
				const elapsed = Date.now() - startedAt;
				while (cursor < actions.length && actions[cursor].at <= elapsed) {
					const a = actions[cursor++];
					a.run();
				}
				setProgress(Math.min(totalSec, (elapsed / stepMs) * 0.5));
				if (elapsed >= totalMs) {
					clearTimers();
					container.classList.remove("is-playing");
					setProgress(totalSec);
					hudActive.textContent = doneLabel();
				}
			}, 40);
		}

		if (options.animate) playEnter();
		else showFinal();

		return {
			fit: fit,
			zoomBy: zoomBy,
			setFullscreen: setFullscreen,
			isFullscreen: () => fullscreen,
			replay: () => playEnter(true),
			destroy() {
				clearTimers();
				if (rafResize) cancelAnimationFrame(rafResize);
				rafResize = 0;
				if (fullscreen) setFullscreen(false);
				window.removeEventListener("keydown", onFsKey);
				window.removeEventListener("resize", onResize);
				scroll.removeEventListener("pointerdown", onPointerDown);
				scroll.removeEventListener("wheel", onStopFollow);
				scroll.removeEventListener("touchstart", onStopFollow);
				scroll.removeEventListener("keydown", onScrollKey);
				if (ro) ro.disconnect();
				container.replaceChildren();
				container.classList.remove("flograph", "flograph--silk", "is-playing");
				container.style.removeProperty("--silk-h");
			},
			_debug: {
				nodes: () => nodes.map((n) => ({ id: n.id, label: n.label, kind: n.kind })),
				edges: () => edgeItems.map((e) => ({ from: e.from, to: e.to, port: e.port })),
				fullscreen: () => fullscreen,
				animated: () => animated,
				revealStep: () => lastStepMs,
				scale: () => zoom,
				contentSize: () => ({ w: geo ? geo.contentW : 0, h: geo ? geo.contentH : 0 }),
			},
		};
	}

	window.FlowGraph = { mount };
})();
