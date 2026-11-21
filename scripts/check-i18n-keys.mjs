#!/usr/bin/env node
/* Anotify · i18n 键完整性守卫（docs/lint-guards.md 的 L2）
 *
 * 拦的事故
 *   ① `8e253d4e`：补 `uxaudit.res.node.none` 的四语言词条 —— 页面上先出现了裸 key。
 *   ② `7dc2c260`：en/es 页面残留中文 —— 某个 key 只在部分语言里有译文。
 *   根因同一个：`internal/sitegen/sitegen.go:356,370`（makeTFunc）遇到缺 key 只
 *   `log.Printf("WARN missing i18n key …")` 然后回退到默认语言 / 裸 key，**构建不失败**。
 *   于是「少一条译文」这种结构性缺陷只能靠人眼在某个语言的页面上撞见。
 *   本守卫把它变成构建期事实：make fe → sitegen 之前先跑。
 *
 * 判据（全部纯函数，可 --self-test 自证红/绿两侧）
 *   a. 源码里引用的每个 key，必须在 4 份 locale 里都存在（逐语言报缺，带 文件:行）。
 *   b. 4 份 locale 的 key 集合必须两两相等（报每个语言的差集；未被引用的 key 也算）。
 *   c. 没有被任何源码引用的 key **只 warn 不失败**，并打印数量（多半是死词条，可清理）。
 *   d. 反假绿：打印 解析文件数 / key 总数 / 引用点总数，且三者都有下限（见 FLOORS）；
 *      解析出 0 个 key 或 0 个引用点一律红 —— 「绿」与「没检查」不能同形。
 *   e. 解析必须 fail-loud：locale YAML 里出现本脚本不认识的语法 → 退出码 2，
 *      绝不静默跳过（`check-wire-fields.mjs:62` 那种 `catch { continue }` 是本仓反复教训）。
 *
 * 支持的引用形态（2026-11 全仓 grep 实测，不是猜的）
 *   Go 模板函数（`internal/sitegen/template.go:51-53,103-108` 注册 t / tHTML）：
 *     {{t "common.close"}}      {{- t "a.b" -}}      {{tHTML "docs.xxx"}}
 *   JS 调用（页面内联 <script> 与 web/*.js 运行期源）：
 *     t("a.b")            t('a.b')              t("a.b", "兜底文案")   ← 第二参是兜底，不是 key
 *     t(c ? "a.b" : "a.c")  三元两支都算引用
 *     A.t("a.b")  Anotify.t("a.b")  options.t(...)                     ← 成员形式（任意接收者）
 *     sharedT("a.b")      ← `const { t: sharedT } = Anotify` 的别名
 *     tt("a.b")           ← web/flograph.js 里 `const tt = (k, d) => …` 的本地包装
 *     section("a.b")      ← capabilities.html 的 `function section(titleKey, …)`，首参就是 key
 *   「前缀包装器」页（如 node/nodes/sources 等页，实测 11 处定义）：
 *     const t = key => A.t("nodes.v4." + key);   ← 这种页里 t("config") 的真实 key 是
 *                                                    nodes.v4.config；脚本按文件级 wrapper 推断还原
 *   预防性形态：`data-i18n="a.b"`（当前仓库 0 处；支持它是为了将来有人这么写时不被静默漏检）
 *
 * 静态不可判的形态 → 只计数、不判红（打印数量，避免「以为查了其实没查」的错觉）
 *     t(k) / t(key + "_confirm") / t("a." + suffix) / t(cond ? 'online' : 'offline')
 *     / {{t (printf "docs.openapi.tag.%s" .Tag)}} / {{t .X}}
 *   `t("前缀." + x)` 这类拼接会额外核对「该命名空间在 4 份 locale 里都非空」（warn 级）。
 *   纯数字实参（web/mock.js 的 t(620)）是时间戳助手，同名不同义 ⇒ 不算引用。
 *
 * 用法
 *   node scripts/check-i18n-keys.mjs                 # 判据校验，违规退出 1
 *   node scripts/check-i18n-keys.mjs --self-test     # 跑脚本自身的红/绿自测（纯内存 fixture）
 *   node scripts/check-i18n-keys.mjs --src=web-src --static=web
 *   退出码：0 绿 / 1 违规 / 2 解析失败或用法错误（含不支持的 YAML 语法）
 *   接线：`make fe`（docs/lint-guards.md 的 L2；Makefile 由主 agent 统一收口，本脚本不自注册）
 *
 * 为什么可以信任这个手写解析器：已与构建期的真实路径对账 ——
 *   `sitegen.LoadTranslations`（yaml.v3 + i18n.go 的 flatten）导出的 key 集合，
 *   与本脚本对 4 份 locale 的解析结果逐一 diff 完全一致（2026-11：每份 2142 个 key）。
 *   即「判据里的 key 集合」与「构建时真正能查到的 key 集合」是同一集合。
 *
 * locale YAML 支持范围（最小缩进解析器，纯 stdlib）
 *   支持：`key: value` 、`key:` 嵌套映射、`#` 整行注释与行尾注释、单/双引号标量、
 *        跨行引号标量（YAML 折叠，如 `cmd_a`）、引号键名（如 `"on":`）、空值（`key:` 无子项 ⇒ ""，
 *        与 `sitegen/i18n.go` 的 flatten 把 nil 映射成 "" 一致）、行首缩进空白。
 *   不支持 ⇒ 退出 2：块标量 `|` `>`、锚点/别名 `&` `*`、流式集合 `[ ]` `{ }`、序列 `- item`、
 *        多文档 `---`、tab 缩进、重复键（扁平平铺后重名也算）、键名含非法字符、
 *        引号标量闭合后有多余内容、缩进比上一行深但上一行不是映射头。
 *   边界：只做「键集合」与「引用」的对账，**不判译文质量**（en 里残留中文仍靠 review）；
 *        值只用于正确跳过跨行，不参与任何断言。
 */
import { promises as fs } from "node:fs";
import path from "node:path";

import { runsAsMain } from "./lib/direct-run.mjs";

/** 站点支持的 4 种语言（与 `make sitegen -langs zh-CN,en,ja,es` 同序）。 */
export const LANGS = ["zh-CN", "en", "ja", "es"];

/** 模板/源码扫描范围（相对仓库根）。 */
const TEMPLATE_DIRS = ["layouts", "pages"]; // web-src/{layouts,pages}/*.html
const STATIC_JSDIR = "web"; // 运行期静态 JS 源（sitegen 只生成 web/*.html 与 web/i18n.*.js）
const GENERATED_JS = /^i18n\.[A-Za-z-]+\.js$/; // 由 sitegen 生成，跳过

/**
 * 反假绿下限（棘轮）：2026-11 在 main 上实测的值。低于它 = 扫描面被改窄了（假绿），红。
 * 合理缩减（删词条/删页面）时**必须同步下调这里**，否则门禁会一直红。
 *   files = web-src/{layouts,pages}/*.html + web/*.js（排除生成的 i18n.*.js）
 *   keys  = 4 份 locale 的 key 并集
 *   refs  = 静态可判的 key 引用点
 */
const FLOORS = {
	files: 28,
	keys: 2142,
	refs: 1861,
};

/** 扁平 key 的字面量形状：至少一个点（locale 顶层全是映射，扁平后必带点）。 */
const KEY_LIKE = /^[A-Za-z0-9_][A-Za-z0-9_-]*(\.[A-Za-z0-9_-]+)+$/;

/** 「前缀包装器」上下文里单个 key 叶子的形状（不含空格/特殊字符；带点也合法）。 */
const LEAF_LIKE = /^[A-Za-z0-9_][A-Za-z0-9_.-]*$/;

/** 纯数字实参：`web/mock.js` 的 `t(620)` 是时间戳助手（同名不同义），不是 key。 */
const NUMERIC_ARG = /^\d+(?:\.\d+)?$/;

/** 单个键名段允许的字符（拒绝空格/冒号/点/引号等会破坏扁平化的字符）。 */
const KEY_SEG = /^[A-Za-z0-9_][A-Za-z0-9_-]*$/;

/** 裸标识符形式的翻译函数（本轮 grep 实测到的全部）。 */
const BARE_TRANSLATORS = new Set(["t", "tt", "tHTML", "sharedT", "section"]);

/** 不支持的 YAML 语法 —— 一律抛 ParseError（exit 2），绝不静默跳过。 */
export class ParseError extends Error {
	constructor(message) {
		super(message);
		this.name = "ParseError";
	}
}

/** 行号查询：把字符偏移映射成 1 起的行号。 */
function lineLookup(text) {
	const starts = [0];
	for (let i = 0; i < text.length; i++) if (text[i] === "\n") starts.push(i + 1);
	return (offset) => {
		let lo = 0;
		let hi = starts.length - 1;
		while (lo < hi) {
			const mid = (lo + hi + 1) >> 1;
			if (starts[mid] <= offset) lo = mid;
			else hi = mid - 1;
		}
		return lo + 1;
	};
}

/**
 * 读一个引号标量：返回 { value, end }（end = 闭合引号之后的下标），未闭合返回 null。
 * 单引号里的 `''` 是转义的单引号；双引号里 `\X` 是转义序列。
 */
function scanQuoted(text, from, quote) {
	let value = "";
	for (let i = from + 1; i < text.length; i++) {
		const c = text[i];
		if (quote === "'") {
			if (c === "'") {
				if (text[i + 1] === "'") {
					value += "'";
					i += 1;
					continue;
				}
				return { value, end: i + 1 };
			}
			value += c;
			continue;
		}
		if (c === "\\") {
			value += text[i + 1] ?? "";
			i += 1;
			continue;
		}
		if (c === quote) return { value, end: i + 1 };
		value += c;
	}
	return null;
}

/**
 * 最小缩进 YAML 解析器：把 locale 文件扁平成 `a.b.c` → { line, value }。
 * 只认 key 集合（值仅用于正确跳过跨行/引号），不认识的语法抛 ParseError。
 */
export function parseLocale(text, file = "<memory>") {
	const lines = text.split("\n");
	/** @type {Map<string, {line:number, value:string}>} */
	const entries = new Map();
	/** 映射头栈：{ indent, seg, line, flat, hasChild } */
	const frames = [];
	const fail = (line, msg) => {
		throw new ParseError(`${file}:${line} 不支持的 YAML 语法：${msg}`);
	};
	const addLeaf = (frame) => {
		// 映射头没有子项 ⇒ 空值叶子（与 sitegen/i18n.go 的 flatten：nil → "" 一致）。
		if (frame.hasChild) return;
		if (entries.has(frame.flat)) {
			fail(frame.line, `重复键 "${frame.flat}"（首次出现在第 ${entries.get(frame.flat).line} 行）`);
		}
		entries.set(frame.flat, { line: frame.line, value: "" });
	};

	let prevIndent = -1;
	let prevWasHeader = true;
	let i = 0;
	while (i < lines.length) {
		const lineNo = i + 1;
		const raw = lines[i];
		i += 1;
		if (raw.trim() === "") continue; // 空行：不改变缩进上下文
		if (raw.startsWith("\t") || /^ +\t/.test(raw)) fail(lineNo, "缩进使用了 tab（只允许空格）");
		const body = raw.replace(/^ +/, "");
		const indent = raw.length - body.length;
		if (body.startsWith("#")) continue; // 整行注释
		if (body === "---" || body === "..." || body.startsWith("--- ") || body.startsWith("... ")) {
			fail(lineNo, `多文档标记「${body.slice(0, 12)}」（locale 只能单文档）`);
		}
		if (body === "-" || body.startsWith("- ") || body.startsWith("-\t")) {
			fail(lineNo, "序列（`- item`）不支持，locale 只允许 key: value 嵌套映射");
		}
		if (indent > prevIndent && !prevWasHeader) {
			fail(lineNo, "缩进比上一行更深，但上一行是标量值（YAML 不允许）");
		}
		// 出栈：缩进回到某层祖先时，把没有子项的映射头补成空值叶子。
		while (frames.length && frames[frames.length - 1].indent >= indent) addLeaf(frames.pop());

		// —— 键名 ——
		let key;
		let rest;
		if (body[0] === '"' || body[0] === "'") {
			const q = scanQuoted(body, 0, body[0]);
			if (!q) fail(lineNo, "引号键名没有在同一行闭合");
			key = q.value;
			let j = q.end;
			while (body[j] === " ") j += 1;
			if (body[j] !== ":") fail(lineNo, `引号键名后缺少 ":"：${body.slice(0, 40)}`);
			rest = body.slice(j + 1);
		} else {
			const idx = body.indexOf(":");
			if (idx < 1) fail(lineNo, `不是 "key: value" 形态：${body.slice(0, 40)}`);
			key = body.slice(0, idx).trim();
			rest = body.slice(idx + 1);
		}
		if (!KEY_SEG.test(key)) {
			fail(lineNo, `键名「${key}」含不支持的字符（只允许字母/数字/_/-，且不能带点）`);
		}

		// —— 值 ——
		let v = rest.replace(/^[ \t]+/, "");
		let header = false;
		let value = null;
		if (v === "" || v.startsWith("#")) {
			header = true;
		} else if (v[0] === "|" || v[0] === ">") {
			fail(lineNo, `块标量「${v[0]}」不支持（值请单行或引号字符串）`);
		} else if (v[0] === "&" || v[0] === "*") {
			fail(lineNo, "锚点/别名（& / *）不支持");
		} else if (v[0] === "[" || v[0] === "{") {
			fail(lineNo, "流式集合（[ ] / { }）不支持");
		} else if (v[0] === "?" || v[0] === "!") {
			fail(lineNo, `复杂键 / 标签（${v[0]}）不支持`);
		} else if (v[0] === '"' || v[0] === "'") {
			const q = v[0];
			let buf = v;
			let closed = scanQuoted(buf, 0, q);
			let consumed = 0;
			while (!closed && i < lines.length) {
				buf += `\n${lines[i]}`;
				i += 1;
				consumed += 1;
				closed = scanQuoted(buf, 0, q);
			}
			if (!closed) fail(lineNo, "引号字符串到文件结束都没闭合");
			const after = buf.slice(closed.end).trim();
			if (after !== "" && !after.startsWith("#")) {
				fail(lineNo + consumed, `引号字符串闭合后还有多余内容：「${after.slice(0, 30)}」`);
			}
			value = closed.value;
		} else {
			// 纯标量：行尾注释（前面必须有空白）截断；出现 ": " 说明是没加引号的嵌套映射，静态不可解析。
			const cut = v.search(/[ \t]#/);
			if (cut >= 0) v = v.slice(0, cut).trimEnd();
			if (v.includes(": ")) fail(lineNo, `纯标量里出现 ": "（会被 YAML 当成嵌套映射）：${v.slice(0, 40)}`);
			value = v;
		}

		const segs = frames.map((f) => f.seg);
		const parent = frames[frames.length - 1];
		if (parent) parent.hasChild = true;
		const flat = [...segs, key].join(".");
		if (header) {
			frames.push({ indent, seg: key, line: lineNo, flat, hasChild: false });
		} else {
			if (entries.has(flat)) {
				fail(lineNo, `重复键 "${flat}"（首次出现在第 ${entries.get(flat).line} 行）`);
			}
			entries.set(flat, { line: lineNo, value });
		}
		prevIndent = indent;
		prevWasHeader = header;
	}
	while (frames.length) addLeaf(frames.pop());
	return { file, entries };
}

/**
 * 读一个 JS 调用的实参列表（顶层逗号切分），返回 { args, end }。
 * end = 右括号下标；未闭合返回 -1（调用跨出扫描窗口）。
 */
function readArgs(text, openIdx) {
	let depth = 0;
	let quote = null;
	let start = openIdx + 1;
	const args = [];
	for (let i = openIdx; i < text.length; i++) {
		const c = text[i];
		if (quote) {
			if (quote !== "`" && c === "\\") {
				i += 1;
				continue;
			}
			if (c === quote) quote = null;
			continue;
		}
		if (c === '"' || c === "'" || c === "`") {
			quote = c;
			continue;
		}
		if (c === "(" || c === "[" || c === "{") {
			depth += 1;
			continue;
		}
		if (c === ")" || c === "]" || c === "}") {
			depth -= 1;
			if (depth === 0) {
				args.push(text.slice(start, i));
				return { args, end: i };
			}
			continue;
		}
		if (c === "," && depth === 1) {
			args.push(text.slice(start, i));
			start = i + 1;
		}
	}
	return { args, end: -1 };
}

/** 收集一段文本里的所有引号字符串字面量（不含模板字符串）。 */
function literalsIn(arg) {
	const out = [];
	for (let i = 0; i < arg.length; i++) {
		const q = arg[i];
		if (q !== '"' && q !== "'") continue;
		const got = scanQuoted(arg, i, q);
		if (!got) break;
		out.push({ value: got.value, end: got.end });
		i = got.end - 1;
	}
	return out;
}

/**
 * 文件级「前缀包装器」：`const t = key => A.t("nodes.v4." + key)`。
 * 实测 11 处（node/nodes/source/sources 四页）：这类页里的 `t("config")` 真实 key 是
 * `nodes.v4.config`，不还原就会全落成「非扁平字面量」而静默漏检。返回 { 函数名 → 前缀 }。
 */
function wrapperPrefixes(source) {
	const out = new Map();
	const re =
		/([A-Za-z_$][\w$]*)\s*=\s*(?:\(?\s*([A-Za-z_$][\w$]*)\s*\)?\s*=>\s*)?[A-Za-z_$][\w$.]*\.t\(\s*(['"])([^'"%]+)\3\s*\+\s*\2\s*\)/g;
	for (const m of source.matchAll(re)) out.set(m[1], m[4]);
	return out;
}

/**
 * 本文件的 `section(key)` 是不是转发到本文件的 `t()`（capability.html 实测如此）。
 * 决定了 `section("k")` 的 key 要不要跟着 `t` 的前缀包装器走。
 */
function helperForwardsToT(source) {
	return /function\s+section\s*\([^)]*\)\s*\{[\s\S]{0,600}?\bt\(/.test(source);
}

/**
 * 扫一份源码，抽出 i18n key 引用。纯函数（自测直接喂 fixture）。
 * 返回 { refs, dynamic, loose }
 *   refs    = 静态可判的字面量 key（要参与「必须存在」判据）
 *   dynamic = 静态不可判的引用（只计数 + 报形态）
 *   loose   = t() 首参里的字面量但不长成 key 形状（多半是「渲染裸标签」的潜在裸 key，仅提示）
 */
export function extractRefs(source, file = "<memory>") {
	const at = lineLookup(source);
	const refs = [];
	const dynamic = [];
	const loose = [];
	const wrappers = wrapperPrefixes(source);
	// `section(key)` 转发到本文件 `t`：若 `t` 带前缀，section 的 key 也得带上。
	const sectionPrefix = helperForwardsToT(source) ? (wrappers.get("t") ?? null) : null;
	// Go 模板区间：`{{t (printf …)}}` 这种不是 JS 调用，留给下面的模板扫描，不重复计。
	const spans = [...source.matchAll(/\{\{[\s\S]*?\}\}/g)].map((m) => [m.index, m.index + m[0].length]);
	const inTemplate = (idx) => spans.some(([s, e]) => idx >= s && idx < e);

	const classifyArg = (rawArg, line, form, prefix = null) => {
		const arg = rawArg.trim();
		const asKey = (lit) => (prefix ? prefix + lit : lit);
		const isKey = (lit) => (prefix ? LEAF_LIKE.test(lit) : KEY_LIKE.test(lit));
		if (arg === "") {
			dynamic.push({ file, line, raw: "", prefix: null, form });
			return;
		}
		// 纯数字实参不是 key（`web/mock.js` 的 `t(620)` 是时间戳助手，同名不同义）⇒ 不计。
		if (NUMERIC_ARG.test(arg)) return;
		const lits = literalsIn(arg);
		const headLiteral = lits.length > 0 && (arg[0] === '"' || arg[0] === "'");
		// ① `"前缀." + x` 动态拼接：记下前缀命名空间（可核对它非空）。
		if (headLiteral) {
			const after = arg.slice(lits[0].end).trim();
			if (after.startsWith("+")) {
				dynamic.push({ file, line, raw: arg.slice(0, 60), prefix: asKey(lits[0].value), form });
				return;
			}
		}
		// ② `key + "_confirm"` 这类后缀/中缀拼接：静态不可判（`_confirm` 不是独立 key）。
		if (!headLiteral && /\+/.test(arg)) {
			dynamic.push({ file, line, raw: arg.slice(0, 60), prefix: null, form });
			return;
		}
		// ③ 纯字面量 / 三元两支：都算引用。
		const keys = lits.filter((l) => isKey(l.value));
		if (keys.length) {
			for (const l of keys) refs.push({ file, line, key: asKey(l.value), form });
			return;
		}
		if (lits.length) {
			// 有字面量但不是 key 形状：运行时真的会去查一个不存在的扁平 key，提示但不判红。
			loose.push({ file, line, raw: arg.slice(0, 60) });
			return;
		}
		dynamic.push({ file, line, raw: arg.slice(0, 60), prefix: null, form });
	};

	// ── JS 调用 ──────────────────────────────────────────────
	const bareRe = /(?<![\w$.])(t|tt|tHTML|sharedT|section)\s*\(/g;
	for (const m of source.matchAll(bareRe)) {
		const id = m[1];
		if (!BARE_TRANSLATORS.has(id) || inTemplate(m.index)) continue;
		const before = source.slice(Math.max(0, m.index - 12), m.index);
		if (/\bfunction\s*$/.test(before)) continue; // 函数定义，不是调用
		const open = m.index + m[0].length - 1;
		const { args, end } = readArgs(source, open);
		if (end < 0) {
			dynamic.push({ file, line: at(m.index), raw: "", prefix: null, form: `${id}()` });
			continue;
		}
		classifyArg(args[0] ?? "", at(m.index), `${id}(...)`, wrappers.get(id) ?? (id === "section" ? sectionPrefix : null));
	}
	const memberRe = /([A-Za-z_$][\w$]*)\.(t|tHTML)\s*\(/g;
	for (const m of source.matchAll(memberRe)) {
		if (inTemplate(m.index)) continue;
		const open = m.index + m[0].length - 1;
		const { args, end } = readArgs(source, open);
		if (end < 0) {
			dynamic.push({ file, line: at(m.index), raw: "", prefix: null, form: `${m[1]}.${m[2]}()` });
			continue;
		}
		classifyArg(args[0] ?? "", at(m.index), `${m[1]}.${m[2]}(...)`);
	}

	// ── data-i18n="key"（当前 0 处，预防性支持） ──────────────
	for (const m of source.matchAll(/data-i18n\s*=\s*("([^"]*)"|'([^']*)')/g)) {
		const key = m[2] ?? m[3] ?? "";
		if (KEY_LIKE.test(key)) refs.push({ file, line: at(m.index), key, form: "data-i18n" });
		else loose.push({ file, line: at(m.index), raw: `data-i18n="${key}"` });
	}

	// ── Go 模板：{{t "k"}} / {{tHTML "k"}} / {{t (printf "prefix.%s" .X)}} ──
	for (const span of source.matchAll(/\{\{[\s\S]*?\}\}/g)) {
		const inner = span[0].slice(2, -2);
		const base = span.index + 2;
		for (const call of inner.matchAll(/(?<![\w$.])(t|tHTML)\s+/g)) {
			const tail = inner.slice(call.index + call[0].length);
			const line = at(base + call.index);
			const form = `{{${call[1]} …}}`;
			const q = tail[0];
			if (q === '"' || q === "'") {
				const got = scanQuoted(tail, 0, q);
				if (!got) {
					dynamic.push({ file, line, raw: tail.slice(0, 40), prefix: null, form });
					continue;
				}
				if (KEY_LIKE.test(got.value)) refs.push({ file, line, key: got.value, form });
				else loose.push({ file, line, raw: tail.slice(0, 40) });
				continue;
			}
			// 动态：{{t (printf "docs.openapi.tag.%s" .Tag)}} → 取 printf 的字面前缀。
			const pre = /\(?\s*printf\s+(["'])([^"']*%[svd])\1/.exec(tail);
			dynamic.push({
				file,
				line,
				raw: tail.slice(0, 40),
				prefix: pre ? pre[2].slice(0, pre[2].indexOf("%")) : null,
				form,
			});
		}
	}
	return { refs, dynamic, loose };
}

/**
 * 对账：key 存在性 + 4 份 locale 集合相等 + 未引用告警 + 反假绿下限。
 * 纯函数（自测直接喂 fixture）。locales = { lang: Map<flatKey, {line}> }。
 * localeDir 只用于把「哪份 locale 缺 key」显示成可点的路径（默认 web-src/locales）。
 */
export function analyze({
	locales,
	refs,
	dynamic = [],
	loose = [],
	files = 0,
	floors = FLOORS,
	localeDir = "web-src/locales",
}) {
	const langs = Object.keys(locales);
	const failures = [];
	const warns = [];
	const refsByKey = new Map();
	for (const r of refs) {
		if (!refsByKey.has(r.key)) refsByKey.set(r.key, []);
		refsByKey.get(r.key).push(r);
	}

	// ① 4 份 locale 的键集合两两相等（等价：每个 key 在 4 份里都在）。
	const union = new Set();
	for (const lang of langs) for (const k of locales[lang].keys()) union.add(k);
	const driftByLang = new Map(langs.map((l) => [l, []]));
	const sortedUnion = [...union].sort();
	for (const key of sortedUnion) {
		const missing = langs.filter((l) => !locales[l].has(key));
		if (!missing.length) continue;
		const where = refsByKey.get(key) ?? [];
		// 逐语言报缺：有引用点 ⇒ 报到真实的 文件:行（页面会渲染裸 key / 回退语言）。
		for (const l of missing) {
			driftByLang.get(l).push(key);
			failures.push({
				rule: where.length ? "ref-missing-key" : "locale-drift",
				at: where.length ? `${where[0].file}:${where[0].line}` : `${localeDir}/${l}.yaml`,
				message: where.length
					? `引用的 key "${key}" 在 ${l} 里不存在（${where[0].form}；共 ${where.length} 处引用）—— 4 份 locale 都要有该词条`
					: `key "${key}" 在 ${l} 里缺失（只写在 ${langs
							.filter((x) => locales[x].has(key))
							.join("/")} 里）—— 4 份 locale 的键集合必须相等`,
			});
		}
	}
	// ② 被引用但 4 份 locale 里全部没有 ⇒ sitegen 只能渲染裸 key。
	for (const [key, where] of refsByKey) {
		if (union.has(key)) continue;
		failures.push({
			rule: "ref-missing-key",
			at: `${where[0].file}:${where[0].line}`,
			message: `引用的 key "${key}" 在全部 ${langs.length} 份 locale 里都不存在（sitegen/i18n.go 的 tFunc 会渲染裸 key；共 ${where.length} 处引用）`,
		});
	}
	for (const lang of langs) {
		const miss = driftByLang.get(lang);
		if (!miss.length) continue;
		warns.push({
			rule: "locale-drift-summary",
			message: `${lang}.yaml 相对其他语言少 ${miss.length} 个 key（明细见上方逐条违规）`,
		});
	}

	// ③ 未被引用的 key：只 warn。
	const unreferenced = sortedUnion.filter((k) => !refsByKey.has(k));
	if (unreferenced.length) {
		warns.push({
			rule: "unreferenced",
			message: `${unreferenced.length} 个 key 没有被任何模板/源码引用（可清理，不阻塞构建）：${unreferenced
				.slice(0, 12)
				.join(", ")}${unreferenced.length > 12 ? " …" : ""}`,
		});
	}

	// ④ t() 首参是非扁平字面量（criterion c 之外的另一类「疑似裸 key」）：只提示。
	if (loose.length) {
		warns.push({
			rule: "loose-literal",
			message: `${loose.length} 处 t() 首参不是扁平 key（运行时查不到，会渲染字面量本身）：${loose
				.slice(0, 8)
				.map((l) => `${l.file}:${l.line}`)
				.join(", ")}${loose.length > 8 ? " …" : ""}`,
		});
	}

	// ⑤ 动态拼接前缀的命名空间在每份 locale 里都非空（warn：整段命名空间漏译会被看出来）。
	const prefixes = new Map();
	for (const d of dynamic) {
		if (!d.prefix) continue;
		if (!prefixes.has(d.prefix)) prefixes.set(d.prefix, []);
		prefixes.get(d.prefix).push(d);
	}
	for (const [prefix, sites] of prefixes) {
		for (const lang of langs) {
			const any = sortedUnion.some((k) => k.startsWith(prefix) && locales[lang].has(k));
			if (any) continue;
			warns.push({
				rule: "dynamic-prefix",
				at: `${sites[0].file}:${sites[0].line}`,
				message: `动态 key 前缀 "${prefix}" 在 ${lang}.yaml 里没有任何词条（拼接出来的 key 会全部回退）`,
			});
		}
	}

	// ⑥ 反假绿下限：解析/扫描面被改窄即红，0 项必红。
	const counts = {
		files,
		langs: langs.length,
		keys: sortedUnion.length,
		refs: refs.length,
		refKeys: refsByKey.size,
		dynamic: dynamic.length,
		loose: loose.length,
		unreferenced: unreferenced.length,
	};
	for (const [name, floor] of Object.entries(floors)) {
		const got = counts[name] ?? 0;
		if (got < floor) {
			failures.push({
				rule: "floor",
				at: "scripts/check-i18n-keys.mjs",
				message: `${name} 只有 ${got}，低于下限 ${floor} —— 要么扫描面被改窄（假绿），要么确实缩了（那就同步下调 FLOORS）`,
			});
		}
	}
	return { ok: failures.length === 0, failures, warns, counts };
}

// ── CLI ────────────────────────────────────────────────────
const argOf = (name, def) => process.argv.find((a) => a.startsWith(`--${name}=`))?.split("=")[1] ?? def;

async function collectSources(srcRoot, staticRoot) {
	const files = [];
	for (const dir of TEMPLATE_DIRS) {
		const full = path.join(srcRoot, dir);
		let names = [];
		try {
			names = await fs.readdir(full);
		} catch {
			continue; // 目录不存在时由 FLOORS.files 兜底判红，不静默算通过
		}
		for (const n of names.filter((x) => x.endsWith(".html")).sort()) files.push(path.join(full, n));
	}
	let jsNames = [];
	try {
		jsNames = await fs.readdir(staticRoot);
	} catch {
		jsNames = [];
	}
	for (const n of jsNames.filter((x) => x.endsWith(".js") && !GENERATED_JS.test(x)).sort()) {
		files.push(path.join(staticRoot, n));
	}
	return files;
}

async function main() {
	const srcRoot = path.resolve(argOf("src", "web-src"));
	const staticRoot = path.resolve(argOf("static", STATIC_JSDIR));
	const repoRoot = process.cwd();
	const display = (p) => {
		const rel = path.relative(repoRoot, p);
		return rel === "" || rel.startsWith("..") ? p : rel;
	};

	const locales = {};
	for (const lang of LANGS) {
		const file = path.join(srcRoot, "locales", `${lang}.yaml`);
		let text;
		try {
			text = await fs.readFile(file, "utf8");
		} catch (e) {
			console.error(`✗ check-i18n-keys：读不到 locale 文件 ${file}（${e.code ?? e.message}）`);
			console.error(`   --src 指向的是 web-src 根目录，4 份 locale 必须都在：${LANGS.join(", ")}`);
			process.exit(2);
		}
		try {
			locales[lang] = parseLocale(text, display(file)).entries;
		} catch (e) {
			if (e instanceof ParseError) {
				console.error(`❌ ${e.message}`);
				console.error("   （locale 解析必须 fail-loud：不认识的语法请改成受支持写法，或扩展本脚本的解析器）");
				process.exit(2);
			}
			throw e;
		}
	}

	const files = await collectSources(srcRoot, staticRoot);
	const refs = [];
	const dynamic = [];
	const loose = [];
	for (const file of files) {
		const text = await fs.readFile(file, "utf8");
		const got = extractRefs(text, display(file));
		refs.push(...got.refs);
		dynamic.push(...got.dynamic);
		loose.push(...got.loose);
	}

	const res = analyze({
		locales,
		refs,
		dynamic,
		loose,
		files: files.length,
		localeDir: display(path.join(srcRoot, "locales")),
	});
	const c = res.counts;
	console.log(
		`解析 ${c.files} 个源文件 / ${c.langs} 份 locale / ${c.keys} 个 key / ${c.refs} 个 key 引用点` +
			`（覆盖 ${c.refKeys} 个不同 key；另有 ${c.dynamic} 处动态引用、${c.loose} 处非扁平字面量）`,
	);
	for (const w of res.warns) console.warn(`⚠️  [${w.rule}] ${w.at ? `${w.at}  ` : ""}${w.message}`);

	if (!res.ok) {
		console.error(`❌ i18n 键守卫：${res.failures.length} 处违规`);
		const shown = res.failures.slice(0, 60);
		for (const f of shown) console.error(`   ${f.at}  [${f.rule}] ${f.message}`);
		if (res.failures.length > shown.length) {
			console.error(`   … 另有 ${res.failures.length - shown.length} 处同类违规`);
		}
		console.error("");
		console.error("   修法：4 份 locale（zh-CN/en/ja/es）同时补词条；删词条时先删引用点。");
		console.error("   禁止用白名单掩盖：缺译文是产品缺陷，sitegen 的 WARN 不会让构建失败，只有这里会。");
		process.exit(1);
	}
	console.log(
		`✅ i18n 键守卫：${c.langs} 份 locale 键集合一致，${c.refs} 个引用点全部有词条` +
			`（${c.unreferenced} 个未被引用的 key 仅告警）`,
	);
}

// ── 自测：红/绿两侧都必须自证（纯内存 fixture，不碰仓库文件） ──
function fixtureLocale(text) {
	return parseLocale(text, "<fixture>").entries;
}

const YAML_ABC = "a:\n  b: 乙\n  c: 丙\n";
const YAML_AB = "a:\n  b: 乙\n";

export async function selfTest() {
	let failed = 0;
	const check = (name, ok, detail = "") => {
		console.log(`${ok ? "✅" : "❌"} ${name}${ok ? "" : ` —— ${detail}`}`);
		if (!ok) failed += 1;
	};
	const cases = [
		{
			name: "red: 某 key 缺 en（引用点在，en 没有）",
			run: () => {
				const locales = {
					"zh-CN": fixtureLocale(YAML_ABC),
					en: fixtureLocale(YAML_AB),
					ja: fixtureLocale(YAML_ABC),
					es: fixtureLocale(YAML_ABC),
				};
				const refs = [{ file: "f.html", line: 3, key: "a.c", form: 't("...")' }];
				const res = analyze({ locales, refs, files: 1, floors: { files: 1, keys: 1, refs: 1 } });
				return !res.ok && res.failures.some((f) => f.rule === "ref-missing-key" && f.at === "f.html:3");
			},
		},
		{
			name: "red: 4 份 key 集合不等（未引用的 key 也照样红）",
			run: () => {
				const locales = {
					"zh-CN": fixtureLocale(YAML_ABC),
					en: fixtureLocale(YAML_ABC),
					ja: fixtureLocale(YAML_AB), // 少 a.c，且没人引用
					es: fixtureLocale(YAML_ABC),
				};
				const refs = [{ file: "f.html", line: 1, key: "a.b", form: 't("...")' }];
				const res = analyze({ locales, refs, files: 1, floors: { files: 1, keys: 1, refs: 1 } });
				return !res.ok && res.failures.some((f) => f.rule === "locale-drift" && f.message.includes("a.c"));
			},
		},
		{
			name: "red: key 在 4 份 locale 里都没有（裸 key 渲染）",
			run: () => {
				const locales = Object.fromEntries(LANGS.map((l) => [l, fixtureLocale(YAML_ABC)]));
				const refs = [{ file: "f.html", line: 9, key: "nope.gone", form: 't("...")' }];
				const res = analyze({ locales, refs, files: 1, floors: { files: 1, keys: 1, refs: 1 } });
				return !res.ok && res.failures.some((f) => f.message.includes("全部"));
			},
		},
		{
			name: "green: 4 份齐全 + 引用点都有词条",
			run: () => {
				const locales = Object.fromEntries(LANGS.map((l) => [l, fixtureLocale(YAML_ABC)]));
				const refs = [
					{ file: "f.html", line: 1, key: "a.b", form: 't("...")' },
					{ file: "g.js", line: 2, key: "a.c", form: "t(...)" },
				];
				const res = analyze({ locales, refs, files: 2, floors: { files: 1, keys: 1, refs: 1 } });
				return res.ok && res.counts.keys === 2 && res.counts.refs === 2;
			},
		},
		{
			name: "red: 反假绿下限（0 引用点必须红）",
			run: () => {
				const locales = Object.fromEntries(LANGS.map((l) => [l, fixtureLocale(YAML_ABC)]));
				const res = analyze({ locales, refs: [], files: 1, floors: { files: 1, keys: 2, refs: 1 } });
				return !res.ok && res.failures.some((f) => f.rule === "floor" && f.message.includes("refs"));
			},
		},
		{
			name: "green: 未引用的 key 只 warn 不失败",
			run: () => {
				const locales = Object.fromEntries(LANGS.map((l) => [l, fixtureLocale(YAML_ABC)]));
				const refs = [{ file: "f.html", line: 1, key: "a.b", form: 't("...")' }];
				const res = analyze({ locales, refs, files: 1, floors: { files: 1, keys: 1, refs: 1 } });
				return res.ok && res.counts.unreferenced === 1 && res.warns.some((w) => w.rule === "unreferenced");
			},
		},
		{
			name: "green: 引用形态 t(\"a.b\") 命中且行号正确",
			run: () => {
				const src = '<p>x</p>\n<script>\n  const x = t("a.b");\n</script>\n';
				const { refs } = extractRefs(src, "f.html");
				return refs.length === 1 && refs[0].key === "a.b" && refs[0].line === 3;
			},
		},
		{
			name: "green: 引用形态 t(\"a.b\", \"默认值\") 命中（第二参不是 key）",
			run: () => {
				const src = "const y = t('a.c', '兜底文案');\n";
				const { refs } = extractRefs(src, "f.js");
				return refs.length === 1 && refs[0].key === "a.c";
			},
		},
		{
			name: "green: 成员/别名/三元/模板形态都命中",
			run: () => {
				const src = [
					"<title>{{t \"a.b\"}}</title>",
					"<p>{{tHTML 'a.c'}}</p>",
					"A.t(\"a.b\"); Anotify.t('a.c'); sharedT(\"a.b\"); section(\"a.c\", \"wire\");",
					"t(ok ? \"a.b\" : \"a.c\");",
				].join("\n");
				const { refs } = extractRefs(src, "f.html");
				const keys = refs.map((r) => r.key).sort();
				return (
					keys.length === 8 &&
					keys.filter((k) => k === "a.b").length === 4 &&
					keys.filter((k) => k === "a.c").length === 4
				);
			},
		},
		{
			name: "green: 动态形态不误判为 key（t(k) / t(\"a.\" + x) / printf）",
			run: () => {
				const src = [
					"t(k); t(key + '_confirm'); t(\"a.\" + suffix);",
					"<span>{{t (printf \"docs.openapi.tag.%s\" .Tag)}}</span>",
				].join("\n");
				const { refs, dynamic } = extractRefs(src, "f.html");
				const prefixes = dynamic.map((d) => d.prefix).filter(Boolean);
				return refs.length === 0 && dynamic.length === 4 && prefixes.includes("a.") && prefixes.includes("docs.openapi.tag.");
			},
		},
		{
			name: "green: 前缀包装器还原（const t = key => A.t(\"a.\" + key)）",
			run: () => {
				const src = [
					'const t = key => A.t("a." + key);',
					'const y = t("b");',
					'const z = t("c.d");',
				].join("\n");
				const { refs, dynamic } = extractRefs(src, "f.html");
				const keys = refs.map((r) => r.key).sort();
				// wrapper 定义本身是动态前缀（a.），两个调用还原成 a.b / a.c.d。
				return (
					JSON.stringify(keys) === JSON.stringify(["a.b", "a.c.d"]) &&
					dynamic.some((d) => d.prefix === "a.")
				);
			},
		},
		{
			name: "green: section(key) 转发到带前缀的 t ⇒ key 跟着带前缀",
			run: () => {
				const src = [
					'const t = (key) => A.t("sources.v4." + key);',
					"function section(key) { return el('div', t(key)); }",
					'const a = section("recent");',
				].join("\n");
				const { refs } = extractRefs(src, "f.html");
				return refs.length === 1 && refs[0].key === "sources.v4.recent";
			},
		},
		{
			name: "green: 后缀拼接算动态、数字实参不计、纯字面量算引用",
			run: () => {
				const src = ['t(action + "_confirm");', "t(620);", 't("a.b");'].join("\n");
				const { refs, dynamic } = extractRefs(src, "f.html");
				// t(620) 是 mock.js 的时间戳助手，两边都不计；后缀拼接进 dynamic。
				return refs.length === 1 && refs[0].key === "a.b" && dynamic.length === 1;
			},
		},
		{
			name: "green: 模板里的 printf 不被当成 JS 调用重复计",
			run: () => {
				const src = '<span>{{t (printf "docs.openapi.tag.%s" .Tag)}}</span>';
				const { refs, dynamic, loose } = extractRefs(src, "f.html");
				return (
					refs.length === 0 &&
					loose.length === 0 &&
					dynamic.length === 1 &&
					dynamic[0].prefix === "docs.openapi.tag."
				);
			},
		},
		{
			name: "red: 解析器拒绝块标量 | / >",
			run: () => throwsParseError(() => fixtureLocale("a:\n  b: |\n    多行\n")),
		},
		{
			name: "red: 解析器拒绝锚点/别名、流式集合、序列、多文档、tab",
			run: () =>
				["a: &x 1\n", "a: *x\n", "a: [1, 2]\n", "a: {b: 1}\n", "a:\n  - b\n", "---\na: 1\n", "a:\n\tb: 1\n"].every(
					(bad) => throwsParseError(() => fixtureLocale(bad)),
				),
		},
		{
			name: "red: 解析器拒绝重复键（含扁平平铺后重名）",
			run: () =>
				["a:\n  b: 1\n  b: 2\n", "a.b: 1\na:\n  b: 2\n"].every((bad) => throwsParseError(() => fixtureLocale(bad))),
		},
		{
			name: "green: 解析器支持跨行引号标量 / 行尾注释 / 引号键名 / 空值",
			run: () => {
				const y = [
					"# 顶层注释",
					'a: 甲  # 行尾注释',
					'b: "引号 乙"',
					'c: \'跨行开头',
					"",
					"   1. 续行里的：冒号不影响键集合 '",
					'"on": 丙',
					"empty:",
				].join("\n");
				const got = [...fixtureLocale(y).keys()].sort();
				return JSON.stringify(got) === JSON.stringify(["a", "b", "c", "empty", "on"]);
			},
		},
	];
	for (const c of cases) check(c.name, c.run());
	console.log(failed === 0 ? "✅ 自测全绿" : `❌ 自测失败 ${failed} 项`);
	return failed === 0;
}

function throwsParseError(fn) {
	try {
		fn();
		return false;
	} catch (e) {
		return e instanceof ParseError;
	}
}

const isDirectRun = runsAsMain(import.meta.url);
if (isDirectRun) {
	const USAGE = "用法：node scripts/check-i18n-keys.mjs [--src=web-src] [--static=web] [--self-test]";
	const known = (a) => a === "--self-test" || a.startsWith("--src=") || a.startsWith("--static=");
	const unknown = process.argv.slice(2).filter((a) => !known(a));
	if (unknown.length) {
		console.error(`✗ check-i18n-keys：不认识的参数 ${unknown.join(" ")}`);
		console.error(USAGE);
		process.exit(2);
	}
	if (process.argv.includes("--self-test")) {
		selfTest()
			.then((passed) => process.exit(passed ? 0 : 1))
			.catch((e) => {
				console.error(e);
				process.exit(1);
			});
	} else {
		main().catch((e) => {
			if (e instanceof ParseError) {
				console.error(`❌ ${e.message}`);
				process.exit(2);
			}
			console.error(e);
			process.exit(1);
		});
	}
}
