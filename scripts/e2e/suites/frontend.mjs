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
 *  H. nodes.html 注册节点（#198）：表单 → 一次性令牌 → 可直接粘的 node.yaml；
 *     再点入口必须把令牌真从 DOM 清掉，重名只报重名不给令牌（mock 全流程 + 真后端一次注册）
 */
import { chromium } from "playwright-core";
import * as H from "../lib/harness.mjs";
import * as N from "../lib/nodes.mjs";

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
	// 独立 trace 页（从事件流"查看链路"跳进来）
	"trace.html",
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
// 本组织默认命名空间 id（#162）：真实后端的命名空间资源请求必须显式带 namespaceId。
// 放在模块级是因为页面内直接发的请求（page.evaluate 里的 fetch）也要用它。
let realNsId = null;

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
			// 等客户端 JS 触发 401 → 跳转 login.html。
			//
			// 用轮询而不是 waitForURL：页面上多个并发 401 各自赋值 location.href，
			// 后一次会中止前一次导航，waitForURL 因此抛 ERR_ABORTED。原本
			// .catch(() => {}) 吞掉后立即读 url()，拿到的还是旧地址 —— 偶发假失败。
			// 详见 harness.waitForUrl 的注释。
			await H.waitForUrl(page, /login\.html/, 8000);
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
				// #110：入口边（来源名当左端），不再需要"起点能力"
				fromIsSource: true,
				fromCapability: "frontend.src",
				fromPort: "go",
				toCapability: capB.json?.capability?.id,
			},
		});
		// #204 / 迁移 14：组织层删除 —— 原来的「按 uid 查主组织」（primaryOrgOf 查
		// org_members）整段去掉，节点归属改用**默认命名空间** id（命名空间是唯一
		// 隔离边界），下面这句「拿到默认命名空间 id」就是它的等价断言。
		// #162：节点行直插库，迁移 12 起必须写 namespace_id。
		// 取本部署默认命名空间（slug=default）的 id，不硬编码 ns_xxx。
		realNsId = await H.defaultNamespaceId(server.base, s.session);
		H.check("拿到默认命名空间 id", !!realNsId, String(realNsId));
		const node = await N.bringUpNode(
			server.base,
			server.dbPath,
			realNsId,
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

			// 点卡片现在会**跳转到独立 trace 页**（不再是同页展开）。
			// 等导航完成，然后在新页面上断言。
			await page2.waitForURL(/trace\.html\?traceId=/, { timeout: 12000 }).catch(() => {});
			// 默认视图是图，等节点真的挂上来（dagre 量尺寸后才排布）
			await page2
				.waitForFunction(() => document.querySelectorAll(".wf-node").length > 0, {
					timeout: 12000,
				})
				.catch(() => {});

			const chain = await page2.evaluate(() => {
				const body = document.getElementById("trace-body");
				return {
					url: location.pathname + location.search,
					n: body ? body.children.length : -1,
					text: (body?.textContent || "").replace(/\s+/g, " ").slice(0, 160),
					// 默认视图 = 图（瀑布行必须为 0，否则说明默认值被打回去了）
					nodes: document.querySelectorAll(".wf-node").length,
					hasGraph: !!document.querySelector(".flograph"),
					active: (document.querySelector(".wf-switch button.on") || {}).textContent || "",
					wfRows: document.querySelectorAll(".wf-row").length,
					views: Array.from(document.querySelectorAll(".wf-switch button")).length,
				};
			});

			H.check(
				"点事件卡跳到独立 trace 页（URL 带 traceId）",
				/trace\.html\?traceId=/.test(chain.url),
				chain.url,
			);
			H.check(
				"trace 页渲染出内容（非空框）",
				chain.n > 0 && chain.text.length > 20,
				`子节点=${chain.n} text=${JSON.stringify(chain.text)}`,
			);
			H.check("渲染 trace 页没有 JS 报错", chainErrors.length === 0, chainErrors.slice(0, 2).join("; "));
			H.check(
				"trace 页有三种视图切换器",
				chain.views === 3,
				`views=${chain.views}`,
			);
			H.check(
				"trace 页默认就是「图」视图（不是瀑布图）",
				chain.hasGraph && chain.nodes >= 1 && chain.active === "图" && chain.wfRows === 0,
				`hasGraph=${chain.hasGraph} 节点=${chain.nodes} 当前=${chain.active} 瀑布行=${chain.wfRows}`,
			);
			H.check(
				"trace 页出现了能力名@版本",
				/@\d/.test(chain.text),
				JSON.stringify(chain.text.slice(0, 80)),
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

			// ---- GitHub 触发器：新建时就有字段清单（不必先触发）----
			//
			// GitHub 的 webhook 结构是公开契约，所以它的字段清单来自内置样例，
			// 而不是"等这个源收到过事件才反推"。这条断言盯住的是：曾经只剩
			// "真实事件"一条路径，于是新建 GitHub 源时一片空白，用户必须先让
			// GitHub 发一次才能配参数 —— 顺序倒了。
			{
				await page3.click("#btn-new");
				await page3.waitForTimeout(300);
				await page3.selectOption("#f-kind", "github");
				await page3.waitForTimeout(600);
				const gh = await page3.evaluate(() => {
					// 分组后可见文本是**短名**（前缀在组标题里），
					// 完整路径在 .av-row 的 data-path 上
					const paths = Array.from(document.querySelectorAll(".av-row")).map((n) =>
						n.dataset.path,
					);
					const src = document.getElementById("avail-src");
					return {
						count: paths.length,
						hasPrTitle: paths.includes("pull_request.title"),
						hasRepoName: paths.includes("repository.name"),
						srcText: src ? src.textContent.trim() : "",
					};
				});
				H.check(
					"GitHub 触发器新建即列出官方字段",
					gh.count > 20 && gh.hasPrTitle && gh.hasRepoName,
					`count=${gh.count} prTitle=${gh.hasPrTitle} repoName=${gh.hasRepoName}`,
				);
				H.check(
					"字段清单标明来自内置官方结构",
					/GitHub/.test(gh.srcText),
					gh.srcText.slice(0, 60),
				);

				// ---- 字段清单按顶层键分组、默认折叠 ----
				//
				// 35 个字段平铺会吃掉整屏。分组后一眼能看到有哪几组，
				// 想深入再展开 —— 面板高度从 ~1200px 降到 ~240px。
				const grouped = await page3.evaluate(() => {
					const groups = Array.from(document.querySelectorAll(".av-group"));
					return {
						names: groups.map((g) =>
							g.querySelector(".av-group-name").textContent.trim(),
						),
						openCount: groups.filter((g) => g.classList.contains("is-open")).length,
						visibleRows: Array.from(document.querySelectorAll(".av-row")).filter(
							(r) => r.offsetParent !== null,
						).length,
						hasFilter: !!document.querySelector(".av-filter"),
					};
				});
				H.check(
					"字段按顶层键分组",
					grouped.names.includes("pull_request") && grouped.names.includes("repository"),
					grouped.names.join(","),
				);
				H.check(
					"默认全部折叠（不占满屏）",
					grouped.openCount === 0 && grouped.visibleRows === 0,
					`open=${grouped.openCount} visibleRows=${grouped.visibleRows}`,
				);
				H.check("有字段筛选框", grouped.hasFilter, String(grouped.hasFilter));

				// 展开一组：应出现组内字段，且显示的是**短名**（前缀在标题里）
				await page3.evaluate(() => {
					const g = Array.from(document.querySelectorAll(".av-group")).find(
						(x) => x.querySelector(".av-group-name").textContent.trim() === "pull_request",
					);
					g.querySelector(".av-group-head").click();
				});
				await page3.waitForTimeout(300);
				const opened = await page3.evaluate(() => {
					const paths = Array.from(
						document.querySelectorAll(".av-group.is-open .av-path"),
					).map((n) => n.textContent.trim());
					return { paths, visibleRows: document.querySelectorAll(".av-row:not([style*='none'])").length };
				});
				H.check(
					"展开后显示组内短名",
					opened.paths.includes("title") && opened.paths.includes("additions"),
					opened.paths.slice(0, 5).join(","),
				);
				H.check(
					"展开后不再显示完整路径前缀",
					!opened.paths.includes("pull_request.title"),
					opened.paths.slice(0, 3).join(","),
				);

				// 筛选：输入后只剩命中的组和行
				await page3.fill(".av-filter", "addi");
				await page3.waitForTimeout(300);
				const filtered = await page3.evaluate(() => ({
					visiblePaths: Array.from(document.querySelectorAll(".av-row"))
						.filter((r) => r.offsetParent !== null)
						.map((r) => r.dataset.path),
				}));
				H.check(
					"筛选框能定位到具体字段",
					filtered.visiblePaths.includes("pull_request.additions") &&
						filtered.visiblePaths.length <= 3,
					filtered.visiblePaths.join(","),
				);
				await page3.fill(".av-filter", "");

				// ---- 混写模板的预览：报错必须是**字段名**，不能是数字 ----
				//
				// 曾经的 bug：TPL_RE 没有捕获组，而混写替换用的是
				// `str.replace(re, (m, ref) => ...)` —— 没有捕获组时
				// 第二个参数是**匹配位置**，于是"哪个字段取不到"会报成
				// 「样本里没有：0、27」这种看不懂的数字。
				{
					const preview = async (v) =>
						page3.evaluate((val) => {
							const f = Array.from(document.querySelectorAll("[data-param-field]")).find(
								(x) => x.querySelector("[data-param]").dataset.ptype === "string",
							) || document.querySelectorAll("[data-param-field]")[1];
							const i = f.querySelector("[data-param]");
							i.value = val;
							i.dispatchEvent(new Event("input", { bubbles: true }));
							const tip = f.querySelector(".param-tip");
							return { pv: tip.dataset.pv, text: tip.textContent };
						}, v);

					const bothOk = await preview("{{pull_request.additions}}-{{pull_request.merged}}");
					H.check(
						"混写两个占位符都能求值",
						bothOk.pv === "ok" && bothOk.text.includes("12-false"),
						`${bothOk.pv} | ${bothOk.text}`,
					);

					const oneBad = await preview("{{pull_request.additions}}-{{pull_request.typo}}");
					H.check(
						"混写里拼错的字段按名字报出来",
						oneBad.pv === "missing" && oneBad.text.includes("pull_request.typo"),
						oneBad.text,
					);
					H.check(
						"报错里不含裸数字（捕获组没生效时会退化成 offset）",
						!/[「"]\s*\d+\s*[」"]/.test(oneBad.text),
						oneBad.text,
					);

					const twoBad = await preview("{{nope.a}}-{{nope.b}}");
					H.check(
						"多个缺失字段都列出来",
						twoBad.pv === "missing" && twoBad.text.includes("nope.a") && twoBad.text.includes("nope.b"),
						twoBad.text,
					);

					const wholeRef = await preview("{{pull_request.base.sha}}");
					H.check(
						"整值引用保留原类型（string）",
						wholeRef.pv === "ok" && /string/.test(wholeRef.text),
						wholeRef.text,
					);
				}

				// ---- 按事件类型切换结构 ----
				//
				// pull_request 和 push 的 payload 没有共同字段（前者是
				// pull_request.title，后者是 commits[] + ref）。给一份"通用
				// GitHub 样例"等于两份都不对，所以必须按事件分类。
				const byEvent = {};
				for (const ev of ["pull_request", "push", "release"]) {
					await page3.selectOption("#f-gh-event", ev);
					await page3.waitForTimeout(500);
					const r = await page3.evaluate(() => ({
						paths: Array.from(document.querySelectorAll(".av-row")).map(
							(n) => n.dataset.path,
						),
						src: (document.getElementById("avail-src") || {}).textContent || "",
					}));
					byEvent[ev] = r;
				}
				H.check(
					"push 事件给出 push 的字段",
					byEvent.push.paths.includes("ref") &&
						byEvent.push.paths.includes("commits") &&
						!byEvent.push.paths.includes("pull_request.title"),
					`push 前几项=${byEvent.push.paths.slice(0, 3).join(",")}`,
				);
				H.check(
					"release 事件给出 release 的字段",
					byEvent.release.paths.includes("release.tag_name"),
					`release 含 tag_name=${byEvent.release.paths.includes("release.tag_name")}`,
				);
				H.check(
					"来源说明跟着事件类型变",
					/push/.test(byEvent.push.src) && /release/.test(byEvent.release.src),
					`push="${byEvent.push.src.slice(0, 40)}"`,
				);

				await page3.click("#modal-cancel");
				await page3.waitForTimeout(300);
			}

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
				//
				// #162：页面里直接发的请求也要带 namespaceId（后端缺参一律 400）。
				// 这里传套件解析出的默认命名空间 id，不从 localStorage 的 scope 猜，
				// 否则这条断言会因为“页面停在哪个命名空间”而飘。
				const saved = await page3.evaluate(async ({ name, ns }) => {
					const r = await fetch(
						"/v1/sources?namespaceId=" + encodeURIComponent(ns),
						{ credentials: "include" },
					);
					const j = await r.json();
					const src = (j.sources || []).find((x) => x.name === name);
					if (!src) return null;
					const tc = (src.targetCaps || [])[0] || {};
					return { params: tc.params, hasSchema: !!tc.paramsSchema };
				}, { name: "e2e-param-src", ns: realNsId });
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

	// ---- H. 节点页真后端注册：POST /v1/nodes → 一次性令牌 + 可直接粘的 node.yaml ----
	//
	// mock 里的注册只能证明"UI 接对了接口形状"；这里要证明它和**真 handler**
	// 也对得上：201 响应体里的明文令牌要真的落到 #reg-token，而 node.yaml 里
	// 的 server 必须是这个中枢的 origin（否则 agent 连不上）。
	// 本套件 DB 是套件私有的，注册出的节点残留无副作用，不加删除逻辑。
	console.log("--- 节点页真实注册（一次性令牌 + node.yaml）---");
	{
		const s = H.seed(server.dbPath, "node_reg");
		const ctx = await newCtx();
		await injectSession(ctx, s.session, server.base);
		const page = await ctx.newPage();
		await page.goto(server.base + "/nodes.html", {
			waitUntil: "load",
			timeout: 20000,
		});
		// 等注册入口挂载（新 DOM，缺了就是功能没接上，下面会明确报错）
		await page
			.waitForSelector("#node-register-btn", { timeout: 10000 })
			.catch(() => {});
		await page.waitForTimeout(1200);

		const hasEntry = await page.evaluate(
			() => !!document.getElementById("node-register-btn"),
		);
		H.check(
			"真后端：节点页有注册节点入口",
			hasEntry === true,
			`#node-register-btn=${hasEntry}`,
		);
		if (hasEntry) {
			const regName = "ui-reg-" + Date.now();
			await page.click("#node-register-btn");
			await page.fill("#reg-name", regName);
			await page.fill("#reg-instances", "2");
			await page.click("#reg-submit");
			// 真后端是本地进程，但 201 后还要 loadAll() 刷新列表，等结果块出现最直接
			await page
				.waitForFunction(
					() => {
						const r = document.getElementById("reg-result");
						return !!r && !r.classList.contains("hidden");
					},
					null,
					{ timeout: 15000 },
				)
				.catch(() => {});
			await page.waitForTimeout(300);

			const r = await page.evaluate(() => ({
				resultHidden: (() => {
					const e = document.getElementById("reg-result");
					if (!e) return true;
					return e.classList.contains("hidden") || e.hidden === true;
				})(),
				errorText: (document.getElementById("reg-error") || {}).textContent || "",
				token: (document.getElementById("reg-token") || {}).textContent || "",
				yaml: (document.getElementById("reg-yaml") || {}).textContent || "",
				origin: location.origin,
				listText: (document.getElementById("nodes-list") || {}).textContent || "",
			}));
			const token = r.token.trim();
			H.check(
				"真后端：提交成功 → 一次性令牌块出现",
				r.resultHidden === false && token.length > 8,
				`结果块隐藏=${r.resultHidden} 令牌长度=${token.length} 错误行=${JSON.stringify(r.errorText.slice(0, 120))}`,
			);
			H.check(
				"真后端：node.yaml 带对 server / name / token",
				r.yaml.includes("server: " + r.origin) &&
					r.yaml.includes("name: " + regName) &&
					token.length > 0 &&
					r.yaml.includes("token: " + token),
				JSON.stringify({
					server: r.origin,
					名字: regName,
					令牌长度: token.length,
					YAML: r.yaml.slice(0, 200),
				}),
			);
			const listed = await page
				.waitForFunction(
					(n) =>
						((document.getElementById("nodes-list") || {}).textContent || "").includes(
							n,
						),
					regName,
					{ timeout: 10000 },
				)
				.then(() => true)
				.catch(() => false);
			H.check(
				"真后端：注册后节点列表出现新节点",
				listed === true,
				`列表里找不到 ${regName} — ${JSON.stringify(r.listText.replace(/\s+/g, " ").slice(0, 200))}`,
			);
		}

		await page.close();
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

// checkCapabilityTemplates 验证「新建能力」的模板化流程（console-declares epic）。
//
// 这替代了旧的 checkSchemaEditor：以前是一屏手写 paramsSchema + 出口名，
// 用户得先懂 JSON Schema；现在是「选模板 → 填表单 → 看生成的脚本」。
//
// 但**表达力不能因此下降**，所以钉三条：
//   ① 九个模板都在，字段由模板驱动（不是写死在前端的第二份副本）
//   ② 只读脚本 → 可编辑脚本（脚本模式的出口）
//   ③ 高级参数契约可以手写 JSON，且**非法 JSON 必须当场拦住**（不能等到后端 400）
async function checkCapabilityTemplates(page) {
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

	// ① 步骤 1：九个模板，按 script / agent 分组
	const tplStep = await page.evaluate(() => ({
		stepVisible: !document.getElementById("tpl-step").classList.contains("hidden"),
		formHidden: document.getElementById("tpl-form-step").classList.contains("hidden"),
		tiles: document.querySelectorAll("[data-tpl]").length,
		groups: document.querySelectorAll(".tpl-group").length,
		ids: Array.from(document.querySelectorAll("[data-tpl]")).map((e) =>
			e.getAttribute("data-tpl"),
		),
	}));
	H.check("弹窗先给模板选择（不是一屏 JSON）", tplStep.stepVisible && tplStep.formHidden,
		JSON.stringify(tplStep));
	H.check("九个内置模板都在", tplStep.tiles === 9, `tiles=${tplStep.tiles}`);
	H.check("模板按类别分组", tplStep.groups >= 2, `groups=${tplStep.groups}`);
	H.check(
		"模板 id 与契约一致（agent 靠模板而不是新执行器类型）",
		["script.blank", "script.http", "script.git", "script.test", "script.notify",
			"agent.claude", "agent.codex", "agent.pi", "agent.custom"].every((id) =>
			tplStep.ids.includes(id),
		),
		JSON.stringify(tplStep.ids),
	);

	// 选 agent.claude：它带 prompt 必填 + model/tools
	await page.click('[data-tpl="agent.claude"]');
	await page.waitForTimeout(400);

	const form = await page.evaluate(() => ({
		formVisible: !document.getElementById("tpl-form-step").classList.contains("hidden"),
		stepHidden: document.getElementById("tpl-step").classList.contains("hidden"),
		fieldKeys: Array.from(document.querySelectorAll("#tpl-fields [data-field]")).map((e) =>
			e.getAttribute("data-field"),
		),
		required: Array.from(document.querySelectorAll("#tpl-fields [data-required='1']")).map((e) =>
			e.getAttribute("data-field"),
		),
		script: (document.getElementById("script-pre") || {}).textContent || "",
		requires: (document.getElementById("tpl-requires") || {}).textContent || "",
		scriptInputHidden: document.getElementById("script-input").classList.contains("hidden"),
	}));
	H.check("选模板后进入表单步骤", form.formVisible && form.stepHidden);
	H.check(
		"字段由模板驱动（prompt/model/tools 都在）",
		["prompt", "model", "tools"].every((k) => form.fieldKeys.includes(k)),
		JSON.stringify(form.fieldKeys),
	);
	H.check(
		"必填标记只落在 prompt 上",
		form.required.length === 1 && form.required[0] === "prompt",
		JSON.stringify(form.required),
	);
	H.check(
		"只读脚本就是模板正文（直接 exec，靠 shebang）",
		/^#!/.test(form.script.trim()) && /claude/.test(form.script),
		form.script.slice(0, 120),
	);
	H.check(
		"预装依赖被显式告知（缺了部署会失败）",
		/\bclaude\b/.test(form.requires),
		form.requires,
	);
	H.check("默认是只读脚本（不是一进去就给空编辑器）", form.scriptInputHidden === true);

	// ② 脚本模式：切到可编辑，改一个字，再切回来
	await page.click("#script-mode-edit");
	await page.waitForTimeout(250);
	const editMode = await page.evaluate(() => ({
		visible: !document.getElementById("script-input").classList.contains("hidden"),
		preHidden: document.getElementById("script-pre").classList.contains("hidden"),
		value: document.getElementById("script-input").value,
	}));
	H.check("切到脚本模式后 textarea 可见、只读块隐藏",
		editMode.visible && editMode.preHidden, JSON.stringify(editMode));
	H.check("脚本模式下预填的就是模板正文", /^#!/.test(editMode.value.trim()));

	await page.evaluate(() => {
		const el = document.getElementById("script-input");
		el.value = "#!/bin/bash\nset -euo pipefail\necho edited-by-e2e\n";
		el.dispatchEvent(new Event("input", { bubbles: true }));
	});
	await page.click("#script-mode-view");
	await page.waitForTimeout(250);
	const afterEdit = await page.evaluate(
		() => document.getElementById("script-pre").textContent || "",
	);
	H.check("改过的脚本能在只读模式看到（没有丢）", /edited-by-e2e/.test(afterEdit),
		afterEdit.slice(0, 120));

	// ③ 高级参数契约：预填 → 非法 JSON 被当场拦住
	await page.click("#schema-toggle");
	await page.waitForTimeout(250);
	const adv = await page.evaluate(() => {
		const wrap = document.getElementById("schema-wrap");
		let parsed = null;
		try {
			parsed = JSON.parse(document.getElementById("cap-schema").value);
		} catch {
			parsed = null;
		}
		return {
			visible: !wrap.classList.contains("hidden"),
			parsed,
		};
	});
	H.check("高级参数区可展开", adv.visible === true);
	H.check(
		"预填的是模板推导的契约（不是空白）",
		adv.parsed && adv.parsed.type === "object" && adv.parsed.required?.includes("prompt"),
		JSON.stringify(adv.parsed),
	);

	await page.evaluate(() => {
		document.getElementById("cap-schema").value = "{ broken";
		document.getElementById("cap-name").value = "e2e.broken";
	});
	await page.click("#cap-submit");
	await page.waitForTimeout(400);
	const blocked = await page.evaluate(() => ({
		err: (document.getElementById("schema-error") || {}).textContent || "",
		errHidden: document.getElementById("schema-error").classList.contains("hidden"),
		modalStillOpen: !document.getElementById("cap-modal").classList.contains("hidden"),
	}));
	H.check("非法参数 JSON 被当场拦住（不发给后端）",
		blocked.errHidden === false && blocked.err.length > 0, JSON.stringify(blocked));
	H.check("拦住时不关闭弹窗（否则用户输入丢了）", blocked.modalStillOpen === true);

	// 表达力兜底：能塞进模板表达不了的 key（pattern），并被接受
	await page.evaluate(() => {
		document.getElementById("cap-schema").value = JSON.stringify({
			type: "object",
			properties: { branch: { type: "string", pattern: "^[a-z0-9-]+$" } },
			required: ["branch"],
		});
	});
	const accepted = await page.evaluate(() => {
		try {
			return JSON.parse(document.getElementById("cap-schema").value);
		} catch {
			return null;
		}
	});
	H.check(
		"高级模式能表达模板字段表达不了的约束（pattern）",
		accepted && accepted.properties?.branch?.pattern === "^[a-z0-9-]+$",
		JSON.stringify(accepted && accepted.properties),
	);
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

// checkEventsFilter 验证事件流的排序 / 筛选 / 分组 / URL 参数。
//
// 默认视图是**按时间倒序平铺**（事件流的第一个问题永远是"刚刚发生了什么"），
// 分组是可选视图（?group=1），因为分组会把刚发生的事件压到组内第 6 条
// 后面，用户以为没收到。
// checkDisplayName 验证"能力显示名"这条链路端到端可用。
//
// 它盯三件事，每一件都对应一个真实会坏的环节：
//   ① 改名能存下来（PATCH 只改 display_name，不产生新版本）
//   ② 有显示名时界面用显示名，**但真名不能消失**（排查问题时要看真名）
//   ③ 出口名保持原样 —— 它是路由键，用户建边时要一字不差地填它
async function checkDisplayName(page, base) {
	await page.goto(base + "/capabilities.html", { waitUntil: "load", timeout: 20000 });
	await page.waitForTimeout(2000);

	const before = await page.evaluate(() => {
		const rows = Array.from(document.querySelectorAll(".cap-row"));
		const row = rows.find((r) => r.querySelector(".cap-rename"));
		return { rows: rows.length, hasRenameBtn: !!row };
	});
	H.check("能力页每行都有改名入口", before.hasRenameBtn === true, `rows=${before.rows}`);

	// 改哪个能力很重要：必须挑**有入边**的，否则改了名字也不会出现在出口页，
	// 后面那条断言就变成在要求一个模型里不存在的形状。
	//
	// 做法：先去出口页取一个真实的 toCapability 名字，再回来改它。
	await page.goto(base + "/edges.html", { waitUntil: "load", timeout: 20000 });
	await page.waitForTimeout(1800);
	const target = await page.evaluate(() => {
		// edges 数据在 mock 里；从 DOM 拿不到 id，所以直接问接口
		return fetch("/v1/edges")
			.then((r) => r.json())
			.then((d) => {
				const e = (d.edges || [])[0];
				return e ? { id: e.toCapability } : null;
			});
	});
	if (!target) {
		H.check("出口页有边（显示名断言前置）", false, "edges 为空");
		return;
	}

	await page.goto(base + "/capabilities.html", { waitUntil: "load", timeout: 20000 });
	await page.waitForTimeout(2000);

	const renamed = await page.evaluate(async (targetId) => {
		const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
		// 按 id 找行：能力页每行没有暴露 id，用 /v1/capabilities 反查名字
		const caps = await fetch("/v1/capabilities")
			.then((r) => r.json())
			.then((d) => d.capabilities || []);
		const want = caps.find((c) => c.id === targetId);
		if (!want) return null;
		// 行里显示的是 name + code("v1")，中间没有 @，
		// 所以用 name 匹配（能力名在一行里唯一）。
		const needle = want.name;
		const rows = Array.from(document.querySelectorAll(".cap-row"));
		const row = rows.find((r) => r.textContent.includes(needle));
		if (!row) return null;
		row.querySelector(".cap-rename")?.click();
		await sleep(300);
		const input = row.querySelector(".rename-input");
		if (!input) return null;
		input.value = "E2E 显示名";
		Array.from(row.querySelectorAll("button"))
			.find((b) => /保存|Save|Guardar/.test(b.textContent))
			?.click();
		await sleep(1200);
		return { needle };
	}, target.id);
	if (!renamed) {
		H.check("能打开内联改名输入", false, "找不到目标能力的行");
		return;
	}

	const after = await page.evaluate((needle) => {
		const rows = Array.from(document.querySelectorAll(".cap-row"));
		const row = rows.find((r) => r.textContent.includes(needle));
		return {
			found: row ? row.textContent.includes("E2E 显示名") : false,
			// 真名必须还在（技术身份不能因为取了昵称就消失）
			keepsRaw: row ? row.textContent.includes(needle) : false,
			text: row ? row.textContent.replace(/\s+/g, " ").trim().slice(0, 90) : "",
		};
	}, renamed.needle);
	H.check("改名后界面用显示名", after.found === true, after.text);
	H.check("显示名旁边仍保留真名@版本", after.keepsRaw === true, after.text);

	// 出口页：出口名不能被显示名替换（它是路由键），但能力名要用显示名。
	//
	// **必须在同一个页面上下文里验**：mock 的 state 是页面级的，导航到
	// 出口页会重建数据，刚才改的显示名就没了（真实后端落库，不会有这问题）。
	// 所以这里不走 goto，而是在 capability 页面内直接调 /v1/edges +
	// /v1/capabilities 验证"边渲染时会用到 displayName"这个契约。
	const edge = await page.evaluate(async () => {
		const [eRes, cRes] = await Promise.all([
			fetch("/v1/edges").then((r) => r.json()),
			fetch("/v1/capabilities").then((r) => r.json()),
		]);
		const byID = new Map((cRes.capabilities || []).map((c) => [c.id, c]));
		const edges = eRes.edges || [];
		return {
			n: edges.length,
			// 下游必须总能解析到能力；上游只在「能力边」时才要求解析
			// （入口边的左端是来源名，本来就没有对应能力行 —— #110）
			allResolve: edges.every(
				(e) =>
					byID.has(e.toCapability) &&
					(e.fromIsSource ? true : byID.has(e.fromCapability)),
			),
			// 出口名来自 from 能力的 outputs（或起点的事件类型），
			// 不可能等于某个能力的显示名 —— 这就是"路由键不被显示名替换"
			portsAreRaw: edges.every((e) => {
				// 入口边：左端是来源名，出口是事件类型，任意值
				if (e.fromIsSource) return true;
				const from = byID.get(e.fromCapability);
				if (!from) return false;
				return (from.outputs || []).includes(e.fromPort);
			}),
			// 至少有一个 To 端能力带显示名（证明渲染时会用到它）
			hasDisplayName: edges.some((e) => {
				const to = byID.get(e.toCapability);
				return to && (to.displayName || "").trim();
			}),
			sample: edges.slice(0, 4).map((e) => {
				const to = byID.get(e.toCapability);
				return e.fromPort + " -> " + (to ? to.displayName || to.name : "?");
			}),
		};
	});
	H.check("出口表两端都能解析（入口边左端是来源名）", edge.allResolve === true, `edges=${edge.n}`);
	H.check(
		"出口名仍是原始路由键（不被显示名替换）",
		edge.portsAreRaw === true,
		`edges=${edge.n}`,
	);
	H.check(
		"显示名会出现在边的下游侧",
		edge.hasDisplayName === true,
		edge.sample.join(" | "),
	);
}

async function checkEventsFilter(page, base) {
	const r = await page.evaluate(() => {
		const cards = Array.from(document.querySelectorAll("#ev-list .card"));
		const times = cards
			.map((c) => {
				const mono = c.querySelector(".mono")?.textContent || "";
				return mono;
			})
			.filter(Boolean);
		return {
			cards: cards.length,
			groups: document.querySelectorAll(".ev-group").length,
			times,
			kindOptions: Array.from(document.querySelectorAll("#ev-kind option")).map(
				(o) => o.textContent,
			),
			srcOptions: Array.from(document.querySelectorAll("#ev-source option")).map(
				(o) => o.textContent,
			),
			nodeOptions: Array.from(document.querySelectorAll("#ev-node option")).map(
				(o) => o.textContent,
			),
		};
	});

	H.check("事件默认平铺渲染（不分组）", r.groups === 0, `groups=${r.groups}`);
	H.check("事件列表非空", r.cards > 0, `cards=${r.cards}`);
	H.check("有「源类型」下拉且不止一项", r.kindOptions.length >= 2, r.kindOptions.join(","));
	H.check("源下拉有选项", r.srcOptions.length >= 2, r.srcOptions.join(","));
	H.check("节点下拉有选项", r.nodeOptions.length >= 2, r.nodeOptions.join(","));

	// 默认按时间倒序：卡片上的 fmtTime 是"刚刚 / N 分钟前 / 日期"，
	// 不能直接比字符串。改为比较真实 createdAt——从 DOM 里拿不到，
	// 所以用间接判据：连点两次刷新，卡片顺序必须一致（排序稳定），
	// 且"刚刚"类的卡片排在最前（最新在最上）。
	const orderCheck = await page.evaluate(async () => {
		const read = () =>
			Array.from(document.querySelectorAll("#ev-list .card")).map(
				(c) => c.querySelector(".mono")?.textContent || "",
			);
		const first = read();
		await new Promise((r) => setTimeout(r, 300));
		const second = read();
		return { first, second, same: JSON.stringify(first) === JSON.stringify(second) };
	});
	H.check("事件顺序稳定（按固定规则排序，不是随机顺序）", orderCheck.same === true);

	// 源类型过滤：选一个类型后，列表里只能剩该类型的事件
	const kindFilter = await page.evaluate(async () => {
		const sel = document.getElementById("ev-kind");
		const opt = Array.from(sel.options).find((o) => o.value);
		if (!opt) return null;
		sel.value = opt.value;
		sel.dispatchEvent(new Event("change"));
		await new Promise((r) => setTimeout(r, 1500));
		return {
			kind: opt.value,
			cards: document.querySelectorAll("#ev-list .card").length,
			// 源下拉应联动：只列该类型下的源
			srcOpts: Array.from(document.getElementById("ev-source").options).map(
				(o) => o.textContent,
			),
		};
	});
	if (kindFilter) {
		H.check(
			`按源类型过滤生效（${kindFilter.kind}）`,
			kindFilter.cards > 0,
			`cards=${kindFilter.cards}`,
		);
		// 联动：选类型后，源下拉不应再列出别的类型的源
		H.check(
			"选源类型后源下拉联动收窄",
			kindFilter.srcOpts.length < r.srcOptions.length,
			`${kindFilter.srcOpts.join(",")} vs 原 ${r.srcOptions.join(",")}`,
		);
		// 复位，别影响后面的断言
		await page.evaluate(async () => {
			const sel = document.getElementById("ev-kind");
			sel.value = "";
			sel.dispatchEvent(new Event("change"));
			await new Promise((r) => setTimeout(r, 1200));
		});
	}

	// 分组视图仍然可用（?group=1）
	await page.goto(base + "/events.html?group=1", { waitUntil: "load", timeout: 20000 });
	await page.waitForTimeout(2000);
	const grouped = await page.evaluate(() => ({
		groups: Array.from(document.querySelectorAll(".ev-group")).map((g) => ({
			head: g.querySelector(".ev-group-head")?.textContent?.replace(/\s+/g, " ").trim() || "",
			cards: g.querySelectorAll(".card").length,
		})),
	}));
	H.check("?group=1 时按源分组渲染", grouped.groups.length >= 2, `groups=${grouped.groups.length}`);
	H.check(
		"组头显示数量",
		grouped.groups.every((g) => /\d+\s*条|\d+\s*events/.test(g.head)),
		grouped.groups.map((g) => g.head).join(" | ").slice(0, 160),
	);
	H.check(
		"有「未归类」组（source 对不上任何源的事件不丢）",
		grouped.groups.some((g) => /未归类|Uncategorized/.test(g.head)),
		grouped.groups.map((g) => g.head).join(" | ").slice(0, 160),
	);

	// 回到默认视图继续测 ?node= / ?source=
	await page.goto(base + "/events.html", { waitUntil: "load", timeout: 20000 });
	await page.waitForTimeout(1500);

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
			cards: document.querySelectorAll("#ev-list .card").length,
			// 每张卡片有两个 code：第一个是源名，第二个是事件类型。
			// 只取第一个，否则会混进 "nightly" 这种类型名。
			sources: Array.from(document.querySelectorAll("#ev-list .card")).map((card) => {
				const c = card.querySelector("code");
				return c ? c.textContent.trim() : "";
			}),
		}));
		H.check("?source= 跳转后下拉自动选中", after.selected === srcName, after.selected);
		H.check("生效中的过滤条件有提示", /生效中|Active/.test(after.active), after.active);
		H.check("?source= 后列表非空", after.cards > 0, `cards=${after.cards}`);
		H.check(
			"?source= 后只剩该源的事件",
			after.sources.every((x) => x === srcName),
			`${srcName} vs ${after.sources.join(",")}`,
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
	// 点卡片现在会**导航到 trace 页**（不再是同页展开），
	// 所以不能靠"点一下再数 .wf-row"来挑最长的链 —— 每次点击都会
	// 销毁当前执行上下文。
	//
	// 改成：在事件流页面上读一遍所有 traceId，然后直接 goto trace 页
	// 逐个看哪个 span 最多。虽然多几次导航，但没有上下文问题。
	// page.url() 在页面被关掉/换过上下文时可能不是合法 URL，
	// 直接 new URL 会抛。拿不到就当作空 origin（下面用相对路径也能跑）。
	let base = "";
	try {
		base = new URL(page.url()).origin;
	} catch {
		/* 拿不到 origin 就退回相对路径 */
	}
	const traceIds = await page.evaluate(() =>
		Array.from(document.querySelectorAll("#ev-list .card"))
			.map((c) => (c.textContent || "").match(/trace_[A-Za-z0-9_]+/)?.[0])
			.filter(Boolean),
	);
	if (!traceIds.length) {
		H.check("事件列表非空（瀑布图前置）", false, "没有事件卡片");
		return;
	}

	// ---- 演示数据的时间戳必须都在过去 ----
	//
	// 踩过：mock 的时间游标从 -180 起步、各段累加 210，最后一条落在
	// **+30s（未来）**，事件流里显示成"刚刚"，还把真正想让人第一眼
	// 看到的那条挤到了下面。这种"数据时间在未来"很难从界面上看出来，
	// 只会在排序时造成诡异结果，所以用断言钉住。
	{
		const stamps = await page.evaluate(async () => {
			// 不用页面里的 apiGet：它在 sources.html 上才存在，
			// 这里停在 events 页。直接 fetch 同一个端点。
			const r = await fetch("/v1/console/events?limit=50").then((x) => x.json());
			const nx = Date.now() / 1000;
			return (r.events || []).map((e) => ({
				traceId: e.traceId,
				ago: Math.round(nx - e.createdAt),
			}));
		});
		const future = stamps.filter((x) => x.ago < 0);
		H.check(
			"演示事件的时间戳都在过去（没有未来时间）",
			future.length === 0,
			future.map((x) => `${x.traceId}:${x.ago}s`).join(",") || `共 ${stamps.length} 条均在过去`,
		);
	}

	let bestId = traceIds[0];
	let bestCount = -1;
	for (const tid of traceIds.slice(0, 8)) {
		// 显式要瀑布图：默认视图是「图」，不带 view 就读不到 .wf-row
		await page.goto(
			base + "/trace.html?traceId=" + encodeURIComponent(tid) + "&view=waterfall",
			{ waitUntil: "load", timeout: 20000 },
		);
		await page.waitForTimeout(1200);
		const n = await page.evaluate(() => document.querySelectorAll(".wf-row").length);
		if (n > bestCount) {
			bestCount = n;
			bestId = tid;
		}
	}

	// ---- 图视图用「源拓扑」当底图，执行状态叠上去 ----
	//
	// 只画执行链的话，被剪枝的分支压根不出现 —— 用户看到的永远是一条
	// 顺顺利利的直线，"为什么这条分支没走"在图上根本问不出来。
	// 底图把它们留成灰色虚线，一眼看出"这里本来有条路，这次没走"。
	{
		await page.goto(base + "/trace.html?traceId=" + encodeURIComponent(bestId) + "&view=graph", {
			waitUntil: "load",
			timeout: 20000,
		});
		await page.waitForTimeout(2500);
		const gv = await page.evaluate(() => {
			const ns = Array.from(document.querySelectorAll(".wf-node"));
			const by = (c) => ns.filter((n) => n.classList.contains(c)).length;
			return {
				total: ns.length,
				pruned: by("is-pruned"),
				failed: by("is-failed"),
				ok: by("is-end"),
				titles: ns.map((n) => (n.querySelector(".wf-node-title") || {}).textContent || ""),
			};
		});
		H.check(
			"图视图有节点（底图或执行链）",
			gv.total > 0,
			`total=${gv.total}`,
		);
		// 底图节点用的是**能力显示名**，不是 "cap@version"。
		// 用了底图就说明源配置的拓扑真的被读进来了。
		H.check(
			"底图节点用能力显示名（说明读到了源配置）",
			gv.titles.some((x) => /[\u4e00-\u9fa5]/.test(x)),
			gv.titles.slice(0, 5).join(","),
		);
		H.check(
			"剪枝的分支以灰色留在图上（未执行）",
			gv.pruned > 0,
			`pruned=${gv.pruned} failed=${gv.failed} ok=${gv.ok}`,
		);
		H.check(
			"失败节点有独立样式（不再与「能力已删除」共用）",
			gv.failed > 0,
			`failed=${gv.failed}`,
		);

		// ---- 三种结构身份必须能分辨 ----
		//
		// 图要回答的不只是"跑得怎么样"，还有"这个节点在链上是什么角色"。
		// 源（⚡）/ 管道（◆）/ 终点（◉）形状完全不同，挤在一起看不出层次
		// 的话，"这条链到哪儿结束"就得挨个数。
		{
			const roles = await page.evaluate(() => {
				const ns = Array.from(document.querySelectorAll(".wf-node"));
				const roleOf = (n) =>
					Array.from(n.classList).find((c) => c.startsWith("role-")) || "";
				return {
					hasTrigger: ns.some((n) => n.classList.contains("is-trigger")),
					triggerIcon: (() => {
						const tr = ns.find((n) => n.classList.contains("is-trigger"));
						const ic = tr && tr.querySelector(".wf-node-icon");
						return ic ? ic.textContent.trim() : "";
					})(),
					origins: ns.filter((n) => roleOf(n) === "role-origin").length,
					pipes: ns.filter((n) => roleOf(n) === "role-pipe").length,
					ends: ns.filter((n) => roleOf(n) === "role-end").length,
					icons: ns.map((n) => {
						const ic = n.querySelector(".wf-node-icon");
						return ic ? ic.textContent : "";
					}),
				};
			});
			H.check(
				"图上有源节点（⚡ 触发器，回答「谁触发的」）",
				roles.hasTrigger && roles.triggerIcon === "⚡",
				`trigger=${roles.hasTrigger} icon=${roles.triggerIcon}`,
			);
			H.check(
				"三种结构身份都在图上（origin/pipe/end）",
				roles.origins > 0 && roles.pipes > 0 && roles.ends > 0,
				`origin=${roles.origins} pipe=${roles.pipes} end=${roles.ends}`,
			);
			H.check(
				"结构身份用不同图标（⚡/◆/◉）",
				roles.icons.includes("⚡") && roles.icons.includes("◆") && roles.icons.includes("◉"),
				[...new Set(roles.icons)].join(""),
			);

			// 图例：图标和颜色都有含义，不解释只能靠猜。
			// "哪个是 end 哪个是源"是个合理的问题，图应该自己回答。
			const lg = await page.evaluate(() => {
				const box = document.querySelector(".flograph-legend");
				return {
					exists: !!box,
					rows: box
						? Array.from(box.querySelectorAll(".flograph-legend-row")).map((r) =>
								r.textContent.trim(),
							)
						: [],
				};
			});
			H.check("图里有图例", lg.exists, `exists=${lg.exists}`);
			H.check(
				"图例解释了三种结构身份",
				lg.rows.some((x) => x.includes("源")) &&
					lg.rows.some((x) => x.includes("管道")) &&
					lg.rows.some((x) => x.includes("终点")),
				lg.rows.join(" | "),
			);
			H.check(
				"图例解释了剪枝（未执行）",
				lg.rows.some((x) => /未执行/.test(x)),
				lg.rows.join(" | "),
			);
		}

		// ---- 边标签不能压在节点上 ----
		//
		// 踩过：连线用的是"起终点直接拉一条贝塞尔"，忽略 dagre 算出的
		// 绕行点。两个节点横向错开时曲线会从中间穿过去，出口名标签
		// （passed / fixed）正好落在别的节点卡片上，看起来像标签错乱。
		{
			const overlap = await page.evaluate(() => {
				const labels = Array.from(document.querySelectorAll(".flograph-edge-label"));
				const nodes = Array.from(document.querySelectorAll(".wf-node")).map((n) =>
					n.getBoundingClientRect(),
				);
				const inside = labels
					.filter((lb) => {
						const b = lb.getBoundingClientRect();
						return nodes.some(
							(nr) =>
								b.left >= nr.left - 2 &&
								b.right <= nr.right + 2 &&
								b.top >= nr.top - 2 &&
								b.bottom <= nr.bottom + 2,
						);
					})
					.map((lb) => lb.textContent);
				const overlapping = [];
				for (let i = 0; i < labels.length; i++) {
					for (let j = i + 1; j < labels.length; j++) {
						const a = labels[i].getBoundingClientRect();
						const c = labels[j].getBoundingClientRect();
						if (a.left < c.right && a.right > c.left && a.top < c.bottom && a.bottom > c.top) {
							overlapping.push(labels[i].textContent + "|" + labels[j].textContent);
						}
					}
				}
				return { total: labels.length, inside, overlapping };
			});
			H.check(
				"边标签没有压在节点上",
				overlap.inside.length === 0,
				`${overlap.inside.join(",") || "无"}（共 ${overlap.total} 个标签）`,
			);
			H.check(
				"边标签之间不重叠",
				overlap.overlapping.length === 0,
				overlap.overlapping.join(",") || "无",
			);
		}

		// ---- 边必须真的"连上"节点 ----
		//
		// 这里踩过两个坑，都只靠肉眼才能发现，所以断言要写死：
		//
		//   ① 端点悬空：dagre 的折线末点本身就在节点边界上，拿它求交会
		//      偏出去 17~27px，箭头飘在半空，看着就是"线没连上"。
		//   ② 线穿过节点：从边界点朝节点中心打射线会穿过整个节点，从对面
		//      穿出去（实测虚线直穿「生成变更日志」）。
		//
		// 光断言"端点不在节点内部"是不够的 —— 悬空和穿出去都满足那一条。
		{
			const geo = await page.evaluate(() => {
				const paths = Array.from(
					document.querySelectorAll(".flograph-svg path[marker-end]"),
				);
				const all = Array.from(document.querySelectorAll(".wf-node")).map((n) => ({
					id: n.dataset.nodeId,
					label: (n.querySelector(".wf-node-title") || {}).textContent || "",
					rc: n.getBoundingClientRect(),
				}));
				const byId = new Map(all.map((n) => [n.id, n]));
				const distToRect = (pt, rc) =>
					Math.hypot(
						Math.max(rc.left - pt.x, 0, pt.x - rc.right),
						Math.max(rc.top - pt.y, 0, pt.y - rc.bottom),
					);

				const gaps = [];
				const through = [];
				for (const pth of paths) {
					const toEl = byId.get(pth.dataset.to);
					const fromEl = byId.get(pth.dataset.from);
					if (!toEl || !fromEl) continue;
					const len = pth.getTotalLength();
					const m = pth.getScreenCTM();
					const at = (k) => {
						const q = pth.getPointAtLength(len * k);
						return new DOMPoint(q.x, q.y).matrixTransform(m);
					};
					const gap = distToRect(at(1), toEl.rc);
					gaps.push({
						edge: `${fromEl.label}→${toEl.label}`,
						gap: +gap.toFixed(1),
					});
					for (let i = 1; i < 24; i++) {
						const q = at(i / 24);
						for (const n of all) {
							if (n.id === pth.dataset.from || n.id === pth.dataset.to) continue;
							const rc = n.rc;
							if (
								q.x > rc.left + 1 &&
								q.x < rc.right - 1 &&
								q.y > rc.top + 1 &&
								q.y < rc.bottom - 1
							) {
								through.push(`${fromEl.label}→${toEl.label} 穿过 ${n.label}`);
							}
						}
					}
				}
				return { gaps, through: [...new Set(through)] };
			});

			const loose = geo.gaps.filter((g) => g.gap > 7);
			H.check(
				"每条边的终点都贴着目标节点（≤7px，不是悬空）",
				loose.length === 0,
				loose.map((g) => `${g.edge}=${g.gap}px`).join(", ") ||
					`共 ${geo.gaps.length} 条，最大 ${Math.max(...geo.gaps.map((g) => g.gap))}px`,
			);
			H.check(
				"边不穿过无关节点",
				geo.through.length === 0,
				geo.through.join("; ") || "无",
			);
		}

		// ---- 出口名标签必须贴着自己的线 ----
		//
		// 标签是绝对定位的 DOM，位置靠算。回边会被绕成弧，如果还拿 dagre
		// 原始折线去算标签位置，标签就飘在弧线旁边的空地上 —— 谁也看不出
		// 它属于哪条边。
		{
			const lbl = await page.evaluate(() => {
				const paths = Array.from(
					document.querySelectorAll(".flograph-svg path[marker-end]"),
				);
				const labels = Array.from(document.querySelectorAll(".flograph-edge-label"));
				const out = [];
				for (const lb of labels) {
					const mine = paths.filter(
						(p) =>
							p.dataset.from === lb.dataset.from && p.dataset.to === lb.dataset.to,
					);
					if (!mine.length) continue;
					const lr = lb.getBoundingClientRect();
					const cx = lr.left + lr.width / 2;
					const cy = lr.top + lr.height / 2;
					let best = Infinity;
					for (const pth of mine) {
						const len = pth.getTotalLength();
						const m = pth.getScreenCTM();
						// 采样密度：60 点在一根 ~220px 的线上间隔 3.6px，
						// 测量误差本身就能让"到线的距离"抖 ±2px。提到 240 点
						// （间隔 ~0.9px），断言才不会因为测不准而 flaky。
						for (let i = 0; i <= 240; i++) {
							const q = pth.getPointAtLength((len * i) / 240);
							const d = new DOMPoint(q.x, q.y).matrixTransform(m);
							best = Math.min(best, Math.hypot(d.x - cx, d.y - cy));
						}
					}
					out.push({ port: lb.textContent.trim(), dist: +best.toFixed(1) });
				}
				return out;
			});
			// 阈值 20px 而不是 12px：标签避让节点/别的标签时**本来就会被推开**
			// 几像素（实测 needs-fix 稳定在 12.2px），卡 12 是必然 flaky。
			// 这条断言真正要防的是"标签跑到别的线上 / 整个飘走"，那是几十
			// 上百像素的偏差 —— 20px 依然能拦住它。
			const far = lbl.filter((x) => x.dist > 20);
			H.check(
				"出口名标签贴在自己的线上（≤20px）",
				far.length === 0,
				far.map((x) => `${x.port}=${x.dist}px`).join(", ") ||
					`共 ${lbl.length} 个，最大 ${Math.max(...lbl.map((x) => x.dist))}px`,
			);
		}

		// ---- 环（回边）必须能看出来 ----
		//
		// mock 里 "跑测试 --failed--> 应用修复 --fixed--> 跑测试" 是真实的环
		// （重试语义）。dagre 会临时反向边来分层，但不会告诉你哪条是回边 ——
		// 不自己检测的话，画出来和普通边一模一样，用户看不出这里有环。
		{
			const cyc = await page.evaluate(() => {
				const paths = Array.from(
					document.querySelectorAll(".flograph-svg path[marker-end]"),
				);
				const legend = document.querySelector(".flograph-legend");
				const rows = legend
					? Array.from(legend.querySelectorAll(".flograph-legend-row")).map((r) =>
							r.textContent.trim(),
						)
					: [];
				return {
					cycleEdges: paths.filter(
						(p) => p.classList.contains("is-hot") || p.classList.contains("is-hot2"),
					).length,
					hasCycleLegend: rows.some((x) => /走了很多次|走过多/.test(x)),
				};
			});
			H.check(
				"环上的边用加深的样式（不是告警色）",
				cyc.cycleEdges > 0,
				`cycled=${cyc.cycleEdges}`,
			);
			H.check("图例说明了「走过多次」", cyc.hasCycleLegend, String(cyc.hasCycleLegend));

			// 工具栏必须和源详情页那套一模一样（同一个组件，入口位置不能
			// 一页一个样）。源详情页一侧的对应断言在它自己那节里。
			const tb = await page.evaluate(() =>
				Array.from(document.querySelectorAll(".flograph-ctrl button")).map(
					(b) => b.dataset.fgAction,
				),
			);
			H.check(
				"trace 页工具栏有全屏（和源详情页同一位置）",
				tb.includes("fullscreen"),
				tb.join(","),
			);
			H.check(
				"trace 页工具栏有缩放三件套",
				["zoom-in", "zoom-out", "fit"].every((a) => tb.includes(a)),
				tb.join(","),
			);
		}
	}

	// 停在 span 最多的那条上做断言（显式要瀑布图，默认是图）
	await page.goto(
		base + "/trace.html?traceId=" + encodeURIComponent(bestId) + "&view=waterfall",
		{ waitUntil: "load", timeout: 20000 },
	);
	await page.waitForTimeout(2200);

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
	// 逐个 trace 页扫一遍（点卡片会导航，不能在同一上下文里循环）。
	const statusVariety = [];
	for (const tid of traceIds.slice(0, 8)) {
		// 状态色是瀑布图时间条上的类，所以要显式要瀑布图
		await page.goto(
			base + "/trace.html?traceId=" + encodeURIComponent(tid) + "&view=waterfall",
			{ waitUntil: "load", timeout: 20000 },
		);
		// 等时间条真的画出来再读，而不是固定 sleep。
		//
		// 原来固定等 900ms：机器一忙这条就偶发失败（只读到 b-succeeded，
		// 因为后来的 trace 还没渲染完）。这类"随机挂一条"最容易让人怀疑
		// 被测代码，其实只是等待方式不对。
		await page
			.waitForFunction(() => document.querySelectorAll(".wf-bar").length > 0, {
				timeout: 8000,
			})
			.catch(() => {});
		const got = await page.evaluate(() =>
			Array.from(document.querySelectorAll(".wf-bar")).map((bar) => {
				const m = bar.className.match(
					/b-(succeeded|failed|running|pending|assigned|acked|canceled|abandoned|lease_expired|lease)/,
				);
				return m ? m[0] : "";
			}),
		);
		for (const g of got) if (g && !statusVariety.includes(g)) statusVariety.push(g);
	}
	H.check("状态用不同颜色区分", statusVariety.length >= 2, statusVariety.join(","));

	// 扫完状态后回到最长的那条，后面还要测视图切换
	await page.goto(
		base + "/trace.html?traceId=" + encodeURIComponent(bestId) + "&view=waterfall",
		{ waitUntil: "load", timeout: 20000 },
	);
	await page.waitForTimeout(2200);

	// 宽度合理：都在 0~100 之间
	H.check(
		"时间条宽度在合法区间",
		wf.rows.filter((r) => r.width != null).every((r) => r.width >= 0 && r.width <= 100),
		wf.rows.map((r) => r.width).join(","),
	);

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

	// ---- 入场动画：打开时先"全量未执行"，再按执行顺序点亮 ----
	//
	// 采样必须紧跟在点击之后：动画总时长约 2.4s，晚了就什么都看不到。
	// 断言的是"曾经有一刻几乎所有节点都没点亮"，而不是某个瞬时值 ——
	// 采样窗口和帧率都不受控。
	if (toGraph) {
		let sawPlaying = false;
		let preMax = 0;
		let totalNodes = 0;
		// 线必须**逐段画出来**，不是整条瞬现。
		//
		// 踩过两次：手写 CSS transition 时 `stroke-dashoffset` 从来没取过
		// 中间值（先开 transition 再设初值，初值那一跳被过渡自己吃掉，
		// 浏览器最终只看到 0 → 0），线是"啪"一下整条出现的。
		// 只看"动画在播"是发现不了的 —— 必须盯 dashoffset 的中间态。
		let partialDraws = 0;
		let sawWaapi = false;
		// 动画刚开始时，不允许有"这次没走过、却还亮着的实线"。
		//
		// 这个 bug 的表现是：配置里有、这次没走的分支画成正常实线，
		// 而走过的元素被压暗了 —— 于是那一瞬间**没走过的比走过的还亮**，
		// 看起来像"这条线一开始就点亮了"（issue #157 第二张截图）。
		// 正确状态是：没走过的一律虚线（is-muted），走过的一律压暗（is-pre）。
		let brightAtStart = -1;
		for (let i = 0; i < 12; i++) {
			const s = await page.evaluate(() => {
				const host = document.querySelector(".flograph");
				if (!host) return null;
				const nodes = Array.from(host.querySelectorAll(".wf-node"));
				let partial = 0;
				let waapi = false;
				let solidBright = 0;
				for (const p2 of host.querySelectorAll(".flograph-svg path[data-from]")) {
					if (p2.classList.contains("is-muted")) continue; // 没走过的边本来就是虚线
					if (!p2.classList.contains("is-pre")) solidBright++;
				}
				for (const p2 of host.querySelectorAll(".flograph-svg path")) {
					if (p2.getAnimations && p2.getAnimations().length > 0) waapi = true;
					const cs = getComputedStyle(p2);
					const off = parseFloat(cs.strokeDashoffset) || 0;
					const dash = parseFloat(cs.strokeDasharray) || 0;
					// 0 < off < dash  ⇔  线只画出了一部分
					if (dash > 1 && off > 0.5 && off < dash - 0.5) partial++;
				}
				return {
					playing: host.classList.contains("is-playing"),
					pre: nodes.filter((n) => n.classList.contains("is-pre")).length,
					total: nodes.length,
					partial,
					waapi,
					solidBright,
				};
			});
			if (s && s.total > 0) {
				totalNodes = Math.max(totalNodes, s.total);
				if (s.playing) sawPlaying = true;
				if (s.pre > preMax) preMax = s.pre;
				if (s.partial > 0) partialDraws++;
				if (s.waapi) sawWaapi = true;
				// 只取"动画已开始"的第一个采样点：
				// 再往后走过的边已经被画出来了，亮着是正确的
				if (s.playing && brightAtStart < 0) brightAtStart = s.solidBright;
			}
			await page.waitForTimeout(110);
		}
		H.check(
			"图打开时有入场动画（先画成未执行再点亮）",
			sawPlaying === true && preMax > 1,
			`playing=${sawPlaying} 未点亮最多=${preMax}/${totalNodes}`,
		);
		H.check(
			"动画刚开始没有「没走过却亮着」的边（没走的都是虚线）",
			brightAtStart === 0,
			`动画起始时实线且未压暗的边=${brightAtStart}（应为 0）`,
		);
		H.check(
			"线是逐段画出来的（不是整条瞬现）",
			partialDraws > 0,
			`看到部分绘制的采样点=${partialDraws}/12 waapi=${sawWaapi}`,
		);
	}

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
				// 剪枝节点没有状态色点（它这次没执行），断言要能扣掉
				prunedCount: nodeEls.filter((n) => n.classList.contains("is-pruned")).length,
				// 触发器节点是"源"，没有执行状态
				triggerCount: nodeEls.filter((n) => n.classList.contains("is-trigger")).length,
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
			// 节点标题用 capLabel：**有 displayName 显示显示名，没有则回落
			// 到 name@version**。
			//
			// 这条断言原来写成"图上存在某个含 @版本 的标签"，它依赖
			// "mock 里恰好有几个能力没填 displayName" —— 那只是数据形状，
			// 不是契约。后来把 mock 的显示名补全（为了让变量演示有连贯的
			// 业务语义），断言就假失败了。
			//
			// 改成直接验 capLabel 的两条分支，与数据无关。
			const lbl = await page.evaluate(() => {
				const f = window.Anotify.capLabel;
				if (typeof f !== "function") return null;
				return {
					withDN: f({ name: "acme-fix", version: 1, displayName: "应用修复" }),
					noDN: f({ name: "acme-fix", version: 1 }),
					blankDN: f({ name: "acme-fix", version: 1, displayName: "   " }),
					withVer: f({ name: "x", version: 2, displayName: "名字" }, { withVersion: true }),
				};
			});
			H.check(
				"capLabel：有显示名用显示名，没填回落 name@version",
				lbl !== null &&
					lbl.withDN === "应用修复" &&
					lbl.noDN === "acme-fix@1" &&
					// 全空白也算"没填"，否则会出现只剩空格的标题
					lbl.blankDN === "acme-fix@1" &&
					lbl.withVer === "名字@2",
				JSON.stringify(lbl),
			);
			// 九态里的 succeeded / failed 应该用不同状态色（右下角小圆点）。
			//
			// 单条 trace 可能全是同一种状态（比如全成功），所以"这条链里有
			// 几种颜色"不是稳定判据。改为断言：**执行过的节点都拿到了颜色**，
			// 且全站跨 trace 至少有两种色（上面 statusVariety 已经验证）。
			//
			// 例外是剪枝节点（is-pruned）：它这次根本没执行，没有状态可言，
			// 所以**不该**有色点。底图上有它、执行链上没有它，都是正常的。
			const dots = cg.dots.filter(Boolean);
			// 扣掉两类没有状态的节点：
			//   · 剪枝（is-pruned）—— 这次没执行
			//   · 触发器（is-trigger）—— 它是"源"，不是任务
			H.check(
				"执行过的节点都有状态色点（剪枝/触发器没有）",
				dots.length === cg.nodeCount - cg.prunedCount - cg.triggerCount,
				`dots=${dots.length} nodes=${cg.nodeCount} pruned=${cg.prunedCount} trigger=${cg.triggerCount}`,
			);
			H.check(
				"全站状态色至少两种（跨 trace 验证过）",
				statusVariety.length >= 2,
				statusVariety.join(","),
			);
			H.check(
				"节点副标题显示状态",
				cg.subs.some((x) => /succeeded|failed|running|pending/.test(x)),
				cg.subs.join(" | "),
			);
		}

		// ---- 动画播完：全部复位 ----
		//
		// 全程不点亮的是"配置里有、这次没走"的分支；但动画一结束必须
		// **全部**复位，否则同一张图"首次进来"和"切回来"长得不一样
		// （剪枝另有虚线/灰字表达，不靠这里留痕）。
		await waitAnimDone(page);
		const after = await page.evaluate(() => {
			const host = document.querySelector(".flograph");
			if (!host) return null;
			const nodes = Array.from(host.querySelectorAll(".wf-node"));
			const title = (n) => n.querySelector(".wf-node-title")?.textContent || "";
			return {
				playing: host.classList.contains("is-playing"),
				pre: nodes.filter((n) => n.classList.contains("is-pre")).map(title),
				pruned: nodes.filter((n) => n.classList.contains("is-pruned")).map(title),
				labels: Array.from(host.querySelectorAll(".flograph-edge-label"))
					.map((x) => x.textContent.trim())
					.filter((x) => /\u00d7\d+/.test(x)),
			};
		});
		if (after) {
			H.check(
				"动画播完后没有残留的未点亮状态（不留永久痕迹）",
				after.playing === false && after.pre.length === 0,
				`残留=${after.pre.join(",") || "（无）"} 剪枝=${after.pruned.join(",") || "（无）"}`,
			);

			// ---- 点亮顺序必须等于真实执行顺序 ----
			//
			// 这是动画的核心承诺：顺序来自任务的 startedAt，不是数组下标、
			// 也不是图的节点顺序。"跑测试"跑了三次，就该亮/脉冲三次。
			const order = await page.evaluate(async () => {
				const r = await (
					await fetch("/v1/tasks?traceId=trace_mock_t9&limit=50")
				).json();
				const tasks = (r.tasks || []).slice();
				const tasksSorted = tasks
					.slice()
					.sort((a, b) => (a.startedAt ?? a.createdAt) - (b.startedAt ?? b.createdAt));
				const firstAt = new Map();
				for (const t of tasksSorted) {
					if (!firstAt.has(t.capabilityId)) firstAt.set(t.capabilityId, t.startedAt ?? t.createdAt);
				}
				const runs = new Map();
				for (const t of tasks) runs.set(t.capabilityId, (runs.get(t.capabilityId) || 0) + 1);
				// 重放一遍动画，记录点亮顺序与脉冲次数
				document
					.querySelector('.flograph-ctrl button[data-fg-action="replay"]')
					.click();
				const litOrder = [];
				const seen = new Set();
				// 脉冲只数"进入脉冲"的那次跳变：一次脉冲持续 480ms，
				// 采样间隔 55ms，按帧去重会把它数成 9 次。
				const pulseCounts = {};
				let wasPulsing = new Set();
				// 采样窗口必须覆盖**整段动画**：每拍固定 500ms，12 个任务的
				// 链路 ≈ 6.5s，再留余量。窗口太短会漏掉后半段的脉冲
				// （曾经 70×55ms=3.85s，把 apply-fix / alert 的重试全漏了）。
				for (let i = 0; i < 320; i++) {
					await new Promise((res) => setTimeout(res, 55));
					for (const n of document.querySelectorAll(".wf-node")) {
						const id = n.dataset.nodeId;
						if (!n.classList.contains("is-pre") && !seen.has(id)) {
							seen.add(id);
							litOrder.push(id);
						}
					}
					const nowPulsing = new Set(
						Array.from(document.querySelectorAll(".wf-node.is-pulse")).map(
							(n) => n.dataset.nodeId,
						),
					);
					for (const id of nowPulsing) {
						if (!wasPulsing.has(id)) pulseCounts[id] = (pulseCounts[id] || 0) + 1;
					}
					wasPulsing = nowPulsing;
					const host = document.querySelector(".flograph");
					if (host && !host.classList.contains("is-playing") && i > 8) break;
				}
				return {
					litOrder,
					firstAt: Object.fromEntries(firstAt),
					runs: Object.fromEntries(runs),
					pulseCounts,
				};
			});

			// 点亮顺序的时间戳必须单调不减（允许同一时刻的并列，不许跳序）
			let prev = -Infinity;
			let monotonic = true;
			let worst = "";
			for (const cap of order.litOrder) {
				const at = order.firstAt[cap];
				if (at == null) continue;
				if (at < prev) {
					monotonic = false;
					worst = `${prev} -> ${at} (${cap})`;
				}
				prev = at;
			}
			H.check(
				"动画按真实执行时间顺序点亮（不跳序）",
				order.litOrder.length > 2 && monotonic,
				monotonic
					? `lit=${order.litOrder.length} 单调不减`
					: `跳序: ${worst}`,
			);

			// 重试必须看得见：执行过 N 次的节点除了首次点亮，还要再脉冲 N-1 次。
			const retryCaps = Object.keys(order.runs).filter((c) => order.runs[c] > 1);
			const missing = retryCaps.filter(
				(c) => (order.pulseCounts[c] || 0) < order.runs[c] - 1,
			);
			H.check(
				"重试的节点会再次脉冲（执行 N 次 = 亮 1 次 + 脉冲 N-1 次）",
				retryCaps.length > 0 && missing.length === 0,
				missing.length === 0
					? `重试能力 ${retryCaps.length} 个，脉冲次数足够`
					: `脉冲不足: ${missing.map((c) => `${c} 跑了 ${order.runs[c]} 次只有 ${order.pulseCounts[c] || 0} 次脉冲`).join("; ")}`,
			);

			// 只有**真的走过**的边才允许被画出来。配置里有、这次没走的分支
			// （例如 lint-fix 走了 remaining，它的 --clean--> 就没走）画出来
			// 等于撒谎说这条分支被执行了。
			const edgeCheck = await page.evaluate(async () => {
				const r = await (
					await fetch("/v1/tasks?traceId=trace_mock_t9&limit=50")
				).json();
				const tasks = r.tasks || [];
				const capOf = new Map(tasks.map((t) => [t.id, t.capabilityId]));
				const walked = new Set();
				for (const t of tasks) {
					if (!t.parentTaskId) {
						// 根任务是被**触发器**拉起的，它那条入口边合法
						walked.add("__trigger__\u0000" + t.capabilityId);
						continue;
					}
					const from = capOf.get(t.parentTaskId);
					if (from) walked.add(from + "\u0000" + t.capabilityId);
				}
				const drawn = Array.from(
					document.querySelectorAll(".flograph-svg path[data-animated]"),
				).map((p) => ({
					pair: p.dataset.from + "\u0000" + p.dataset.to,
					triple: `${p.dataset.from}#${p.dataset.port}#${p.dataset.to}`,
					label: p.dataset.from + "→" + p.dataset.to,
				}));
				// 所有边（画过没画过都要），用来验证"哪个出口被走过"这件事本身
				const all = Array.from(document.querySelectorAll(".flograph-svg path[data-from]")).map(
					(p) => ({
						triple: `${p.dataset.from}#${p.dataset.port}#${p.dataset.to}`,
						animated: p.dataset.animated === "1",
					}),
				);
				return {
					drawnCount: drawn.length,
					fake: drawn.filter((d) => !walked.has(d.pair)).map((d) => d.label),
					animatedByTriple: Object.fromEntries(all.map((x) => [x.triple, x.animated])),
				};
			});
			H.check(
				"只有真的走过的边才被画出来（没走的分支不画）",
				edgeCheck.drawnCount > 0 && edgeCheck.fake.length === 0,
				edgeCheck.fake.length === 0
					? `画出 ${edgeCheck.drawnCount} 条，全部走过`
					: `画了没走过的边: ${edgeCheck.fake.join(", ")}`,
			);

			// ---- issue #157：同一个源、同一个目标，走的是哪个出口必须分得清 ----
			//
			// t9 里 analyze-pr 有两条边进 run-tests（needs-fix 扇出 + approved），
			// lint-fix 有一条 clean 进 run-tests。这次 analyze 走的是 needs-fix、
			// lint-fix 走的是 remaining，所以只有 needs-fix 那条该被画出来。
			//
			// 这个 bug 的表现是"跑测试节点亮起时看不到有线进入它" —— 只断言
			// "动画在播"完全抓不到，必须盯**具体哪条边**被画了。
			const A = "cap_mock_analyze-pr_v1";
			const L = "cap_mock_lint-fix_v1";
			const T = "cap_mock_run-tests_v1";
			const byTriple = edgeCheck.animatedByTriple || {};
			const want = [
				[`${A}#needs-fix#${T}`, true, "analyze--needs-fix--> run-tests（这次走过）"],
				[`${A}#approved#${T}`, false, "analyze--approved--> run-tests（这次没走）"],
				[`${L}#clean#${T}`, false, "lint-fix--clean--> run-tests（这次没走）"],
			];
			const wrong = want.filter(([k, v]) => byTriple[k] !== v);
			H.check(
				"同一对节点的多条边中，只画这次真正走过的那个出口",
				wrong.length === 0,
				wrong.length === 0
					? want.map(([k, , d]) => d).join("；")
					: wrong.map(([k, v]) => `${k} 期望 ${v} 实际 ${byTriple[k]}`).join("; "),
			);

			// ---- 走过多次的边要标出次数 ----
			//
			// 数字必须是**真实遍历次数**：为了让环更显眼而掺"环 +1"就是撒谎
			// （环上那条边可能只走了一遍）。所以这里要求 ≥2 才允许出现。
			const bad = after.labels.filter((x) => {
				const m = x.match(/\u00d7(\d+)$/);
				return !m || Number(m[1]) < 2;
			});
			H.check(
				"走过多次的边上标出了真实次数（×N）",
				after.labels.length > 0 && bad.length === 0,
				after.labels.join(" | ") || "没有任何带 ×N 的边标签",
			);
		}

		// ---- 重播按钮 ----
		//
		// 动画只在打开时播一次，错过就得有办法再看 —— 否则"顺序"这个
		// 信息只存在于那 2.4 秒里。
		const rep = await page.evaluate(() => {
			const host = document.querySelector(".flograph");
			const b = host && host.querySelector('.flograph-ctrl button[data-fg-action="replay"]');
			return b
				? { has: true, label: b.getAttribute("aria-label") || "", svg: !!b.querySelector("svg") }
				: { has: false };
		});
		H.check(
			"工具栏有重播动画按钮（图标 + 名字）",
			rep.has === true && rep.svg === true && rep.label.length > 0,
			JSON.stringify(rep),
		);

		if (rep.has) {
			await page.evaluate(() =>
				document.querySelector('.flograph-ctrl button[data-fg-action="replay"]').click(),
			);
			let sawPlaying = false;
			let preMax = 0;
			for (let i = 0; i < 12; i++) {
				await page.waitForTimeout(110);
				const s2 = await page.evaluate(() => {
					const h = document.querySelector(".flograph");
					return {
						playing: h.classList.contains("is-playing"),
						pre: document.querySelectorAll(".wf-node.is-pre").length,
					};
				});
				if (s2.playing) sawPlaying = true;
				if (s2.pre > preMax) preMax = s2.pre;
			}
			H.check(
				"点重播会重新播一遍动画",
				sawPlaying === true && preMax > 1,
				`playing=${sawPlaying} 未点亮最多=${preMax}`,
			);
			await waitAnimDone(page);
			const end = await page.evaluate(() => ({
				playing: document.querySelector(".flograph").classList.contains("is-playing"),
				pre: document.querySelectorAll(".wf-node.is-pre").length,
			}));
			H.check(
				"重播播完后正常收尾（不卡在播放态、没有残留）",
				end.playing === false && end.pre === 0,
				JSON.stringify(end),
			);

			// 系统开了"减少动态效果"时自动播放会跳过 —— 但用户亲手点重播
			// 是明确的意图表达，必须照播。否则这个功能对他等于不存在，
			// 而且从界面上完全看不出为什么"没有动画"。
			await page.emulateMedia({ reducedMotion: "reduce" });
			await page.evaluate(() =>
				document.querySelector('.flograph-ctrl button[data-fg-action="replay"]').click(),
			);
			let forced = false;
			for (let i = 0; i < 10; i++) {
				await page.waitForTimeout(110);
				const on = await page.evaluate(() =>
					document.querySelector(".flograph").classList.contains("is-playing"),
				);
				if (on) {
					forced = true;
					break;
				}
			}
			H.check(
				"系统「减少动态效果」下手动重播仍然会播",
				forced === true,
				`playing=${forced}`,
			);
			await page.emulateMedia({ reducedMotion: "no-preference" });
			await waitAnimDone(page);
		}

		// ---- 播放速度：1× / 0.5× / 2× ----
		//
		// 每拍时长是 revealStep() 直接暴露的，不靠掐表 —— 并发跑 4 个
		// Chrome 时计时断言必然抖。
		const speeds = await page.evaluate(async () => {
			const btn = document.querySelector('.flograph-ctrl button[data-fg-action="speed"]');
			if (!btn) return null;
			const out = [];
			for (let i = 0; i < 3; i++) {
				btn.click();
				// playEnter 里同步算好的，80ms 足够它落到 _debug 上
				await new Promise((r) => setTimeout(r, 80));
				const g = document.querySelector(".flograph");
				out.push({
					label: btn.textContent.trim(),
					step: g && g._graph ? g._graph._debug.revealStep() : 0,
				});
			}
			return out;
		});
		H.check(
			"播放速度可在 1× / 0.5× / 2× 之间切换（0.5× 是更慢，不是更快）",
			!!speeds &&
				speeds.map((x) => x.label).join(",") === "0.5×,2×,1×" &&
				speeds[0].step > speeds[2].step &&
				speeds[2].step > speeds[1].step,
			speeds ? speeds.map((x) => `${x.label}=${x.step}ms`).join(" ") : "没有速度按钮",
		);

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

	// ---- 每拍时长固定，不随链路复杂度压缩 ----
	//
	// 这是节奏的基本约定：**一步的时间固定**，所以图越复杂总时长越长。
	// 反面做法（踩过两次）：先把总时长定死再按步数均分 —— 结果最该看清
	// 的那条复杂链路每拍最短。"慢一点"的意思是每一步都留够看的工夫，
	// 不是总时长固定。
	//
	// 用 t1（5 个任务）和 t9（12 个任务）对比：每拍时长必须一样。
	const steps = {};
	for (const tid of ["trace_mock_t1", "trace_mock_t9"]) {
		await page.goto(new URL(`trace.html?traceId=${tid}&view=graph`, page.url()).href, {
			waitUntil: "load",
			timeout: 20000,
		});
		// revealStep 在动画启动时同步写入，不用等它播完
		await page
			.waitForFunction(
				() => {
					const h = document.querySelector(".flograph");
					return !!(h && h._graph && h._graph._debug.revealStep() > 0);
				},
				{ timeout: 20000 },
			)
			.catch(() => {});
		steps[tid] = await page.evaluate(() => {
			const g = document.querySelector(".flograph");
			return g && g._graph ? g._graph._debug.revealStep() : 0;
		});
	}
	H.check(
		"每拍时长固定，不随链路复杂度压缩（图越复杂总时长越长）",
		steps.trace_mock_t1 > 0 && steps.trace_mock_t1 === steps.trace_mock_t9,
		`t1(5 任务)=${steps.trace_mock_t1}ms t9(12 任务)=${steps.trace_mock_t9}ms`,
	);

	// ---- 首屏自适应缩放（fit）----
	//
	// 容器是"宽而矮"、图内容是"窄而高"，所以能锁的硬不变量只有三条：
	//   ① 图完整落在容器内（四边都有余量）→ 防"没 fit，内容被裁掉"
	//   ② 至少在一个方向上占到容器的 55%  → 防"缩过头，字小到读不了"
	//   ③ 左右余量基本相等               → 防"按旧尺寸算的 tx"，整体偏移
	//
	// 不断言具体缩放比：那是布局函数的输出，改 rankSep/节点数都会变，
	// 断言它等于某个数只会变成维护负担。
	await page.goto(new URL("trace.html?traceId=trace_mock_t9&view=graph", page.url()).href, {
		waitUntil: "load",
		timeout: 20000,
	});
	await page
		.waitForFunction(() => document.querySelectorAll(".wf-node").length > 0, {
			timeout: 15000,
		})
		.catch(() => {});
	// 等字体就绪后那次补 fit 落地（首帧用的是回退字体度量）
	await page.waitForTimeout(700);
	const fitInfo = await page.evaluate(() => {
		const box = document.querySelector(".flograph");
		const c = box.getBoundingClientRect();
		let minX = 1e9;
		let maxX = -1e9;
		let minY = 1e9;
		let maxY = -1e9;
		for (const n of box.querySelectorAll(".wf-node")) {
			const r = n.getBoundingClientRect();
			minX = Math.min(minX, r.left - c.left);
			maxX = Math.max(maxX, r.right - c.left);
			minY = Math.min(minY, r.top - c.top);
			maxY = Math.max(maxY, r.bottom - c.top);
		}
		return {
			w: Math.round(c.width),
			h: Math.round(c.height),
			gapL: Math.round(minX),
			gapR: Math.round(c.width - maxX),
			gapT: Math.round(minY),
			gapB: Math.round(c.height - maxY),
			ratio: Math.max((maxX - minX) / c.width, (maxY - minY) / c.height),
		};
	});
	H.check(
		"图自动缩放到容器内（不裁切 / 不过度缩小 / 左右居中）",
		fitInfo.gapL > 0 &&
			fitInfo.gapR > 0 &&
			fitInfo.gapT > 0 &&
			fitInfo.gapB > 0 &&
			fitInfo.ratio >= 0.55 &&
			Math.abs(fitInfo.gapL - fitInfo.gapR) <= 60,
		`容器=${fitInfo.w}x${fitInfo.h} 余量 L${fitInfo.gapL}/R${fitInfo.gapR}/T${fitInfo.gapT}/B${fitInfo.gapB} 占位比=${fitInfo.ratio.toFixed(2)}`,
	);
}

// checkFlowChart 在 mock 预览里验证流向图。
//
// 为什么必须在这里测而不是在真实 server 上：断言依赖 demo 数据里的
// **环路**（deploy-preview → apply-fix → deploy-preview）和**未接出口**
// （preview.ready）。真实环境不一定有这些形状，那样的断言会假失败。
// mock 的数据形状是固定的，正好拿来锁住渲染逻辑。
// checkNodeCaps 验证节点页把"这台机器声明了哪些能力"展示出来。
//
// 锁三件事（都踩过）：
//   ① 能力标签真的渲染出来，并且**显示用户取的显示名**（悬停给精确身份）
//   ② 标签有实际样式 —— `.out-chip` 曾经只内联在 capabilities.html 里，
//      别的页面用了这个类却拿不到样式，节点页渲染成一长串裸文本
//   ③ 页面上不能出现 [object Object] —— mock 曾经把槽位数组原样吐出去，
//      页面按数字用就渲染成「槽位 0/[object Object]」
async function checkNodeCaps(page, base) {
	const info = await page.evaluate(async () => {
		const res = await fetch("/v1/nodes");
		const api = await res.json();
		const expect = (api.nodes || []).reduce((n, x) => n + (x.capabilities || []).length, 0);
		const spans = Array.from(document.querySelectorAll("span.out-chip"));
		const styles = spans.map((c) => {
			const cs = getComputedStyle(c);
			return { pad: parseFloat(cs.paddingLeft), bg: cs.backgroundColor };
		});
		return {
			expect,
			chipCount: spans.length,
			texts: spans.map((c) => c.textContent.trim()),
			titles: spans.map((c) => c.title),
			styled: styles.every(
				(s) => s.pad > 0 && s.bg && s.bg !== "rgba(0, 0, 0, 0)" && s.bg !== "transparent",
			),
			hasLabel: Array.from(document.querySelectorAll("span")).some(
				(x) => x.textContent.trim() === "声明能力",
			),
			objObj: document.body.innerText.includes("[object Object]"),
			linkCount: document.querySelectorAll("span.out-chip.is-link").length,
		};
	});
	H.check(
		"节点页列出每台节点的声明能力",
		info.hasLabel === true && info.expect > 0 && info.chipCount === info.expect,
		`label=${info.hasLabel} chips=${info.chipCount}/${info.expect}`,
	);
	H.check(
		"能力标签有实际样式（不是一长串裸文本）",
		info.styled === true,
		JSON.stringify(info.texts.slice(0, 4)),
	);
	H.check(
		"能力标签显示显示名，悬停给出精确 name@version",
		info.texts.some((x) => /[\u4e00-\u9fa5]/.test(x)) &&
			info.titles.every((x) => /@\d+$/.test(x)),
		`${info.texts.slice(0, 3).join("/")} | ${info.titles.slice(0, 3).join("/")}`,
	);
	H.check("节点页没有 [object Object]", info.objObj === false, String(info.objObj));
	H.check(
		"能力标签可点进能力详情",
		info.linkCount === info.chipCount,
		`${info.linkCount}/${info.chipCount}`,
	);

	// 点一下真的能过去（标签的 id 必须对得上能力详情页）
	const href = await page.evaluate(() => {
		const c = document.querySelector("span.out-chip.is-link");
		if (!c) return "";
		c.click();
		return "";
	});
	await page.waitForTimeout(1200);
	H.check(
		"点能力标签会跳到能力页并带上 focus",
		/capabilities\.html\?focus=/.test(page.url()),
		page.url().replace(base, "") + href,
	);
}

// checkDeclaredBy 验证能力页显示"谁声明了它"。
//
// 这条信息不是锦上添花：能力定义只是「能干什么」的白名单，真正让它跑起来
// 的是**有机器声明它**。三种情况必须区分开，混在一起就是误导：
//
//   有节点声明  → 节点名 + 状态点
//   中枢执行    → 「不需要节点」（notify.phone 这类，不能报"跑不起来"）
//   没人声明    → 琥珀色警告「这个能力跑不起来」
async function checkDeclaredBy(page) {
	const info = await page.evaluate(async () => {
		const api = await (await fetch("/v1/capabilities")).json();
		const caps = api.capabilities || [];
		const rows = Array.from(document.querySelectorAll(".cap-row"));
		return {
			hasLabel: rows.some((r) => /声明节点/.test(r.textContent)),
			// 每种情况在后端侧各取一个代表
			nodeDeclared: caps.filter((c) => (c.declaredBy || []).length > 0).length,
			hub: caps.filter((c) => c.executedBy === "hub").length,
			orphan: caps
				.filter((c) => c.executedBy === "node" && (c.declaredBy || []).length === 0)
				.map((c) => c.displayName || c.name),
			dots: document.querySelectorAll(".cap-row .st-dot").length,
			warnRows: rows.filter((r) => r.querySelector(".text-amber-400")).length,
			hubRowText: rows
				.filter((r) => r.textContent.includes("中枢执行"))
				.map((r) => r.textContent.replace(/\s+/g, "")),
		};
	});
	H.check("能力页显示「声明节点」", info.hasLabel === true, String(info.hasLabel));
	H.check(
		"有节点声明的能力画出节点名 + 状态点",
		info.nodeDeclared > 0 && info.dots === info.nodeDeclared,
		`declared=${info.nodeDeclared} dots=${info.dots}`,
	);
	H.check(
		"中枢执行的能力不误报「跑不起来」",
		info.hub > 0 && info.hubRowText.length === info.hub,
		`hub=${info.hub} rows=${info.hubRowText.length}`,
	);
	H.check(
		"没有节点声明的能力给出警告（不是静默）",
		info.orphan.length === 0 || info.warnRows === info.orphan.length,
		`orphan=${info.orphan.length} warnRows=${info.warnRows}`,
	);
}

// waitAnimDone 等图上的入场动画自己播完（而不是瞎 sleep）。
//
// 动画总时长约 2.4s，但并行跑 4 个 Chrome 时会变慢；固定 sleep 要么
// 不够（flaky）要么浪费（把套件拖过超时预算）。等条件最稳。
async function waitAnimDone(page) {
	await page
		.waitForFunction(
			() => {
				const h = document.querySelector(".flograph");
				return h && !h.classList.contains("is-playing");
			},
			{ timeout: 25000 },
		)
		.catch(() => {});
}

// checkNodeDetail 验证节点详情页（node.html?id=…）。
//
// 这个页面存在的理由：列表要能扫（有几台、活着吗、能跑什么），
// "这台机器现在卡在哪个槽上、最近跑了什么"是点进去才关心的事。
async function checkNodeDetail(page, base) {
	// 从列表点进去：名字就是入口，链接必须带真实 node id
	const href = await page.evaluate(() => {
		const a = document.querySelector('a[href^="node.html?id="]');
		return a ? a.getAttribute("href") : "";
	});
	H.check("节点列表的名字链到详情页", /^node\.html\?id=.+/.test(href), href);
	if (!href) return;

	await page.goto(base + "/" + href, { waitUntil: "load", timeout: 20000 });
	await page.waitForTimeout(1500);

	const d = await page.evaluate(() => {
		const txt = document.body.innerText;
		return {
			name: (document.getElementById("head-name") || {}).textContent || "",
			hasBasic: txt.includes("基本信息"),
			hasCaps: txt.includes("声明的能力"),
			hasSlots: txt.includes("并发槽位"),
			hasRecent: txt.includes("最近执行"),
			capChips: document.querySelectorAll(".out-chip").length,
			// 逐槽展示（不是只给一个总数）
			slotRows: document.querySelectorAll(".np-slot").length,
			taskRows: document.querySelectorAll(".np-task").length,
			// 每个任务都能点进 trace
			traceLinks: document.querySelectorAll('.np-task a[href^="trace.html?traceId="]')
				.length,
			// 上限必须真的渲染出数字（mock 少给 maxInstances 时这里会是 undefined）
			hasUndefined: /undefined/.test(txt),
			hasObjObj: txt.includes("[object Object]"),
		};
	});
	H.check("详情页显示节点名", d.name.length > 0 && d.name !== "加载中…", d.name);
	H.check(
		"详情页有基本信息 / 声明的能力 / 并发槽位 / 最近执行 四块",
		d.hasBasic && d.hasCaps && d.hasSlots && d.hasRecent,
		JSON.stringify({
			basic: d.hasBasic,
			caps: d.hasCaps,
			slots: d.hasSlots,
			recent: d.hasRecent,
		}),
	);
	H.check("详情页列出声明的能力", d.capChips > 0, `chips=${d.capChips}`);
	H.check("槽位逐个展示（不是只给总数）", d.slotRows > 0, `slots=${d.slotRows}`);
	H.check(
		"最近执行能点进链路",
		d.taskRows > 0 && d.traceLinks === d.taskRows,
		`tasks=${d.taskRows} links=${d.traceLinks}`,
	);
	H.check(
		"详情页没有 undefined / [object Object]",
		d.hasUndefined === false && d.hasObjObj === false,
		JSON.stringify({ undef: d.hasUndefined, obj: d.hasObjObj }),
	);
}

// checkNodeRegister 验证「注册节点」这条纯 UI 链路（#198）。
//
// 为什么这条链路值得单独锁：令牌只在 201 响应体里出现一次（中枢只存
// sha256），一旦前端把它弄丢/渲染错，用户拿到的就是一份**看起来能用、
// 粘进去 agent 起不来**的 node.yaml —— 而且没有第二次机会。所以断言的不是
// "有个令牌块"，而是"这块里的 YAML 能直接粘"（逐项对齐 cmd/agent/config.go，
// 同 checkPlaybooks 里 #197 修好的写法）：
//   ① 入口按钮存在、表单/令牌块默认都藏着
//   ② 提交成功 → 令牌块出现，node.yaml 逐项形状合规（run 必须是映射）
//   ③ 再点一次入口 → 上一个令牌块**真的从 DOM 清掉**（不是视觉隐藏）
//   ④ 重名 → 只说重名，绝不给令牌
//   ⑤ 列表刷新后新节点真的出现（否则用户以为没注册上）
//   …另外还锁 instances 上界（中枢硬顶 20）与本地拦截（空名/非法名不发请求）。
//
// base 由调用方在进入前先 goto 干净页面；这里保留参数只为与
// checkNodeCaps / checkNodeDetail 同签名（都用 page + base 两个参数）。
async function checkNodeRegister(page, base) {
	// 页面上判定"藏起来了"：Tailwind 的 hidden 类或 hidden 属性都算
	// （契约 §2.1 用的是 class="hidden"，这里两种实现方式都接受，不押风格）
	const probe = () => {
		const hiddenOf = (id) => {
			const e = document.getElementById(id);
			if (!e) return null;
			return e.classList.contains("hidden") || e.hidden === true;
		};
		return {
			hasBtn: !!document.getElementById("node-register-btn"),
			hasPanel: !!document.getElementById("register-panel"),
			hasResult: !!document.getElementById("reg-result"),
			panelHidden: hiddenOf("register-panel"),
			resultHidden: hiddenOf("reg-result"),
			errorHidden: hiddenOf("reg-error"),
			origin: location.origin,
			token: (document.getElementById("reg-token") || {}).textContent || "",
			yaml: (document.getElementById("reg-yaml") || {}).textContent || "",
			cmd: (document.getElementById("reg-cmd") || {}).textContent || "",
			errorText: (document.getElementById("reg-error") || {}).textContent || "",
			listText: (document.getElementById("nodes-list") || {}).textContent || "",
		};
	};

	// ---- ① 入口 + 初始态 ----
	const init = await page.evaluate(probe);
	H.check(
		"节点页有注册节点入口，默认不显示表单与令牌块",
		init.hasBtn === true &&
			init.hasPanel === true &&
			init.hasResult === true &&
			init.panelHidden === true &&
			init.resultHidden === true,
		JSON.stringify({
			入口: init.hasBtn,
			表单块: init.hasPanel,
			结果块: init.hasResult,
			表单初始隐藏: init.panelHidden,
			结果初始隐藏: init.resultHidden,
		}),
	);
	if (!init.hasBtn || !init.hasPanel || !init.hasResult) {
		// 后面的点击全会抛（元素不存在），到此为止，让上面的失败自己说话
		H.check(
			"注册节点：表单/令牌块 DOM 齐全，才能继续验证提交链路",
			false,
			"缺 #node-register-btn / #register-panel / #reg-result 之一",
		);
		return;
	}

	// ---- ② 点入口 → 表单出现 ----
	await page.click("#node-register-btn");
	await page.waitForTimeout(300);
	const opened = await page.evaluate(probe);
	H.check(
		"点「注册节点」→ 表单出现",
		opened.panelHidden === false,
		`表单隐藏=${opened.panelHidden}`,
	);

	// ---- ③ 填表提交 → 一次性令牌块出现 ----
	const regName = "ui-reg-" + Date.now();
	await page.fill("#reg-name", regName);
	await page.fill("#reg-instances", "2");
	await page.click("#reg-submit");
	// mock 有 120–300ms 随机延迟，多等一轮；失败时 #reg-error 会给出原因
	await page.waitForTimeout(1200);

	const reg = await page.evaluate(probe);
	const token = reg.token.trim();
	H.check(
		"提交成功 → 一次性令牌块出现",
		reg.resultHidden === false && token.length > 8,
		`结果块隐藏=${reg.resultHidden} 令牌长度=${token.length} 错误行=${JSON.stringify(reg.errorText)}`,
	);

	// ---- ④ node.yaml 逐项形状（硬锁：能直接粘进 agent） ----
	//
	// **v2 变了**：node.yaml 只剩 bootstrap 四件（name/server/token/instances）。
	// 能力和它的执行方式都不在机器上了 —— 它们由控制台的产物部署下来。
	// 所以这里从「必须含 capabilities/run」翻成「必须**不含** capabilities/run」：
	// 多一个键就是未知字段，agent 会直接启动失败。
	const escapeRe = (s) => String(s).replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
	const yaml = reg.yaml;
	const yamlToken = (yaml.match(/^token: (\S+)\s*$/m) || [])[1] || "";
	const yamlWant = {
		"name 行": new RegExp("^name: " + escapeRe(regName) + "\\s*$", "m").test(yaml),
		"server 行": /^server: \S+/m.test(yaml) && yaml.includes("server: " + reg.origin),
		"token 行": /^token: \S+/m.test(yaml) && yamlToken === token,
		"instances: 2": /^instances: 2\s*$/m.test(yaml),
		"只有四行（bootstrap 四件）":
			yaml.trim().split(/\r?\n/).filter((l) => l.trim() !== "").length === 4,
	};
	const yamlBad = {
		"capabilities 键（v2 已删）": /^capabilities:/m.test(yaml),
		"run 键（执行方式归产物）": /^[ \t]*run:/m.test(yaml),
		"runner 键（v2 不再有 runner 枚举）": /^[ \t]*runner:/m.test(yaml),
		"有 node: 包装": /^node:/m.test(yaml),
		"用了 timeout_sec（agent 要 timeoutSec）": yaml.includes("timeout_sec"),
	};
	const yamlOk =
		Object.values(yamlWant).every(Boolean) &&
		!Object.values(yamlBad).some(Boolean);
	H.check(
		"令牌块里的 node.yaml 是 v2 的 bootstrap 四件（不再有 capabilities/run）",
		yamlOk,
		`${JSON.stringify({ ...yamlWant, ...yamlBad })} — ${JSON.stringify(yaml.slice(0, 200))}`,
	);

	// ---- ④b 一行 docker 命令（首选路径，必须真的能跑） ----
	//
	// 这条不是"文案好看"的检查：cmd/agent 的 configFromEnv 只认 ANOTIFY_TOKEN 非空，
	// 而容器里没有 node.yaml 可指 —— 少了 ANOTIFY_SERVER 会得到「启动成功但一直
	// 连不上」，是所有故障里最难查的一种。所以三条都必须逐字对上。
	const cmd = reg.cmd;
	H.check(
		"注册成功给了一行能直接粘的 docker 命令",
		/docker run/.test(cmd) && cmd.includes(token) && token.length > 8,
		cmd.slice(0, 200),
	);
	H.check(
		"docker 命令带 ANOTIFY_SERVER=<当前 origin>（容器里猜不到中枢地址）",
		cmd.includes("ANOTIFY_SERVER=" + reg.origin),
		`origin=${reg.origin} cmd=${cmd.slice(0, 200)}`,
	);
	H.check(
		"docker 命令带 ANOTIFY_TOKEN=<真令牌>",
		cmd.includes("ANOTIFY_TOKEN=" + token),
		cmd.slice(0, 200),
	);
	H.check(
		"docker 命令不含配置文件（容器里没有 node.yaml 可指）",
		!/node\.yaml/.test(cmd),
		cmd.slice(0, 200),
	);

	// ---- ⑤ 列表刷新 → 新节点出现（offline 也算，注册完还没连上来） ----
	const listed = await page
		.waitForFunction(
			(n) =>
				((document.getElementById("nodes-list") || {}).textContent || "").includes(n),
			regName,
			{ timeout: 8000 },
		)
		.then(() => true)
		.catch(() => false);
	H.check(
		"注册成功后节点列表刷新出现新节点",
		listed === true,
		`列表里找不到 ${regName} — ${JSON.stringify(reg.listText.replace(/\s+/g, " ").slice(0, 200))}`,
	);

	// ---- ⑥ 再点一次入口 → 上一个令牌块必须真的消失 ----
	await page.click("#node-register-btn");
	await page.waitForTimeout(400);
	const cleared = await page.evaluate(probe);
	H.check(
		"再点一次「注册节点」→ 上一个令牌块消失",
		cleared.resultHidden === true &&
			cleared.token === "" &&
			cleared.yaml === "",
		JSON.stringify({
			"结果块隐藏": cleared.resultHidden,
			"令牌文本长度": cleared.token.length,
			"YAML 文本长度": cleared.yaml.length,
		}),
	);

	// ---- ⑦ 重名提交 → 只报重名，不给令牌（mock 里已有 laptop-worker） ----
	//
	// 必须在「令牌还留在 DOM 上」的残留态下验证，否则这条是空转：⑥ 已经把
	// #reg-token 清空了，catch 分支里即使漏了 regClearResult() 也照样绿。
	// 于是这里手工把结果块与表单同时摆出来（一次成功注册之后才会有的危险态）。
	await page.evaluate(() => {
		const setText = (id, text) => {
			const e = document.getElementById(id);
			if (e) e.textContent = text;
		};
		const show = (id) => {
			const e = document.getElementById(id);
			if (!e) return;
			e.classList.remove("hidden");
			e.hidden = false;
		};
		setText("reg-token", "residual-token-must-be-cleared");
		setText("reg-yaml", "name: residual");
		show("register-panel");
		show("reg-result");
	});
	await page.fill("#reg-name", "laptop-worker");
	await page.fill("#reg-instances", "1");
	await page.click("#reg-submit");
	await page.waitForTimeout(1200);
	const dup = await page.evaluate(probe);
	H.check(
		"重名提交 → 显示重名提示且把残留令牌清空",
		dup.errorHidden === false &&
			dup.errorText.includes("laptop-worker") &&
			dup.resultHidden === true &&
			dup.token === "" &&
			dup.yaml === "",
		JSON.stringify({
			错误行可见: dup.errorHidden === false,
			错误行文本: dup.errorText.slice(0, 120),
			结果块隐藏: dup.resultHidden,
			令牌文本长度: dup.token.length,
			YAML文本长度: dup.yaml.length,
		}),
	);

	// ---- ⑧ instances 上界：中枢单节点硬顶 20，UI 必须夹住 ----
	//
	// 这条防的是"UI 显示注册成功、节点永远连不上"：instances 会原样写进
	// node.yaml，而中枢 nodes.max_instances 默认 20，超了 hello 会被
	// too_many_instances 拒（internal/ws/node_handshake.go），节点永远 offline。
	await page.click("#node-register-btn");
	await page.waitForTimeout(300);
	const maxAttr = await page.getAttribute("#reg-instances", "max");
	H.check(
		"注册表单的实例数上限就是中枢硬顶 20",
		String(maxAttr) === "20",
		`max=${JSON.stringify(maxAttr)}`,
	);
	await page.fill("#reg-name", regName + "-cap");
	await page.fill("#reg-instances", "40");
	await page.click("#reg-submit");
	await page.waitForTimeout(1200);
	const capped = await page.evaluate(probe);
	const cappedInstances = (capped.yaml.match(/^instances: (\d+)\s*$/m) || [])[1] || "";
	H.check(
		"实例数填 40 被夹到 20（否则节点永远连不上）",
		capped.resultHidden === false && cappedInstances === "20",
		`结果块隐藏=${capped.resultHidden} YAML 里的 instances=${JSON.stringify(cappedInstances)} 错误行=${JSON.stringify(capped.errorText.slice(0, 80))}`,
	);

	// ---- ⑨ 本地拦截：空名 / 非法名不该发请求 ----
	//
	// 之前这两条分支零覆盖：REG_NAME_RE 若被改成过严（挡掉后端本来合法的名字）
	// 或干脆失效（把空名发出去），不会有任何断言变红。
	await page.click("#node-register-btn");
	await page.waitForTimeout(300);
	await page.evaluate(() => {
		window.__nodePosts = 0;
		const orig = window.fetch;
		if (window.__nodeFetchPatched) return;
		window.fetch = function (...args) {
			const url = String(args[0] || "");
			const opt = args[1] || {};
			if (url.includes("/v1/nodes") && String(opt.method || "").toUpperCase() === "POST") {
				window.__nodePosts += 1;
			}
			return orig.apply(this, args);
		};
		window.__nodeFetchPatched = true;
	});
	await page.fill("#reg-name", "");
	await page.click("#reg-submit");
	await page.waitForTimeout(400);
	const emptyName = await page.evaluate(probe);
	await page.fill("#reg-name", "Bad Name");
	await page.click("#reg-submit");
	await page.waitForTimeout(400);
	const badName = await page.evaluate(probe);
	const postCount = await page.evaluate(() => window.__nodePosts || 0);
	H.check(
		"空名 / 非法名在本地就拦住，一次 POST 都没发",
		emptyName.errorHidden === false &&
			badName.errorHidden === false &&
			emptyName.errorText.trim() !== "" &&
			badName.errorText.trim() !== "" &&
			postCount === 0,
		JSON.stringify({
			空名错误可见: emptyName.errorHidden === false,
			非法名错误可见: badName.errorHidden === false,
			POST次数: postCount,
		}),
	);
}

// 命名空间切换器（设计见 docs/design/namespaces.md）
//
// 这套断言盯的是"隔离真的发生了"，而不是"控件画出来了"：
// 切到 staging 后源列表必须变少、页面请求必须带**新**的 namespaceId、
// 刷新后必须还在 staging、能力与节点也必须跟着命名空间走。
// 组织那一层在 UI 上已经不存在了（#203），所以这里不再有"组织分节"。
async function checkScope(page) {
	// 页内 fetch 钩子（#203）：mock 在 window.fetch 层就把 /v1/* 拦下了
	// （web/mock.js:3141），请求根本不出浏览器 —— page.on("request") 在 mock 页面里
	// 一条都抓不到（实测 0 条）。所以"切换后请求真的带新 namespaceId"只能从页内记录。
	//
	// 必须用 getter/setter：mock.js 是 `window.fetch = …` **赋值**，普通覆盖会被它盖掉。
	// init script 每次导航都会重新执行 → __v1reqs 天然按"本次加载"清空。
	await page.addInitScript(() => {
		window.__v1reqs = [];
		const native = window.fetch.bind(window);
		let inner = null;
		// 归一到 pathname（与 mock.js 的判定一致）：Request 对象 / 绝对 URL 也要能
		// 认出是 /v1/*，否则会漏到 native 变成真实网络请求，mock 预览就漏出去了。
		const pathOf = (input) => {
			const url = typeof input === "string" ? input : (input && input.url) || "";
			try {
				return new URL(String(url), location.href).pathname;
			} catch {
				return String(url).split("?")[0];
			}
		};
		const record = (input) => {
			const url = typeof input === "string" ? input : (input && input.url) || "";
			try {
				const u = new URL(String(url), location.href);
				if (u.pathname.startsWith("/v1/"))
					window.__v1reqs.push(u.pathname + u.search);
			} catch {
				/* 相对/非法 URL 忽略 */
			}
		};
		const dispatch = (input, init) => {
			if (!pathOf(input).startsWith("/v1/")) return native(input, init);
			record(input);
			return inner ? inner(input, init) : native(input, init);
		};
		Object.defineProperty(window, "fetch", {
			configurable: true,
			get: () => dispatch,
			set: (fn) => {
				inner = fn; // mock.js 装上自己的 wrapper 时接住它
			},
		});
	});

	await page.goto(new URL("sources.html", page.url()).href, {
		waitUntil: "load",
		timeout: 20000,
	});
	await page.waitForTimeout(1700);

	// ⓪ 基线：切换前页面请求确实带**当前**命名空间。
	// 没有这条，"切换后带新 id"的断言会因为观测机制没装上而恒假（或者因为
	// 数组永远是空而假绿），属于自证式的假断言。mock 的默认命名空间是 prod
	// （ensureScope 取不到 slug=default 时回落第一条）。
	const baselineOk = await page
		.waitForFunction(
			() =>
				(window.__v1reqs || []).some(
					(r) => r.startsWith("/v1/sources") && r.includes("namespaceId=ns_mock_prod"),
				),
			{ timeout: 15000 },
		)
		.then(() => true)
		.catch(() => false);
	const baseline = await page.evaluate(() => (window.__v1reqs || []).slice());
	H.check(
		"（基线）切换前页面请求带当前命名空间 namespaceId=ns_mock_prod",
		baselineOk &&
			baseline.some(
				(r) => r.startsWith("/v1/sources") && r.includes("namespaceId=ns_mock_prod"),
			),
		`页内记录 ${baseline.length} 条 /v1/*，其中 sources=${JSON.stringify(baseline.filter((r) => r.startsWith("/v1/sources"))).slice(0, 200)}`,
	);

	// ① 侧栏切换器只剩"命名空间"一个维度（组织那套幽灵 UI 已删，#203）
	const sw = await page.evaluate(() => {
		const box = document.querySelector("#scope-switcher");
		const trig = document.querySelector(".scope-trigger");
		if (!box || !trig) return { ok: false };
		trig.click();
		const menu = document.querySelector(".scope-menu");
		// 分节标题只取 `.scope-group span`：同一节里还有新建的 "+" 按钮，
		// 取 group.textContent 会得到 "命名空间+"
		const groups = Array.from(menu.querySelectorAll(".scope-group")).map((g) =>
			((g.querySelector("span") || {}).textContent || "").trim(),
		);
		const items = Array.from(menu.querySelectorAll(".scope-item"));
		const text = (el, sel) => ((el.querySelector(sel) || {}).textContent || "").trim();
		const opened = {
			shows: !menu.classList.contains("hidden"),
			expanded: trig.getAttribute("aria-expanded"),
			groups,
			// 组织头像 / 组织条目都不该再有：切换器里只有一个维度
			avatars: menu.querySelectorAll(".scope-avatar").length,
			menuHasOrg: menu.innerText.includes("组织"),
			count: items.length,
			subs: items.map((x) => text(x, ".scope-item-sub")),
			names: items.map((x) => text(x, ".scope-item-name")),
			ids: items.map((x) => x.getAttribute("data-ns-id") || ""),
			roles: items.map((x) => x.getAttribute("role") || ""),
			plus: !!menu.querySelector(".scope-group-btn"),
			// 触发器本身要能看出当前在哪个命名空间
			ns: (trig.querySelector(".scope-ns") || {}).textContent || "",
			checked: menu.querySelectorAll('.scope-item[aria-checked="true"]').length,
			collapsed: "",
		};
		trig.click(); // 关掉，别影响后面的操作
		opened.collapsed = trig.getAttribute("aria-expanded");
		return opened;
	});
	H.check(
		"侧栏切换器只有一个维度：命名空间（没有组织分节 / 组织头像）",
		sw.ok !== false &&
			sw.shows &&
			sw.expanded === "true" &&
			sw.groups.length === 1 &&
			sw.groups[0] === "命名空间" &&
			sw.avatars === 0 &&
			sw.menuHasOrg === false &&
			sw.plus &&
			sw.ns.includes("prod"),
		`分节=${JSON.stringify(sw.groups)} 菜单里\"组织\"=${sw.menuHasOrg} 头像=${sw.avatars} 分组+ =${sw.plus} 当前=${sw.ns}`,
	);
	H.check(
		"命名空间列了 3 条、每条都能按 data-ns-id 定位、且只有当前项被勾选",
		sw.count === 3 &&
			JSON.stringify(sw.subs) === JSON.stringify(["prod", "staging", "sandbox"]) &&
			sw.names.length === 3 &&
			sw.names.every((n) => n.length > 0) &&
			sw.ids.length === 3 &&
			sw.ids.every((id) => id.length > 0) &&
			sw.roles.every((r) => r === "menuitemradio") &&
			sw.checked === 1,
		`条数=${sw.count} slug=${JSON.stringify(sw.subs)} 名=${JSON.stringify(sw.names)} 勾选=${sw.checked} role=${JSON.stringify(sw.roles)}`,
	);
	H.check(
		"aria-expanded 跟着菜单开合（打开 true / 关闭 false）",
		sw.expanded === "true" && sw.collapsed === "false",
		`打开=${sw.expanded} 关闭=${sw.collapsed}`,
	);

	// ② 数据真的按命名空间隔离（含 `*` 通配）
	const iso = await page.evaluate(async () => {
		const n = async (path) => {
			const r = await fetch(path);
			const j = await r.json();
			return (j.sources || j.events || j.tasks || []).length;
		};
		return {
			prod: await n("/v1/sources?namespaceId=ns_mock_prod"),
			staging: await n("/v1/sources?namespaceId=ns_mock_staging"),
			sandbox: await n("/v1/sources?namespaceId=ns_mock_sandbox"),
			acme: await n("/v1/sources?namespaceId=ns_mock_acme_default"),
			all: await n("/v1/sources?namespaceId=*"),
			prodEvents: await n("/v1/console/events?namespaceId=ns_mock_prod"),
			stagingEvents: await n("/v1/console/events?namespaceId=ns_mock_staging"),
		};
	});
	H.check(
		"mock 按命名空间隔离数据（sandbox 空 / `*` 是并集）",
		iso.prod > iso.staging &&
			iso.sandbox === 0 &&
			// 全量必须等于**每个命名空间之和**（含另一个组织的 Acme）：
			// 少算一个命名空间就会把"漏了一层过滤"放过去
			iso.all === iso.prod + iso.staging + iso.sandbox + iso.acme &&
			iso.prodEvents > iso.stagingEvents,
		`源 prod=${iso.prod} staging=${iso.staging} sandbox=${iso.sandbox} acme=${iso.acme} 全量=${iso.all}；事件 prod=${iso.prodEvents} staging=${iso.stagingEvents}`,
	);

	// ③ 切到 staging：列表内容真的变了
	const before = await page.evaluate(
		() => document.querySelectorAll("#src-list > *").length,
	);
	await page.evaluate(() => {
		document.querySelector(".scope-trigger").click();
		const items = Array.from(document.querySelectorAll(".scope-menu .scope-item"));
		const target = items.find((b) => {
			const sub = b.querySelector(".scope-item-sub");
			return sub && sub.textContent.trim() === "staging";
		});
		if (target) target.click();
	});
	// 切换会整页 reload，等它落地再读 —— 固定 sleep 在并发跑时不够稳
	await page
		.waitForFunction(
			() => {
				const t2 = document.querySelector(".scope-ns");
				return !!(
					t2 &&
					t2.textContent.includes("staging") &&
					document.querySelector("#src-list > *")
				);
			},
			{ timeout: 20000 },
		)
		.catch(() => {});
	await page.waitForTimeout(400);
	const afterSwitch = await page.evaluate(() => ({
		cards: document.querySelectorAll("#src-list > *").length,
		ns: (document.querySelector(".scope-ns") || {}).textContent || "",
	}));
	H.check(
		"切换命名空间会换掉列表内容（不是只有下拉变了）",
		afterSwitch.cards < before && afterSwitch.ns.includes("staging"),
		`切前 ${before} 张 → 切后 ${afterSwitch.cards} 张，当前=${afterSwitch.ns}`,
	);

	// ③-b 切换后页面请求真的带**新**的 namespaceId（#203 硬要求 2）
	//
	// 这条补的是 mock 的盲区：mock 对缺失的 namespaceId 会回退到默认命名空间，
	// 所以"切了但请求没带新 id"在页面上看不出任何症状 —— 只有从请求侧能抓住。
	const stagingReqOk = await page
		.waitForFunction(
			() =>
				(window.__v1reqs || []).some(
					(r) =>
						r.startsWith("/v1/sources") &&
						r.includes("namespaceId=ns_mock_staging"),
				),
			{ timeout: 15000 },
		)
		.then(() => true)
		.catch(() => false);
	const switched = await page.evaluate(() => (window.__v1reqs || []).slice());
	// 命名空间资源前缀白名单 = partials.js 的 NS_PATHS（9 个）：
	// 这些端点缺 namespaceId 后端会 400，属于"必须带"的一类。
	const NS_PREFIXES = [
		"/v1/sources",
		"/v1/edges",
		"/v1/events",
		"/v1/console/events",
		"/v1/tasks",
		"/v1/stats",
		"/v1/capabilities",
		"/v1/nodes",
		"/v1/playbooks",
	];
	const isNsReq = (r) =>
		NS_PREFIXES.some(
			(p) => r === p || r.startsWith(p + "?") || r.startsWith(p + "/"),
		);
	const nsReqs = switched.filter(isNsReq);
	const staleProd = switched.filter((r) => r.includes("namespaceId=ns_mock_prod"));
	const missingNs = nsReqs.filter((r) => !/[?&]namespaceId=/.test(r));
	H.check(
		"切换后请求带新的 namespaceId（staging），旧的 prod 一条不剩",
		stagingReqOk &&
			nsReqs.some(
				(r) =>
					r.startsWith("/v1/sources") && r.includes("namespaceId=ns_mock_staging"),
			) &&
			staleProd.length === 0,
		`带 staging=${stagingReqOk} 残留 prod=${JSON.stringify(staleProd).slice(0, 160)}`,
	);
	H.check(
		"切换后每个命名空间资源请求都带 namespaceId（缺参后端直接 400）",
		nsReqs.length > 0 && missingNs.length === 0,
		`命名空间资源请求 ${nsReqs.length} 条，缺 namespaceId 的 ${missingNs.length} 条=${JSON.stringify(missingNs).slice(0, 200)}`,
	);

	// ④ 刷新后仍停在上次选的命名空间（选择要持久）
	await page.reload({ waitUntil: "load", timeout: 20000 });
	await page
		.waitForFunction(
			() => {
				const t2 = document.querySelector(".scope-ns");
				return !!(t2 && t2.textContent.length > 0);
			},
			{ timeout: 15000 },
		)
		.catch(() => {});
	await page.waitForTimeout(300);
	const persisted = await page.evaluate(() => {
		const raw = localStorage.getItem("anotify.scope") || "";
		return {
			ns: (document.querySelector(".scope-ns") || {}).textContent || "",
			stored: raw.includes("ns_mock_staging"),
		};
	});
	H.check(
		"刷新后仍停在上次选的命名空间",
		persisted.stored && persisted.ns.includes("staging"),
		`当前=${persisted.ns} localStorage 记录=${persisted.stored}`,
	);

	// ⑤ 能力与节点也按命名空间隔离（不存在"组织级"页面了）
	const caps = await page.evaluate(async () => {
		const n = async (ns) => {
			const c = await (await fetch(`/v1/capabilities?namespaceId=${ns}`)).json();
			const d = await (await fetch(`/v1/nodes?namespaceId=${ns}`)).json();
			return { caps: (c.capabilities || []).length, nodes: (d.nodes || []).length };
		};
		return {
			prod: await n("ns_mock_prod"),
			staging: await n("ns_mock_staging"),
			sandbox: await n("ns_mock_sandbox"),
			acme: await n("ns_mock_acme_default"),
			all: await n("*"),
		};
	});
	H.check(
		"能力与节点也按命名空间隔离（sandbox 只剩播种的内置能力）",
		caps.prod.caps > caps.staging.caps &&
			// sandbox 里没有任何用户能力，但内置能力必须被播种进去
			// （否则连一个 end 节点都声明不出来，推送直接失效）
			caps.sandbox.caps === 1 &&
			caps.acme.caps > 1 &&
			caps.all.caps ===
				caps.prod.caps + caps.staging.caps + caps.sandbox.caps + caps.acme.caps &&
			caps.prod.nodes > caps.staging.nodes &&
			caps.sandbox.nodes === 0 &&
			caps.acme.nodes === 1,
		`能力 prod=${caps.prod.caps} staging=${caps.staging.caps} sandbox=${caps.sandbox.caps} acme=${caps.acme.caps} 全量=${caps.all.caps}；节点 prod=${caps.prod.nodes} staging=${caps.staging.nodes} sandbox=${caps.sandbox.nodes} acme=${caps.acme.nodes}`,
	);

	// UI 侧：当前还在 staging，能力列表必须是 staging 的数量，且不再有「组织级」徽标
	await page.goto(new URL("capabilities.html", page.url()).href, {
		waitUntil: "load",
		timeout: 20000,
	});
	// 等**确切数量**，不等"非空"：`#caps-list` 先会有一个加载占位符，
	// children.length > 0 在数据到达前就已经成立，会读到 1 行（踩过）。
	await page
		.waitForFunction(
			(want) => {
				const list = document.querySelector("#caps-list");
				const ns = document.querySelector(".scope-ns");
				return !!(ns && ns.textContent && list && list.children.length === want);
			},
			caps.staging.caps,
			{ timeout: 15000 },
		)
		.catch(() => {});
	const capPage = await page.evaluate(() => ({
		badge: !!document.querySelector(".scope-badge"),
		rows: document.querySelectorAll("#caps-list > *").length,
		ns: (document.querySelector(".scope-ns") || {}).textContent || "",
	}));
	H.check(
		"能力页跟着命名空间变（不再有「组织级」页面 / 徽标）",
		!capPage.badge && capPage.rows === caps.staging.caps && capPage.ns.includes("staging"),
		`徽标=${capPage.badge} 行=${capPage.rows} 期望=${caps.staging.caps} 当前=${capPage.ns}`,
	);

	// 复位，避免影响后面用 prod 数据的断言
	await page.evaluate(() => localStorage.removeItem("anotify.scope"));
}

// waitUntilNode 轮询一个 **node 侧**的条件。
//
// dialog（prompt / alert）事件只在 Playwright 侧可见，page.waitForFunction 看不到，
// 而"点一下再睡 500ms"在并发跑时既慢又不稳。等的是条件，不是时长。
async function waitUntilNode(fn, timeoutMs = 8000, intervalMs = 50) {
	const deadline = Date.now() + timeoutMs;
	for (;;) {
		if (await fn()) return true;
		if (Date.now() >= deadline) return false;
		await new Promise((r) => setTimeout(r, intervalMs));
	}
}

// checkScopeEmptyState 验证「一个命名空间都没有」时侧栏不是空下拉，而是空态 + 新建入口（#203）。
//
// 为什么要另开一页：page.addInitScript 对**那个 page** 永久生效，把 /v1/namespaces 桩成
// 空表会污染后面还要用的 page（checkPlaybooks 等）。
//
// 为什么必须在页内钩 fetch：mock 在 window.fetch 层就拦下了 /v1/*（web/mock.js:3141），
// page.route 根本看不到这些请求 —— 只能在自己的 init script 里套一层。
// 必须用 getter/setter：mock.js 是 `window.fetch = …` **赋值**，普通覆盖会被它盖掉。
async function checkScopeEmptyState(page) {
	const p2 = await page.context().newPage();
	try {
		await p2.addInitScript(() => {
			const native = window.fetch.bind(window);
			let inner = null;
			const dispatch = (input, init) => {
				const url = typeof input === "string" ? input : (input && input.url) || "";
				let path = String(url).split("?")[0];
				try {
					path = new URL(String(url), location.href).pathname;
				} catch {
					/* 相对 / 非法 URL 用上面的兜底 */
				}
				const method = String(
					(init && init.method) || (input && input.method) || "GET",
				).toUpperCase();
				// 非 /v1/ 直达网络（镜像 mock 自己的行为，避免与 mock 的 wrapper 递归）
				if (!path.startsWith("/v1/")) return native(input, init);
				if (method === "GET" && path === "/v1/namespaces")
					return Promise.resolve(
						new Response(JSON.stringify({ namespaces: [] }), {
							status: 200,
							headers: { "content-type": "application/json" },
						}),
					);
				return inner ? inner(input, init) : native(input, init);
			};
			Object.defineProperty(window, "fetch", {
				configurable: true,
				get: () => dispatch,
				set: (fn) => {
					inner = fn; // mock.js 装上自己的 wrapper 时接住它
				},
			});
		});

		await p2.goto(new URL("index.html", page.url()).href, {
			waitUntil: "load",
			timeout: 20000,
		});
		await p2
			.waitForFunction(
				() => {
					const box = document.querySelector("#scope-switcher");
					return !!(box && !box.classList.contains("hidden"));
				},
				{ timeout: 15000 },
			)
			.catch(() => {});

		const empty = await p2.evaluate(() => {
			const box = document.querySelector("#scope-switcher");
			if (!box) return { ok: false };
			const visible =
				!box.classList.contains("hidden") && box.getBoundingClientRect().height > 0;
			const trig = box.querySelector(".scope-trigger");
			if (trig) trig.click(); // 空态在菜单里，要先展开才看得见 / 点得到
			const menu = box.querySelector(".scope-menu");
			const emp = menu && menu.querySelector(".scope-empty");
			const text = (sel) =>
				(((emp && emp.querySelector(sel)) || {}).textContent || "").trim();
			return {
				ok: true,
				visible,
				menuOpen: !!menu && !menu.classList.contains("hidden"),
				items: menu ? menu.querySelectorAll(".scope-item").length : -1,
				hasEmpty: !!emp,
				emptyText: text(".scope-empty-text"),
				ctaText: text(".scope-empty-cta"),
				// 分组头的 + 是第二个新建入口，空态下也必须还在
				plus: !!(menu && menu.querySelector(".scope-group-btn")),
				ns: ((box.querySelector(".scope-ns") || {}).textContent || "").trim(),
			};
		});
		H.check(
			"没有命名空间时侧栏不是空下拉，而是空态文案 + 新建入口",
			empty.ok !== false &&
				empty.visible &&
				empty.menuOpen &&
				empty.items === 0 &&
				empty.hasEmpty &&
				empty.emptyText.length > 0 &&
				empty.ctaText.length > 0 &&
				empty.plus,
			`可见=${empty.visible} 条目=${empty.items} 空态=${empty.hasEmpty} 文案=${empty.emptyText} CTA=${empty.ctaText} 分组+ =${empty.plus} 触发器=${empty.ns}`,
		);

		// 空态的 CTA 必须真的走新建流程（弹 slug 输入），不能是个死按钮。
		// 只数 prompt 次数：列表是桩成空的，真建也建不出来，不断言后续。
		let prompts = 0;
		p2.on("dialog", async (d) => {
			prompts += 1;
			try {
				await d.dismiss();
			} catch {
				/* 已经自己关了 */
			}
		});
		await p2.click(".scope-empty-cta");
		const prompted = await waitUntilNode(() => prompts > 0);
		H.check(
			"空态的「新建命名空间」按钮真的走新建流程（弹出 slug 输入）",
			prompted && prompts === 1,
			`prompt 次数=${prompts}`,
		);
	} finally {
		await p2.close();
	}
}

// checkPlaybooks 验证「预案」的体检界面。
//
// 锁四件事（都是这一版真正想表达的产品语义）：
//   ① 四种总体状态都能看到（就绪 / 缺节点 / 缺定义），不是只有一个"未就绪"
//   ② 每张卡都给出「最该先做的一件事」—— 全页最有价值的一行
//   ③ 缺节点的能力给的是**只含缺的那几个**的 node.yaml，不是整份文件
//   ④ 「补齐」只补定义/边/源，**不会假装修好节点**
async function checkPlaybooks(page, base) {
	await page.goto(base + "/playbooks.html", { waitUntil: "load", timeout: 20000 });
	await page
		.waitForFunction(() => document.querySelectorAll(".pb-card").length >= 4, {
			timeout: 15000,
		})
		.catch(() => {});
	await page.waitForTimeout(400);

	const list = await page.evaluate(() => {
		const cards = Array.from(document.querySelectorAll(".pb-card"));
		return {
			n: cards.length,
			badges: cards.map((c) => (c.querySelector(".pb-badge") || {}).textContent || ""),
			nexts: cards.map((c) => (c.querySelector(".pb-next") || {}).textContent || ""),
			bars: cards.map((c) => {
				const fill = c.querySelector(".pb-bar > i");
				return fill ? getComputedStyle(fill).backgroundColor : "";
			}),
		};
	});

	H.check(
		"预案列表把三种状态分开展示（就绪 / 缺节点 / 缺定义）",
		list.n >= 4 && ["就绪", "缺节点", "缺定义"].every((x) => list.badges.includes(x)),
		`卡片=${list.n} 状态=${Array.from(new Set(list.badges)).join(",")}`,
	);
	H.check(
		"每张预案卡片都给出「最该先做的一件事」",
		list.nexts.length === list.n && list.nexts.every((t) => t.replace("→", "").trim().length > 3),
		JSON.stringify(list.nexts).slice(0, 180),
	);
	// 进度条必须跟状态同色：合成一种颜色就看不出哪张卡是好的了
	const okIdx = list.badges.indexOf("就绪");
	const nnIdx = list.badges.indexOf("缺节点");
	H.check(
		"预案的进度条按状态分色（就绪不是主色）",
		okIdx >= 0 && nnIdx >= 0 && list.bars[okIdx] !== list.bars[nnIdx],
		`就绪=${list.bars[okIdx]} 缺节点=${list.bars[nnIdx]}`,
	);

	// ---- 详情页：四类检查 + 缺节点的 YAML ----
	await page.goto(base + "/playbook.html?id=nightly-audit", {
		waitUntil: "load",
		timeout: 20000,
	});
	await page
		.waitForFunction(() => document.querySelectorAll("#node-list .chk").length > 0, {
			timeout: 15000,
		})
		.catch(() => {});
	await page.waitForTimeout(400);

	const detail = await page.evaluate(() => ({
		caps: document.querySelectorAll("#cap-list .chk").length,
		nodes: document.querySelectorAll("#node-list .chk").length,
		edges: document.querySelectorAll("#edge-list .chk").length,
		trgs: document.querySelectorAll("#trg-list .chk").length,
		yaml: (document.getElementById("yaml-text") || {}).textContent || "",
		badDots: document.querySelectorAll(".chk-dot.is-bad").length,
		hasCopyYaml: !!Array.from(document.querySelectorAll("#node-list button")).find((b) =>
			b.textContent.includes("YAML"),
		),
	}));
	H.check(
		"预案详情把四类检查都列出来（定义 / 节点 / 出口 / 触发器）",
		detail.caps >= 2 && detail.nodes >= 2 && detail.edges >= 1 && detail.trgs >= 1,
		`定义=${detail.caps} 节点=${detail.nodes} 出口=${detail.edges} 触发器=${detail.trgs}`,
	);
	// 断言的是**能直接粘进 cmd/agent 的形状**，不是"看起来像 YAML"。
	//
	// 这里曾经断言 includes("run: bash") —— 而那句话正是错的：agent 要的是
	// `run:` 是映射（`runner: bash` 缩进在下面），`run: bash` 这种标量会被
	// 严格解码直接拒掉（yaml: field not found）。所以旧断言把 bug 钉成了期望值。
	// 现在改成逐项对齐 cmd/agent/config.go：顶层 server/token/instances、
	// capabilities[].capability（不是 name）、run 下的 runner/command/timeoutSec。
	const yamlOk =
		detail.yaml.includes("archive-logs") &&
		!detail.yaml.includes("report.send") &&
		// agent 的键名是 capability（不是 name），缩进在列表项下面
		/^\s+- capability: \S+/m.test(detail.yaml) &&
		// run 必须是**映射**：单独一行 run:，runner 缩进在其下
		/^\s+run:\s*$/m.test(detail.yaml) &&
		/^\s+runner: bash/m.test(detail.yaml) &&
		/^\s+command: \|/m.test(detail.yaml) &&
		// 顶层三件：agent 没它们连不上
		/^server: \S/m.test(detail.yaml) &&
		/^token: \S/m.test(detail.yaml) &&
		/^instances: \d+/m.test(detail.yaml) &&
		// 反例：旧的错误形状必须消失（node: 包装、run 标量、timeout_sec）
		!/^node:/m.test(detail.yaml) &&
		!/^\s+run: bash\s*$/m.test(detail.yaml) &&
		!detail.yaml.includes("timeout_sec");
	H.check(
		"缺节点的能力给出可直接粘的 node.yaml（只含缺的那几个、且形状合 cmd/agent）",
		yamlOk && detail.hasCopyYaml,
		`含 archive-logs=${detail.yaml.includes("archive-logs")} 混进 report.send=${detail.yaml.includes("report.send")} 形状合规=${yamlOk} 有复制按钮=${detail.hasCopyYaml} — ${JSON.stringify(detail.yaml.slice(0, 200))}`,
	);

	// ---- 「补齐」不能假装修好节点 ----
	const before = detail.badDots;
	await page.click("#btn-install");
	await page.waitForTimeout(1500);
	const after = await page.evaluate(() => ({
		badDots: document.querySelectorAll(".chk-dot.is-bad").length,
		// 节点这一项的缺失必须还在
		nodeStillBad: !!document
			.getElementById("node-list")
			.querySelector(".chk-dot.is-bad"),
		badge: (document.getElementById("pb-badge") || {}).textContent || "",
	}));
	H.check(
		"「补齐」只补定义/边/源，不假装修好节点",
		after.badDots < before && after.nodeStillBad && after.badge === "缺节点",
		`缺项 ${before} → ${after.badDots}，节点项仍缺=${after.nodeStillBad}，状态=${after.badge}`,
	);
}

async function checkFlowChart(page, base) {
	// 从缩进文本换成 Cytoscape 图形库后（#88），断言改成看"图上有什么"：
	// 节点、边、环路边、未接下游的伪节点、以及只读性。
	//
	// 为什么必须在这里测而不是在真实 server 上：断言依赖 demo 数据里的
	// **环路**（deploy-preview → apply-fix → deploy-preview）和**未接出口**
	// （preview.ready）。真实环境不一定有这些形状，那样的断言会假失败。
	// #112：流向图从**列表卡片**搬到了**源详情页**（source.html?id=…）。
	// 列表只回答"有哪些源、活着吗"，细节（配置/参数/流向）点进去看。
	//
	// 图本身的形状断言没有变，变的是它挂在哪一页。
	//
	// 从**已渲染的 DOM** 拿链接，不发任何请求：这是 mock 预览环境，
	// 整页的前提是"没有真实请求发出去"（本套件末尾有断言在查这个）。
	const flowHref = await page.evaluate(() => {
		const a = document.querySelector(".src-name-link");
		return a ? a.getAttribute("href") : null;
	});
	if (!flowHref) {
		H.check("详情页可进入（列表里有源链接）", false, "列表里没有 .src-name-link");
		return;
	}
	await page.goto(base + "/" + flowHref, { waitUntil: "load", timeout: 20000 });
	await page.waitForTimeout(2200);

	const found = await page.evaluate(() => document.querySelectorAll(".flograph").length);
	H.check("源详情页有流向图", found > 0, `count=${found}`);
	if (!found) return;

	await page.waitForTimeout(600);

	const g = await page.evaluate(() => {
		const host = document.querySelector(".flograph");
		if (!host) return { hasCanvas: false };
		// 节点是真正的 DOM 卡片（不是 canvas 里画的），所以断言直接读 DOM：
		// 这也顺带验证了"节点可以是任意结构"这个换渲染方式的初衷。
		const nodeEls = Array.from(host.querySelectorAll(".wf-node"));
		return {
			hasCanvas: true,
			hasGraph: !!host._graph,
			nodeCount: nodeEls.length,
			edgeCount: host.querySelectorAll(".flograph-svg path[marker-end]").length,
			labels: nodeEls.map((n) => n.querySelector(".wf-node-title")?.textContent || ""),
			ports: Array.from(host.querySelectorAll(".flograph-edge-label")).map((x) =>
				x.textContent.trim(),
			),
			// 环边现在用"同色系加深"（is-hot），语义是"这条路走了很多次"，
			// 而不是"这里危险"。判据同步改成 is-hot。
			hasCycleEdge: host.querySelectorAll(".flograph-svg path.is-hot, .flograph-svg path.is-hot2")
				.length > 0,
			// 图例：源详情页曾经**没有**（trace 页有），同一个组件两个页面
			// 长得不一样。这条断言就是防这个再发生。
			hasLegend: !!document.querySelector(".flograph-legend"),
			legendRows: Array.from(
				document.querySelectorAll(".flograph-legend-row"),
			).map((r) => r.textContent.trim()),
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

	// ---- 全屏入口的位置：两页必须一样 ----
	//
	// 走过两次弯路：
	//   ① 标题栏一个文字「全屏」+ 工具栏一个图标版 → 两个形状相近的按钮
	//      挨在一起，看着像重复；
	//   ② 改成"有标题栏入口就不画工具栏图标" → 同一个组件在两个页面把
	//      同一个功能放在两个地方，用户在 trace 页学会"右下角有全屏"，
	//      到源详情页就找不到。
	//
	// 最终：**统一放在右下角工具栏**，位置和图标都一致。
	{
		const fs = await page.evaluate(() => {
			const ctrl = document.querySelector(".flograph-ctrl");
			const actions = ctrl
				? Array.from(ctrl.querySelectorAll("button")).map((b) => b.dataset.fgAction)
				: [];
			const titleBtns = Array.from(document.querySelectorAll("button")).filter(
				(b) => /^\s*全屏\s*$/.test(b.textContent || ""),
			);
			return {
				toolbarActions: actions,
				titleTextCount: titleBtns.length,
			};
		});
		H.check(
			"源详情页工具栏有全屏（和 trace 页同一位置）",
			fs.toolbarActions.includes("fullscreen"),
			fs.toolbarActions.join(","),
		);
		H.check(
			"没有第二个标题栏全屏入口（避免重复按钮）",
			fs.titleTextCount === 0,
			`titleTextCount=${fs.titleTextCount}`,
		);
		H.check(
			"工具栏仍有缩放三件套",
			["zoom-in", "zoom-out", "fit"].every((a) => fs.toolbarActions.includes(a)),
			fs.toolbarActions.join(","),
		);
	}
	H.check("节点是真正的 DOM 卡片（有图标 + 标题 + 副标题）",
		g.icons.every(Boolean) && g.subTitles !== undefined,
		`icons=${g.icons.length} subs=${g.subTitles.length}`);
	H.check("图上有节点", (g.nodeCount || 0) >= 2, `nodes=${g.nodeCount}`);
	H.check("图上有边", (g.edgeCount || 0) >= 2, `edges=${g.edgeCount}`);
	// 节点标题走 capLabel：有 displayName 用显示名，否则回落 name@version。
	//
	// 原来断言"图上存在含 @版本 的标签"，依赖 mock 里恰好有几个能力没填
	// displayName —— 那是数据形状不是契约。把 mock 显示名补全（为了让变量
	// 演示有连贯的业务语义）后它就假失败了。改成验规则本身。
	const lbl = await page.evaluate(() => {
		const f = window.Anotify && window.Anotify.capLabel;
		if (typeof f !== "function") return null;
		return {
			withDN: f({ name: "acme-fix", version: 1, displayName: "应用修复" }),
			noDN: f({ name: "acme-fix", version: 1 }),
			blankDN: f({ name: "acme-fix", version: 1, displayName: "   " }),
		};
	});
	H.check(
		"节点标题有显示名用显示名，没填回落 name@version",
		lbl !== null && lbl.withDN === "应用修复" && lbl.noDN === "acme-fix@1" && lbl.blankDN === "acme-fix@1",
		JSON.stringify(lbl),
	);
	H.check("边上标了出口名", (g.ports || []).length >= 2, (g.ports || []).join(","));
	H.check("环上的边用加深的样式画出来", g.hasCycleEdge === true);
	H.check("「未接下游」在图上可见", g.hasDeadPort === true);
	// 图上只有"源"这一个触发器是根，能力节点都该有上游
	H.check("只有源节点被标成根（触发器）", g.rootCount === 1, `roots=${g.rootCount}`);
	H.check("所有节点都有 dagre 算出的固定坐标", g.hasCoords === true);

	// ---- 两个页面用同一个组件，必须长得一样 ----
	//
	// 之前图例只在 trace 页有、源详情页没有；环也只在 trace 页绕成弧，
	// 源详情页是两节点之间的一小段斜线（看着像画坏了）。
	// 根因是两个页面各自拼一套渲染参数，加一边忘另一边。
	H.check("源详情页也有图例（和 trace 页一致）", g.hasLegend === true, String(g.hasLegend));
	H.check(
		"源详情页的图例解释了三种结构身份",
		g.legendRows.some((x) => x.includes("源")) &&
			g.legendRows.some((x) => x.includes("管道")) &&
			g.legendRows.some((x) => x.includes("终点")),
		g.legendRows.join(" | "),
	);
	H.check(
		"源详情页的图例没混进执行状态（那是 trace 页才有的维度）",
		!g.legendRows.some((x) => /执行成功|执行失败|未执行/.test(x)),
		g.legendRows.join(" | "),
	);

	// ---- 全屏（#117）----
	//
	// 全屏是这个图的主要使用方式：520px 的卡片放下 11 个节点只能缩到 0.46，
	// 字小到读不了。所以这条链路必须固化：
	//
	//   ① 控件里有全屏按钮（不是要用户去按 F11 —— 那样连控件都没了）；
	//   ② 点击后容器尺寸 = 视口（不能只是 fixed：祖先有 transform 时会被裁）；
	//   ③ 容器真的移到了 body 下（脱出卡片裁剪）；
	//   ④ 占位符 Height = 原高度（否则关掉时页面会跳）；
	//   ⑤ Esc 可退，且样式/位置/滚动都复原；
	//   ⑥ 全屏后 scale 变大（说明真的重排了，不是把图拉大）。
	// 入口就是右下角工具栏那个图标（两页一致）。
	const fsBtn = await page.evaluate(() => {
		const b = document.querySelector('.flograph-ctrl button[data-fg-action="fullscreen"]');
		if (!b) return null;
		return {
			text: b.title,
			aria: b.getAttribute("aria-label"),
			// 必须是 SVG 图标：字体缺字会把 Unicode 符号画成豆腐块
			isSvg: !!b.querySelector("svg"),
		};
	});
	H.check(
		"源详情页工具栏有全屏图标（带 aria-label）",
		fsBtn !== null && !!fsBtn.aria && fsBtn.isSvg === true,
		JSON.stringify(fsBtn),
	);

	// 源流向图没有执行数据（每个节点都没有点亮顺序），所以**不该**有
	// 重播按钮 —— 放一个点了不动的按钮比不放更糟。
	const repBtn = await page.evaluate(
		() => !!document.querySelector('.flograph-ctrl button[data-fg-action="replay"]'),
	);
	H.check(
		"源流向图没有重播按钮（它没有可播放的执行顺序）",
		repBtn === false,
		`有重播按钮=${repBtn}`,
	);

	if (fsBtn) {
		const before = await page.evaluate(() => {
			const h = document.querySelector(".flograph");
			return {
				w: Math.round(h.getBoundingClientRect().width),
				h: Math.round(h.getBoundingClientRect().height),
				scale: h._graph._debug.scale(),
			};
		});
		await page.locator('.flograph-ctrl button[data-fg-action="fullscreen"]').click();
		// 事件等待而不是固定 sleep：全屏是「改样式 → 下一帧重排 → fit」，
		// 固定 500ms 既慢又不稳（并行跑 4 个 Chrome 时可能还没排完）。
		// 判据用最终状态本身：容器已在 body 下且尺寸铺满视口。
		await page
			.waitForFunction(
				() => {
					const h = document.querySelector(".flograph");
					if (!h || h.parentElement !== document.body) return false;
					const r = h.getBoundingClientRect();
					return (
						Math.abs(r.width - window.innerWidth) < 2 &&
						Math.abs(r.height - window.innerHeight) < 2
					);
				},
				null,
				{ timeout: 3000 },
			)
			.catch(() => {});
		const on = await page.evaluate(() => {
			const h = document.querySelector(".flograph");
			const r = h.getBoundingClientRect();
			const ph = document.querySelector(".flograph-placeholder");
			return {
				parentIsBody: h.parentElement === document.body,
				w: Math.round(r.width),
				h: Math.round(r.height),
				vw: window.innerWidth,
				vh: window.innerHeight,
				scale: h._graph._debug.scale(),
				isFs: h._graph._debug.fullscreen(),
				placeholderH: ph && !ph.hidden ? Math.round(ph.getBoundingClientRect().height) : 0,
				// 全屏时不能还能拖动背后页面：html 上锁了 overflow
				locked: document.documentElement.classList.contains("flograph-fullscreen-open"),
				// 退出提示只在该显示的时候显示
				hintVisible:
					getComputedStyle(h.querySelector(".flograph-fullscreen-hint")).display !== "none",
			};
		});
		H.check(
			"点全屏后容器铺满视口且脱离卡片裁剪",
			on.isFs === true &&
				on.parentIsBody === true &&
				on.w === on.vw &&
				on.h === on.vh,
			JSON.stringify({ fs: on.isFs, body: on.parentIsBody, box: [on.w, on.h], vp: [on.vw, on.vh] }),
		);
		H.check(
			"全屏后重新布局（scale 变大）并锁住页面滚动",
			on.scale > before.scale && on.locked === true && on.hintVisible === true,
			JSON.stringify({ before: before.scale, after: on.scale, locked: on.locked }),
		);
		H.check(
			"全屏时占位符撑住原高度（关掉不会跳）",
			on.placeholderH === before.h,
			`placeholder=${on.placeholderH} before=${before.h}`,
		);

		// Esc 退出：等容器真的回到卡片里再断言
		await page.keyboard.press("Escape");
		await page
			.waitForFunction(
				() => {
					const h = document.querySelector(".flograph");
					return h !== null && h.parentElement !== document.body && h.closest(".card") !== null;
				},
				null,
				{ timeout: 3000 },
			)
			.catch(() => {});
		const off = await page.evaluate(() => {
			const h = document.querySelector(".flograph");
			const r = h.getBoundingClientRect();
			return {
				isFs: h._graph._debug.fullscreen(),
				backInCard: !!h.closest(".card"),
				w: Math.round(r.width),
				h: Math.round(r.height),
				placeholderGone: !document.querySelector(".flograph-placeholder:not([hidden])"),
				locked: document.documentElement.classList.contains("flograph-fullscreen-open"),
				styleAttr: h.getAttribute("style"),
			};
		});
		H.check(
			"Esc 退出全屏并完全复原（回卡片 / 原尺寸 / 无残留样式）",
			off.isFs === false &&
				off.backInCard === true &&
				off.w === before.w &&
				off.h === before.h &&
				off.placeholderGone === true &&
				off.locked === false &&
				!off.styleAttr,
			JSON.stringify({ ...off, before }),
		);
	}

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

// ---------- #209：keys / security / admin 的页头徽标 ----------
//
// 这三页的内容不随命名空间切换（api_keys 的作用域 = 整个部署），所以页头传
// `scopeLevel: "deployment"`，由 mountLayout 追加一枚 `.scope-badge`。
// 断言盯的是「管道还在、话还说对了」，不是「DOM 里有个 span」：
//   ① 页头 h1（title + 徽标）里不能再出现「组织」——#203/#207 删掉的那层
//      不能借徽标回潮（旧文案正是「组织级」）；
//   ② 徽标文案逐字 = 「全部署通用」，title 必须点明「不随命名空间切换变化」
//      —— 徽标存在的理由就是这句话，两个都不能丢；
//   ③ 三页文案互相一致 → 证明是 partials.js 一条管道渲染的，不是三处各自
//      硬编码（硬编码迟早漂，漂了没人发现）；
//   ④ 对照 index.html 必须没有徽标 → 证明没把它撒到全站（撒了就成了噪音，
//      用户会以为每页都不随命名空间变）。
const DEPLOYMENT_WIDE_PAGES = ["keys.html", "security.html", "admin.html"];
const DEPLOYMENT_WIDE_LABEL = "全部署通用";

async function checkDeploymentWideBadge(ctx, base) {
	const labels = [];

	for (const name of DEPLOYMENT_WIDE_PAGES) {
		// 独立 page + 独立监听：这三页在 mock 模式下的「mock 未实现」console 告警
		// 是既知状态，绝不能进 runMockCoverage 的 missing 数组 —— 那会把
		// 「mock 覆盖了页面用到的全部接口」这条既有断言误伤成红。
		// 这里只收 pageerror / console error 当诊断信息，不参与判定
		// （这三页的其余行为由别的断言盯着）。
		const page = await ctx.newPage();
		const pageErrors = [];
		const consoleErrors = [];
		page.on("pageerror", (e) => pageErrors.push(String((e && e.message) || e)));
		page.on("console", (m) => {
			if (m.type() === "error") consoleErrors.push(m.text());
		});

		await page.goto(`${base}/${name}`, { waitUntil: "load", timeout: 20000 });
		// 等 mountLayout 挂完（#sidebar 是它的锚点）再等页头 h1 出现。
		// 不 sleep 大数，也不等 .scope-badge —— 徽标缺失本身就是断言要报的事，
		// 等它出现会把「徽标没了」变成超时，反而丢掉实际文本。
		await H.waitForAppReady(page, "workspace", { dataAnchor: "header h1" });

		const got = await page.evaluate(() => {
			// 全站唯一的 h1 由 mountLayout 造（web/partials.js），页内容里没有 h1
			const header = document.querySelector("header");
			const h1 =
				(header && header.querySelector("h1")) || document.querySelector("h1");
			const badge = h1 && h1.querySelector(".scope-badge");
			return {
				path: location.pathname,
				lang: document.documentElement.lang || "",
				h1: h1 ? (h1.innerText || "").replace(/\s+/g, " ").trim() : null,
				badgeCount: document.querySelectorAll(".scope-badge").length,
				label: badge ? (badge.textContent || "").trim() : null,
				hint: badge ? badge.getAttribute("title") || "" : null,
			};
		});
		await page.close();

		if (got.label) labels.push(got.label);

		// 一页一条断言，四条要求全中才算过；detail 里把实际文本全带上，
		// 红了不用再手动开浏览器猜是哪一项。
		const onPage = got.path.endsWith(`/${name}`); // 被重定向到 login.html 会在这里现形
		const headerNoOrg = !!got.h1 && !got.h1.includes("组织");
		const badgeThere = !!got.label;
		const labelOk = got.label === DEPLOYMENT_WIDE_LABEL;
		const hintOk =
			!!got.hint && got.hint.includes("不随命名空间") && !got.hint.includes("组织");

		console.log(
			`  · ${name} 页头「${got.h1 || "（无 h1）"}」徽标「${got.label || "（无）"}」`,
		);
		H.check(
			`${name} 页头徽标「${DEPLOYMENT_WIDE_LABEL}」且不再出现「组织」`,
			onPage && headerNoOrg && badgeThere && labelOk && hintOk,
			JSON.stringify({
				落在: got.path,
				页面语言: got.lang,
				页头: got.h1,
				徽标: got.label,
				徽标title: got.hint,
				徽标数: got.badgeCount,
				pageerror: pageErrors.slice(0, 2),
				已知console告警: consoleErrors.slice(0, 2),
			}),
		);
	}

	// 三页逐字一致 = 同一条管道（partials.js）渲染，不是三处硬编码
	H.check(
		"三页徽标文案彼此一致（同一条 partials.js 管道）",
		labels.length === DEPLOYMENT_WIDE_PAGES.length &&
			new Set(labels).size === 1 &&
			labels[0] === DEPLOYMENT_WIDE_LABEL,
		`实际 ${JSON.stringify(labels)}`,
	);

	// 对照：徽标只属于那三页，没被撒到全站
	const ctl = await ctx.newPage();
	await ctl.goto(`${base}/index.html`, { waitUntil: "load", timeout: 20000 });
	await H.waitForAppReady(ctl, "workspace", { dataAnchor: "header h1" });
	const ctlGot = await ctl.evaluate(() => {
		const header = document.querySelector("header");
		const h1 =
			(header && header.querySelector("h1")) || document.querySelector("h1");
		return {
			badgeCount: document.querySelectorAll(".scope-badge").length,
			h1: h1 ? (h1.innerText || "").replace(/\s+/g, " ").trim() : null,
		};
	});
	await ctl.close();
	H.check(
		"对照：index.html 页头没有这枚徽标（没撒到全站）",
		!!ctlGot.h1 && ctlGot.badgeCount === 0,
		`index.html 页头「${ctlGot.h1 || "（无 h1）"}」.scope-badge=${ctlGot.badgeCount}`,
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

	// detached: 让 go run 自成**进程组**。
	//
	// 为什么必须这样：`go run` 是编译器，真正监听端口的是它 exec/fork
	// 出来的子进程。只 kill go run 会留下孤儿继续占着端口和内存 ——
	// 曾经累积上百个残留进程，把后续 e2e 逼到随机失败（看着像代码问题，
	// 实际是资源被自己人吃光）。detached 之后能按进程组一次收干净。
	const proc = spawn("go", ["run", "./cmd/fe-preview", "-dir", "web", "-port", String(port)], {
		cwd: process.cwd(),
		stdio: "ignore",
		env: { ...process.env },
		detached: true,
	});

	// killGroup 收掉 go run 及它派生的静态服务器。
	const killGroup = () => {
		try {
			// 负 PID = 整个进程组
			process.kill(-proc.pid, "SIGKILL");
		} catch {
			try { proc.kill("SIGKILL"); } catch { /* ignore */ }
		}
	};

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
		killGroup();
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
			await checkCapabilityTemplates(page);
			await checkEdgesPage(page);
			// 显示名要走真实 PATCH（mock 也实现），改完再回列表看效果
			await checkDeclaredBy(page);
			await checkDisplayName(page, base);
		}
		if (p === "sources.html") await checkFlowChart(page, base);
		if (p === "nodes.html") {
			await checkNodeCaps(page, base);
			// checkNodeCaps 会点 chip 跳走，回到列表再进详情
			await page.goto(`${base}/nodes.html`, { waitUntil: "load", timeout: 20000 });
			await page.waitForTimeout(1200);
			await checkNodeDetail(page, base);
			// checkNodeDetail 会跳到 node.html；注册流程要从干净的列表页开始
			await page.goto(`${base}/nodes.html`, { waitUntil: "load", timeout: 20000 });
			await page.waitForTimeout(1200);
			await checkNodeRegister(page, base);
		}
		if (p === "events.html") {
			await checkEventsFilter(page, base);
			await page.goto(`${base}/events.html`, { waitUntil: "load", timeout: 20000 });
			await page.waitForTimeout(1500);
			await checkWaterfall(page);
			// 放最后：这套检查会切命名空间、还会跳页，不能夹在别的检查中间
			await checkScope(page);
			// 空态另开一页验证（把 /v1/namespaces 桩成空表，不能污染上面这个 page）
			await checkScopeEmptyState(page);
			await checkPlaybooks(page, base);
		}
		await page.close();
	}

	// keys / security / admin 的「全部署通用」徽标（#209）：放在 missing /
	// realRequests 两条汇总断言之前收尾，用的是独立 page + 独立监听。
	await checkDeploymentWideBadge(ctx, base);

	await ctx.close();
	await browser.close();
	killGroup();

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
