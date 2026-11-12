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
 * **数据源（v4）**：`GET /v1/namespaces/{namespace}/traces/{traceUID}` →
 * `{events, tasks, pruned}`。任务字段一律走 v4：
 *
 *   metadata.name    任务名（= 深链 `?id=`）
 *   status.phase     小写六态（pending/running/succeeded/failed/cancelled/abandoned）
 *   status.startedAt / finishedAt   RFC3339 字符串（本模块统一折算成 Unix 秒）
 *   status.attempt / nodeName / result.error
 *   spec.parentTaskId / spec.capability{name,version}
 *
 * v3 的九态、`out`、`capabilityId` 不再存在，这里也不再读。
 *
 * 依赖：web/graphs.js（AnotifyGraphs.taskChain / taskOf）+ 全局 el()/code()/t() 的约定。
 * 本模块不假设那些全局存在，都通过 opts 传入。
 */
(function () {
	"use strict";

	// v4 phase → CSS 类名后缀。**映射只有一份**（partials.js 的 Anotify.STATUS_CLASS）：
	// v4 的 `cancelled` 与 tokens/ui.css 里的历史拼写 `canceled` 只在那一个地方归一
	// （ST-5）。这里不拄第二份表：取不到映射时直接退回 raw phase（同名相位仍有色，
	// 只有 cancelled 掉回默认样式），而不是维护一份必然漂移的副表。
	function statusClass(st) {
		const A = window.Anotify;
		const G = window.AnotifyGraphs;
		const map =
			(A && A.STATUS_CLASS) || (G && G.STATUS_CLASS) || null;
		return (map && map[st]) || String(st || "pending");
	}

	// 状态显示名：**不写死中文**，文案走 `message.task_state.*`；
	// locale 里查不到就把状态码原样显示，不猜。
	function statusLabel(t, st) {
		const key = "message.task_state." + st;
		return t ? t(key, st) : st;
	}

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
	 * normalizeTask —— 把任务摊平成渲染用的行（v4 字段）。
	 *
	 * 走 `AnotifyGraphs.taskOf`（图库与这里共用同一套字段解析）：页面可能传
	 * trace 的 `tasks[]`（资源信封），也可能传已摊平的 `model.task()` 行。
	 * 时间统一折算成 Unix 秒 —— 瀑布图算宽度要数字，而 v4 wire 是 RFC3339。
	 */
	function normalizeTask(tk) {
		const G = window.AnotifyGraphs;
		if (G && typeof G.taskOf === "function") return G.taskOf(tk);
		// 兜底（图库脚本没加载）：只读 v4 字段，缺了就是缺了，不猜
		const r = tk || {};
		const meta = r.metadata || {};
		const spec = r.spec || {};
		const st = r.status || {};
		const cap = spec.capability || null;
		const toSec = (v) => {
			if (v == null) return null;
			if (typeof v === "number") return v;
			const ms = Date.parse(String(v));
			return Number.isFinite(ms) ? Math.floor(ms / 1000) : null;
		};
		const result = st.result || {};
		return {
			name: meta.name || null,
			phase: st.phase == null ? null : String(st.phase).toLowerCase(),
			attempt: st.attempt,
			parentTaskId: spec.parentTaskId || null,
			capability: cap ? { name: cap.name, version: cap.version } : null,
			nodeName: st.nodeName || null,
			startedSec: toSec(st.startedAt),
			finishedSec: toSec(st.finishedAt),
			sortAt: toSec(st.startedAt) != null ? toSec(st.startedAt) : toSec(st.finishedAt) || 0,
			errorReason: result.error ? result.error.reason : null,
			summary: result.summary || "",
		};
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
			byParent[k].sort((a, b) => (a.sortAt || 0) - (b.sortAt || 0));
		}

		const out = [];
		const roots = byParent["__root__"] || [];
		// 兜底：一条 trace 里找不到 root（可能只查到部分任务）时，
		// 把没有父的当 root，别让整张图空着。
		const start = roots.length
			? roots
			: tasks.filter(
					(t) => !t.parentTaskId || !tasks.some((x) => x.name === t.parentTaskId),
				);

		const seen = new Set();
		(function walk(list, depth) {
			for (const tk of list) {
				if (seen.has(tk.name)) continue; // 防御：数据异常时不成环
				seen.add(tk.name);
				out.push({ task: tk, depth });
				walk(byParent[tk.name] || [], depth + 1);
			}
		})(start, 0);

		// 孤儿也补上，宁可多画也不要丢
		for (const tk of tasks) {
			if (!seen.has(tk.name)) out.push({ task: tk, depth: 0 });
		}
		return out;
	}

	/** 「能力@版本」：v4 的 spec.capability 是钉住的版本，缺的用 ? 兜底。 */
	function capLabelOf(tk) {
		if (!tk.capability || !tk.capability.name) return "?";
		const v = tk.capability.version == null ? "?" : tk.capability.version;
		return tk.capability.name + "@" + v;
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
			const s = tk.startedSec != null ? tk.startedSec : tk.sortAt;
			const e =
				tk.finishedSec != null
					? tk.finishedSec
					: tk.startedSec != null
						? now
						: tk.sortAt;
			if (s != null && s < traceStart) traceStart = s;
			if (e != null && e > traceEnd) traceEnd = e;
		}
		if (!Number.isFinite(traceStart)) traceStart = now;
		if (!Number.isFinite(traceEnd)) traceEnd = traceStart;
		const total = Math.max(traceEnd - traceStart, 0);

		// 刻度单位：这里的 startedSec/finishedSec 全是 **Unix 秒**，
		// 而 fmtDur 收**毫秒**。直接把秒喂进去会差 1000 倍。这里统一乘 1000。
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
			const st = tk.phase || "pending";
			const row = el("div", "wf-row");

			// 左：缩进 + 状态点 + 能力名@版本
			const name = el("div", "wf-name");
			name.style.paddingLeft = sp.depth * 0.9 + "rem";
			name.appendChild(el("span", "st-dot st-" + statusClass(st)));
			name.appendChild(el("span", "cap", capLabelOf(tk)));
			if (tk.attempt > 1) name.appendChild(el("span", "meta", "×" + tk.attempt));
			name.title = [
				capLabelOf(tk),
				"phase: " + st,
				tk.nodeName ? "node: " + tk.nodeName : "",
				tk.errorReason ? "error: " + tk.errorReason : "",
				tk.summary ? "summary: " + tk.summary : "",
			]
				.filter(Boolean)
				.join("\n");
			row.appendChild(name);

			const track = el("div", "wf-track");
			const started = tk.startedSec;
			const finished = tk.finishedSec;

			if (started == null) {
				const leftPct = total > 0 ? ((tk.sortAt - traceStart) / total) * 100 : 0;
				const wait = el("div", "wf-wait");
				wait.style.left = Math.max(0, Math.min(100, leftPct)) + "%";
				wait.title = t("events.wf.not_started", "还没开始执行");
				track.appendChild(wait);
			} else {
				const end = finished != null ? finished : now;
				const leftPct = total > 0 ? ((started - traceStart) / total) * 100 : 0;
				const widthPct = total > 0 ? ((end - started) / total) * 100 : 100;
				const bar = el("div", "wf-bar b-" + statusClass(st));
				bar.style.left = Math.max(0, Math.min(100, leftPct)) + "%";
				bar.style.width = Math.max(0, Math.min(100 - leftPct, widthPct)) + "%";

				const dur = fmtDur((end - started) * 1000);
				const tip = [dur, st];
				if (finished == null) tip.push(t("events.wf.running", "进行中"));
				if (tk.errorReason) tip.push(tk.errorReason);
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
		const kids = byParent[tk.name] || [];
		const st = tk.phase || "pending";

		const box = el("div", "chain-node py-2");
		const line = el("div", "flex flex-wrap items-center gap-2");

		const name = el("span", "text-[13px] tx-name");
		name.appendChild(el("span", "st-dot st-" + statusClass(st)));
		name.appendChild(document.createTextNode(tk.capability && tk.capability.name ? tk.capability.name : "?"));
		line.appendChild(name);

		line.appendChild(
			code("v" + (tk.capability && tk.capability.version != null ? tk.capability.version : "?"), "text-[10px] tx-faint"),
		);
		line.appendChild(el("span", "text-[11px] tx-meta", statusLabel(t, st)));

		if (tk.startedSec != null && tk.finishedSec != null) {
			line.appendChild(
				el(
					"span",
					"text-[11px] tx-faint",
					"· " + (tk.finishedSec - tk.startedSec) + "s",
				),
			);
		}

		// 「到头了」只看一件事：**有没有下游任务**（v4 没有 out 列，也不该按它判）。
		// 有下游 → 什么都不显示（产出在「产出」区里，这里再提一句是噪音）。
		if (kids.length === 0) {
			line.appendChild(
				el("span", "text-[11px] tx-meta", "· " + t("events.chain.out_end", "到头了")),
			);
		}

		if (tk.errorReason) {
			line.appendChild(el("span", "text-[11px] tx-err", "· " + tk.errorReason));
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
	 * @param {Array}  opts.tasks      任务列表（trace 的 tasks[] 或 model.task 行）
	 * @param {Function} opts.el       el(tag, className, text)
	 * @param {Function} [opts.code]   code(text, className)，树视图用
	 * @param {Function} opts.t        i18n
	 * @param {string} [opts.view]     初始视图 waterfall|graph|tree
	 * @param {Function} [opts.onViewChange]
	 */
	function mount(container, opts) {
		const { el, t } = opts;
		// 兜底与 trace 页保持一致：默认「图」。两处不一致的话，
		// 任何忘记传 view 的调用方都会静默退回瀑布图，很难发现。
		let view = opts.view || "graph";
		// 摊平一次，三个视图共用（同一份字段解析，不各读各的）
		const tasks = (opts.tasks || []).map(normalizeTask);
		const ctx = Object.assign({}, opts, { tasks: tasks });

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
				container.appendChild(renderWaterfall(ctx, tasks));
			} else if (view === "graph") {
				const box = el("div", "");
				container.appendChild(box);
				// 必须先插入文档再挂图：FlowGraph 靠 offsetWidth 量节点尺寸，
				// 脱离文档量出来是 0，dagre 会把节点全排在原点。
				mountGraph(box, tasks, opts);
			} else {
				for (const r of roots) container.appendChild(renderTree(ctx, r, byParent));
				// 孤儿也要画（部分任务缺父时不能让它们消失）
				for (const tk of tasks) {
					if (tk.parentTaskId && !tasks.some((x) => x.name === tk.parentTaskId)) {
						container.appendChild(renderTree(ctx, tk, byParent));
					}
				}
			}
		}

		render();

		// 主题切换：状态点/文字色都是类名 + tokens，重渲染一次即可让
		// 图视图（内联色由 flograph 写入）也跟上新主题。
		document.addEventListener("anotify:theme", () => render());

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
			// 底图：这个 trace 所属源的静态配置拓扑（sourceTopology 的产物）。
			// 有它时图视图画"配置拓扑 + 执行染色"，被剪枝的分支留成灰色；
			// 没有时退回"只画执行链"。
			baseGraph: opts.baseGraph || null,
			// 源节点：图左上角那个 ⚡ —— 回答"这条 trace 谁触发的"。
			sourceLabel: opts.sourceLabel || "",
			sourceSub: opts.sourceSub || "",
			t: t,
			// 这一页没有标题栏的「全屏」入口（源详情页有），所以保留
			// 工具栏里的全屏按钮 —— 否则链路图就没法全屏看了。
			showFullscreen: true,
			onNodeClick: (n) => {
				const tk = tasks.find((x) => x.name === n.id);
				if (tk && tk.nodeName) {
					window.open("nodes.html?focus=" + encodeURIComponent(tk.nodeName), "_blank");
				}
			},
		});

		// 容器尺寸可能还没稳定 —— 下一帧再 fit 一次
		if (res && res.graph) requestAnimationFrame(() => res.graph.fit());
	}

	window.TraceView = {
		mount,
		buildSpans,
		renderWaterfall,
		renderTree,
		fmtDur,
		normalizeTask,
		/* 唯一一份映射在 partials.js；这里导出的只是同一张表的引用。 */
		STATUS_CLASS: (window.Anotify && window.Anotify.STATUS_CLASS) || {},
	};
})();
