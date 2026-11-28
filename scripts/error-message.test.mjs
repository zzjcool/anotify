#!/usr/bin/env node

import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const PARTIALS_PATH = "web/partials.js";
const PARTIALS_SRC = fs.readFileSync(PARTIALS_PATH, "utf8");

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
		getBoundingClientRect: () => ({ top: 0, left: 0, right: 0, bottom: 0, width: 0, height: 0 }),
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
		location: { pathname: "/security.html", search: "", hash: "" },
		navigator: { languages: ["zh-CN"], language: "zh-CN" },
		matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
		document: fakeDocument(),
		CustomEvent: class {
			constructor(type, init) {
				this.type = type;
				Object.assign(this, init || {});
			}
		},
		addEventListener() {},
		removeEventListener() {},
		dispatchEvent: () => true,
		getComputedStyle: () => ({ getPropertyValue: () => "" }),
	};
}

function loadAnotify(src = PARTIALS_SRC) {
	const ctx = vm.createContext(makeSandbox());
	vm.runInContext("globalThis.window = globalThis; globalThis.self = globalThis;", ctx);
	vm.runInContext(src, ctx, { filename: PARTIALS_PATH });
	return ctx.Anotify;
}

function fakeResponse(status, body, { jsonThrows = false } = {}) {
	let read = false;
	return {
		status,
		reads: () => read,
		async json() {
			if (read) throw new Error("body already consumed");
			read = true;
			if (jsonThrows) throw new SyntaxError("Unexpected token < in JSON");
			return body;
		},
	};
	}

const Anotify = loadAnotify();

test("#587 v4 Status 信封：取 message", async () => {
	const res = fakeResponse(400, { kind: "Status", status: "Failure", code: 400, reason: "bad_request", message: "当前密码不正确" });
	assert.equal(await Anotify.errorMessage(res), "当前密码不正确");
});

test("#587 认证域扁平体：取 error", async () => {
	const cases = [
		[409, "至少保留一种登录方式：请先设置密码，再删除最后一个 Passkey。"],
		[400, "当前密码不正确"],
		[400, "密码至少 8 个字符"],
		[404, "会话不存在"],
	];
	for (const [status, error] of cases) {
		assert.equal(await Anotify.errorMessage(fakeResponse(status, { error })), error, `HTTP ${status} 的 {error} 必须原样透出`);
	}
});

test("#587 两种字段都有时 message 优先", async () => {
	const res = fakeResponse(400, { message: "Status 的话", error: "扁平的话" });
	assert.equal(await Anotify.errorMessage(res), "Status 的话");
});

test("#587 非 JSON / 空体：退回 HTTP <status>", async () => {
	assert.equal(await Anotify.errorMessage(fakeResponse(502, null, { jsonThrows: true })), "HTTP 502");
	assert.equal(await Anotify.errorMessage(fakeResponse(500, null)), "HTTP 500");
	assert.equal(await Anotify.errorMessage(fakeResponse(400, { message: "", error: "" })), "HTTP 400");
	assert.equal(await Anotify.errorMessage(fakeResponse(400, "plain text")), "HTTP 400");
	assert.equal(await Anotify.errorMessage({ status: 0 }), "HTTP 0", "网络不可达（没有 res.json）也要有话说");
});

test("#587 响应体只读一次", async () => {
	const res = fakeResponse(400, { error: "读一次就好" });
	assert.equal(await Anotify.errorMessage(res), "读一次就好");
	assert.equal(res.reads(), true);
	await assert.rejects(() => res.json(), /already consumed/);
});
