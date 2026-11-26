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
 *   4. 清掉 <outDir> 里本轮没有产出的文件（只删多出来的，不整目录删）—— #649：生成物
 *      只写不删时，「源页面已删、产物还在」会静默活下来并被 go:embed 进二进制
 *   5. 写 <outDir>/manifest.json（原始名 → 哈希名 映射）
 */
import { createHash } from "node:crypto";
import { promises as fs } from "node:fs";
import path from "node:path";

import { runsAsMain } from "./lib/direct-run.mjs";

const FINGERPRINT_EXT = new Set([
	".js",
	".css",
	".png",
	".jpg",
	".jpeg",
	".svg",
	".ico",
	".woff",
	".woff2",
	".ttf",
	".webmanifest",
]);
const HTML_EXT = new Set([".html"]);
// 不指纹的文件：manifest.json 是自身产物；manifest.webmanifest 被 HTML 固定引用
// （PWA 清单路径写死，改指纹名 Android 图标会 404）。
const NO_FINGERPRINT = new Set([
	"manifest.json",
	"manifest.webmanifest",
]);

// mock.js 是 make fe-preview 用的 API 拦截层（假数据），进了产物就多了个"假数据模式"入口。
const DEV_ONLY = new Set([
	"mock.js",
]);

export const MANIFEST_NAME = "manifest.json";

/** 由其它阶段写进 <outDir> 的产物：本脚本既不认识、也不许删。 */
export const OWNED_BY_OTHER_STAGES = new Set([".source-stamp"]);

/** 源文件在 outDir 里会变成什么样。writer 与 scripts/dist-stamp.mjs 的期望集共用这份判据。 */
export function classify(rel) {
	const base = path.basename(rel);
	if (DEV_ONLY.has(base)) return "drop";
	if (NO_FINGERPRINT.has(base)) return "copy";
	if (FINGERPRINT_EXT.has(path.extname(rel).toLowerCase())) return "fingerprint";
	return "copy";
}

/** 纯函数：srcRels + manifest ⇒ outDir 应当含的路径；unresolved = 该指纹却无 manifest 条目。 */
export function expectedDistPaths({ srcRels, manifest }) {
	const files = new Set([MANIFEST_NAME, ...OWNED_BY_OTHER_STAGES]);
	const unresolved = [];
	for (const rel of srcRels) {
		const p = rel.split(path.sep).join("/");
		const kind = classify(p);
		if (kind === "drop") continue;
		if (kind === "fingerprint") {
			const hashed = manifest[p];
			if (hashed) files.add(hashed);
			else unresolved.push(p);
			continue;
		}
		files.add(p);
	}
	return { files: [...files].sort(), unresolved: unresolved.sort() };
}

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

/** 删掉 outDir 里 produced 之外的残留（自底向上，顺带收掉空目录），返回删掉的路径。 */
async function pruneStaleOutputs(out, produced) {
	const removed = [];
	async function rec(dir, rel) {
		for (const e of await fs.readdir(dir, { withFileTypes: true })) {
			const childRel = rel ? `${rel}/${e.name}` : e.name;
			const full = path.join(dir, e.name);
			if (e.isDirectory()) {
				await rec(full, childRel);
				if ((await fs.readdir(full)).length === 0) {
					await fs.rmdir(full);
					removed.push(`${childRel}/`);
				}
				continue;
			}
			if (produced.has(childRel)) continue;
			await fs.rm(full, { force: true });
			removed.push(childRel);
		}
	}
	await rec(out, "");
	return removed.sort();
}

export async function buildInto(srcDir, outDir) {
	const src = path.resolve(srcDir);
	const out = path.resolve(outDir);
	if (src === out) throw new Error(`hash.mjs: 源与目标是同一个目录（${src}），prune 会删掉源文件`);
	await fs.mkdir(out, { recursive: true });

	const manifest = {}; // 相对路径(原) → 相对路径(哈希)
	const htmlFiles = [];
	const produced = new Set([MANIFEST_NAME]);

	for await (const file of walk(src)) {
		const rel = path.relative(src, file);
		const relPosix = rel.split(path.sep).join("/");
		const kind = classify(relPosix);
		if (kind === "drop") continue;
		if (kind === "fingerprint") {
			const buf = await fs.readFile(file);
			const h = hash8(buf);
			const ext = path.extname(rel).toLowerCase();
			const stem = path.basename(rel, ext);
			const hashedRel = path
				.join(path.dirname(rel), `${stem}.${h}${ext}`)
				.split(path.sep)
				.join("/");
			const target = path.join(out, hashedRel);
			await fs.mkdir(path.dirname(target), { recursive: true });
			await fs.writeFile(target, buf);
			manifest[relPosix] = hashedRel;
			produced.add(hashedRel);
			continue;
		}
		produced.add(relPosix);
		const target = path.join(out, rel);
		await fs.mkdir(path.dirname(target), { recursive: true });
		await fs.copyFile(file, target);
		if (HTML_EXT.has(path.extname(rel).toLowerCase())) htmlFiles.push({ file, rel });
	}

	// 改写 HTML 中的引用：捕获 src/href 的完整值，按 manifest 精确映射为哈希名。
	// （之前的「前缀+orig+\\2 同引号」正则在值紧邻引号时失配，如 href="ui.css" 仅 8 字符）
	for (const { file, rel } of htmlFiles) {
		let html = await fs.readFile(file, "utf8");
		html = html.replace(
			/(src|href)=("|')([^"']+)\2/g,
			(match, attr, q, value) => {
				// 精确匹配原始相对路径（manifest key 无前导 /）
				if (manifest[value]) return `${attr}=${q}${manifest[value]}${q}`;
				// 根绝对路径（/assets/x.css）：strip 前导 / 查 manifest，保留 / 输出
				if (value.startsWith("/")) {
					const hashed = manifest[value.slice(1)];
					if (hashed) return `${attr}=${q}/${hashed}${q}`;
				}
				return match;
			},
		);
		const target = path.join(out, rel);
		await fs.mkdir(path.dirname(target), { recursive: true });
		await fs.writeFile(target, html);
	}

	// 改写 CSS 内的 url(...) 引用（如 fonts.css 引用同目录 .woff2）。
	// CSS 也被指纹，但其内容的引用要指向哈希后的字体文件名。
	for (const [orig, hashed] of Object.entries(manifest)) {
		if (!orig.toLowerCase().endsWith(".css")) continue;
		const cssPath = path.join(out, hashed);
		let css;
		try {
			css = await fs.readFile(cssPath, "utf8");
		} catch {
			continue;
		}
		const cssDir = path.dirname(orig);
		css = css.replace(/url\(("|')?([^"')]+)\1?\)/g, (match, q, value) => {
			// 该 CSS 同目录下的相对引用 → 拼出 manifest 键再查哈希名
			if (/^(https?:|data:|\/)/.test(value)) return match;
			const key = path
				.normalize(path.join(cssDir, value))
				.split(path.sep)
				.join("/");
			const hashedVal = manifest[key];
			if (!hashedVal) return match;
			// 引用转为「同目录哈希文件名」（CSS 与字体同目录时直接用 basename）
			const base = path.basename(hashedVal);
			return `url(${q || ""}${base}${q || ""})`;
		});
		await fs.writeFile(cssPath, css);
	}

	// 改写 dist 内 manifest.webmanifest 的 icons[].src 为哈希名。
	// manifest.webmanifest 自身不指纹（被 HTML 固定引用），但其引用的图标会被指纹，
	// 不改写会导致 Android PWA 图标 404。
	const webmanifestPath = path.join(out, "manifest.webmanifest");
	try {
		const wm = JSON.parse(await fs.readFile(webmanifestPath, "utf8"));
		if (Array.isArray(wm.icons)) {
			for (const icon of wm.icons) {
				const hashed = manifest[icon.src];
				if (hashed) icon.src = hashed;
			}
			await fs.writeFile(webmanifestPath, JSON.stringify(wm, null, 2));
		}
	} catch {
		/* 无 manifest.webmanifest 时忽略 */
	}


	const stale = await pruneStaleOutputs(
		out,
		new Set([...produced, ...OWNED_BY_OTHER_STAGES]),
	);
	for (const rel of stale) console.log(`   · 清掉无源残留：${rel}`);

	await fs.writeFile(
		path.join(out, MANIFEST_NAME),
		JSON.stringify(manifest, null, 2),
	);
	console.log(
		`✅ 指纹完成：${Object.keys(manifest).length} 个资源，${htmlFiles.length} 个 HTML → ${out}`,
	);
}

if (runsAsMain(import.meta.url)) {
	const [srcDir, outDir] = process.argv.slice(2);
	if (!srcDir || !outDir) {
		console.error("用法: node scripts/hash.mjs <srcDir> <outDir>");
		process.exit(1);
	}
	buildInto(srcDir, outDir).catch((e) => {
		console.error(e);
		process.exit(1);
	});
}
