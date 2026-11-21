#!/usr/bin/env node
/* 页面/页面级脚本不得自带 YAML 序列化器（唯一实现在 web/yamldoc.js），前端 kind 表的字段
   顺序必须与服务端文档结构体（yaml tag）逐字同序 —— 任一项扫描面归零都退 2，绿不许与「没检查」同形。 */
import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import vm from "node:vm";

import { runsAsMain } from "./lib/direct-run.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

export const RULE = {
	DUP: "dup-serializer",
	LITERAL: "doc-literal",
	PARITY: "kind-parity",
	FLOOR: "floor",
};

export const FLOORS = { pages: 1, scripts: 1, kinds: 1, fields: 1 };

const PAGE_DIRS = ["web-src/pages", "web-src/layouts"];
const JS_ROOT = "web";
const YAMLDOC = "web/yamldoc.js";
const DOC_LITERAL_EXEMPT = new Set([YAMLDOC, "web/mock.js"]);

const DUP_RE =
	/(?:function\s+|(?:const|let|var)\s+)(yamlScalar|yamlLines|yamlList|yamlMap|yamlQuote|varsToYAMLLines|docToYAML|toYaml|namespaceYAMLText)\b/g;

const DOC_LITERAL_RES = [
	{
		re: /(['"])apiVersion: anotify\/v1\1/g,
		why: "整行文档头的字符串字面量（文档头只能由 web/yamldoc.js 生成）",
	},
	{
		re: /(['"])(apiVersion: |kind: )\1\s*\+/g,
		why: "以文档头为起点的字符串拼接（这就是「页面自己拼一份文档」的起点）",
	},
];

export const PARITY = [
	{
		kind: "Node",
		file: "internal/server/resource_node_yaml.go",
		root: "nodeYAMLDoc",
		structs: ["nodeYAMLDoc", "nodeYAMLMeta", "nodeYAMLSpec", "nodeYAMLLimits"],
	},
	{
		kind: "Namespace",
		file: "internal/server/resource_namespace_yaml.go",
		root: "namespaceYAMLDoc",
		structs: ["namespaceYAMLDoc", "namespaceYAMLMeta", "namespaceYAMLSpec"],
	},
];

export function lineOf(text, offset) {
	return text.slice(0, offset).split("\n").length;
}


const REGEX_AFTER_CHAR = new Set("(,=:[!&|?{};+-*/%~^<>".split(""));
const REGEX_AFTER_WORD = new Set([
	"return",
	"typeof",
	"case",
	"in",
	"of",
	"new",
	"delete",
	"void",
	"instanceof",
	"do",
	"else",
	"yield",
	"await",
]);

function skipQuoted(src, i) {
	const q = src[i];
	i += 1;
	while (i < src.length) {
		if (src[i] === "\\") {
			i += 2;
			continue;
		}
		if (src[i] === q) return i + 1;
		if (src[i] === "\n") return i;
		i += 1;
	}
	return i;
}

function skipTemplate(src, i) {
	i += 1;
	while (i < src.length) {
		const c = src[i];
		if (c === "\\") {
			i += 2;
			continue;
		}
		if (c === "`") return i + 1;
		if (c === "$" && src[i + 1] === "{") {
			i += 2;
			let depth = 1;
			while (i < src.length && depth > 0) {
				const ch = src[i];
				if (ch === '"' || ch === "'") {
					i = skipQuoted(src, i);
					continue;
				}
				if (ch === "`") {
					i = skipTemplate(src, i);
					continue;
				}
				if (ch === "{") depth += 1;
				else if (ch === "}") depth -= 1;
				i += 1;
			}
			continue;
		}
		i += 1;
	}
	return i;
}

function skipRegex(src, i) {
	let j = i + 1;
	let inClass = false;
	while (j < src.length) {
		const ch = src[j];
		if (ch === "\\") {
			j += 2;
			continue;
		}
		if (ch === "\n") return -1;
		if (ch === "[") inClass = true;
		else if (ch === "]") inClass = false;
		else if (ch === "/" && !inClass) {
			j += 1;
			while (j < src.length && /[A-Za-z]/.test(src[j])) j += 1;
			return j;
		}
		j += 1;
	}
	return -1;
}

export function stripComments(src, errors = []) {
	const out = src.split("");
	const blank = (from, to) => {
		for (let k = Math.max(0, from); k < Math.min(out.length, to); k += 1) {
			if (out[k] !== "\n") out[k] = " ";
		}
	};
	let i = 0;
	let prevSig = "";
	let prevWord = "";
	while (i < src.length) {
		const c = src[i];
		if (c === "/" && src[i + 1] === "/") {
			let j = src.indexOf("\n", i);
			if (j < 0) j = src.length;
			blank(i, j);
			i = j;
			continue;
		}
		if (c === "/" && src[i + 1] === "*") {
			let j = src.indexOf("*/", i + 2);
			if (j < 0) {
				errors.push({
					offset: i,
					message: "块注释 `/*` 没有闭合 —— 后面的整段都没被检查（拒绝静默跳过）",
				});
				j = src.length;
			} else {
				j += 2;
			}
			blank(i, j);
			i = j;
			continue;
		}
		if (c === '"' || c === "'") {
			i = skipQuoted(src, i);
			prevSig = c;
			prevWord = "";
			continue;
		}
		if (c === "`") {
			i = skipTemplate(src, i);
			prevSig = "`";
			prevWord = "";
			continue;
		}
		if (
			c === "/" &&
			(prevSig === "" || REGEX_AFTER_CHAR.has(prevSig) || REGEX_AFTER_WORD.has(prevWord))
		) {
			const j = skipRegex(src, i);
			if (j > 0) {
				i = j;
				prevSig = "/";
				prevWord = "";
				continue;
			}
		}
		if (/\s/.test(c)) {
			i += 1;
			continue;
		}
		if (/[A-Za-z_$]/.test(c)) {
			let j = i;
			while (j < src.length && /[\w$]/.test(src[j])) j += 1;
			prevWord = src.slice(i, j);
			prevSig = "w";
			i = j;
			continue;
		}
		prevSig = c;
		prevWord = "";
		i += 1;
	}
	return out.join("");
}


export function inlineScriptBlocks(html) {
	const blocks = [];
	let skipped = 0;
	for (const m of html.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script>/gi)) {
		const attrs = m[1] ?? "";
		if (/\bsrc\s*=/i.test(attrs)) {
			skipped += 1;
			continue;
		}
		const at = (m.index ?? 0) + m[0].indexOf(">") + 1;
		blocks.push({ src: m[2], at });
	}
	return { blocks, skipped };
}


function scanScript(name, text, { literal, base = 0, outer = null }) {
	const violations = [];
	const errors = [];
	const view = stripComments(text, errors);
	const lineAt = (offset) => (outer ? lineOf(outer, base + offset) : lineOf(view, offset));
	for (const m of view.matchAll(DUP_RE)) {
		violations.push({
			file: name,
			line: lineAt(m.index),
			rule: RULE.DUP,
			what: m[1],
			why: `声明了 YAML 序列化器 \`${m[1]}\` —— 转义与排版只许有一份（web/yamldoc.js）`,
		});
	}
	if (literal) {
		for (const { re, why } of DOC_LITERAL_RES) {
			for (const m of view.matchAll(re)) {
				violations.push({
					file: name,
					line: lineAt(m.index),
					rule: RULE.LITERAL,
					what: m[0].replace(/\s+/g, " "),
					why,
				});
			}
		}
	}
	return {
		violations,
		errors: errors.map((e) => ({ file: name, line: lineAt(e.offset), ...e })),
	};
}

export function scanSources({ jsFiles = [], pages = [] } = {}) {
	const violations = [];
	const errors = [];
	const counts = {
		jsFiles: 0,
		skippedYamlDoc: 0,
		pages: 0,
		scripts: 0,
		skippedScripts: 0,
	};
	for (const f of jsFiles) {
		if (f.file === YAMLDOC) {
			counts.skippedYamlDoc += 1;
			continue;
		}
		counts.jsFiles += 1;
		const r = scanScript(f.file, f.text, { literal: !DOC_LITERAL_EXEMPT.has(f.file) });
		violations.push(...r.violations);
		errors.push(...r.errors);
	}
	for (const p of pages) {
		counts.pages += 1;
		const { blocks, skipped } = inlineScriptBlocks(p.html);
		counts.scripts += blocks.length;
		counts.skippedScripts += skipped;
		for (const b of blocks) {
			const r = scanScript(p.file, b.src, { literal: true, base: b.at, outer: p.html });
			violations.push(...r.violations);
			errors.push(...r.errors);
		}
	}
	return { violations, errors, counts };
}


export function loadKinds(src, filename = YAMLDOC) {
	const ctx = vm.createContext({ console, JSON, Object, Array, String, Number, RegExp });
	vm.runInContext("globalThis.window = globalThis;", ctx);
	vm.runInContext(src, ctx, { filename });
	const api = ctx.window && ctx.window.AnotifyYamlDoc;
	if (!api || !api.KINDS) {
		throw new Error("web/yamldoc.js 没有挂载 window.AnotifyYamlDoc.KINDS");
	}
	return JSON.parse(JSON.stringify(api.KINDS));
}

export function flattenKindFields(fields, prefix = "") {
	const out = [];
	for (const f of fields || []) {
		const p = prefix ? `${prefix}.${f.key}` : String(f.key);
		out.push(p);
		if (f.type === "object") out.push(...flattenKindFields(f.fields, p));
	}
	return out;
}


export function parseGoStructs(src) {
	const structs = new Map();
	for (const head of src.matchAll(/type\s+([A-Za-z_]\w*)\s+struct\s*\{/g)) {
		const open = head.index + head[0].length - 1;
		let depth = 0;
		let close = src.length;
		for (let i = open; i < src.length; i += 1) {
			if (src[i] === "{") depth += 1;
			else if (src[i] === "}") {
				depth -= 1;
				if (depth === 0) {
					close = i;
					break;
				}
			}
		}
		structs.set(head[1], {
			line: lineOf(src, head.index),
			fields: parseGoFields(src.slice(open + 1, close), lineOf(src, open + 1)),
		});
	}
	return structs;
}

function parseGoFields(body, firstLine) {
	const fields = [];
	const lines = body.split("\n");
	for (let i = 0; i < lines.length; i += 1) {
		const line = lines[i].replace(/\s\/\/.*$/, "").trim();
		if (!line || line.startsWith("//") || line.startsWith("/*")) continue;
		const m = /^([A-Za-z_]\w*)\s+(\S+)(?:\s+`([^`]*)`)?$/.exec(line);
		if (!m) continue;
		const [, name, type, tags = ""] = m;
		const tag = /\byaml:"([^"]*)"/.exec(tags);
		if (!tag) continue;
		const yamlName = tag[1].split(",")[0].trim();
		if (!yamlName || yamlName === "-") continue;
		// 行号 = 该字段在 Go 源码里的声明行（kind-parity 的命中要指到真实位置）。
		fields.push({ name, type, yaml: yamlName, line: firstLine + i });
	}
	return fields;
}

/** 根结构体 → [{ path, line }]：字段类型是本文件里出现过的 struct 就递归，否则是叶子。 */
export function flattenGoDoc(structs, root) {
	const out = [];
	const seen = new Set();
	const walk = (typeName, prefix) => {
		const key = `${typeName}|${prefix}`;
		if (seen.has(key)) return;
		seen.add(key);
		const def = structs.get(typeName);
		if (!def) return;
		for (const f of def.fields) {
			const p = prefix ? `${prefix}.${f.yaml}` : f.yaml;
			out.push({ path: p, line: f.line });
			const base = f.type.replace(/^\*/, "").replace(/^\[\d*\]/, "");
			if (structs.has(base)) walk(base, p);
		}
	};
	walk(root, "");
	return out;
}

/** 同名字段集 + 相对顺序：多一个、少一个、顺序不同都算漂移。 */
export function compareKindFields(entry, feItems, goItems) {
	const findings = [];
	const feFields = feItems.map((f) => f.path);
	const goFields = goItems.map((f) => f.path);
	const feSet = new Set(feFields);
	const goSet = new Set(goFields);
	for (const p of feFields) {
		if (!goSet.has(p)) {
			findings.push({
				file: YAMLDOC,
				line: 1,
				rule: RULE.PARITY,
				kind: entry.kind,
				path: p,
				why: `前端 kind 表有 \`${p}\`，服务端文档结构体没有（${entry.file}）`,
			});
		}
	}
	for (const item of goItems) {
		if (!feSet.has(item.path)) {
			findings.push({
				file: entry.file,
				line: item.line,
				rule: RULE.PARITY,
				kind: entry.kind,
				path: item.path,
				why: `服务端文档结构体有 \`${item.path}\`，前端 kind 表没有 —— 这个字段会被静默丢掉`,
			});
		}
	}
	const commonFe = feFields.filter((p) => goSet.has(p));
	const commonGo = goItems.filter((item) => feSet.has(item.path));
	for (let i = 0; i < Math.min(commonFe.length, commonGo.length); i += 1) {
		if (commonFe[i] !== commonGo[i].path) {
			findings.push({
				file: entry.file,
				line: commonGo[i].line,
				rule: RULE.PARITY,
				kind: entry.kind,
				path: commonFe[i],
				why:
					`字段顺序不同：前端第 ${i + 1} 位是 \`${commonFe[i]}\`，服务端第 ${i + 1} 位是 ` +
					`\`${commonGo[i].path}\`（文档字节因此在两边不相等）`,
			});
			break;
		}
	}
	return findings;
}

/**
 * 逐 kind 对账。goSources = { "internal/server/xxx.go": 源码文本 }。
 * 结构体缺失、kind 缺失都算 kind-parity 违规（不是「跳过」：跳过就是假绿）。
 */
export function checkParity(kinds, goSources, plan = PARITY) {
	const findings = [];
	const counts = { kinds: 0, fields: 0 };
	for (const entry of plan) {
		const src = goSources[entry.file];
		if (typeof src !== "string") {
			findings.push({
				file: entry.file,
				line: 1,
				rule: RULE.PARITY,
				kind: entry.kind,
				why: "读不到服务端文档结构体所在文件",
			});
			continue;
		}
		const structs = parseGoStructs(src);
		const missing = entry.structs.filter((name) => !structs.has(name));
		if (missing.length) {
			findings.push({
				file: entry.file,
				line: 1,
				rule: RULE.PARITY,
				kind: entry.kind,
				why: `服务端文档结构体缺失：${missing.join(" / ")}`,
			});
			continue;
		}
		const feKind = kinds[entry.kind];
		if (!feKind || !Array.isArray(feKind.fields)) {
			findings.push({
				file: YAMLDOC,
				line: 1,
				rule: RULE.PARITY,
				kind: entry.kind,
				why: "前端 KINDS 里没有这个 kind 的字段表",
			});
			continue;
		}
		const feFields = flattenKindFields(feKind.fields).map((p) => ({ path: p, line: 1 }));
		const goFields = flattenGoDoc(structs, entry.root);
		counts.kinds += 1;
		counts.fields += feFields.length;
		findings.push(...compareKindFields(entry, feFields, goFields));
	}
	return { findings, counts };
}

/* ---------- ④ 反假绿下限 + 退出码 ---------- */

export function judgeFloors(counts, floors = FLOORS) {
	const out = [];
	const push = (label, have, want) =>
		out.push({
			rule: RULE.FLOOR,
			label,
			message: `${label} ${have}（下限 ${want}）—— 扫描面是空的，「绿」与「没检查」同形`,
		});
	for (const [key, label] of [
		["pages", "页面文件数"],
		["scripts", "内联 script 块数"],
		["kinds", "被对账的 kind 数"],
		["fields", "对账的字段数"],
	]) {
		if ((counts[key] ?? 0) < floors[key]) push(label, counts[key] ?? 0, floors[key]);
	}
	return out;
}

/** 退出码：解析错误 2 · 扫描面为空 2 · 违规 1 · 干净 0。 */
export function exitCodeOf({ errors = [], violations = [], floorFails = [] } = {}) {
	if (errors.length) return 2;
	if (floorFails.length) return 2;
	if (violations.length) return 1;
	return 0;
}

/* ---------- 主流程 ---------- */

const labelOf = (full) => path.relative(ROOT, full).split(path.sep).join("/");

async function readPageDir(dir) {
	const full = path.join(ROOT, dir);
	let names;
	try {
		names = await fs.readdir(full);
	} catch (e) {
		console.error(`✗ 读不到 ${dir}：${e.message}`);
		console.error("  —— 扫描面缺失时不许报绿（退出码 2）");
		process.exit(2);
	}
	const pages = [];
	for (const name of names.filter((n) => n.endsWith(".html")).sort()) {
		const file = path.join(full, name);
		pages.push({ file: labelOf(file), html: await fs.readFile(file, "utf8") });
	}
	return pages;
}

async function walkJs(dir) {
	const out = [];
	let entries;
	try {
		entries = await fs.readdir(dir, { withFileTypes: true });
	} catch (e) {
		console.error(`✗ 读不到 ${path.relative(ROOT, dir)}：${e.message}`);
		console.error("  —— 扫描面缺失时不许报绿（退出码 2）");
		process.exit(2);
	}
	for (const entry of entries.sort((a, b) => (a.name < b.name ? -1 : 1))) {
		const full = path.join(dir, entry.name);
		if (entry.isDirectory()) out.push(...(await walkJs(full)));
		else if (entry.name.endsWith(".js")) out.push(full);
	}
	return out;
}

function printCounts(counts) {
	console.log(
		`   扫描面：页面 ${counts.pages}（内联 script 块 ${counts.scripts}，跳过 <script src> ${counts.skippedScripts}）` +
			` · 页面级脚本 ${counts.jsFiles}（跳过 web/yamldoc.js ${counts.skippedYamlDoc}）`,
	);
	console.log(
		`   对账：kind ${counts.kinds} · 字段 ${counts.fields}` +
			`（下限：页面 ${FLOORS.pages} · script 块 ${FLOORS.scripts} · kind ${FLOORS.kinds} · 字段 ${FLOORS.fields}）`,
	);
}

async function main() {
	const pages = [];
	for (const dir of PAGE_DIRS) pages.push(...(await readPageDir(dir)));

	const jsFiles = [];
	for (const full of await walkJs(path.join(ROOT, JS_ROOT))) {
		jsFiles.push({ file: labelOf(full), text: await fs.readFile(full, "utf8") });
	}

	const scan = scanSources({ jsFiles, pages });

	const goSources = {};
	for (const entry of PARITY) {
		goSources[entry.file] = await fs.readFile(path.join(ROOT, entry.file), "utf8");
	}
	let kinds;
	let kindsError = null;
	try {
		kinds = loadKinds(await fs.readFile(path.join(ROOT, YAMLDOC), "utf8"));
	} catch (e) {
		kindsError = e;
	}
	if (kindsError) {
		console.error(`✗ 载入 ${YAMLDOC} 失败：${kindsError.message}`);
		process.exit(2);
	}
	const parity = checkParity(kinds, goSources);

	const counts = {
		...scan.counts,
		kinds: parity.counts.kinds,
		fields: parity.counts.fields,
	};
	const violations = [...scan.violations, ...parity.findings];
	const floors = judgeFloors(counts);
	const code = exitCodeOf({ errors: scan.errors, violations, floorFails: floors });

	if (scan.errors.length) {
		console.error(`✗ 文档层：解析失败 ${scan.errors.length} 处（退出码 2，不静默跳过）`);
		for (const e of scan.errors) console.error(`   ${e.file}:${e.line} → ${e.message}`);
		console.error("");
		printCounts(counts);
		process.exit(code);
	}

	if (floors.length) {
		console.error(`✗ 文档层：${floors.length} 项扫描面低于下限（反假绿，退出码 2）`);
		for (const f of floors) console.error(`   [${f.rule}] ${f.message}`);
		console.error("");
		printCounts(counts);
		process.exit(code);
	}

	if (violations.length) {
		console.error(`✗ 文档层：${violations.length} 处违反「只有一份 YAML 序列化」的判据（#473）`);
		for (const v of violations) {
			const head = `${v.file}:${v.line} → [${v.rule}]`;
			const body = v.what ? `${head} ${v.what} → ${v.why}` : `${head} ${v.why}`;
			console.error(`   ${body}`);
		}
		console.error("");
		console.error("   处置：文档文本只由 web/yamldoc.js 生成；页面把表单改动交给 YD.edit(kind, 原文, patch)，");
		console.error("   服务端加文档字段时同批更新前端 kind 表（顺序也要一致，否则文档字节两边不相等）。");
		console.error("");
		printCounts(counts);
		process.exit(1);
	}

	console.log("✅ 文档层：页面与页面级脚本没有第二份 YAML 序列化器；前端 kind 表与服务端文档结构体同序同集");
	printCounts(counts);
}

/* ---------- --self-test：红/绿双侧自证（扫描夹具纯内存，对账夹具读真实文件后在内存里变异） ---------- */

const pageOf = (js) => `<script>\n${js}\n</script>`;

/** 在结构体体内部做一次编译期编辑（返回 null = 锚点不在了 ⇒ 自测要红，不是静默跳过）。 */
function editGoStruct(src, structName, edit) {
	const head = new RegExp(`type\\s+${structName}\\s+struct\\s*\\{`).exec(src);
	if (!head) return null;
	const open = head.index + head[0].length - 1;
	let depth = 0;
	let close = -1;
	for (let i = open; i < src.length; i += 1) {
		if (src[i] === "{") depth += 1;
		else if (src[i] === "}") {
			depth -= 1;
			if (depth === 0) {
				close = i;
				break;
			}
		}
	}
	if (close < 0) return null;
	const body = src.slice(open + 1, close);
	const edited = edit(body);
	if (edited == null || edited === body) return null;
	return src.slice(0, open + 1) + edited + src.slice(close);
}

const dropLine = (body, re) => {
	const lines = body.split("\n").filter((line) => !re.test(line));
	return lines.length === body.split("\n").length ? null : lines.join("\n");
};

export function selfTestCases() {
	return [
		{
			name: "红① 页面声明标量转义器：const yamlScalar = …",
			scan: { pages: [{ file: "web-src/pages/node.html", html: pageOf("const yamlScalar = v => String(v);") }] },
			wantRules: [RULE.DUP],
			wantCounts: { pages: 1, scripts: 1 },
		},
		{
			name: "红② 页面级脚本声明拼行器：function varsToYAMLLines（web/vars-table.js 同形）",
			scan: {
				jsFiles: [
					{ file: "web/vars-table.js", text: "function varsToYAMLLines(vars, indent) {\n  return [];\n}\n" },
				],
			},
			wantRules: [RULE.DUP],
			wantCounts: { jsFiles: 1 },
		},
		{
			name: "红③ var 形态：var toYaml = doc => doc",
			scan: { jsFiles: [{ file: "web/partials.js", text: "var toYaml = doc => doc;\n" }] },
			wantRules: [RULE.DUP],
			wantCounts: { jsFiles: 1 },
		},
		{
			name: "红④ 页面声明整套发射器：function docToYAML",
			scan: {
				pages: [{ file: "web-src/pages/namespace.html", html: pageOf("function docToYAML(doc) {\n  return '';\n}") }],
			},
			wantRules: [RULE.DUP],
			wantCounts: { scripts: 1 },
		},
		{
			name: "红⑤ doc-literal：把文档头当拼接起点 ['apiVersion: ' + 'anotify/v1']",
			scan: {
				pages: [
					{ file: "web-src/pages/node.html", html: pageOf("const t = ['apiVersion: ' + 'anotify/v1'].join('');") },
				],
			},
			wantRules: [RULE.LITERAL],
			wantCounts: { scripts: 1 },
		},
		{
			name: "红⑥ doc-literal：双引号 \"kind: \" + 名字",
			scan: { pages: [{ file: "web-src/pages/node.html", html: pageOf('const t = "kind: " + k;') }] },
			wantRules: [RULE.LITERAL],
			wantCounts: { scripts: 1 },
		},
		{
			name: "红⑦ doc-literal：整行文档头的字面量",
			scan: {
				pages: [{ file: "web-src/layouts/base.html", html: pageOf("const head = 'apiVersion: anotify/v1';") }],
			},
			wantRules: [RULE.LITERAL],
			wantCounts: { scripts: 1 },
		},
		{
			name: "红⑧ doc-literal：页面级脚本也在这个扫描面里（web/partials.js）",
			scan: { jsFiles: [{ file: "web/partials.js", text: "const head = 'apiVersion: ' + v;\n" }] },
			wantRules: [RULE.LITERAL],
			wantCounts: { jsFiles: 1 },
		},
		{
			name: "红⑨ 同一条规则在一份文件里命中多处也只报多行（行号各指真实位置）",
			scan: {
				pages: [
					{
						file: "web-src/pages/node.html",
						html: `<script>\nconst a = 1;\nconst yamlList = v => v;\nconst b = 'kind: ' + a;\n</script>`,
					},
				],
			},
			wantRules: [RULE.DUP, RULE.LITERAL],
			wantCounts: { scripts: 1 },
			wantLines: [3, 4],
		},
		{
			name: "绿① <script src> 块跳过（不算扫描面、也不算违规）",
			scan: {
				pages: [
					{
						file: "web-src/layouts/base.html",
						html: `<script src="/yamldoc.js"></script>\n<script>\nconst ok = 1;\n</script>`,
					},
				],
			},
			wantRules: [],
			wantCounts: { scripts: 1, skippedScripts: 1 },
		},
		{
			name: "绿② 注释里的同样文本不算（行注释 + 块注释）",
			scan: {
				pages: [
					{
						file: "web-src/pages/node.html",
						html: pageOf(
							"// const yamlScalar = v => String(v);  —— 注释里提到不算\n/* const head = 'apiVersion: ' + 'anotify/v1'; */\nconst ok = 1;",
						),
					},
				],
			},
			wantRules: [],
			wantCounts: { scripts: 1 },
		},
		{
			name: "绿③ web/mock.js 的文档文本字面量豁免（假服务端的 /yaml 路由要返回文本）",
			scan: {
				jsFiles: [
					{
						file: "web/mock.js",
						text: 'const lines = ["apiVersion: anotify/v1", "kind: Node"];\nconst t = "kind: " + n;\n',
					},
				],
			},
			wantRules: [],
			wantCounts: { jsFiles: 1 },
		},
		{
			name: "绿④ web/yamldoc.js 自身不进扫描面（它就是那份实现）",
			scan: {
				jsFiles: [
					{ file: YAMLDOC, text: "function scalar(v) { return String(v); }\nconst head = 'apiVersion: ' + v;\n" },
					{ file: "web/graphs.js", text: "const ok = 1;\n" },
				],
			},
			wantRules: [],
			wantCounts: { jsFiles: 1, skippedYamlDoc: 1 },
		},
		{
			name: "绿⑤ 页面级脚本里的注释提到序列化器名字不算（web/vars-table.js 的真实注释同形）",
			scan: {
				jsFiles: [
					{
						file: "web/vars-table.js",
						text: "/**\n * 本文件曾经自带一份 `yamlScalar` / `varsToYAMLLines`，那是第二份转义规则。\n */\nvar API = {};\n",
					},
				],
			},
			wantRules: [],
			wantCounts: { jsFiles: 1 },
		},
		{
			name: "绿⑥ 字符串里的 // 不是注释（'https://…' 不许把后半行吞掉）",
			scan: {
				pages: [
					{
						file: "web-src/pages/node.html",
						html: pageOf("const u = 'https://hub.example/v1';\nconst yamlMap = v => v;"),
					},
				],
			},
			wantRules: [RULE.DUP],
			wantCounts: { scripts: 1 },
		},
	];
}

export function selfTestParityCases() {
	return [
		{ name: "绿⑦ 仓库当前的真实文件：前端 kind 表与服务端文档结构体对齐", mutate: null },
		{
			name: "红⑩ Go 结构体里删掉 tags 字段",
			goFile: "internal/server/resource_node_yaml.go",
			mutate: (src, file) =>
				file === "internal/server/resource_node_yaml.go"
					? editGoStruct(src, "nodeYAMLSpec", (body) => dropLine(body, /^\s*Tags\s+\[\]string\s/))
					: null,
			wantRules: [RULE.PARITY],
			wantKind: "Node",
			wantPath: "spec.tags",
		},
		{
			name: "红⑪ Go 结构体里把 namespaces 与 vars 顺序换一下",
			goFile: "internal/server/resource_node_yaml.go",
			mutate: (src, file) => {
				if (file !== "internal/server/resource_node_yaml.go") return null;
				return editGoStruct(src, "nodeYAMLSpec", (body) => {
					const lines = body.split("\n");
					const i = lines.findIndex((l) => /^\s*Namespaces\s+\[\]string\s/.test(l));
					const j = lines.findIndex((l) => /^\s*Vars\s+map\[string\]string\s/.test(l));
					if (i < 0 || j < 0 || i >= j) return null;
					const swapped = lines.slice();
					[swapped[i], swapped[j]] = [swapped[j], swapped[i]];
					return swapped.join("\n");
				});
			},
			wantRules: [RULE.PARITY],
			wantKind: "Node",
		},
		{
			name: "红⑫ 前端 kind 表里删掉 tags 那一行",
			mutateYamlDoc: (src) => {
				/* 锚点跟着 kind 表的真实写法走：`tags` 那一行可能带/不带 `omit`/`empty` 声明，
				 * 这里按「key: 'tags' 开头的字段声明行」匹配 —— 判据没放宽，只是夹具不再绑死
				 * 一行的完整文本（declaration 的尾注改了不该让自测变红）。 */
				const out = src.replace(/^[ \t]*\{ key: 'tags',[^\n]*\},$/m, "");
				return out === src ? null : out;
			},
			wantRules: [RULE.PARITY],
			wantKind: "Node",
			wantPath: "spec.tags",
		},
		{
			name: "红⑬ 前端 kind 表缺整个 kind（Namespace 被删掉）",
			mutateKinds: (kinds) => {
				if (!kinds.Namespace) return null;
				const out = { ...kinds };
				delete out.Namespace;
				return out;
			},
			wantRules: [RULE.PARITY],
			wantKind: "Namespace",
		},
		{
			name: "绿⑧ 自足的内存对照对（不依赖仓库文件也能对上）",
			synthetic: true,
			wantRules: [],
		},
	];
}

async function selfTest() {
	let failed = 0;
	let cases = 0;
	const test = (name, ok, detail) => {
		cases += 1;
		if (ok) console.log(`  ✅ ${name}`);
		else {
			failed += 1;
			console.error(`  ❌ ${name}${detail ? ` —— ${detail}` : ""}`);
		}
	};

	// ①② 扫描面：红/绿两侧
	for (const c of selfTestCases()) {
		const r = scanSources(c.scan);
		const got = r.violations.map((v) => v.rule).sort();
		const want = [...c.wantRules].sort();
		const same = JSON.stringify(got) === JSON.stringify(want);
		test(
			c.name,
			same && r.errors.length === 0,
			same
				? `解析错误 ${JSON.stringify(r.errors.map((e) => e.message))}`
				: `want ${JSON.stringify(want)}, got ${JSON.stringify(got)}` +
						(r.violations.length ? ` (${r.violations.map((v) => `${v.line}:${v.what}`).join(" | ")})` : ""),
		);
		for (const [key, value] of Object.entries(c.wantCounts ?? {})) {
			test(
				`  ↳ ${key} 计数 = ${value}`,
				r.counts[key] === value,
				`got ${r.counts[key]}`,
			);
		}
		if (c.wantLines) {
			const lines = r.violations.map((v) => v.line).sort((a, b) => a - b);
			test(`  ↳ 行号指向真实行 ${JSON.stringify(c.wantLines)}`, JSON.stringify(lines) === JSON.stringify(c.wantLines), `got ${JSON.stringify(lines)}`);
		}
		// 反假绿：每条绿用例的扫描面必须非空（否则「没扫到」也会绿）
		if (c.wantRules.length === 0) {
			const surface = r.counts.pages + r.counts.scripts + r.counts.jsFiles + r.counts.skippedScripts;
			test("  ↳ 绿用例的扫描面非空", surface > 0, JSON.stringify(r.counts));
		}
	}

	// ③ 对账：真实文件 + 内存变异
	const yamlDocSrc = await fs.readFile(path.join(ROOT, YAMLDOC), "utf8");
	const realGo = {};
	for (const entry of PARITY) {
		realGo[entry.file] = await fs.readFile(path.join(ROOT, entry.file), "utf8");
	}
	for (const c of selfTestParityCases()) {
		if (c.synthetic) {
			const synthKinds = {
				Thing: {
					fields: [
						{ key: "apiVersion", type: "string" },
						{
							key: "spec",
							type: "object",
							fields: [
								{ key: "vars", type: "string-map", empty: "{}" },
								{ key: "tags", type: "strings", empty: "[]" },
							],
						},
					],
				},
			};
			const synthGo = {
				"internal/server/resource_node_yaml.go":
					'package server\n\ntype thingDoc struct {\n\tAPIVersion string `yaml:"apiVersion" json:"apiVersion"`\n\tSpec       thingSpec `yaml:"spec" json:"spec"`\n}\n\ntype thingSpec struct {\n\tVars map[string]string `yaml:"vars" json:"vars"`\n\tTags []string          `yaml:"tags,omitempty" json:"tags"`\n}\n',
			};
			// 自带一张对照表（不碰生产 PARITY）：形状与 Node 同构，但类型名与 kind 都是自造的。
			const synthPlan = [
				{
					kind: "Thing",
					file: "internal/server/resource_node_yaml.go",
					root: "thingDoc",
					structs: ["thingDoc", "thingSpec"],
				},
			];
			const r = checkParity(synthKinds, synthGo, synthPlan);
			test(
				c.name,
				r.findings.length === 0 && r.counts.kinds === 1 && r.counts.fields === 4,
				`findings ${JSON.stringify(r.findings.map((f) => f.why))} counts ${JSON.stringify(r.counts)}`,
			);
			const surface = r.counts.kinds + r.counts.fields;
			test("  ↳ 绿用例的对账面非空", surface > 0, JSON.stringify(r.counts));
			continue;
		}

		let kinds;
		let goSources = { ...realGo };
		let mutateOk = true;
		try {
			let src = yamlDocSrc;
			if (c.mutateYamlDoc) {
				const mutated = c.mutateYamlDoc(src);
				mutateOk = mutated != null;
				src = mutated ?? src;
			}
			kinds = loadKinds(src);
			if (c.mutateKinds) {
				const mutated = c.mutateKinds(kinds);
				mutateOk = mutateOk && mutated != null;
				kinds = mutated ?? kinds;
			}
			if (c.mutate) {
				for (const file of Object.keys(goSources)) {
					const mutated = c.mutate(goSources[file], file);
					if (file === c.goFile) {
						mutateOk = mutateOk && mutated != null;
						goSources[file] = mutated ?? goSources[file];
					}
				}
			}
		} catch (e) {
			test(c.name, false, e.message);
			continue;
		}
		const r = checkParity(kinds, goSources);
		const got = r.findings.map((f) => f.rule).sort();
		const want = [...(c.wantRules ?? [])].sort();
		const same = JSON.stringify(got) === JSON.stringify(want);
		const wantKindOk = !c.wantKind || r.findings.some((f) => f.kind === c.wantKind);
		const wantPathOk = !c.wantPath || r.findings.some((f) => f.path === c.wantPath);
		test(
			c.name,
			mutateOk && same && wantKindOk && wantPathOk,
			!mutateOk
				? "变异锚点不在了（源码形态变了：先修夹具，不要放宽判据）"
				: `want ${JSON.stringify(want)} / kind ${c.wantKind ?? "-"} / path ${c.wantPath ?? "-"}, got ${JSON.stringify(r.findings.map((f) => `${f.rule}:${f.kind ?? ""}:${f.path ?? ""}:${f.why}`))}`,
		);
		// 反假绿：对齐用的绿用例，两侧都必须真的出了字段
		if ((c.wantRules ?? []).length === 0) {
			test(
				"  ↳ 绿用例的对账面非空（kind 与字段都 > 0）",
				r.counts.kinds > 0 && r.counts.fields > 0,
				JSON.stringify(r.counts),
			);
		}
	}

	// ④ 下限与退出码
	const zero = judgeFloors({ pages: 0, scripts: 0, kinds: 0, fields: 0 });
	test(
		"下限归零 ⇒ 逐项点名 floor，且退出码 2",
		zero.length === 4 && zero.every((f) => f.rule === RULE.FLOOR) && exitCodeOf({ floorFails: zero }) === 2,
		JSON.stringify(zero.map((f) => `${f.rule}:${f.label}`)),
	);
	test(
		"计数达标 ⇒ 下限不判红（exit 0）",
		judgeFloors({ pages: 1, scripts: 1, kinds: 1, fields: 1 }).length === 0 &&
			exitCodeOf({}) === 0,
	);
	test(
		"解析错误 ⇒ exit 2（不是 1）",
		exitCodeOf({ errors: [{ file: "x.html", line: 1, message: "块注释没闭合" }] }) === 2,
	);
	test(
		"有违规 ⇒ exit 1",
		exitCodeOf({ violations: [{ rule: RULE.DUP }] }) === 1,
	);

	console.log(
		failed === 0
			? `✅ self-test ${cases}/${cases} 通过`
			: `✗ self-test ${cases - failed}/${cases} 通过`,
	);
	return failed === 0;
}

/* ---------- 入口 ---------- */

const USAGE = `用法：
  node scripts/check-yaml-doc.mjs             # 体检：exit 0 绿 / 1 违规 / 2 解析或用法错
  node scripts/check-yaml-doc.mjs --self-test # 守卫自身的红/绿双侧自证
  node scripts/check-yaml-doc.mjs --help

判据（#473）：页面与页面级脚本不得自带 YAML 序列化器（${RULE.DUP}）；
  页面不得自己拼文档头（${RULE.LITERAL}）；
  前端 kind 表与服务端文档结构体逐字段同序同集（${RULE.PARITY}）；
  扫描面不得为空（${RULE.FLOOR}）。`;

const isDirectRun = runsAsMain(import.meta.url);
if (isDirectRun) {
	const argv = process.argv.slice(2);
	const known = new Set(["--self-test", "--help", "-h"]);
	const unknown = argv.filter((a) => !known.has(a));
	if (unknown.length) {
		console.error(`✗ 不认识的参数：${unknown.join(" ")}`);
		console.error(USAGE);
		process.exit(2);
	}
	if (argv.includes("--help") || argv.includes("-h")) {
		console.log(USAGE);
		process.exit(0);
	}
	if (argv.includes("--self-test")) {
		selfTest()
			.then((passed) => process.exit(passed ? 0 : 1))
			.catch((e) => {
				console.error(e);
				process.exit(2);
			});
	} else {
		main().catch((e) => {
			console.error(e);
			process.exit(2);
		});
	}
}
