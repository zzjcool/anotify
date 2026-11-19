#!/usr/bin/env node
/* Anotify · 管理后台「全局消息」流单测（#456）
 *
 * 拦的事故：`web-src/pages/admin.html` 的「全局消息」表按**旧通知实体**读
 * `m.title` / `m.username` / `m.agentState`，而这个端点的数据源早已是 events
 * （`GET /v1/admin/messages` → `api/auth.yaml · AdminEvent`）。三个名字服务端一个都不发，
 * 于是整表空白 + 第三列一枚 `undefined` 徽章 —— 而静态检查当时全绿（没人读 JSON 就知道
 * 字段对不上），e2e 也没盯过这三列。
 *
 * 判据（三层，缺一层就等于没查）：
 *   ① **行为**：把 web/partials.js 真的读进 vm 跑起来，喂一条**真实形状**的事件行，
 *      断言页面用的那三列（事件 / 来源 / 命名空间）都拿到值，时间列拿到 Unix 秒；
 *   ② **变异自证**：把 model 里的字段名改回旧名（type→title、source→username、
 *      namespaceId→agentState）重新加载，① 的断言必须**变红** —— 否则「绿」可能只是
 *      断言写松了，而不是代码对了；
 *   ③ **接线**：页面 `loadMessages()` 真的走 `Anotify.model.adminEvent()`，表头引用的是
 *      真实字段对应的词条，空列表走 `showEmpty`（明确空态，不是一张只有表头的空白表）。
 *      只做 ① 不做 ③，页面可以继续读旧名字而单测照样绿。
 *
 * 跑法：`node --test scripts/admin-messages.test.mjs`
 * 接线：`make test-frontend`（`make ci` 内跑，合并前的检查里红/绿）
 */

import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import vm from "node:vm";
import test from "node:test";
import assert from "node:assert/strict";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const PARTIALS_PATH = "web/partials.js";
const PAGE_PATH = "web-src/pages/admin.html";
const PARTIALS_SRC = readFileSync(path.join(ROOT, PARTIALS_PATH), "utf8");
const PAGE_SRC = readFileSync(path.join(ROOT, PAGE_PATH), "utf8");

/* ------------------------------------------------------------------ *
 * 真实形状的事件行
 *
 * 形状照 `api/auth.yaml · AdminEvent`（required 七个字段全在）+ `internal/store/admin.go ·
 * AdminGlobalEvent` 的 json tag；取值照 `internal/server/admin_test.go` 里真写进库的那条
 * （github / pr_opened / trace_… / deliveryId）。`createdAt` 是 **Unix 秒**（int64）。
 * 故意不放 title / username / agentState：服务端从不发它们，放进来就等于替 bug 打掩护。
 * ------------------------------------------------------------------ */
const REAL_EVENT = {
	id: "evt_01J8ZQ4W6T9K3M5N7P1R2S4V6X",
	namespaceId: "ns_01J8ZQ4W6T9K3M5N7P1R2S4V6X",
	source: "github",
	type: "pr_opened",
	traceId: "trace_admin_test",
	deliveryId: "gh-delivery-88213",
	createdAt: 1763467200,
};

/* ------------------------------------------------------------------ *
 * 最小浏览器沙箱（只够加载 web/partials.js 并调用 model.*）
 *
 * partials.js 是纯静态 IIFE（无模块系统），加载期只碰 document/window/location/
 * localStorage 这几处；这里把它们 stub 掉，然后**真实执行**源码 —— 断言的是一长串行为，
 * 不是"字符串出现在源码里"。
 * ------------------------------------------------------------------ */

function fakeNode(tag = "div") {
	const attrs = new Map();
	const children = [];
	const node = {
		nodeType: 1,
		tagName: String(tag).toUpperCase(),
		className: "",
		textContent: "",
		dataset: {},
		style: {
			setProperty() {},
			removeProperty() {},
			getPropertyValue: () => "",
		},
		classList: { add() {}, remove() {}, toggle() {}, contains: () => false },
		children,
		append(...items) {
			for (const item of items) if (item != null) children.push(item);
			return node;
		},
		appendChild(child) {
			children.push(child);
			return child;
		},
		prepend(...items) {
			children.unshift(...items);
			return node;
		},
		replaceChildren(...items) {
			children.length = 0;
			for (const item of items) if (item != null) children.push(item);
		},
		remove() {},
		setAttribute(k, v) {
			attrs.set(k, String(v));
		},
		getAttribute: (k) => (attrs.has(k) ? attrs.get(k) : null),
		hasAttribute: (k) => attrs.has(k),
		removeAttribute(k) {
			attrs.delete(k);
		},
		addEventListener() {},
		removeEventListener() {},
		dispatchEvent: () => true,
		querySelector: () => null,
		querySelectorAll: () => [],
		contains: () => false,
		closest: () => null,
		focus() {},
		blur() {},
		click() {},
		getBoundingClientRect: () => ({
			top: 0,
			left: 0,
			right: 0,
			bottom: 0,
			width: 0,
			height: 0,
		}),
		get firstChild() {
			return children[0] || null;
		},
	};
	return node;
}

function fakeDocument() {
	const doc = {
		documentElement: fakeNode("html"),
		head: fakeNode("head"),
		body: fakeNode("body"),
		cookie: "",
		createElement: (tag) => fakeNode(tag),
		createElementNS: (_ns, tag) => fakeNode(tag),
		createTextNode: (text) => ({ nodeType: 3, textContent: String(text) }),
		getElementById: () => null,
		querySelector: () => null,
		querySelectorAll: () => [],
		addEventListener() {},
		removeEventListener() {},
		dispatchEvent: () => true,
		fonts: { ready: Promise.resolve() },
	};
	doc.documentElement.lang = "zh-CN";
	return doc;
}

function makeSandbox() {
	const store = new Map();
	return {
		console,
		setTimeout,
		clearTimeout,
		setInterval,
		clearInterval,
		requestAnimationFrame: (fn) => setTimeout(fn, 0),
		cancelAnimationFrame: (id) => clearTimeout(id),
		fetch: async () => {
			throw new Error("单测不该发请求：这里只验证 model 层的字段映射");
		},
		localStorage: {
			getItem: (k) => (store.has(k) ? store.get(k) : null),
			setItem: (k, v) => {
				store.set(k, String(v));
			},
			removeItem: (k) => {
				store.delete(k);
			},
			clear: () => store.clear(),
		},
		location: { pathname: "/admin.html", search: "", hash: "" },
		navigator: { languages: ["zh-CN"], language: "zh-CN" },
		matchMedia: () => ({
			matches: false,
			addEventListener() {},
			removeEventListener() {},
		}),
		document: fakeDocument(),
		/* partials.js 在加载期就会 applyTheme() → 发一条主题事件；vm 里没有 CustomEvent */
		CustomEvent: class {
			constructor(type, init) {
				this.type = type;
				Object.assign(this, init || {});
			}
		},
		/* vm 里 globalThis 就是页面的 window：partials.js 会在它上面挂 window 级监听 */
		addEventListener() {},
		removeEventListener() {},
		dispatchEvent: () => true,
		getComputedStyle: () => ({ getPropertyValue: () => "" }),
	};
}

/** 在独立 vm context 里加载一份 partials.js（默认是仓库里的那一份），返回 window.Anotify。 */
function loadAnotify(src = PARTIALS_SRC) {
	const ctx = vm.createContext(makeSandbox());
	vm.runInContext("globalThis.window = globalThis; globalThis.self = globalThis;", ctx);
	vm.runInContext(src, ctx, { filename: PARTIALS_PATH });
	return ctx.Anotify;
}

/* ------------------------------------------------------------------ *
 * 被测行为：页面的三列 + 时间列
 * ------------------------------------------------------------------ */

/** model 层给出的一行 → 页面实际画进表格的四列（前三列是数据列，第四列是时间）。 */
function columnsOf(Anotify, row) {
	const e = Anotify.model.adminEvent(row);
	return {
		type: e.type,
		source: e.source,
		namespace: e.namespaceId,
		createdAt: e.createdAt,
	};
}

/** 三列都必须有**真实值**：空串与占位符 `—` 都算没渲染出来。 */
function assertThreeColumnsHaveValues(Anotify, row) {
	const cols = columnsOf(Anotify, row);
	assert.equal(cols.type, row.type, "事件列必须是事件的 type");
	assert.equal(cols.source, row.source, "来源列必须是事件的 source");
	assert.equal(cols.namespace, row.namespaceId, "命名空间列必须是事件的 namespaceId");
	assert.equal(cols.createdAt, row.createdAt, "时间列必须是事件的 createdAt（Unix 秒）");
	for (const col of ["type", "source", "namespace"]) {
		assert.notEqual(cols[col], "", `${col} 列渲染成了空串（= 整表空白）`);
		assert.notEqual(cols[col], "—", `${col} 列只剩占位符（= 读了一个服务端不发的字段）`);
	}
	return cols;
}

test("真实形状的事件 → 事件/来源/命名空间三列都有值，时间列是 Unix 秒", () => {
	const A = loadAnotify();
	const cols = assertThreeColumnsHaveValues(A, REAL_EVENT);
	assert.deepEqual(
		cols,
		{
			type: "pr_opened",
			source: "github",
			namespace: "ns_01J8ZQ4W6T9K3M5N7P1R2S4V6X",
			createdAt: 1763467200,
		},
		"四列必须逐字等于事件行的字段值",
	);
});

test("契约里的其余字段不被模型层丢掉（traceId / deliveryId / id）", () => {
	const A = loadAnotify();
	const e = A.model.adminEvent(REAL_EVENT);
	assert.equal(e.id, REAL_EVENT.id);
	assert.equal(e.traceId, "trace_admin_test");
	assert.equal(e.deliveryId, "gh-delivery-88213");
	/* deliveryId 可空串（人工/内部事件没有投递键）：空串是事实，不能被折成 "—" 或 null */
	assert.equal(A.model.adminEvent({ ...REAL_EVENT, deliveryId: "" }).deliveryId, "");
	/* createdAt 缺失/非数字 → null（页面画 "—"），不编时间戳 */
	assert.equal(A.model.adminEvent({ ...REAL_EVENT, createdAt: undefined }).createdAt, null);
	assert.equal(A.model.adminEvent({ ...REAL_EVENT, createdAt: "1763467200" }).createdAt, null);
});

test("变异自证：字段名改回旧名（title/username/agentState）→ 三列断言必红", () => {
	/* 旧代码就是这三个名字：整表空白 + `undefined` 徽章。把模型层改回它们，
	   ① 的断言必须变红 —— 否则说明断言太松，红灯只是运气。 */
	const renames = [
		[/r\.type\b/g, "r.title"],
		[/r\.source\b/g, "r.username"],
		[/r\.namespaceId\b/g, "r.agentState"],
	];
	let mutated = PARTIALS_SRC;
	for (const [re, to] of renames) {
		const before = mutated;
		mutated = mutated.replace(re, to);
		assert.notEqual(mutated, before, `变异没落地：源码里找不到 ${re}（字段名改了就要同步这份自证）`);
	}
	assert.notEqual(mutated, PARTIALS_SRC, "变异后的源码必须与仓库里的一致之外的东西不同");

	const Mutant = loadAnotify(mutated);
	assert.throws(
		() => assertThreeColumnsHaveValues(Mutant, REAL_EVENT),
		/事件列必须是事件的 type|来源列必须是事件的 source|命名空间列必须是事件的 namespaceId/,
		"字段名改回旧名后三列断言仍是绿的 —— 这个单测保护不了 #456",
	);
});

test("接线：页面 loadMessages() 走 model.adminEvent()，不再读旧通知字段", () => {
	/* ③ 为什么也要查：只测 model 层的话，页面可以继续 `m.title` 而单测全绿。
	   这里从页面源码里切出 loadMessages() 这一整段（到下一个分节注释为止），
	   断言它真的调用了被测的映射函数、并且不再出现旧字段名。 */
	const start = PAGE_SRC.indexOf("async function loadMessages()");
	assert.notEqual(start, -1, `页面里找不到 loadMessages()：${PAGE_PATH} 结构变了，修这份断言`);
	const end = PAGE_SRC.indexOf("/* ================= 活跃会话", start);
	assert.notEqual(end, -1, `页面里找不到 loadMessages() 的结束锚点：${PAGE_PATH} 结构变了，修这份断言`);
	const fn = PAGE_SRC.slice(start, end);

	assert.match(fn, /Anotify\.model\.adminEvent\(/, "loadMessages() 必须用 model.adminEvent() 读字段");
	for (const old of ["m.title", "m.username", "m.agentState", ".username", ".agentState"]) {
		assert.ok(!fn.includes(old), `loadMessages() 仍在读旧通知字段 ${old}（服务端不发 → 空白/undefined）`);
	}
	/* 空列表必须是**明确空态**（.empty 区块 + 重试按钮），不是一张只有表头的空白表 */
	assert.match(fn, /showEmpty\(st\)/, "空列表必须走 showEmpty（明确空态）");
	const emptyBlock = PAGE_SRC.slice(
		PAGE_SRC.indexOf('id="messages-empty"') - 200,
		PAGE_SRC.indexOf('id="messages-empty"') + 800,
	);
	assert.match(emptyBlock, /admin\.messages\.empty/, "空态要有标题词条");
	assert.match(emptyBlock, /messages-retry/, "空态要有可点重试");
});

test("接线：表头三列引用的是真实字段对应的词条（admin.messages.type/source/namespace）", () => {
	const thead = PAGE_SRC.slice(
		PAGE_SRC.indexOf('id="messages-table"'),
		PAGE_SRC.indexOf("</thead>", PAGE_SRC.indexOf('id="messages-table"')),
	);
	assert.notEqual(thead.length, 0, `页面里找不到 #messages-table 的表头：${PAGE_PATH} 结构变了`);
	for (const key of ["admin.messages.type", "admin.messages.source", "admin.messages.namespace", "admin.messages.time"]) {
		assert.ok(thead.includes(`{{t "${key}"}}`), `表头缺少词条 ${key}`);
	}
	for (const stale of ["admin.messages.title", "admin.messages.user", "admin.messages.agent_state"]) {
		assert.ok(!thead.includes(stale), `表头还在用旧词条 ${stale}`);
	}
});
