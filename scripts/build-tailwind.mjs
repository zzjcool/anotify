#!/usr/bin/env node
/* 生成 web/tailwind.css（Tailwind CLI）。
 * CI / make fe 不跑本脚本（不安装 npm 依赖），只核对已提交的产物。
 * 改了 class 后本地执行：node scripts/build-tailwind.mjs
 */
import { spawnSync } from "node:child_process";
import { existsSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const bin = path.join(root, "node_modules", ".bin", "tailwindcss");
if (!existsSync(bin)) {
	console.error("缺少 tailwindcss。请在仓库根目录执行：npm install");
	process.exit(1);
}

const result = spawnSync(
	bin,
	[
		"-c",
		"tailwind.config.js",
		"-i",
		"web-src/tailwind.input.css",
		"-o",
		"web/tailwind.css",
		"--minify",
	],
	{ cwd: root, stdio: "inherit" },
);
if (result.error) {
	console.error(result.error);
	process.exit(1);
}
process.exit(result.status ?? 1);
