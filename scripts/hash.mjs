#!/usr/bin/env node
/* Anotify 指纹脚本（content-hash + 引用改写 + manifest）
 *
 * 作用：在不引入重型构建链的前提下，给纯静态前端资源加内容哈希指纹，
 *       使 CDN 可对哈希文件 immutable 长缓存。
 *
 * 用法：node scripts/hash.mjs <srcDir> <outDir>
 *   例：node scripts/hash.mjs web dist
 *
 * 行为：
 *   1. 扫描 <srcDir> 下的 .js/.css/.png/.svg/.woff2 等资源（不含 .html）
 *   2. 计算内容 hash，重命名为 name.<hash8>.ext 复制到 <outDir>
 *   3. 复制 .html 到 <outDir>，并把其中对资源的引用改写为哈希文件名
 *   4. 生成 <outDir>/manifest.json（原始名 → 哈希名 映射）
 */
import { createHash } from "node:crypto";
import { promises as fs } from "node:fs";
import path from "node:path";

const [srcDir, outDir] = process.argv.slice(2);
if (!srcDir || !outDir) {
	console.error("用法: node scripts/hash.mjs <srcDir> <outDir>");
	process.exit(1);
}

const FINGERPRINT_EXT = new Set([
	".js", ".css", ".png", ".jpg", ".jpeg", ".svg", ".ico",
	".woff", ".woff2", ".ttf", ".webmanifest",
]);
const HTML_EXT = new Set([".html"]);
const SKIP = new Set(["manifest.json"]);

async function* walk(dir) {
	for (const e of await fs.readdir(dir, { withFileTypes: true })) {
		const full = path.join(dir, e.name);
		if (e.isDirectory()) yield* walk(full);
		else yield full;
	}
}

function hash8(buf) {
	return createHash("sha256").update(buf).digest("hex").slice(0, 8);
}

async function main() {
	const src = path.resolve(srcDir);
	const out = path.resolve(outDir);
	await fs.rm(out, { recursive: true, force: true });
	await fs.mkdir(out, { recursive: true });

	const manifest = {}; // 相对路径(原) → 相对路径(哈希)
	const htmlFiles = [];

	for await (const file of walk(src)) {
		const rel = path.relative(src, file);
		const ext = path.extname(file).toLowerCase();
		const base = path.basename(file);
		if (SKIP.has(base)) continue;

		if (FINGERPRINT_EXT.has(ext)) {
			const buf = await fs.readFile(file);
			const h = hash8(buf);
			const dir = path.dirname(rel);
			const stem = path.basename(rel, ext);
			const hashedRel = path.join(dir, `${stem}.${h}${ext}`);
			const target = path.join(out, hashedRel);
			await fs.mkdir(path.dirname(target), { recursive: true });
			await fs.writeFile(target, buf);
			manifest[rel.split(path.sep).join("/")] = hashedRel.split(path.sep).join("/");
		} else if (HTML_EXT.has(ext)) {
			htmlFiles.push({ file, rel });
		} else {
			// 其它文件原样复制
			const target = path.join(out, rel);
			await fs.mkdir(path.dirname(target), { recursive: true });
			await fs.copyFile(file, target);
		}
	}

	// 改写 HTML 中的引用
	for (const { file, rel } of htmlFiles) {
		let html = await fs.readFile(file, "utf8");
		for (const [orig, hashed] of Object.entries(manifest)) {
			// 匹配 src/href="...orig"（支持相对路径），替换为哈希名
			const esc = orig.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
			html = html.replace(new RegExp(`(src|href)=("')([^"']*?)${esc}\\2`, "g"),
				(_, attr, q, prefix) => `${attr}=${q}${prefix}${hashed}${q}`);
		}
		const target = path.join(out, rel);
		await fs.mkdir(path.dirname(target), { recursive: true });
		await fs.writeFile(target, html);
	}

	await fs.writeFile(path.join(out, "manifest.json"), JSON.stringify(manifest, null, 2));
	console.log(`✅ 指纹完成：${Object.keys(manifest).length} 个资源，${htmlFiles.length} 个 HTML → ${out}`);
}

main().catch((e) => { console.error(e); process.exit(1); });
