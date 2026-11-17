#!/usr/bin/env node
/* Anotify · 模块约定（AGENTS.md）漂移检查
 *
 * 作用：防止「模块约定文档」与代码慢慢脱节。这类文件的失效方式很安静 ——
 * 代码挪了一行、函数改名，文档里的 `mux.go:421` 就指向了别的东西，
 * 而读它的人（尤其是子 agent）会把错的当成约定照做。本轮已真实发生过两次：
 *   ① 迁移块规则：`baselineLayout()` 硬校验单块，两份模块约定却给出相反结论；
 *   ② 根索引里指向 `internal/broker/`、`api/openapi.yaml`、workspace `.pi/agents/` 的悬空引用。
 *
 * 校验规则（任一失败即非零退出）：
 *   ① 根 `AGENTS.md` 的 §0 索引表：标「已写」的模块约定必须存在；标「待写」的必须**不**存在
 *      （后者防「文件写了但索引没更新」→ 没有子 agent 会去读它）。
 *   ② 根 `AGENTS.md` 里的本地 markdown 链接必须能解析（防悬空指路）。
 *   ③ 每个模块 `AGENTS.md`（非根）：≤ 80 行、前 5 行内有 `as_of:`、所有 `path:line`
 *      引用必须落在真实文件的行范围内。
 *
 * 引用解析顺序：先按「该 AGENTS.md 所在目录」解析（模块约定惯例：写 `mux.go:69`），
 * 再按仓库根解析（写全路径的）。两处都不存在才算悬空。
 *
 * 转义：某一行确实要举例一个不存在的路径时，在该行加 `agents-ref-ignore` 标记。
 *
 * 用法：
 *   node scripts/check-agents.mjs              # 检查本仓库
 *   node scripts/check-agents.mjs --self-test  # 跑脚本自身的红/绿自测（临时目录 fixture）
 *   node scripts/check-agents.mjs --root=<dir> # 检查指定目录（测试/复用）
 */
import { existsSync, readFileSync, readdirSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { join, dirname, resolve, relative, sep } from "node:path";

const MAX_MODULE_LINES = 80;
const SKIP_DIRS = new Set([
	".git", "node_modules", ".pi-subagents", ".e2e-bin", ".tmp", "perf-b5",
	"web", // 生成物，可能带第三方 AGENTS.md
]);

// 只认「有扩展名的文件:行号」，避免把 `http://x:8080` 之类当引用。
const REF_RE = /([A-Za-z0-9_][A-Za-z0-9_./-]*\.(?:go|sql|md|mjs|js|py|yaml|yml|sh|html|css|json)):(\d+)/g;
const LINK_RE = /\[[^\]]*\]\(([^)\s]+)\)/g;

function stripUrls(line) {
	return line.replace(/https?:\/\/\S+/g, "");
}

function walkAgents(dir, out = []) {
	for (const name of readdirSync(dir, { withFileTypes: true })) {
		if (name.isDirectory()) {
			if (SKIP_DIRS.has(name.name)) continue;
			walkAgents(join(dir, name.name), out);
		} else if (name.name === "AGENTS.md") {
			out.push(join(dir, name.name));
		}
	}
	return out;
}

function lineCount(path) {
	const text = readFileSync(path, "utf8");
	// 末尾换行不应多算一行
	return text.length === 0 ? 0 : text.split("\n").length - (text.endsWith("\n") ? 1 : 0);
}

/** 解析 §0 索引表：返回 [{area, path, status}] */
function parseIndex(rootAgents) {
	const lines = readFileSync(rootAgents, "utf8").split("\n");
	const rows = [];
	for (const line of lines) {
		if (!line.startsWith("|")) continue;
		const cells = line.split("|").map((c) => c.trim());
		if (cells.length < 5) continue;
		const [, area, , docCell, status] = cells;
		if (area === "area" || /^-+$/.test(area)) continue;
		const link = docCell.match(/\]\(([^)]+)\)/);
		const inline = docCell.match(/`([^`]+\.md)`/);
		const doc = link ? link[1] : inline ? inline[1] : "";
		if (!doc) continue;
		rows.push({ area, doc, status });
	}
	return rows;
}

function check(root, { report }) {
	let failed = 0;
	const fail = (msg) => {
		failed++;
		report.push(`❌ ${msg}`);
	};
	const ok = (msg) => report.push(`✅ ${msg}`);

	const rootAgents = join(root, "AGENTS.md");
	if (!existsSync(rootAgents)) {
		fail("仓库根缺少 AGENTS.md");
		return failed;
	}

	// ① §0 索引表与真实文件的一致性
	const rows = parseIndex(rootAgents);
	if (rows.length === 0) {
		fail("根 AGENTS.md 的 §0 索引表为空或格式变了（找不到「| area | 目录 | 文档 | 状态 |」行）");
	}
	for (const { area, doc, status } of rows) {
		const exists = existsSync(join(root, doc));
		if (status.includes("已写") && !exists) fail(`§0 索引第${area}行标「已写」，但 ${doc} 不存在`);
		if (status.includes("待写") && exists)
			fail(`§0 索引第${area}行标「待写」，但 ${doc} 已存在 —— 索引没更新，子 agent 不会去读它`);
	}
	if (rows.length > 0) ok(`§0 索引表：${rows.length} 个 area，与模块约定文件一致`);

	// ② 本地链接：根与模块约定都查（模块约定里写链接的相对路径按仓库根解析）
	const checkLinks = (file, rel, text) => {
		let links = 0;
		let bad = 0;
		text.split("\n").forEach((line, i) => {
			if (line.includes("agents-ref-ignore")) return;
			for (const m of stripUrls(line).matchAll(LINK_RE)) {
				const target = m[1].split("#")[0].replace(/^</, "").replace(/>$/, "");
				if (!target || /^[a-z]+:/i.test(target)) continue;
				links++;
				if (!existsSync(resolve(root, target))) {
					fail(`${rel}:${i + 1} 悬空链接 → ${target}`);
					bad++;
				}
			}
		});
		return { links, bad };
	};

	const rootText = readFileSync(rootAgents, "utf8");
	const rootLinks = checkLinks(rootAgents, "AGENTS.md", rootText);
	if (rootLinks.links > 0) ok(`根文档本地链接：${rootLinks.links} 条全部可解析`);

	// ③ 每个模块约定：行数 / as_of / path:line 引用 / 本地链接
	const files = walkAgents(root).filter((f) => f !== rootAgents);
	for (const file of files) {
		const rel = relative(root, file).split(sep).join("/");
		const n = lineCount(file);
		const before = failed;
		if (n > MAX_MODULE_LINES) fail(`${rel}: ${n} 行，超过 ${MAX_MODULE_LINES} 行预算（超了就不会有人读完）`);

		const text = readFileSync(file, "utf8");
		const lines = text.split("\n");
		if (!lines.slice(0, 5).some((l) => l.includes("as_of:")))
			fail(`${rel}: 前 5 行缺少 \`as_of:\`（无法判断这份约定是什么时候校准的）`);

		let refs = 0;
		lines.forEach((line, i) => {
			if (line.includes("agents-ref-ignore")) return;
			for (const m of stripUrls(line).matchAll(REF_RE)) {
				const [, ref, rawLine] = m;
				refs++;
				const want = Number(rawLine);
				const candidates = [resolve(dirname(file), ref), resolve(root, ref)];
				const hit = candidates.find((c) => existsSync(c));
				if (!hit) {
					fail(`${rel}:${i + 1} 引用 ${ref}:${want} —— 文件不存在（按模块目录与仓库根都找不到）`);
					continue;
				}
				const total = lineCount(hit);
				if (want < 1 || want > total)
					fail(`${rel}:${i + 1} 引用 ${ref}:${want} 越界（该文件只有 ${total} 行）`);
			}
		});
		const l = checkLinks(file, rel, text);
		if (failed === before) ok(`${rel}: ${n} 行，${refs} 处引用 + ${l.links} 条链接全部可解析，带 as_of`);
	}
	if (files.length === 0) ok("没有模块级 AGENTS.md（不算失败）");

	return failed;
}

function selfTest() {
	const tmp = join(process.cwd(), ".tmp", "check-agents-selftest");
	rmSync(tmp, { recursive: true, force: true });
	const w = (p, s) => {
		const full = join(tmp, p);
		mkdirSync(dirname(full), { recursive: true });
		writeFileSync(full, s);
	};
	const indexRow = (status) =>
		`| \`m\` | \`internal/m/\` | [\`internal/m/AGENTS.md\`](internal/m/AGENTS.md) | ${status} |\n`;

	// 绿：索引已写 + 文件存在 + 引用可解析
	w("AGENTS.md", `# root\n\n| area | 目录 | 模块约定 | 状态 |\n| --- | --- | --- | --- |\n${indexRow("✅ 已写")}`);
	w("internal/m/a.go", "1\n2\n3\n");
	w("internal/m/AGENTS.md", "as_of: 2026-01-01\n\n参考 `a.go:3`。\n");

	// 红 1：引用越界
	w("internal/m/AGENTS.md", "as_of: 2026-01-01\n\n参考 `a.go:99`。\n");
	// 红 2：悬空文件
	w("internal/m/AGENTS.md", "as_of: 2026-01-01\n\n参考 `nope.go:1`。\n");
	// 红 3：索引说待写但文件存在
	w("internal/m/AGENTS.md", "as_of: 2026-01-01\n");
	w("AGENTS.md", `# root\n\n| area | 目录 | 模块约定 | 状态 |\n| --- | --- | --- | --- |\n${indexRow("待写")}`);

	const quiet = { write: () => {} };
	void quiet;
	const cases = [
		["绿", () => {
			w("AGENTS.md", `# root\n\n| area | 目录 | 模块约定 | 状态 |\n| --- | --- | --- | --- |\n${indexRow("✅ 已写")}`);
			w("internal/m/AGENTS.md", "as_of: 2026-01-01\n\n参考 `a.go:3`。\n");
			return 0;
		}, 0],
		["引用越界", () => {
			w("internal/m/AGENTS.md", "as_of: 2026-01-01\n\n参考 `a.go:99`。\n");
			return 0;
		}, 1],
		["引用文件不存在", () => {
			w("internal/m/AGENTS.md", "as_of: 2026-01-01\n\n参考 `nope.go:1`。\n");
			return 0;
		}, 1],
		["待写却已存在", () => {
			w("AGENTS.md", `# root\n\n| area | 目录 | 模块约定 | 状态 |\n| --- | --- | --- | --- |\n${indexRow("待写")}`);
			w("internal/m/AGENTS.md", "as_of: 2026-01-01\n");
			return 0;
		}, 1],
		["超行数预算", () => {
			w("AGENTS.md", `# root\n\n| area | 目录 | 模块约定 | 状态 |\n| --- | --- | --- | --- |\n${indexRow("✅ 已写")}`);
			w("internal/m/AGENTS.md", "as_of: 2026-01-01\n" + "x\n".repeat(90));
			return 0;
		}, 1],
		["缺 as_of", () => {
			w("internal/m/AGENTS.md", "# no as_of\n");
			return 0;
		}, 1],
	];

	let bad = 0;
	for (const [name, setup, wantFail] of cases) {
		setup();
		const report = [];
		const failed = check(tmp, { report });
		const gotFail = failed > 0 ? 1 : 0;
		if (gotFail !== wantFail) {
			bad++;
			console.error(`❌ self-test「${name}」：期望 fail=${wantFail}，实际 ${gotFail}`);
			console.error(report.join("\n"));
		} else {
			console.log(`✅ self-test「${name}」`);
		}
	}
	rmSync(tmp, { recursive: true, force: true });
	if (bad > 0) {
		console.error(`self-test 失败 ${bad} 项`);
		process.exit(1);
	}
	console.log("self-test 全绿");
}

const args = process.argv.slice(2);
if (args.includes("--self-test")) {
	selfTest();
} else {
	const rootArg = args.find((a) => a.startsWith("--root="));
	const root = resolve(rootArg ? rootArg.slice("--root=".length) : ".");
	const report = [];
	const failed = check(root, { report });
	console.log(report.join("\n"));
	if (failed > 0) {
		console.error(`\n❌ 模块约定漂移：${failed} 处（修完再提交；确属示例的行加 agents-ref-ignore）`);
		process.exit(1);
	}
	console.log("\n✅ 模块约定与代码一致");
}
