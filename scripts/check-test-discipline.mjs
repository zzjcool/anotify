#!/usr/bin/env node
/**
 * 测试纪律棘轮守卫（docs/lint-guards.md · L3）
 *
 * 为什么需要它 —— 四条线，每条都对应真实返工，不是「最佳实践」清单：
 *
 *   ① `waitForTimeout` 当成「就绪」用：`edc71233` / `44184536` 里 e2e 等抽屉动画
 *      用的是 `await page.waitForTimeout(300)`，随后 `mouse.wheel` 打在**抽屉**上
 *      （不是页面），`window.scrollY` 恒为 0，于是「页面不可滚动」这个断言成立 ——
 *      一次静默的错误结论。等待必须是有条件的（等元素/等状态），不是等时间。
 *   ② `.catch(() => {})` 吞掉等待失败后继续断言半渲染 DOM：等待超时 → 异常被吞 →
 *      后面的断言跑在一个**没渲染完**的页面上，红的绿的都是假的。全仓 33 处这种
 *      「无理由标注」的空 catch，就是这么攒起来的。
 *   ③ `t.Skip` 静默跳过：`internal/server/static_test.go:120/166/1292` 三个产物级
 *      守卫在 dist 不存在时直接 skip，`go test` 输出仍然是 `ok`。「跳过」和「通过」
 *      在 CI 里长得一模一样 —— 这是最贵的一种假绿。
 *   ④ 裸标签定位器 + `.first()`：`d910474b` / `588f075a` 用
 *      `locator("button").first()` 把「真的命中了两个按钮」（结构回归）静默降级成
 *      「点了第一个」。选择器越宽测试越安全，回归越看不见。
 *
 * 判据（棘轮 ratchet，全部纯静态、零 npm 依赖）：
 *
 *   1. `scripts/e2e/**\/*.mjs` 的 `waitForTimeout(`                      ≤ BASELINE.waitForTimeout
 *   2. 同目录 `.catch(() => {})` / `.catch(()=>{})` / `.catch(() => null)` ≤ BASELINE.swallowCatch
 *   3. `internal/**\/*_test.go` 的 `t.Skip(` / `t.Skipf(`                 ≤ BASELINE.testSkip
 *      且**每一处**都必须在 SKIP_ALLOWLIST 里登记理由；未登记的新增点必红
 *   4. 同目录 `time.Sleep(`                                              ≤ BASELINE.timeSleep
 *   5. `scripts/e2e/**\/*.mjs` 里「裸标签选择器 + 紧跟 `.first()`/`.nth(`」 ≤ BASELINE.bareTagFirstNth
 *   6. **双向棘轮**：当前值**低于**基线也红（「已改善，请同步下调基线」）。
 *      基线一旦过期就变成下一次回归的挡箭牌；本仓已有这条纪律的范本 ——
 *      `internal/server/vocabulary_test.go:303`「白名单不许过期」。
 *
 * 口径（这是判据的一部分，别按印象读）：
 *   · 「计数」= **剥掉注释与字符串之后的**命中数。注释里的 `time.Sleep(`、
 *     字符串里的 `waitForTimeout(` 不算（见 --self-test ⑤⑥）。所以 `grep -c time.Sleep(`
 *     目前是 38，本守卫是 37 —— 差的那一条是 `internal/server/backpressure_test.go:663`
 *     的注释，它只是在解释「裸 sleep 会超时」；`grep -c 'catch(() => {})'` 是 33，
 *     本守卫是 32 —— 差的那一条是 `scripts/e2e/lib/harness.mjs:285` 的注释，它在解释
 *     「调方一旦写 .catch(() => {}) 会拿到旧地址」。这两处「grep 多出来的」正是遮罩存在的理由。
 *   · 第 2 项只数「空 body」与「返回 null」两种形态；`.catch(() => false)` /
 *     `.catch(() => true)`（6 处）、body 里已经写了注释理由的 `.catch(() => { ... })`（2 处）
 *     不在判据内 —— 前者是「返回一个值」，后者已经写了理由。要扩判据就改这里 + 重置基线。
 *   · 第 5 项的「裸标签」= 选择器是**字符串字面量**且整体匹配 `^[a-z][a-z0-9]*$`
 *     （单标签、无 `#`/`.`/`[`/空格/组合符）。`locator('#pb-receipt a').first()`、
 *     `locator('a[href]')` 都不算，因为它们本来就该命中唯一一个；`locator(sel)` /
 *     `locator(ready)` 也不算 —— 变量不是「宽选择器」（这两处是实测出来的假阳性）。
 *     `page.click("button")` 这类动作调用也在「裸标签定位器」的识别面里，但判据要求
 *     **紧跟 `.first()/.nth(`** 才算命中。
 *   · 扫描范围剔除 `node_modules/`、`.pi` 系列目录、`design/`、`_tmp_` 系列、`vendor/`、`.git/`。
 *
 * 用法：
 *   node scripts/check-test-discipline.mjs               # 跑守卫（CI 用这个）
 *   node scripts/check-test-discipline.mjs --self-test   # 守卫自身的红/绿双侧自证（内存 fixture）
 *   node scripts/check-test-discipline.mjs --help
 *
 * 退出码：0 = 五项全部吻合基线；1 = 有一项判据不成立（超基线/低于基线/未登记 skip/文件数过少）；
 *         2 = 用法错误（未知参数）。
 *
 * 接线：计划挂在 `make ci`（docs/lint-guards.md 的 L3 行）。本任务冻结契约只允许新建
 *   本文件，所以 Makefile / ci.yml 的接线由协调者落地；单独跑就是上面的命令。两个模式
 *   都是毫秒级（纯解析，不起浏览器，不跑 go test），适合放进 PR 门禁的最前一道。
 *
 * 边界（明确不做）：
 *   · 不做语法树分析：判据是「形态计数」，宁可少抓一种写法，也不做会误报的启发式。
 *   · 不做语义判断：`.catch(() => null)` 也可能真的是合法兜底 —— 它的成本是「证明一次」，
 *     不是「一定错」。要放行就把计数降到基线以下并**同步下调 BASELINE**（守卫会提醒）。
 *   · 不联网、不依赖仓库外状态、不读写任何被测文件。
 *
 * 基线实测：2026-11-16，main @ `e6fcc8fe`，命令 `node scripts/check-test-discipline.mjs`
 *   （55 个 e2e mjs / 201 个 internal 测试文件）。
 *   五项实测值：waitForTimeout 21 / swallowCatch 38 / testSkip 3 / timeSleep 37 / bareTagFirstNth 3
 *   复现命令（与脚本口径一致，剥注释后的值）见每项注释里的 grep 基线命令。
 */
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

/* ============================ 基线与登记表 ============================ */

/**
 * 棘轮基线（实测值，见头注释的日期与命令）。
 * 只允许**下调**：改善后守卫会红并提示同步下调，这是刻意的 —— 基线过期 = 下一次回归
 * 的挡箭牌。
 */
export const BASELINE = {
	// grep -rn 'waitForTimeout(' scripts/e2e --include='*.mjs' | wc -l  → 21
	waitForTimeout: 21,
	// grep -rn 'catch(() => {})' → 33，'catch(() => null)' → 7，共 40；其中 3 条出现在
	// 注释里（harness.mjs:285 的 `.catch(() => {})`，f3-ui-cases.mjs:854/859 的
	// `.catch(()=>{})`）都在解释反例 —— 剥注释后 32 + 7 = 39
	swallowCatch: 38,
	// grep -rnE 't\.Skipf?\(' internal --include='*_test.go' | wc -l → 3
	testSkip: 3,
	// grep -rn 'time.Sleep(' internal --include='*_test.go' | wc -l → 38，剥掉
	// backpressure_test.go:663 的注释后 37（本守卫口径）
	timeSleep: 37,
	// 裸标签 + .first()/.nth(；实测点：f3-source.mjs:145/147、f3-i18n-cases.mjs:139
	bareTagFirstNth: 3,
};

/** 五项的门面名字（打印用），顺序即判据顺序。 */
export const ITEMS = [
	{ key: "waitForTimeout", name: "e2e/waitForTimeout" },
	{ key: "swallowCatch", name: "e2e/swallow-catch" },
	{ key: "testSkip", name: "go/t.Skip" },
	{ key: "timeSleep", name: "go/time.Sleep" },
	{ key: "bareTagFirstNth", name: "e2e/bare-tag-first-nth" },
];

/**
 * `t.Skip` 登记表：**每一处**跳过都必须在这里说明为什么跳过是可接受的。
 * 匹配方式 = 同文件 + 行文本（空白折叠后）包含 `match`；所以行号漂移不会假红，
 * 但「跳过理由被改写」会红（提醒你同步登记）。
 * 未被任何命中点用到的条目 = 过期条目 = 红（`vocabulary_test.go:303` 的纪律）。
 */
export const SKIP_ALLOWLIST = [
	{
		file: "internal/server/static_test.go",
		match: "dist 不存在（先 make fe 生成 embed 产物），跳过 dist 侧守卫: %v",
		reason:
			"embed 产物由 make fe 生成，干净 checkout 上不存在；本体是产物级守卫，resolved by CI 里先 make fe 再 make test（见 L3 判据③指出的问题：这里 skip 后 go test 仍报 ok，运行期缺口由 backlog 5「-json 断言 skip 集合为空」兜）",
	},
	{
		file: "internal/server/static_test.go",
		match: "dist 不存在（先 make fe 生成 embed 产物），跳过: %v",
		reason: "同上：dist 缺失时无法读产物清单",
	},
	{
		file: "internal/server/static_test.go",
		match: "dist 不存在（先 make fe 生成 embed 产物），跳过产物级 HTTP 护栏: %v",
		reason: "同上：真 dist 的 HTTP 护栏需要真实 embed 产物",
	},
];

/**
 * 扫描文件数下限（反假绿：解析出 0 文件 = 守卫失明，不是绿）。
 * 判定是「**不得少于**下限」而不是「等于」：并行开发每天都在加文件，等于会天天假红；
 * 少了下限则一定是有文件没被解析（过滤写错、目录改名、读失败）—— 那才是真问题。
 */
export const MIN_SCANNED_FILES = {
	// find scripts/e2e -name '*.mjs' | wc -l → 55（2026-11-16 实测）
	e2e: 55,
	// find internal -name '*_test.go' | wc -l → 201（2026-11-16 实测；随后兄弟任务又加 1 个）
	go: 201,
};

/** 剔除的目录段（非目标目录，避免误报与拖慢）。 */
export const EXCLUDED_SEGMENTS = [
	/^node_modules$/,
	/^vendor$/,
	/^design$/,
	/^\.git$/,
	/^\.pi/, // .pi / .pi-orchestrator / .pi-subagents …
	/^_tmp_/, // 临时工作目录 _tmp_pbvalidate 之类
];

/* ============================ 纯函数：词法遮罩 ============================ */

const REGEX_PREV_CHARS = new Set("(,=:[!&|?{};+-*%~^<>".split(""));
const REGEX_PREV_WORDS = new Set([
	"return", "typeof", "case", "in", "of", "new", "delete", "void",
	"instanceof", "do", "else", "yield", "await",
]);

function skipQuoted(text, i) {
	const quote = text[i];
	i++;
	while (i < text.length) {
		if (text[i] === "\\") {
			i += 2;
			continue;
		}
		if (text[i] === quote) return i + 1;
		i++;
	}
	return i;
}

function skipRegex(text, i) {
	i++; // 跳过起始 `/`
	let inClass = false;
	while (i < text.length) {
		const c = text[i];
		if (c === "\\") {
			i += 2;
			continue;
		}
		if (c === "\n") return i; // 正则不能跨行：说明这不是正则，退回原样
		if (c === "[") inClass = true;
		else if (c === "]") inClass = false;
		else if (c === "/" && !inClass) return i + 1;
		i++;
	}
	return i;
}

/**
 * maskCode(text, lang) —— 返回与 text 等长的 Uint8Array：
 * 1 = 真正的代码字符，0 = 注释 / 字符串 / 模板字面量 / 正则字面量的内容。
 *
 * 为什么要遮罩而不是用 grep：注释里写 `time.Sleep(` 是在解释反例，不该计入棘轮；
 * 字符串里写 `waitForTimeout(` 是文档/fixture，也不该计入。**误报会让开发者
 * 去改注释来"过守卫"**，那是把守卫变成噪音的第一步。
 *
 * 换行符属于代码（mask=1），这样行号/列号不会漂。
 */
export function maskCode(text, lang = "js") {
	const n = text.length;
	const mask = new Uint8Array(n);
	const isGo = lang === "go";
	// 容器栈：code（普通代码 / 模板的 ${} 表达式）与 template（模板字面量正文）
	const stack = [{ kind: "code" }];
	let i = 0;
	let prev = ""; // 上一个非空白代码字符（判断 `/` 是除号还是正则）
	let lastWord = ""; // 上一个标识符（判断 return /re/ 这类）

	while (i < n) {
		const top = stack[stack.length - 1];
		const c = text[i];

		if (top.kind === "template") {
			if (c === "\\") {
				i += 2;
				continue;
			}
			if (c === "`") {
				stack.pop();
				prev = "`";
				lastWord = "";
				i++;
				continue;
			}
			// Go 的 raw string 没有插值，整段当字符串正文跳过
			if (!isGo && c === "$" && text[i + 1] === "{") {
				stack.push({ kind: "code", template: true, depth: 0 });
				prev = "{";
				lastWord = "";
				i += 2;
				continue;
			}
			i++;
			continue;
		}

		// ---- code ----
		if (c === "/" && text[i + 1] === "/") {
			while (i < n && text[i] !== "\n") i++;
			continue;
		}
		if (c === "/" && text[i + 1] === "*") {
			i += 2;
			while (i < n && !(text[i] === "*" && text[i + 1] === "/")) i++;
			i += 2;
			continue;
		}
		if (c === '"' || c === "'") {
			i = skipQuoted(text, i);
			prev = c;
			lastWord = "";
			continue;
		}
		if (c === "`") {
			stack.push({ kind: "template" });
			prev = "`";
			lastWord = "";
			i++;
			continue;
		}
		if (!isGo && c === "/" && (prev === "" || REGEX_PREV_CHARS.has(prev) || REGEX_PREV_WORDS.has(lastWord))) {
			i = skipRegex(text, i);
			prev = "/";
			lastWord = "";
			continue;
		}

		if (top.template) {
			if (c === "{") top.depth++;
			else if (c === "}") {
				if (top.depth === 0) {
					stack.pop();
					prev = "}";
					lastWord = "";
					i++;
					continue;
				}
				top.depth--;
			}
		}

		mask[i] = 1;
		if (/[A-Za-z0-9_$]/.test(c)) {
			lastWord = i > 0 && /[A-Za-z0-9_$]/.test(text[i - 1]) ? lastWord + c : c;
		} else {
			lastWord = "";
		}
		if (!/\s/.test(c)) prev = c;
		i++;
	}
	return mask;
}

/* ============================ 纯函数：扫描 ============================ */

function lineStarts(text) {
	const starts = [0];
	for (let i = 0; i < text.length; i++) if (text[i] === "\n") starts.push(i + 1);
	return starts;
}

function position(starts, idx) {
	let lo = 0;
	let hi = starts.length - 1;
	while (lo < hi) {
		const mid = (lo + hi + 1) >> 1;
		if (starts[mid] <= idx) lo = mid;
		else hi = mid - 1;
	}
	return { line: lo + 1, col: idx - starts[lo] + 1 };
}

/** buildSource(path, text, lang) —— 扫描单元（纯函数，--self-test 直接喂内存字符串）。 */
export function buildSource(filePath, text, lang) {
	return { path: filePath, text, lang, mask: maskCode(text, lang), starts: lineStarts(text) };
}

function hitAt(src, idx, snippet) {
	const { line, col } = position(src.starts, idx);
	return { path: src.path, line, col, snippet };
}

/** 逐个正则扫（只认遮罩为代码的命中点）。 */
function scanPattern(sources, regexes) {
	const hits = [];
	for (const src of sources) {
		for (const re of regexes) {
			re.lastIndex = 0;
			let m;
			while ((m = re.exec(src.text)) !== null) {
				if (m[0].length === 0) {
					re.lastIndex++;
					continue;
				}
				if (src.mask[m.index] !== 1) continue; // 注释/字符串里：不算
				hits.push(hitAt(src, m.index, m[0].trim()));
			}
		}
	}
	return hits.sort((a, b) => (a.path === b.path ? a.line - b.line : a.path < b.path ? -1 : 1));
}

/** ① e2e 的 `waitForTimeout(` */
export function scanWaitForTimeout(sources) {
	return scanPattern(sources, [/waitForTimeout\s*\(/g]);
}

/** ② e2e 的吞异常 catch（空 body / 返回 null） */
export function scanSwallowedCatch(sources) {
	return scanPattern(sources, [
		/\.catch\(\s*\(\s*\)\s*=>\s*\{\s*\}\s*\)/g,
		/\.catch\(\s*\(\s*\)\s*=>\s*null\s*\)/g,
	]);
}

/** ③ go 测试里的 `t.Skip(` / `t.Skipf(` */
export function scanTestSkips(sources) {
	return scanPattern(sources, [/\bt\.Skipf?\s*\(/g]);
}

/** ④ go 测试里的 `time.Sleep(` */
export function scanTimeSleep(sources) {
	return scanPattern(sources, [/\btime\.Sleep\s*\(/g]);
}

/* ---- ⑤ 裸标签选择器 + 紧跟 .first()/.nth( ---- */

const SELECTOR_METHODS = [
	"locatorAll", "locator",
	"waitForSelector", "waitForFunction",
	"dblclick", "dispatchEvent", "dragAndDrop",
	"click", "tap", "hover", "fill", "press", "type", "selectOption",
	"check", "uncheck", "isVisible", "isHidden", "textContent", "innerText",
	"getAttribute", "setInputFiles", "boundingBox", "focus", "blur",
];

/** 裸标签 = 单个 html 标签名（无 #/./[/空格/组合符）。 */
export function isBareTagSelector(selector) {
	return /^[a-z][a-z0-9]*$/.test(selector);
}

function matchParen(text, mask, open) {
	let depth = 0;
	for (let i = open; i < text.length; i++) {
		if (mask[i] !== 1) continue; // 字符串/注释里的括号不参与配对
		const c = text[i];
		if (c === "(") depth++;
		else if (c === ")") {
			depth--;
			if (depth === 0) return i;
		}
	}
	return -1;
}

function firstArgRaw(text, mask, open, close) {
	let depth = 0;
	for (let i = open + 1; i < close; i++) {
		if (mask[i] !== 1) continue;
		const c = text[i];
		if (c === "(" || c === "[" || c === "{") depth++;
		else if (c === ")" || c === "]" || c === "}") depth--;
		else if (c === "," && depth === 0) return text.slice(open + 1, i);
	}
	return text.slice(open + 1, close);
}

/**
 * unquoteLiteral —— 只接受**引号包住的字符串字面量**，其它返回 null。
 * 这条很关键：`locator(sel)` / `locator(ready)` 里的 `sel` 是变量，形如裸标签名
 * 但根本不是「宽选择器」——把它算进来就是假阳性（实测误报过 2 处：
 * f3-i18n-cases.mjs:35 的 `locator(sel)` 与 parent-repair-evidence.mjs:43 的
 * `locator(ready)`）。
 */
function unquoteLiteral(raw) {
	const s = raw.trim();
	if (s.length < 2) return null;
	const q = s[0];
	if (q !== '"' && q !== "'" && q !== "`") return null;
	if (s[s.length - 1] !== q) return null;
	return s.slice(1, -1);
}

function nextCodeToken(text, mask, from) {
	for (let i = from; i < text.length; i++) {
		if (mask[i] === 1 && !/\s/.test(text[i])) return i;
	}
	return -1;
}

/**
 * ⑤ 「裸标签定位器后紧跟 `.first()` / `.nth(`」。
 * 判据 = 选择器是裸标签（宽定位）**且**紧接着用 `.first()/.nth(` 取其中一个 ——
 * 这两件事同时出现，就等于「真的双命中」被静默降级成「点了第一个」。
 */
export function scanBareTagFirstNth(sources) {
	const callRe = new RegExp(`\\.\\s*(${SELECTOR_METHODS.join("|")})\\s*\\(`, "g");
	const hits = [];
	for (const src of sources) {
		const { text, mask } = src;
		callRe.lastIndex = 0;
		let m;
		while ((m = callRe.exec(text)) !== null) {
			if (mask[m.index] !== 1) continue;
			const open = m.index + m[0].length - 1;
			const close = matchParen(text, mask, open);
			if (close < 0) continue;
			const selector = unquoteLiteral(firstArgRaw(text, mask, open, close));
			if (selector === null || !isBareTagSelector(selector)) continue;
			const tok = nextCodeToken(text, mask, close + 1);
			if (tok < 0 || text[tok] !== ".") continue;
			const follow = /^\.\s*(first|nth)\s*\(/.exec(text.slice(tok));
			if (!follow) continue;
			hits.push(hitAt(src, m.index, `.${m[1]}(${selector}) → ${follow[1]}()`));
		}
	}
	return hits.sort((a, b) => (a.path === b.path ? a.line - b.line : a.path < b.path ? -1 : 1));
}

/* ============================ 纯函数：棘轮判定 ============================ */

function normalizeWs(s) {
	return String(s ?? "").replace(/\s+/g, " ").trim();
}

function lineTextOf(src, line) {
	const { text, starts } = src;
	const start = starts[line - 1] ?? 0;
	const end = line < starts.length ? starts[line] - 1 : text.length;
	return text.slice(start, end);
}

/**
 * evaluate —— 棘轮判定（纯函数，--self-test 就是靠它做红/绿双侧自证）。
 *
 * 入参：
 *   sources  = buildSource 的数组（用于把 skip 命中点对到登记表行文本）
 *   counts   = { waitForTimeout, swallowCatch, testSkip, timeSleep, bareTagFirstNth }
 *   skipHits = scanTestSkips 的命中点（未登记即红）
 *   baseline / allowlist / scanned / minScanned / items
 * 出参：{ rows, issues }；rows 恒为五项（打印 `name: count / baseline`）。
 */
export function evaluate({
	sources = [],
	counts = {},
	baseline = BASELINE,
	skipHits = [],
	allowlist = SKIP_ALLOWLIST,
	scanned = {},
	minScanned = MIN_SCANNED_FILES,
	items = ITEMS,
} = {}) {
	const rows = [];
	const issues = [];

	// ---- 反假绿：文件数下限 ----
	for (const [kind, min] of Object.entries(minScanned)) {
		const got = scanned[kind] ?? 0;
		if (got < min) {
			issues.push({
				kind: "files-too-few",
				subject: kind,
				message:
					got === 0
						? `扫描到 0 个 ${kind} 文件 —— 守卫失明，不是绿（路径/过滤写错了？）`
						: `只扫描到 ${got} 个 ${kind} 文件，低于下限 ${min} —— 解析漏文件即假绿`,
			});
		}
	}

	// ---- 双向棘轮（超也红、低也红） ----
	for (const item of items) {
		const count = counts[item.key] ?? 0;
		const base = baseline[item.key] ?? 0;
		let status = "ok";
		if (count > base) {
			status = "over";
			issues.push({
				kind: "over",
				subject: item.key,
				message: `${item.name}: ${count} > 基线 ${base}（新增了 ${count - base} 处）`,
			});
		} else if (count < base) {
			status = "under";
			issues.push({
				kind: "under",
				subject: item.key,
				message: `${item.name}: ${count} < 基线 ${base} —— 已改善，请同步下调 BASELINE.${item.key} 到 ${count}`,
			});
		}
		rows.push({ key: item.key, name: item.name, count, baseline: base, status });
	}

	// ---- `t.Skip` 登记表：未登记即红 ----
	const byPath = new Map(sources.map((s) => [s.path, s]));
	const usedEntries = new Set();
	for (const hit of skipHits) {
		const src = byPath.get(hit.path);
		const lineText = src ? normalizeWs(lineTextOf(src, hit.line)) : "";
		let matched = -1;
		allowlist.forEach((entry, idx) => {
			if (entry.file !== hit.path) return;
			if (!lineText.includes(normalizeWs(entry.match))) return;
			if (matched < 0) matched = idx;
		});
		if (matched < 0) {
			issues.push({
				kind: "skip-unregistered",
				subject: hit.path,
				message: `${hit.path}:${hit.line} 的 t.Skip 没在 SKIP_ALLOWLIST 里登记理由：${hit.snippet}`,
			});
			continue;
		}
		usedEntries.add(matched);
	}

	// ---- 「白名单不许过期」：没写理由 / 已无对应命中点的条目 ----
	allowlist.forEach((entry, idx) => {
		if (!normalizeWs(entry.reason)) {
			issues.push({
				kind: "allowlist-no-reason",
				subject: entry.file,
				message: `SKIP_ALLOWLIST 第 ${idx + 1} 条（${entry.file}）没写理由 —— 豁免必须说明为什么`,
			});
		}
		if (!usedEntries.has(idx)) {
			issues.push({
				kind: "allowlist-stale",
				subject: entry.file,
				message: `SKIP_ALLOWLIST 第 ${idx + 1} 条（${entry.file}：${entry.match}）已无对应跳过点，请删掉这条过期豁免`,
			});
		}
	});

	return {
		rows,
		issues,
		// stats 只用于打印“登记表是否真的活着”（3/3 命中比一句“绿”信息量大）
		stats: {
			skipHits: skipHits.length,
			skipRegistered: usedEntries.size,
			allowlistEntries: allowlist.length,
		},
	};
}

/* ============================ 从磁盘收集文件 ============================ */

function excluded(relPath) {
	return relPath.split("/").some((seg) => EXCLUDED_SEGMENTS.some((re) => re.test(seg)));
}

function walk(relDir, accept) {
	const out = [];
	const root = path.join(ROOT, relDir);
	if (!fs.existsSync(root)) return out;
	const stack = [relDir];
	while (stack.length) {
		const dir = stack.pop();
		for (const ent of fs.readdirSync(path.join(ROOT, dir), { withFileTypes: true })) {
			const rel = `${dir}/${ent.name}`;
			if (excluded(rel)) continue;
			if (ent.isDirectory()) stack.push(rel);
			else if (accept(rel)) out.push(rel);
		}
	}
	return out.sort();
}

export function collectSources() {
	const e2eFiles = walk("scripts/e2e", (p) => p.endsWith(".mjs"));
	const goFiles = walk("internal", (p) => p.endsWith("_test.go"));
	const read = (p) => fs.readFileSync(path.join(ROOT, p), "utf8");
	const e2e = e2eFiles.map((p) => buildSource(p, read(p), "js"));
	const go = goFiles.map((p) => buildSource(p, read(p), "go"));
	return { e2e, go, files: { e2e: e2eFiles, go: goFiles } };
}

/* ============================ --self-test ============================ */

const E2E = "scripts/e2e/selftest-fixture.mjs";
const GO = "internal/selftest/fixture_test.go";

function zeroBaseline(overrides = {}) {
	return {
		waitForTimeout: 0,
		swallowCatch: 0,
		testSkip: 0,
		timeSleep: 0,
		bareTagFirstNth: 0,
		...overrides,
	};
}

/** 把一个内存 fixture 跑完整判据（与真实运行同一条纯函数链）。 */
function runFixture({ e2eText = "", goText = "", baseline = zeroBaseline(), allowlist = [] }) {
	const e2e = e2eText ? [buildSource(E2E, e2eText, "js")] : [];
	const go = goText ? [buildSource(GO, goText, "go")] : [];
	const sources = [...e2e, ...go];
	const counts = {
		waitForTimeout: scanWaitForTimeout(e2e).length,
		swallowCatch: scanSwallowedCatch(e2e).length,
		testSkip: scanTestSkips(go).length,
		timeSleep: scanTimeSleep(go).length,
		bareTagFirstNth: scanBareTagFirstNth(e2e).length,
	};
	const res = evaluate({
		sources,
		counts,
		baseline,
		skipHits: scanTestSkips(go),
		allowlist,
		scanned: { e2e: e2e.length, go: go.length },
		minScanned: { e2e: e2e.length, go: go.length },
	});
	return { ...res, counts };
}

function selfTest() {
	let failed = 0;
	const check = (name, cond, detail = "") => {
		if (cond) {
			console.log(`  ✅ ${name}`);
		} else {
			failed++;
			console.error(`  ❌ ${name}${detail ? ` — ${detail}` : ""}`);
		}
	};
	const kinds = (res) => res.issues.map((i) => i.kind);

	// ① 超基线必须红
	{
		const res = runFixture({
			e2eText: "await page.waitForTimeout(100);\nawait page.waitForTimeout(200);\n",
			baseline: zeroBaseline({ waitForTimeout: 1 }),
		});
		check("① 超基线 → 红", res.counts.waitForTimeout === 2 && kinds(res).includes("over"), JSON.stringify(res.counts));
	}

	// ② 低于基线必须红（提示同步下调）
	{
		const res = runFixture({
			e2eText: "await page.waitForTimeout(100);\n",
			baseline: zeroBaseline({ waitForTimeout: 3 }),
		});
		const under = res.issues.find((i) => i.kind === "under");
		check("② 低于基线 → 红并提示同步下调", !!under && under.message.includes("BASELINE.waitForTimeout"), JSON.stringify(res.issues));
	}

	// ③ 等于基线必须绿
	{
		const res = runFixture({
			e2eText: "await page.waitForTimeout(100);\nawait page.waitForTimeout(200);\n",
			baseline: zeroBaseline({ waitForTimeout: 2 }),
		});
		check("③ 等于基线 → 绿", res.issues.length === 0, JSON.stringify(res.issues));
	}

	// ④ ALLOWLIST 未登记的 t.Skip 必须红
	{
		const goText = 'func TestX(t *testing.T) {\n\tt.Skip("dist 不存在")\n}\n';
		const unregistered = runFixture({ goText, baseline: zeroBaseline({ testSkip: 1 }) });
		check("④ 未登记的 t.Skip → 红", kinds(unregistered).includes("skip-unregistered"), JSON.stringify(kinds(unregistered)));
		const registered = runFixture({
			goText,
			baseline: zeroBaseline({ testSkip: 1 }),
			allowlist: [{ file: GO, match: 't.Skip("dist 不存在")', reason: "fixture：产物缺失时无法检查" }],
		});
		check("④' 已登记的 t.Skip → 绿", registered.issues.length === 0, JSON.stringify(registered.issues));
		const noReason = runFixture({
			goText,
			baseline: zeroBaseline({ testSkip: 1 }),
			allowlist: [{ file: GO, match: 't.Skip("dist 不存在")', reason: "" }],
		});
		check("④'' 登记但没写理由 → 红", kinds(noReason).includes("allowlist-no-reason"), JSON.stringify(kinds(noReason)));
	}

	// ⑤ 注释里的 time.Sleep( 不得误报；同一行的真调用必须数到
	{
		const goText = [
			"func TestY(t *testing.T) {",
			"\t// 裸 time.Sleep(1h) 会让这里超时（反例，不是调用）",
			"\t/* time.Sleep(time.Second) 块注释里的目录也不是调用 */",
			'\tmsg := "time.Sleep(time.Second) 字符串里也不是调用"',
			"\t_ = msg",
			"\ttime.Sleep(time.Millisecond) // 只有这一处是真的",
			"}",
			"",
		].join("\n");
		const res = runFixture({ goText });
		check("⑤ 注释/字符串里的 time.Sleep( 不算命中", res.counts.timeSleep === 1, JSON.stringify(res.counts));
	}

	// ⑥ 字符串里的 waitForTimeout( 不得误报
	{
		const res = runFixture({ e2eText: 'const doc = "await page.waitForTimeout(100)";\n' });
		check("⑥ 字符串里的 waitForTimeout( 不算命中", res.counts.waitForTimeout === 0, JSON.stringify(res.counts));
	}

	// ⑦ 裸标签 + .first()/.nth( 命中；带 id/属性的宽选择器不命中
	{
		const res = runFixture({
			e2eText: [
				"await page.locator('a').first().click();",
				"await row.locator(\"button\").nth(2).click();",
				"await page.locator('#pb-receipt a').first().click();",
				"await page.locator('a[href]').first().click();",
				"await page.locator('body').innerText();",
				"// await page.locator('a').first();  // 注释里不算",
				"",
			].join("\n"),
		});
		check("⑦ 只数「裸标签 + first/nth」两处", res.counts.bareTagFirstNth === 2, JSON.stringify(res.counts));
	}

	// ⑧ ALLOWLIST 过期条目必须红
	{
		const res = runFixture({
			goText: "func TestZ(t *testing.T) {}\n",
			allowlist: [{ file: GO, match: "已经不存在的跳过点", reason: "fixture" }],
		});
		check("⑧ 过期登记条目 → 红", kinds(res).includes("allowlist-stale"), JSON.stringify(kinds(res)));
	}

	// ⑨ 文件数不足必须红（含 0 文件 = 失明）
	{
		const zero = evaluate({
			sources: [],
			counts: zeroBaseline(),
			baseline: zeroBaseline(),
			scanned: { e2e: 0, go: 0 },
			minScanned: { e2e: 55, go: 201 },
		});
		check(
			"⑨ 0 文件 → 红（守卫失明不是绿）",
			zero.issues.filter((i) => i.kind === "files-too-few").length === 2,
			JSON.stringify(kinds(zero)),
		);
	}

	// ⑩ 吞异常 catch 的三种形态都数，带理由的注释 body 不数
	{
		const res = runFixture({
			e2eText: [
				"await p.waitForSelector('x').catch(() => {});",
				"await p.waitForSelector('x').catch(()=>{});",
				"await p.waitForSelector('x').catch(() => null);",
				"await p.waitForSelector('x').catch(() => { /* 有理由 */ });",
				"await p.waitForSelector('x').catch(() => false);",
				"",
			].join("\n"),
		});
		check("⑩ 只数空 body / null 两种吞异常形态（3 处）", res.counts.swallowCatch === 3, JSON.stringify(res.counts));
	}

	// —— 以下是实测出来的两类假阳性，必须钉住（否则底座一旦“修正”成宽口径就全仓红）——

	// ⑪ 选择器是变量时不算「裸标签」：locator(sel) / locator(ready) 不是宽选择器
	{
		const res = runFixture({
			e2eText: [
				"await page.locator(sel).first().click();",
				"await page.locator(ready).first().click();",
				"await page.locator('a').first().click();",
				"",
			].join("\n"),
		});
		check("⑪ 变量选择器不误报（只数字符串字面量那 1 处）", res.counts.bareTagFirstNth === 1, JSON.stringify(res.counts));
	}

	// ⑫ 注释里的 `.catch(() => {})` 不算（harness.mjs:285 就是这种：它在解释反例）
	{
		const res = runFixture({
			e2eText: [
				"// 调用方一旦写 .catch(() => {}) 就拿到旧地址",
				"await p.waitForSelector('x').catch(() => {});",
				"",
			].join("\n"),
		});
		check("⑫ 注释里的吞异常不误报", res.counts.swallowCatch === 1, JSON.stringify(res.counts));
	}

	if (failed > 0) {
		console.error(`❌ 守卫自测失败（${failed} 项）—— 先修守卫，别让它假绿`);
		return 1;
	}
	console.log("  ✅ 守卫自测通过（超/低/等/登记/过期/注释/字符串/裸标签/文件数/吞异常/两类假阳性 十二条路都钉住了）");
	return 0;
}

/* ============================ CLI ============================ */

function pad(s, n) {
	return s.length >= n ? s : s + " ".repeat(n - s.length);
}

function runCheck() {
	const { e2e, go } = collectSources();
	const sources = [...e2e, ...go];

	const counts = {
		waitForTimeout: scanWaitForTimeout(e2e).length,
		swallowCatch: scanSwallowedCatch(e2e).length,
		testSkip: scanTestSkips(go).length,
		timeSleep: scanTimeSleep(go).length,
		bareTagFirstNth: scanBareTagFirstNth(e2e).length,
	};
	const skipHits = scanTestSkips(go);

	const { rows, issues, stats } = evaluate({
		sources,
		counts,
		skipHits,
		scanned: { e2e: e2e.length, go: go.length },
	});

	console.log("🔍 测试纪律棘轮（docs/lint-guards.md · L3）");
	console.log(
		`   扫描 ${pad("scripts/e2e/**/*.mjs", 24)} = ${e2e.length} 个（下限 ${MIN_SCANNED_FILES.e2e}）`,
	);
	console.log(
		`   扫描 ${pad("internal/**/*_test.go", 24)} = ${go.length} 个（下限 ${MIN_SCANNED_FILES.go}）`,
	);
	console.log("");

	const width = Math.max(...rows.map((r) => r.name.length));
	for (const r of rows) {
		const marker = r.status === "ok" ? "✅" : r.status === "over" ? "❌ 超基线" : "❌ 低于基线";
		console.log(`   ${pad(r.name, width)}: ${String(r.count).padStart(3)} / ${String(r.baseline).padStart(3)}   ${marker}`);
	}
	console.log("");

	const detail = {
		over: "新增的这类写法回到了历史事故的形态：请改代码（有条件的等待、显式断言、收窄选择器），别上调基线。",
		under: "已改善：把 BASELINE 下调到实测值再提交（基线过期会变成下一次回归的挡箭牌）。",
		"skip-unregistered": "跳过 = 静默的绿。要么让它在 CI 里真的跑（先 make fe 生成 embed 产物），要么在 SKIP_ALLOWLIST 登记理由。",
		"allowlist-no-reason": "豁免必须写清「为什么跳过的代价可以接受」。",
		"allowlist-stale": "登记表不许过期：清掉已经没有对应跳过点的条目。",
		"files-too-few": "守卫自己的扫描范围出问题了（过滤/路径/解析），先修守卫 —— 它现在什么都看不见。",
	};

	if (issues.length === 0) {
		console.log(
			`✅ 五项全部与基线一致（t.Skip 登记 ${stats.skipRegistered}/${stats.skipHits} 命中）—— ` +
				"有改善请同步下调基线；新增必须在此刻被拦住",
		);
		return 0;
	}

	const byKind = new Map();
	for (const it of issues) {
		if (!byKind.has(it.kind)) byKind.set(it.kind, []);
		byKind.get(it.kind).push(it);
	}
	console.error(`❌ 测试纪律守卫不通过（${issues.length} 项）：`);
	for (const [kind, list] of byKind) {
		console.error("");
		console.error(`  · ${detail[kind] ?? kind}`);
		for (const it of list) console.error(`    - ${it.message}`);
	}
	return 1;
}

function usage() {
	console.error(
		[
			"用法：",
			"  node scripts/check-test-discipline.mjs              跑棘轮守卫（五项，超/低于基线都红）",
			"  node scripts/check-test-discipline.mjs --self-test  守卫自身的红/绿双侧自证（内存 fixture）",
			"  node scripts/check-test-discipline.mjs --help",
			"退出码：0 全绿 / 1 判据不成立 / 2 用法错误",
		].join("\n"),
	);
}

function main() {
	const argv = process.argv.slice(2);
	for (const a of argv) {
		if (a === "--self-test") return selfTest();
		if (a === "--help" || a === "-h") {
			usage();
			return 0;
		}
		if (a !== "--check") {
			console.error(`未知参数：${a}`);
			usage();
			return 2;
		}
	}
	return runCheck();
}

// 只在「被当作脚本执行」时跑 CLI：本文件同时导出纯函数（--self-test 与将来的
// 单测都要能 import 它而不触发一次全仓扫描）。
const invokedDirectly =
	process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (invokedDirectly) process.exitCode = main();
