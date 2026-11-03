#!/usr/bin/env node
/* Anotify · 前端死类（Dead Class）守卫
 *
 * 作用：扫描 web-src/pages|layouts 里的 class="..."，以及 JS 里的
 *       class 字符串（class: "..." / '...' / `...`），逐个校验是否落在
 *       本项目设计系统 / Tailwind 工具类的有效集合内。防的是「新增前端区块时
 *       发明了一个没在任何 CSS 里定义、又不是合法 Tailwind 工具类的 class」
 *       （例：曾出现的 input-field、dot-info），这种类在 Tailwind CDN 下
 *       会静默不生效，造成风格格格不入。
 *
 * 校验规则（任一命中即视为有效）：
 *   ① 在 ui.css / tokens.css 里定义的组件类（含 `.chip .x` 这类嵌套选择器）
 *   ② 在页面内联 <style> 块里定义的类
 *   ③ 在 JS 里作为 querySelector(All)(".x") 选择器 hook 出现的类
 *   ④ 合法的 Tailwind 工具类（含响应式/暗色/变体前缀 + 任意值语法）
 *
 * 用法：
 *   node scripts/check-classes.mjs [--src=web-src] [--self-test]
 *   --self-test  跑脚本自身的红/绿自测（纯内存 fixture，不改仓库文件）
 *   默认直接列出死类并以非零退出码失败（供 CI / make fe 拦下）
 *
 * 说明（JS 扫描边界，均为「宁可漏报不误报」）：
 *   · 动态拼接的类（`x ${meta.y}`）只校验 ${...} 之外的静态片段，表达式内部跳过；
 *   · `className = "..."` / `setAttribute("class", ...)` / `classList.add("...")`
 *     暂不在 JS 扫描范围内（只覆盖 class: 字面量）；
 *   · 字符串拼接 `"a " + x + " b"` 只取第一段字面量。
 */
import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const SRC = path.resolve(
	process.argv.find((a) => a.startsWith("--src="))?.split("=")[1] ?? "web-src",
);

// ---------- ① ui.css / tokens.css 里定义的组件类 ----------
/** 从 CSS 文本里取出所有「选择器位置」的类名。
 *  逐段扫 `{ ... }`，把每个 `{` 之前的 prelude 当选择器（@ 开头的 at-rule
 *  prelude 除外），因此能覆盖 `.chip .x {`、`.a,\n.b {` 与 @media 内嵌规则，
 *  又不会把声明块里的 0.5rem 或注释文字当成类名。 */
function selectorClasses(css) {
	const out = new Set();
	let buf = "";
	for (const ch of css.replace(/\/\*[\s\S]*?\*\//g, "")) {
		if (ch === "{") {
			const prelude = buf.trim();
			buf = "";
			if (!prelude.startsWith("@")) {
				for (const m of prelude.matchAll(/(?:^|[\s,>+~])\.([a-zA-Z0-9_][\w-]*)/g))
					out.add(m[1]);
			}
		} else if (ch === "}" || ch === ";") {
			buf = "";
		} else {
			buf += ch;
		}
	}
	return out;
}

async function cssTopClasses() {
	const out = new Set();
	for (const file of ["web/ui.css", "web/tokens.css"]) {
		try {
			const css = await fs.readFile(file, "utf8");
			for (const cls of selectorClasses(css)) out.add(cls);
		} catch {
			/* 文件缺失忽略 */
		}
	}
	return out;
}

// ---------- ①bis JS 里的 class 字符串（class: "..." / '...' / `...`） ----------
/** 跳过 `${ ... }` 表达式（含嵌套花括号与其内部字符串字面量），返回 `}` 之后的下标。 */
function skipTemplateExpression(src, i) {
	let depth = 1;
	while (i < src.length && depth > 0) {
		const ch = src[i];
		if (ch === '"' || ch === "'" || ch === "`") {
			i++;
			while (i < src.length && src[i] !== ch) {
				if (src[i] === "\\") i++;
				i++;
			}
			i++;
			continue;
		}
		if (ch === "{") depth++;
		else if (ch === "}") depth--;
		i++;
	}
	return i;
}

/** 提取 JS 源码里 class 字符串的静态片段（每个片段为未拆空白的原始字符串）。
 *  模板字符串按 `${...}` 切段：动态表达式整体跳过，只保留前后静态片段。 */
export function jsClassLiterals(src) {
	const out = [];
	const re = /\bclass\s*:\s*(["'`])/g;
	let m;
	while ((m = re.exec(src))) {
		const quote = m[1];
		const start = re.lastIndex;
		if (quote !== "`") {
			const end = src.indexOf(quote, start);
			if (end === -1) continue;
			out.push(src.slice(start, end));
			re.lastIndex = end + 1;
			continue;
		}
		let i = start;
		let chunk = "";
		while (i < src.length) {
			const ch = src[i];
			if (ch === "\\") {
				chunk += src[i + 1] ?? "";
				i += 2;
				continue;
			}
			if (ch === "`") break;
			if (ch === "$" && src[i + 1] === "{") {
				out.push(chunk);
				chunk = "";
				i = skipTemplateExpression(src, i + 2);
				continue;
			}
			chunk += ch;
			i++;
		}
		out.push(chunk);
		re.lastIndex = i + 1;
	}
	return out;
}

/** 取出 HTML 里所有内联 <script> 块的内容（外链脚本天然跳过）。 */
export function scriptBlocks(html) {
	const out = [];
	for (const m of html.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/gi))
		out.push(m[1]);
	return out;
}

// ---------- ② 页面内联 <style> 类 ----------
async function inlineStyleClasses(files) {
	const out = new Set();
	for (const f of files) {
		const html = await fs.readFile(f, "utf8");
		for (const m of html.matchAll(/^\s*\.([a-zA-Z0-9_][\w-]*)/gm))
			out.add(m[1]);
	}
	return out;
}

// ---------- ③ JS 选择器 hook 类 ----------
async function jsHookClasses(files) {
	const out = new Set();
	for (const f of files) {
		const html = await fs.readFile(f, "utf8");
		for (const m of html.matchAll(
			/querySelector(?:All)?\(['"][^'"]*?\.([\w-]+)/g,
		))
			out.add(m[1]);
		// 也抓 JS 里 class: "...x..." 模板的静态片段（弱化，仅收集已知）
	}
	return out;
}

// ---------- ④ Tailwind 工具类判定 ----------
// 依据本项目实际用到的类 + 标准 Tailwind 工具类族归纳。
// 一个类名若命中下面任一「族前缀/完整模式」即视为合法工具类。
const TAILWIND_COMPLETE = new Set([
	// 布局
	"active", // 激活态 hook（配合 .x.active 内联复合选择器，如 tab-btn.active）
	"block",
	"inline",
	"inline-block",
	"inline-flex",
	"flex",
	"inline-grid",
	"grid",
	"table",
	"table-row",
	"table-cell",
	"contents",
	"hidden",
	"flow-root",
	"static",
	"fixed",
	"absolute",
	"relative",
	"sticky",
	"visible",
	"invisible",
	"flex-1",
	"flex-auto",
	"flex-initial",
	"flex-none",
	"flex-col",
	"flex-row",
	"flex-wrap",
	"flex-nowrap",
	"flex-col-reverse",
	"flex-row-reverse",
	"shrink",
	"shrink-0",
	"flex-shrink",
	"flex-shrink-0", // Tailwind v3 旧名；web/tailwind.css 产物里仍存在，web/partials.js 在用
	"grow",
	"grow-0",
	"order-first",
	"order-last",
	"grid-flow-row",
	"grid-flow-col",
	"auto-rows-auto",
	"auto-cols-auto",
	"justify-normal",
	"justify-stretch",
	"overflow-x-hidden",
	"overflow-y-hidden",
	"overflow-x-auto",
	"overflow-y-auto",
	"overscroll-x-none",
	"overscroll-y-none",
	"overscroll-contain",
	"truncate",
	"whitespace-nowrap",
	"whitespace-pre",
	"whitespace-pre-line",
	"whitespace-pre-wrap",
	"break-words",
	"break-all",
	"break-normal",
	"break-keep",
	"antialiased",
	"subpixel-antialiased",
	"sr-only",
	"not-sr-only",
	"box-border",
	"box-content",
	"pointer-events-none",
	"pointer-events-auto",
	"select-none",
	"select-text",
	"select-all",
	"select-auto",
	"appearance-none",
	"resize-none",
	"resize-y",
	"resize-x",
	"resize",
	"snap-x",
	"snap-y",
	"snap-start",
	"snap-center",
	"snap-end",
	"snap-mandatory",
	"isolation-auto",
	"isolate",
	"mix-blend-normal",
	"mix-blend-multiply",
	"mix-blend-screen",
	"mix-blend-overlay",
	"underline",
	"no-underline",
	"uppercase",
	"lowercase",
	"capitalize",
	"normal-case",
	"italic",
	"not-italic",
	"ordinal",
	"tabular-nums",
	"line-clamp-1",
	"line-clamp-2",
	"line-clamp-3",
	"list-none",
	"list-disc",
	"list-decimal",
	"text-center",
	"text-left",
	"text-right",
	"text-justify",
	"text-start",
	"text-end",
	"decoration-auto",
	"decoration-solid",
	"decoration-double",
	"decoration-dotted",
	"decoration-dashed",
	"decoration-wavy",
	"animate-ping",
	"animate-pulse",
	"animate-pulse-fast",
	"animate-spin",
	"animate-bounce",
	"animate-none",
	"animate-ping-slow",
	"shadow-sm",
	"shadow",
	"shadow-md",
	"shadow-lg",
	"shadow-xl",
	"shadow-2xl",
	"shadow-inner",
	"shadow-none",
	"ring-1",
	"ring-2",
	"ring-4",
	"ring-8",
	"ring-0",
	"ring",
	"ring-inset",
	"cursor-pointer",
	"cursor-default",
	"cursor-not-allowed",
	"cursor-text",
	"cursor-wait",
	"cursor-move",
	"cursor-grab",
	"cursor-zoom-in",
	"cursor-zoom-out",
	"overflow-auto",
	"overflow-hidden",
	"overflow-clip",
	"overflow-visible",
	"overflow-scroll",
	"overflow-x-auto",
	"overflow-y-auto",
	"overflow-x-hidden",
	"overflow-y-hidden",
	"overflow-x-scroll",
	"overflow-y-scroll",
	"overflow-x-clip",
	"overflow-y-clip",
	"overflow-x-visible",
	"overflow-y-visible",
	"transition",
	"transition-none",
	"transition-all",
	"transition-colors",
	"transition-opacity",
	"transition-shadow",
	"transition-transform",
	"transition-[.*]",
	"scale-95",
	"scale-100",
	"scale-105",
	"scale-110",
	"scale-125",
	"scale-150",
	"rotate-0",
	"rotate-45",
	"rotate-90",
	"rotate-180",
	"rotate-[-.*]",
	"self-auto",
	"self-start",
	"self-center",
	"self-end",
	"self-stretch",
	"self-baseline",
	"items-start",
	"items-center",
	"items-end",
	"items-baseline",
	"items-stretch",
	"justify-start",
	"justify-center",
	"justify-end",
	"justify-between",
	"justify-around",
	"justify-evenly",
	"justify-items-start",
	"justify-items-center",
	"justify-items-end",
	"justify-items-stretch",
	"content-start",
	"content-center",
	"content-end",
	"content-between",
	"content-around",
	"content-evenly",
	"content-stretch",
	"place-items-start",
	"place-items-center",
	"place-items-end",
	"place-content-center",
	"place-content-between",
	"place-content-around",
	"place-content-stretch",
	"object-contain",
	"object-cover",
	"object-fill",
	"object-none",
	"object-scale-down",
	"outline-hidden",
	"list-none",
	"list-disc",
	"list-decimal",
	"list-inside",
	"list-outside",
	"select-none",
	"select-text",
	"select-all",
	"select-auto",
	"whitespace-normal",
	"whitespace-nowrap",
	"whitespace-pre",
	"whitespace-pre-line",
	"whitespace-pre-wrap",
	"break-normal",
	"break-words",
	"break-all",
	"break-keep",
	"pointer-events-none",
	"pointer-events-auto",
	"border-collapse",
	"border-separate",
	"table-auto",
	"table-fixed",
	"cursor-default",
	"uppercase",
	"lowercase",
	"capitalize",
	"normal-case",
	"italic",
	"not-italic",
	"underline",
	"overline",
	"line-through",
	"no-underline",
	"transform",
	"transform-gpu",
	"transform-none",
	"duration-75",
	"duration-100",
	"duration-150",
	"duration-200",
	"duration-300",
	"duration-500",
	"duration-700",
	"duration-1000",
	"duration-[.*]",
	"ease-linear",
	"ease-in",
	"ease-out",
	"ease-in-out",
	"ease-[.*]",
	"delay-75",
	"delay-100",
	"delay-150",
	"delay-200",
	"delay-300",
	"delay-500",
	"delay-700",
	"delay-1000",
	"animate-none",
	"animate-spin",
	"animate-ping",
	"animate-pulse",
	"animate-bounce",
	"animate-[.*]",
	"invisible",
	"visible",
	"opacity-0",
	"opacity-100",
	"ring-0",
	"ring-1",
	"ring-2",
	"ring-4",
	"ring-8",
	"ring",
	"ring-inset",
	"bg-clip-text",
	"bg-clip-border",
	"bg-clip-padding",
	"bg-clip-content",
	"bg-origin-border",
	"bg-origin-padding",
	"bg-origin-content",
	"bg-cover",
	"bg-contain",
	"bg-repeat",
	"bg-no-repeat",
	"bg-repeat-x",
	"bg-repeat-y",
	"bg-center",
	"bg-top",
	"bg-bottom",
	"bg-left",
	"bg-right",
	"text-ellipsis",
	"text-clip",
	"sr-only",
	"not-sr-only",
	"shrink",
	"shrink-0",
	"grow",
	"grow-0",
	"flex-wrap",
	"flex-nowrap",
	"flex-col",
	"flex-row",
	"flex-col-reverse",
	"flex-row-reverse",
	"flex-1",
	"flex-auto",
	"flex-initial",
	"flex-none",
	"grid-flow-row",
	"grid-flow-col",
	"grid-flow-dense",
	"grid-flow-row-dense",
	"grid-flow-col-dense",
	"aspect-auto",
	"aspect-square",
	"aspect-video",
	"isolate",
	"isolation-auto",
	"text-ellipsis",
	"text-clip",
	"snap-start",
	"snap-center",
	"snap-end",
	"snap-mandatory",
	"snap-none",
	"decoration-solid",
	"decoration-double",
	"decoration-dotted",
	"decoration-dashed",
	"decoration-wavy",
	"accent-auto",
	"caret-transparent",
	"text-transparent",
	"text-current",
	"text-inherit",
	"rounded-none",
	"rounded-sm",
	"rounded",
	"rounded-md",
	"rounded-lg",
	"rounded-xl",
	"rounded-2xl",
	"rounded-3xl",
	"rounded-full",
	"border-0",
	"border",
	"border-2",
	"border-4",
	"border-8",
	"border-solid",
	"border-dashed",
	"border-dotted",
	"border-double",
	"border-none",
	"divide-x",
	"divide-y",
	"divide-x-reverse",
	"divide-y-reverse",
	"outline-none",
	"outline",
	"outline-dashed",
	"outline-dotted",
	"group",
	"peer",
	"container",
]);

const TAILWIND_PREFIX = [
	// 尺寸/间距类：任意数值
	/^(w|h|min-w|max-w|min-h|max-h)-/,
	/^(p|px|py|pt|pr|pb|pl|m|mx|my|mt|mr|mb|ml|gap|gap-x|gap-y|space-x|space-y|space-x-reverse|space-y-reverse)-/,
	/^inset-(0|x|y|t|r|b|l|auto)/,
	/^(top|right|bottom|left)-/,
	/^z-\d+/,
	/^order-\d+/,
	/^grid-(cols|rows)-/,
	/^col-(span|start|end)-/,
	/^row-(span|start|end)-/,
	/^flex-grow|^grow-/,
	/^basis-/,
	/^auto-(rows|cols)-(min|max|fr|\d+)/,
	// 排版
	/^text-(xs|sm|base|lg|xl|2xl|3xl|4xl|5xl|6xl|7xl|8xl|9xl)$/,
	/^text-\[[^\]]+\]$/,
	/^text-(white|black|zinc|gray|neutral|stone|red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose)(-\d+)?(\/\d+)?$/,
	/^font-(thin|extralight|light|normal|medium|semibold|bold|extrabold|black|mono|sans|serif)$/,
	/^leading-(none|tight|snug|normal|relaxed|loose|\d+)$/,
	/^tracking-(tighter|tight|normal|wide|wider|widest)$/,
	/^text-(ellipsis|clip)$/,
	/^indent-?/,
	/^list-(inside|outside)$/,
	/^decoration-(\d+|auto)/,
	/^underline-offset-/,
	/^align-(baseline|top|middle|bottom|text-top|text-bottom|sub|super)/,
	// 颜色类：背景/边框/文字/填充/描边 + 透明度（含 /5、/[0.06] 任意透明度）
	/^(bg|border|divide|ring|outline|text|shadow|decoration|fill|stroke|accent|caret|placeholder)-(white|black|transparent|current|inherit|zinc|gray|neutral|stone|red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose)(-\d+)?(\/\d+|\/\[[^\]]+\])?$/,
	/^(from|via|to)-(white|black|transparent|zinc|gray|red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose)(-\d+)?(\/\d+|\/\[[^\]]+\])?$/,
	/^bg-gradient-to-(t|tr|r|br|b|bl|l|tl|rb|rt|lb|lt)$/,
	/^(bg|border|ring|text)-\[(var\([^)]*\)|#[0-9a-fA-F]{3,8}|rgb[^]]*)\]$/,
	/^divide-(x|y)($|-)/,
	/^border-(x|y|t|r|b|l)($|-)/,
	// 渐变/杂项
	/^opacity-\d+$/,
	/^blur-(none|sm|md|lg|xl|2xl|3xl)$/,
	/^blur-\[\d+px\]$/,
	/^backdrop-blur(-(none|sm|md|lg|xl|2xl|3xl))?$/,
	/^backdrop-saturate-\d+$/,
	/^backdrop-brightness-\d+$/,
	/^backdrop-contrast-\d+$/,
	// transform：translate（含负值，如 -translate-y-full）
	/^-?translate-(x|y)-(0|1|2|3|4|5|6|7|8|9|10|11|12|14|16|20|24|28|32|36|40|44|48|52|56|60|64|72|80|96|px|0\.5|1\.5|2\.5|3\.5|full|1\/2|1\/3|2\/3|1\/4|2\/4|3\/4)$/,
	/^-?translate-(x|y)-\[[^\]]+\]$/,
	/^-?translate-z-(0|4|8|12|16|20|24|32|40|48|56|64)$/,
	/^brightness-\d+$/,
	/^contrast-\d+$/,
	/^saturate-\d+$/,
	/^sepia(-\d+)?$/,
	/^grayscale(-\d+)?$/,
	/^invert(-\d+)?$/,
	/^drop-shadow(-(sm|md|lg|xl|2xl))?$/,
	// 对齐/显示 变体（hover/focus/focus-visible/active/disabled/group-hover 等）由下面统一前缀处理
];

// 变体前缀（Tailwind CDN 常见）：响应式、状态、暗色、任意、group/peer
const TAILWIND_VARIANT =
	/^((sm|md|lg|xl|2xl|min-sm|min-md|min-lg|min-xl|min-2xl|max-sm|max-md|max-lg|max-xl|max-2xl|dark|motion-safe|motion-reduce|forced-colors|print|portrait|landscape|rtl|ltr|supports-\[\w+\]|aria-\[\w+=?\\?\w*\]|group|peer|group-hover|group-focus|group-active|group-\[.*\]|peer-hover|has-\[[^\]]+\]|first|last|odd|even|only|first-of-type|last-of-type|visited|target|open|checked|indeterminate|default|required|valid|invalid|in-range|out-of-range|placeholder-shown|autofill|read-only|disabled|enabled|hover|focus|focus-visible|focus-within|active|link|any-hover|any-pointer):)+/;

export function isTailwind(cls) {
	// 重要修饰符 ! 前缀（如 !bg-[var(--panel-modal)]）
	if (cls.startsWith("!")) cls = cls.slice(1);
	if (TAILWIND_COMPLETE.has(cls)) return true;
	// 任意值通用形：bg-[var(...)]、w-[calc(...)]、grid-cols-[12px_auto] 等
	if (/^[a-z-]+-\[[^\]]+\]$/.test(cls) || /^-?[a-z-]+-\[[^\]]+\]$/.test(cls))
		return true;
	// 去掉变体前缀后再判断（如 hover:bg-white/5 → bg-white/5）
	const stripped = cls.replace(TAILWIND_VARIANT, "");
	// 负值（如 -right-1、-top-4、-mt-2）
	if (/^-[a-z-]+-\d+$/.test(cls) || /^-[a-z-]+-\[[^\]]+\]$/.test(cls))
		return true;
	if (stripped === cls) {
		return TAILWIND_PREFIX.some((re) => re.test(cls));
	}
	// 有变体前缀：递归剥除后的基类必须仍是合法工具类
	return (
		isTailwind(stripped) || TAILWIND_PREFIX.some((re) => re.test(stripped))
	);
}

// ---------- 已知死类（JS class 串新暴露、产品侧待裁决） ----------
// 这些是扫描新增覆盖面后才可见、目前确实没有 CSS 定义的类。它们不阻塞构建
// （否则 make fe / 全量 e2e 会直接停摆），但会以 ⚠️ 打出；产品侧补完定义或删掉
// 用法后应从本表删除，不要把它当永久豁免用。
// 目前为空：lang-hint-inner 已在 ui.css 补上 min-width:0 的真实定义。
const KNOWN_DEAD_JS_CLASSES = new Map();

// ---------- 自测：证明「错拼判死、正确判活」（纯内存，不改仓库文件） ----------
async function selfTest() {
	const cases = [];
	const t = (name, actual, expected) =>
		cases.push({ name, actual, expected });

	t(
		"双引号 class: 提取",
		jsClassLiterals('el("div", { class: "side-link active" })'),
		["side-link active"],
	);
	t(
		"单引号 class: 提取",
		jsClassLiterals("el('div', { class: 'btn btn-primary' })"),
		["btn btn-primary"],
	);
	t(
		"多行 class: 提取",
		jsClassLiterals('el("div", {\n\t\tclass:\n\t\t\t"side-link",\n\t})'),
		["side-link"],
	);
	t(
		"模板串：${...} 跳过、保留前后静态片段",
		jsClassLiterals(
			'el("div", { class: `brand-tile ${wrap ? "active" : ""} rounded-lg` })',
		),
		["brand-tile ", " rounded-lg"],
	);
	t(
		"模板串：表达式里的字符串/花括号不破坏切分",
		jsClassLiterals('x = { class: `${a.b === "}" ? "active" : ""} side-link` }'),
		["", " side-link"],
	);
	t(
		"不误抄 className / classList / 无冒号字符串",
		jsClassLiterals(
			'node.className = "x"; el.classList.add("y"); if (k === "class") {}',
		),
		[],
	);
	t(
		"scriptBlocks 只取内联脚本块",
		scriptBlocks(
			'<p class="x"></p><script>const a=1</script><script src="/x.js"></script>',
		),
		["const a=1", ""],
	);

	const cssClasses = await cssTopClasses();
	const valid = (cls) =>
		cssClasses.has(cls) || isTailwind(cls);
	t("正确类 side-link → 判活", valid("side-link"), true);
	t("错拼类 side-lnik → 判死", valid("side-lnik"), false);
	t("Tailwind flex-shrink-0 → 判活", valid("flex-shrink-0"), true);
	t("嵌套选择器 .tag-chip .tag-x → 识别 tag-x", cssClasses.has("tag-x"), true);

	let failed = 0;
	for (const c of cases) {
		const same = JSON.stringify(c.actual) === JSON.stringify(c.expected);
		if (same) console.log(`  ✅ ${c.name}`);
		else {
			failed++;
			console.error(
				`  ❌ ${c.name}\n     actual  = ${JSON.stringify(c.actual)}\n     expected= ${JSON.stringify(c.expected)}`,
			);
		}
	}
	console.log(
		failed === 0
			? `✅ self-test ${cases.length}/${cases.length} 通过`
			: `✗ self-test ${cases.length - failed}/${cases.length} 通过`,
	);
	return failed === 0;
}

// ---------- 主流程 ----------
async function main() {
	const pageDir = path.join(SRC, "pages");
	const layoutDir = path.join(SRC, "layouts");
	const allFiles = [];
	for (const dir of [pageDir, layoutDir]) {
		try {
			for (const f of await fs.readdir(dir)) allFiles.push(path.join(dir, f));
		} catch {
			/* 目录缺失忽略 */
		}
	}
	const cssClasses = await cssTopClasses();
	const inlineClasses = await inlineStyleClasses(allFiles);
	const hookClasses = await jsHookClasses(allFiles);

	// 收集所有 class="..." 的静态值
	const used = new Set();
	const usageByClass = new Map();
	const htmlByFile = new Map();
	for (const f of allFiles) {
		const html = await fs.readFile(f, "utf8");
		htmlByFile.set(f, html);
		for (const m of html.matchAll(/class="([^"]*)"/g)) {
			if (m[1].includes("{{")) continue; // 模板驱动类（含 {{ }}）无法静态校验，整条跳过
			for (const cls of m[1].split(/\s+/).filter(Boolean)) {
				used.add(cls);
				if (!usageByClass.has(cls)) usageByClass.set(cls, []);
				usageByClass.get(cls).push(path.basename(f));
			}
		}
	}

	const htmlCount = used.size;

	// 收集 JS 里 class 字符串的静态片段（web/partials.js + 页面内联 <script>）。
	// 它们不进 HTML class 属性计数，但同样要过同一套 allowlist 判定。
	const jsSources = [];
	for (const file of ["web/partials.js"]) {
		try {
			jsSources.push([file, await fs.readFile(file, "utf8")]);
		} catch {
			/* 文件缺失忽略（例如只跑某个 --src 子集时） */
		}
	}
	for (const [f, html] of htmlByFile) {
		for (const block of scriptBlocks(html))
			jsSources.push([`${path.basename(f)}#script`, block]);
	}
	for (const [label, src] of jsSources) {
		for (const literal of jsClassLiterals(src)) {
			for (const cls of literal.split(/\s+/).filter(Boolean)) {
				used.add(cls);
				if (!usageByClass.has(cls)) usageByClass.set(cls, []);
				usageByClass.get(cls).push(`${label}(class:)`);
			}
		}
	}

	// 判定死类
	const dead = [];
	const knownDead = [];
	for (const cls of used) {
		if (cssClasses.has(cls)) continue;
		if (inlineClasses.has(cls)) continue;
		if (hookClasses.has(cls)) continue;
		if (isTailwind(cls)) continue;
		(KNOWN_DEAD_JS_CLASSES.has(cls) ? knownDead : dead).push(cls);
	}
	dead.sort();
	knownDead.sort();

	if (knownDead.length > 0) {
		console.warn(
			`⚠️  已知死类（JS class 串暴露、无 CSS 定义，待产品裁决，不阻塞构建）：${knownDead.join(", ")}`,
		);
		for (const cls of knownDead) {
			const why = KNOWN_DEAD_JS_CLASSES.get(cls);
			console.warn(
				`   - "${cls}"  用在: ${[...new Set(usageByClass.get(cls))].join(", ")}${why ? `\n     原因: ${why}` : ""}`,
			);
		}
	}

	if (dead.length === 0) {
		const jsOnly = used.size - htmlCount;
		console.log(
			`✅ class 校验通过：${used.size} 个类（HTML class 属性 ${htmlCount} + JS class 串新增 ${jsOnly}）全部落在设计系统 / Tailwind 工具类内`,
		);
		return;
	}

	console.error("✗ 发现「死类」（无任何 CSS 定义、非合法 Tailwind 工具类）：");
	for (const cls of dead) {
		console.error(
			`   - "${cls}"  用在: ${[...new Set(usageByClass.get(cls))].join(", ")}`,
		);
	}
	console.error("\n处置建议：");
	console.error(
		"   · 若是想表达视觉样式 → 在 ui.css 定义该组件类（或改用已有类）",
	);
	console.error(
		"   · 若只是 JS 选择器 hook → 守卫应已识别；若被误报，请给本脚本加豁免",
	);
	console.error(
		"   · 若是合法 Tailwind 类 → 判定规则未覆盖，请在 scripts/check-classes.mjs 补充",
	);
	process.exit(1);
}

const isDirectRun =
	process.argv[1] &&
	path.resolve(process.argv[1]) === path.resolve(fileURLToPath(import.meta.url));
if (isDirectRun) {
	if (process.argv.includes("--self-test")) {
		selfTest()
			.then((passed) => process.exit(passed ? 0 : 1))
			.catch((e) => {
				console.error(e);
				process.exit(1);
			});
	} else {
		main().catch((e) => {
			console.error(e);
			process.exit(1);
		});
	}
}
