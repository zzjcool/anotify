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
 * 判据
 *   扫 `web-src/locales/*.yaml` 的**值**（跳过注释行与 key 名；值里的 HTML 标签不剥，
 *   因为泄漏点也可能在 <b>/<code> 里）。命中高危词表即失败，逐条打印
 *   `文件:行 → 命中词 → 原文`。
 *
 * 为什么只扫 locale 值：`web-src/pages/*.html` 里**没有散文**——
 *   2026-11 实测：把注释 / <script> / <style> / {{…}} 表达式剥掉后，全部页面里
 *   渲染出来的可见文字只剩符号、代码样例与 ASCII 图（零散文）；所有散文
 *   都走 `{{t "…"}}` / `t("…")`。所以 locale 就是用户可见文案的唯一真相源，
 *   一个检查面 = 全量检查面。页面里的开发注释（HTML 注释、块注释、行注释）是合法的，
 *   不在本检查的范围内（它们本来就该写给维护者看）。
 *
 * 词表怎么定的（窄而准，靠实测不是靠感觉）
 *   先按 5 类特征（迁移叙述 / 对照内部实现 / 对开发者喊话 / 解释「为什么这么设计」/
 *   悬空指代）全量扫一遍 4 份 locale，逐条人工判定后**只留零误报的词**：
 *   定稿前在修完的 locale 上跑过，命中为零（见 docs/reports/ui-copy-audit.md）。
 *   被**故意排除**的高频词（都会误报，理由逐条记录）：
 *     · 「make + 目标名」：`make dev` 是本仓自托管排障指引的正常文案
 *       （receivers.pair.no_backend 等处），英文的 "make it" 是动词 → 改为只查 `sitegen`
 *     · `\b[0-9a-f]{7,40}\b`（commit sha 形状）：会命中 `2592000` 这种秒数
 *     · `移行`（ja）：`…リアルタイムストリームに移行します` 是数据流状态迁移，不是「配置搬家」
 *     · `旧 `（ja/zh 裸词）：`更旧 →`（分页）、`旧 digest`（真实陈旧态标签）都是正常文案
 *     · `legacy`：只保留在 en 词表里（本仓当前没有正常用法），中文侧用「旧 spec/旧形态」等具体词
 *     · `XXX`、`TXXX`：`XXXX-XXXX` 是恢复码的格式提示，是正常文案
 *
 * 允许行内豁免
 *   值所在行、或**上一行**的注释里写 `# ui-copy-ok: <理由>`。
 *   理由必填：空理由（`# ui-copy-ok:` 后面什么都没有）仍然失败 —— 否则
 *   「加个标记就过」会变成新的假绿通道。
 *
 * 反假绿
 *   · `--self-test` 跑内置红/绿样例（12 红 / 8 绿 / 2 豁免），红样例必须真红，
 *     且断言**是哪条规则**红的（防「碰巧被别的规则抓到」）。
 *   · 打印 文件数 / 值数 / 命中数，并对文件数与值数设下限：解析出空值集一律失败 ——
 *     「绿」与「没检查」不能同形（本仓反复吃这个亏）。
 *   · locale YAML 解析复用 check-i18n-keys.mjs 的 parseLocale：遇到不认识的语法
 *     抛 ParseError → 退出码 2，绝不静默跳过。
 *
 * 用法
 *   node scripts/check-ui-copy.mjs                 # 判据校验，违规退出 1
 *   node scripts/check-ui-copy.mjs --self-test     # 跑脚本自身的红/绿自测
 *   node scripts/check-ui-copy.mjs --src=web-src
 *   退出码：0 绿 / 1 违规 / 2 解析失败或用法错误
 *   接线：`make ci`（紧跟 check-i18n-keys；Makefile 由主 agent 统一收口，本脚本不自注册）
 */
import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { LANGS, ParseError, parseLocale } from "./check-i18n-keys.mjs";

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
 * 反假绿下限（棘轮）：2026-11 在 main 上实测的值 —— 4 份 locale、非空值 9284。
 * 低于它 = 扫描面被改窄了（要么 locale 被拆走，要么解析器坏了），红。
 * 合理缩减（删词条/加语言）时必须同步下调这里，否则这项检查会一直失败。
 */
const FLOORS = { files: 4, values: 9000 };

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
export function scanLocale(text, file = "<memory>") {
	const { entries } = parseLocale(text, file);
	const lines = text.split("\n");
	/** 每条违规：{ file, line, key, ruleId, why, hit, value, exempted, reason } */
	const violations = [];
	let values = 0;

	for (const [key, { line, value }] of entries) {
		if (!value.trim()) continue;
		values += 1;
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
	}
	return { violations, values };
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
		{ text: `docs:\n  note: "细节见 docs/design/v4/decisions.md"\n`, rule: "docs-path" },
		{ text: `docs:\n  empty: "重新运行 make sitegen 即可生成这一节"\n`, rule: "build-tool" },
		{ text: `docs:\n  note: "Re-run make sitegen to generate this section"\n`, rule: "build-tool" },
		// 对开发者喊话
		{ text: `ux:\n  summary: "开发者说明：槽位判定与批量写规则"\n`, rule: "zh-dev-audience" },
		{ text: `docs:\n  lede: "The notes below are for maintainers and anyone debugging."\n`, rule: "en-dev-audience" },
		{ text: `sources:\n  edges: "Legacy declaration"\n`, rule: "en-retired-shape" },
		{ text: `caps:\n  stale: "旧形态声明"\n`, rule: "cjk-retired-shape" },
		{ text: `docs:\n  x: "ビルド時に生成されます"\n`, rule: "ja-build-pipeline" },
		{ text: `docs:\n  x: "Se genera en tiempo de compilación"\n`, rule: "es-build-pipeline" },
		// TODO
		{ text: `a:\n  b: "TODO: 换成真数据"\n`, rule: "dev-todo" },
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
		// `make dev` 是本仓自托管排障指引的正常文案；英文的 "make it" 是动词
		`recv:\n  no_backend: "请确认服务端已启动（本地可运行 make dev）后重试。"\n`,
		`ns:\n  yaml_hint: "Paste it back to save; the page will not make it public."\n`,
		// `移行` 是数据流状态迁移（进入实时流），不是配置搬家
		`docs:\n  reconnect: "その後リアルタイムストリームに移行します。"\n`,
		// 历史「状态」标签（旧 digest = 陈旧 digest）不是历史「过程」叙述
		`caps:\n  stale: "旧 digest"\n`,
		// 注解里的 `#` 与值里的普通数字
		`docs:\n  retain: "默认 604800 秒，最大 2592000 秒"\n`,
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
	expect(SELF_TEST.red.length >= 6, `红样例必须 ≥6（当前 ${SELF_TEST.red.length}）`);
	expect(SELF_TEST.green.length >= 4, `绿样例必须 ≥4（当前 ${SELF_TEST.green.length}）`);

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

	log(failures === 0 ? "  ✅ 自测通过：红样例全红、绿样例全绿、豁免正反都对" : `  ✗ 自测失败 ${failures} 条`);
	return failures === 0;
}

const argOf = (name, def) => process.argv.find((a) => a.startsWith(`--${name}=`))?.split("=")[1] ?? def;

async function main() {
	const src = argOf("src", "web-src");
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
			got = scanLocale(text, `${src}/locales/${name}`);
		} catch (e) {
			if (e instanceof ParseError) {
				console.error(`✗ ${e.message}`);
				return 2;
			}
			throw e;
		}
		stats.push({ name, values: got.values, violations: got.violations });
		all.push(...got.violations);
	}

	const values = stats.reduce((n, s) => n + s.values, 0);
	const counts = `${names.length} 文件 / ${values} 条值 / ${RULES.length} 条规则`;
	const floors = `下限 文件≥${FLOORS.files}、值≥${FLOORS.values}`;

	// 反假绿：扫描面被改窄（少语言、值为空）不能算绿。
	const floorBreach = [];
	if (names.length < FLOORS.files) floorBreach.push(`只解析到 ${names.length} 份 locale（下限 ${FLOORS.files}）`);
	if (values < FLOORS.values) floorBreach.push(`只解析到 ${values} 条值（下限 ${FLOORS.values}）`);
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
			console.error(`      [${v.ruleId}] ${v.why}   (key: ${v.key})`);
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
	console.error("    · 确属合理（例如过渡期公告要留工单号）→ 值所在行或上一行写");
	console.error(`      \`# ${EXEMPT_MARKER}: <理由>\`（理由必填，空理由仍然失败）。`);
	return 1;
}

const isDirectRun =
	process.argv[1] && path.resolve(process.argv[1]) === path.resolve(fileURLToPath(import.meta.url));
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
