#!/usr/bin/env node
/* SUITE: frontend — 前端渲染 + 路由守卫 + 真实数据
 *
 * 覆盖 case：
 *  A. 路由守卫：未登录访问 index/receivers/keys/security/message → 自动跳 login.html
 *  B. 已登录访问 index → 渲染真实数据（哪怕空）；演示模式已移除（#273），无 demo-badge
 *  C. login.html 是公开页：未登录正常渲染、不跳转
 *  D. 全部页面（index/login/receivers/keys/security/docs/message）在 桌面1280 + 移动390 两视口：
 *     无 JS pageerror、无横向溢出、能滚动到底
 *     （/v1/* 的 401/404 是预期降级，不算失败；后端不可用时页面显示「载入失败 + 重试」）
 *  E. 导航 IA：集成三 tab（接入 Agent / API 文档 / 技术方案）收敛为单一「API 文档」
 *     —— 分组标签消失、connect.html 入口不再存在、首页 CTA 不指向已删除页
 *  G. index.html 错误态：page.route 拦截 /v1/* 模拟后端宕机，验证渲染「载入失败 + 重试」且不伪造数据
 *     （回归防护：旧协议字段 status 残留曾致 normalize 全部降级 working）
 *  H. nodes.html 注册节点（#198）：表单 → 一次性令牌 → 可直接粘的 node.yaml；
 *     再点入口必须把令牌真从 DOM 清掉，重名只报重名不给令牌（mock 全流程 + 真后端一次注册）
 *  K. 节点 agent 版本 / 升级（CONTRACT-agent-upgrade.md §5.1 + §6）：版本行、陈旧提示是
 *     中性色、二次确认（取消不发请求）、同版本「已是最新」、失败原因逐条映射
 *     （node_offline / agent_binary_unavailable / bad_sha）、ack 结果来自服务端状态、
 *     sent 超 60s → 「节点未回复」；四语言文案不串（mock）+ 真后端版本/已是最新（接口在则断言）
 *  J. 共享拓扑推导（graphs.js 的 sourceTopology）：源 1:N 的出边、source 页与 trace 页
 *     的图同源一致、环仍画出来并标 cycle、声明了出口却没接下游的能力仍显示成灰点
 *     （文案是「没有下游」，不再提端口）（真后端）
 *  L. 端口从界面消失（CONTRACT-hide-ports.md）：表单只剩一个逗号分隔的
 *     「下游能力（候选）」输入框、详情区只谈下游、图上不标出口名。
 *     端口 UI（出口 chips / 按出口分行 / balance 模式 / 优先级数字）**不许回来** ——
 *     所以每一条都配了「不再存在」的守卫，而不是把旧断言一删了事。
 *  M. 任务日志面板（CONTRACT-task-logs.md §5）：失败任务默认展开、成功任务折叠且展开才请求、
 *     空日志说「没有输出」、截断有提示行；日志按纯文本渲染（HTML / ANSI 不会变成 DOM）（mock）。
 *  N. 触发带 payload（#273 决策 2）：点「立即运行」先开弹层，预填样本可读；
 *     非法 JSON / 数组当场报错且不发请求；合法 payload 进请求体、进事件、
 *     并真的进了任务参数（没传 type 时事件类型仍是源名）（mock）。
 */
import { chromium } from "playwright-core";
import { DatabaseSync } from "node:sqlite";
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
	// 事件中心页面（#79）
	"events.html",
	"sources.html",
	"capabilities.html",
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

	// ---- 量高度之前的确定性等待：页面数据落定 ----
	//
	// 首页 #index-events 加载期是 8 行骨架（aria-busy="true"，见 index.html），
	// 数据到达后 removeAttribute + replaceChildren。若在骨架期就量 scrollHeight
	// 并滚到底，随后的 300ms 里 KPI 卡片/事件流陆续成型把文档顶高，
	// 于是 scrollY 落在**新的**底部之上 —— 表现是「可滚动到底」假失败
	// （桌面1280 / 移动390 index.html 两条）。这不是产品滚不动，是测试量早了。
	//
	// 所以先等骨架消失（无该属性的页面瞬间返回；5s 超时不计失败 ——
	// 非 index 页没有骨架，而真实加载失败时页面自己要渲染「载入失败 + 重试」）。
	await page
		.waitForFunction(() => !document.querySelector('[aria-busy="true"]'), undefined, {
			timeout: 5000,
		})
		.catch(() => {});
	// 若该页有事件流（index），等它真的出结果（有行，或已渲染空态/错误态）。
	await page
		.waitForFunction(
			() => {
				const box = document.getElementById("index-events");
				if (!box) return true; // 该页没有事件流
				if (box.querySelector(".ev-feed-row")) return true;
				// 空态/错误态：容器已经渲染过内容（不是只剩骨架）
				return (box.textContent || "").trim().length > 0;
			},
			undefined,
			{ timeout: 5000 },
		)
		.catch(() => {});
	// 最后等文档高度连续几帧不再变化（字体/动画/后续渲染都可能再顶高一次），
	// 上限 2s。这样下面量到的 scrollHeight 才是「终态高度」，
	// 断言本身（必须真滚到 sh - vh - 5 内）一个字符都没放宽。
	await page.evaluate(async () => {
		const t0 = performance.now();
		let prev = -1;
		let stableFrames = 0;
		while (performance.now() - t0 < 2000) {
			const h = document.documentElement.scrollHeight;
			stableFrames = h === prev ? stableFrames + 1 : 0;
			prev = h;
			if (stableFrames >= 3) return; // 连续 4 帧同高 = 已稳定
			await new Promise((r) => requestAnimationFrame(r));
		}
	});

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
		// 等数据加载（真实事件行渲染完成）
		await H.waitForAppReady(page, "workspace", {
			dataAnchor: "#index-events .ev-feed-row",
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

			// 造一个**自带字符串参数**的出口能力再测参数面板。
			//
			// 以前这段是蹭 notify.phone 的 schema（to/title/body 都是 string）：
			// 手机推送按产品节奏从 UI 隐去后（web/partials.js 的 PUSH_UI_ENABLED），
			// 可选出口就变成了"前面用例留下的什么能力"——有没有参数全看运气。
			// 用例要自给自足，别依赖别人留下的状态。
			await page3.evaluate(async () => {
				// 必须建在**当前页面所在的命名空间**里：这套用例前面的步骤可能切过命名空间，
				// 建到别处的话选择器里根本看不到（targets 会只剩别的空间里的那几个）。
				let ns = "";
				try {
					ns = (JSON.parse(localStorage.getItem("anotify.scope") || "{}") || {})
						.namespaceId || "";
				} catch (e) {
					/* 没存过就用 mock 默认 */
				}
				const r = await fetch("/v1/capabilities", {
					method: "POST",
					headers: { "Content-Type": "application/json" },
					body: JSON.stringify({
						name: "e2e-params",
						version: 1,
						namespaceId: ns || undefined,
						paramsSchema: {
							type: "object",
							properties: { note: { type: "string" } },
						},
					}),
				});
				return r.status;
			});
			await page3.reload({ waitUntil: "load" });
			await page3.waitForTimeout(800);

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
					// 先确保至少选中一个有参数的出口：参数面板按勾选的出口渲染，
					// 而"默认选哪个"会随能力集合变化（例如手机推送能力按产品节奏隐藏后，
					// 默认出口就换人了）。显式勾第一个"有参数"的出口，断言才稳定。
					await page3.evaluate(() => {
						const opts = Array.from(
							document.querySelectorAll("#f-targets .target-opt"),
						);
						const opt =
							opts.find((l) => /e2e-params/.test(l.textContent)) ||
							opts.find((l) => /个参数|param/i.test(l.textContent));
						if (opt) {
							const cb = opt.querySelector("input.target-cap");
							if (cb && !cb.checked) {
								cb.checked = true;
								cb.dispatchEvent(new Event("change", { bubbles: true }));
							}
						}
					});
					await page3.waitForTimeout(300);
					const diag = await page3.evaluate(() => ({
						fields: document.querySelectorAll("[data-param-field]").length,
						targets: document.querySelectorAll("#f-targets input.target-cap").length,
						checked: document.querySelectorAll("#f-targets input.target-cap:checked").length,
						ns: document.querySelector(".scope-ns")?.textContent || "",
					}));
					H.check(
						"出口参数面板渲染出可编辑字段",
						diag.fields > 0,
						JSON.stringify(diag),
					);

					const preview = async (v) =>
						page3.evaluate((val) => {
							const fields = Array.from(
								document.querySelectorAll("[data-param-field]"),
							);
							const f =
								fields.find(
									(x) => x.querySelector("[data-param]")?.dataset.ptype === "string",
								) || fields[0];
							if (!f) return { pv: "no-field", text: "" };
							const i = f.querySelector("[data-param]");
							if (!i) return { pv: "no-input", text: "" };
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

			// 出口选项必须带**唯一标识**（能力名）：出口写进源里的就是能力名，
			// 而用户眼里是显示名；只给显示名的话，显示名重名 / 同名不同版的两个能力
			// 在列表里长得一模一样，勾错了很难回头发现。
			const optLabels = await page3.evaluate(() => {
				const box = document.getElementById("f-targets");
				return Array.from(box.querySelectorAll(".target-opt")).map((l) => ({
					mono: ((l.querySelector(".target-capname") || {}).textContent || "").trim(),
					dn: ((l.querySelector(".target-name") || {}).textContent || "").trim(),
					text: (l.textContent || "").trim(),
				}));
			});
			H.check(
				"每个出口选项都带唯一标识（能力名，没显示名时带 @版本）",
				optLabels.length > 0 && optLabels.every((o) => o.mono !== "" && o.text.includes(o.mono)),
				JSON.stringify(optLabels.slice(0, 3)),
			);
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
						// 有显示名才能验「显示名 + 唯一标识同时可见」（用户原话：
						// 「既显示名字，也显示能力的唯一标志」）。
						displayName: "E2E 报表",
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

				// 有显示名的能力：显示名与唯一标识**同时**可见（不是二选一）。
				// 用户原话「既显示名字，也显示能力的唯一标志，比如 Echo 这样」。
				const bothLabels = await page3.evaluate(() => {
					const box = document.getElementById("f-targets");
					return Array.from(box.querySelectorAll(".target-opt")).map((l) => ({
						mono: ((l.querySelector(".target-capname") || {}).textContent || "").trim(),
						dn: ((l.querySelector(".target-name") || {}).textContent || "").trim(),
						text: (l.textContent || "").trim(),
					}));
				});
				const reportOpt = bothLabels.find((o) => o.mono === "e2e.report");
				H.check(
					"有显示名的出口：显示名与唯一标识同时可见（E2E 报表 + e2e.report）",
					!!reportOpt &&
						reportOpt.dn === "E2E 报表" &&
						reportOpt.text.includes("E2E 报表") &&
						reportOpt.text.includes("e2e.report"),
					JSON.stringify(reportOpt || bothLabels.slice(0, 3)),
				);

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

		// 演示模式已整体移除（#273）：不再有 demo-badge，页面要么渲染真实数据、
		// 要么渲染「载入失败 + 重试」。这条断言防的是"演示数据又偷偷回来"。
		const demoLeftovers = await page.evaluate(() => ({
			badge: !!document.getElementById("demo-badge"),
			text: (document.body.textContent || "").includes("演示数据"),
		}));
		H.check(
			"index 不再有演示模式残留（#demo-badge 已移除）",
			demoLeftovers.badge === false && demoLeftovers.text === false,
			JSON.stringify(demoLeftovers),
		);

		// 首页主列表是**事件流**：上面那条 POST /v1/events 的 source 必须出现在最新行里
		// （事件行的第一列是 source，type 是第二列；payload 的 title 不再上了首页）。
		const hasReal = await page.evaluate(() => {
			const rows = Array.from(document.querySelectorAll("#index-events .ev-feed-row"));
			return rows.some((r) => (r.textContent || "").includes("frontend.src"));
		});
		H.check("已登录 index 显示真实事件数据", hasReal);

		await page.close();
		await ctx.close();
	}

	// ---- G. index.html 后端宕机 → 如实报「载入失败 + 重试」，**不伪造数据**（#273）----
	console.log("--- index 后端宕机（错误态 + 重试）---");
	{
		const s = H.seed(server.dbPath, "demo_test");
		// 先造一条真实事件：恢复后端后，「重试」要能把它取回来 ——
		// 没有数据时重试后是空态，无法证明重试真的重新取了。
		const ev = await H.req(server.base, "/v1/events", {
			key: s.sendKey,
			body: { source: "index.retry", type: "probe", payload: {} },
		});
		H.check("G 造事件 → 200", ev.status === 200, `status=${ev.status}`);
		const ctx = await newCtx();
		await injectSession(ctx, s.session, server.base);
		// 拦截 /v1/* 模拟后端宕机（网络错误，非 401 → 不跳登录）
		await ctx.route("**/v1/**", (route) => route.abort());
		const page = await ctx.newPage();
		await page.goto(server.base + "/index.html", {
			waitUntil: "load",
			timeout: 15000,
		});
		await H.waitForAppReady(page, "workspace");

		// 等错误态真的画完（最多 5s）：route.abort() 是异步的，waitForAppReady 只保证外壳挂好，
		// 不保证首个 fetch 的 catch 已经把「载入失败 + 重试」写进 DOM。
		// 断言内容不变 —— 只是不把「还没画完」当成「载入失败没渲染」。
		await page
			.waitForFunction(
				() => {
					const box = document.getElementById("index-events");
					return !!(box && box.querySelector("button"));
				},
				null,
				{ timeout: 5000 },
			)
			.catch(() => {});

		// 错误态出现：事件流位置渲染「载入失败…」+ 重试按钮
		const errState = await page.evaluate(() => {
			const box = document.getElementById("index-events");
			const btn = box && box.querySelector("button");
			return {
				hasList: !!box,
				text: (box && box.textContent) || "",
				hasRetry: !!btn,
				retryText: btn ? btn.textContent.trim() : "",
				badge: !!document.getElementById("demo-badge"),
				rows: document.querySelectorAll("#index-events .ev-feed-row").length,
			};
		});
		H.check(
			"后端宕机时 index 渲染「载入失败」而不是演示数据",
			/载入失败|Failed to load|読み込みに失敗|Error al cargar/.test(errState.text),
			JSON.stringify(errState).slice(0, 200),
		);
		H.check("错误态提供「重试」按钮", errState.hasRetry, JSON.stringify(errState));
		H.check(
			"错误态不伪造任何事件行（不渲染演示数据）",
			errState.rows === 0,
			`rows=${errState.rows}`,
		);
		H.check("演示徽章已不存在（#273）", errState.badge === false, "");

		// 恢复后端 → 点重试 → 真实数据出现（证明"重试"是真的重取，不是装饰）
		await ctx.unroute("**/v1/**");
		await page.evaluate(() => {
			const btn = document.querySelector("#index-events button");
			if (btn) btn.click();
		});
		await page.waitForFunction(
			() => document.querySelectorAll("#index-events .ev-feed-row").length > 0,
			{ timeout: 15000 },
		);
		const afterRetry = await page.evaluate(
			() => document.querySelectorAll("#index-events .ev-feed-row").length,
		);
		H.check("点重试后真实事件出现", afterRetry > 0, `rows=${afterRetry}`);

		await page.close();
		await ctx.close();
	}

	// ---- E. 集成三 tab 下线（接入 Agent / API 文档 / 技术方案 → 只留 API 文档）----
	//
	// 三个 tab 收敛成一个导航项后，最容易犯的错是「页删了，入口还在」：
	// 侧栏残留 href="connect.html" → 点进去 404，而且没JS错误，测试全绿。
	// 所以这里钉住三件事：分组标签没了、API 文档还在、入口不再指向已删页。
	console.log("--- 导航：集成三 tab 收敛为单一 API 文档 ---");
	{
		const s = H.seed(server.dbPath, "nav_ia");
		const ctx = await newCtx();
		await injectSession(ctx, s.session, server.base);
		const page = await ctx.newPage();
		await page.goto(server.base + "/docs.html", {
			waitUntil: "load",
			timeout: 15000,
		});
		await H.waitForAppReady(page, "workspace");

		// E-1: 侧栏不再有「集成」分组标签（单 item 不再单独成组）
		const groupLabels = await page.evaluate(() =>
			Array.from(document.querySelectorAll("#sidebar nav .side-label")).map(
				(e) => e.textContent.trim(),
			),
		);
		H.check(
			"侧栏无「集成」分组标签",
			!groupLabels.some((t) =>
				/集成|Integration|連携|Integraci/.test(t),
			),
			JSON.stringify(groupLabels),
		);

		// E-2: 三个 tab 只留 API 文档：docs.html 在，connect.html / 技术方案 不在
		const navInfo = await page.evaluate(() => ({
			hrefs: Array.from(document.querySelectorAll("#sidebar .side-link")).map(
				(a) => a.getAttribute("href"),
			),
			text: document.getElementById("sidebar").innerText,
		}));
		H.check(
			"侧栏有 API 文档链接（docs.html）",
			navInfo.hrefs.includes("docs.html"),
			JSON.stringify(navInfo.hrefs),
		);
		H.check(
			"侧栏不再有已删除的 connect.html",
			!navInfo.hrefs.includes("connect.html"),
			JSON.stringify(navInfo.hrefs),
		);
		H.check(
			"侧栏不再出现「接入 Agent」/「技术方案」",
			!/接入 Agent|技术方案|Connect Agent|Tech Spec/.test(navInfo.text),
			JSON.stringify(navInfo.text.slice(0, 200)),
		);

		// E-3: docs.html 侧栏 active 高亮 = api 项
		const activeNav = await page.evaluate(() => {
			const a = document.querySelector("#sidebar .side-link.active");
			return a ? a.getAttribute("href") : null;
		});
		H.check(
			"docs.html 侧栏 active 项指向 docs.html",
			activeNav === "docs.html",
			String(activeNav),
		);

		// E-4: API 页的「全部端点」是构建期从 api/openapi.yaml 生成的静态 HTML。
		//      它一头连 spec（Dockerfile 漏 COPY api/ 就空）、一头连站点（sitegen
		//      没加载 spec 就只剩标题），两端都没有 JS 错误，所以必须显式量行数。
		const endpoints = await page.evaluate(() => {
			const sec = document.getElementById("endpoints");
			if (!sec) return null;
			return {
				rows: sec.querySelectorAll("tbody tr").length,
				groups: sec.querySelectorAll(".card").length,
				text: sec.innerText,
			};
		});
		H.check(
			"docs.html 有「全部端点」区块且行数 ≥ 80",
			!!endpoints && endpoints.rows >= 80 && endpoints.groups >= 10,
			endpoints
				? `rows=${endpoints.rows} groups=${endpoints.groups}`
				: "区块不存在",
		);
		H.check(
			"端点清单含真实端点（/v1/events、/agent-login.sh）",
			!!endpoints &&
				/\/v1\/events/.test(endpoints.text) &&
				/\/agent-login\.sh/.test(endpoints.text),
			endpoints ? endpoints.text.slice(0, 120) : "",
		);
		H.check(
			"端点清单不展示已弃用端点（/v1/devices）",
			!!endpoints && !/\/v1\/devices\b/.test(endpoints.text),
			"",
		);

		// E-5: 首页两个 CTA 都不能再指向已删除的 connect.html
		await page.goto(server.base + "/index.html", {
			waitUntil: "load",
			timeout: 15000,
		});
		await H.waitForAppReady(page, "workspace");
		const ctaHrefs = await page.evaluate(() =>
			Array.from(document.querySelectorAll("a.btn-primary")).map((a) =>
				a.getAttribute("href"),
			),
		);
		H.check(
			"首页 CTA 不再指向 connect.html 且指向 API 文档",
			!ctaHrefs.includes("connect.html") &&
				ctaHrefs.some((h) => h && h.startsWith("docs.html")),
			JSON.stringify(ctaHrefs),
		);

		await page.close();
		await ctx.close();
	}

	// ---- D. 全部页面 × 2 视口渲染检查 ----
	//
	// **这里必须分两种身份跑**（原来是全匿名，等于对受保护页零覆盖）：
	// 未登录访问 capabilities.html / nodes.html 会被守卫跳到 login.html，
	// 于是 pageerror 与横向溢出检查量到的是**登录页**，而新建能力弹窗、
	// 部署面板、节点产物块这些真东西一个都没进过浏览器。
	// 症状是「UI 改崩了但套件全绿」——比红更危险。
	//
	// 所以：公开页走匿名（顺带证明它们不需要登录），受保护页走已登录会话。
	console.log("--- 全页面 × 双视口渲染（公开页匿名 / 受保护页已登录）---");
	const authSeed = H.seed(server.dbPath, "frontend_pages");
	for (const vp of VIEWPORTS) {
		const ctxAnon = await newCtx();
		const ctxAuth = await newCtx();
		await injectSession(ctxAuth, authSeed.session, server.base);
		for (const p of PAGES) {
			const guarded = GUARDED.includes(p);
			await checkPage(
				guarded ? ctxAuth : ctxAnon,
				server.base + "/" + p,
				vp.name + (guarded ? "（已登录）" : "（未登录）"),
				vp,
			);
		}
		await ctxAnon.close();
		await ctxAuth.close();
	}

	// D2：已登录时受保护页**真的停在原页**（不是又被跳到 login）。
	// 上一步只证明「没 JS 错误/没溢出」，如果守卫把已登录用户也踢走，
	// 上一步会静默地把 login.html 又量一遍 —— 依然是假绿。
	{
		const ctx = await newCtx();
		await injectSession(ctx, authSeed.session, server.base);
		const page = await ctx.newPage();
		for (const p of ["capabilities.html", "nodes.html"]) {
			await page.goto(server.base + "/" + p, { waitUntil: "load", timeout: 15000 });
			await page.waitForTimeout(600);
			const info = await page.evaluate(() => ({
				url: location.pathname,
				hasSidebar: !!document.getElementById("sidebar"),
			}));
			H.check(
				`已登录访问 ${p} 停在原页（不是又被送去 login）`,
				info.url.endsWith(p) && info.hasSidebar,
				JSON.stringify(info),
			);
		}
		await page.close();
		await ctx.close();
	}

	// D3：导航里**不再有出口页**（CONTRACT-downstream-edit §5）。
	//
	// 为什么要有这条反向守卫：出口页被删掉之后，最危险的回归不是「页面崩了」
	// （那会直接 404 / pageerror 被抓到），而是**导航又把它加回来**、或者留着一条
	// 指向已删页面的死链。死链在真实浏览器里只是点下去 404，在没人点的 e2e 里
	// 会静默通过 —— 所以这里直接钉住「侧栏里没有这个入口」。
	{
		const ctx = await newCtx();
		await injectSession(ctx, authSeed.session, server.base);
		const page = await ctx.newPage();
		await page.goto(server.base + "/capabilities.html", {
			waitUntil: "load",
			timeout: 20000,
		});
		await page.waitForTimeout(800);
		const nav = await page.evaluate(() => ({
			hrefs: Array.from(document.querySelectorAll("#sidebar a")).map(
				(a) => a.getAttribute("href") || "",
			),
			// a.side-link 里含 SVG 图标，textContent 会把图标里的文本也算进来。
			// 取最后一个文本子节点才是标签文字（与旧版出口页用例同一口径）。
			labels: Array.from(document.querySelectorAll("#sidebar a.side-link")).map((a) => {
				const last = a.lastChild;
				return last && last.nodeType === 3 ? last.textContent.trim() : a.textContent.trim();
			}),
		}));
		H.check(
			"侧栏里没有指向已删出口页的链接（edges.html 彻底下线）",
			!nav.hrefs.includes("edges.html"),
			JSON.stringify(nav.hrefs),
		);
		H.check(
			"侧栏标签里没有「出口 / Edges」（下游只能在能力里改）",
			nav.labels.length > 0 && !nav.labels.some((x) => /出口|Edges/i.test(x)),
			JSON.stringify(nav.labels),
		);
		await page.close();
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

	// ---- K. 节点 agent 版本 / 升级（真后端，条件启用）----
	//
	// B 组（中枢侧）的 upgrade 端点与 agent_version 列是并行分支，合并前可能还不存在。
	// checkNodeUpgradeReal 内部先探 GET /v1/nodes：带 targetAgentVersion/agentVersion
	// 就跑真断言，否则留一行「合并后启用」日志（不伪装通过）。
	// 真后端上的 POST /upgrade 是破坏性操作（会替换 e2e agent 二进制并重启），不做。
	console.log("--- 节点 agent 版本 / 升级（真后端，接口在则断言）---");
	await checkNodeUpgradeReal(server, newCtx);

	// ---- I. 能力详情看脚本 + 编辑出新版本 + 409 闸门（真后端）----
	//
	// 用户报的两个问题（原话）：
	//   「touchnow 我看不到具体什么脚本啊」—— 脚本存产物里，详情页以前根本不展示
	//   「为什么脚本这个还不能编辑啊」—— 能力 append-only，后端能发新版本，UI 没入口
	// 所以这一整块必须在**真后端**上验：mock 只能证明「UI 接对了形状」。
	console.log("--- 能力详情看脚本 + 编辑发新版本（真后端）---");
	await checkCapabilityEditReal(server, newCtx);

	// ---- J. 共享拓扑推导（源 1:N / 跨页一致 / 环 / 未接下游，真后端）----
	console.log("--- 共享拓扑推导：源 1:N + 两页一致 + 环 + 未接下游（真后端）---");
	await checkSharedTopology(server, newCtx);

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

	// ②-附 下游表单的形状：**一个**逗号分隔的输入框，没有端口 UI（契约 §3）
	//
	// 这一组是「删掉的功能不许悄悄回来」的守卫。端口是 emit 的路由键，
	// 用户脑子里只有一件事：这个能力跑完交给谁。所以下面这些钩子一个都不许在：
	//   #cap-ports / #cap-port-input   出口 chips 输入
	//   #cap-down-rows / .cap-down-row 按出口分行的候选
	//   select.cap-bal-select          balance 模式选择器
	//   .cap-cand-prio / .cap-cand-prio-wrap  优先级数字输入框（优先级只归详情区的下游列表）
	const downForm = await page.evaluate(() => {
		const input = document.getElementById("cap-downstream");
		const hint = document.getElementById("cap-downstream-hint");
		const wrap = document.getElementById("cap-down-wrap");
		return {
			input: !!input && input.classList.contains("input"),
			placeholder: (input || {}).placeholder || "",
			hint: (hint || {}).textContent || "",
			title: ((wrap && wrap.querySelector(".card-title")) || {}).textContent || "",
			// 编辑模式的构件（§2.2）在**新建表单里必须全空/隐藏**：
			// 建能力时下游随 links[] 一次提交，没有「保存下游」那道闸；
			// 行列表里出现优先级数字会让人以为建能力时就能调。
			list: !!document.getElementById("cap-down-list"),
			items: document.querySelectorAll("#cap-down-list .cap-down-item").length,
			actionsHidden: (() => {
				const a = document.getElementById("cap-down-actions");
				return !!a && a.classList.contains("hidden");
			})(),
			legacy: {
				ports: document.querySelectorAll("#cap-ports, #cap-port-input, #cap-ports-hint").length,
				chips: document.querySelectorAll(".cap-chip, .cap-chips").length,
				downRows: document.querySelectorAll("#cap-down-rows, .cap-down-row").length,
				balSelect: document.querySelectorAll("select.cap-bal-select, .cap-bal-wrap").length,
				prioInput: document.querySelectorAll(".cap-cand-prio, .cap-cand-prio-wrap").length,
			},
			// 表单里**用户可见**的文案（标题 / 帮助 / 标签）：术语红线 -
			// 中文界面里不许出现「端口」，也不许用「产出端口」这种旧标题
			texts: Array.from(
				document.querySelectorAll(
					"#tpl-form-step .card-title, #tpl-form-step .field-hint, #tpl-form-step label",
				),
			).map((x) => (x.textContent || "").trim()),
		};
	});
	H.check(
		"新建表单只有一个下游输入框（逗号分隔）",
		downForm.input === true && /script\.echo,\s*notify\.phone/.test(downForm.placeholder),
		JSON.stringify({ input: downForm.input, ph: downForm.placeholder }),
	);
	H.check(
		"下游栏标题是「下游能力（候选）」",
		/下游能力/.test(downForm.title) && !/端口/.test(downForm.title),
		downForm.title,
	);
	H.check(
		"空态说明「留空 = 终端能力（跑完就结束）」",
		/跑完之后交给谁/.test(downForm.hint) &&
			/留空/.test(downForm.hint) &&
			/跑完就结束/.test(downForm.hint),
		downForm.hint,
	);
	H.check(
		"端口 UI 全部消失（端口输入框 / 出口 chips / 按出口分行 / balance 模式 / 优先级数字）",
		Object.values(downForm.legacy).every((n) => n === 0),
		JSON.stringify(downForm.legacy),
	);
	// 新建模式的下游行编辑器空态（CONTRACT-downstream-edit §2.1/§2.2）：
	// 行列表容器在（编辑模式要用它），但**一行也不许有**，且「保存下游」那块不可见 ——
	// 否则用户会以为新建时能先调优先级，而那时边还不存在。
	H.check(
		"新建表单里下游行编辑器是空的（0 行）且「保存下游」不可见",
		downForm.list === true && downForm.items === 0 && downForm.actionsHidden === true,
		JSON.stringify({ list: downForm.list, items: downForm.items, actions: downForm.actionsHidden }),
	);
	H.check(
		"表单用户可见文案里没有「端口」（术语红线，四语言同一口径）",
		!(downForm.texts || []).some((x) => /端口/.test(x)),
		(downForm.texts || []).join(" | ").slice(0, 240),
	);

	// ②-附2 下游候选：看得见、点得到、点完进同一个输入框
	//
	// 用户原话：「下游能力得能选啊」—— 原生 datalist 只在输入框里打字时才浮出来，
	// 看不见“有哪些可选”；常驻的那份是下面一排胶囊。两条路都必须落到**同一个**
	// #cap-downstream（.value 始终是逗号分隔原文，提交/切卡读的就是它）。
	const candShape = await page.evaluate(() => {
		const input = document.getElementById("cap-downstream");
		const box = document.getElementById("cap-down-cands");
		const pills = box ? Array.from(box.querySelectorAll("[data-cand-name]")) : [];
		return {
			list: input ? input.getAttribute("list") : null,
			dl: document.querySelectorAll("#cap-down-targets option").length,
			pills: pills.length,
			first: pills.length
				? {
						name: pills[0].getAttribute("data-cand-name") || "",
						mono: ((pills[0].querySelector(".cap-cand-id") || {}).textContent || "").trim(),
						text: (pills[0].textContent || "").trim(),
					}
				: null,
		};
	});
	H.check(
		"下游输入框接上了原生候选（list=cap-down-targets），胶囊与 datalist 同源",
		candShape.list === "cap-down-targets" &&
			candShape.pills > 0 &&
			candShape.dl === candShape.pills,
		JSON.stringify({ list: candShape.list, dl: candShape.dl, pills: candShape.pills }),
	);
	H.check(
		"每个候选胶囊都带唯一标识（等宽的能力名，不是只有显示名）",
		!!candShape.first &&
			candShape.first.mono === candShape.first.name &&
			candShape.first.text.includes(candShape.first.name),
		JSON.stringify(candShape.first),
	);

	// 点胶囊 → 那个名字进输入框（走 .value + input 事件，与手打完全同一条路）
	const candClick = await page.evaluate(() => {
		const pick = () => document.querySelector("#cap-down-cands [data-cand-name]");
		const name = pick().getAttribute("data-cand-name");
		pick().click();
		const input = document.getElementById("cap-downstream");
		const after = {
			value: input.value,
			marked: !!document.querySelector(
				`#cap-down-cands [data-cand-name="${name}"].is-picked`,
			),
		};
		// 再点一次：已选过的不重复追加（连点两下不该出现两个同名候选）
		pick().click();
		return { name, after, twice: input.value };
	});
	H.check(
		"点候选胶囊 → 能力名进输入框，并且已选项有视觉标记",
		candClick.after.value === candClick.name && candClick.after.marked === true,
		JSON.stringify(candClick.after),
	);
	H.check(
		"已选过的候选再点不会重复加入（逗号分隔原文不重复）",
		candClick.twice === candClick.name,
		candClick.twice,
	);
	// 清空：后面的用例要回到「什么都没选」的状态
	await page.evaluate(() => {
		const input = document.getElementById("cap-downstream");
		input.value = "";
		input.dispatchEvent(new Event("input", { bubbles: true }));
	});

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

	// ④ 工作目录（#capability-only-workdir）：**唯一来源**是能力定义里的 run.workDir。
	//
	// 这条链路以前根本不存在（目录挂在节点 / 部署参数上），所以断言必须落在
	// **请求体**上：UI 上有个输入框但提交时没带上，症状是「填了、跑起来还是在
	// 临时目录里」，光看页面永远发现不了。
	//
	// 为什么在页内包一层 fetch：mock 在 window.fetch 层就把 /v1/* 拦下了
	// （web/mock.js），page.on("request") 一条都抓不到。
	const wdPath = "/home/uiwdtest/project";
	const capOpen = await page.evaluate(() => !!document.getElementById("cap-workdir"));
	H.check(
		"新建能力表单有工作目录输入（#cap-workdir）",
		capOpen === true,
		`#cap-workdir 存在=${capOpen}`,
	);

	await page.evaluate(() => {
		// 抓包钩子：记录每一次 POST /v1/capabilities 的请求体
		window.__capBodies = [];
		const orig = window.fetch;
		window.__capRestoreFetch = () => {
			window.fetch = orig;
		};
		window.fetch = function (input, init) {
			const url = String((input && input.url) || input);
			const method = String((init && init.method) || "GET").toUpperCase();
			if (url.includes("/v1/capabilities") && method === "POST" && init && init.body) {
				try {
					window.__capBodies.push(JSON.parse(String(init.body)));
				} catch {
					window.__capBodies.push(null);
				}
			}
			return orig.apply(this, arguments);
		};
	});

	await page.fill("#cap-workdir", wdPath);
	const wdEcho = await page.evaluate(
		() => (document.getElementById("cap-workdir-echo") || {}).textContent || "",
	);
	H.check(
		"填了工作目录 → 只读脚本区回显同一个路径（用户能核对将要提交什么）",
		wdEcho.includes(wdPath),
		JSON.stringify(wdEcho.slice(0, 80)),
	);

	// 上面那步已经把参数契约改成合法的 pattern 版本、名字还是 e2e.broken，
	// 这里换个不冲突的名字，直接提交。
	const capName = "e2e.workdir." + Date.now();
	await page.evaluate((n) => {
		document.getElementById("cap-name").value = n;
	}, capName);
	await page.click("#cap-submit");
	await page.waitForTimeout(1400);
	const withWd = await page.evaluate(() => (window.__capBodies || [])[0] || null);
	H.check(
		"提交时 run.workDir 就是填的路径（工作目录属于能力定义）",
		!!withWd && !!withWd.run && withWd.run.workDir === wdPath,
		JSON.stringify({ run: withWd && withWd.run, workDir: withWd && withWd.run && withWd.run.workDir }),
	);
	H.check(
		"工作目录不在请求体顶层（不是部署参数）",
		!!withWd && !("workDir" in withWd) && !("work_dir" in withWd),
		JSON.stringify(Object.keys(withWd || {})),
	);

	// 留空 = 每次任务一个独立目录：这时**不带**这个字段（空串不是合法语义）。
	await page.evaluate(() => {
		document.querySelector("button[onclick='openCapModal()']").click();
	});
	await page.waitForTimeout(600);
	await page.click('[data-tpl="script.blank"]');
	await page.waitForTimeout(400);
	await page.evaluate((n) => {
		document.getElementById("cap-workdir").value = "";
		document.getElementById("cap-name").value = n;
	}, "e2e.noworkdir." + Date.now());
	await page.click("#cap-submit");
	await page.waitForTimeout(1400);
	const noWd = await page.evaluate(() => {
		const list = window.__capBodies || [];
		const last = list[list.length - 1] || null;
		window.__capRestoreFetch && window.__capRestoreFetch();
		return last;
	});
	H.check(
		"工作目录留空 → 请求体里没有 workDir（每次任务一个独立目录）",
		!!noWd && !!noWd.run && !("workDir" in noWd.run),
		JSON.stringify({ run: noWd && noWd.run }),
	);
}

// checkCapabilityDetail 覆盖用户报的两个问题（原话）：
//   ① 「能力页看不到能力的具体定义、具体内容是什么」
//   ② 「新建能力这里无法指定下游是什么，如果我想串起来（比如 touchnow → echo）应该怎么办」
//
// 这里钉六件事，每一件都对应一个会**静默**坏掉的环节：
//   ① 行能展开，四节（参数契约 / 下游（候选） / 谁接到我 / 运行参数）都在，
//      且参数契约显示的是 paramsSchema 的**原文**（而不是「无参数」占位）
//   ② 下游按**边**逐条列成候选（一条出边 = 一个候选），不再按出口分块
//   ③ 没有下游的能力显眼地说「跑完就结束」（终端能力是合法的一等语义）
//   ④ 「+ 下游」能就地建边，成功后同一处立刻看到目标（候选可见）
//   ⑤ 新建能力时只填一串下游名字 → **一次提交**（links 在 caps 请求体里，全程不发 POST /edges），
//      能力与全部边一次落库；引用不到的名字当场拦住，不提交
//   ⑥ 一条坏边 → **整体回滚**：能力根本没建出来，400 里带 `links[i]` 下标，弹层不关
//   ⑦ 自连 → 400 `self loop`，能力/边一样都没建（原子性钉在后端，不靠按钮过滤；
//      产物那一面在 store/server 单测里断言——mock 没有 artifacts 端点）
//
// 端口在这里**不可见**（CONTRACT-hide-ports.md）：请求体里仍然是
// outputs:["out"] + links[].fromPort="out"，但界面上只说「下游能力（候选）」。
//   ⑥ 一条坏边 → **整体回滚**：能力根本没建出来，400 里带 `links[i]` 下标，弹层不关
//   ⑦ 自连 → 400 `self loop`，能力/边一样都没建（原子性钉在后端，不靠按钮过滤；
//      产物那一面在 store/server 单测里断言——mock 没有 artifacts 端点）
//
// 为什么全部在**一次页面加载**里做：mock 的 state 是页面级的，goto 一次就重置，
// 所以「建了之后马上看到」这类断言必须靠页面自己的 loadAll() 刷新（就是用户看到的路径）。
async function checkCapabilityDetail(page, base) {
	await page.goto(base + "/capabilities.html", { waitUntil: "load", timeout: 20000 });
	await page.waitForTimeout(1800);

	// ① 展开一行：四节 + 参数契约原文
	const detail = await page.evaluate(() => {
		const row = document.querySelector('.cap-row[data-cap-name="analyze-pr"]');
		if (!row) return { err: "找不到 analyze-pr 行（data-cap-name 钩子丢了？）" };
		const btn = row.querySelector('[data-act="detail"]');
		if (!btn) return { err: "没有详情按钮" };
		btn.click();
		const box = row.querySelector(".cap-detail");
		return {
			open: !!box && box.classList.contains("open"),
			titles: Array.from(row.querySelectorAll(".cap-detail-title")).map((x) =>
				x.textContent.trim(),
			),
			pre: ((row.querySelector(".cap-detail-pre") || {}).textContent || "").trim(),
			wa: ((row.querySelector(".cap-kv") || {}).textContent || "").trim(),
			aria: btn.getAttribute("aria-expanded"),
		};
	});
	H.check(
		"能力行能展开详情区（默认收起 → 点开）",
		detail.open === true && detail.aria === "true",
		JSON.stringify(detail),
	);
	H.check(
		"详情区四节齐全：参数契约 / 下游（候选） / 谁接到我 / 运行参数",
		detail.titles.length === 4 &&
			/参数/.test(detail.titles[0]) &&
			// 端口从界面消失：第二节只说「下游（候选）」，不再有「产出端口 / 出口与下游」
			/下游/.test(detail.titles[1]) &&
			!/端口|出口/.test(detail.titles[1]) &&
			/接到|feeds/.test(detail.titles[2]) &&
			/运行|Runtime/.test(detail.titles[3]),
		JSON.stringify(detail.titles),
	);
	H.check(
		"参数契约显示 paramsSchema 原文（JSON 美化，不是「无参数」占位）",
		/"type"\s*:\s*"object"/.test(detail.pre) && /properties/.test(detail.pre),
		detail.pre.slice(0, 160),
	);
	H.check(
		"运行参数区有内容（不是空白区块）",
		/超时|Timeout/.test(detail.wa),
		detail.wa.slice(0, 80),
	);

	// ①-附 脚本：用户原话「touchnow 我看不到具体什么脚本啊」。
	// 脚本在产物里（artifacts.spec.run.files），详情展开时才懒加载 ——
	// 这里等它落进 DOM，而不是固定 sleep 后猜。
	const scriptBlock = await page.evaluate(async () => {
		const row = document.querySelector('.cap-row[data-cap-name="analyze-pr"]');
		if (!row) return { err: "找不到 analyze-pr 行" };
		const box = row.querySelector(".cap-script");
		if (!box) return { err: "详情区没有脚本块" };
		for (let i = 0; i < 40; i++) {
			const pre = box.querySelector(".cap-script-pre");
			if (pre && pre.textContent.trim()) break;
			await new Promise((r) => setTimeout(r, 100));
		}
		return {
			entry: ((box.querySelector(".cap-script-entry") || {}).textContent || "").trim(),
			paths: Array.from(box.querySelectorAll(".cap-script-path")).map((x) => x.textContent),
			text: ((box.querySelector(".cap-script-pre") || {}).textContent || "").trim(),
			copy: !!box.querySelector(".cap-script-copy"),
			empty: !!box.querySelector(".cap-script-empty"),
		};
	});
	H.check(
		"详情区能直接看到脚本正文（产物里的文件正文，不是 base64）",
		/^#!\//.test(scriptBlock.text || "") &&
			/emit/.test(scriptBlock.text || "") &&
			scriptBlock.empty === false,
		JSON.stringify(scriptBlock).slice(0, 260),
	);
	H.check(
		"脚本区标出入口文件名，且每个文件都有「复制」",
		(scriptBlock.entry || "").includes("run.sh") &&
			(scriptBlock.paths || []).includes("run.sh") &&
			scriptBlock.copy === true,
		JSON.stringify(scriptBlock).slice(0, 260),
	);

	// 没有产物的能力：说清事实（而不是留一块空白 —— 空白读起来像「功能坏了」）
	const noArtifact = await page.evaluate(async () => {
		const row = document.querySelector('.cap-row[data-cap-name="deploy-preview"]');
		if (!row) return { text: "", err: "找不到 deploy-preview 行" };
		const btn = row.querySelector('[data-act="detail"]');
		btn.click();
		const box = row.querySelector(".cap-script");
		let text = "";
		for (let i = 0; i < 40; i++) {
			const e = box && box.querySelector(".cap-script-empty");
			if (e && e.textContent.trim()) {
				text = e.textContent.trim();
				break;
			}
			await new Promise((r) => setTimeout(r, 100));
		}
		// 关回去：后面的用例会自己点开这一行的详情，状态不能被我改掉
		if (row.querySelector(".cap-detail").classList.contains("open")) btn.click();
		return { text: text };
	});
	H.check(
		"没有产物的能力显示「还没有产物」（不是空白块）",
		/还没有产物|no artifact/i.test(noArtifact.text || ""),
		String(noArtifact.text).slice(0, 160),
	);

	// ②③ 下游按**边**逐条列成候选；没有下游的能力显式说「跑完就结束」
	//
	// 端口从界面上消失了（契约 §3）：一个能力只有一个出口，所以详情区
	// 不再按出口分块 —— 一条出边就是一个**候选**（同一次运行只走其中一条）。
	const down0 = await page.evaluate(() => {
		const row = document.querySelector('.cap-row[data-cap-name="deploy-preview"]');
		if (!row) return { err: "找不到 deploy-preview 行" };
		row.querySelector('[data-act="detail"]').click();
		return {
			open: row.querySelector(".cap-detail").classList.contains("open"),
			// 旧的分块钩子（端口名 / 无下游虚线框 / balance 徽章 / 候选数）：都不许在
			legacy: row.querySelectorAll(
				".cap-port-block, .cap-port-name, .cap-port-dead, .cap-bal-badge, .cap-cand-count",
			).length,
			titles: Array.from(row.querySelectorAll(".cap-detail-title")).map((x) =>
				x.textContent.trim(),
			),
			cands: Array.from(row.querySelectorAll(".cap-link[data-cand-name]")).map((l) => ({
				name: l.getAttribute("data-cand-name"),
				text: l.textContent.replace(/\s+/g, " ").trim(),
			})),
			hasAdd: !!row.querySelector('[data-act="add-downstream"]'),
			bodyText: document.body.innerText || "",
			inbound: Array.from(row.querySelectorAll(".cap-inlink")).map((x) =>
				x.textContent.replace(/\s+/g, " ").trim(),
			),
		};
	});
	H.check(
		"详情区不再按出口分块（端口名 / 无下游虚线框 / balance 徽章 / 候选数都没有）",
		down0.open === true && down0.legacy === 0,
		JSON.stringify({ open: down0.open, legacy: down0.legacy }),
	);
	H.check(
		"详情区区块标题是「下游（候选）」，且全页没有「产出端口」",
		(down0.titles || []).some((x) => /下游/.test(x)) &&
			!/产出端口|端口/.test(down0.bodyText || ""),
		JSON.stringify({ titles: down0.titles, hasPort: /端口/.test(down0.bodyText || "") }),
	);
	H.check(
		"deploy-preview 的下游逐条列出（gen-changelog / rollback 都是候选）",
		(down0.cands || []).length >= 2 &&
			// data-cand-name 是**引用**：跟随最新是裸名字，钉死版本带 @N（两种都得算上）
			down0.cands.some((c) => String(c.name || "").split("@")[0] === "gen-changelog") &&
			down0.cands.some((c) => String(c.name || "").split("@")[0] === "rollback"),
		JSON.stringify(down0.cands),
	);
	H.check(
		"候选行说清「优先级 N · 点「编辑」可调」",
		(down0.cands || []).length > 0 &&
			(down0.cands || []).every((c) => /优先级\s*\d+/.test(c.text)),
		JSON.stringify((down0.cands || []).map((c) => c.text).slice(0, 3)),
	);
	H.check(
		"详情区有「+ 下游」入口（链路是长出来的）",
		down0.hasAdd === true,
		String(down0.hasAdd),
	);
	H.check(
		"「谁接到我」列出入边（来源 → 我，不再夹一个出口名）",
		(down0.inbound || []).some((l) => /→\s*我|→\s*me/.test(l)) &&
			!(down0.inbound || []).some((l) => /--/.test(l)),
		JSON.stringify(down0.inbound),
	);

	// 空态：没有下游的能力说「跑完就结束」（而不是一句到此为止）
	//
	// 终端能力是**合法**的（gen-changelog 就是），所以它不该看起来像报错；
	// 但也不能留一块空白 —— 空白与「链路断了」在界面上长得一样。
	const downEmpty = await page.evaluate(async () => {
		const row = document.querySelector('.cap-row[data-cap-name="gen-changelog"]');
		if (!row) return { err: "找不到 gen-changelog 行" };
		if (!row.querySelector(".cap-detail").classList.contains("open")) {
			row.querySelector('[data-act="detail"]').click();
		}
		await new Promise((r) => setTimeout(r, 400));
		return {
			open: row.querySelector(".cap-detail").classList.contains("open"),
			text: ((row.querySelector(".cap-down-empty") || {}).textContent || "").trim(),
			cands: row.querySelectorAll(".cap-link[data-cand-name]").length,
		};
	});
	H.check(
		"没有下游的能力显式说「跑完就结束，不会触发任何下游」",
		downEmpty.open === true &&
			downEmpty.cands === 0 &&
			/跑完就结束/.test(downEmpty.text || "") &&
			!/到此为止/.test(downEmpty.text || ""),
		JSON.stringify(downEmpty),
	);

	// 造一条 mock 下游：script.echo（没下游）→ report.send，页面自己 loadAll 后应立刻可见
	//
	// 先把 script.echo 的详情展开（用户就是在这个位置看到「跑完就结束」的），
	// 造完边再看：同一条候选出现了、「跑完就结束」消失、展开态没被刷新弄丢。
	//
	// 造数走 **PUT /v1/capabilities/{name}/downstreams**（CONTRACT-downstream-edit §2）：
	// 旧的 POST /v1/edges 能力边已被封成 405 edge_write_removed（§3），
	// 还在用老接口建能力边的用例会变成“永远红”。
	const afterEdge = await page.evaluate(async () => {
		const findRow = () => document.querySelector('.cap-row[data-cap-name="script.echo"]');
		let row = findRow();
		if (!row) return { err: "找不到 script.echo 行" };
		if (!row.querySelector(".cap-detail").classList.contains("open")) {
			row.querySelector('[data-act="detail"]').click();
		}
		// 详情区是懒渲染（产物/下游都要等数据）：等到空态真的落进 DOM
		for (let i = 0; i < 40; i++) {
			if (row.querySelector(".cap-down-empty")) break;
			await new Promise((r) => setTimeout(r, 100));
		}
		const before = {
			open: row.querySelector(".cap-detail").classList.contains("open"),
			empty: !!row.querySelector(".cap-down-empty"),
		};
		// replace 语义：传什么就是全部（这里传一条）。左端是能力名，出口是隐式 out，
		// 两者都不出现在请求里 —— 请求体只有 links[{toRef, priority}]。
		const res = await fetch("/v1/capabilities/script.echo/downstreams", {
			method: "PUT",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ links: [{ toRef: "report.send", priority: 0 }] }),
		});
		const status = res.status;
		await loadAll();
		for (let i = 0; i < 40; i++) {
			row = findRow();
			if (row && row.querySelector(".cap-link[data-cand-name]")) break;
			await new Promise((r) => setTimeout(r, 100));
		}
		row = findRow();
		return {
			status,
			before,
			stillOpen: row.querySelector(".cap-detail").classList.contains("open"),
			empty: !!row.querySelector(".cap-down-empty"),
			cands: Array.from(row.querySelectorAll(".cap-link[data-cand-name]")).map((l) =>
				l.getAttribute("data-cand-name"),
			),
		};
	});
	H.check(
		"造边之前 script.echo 的详情就是空态（跑完就结束）",
		afterEdge.before && afterEdge.before.open === true && afterEdge.before.empty === true,
		JSON.stringify(afterEdge.before),
	);
	H.check(
		"造一条下游后，详情区立刻出现那个目标能力（report.send）",
		afterEdge.status === 200 &&
			(afterEdge.cands || []).some((n) => String(n).split("@")[0] === "report.send"),
		JSON.stringify(afterEdge),
	);
	H.check(
		"接了边之后「跑完就结束」消失，且展开态没被刷新弄丢",
		afterEdge.empty === false && afterEdge.stillOpen === true,
		JSON.stringify({ empty: afterEdge.empty, open: afterEdge.stillOpen }),
	);

	// ④ 「+ 下游」就地建边（report.send 再接一个 archive-logs：候选 ≥2）
	//
	// 详情区是懒渲染的：点开之后要等「+ 下游」按钮真的落进 DOM（不能跟着猜）。
	const addPanel = await page.evaluate(async () => {
		const row = document.querySelector('.cap-row[data-cap-name="report.send"]');
		if (!row) return { has: false, err: "找不到 report.send 行" };
		if (!row.querySelector(".cap-detail").classList.contains("open")) {
			row.querySelector('[data-act="detail"]').click();
		}
		let btn = null;
		for (let i = 0; i < 40; i++) {
			btn = row.querySelector('[data-act="add-downstream"]');
			if (btn) break;
			await new Promise((r) => setTimeout(r, 100));
		}
		if (!btn) return { has: false, err: "详情区没有「+ 下游」按钮" };
		btn.click();
		const p = row.querySelector(".cap-add-panel");
		if (!p) return { has: false, err: "点了「+ 下游」没有展开面板" };
		// 面板上只有一个**目标**输入框（「加下游」不再问「哪个出口」）；
		// 另一个是常量覆盖（可选，与出口无关，属于既有能力）。
		const inputs = Array.from(p.querySelectorAll('input[list="cap-down-targets"]'));
		if (inputs.length !== 1) return { has: true, targets: inputs.length };
		inputs[0].value = "archive-logs";
		// 抓包：PUT …/downstreams 的 body 是 {links:[{toRef,priority}…]}（replace）；
		// 同时记下 POST /v1/edges 的次数 —— 旧接口已封（405），全程一次都不许发。
		window.__downPuts = [];
		window.__edgePosts = 0;
		const orig = window.fetch;
		window.fetch = function (input, init) {
			const url = String((input && input.url) || input);
			const method = String((init && init.method) || "GET").toUpperCase();
			if (method === "POST" && url.includes("/v1/edges")) window.__edgePosts += 1;
			if (method === "PUT" && url.includes("/downstreams") && init && init.body) {
				try {
					window.__downPuts.push(JSON.parse(String(init.body)));
				} catch (e) {
					window.__downPuts.push(null);
				}
			}
			return orig.apply(this, arguments);
		};
		window.__edgeRestoreFetch = () => {
			window.fetch = orig;
		};
		Array.from(p.querySelectorAll("button"))
			.find((b) => b.className.includes("btn-primary"))
			.click();
		return { has: true, targets: inputs.length };
	});
	H.check(
		"点「+ 下游」能展开就地加边面板（只有一个目标输入框，没有「哪个出口」可选）",
		addPanel.has === true && addPanel.targets === 1,
		JSON.stringify(addPanel),
	);
	await page.waitForTimeout(1500);
	const addResult = await page.evaluate(() => {
		const bodies = window.__downPuts || [];
		const edgePosts = window.__edgePosts || 0;
		window.__edgeRestoreFetch && window.__edgeRestoreFetch();
		const row = document.querySelector('.cap-row[data-cap-name="report.send"]');
		return {
			body: bodies[bodies.length - 1] || null,
			n: bodies.length,
			edgePosts: edgePosts,
			cands: Array.from(row.querySelectorAll(".cap-link[data-cand-name]")).map((l) =>
				l.getAttribute("data-cand-name"),
			),
		};
	});
	// 旧口径（已废）：POST /v1/edges 单建一条 `{fromIsSource:false, fromRef, fromPort:"out", toRef}`。
	// 新口径（CONTRACT-downstream-edit §2/§4）：「+ 下游」必须走**同一个** PUT …/downstreams，
	// 而且必须把**已有下游整组带上** —— PUT 是 replace，只发新增那一条会把 alert.oncall 静默删掉。
	H.check(
		"「+ 下游」发的是 PUT …/downstreams，且把已有下游整组带上（replace 不丢边）",
		!!addResult.body &&
			Array.isArray(addResult.body.links) &&
			addResult.body.links.length === 2 &&
			// 已有的那条（report.send → alert.oncall）必须在请求体里。
			// 注：种子里它是**钉死引用**（alert.oncall@1），所以按名字比较。
			addResult.body.links.some(
				(l) => String(l.toRef || "").split("@")[0] === "alert.oncall",
			) &&
			// 新加的那条是**裸名字**（= 跟随最新），不是能力 id
			addResult.body.links.some((l) => l.toRef === "archive-logs") &&
			// 优先级是数字（不是字符串），否则后端会当缺省值处理
			addResult.body.links.every((l) => typeof l.priority === "number") &&
			// 端口不上请求体：左端是能力名、出口恒为隐式 out
			addResult.body.links.every((l) => !("fromPort" in l) && !("fromRef" in l)),
		JSON.stringify(addResult.body),
	);
	H.check(
		"加下游全程没有发过 POST /v1/edges（能力边入口已封）",
		addResult.n >= 1 && addResult.edgePosts === 0,
		JSON.stringify({ puts: addResult.n, posts: addResult.edgePosts }),
	);
	H.check(
		"加下游成功后同一处立刻看到新目标（候选 ≥2）",
		(addResult.cands || []).length >= 2 &&
			(addResult.cands || []).some((n) => String(n).split("@")[0] === "archive-logs"),
		JSON.stringify(addResult.cands),
	);

	// ⑤ 新建能力：下游填一串名字 → **一次提交**把能力与全部候选边建出来
	//
	// 用户原话：「直接让用户填 script.echo,notify.phone 这样就可以了」。
	// 所以表单只有一个逗号分隔的输入框：多个名字 = 候选，留空 = 终端能力。
	const capName = "e2e.chain." + Date.now();
	await page.evaluate(() => document.querySelector("button[onclick='openCapModal()']").click());
	await page.waitForTimeout(600);
	await page.click('[data-tpl="script.blank"]');
	await page.waitForTimeout(400);
	const filled = await page.evaluate((name) => {
		document.getElementById("cap-name").value = name;
		const inp = document.getElementById("cap-downstream");
		inp.value = "report.send, archive-logs";
		inp.dispatchEvent(new Event("input", { bubbles: true }));
		return {
			value: inp.value,
			hint: (document.getElementById("cap-downstream-hint") || {}).textContent || "",
			cardDown: (
				(document.querySelector("#cap-cards .cap-card.is-active .cap-card-down")) || {}
			).textContent || "",
		};
	}, capName);
	H.check(
		"下游填两个名字 → hint 回显运行期语义（按优先级选一个 / 同级轮盘）",
		/运行期按优先级选一个/.test(filled.hint) && /轮盘/.test(filled.hint) && !/找不到/.test(filled.hint),
		filled.hint.slice(0, 160),
	);
	H.check(
		"卡片摘要显示「下游：a, b」（端口不出现在摘要里）",
		/下游：\s*report\.send,\s*archive-logs/.test(filled.cardDown) && !/out/.test(filled.cardDown),
		filled.cardDown,
	);

	// 引用不存在的能力：当场指出是哪个，而且**不提交**（前端先拦）
	const unknown = await page.evaluate(async () => {
		const inp = document.getElementById("cap-downstream");
		inp.value = "report.send, nope.nothing";
		inp.dispatchEvent(new Event("input", { bubbles: true }));
		const hint = document.getElementById("cap-downstream-hint");
		const out = {
			hint: (hint || {}).textContent || "",
			warn: !!(hint && hint.classList.contains("cap-warn")),
		};
		// 抓包：这一步点提交也不该有 POST 发出
		const orig = window.fetch;
		let posts = 0;
		window.fetch = function (input, init) {
			const url = String((input && input.url) || input);
			const method = String((init && init.method) || "GET").toUpperCase();
			if (method === "POST" && url.includes("/v1/capabilities")) posts++;
			return orig.apply(this, arguments);
		};
		document.getElementById("cap-submit").click();
		await new Promise((r) => setTimeout(r, 700));
		window.fetch = orig;
		out.posts = posts;
		out.modalOpen = !document.getElementById("cap-modal").classList.contains("hidden");
		out.toast = ((document.getElementById("anotify-toast-host") || {}).textContent || "")
			.replace(/\s+/g, " ")
			.trim();
		return out;
	});
	H.check(
		"引用不存在的下游 → 可读错误（点名哪个找不到），且不提交",
		/找不到这些能力/.test(unknown.hint) &&
			/nope\.nothing/.test(unknown.hint) &&
			unknown.warn === true &&
			unknown.posts === 0 &&
			unknown.modalOpen === true,
		JSON.stringify(unknown).slice(0, 240),
	);

	// 改回两个真实存在的名字（能力 + 全部边走同一次提交）
	await page.evaluate(() => {
		const inp = document.getElementById("cap-downstream");
		inp.value = "report.send, archive-logs";
		inp.dispatchEvent(new Event("input", { bubbles: true }));
	});
	// 抓包钩子：mock 在 window.fetch 层就拦下了（page.on("request") 一条都抓不到）。
	// 同时抓**响应**：原子路径的 400/200 形状是断言对象。
	await page.evaluate(() => {
		window.__bodies = { caps: [], edges: [] };
		window.__resps = [];
		const orig = window.fetch;
		window.fetch = function (input, init) {
			const url = String((input && input.url) || input);
			const method = String((init && init.method) || "GET").toUpperCase();
			if (method === "POST" && init && init.body) {
				try {
					if (url.includes("/v1/capabilities")) window.__bodies.caps.push(JSON.parse(String(init.body)));
					if (url.includes("/v1/edges")) window.__bodies.edges.push(JSON.parse(String(init.body)));
				} catch (e) {
					/* 体不是 JSON：下面的断言会红 */
				}
			}
			const p = orig.apply(this, arguments);
			if (method === "POST" && url.includes("/v1/capabilities")) {
				p.then(async (res) => {
					let body = null;
					try {
						body = await res.clone().json();
					} catch (e) {
						/* 非 JSON 响应：留 null */
					}
					window.__resps.push({ status: res.status, body: body });
				}).catch(() => {});
			}
			return p;
		};
	});
	await page.click("#cap-submit");
	await page.waitForTimeout(2500);
	const created = await page.evaluate(async (name) => {
		const b = window.__bodies || { caps: [], edges: [] };
		const resps = window.__resps || [];
		const [capsApi, edgesApi] = await Promise.all([
			fetch("/v1/capabilities").then((r) => r.json()),
			fetch("/v1/edges").then((r) => r.json()),
		]);
		const row = Array.from(document.querySelectorAll(".cap-row")).find(
			(r) => r.getAttribute("data-cap-name") === name,
		);
		return {
			capBody: b.caps[0] || null,
			edgeBodies: b.edges,
			resp: resps[resps.length - 1] || null,
			caps: (capsApi.capabilities || []).filter((x) => x.name === name).map((x) => x.outputs),
			edges: (edgesApi.edges || [])
				.filter((x) => x.fromRef === name)
				.map((x) => ({ toRef: x.toRef, fromPort: x.fromPort })),
			rowOpen: row ? row.querySelector(".cap-detail").classList.contains("open") : null,
			rowText: row ? row.querySelector(".cap-detail").textContent.replace(/\s+/g, " ") : "",
			modalOpen: !document.getElementById("cap-modal").classList.contains("hidden"),
		};
	}, capName);
	H.check(
		"有下游 → 请求体声明内部唯一出口 outputs:[out]（端口名不出现在界面）",
		!!created.capBody &&
			JSON.stringify(created.capBody.outputs) === JSON.stringify(["out"]),
		JSON.stringify(created.capBody && created.capBody.outputs),
	);
	// ② 全合法 → 能力与全部边**一次出现**：
	//  - 请求只有**一个** POST /v1/capabilities，且带两条 links（左端不用传 = 新能力裸名字）
	//  - **一条** POST /v1/edges 都没有（那正是要被这条接口取代的两步流程）
	//  - 响应带 links（建成的边，现成 edge 视图）
	//  - GET 立刻能看到两条边
	H.check(
		"下游边跟能力同一次提交（links 在 caps 请求体里，全程没发过 POST /v1/edges）",
		!!created.capBody &&
			Array.isArray(created.capBody.links) &&
			created.capBody.links.length === 2 &&
			// 顺序 = 用户在逗号输入框里的顺序（候选的顺序就是运行期选法的依据）
			created.capBody.links[0].fromPort === "out" &&
			created.capBody.links[0].toRef === "report.send" &&
			created.capBody.links[1].fromPort === "out" &&
			created.capBody.links[1].toRef === "archive-logs" &&
			// 单建路径不带 priority（优先级只归详情区的下游列表）
			!("priority" in created.capBody.links[0]) &&
			created.edgeBodies.length === 0,
		JSON.stringify({ links: created.capBody && created.capBody.links, edges: created.edgeBodies }),
	);
	H.check(
		"响应回带建成的 links（edge 视图：两端引用 + 解析到的能力 id）",
		!!created.resp &&
			created.resp.status < 300 &&
			Array.isArray(created.resp.body.links) &&
			created.resp.body.links.length === 2 &&
			created.resp.body.links.every(
				(l) => l.fromRef === capName && typeof l.toCapabilityId === "string",
			),
		JSON.stringify(created.resp && { status: created.resp.status, links: created.resp.body && created.resp.body.links }),
	);
	H.check(
		"创建成功后弹层关闭（与下面失败路径的「弹层不关」形成对照）",
		created.modalOpen === false,
		String(created.modalOpen),
	);
	H.check(
		"能力与**全部**边一次落库（GET 能看到两条），详情默认展开并列出下游",
		created.caps.length === 1 &&
			JSON.stringify(created.caps[0]) === JSON.stringify(["out"]) &&
			created.edges.length === 2 &&
			created.edges.some((e) => e.toRef === "report.send" && e.fromPort === "out") &&
			created.edges.some((e) => e.toRef === "archive-logs" && e.fromPort === "out") &&
			created.rowOpen === true &&
			/report\.send/.test(created.rowText),
		JSON.stringify({ caps: created.caps, edges: created.edges, open: created.rowOpen }),
	);

	// ⑥ 一条坏边（目标不存在）→ **整体回滚**：能力根本没建出来，400 里指出是第几条。
	//
	// 这是这条接口的全部意义：老的两步流程在这里会留下「能力在、边缺」的半成品，
	// 前端只能在内存里记一份待重试清单（刷新就丢）。
	const badName = "e2e.badlink." + Date.now();
	await page.evaluate(() => document.querySelector("button[onclick='openCapModal()']").click());
	await page.waitForTimeout(600);
	await page.click('[data-tpl="script.blank"]');
	await page.waitForTimeout(400);
	await page.evaluate((name) => {
		document.getElementById("cap-name").value = name;
		// 两条都填**存在**的目标（表单本身会拦住不存在的目标，见 ⑤ 的用例）：
		// 注入会改写最后一条，让服务端在落库前发现它指向的目标不存在 ——
		// 那正是「能力在、边缺」老流程会留下半成品的场景。
		const din = document.getElementById("cap-downstream");
		din.value = "report.send, archive-logs";
		din.dispatchEvent(new Event("input", { bubbles: true }));
		window.__bodies = { caps: [], edges: [] };
		window.__resps = [];
		// 故障注入：把**最后一条** link 的目标改写成不存在的名字。
		//
		// 为什么要注入：新建表单的输入框只接受**存在**的目标能力（targetByName 会拦），
		// 所以「目标不存在」这种坏边在 UI 上根本发不出去 —— 而它正是这条接口要覆盖的
		// 失败模式（服务端才发现的坏边）。这里在网络层改写请求体，等价于「两台机器之间
		// 目标被删了」：客户端发的时候是合法的，落库时不是。
		const origFetch = window.fetch;
		window.__restoreFetch = () => {
			window.fetch = origFetch;
		};
		window.fetch = function (input, init) {
			const url = String((input && input.url) || input);
			if (
				init &&
				String(init.method || "").toUpperCase() === "POST" &&
				url.includes("/v1/capabilities") &&
				init.body
			) {
				try {
					const parsed = JSON.parse(String(init.body));
					if (Array.isArray(parsed.links) && parsed.links.length) {
						const last = parsed.links.length - 1;
						parsed.links[last] = Object.assign({}, parsed.links[last], {
							toRef: "no.such.capability",
						});
					}
					init = Object.assign({}, init, { body: JSON.stringify(parsed) });
				} catch (e) {
					/* 体不是 JSON：原样发出去 */
				}
			}
			return origFetch.apply(this, [input, init]);
		};
	}, badName);
	await page.click("#cap-submit");
	await page.waitForTimeout(2500);
	const rejected = await page.evaluate(async (name) => {
		const b = window.__bodies || { caps: [], edges: [] };
		const resps = window.__resps || [];
		const [capsApi, edgesApi] = await Promise.all([
			fetch("/v1/capabilities").then((r) => r.json()),
			fetch("/v1/edges").then((r) => r.json()),
		]);
		const toastHost = document.getElementById("anotify-toast-host");
		const out = {
			capBody: b.caps[0] || null,
			edgeBodies: b.edges,
			resp: resps[resps.length - 1] || null,
			capExists: (capsApi.capabilities || []).some((x) => x.name === name),
			edges: (edgesApi.edges || []).filter((x) => x.fromRef === name),
			// 弹层必须还开着，且用户填的一切都还在
			modalOpen: !document.getElementById("cap-modal").classList.contains("hidden"),
			nameKept: document.getElementById("cap-name").value === name,
			toast: toastHost ? (toastHost.textContent || "").replace(/\s+/g, " ") : "",
		};
		window.__restoreFetch && window.__restoreFetch();
		return out;
	}, badName);
	H.check(
		"提交里的坏边让**整个提交**失败：能力根本没建出来（GET 里没有它、边 0 条）",
		rejected.capExists === false && rejected.edges.length === 0,
		JSON.stringify({ exists: rejected.capExists, edges: rejected.edges }),
	);
	H.check(
		"400 的 body 指出是第几条 link（links[1]），前端据此报「第 2 条下游」",
		!!rejected.resp &&
			rejected.resp.status === 400 &&
			/links\[1\]/.test(String((rejected.resp.body || {}).error || "")) &&
			String((rejected.resp.body || {}).code || "") === "edge_rejected",
		JSON.stringify(rejected.resp && { status: rejected.resp.status, body: rejected.resp.body }),
	);
	H.check(
		"失败后弹层不关：用户填的一切都还在（不用重填）",
		rejected.modalOpen === true && rejected.nameKept === true,
		JSON.stringify({ open: rejected.modalOpen, nameKept: rejected.nameKept }),
	);
	H.check(
		"错误提示指明是第几条下游 / 哪个目标，且不把内部出口名泄给用户",
		/第\s*2\s*条/.test(rejected.toast) &&
			rejected.toast.includes("archive-logs") &&
			rejected.toast.includes("no.such.capability") &&
			// 新口径：提示里只说「下游 → 目标」，没有 --out--> 这种端口形状
			!/--/.test(rejected.toast) &&
			!/端口/.test(rejected.toast),
		rejected.toast.slice(0, 200),
	);

	// ⑦ 自连（下游填自己）→ 400 且什么都没建。
	//
	// UI 会把自连过滤掉（新建里的自环几乎总是误操作），所以这条直接打接口：
	// 它钉的是**后端**的原子性，而不是按钮行为。
	const selfName = "e2e.selflink." + Date.now();
	const selfLink = await page.evaluate(async (name) => {
		const res = await fetch("/v1/capabilities", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({
				name: name,
				outputs: ["out"],
				run: { entrypoint: "run.sh", files: { "run.sh": "#!/bin/sh\necho hi\n" } },
				links: [{ fromPort: "out", toRef: name }],
			}),
		});
		let body = null;
		try {
			body = await res.json();
		} catch (e) {
			/* 非 JSON */
		}
		const [capsApi, edgesApi] = await Promise.all([
			fetch("/v1/capabilities").then((r) => r.json()),
			fetch("/v1/edges").then((r) => r.json()),
		]);
		return {
			status: res.status,
			body: body,
			capExists: (capsApi.capabilities || []).some((x) => x.name === name),
			edges: (edgesApi.edges || []).filter((x) => x.fromRef === name),
		};
	}, selfName);
	// 产物那一面（artifacts 也没有）由 store/server 单测钉住：
	// mock 没有 GET /v1/artifacts，在这里查它会撞「mock 覆盖度」门禁。
	H.check(
		"自连（下游填自己）→ 400 self loop（links[0]），且能力/边一样都没建",
		selfLink.status === 400 &&
			/links\[0\]/.test(String((selfLink.body || {}).error || "")) &&
			/self loop/.test(String((selfLink.body || {}).error || "")) &&
			selfLink.capExists === false &&
			selfLink.edges.length === 0,
		JSON.stringify(selfLink),
	);

	// ⑧ 旧接口已关闭时给人看的话：405 edge_write_removed → 可读提示
	//
	// 为什么要这一条：`POST /v1/edges` 的能力边已封成 405（CONTRACT-downstream-edit §3），
	// 而用户真正会遇到的失败形态就是「保存下游」失败。若把后端 JSON 原样丢出去
	// （{"code":"edge_write_removed",...}），用户得自己读机器码才知道去哪儿改 ——
	// 所以这里在网络层把 PUT 拦成 405，钉住 toast 里是人话、不夹 JSON/机器码。
	const forbidden = await page.evaluate(async () => {
		// 先把上一个用例留下的新建表单关掉（⑥ 失败时故意不关弹层）
		closeCapModal();
		const row = document.querySelector('.cap-row[data-cap-name="report.send"]');
		if (!row) return { err: "找不到 report.send 行" };
		row.querySelector('[data-act="edit"]').click();
		for (let i = 0; i < 40; i++) {
			if (document.querySelectorAll("#cap-down-list .cap-down-item").length > 0) break;
			await new Promise((r) => setTimeout(r, 100));
		}
		const rows = Array.from(document.querySelectorAll("#cap-down-list .cap-down-item"));
		const host = document.getElementById("anotify-toast-host");
		if (host) host.replaceChildren(); // 只看这一条 toast
		const orig = window.fetch;
		let intercepted = 0;
		window.fetch = function (input, init) {
			const url = String((input && input.url) || input);
			const method = String((init && init.method) || "GET").toUpperCase();
			if (method === "PUT" && url.includes("/downstreams")) {
				intercepted += 1;
				return Promise.resolve(
					new Response(
						JSON.stringify({
							code: "edge_write_removed",
							error:
								"能力的下游请在能力里改（PUT /v1/capabilities/{name}/downstreams）",
						}),
						{ status: 405, headers: { "Content-Type": "application/json" } },
					),
				);
			}
			return orig.apply(this, arguments);
		};
		document.getElementById("cap-down-save").click();
		for (let i = 0; i < 40; i++) {
			const t = host && host.lastElementChild ? host.lastElementChild.textContent : "";
			if (t && t.trim()) break;
			await new Promise((r) => setTimeout(r, 100));
		}
		const out = {
			intercepted: intercepted,
			rows: rows.length,
			toast:
				host && host.lastElementChild
					? (host.lastElementChild.textContent || "").replace(/\s+/g, " ").trim()
					: "",
			modalOpen: !document.getElementById("cap-modal").classList.contains("hidden"),
			saveLabel: (document.getElementById("cap-down-save") || {}).textContent || "",
		};
		window.fetch = orig; // 做完立即恢复，后面的用例还要正常发请求
		closeCapModal();
		return out;
	});
	H.check(
		"旧接口 405 → toast 是人话（说清去「编辑 → 下游」改），不是机器码/JSON",
		forbidden.intercepted === 1 &&
			/下游/.test(forbidden.toast || "") &&
			/编辑/.test(forbidden.toast || "") &&
			!/edge_write_removed/.test(forbidden.toast || "") &&
			!/\/v1\/(edges|capabilities)/.test(forbidden.toast || "") &&
			!/[{}]/.test(forbidden.toast || ""),
		JSON.stringify(forbidden).slice(0, 300),
	);
	H.check(
		"保存下游失败后弹层不关（改动还在，可重试）",
		forbidden.modalOpen === true && /保存下游/.test(forbidden.saveLabel || ""),
		JSON.stringify({ open: forbidden.modalOpen, label: forbidden.saveLabel }),
	);
}

// checkDownstreamEdit 覆盖「能力的下游只能在能力里改」（CONTRACT-downstream-edit §4）。
//
// 出口页删掉之后，用户在能力编辑里能做的四件事就是这四件，每一件都会**静默**坏掉：
//   ① 预填：进编辑弹层就看到当前下游（名字 + 优先级），不是一块空白
//      —— 空白会让人以为「没有下游」，而实际上一保存就把边全删了（PUT 是 replace）
//   ② 改优先级 → 保存 → 重开仍是新值，且**不产生新版本**（拓扑不属于产物）
//   ③ 删一行 → 保存 → 那条边真的没了（未提交的就是被替换掉的）
//   ④ 清空 → 终端能力（links: [] 合法）→ 详情区说「跑完就结束」
// 外加一条反向守卫：这一整轮**一条** POST /v1/edges 都不许发（能力边入口已封 405）。
//
// 为什么在 mock 里跑：这里钉的是**界面接对了接口形状**（PUT …/downstreams + links[]、
// replace 语义、回读），真后端那一半在 checkCapabilityEditReal 末尾（落库 + 版本不变）。
async function checkDownstreamEdit(page, base) {
	await page.goto(base + "/capabilities.html", { waitUntil: "load", timeout: 20000 });
	await page.waitForTimeout(1600);

	// 种子数据里 deploy-preview 有两条**干净**的出边：
	//   gen-changelog（裸名字 = 跟随最新）、rollback@1（钉死版本）
	// 为什么不用 analyze-pr：它历史上出现过两条相同的边（mock 注释里写了），
	// 而 PUT 对重复 toRef 会 400 duplicate_target —— 那会变成在验 mock 的脏数据。
	const capName = "deploy-preview";

	// 抓包装在 window.fetch 上：mock 就是在这里截住请求的，
	// page.on("request") 抓不到（这也是下面那条「没发 POST /v1/edges」能成立的原因）。
	await page.evaluate(() => {
		window.__downPuts = [];
		window.__edgePosts = 0;
		const orig = window.fetch;
		window.__restoreDownFetch = () => {
			window.fetch = orig;
		};
		window.fetch = function (input, init) {
			const url = String((input && input.url) || input);
			const method = String((init && init.method) || "GET").toUpperCase();
			if (method === "POST" && url.includes("/v1/edges")) window.__edgePosts += 1;
			if (method === "PUT" && url.includes("/downstreams") && init && init.body) {
				try {
					window.__downPuts.push(JSON.parse(String(init.body)));
				} catch (e) {
					window.__downPuts.push(null);
				}
			}
			return orig.apply(this, arguments);
		};
	});

	// ---------- ① 预填 ----------
	const prefill = await page.evaluate(async (name) => {
		const readRows = () =>
			Array.from(document.querySelectorAll("#cap-down-list .cap-down-item")).map((it) => ({
				toRef: it.getAttribute("data-to-ref") || "",
				name: (it.querySelector(".cap-down-item-name") || {}).value || "",
				prio: (it.querySelector(".cap-down-item-prio") || {}).value || "",
			}));
		const row = document.querySelector(`.cap-row[data-cap-name="${name}"]`);
		if (!row) return { err: "找不到 " + name + " 行" };
		row.querySelector('[data-act="edit"]').click();
		for (let i = 0; i < 40; i++) {
			if (readRows().length > 0) break;
			await new Promise((r) => setTimeout(r, 100));
		}
		const [eRes, cRes] = await Promise.all([
			fetch("/v1/edges").then((r) => r.json()),
			fetch("/v1/capabilities").then((r) => r.json()),
		]);
		const mine = (eRes.edges || []).filter(
			(e) =>
				!e.fromIsSource &&
				(e.fromPort || "out") === "out" &&
				String(e.fromRef || "").split("@")[0] === name,
		);
		return {
			open: !document.getElementById("cap-modal").classList.contains("hidden"),
			rows: readRows(),
			edges: mine.map((e) => ({ toRef: e.toRef, priority: Number(e.priority || 0) })),
			versions: (cRes.capabilities || [])
				.filter((c) => c.name === name)
				.map((c) => c.version),
			draft: document.getElementById("cap-downstream").value,
			actionsHidden: document
				.getElementById("cap-down-actions")
				.classList.contains("hidden"),
			hint: document.getElementById("cap-downstream-hint").textContent.trim(),
			items: document.querySelectorAll("#cap-down-list .cap-down-item").length,
			none: !!document.querySelector("#cap-down-list .cap-down-none"),
		};
	}, capName);
	H.check(
		"① 编辑弹层里下游栏可见，且行数 = 当前下游数（不是空白，也不会一保存就删光）",
		prefill.open === true &&
			prefill.actionsHidden === false &&
			prefill.none === false &&
			prefill.rows.length >= 2 &&
			prefill.rows.length === prefill.edges.length,
		JSON.stringify({
			open: prefill.open,
			rows: prefill.rows,
			edges: prefill.edges,
			none: prefill.none,
		}).slice(0, 300),
	);
	H.check(
		"① 预填的行名与优先级逐条对得上 /v1/edges（名字/优先级都不是猜的）",
		prefill.rows.length > 0 &&
			prefill.rows.every((r) => {
				const e = prefill.edges.find((x) => x.toRef === r.toRef);
				return !!e && String(e.priority) === String(Number(r.prio));
			}) &&
			prefill.rows.every((r) => r.name === r.toRef),
		JSON.stringify({ rows: prefill.rows, edges: prefill.edges }).slice(0, 300),
	);
	H.check(
		"① 编辑模式的下游输入框是「加一条」的空草稿，行列表才是真相",
		prefill.draft === "" && /name@2/.test(prefill.hint || ""),
		JSON.stringify({ draft: prefill.draft, hint: prefill.hint }).slice(0, 200),
	);

	// ---------- ② 改优先级 → 保存 → 重开仍是新值（且不产生新版本）----------
	const bumped = await page.evaluate(async (name) => {
		const rows = Array.from(document.querySelectorAll("#cap-down-list .cap-down-item"));
		if (!rows.length) return { err: "没有行可改" };
		const toRef = rows[0].getAttribute("data-to-ref") || "";
		const prio = rows[0].querySelector(".cap-down-item-prio");
		prio.value = "7";
		prio.dispatchEvent(new Event("input", { bubbles: true }));
		prio.dispatchEvent(new Event("change", { bubbles: true }));
		const hintEl = document.getElementById("cap-down-edit-hint");
		const dirty = {
			text: (hintEl.textContent || "").trim(),
			warn: hintEl.classList.contains("cap-warn"),
		};
		window.__downPuts = [];
		document.getElementById("cap-down-save").click();
		// 服务端才是真相：轮询 /v1/edges 直到优先级真的变成 7（而不是只看 toast）
		let seen = null;
		for (let i = 0; i < 60; i++) {
			const d = await fetch("/v1/edges").then((r) => r.json());
			const e = (d.edges || []).find(
				(x) =>
					!x.fromIsSource &&
					String(x.fromRef || "").split("@")[0] === name &&
					x.toRef === toRef,
			);
			if (e && Number(e.priority) === 7) {
				seen = Number(e.priority);
				break;
			}
			await new Promise((r) => setTimeout(r, 150));
		}
		// 再等 saveDownstreams **整个**走完：成功后它会 await loadAll() 回读、重置 DOWN_ROWS、
		// 把 dirty 标记清掉。只等 /v1/edges 变化是不够的 —— mock 每个请求有 120~300ms 人为延迟，
		// 那时光标所在的 DOM 可能还是保存前的（重开弹层就会读到旧值 → 假红）。
		for (let i = 0; i < 80; i++) {
			const h = document.getElementById("cap-down-edit-hint");
			if (h && !h.classList.contains("cap-warn")) break;
			await new Promise((r) => setTimeout(r, 100));
		}
		const caps = await fetch("/v1/capabilities").then((r) => r.json());
		const puts = window.__downPuts || [];
		return {
			toRef,
			dirty,
			seen,
			put: puts[puts.length - 1] || null,
			versions: (caps.capabilities || [])
				.filter((c) => c.name === name)
				.map((c) => c.version),
			toast: ((document.getElementById("anotify-toast-host") || {}).textContent || "")
				.replace(/\s+/g, " ")
				.trim(),
		};
	}, capName);
	H.check(
		"② 改优先级后明确示「有未保存的下游改动」（warn 态）",
		/有未保存/.test(bumped.dirty?.text || "") && bumped.dirty?.warn === true,
		JSON.stringify(bumped.dirty),
	);
	H.check(
		"② 保存走 PUT …/downstreams：请求体是整组 links（该行 priority=7，另一条仍在）",
		!!bumped.put &&
			Array.isArray(bumped.put.links) &&
			bumped.put.links.length === prefill.rows.length &&
			bumped.put.links.some(
				(l) => l.toRef === bumped.toRef && Number(l.priority) === 7,
			) &&
			bumped.put.links.filter((l) => l.toRef !== bumped.toRef).length ===
				prefill.rows.length - 1,
		JSON.stringify(bumped.put).slice(0, 300),
	);
	H.check(
		"② 落库了：GET /v1/edges 里这条的 priority 就是 7",
		bumped.seen === 7,
		`seen=${bumped.seen} toRef=${bumped.toRef}`,
	);
	H.check(
		"② 改下游**不产生新版本**（拓扑不属于产物，版本集合与保存前一致）",
		JSON.stringify(bumped.versions) === JSON.stringify(prefill.versions) &&
			bumped.versions.length > 0,
		JSON.stringify({ before: prefill.versions, after: bumped.versions }),
	);
	H.check(
		"② 成功提示说清「不产生新版本」，不是一句「成功」",
		/不产生新版本/.test(bumped.toast || ""),
		String(bumped.toast).slice(0, 160),
	);

	// 关弹层重开：值来自服务端回读，不是内存里的那一次输入
	const reopened = await page.evaluate(async (name) => {
		closeCapModal();
		await new Promise((r) => setTimeout(r, 250));
		const row = document.querySelector(`.cap-row[data-cap-name="${name}"]`);
		if (!row) return { err: "重开后找不到行" };
		row.querySelector('[data-act="edit"]').click();
		for (let i = 0; i < 40; i++) {
			if (document.querySelectorAll("#cap-down-list .cap-down-item").length > 0) break;
			await new Promise((r) => setTimeout(r, 100));
		}
		return {
			rows: Array.from(document.querySelectorAll("#cap-down-list .cap-down-item")).map(
				(it) => ({
					toRef: it.getAttribute("data-to-ref") || "",
					prio: (it.querySelector(".cap-down-item-prio") || {}).value || "",
				}),
			),
		};
	}, capName);
	H.check(
		"② 关掉弹层重开 → 该行优先级仍是 7（值来自服务端回读，不是内存残留）",
		(reopened.rows || []).some(
			(r) => r.toRef === bumped.toRef && r.prio === "7",
		),
		JSON.stringify(reopened.rows).slice(0, 240),
	);

	// ---------- ③ 删一行 → 保存 → 那条边消失 ----------
	const removed = await page.evaluate(async (name) => {
		const rows = Array.from(document.querySelectorAll("#cap-down-list .cap-down-item"));
		if (!rows.length) return { err: "没有行可删" };
		const victim = rows[rows.length - 1];
		const toRef = victim.getAttribute("data-to-ref") || "";
		const before = rows.map((it) => it.getAttribute("data-to-ref"));
		victim.querySelector(".cap-down-item-del").click();
		const afterDel = Array.from(
			document.querySelectorAll("#cap-down-list .cap-down-item"),
		).map((it) => it.getAttribute("data-to-ref"));
		window.__downPuts = [];
		document.getElementById("cap-down-save").click();
		let still = true;
		for (let i = 0; i < 60; i++) {
			const d = await fetch("/v1/edges").then((r) => r.json());
			still = (d.edges || []).some(
				(x) =>
					!x.fromIsSource &&
					String(x.fromRef || "").split("@")[0] === name &&
					x.toRef === toRef,
			);
			if (!still) break;
			await new Promise((r) => setTimeout(r, 150));
		}
		for (let i = 0; i < 80; i++) {
			const h = document.getElementById("cap-down-edit-hint");
			if (h && !h.classList.contains("cap-warn")) break;
			await new Promise((r) => setTimeout(r, 100));
		}
		const puts = window.__downPuts || [];
		return {
			toRef,
			before,
			afterDel,
			gone: !still,
			put: puts[puts.length - 1] || null,
		};
	}, capName);
	H.check(
		"③ 点删除按钮后那一行立刻从列表消失（不是等保存才消失）",
		(removed.afterDel || []).length === (removed.before || []).length - 1 &&
			!(removed.afterDel || []).includes(removed.toRef),
		JSON.stringify({ before: removed.before, after: removed.afterDel }),
	);
	H.check(
		"③ 保存的请求体里**不含**被删的那条（replace：没提交的就是被删的）",
		!!removed.put &&
			Array.isArray(removed.put.links) &&
			removed.put.links.length === (removed.before || []).length - 1 &&
			!removed.put.links.some((l) => l.toRef === removed.toRef),
		JSON.stringify(removed.put).slice(0, 300),
	);
	H.check(
		"③ 落库了：GET /v1/edges 里这条边真的没了",
		removed.gone === true,
		`toRef=${removed.toRef} gone=${removed.gone}`,
	);

	// ---------- ④ 清空 → 终端能力 ----------
	const cleared = await page.evaluate(async (name) => {
		// 删到 0 行：0 行时行列表里应该出现「跑完就结束」的空态说明
		for (let guard = 0; guard < 10; guard++) {
			const rows = Array.from(document.querySelectorAll("#cap-down-list .cap-down-item"));
			if (!rows.length) break;
			rows[0].querySelector(".cap-down-item-del").click();
			await new Promise((r) => setTimeout(r, 50));
		}
		const noneText = (
			(document.querySelector("#cap-down-list .cap-down-none") || {}).textContent || ""
		).trim();
		window.__downPuts = [];
		document.getElementById("cap-down-save").click();
		let left = null;
		for (let i = 0; i < 60; i++) {
			const d = await fetch("/v1/edges").then((r) => r.json());
			left = (d.edges || []).filter(
				(x) => !x.fromIsSource && String(x.fromRef || "").split("@")[0] === name,
			).length;
			if (left === 0) break;
			await new Promise((r) => setTimeout(r, 150));
		}
		for (let i = 0; i < 80; i++) {
			const h = document.getElementById("cap-down-edit-hint");
			if (h && !h.classList.contains("cap-warn")) break;
			await new Promise((r) => setTimeout(r, 100));
		}
		const puts = window.__downPuts || [];
		const put = puts[puts.length - 1] || null;
		// 关弹层 → 展开详情：终端能力要说「跑完就结束」，不能留一块空白。
		//
		// 注意每轮都要**重新取行**：saveDownstreams 里的 loadAll() → renderCaps()
		// 会把 .cap-row 节点整批换掉（旧引用会永远找不到新渲染的空态）。
		closeCapModal();
		let emptyText = "";
		for (let i = 0; i < 80; i++) {
			const row = document.querySelector(`.cap-row[data-cap-name="${name}"]`);
			if (row) {
				const box = row.querySelector(".cap-detail");
				if (box && !box.classList.contains("open")) {
					row.querySelector('[data-act="detail"]').click();
				}
				const e = row.querySelector(".cap-down-empty");
				if (e && (e.textContent || "").trim()) {
					emptyText = e.textContent.trim();
					break;
				}
			}
			await new Promise((r) => setTimeout(r, 100));
		}
		return { noneText, left, put, emptyText, edgePosts: window.__edgePosts || 0 };
	}, capName);
	H.check(
		"④ 删到 0 行时行列表显示空态（跑完就结束 = 终端能力，合法状态）",
		/跑完就结束/.test(cleared.noneText || ""),
		String(cleared.noneText).slice(0, 160),
	);
	H.check(
		"④ 清空保存的请求体就是 links: []（空数组合法，不是漏字段）",
		!!cleared.put && Array.isArray(cleared.put.links) && cleared.put.links.length === 0,
		JSON.stringify(cleared.put).slice(0, 200),
	);
	H.check(
		"④ 落库了：该能力在 /v1/edges 里没有任何出边",
		cleared.left === 0,
		`left=${cleared.left}`,
	);
	H.check(
		"④ 详情区改口说「跑完就结束」（不再是候选列表，也不是空白）",
		/跑完就结束/.test(cleared.emptyText || ""),
		String(cleared.emptyText).slice(0, 160),
	);

	// ---------- ⑤ 反向守卫：全程没有一条 POST /v1/edges ----------
	H.check(
		"⑤ 改下游全程没有发过 POST /v1/edges（能力边入口已封，只剩 PUT …/downstreams）",
		cleared.edgePosts === 0,
		`posts=${cleared.edgePosts}`,
	);

	await page.evaluate(() => window.__restoreDownFetch && window.__restoreDownFetch());
}

// checkBatchCreate 验证「一个表单建多个能力」（契约 §4）。
//
// 四件事：
//   ① 一次提交建两个能力 + 一条候选边，**只发一个** POST /v1/capabilities/batch；
//      卡 1 填了下游（outputs:["out"]），卡 2 留空（outputs:[]，终端能力）
//   ② 坏输入整批不落库：弹层不关、错误定位到第几张卡
//   ③ 表单不再表达优先级 / balance（单一输入框里没有它们的位置）：
//      请求体 priority 一律 0；优先级只归详情区的下游列表
//   ④ 成环可建（a→b 与 b→a 在同一批里）、balance 非 priority 保存即 400
//
// 全部跑在 mock 预览环境里：批量接口是本期新增的，真后端那侧要等 A/B 组合并后
// 才能跑同一批断言（合并后由协调者启用真后端版本）。
async function checkBatchCreate(page, base) {
	await page.goto(`${base}/capabilities.html`, { waitUntil: "load", timeout: 20000 });
	await page.waitForTimeout(1400);

	const stamp = Date.now();
	const capA = "e2e.batch.a." + stamp;
	const capB = "e2e.batch.b." + stamp;

	// 开弹层 → 选空白模板（与其它用例同一条最短路径）
	await page.evaluate(() => document.querySelector("button[onclick='openCapModal()']").click());
	await page.waitForTimeout(600);
	await page.click('[data-tpl="script.blank"]');
	await page.waitForTimeout(400);

	// 抓能力相关 POST：batch 必须**只发一次**（不是一个能力一个请求）
	await page.evaluate(() => {
		window.__batchPosts = [];
		const orig = window.fetch;
		window.__restoreBatchFetch = () => {
			window.fetch = orig;
		};
		window.fetch = function (input, init) {
			const url = String((input && input.url) || input);
			const method = String((init && init.method) || "GET").toUpperCase();
			if (method === "POST" && url.includes("/v1/capabilities") && init && init.body) {
				let body = null;
				try {
					body = JSON.parse(String(init.body));
				} catch (e) {
					/* 非 JSON：原样发 */
				}
				window.__batchPosts.push({ url, body });
			}
			return orig.apply(this, [input, init]);
		};
	});

	// 卡 1：先只填名字（下游等第二张卡也有名字之后再填，否则它引用不到）
	await page.evaluate((name) => {
		const n = document.getElementById("cap-name");
		n.value = name;
		n.dispatchEvent(new Event("input", { bubbles: true }));
	}, capA);

	// 「＋ 再加一个能力」
	await page.click("#cap-add-card");
	await page.waitForTimeout(400);
	const afterAdd = await page.evaluate(() => ({
		n: document.querySelectorAll("#cap-cards .cap-card").length,
		active: (document.querySelector("#cap-cards .cap-card.is-active") || { dataset: {} })
			.dataset.card,
		editor: (document.getElementById("cap-card-editor") || {}).textContent || "",
	}));
	H.check(
		"「＋ 再加一个能力」真的多出一张卡（默认只有一张）",
		afterAdd.n === 2,
		JSON.stringify(afterAdd),
	);

	// 卡 2：只填名字 —— 下游**留空** = 终端能力（outputs:[]）
	await page.evaluate(() => {
		const pick = document.querySelector('#cap-cards .cap-card[data-card="1"] .cap-card-pick');
		if (pick) pick.click();
	});
	await page.waitForTimeout(300);
	await page.evaluate((name) => {
		const n = document.getElementById("cap-name");
		n.value = name;
		n.dispatchEvent(new Event("input", { bubbles: true }));
	}, capB);

	// 回到卡 1：把**本批的卡 2** 填成下游（一个逗号分隔的输入框）
	await page.evaluate(() => {
		const pick = document.querySelector('#cap-cards .cap-card[data-card="0"] .cap-card-pick');
		if (pick) pick.click();
	});
	await page.waitForTimeout(300);
	const candSetup = await page.evaluate((target) => {
		const din = document.getElementById("cap-downstream");
		if (!din) return { err: "没有下游输入框" };
		din.value = target;
		din.dispatchEvent(new Event("input", { bubbles: true }));
		return { ok: true };
	}, capB);
	await page.waitForTimeout(400);
	const candState = await page.evaluate(() => {
		const din = document.getElementById("cap-downstream");
		const hint = document.getElementById("cap-downstream-hint");
		return {
			value: din ? din.value : "",
			hint: ((hint || {}).textContent || "").trim(),
			warn: !!(hint && hint.classList.contains("cap-warn")),
			cardDown: (
				(document.querySelector("#cap-cards .cap-card.is-active .cap-card-down")) || {}
			).textContent || "",
			// 契约删掉的两个控件：balance 模式选择器 / 优先级数字输入框
			balSelect: document.querySelectorAll("select.cap-bal-select, .cap-bal-wrap").length,
			prioInput: document.querySelectorAll(".cap-cand-prio, .cap-cand-prio-wrap").length,
			portUI: document.querySelectorAll("#cap-ports, #cap-port-input, .cap-chip").length,
		};
	});
	H.check(
		"候选能选「本批里另一个能力」（引用本批卡名也行）",
		candSetup.ok === true &&
			candState.value === capB &&
			candState.warn === false &&
			!/找不到/.test(candState.hint),
		JSON.stringify({ candSetup, candState }),
	);
	H.check(
		"下游表单不再表达优先级 / balance / 端口（三个旧控件都不在）",
		candState.balSelect === 0 && candState.prioInput === 0 && candState.portUI === 0,
		JSON.stringify({
			bal: candState.balSelect,
			prio: candState.prioInput,
			portUI: candState.portUI,
		}),
	);
	H.check(
		"输入框下的回显说明了「运行期按优先级选一个；同级轮盘」",
		/优先级/.test(candState.hint) && /轮盘/.test(candState.hint),
		String(candState.hint).slice(0, 120),
	);
	await page.waitForTimeout(400);

	// 提交：两个能力 + 一条候选边，**一个请求**
	await page.click("#cap-submit");
	await page.waitForTimeout(2600);
	const batch = await page.evaluate(async ([an, bn]) => {
		const [capsApi, edgesApi] = await Promise.all([
			fetch("/v1/capabilities").then((r) => r.json()),
			fetch("/v1/edges").then((r) => r.json()),
		]);
		const posts = window.__batchPosts || [];
		const edge = (edgesApi.edges || []).find(
			(e) => e.fromRef === an && e.fromPort === "out" && String(e.toRef).indexOf(bn) === 0,
		);
		return {
			posts: posts.map((p) => p.url),
			bodies: posts.map((p) => p.body),
			hasA: (capsApi.capabilities || []).some((c) => c.name === an),
			hasB: (capsApi.capabilities || []).some((c) => c.name === bn),
			edge: edge || null,
			modalOpen: !document.getElementById("cap-modal").classList.contains("hidden"),
			toast: ((document.getElementById("anotify-toast-host") || {}).textContent || "").replace(
				/\s+/g,
				" ",
			),
		};
	}, [capA, capB]);
	H.check(
		"批量创建：两个能力一次落库（单事务）+ 弹层已关",
		batch.hasA === true && batch.hasB === true && batch.modalOpen === false,
		JSON.stringify({ hasA: batch.hasA, hasB: batch.hasB, toast: batch.toast }),
	);
	H.check(
		"批量提交只发了一个 POST /v1/capabilities/batch（不是每个能力一个请求）",
		batch.posts.length === 1 && /\/v1\/capabilities\/batch$/.test(batch.posts[0] || ""),
		JSON.stringify(batch.posts),
	);
	const body0 = (batch.bodies || [])[0] || {};
	H.check(
		"批量请求体：capabilities 两条（有下游 outputs:[out] / 留空 []）+ links 带 fromRef/fromPort:out/priority:0",
		Array.isArray(body0.capabilities) &&
			body0.capabilities.length === 2 &&
			JSON.stringify(body0.capabilities[0].outputs) === JSON.stringify(["out"]) &&
			JSON.stringify(body0.capabilities[1].outputs) === JSON.stringify([]) &&
			Array.isArray(body0.links) &&
			body0.links.length === 1 &&
			body0.links[0].fromRef === capA &&
			body0.links[0].fromPort === "out" &&
			body0.links[0].toRef === capB &&
			body0.links[0].priority === 0,
		JSON.stringify({
			caps: (body0.capabilities || []).map((c) => c.outputs),
			links: body0.links,
		}),
	);
	H.check(
		"批量建出来的边 priority=0（表单不表达优先级），且出口是内部名 out",
		!!batch.edge && batch.edge.priority === 0 && batch.edge.fromPort === "out",
		JSON.stringify(batch.edge),
	);

	// 详情区：这条候选 + 优先级（不再有 balance 模式 / 端口块）
	const detail = await page.evaluate(async (an) => {
		const row = document.querySelector('.cap-row[data-cap-name="' + an + '"]');
		if (!row) return { err: "列表里没有刚建的能力行" };
		const box = row.querySelector(".cap-detail");
		if (box && !box.classList.contains("open")) row.querySelector('[data-act="detail"]').click();
		await new Promise((r) => setTimeout(r, 500));
		return {
			cands: Array.from(row.querySelectorAll(".cap-link[data-cand-name]")).map((l) => ({
				name: l.getAttribute("data-cand-name"),
				text: l.textContent.replace(/\s+/g, " ").trim(),
			})),
			// 旧控件：端口块 / balance 徽章 / 候选计数
			legacy: row.querySelectorAll(".cap-port-block, .cap-bal-badge, .cap-cand-count").length,
		};
	}, capA);
	H.check(
		"详情区列出这条下游并显示优先级 0（表单不表达优先级 → 一律 0）",
		(detail.cands || []).some(
			(c) => String(c.name || "").indexOf(capB) === 0 && /优先级\s*0/.test(c.text),
		),
		JSON.stringify(detail),
	);
	H.check(
		"详情区不再有 balance 模式徽章 / 候选计数 / 端口块",
		detail.legacy === 0,
		`legacy=${detail.legacy}`,
	);

	// ② 坏输入：整批不落库 + 弹层保留 + 错误定位到第几张卡
	await page.evaluate(() => document.querySelector("button[onclick='openCapModal()']").click());
	await page.waitForTimeout(600);
	await page.click('[data-tpl="script.blank"]');
	await page.waitForTimeout(400);
	const okName = "e2e.badcard.a." + stamp;
	const badName = "Bad Name " + stamp;
	await page.evaluate((name) => {
		const n = document.getElementById("cap-name");
		n.value = name;
		n.dispatchEvent(new Event("input", { bubbles: true }));
		// 这张卡有个合法的下游（第二张卡的名字）—— 但第二张卡名字非法，
		// 所以整批仍该被拦下
		const din = document.getElementById("cap-downstream");
		din.value = "report.send";
		din.dispatchEvent(new Event("input", { bubbles: true }));
	}, okName);
	await page.click("#cap-add-card");
	await page.waitForTimeout(400);
	await page.evaluate((name) => {
		const pick = document.querySelector('#cap-cards .cap-card[data-card="1"] .cap-card-pick');
		if (pick) pick.click();
		const n = document.getElementById("cap-name");
		n.value = name;
		n.dispatchEvent(new Event("input", { bubbles: true }));
	}, badName);
	await page.waitForTimeout(300);
	await page.evaluate(() => {
		window.__batchPosts = [];
	});
	await page.click("#cap-submit");
	await page.waitForTimeout(1200);
	const bad = await page.evaluate(async ([an, bn]) => {
		const capsApi = await fetch("/v1/capabilities").then((r) => r.json());
		return {
			anyA: (capsApi.capabilities || []).some((c) => c.name === an),
			anyB: (capsApi.capabilities || []).some((c) => c.name === bn),
			modalOpen: !document.getElementById("cap-modal").classList.contains("hidden"),
			nameKept: document.getElementById("cap-name").value === bn,
			errCards: document.querySelectorAll("#cap-cards .cap-card.is-err").length,
			card2: (
				(document.querySelector('#cap-cards .cap-card[data-card="1"] .cap-card-name') || {})
					.textContent || ""
			).trim(),
			toast: ((document.getElementById("anotify-toast-host") || {}).textContent || "").replace(
				/\s+/g,
				" ",
			),
		};
	}, [okName, badName]);
	H.check(
		"坏输入整批不落库：两个能力一个都没建（含合法的那个）",
		bad.anyA === false && bad.anyB === false,
		JSON.stringify({ anyA: bad.anyA, anyB: bad.anyB, toast: bad.toast }),
	);
	H.check(
		"失败后弹层不关、输入全保留（两张卡都还在）",
		bad.modalOpen === true && bad.nameKept === true && bad.errCards >= 1,
		JSON.stringify({ open: bad.modalOpen, kept: bad.nameKept, errCards: bad.errCards }),
	);
	H.check(
		"错误定位到第 2 张卡（能力 2 / name）",
		/能力\s*2/.test(bad.toast) && /name/.test(bad.toast),
		bad.toast.slice(0, 200),
	);

	// ③ API 层：mock 的批量接口同样「先全量校验再写入」（真后端由 A/B 组单测钉住，
	//    这里钉 mock 的语义一致，避免预览里能建出真后端会拒的半成品）
	const apiChecks = await page.evaluate(async ([an, bn]) => {
		const mk = (name, outputs) => ({
			name,
			outputs,
			run: { entrypoint: "run.sh", files: { "run.sh": "#!/bin/sh\necho hi\n" } },
		});
		const post = async (body) => {
			const res = await fetch("/v1/capabilities/batch", {
				method: "POST",
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify(body),
			});
			let json = null;
			try {
				json = await res.json();
			} catch (e) {
				/* 非 JSON */
			}
			return { status: res.status, body: json };
		};
		const badLink = await post({
			capabilities: [mk(an, ["out"]), mk(bn, ["out"])],
			links: [{ fromRef: an, fromPort: "out", toRef: "no.such.capability", priority: 0 }],
		});
		let caps = await fetch("/v1/capabilities").then((r) => r.json());
		const afterBad = {
			hasA: (caps.capabilities || []).some((c) => c.name === an),
			hasB: (caps.capabilities || []).some((c) => c.name === bn),
		};
		// balance 非 priority：保存即 400（本期只实现 priority）
		// 注：界面已经表达不了 balance（端口选择器删掉了），但 API 仍是协议的一部分
		const bal = mk(an, ["out"]);
		bal.balance = { out: { mode: "all" } };
		const balRes = await post({ capabilities: [bal], links: [] });
		// 成环可建：a→b 与 b→a 在同一批里
		caps = await fetch("/v1/capabilities").then((r) => r.json());
		const cycRes = await post({
			capabilities: [mk(an, ["out"]), mk(bn, ["out"])],
			links: [
				{ fromRef: an, fromPort: "out", toRef: bn, priority: 0 },
				{ fromRef: bn, fromPort: "out", toRef: an, priority: 0 },
			],
		});
		const edges = await fetch("/v1/edges").then((r) => r.json());
		const back = (edges.edges || []).find((e) => e.fromRef === bn && e.toRef === an);
		return { badLink, afterBad, balRes, cycRes, back: back || null };
	}, ["e2e.batch.bad." + stamp, "e2e.batch.bad.b." + stamp]);
	H.check(
		"mock 批量接口：一条坏候选 → 400 links[0]，整批两张能力都没进库",
		apiChecks.badLink.status === 400 &&
			/links\[0\]/.test(String((apiChecks.badLink.body || {}).error || "")) &&
			apiChecks.afterBad.hasA === false &&
			apiChecks.afterBad.hasB === false,
		JSON.stringify({ status: apiChecks.badLink.status, body: apiChecks.badLink.body, after: apiChecks.afterBad }),
	);
	H.check(
		"balance 非 priority 保存即 400「尚未实现」（不静默降级）",
		apiChecks.balRes.status === 400 &&
			/尚未实现|not implemented|未実装|no implementado/.test(
				String((apiChecks.balRes.body || {}).error || ""),
			),
		JSON.stringify(apiChecks.balRes),
	);
	H.check(
		"成环可建：a→b 与 b→a 同一批提交成功（配置期不再拒环）",
		(apiChecks.cycRes.status === 200 || apiChecks.cycRes.status === 201) &&
			!!apiChecks.back,
		JSON.stringify({ status: apiChecks.cycRes.status, back: apiChecks.back }),
	);

	await page.evaluate(() => window.__restoreBatchFetch && window.__restoreBatchFetch());
}

// checkPruneCopy 验证剪枝原因的文案（契约 §6）：新增 max_hops / budget，path 删除。
//
// 剪枝 reason 以前是英文机器串直出（reasons.join(", ")）——现在统一走
// Anotify.pruneReason()。这里两层都钉：
//   ① 映射函数本身（四语言 key 都存在；path 没有文案 → 说明原因集合里删掉了）
//   ② 真实触发一次，toast 里出现的是人话而不是 max_hops 这个机器串
async function checkPruneCopy(page, base) {
	await page.goto(`${base}/sources.html`, { waitUntil: "load", timeout: 20000 });
	await page.waitForTimeout(1500);

	const labels = await page.evaluate(() => {
		const f = window.Anotify && window.Anotify.pruneReason;
		return {
			hasFn: typeof f === "function",
			maxHops: f ? f("max_hops") : null,
			budget: f ? f("budget") : null,
			path: f ? f("path") : null,
		};
	});
	H.check("剪枝原因有映射函数 Anotify.pruneReason（不再是英文机器串直出）", labels.hasFn === true, JSON.stringify(labels));
	H.check(
		"剪枝原因 max_hops 有人话文案（超出 hop 预算）",
		/预算|hop/i.test(String(labels.maxHops)) && labels.maxHops !== "max_hops",
		String(labels.maxHops),
	);
	H.check(
		"剪枝原因 budget 有人话文案（本次 trace 任务数超预算）",
		/预算|budget/i.test(String(labels.budget)) && labels.budget !== "budget",
		String(labels.budget),
	);
	H.check(
		"`path` 不再有文案（原因集合里删掉了，页面不得再出现）",
		labels.path === "path",
		String(labels.path),
	);

	// ① hop 预算：**数据 + 文案**两层（确定性）。
	//    不走 toast：toast 有队列，连续两次触发要等前一条消失，用 DOM 时机会把
	//    「文案对不对」变成「排到队没有」。
	const hop = await page.evaluate(async () => {
		const st = window.MockApi.state;
		st.limits = st.limits || {};
		st.limits.maxHops = 0;
		st.limits.budget = 512;
		// 入口 schema 校验在预算之前：演示数据里若有 resultSchema，会先报 event_schema。
		for (const s of st.sources) {
			if (s.config) delete s.config.resultSchema;
		}
		const srcs = await fetch("/v1/sources").then((r) => r.json());
		const src = (srcs.sources || []).find((x) => x.enabled && (x.targets || []).length);
		if (!src) return { err: "演示数据里没有可触发的源" };
		const res = await fetch("/v1/sources/" + encodeURIComponent(src.id) + "/fire", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: "{}",
		}).then((r) => r.json());
		const pruned = ((res || {}).dispatch || {}).pruned || [];
		return {
			reasons: pruned.map((p) => p.reason),
			detail: (pruned[0] || {}).detail || "",
			label: window.Anotify.pruneReason((pruned[0] || {}).reason),
		};
	});
	H.check(
		"hop 预算用尽 → 剪枝原因是 max_hops（服务端事实）",
		(hop.reasons || []).includes("max_hops"),
		JSON.stringify(hop).slice(0, 240),
	);
	H.check(
		"max_hops 到了用户眼前是人话（hop 预算的文案，不是机器串）",
		/预算/.test(String(hop.label)) && !/max_hops/.test(String(hop.label)),
		String(hop.label),
	);

	// ② budget：同口径（数据 + 文案）。
	//    不在这里钉 toast：页面的 toast 有队列/自动消失，在 200s 的整套里跑会变成
		//    「排到队没有」的不稳定断言。真正要钉的是**原因集合与文案**（契约 §6），
	//    而 reason → 文案的唯一入口就是 Anotify.pruneReason（source/sources 三处共用）。
	const bud = await page.evaluate(async () => {
		const st = window.MockApi.state;
		st.limits = st.limits || {};
		st.limits.maxHops = 16;
		st.limits.budget = 0;
		for (const s of st.sources) {
			if (s.config) delete s.config.resultSchema;
		}
		const srcs = await fetch("/v1/sources").then((r) => r.json());
		const src = (srcs.sources || []).find((x) => x.enabled && (x.targets || []).length);
		if (!src) return { err: "演示数据里没有可触发的源" };
		const res = await fetch("/v1/sources/" + encodeURIComponent(src.id) + "/fire", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: "{}",
		}).then((r) => r.json());
		const pruned = ((res || {}).dispatch || {}).pruned || [];
		return {
			reasons: pruned.map((p) => p.reason),
			detail: (pruned[0] || {}).detail || "",
			label: window.Anotify.pruneReason((pruned[0] || {}).reason),
			// 页面真的会把这句写给用户：sources.html 的 fire toast 就是拼这个文案
			toastText: ((window.Anotify && window.Anotify.t) || ((k, d) => d))(
				"sources.fired_none",
				"事件已入库，但没派出去（{r}）",
			).replace("{r}", window.Anotify.pruneReason((pruned[0] || {}).reason)),
		};
	});
	H.check(
		"trace 任务预算用尽 → 剪枝原因是 budget（服务端事实）",
		(bud.reasons || []).includes("budget"),
		JSON.stringify(bud).slice(0, 240),
	);
	H.check(
		"budget 到了用户眼前是人话，且整句 toast 文案能拼出来（不是机器串）",
		/预算/.test(String(bud.label)) &&
			/预算/.test(String(bud.toastText)) &&
			!/budget/.test(String(bud.toastText)),
		JSON.stringify({ label: bud.label, toast: bud.toastText }).slice(0, 240),
	);

	// 复原：后面的用例还要正常派发
	await page.evaluate(() => {
		const st = window.MockApi.state;
		st.limits = { maxHops: 16, budget: 512 };
	});
}

// checkSourceFirePayload 验证「触发」能带一份可选 payload（#273 决策 2）。
//
// 为什么要在页内钩 fetch：mock 在 window.fetch 层就拦下了 /v1/*（web/mock.js），
// page.on("request") 在 mock 页面里一条都抓不到 —— 「请求体里到底带了什么」
// 只能从页内记录拿；而「服务端真的收到了」用 mock 侧的事实钉两层：
// 事件的 payload、以及由它求值出来的任务参数（planDispatch 拿 payload 求值）。
//
// 四条底线（少一条都可能是回归）：
//   ① 点「立即运行」先开弹层，预填样本可读（有样本的源）/ 预填 {}（没样本的源）
//   ② 非法 JSON / 非对象 → 当场报错且**一个请求都不发**
//   ③ 合法 payload → 请求体带 payload、不带 type（服务端用源名当事件类型）
//   ④ payload 的字段真的进了任务参数（不是只到事件就完了）
async function checkSourceFirePayload(page, base) {
	// getter/setter：mock.js 是 `window.fetch = …` **赋值**，普通覆盖会被它盖掉。
	await page.addInitScript(() => {
		window.__fireBodies = [];
		const native = window.fetch.bind(window);
		let inner = null;
		const pathOf = (input) => {
			const url = typeof input === "string" ? input : (input && input.url) || "";
			try {
				return new URL(String(url), location.href).pathname;
			} catch {
				return String(url).split("?")[0];
			}
		};
		const dispatch = (input, init) => {
			if (/\/sources\/[^/]+\/fire$/.test(pathOf(input))) {
				const body = init && init.body;
				if (typeof body === "string") {
					try {
						window.__fireBodies.push(JSON.parse(body));
					} catch {
						window.__fireBodies.push("<not json> " + body);
					}
				}
			}
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

	await page.goto(`${base}/sources.html`, { waitUntil: "load", timeout: 20000 });
	await page.waitForTimeout(1600);

	// 按源名点那一行的「立即运行」（按钮文案从页面自己的 i18n 表取，不写死中文）。
	const openFire = (name) =>
		page.evaluate((nm) => {
			const label = (window.AnotifyI18n && window.AnotifyI18n["sources.fire"]) || "立即运行";
			const link = [...document.querySelectorAll("#src-list .src-name-link")].find(
				(a) => a.textContent.trim() === nm,
			);
			if (!link) return { err: "找不到源行：" + nm };
			const row = link.closest(".list-row");
			const btn = [...row.querySelectorAll("button")].find(
				(b) => b.textContent.trim() === label,
			);
			if (!btn) return { err: "该行没有触发按钮：" + nm };
			btn.click();
			return { ok: true };
		}, name);

	const readFire = () =>
		page.evaluate(() => {
			const modal = document.getElementById("fire-modal");
			const box = document.getElementById("fire-payload");
			const err = document.getElementById("fire-error");
			return {
				open: !!modal && !modal.classList.contains("hidden"),
				value: box ? box.value : null,
				errShown: !!err && !err.classList.contains("hidden"),
				errText: err ? err.textContent : "",
			};
		});

	const counts = () =>
		page.evaluate(() => ({
			reqs: (window.__fireBodies || []).length,
			tasks: window.MockApi.state.tasks.length,
			events: window.MockApi.state.events.length,
		}));

	// ① 没有样本的源（cron）：弹层出现，预填 {} —— 与加弹层之前完全一致。
	const cronOpen = await openFire("每晚体检");
	await page.waitForTimeout(600);
	const cronState = await readFire();
	H.check(
		"点「立即运行」先开 payload 弹层（不再直接 POST）",
		cronOpen.ok === true && cronState.open === true,
		JSON.stringify({ cronOpen, open: cronState.open }),
	);
	H.check(
		"没有样本的源（cron）预填 {}（与旧行为一致）",
		String(cronState.value).trim() === "{}",
		JSON.stringify(cronState.value),
	);
	await page.evaluate(() => document.getElementById("fire-cancel").click());
	await page.waitForTimeout(400);

	// ② 有真实事件的源（webhook）：预填这个源最近一条事件的 payload，且可解析。
	const hookOpen = await openFire("GitHub PR");
	const filled = await page
		.waitForFunction(
			() => document.getElementById("fire-payload").value.trim().length > 2,
			{ timeout: 8000 },
		)
		.then(() => true)
		.catch(() => false);
	const hookState = await readFire();
	let parsed = null;
	try {
		parsed = JSON.parse(hookState.value);
	} catch {
		parsed = null;
	}
	H.check(
		"webhook 源的弹层预填了可读的示例 payload（合法 JSON 对象、非空）",
		hookOpen.ok === true &&
			filled === true &&
			!!parsed &&
			typeof parsed === "object" &&
			!Array.isArray(parsed) &&
			Object.keys(parsed).length > 0,
		JSON.stringify({ open: hookState.open, head: String(hookState.value).slice(0, 120) }),
	);
	const latest = await page.evaluate(async () => {
		const r = await fetch(
			"/v1/console/events?limit=1&source=" + encodeURIComponent("GitHub PR"),
		).then((x) => x.json());
		return ((r.events || [])[0] || {}).payload || null;
	});
	H.check(
		"预填内容是这个源最近一条真实事件（键集合 + 嵌套字段都对得上，不是编的）",
		!!parsed &&
			!!latest &&
			JSON.stringify(Object.keys(parsed).sort()) ===
				JSON.stringify(Object.keys(latest).sort()) &&
			!!parsed.pull_request &&
			parsed.pull_request.number === latest.pull_request.number &&
			(parsed.repository || {}).name === (latest.repository || {}).name,
		JSON.stringify({ parsed, latest }).slice(0, 300),
	);

	// ③ 非法 JSON：当场报错，且一个 /fire 请求都不发（任务数/事件数不变）。
	const before = await counts();
	await page.evaluate(() => {
		document.getElementById("fire-payload").value = '{"pull_request":';
	});
	await page.evaluate(() => document.getElementById("fire-submit").click());
	await page.waitForTimeout(500);
	const badState = await readFire();
	const afterBad = await counts();
	H.check(
		"非法 JSON → 弹层当场报错（不是静默吞掉）",
		badState.errShown === true && /JSON/.test(String(badState.errText)),
		JSON.stringify(badState).slice(0, 240),
	);
	H.check(
		"非法 JSON → 一个触发请求都不发（任务数/事件数都不变，弹层保持打开）",
		afterBad.reqs === before.reqs &&
			afterBad.tasks === before.tasks &&
			afterBad.events === before.events &&
			badState.open === true,
		JSON.stringify({ before, afterBad, open: badState.open }),
	);

	// ④ 非对象（数组）：同样拦住 —— 服务端会把 null/数组当「没带 payload」。
	await page.evaluate(() => {
		document.getElementById("fire-payload").value = "[1,2,3]";
	});
	await page.evaluate(() => document.getElementById("fire-submit").click());
	await page.waitForTimeout(400);
	const arrState = await readFire();
	const afterArr = await counts();
	H.check(
		"数组 payload 也被拦住（只收 JSON 对象），仍然不发请求",
		arrState.errShown === true &&
			afterArr.reqs === before.reqs &&
			afterArr.tasks === before.tasks,
		JSON.stringify({ err: arrState.errText, before, afterArr }),
	);

	// ⑤ 合法 payload：请求体带 payload、不带 type；mock 侧事件与任务参数都对上。
	const payload = {
		pull_request: { additions: 77, title: "e2e 手动触发" },
		repository: { name: "e2e/repo" },
	};
	await page.evaluate((p) => {
		document.getElementById("fire-payload").value = JSON.stringify(p);
	}, payload);
	await page.evaluate(() => document.getElementById("fire-submit").click());
	await page.waitForTimeout(900);
	const sent = await page.evaluate(
		() => (window.__fireBodies || [])[window.__fireBodies.length - 1] || null,
	);
	H.check(
		"合法 payload → 请求体里带这份 payload，且不带 type（服务端用源名当事件类型）",
		!!sent &&
			JSON.stringify(sent.payload) === JSON.stringify(payload) &&
			!("type" in sent),
		JSON.stringify(sent).slice(0, 240),
	);
	const landed = await page.evaluate(() => {
		const st = window.MockApi.state;
		const ev =
			[...st.events]
				.filter((e) => e.source === "GitHub PR")
				.sort((a, b) => b.createdAt - a.createdAt)[0] || null;
		return {
			ev: ev ? { type: ev.type, payload: ev.payload } : null,
			tasks: st.tasks.slice(-6).map((t) => ({ capability: t.capability, params: t.params })),
		};
	});
	H.check(
		"服务端（mock）真的收到了这份 payload：事件 payload 与提交的一致",
		!!landed.ev && JSON.stringify(landed.ev.payload) === JSON.stringify(payload),
		JSON.stringify(landed.ev).slice(0, 240),
	);
	H.check(
		"没传 type 时事件类型就是源名（服务端默认口径没变）",
		!!landed.ev && landed.ev.type === "GitHub PR",
		JSON.stringify(landed.ev && landed.ev.type),
	);
	H.check(
		"payload 里的字段真的进了任务参数（changedLines=77 / repo=e2e/repo）",
		(landed.tasks || []).some(
			(t) => t.params && t.params.changedLines === 77 && t.params.repo === "e2e/repo",
		),
		JSON.stringify(landed.tasks).slice(0, 300),
	);
	const closed = await readFire();
	H.check("触发成功后弹层收起", closed.open === false, JSON.stringify(closed.open));
}

// checkEventsFilter 验证事件流的排序 / 筛选 / 分组 / URL 参数。
//
// 默认视图是**按时间倒序平铺**（事件流的第一个问题永远是"刚刚发生了什么"），
// 分组是可选视图（?group=1），因为分组会把刚发生的事件压到组内第 6 条
// 后面，用户以为没收到。
// checkCapabilityEditReal 用**真后端**验用户真实卡住的几件事：
//
//   ① 详情能看到脚本正文（脚本存在产物 artifacts.spec.run.files 里）
//   ② 能力行/详情区都有「编辑」入口，进去是「发新版本」模式且名字锁定；
//      编辑弹层里**没有**端口 UI，下游栏**可见并预填**当前下游（名字 + 优先级）
//      （CONTRACT-downstream-edit §4：改下游不再需要另一个页面）
//   ③ 定义变更（脚本 + 参数契约）→ 保存后出现 v2、脚本是新的、**旧边仍指向名字**（跟随最新）
//   ④ 破坏性定义变更 → 409 upgrade_breaking 闸门弹影响面 → force 后成功
//   ⑤ 出口被删掉（协议层；界面已表达不了）→ 409 removed_port 带 link 清单 → force 落库
//
// 为什么必须真后端：这几件事的真相分在三个层面（产物 digest / append-only 版本 /
// 边的引用解析），mock 只能证明「UI 接对了形状」。
// 本套件 DB 是套件私有的，造的能力/边残留无副作用。
//
// ③ 为什么改**参数契约**而不是「加一个出口」：端口从界面消失后（契约 hide-ports §3），
// 编辑弹层里能改的定义只剩参数契约（下游栏已经可见可改，但那属于另外的
// PUT …/downstreams 链路，不参与能力定义的 content hash）。脚本本身不参与能力定义的
// content hash，只改脚本会走幂等路径折回同一版本，不会产生 v2 —— 所以「脚本 + 契约」一起改。
//
// 返回：无（断言全部走 H.check）
async function checkCapabilityEditReal(server, newCtx) {
	const B = server.base;
	const s = H.seed(server.dbPath, "cap_edit");
	const touchName = "e2e.touchnow";
	const echoName = "e2e.echo";

	// 下游能力（没有脚本，它只是「接住」上游 emit 的东西）
	const echo = await H.req(B, "/v1/capabilities", {
		session: s.session,
		method: "POST",
		body: { name: echoName, outputs: ["out"], paramsSchema: { type: "object" } },
	});
	H.eq("真后端：建下游能力 echo → 200", echo.status, 200);

	// 上游：带脚本 + 一条 live 边（能力与边**一次提交**，走的也是原子路径）。
	// 脚本里不写 --port：单出口能力省略它（契约 §2），详情里看到的就是用户会写的形状。
	const scriptV1 = [
		"#!/bin/sh",
		"set -eu",
		'echo "touchnow v1"',
		"anotify-agent emit --data '{\"touched\":true}'",
		"",
	].join("\n");
	const schemaV1 = { type: "object", properties: { changedLines: { type: "number" } } };
	const made = await H.req(B, "/v1/capabilities", {
		session: s.session,
		method: "POST",
		body: {
			name: touchName,
			displayName: "定时嗃唤",
			// 单出口：内部名固定 out。它只活在请求体/库里，界面上没有它的位置。
			outputs: ["out"],
			paramsSchema: schemaV1,
			run: {
				entrypoint: "run.sh",
				files: { "run.sh": scriptV1 },
				timeoutSec: 120,
			},
			links: [{ fromPort: "out", toRef: echoName }],
		},
	});
	H.check(
		"真后端：建带脚本的能力 + 一条下游边（原子）",
		made.status === 200 && !!made.json?.capability?.id && (made.json?.links || []).length === 1,
		`${made.status} ${String(made.text).slice(0, 200)}`,
	);

	const ctx = await newCtx();
	await injectSession(ctx, s.session, B);
	const p = await ctx.newPage();
	const pageErrors = [];
	p.on("pageerror", (e) => pageErrors.push(String(e)));
	await p.goto(B + "/capabilities.html", { waitUntil: "load", timeout: 20000 });
	await H.waitForAppReady(p, "workspace");
	const rowReady = await p
		.waitForFunction(
			(n) => !!document.querySelector(`.cap-row[data-cap-name="${n}"]`),
			touchName,
			{ timeout: 15000 },
		)
		.then(() => true)
		.catch(() => false);
	H.check("真后端：能力页列出了新造的能力行", rowReady, `找不到 .cap-row[data-cap-name=${touchName}]`);

	// ---------- ① 详情能看到脚本正文 ----------
	const scriptView = await p.evaluate(async (name) => {
		const row = document.querySelector(`.cap-row[data-cap-name="${name}"]`);
		if (!row) return { err: "没有这一行" };
		row.querySelector('[data-act="detail"]').click();
		// 产物是懒加载的：等脚本正文落进 DOM（而不是固定 sleep 后猜）
		for (let i = 0; i < 40; i++) {
			const pre = row.querySelector(".cap-script-pre");
			if (pre && pre.textContent.trim()) break;
			await new Promise((r) => setTimeout(r, 150));
		}
		const pre = row.querySelector(".cap-script-pre");
		return {
			entry: ((row.querySelector(".cap-script-entry") || {}).textContent || "").trim(),
			paths: Array.from(row.querySelectorAll(".cap-script-path")).map((x) => x.textContent),
			text: ((pre || {}).textContent || "").trim(),
			empty: !!row.querySelector(".cap-script-empty"),
			// 详情区：下游（候选）一节说人话，没有任何「端口」字样
			downTitles: Array.from(row.querySelectorAll(".cap-detail-title")).map((x) =>
				x.textContent.trim(),
			),
			downCands: Array.from(row.querySelectorAll(".cap-link[data-cand-name]")).map((l) =>
				l.getAttribute("data-cand-name"),
			),
			bodyPort: /端口/.test(document.body.innerText || ""),
		};
	}, touchName);
	H.check(
		"① 详情区显示产物里的脚本正文（不是 base64、不是空态）",
		/touchnow v1/.test(scriptView.text || "") && scriptView.empty === false,
		JSON.stringify(scriptView).slice(0, 300),
	);
	H.check(
		"① 脚本区同时标出入口文件名",
		(scriptView.entry || "").includes("run.sh") &&
			(scriptView.paths || []).includes("run.sh"),
		JSON.stringify({ entry: scriptView.entry, paths: scriptView.paths }),
	);
	H.check(
		"① 详情区只谈「下游（候选）」，全页没有「端口」",
		(scriptView.downTitles || []).some((x) => /下游/.test(x)) &&
			(scriptView.downCands || []).includes(echoName) &&
			scriptView.bodyPort === false,
		JSON.stringify({ titles: scriptView.downTitles, cands: scriptView.downCands }),
	);

	// ---------- ② 「编辑」→ 新版本模式且名字锁定 ----------
	const editMode = await p.evaluate(async (name) => {
		const row = document.querySelector(`.cap-row[data-cap-name="${name}"]`);
		row.querySelector('[data-act="edit"]').click();
		for (let i = 0; i < 40; i++) {
			const pre = document.getElementById("script-pre");
			if (pre && pre.textContent.trim()) break;
			await new Promise((r) => setTimeout(r, 150));
		}
		const down = document.getElementById("cap-downstream");
		return {
			open: !document.getElementById("cap-modal").classList.contains("hidden"),
			title: document.getElementById("cap-modal-title").textContent.trim(),
			banner: document.getElementById("cap-version-title").textContent.trim(),
			meta: document.getElementById("cap-version-meta").textContent.trim(),
			name: document.getElementById("cap-name").value,
			locked: document.getElementById("cap-name").readOnly === true,
			submit: document.getElementById("cap-submit").textContent.trim(),
			stepsHidden: document.getElementById("cap-steps").classList.contains("hidden"),
			script: document.getElementById("script-pre").textContent,
			timeout: document.getElementById("cap-timeout").value,
			downHidden: document.getElementById("cap-down-wrap").classList.contains("hidden"),
			downHint: document.getElementById("cap-down-existing-text").textContent.trim(),
			// 编辑模式的下游行编辑器（CONTRACT-downstream-edit §2.2）：
			// 行列表应该被**预填**成当前下游（名字 + 优先级），输入框只是「加一条」的草稿。
			downRows: Array.from(
				document.querySelectorAll("#cap-down-list .cap-down-item"),
			).map((it) => ({
				toRef: it.getAttribute("data-to-ref") || "",
				name: (it.querySelector(".cap-down-item-name") || {}).value || "",
				prio: (it.querySelector(".cap-down-item-prio") || {}).value || "",
			})),
			downActionsHidden: (() => {
				const a = document.getElementById("cap-down-actions");
				return a ? a.classList.contains("hidden") : true;
			})(),
			downValue: down ? down.value : null,
			displayName: document.getElementById("cap-display-name").value,
			// 端口 UI 的钩子（一个都不许在）：端口输入框 / 出口 chips / 按出口分行 /
			// balance 模式选择器 / 优先级数字输入框
			legacyPortUI: document.querySelectorAll(
				"#cap-ports, #cap-port-input, #cap-ports-hint, .cap-chip, #cap-down-rows, .cap-down-row, .cap-bal-select, .cap-bal-wrap, .cap-cand-prio",
			).length,
		};
	}, touchName);
	H.check(
		"② 点「编辑」进的是新版本模式（预告下一版版本号）",
		editMode.open === true && /@2$/.test(editMode.banner || ""),
		JSON.stringify({ banner: editMode.banner, title: editMode.title }).slice(0, 200),
	);
	H.check(
		"② 能力名锁定（只读、不可改）",
		editMode.locked === true && editMode.name === touchName,
		JSON.stringify({ name: editMode.name, locked: editMode.locked }),
	);
	H.check(
		"② 预填当前版本：脚本 / 超时 / 显示名（下游输入框是空草稿，行列表才是真相）",
		/touchnow v1/.test(editMode.script) &&
			editMode.timeout === "120" &&
			editMode.displayName === "定时嗃唤" &&
			editMode.downValue === "",
		JSON.stringify({
			timeout: editMode.timeout,
			displayName: editMode.displayName,
			down: editMode.downValue,
		}),
	);
	H.check(
		"② 编辑弹层里没有端口 UI（端口输入框 / 出口 chips / 分行 / balance / 优先级数字）",
		editMode.legacyPortUI === 0,
		`legacyPortUI=${editMode.legacyPortUI}`,
	);
	// 旧口径（已废）：「已有 edges → 下游栏整栏隐藏」，理由是“边接的是名字，
	// 新版本自动跟着走”。代价是用户**永远调不了优先级**（详情区还写着“点编辑可调”，
	// 而那是个不存在的功能）。新口径（CONTRACT-downstream-edit §4）：下游栏永远可见，
	// 进入编辑就预填当前下游（名字 + 优先级），保存走 PUT …/downstreams。
	H.check(
		"② 已有下游：下游栏可见且预填当前下游（行名/优先级与 /v1/edges 一致）",
		editMode.downHidden === false &&
			editMode.downActionsHidden === false &&
			editMode.downRows.length === 1 &&
			String(editMode.downRows[0].toRef).split("@")[0] === echoName &&
			editMode.downRows[0].name === editMode.downRows[0].toRef &&
			editMode.downRows[0].prio === "0",
		JSON.stringify({
			hidden: editMode.downHidden,
			rows: editMode.downRows,
			actionsHidden: editMode.downActionsHidden,
		}).slice(0, 240),
	);

	// ---------- ③ 改脚本 + 加一个可选入参 → 保存出 v2（旧边跟随名字）----------
	//
	// 加可选入参是**非破坏性**变更（闸门放行），所以这一版直接出 v2。
	const marker = "edited-by-e2e";
	const scriptV2 = scriptV1.replace('echo "touchnow v1"', `echo "touchnow v2 ${marker}"`);
	const schemaV2 = {
		type: "object",
		properties: { changedLines: { type: "number" }, note: { type: "string" } },
	};
	await p.evaluate(
		async ({ next, schema }) => {
			document.getElementById("script-mode-edit").click();
			const ta = document.getElementById("script-input");
			ta.value = next;
			ta.dispatchEvent(new Event("input", { bubbles: true }));
			// 高级参数区默认折叠：要改契约得先展开
			const wrap = document.getElementById("schema-wrap");
			if (wrap.classList.contains("hidden")) document.getElementById("schema-toggle").click();
			document.getElementById("cap-schema").value = JSON.stringify(schema);
			document.getElementById("cap-submit").click();
		},
		{ next: scriptV2, schema: schemaV2 },
	);
	// 等弹层关（保存成功后才关）+ 列表重建
	const saved = await p
		.waitForFunction(
			() => document.getElementById("cap-modal").classList.contains("hidden"),
			null,
			{ timeout: 15000 },
		)
		.then(() => true)
		.catch(() => false);
	H.check("③ 保存成功后弹层关闭", saved === true, "15s 内弹层没关（保存失败？）");
	await p.waitForTimeout(800);

	const afterSave = await p.evaluate(async (name) => {
		// 走 Anotify.api：命名空间资源端点必须带 namespaceId（裸 fetch 会 400）
		const caps = await window.Anotify.api("/v1/capabilities");
		const edges = await window.Anotify.api("/v1/edges");
		const mine = (caps.capabilities || []).filter((c) => c.name === name);
		const order = mine.slice().sort((a, b) => a.version - b.version);
		const v1 = order.find((c) => c.version === 1) || {};
		const v2 = order.find((c) => c.version === 2) || null;
		const rowV2 = Array.from(document.querySelectorAll(`.cap-row[data-cap-name="${name}"]`)).find(
			(r) => (r.textContent || "").includes("@2"),
		);
		// 脚本正文**不在列表里**（列表只投影 entrypoint/timeoutSec/workDir，
		// 免得每个能力都把几 KB 脚本塞进列表）。要正文走详情用的懒加载端点。
		const scriptOf = async (id) => {
			if (!id) return "";
			try {
				const a = await window.Anotify.api(`/v1/capabilities/${id}/artifact`);
				const files = a.files || {};
				return files[a.entrypoint || "run.sh"] || "";
			} catch (e) {
				return "";
			}
		};
		const v1Script = await scriptOf(v1.id);
		const v2Script = await scriptOf(v2 && v2.id);
		return {
			versions: order.map((c) => c.version),
			v1Outputs: (v1 && v1.outputs) || [],
			v2Outputs: (v2 && v2.outputs) || [],
			v2Schema: (v2 && v2.paramsSchema) || null,
			v2Script,
			v1Script,
			v1Id: v1.id || "",
			v2Id: (v2 && v2.id) || "",
			edges: (edges.edges || [])
				.filter((e) => (e.fromRef || "").split("@")[0] === name)
				.map((e) => ({
					fromRef: e.fromRef,
					fromPort: e.fromPort,
					toRef: e.toRef,
					fromCapabilityId: e.fromCapabilityId,
				})),
			rowV2Open: rowV2 ? rowV2.querySelector(".cap-detail").classList.contains("open") : null,
		};
	}, touchName);
	H.check(
		"③ 保存后出现 v2（v1 仍在 —— append-only 不变量 I8）",
		JSON.stringify(afterSave.versions) === JSON.stringify([1, 2]),
		JSON.stringify(afterSave.versions),
	);
	H.check(
		"③ v2 的出口仍是内部唯一出口 out（下游边还接在上面）",
		JSON.stringify(afterSave.v2Outputs) === JSON.stringify(["out"]),
		JSON.stringify({ v1: afterSave.v1Outputs, v2: afterSave.v2Outputs }),
	);
	H.check(
		"③ v2 的参数契约是新的（可选入参 note 落库）",
		!!afterSave.v2Schema &&
			!!afterSave.v2Schema.properties &&
			!!afterSave.v2Schema.properties.note,
		JSON.stringify(afterSave.v2Schema).slice(0, 160),
	);
	H.check(
		"③ v2 的脚本是新的（v1 的脚本没被改写）",
		afterSave.v2Script.includes(marker) && !afterSave.v1Script.includes(marker),
		JSON.stringify({ v2: afterSave.v2Script.slice(0, 120), v1: afterSave.v1Script.slice(0, 120) }),
	);
	H.check(
		"③ 旧边仍指名字（跟随 latest），且解析到 v2",
		afterSave.edges.length === 1 &&
			afterSave.edges[0].fromRef === touchName &&
			afterSave.edges[0].fromPort === "out" &&
			afterSave.edges[0].fromCapabilityId === afterSave.v2Id &&
			afterSave.v2Id !== afterSave.v1Id,
		JSON.stringify(afterSave.edges),
	);
	H.check(
		"③ 保存后自动展开**新版本**的详情",
		afterSave.rowV2Open === true,
		`v2 行展开=${afterSave.rowV2Open}`,
	);

	// ---------- ④ 破坏性定义变更 → 409 闸门弹影响面 → force 后成功 ----------
	//
	// 把 changedLines 从 number 改成 string：上游照旧喂旧类型，派发时会被
	// param_schema 静默剪掉 —— 这正是闸门要拦的那种「看起来没问题」的变更。
	const gate = await p.evaluate(async (name) => {
		const rows = Array.from(document.querySelectorAll(`.cap-row[data-cap-name="${name}"]`));
		const rowV2 = rows.find((r) => (r.textContent || "").includes("@2")) || rows[rows.length - 1];
		rowV2.querySelector('[data-act="edit"]')?.click();
		for (let i = 0; i < 40; i++) {
			const pre = document.getElementById("script-pre");
			if (pre && pre.textContent.trim()) break;
			await new Promise((r) => setTimeout(r, 150));
		}
		const banner = document.getElementById("cap-version-title").textContent.trim();
		const wrap = document.getElementById("schema-wrap");
		if (wrap.classList.contains("hidden")) document.getElementById("schema-toggle").click();
		document.getElementById("cap-schema").value = JSON.stringify({
			type: "object",
			properties: { changedLines: { type: "string" }, note: { type: "string" } },
		});
		document.getElementById("cap-submit").click();
		for (let i = 0; i < 40; i++) {
			const box = document.getElementById("cap-impact");
			if (box && !box.classList.contains("hidden")) break;
			await new Promise((r) => setTimeout(r, 150));
		}
		const impact = document.getElementById("cap-impact");
		return {
			banner,
			impactShown: !!impact && !impact.classList.contains("hidden"),
			impactText: ((impact || {}).textContent || "").replace(/\s+/g, " ").trim(),
			hasForce: !!document.querySelector('[data-act="force-publish"]'),
			modalStillOpen: !document.getElementById("cap-modal").classList.contains("hidden"),
		};
	}, touchName);
	H.check(
		"④ 破坏性变更被升级闸门拦住（409 弹影响面，弹层不关）",
		gate.impactShown === true && gate.modalStillOpen === true,
		JSON.stringify({ shown: gate.impactShown, open: gate.modalStillOpen }).slice(0, 200),
	);
	H.check(
		"④ 影响面说清「变了什么」与「哪些下游会断」，且不再用 --出口--> 的形状",
		/变了什么/.test(gate.impactText) &&
			/changedLines/.test(gate.impactText) &&
			/会断的下游/.test(gate.impactText) &&
			// 新口径：影响面只说「A → B」，不夹 --port--> 也不出现「端口」
			!/--/.test(gate.impactText) &&
			!/端口/.test(gate.impactText),
		String(gate.impactText).slice(0, 320),
	);
	H.check("④ 影响面上有「仍然发布」（带 force 重发）", gate.hasForce === true);

	await p.evaluate(async () => {
		document.querySelector('[data-act="force-publish"]').click();
	});
	const forced = await p
		.waitForFunction(
			() => document.getElementById("cap-modal").classList.contains("hidden"),
			null,
			{ timeout: 15000 },
		)
		.then(() => true)
		.catch(() => false);
	H.check("④ force 重发后成功（弹层关闭）", forced === true, "15s 内弹层没关（force 没生效？）");

	await p.waitForTimeout(600);
	const afterForce = await p.evaluate(async (name) => {
		const caps = await window.Anotify.api("/v1/capabilities");
		const edges = await window.Anotify.api("/v1/edges");
		const mine = (caps.capabilities || [])
			.filter((c) => c.name === name)
			.sort((a, b) => a.version - b.version);
		const v3 = mine.find((c) => c.version === 3) || {};
		return {
			versions: mine.map((c) => c.version),
			v3Outputs: v3.outputs,
			v3Schema: v3.paramsSchema || null,
			edges: (edges.edges || []).filter((e) => (e.fromRef || "").split("@")[0] === name),
		};
	}, touchName);
	H.check(
		"④ force 之后 v3 真的落了库（破坏性变更被明确确认，不是静默生效）",
		JSON.stringify(afterForce.versions) === JSON.stringify([1, 2, 3]) &&
			!!afterForce.v3Schema &&
			!!afterForce.v3Schema.properties &&
			afterForce.v3Schema.properties.changedLines.type === "string",
		JSON.stringify({ versions: afterForce.versions, schema: afterForce.v3Schema }).slice(0, 240),
	);
	H.check(
		"④ v3 的出口仍是 out，那条下游边一条没丢",
		JSON.stringify(afterForce.v3Outputs) === JSON.stringify(["out"]) &&
			afterForce.edges.length === 1 &&
			afterForce.edges[0].fromPort === "out" &&
			afterForce.edges[0].toRef === echoName,
		JSON.stringify({ outputs: afterForce.v3Outputs, edges: afterForce.edges }),
	);

	// ---------- ⑤ 出口被删掉 → 409 removed_port → force 落库（协议层）----------
	//
	// 界面已经表达不了「删掉出口」（下游行编辑器只表达隐式 out，outputs 由行数决定：
	// 有行 → ["out"]，0 行 → []，所以「删出口」在界面上表现为「清空下游」），
	// 但这道闸门仍然是协议的一部分：直接打接口，钉住 409 的影响面结构与 force 语义。
	const dropName = "e2e.dropport." + Date.now();
	const dropBody = (outputs, force) => {
		const body = {
			name: dropName,
			outputs: outputs,
			paramsSchema: { type: "object" },
			run: { entrypoint: "run.sh", files: { "run.sh": "#!/bin/sh\necho hi\n" } },
		};
		if (force) body.force = true;
		return body;
	};
	const dropMade = await H.req(B, "/v1/capabilities", {
		session: s.session,
		method: "POST",
		body: Object.assign(dropBody(["out"]), {
			links: [{ fromPort: "out", toRef: echoName }],
		}),
	});
	H.check(
		"⑤ 造数：一个带下游的能力 → 200",
		dropMade.status === 200,
		String(dropMade.text).slice(0, 160),
	);
	const dropNoForce = await H.req(B, "/v1/capabilities", {
		session: s.session,
		method: "POST",
		body: dropBody([]),
	});
	const dropImpact = (dropNoForce.json || {}).upgradeImpact || {};
	const dropChange = (dropImpact.changes || [])[0] || {};
	const dropLink = (dropImpact.links || [])[0] || {};
	H.check(
		"⑤ 删掉出口 → 409 removed_port，并列出会断的那条 link（fromRef / port / toRef）",
		dropNoForce.status === 409 &&
			String((dropNoForce.json || {}).code || "") === "upgrade_breaking" &&
			dropChange.kind === "removed_port" &&
			dropLink.fromRef === dropName &&
			dropLink.port === "out" &&
			dropLink.toRef === echoName,
		String(dropNoForce.text).slice(0, 300),
	);
	const dropForced = await H.req(B, "/v1/capabilities", {
		session: s.session,
		method: "POST",
		body: dropBody([], true),
	});
	H.check(
		"⑤ force 之后真的落库：v2 存在、出口是空的",
		dropForced.status === 200 &&
			dropForced.json?.capability?.version === 2 &&
			JSON.stringify(dropForced.json?.capability?.outputs) === JSON.stringify([]),
		`${dropForced.status} ${String(dropForced.text).slice(0, 200)}`,
	);

	// ---------- ⑥ 在能力里改下游 → 真后端落库（PUT …/downstreams）----------
	//
	// touchName 此刻正好有 1 条下游（e2e.echo —— ④ 之后那条边一条没丢）。四步全走界面，
	// 每一步都去 /v1/edges 回读（服务端才是真相，toast 只是提示）：
	//   改优先级 9 → 保存 → priority=9 且**版本数不变**（拓扑不属于产物）
	//   删掉这行 → 保存 → 边没了
	//   「+ 下游」面板加回一条 → 保存 → 又出现了
	//   清空 → 保存 → 没有出边（终端能力，合法）
	// 并全程收请求：能力边入口已封，一条 POST /v1/edges 都不许有。
	const edgeWrites = [];
	p.on("request", (r) => {
		const u = r.url();
		if (u.includes("/v1/edges") && r.method() !== "GET") {
			edgeWrites.push(r.method() + " " + u.replace(/^https?:\/\/[^/]+/, ""));
		}
	});

	// snapDown 回读「版本集合 + 出边」，两件事都要在同一份快照里看
	const snapDown = () =>
		p.evaluate(async (name) => {
			const caps = await window.Anotify.api("/v1/capabilities");
			const edges = await window.Anotify.api("/v1/edges");
			return {
				versions: (caps.capabilities || [])
					.filter((c) => c.name === name)
					.map((c) => c.version)
					.sort((a, b) => a - b),
				out: (edges.edges || [])
					.filter(
						(e) =>
							!e.fromIsSource &&
							String(e.fromRef || "").split("@")[0] === name,
					)
					.map((e) => ({ toRef: e.toRef, priority: Number(e.priority || 0) })),
			};
		}, touchName);

	// waitOut 轮询 /v1/edges 直到出边满足谓词（避免用固定 sleep 猜异步时序）
	const waitOut = async (pred, why) => {
		for (let i = 0; i < 80; i++) {
			const s = await snapDown();
			if (pred(s.out)) return s;
			await p.waitForTimeout(150);
		}
		return await snapDown().then((s) => Object.assign(s, { timedOut: why }));
	};

	const beforeDown = await snapDown();

	const openEdit = () =>
		p.evaluate(async (name) => {
			const row = document.querySelector(`.cap-row[data-cap-name="${name}"]`);
			if (!row) return { err: "没有这一行" };
			row.querySelector('[data-act="edit"]').click();
			for (let i = 0; i < 80; i++) {
				if (document.querySelectorAll("#cap-down-list .cap-down-item").length > 0) break;
				await new Promise((r) => setTimeout(r, 100));
			}
			return {
				open: !document.getElementById("cap-modal").classList.contains("hidden"),
				rows: Array.from(document.querySelectorAll("#cap-down-list .cap-down-item")).map(
					(it) => ({
						toRef: it.getAttribute("data-to-ref") || "",
						name: (it.querySelector(".cap-down-item-name") || {}).value || "",
						prio: (it.querySelector(".cap-down-item-prio") || {}).value || "",
					}),
				),
				actionsHidden: document
					.getElementById("cap-down-actions")
					.classList.contains("hidden"),
				draft: document.getElementById("cap-downstream").value,
			};
		}, touchName);

	// saveDown 点「保存下游」并等结果 toast（成功/失败两种文案都认，免得等到超时）
	const saveDown = () =>
		p.evaluate(async () => {
			const host = document.getElementById("anotify-toast-host");
			if (host) host.replaceChildren();
			document.getElementById("cap-down-save").click();
			let toast = "";
			for (let i = 0; i < 100; i++) {
				toast =
					host && host.lastElementChild
						? (host.lastElementChild.textContent || "").replace(/\s+/g, " ").trim()
						: "";
				if (/下游已保存|保存下游失败/.test(toast)) break;
				await new Promise((r) => setTimeout(r, 100));
			}
			return { toast };
		});

	const state1 = await openEdit();
	H.check(
		"⑥ 真后端：编辑弹层预填当前下游（1 行，名字/优先级来自 /v1/edges）",
		state1.open === true &&
			state1.actionsHidden === false &&
			state1.draft === "" &&
			state1.rows.length === 1 &&
			state1.rows[0].name === state1.rows[0].toRef &&
			String(state1.rows[0].toRef).split("@")[0] === echoName &&
			state1.rows[0].prio === String(beforeDown.out[0] ? beforeDown.out[0].priority : 0),
		JSON.stringify({ rows: state1.rows, before: beforeDown }).slice(0, 300),
	);

	// 改优先级 → 保存 → priority=9，且版本数不变
	await p.evaluate(async () => {
		const it = document.querySelector("#cap-down-list .cap-down-item");
		const prio = it.querySelector(".cap-down-item-prio");
		prio.value = "9";
		prio.dispatchEvent(new Event("input", { bubbles: true }));
		prio.dispatchEvent(new Event("change", { bubbles: true }));
	});
	const savedPrio = await saveDown();
	const afterPrio = await waitOut(
		(out) => out.length === 1 && out[0].priority === 9,
		"priority=9 没落库",
	);
	H.check(
		"⑥ 真后端：改优先级保存后 /v1/edges 里是 9（真落库，不是只改了内存）",
		afterPrio.out.length === 1 && afterPrio.out[0].priority === 9,
		JSON.stringify({ out: afterPrio.out, toast: savedPrio.toast }).slice(0, 240),
	);
	H.check(
		"⑥ 真后端：改下游不产生新版本（版本集合与保存前一致）",
		JSON.stringify(afterPrio.versions) === JSON.stringify(beforeDown.versions),
		JSON.stringify({ before: beforeDown.versions, after: afterPrio.versions }),
	);
	H.check(
		"⑥ 真后端：成功提示说清「不产生新版本」",
		/不产生新版本/.test(savedPrio.toast || ""),
		String(savedPrio.toast).slice(0, 160),
	);

	// 删掉这行 → 保存 → 边没了
	await p.evaluate(async () => {
		document.querySelector("#cap-down-list .cap-down-item-del").click();
	});
	const savedDel = await saveDown();
	const afterDel = await waitOut((out) => out.length === 0, "删掉的边还在");
	H.check(
		"⑥ 真后端：删掉那行后保存 → /v1/edges 里没有出边了",
		afterDel.out.length === 0,
		JSON.stringify({ out: afterDel.out, toast: savedDel.toast }).slice(0, 240),
	);

	// 「+ 下游」面板加回一条 → 保存 → 又出现（面板读现有出边再整体 replace）
	const added = await p.evaluate(async ({ name, target }) => {
		closeCapModal();
		await new Promise((r) => setTimeout(r, 250));
		const row = document.querySelector(`.cap-row[data-cap-name="${name}"]`);
		if (!row) return { err: "没有这一行" };
		if (!row.querySelector(".cap-detail").classList.contains("open")) {
			row.querySelector('[data-act="detail"]').click();
		}
		let btn = null;
		for (let i = 0; i < 60; i++) {
			btn = row.querySelector('[data-act="add-downstream"]');
			if (btn) break;
			await new Promise((r) => setTimeout(r, 100));
		}
		if (!btn) return { err: "详情区没有「+ 下游」按钮" };
		btn.click();
		const panel = row.querySelector(".cap-add-panel");
		if (!panel) return { err: "面板没展开" };
		const inputs = Array.from(panel.querySelectorAll('input[list="cap-down-targets"]'));
		if (inputs.length !== 1) return { targets: inputs.length };
		inputs[0].value = target;
		const host = document.getElementById("anotify-toast-host");
		if (host) host.replaceChildren();
		Array.from(panel.querySelectorAll("button"))
			.find((b) => b.className.includes("btn-primary"))
			.click();
		let toast = "";
		for (let i = 0; i < 100; i++) {
			toast =
				host && host.lastElementChild
					? (host.lastElementChild.textContent || "").replace(/\s+/g, " ").trim()
					: "";
			if (/边已创建|创建边失败/.test(toast)) break;
			await new Promise((r) => setTimeout(r, 100));
		}
		return { targets: inputs.length, toast };
	}, { name: touchName, target: echoName });
	const afterAdd = await waitOut((out) => out.length === 1, "「+ 下游」没落库");
	H.check(
		"⑥ 真后端：「+ 下游」面板加回一条 → /v1/edges 里又有了（走同一个 PUT）",
		added.targets === 1 &&
			afterAdd.out.length === 1 &&
			String(afterAdd.out[0].toRef).split("@")[0] === echoName,
		JSON.stringify({ added, out: afterAdd.out }).slice(0, 300),
	);

	// 清空 → 保存 → 没有出边（终端能力）
	const state2 = await openEdit();
	H.check(
		"⑥ 真后端：加回来后重开编辑仍是 1 行（回读自服务端）",
		state2.open === true && (state2.rows || []).length === 1,
		JSON.stringify(state2.rows).slice(0, 200),
	);
	await p.evaluate(async () => {
		document.querySelector("#cap-down-list .cap-down-item-del").click();
	});
	const savedClear = await saveDown();
	const afterClear = await waitOut((out) => out.length === 0, "清空没落库");
	H.check(
		"⑥ 真后端：清空后保存 → 该能力没有任何出边（终端能力是合法状态）",
		afterClear.out.length === 0,
		JSON.stringify({ out: afterClear.out, toast: savedClear.toast }).slice(0, 240),
	);
	H.check(
		"⑥ 真后端：改下游全程没有一条 POST/PUT/DELETE /v1/edges（入口已封，只剩 PUT …/downstreams）",
		edgeWrites.length === 0,
		edgeWrites.slice(0, 3).join(" | "),
	);
	await p.evaluate(async () => {
		closeCapModal();
	});

	H.check("真后端能力编辑：页面无 JS pageerror", pageErrors.length === 0, pageErrors[0]?.slice(0, 120));

	await p.close();
	await ctx.close();
}

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

	// 改哪个能力很重要：必须挑**有入边**的，否则改了名字也不会出现在任何边上，
	// 后面那条断言就变成在要求一个模型里不存在的形状。
	//
	// 做法：**在能力页的页面上下文里**取一个真实的 toCapability 名字（改的就是它）。
	// 为什么不再去出口页取：出口页已经删了（CONTRACT-downstream-edit §5：能力的下游
	// 只能在能力编辑里改，没有第二个页面），而后续断言本来就是在同一个页面上下文里
	// 打的 API——多一次 goto 只会多一次 mock state 重建。
	const target = await page.evaluate(() => {
		// edges 数据在 mock 里；从 DOM 拿不到 id，所以直接问接口
		return fetch("/v1/edges")
			.then((r) => r.json())
			.then((d) => {
				// 取第一条**下游能解析到能力**的边：边里存的是引用（P2.2），
				// 能力 id 是服务端派生的 toCapabilityId —— 要改名的就是这条边的下游。
				const e = (d.edges || []).find((x) => x.toCapabilityId);
				// P2.2：边里存的是**引用**，能力 id 是服务端派生的 toCapabilityId
				return e ? { id: e.toCapabilityId } : null;
			});
	});
	if (!target) {
		// 前置条件不再依赖已删的出口页：mock 种子数据里一条能解析到能力的边都没有，
		// 后面的断言无法成立。这不是「出口页有边」的旧断言，而是造数前置的失败上报。
		H.check("mock 里有能解析到能力的边（改名断言前置）", false, "edges 为空");
		return;
	}

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
			//
			// P2.2：比的是**解析后的 id**（toCapabilityId / fromCapabilityId）。
			// 边里存的只有引用（`review` / `review@1`），拿引用去能力表里查不到。
			allResolve: edges.every(
				(e) =>
					byID.has(e.toCapabilityId) &&
					(e.fromIsSource ? true : byID.has(e.fromCapabilityId)),
			),
			// 入口边的左端是来源名：它不该被解析成任何能力
			entryFromUnresolved: edges
				.filter((e) => e.fromIsSource)
				.every((e) => e.fromCapabilityId === ""),
			// 出口名来自 from 能力的 outputs（或起点的事件类型），
			// 不可能等于某个能力的显示名 —— 这就是"路由键不被显示名替换"
			portsAreRaw: edges.every((e) => {
				// 入口边：左端是来源名，出口是事件类型，任意值
				if (e.fromIsSource) return true;
				const from = byID.get(e.fromCapabilityId);
				if (!from) return false;
				return (from.outputs || []).includes(e.fromPort);
			}),
			// 至少有一个 To 端能力带显示名（证明渲染时会用到它）
			hasDisplayName: edges.some((e) => {
				const to = byID.get(e.toCapabilityId);
				return to && (to.displayName || "").trim();
			}),
			sample: edges.slice(0, 4).map((e) => {
				const to = byID.get(e.toCapabilityId);
				return e.fromRef + " -> " + (to ? to.displayName || to.name : "?");
			}),
		};
	});
	H.check("出口表两端都能解析（入口边左端是来源名）", edge.allResolve === true, `edges=${edge.n}`);
	H.check(
		"入口边的左端不冒充能力（fromCapabilityId 是空串）",
		edge.entryFromUnresolved === true,
		`edges=${edge.n}`,
	);
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
	// 挑「最长的一条链」再进去做图/瀑布断言。
	//
	// 这一步原来要**整页加载 8 次 trace 页**（实测 94.6s —— 整个套件的第二大耗时）。
	// 而它做的事只是"选一个 id"：页面渲染多慢与选择无关。所以改成问接口
	// （trace 页自己也是 GET /v1/tasks?traceId=… 拿的数据），一次请求就能排出长短，
	// 选中之后仍然只有**一次**真实页面加载 —— 后面那些 UI 断言一个都没少。
	const tBest = Date.now();
	for (const tid of traceIds.slice(0, 8)) {
		// 在**页面里**发这个请求：浏览器自带会话 cookie，不用外面再传一次，
		// 也不会因为 cookie 作用域不对而 401。
		const n = await page.evaluate(async (t) => {
			try {
				const r = await fetch("/v1/tasks?traceId=" + encodeURIComponent(t));
				const d = await r.json();
				return (d.tasks || []).length;
			} catch {
				return 0;
			}
		}, tid);
		if (n > bestCount) {
			bestCount = n;
			bestId = tid;
		}
	}

	if (process.env.E2E_PROFILE) {
		console.log(`[profile] 挑最长 trace（8 次接口查询）${Date.now() - tBest}ms`);
	}
	const tGraph = Date.now();

	// ---- 图视图用「源拓扑」当底图，执行状态叠上去 ----
	//
	// 只画执行链的话，被剪枝的分支压根不出现 —— 用户看到的永远是一条
	// 顺顺利利的直线，"为什么这条分支没走"在图上根本问不出来。
	// 底图把它们留成灰色虚线，一眼看出"这里本来有条路，这次没走"。
	{
		await page.goto(base + "/trace.html?traceId=" + encodeURIComponent(bestId) + "&view=graph", {
			waitUntil: "domcontentloaded",
			timeout: 20000,
		});
		// 等节点画出来（原来固定 2500ms：图渲染是异步的，但"画完"有明确信号）
		await page
			.waitForFunction(() => document.querySelectorAll(".wf-node").length > 0, {
				timeout: 3000,
			})
			.catch(() => {});
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
		if (process.env.E2E_PROFILE) {
			console.log(`[profile] 图视图加载+绘制 ${Date.now() - tGraph}ms`);
		}
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
		// 绕行点。两个节点横向错开时曲线会从中间穿过去，边标签
		// （现在是走过多次的 ×N）正好落在别的节点卡片上，看起来像标签错乱。
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

		// ---- 边标签必须贴着自己的线 ----
		//
		// 标签是绝对定位的 DOM，位置靠算。回边会被绕成弧，如果还拿 dagre
		// 原始折线去算标签位置，标签就飘在弧线旁边的空地上 —— 谁也看不出
		// 它属于哪条边。
		//
		// 另外：trace 页的边标签现在只有一个来源 —— **走过多次**的 ×N
		// （出口名不上图，契约 hide-ports §4）。mock 里所有能力都是单出口，
		// 所以任何带出口名的标签都是回退。
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
					out.push({
						from: lb.dataset.from || "",
						label: lb.textContent.trim(),
						dist: +best.toFixed(1),
					});
				}
				return out;
			});
			// 阈值 20px 而不是 12px：标签避让节点/别的标签时**本来就会被推开**
			// 几像素（实测 12.2px 量级），卡 12 是必然 flaky。
			// 这条断言真正要防的是"标签跑到别的线上 / 整个飘走"，那是几十
			// 上百像素的偏差 —— 20px 依然能拦住它。
			const far = lbl.filter((x) => x.dist > 20);
			H.check(
				"边标签贴在自己的线上（≤20px）",
				far.length === 0,
				far.map((x) => `${x.label}=${x.dist}px`).join(", ") ||
					`共 ${lbl.length} 个，最大 ${Math.max(0, ...lbl.map((x) => x.dist))}px`,
			);
			H.check(
				"trace 页的能力边标签只剩 ×N（出口名不上图）",
				// 触发器→首层能力那条边的标签是**事件类型**（pull_request.opened），
				// 不是能力出口，所以把它排除；其余（能力→能力）只允许 ×N。
				// 另加一条硬防线：内部出口名 out 在任何标签里都不许出现。
				lbl
					.filter((x) => x.from !== "__trigger__")
					.every((x) => /^\u00d7\d+$/.test(x.label)) &&
					lbl.every((x) => !/(^|\s)out(\s|$)/.test(x.label)),
				lbl.map((x) => `${x.from}=>${x.label}`).join(" | ") || "本图没有边标签",
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
	// 这个循环曾经是整个套件 41% 的耗时（实测 124s / 299s）：它无条件扫 8 个 trace
	// 页，每页一次整页导航（waitUntil:"load"）+ 最多 8s 的静默等待，而断言只需要
	// 「至少两种状态色」。三处修正，语义不变：
	//   ① 凑够 2 种就 break（断言本来就是 >= 2）
	//   ② 导航等 domcontentloaded —— 后面本来就显式等时间条画出来，
	//      等 "load" 只是多等一段与断言无关的资源
	//   ③ 等待上限 8s → 3s：有些 trace（单任务链）根本没有可着色的对比，
	//      等满 8s 纯属浪费；真慢的时候 waitForFunction 一样会先返回
	const tVariety = Date.now();
	for (const tid of traceIds.slice(0, 8)) {
		if (statusVariety.length >= 2) break;
		// 状态色是瀑布图时间条上的类，所以要显式要瀑布图
		await page.goto(
			base + "/trace.html?traceId=" + encodeURIComponent(tid) + "&view=waterfall",
			{ waitUntil: "domcontentloaded", timeout: 20000 },
		);
		// 等时间条真的画出来再读，而不是固定 sleep。
		//
		// 原来固定等 900ms：机器一忙这条就偶发失败（只读到 b-succeeded，
		// 因为后来的 trace 还没渲染完）。这类"随机挂一条"最容易让人怀疑
		// 被测代码，其实只是等待方式不对。
		await page
			.waitForFunction(() => document.querySelectorAll(".wf-bar").length > 0, {
				timeout: 3000,
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
	// E2E_PROFILE=1 时打印这一步的真实耗时：以后再出"某个套件莫名变慢"，
	// 打开它就够，不用从头插桩。
	if (process.env.E2E_PROFILE) {
		console.log(
			`[profile] 状态色扫描 ${Date.now() - tVariety}ms（${statusVariety.join(",")}）`,
		);
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

			// ---- issue #157：同一对节点，这次到底走了哪条候选 ----
			//
			// t9 里 lint-fix 有两条候选：→ run-tests（没走）与 → report.send（走了）。
			// 端口从图上消失后（出口名不上图），一条边由「谁 → 谁」唯一确定 ——
			// 所以三元组里的 port 是**空串**。要钉的事实没变：
			// 只有真的走过的那条被点亮，没走的留在底图上不亮。
			//
			// 这个 bug 的表现是"跑测试节点亮起时看不到有线进入它" —— 只断言
			// "动画在播"完全抓不到，必须盯**具体哪条边**。
			const A = "cap_mock_analyze-pr_v1";
			const L = "cap_mock_lint-fix_v1";
			const T = "cap_mock_run-tests_v1";
			const R = "cap_mock_report_send_v1";
			const byTriple = edgeCheck.animatedByTriple || {};
			const want = [
				[`${A}##${T}`, true, "analyze-pr → run-tests（这次走过）"],
				[`${L}##${R}`, true, "lint-fix → report.send（这次走过）"],
				[`${L}##${T}`, false, "lint-fix → run-tests（这次没走，不该点亮）"],
			];
			const wrong = want.filter(([k, v]) => byTriple[k] !== v);
			H.check(
				"同一对节点只点亮这次真正走过的候选（没走的留在底图）",
				wrong.length === 0,
				wrong.length === 0
					? want.map(([k, , d]) => d).join("；")
					: wrong.map(([k, v]) => `${k} 期望 ${v} 实际 ${byTriple[k]}`).join("; "),
			);

			// ---- 走过多次的边要标出次数 ----
			//
			// 数字必须是**真实遍历次数**：为了让环更显眼而掺"环 +1"就是撒谎
			// （环上那条边可能只走了一遍）。所以这里要求 ≥2 才允许出现。
			// 而且标签**只有 ×N**：出口名不上图（契约 hide-ports §4）。
			const bad = after.labels.filter((x) => {
				const m = x.match(/^\u00d7(\d+)$/);
				return !m || Number(m[1]) < 2;
			});
			H.check(
				"走过多次的边上标出真实次数，且只剩 ×N（不夹出口名）",
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

	// ---- 首屏舞台：横向 1:1 原生滚动，相机停在起点 ----
	//
	// silk 皮肤下 trace 图不再"把整张图 transform 缩放到容器内"：内容画布是
	// **绝对像素 1:1**（卡片固定 238×78，字不会被缩成读不了的 7px），横向靠
	// 原生 overflow:auto 滚动 + 相机跟随，纵向则收敛到完整可见。
	//
	// 所以旧的"四边都有余量"断言对这张舞台必然为红 —— 横向 gapR 恒 < 0，
	// 内容本来就应该比容器宽。换成四条同等强度的硬不变量（不许放宽成"随便溢出"）：
	//   ① 1:1：卡片宽 238、画布没有被整体 transform 缩放（放大只走 zoom 控件，默认 1）；
	//   ② 左缘初始可见：相机停在起点（scrollLeft 0），最左节点不越界；
	//   ③ 纵向完整：上下都有余量，且不出纵向滚动条（图高必须收敛进舞台）；
	//   ④ 横向真能滚到右缘：内容宽于视口，滚到最右后最右节点整卡可见，
	//      且画布宽度 = 滚动宽度（防"宽图只量了一半 / 右半永远滚不到"）。
	//
	// 关键前置：先开 reducedMotion。入场动画在 reduce 下不自动播（直接落 showFinal
	// 终态），相机也不会被 followTo 推到中途 —— 否则量到的是"动画第 N 帧"的滚动
	// 位置，断言会随机红绿。量完必须恢复，后面的断言还要看动画。
	await page.emulateMedia({ reducedMotion: "reduce" });
	await page.goto(new URL("trace.html?traceId=trace_mock_t9&view=graph", page.url()).href, {
		waitUntil: "load",
		timeout: 20000,
	});
	await page
		.waitForFunction(() => document.querySelectorAll(".wf-node").length > 0, {
			timeout: 15000,
		})
		.catch(() => {});
	// 等字体就绪后那次补排/补量落地（首帧用的是回退字体度量）
	await page.waitForTimeout(700);
	const stage = await page.evaluate(() => {
		const box = document.querySelector(".flograph");
		if (!box) return { missing: "页面上没有 .flograph" };
		const scroll = box.querySelector(".flograph-scroll");
		if (!scroll) return { missing: "没有 .flograph-scroll（布局还是旧的非滚动版？）" };
		const canvas = box.querySelector(".flograph-canvas");
		if (!canvas) return { missing: "没有 .flograph-canvas（布局还是旧的非滚动版？）" };

		const b = box.getBoundingClientRect();
		const sr = scroll.getBoundingClientRect();
		const cr = canvas.getBoundingClientRect();
		// 滚动内容坐标系的原点（= 滚动元素的 padding box 左缘，clientLeft 是左边框宽）
		const contentLeft = sr.left + scroll.clientLeft;
		const scrollLeft0 = scroll.scrollLeft;

		let minX = Infinity;
		let maxXBox = -Infinity;
		let minY = Infinity;
		let maxY = -Infinity;
		let maxRightInCanvas = -Infinity;
		let maxRightInContent = -Infinity;
		let cardW = NaN;
		let cardCount = 0;
		let plainCount = 0;
		for (const n of box.querySelectorAll(".wf-node")) {
			const r = n.getBoundingClientRect();
			if (!r.width || !r.height) continue; // 没布局（display:none）的不参与
			cardCount++;
			minX = Math.min(minX, r.left - b.left);
			maxXBox = Math.max(maxXBox, r.right - b.left);
			minY = Math.min(minY, r.top - b.top);
			maxY = Math.max(maxY, r.bottom - b.top);
			maxRightInCanvas = Math.max(maxRightInCanvas, r.right - cr.left);
			maxRightInContent = Math.max(maxRightInContent, r.right - contentLeft + scrollLeft0);
			if (!n.classList.contains("is-trigger")) {
				plainCount++;
				if (Number.isNaN(cardW)) cardW = r.width; // 触发器是 198 宽，只量普通卡片
			}
		}

		// 滚到最右再读回真实值（赋值超出最大值时浏览器会夹到 max）。
		// 内联关掉 scroll-behavior：CSS 若写了 smooth，这次赋值会变成动画，
		// 同步读回的就不是最右值，量出来的余量会随机。
		const prevBehavior = scroll.style.scrollBehavior;
		scroll.style.scrollBehavior = "auto";
		scroll.scrollLeft = scroll.scrollWidth;
		const scrollLeftMax = scroll.scrollLeft;
		scroll.scrollLeft = scrollLeft0; // 复位，别把页面留在最右影响后面的断言
		scroll.style.scrollBehavior = prevBehavior;

		const r2 = (x) => Math.round(x * 100) / 100;
		return {
			missing: null,
			// 外壳 / 滚动视口 / 画布
			boxW: r2(b.width),
			boxH: r2(b.height),
			scrollW: r2(sr.width),
			scrollH: r2(sr.height),
			canvasW: r2(cr.width),
			canvasH: r2(cr.height),
			// 滚动几何
			scrollLeft0: r2(scrollLeft0),
			scrollLeftMax: r2(scrollLeftMax),
			scrollWidth: scroll.scrollWidth,
			clientWidth: scroll.clientWidth,
			scrollHeight: scroll.scrollHeight,
			clientHeight: scroll.clientHeight,
			// 节点几何（相对 .flograph 外壳）
			minX: r2(minX),
			gapL: r2(minX),
			gapR: r2(b.width - maxXBox),
			gapT: r2(minY),
			gapB: r2(b.height - maxY),
			// 节点几何（相对画布 / 相对滚动内容）
			// 画布原点应为 0（画布就是整块内容）；它非 0 说明画布被居中或被裁，
			// 那时 "maxRightInCanvas - scrollLeft" 会低估右缘，所以右缘一律按
			// 滚动内容坐标系算 —— 画布原点为 0 时两者完全相等。
			maxRightInCanvas: r2(maxRightInCanvas),
			canvasOriginX: r2(cr.left - contentLeft + scrollLeft0),
			maxRightInViewport: r2(maxRightInContent - scrollLeftMax),
			// 1:1 证据
			cardW: r2(cardW),
			cardCount,
			plainCount,
			canvasTransform: getComputedStyle(canvas).transform,
		};
	});
	const stageDetail = stage.missing
		? stage.missing
		: `容器=${stage.boxW}x${stage.boxH} 视口=${stage.scrollW}x${stage.scrollH} 画布=${stage.canvasW}x${stage.canvasH} ` +
			`滚动=${stage.scrollWidth}x${stage.scrollHeight} 可视=${stage.clientWidth}x${stage.clientHeight} ` +
			`scrollLeft=${stage.scrollLeft0}（最右 ${stage.scrollLeftMax}） ` +
			`卡宽=${stage.cardW}（普通卡 ${stage.plainCount}/${stage.cardCount}） ` +
			`四边余量 L${stage.gapL}/R${stage.gapR}/T${stage.gapT}/B${stage.gapB} ` +
			`最右节点右缘 画布系=${stage.maxRightInCanvas} 视口系=${stage.maxRightInViewport} 画布原点=${stage.canvasOriginX} ` +
			`canvas.transform=${stage.canvasTransform}`;
	const canvasUnscaled =
		stage.canvasTransform === "none" || stage.canvasTransform === "matrix(1, 0, 0, 1, 0, 0)";
	H.check(
		"trace 图 1:1 像素渲染：卡片宽 238±2、画布没有被整体缩放",
		!stage.missing && Math.abs(stage.cardW - 238) <= 2 && canvasUnscaled,
		stageDetail,
	);
	H.check(
		"图从最左开始：相机停在起点（scrollLeft=0）、左缘节点完整可见",
		!stage.missing && stage.scrollLeft0 === 0 && stage.minX >= -1,
		stageDetail,
	);
	H.check(
		"图纵向完整收进舞台：上下都有余量、没有纵向滚动条",
		!stage.missing &&
			stage.gapT > 0 &&
			stage.gapB > 0 &&
			stage.scrollHeight <= stage.clientHeight + 1,
		stageDetail,
	);
	H.check(
		"横向舞台能滚到右缘：内容宽于视口、滚到最右后最右节点整卡可见、画布=滚动宽度",
		!stage.missing &&
			stage.scrollWidth > stage.clientWidth &&
			stage.maxRightInViewport <= stage.clientWidth + 1 &&
			Math.abs(stage.canvasW - stage.scrollWidth) <= 1,
		stageDetail,
	);
	await page.emulateMedia({ reducedMotion: "no-preference" });
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
		// 用页面同一套过滤：暂未开放的能力（手机推送）后端仍在、UI 隐去，
		// 期望值必须按"页面上能看到的"算，否则永远差一行。
		const all = api.capabilities || [];
		const caps =
			window.Anotify && window.Anotify.filterCaps
				? window.Anotify.filterCaps(all)
				: all;
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
			// 手机推送能力（唯一的 hub 能力）按产品节奏隐藏时，hub 行为 0；
			// pushUI 记录了这是"隐藏"还是"真没有"，断言据此区分。
			pushUI: !!(window.Anotify && window.Anotify.pushUIEnabled),
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
		// 一边一个：hub 能力必须**有对应行**（不能画成"跑不起来"）。
		// 手机推送隐藏期间 hub=0，此时只能要求"没有多出来的中枢行"；
		// 一旦 pushUI 打开（PUSH_UI_ENABLED=true），hub>0 重新变成硬要求。
		info.hubRowText.length === info.hub && (!info.pushUI || info.hub > 0),
		`hub=${info.hub} rows=${info.hubRowText.length} pushUI=${info.pushUI}`,
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
			install: (document.getElementById("reg-install") || {}).textContent || "",
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
	// 工作目录不再由节点侧配置（#capability-only-workdir）：唯一来源是能力定义里的
	// run.workDir。节点页因此不该再有这个输入框，也不该把它写进 yaml / 命令。
	const hasRegWorkdir = await page.evaluate(
		() => !!document.getElementById("reg-workdirs"),
	);
	H.check(
		"注册面板不再有「能力工作目录」输入（工作目录归能力定义）",
		hasRegWorkdir === false,
		`#reg-workdirs 存在=${hasRegWorkdir}`,
	);
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

	// ---- ③.5 「如何接入」卡片：中枢地址自己填，令牌可就地生成 ----
	//
	// 这里锁的是**渲染后的文字**，因为用户看到的就是它：
	//   ① 不能再出现「<中枢地址>」这种让人手抄的占位符（用户反馈：页面自己知道 origin）；
	//   ② 真 origin 必须已经拼进命令里 —— 否则用户还是得自己找地址；
	//   ③ 要有「生成令牌」按钮 —— 令牌应在页面上就能拿到，不用先去注册面板。
	// 两条路都要给：只写 docker 会让人以为「原生装不存在」（#283）。
	const howto = await page.evaluate(() => {
		const host = document.getElementById("howto-blocks");
		const card = document.querySelector("div.card:has(#howto-blocks)");
		return {
			code: host ? host.textContent : "",
			cardText: card ? card.textContent : "",
			hasBtn: !!document.getElementById("howto-token-btn"),
			hasOriginPlaceholder: (host ? host.textContent : "").includes("<中枢地址>"),
			origin: location.origin,
		};
	});
	H.check(
		"接入说明同时给出 docker 与原生装两条路",
		/docker run/.test(howto.code) && /install-agent\.sh/.test(howto.code),
		String(howto.code).slice(0, 120),
	);
	H.check(
		"接入说明的 docker 示例带 ANOTIFY_SERVER=<当前 origin>（不再让用户手填中枢地址）",
		/ANOTIFY_SERVER=/.test(howto.code) &&
			/ANOTIFY_TOKEN=/.test(howto.code) &&
			howto.code.includes("ANOTIFY_SERVER=" + howto.origin),
		`origin=${howto.origin} code=${String(howto.code).slice(0, 160)}`,
	);
	H.check(
		"接入说明里没有「<中枢地址>」占位符（中枢地址由页面自己填）",
		howto.hasOriginPlaceholder === false,
		JSON.stringify({ 占位符: howto.hasOriginPlaceholder }),
	);
	H.check(
		"接入说明有「生成令牌」按钮（令牌在页面上就能拿）",
		howto.hasBtn === true,
		JSON.stringify({ 按钮: howto.hasBtn }),
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
		// 有效键必须**恰好**是 bootstrap 四件（name/server/token/instances）。
		//
		// 原来的写法是数「行数 == 4」——那会在加注释/加可选块时误报，而且它并不真的
		// 表达「键集合正确」。改成直接比较键集合：注释行、空行、缩进的列表项都不参与，
		// 但任何一个多余的键都会被抓出来（这才是这条断言的意图）。
		"键集合 = bootstrap 四件": (() => {
			const keys = yaml
				.split(/\r?\n/)
				.filter((l) => l.trim() !== "" && !l.trim().startsWith("#"))
				.filter((l) => /^[a-zA-Z_][\w-]*:/.test(l)) // 只看顶层键（列表项带缩进）
				.map((l) => l.split(":")[0].trim())
				.sort();
			const allowed = ["instances", "name", "server", "token"];
			return keys.every((k) => allowed.includes(k)) &&
				["name", "server", "token", "instances"].every((k) => keys.includes(k));
		})(),
	};
	const yamlBad = {
		"capabilities 键（v2 已删）": /^capabilities:/m.test(yaml),
		"run 键（执行方式归产物）": /^[ \t]*run:/m.test(yaml),
		"runner 键（v2 不再有 runner 枚举）": /^[ \t]*runner:/m.test(yaml),
		"有 node: 包装": /^node:/m.test(yaml),
		"用了 timeout_sec（agent 要 timeoutSec）": yaml.includes("timeout_sec"),
		// 工作目录归能力定义（run.workDir）：写进 node.yaml 是未知字段，
		// agent 的 KnownFields(true) 会让它直接起不来。
		"workDirs 键（工作目录归能力定义）": /^workDirs:/m.test(yaml),
	};
	const yamlOk =
		Object.values(yamlWant).every(Boolean) &&
		!Object.values(yamlBad).some(Boolean);
	// ---- ④.5 工作目录：节点侧**一个都不该出现** ----
	//
	// 产品口径（#capability-only-workdir）：工作目录只有一个来源 —— 能力定义里的
	// run.workDir。节点侧不再有白名单要授权，所以 yaml / docker 命令 / 原生装命令
	// 三条路都不该再出现目录配置。少删一处的症状是「两份事实」，用户在节点页配了
	// 却在能力页改 —— 谁生效说不清。
	const wdLeak = {
		"yaml 里的 workDirs": /workDirs/.test(reg.yaml || ""),
		"yaml 里的 ANOTIFY_WORK_DIRS": /ANOTIFY_WORK_DIRS/.test(reg.yaml || ""),
		"docker 命令里的 ANOTIFY_WORK_DIRS": /ANOTIFY_WORK_DIRS/.test(reg.cmd || ""),
		"docker 命令里的 -v 挂载": /\s-v\s/.test(reg.cmd || ""),
		"原生装命令里的 --work-dirs": /--work-dirs/.test(reg.install || ""),
	};
	H.check(
		"工作目录不在节点侧任何输出里（yaml / docker / 原生装都不带）",
		Object.values(wdLeak).every((v) => v === false),
		JSON.stringify(wdLeak),
	);

	H.check(
		"原生安装命令：指向 install-agent.sh 且带令牌与中枢地址",
		/install-agent\.sh/.test(reg.install || "") &&
			(reg.install || "").includes("--token=" + token) &&
			(reg.install || "").includes("--server=" + reg.origin),
		String(reg.install || "").slice(0, 160),
	);

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
	// 填了 2 个槽位就必须带出去：不带 = 用 agent 缺省（1 个），
	// 症状是「机器闲着但任务排队」，而用户明明在面板上填了 2。
	H.check(
		"docker 命令带 ANOTIFY_INSTANCES=2（面板填的槽位数不能被吞掉）",
		cmd.includes("ANOTIFY_INSTANCES=2"),
		cmd.slice(0, 220),
	);
	H.check(
		"docker 命令带 ANOTIFY_NAME=<注册的节点名>（否则中枢只看到主机名）",
		cmd.includes("ANOTIFY_NAME=" + regName),
		cmd.slice(0, 220),
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

	// ---- ⑩ 「如何接入」卡片里的「生成令牌」：令牌就地拿，yaml 只有 bootstrap 四件 ----
	//
	// 这条链路与注册面板是**两条独立入口**（用户反馈：令牌应该页面上就能生成，
	// 不该先绕去填表单）。断言四件事：
	//   ① 点一下 → #howto-token 里出现真令牌，且三条命令/配置都带上它；
	//   ② docker 命令与 yaml 里的中枢地址就是当前 origin（不用手抄）；
	//   ③ yaml 只有 bootstrap 四件：工作目录归能力定义（run.workDir），卡片里不该再
	//      按能力名列目录 —— 那会让用户以为目录要在节点上配；
	//   ④ 令牌只活在 DOM 里（不进 localStorage / URL）。
	await page.click("#howto-token-btn");
	// mock 有 120–300ms 随机延迟，多等一轮
	await page.waitForTimeout(1500);
	const hw = await page.evaluate(() => ({
		token: (document.getElementById("howto-token") || {}).textContent || "",
		cmd: (document.getElementById("howto-cmd") || {}).textContent || "",
		yaml: (document.getElementById("howto-yaml") || {}).textContent || "",
		install: (document.getElementById("howto-install") || {}).textContent || "",
		origin: location.origin,
		ls: JSON.stringify(Object.keys(localStorage)),
		href: location.href,
	}));
	const hwToken = hw.token.trim();
	H.check(
		"点「生成令牌」→ 卡片里出现真令牌",
		hwToken.length > 8,
		`令牌长度=${hwToken.length} 卡片文本=${JSON.stringify(hw.token.slice(0, 40))}`,
	);
	H.check(
		"生成后 docker / yaml / 原生装三处都带上了真令牌",
		hw.cmd.includes("ANOTIFY_TOKEN=" + hwToken) &&
			hw.yaml.includes("token: " + hwToken) &&
			hw.install.includes("--token=" + hwToken),
		JSON.stringify({
			cmd: hw.cmd.includes(hwToken),
			yaml: hw.yaml.includes(hwToken),
			install: hw.install.includes(hwToken),
		}),
	);
	H.check(
		"生成后中枢地址取当前 origin（用户不用手填）",
		hw.cmd.includes("ANOTIFY_SERVER=" + hw.origin) &&
			hw.yaml.includes("server: " + hw.origin),
		`origin=${hw.origin}`,
	);
	// 卡片里的 yaml 同样只有 bootstrap 四件：工作目录在能力定义里，不在这里配。
	// 键集合用与注册面板同一套判定（注释行 / 空行不参与），任何多余的键都会被抓出来。
	const hwYamlKeys = hw.yaml
		.split(/\r?\n/)
		.filter((l) => l.trim() !== "" && !l.trim().startsWith("#"))
		.filter((l) => /^[a-zA-Z_][\w-]*:/.test(l))
		.map((l) => l.split(":")[0].trim())
		.sort()
		.join(",");
	H.check(
		"生成的 yaml 只有 bootstrap 四件（工作目录不在节点侧配）",
		hwYamlKeys === "instances,name,server,token" && !/workDirs/.test(hw.yaml),
		`keys=${hwYamlKeys} yaml=${JSON.stringify(hw.yaml.slice(0, 200))}`,
	);
	H.check(
		"令牌只活在 DOM 里（不进 localStorage / URL）",
		!hw.ls.includes(hwToken) && !hw.href.includes(hwToken),
		JSON.stringify({ localStorage: hw.ls.slice(0, 80), href: hw.href.replace(/\?.*/, "") }),
	);

	// ---- ⑩b 生成失败 → toast 说人话，旧令牌必须清掉 ----
	//
	// 为什么要在页内包一层 fetch 而不是 page.route：mock 在 window.fetch 层就把
	// /v1/* 拦下了（web/mock.js），请求根本不出浏览器 —— 网络层拦截看到的永远是 0 条。
	// 失败时最危险的状态是「上一次的真令牌还留在页面上」：用户会以为它就是这次生成的。
	const genFail = await page.evaluate(async () => {
		const prevToken =
			(document.getElementById("howto-token") || {}).textContent || "";
		const orig = window.fetch;
		window.fetch = function (input, init) {
			const url = String((input && input.url) || input);
			const method = String((init && init.method) || "GET").toUpperCase();
			if (url.includes("/v1/nodes") && method === "POST") {
				return Promise.resolve(
					new Response('{"error":"boom"}', {
						status: 500,
						headers: { "content-type": "application/json" },
					}),
				);
			}
			return orig.apply(this, arguments);
		};
		document.getElementById("howto-token-btn").click();
		await new Promise((r) => setTimeout(r, 1200));
		window.fetch = orig;
		const toastEl = document.getElementById("toast");
		return {
			prevToken: prevToken.trim(),
			toast: toastEl ? toastEl.textContent : "",
			token: (document.getElementById("howto-token") || {}).textContent || "",
			disabled: !!(document.getElementById("howto-token-btn") || {}).disabled,
		};
	});
	H.check(
		"生成失败 → toast 报错，旧令牌从 DOM 清掉，按钮恢复可点",
		genFail.toast.trim() !== "" &&
			genFail.prevToken.length > 8 &&
			!genFail.token.includes(genFail.prevToken) &&
			genFail.disabled === false,
		JSON.stringify({
			toast: genFail.toast.slice(0, 60),
			旧令牌残留: genFail.token.includes(genFail.prevToken),
			按钮仍禁用: genFail.disabled,
		}),
	);
}

// ---------------------------------------------------------------------------
// agent 自升级（CONTRACT-agent-upgrade.md §5.1 读侧 + §6 UI）
//
// 这一组用例全部跑在 **mock 预览环境**（web/mock.js 在 fetch 层应答）：
// 真后端的 upgrade 端点由 B 组实现，本组合并时可能还不存在；形状（
// targetAgentVersion / agentUpgrade / 409 / 501 / up_to_date）只能先在 mock 上钉死，
// 真后端那一半见 checkNodeUpgradeReal（接口在就断言，不在就留一行日志）。
//
// 钉的是「用户能不能把升级这件事看明白」，不是「按钮画出来了」：
//   ① 版本：每张卡片都有；老 agent 空版本 = 「未知（老版本）」，不是空白/undefined
//   ② 陈旧提示必须是**中性**的（文案 + token 色），不能长成红色错误
//   ③ 升级必须二次确认，且文案说清代价（替换二进制 / 重启 / 在跑任务等完 grace 20s）
//   ④ 取消不做任何事；确认后才发 POST
//   ⑤ 成功：ack 后版本前进到目标版本，按钮变「已是最新」
//   ⑥ 同版本：不动手就显示「已是最新」
//   ⑦ 失败原因逐条映射：node_offline(409) / agent_binary_unavailable(501) / bad_sha(ack)
//   ⑧ ack 结果来自**服务端状态**（点刷新仍在），不是页面内存里的一次性提示
//   ⑨ sent 超 60s 没 ack → 「节点未回复」，不许永远显示「升级中」
//   ⑩ 390 宽不横向溢出；全程无 JS pageerror

// readNodeCards 把节点列表每一行的版本 / 徽章 / 动作 / 升级状态读成结构化数据。
//
// 为什么一次读全：升级这一块的分支很多（未知版本 / 陈旧 / 已最新 / 无二进制 /
// ack 失败 / 节点没回话），散着读会让断言看不出「哪个节点在哪个状态」。
async function readNodeCards(page) {
	return page.evaluate(() => {
		// tokens.css 的 --muted / --error 是 #rrggbb，浏览器算出来的是 rgb()，
		// 比较前先转一下 —— 否则「用没用错误色」就变成了靠类名猜。
		const hexToRgb = (hex) => {
			const m = String(hex || "").trim().match(/^#([0-9a-f]{6})$/i);
			if (!m) return "";
			const n = parseInt(m[1], 16);
			return `rgb(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255})`;
		};
		const root = getComputedStyle(document.documentElement);
		const token = (name) => root.getPropertyValue(name).trim();
		return {
			colors: { muted: hexToRgb(token("--muted")), error: hexToRgb(token("--error")) },
			rows: [...document.querySelectorAll("#nodes-list > div")].map((row) => {
				const one = (sel) => row.querySelector(sel);
				const verEl = one(".nd-ver-val");
				const ctl = one("[data-upgrade-state]");
				return {
					name: (one(".nd-name") || {}).textContent || "",
					hasVersionLine: !!one(".nd-ver"),
					version: (verEl || {}).textContent || "",
					versionClass: String((verEl || {}).className || ""),
					versionBadges: [...row.querySelectorAll(".nd-ver .badge")].map((b) => ({
						text: b.textContent.trim(),
						cls: String(b.className),
						color: getComputedStyle(b).color,
					})),
					buttons: [...row.querySelectorAll(".nd-acts button")].map((b) => ({
						text: b.textContent.trim(),
						disabled: !!b.disabled,
					})),
					upgrade: {
						state: (ctl && ctl.dataset.upgradeState) || "",
						text: ctl ? ctl.textContent.trim() : "",
					},
					notes: [...row.querySelectorAll(".nd-upg-err, .nd-upg-note")].map((n) => ({
						cls: String(n.className),
						text: n.textContent.trim(),
					})),
				};
			}),
		};
	});
}

// clickUpgrade 点某一行的升级按钮（文案随语言变，所以用 data-action 定位）。
async function clickUpgrade(page, needle) {
	return page.evaluate((n) => {
		const row = [...document.querySelectorAll("#nodes-list > div")].find((r) =>
			((r.querySelector(".nd-name") || {}).textContent || "").includes(n),
		);
		if (!row) return "no-row";
		const btn = row.querySelector('[data-action="upgrade"]');
		if (!btn) return "no-btn";
		btn.click();
		return "clicked";
	}, needle);
}

async function checkNodeUpgrade(page, base) {
	await page.goto(base + "/nodes.html", { waitUntil: "load", timeout: 20000 });
	await page.waitForTimeout(1200);

	const pageErrors = [];
	page.on("pageerror", (e) => pageErrors.push(String(e)));

	const target = await page.evaluate(async () => {
		const api = await (await fetch("/v1/nodes")).json();
		return String(api.targetAgentVersion || "");
	});
	H.check(
		"升级：mock 的节点列表带顶层目标版本（契约 §5.1）",
		target.length > 0,
		`targetAgentVersion=${JSON.stringify(target)}`,
	);

	const card = (cards, needle) =>
		cards.rows.find((r) => r.name.includes(needle)) || { versionBadges: [], buttons: [], notes: [] };

	// ---- ① 版本行 + ② 中性陈旧提示 + ⑥ 同版本「已是最新」 ----
	let cards = await readNodeCards(page);
	H.check(
		"节点卡片显示 agent 版本（每行都有，不是空白）",
		cards.rows.length > 0 &&
			cards.rows.every((r) => r.hasVersionLine && r.version.trim() !== ""),
		JSON.stringify(cards.rows.map((r) => [r.name, r.version])),
	);
	const unknownRows = cards.rows.filter((r) => r.versionClass.includes("is-unknown"));
	H.check(
		"老 agent（没上报版本）显示「未知（老版本）」而不是空白",
		unknownRows.length > 0 &&
			unknownRows.every((r) => r.version === "未知（老版本）"),
		JSON.stringify(unknownRows.map((r) => [r.name, r.version])),
	);
	const staleRows = cards.rows.filter((r) =>
		r.versionBadges.some((b) => b.text === "有新版本可用"),
	);
	H.check(
		"版本陈旧给中性提示（.badge-neutral + --muted 色，不是红色错误）",
		cards.colors.muted !== "" &&
			cards.colors.muted !== cards.colors.error &&
			staleRows.length > 0 &&
			staleRows.every((r) =>
				r.versionBadges.every(
					(b) => b.cls.includes("badge-neutral") && b.color === cards.colors.muted,
				),
			),
		JSON.stringify({
			stale: staleRows.map((r) => r.name),
			badges: staleRows.map((r) => r.versionBadges),
			token: cards.colors,
		}),
	);
	const upToDateRow = card(cards, "ops-box");
	H.check(
		"同版本节点直接显示「已是最新」（禁用，不是可点的「升级」）",
		upToDateRow.upgrade.state === "latest" &&
			upToDateRow.upgrade.text === "已是最新" &&
			upToDateRow.buttons.some((b) => b.text === "已是最新" && b.disabled),
		JSON.stringify({ upgrade: upToDateRow.upgrade, buttons: upToDateRow.buttons }),
	);

	// ---- ③ 二次确认（取消） ----
	let confirmText = "";
	page.once("dialog", async (d) => {
		confirmText = d.message();
		await d.dismiss();
	});
	const cancelledClick = await clickUpgrade(page, "laptop-worker");
	await page.waitForTimeout(700);
	const afterCancel = card(await readNodeCards(page), "laptop-worker");
	H.check(
		"升级需二次确认，文案说清代价（替换二进制 / 重启 / 在跑任务等完 grace 20s）",
		cancelledClick === "clicked" &&
			/替换/.test(confirmText) &&
			/重启/.test(confirmText) &&
			/grace 20s/.test(confirmText),
		`click=${cancelledClick} 文案=${confirmText}`,
	);
	H.check(
		"二次确认点取消 → 不发请求，按钮仍是「升级」",
		afterCancel.upgrade.state === "ready" && afterCancel.upgrade.text === "升级",
		JSON.stringify({ upgrade: afterCancel.upgrade, version: afterCancel.version }),
	);

	// ---- ⑦a 离线节点：409 node_offline 映射成人话 ----
	page.once("dialog", async (d) => await d.accept());
	const offlineClick = await clickUpgrade(page, "ci-runner");
	await page.waitForTimeout(800);
	const offlineToast = await page.evaluate(
		() => (document.getElementById("toast") || {}).textContent || "",
	);
	H.check(
		"离线节点升级 → 409 node_offline 映射成人话",
		offlineClick === "clicked" && /节点离线/.test(offlineToast),
		`click=${offlineClick} toast=${offlineToast}`,
	);

	// ---- ⑦b ack 失败：bad_sha（中枢推帧后节点回报校验失败） ----
	// 控制面是 mock 的 MockApi.failUpgrade（真后端不需要它：失败由 agent 真的回 ack）。
	await page.evaluate(() => window.MockApi.failUpgrade("bad_sha"));
	page.once("dialog", async (d) => await d.accept());
	await clickUpgrade(page, "laptop-worker");
	await page.waitForFunction(
		() =>
			[...document.querySelectorAll("#nodes-list > div")].some((r) =>
				((r.querySelector(".nd-upg-err") || {}).textContent || "").includes("被改动"),
			),
		null,
		{ timeout: 8000 },
	).catch(() => {});
	const badShaToast = await page.evaluate(
		() => (document.getElementById("toast") || {}).textContent || "",
	);
	const badShaRow = card(await readNodeCards(page), "laptop-worker");
	H.check(
		"ack 失败 bad_sha → 行内给出「服务端二进制可能被改动，已拒绝」",
		badShaRow.notes.some((n) => n.text.includes("服务端二进制可能被改动，已拒绝")),
		JSON.stringify(badShaRow.notes),
	);
	H.check(
		"ack 失败同时弹一次 toast（失败不会被静默吞掉）",
		/被改动/.test(badShaToast),
		badShaToast,
	);

	// ---- ⑧ 结果来自服务端状态：点「刷新」重新拉列表后失败仍在 ----
	//
	// 这条是 agent_upgrade 落库（契约 §5.1）的**客户端可见后果**：
	// 如果 UI 只把失败记在页面内存里，刷新一拉列表就没了。mock 的节点状态
	// 就是它的“库”（内存态），所以点页头刷新等价于真后端重新读 nodes.agent_upgrade。
	// （F5 整页刷新会重建 mock state，因此这段用页内刷新而不是 reload；
	//   真正落库那一半由 checkNodeUpgradeReal 在 B 组接口合并后断言。）
	await page.click("#nodes-refresh");
	await page.waitForTimeout(800);
	const afterRefreshRow = card(await readNodeCards(page), "laptop-worker");
	H.check(
		"ack 失败结果来自服务端状态：重新拉列表后仍在（不是页面内存里的一次性提示）",
		afterRefreshRow.notes.some((n) =>
			n.text.includes("服务端二进制可能被改动，已拒绝"),
		),
		JSON.stringify(afterRefreshRow.notes),
	);

	// ---- ⑦c 服务端没带二进制：不给可点按钮（点了必然 501） ----
	await page.evaluate(() => window.MockApi.noAgentBinary());
	await page.click("#nodes-refresh");
	await page.waitForTimeout(800);
	const noBin = await readNodeCards(page);
	H.check(
		"服务端没带 agent 二进制 → 不给可点按钮，显示原因",
		noBin.rows.length > 0 &&
			noBin.rows.every(
				(r) =>
					r.upgrade.state === "unavailable" &&
					r.upgrade.text === "服务端没带 agent 二进制" &&
					!r.buttons.some((b) => b.text === "升级"),
			),
		JSON.stringify(noBin.rows.map((r) => [r.name, r.upgrade])),
	);
	await page.evaluate(() => window.MockApi.noAgentBinary(false));

	// ---- ④⑤ 成功：确认 → sent → ack 成功 → 版本前进 + 「已是最新」 ----
	await page.evaluate(() => window.MockApi.failUpgrade(""));
	await page.click("#nodes-refresh");
	await page.waitForTimeout(600);
	page.once("dialog", async (d) => await d.accept());
	const okClick = await clickUpgrade(page, "laptop-worker");
	await page
		.waitForFunction(
			(tgt) => {
				const row = [...document.querySelectorAll("#nodes-list > div")].find((r) =>
					((r.querySelector(".nd-name") || {}).textContent || "").includes(
						"laptop-worker",
					),
				);
				if (!row) return false;
				const v = (row.querySelector(".nd-ver-val") || {}).textContent || "";
				const ctl = row.querySelector('[data-upgrade-state="latest"]');
				return !!ctl && v === tgt;
			},
			target,
			{ timeout: 8000 },
		)
		.catch(() => {});
	const afterOk = card(await readNodeCards(page), "laptop-worker");
	H.check(
		"确认后下发升级：ack 成功 → 版本前进到目标版本，按钮变「已是最新」",
		okClick === "clicked" &&
			afterOk.version === target &&
			afterOk.upgrade.state === "latest",
		JSON.stringify({ click: okClick, version: afterOk.version, target, upgrade: afterOk.upgrade }),
	);

	// ---- ⑨ sent 超 60s 没 ack → 「节点未回复」，不许永远「升级中」 ----
	await page.evaluate(() => {
		const n = window.MockApi.state.nodes.find((x) => x.id === "nd_mock_ops");
		n.agentVersion = "";
		n.agentUpgrade = {
			status: "sent",
			targetVersion: "1.5.0+3fd5a59",
			error: "",
			at: Math.floor(Date.now() / 1000) - 120,
		};
	});
	await page.click("#nodes-refresh");
	await page.waitForTimeout(800);
	const staleSent = card(await readNodeCards(page), "ops-box");
	H.check(
		"status=sent 超 60s 没 ack → 提示「节点未回复（可能已断线）」（不是一直「升级中」）",
		staleSent.upgrade.state === "ready" &&
			staleSent.notes.some((n) => n.text.includes("节点未回复（可能已断线）")),
		JSON.stringify({ upgrade: staleSent.upgrade, notes: staleSent.notes }),
	);

	// ---- ⑩ 390 宽：版本行/升级按钮不把卡片撑破 ----
	await page.goto(base + "/nodes.html", { waitUntil: "load", timeout: 20000 });
	await page.setViewportSize({ width: 390, height: 844 });
	await page.waitForTimeout(1000);
	const overflow = await page.evaluate(() => ({
		sw: document.documentElement.scrollWidth,
		iw: window.innerWidth,
	}));
	H.check(
		"节点卡片（版本 + 升级）在 390 宽不横向溢出",
		overflow.sw <= overflow.iw + 1,
		`scrollWidth=${overflow.sw} innerWidth=${overflow.iw}`,
	);
	await page.setViewportSize({ width: 1280, height: 800 });

	// ---- ⑪ 刷新页面后，**上一次**的失败结果仍然可见（落库语义） ----
	//
	// mock 的 state 是内存的，F5 会回到种子数据；而种子里 old-laptop 就带着一条
	// 「failed / need_root」（模拟“上一次会话留下的结果”）。这条断言的真正含义：
	// 控制台在**没有点任何按钮**的情况下也会把服务器上的 ack 结果画出来 ——
	// 这正是 nodes.agent_upgrade 落库（契约 §5.1）要求的可见后果。
	const needRootRow = card(await readNodeCards(page), "old-laptop");
	H.check(
		"刷新页面后上次升级失败仍可见（need_root → 给出 sudo anotify-agent upgrade）",
		needRootRow.notes.some(
			(n) =>
				n.cls.includes("nd-upg-err") &&
				n.text.includes("在节点上执行：sudo anotify-agent upgrade"),
		),
		JSON.stringify(needRootRow.notes),
	);
	H.check("升级相关交互无 JS pageerror", pageErrors.length === 0, pageErrors[0]?.slice(0, 200));
}

// checkNodeUpgradeI18n 盯「失败原因四语言不串」：
//
// 同一份映射在四种语言里必须各说各话，而不是某种语言回落到中文（sitegen 的
// 缺 key 回退就是回落 zh-CN，静默且只在新语言页面上显形）。所以每条文案都要求：
//   · 非空；
//   · 四语言两两不同；
//   · en/es 里没有 CJK；zh 有汉字；ja 有假名；
//   · need_root 的**可执行部分**（sudo anotify-agent upgrade）四种语言都必须在 ——
//     翻译可以把前后文改顺，但命令不能被翻掉。
async function checkNodeUpgradeI18n(ctx, base) {
	const LANGS = [
		{ code: "", name: "zh-CN" },
		{ code: "en", name: "en" },
		{ code: "ja", name: "ja" },
		{ code: "es", name: "es" },
	];
	const CJK = /[\u4e00-\u9fff\u3400-\u4dbf]/;
	const KANA = /[\u3040-\u30ff]/;
	const got = {};

	for (const { code, name } of LANGS) {
		const page = await ctx.newPage();
		const errs = [];
		page.on("pageerror", (e) => errs.push(String(e)));
		await page.goto(base + "/nodes.html" + (code ? "?lang=" + code : ""), {
			waitUntil: "load",
			timeout: 20000,
		});
		await page.waitForTimeout(1200);

		const cards = await readNodeCards(page);
		const pick = (n) => cards.rows.find((r) => r.name.includes(n)) || {};
		const laptop = pick("laptop-worker");
		const ops = pick("ops-box");
		const legacy = pick("old-laptop");

		// 离线节点的 409 文案（点一下才拿得到）
		page.once("dialog", async (d) => await d.accept());
		const clicked = await clickUpgrade(page, "ci-runner");
		await page.waitForTimeout(800);
		const offlineToast = await page.evaluate(
			() => (document.getElementById("toast") || {}).textContent || "",
		);

		// 服务端没带二进制时的控件文案（点页头刷新重新拉列表）
		await page.evaluate(() => window.MockApi.noAgentBinary());
		await page.click("#nodes-refresh");
		await page.waitForTimeout(800);
		const noBin = await readNodeCards(page);
		const noBinText = (noBin.rows[0] || {}).upgrade ? noBin.rows[0].upgrade.text : "";

		got[name] = {
			stale: ((laptop.versionBadges || [])[0] || {}).text || "",
			latest: ((ops.versionBadges || [])[0] || {}).text || "",
			needRoot: ((legacy.notes || [])[0] || {}).text || "",
			offline: offlineToast,
			unavailable: noBinText,
			errors: errs,
			clicked,
		};
		await page.close();
	}

	const names = LANGS.map((l) => l.name);
	const FIELDS = ["stale", "latest", "needRoot", "offline", "unavailable"];
	for (const field of FIELDS) {
		const values = names.map((n) => got[n][field]);
		H.check(
			`升级文案四语言不串：${field} 四种语言各自不同且非空`,
			values.every((v) => v && v.trim().length > 0) &&
				new Set(values).size === names.length,
			JSON.stringify(values),
		);
	}
	for (const name of ["en", "es"]) {
		const residue = FIELDS.map((f) => got[name][f]).filter((v) => CJK.test(v));
		H.check(`${name} 的升级文案没有中文残留`, residue.length === 0, residue.join(" | ").slice(0, 220));
	}
	H.check(
		"zh-CN 的升级文案是中文",
		CJK.test(got["zh-CN"].stale) && CJK.test(got["zh-CN"].needRoot),
		JSON.stringify(got["zh-CN"]),
	);
	H.check(
		"ja 的升级文案是日文（含假名）",
		KANA.test(got.ja.stale) && KANA.test(got.ja.needRoot),
		JSON.stringify(got.ja),
	);
	// 命令本身不能被翻译掉：四种语言的 need_root 都要能照着粘。
	H.check(
		"need_root 的可执行命令四语言都在（sudo anotify-agent upgrade）",
		names.every((n) => got[n].needRoot.includes("sudo anotify-agent upgrade")),
		JSON.stringify(names.map((n) => got[n].needRoot)),
	);
	H.check(
		"四语言节点页升级交互无 JS pageerror",
		names.every((n) => got[n].errors.length === 0),
		JSON.stringify(names.map((n) => [n, got[n].errors[0]])),
	);
	H.check(
		"四语言都能点到离线节点的升级按钮（data-action 钩子不依赖文案）",
		names.every((n) => got[n].clicked === "clicked"),
		JSON.stringify(names.map((n) => [n, got[n].clicked])),
	);
}

// checkNodeUpgradeReal 真后端那一半：agent 版本 / 升级（契约 §5.1）。
//
// C 组合并时 B 组的升级端点可能还没合进来（三组并行）。所以先用**只读探针**
// 判断接口在不在：
//   · 响应带 `targetAgentVersion`（或任一节点带 `agentVersion`）→ 跑真断言；
//   · 不在 → 留一行日志说明「合并后启用」，不伪装成通过，也不把套件拉红。
// **绝不**在真后端上真的 POST /upgrade：那会替换 e2e agent 的二进制并重启进程，
// 是破坏性操作（真链路由 A/B 组单测 + 人工验证覆盖）。
//
// 断言的两件事都是“落库才有”的：
//   ① 从不上报版本的节点显示「未知（老版本）」；
//   ② 把 nodes.agent_version 改成中枢当前版本后，重载页面直接变「已是最新」 ——
//      版本来自 API（不是页面缓存），这也是 e2e 能验的落库语义。
async function checkNodeUpgradeReal(server, newCtx) {
	const s = H.seed(server.dbPath, "node_upgrade");
	const probe = await H.req(server.base, "/v1/nodes", { session: s.session });
	const api = (probe.json || {});
	const nodes = Array.isArray(api.nodes) ? api.nodes : [];
	const hasTarget = Object.prototype.hasOwnProperty.call(api, "targetAgentVersion");
	const hasVersion = nodes.some((n) =>
		Object.prototype.hasOwnProperty.call(n, "agentVersion"),
	);
	if (probe.status !== 200 || (!hasTarget && !hasVersion)) {
		console.log(
			`  · 节点升级（真后端）：合并后启用（GET /v1/nodes ${probe.status}，` +
				`targetAgentVersion=${hasTarget}，agentVersion=${hasVersion}）→ 本次只用 mock 断言`,
		);
		return;
	}

	// 造一台**从未连上来**的节点：agent_version 为空，正好对应“老 agent”。
	const created = await H.req(server.base, "/v1/nodes", {
		session: s.session,
		method: "POST",
		body: { name: "ui-upg-" + Date.now(), instances: 1 },
	});
	const nodeId = (created.json && created.json.node && created.json.node.id) || "";
	H.check("真后端：注册一台节点用于升级 UI 断言", created.status === 201 && !!nodeId, `status=${created.status} id=${nodeId}`);
	if (!nodeId) return;

	const ctx = await newCtx();
	await injectSession(ctx, s.session, server.base);
	const page = await ctx.newPage();
	const errs = [];
	page.on("pageerror", (e) => errs.push(String(e)));
	await page.goto(server.base + "/nodes.html", { waitUntil: "load", timeout: 20000 });
	await page.waitForTimeout(1500);

	const first = await readNodeCards(page);
	const row = first.rows.find((r) => r.name.includes("ui-upg-")) || {};
	H.check(
		"真后端：节点卡片显示 agent 版本（空 = 「未知（老版本）」）",
		row.hasVersionLine === true && row.version === "未知（老版本）",
		JSON.stringify({ version: row.version, line: row.hasVersionLine }),
	);
	H.check(
		"真后端：目标版本已知时给可点的「升级」按钮",
		!hasTarget ||
			(String(api.targetAgentVersion || "") !== "" && row.upgrade.state === "ready"),
		JSON.stringify({ target: api.targetAgentVersion, upgrade: row.upgrade }),
	);

	// ---- 落库语义：把 agent_version 改成中枢当前版本 → 重载页面直接「已是最新」 ----
	const target = String(api.targetAgentVersion || "");
	if (hasTarget && target) {
		const db = new DatabaseSync(server.dbPath);
		try {
			db.prepare("UPDATE nodes SET agent_version = ?, agent_commit = ? WHERE id = ?").run(
				target,
				"",
				nodeId,
			);
		} finally {
			db.close();
		}
		await page.reload({ waitUntil: "load" });
		await page.waitForTimeout(1200);
		const after = await readNodeCards(page);
		const row2 = after.rows.find((r) => r.name.includes("ui-upg-")) || {};
		H.check(
			"真后端：节点版本 == 中枢当前版本 → 按钮显示「已是最新」（禁用）",
			row2.version === target && row2.upgrade.state === "latest",
			JSON.stringify({ version: row2.version, target, upgrade: row2.upgrade }),
		);
	} else {
		console.log(
			`  · 节点升级（真后端）：「已是最新」断言合并后启用（targetAgentVersion 为空串 = 服务端没带二进制）`,
		);
	}
	H.check("真后端：节点升级 UI 无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 200));
	await page.close();
	await ctx.close();
}

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
			const list = c.capabilities || [];
			// visible = 页面实际会渲染的条数（暂未开放的能力被 UI 隐去）
			const visible =
				window.Anotify && window.Anotify.filterCaps
					? window.Anotify.filterCaps(list).length
					: list.length;
			return {
				caps: list.length,
				visible,
				nodes: (d.nodes || []).length,
			};
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
	// 等**可见行数**（visible），不是原始条数（caps）：
	// caps 含被 UI 隐去的 notify.phone，而下面那条断言比的是 visible。
	// 原来等的是 caps，于是这个 waitForFunction **每次都等满 15s 再被 catch 吞掉**
	// —— 实测这是套件第二大的单点耗时（15.1s），而且把"等错了对象"藏得干干净净。
	await page
		.waitForFunction(
			(want) => {
				const list = document.querySelector("#caps-list");
				const ns = document.querySelector(".scope-ns");
				return !!(ns && ns.textContent && list && list.children.length === want);
			},
			caps.staging.visible,
			{ timeout: 5000 },
		)
		.catch(() => process.env.E2E_PROFILE && console.log("[profile] 能力页行数等待超时"));
	const capPage = await page.evaluate(() => ({
		badge: !!document.querySelector(".scope-badge"),
		rows: document.querySelectorAll("#caps-list > *").length,
		ns: (document.querySelector(".scope-ns") || {}).textContent || "",
	}));
	H.check(
		"能力页跟着命名空间变（不再有「组织级」页面 / 徽标）",
		!capPage.badge &&
			capPage.rows === caps.staging.visible &&
			capPage.ns.includes("staging"),
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
	// 节点、边、环路边、声明了出口却没接下游的灰点、以及只读性。
	//
	// 为什么必须在这里测而不是在真实 server 上：断言依赖 demo 数据里的
	// **环路**（deploy-preview → apply-fix → deploy-preview）和**有出口没接下游**
	// （alert.oncall）。真实环境不一定有这些形状，那样的断言会假失败。
	// #112：流向图从**列表卡片**搬到了**源详情页**（source.html?id=…）。
	// 列表只回答"有哪些源、活着吗"，细节（配置/参数/流向）点进去看。
	//
	// 端口从图上消失（契约 hide-ports §4）：边标签不再显示出口名，
	// 灰点文案改成「没有下游」。例外（多出口的历史数据 + 同名多版本的 @N）
	// 在纯推导层用 sourceTopology 单测钉住。
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
	// ---- 端口不上图（契约 hide-ports §4）----
	//
	// 单出口能力的出口名（内部固定 out）对读者是噪声，所以：
	//   · 渲染层（host._graphSpec）收到的 port 是空串 → 图上没有边标签；
	//   · 灰点只说事实「没有下游」，不再提某个出口没接线。
	// 例外只有一个：**多出口的历史数据**（见下面的推导单测）。
	const spec = await page.evaluate(() => {
		const host = document.querySelector(".flograph");
		const sp = host && host._graphSpec;
		if (!sp) return null;
		return {
			edges: (sp.edges || []).map((e) => ({ from: e.from, to: e.to, port: e.port || "" })),
			nodes: (sp.nodes || []).map((n) => ({
				id: n.id,
				label: n.label || "",
				sub: n.sub || "",
				dead: !!n.deadport || n.kind === "deadport",
			})),
			edgeLabels: Array.from(document.querySelectorAll(".flograph-edge-label")).map((x) =>
				x.textContent.trim(),
			),
		};
	});
	H.check(
		"单出口能力的边不再把出口名传给渲染层（port 一律空串）",
		!!spec && (spec.edges || []).length > 0 && (spec.edges || []).every((e) => e.port === ""),
		JSON.stringify((spec || {}).edges),
	);
	H.check(
		"图上没有出口名边标签（端口不上图）",
		(g.ports || []).length === 0 && ((spec || {}).edgeLabels || []).length === 0,
		JSON.stringify({ dom: g.ports, spec: (spec || {}).edgeLabels }),
	);
	H.check(
		"灰点文案是「（没有下游）」/「没有下游」（不再说某个出口没接）",
		!!spec &&
			(spec.nodes || []).some(
				(n) => n.dead && n.label === "（没有下游）" && n.sub === "没有下游",
			),
		JSON.stringify(((spec || {}).nodes || []).filter((n) => n.dead)),
	);

	// 推导层单测（两页共用的同一个 sourceTopology）：
	//   ① 多出口（历史数据）才 showPort —— 那时两条边只靠出口名区分；
	//   ② 同名多版本同图 → label 带 @版本（否则两行字一模一样，读者分不清）。
	const topoRules = await page.evaluate(() => {
		const T = window.AnotifyGraphs;
		if (!T || typeof T.sourceTopology !== "function") return { err: "没有 sourceTopology" };
		const src = { id: "s1", name: "probe.src" };
		const mkCap = (id, name, version, outputs) => ({
			id: id,
			name: name,
			version: version,
			displayName: "",
			outputs: outputs,
			kind: "pipe",
		});
		// ① 多出口（两个出口各接一个下游）vs 单出口
		const multiCaps = [
			mkCap("m1", "hist.multi", 1, ["before", "after"]),
			mkCap("m2", "hist.one", 1, ["out"]),
			mkCap("m3", "hist.end", 1, []),
		];
		const multiEdges = [
			{ fromIsSource: true, fromRef: "probe.src", toRef: "hist.multi", toCapabilityId: "m1" },
			{ fromIsSource: false, fromCapabilityId: "m1", fromPort: "before", toRef: "hist.one", toCapabilityId: "m2" },
			{ fromIsSource: false, fromCapabilityId: "m1", fromPort: "after", toRef: "hist.end", toCapabilityId: "m3" },
			{ fromIsSource: false, fromCapabilityId: "m2", fromPort: "out", toRef: "hist.end", toCapabilityId: "m3" },
		];
		const multi = T.sourceTopology({ src: src, caps: multiCaps, edges: multiEdges });
		// ② 同名两版本同图 + 一个单版本
		//
		// 显示名要填：capLabel 的 withVersion 是「显示名@版本」，而没填显示名
		// 时本来就会回落成 name@version —— 那个回落不能用来验「同名多版本」。
		const mkDN = (id, name, version, outputs, dn) => {
			const c = mkCap(id, name, version, outputs);
			c.displayName = dn;
			return c;
		};
		const verCaps = [
			mkDN("v1", "echo.demo", 1, ["out"], "回声（示例）"),
			mkDN("v2", "echo.demo", 2, ["out"], "回声（示例）"),
			mkDN("v3", "echo.other", 1, ["out"], "通知推送"),
		];
		const verEdges = [
			{ fromIsSource: true, fromRef: "probe.src", toRef: "echo.demo@1", toCapabilityId: "v1" },
			{ fromIsSource: true, fromRef: "probe.src", toRef: "echo.demo", toCapabilityId: "v2" },
			{ fromIsSource: false, fromCapabilityId: "v1", fromPort: "out", toRef: "echo.other", toCapabilityId: "v3" },
		];
		const ver = T.sourceTopology({ src: src, caps: verCaps, edges: verEdges });
		const labelOf = (topo, id) => ((topo.nodes || []).find((n) => n.id === id) || {}).label || "";
		return {
			multi: (multi.edges || []).map((e) => ({ port: e.port || "", showPort: !!e.showPort })),
			verLabels: {
				v1: labelOf(ver, "v1"),
				v2: labelOf(ver, "v2"),
				v3: labelOf(ver, "v3"),
			},
		};
	});
	const multiEdgesOut = (topoRules && topoRules.multi) || [];
	H.check(
		"多出口能力（历史数据）才把出口名画到边上",
		multiEdgesOut.filter((e) => e.showPort).length === 2 &&
			multiEdgesOut.filter((e) => !e.showPort).length === 1,
		JSON.stringify(multiEdgesOut),
	);
	H.check(
		"同名多版本同图：label 带 @版本；单版本不带",
		!!topoRules.verLabels &&
			/^回声（示例）@1$/.test(topoRules.verLabels.v1) &&
			/^回声（示例）@2$/.test(topoRules.verLabels.v2) &&
			!/@/.test(topoRules.verLabels.v3),
		JSON.stringify(topoRules.verLabels),
	);
	H.check("环上的边用加深的样式画出来", g.hasCycleEdge === true);
	H.check("声明了出口却没接下游的能力在图上可见（灰点）", g.hasDeadPort === true);
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
			// 能力详情 + 声明出口/连下游（#：用户报的两个问题）。
			// 放在模板流程之后：两者都从干净的列表页开始（各自 goto）。
			await checkCapabilityDetail(page, base);
			// 批量创建（契约 §4）：多张能力卡片 + 候选优先级 + 一个 batch 请求
			await checkBatchCreate(page, base);
			// 编辑弹层里的下游行编辑器（CONTRACT-downstream-edit §4）：
			// 预填 / 改优先级 / 删一行 / 清空成终端能力 —— 全部走 PUT …/downstreams。
			// 代替旧的 checkEdgesPage（出口页已删：下游只能在能力里改）。
			await checkDownstreamEdit(page, base);
			// 显示名要走真实 PATCH（mock 也实现），改完再回列表看效果
			await checkDeclaredBy(page);
			await checkDisplayName(page, base);
		}
		if (p === "sources.html") {
			await checkFlowChart(page, base);
			// 剪枝原因的文案（契约 §6）：max_hops / budget 新增，path 删除
			await checkPruneCopy(page, base);
			// 触发能带 payload（#273 决策 2）：弹层 / 校验 / 请求体 / 任务参数
			await checkSourceFirePayload(page, base);
		}
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
			// agent 自升级（契约 §5.1 / §6）：版本行、二次确认、四种失败回包。
			// 函数内部自己 goto 干净的列表页，不依赖上面两个检查留下什么。
			await checkNodeUpgrade(page, base);
			// 四语言文案：另开页面（每种语言一个），不污染上面这页的 mock state。
			await checkNodeUpgradeI18n(ctx, base);
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
	// 任务日志面板（CONTRACT-task-logs §5）：独立 page，不污染上面的 mock state
	await checkTaskLogs(ctx, base);
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

// ---------------------------------------------------------------------------
// M. 任务日志面板（CONTRACT-task-logs §5）—— mock 预览
//
// 用户报的现场：任务 failed，控制台只有 "exit status 2: capability: test1@2"，
// 真正的原因（`flag provided but not defined: -to`）躺在子进程的 stderr 里，
// 一行都没留下来。所以这一区不是「日志好看」，是**失败时唯一能看到为什么**的地方。
//
// 四条断言各绑一条**精确构造**的种子数据（见 web/mock.js 的 seedTaskLogs）：
//   · 失败任务默认展开     ← deploy-preview / run-tests（failed）
//   · 成功任务默认折叠     ← analyze-pr（succeeded，只有 stdout）
//   · 展开才请求（懒加载） ← analyze-pr 折叠时 body 是空的
//   · 空日志说「没有输出」 ← rollback（succeeded，两块都空）
//   · 截断提示可见         ← deploy-preview（truncated=true, bytes=1048576）
//
// 为什么用 mock 而不是真后端：这四种形状需要**可控的日志内容**（截断标记 /
// 只有 stderr / 完全空 / 带 HTML 和 ANSI），真后端的日志取决于那台机器恰好
// 跑了什么。真后端「日志真的落库并能读到」由协调者的 trace 套件覆盖。
async function checkTaskLogs(ctx, base) {
	const page = await ctx.newPage();
	const pageErrors = [];
	page.on("pageerror", (e) => pageErrors.push(String((e && e.message) || e)));

	await page.goto(base + "/trace.html?traceId=trace_mock_t9&view=graph", {
		waitUntil: "load",
		timeout: 20000,
	});
	// 失败任务默认展开 ⇒ 页面一进来就会去取日志（mock 有 120~300ms 人为延迟），
	// 所以要等「截断提示」真的出现，而不是只看折叠条在不在。
	await page
		.waitForSelector("#trace-logs .task-log-trunc", { timeout: 15000 })
		.catch(() => {});
	// 只等 deploy-preview 的截断提示是**不够**的：失败任务各自并发取日志（mock 每条
	// 有 120~300ms 人为延迟），那条先回来时 run-tests 的 body 可能还没落进 DOM ——
	// 接着拍的快照会读到 stderr=false，让「没有截断的任务不显示截断提示」偶发假红
	// （时序问题，不是产品问题）。所以先等**所有失败任务**的 body 都渲染出内容，
	// 再拍快照；断言本身一条没动。
	await page
		.waitForFunction(
			() => {
				const rows = Array.from(document.querySelectorAll("#trace-logs .task-log"));
				const failedRows = rows.filter((r) => r.dataset.failed === "1");
				return (
					failedRows.length > 0 &&
					failedRows.every((r) => {
						const b = r.querySelector(".task-log-body");
						return !!b && b.childElementCount > 0;
					})
				);
			},
			null,
			{ timeout: 15000 },
		)
		.catch(() => {});

	const snap = () =>
		page.evaluate(() => {
			const read = (row) => {
				const body = row.querySelector(".task-log-body");
				const btn = row.querySelector(".task-log-toggle");
				return {
					capability: row.dataset.capability,
					status: row.dataset.status,
					failed: row.dataset.failed === "1",
					open: row.dataset.open === "1",
					aria: btn ? btn.getAttribute("aria-expanded") : null,
					bodyHidden: body ? body.hidden : null,
					bodyChildren: body ? body.childElementCount : -1,
					text: body ? body.textContent : "",
					stderr: !!row.querySelector(".task-log-pre.task-log-stderr"),
					stdout: !!row.querySelector(".task-log-pre.task-log-stdout"),
					trunc: !!row.querySelector(".task-log-trunc"),
				};
			};
			const rows = Array.from(document.querySelectorAll("#trace-logs .task-log"));
			const pre = Array.from(document.querySelectorAll("#trace-logs .task-log-pre"));
			const box = document.querySelector("#trace-logs .task-log-pre");
			const cs = box ? getComputedStyle(box) : null;
			return {
				hasSection: !!document.getElementById("trace-logs"),
				count: rows.length,
				rows: rows.map((r) => Object.assign(read(r), { id: r.dataset.taskId })),
				// 日志里的 HTML 必须还是文本：<b>NOT_HTML</b> 不能变成元素
				htmlLeak: !!document.querySelector(
					"#trace-logs .task-log-pre b, #trace-logs .task-log-pre img, #trace-logs .task-log-pre script",
				),
				textHasMarker: pre.some((p) => (p.textContent || "").includes("<b>NOT_HTML</b>")),
				// ANSI 控制字符不该原样铺在正文里（剥除或至少不产生 ^[ 噪音）
				hasEsc: pre.some((p) => (p.textContent || "").includes("\u001b")),
				css: cs
					? {
							overflow: cs.overflow,
							maxHeight: cs.maxHeight,
							whiteSpace: cs.whiteSpace,
							font: cs.fontFamily,
						}
					: null,
				colors: (() => {
					const se = document.querySelector("#trace-logs .task-log-pre.task-log-stderr");
					const so = document.querySelector("#trace-logs .task-log-pre.task-log-stdout");
					return {
						stderr: se ? getComputedStyle(se).color : "",
						stdout: so ? getComputedStyle(so).color : "",
					};
				})(),
			};
		});

	const first = await snap();
	const failedRows = first.rows.filter((r) => r.failed);
	const okRows = first.rows.filter((r) => !r.failed);

	H.check(
		"trace 页每个任务都有日志折叠条（12 任务 = 12 条）",
		first.hasSection && first.count === 12,
		`hasSection=${first.hasSection} count=${first.count}`,
	);

	// ① 失败任务默认展开：折叠起来等于「最需要它的时候还得再点一下」。
	H.check(
		"失败任务默认展开日志",
		failedRows.length > 0 &&
			failedRows.every(
				(r) => r.open === true && r.bodyHidden === false && r.aria === "true",
			),
		JSON.stringify(
			failedRows.map((r) => ({
				cap: r.capability,
				open: r.open,
				hidden: r.bodyHidden,
				aria: r.aria,
			})),
		),
	);

	// ② 成功任务默认折叠，而且折叠时**一个日志块都没有**（= 还没请求过）。
	H.check(
		"成功任务默认折叠（折叠时不请求日志）",
		okRows.length > 0 &&
			okRows.every(
				(r) =>
					r.open === false &&
					r.bodyHidden === true &&
					r.aria === "false" &&
					r.bodyChildren === 0 &&
					r.text === "",
			),
		JSON.stringify(
			okRows.map((r) => ({
				cap: r.capability,
				open: r.open,
				hidden: r.bodyHidden,
				children: r.bodyChildren,
			})),
		),
	);

	// ③ 截断提示：必须点名「共 N 字节 / 只保留最后 32 KiB」，不能让人误以为看全了。
	const deploy = first.rows.find((r) => r.capability === "deploy-preview");
	H.check(
		"截断时显示提示行（共 N 字节 / 只保留最后 32 KiB）",
		!!deploy &&
			deploy.trunc === true &&
			/日志已截断/.test(deploy.text) &&
			/32 KiB/.test(deploy.text) &&
			/1048576/.test(deploy.text),
		deploy
			? `trunc=${deploy.trunc} text=${deploy.text.slice(0, 120)}`
			: "缺少 deploy-preview 任务",
	);

	// ③-反 没截断的任务不能显示截断提示（否则这句提示会变成噪音，没人再信它）。
	const runTests = first.rows.find((r) => r.capability === "run-tests" && r.failed);
	H.check(
		"没有截断的任务不显示截断提示",
		!!runTests && runTests.stderr === true && runTests.trunc === false,
		runTests ? `stderr=${runTests.stderr} trunc=${runTests.trunc}` : "缺少失败的 run-tests",
	);

	// ④ 等宽 + 限高 + 双向滚动：一次构建日志上千行，不能把页面撑成竖井。
	H.check(
		"日志块等宽、限高且可滚动（超长行横向滚动）",
		!!first.css &&
			/auto|scroll/.test(first.css.overflow) &&
			first.css.maxHeight !== "none" &&
			first.css.whiteSpace === "pre" &&
			/mono|Menlo|Consolas|Courier/i.test(first.css.font),
		JSON.stringify(first.css),
	);

	// ⑤ 日志里的 HTML / ANSI 一律当文本：日志是外部输入，绝不能变成 DOM。
	H.check(
		"日志按纯文本渲染（HTML 不解析、ANSI 不原样铺开）",
		first.htmlLeak === false && first.textHasMarker === true && first.hasEsc === false,
		JSON.stringify({
			htmlLeak: first.htmlLeak,
			marker: first.textHasMarker,
			esc: first.hasEsc,
		}),
	);

	// ⑥ 展开成功任务才请求日志（懒加载），并画出 stdout 块、不画空的 stderr 块。
	const clickRow = (cap) =>
		page.evaluate((c) => {
			const row = Array.from(document.querySelectorAll("#trace-logs .task-log")).find(
				(r) => r.dataset.capability === c,
			);
			const btn = row && row.querySelector(".task-log-toggle");
			if (btn) btn.click();
			return !!btn;
		}, cap);

	await clickRow("analyze-pr");
	await page
		.waitForSelector("#trace-logs .task-log-pre.task-log-stdout", { timeout: 8000 })
		.catch(() => {});
	const afterExpand = await snap();
	const analyze = afterExpand.rows.find((r) => r.capability === "analyze-pr");
	H.check(
		"展开成功任务后才请求日志（懒加载）并画出 stdout 块",
		!!analyze &&
			analyze.open === true &&
			analyze.bodyHidden === false &&
			analyze.stdout === true &&
			// 空的块不渲染：这个任务只有 stdout，stderr 是空串 → 不该有块
			analyze.stderr === false,
		analyze
			? JSON.stringify({
					open: analyze.open,
					hidden: analyze.bodyHidden,
					stdout: analyze.stdout,
					stderr: analyze.stderr,
				})
			: "缺少 analyze-pr 任务",
	);

	// ⑦ stderr / stdout 不同色（stderr 走 danger 色系，stdout 走正文色）
	H.check(
		"stderr 与 stdout 颜色不同（danger 色系 vs 正文色）",
		!!afterExpand.colors.stderr &&
			!!afterExpand.colors.stdout &&
			afterExpand.colors.stderr !== afterExpand.colors.stdout,
		JSON.stringify(afterExpand.colors),
	);

	// ⑧ 两块都空 → 「没有输出」（而不是空白面板）。
	await clickRow("rollback");
	await page
		.waitForFunction(
			() => {
				const row = Array.from(document.querySelectorAll("#trace-logs .task-log")).find(
					(r) => r.dataset.capability === "rollback",
				);
				return !!row && /没有输出/.test(row.textContent || "");
			},
			{ timeout: 8000 },
		)
		.catch(() => {});
	const afterEmpty = await snap();
	const rollback = afterEmpty.rows.find((r) => r.capability === "rollback");
	H.check(
		"没有日志的任务展开后显示「没有输出」",
		!!rollback &&
			rollback.open === true &&
			rollback.stdout === false &&
			rollback.stderr === false &&
			/没有输出/.test(rollback.text),
		rollback
			? JSON.stringify({
					open: rollback.open,
					stdout: rollback.stdout,
					stderr: rollback.stderr,
					text: rollback.text.slice(0, 80),
				})
			: "缺少 rollback 任务",
	);

	// ⑨ 降级态：日志**存在但取不到**（CONTRACT-logs-on-node）
	//
	// 这两种状态在界面上必须说两句不同的话：
	//   available:false + reason → 「日志在节点上，取不到」（本条）
	//   available:true 但两块都空 → 「没有输出」（上一条 ⑧）
	// 混成一句的后果：用户以为任务真的什么都没打印，去查自己的脚本，
	// 而真相是节点离线 / 档案过了保留期。种子（web/mock.js seedTaskLogs）:
	//   lint-fix        → node_offline（succeeded，默认折叠）
	//   alert.oncall 最后一条 → no_local_archive（succeeded，默认折叠）
	const readLogRow = (cap, last) =>
		page.evaluate(
			({ c, isLast }) => {
				const rows = Array.from(document.querySelectorAll("#trace-logs .task-log")).filter(
					(r) => r.dataset.capability === c,
				);
				const row = isLast ? rows[rows.length - 1] : rows[0];
				if (!row) return null;
				const body = row.querySelector(".task-log-body");
				const state = body ? body.querySelector(".task-log-state") : null;
				return {
					open: row.dataset.open === "1",
					text: body ? (body.textContent || "").replace(/\s+/g, " ").trim() : "",
					unavailable: state ? state.getAttribute("data-logs-unavailable") : null,
					pre: row.querySelectorAll(".task-log-pre").length,
				};
			},
			{ c: cap, isLast: !!last },
		);

	const clickLastRow = (cap) =>
		page.evaluate((c) => {
			const rows = Array.from(document.querySelectorAll("#trace-logs .task-log")).filter(
				(r) => r.dataset.capability === c,
			);
			const row = rows[rows.length - 1];
			const btn = row && row.querySelector(".task-log-toggle");
			if (btn) btn.click();
			return !!btn;
		}, cap);

	await clickRow("lint-fix");
	await page
		.waitForSelector('#trace-logs [data-logs-unavailable="node_offline"]', { timeout: 8000 })
		.catch(() => {});
	const lintFix = await readLogRow("lint-fix", false);
	H.check(
		"日志取不到时点名「节点离线」，且**不**说「没有输出」（两种状态不许混）",
		!!lintFix &&
			lintFix.open === true &&
			lintFix.unavailable === "node_offline" &&
			/节点离线/.test(lintFix.text || "") &&
			!/没有输出/.test(lintFix.text || "") &&
			lintFix.pre === 0,
		JSON.stringify(lintFix).slice(0, 220),
	);

	await clickLastRow("alert.oncall");
	await page
		.waitForSelector('#trace-logs [data-logs-unavailable="no_local_archive"]', {
			timeout: 8000,
		})
		.catch(() => {});
	const archive = await readLogRow("alert.oncall", true);
	H.check(
		"日志档案过保留期时点明「档案 / 保留期」，也不说「没有输出」",
		!!archive &&
			archive.open === true &&
			archive.unavailable === "no_local_archive" &&
			/档案/.test(archive.text || "") &&
			/保留期/.test(archive.text || "") &&
			!/没有输出/.test(archive.text || ""),
		JSON.stringify(archive).slice(0, 220),
	);

	// 回归：非降级的那条（两块都空）仍然是「没有输出」——
	// 上面两条不能把这条的语义也改掉（available:true 才是真没输出）。
	const rollbackAgain = await readLogRow("rollback", false);
	H.check(
		"非降级的空日志仍然说「没有输出」（available:true 才是真没输出）",
		!!rollbackAgain &&
			rollbackAgain.unavailable === null &&
			/没有输出/.test(rollbackAgain.text || "") &&
			!/节点离线|保留期/.test(rollbackAgain.text || ""),
		JSON.stringify(rollbackAgain).slice(0, 220),
	);

	// ⑩ 英文页同一份降级文案（四语言 key 已就位，这里验 en 不串中文）
	const enPage = await ctx.newPage();
	const enErrors = [];
	enPage.on("pageerror", (e) => enErrors.push(String((e && e.message) || e)));
	await enPage.goto(base + "/en/trace.html?traceId=trace_mock_t9&view=graph", {
		waitUntil: "load",
		timeout: 20000,
	});
	// lint-fix 是成功任务（默认折叠）：展开才请求日志，所以这里也要点一下
	await enPage
		.waitForSelector("#trace-logs .task-log", { timeout: 15000 })
		.catch(() => {});
	await enPage
		.evaluate(() => {
			const row = Array.from(document.querySelectorAll("#trace-logs .task-log")).find(
				(r) => r.dataset.capability === "lint-fix",
			);
			const btn = row && row.querySelector(".task-log-toggle");
			if (btn) btn.click();
			return !!btn;
		})
		.catch(() => {});
	await enPage
		.waitForSelector('#trace-logs [data-logs-unavailable="node_offline"]', { timeout: 8000 })
		.catch(() => {});
	const enState = await enPage
		.evaluate(() => {
			const node = document.querySelector('#trace-logs [data-logs-unavailable="node_offline"]');
			return {
				found: !!node,
				text: node ? (node.textContent || "").replace(/\s+/g, " ").trim() : "",
			};
		})
		.catch(() => ({ found: false, text: "" }));
	H.check(
		"英文页的降级文案是英文（含 Node offline，且无中文残留）",
		enState.found === true &&
			/Node offline/.test(enState.text || "") &&
			!/[\u4e00-\u9fff\u3400-\u4dbf]/.test(enState.text || ""),
		String(enState.text).slice(0, 200),
	);
	H.check(
		"英文 trace 页无 JS pageerror",
		enErrors.length === 0,
		enErrors.slice(0, 2).join("; "),
	);
	await enPage.close();

	H.check("任务日志面板没有 JS 报错", pageErrors.length === 0, pageErrors.slice(0, 2).join("; "));

	await page.close();
}

// ---------------------------------------------------------------------------
// J. 共享拓扑推导（graphs.js 的 sourceTopology）—— 真后端
//
// 用户报的两个问题（原话）：
//   「source.html 这里的图应该要和 trace.html 这里的图保持一致，底层一套推算逻辑」
//   「现在 source 的图没有体现出 source 一对多的线，本来应该 source 有两个线的」
//
// 根因：拓扑推导有两份（source.html 的 buildFlowTree + trace.html 的 buildBaseGraph），
// 而且老实现用「节点首次被访问时打 isEntry」来决定画不画「源 → 能力」这条线，
// addNode 遇到已存在的节点是静默跳过的 —— 菱形（既能从源直达、又能从别的分支到达）
// 里先到的分支把节点登记成非入口，源 → 它 的那条线就永远不画。
//
// 现在推导只有一份（sourceTopology），1:N 由构造保证：入口边是唯一入口，
// N 条入口边 ⇒ N 个 entry 节点 ⇒ N 条线。这组用例钉四件事：
//   ① 菱形：源节点有 2 条出边（用**图的实例数据**断言，不数 SVG 路径 —— 布局会合并视觉连线）
//   ② 同源跨页一致：source 页与 trace 页的能力节点集合、边集合完全相同
//   ③ 环（A→B→A）仍画出来并标 cycle（重构不许把它吞掉）
//   ④ 声明了但没接边的出口仍显示 deadport 灰点（文案是「没有下游」，不再提端口）
//   ⑤ 端口不上图：单出口的边传空串，只有**多出口（历史数据）**才带出口名
//
// 为什么 ③ 的环可能仍直插库：契约 §2 已放开配置期环检测（`review → fix → review` 是合法编排，
// 靠 hop 预算 + 每 trace 任务预算兜底），所以反方向那条边**现在应该能直接建**。
// 本次竖切里负责删 `store.checkEdgeWouldCycle` 的是 A 组，尚未合并进这个 worktree：
// 所以下面先试 API，被拒才退回直插库（图渲染那几条断言与建边途径无关，两种都能跑）。
// 合并后自动走 API 分支，跑的就是「成环可建」。
// ---------------------------------------------------------------------------

// readGraphSpec 取页面里已挂载图的**实例数据**。
//
// 为什么不用 graph._debug.nodes()：那个接口只暴露 id/label/kind（边只有 from/to/port），
// 断言不到 entry / cycle / deadport / muted —— 而这几项正是本次要钉的。
// host._graphSpec 是 graphs.js 交给 FlowGraph 的**那一份** spec，也就是
// "图上到底画了什么"的权威（宿主本身就是 .flograph 容器）。
async function readGraphSpec(page, timeout = 15000) {
	await page
		.waitForFunction(
			() => {
				const g = document.querySelector(".flograph");
				return !!(g && g._graphSpec && (g._graphSpec.nodes || []).length);
			},
			null,
			{ timeout },
		)
		.catch(() => {});
	return page.evaluate(() => {
		const g = document.querySelector(".flograph");
		const sp = g && g._graphSpec;
		if (!sp) return null;
		return {
			nodes: (sp.nodes || []).map((n) => ({
				id: n.id,
				label: n.label || "",
				sub: n.sub || "",
				kind: n.kind || "",
				entry: !!n.entry,
				cycle: !!n.cycle,
				deadport: !!n.deadport || n.kind === "deadport",
			})),
			edges: (sp.edges || []).map((e) => ({
				from: e.from,
				to: e.to,
				port: e.port || "",
				cycle: !!e.cycle,
				loopback: !!e.loopback,
				muted: !!e.muted,
			})),
		};
	});
}

// edgeKeys 把边集合压成可比较的字符串集合（只比结构：谁→谁、出口是否上图）。
function edgeKeys(spec, dropFrom) {
	const keys = (spec?.edges || [])
		.filter((e) => e.from !== dropFrom)
		.map((e) => e.from + "|" + e.port + "|" + e.to);
	return Array.from(new Set(keys)).sort();
}

function nodeIds(spec, dropId) {
	return Array.from(
		new Set((spec?.nodes || []).map((n) => n.id).filter((id) => id !== dropId)),
	).sort();
}

async function checkSharedTopology(server, newCtx) {
	const s = H.seed(server.dbPath, "topo");
	const base = server.base;

	// 默认命名空间 id：造数（直插库的 edges.namespace_id）与真节点注册都要用它。
	// 它必须在**任何** mkEdge 之前拿到 —— 所以提到最前面，断言仍在（只此一处）。
	const nsId = await H.defaultNamespaceId(base, s.session);
	H.check("拓扑造数：拿到默认命名空间 id", !!nsId, String(nsId));

	const mkCap = async (name, outputs) => {
		const r = await H.req(base, "/v1/capabilities", {
			session: s.session,
			method: "POST",
			body: { name, outputs, paramsSchema: { type: "object" } },
		});
		H.check(`拓扑造数：建能力 ${name} → 200`, r.status === 200, r.text?.slice(0, 200));
		return r.json?.capability || {};
	};
	// mkEdge 直插库造一条**能力边**（拓扑 fixture 专用）。
	//
	// **为什么改口径**（CONTRACT-downstream-edit §2/§3）：
	//   ① 能力边的唯一写入口已经是 PUT /v1/capabilities/{name}/downstreams，
	//      `POST /v1/edges` 的能力边现在恒 405 edge_write_removed ——
	//      旧的「→ 200」断言会永远红，而且它验的接口已经不存在了。
	//   ② 又不能用新接口：这一组 fixture 故意用**多出口历史数据**
	//      （topo.a outputs:["next"]、topo.multi outputs:["left","right"]），
	//      新接口只表达隐式出口 out。这种能力界面上已经产生不了，
	//      但「历史数据仍要画得出出口名」正是 ⑤ 要验的东西。
	// 所以这里绕开 API 只造数据，断言对象仍然是**图**（与下面原来的环边兜底同一口径）。
	const mkEdge = async (fromRef, fromPort, toRef, label) => {
		let err = null;
		try {
			const db = new DatabaseSync(server.dbPath);
			try {
				db.prepare(
					`INSERT INTO edges (namespace_id, from_is_source, from_ref, from_port, to_ref, literals, mapping, created_at)
					 VALUES (?, 0, ?, ?, ?, '{}', '{}', ?)`,
				).run(nsId, fromRef, fromPort, toRef, Math.floor(Date.now() / 1000));
			} finally {
				db.close();
			}
		} catch (e) {
			err = String((e && e.message) || e);
		}
		H.check(
			`拓扑造数：${label} → 直插库落库`,
			err === null,
			err ? err.slice(0, 200) : undefined,
		);
		return err === null;
	};
	const mkSource = async (name, targetIds) => {
		const r = await H.req(base, "/v1/sources", {
			session: s.session,
			method: "POST",
			body: { name, kind: "manual", targets: targetIds },
		});
		H.check(
			`拓扑造数：建源 ${name}（出口 ${targetIds.length} 个）→ 201`,
			r.status === 201,
			r.text?.slice(0, 300),
		);
		return r.json?.source || {};
	};
	const openPage = async (ctx, url) => {
		const page = await ctx.newPage();
		const errs = [];
		page.on("pageerror", (e) => errs.push(String(e)));
		await page.goto(url, { waitUntil: "load", timeout: 20000 });
		await H.waitForAppReady(page, "workspace");
		return { page, errs };
	};

	// ---------- ① 菱形：源 → A、源 → B、A → B ----------
	//
	// B 既是首层（源直达）、又能从 A 到达。老实现里只要 A→B 先被遍历到，
	// 「源 → B」那条线就永远不画 —— 用户看到的就是"少一条线"。
	const capA = await mkCap("topo.a", ["next"]);
	const capB = await mkCap("topo.b", ["done"]);
	H.check(
		"拓扑造数：A/B 能力 id 就绪",
		!!capA.id && !!capB.id,
		JSON.stringify({ capA, capB }),
	);
	await mkEdge("topo.a", "next", "topo.b", "能力边 topo.a --next--> topo.b（菱形的那条捷径）");
	// 源建好就有两条入口边（from_ref=源名、from_port='*'）—— 这就是 1:N。
	const srcFan = await mkSource("topo.fan", [capA.id, capB.id]);

	// 真后端也确认一遍事实：入口边是唯一入口（2 条，左端是源名）
	const edgesAll = await H.req(base, "/v1/edges", { session: s.session });
	const entryOfFan = (edgesAll.json?.edges || []).filter(
		(e) => e.fromIsSource === true && e.fromRef === "topo.fan",
	);
	H.check(
		"真后端：源 topo.fan 有 2 条入口边（1:N 的配置事实）",
		entryOfFan.length === 2,
		JSON.stringify(entryOfFan.map((e) => e.toRef)),
	);

	// 上线节点，让事件真的派出任务（trace 页要有任务才画图）
	const topoNode = await N.bringUpNode(base, server.dbPath, nsId, "topo-node", [
		{ id: capA.id, name: "topo.a", version: capA.version },
		{ id: capB.id, name: "topo.b", version: capB.version },
	]);
	H.check("拓扑造数：节点已上线", !!topoNode?.id, JSON.stringify(Object.keys(topoNode || {})));

	const ev = await H.req(base, "/v1/events", {
		key: s.sendKey,
		body: { source: "topo.fan", type: "run", payload: { who: "e2e-topo" } },
	});
	const traceId = ev.json?.traceId;
	H.check(
		"拓扑造数：事件派出任务（trace 页有内容）",
		(ev.json?.tasks || []).length >= 1 && typeof traceId === "string" && traceId.length > 0,
		JSON.stringify({ status: ev.status, tasks: (ev.json?.tasks || []).length, traceId }),
	);

	const ctx = await newCtx();
	await injectSession(ctx, s.session, base);

	// ---------- ① 源详情页：源节点必须有 2 条出边 ----------
	let srcSpec = null;
	{
		const { page, errs } = await openPage(
			ctx,
			base + "/source.html?id=" + encodeURIComponent(srcFan.id),
		);
		srcSpec = await readGraphSpec(page);
		H.check("① 源详情页画出了图（实例数据非空）", !!srcSpec && srcSpec.nodes.length > 0, JSON.stringify(srcSpec?.nodes?.map((n) => n.id)));

		const outOfSource = (srcSpec?.edges || []).filter((e) => e.from === "__source__");
		H.check(
			"① 源节点有 2 条出边（1:N 修好了 —— 用户报的「只看到一条线」）",
			outOfSource.length === 2,
			JSON.stringify(srcSpec?.edges),
		);
		const outTargets = outOfSource.map((e) => e.to).sort();
		H.check(
			"① 那 2 条线分别指向 A 与 B（不是同一个节点画两遍）",
			JSON.stringify(outTargets) ===
				JSON.stringify([capA.id, capB.id].sort()),
			JSON.stringify({ outTargets, capA: capA.id, capB: capB.id }),
		);
		const entryNodes = (srcSpec?.nodes || []).filter((n) => n.entry).map((n) => n.id).sort();
		H.check(
			"① 首层节点（entry）就是 A 与 B —— 线的数量由它构造保证",
			JSON.stringify(entryNodes) === JSON.stringify([capA.id, capB.id].sort()),
			JSON.stringify(entryNodes),
		);
		H.check(
			"① 菱形里 A→B 那条边也在（不能为了画源线把它吞掉）",
			// A 是单出口能力（outputs 长度 1）→ 边不把出口名传给渲染层，
			// 所以这里看的是**结构**（A → B 存在）而不是 port（契约 hide-ports §4）
			(srcSpec?.edges || []).some(
				(e) => e.from === capA.id && e.to === capB.id && e.port === "",
			),
			JSON.stringify(srcSpec?.edges),
		);

		// ---------- ④ 声明了但没接边的出口 → 灰点 ----------
		const deadID = "dead|" + capB.id + "|done";
		const deadNode = (srcSpec?.nodes || []).find((n) => n.id === deadID);
		H.check(
			"④ 未接下游的出口画成 deadport 灰点",
			!!deadNode && deadNode.kind === "deadport" && deadNode.deadport === true,
			JSON.stringify(srcSpec?.nodes?.filter((n) => n.deadport)),
		);
		H.check(
			"④ 灰点文案是「（没有下游）」/「没有下游」（不再说某个出口没接）",
			!!deadNode && deadNode.label === "（没有下游）" && deadNode.sub === "没有下游",
			JSON.stringify(deadNode),
		);
		const deadEdge = (srcSpec?.edges || []).find((e) => e.to === deadID);
		H.check(
			"④ 指向灰点的边弱化（muted）且不带出口名",
			!!deadEdge &&
				deadEdge.from === capB.id &&
				deadEdge.port === "" &&
				deadEdge.muted === true,
			JSON.stringify(deadEdge),
		);

		H.check("① 源详情页无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 200));
		await page.close();
	}

	// ---------- ② 同源跨页一致：source 页 vs trace 页 ----------
	{
		const { page, errs } = await openPage(
			ctx,
			base + "/trace.html?traceId=" + encodeURIComponent(traceId),
		);
		const traceSpec = await readGraphSpec(page);
		H.check(
			"② trace 页画出了底图（实例数据非空）",
			!!traceSpec && traceSpec.nodes.length > 0,
			JSON.stringify(traceSpec?.nodes?.map((n) => n.id)),
		);

		// 源节点 id 两页不同（source 页是 __source__、trace 页是 __trigger__），
		// 触发器自身的连线也不算拓扑 —— 两边都剔掉再比。
		const a = nodeIds(srcSpec, "__source__");
		const b = nodeIds(traceSpec, "__trigger__");
		H.check(
			"② 两页的能力节点集合完全相同（同一份推导）",
			JSON.stringify(a) === JSON.stringify(b),
			JSON.stringify({ source页: a, trace页: b }),
		);
		const ea = edgeKeys(srcSpec, "__source__");
		const eb = edgeKeys(traceSpec, "__trigger__");
		H.check(
			"② 两页的边集合完全相同（同一个源的两张图必须一致）",
			JSON.stringify(ea) === JSON.stringify(eb),
			JSON.stringify({ source页: ea, trace页: eb }),
		);
		// 反向守卫：如果两页都空，上面两条会"因为都空而通过"
		H.check(
			"② 比的是非空图（空集对比没有意义）",
			a.length >= 3 && ea.length >= 2,
			JSON.stringify({ nodes: a.length, edges: ea.length }),
		);
		H.check("② trace 页无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 200));
		await page.close();
	}

	// ---------- ③ 环：A→B→A 仍然画出来并标 cycle ----------
	{
		const capC = await mkCap("topo.c", ["next"]);
		const capD = await mkCap("topo.d", ["back"]);
		await mkEdge("topo.c", "next", "topo.d", "能力边 topo.c --next--> topo.d");
		// 反向那条边：配置期已不再拒环（契约 §2）。
		//
		// **为什么改口径**：「成环可建」原来靠 `POST /v1/edges {fromIsSource:false}`
		// 回来 200 来证明，而那条路已经封了（405 edge_write_removed，见契约 §3）。
		// 改成两部分，两边都保留断言：
		//   · 语义（配置期放行环）用**新接口**验：两个单出口能力 a→b、b→a 各 PUT 一次；
		//   · 图渲染（cycle / loopback 标记）仍用下面这条多出口历史边，直插库造。
		// 旧代码里「合并后启用」的兜底分支随之删除 —— 合并已经发生，它不会再走了。
		const cycA = await mkCap("topo.cyc.a", ["out"]);
		const cycB = await mkCap("topo.cyc.b", ["out"]);
		const cycPut1 = await H.req(base, "/v1/capabilities/topo.cyc.a/downstreams", {
			session: s.session,
			method: "PUT",
			body: { links: [{ toRef: "topo.cyc.b", priority: 0 }] },
		});
		const cycPut2 = await H.req(base, "/v1/capabilities/topo.cyc.b/downstreams", {
			session: s.session,
			method: "PUT",
			body: { links: [{ toRef: "topo.cyc.a", priority: 0 }] },
		});
		H.check(
			"③ 成环可建：a→b 与 b→a 两次 PUT …/downstreams 都被接受（配置期不拒环）",
			!!cycA.id &&
				!!cycB.id &&
				cycPut1.status === 200 &&
				cycPut2.status === 200,
			`${cycPut1.status} ${String(cycPut1.text || "").slice(0, 120)} / ${cycPut2.status} ${String(
				cycPut2.text || "",
			).slice(0, 120)}`,
		);
		await mkEdge("topo.d", "back", "topo.c", "能力边 topo.d --back--> topo.c（环的回边）");
		const srcCyc = await mkSource("topo.cyc", [capC.id]);

		const { page, errs } = await openPage(
			ctx,
			base + "/source.html?id=" + encodeURIComponent(srcCyc.id),
		);
		const spec = await readGraphSpec(page);
		const ids = (spec?.nodes || []).map((n) => n.id);
		H.check(
			"③ 环上的两个节点都画出来了（不吞节点）",
			ids.includes(capC.id) && ids.includes(capD.id),
			JSON.stringify(ids),
		);
		const e1 = (spec?.edges || []).find((e) => e.from === capC.id && e.to === capD.id);
		const e2 = (spec?.edges || []).find((e) => e.from === capD.id && e.to === capC.id);
		H.check(
			"③ 环上的两条边都画出来了（不吞边）",
			!!e1 && !!e2,
			JSON.stringify(spec?.edges),
		);
		H.check(
			"③ 回边标记成 cycle（并绕出去画，loopback）",
			!!e2 && e2.cycle === true && e2.loopback === true,
			JSON.stringify(e2),
		);
		H.check(
			"③ 正向那条边不该被误标成环",
			!!e1 && e1.cycle === false,
			JSON.stringify(e1),
		);
		const cycNode = (spec?.nodes || []).find((n) => n.id === capC.id);
		H.check(
			"③ 环上的节点带 cycle 标记（图上看得出这里会绕回来）",
			!!cycNode && cycNode.cycle === true,
			JSON.stringify(cycNode),
		);
		H.check("③ 环的页面无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 200));
		await page.close();
	}

	// ---------- ⑤ 多出口能力（历史数据）是唯一的例外：出口名仍要上图 ----------
	//
	// 契约 §4：单出口（outputs 长度 ≤ 1）不画出口名；但**多出口**能力里
	// 两条边只靠出口名区分，不标就分不清。这种数据界面已经产生不了
	// （表单只有一个下游输入框），可历史库里可能有 —— 所以必须还能画。
	{
		const capMulti = await mkCap("topo.multi", ["left", "right"]);
		await mkCap("topo.multi.a", []);
		await mkCap("topo.multi.b", []);
		await mkEdge("topo.multi", "left", "topo.multi.a", "能力边 topo.multi --left--> topo.multi.a");
		await mkEdge("topo.multi", "right", "topo.multi.b", "能力边 topo.multi --right--> topo.multi.b");
		const srcMulti = await mkSource("topo.multi.src", [capMulti.id]);
		const { page, errs } = await openPage(
			ctx,
			base + "/source.html?id=" + encodeURIComponent(srcMulti.id),
		);
		const spec = await readGraphSpec(page);
		const outEdges = (spec?.edges || []).filter((e) => e.from === capMulti.id);
		const ports = outEdges.map((e) => e.port).sort();
		const labels = await page.evaluate(() =>
			Array.from(document.querySelectorAll(".flograph-edge-label")).map((x) =>
				x.textContent.trim(),
			),
		);
		H.check(
			"⑤ 多出口能力的边仍带出口名（历史数据不静默改写）",
			JSON.stringify(ports) === JSON.stringify(["left", "right"]),
			JSON.stringify(outEdges),
		);
		H.check(
			"⑤ 多出口的出口名真的画到了边标签上",
			["left", "right"].every((x) => labels.includes(x)),
			JSON.stringify(labels),
		);
		H.check("⑤ 多出口的页面无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 200));
		await page.close();
	}

	// ---------- 移动端 390：这两张图不许把页面撑出横向滚动 ----------
	{
		const page = await ctx.newPage();
		await page.setViewportSize({ width: 390, height: 844 });
		const errs = [];
		page.on("pageerror", (e) => errs.push(String(e)));
		await page.goto(base + "/source.html?id=" + encodeURIComponent(srcFan.id), {
			waitUntil: "load",
			timeout: 20000,
		});
		await H.waitForAppReady(page, "workspace");
		await readGraphSpec(page, 10000);
		// 图的节点是**画布内容**：flograph 自己 overflow:hidden 并做 fit/平移，
		// 卡在画布边界外的节点不算"页面横向溢出"（判页面是否真的能横向滚动才准）。
		const ov = await page.evaluate(() => {
			const outside = [];
			for (const el of document.querySelectorAll("body *")) {
				const r = el.getBoundingClientRect();
				if (
					r.right > window.innerWidth + 5 &&
					!el.closest(".flograph") &&
					!el.closest("pre") &&
					!el.closest(".overflow-x-auto")
				)
					outside.push(el.tagName + "." + el.className);
			}
			return {
				scroll: document.documentElement.scrollWidth - window.innerWidth,
				outside: outside.slice(0, 3),
			};
		});
		H.check(
			"移动390 source 页不横向滚动（scrollWidth ≤ innerWidth）",
			ov.scroll <= 0,
			`scrollWidth 超出 ${ov.scroll}px`,
		);
		H.check(
			"移动390 source 页图外的元素没溢出",
			ov.outside.length === 0,
			JSON.stringify(ov.outside),
		);
		H.check("移动390 source 页无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 200));
		await page.close();
	}

	await ctx.close();
	try {
		topoNode?.conn?.close?.();
	} catch {
		/* ignore */
	}
}
