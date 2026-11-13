// Anotify v4 前端 · canonical 层单测（node:test + node:vm）
//
// 为什么用 vm：`web/partials.js` 是纯静态 IIFE（没有模块系统、没有构建步骤），
// 但它导出的 helper/模型是页面唯一的 canonical 层。这里把源码真的读进来、
// stub 掉 window/document/localStorage/location/fetch，然后**真实调用**这些
// 函数 —— 断言的是一长串行为（路径怎么拼、错误怎么解析、模型长什么样），
// 不是「文件存在」「字符串出现在源码里」这种假测试。
//
// F1a 覆盖 §3.4.1；F1b / F2a / F2b / F2c / F3 会在这份文件上继续追加。
//
// 跑法：`node --test scripts/v4-frontend.test.mjs`

import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import vm from "node:vm";
import { execFileSync } from "node:child_process";
import test from "node:test";
import assert from "node:assert/strict";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const PARTIALS_SRC = readFileSync(path.join(ROOT, "web/partials.js"), "utf8");
const MOCK_SRC = readFileSync(path.join(ROOT, "web/mock.js"), "utf8");
const GRAPHS_SRC = readFileSync(path.join(ROOT, "web/graphs.js"), "utf8");
const TRACEVIEW_SRC = readFileSync(path.join(ROOT, "web/traceview.js"), "utf8");

/* ------------------------------------------------------------------ *
 * 最小 DOM / 浏览器 stub（够 IIFE 加载与 helper 调用即可）
 * ------------------------------------------------------------------ */

function fakeNode(tag = "div") {
	const attrs = new Map();
	const children = [];
	const node = {
		nodeType: 1,
		tagName: String(tag).toUpperCase(),
		// graphs.js 的 render() 要求宿主已在文档里（否则 dagre 量到 0 尺寸）；
		// stub 出来的节点一律当作"已挂载"，图库才能在单测里跑通推导+装配。
		isConnected: true,
		// 真 DOM 有，traceview 的视图切换会用它（每次重渲染先清空容器）。
		// 必须**原地改** children 数组：appendChild 推的是上面闭包里的那个数组，
		// 换成 this.children = [...] 会让 appendChild 继续推旧数组（stub 特有的坑）。
		replaceChildren(...c) {
			children.length = 0;
			for (const item of c) if (item != null) children.push(item);
		},
		className: "",
		textContent: "",
		dataset: {},
		style: { setProperty() {}, removeProperty() {}, getPropertyValue: () => "" },
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
		insertBefore(child) {
			children.unshift(child);
			return child;
		},
		removeChild(child) {
			const i = children.indexOf(child);
			if (i >= 0) children.splice(i, 1);
			return child;
		},
		remove() {},
		setAttribute(k, v) {
			attrs.set(k, String(v));
		},
		getAttribute(k) {
			return attrs.has(k) ? attrs.get(k) : null;
		},
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
		focus() {},
		blur() {},
		click() {},
		closest: () => null,
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

function jsonResponse(body, status = 200) {
	return {
		ok: status >= 200 && status < 300,
		status,
		headers: { get: () => null },
		json: async () => body,
		text: async () => JSON.stringify(body),
	};
}

/**
 * 在独立 vm context 里加载 web/partials.js，返回 window.Anotify。
 *
 * @param {object} [opts]
 * @param {(path: string, init: object) => Promise<any>} [opts.fetch]
 * @param {Record<string,string>} [opts.storage]  预置 localStorage
 * @param {string} [opts.pathname]                 location.pathname
 */
/**
 * 造一个最小浏览器沙箱（partials.js 与 mock.js 共用）。
 *
 * @param {object} [opts]
 * @param {(path: string, init: object) => Promise<any>} [opts.fetch]
 * @param {Record<string,string>} [opts.storage]  预置 localStorage
 * @param {string} [opts.pathname]                 location.pathname
 */
function makeSandbox(opts = {}) {
	const store = new Map(Object.entries(opts.storage || {}));
	const calls = [];
	const fetchImpl = opts.fetch || (async () => jsonResponse({}));
	let hrefWrites = 0;
	let hrefValue = "";
	const location = {
		pathname: opts.pathname || "/index.html",
		search: opts.search || "",
		hash: "",
		reloaded: false,
		reload() {
			location.reloaded = true;
		},
	};
	// href 的真实写入次数：并发 401 只允许跳一次，断言需要能数到赋值。
	Object.defineProperty(location, "href", {
		get: () => hrefValue,
		set(value) {
			hrefWrites += 1;
			hrefValue = String(value);
		},
		enumerable: true,
	});

	const sandbox = {
		console,
		setTimeout,
		clearTimeout,
		setInterval,
		clearInterval,
		requestAnimationFrame: (fn) => setTimeout(fn, 0),
		cancelAnimationFrame: (id) => clearTimeout(id),
		fetch: (p, init) => {
			calls.push({ path: String(p), init: init || {} });
			return fetchImpl(String(p), init || {});
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
		location,
		navigator: {
			languages: ["zh-CN"],
			language: "zh-CN",
			clipboard: { writeText: async () => {} },
		},
		matchMedia: () => ({
			matches: false,
			addEventListener() {},
			removeEventListener() {},
			addListener() {},
			removeListener() {},
		}),
		document: fakeDocument(),
		getComputedStyle: () => ({ getPropertyValue: () => "" }),
		alert() {},
		prompt: () => null,
		CustomEvent: class {
			constructor(type, init) {
				this.type = type;
				Object.assign(this, init || {});
			}
		},
	};

	return { sandbox, store, calls, location, hrefWrites: () => hrefWrites };
}

function loadAnotify(opts = {}) {
	const base = makeSandbox(opts);
	const ctx = vm.createContext(base.sandbox);
	vm.runInContext("globalThis.window = globalThis; globalThis.self = globalThis;", ctx);
	vm.runInContext(PARTIALS_SRC, ctx, { filename: "web/partials.js" });

	return {
		A: base.sandbox.Anotify,
		ctx,
		sandbox: base.sandbox,
		store: base.store,
		calls: base.calls,
		location: base.location,
		hrefWrites: base.hrefWrites,
	};
}

/**
 * 加载 partials.js + mock.js（同一 vm context）：mock 会替换 window.fetch，
 * 所以之后的每个请求都真的走 mock 路由。返回的 `req()` 直接给出 {status, body}。
 */
function loadMock(opts = {}) {
	const base = makeSandbox(opts);
	const ctx = vm.createContext(base.sandbox);
	vm.runInContext("globalThis.window = globalThis; globalThis.self = globalThis;", ctx);
	vm.runInContext(PARTIALS_SRC, ctx, { filename: "web/partials.js" });
	vm.runInContext(opts.mockSrc || MOCK_SRC, ctx, { filename: "web/mock.js" });
	// 图库（可选）：sourceTopology / taskOf 是纯函数，能在没有布局的情况下断言
	if (opts.graphs) {
		// FlowGraph 的桩：只记录 spec，不做布局（布局交给 F3 的浏览器预览验）。
		// 这样 capabilityFlow / taskChain 的**装配**能在纯 node 里跑通。
		vm.runInContext(
			`window.FlowGraph = { mount: function (host, spec) {
				host._mountSpec = spec;
				return { fit: function () {}, replay: function () {}, _debug: { animated: function () { return false; } } };
			} };`,
			ctx,
		);
		vm.runInContext(GRAPHS_SRC, ctx, { filename: "web/graphs.js" });
		vm.runInContext(TRACEVIEW_SRC, ctx, { filename: "web/traceview.js" });
	}
	const sandbox = base.sandbox;
	// 演练时不需要"像真的"的延迟：请求数多，120~300ms 一条会拖慢整个套件。
	sandbox.MockApi.latency(0);
	const req = async (p, init) => {
		const res = await sandbox.fetch(
			p,
			Object.assign({ credentials: "include" }, init || {}),
		);
		// 204/205 无响应体（v4 的 DELETE 一律如此）：真 fetch 上 `res.json()` 会抛
		// SyntaxError，页面 `api()` 也是先看状态码再解析 —— 这里同样按状态码短路，
		// 而不是让一次**成功**的删除变成解析失败。
		const body = res.status === 204 || res.status === 205 ? null : await res.json();
		return { status: res.status, body: plain(body) };
	};
	return {
		A: sandbox.Anotify,
		Mock: sandbox.MockApi,
		ctx,
		sandbox,
		req,
	};
}

/* ------------------------------------------------------------------ *
 * 断言小工具（vm 里造出来的对象属于另一个 realm，先 JSON 归一化再用严格比较）
 * ------------------------------------------------------------------ */

function plain(value) {
	return value === undefined ? undefined : JSON.parse(JSON.stringify(value));
}

function throwsWith(fn, props = {}) {
	let caught = null;
	try {
		fn();
	} catch (err) {
		caught = err;
	}
	assert.ok(caught, "期望抛错，但没有抛");
	for (const [k, v] of Object.entries(props)) {
		assert.deepEqual(plain(caught[k]), v, `err.${k} 不符合预期`);
	}
	return caught;
}

async function rejectsWith(promise, props = {}) {
	let caught = null;
	try {
		await promise;
	} catch (err) {
		caught = err;
	}
	assert.ok(caught, "期望 reject，但没有 reject");
	for (const [k, v] of Object.entries(props)) {
		assert.deepEqual(plain(caught[k]), v, `err.${k} 不符合预期`);
	}
	return caught;
}

function keysDeep(value, out = new Set()) {
	if (Array.isArray(value)) {
		for (const item of value) keysDeep(item, out);
	} else if (value && typeof value === "object") {
		for (const [k, v] of Object.entries(value)) {
			out.add(k);
			keysDeep(v, out);
		}
	}
	return out;
}

/* ------------------------------------------------------------------ *
 * scope / 路径
 * ------------------------------------------------------------------ */

test("scope 存 localStorage.anotify.scope 的 { namespace }，nsPath 正确编码", () => {
	const { A, store } = loadAnotify();
	assert.equal(A.SCOPE_KEY, "anotify.scope");

	A.saveScope({ namespace: "team a/x" });
	assert.equal(plain(A.readScope()).namespace, "team a/x");
	assert.equal(
		JSON.parse(store.get("anotify.scope")).namespace,
		"team a/x",
		"scope 落盘的 key 必须是 anotify.scope 且只存 namespace",
	);

	assert.equal(
		A.nsPath("capabilities"),
		"/v1/namespaces/team%20a%2Fx/capabilities",
	);
	assert.equal(
		A.nsPath("capabilities", "cap 1"),
		"/v1/namespaces/team%20a%2Fx/capabilities/cap%201",
	);
	assert.equal(
		A.nsPath("traces", "tr/1"),
		"/v1/namespaces/team%20a%2Fx/traces/tr%2F1",
	);
});

test("nsPath 没有 scope 时抛 namespace_required（不拼畸形 URL）", () => {
	const { A } = loadAnotify();
	throwsWith(() => A.nsPath("capabilities"), { reason: "namespace_required" });
});

test("clusterPath 拼 cluster-scoped 路径并编码", () => {
	const { A } = loadAnotify();
	assert.equal(A.clusterPath("nodes"), "/v1/nodes");
	assert.equal(A.clusterPath("namespaces"), "/v1/namespaces");
	assert.equal(A.clusterPath("capability-templates"), "/v1/capability-templates");
	assert.equal(A.clusterPath("nodes", "node/1"), "/v1/nodes/node%2F1");
});

test("readScope 从已有 localStorage 恢复范围", () => {
	const { A } = loadAnotify({
		storage: { "anotify.scope": JSON.stringify({ namespace: "prod" }) },
	});
	assert.equal(A.readScope().namespace, "prod");
	assert.equal(A.nsPath("tasks"), "/v1/namespaces/prod/tasks");
});

test("saveScope 写入时清掉 v3 遗留的 namespaceId", () => {
	const { A, store } = loadAnotify({
		storage: {
			"anotify.scope": JSON.stringify({ namespaceId: "ns_old", namespace: "legacy" }),
		},
	});
	A.saveScope({ namespace: "prod" });
	const saved = JSON.parse(store.get("anotify.scope"));
	assert.equal(saved.namespace, "prod");
	assert.equal("namespaceId" in saved, false, "v3 的 namespaceId 不得残留在 scope 里");
});

test("v3 的 namespaceId 补参逻辑已删除：api 原样请求，不加 query", async () => {
	const { A, calls } = loadAnotify({
		fetch: async () => jsonResponse({ apiVersion: "anotify/v1", kind: "SourceList", items: [] }),
	});
	A.saveScope({ namespace: "default" });

	await A.api("/v1/sources");
	assert.equal(calls.length, 1, "scope 已确定时不该再拉 /v1/namespaces");
	assert.equal(calls[0].path, "/v1/sources", "不得再自动补 namespaceId query");
	assert.equal(calls[0].init.credentials, "include");

	assert.equal(A.withScope, undefined, "withScope 兼容层必须已删除");
	assert.equal(A.NS_PATHS, undefined, "NS_PATHS 兼容层必须已删除");
});

test("ensureScope 读 envelope items[].metadata.name，优先 default", async () => {
	const { A, store } = loadAnotify({
		fetch: async () =>
			jsonResponse({
				apiVersion: "anotify/v1",
				kind: "NamespaceList",
				metadata: {},
				items: [
					{ metadata: { name: "prod", displayName: "生产" } },
					{ metadata: { name: "default", displayName: "默认" } },
				],
			}),
	});

	await A.ensureScope();
	assert.equal(A.readScope().namespace, "default");
	assert.equal(JSON.parse(store.get("anotify.scope")).namespace, "default");
	assert.equal(A.nsPath("sources"), "/v1/namespaces/default/sources");
});

test("ensureScope 严格：v3 {namespaces:[]} / 裸数组视为失败且不缓存", async () => {
	let hits = 0;
	const { A } = loadAnotify({
		fetch: async () => {
			hits += 1;
			return jsonResponse({
				namespaces: [{ id: "ns_1", slug: "default", name: "默认" }],
			});
		},
	});

	await A.ensureScope();
	assert.equal(A.readScope().namespace, undefined, "旧形状不得写入 scope");
	await A.ensureScope();
	assert.equal(hits, 2, "失败不能缓存：下一次请求还要再试");

	const naked = loadAnotify({ fetch: async () => jsonResponse([{ id: "ns_1" }]) });
	await naked.A.ensureScope();
	assert.equal(naked.A.readScope().namespace, undefined);
});

test("ensureScope 空 items 是合法空列表（不写 scope，也不算失败）", async () => {
	const { A, calls } = loadAnotify({
		fetch: async () =>
			jsonResponse({ apiVersion: "anotify/v1", kind: "NamespaceList", metadata: {}, items: [] }),
	});
	await A.ensureScope();
	assert.equal(A.readScope().namespace, undefined);
	assert.equal(calls.length, 1);
});

/* ------------------------------------------------------------------ *
 * 页面数据入口：ready / nsApi / nsList（先引导 scope，再拼路径）
 *
 * 这三条测的就是 review 指出的组合缺陷：`api(nsPath(...))` 的**参数先求值**，
 * 空 scope 下永远走不到 api() 里的引导。所以断言分两半：请求顺序（先
 * `/v1/namespaces` 再目标端点）与失败时“只发了引导请求”。
 * ------------------------------------------------------------------ */

const NS_LIST_BODY = {
	apiVersion: "anotify/v1",
	kind: "NamespaceList",
	metadata: {},
	items: [
		{ metadata: { name: "prod", displayName: "生产" } },
		{ metadata: { name: "default", displayName: "默认" } },
	],
};

test("nsApi 空 scope：先 GET /v1/namespaces（envelope，取 default）再 GET 目标端点", async () => {
	const CAPS = {
		apiVersion: "anotify/v1",
		kind: "CapabilityList",
		metadata: {},
		items: [{ metadata: { name: "fixer" } }],
	};
	const { A, calls } = loadAnotify({
		fetch: async (p) => {
			if (p === "/v1/namespaces") return jsonResponse(NS_LIST_BODY);
			if (p === "/v1/namespaces/default/capabilities") return jsonResponse(CAPS);
			throw new Error("unexpected request: " + p);
		},
	});
	// 前置：scope 必须是空的 —— 不许先 saveScope 绕过 ensureScope
	assert.equal(A.readScope().namespace, undefined);

	const res = await A.nsApi("capabilities");

	assert.deepEqual(
		calls.map((c) => c.path),
		["/v1/namespaces", "/v1/namespaces/default/capabilities"],
		"必须先引导 scope，再请求 ns 路径",
	);
	assert.equal(calls[0].init.credentials, "include");
	assert.deepEqual(plain(res), plain(CAPS), "nsApi 要返回解析后的响应体");
	assert.equal(A.readScope().namespace, "default");
});

test("nsApi 引导失败：抛 namespace_required，且不对目标端点发请求", async () => {
	const { A, calls } = loadAnotify({
		fetch: async (p) => {
			assert.equal(p, "/v1/namespaces", "失败路径只允许发引导请求");
			return jsonResponse(
				{
					apiVersion: "anotify/v1",
					kind: "Status",
					status: "Failure",
					code: 500,
					reason: "internal",
					message: "boom",
				},
				500,
			);
		},
	});

	await rejectsWith(A.nsApi("capabilities"), { reason: "namespace_required" });
	assert.deepEqual(calls.map((c) => c.path), ["/v1/namespaces"]);
});

test("nsList 空 scope：先引导 scope 再取严格列表，返回 {items,warnings}", async () => {
	const LIST = {
		apiVersion: "anotify/v1",
		kind: "SourceList",
		metadata: {},
		items: [{ metadata: { name: "hook" } }],
		warnings: [{ code: "workdir_shared_concurrency", message: "共享工作目录" }],
	};
	const { A, calls } = loadAnotify({
		fetch: async (p) => {
			if (p === "/v1/namespaces") return jsonResponse(NS_LIST_BODY);
			if (p === "/v1/namespaces/default/sources") return jsonResponse(LIST);
			throw new Error("unexpected request: " + p);
		},
	});
	assert.equal(A.readScope().namespace, undefined);

	const out = await A.nsList("sources");
	assert.deepEqual(calls.map((c) => c.path), [
		"/v1/namespaces",
		"/v1/namespaces/default/sources",
	]);
	assert.equal(out.items.length, 1);
	assert.equal(out.items[0].metadata.name, "hook");
	assert.equal(out.warnings.length, 1);
});

test("nsList 引导失败：抛 namespace_required，不请求列表端点", async () => {
	const { A, calls } = loadAnotify({
		fetch: async () => jsonResponse({ kind: "Status", reason: "internal", message: "boom" }, 500),
	});
	await rejectsWith(A.nsList("sources"), { reason: "namespace_required" });
	assert.deepEqual(calls.map((c) => c.path), ["/v1/namespaces"]);
});

test("ready() = ensureScope：空 scope 时拉一次命名空间列表并写入 default", async () => {
	const { A, calls } = loadAnotify({ fetch: async () => jsonResponse(NS_LIST_BODY) });
	assert.equal(A.readScope().namespace, undefined);
	const scope = await A.ready();
	assert.equal(plain(scope).namespace, "default");
	assert.deepEqual(calls.map((c) => c.path), ["/v1/namespaces"]);
});

/* ------------------------------------------------------------------ *
 * api()：401 / Status 错误解析
 * ------------------------------------------------------------------ */

test("api 401 保留白名单跳转（行为不变）", async () => {
	const { A, location } = loadAnotify({
		fetch: async () => jsonResponse({ message: "unauthorized" }, 401),
		pathname: "/capabilities.html",
	});
	A.saveScope({ namespace: "default" });
	const err = await rejectsWith(A.api("/v1/namespaces/default/capabilities"));
	assert.equal(err.message, "HTTP 401");
	assert.equal(location.href, "login.html?next=capabilities.html");
});

test("api 并发 401 只跳一次登录页（重复赋值会打断同一个导航，登录页反而可能加载不完）", async () => {
	const { A, hrefWrites, location } = loadAnotify({
		fetch: async () => jsonResponse({ message: "unauthorized" }, 401),
		pathname: "/index.html",
	});
	A.saveScope({ namespace: "default" });
	const results = await Promise.allSettled([
		A.api("/v1/auth/me"),
		A.api("/v1/namespaces"),
		A.api("/v1/stats"),
	]);
	assert.deepEqual(results.map((r) => r.status), ["rejected", "rejected", "rejected"]);
	assert.equal(hrefWrites(), 1, "三个并发 401 只允许写一次 location.href");
	assert.equal(location.href, "login.html?next=index.html");
	// 每次调用仍然如实失败（不因为“已经跳过了”就把 401 当成功）。
	for (const r of results) assert.equal(r.reason.message, "HTTP 401");
});

test("api 409 Status：解析 reason/message/details/currentResourceVersion/warnings", async () => {
	const status = {
		apiVersion: "anotify/v1",
		kind: "Status",
		status: "Failure",
		code: 409,
		reason: "resource_version_conflict",
		message: "resourceVersion 已过期",
		details: { causes: [], current: { resourceVersion: "1842" } },
		warnings: [{ code: "workdir_shared_concurrency", message: "共享工作目录" }],
	};
	const { A } = loadAnotify({ fetch: async () => jsonResponse(status, 409) });
	A.saveScope({ namespace: "default" });

	const err = await rejectsWith(
		A.api("/v1/namespaces/default/capabilities/fixer", { method: "PATCH" }),
		{
			status: 409,
			code: 409,
			reason: "resource_version_conflict",
			message: "resourceVersion 已过期",
		},
	);
	assert.equal(err.currentResourceVersion, "1842");
	assert.equal(err.details.current.resourceVersion, "1842");
	assert.equal(plain(err.warnings).length, 1);
	assert.equal(A.isConflict(err), true);
});

test("api 400 的 Status 不是冲突；非 Status 响应保留原文 detail", async () => {
	const { A } = loadAnotify({
		fetch: async () =>
			jsonResponse(
				{
					apiVersion: "anotify/v1",
					kind: "Status",
					status: "Failure",
					code: 400,
					reason: "resource_version_required",
					message: "缺少 metadata.resourceVersion",
					details: { causes: [] },
				},
				400,
			),
	});
	A.saveScope({ namespace: "default" });
	const required = await rejectsWith(A.api("/v1/namespaces/default/sources/a"), {
		status: 400,
		reason: "resource_version_required",
	});
	assert.equal(A.isConflict(required), false);
	assert.equal(required.currentResourceVersion, null);

	const plainErr = loadAnotify({
		fetch: async () => jsonResponse({ error: "gateway down" }, 502),
	});
	plainErr.A.saveScope({ namespace: "default" });
	const err = await rejectsWith(plainErr.A.api("/v1/stats"), { status: 502 });
	assert.equal(err.detail, "gateway down");
	assert.equal(err.reason, undefined, "非 Status 响应不得伪造 reason");
	assert.equal(plainErr.A.isConflict(err), false);
});

/*
 * v4 的 DELETE 一律 204（resource_source.go / resource_node.go …：
 * `w.WriteHeader(http.StatusNoContent)`）。`api()` 若无条件 `res.json()`，
 * 一次**成功**的删除会抛 SyntaxError → 页面走 catch 把错误显示出来、列表不刷新。
 * 这条钉住「成功但无响应体」的返回语义，也钉住 304/空体不被误当成 JSON。
 */
test("api 204/205 无响应体：成功删除返回 null，不把成功当解析失败", async () => {
	// 真 Response 在无体时 json() 抛 SyntaxError，这里按同样行为建模。
	const bodyless = (status) => ({
		ok: status >= 200 && status < 300,
		status,
		headers: { get: () => null },
		json: async () => {
			throw new SyntaxError("Unexpected end of JSON input");
		},
		text: async () => "",
	});
	for (const status of [204, 205]) {
		const { A, calls } = loadAnotify({ fetch: async () => bodyless(status) });
		A.saveScope({ namespace: "default" });
		assert.equal(
			await A.api(A.deletePath("/v1/namespaces/default/sources/hook", "7"), { method: "DELETE" }),
			null,
			`HTTP ${status} 的成功响应必须解析成 null`,
		);
		assert.equal(calls.filter((c) => c.path.includes("/sources/hook")).length, 1);
	}
	// 200 + 空体（网关型的成功但无内容）同样不能抛。
	const empty200 = loadAnotify({ fetch: async () => bodyless(200) });
	empty200.A.saveScope({ namespace: "default" });
	assert.equal(await empty200.A.api("/v1/namespaces/default/sources/hook", { method: "DELETE" }), null);
	// 304 不是 2xx：仍走错误分支（不能被 204 的“无体”语义顺手放过）。
	const notModified = loadAnotify({ fetch: async () => bodyless(304) });
	notModified.A.saveScope({ namespace: "default" });
	const err304 = await rejectsWith(notModified.A.api("/v1/namespaces/default/capabilities"), { status: 304 });
	assert.equal(err304.message, "HTTP 304");
});

/* ------------------------------------------------------------------ *
 * 列表严格性：itemsOf / apiList / warningsOf
 * ------------------------------------------------------------------ */

test("itemsOf 严格只认 items 数组", () => {
	const { A } = loadAnotify();
	assert.deepEqual(plain(A.itemsOf({ kind: "CapabilityList", items: [{ name: "a" }] })), [
		{ name: "a" },
	]);
	throwsWith(() => A.itemsOf([{ name: "a" }]), { reason: "malformed_list" });
	throwsWith(() => A.itemsOf({ namespaces: [] }), { reason: "malformed_list" });
	throwsWith(() => A.itemsOf({ items: {} }), { reason: "malformed_list" });
	throwsWith(() => A.itemsOf(null), { reason: "malformed_list" });
});

test("apiList 返回 {items,warnings}，畸形列表抛 malformed_list", async () => {
	const list = {
		apiVersion: "anotify/v1",
		kind: "SourceList",
		metadata: {},
		items: [{ metadata: { name: "hook" } }],
		warnings: [{ code: "unreachable", message: "node unreachable" }],
	};
	const okLoad = loadAnotify({ fetch: async () => jsonResponse(list) });
	okLoad.A.saveScope({ namespace: "default" });
	const out = await okLoad.A.apiList("/v1/namespaces/default/sources");
	assert.equal(plain(out.items).length, 1);
	assert.equal(plain(out.warnings)[0].code, "unreachable");

	const v3 = loadAnotify({ fetch: async () => jsonResponse({ namespaces: [] }) });
	v3.A.saveScope({ namespace: "default" });
	await rejectsWith(v3.A.apiList("/v1/namespaces/default/sources"), {
		reason: "malformed_list",
	});

	const notList = loadAnotify({
		fetch: async () =>
			jsonResponse({ apiVersion: "anotify/v1", kind: "Source", metadata: {}, spec: {} }),
	});
	notList.A.saveScope({ namespace: "default" });
	await rejectsWith(notList.A.apiList("/v1/namespaces/default/sources"), {
		reason: "malformed_list",
	});
});

test("warningsOf 缺省 []，statusOf/readyOf/conditionOf 走 status", () => {
	const { A } = loadAnotify();
	assert.deepEqual(plain(A.warningsOf({ items: [] })), []);
	assert.deepEqual(plain(A.warningsOf({ warnings: [{ code: "x" }] })), [{ code: "x" }]);

	const res = { status: { ready: true, conditions: [{ type: "Implemented", status: "True" }] } };
	assert.equal(A.statusOf(res).ready, true);
	assert.equal(A.readyOf(res), true);
	assert.equal(A.readyOf({}), false);
	assert.equal(A.conditionOf(res, "Implemented").status, "True");
	assert.equal(A.conditionOf(res, "Deployed"), null);
	assert.deepEqual(plain(A.statusOf({})), {});
});

/* ------------------------------------------------------------------ *
 * 写路径：writable / withResourceVersion / deletePath
 * ------------------------------------------------------------------ */

const CAP_RES = {
	apiVersion: "anotify/v1",
	kind: "Capability",
	metadata: {
		name: "fixer",
		namespace: "default",
		uid: "cap_1",
		version: 3,
		resourceVersion: "1841",
		creationTimestamp: "2024-01-01T00:00:00Z",
		displayName: "修 bug",
	},
	spec: { run: { entrypoint: "main.sh" }, next: ["analyze@2"] },
	status: { ready: true, conditions: [{ type: "Implemented", status: "True" }] },
};

test("writable 剥离 status；create 再剥离 uid/resourceVersion/creationTimestamp", () => {
	const { A } = loadAnotify();

	const patch = A.writable(CAP_RES);
	assert.equal(patch.status, undefined, "PATCH 不得带 status");
	assert.equal(patch.metadata.uid, "cap_1", "更新时保留 uid（服务端字段原样回去无害）");
	assert.equal(patch.metadata.resourceVersion, "1841");
	assert.equal(patch.metadata.name, "fixer");
	assert.equal(patch.metadata.namespace, "default");
	assert.equal(patch.metadata.version, 3);
	assert.equal(patch.metadata.displayName, "修 bug");
	assert.equal(patch.spec.run.entrypoint, "main.sh");

	const create = A.writable(CAP_RES, { create: true });
	assert.equal(create.status, undefined);
	assert.equal(create.metadata.uid, undefined);
	assert.equal(create.metadata.resourceVersion, undefined);
	assert.equal(create.metadata.creationTimestamp, undefined);
	assert.equal(create.metadata.name, "fixer");
	assert.equal(create.metadata.namespace, "default");
	assert.equal(create.metadata.version, 3);

	// 深拷贝：改提交体不能污染编辑中的原对象
	create.spec.run.entrypoint = "changed.sh";
	assert.equal(CAP_RES.spec.run.entrypoint, "main.sh");
});

test("withResourceVersion 注入 metadata.resourceVersion，缺失抛 resource_version_required", () => {
	const { A } = loadAnotify();
	const body = { metadata: { name: "fixer" }, spec: {} };
	const out = A.withResourceVersion(body, "1841");
	assert.equal(out.metadata.resourceVersion, "1841");
	assert.equal(out.metadata.name, "fixer");
	assert.equal(body.metadata.resourceVersion, undefined, "不得改写调用方对象");

	const noMeta = A.withResourceVersion({ spec: {} }, 7);
	assert.equal(noMeta.metadata.resourceVersion, "7");

	throwsWith(() => A.withResourceVersion(body, ""), {
		reason: "resource_version_required",
	});
	throwsWith(() => A.withResourceVersion(body, null), {
		reason: "resource_version_required",
	});
});

test("deletePath 用 ?resourceVersion= 编码，缺失 rv 抛错", () => {
	const { A } = loadAnotify();
	assert.equal(
		A.deletePath("/v1/namespaces/default/sources/hook", "1841"),
		"/v1/namespaces/default/sources/hook?resourceVersion=1841",
	);
	assert.equal(
		A.deletePath("/v1/nodes/n1", "a b/c"),
		"/v1/nodes/n1?resourceVersion=a%20b%2Fc",
	);
	assert.equal(
		A.deletePath("/v1/nodes/n1?x=1", "7"),
		"/v1/nodes/n1?x=1&resourceVersion=7",
	);
	throwsWith(() => A.deletePath("/v1/nodes/n1", ""), {
		reason: "resource_version_required",
	});
});

test("parseJSON 成功返回对象；失败给出可显示错误并保留原文", () => {
	const { A } = loadAnotify();
	assert.deepEqual(plain(A.parseJSON('{"a":1}', "spec.params")), { a: 1 });

	const err = throwsWith(() => A.parseJSON("{oops", "spec.params"), {
		reason: "invalid_json",
	});
	assert.match(err.message, /spec\.params/);
	assert.equal(err.text, "{oops", "原文必须留着（页面不清空输入框）");
});

/* ------------------------------------------------------------------ *
 * 模型：capability / source / playbook / receipt / installGroup
 * ------------------------------------------------------------------ */

const CAP_STATUS = {
	ready: true,
	version: 3,
	digest: "sha256:abc",
	conditions: [
		{ type: "Implemented", status: "True", reason: "Ready", message: "" },
		{ type: "NodesAvailable", status: "False", reason: "NoNode", message: "" },
		{ type: "DownstreamResolved", status: "True", reason: "Resolved", message: "" },
		{ type: "Deployed", status: "False", reason: "NotDeployed", message: "" },
	],
	nodes: [{ name: "node-a", state: "deployed", extra: "ignored" }],
	resolved: {
		next: [
			{
				name: "analyze",
				version: 2,
				digest: "sha256:next",
				ready: true,
				params: { required: ["repo"] },
				missing: false,
			},
			{ name: "ghost", version: 1, missing: true, reason: "NotFound" },
		],
	},
	observed: {
		taskId: "task-1",
		emits: [{ eventType: "webhook" }],
		pruned: [{ reason: "no_target" }],
		tasks: { "task-1": 2 },
	},
	installedFrom: "delivery",
};

test("model.capability 读 v4 信封：conditions/nodes/resolved.next/observed 明细", () => {
	const { A } = loadAnotify();
	const res = Object.assign({}, CAP_RES, { status: CAP_STATUS });
	const m = plain(A.model.capability(res));

	assert.equal(m.name, "fixer");
	assert.equal(m.namespace, "default");
	assert.equal(m.version, 3);
	assert.equal(m.displayName, "修 bug");
	assert.equal(m.resourceVersion, "1841");
	assert.equal(m.spec.next[0], "analyze@2");
	assert.equal(m.ready, true);
	assert.equal(m.conditions.length, 4);
	assert.equal(m.conditions[3].type, "Deployed");
	assert.deepEqual(m.nodes, [{ name: "node-a", state: "deployed" }]);
	assert.deepEqual(
		m.resolvedNext,
		[
			{
				name: "analyze",
				version: 2,
				digest: "sha256:next",
				ready: true,
				params: { required: ["repo"] },
				missing: false,
			},
			{ name: "ghost", version: 1, missing: true, reason: "NotFound" },
		],
		"resolved.next 必须保留条目明细（digest/ready/params/reason），不只三个字段",
	);
	assert.equal(m.observed.taskId, "task-1");
	assert.equal(m.observed.emits.length, 1);
	assert.equal(m.observed.pruned[0].reason, "no_target");
	assert.equal(m.observed.tasks["task-1"], 2);
	assert.equal(m.installedFrom, "delivery");

	assert.equal(A.conditionOf(res, "Deployed").status, "False");
	assert.equal(A.conditionOf(res, "Missing"), null);
});

test("model 对空对象/缺字段不抛，给 null 或 []", () => {
	const { A } = loadAnotify();
	const cap = plain(A.model.capability({}));
	assert.equal(cap.name, null);
	assert.deepEqual(cap.conditions, []);
	assert.deepEqual(cap.nodes, []);
	assert.deepEqual(cap.resolvedNext, []);
	assert.deepEqual(cap.observed, { taskId: null, emits: [], pruned: [], tasks: {} });
	assert.equal(cap.ready, false);
});

const SOURCE_RES = {
	apiVersion: "anotify/v1",
	kind: "Source",
	metadata: {
		name: "hook",
		namespace: "default",
		resourceVersion: "77",
		displayName: "Webhook 入口",
	},
	spec: {
		kind: "webhook",
		enabled: true,
		config: {
			hookUrl: "https://hub.example/v1/hooks/tok_abc",
			secretSet: true,
			// 故意塞进响应：模型必须逐字段投影，绝不整体展开 config
			token: "tok_secret_value",
			secret: "hmac_secret_value",
			payloadSchema: { type: "object", properties: { repo: { type: "string" } } },
			sample: { repo: "a/b" },
		},
		targets: [{ capability: "fixer", params: { repo: "$(payload.repo)" } }],
	},
	status: {
		hookUrl: "https://hub.example/v1/hooks/tok_abc",
		health: "ok",
		lastFiredAt: "2024-01-02T00:00:00Z",
		nextFireAt: null,
		targetsResolved: true,
		installedFrom: null,
	},
};

test("model.source 读投影：hookUrl/secretSet/payloadSchema，绝不含 token/secret", () => {
	const { A } = loadAnotify();
	const m = plain(A.model.source(SOURCE_RES));

	assert.equal(m.name, "hook");
	assert.equal(m.kind, "webhook");
	assert.equal(m.enabled, true);
	assert.equal(m.resourceVersion, "77");
	assert.equal(m.config.hookUrl, "https://hub.example/v1/hooks/tok_abc");
	assert.equal(m.config.secretSet, true);
	assert.equal(m.config.payloadSchema.properties.repo.type, "string");
	assert.deepEqual(m.config.sample, { repo: "a/b" });
	assert.equal(m.config.expr, null);
	assert.equal(m.config.tz, null);
	assert.equal(m.targets[0].params.repo, "$(payload.repo)");
	assert.equal(m.health, "ok");
	assert.equal(m.lastFiredAt, "2024-01-02T00:00:00Z");
	assert.equal(m.nextFireAt, null);
	// 契约 api-conventions §7.2：boolean，不是解析后的数组
	assert.equal(m.targetsResolved, true);
	assert.equal(typeof m.targetsResolved, "boolean");

	const keys = [...keysDeep(m)];
	assert.ok(!keys.includes("token"), "模型里不得出现 token 字段");
	assert.ok(!keys.includes("secret"), "模型里不得出现 secret 字段");
	const dumped = JSON.stringify(m);
	assert.ok(!dumped.includes("tok_secret_value"), "不得回显 token 值");
	assert.ok(!dumped.includes("hmac_secret_value"), "不得回显 secret 值");

	const empty = plain(A.model.source({}));
	assert.equal(empty.kind, null);
	assert.equal(empty.config.hookUrl, null);
	assert.deepEqual(empty.targets, []);
	assert.equal(empty.targetsResolved, false, "缺字段给 false（不是 []）");
});

const PLAYBOOK_RES = {
	apiVersion: "anotify/v1",
	kind: "Playbook",
	metadata: { name: "delivery", namespace: "default", version: "1.2.0", displayName: "交付" },
	spec: {
		files: { "capabilities/a.yaml": "kind: Capability", "playbook.yaml": JSON.stringify({
			metadata: { name: "delivery" }, spec: { values: { type: "object", properties: { repo: { type: "string" } } }, capabilities: ["capabilities/a.yaml"], sources: ["sources/s.yaml"] }
		}) },
	},
	status: {
		ready: false,
		total: 2,
		unapplied: [
			{
				kind: "Capability",
				ref: "fixer",
				field: "routing",
				declared: "analyze@2",
				actual: "analyze@1",
				reason: "routing_unapplied",
			},
		],
		conditions: [{ type: "Installed", status: "True" }],
		blocker: { type: "pending_ref", capability: "analyze", action: "install analyze" },
		items: [
			{ kind: "Capability", name: "fixer", version: 3, state: "no_node", targets: ["repo"] },
			{ kind: "Source", name: "hook", state: "unapplied" },
		],
		sources: { total: 1, enabled: 0, blocked: 0, disabled: 0, unapplied: 1 },
		lastInstallId: "inst-1",
	},
};

test("model.playbook：valuesSchema/status.unapplied/items/sources/blocker/files", () => {
	const { A } = loadAnotify();
	const m = plain(A.model.playbook(PLAYBOOK_RES));

	assert.equal(m.slug, "delivery");
	assert.equal(m.version, "1.2.0");
	assert.equal(m.displayName, "交付");
	assert.equal(m.valuesSchema.properties.repo.type, "string");
	assert.deepEqual(m.capabilities, ["capabilities/a.yaml"]);
	assert.deepEqual(m.sources, ["sources/s.yaml"]);
	assert.equal(m.status.ready, false);
	assert.equal(m.status.total, 2, "status.total 要保留（回执 12 格的“总条数”）");
	assert.equal(m.status.unapplied[0].reason, "routing_unapplied");
	assert.equal(m.status.unapplied[0].declared, "analyze@2");
	assert.equal(m.status.items[0].state, "no_node");
	assert.equal(m.status.items[1].state, "unapplied");
	assert.equal(m.status.sources.unapplied, 1);
	assert.equal(m.status.blocker.action, "install analyze");
	assert.equal(m.status.lastInstallId, "inst-1");
	assert.equal(m.files["capabilities/a.yaml"], "kind: Capability");

	const noBody = plain(A.model.playbook(Object.assign({}, PLAYBOOK_RES, { spec: { ...PLAYBOOK_RES.spec, files: undefined } })));
	assert.equal("files" in noBody, false, "没带 includeBody 时不该出现 files");

	const empty = plain(A.model.playbook({}));
	assert.deepEqual(empty.status.unapplied, []);
	assert.deepEqual(empty.status.items, []);
	assert.deepEqual(empty.status.conditions, []);
	assert.equal(empty.status.ready, false);
});

const RECEIPT = {
	apiVersion: "anotify/v1",
	kind: "InstallReceipt",
	playbook: { name: "delivery", version: "1.2.0" },
	source: "inline",
	install: { id: "inst-1", at: "2024-01-01T00:00:00Z", dryRun: false },
	namespace: "default",
	values: { repo: "a/b" },
	valuesDigest: "sha256:1",
	created: {
		capabilities: [{ kind: "Capability", name: "fixer" }],
		sources: [],
	},
	skipped: { capabilities: [], sources: [] },
	upgraded: [{ kind: "Capability", name: "analyze", from: 1, to: 2 }],
	unapplied: [
		{
			kind: "Source",
			ref: "hook",
			field: "targets",
			declared: 1,
			actual: 0,
			reason: "targets_unapplied",
		},
	],
	external: ["https://example/x.yaml"],
	pending: [{ kind: "Capability", ref: "analyze@3" }],
	notes: [{ code: "pending_ref", ref: "analyze@3", message: "未安装" }],
	conflicts: [],
};

test("model.receipt：12 格字段齐全，created/skipped 是对象，其余数组缺省 []", () => {
	const { A } = loadAnotify();
	const m = plain(A.model.receipt(RECEIPT));

	assert.deepEqual(m.playbook, { name: "delivery", version: "1.2.0" });
	assert.equal(m.source, "inline", "顶层 source（安装来源）要保留");
	assert.equal(m.namespace, "default");
	assert.equal(m.install.id, "inst-1");
	assert.equal(m.install.dryRun, false);
	assert.equal(m.valuesDigest, "sha256:1");
	// created / skipped 是**对象**（playbook.md §4.5）：arr() 强转会把它变成 []
	assert.deepEqual(m.created, {
		capabilities: [{ kind: "Capability", name: "fixer" }],
		sources: [],
	});
	assert.equal(m.created.capabilities.length, 1);
	assert.deepEqual(m.skipped, { capabilities: [], sources: [] });
	assert.equal(m.upgraded[0].to, 2);
	assert.equal(m.unapplied[0].reason, "targets_unapplied");
	assert.equal(m.unapplied[0].declared, 1);
	assert.equal(m.external.length, 1);
	assert.equal(m.pending[0].ref, "analyze@3");
	assert.equal(m.notes[0].code, "pending_ref");
	assert.deepEqual(m.conflicts, []);

	const empty = plain(A.model.receipt({}));
	assert.deepEqual(empty.created, { capabilities: [], sources: [] });
	assert.deepEqual(empty.skipped, { capabilities: [], sources: [] });
	for (const key of ["upgraded", "unapplied", "external", "pending", "notes", "conflicts"]) {
		assert.deepEqual(empty[key], [], `${key} 缺省必须是 []`);
	}
	assert.equal(empty.source, null);
	assert.deepEqual(empty.install, { id: null, at: null, dryRun: false });
});

test("model.installGroup：按 valuesDigest 折叠的 attempts/lastInstallId", () => {
	const { A } = loadAnotify();
	const m = plain(
		A.model.installGroup({
			valuesDigest: "sha256:1",
			values: { repo: "a/b" },
			firstAt: "t1",
			lastAt: "t3",
			attempts: 3,
			lastInstallId: "inst-3",
			capabilities: [{ name: "fixer" }],
			sources: [],
		}),
	);
	assert.equal(m.valuesDigest, "sha256:1");
	assert.equal(m.attempts, 3);
	assert.equal(m.lastInstallId, "inst-3");
	assert.equal(m.capabilities.length, 1);
	assert.deepEqual(m.sources, []);

	const empty = plain(A.model.installGroup({}));
	assert.equal(empty.attempts, 0);
	assert.deepEqual(empty.capabilities, []);
});

/* ------------------------------------------------------------------ *
 * 模型：task / node / edge / event / trace
 * ------------------------------------------------------------------ */

const TASK_RES = {
	apiVersion: "anotify/v1",
	kind: "Task",
	metadata: { name: "task-7", namespace: "default", creationTimestamp: "2024-01-01T00:00:00Z" },
	spec: {
		traceUID: "tr-1",
		sequence: 2,
		sourceRef: "hook",
		parentRef: "fixer@3",
		parentTaskId: "task-6",
		capability: { name: "fixer", version: 3, digest: "sha256:x" },
		params: { repo: "a/b" },
	},
	status: {
		phase: "Running",
		waitingReason: "retry_backoff",
		nextAttemptAt: "2024-01-01T00:01:00Z",
		nodeName: "node-a",
		instanceId: "i-1",
		attempt: 2,
		maxAttempts: 3,
		leaseExpiresAt: "2024-01-01T00:00:30Z",
		startedAt: "2024-01-01T00:00:00Z",
		finishedAt: null,
		configRevision: 7,
		namespaceVarsRevision: 4,
		envDiagnostics: [
			// 后端理论不返回值；万一带了，模型也必须只留名字+来源
			{ name: "GIT_TOKEN", source: "namespace", value: "ghp_secret" },
			{ name: "PATH", source: "builtin" },
		],
		result: {
			summary: "ok",
			artifacts: [{ name: "a" }],
			error: { reason: "x", message: "y" },
		},
		attempts: [
			{
				attempt: 1,
				nodeName: "node-b",
				configRevision: 6,
				phase: "Failed",
				startedAt: "t1",
				finishedAt: "t2",
				reason: "lease_expired",
			},
		],
	},
};

test("model.task：phase 小写、waitingReason/attempt/envDiagnostics 只名字+来源", () => {
	const { A } = loadAnotify();
	const m = plain(A.model.task(TASK_RES));

	assert.equal(m.name, "task-7");
	assert.equal(m.creationTimestamp, "2024-01-01T00:00:00Z", "保留 metadata.creationTimestamp");
	assert.equal(m.phase, "running", "phase 必须小写");
	assert.equal(m.waitingReason, "retry_backoff");
	assert.equal(m.nextAttemptAt, "2024-01-01T00:01:00Z");
	assert.equal(m.attempt, 2);
	assert.equal(m.maxAttempts, 3);
	assert.equal(m.nodeName, "node-a");
	assert.equal(m.parentRef, "fixer@3");
	assert.equal(m.parentTaskId, "task-6");
	assert.equal(m.traceUID, "tr-1");
	assert.equal(m.capability.digest, "sha256:x");
	assert.equal(m.configRevision, 7);
	assert.equal(m.namespaceVarsRevision, 4);
	assert.deepEqual(m.envDiagnostics, [
		{ name: "GIT_TOKEN", source: "namespace" },
		{ name: "PATH", source: "builtin" },
	]);
	assert.ok(
		!keysDeep(m.envDiagnostics).has("value"),
		"envDiagnostics 不得带值（值只在本机）",
	);
	assert.equal(JSON.stringify(m).includes("ghp_secret"), false);
	assert.equal(m.result.summary, "ok");
	assert.equal(m.result.artifacts.length, 1);
	assert.equal(m.result.error.message, "y");
	assert.equal(m.attempts[0].attempt, 1);
	assert.equal(m.attempts[0].phase, "failed");
	assert.equal(m.attempts[0].reason, "lease_expired");

	const empty = plain(A.model.task({}));
	assert.equal(empty.phase, null);
	assert.equal(empty.creationTimestamp, null);
	assert.deepEqual(empty.envDiagnostics, []);
	assert.deepEqual(empty.attempts, []);
	assert.deepEqual(empty.result, { summary: null, artifacts: [], error: null });
});

const NODE_RES = {
	apiVersion: "anotify/v1",
	kind: "Node",
	metadata: { name: "node-a", resourceVersion: "12" },
	spec: {
		namespaces: ["default", "prod"],
		vars: { NODE_DEFAULT: "n" },
		limits: { total: 4, perCapability: { "*": 1 } },
		tags: ["gpu"],
	},
	status: {
		hostIP: "10.0.0.5",
		hostname: "build-1",
		os: "linux",
		arch: "amd64",
		capacity: { cpu: 8 },
		workRoot: "/var/lib/anotify",
		homeDir: "/home/u",
		machineVars: { MACHINE: "m" },
		localVars: { LOCAL: "l" },
		nodeVars: { NODE_DEFAULT: "n", MACHINE: "override" },
		configRevision: 7,
		appliedRevision: 6,
		appliedHash: "sha256:y",
		effectiveNamespaces: ["default"],
		agentVersion: "1.4.0",
		instances: [],
		lastSeenAt: "2024-01-01T00:00:00Z",
		namespaceVars: { NS_VAR: "ns" },
		// v4：effectiveVars 每项是 {value,source}（resource.yaml · NodeStatus）
		effectiveVars: { NS_VAR: { value: "ns", source: "namespace" } },
	},
	// v4：`?namespace=` 的读上下文是**信封顶层**键（Node.context），不在 status 里
	context: { namespace: "default" },
};

test("model.node：三层 vars、双 revision、pending、?namespace 上下文", () => {
	const { A } = loadAnotify();
	const m = plain(A.model.node(NODE_RES));

	assert.equal(m.name, "node-a");
	assert.deepEqual(m.spec.namespaces, ["default", "prod"]);
	assert.deepEqual(m.spec.limits, { total: 4, perCapability: { "*": 1 } });
	assert.deepEqual(m.spec.tags, ["gpu"]);
	assert.deepEqual(m.machineVars, { MACHINE: "m" });
	assert.deepEqual(m.localVars, { LOCAL: "l" });
	assert.deepEqual(m.nodeVars, { NODE_DEFAULT: "n", MACHINE: "override" });
	assert.equal(m.configRevision, 7);
	assert.equal(m.appliedRevision, 6);
	assert.equal(m.pending, true, "appliedRevision !== configRevision → ConfigPending");
	assert.deepEqual(m.effectiveNamespaces, ["default"]);
	assert.deepEqual(m.context, { namespace: "default" });
	assert.deepEqual(m.namespaceVars, { NS_VAR: "ns" });
	assert.deepEqual(m.effectiveVars, { NS_VAR: { value: "ns", source: "namespace" } });

	const synced = plain(
		A.model.node(
			Object.assign({}, NODE_RES, {
				status: Object.assign({}, NODE_RES.status, { appliedRevision: 7 }),
			}),
		),
	);
	assert.equal(synced.pending, false);

	// 没带 `?namespace=` 时（信封顶层没有 context），即使 status 里混进了
	// namespaceVars/effectiveVars，也不能当成“覆盖全部 ns”展示 —— 那正是 review
	// 指出的假覆盖。
	const noCtx = plain(
		A.model.node(
			Object.assign({}, NODE_RES, {
				context: undefined,
			}),
		),
	);
	assert.equal(noCtx.context, null);
	assert.equal(noCtx.namespaceVars, null);
	assert.equal(noCtx.effectiveVars, null);

	const empty = plain(A.model.node({}));
	assert.equal(empty.pending, false);
	assert.deepEqual(empty.machineVars, {});
	assert.deepEqual(empty.effectiveNamespaces, []);
	assert.equal(empty.context, null);
	assert.equal(empty.namespaceVars, null);
	assert.equal(empty.effectiveVars, null);
});

test("model.edge：只留 v4 的 from/to/kind，v3 的 fromIsSource/position/port 一律丢掉", () => {
	const { A } = loadAnotify();
	const m = plain(
		A.model.edge({
			from: "hook",
			to: "fixer@3",
			kind: "source",
			// v3 残留字段：模型必须丢掉（也不改名映射成 v4 字段）
			fromIsSource: true,
			fromRef: "hook",
			toRef: "fixer@3",
			position: 2,
			port: "out",
			priority: 5,
			mapping: { a: "b" },
		}),
	);
	assert.deepEqual(m, { from: "hook", to: "fixer@3", kind: "source" });
	for (const gone of ["fromIsSource", "fromRef", "toRef", "position", "port", "priority", "mapping"]) {
		assert.equal(gone in m, false, `v3 字段 ${gone} 不得出现在 edge 模型里`);
	}
	// kind 是封闭枚举：不认识的值给 null，不猜成 capability/source。
	assert.equal(plain(A.model.edge({ from: "a", to: "b", kind: "trigger" })).kind, null);

	const empty = plain(A.model.edge({}));
	assert.equal(empty.from, null);
	assert.equal(empty.to, null);
	assert.equal(empty.kind, null);
});

test("model.event：v4 信封字段 deliveryId/dispatched/pruned（W6 console 同信封）", () => {
	const { A } = loadAnotify();
	const envelope = plain(
		A.model.event({
			apiVersion: "anotify/v1",
			kind: "Event",
			metadata: { name: "ev-1", namespace: "default" },
			spec: {
				reason: "Ingested",
				type: "Normal",
				involvedObject: { kind: "Source", name: "hook", uid: "u1" },
				traceUID: "tr-1",
				eventType: "webhook",
				deliveryId: "d-1",
				payload: { repo: "a/b" },
				signature: { algorithm: "hmac-sha256", verified: true },
			},
			status: {
				dispatched: [{ capability: "fixer", state: "ok" }],
				pruned: { reason: "param_missing", detail: "需要 file" },
				deliveredAt: "2024-01-01T00:00:00Z",
			},
		}),
	);
	assert.equal(envelope.name, "ev-1");
	assert.equal(envelope.reason, "Ingested");
	assert.equal(envelope.type, "Normal");
	assert.equal(envelope.involvedObject.kind, "Source");
	assert.equal(envelope.deliveryId, "d-1");
	assert.equal(envelope.traceUID, "tr-1");
	assert.equal(envelope.signature.verified, true);
	assert.equal(envelope.dispatched.length, 1);
	// pruned 是对象（playbook/event 语义：{reason,detail}）或 null，不是计数
	assert.deepEqual(envelope.pruned, { reason: "param_missing", detail: "需要 file" });
	assert.equal(envelope.deliveredAt, "2024-01-01T00:00:00Z");

	const row = plain(
		A.model.event({
			reason: "Emitted",
			type: "Info",
			deliveryId: "d-2",
			pruned: null,
			dispatched: [],
		}),
	);
	// W6 supersedes the old flat console example; explicitly reject it.
	assert.equal(row.reason, null);
	assert.equal(row.deliveryId, null);
	assert.equal(row.pruned, null);
	assert.deepEqual(row.dispatched, []);
});

test("model.trace：{events,tasks,pruned} 缺省 []", () => {
	const { A } = loadAnotify();
	const m = plain(
		A.model.trace({
			traceUID: "tr-1",
			events: [{ name: "ev-1" }],
			tasks: [{ name: "task-1" }],
			pruned: [{ reason: "no_target" }],
		}),
	);
	assert.equal(m.traceUID, "tr-1");
	assert.equal(m.events.length, 1);
	assert.equal(m.tasks.length, 1);
	assert.equal(m.pruned[0].reason, "no_target");

	const empty = plain(A.model.trace({}));
	assert.deepEqual(empty.events, []);
	assert.deepEqual(empty.tasks, []);
	assert.deepEqual(empty.pruned, []);
});

/* ------------------------------------------------------------------ *
 * 保留导出（UI 层不能被 v4 改造误删）
 * ------------------------------------------------------------------ */

test("保留的 UI helper 仍在（el/t/toast/mountLayout/openModal/theme…）", () => {
	const { A } = loadAnotify();
	for (const name of [
		"el",
		"icon",
		"mountLayout",
		"mountPullRefresh",
		"openModal",
		"closeModal",
		"toast",
		"t",
		"loadErrorNode",
		"copyText",
		"timeAgo",
		"loadMe",
		"logout",
		"mountLangHint",
		"mountLoginLangSwitcher",
		"ready",
		"nsApi",
		"nsList",
	]) {
		assert.equal(typeof A[name], "function", `${name} 必须仍是函数`);
	}
	assert.equal(typeof A.theme, "object");
	assert.equal(typeof A.theme.cycle, "function");
	assert.equal(typeof A.api, "function");
	assert.equal(A.t("nope.key", "fallback"), "fallback");
});

/* ================================================================== *
 * F1b · web/mock.js 的 v4 端点
 *
 * 这一节与上面不同：F1a 测的是纯 helper（喂样例对象），这里**真的发请求** ——
 * 在同一个 vm 里加载 partials.js + mock.js，mock 会替换 window.fetch，
 * 于是每条断言背后都是一次真实的路由匹配 + 状态派生。
 * ================================================================== */

const CAP_LIST = "/v1/namespaces/default/capabilities";
const itemByName = (items, name) =>
	items.find((x) => x.metadata && x.metadata.name === name);

const jsonInit = (method, obj) => ({
	method,
	body: obj === undefined ? undefined : JSON.stringify(obj),
});

test("mock: capability 列表是 CapabilityList 信封，status 从事实派生（四条件 + nodes/resolved/observed）", async () => {
	const { req } = loadMock();
	const { status, body } = await req(CAP_LIST);

	assert.equal(status, 200);
	assert.equal(body.apiVersion, "anotify/v1");
	assert.equal(body.kind, "CapabilityList");
	assert.ok(Array.isArray(body.items), "items 必须是数组");
	assert.ok(body.items.length >= 8);

	const fixer = itemByName(body.items, "fixer");
	assert.equal(fixer.kind, "Capability");
	assert.equal(fixer.metadata.namespace, "default");
	assert.equal(fixer.metadata.version, 1);
	assert.ok(fixer.metadata.resourceVersion, "必须带 resourceVersion（CAS 令牌）");
	assert.deepEqual(fixer.spec.next, ["analyze-pr", "lint-fix@1"]);

	// 四个 condition，type 与 reason 都落在 v4 词表里
	assert.deepEqual(
		fixer.status.conditions.map((c) => c.type),
		["Implemented", "NodesAvailable", "DownstreamResolved", "Deployed"],
	);
	const cond = (r, t) => r.status.conditions.find((c) => c.type === t);
	assert.equal(cond(fixer, "Implemented").reason, "Valid");
	assert.equal(cond(fixer, "NodesAvailable").reason, "Valid");
	assert.equal(cond(fixer, "DownstreamResolved").reason, "Valid");
	// anode-laptop 上还是旧 digest → Stale，ready 必须为 false
	assert.equal(cond(fixer, "Deployed").reason, "Stale");
	assert.equal(cond(fixer, "Deployed").status, "False");
	assert.equal(fixer.status.ready, false);
	assert.match(cond(fixer, "Deployed").message, /旧 digest/);

	// nodes[]：候选节点 × deployments 派生的 deployed/stale/missing
	const nodeState = (r, name) => r.status.nodes.find((n) => n.name === name);
	assert.equal(nodeState(fixer, "anode-1").state, "deployed");
	assert.equal(nodeState(fixer, "anode-laptop").state, "stale");
	assert.equal(nodeState(fixer, "anode-offline"), undefined, "未授权的 ns 不该出现在候选里");

	// resolved.next 是解析结果（含版本与 required），不是原始引用
	const next = fixer.status.resolved.next;
	assert.equal(next[0].name, "analyze-pr");
	assert.equal(next[0].version, 2);
	assert.equal(next[0].missing, false);
	assert.equal(next[1].name, "lint-fix");

	// observed：明细数组（不是只给计数），来自 mock 的 emissions / prunes / tasks
	assert.equal(fixer.status.observed.taskId, "tsk_mock_1");
	assert.equal(fixer.status.observed.emits.length, 2);
	assert.equal(fixer.status.observed.emits[0].to, "analyze-pr");
	assert.deepEqual(fixer.status.observed.tasks, { total: 1, failed: 0 });

	// 派生的各种 reason 都在种子里覆盖到（换页面演示时不用现造数据）
	const reasonOf = (name, type) => cond(itemByName(body.items, name), type).reason;
	assert.equal(reasonOf("lint-fix", "NodesAvailable"), "ConfigPending");
	assert.equal(reasonOf("deploy-preview", "NodesAvailable"), "ValueMissing");
	assert.equal(reasonOf("archive", "NodesAvailable"), "CommandMissing");
	assert.equal(reasonOf("sketch", "Implemented"), "Invalid");
	assert.equal(itemByName(body.items, "run-tests").status.ready, true);
	assert.equal(
		byNameOrFail(body.items, "run-tests").status.conditions.find((c) => c.type === "Deployed")
			.reason,
		"Valid",
	);

	const prod = await req("/v1/namespaces/prod/capabilities");
	assert.equal(
		itemByName(prod.body.items, "report.send").status.conditions.find(
			(c) => c.type === "DownstreamResolved",
		).reason,
		"NotFound",
	);
	assert.equal(
		itemByName(prod.body.items, "report.send").status.resolved.next[0].missing,
		true,
	);
	assert.equal(
		itemByName(prod.body.items, "prod-deploy").status.conditions.find(
			(c) => c.type === "NodesAvailable",
		).reason,
		"NodeOffline",
	);
	// observed.pruned：最近一个 task 的剪枝留痕（明细，不是计数）
	assert.equal(
		itemByName(prod.body.items, "prod-deploy").status.observed.pruned[0].reason,
		"param_missing",
	);

	const team = await req("/v1/namespaces/team-a/capabilities");
	assert.equal(
		itemByName(team.body.items, "team-task").status.conditions.find(
			(c) => c.type === "NodesAvailable",
		).reason,
		"NamespaceUnauthorized",
	);
	assert.equal(
		itemByName(team.body.items, "team-missing").status.conditions.find(
			(c) => c.type === "NodesAvailable",
		).reason,
		"NoMatchingNode",
	);

	// partials.js 的严格列表 + 模型层吃得下同一份响应
	let listed = null;
	const { req: req2, A } = loadMock();
	listed = await A.apiList(CAP_LIST);
	assert.equal(listed.items.length, body.items.length);
	assert.deepEqual(plain(listed.warnings), []);
	const model = plain(A.model.capability(listed.items.find((c) => c.metadata.name === "fixer")));
	assert.equal(model.name, "fixer");
	assert.equal(model.conditions.length, 4);
	assert.equal(model.resolvedNext[0].name, "analyze-pr");
	assert.equal(model.observed.emits.length, 2);
	void req2;
});

function byNameOrFail(items, name) {
	const hit = itemByName(items, name);
	assert.ok(hit, "种子里必须有 " + name);
	return hit;
}

test("mock: capability POST 内容寻址（同内容复用同一版本行）；PATCH 只接 displayName/next，改内容 400 immutable_field；DELETE 204", async () => {
	const { req } = loadMock();
	const list = await req(CAP_LIST);
	const fixer = byNameOrFail(list.body.items, "fixer");
	const rv = fixer.metadata.resourceVersion;
	const one = CAP_LIST + "/fixer";

	// POST 同名**不再是** 409 AlreadyExists：v4 的 POST 是内容寻址
	// （store.createCapabilityTx）——同内容复用那一行，新内容分配 max+1。
	const reuse = await req(CAP_LIST, jsonInit("POST", {
		apiVersion: "anotify/v1",
		kind: "Capability",
		metadata: { name: "fixer" },
		spec: plain(fixer.spec),
	}));
	assert.equal(reuse.status, 201);
	assert.equal(reuse.body.kind, "Capability");
	assert.equal(reuse.body.metadata.version, fixer.metadata.version, "同内容 POST 复用同一版本行");

	const noRv = await req(one, jsonInit("PATCH", {
		apiVersion: "anotify/v1",
		kind: "Capability",
		metadata: { name: "fixer" },
		spec: { next: [] },
	}));
	assert.equal(noRv.status, 400);
	assert.equal(noRv.body.reason, "resource_version_required");

	const stale = await req(one, jsonInit("PATCH", {
		apiVersion: "anotify/v1",
		kind: "Capability",
		metadata: { name: "fixer", resourceVersion: "1" },
		spec: { next: [] },
	}));
	assert.equal(stale.status, 409);
	assert.equal(stale.body.reason, "resource_version_conflict");
	assert.equal(stale.body.details.current.resourceVersion, rv, "409 必须带回当前 resourceVersion");

	// PATCH 的**合法面**只有 metadata.displayName 与 spec.next。
	const ok = await req(one, jsonInit("PATCH", {
		apiVersion: "anotify/v1",
		kind: "Capability",
		metadata: { name: "fixer", resourceVersion: rv },
		spec: { next: ["notify"] },
	}));
	assert.equal(ok.status, 200);
	assert.deepEqual(ok.body.spec.next, ["notify"]);
	assert.equal(ok.body.metadata.version, fixer.metadata.version, "next 不是内容：不换版本，也不换 digest");
	assert.equal(ok.body.status.digest, fixer.status.digest, "digest 不含 next");
	assert.notEqual(ok.body.metadata.resourceVersion, rv, "写成功必须换新的 resourceVersion");
	const rv2 = ok.body.metadata.resourceVersion;

	// 内容面不可变（resource_capability.go·immutableCapabilitySpecPatch）：
	// 写 run/params 一律 400 immutable_field —— 静默忽略会让人以为改生效了。
	const immutable = await req(one, jsonInit("PATCH", {
		apiVersion: "anotify/v1",
		kind: "Capability",
		metadata: { name: "fixer", resourceVersion: rv2 },
		spec: Object.assign({}, plain(ok.body.spec), {
			run: { entrypoint: "run.sh", files: { "run.sh": "#!/bin/sh\necho v2\n" } },
		}),
	}));
	assert.equal(immutable.status, 400);
	assert.equal(immutable.body.reason, "immutable_field");
	assert.match(immutable.body.message, /spec\.run/);
	assert.deepEqual((await req(one)).body, ok.body, "immutable_field 拒绝必须零写");

	const withStatus = await req(one, jsonInit("PATCH", {
		apiVersion: "anotify/v1",
		metadata: { name: "fixer", resourceVersion: rv2 },
		spec: {},
		status: { ready: true },
	}));
	assert.equal(withStatus.status, 400);
	assert.equal(withStatus.body.reason, "status_not_writable");

	// 跨命名空间写同一行：kind / namespace 不匹配都要明确拒绝
	const kindBad = await req(one, jsonInit("PATCH", {
		apiVersion: "anotify/v1",
		kind: "Source",
		metadata: { name: "fixer", resourceVersion: rv2 },
		spec: {},
	}));
	assert.equal(kindBad.status, 400);
	assert.equal(kindBad.body.reason, "kind_mismatch");

	const del = await req(one, { method: "DELETE" });
	assert.equal(del.status, 400);
	assert.equal(del.body.reason, "resource_version_required");

	// DELETE 成功是 204 无响应体（resource_capability.go），不是 200+Status
	const delOk = await req(one + "?resourceVersion=" + encodeURIComponent(rv2), {
		method: "DELETE",
	});
	assert.equal(delOk.status, 204);
	assert.equal(delOk.body, null, "204 不得带回响应体");
	const after = await req(CAP_LIST);
	assert.equal(itemByName(after.body.items, "fixer"), undefined, "删掉之后列表里就没有了");
});

test("mock: capability 版本列表 / 单版本 ?includeBody、人工 deploy 写 deployments 事实", async () => {
	const { req } = loadMock();
	const versions = await req(CAP_LIST + "/analyze-pr/versions");
	assert.equal(versions.status, 200);
	assert.equal(versions.body.kind, "CapabilityList", "versions 不是独立 kind：列表仍是 CapabilityList");
	assert.deepEqual(
		versions.body.items.map((v) => v.metadata.version),
		[2, 1],
		"新→旧（metadata.version）",
	);
	assert.ok(
		versions.body.items.every((v) => v.kind === "Capability"),
		"每个版本项都是同一个 Capability 信封",
	);
	assert.ok(
		versions.body.items.every((v) => !("current" in v)),
		"wire 不自造 item.current；当前 = max(metadata.version)",
	);
	assert.equal(Math.max(...versions.body.items.map((v) => v.metadata.version)), 2);

	const v1 = await req(CAP_LIST + "/analyze-pr/versions/1?includeBody=true");
	assert.equal(v1.status, 200);
	assert.equal(v1.body.kind, "Capability", "单版本也是 Capability 信封");
	assert.equal(v1.body.metadata.version, 1);
	assert.ok(v1.body.status.digest, "单版本带 status.digest");
	assert.ok(v1.body.spec.run.files["main.sh"].includes("v1"), "includeBody=true 才带脚本全文");

	const noBody = await req(CAP_LIST + "/analyze-pr/versions/1");
	assert.equal(noBody.body.kind, "Capability");
	assert.equal(noBody.body.metadata.version, 1);
	assert.equal(noBody.body.spec.run.files, undefined, "默认不带正文（run.files 省略）");

	const deployed = await req(CAP_LIST + "/analyze-pr/deploy", jsonInit("POST", { nodes: ["anode-2"] }));
	assert.equal(deployed.status, 201);
	assert.equal(deployed.body.kind, "DeploymentList");
	const hit = deployed.body.items.find((d) => d.node === "anode-2");
	assert.ok(hit);
	assert.equal(hit.capability, "analyze-pr");
	assert.equal(hit.version, 2);
	assert.equal(hit.state, "deployed");

	// 部署事实写完，能力的 nodes[] 立刻跟着变（status 是读时派生的，没有副本）
	const detail = await req(CAP_LIST + "/analyze-pr");
	const anode2 = detail.body.status.nodes.find((n) => n.name === "anode-2");
	assert.equal(anode2.state, "deployed");
});

test("mock: source GET 投影只有 hookUrl/secretSet（无 token/secret），PATCH 省略 token 保留已有", async () => {
	const { req } = loadMock();
	const list = await req("/v1/namespaces/default/sources");
	assert.equal(list.body.kind, "SourceList");
	const hook = byNameOrFail(list.body.items, "deploy-hook");
	assert.equal(hook.spec.kind, "webhook");
	assert.equal(hook.spec.config.hookUrl, "/v1/hooks/src_mock_deploy_hook");
	assert.equal(hook.spec.config.secretSet, true);
	assert.equal("token" in hook.spec.config, false, "GET 不得回显 token");
	assert.equal("secret" in hook.spec.config, false, "GET 不得回显 secret");
	assert.equal(JSON.stringify(list.body).includes("hmac_mock_secret_deploy_hook"), false);
	// api-conventions §7.2：targetsResolved 是 boolean，不是解析后的数组
	assert.equal(hook.status.targetsResolved, true);
	assert.equal(typeof hook.status.targetsResolved, "boolean");
	assert.equal(hook.status.health, "ok");

	// 三种 kind 都在种子里
	const kinds = list.body.items.map((s) => s.spec.kind);
	assert.ok(kinds.includes("cron"));
	assert.ok(kinds.includes("webhook"));
	assert.ok(kinds.includes("manual"));
	const nightly = byNameOrFail(list.body.items, "nightly");
	assert.equal(nightly.spec.config.expr, "0 3 * * *");
	assert.equal(nightly.spec.config.tz, "UTC");
	assert.ok(nightly.status.nextFireAt, "cron 才有 nextFireAt");
	assert.equal(byNameOrFail(list.body.items, "paused-hook").status.health, "disabled");

	// 没有 target 的源（prod-noop）：targetsResolved 是 false（boolean 口径）
	const prodList = await req("/v1/namespaces/prod/sources");
	assert.equal(
		byNameOrFail(prodList.body.items, "prod-noop").status.targetsResolved,
		false,
	);

	// PATCH 不写 token = 保留已有（hookUrl 不变）
	const patched = await req("/v1/namespaces/default/sources/deploy-hook", jsonInit("PATCH", {
		kind: "Source",
		metadata: { name: "deploy-hook", resourceVersion: hook.metadata.resourceVersion },
		spec: {
			kind: "webhook",
			enabled: true,
			config: { secretSet: false, payloadSchema: { type: "object" } },
			targets: [{ capability: "fixer", params: { repo: "$(payload.repo)" } }],
		},
	}));
	assert.equal(patched.status, 200);
	assert.equal(patched.body.spec.config.hookUrl, "/v1/hooks/src_mock_deploy_hook");
	assert.equal("token" in patched.body.spec.config, false);

	// 换 kind 拒绝
	const kindChange = await req("/v1/namespaces/default/sources/deploy-hook", jsonInit("PATCH", {
		kind: "Source",
		metadata: { name: "deploy-hook", resourceVersion: patched.body.metadata.resourceVersion },
		spec: { kind: "cron", config: { expr: "0 0 * * *" }, targets: [] },
	}));
	assert.equal(kindChange.status, 400);
	assert.equal(kindChange.body.reason, "source_kind_immutable");

	// 新建 webhook 不给 token：服务端生成，响应里仍然只有 hookUrl
	const created = await req("/v1/namespaces/default/sources", jsonInit("POST", {
		kind: "Source",
		metadata: { name: "new-hook" },
		spec: { kind: "webhook", enabled: true, config: {}, targets: [] },
	}));
	assert.equal(created.status, 201);
	assert.match(created.body.spec.config.hookUrl, /^\/v1\/hooks\/src_mock_new-hook/);
	assert.equal("token" in created.body.spec.config, false);
	assert.equal(created.body.status.health, "blocked", "没有 target 的源是 blocked，不是 ok");
	assert.equal(created.body.status.targetsResolved, false, "没有 target 不算“已解析”");

	// 配置期校验：源参数里写 $(vars.…) 是错误
	const badTmpl = await req("/v1/namespaces/default/sources", jsonInit("POST", {
		kind: "Source",
		metadata: { name: "bad-tmpl" },
		spec: {
			kind: "manual",
			enabled: true,
			config: {},
			targets: [{ capability: "fixer", params: { repo: "$(vars.SOURCE_ROOT)" } }],
		},
	}));
	assert.equal(badTmpl.status, 400);
	assert.equal(badTmpl.body.reason, "invalid_source_config");
});

test("mock: Source W6 fire / saved+draft preview: exact receipts and zero persistence", async () => {
 const {req, Mock} = loadMock();
 const before = JSON.stringify(Mock.state);
 const path = "/v1/namespaces/default/sources/deploy-hook";
 const payload = {repo:"acme/api", environment:"staging"};
 const dry = await req(path+"/dry-run", jsonInit("POST", {payload}));
 assert.equal(dry.status,200);
 assert.deepEqual(Object.keys(dry.body).sort(),["pruned","targets"]);
 assert.equal(dry.body.targets[0].state,"ready");
 assert.equal(dry.body.targets[0].params.repo,"acme/api");
 assert.equal(dry.body.targets[0].params.environment,"staging");
 assert.equal(JSON.stringify(Mock.state), before, "preview: no source/RV/event/task/dedup write");
 const fire = await req(path+"/fire", jsonInit("POST", {payload}));
 assert.equal(fire.status,200);
 assert.deepEqual(Object.keys(fire.body).sort(),["duplicate","eventId","pruned","tasks","traceId"]);
 assert.equal(fire.body.duplicate,false);
 assert.ok(fire.body.eventId && fire.body.traceId);
 assert.equal(fire.body.tasks.length,1);
 assert.deepEqual(Object.keys(fire.body.tasks[0]).sort(),["capability","taskId","version"]);
 assert.equal(fire.body.tasks[0].capability,"fixer");
 assert.deepEqual(fire.body.pruned,[]);
 const task = await req("/v1/namespaces/default/tasks/"+fire.body.tasks[0].taskId);
 assert.equal(task.body.status.phase,"pending", "acceptance does not imply success");
 const event = (await req("/v1/namespaces/default/events")).body.items.find(e=>e.metadata.name===fire.body.eventId);
 assert.equal(event.spec.reason,"Ingested");
 assert.equal(event.spec.traceUID,fire.body.traceId);
 assert.equal(event.spec.involvedObject.name,"deploy-hook");
 assert.deepEqual(event.spec.payload,payload);
 const schema = await req(path+"/fire", jsonInit("POST",{payload:{environment:"prod"}}));
 assert.equal(schema.status,200);
 assert.equal(schema.body.tasks.length,0);
 assert.equal(schema.body.pruned[0].reason,"event_schema");
 const param = await req("/v1/namespaces/default/sources/nightly/fire",jsonInit("POST",{payload:{suite:"smoke"}}));
 assert.equal(param.body.tasks.length,0);
 assert.equal(param.body.pruned[0].reason,"param_schema");
 assert.equal(param.body.pruned[0].capability,"run-tests");
 const noop=await req("/v1/namespaces/prod/sources/prod-noop/fire",jsonInit("POST",{}));
 assert.equal(noop.status,200); assert.ok(noop.body.eventId && noop.body.traceId);
 assert.equal(noop.body.pruned[0].reason,"no_target");
 const snapshot=JSON.stringify(Mock.state);
 const draft={apiVersion:"anotify/v1",kind:"Source",metadata:{name:"draft"},spec:{kind:"webhook",enabled:true,config:{},targets:[{capability:"fixer",params:{repo:"$(payload.repo)"}},{capability:"unknown",params:{}}]}};
 const preview=await req("/v1/namespaces/default/sources/dry-run",jsonInit("POST",{source:draft,payload:{}}));
 assert.equal(preview.status,200);
 assert.deepEqual(preview.body.targets.map(t=>t.state),["pruned","missing"]);
 assert.equal(preview.body.targets[0].reason,"param_missing");
 assert.match(preview.body.targets[0].detail,/repo/);
 assert.equal(preview.body.targets[1].reason,"no_target");
 assert.equal(JSON.stringify(Mock.state),snapshot,"draft preview must make zero writes");
});


test("mock: node 列表/详情是 v4 信封；?namespace= 带 context/namespaceVars/effectiveVars", async () => {
	const { req, A } = loadMock();
	const list = await req("/v1/nodes");
	assert.equal(list.body.kind, "NodeList");
	assert.ok(list.body.items.length >= 4);
	const n1 = byNameOrFail(list.body.items, "anode-1");
	assert.equal(n1.kind, "Node");
	assert.equal(n1.metadata.namespace, undefined, "Node 是 cluster-scoped，没有 namespace");

	const st = n1.status;
	for (const key of [
		"hostIP",
		"hostname",
		"os",
		"arch",
		"capacity",
		"workRoot",
		"homeDir",
		"localVars",
		"nodeVars",
		"machineVars",
		"configRevision",
		"appliedRevision",
		"appliedHash",
		"effectiveNamespaces",
		"agentVersion",
		"instances",
		"lastSeenAt",
	]) {
		assert.ok(key in st, "Node.status 缺字段 " + key);
	}
	assert.equal(st.localVars.SOURCE_ROOT, "/srv/src");
	assert.equal(st.nodeVars.TOOLCHAIN, "/opt/go1.25");
	assert.equal(st.machineVars.SOURCE_ROOT, "/srv/src", "machineVars = node ∪ local（local 压住）");
	assert.deepEqual(st.effectiveNamespaces, ["default", "prod"]);
	assert.equal("namespaceVars" in st, false, "没有 ?namespace= 就不该有 ns 上下文");
	assert.equal("effectiveVars" in st, false, "不许给一份扁平 effectiveVars 假装覆盖全部 ns");

	// ConfigPending 的节点：appliedRevision 落后 configRevision
	const n2 = byNameOrFail(list.body.items, "anode-2");
	assert.notEqual(n2.status.appliedRevision, n2.status.configRevision);
	assert.equal(plain(A.model.node(n2)).pending, true);

	// 授权交集为空：本机申报 default，中枢授权 team-a
	const n3 = byNameOrFail(list.body.items, "anode-3");
	assert.deepEqual(n3.status.effectiveNamespaces, []);

	const detail = await req("/v1/nodes/anode-1?namespace=prod");
	assert.equal(detail.status, 200);
	// v4：读上下文在信封顶层（Node.context），有效值在 status.effectiveVars
	assert.equal(detail.body.context.namespace, "prod");
	assert.equal(detail.body.status.namespaceVars.SLACK_CHANNEL, "#prod");
	assert.equal(detail.body.status.effectiveVars.NOTIFY_URL.value, "https://hooks.example/notify/prod");
	assert.equal(detail.body.status.effectiveVars.NOTIFY_URL.source, "namespace");
	assert.equal(detail.body.status.machineVars.TOOLCHAIN, "/opt/go1.25");

	const missingNs = await req("/v1/nodes/anode-1?namespace=nope");
	assert.equal(missingNs.status, 404);
	assert.equal(missingNs.body.kind, "Status");

	const missingNode = await req("/v1/nodes/ghost");
	assert.equal(missingNode.status, 404);
});

test("mock: task 只读列表/详情（phase 小写、attempt、waitingReason、envDiagnostics 无值）+ cancel/abandon", async () => {
	const { req } = loadMock();
	const list = await req("/v1/namespaces/default/tasks");
	assert.equal(list.body.kind, "TaskList");
	const names = list.body.items.map((t) => t.metadata.name);
	for (const n of ["tsk_mock_1", "tsk_mock_5", "tsk_mock_6", "tsk_mock_7"]) {
		assert.ok(names.includes(n), "种子任务缺 " + n);
	}
	const phases = list.body.items.map((t) => t.status.phase);
	for (const p of ["pending", "running", "succeeded", "failed"]) {
		assert.ok(phases.includes(p), "种子任务缺 phase " + p);
	}

	const t5 = await req("/v1/namespaces/default/tasks/tsk_mock_5");
	assert.equal(t5.body.kind, "Task");
	assert.equal(t5.body.status.phase, "pending");
	assert.equal(t5.body.status.waitingReason, "retry_backoff");
	assert.ok(t5.body.status.nextAttemptAt, "waitingReason 非空时必须有 nextAttemptAt");
	assert.equal(t5.body.status.attempt, 2);
	assert.equal(t5.body.status.attempts.length, 1, "attempt>1 时要有 attempt 历史");

	const t4 = await req("/v1/namespaces/default/tasks/tsk_mock_4");
	assert.equal(t4.body.status.phase, "failed");
	assert.equal(t4.body.status.attempt, 3);
	assert.equal(t4.body.status.attempts.length, 3);
	assert.equal(t4.body.status.result.error.reason, "max_attempts_exceeded");

	// envDiagnostics：只有名字 + 来源，五类来源都在种子里
	const t1 = await req("/v1/namespaces/default/tasks/tsk_mock_1");
	const sources = t1.body.status.envDiagnostics.map((e) => e.source);
	assert.deepEqual(sources.sort(), ["builtin", "local", "namespace", "node", "passthrough"]);
	assert.equal(keysDeep(t1.body.status.envDiagnostics).has("value"), false, "不得回显任何值");

	// W6/A1: actual archive lines; unavailable node is an error, not empty success.
	const log = await req("/v1/namespaces/default/tasks/tsk_mock_2/logs");
	assert.equal(log.body.taskId, "tsk_mock_2");
	assert.equal(log.body.attempt, 1);
	assert.match(log.body.lines.map(line => line.text).join("\n"), /issues found/);
	assert.equal(log.body.truncated, true);
	const noLog = await req("/v1/namespaces/prod/tasks/tsk_mock_10/logs");
	assert.equal(noLog.status, 503);
	assert.equal(noLog.body.reason, "node_offline");

	// W6 emissions keeps the same two acceptance facts under the canonical wrapper.
	const em = await req("/v1/namespaces/default/tasks/tsk_mock_1/emissions");
	assert.equal(em.body.metadata.continue, "");
	assert.equal(em.body.items.length, 2);
	assert.equal(em.body.items[0].to, "analyze-pr");
	assert.equal(em.body.items[0].taskId, "tsk_mock_2");

	// 取消 / 放弃（终态不能再动）
	const cancelled = await req("/v1/namespaces/default/tasks/tsk_mock_7/cancel", jsonInit("POST", {}));
	assert.equal(cancelled.status, 200);
	assert.equal(cancelled.body.status.phase, "cancelled");
	assert.equal(cancelled.body.status.result.error.reason, "cancelled_by_user");
	const again = await req("/v1/namespaces/default/tasks/tsk_mock_7/cancel", jsonInit("POST", {}));
	assert.equal(again.status, 409);
	assert.equal(again.body.reason, "Conflict");

	const abandoned = await req("/v1/namespaces/default/tasks/tsk_mock_5/abandon", jsonInit("POST", {}));
	assert.equal(abandoned.body.status.phase, "abandoned");

	// Task 只读：不能 POST 创建
	const create = await req("/v1/namespaces/default/tasks", jsonInit("POST", { kind: "Task" }));
	assert.equal(create.status, 405);
	assert.equal(create.body.kind, "Status");
});

test("mock: edges / deployments 是只读投影（无 port/priority）；trace 是 {events,tasks,pruned}", async () => {
	const { req } = loadMock();
	const edges = await req("/v1/namespaces/default/edges");
	// 契约：`{items}` 文档，没有 apiVersion/kind（resource.yaml · EdgeList）。
	assert.deepEqual(Object.keys(edges.body), ["items"]);
	const e = edges.body.items[0];
	// v4 CapabilityEdge：wire 只有 from/to/kind（enum capability|source）。
	assert.deepEqual(constKeys(e), ["from", "kind", "to"]);
	assert.equal("port" in e, false);
	assert.equal("priority" in e, false);
	assert.ok(edges.body.items.some((x) => x.kind === "source" && x.from === "deploy-hook"));
	assert.ok(
		edges.body.items.some(
			(x) => x.kind === "capability" && x.from === "fixer" && x.to === "analyze-pr",
		),
		"能力边来自 spec.next",
	);

	const deps = await req("/v1/namespaces/default/deployments");
	assert.equal(deps.body.kind, "DeploymentList");
	const d = deps.body.items.find((x) => x.capability === "fixer" && x.node === "anode-1");
	assert.ok(d);
	assert.equal(d.kind, "Deployment");
	assert.equal(d.spec.capability.name, "fixer");
	assert.equal(d.node, "anode-1");
	assert.ok(d.digest.startsWith("sha256:"));
	assert.equal(d.state, "deployed");
	assert.equal(
		deps.body.items.find((x) => x.node === "anode-laptop" && x.capability === "fixer").state,
		"stale",
	);

	const trace = await req("/v1/namespaces/default/traces/trace_mock_t1");
	assert.equal(trace.status, 200);
	assert.equal(trace.body.kind, "Trace");
	assert.equal(trace.body.traceUID, "trace_mock_t1");
	assert.ok(trace.body.tasks.length >= 5);
	assert.ok(trace.body.events.length >= 3);
	assert.equal(trace.body.pruned[0].reason, "param_schema");
	assert.ok(trace.body.tasks.every((t) => t.kind === "Task"), "trace 里的任务是资源信封");

	const absent = await req("/v1/namespaces/default/traces/nope");
	assert.equal(absent.status, 404);
});

function constKeys(obj) {
	return Object.keys(obj).sort();
}

test("mock: 已删除的 v3 端点返回 404 Status（不是 501，也不是静默成功）", async () => {
	const { req } = loadMock();
	const cases = [
		["PUT", "/v1/namespaces/default/capabilities/fixer/downstreams"],
		["POST", "/v1/edges"],
		["DELETE", "/v1/edges"],
		["POST", "/v1/namespaces/default/edges"],
		["DELETE", "/v1/namespaces/default/edges"],
		["GET", "/v1/namespaces/default/capabilities/fixer/artifact"],
		["GET", "/v1/namespaces/default/capabilities/fixer/revisions"],
		["POST", "/v1/namespaces/default/deployments"],
		["DELETE", "/v1/namespaces/default/deployments"],
		["GET", "/v1/capabilities"],
		["GET", "/v1/sources"],
		["GET", "/v1/tasks"],
		["GET", "/v1/playbooks"],
		["GET", "/v1/edges"],
		["GET", "/v1/deployments"],
		["GET", "/v1/traces/t1"],
		["POST", "/v1/events"],
		["GET", "/v1/orgs"],
	];
	for (const [method, path] of cases) {
		const res = await req(path, { method });
		assert.equal(res.status, 404, method + " " + path + " 必须 404");
		assert.equal(res.body.kind, "Status", method + " " + path + " 错误体必须是 Status");
		assert.equal(res.body.reason, "NotFound");
		assert.ok(res.body.message, "要给出可显示的人话");
	}
});

test("mock: 真正没实现的新接口仍是 501 + 既有 warn 文案（e2e 门禁文案不能改）", async () => {
	const warns = [];
	const { req, sandbox } = loadMock();
	sandbox.console = Object.assign({}, console, {
		warn: (...args) => warns.push(args.join(" ")),
	});
	const res = await req("/v1/orphans?namespace=default");
	assert.equal(res.status, 501);
	assert.ok(
		warns.some((w) => w.includes("mock 未实现")),
		"warn 文案必须含 'mock 未实现'（e2e 靠这个子串判定覆盖）",
	);
});

test("mock: 保留域仍可用（auth/me、devices、keys、admin/stats、cli-auth、templates、vapid）", async () => {
	const { req } = loadMock();
	const me = await req("/v1/auth/me");
	assert.equal(me.status, 200);
	assert.equal(me.body.username, "demo");

	const devices = await req("/v1/devices");
	assert.equal(devices.status, 200);
	assert.deepEqual(devices.body.devices, []);

	const keys = await req("/v1/keys");
	assert.equal(keys.body.keys.length, 2);
	assert.equal(keys.body.keys[0].enabled, true);

	const created = await req("/v1/keys", jsonInit("POST", { name: "新 key", scopes: ["send"] }));
	assert.equal(created.status, 200);
	assert.equal(typeof created.body.key, "string", "key 是明文串（只返回一次）");
	assert.equal(created.body.record.name, "新 key");

	const admin = await req("/v1/admin/stats");
	assert.equal(admin.body.nodes, 5);
	assert.ok(admin.body.tasks > 0);

	const cli = await req("/v1/cli-auth/sessions/by-code?code=MOCK-1234");
	assert.equal(cli.body.session.code, "MOCK-1234");

	// passkey 设备注册会话（保留域）：状态机 pending → requested → approved
	const created2 = await req("/v1/passkey-enroll/sessions", jsonInit("POST", { deviceName: "cli" }));
	assert.equal(created2.status, 201);
	assert.ok(created2.body.sessionId);
	assert.ok(created2.body.userCode);
	const byId = await req("/v1/passkey-enroll/sessions/" + created2.body.sessionId);
	assert.equal(byId.body.status, "pending");
	const byCode = await req("/v1/passkey-enroll/sessions/by-code?code=" + created2.body.userCode);
	assert.equal(byCode.body.sessionId, created2.body.sessionId);
	const reqd = await req(
		"/v1/passkey-enroll/sessions/" + created2.body.sessionId + "/request",
		jsonInit("POST", { deviceHint: "Chrome on Linux" }),
	);
	assert.equal(reqd.body.status, "requested");
	assert.equal(reqd.body.secret, created2.body.secret, "secret 只回给发起请求的设备");
	const badPoll = await req(
		"/v1/passkey-enroll/sessions/" + created2.body.sessionId + "/poll?secret=wrong",
	);
	assert.equal(badPoll.status, 404);
	const poll = await req(
		"/v1/passkey-enroll/sessions/" + created2.body.sessionId + "/poll?secret=" + created2.body.secret,
	);
	assert.equal(poll.body.status, "requested");
	// mock 不伪造 WebAuthn：不返回 attestationOptions，页面会停在"等待批准"
	assert.equal("attestationOptions" in poll.body, false);

	const tpl = await req("/v1/capability-templates");
	assert.ok(Array.isArray(tpl.body.templates));
	assert.ok(tpl.body.templates.length >= 9);
	assert.equal(tpl.body.templates[0].outputs, undefined, "v4 模板没有 outputs");

	const vapid = await req("/v1/vapid-public-key");
	assert.ok(vapid.body.publicKey);

	const logout = await req("/v1/auth/logout", jsonInit("POST", {}));
	assert.equal(logout.body.ok, true);
});

test("mock: ?mockFail=1 强制错误态；MockApi 旋钮（state/reset/capByName/noAgentBinary/failUpgrade/latency）", async () => {
	const { req, Mock } = loadMock();
	const fail = await req("/v1/stats?namespace=default&mockFail=1");
	assert.equal(fail.status, 500);
	assert.equal(fail.body.kind, "Status");
	assert.match(fail.body.message, /mock 强制失败/);

	assert.ok(Mock.state.tasks.length > 0);
	assert.equal(Mock.capByName("fixer").name, "fixer");
	assert.equal(Mock.capByName("ghost"), null);
	assert.equal(Mock.noAgentBinary(), false);
	assert.equal(Mock.noAgentBinary(false), true);
	assert.equal(Mock.failUpgrade("bad_sha"), "bad_sha");
	assert.equal(Mock.failUpgrade(""), "");
	assert.equal(Mock.latency(0), 0);
	assert.equal(typeof Mock.reset, "function");
});

test("mock: stats / console.events 必须显式给 namespace（不再默认全 ns）", async () => {
	const { req } = loadMock();
	const noNs = await req("/v1/stats");
	assert.equal(noNs.status, 400);
	assert.equal(noNs.body.kind, "Status");

	const consoleNoNs = await req("/v1/console/events");
	assert.equal(consoleNoNs.status, 400);

	const stats = await req("/v1/stats?namespace=default");
	assert.equal(stats.status, 200);
	assert.equal(typeof stats.body.total, "number");
	assert.equal(typeof stats.body.deliveryTotal, "number");
	assert.ok(Array.isArray(stats.body.daily));
	assert.equal(stats.body.daily.length, 7);
	assert.equal(stats.body.nodesTotal, 3, "default 的候选节点数（授权交集）");

	const all = await req("/v1/stats?namespace=*");
	assert.ok(all.body.total >= stats.body.total, "* = 跨 ns 汇总（显式请求才给）");

	const rows = await req("/v1/console/events?namespace=default");
	assert.equal(rows.status, 200);
	assert.equal(rows.body.kind, "EventList");
	assert.ok(Array.isArray(rows.body.items));
	assert.equal(rows.body.metadata.continue, "", "W6 metadata.continue, not a flat wrapper");
	const row = rows.body.items.find(r => r.spec.reason === "Ingested");
	assert.ok(row.spec.deliveryId, "Event spec deliveryId remains visible");
	assert.ok(Array.isArray(row.status.dispatched));
	assert.equal(row.status.pruned, null);
	assert.equal(row.metadata.namespace, "default");
});

/* ------------------------------------------------------------------ *
 * F1b · 图投影（graphs.js / traceview.js）去 v3 模型
 * ------------------------------------------------------------------ */

test("graphs: sourceTopology 只吃 v4 字段（spec.next / spec.targets / edges），边没有 port/outputs", async () => {
	const { req, sandbox } = loadMock({ graphs: true });
	const G = sandbox.AnotifyGraphs;
	const [srcRes, capRes, edgeRes] = await Promise.all([
		req("/v1/namespaces/default/sources"),
		req("/v1/namespaces/default/capabilities"),
		req("/v1/namespaces/default/edges"),
	]);
	const topo = G.sourceTopology({
		sources: srcRes.body.items,
		source: "deploy-hook",
		capabilities: capRes.body.items,
		edges: edgeRes.body.items,
	});

	const ids = topo.nodes.map((n) => n.id).sort();
	assert.deepEqual(plain(ids), ["analyze-pr", "deploy-preview", "fixer", "lint-fix", "run-tests"]);
	// 结构身份：空 next 的能力是终点；其余是管道
	const node = (id) => topo.nodes.find((n) => n.id === id);
	assert.equal(node("fixer").kind, "pipe");
	assert.equal(node("deploy-preview").kind, "end");
	assert.equal(node("fixer").entry, true, "源直接指向的能力是入口");
	assert.equal(node("analyze-pr").entry, false);
	// v4 没有 deadport 伪节点（"声明了出口却没接下游"这个概念不存在）
	assert.ok(topo.nodes.every((n) => !("deadport" in n)));

	// 边：端口语义整体消失；`name@version` 的引用收敛到名字节点
	for (const e of topo.edges) {
		assert.deepEqual(plain(Object.keys(e).sort()), ["cycle", "from", "muted", "port", "to"]);
		assert.equal(e.port, "", "v4 没有端口：port 恒为空串");
		assert.equal("showPort" in e, false);
	}
	assert.ok(topo.edges.some((e) => e.from === "fixer" && e.to === "lint-fix"));
	assert.ok(topo.edges.some((e) => e.from === "lint-fix" && e.to === "run-tests"));

	// 未解析的引用画成 unknown（DownstreamResolved=False 在图上的样子）
	const prod = await Promise.all([
		req("/v1/namespaces/prod/capabilities"),
		req("/v1/namespaces/prod/edges"),
		req("/v1/namespaces/prod/sources"),
	]);
	const prodTopo = G.sourceTopology({
		capabilities: prod[0].body.items,
		edges: prod[1].body.items,
		sources: prod[2].body.items,
	});
	const ghost = prodTopo.nodes.find((n) => n.id === "ghost-cap");
	assert.ok(ghost, "解析不到的 next 也要出现在图上");
	assert.equal(ghost.unknown, true);
	assert.equal(ghost.kind, "unknown");

	// 空数据不抛（图库空态由页面负责）
	assert.deepEqual(plain(G.sourceTopology({}).nodes), []);
	assert.deepEqual(plain(G.sourceTopology({ capabilities: [], sources: [] }).edges), []);
});

test("graphs: taskOf / TraceView.buildSpans 用 v4 任务字段（phase 小写、parentTaskId、ISO 时间折算秒）", async () => {
	const { req, sandbox } = loadMock({ graphs: true });
	const G = sandbox.AnotifyGraphs;
	const TV = sandbox.TraceView;
	const trace = await req("/v1/namespaces/default/traces/trace_mock_t1");

	const flat = G.taskOf(trace.body.tasks.find((t) => t.metadata.name === "tsk_mock_4"));
	assert.equal(flat.name, "tsk_mock_4");
	assert.equal(flat.phase, "failed", "phase 必须小写");
	assert.equal(flat.attempt, 3);
	assert.equal(flat.capability.name, "run-tests");
	assert.equal(flat.capability.version, 3);
	assert.equal(typeof flat.startedSec, "number", "RFC3339 → Unix 秒（瀑布图要数字）");
	assert.equal(flat.errorReason, "max_attempts_exceeded");
	assert.equal(flat.parentTaskId, "tsk_mock_2");

	const spans = TV.buildSpans(trace.body.tasks.map(TV.normalizeTask));
	assert.equal(spans[0].depth, 0, "根任务在第一层");
	assert.equal(spans[0].task.name, "tsk_mock_1");
	const depthOf = (name) => spans.find((s) => s.task.name === name).depth;
	assert.equal(depthOf("tsk_mock_2"), 1, "扇出的一支");
	assert.equal(depthOf("tsk_mock_3"), 1, "另一支兄弟");
	assert.equal(depthOf("tsk_mock_4"), 2, "子任务在下一层");

	// 取消态的 class 名走 ui.css 现有拼写（canceled，一个 l）
	assert.equal(TV.STATUS_CLASS.cancelled, "canceled");
	assert.equal(sandbox.AnotifyGraphs.STATUS_CLASS.cancelled, "canceled");
	assert.equal(sandbox.AnotifyGraphs.STATUS_COLOR.failed != null, true);
});

test("graphs: capabilityFlow / taskChain 装配出 v4 spec（边无端口、任务链走 parentTaskId、空 trace 不崩）", async () => {
	const { req, sandbox } = loadMock({ graphs: true });
	const G = sandbox.AnotifyGraphs;
	const TV = sandbox.TraceView;
	const [srcRes, capRes, edgeRes, trace] = await Promise.all([
		req("/v1/namespaces/default/sources"),
		req("/v1/namespaces/default/capabilities"),
		req("/v1/namespaces/default/edges"),
		req("/v1/namespaces/default/traces/trace_mock_t1"),
	]);
	const src = srcRes.body.items.find((s) => s.metadata.name === "deploy-hook");
	const topology = G.sourceTopology({
		sources: [src],
		source: src,
		capabilities: capRes.body.items,
		edges: edgeRes.body.items,
	});

	const host = sandbox.document.createElement("div");
	const flow = G.capabilityFlow(host, {
		src: { name: "deploy-hook", kind: "webhook" },
		topology: topology,
		t: (k, d) => d || k,
	});
	assert.ok(flow.graph, "capabilityFlow 必须返回图实例");
	const spec = host._graphSpec;
	assert.equal(spec.nodes[0].id, "__source__", "第一个节点是源（触发器）");
	assert.ok(spec.nodes.some((n) => n.id === "fixer" && n.entry === true));
	assert.ok(spec.edges.every((e) => e.port === ""), "v4 没有端口：边一律不画出口名");
	assert.ok(spec.edges.every((e) => !("showPort" in e)));
	assert.ok(spec.edges.some((e) => e.from === "__source__" && e.to === "fixer"));
	assert.ok(spec.legend.length > 0);

	const host2 = sandbox.document.createElement("div");
	const res2 = G.taskChain(host2, {
		tasks: trace.body.tasks,
		baseGraph: topology,
		sourceLabel: "deploy-hook",
		t: (k, d) => d || k,
	});
	assert.ok(res2.graph);
	const spec2 = host2._graphSpec;
	assert.equal(spec2.nodes[0].id, "__trigger__");
	const node = (id) => spec2.nodes.find((n) => n.id === id);
	assert.equal(node("run-tests").kind, "failed", "failed 任务染成失败色组");
	assert.equal(node("deploy-preview").kind, "running");
	// 节拍按真实开始时间：根任务（fixer）第一
	assert.equal(spec2.beats[0].cap, "fixer");
	assert.equal(spec2.beats[0].port, "");
	// 剪枝分支（配置里有、这次没走）留在图上，不是消失。
	// 用 prod 那条 trace 验：底图里有 report.send → ghost-cap（解析不到的下游），
	// 而 ghost-cap 这次当然没执行 → 必须作为 pruned 节点留在图上、副标题说明原因。
	const [pSrc, pCap, pEdge, pTrace] = await Promise.all([
		req("/v1/namespaces/prod/sources"),
		req("/v1/namespaces/prod/capabilities"),
		req("/v1/namespaces/prod/edges"),
		req("/v1/namespaces/prod/traces/trace_mock_t2"),
	]);
	const prodSrc = pSrc.body.items.find((x) => x.metadata.name === "prod-hook");
	const prodTopo = G.sourceTopology({
		sources: [prodSrc],
		source: prodSrc,
		capabilities: pCap.body.items,
		edges: pEdge.body.items,
	});
	const host5 = sandbox.document.createElement("div");
	G.taskChain(host5, { tasks: pTrace.body.tasks, baseGraph: prodTopo, sourceLabel: "prod-hook", t: (k, d) => d || k });
	const ghost = host5._graphSpec.nodes.find((n) => n.id === "ghost-cap");
	assert.ok(ghost, "配置里有、这次没走的分支必须留在图上");
	assert.equal(ghost.kind, "pruned");
	assert.equal(ghost.role, "unknown");
	assert.equal(ghost.dead, true);
	assert.match(ghost.sub, /不存在/, "unknown 分支要说清是引用解析不到，不是「未执行」");

	// 没有底图时退回只画执行链（节点 id = 任务名）
	const host3 = sandbox.document.createElement("div");
	G.taskChain(host3, { tasks: trace.body.tasks, t: (k, d) => d || k });
	assert.deepEqual(plain(host3._graphSpec.legend), [], "没有底图时没有图例可讲（scope=none）");
	assert.equal(host3._graphSpec.nodes[0].id, "tsk_mock_1");
	assert.ok(host3._graphSpec.edges.every((e) => e.port === "" && e.times > 0));

	// 空 trace / 不认识的数据形状：不画图、不抛（页面显示空态）
	const host4 = sandbox.document.createElement("div");
	const empty = G.taskChain(host4, { tasks: [], t: (k, d) => d || k });
	assert.equal(empty.graph, null);
	const weird = G.taskChain(sandbox.document.createElement("div"), {
		tasks: [{ capability: "fixer", status: "succeeded" }],
		t: (k, d) => d || k,
	});
	assert.equal(weird.graph, null, "v3 形状的任务不得把 dagre 拖崩");

	// TraceView 三个视图都能挂载（图视图走上面的 taskChain）
	const box = sandbox.document.createElement("div");
	const view = TV.mount(box, {
		tasks: trace.body.tasks,
		el: (tag, cls, text) => {
			const n = sandbox.document.createElement(tag || "div");
			n.className = cls || "";
			n.textContent = text || "";
			return n;
		},
		code: (text, cls) => {
			const n = sandbox.document.createElement("code");
			n.className = cls || "";
			n.textContent = text;
			return n;
		},
		t: (k, d) => d || k,
		view: "waterfall",
	});
	assert.equal(view.getView(), "waterfall");
	assert.ok(box.children.length >= 1);
	for (const v of ["graph", "tree", "waterfall"]) {
		view.setView(v);
		assert.equal(view.getView(), v);
	}
});

/* ================================================================== *
 * F2a · capabilities 页面（v4 能力模型 + 人工部署 + 静态去旧守卫）
 * ================================================================== */

test("mock: 单建能力严格校验（entrypoint ∈ files / unknown_target / undeclared_need）", async () => {
	const { req } = loadMock();
	const post = (spec) =>
		req(CAP_LIST, jsonInit("POST", {
			apiVersion: "anotify/v1",
			kind: "Capability",
			metadata: { name: "brand-new" },
			spec,
		}));

	const noImpl = await post({});
	assert.equal(noImpl.status, 400);
	assert.equal(noImpl.body.reason, "impl_missing");

	const noEntry = await post({
		run: { entrypoint: "run.sh", files: { "other.sh": "#!/bin/sh\n" } },
	});
	assert.equal(noEntry.status, 400);
	assert.equal(noEntry.body.reason, "entrypoint_no_shebang");

	const badTarget = await post({
		next: ["ghost-cap"],
		run: { entrypoint: "run.sh", files: { "run.sh": "#!/bin/sh\necho hi\n" } },
	});
	assert.equal(badTarget.status, 400);
	assert.equal(badTarget.body.reason, "unknown_target");
	assert.match(badTarget.body.message, /ghost-cap/);

	const undeclared = await post({
		next: ["notify"],
		run: {
			entrypoint: "run.sh",
			files: { "run.sh": "#!/bin/sh\necho hi\n" },
			env: { ROOT: { valueFrom: { fieldRef: { fieldPath: "vars.SOURCE_ROOT" } } } },
		},
		requires: { values: [] },
	});
	assert.equal(undeclared.status, 400);
	assert.equal(undeclared.body.reason, "undeclared_need");

	const ok = await post({
		next: ["notify"],
		run: {
			entrypoint: "run.sh",
			files: { "run.sh": "#!/bin/sh\necho hi\n" },
			env: { ROOT: { valueFrom: { fieldRef: { fieldPath: "vars.SOURCE_ROOT" } } } },
			workDir: "$(vars.ROOT)", // 名字不是 env 键 → 页面/后端另行校验，这里只要 201
		},
		requires: { values: ["vars.SOURCE_ROOT"] },
		lifecycle: { idempotent: true, maxAttempts: 2 },
		concurrency: { maxPerNode: 1 },
	});
	assert.equal(ok.status, 201, "满足约束的单建必须成功： " + JSON.stringify(ok.body));
	assert.equal(ok.body.kind, "Capability");
	assert.equal(ok.body.spec.next[0], "notify");
	assert.equal(ok.body.status.conditions.length, 4, "status 仍是读时派生");
});

test("mock: 能力版本规则（只改 next 不换版本/digest；改 run 只能 POST 出新版本；显式同版本正文改动 400）", async () => {
	const { req } = loadMock();
	const list = await req(CAP_LIST);
	const cap = byNameOrFail(list.body.items, "analyze-pr");
	const rv = cap.metadata.resourceVersion;
	const one = CAP_LIST + "/analyze-pr";

	// ① 只改 next：版本与 digest 都不变（路由属于图，不属于内容）。
	// PATCH 的合法面只有 next：把整份 spec 塞进去会被 400 immutable_field 拒掉。
	const nextOnly = await req(one, jsonInit("PATCH", {
		apiVersion: "anotify/v1",
		kind: "Capability",
		metadata: { name: "analyze-pr", resourceVersion: rv },
		spec: { next: ["lint-fix"] },
	}));
	assert.equal(nextOnly.status, 200);
	assert.equal(nextOnly.body.metadata.version, cap.metadata.version);
	assert.equal(nextOnly.body.status.digest, cap.status.digest, "digest 不含 next");
	assert.deepEqual(nextOnly.body.spec.next, ["lint-fix"]);

	// 2 改内容：PATCH 不接（immutable_field），**POST 省略 version** = 内容寻址
	// 自动分配 max+1，旧版进历史。
	const runBody = Object.assign(plain(nextOnly.body.spec), {
		run: { entrypoint: "main.sh", files: { "main.sh": "#!/bin/sh\necho v3\n" } },
	});
	const patchedRun = await req(one, jsonInit("PATCH", {
		apiVersion: "anotify/v1",
		kind: "Capability",
		metadata: { name: "analyze-pr", resourceVersion: nextOnly.body.metadata.resourceVersion },
		spec: runBody,
	}));
	assert.equal(patchedRun.status, 400);
	assert.equal(patchedRun.body.reason, "immutable_field");
	const runChange = await req(CAP_LIST, jsonInit("POST", {
		apiVersion: "anotify/v1",
		kind: "Capability",
		metadata: { name: "analyze-pr" },
		spec: runBody,
	}));
	assert.equal(runChange.status, 201, JSON.stringify(runChange.body));
	assert.equal(runChange.body.metadata.version, cap.metadata.version + 1, "改正文必须出新版本");
	assert.notEqual(runChange.body.status.digest, cap.status.digest);
	const versions = await req(one + "/versions");
	assert.equal(versions.body.kind, "CapabilityList", "versions 不是独立 kind：列表仍是 CapabilityList");
	assert.deepEqual(
		versions.body.items.map((v) => v.metadata.version),
		[3, 2, 1],
		"新→旧，旧版留历史（metadata.version）",
	);
	assert.ok(
		versions.body.items.every((v) => v.kind === "Capability"),
		"每个版本项都是同一个 Capability 信封",
	);
	assert.ok(
		versions.body.items.every((v) => !("current" in v)),
		"wire 不得自造 item.current；“当前版本”由 max(metadata.version) 判定",
	);
	const maxVersion = Math.max(...versions.body.items.map((v) => v.metadata.version));
	assert.equal(maxVersion, runChange.body.metadata.version, "当前版本 = max(metadata.version)");

	// 单版本 GET 也是同一个 Capability 信封；includeBody=true 带正文。
	const v2 = await req(one + "/versions/2");
	assert.equal(v2.body.kind, "Capability");
	assert.equal(v2.body.metadata.version, 2);
	assert.ok(v2.body.status.digest, "单版本必须带 status.digest");
	const v2body = await req(one + "/versions/2?includeBody=true");
	assert.ok(v2body.body.spec && v2body.body.spec.run, "includeBody=true 必须带正文（spec.run）");

	// ③ 显式写“已存在的那个版本号”+ 正文不同 → POST 400 version_content_immutable
	// （content-addressed 单建的拒绝面；PATCH 干脆不接内容，回 immutable_field）
	const otherBody = (version) => ({
		apiVersion: "anotify/v1",
		kind: "Capability",
		metadata: Object.assign({ name: "analyze-pr" }, version == null ? {} : { version }),
		spec: Object.assign({}, plain(runChange.body.spec), {
			run: { entrypoint: "main.sh", files: { "main.sh": "#!/bin/sh\necho other\n" } },
		}),
	});
	const immutable = await req(CAP_LIST, jsonInit("POST", otherBody(3)));
	assert.equal(immutable.status, 400);
	assert.equal(immutable.body.reason, "version_content_immutable");
	assert.equal(immutable.body.kind, "Status");
	// 同版本号同内容 → 内容寻址复用，不报错也不新开版本
	const sameContent = await req(CAP_LIST, jsonInit("POST", { ...otherBody(3), spec: plain(runChange.body.spec) }));
	assert.equal(sameContent.status, 201);
	assert.equal(sameContent.body.metadata.version, 3, "同版本同内容复用那一行");

	// ④ name@旧版本 仍然解析得到（钉死版本不回退成“缺失”）
	const pinned = await req(CAP_LIST);
	const fixer = byNameOrFail(pinned.body.items, "fixer");
	assert.ok(
		fixer.spec.next.indexOf("lint-fix@1") >= 0,
		"种子保持 name@version 写法，页面要展示钉死版本",
	);
});

test("mock: 人工 deploy 可指定历史版本（写该版本的 digest 事实，不自动部署）", async () => {
	const { req } = loadMock();
	const versions = await req(CAP_LIST + "/analyze-pr/versions");
	const v1 = versions.body.items.find((v) => v.metadata.version === 1);
	assert.ok(v1 && v1.status.digest, "历史版本带 status.digest");

	const deployed = await req(
		CAP_LIST + "/analyze-pr/deploy",
		jsonInit("POST", { version: 1, nodes: ["anode-1"] }),
	);
	assert.equal(deployed.status, 201);
	const hit = deployed.body.items.find((d) => d.node === "anode-1");
	assert.equal(hit.version, 1);
	assert.equal(hit.digest, v1.status.digest, "部署的是那个版本的 digest，不是当前版本");

	// 当前版本没变：能力详情仍然报 v2，节点上拿着旧 digest → Deployed=Stale
	const detail = await req(CAP_LIST + "/analyze-pr");
	assert.equal(detail.body.metadata.version, 2);
	const node = detail.body.status.nodes.find((n) => n.name === "anode-1");
	assert.equal(node.state, "stale");
});

test("F2a 静态守卫：capabilities 页只用 v4 字段/路径，人工部署走能力子资源", () => {
	const page = readFileSync(path.join(ROOT, "web-src/pages/capabilities.html"), "utf8");

	const banned = [
		{ re: /namespaceId/, why: "v4 用 scope + 显式 ns 路径，不再有 namespaceId query" },
		{ re: /spec\.outputs/, why: "spec.outputs 在 v4 已删除（能力只有一个结果）" },
		{ re: /\.outputs\b/, why: "页面不得读旧出口字段" },
		{ re: /\bpriority\b/, why: "候选排序/priority 在 v4 已删除（一个下游只有一条路）" },
		{ re: /downstreams/, why: "PUT downstreams 已删除；边是 spec.next 的投影" },
		{ re: /ifRevision/, why: "v4 用 metadata.resourceVersion 做 CAS，没有 ifRevision" },
		{ re: /resolvedEnv/, why: "v4 用 run.env/requires.values + envDiagnostics，不展示 resolvedEnv" },
		{ re: /\/v1\/capabilities(?![a-z0-9-])/, why: "扁平资源路径已删除：必须走 /v1/namespaces/{ns}/capabilities" },
		{ re: /\/deployments/, why: "Deployment 写端点已删除：人工部署只走 POST .../capabilities/{name}/deploy" },
		{ re: /\/v1\/edges/, why: "Edge 写端点已删除：边由 spec.next / targets 派生" },
		{ re: /\.artifact\b|\/artifact/, why: "v4 没有 artifact 概念：脚本全文在 versions?includeBody=true" },
	];
	for (const { re, why } of banned) {
		const m = page.match(re);
		assert.equal(m, null, why + "（命中 " + (m && m[0]) + "）");
	}

	const required = [
		"nsApi",
		"nsList",
		"model.capability",
		"conditions",
		"resolved",
		"observed",
		"deploy",
		"resourceVersion",
		"withResourceVersion",
		"writable",
		"deletePath",
		"isConflict",
	];
	for (const token of required) {
		assert.ok(page.includes(token), "capabilities 页必须落地 v4 canonical 层：缺 " + token);
	}

	// 编辑/创建都必须经过 writable()，提交不得把 status 带回去
	assert.match(page, /writable\([^)]*create/, "创建必须 writable(..., {create:true})");
	assert.match(page, /withResourceVersion\(/, "PATCH 必须注入 resourceVersion");
	assert.match(page, /deletePath\(/, "DELETE 必须带 ?resourceVersion=");
});

/* ------------------------------------------------------------------ *
 * F2a · capabilities 页的字段覆盖 / 版本与 CAS 指引 / 四语言 key
 * ------------------------------------------------------------------ */

// 读 locale 的 caps 段并拍平成 `caps.x.y` → 值（值只用来判空）。
// 只解析 caps 这一层缩进（页面/测试不需要通用 YAML 解析器）。
function readCapsLocale(lang) {
	const src = readFileSync(path.join(ROOT, "web-src/locales", lang + ".yaml"), "utf8");
	const out = new Map();
	const stack = [];
	let inside = false;
	for (const line of src.split("\n")) {
		if (/^caps:\s*$/.test(line)) {
			inside = true;
			continue;
		}
		if (!inside) continue;
		if (/^[A-Za-z0-9_-]+:/.test(line)) break;
		const m = line.match(/^(\s*)([A-Za-z0-9_.-]+):\s*(.*)$/);
		if (!m) continue;
		const ind = m[1].length;
		while (stack.length && stack[stack.length - 1].ind >= ind) stack.pop();
		const key = "caps." + stack.map((x) => x.k).concat(m[2]).join(".");
		out.set(key, m[3].trim().replace(/^['"]|['"]$/g, ""));
		stack.push({ ind, k: m[2] });
	}
	return out;
}

test("F2a i18n：capabilities 页使用的 caps.* key 在四语言都存在（含动态 cond/nodes、无空值）", () => {
	const langs = ["zh-CN", "en", "ja", "es"];
	const dict = new Map(langs.map((l) => [l, readCapsLocale(l)]));
	const page = readFileSync(path.join(ROOT, "web-src/pages/capabilities.html"), "utf8");

	// 页面里三种来源：JS 的 t("caps.…")、模板的 {{t "caps.…"}}、以及动态拼的
	// cond.<type> / nodes.<state>（key 的**完整集合**是冻结词表，不是字面前缀）。
	const used = new Set();
	for (const m of page.matchAll(/t\(\s*"(caps\.[A-Za-z0-9_.]+)"/g)) used.add(m[1]);
	for (const m of page.matchAll(/\{\{t\s+"(caps\.[A-Za-z0-9_.]+)"/g)) used.add(m[1]);
	for (const type of ["Implemented", "NodesAvailable", "DownstreamResolved", "Deployed"]) {
		used.add("caps.cond." + type);
	}
	for (const state of ["deployed", "stale", "missing"]) used.add("caps.nodes." + state);

	const missing = [];
	const empty = [];
	for (const key of [...used].sort()) {
		if (key.endsWith(".")) continue; // 动态前缀本身不是 key
		for (const lang of langs) {
			if (!dict.get(lang).has(key)) missing.push(key + "@" + lang);
			else if (!dict.get(lang).get(key)) empty.push(key + "@" + lang);
		}
	}
	assert.deepEqual(missing, [], "四语言缺 key（页面会显示裸 key）");
	assert.deepEqual(empty, [], "四语言存在空值 key");

	// 四语的 caps 段 key 集合必须一致：新增/删除只改一种语言会被这里拦住。
	const sets = langs.map((l) => [...dict.get(l).keys()].sort().join("\n"));
	assert.equal(new Set(sets).size, 1, "四语言 caps key 集合不一致");
});

test("F2a 静态守卫：能力编辑器覆盖 params/files/env/workDir/next/requires/lifecycle/concurrency", () => {
	const page = readFileSync(path.join(ROOT, "web-src/pages/capabilities.html"), "utf8");

	// 表单控件：每个 v4 spec 段都要有可输入/可选择的落点（不能只读展示）。
	const controls = [
		"cap-name",
		"cap-display-name",
		"cap-version",
		"cap-params",
		"cap-next",
		"cap-entrypoint",
		"cap-files",
		"cap-env",
		"cap-workdir",
		"cap-requires-commands",
		"cap-requires-values",
		"cap-idempotent",
		"cap-max-attempts",
		"cap-result-ttl",
		"cap-timeout",
		"cap-concurrency",
	];
	for (const id of controls) {
		assert.ok(page.includes('id="' + id + '"'), "能力编辑器缺少控件：" + id);
	}

	// 读表单必须真的组装 v4 spec（不是拼旧字段）。
	assert.match(page, /run:\s*\{\s*entrypoint:\s*entrypoint,\s*files:\s*files\s*\}/, "readForm 必须组装 spec.run.files/entrypoint");
	assert.match(page, /requires:\s*\{\s*commands:\s*commands,\s*values:\s*values\s*\}/, "readForm 必须组装 spec.requires");
	assert.match(page, /lifecycle:\s*\{[\s\S]*?idempotent:[\s\S]*?maxAttempts:[\s\S]*?resultTtlSec:[\s\S]*?timeoutSec:/, "readForm 必须组装 spec.lifecycle");
	assert.match(page, /concurrency:\s*\{\s*maxPerNode:\s*concurrency\s*\}/, "readForm 必须组装 spec.concurrency");
	assert.match(page, /if \(workDir\) spec\.run\.workDir = workDir/, "readForm 必须写 spec.run.workDir（空 = 独立目录）");
	assert.match(page, /spec\.run\.env = env/, "readForm 必须写 spec.run.env");
});

test("F2a 静态守卫：版本不可变/409 保留输入/人工部署/删除二次确认的指引都在页面里", () => {
	const page = readFileSync(path.join(ROOT, "web-src/pages/capabilities.html"), "utf8");

	// 版本：显式版本内容不可变的用户指引 + 服务端 400 的个人化显示。
	assert.ok(page.includes("version_content_immutable"), "必须识别 400 version_content_immutable");
	assert.ok(page.includes("caps.error.immutable"), "400 必须显示不可变指引（换版本）");
	assert.ok(page.includes("caps.form.version_hint") || page.includes("caps.form.version_edit_hint"), "版本输入必须有内容寻址/不可变提示");
	assert.ok(page.includes("caps.form.current_version"), "编辑时必须显示当前版本");

	// CAS：409 横幅说明当前 rv + 「输入都还在」+ 只有显式点重新载入才丢弃。
	assert.ok(page.includes("caps.conflict.body"), "409 必须解释当前 resourceVersion 且输入保留");
	assert.ok(page.includes("caps.conflict.reload"), "409 必须提供「重新载入最新」显式动作");
	assert.match(page, /reloadAfterConflict/, "409 重载必须走独立函数（用户显式触发）");
	assert.ok(page.includes("isConflict(e)"), "必须用统一 isConflict 判 409");

	// 人工部署：不自动部署；部署只由 submitDeploy 的 POST 子资源发起。
	assert.ok(page.includes("caps.deploy.hint"), "部署弹层必须写明不会自动部署");
	const deployPosts = page.match(/"\/deploy"/g) || [];
	assert.equal(deployPosts.length, 1, "页面只允许一个人工部署调用点（POST …/deploy）");

	// 删除：CAS 冲突后重新载入最新 rv，并要求二次确认（不自动重试）。
	assert.ok(page.includes("caps.delete.conflict"), "删除 409 必须提示已重载并需再次确认");
	assert.match(page, /deletePath\(/, "DELETE 必须走 deletePath（带 CAS）");

	// 版本“当前”判定：wire 是 CapabilityList.items（每项 Capability 信封），
	// 没有 item.current；页面必须自己按 max(metadata.version) 算。
	assert.ok(!/\bv\.current\b/.test(page) && !/\bitem\.current\b/.test(page), "版本“当前”禁止读自造的 item.current");
	assert.match(page, /currentVersionOf\(/, "页面必须用 currentVersionOf（max metadata.version）判定当前版本");
	assert.match(page, /versionIndexOf\(/, "版本项必须从 Capability 信封投影 version/digest/creationTimestamp");
});

/* F2b1 · Source closure only: transport, projections and side-effect boundaries. */
const SRC_LIST = "/v1/namespaces/default/sources";
const sourceDoc = (name, kind="manual", config={}, targets=[]) => ({
 apiVersion:"anotify/v1", kind:"Source", metadata:{name,namespace:"default"},
 spec:{kind,enabled:true,config,targets},
});

test("Source CAS CRUD: missing400/stale409, immutable identity, atomic target edges and namespace isolation",async()=>{
 const {req}=loadMock();
 const targets=[{capability:"notify",params:{message:"constant",extra:{ok:true}}},{capability:"fixer@1",params:{repo:"$(payload.repo)"}}];
 const created=await req(SRC_LIST,jsonInit("POST",sourceDoc("source-cas","manual",{payload:{repo:"a/b"}},targets)));
 assert.equal(created.status,201);assert.deepEqual(created.body.spec.targets,targets);
 const path=SRC_LIST+"/source-cas";const rv=created.body.metadata.resourceVersion;
 const patch=sourceDoc("source-cas","manual",{payload:{repo:"c/d"}},[{capability:"notify",params:{message:"new"}}]);
 assert.equal((await req(path,jsonInit("PATCH",patch))).body.reason,"resource_version_required");
 assert.equal((await req(path,{method:"DELETE"})).status,400);
 assert.equal((await req(path+"?resourceVersion=old",{method:"DELETE"})).status,409);
 assert.deepEqual((await req(path)).body.spec.targets,targets,"failed CAS has no writes");
 patch.metadata.resourceVersion=rv;
 const renamed=plain(patch);renamed.metadata.name="renamed";
 assert.equal((await req(path,jsonInit("PATCH",renamed))).body.reason,"name_immutable");
 const changedKind=plain(patch);changedKind.spec.kind="webhook";
 assert.equal((await req(path,jsonInit("PATCH",changedKind))).body.reason,"source_kind_immutable");
 const saved=await req(path,jsonInit("PATCH",patch));assert.equal(saved.status,200);
 assert.notEqual(saved.body.metadata.resourceVersion,rv);
 assert.deepEqual(saved.body.spec.targets,patch.spec.targets);
 const stale=await req(path,jsonInit("PATCH",patch));assert.equal(stale.status,409);
 assert.equal(stale.body.details.current.resourceVersion,saved.body.metadata.resourceVersion);
 const edges=(await req("/v1/namespaces/default/edges")).body.items.filter(e=>e.kind==="source" && e.from==="source-cas");
 assert.deepEqual(edges,[{from:"source-cas",to:"notify",kind:"source"}]);
 assert.equal((await req("/v1/namespaces/prod/sources/source-cas")).status,404);
 const prod=sourceDoc("source-cas");prod.metadata.namespace="prod";
 assert.equal((await req("/v1/namespaces/prod/sources",jsonInit("POST",prod))).status,201);
 assert.equal((await req(path+"?resourceVersion="+rv,{method:"DELETE"})).status,409);
 assert.equal((await req(path+"?resourceVersion="+saved.body.metadata.resourceVersion,{method:"DELETE"})).status,204,'v4 的 DELETE 成功是 204 无体');
 assert.equal((await req(path)).status,404);
 assert.equal((await req("/v1/namespaces/prod/sources/source-cas")).status,200);
});

test("Source diagnostics: cron range/tz, every-kind payloadSchema, template syntax/nested numeric paths and literal other roots",async()=>{
 const {req,Mock}=loadMock();
 for(const [kind,config] of [["cron",{expr:"61 * * * *"}],["cron",{expr:"0 3 * * *",tz:"Moon/Base"}],["manual",{payloadSchema:[]}],["webhook",{sample:[]}]] ){
  const result=await req(SRC_LIST,jsonInit("POST",sourceDoc("invalid-source",kind,config)));
  assert.equal(result.status,400);assert.equal(result.body.reason,"invalid_source_config");
 }
 for(const tmpl of ["$(payload.a-b)","$(payload..x)","$(payload.","$(vars.x)","$(params.x)"]){
  const result=await req(SRC_LIST,jsonInit("POST",sourceDoc("bad-template","manual",{},[{capability:"notify",params:{nested:{value:tmpl}}}])));
  assert.equal(result.status,400,tmpl);assert.equal(result.body.reason,"invalid_source_config");
 }
 const source=sourceDoc("templates","manual",{payload:{items:[{name:"one"}],flag:true}},[{capability:"notify",params:{text:"required text",message:"hello $(payload.items.0.name)",nested:{bool:"$(payload.flag)"},literal:"$(secret.x)",whole:"$(payload)"}}]);
 const before=JSON.stringify(Mock.state);
 const preview=await req(SRC_LIST+"/dry-run",jsonInit("POST",{source}));
 assert.equal(preview.status,200);
 assert.equal(preview.body.targets[0].state,"ready");
 assert.equal(preview.body.targets[0].params.message,"hello one");
 assert.equal(preview.body.targets[0].params.nested.bool,true);
 assert.equal(preview.body.targets[0].params.literal,"$(secret.x)");
 assert.deepEqual(preview.body.targets[0].params.whole,source.spec.config.payload);
 assert.equal(JSON.stringify(Mock.state),before);
 const bare=await req(SRC_LIST+"/dry-run",jsonInit("POST",{source:source.spec}));
 assert.equal(bare.status,400,"unsaved preview requires a write Source envelope");
});

test("Source hooks: HMAC raw bytes gate401, GET masks, credential rotation, enabled404, source scoped 30day dedup",async()=>{
 const {createHmac,webcrypto}=await import("node:crypto");
 const {req,Mock,sandbox}=loadMock();sandbox.crypto=webcrypto;sandbox.TextEncoder=TextEncoder;
 const secret="source-test-hidden-secret";
 const src=sourceDoc("signed-hook","webhook",{token:"signed-test-token",secret,sample:{message:"hint"},payload:{message:"ignored"}},[{capability:"notify",params:{text:"$(payload.message)",message:"$(payload.message)"}}]);
 const saved=await req(SRC_LIST,jsonInit("POST",src));assert.equal(saved.status,201);
 assert.deepEqual(Object.keys(saved.body.spec.config).sort(),["hookUrl","sample","secretSet"]);
 assert.equal(saved.body.spec.config.secretSet,true);
 assert.equal(JSON.stringify(saved.body).includes(secret),false);
 const hook="/v1/hooks/signed-test-token";const body=' {"id":"delivery-one", "payload":{"message":"real"}} ';
 const snapshot=JSON.stringify(Mock.state);
 for(const headers of [{},{"X-Anotify-Signature":"sha256="+"0".repeat(64)}])assert.equal((await req(hook,{method:"POST",body,headers})).status,401);
 assert.equal(JSON.stringify(Mock.state),snapshot,"invalid HMAC never produces event/task/dedup");
 const send=async(raw)=>req(hook,{method:"POST",body:raw,headers:{"X-Anotify-Signature":"sha256="+createHmac("sha256",secret).update(raw).digest("hex")}});
 const accepted=await send(body);assert.equal(accepted.status,200);assert.equal(accepted.body.tasks.length,1);
 const task=(await req("/v1/namespaces/default/tasks/"+accepted.body.tasks[0].taskId)).body;
 assert.equal(task.spec.params.message,"real","sample/default webhook payload never dispatched");
 const ingested=(await req("/v1/namespaces/default/events")).body.items.find(e=>e.metadata.name===accepted.body.eventId);
 assert.equal(ingested.spec.deliveryId,"delivery-one");
 assert.equal(ingested.spec.reason,"Ingested");
 const count=Mock.state.tasks.length;
 const duplicate=await send(body.replace('real','changed'));
 assert.equal(duplicate.body.duplicate,true);assert.equal(duplicate.body.eventId,accepted.body.eventId);
 assert.equal(duplicate.body.traceId,accepted.body.traceId);assert.equal(Mock.state.tasks.length,count);
 const event=Mock.state.events.find(e=>e.name===accepted.body.eventId);event.createdAt=new Date(Date.now()-31*86400000).toISOString();
 const expired=await send(body);assert.equal(expired.body.duplicate,false);assert.notEqual(expired.body.eventId,accepted.body.eventId);
 const other=await req(SRC_LIST,jsonInit("POST",sourceDoc("another-hook","webhook",{},[])));
 const otherReceipt=await req(SRC_LIST.replace('/sources','/events'),jsonInit("POST",{source:"another-hook",id:"delivery-one",payload:{}}));
 assert.equal(otherReceipt.body.duplicate,false,"same id on a different source is independent");
 const path=SRC_LIST+"/signed-hook";
 const rotated=await req(path,jsonInit("PATCH",{apiVersion:"anotify/v1",kind:"Source",metadata:{resourceVersion:saved.body.metadata.resourceVersion},spec:{config:{token:"rotated-token"}}}));
 assert.equal(rotated.status,200);assert.equal(rotated.body.spec.config.secretSet,true);
 assert.equal((await send(body)).status,404,"old URL invalid immediately");
 assert.equal((await req("/v1/hooks/rotated-token",jsonInit("POST",{}))).status,401);
 const disabled=await req(path,jsonInit("PATCH",{kind:"Source",metadata:{resourceVersion:rotated.body.metadata.resourceVersion},spec:{enabled:false}}));
 assert.equal(disabled.status,200);
 assert.equal((await req("/v1/hooks/rotated-token",jsonInit("POST",{}))).status,404);
 assert.equal((await req("/v1/hooks/not-found",jsonInit("POST",{}))).status,404);
});

test("Source pages: frozen wire/ns/CAS/credential/read projection/static classes and four-language key parity",()=>{
 const pages=["sources","source"].map(name=>readFileSync(path.join(ROOT,"web-src/pages/"+name+".html"),"utf8"));
 for(const page of pages){assert.match(page,/A\.nsList/);assert.match(page,/A\.nsApi|A\.nsPath/);assert.match(page,/await A\.ready/);assert.match(page,/A\.deletePath/);assert.match(page,/A\.withResourceVersion/);assert.doesNotMatch(page,/configRaw|targetCaps|resultSchema|\?id=|fallbackMock|priority|mapping:/);}
 const sets=["zh-CN","en","ja","es"].map(lang=>{const yaml=readFileSync(path.join(ROOT,"web-src/locales/"+lang+".yaml"),"utf8");const block=yaml.split('sources:\n  v4:\n')[1].split(/\n  [^ ]/)[0];return [...block.matchAll(/^    ([a-z_]+):/gm)].map(m=>m[1]).sort();});
 assert.ok(sets[0].length>60);for(const keys of sets.slice(1))assert.deepEqual(keys,sets[0]);
});

/* F2b2: canonical W6 only. These behavior assertions intentionally reject
 * the obsolete console/log/emission wrappers instead of accepting both. */
test("F2b2 W6 EventList: opaque pagination bound to namespace/source/traceId, UID refresh", async () => {
  const { req } = loadMock();
  const base = '/v1/console/events?namespace=default&limit=2';
  const first = (await req(base)).body;
  assert.equal(first.kind, 'EventList');
  assert.equal(first.apiVersion, 'anotify/v1');
  assert.equal(first.items.length, 2);
  assert.ok(first.metadata.continue);
  assert.ok(first.items.every(e => e.metadata.namespace === 'default' && e.metadata.uid));
  const second = (await req(base + '&continue=' + encodeURIComponent(first.metadata.continue))).body;
  assert.ok(second.items.length);
  assert.ok(second.items.every(e => !first.items.some(f => f.metadata.uid === e.metadata.uid)));
  for (const suffix of ['namespace=prod', 'namespace=default&source=deploy-hook', 'namespace=default&traceId=trace_mock_t1']) {
    const foreign = await req('/v1/console/events?' + suffix + '&continue=' + encodeURIComponent(first.metadata.continue));
    assert.equal(foreign.status, 400);
    // 真服务（api_cas.go·decodeCursor）的 reason 是 BadRequest，message 点名 continue token：
    // cursor 绑 ns+筛选条件，“它就是不属于这次查询”。
    assert.equal(foreign.body.reason, 'BadRequest');
    assert.match(String(foreign.body.message), /continue token/);
  }
  assert.equal((await req(base + '&continue=forged')).status, 400);
  const filtered = (await req('/v1/console/events?namespace=default&source=deploy-hook&traceId=trace_mock_t1')).body;
  assert.ok(filtered.items.length);
  assert.ok(filtered.items.every(e => e.spec.involvedObject.name === 'deploy-hook' && e.spec.traceUID === 'trace_mock_t1'));
  const refresh = (await req(base)).body;
  assert.equal(new Set([...first.items, ...second.items, ...refresh.items].map(e => e.metadata.uid)).size, first.items.length + second.items.length);
  assert.equal(first.items.some(e => 'nextCursor' in e || 'createdAt' in e), false);
});

test("F2b2 Event model: envelope identity/severity/business type; never flat fallback", () => {
  const { A } = loadAnotify();
  const m = plain(A.model.event({ metadata: { uid: 'u', name: 'e', namespace: 'default', creationTimestamp: 'now' }, spec: { type: 'Warning', eventType: 'build.failed', deliveryId: 'business-id' }, status: { pruned: { reason: 'param_missing', detail: 'required repo' } } }));
  assert.equal(m.uid, 'u');
  assert.equal(m.creationTimestamp, 'now');
  assert.equal(m.type, 'Warning');
  assert.equal(m.eventType, 'build.failed');
  assert.equal(m.deliveryId, 'business-id');
  assert.equal(A.model.event({ type: 'Normal', eventType: 'flat', deliveryId: 'old' }).deliveryId, null);
});

test("F2b2 Task logs: exact lines/current/previous/tail/since/truncated and scope", async () => {
  const { req, Mock } = loadMock();
  const url = '/v1/namespaces/default/tasks/tsk_mock_5/logs';
  const current = (await req(url)).body;
  assert.deepEqual(Object.keys(current).sort(), ['attempt', 'lines', 'taskId', 'truncated']);
  assert.equal(current.taskId, 'tsk_mock_5');
  assert.equal(current.attempt, 1, 'A1: return the actual latest archive, not pending retry number');
  const previous = (await req(url + '?previous=true')).body;
  assert.equal(previous.attempt, 1);
  assert.ok(previous.lines.length);
  assert.deepEqual(Object.keys(previous.lines[0]).sort(), ['at', 'sequence', 'stream', 'text']);
  assert.equal(previous.truncated, true);
  const tail = (await req(url + '?previous=true&tailLines=1')).body;
  assert.equal(tail.lines.length, 1);
  assert.deepEqual(tail.lines[0], previous.lines.at(-1));
  Mock.state.taskLogs.find(l => l.task === 'tsk_mock_5' && l.attempt === 1).lines[0].at = '2000-01-01T00:00:00Z';
  const recent = (await req(url + '?previous=true&sinceSeconds=3600')).body;
  assert.equal(recent.lines.length, previous.lines.length - 1);
  assert.deepEqual((await req('/v1/namespaces/default/tasks/tsk_mock_1/logs?previous=true')).body.lines, []);
  assert.equal((await req('/v1/namespaces/prod/tasks/tsk_mock_5/logs')).status, 404);
  assert.equal((await req('/v1/namespaces/prod/tasks/tsk_mock_10/logs')).status, 503);
});

test("F2b2 Task emissions: Event facts, cursor scope, eventId dedup, no ports/payload/env", async () => {
  const { req } = loadMock();
  const url = '/v1/namespaces/default/tasks/tsk_mock_2/emissions?limit=1';
  const first = (await req(url)).body;
  assert.deepEqual(Object.keys(first).sort(), ['items', 'metadata']);
  assert.equal(first.items.length, 1);
  assert.ok(first.metadata.continue);
  const second = (await req(url + '&continue=' + encodeURIComponent(first.metadata.continue))).body;
  assert.ok(second.items.length);
  assert.notEqual(first.items[0].eventId, second.items[0].eventId);
  const all = [...first.items, ...second.items];
  assert.ok(all.some(e => e.pruned && e.taskId === null));
  assert.ok(all.every(e => e.eventId && e.to && Number.isInteger(e.version)));
  for (const forbidden of ['seq', 'port', 'ports', 'fields', 'payload', 'env', 'ok']) assert.equal(keysDeep(all).has(forbidden), false);
  const wrong = await req('/v1/namespaces/default/tasks/tsk_mock_1/emissions?continue=' + encodeURIComponent(first.metadata.continue));
  assert.equal(wrong.status, 400);
});

test("F2b2 Task facts: waiting preserves attempt, dual revisions, masked env/attempts, exact wire, real actions", async () => {
  const { req, A } = loadMock();
  const url = '/v1/namespaces/default/tasks/tsk_mock_5';
  const task = (await req(url)).body;
  assert.equal(task.status.waitingReason, 'retry_backoff');
  assert.equal(task.status.attempt, 2);
  assert.ok(task.status.nextAttemptAt);
  assert.equal((await req(url)).body.status.attempt, 2);
  task.status.attempts[0].namespaceVarsRevision = '11';
  task.status.attempts[0].instanceId = 7;
  task.status.attempts[0].envDiagnostics = [{ name: 'TOKEN', source: 'passthrough', value: 'never-display-secret' }];
  assert.equal('installedFrom' in task.status, false, 'Task wire never defines provenance');
  const model = plain(A.model.task(task));
  assert.equal('installedFrom' in model, false, 'Do not invent Task provenance or a fallback lookup');
  assert.equal(model.attempts[0].namespaceVarsRevision, '11');
  assert.equal(model.attempts[0].instanceId, 7);
  assert.equal(JSON.stringify(model).includes('never-display-secret'), false);
  const cancel = await req(url + '/cancel', { method: 'POST', body: '{}' });
  assert.equal(cancel.body.status.phase, 'cancelled');
  assert.equal(cancel.body.status.waitingReason, null);
  assert.equal((await req(url + '/abandon', { method: 'POST', body: '{}' })).status, 409);
  const abandon = await req('/v1/namespaces/default/tasks/tsk_mock_6/abandon', { method: 'POST', body: '{}' });
  assert.equal(abandon.body.status.phase, 'abandoned');
  assert.ok(abandon.body.status.result.error.reason);
});

test("F2b2 stats: daily uses actual events; delivery not task terminal count; empty devices honest", async () => {
  const { req, Mock } = loadMock();
  const stats = (await req('/v1/stats?namespace=default')).body;
  assert.equal(stats.total, Mock.state.events.filter(e => e.ns === 'default').length);
  assert.equal(stats.daily.reduce((sum, d) => sum + d.count, 0), stats.total);
  assert.equal(stats.deviceCount, 0);
  assert.equal(stats.deliveryTotal, 0, 'No push facts means no fabricated task-as-delivery samples');
  assert.equal(stats.delivered, 0);
  assert.equal(stats.running, Mock.state.tasks.filter(t => t.ns === 'default' && t.phase === 'running').length);
});

test("F2b2 Trace: single namespace virtual readmodel, actual graph parentTaskId not attempt, pruned reasons", async () => {
  const { req, sandbox } = loadMock({ graphs: true });
  const doc = (await req('/v1/namespaces/default/traces/trace_mock_t1')).body;
  assert.ok(doc.events.length && doc.tasks.length && doc.pruned.length);
  assert.equal((await req('/v1/namespaces/prod/traces/trace_mock_t1')).status, 404);
  const host = fakeNode();
  sandbox.AnotifyGraphs.taskChain(host, { tasks: doc.tasks, sourceLabel: 'deploy-hook', animate: false });
  assert.ok(host._mountSpec.nodes.length >= doc.tasks.length);
  const fourth = doc.tasks.find(t => t.metadata.name === 'tsk_mock_4');
  assert.equal(fourth.status.attempts.length, 3);
  assert.equal(fourth.spec.parentTaskId, 'tsk_mock_2');
  assert.ok(doc.pruned.every(p => p.reason && p.detail));
});

test("F2b2 A1 artifacts: scoped manifest-only 64 lowerhex bytes, expired410/missing404, no node paths", async () => {
  const { req, sandbox } = loadMock();
  const task = (await req('/v1/namespaces/default/tasks/tsk_mock_2')).body;
  const artifact = task.status.result.artifacts[0];
  assert.match(artifact.sha256, /^[a-f0-9]{64}$/);
  const url = '/v1/namespaces/default/tasks/tsk_mock_2/artifacts/' + artifact.sha256;
  const response = await sandbox.fetch(url);
  assert.equal(response.status, 200);
  assert.equal(response.headers.get('Content-Type'), 'application/octet-stream');
  assert.match(await response.text(), /3 issues/);
  assert.equal((await req('/v1/namespaces/default/tasks/tsk_mock_1/artifacts/' + artifact.sha256)).status, 404);
  assert.equal((await req('/v1/namespaces/prod/tasks/tsk_mock_2/artifacts/' + artifact.sha256)).status, 404);
  const expired = (await req('/v1/namespaces/default/tasks/tsk_mock_4')).body.status.result.artifacts[0];
  const expiredResponse = await req('/v1/namespaces/default/tasks/tsk_mock_4/artifacts/' + expired.sha256);
  assert.equal(expiredResponse.status, 410);
  assert.equal(expiredResponse.body.reason, 'artifact_expired');
});

test("F2b2 A1 logs: actual latest archive and previous retained attempt; expired/offline never empty success", async () => {
  const { req } = loadMock();
  const latest = (await req('/v1/namespaces/default/tasks/tsk_mock_5/logs')).body;
  assert.equal(latest.attempt, 1, 'pending retry must not fabricate a current attempt archive');
  const previous = (await req('/v1/namespaces/default/tasks/tsk_mock_4/logs?previous=true')).body;
  assert.equal(previous.attempt, 2);
  assert.ok(previous.lines.length);
  const expired = await req('/v1/namespaces/default/tasks/tsk_mock_1/logs');
  assert.equal(expired.status, 410);
  assert.equal(expired.body.reason, 'logs_expired');
  const absent = (await req('/v1/namespaces/default/tasks/tsk_mock_1/logs?previous=true')).body;
  assert.deepEqual(absent.lines, []);
  assert.equal(absent.attempt, 0);
});

// Run actual inline page controllers with a recording DOM. The existing F1 VM
// helpers are unchanged; browser rendering/clicks are separately covered below.
async function b2Page(pageName, search, setup = () => {}, options = {}) {
  const run = loadMock({ graphs: true, ...options, search });
  const nodes = new Map();
  function element(tag) {
    const node = fakeNode(tag);
    const listeners = new Map();
    const set = node.setAttribute;
    node.setAttribute = (key, value) => {
      set(key, value);
      if (key === 'id') nodes.set(String(value), node);
      if (key === 'value') node.value = String(value);
    };
    node.addEventListener = (type, callback) => {
      if (!listeners.has(type)) listeners.set(type, []);
      listeners.get(type).push(callback);
    };
    node.fire = async type => {
      for (const callback of listeners.get(type) || []) await callback({ preventDefault() {} });
    };
    node.value = '';
    return node;
  }
  run.sandbox.document.createElement = element;
  run.sandbox.document.createElementNS = (_, tag) => element(tag);
  run.sandbox.document.createDocumentFragment = () => element('fragment');
  run.sandbox.document.getElementById = id => {
    if (!nodes.has(id)) {
      const node = element('div');
      node.setAttribute('id', id);
      if (id === 'ev-limit') node.value = '20';
    }
    return nodes.get(id);
  };
  run.sandbox.URLSearchParams = URLSearchParams;
  run.sandbox.URL = URL;
  // 浏览器里 sitegen 注入 window.AnotifyI18n；页面直接索引它，这里给空字典让 fallback 生效。
  run.sandbox.AnotifyI18n = {};
  run.sandbox.confirm = () => true;
  run.sandbox.location.search = search;
  run.A.mountLayout = () => {};
  run.A.mountPullRefresh = () => {};
  // 真页面里有 DOMContentLoaded 才启动的控制器（playbook/playbooks/capabilities）：
  // fakeDocument 的 addEventListener 是 no-op，这里先收集再在脚本求值后真实触发一次。
  const domReady = [];
  run.sandbox.document.addEventListener = (type, callback) => {
    if (type === 'DOMContentLoaded') domReady.push(callback);
  };
  const calls = [];
  const fetches = [];
  const rawFetch = run.sandbox.fetch;
  run.sandbox.fetch = (url, opts) => {
    fetches.push({ url: String(url), opts: opts || {} });
    return rawFetch(url, opts);
  };
  const api = run.A.api;
  run.A.api = async (url, opts) => {
    calls.push({ url, opts });
    return api(url, opts);
  };
  await setup(run);
  const source = readFileSync(path.join(ROOT, 'web-src/pages/' + pageName + '.html'), 'utf8');
  for (const script of source.matchAll(/<script>([\s\S]*?)<\/script>/g)) {
    vm.runInContext(script[1], run.ctx, { filename: pageName + '.inline.js' });
  }
  const settle = async () => { for (let i = 0; i < 8; i++) await new Promise(setImmediate); };
  for (const callback of domReady) await callback();
  await settle();
  return { ...run, nodes, calls, fetches, settle, get: id => run.sandbox.document.getElementById(id) };
}
function b2Text(node) {
  return [node?.textContent || '', ...(node?.children || []).map(b2Text)].join(' ');
}
function b2Find(node, predicate, out = []) {
  if (predicate(node)) out.push(node);
  for (const child of node?.children || []) b2Find(child, predicate, out);
  return out;
}

test("F2b2 events controller: actual filter/page/refresh/scope clicks reset opaque cursor and dedup UID", async () => {
  const run = await b2Page('events', '?namespace=default');
  run.get('ev-limit').value = '2';
  await run.get('ev-filters').fire('submit'); await run.settle();
  assert.equal(run.get('ev-list').children.length, 2);
  await run.get('ev-more').fire('click'); await run.settle();
  assert.equal(run.get('ev-list').children.length, 4);
  assert.match(run.calls.at(-1).url, /continue=/);
  await run.get('ev-refresh').fire('click'); await run.settle();
  assert.equal(run.get('ev-list').children.length, 4);
  assert.equal(new Set(run.get('ev-list').children.map(n => n.getAttribute('data-uid'))).size, 4);
  run.get('ev-source').value = 'deploy-hook';
  run.get('ev-trace').value = 'trace_mock_t1';
  await run.get('ev-filters').fire('submit'); await run.settle();
  const filtered = run.calls.at(-1).url;
  assert.match(filtered, /source=deploy-hook/);
  assert.match(filtered, /traceId=trace_mock_t1/);
  assert.equal(filtered.includes('continue='), false);
  run.get('ev-namespace').value = 'prod';
  await run.get('ev-namespace').fire('change'); await run.settle();
  assert.match(run.calls.at(-1).url, /namespace=prod/);
  assert.equal(run.calls.at(-1).url.includes('continue='), false);
  assert.match(b2Text(run.get('ev-list')), /events.v4.empty/);
});

test("F2b2 message controller: diagnostics mask env, actual previous logs/emission paging/actions/A1 path safety", async () => {
  const waiting = await b2Page('message', '?namespace=default&id=tsk_mock_5', ({ Mock }) => {
    Mock.state.tasks.find(t => t.name === 'tsk_mock_5').envDiagnostics = [{ name: 'TOKEN', source: 'passthrough', value: 'never-print-value' }];
  });
  assert.match(b2Text(waiting.get('task-facts')), /retry_backoff/);
  assert.match(b2Text(waiting.get('task-env')), /TOKEN/);
  assert.equal(b2Text(waiting.get('msg-content')).includes('never-print-value'), false);
  waiting.get('logs-previous').value = 'true';
  await waiting.get('logs-previous').fire('change'); await waiting.settle();
  assert.match(b2Text(waiting.get('logs-output')), /infra_transient/);
  assert.match(b2Text(waiting.get('logs-status')), /message.v4.truncated/);
  await waiting.get('task-cancel').fire('click'); await waiting.settle();
  assert.equal(waiting.get('task-phase').getAttribute('data-phase'), 'cancelled');
  assert.equal(waiting.get('task-abandon').disabled, true);
  const running = await b2Page('message', '?namespace=default&id=tsk_mock_6');
  await running.get('task-abandon').fire('click'); await running.settle();
  assert.equal(running.get('task-phase').getAttribute('data-phase'), 'abandoned');
  const emitted = await b2Page('message', '?namespace=default&id=tsk_mock_2', ({ Mock }) => {
    Mock.state.tasks.find(t => t.name === 'tsk_mock_2').result.artifacts.push({ path: '../private', sha256: 'b'.repeat(64) });
  });
  assert.equal(emitted.get('emissions-list').children.length, 2);
  await emitted.get('emissions-more').fire('click'); await emitted.settle();
  assert.equal(emitted.get('emissions-list').children.length, 3);
  assert.match(emitted.calls.at(-1).url, /continue=/);
  const links = b2Find(emitted.get('task-result'), n => n.tagName === 'A');
  assert.equal(links.length, 1, 'invalid path is display-only and cannot produce a link');
  assert.match(links[0].getAttribute('href'), /^\/v1\/namespaces\/default\/tasks\/tsk_mock_2\/artifacts\/[a-f0-9]{64}$/);
  const expired = await b2Page('message', '?namespace=default&id=tsk_mock_4');
  const expiredLink = b2Find(expired.get('task-result'), n => n.tagName === 'A')[0];
  await expiredLink.fire('click'); await expired.settle();
  assert.match(b2Text(expired.get('task-result')), /message.v4.artifact_expired/);
});

test("F2b2 trace/index controllers: actual namespace Trace graph and task deep links, stats and 0 devices", async () => {
  const trace = await b2Page('trace', '?namespace=default&traceUID=trace_mock_t1');
  assert.equal(trace.calls.filter(c => c.url.includes('/traces/')).length, 1);
  assert.ok(trace.get('trace-graph')._mountSpec.nodes.length >= 7);
  assert.equal(trace.get('trace-tasks').children.length, 7, 'three attempts still one logical task');
  assert.match(b2Text(trace.get('trace-pruned')), /param_schema/);
  const links = b2Find(trace.get('trace-tasks'), n => n.tagName === 'A');
  assert.ok(links.every(n => n.getAttribute('href').includes('namespace=default&id=tsk_mock_')));
  const index = await b2Page('index', '?namespace=default');
  assert.equal(index.get('kpi-total').textContent, '4');
  assert.equal(index.get('kpi-devices').textContent, '0');
  assert.equal(index.get('kpi-rate').children[0].textContent, '–');
  assert.equal(index.calls.some(c => /^\/v1\/tasks/.test(c.url)), false);
  assert.ok(index.get('flow-host')._mountSpec.nodes.length >= 7);
});

/* ------------------------------------------------------------------ *
 * TRACE-STARTUP：Trace 首屏依赖规则（真实现场 final-28bbef29 UI shots-trace）
 *
 * 现场：同 document 里 Trace GET 200 / 7 tasks，132.4ms 解析返回，138ms 抛
 * `TypeError: Cannot read properties of undefined (reading 'taskChain')`；
 * DOMContentLoaded 152.5ms 才到。图库是 defer（base.html graph-libs 槽位），
 * defer 脚本在解析完成后、DOMContentLoaded 之前执行；解析期的内联脚本先跑，
 * 所以「Trace 响应比 defer 图库快」时页面会去读还不存在的 window.AnotifyGraphs。
 *
 * 下面的沙箱只为这一条启动依赖规则服务（不改 b2Page）：readyState 与
 * DOMContentLoaded 的时序由测试精确控制，图库在“defer 脚本执行”那一刻才注入。
 * ------------------------------------------------------------------ */
function traceStartup(options = {}) {
  const run = loadMock({ graphs: false, search: '?namespace=default&traceUID=trace_mock_t1' });
  const nodes = new Map();
  function element(tag) {
    const node = fakeNode(tag);
    const listeners = new Map();
    const set = node.setAttribute;
    node.setAttribute = (key, value) => {
      set(key, value);
      if (key === 'id') nodes.set(String(value), node);
    };
    node.addEventListener = (type, callback) => {
      if (!listeners.has(type)) listeners.set(type, []);
      listeners.get(type).push(callback);
    };
    node.fire = async type => {
      for (const callback of listeners.get(type) || []) await callback({ preventDefault() {} });
    };
    return node;
  }
  run.sandbox.document.createElement = element;
  run.sandbox.document.createElementNS = (_, tag) => element(tag);
  run.sandbox.document.createDocumentFragment = () => element('fragment');
  run.sandbox.document.getElementById = id => {
    if (!nodes.has(id)) {
      const node = element('div');
      node.setAttribute('id', id);
    }
    return nodes.get(id);
  };
  run.sandbox.URLSearchParams = URLSearchParams;
  run.sandbox.URL = URL;
  run.sandbox.AnotifyI18n = {};
  run.A.mountLayout = () => {};
  run.A.mountPullRefresh = () => {};
  const state = { readyState: options.readyState || 'loading' };
  Object.defineProperty(run.sandbox.document, 'readyState', { configurable: true, get: () => state.readyState });
  // 只收集 DOMContentLoaded；真实浏览器里 defer 图库恰好在它之前执行完。
  const domReady = [];
  run.sandbox.document.addEventListener = (type, callback, opts) => {
    if (type === 'DOMContentLoaded') domReady.push({ callback, once: !!(opts && opts.once) });
  };
  const calls = [];
  const api = run.A.api;
  run.A.api = async (url, apiOpts) => {
    calls.push({ url });
    return api(url, apiOpts);
  };
  const loadGraphs = () => {
    // 与 b2Page / loadMock({graphs:true}) 同一套 FlowGraph 桩：只记录装配后的 spec。
    vm.runInContext(
      `window.FlowGraph = { mount: function (host, spec) {
        host._mountSpec = spec;
        return { fit: function () {}, replay: function () {}, _debug: { animated: function () { return false; } } };
      } };`,
      run.ctx,
    );
    vm.runInContext(GRAPHS_SRC, run.ctx, { filename: 'web/graphs.js' });
  };
  if (options.graphs) loadGraphs();
  const source = readFileSync(path.join(ROOT, 'web-src/pages/trace.html'), 'utf8');
  for (const script of source.matchAll(/<script>([\s\S]*?)<\/script>/g)) {
    vm.runInContext(script[1], run.ctx, { filename: 'trace.html.inline.js' });
  }
  const settle = async () => { for (let i = 0; i < 8; i++) await new Promise(setImmediate); };
  const fireDomReady = async () => {
    state.readyState = 'interactive';
    const pending = domReady.splice(0);
    for (const { callback } of pending) await callback();
    state.readyState = 'complete';
  };
  const traceCalls = () => calls.filter(c => c.url.includes('/traces/')).length;
  return { ...run, nodes, calls, state, domReady, settle, fireDomReady, loadGraphs, traceCalls, get: id => run.sandbox.document.getElementById(id) };
}

test("TRACE-STARTUP barrier: first render waits for the deferred graph library instead of reading taskChain too early", async () => {
  const run = traceStartup({ readyState: 'loading' });
  await run.settle();
  // 解析期已发出 Trace 请求并拿到 7 tasks —— 真实现场的竞态窗口已经建立。
  assert.equal(run.traceCalls(), 1, 'trace GET already returned while the deferred graph library is still pending');
  assert.equal(run.get('trace-tasks').children.length, 0, 'no section may be rendered before the deferred graph library ran');
  assert.equal(run.get('trace-content').getAttribute('aria-busy'), 'true', 'load stays busy while the startup barrier waits');
  assert.equal(
    b2Text(run.get('trace-error')), '',
    'no error may be shown while the deferred graph library is still pending (original code rendered too early: ' + b2Text(run.get('trace-error')) + ')',
  );
  // defer 图库执行（= 浏览器里 DOMContentLoaded 之前的时刻），随后才触发 DOMContentLoaded。
  run.loadGraphs();
  assert.equal(typeof run.sandbox.AnotifyGraphs.taskChain, 'function');
  assert.equal(run.get('trace-tasks').children.length, 0, 'injecting the library alone must not render anything');
  await run.fireDomReady();
  await run.settle();
  assert.equal(run.traceCalls(), 1, 'the barrier is not another Trace request');
  assert.equal(run.get('trace-tasks').children.length, 7, 'three attempts still one logical task');
  const spec = run.get('trace-graph')._mountSpec;
  assert.ok(spec && spec.nodes.length >= 7, 'the real graphs.js taskChain spec is mounted');
  assert.equal(b2Text(run.get('trace-error')), '', 'no error after the dependency arrived');
  spec.onNodeClick({ id: 'tsk_mock_1' });
  assert.equal(run.sandbox.location.href, 'message.html?namespace=default&id=tsk_mock_1', 'canonical task node link still navigates');
});

test("TRACE-STARTUP failure: missing graph library is a visible real error with no fake rows, and the original retry recovers once the script is available", async () => {
  const run = traceStartup({ readyState: 'loading' });
  await run.settle();
  await run.fireDomReady();
  await run.settle();
  // 图库真缺失：DOMContentLoaded 之后必须明确失败，不能静默删图/假成功。
  assert.match(b2Text(run.get('trace-error')), /trace\.nolib/, 'explicit dependency error is shown');
  assert.equal(run.get('trace-content').children.length, 0, 'no fabricated sections or rows');
  assert.equal(run.traceCalls(), 1);
  const retry = b2Find(run.get('trace-error'), n => n.tagName === 'BUTTON')[0];
  assert.ok(retry, 'the original loadErrorNode retry stays available');
  // 脚本后来恢复可用：真实点击原 retry，必须重新取 API 并画出真图。
  run.loadGraphs();
  await retry.fire('click');
  await run.settle();
  assert.equal(run.traceCalls(), 2, 'retry really issues a fresh Trace GET');
  assert.equal(run.get('trace-tasks').children.length, 7);
  assert.ok(run.get('trace-graph')._mountSpec.nodes.length >= 7, 'retry draws the real taskChain graph');
  assert.equal(b2Text(run.get('trace-error')), '', 'error clears after a successful retry');
  // interactive 不天然保证 defer 完成：既不能错过事件、也不能挂死。
  const interactive = traceStartup({ readyState: 'interactive' });
  await interactive.settle();
  assert.match(b2Text(interactive.get('trace-error')), /trace\.nolib/, 'interactive page fails explicitly instead of waiting for an already-fired event');
  assert.equal(interactive.traceCalls(), 1);
  interactive.loadGraphs();
  const interactiveRetry = b2Find(interactive.get('trace-error'), n => n.tagName === 'BUTTON')[0];
  await interactiveRetry.fire('click');
  await interactive.settle();
  assert.equal(interactive.get('trace-tasks').children.length, 7, 'interactive retry succeeds without any DOMContentLoaded');
});

test("TRACE-STARTUP loaded path: already interactive/complete documents render with the graph and never wait on an event", async () => {
  for (const readyState of ['interactive', 'complete']) {
    const run = traceStartup({ graphs: true, readyState });
    await run.settle();
    assert.equal(run.domReady.length, 0, readyState + ': the startup barrier must not register a DOMContentLoaded listener');
    assert.equal(run.get('trace-tasks').children.length, 7, readyState + ': normal loaded page renders immediately');
    assert.ok(run.get('trace-graph')._mountSpec.nodes.length >= 7, readyState + ': real graph spec mounted');
    assert.equal(b2Text(run.get('trace-error')), '', readyState + ': no error on the normal path');
    assert.equal(run.traceCalls(), 1);
  }
});

test("TRACE-STARTUP native: delayed defer graph library, missing-script retry recovery and loaded page", { timeout: 55000 }, () => {
  let rejected, output;
  try {
    output = execFileSync(process.execPath, [path.join(ROOT, 'scripts/e2e/fixtures/f3/f3-trace-startup.mjs')], {
      cwd: ROOT, timeout: 52000, encoding: 'utf8', maxBuffer: 8 * 1024 * 1024, stdio: ['ignore', 'pipe', 'pipe'],
      env: { ...process.env, E2E_MOCK_ONLY: '1', F3_RUN_ID: 'trace-startup-native-' + globalThis.crypto.randomUUID(), F3_ATTEMPT_ID: '' },
    });
  } catch (error) { rejected = error; output = error.stdout; }
  console.log('TRACE-STARTUP NATIVE COMMAND_EXIT=' + (rejected?.status ?? 0));
  console.log(output || '');
  if (rejected) console.log(rejected.stderr);
  assert.equal(rejected, undefined, 'the native startup dependency cases must pass (see stderr/output above)');
  for (const id of ['trace-startup-delayed-graphs', 'trace-startup-missing-graphs', 'trace-startup-loaded-page']) {
    assert.match(output, new RegExp('TRACE-STARTUP PASS ' + id));
  }
  assert.match(output, /TRACE-STARTUP OK /, 'the helper reports its own source epoch and runId');
});

/* ------------------------------------------------------------------ *
 * SOURCE-STARTUP：源详情页的图库启动依赖（与 TRACE-STARTUP 同一条规则）
 *
 * 现场：source.html 曾在 fetch 续体里同步读 window.AnotifyGraphs。只要 3 个 GET
 * 比 4 个 defer 图库先回来（API 命中缓存 / 弱网 / 移动端），图上就印「图组件未
 * 加载，请重新加载。」，而库往往几十毫秒后就绪——页面却不再重画。
 * 下面的沙箱精确控制 readyState 与 DCL：图库只在「defer 脚本执行」那一刻注入。
 * ------------------------------------------------------------------ */
function sourceStartup(options = {}) {
  const run = loadMock({ graphs: false, ...options, search: options.search || '?namespace=default&name=deploy-hook' });
  const nodes = new Map();
  function element(tag) {
    const node = fakeNode(tag);
    const listeners = new Map();
    const set = node.setAttribute;
    node.setAttribute = (key, value) => {
      set(key, value);
      if (key === 'id') nodes.set(String(value), node);
    };
    // source.html 里 `host.id = "src-flow"` 是**属性赋值**（不是 setAttribute），
    // 断言要拿到这个节点，就得让 id 的赋值也进索引。
    Object.defineProperty(node, 'id', {
      configurable: true,
      get: () => node.getAttribute('id') || '',
      set: value => { set('id', value); nodes.set(String(value), node); },
    });
    node.addEventListener = (type, callback) => {
      if (!listeners.has(type)) listeners.set(type, []);
      listeners.get(type).push(callback);
    };
    node.fire = async type => {
      for (const callback of listeners.get(type) || []) await callback({ preventDefault() {} });
    };
    return node;
  }
  run.sandbox.document.createElement = element;
  run.sandbox.document.createElementNS = (_, tag) => element(tag);
  run.sandbox.document.createDocumentFragment = () => element('fragment');
  run.sandbox.document.getElementById = id => {
    if (!nodes.has(id)) {
      const node = element('div');
      node.setAttribute('id', id);
    }
    return nodes.get(id);
  };
  run.sandbox.URLSearchParams = URLSearchParams;
  run.sandbox.URL = URL;
  run.sandbox.AnotifyI18n = {};
  run.A.mountLayout = () => {};
  run.A.mountPullRefresh = () => {};
  const state = { readyState: options.readyState || 'loading' };
  Object.defineProperty(run.sandbox.document, 'readyState', { configurable: true, get: () => state.readyState });
  // 只收集 DOMContentLoaded；真实浏览器里 defer 图库恰好在它之前执行完。
  const domReady = [];
  run.sandbox.document.addEventListener = (type, callback) => {
    if (type === 'DOMContentLoaded') domReady.push(callback);
  };
  const calls = [];
  const api = run.A.api;
  run.A.api = async (url, opts) => {
    calls.push({ url });
    return api(url, opts);
  };
  const loadGraphs = () => {
    vm.runInContext(
      `window.FlowGraph = { mount: function (host, spec) {
        host._mountSpec = spec;
        return { fit: function () {}, replay: function () {}, _debug: { animated: function () { return false; } } };
      } };`,
      run.ctx,
    );
    vm.runInContext(GRAPHS_SRC, run.ctx, { filename: 'web/graphs.js' });
  };
  if (options.graphs) loadGraphs();
  const source = readFileSync(path.join(ROOT, 'web-src/pages/source.html'), 'utf8');
  for (const script of source.matchAll(/<script>([\s\S]*?)<\/script>/g)) {
    vm.runInContext(script[1], run.ctx, { filename: 'source.html.inline.js' });
  }
  const settle = async () => { for (let i = 0; i < 12; i++) await new Promise(setImmediate); };
  const fireDomReady = async () => {
    state.readyState = 'interactive';
    const pending = domReady.splice(0);
    for (const callback of pending) await callback();
    state.readyState = 'complete';
  };
  return { ...run, nodes, calls, state, domReady, settle, fireDomReady, loadGraphs,
    get: id => run.sandbox.document.getElementById(id) };
}

test("SOURCE-STARTUP barrier: graph area waits for the deferred library instead of printing the stale error", async () => {
  const run = sourceStartup({ readyState: 'loading' });
  await run.settle();
  // 竞态窗口已经建立：图容器已经进了 DOM（= 3 个 GET 都回来了、render 走到了画图那一行），
  // 而 defer 图库还没执行。
  const host = run.get('src-flow');
  assert.ok(host, 'the graph host is in the document');
  assert.equal(host.children.length, 0, 'nothing may be drawn before the deferred graph library ran');
  assert.equal(b2Text(host), '', 'no error while the deferred library is still pending');
  // defer 图库执行（= 浏览器里 DCL 之前的时刻），再触发 DCL。
  run.loadGraphs();
  assert.equal(host.children.length, 0, 'injecting the library alone must not draw');
  await run.fireDomReady();
  await run.settle();
  assert.equal(b2Text(host), '', 'no error after the dependency arrived');
  assert.ok(host._mountSpec && host._mountSpec.nodes.length >= 2, 'the real capabilityFlow spec is mounted');
});

test("SOURCE-STARTUP failure: missing library is a visible retryable error and the original retry recovers", async () => {
  const run = sourceStartup({ readyState: 'loading' });
  await run.settle();
  await run.fireDomReady();
  await run.settle();
  const host = run.get('src-flow');
  assert.match(b2Text(host), /graph_error/, 'explicit dependency error is shown');
  assert.equal(host._mountSpec, undefined, 'no fabricated graph');
  const retry = b2Find(host, n => n.tagName === 'BUTTON')[0];
  assert.ok(retry, 'the retry button from loadErrorNode stays available');
  // 库晚到：点原 retry（= 重新 load 整页），必须真的画出图并清掉错误。
  run.loadGraphs();
  await retry.fire('click');
  await run.settle();
  const redrawn = run.get('src-flow');
  assert.ok(redrawn._mountSpec && redrawn._mountSpec.nodes.length >= 2, 'retry draws the real graph');
  assert.equal(b2Text(redrawn).includes('graph_error'), false, 'error clears after the library is available');
});

test("SOURCE-STARTUP loaded path: already interactive/complete documents never wait on an event", async () => {
  for (const readyState of ['interactive', 'complete']) {
    const run = sourceStartup({ graphs: true, readyState });
    await run.settle();
    assert.equal(run.domReady.length, 0, readyState + ': no DOMContentLoaded listener when the document is already past parsing');
    const host = run.get('src-flow');
    assert.ok(host._mountSpec && host._mountSpec.nodes.length >= 2, readyState + ': normal loaded page draws immediately');
    assert.equal(b2Text(host), '', readyState + ': no error on the normal path');
  }
});

test("PAGE-GUARD: no page template reads AnotifyGraphs directly or calls graphLib without arming the gate", async () => {
  const guard = await import(path.join(ROOT, 'scripts/check-graph-access.mjs'));
  for (const page of ['source', 'trace', 'index']) {
    const html = readFileSync(path.join(ROOT, 'web-src/pages/' + page + '.html'), 'utf8');
    const violations = guard.scanTemplate(page + '.html', html);
    assert.deepEqual(violations, [], page + ' must go through Anotify.graphLib() with expectGraphics() armed');
  }
});

test("F2b2 four-page/four-language keys and forbidden old UI guard complement behavior", () => {
  function keys(lang) {
    const source = readFileSync(path.join(ROOT, 'web-src/locales/' + lang + '.yaml'), 'utf8');
    const out = new Set();
    const stack = [];
    for (const line of source.split('\n')) {
      const match = /^(\s*)([A-Za-z0-9_-]+):(?:\s+(.*))?$/.exec(line);
      if (!match) continue;
      const depth = match[1].length;
      while (stack.length && stack.at(-1).depth >= depth) stack.pop();
      const key = [...stack.map(s => s.key), match[2]].join('.');
      if (match[3]) out.add(key); else stack.push({ key: match[2], depth });
    }
    return out;
  }
  const locales = ['zh-CN', 'en', 'ja', 'es'].map(keys);
  for (const prefix of ['events.v4.', 'trace.v4.', 'message.v4.']) {
    const expected = [...locales[0]].filter(k => k.startsWith(prefix)).sort();
    assert.ok(expected.length);
    for (const actual of locales) assert.deepEqual([...actual].filter(k => k.startsWith(prefix)).sort(), expected);
  }
  for (const page of ['index', 'events', 'trace', 'message']) {
    const source = readFileSync(path.join(ROOT, 'web-src/pages/' + page + '.html'), 'utf8');
    assert.doesNotMatch(source, /nextCursor|sinceId|sourcePort|\/v1\/tasks[/?]|\/v1\/sources[/?]|\/v1\/notifications/);
    for (const match of source.matchAll(/(?:\bt\(|\{\{t\s+)"([\w.-]+)"/g)) {
      const dynamic = {
        'message.v4.phase.': ['pending', 'running', 'succeeded', 'failed', 'cancelled', 'abandoned'],
        'message.v4.confirm_': ['cancel', 'abandon'],
        'message.v4.': ['cancel', 'abandon'],
      };
      const requested = dynamic[match[1]]
        ? dynamic[match[1]].map(suffix => match[1] + suffix) : [match[1]];
      for (const key of requested) {
        for (const locale of locales) assert.ok(locale.has(key), page + ' missing key ' + key);
      }
    }
  }
});

test("F2b2 controllers: offline/error/empty are distinct, never automatic mock fallback", async () => {
  const failed = await b2Page('events', '?namespace=default', run => {
    const fetch = run.sandbox.fetch;
    run.sandbox.fetch = (url, opts) => String(url).includes('/console/events')
      ? Promise.reject(new Error('Disconnected fixture')) : fetch(url, opts);
  });
  assert.ok(failed.get('ev-error').children.length);
  assert.equal(failed.get('ev-list').children.length, 0);
  assert.equal(b2Text(failed.get('ev-error')).includes('events.v4.empty'), false);
  const empty = await b2Page('events', '?namespace=default', ({ Mock }) => { Mock.state.events = []; });
  assert.match(b2Text(empty.get('ev-list')), /events.v4.empty/);
  assert.equal(empty.get('ev-error').children.length, 0);
  const traceFailure = await b2Page('trace', '?namespace=prod&traceUID=trace_mock_t1');
  assert.equal(traceFailure.get('trace-content').children.length, 0);
  assert.ok(traceFailure.get('trace-error').children.length);
  const missing = await b2Page('message', '?namespace=prod&id=tsk_mock_2');
  assert.match(b2Text(missing.get('msg-error')), /message.v4.not_found/);
});

test("F2b2 final index: event read failure is graph error, actual Source root, no invented execution trends", async () => {
  const failed = await b2Page('index', '?namespace=default', run => {
    const fetch = run.sandbox.fetch;
    run.sandbox.fetch = (url, opts) => String(url).includes('/console/events')
      ? Promise.reject(new Error('Disconnected event fixture')) : fetch(url, opts);
  });
  assert.equal(b2Text(failed.get('flow-host')).includes('index.flow.empty'), false,
    'A failed event read is not an empty trace');
  assert.ok(failed.get('flow-host').children.length);
  let sourceLabel;
  await b2Page('index', '?namespace=default', run => {
    const render = run.sandbox.AnotifyGraphs.taskChain;
    run.sandbox.AnotifyGraphs.taskChain = (host, options) => {
      sourceLabel = options.sourceLabel;
      return render(host, options);
    };
  });
  assert.equal(sourceLabel, 'deploy-hook');
  const source = readFileSync(path.join(ROOT, 'web-src/pages/index.html'), 'utf8');
  assert.equal((source.match(/data-spark-scope="events"/g) || []).length, 2);
  assert.match(source, /node.dataset.sparkScope !== "events"/);
});

test("F2b2 final Task action409: concurrent real cancel cannot paint abandon success; refresh shows terminal", async () => {
  const run = await b2Page('message', '?namespace=default&id=tsk_mock_5');
  const cancelled = await run.req('/v1/namespaces/default/tasks/tsk_mock_5/cancel', { method: 'POST', body: '{}' });
  assert.equal(cancelled.body.status.phase, 'cancelled');
  await run.get('task-abandon').fire('click'); await run.settle();
  assert.ok(run.get('task-action-error').children.length);
  assert.equal(run.get('task-phase').getAttribute('data-phase'), 'pending', 'No optimistic abandon terminal');
  assert.equal(run.Mock.state.tasks.find(task => task.name === 'tsk_mock_5').phase, 'cancelled');
  await run.get('msg-refresh').fire('click'); await run.settle();
  assert.equal(run.get('task-phase').getAttribute('data-phase'), 'cancelled');
  assert.equal(run.get('task-abandon').disabled, true);
});

/* F2c1: self-contained packages, pure render, atomic additive install. */
function playbookFiles(options = {}) {
	const schema = { type: "object", additionalProperties: false, required: ["project"], properties: {
		project: { type: "string", pattern: "^[a-z][a-z0-9-]*$" },
		env: { type: "string", default: "prod" },
		credential: { type: "string", writeOnly: true },
	} };
	const cap = (name, next = []) => ({ metadata: { name }, spec: { next, run: {
		entrypoint: "run.sh", files: { "run.sh": "#!/bin/sh\necho '{{values.project}} {{values.env}}'\n" },
	} } });
	const documents = {
		"playbook.yaml": { apiVersion: "anotify/v1", kind: "Playbook", metadata: { name: options.slug || "project-chain", version: 1 },
			spec: { values: schema, capabilities: ["capabilities/review.yaml", "capabilities/fixer.yaml"], sources: ["sources/on-push.yaml"] } },
		"capabilities/review.yaml": cap("{{values.project}}-review", ["{{values.project}}-fixer"]),
		"capabilities/fixer.yaml": cap("{{values.project}}-fixer"),
		"sources/on-push.yaml": { metadata: { name: "{{values.project}}-push" }, spec: {
			kind: "webhook", config: {}, targets: [{ capability: "{{values.project}}-review", params: { repo: "$(payload.repo)", env: "{{values.env}}" } }],
		} },
	};
	options.edit?.(documents);
	return Object.fromEntries(Object.entries(documents).map(([p, d]) => [p, JSON.stringify(d, null, 2)]));
}
function pbWrite(files, ns = "default", rv) {
	const root = JSON.parse(files["playbook.yaml"]);
	return { apiVersion: "anotify/v1", kind: "Playbook", metadata: { name: root.metadata.name, namespace: ns,
		...(rv == null ? {} : { resourceVersion: rv }) }, spec: { files } };
}
async function pbCreate(req, files = playbookFiles(), ns = "default") {
	const result = await req(`/v1/namespaces/${ns}/playbooks`, { method: "POST", body: JSON.stringify(pbWrite(files, ns)) });
	assert.equal(result.status, 201, JSON.stringify(result.body));
	return result.body;
}
async function pbAction(req, action, values = { project: "a" }, extra = {}, slug = "project-chain", ns = "default") {
	return req(`/v1/namespaces/${ns}/playbooks/${slug}/${action}`, { method: "POST", body: JSON.stringify({ values, ...extra }) });
}
function allFacts(Mock) { return JSON.stringify(Mock.state); }
function pbRawEdit(files, edit) {
	const docs = Object.fromEntries(Object.entries(files).map(([p, s]) => [p, JSON.parse(s)]));
	edit(docs);
	return Object.fromEntries(Object.entries(docs).map(([p, d]) => [p, JSON.stringify(d, null, 2)]));
}
async function pbReplace(req, res, files) {
	return req(`/v1/namespaces/${res.metadata.namespace}/playbooks/${res.metadata.name}`, {
		method: "PUT", body: JSON.stringify(pbWrite(files, res.metadata.namespace, res.metadata.resourceVersion)),
	});
}

test("F2c1 package: W3 summary, raw spec.files roundtrip and whole-template CAS retain all facts", async () => {
	const { req, Mock, A } = loadMock();
	const files = playbookFiles(); const deployments = JSON.stringify(Mock.state.deployments); const created = await pbCreate(req, files);
	const base = "/v1/namespaces/default/playbooks/project-chain";
	assert.deepEqual((await req(base)).body.spec, {});
	const list = (await req("/v1/namespaces/default/playbooks")).body;
	assert.equal(list.kind, "PlaybookList"); assert.deepEqual(list.items[0].spec, {});
	const raw = (await req(base + "?includeBody=true")).body;
	assert.deepEqual(raw.spec.files, files); assert.deepEqual(plain(A.model.playbook(raw)).files, files);
	const before = allFacts(Mock);
	assert.equal((await req("/v1/namespaces/default/playbooks", { method: "POST", body: JSON.stringify(pbWrite(files)) })).body.reason, "slug_taken");
	assert.equal(allFacts(Mock), before);
	assert.equal((await req(base, { method: "PUT", body: JSON.stringify(pbWrite(files)) })).body.reason, "resource_version_required");
	const changed = pbRawEdit(files, d => d["capabilities/review.yaml"].spec.run.files["run.sh"] += "echo upgraded\n");
	const saved = await pbReplace(req, created, changed); assert.equal(saved.status, 200);
	assert.equal((await pbReplace(req, created, files)).status, 409);
	assert.deepEqual((await req(base + "?includeBody=true")).body.spec.files, changed);
	assert.equal(Mock.state.playbookInstalls.length, 0); assert.equal(JSON.stringify(Mock.state.deployments), deployments);
});

test("F2c1 package: files/source exclusive, namespace/path/reference rejection is atomic", async () => {
	const { req, Mock } = loadMock(); const files = playbookFiles(); const before = allFacts(Mock);
	const cases = [
		[Object.assign(pbWrite(files), { spec: { files, source: "oci://registry/pkg:1" } }), "invalid_playbook"],
		[pbWrite({ ...files, "../escape.txt": "bad" }), "file_path_invalid"],
		[pbWrite({ ...files, "orphan.yaml": "{}" }), "file_unreferenced"],
		[pbWrite(pbRawEdit(files, d => d["playbook.yaml"].spec.sources.push("missing.yaml"))), "file_missing"],
		[pbWrite(pbRawEdit(files, d => d["playbook.yaml"].spec.capabilities.push("capabilities/review.yaml"))), "file_duplicate"],
		[pbWrite(pbRawEdit(files, d => d["capabilities/review.yaml"].metadata.namespace = "prod")), "namespace_in_package"],
	];
	for (const [body, reason] of cases) {
		const result = await req("/v1/namespaces/default/playbooks", { method: "POST", body: JSON.stringify(body) });
		assert.equal(result.status, 400); assert.equal(result.body.reason, reason); assert.equal(allFacts(Mock), before);
	}
	const mismatch = await req("/v1/namespaces/prod/playbooks", { method: "POST", body: JSON.stringify(pbWrite(files)) });
	assert.equal(mismatch.body.reason, "namespace_mismatch");
	assert.equal((await req("/v1/namespaces/absent/playbooks")).status, 404);
});

test("F2c1 render: defaults, typed tree interpolation, literal roots and no conflict DB access/write", async () => {
	const { req, Mock } = loadMock();
	const files = playbookFiles({ edit: d => {
		d["playbook.yaml"].spec.values.properties.n = { type: "integer", default: 3 };
		d["capabilities/review.yaml"].spec.concurrency = { maxPerNode: "{{values.n}}" };
		d["capabilities/review.yaml"].spec.run.files["run.sh"] += "echo '{{payload.x}} $(payload.x) $NAME ${NAME}'\n";
	} });
	await pbCreate(req, files); const before = allFacts(Mock);
	const result = await pbAction(req, "render", { project: "a" }); assert.equal(result.status, 200);
	assert.equal(result.body.values.env, "prod"); assert.equal(result.body.capabilities[0].spec.concurrency.maxPerNode, 3);
	assert.match(result.body.files["capabilities/review.yaml"], /\{\{payload.x\}\}/);
	assert.match(result.body.files["sources/on-push.yaml"], /\$\(payload.repo\)/);
	// 平铺 RenderResult 的 files 只含被引用的能力/源文件：playbook.yaml 不渲染、
	// 也不出现在 render 响应里（要看原文走 GET playbook?includeBody=true）。
	assert.equal("playbook.yaml" in result.body.files, false, "playbook.yaml 不渲染，故不在 render.files 里");
	assert.ok(result.body.valuesDigest.startsWith("sha256:")); assert.equal(allFacts(Mock), before);
	const original = Mock.state.sources;
	Mock.state.sources = new Proxy([], { get() { throw new Error("render read conflict DB"); } });
	assert.equal((await pbAction(req, "render")).status, 200);
	Mock.state.sources = original;
	assert.equal((await pbAction(req, "render", {})).body.reason, "values_missing");
	assert.equal((await pbAction(req, "render", { project: "BAD" })).body.reason, "values_invalid");
});

test("F2c1 dryRun: same receipt plan but ALL state stays unchanged and no installId persists", async () => {
	const { req, Mock } = loadMock(); await pbCreate(req); const before = allFacts(Mock); const deployments = JSON.stringify(Mock.state.deployments);
	const dry = await pbAction(req, "install", { project: "a" }, { dryRun: true });
	assert.equal(dry.status, 200); assert.equal(dry.body.kind, "InstallReceipt"); assert.equal(dry.body.install.dryRun, true);
	assert.deepEqual(dry.body.created, { capabilities: ["a-review@1", "a-fixer@1"], sources: ["a-push"] });
	assert.equal(allFacts(Mock), before); assert.equal((await req("/v1/namespaces/default/playbooks/project-chain/installs")).body.total, 0);
	const installed = await pbAction(req, "install"); assert.equal(installed.status, 200);
	assert.deepEqual(installed.body.created, dry.body.created); assert.equal(installed.body.install.id, "ins_mock_1", "dry run must not consume persistent ID sequence");
	assert.equal(JSON.stringify(Mock.state.deployments), deployments); assert.equal(Mock.state.playbookInstalls.length, 1);
});

test("F2c1 installs: ordinary repeat ALWAYS new receipt, digest groups and single receipt/provenance retained", async () => {
	const { req, Mock } = loadMock(); await pbCreate(req);
	const first = (await pbAction(req, "install")).body; const second = (await pbAction(req, "install")).body;
	assert.notEqual(first.install.id, second.install.id); assert.equal(first.valuesDigest, second.valuesDigest);
	assert.deepEqual(second.created, { capabilities: [], sources: [] }); assert.deepEqual(second.skipped, first.created);
	assert.equal(Mock.capByName("a-review", "default").installedFrom, first.install.id);
	assert.equal(Mock.state.sources.find(s => s.name === "a-push").installedFrom, first.install.id);
	await pbAction(req, "install", { project: "b" });
	const groups = (await req("/v1/namespaces/default/playbooks/project-chain/installs")).body;
	assert.equal(groups.total, 2); const a = groups.installs.find(g => g.values.project === "a");
	assert.equal(a.attempts, 2); assert.equal(a.lastInstallId, second.install.id); assert.ok(a.firstAt && a.lastAt);
	assert.deepEqual(a.capabilities, first.created.capabilities); assert.deepEqual(a.sources, first.created.sources);
	Mock.state.events = []; Mock.state.tasks = [];
	assert.deepEqual((await req(`/v1/namespaces/default/playbooks/project-chain/installs/${first.install.id}`)).body, first);
});

test("F2c1 matrix: foldKey reuses ANY matching version, allocates max+1, explicit versions immutable", async () => {
	const { req, Mock } = loadMock(); const files = playbookFiles(); let pb = await pbCreate(req, files);
	const first = (await pbAction(req, "install")).body;
	const next = pbRawEdit(files, d => d["capabilities/review.yaml"].spec.run.files["run.sh"] += "echo v2\n");
	pb = (await pbReplace(req, pb, next)).body;
	const upgrade = (await pbAction(req, "install")).body;
	assert.ok(upgrade.created.capabilities.includes("a-review@2")); assert.equal(upgrade.upgraded[0].from, 1); assert.equal(upgrade.upgraded[0].to, 2);
	assert.deepEqual(Object.keys(upgrade.upgraded[0].affects).sort(), ["edges", "sources"]);
	pb = (await pbReplace(req, pb, files)).body;
	const reuse = (await pbAction(req, "install")).body; assert.ok(reuse.skipped.capabilities.includes("a-review@1"));
	assert.equal(Mock.capByName("a-review", "default").version, 2); assert.equal(Mock.capByName("a-review", "default").installedFrom, upgrade.install.id);
	const explicit = pbRawEdit(next, d => d["capabilities/review.yaml"].metadata.version = 7);
	pb = (await pbReplace(req, pb, explicit)).body;
	assert.ok((await pbAction(req, "install")).body.created.capabilities.includes("a-review@7"));
	const clash = pbRawEdit(explicit, d => d["capabilities/review.yaml"].spec.run.files["run.sh"] += "echo forbidden\n");
	pb = (await pbReplace(req, pb, clash)).body; const before = allFacts(Mock);
	const rejected = await pbAction(req, "install"); assert.equal(rejected.status, 400);
	// 纯能力冲突的**顶层** reason 是 version_content_immutable（playbook.conflictError）；
	// 只有出现源冲突才是 playbook_conflict。逐条明细在 details.conflicts[].reason。
	assert.equal(rejected.body.reason, "version_content_immutable");
	assert.equal(rejected.body.details.conflicts[0].reason, "version_conflict");
	assert.equal(rejected.body.details.conflicts[0].kind, "capability"); assert.ok(rejected.body.details.conflicts[0].declaredDigest); assert.ok(rejected.body.details.conflicts[0].actualDigest);
	assert.ok(rejected.body.details.hint); assert.equal(allFacts(Mock), before);
	assert.ok(first.install.id);
});

test("F2c1 matrix: next/targets.params never edited on reuse, unapplied declared/actual and not-ready checkup", async () => {
	const { req, Mock } = loadMock(); const files = playbookFiles(); const pb = await pbCreate(req, files); await pbAction(req, "install");
	const oldCap = JSON.stringify(Mock.capByName("a-review", "default")); const oldSource = JSON.stringify(Mock.state.sources.find(s => s.name === "a-push"));
	const edit = pbRawEdit(files, d => {
		d["capabilities/review.yaml"].spec.next = ["notify"];
		d["sources/on-push.yaml"].spec.targets[0].params.env = "different";
	});
	assert.equal((await pbReplace(req, pb, edit)).status, 200);
	const result = (await pbAction(req, "install")).body;
	assert.deepEqual(result.unapplied.map(u => u.reason), ["routing_unapplied", "targets_unapplied"]);
	assert.deepEqual(result.unapplied[0].declared, ["notify"]); assert.deepEqual(result.unapplied[0].actual, ["a-fixer"]);
	assert.equal(result.unapplied[1].actual[0].params.env, "prod");
	assert.equal(JSON.stringify(Mock.capByName("a-review", "default")), oldCap); assert.equal(JSON.stringify(Mock.state.sources.find(s => s.name === "a-push")), oldSource);
	const status = (await req("/v1/namespaces/default/playbooks/project-chain")).body.status;
	assert.equal(status.ready, false); assert.equal(status.unapplied.length, 2); assert.ok(status.items.some(i => i.kind === "source" && i.state === "unapplied"));
	assert.equal(status.lastInstallId, result.install.id); assert.equal(status.conditions.length, 4); assert.ok(status.blocker);
});

test("F2c1 matrix: normalized defaults/token omitted reuse, true config/kind conflict gives atomic ALL-state zero writes", async () => {
	const { req, Mock } = loadMock(); const files = playbookFiles(); let pb = await pbCreate(req, files); await pbAction(req, "install");
	const normalized = pbRawEdit(files, d => d["sources/on-push.yaml"].spec.enabled = true);
	pb = (await pbReplace(req, pb, normalized)).body;
	assert.equal((await pbAction(req, "install")).status, 200);
	for (const kind of [false, true]) {
		const clash = pbRawEdit(normalized, d => {
			d["capabilities/fixer.yaml"].metadata.name = "brand-new";
			if (kind) { d["sources/on-push.yaml"].spec.kind = "manual"; d["sources/on-push.yaml"].spec.config = { payload: {} }; }
			else d["sources/on-push.yaml"].spec.config.token = "explicit-different-secret-sentinel";
		});
		pb = (await pbReplace(req, pb, clash)).body; const before = allFacts(Mock);
		for (const dryRun of [false, true]) {
			const rejected = await pbAction(req, "install", { project: "a" }, { dryRun });
			assert.equal(rejected.status, 400); assert.equal(rejected.body.kind, "Status"); assert.equal(rejected.body.code, 400); assert.equal(rejected.body.reason, "playbook_conflict");
			assert.ok(rejected.body.details.valuesDigest && rejected.body.details.hint); assert.equal(rejected.body.details.conflicts[0].kind, "source");
			assert.ok(rejected.body.details.conflicts[0].fields.includes(kind ? "kind" : "config.token"));
			assert.ok(!JSON.stringify(rejected.body).includes("explicit-different-secret-sentinel")); assert.equal(allFacts(Mock), before);
		}
	}
});

test("F2c1 credentials: exact writeOnly masks in receipt/history/status; resource auth config persists normally", async () => {
	const { req, Mock } = loadMock(); const files = playbookFiles({ edit: d => {
		d["sources/on-push.yaml"].spec.config.secret = "{{values.credential}}";
		d["sources/on-push.yaml"].spec.config.token = "fixed-token-pair-sentinel";
	} }); await pbCreate(req, files);
	const values = { project: "a", credential: "hmac-pair-secret-sentinel" };
	const receipt = (await pbAction(req, "install", values)).body;
	assert.equal(receipt.values.credential, "***"); assert.equal(Mock.state.sources.find(s => s.name === "a-push").config.secret, values.credential);
	assert.equal(Mock.state.playbookInstalls[0].values.credential, "***");
	for (const suffix of ["", "/installs", "/installs/" + receipt.install.id]) {
		const publicRead = (await req("/v1/namespaces/default/playbooks/project-chain" + suffix)).body;
		assert.ok(!JSON.stringify(publicRead).includes(values.credential)); assert.ok(!JSON.stringify(publicRead).includes("fixed-token-pair-sentinel"));
	}
	const source = (await req("/v1/namespaces/default/sources/a-push")).body;
	assert.equal(source.spec.config.secretSet, true); assert.equal(source.spec.config.secret, undefined); assert.equal(source.spec.config.token, undefined);
	assert.ok(!JSON.stringify(Mock.state.playbookInstalls).includes(values.credential));
});

test("F2c1 references/notes: pending and external facts, six-code vocabulary, no deployments", async () => {
	const { req, Mock } = loadMock(); const files = playbookFiles({ edit: d => {
		d["capabilities/review.yaml"].spec.next = ["notify", "absent-cap"];
		d["sources/on-push.yaml"].spec.enabled = false;
		d["sources/on-push.yaml"].spec.targets = [{ capability: "absent-cap" }];
		d["capabilities/review.yaml"].spec.requires = { commands: ["not-installed"] };
		d["capabilities/fixer.yaml"].spec.requires = { commands: ["not-installed"] };
	} }); await pbCreate(req, files); const deployments = JSON.stringify(Mock.state.deployments);
	const result = (await pbAction(req, "install")).body;
	assert.ok(result.pending.includes("absent-cap"));
	// external 里是**包里写的那一串**（plan.go·resolveRefs 原样收集），不补 `@版本`。
	assert.ok(result.external.includes("notify"));
	assert.ok(result.notes.some(n => n.code === "pending_ref")); assert.ok(result.notes.some(n => n.code === "source_disabled"));
	assert.ok(result.notes.some(n => n.code === "unreachable")); assert.ok(result.notes.some(n => n.code === "no_node"));
	assert.ok(result.notes.every(n => ["routing_unapplied", "targets_unapplied", "unreachable", "pending_ref", "source_disabled", "no_node"].includes(n.code)));
	assert.equal(JSON.stringify(Mock.state.deployments), deployments);
});

test("F2c1 delete: W1 missing/stale CAS, only manifest deleted, kept resources/values/receipts unchanged", async () => {
	const { req, Mock } = loadMock(); const pb = await pbCreate(req); const receipt = (await pbAction(req, "install")).body;
	const base = "/v1/namespaces/default/playbooks/project-chain";
	assert.equal((await req(base, { method: "DELETE" })).body.reason, "resource_version_required");
	assert.equal((await req(base + "?resourceVersion=old", { method: "DELETE" })).status, 409);
	const resources = JSON.stringify([Mock.state.capabilities, Mock.state.sources, Mock.state.playbookInstalls]);
	const deleted = await req(base + "?resourceVersion=" + pb.metadata.resourceVersion, { method: "DELETE" });
	assert.equal(deleted.status, 200); assert.equal(deleted.body.deleted, true); assert.deepEqual(deleted.body.kept, receipt.created);
	assert.equal(deleted.body.values[0].project, "a"); assert.ok(deleted.body.note);
	assert.equal(JSON.stringify([Mock.state.capabilities, Mock.state.sources, Mock.state.playbookInstalls]), resources);
	assert.equal((await req(base)).status, 404);
	assert.deepEqual((await req(base + "/installs/" + receipt.install.id)).body, receipt, "retained receipt remains addressable");
});

test("F2c1 parser: frozen authored block/flow/quoted/literal package uses ONE shared normalization and raw bytes", async () => {
	const { req, A } = loadMock();
	const root = `apiVersion: anotify/v1\nkind: Playbook\nmetadata:\n  name: yaml-chain\n  displayName: 'A: chain'\nspec:\n  values:\n    type: object\n    required: [project]\n    additionalProperties: false\n    properties:\n      project: { type: string, pattern: '^[a-z]+$' }\n      env: { type: string, enum: [dev, prod], default: prod }\n  capabilities:\n    - capabilities/review.yaml\n  sources:\n    - sources/on-push.yaml\n`;
	const cap = `metadata:\n  name: '{{values.project}}-review'\nspec:\n  next: []\n  run:\n    entrypoint: run.sh\n    files:\n      run.sh: |\n        #!/bin/sh\n        echo "project={{values.project}} env={{values.env}}"\n`;
	const src = `metadata:\n  name: '{{values.project}}-push'\nspec:\n  kind: webhook\n  config: {}\n  targets:\n    - capability: '{{values.project}}-review'\n      params: { repo: '$(payload.repo)' }\n`;
	const files = { "playbook.yaml": root, "capabilities/review.yaml": cap, "sources/on-push.yaml": src };
	const parsed = plain(A.model.playbook.parseDocument(root));
	assert.equal(parsed.spec.values.properties.env.default, "prod");
	assert.equal(plain(A.model.playbook.parseDocument(cap)).spec.run.files["run.sh"], '#!/bin/sh\necho "project={{values.project}} env={{values.env}}"\n');
	assert.deepEqual(plain(A.model.playbook.parseDocument(src)).spec.targets[0].params, { repo: "$(payload.repo)" });
	const body = { apiVersion: "anotify/v1", kind: "Playbook", metadata: { name: "yaml-chain" }, spec: { files } };
	assert.equal((await req("/v1/namespaces/default/playbooks", { method: "POST", body: JSON.stringify(body) })).status, 201);
	const raw = (await req("/v1/namespaces/default/playbooks/yaml-chain?includeBody=true")).body;
	assert.deepEqual(raw.spec.files, files); assert.deepEqual(plain(A.model.playbook(raw)).valuesSchema, parsed.spec.values);
	const rendered = await pbAction(req, "render", { project: "a" }, {}, "yaml-chain"); assert.equal(rendered.status, 200);
	assert.equal(rendered.body.capabilities[0].metadata.name, "a-review");
	assert.equal("playbook.yaml" in rendered.body.files, false, "playbook.yaml 不渲染，故不在 render.files 里");
	assert.throws(() => A.model.playbook.parseDocument("spec: &anchor {}"), /anchors/);
	assert.throws(() => A.model.playbook.parseDocument("spec: [unterminated"), /unterminated/);
	assert.throws(() => A.model.playbook.parseDocument("metadata:\n  name: a\n  name: b"), /duplicate/);
	const legacy = plain(A.model.playbook({ spec: { values: parsed.spec.values }, files }));
	assert.equal(legacy.valuesSchema, null); assert.equal("files" in legacy, false, "no second manifest or res.files adapter");
});

test("F2c1 package render errors: duplicate names, cycles and invalid typed name reject without changing ALL state", async () => {
	for (const [edit, reason] of [
		[d => d["capabilities/fixer.yaml"].metadata.name = "{{values.project}}-review", "duplicate_capability"],
		[d => d["capabilities/fixer.yaml"].spec.next = ["{{values.project}}-review"], "cycle"],
		[d => { d["playbook.yaml"].spec.values.properties.n = { type: "integer", default: 3 }; d["capabilities/fixer.yaml"].metadata.name = "{{values.n}}"; }, "render_type_mismatch"],
	]) {
		const { req, Mock } = loadMock(); await pbCreate(req, playbookFiles({ edit })); const before = allFacts(Mock);
		assert.equal((await pbAction(req, "render")).body.reason, reason);
		assert.equal((await pbAction(req, "install")).body.reason, reason); assert.equal(allFacts(Mock), before);
	}
});

/* F2c2: Node/config closure. These requests exercise genuine mock mutations. */
const c2NodeBody = (name, spec = {}, rv) => ({ apiVersion: "anotify/v1", kind: "Node",
 metadata: { name, ...(rv == null ? {} : { resourceVersion: rv }) }, spec });

test("F2c2: offline Node creation precedes long-lived identity token; duplicate409/read masked/rotate/notfound", async () => {
 const { req, Mock } = loadMock();
 const create = await req("/v1/nodes", jsonInit("POST", c2NodeBody("new-node")));
 assert.equal(create.status, 201); assert.equal(create.body.kind, "Node");
 assert.deepEqual(create.body.spec, { namespaces: ["*"], vars: {}, limits: { total: 1, perCapability: {} }, tags: [] });
 assert.equal(create.body.metadata.namespace, undefined); assert.equal(create.body.status.online, false);
 assert.equal((await req("/v1/nodes", jsonInit("POST", c2NodeBody("new-node")))).status, 409);
 assert.equal((await req("/v1/nodes/ghost/tokens", jsonInit("POST", {}))).status, 404);
 const issue = () => req("/v1/nodes/new-node/tokens", jsonInit("POST", {}));
 const one = (await issue()).body, two = (await issue()).body;
 assert.deepEqual(Object.keys(one).sort(), ["apiVersion", "kind", "node", "token"]);
 assert.equal(one.kind, "NodeToken"); assert.deepEqual(one.node, { name: "new-node" }); assert.ok(one.token);
 assert.notEqual(one.token, two.token); assert.equal(Mock.state.nodes.find(n => n.name === "new-node").identityToken, two.token);
 for (const path of ["/v1/nodes", "/v1/nodes/new-node", "/v1/nodes/new-node/config"]) {
  const read = await req(path); assert.equal(read.status, 200);
  assert.ok(!JSON.stringify(read.body).includes(one.token)); assert.ok(!JSON.stringify(read.body).includes(two.token));
 }
 assert.equal((await req("/v1/nodes/new-node/tokens")).status, 501, "NodeToken has no GET route; missing-endpoint guard remains");
});

test("F2c2: namespace intersection, per-key vars context, two-layer limits, revisions and A3 presence", async () => {
 const { req, Mock, A } = loadMock(); const row = Mock.state.nodes[0];
 row.spec.namespaces = ["*"]; row.declared = ["default", "prod"];
 row.spec.vars = { SOURCE_ROOT: "/node", ONLY_NODE: "node" }; row.facts.localVars = { SOURCE_ROOT: "/local" };
 row.spec.limits = { total: 8, perCapability: { "*": 3, fixer: 2 } }; row.facts.localLimits = { total: 4, perCapability: { fixer: 1 } };
 Mock.state.nsVars.default = { SOURCE_ROOT: "/ns", ONLY_NODE: "ns-loses", ONLY_NS: "default" };
 Mock.state.nsVars.prod = { ONLY_NS: "prod" };
 // v4 NodeInstanceRef 只有 {id, task, busy}：槽位身份是 id，busy 缺失 = 未知。
 row.facts.instances = [{ id: "nins_1", task: "terminal-task", busy: true }, { id: "nins_2", task: null }, { id: "nins_3", task: null, busy: false }];
 const def = (await req("/v1/nodes/" + row.name + "?namespace=default")).body;
 assert.deepEqual(def.status.effectiveNamespaces, ["default", "prod"]);
 assert.equal(def.context.namespace, "default", "v4：读上下文在信封顶层，不在 status 里");
 // v4 的 effectiveVars 每项是 {value,source}：来源由中枢逐键解析（local > node > namespace）
 assert.deepEqual(def.status.effectiveVars, {
  SOURCE_ROOT: { value: "/local", source: "local" },
  ONLY_NODE: { value: "node", source: "node" },
  ONLY_NS: { value: "default", source: "namespace" },
 });
 assert.equal(def.status.machineVars.ONLY_NS, undefined); assert.equal((await req("/v1/nodes/" + row.name + "?namespace=prod")).body.status.effectiveVars.ONLY_NS.value, "prod");
 const model = plain(A.model.node(def));
 assert.deepEqual(model.machineLimits, { total: 4, perCapability: { "*": 3, fixer: 1 } });
 assert.equal(model.instances[0].id, "nins_1"); assert.equal(model.instances[0].task, "terminal-task"); assert.equal(model.instances[0].busy, true);
 assert.equal(model.instances[0].occupancy, "occupied");
 assert.equal(model.instances[1].occupancy, "unknown", "缺 busy 是未知，不是空闲");
 assert.equal(model.instances[2].occupancy, "idle");
 // v3 残留字段不读、不映射：旧 slot/taskId/assignmentId 只出现在响应里也不伪造槽位。
 const v3 = plain(A.model.node({ ...def, status: { ...def.status, instances: [{ slot: 0, taskId: "old", assignmentId: "res", busy: false }] } }));
 assert.equal(v3.instances.length, 1);
 assert.deepEqual(Object.keys(v3.instances[0]).sort(), ["busy", "id", "occupancy", "task"]);
 assert.equal(v3.instances[0].id, null); assert.equal(v3.instances[0].task, null); assert.equal(v3.instances[0].occupancy, "idle");
 assert.equal(plain(A.model.node({ ...def, status: { ...def.status, instances: undefined } })).instances, null);
 row.declared = ["*"]; row.spec.namespaces = ["team-a"];
 assert.deepEqual((await req("/v1/nodes/" + row.name)).body.status.effectiveNamespaces, ["team-a"]);
 row.facts.instances = [{ id: "nins_res", task: "terminal-task", busy: true }];
 assert.equal(plain(A.model.node((await req("/v1/nodes/" + row.name)).body)).instances[0].occupancy, "occupied", "terminal task/reservation is not a physical reap");
});

test("F2c2: Node config W5 read/strict PUT CAS400/409 atomic; root shares fact and revisions", async () => {
 const { req, Mock } = loadMock(); const path = "/v1/nodes/anode-1/config";
 const initial = (await req(path)).body;
 assert.deepEqual(Object.keys(initial).sort(), ["appliedHash", "appliedRevision", "configRevision", "limits", "metadata", "namespaces", "vars"]);
 const before = JSON.stringify(Mock.state.nodes[0]);
 assert.equal((await req(path, jsonInit("PUT", { vars: { URL: "draft" } }))).status, 400);
 assert.equal((await req(path, jsonInit("PUT", { metadata: { resourceVersion: "stale" }, vars: { URL: "draft" } }))).status, 409);
 assert.equal(JSON.stringify(Mock.state.nodes[0]), before);
 for (const field of ["spec", "status", "configRevision", "appliedRevision", "appliedHash", "envPassthrough"]) {
  assert.equal((await req(path, jsonInit("PUT", { metadata: initial.metadata, [field]: {} }))).status, 400);
 }
 const saved = (await req(path, jsonInit("PUT", { metadata: initial.metadata, namespaces: ["*"], vars: { URL: "saved" }, limits: { total: 3, perCapability: {} } }))).body;
 assert.notEqual(saved.metadata.resourceVersion, initial.metadata.resourceVersion); assert.notEqual(saved.configRevision, initial.configRevision);
 assert.equal(saved.appliedRevision, initial.appliedRevision);
 const root = (await req("/v1/nodes/anode-1")).body;
 assert.deepEqual(root.spec.vars, saved.vars); assert.equal(root.metadata.resourceVersion, saved.metadata.resourceVersion); assert.equal(root.status.configRevision, saved.configRevision);
 assert.equal((await req("/v1/nodes/anode-1", jsonInit("PUT", c2NodeBody("anode-1", { vars: {} }, initial.metadata.resourceVersion)))).status, 409);
 assert.equal((await req(path, jsonInit("PUT", { metadata: saved.metadata, limits: { total: 0 } }))).status, 400);
 assert.deepEqual((await req(path)).body, saved);
});

test("F2c2: namespace/system current CAS and partial per-node batch no leaks", async () => {
 const { req, Mock } = loadMock();
 for (const [path, field] of [["/v1/namespaces/default/config", "vars"], ["/v1/system/config", "retention"]]) {
  const start = await req(path); assert.equal(start.status, 200); const body = start.body;
  assert.equal((await req(path, jsonInit("PUT", { [field]: {} }))).status, 400);
  assert.equal((await req(path, jsonInit("PUT", { metadata: { resourceVersion: "stale" }, [field]: {} }))).status, 409);
  assert.deepEqual((await req(path)).body, body);
  if (field === "vars") {
   assert.equal((await req(path, jsonInit("PUT", { metadata: body.metadata, vars: {}, limits: { total: 9 } }))).status, 400);
   const save = (await req(path, jsonInit("PUT", { metadata: body.metadata, vars: { NEW: "nonsecret" } }))).body;
   assert.notEqual(save.namespaceVarsRevision, body.namespaceVarsRevision);
  } else {
   assert.deepEqual(body.retention, { resultTtlSec: 604800, eventTtlSec: 2592000, taskTtlSec: 2592000 });
   const save = (await req(path, jsonInit("PUT", { metadata: body.metadata, retention: { ...body.retention, resultTtlSec: 0 } }))).body;
   assert.equal(save.retention.resultTtlSec, 0); assert.notEqual(save.metadata.resourceVersion, body.metadata.resourceVersion);
   assert.equal((await req(path, jsonInit("PUT", { metadata: body.metadata, retention: body.retention }))).status, 409);
  }
 }
 const first = (await req("/v1/nodes/anode-1/config")).body;
 const secondBefore = JSON.stringify(Mock.state.nodes[1]);
 const mixed = await req("/v1/nodes/config/batch", jsonInit("POST", { updates: [
  { name: "anode-1", metadata: first.metadata, vars: { BATCH: "ok" } },
  { name: "anode-2", metadata: { resourceVersion: "stale" }, vars: { BATCH: "bad" } },
  { name: "anode-3", vars: { BATCH: "bad" } }, { name: "ghost", metadata: first.metadata, vars: {} }
 ] }));
 assert.equal(mixed.status, 200); assert.deepEqual(mixed.body.items.map(x => x.ok), [true, false, false, false]);
 assert.deepEqual(mixed.body.items.slice(1).map(x => x.error.code), [409, 400, 404]);
 assert.equal(JSON.stringify(Mock.state.nodes[1]), secondBefore); assert.equal((await req("/v1/nodes/anode-1/config")).body.vars.BATCH, "ok");
 assert.ok(mixed.body.items.slice(1).every(x => !JSON.stringify(x.error).includes("bad")));
});

test("F2c2: confirmed Node actions expose actual offline/unavailable/failed upgrade", async () => {
 const { req, Mock } = loadMock();
 Mock.noAgentBinary(); const unavailable = await req("/v1/nodes/anode-1/upgrade", jsonInit("POST", {}));
 assert.equal(unavailable.status, 501); assert.equal(unavailable.body.reason, "agent_binary_unavailable");
 Mock.noAgentBinary(false); Mock.failUpgrade("checksum_mismatch");
 const failed = await req("/v1/nodes/anode-1/upgrade", jsonInit("POST", {}));
 assert.equal(failed.status, 500); assert.equal(failed.body.reason, "checksum_mismatch");
 const before = JSON.stringify(Mock.state.nodes[0].facts.instances);
 assert.equal((await req("/v1/nodes/anode-1/drain", jsonInit("POST", {}))).status, 200);
 assert.equal(JSON.stringify(Mock.state.nodes[0].facts.instances), before, "drain does not falsely reap occupied slots");
});

test("F2c2: retained login AUTH1 identity.error rejection and zero-device admin facts", async () => {
 const page = readFileSync(path.join(ROOT, "web-src/pages/login.html"), "utf8");
 const fn = page.slice(page.indexOf("async function postJSON"), page.indexOf("return res.json();", page.indexOf("async function postJSON")) + "return res.json();".length) + "\n}";
 const run = vm.runInNewContext("(" + fn + ")", { fetch: async () => ({ ok: false, status: 400, json: async () => ({ error: "Actual retained rejection sentinel" }) }) });
 await rejectsWith(run("/v1/auth/password/login", {}), { message: "Actual retained rejection sentinel", status: 400 });
 const { req, Mock } = loadMock();
 const stats = (await req("/v1/admin/stats")).body;
 assert.equal(stats.deviceCount, Mock.state.devices.length); assert.equal(stats.deliveryTotal, 0); assert.equal(stats.deliveredCount, 0);
 assert.equal((await req("/v1/auth/password")).body.hasPassword, true);
 const invalid = await req("/v1/auth/password", jsonInit("DELETE", { currentPassword: "wrong" })); assert.equal(invalid.status, 400);
 assert.ok(!JSON.stringify(invalid.body).includes("wrong"));
});

test("F2c2: real Node controllers consume apiList.items, bootstrap config, preserve CAS drafts and namespace context", async () => {
 const list = await b2Page('nodes', '?namespace=default', ({ sandbox }) => { sandbox.location.origin = 'https://controller.example'; });
 assert.equal(b2Find(list.get('nodes-list'), n => !!n?.dataset?.node).length, 5, 'actual NodeList GET must render all rows');
 assert.equal(list.get('namespace-save').disabled, false, 'actual NamespaceList items must bootstrap the plain config wrapper');
 list.get('reg-name').value = 'controller-new-node'; await list.get('reg-submit').onclick(); await list.settle();
 assert.ok(list.get('reg-token').textContent); assert.ok(list.get('reg-yaml').textContent.includes('wss://controller.example'));
 list.get('reg-close').onclick(); assert.equal(list.get('reg-token').textContent, '');
 const detail = await b2Page('node', '?id=anode-1&namespace=default', ({ Mock }) => {
  Mock.state.nodes[0].facts.instances = [{ id: 'nins_mock_1', task: 'tsk_mock_4', busy: true }, { id: 'nins_mock_2', task: null, busy: false }];
 });
 assert.equal(detail.get('head-name').textContent, 'anode-1'); assert.equal(detail.get('node-config-save').disabled, false);
 assert.ok(detail.calls.some(c => c.url === '/v1/nodes/anode-1?namespace=default'));
 assert.ok(b2Text(detail.get('node-tasks')).includes('tsk_mock_'), 'actual TaskList items must be consumed');
 const current = (await detail.req('/v1/nodes/anode-1/config')).body;
 await detail.req('/v1/nodes/anode-1/config', jsonInit('PUT', { metadata: current.metadata, vars: { SOURCE_ROOT: '/remote-default' } }));
 const draft = '{\n  "SOURCE_ROOT":"/controller-draft"\n}'; detail.get('node-vars').value = draft;
 await detail.get('node-config-save').onclick(); await detail.settle();
 assert.equal(detail.get('node-vars').value, draft); assert.match(b2Text(detail.get('node-config-state')), /nodes.v4.stale/);
 await detail.get('node-config-reload').onclick(); assert.deepEqual(JSON.parse(detail.get('node-vars').value), { SOURCE_ROOT: '/remote-default' });
 detail.get('node-vars').value = '{"SOURCE_ROOT":"/ui-default"}'; await detail.get('node-config-save').onclick();
 assert.equal(detail.Mock.state.nodes[0].spec.vars.SOURCE_ROOT, '/ui-default');
 detail.get('node-namespace').value = 'prod'; await detail.get('node-namespace').onchange();
 assert.match(b2Text(detail.get('node-context-revision')), /namespace=prod/);
 assert.ok(b2Text(detail.get('node-effective-vars')).includes('hooks.example/notify/prod'));
 assert.equal(detail.Mock.state.nodes[0].facts.instances[0].busy, true, 'terminal Task does not clear occupied slot');
});

/* ================================================================== *
 * R1: independent review repairs (DELIVERY-R1-TASK.md §1–§9)
 * 每个用例都走真实页面控制器 / 真实 mock 路由 / 真实注册的回调，
 * 失败时必须是行为性失败（不是源码字符串或“非空”占位断言）。
 * ================================================================== */

test("R1 namespace deep link: empty storage + ?namespace=prod reads/renders/installs/saves/deletes prod only", async () => {
  const page = await b2Page('playbook', '?namespace=prod&id=project-chain', async run => {
    await pbCreate(run.req, playbookFiles(), 'default');
    await pbCreate(run.req, playbookFiles(), 'prod');
    // 两个 namespace 的同名包共用同一 resourceVersion：CAS 分辨不了上下文，
    // 只有“路径与 metadata.namespace 绑对”才能保证不写错。
    for (const row of run.Mock.state.playbooks) row.rv = 'shared-rv';
  });
  const stored = () => JSON.parse(page.sandbox.localStorage.getItem('anotify.scope') || '{}');
  assert.equal(stored().namespace, 'prod', 'empty storage must adopt the explicit URL namespace');
  assert.match(page.get('pb-slug').textContent, /^prod \/ project-chain/);
  const bookCalls = () => page.calls.filter(c => c.url.includes('/playbooks'));
  assert.ok(bookCalls().length >= 2);
  assert.ok(bookCalls().every(c => c.url.startsWith('/v1/namespaces/prod/playbooks/project-chain')),
    'every read must be prod: ' + JSON.stringify(bookCalls().map(c => c.url)));
  assert.equal(bookCalls().some(c => c.url.includes('/namespaces/default/')), false, 'persisted default must never be addressed');
  assert.ok(bookCalls().some(c => /includeBody=true$/.test(c.url)));

  // render：读 prod 包的 schema 后渲染（不写事实）
  page.get('pb-values-json').value = JSON.stringify({ project: 'a' });
  await page.get('pb-use-json').fire('click');
  await page.get('btn-render').fire('click'); await page.settle();
  assert.equal(page.calls.at(-1).url, '/v1/namespaces/prod/playbooks/project-chain/render');
  assert.match(b2Text(page.get('pb-preview')), /a-review/);

  // install：真实写进 prod；default 那份同名包一条事实都不能多
  await page.get('btn-install').fire('click'); await page.settle();
  const install = page.calls.filter(c => c.url.endsWith('/install')).at(-1);
  assert.equal(install.url, '/v1/namespaces/prod/playbooks/project-chain/install');
  assert.ok(page.Mock.state.capabilities.some(c => c.ns === 'prod' && c.name === 'a-review'), 'install lands in prod');
  assert.equal(page.Mock.state.capabilities.some(c => c.ns === 'default' && c.name === 'a-review'), false, 'default must not be installed');
  assert.ok(page.Mock.state.sources.some(s => s.ns === 'prod' && s.name === 'a-push'));

  // save template：同一包的 CAS 必须用 prod 的 resourceVersion
  await page.get('pb-save-template').fire('click'); await page.settle();
  const save = page.calls.filter(c => c.opts && c.opts.method === 'PUT').at(-1);
  assert.equal(save.url, '/v1/namespaces/prod/playbooks/project-chain');
  const saveBody = JSON.parse(save.opts.body);
  assert.equal(saveBody.metadata.namespace, 'prod');
  assert.equal(saveBody.metadata.resourceVersion, 'shared-rv');
  const defaultRow = page.Mock.state.playbooks.find(p => p.ns === 'default');
  assert.equal(defaultRow.rv, 'shared-rv', 'default package resourceVersion untouched');

  // delete：仍是 prod，并用刷新后的 RV
  await page.get('pb-delete').fire('click'); await page.settle();
  const del = page.calls.filter(c => c.opts && c.opts.method === 'DELETE').at(-1);
  assert.match(del.url, /^\/v1\/namespaces\/prod\/playbooks\/project-chain\?resourceVersion=/);
  assert.equal(page.Mock.state.playbooks.some(p => p.ns === 'prod'), false);
  assert.ok(page.Mock.state.playbooks.some(p => p.ns === 'default'), 'the default package survives');
  assert.equal(stored().namespace, 'prod');
});

test("R1 namespace deep link: playbooks list and capabilities actions bind the URL namespace (empty storage)", async () => {
  const list = await b2Page('playbooks', '?namespace=prod', async run => { await pbCreate(run.req, playbookFiles(), 'prod'); });
  assert.equal(JSON.parse(list.sandbox.localStorage.getItem('anotify.scope')).namespace, 'prod');
  assert.ok(list.fetches.some(c => c.url === '/v1/namespaces/prod/playbooks'));
  assert.equal(list.fetches.some(c => c.url.includes('/namespaces/default/')), false);
  assert.match(b2Text(list.get('pb-scope')), /prod/);
  assert.equal(b2Text(list.get('pb-grid')).includes('project-chain'), true, 'prod card really rendered');
  list.get('pb-input-name').value = 'second-pack';
  list.get('pb-input-files').value = JSON.stringify(playbookFiles({ slug: 'second-pack' }));
  list.get('pb-input-kind').value = 'files';
  await list.get('pb-import-form').fire('submit'); await list.settle();
  const created = list.calls.filter(c => c.opts && c.opts.method === 'POST' && c.url.endsWith('/playbooks')).at(-1);
  assert.equal(created.url, '/v1/namespaces/prod/playbooks');
  assert.equal(JSON.parse(created.opts.body).metadata.namespace, 'prod');
  assert.ok(list.Mock.state.playbooks.some(p => p.ns === 'prod' && p.name === 'second-pack'));

  const caps = await b2Page('capabilities', '?namespace=prod');
  assert.equal(JSON.parse(caps.sandbox.localStorage.getItem('anotify.scope')).namespace, 'prod');
  assert.ok(caps.fetches.some(c => c.url === '/v1/namespaces/prod/capabilities'));
  assert.equal(caps.fetches.some(c => c.url.includes('/namespaces/default/')), false, 'capabilities must not read default');
  const capRow = () => b2Find(caps.get('caps-list'), n => n.getAttribute && n.getAttribute('data-cap') === 'report.send')[0];
  assert.ok(capRow(), 'prod-only capability must be listed');
  await b2Find(capRow(), n => n.tagName === 'BUTTON' && n.textContent === '详情')[0].fire('click'); await caps.settle();
  await b2Find(capRow(), n => n.getAttribute && n.getAttribute('data-act') === 'delete')[0].fire('click'); await caps.settle();
  await caps.get('del-submit').fire('click'); await caps.settle();
  const del = caps.calls.filter(c => c.opts && c.opts.method === 'DELETE').at(-1);
  assert.match(del.url, /^\/v1\/namespaces\/prod\/capabilities\/report\.send\?resourceVersion=/);
  assert.equal(caps.Mock.state.capabilities.some(c => c.ns === 'prod' && c.name === 'report.send'), false);
  assert.ok(caps.Mock.state.capabilities.some(c => c.ns === 'default' && c.name === 'fixer'), 'default resources untouched');
});

test("R1 scope rule: URL namespace wins at entry (mount/ensureScope), in-page switch wins afterwards", () => {
  const entry = loadAnotify({ storage: {} });
  entry.sandbox.URLSearchParams = URLSearchParams;
  entry.sandbox.location.search = '?namespace=prod';
  entry.sandbox.console = { log() {}, error() {}, warn() {} };
  entry.A.mountLayout({ active: 'index' }); // fake document has no #page-main → early return, adoption already done
  assert.equal(JSON.parse(entry.store.get('anotify.scope')).namespace, 'prod', 'mount must adopt before rendering the sidebar');
  // 页内选择器改过的 scope 不能被后续请求/重复 adopt 顶回 URL 旧值
  entry.A.saveScope({ namespace: 'team-a' });
  entry.A.ensureScope();
  entry.A.adoptUrlNamespace();
  assert.equal(JSON.parse(entry.store.get('anotify.scope')).namespace, 'team-a');
});

test("R1 graph clicks: task-name nodes navigate to the canonical Task name; baseGraph capability ids hand back name", async () => {
  const trace = await b2Page('trace', '?namespace=default&traceUID=trace_mock_t1');
  const traceSpec = trace.get('trace-graph')._mountSpec;
  assert.equal(typeof traceSpec.onNodeClick, 'function');
  // 无底图：真实注册的节点是任务名（tsk_mock_2）
  traceSpec.onNodeClick({ id: 'tsk_mock_3' });
  assert.equal(trace.sandbox.location.href, 'message.html?namespace=default&id=tsk_mock_3');

  const index = await b2Page('index', '?namespace=default');
  index.get('flow-host')._mountSpec.onNodeClick({ id: 'tsk_mock_2' });
  assert.equal(index.sandbox.location.href, 'message.html?namespace=default&id=tsk_mock_2');

  // 有底图：节点 id 是能力名；回调必须交出规范化 Task（name），不是不存在的 id
  const { sandbox, req } = loadMock({ graphs: true });
  const G = sandbox.AnotifyGraphs;
  const [srcRes, capRes, edgeRes, traceRes] = await Promise.all([
    req('/v1/namespaces/default/sources'),
    req('/v1/namespaces/default/capabilities'),
    req('/v1/namespaces/default/edges'),
    req('/v1/namespaces/default/traces/trace_mock_t1'),
  ]);
  const src = srcRes.body.items.find(s => s.metadata.name === 'deploy-hook');
  const topology = G.sourceTopology({ sources: [src], source: src, capabilities: capRes.body.items, edges: edgeRes.body.items });
  const host = fakeNode();
  const clicks = [];
  G.taskChain(host, { tasks: traceRes.body.tasks, baseGraph: topology, sourceLabel: 'deploy-hook',
    t: (k, d) => d || k, onNodeClick: task => clicks.push(task) });
  assert.ok(host._mountSpec.nodes.some(n => n.id === 'analyze-pr'));
  host._mountSpec.onNodeClick({ id: 'analyze-pr' });
  assert.equal(clicks.length, 1);
  assert.ok(clicks[0].name && clicks[0].name.startsWith('tsk_mock_'), 'callback carries the canonical task name');
  assert.equal('id' in clicks[0], false, 'no invented id fallback');
  host._mountSpec.onNodeClick({ id: 'ghost-node' });
  assert.equal(clicks.length, 1, 'unknown nodes never fabricate a task');
});

test("R1 cross-page links: Source receipts/recent emit traceUID, Playbook source links emit name; destination really requests", async () => {
  const src = await b2Page('source', '?namespace=default&name=deploy-hook');
  const fireButton = b2Find(src.get('src-actions'), n => n.tagName === 'BUTTON' && n.textContent === 'sources.v4.fire')[0];
  assert.ok(fireButton);
  await fireButton.onclick(); await src.settle();
  await src.get('fire-submit').onclick(); await src.settle();
  const receiptLink = b2Find(src.get('fire-result'), n => n.tagName === 'A')[0];
  assert.ok(receiptLink, 'fire receipt must expose its trace');
  const receiptHref = receiptLink.href;
  assert.match(receiptHref, /^trace\.html\?namespace=default&traceUID=trace_mock_run/);
  assert.equal(receiptHref.includes('traceId='), false, 'the old traceId parameter must be gone');
  const recent = b2Find(src.get('source-events'), n => n.tagName === 'A');
  assert.ok(recent.length);
  assert.ok(recent.every(n => String(n.href).includes('traceUID=')));
  const opened = await b2Page('trace', '?' + receiptHref.split('?')[1]);
  const traceCalls = opened.fetches.filter(c => c.url.includes('/traces/'));
  assert.equal(traceCalls.length, 1, 'destination really issues the Trace request');
  assert.match(traceCalls[0].url, /^\/v1\/namespaces\/default\/traces\/trace_mock_run/);
  assert.equal(b2Text(opened.get('trace-error')).includes('trace.v4.missing'), false);

  const page = await b2Page('playbook', '?namespace=default&id=project-chain', async run => {
    await pbCreate(run.req);
    await pbAction(run.req, 'install');
  });
  const sourceLinks = b2Find(page.get('pb-items'), n => n.tagName === 'A' && String(n.href).startsWith('source.html?'));
  assert.ok(sourceLinks.length, 'installed source must be linked');
  const href = sourceLinks[0].href;
  assert.match(href, /[?&]name=a-push/);
  assert.equal(href.includes('&id='), false, 'Source detail reads name, not id');
  const detail = await b2Page('source', '?' + href.split('?')[1]);
  assert.ok(detail.fetches.some(c => c.url === '/v1/namespaces/default/sources/a-push'), 'destination really reads the source');
  assert.equal(b2Text(detail.get('src-body')).includes('sources.v4.no_name'), false);
});

test("R1 mock versions: archived content immutable (PATCH 400 immutable_field, POST 同版本 400 version_content_immutable) and latest=max", async () => {
  const { req, Mock } = loadMock();
  const base = '/v1/namespaces/default/capabilities';
  const one = base + '/r1-versioned';
  const body = (version, script) => ({ apiVersion: 'anotify/v1', kind: 'Capability',
    metadata: Object.assign({ name: 'r1-versioned' }, version == null ? {} : { version }),
    spec: { next: [], run: { entrypoint: 'run.sh', files: { 'run.sh': script } } } });
  const created = await req(base, jsonInit('POST', body(1, '#!/bin/sh\necho v1\n')));
  assert.equal(created.status, 201, JSON.stringify(created.body));
  const v1Before = plain((await req(one + '/versions/1?includeBody=true')).body);
  const patch = (spec, rv, version) => req(one, jsonInit('PATCH', Object.assign(
    { apiVersion: 'anotify/v1', kind: 'Capability', metadata: Object.assign({ name: 'r1-versioned', resourceVersion: rv }, version == null ? {} : { version }), spec },
  )));
  const row = () => Mock.state.capabilities.find(c => c.ns === 'default' && c.name === 'r1-versioned');

  // 改正文的路只有 POST（内容寻址）：PATCH 带 run 直接 400 immutable_field，
  // 且 whole-state 一个字节都不动。
  const wholeBeforePatch = allFacts(Mock);
  const rejectedPatch = await patch({ next: [], run: { entrypoint: 'run.sh', files: { 'run.sh': '#!/bin/sh\necho v2\n' } } }, created.body.metadata.resourceVersion);
  assert.equal(rejectedPatch.status, 400);
  assert.equal(rejectedPatch.body.reason, 'immutable_field');
  assert.equal(allFacts(Mock), wholeBeforePatch, 'immutable_field rejection must preserve the complete Mock.state');

  const v2 = await req(base, jsonInit('POST', body(undefined, '#!/bin/sh\necho v2\n')));
  assert.equal(v2.status, 201, JSON.stringify(v2.body));
  assert.equal(v2.body.metadata.version, 2);
  const before = JSON.stringify(row());
  const wholeBefore = allFacts(Mock);

  // 归档版本 v1：显式同版本 + 异正文 → POST 400 version_content_immutable，whole-state 不变
  const overwrite = await req(base, jsonInit('POST', body(1, '#!/bin/sh\necho overwritten\n')));
  assert.equal(overwrite.status, 400);
  assert.equal(overwrite.body.reason, 'version_content_immutable');
  assert.equal(JSON.stringify(row()), before, 'rejection must not mutate any fact');
  assert.equal(allFacts(Mock), wholeBefore, 'rejection must preserve the complete Mock.state');
  const v1After = plain((await req(one + '/versions/1?includeBody=true')).body);
  assert.deepEqual(v1After.spec, v1Before.spec, 'archived spec stays byte-identical after a rejected overwrite');
  assert.equal(v1After.status.digest, v1Before.status.digest, 'archived digest stays stable');

  // 当前版本必须仍是 max(metadata.version)，历史里没有重复版本号
  const versions = (await req(one + '/versions')).body.items;
  assert.deepEqual(versions.map(v => v.metadata.version), [2, 1]);
  assert.equal(Math.max(...versions.map(v => v.metadata.version)), (await req(one)).body.metadata.version);

  // 显式旧版本 + 同内容：内容寻址复用（不新开版本、不降当前版本）
  const noop = await req(base, jsonInit('POST', body(1, '#!/bin/sh\necho v1\n')));
  assert.equal(noop.status, 201);
  assert.equal(noop.body.metadata.version, 1, '同内容复用那一行');
  assert.equal((await req(one)).body.metadata.version, 2, '当前版本仍是 max');
  assert.deepEqual((await req(one + '/versions')).body.items.map(v => v.metadata.version), [2, 1]);

  // 显式更大的版本号：插入该版本，当前 = max
  assert.equal((await req(base, jsonInit('POST', body(5, '#!/bin/sh\necho v5\n')))).status, 201);
  assert.equal((await req(one)).body.metadata.version, 5);
  // 比历史最大值小的“空洞”显式版本也允许落库（真服务不禁止），但当前版本不会因此降低
  const holeBefore = JSON.stringify(row());
  const hole = await req(base, jsonInit('POST', body(3, '#!/bin/sh\necho v3\n')));
  assert.equal(hole.status, 201, JSON.stringify(hole.body));
  assert.equal(hole.body.metadata.version, 3);
  assert.notEqual(JSON.stringify(row()), holeBefore, '显式版本 3 真的落库');
  assert.equal((await req(one)).body.metadata.version, 5, '当前版本 = max(metadata.version)，不被空洞版本拉低');
  assert.deepEqual((await req(one + '/versions')).body.items.map(v => v.metadata.version), [5, 3, 2, 1]);
});

test("R1 writeOnly masking: integer/boolean/object/array leaves stay secret in receipt/history/Status; render+digest keep raw", async () => {
  const files = playbookFiles({ edit: d => {
    d['playbook.yaml'].spec.values = { type: 'object', additionalProperties: false, required: ['project'], properties: {
      project: { type: 'string', pattern: '^[a-z][a-z0-9-]*$' },
      env: { type: 'string', default: 'prod' },
      credential: { type: 'string', writeOnly: true },
      accessCode: { type: 'integer', writeOnly: true },
      flagOn: { type: 'boolean', writeOnly: true },
      blobData: { type: 'object', writeOnly: true },
      listData: { type: 'array', writeOnly: true },
      nested: { type: 'array', items: { type: 'object', properties: { code: { type: 'string', writeOnly: true } } } },
    } };
    d['sources/on-push.yaml'].spec.config.secret = '{{values.credential}}';
  } });
  const { req, Mock } = loadMock();
  await pbCreate(req, files);
  const values = { project: 'a', credential: 'cred-string-sentinel', accessCode: 987654321, flagOn: true,
    blobData: { inner: 'blob-secret-sentinel' }, listData: ['list-secret-sentinel'], nested: [{ code: 'nested-secret-sentinel' }] };
  const sentinels = ['cred-string-sentinel', '987654321', 'blob-secret-sentinel', 'list-secret-sentinel', 'nested-secret-sentinel'];
  const renderRes = await pbAction(req, 'render', values);
  assert.equal(renderRes.status, 200, JSON.stringify(renderRes.body));
  const render = renderRes.body;
  assert.equal(render.values.accessCode, 987654321, 'raw values only live in render');
  assert.equal(render.values.flagOn, true);

  const receiptRes = await pbAction(req, 'install', values);
  assert.equal(receiptRes.status, 200, JSON.stringify(receiptRes.body));
  const receipt = receiptRes.body;
  assert.equal(receipt.values.project, 'a');
  assert.equal(receipt.values.credential, '***');
  assert.equal(receipt.values.accessCode, '***', 'numeric writeOnly must be schema-masked');
  assert.equal(receipt.values.flagOn, '***', 'boolean writeOnly must be schema-masked');
  assert.equal(receipt.values.blobData, '***', 'object writeOnly must be masked');
  assert.equal(receipt.values.listData, '***', 'array writeOnly must be masked');
  assert.deepEqual(receipt.values.nested, [{ code: '***' }], 'nested array leaves stay masked');
  for (const secret of sentinels) assert.equal(JSON.stringify(receipt).includes(secret), false, 'receipt leaks ' + secret);
  assert.equal(receipt.valuesDigest, render.valuesDigest, 'raw values still define the digest');
  assert.equal(Mock.state.playbookInstalls[0].valuesDigest, receipt.valuesDigest);

  // 安装历史（groups / single receipt）同样脱敏；持久化的应用值仍是原文
  const groups = (await req('/v1/namespaces/default/playbooks/project-chain/installs')).body;
  for (const secret of sentinels) assert.equal(JSON.stringify(groups).includes(secret), false, 'install history leaks ' + secret);
  assert.equal(groups.installs[0].values.accessCode, '***');
  assert.equal((await req('/v1/namespaces/default/playbooks/project-chain/installs/' + receipt.install.id)).body.values.blobData, '***');
  assert.equal(Mock.state.sources.find(s => s.name === 'a-push').config.secret, 'cred-string-sentinel', 'app config keeps the real credential');
  const source = (await req('/v1/namespaces/default/sources/a-push')).body;
  assert.equal(source.spec.config.secret, undefined); assert.equal(source.spec.config.secretSet, true);

  // 冲突 Status.details 只能给摘要，不能回显 writeOnly 原文
  const other = Object.assign({}, values, { credential: 'second-cred-sentinel' });
  const err = await pbAction(req, 'install', other);
  assert.equal(err.status, 400); assert.equal(err.body.reason, 'playbook_conflict');
  assert.ok(JSON.stringify(err.body).includes('config.secret'), 'conflict still names the differing field');
  for (const secret of sentinels.concat(['second-cred-sentinel'])) {
    assert.equal(JSON.stringify(err.body).includes(secret), false, 'Status.details leaks ' + secret);
  }
});

test("R1 nodes namespace config: captured ns+generation discards reversed stale success/error; save binds loaded ns/RV; refresh failure stays visible", async () => {
  const run = await b2Page('nodes', '?namespace=default', ({ sandbox, Mock }) => {
    Mock.state.nsConfig.default.rv = 'shared-config-rv';
    Mock.state.nsConfig.prod.rv = 'shared-config-rv';
    const original = sandbox.fetch;
    const holds = [];
    let failDefaultRead = false;
    sandbox.fetch = (url, opts = {}) => {
      const u = String(url);
      if (u === '/v1/namespaces/prod/config' && !opts.method) return new Promise(resolve => holds.push(resolve));
      if (failDefaultRead && u === '/v1/namespaces/default/config' && !opts.method) {
        failDefaultRead = false;
        return original('/v1/namespaces/default/missing-endpoint'); // 真实 501 Status：读失败
      }
      return original(url, opts);
    };
    sandbox.r1 = { holds, original, failDefaultRead: () => { failDefaultRead = true; } };
  });
  const vars = () => JSON.parse(run.get('namespace-vars').value);
  assert.deepEqual(vars(), plain(run.Mock.state.nsVars.default));
  const pick = run.get('config-namespace');

  // A(prod) 挂起 → B(default) 先回 → A 后回（反序）：A 的响应属于过期请求
  pick.value = 'prod'; await pick.onchange(); await run.settle();
  pick.value = 'default'; await pick.onchange(); await run.settle();
  const prodResponse = await run.sandbox.r1.original('/v1/namespaces/prod/config');
  run.sandbox.r1.holds.shift()(prodResponse); await run.settle();
  assert.deepEqual(vars(), plain(run.Mock.state.nsVars.default), 'late prod success must not paint the default editor');
  assert.match(run.get('namespace-revisions').textContent, /resourceVersion=shared-config-rv/);

  // 保存：路径/CAS 都绑“已加载的” namespace，而不是全局 scope/当前选择
  run.get('namespace-vars').value = JSON.stringify({ ONLY_DEFAULT: 'saved-in-default' });
  await run.get('namespace-save').onclick(); await run.settle();
  const put = run.calls.filter(c => c.opts && c.opts.method === 'PUT').at(-1);
  assert.equal(put.url, '/v1/namespaces/default/config');
  const putBody = JSON.parse(put.opts.body);
  assert.equal(putBody.metadata.resourceVersion, 'shared-config-rv', 'save CASes the loaded namespace RV');
  assert.deepEqual(putBody.vars, { ONLY_DEFAULT: 'saved-in-default' });
  assert.deepEqual(plain(run.Mock.state.nsVars.default), { ONLY_DEFAULT: 'saved-in-default' });
  assert.deepEqual(plain(run.Mock.state.nsVars.prod), { NOTIFY_URL: 'https://hooks.example/notify/prod', SLACK_CHANNEL: '#prod' },
    'prod config must never receive the default draft');

  // 反序的“过期错误”同样不许落进界面
  pick.value = 'prod'; await pick.onchange(); await run.settle();
  pick.value = 'default'; await pick.onchange(); await run.settle();
  run.sandbox.r1.holds.shift()(await run.sandbox.r1.original('/v1/namespaces/default/missing-endpoint')); await run.settle();
  assert.equal(b2Text(run.get('namespace-state')).includes('nodes.v4.error'), false, 'stale error must not paint');
  assert.deepEqual(vars(), { ONLY_DEFAULT: 'saved-in-default' });

  // 保存成功但刷新读失败：必须保留“已保存 + 刷新失败”，不能只报 saved
  run.sandbox.r1.failDefaultRead();
  await run.get('namespace-save').onclick(); await run.settle();
  const state = b2Text(run.get('namespace-state'));
  assert.match(state, /nodes\.v4\.saved/, 'the save itself is still reported');
  assert.match(state, /nodes\.v4\.error/, 'the failed refresh stays visible');
  assert.equal(run.calls.some(c => c.opts && c.opts.method === 'PUT' && c.url === '/v1/namespaces/prod/config'), false, 'no namespace write ever crosses scopes');
});

test("R1 sources template copy: copyText(button, () => token) really writes the expression", async () => {
  const run = await b2Page('sources', '?namespace=default', ({ sandbox }) => {
    sandbox.r1clip = [];
    sandbox.navigator.clipboard.writeText = async text => { sandbox.r1clip.push(text); };
  });
  await run.get('btn-new').onclick(); await run.settle();
  run.get('f-sample').value = JSON.stringify({ repo: 'acme/api' });
  run.get('f-kind').value = 'webhook';
  await run.get('f-kind').onchange(); await run.settle();
  const tokens = b2Find(run.get('avail-vars-body'), n => n.tagName === 'BUTTON');
  const token = tokens.find(n => n.textContent === '$(payload.repo)');
  assert.ok(token, 'suggestion buttons must be rendered');
  await token.onclick();
  assert.deepEqual(run.sandbox.r1clip, ['$(payload.repo)'], 'the clipboard receives the variable expression, not null');
  assert.notEqual(token.textContent, '$(payload.repo)', 'button shows the copied confirmation');
});

test("R1 A1 artifacts: valid logical path + malformed hash stays display-only (no link, no artifact request)", async () => {
  const bad = ['A'.repeat(64), 'a'.repeat(63), 'a'.repeat(65), 'a'.repeat(62) + '/x', 'g'.repeat(64), '../../etc/passwd'];
  for (const hash of bad) {
    const run = await b2Page('message', '?namespace=default&id=tsk_mock_2', ({ Mock }) => {
      Mock.state.tasks.find(t => t.name === 'tsk_mock_2').result.artifacts = [{ path: 'reports/ok.txt', sha256: hash }];
    });
    assert.equal(b2Find(run.get('task-result'), n => n.tagName === 'A').length, 0, 'malformed hash must not produce a link: ' + hash);
    assert.match(b2Text(run.get('task-result')), /message\.v4\.artifact_invalid/);
    assert.equal(run.calls.some(c => c.url.includes('/artifacts/')), false, 'malformed hash must not issue a request: ' + hash);
    assert.equal(run.fetches.some(c => c.url.includes('/artifacts/')), false, 'direct fetch must also reject malformed hash: ' + hash);
  }
  const ok = await b2Page('message', '?namespace=default&id=tsk_mock_2', ({ Mock }) => {
    Mock.state.tasks.find(t => t.name === 'tsk_mock_2').result.artifacts = [{ path: 'reports/ok.txt', sha256: 'b'.repeat(64) }];
  });
  const link = b2Find(ok.get('task-result'), n => n.tagName === 'A')[0];
  assert.ok(link, 'a valid logical path + lowercase 64-hex hash still yields a link');
  assert.equal(link.getAttribute('href'), '/v1/namespaces/default/tasks/tsk_mock_2/artifacts/' + 'b'.repeat(64));
});

test("R1 A2 capability versions: selected version issues its own includeBody request and renders that exact script", async () => {
  const run = await b2Page('capabilities', '?namespace=default', ({ Mock }) => {
    const cap = Mock.state.capabilities.find(c => c.ns === 'default' && c.name === 'analyze-pr' && c.version === 2);
    const historical = plain(cap.spec);
    historical.run.files['main.sh'] = '#!/bin/sh\necho r1-selected-v2-sentinel\n';
    cap.versions.unshift({ version: 2, spec: historical, digest: cap.digest, at: cap.versionAt });
    cap.version = 3;
    cap.spec.run.files['main.sh'] = '#!/bin/sh\necho r1-current-v3-only\n';
  });
  const row = () => b2Find(run.get('caps-list'), n => n.getAttribute && n.getAttribute('data-cap') === 'analyze-pr')[0];
  await b2Find(row(), n => n.tagName === 'BUTTON' && n.textContent === '详情')[0].fire('click'); await run.settle();
  await b2Find(row(), n => n.getAttribute && n.getAttribute('data-act') === 'versions')[0].fire('click'); await run.settle();
  const v2 = b2Find(row(), n => String(n.className).includes('cap-ver-row') && String(b2Text(n)).trim().startsWith('v2'))[0];
  assert.ok(v2, 'historical version v2 row must be rendered');
  await b2Find(v2, n => n.tagName === 'BUTTON')[0].fire('click'); await run.settle();
  const requested = run.calls.filter(c => c.url.includes('/versions/')).at(-1);
  assert.equal(requested.url, '/v1/namespaces/default/capabilities/analyze-pr/versions/2?includeBody=true');
  const body = b2Find(row(), n => String(n.className).split(' ').includes('cap-ver-body'))[0];
  assert.ok(body, 'selected historical body has its own container');
  const script = b2Find(body, n => String(n.className).split(' ').includes('cap-script-pre'))[0];
  assert.equal(b2Text(script).trim(), '#!/bin/sh\necho r1-selected-v2-sentinel', 'isolated historical script equals the full requested body');
  const shown = b2Text(body);
  assert.equal(shown.includes('r1-current-v3-only'), false);
  assert.equal(shown.includes('echo v1'), false, 'v1 body must not be displayed for a v2 request');
  assert.equal(shown.includes('caps.versions.body_empty'), false, 'no empty-body placeholder');
});


// R1b: genuine registered regressions for deferred writes and exact display provenance.
for (const [journey, outcome] of [['ABA', 'success'], ['ABA', '409'], ['ABA', 'error'], ['sameNS', 'success'], ['sameNS', '409']]) {
  test(`R1b nodes deferred actual PUT: ${journey} ${outcome} preserves newer draft/CAS/state/finally`, async () => {
    const run = await b2Page('nodes', '?namespace=default', ({ sandbox, Mock }) => {
      const original = sandbox.fetch;
      const held = [];
      sandbox.fetch = async (url, opts = {}) => {
        if (String(url) === '/v1/namespaces/default/config' && opts.method === 'PUT') {
          if (outcome === '409') Mock.state.nsConfig.default.rv = 'concurrent-rv';
          const response = await original(url, opts); // execute the real PUT before deferring its response
          return new Promise(resolve => held.push({ resolve, response }));
        }
        return original(url, opts);
      };
      sandbox.r1bHeld = held;
    });
    const edit = async text => { run.get('namespace-vars').value = text; await run.get('namespace-vars').fire('input'); };
    const loadedRV = /resourceVersion=([^ ]+)/.exec(run.get('namespace-revisions').textContent)[1];
    await edit('{"OLD_SAVE":"old-submission"}');
    const saving = run.get('namespace-save').onclick(); await run.settle();
    assert.equal(run.sandbox.r1bHeld.length, 1, 'actual PUT is now held');
    if (journey === 'ABA') {
      run.get('config-namespace').value = 'prod'; await run.get('config-namespace').onchange(); await run.settle();
      run.get('config-namespace').value = 'default'; await run.get('config-namespace').onchange(); await run.settle();
    }
    await edit('{"NEW_DRAFT":"must-survive-old-response"}');
    const before = { draft: run.get('namespace-vars').value, revisions: run.get('namespace-revisions').textContent,
      state: b2Text(run.get('namespace-state')), disabled: run.get('namespace-save').disabled, calls: run.calls.length };
    const held = run.sandbox.r1bHeld.shift();
    held.resolve(outcome === 'error' ? jsonResponse({ kind: 'Status', reason: 'DeferredFailure', message: 'old-response-error' }, 501) : held.response);
    await saving; await run.settle();
    assert.equal(run.get('namespace-vars').value, before.draft);
    assert.equal(run.get('namespace-revisions').textContent, before.revisions, 'new editor CAS must not be replaced');
    assert.equal(b2Text(run.get('namespace-state')), before.state, 'old completion must not claim the new draft saved/error');
    assert.equal(run.get('namespace-save').disabled, before.disabled, 'stale finally must not change the new editor button');
    assert.equal(run.calls.length, before.calls, 'stale completion cannot refresh or auto-save');
    const puts = run.calls.filter(c => c.opts?.method === 'PUT');
    assert.equal(puts.length, 1);
    assert.equal(puts[0].url, '/v1/namespaces/default/config');
    assert.equal(JSON.parse(puts[0].opts.body).metadata.resourceVersion, loadedRV);
    assert.deepEqual(JSON.parse(puts[0].opts.body).vars, { OLD_SAVE: 'old-submission' });
    if (outcome !== '409') assert.deepEqual(plain(run.Mock.state.nsVars.default), { OLD_SAVE: 'old-submission' });
    assert.equal('NEW_DRAFT' in run.Mock.state.nsVars.default, false);
  });
}
for (const outcome of ['success', 'error']) {
  test(`R1b nodes duplicate sameNS GET reverse ${outcome} detects load generation`, async () => {
    const run = await b2Page('nodes', '?namespace=default');
    const original = run.sandbox.fetch;
    const held = [];
    run.sandbox.fetch = (url, opts = {}) => String(url) === '/v1/namespaces/default/config' && !opts.method
      ? new Promise(resolve => held.push(resolve)) : original(url, opts);
    run.get('namespace-reload').onclick(); run.get('namespace-reload').onclick(); await run.settle();
    assert.equal(held.length, 2);
    held[1](jsonResponse({ metadata: { resourceVersion: 'new-rv' }, vars: { GENERATION: 'new' }, namespaceVarsRevision: 20 }));
    await run.settle();
    const before = [run.get('namespace-vars').value, run.get('namespace-revisions').textContent, b2Text(run.get('namespace-state'))];
    held[0](outcome === 'error' ? jsonResponse({ kind: 'Status', reason: 'OldFailure', message: 'old-get-error' }, 409)
      : jsonResponse({ metadata: { resourceVersion: 'old-rv' }, vars: { GENERATION: 'old' }, namespaceVarsRevision: 10 }));
    await run.settle();
    assert.deepEqual([run.get('namespace-vars').value, run.get('namespace-revisions').textContent, b2Text(run.get('namespace-state'))], before);
  });
}
function r1bSecretFiles() {
  return playbookFiles({ edit: d => {
    Object.assign(d['playbook.yaml'].spec.values.properties, {
      pin: { type: 'integer', writeOnly: true }, flag: { type: 'boolean', writeOnly: true },
      blob: { type: 'object', writeOnly: true }, list: { type: 'array', writeOnly: true },
      route: { type: 'string', writeOnly: true },
      nested: { type: 'object', properties: { visible: { type: 'integer' }, hidden: { type: 'integer', writeOnly: true } } },
    });
    const spec = d['sources/on-push.yaml'].spec;
    spec.targets[0].params = { pin: '{{values.pin}}', flag: '{{values.flag}}', blob: '{{values.blob}}', list: '{{values.list}}',
      mixed: 'pin={{values.pin}};flag={{values.flag}};blob={{values.blob}}', nested: '{{values.nested}}',
      descendant: '{{values.blob.inner}}', fixedNumber: 123456789, fixedBoolean: true, fixedText: 'same-scalar-facts' };
    d['capabilities/review.yaml'].spec.next = ['{{values.route}}'];
  } });
}
test('R1b writeOnly provenance: two installs mask declared/actual/mixed/notes/history/Status and keep unrelated scalar/render/digest/actions', async () => {
  const { req, Mock } = loadMock();
  const files = r1bSecretFiles(); await pbCreate(req, files);
  const old = { project: 'a', pin: 123456789, flag: true, blob: { inner: 'old-object-secret' }, list: ['old-array-secret'],
    route: 'old-secret-route', nested: { visible: 123456789, hidden: 135791357 } };
  const fresh = { ...old, pin: 987654321, flag: false, blob: { inner: 'new-object-secret' }, list: ['new-array-secret'],
    route: 'new-secret-route', nested: { visible: 123456789, hidden: 975319753 } };
  const renderOld = await pbAction(req, 'render', old);
  assert.equal(renderOld.status, 200, JSON.stringify(renderOld.body));
  const first = await pbAction(req, 'install', old); assert.equal(first.status, 200, JSON.stringify(first.body));
  const appBefore = JSON.stringify(Mock.state.sources.find(s => s.name === 'a-push'));
  const renderFresh = await pbAction(req, 'render', fresh);
  const second = await pbAction(req, 'install', fresh); assert.equal(second.status, 200, JSON.stringify(second.body));
  const u = second.body.unapplied.find(u => u.field === 'targets'); assert.ok(u, 'unapplied facts are retained');
  for (const branch of ['declared', 'actual']) {
    const p = u[branch][0].params;
    for (const key of ['pin', 'flag', 'blob', 'list', 'mixed', 'descendant']) assert.equal(p[key], '***', `${branch}.${key}`);
    assert.deepEqual(p.nested, { visible: 123456789, hidden: '***' });
    assert.equal(p.fixedNumber, 123456789, 'nonsecret equal numeric scalar stays intact');
    assert.equal(p.fixedBoolean, true, 'nonsecret equal boolean scalar stays intact');
    assert.equal(p.fixedText, 'same-scalar-facts');
    assert.equal(u[branch][0].capability, 'a-review', 'nonsecret navigation/action identity remains');
  }
  assert.equal(JSON.stringify(Mock.state.sources.find(s => s.name === 'a-push')), appBefore, 'unapplied did not overwrite actual resources');
  assert.equal(renderOld.body.sources[0].spec.targets[0].params.pin, old.pin);
  assert.equal(renderFresh.body.sources[0].spec.targets[0].params.pin, fresh.pin);
  assert.equal(first.body.valuesDigest, renderOld.body.valuesDigest);
  assert.equal(second.body.valuesDigest, renderFresh.body.valuesDigest);
  const base = '/v1/namespaces/default/playbooks/project-chain';
  const copies = [first.body, second.body, (await req(base + '/installs/' + second.body.install.id)).body,
    (await req(base + '/installs')).body, (await req(base)).body.status, ...Mock.state.playbookInstalls];
  for (const copy of copies) for (const secret of ['old-object-secret', 'new-object-secret', 'old-array-secret', 'new-array-secret', 'old-secret-route', 'new-secret-route', '987654321', '135791357', '975319753']) {
    assert.equal(JSON.stringify(copy).includes(secret), false, 'display/persisted copy leaks ' + secret);
  }
  assert.ok(second.body.notes.some(n => n.code === 'targets_unapplied'));
  const beforeConflict = allFacts(Mock);
  const conflictFiles = pbRawEdit(files, d => { d['sources/on-push.yaml'].spec.config.secret = '{{values.credential}}'; });
  const current = (await req(base)).body;
  assert.equal((await pbReplace(req, current, conflictFiles)).status, 200);
  const facts = allFacts(Mock);
  const conflict = await pbAction(req, 'install', { ...fresh, credential: 'conflict-string-secret' });
  assert.equal(conflict.status, 400); assert.equal(conflict.body.reason, 'playbook_conflict');
  assert.equal(allFacts(Mock), facts);
  assert.equal(JSON.stringify(conflict.body).includes('conflict-string-secret'), false);
  assert.ok(conflict.body.details.conflicts[0].fields.includes('config.secret'));
  assert.notEqual(facts, beforeConflict, 'only the explicit template edit changed state');
});
test('R1b graph capability-ID collision selects only the appropriate exact Task index', async () => {
  const run = loadMock({ graphs: true });
  const seed = (await run.req('/v1/namespaces/default/tasks/tsk_mock_2')).body;
  const task = (name, cap) => { const r = plain(seed); r.metadata.name = name; r.spec.capability.name = cap; return r; };
  const tasks = [task('tsk_collision', 'other'), task('tsk_target', 'tsk_collision')];
  const host = fakeNode(); const clicked = [];
  const options = { tasks, onNodeClick: t => clicked.push(t.name), animate: false };
  run.sandbox.AnotifyGraphs.taskChain(host, { ...options, baseGraph: { nodes: [{ id: 'tsk_collision', label: 'tsk_collision' }], edges: [] } });
  assert.ok(host._mountSpec.nodes.some(n => n.id === 'tsk_collision'));
  host._mountSpec.onNodeClick({ id: 'tsk_collision' });
  assert.deepEqual(clicked, ['tsk_target']);
  run.sandbox.AnotifyGraphs.taskChain(host, options);
  host._mountSpec.onNodeClick({ id: 'tsk_collision' });
  assert.deepEqual(clicked, ['tsk_target', 'tsk_collision']);
});
test('R1b persisted default before partials: URLprod read/render/install/save/delete preserves complete default scope; real switch updates URL/reloads', async () => {
  let defaultBefore, setupFetches;
  const defaultFacts = Mock => JSON.stringify(Object.fromEntries(Object.entries(Mock.state).map(([k, v]) => [k,
    Array.isArray(v) ? v.filter(x => x.ns === 'default') : v && typeof v === 'object' && 'default' in v ? v.default : v])));
  const page = await b2Page('playbook', '?namespace=prod&id=project-chain', async run => {
    assert.equal(run.A.readScope().namespace, 'default', 'persisted scope was read by partials before page init');
    await pbCreate(run.req, playbookFiles(), 'default'); await pbCreate(run.req, playbookFiles(), 'prod');
    for (const row of run.Mock.state.playbooks) row.rv = 'same-rv';
    defaultBefore = defaultFacts(run.Mock);
    setupFetches = 2;
  }, { storage: { 'anotify.scope': JSON.stringify({ namespace: 'default' }) } });
  assert.equal(page.A.readScope().namespace, 'prod');
  page.get('pb-values-json').value = '{"project":"a"}'; await page.get('pb-use-json').fire('click');
  await page.get('btn-render').fire('click'); await page.settle();
  await page.get('btn-install').fire('click'); await page.settle();
  await page.get('pb-save-template').fire('click'); await page.settle();
  await page.get('pb-delete').fire('click'); await page.settle();
  assert.equal(defaultFacts(page.Mock), defaultBefore);
  const requests = page.fetches.slice(setupFetches).filter(c => c.url.includes('/playbooks'));
  assert.ok(requests.some(c => c.opts.method === 'PUT' && JSON.parse(c.opts.body).metadata.resourceVersion === 'same-rv'));
  assert.ok(requests.some(c => c.opts.method === 'DELETE'));
  assert.ok(requests.every(c => c.url.startsWith('/v1/namespaces/prod/')));
  page.sandbox.location.href = 'http://localhost/playbook.html?namespace=prod&id=project-chain#history';
  const replacements = [];
  page.sandbox.history = { replaceState: (_, __, url) => { replacements.push(url); page.sandbox.location.search = new URL(url, 'http://localhost').search; } };
  page.A.switchScope('default');
  assert.deepEqual(replacements, ['/playbook.html?namespace=default&id=project-chain#history']);
  assert.equal(page.sandbox.location.reloaded, true);
  assert.equal(page.A.readScope().namespace, 'default');
});
test('R1b prod graph click opens message controller with exact Task GET and displayed identity; baseGraph exact mapping', async () => {
  const run = await b2Page('trace', '?namespace=prod&traceUID=trace_mock_t1', ({ Mock }) => {
    const ts = Mock.state.tasks.filter(t => t.traceUID === 'trace_mock_t1'); for (const t of ts) t.ns = 'prod';
    for (const e of Mock.state.events.filter(e => e.traceUID === 'trace_mock_t1')) e.ns = 'prod';
  });
  const spec = run.get('trace-graph')._mountSpec; assert.ok(spec?.nodes.length);
  const chosen = spec.nodes.find(n => n.id.startsWith('tsk_mock_'));
  assert.ok(chosen); spec.onNodeClick(chosen);
  const href = run.sandbox.location.href;
  const target = await b2Page('message', '?' + href.split('?')[1], ({ Mock }) => {
    const row = Mock.state.tasks.find(t => t.name === chosen.id); row.ns = 'prod';
  });
  assert.ok(target.fetches.some(c => c.url === '/v1/namespaces/prod/tasks/' + chosen.id));
  assert.equal(b2Text(target.get('msg-sub')).trim(), 'prod · ' + chosen.id);
  const seed = (await target.req('/v1/namespaces/prod/tasks/' + chosen.id)).body;
  const host = fakeNode(); let task;
  target.sandbox.AnotifyGraphs.taskChain(host, { tasks: [seed], baseGraph: { nodes: [{ id: seed.spec.capability.name, label: 'exact capability' }], edges: [] }, onNodeClick: t => { task = t; } });
  host._mountSpec.onNodeClick({ id: seed.spec.capability.name });
  assert.equal(task.name, seed.metadata.name); assert.equal(task.capability.name, seed.spec.capability.name);
});
test('R1b sources LIST real fire receipt opens Trace exact returned ID in same namespace', async () => {
  const list = await b2Page('sources', '?namespace=prod');
  const fire = b2Find(list.get('src-list'), n => n.tagName === 'BUTTON' && n.textContent === 'sources.v4.fire')[0];
  assert.ok(fire); await fire.onclick(); await list.settle();
  await list.get('fire-submit').onclick(); await list.settle();
  const request = list.calls.findLast(c => c.url.endsWith('/fire')); assert.ok(request);
  const traceID = b2Find(list.get('fire-result'), n => n.tagName === 'A')[0].textContent;
  const emitted = list.Mock.state.events.findLast(e => e.traceUID === traceID && e.ns === 'prod');
  assert.ok(emitted, 'receipt links the exact Trace emitted by the actual fire request');
  const link = b2Find(list.get('fire-result'), n => n.tagName === 'A')[0]; assert.ok(link);
  const query = new URL(link.href, 'http://localhost').searchParams;
  assert.equal(query.get('namespace'), 'prod'); assert.equal(query.get('traceUID'), traceID);
  const target = await b2Page('trace', '?' + link.href.split('?')[1], ({ Mock }) => {
    Mock.state.events.push(...plain(list.Mock.state.events.filter(e => e.traceUID === traceID)));
    Mock.state.tasks.push(...plain(list.Mock.state.tasks.filter(t => t.traceUID === traceID)));
  });
  assert.ok(target.fetches.some(c => c.url === '/v1/namespaces/prod/traces/' + traceID));
});

for (const stage of ['sameNS edit ABA', 'refresh GET']) {
  test(`R1b nodes edit epoch during ${stage} isolates old submission even with equal draft text`, async () => {
    const run = await b2Page('nodes', '?namespace=default');
    const original = run.sandbox.fetch;
    const held = [];
    let wrote = false;
    run.sandbox.fetch = async (url, opts = {}) => {
      const response = await original(url, opts);
      if (String(url) === '/v1/namespaces/default/config') {
        if (opts.method === 'PUT') {
          wrote = true;
          if (stage === 'sameNS edit ABA') return new Promise(resolve => held.push({ response, resolve }));
        } else if (wrote) return new Promise(resolve => held.push({ response, resolve }));
      }
      return response;
    };
    const edit = async text => { run.get('namespace-vars').value = text; await run.get('namespace-vars').fire('input'); };
    const text = '{"SAVED":"old-submission"}'; await edit(text);
    const saving = run.get('namespace-save').onclick(); await run.settle();
    assert.equal(held.length, 1);
    await edit('{"TEMP":"intermediate edit"}'); await edit(text);
    const before = [run.get('namespace-vars').value, run.get('namespace-revisions').textContent, b2Text(run.get('namespace-state')), run.get('namespace-save').disabled, run.calls.length];
    held[0].resolve(held[0].response); await saving; await run.settle();
    assert.deepEqual([run.get('namespace-vars').value, run.get('namespace-revisions').textContent, b2Text(run.get('namespace-state')), run.get('namespace-save').disabled, run.calls.length], before);
    assert.equal(b2Text(run.get('namespace-state')).includes('nodes.v4.saved'), false, 'equal bytes do not make a newer edit the old submission');
    assert.equal(run.calls.filter(c => c.opts?.method === 'PUT').length, 1);
  });
}
test('R1b writeOnly original applied path survives template relocation; validation errors and historical resource refs remain masked', async () => {
  const { req, Mock } = loadMock(); const files = r1bSecretFiles();
  const created = await pbCreate(req, files);
  const old = { project: 'a', pin: 123456789, flag: true, blob: { inner: 'old-relocated-secret' }, list: ['old-relocated-array'], route: 'old-relocated-route', nested: { visible: 123456789, hidden: 333555777 } };
  const first = await pbAction(req, 'install', old); assert.equal(first.status, 200);
  const shifted = pbRawEdit(files, d => {
    d['sources/on-push.yaml'].spec.targets[0].params.pin = 123456789;
    d['sources/on-push.yaml'].spec.targets[0].params.newPin = '{{values.pin}}';
  });
  assert.equal((await pbReplace(req, created, shifted)).status, 200);
  const second = await pbAction(req, 'install', { ...old, pin: 987654321 }); assert.equal(second.status, 200);
  const targets = second.body.unapplied.find(u => u.field === 'targets');
  assert.equal(targets.actual[0].params.pin, '***', 'old applied template owns the old actual path');
  assert.equal(targets.declared[0].params.pin, 123456789, 'new literal is unrelated, even when equal to old secret');
  assert.equal(targets.declared[0].params.newPin, '***');
  const current = (await req('/v1/namespaces/default/playbooks/project-chain')).body;
  const invalidNames = pbRawEdit(shifted, d => { d['capabilities/review.yaml'].metadata.name = 'cap-{{values.pin}}-{{values.route}}'; });
  assert.equal((await pbReplace(req, current, invalidNames)).status, 200);
  const before = allFacts(Mock);
  const failure = await pbAction(req, 'install', { ...old, pin: 987654321, route: 'UPPER-invalid-secret' });
  assert.equal(failure.status, 400); assert.equal(failure.body.reason, 'render_name_invalid');
  assert.equal(allFacts(Mock), before);
  assert.equal(JSON.stringify(failure.body).includes('987654321'), false);
  assert.equal(JSON.stringify(failure.body).includes('UPPER-invalid-secret'), false);

  const other = loadMock();
  const names = playbookFiles({ edit: d => {
    Object.assign(d['playbook.yaml'].spec.values.properties, { identity: { type: 'string', writeOnly: true }, version: { type: 'integer', writeOnly: true } });
    d['capabilities/review.yaml'].metadata.name = '{{values.identity}}';
    d['capabilities/review.yaml'].metadata.version = '{{values.version}}';
  } });
  await pbCreate(other.req, names);
  const result = await pbAction(other.req, 'install', { project: 'a', identity: 'secret-resource-identity', version: 123456789 });
  assert.equal(result.status, 200, JSON.stringify(result.body));
  const base = '/v1/namespaces/default/playbooks/project-chain';
  for (const copy of [result.body, ...other.Mock.state.playbookInstalls, (await other.req(base + '/installs')).body, (await other.req(base)).body.status]) {
    assert.equal(JSON.stringify(copy).includes('secret-resource-identity'), false);
    assert.equal(JSON.stringify(copy).includes('123456789'), false);
  }
  assert.ok(other.Mock.state.capabilities.some(c => c.name === 'secret-resource-identity' && c.version === 123456789), 'actual canonical execution identity is unchanged');
});

// F3 parent opaque-CAS decision: tokens are strings, bound to object identity.
// Genuine public mock/API create/delete/recreate; no private counter/tombstone manipulation.
test('F3 opaque resourceVersion survives helper storage and DELETE URL encoding without numeric interpretation', () => {
  const { A } = loadMock();
  const token = 'rv1:uid/new+object:9007199254740993123 ?&#%';
  const body = A.withResourceVersion({ kind: 'Capability', metadata: { name: 'opaque' }, spec: {} }, token);
  assert.equal(body.metadata.resourceVersion, token);
  const url = A.deletePath('/v1/namespaces/prod/capabilities/opaque', token);
  assert.equal(new URL(url, 'http://fixture').searchParams.get('resourceVersion'), token);
  assert.equal(url, '/v1/namespaces/prod/capabilities/opaque?resourceVersion=' + encodeURIComponent(token));
});

test('F3 public API ABA: same-name Capability recreation has new UID and rejects former DELETE resourceVersion', async () => {
  const { req, Mock } = loadMock();
  const url = '/v1/namespaces/default/capabilities';
  const body = { apiVersion: 'anotify/v1', kind: 'Capability', metadata: { name: 'f3-public-aba' }, spec: { next: [], run: { entrypoint: 'run.sh', files: { 'run.sh': 'echo same' } } } };
  const first = await req(url, jsonInit('POST', body));
  assert.equal(first.status, 201);
  const old = first.body.metadata;
  assert.equal(typeof old.resourceVersion, 'string');
  assert.equal((await req(url + '/f3-public-aba?resourceVersion=' + encodeURIComponent(old.resourceVersion), { method: 'DELETE' })).status, 204, 'v4 的 DELETE 成功是 204 无体');
  const recreated = await req(url, jsonInit('POST', body));
  assert.equal(recreated.status, 201);
  assert.notEqual(recreated.body.metadata.uid, old.uid, 'new object must have a different UID after same-name recreation');
  assert.notEqual(recreated.body.metadata.resourceVersion, old.resourceVersion);
  const before = allFacts(Mock);
  const stale = await req(url + '/f3-public-aba?resourceVersion=' + encodeURIComponent(old.resourceVersion), { method: 'DELETE' });
  assert.equal(stale.status, 409, 'former identity token must never delete recreated object');
  assert.equal(stale.body.kind, 'Status');
  assert.equal(allFacts(Mock), before, 'stale DELETE preserves the whole mock state');
  const patch = await req(url + '/f3-public-aba', jsonInit('PATCH', { ...body, metadata: { ...body.metadata, resourceVersion: old.resourceVersion, displayName: 'stale write' } }));
  assert.equal(patch.status, 409);
  assert.equal(allFacts(Mock), before, 'stale PATCH preserves the whole mock state');
  const kept = await req(url + '/f3-public-aba');
  assert.equal(kept.status, 200);
  assert.deepEqual(kept.body, recreated.body);
});

test('F3 public API Node root/config share one opaque resourceVersion token', async () => {
  const { req } = loadMock();
  const node = await req('/v1/nodes/anode-1');
  const config = await req('/v1/nodes/anode-1/config');
  assert.equal(node.status, 200); assert.equal(config.status, 200);
  assert.equal(typeof node.body.metadata.resourceVersion, 'string');
  assert.equal(config.body.metadata.resourceVersion, node.body.metadata.resourceVersion);
  assert.deepEqual(Object.keys(config.body.metadata), ['resourceVersion'], 'config must not gain a public uid field');
  const created = await req('/v1/nodes', jsonInit('POST', { apiVersion: 'anotify/v1', kind: 'Node', metadata: { name: 'm0-created-node' }, spec: { vars: { VALUE: 'before' } } }));
  assert.equal(created.status, 201);
  const base = '/v1/nodes/m0-created-node';
  const initial = await req(base + '/config');
  assert.equal(initial.body.metadata.resourceVersion, created.body.metadata.resourceVersion);
  const saved = await req(base + '/config', jsonInit('PUT', { metadata: initial.body.metadata, vars: { VALUE: 'after' } }));
  assert.equal(saved.status, 200);
  const root = await req(base);
  assert.equal(root.body.metadata.uid, created.body.metadata.uid);
  assert.equal(root.body.metadata.resourceVersion, saved.body.metadata.resourceVersion);
  assert.notEqual(root.body.metadata.resourceVersion, initial.body.metadata.resourceVersion);
  assert.deepEqual(Object.keys(saved.body.metadata), ['resourceVersion']);
});

for (const kind of ['Source', 'Playbook']) {
  test(`M0 public API ${kind} recreation rejects former DELETE and update tokens without changing whole state`, async () => {
    const { req, Mock } = loadMock();
    const name = 'm0-recreated-' + kind.toLowerCase();
    const base = '/v1/namespaces/default/' + kind.toLowerCase() + 's';
    const body = kind === 'Source'
      ? { apiVersion: 'anotify/v1', kind, metadata: { name }, spec: { kind: 'manual', config: {}, targets: [] } }
      : pbWrite(playbookFiles({ slug: name }));
    const first = await req(base, jsonInit('POST', body)); assert.equal(first.status, 201);
    const old = first.body.metadata;
    // Source 删除 = 204 无体；Playbook 删除 = 200 + kept 回执（v4 只有清单删除有回执）。
    const removed = await req(base + '/' + name + '?resourceVersion=' + encodeURIComponent(old.resourceVersion), { method: 'DELETE' });
    assert.equal(removed.status, kind === 'Source' ? 204 : 200);
    if (kind === 'Source') assert.equal(removed.body, null, '204 不得带回响应体');
    const rebuilt = await req(base, jsonInit('POST', body)); assert.equal(rebuilt.status, 201);
    assert.notEqual(rebuilt.body.metadata.uid, old.uid);
    assert.equal(typeof rebuilt.body.metadata.resourceVersion, 'string');
    assert.notEqual(rebuilt.body.metadata.resourceVersion, old.resourceVersion);
    const before = allFacts(Mock);
    for (const operation of ['DELETE', kind === 'Source' ? 'PATCH' : 'PUT']) {
      const response = operation === 'DELETE'
        ? await req(base + '/' + name + '?resourceVersion=' + encodeURIComponent(old.resourceVersion), { method: operation })
        : await req(base + '/' + name, jsonInit(operation, { ...body, metadata: { ...body.metadata, resourceVersion: old.resourceVersion } }));
      assert.equal(response.status, 409); assert.equal(response.body.reason, 'resource_version_conflict');
      assert.equal(response.body.details.current.resourceVersion, rebuilt.body.metadata.resourceVersion);
      assert.equal(allFacts(Mock), before);
    }
    assert.deepEqual((await req(base + '/' + name)).body, rebuilt.body);
    const updated = await req(base + '/' + name, jsonInit(kind === 'Source' ? 'PATCH' : 'PUT', { ...body, metadata: { ...body.metadata, resourceVersion: rebuilt.body.metadata.resourceVersion } }));
    assert.equal(updated.status, 200); assert.equal(updated.body.metadata.uid, rebuilt.body.metadata.uid);
    assert.notEqual(updated.body.metadata.resourceVersion, rebuilt.body.metadata.resourceVersion);
    const afterUpdate = allFacts(Mock);
    assert.equal((await req(base + '/' + name + '?resourceVersion=' + encodeURIComponent(old.resourceVersion), { method: 'DELETE' })).status, 409);
    assert.equal(allFacts(Mock), afterUpdate);
  });
}

test('M0 Playbook commit recreates Capability/Source identities but retains live Capability UID on upgrade', async () => {
  const { req, Mock } = loadMock(); const files = playbookFiles();
  const manifest = await pbCreate(req, files);
  assert.equal((await pbAction(req, 'install', { project: 'm0' })).status, 200);
  const paths = ['/v1/namespaces/default/capabilities/m0-review', '/v1/namespaces/default/sources/m0-push'];
  const old = [];
  for (const path of paths) {
    const res = await req(path); old.push(res.body);
    assert.equal((await req(path + '?resourceVersion=' + encodeURIComponent(res.body.metadata.resourceVersion), { method: 'DELETE' })).status, 204, 'v4 的 DELETE 成功是 204 无体');
  }
  assert.equal((await pbAction(req, 'install', { project: 'm0' })).status, 200);
  const fresh = [];
  for (const [i, path] of paths.entries()) {
    const res = await req(path); assert.equal(res.status, 200); fresh.push(res.body);
    assert.notEqual(res.body.metadata.uid, old[i].metadata.uid);
    assert.notEqual(res.body.metadata.resourceVersion, old[i].metadata.resourceVersion);
    const before = allFacts(Mock);
    assert.equal((await req(path + '?resourceVersion=' + encodeURIComponent(old[i].metadata.resourceVersion), { method: 'DELETE' })).status, 409);
    assert.equal(allFacts(Mock), before);
  }
  assert.deepEqual(fresh[0].spec, old[0].spec, 'identical execution content does not reuse deleted identity');
  assert.equal(fresh[0].metadata.version, old[0].metadata.version);
  const upgradedFiles = pbRawEdit(files, d => { d['capabilities/review.yaml'].spec.run.files['run.sh'] += 'echo upgraded\n'; });
  const updated = await pbReplace(req, manifest, upgradedFiles); assert.equal(updated.status, 200);
  assert.equal(updated.body.metadata.uid, manifest.metadata.uid);
  assert.equal((await pbAction(req, 'install', { project: 'm0' })).status, 200);
  const upgraded = (await req(paths[0])).body;
  assert.equal(upgraded.metadata.uid, fresh[0].metadata.uid, 'a live resource version upgrade is not a new named object');
  assert.notEqual(upgraded.metadata.resourceVersion, fresh[0].metadata.resourceVersion);
});

test('M0 UID/revision allocation occurs only on successful creation/commit, never GET/render/dryRun/conflict', async () => {
  const control = loadMock(), exercised = loadMock();
  const files = playbookFiles({ edit: d => { d['capabilities/review.yaml'].metadata.version = 1; } });
  for (const run of [control, exercised]) await pbCreate(run.req, files);
  const before = allFacts(exercised.Mock);
  for (const path of ['/v1/nodes/anode-1', '/v1/nodes/anode-1/config', '/v1/namespaces/default/capabilities/fixer', '/v1/namespaces/default/sources']) assert.equal((await exercised.req(path)).status, 200);
  assert.equal((await pbAction(exercised.req, 'render')).status, 200);
  assert.equal((await pbAction(exercised.req, 'install', { project: 'a' }, { dryRun: true })).status, 200);
  assert.equal((await exercised.req('/v1/namespaces/default/playbooks', jsonInit('POST', pbWrite(files)))).status, 409);
  assert.equal((await exercised.req('/v1/nodes', jsonInit('POST', { apiVersion: 'anotify/v1', kind: 'Node', metadata: { name: 'invalid-node' }, spec: { namespaces: [] } }))).status, 400);
  assert.equal(allFacts(exercised.Mock), before);
  for (const run of [control, exercised]) assert.equal((await pbAction(run.req, 'install')).status, 200);
  const committed = allFacts(exercised.Mock);
  const conflict = await pbAction(exercised.req, 'install', { project: 'a', env: 'other' });
  assert.equal(conflict.status, 400, 'explicit same-version different content rejects the entire plan');
  // 只有能力冲突（源 config 没变、targets 差异走 unapplied）→ version_content_immutable。
  assert.equal(conflict.body.reason, 'version_content_immutable');
  assert.equal(conflict.body.details.conflicts[0].kind, 'capability');
  assert.equal(allFacts(exercised.Mock), committed);
  for (const path of ['/v1/namespaces/default/capabilities/a-review', '/v1/namespaces/default/capabilities/a-fixer', '/v1/namespaces/default/sources/a-push']) {
    const a = (await control.req(path)).body.metadata, b = (await exercised.req(path)).body.metadata;
    assert.equal(b.uid, a.uid); assert.equal(b.resourceVersion, a.resourceVersion);
  }
  const body = { apiVersion: 'anotify/v1', kind: 'Node', metadata: { name: 'm0-counter-probe' }, spec: {} };
  const a = await control.req('/v1/nodes', jsonInit('POST', body)), b = await exercised.req('/v1/nodes', jsonInit('POST', body));
  assert.equal(a.status, 201); assert.equal(b.status, 201);
  assert.equal(b.body.metadata.uid, a.body.metadata.uid); assert.equal(b.body.metadata.resourceVersion, a.body.metadata.resourceVersion);
});

test('M0 restored mock snapshot retains identity/revision allocation across same-name recreation', async () => {
  const initial = loadMock(); const base = '/v1/namespaces/default/capabilities';
  const body = { apiVersion: 'anotify/v1', kind: 'Capability', metadata: { name: 'm0-snapshot-aba' }, spec: { next: [], run: { entrypoint: 'run.sh', files: { 'run.sh': 'echo snapshot' } } } };
  const first = await initial.req(base, jsonInit('POST', body)); assert.equal(first.status, 201);
  const restored = loadMock();
  Object.assign(restored.Mock.state, JSON.parse(allFacts(initial.Mock)), { latencyMs: 0 });
  const old = first.body.metadata; const path = base + '/m0-snapshot-aba';
  assert.equal((await restored.req(path + '?resourceVersion=' + encodeURIComponent(old.resourceVersion), { method: 'DELETE' })).status, 204, 'v4 的 DELETE 成功是 204 无体');
  const fresh = await restored.req(base, jsonInit('POST', body)); assert.equal(fresh.status, 201);
  assert.notEqual(fresh.body.metadata.uid, old.uid);
  assert.notEqual(fresh.body.metadata.resourceVersion, old.resourceVersion);
  // PATCH 只改 metadata.displayName（内容面不可变：写 spec 会被 400 immutable_field 拒掉）
  const updated = await restored.req(path, jsonInit('PATCH', { apiVersion: 'anotify/v1', kind: 'Capability', metadata: { name: 'm0-snapshot-aba', resourceVersion: fresh.body.metadata.resourceVersion, displayName: 'updated new object' } }));
  assert.equal(updated.status, 200); assert.equal(updated.body.metadata.uid, fresh.body.metadata.uid);
  assert.notEqual(updated.body.metadata.resourceVersion, fresh.body.metadata.resourceVersion);
  assert.notEqual(updated.body.metadata.resourceVersion, old.resourceVersion);
  const before = allFacts(restored.Mock);
  assert.equal((await restored.req(path + '?resourceVersion=' + encodeURIComponent(old.resourceVersion), { method: 'DELETE' })).status, 409);
  assert.equal((await restored.req(path, jsonInit('PATCH', { apiVersion: 'anotify/v1', kind: 'Capability', metadata: { name: 'm0-snapshot-aba', resourceVersion: old.resourceVersion, displayName: 'stale' } }))).status, 409);
  assert.equal(allFacts(restored.Mock), before);
  assert.deepEqual((await restored.req(path)).body, updated.body);
});

test('F3 report persistence retains real failure across explicit-mock and same-mode later attempts', async () => {
  const { persistReport } = await import('./e2e/fixtures/f3/f3-driver.mjs');
  const H = await import('./e2e/lib/harness.mjs');
  const { readFile } = await import('node:fs/promises');
  const directory = H.mkdtempTmp('anotify-f3-evidence-');
  try {
    const first = { id: 'evidence-probe', runId: 'unit', mode: 'real', attemptId: 'first', cwd: ROOT, sourceHash: 'fixture-source', checks: [], errors: ['ANOTIFY_BIN missing'] };
    const second = { ...first, mode: 'explicit-mock', attemptId: 'second', checks: ['fixture assertion'], errors: [] };
    const third = { ...second, attemptId: 'third', checks: [], errors: ['later mock failure'] };
    const firstPath = await persistReport(first, directory);
    const secondPath = await persistReport(second, directory);
    const thirdPath = await persistReport(third, directory);
    assert.deepEqual(JSON.parse(await readFile(firstPath, 'utf8')), first, 'original real missing-binary failure must not be overwritten');
    assert.deepEqual(JSON.parse(await readFile(secondPath, 'utf8')), second, 'same-mode next attempt must not overwrite earlier evidence');
    assert.deepEqual(JSON.parse(await readFile(thirdPath, 'utf8')), third);
    assert.equal(new Set([firstPath, secondPath, thirdPath]).size, 3);
  } finally { H.removeTmpDir(directory); }
});

// G1 red mode substitutes ONLY the four frozen mock seams from immutable fec025f
// in memory. Current UID/allocation code and the worktree are never reverted.
function g1Function(source, name) {
  const start = source.indexOf('\n\tfunction ' + name + '(');
  assert.ok(start >= 0, name + ' exists');
  const end = source.indexOf('\n\tfunction ', start + 1);
  assert.ok(end > start, name + ' has a bounded next function');
  return source.slice(start, end);
}
function g1Mock() {
  if (process.env.G1_BASELINE !== 'fec025f') return loadMock();
  const snapshot = execFileSync('git', ['show', 'fec025f:web/mock.js'], { cwd: ROOT, encoding: 'utf8', timeout: 10000 });
  let mockSrc = MOCK_SRC;
  for (const name of ['pbVersions', 'pbStatus', 'pbPlan', 'pbCommit']) mockSrc = mockSrc.replace(g1Function(mockSrc, name), g1Function(snapshot, name));
  return loadMock({ mockSrc });
}
function g1VersionFiles() {
  return playbookFiles({ edit: d => {
    d['playbook.yaml'].spec.values.properties.version = { type: 'integer', writeOnly: true };
    d['capabilities/review.yaml'].metadata.version = '{{values.version}}';
    d['capabilities/review.yaml'].spec.run.files['fixed.txt'] = '17';
  } });
}
const G1_PB = '/v1/namespaces/default/playbooks/project-chain';
test('G1 M1 sensitive version upgrade/from/to and implicit reuse refs stay masked without altering raw version/digest/actions', async () => {
  const { req, Mock } = g1Mock(); const files = g1VersionFiles(); await pbCreate(req, files);
  const first = await pbAction(req, 'install', { project: 'a', version: 17 }); assert.equal(first.status, 200);
  const raw = await pbAction(req, 'render', { project: 'a', version: 23 });
  const second = await pbAction(req, 'install', { project: 'a', version: 23 }); assert.equal(second.status, 200);
  const upgraded = second.body.upgraded.find(u => u.capability === 'a-review'); assert.ok(upgraded);
  assert.equal(upgraded.from, '***', 'old writeOnly version is masked in derived from');
  assert.equal(upgraded.to, '***', 'new writeOnly version is masked in derived to');
  assert.equal(upgraded.affects.sources, 1, 'unrelated integer fact is intact');
  assert.equal(second.body.valuesDigest, raw.body.valuesDigest);
  assert.equal(raw.body.capabilities[0].metadata.version, 23);
  const applied = Mock.state.capabilities.find(c => c.name === 'a-review');
  assert.equal(applied.version, 23); assert.equal(applied.spec.run.files['fixed.txt'], '17');
  const digest = applied.digest;
  const current = (await req(G1_PB)).body;
  const reusedFiles = pbRawEdit(files, d => { delete d['capabilities/review.yaml'].metadata.version; });
  assert.equal((await pbReplace(req, current, reusedFiles)).status, 200);
  const reuse = await pbAction(req, 'install', { project: 'a', version: 23 }); assert.equal(reuse.status, 200);
  assert.ok(reuse.body.skipped.capabilities.includes('***'), 'implicit reuse retains original sensitive identity');
  assert.ok(reuse.body.notes.every(n => n.code !== '***'), 'fixed note codes remain');
  assert.equal(Mock.state.capabilities.find(c => c.name === 'a-review').digest, digest);
  const copies = [second.body, reuse.body, (await req(G1_PB + '/installs/' + second.body.install.id)).body,
    (await req(G1_PB + '/installs/' + reuse.body.install.id)).body, ...Mock.state.playbookInstalls.map(i => i.receipt)];
  for (const copy of copies) { assert.equal(JSON.stringify(copy).includes('a-review@23'), false); assert.equal(JSON.stringify(copy).includes('a-review@17'), false); }
  assert.equal((await req(G1_PB)).body.status.blocker.action, 'deploy', 'fixture has compatible nodes but no applied deployment');
  // Identical non-secret integers MUST NOT be globally masked.
  const plainRun = loadMock(); const plainFiles = pbRawEdit(files, d => { delete d['playbook.yaml'].spec.values.properties.version.writeOnly; });
  await pbCreate(plainRun.req, plainFiles);
  await pbAction(plainRun.req, 'install', { project: 'a', version: 17 });
  const publicUpgrade = await pbAction(plainRun.req, 'install', { project: 'a', version: 23 });
  const publicRow = publicUpgrade.body.upgraded.find(u => u.capability === 'a-review');
  assert.equal(publicRow.from, 17); assert.equal(publicRow.to, 23);
});
test('G1 M2 each archived capability retains its own deep-copied policy/ref across schema/value change, old reuse/status/history', async () => {
  const { req, Mock } = g1Mock();
  const files = pbRawEdit(g1VersionFiles(), d => {
    d['playbook.yaml'].spec.values.properties.route = { type: 'string', writeOnly: true };
    d['capabilities/review.yaml'].spec.next = ['{{values.route}}'];
  });
  await pbCreate(req, files);
  const first = await pbAction(req, 'install', { project: 'a', version: 11, route: 'old-secret-route' }); assert.equal(first.status, 200);
  const oldRaw = plain(Mock.state.capabilities.find(c => c.name === 'a-review'));
  const secondFiles = pbRawEdit(files, d => {
    d['capabilities/review.yaml'].metadata.version = 12;
    d['capabilities/review.yaml'].spec.next = ['new-public-route'];
    delete d['playbook.yaml'].spec.values.properties.version.writeOnly;
    delete d['playbook.yaml'].spec.values.properties.route.writeOnly;
  });
  assert.equal((await pbReplace(req, (await req(G1_PB)).body, secondFiles)).status, 200);
  const second = await pbAction(req, 'install', { project: 'a', version: 12, route: 'public-value', env: 'stage' }); assert.equal(second.status, 200);
  const live = Mock.state.capabilities.find(c => c.name === 'a-review'), archive = live.versions.find(v => v.version === 11);
  assert.deepEqual(plain(archive.receiptPolicy), oldRaw.receiptPolicy, 'archive has ORIGINAL schema/template provenance');
  assert.equal(archive.receiptRef, oldRaw.receiptRef, 'archive has ORIGINAL display identity');
  assert.notStrictEqual(archive.receiptPolicy, live.receiptPolicy);
  assert.equal(archive.digest, oldRaw.digest); assert.deepEqual(plain(archive.spec), oldRaw.spec);
  const history = (await req(G1_PB + '/installs')).body;
  assert.ok(history.installs.find(g => g.valuesDigest === first.body.valuesDigest).capabilities.includes('***'));
  assert.ok(history.installs.find(g => g.valuesDigest === second.body.valuesDigest).capabilities.includes('a-review@12'));
  assert.equal(JSON.stringify((await req(G1_PB)).body.status).includes('old-secret-route'), false);
  const reuseFiles = pbRawEdit(secondFiles, d => { d['capabilities/review.yaml'].metadata.version = 11; });
  assert.equal((await pbReplace(req, (await req(G1_PB)).body, reuseFiles)).status, 200);
  const reuse = await pbAction(req, 'install', { project: 'a', version: 11, route: 'public-value', env: 'prod' }); assert.equal(reuse.status, 200);
  const u = reuse.body.unapplied.find(u => u.kind === 'capability'); assert.ok(u);
  assert.deepEqual(u.actual, ['***']); assert.deepEqual(u.declared, ['new-public-route']); assert.equal(u.ref, '***');
  assert.equal(JSON.stringify((await req(G1_PB + '/installs/' + reuse.body.install.id)).body).includes('old-secret-route'), false);
  assert.equal(Mock.state.capabilities.find(c => c.name === 'a-review').version, 12, 'reuse does not change pinned actual');
});
test('G1 M3 status masks sensitive-derived messages but preserves edit_capability/control and unrelated same-value identity', async () => {
  const { req } = g1Mock();
  const files = playbookFiles({ edit: d => {
    d['playbook.yaml'].spec.values.properties.route = { type: 'string', writeOnly: true };
    d['capabilities/review.yaml'].spec.next = ['{{values.route}}'];
    d['capabilities/fixer.yaml'].metadata.name = 'edit';
  } });
  await pbCreate(req, files);
  assert.equal((await pbAction(req, 'install', { project: 'a', route: 'edit' })).status, 200);
  const second = await pbAction(req, 'install', { project: 'a', route: 'missing-next' }); assert.equal(second.status, 200);
  const status = (await req(G1_PB)).body.status;
  assert.equal(status.blocker.action, 'edit_capability', 'fixed action must not be swept by secret substring edit');
  assert.equal(status.blocker.type, 'routing_unapplied'); assert.equal(status.blocker.capability, 'a-review@1');
  assert.ok(status.items.some(i => i.name === 'edit'), 'unrelated literal resource name equal to secret stays navigable');
  assert.ok(status.conditions.every(c => ['Implemented', 'NodesAvailable', 'DownstreamResolved', 'Deployed'].includes(c.type)));
  assert.deepEqual(status.unapplied[0].actual, ['***']); assert.deepEqual(status.unapplied[0].declared, ['***']);
  assert.equal(status.unapplied[0].field, 'next'); assert.equal(status.ready, false);
  assert.equal(second.body.notes.find(n => n.code === 'routing_unapplied').code, 'routing_unapplied');
  // A missing sensitive next appears only inside a mixed condition message, not control metadata.
  const missing = loadMock(); await pbCreate(missing.req, files);
  await pbAction(missing.req, 'install', { project: 'a', route: 'edit-private-missing' });
  const mixed = (await missing.req(G1_PB)).body.status;
  assert.equal(mixed.conditions.find(c => c.type === 'DownstreamResolved').message, '***');
  assert.equal(mixed.conditions.find(c => c.type === 'DownstreamResolved').reason, 'NotFound');
});
test('G1 masked resource identifier is not an actual URL while valid identity and masked-version links stay valid', () => {
  const source = readFileSync(path.join(ROOT, 'web-src/pages/playbook.html'), 'utf8');
  const start = source.indexOf('    function resourceLink('), end = source.indexOf('    function manualAction(', start);
  const ctx = vm.createContext({ el: (tag, cls, text) => ({ tag, cls, text }), scoped: (page, key, value) => `${page}.html?namespace=prod&${key}=${encodeURIComponent(value)}` });
  vm.runInContext(source.slice(start, end) + '\n globalThis.link = resourceLink;', ctx);
  for (const kind of ['capability', 'source']) assert.equal(ctx.link(kind, '***').href, undefined, 'masked identity must not become resource URL');
  assert.equal(ctx.link('capability', 'real-cap@***').href, 'capabilities.html?namespace=prod&name=real-cap');
  assert.equal(ctx.link('capability', 'real-cap@17').href, 'capabilities.html?namespace=prod&name=real-cap');
  assert.equal(ctx.link('source', 'real-source').href, 'source.html?namespace=prod&name=real-source');
});

test('G1 Silk pan defers capture for native clicks and retains horizontal/vertical drag suppression and cleanup', () => {
  const source = readFileSync(path.join(ROOT, 'web/flograph.js'), 'utf8');
  const start = source.indexOf('\t\tfunction onPointerDown(ev) {'), end = source.indexOf('\t\tconst onStopFollow', start);
  const listeners = new Map(), captured = [], released = [], classes = new Set();
  const target = { scrollLeft: 100, classList: { add: x => classes.add(x), remove: x => classes.delete(x) },
    setPointerCapture: id => captured.push(id), hasPointerCapture: id => captured.includes(id) && !released.includes(id), releasePointerCapture: id => released.push(id),
    addEventListener: (t, fn) => listeners.set(t, fn), removeEventListener: t => listeners.delete(t) };
  const ctx = vm.createContext({ scroll: target, window: target, followOff: false, dragMoved: false });
  vm.runInContext(source.slice(start, end) + '\n globalThis.down = onPointerDown;', ctx);
  const event = (x, y = 20) => ({ pointerType: 'mouse', button: 0, pointerId: 1, clientX: x, clientY: y, preventDefault() {} });
  ctx.down(event(50)); assert.equal(captured.length, 0, 'no capture before actual movement');
  listeners.get('pointermove')(event(52)); assert.equal(ctx.dragMoved, false); assert.equal(target.scrollLeft, 100);
  listeners.get('pointerup')(event(52)); assert.equal(listeners.size, 0);
  ctx.down(event(50)); listeners.get('pointermove')(event(30)); assert.equal(ctx.dragMoved, true);
  assert.equal(target.scrollLeft, 120); assert.equal(captured.length, 1);
  listeners.get('pointerup')(event(30)); assert.equal(released.length, 1); assert.equal(classes.size, 0); assert.equal(listeners.size, 0);
  ctx.down(event(50)); listeners.get('pointermove')(event(50, 50)); assert.equal(ctx.dragMoved, true, 'vertical node drag also suppresses navigation');
  listeners.get('pointercancel')(event(50, 50)); assert.equal(listeners.size, 0);
  const before = captured.length; ctx.down({ ...event(50), pointerType: 'touch' }); assert.equal(captured.length, before, 'native touch scrolling remains uncaptured');
});

test('G1 masked blocker identity has no blind resource URL but keeps identity-free Node action and valid edit actions', () => {
  const source = readFileSync(path.join(ROOT, 'web-src/pages/playbook.html'), 'utf8');
  const start = source.indexOf('    function manualAction('), end = source.indexOf('    function showUnapplied(', start);
  const ctx = vm.createContext({ el: (tag, cls, text) => ({ tag, cls, text }), t: () => 'Manual action', scoped: (page, key, value) => `${page}.html?namespace=prod${key ? `&${key}=${encodeURIComponent(value)}` : ''}` });
  vm.runInContext(source.slice(start, end) + '\n globalThis.action = manualAction;', ctx);
  assert.equal(ctx.action({ action: 'edit_capability', capability: '***' }).href, undefined, 'masked blocker identity must not become URL');
  assert.equal(ctx.action({ action: 'edit_source', capability: '***' }).href, undefined);
  assert.equal(ctx.action({ action: 'open_node_page', capability: '***' }).href, 'nodes.html?namespace=prod');
  assert.equal(ctx.action({ action: 'edit_capability', capability: 'real-cap@***' }).href, 'capabilities.html?namespace=prod&name=real-cap');
  assert.equal(ctx.action({ action: 'edit_source', capability: 'real-source' }).href, 'source.html?namespace=prod&name=real-source');
});

test('G1 M3 condition provenance masks a private versioned-next derived name without masking equal public next or unrelated conditions', async () => {
  const { req } = g1Mock();
  const files = playbookFiles({ edit: d => {
    d['playbook.yaml'].spec.values.properties.route = { type: 'string', writeOnly: true };
    d['capabilities/review.yaml'].spec.next = ['{{values.route}}'];
    d['capabilities/fixer.yaml'].spec.next = ['masked-destination'];
  } });
  await pbCreate(req, files);
  const install = await pbAction(req, 'install', { project: 'a', route: 'masked-destination@17' }); assert.equal(install.status, 200);
  const status = (await req(G1_PB)).body.status;
  const downstream = status.conditions.find(c => c.type === 'DownstreamResolved');
  assert.equal(downstream.message, '***; 下游解析不到：masked-destination', 'only the private next-derived message is masked, not equal public identity');
  assert.equal(downstream.reason, 'NotFound'); assert.deepEqual(downstream.members, ['a-review', 'a-fixer']);
  assert.equal(status.conditions.find(c => c.type === 'Deployed').message.includes('***'), false, 'deployment facts do not derive from next');
});

// F3b2a locale registrations and aggregate anti-degeneration (original 141 retained).
test('F3b2a both locale suites register all nineteen pages in each of four languages and retained interactions', async () => {
  const { casesFor, pageBatches } = await import('./e2e/fixtures/f3/f3-i18n-cases.mjs');
  const { PAGES, LANGS } = await import('./e2e/fixtures/f3/f3-driver.mjs');
  assert.equal(pageBatches.flat().length, 19);
  for (const suite of ['i18n', 'i18n_coverage']) {
    const cases = casesFor(suite); assert.equal(new Set(cases.map(x => x.id)).size, cases.length);
    for (const lang of LANGS) {
      assert.deepEqual(cases.filter(x => x.lang === lang && x.pages).flatMap(x => x.pages), PAGES);
      assert.ok(cases.some(x => x.id === 'authenticated-login-' + lang));
      if (suite === 'i18n_coverage') for (const id of ['retained', 'resource-states', 'task-states', 'conflict-draft']) assert.ok(cases.some(x => x.id === id + '-' + lang));
    }
  }
});
test('F3b2a locale residue gate rejects nonempty Chinese leaks and Chinese-only Japanese wording', async () => {
  const { residue } = await import('./e2e/fixtures/f3/f3-i18n-cases.mjs');
  assert.deepEqual(residue('Overview\n已停用', 'en'), ['已停用']);
  assert.deepEqual(residue('Resumen\n通知接收', 'es'), ['通知接收']);
  assert.deepEqual(residue('概要 セキュリティ 已停用', 'ja'), ['已停用']);
  assert.deepEqual(residue('概要 セキュリティ', 'ja'), []);
});
test('F3b2a locale aggregate rejects missing pages, no-op checks, wrong mode/source/attempt/cwd and actual nonzero exits', async () => {
  const { validateLocaleEvidence: validate } = await import('./e2e/fixtures/f3/f3-i18n-runner.mjs');
  const expected = { suite:'i18n', id:'pages-en-1', runId:'test', attemptId:'a', cwd:process.cwd(), sourceHash:'hash', mode:'explicit-mock', authority:'explicit-mock', pages:['admin','keys'], lang:'en' };
  const meta = Object.fromEntries(Object.entries(expected).filter(([k]) => !['pages','lang'].includes(k)));
  const result = {...meta, ok:true, checks:2, report:{errors:[], pages:expected.pages.map(page => ({page,lang:'en',attempted:true,ok:true}))}};
  const exit = {...meta,exitCode:0,signal:null,timedOut:false};
  validate(meta,result,exit,expected);
  for(const key of ['cwd','sourceHash','mode','attemptId'])assert.throws(()=>validate({...meta,[key]:'wrong'},result,exit,expected));
  assert.throws(()=>validate(meta,{...result,checks:0},exit,expected));
  assert.throws(()=>validate(meta,{...result,report:{errors:[],pages:result.report.pages.slice(0,1)}},exit,expected));
  assert.throws(()=>validate(meta,result,{...exit,exitCode:1},expected));
  assert.throws(()=>validate(meta,{...result,report:{...result.report,errors:['mock 未实现 501']}},exit,expected));
});

// G2/L1: four finite regressions; the original 144 callbacks are unchanged.
test('G2 external archived private-version provenance masks separate consumer receipt/history but preserves pinned execution and public control', async () => {
  for (const privateVersion of [true, false]) {
    const { req, Mock } = loadMock();
    const files = pbRawEdit(g1VersionFiles(), d => {
      if (!privateVersion) delete d['playbook.yaml'].spec.values.properties.version.writeOnly;
    });
    await pbCreate(req, files);
    assert.equal((await pbAction(req, 'install', { project: 'a', version: 17 })).status, 200);
    assert.equal((await pbAction(req, 'install', { project: 'a', version: 23 })).status, 200);
    const cap = Mock.state.capabilities.find(c => c.name === 'a-review');
    const archive = cap.versions.find(v => v.version === 17);
    assert.equal(archive.receiptRef, privateVersion ? '***' : 'a-review@17');
    const execution = plain({ version: cap.version, digest: archive.digest, spec: archive.spec });
    const consumer = playbookFiles({ slug: 'archive-consumer', edit: d => {
      d['capabilities/review.yaml'].spec.next = ['a-review@17'];
    } });
    await pbCreate(req, consumer);
    const raw = await pbAction(req, 'render', { project: 'b' }, {}, 'archive-consumer');
    const install = await pbAction(req, 'install', { project: 'b' }, {}, 'archive-consumer');
    assert.equal(install.status, 200);
    const base = '/v1/namespaces/default/playbooks/archive-consumer';
    const expected = [privateVersion ? '***' : 'a-review@17'];
    assert.deepEqual(install.body.external, expected);
    const stored = Mock.state.playbookInstalls.find(i => i.id === install.body.install.id).receipt;
    const detail = (await req(base + '/installs/' + install.body.install.id)).body;
    const history = (await req(base + '/installs')).body;
    for (const receipt of [stored, detail]) assert.deepEqual(plain(receipt.external), expected);
    if (privateVersion) assert.equal(JSON.stringify([install.body, stored, detail, history]).includes('a-review@17'), false);
    assert.equal(raw.body.capabilities[0].spec.next[0], 'a-review@17');
    assert.equal(Mock.state.capabilities.find(c => c.name === 'b-review').spec.next[0], 'a-review@17');
    assert.deepEqual(plain({ version: cap.version, digest: archive.digest, spec: archive.spec }), execution);
  }
});

test('G2 Source whole-array/object and mixed target ancestor policies mask only derived status targets, keep raw params/public identity and unapplied control', async () => {
  const targetSchema = { type: 'object', properties: { capability: { type: 'string' }, params: { type: 'object', properties: { fixed: { type: 'integer' } } } } };
  for (const shape of ['array', 'object', 'mixed']) {
    const { req, Mock } = loadMock();
    const targets = [{ capability: 'a-review', params: { fixed: 17 } }];
    const files = playbookFiles({ edit: d => {
      if (shape === 'mixed') {
        d['playbook.yaml'].spec.values.properties.target = { ...targetSchema, writeOnly: true };
        d['sources/on-push.yaml'].spec.targets = ['{{values.target}}', { capability: 'a-review', params: { fixed: 17 } }];
      } else {
        d['playbook.yaml'].spec.values.properties.targets = { type: 'array', ...(shape === 'array' ? { writeOnly: true } : {}), items: { ...targetSchema, ...(shape === 'object' ? { writeOnly: true } : {}) } };
        d['sources/on-push.yaml'].spec.targets = '{{values.targets}}';
      }
    } });
    const values = { project: 'a', ...(shape === 'mixed' ? { target: targets[0] } : { targets }) };
    await pbCreate(req, files);
    const raw = await pbAction(req, 'render', values);
    const install = await pbAction(req, 'install', values); assert.equal(install.status, 200);
    const applied = plain(Mock.state.sources.find(s => s.name === 'a-push').targets);
    assert.deepEqual(applied, raw.body.sources[0].spec.targets);
    const status = (await req(G1_PB)).body.status;
    assert.deepEqual(status.items.find(i => i.kind === 'source').targets, shape === 'mixed' ? ['***', 'a-review'] : ['***']);
    assert.ok(status.items.some(i => i.kind === 'capability' && i.name === 'a-review'));
    assert.equal(status.blocker.action, 'deploy');
    const changed = { project: 'a', ...(shape === 'mixed' ? { target: { capability: 'a-fixer', params: { fixed: 23 } } } : { targets: [{ capability: 'a-fixer', params: { fixed: 23 } }] }) };
    const reused = await pbAction(req, 'install', changed); assert.equal(reused.status, 200);
    const next = (await req(G1_PB)).body.status;
    assert.equal(next.blocker.action, 'edit_source'); assert.equal(next.blocker.type, 'targets_unapplied');
    assert.deepEqual(next.items.find(i => i.kind === 'source').targets, shape === 'mixed' ? ['***', 'a-review'] : ['***']);
    assert.equal(next.unapplied[0].field, 'targets'); assert.equal(next.unapplied[0].reason, 'targets_unapplied');
    assert.deepEqual(plain(Mock.state.sources.find(s => s.name === 'a-push').targets), applied);
    const detail = (await req(G1_PB + '/installs/' + reused.body.install.id)).body;
    assert.deepEqual(detail.unapplied, reused.body.unapplied);
    // Receipt-wide equal-value masking is pre-existing; this card freezes that engine.
    // The new contract is private receipt provenance plus public STATUS/raw facts above.
    if (shape === 'mixed') assert.equal(reused.body.unapplied[0].actual[0], '***');
    else assert.deepEqual(reused.body.unapplied[0].actual, shape === 'array' ? '***' : ['***']);
  }
});

test('L1 retained exact password login validates known user/hash, preserves raw whitespace and AUTH1 identity/session/logout shape', async () => {
  const { req, Mock } = loadMock();
  const route = '/v1/auth/password/login';
  assert.deepEqual((await req('/v1/auth/logout', jsonInit('POST', {}))).body, { ok: true });
  for (const body of [{ username: 'nonexistent', password: 'mock-only-password' }, { username: 'demo', password: 'wrong' }, { username: 'demo', password: ' mock-only-password ' }, { username: 'demo', password: 42 }, {}]) {
    const r = await req(route, jsonInit('POST', body));
    assert.equal(r.status, typeof body.password === 'string' ? 401 : 400); assert.deepEqual(Object.keys(r.body), ['error']); assert.ok(r.body.error);
    assert.equal((await req('/v1/auth/me')).status, 401, 'wrong credentials never establish a session');
  }
  const login = await req(route, jsonInit('POST', { username: 'demo', password: 'mock-only-password' }));
  assert.equal(login.status, 200); assert.deepEqual(login.body, { ok: true });
  const me = (await req('/v1/auth/me')).body;
  assert.equal(me.username, 'demo'); assert.equal(me.displayName, 'demo'); assert.equal(me.role, 'admin');
  assert.equal(me.user, undefined); assert.equal(me.authenticated, undefined); assert.equal(me.kind, undefined);
  const updated = await req('/v1/auth/password', jsonInit('POST', { currentPassword: 'mock-only-password', newPassword: ' trailing-space ' })); assert.equal(updated.status, 200);
  await req('/v1/auth/logout', jsonInit('POST', {}));
  assert.equal((await req(route, jsonInit('POST', { username: 'demo', password: 'trailing-space' }))).status, 401);
  assert.equal((await req(route, jsonInit('POST', { username: 'demo', password: ' trailing-space ' }))).status, 200);
  assert.ok(Mock.state.auth.passwordHash);
});

test('L1 retained device PATCH enabled=false preserves exact identity/other facts and rejects missing or invalid updates with DEV1 wire', async () => {
  const { req, Mock } = loadMock();
  const device = { id: 'f3-locale-device', userId: 'usr_mock_demo', name: 'Locale Receiver', enabled: true, eventScope: 'all', tags: ['fixed'], endpoint: 'https://push.example.invalid/device' };
  Mock.state.devices.push(device);
  const before = plain(device);
  const route = '/v1/devices/' + device.id;
  const updated = await req(route, jsonInit('PATCH', { enabled: false }));
  assert.equal(updated.status, 200); assert.deepEqual(updated.body, { ok: true, device: { ...before, enabled: false } });
  const current = (await req('/v1/devices')).body.devices.find(d => d.id === device.id);
  assert.deepEqual(current, { ...before, enabled: false }); assert.equal(current.enabled, false);
  assert.equal((await req(route, jsonInit('PATCH', {}))).status, 200, 'DEV1 omitted fields keep facts');
  for (const body of [{ enabled: 'false' }, { enabled: false, uid: 'replace-uid' }]) {
    const invalid = await req(route, jsonInit('PATCH', body));
    assert.equal(invalid.status, 400); assert.deepEqual(Object.keys(invalid.body), ['error']);
  }
  const missing = await req('/v1/devices/missing-device', jsonInit('PATCH', { enabled: false }));
  assert.equal(missing.status, 404); assert.deepEqual(Object.keys(missing.body), ['error']);
  assert.deepEqual((await req('/v1/devices')).body.devices.find(d => d.id === device.id), current);
});

async function l1Boot(response, search = '') {
  const base = makeSandbox({ pathname: '/login.html', search, fetch: async () => {
    if (response instanceof Error) throw response;
    if (response === 'bad-json') return { status: 200, json: async () => { throw new SyntaxError('bad JSON'); } };
    return jsonResponse(response.body, response.status);
  } });
  const nodes = new Map();
  base.sandbox.document.getElementById = id => {
    if (!nodes.has(id)) nodes.set(id, Object.assign(fakeNode(), { value: '' }));
    return nodes.get(id);
  };
  base.sandbox.Anotify = { t: (_key, fallback) => fallback || '', b64urlToBuf: x => x, bufToB64url: x => x };
  base.sandbox.URLSearchParams = URLSearchParams;
  base.sandbox.performance = { now: () => 0 };
  base.sandbox.addEventListener = () => {};
  const source = readFileSync(path.join(ROOT, 'web-src/pages/login.html'), 'utf8');
  const script = source.slice(source.indexOf('{{define "script"}}')).match(/<script>([\s\S]*?)<\/script>/)[1];
  const ctx = vm.createContext(base.sandbox);
  vm.runInContext('globalThis.window = globalThis;', ctx);
  vm.runInContext(script, ctx, { filename: 'web-src/pages/login.html' });
  await new Promise(resolve => setTimeout(resolve, 0));
  return base;
}
test('L1 actual login controller boots only valid canonical200 sessions through safeNEXT; 401/wrappers/invalid/network/JSON stay form', async () => {
  const user = { id: 'usr_mock_demo', username: 'demo', displayName: 'demo', role: 'admin', disabled: false, createdAt: 123 };
  const valid = await l1Boot({ status: 200, body: user }, '?next=nodes.html%3Fnamespace%3Dprod');
  assert.equal(valid.location.href, 'nodes.html?namespace=prod');
  assert.equal(valid.calls.length, 1, 'credentialed me probe needs no namespace bridge');
  assert.equal(valid.calls[0].path, '/v1/auth/me'); assert.equal(valid.calls[0].init.credentials, 'include');
  for (const next of ['https://evil.invalid', '//evil.invalid', 'unknown']) {
    assert.equal((await l1Boot({ status: 200, body: user }, '?next=' + encodeURIComponent(next))).location.href, 'index.html');
  }
  assert.equal((await l1Boot({ status: 200, body: user }, '?next=' + encodeURIComponent('nodes.html?bad=\\evil'))).location.href, 'nodes.html', 'existing safeNEXT drops only the unsafe query');
  for (const response of [{ status: 401, body: user }, { status: 201, body: user }, { status: 200, body: null }, { status: 200, body: { user } }, { status: 200, body: { authenticated: true } }, { status: 200, body: { username: 'demo' } }, { status: 200, body: { ...user, role: '' } }, 'bad-json', new TypeError('offline')]) {
    const anonymous = await l1Boot(response, '?authenticated=1&username=demo');
    assert.equal(anonymous.location.href, '', 'anonymous/failed probe must not claim login');
  }
});

// F3b2b: last two suites — truthful 304-view registry, retained UX19–23 and
// aggregate rules that cannot be satisfied by a partial or retried run.
test('F3b2b ui_shift registers every one of the 304 page views exactly once with an anonymous login view', async () => {
  const { casesFor, registeredViews } = await import('./e2e/fixtures/f3/f3-ui-cases.mjs');
  const { PAGES, LANGS, THEMES, VIEWPORTS, KEY_PAGES, MATRIX_VIEWS } = await import('./e2e/fixtures/f3/f3-ui-helper.mjs');
  assert.deepEqual(PAGES.map(x => x), ['admin','capabilities','cli-auth','docs','events','index','keys','login','message','node','nodes','passkey-enroll','playbook','playbooks','receivers','security','source','sources','trace']);
  assert.deepEqual(LANGS.map(x => x), ['zh-CN','en','ja','es']);
  assert.deepEqual(THEMES.map(x => x), ['light','dark']);
  assert.deepEqual(Object.keys(VIEWPORTS), ['desktop','mobile']);
  assert.equal(VIEWPORTS.desktop.width, 1440); assert.equal(VIEWPORTS.mobile.width, 390);
  const cases = casesFor('ui_shift');
  assert.equal(new Set(cases.map(x => x.id)).size, cases.length, 'unique registered ids');
  const grid = cases.filter(x => x.kind === 'grid');
  const cells = grid.flatMap(c => c.pages.map(page => [page, c.lang, c.theme, c.viewport].join('|')));
  assert.equal(cells.length, MATRIX_VIEWS, '304 registered matrix views');
  assert.equal(new Set(cells).size, 304, 'no duplicated page/lang/theme/viewport cell');
  for (const lang of LANGS) for (const theme of THEMES) for (const viewport of Object.keys(VIEWPORTS)) {
    const slice = grid.filter(c => c.lang === lang && c.theme === theme && c.viewport === viewport);
    assert.deepEqual(slice.flatMap(c => c.pages).sort(), PAGES.slice().sort(), `complete page coverage for ${lang}/${theme}/${viewport}`);
  }
  assert.equal(registeredViews('ui_shift').matrix, 304);
  // login is a genuine guest form view in the matrix (no session), and never a duplicate of index.
  const loginViews = grid.filter(c => c.pages.includes('login')).length;
  assert.equal(loginViews, 16, 'login is registered in all sixteen lang/theme/viewport combinations');
  const shotsIds = cases.filter(c => c.kind === 'shots').map(c => c.id);
  assert.equal(new Set(shotsIds).size, shotsIds.length, 'one shot case per key page, no duplicate/index stand-in');
  assert.ok(!shotsIds.some(id => grid.some(c => c.id === id)), 'shot cases never reuse matrix ids');
  assert.deepEqual(cases.filter(c => c.kind === 'shots').map(c => c.page), KEY_PAGES.map(x => x));
  for (const c of cases.filter(c => c.kind === 'shots')) assert.equal(c.views, 4, 'each key page shot case covers 4 views');
  assert.ok(cases.some(c => c.id === 'shift-lang-hint') && cases.some(c => c.id === 'shift-scope-slot') && cases.some(c => c.id === 'shift-collapsed'));
  assert.ok(cases.some(c => c.id === 'interact-namespace-switch') && cases.some(c => c.id === 'interact-dialogs') && cases.some(c => c.id === 'guards-guest'));
});
test('F3b2b ux_regression_19_23 retains all five behaviours plus device PATCH, guards and the exact password form', async () => {
  const { casesFor, registeredViews } = await import('./e2e/fixtures/f3/f3-ui-cases.mjs');
  const cases = casesFor('ux_regression_19_23');
  const ids = cases.map(c => c.id);
  assert.equal(new Set(ids).size, ids.length);
  for (const id of ['ux19-member-admin','ux20-notification-denied','ux21-authme-identity','ux22-passkey-empty-enrollment','ux23-zero-devices-honest','retained-device-patch','retained-guards','retained-login-form'])
    assert.ok(ids.includes(id), 'registered retained case ' + id);
  assert.ok(cases.filter(c => c.id.startsWith('ux19') || c.id.startsWith('ux2')).every(c => c.views === 1));
  assert.ok(registeredViews('ux_regression_19_23').views >= 12);
  const source = readFileSync(path.join(ROOT, 'scripts/e2e/fixtures/f3/f3-ui-cases.mjs'), 'utf8')
    + readFileSync(path.join(ROOT, 'scripts/e2e/fixtures/f3/f3-ui-helper.mjs'), 'utf8');
  // Genuine wire/DOM facts, not "element exists" theatre.
  for (const needle of ['role, "member"', '/v1/devices/', 'enabled, false', 'f3-guest', 'login-panel', 'Notification.permission', 'auth/me'])
    assert.ok(source.includes(needle), 'case body keeps genuine fact: ' + needle);
  assert.equal(/"skip"|skipped:|test\.skip/.test(source), false, 'no skip path in the retained UX cases');
});
test('F3b2b aggregate rejects wrong authority/epoch/cwd/attempt, duplicate cells, missing views, deadlines and no-op greens', async () => {
  const { validateUiEvidence: validate } = await import('./e2e/fixtures/f3/f3-ui-runner.mjs');
  const expected = { suite:'ui_shift', id:'grid-en-dark-mobile-1', runId:'test', attemptId:'a', cwd:process.cwd(), sourceHash:'hash', mode:'explicit-mock', authority:'explicit-mock', views:2, pages:['admin','capabilities'], lang:'en', theme:'dark', viewport:'mobile' };
  const meta = Object.fromEntries(Object.entries(expected).filter(([k]) => !['views','pages','lang','theme','viewport'].includes(k)));
  const pages = expected.pages.map(page => ({ page, lang:'en', theme:'dark', viewport:'mobile', attempted:true, ok:true }));
  const result = { ...meta, ok:true, checks:2, report:{ errors:[], pages } };
  const exit = { ...meta, exitCode:0, signal:null, timedOut:false };
  validate(meta, result, exit, expected);
  for (const key of ['cwd','sourceHash','mode','attemptId','runId','authority']) assert.throws(() => validate({ ...meta, [key]: 'wrong' }, result, exit, expected), undefined, 'wrong ' + key + ' must fail');
  assert.throws(() => validate(meta, { ...result, checks:0 }, exit, expected), undefined, 'no-op green rejected');
  assert.throws(() => validate(meta, { ...result, report:{ errors:[], pages: pages.slice(0,1) } }, exit, expected), undefined, 'missing view rejected');
  assert.throws(() => validate(meta, { ...result, report:{ errors:[], pages:[...pages, pages[0]] } }, exit, expected), undefined, 'duplicate matrix cell rejected');
  assert.throws(() => validate(meta, { ...result, report:{ errors:['mock 未实现 501'], pages } }, exit, expected), undefined, 'unknown501/console error rejected');
  assert.throws(() => validate(meta, { ...result, ok:false, error:'boom' }, exit, expected), undefined, 'failed case rejected');
  assert.throws(() => validate(meta, result, { ...exit, exitCode:1 }, expected), undefined, 'nonzero child exit rejected');
  assert.throws(() => validate(meta, result, { ...exit, timedOut:true }, expected), undefined, 'deadline-truncated case rejected');
  assert.throws(() => validate(meta, result, { ...exit, signal:'SIGTERM' }, expected), undefined, 'signal-terminated case rejected');
  assert.throws(() => validate({ ...meta, cwd: '/tmp' }, result, exit, expected), undefined, 'wrong cwd cannot pass');
});

// AUTH1/DEV1: retained identity/device wire is NOT the resource Status domain.
const L1D_USER = { id: 'usr_mock_demo', username: 'demo', displayName: 'demo', role: 'admin', disabled: false, createdAt: 123 };
test('L1D AUTH1 cookie-only exact canonical six-field me and anonymous text/plain error', async () => {
  const { sandbox, req } = loadMock();
  const me = await req('/v1/auth/me');
  assert.equal(me.status, 200);
  assert.deepEqual(Object.keys(me.body).sort(), Object.keys(L1D_USER).sort());
  assert.equal(me.body.disabled, false); assert.equal(typeof me.body.createdAt, 'number');
  sandbox.document.cookie = 'session=alias; other=ignored';
  const anon = await sandbox.fetch('/v1/auth/me', { headers: { Authorization: 'Bearer mock-key' }, credentials: 'include' });
  assert.equal(anon.status, 401); assert.match(anon.headers.get('Content-Type'), /^text\/plain/);
  assert.equal(await anon.text(), '{"error":"未登录"}\n');
});
test('L1D AUTH1 password exact 400 401 429 error and RAW success cookie without aliases', async () => {
  const { sandbox, req } = loadMock(); const route='/v1/auth/password/login';
  await req('/v1/auth/logout', jsonInit('POST', {}));
  for (const body of [{}, { username:'demo', passwordAlias:'mock-only-password' }, { username:'demo', password:42 }]) {
    const r=await req(route,jsonInit('POST',body)); assert.equal(r.status,400); assert.deepEqual(Object.keys(r.body),['error']);
  }
  const bad=await req(route,jsonInit('POST',{username:'demo',password:' mock-only-password '}));
  assert.equal(bad.status,401); assert.deepEqual(Object.keys(bad.body),['error']);
  const good=await sandbox.fetch(route,jsonInit('POST',{username:' demo ',password:'mock-only-password'}));
  assert.equal(good.status,200); assert.deepEqual(plain(await good.json()),{ok:true});
  assert.match(good.headers.get('Set-Cookie'), /^anotify_session=[^;]+; Path=\/; HttpOnly; SameSite=Lax;/);
  assert.equal((await req('/v1/auth/me')).status,200);
  let limited;
  for(let n=0;n<10;n++) limited=await sandbox.fetch(route,jsonInit('POST',{username:'DEMO',password:'wrong'}));
  assert.equal(limited.status,429); assert.ok(Number(limited.headers.get('Retry-After'))>0);
  assert.deepEqual(plain(await limited.json()),{error:'请求过于频繁，请稍后再试'});
});
test('L1D DEV1 exact wrapped flat device PATCH null/omitted keep tags empty clear owner and persisted false', async () => {
  const { req, Mock }=loadMock();
  const device={id:'l1d-device',userId:'usr_mock_demo',name:'Owner',platform:'desktop',enabled:true,eventScope:'all',tags:['fixed'],endpoint:'https://push.invalid',p256dh:'fixed-key',auth:'fixed-auth',userAgent:'Chrome',createdAt:123};
  Mock.state.devices.push(device,{...device,id:'other-device',userId:'other-user'});
  const r=await req('/v1/devices/l1d-device',jsonInit('PATCH',{enabled:false,name:null,eventScope:null,tags:null}));
  assert.equal(r.status,200); assert.deepEqual(r.body,{ok:true,device:{...device,enabled:false}});
  assert.deepEqual((await req('/v1/devices')).body.devices.find(d=>d.id===device.id),r.body.device);
  const clear=await req('/v1/devices/l1d-device',jsonInit('PATCH',{name:'Renamed',eventScope:'final',tags:[]}));
  assert.equal(clear.body.device.enabled,false); assert.deepEqual(clear.body.device.tags,[]); assert.equal(clear.body.device.name,'Renamed');
  for(const id of ['missing','other-device']) { const bad=await req('/v1/devices/'+id,jsonInit('PATCH',{enabled:false})); assert.equal(bad.status,404); assert.deepEqual(Object.keys(bad.body),['error']); }
  await req('/v1/auth/logout',jsonInit('POST',{}));
  const guest=await req('/v1/devices/l1d-device',jsonInit('PATCH',{enabled:true})); assert.equal(guest.status,401); assert.deepEqual(guest.body,{error:'未登录'});
});
test('L1D AUTH1 startup only enabled canonical six fields and visible honest unhealthy error', async () => {
  const valid=await l1Boot({status:200,body:L1D_USER}); assert.equal(valid.location.href,'index.html');
  for(const body of [{...L1D_USER,disabled:true},{...L1D_USER,id:undefined},{...L1D_USER,createdAt:'123'},{...L1D_USER,authenticated:true}]) {
    const b=await l1Boot({status:200,body}); assert.equal(b.location.href,''); assert.ok(b.sandbox.document.getElementById('status-box').textContent.trim());
  }
  for(const response of [{status:503,body:{error:'unhealthy'}},'bad-json',new TypeError('Failed to fetch')]) {
    const b=await l1Boot(response); assert.equal(b.location.href,''); assert.ok(b.sandbox.document.getElementById('status-box').textContent.trim());
  }
  const anon=await l1Boot({status:401,body:{error:'未登录'}}); assert.equal(anon.location.href,''); assert.equal(anon.sandbox.document.getElementById('status-box').textContent,'');
});
test('L1D AUTH1 postJSON keeps exact identity error and status without resource message fallback', async () => {
  const source=readFileSync(path.join(ROOT,'web-src/pages/login.html'),'utf8');
  const fn=source.slice(source.indexOf('async function postJSON('),source.indexOf('// 登录成功后跳回'));
  for(const [status,body,message] of [[401,{error:'raw identity error'},'raw identity error'],[429,{error:'retry identity'},'retry identity'],[400,{kind:'Status',message:'wrong-domain'},'HTTP 400']]) {
    const ctx=vm.createContext({fetch:async()=>jsonResponse(body,status)}); vm.runInContext(fn,ctx);
    await assert.rejects(vm.runInContext("postJSON('/v1/auth/password/login',{})",ctx),e=>e.message===message&&e.status===status);
  }
});


// E1a: only loadErrorNode's decoded message leaf is raw; UI chrome stays translated.
test('E1a loadErrorNode retains the actual rejected Status.message in one independent raw span', async () => {
  const message = 'E1a unique Status sentinel: targets must be an array';
  const { A } = loadAnotify({ fetch: async () => jsonResponse({ kind: 'Status', message, reason: 'invalid_values' }, 400) });
  let error;
  try { await A.api('/v1/namespaces/prod/playbooks/e1a/render', jsonInit('POST', {})); } catch (e) { error = e; }
  assert.equal(error.status, 400); assert.equal(error.message, message);
  const box = A.loadErrorNode(error, () => {});
  const raw = box.children.filter(n => n.hasAttribute('data-raw-content'));
  assert.equal(raw.length, 1, 'decoded Status.message must not disappear behind a generic HTTP heading');
  assert.equal(raw[0].tagName, 'SPAN'); assert.equal(raw[0].textContent, message);
  assert.equal(box.hasAttribute('data-raw-content'), false);
  assert.equal(box.children[0].hasAttribute('data-raw-content'), false);
  assert.equal(box.children.at(-1).tagName, 'BUTTON');
});

test('E1a loadErrorNode HTML-like/special-character message is text only, never whole error reflection', () => {
  const { A } = loadAnotify();
  const message = '原文 <img src=x onerror="window.e1aInjected=1"> & < > " \' E1a';
  const error = { status: 400, message, url: 'private-url', stack: 'private-stack', params: 'private-params',
    get headers() { throw new Error('headers must not be inspected'); },
    toString() { throw new Error('whole error must not be serialized'); } };
  const box = A.loadErrorNode(error);
  const raw = box.children.find(n => n.hasAttribute('data-raw-content'));
  assert.ok(raw); assert.equal(raw.tagName, 'SPAN'); assert.equal(raw.textContent, message);
  assert.equal(raw.children.length, 0); assert.equal(raw.innerHTML, undefined);
  assert.equal(box.children[0].textContent.includes(message), false);
  assert.equal(box.children.map(n => n.textContent).join(' ').includes('private-'), false);
});

for (const lang of ['zh-CN', 'en', 'ja', 'es']) {
  test('E1a loadErrorNode four-language translated heading/retry retains raw sentinel: ' + lang, () => {
    const { A, ctx, sandbox } = loadAnotify();
    vm.runInContext(readFileSync(path.join(ROOT, 'web/i18n.' + lang + '.js'), 'utf8'), ctx);
    const dict = sandbox.AnotifyI18n;
    const message = 'E1a 中文 raw sentinel <b>原样</b> & "';
    const box = A.loadErrorNode({ status: 400, message }, () => {});
    assert.equal(box.children[0].textContent, dict['common.load_failed_prefix'] + dict['common.request_rejected'].replace('{code}', '400'));
    assert.equal(box.children[1].tagName, 'SPAN');
    assert.equal(box.children[1].hasAttribute('data-raw-content'), true);
    assert.equal(box.children[1].textContent, message);
    assert.equal(box.children[2].textContent, dict['common.retry']);
    for (const n of [box, box.children[0], box.children[2]]) assert.equal(n.hasAttribute('data-raw-content'), false);
    if (lang === 'en' || lang === 'es') assert.doesNotMatch(box.children[0].textContent + box.children[2].textContent, /[\u3400-\u9fff]/);
  });
}

test('E1a loadErrorNode preserves 401/network/5xx headings and accepts only message strings', () => {
  const { A } = loadAnotify();
  for (const [error, reason] of [[{ status: 401, message: 'identity error' }, '请求被拒绝 HTTP 401'], [{ status: 503, message: 'upstream unavailable' }, '服务端错误 HTTP 503'], [new TypeError('Failed to fetch'), '连不上服务端']]) {
    const box = A.loadErrorNode(error, () => {});
    assert.equal(box.children[0].textContent, '载入失败：' + reason);
    assert.equal(box.children[1].textContent, error.message);
    assert.equal(box.children[2].textContent, '重试');
  }
  for (const error of [{ status: 400 }, { status: 400, message: { raw: 'must not stringify' } }, { status: 400, message: 42 }]) {
    const box = A.loadErrorNode(error);
    assert.equal(box.children.filter(n => n.hasAttribute('data-raw-content')).length, 0);
    assert.equal(box.children.length, 1);
  }
});

// E1b regression: execute the real page controllers; no source-string PASS.
for (const page of ['message', 'trace']) test('E1b raw leaf controller preserves service sentinel safely: ' + page, async () => {
  const sentinel = 'E1b 中文 service <img src=x onerror="bad()"> & "原文"';
  const run = await b2Page(page, page === 'message' ? '?namespace=default&id=tsk_mock_2' : '?namespace=default&traceUID=trace_mock_t1', ({ Mock }) => {
    Mock.state.tasks.find(t => t.name === 'tsk_mock_2').result.summary = sentinel;
    Mock.state.tasks.find(t => t.name === 'tsk_mock_2').envDiagnostics = [{name:'TOKEN', source:'passthrough', value:'E1b-secret-never-visible'}];
  });
  const host = run.get(page === 'message' ? 'task-result' : 'trace-tasks');
  const leaves = b2Find(host, n => n.hasAttribute?.('data-raw-content'));
  assert.ok(leaves.some(n => n.textContent === sentinel), 'actual raw sentinel must have its own marked leaf');
  for (const n of leaves) { assert.equal(n.tagName, 'SPAN'); assert.equal(n.children.length, 0); }
  assert.equal(b2Find(host, n => n.tagName === 'IMG').length, 0, 'HTML-like raw string never parses');
  assert.equal(b2Text(run.get(page === 'message' ? 'msg-content' : 'trace-content')).includes('E1b-secret-never-visible'), false);
  if (page === 'message') {
    const pre = b2Find(run.get('task-params'), n => n.tagName === 'PRE')[0];
    assert.equal(pre.hasAttribute('data-raw-content'), false, 'never mark whole PRE');
    assert.equal(pre.children[0]?.tagName, 'SPAN');
    assert.equal(pre.children[0]?.hasAttribute('data-raw-content'), true);
    assert.equal(run.get('task-cancel').hasAttribute('data-raw-content'), false, 'controls not raw');
  }
});

const e1bEpoch = await import('./e2e/fixtures/f3/f3-epoch.mjs');
const e1bRaw = await import('./e2e/fixtures/f3/f3-raw-content.mjs');
test('E1b finite raw checker retains exact leaf and rejects adjacent Chinese chrome/nested widgets', () => {
  const root=fakeNode('div'), raw=fakeNode('span');raw.setAttribute('data-raw-content','');
  raw.textContent='E1b 中文 <img> & 原文';root.append(raw);
  const before=e1bRaw.scanRawChrome(root);
  assert.equal(before.raw[0].text,raw.textContent);assert.deepEqual(e1bRaw.chromeResidue(before,'en'),[]);
  const heading=fakeNode('h3');heading.textContent='故意中文 UI';root.append(heading);
  assert.equal(e1bRaw.chromeResidue(e1bRaw.scanRawChrome(root),'en').length,1,'Adjacent chrome cannot be hidden by raw sentinel');
  const button=fakeNode('button');button.textContent='嵌套按钮';raw.append(button);
  const nested=e1bRaw.scanRawChrome(root);assert.equal(nested.invalid.length,1);
  assert.ok(e1bRaw.chromeResidue(nested,'en').some(x=>x.text==='嵌套按钮'));
  const pre=fakeNode('pre');pre.setAttribute('data-raw-content','');pre.textContent='中文';
  assert.equal(e1bRaw.scanRawChrome(pre).invalid.length,1,'Whole pre markers rejected');
});
test('E1b full closure hashes actual HTML CSS locale generated/dist mutations, excludes logs/results (UNIT ONLY)', async () => {
  const fs=await import('node:fs/promises');const {randomUUID}=await import('node:crypto');
  const base=await e1bEpoch.captureEpoch(ROOT);
  const temp=path.join(ROOT,'scripts/e2e/fixtures/f3/E1B-logs/unit-'+randomUUID());
  await fs.mkdir(temp,{recursive:true});
  try {
    for(const file of base.files) {const dst=path.join(temp,file.path);await fs.mkdir(path.dirname(dst),{recursive:true});await fs.copyFile(path.join(ROOT,file.path),dst);}
    const initial=await e1bEpoch.captureEpoch(temp);assert.equal(initial.fullSourceArtifactSHA,base.fullSourceArtifactSHA);
    for(const file of ['web-src/pages/message.html','web/ui.css','web-src/locales/en.yaml','web/en/message.html','internal/server/dist/en/message.html']) {
      assert.ok(base.files.some(x=>x.path===file),'Actual artifact fixture must exist: '+file);
      const original=await fs.readFile(path.join(temp,file));await fs.appendFile(path.join(temp,file),'\nE1b UNIT byte mutation');
      const mutated=await e1bEpoch.captureEpoch(temp);assert.notEqual(mutated.fullSourceArtifactSHA,initial.fullSourceArtifactSHA,file);
      assert.throws(()=>e1bEpoch.assertFrozen(initial,mutated));await fs.writeFile(path.join(temp,file),original);
    }
    for(const file of ['web/results/unit.json','web/unit.log','scripts/e2e/fixtures/f3/E1B-logs/unit.log']) {const dst=path.join(temp,file);await fs.mkdir(path.dirname(dst),{recursive:true});await fs.writeFile(dst,'UNIT record, not browser PASS');}
    assert.equal((await e1bEpoch.captureEpoch(temp)).fullSourceArtifactSHA,initial.fullSourceArtifactSHA);
    assert.equal(base.files.filter(x=>/^scripts\/e2e\/suites\/(frontend|i18n|i18n_coverage|ui_shift|ux_regression_19_23)\.mjs$/.test(x.path)).length,5);
    assert.deepEqual(base.presentSpecs.concat(base.missingSpecs).sort(),['agent','auth','events','ingress','resource'].map(x=>'api/'+x+'.yaml').sort());
  } finally {await fs.rm(temp,{recursive:true,force:true});}
});
// UNIT ONLY: mutate copied runtime helpers, never live source or run receipts.
for (const helper of ['parent-geometry-probe.mjs', 'parent-repair-evidence.mjs']) {
  test('PARENT-EPOCH UNIT: runtime helper changes both hashes: ' + helper, async () => {
    const fs = await import('node:fs/promises');
    const base = await e1bEpoch.captureEpoch(ROOT);
    const temp = path.join(ROOT, 'scripts/e2e/fixtures/f3/PARENT-EPOCH-logs/unit-' + globalThis.crypto.randomUUID());
    const relative = 'scripts/e2e/fixtures/f3/' + helper;
    await fs.mkdir(temp, { recursive: true });
    try {
      // Include both real helpers even on the old collector, to expose omissions.
      for (const file of new Set([...base.files.map(x => x.path), ...['parent-geometry-probe.mjs', 'parent-repair-evidence.mjs'].map(x => 'scripts/e2e/fixtures/f3/' + x)])) {
        const dst = path.join(temp, file);
        await fs.mkdir(path.dirname(dst), { recursive: true });
        await fs.copyFile(path.join(ROOT, file), dst);
      }
      const before = await e1bEpoch.captureEpoch(temp);
      assert.equal(before.sourceCodeSHA, base.sourceCodeSHA);
      assert.equal(before.fullSourceArtifactSHA, base.fullSourceArtifactSHA);
      await fs.appendFile(path.join(temp, relative), '\n// UNIT ONLY runtime helper byte mutation\n');
      const after = await e1bEpoch.captureEpoch(temp);
      assert.notEqual(after.sourceCodeSHA, before.sourceCodeSHA, relative + ' changes sourceCodeSHA');
      assert.notEqual(after.fullSourceArtifactSHA, before.fullSourceArtifactSHA, relative + ' changes FULL SHA');
      assert.throws(() => e1bEpoch.assertFrozen(before, after), /Source code changed within run/);
      for (const collection of ['sourceFiles', 'files']) {
        assert.deepEqual(after[collection].map(x => x.path), before[collection].map(x => x.path));
        assert.deepEqual(after[collection].filter(x => x.path !== relative), before[collection].filter(x => x.path !== relative), 'Every other real input remains covered and unchanged');
        assert.notDeepEqual(after[collection].find(x => x.path === relative), before[collection].find(x => x.path === relative));
      }
      const report = path.join(temp, 'scripts/e2e/fixtures/f3/PARENT-EPOCH-CLOSURE.md');
      await fs.writeFile(report, 'UNIT ONLY Markdown evidence is not source');
      const withReport = await e1bEpoch.captureEpoch(temp);
      assert.equal(withReport.sourceCodeSHA, after.sourceCodeSHA);
      assert.equal(withReport.fullSourceArtifactSHA, after.fullSourceArtifactSHA);
    } finally { await fs.rm(temp, { recursive: true, force: true }); }
  });
}

test('E1b immutable exactly-once attempt validator: UNIT fake records never browser PASS', () => {
  const id={suite:'frontend',id:'one',runId:'UNIT-only',attemptId:'a',cwd:ROOT,mode:'explicit-mock',authority:'explicit-mock',sourceHash:'sha',sourceCodeSHA:'code',fullSourceArtifactSHA:'sha',artifactEpoch:'sha'};
  const result={...id,ok:true,checks:1,report:{errors:[]}}, exit={...id,exitCode:0,signal:null,timedOut:false};
  const manifest={registered:['one'],attempts:[id]};
  e1bEpoch.validateAttempt(id,result,exit,id);e1bEpoch.validateManifest(manifest,['one'],[id],[exit]);
  assert.throws(()=>e1bEpoch.validateManifest(manifest,['one'],[id,{...id,attemptId:'failed-before-green'}],[exit]),/Missing\/extra/);
  assert.throws(()=>e1bEpoch.validateManifest(manifest,['one'],[],[exit]));
  assert.throws(()=>e1bEpoch.validateManifest(manifest,['one'],[id],[exit,exit]));
  for(const key of ['suite','id','runId','attemptId','cwd','mode','authority','sourceHash','sourceCodeSHA','fullSourceArtifactSHA','artifactEpoch']) {
    assert.throws(()=>e1bEpoch.validateAttempt({...id,[key]:'WRONG'},result,exit,id),key);
    const missing={...exit};delete missing[key];assert.throws(()=>e1bEpoch.validateAttempt(id,result,missing,id),key+' missing');
  }
  for(const change of [{exitCode:1},{signal:'SIGTERM'},{timedOut:true},{exitCode:null}])assert.throws(()=>e1bEpoch.validateAttempt(id,result,{...exit,...change},id));
  for(const change of [{ok:false},{checks:0},{report:{errors:['501 network failure']}}])assert.throws(()=>e1bEpoch.validateAttempt(id,{...result,...change},exit,id));
  assert.throws(()=>e1bEpoch.validateAttempt(id,result,null,id));
});
test('E1b all 35 ACTUAL frontend registered IDs exactly once: UNIT records, not browser PASS', async () => {
  const {CASES}=await import('./e2e/fixtures/f3/f3-cases.mjs');
  assert.equal(CASES.length,35);
  const attempts=CASES.map(({id},i)=>({suite:'frontend',id,runId:'UNIT-all35',attemptId:'a'+i,cwd:ROOT,mode:'explicit-mock',authority:'explicit-mock',sourceHash:'unitSHA',sourceCodeSHA:'unitCode',fullSourceArtifactSHA:'unitSHA',artifactEpoch:'unitSHA'}));
  const exits=attempts.map(x=>({...x,exitCode:0,signal:null,timedOut:false}));
  const registered=CASES.map(x=>x.id),manifest={registered,attempts};
  e1bEpoch.validateManifest(manifest,registered,attempts,exits);
  for(let i=0;i<attempts.length;i++)e1bEpoch.validateAttempt(attempts[i],{...attempts[i],ok:true,checks:1,report:{errors:[]}},exits[i],attempts[i]);
  assert.throws(()=>e1bEpoch.validateManifest(manifest,registered,[...attempts,{...attempts[0],attemptId:'OLD-FAILED-ATTEMPT'}],exits));
  assert.throws(()=>e1bEpoch.validateManifest(manifest,registered,attempts,exits.slice(1)));
});

// S1: execute the actual mount, including delayed auth/me, not a source-order regex.
test('S1 sidebar DOM mount keeps primary nav before the same role-gated admin section', async () => {
  for (const role of ['admin', 'member', 'guest']) {
    let resolveMe;
    const me = new Promise(resolve => { resolveMe = resolve; });
    const run = loadAnotify({ fetch: async url => url === '/v1/auth/me'
      ? me : jsonResponse({ apiVersion: 'anotify/v1', kind: 'NamespaceList', items: [] }) });
    const doc = run.sandbox.document, ids = new Map(), timers = [];
    function element(tag) {
      const n = fakeNode(tag), set = n.setAttribute;
      n.setAttribute = (key, value) => {
        set(key, value);
        if (key === 'id') { n.id = String(value); ids.set(n.id, n); }
        if (key === 'class') n.className = String(value);
      };
      const classes = () => new Set(n.className.split(/\s+/).filter(Boolean));
      n.classList = {
        contains: c => classes().has(c),
        add(...cs) { n.className = [...new Set([...classes(), ...cs])].join(' '); },
        remove(...cs) { n.className = [...classes()].filter(c => !cs.includes(c)).join(' '); },
        toggle(c, force) { const on = force ?? !classes().has(c); this[on ? 'add' : 'remove'](c); return on; },
      };
      n.append = (...children) => { for (const child of children) if (child != null) { n.children.push(child); if (typeof child === 'object') child.parentElement = n; } };
      n.appendChild = child => { n.append(child); return child; };
      return n;
    }
    doc.createElement = element; doc.createElementNS = (_, tag) => element(tag);
    doc.createDocumentFragment = () => element('fragment');
    const main = element('main'); main.setAttribute('id', 'page-main');
    doc.body.appendChild(main);
    doc.getElementById = id => ids.get(id) || null;
    run.sandbox.addEventListener = () => {};
    run.sandbox.removeEventListener = () => {};
    run.sandbox.URLSearchParams = URLSearchParams;
    run.sandbox.IntersectionObserver = class { observe() {} disconnect() {} };
    run.sandbox.setInterval = fn => { const id = setInterval(fn, 100000); timers.push(id); return id; };
    try {
      run.A.mountLayout({ active: 'admin' });
      const section = ids.get('admin-nav-section'), sidebar = ids.get('sidebar');
      assert.ok(sidebar && section, 'actual mount must create both nodes');
      const column = sidebar.children[0];
      const nav = column.children.find(n => n.tagName === 'NAV');
      assert.ok(nav, 'actual primary nav exists');
      assert.equal(column.children[1], ids.get('scope-slot'));
      assert.ok(column.children[2] === nav, 'primary nav precedes late privileged section');
      assert.equal(column.children[3], section, 'existing admin section stays before footer');
      assert.ok(column.children[4].className.includes('app-sidebar-foot'));
      assert.equal(section.classList.contains('hidden'), true, 'no privileged entry before identity');
      resolveMe(role === 'guest' ? jsonResponse({ error: 'unauthorized' }, 401)
        : jsonResponse({ id: role, username: role, role }));
      for (let i = 0; i < 8; i++) await new Promise(setImmediate);
      assert.equal(ids.get('admin-nav-section'), section, 'auth retains the same section');
      assert.equal(column.children[2], nav, 'auth retains the same primary nav');
      const links = section.children.filter(n => n.tagName === 'A');
      assert.equal(section.classList.contains('hidden'), role !== 'admin');
      assert.equal(links.length, role === 'admin' ? 1 : 0);
      if (role === 'admin') {
        assert.equal(links[0].getAttribute('href'), 'admin.html');
        assert.equal(links[0].getAttribute('aria-current'), 'page');
        assert.ok(links[0].className.includes('active'));
      }
    } finally { for (const id of timers) clearInterval(id); }
  }
});

test('S1 namespace switcher marks only DTO text leaves, not loading or empty UI, in four languages', async () => {
  for (const lang of ['zh-CN', 'en', 'ja', 'es']) for (const populated of [true, false]) {
    let resolveNamespaces;
    const pending = new Promise(resolve => { resolveNamespaces = resolve; });
    const run = loadAnotify({ fetch: async url => url === '/v1/namespaces' ? pending : jsonResponse({ error: 'unauthorized' }, 401) });
    const doc = run.sandbox.document, ids = new Map(), timers = [];
    doc.documentElement.lang = lang;
    doc.createElement = tag => {
      const n = fakeNode(tag), set = n.setAttribute;
      n.setAttribute = (k, v) => { set(k, v); if (k === 'id') ids.set(String(v), n); if (k === 'class') n.className = String(v); };
      return n;
    };
    doc.createElementNS = (_, tag) => doc.createElement(tag);
    const main = doc.createElement('main'); main.setAttribute('id', 'page-main'); doc.body.append(main);
    doc.getElementById = id => ids.get(id) || null;
    run.sandbox.addEventListener = () => {}; run.sandbox.removeEventListener = () => {};
    run.sandbox.IntersectionObserver = class { observe() {} disconnect() {} };
    run.sandbox.setInterval = fn => { const id = setInterval(fn, 100000); timers.push(id); return id; };
    try {
      run.A.mountLayout({ active: 'index' });
      const slot = ids.get('scope-slot');
      assert.equal(b2Find(slot, n => n.hasAttribute?.('data-raw-content')).length, 0, 'loading has no stale raw marker');
      const displayName = '实际命名空间 <img src=x onerror="bad()"> & 原值';
      resolveNamespaces(jsonResponse({ apiVersion: 'anotify/v1', kind: 'NamespaceList', items: populated
        ? [{ metadata: { name: 'default', displayName } }] : [] }));
      for (let i = 0; i < 8; i++) await new Promise(setImmediate);
      const raw = b2Find(slot, n => n.hasAttribute?.('data-raw-content'));
      if (!populated) { assert.equal(raw.length, 0, 'empty fallback is UI, never DTO raw'); continue; }
      assert.equal(raw.length, 3, 'only trigger data plus menu displayName/name leaves');
      assert.ok(raw.every(n => n.tagName === 'SPAN' && n.children.length === 0));
      assert.deepEqual(raw.map(n => n.textContent), [`${displayName} · default`, displayName, 'default']);
      const controls = b2Find(slot, n => n.tagName === 'BUTTON' || n.getAttribute?.('role') === 'menu');
      assert.ok(controls.every(n => !n.hasAttribute('data-raw-content')), 'controls never blanket-exempt chrome');
    } finally { for (const id of timers) clearInterval(id); }
  }
});


// FINAL-R1: execute the actual API and registered case bodies, not source needles.
test('FINALR1 P1 ordinary PATCH archives original per-version privacy and immutable execution facts', async () => {
  const { req, Mock } = loadMock(); await pbCreate(req, g1VersionFiles());
  const installed = await pbAction(req, 'install', { project: 'a', version: 17 }); assert.equal(installed.status, 200);
  const old = plain(Mock.state.capabilities.find(c => c.name === 'a-review'));
  const current = (await req('/v1/namespaces/default/capabilities/a-review')).body;
  // 内容变更的合法路径是 POST 新版本（省略 metadata.version = 内容寻址）；
  // PATCH 带 spec 会被 400 immutable_field 拒掉，永远不会换版本。
  const patched = await req('/v1/namespaces/default/capabilities/a-review', { method: 'PATCH', body: JSON.stringify({
    metadata: { resourceVersion: current.metadata.resourceVersion }, spec: { run: { ...current.spec.run, files: { ...current.spec.run.files, 'run.sh': '#!/bin/sh\necho upgraded\n' } } }
  }) }); assert.equal(patched.status, 400); assert.equal(patched.body.reason, 'immutable_field');
  const upgraded = await req('/v1/namespaces/default/capabilities', { method: 'POST', body: JSON.stringify({
    apiVersion: 'anotify/v1', kind: 'Capability', metadata: { name: 'a-review' },
    spec: { ...current.spec, run: { ...current.spec.run, files: { ...current.spec.run.files, 'run.sh': '#!/bin/sh\necho upgraded\n' } } }
  }) }); assert.equal(upgraded.status, 201);
  const live = Mock.state.capabilities.find(c => c.name === 'a-review'), archived = live.versions.find(v => v.version === 17);
  assert.equal(live.uid, old.uid); assert.equal(live.version, 18); assert.notEqual(live.digest, old.digest);
  assert.deepEqual(plain(archived.spec), old.spec); assert.equal(archived.digest, old.digest);
  assert.deepEqual(plain(archived.receiptPolicy), old.receiptPolicy, 'ordinary PATCH must preserve original archived privacy');
  assert.equal(archived.receiptRef, old.receiptRef); assert.equal(archived.installedFrom, old.installedFrom);
  assert.notStrictEqual(archived.receiptPolicy, live.receiptPolicy, 'per-version policy is deep-copied');
  const history = (await req(G1_PB + '/installs')).body;
  assert.ok(history.installs.find(i => i.valuesDigest === installed.body.valuesDigest).capabilities.includes('***'));
  assert.equal(JSON.stringify(history).includes('a-review@17'), false);
  assert.equal(JSON.stringify((await req(G1_PB)).body.status).includes('a-review@17'), false);
  live.receiptPolicy.properties.metadata.properties.version.writeOnly = false;
  assert.equal(archived.receiptPolicy.properties.metadata.properties.version.writeOnly, true);
});
test('FINALR1 P2 reused Source keeps original private identity despite new literal template and public schema', async () => {
  const { req, Mock } = loadMock(); const actualName = 'original-private-source';
  const files = playbookFiles({ edit: d => {
    d['playbook.yaml'].spec.values.properties.sourceName = { type: 'string', writeOnly: true };
    d['sources/on-push.yaml'].metadata.name = '{{values.sourceName}}';
    d['sources/on-push.yaml'].spec.enabled = false;
  } }); await pbCreate(req, files);
  const first = await pbAction(req, 'install', { project: 'a', sourceName: actualName }); assert.equal(first.status, 200);
  const original = plain(Mock.state.sources.find(s => s.name === actualName));
  const literal = pbRawEdit(files, d => {
    delete d['playbook.yaml'].spec.values.properties.sourceName;
    d['sources/on-push.yaml'].metadata.name = actualName;
    d['sources/on-push.yaml'].spec.targets[0].params.env = 'stage';
  }); assert.equal((await pbReplace(req, (await req(G1_PB)).body, literal)).status, 200);
  const reused = await pbAction(req, 'install', { project: 'a' }); assert.equal(reused.status, 200);
  assert.deepEqual(reused.body.skipped.sources, ['***'], 'reuse must consult the EXISTING Source privacy');
  const u = reused.body.unapplied.find(u => u.kind === 'source'); assert.ok(u);
  assert.equal(u.ref, '***'); assert.equal(u.field, 'targets'); assert.equal(u.reason, 'targets_unapplied');
  assert.equal(u.declared[0].params.env, 'stage'); assert.equal(u.actual[0].params.env, 'prod');
  assert.equal(reused.body.notes.find(n => n.code === 'source_disabled').ref, '***');
  assert.equal(reused.body.notes.find(n => n.code === 'targets_unapplied').ref, '***');
  assert.deepEqual(plain(Mock.state.sources.find(s => s.name === actualName)), original, 'no autoapply/config/targets/UID mutation');
  for (const route of [G1_PB + '/installs', G1_PB + '/installs/' + reused.body.install.id])
    assert.equal(JSON.stringify((await req(route)).body).includes(actualName), false, 'history/detail retain original identity privacy');
  assert.equal((await req(G1_PB)).body.status.blocker.action, 'edit_source');
});
function finalR1CaseBody(name, next, sandbox) {
  const src = process.env.FINALR1_CASE_BASELINE === '9022863'
    ? execFileSync('git', ['show','9022863:.pi-orchestrator/v4/teams/frontend/f3-ui-cases.mjs'], { cwd:ROOT, encoding:'utf8', timeout:5000 })
    : readFileSync(path.join(ROOT, 'scripts/e2e/fixtures/f3/f3-ui-cases.mjs'), 'utf8');
  const body = src.slice(src.indexOf('async function ' + name + '('), src.indexOf('async function ' + next + '('));
  const ctx = vm.createContext({ assert: { ...assert, deepEqual: (a, b, message) => assert.deepEqual(plain(a), plain(b), message) }, ...sandbox });
  return vm.runInContext(body + '\n' + name, ctx);
}
test('FINALR1 U7 registered UX19 rejects unexpected admin GET even after archive clears the live calls', async () => {
  // Explicit mutation provider: the extra privileged GET answers 200, so
  // absence of writes / backend errors cannot accidentally prove its absence.
  const run = loadMock({ mockSrc: MOCK_SRC + `
const finalR1Fetch = window.fetch; window.fetch = (p,o) => p === '/v1/admin/finalr1-extra-get' ? Promise.resolve(new Response(JSON.stringify({ extraGet:true }), {status:200,headers:{'Content-Type':'application/json'}})) : finalR1Fetch(p,o);` });
  run.sandbox.Response = Response;
  let calls = [], member = false;
  const main = { innerText: 'Forbidden not an admin', querySelector: () => ({}) };
  const doc = { getElementById: id => id === 'page-main' ? main : null };
  const d = { mock: true, base: 'http://fixture', report: { requests: [] },
    goto: async () => {}, req: async p => member ? { role: 'member' } : (await run.req(p)).body,
    calls: async () => calls, archive: async () => { d.report.requests.push(...calls); calls = []; }, finish: async () => {},
    page: { goto: async () => { const r = await run.req('/v1/admin/finalr1-extra-get'); assert.equal(r.status,200,'mutation provider is an actual 200 extra GET'); calls.push({ url: '/v1/admin/finalr1-extra-get', method: 'GET', httpStatus: r.status }); },
      waitForFunction: async () => {}, evaluate: async fn => vm.runInNewContext('(' + fn + ')()', { document: doc, window: { Anotify: { me: { role: 'member' } } } }) } };
  const body = finalR1CaseBody('ux19', 'ux20', { withDriver: async (id, opt, fn) => fn(d, { n: 0, inc() {} }),
    greetMember: async () => { member = true; }, writeWrites: async () => calls.filter(c => c.method !== 'GET'), ok: () => ({ ok: true }) });
  await assert.rejects(body({ id: 'mutation-admin-get' }), /no privileged admin API/);
});
test('FINALR1 m1 registered UX22 rejects a no-op button despite the preexisting positive empty-list copy', async () => {
  const list = { innerText: 'Add on this device', textContent: 'Add on this device' };
  const document = { getElementById: id => id === 'passkey-list' ? list : null, querySelectorAll: sel => sel === 'button' ? [list] : [] };
  const control = { filter() { return this; }, first() { return this; }, count: async () => 1, click: async () => {} };
  const d = { base: 'http://fixture', calls: async () => [], finish: async () => {}, page: {
    goto: async () => {}, waitForFunction: async () => {}, waitForTimeout: async () => {}, url: () => 'http://fixture/security.html', locator: () => control, on() {}, off() {},
    evaluate: async fn => vm.runInNewContext('(' + fn + ')()', { document }) } };
  const body = finalR1CaseBody('ux22', 'ux23', { withDriver: async (id, opt, fn) => fn(d, { n: 0, inc() {} }), ok: () => ({ ok: true }) });
  await assert.rejects(body({ id: 'mutation-noop' }), /answered, never silent/);
});

test('FINALR1 m2 actual frontend aggregate retains completed nonzero red without calling collection a pass', async () => {
  const epoch = await import('./e2e/fixtures/f3/f3-epoch.mjs');
  const source = readFileSync(path.join(ROOT, 'scripts/e2e/suites/frontend.mjs'), 'utf8');
  const meta = { suite: 'frontend', id: 'unit-completed-red', cwd: ROOT, runId: 'unit-finalr1-red', mode: 'explicit-mock', authority: 'explicit-mock', attemptId: 'actual-red-attempt', sourceHash: 'unit-hash', sourceCodeSHA: 'unit-code', fullSourceArtifactSHA: 'unit-hash', artifactEpoch: 'unit-artifact' };
  const evidence = { ...meta, ok: false, checks: 0, error: 'real child behavior failure', report: { errors: ['mock 未实现'] }, reportPath: '/UNIT/report.json' };
  const exit = { ...meta, exitCode: 1, signal: null, timedOut: false };
  const frozen = { sourceCodeSHA: meta.sourceCodeSHA, fullSourceArtifactSHA: meta.fullSourceArtifactSHA, artifactEpoch: meta.artifactEpoch };
  let aggregate; const failures = [];
  const ctx = vm.createContext({ assert, frozen, ROOT, cwd: ROOT, mode: meta.mode, runId: meta.runId, runDir: '/UNIT', plan: { registered: [meta.id], attempts: [meta] }, CASES: [{ id: meta.id }],
    identity: () => meta, captureEpoch: async () => frozen, assertFrozen: epoch.assertFrozen, epochFields: epoch.epochFields, validateManifest: epoch.validateManifest, validateAttempt: epoch.validateAttempt,
    evidenceDirectory: () => '/UNIT', readEnrollments: async () => [meta],
    readFile: async file => JSON.stringify(file.endsWith('case.json') ? evidence : exit),
    writeFile: async (file, text) => { aggregate = JSON.parse(text); }, path,
    H: { bad: (id, message) => failures.push({ id, message }), ok: () => assert.fail('red cannot pass') } });
  const start = source.includes('function validateCompletion(') ? source.indexOf('function validateCompletion(') : source.indexOf('async function aggregate(');
  const fn = vm.runInContext(source.slice(start, source.indexOf('\nH.startTimer();', start)) + '\naggregate', ctx);
  await fn(meta.sourceHash, [exit]);
  assert.equal(aggregate.complete, true, 'all valid completed records collected even when child failed');
  assert.equal(aggregate.ok, false); assert.equal(aggregate.failed, 1); assert.equal(aggregate.passed, 0);
  assert.equal(aggregate.collected.length, 1); assert.equal(aggregate.collected[0].exitCode, 1);
  assert.equal(aggregate.collected[0].attemptId, meta.attemptId); assert.equal(failures.length, 1);
});

test('FINALR1 U1 actual auth provider returns guest401 then restores original authenticated identity without resource reset', async () => {
  const run = loadMock(); const original = (await run.req('/v1/auth/me')).body;
  run.sandbox.assert = assert;
  const storage = new Map(); run.sandbox.sessionStorage = { getItem: k => storage.get(k) ?? null, setItem: (k,v) => storage.set(k,String(v)), removeItem: k => storage.delete(k) };
  const source = readFileSync(path.join(ROOT, 'scripts/e2e/fixtures/f3/f3-ui-helper.mjs'), 'utf8');
  vm.runInContext(source.slice(source.indexOf('const requireOrigin'), source.indexOf('// One real navigation')).replaceAll('export ', '') + '\nglobalThis.authFlagUnderTest = authFlag;', run.ctx);
  const d = { mock: true, page: { url: () => 'http://fixture/index.html', evaluate: async (fn,arg) => vm.runInContext('(' + fn + ')(' + JSON.stringify(arg) + ')', run.ctx) } };
  await run.sandbox.greetGuest(d);
  run.Mock.state.auth.loggedOut = true; run.sandbox.sessionStorage.setItem('anotify.mock.loggedOut', '1'); run.sandbox.document.cookie = '';
  assert.equal((await run.req('/v1/auth/me')).status, 401);
  const nodeUid = (await run.req('/v1/nodes')).body.items[0].metadata.uid;
  await run.sandbox.authFlagUnderTest(d, false, false);
  const restored = await run.req('/v1/auth/me'); assert.equal(restored.status, 200); assert.deepEqual(restored.body, original);
  assert.equal((await run.req('/v1/nodes')).body.items[0].metadata.uid, nodeUid, 'identity restore must not reset protected resource facts');
});
test('FINALR1 U2 actual helper fixture creates canonical Playbook and GET content for all sixteen URLs', async () => {
  const run = loadMock(); const source = readFileSync(path.join(ROOT, 'scripts/e2e/fixtures/f3/f3-ui-helper.mjs'), 'utf8');
  const ctx = vm.createContext({ assert, URLSearchParams });
  const code = source.slice(source.indexOf('export async function fixtures'), source.indexOf('export function viewUrl')).replaceAll('export ', '');
  vm.runInContext(code + '\nglobalThis.fixtureUnderTest = fixtures; globalThis.queryUnderTest = query;', ctx);
  const d = { goto: async () => {}, req: async p => (await run.req(p)).body, response: async (p,b,m) => run.req(p, { method:m, body:JSON.stringify(b) }) };
  const fixture = await ctx.fixtureUnderTest(d); assert.ok(fixture.playbook);
  const detail = await run.req('/v1/namespaces/default/playbooks/' + fixture.playbook + '?includeBody=true');
  assert.equal(detail.status, 200); assert.equal(detail.body.metadata.name, fixture.playbook); assert.ok(detail.body.spec.files['playbook.yaml']);
  for (const lang of ['zh-CN','en','ja','es']) for (const theme of ['light','dark']) for (const viewport of ['desktop','mobile'])
    assert.equal(new URLSearchParams(ctx.queryUnderTest('playbook', fixture, false)).get('id'), detail.body.metadata.name, lang+theme+viewport);
  assert.equal((await ctx.fixtureUnderTest(d)).playbook, fixture.playbook, 'existing canonical package is reused');
  assert.equal((await run.req('/v1/namespaces/default/playbooks')).body.items.length, 1);
  assert.equal(new URLSearchParams(ctx.queryUnderTest('playbook', {}, false)).has('id'), false, 'missing-id negative remains distinct');
});
test('FINALR1 U3 actual me.id owner PATCH wrapped200 GETfalse and foreign404 retain strict DEV1 domain', async () => {
  const run = loadMock(), me = (await run.req('/v1/auth/me')).body;
  const own = { id:'finalr1-own-device', userId:me.id, name:'Own receiver', platform:'web', enabled:true, eventScope:'all', tags:[] };
  const foreign = { ...own, id:'finalr1-foreign-device', userId:'other-owner' };
  run.Mock.state.devices = [own, foreign];
  const patched = await run.req('/v1/devices/' + own.id, { method:'PATCH', body:JSON.stringify({enabled:false}) });
  assert.equal(patched.status,200); assert.equal(patched.body.device.id,own.id); assert.equal(patched.body.device.enabled,false);
  assert.equal((await run.req('/v1/devices')).body.devices.find(d=>d.id===own.id).enabled,false);
  const rejected = await run.req('/v1/devices/' + foreign.id, { method:'PATCH', body:JSON.stringify({enabled:false}) });
  assert.equal(rejected.status,404); assert.equal(foreign.enabled,true);
});

// A finite native delete case with one exact response-boundary mutation.
for (const mutation of [false, true]) {
  test('PARENT-EPOCH native: deleted installs group ' + (mutation ? 'rejects exact GET 200 mutant' : '404 keeps exact receipt 200'), { timeout: 55000 }, () => {
    const code = `
      import assert from 'node:assert/strict';
      import { openDriver, withPreviewResources } from './scripts/e2e/fixtures/f3/f3-driver.mjs';
      import { playbook } from './scripts/e2e/fixtures/f3/f3-playbook.mjs';
      const controller = new AbortController();
      const deadline = setTimeout(() => controller.abort(), 50000);
      try {
        await withPreviewResources(async () => {
          const d = await openDriver('epoch-delete');
          try {
            const response = d.response;
            d.response = async (url, body, method = 'GET') => {
              const result = await response(url, body, method);
              if (url === '/v1/namespaces/default/playbooks/project-chain/installs' && method === 'GET') {
                assert.equal(result.status, 404, 'Narrow mutation starts from an actual deleted-parent 404');
                console.log('EPOCH EXACT GET ' + url + ' nativeStatus=' + result.status);
                if (process.env.PARENT_EPOCH_GROUP_MUTANT === '1') return { ...result, status: 200 };
              }
              return result;
            };
            await playbook(d, 'delete');
            await d.finish();
            console.log('EPOCH DELETE PASS');
          } finally { await d.stop(); }
        }, controller.signal);
        assert.equal(controller.signal.aborted, false, 'Finite deadline cannot become success');
      } catch (error) { console.error(error.stack || error); process.exitCode = 1; }
      finally { clearTimeout(deadline); }
    `;
    let output, rejected;
    try {
      output = execFileSync(process.execPath, ['--input-type=module', '-e', code], {
        cwd: ROOT, timeout: 52000, encoding: 'utf8', maxBuffer: 4 * 1024 * 1024, stdio: ['ignore', 'pipe', 'pipe'],
        env: { ...process.env, E2E_MOCK_ONLY: '1', PARENT_EPOCH_GROUP_MUTANT: mutation ? '1' : '0', F3_RUN_ID: 'parent-epoch-delete-' + globalThis.crypto.randomUUID() },
      });
    } catch (error) { rejected = error; }
    console.log(output || rejected?.stdout || '');
    if (rejected) console.log(rejected.stderr);
    if (mutation) {
      assert.equal(rejected?.status, 1, 'Exact group-200 mutant must fail the actual case');
      assert.equal(rejected.signal, null, 'No timeout/signal substitutes for the exact 404 assertion');
      assert.match(rejected.stderr, /Deleted parent group GET \/v1\/namespaces\/default\/playbooks\/project-chain\/installs must return 404/);
      assert.match(rejected.stderr, /200 !== 404/);
      assert.match(rejected.stdout, /EPOCH EXACT GET .*\/installs nativeStatus=404/);
      console.log('EPOCH DELETE MUTANT COMMAND_EXIT=1');
    } else {
      assert.equal(rejected, undefined, 'Native positive must complete without product/driver failure');
      assert.match(output, /EPOCH EXACT GET .*\/installs nativeStatus=404/);
      assert.match(output, /EPOCH DELETE PASS/);
      console.log('EPOCH DELETE POSITIVE COMMAND_EXIT=0');
    }
  });
}

// Parent full-gate regressions execute native controllers and the unchanged
// text-node chrome scanner, not source needles or invented response adapters.
for (const [mode, claim] of [
  ["observe", "fault survives same-case navigation and native retry recovers"],
  ["playbook", "manifest deletion keeps exact receipt/resources/execution references"],
  ["data", "five raw-data leaves stay safe in four locales without exempting chrome"],
]) {
  test("PARENT-GATE: " + claim, { timeout: 55000 }, () => {
    const output = execFileSync(process.execPath, [
      path.join(ROOT, "scripts/e2e/fixtures/f3/parent-repair-evidence.mjs"), mode,
    ], {
      cwd: ROOT, timeout: 52000, encoding: "utf8", maxBuffer: 4 * 1024 * 1024,
      env: { ...process.env, E2E_MOCK_ONLY: "1", PARENT_REPAIR_LANG: "", F3_RUN_ID: "parent-node-" + mode + "-" + globalThis.crypto.randomUUID() },
    });
    assert.match(output, new RegExp("PARENT REPAIR PASS " + mode));
  });
}

for (const id of ["shift-prelayout-expanded", "shift-prelayout-collapsed", "interact-namespace-switch"]) {
  test("PARENT-GEOMETRY: native " + id, { timeout: 55000 }, () => {
    const output = execFileSync(process.execPath, [
      path.join(ROOT, "scripts/e2e/fixtures/f3/parent-repair-evidence.mjs"), "geometry",
    ], {
      cwd: ROOT, timeout: 52000, encoding: "utf8", maxBuffer: 4 * 1024 * 1024,
      env: { ...process.env, E2E_MOCK_ONLY: "1", PARENT_GEOMETRY_CASE: id, PARENT_GEOMETRY_MUTATION: "", F3_RUN_ID: "parent-geometry-node-" + id + "-" + globalThis.crypto.randomUUID() },
    });
    assert.match(output, /PARENT REPAIR PASS geometry/);
  });
}

for (const [mutation, id, rejection] of [
  ["pre-offset", "shift-prelayout-expanded", /parent geometry: premount sidebar reservation/],
  ["outer-left", "shift-prelayout-expanded", /parent geometry: mounted outer boundary/],
  ["page-x", "shift-prelayout-expanded", /parent geometry: same-document visual page x/],
  ["slot-zero-recovered", "interact-namespace-switch", /parent geometry: first-mounted slot reservation >=40/],
  ["slot-missing-recovered", "interact-namespace-switch", /parent geometry: first-mounted slot reservation >=40/],
]) {
  test("PARENT-GEOMETRY-MUTANT: native rejects " + mutation, { timeout: 55000 }, () => {
    let rejected;
    try {
      execFileSync(process.execPath, [
        path.join(ROOT, "scripts/e2e/fixtures/f3/parent-repair-evidence.mjs"), "geometry",
      ], {
        cwd: ROOT, timeout: 52000, encoding: "utf8", maxBuffer: 4 * 1024 * 1024, stdio: ["ignore", "pipe", "pipe"],
        env: { ...process.env, E2E_MOCK_ONLY: "1", PARENT_GEOMETRY_CASE: id, PARENT_GEOMETRY_MUTATION: mutation, F3_RUN_ID: "parent-geometry-mutant-" + mutation + "-" + globalThis.crypto.randomUUID() },
      });
    } catch (error) { rejected = error; }
    assert.equal(rejected?.status, 1, "The actual native mutant must fail, not hang or pass");
    assert.equal(rejected.signal, null, "No timeout/signal may stand in for the intended geometry failure");
    assert.match(rejected.stderr, rejection, "Only the exact observed boundary/first-mounted failure is accepted");
    console.log("PARENT GEOMETRY MUTANT " + mutation + " COMMAND_EXIT=" + rejected.status);
    console.log(rejected.stdout);
    console.log(rejected.stderr);
  });
}

// A native controller failure, not a fake JSON record. Fault injection is owned
// only by the new diagnostic context; original UI assertions/budgets stay intact.
for (const synthetic of [true, false]) {
  test('TRACE-ARCHIVE: native ' + (synthetic ? 'caught render failure' : 'seven-task contrast'), { timeout: 55000 }, () => {
    const code = `
      import assert from 'node:assert/strict';
      import { openDriver } from './scripts/e2e/fixtures/f3/f3-driver.mjs';
      import { fixtures, gotoPage, auditPage, ANCHORS } from './scripts/e2e/fixtures/f3/f3-ui-helper.mjs';
      const d = await openDriver('trace-archive-${synthetic ? 'synthetic' : 'contrast'}', { lang: 'es', theme: 'light' });
      try {
        ${synthetic ? `await d.context.route(/\\/graphs(?:\\.[^/]+)?\\.js(?:\\?.*)?$/, async route => {
          const response = await route.fetch();
          await route.fulfill({ response, body: (await response.text()) + '\\nwindow.AnotifyGraphs = {...window.AnotifyGraphs, taskChain() { throw new TypeError("safeSyntheticRenderError"); }};' });
        });` : ''}
        async function nativeTraceCase() {
          const f = await fixtures(d);
          for (const page of ['security', 'source', 'sources', 'trace']) {
            const from = d.report.errors.length;
            await gotoPage(d, page, f, 'es');
            await auditPage(d, { page, lang: 'es', theme: 'light', viewport: 'desktop', anchor: ANCHORS[page] }, from);
          }
          assert.equal(await d.page.locator('#trace-tasks [data-task]').count(), 7);
          // Archive the Trace document before another document clears its calls.
          await d.goto('index');
          await d.finish();
        }
        await nativeTraceCase();
      } finally {
        await d.stop();
        console.log('TRACE EVIDENCE ' + JSON.stringify({ pageErrors: d.report.errors, reportPath: d.report.reportPath }));
      }
    `;
    let rejected, output;
    try { output = execFileSync(process.execPath, ['--input-type=module', '-e', code], {
      cwd: ROOT, timeout: 52000, encoding: 'utf8', maxBuffer: 4 * 1024 * 1024,
      stdio: ['ignore', 'pipe', 'pipe'],
      env: { ...process.env, E2E_MOCK_ONLY: '1', F3_RUN_ID: 'trace-archive-' + globalThis.crypto.randomUUID(), F3_ATTEMPT_ID: '' },
    }); } catch (error) { rejected = error; output = error.stdout; }
    console.log('TRACE ARCHIVE NATIVE ' + (synthetic ? 'SYNTHETIC' : 'CONTRAST') + ' COMMAND_EXIT=' + (rejected?.status ?? 0));
    console.log(output);
    if (rejected) console.log(rejected.stderr);
    if (synthetic) {
      assert.equal(rejected?.status, 1, 'Native anchor assertion must really fail');
      assert.equal(rejected.signal, null, 'A timeout cannot substitute for a render failure');
      assert.match(rejected.stderr, /structural anchor #trace-tasks present/);
    } else assert.equal(rejected, undefined, 'Normal same-case contrast must pass');
    const saved = JSON.parse(output.split('\n').find(s => s.startsWith('TRACE EVIDENCE ')).slice(15));
    const persisted = JSON.parse(readFileSync(saved.reportPath, 'utf8'));
    assert.ok(Array.isArray(persisted.traceDocuments), 'Evidence must reach the real driver report before close');
    const traceDocs = persisted.traceDocuments.filter(d => /\/trace\.html(?:\?|$)/.test(d.snapshot.actualDocumentURL));
    assert.equal(traceDocs.length, 1, 'One archived native Trace document');
    const doc = traceDocs[0];
    for (const d of persisted.traceDocuments) {
      assert.equal(d.documentId, d.snapshot.documentId);
      assert.ok(d.responses.every(r => r.documentId === d.documentId), 'Do not bind another document response');
    }
    assert.match(doc.snapshot.actualDocumentURL, /\/es\/trace\.html\?namespace=default&traceUID=trace_mock_t1$/);
    assert.equal(doc.documentId, doc.snapshot.documentId);
    assert.equal(doc.snapshot.currentNamespace, 'default');
    assert.equal(doc.snapshot.documentLang, 'es');
    assert.equal(doc.snapshot.readyState, 'complete');
    assert.equal(doc.snapshot.busy, false);
    assert.equal(doc.snapshot.anchorCount, synthetic ? 0 : 1);
    assert.equal(doc.snapshot.taskRows, synthetic ? 0 : 7);
    assert.deepEqual(saved.pageErrors, [], 'Caught render error is not a global pageerror');
    const response = doc.responses.at(-1);
    assert.equal(response.documentId, doc.documentId);
    assert.equal(response.httpStatus, 200);
    assert.equal(response.response.kind, 'Trace');
    assert.equal(response.response.tasks.count, 7);
    assert.equal(response.response.tasks.items[0].metadata.name, 'tsk_mock_1');
    assert.ok(response.parsedAtMs <= response.returnAtMs);
    assert.ok(response.returnAtMs <= doc.snapshot.atMs);
    if (synthetic) {
      const caught = doc.events.find(e => e.event === 'loadErrorNode:input');
      assert.equal(caught.error.name, 'TypeError');
      assert.equal(caught.error.message, 'safeSyntheticRenderError');
      assert.ok(caught.error.stack.some(s => s.includes('taskChain')));
      assert.ok(caught.state.busy, 'Observe catch before finally removes busy');
      assert.equal(caught.state.anchorCount, 0, 'Catch has cleared content');
      assert.match(doc.snapshot.errorText, /safeSyntheticRenderError/);
      assert.ok(doc.events.some(e => e.event === 'taskChain:throw-before-cleanup' && e.state.anchorCount === 1));
      assert.ok(doc.events.some(e => e.event === 'mutation-delivery' && e.state.busy === false));
    } else {
      assert.equal(doc.snapshot.errorText, '');
      assert.equal(doc.events.some(e => e.error), false);
      const next = persisted.traceDocuments.at(-1);
      assert.notEqual(next.documentId, doc.documentId, 'Subsequent index has its own Trace GET, never the old document');
      assert.match(next.snapshot.actualDocumentURL, /\/index\.html\?namespace=default$/);
      assert.equal(next.snapshot.taskRows, 0, 'Do not copy the old Trace body into index evidence');
    }
    console.log('TRACE ARCHIVE SAVED ' + JSON.stringify({ reportPath: saved.reportPath, documentId: doc.documentId, snapshot: doc.snapshot, response: { actualURL: response.actualURL, namespace: response.namespace, uid: response.uid, httpStatus: response.httpStatus, kind: response.response.kind, taskIDs: response.response.tasks.items.map(t => t.metadata.name), parseStartedAtMs: response.parseStartedAtMs, parsedAtMs: response.parsedAtMs, returnAtMs: response.returnAtMs }, caught: doc.events.filter(e => e.event === 'loadErrorNode:input') }));
  });
}

test('TRACE-ARCHIVE: selected response structure excludes all private payloads', async () => {
  const { traceEvidenceTools } = await import('./e2e/fixtures/f3/f3-trace-evidence.mjs');
  const T = traceEvidenceTools(), sentinel = 'TRACE_PRIVATE_SENTINEL_728f';
  const task = { kind: 'Task', metadata: { uid: 'public-uid', name: 'public-task', namespace: 'default', resourceVersion: 1 }, spec: { traceUID: 'public-trace', parentTaskId: 'parent-task', capability: { name: sentinel, version: 2 }, env: { X: sentinel }, vars: { X: sentinel }, params: { X: sentinel }, payload: sentinel, token: sentinel }, status: { phase: 'failed', result: { summary: sentinel, error: { code: sentinel, message: sentinel } } } };
  const selected = T.shape({ kind: 'Trace', traceUID: 'public-trace', tasks: [task], events: [], edges: [], pruned: [{ detail: sentinel }], env: sentinel, vars: sentinel, token: sentinel, password: sentinel, hmac: sentinel, payload: sentinel });
  assert.equal(JSON.stringify(selected).includes(sentinel), false);
  assert.equal(selected.tasks.items[0].metadata.uid, 'public-uid');
  assert.equal(selected.tasks.items[0].spec.capability.version, 2);
  assert.equal(selected.tasks.items[0].status.errorTypes.message, 'string');
  assert.deepEqual(T.apiPath('/v1/namespaces/team%20a/traces/tr%2F1?token=' + sentinel, 'http://fixture/es/trace.html'), { actualURL: 'http://fixture/v1/namespaces/team%20a/traces/tr%2F1', namespace: 'team a', uid: 'tr/1' });
  for (const p of ['/v1/traces/id', '/v1/namespaces/default/traces', '/v1/namespaces/default/traces/id/extra', '/v1/namespaces/default/tasks/id', 'http://external/v1/namespaces/default/traces/id']) assert.equal(T.apiPath(p, 'http://fixture/'), null);
  const safe = T.error(new TypeError('token="' + sentinel + '" password=' + sentinel + ' hmac=' + sentinel + ' Bearer ' + sentinel));
  assert.equal(JSON.stringify(safe).includes(sentinel), false);
  assert.equal(safe.name, 'TypeError');
});

test('TRACE-ARCHIVE: observer preserves original return Promise, this, arguments and thrown error', async () => {
  const { traceEvidenceTools, installTraceEvidence } = await import('./e2e/fixtures/f3/f3-trace-evidence.mjs');
  const promise = Promise.resolve('native-return'), originalError = new TypeError('safeSyntheticRenderError');
  const args = [{ kind: 'Task' }, { tasks: [] }];
  let receiver, received;
  const graphs = { taskChain(...values) { receiver = this; received = values; return promise; } };
  const A = { model: { task() { throw originalError; } }, loadErrorNode() { return promise; }, readScope() { return { namespace: 'default' }; } };
  const doc = { readyState: 'complete', documentElement: { lang: 'es' }, querySelector: () => null, querySelectorAll: () => [], getElementById: () => null, addEventListener() {} };
  const win = { Anotify: A, AnotifyGraphs: graphs, addEventListener() {} };
  const context = vm.createContext({ window: win, document: doc, navigator: { language: 'es' }, location: new URL('http://fixture/es/trace.html?namespace=default'), URL, URLSearchParams, performance: { timeOrigin: 123, now: () => 7 }, MutationObserver: class { observe() {} } });
  vm.runInContext('(' + installTraceEvidence.toString() + ')(' + traceEvidenceTools.toString() + ');', context);
  assert.equal(graphs.taskChain(...args), promise);
  assert.equal(receiver, graphs);
  assert.deepEqual(received, args);
  assert.throws(() => A.model.task(args[0]), e => e === originalError);
  assert.equal(A.loadErrorNode(originalError), promise);
  const scene = win.__f3TraceSnapshot();
  assert.ok(scene.events.some(e => e.event === 'task:throw-before-cleanup'));
  assert.ok(scene.events.some(e => e.event === 'loadErrorNode:input'));
});
