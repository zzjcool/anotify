#!/usr/bin/env node
/* SUITE: frontend — 前端渲染 + 路由守卫 + 真实数据
 *
 * 覆盖 case：
 *  A. 路由守卫：未登录访问 index/receivers/keys/security/message/connect → 自动跳 login.html
 *  B. 已登录访问 index → 不显示「演示数据」徽章（demo-badge 隐藏），渲染真实数据（哪怕空）
 *  C. login.html 是公开页：未登录正常渲染、不跳转
 *  D. 全部页面（index/login/receivers/keys/security/docs/message/connect）在 桌面1280 + 移动390 两视口：
 *     无 JS pageerror、无横向溢出、能滚动到底
 *     （/v1/* 的 401/404 是预期降级，不算失败；demo-badge 显示是后端未连接的预期行为）
 *  E. connect.html 独立页：侧栏导航直连 + active 高亮 + 6 区块 + 无裸 i18n key
 *  F. connect.html 演示态：page.route 拦截 /v1/* 模拟后端宕机，验证 demo 徽章亮 + 三灯显「—」（尚未实现）
 *  G. index.html 演示态：page.route 拦截 /v1/* 模拟后端宕机，验证演示数据状态徽章按真实 agentState 渲染
 *     （回归防护：旧协议字段 status 残留曾致 normalize 全部降级 working）
 */
import { chromium } from "playwright-core";
import * as H from "../lib/harness.mjs";

const RP = "localhost";
const PAGES = [
	"index.html",
	"login.html",
	"receivers.html",
	"keys.html",
	"security.html",
	"docs.html",
	"message.html",
	"connect.html",
];
const GUARDED = [
	"index.html",
	"receivers.html",
	"keys.html",
	"security.html",
	"message.html",
	"connect.html",
];
const VIEWPORTS = [
	{ name: "桌面1280", width: 1280, height: 800 },
	{ name: "移动390", width: 390, height: 844 },
];

let server, browser;

// 收集单页在指定视口下的渲染问题
async function checkPage(ctx, url, viewportName, viewport) {
	const page = await ctx.newPage();
	await page.setViewportSize({
		width: viewport.width,
		height: viewport.height,
	});
	const pageErrors = [];
	page.on("pageerror", (e) => pageErrors.push(String(e)));
	try {
		await page.goto(url, { waitUntil: "load", timeout: 15000 });
		// 等渲染稳定：受保护页未登录会跳转 login，所以可能停在 workspace 也可能停在 login。
		// 用 Promise.race 竞争等待两个锚点之一出现（3s 超时，足够覆盖跳转+渲染）。
		await Promise.race([
			page.waitForSelector("#sidebar", { timeout: 3000 }).catch(() => {}),
			page
				.waitForSelector("#lang-switcher-login button[aria-haspopup]", {
					timeout: 3000,
				})
				.catch(() => {}),
		]);
	} catch (e) {
		H.bad(
			`${viewportName} ${url.split("/").pop()} 加载`,
			String(e).slice(0, 80),
		);
		await page.close();
		return;
	}
	const name = url.split("/").pop();

	// JS 错误
	H.check(
		`${viewportName} ${name} 无 JS pageerror`,
		pageErrors.length === 0,
		pageErrors[0]?.slice(0, 100),
	);

	// 横向溢出（排除 pre / overflow-x-auto / 可横向滚动的代码容器内的合法内容）
	const overflowCount = await page.evaluate(() => {
		const vw = window.innerWidth;
		let n = 0;
		for (const el of document.querySelectorAll("body *")) {
			const r = el.getBoundingClientRect();
			if (
				r.right > vw + 5 &&
				!el.closest("pre") &&
				!el.closest(".overflow-x-auto") &&
				!el.closest(".code-body") && // docs.html 语法高亮代码块（overflow-x:auto，合法横向滚动）
				!el.closest("code")
			)
				n++;
		}
		return n;
	});
	H.check(
		`${viewportName} ${name} 无横向溢出`,
		overflowCount === 0,
		`${overflowCount} 个元素超出`,
	);

	// 能滚动到底（若可滚动）
	const canScrollToBottom = await page.evaluate(async () => {
		const sh = document.documentElement.scrollHeight;
		const vh = window.innerHeight;
		if (sh <= vh) return true; // 不可滚动也算通过
		window.scrollTo(0, sh);
		await new Promise((r) => setTimeout(r, 300));
		return window.scrollY >= sh - vh - 5;
	});
	H.check(`${viewportName} ${name} 可滚动到底`, canScrollToBottom);

	await page.close();
}

// 注入 SESSION cookie 到 context
async function injectSession(ctx, sessionValue, base) {
	let u;
	try {
		u = new URL(base);
	} catch {
		throw new Error("server.base 非法: " + base);
	}
	await ctx.addCookies([
		{
			name: "anotify_session",
			value: sessionValue,
			domain: u.hostname,
			path: "/",
			httpOnly: true,
			secure: u.protocol === "https:",
		},
	]);
}

async function main() {
	H.startTimer();
	console.log("=== SUITE: frontend（前端渲染 + 路由守卫 + 真实数据）===");
	server = await H.startServer({ suiteName: "frontend", rpId: RP });
	browser = await chromium.launch({
		channel: "chrome",
		headless: true,
		args: ["--no-sandbox"],
	});

	// 外部 CDN mock：见 harness.mockExternalCdn（离线门禁保障，固件为本地
	// @tailwindcss/browser v4）。所有 context 走 newCtx 建立并挂拦截。
	const newCtx = async () => {
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		return ctx;
	};

	// ---- A. 路由守卫：未登录访问受保护页 → 跳 login.html ----
	console.log("--- 路由守卫（未登录→跳登录）---");
	for (const p of GUARDED) {
		const ctx = await newCtx();
		const page = await ctx.newPage();
		try {
			await page.goto(server.base + "/" + p, {
				waitUntil: "load",
				timeout: 15000,
			});
			// 等客户端 JS 触发 401 → 跳转 login.html
			await page
				.waitForURL("**/login.html*", { timeout: 8000 })
				.catch(() => {});
			const finalUrl = page.url();
			H.check(
				`未登录访问 ${p} → 跳转到 login.html`,
				finalUrl.includes("login.html"),
				`最终 ${finalUrl}`,
			);
		} catch (e) {
			H.bad(`未登录访问 ${p} 路由守卫`, String(e).slice(0, 80));
		}
		await page.close();
		await ctx.close();
	}

	// ---- C. login.html 公开页：未登录正常渲染不跳 ----
	console.log("--- login 公开页 ---");
	{
		const ctx = await newCtx();
		const page = await ctx.newPage();
		await page.goto(server.base + "/login.html", {
			waitUntil: "load",
			timeout: 15000,
		});
		await H.waitForAppReady(page, "login");
		H.check(
			"login.html 未登录停留本页(不跳转)",
			page.url().includes("login.html"),
			`最终 ${page.url()}`,
		);
		const hasForm = await page.evaluate(
			() => !!document.body && document.body.innerText.length > 20,
		);
		H.check("login.html 正常渲染有内容", hasForm);
		await page.close();
		await ctx.close();
	}

	// ---- login UX：恢复码入口可用（非占位），#7 用户名错误可见，#8 切 tab 清错误，
	//      #9 不加载 Play CDN，#10 无认证器时不说「请允许」 ----
	console.log("--- login UX ---");
	{
		const ctx = await newCtx();
		const page = await ctx.newPage();
		const consoleNotes = [];
		const cdnHits = [];
		page.on("console", (msg) => {
			const kind = msg.type();
			if (kind === "warning" || kind === "error") consoleNotes.push(msg.text());
		});
		page.on("request", (req) => {
			if (req.url().includes("cdn.tailwindcss.com")) cdnHits.push(req.url());
		});
		await page.goto(server.base + "/login.html", {
			waitUntil: "load",
			timeout: 15000,
		});
		await H.waitForAppReady(page, "login");

		const usesCdnScript = await page.evaluate(() =>
			[...document.scripts].some((s) =>
				(s.src || "").includes("cdn.tailwindcss.com"),
			),
		);
		H.check(
			"login 不加载 Tailwind Play CDN",
			!usesCdnScript && cdnHits.length === 0,
			cdnHits[0] || "",
		);
		H.check(
			"login 控制台无 Tailwind 生产警告",
			!consoleNotes.some((m) => m.includes("should not be used in production")),
			consoleNotes.join(" | ").slice(0, 180),
		);
		H.check(
			"用户名 pattern 不是非法正则",
			!consoleNotes.some((m) => /not a valid regular expression/i.test(m)),
			consoleNotes.join(" | ").slice(0, 220),
		);
		const layoutOk = await page.evaluate(() => {
			const main = document.querySelector("main");
			return !!main && getComputedStyle(main).display === "flex";
		});
		H.check("login 本地 tailwind.css 生效（main 为 flex）", layoutOk === true);

		// 恢复码是**已上线**的功能（recovery_next 套件端到端验证了恢复码登录），
		// 所以这里断言的是「入口真的可用」，而不是早期「即将推出」的占位状态。
		const recovery = await page.evaluate(() => {
			const toggle = document.getElementById("recovery-toggle");
			const btn = document.getElementById("recovery-btn");
			const panel = document.getElementById("recovery-panel");
			const input = document.getElementById("recovery-code");
			return {
				hasToggle: !!toggle,
				toggleText: toggle ? toggle.textContent.trim() : "",
				hasBtn: !!btn,
				btnDisabled: btn ? btn.disabled : null,
				hasPanel: !!panel,
				panelHidden: panel ? panel.classList.contains("hidden") : null,
				hasInput: !!input,
			};
		});
		H.check(
			"恢复码入口是可用按钮（不是占位）",
			recovery.hasToggle &&
				recovery.hasBtn &&
				recovery.btnDisabled === false &&
				recovery.hasPanel &&
				recovery.hasInput,
			JSON.stringify(recovery),
		);
		H.check(
			"恢复码入口默认折叠，点击后展开",
			recovery.panelHidden === true,
			JSON.stringify(recovery),
		);
		await page.click("#recovery-toggle");
		const expanded = await page.evaluate(() => {
			const panel = document.getElementById("recovery-panel");
			return panel ? !panel.classList.contains("hidden") : false;
		});
		H.check("点击后恢复码面板展开", expanded === true);

		await page.click("#tab-signup");
		const hint = await page.locator("#signup-panel").innerText();
		// 文案应如实承诺「注册后展示一次性恢复码」（功能已上线），
		// 而不是早期那条「尚未开放」的占位说明。
		H.check(
			"注册说明如实告知会发放一次性恢复码",
			hint.includes("恢复码") && hint.includes("丢失"),
			hint.slice(0, 180),
		);
		const cases = [
			["x", "至少 2"],
			["has space", "只能包含"],
			["-bad-", "只能包含"],
		];
		for (const [value, snippet] of cases) {
			await page.fill("#new-username", value);
			const state = await page.evaluate(() => {
				const err = document.getElementById("username-error");
				const btn = document.getElementById("signup-btn");
				return {
					hidden: err.classList.contains("hidden"),
					text: err.textContent,
					disabled: btn.disabled,
				};
			});
			H.check(
				`非法用户名 ${JSON.stringify(value)} 有 inline 错误且按钮禁用`,
				!state.hidden && state.disabled && state.text.includes(snippet),
				JSON.stringify(state),
			);
		}
		await page.fill("#new-username", "ivy-dogfood");
		const okState = await page.evaluate(() => {
			const err = document.getElementById("username-error");
			const btn = document.getElementById("signup-btn");
			return {
				hidden: err.classList.contains("hidden"),
				disabled: btn.disabled,
			};
		});
		H.check(
			"合法用户名不报错且按钮可点",
			okState.hidden && !okState.disabled,
			JSON.stringify(okState),
		);
		const nativePattern = await page.evaluate(() => {
			const el = document.getElementById("new-username");
			const samples = ["x", "has space", "-bad-", "ab", "ivy-dogfood"];
			const out = {};
			for (const s of samples) {
				el.value = s;
				out[s] = el.validity.valid;
			}
			el.value = "ivy-dogfood";
			return out;
		});
		H.check(
			"浏览器 pattern 拒绝过短、空格、符号开头",
			nativePattern.x === false &&
				nativePattern["has space"] === false &&
				nativePattern["-bad-"] === false &&
				nativePattern.ab === true &&
				nativePattern["ivy-dogfood"] === true,
			JSON.stringify(nativePattern),
		);
		H.check(
			"调用 pattern 校验后控制台仍无非法正则",
			!consoleNotes.some((m) => /not a valid regular expression/i.test(m)),
			consoleNotes.join(" | ").slice(0, 220),
		);

		await page.evaluate(() => {
			const box = document.getElementById("status-box");
			box.classList.remove("hidden");
			box.textContent = "注册失败探针";
		});
		await page.click("#tab-login");
		const cleared = await page.evaluate(() => {
			const box = document.getElementById("status-box");
			return {
				loginShown: !document
					.getElementById("login-panel")
					.classList.contains("hidden"),
				statusHidden: box.classList.contains("hidden"),
				text: box.textContent,
			};
		});
		H.check(
			"切到登录 tab 后状态条被清空",
			cleared.loginShown && cleared.statusHidden && cleared.text === "",
			JSON.stringify(cleared),
		);

		// 无虚拟认证器：打开 WebAuthn API，但不添加认证器。页面在点击时再探测。
		// 把超时压到 1.5s，避免等服务端默认的几十秒。
		const cdp = await ctx.newCDPSession(page);
		await cdp.send("WebAuthn.enable");
		const platform = await page.evaluate(async () => {
			try {
				if (!window.PublicKeyCredential) return false;
				return await PublicKeyCredential.isUserVerifyingPlatformAuthenticatorAvailable();
			} catch {
				return false;
			}
		});
		if (platform) {
			H.check("本环境有平台认证器，跳过无提示文案断言", true);
		} else {
			await page.route("**/v1/auth/register/options", async (route) => {
				const resp = await route.fetch();
				const json = await resp.json();
				const pk = json.publicKey || json;
				pk.timeout = 1500;
				await route.fulfill({
					status: resp.status(),
					contentType: "application/json",
					body: JSON.stringify(json),
				});
			});
			await page.click("#tab-signup");
			await page.fill("#new-username", "ivy-dogfood");
			await page.click("#signup-btn");
			try {
				await page.waitForFunction(
					() => {
						const box = document.getElementById("status-box");
						return (
							box &&
							!box.classList.contains("hidden") &&
							(box.textContent || "").length > 0
						);
					},
					{ timeout: 12000 },
				);
			} catch (e) {
				H.bad("无认证器注册应出现状态文案", String(e).slice(0, 80));
			}
			const msg = await page.locator("#status-box").innerText();
			H.check(
				"无认证器时不说「请允许创建 Passkey」",
				!msg.includes("请允许") &&
					(msg.includes("没有弹出") || msg.includes("没有可用")),
				msg,
			);
			// 恢复码是真实可用的退路（recovery_next 端到端验证），
			// 所以文案**应该**指向它——这是帮助而非误导。
			H.check(
				"无认证器文案把恢复码当作可用退路",
				msg.includes("恢复码"),
				msg,
			);
			await page.click("#tab-login");
			const leaked = await page.evaluate(() => {
				const box = document.getElementById("status-box");
				return box.classList.contains("hidden");
			});
			H.check("注册失败后切到登录 tab，错误不再显示", leaked === true);
		}

		await page.close();
		await ctx.close();
	}

	// ---- B. 已登录访问 index → 不显示演示徽章（真实数据）----
	console.log("--- 已登录真实数据 ---");
	{
		// 用 devseed 建会话（无需 WebAuthn），注入 cookie
		const s = H.seed(server.dbPath, "frontend");
		// 先上报一条真实通知，验证真实数据渲染
		await H.req(server.base, "/v1/notify", {
			key: s.sendKey,
			body: { title: "前端真实数据验证", agentState: "done" },
		});

		const ctx = await newCtx();
		await injectSession(ctx, s.session, server.base);
		const page = await ctx.newPage();
		await page.goto(server.base + "/index.html", {
			waitUntil: "load",
			timeout: 15000,
		});
		// 等数据加载（真实通知行渲染完成）
		await H.waitForAppReady(page, "workspace", {
			dataAnchor: "#notif-list .notif-row",
		});

		// FileServer 会把 /index.html 规范重定向到 /，所以这里验证「未跳去登录页」而非死磕文件名
		H.check(
			"已登录访问 index 未跳登录页",
			!page.url().includes("login.html"),
			`最终 ${page.url()}`,
		);

		// demo-badge 应隐藏（有真实数据，非演示模式）
		const badgeHidden = await page.evaluate(() => {
			const b = document.getElementById("demo-badge");
			if (!b) return "missing";
			return b.classList.contains("hidden");
		});
		H.check(
			"已登录 index 不显示「演示数据」徽章",
			badgeHidden === true,
			`badgeHidden=${badgeHidden}`,
		);

		// 真实通知应出现在列表中
		const hasReal = await page.evaluate(() =>
			document.body.innerText.includes("前端真实数据验证"),
		);
		H.check("已登录 index 显示真实通知数据", hasReal);

		await page.close();
		await ctx.close();
	}

	// ---- G. index.html 演示态：后端宕机 → 演示数据状态正确（回归：旧字段 status 曾致全部降级 working）----
	console.log("--- index 演示态（后端宕机）---");
	{
		const s = H.seed(server.dbPath, "demo_test");
		const ctx = await newCtx();
		await injectSession(ctx, s.session, server.base);
		// 拦截 /v1/* 模拟后端宕机：api() 网络错误（非 401）→ 不跳登录，回退演示数据
		await ctx.route("**/v1/**", (route) => route.abort());
		const page = await ctx.newPage();
		await page.goto(server.base + "/index.html", {
			waitUntil: "load",
			timeout: 15000,
		});
		await H.waitForAppReady(page, "workspace", {
			dataAnchor: "#notif-list .notif-row",
		});

		// 演示徽章亮
		const demoBadgeVisible = await page.evaluate(() => {
			const b = document.getElementById("demo-badge");
			return !!b && !b.classList.contains("hidden");
		});
		H.check("index 演示态显示「演示数据」徽章", demoBadgeVisible);

		// 演示数据 8 条：done×5 / error×2 / interrupted×1 / working×0
		// （旧残留：demo 数据用旧协议字段 status，normalize 只读 agentState，全部降级 working）
		const badgeCounts = await page.evaluate(() => ({
			success: document.querySelectorAll(
				"#notif-list .notif-row .status-badge.status-success",
			).length,
			error: document.querySelectorAll(
				"#notif-list .notif-row .status-badge.status-error",
			).length,
			warn: document.querySelectorAll(
				"#notif-list .notif-row .status-badge.status-warn",
			).length,
			info: document.querySelectorAll(
				"#notif-list .notif-row .status-badge.status-info",
			).length,
		}));
		H.check(
			"演示态徽章 done×5",
			badgeCounts.success === 5,
			JSON.stringify(badgeCounts),
		);
		H.check(
			"演示态徽章 error×2",
			badgeCounts.error === 2,
			JSON.stringify(badgeCounts),
		);
		H.check(
			"演示态徽章 interrupted×1（warn）",
			badgeCounts.warn === 1,
			JSON.stringify(badgeCounts),
		);
		H.check(
			"演示态无 working 降级（info×0）",
			badgeCounts.info === 0,
			JSON.stringify(badgeCounts),
		);

		// 汇总行 okCnt 按真实状态统计（旧 bug：全部 working → 0 条成功）
		const summaryOk = await page.evaluate(() => {
			const m = document
				.getElementById("summary-line")
				?.textContent.match(/(\d+)/g);
			return m ? m.map(Number) : null;
		});
		H.check(
			"演示态汇总行「8 条通知，5 条成功」",
			!!summaryOk && summaryOk[0] === 8 && summaryOk[1] === 5,
			JSON.stringify(summaryOk),
		);

		await page.close();
		await ctx.close();
	}

	// ---- E. connect.html 独立页：侧栏导航 + active 高亮 + 6 区块 ----
	console.log("--- connect.html 独立页导航 ---");
	{
		const s = H.seed(server.dbPath, "connect_test");
		const ctx = await newCtx();
		await injectSession(ctx, s.session, server.base);
		const page = await ctx.newPage();
		await page.goto(server.base + "/connect.html", {
			waitUntil: "load",
			timeout: 15000,
		});
		await H.waitForAppReady(page, "workspace");

		// E-1: 侧栏「接入 Agent」链接 href = connect.html（非锚点）
		const agentNavHref = await page.evaluate(() => {
			const links = document.querySelectorAll("#sidebar .side-link");
			for (const a of links) {
				if (
					a.textContent.includes("Agent") ||
					a.textContent.includes("接入") ||
					a.textContent.includes("連携") ||
					a.textContent.includes("Conectar")
				)
					return a.getAttribute("href");
			}
			return null;
		});
		H.check(
			"侧栏接入 Agent 链接指向 connect.html",
			agentNavHref === "connect.html",
			agentNavHref,
		);

		// E-2: connect.html 侧栏 active 高亮 = agent 项
		const activeNav = await page.evaluate(() => {
			const active = document.querySelector("#sidebar .side-link.active");
			if (!active) return null;
			return active.getAttribute("href");
		});
		H.check(
			"connect.html 侧栏 active 项指向 connect.html",
			activeNav === "connect.html",
			activeNav,
		);

		// E-3: 插件目录 — 4 个插件卡片（pi 可安装 + 3 个占位），且含 pi 安装命令
		const pluginInfo = await page.evaluate(() => {
			const cards = document.querySelectorAll(".grid .card");
			const txt = document.body.innerText;
			return {
				count: cards.length,
				hasPiInstall: txt.includes("pi install npm:pi-anotify"),
				comingSoon: (
					txt.match(/即将支持|Coming soon|近日対応|Próximamente/g) || []
				).length,
			};
		});
		H.check(
			"connect.html 插件目录 4 个卡片",
			pluginInfo.count === 4,
			`cards=${pluginInfo.count}`,
		);
		H.check("connect.html 含 pi 安装命令", pluginInfo.hasPiInstall);
		H.check(
			"connect.html 3 个占位插件标「即将支持」",
			pluginInfo.comingSoon === 3,
			`comingSoon=${pluginInfo.comingSoon}`,
		);

		// E-4: 无裸 i18n key 泄漏（页面文本不含 connect.* 或 docs.* 键）
		const hasRawKey = await page.evaluate(() => {
			const txt = document.body.innerText;
			return /\bconnect\.\w+\b/.test(txt) || /\bcommon\.nav\.\w+\b/.test(txt);
		});
		H.check(
			"connect.html 无裸 i18n key 泄漏",
			!hasRawKey,
			hasRawKey ? "发现裸 key" : "",
		);

		await page.close();
		await ctx.close();
	}

	// ---- D. 全部页面 × 2 视口渲染检查（未登录态，验证纯渲染；受保护页会跳 login，跳后渲染 login 也算无 JS 错误）----
	console.log("--- 全页面 × 双视口渲染 ---");
	for (const vp of VIEWPORTS) {
		const ctx = await newCtx();
		for (const p of PAGES) {
			await checkPage(ctx, server.base + "/" + p, vp.name, vp);
		}
		await ctx.close();
	}

	const passed = H.summary("frontend");
	await browser.close();
	server.stop();
	process.exit(passed ? 0 : 1);
}
main().catch(async (e) => {
	console.error(e);
	try {
		await browser?.close();
		server?.stop();
	} catch {
		/* ignore */
	}
	process.exit(1);
});
