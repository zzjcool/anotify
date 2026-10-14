#!/usr/bin/env node
/* SUITE: ux_regression_19_23 — #19–#23 控制台 UX 回归断言
 *
 * 锁死 Wren PR #25 修好的行为，防再回归：
 *  #19 member 打开 admin.html → 明确无权限页（非坏掉的 KPI 外壳）
 *  #20 Notification.permission=denied → 主按钮「查看如何开启」+ toast 反馈
 *  #21 问候语跟 /v1/auth/me 对齐（不再硬编码 zheng）
 *  #22 security 空态文案指向「+ 本机添加」
 *  #23 0 设备时送达率非假 100%、点测试通知被拦且表单不打开
 */
import { chromium } from "playwright-core";
import * as H from "../lib/harness.mjs";

const RP = "localhost";
let server, browser;

async function injectSession(ctx, sessionValue, base) {
	const u = new URL(base);
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

async function newCtx() {
	const ctx = await browser.newContext();
	await H.mockExternalCdn(ctx);
	return ctx;
}

async function denyNotifications(ctx) {
	// Headless Chrome（尤其 Docker）常忽略 CDP Browser.setPermission(denied)，
	// permission 仍停在 default。用 addInitScript 直接 stub，页面读到 denied。
	await ctx.addInitScript(() => {
		Object.defineProperty(window, "Notification", {
			configurable: true,
			writable: true,
			value: class Notification {
				static get permission() {
					return "denied";
				}
				static requestPermission() {
					return Promise.resolve("denied");
				}
			},
		});
	});
}

async function main() {
	H.startTimer();
	console.log("=== SUITE: ux_regression_19_23（#19–#23 控制台 UX 回归）===");
	server = await H.startServer({ suiteName: "ux_regression_19_23", rpId: RP });
	browser = await chromium.launch({
		channel: "chrome",
		headless: true,
		args: ["--no-sandbox"],
	});

	// ---------- #19 member 看 admin → 无权限页 ----------
	console.log("--- #19 member admin 无权限态 ---");
	{
		const member = H.seed(server.dbPath, "ux19_member_" + Date.now());
		const me = await H.req(server.base, "/v1/auth/me", {
			session: member.session,
		});
		H.eq("#19 前置 member role", me.json?.role, "member");

		const ctx = await newCtx();
		await injectSession(ctx, member.session, server.base);
		const page = await ctx.newPage();
		await page.goto(server.base + "/admin.html", {
			waitUntil: "load",
			timeout: 15000,
		});
		await H.waitForAppReady(page, "workspace");
		// 等 forbidden 态替换 page-main（或确认不再是加载中 KPI）
		await page
			.waitForFunction(
				() => {
					const main = document.getElementById("page-main");
					if (!main) return false;
					const txt = main.innerText || "";
					return (
						txt.includes("无权访问") ||
						txt.includes("无权限") ||
						txt.includes("Forbidden") ||
						txt.includes("not an admin") ||
						!!main.querySelector('a[href="index.html"]')
					);
				},
				{ timeout: 8000 },
			)
			.catch(() => {});

		const state = await page.evaluate(() => {
			const main = document.getElementById("page-main");
			const txt = main ? main.innerText : "";
			const kpi = document.getElementById("kpi-users");
			const back = main
				? main.querySelector('a[href="index.html"]')
				: null;
			return {
				txt: txt.slice(0, 400),
				hasForbiddenTitle:
					/无权访问|无权限|Forbidden|not an admin/i.test(txt),
				hasBackLink: !!back,
				kpiStillLoadingShell:
					!!kpi &&
					(kpi.textContent || "").trim() !== "" &&
					!/无权|Forbidden/i.test(txt),
			};
		});
		H.check(
			"#19 member 打开 admin 显示无权限标题",
			state.hasForbiddenTitle,
			state.txt,
		);
		H.check(
			"#19 无权限页有返回总览链接",
			state.hasBackLink,
			state.txt.slice(0, 120),
		);
		H.check(
			"#19 不是坏掉的 KPI/加载外壳（无权限页替换主区）",
			state.hasForbiddenTitle && !state.kpiStillLoadingShell,
			JSON.stringify(state),
		);

		await page.close();
		await ctx.close();
	}

	// ---------- #20 denied 时主按钮反馈 ----------
	console.log("--- #20 Push denied 反馈 ---");
	{
		const user = H.seed(server.dbPath, "ux20_recv_" + Date.now());
		const ctx = await newCtx();
		await injectSession(ctx, user.session, server.base);
		await denyNotifications(ctx);

		const page = await ctx.newPage();
		await page.goto(server.base + "/receivers.html", {
			waitUntil: "load",
			timeout: 15000,
		});
		await H.waitForAppReady(page, "workspace", {
			dataAnchor: "#current-device-card",
		});
		await page
			.waitForFunction(
				() => {
					const card = document.getElementById("current-device-card");
					return card && card.querySelector("button");
				},
				{ timeout: 8000 },
			)
			.catch(() => {});

		const btnText = await page.evaluate(() => {
			const btn = document.querySelector("#current-device-card button");
			return btn ? btn.textContent.trim() : "";
		});
		H.check(
			"#20 denied 时主按钮文案为「查看如何开启」（非沉默「开启本机通知」）",
			/查看如何开启|how to enable|設定|cómo/i.test(btnText),
			`btn=${btnText}`,
		);

		await page.click("#current-device-card button");
		await page
			.waitForSelector("#anotify-toast-host", { timeout: 5000 })
			.catch(() => {});
		const toast = await page.evaluate(() => {
			const host = document.getElementById("anotify-toast-host");
			return host ? host.innerText.trim() : "";
		});
		H.check(
			"#20 点击后出现用户可见 toast（含 denied/权限提示）",
			toast.length > 0 &&
				(/denied|权限|通知|browser|設定|permiso/i.test(toast)),
			`toast=${toast.slice(0, 200)}`,
		);

		await page.close();
		await ctx.close();
	}

	// ---------- #21 问候语跟 /auth/me ----------
	console.log("--- #21 问候语对齐 /auth/me ---");
	{
		const uname = "ux21_greet_" + Date.now();
		const user = H.seed(server.dbPath, uname);
		const me = await H.req(server.base, "/v1/auth/me", {
			session: user.session,
		});
		const expectName = me.json?.displayName || me.json?.username || uname;

		const ctx = await newCtx();
		await injectSession(ctx, user.session, server.base);
		const page = await ctx.newPage();
		// 故意带 ?u=zheng 验证 loadMe 覆盖硬编码/查询串兜底
		await page.goto(server.base + "/index.html?u=zheng", {
			waitUntil: "load",
			timeout: 15000,
		});
		await H.waitForAppReady(page, "workspace");
		await page
			.waitForFunction(
				(name) => {
					const g = document.getElementById("greeting");
					const s = document.getElementById("sidebar-username");
					return (
						g &&
						s &&
						(g.textContent || "").includes(name) &&
						(s.textContent || "").includes(name)
					);
				},
				expectName,
				{ timeout: 8000 },
			)
			.catch(() => {});

		const names = await page.evaluate(() => ({
			greeting: document.getElementById("greeting")?.textContent || "",
			sidebar: document.getElementById("sidebar-username")?.textContent || "",
		}));
		H.check(
			"#21 问候语包含 /auth/me 用户名",
			names.greeting.includes(expectName),
			`greeting=${names.greeting} expect=${expectName}`,
		);
		H.check(
			"#21 侧栏用户名与问候语同一身份",
			names.sidebar.includes(expectName) &&
				names.greeting.includes(names.sidebar.trim()),
			JSON.stringify(names),
		);
		H.check(
			"#21 问候语不再残留硬编码 zheng（除非真是该用户）",
			expectName.includes("zheng") || !names.greeting.includes("zheng"),
			`greeting=${names.greeting}`,
		);

		await page.close();
		await ctx.close();
	}

	// ---------- #22 security 空态文案 ----------
	console.log("--- #22 security 空态「+ 本机添加」---");
	{
		const user = H.seed(server.dbPath, "ux22_sec_" + Date.now());
		const ctx = await newCtx();
		await injectSession(ctx, user.session, server.base);
		const page = await ctx.newPage();
		await page.goto(server.base + "/security.html", {
			waitUntil: "load",
			timeout: 15000,
		});
		await H.waitForAppReady(page, "workspace", {
			dataAnchor: "#passkey-list",
		});
		await page
			.waitForFunction(
				() => {
					const list = document.getElementById("passkey-list");
					return list && (list.innerText || "").length > 0;
				},
				{ timeout: 8000 },
			)
			.catch(() => {});

		const copy = await page.evaluate(() => {
			const list = document.getElementById("passkey-list");
			const empty = list ? list.innerText : "";
			const btns = [...document.querySelectorAll("button")].map((b) =>
				(b.textContent || "").trim(),
			);
			return { empty, btns };
		});
		H.check(
			"#22 空态文案含「本机添加」",
			/本机添加|Add on this device|この端末|este dispositivo/i.test(
				copy.empty,
			),
			copy.empty.slice(0, 200),
		);
		H.check(
			"#22 空态不再指向旧文案「在新设备添加」/「添加 Passkey」误导",
			!/在新设备添加/.test(copy.empty) &&
				!/点右上角「\+ 添加 Passkey」/.test(copy.empty),
			copy.empty.slice(0, 200),
		);
		H.check(
			"#22 右上角存在「+ 本机添加」按钮",
			copy.btns.some((t) => /本机添加|Add on this device/i.test(t)),
			JSON.stringify(copy.btns.slice(0, 8)),
		);

		await page.close();
		await ctx.close();
	}

	// ---------- #23 0 设备：送达率诚实 + 测试通知拦截 ----------
	console.log("--- #23 0 设备测试通知 / 送达率 ---");
	{
		const user = H.seed(server.dbPath, "ux23_zero_" + Date.now());
		const ctx = await newCtx();
		await injectSession(ctx, user.session, server.base);
		const page = await ctx.newPage();
		await page.goto(server.base + "/index.html", {
			waitUntil: "load",
			timeout: 15000,
		});
		await H.waitForAppReady(page, "workspace");
		// 等 KPI 渲染（load 完成）
		await page
			.waitForFunction(
				() => {
					const rate = document.getElementById("kpi-rate");
					const sub = document.getElementById("kpi-rate-sub");
					const devices = document.getElementById("kpi-devices");
					return (
						rate &&
						sub &&
						devices &&
						(devices.textContent || "").trim() !== "" &&
						(rate.textContent || "").trim() !== ""
					);
				},
				{ timeout: 8000 },
			)
			.catch(() => {});

		const kpi = await page.evaluate(() => ({
			rate: (document.getElementById("kpi-rate")?.textContent || "").trim(),
			sub: (document.getElementById("kpi-rate-sub")?.textContent || "").trim(),
			devices: (
				document.getElementById("kpi-devices")?.textContent || ""
			).trim(),
			demo: !document
				.getElementById("demo-badge")
				?.classList.contains("hidden"),
		}));
		H.check(
			"#23 0 设备时送达率不是假 100%",
			!/^100\s*%?$/.test(kpi.rate.replace(/\s/g, "")) &&
				!kpi.rate.startsWith("100"),
			JSON.stringify(kpi),
		);
		H.check(
			"#23 0 设备送达率展示 – 或「暂无接收设备」类诚实空态",
			kpi.rate === "–" ||
				kpi.rate === "-" ||
				kpi.rate === "—" ||
				/暂无|不可用|no device|N\/A|unavailable/i.test(kpi.sub),
			JSON.stringify(kpi),
		);

		// 点「发送测试通知」应被拦，表单保持 hidden
		const beforeHidden = await page.evaluate(() =>
			document.getElementById("tn-form")?.classList.contains("hidden"),
		);
		H.check("#23 前置 tn-form 默认隐藏", beforeHidden === true);

		await page.click("#tn-toggle");
		await page.waitForFunction(
			() => {
				const msg = document.getElementById("tn-agent-state-msg");
				return msg && (msg.textContent || "").trim().length > 0;
			},
			{ timeout: 3000 },
		).catch(() => {});
		const after = await page.evaluate(() => {
			const form = document.getElementById("tn-form");
			const msg = document.getElementById("tn-agent-state-msg");
			return {
				formHidden: form ? form.classList.contains("hidden") : true,
				status: msg ? msg.textContent.trim() : "",
			};
		});
		H.check(
			"#23 0 设备点击测试通知后表单仍隐藏（不打开假成功路径）",
			after.formHidden === true,
			JSON.stringify(after),
		);
		H.check(
			"#23 状态区提示先添加/开启接收设备",
			after.status.length > 0 &&
				(/没有接收设备|开启本机通知|添加设备|no (receiving )?device|receiv/i.test(
					after.status,
				) ||
					/通知接收/.test(after.status)),
			`status=${after.status}`,
		);
		H.check(
			"#23 状态区不宣称「完成」/假成功",
			!/完成|100%|已发送，命中/.test(after.status),
			`status=${after.status}`,
		);

		await page.close();
		await ctx.close();
	}

	const passed = H.summary("ux_regression_19_23");
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
