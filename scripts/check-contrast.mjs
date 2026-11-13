#!/usr/bin/env node
/* ============================================================
 * check-contrast.mjs · #370 / #371 对比度机械门禁（纯 node，零依赖）
 * ------------------------------------------------------------
 * 解析 web/tokens.css 的两个主题块（`:root` 暗色 / `:root[data-theme="light"]` 亮色），
 * 按 issue #370（容器层次）与 #371（对比度 / 语义药丸）的验收配对计算 WCAG 2.1 对比度，
 * 打印矩阵；任一 REQUIRED 项低于阈值 → process.exit(1)。
 *
 * 用法：
 *   node scripts/check-contrast.mjs            # 门禁
 *   node scripts/check-contrast.mjs --verbose  # 额外打印参考配对（不参与判定）
 *
 * 约定：
 * - 只读 tokens.css，不校验页面；改 token 后必须跑本脚本。
 * - 半透明色（--*-soft / --line / color-mix(… transparent)）一律先与所在底色
 *   合成（alpha compositing）再算对比度，这是浏览器实际渲染的像素。
 * ============================================================ */

import { readFileSync } from "node:fs";
import { dirname, resolve as resolvePath } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const TOKENS = resolvePath(HERE, "..", "web", "tokens.css");
const VERBOSE = process.argv.includes("--verbose");

/* ---------- 解析 tokens.css ---------- */

/** 去掉 CSS 注释（保留换行，便于定位）。 */
function stripComments(css) {
	return css.replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, " "));
}

/** 取出 `:root { … }`（暗色）与 `:root[data-theme="light"] { … }` 两个块。 */
function themeBlocks(css) {
	const out = {};
	const re = /(:root(?:\[data-theme="light"\])?)\s*\{/g;
	let m;
	while ((m = re.exec(css))) {
		const selector = m[1];
		let depth = 1;
		let i = re.lastIndex;
		while (i < css.length && depth > 0) {
			const ch = css[i];
			if (ch === "{") depth++;
			else if (ch === "}") depth--;
			i++;
		}
		const body = css.slice(re.lastIndex, i - 1);
		if (selector === ":root[data-theme=\"light\"]") out.light = body;
		else if (out.dark === undefined) out.dark = body;
		re.lastIndex = i;
	}
	return out;
}

/** `--name: value;` → Map(name → value) */
function declarations(body) {
	const map = new Map();
	const re = /(--[a-z0-9-]+)\s*:\s*([^;]+);/gi;
	let m;
	while ((m = re.exec(body))) map.set(m[1], m[2].trim());
	return map;
}

/* ---------- 颜色求值 ---------- */

function parseHex(hex) {
	let h = hex.slice(1);
	if (h.length === 3) h = h.split("").map((c) => c + c).join("");
	if (h.length !== 6) throw new Error(`unsupported hex: ${hex}`);
	return [0, 2, 4].map((i) => parseInt(h.slice(i, i + 2), 16));
}

function parseColorFn(value) {
	// rgb( / rgba( —— 逗号或空格分隔
	const inner = value.slice(value.indexOf("(") + 1, value.lastIndexOf(")"));
	const parts = inner.replace(/\//g, " ").split(/[,\s]+/).filter(Boolean);
	const rgb = parts.slice(0, 3).map((p) => {
		const n = p.endsWith("%") ? (parseFloat(p) / 100) * 255 : parseFloat(p);
		return Math.round(Math.max(0, Math.min(255, n)));
	});
	let alpha = 1;
	const maybeAlpha = parts[3];
	if (maybeAlpha !== undefined) {
		alpha = maybeAlpha.endsWith("%") ? parseFloat(maybeAlpha) / 100 : parseFloat(maybeAlpha);
	}
	return { rgb, alpha };
}

/** 求值一个 token（支持 var() 链、#hex、rgb()/rgba()、color-mix(… transparent)）。 */
function evalToken(map, name, seen = new Set()) {
	if (seen.has(name)) throw new Error(`circular token reference: ${name}`);
	seen.add(name);
	const raw = map.get(name);
	if (raw === undefined) throw new Error(`undefined token: ${name}`);
	return evalValue(map, raw, seen);
}

function evalValue(map, raw, seen) {
	const value = raw.trim();

	// color-mix(in srgb, <color> N%, transparent)
	const mix = value.match(/^color-mix\(\s*in srgb\s*,\s*(.+?)\s+([\d.]+)%\s*,\s*transparent\s*\)$/);
	if (mix) {
		const base = evalValue(map, mix[1], seen);
		return { rgb: base.rgb, alpha: base.alpha * (parseFloat(mix[2]) / 100) };
	}

	// var(--x) 或 var(--x, fallback)
	const varMatch = value.match(/^var\(\s*(--[a-z0-9-]+)\s*(?:,[^)]*)?\)$/i);
	if (varMatch) return evalToken(map, varMatch[1], new Set(seen));

	if (value.startsWith("#")) return { rgb: parseHex(value), alpha: 1 };
	if (/^rgba?\(/i.test(value)) return parseColorFn(value);

	throw new Error(`unsupported token value: ${value}`);
}

/* ---------- WCAG ---------- */

const srgbToLinear = (c) => {
	const v = c / 255;
	return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
};

const luminance = ([r, g, b]) =>
	0.2126 * srgbToLinear(r) + 0.7152 * srgbToLinear(g) + 0.0722 * srgbToLinear(b);

/** 半透明前景合成到不透明底色上（浏览器实际像素）。 */
function composite(fg, bgRgb) {
	if (fg.alpha >= 1) return fg.rgb;
	const a = fg.alpha;
	return fg.rgb.map((c, i) => Math.round(c * a + bgRgb[i] * (1 - a)));
}

function contrast(rgbA, rgbB) {
	const la = luminance(rgbA);
	const lb = luminance(rgbB);
	const [hi, lo] = la >= lb ? [la, lb] : [lb, la];
	return (hi + 0.05) / (lo + 0.05);
}

/* ---------- 主题封装 ---------- */

function theme(map) {
	const solid = (name) => {
		const c = evalToken(map, name);
		if (c.alpha < 1) throw new Error(`token ${name} is translucent; composite it over a surface first`);
		return c.rgb;
	};
	const layer = (name, baseName) => composite(evalToken(map, name), solid(baseName));
	return { map, solid, layer };
}

const hex = (rgb) => "#" + rgb.map((c) => c.toString(16).padStart(2, "0")).join("");

/* ---------- 配对定义 ---------- */

/**
 * 每项：{ id, theme, label, fg, bg, min }
 * fg/bg 是 (t) => rgb；t 是 theme() 返回的封装。
 */
function buildCases(dark, light) {
	const PILLS = [
		// [语义, 文字色 token, 底色 token]
		["success", "--pill-success-fg", "--success-soft"],
		["warn", "--pill-warn-fg", "--warn-soft"],
		["error", "--pill-error-fg", "--error-soft"],
		["info", "--pill-info-fg", "--info-soft"],
		["accent", "--pill-accent-fg", "--accent-soft"],
	];

	const cases = [];

	/* —— #370 容器层次 —— */
	cases.push({
		id: "D370-1",
		theme: "dark",
		label: "卡片 --surface-1 / 页底 --bg ≥ 1.25",
		fg: (t) => t.solid("--surface-1"),
		bg: (t) => t.solid("--bg"),
		min: 1.25,
	});
	/* 复审 N1：为什么这里取 1.15 而不是评审的"能分出块"经验值 1.3 ——
	 * 这一对（卡片 vs 侧栏）只差 --bg-raise 一档，可用性由"卡片填充差 + 1px 描边
	 * + --shadow-1"三条腿共同承担；单看填充比率会低估层次（亮色侧栏同样如此）。
	 * 取 1.15 = 在暗色近黑区仍能肉眼分辨的下限，同时不让 token 为了刷比率而失真。 */
	cases.push({
		id: "D370-2",
		theme: "dark",
		label: "卡片 --surface-1 / 侧栏 --bg-raise ≥ 1.15（可辨）",
		fg: (t) => t.solid("--surface-1"),
		bg: (t) => t.solid("--bg-raise"),
		min: 1.15,
	});
	cases.push({
		id: "D370-3",
		theme: "dark",
		label: "表头 --surface-2 / 行底 --surface-1 ≥ 1.045（不低于改前）",
		fg: (t) => t.solid("--surface-2"),
		bg: (t) => t.solid("--surface-1"),
		min: 1.045,
	});

	/* 复审 N2：容器层次门禁原先只覆盖卡片/表头，浮层与"侧栏 vs 页底"这两对
	 * 同样是层次的一部分（弹层掉进卡片里、侧栏和页底糊在一起，都是同一类病）。
	 * 先量再定阈值：这里卡的是"不低于本 PR 落地值"，属回归下限，不是设计目标。 */
	cases.push({
		id: "D370-4",
		theme: "dark",
		label: "浮层 --panel-overlay / 卡片 --surface-1 ≥ 1.04（回归下限）",
		fg: (t) => t.solid("--panel-overlay"),
		bg: (t) => t.solid("--surface-1"),
		min: 1.04,
	});
	cases.push({
		id: "D370-5",
		theme: "dark",
		label: "弹层 --panel-modal / 页底 --bg ≥ 1.03（回归下限）",
		fg: (t) => t.solid("--panel-modal"),
		bg: (t) => t.solid("--bg"),
		min: 1.03,
	});
	cases.push({
		id: "D370-6",
		theme: "dark",
		label: "侧栏 --bg-raise / 页底 --bg ≥ 1.02（回归下限）",
		fg: (t) => t.solid("--bg-raise"),
		bg: (t) => t.solid("--bg"),
		min: 1.02,
	});

	// 亮色：白卡 + 1px 边框必须可辨（≥1.3），填充差做兜底（≥1.08）
	for (const [id, label, outer] of [
		["L370-1", "亮色卡片边框 / 卡片底 ≥ 1.30", "--surface-1"],
		["L370-2", "亮色卡片边框 / 页底 --bg ≥ 1.30", "--bg"],
		["L370-3", "亮色侧栏边框 / 侧栏 --bg-raise ≥ 1.30", "--bg-raise"],
	]) {
		cases.push({
			id,
			theme: "light",
			label,
			fg: (t) => t.layer("--line", outer),
			bg: (t) => t.solid(outer),
			min: 1.3,
		});
	}
	for (const [id, label, outer] of [
		["L370-4", "亮色卡片底 / 页底（填充差，反白 on 白）≥ 1.08", "--bg"],
		["L370-5", "亮色卡片底 / 侧栏（填充差，反白 on 白）≥ 1.08", "--bg-raise"],
	]) {
		cases.push({
			id,
			theme: "light",
			label,
			fg: (t) => t.solid("--surface-1"),
			bg: (t) => t.solid(outer),
			min: 1.08,
		});
	}
	cases.push({
		id: "L370-6",
		theme: "light",
		label: "表头 --surface-2 / 行底 --surface-1 ≥ 1.045（不低于改前）",
		fg: (t) => t.solid("--surface-2"),
		bg: (t) => t.solid("--surface-1"),
		min: 1.045,
	});

	/* —— #371 极弱文字 --faint（三种底：卡片 / 页底 / 输入框 = --surface-2）—— */
	for (const [themeName, t] of [
		["dark", dark],
		["light", light],
	]) {
		for (const [id, label, bgTok] of [
			["1", "卡片底 --surface-1", "--surface-1"],
			["2", "页底 --bg", "--bg"],
			["3", "输入框底 --surface-2（.input::placeholder）", "--surface-2"],
		]) {
			cases.push({
				id: `${themeName === "dark" ? "D" : "L"}371-faint-${id}`,
				theme: themeName,
				label: `${themeName === "dark" ? "暗" : "亮"} --faint / ${label} ≥ 4.5`,
				fg: (tt) => tt.solid("--faint"),
				bg: (tt) => tt.solid(bgTok),
				min: 4.5,
			});
		}
		// 层级不得倒挂：text > muted > faint，且三者都要能当正文读
		for (const [tok, name] of [
			["--text", "正文 --text"],
			["--muted", "次要 --muted"],
		]) {
			cases.push({
				id: `${themeName === "dark" ? "D" : "L"}371-${tok.slice(2)}`,
				theme: themeName,
				label: `${themeName === "dark" ? "暗" : "亮"} ${name} / 卡片底 ≥ 4.5`,
				fg: (tt) => tt.solid(tok),
				bg: (tt) => tt.solid("--surface-1"),
				min: 4.5,
			});
		}
	}

	/* —— #371 语义药丸：.status-*（无边框）与 .badge-*（1px 边框）同一色阶 —— */
	for (const [themeName, t] of [
		["dark", dark],
		["light", light],
	]) {
		for (const [semantic, fgTok, bgTok] of PILLS) {
			for (const family of [".status-*", ".badge-*"]) {
				cases.push({
					id: `${themeName === "dark" ? "D" : "L"}371-pill-${semantic}-${family === ".status-*" ? "status" : "badge"}`,
					theme: themeName,
					label: `${themeName === "dark" ? "暗" : "亮"} ${family} ${semantic} ≥ 4.5`,
					fg: (tt) => tt.solid(fgTok),
					bg: (tt) => tt.layer(bgTok, "--surface-1"),
					min: 4.5,
				});
			}
		}
		// neutral：.status-off（--surface-3 底）/ .badge-neutral（--surface-2 底）
		for (const [id, baseTok, label] of [
			["status", "--surface-3", ".status-* neutral（--surface-3 底）"],
			["badge", "--surface-2", ".badge-* neutral（--surface-2 底）"],
		]) {
			cases.push({
				id: `${themeName === "dark" ? "D" : "L"}371-pill-neutral-${id}`,
				theme: themeName,
				label: `${themeName === "dark" ? "暗" : "亮"} ${label} ≥ 4.5`,
				fg: (tt) => tt.solid("--pill-neutral-fg"),
				bg: (tt) => tt.solid(baseTok),
				min: 4.5,
			});
		}
	}

	/* —— 回归下限：已 PASS 项只许升不许降（#371 明确点名）—— */
	cases.push({
		id: "FLOOR-status-success",
		theme: "light",
		label: "亮 .status-success 不得低于改前 4.85",
		fg: (t) => t.solid("--pill-success-fg"),
		bg: (t) => t.layer("--success-soft", "--surface-1"),
		min: 4.85,
	});
	cases.push({
		id: "FLOOR-badge-accent",
		theme: "light",
		label: "亮 .badge-accent 不得低于改前 4.71（脚本实测 4.706，按 4.70 卡）",
		fg: (t) => t.solid("--pill-accent-fg"),
		bg: (t) => t.layer("--accent-soft", "--surface-1"),
		min: 4.7,
	});

	return cases;
}

function referenceCases(dark, light) {
	const refs = [];
	for (const [themeName, t] of [
		["dark", dark],
		["light", light],
	]) {
		for (const [label, fgTok, bgTok] of [
			["--faint / --surface-3", "--faint", "--surface-3"],
			["--muted / --surface-2", "--muted", "--surface-2"],
			["--text / --surface-2", "--text", "--surface-2"],
			["--text / --bg", "--text", "--bg"],
			["--faint / --surface-1（参考）", "--faint", "--surface-1"],
			["--surface-3 / --surface-2", "--surface-3", "--surface-2"],
			["--surface-2 / --bg", "--surface-2", "--bg"],
			["--line-strong / --surface-1", "--line-strong", "--surface-1"],
		]) {
			refs.push({
				id: `ref-${themeName}-${fgTok}-${bgTok}`,
				theme: themeName,
				label: `${themeName === "dark" ? "暗" : "亮"} ${label}`,
				fg: (tt) => tt.layer(fgTok, bgTok),
				bg: (tt) => tt.solid(bgTok),
				min: 0,
			});
		}
	}
	return refs;
}

/* ---------- 主流程 ---------- */

const css = stripComments(readFileSync(TOKENS, "utf8"));
const blocks = themeBlocks(css);
if (!blocks.dark || !blocks.light) {
	console.error("✖ 无法解析 web/tokens.css 的 :root / [data-theme=\"light\"] 块");
	process.exit(2);
}
const dark = theme(declarations(blocks.dark));
const light = theme(declarations(blocks.light));
const themed = { dark, light };

const cases = buildCases(dark, light);
const rows = cases.map((c) => {
	const t = themed[c.theme];
	const fg = c.fg(t);
	const bg = c.bg(t);
	const ratio = contrast(fg, bg);
	return { ...c, ratio, ok: ratio + 1e-9 >= c.min, fgHex: hex(fg), bgHex: hex(bg) };
});

const pad = (s, n) => String(s) + " ".repeat(Math.max(0, n - String(s).length));
const themeLabel = { dark: "暗", light: "亮" };

console.log(`\nWCAG 对比度矩阵 · ${TOKENS}`);
console.log("=".repeat(104));
console.log(
	pad("ID", 26) + pad("主题", 6) + pad("配对", 46) + pad("实测", 8) + pad("阈值", 8) + "判定",
);
console.log("-".repeat(104));
for (const r of rows) {
	console.log(
		pad(r.id, 26) +
			pad(themeLabel[r.theme], 6) +
			pad(r.label, 46) +
			pad(r.ratio.toFixed(3) + ":1", 8) +
			pad(r.min.toFixed(3), 8) +
			(r.ok ? "PASS" : "FAIL"),
	);
}

if (VERBOSE) {
	const refs = referenceCases(dark, light).map((c) => {
		const t = themed[c.theme];
		const fg = c.fg(t);
		const bg = c.bg(t);
		const ratio = contrast(fg, bg);
		return { ...c, ratio, fgHex: hex(fg), bgHex: hex(bg) };
	});
	console.log("\n参考配对（不参与判定）");
	console.log("-".repeat(104));
	for (const r of refs) {
		console.log(
			pad(r.id, 30) + pad(r.label, 50) + pad(r.ratio.toFixed(3) + ":1", 10) + `(${r.fgHex} on ${r.bgHex})`,
		);
	}
}

const failed = rows.filter((r) => !r.ok);
const total = rows.length;
console.log("=".repeat(104));
console.log(`共 ${total} 项判定：PASS ${total - failed.length} / FAIL ${failed.length}`);
if (failed.length) {
	console.error(`\n✖ ${failed.length} 项对比度不达标：`);
	for (const f of failed) {
		console.error(`  - ${f.id} ${f.label} → ${f.ratio.toFixed(3)}:1 < ${f.min}`);
	}
	process.exit(1);
}
console.log("✔ 全部通过与 #370 / #371 的验收阈值\n");
