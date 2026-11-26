import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import vm from "node:vm";
import test from "node:test";
import assert from "node:assert/strict";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const PARTIALS_SRC = readFileSync(path.join(ROOT, "web/partials.js"), "utf8");

let DOC = null;

function makeNode(tag = "div") {
	const attrs = new Map();
	const children = [];
	const node = {
		nodeType: 1,
		tagName: String(tag).toUpperCase(),
		parentElement: null,
		_text: "",
		_classes: new Set(),
		dataset: {},
		style: { setProperty() {}, removeProperty() {}, getPropertyValue: () => "" },
		focused: 0,
		scrolled: 0,
		get id() {
			return attrs.has("id") ? attrs.get("id") : "";
		},
		set id(value) {
			attrs.set("id", String(value));
		},
		get className() {
			return [...node._classes].join(" ");
		},
		set className(value) {
			node._classes = new Set(String(value || "").split(/\s+/).filter(Boolean));
		},
		get classList() {
			return {
				add: (name) => node._classes.add(name),
				remove: (name) => node._classes.delete(name),
				contains: (name) => node._classes.has(name),
				toggle: (name, on) => {
					const want = on === undefined ? !node._classes.has(name) : !!on;
					if (want) node._classes.add(name);
					else node._classes.delete(name);
					return want;
				},
			};
		},
		get children() {
			return children;
		},
		get textContent() {
			return node._text + children.map((child) => child.textContent).join("");
		},
		set textContent(value) {
			children.length = 0;
			node._text = value == null ? "" : String(value);
		},
		append(...items) {
			for (const item of items) {
				if (item == null) continue;
				if (item.nodeType === 1) item.parentElement = node;
				children.push(item);
			}
			return node;
		},
		appendChild(item) {
			node.append(item);
			return item;
		},
		prepend(...items) {
			for (const item of items) {
				if (item == null) continue;
				if (item.nodeType === 1) item.parentElement = node;
				children.unshift(item);
			}
		},
		replaceChildren(...items) {
			children.length = 0;
			node._text = "";
			node.append(...items);
		},
		after(sibling) {
			const parent = node.parentElement;
			if (!parent) return;
			parent.insertAfter(node, sibling);
		},
		insertAfter(anchor, sibling) {
			const at = children.indexOf(anchor);
			if (at < 0) return;
			if (sibling && sibling.nodeType === 1) sibling.parentElement = node;
			children.splice(at + 1, 0, sibling);
		},
		remove() {
			const parent = node.parentElement;
			if (!parent) return;
			const at = parent.children.indexOf(node);
			if (at >= 0) parent.children.splice(at, 1);
			node.parentElement = null;
		},
		setAttribute(name, value) {
			attrs.set(name, String(value));
		},
		getAttribute(name) {
			return attrs.has(name) ? attrs.get(name) : null;
		},
		hasAttribute: (name) => attrs.has(name),
		removeAttribute(name) {
			attrs.delete(name);
		},
		addEventListener() {},
		removeEventListener() {},
		dispatchEvent: () => true,
		contains(other) {
			for (let at = other; at; at = at.parentElement) if (at === node) return true;
			return false;
		},
		closest(selector) {
			for (let at = node; at; at = at.parentElement) {
				if (at.nodeType === 1 && matches(at, selector)) return at;
			}
			return null;
		},
		focus() {
			node.focused += 1;
			if (DOC) DOC.activeElement = node;
		},
		scrollIntoView() {
			node.scrolled += 1;
		},
		querySelector: (selector) => query(node, selector)[0] || null,
		querySelectorAll: (selector) => query(node, selector),
	};
	return node;
}

function makeDoc() {
	const doc = {
		documentElement: makeNode("html"),
		head: makeNode("head"),
		body: makeNode("body"),
		cookie: "",
		activeElement: null,
		createElement: (tag) => makeNode(tag),
		createElementNS: (_ns, tag) => makeNode(tag),
		createTextNode: (text) => ({ nodeType: 3, textContent: String(text) }),
		getElementById: (id) => query(doc.body, "#" + id)[0] || null,
		querySelector: (selector) => query(doc.body, selector)[0] || null,
		querySelectorAll: (selector) => query(doc.body, selector),
		addEventListener() {},
		removeEventListener() {},
		dispatchEvent: () => true,
		fonts: { ready: Promise.resolve() },
	};
	doc.documentElement.lang = "zh-CN";
	return doc;
}

function parseCompound(text) {
	const parts = { tag: "", id: "", classes: [], attrs: [] };
	const rest = text.replace(/^([a-zA-Z][a-zA-Z0-9-]*)/, (_m, tag) => {
		parts.tag = tag.toLowerCase();
		return "";
	});
	const re = /#([\w-]+)|\.([\w-]+)|\[([\w-]+)(?:=(?:"([^"]*)"|'([^']*)'|([^\]]*)))?\]/g;
	let m;
	while ((m = re.exec(rest))) {
		if (m[1]) parts.id = m[1];
		else if (m[2]) parts.classes.push(m[2]);
		else if (m[3])
			parts.attrs.push({
				name: m[3],
				value: m[4] !== undefined ? m[4] : m[5] !== undefined ? m[5] : m[6],
			});
	}
	return parts;
}

function matches(node, selector) {
	const compound = parseCompound(String(selector).trim());
	if (compound.tag && node.tagName !== compound.tag.toUpperCase()) return false;
	if (compound.id && node.id !== compound.id) return false;
	const classes = new Set(node.className.split(/\s+/).filter(Boolean));
	for (const name of compound.classes) if (!classes.has(name)) return false;
	for (const attr of compound.attrs) {
		const value = node.getAttribute(attr.name);
		if (value == null) return false;
		if (attr.value !== undefined && value !== attr.value) return false;
	}
	return true;
}

function query(root, selector) {
	const chain = String(selector).trim().split(/\s+/);
	const out = [];
	const walk = (node, depth) => {
		for (const child of node.children || []) {
			if (child.nodeType !== 1) continue;
			if (matches(child, chain[depth])) {
				if (depth === chain.length - 1) out.push(child);
				else walk(child, depth + 1);
			}
			walk(child, depth);
		}
	};
	walk(root, 0);
	return out;
}

function loadAnotify(src = PARTIALS_SRC) {
	DOC = makeDoc();
	const store = new Map();
	const sandbox = {
		console,
		setTimeout,
		clearTimeout,
		setInterval,
		clearInterval,
		requestAnimationFrame: (fn) => setTimeout(fn, 0),
		cancelAnimationFrame: (id) => clearTimeout(id),
		fetch: async () => {
			throw new Error("单元测试不发请求");
		},
		localStorage: {
			getItem: (k) => (store.has(k) ? store.get(k) : null),
			setItem: (k, v) => store.set(k, String(v)),
			removeItem: (k) => store.delete(k),
			clear: () => store.clear(),
		},
		location: { pathname: "/sources.html", search: "", hash: "" },
		navigator: { languages: ["zh-CN"], language: "zh-CN" },
		matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
		document: DOC,
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
	const ctx = vm.createContext(sandbox);
	vm.runInContext("globalThis.window = globalThis; globalThis.self = globalThis;", ctx);
	vm.runInContext(src, ctx, { filename: "web/partials.js" });
	return ctx.Anotify;
}

function mountField(doc, { id, label, hint }) {
	const field = doc.createElement("div");
	field.className = "field";
	const text = doc.createElement("label");
	text.className = "field-label";
	text.setAttribute("for", id);
	text.textContent = label;
	const control = doc.createElement("input");
	control.className = "input";
	control.setAttribute("id", id);
	field.append(text, control);
	if (hint) {
		const own = doc.createElement("p");
		own.className = "field-error hidden";
		own.setAttribute("id", id + "-error");
		own.textContent = hint;
		field.append(own);
	}
	doc.body.append(field);
	return { field, control };
}

function causesError(causes) {
	return { status: 400, reason: "invalid_source_config", message: "invalid source config", details: { causes } };
}

const causeReportItems = (box) => box.querySelectorAll(".cause-report-item");

test("applyCauses: 映射到的控件拿到 aria-invalid + 红框 + 服务端原文 message", () => {
	const A = loadAnotify();
	const { control } = mountField(DOC, { id: "f-poll-url", label: "接口地址" });
	const err = causesError([{ field: "spec.config.url", reason: "value_invalid", message: "必须是字符串" }]);
	const res = A.applyCauses(DOC.body, err, { "config.url": "f-poll-url" });
	assert.equal(res.causes.length, 1);
	assert.equal(res.fields.length, 1, "命中的 cause 必须进 fields");
	assert.equal(res.rest.length, 0, "命中的 cause 不许同时落 rest");
	assert.equal(control.getAttribute("aria-invalid"), "true", "控件必须 aria-invalid=true");
	assert.equal(control.classList.contains("is-invalid"), true, "控件必须带红框类");
	const hint = DOC.body.querySelector('[data-cause-for="f-poll-url"]');
	assert.ok(hint, "控件后面必须出现一行 cause 提示");
	assert.equal(hint.textContent, "必须是字符串", "提示必须逐字等于服务端 message");
	assert.equal(res.fields[0].label, "接口地址", "清单里的人话名取 label[for]");
});

test("applyCauses: 前缀命中就能标（config.headers.X → config.headers，targets[0].capability → targets）", () => {
	const A = loadAnotify();
	const { control: headers } = mountField(DOC, { id: "f-poll-headers", label: "请求头" });
	const { control: targets } = mountField(DOC, { id: "f-targets", label: "出口能力" });
	const err = causesError([
		{ field: "spec.config.headers.X-Api-Key", reason: "value_invalid", message: "头名不合法" },
		{ field: "spec.targets[0].capability", reason: "value_invalid", message: "不能为空" },
	]);
	const res = A.applyCauses(DOC.body, err, { "config.headers": "f-poll-headers", targets: "f-targets" });
	assert.equal(res.fields.length, 2, "两条都必须命中（前缀/下标都算命中）");
	assert.equal(headers.getAttribute("aria-invalid"), "true", "config.headers.X 必须标到 config.headers 的控件");
	assert.equal(targets.getAttribute("aria-invalid"), "true", "targets[0].capability 必须标到 targets 的控件");
	assert.equal(res.rest.length, 0, "前缀命中不是 rest");
});

test("applyCauses: 多个键都能命中时取最长的那个（config.schedule.expr 不该退到 config.schedule）", () => {
	const A = loadAnotify();
	const { control: hour } = mountField(DOC, { id: "f-expr", label: "排期" });
	const { control: tz } = mountField(DOC, { id: "f-tz", label: "时区" });
	const err = causesError([{ field: "spec.config.schedule.tz", reason: "value_invalid", message: "未知时区" }]);
	const res = A.applyCauses(DOC.body, err, {
		"config.schedule": "f-expr",
		"config.schedule.tz": "f-tz",
	});
	assert.equal(res.fields[0].label, "时区", "更长的键必须赢");
	assert.equal(tz.getAttribute("aria-invalid"), "true");
	assert.equal(hour.getAttribute("aria-invalid"), null, "短键不该被顺带标红");
});

test("applyCauses: 同一个控件上的多条 cause 合成一行提示，清单里仍是两条", () => {
	const A = loadAnotify();
	const { control } = mountField(DOC, { id: "f-poll-url", label: "接口地址" });
	const err = causesError([
		{ field: "spec.config.url", reason: "value_invalid", message: "必须是字符串" },
		{ field: "spec.config.url", reason: "value_invalid", message: "必须是 http/https" },
	]);
	const res = A.applyCauses(DOC.body, err, { "config.url": "f-poll-url" });
	assert.equal(res.fields.length, 2);
	const hints = DOC.body.querySelectorAll('[data-cause-for="f-poll-url"]');
	assert.equal(hints.length, 1, "同一个控件只允许一行提示");
	assert.equal(hints[0].textContent, "必须是字符串；必须是 http/https");
	assert.equal(control.getAttribute("aria-invalid"), "true");
});

test("causeReport: 命中不到控件的 cause 落 rest[]，清单里画成不可点的一行（不是按钮）", () => {
	const A = loadAnotify();
	const { control } = mountField(DOC, { id: "cap-params", label: "参数契约" });
	const err = causesError([
		{ field: "spec.params.properties.project.pattern", reason: "param_pattern_invalid", message: "pattern 不是合法正则" },
		{ field: "spec.run.entrypoint", reason: "bad_request", message: "run.entrypoint 不在文件清单里" },
	]);
	const res = A.applyCauses(DOC.body, err, { params: "cap-params" });
	assert.equal(res.fields.length, 1);
	assert.equal(res.rest.length, 1, "命不中的 cause 必须留在 rest[]");
	assert.equal(res.rest[0].label, "run.entrypoint", "rest 那行的字段名是去掉 spec. 的路径");
	const box = A.causeReport(err, res);
	const rows = causeReportItems(box);
	assert.equal(rows.length, 2, "清单一条 cause 一行，命中的和没命中的都在里面");
	const hitRow = rows.find((row) => row.getAttribute("data-cause-jump") === "cap-params");
	const restRow = rows.find((row) => row.getAttribute("data-cause-jump") === null);
	assert.ok(hitRow, "命中的那行必须是可点的跳转行");
	assert.equal(hitRow.tagName, "BUTTON", "跳转行是 button");
	assert.ok(restRow, "命不中的那行必须留在清单里（不能静默丢掉）");
	assert.equal(restRow.tagName, "P", "命不中的行不可点：是 p 不是 button");
	assert.equal(restRow.textContent, "run.entrypoint：run.entrypoint 不在文件清单里");
	assert.equal(control.getAttribute("aria-invalid"), "true");
});

test("causeReport: 标题跟动作走（preview → 试运行失败 / 默认 → 保存失败），条数逐字入标题", () => {
	const A = loadAnotify();
	const err = causesError([
		{ field: "spec.config.url", reason: "value_invalid", message: "a" },
		{ field: "spec.config.schedule", reason: "value_invalid", message: "b" },
	]);
	const save = A.causeReport(err, null, { action: "save" });
	const preview = A.causeReport(err, null, { action: "preview" });
	const plain = A.causeReport(err);
	assert.equal(save.querySelector(".cause-report-title").textContent, "保存失败：2 处需要修改");
	assert.equal(preview.querySelector(".cause-report-title").textContent, "试运行失败：2 处需要修改");
	assert.equal(plain.querySelector(".cause-report-title").textContent, "保存失败：2 处需要修改", "不传 action 时的默认标题");
});

test("causeReport: message 原样上屏，前端不再剪 `create source: ` 前缀（剪裁是后端的事）", () => {
	const A = loadAnotify();
	const message = "create source: invalid source config: config.url 必须是字符串";
	const err = causesError([{ field: "spec.config.url", reason: "value_invalid", message }]);
	const box = A.causeReport(err, null);
	const row = causeReportItems(box)[0];
	assert.equal(row.textContent, "config.url：" + message, "上屏文案必须逐字等于服务端 message，不再二次剪裁");
	const raw = box.querySelector('[data-raw-content]').textContent;
	assert.ok(raw.includes(message), "折叠里的 JSON 原文保留服务端 message");
	assert.ok(raw.includes("invalid_source_config"), "折叠里的 JSON 原文保留 reason 码");
});

test("接线：试运行分支交 action=preview、保存分支交 action=save（同一个 error()/causeReport，两套标题）", () => {
	const src = readFileSync(path.join(ROOT, "web/source-editor.js"), "utf8");
	const saveAt = src.indexOf('$("modal-save").onclick');
	const dryAt = src.indexOf('$("btn-dry").onclick');
	const reloadAt = src.indexOf('$("reload-draft").onclick');
	assert.ok(saveAt > 0 && dryAt > saveAt && reloadAt > dryAt, "source-editor.js 的三个动作入口还在吗？结构变了就改这份断言");
	const save = src.slice(saveAt, dryAt);
	const dry = src.slice(dryAt, reloadAt);
	assert.ok(save.includes('error($("form-result"),err,"save")'), "保存失败必须把 action=save 交给 error()");
	assert.ok(save.includes('A.causeTitle("save",causes.length)'), "保存失败的 toast 必须用 save 标题");
	assert.ok(dry.includes('error($("form-result"),err,"preview")'), "试运行失败必须把 action=preview 交给 error()");
	assert.ok(dry.includes('A.causeTitle("preview",causes.length)'), "试运行失败的 toast 必须用 preview 标题");
	assert.equal(dry.includes('A.causeTitle("save"'), false, "试运行分支不许再用保存标题");
	assert.equal(dry.includes('error($("form-result"),err,"save")'), false, "试运行分支不许再传 save");
});

test("clearCauses: 收掉上一轮的红框/aria-invalid/新建提示，页面原有的 hint 原样还原", () => {
	const A = loadAnotify();
	const { control } = mountField(DOC, { id: "f-poll-url", label: "接口地址", hint: "原有提示" });
	const err = causesError([{ field: "spec.config.url", reason: "value_invalid", message: "必须是字符串" }]);
	A.applyCauses(DOC.body, err, { "config.url": "f-poll-url" });
	const own = DOC.body.querySelector('[id="f-poll-url-error"]');
	assert.equal(own.textContent, "必须是字符串");
	assert.equal(own.classList.contains("hidden"), false);
	A.clearCauses(DOC.body);
	assert.equal(control.getAttribute("aria-invalid"), null, "清场后不许还挂着 aria-invalid");
	assert.equal(control.classList.contains("is-invalid"), false, "清场后不许还是红框");
	assert.equal(own.textContent, "原有提示", "页面原有的 hint 文案必须还原");
	assert.equal(own.classList.contains("hidden"), true, "页面原有的 hint 必须回到隐藏态");
	assert.equal(DOC.body.querySelectorAll("[data-cause-for]").length, 0, "新建的提示必须被移除");
});

test("applyCauses: 第一条命中的 cause 把焦点带走（focus + scrollIntoView）", () => {
	const A = loadAnotify();
	const { control: url } = mountField(DOC, { id: "f-poll-url", label: "接口地址" });
	const err = causesError([
		{ field: "spec.config.timeoutMs", reason: "value_invalid", message: "必须在 [1000, 120000] 毫秒内" },
		{ field: "spec.config.url", reason: "value_invalid", message: "必须是 http/https" },
	]);
	A.applyCauses(DOC.body, err, { "config.url": "f-poll-url" });
	assert.equal(DOC.activeElement, url, "焦点必须落到第一个命中的控件上");
	assert.equal(url.scrolled, 1, "聚焦同时把它滚进可视区");
});
