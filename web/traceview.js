/**
 * TraceView —— 一条 trace 的渲染（瀑布图 / 图 / 树）
 *
 * 为什么单独成一个模块：
 *
 *   事件流页里"点开一条链"和独立的 trace 页要画的是**同一样东西**。
 *   这段代码原本长在 events.html 里（350+ 行），一旦 trace 页也要用，
 *   复制一份就是两份要同时维护的渲染逻辑——它们在"怎么算 span 时间"
 *   这种地方必须完全一致，否则同一个 trace 在两个页面显示不同。
 *
 * 三种视图各有强项，不是重复：
 *   瀑布图 waterfall → 时序（谁先谁后、各跑多久、哪里并行）
 *   图     graph     → 拓扑（谁派生自谁，扇出的形状）
 *   树     tree      → 紧凑（扫一眼层级，占地方最少）
 *
 * 数据全在 `/v1/tasks?traceId=` 的响应里，不需要新接口：
 *   createdAt    trace 的起点候选
 *   startedAt    span 起点（节点 ack 之后才填）
 *   finishedAt   span 终点（未完成的任务为 null）
 *   parentTaskId 层级
 *   status       九态着色
 *
 * 依赖：web/graphs.js（AnotifyGraphs.taskChain）+ 全局 el()/code()/t() 的约定。
 * 本模块不假设那些全局存在，都通过 opts 传入。
 */
(function () {
	"use strict";

	// 九态 → CSS 类后缀。与 ui.css 的 .st-* / .b-* 对应。
	const STATUS_CLASS = {
		pending: "pending",
		assigned: "assigned",
		acked: "acked",
		running: "running",
		succeeded: "succeeded",
		failed: "failed",
		canceled: "canceled",
		abandoned: "abandoned",
		lease_expired: "lease",
	};

	const STATUS_LABEL = {
		pending: "排队中",
		assigned: "已分配",
		acked: "已接单",
		running: "执行中",
		succeeded: "成功",
		failed: "失败",
		canceled: "已取消",
		abandoned: "已放弃",
		lease_expired: "租约过期",
	};

	/** 毫秒 → "1.2s" / "340ms" / "1m12s" */
	function fmtDur(ms) {
		if (!Number.isFinite(ms) || ms < 0) return "";
		if (ms < 1000) return Math.round(ms) + "ms";
		if (ms < 60000) return (ms / 1000).toFixed(ms < 10000 ? 2 : 1) + "s";
		const m = Math.floor(ms / 60000);
		const s = Math.round((ms % 60000) / 1000);
		return m + "m" + (s ? s + "s" : "");
	}

	/**
	 * buildSpans 把任务列表整理成带时间与层级的 span 数组。
	 *
	 * 顺序按 DFS（父在前、子紧随）：瀑布图从上到下就是调用顺序。
	 * 按时间戳排会把父子关系打散 —— 尤其扇出时兄弟分支交错，
	 * 看不出谁派生自谁。
	 */
	function buildSpans(tasks) {
		const byParent = {};
		for (const tk of tasks) {
			const p = tk.parentTaskId || "__root__";
			(byParent[p] = byParent[p] || []).push(tk);
		}
		for (const k of Object.keys(byParent)) {
			byParent[k].sort((a, b) => (a.createdAt || 0) - (b.createdAt || 0));
		}

		const out = [];
		const roots = byParent["__root__"] || [];
		// 兜底：一条 trace 里找不到 root（可能只查到部分任务）时，
		// 把没有父的当 root，别让整张图空着。
		const start = roots.length
			? roots
			: tasks.filter(
					(t) => !t.parentTaskId || !tasks.some((x) => x.id === t.parentTaskId),
				);

		const seen = new Set();
		(function walk(list, depth) {
			for (const tk of list) {
				if (seen.has(tk.id)) continue; // 防御：数据异常时不成环
				seen.add(tk.id);
				out.push({ task: tk, depth });
				walk(byParent[tk.id] || [], depth + 1);
			}
		})(start, 0);

		// 孤儿也补上，宁可多画也不要丢
		for (const tk of tasks) {
			if (!seen.has(tk.id)) out.push({ task: tk, depth: 0 });
		}
		return out;
	}

	/**
	 * renderWaterfall 画瀑布图。
	 *
	 * 必须处理好的边界：
	 *   ① 单任务 trace（只有一层）—— 层级少也不能画歪
	 *   ② 未开始（startedAt 为空）—— 左边界细线，不当成 0 宽度
	 *   ③ 进行中（finishedAt 为空）—— 宽度算到"现在"，脉动
	 *   ④ 零耗时（started==finished）—— min-width 保证看得见
	 *   ⑤ 所有时间戳相同 —— total 为 0，除零要防
	 */
	function renderWaterfall(ctx, tasks) {
		const { el, t } = ctx;
		const spans = buildSpans(tasks);
		if (!spans.length) {
			return el("div", "wf-empty", t("events.chain.empty", "这条 trace 下没有任务。"));
		}

		const now = Math.floor(Date.now() / 1000);

		let traceStart = Infinity;
		let traceEnd = -Infinity;
		for (const sp of spans) {
			const tk = sp.task;
			const s = tk.startedAt != null ? tk.startedAt : tk.createdAt;
			const e =
				tk.finishedAt != null ? tk.finishedAt : tk.startedAt != null ? now : tk.createdAt;
			if (s != null && s < traceStart) traceStart = s;
			if (e != null && e > traceEnd) traceEnd = e;
		}
		if (!Number.isFinite(traceStart)) traceStart = now;
		if (!Number.isFinite(traceEnd)) traceEnd = traceStart;
		const total = Math.max(traceEnd - traceStart, 0);

		// 刻度单位：createdAt/startedAt/finishedAt 全是 **Unix 秒**，
		// 而 fmtDur 收**毫秒**。直接把秒喂进去会差 1000 倍
		// （2 秒的跨度显示成 "2ms"）。这里统一乘 1000。
		const TICK_N = 4;
		const ticks = [];
		for (let i = 0; i <= TICK_N; i++) {
			ticks.push(total > 0 ? ((total * i) / TICK_N) * 1000 : 0);
		}

		const box = el("div", "wf");

		// 顶部刻度尺
		const axis = el("div", "wf-axis");
		axis.appendChild(el("div", "wf-axis-name", ""));
		const axisTrack = el("div", "wf-axis-track");
		ticks.forEach((ms, i) => {
			const tick = el("span", "wf-tick", total > 0 ? fmtDur(ms) : "0ms");
			tick.style.left = (i / TICK_N) * 100 + "%";
			if (i === 0) tick.style.transform = "translateX(0)";
			if (i === TICK_N) tick.style.transform = "translateX(-100%)";
			axisTrack.appendChild(tick);
		});
		axis.appendChild(axisTrack);
		box.appendChild(axis);

		for (const sp of spans) {
			const tk = sp.task;
			const st = tk.status || "pending";
			const row = el("div", "wf-row");

			// 左：缩进 + 状态点 + 能力名@版本
			const name = el("div", "wf-name");
			name.style.paddingLeft = sp.depth * 0.9 + "rem";
			name.appendChild(el("span", "st-dot st-" + (STATUS_CLASS[st] || "pending")));
			name.appendChild(
				el(
					"span",
					"cap",
					(tk.capability || "?") + "@" + (tk.version != null ? tk.version : "?"),
				),
			);
			if (tk.attempts > 1) name.appendChild(el("span", "meta", "×" + tk.attempts));
			name.title = [
				tk.capability + "@" + tk.version,
				"status: " + st,
				tk.nodeId ? "node: " + tk.nodeId : "",
				tk.error ? "error: " + tk.error : "",
				tk.summary ? "summary: " + tk.summary : "",
			]
				.filter(Boolean)
				.join("\n");
			row.appendChild(name);

			const track = el("div", "wf-track");
			const started = tk.startedAt;
			const finished = tk.finishedAt;

			if (started == null) {
				const leftPct = total > 0 ? ((tk.createdAt - traceStart) / total) * 100 : 0;
				const wait = el("div", "wf-wait");
				wait.style.left = Math.max(0, Math.min(100, leftPct)) + "%";
				wait.title = t("events.wf.not_started", "还没开始执行");
				track.appendChild(wait);
			} else {
				const end = finished != null ? finished : now;
				const leftPct = total > 0 ? ((started - traceStart) / total) * 100 : 0;
				const widthPct = total > 0 ? ((end - started) / total) * 100 : 100;
				const bar = el("div", "wf-bar b-" + (STATUS_CLASS[st] || "pending"));
				bar.style.left = Math.max(0, Math.min(100, leftPct)) + "%";
				bar.style.width = Math.max(0, Math.min(100 - leftPct, widthPct)) + "%";

				const dur = fmtDur((end - started) * 1000);
				const tip = [dur, st];
				if (finished == null) tip.push(t("events.wf.running", "进行中"));
				if (tk.error) tip.push(tk.error);
				bar.title = tip.join(" · ");
				track.appendChild(bar);

				// 时长标签：只给根任务和够宽的条，否则糊成一团
				if (sp.depth === 0 || widthPct > 8) {
					const lab = el("div", "wf-dur", dur + (finished == null ? " ⏳" : ""));
					const labLeft = leftPct + Math.min(widthPct, 100 - leftPct) + 0.6;
					lab.style.left = Math.min(98, labLeft) + "%";
					track.appendChild(lab);
				}
			}

			row.appendChild(track);
			box.appendChild(row);
		}

		return box;
	}

	/** renderTree 递归渲染一个任务及其子任务（紧凑视图）。 */
	function renderTree(ctx, tk, byParent) {
		const { el, code, t } = ctx;
		const kids = byParent[tk.id] || [];
		const st = tk.status || "pending";

		const box = el("div", "chain-node py-2");
		const line = el("div", "flex flex-wrap items-center gap-2");

		const name = el("span", "text-[13px] text-zinc-200");
		name.appendChild(el("span", "st-dot st-" + (STATUS_CLASS[st] || "pending")));
		name.appendChild(document.createTextNode(tk.capability || "?"));
		line.appendChild(name);

		line.appendChild(code("v" + tk.version, "text-[10px] text-zinc-600"));
		line.appendChild(el("span", "text-[11px] text-zinc-500", STATUS_LABEL[st] || st));

		if (tk.startedAt && tk.finishedAt) {
			line.appendChild(
				el("span", "text-[11px] text-zinc-600", "· " + (tk.finishedAt - tk.startedAt) + "s"),
			);
		}

		// out 三态（不变量 I5）：未回报 / 到头了 / 走某个出口
		const outWrap = el("span", "text-[11px] text-zinc-500");
		outWrap.appendChild(document.createTextNode("· "));
		if (!tk.reported) {
			outWrap.appendChild(document.createTextNode(t("events.chain.out_none", "未回报")));
		} else if (tk.out == null) {
			outWrap.appendChild(document.createTextNode(t("events.chain.out_end", "到头了")));
		} else {
			outWrap.appendChild(document.createTextNode("→ "));
			outWrap.appendChild(code(tk.out, "text-emerald-300"));
		}
		line.appendChild(outWrap);

		if (tk.error) {
			line.appendChild(el("span", "text-[11px] text-red-400", "· " + tk.error));
		}
		box.appendChild(line);

		if (kids.length) {
			const childBox = el("div", "mt-1");
			for (const k of kids) childBox.appendChild(renderTree(ctx, k, byParent));
			box.appendChild(childBox);
		}
		return box;
	}

	/**
	 * TraceView.mount —— 把一个 trace 的三种视图挂到容器里，带切换器。
	 *
	 * @param {HTMLElement} container  已挂载到文档的容器
	 * @param {Object} opts
	 * @param {Array}  opts.tasks      任务列表
	 * @param {Function} opts.el       el(tag, className, text)
	 * @param {Function} [opts.code]   code(text, className)，树视图用
	 * @param {Function} opts.t        i18n
	 * @param {string} [opts.view]     初始视图 waterfall|graph|tree
	 * @param {Function} [opts.onViewChange]
	 */
	function mount(container, opts) {
		const { el, t } = opts;
		let view = opts.view || "waterfall";
		const tasks = opts.tasks || [];

		const byParent = {};
		for (const tk of tasks) {
			const p = tk.parentTaskId || "__root__";
			(byParent[p] = byParent[p] || []).push(tk);
		}
		const roots = byParent["__root__"] || [];

		function render() {
			container.replaceChildren();

			const sw = el("div", "wf-switch mb-3");
			for (const [key, label] of [
				["waterfall", t("events.wf.waterfall", "瀑布图")],
				["graph", t("events.wf.graph", "图")],
				["tree", t("events.wf.tree", "树")],
			]) {
				const b = el("button", view === key ? "on" : "", label);
				b.addEventListener("click", () => {
					view = key;
					if (opts.onViewChange) opts.onViewChange(view);
					render();
				});
				sw.appendChild(b);
			}
			container.appendChild(sw);

			if (view === "waterfall") {
				container.appendChild(renderWaterfall(opts, tasks));
			} else if (view === "graph") {
				const box = el("div", "");
				container.appendChild(box);
				// 必须先插入文档再挂图：FlowGraph 靠 offsetWidth 量节点尺寸，
				// 脱离文档量出来是 0，dagre 会把节点全排在原点。
				mountGraph(box, tasks, opts);
			} else {
				for (const r of roots) container.appendChild(renderTree(opts, r, byParent));
			}
		}

		render();
		return {
			setView(v) {
				view = v;
				render();
			},
			getView: () => view,
		};
	}

	/** 图视图：交给共享的 AnotifyGraphs.taskChain（与源流向图同一套渲染）。 */
	function mountGraph(container, tasks, opts) {
		const { el, t } = opts;
		container.replaceChildren();

		if (!window.AnotifyGraphs) {
			container.appendChild(
				el("div", "wf-empty", t("events.wf.nolib", "图渲染脚本未能加载。")),
			);
			return;
		}

		const host = el("div", "");
		container.appendChild(host);

		const res = window.AnotifyGraphs.taskChain(host, {
			tasks: tasks,
			// 底图：这个 trace 所属源的静态配置拓扑。
			// 有它时图视图画"配置拓扑 + 执行染色"，被剪枝的分支留成灰色；
			// 没有时退回"只画执行链"。见 graphs.js 里 taskChain 的注释。
			baseGraph: opts.baseGraph || null,
			// 源节点：图左上角那个 ⚡ —— 回答"这条 trace 谁触发的"。
			// 没有它，图只是一堆能力怎么连，看不出起点。
			sourceLabel: opts.sourceLabel || "",
			sourceSub: opts.sourceSub || "",
			sourcePort: opts.sourcePort || "",
			t: t,
			// 这一页没有标题栏的「全屏」入口（源详情页有），所以保留
			// 工具栏里的全屏按钮 —— 否则链路图就没法全屏看了。
			showFullscreen: true,
			onNodeClick: (n) => {
				const tk = tasks.find((x) => x.id === n.id);
				if (tk && tk.nodeId) {
					window.open("nodes.html?focus=" + encodeURIComponent(tk.nodeId), "_blank");
				}
			},
		});

		// 容器尺寸可能还没稳定 —— 下一帧再 fit 一次
		if (res && res.graph) requestAnimationFrame(() => res.graph.fit());
	}

	window.TraceView = { mount, buildSpans, renderWaterfall, renderTree, fmtDur, STATUS_CLASS };
})();
