/** @type {import('tailwindcss').Config} */
// 构建期 Tailwind。线上 / embed 页面用产物 web/tailwind.css，
// 不加载 cdn.tailwindcss.com（Play CDN 会在控制台打出 production 警告）。
// 改了 web-src 或 web/partials.js 里的工具类后：node scripts/build-tailwind.mjs
module.exports = {
	content: ["./web-src/**/*.html", "./web/partials.js", "./web/sw.js"],
	theme: {
		extend: {},
	},
	plugins: [],
};
