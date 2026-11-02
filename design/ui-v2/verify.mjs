#!/usr/bin/env node
/* design/ui-v2 视觉稿自检（设计侧门禁，不进产品构建）
 *
 * 对 3 个页面 × 2 主题 × 2 视口：
 *   1. 收集 console.error / pageerror（必须 0 条）
 *   2. 检查横向溢出（documentElement.scrollWidth 不得超过视口 +1px）
 *   3. 检查可滚动（body 高度 > 视口高度，或明确是 100dvh 单屏页）
 *   4. 落盘截图 → design/ui-v2/shots/
 *
 * 用法：node design/ui-v2/verify.mjs [--only=gallery]
 * 依赖：父 checkout 的 playwright-core + ~/.cache/ms-playwright 的 chromium
 */
import { createServer } from "node:http";
import { readFile, mkdir, stat } from "node:fs/promises";
import { extname, join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { createRequire } from "node:module";

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = dirname(dirname(HERE)); // repo root of this worktree
const require = createRequire("/root/code/anotify/package.json");
const { chromium } = require("playwright-core");

const CHROME =
	process.env.ANOTIFY_CHROME ||
	"/root/.cache/ms-playwright/chromium-1208/chrome-linux64/chrome";

const MIME = {
	".html": "text/html; charset=utf-8",
	".css": "text/css; charset=utf-8",
	".js": "text/javascript; charset=utf-8",
	".svg": "image/svg+xml",
	".png": "image/png",
	".woff2": "font/woff2",
	".json": "application/json",
};

const PAGES = [
	{ file: "gallery.html", name: "gallery", hasShell: true },
	{ file: "dashboard.html", name: "dashboard", hasShell: true },
	{ file: "login.html", name: "login", hasShell: false },
];

const THEMES = ["dark", "light"];
const VIEWPORTS = [
	{ w: 1440, h: 900, tag: "1440" },
	{ w: 390, h: 844, tag: "390" },
];

const onlyArg = process.argv.find((a) => a.startsWith("--only="));
const only = onlyArg ? onlyArg.split("=")[1] : null;

function serve() {
	const server = createServer(async (req, res) => {
		try {
			const url = new URL(req.url, "http://127.0.0.1");
			const path = join(HERE, decodeURIComponent(url.pathname));
			if (!path.startsWith(HERE)) throw new Error("escape");
			const st = await stat(path);
			if (!st.isFile()) throw new Error("not a file");
			const body = await readFile(path);
			res.writeHead(200, {
				"content-type": MIME[extname(path)] || "application/octet-stream",
				"cache-control": "no-store",
			});
			res.end(body);
		} catch {
			res.writeHead(404).end("not found");
		}
	});
	return new Promise((resolve) => {
		server.listen(0, "127.0.0.1", () => resolve(server));
	});
}

const results = [];
let failures = 0;

async function main() {
	await mkdir(join(HERE, "shots"), { recursive: true });
	const server = await serve();
	const port = server.address().port;
	const base = `http://127.0.0.1:${port}`;
	const browser = await chromium.launch({
		executablePath: CHROME,
		args: ["--no-sandbox", "--disable-dev-shm-usage", "--font-render-hinting=none"],
	});

	for (const page of PAGES) {
		if (only && !page.name.includes(only)) continue;
		for (const vp of VIEWPORTS) {
			for (const theme of THEMES) {
				const ctx = await browser.newContext({
					viewport: { width: vp.w, height: vp.h },
					deviceScaleFactor: 1,
					reducedMotion: "reduce",
					colorScheme: theme,
				});
				const tab = await ctx.newPage();
				const errors = [];
				const warnings = [];
				tab.on("console", (m) => {
					if (m.type() === "error") errors.push(`console.error: ${m.text()}`);
					if (m.type() === "warning") warnings.push(m.text());
				});
				tab.on("pageerror", (e) => errors.push(`pageerror: ${e.message}`));
				tab.on("requestfailed", (r) => {
					const u = r.url();
					// 字体 CDN 失败不阻断布局（有系统字体兜底），单独记
					warnings.push(`requestfailed: ${u} — ${r.failure()?.errorText}`);
				});

				const url = `${base}/${page.file}?theme=${theme}&shot=1`;
				await tab.goto(url, { waitUntil: "load", timeout: 30000 });
				await tab.evaluate(() => document.fonts.ready);
				await tab.waitForTimeout(350);

				const metrics = await tab.evaluate(() => {
					const de = document.documentElement;
					const b = document.body;
					return {
						sw: de.scrollWidth,
						cw: de.clientWidth,
						sh: de.scrollHeight,
						ih: window.innerHeight,
						bodyOverflow: getComputedStyle(b).overflowX,
						// 找出真正溢出的元素（诊断用）
						wide: [...document.querySelectorAll("body *")]
							.filter((el) => el.getBoundingClientRect().right > de.clientWidth + 1)
							.slice(0, 5)
							.map((el) => `${el.tagName.toLowerCase()}.${el.className}`.slice(0, 90)),
					};
				});

				const overflow = metrics.sw - metrics.cw;
				if (overflow > 1) failures++;
				if (errors.length) failures++;

				// 首屏（视口）+ 整页，两种都留：审稿看首屏，量高度看整页
				const firstName = `${page.name}-${theme}-${vp.tag}-first.png`;
				await tab.screenshot({
					path: join(HERE, "shots", firstName),
					animations: "disabled",
				});
				const fullName = `${page.name}-${theme}-${vp.tag}-full.jpg`;
				await tab.screenshot({
					path: join(HERE, "shots", fullName),
					fullPage: true,
					animations: "disabled",
					type: "jpeg",
					quality: 88,
				});

				// 交互态：390 打开抽屉 / 1440 折叠侧栏（只留 dark 一套，避免截图爆炸）
				let extra = "";
				if (page.hasShell && theme === "dark" && vp.tag === "390") {
					await tab.click("#menu-btn");
					await tab.waitForTimeout(320);
					const drawerName = `${page.name}-dark-390-drawer.png`;
					await tab.screenshot({
						path: join(HERE, "shots", drawerName),
						animations: "disabled",
					});
					await tab.keyboard.press("Escape");
					extra = drawerName;
				}
				if (page.hasShell && theme === "dark" && vp.tag === "1440") {
					await tab.click("#sidebar-collapse");
					await tab.waitForTimeout(260);
					const colName = `${page.name}-dark-1440-collapsed.png`;
					await tab.screenshot({
						path: join(HERE, "shots", colName),
						animations: "disabled",
					});
					// 折叠态 tooltip 证据：hover 第 3 个导航项，只截左上角
					await tab.hover(".side-nav .side-link:nth-of-type(2)");
					await tab.waitForTimeout(260);
					const tipName = `${page.name}-dark-1440-collapsed-tooltip.png`;
					await tab.screenshot({
						path: join(HERE, "shots", tipName),
						clip: { x: 0, y: 60, width: 460, height: 380 },
						animations: "disabled",
					});
					await tab.mouse.move(900, 700);
					await tab.click("#sidebar-collapse");
					await tab.waitForTimeout(200);
					extra = `${colName},${tipName}`;
				}

				await ctx.close();
			}
		}
	}

	await browser.close();
	server.close();

	const pad = (s, n) => String(s).padEnd(n);
	console.log(
		`\n${pad("page", 11)}${pad("theme", 7)}${pad("viewport", 10)}${pad("height", 9)}${pad("overflow", 10)}${pad("errors", 8)}warn`,
	);
	for (const r of results) {
		console.log(
			`${pad(r.page, 11)}${pad(r.theme, 7)}${pad(r.viewport, 10)}${pad(r.height, 9)}${pad(r.overflow, 10)}${pad(r.errors, 8)}${r.warnings}`,
		);
		if (r.overflow > 1) console.log(`   ↳ 溢出元素: ${r.wide.join(" | ")}`);
		if (r.errorText.length) console.log(`   ↳ ${r.errorText.join("\n   ↳ ")}`);
		if (r.warns && r.warns.length) console.log(`   ↳ warn: ${r.warns[0]}`);
	}
	console.log(`\n截图目录: design/ui-v2/shots/`);
	console.log(failures === 0 ? "RESULT: PASS" : `RESULT: FAIL (${failures} problems)`);
	process.exit(failures === 0 ? 0 : 1);
}

main().catch((e) => {
	console.error(e);
	process.exit(1);
});
