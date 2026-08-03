#!/usr/bin/env node
/* Anotify <head> 图标声明统一注入
 *
 * 所有页面在 <title> 后统一插入标准图标块（幂等）：
 *   manifest / theme-color / apple-touch-icon / favicon.svg
 * 并移除页面原有的重复 manifest 与 favicon 声明，避免重复。
 *
 * 用法：node scripts/inject-head.mjs
 * 之后请重新跑 `make build`（hash.mjs 会指纹化引用）。
 */

import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

function scriptDir() {
	try {
		return path.dirname(fileURLToPath(import.meta.url));
	} catch {
		return process.cwd();
	}
}

const ROOT = path.resolve(scriptDir(), "..");
const WEB = path.join(ROOT, "web");

const ICON_BLOCK = [
	'<link rel="manifest" href="manifest.webmanifest" />',
	'<meta name="theme-color" content="#050508" />',
	'<link rel="apple-touch-icon" href="assets/apple-touch-icon.png" />',
	'<link rel="icon" type="image/svg+xml" href="assets/favicon.svg" />',
].join("\n    ");

async function processFile(file) {
	let html = await fs.readFile(file, "utf8");
	const orig = html;

	// 1) 移除已有的 manifest / theme-color / apple-touch-icon / favicon.svg 行（含前导空白与换行）
	html = html.replace(/^\s*<link rel="manifest"[^>]*\/>\s*\n/gm, "");
	html = html.replace(/^\s*<meta name="theme-color"[^>]*\/>\s*\n/gm, "");
	html = html.replace(/^\s*<link rel="apple-touch-icon"[^>]*\/>\s*\n/gm, "");
	html = html.replace(
		/^\s*<link rel="icon"[^>]*favicon\.svg[^>]*\/>\s*\n/gm,
		"",
	);

	// 2) 在 <title>...</title> 行后插入标准图标块
	const titleRe = /(\s*<title>.*?<\/title>\n)/;
	if (!titleRe.test(html)) {
		console.warn(`⚠️  ${path.basename(file)} 未找到 <title>，跳过`);
		return false;
	}
	html = html.replace(titleRe, `$1    ${ICON_BLOCK}\n`);

	if (html !== orig) {
		await fs.writeFile(file, html);
		return true;
	}
	return false;
}

async function main() {
	const entries = await fs.readdir(WEB);
	const pages = entries.filter((f) => f.endsWith(".html"));
	let changed = 0;
	for (const p of pages) {
		const full = path.join(WEB, p);
		if (await processFile(full)) {
			console.log(`✅ ${p}`);
			changed++;
		} else {
			console.log(`—  ${p}（无变化）`);
		}
	}
	console.log(`\n完成：${changed}/${pages.length} 个页面已统一图标声明。`);
}

main().catch((e) => {
	console.error(e);
	process.exit(1);
});
