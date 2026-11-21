#!/usr/bin/env node
/* Anotify · 用户可见文案检查：拦住「开发过程叙述」漏进产品界面（docs/lint-guards.md 的 L2）
 *
 * 拦的事故
 *   `9ec8afff`（PR #447）：节点页上挂着一整块卡片——
 *     「命名空间与系统配置不在这页 / 命名空间 vars 是命名空间级配置（已迁到命名空间页）…」
 *   用户在节点页看到后直接问「搞笑吧？你没有审核出来吗？」。
 *   它不是功能说明，是**开发过程的旁白**：讲这块东西搬到哪去了、以前在哪。
 *   为什么现有检查没抓到：check-i18n-keys 只验「key 在 4 份 locale 里都存在 + 集合一致」，
 *   e2e 只验页面 200 / 元素在。**「这条文案写得对不对」整个没有检查面**，
 *   所以只要 key 补全了，任何旁白都能一路绿到生产。
 *
 * 判据（两个扫描面，缺一就是半盲）
 *   ① `web-src/locales/*.yaml` 的**值**：跳过注释行与 key 名；值里的 HTML 标签不剥
 *      （泄漏点也可能在 <b>/<code> 里）。
 *   ② `web-src/pages/*.html` 的**文本节点**：只留真的会渲染给用户的那部分（#458）。
 *   两个面命中都判失败，逐条打印 `文件:行 → 命中词 → 原文`。
 *
 * 为什么页面也必须扫（#458）：#448 只扫 locale 值，理由写的是「页面里没有散文」——
 *   那条实测只对**改完之后**的页面成立，却把「现在没有」当成了「不会有」。
 *   2026-11 审计里最露骨的一句「下面这些是写给维护者与排障者的说明」，当时就写在
 *   `docs` 页的 <p> 里（#448 才把它换成 {{t "docs.design.lede"}}）；同一页还有
 *   「为什么界面这样显示」这类标题。既然散文本来就会落在页面上，扫描面不覆盖页面 = 漏一半。
 *
 * 页面侧「哪些字算用户可见」（正反都在 --self-test 的 page 样例里测过）：
 *   排除 · HTML 注释 <!-- -->（写给维护者的，本来就该在）
 *   排除 · 模板注释段（`{{` 与 `}}` 之间以斜杠星号开头的内容）、模板表达式 {{t "…"}} / {{if …}} / {{range …}}（逻辑，不渲染成文字）
 *   排除 · <script> / <style> 的内容（脚本与样式，不是文案）
 *   排除 · 标签本身：属性名、属性值、类名都不是文本节点
 *   排除 · 没被 layout 的 {{block "…"}} 接进渲染树的 define 体：sitegen 只执行 __layout__
 *          （internal/sitegen/template.go 的 execute），页面顶层文字与死 define 永远进不了产物
 *   保留 · 其余文本节点（含 <pre> 里的代码样例 —— 它们真的显示给用户；词表窄，实测零误报）
 *
 * 词表怎么定的（窄而准，靠实测不是靠感觉）
 *   先按 5 类特征（迁移叙述 / 对照内部实现 / 对开发者喊话 / 解释「为什么这么设计」/
 *   悬空指代）全量扫一遍 4 份 locale，逐条人工判定后**只留零误报的词**：
 *   定稿前在修完的 locale 上跑过，命中为零（见 docs/reports/ui-copy-audit.md）。
 *   被**故意排除**的高频词（都会误报，理由逐条记录）：
 *     · 「make + 目标名」曾整条排除（`make dev` 被视为「自托管排障指引」）。#458
 *       纠正了这个判断：产品界面是部署后的东西，用户手上不一定有本仓 checkout，
 *       界面里不出现构建命令。现在按**真实 target 名单**查（`make dev` / `make fe` /
 *       `make sitegen` …），英文的 "make it / make sure" 是动词，不在名单里。
 *     · `\b[0-9a-f]{7,40}\b`（commit sha 形状）：会命中 `2592000` 这种秒数
 *     · `移行`（ja）：`…リアルタイムストリームに移行します` 是数据流状态迁移，不是「配置搬家」
 *     · `旧 `（ja/zh 裸词）：`更旧 →`（分页）、`旧 digest`（真实陈旧态标签）都是正常文案
 *     · `legacy`：只保留在 en 词表里（本仓当前没有正常用法），中文侧用「旧 spec/旧形态」等具体词
 *     · `XXX`、`TXXX`：`XXXX-XXXX` 是恢复码的格式提示，是正常文案
 *
 * 允许行内豁免
 *   locale：值所在行、或**上一行**的注释里写 `# ui-copy-ok: <理由>`。
 *   页面：文本节点所在行、或**上一行**的注释里写 `<!-- ui-copy-ok: <理由> -->`。
 *   理由必填：空理由（`ui-copy-ok:` 后面什么都没有）仍然失败 —— 否则
 *   「加个标记就过」会变成新的假绿通道。
 *
 * 反假绿
 *   · `--self-test` 跑内置红/绿样例（locale 20 红 / 13 绿 / 5 豁免；页面 6 红 / 7 绿 / 4 豁免），
 *     红样例必须真红，且断言**是哪条规则**红的（防「碰巧被别的规则抓到」）；页面样例
 *     另外断言行号，绿样例还钉住**抽到多少个文本节点**（扫出空集不能算绿）。
 *   · 打印 文件数 / 值数 / 页面数 / 文本节点数，四个数各设下限：解析出空集一律
 *     判失败 ——「绿」与「没检查」不能同形（本仓反复吃这个亏）。
 *   · locale YAML 解析复用 check-i18n-keys.mjs 的 parseLocale：遇到不认识的语法
 *     抛 ParseError → 退出码 2，绝不静默跳过；页面目录 / layout 目录读不到也是退出码 2。
 *
 * 用法
 *   node scripts/check-ui-copy.mjs                 # 判据校验（locale 值 + 页面文本节点），违规退出 1
 *   node scripts/check-ui-copy.mjs --self-test     # 跑脚本自身的红/绿自测
 *   node scripts/check-ui-copy.mjs --src=web-src   # 声明式全扫（locales/ + pages/ + layouts/ 都在这个根下）
 *   退出码：0 绿 / 1 违规 / 2 解析失败或用法错误
 *   接线：`make ci`（紧跟 check-i18n-keys；Makefile 由主 agent 统一收口，本脚本不自注册）
 */
import { promises as fs } from "node:fs";
import path from "node:path";
import { LANGS, ParseError, extractRefs, parseLocale } from "./check-i18n-keys.mjs";
import { runsAsMain } from "./lib/direct-run.mjs";

/** 豁免标记（值所在行或上一行的注释里）。理由必填。 */
export const EXEMPT_MARKER = "ui-copy-ok";

/**
 * 高危词表。每条 = 一类「用户不该看到的叙述」，配一个能在 4 种语言里都写出来的 `why`。
 * `re` 一律用 `g` 之外的形式（只报第一处），命中文本由调用方用同一条正则取。
 */
export const RULES = [
	// ── 语言无关：内部实现 / 构建管线 / 工单号 ──
	{
		id: "issue-ref",
		why: "工单号是开发过程的坐标，用户看不到也不该看到",
		re: /(?<![&\w])#\d{3,4}(?!\d)/,
	},
	{
		id: "internal-version",
		why: "内部版本代号（v4 是唯一在跑的版本，用户眼里没有「几代」这个概念）",
		re: /\bv4\b/,
	},
	{
		id: "docs-path",
		why: "仓库路径：用户拿到手点不开",
		re: /\bdocs\//,
	},
	{
		id: "build-tool",
		why: "构建工具名：用户跑不了 make/sitegen，这类话属于 README",
		re: /\bsitegen\b/i,
	},
	{
		// #458：`make dev` ×4 曾作为「正常文案」被排除（见头注释），但用户在界面上
		// 没有 checkout、也没有 make。名单取 Makefile 里**本仓特有**的 target（白名单），
		// 所以英文的 "make it" / "make sure" 不会被误伤。
		// 故意不收 `make test` / `make build` / `make up`：容器安装提示里举的就是
		// 「用户自己项目里跑 make test」这种例句（nodes.register.install_intro），
		// 这类词在两个语境里同形，宁可漏也不误伤。
		id: "build-command",
		why: "构建/开发命令：用户手上没有本仓，跑不了 make；说「重启服务」而不是「跑 make dev」",
		re: /\bmake\s+(?:dev|fe|ci|sitegen|e2e|docker|anode|manifest|tailwind|lint-go|contract|integration)\b/i,
	},
	{
		id: "dev-todo",
		why: "TODO / FIXME：开发待办不是产品文案",
		re: /\b(TODO|FIXME)\b/,
	},

	// ── 中文：迁移叙述 / 历史对照 / 对开发者喊话 ──
	{
		id: "zh-migration",
		why: "迁移叙述：讲东西搬到哪去了，而不是这块能做什么",
		re: /已迁|迁到|迁移到|搬到|不在这页|不在本页|本页不再|本页只|页只保留|已移出|移到了/,
	},
	{
		id: "cjk-retired-shape",
		why: "历史形态对照：解释「以前不是这样」，用户没有这段记忆",
		re: /旧 spec|旧版本|旧「|旧形态|旧 agent|旧接口|旧端点|已弃用/,
	},
	{
		id: "zh-dev-audience",
		why: "对维护者/开发者喊话：产品界面不设这个读者",
		re: /开发者说明|写给维护者|维护者|排障者|待实现|mock 数据|调试用/,
	},

	// ── English ──
	{
		id: "en-migration",
		why: "迁移叙述（英）",
		re: /\b(moved to|no longer on this page|not on this page|migrated from)\b/i,
	},
	{
		id: "en-dev-audience",
		why: "对维护者/开发者喊话（英）",
		re: /\b(maintainers?|for developers?|developer notes?)\b/i,
	},
	{
		id: "en-build-pipeline",
		why: "构建管线叙述（英）：构建期发生了什么不该出现在产品里",
		re: /\b(build time|not loaded yet|not implemented yet)\b/i,
	},
	{
		id: "en-retired-shape",
		why: "历史形态对照（英）",
		re: /\blegacy\b/i,
	},

	// ── 日本語 ──
	{
		id: "ja-migration",
		why: "迁移叙述（日）",
		re: /へ移動|移動しました|このページにはありません|ページにありません/,
	},
	{
		id: "ja-retired-shape",
		why: "历史形态对照（日）",
		re: /旧形態|レガシー/,
	},
	{
		id: "ja-dev-audience",
		why: "对开发者喊话（日）",
		re: /開発者向け/,
	},
	{
		id: "ja-build-pipeline",
		why: "构建管线叙述（日）",
		re: /ビルド時/,
	},

	// ── Español ──
	{
		id: "es-migration",
		why: "迁移叙述（西）",
		re: /movidas a|movido a|se movió/,
	},
	{
		id: "es-retired-shape",
		why: "历史形态对照（西）",
		re: /heredad/i,
	},
	{
		id: "es-dev-audience",
		why: "对开发者喊话（西）",
		re: /(?:notas|para) de desarrollo|para desarrollo/i,
	},
	{
		id: "es-build-pipeline",
		why: "构建管线叙述（西）",
		re: /tiempo de compilación/,
	},
];

/**
 * 实现词表（ui-audit 2026-11-19 §3.2）：内部机制词当了按钮名/字段名。
 * 只收「用户据此无法改变操作」的词；协议值本身（GET / resourceVersion 字段）留在
 * `<code>`、`<pre>`、YAML 块或 class 含 dev 的容器里就不算「产品文案」。
 * 大小写敏感：`spec` 只抓小写（避免命中 Spec 缩写以外的普通词）。
 */
export const IMPL_WORDS = [
	{ word: "CAS", re: /(?<![\w-])CAS(?![\w-])/ },
	{ word: "schema", re: /(?<![\w-])schema(?![\w-])/i },
	{ word: "PUT", re: /(?<![\w-])PUT(?![\w-])/ },
	{ word: "GET", re: /(?<![\w-])GET(?![\w-])/ },
	{ word: "resourceVersion", re: /(?<![\w-])resourceVersion(?![\w-])/ },
	{ word: "digest", re: /(?<![\w-])digest(?![\w-])/i },
	{ word: "租约", re: /租约/ },
	{ word: "幂等", re: /幂等/ },
	{ word: "spec", re: /(?<![\w-])spec(?![\w-])/ },
	{ word: "applied", re: /(?<![\w-])applied(?![\w-])/ },
];

/** 代码/协议上下文：命在这里面的实现词是「照抄协议」，不是产品文案。 */
const CODE_TAGS = new Set(["code", "pre", "kbd", "samp"]);
const CODE_CLASSES = new Set(["dev", "devf", "dev-only", "method", "code-body", "doc-code"]);
const LITERAL_CLASSES = new Set(["code-body", "doc-code"]);

/** 找出一段文本里 `<code>…</code>` / `<pre>…</pre>` 的区间（含标签本身）。 */
export function codeRanges(text) {
	const out = [];
	const re = /<(code|pre|kbd|samp)\b[^>]*>[\s\S]*?<\/\1>/gi;
	let m;
	while ((m = re.exec(text))) out.push([m.index, m.index + m[0].length]);
	return out;
}

/**
 * 实现词命中（跳过代码区间）。返回 `{ word }` 或 null。
 * `ranges` 由调用方给：页面文本节点用它所在的祖先链判定（见 textNodeLines），
 * locale 值用值里的 `<code>`/`<pre>` 标记判定。
 */
export function implWordHit(text, ranges = codeRanges(text)) {
	for (const w of IMPL_WORDS) {
		const re = new RegExp(w.re.source, "g");
		let m;
		while ((m = re.exec(text))) {
			if (!ranges.some(([a, b]) => m.index >= a && m.index < b)) return { word: w.word };
		}
	}
	return null;
}

/**
 * 反假绿下限（棘轮）：2026-11-19 在 main 上实测 ——
 * @count node scripts/check-ui-copy.mjs
 * 4 份 locale、非空值 9280；23 个页面、222 个文本节点。
 * 低于它 = 扫描面被改窄了（要么 locale/页面被拆走，要么解析器坏了），报失败。
 * 页面数留了删页的余量（真实删页就同步下调）；文本节点数同理，但不允许归零 ——
 * 「扫出空集却报绿」正是这个检查最怕的假绿形态。
 */
const FLOORS = { files: 4, values: 9000, pages: 20, pageNodes: 160, implNodes: 90, proseNodes: 70 };

export function blankLiterals(text) { // 把字面量区间抹成等长空格：命中下标仍对得上原文，句子里剩下的才算散文
	const ranges = codeRanges(text);
	if (!ranges.length) return text;
	let out = "";
	let at = 0;
	for (const [a, b] of ranges) {
		out += text.slice(at, a) + " ".repeat(b - a);
		at = b;
	}
	return out + text.slice(at);
}

export function isProse(text) { // #470 的句子判据：中文（含日文假名）一个字符即散文；拉丁文要三个词才算句子（端点路径与 {占位} 不是词）
	if (/[\u3040-\u30ff\u3400-\u9fff]/.test(text)) return true;
	const words = text.replace(/[\w{}$.:-]*\/[\w{}$./:-]*/g, " ").match(/[A-Za-z][\w'-]*/g) ?? [];
	return words.length >= 3;
}

export const FILE_ALLOWLIST = [ // #470 Q2：显式文件级白名单（禁止通配），只列「整页就是接口说明」的页面
	{ file: "web-src/pages/docs.html", why: "技术文档页：接入步骤/端点表/参数表就是这一页的正文，方法名与端点属于它要讲的内容" },
];

export const EXEMPT_KEYS = [ // 面板不是文件，所以按**精确 key** 登记（每条带理由，同样禁止通配）
	{ key: "nodes.v4.yaml_title", why: "「原始 YAML（高级）」面板标题：这份文档的型别（apiVersion/kind）就是面板里那几行的抬头，删了用户认不出贴的是什么" },
];

export const PROSE_RULES = [ // #470：三条「散文里讲协议」的窄规则；minHits = 自测红样例里必须命中的条数（正则写坏就立刻红）
	{
		id: "method-endpoint-prose",
		why: "散文里给用户派活：方法名 + 端点只该出现在代码样例或接入文档；界面说「点哪里、会怎样」",
		re: /(?<![\w-])(?:GET|POST|PUT|PATCH|DELETE)(?![\w-])[^\n]{0,24}?\/[A-Za-z0-9{…]/,
		minHits: 4,
	},
	{
		id: "cas-prose",
		why: "CAS / resourceVersion 是协议令牌名：界面说「保存时服务端会校验版本，过期就让你先重载」，409 这类用户真会看到的状态码照留",
		re: /(?<![\w-])(?:CAS|resourceVersion)(?![\w-])/,
		minHits: 4,
	},
	{
		id: "wire-token-prose",
		why: "端点路径与 wire 字段名（spec./metadata./deliveryId…）不进用户说明句：要说的信息用用户视角写出来",
		re: /\/v\d+\/|(?<![\w-])(?:spec|status|metadata|config)\.[a-z][\w.]*|(?<![\w-])(?:deliveryId|payloadSchema|resultSchema|idemKey|installedFrom|targetsResolved|hookUrl|secretSet|appliedRevision|configRevision|apiVersion|overrideStale|event_schema)(?![\w-])/,
		minHits: 5,
	},
];

export function allowlistedFile(file) {
	return FILE_ALLOWLIST.find((a) => a.file === file) ?? null;
}

export function exemptKeyReason(key, keyRefs) { // key 的**全部**引用点都落在白名单文件里 ⇒ 这条语言的用户看不到它（文件级判据，不是 key 名前缀）
	const exact = EXEMPT_KEYS.find((e) => e.key === key);
	if (exact) return exact.why;
	const refs = keyRefs?.get(key);
	if (!refs || refs.size === 0) return null;
	for (const f of refs) if (!allowlistedFile(f)) return null;
	return `引用面全在白名单文件里：${[...refs].join(", ")}`;
}

/**
 * 取一行的注释部分：第一个「不在引号里的 `#`」到行尾。没有注释返回 ""。
 * 用来判定 `# ui-copy-ok:` —— 值里出现同样的字面量不算豁免（必须写在注释里）。
 */
export function commentPart(raw) {
	let quote = null;
	for (let i = 0; i < raw.length; i++) {
		const c = raw[i];
		if (quote) {
			if (quote !== "`" && c === "\\") {
				i += 1;
				continue;
			}
			if (c === quote) quote = null;
			continue;
		}
		if (c === '"' || c === "'") {
			quote = c;
			continue;
		}
		if (c === "#") {
			// YAML 的行尾注释 `#` 前面必须是空白（`a#b` 是值的一部分）。
			if (i === 0 || raw[i - 1] === " " || raw[i - 1] === "\t") return raw.slice(i + 1);
			continue;
		}
	}
	return "";
}

/** 从一段注释里读豁免理由。返回 { marked, reason }。 */
export function readExemption(comment) {
	const idx = comment.indexOf(EXEMPT_MARKER);
	if (idx < 0) return { marked: false, reason: "" };
	const after = comment.slice(idx + EXEMPT_MARKER.length);
	// 只接受 `ui-copy-ok:`（冒号后接理由）；`ui-copy-ok` 后面没有冒号也按「标记了但没理由」处理。
	const m = after.match(/^\s*[:：]\s*(.*)$/);
	return { marked: true, reason: m ? m[1].trim() : "" };
}

/**
 * 纯函数：扫一份 locale 文本，返回 { violations, values }。
 * @param {string} text
 * @param {string} file 报错用的文件名
 */
export function scanLocale(text, file = "<memory>", opts = {}) {
	const { entries } = parseLocale(text, file);
	const lines = text.split("\n");
	/** 每条违规：{ file, line, key, ruleId, why, hit, value, exempted, reason } */
	const violations = [];
	let values = 0;
	const fileWhy = allowlistedFile(file)?.why ?? null;
	const exempt = (key, comment) => {
		if (fileWhy) return { marked: true, reason: fileWhy };
		const keyWhy = exemptKeyReason(key, opts.keyRefs);
		return comment.marked ? comment : keyWhy ? { marked: true, reason: keyWhy } : comment;
	};

	for (const [key, { line, value }] of entries) {
		if (!value.trim()) continue;
		values += 1;
		const impl = implWordHit(value);
		const prose = blankLiterals(value); // #470：散文规则只看字面量外面剩下的那句
		const isSentence = isProse(prose);
		for (const rule of RULES) {
			const m = value.match(rule.re);
			if (!m) continue;
			// 豁免：值所在行、或上一行的注释里写了 `# ui-copy-ok: <理由>`。
			const own = readExemption(commentPart(lines[line - 1] ?? ""));
			const prev = line >= 2 ? readExemption(commentPart(lines[line - 2] ?? "")) : { marked: false, reason: "" };
			const ex = own.marked ? own : prev;
			violations.push({
				file,
				line,
				key,
				ruleId: rule.id,
				why: rule.why,
				hit: m[0],
				value,
				exempted: ex.marked && ex.reason !== "",
				reason: ex.reason,
				// 「标记了但没写理由」要单独报：它不是豁免，是格式错。
				blankReason: ex.marked && ex.reason === "",
			});
		}
		for (const rule of PROSE_RULES) {
			if (!isSentence) continue;
			const m = prose.match(rule.re);
			if (!m) continue;
			const own = readExemption(commentPart(lines[line - 1] ?? ""));
			const prev = line >= 2 ? readExemption(commentPart(lines[line - 2] ?? "")) : { marked: false, reason: "" };
			const ex = exempt(key, own.marked ? own : prev);
			violations.push({
				file,
				line,
				key,
				ruleId: rule.id,
				why: rule.why,
				hit: m[0],
				value,
				exempted: ex.marked && ex.reason !== "",
				reason: ex.reason,
				blankReason: ex.marked && ex.reason === "",
			});
		}
		if (impl) {
			const own = readExemption(commentPart(lines[line - 1] ?? ""));
			const prev = line >= 2 ? readExemption(commentPart(lines[line - 2] ?? "")) : { marked: false, reason: "" };
			const ex = own.marked ? own : prev;
			violations.push({
				file,
				line,
				key,
				ruleId: "impl-word",
				why: "实现词（审计 §3.2）：用户界面说「能做什么」，不说协议字段名",
				hit: impl.word,
				value,
				exempted: ex.marked && ex.reason !== "",
				reason: ex.reason,
				blankReason: ex.marked && ex.reason === "",
			});
		}
	}
	return { violations, values, exemptKeys: EXEMPT_KEYS.filter((e) => entries.has(e.key)).map((e) => e.key) };
}

/* ---------- 页面模板（#458）：只取「会渲染给用户看」的文本节点 ---------- */

/**
 * Go 模板里需要配一个 `{{end}}` 的 action。
 * `template` 不进栈（自闭合），`else` 也不进栈（不新增嵌套）。
 */
const OPEN_ACTIONS = new Set(["if", "range", "with", "define", "block"]);

/**
 * 找出 `{{define "X"}}…{{end}}` 的**体区间**（体内的 if/range 也按 end 配对）。
 * 返回 [{ name, start, end }]；start/end 是字符下标，不含两端的 action。
 */
export function defineRanges(tpl) {
	const ranges = [];
	const stack = [];
	const re = /\{\{-?\s*([a-zA-Z_][a-zA-Z0-9_]*)?/g;
	let m;
	while ((m = re.exec(tpl))) {
		const word = m[1] ?? "";
		if (OPEN_ACTIONS.has(word)) {
			const close = tpl.indexOf("}}", m.index + 2);
			const after = close < 0 ? tpl.length : close + 2;
			let name = null;
			if (word === "define" || word === "block") {
				const rest = tpl.slice(m.index + m[0].length, close < 0 ? tpl.length : close);
				const nm = /^\s+"([^"]+)"/.exec(rest);
				name = nm ? nm[1] : null;
			}
			stack.push({ name, start: after });
			continue;
		}
		if (word !== "end") continue;
		const top = stack.pop();
		if (top && top.name) ranges.push({ name: top.name, start: top.start, end: m.index });
	}
	return ranges;
}

/**
 * layout 把哪些 define 接进了渲染树：`{{block "X"}}` / `{{template "X"}}` 的名字集合。
 * sitegen 只执行 `__layout__`（internal/sitegen/template.go 的 execute），所以不在
 * 这个集合里的 define 体永远进不了产物 —— 比如 `docs.html` 里那个没人引用的
 * `{{define "fonts-extra"}}`：它的体是一段 URL 碎片，不是用户看得见的文字。
 * 扫它只会把「给未来留的片段」当成文案。
 */
export function renderedDefineNames(texts) {
	const names = new Set();
	for (const t of texts) for (const m of t.matchAll(/\{\{-?\s*(?:block|template)\s+"([^"]+)"/g)) names.add(m[1]);
	return names;
}

/** 页面侧豁免：同一行或上一行的 HTML 注释里写 `<!-- ui-copy-ok: <理由> -->`。 */
export function pageExemption(rawLine) {
	for (const c of rawLine.match(/<!--[\s\S]*?-->/g) ?? []) {
		const r = readExemption(c.slice(4, -3));
		if (r.marked) return r;
	}
	return { marked: false, reason: "" };
}

/** 数一段区间里的换行数（遮蔽区也要把行号抬到位，否则报错全落在第一行）。 */
function countNl(text, from, to) {
	let n = 0;
	for (let i = from; i < to; i++) if (text[i] === "\n") n += 1;
	return n;
}

/**
 * 展平状态机：从（已按 define 遮罩过的）模板文本里取文本节点。
 * 丢掉：标签（属性名/属性值/类名）、HTML 注释、<script>/<style> 内容、{{…}} 表达式。
 * 保留一个祖先栈，给每个节点标 `dev`：`pre/code/kbd/samp`、class 含 dev/devf/method…
 * 或带 `data-dev` 的容器里的文字是「照拄协议」，实现词表不扫它。
 * 返回 [{ line, text, dev }]；text 已折叠空白，line 是 1-based 原文件行号。
 */
const VOID_TAGS = new Set(["area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "param", "source", "track", "wbr"]);

function inCodeContext(stack) {
	return stack.some(
		(e) => CODE_TAGS.has(e.tag) || e.devAttr || e.classes.some((c) => CODE_CLASSES.has(c)),
	);
}

function inLiteralContext(stack) { // #470：契约的豁免面是 pre/code 里的示例本身，不含「开发者信息」折叠里的标签文字
	return stack.some((e) => CODE_TAGS.has(e.tag) || e.classes.some((c) => LITERAL_CLASSES.has(c)));
}

function parseTag(raw) {
	const m = /^\/?\s*([a-zA-Z][a-zA-Z0-9-]*)/.exec(raw);
	if (!m) return null;
	const closing = /^\//.test(raw);
	const selfClose = /\/\s*$/.test(raw);
	const tag = m[1].toLowerCase();
	const cls = /(?:^|\s)class\s*=\s*("([^"]*)"|'([^']*)')/i.exec(raw);
	const classes = cls ? (cls[2] ?? cls[3] ?? "").split(/\s+/).filter(Boolean) : [];
	return { tag, closing, selfClose, classes, devAttr: /(?:^|\s)data-dev(?:\s|=|$)/i.test(raw) };
}

function textNodeLines(masked) {
	const out = [];
	let buf = "";
	let bufLine = 1;
	let bufDev = false;
	let bufLiteral = false;
	let line = 1;
	const stack = [];
	const flush = () => {
		const v = buf.replace(/\s+/g, " ").trim();
		if (v) out.push({ line: bufLine, text: v, dev: bufDev, literal: bufLiteral });
		buf = "";
	};
	const n = masked.length;
	let i = 0;
	while (i < n) {
		const c = masked[i];
		if (c === "\n") {
			flush();
			line += 1;
			i += 1;
			continue;
		}
		if (c === "<") {
			flush();
			if (masked.startsWith("<!--", i)) {
				const end = masked.indexOf("-->", i + 4);
				const stop = end < 0 ? n : end + 3;
				line += countNl(masked, i, stop);
				i = stop;
				continue;
			}
			const raw = /^<(script|style)\b/i.exec(masked.slice(i, i + 12));
			if (raw) {
				const closeAt = masked.toLowerCase().indexOf(`</${raw[1].toLowerCase()}`, i + 1);
				const stop = closeAt < 0 ? n : closeAt;
				line += countNl(masked, i, stop);
				i = stop;
				continue;
			}
			// 普通标签：跳到 `>`；引号里的 `>` 不算结束（如 d="M4 17v2a1 1 0 0 0 1 1h14"）
			let j = i + 1;
			let quote = null;
			while (j < n) {
				const d = masked[j];
				if (d === "\n") line += 1;
				if (quote) {
					if (d === quote) quote = null;
				} else if (d === '"' || d === "'") {
					quote = d;
				} else if (d === ">") {
					j += 1;
					break;
				}
				j += 1;
			}
			const info = parseTag(masked.slice(i + 1, Math.max(i + 1, j - 1)));
			if (info) {
				if (info.closing) {
					for (let k = stack.length - 1; k >= 0; k--) {
						if (stack[k].tag === info.tag) {
							stack.length = k;
							break;
						}
					}
				} else if (!info.selfClose && !VOID_TAGS.has(info.tag)) {
					stack.push(info);
				}
			}
			i = j;
			continue;
		}
		if (c === "{" && masked[i + 1] === "{") {
			flush();
			const end = masked.indexOf("}}", i + 2);
			const stop = end < 0 ? n : end + 2;
			line += countNl(masked, i, stop);
			i = stop;
			continue;
		}
		if (!buf) {
			bufLine = line;
			bufDev = inCodeContext(stack);
			bufLiteral = inLiteralContext(stack);
		}
		buf += c;
		i += 1;
	}
	flush();
	return out;
}

/**
 * 纯函数：扫一个页面模板，返回 { violations, nodes, defines }。
 * `opts.renderedDefines` 是 layout 接进渲染树的 define 名集合（见 renderedDefineNames）；
 * 不传则不按 define 过滤（自测用），传空 Set 则一条文字都不扫（下限会报失败，这是故意的）。
 */
export function scanPage(text, file = "<memory>", opts = {}) {
	const lines = text.split("\n");
	const all = defineRanges(text);
	let masked = text;
	let defines = all.length;
	if (opts.renderedDefines) {
		const keep = new Uint8Array(text.length);
		defines = 0;
		for (const r of all) {
			if (!opts.renderedDefines.has(r.name)) continue;
			keep.fill(1, r.start, r.end);
			defines += 1;
		}
		const buf = new Array(text.length);
		for (let i = 0; i < text.length; i++) buf[i] = keep[i] || text[i] === "\n" ? text[i] : " ";
		masked = buf.join("");
	}

	const nodes = textNodeLines(masked);
	const violations = [];
	let implNodes = 0;
	let proseNodes = 0;
	const fileWhy = allowlistedFile(file)?.why ?? null;
	for (const node of nodes) {
		const checked = !node.dev;
		if (checked) implNodes += 1;
		const prose = !node.literal && !fileWhy;
		if (prose) proseNodes += 1;
		const push = (ruleId, why, hit) => {
			const own = pageExemption(lines[node.line - 1] ?? "");
			const prev = node.line >= 2 ? pageExemption(lines[node.line - 2] ?? "") : { marked: false, reason: "" };
			const ex = own.marked ? own : prev;
			violations.push({
				file,
				line: node.line,
				key: null,
				ruleId,
				why,
				hit,
				value: node.text,
				exempted: ex.marked && ex.reason !== "",
				reason: ex.reason,
				blankReason: ex.marked && ex.reason === "",
			});
		};
		for (const rule of RULES) {
			const m = node.text.match(rule.re);
			if (!m) continue;
			push(rule.id, rule.why, m[0]);
		}
		if (checked) {
			const impl = implWordHit(node.text, []);
			if (impl) push("impl-word", "实现词（审计 §3.2）：用户界面说「能做什么」，不说协议字段名", impl.word);
		}
		if (!prose) continue;
		for (const rule of PROSE_RULES) {
			const m = node.text.match(rule.re);
			if (m && isProse(node.text)) push(rule.id, rule.why, m[0]);
		}
	}
	return { violations, nodes: nodes.length, implNodes, proseNodes, defines };
}

/** 自测用：红样例必须真红（且命中指定规则），绿样例必须完全无违规。 */
const SELF_TEST = {
	red: [
		// #447 的原文（zh）：迁移叙述
		{ text: `nodes:\n  moved_note: "命名空间 vars 是命名空间级配置（已迁到命名空间页）；节点页只保留授权列表。"\n`, rule: "zh-migration" },
		{ text: `nodes:\n  moved_title: "命名空间与系统配置不在这页"\n`, rule: "zh-migration" },
		{ text: `nodes:\n  moved_title: "Namespace and system configuration are not on this page"\n`, rule: "en-migration" },
		{ text: `nodes:\n  moved_title: "ネームスペース設定はこのページにありません"\n`, rule: "ja-migration" },
		{ text: `nodes:\n  moved_note: "Las vars son configuración del espacio (movidas a su página)."\n`, rule: "es-migration" },
		// 对照内部实现
		{ text: `pb:\n  hint: "槽位判定（#436）按物理槽位算"\n`, rule: "issue-ref" },
		{ text: `docs:\n  note: "端点来自五份 v4 spec"\n`, rule: "internal-version" },
		{ text: `docs:\n  note: "细节见 docs/contracts/CONTRACT.md"\n`, rule: "docs-path" },
		{ text: `docs:\n  empty: "重新运行 make sitegen 即可生成这一节"\n`, rule: "build-tool" },
		{ text: `docs:\n  note: "Re-run make sitegen to generate this section"\n`, rule: "build-tool" },
		// #458：构建命令（白名单式，见 RULES 里的 build-command）
		{ text: `recv:\n  no_backend: "请确认服务端已启动（本地可运行 make dev）后重试。"\n`, rule: "build-command" },
		{ text: `recv:\n  no_backend: "Make sure the server is running (run make dev locally), then retry."\n`, rule: "build-command" },
		{ text: `docs:\n  hint: "Run make fe after touching web-src"\n`, rule: "build-command" },
		// 对开发者喊话
		{ text: `ux:\n  summary: "开发者说明：槽位判定与批量写规则"\n`, rule: "zh-dev-audience" },
		{ text: `docs:\n  lede: "The notes below are for maintainers and anyone debugging."\n`, rule: "en-dev-audience" },
		{ text: `sources:\n  edges: "Legacy declaration"\n`, rule: "en-retired-shape" },
		{ text: `caps:\n  stale: "旧形态声明"\n`, rule: "cjk-retired-shape" },
		{ text: `docs:\n  x: "ビルド時に生成されます"\n`, rule: "ja-build-pipeline" },
		{ text: `docs:\n  x: "Se genera en tiempo de compilación"\n`, rule: "es-build-pipeline" },
		// 实现词（审计 §3.2）：协议词当了产品文案
		{ text: `nodes:\n  save: "以当前 CAS 保存"\n`, rule: "impl-word" },
		{ text: `caps:\n  rv: "当前 resourceVersion：{rv}"\n`, rule: "impl-word" },
		{ text: `ns:\n  hint: "只影响该命名空间的下一次解析；不写进节点 applied。"\n`, rule: "impl-word" },
		{ text: `pb:\n  x: "省略（使用 schema 默认值）"\n`, rule: "impl-word" },
		{ text: `caps:\n  x: "当前 digest"\n`, rule: "impl-word" },
		{ text: `m:\n  x: "租约过期"\n`, rule: "impl-word" },
		{ text: `docs:\n  x: "用 PUT 整体替换模板"\n`, rule: "impl-word" },
		// TODO
		{ text: `a:\n  b: "TODO: 换成真数据"\n`, rule: "dev-todo" },
		{ text: `events:\n  empty: "还没有事件。用 POST /v1/namespaces/{namespace}/events 提交第一个。"\n`, rule: "method-endpoint-prose" },  // #470 R1：散文里的「方法名 + 端点」
		{ text: `pb:\n  intro: "安装是单独的 POST …/install，部署是能力的 POST …/deploy。"\n`, rule: "method-endpoint-prose" },
		{ text: `pb:\n  hint: "以 PUT /v1/playbooks/{name} 整体替换模板"\n`, rule: "method-endpoint-prose" },
		{ text: `events:\n  empty: "No events yet. Submit one with POST /v1/namespaces/{namespace}/events."\n`, rule: "method-endpoint-prose" },
		{ text: `nodes:\n  save: "以当前 CAS 保存"\n`, rule: "cas-prose" },  // #470 R2：CAS / resourceVersion 出现在句子里（409 这类用户真会看到的状态码不在词表里）
		{ text: `nodes:\n  stale: "409：CAS 已过期，先重载再保存。"\n`, rule: "cas-prose" },
		{ text: `sources:\n  rv: "当前 resourceVersion 是 {rv}"\n`, rule: "cas-prose" },
		{ text: `caps:\n  hint: "Saving uses CAS; a stale token comes back as 409."\n`, rule: "cas-prose" },
		{ text: `sources:\n  name: "资源名 metadata.name"\n`, rule: "wire-token-prose" },  // #470 R3：端点路径与 wire 字段名混进用户说明句
		{ text: `sources:\n  pending: "保存后生成（形如 /v1/hooks/src_xxx）"\n`, rule: "wire-token-prose" },
		{ text: `sources:\n  dedup: "deliveryId 默认去重 30 天"\n`, rule: "wire-token-prose" },
		{ text: `sources:\n  hint: "每次投递先检查 payloadSchema；不合规事件仍入库"\n`, rule: "wire-token-prose" },
		{ text: `events:\n  uid: "Event UID (not deliveryId)"\n`, rule: "wire-token-prose" },
	],
	green: [
		// 恢复码格式提示：`XXXX-XXXX` 不是 TODO / 不是占位数据
		`login:\n  code_placeholder: "XXXX-XXXX-XXXX-XXXX"\n`,
		// HTML 实体 `&#123;`：`#` 前面是 `&`，不是工单号
		`docs:\n  step: '<code>pushManager.subscribe(&#123; applicationServerKey &#125;)</code>'\n`,
		// 「见下方」有真实锚点（Key 的安全实践），不是悬空指代
		`keys:\n  desc_tip: "Key 仅创建时展示一次（见下方「Key 的安全实践」）。"\n`,
		// `commit` 是安全建议里的动词，不是「对照提交号」
		`keys:\n  point4: "Never commit Keys to version control; inject via env vars"\n`,
		// `noop 占位` 是 playbook 里真实存在的实现种类标签
		`pb:\n  noop: "noop 占位"\n`,
		`pb:\n  no_impl: "目标是占位实现（noop），中枢不会执行它"\n`,
		// `更旧` 是分页方向的正常文案
		`events:\n  older: "更旧 →"\n`,
		// 英文的 make it / make sure 是动词，不在 target 白名单里（#458 的反向证明）。
		// 注：这两个词不要写成行内反引号 —— check-annotations 会把反引号里的 make 子句当引用去解析。
		`ns:\n  yaml_hint: "Paste it back to save; the page will not make it public."\n`,
		`recv:\n  no_backend: "Make sure the server is running, then retry."\n`,
		`recv:\n  hint: "Make the node visible to this namespace first."\n`,
		// `移行` 是数据流状态迁移（进入实时流），不是配置搬家
		`docs:\n  reconnect: "その後リアルタイムストリームに移行します。"\n`,
		// 历史「状态」标签（旧指纹 = 前一代内容指纹）不是历史「过程」叙述
		`caps:\n  stale: "旧指纹"\n`,
		// 实现词只出现在 <code>/<pre> 里 = 照抄协议，不是产品文案
		`caps:\n  hint: "字段名 <code>spec.concurrency.maxPerNode</code>，默认 1。"\n`,
		`docs:\n  sample: "<pre>GET /v1/nodes\nresourceVersion: 3</pre>"\n`,
		`nodes:\n  save: "保存"\n`,
		// 注解里的 `#` 与值里的普通数字
		`docs:\n  retain: "默认 604800 秒，最大 2592000 秒"\n`,
		`docs:\n  toc_events: "POST /v1/namespaces/{namespace}/events"\n`,  // #470 绿侧：纯端点值（表格单元格/TOC）不是散文，两个 token 而已
		`nodes:\n  dev_delivery: "deliveryId"\n`,  // #470 绿侧：开发者信息折叠里的**标签**就是字段名本身，没有句子
		`sources:\n  hint: "字段 <code>spec.config.token</code> 只写不读"\n`,  // #470 绿侧：协议词在 <code> 里 = 照抄协议
		`nodes:\n  stale_ok: "409：你手里的版本旧了，先重载再保存。"\n`,  // #470 绿侧：Q1 的判据 —— 409 + 「先重载再存」是可执行信息，保留且不触发任何规则
	],
	// 豁免：上一行带理由 → 绿；行内带理由 → 绿；标记了但没理由 → 仍然红。
	exempt: [
		{ text: `nodes:\n  # ui-copy-ok: 停用公告，保留工单号方便用户报障\n  x: "已迁到新页（#447）"\n`, ok: true },
		{ text: `nodes:\n  x: "已迁到新页（#447）"  # ui-copy-ok: 过渡期公告\n`, ok: true },
		{ text: `nodes:\n  # ui-copy-ok:\n  x: "已迁到新页（#447）"\n`, ok: false },
		{ text: `nodes:\n  # ui-copy-ok\n  x: "已迁到新页（#447）"\n`, ok: false },
		// 豁免写在**值里**（不是注释里）不算数
		{ text: `nodes:\n  x: "已迁到新页（#447） # ui-copy-ok: 假装是注释"\n`, ok: false },
	],
	// 页面模板（#458）：只扫「会渲染给用户看的文本节点」。red 带 line，用来证明遮蔽没把行号搞丢。
	page: {
		red: [
			// 散文直接写在页面里（#448 修之前 docs 页就是这样）
			{ html: `<div>\n  <p>下面这些是写给维护者与排障者的说明。</p>\n</div>\n`, rule: "zh-dev-audience", line: 2 },
			{ html: `<div>\n  <p>命名空间与系统配置不在这页</p>\n</div>\n`, rule: "zh-migration", line: 2 },
			{ html: `<div>\n  <p>本地可运行 make dev 起服务</p>\n</div>\n`, rule: "build-command", line: 2 },
			// 行号：前面隔了 comment 与 script，仍要报在真实行号上（不是第一行）
			{ html: `<div>\n  <!-- 注释吃掉三行\n       第二行\n       第三行 -->\n  <script>const a = 1;</script>\n  <p>Re-run make sitegen to generate it</p>\n</div>\n`, rule: "build-tool", line: 6 },
			// <pre> 里的代码样例也是可见文字：塞一句旁白就判红
			{ html: `<pre>\nGET /v1/nodes\n已迁到命名空间页\n</pre>\n`, rule: "zh-migration", line: 3 },
			// 同一个 define：被 layout 接进渲染树就被扫（证明过滤是「选」，不是「一律不扫」）
			{ html: `{{define "content"}}<p>写给维护者</p>{{end}}\n`, rule: "zh-dev-audience", line: 1, renderedDefines: ["content"] },
			// 实现词写进可见正文（不是代码块、不是 dev 容器）
			{ html: `<div>\n  <p>以当前 CAS 保存</p>\n</div>\n`, rule: "impl-word", line: 2 },
			{ html: `<div>\n  <p>保存时用 PUT /v1/nodes/{name}/yaml 提交整份文档</p>\n</div>\n`, rule: "method-endpoint-prose", line: 2 },  // #470：同一类叙述落在页面 <p> 里照样红（散文规则与 impl-word 是同一条线上的两段）
			{ html: `<div>\n  <p>两个页面并发写时靠 CAS 判定，版本旧了就报 409</p>\n</div>\n`, rule: "cas-prose", line: 2 },
			{ html: `<div>\n  <p>目标解析结果放在 metadata.annotations 里</p>\n</div>\n`, rule: "wire-token-prose", line: 2 },
			{ html: `<div>\n  <p>当前 resourceVersion 已过期</p>\n</div>\n`, rule: "impl-word", line: 2 },
		],
		green: [
			// 同一句放 HTML 注释里 → 不扫（写给维护者的注释合法）；`nodes` 是预期抽到的文本节点数，
			// 用它钉住「确实扫了 / 确实什么也没渲染」，而不是只看「没违规」——后者与没检查同形。
			{ html: `<div>\n  <!-- 下面这些是写给维护者与排障者的说明（已从 <p> 里删掉） -->\n  <p>{{t "docs.design.lede"}}</p>\n</div>\n`, nodes: 0 },
			// <script> / <style> 内容不是文案
			{ html: `<div>\n  <script>const hint = "本地可运行 make dev";</script>\n  <style>.a::after { content: "写给维护者"; }</style>\n</div>\n`, nodes: 0 },
			// 属性名 / 属性值 / 类名都不是文本节点
			{ html: `<div class="make-dev-note" title="写给维护者" data-hint="已迁到新页">\n  <span>{{.Lang}}</span>\n</div>\n`, nodes: 0 },
			// 模板表达式（含 if/range 的条件）不渲染成文字
			{ html: `{{if .OpenAPI}}<div>{{range .OpenAPI}}{{.Tag}}{{end}}</div>{{end}}\n`, nodes: 0 },
			// 没进渲染树的 define 体（sitegen 只执行 layout）不扫：命中词全在 fonts-extra 里也绿地；
			// content 被引用 → 只抽到它的一个节点
			{ html: `{{define "fonts-extra"}}写给维护者 · docs/ · 已迁到{{end}}\n{{define "content"}}<p>ok</p>{{end}}\n`, renderedDefines: ["content"], nodes: 1 },
			// 正常文案：路径型代码样例（真实页面里的）不误报
			{ html: `<pre>GET /v1/nodes/{name}/config\ncurl -fsSL https://&lt;hub&gt;/install-anode.sh | sh</pre>\n`, nodes: 2 },
			// 正常散文（真实页面上就有这种）：必须真抽到
			{ html: `<div>\n  <p>下面这几条是产品行为里最容易误读的地方，出问题时按它们对账。</p>\n</div>\n`, nodes: 1 },
			{ html: `<pre>\n保存时用 PUT /v1/nodes/{name}/yaml 提交整份文档\n</pre>\n`, nodes: 1 },  // #470 绿侧：同一句话放进 <pre> 就是示例本身（M4 的自测版：不是靠全员变红过关）
			// 实现词（审计 §3.2）的结构化例外：<pre> / <code> / class 含 dev 的容器 / data-dev / HTML 注释
			{ html: `<pre>PUT /v1/nodes/{name}/config\nresourceVersion: 3</pre>\n`, nodes: 2 },
			{ html: `<p>字段名 <code>spec.vars</code> 由服务端管。</p>\n`, nodes: 3 },
			{ html: `<details class="devf"><summary>开发者信息</summary><p>schema 与 applied</p></details>\n`, nodes: 2 },
			{ html: `<div data-dev><span>digest</span></div>\n`, nodes: 1 },
			{ html: `<p><span class="method m-get">GET</span> /v1/nodes</p>\n`, nodes: 2 },
			{ html: `<div>\n  <!-- CAS 只在注释里，写给维护者 -->\n  <p>保存</p>\n</div>\n`, nodes: 1 },
		],
		// 页面侧豁免：`<!-- ui-copy-ok: 理由 -->` 同行/上一行→绿；空理由/写在文字里→红。
		exempt: [
			{ html: `<div>\n  <!-- ui-copy-ok: 过渡期公告，保留工单号方便报障 -->\n  <p>已迁到新页（#447）</p>\n</div>\n`, ok: true },
			{ html: `<div>\n  <p>已迁到新页</p> <!-- ui-copy-ok: 过渡期公告 -->\n</div>\n`, ok: true },
			{ html: `<div>\n  <!-- ui-copy-ok: -->\n  <p>已迁到新页</p>\n</div>\n`, ok: false },
			{ html: `<div>\n  <p>已迁到新页 # ui-copy-ok: 假装是注释</p>\n</div>\n`, ok: false },
		],
	},
};

/** 跑自测。返回 true = 全绿。 */
export function selfTest(log = console.log) {
	let failures = 0;
	const expect = (cond, msg) => {
		if (cond) return true;
		failures += 1;
		log(`  ✗ ${msg}`);
		return false;
	};

	log("check-ui-copy --self-test");
	log(`  规则 ${RULES.length} 条；红样例 ${SELF_TEST.red.length} / 绿样例 ${SELF_TEST.green.length} / 豁免 ${SELF_TEST.exempt.length}`);
	log(`  页面样例：红 ${SELF_TEST.page.red.length} / 绿 ${SELF_TEST.page.green.length} / 豁免 ${SELF_TEST.page.exempt.length}`);
	expect(SELF_TEST.red.length >= 6, `红样例必须 ≥6（当前 ${SELF_TEST.red.length}）`);
	expect(SELF_TEST.green.length >= 4, `绿样例必须 ≥4（当前 ${SELF_TEST.green.length}）`);
	expect(SELF_TEST.page.red.length >= 3, `页面红样例必须 ≥3（当前 ${SELF_TEST.page.red.length}）`);
	expect(SELF_TEST.page.green.length >= 4, `页面绿样例必须 ≥4（当前 ${SELF_TEST.page.green.length}）`);

	for (const [i, c] of SELF_TEST.red.entries()) {
		let got;
		try {
			got = scanLocale(c.text, `red-${i + 1}.yaml`);
		} catch (e) {
			expect(false, `red-${i + 1} 解析失败：${e?.message ?? e}`);
			continue;
		}
		const active = got.violations.filter((v) => !v.exempted);
		if (!expect(active.length > 0, `red-${i + 1} 应该红，却绿了：${c.text.trim()}`)) continue;
		expect(
			active.some((v) => v.ruleId === c.rule),
			`red-${i + 1} 红的规则应是 ${c.rule}，实际 ${active.map((v) => v.ruleId).join(",")}`,
		);
	}

	for (const [i, text] of SELF_TEST.green.entries()) {
		let got;
		try {
			got = scanLocale(text, `green-${i + 1}.yaml`);
		} catch (e) {
			expect(false, `green-${i + 1} 解析失败：${e?.message ?? e}`);
			continue;
		}
		const active = got.violations.filter((v) => !v.exempted);
		expect(active.length === 0, `green-${i + 1} 应该绿，却红了：${active.map((v) => `${v.ruleId}«${v.hit}»`).join(", ")} ← ${text.trim()}`);
	}

	for (const [i, c] of SELF_TEST.exempt.entries()) {
		let got;
		try {
			got = scanLocale(c.text, `exempt-${i + 1}.yaml`);
		} catch (e) {
			expect(false, `exempt-${i + 1} 解析失败：${e?.message ?? e}`);
			continue;
		}
		const active = got.violations.filter((v) => !v.exempted);
		const pass = c.ok ? active.length === 0 : active.length > 0;
		expect(pass, `exempt-${i + 1} 期望 ${c.ok ? "绿" : "红"}，实际 ${active.length === 0 ? "绿" : `红 ${active.map((v) => v.ruleId).join(",")}`}：${c.text.trim()}`);
	}

	// 反向证明：把命中词从红样例里删掉，必须变绿（防「这条规则永远红」的假阳性实现）。
	const stripped = scanLocale(`nodes:\n  moved_title: "Namespace and system configuration"\n`, "strip.yaml");
	expect(stripped.violations.length === 0, "删掉命中词后应转绿（否则规则在乱扫）");

	// 页面侧：红样例必须真红 + 报对行号（行号错了 = 遮蔽把行搞丢，审阅者找不到原文）。
	for (const [i, c] of SELF_TEST.page.red.entries()) {
		const got = scanPage(c.html, `page-red-${i + 1}.html`, { renderedDefines: c.renderedDefines ? new Set(c.renderedDefines) : undefined });
		const active = got.violations.filter((v) => !v.exempted);
		if (!expect(active.length > 0, `page-red-${i + 1} 应该红，却绿了：${c.html.trim()}`)) continue;
		expect(
			active.some((v) => v.ruleId === c.rule),
			`page-red-${i + 1} 红的规则应是 ${c.rule}，实际 ${active.map((v) => v.ruleId).join(",")}`,
		);
		if (c.line) {
			expect(
				active.some((v) => v.line === c.line),
				`page-red-${i + 1} 应报在第 ${c.line} 行，实际 ${active.map((v) => v.line).join(",")}`,
			);
		}
	}

	// 页面侧绿样例还要钉住抽到的节点数：扫出空集时「绿」与「没检查」同形，这正是本检查最怕的假绿。
	for (const [i, c] of SELF_TEST.page.green.entries()) {
		const got = scanPage(c.html, `page-green-${i + 1}.html`, { renderedDefines: c.renderedDefines ? new Set(c.renderedDefines) : undefined });
		const active = got.violations.filter((v) => !v.exempted);
		expect(active.length === 0, `page-green-${i + 1} 应该绿，却红了：${active.map((v) => `${v.ruleId}«${v.hit}»@${v.line}`).join(", ")} ← ${c.html.trim()}`);
		expect(got.nodes === c.nodes, `page-green-${i + 1} 应抽到 ${c.nodes} 个文本节点，实际 ${got.nodes}`);
	}

	for (const [i, c] of SELF_TEST.page.exempt.entries()) {
		const got = scanPage(c.html, `page-exempt-${i + 1}.html`);
		const active = got.violations.filter((v) => !v.exempted);
		const pass = c.ok ? active.length === 0 : active.length > 0;
		expect(pass, `page-exempt-${i + 1} 期望 ${c.ok ? "绿" : "红"}，实际 ${active.length === 0 ? "绿" : `红 ${active.map((v) => v.ruleId).join(",")}`}：${c.html.trim()}`);
	}

	const proseHits = new Map(PROSE_RULES.map((r) => [r.id, 0]));  // #470：每条散文规则的自测命中数就是它的 minHits 下限 —— 正则写坏成永不命中，这里立刻红。
	for (const c of SELF_TEST.red) for (const v of scanLocale(c.text, "cover.yaml").violations) if (proseHits.has(v.ruleId) && !v.exempted) proseHits.set(v.ruleId, proseHits.get(v.ruleId) + 1);
	for (const rule of PROSE_RULES) expect(proseHits.get(rule.id) >= rule.minHits, `散文规则 ${rule.id} 只被 ${proseHits.get(rule.id)} 条样例命中（minHits ${rule.minHits}）：正则写坏了？`);
	log(`  #470 散文规则命中数（下限 minHits）：${PROSE_RULES.map((r) => `${r.id}=${proseHits.get(r.id)}/${r.minHits}`).join(" · ")}`);
	for (const a of FILE_ALLOWLIST) expect(!/[*?]/.test(a.file) && a.why.length > 10, `文件白名单 ${a.file} 不许带通配符，且必须写理由`);
	for (const e of EXEMPT_KEYS) expect(!/[*]/.test(e.key) && e.why.length > 10, `EXEMPT_KEYS 的 ${e.key} 不许带通配符，且必须写理由`);

	const docLine = `<p>保存后会生成形如 /v1/hooks/src_xxx 的地址</p>\n`;  // #470 豁免的判据是**文件**，不是「这句话看着像文档」：同一句话在 docs 页绿、在别的页面红。
	expect(scanPage(docLine, "web-src/pages/docs.html").violations.filter((v) => !v.exempted).length === 0, "白名单文件（技术文档页）里的端点句必须绿");
	expect(scanPage(docLine, "web-src/pages/nodes.html").violations.some((v) => v.ruleId === "wire-token-prose"), "非白名单页面里的同一句话必须红");
	const docsOnly = new Map([["docs.k", new Set(["web-src/pages/docs.html"])]]);
	const yamlKey = `docs:\n  k: "保存后会生成形如 /v1/hooks/src_xxx 的地址"\n`;
	expect(scanLocale(yamlKey, "l.yaml", { keyRefs: docsOnly }).violations.filter((v) => !v.exempted).length === 0, "key 只被白名单页面引用 ⇒ 豁免");
	const mixed = new Map([["docs.k", new Set(["web-src/pages/docs.html", "web-src/pages/nodes.html"])]]);
	expect(scanLocale(yamlKey, "l.yaml", { keyRefs: mixed }).violations.some((v) => v.ruleId === "wire-token-prose"), "只要有一个引用点不在白名单就要红（豁免不许漏出文档页）");
	expect(scanLocale(yamlKey, "l.yaml", { keyRefs: new Map() }).violations.filter((v) => !v.exempted).length > 0, "没人引用的 key 不豁免（死文案也得干净，否则删了才敢信）");
	const segs = EXEMPT_KEYS[0].key.split(".");  // 面板 key 是嵌套的（`nodes.v4.yaml_title` 这类）：按登记表的 key 现场拼出嵌套 YAML，顺带钉住扁平名与嵌套名是同一个 key
	const panel = `${segs.map((seg, i) => `${"  ".repeat(i)}${seg}:${i === segs.length - 1 ? ' "节点 YAML（apiVersion anotify/v1 · kind Node）"' : ""}`).join("\n")}\n`;
	expect(scanLocale(panel, "l.yaml", {}).violations.filter((v) => !v.exempted).length === 0, "「原始 YAML（高级）」面板的型别抬头按 EXEMPT_KEYS 豁免");

	// 页面侧反向证明：删掉命中词后转绿（防「页面规则永远红」的假阳性）。
	const pageStripped = scanPage(`<p>Namespace and system configuration</p>\n`, "page-strip.html");
	expect(pageStripped.violations.length === 0, "页面删掉命中词后应转绿（否则规则在乱扫）");

	// 实现词表：正向命中 + 三处结构化例外都不能「扫空」。
	const implRed = scanPage(`<p>以 CAS 保存</p>\n`, "impl-red.html");
	expect(implRed.violations.some((v) => v.ruleId === "impl-word" && v.hit === "CAS"), "实现词表未命中 CAS");
	const implGreen = scanPage(`<pre>PUT /v1/x</pre>\n<p><code>resourceVersion</code></p>\n<details class=\"devf\"><p>schema</p></details>\n<!-- digest -->\n`, "impl-green.html");
	expect(implGreen.violations.length === 0, `代码/dev 容器里的实现词不该红：${implGreen.violations.map((v) => v.hit).join(",")}`);
	expect(implGreen.implNodes === 0, `纯代码/dev 容器页面不该有实现词检查面（实际 ${implGreen.implNodes}）`);
	const implStripped = scanPage(`<p>保存</p>\n`, "impl-strip.html");
	expect(implStripped.violations.length === 0, "删掉实现词后应转绿（否则实现在乱扫）");
	const implComment = scanPage(`<div>\n  <!-- 以 CAS 保存 -->\n  <p>保存</p>\n</div>\n`, "impl-comment.html");
	expect(implComment.violations.length === 0, "实现词只在 HTML 注释里时必须保持绿（注释是写给维护者的）");

	// 页面侧 define 过滤不是空转：同一个 define，没被 layout 引用就不扫、被引用就扫。
	const dead = scanPage(`{{define "dead"}}<p>写给维护者</p>{{end}}\n`, "page-dead.html", { renderedDefines: new Set(["content"]) });
	expect(dead.violations.length === 0 && dead.nodes === 0, "未被引用的 define 体不该扫（当前扫到了）");

	log(failures === 0 ? "  ✅ 自测通过：红样例全红、绿样例全绿、豁免正反都对" : `  ✗ 自测失败 ${failures} 条`);
	return failures === 0;
}

const argOf = (name, def) => process.argv.find((a) => a.startsWith(`--${name}=`))?.split("=")[1] ?? def;

async function keyRefsOf(src) { // key → 引用它的文件集合：只扫静态可判的引用形态（动态拼接的 key 一个都不豁免）
	const keyRefs = new Map();
	const roots = [path.resolve(src, "pages"), path.resolve(src, "layouts"), path.resolve("web")];
	for (const root of roots) {
		let files;
		try {
			files = (await fs.readdir(root)).filter((f) => /\.(html|js)$/.test(f)).sort();
		} catch {
			continue;
		}
		for (const name of files) {
			const rel = `${path.relative(process.cwd(), root).split(path.sep).join("/")}/${name}`;
			const text = await fs.readFile(path.join(root, name), "utf8");
			for (const r of extractRefs(text, rel).refs) {
				if (!keyRefs.has(r.key)) keyRefs.set(r.key, new Set());
				keyRefs.get(r.key).add(rel);
			}
		}
	}
	return keyRefs;
}

async function main() {
	const src = argOf("src", "web-src");
	const keyRefs = await keyRefsOf(src);
	const dir = path.resolve(src, "locales");
	let names;
	try {
		names = (await fs.readdir(dir)).filter((f) => f.endsWith(".yaml")).sort();
	} catch (e) {
		console.error(`✗ 读不到 locale 目录 ${dir}：${e?.message ?? e}`);
		return 2;
	}

	const all = [];
	const stats = [];
	for (const name of names) {
		const file = path.join(dir, name);
		const text = await fs.readFile(file, "utf8");
		let got;
		try {
			got = scanLocale(text, `${src}/locales/${name}`, { keyRefs });
		} catch (e) {
			if (e instanceof ParseError) {
				console.error(`✗ ${e.message}`);
				return 2;
			}
			throw e;
		}
		stats.push({ name, values: got.values, violations: got.violations, exemptKeys: got.exemptKeys });
		all.push(...got.violations);
	}

	const values = stats.reduce((n, s) => n + s.values, 0);

	// 页面模板（#458）：先算「哪些 define 会被 layout 接进渲染树」，再逐个页面抽文本节点。
	const pageDir = path.resolve(src, "pages");
	const layoutDir = path.resolve(src, "layouts");
	let pageNames;
	let layoutTexts;
	try {
		pageNames = (await fs.readdir(pageDir)).filter((f) => f.endsWith(".html")).sort();
	} catch (e) {
		console.error(`✗ 读不到页面目录 ${pageDir}：${e?.message ?? e}`);
		return 2;
	}
	try {
		const layoutNames = (await fs.readdir(layoutDir)).filter((f) => f.endsWith(".html")).sort();
		layoutTexts = await Promise.all(layoutNames.map((n) => fs.readFile(path.join(layoutDir, n), "utf8")));
	} catch (e) {
		console.error(`✗ 读不到 layout 目录 ${layoutDir}（页面文本节点的可见性靠它判定）：${e?.message ?? e}`);
		return 2;
	}
	const pageTexts = await Promise.all(pageNames.map((n) => fs.readFile(path.join(pageDir, n), "utf8")));
	const rendered = renderedDefineNames([...layoutTexts, ...pageTexts]);
	let pageNodes = 0;
	let implNodes = 0;
	let proseNodes = 0;
	for (const [i, name] of pageNames.entries()) {
		const got = scanPage(pageTexts[i], `${src}/pages/${name}`, { renderedDefines: rendered });
		pageNodes += got.nodes;
		implNodes += got.implNodes;
		proseNodes += got.proseNodes;
		all.push(...got.violations);
	}

	const counts = `${names.length} 份 locale / ${values} 条值 · ${pageNames.length} 个页面 / ${pageNodes} 个文本节点（实现词检查面 ${implNodes} 个 · 散文检查面 ${proseNodes} 个）· ${RULES.length} 条规则 + ${IMPL_WORDS.length} 个实现词 + ${PROSE_RULES.length} 条散文规则`;
	const floors = `下限 locale≥${FLOORS.files} 份、值≥${FLOORS.values}；页面≥${FLOORS.pages} 个、文本节点≥${FLOORS.pageNodes}、实现词检查面≥${FLOORS.implNodes}、散文检查面≥${FLOORS.proseNodes}`;

	// 反假绿：扫描面被改窄（少语言、值为空、页面扫不到、文本节点归零）不能算绿。
	const seenExempt = new Set(stats.flatMap((st) => st.exemptKeys ?? []));
	const floorBreach = [];
	if (names.length < FLOORS.files) floorBreach.push(`只解析到 ${names.length} 份 locale（下限 ${FLOORS.files}）`);
	if (values < FLOORS.values) floorBreach.push(`只解析到 ${values} 条值（下限 ${FLOORS.values}）`);
	if (pageNames.length < FLOORS.pages) floorBreach.push(`只扫到 ${pageNames.length} 个页面（下限 ${FLOORS.pages}）`);
	if (pageNodes < FLOORS.pageNodes) floorBreach.push(`只抽到 ${pageNodes} 个页面文本节点（下限 ${FLOORS.pageNodes}）`);
	if (implNodes < FLOORS.implNodes) floorBreach.push(`实现词检查面只剩 ${implNodes} 个页面文本节点（下限 ${FLOORS.implNodes}）：代码容器标记被写宽了？`);
	if (proseNodes < FLOORS.proseNodes) floorBreach.push(`散文检查面只剩 ${proseNodes} 个页面文本节点（下限 ${FLOORS.proseNodes}）：字面量判据是不是把整页都当成 <pre> 了？`);
	for (const e of EXEMPT_KEYS) {
		if (!seenExempt.has(e.key)) floorBreach.push(`EXEMPT_KEYS 里的 ${e.key} 在 4 份 locale 里都不存在 —— 过期的豁免就是下一次增长的口子，删掉或改名`);
	}
	for (const lang of LANGS) {
		if (!names.includes(`${lang}.yaml`)) floorBreach.push(`缺 ${lang}.yaml`);
	}

	const active = all.filter((v) => !v.exempted);
	const exempted = all.filter((v) => v.exempted);
	const blank = all.filter((v) => v.blankReason && !v.exempted);
	const unused = exempted.filter((v) => v.reason);

	if (active.length === 0 && floorBreach.length === 0) {
		console.log(`✅ 文案守卫通过：${counts}（${floors}）`);
		if (exempted.length) {
			console.log(`   行内豁免 ${exempted.length} 条：`);
			for (const v of exempted) console.log(`     ${v.file}:${v.line} → ${v.ruleId} → 理由：${v.reason}`);
		}
		return 0;
	}

	if (floorBreach.length) {
		console.error(`✗ 文案守卫：扫描面不足（这是假绿，不是通过）`);
		for (const b of floorBreach) console.error(`   · ${b}`);
		console.error(`   检查量：${counts}（${floors}）`);
		console.error(`   处置：合理缩减（删词条/加语言）就同步下调 scripts/check-ui-copy.mjs 的 FLOORS；否则查解析器。`);
		return 1;
	}

	console.error(`✗ 文案违规：${active.length} 条（用户可见文案里混进了开发过程叙述）`);
	console.error(`  检查量：${counts}（${floors}）`);
	const byFile = new Map();
	for (const v of active) {
		if (!byFile.has(v.file)) byFile.set(v.file, []);
		byFile.get(v.file).push(v);
	}
	for (const [file, vs] of byFile) {
		console.error(`\n  ${file}`);
		vs.sort((a, b) => a.line - b.line);
		for (const v of vs) {
			const shown = v.value.length > 160 ? `${v.value.slice(0, 160)}…` : v.value;
			console.error(`    ${v.file}:${v.line} → «${v.hit}» → ${shown}`);
			console.error(`      [${v.ruleId}] ${v.why}${v.key ? `   (key: ${v.key})` : "   (页面文本节点)"}`);
			if (v.blankReason) console.error(`      ↑ 这里有 ${EXEMPT_MARKER} 标记但没写理由 → 理由必填，请补或删标记`);
		}
	}
	if (unused.length) {
		console.error(`\n  另有 ${unused.length} 条已豁免（计数仅供对账）：`);
		for (const v of unused) console.error(`    ${v.file}:${v.line} → ${v.ruleId} → ${v.reason}`);
	}

	console.error("\n  处置建议：");
	console.error("    · 纯开发叙述（搬到哪去了 / 以前是 v3 / 工单号 / 构建工具）→ 直接删；");
	console.error("      没人引用的 key 四份 locale 一起删（make fe 之后 check-i18n-keys 会跟上）；");
	console.error("    · 有信息量的 → 改写成用户视角：说「是什么、能做什么」，不说「从哪来」。");
	console.error("      别把功能文案一起删掉，也别为了过守卫把句子拆得读不通；");
	console.error("    · 页面模板里的同类文字 → 挪进 locale 再改写成用户视角；");
	console.error("      纯开发注释留在 <!-- --> 里，那本来就该写给维护者看；");
	console.error("    · 确属合理（例如过渡期公告要留工单号）→ 值所在行或上一行写");
	console.error(`      \`# ${EXEMPT_MARKER}: <理由>\`（locale）或 \`<!-- ${EXEMPT_MARKER}: <理由> -->\`（页面）；理由必填，空理由仍然失败。`);
	return 1;
}

const isDirectRun = runsAsMain(import.meta.url);
if (isDirectRun) {
	const known = (a) => a === "--self-test" || a.startsWith("--src=");
	const unknown = process.argv.slice(2).filter((a) => !known(a));
	if (unknown.length) {
		console.error(`✗ 不认识的参数：${unknown.join(" ")}`);
		console.error("用法：node scripts/check-ui-copy.mjs [--src=web-src] [--self-test]");
		process.exit(2);
	}
	if (process.argv.includes("--self-test")) {
		process.exit(selfTest() ? 0 : 1);
	}
	main()
		.then((code) => process.exit(code))
		.catch((e) => {
			console.error(`✗ check-ui-copy 运行失败：${e?.stack ?? e}`);
			process.exit(2);
		});
}
