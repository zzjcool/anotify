#!/usr/bin/env node
/* spec-doc: 本文件是「web-src/pages 里不得出现对 /config 的写调用」这条检查自己的规格说明
 *   —— 判据、例外面、下限与 --self-test 夹具都写在本文件里，它描述的是本文件实现的检查。
 *
 * Anotify · 配置写路径守卫（#476）
 *
 * 拦的事故
 *   #476 把 `/config` 降级成**读投影**：节点/命名空间的 vars 与配置以 YAML 为唯一编辑面，
 *   `/config` 只用来显示「现在生效值是什么」。可页面里残留着第二条写路径：
 *     node.html:550      write(path + '/config', body)        ← 表单直接 PUT …/config
 *     namespace.html:156 write(nsPath(saveScope), { … })      ← nsPath() 拼出 …/config
 *   两条路都能改同一份事实，而 YAML 那一面看不到表单改的值 —— 也就是「同一个事实两处
 *   各自演化」（docs/lint-guards.md §0 第 ⑤ 类缝隙）。这类漂移没有任何运行期报错：
 *   两个入口各自 200，只有用户看到的值对不上。所以必须在静态面上把它们锁成 0。
 *
 * 判据（真解析，不是 grep 一遍字符串）
 *   ① 扫描面：`web-src/pages/*.html` 的内联 `<script>` 块（`<script src>` 天然跳过）；
 *      先剥 JS 注释（状态机：字符串 / 模板字符串 / 正则里的 `//` 不是注释 —— 否则
 *      `'https://…/config'` 会把后半行吞掉，静默变成假绿）。
 *   ② 「config 路径源」（两种形态，都是**值**层面的判据，不是文本包含）：
 *      a) 任何单/双引号字符串或模板字符串，其**静态尾部**以 `/config` 结尾；
 *      b) `const|let|var NAME = <表达式>;` 且表达式区间里含上述字面量
 *         → NAME 是 config 路径源（含 `NAME = arg => …/config` 这种函数形态）。
 *   ③ 写调用（按**调用点**判，不按行：一行里可以有读也有写）：
 *      a) 页面自己定义了 `const write =` / `function write(` 时，`write(...)` 是写调用
 *         （显式第 3 参是 `'GET'` 的除外 —— node.html 的 helper 就是 (url, body, method='PUT')）；
 *      b) `….api(` 且选项对象里 `method:` 是字面量 PUT/POST/PATCH/DELETE；
 *      c) `fetch(` 且 method 字面量是写方法。
 *      取第一个实参的源文本（括号配平切分实参），若它
 *        · 含 `/config` 结尾的字面量，或
 *        · 等于一个 config 路径源名 / 以 `NAME(` / `NAME +` 开头引用它
 *      判红。GET 读 /config（`A.api('/v1/…/config')`、`A.api(nsPath(ns))`）不判 ——
 *      判据只在「写调用点」上求值，读调用根本进不了这个集合。
 *   ④ 输出 `文件:行 → 调用原文 → 命中原因`；退出码 0 绿 / 1 违规 / 2 解析或用法错误。
 *   ⑤ 反假绿：页面数 / script 块数 / 写调用点数 / config 路径源数各带下限（0 即红），
 *      目录读不到直接退 2 —— 「什么都没扫到」不许长得像「没问题」。
 *   ⑥ 唯一的例外面（系统保留配置单例）：只挂**资源级 config 投影**的写，也就是
 *      `…/nodes/<x>/config` / `…/namespaces/<x>/config`（含 `const nsPath = …` 这种
 *      helper 拼出的同形状路径）。`write('/v1/system/config', …)`（admin.html）**不判红**：
 *      它不是资源级投影，而是管理员单例，且它的 system YAML 是**只读**的
 *      （`/v1/system/yaml` 只有 GET）——所以 `PUT /v1/system/config` 就是系统保留期的
 *      **唯一写入口**（CAS + 危险操作确认；服务端 putSystemConfig 就是它）。
 *      拢掉它等于删功能，而且 #476 的范围是节点/命名空间 vars 的投影，不含它。
 *      默认仍是 fail-closed：凡末尾 `/config` 但**没明写** `/system/` 的，一律按资源
 *      投影判红 —— 例外只认「明写系统配置路径」这一个形状，不靠减号名单。
 *
 * 变异口径（--self-test 里每条都自证过）
 *   红：`write(nsPath(x), {})`（源名调用）· `A.api('/v1/nodes/' + n + '/config', {method:'PUT'})`
 *       （字面量）· `const p = '…/config'; A.api(p, {method:'DELETE'})`（源名等于）·
 *       `fetch('…/config', {method:'PATCH'})` · URL 里的 `//` 不被当注释。
 *   绿：GET 读 /config 两种形态 · `path + '/yaml'` 的 PUT · `write(path + '/yaml')` ·
 *       显式 `write(…, {}, 'GET')` · 注释里的 `write(…'/config')` · `<script src>` 块 ·
 *       **系统保留期 `write('/v1/system/config', body)`**（⑥ 的例外面）。
 *   每条红用例断言**命中的是哪条规则**（防碰巧被另一条规则抓到），每条绿用例断言
 *   写调用计数 > 0（证明该夹具的扫描面非空，不是「没扫到所以绿」）。
 *
 * 已知的判据边界（宁可漏报不误报）
 *   · 只认 `write` / `.api` / `fetch` 三种调用形态：页面若自造 `const act = p => A.api(p,
 *     {method:'POST'})` 再 `act(nsPath(x))`，本守卫看不见（那是另一种 helper 形态，
 *     真出现时按同一判据扩表，而不是放宽）；
 *   · 第一个实参用变量间接引用（`const u = nsPath(x); A.api(u, {method:'PUT'})`）不在
 *     判据内 —— 本仓页面没有这种写法，加了会显著放大误报面；
 *   · 源名判据是**语法**层面的（「初始化器里含 /config 字面量」），所以
 *     `const value = await A.api(path + '/config')` 也会把 `value` 记成源。这是刻意的
 *     fail-closed：代价是极少数 `write(value, …)` 会被多拦一次，收益是「先读进变量
 *     再写」这种绕法不会成为口子。
 *
 * 用法
 *   node scripts/check-config-writes.mjs [--src=web-src] [--self-test]
 */
import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const SRC = path.resolve(
	process.argv.find((a) => a.startsWith("--src="))?.split("=")[1] ?? "web-src",
);

/** 规则码（docs/lint-guards.md §3.2 的 `@guard …#<规则码>` 锚点用这几个字面量）。 */
export const RULE = {
	LITERAL: "config-literal-write",
	SOURCE: "config-source-write",
	UNBALANCED: "parse-unbalanced-call",
	UNTERMINATED: "parse-unterminated-literal",
	NO_PAGES: "no-pages-scanned",
	NO_SCRIPTS: "no-script-blocks",
	NO_WRITES: "no-write-call-sites",
	NO_SOURCES: "no-config-sources",
};

/** 反假绿下限：任何一项归零都判红（「绿」不许与「没检查」同形）。 */
export const FLOORS = { pages: 1, scripts: 1, writeCalls: 1, configSources: 1 };

const WRITE_METHODS = new Set(["PUT", "POST", "PATCH", "DELETE"]);
/**
 * 系统保留配置单例的写入路径后缀 —— 判据的唯一边界（见文件头 ⑥）。
 * 它**不是**资源级 config 投影：system YAML 只读（`GET /v1/system/yaml`），
 * 所以 `PUT /v1/system/config` 是保留期唯一写入口（服务端 putSystemConfig），
 * 本守卫不碰它。
 */
const SYSTEM_CONFIG_SUFFIX = "/system/config";
/** 选项对象里的 `method: 'PUT'` —— 只认字面量（`method` 变量不是静态可判的）。 */
const METHOD_LITERAL_RE = /(?<![\w$.])method\s*:\s*(['"`])([A-Za-z]+)\1/;
const WRITE_HELPER_DECL_RE = /(?<![\w$.])(?:const|let|var)\s+write\s*=(?!=)/;
const WRITE_HELPER_FN_RE = /(?<![\w$.])function\s+write\s*\(/g;
const DECL_RE = /(?<![\w$.])(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=(?![=>])/g;

export function lineOf(text, offset) {
	return text.slice(0, offset).split("\n").length;
}

/* ---------- ① 状态机：剥注释 + 产出 raw/code 两个视图 ---------- */

/** 正则字面量可以出现在这些字符之后（`/` 是除号还是正则开头）。 */
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

/** 跳过一个字符串字面量（返回 `结束引号` 之后的下标；未闭合不跨行）。 */
function skipString(src, i) {
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

/** 跳过模板字符串表达式 `${ … }`（含嵌套字符串/花括号），返回 `}` 之后的下标。 */
function skipTemplateExpression(src, i) {
	let depth = 1;
	while (i < src.length && depth > 0) {
		const c = src[i];
		if (c === '"' || c === "'" || c === "`") {
			i = skipString(src, i);
			continue;
		}
		if (c === "{") depth += 1;
		else if (c === "}") depth -= 1;
		i += 1;
	}
	return i;
}

/**
 * 一遍扫描：注释抹成空格（保留换行 ⇒ 行号不变），字符串/模板/正则的内容在 code 视图里抹空。
 *   raw  —— 注释已剥，字面量内容保留（读「值」与实参原文用）
 *   code —— 注释 + 字面量内容都抹空（找调用点用；字符串里写着 `.api(` 不算调用）
 * 同时收集字面量表（含模板字符串的静态尾部 `tail`）与解析错误。
 */
export function tokenize(src) {
	const raw = src.split("");
	const code = src.split("");
	const literals = [];
	const errors = [];
	const blank = (arr, from, to) => {
		for (let k = Math.max(0, from); k < Math.min(arr.length, to); k += 1) {
			if (arr[k] !== "\n") arr[k] = " ";
		}
	};
	let i = 0;
	let prevSig = "";
	let prevWord = "";
	while (i < src.length) {
		const c = src[i];
		// 行注释：`'https://…'` 里的 // 在字符串分支里已被吃掉，走不到这里
		if (c === "/" && src[i + 1] === "/") {
			let j = src.indexOf("\n", i);
			if (j < 0) j = src.length;
			blank(raw, i, j);
			blank(code, i, j);
			i = j;
			continue;
		}
		// 块注释
		if (c === "/" && src[i + 1] === "*") {
			let j = src.indexOf("*/", i + 2);
			if (j < 0) {
				errors.push({
					rule: RULE.UNTERMINATED,
					offset: i,
					message: "块注释 `/*` 没有闭合 —— 后面的整段都没被检查（拒绝静默跳过）",
				});
				j = src.length;
			} else {
				j += 2;
			}
			blank(raw, i, j);
			blank(code, i, j);
			i = j;
			continue;
		}
		// 单/双引号字符串
		if (c === '"' || c === "'") {
			const start = i;
			i += 1;
			let value = "";
			let closed = false;
			while (i < src.length) {
				const ch = src[i];
				if (ch === "\\") {
					value += src.slice(i, i + 2);
					blank(code, i, i + 2);
					i += 2;
					continue;
				}
				if (ch === c) {
					closed = true;
					i += 1;
					break;
				}
				if (ch === "\n") break;
				value += ch;
				blank(code, i, i + 1);
				i += 1;
			}
			if (!closed) {
				errors.push({
					rule: RULE.UNTERMINATED,
					offset: start,
					message: `${c} 字符串没有闭合 —— 后面的整段都没被检查（拒绝静默跳过）`,
				});
			}
			literals.push({ start, end: i, quote: c, template: false, tail: value });
			prevSig = c;
			prevWord = "";
			continue;
		}
		// 模板字符串：静态块抹空、`${…}` 表达式保留（里面可能是真调用）
		if (c === "`") {
			const start = i;
			i += 1;
			let chunk = "";
			let closed = false;
			while (i < src.length) {
				const ch = src[i];
				if (ch === "\\") {
					chunk += src.slice(i, i + 2);
					blank(code, i, i + 2);
					i += 2;
					continue;
				}
				if (ch === "`") {
					closed = true;
					i += 1;
					break;
				}
				if (ch === "$" && src[i + 1] === "{") {
					chunk = "";
					i = skipTemplateExpression(src, i + 2);
					continue;
				}
				chunk += ch;
				blank(code, i, i + 1);
				i += 1;
			}
			if (!closed) {
				errors.push({
					rule: RULE.UNTERMINATED,
					offset: start,
					message: "模板字符串 ` 没有闭合 —— 后面的整段都没被检查（拒绝静默跳过）",
				});
			}
			literals.push({ start, end: i, quote: c, template: true, tail: chunk });
			prevSig = c;
			prevWord = "";
			continue;
		}
		// 正则字面量：内容不该参与判据（`/…\/config$/` 不是路径字面量）
		if (
			c === "/" &&
			(prevSig === "" || REGEX_AFTER_CHAR.has(prevSig) || REGEX_AFTER_WORD.has(prevWord))
		) {
			let j = i + 1;
			let inClass = false;
			let closed = false;
			while (j < src.length) {
				const ch = src[j];
				if (ch === "\\") {
					j += 2;
					continue;
				}
				if (ch === "\n") break;
				if (ch === "[") inClass = true;
				else if (ch === "]") inClass = false;
				else if (ch === "/" && !inClass) {
					closed = true;
					j += 1;
					break;
				}
				j += 1;
			}
			if (closed) {
				blank(code, i + 1, j - 1);
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
	return { raw: raw.join(""), code: code.join(""), literals, errors };
}

/* ---------- ② config 路径源 ---------- */

/** 以 `/config` 结尾的字面量（模板字符串按**静态尾部**判）。 */
export function configLiteralsOf(literals) {
	return literals.filter((l) => l.tail.endsWith("/config"));
}

/** 是不是系统保留配置单例的路径（`…/system/config`）—— 本判据的唯一边界，见文件头 ⑥。 */
export function isSystemConfigLiteral(literal) {
	return literal.tail.endsWith(SYSTEM_CONFIG_SUFFIX);
}

/** 资源级 config 投影的字面量：末尾 `/config` 且**不是**明写的系统配置路径。 */
export function resourceConfigLiteralsOf(literals) {
	return configLiteralsOf(literals).filter((l) => !isSystemConfigLiteral(l));
}

/** 表达式（初始化器）结束位置：深度 0 的 `;` / 收尾括号 / ASI 换行。 */
export function findInitializerEnd(code, from) {
	let depth = 0;
	for (let i = from; i < code.length; i += 1) {
		const ch = code[i];
		if (ch === "(" || ch === "[" || ch === "{") depth += 1;
		else if (ch === ")" || ch === "]" || ch === "}") {
			if (depth === 0) return i;
			depth -= 1;
		} else if (ch === ";" && depth === 0) return i;
		else if (ch === "\n" && depth === 0) {
			const before = code.slice(from, i).trimEnd();
			const next = code.slice(i + 1).trimStart().slice(0, 1);
			if (!/[=+\-*/%&|?:,(\[{!<>]$/.test(before) && !/[.?)]/.test(next)) return i;
		}
	}
	return code.length;
}

/** `const|let|var NAME = <表达式里含资源级 /config 字面量>` → NAME 是 config 路径源。
 *  系统配置单例（`/v1/system/config`）不算源：它的写入口是允许的。 */
export function configSourcesOf(code, literals) {
	const configLiterals = resourceConfigLiteralsOf(literals);
	const out = new Map();
	for (const m of code.matchAll(DECL_RE)) {
		const name = m[1];
		const exprStart = m.index + m[0].length;
		const exprEnd = findInitializerEnd(code, exprStart);
		const hit = configLiterals.find(
			(l) => l.start >= exprStart && l.start < exprEnd,
		);
		if (hit && !out.has(name)) out.set(name, { at: m.index, literalStart: hit.start });
	}
	return out;
}

/* ---------- ③ 调用点 ---------- */

/** 从 `(` 开始切实参（括号配平；字符串内容已在 code 视图抹空，不会误计括号）。
 *  返回 { args, close }；括号没配平返回 null（调用点解析失败 ⇒ 退出码 2）。 */
export function splitArgs(code, openIdx) {
	let depth = 0;
	const args = [];
	let start = openIdx + 1;
	for (let i = openIdx; i < code.length; i += 1) {
		const ch = code[i];
		if (ch === "(" || ch === "[" || ch === "{") depth += 1;
		else if (ch === ")" || ch === "]" || ch === "}") {
			depth -= 1;
			if (depth === 0) {
				if (code.slice(start, i).trim() !== "" || args.length > 0) {
					args.push({ start, end: i });
				}
				return { args, close: i };
			}
			if (depth < 0) return null;
		} else if (ch === "," && depth === 1) {
			args.push({ start, end: i });
			start = i + 1;
		}
	}
	return null; // 括号没配平 → 调用点解析失败
}

function isWriteMethod(text) {
	const m = METHOD_LITERAL_RE.exec(text);
	return !!m && WRITE_METHODS.has(m[2].toUpperCase());
}

function escapeRe(s) {
	return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/** 第一个实参引用了某个 config 路径源（`NAME` / `NAME(…)` / `NAME + …`）。 */
export function firstArgReferencesSource(argText, sourceNames) {
	const t = argText.trim();
	for (const name of sourceNames) {
		const n = escapeRe(name);
		if (new RegExp(`^${n}$`).test(t)) return name;
		if (new RegExp(`^${n}\\s*\\(`).test(t)) return name;
		if (new RegExp(`^${n}\\s*\\+`).test(t)) return name;
	}
	return null;
}

/* ---------- 页面扫描（纯函数，--self-test 直接喂 fixture） ---------- */

/** 取出内联 <script> 块（`<script src>` 跳过），返回 [{src, at}]，at = 块在 html 里的偏移。 */
export function inlineScriptBlocks(html) {
	const out = [];
	let skipped = 0;
	for (const m of html.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script>/gi)) {
		const attrs = m[1] ?? "";
		if (/\bsrc\s*=/i.test(attrs)) {
			skipped += 1;
			continue;
		}
		const at = (m.index ?? 0) + m[0].indexOf(">") + 1;
		out.push({ src: m[2], at });
	}
	return { blocks: out, skipped };
}

export function scanPageHtml(name, html) {
	const violations = [];
	const errors = [];
	const { blocks, skipped } = inlineScriptBlocks(html);
	const views = blocks.map((b) => ({ ...b, ...tokenize(b.src) }));

	const hasWriteHelper = views.some(
		(v) => WRITE_HELPER_DECL_RE.test(v.code) || /function\s+write\s*\(/.test(v.code),
	);
	const sourceNames = new Map();
	let literalCount = 0;
	let resourceLiteralCount = 0;
	for (const v of views) {
		literalCount += configLiteralsOf(v.literals).length;
		resourceLiteralCount += resourceConfigLiteralsOf(v.literals).length;
		for (const [n, info] of configSourcesOf(v.code, v.literals)) {
			if (!sourceNames.has(n)) sourceNames.set(n, info);
		}
	}

	const stats = {
		scripts: views.length,
		skippedScripts: skipped,
		callSites: 0,
		writeCalls: 0,
		/** 命中⑥ 而**豁免**的系统配置写（只计数、不判红）。 */
		systemWrites: 0,
		configLiterals: resourceLiteralCount,
		systemConfigLiterals: literalCount - resourceLiteralCount,
		configSourceNames: sourceNames.size,
		configSources: resourceLiteralCount + sourceNames.size,
	};

	for (const v of views) {
		for (const e of v.errors) {
			errors.push({ file: name, ...e, line: lineOf(html, v.at + e.offset) });
		}
		const defParens = new Set();
		for (const m of v.code.matchAll(WRITE_HELPER_FN_RE)) {
			defParens.add(m.index + m[0].lastIndexOf("("));
		}
		const patterns = [
			{ re: /(?<![\w$.])write\s*\(/g, kind: "write-helper" },
			{ re: /\.api\s*\(/g, kind: "api" },
			{ re: /(?<![\w$.])fetch\s*\(/g, kind: "fetch" },
		];
		for (const { re, kind } of patterns) {
			for (const m of v.code.matchAll(re)) {
				const open = m.index + m[0].lastIndexOf("(");
				if (kind === "write-helper") {
					// 页面没定义 write helper 时，`write(...)` 不是本判据认识的写入口；
					// 定义处 `function write(` 的括号也不是调用点。
					if (!hasWriteHelper || defParens.has(open)) continue;
				}
				stats.callSites += 1;
				const split = splitArgs(v.code, open);
				if (!split) {
					errors.push({
						file: name,
						rule: RULE.UNBALANCED,
						line: lineOf(html, v.at + m.index),
						offset: m.index,
						message: "调用点括号没配平（找不到与之配对的右括号）—— 这个调用点没被检查",
					});
					continue;
				}
				const { args, close } = split;
				const argText = (idx) =>
					idx < args.length ? v.raw.slice(args[idx].start, args[idx].end) : "";

				if (kind === "write-helper") {
					// 显式第 3 参 'GET' ⇒ 读调用（node.html 的 helper 第 3 参就是 method）
					if (args.length >= 3 && /^(['"`])GET\1$/i.test(argText(2).trim())) continue;
				} else if (!isWriteMethod(argText(1))) {
					continue; // 没有写方法 ⇒ 读调用，判据不在此求值
				}

				stats.writeCalls += 1;
				const range = args[0];
				const first = range ? v.raw.slice(range.start, range.end) : "";
				const callText = v.raw.slice(m.index, close + 1).replace(/\s+/g, " ");
				const call = callText.length > 160 ? `${callText.slice(0, 157)}…` : callText;
				const literalsInArg = range
					? configLiteralsOf(v.literals).filter(
							(l) => l.start >= range.start && l.start < range.end,
						)
					: [];
				// ⑥ 例外面：第一个实参**只**指向系统保留配置单例（`…/system/config`）。
				// 它不是资源级投影，且是保留期的唯一写入口（system YAML 只读）——
				// 不判红，但必须计数（否则「豁免」与「没扫到」同形）。
				if (literalsInArg.length > 0 && literalsInArg.every(isSystemConfigLiteral)) {
					stats.systemWrites += 1;
					continue;
				}
				// 命中①：第一个实参里含以 /config 结尾的资源级字面量
				const literal = literalsInArg.find((l) => !isSystemConfigLiteral(l));
				if (literal) {
					violations.push({
						file: name,
						line: lineOf(html, v.at + literal.start),
						call,
						rule: RULE.LITERAL,
						why: `第一个实参含以 /config 结尾的字面量「${literal.tail}」`,
					});
					continue;
				}
				// 命中②：第一个实参引用了一个 config 路径源（等于 / NAME( / NAME +）
				const ref = firstArgReferencesSource(first, sourceNames.keys());
				if (ref) {
					violations.push({
						file: name,
						line: lineOf(html, v.at + m.index),
						call,
						rule: RULE.SOURCE,
						why: `第一个实参「${first.trim()}」引用了 config 路径源 \`${ref}\``,
					});
				}
			}
		}
	}
	return { violations, errors, stats, sourceNames: [...sourceNames.keys()] };
}

/* ---------- ⑤ 反假绿下限 ---------- */

export function judgeFloors(counts, floors = FLOORS) {
	const out = [];
	const push = (rule, label, have, want) =>
		out.push({
			rule,
			message: `${label} ${have}（下限 ${want}）—— 扫描面是空的，「绿」与「没检查」同形`,
		});
	if (counts.pages < floors.pages) push(RULE.NO_PAGES, "页面文件数", counts.pages, floors.pages);
	if (counts.scripts < floors.scripts)
		push(RULE.NO_SCRIPTS, "内联 script 块数", counts.scripts, floors.scripts);
	if (counts.writeCalls < floors.writeCalls)
		push(RULE.NO_WRITES, "写调用点数", counts.writeCalls, floors.writeCalls);
	if (counts.configSources < floors.configSources)
		push(RULE.NO_SOURCES, "config 路径源数", counts.configSources, floors.configSources);
	return out;
}

/* ---------- 主流程 ---------- */

/** 报告里用的文件名：相对 cwd（`web-src/pages/node.html`）；不在 cwd 下时相对扫描根并补回根名。 */
function labelOf(full) {
	const rel = path.relative(process.cwd(), full);
	const label = rel && !rel.startsWith("..") ? rel : path.join(path.basename(SRC), path.relative(SRC, full));
	return label.split(path.sep).join("/");
}

function printCounts(counts, sourceNames) {
	console.log(
		`   扫描面：页面 ${counts.pages} · 内联 script 块 ${counts.scripts}` +
			`（跳过 <script src> ${counts.skippedScripts}）· 写调用点 ${counts.writeCalls}` +
			`（候选调用 ${counts.callSites}）· config 路径源 ${counts.configSources}` +
			`（字面量 ${counts.configLiterals} + 命名源 ${counts.configSourceNames}）`,
	);
	if (sourceNames?.size) {
		console.log(`   命名源：${[...sourceNames].map((n) => `\`${n}\``).join(" ")}`);
	}
	console.log(
		`   例外（⑥ 系统保留配置）：识别为写调用并豁免 ${counts.systemWrites} 处` +
			`（system 字面量 ${counts.systemConfigLiterals} 个）—— 不判红，但不是没扫到`,
	);
	console.log(
		`   下限：页面 ${FLOORS.pages} · script 块 ${FLOORS.scripts} · 写调用点 ${FLOORS.writeCalls} · config 路径源 ${FLOORS.configSources}`,
	);
}

async function main() {
	const pageDir = path.join(SRC, "pages");
	let names;
	try {
		names = await fs.readdir(pageDir);
	} catch (e) {
		console.error(`✗ 读不到 ${pageDir}：${e.message}`);
		console.error("  —— 扫描面缺失时不许报绿（退出码 2）");
		process.exit(2);
	}
	const files = names.filter((n) => n.endsWith(".html")).sort();
	const counts = {
		pages: files.length,
		scripts: 0,
		skippedScripts: 0,
		callSites: 0,
		writeCalls: 0,
		systemWrites: 0,
		systemConfigLiterals: 0,
		configLiterals: 0,
		configSourceNames: 0,
		configSources: 0,
	};
	const violations = [];
	const errors = [];
	const allSourceNames = new Set();
	for (const n of files) {
		const full = path.join(pageDir, n);
		const html = await fs.readFile(full, "utf8");
		const r = scanPageHtml(labelOf(full), html);
		violations.push(...r.violations);
		errors.push(...r.errors);
		for (const s of r.sourceNames) allSourceNames.add(s);
		for (const k of [
			"scripts",
			"skippedScripts",
			"callSites",
			"writeCalls",
			"systemWrites",
			"systemConfigLiterals",
			"configLiterals",
			"configSourceNames",
			"configSources",
		]) {
			counts[k] += r.stats[k];
		}
	}
	const floorFails = judgeFloors(counts);

	if (errors.length) {
		console.error(`✗ 配置写路径守卫：解析失败 ${errors.length} 处（退出码 2，不静默跳过）`);
		for (const e of errors) console.error(`   ${e.file}:${e.line} → [${e.rule}] ${e.message}`);
		console.error("");
		printCounts(counts, allSourceNames);
		process.exit(2);
	}

	if (violations.length) {
		console.error(
			`✗ 配置写路径守卫：${violations.length} 处**写** /config 的调用（YAML 才是唯一编辑面，#476）`,
		);
		for (const v of violations) {
			console.error(`   ${v.file}:${v.line} → ${v.call} → ${v.rule}：${v.why}`);
		}
		console.error("");
		console.error("   处置：页面只读 /config（GET 投影），编辑走 YAML 面（PUT …/yaml 或表单 → yaml）；");
		console.error("   同一个事实不许有第二条写路径（docs/lint-guards.md §0 第 ⑤ 类缝隙）。");
		console.error("");
		printCounts(counts, allSourceNames);
		process.exit(1);
	}

	if (floorFails.length) {
		console.error(`✗ 配置写路径守卫：${floorFails.length} 项扫描面低于下限（反假绿）`);
		for (const f of floorFails) console.error(`   [${f.rule}] ${f.message}`);
		console.error("");
		printCounts(counts, allSourceNames);
		process.exit(1);
	}

	console.log("✅ 配置写路径守卫：页面对 /config 只有读、没有写");
	printCounts(counts);
}

/* ---------- --self-test：纯内存 fixture，红/绿双侧自证 ---------- */

const page = (js) => `<script>\n${js}\n</script>`;

/** 页面自己定义写 helper（node.html / namespace.html / admin.html 都是这个形状）。 */
const WRITE_HELPER = "const write = (url, body, method = 'PUT') => A.api(url, { method, body: JSON.stringify(body) });";
/** namespace.html:112 同形的 config 路径源。 */
const NS_PATH = "const nsPath = ns => '/v1/namespaces/' + ns + '/config';";
/** 绿用例的控制组：一条真实写调用，证明「写调用计数 > 0」不是因为漏扫。 */
const CONTROL_WRITE = "A.api('/v1/nodes/x/yaml', { method: 'PUT' }); // 控制组：真实写调用";

export function selfTestCaseList() {
	const writeHelper = WRITE_HELPER;
	const nsPath = NS_PATH;
	const controlWrite = CONTROL_WRITE;
	return [
		{
			name: "红① 源名调用：write(nsPath(x), {})",
			html: page(`${writeHelper}\n${nsPath}\nwrite(nsPath(x), {});`),
			wantRules: [RULE.SOURCE],
			wantWriteCalls: 1,
		},
		{
			name: "红② 字面量在第一个实参里：A.api('…/config', {method:'PUT'})",
			html: page("A.api('/v1/nodes/' + n + '/config', { method: 'PUT', body: '{}' });"),
			wantRules: [RULE.LITERAL],
		},
		{
			name: "红③ 源名等于：const p = '…/config'; A.api(p, {method:'DELETE'})",
			html: page("const p = '/v1/x/' + id + '/config'; A.api(p, {method:'DELETE'});"),
			wantRules: [RULE.SOURCE],
		},
		{
			name: "红④ fetch + PATCH",
			html: page("fetch('/v1/nodes/x/config', {method:'PATCH'});"),
			wantRules: [RULE.LITERAL],
		},
		{
			name: "红⑤ 页面自己的 write helper 直接拼 /config（node.html:550 同形）",
			html: page(`${writeHelper}\nwrite(path + '/config', body);`),
			wantRules: [RULE.LITERAL],
			wantWriteCalls: 1,
		},
		{
			name: "红⑥ 模板字符串尾以 /config 结尾 ⇒ 是 config 路径源",
			html: page("const ns = n => `/v1/namespaces/${n}/config`;\nA.api(ns(x), { method: 'PUT' });"),
			wantRules: [RULE.SOURCE],
			wantWriteCalls: 1,
		},
		{
			name: "红⑦ URL 里的 // 不是注释（否则后半行被吞 ⇒ 假绿）",
			html: page("A.api('https://hub.example/v1/nodes/x/config', { method: 'PUT' });"),
			wantRules: [RULE.LITERAL],
			wantWriteCalls: 1,
		},
		{
			name: "红⑧ 写 helper 定义在另一个 script 块里也算数（页面级判据）",
			html: `<script>\n${writeHelper}\n</script>\n<script>\nwrite(path + '/config', body);\n</script>`,
			wantRules: [RULE.LITERAL],
			wantWriteCalls: 1,
			wantScripts: 2,
		},
		{
			name: "绿① GET 读：A.api(nsPath(x))（无 method ⇒ 不是写调用）",
			html: page(`${writeHelper}\n${nsPath}\nA.api(nsPath(x));\n${controlWrite}`),
			wantRules: [],
			wantWriteCalls: 1,
		},
		{
			name: "绿② 显式 method:'GET' 读 /config",
			html: page(`${writeHelper}\n${nsPath}\nA.api(nsPath(x), { method: 'GET' });\n${controlWrite}`),
			wantRules: [],
			wantWriteCalls: 1,
		},
		{
			name: "绿③ path + '/yaml' 的 PUT（path 不是 config 路径源）",
			html: page("A.api(path + '/yaml', { method: 'PUT' });"),
			wantRules: [],
			wantWriteCalls: 1,
		},
		{
			name: "绿④ write(path + '/yaml', body)",
			html: page(`${writeHelper}\nwrite(path + '/yaml', body);`),
			wantRules: [],
			wantWriteCalls: 1,
		},
		{
			name: "绿⑤ 显式第 3 参 'GET' ⇒ 这个 write(...) 不算写",
			html: page(`${writeHelper}\n${nsPath}\nwrite(nsPath(x), {}, 'GET');\n${controlWrite}`),
			wantRules: [],
			wantWriteCalls: 1,
		},
		{
			name: "绿⑥ 注释里的 /config 写调用不算（行注释 + 块注释）",
			html: page(
				`// write(path + '/config', body) —— 注释里提到不算\n/* A.api('/v1/nodes/x/config', { method: 'PUT' }) */\n${controlWrite}`,
			),
			wantRules: [],
			wantWriteCalls: 1,
		},
		{
			name: "绿⑦ 模板路径 + '/yaml' 不是 config 源",
			html: page("const u = `/v1/nodes/${n}/yaml`;\nA.api(u, { method: 'PUT' });"),
			wantRules: [],
			wantWriteCalls: 1,
		},
		{
			name: "绿⑧ <script src> 块跳过（不算扫描面、也不算违规）",
			html: `<script src="/partials.js"></script>\n<script defer src="/graphs.js"></script>`,
			wantRules: [],
			wantScripts: 0,
			wantSkippedScripts: 2,
			wantWriteCalls: 0,
		},
		{
			// ⑥ 例外面：系统保留期单个配置——唯一写入口（system YAML 只读），不属 #476
			name: "绿⑨ 系统保留期 write('/v1/system/config', body) 豁免（admin.html:1507 同形）",
			html: page(`${writeHelper}\nwrite('/v1/system/config', body);`),
			wantRules: [],
			wantWriteCalls: 1,
			wantSystemWrites: 1,
		},
		{
			name: "绿⑩ 系统配置路径经 helper（const sysPath = () => '/v1/system/config'）不建 config 源",
			html: page(
				`${writeHelper}\nconst sysPath = () => '/v1/system/config';\nwrite(sysPath(), body);`,
			),
			wantRules: [],
			wantWriteCalls: 1,
		},
		{
			// 反例对照：例外只认「末尾就是 /system/config」这一个形状 ——
			// 路过的 /system/ 不算（否则一个 contains 判断就能把任何写都放过去）
			name: "红⑨ 例外不溢：'/v1/system/nodes/x/config' 仍判红（例外是后缀精确匹配）",
			html: page(`${writeHelper}\nwrite('/v1/system/nodes/x/config', body);`),
			wantRules: [RULE.LITERAL],
			wantWriteCalls: 1,
			wantSystemWrites: 0,
		},
		{
			name: "解析错误：括号没配平 ⇒ errors 里点名（退出码 2，不静默跳过）",
			html: page("A.api('/v1/x/config', { method: 'PUT'"),
			wantRules: [],
			wantErrorRule: RULE.UNBALANCED,
		},
		{
			name: "解析错误：字符串没闭合 ⇒ errors 里点名",
			html: page("A.api('/v1/x/config, { method: 'PUT' });"),
			wantRules: [],
			wantErrorRule: RULE.UNTERMINATED,
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

	for (const c of selfTestCaseList()) {
		const r = scanPageHtml("fixture.html", c.html);
		const gotRules = r.violations.map((v) => v.rule).sort();
		const wantRules = [...c.wantRules].sort();
		const same = JSON.stringify(gotRules) === JSON.stringify(wantRules);
		const detail = same
			? ""
			: `want ${JSON.stringify(wantRules)}, got ${JSON.stringify(gotRules)}` +
				(r.violations.length
					? ` (${r.violations.map((v) => `${v.line}:${v.call}`).join(" | ")})`
					: "");
		test(c.name, same, detail);

		if (c.wantWriteCalls !== undefined) {
			test(
				`  ↳ 写调用计数 = ${c.wantWriteCalls}`,
				r.stats.writeCalls === c.wantWriteCalls,
				`got ${r.stats.writeCalls}`,
			);
		}
		if (c.wantSystemWrites !== undefined) {
			test(
				`  ↳ 系统配置豁免计数 = ${c.wantSystemWrites}`,
				r.stats.systemWrites === c.wantSystemWrites,
				`got ${r.stats.systemWrites}`,
			);
		}
		if (c.wantScripts !== undefined) {
			test(
				`  ↳ script 块数 = ${c.wantScripts}`,
				r.stats.scripts === c.wantScripts,
				`got ${r.stats.scripts}`,
			);
		}
		if (c.wantSkippedScripts !== undefined) {
			test(
				`  ↳ 跳过 <script src> = ${c.wantSkippedScripts}`,
				r.stats.skippedScripts === c.wantSkippedScripts,
				`got ${r.stats.skippedScripts}`,
			);
		}
		if (c.wantErrorRule) {
			const got = r.errors.map((e) => e.rule);
			test(
				`  ↳ 解析错误点名 ${c.wantErrorRule}`,
				got.includes(c.wantErrorRule),
				`got ${JSON.stringify(got)}`,
			);
		}
		// 反假绿：每条绿用例的扫描面必须非空（否则「没扫到」也会绿）
		if (c.wantRules.length === 0 && c.wantWriteCalls === undefined && c.wantErrorRule === undefined) {
			test(
				"  ↳ 绿用例的扫描面非空（script 块 > 0）",
				r.stats.scripts > 0,
				`scripts=${r.stats.scripts}`,
			);
		}
	}

	// 行号必须指向真实位置（不是一句「某处」）
	const lineCase = scanPageHtml(
		"fixture.html",
		`<script>\n${WRITE_HELPER}\nwrite(path + '/config', body);\n</script>`,
	);
	test(
		"行号指向真实行（第 3 行）",
		lineCase.violations[0]?.line === 3,
		JSON.stringify(lineCase.violations.map((v) => v.line)),
	);

	// 反假绿下限本身：四项全 0 ⇒ 四条规则全点名
	const zero = judgeFloors({
		pages: 0,
		scripts: 0,
		writeCalls: 0,
		configSources: 0,
	});
	test(
		"四项计数全 0 ⇒ 下限判红并逐项点名",
		zero.length === 4 &&
			[RULE.NO_PAGES, RULE.NO_SCRIPTS, RULE.NO_WRITES, RULE.NO_SOURCES].every((r) =>
				zero.some((f) => f.rule === r),
			),
		JSON.stringify(zero.map((f) => f.rule)),
	);
	test(
		"计数达标 ⇒ 下限不判红",
		judgeFloors({ pages: 1, scripts: 1, writeCalls: 1, configSources: 1 }).length === 0,
	);

	// 变异自证：把一条绿 fixture 的 '/yaml' 改成 '/config' ⇒ 必须转红（判据有牙）
	const mutated = scanPageHtml(
		"fixture.html",
		page(`${WRITE_HELPER}\nA.api(path + '/config', { method: 'PUT' });`),
	);
	test(
		"变异：'/yaml' → '/config' 必须转红（LITERAL）",
		mutated.violations.length === 1 && mutated.violations[0].rule === RULE.LITERAL,
		JSON.stringify(mutated.violations.map((v) => v.rule)),
	);
	// 变异自证：绿① 的 GET 加上 method:'PUT' ⇒ 必须转红
	const mutated2 = scanPageHtml(
		"fixture.html",
		page(`${WRITE_HELPER}\n${NS_PATH}\nA.api(nsPath(x), { method: 'PUT' });`),
	);
	test(
		"变异：GET → PUT 必须转红（SOURCE）",
		mutated2.violations.length === 1 && mutated2.violations[0].rule === RULE.SOURCE,
		JSON.stringify(mutated2.violations.map((v) => v.rule)),
	);

	console.log(
		failed === 0
			? `✅ self-test ${cases}/${cases} 通过`
			: `✗ self-test ${cases - failed}/${cases} 通过`,
	);
	return failed === 0;
}

const isDirectRun =
	process.argv[1] &&
	path.resolve(process.argv[1]) === path.resolve(fileURLToPath(import.meta.url));
if (isDirectRun) {
	const argv = process.argv.slice(2);
	const known = new Set(["--self-test"]);
	for (const a of argv) {
		if (a.startsWith("--src=") || known.has(a)) continue;
		console.error(`✗ 不认识的参数：${a}`);
		console.error("用法：node scripts/check-config-writes.mjs [--src=web-src] [--self-test]");
		process.exit(2);
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
