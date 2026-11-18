#!/usr/bin/env node
/* affected.mjs — 按改动文件选择「要跑的 Go 包 / e2e 套件」并（可选）执行它们
 *
 * 为什么：CI 与本地 agent 反复跑 `make ci` + 全量 e2e，而一页 HTML 的改动与
 * 一个协议帧的改动风险面完全不同。本脚本把「这次到底需要跑什么」算出来，
 * 并且**必须**把「为什么选它 / 跳过了谁 / 为什么跳过」全部打印出来 ——
 * 静默少跑等于静默降低覆盖，那比慢更贵。
 *
 * 三条不可协商的判据：
 *   ① fail-closed：映射表（affected-map.mjs）里没登记的路径 ⇒ 全量。表腐烂的
 *      代价是**多跑**，不是漏跑。
 *   ② 全量必须永远可达：`PI_E2E_FULL=1` 或 `--full` 强制全跑，CI 的 nightly 用它。
 *   ③ Go 包用 import 图反向闭包（`go list`），不拍脑袋；e2e 套件用显式表，
 *      并用 `affected.test.mjs` 反向对账套件源码里的页面字面量。
 *
 * 用法：
 *   node scripts/e2e/affected.mjs                     # 打印计划（默认）
 *   node scripts/e2e/affected.mjs --run               # 跑 Go 测试 + 选中的 e2e 套件
 *   node scripts/e2e/affected.mjs --run-go            # 只跑 Go 包测试
 *   node scripts/e2e/affected.mjs --run-e2e           # 只跑选中的 e2e 套件
 *   node scripts/e2e/affected.mjs --full              # 强制全量（等价 PI_E2E_FULL=1）
 *   node scripts/e2e/affected.mjs --base origin/main  # 自定义基准（默认 merge-base）
 *   node scripts/e2e/affected.mjs --files a.html,b.go # 显式给改动集合（CI / 测试用）
 *   node scripts/e2e/affected.mjs --json              # 机器可读（CI 用）
 *
 * 环境变量：
 *   PI_E2E_FULL=1        强制全量（覆盖一切选择逻辑）
 *   AFFECTED_BASE=<ref>  同 --base
 *   AFFECTED_FILES=...   同 --files（换行分隔）
 */
import { execFileSync, spawnSync } from "node:child_process";
import { readdirSync, readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";
import path from "node:path";

import {
	FULL_SURFACES,
	NO_E2E_IMPACT,
	SHARED_CHROME_SURFACES,
	SUITE_SURFACES,
} from "./affected-map.mjs";

const ROOT = path.resolve(path.dirname(new URL(import.meta.url).pathname), "../..");

// ---------------------------------------------------------------- glob（无依赖）
/** 段级 glob：`**` 跨目录，`*` 段内任意，其余字面量。 */
export function globMatch(pattern, file) {
	const ps = pattern.split("/").filter(Boolean);
	const fs = file.split("/").filter(Boolean);
	const walk = (i, j) => {
		if (i === ps.length) return j === fs.length;
		if (ps[i] === "**") {
			// `**` 吞 0..n 段
			for (let k = j; k <= fs.length; k++) if (walk(i + 1, k)) return true;
			return false;
		}
		if (j >= fs.length) return false;
		if (!segmentMatch(ps[i], fs[j])) return false;
		return walk(i + 1, j + 1);
	};
	return walk(0, 0);
}

function segmentMatch(pat, seg) {
	if (pat === "*") return true;
	if (!pat.includes("*")) return pat === seg;
	const parts = pat.split("*");
	let rest = seg;
	if (!rest.startsWith(parts[0])) return false;
	rest = rest.slice(parts[0].length);
	for (let i = 1; i < parts.length; i++) {
		const p = parts[i];
		if (p === "") continue;
		const at = rest.indexOf(p);
		if (at < 0) return false;
		rest = rest.slice(at + p.length);
	}
	return pat.endsWith("*") || rest === "";
}

// ---------------------------------------------------------------- 基础事实
/** 已注册套件 = suites/ 下的文件（文件系统是唯一真源，防止表里凭空多一行）。 */
export function registeredSuites(root = ROOT) {
	return readdirSync(path.join(root, "scripts/e2e/suites"))
		.filter((f) => f.endsWith(".mjs"))
		.map((f) => f.slice(0, -4))
		.sort();
}

/** 站点页面名 = web-src/pages/*.html（新增页面没人跑 ⇒ 由测试发现）。 */
export function sitePages(root = ROOT) {
	return readdirSync(path.join(root, "web-src/pages"))
		.filter((f) => f.endsWith(".html"))
		.map((f) => f.slice(0, -5))
		.sort();
}

const CHROME_CLASS_HINT = undefined; // 已废弃：Chrome 类由 chromeSuites() 从注册表减出来

/** HTTP 类套件（无 Chrome）——与 parallel_runner.mjs 的 HTTP_SUITES 同源，由 affected.test.mjs 对账。 */
export const HTTP_SUITES = [
	"api_contract", "contract", "ws_protocol", "routing", "persistence", "security",
	"edge_cases", "passkeys", "capacity", "orphans", "deploy", "live_agent", "node_v4",
];

/** 只吃节点二进制的套件：`cmd/anode/**` 改动时它们的风险面。 */
export const ANODE_ONLY_SUITES = ["deploy", "live_agent", "node_v4"];

/**
 * 执行顺序 = 注册表顺序（HTTP 类在前，Chrome 类按 parallel_runner 的声明顺序）。
 *
 * 为什么不是字母序：Chrome 类走信号量（并发 4），**谁先拿到信号量谁先跑**。
 * 全量运行时 i18n/i18n_coverage/frontend 先上，四个 f3 矩阵套件被"错峰"开；
 * 如果子集按字母序（frontend, i18n, i18n_coverage, ui_shift 一起开始），
 * 四个多进程 Chrome 套件同时抢 8 核 —— 实测 ui_shift 会撞到自己的 300s 预算
 * （全量跑里从没超过 256s）。按注册表顺序跑 = 与全量完全相同的调度形状。
 */
export function registryOrder(root = ROOT) {
	try {
		const src = readFileSync(path.join(root, "scripts/e2e/parallel_runner.mjs"), "utf8");
		const pick = (name) => {
			const m = src.match(new RegExp(`const ${name} = \\[([\\s\\S]*?)\\];`));
			if (!m) return [];
			return [...m[1].matchAll(/"([^"]+)"/g)].map((x) => x[1]);
		};
		const order = [...pick("HTTP_SUITES"), ...pick("CHROME_SUITES")];
		if (order.length > 0) return order;
	} catch {
		/* 落到下面的兜底 */
	}
	// 兜底：字母序的已注册套件（root 里连 suites/ 都没有时退到本仓库）
	try {
		return registeredSuites(root);
	} catch {
		return registeredSuites();
	}
}

/** 按执行顺序排列给定的套件（未知的排最后，保持不丢）。 */
export function inRunOrder(suites, order = registryOrder()) {
	const rank = new Map(order.map((s, i) => [s, i]));
	return [...suites].sort((a, b) => (rank.get(a) ?? 1e9) - (rank.get(b) ?? 1e9) || a.localeCompare(b));
}

/** 全部 Chrome 类套件（= 已注册 - HTTP 类）。 */
export function chromeSuites(suites = registeredSuites()) {
	const http = new Set(HTTP_SUITES);
	return suites.filter((s) => !http.has(s));
}

// ---------------------------------------------------------------- 选择（纯函数）
/**
 * 纯函数：不碰 git / go，输入 = 改动文件清单，输出 = 选择结果。
 * 这样才能被 affected.test.mjs 用「删掉表里一行」的方式反证。
 */
export function selectE2E({ files, suites, table, pages = [] }) {
	const { SUITE_SURFACES: surfaces, FULL_SURFACES: fulls, SHARED_CHROME_SURFACES: shared, NO_E2E_IMPACT: noImpact } = table;
	const chrome = new Set(chromeSuites(suites));
	const knownPages = new Set(pages);

	const picked = new Map(); // suite -> [why]
	// 只有**表里有行**的套件能被选中：删掉一行就等于「它不再由改动驱动」，
	// 而「有没有行」由 affected.test.mjs 的完整性用例兜住（不许静默永不选中）。
	const hit = (s, why) => {
		if (!surfaces[s]) return;
		if (!picked.has(s)) picked.set(s, []);
		const arr = picked.get(s);
		if (!arr.includes(why)) arr.push(why);
	};
	const ignored = [];
	const unknown = [];
	let full = null; // {why}

	for (const file of files) {
		// ① 共享面优先于白名单：两者都命中时按**更保守**的那一头算。
		//    实例：scripts/check-tmp-leaks.mjs 既像"检查脚本"（白名单）又是每个 e2e 的
		//    外层包装（共享面）—— 判成"无影响"就会漏掉一次真实的运行期变化。
		const fu = fulls.find((r) => globMatch(r.glob, file));
		if (fu) {
			full = full || { why: `${file} 落在共享面（${fu.why}）` };
			continue;
		}
		// ② 白名单：明确没有 e2e 影响的路径
		const ni = noImpact.find((r) => globMatch(r.glob, file));
		if (ni) {
			ignored.push({ file, why: ni.why });
			continue;
		}
		// ③ 页面：`web-src/pages/<p>.html` 与生成物 `web/<p>.html`
		const page = pageOf(file);
		if (page) {
			if (!knownPages.has(page)) {
				// 页面名不在站点页面清单里（新增页/改名/打错）⇒ 矩阵类套件的 PAGES 常量
				// 未必包含它，不能假定「ALL 套件会跑到」。
				unknown.push({ file, why: `页面 ${page} 不在 web-src/pages 清单里，无法判定谁覆盖它` });
				continue;
			}
			const owners = suites.filter((s) => {
				const row = surfaces[s];
				if (!row) return false;
				return row.pages === "ALL" || (Array.isArray(row.pages) && row.pages.includes(page));
			});
			if (owners.length === 0) {
				// 页面没有任何套件登记 ⇒ 不能判定它有没有被别的套件间接触达
				unknown.push({ file, why: `页面 ${page} 在所有套件的映射面里都没登记` });
				continue;
			}
			for (const s of owners) {
				const row = surfaces[s];
				hit(s, `${file} 是它访问的页面${row.pages === "ALL" ? "（矩阵遍历全部页面）" : ""}`);
			}
			continue;
		}
		// ④ 单套件自身：改套件只看它自己
		const suiteSelf = file.match(/^scripts\/e2e\/suites\/([^/]+)\.mjs$/);
		if (suiteSelf) {
			if (!suites.includes(suiteSelf[1])) {
				unknown.push({ file, why: `套件 ${suiteSelf[1]} 未注册（suites/ 里没有它）` });
			} else {
				hit(suiteSelf[1], `${file} 是它自己的套件源码`);
			}
			continue;
		}
		// ⑤ fixtures：f3 驱动面 → f3 系；其余（CDN 替身等）→ 全部 Chrome
		if (file.startsWith("scripts/e2e/fixtures/f3/")) {
			const f3 = suites.filter((s) => s === "frontend" || s.startsWith("i18n") || s === "ui_shift" || s === "ux_regression_19_23");
			if (f3.length === 0) unknown.push({ file, why: "f3 fixtures 没有对应套件" });
			for (const s of f3) hit(s, `${file} 是 f3 驱动（f3 系共用）`);
			continue;
		}
		if (file.startsWith("scripts/e2e/fixtures/")) {
			for (const s of chrome) hit(s, `${file} 是 Chrome 套件共用的外部资源替身`);
			continue;
		}
		// ⑥ 共享 Chrome 运行期资源
		const sh = shared.find((r) => globMatch(r.glob, file));
		if (sh) {
			if (chrome.size === 0) unknown.push({ file, why: "没有 Chrome 类套件可命中" });
			for (const s of chrome) hit(s, `${file} 是页面共用资源（${sh.why}）`);
			continue;
		}
		// ⑦ Go / 其它交给 goPlan，这里只记「不是 e2e 判定的输入」
		if (file.endsWith(".go") || /^(internal|cmd)\//.test(file)) continue;
		// ⑧ 不认识的路径 ⇒ 不能判定 ⇒ 全量
		unknown.push({ file, why: "映射表里没有这条路径的分类" });
	}

	const selectedSuites = full || unknown.length > 0 ? [...suites] : suites.filter((s) => picked.has(s));
	return {
		full: full ? full.why : unknown.length > 0 ? `无法判定：${unknown.map((u) => u.file).join(", ")}` : null,
		selected: selectedSuites.sort(),
		reasons: Object.fromEntries([...picked.entries()].map(([s, w]) => [s, w])),
		skipped: suites
			.filter((s) => !selectedSuites.includes(s))
			.map((s) => ({
				suite: s,
				// 跳过理由要能直接回答「为什么它不需要跑」：说出它盯的面，让人自己判断表对不对
				why: `未触及它的映射面（${describeSurface(surfaces[s])}）`,
			})),
		ignored,
		unknown,
	};
}

function describeSurface(row) {
	if (!row) return "映射表里没有这一行";
	const bits = [];
	if (row.pages === "ALL") bits.push("全部页面");
	else if (Array.isArray(row.pages) && row.pages.length) bits.push(`页面 ${row.pages.join("/")}`);
	if (Array.isArray(row.paths) && row.paths.length) bits.push(`路径 ${row.paths.join(" ")}`);
	return bits.join("；") || "（空）";
}

/** `web-src/pages/x.html` / `web/x.html` → `x`；其余 → null。 */
export function pageOf(file) {
	let m = file.match(/^web-src\/pages\/([a-z0-9-]+)\.html$/);
	if (m) return m[1];
	m = file.match(/^web\/([a-z0-9-]+)\.html$/);
	if (m) return m[1];
	return null;
}

// ---------------------------------------------------------------- Go 包（import 图）
/** 找 go：PATH 优先；缺失时退到 /usr/local/go/bin（本机装法）。找不到会抛错（不静默降级）。 */
export function findGo() {
	const candidates = [process.env.GO, "go", "/usr/local/go/bin/go"].filter(Boolean);
	for (const c of candidates) {
		const r = spawnSync(c, ["env", "GOVERSION"], { encoding: "utf8" });
		if (r.status === 0) return c;
	}
	throw new Error("找不到 go（PATH / $GO / /usr/local/go/bin 都没有）；affected 需要它读 import 图");
}

function goList() {
	const out = execFileSync(
		findGo(),
		["list", "-f", "{{.Dir}}|{{.ImportPath}}|{{join .Deps \" \"}}|{{join .TestImports \" \"}}|{{join .XTestImports \" \"}}", "./..."],
		{ cwd: ROOT, encoding: "utf8", maxBuffer: 64 << 20 },
	);
	const pkgs = [];
	for (const line of out.split("\n")) {
		if (!line.trim()) continue;
		const [dir, importPath, deps = "", testImports = "", xtestImports = ""] = line.split("|");
		pkgs.push({
			dir,
			importPath,
			// prodDeps 决定「二进制变没变」；testEdges 只影响「哪些包的测试要跑」。
			// 两者混在一起会把「测试专用 helper 改了」也算成「中枢二进制变了」→ 全量，
			// 那就是用过度保守掩护不精确。
			prodDeps: deps.split(" ").filter(Boolean),
			testEdges: [...testImports.split(" "), ...xtestImports.split(" ")].filter(Boolean),
		});
	}
	return pkgs;
}

const MODULE = "github.com/zzjcool/anotify";

/**
 * Go 侧计划（纯函数，输入是 goList() 的结果，便于测试）：
 *   受影响的**测试包** = 反向闭包（谁 import 了被改包 → 谁的测试也要跑）
 *   受影响的**二进制** = 被改包出现在哪个 cmd/ 闭包里 → 决定 e2e 风险面
 */
export function goPlan({ files, pkgs, root = ROOT }) {
	const byDir = [...pkgs].sort((a, b) => b.dir.length - a.dir.length);
	const direct = new Map(); // 直接被改的包 -> why
	const binaries = new Map(); // 'server'|'anode'|'sitegen'|'fe-preview'|'devseed' -> why
	let allPackages = null;

	const short = (p) => p.replace(`${MODULE}/`, "");

	const binaryOf = (importPath) => {
		const p = short(importPath);
		if (p === "cmd/server") return "server";
		if (p === "cmd/anode") return "anode";
		if (p === "cmd/sitegen" || p === "internal/sitegen") return "sitegen";
		if (p === "cmd/fe-preview") return "fe-preview";
		if (p === "cmd/devseed") return "devseed";
		return null;
	};

	if (files.some((f) => /^go\.(mod|sum)$/.test(f))) {
		allPackages = "go.mod/go.sum 变了（依赖图整体变化）";
	}

	for (const file of files) {
		if (/^go\.(mod|sum)$/.test(file)) continue;
		const abs = path.join(root, file);
		// 目录归属（不只是 *.go）：schema.sql / 测试夹具 / 嵌入资源都在包目录里
		const owner = byDir.find((p) => abs === p.dir || abs.startsWith(p.dir + path.sep));
		if (!owner) {
			if (file.endsWith(".go")) allPackages = allPackages || `${file} 不属于任何已知包`;
			continue;
		}
		if (!direct.has(owner.importPath)) direct.set(owner.importPath, `${file} 属于 ${short(owner.importPath)}`);
	}

	if (allPackages) {
		return { allPackages, packages: pkgs.map((p) => p.importPath).sort(), binaries, direct };
	}

	/** 反向闭包：`edgeOf(pkg)` 里出现被改包的下游，迭代到不再增长。 */
	const closure = (edgeOf) => {
		const seen = new Map(direct); // importPath -> why
		const byEdge = new Map(pkgs.map((p) => [p.importPath, edgeOf(p)]));
		let grew = true;
		while (grew) {
			grew = false;
			for (const p of pkgs) {
				if (seen.has(p.importPath)) continue;
				for (const e of byEdge.get(p.importPath) || []) {
					if (seen.has(e)) {
						seen.set(p.importPath, `反向闭包：${short(e)}（被改/下游）`);
						grew = true;
						break;
					}
				}
			}
		}
		return seen;
	};

	// ① 要跑测试的包：生产依赖 ∪ 测试专用依赖（测试 helper 变了，用它的测试也要重跑）
	const forTests = closure((p) => [...p.prodDeps, ...p.testEdges]);
	// ② 二进制有没有变：只看生产依赖。测试专用 helper 不进二进制，不该把 e2e 拉成全量。
	const prodSeen = closure((p) => p.prodDeps);

	for (const p of prodSeen.keys()) {
		const b = binaryOf(p);
		if (b && !binaries.has(b)) binaries.set(b, short(p));
	}

	return { allPackages: null, packages: [...forTests.keys()].sort(), binaries, direct };
}

/** e2e 风险面 ← 哪些二进制变了（机械推导，不逐包拍脑袋）。 */
export function suitesForBinaries(binaries, suites) {
	const chrome = chromeSuites(suites);
	const picks = new Map();
	const add = (s, why) => {
		if (!picks.has(s)) picks.set(s, []);
		if (!picks.get(s).includes(why)) picks.get(s).push(why);
	};
	if (binaries.has("server") || binaries.has("devseed")) {
		const which = binaries.has("server") ? "中枢（cmd/server）" : "devseed（播种）";
		for (const s of suites) add(s, `${which} 二进制变了 → 每个套件都起它`);
	}
	if (binaries.has("sitegen") || binaries.has("fe-preview")) {
		for (const s of chrome) add(s, "站点生成 / 预览变了 → web/*.html 与 dist 变了");
	}
	if (binaries.has("anode")) {
		for (const s of ANODE_ONLY_SUITES) {
			if (suites.includes(s)) add(s, "anode 二进制变了（只有跑真节点的套件受影响）");
		}
	}
	return picks;
}

// ---------------------------------------------------------------- git 改动集合
function git(args, opts = {}) {
	return execFileSync("git", args, { cwd: ROOT, encoding: "utf8", ...opts });
}

export function changedFiles({ base } = {}) {
	const ref = base || process.env.AFFECTED_BASE;
	const tip = ref || "origin/main";
	let mergeBase = null;
	try {
		mergeBase = git(["merge-base", "HEAD", tip]).trim();
	} catch {
		mergeBase = null;
	}
	const sets = [];
	if (mergeBase) {
		sets.push(git(["diff", "--name-only", "-z", `${mergeBase}...HEAD`]));
	} else if (ref || process.env.AFFECTED_BASE) {
		// 显式给了基准却算不出 merge-base：这是**配置错误**，不许静默退化成 HEAD~1
		// （在 shallow clone 里 HEAD~1 也不存在，退化只会换一种更难懂的方式红）。
		throw new Error(
			`算不出与 ${tip} 的 merge-base（浅克隆？）—— 在 CI 里给 checkout 加 fetch-depth: 0，` +
				`或显式用 --files 传改动集合`,
		);
	} else {
		try {
			sets.push(git(["diff", "--name-only", "-z", "HEAD~1", "HEAD"], { stdio: ["ignore", "pipe", "ignore"] }));
		} catch {
			throw new Error("没有 origin/main 也没有 HEAD~1：无法确定改动范围（用 --files 显式给）");
		}
	}
	// 未提交的改动也算（agent 边改边验证的最常见形态）
	sets.push(git(["diff", "--name-only", "-z", "HEAD"]));
	sets.push(git(["diff", "--cached", "--name-only", "-z"]));
	sets.push(git(["ls-files", "--others", "--exclude-standard", "-z"]));
	const files = new Set();
	for (const s of sets) for (const f of s.split("\0")) if (f) files.add(f);
	return { base: mergeBase ? `${mergeBase.slice(0, 8)}（merge-base with ${ref || "origin/main"}）` : "HEAD~1", files: [...files].sort() };
}

// ---------------------------------------------------------------- CLI
function parseArgs(argv) {
	const out = { run: false, runGo: false, runE2e: false, full: false, json: false, noGo: false, files: null, base: null };
	for (let i = 0; i < argv.length; i++) {
		const a = argv[i];
		if (a === "--run") out.run = true;
		else if (a === "--run-go") out.runGo = true;
		else if (a === "--run-e2e") out.runE2e = true;
		else if (a === "--full" || a === "-f") out.full = true;
		else if (a === "--json") out.json = true;
		else if (a === "--no-go") out.noGo = true;
		else if (a === "--base") out.base = argv[++i];
		else if (a.startsWith("--base=")) out.base = a.slice(7);
		else if (a === "--files") out.files = argv[++i].split(",").map((s) => s.trim()).filter(Boolean);
		else if (a.startsWith("--files=")) out.files = a.slice(8).split(",").map((s) => s.trim()).filter(Boolean);
		else if (a === "-h" || a === "--help") out.help = true;
		else throw new Error(`未知参数 ${a}`);
	}
	return out;
}

function printPlan({ baseLabel, files, suites, e2e, go }) {
	const total = suites.length;
	const ran = e2e.selected.length;
	console.log("");
	console.log("==========================================");
	console.log(" 改动自适应选择（affected）");
	console.log("==========================================");
	console.log(`  基准: ${baseLabel}`);
	console.log(`  改动文件: ${files.length} 个`);
	if (files.length <= 20) for (const f of files) console.log(`    · ${f}`);
	else {
		for (const f of files.slice(0, 20)) console.log(`    · ${f}`);
		console.log(`    … 另有若干文件省略（共 ${files.length} 个）`);
	}

	console.log("");
	if (e2e.full) {
		console.log(`  ⚠️ 全量 e2e（${ran}/${total} 套）：${e2e.full}`);
	} else {
		console.log(`  e2e 套件: 跑 ${ran} 套 / 跳过 ${total - ran} 套（共 ${total} 套）`);
		for (const s of e2e.selected) {
			const why = (e2e.reasons[s] || []).join("；") || e2e.full || "（二进制风险面）";
			console.log(`    ✅ ${s.padEnd(20)} ← ${why}`);
		}
		if (e2e.skipped.length) {
			console.log("    ── 跳过的套件与原因 ──");
			for (const s of e2e.skipped) console.log(`    ⏭  ${s.suite.padEnd(20)} ← ${s.why}`);
		}
	}
	if (e2e.ignored.length) {
		console.log("    ── 明确没有 e2e 影响的改动 ──");
		for (const i of e2e.ignored) console.log(`    ○  ${i.file} ← ${i.why}`);
	}
	if (e2e.unknown.length) {
		console.log("    ── 无法判定 ⇒ 保守跑全套 ──");
		for (const u of e2e.unknown) console.log(`    ?  ${u.file} ← ${u.why}`);
	}

	console.log("");
	if (go.allPackages) {
		console.log(`  Go 包: 全部 ${go.packages.length} 个（${go.allPackages}）`);
	} else {
		console.log(`  Go 包: ${go.packages.length} 个（反向闭包）`);
		for (const p of go.packages) console.log(`    · ${p.replace(`${MODULE}/`, "")}`);
	}

	console.log("");
	console.log("  强制全量: PI_E2E_FULL=1 或 --full（CI 的 nightly 用它，别让选择逻辑成为唯一出口）");
	console.log("==========================================");
	console.log("");
}

function runGoTests(packages, allPackages) {
	// 空列表必须短路：`go test` 不带包参数会退回「当前目录的包」——那既不是全量
	// 也不是空集，而是一个没人打算跑的包（本仓根目录还没有 .go 文件，会直接报错）。
	if (!allPackages && packages.length === 0) {
		console.log("▶ 没有需要重跑的 Go 包，跳过 go test（改动只落在非 Go 面上）");
		return 0;
	}
	const targets = allPackages ? ["./..."] : packages;
	const args = ["test", ...targets, "-count=1", "-shuffle=on"];
	console.log(`▶ go ${args.join(" ")}`);
	const r = spawnSync("go", args, { cwd: ROOT, stdio: "inherit" });
	return r.status ?? 1;
}

function runE2E(suites) {
	if (suites.length === 0) {
		console.log("▶ 没有选中的 e2e 套件，跳过（跳过原因见上面的清单）");
		return 0;
	}
	// **显式 --parallel**：带套件名参数时执行器默认串行（为 e2e-one 兼容），
	// 多套件串行会把「少跑几套」的收益吃掉一半。
	// 顺序按注册表（见 registryOrder 的注释：信号量是按提交顺序发的）。
	const args = ["./scripts/e2e/run_all.sh", "--parallel", ...inRunOrder(suites)];
	console.log(`▶ ${args.join(" ")}`);
	const r = spawnSync("bash", args, { cwd: ROOT, stdio: "inherit" });
	return r.status ?? 1;
}

async function main() {
	const args = parseArgs(process.argv.slice(2));
	if (args.help) {
		console.log(
			[
				"用法: node scripts/e2e/affected.mjs [--run|--run-go|--run-e2e] [--full] [--json]",
				"                                  [--base <ref>] [--files a,b]",
				"",
				"PI_E2E_FULL=1 强制全量；AFFECTED_BASE / AFFECTED_FILES 是等价环境变量。",
			].join("\n"),
		);
		return 0;
	}

	const suites = registeredSuites();
	const envFiles = process.env.AFFECTED_FILES
		? process.env.AFFECTED_FILES.split("\n").map((s) => s.trim()).filter(Boolean)
		: null;
	let files;
	let baseLabel;
	if (args.files || envFiles) {
		files = args.files || envFiles;
		baseLabel = "显式给定（--files / AFFECTED_FILES）";
	} else {
		const c = changedFiles({ base: args.base });
		files = c.files;
		baseLabel = c.base;
	}

	const forceFull = args.full || process.env.PI_E2E_FULL === "1";
	if (files.length === 0 && !forceFull) {
		console.log("本次没有任何改动（相对基准为空）⇒ 没有需要跑的东西。");
		console.log("这是「无事可做」，不是「选择器判定全绿」：要跑全量用 PI_E2E_FULL=1。");
		return 0;
	}

	const e2e = forceFull
		? {
				full: forceFull ? "PI_E2E_FULL / --full 强制全量" : null,
				selected: [...suites].sort(),
				reasons: {},
				skipped: [],
				ignored: [],
				unknown: [],
			}
		: selectE2E({
				files,
				suites,
				// 已知页面清单必须一起传：`pages: ALL` 的套件只对**真实存在**的页面成立
				// （它们的矩阵常量是硬编码数组，不是 readdir）。
				pages: sitePages(),
				table: { SUITE_SURFACES, FULL_SURFACES, SHARED_CHROME_SURFACES, NO_E2E_IMPACT },
			});

	// 二进制风险面：并进 e2e 选择（除非已经全量）
	let go;
	if (args.noGo) {
		go = { allPackages: null, packages: [], binaries: new Map(), skipped: "--no-go" };
	} else if (forceFull) {
		go = { allPackages: "PI_E2E_FULL / --full 强制全量", packages: [], binaries: new Map() };
		try {
			const pkgs = goList();
			go.packages = pkgs.map((p) => p.importPath).sort();
		} catch {
			go.packages = [];
		}
	} else {
		go = goPlan({ files, pkgs: goList() });
	}

	if (!e2e.full && !args.noGo && go.binaries.size > 0) {
		const byBin = suitesForBinaries(go.binaries, suites);
		const picked = [...byBin.keys()];
		if (picked.length === suites.length) {
			e2e.full = `二进制风险面：${[...go.binaries.entries()].map(([b, w]) => `${b}←${w}`).join("；")}`;
			e2e.selected = [...suites].sort();
			e2e.skipped = [];
		} else {
			const before = new Set(e2e.selected);
			for (const s of picked) {
				if (!before.has(s)) {
					before.add(s);
					if (!e2e.reasons[s]) e2e.reasons[s] = [];
					e2e.reasons[s].push(...byBin.get(s));
				}
			}
			e2e.selected = [...before].sort();
			e2e.skipped = suites
				.filter((s) => !e2e.selected.includes(s))
				.map((s) => ({ suite: s, why: `未触及它的映射面（${describeSurface(SUITE_SURFACES[s])}）` }));
		}
	}

	if (args.json) {
		console.log(
			JSON.stringify({
				base: baseLabel,
				files,
				// docsOnly：这次改动只碰了「e2e 不读」的东西（CI 用它决定是否整条跳过）。
				// 判据来自 NO_E2E_IMPACT，不是「文件后缀像文档」。
				docsOnly: files.length > 0 && e2e.ignored.length === files.length,
				e2e: { full: e2e.full, selected: e2e.selected, skipped: e2e.skipped, reasons: e2e.reasons },
				go: args.noGo
					? null
					: { allPackages: go.allPackages, packages: go.packages, binaries: [...go.binaries.entries()] },
			}),
		);
	} else {
		printPlan({ baseLabel, files, suites, e2e, go });
	}

	let rc = 0;
	if (args.run || args.runGo) rc = runGoTests(go.packages, go.allPackages) || rc;
	if (args.run || args.runE2e) rc = runE2E(e2e.selected) || rc;
	return rc;
}

/* v8 ignore next 3 —— 作为模块被 import 时不跑 CLI */
const invokedDirectly =
	process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href;
if (invokedDirectly) {
	main()
		.then((rc) => process.exit(rc))
		.catch((err) => {
			console.error("affected.mjs 失败:", err && err.message ? err.message : err);
			process.exit(2);
		});
}
