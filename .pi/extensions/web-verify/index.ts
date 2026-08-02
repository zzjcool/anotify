/* Anotify · 前端自我验证扩展
 * 注册一个 web_verify 工具：用 Playwright(headless Chromium) 打开本地 HTML/URL，
 * 自动检查渲染、console/页面错误、请求失败、滚动到底、元素溢出，并截图。
 *
 * 依赖：npm install playwright-core （浏览器用系统 Chrome）
 */
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";

export default function (pi: ExtensionAPI) {
	pi.registerTool({
		name: "web_verify",
		label: "Web Verify",
		description:
			"用无头浏览器打开一个本地 HTML 文件或 URL，验证前端是否正常渲染。检查 console/页面错误、网络请求失败、页面能否滚动到底部、是否有元素溢出视口，并截图保存。适合验证纯 HTML/Tailwind/shadcn 页面。",
		parameters: Type.Object({
			target: Type.String({
				description:
					"要验证的本地 HTML 文件路径（如 design/tech-scheme.html）或完整 URL（http://...）",
			}),
			title: Type.Optional(
				Type.String({
					description: "要验证出现的标题文本（存在性检查）",
					examples: ["技术方案"],
				}),
			),
			viewport: Type.Optional(
				Type.Object(
					{
						width: Type.Optional(Type.Number({ default: 1280 })),
						height: Type.Optional(Type.Number({ default: 800 })),
					},
					{ description: "视口尺寸，默认 1280x800" },
				),
			),
			screenshot: Type.Optional(
				Type.Boolean({ default: true, description: "是否截图保存" }),
			),
		}),
		async execute(toolCallId, params, signal, onUpdate, ctx) {
			const { chromium } = await import("playwright-core");
			const fs = await import("node:fs");
			const path = await import("node:path");

			// 解析目标：相对路径基于 cwd
			let url = params.target;
			const looksLikeUrl = /^https?:\/\//i.test(url);
			if (!looksLikeUrl) {
				const abs = path.resolve(ctx.cwd, url);
				if (!fs.existsSync(abs)) {
					return {
						content: [{ type: "text", text: `❌ 文件不存在: ${abs}` }],
						details: {},
					};
				}
				url = "file://" + abs;
			}

			const width = params.viewport?.width ?? 1280;
			const height = params.viewport?.height ?? 800;

			// 选择浏览器：优先系统 Chrome
			let browser;
			try {
				browser = await chromium.launch({
					channel: "chrome",
					headless: true,
					args: ["--no-sandbox"],
				});
			} catch (e) {
				try {
					browser = await chromium.launch({ headless: true });
				} catch (e2) {
					return {
						content: [
							{
								type: "text",
								text: "❌ 找不到可用浏览器。请运行 `npx playwright-core install chromium` 安装，或用系统 Chrome。",
							},
						],
						details: { error: String(e2) },
					};
				}
			}

			const page = await browser.newPage({ viewport: { width, height } });

			const consoleErrors = [];
			const pageErrors = [];
			const failedRequests = [];

			page.on("console", (msg) => {
				if (msg.type() === "error") consoleErrors.push(msg.text());
			});
			page.on("pageerror", (err) => pageErrors.push(String(err)));
			page.on("requestfailed", (req) => {
				failedRequests.push(
					`${req.method()} ${req.url()} → ${req.failure()?.errorText}`,
				);
			});

			let status = "ok";
			let httpStatus = null;
			let title = null;
			try {
				const resp = await page.goto(url, {
					waitUntil: "load",
					timeout: 30000,
				});
				httpStatus = resp ? resp.status() : null;
				if (resp && resp.status() >= 400) status = "http-error";
				// 等渲染完成
				await page.waitForTimeout(1200);
				title = await page.title();
			} catch (e) {
				status = "nav-error";
				pageErrors.push(String(e));
			}

			const issues = [];

			// 标题存在性
			if (params.title) {
				const found = await page
					.getByText(params.title, { exact: false })
					.first()
					.isVisible()
					.catch(() => false);
				if (!found) issues.push(`标题「${params.title}」未在页面中找到`);
			}

			// 滚动到底部测试
			let scrollable = false;
			let scrollHeight = 0;
			try {
				scrollHeight = await page.evaluate(
					() => document.documentElement.scrollHeight,
				);
				const viewportH = await page.evaluate(() => window.innerHeight);
				scrollable = scrollHeight > viewportH;
				if (scrollable) {
					await page.evaluate(() =>
						window.scrollTo(0, document.documentElement.scrollHeight),
					);
					await page.waitForTimeout(400);
					const bottomY = await page.evaluate(() => window.scrollY);
					const maxY = scrollHeight - viewportH;
					if (bottomY < maxY - 5) {
						issues.push(
							`页面无法滚动到底部（当前 ${Math.round(bottomY)}px / 最大 ${Math.round(maxY)}px）`,
						);
					}
				}
			} catch (e) {
				issues.push("滚动测试异常: " + e);
			}

			// 元素溢出视口检测
			const overflowEls = await page
				.evaluate(() => {
					const out = [];
					const vw = window.innerWidth;
					for (const el of document.querySelectorAll("body *")) {
						const r = el.getBoundingClientRect();
						if (
							r.right > vw + 5 &&
							!el.closest("pre") &&
							!el.closest(".overflow-x-auto")
						) {
							const c =
								el.className && typeof el.className === "string"
									? el.className
									: el.tagName;
							out.push({
								tag: el.tagName,
								cls: c.slice(0, 60),
								right: Math.round(r.right),
								vw,
							});
						}
					}
					return out.slice(0, 8);
				})
				.catch(() => []);

			// 截图
			let screenshotPath = null;
			if (params.screenshot !== false) {
				// 回到顶部截图
				await page.evaluate(() => window.scrollTo(0, 0));
				await page.waitForTimeout(300);
				const shotDir = path.join(ctx.cwd, ".pi", "web-verify-shots");
				fs.mkdirSync(shotDir, { recursive: true });
				screenshotPath = path.join(shotDir, `${Date.now()}.png`);
				await page.screenshot({ path: screenshotPath, fullPage: true });
			}

			await browser.close();

			const results = {
				target: url,
				status,
				httpStatus,
				title,
				viewport: `${width}x${height}`,
				scrollable,
				scrollHeight,
				consoleErrors,
				pageErrors,
				failedRequests,
				overflowElements: overflowEls,
				screenshotPath,
			};

			const lines = [
				`## web_verify 结果`,
				``,
				`- **目标**: ${url}`,
				`- **HTTP 状态**: ${httpStatus ?? "N/A"}`,
				`- **页面标题**: ${title ?? "（空）"}`,
				`- **加载状态**: ${status}`,
				`- **可滚动**: ${scrollable} (总高 ${scrollHeight}px)`,
				``,
				`### 发现的问题 (${issues.length})`,
				issues.length
					? issues.map((i) => `- ⚠️ ${i}`).join("\n")
					: `- ✅ 无问题`,
				``,
				`### Console 错误 (${consoleErrors.length})`,
				consoleErrors.length
					? consoleErrors.map((c) => `- ${c}`).join("\n")
					: `- ✅ 无`,
				``,
				`### 页面 JS 错误 (${pageErrors.length})`,
				pageErrors.length
					? pageErrors.map((c) => `- ${c}`).join("\n")
					: `- ✅ 无`,
				``,
				`### 失败请求 (${failedRequests.length})`,
				failedRequests.length
					? failedRequests.map((c) => `- ${c}`).join("\n")
					: `- ✅ 无`,
				``,
				`### 视口溢出元素 (${overflowEls.length})`,
				overflowEls.length
					? overflowEls
							.map(
								(o) =>
									`- <${o.tag}> ${o.cls} 超出视口 (right=${o.right}px > vw=${o.vw}px)`,
							)
							.join("\n")
					: `- ✅ 无`,
				screenshotPath ? `\n📸 截图已保存: ${screenshotPath}` : "",
			];

			return {
				content: [{ type: "text", text: lines.filter(Boolean).join("\n") }],
				details: results,
			};
		},
	});
}
