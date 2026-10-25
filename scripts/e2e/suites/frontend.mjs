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
	// 找任务最多的那条事件（扇出的那条）
	const picked = await page.evaluate(() => {
		// 注意：事件现在按源分组，卡片在 .ev-group 里面，不在 #ev-list 的直接子级。
		// 用 .card 选真正的可点击事件卡（点 .ev-group 不会触发 showChain）。
		const cards = Array.from(document.querySelectorAll("#ev-list .card"));
		const target =
			cards.find((c) => c.textContent.includes("pull_request.opened")) || cards[0];
		if (!target) return null;
		target.click();
		return true;
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

	// 并行：两个不同任务的 left 相同
	const lefts = wf.rows.filter((r) => r.left != null).map((r) => r.left);
	const hasParallel = lefts.some((v, i) => lefts.indexOf(v) !== i);
	H.check("能画出并行分支（两个 span 起点相同）", hasParallel, lefts.join(","));

	// 层级：至少有一行缩进 > 0
	H.check(
		"子任务有缩进（层级可见）",
		wf.rows.some((r) => parseFloat(r.indent) > 0),
		wf.rows.map((r) => r.indent).join(","),
	);

	// 状态着色：出现 failed / running / succeeded 中至少两种
	const classes = wf.rows.map((r) => r.cls).join(" ");
	const kinds = ["b-succeeded", "b-failed", "b-running", "b-pending"].filter((k) =>
		classes.includes(k),
	);
	H.check("状态用不同颜色区分", kinds.length >= 2, kinds.join(","));

	// 宽度合理：都在 0~100 之间
	H.check(
		"时间条宽度在合法区间",
		wf.rows.filter((r) => r.width != null).every((r) => r.width >= 0 && r.width <= 100),
		wf.rows.map((r) => r.width).join(","),
	);

	// 切到树视图再切回来，两个视图都能渲染
	const switched = await page.evaluate(() => {
		const btns = Array.from(document.querySelectorAll(".wf-switch button"));
		const treeBtn = btns.find((b) => !b.classList.contains("on"));
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
}

// checkFlowChart 在 mock 预览里验证流向图。
//
// 为什么必须在这里测而不是在真实 server 上：断言依赖 demo 数据里的
// **环路**（deploy-preview → apply-fix → deploy-preview）和**未接出口**
// （preview.ready）。真实环境不一定有这些形状，那样的断言会假失败。
// mock 的数据形状是固定的，正好拿来锁住渲染逻辑。
async function checkFlowChart(page) {
			// ---- 下游流向图：只读预览，能画出环路与未接出口 ----
			//
			// 这块的核心是"从源出发把所有可能路线画出来"。含环的图最容易出错
			// （无限递归 / 重复展开），所以断言要盯住：
			//   ① 环路被识别成「回到 X（环路）」而不是死循环
			//   ② 同一子树不重复展开两遍
			//   ③ 多个出口（多个根）各自完整展开，不互相挤掉
			//   ④ 声明了但没接边的出口要显式标出来
				const flow = await page.evaluate(() => {
					const wraps = Array.from(document.querySelectorAll(".flow-wrap"));
					return wraps.map((w) => {
						// 展开它
						const toggle = w.querySelector(".flow-toggle");
						toggle.click();
						const body = w.querySelector(".flow-body");
						return {
							title: toggle.textContent.replace(/\s+/g, " ").trim(),
							rows: Array.from(body.querySelectorAll(".flow-node")).map((el) => ({
								text: el.textContent.replace(/\s+/g, " ").trim(),
								back: !!el.querySelector(".flow-back"),
								dead: !!el.querySelector(".flow-dead"),
								terminal: !!el.querySelector(".flow-terminal"),
								port: el.querySelector(".flow-port")?.textContent?.trim() || "",
							})),
							seps: body.querySelectorAll(".flow-root-sep").length,
						};
					});
				});

				H.check("每个源都有流向图", flow.length > 0, `共 ${flow.length} 个`);

				const nightly = flow.find((f) => /每日|nightly|每晚/.test(f.title)) || flow[0];
				H.check(
					"流向图列出了下游能力",
					nightly.rows.some((r) => /deploy-preview|apply-fix/.test(r.text)),
					nightly.rows.map((r) => r.text).join(" | ").slice(0, 160),
				);
				H.check(
					"环路被标成「回到 X」而不是无限递归",
					nightly.rows.some((r) => r.back && /deploy-preview/.test(r.text)),
					nightly.rows.filter((r) => r.back).map((r) => r.text).join(" | ").slice(0, 160),
				);
				H.check(
					"同一子树不重复展开（出现「已在上方展开」）",
					nightly.rows.some((r) => /已在上方展开|expanded above/.test(r.text)),
					nightly.rows.map((r) => r.text).join(" | ").slice(0, 200),
				);
				H.check(
					"出口名标在每条支路上",
					nightly.rows.filter((r) => r.port).length >= 2,
					nightly.rows.filter((r) => r.port).map((r) => r.port).join(","),
				);
				H.check(
					"声明了但没接边的出口被标出",
					flow.some((f) => f.rows.some((r) => r.dead && r.port === "preview.ready")),
					flow.flatMap((f) => f.rows.filter((r) => r.dead).map((r) => r.port)).join(","),
				);

				// 多出口的源：两个根之间要有分隔线，否则会被看成同一棵树
				const multi = flow.find((f) => f.seps > 0);
				H.check(
					"多出口的源在根之间有分隔",
					!!multi,
					flow.map((f) => `${f.title.slice(0, 12)}=seps${f.seps}`).join(" "),
				);

				// 终止能力（没有下游）要能区分于"未接下游"
				H.check(
					"没有下游的能力标为「没有下游」/「终点」",
					flow.some((f) =>
						f.rows.some((r) => r.dead && /没有下游|no downstream/.test(r.text)),
					),
					"",
				);
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
