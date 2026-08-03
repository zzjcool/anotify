#!/usr/bin/env node
/* Anotify 图标生成器
 *
 * 用本地 Sacramento 手写体渲染 "An"，经 headless Chrome 栅格化为各尺寸 PNG，
 * 与 Dashboard 左上角 logo()（partials.js）保持视觉一致：黑底 + 白 "An" + 右上角红点。
 *
 * 用法：node scripts/gen-icons.mjs
 * 产物：web/assets/ 下 apple-touch-icon.png / icon-192.png / icon-512.png / icon.png
 *       及 favicon.svg（用 Sacramento 渲染的 path 替换原手绘 path）
 *
 * 依赖：系统需安装 Google Chrome（headless 截图）。
 * 注意：apple-touch-icon.png 为满版方形黑底（不预裁圆角），圆角交给 iOS squircle 遮罩。
 */

import { execFileSync } from "node:child_process";
import { promises as fs } from "node:fs";
import path from "node:path";
import os from "node:os";

const ROOT = path.resolve(
	path.dirname(new URL(import.meta.url).pathname),
	"..",
);
const FONT = path.join(ROOT, "web/fonts/sacramento.woff2");
const OUT_DIR = path.join(ROOT, "web/assets");
const CHROME =
	process.env.CHROME_PATH ||
	"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";

/* 构图参数（相对 size 的比例，对齐 partials.js logo()） */
const RATIO = {
	fontSize: 0.39, // "An" 字号 / size
	radius: 0.1875, // 圆角 / size（rounded-lg ≈ 18.75%）
	dotD: 0.094, // 红点直径 / size（dashboard 12px/36px≈0.33→ 视觉按 0.094 更稳）
	dotOffset: 0.07, // 红点距右上 / size
	padTop: 0.06, // 视觉垂直居中微调 / size
};

/* 各产物：尺寸 + 是否满版方形（iOS 不预裁圆角） */
const TARGETS = [
	{ name: "apple-touch-icon.png", size: 180, square: true },
	{ name: "icon-192.png", size: 192, square: false },
	{ name: "icon-512.png", size: 512, square: false },
	{ name: "icon.png", size: 192, square: false }, // sw.js 通知 icon 用
];

function htmlFor(size, square) {
	const r = RATIO;
	const radius = square ? 0 : Math.round(size * r.radius);
	return `<!doctype html>
<html><head><meta charset="utf-8">
<style>
@font-face { font-family:"Sacramento"; src:url("file://${FONT}") format("woff2"); }
* { margin:0; padding:0; box-sizing:border-box; }
body { background: transparent; }
.icon {
  width:${size}px; height:${size}px;
  background:#000; border-radius:${radius}px;
  display:flex; align-items:center; justify-content:center;
  position:relative; font-family:"Sacramento",cursive; color:#fff;
  font-size:${Math.round(size * r.fontSize)}px; line-height:1;
  padding-top:${Math.round(size * r.padTop)}px;
}
.dot {
  position:absolute; top:${Math.round(size * r.dotOffset)}px; right:${Math.round(size * r.dotOffset)}px;
  width:${Math.round(size * r.dotD)}px; height:${Math.round(size * r.dotD)}px;
  border-radius:50%; background:#ef4444;
}
</style></head>
<body><div class="icon">An<span class="dot"></span></div>
<script>document.fonts.ready.then(()=>{document.title="READY";});</script>
</body></html>`;
}

async function render(tmpHtml, size, outPng) {
	await fs.writeFile(tmpHtml, htmlFor(size, outPng.square));
	execFileSync(
		CHROME,
		[
			"--headless",
			"--disable-gpu",
			"--hide-scrollbars",
			`--window-size=${size},${size}`,
			`--screenshot=${outPng.path}`,
			"--virtual-time-budget=2000",
			`file://${tmpHtml}`,
		],
		{ stdio: ["ignore", "pipe", "pipe"] },
	);
}

async function main() {
	await fs.access(FONT);
	await fs.mkdir(OUT_DIR, { recursive: true });
	const tmp = path.join(os.tmpdir(), `anotify-icon-${Date.now()}.html`);

	for (const t of TARGETS) {
		const outPng = { path: path.join(OUT_DIR, t.name), square: t.square };
		await render(tmp, t.size, outPng);
		const st = await fs.stat(outPng.path);
		console.log(
			`✅ ${t.name}  ${t.size}x${t.size}  ${st.size}B  ${t.square ? "(满版方形)" : "(圆角)"}`,
		);
	}
	await fs.rm(tmp, { force: true });
	console.log(
		"\n提示：favicon.svg 仍为原手绘 path；如需统一为 Sacramento 字体，可后续用字体工具把 'An' 转 path 替换。",
	);
}

main().catch((e) => {
	console.error("生成失败:", e.message);
	process.exit(1);
});
