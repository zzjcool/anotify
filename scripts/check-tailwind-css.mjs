#!/usr/bin/env node
/* 核对已提交的 web/tailwind.css 覆盖源码里的 Tailwind 工具类。
 * 只用 Node 标准库，供 make fe / make ci 在不安装 tailwindcss 时拦下漏生成。
 *
 * 工具类来源：web-src class 属性、JS 的 class:/classList/引号里的类名串。
 * ui.css、tokens.css、页面 <style> 里定义过的组件类不算工具类。
 */
import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { isTailwind } from "./check-classes.mjs";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const CSS_OUT = path.join(root, "web", "tailwind.css");

const SHORT_UTILS = new Set([
	"flex",
	"grid",
	"block",
	"inline",
	"inline-block",
	"inline-flex",
	"hidden",
	"table",
	"contents",
	"relative",
	"absolute",
	"fixed",
	"sticky",
	"static",
	"truncate",
	"transform",
	"italic",
	"underline",
	"antialiased",
	"container",
	"group",
	"peer",
	"sr-only",
	"visible",
	"invisible",
	"collapse",
]);

// 一个 token 像不像 Tailwind 类（含变体、任意值、透明度、负值）。
const CLASS_RE =
	/^!?(?:[a-z0-9-]+:)*-?[a-z][a-z0-9]*(?:-[a-z0-9.]+|-\[[^\]]+\]|\/(?:[a-z0-9.]+|\[[^\]]+\]))*$/;

function stripTpl(s) {
	return s.replace(/\{\{[\s\S]*?\}\}/g, " ");
}

function addBlob(tokens, blob) {
	if (!blob) return;
	for (const raw of stripTpl(blob).split(/\s+/)) {
		const t = raw.trim();
		if (!t || t.includes("${") || t.includes("{{") || t.includes("}}")) continue;
		tokens.add(t);
	}
}

function considerQuoted(tokens, blob) {
	// 只收「空格分隔的类名串」。单个引号 token 多半是 id / aria，不是 class。
	if (
		!blob ||
		blob.length > 240 ||
		!blob.includes(" ") ||
		blob.includes("${") ||
		blob.includes("://")
	)
		return;
	const parts = stripTpl(blob).split(/\s+/).filter(Boolean);
	if (parts.length < 2) return;
	if (!parts.every((p) => CLASS_RE.test(p))) return;
	if (!parts.some((p) => p.includes("-") || SHORT_UTILS.has(p))) return;
	for (const p of parts) tokens.add(p);
}

function collectFromSource(src, tokens) {
	for (const m of src.matchAll(/\bclass\s*=\s*"([^"]*)"/g)) addBlob(tokens, m[1]);
	for (const m of src.matchAll(/\bclass\s*=\s*'([^']*)'/g)) addBlob(tokens, m[1]);
	for (const m of src.matchAll(/\bclass:\s*"([^"]*)"/g)) addBlob(tokens, m[1]);
	for (const m of src.matchAll(/\bclass:\s*'([^']*)'/g)) addBlob(tokens, m[1]);
	for (const m of src.matchAll(/\bclass:\s*`([^`]*)`/g)) addBlob(tokens, m[1]);
	for (const m of src.matchAll(/classList\.(?:add|remove|toggle)\(([^)]*)\)/g)) {
		for (const s of m[1].matchAll(/["'`]([^"'`]+)["'`]/g)) addBlob(tokens, s[1]);
	}
	for (const m of src.matchAll(/"([^"\n]*)"/g)) considerQuoted(tokens, m[1]);
	for (const m of src.matchAll(/'([^'\n]*)'/g)) considerQuoted(tokens, m[1]);
}

function cssClassNames(css) {
	const out = new Set();
	for (const m of css.matchAll(/\.(-?[A-Za-z_][\w-]*)/g)) out.add(m[1]);
	return out;
}

function escapeClass(cls) {
	return cls.replace(/([^a-zA-Z0-9_-])/g, "\\$1");
}

async function walkHtml(dir) {
	const out = [];
	let entries = [];
	try {
		entries = await fs.readdir(dir, { withFileTypes: true });
	} catch {
		return out;
	}
	for (const e of entries) {
		const full = path.join(dir, e.name);
		if (e.isDirectory()) out.push(...(await walkHtml(full)));
		else if (e.name.endsWith(".html")) out.push(full);
	}
	return out;
}

async function main() {
	let built = "";
	try {
		built = await fs.readFile(CSS_OUT, "utf8");
	} catch {
		console.error(
			`缺少 ${path.relative(root, CSS_OUT)}。请执行：npm install && node scripts/build-tailwind.mjs`,
		);
		process.exit(1);
	}
	if (!built.includes(".flex{") && !built.includes(".flex ")) {
		console.error("web/tailwind.css 不像 Tailwind 产物（找不到 .flex）。请重新生成。");
		process.exit(1);
	}

	const defined = new Set();
	for (const file of ["web/ui.css", "web/tokens.css"]) {
		try {
			const css = await fs.readFile(path.join(root, file), "utf8");
			for (const name of cssClassNames(css)) defined.add(name);
		} catch {
			/* ignore */
		}
	}

	const tokens = new Set();
	const htmlFiles = await walkHtml(path.join(root, "web-src"));
	for (const file of htmlFiles) {
		const src = await fs.readFile(file, "utf8");
		for (const name of cssClassNames(src)) defined.add(name);
		collectFromSource(src, tokens);
	}
	for (const rel of ["web/partials.js", "web/sw.js"]) {
		try {
			const src = await fs.readFile(path.join(root, rel), "utf8");
			collectFromSource(src, tokens);
		} catch {
			/* sw.js 可选 */
		}
	}

	const missing = [];
	for (const cls of tokens) {
		if (defined.has(cls)) continue;
		// 只强制「看起来是 Tailwind 工具类」的名字。自定义组件类不在
		// ui.css 里时不该要求产物生成（例如仅作 hook 的 id 风格名字）。
		if (!isTailwind(cls)) continue;
		const needle = "." + escapeClass(cls);
		if (!built.includes(needle)) missing.push(cls);
	}
	missing.sort();
	if (missing.length) {
		console.error(
			`web/tailwind.css 缺少 ${missing.length} 个工具类（页面上这些 class 不会生效）。`,
		);
		console.error("请执行：npm install && node scripts/build-tailwind.mjs");
		for (const cls of missing) console.error("  - " + cls);
		process.exit(1);
	}
	console.log(
		`✅ tailwind.css 覆盖 ${tokens.size} 个源码类名（组件类已排除）`,
	);
}

main().catch((e) => {
	console.error(e);
	process.exit(1);
});
