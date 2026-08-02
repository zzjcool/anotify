const { chromium } = require("playwright-core");

(async () => {
	const browser = await chromium.launch({
		executablePath:
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		headless: true,
	});
	const page = await browser.newPage({
		viewport: { width: 1440, height: 900 },
	});
	const errors = [];
	page.on("pageerror", (e) => errors.push("PAGEERROR: " + e.message));
	page.on("console", (m) => {
		if (m.type() === "error") errors.push("CONSOLE: " + m.text());
	});

	const url = process.argv[2];
	const mode = process.argv[3];
	await page.goto(url, { waitUntil: "networkidle" });

	if (mode === "security") {
		const r = await page.evaluate(() => {
			const txt = document.body.innerText;
			// Passkey credentials
			const passkeySection = Array.from(document.querySelectorAll("*")).find(
				(el) =>
					el.children.length &&
					/Passkey/i.test(el.textContent) &&
					el.tagName.match(/H[1-6]|DIV|SECTION/),
			);
			return { fullText: txt.slice(0, 200) };
		});
		// Count passkey credential items
		const passkeys = await page.evaluate(() => {
			// find container with 'Passkey' heading, count credential rows
			const headings = Array.from(
				document.querySelectorAll("h1,h2,h3,h4,div,span"),
			).filter(
				(e) =>
					/^Passkey/i.test(e.textContent.trim()) &&
					e.textContent.trim().length < 60,
			);
			let list = [];
			for (const h of headings) {
				const container = h.closest("section,div");
				if (container) {
					const items = container.querySelectorAll(
						'[class*="passkey"], [class*="credential"], li, tr',
					);
					if (items.length) {
						list = Array.from(items).map((i) =>
							i.textContent.trim().slice(0, 80),
						);
						break;
					}
				}
			}
			return list;
		});
		const sessions = await page.evaluate(() => {
			const headings = Array.from(
				document.querySelectorAll("h1,h2,h3,h4,div,span"),
			).filter(
				(e) =>
					/会话|设备|登录/.test(e.textContent) &&
					e.textContent.trim().length < 40,
			);
			for (const h of headings) {
				const container = h.closest("section,div");
				if (container) {
					const items = container.querySelectorAll(
						'[class*="session"], li, tr, [class*="device-row"]',
					);
					if (items.length)
						return Array.from(items).map((i) =>
							i.textContent.trim().slice(0, 60),
						);
				}
			}
			return [];
		});
		const codes = await page.evaluate(() => {
			const codeEls = Array.from(
				document.querySelectorAll(
					'code, [class*="code"], [class*="mono"], .font-mono, [class*="recovery"]',
				),
			).filter((e) => /[•*●✱]|xxxx|•{2,}|\*{2,}/i.test(e.textContent));
			return codeEls.map((e) => e.textContent.trim().slice(0, 40));
		});
		console.log(
			JSON.stringify(
				{ errors, passkeys, sessions, codes, codeCount: codes.length },
				null,
				1,
			),
		);
	}

	if (mode === "devices") {
		// initial state
		const initial = await page.evaluate(() => {
			const t = document.body.innerText;
			return {
				hasPushSub: t.includes("推送订阅"),
				hasWS: t.includes("WebSocket"),
				hasActiveConn: t.includes("活跃连接"),
			};
		});
		// click WebSocket tab
		const wsTab = await page.evaluate(() => {
			const tabs = Array.from(
				document.querySelectorAll('button, [role="tab"], a, div'),
			).filter((e) => e.textContent.trim() === "WebSocket");
			if (tabs.length) {
				tabs[0].click();
				return true;
			}
			return false;
		});
		await page.waitForTimeout(600);
		const afterWS = await page.evaluate(() => {
			const t = document.body.innerText;
			// find 活跃连接 count
			const m = t.match(/活跃连接[\s\S]{0,40}?(\d+)/);
			// streaming feed
			const feed = Array.from(
				document.querySelectorAll(
					'[class*="stream"], [class*="feed"], [class*="log"], [class*="notification-list"]',
				),
			).map((e) => e.children.length);
			return {
				wsTabVisible: t.includes("活跃连接"),
				activeConnMatch: m ? m[1] : null,
				hasStream: /实时通知流|实时|通知流/.test(t),
				feedChildren: feed,
			};
		});
		// click back to 推送订阅
		await page.evaluate(() => {
			const tabs = Array.from(
				document.querySelectorAll('button, [role="tab"], a, div'),
			).filter((e) => e.textContent.trim() === "推送订阅");
			if (tabs.length) tabs[0].click();
		});
		await page.waitForTimeout(400);
		const afterPush = await page.evaluate(() => {
			const t = document.body.innerText;
			const visible = (el) => {
				const r = el.getBoundingClientRect();
				const s = getComputedStyle(el);
				return (
					r.width > 0 &&
					r.height > 0 &&
					s.display !== "none" &&
					s.visibility !== "hidden"
				);
			};
			const wsPanel = Array.from(document.querySelectorAll("*"))
				.filter(
					(e) => e.textContent.includes("活跃连接") && e.children.length < 30,
				)
				.pop();
			const pushVisible = Array.from(document.querySelectorAll("*")).some(
				(e) => e.textContent.includes("推送订阅") && visible(e),
			);
			return {
				wsStillVisible: wsPanel ? visible(wsPanel) : false,
				pushVisible,
			};
		});
		// device tags
		const tags = await page.evaluate(() => {
			const tagEls = Array.from(
				document.querySelectorAll('[class*="tag"], [class*="badge"], span'),
			).filter((e) => ["手机", "电脑", "工作"].includes(e.textContent.trim()));
			return tagEls.map((e) => e.textContent.trim());
		});
		console.log(
			JSON.stringify(
				{ errors, initial, wsTabClicked: wsTab, afterWS, afterPush, tags },
				null,
				1,
			),
		);
	}

	if (mode === "keys") {
		const perm = await page.evaluate(() => {
			const ths = Array.from(document.querySelectorAll("th")).map((e) =>
				e.textContent.trim(),
			);
			const badges = Array.from(
				document.querySelectorAll('td [class*="badge"], td span, td'),
			)
				.filter((e) =>
					/^(上报|接收|上报 · 接收|上报, 接收)$/.test(e.textContent.trim()),
				)
				.map((e) => e.textContent.trim());
			return { headers: ths, badges };
		});
		// click 新建 Key
		const clicked = await page.evaluate(() => {
			const btns = Array.from(document.querySelectorAll("button, a")).filter(
				(e) => /新建\s*Key|新建Key/.test(e.textContent),
			);
			if (btns.length) {
				btns[0].click();
				return true;
			}
			return false;
		});
		await page.waitForTimeout(500);
		const afterClick = await page.evaluate(() => {
			const radios = Array.from(
				document.querySelectorAll('input[type="radio"]'),
			).map((r) => r.closest("label")?.textContent.trim() || r.name || r.value);
			const visibleRadios = Array.from(
				document.querySelectorAll('input[type="radio"]'),
			)
				.filter((r) => {
					const el = r.closest("div,label");
					const rect = el.getBoundingClientRect();
					return rect.width > 0 && rect.height > 0;
				})
				.map((r) => r.closest("label")?.textContent.trim());
			const dialog = document.querySelector(
				'[role="dialog"], [class*="modal"], [class*="dialog"], [class*="sheet"], [class*="drawer"]',
			);
			return { dialogVisible: !!dialog, radios, visibleRadios };
		});
		console.log(JSON.stringify({ errors, perm, clicked, afterClick }, null, 1));
	}

	await browser.close();
})().catch((e) => {
	console.error("FATAL", e.message);
	process.exit(1);
});
