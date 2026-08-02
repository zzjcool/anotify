import { chromium } from "playwright-core";
const browser = await chromium.launch({
	executablePath:
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	headless: true,
});
const page = await browser.newPage({
	viewport: { width: 1280, height: 800 },
	deviceScaleFactor: 2,
});
await page.goto("http://localhost:5699/design/mockup-home.html", {
	waitUntil: "networkidle",
});

// Locate brand element
const loc = page
	.locator('a[href="#"].flex .font-script.text-2xl')
	.last()
	.or(
		page
			.locator("span.font-script.text-2xl")
			.filter({ hasText: "Anotify" })
			.last(),
	);
// Try to find the brand link (the <a> wrapping logo+text)
const brand = page
	.locator("header a, nav a")
	.filter({ hasText: "Anotify" })
	.first();
const box = await brand.boundingBox();
console.log("brand bbox:", JSON.stringify(box));

// Screenshot the brand link region
await brand.screenshot({
	path: "/Users/zheng/code/anotify/.pi/brand-screenshot.png",
});

// Also full page screenshot
await page.screenshot({
	path: "/Users/zheng/code/anotify/.pi/full-top.png",
	clip: { x: 0, y: 0, width: 1280, height: 160 },
});

await browser.close();
console.log("saved");
