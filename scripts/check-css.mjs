#!/usr/bin/env node
/* Anotify · CSS 结构守卫（括号平衡 / 禁止嵌套 / 顶层裸声明）
 *
 * 为什么需要它（真实事故 fdba2013）：
 *   #401 rebase 解冲突时，`web/ui.css` 里 `.code-body 软换行` 规则的 `}` 和它所在的
 *   `@media` 的 `}` 一起被解掉了。CSS 的容错解析于是把 `.code-body` 之后的整段
 *   （5945→EOF：#385 的 tap-highlight / 安全区 / pointer:coarse 全屏 HUD / <640 首屏
 *   压缩）当成 **`.code-body` 的后代选择器**，一条都没生效。而 `make fe` 里的死类守卫
 *   `scripts/check-classes.mjs` 的 `selectorClasses()` 是**上下文无关**的（只管「有哪些
 *   类名出现过」），修好与修坏返回的类集合完全相同 —— 结构性失明，实测确认。
 *   于是：一场静默的整段失效，所有静态门禁全绿，只能靠真机肉眼发现。
 *
 * 判定方法（全部纯静态，只用 node: 内置模块，零 npm 依赖）：
 *   扫描 `web/ui.css`、`web/tokens.css`、`web-src/tailwind.input.css`（存在才查；
 *   缺了只打一条 ⚠️、不算违规、也不失败），用一个逐字符的 `{` / `}` / `;` token→block
 *   栈解析器：
 *   ① 先剥掉注释与字符串（含 `url()` 里的引号、选择器里的 `\` 转义）后，`{` 与 `}`
 *      必须逐块配平；不配对就报，并给出每个文件的 open/close 计数。
 *   ② 禁止 CSS 嵌套（除 at-rule 内部）：**规则块里再出现规则块** ⇒ `疑似丢失右括号`。
 *      允许「规则块出现在 at-rule 体内」的 at-rule 只有白名单
 *      （@media/@supports/@layer/@container/@keyframes/@font-face/@property/@page/@scope）；
 *      @keyframes 体内的 from/to/百分比块按合法处理。
 *      反之，at-rule 嵌在规则块里、规则块嵌在声明体 at-rule（如 @font-face）里，同样报。
 *   ③ 顶层（depth 0）不得出现裸声明（形如 `prop: value;`）—— 多写一个 `}` 的典型签名。
 *      （顶层 at-rule 语句 `@tailwind base;` / `@import …;` 不算裸声明。）
 *   ④ `{` 之前必须已经攒到非空 prelude（选择器或 at-rule 名），否则报。
 *   ⑤ 反假绿：永远打印「文件数 / 规则数 / at-rule 数 / 声明数」，并断言这些计数
 *      **高于实测下限**（常量见 RULE_MIN / AT_RULE_MIN / DECL_MIN）。解析出 0 条规则、
 *      文件没读到、判据空转，都必须红 —— 「绿」不能和「没检查」同形。
 *   ⑥ `--self-test`：表驱动红/绿自证（缺 `}`、多 `}`、嵌套规则块、顶层裸声明、
 *      正确的 @media / @keyframes 嵌套、字符串与注释里的 `{ } ;`、@tailwind 语句、
 *      计数下限），纯内存 fixture，不改仓库文件。
 *
 * 用法：
 *   node scripts/check-css.mjs              # 正常体检，违规 exit 1
 *   node scripts/check-css.mjs --self-test  # 跑本脚本自己的红/绿自测
 *   退出码：0 通过 / 1 有违规 / 2 用法或环境问题（目录不对、一个文件都没读到、参数不认识）
 *
 * 已知边界（有意不做的部分，见 docs/lint-guards.md §2.9）：
 *   · 不做级联语义分析：不判断哪条声明最终生效（属性被后置规则覆盖、`env(safe-area-*)`
 *     被简写冲掉、@layer 顺序、特定性），那些是 §2.9 的窄规则，不是本守卫。
 *   · 不校验选择器/属性名拼写、不校验属性值合法性、不查设计 token。
 *   · 只看 `{` / `}` / `;` 三个结构字符与块的嵌套关系；不构建 AST，不认 `@import`
 *     条件、不展开 `@apply`。
 *   · 本项目**禁止** CSS 原生嵌套（`.a { .b { … } }`）；这种虽然在新版浏览器里合法，
 *     会被本守卫判红（这正是丢括号事故的形态，没法与它区分）。
 *   · 页面内联 `<style>` 块不在范围内（只查上面三个 CSS 文件）。
 *   · at-rule prelude 里出现未转义的 `{`（不合法写法）会被当成块起始，可能误报。
 */
import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

/** 仓库根（按脚本位置推，不依赖 cwd；与 scripts/build-tailwind.mjs 同款）。 */
const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

/** 待检查文件（相对仓库根）。存在才查，缺了静默跳过。 */
export const DEFAULT_FILES = [
	"web/ui.css",
	"web/tokens.css",
	"web-src/tailwind.input.css",
];

/** 允许「体内出现规则块」的 at-rule 白名单（docs/lint-guards.md L1 冻结口径）。
 *  注：@font-face / @property / @page 实际上是声明体 at-rule（@page 里允许内嵌
 *  margin at-rule），列在这里是照抄契约；真正的分界线是「规则块 vs 声明体」，
 *  白名单只决定「规则块出现在它体内算不算合法」。 */
export const AT_RULES_WITH_RULES = new Set([
	"media",
	"supports",
	"layer",
	"container",
	"keyframes",
	"-webkit-keyframes",
	"-moz-keyframes",
	"font-face",
	"property",
	"page",
	"scope",
]);

/* ---------- 反假绿下限（实测值向下取整，不许改成 0） ----------
 * 实测出处：main e6fcc8fe（2026-11，v4 开发期），实测命令 `node scripts/check-css.mjs`：
 *   web/ui.css          888 个 `{`：711 条顶层规则 + 28 个顶层 @media 体内 102 条规则
 *                       + 18 个 @keyframes 体内 28 个 from/to/百分比块
 *                       + 28 个 @media + 18 个 @keyframes + 1 个 @media 体内 @view-transition
 *   web/tokens.css      4 个 `{`：2 条顶层规则 + 1 个 @media 体内 1 条规则 + 1 个 @media
 *   web-src/tailwind.input.css  3 条顶层 at-rule 语句（@tailwind …;），无块
 *   合计：规则块 844 / at-rule 块 48 / 声明 3043 / 顶层 at-rule 语句 3
 * 下限取「明显低于实测、但远高于 0」的整数（规则向下取整到百位，声明留足日常增删余量）：
 *   规则 844 → 800（约 95%）、at-rule 48 → 40、声明 3043 → 2000（约 66%，
 *   声明数随样式增删波动最大）。任一计数落到下限及以下（判据是「必须 > 下限」），
 *   说明解析器坏了或文件没读到，而不是「样式变少了」—— 必须红。 */
export const RULE_MIN = 800;
export const AT_RULE_MIN = 40;
export const DECL_MIN = 2000;

const DECL_PREFIX = /^[-_a-zA-Z][\w-]*\s*:/;

/** 把多行片段压成一行，供报告里贴「原文」。 */
function oneLine(text, max = 120) {
	const flat = String(text).replace(/\s+/g, " ").trim();
	return flat.length > max ? `${flat.slice(0, max)}…` : flat;
}

/** 单条违规的统一格式：`文件:行 规则 原文`（line 为 0 时只打文件，用于总计类违规）。 */
export function formatViolation(v) {
	const where = v.line > 0 ? `${v.file}:${v.line}` : v.file;
	return `${where} ${v.rule} ${v.text}`;
}

/**
 * 纯函数：解析一段 CSS 的结构。
 * @param {string} css  CSS 源码
 * @param {string} file 报告里用的文件名
 * @returns {{stats: object, violations: object[]}}
 *   stats: { file, opens, closes, rules, atRules, atRuleStatements, declarations }
 *   violations: [{ file, line, rule, text, message }]
 *   规则码：BRACE_UNBALANCED / NESTED_RULE / NESTED_AT_RULE / EMPTY_PRELUDE / TOPLEVEL_DECL
 */
export function analyzeCss(css, file = "<inline>") {
	const stats = {
		file,
		opens: 0,
		closes: 0,
		rules: 0,
		atRules: 0,
		atRuleStatements: 0,
		declarations: 0,
	};
	const violations = [];
	/** 块栈：[{ kind: "rule" | "at", name, line, prelude }] */
	const stack = [];

	const report = (rule, atLine, text, message) => {
		violations.push({ file, line: atLine, rule, text: oneLine(text), message });
	};

	let line = 1;
	let buf = ""; // 上一个结构边界（`{` `}` `;`）之后的原文
	let bufLine = 0; // buf 里第一个非空白字符所在行，0 表示 buf 还没内容

	const append = (text) => {
		if (!bufLine && text.trim()) bufLine = line;
		buf += text;
	};
	const resetBuf = () => {
		buf = "";
		bufLine = 0;
	};

	let i = 0;
	while (i < css.length) {
		const ch = css[i];

		// ---- 注释 /* … */：整段跳过，但要把行号数准 ----
		if (ch === "/" && css[i + 1] === "*") {
			i += 2;
			while (i < css.length && !(css[i] === "*" && css[i + 1] === "/")) {
				if (css[i] === "\n") line++;
				i++;
			}
			i += 2;
			continue;
		}

		// ---- 字符串 "…" / '…'：里面的 { } ; 不是结构字符 ----
		if (ch === '"' || ch === "'") {
			const quote = ch;
			i++;
			while (i < css.length && css[i] !== quote) {
				if (css[i] === "\\") {
					if (css[i + 1] === "\n") line++;
					i += 2;
					continue;
				}
				if (css[i] === "\n") line++;
				i++;
			}
			i++; // 跳过收尾引号（未闭合则 i 越过 EOF，循环自然结束）
			continue;
		}

		// ---- url( … )：含引号与「未加引号」两种写法，里面的 ; { } 都属于值 ----
		if (
			(ch === "u" || ch === "U") &&
			/^url\(/i.test(css.slice(i, i + 4)) &&
			!/[\w-]/.test(css[i - 1] ?? "")
		) {
			const urlLine = line;
			let j = i + 4;
			while (j < css.length && css[j] !== ")") {
				if (css[j] === '"' || css[j] === "'") {
					const quote = css[j];
					j++;
					while (j < css.length && css[j] !== quote) {
						if (css[j] === "\\") {
							if (css[j + 1] === "\n") line++;
							j += 2;
							continue;
						}
						if (css[j] === "\n") line++;
						j++;
					}
					j++;
					continue;
				}
				if (css[j] === "\\") {
					if (css[j + 1] === "\n") line++;
					j += 2;
					continue;
				}
				if (css[j] === "\n") line++;
				j++;
			}
			if (!bufLine) bufLine = urlLine;
			buf += css.slice(i, j + 1);
			i = j + 1;
			continue;
		}

		// ---- 块起始 ----
		if (ch === "{") {
			stats.opens++;
			const prelude = buf.trim();
			const atLine = bufLine || line;
			const parent = stack.length > 0 ? stack[stack.length - 1] : null;
			if (!prelude) {
				report(
					"EMPTY_PRELUDE",
					line,
					"{",
					"`{` 之前没有任何选择器或 at-rule 前导（多写了一个 `{`／丢了上一段的选择器？）",
				);
			}
			const isAtRule = prelude.startsWith("@");
			if (isAtRule) {
				const name = (prelude.slice(1).match(/^[\w-]*/) ?? [""])[0].toLowerCase();
				if (parent && parent.kind === "rule") {
					report(
						"NESTED_AT_RULE",
						atLine,
						prelude,
						`at-rule @${name || "?"} 嵌在规则块 \`${oneLine(parent.prelude, 60)}\` 内部 —— 本项目禁止 CSS 嵌套，此处多为丢了一个 \`}\`（疑似丢失右括号）`,
					);
				}
				stats.atRules++;
				stack.push({ kind: "at", name, line: atLine, prelude });
			} else {
				if (parent && parent.kind === "rule") {
					report(
						"NESTED_RULE",
						atLine,
						prelude,
						`规则块内再出现规则块（外层是 \`${oneLine(parent.prelude, 60)}\`）—— CSS 不允许规则嵌套，这说明外层少了一个 \`}\`（疑似丢失右括号）；这正是 fdba2013 的形态：其后整段样式被吞成后代选择器，一条都不生效`,
					);
				} else if (parent && parent.kind === "at" && !AT_RULES_WITH_RULES.has(parent.name)) {
					report(
						"NESTED_RULE",
						atLine,
						prelude,
						`规则块出现在声明体 at-rule @${parent.name} 内部 —— 该 at-rule 只装声明，此处多为丢了 \`}\`（疑似丢失右括号）`,
					);
				}
				stats.rules++;
				stack.push({ kind: "rule", line: atLine, prelude });
			}
			resetBuf();
			i++;
			continue;
		}

		// ---- 块结束 ----
		if (ch === "}") {
			stats.closes++;
			// 块内最后一条声明可能没有分号（`color: red}`），这里补计数
			const tail = buf.trim();
			if (tail && !tail.startsWith("@") && DECL_PREFIX.test(tail)) {
				stats.declarations++;
			}
			if (stack.length === 0) {
				report(
					"BRACE_UNBALANCED",
					line,
					"}",
					"多出来的 `}`：此时栈里没有任何未闭合的块与之配对（多写了一个右括号）",
				);
			} else {
				stack.pop();
			}
			resetBuf();
			i++;
			continue;
		}

		// ---- 分号：声明的边界，也是顶层裸声明的探针 ----
		if (ch === ";") {
			const chunk = buf.trim();
			if (chunk) {
				if (chunk.startsWith("@")) {
					stats.atRuleStatements++;
				} else if (stack.length === 0) {
					if (DECL_PREFIX.test(chunk)) {
						report(
							"TOPLEVEL_DECL",
							bufLine || line,
							chunk,
							"顶层（depth 0）出现裸声明 —— 这是多写了一个 `}` 把外层规则提前关掉的典型签名；该声明不属于任何选择器，浏览器会直接丢弃",
						);
					}
				} else {
					stats.declarations++;
				}
			}
			resetBuf();
			i++;
			continue;
		}

		// ---- 换行 / 转义 / 普通字符 ----
		if (ch === "\n") {
			line++;
			buf += ch;
			i++;
			continue;
		}
		if (ch === "\\") {
			// 选择器或值里的转义（如 .sm\:flex、\2014）：转义后的字符不算结构字符
			const next = css[i + 1] ?? "";
			if (!bufLine) bufLine = line;
			buf += ch + next;
			if (next === "\n") line++;
			i += 2;
			continue;
		}
		append(ch);
		i++;
	}

	// ---- 收尾：还有没闭合的块 ----
	for (const frame of stack.slice(0, 10)) {
		report(
			"BRACE_UNBALANCED",
			frame.line,
			frame.prelude,
			`这个${frame.kind === "at" ? " at-rule 块" : "规则块"}没有闭合：到文件末尾仍缺 \`}\`（其后的一切都会被当成它的内容吞掉）`,
		);
	}
	if (stack.length > 10) {
		report(
			"BRACE_UNBALANCED",
			line,
			`…另有 ${stack.length - 10} 个未闭合块`,
			"未闭合块过多，只列出前 10 个",
		);
	}

	return { stats, violations };
}

/**
 * 纯函数：把多份源码一起体检（结构违规 + 反假绿下限）。
 * @param {{file: string, css: string}[]} sources
 * @param {{ruleMin?: number, atRuleMin?: number, declMin?: number}} [opts]
 */
export function checkSources(sources, opts = {}) {
	const ruleMin = opts.ruleMin ?? RULE_MIN;
	const atRuleMin = opts.atRuleMin ?? AT_RULE_MIN;
	const declMin = opts.declMin ?? DECL_MIN;

	const totals = {
		files: 0,
		opens: 0,
		closes: 0,
		rules: 0,
		atRules: 0,
		atRuleStatements: 0,
		declarations: 0,
	};
	const perFile = [];
	const violations = [];

	for (const src of sources) {
		const { stats, violations: vs } = analyzeCss(src.css, src.file);
		perFile.push(stats);
		totals.files++;
		totals.opens += stats.opens;
		totals.closes += stats.closes;
		totals.rules += stats.rules;
		totals.atRules += stats.atRules;
		totals.atRuleStatements += stats.atRuleStatements;
		totals.declarations += stats.declarations;
		violations.push(...vs);
	}

	// 反假绿：解析出来的东西必须够多，否则判据在空转
	const floors = [
		["RULE_COUNT_BELOW_MIN", totals.rules, ruleMin, "规则块"],
		["AT_RULE_COUNT_BELOW_MIN", totals.atRules, atRuleMin, "at-rule 块"],
		["DECLARATION_COUNT_BELOW_MIN", totals.declarations, declMin, "声明"],
	];
	for (const [rule, actual, min, label] of floors) {
		if (actual > min) continue;
		violations.push({
			file: "（总计）",
			line: 0,
			rule,
			text: `${label} ${actual} ≤ 下限 ${min}`,
			message:
				"解析出来的数量没超过实测下限 —— 文件没读到 / 解析器坏了 / 判据在空转；「绿」不能和「没检查」同形",
		});
	}

	return { stats: { ...totals, perFile }, violations };
}

/* ---------- 自测：证明「错的判红、对的判绿」（纯内存，不改仓库文件） ---------- */
function selfTest() {
	const cases = [];

	// ① 缺一个 `}`：完全复刻 fdba2013 的形态（@media 里的规则块丢了 `}`，
	//    后面的 `.stat-spark` 被当成 `.code-body` 的后代选择器）
	cases.push({
		name: "缺少一个 `}`（fdba2013 同型：@media 内规则块少收尾）",
		css: [
			".code-wrap {",
			"\tpadding: 8px;",
			"}",
			"@media (max-width: 640px) {",
			"\t.code-body {",
			"\t\twhite-space: pre-wrap;",
			"\t/* ← 这里少一个 `}`（规则块的），于是下面整段被吞成后代选择器 */",
			"\t.stat-spark {",
			"\t\theight: 22px;",
			"\t}",
			"}",
		].join("\n"),
		codes: ["BRACE_UNBALANCED", "NESTED_RULE"],
		why: "必须红：规则块外层的 `}` 丢了",
	});

	// ② 多一个 `}`：多出来的右括号没有可配对的块（open 6 / close 7）
	cases.push({
		name: "多一个 `}`（多余的右括号没有配对）",
		css: [".a {", "\tcolor: red;", "}", "}", ".b {", "\tcolor: blue;", "}"].join("\n"),
		codes: ["BRACE_UNBALANCED"],
		why: "必须红：多写的右括号会把后面的规则提前推出外层",
	});

	// ③ 嵌套规则块：括号是配平的，只有嵌套判据能抓到（证明不靠计数）
	cases.push({
		name: "嵌套规则块（括号配平，只靠嵌套判据）",
		css: [".a {", "\tcolor: red;", "\t.b {", "\t\tcolor: blue;", "\t}", "}"].join("\n"),
		codes: ["NESTED_RULE"],
		why: "必须红：规则块里再出现规则块 = 疑似丢失右括号",
	});

	// ④ 顶层裸声明：多写一个 `}` 之后最常见的签名
	cases.push({
		name: "顶层裸声明（多写 `}` 的签名）",
		css: [".a {", "\tcolor: red;", "}", "color: blue;", ".b {", "\tcolor: green;", "}"].join(
			"\n",
		),
		codes: ["TOPLEVEL_DECL"],
		why: "必须红：裸声明不属于任何选择器，浏览器静默丢弃",
	});

	// ⑤ 正确的 @media / @keyframes / @font-face 嵌套：必须绿
	cases.push({
		name: "正确的 at-rule 嵌套（@media / @keyframes / @font-face 体内规则）",
		css: [
			":root {",
			"\t--x: 1px;",
			"}",
			"@media (max-width: 640px) {",
			"\t@view-transition {",
			"\t\tnavigation: none;",
			"\t}",
			"\t.a, .b {",
			"\t\tcolor: var(--x);",
			"\t}",
			"}",
			"@keyframes spin {",
			"\tfrom { transform: rotate(0deg); }",
			"\t50% { opacity: .5 }",
			"\tto { transform: rotate(360deg); }",
			"}",
			"@font-face {",
			'\tfont-family: "X";',
			"\tsrc: url(x.woff2) format(\"woff2\");",
			"}",
		].join("\n"),
		codes: [],
		extra: ({ stats }) =>
			stats.rules === 5 && stats.atRules === 4
				? null
				: `规则块应 5（:root/.a,.b/3 个 keyframe 块）实际 ${stats.rules}；at-rule 块应 4 实际 ${stats.atRules}`,
		why: "必须绿：at-rule 白名单内的嵌套是合法结构",
	});

	// ⑥ 字符串 / 注释 / url() 里的 `{ } ;` 不得误报，也不得打乱结构
	cases.push({
		name: "字符串/注释/url() 里的 `{` `}` `;` 不误报",
		css: [
			"/* 注释里的 { } ; 都不是结构字符 */",
			".a::after {",
			'\tcontent: "}{;";',
			'\tbackground: url("data:image/svg+xml,%3Csvg%3E;a{b}%3C/svg%3E");',
			"\t--esc: '}';",
			"}",
			".b { color: red }",
		].join("\n"),
		codes: [],
		extra: ({ stats, violations: vs }) =>
			vs.length === 0 && stats.rules === 2 && stats.opens === 2 && stats.closes === 2
				? null
				: `期望 2 规则 / 2 open / 2 close 且零违规，实际 ${stats.rules} / ${stats.opens} / ${stats.closes}，违规 ${vs.length}`,
		why: "必须绿：字符串与注释不参与结构判定",
	});

	// ⑦ `{` 前没有 prelude
	cases.push({
		name: "`{` 前没有选择器/at-rule 前导",
		css: ["@media (max-width: 640px) {", "\t{", "\t\tcolor: red;", "\t}", "}"].join("\n"),
		codes: ["EMPTY_PRELUDE"],
		why: "必须红：块起始前必须已有非空 prelude",
	});

	// ⑧ 顶层 at-rule 语句（@tailwind / @import）不是裸声明
	cases.push({
		name: "顶层 at-rule 语句（@tailwind …;）不算裸声明",
		css: ["@tailwind base;", "@tailwind components;", '.a { content: ";"; }'].join("\n"),
		codes: [],
		extra: ({ stats }) =>
			stats.atRuleStatements === 2
				? null
				: `顶层 at-rule 语句应 2 条，实际 ${stats.atRuleStatements}`,
		why: "必须绿：tailwind.input.css 就是这个形状",
	});

	// ⑨ at-rule 嵌在规则块里：本项目禁止 CSS 嵌套（这是丢括号的另一种形态）
	cases.push({
		name: "at-rule 嵌在规则块内（CSS 嵌套）",
		css: [".a {", "\tcolor: red;", "\t@media (min-width: 640px) {", "\t\tcolor: blue;", "\t}", "}"].join(
			"\n",
		),
		codes: ["NESTED_AT_RULE"],
		why: "必须红：项目禁止 CSS 原生嵌套，at-rule 出现在规则块内就是丢了一个 `}`",
	});

	let failed = 0;
	const results = [];
	for (const c of cases) {
		const res = analyzeCss(c.css, `<fixture:${c.name}>`);
		const codes = [...new Set(res.violations.map((v) => v.rule))].sort();
		const want = [...c.codes].sort();
		let problem = null;
		if (JSON.stringify(codes) !== JSON.stringify(want)) {
			problem = `违规码期望 [${want.join(", ") || "无"}]，实际 [${codes.join(", ") || "无"}]`;
		} else if (c.extra) {
			problem = c.extra(res);
		}
		results.push({ name: c.name, why: c.why, problem });
		if (problem) failed++;
	}

	// ⑩ 反假绿：解析出 0 条规则必须红（下限判据本身要能红）
	{
		const res = checkSources([{ file: "<fixture:空文件>", css: "" }]);
		const codes = res.violations.map((v) => v.rule);
		const want = [
			"RULE_COUNT_BELOW_MIN",
			"AT_RULE_COUNT_BELOW_MIN",
			"DECLARATION_COUNT_BELOW_MIN",
		];
		const missing = want.filter((w) => !codes.includes(w));
		const problem =
			missing.length > 0
				? `空文件必须触发 ${missing.join(", ")}，实际 [${codes.join(", ") || "无"}]`
				: null;
		results.push({
			name: "解析出 0 条规则 ⇒ 下限判据必须红（反假绿）",
			why: "必须红：否则「没检查」与「绿」同形",
			problem,
		});
		if (problem) failed++;
	}

	for (const r of results) {
		if (r.problem) {
			console.error(`  ❌ ${r.name}\n     ${r.why}\n     ${r.problem}`);
		} else {
			console.log(`  ✅ ${r.name} —— ${r.why}`);
		}
	}
	console.log(
		failed === 0
			? `✅ self-test ${results.length}/${results.length} 通过（红/绿双侧自证）`
			: `✗ self-test ${results.length - failed}/${results.length} 通过`,
	);
	return failed === 0;
}

/* ---------- 主流程 ---------- */
async function main() {
	const argv = process.argv.slice(2);
	const unknown = argv.filter((a) => a.startsWith("-"));
	if (unknown.length > 0) {
		console.error(`✗ 不认识的参数：${unknown.join(" ")}`);
		console.error("  用法：node scripts/check-css.mjs [--self-test]");
		return 2;
	}

	const sources = [];
	const missing = [];
	for (const file of DEFAULT_FILES) {
		try {
			sources.push({ file, css: await fs.readFile(path.join(ROOT, file), "utf8") });
		} catch (e) {
			if (e.code === "ENOENT") {
				missing.push(file);
				continue;
			}
			console.error(`✗ 读取 ${file} 失败：${e.message}`);
			return 2;
		}
	}
	if (missing.length > 0) {
		console.warn(`⚠️  跳过不存在的文件：${missing.join(", ")}`);
	}
	if (sources.length === 0) {
		console.error(
			`✗ 一个待检查的 CSS 文件都没找到（在 ${ROOT} 下缺 ${DEFAULT_FILES.join(" / ")}）—— 这是环境问题，不是通过`,
		);
		return 2;
	}

	const { stats, violations } = checkSources(sources);

	// 反假绿：无论如何都打印检查数量
	const counts = `${stats.files} 文件 / ${stats.rules} 规则 / ${stats.atRules} at-rule 块（另有 ${stats.atRuleStatements} 条 at-rule 语句） / ${stats.declarations} 声明`;
	const floors = `下限 规则≥${RULE_MIN}、at-rule≥${AT_RULE_MIN}、声明≥${DECL_MIN}`;

	if (violations.length === 0) {
		console.log(`✅ CSS 结构守卫通过：${counts}（${floors}）`);
		return 0;
	}

	const byFile = new Map();
	for (const v of violations) {
		if (!byFile.has(v.file)) byFile.set(v.file, []);
		byFile.get(v.file).push(v);
	}
	console.error(`✗ CSS 结构违规：${violations.length} 条，涉及 ${byFile.size} 个文件`);
	console.error(`  检查量：${counts}`);
	for (const [file, vs] of byFile) {
		console.error(`\n  ${file}`);
		vs.sort((a, b) => a.line - b.line);
		for (const v of vs) {
			console.error(`    ${formatViolation(v)}`);
			console.error(`      → ${v.message}`);
		}
	}

	console.error("\n  每个文件的括号计数（open / close）：");
	for (const s of stats.perFile) {
		console.error(
			`    ${s.file}: ${s.opens} / ${s.closes}${s.opens === s.closes ? "" : "  ← 不平衡"}`,
		);
	}

	console.error("\n  处置建议：");
	console.error(
		"    · 结构违规几乎都是合并/rebase 解冲突时丢或多了 `}`：按报的行号补/删括号后重跑",
	);
	console.error(
		"    · 别用「把整段删掉」「加注释临时注释掉」换绿 —— 丢掉的样式不会有人再补回来（fdba2013 教训）",
	);
	console.error(
		"    · 若确信是合法写法被误判（例如确实要用 CSS 原生嵌套）→ 改本守卫的判据与白名单，并在 docs/lint-guards.md 记一笔",
	);
	return 1;
}

const isDirectRun =
	process.argv[1] &&
	path.resolve(process.argv[1]) === path.resolve(fileURLToPath(import.meta.url));
if (isDirectRun) {
	if (process.argv.includes("--self-test")) {
		process.exit(selfTest() ? 0 : 1);
	} else {
		main()
			.then((code) => process.exit(code))
			.catch((e) => {
				console.error(`✗ check-css 运行失败：${e?.stack ?? e}`);
				process.exit(2);
			});
	}
}
