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
import * as N from "../lib/nodes.mjs";
import { DatabaseSync } from "node:sqlite";

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
	// 事件中心页面（#79）
	"events.html",
	"sources.html",
	"capabilities.html",
	"edges.html",
	"nodes.html",
];
const GUARDED = [
	"index.html",
	"receivers.html",
	"keys.html",
	"security.html",
	"message.html",
	"connect.html",
	// 事件中心页面同样需要登录（未登录 → login.html）
	"events.html",
	"sources.html",
	"capabilities.html",
	"edges.html",
	"nodes.html",
];
const VIEWPORTS = [
	{ name: "桌面1280", width: 1280, height: 800 },
	{ name: "移动390", width: 390, height: 844 },
];

let server, browser;
// 真实数据用例需要上线节点（节点掉线后要 close，避免句柄泄漏）
let frontendNode = null;

function primaryOrgOf(dbPath, uid) {
	const db = new DatabaseSync(dbPath);
	try {
		const row = db
			.prepare(
				"SELECT org_id FROM org_members WHERE user_id = ? ORDER BY rowid LIMIT 1",
			)
			.get(uid);
		return row ? row.org_id : null;
	} finally {
		db.close();
	}
}

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

		// 造一条真实任务，验证真实数据渲染。
		// #80 后 index 读 /v1/tasks（旧 /v1/notify 已删），所以要先建图 + 上线节点，
		// 否则事件只会留在 pending/no_node，列表读不到。
		const capA = await H.req(server.base, "/v1/capabilities", {
			session: s.session,
			method: "POST",
			body: { name: "frontend.src", outputs: ["go"], paramsSchema: { type: "object" } },
		});
		const capB = await H.req(server.base, "/v1/capabilities", {
			session: s.session,
			method: "POST",
			body: { name: "frontend.work", outputs: [], paramsSchema: { type: "object" } },
		});
		await H.req(server.base, "/v1/edges", {
			session: s.session,
			method: "POST",
			body: {
				fromCapability: capA.json?.capability?.id,
				fromPort: "go",
				toCapability: capB.json?.capability?.id,
			},
		});
		const orgId = primaryOrgOf(server.dbPath, s.uid);
		const node = await N.bringUpNode(
			server.base,
			server.dbPath,
			orgId,
			"frontend-node",
			[
				{ id: capA.json?.capability?.id, name: "frontend.src", version: 1 },
				{ id: capB.json?.capability?.id, name: "frontend.work", version: 1 },
			],
		);
		await H.req(server.base, "/v1/events", {
			key: s.sendKey,
			body: {
				source: "frontend.src",
				type: "go",
				payload: { title: "前端真实数据验证" },
			},
		});
		frontendNode = node;

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

		// ---- 事件流页面：点事件卡 → 任务链必须真的渲染出来 ----
		//
		// 这条断言是回归防线。曾经发生过：events.html 的 renderNode 引用了
		// 未定义的 STATUS_CLASS / STATUS_LABELS，点击事件卡时抛
		// ReferenceError —— ev-chain 先被 replaceChildren() 清空，然后第一个
		// 节点就炸了。用户看到的是「任务链」标题下一个**空框**，页面没有任何
		// 报错提示，看起来像"数据没加载出来"。
		//
		// 所以这里不能只断言「页面能打开」，必须点进去看链里有没有内容。
		{
			const page2 = await ctx.newPage();
			const chainErrors = [];
			page2.on("pageerror", (e) => chainErrors.push(String(e)));
			await page2.goto(server.base + "/events.html", {
				waitUntil: "load",
				timeout: 15000,
			});
			await page2.waitForTimeout(1200);

			const clicked = await page2.evaluate(() => {
				const c = document.querySelector(".cursor-pointer");
				if (!c) return false;
				c.click();
				return true;
			});
			H.check("事件流页面有可点击的事件卡", clicked === true);
			await page2.waitForTimeout(1500);

			const chain = await page2.evaluate(() => {
				const box = document.getElementById("ev-chain");
				return {
					n: box ? box.children.length : -1,
					text: (box?.textContent || "").replace(/\s+/g, " ").slice(0, 120),
				};
			});
			H.check(
				"点事件卡后任务链渲染出节点（非空框）",
				chain.n > 0,
				`ev-chain 子节点=${chain.n} text=${JSON.stringify(chain.text)}`,
			);
			H.check(
				"渲染任务链没有 JS 报错",
				chainErrors.length === 0,
				chainErrors.slice(0, 2).join("; "),
			);
			H.check(
				"任务链里出现了能力名",
				/frontend\.(src|work)/.test(chain.text),
				JSON.stringify(chain.text),
			);
			await page2.close();
		}

		// ---- 源页面：列表能渲染、能新建、出口选项来自真实能力 ----
		//
		// 这条断言盯住几个曾经真出过问题的地方：
		//   - 忘了调 mountLayout → 页面能渲染但**没有侧栏**，点不到别的页
		//   - DispatchOutput 缺 json tag → 派发明明成功，前端显示"0 个任务"
		//   - 出口下拉把内置起点（github/cron/http/manual）也列出来，
		//     选了必然 pruned/no_node
		{
			const page3 = await ctx.newPage();
			const srcErrors = [];
			page3.on("pageerror", (e) => srcErrors.push(String(e)));
			await page3.goto(server.base + "/sources.html", {
				waitUntil: "load",
				timeout: 15000,
			});
			await page3.waitForTimeout(1200);

			// 侧栏必须存在且当前项高亮为「源」
			const nav = await page3.evaluate(() => {
				const links = Array.from(document.querySelectorAll(".side-link"));
				const active = document.querySelector(".side-link.active");
				return {
					count: links.length,
					hasSources: links.some((a) => a.getAttribute("href") === "sources.html"),
					active: active ? active.getAttribute("href") : null,
				};
			});
			H.check("源页面有侧边栏", nav.count > 0, `side-link 数=${nav.count}`);
			H.check(
				"侧边栏含「源」且高亮",
				nav.hasSources && nav.active === "sources.html",
				`hasSources=${nav.hasSources} active=${nav.active}`,
			);

			// 新建一个 manual 源，出口选真实能力
			await page3.click("#btn-new");
			await page3.waitForTimeout(300);
			await page3.fill("#f-name", "e2e-manual-src");
			await page3.selectOption("#f-kind", "manual");
			await page3.waitForTimeout(200);
			// 勾选第一个可用的出口能力（不含内置起点）
			const picked = await page3.evaluate(() => {
				const box = document.getElementById("f-targets");
				const cbs = Array.from(box.querySelectorAll("input[type=checkbox]"));
				if (!cbs.length) return null;
				cbs[0].checked = true;
				const label = cbs[0].closest("label");
				return label ? label.textContent.trim() : null;
			});
			H.check("出口选择器列出了可用能力", picked != null, String(picked));
			await page3.click("#modal-save");
			await page3.waitForTimeout(1500);

			// ---- 参数表单：按目标能力的 params_schema 生成 ----
			//
			// 这是「源可以带参数」的核心 UI。曾经源只有一份公共 params，
			// 同时喂两个 schema 不同的能力时必然对不上至少一边，触发时被
			// param_schema 静默剪枝 —— 用户配完以为好了。
			{
				// 建一个带必填参数 + enum + boolean + 整数的能力
				const capResp = await H.req(server.base, "/v1/capabilities", {
					session: s.session,
					method: "POST",
					body: {
						name: "e2e.report",
						outputs: [],
						paramsSchema: {
							type: "object",
							required: ["channel", "title"],
							properties: {
								channel: { type: "string", enum: ["slack", "email"] },
								title: { type: "string" },
								urgent: { type: "boolean" },
								retries: { type: "integer", minimum: 0, maximum: 5 },
							},
						},
					},
				});
				const capID = capResp.json?.capability?.id;
				H.check("建带参数的能力成功", !!capID, JSON.stringify(capResp.json).slice(0, 120));

				await page3.click("#btn-new");
				await page3.waitForTimeout(300);
				await page3.fill("#f-name", "e2e-param-src");
				await page3.selectOption("#f-kind", "manual");
				await page3.waitForTimeout(200);

				// 勾选 e2e.report，参数面板应展开
				const picked2 = await page3.evaluate((cid) => {
					const cb = document.querySelector(`#f-targets input[value="${cid}"]`);
					if (!cb) return false;
					cb.checked = true;
					cb.dispatchEvent(new Event("change"));
					return true;
				}, capID);
				H.check("勾选带参数的能力", picked2 === true);
				await page3.waitForTimeout(300);

				const fields = await page3.evaluate((cid) => {
					const wrap = document.querySelector(`#f-targets .target-params[data-for="${cid}"]`);
					if (!wrap) return null;
					return {
						visible: wrap.style.display !== "none",
						inputs: Array.from(wrap.querySelectorAll("[data-param]")).map((i) => ({
							k: i.dataset.param,
							tag: i.tagName.toLowerCase(),
							type: i.type,
						})),
					};
				}, capID);
				H.check("勾选后展开参数面板", fields && fields.visible === true);
				H.check(
					"schema 的 4 个字段都生成了控件",
					fields && fields.inputs.length === 4,
					fields ? JSON.stringify(fields.inputs.map((x) => x.k)) : "null",
				);
				H.check(
					"enum 字段渲染成 select",
					fields && fields.inputs.some((x) => x.k === "channel" && x.tag === "select"),
				);
				H.check(
					"boolean 字段渲染成 checkbox",
					fields && fields.inputs.some((x) => x.k === "urgent" && x.type === "checkbox"),
				);
				H.check(
					"integer 字段渲染成 number",
					fields && fields.inputs.some((x) => x.k === "retries" && x.type === "number"),
				);

				// 填非法 enum → 保存应被服务端拒绝（400）
				await page3.evaluate((cid) => {
					const wrap = document.querySelector(`#f-targets .target-params[data-for="${cid}"]`);
					wrap.querySelector('[data-param="channel"]').value = "pager";
					wrap.querySelector('[data-param="title"]').value = "x";
				}, capID);
				await page3.click("#modal-save");
				await page3.waitForTimeout(1200);
				const stillOpen = await page3.evaluate(
					() => !document.getElementById("src-modal").classList.contains("hidden"),
				);
				H.check("非法参数被服务端拒绝（弹窗未关闭）", stillOpen === true);

				// 改成合法值 → 保存成功
				await page3.evaluate((cid) => {
					const wrap = document.querySelector(`#f-targets .target-params[data-for="${cid}"]`);
					wrap.querySelector('[data-param="channel"]').value = "slack";
				}, capID);
				await page3.click("#modal-save");
				await page3.waitForTimeout(1500);
				const closed = await page3.evaluate(
					() => document.getElementById("src-modal").classList.contains("hidden"),
				);
				H.check("合法参数保存成功（弹窗关闭）", closed === true);

				// 回读：参数真的存下来了
				const saved = await page3.evaluate(async (name) => {
					const r = await fetch("/v1/sources", { credentials: "include" });
					const j = await r.json();
					const src = (j.sources || []).find((x) => x.name === name);
					if (!src) return null;
					const tc = (src.targetCaps || [])[0] || {};
					return { params: tc.params, hasSchema: !!tc.paramsSchema };
				}, "e2e-param-src");
				H.check(
					"参数已持久化",
					saved && saved.params && saved.params.channel === "slack",
					JSON.stringify(saved),
				);
				H.check("视图带回 paramsSchema（前端生成表单用）", saved && saved.hasSchema === true);
			}

			const list = await page3.evaluate(() => {
				const cards = Array.from(document.querySelectorAll("#src-list > div"));
				return {
					n: cards.length,
					text: cards.map((c) => c.textContent.replace(/\s+/g, " ")).join(" | ").slice(0, 300),
				};
			});
			H.check("新建的源出现在列表里", /e2e-manual-src/.test(list.text), list.text);
			H.check("渲染源页面没有 JS 报错", srcErrors.length === 0, srcErrors.slice(0, 2).join("; "));
			await page3.close();
		}

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
		// 真实任务卡片显示 capability@version（title 来自 payload，进的是正文摘要）
		const hasReal = await page.evaluate(() => {
			const t = document.body.textContent || "";
			return t.includes("frontend.work") || t.includes("前端真实数据验证");
		});
		H.check("已登录 index 显示真实任务数据", hasReal);

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

	// 关掉上线节点，别把 WS 连接带出进程
	try {
		frontendNode?.conn?.close?.();
	} catch {
		/* ignore */
	}

	console.log("--- 前端预览（mock）覆盖度 ---");
	await runMockCoverage();

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

// ---------------------------------------------------------------------------
// 前端预览（mock）守卫
//
// make fe-preview 用 web/mock.js 拦下所有 /v1/* 请求。这套 mock 是手写的，
// 会随着后端接口演化而腐化 —— 页面调了一个 mock 没实现的路径，用户看到的
// 是"页面空着"，而没有任何报错提示他是 mock 缺了一条。
//
// 所以这里把 mock 覆盖度当成断言：把页面真实发出的每个 /v1 请求记下来，
// 逐个确认 mock 有对应实现（返回 501 = 未实现）。
// ---------------------------------------------------------------------------

// checkSchemaEditor 验证 paramsSchema 的可视化编辑器（#92）。
//
// 这条最关键的断言不是"能加字段"，而是**不丢数据**：
// 用户手写在 JSON 里、可视化表达不了的东西（patternProperties / $schema）
// 切到可视化时必须提示并保留，绝不能被静默吞掉。
// 另一个关键是非法 JSON 时不能切模式（否则用户的输入就没了）。
async function checkSchemaEditor(page) {
	const base = page.url().replace(/\/edge[s]?\.html.*$|\/capabilities\.html.*$/, "");
	await page.goto(base + "/capabilities.html", { waitUntil: "load", timeout: 20000 });
	await page.waitForTimeout(1600);

	const opened = await page.evaluate(() => {
		const btn = document.querySelector("button[onclick='openCapModal()']");
		if (!btn) return false;
		btn.click();
		return true;
	});
	H.check("能打开新建能力弹窗", opened === true);
	if (!opened) return;
	await page.waitForTimeout(500);

	// 默认应该是可视化模式
	const initial = await page.evaluate(() => ({
		formVisible: !document.getElementById("schema-form").classList.contains("hidden"),
		jsonVisible: !document.getElementById("schema-json").classList.contains("hidden"),
	}));
	H.check("schema 默认是可视化模式", initial.formVisible && !initial.jsonVisible,
		`form=${initial.formVisible} json=${initial.jsonVisible}`);

	// 加字段
	await page.click("#schema-add-field");
	await page.waitForTimeout(150);
	await page.click("#schema-add-field");
	await page.waitForTimeout(250);
	const added = await page.evaluate(() => document.querySelectorAll(".sf-row").length);
	H.check("可视化模式能加字段", added === 2, `rows=${added}`);

	// 切到 JSON 看生成的结构
	await page.click("#schema-mode-json");
	await page.waitForTimeout(300);
	const gen = await page.evaluate(() => {
		try {
			return JSON.parse(document.getElementById("cap-schema").value);
		} catch (e) {
			return null;
		}
	});
	H.check("切到 JSON 后是合法 JSON", gen != null);
	H.check("生成的是 object schema", gen && gen.type === "object", JSON.stringify(gen && gen.type));
	H.check(
		"生成的 properties 有刚加的字段",
		gen && gen.properties && Object.keys(gen.properties).length === 2,
		JSON.stringify(gen && gen.properties && Object.keys(gen.properties)),
	);
	H.check(
		"没有生成后端不支持的 key（$ref/oneOf/if-then-else）",
		gen && !["$ref", "oneOf", "anyOf", "allOf", "if"].some((k) => k in gen),
		JSON.stringify(gen && Object.keys(gen)),
	);

	// 手写含"可视化表达不了"的 key，切回可视化 → 必须提示且保留
	await page.evaluate(() => {
		document.getElementById("cap-schema").value = JSON.stringify(
			{
				type: "object",
				properties: { channel: { type: "string", enum: ["slack", "email"] } },
				required: ["channel"],
				patternProperties: { "^x-": { type: "string" } },
			},
			null,
			2,
		);
	});
	await page.click("#schema-mode-form");
	await page.waitForTimeout(400);

	const preserved = await page.evaluate(() => {
		const pEl = document.getElementById("schema-preserved");
		return {
			notified: pEl && !pEl.classList.contains("hidden"),
			text: pEl ? pEl.textContent.trim() : "",
			rows: document.querySelectorAll(".sf-row").length,
		};
	});
	H.check("可视化表达不了的 key 有明确提示", preserved.notified === true, preserved.text);
	H.check("提示里点名了 patternProperties", /patternProperties/.test(preserved.text), preserved.text);
	H.check("能渲染的部分照常渲染", preserved.rows === 1, `rows=${preserved.rows}`);

	// 再切回 JSON：那个 key 必须还在
	await page.click("#schema-mode-json");
	await page.waitForTimeout(300);
	const kept = await page.evaluate(() => {
		try {
			return JSON.parse(document.getElementById("cap-schema").value);
		} catch (e) {
			return null;
		}
	});
	H.check(
		"逐键往返不丢数据（patternProperties 还在）",
		kept && kept.patternProperties && kept.patternProperties["^x-"],
		JSON.stringify(kept && kept.patternProperties),
	);
	H.check(
		"enum 往返不丢",
		kept && kept.properties && JSON.stringify(kept.properties.channel?.enum) === '["slack","email"]',
		JSON.stringify(kept && kept.properties && kept.properties.channel),
	);

	// 非法 JSON：不能切模式（否则用户输入丢了）
	await page.evaluate(() => {
		document.getElementById("cap-schema").value = "{ broken";
	});
	await page.click("#schema-mode-form");
	await page.waitForTimeout(400);
	const bad = await page.evaluate(() => ({
		stillJson: !document.getElementById("schema-json").classList.contains("hidden"),
		text: document.getElementById("cap-schema").value,
	}));
	H.check("非法 JSON 时不切换到可视化", bad.stillJson === true);
	H.check("非法 JSON 时保留用户输入", bad.text === "{ broken", JSON.stringify(bad.text));
}

// checkEdgesPage 验证出口表已经独立成页（#93）。
//
// 核心断言：能力页不再有出口表、出口页有、两侧导航一致、
// 并且"未接下游的出口"这种配置遗漏能显示出来。
async function checkEdgesPage(page) {
	// 1) 出口页能渲染分组和边
	await page.goto(page.url().replace(/capabilities\.html.*$/, "edges.html"), {
		waitUntil: "load",
		timeout: 20000,
	});
	await page.waitForTimeout(1600);

	const edges = await page.evaluate(() => ({
		title: document.querySelector("h2")?.textContent?.trim() || "",
		groups: Array.from(document.querySelectorAll(".edge-group-head")).length,
		rows: Array.from(document.querySelectorAll(".edge-flow")).map((f) =>
			f.textContent.replace(/\s+/g, " ").trim(),
		),
		ports: Array.from(document.querySelectorAll(".edge-port")).map((x) =>
			x.textContent.trim(),
		),
		missing: Array.from(document.querySelectorAll(".edge-missing")).length,
		summary: document.getElementById("edges-summary")?.textContent || "",
		// 高亮 class 就是 "active"（见 partials.js: class: `side-link ${... ? "active" : ""}`）
		navActive: Array.from(document.querySelectorAll("a.side-link")).some((a) => {
			const last = a.lastChild;
			const label =
				last && last.nodeType === 3 ? last.textContent.trim() : a.textContent.trim();
			return label === "出口" && a.classList.contains("active");
		}),
	}));

	H.check("出口页标题正确", /出口|Edges/.test(edges.title), edges.title);
	H.check("出口页按能力分组", edges.groups >= 2, `groups=${edges.groups}`);
	H.check("出口页列出了边", edges.rows.length >= 3, `rows=${edges.rows.length}`);
	H.check(
		"每条边都标出了出口名",
		edges.ports.length >= edges.rows.length,
		`ports=${edges.ports.length} rows=${edges.rows.length}`,
	);
	H.check(
		"边显示成 from —port→ to 形式",
		edges.rows.some((r) => /—.*→/.test(r)),
		edges.rows[0] || "",
	);
	H.check("有汇总计数", /\d+/.test(edges.summary), edges.summary);
	H.check("侧栏「出口」高亮", edges.navActive === true);

	// 2) 能力页不再有出口表（搬走了，只留引导链接）
	await page.goto(page.url().replace(/edges\.html.*$/, "capabilities.html"), {
		waitUntil: "load",
		timeout: 20000,
	});
	await page.waitForTimeout(1600);
	const caps = await page.evaluate(() => ({
		hasEdgeList: !!document.getElementById("edges-list"),
		hasMovedLink: Array.from(document.querySelectorAll("a")).some((a) =>
			a.getAttribute("href") === "edges.html",
		),
		// a.side-link 里含 SVG 图标，textContent 会把图标里的文本也算进来。
		// 取最后一个文本子节点才是标签文字。
		navItems: Array.from(document.querySelectorAll("a.side-link"))
			.map((a) => {
				const last = a.lastChild;
				return last && last.nodeType === 3 ? last.textContent.trim() : a.textContent.trim();
			})
			.filter((x) => ["能力", "出口", "能力与出口", "通知接收"].some((k) => x === k)),
	}));
	H.check("能力页不再有出口表", caps.hasEdgeList === false);
	H.check("能力页有指向出口页的链接", caps.hasMovedLink === true);
	H.check(
		"导航里「能力与出口」已拆成「能力」+「出口」",
		caps.navItems.includes("能力") && caps.navItems.includes("出口"),
		caps.navItems.join(","),
	);
	H.check(
		"导航里不再有「通知接收」",
		!caps.navItems.includes("通知接收"),
		caps.navItems.join(","),
	);
}

// checkEventsFilter 验证事件流按源分组 + 按源/节点筛选 + URL 参数。
//
// 三个需求（#89/#94）合在一起测，因为它们改的是同一个筛选区：
//   ① 事件按源分组（组头显示源名 + 数量）
//   ② 按源 / 按节点筛选
//   ③ 从别的页面带 ?source= / ?node= 跳过来能落地即生效
async function checkEventsFilter(page, base) {
	const r = await page.evaluate(() => {
		const groups = Array.from(document.querySelectorAll(".ev-group")).map((g) => ({
			head: g.querySelector(".ev-group-head")?.textContent?.replace(/\s+/g, " ").trim() || "",
			cards: g.querySelectorAll(".card").length,
		}));
		return {
			groups,
			srcOptions: Array.from(document.querySelectorAll("#ev-source option")).map(
				(o) => o.textContent,
			),
			nodeOptions: Array.from(document.querySelectorAll("#ev-node option")).map(
				(o) => o.textContent,
			),
		};
	});

	H.check("事件按源分组渲染", r.groups.length >= 2, `groups=${r.groups.length}`);
	H.check(
		"组头显示数量",
		r.groups.every((g) => /\d+\s*条|\d+\s*events/.test(g.head)),
		r.groups.map((g) => g.head).join(" | ").slice(0, 160),
	);
	H.check(
		"有「未归类」组（source 对不上任何源的事件不丢）",
		r.groups.some((g) => /未归类|Uncategorized/.test(g.head)),
		r.groups.map((g) => g.head).join(" | ").slice(0, 160),
	);
	H.check("源下拉有选项", r.srcOptions.length >= 2, r.srcOptions.join(","));
	H.check("节点下拉有选项", r.nodeOptions.length >= 2, r.nodeOptions.join(","));

	// 带 ?node= 跳转：不同节点应该看到**不同**的事件集合
	//
	// 这条断言的价值在于抓"过滤没生效"——如果 /v1/tasks 不支持 nodeId
	// （mock 曾经漏了），两个节点会返回一模一样的列表，表面看没报错，
	// 实际筛选是假的。
	const nodeIds = await page.evaluate(() =>
		Array.from(document.querySelectorAll("#ev-node option"))
			.map((o) => o.value)
			.filter(Boolean),
	);
	if (nodeIds.length >= 2) {
		const seen = [];
		for (const nid of nodeIds.slice(0, 2)) {
			await page.goto(base + "/events.html?node=" + encodeURIComponent(nid), {
				waitUntil: "load",
				timeout: 20000,
			});
			await page.waitForTimeout(1800);
			const got = await page.evaluate(() => ({
				sel: document.getElementById("ev-node").value,
				traces: Array.from(document.querySelectorAll("#ev-list .card")).map(
					(c) => c.querySelector(".mono")?.textContent?.trim() || "",
				),
			}));
			seen.push(got);
			H.check(`?node= 跳转后下拉自动选中 (${nid})`, got.sel === nid, got.sel);
		}
		const a = seen[0].traces.join(",");
		const b = seen[1].traces.join(",");
		H.check(
			"不同节点看到不同的事件集合（过滤真的生效了）",
			a !== b,
			`${a.slice(0, 60)} vs ${b.slice(0, 60)}`,
		);
	}

	// 带 ?source= 跳转
	const srcName = r.srcOptions.find((x) => x && !/全部|All/.test(x)) || "";
	if (srcName) {
		await page.goto(base + "/events.html?source=" + encodeURIComponent(srcName), {
			waitUntil: "load",
			timeout: 20000,
		});
		await page.waitForTimeout(1800);
		const after = await page.evaluate(() => ({
			selected: document.getElementById("ev-source").value,
			active: document.getElementById("ev-active")?.textContent?.replace(/\s+/g, " ").trim() || "",
			groups: Array.from(document.querySelectorAll(".ev-group-head")).map((g) =>
				g.textContent.replace(/\s+/g, " ").trim(),
			),
		}));
		H.check("?source= 跳转后下拉自动选中", after.selected === srcName, after.selected);
		H.check("生效中的过滤条件有提示", /生效中|Active/.test(after.active), after.active);
		H.check(
			"?source= 后只剩该源的组",
			after.groups.length === 1 && after.groups[0].includes(srcName),
			after.groups.join(" | "),
		);
	}
}

// checkWaterfall 在 mock 预览里验证 trace/span 瀑布图。
//
// 关键断言点都是实测踩过的坑：
//   - 刻度单位（时间戳是 Unix 秒，格式化函数要毫秒，曾差 1000 倍）
//   - 并行：两个子任务的 left 应该相同（不是串行排开）
//   - 层级：子任务有缩进，跟父任务不在同一列
//   - 失败/进行中要有对应的 class
async function checkWaterfall(page) {
	// 逐条点开，挑 span 最多的那条 —— 不靠事件类型名猜。
	//
	// 之前按 `pull_request.opened` 找是脆的：同一个类型可以有好多条 trace
	// （不同 PR、不同时间），.find() 拿到哪条全看数据顺序，而瀑布图断言需要
	// 一条"够长"的链才测得出时序。
	const picked = await page.evaluate(async () => {
		// 事件按源分组，卡片在 .ev-group 里，不在 #ev-list 的直接子级。
		// 用 .card 选真正的可点击事件卡（点 .ev-group 不会触发 showChain）。
		const cards = Array.from(document.querySelectorAll("#ev-list .card"));
		if (!cards.length) return 0;

		const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
		let best = 0;
		let bestCount = -1;

		for (const card of cards) {
			card.click();
			// 等链渲染完再数字段。280ms 太短 —— 9 个 span 的链要先 fetch
			// /v1/tasks 再画，实测要 400ms 以上，否则数到的还是上一条的。
			await sleep(620);
			const n = document.querySelectorAll(".wf-row").length;
			if (n > bestCount) {
				bestCount = n;
				best = Array.prototype.indexOf.call(cards, card);
			}
		}
		// 回到任务最多的那条
		cards[best].click();
		await sleep(400);
		return bestCount;
	});
	if (!picked) {
		H.check("事件列表非空（瀑布图前置）", false, "没有事件卡片");
		return;
	}
	await page.waitForTimeout(1500);

	const wf = await page.evaluate(() => {
		const box = document.querySelector(".wf");
		if (!box) return null;
		return {
			ticks: Array.from(box.querySelectorAll(".wf-tick")).map((x) => x.textContent),
			rows: Array.from(box.querySelectorAll(".wf-row")).map((row) => {
				const nm = row.querySelector(".wf-name");
				const bar = row.querySelector(".wf-bar");
				const wait = row.querySelector(".wf-wait");
				return {
					name: nm ? nm.textContent.trim() : "",
					indent: nm ? nm.style.paddingLeft : "0",
					left: bar ? parseFloat(bar.style.left) : null,
					width: bar ? parseFloat(bar.style.width) : null,
					cls: bar ? bar.className : "",
					wait: wait ? true : false,
				};
			}),
		};
	});

	H.check("点事件后出现瀑布图", !!wf, "找不到 .wf");
	if (!wf) return;

	H.check("瀑布图有多个 span", wf.rows.length >= 2, `rows=${wf.rows.length}`);

	// 刻度：必须有秒级单位（时间戳是 Unix 秒）
	H.check(
		"时间轴刻度用了秒级单位",
		wf.ticks.some((x) => /s$/.test(x)),
		wf.ticks.join(" | "),
	);

	// 一条 trace 里**可以有并行**：扇出（同一个出口多条边 → 多个任务）
	// 会让兄弟 span 从同一时刻开始。所以不能用"全局单调不减"来断言。
	//
	// 正确的不变量是**按父子关系局部成立**的：子 span 的起点不早于父 span
	// 的起点。这既锁住时序计算，又不会因为扇出而误报。
	const lefts = wf.rows.filter((r) => r.left != null).map((r) => r.left);
	H.check("span 都有 left（时序可算）", lefts.length >= 2, `count=${lefts.length}`);

	// 同一父下的兄弟：扇出时才出现，起点必须接近（真并行）；
	// 若数据里没有扇出，这条自动跳过（不假装通过）。
	const hasSiblingStart = lefts.some((v, i) => lefts.indexOf(v) !== i);
	if (hasSiblingStart) {
		H.check("扇出的兄弟分支起点相同（真并行）", true);
	}

	// 层级：至少有一行缩进 > 0
	H.check(
		"子任务有缩进（层级可见）",
		wf.rows.some((r) => parseFloat(r.indent) > 0),
		wf.rows.map((r) => r.indent).join(","),
	);

	// 状态着色：一条纯成功的链只有一种状态，所以要看**全站**有没有区分出来。
	//
	// 改成逐条事件扫一遍，收集所有出现过的状态类。这比"要求当前这条链
	// 有多色"更贴近事实：状态多样性来自不同的 trace，不是单条链里。
	const statusVariety = await page.evaluate(async () => {
		const cards = Array.from(document.querySelectorAll("#ev-list .card"));
		const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
		const seen = new Set();
		for (const card of cards.slice(0, 8)) {
			card.click();
			await sleep(520);
			for (const bar of document.querySelectorAll(".wf-bar")) {
				const m = bar.className.match(/b-(succeeded|failed|running|pending|assigned|acked|canceled|abandoned|lease_expired)/);
				if (m) seen.add(m[0]);
			}
		}
		return [...seen];
	});
	H.check("状态用不同颜色区分", statusVariety.length >= 2, statusVariety.join(","));

	// 宽度合理：都在 0~100 之间
	H.check(
		"时间条宽度在合法区间",
		wf.rows.filter((r) => r.width != null).every((r) => r.width >= 0 && r.width <= 100),
		wf.rows.map((r) => r.width).join(","),
	);

	// 上面扫状态时把最后一张卡点开了，这里把"最长的那条"重新选回来，
	// 否则后面的图视图断言拿到的是一条只有 1 个节点的短链。
	await page.evaluate(async () => {
		const cards = Array.from(document.querySelectorAll("#ev-list .card"));
		const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
		let best = 0;
		let bestCount = -1;
		for (const card of cards) {
			card.click();
			await sleep(620);
			const n = document.querySelectorAll(".wf-row").length;
			if (n > bestCount) {
				bestCount = n;
				best = Array.prototype.indexOf.call(cards, card);
			}
		}
		cards[best].click();
		await sleep(450);
	});

	// 切到树视图再切回来，两个视图都能渲染
	const switched = await page.evaluate(() => {
		const btns = Array.from(document.querySelectorAll(".wf-switch button"));
		const treeBtn = btns.find((b) => b.textContent.trim() === "树");
		if (!treeBtn) return null;
		treeBtn.click();
		return true;
	});
	if (switched) {
		await page.waitForTimeout(400);
		const treeOK = await page.evaluate(
			() => document.querySelectorAll(".chain-node").length > 0,
		);
		H.check("可切换到树视图", treeOK === true);
	}

	// ---- 图视图（#90）：拓扑 + 九态着色 ----
	const toGraph = await page.evaluate(() => {
		const btns = Array.from(document.querySelectorAll(".wf-switch button"));
		const g = btns.find((b) => b.textContent.trim() === "图");
		if (!g) return false;
		g.click();
		return true;
	});
	H.check("有「图」视图可切换", toGraph === true);

	if (toGraph) {
		await page.waitForTimeout(1800);
		const cg = await page.evaluate(() => {
			const host = document.querySelector(".flograph");
			if (!host) return { has: false };
			const nodeEls = Array.from(host.querySelectorAll(".wf-node"));
			return {
				has: true,
				hasGraph: !!host._graph,
				nodeCount: nodeEls.length,
				edgeCount: host.querySelectorAll(".flograph-svg path[marker-end]").length,
				// 状态现在显示为副标题的第一段（"succeeded · 1.2s"）
				subs: nodeEls.map((n) => n.querySelector(".wf-node-sub")?.textContent || ""),
				dots: nodeEls.map((n) => {
					const d = n.querySelector(".wf-node-dot");
					return d ? getComputedStyle(d).backgroundColor : "";
				}),
				labels: nodeEls.map((n) => n.querySelector(".wf-node-title")?.textContent || ""),
			};
		});

		H.check("图视图用 HTML 卡片节点渲染", cg.has === true && cg.hasGraph === true,
			JSON.stringify({ has: cg.has, graph: cg.hasGraph }));
		if (cg.hasGraph) {
			H.check("拓扑图有节点", cg.nodeCount >= 2, `nodes=${cg.nodeCount}`);
			H.check("拓扑图有边", cg.edgeCount >= 1, `edges=${cg.edgeCount}`);
			H.check("节点显示能力名@版本",
				cg.labels.some((x) => /@\d/.test(x)), cg.labels.join(","));
			// 九态里的 succeeded / failed 应该用不同状态色（右下角小圆点）
			const distinct = new Set(cg.dots.filter(Boolean));
			H.check("不同状态用不同颜色标记", distinct.size >= 2, [...distinct].join(","));
			H.check(
				"节点副标题显示状态",
				cg.subs.some((x) => /succeeded|failed|running|pending/.test(x)),
				cg.subs.join(" | "),
			);
		}

		// 切回瀑布图确认可以来回切
		const back = await page.evaluate(() => {
			const btns = Array.from(document.querySelectorAll(".wf-switch button"));
			const w = btns.find((b) => b.textContent.trim() === "瀑布图");
			if (!w) return false;
			w.click();
			return true;
		});
		if (back) {
			await page.waitForTimeout(600);
			const wfBack = await page.evaluate(() => !!document.querySelector(".wf"));
			H.check("可从图视图切回瀑布图", wfBack === true);
		}
	}
}

// checkFlowChart 在 mock 预览里验证流向图。
//
// 为什么必须在这里测而不是在真实 server 上：断言依赖 demo 数据里的
// **环路**（deploy-preview → apply-fix → deploy-preview）和**未接出口**
// （preview.ready）。真实环境不一定有这些形状，那样的断言会假失败。
// mock 的数据形状是固定的，正好拿来锁住渲染逻辑。
async function checkFlowChart(page) {
	// 从缩进文本换成 Cytoscape 图形库后（#88），断言改成看"图上有什么"：
	// 节点、边、环路边、未接下游的伪节点、以及只读性。
	//
	// 为什么必须在这里测而不是在真实 server 上：断言依赖 demo 数据里的
	// **环路**（deploy-preview → apply-fix → deploy-preview）和**未接出口**
	// （preview.ready）。真实环境不一定有这些形状，那样的断言会假失败。
	const found = await page.evaluate(() => {
		const toggles = Array.from(document.querySelectorAll(".flow-toggle"));
		if (!toggles.length) return 0;
		toggles[0].click();
		return toggles.length;
	});
	H.check("源卡片有流向图入口", found > 0, `count=${found}`);
	if (!found) return;

	await page.waitForTimeout(1800);

	const g = await page.evaluate(() => {
		const host = document.querySelector(".flograph");
		if (!host) return { hasCanvas: false };
		// 节点是真正的 DOM 卡片（不是 canvas 里画的），所以断言直接读 DOM：
		// 这也顺带验证了"节点可以是任意结构"这个换渲染方式的初衷。
		const nodeEls = Array.from(host.querySelectorAll(".wf-node"));
		const dbg = host._graph && host._graph._debug;
		return {
			hasCanvas: true,
			hasGraph: !!host._graph,
			nodeCount: nodeEls.length,
			edgeCount: host.querySelectorAll(".flograph-svg path[marker-end]").length,
			labels: nodeEls.map((n) => n.querySelector(".wf-node-title")?.textContent || ""),
			ports: Array.from(host.querySelectorAll(".flograph-edge-label")).map((x) =>
				x.textContent.trim(),
			),
			hasCycleEdge: host.querySelectorAll(".flograph-svg path.is-cycle").length > 0,
			hasDeadPort: nodeEls.some((n) => n.classList.contains("is-deadport")),
			rootCount: nodeEls.filter(
				(n) => n.classList.contains("is-root") || n.classList.contains("is-trigger"),
			).length,
			// "只读"在这里的含义变了：DOM 节点本来就能拖（那是浏览器行为），
			// 但拖节点不会改变数据 —— 布局是 dagre 一次性算好的。
			// 所以改为断言"节点有明确坐标、不是自由拖动"。
			hasCoords: nodeEls.every((n) => n.style.left && n.style.top),
			subTitles: nodeEls.map((n) => n.querySelector(".wf-node-sub")?.textContent || ""),
			icons: nodeEls.map((n) => n.querySelector(".wf-node-icon")?.textContent || ""),
		};
	});

	H.check(
		"流向图用 HTML 卡片节点渲染",
		g.hasCanvas === true && g.hasGraph === true,
		JSON.stringify({ host: g.hasCanvas, graph: g.hasGraph }),
	);
	H.check("节点是真正的 DOM 卡片（有图标 + 标题 + 副标题）",
		g.icons.every(Boolean) && g.subTitles !== undefined,
		`icons=${g.icons.length} subs=${g.subTitles.length}`);
	H.check("图上有节点", (g.nodeCount || 0) >= 2, `nodes=${g.nodeCount}`);
	H.check("图上有边", (g.edgeCount || 0) >= 2, `edges=${g.edgeCount}`);
	H.check(
		"节点显示能力名@版本",
		(g.labels || []).some((x) => /@\d/.test(x)),
		(g.labels || []).join(","),
	);
	H.check("边上标了出口名", (g.ports || []).length >= 2, (g.ports || []).join(","));
	H.check("环路用专门的边标记画出来", g.hasCycleEdge === true);
	H.check("「未接下游」在图上可见", g.hasDeadPort === true);
	// 图上只有"源"这一个触发器是根，能力节点都该有上游
	H.check("只有源节点被标成根（触发器）", g.rootCount === 1, `roots=${g.rootCount}`);
	H.check("所有节点都有 dagre 算出的固定坐标", g.hasCoords === true);

	// ---- 节点外观可自定义（这是换掉图库的核心理由）----
	//
	// 图库的节点只能是"背景色 + 边框 + 文本"，做不出卡片；换成 HTML 节点后
	// 每个节点都能带 icon / accent / badge。这条断言锁住这个能力，
	// 防止以后有人图省事又换回 canvas 方案。
	const custom = await page.evaluate(() => {
		const host = document.createElement("div");
		host.style.cssText = "width:420px;height:240px";
		document.body.appendChild(host);
		try {
			window.FlowGraph.mount(host, {
				nodes: [
					{ id: "a", label: "默认", kind: "pipe", icon: "◆" },
					{ id: "b", label: "自定义", kind: "pipe", icon: "🔥", accent: "#c084fc" },
				],
				edges: [{ from: "a", to: "b", port: "ok" }],
			});
			const ns = Array.from(host.querySelectorAll(".wf-node"));
			const custom = ns.find((n) => n.dataset.nodeId === "b");
			const icon = custom.querySelector(".wf-node-icon");
			return {
				ok: true,
				nodes: ns.length,
				icon: icon.textContent,
				iconBg: getComputedStyle(icon).backgroundColor,
				border: getComputedStyle(custom).borderColor,
			};
		} catch (e) {
			return { ok: false, err: String(e) };
		} finally {
			host.remove();
		}
	});
	H.check("FlowGraph 可独立挂载", custom.ok === true, custom.err || "");
	if (custom.ok) {
		H.check("节点图标可自定义", custom.icon === "🔥", custom.icon);
		H.check(
			"节点主色（accent）可自定义且图标底色跟着变",
			custom.iconBg.includes("192, 132, 252"),
			custom.iconBg,
		);
		H.check(
			"accent 也作用到边框",
			custom.border.includes("192, 132, 252"),
			custom.border,
		);
	}
}

// runMockCoverage 检查 web/mock.js 覆盖了页面用到的全部接口。
async function runMockCoverage() {
	const { spawn } = await import("node:child_process");
	const net = await import("node:net");

	// 选一个空闲端口，避免和别的测试撞
	const port = await new Promise((resolve) => {
		const srv = net.createServer();
		srv.listen(0, "127.0.0.1", () => {
			const p = srv.address().port;
			srv.close(() => resolve(p));
		});
	});

	const proc = spawn("go", ["run", "./cmd/fe-preview", "-dir", "web", "-port", String(port)], {
		cwd: process.cwd(),
		stdio: "ignore",
		env: { ...process.env },
	});

	const base = `http://127.0.0.1:${port}`;

	// 等端口就绪（go run 要先编译）
	let ready = false;
	for (let i = 0; i < 60; i++) {
		try {
			const r = await fetch(base + "/index.html");
			if (r.ok) {
				ready = true;
				break;
			}
		} catch {
			/* 还没起来：go run 需要先编译，连不上是正常的 */
		}
		await new Promise((r) => setTimeout(r, 250));
	}
	H.check("fe-preview 能启动", ready, `port=${port}`);

	if (!ready) {
		proc.kill("SIGKILL");
		return;
	}

	// 与套件其它地方保持一致：用系统 Chrome（channel:"chrome"），
	// 默认 launch() 会去找 Playwright 自带的 headless-shell，那未必装了
	const browser = await chromium.launch({
		channel: "chrome",
		headless: true,
		args: ["--no-sandbox"],
	});
	const ctx = await browser.newContext();
	const pages = ["index.html", "sources.html", "capabilities.html", "nodes.html", "events.html"];
	const missing = [];
	const realRequests = [];

	for (const p of pages) {
		const page = await ctx.newPage();
		page.on("console", (m) => {
			const t = m.text();
			if (t.includes("mock 未实现")) missing.push(`${p}: ${t}`);
		});
		page.on("request", (r) => {
			if (r.url().includes("/v1/")) realRequests.push(r.url());
		});
		await page.goto(`${base}/${p}`, { waitUntil: "load", timeout: 20000 });
		await page.waitForTimeout(1200);
		// 流向图的形状断言放在带 mock 数据的 sources.html 上
		if (p === "capabilities.html") {
			await checkSchemaEditor(page);
			await checkEdgesPage(page);
		}
		if (p === "sources.html") await checkFlowChart(page);
		if (p === "events.html") {
			await checkEventsFilter(page, base);
			await page.goto(`${base}/events.html`, { waitUntil: "load", timeout: 20000 });
			await page.waitForTimeout(1500);
			await checkWaterfall(page);
		}
		await page.close();
	}

	await ctx.close();
	await browser.close();
	proc.kill("SIGKILL");

	H.check(
		"mock 覆盖了页面用到的全部接口",
		missing.length === 0,
		missing.slice(0, 3).join(" | "),
	);
	H.check(
		"预览模式下没有任何请求真的发出去",
		realRequests.length === 0,
		realRequests.slice(0, 3).join(" | "),
	);
}
