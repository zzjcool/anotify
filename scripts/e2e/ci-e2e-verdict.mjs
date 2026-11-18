#!/usr/bin/env node
/* ci-e2e-verdict.mjs — CI 上 e2e 的「能不能红」由这里裁决，而不是整块 continue-on-error
 *
 * 为什么需要这个脚本：GitHub runner 比开发机慢 2–3 倍，f3 的浏览器矩阵套件在那边会偶发
 * **环境性**失败（超时/渲染进程被关）。对这类失败整块 `continue-on-error: true` 的代价是
 * 「永久红着也没人看」—— 新引入的真失败会混在里面。反过来，一点都不容忍也不现实。
 *
 * 所以判据是**棘轮**（与本仓 check-annotations / check-test-discipline 同款思路）：
 *
 *   ① 失败集 ⊆ scripts/e2e/known-env-failures.json  → 容忍：exit 0，逐条 ::warning
 *   ② 出现清单之外的失败                            → exit 1，job 红，**拦合并**
 *   ③ 清单项这次没失败                              → 只打 ::warning 提示删除（不计红：
 *      偶发失败本来就会时有时无，按"消失即红"会把闸门变成噪音）
 *   ④ 某个套件连 results 都没产出（崩溃/超预算）    → 视为「整套失败」，必须显式登记
 *      `{"suite": X, "case": null}` 才容忍
 *
 * 用法：
 *   node scripts/e2e/ci-e2e-verdict.mjs --results .e2e-bin/results \
 *        --known scripts/e2e/known-env-failures.json [--selected a,b,c]
 *   node scripts/e2e/ci-e2e-verdict.mjs --self-test
 *
 * 退出码：0 = 可以绿；1 = 有清单之外的失败（必须修或显式登记并写理由）；2 = 用法/数据错误。
 */
import { readFileSync, existsSync, readdirSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { spawnSync } from "node:child_process";
import path from "node:path";
import { pathToFileURL } from "node:url";

const ROOT = path.resolve(path.dirname(new URL(import.meta.url).pathname), "../..");

/** 从 `"<case> — <detail>"` 里取 case 名（harness 的 bad() 拼法）。 */
export function caseOf(failure) {
	const s = String(failure);
	const at = s.indexOf(" — ");
	return (at < 0 ? s : s.slice(0, at)).trim();
}

/**
 * 纯函数：给定「各套件的结果」与「已知清单」，裁决能不能绿。
 * 不读文件、不打印 —— 这样自测可以逐条钉住判据。
 */
export function verdict({ results, known }) {
	const entries = known.entries || [];
	const tolerated = []; // 命中清单的失败
	const blocking = []; // 清单之外（或清单里没登记整套失败）
	const stale = []; // 清单里这次没失败 → 建议删

	const match = (suite, caseName) =>
		entries.find((e) => e.suite === suite && (e.case ?? null) === caseName);

	for (const r of results) {
		if (r.missing) {
			// 连 results/<suite>.json 都没有：崩溃/被 SIGKILL。只有登记了整套（case: null）才容忍。
			const hit = match(r.suite, null);
			if (hit) tolerated.push({ suite: r.suite, case: null, entry: hit });
			else blocking.push({ suite: r.suite, case: null, detail: r.missing });
			continue;
		}
		for (const f of r.failures || []) {
			const name = caseOf(f);
			const hit = match(r.suite, name);
			if (hit) tolerated.push({ suite: r.suite, case: name, entry: hit });
			else blocking.push({ suite: r.suite, case: name, detail: String(f) });
		}
	}

	// 棘轮：清单项在这次**跑过的**套件里没出现 ⇒ 建议删（不算红）
	const ranSuites = new Set(results.map((r) => r.suite));
	for (const e of entries) {
		if (!ranSuites.has(e.suite)) continue; // 这次没选它，不构成证据
		const failedCases = new Set(
			(results.find((r) => r.suite === e.suite)?.failures || []).map(caseOf),
		);
		const present = e.case === null
			? results.find((r) => r.suite === e.suite)?.missing
			: failedCases.has(e.case);
		if (!present) stale.push(e);
	}

	return { ok: blocking.length === 0, tolerated, blocking, stale };
}

/** 读 `.e2e-bin/results` 下每个套件的结果；`selected` 给了就只看这些套件。 */
export function readResults(resultsDir, selected = null) {
	const want = selected ? new Set(selected) : null;
	if (!existsSync(resultsDir)) {
		// 整个目录都不在（run_all 早退/崩溃）：不能当成「没有失败」——那是假绿。
		return want ? [...want].map((suite) => ({ suite, missing: `results 目录不存在：${resultsDir}` })) : [];
	}
	const out = [];
	for (const f of readdirSync(resultsDir)) {
		if (!f.endsWith(".json") || f.startsWith("_")) continue;
		const suite = f.slice(0, -5);
		if (want && !want.has(suite)) continue;
		let data;
		try {
			data = JSON.parse(readFileSync(path.join(resultsDir, f), "utf8"));
		} catch (err) {
			out.push({ suite, missing: `results 解析失败：${err.message}` });
			continue;
		}
		out.push({ suite, failures: data.failures || [], failed: data.failed || 0 });
	}
	if (want) {
		for (const s of want) {
			if (!out.some((r) => r.suite === s)) out.push({ suite: s, missing: "没有 results/<suite>.json（未跑到 summary / 崩溃前未落盘）" });
		}
	}
	return out;
}

function parseArgs(argv) {
	const o = { selfTest: false, results: null, known: null, selected: null };
	for (let i = 0; i < argv.length; i++) {
		const a = argv[i];
		if (a === "--self-test") o.selfTest = true;
		else if (a === "--results") o.results = argv[++i];
		else if (a.startsWith("--results=")) o.results = a.slice(10);
		else if (a === "--known") o.known = argv[++i];
		else if (a.startsWith("--known=")) o.known = a.slice(8);
		// 逗号或空白都接受：workflow 里那一步输出的是空格分隔的套件名
		else if (a === "--selected") o.selected = argv[++i].split(/[,\s]+/).filter(Boolean);
		else throw new Error(`未知参数 ${a}`);
	}
	return o;
}

/* ---------------------------------------------------------------- 自测
 * 变异验证：把「清单之外的假失败」塞进来，判据必须 exit 1。
 * 只钉三件事，多一条都不必：**容忍清单内 / 拦清单外 / 清单过期只提示不红**。 */
function selfTest() {
	const known = {
		entries: [{ suite: "frontend", case: "source-actions", reason: "x", tracking: "#431" }],
	};
	const cases = [
		{
			name: "清单内的失败 → 容忍（exit 0）",
			results: [{ suite: "frontend", failures: ["source-actions — boom"] }],
			expectOk: true,
			expectTolerated: 1,
			expectBlocking: 0,
		},
		{
			name: "★ 清单之外的失败 → 拦（exit 1）",
			results: [{ suite: "frontend", failures: ["source-actions — boom", "opaque-cas-aba — new real bug"] }],
			expectOk: false,
			expectTolerated: 1,
			expectBlocking: 1,
		},
		{
			name: "★ 另一个套件的失败（清单没登记）→ 拦",
			results: [{ suite: "contract", failures: ["CAS 语义 — 真的坏了"] }],
			expectOk: false,
			expectTolerated: 0,
			expectBlocking: 1,
		},
		{
			name: "整套崩溃（没有 results）且未登记 → 拦",
			results: [{ suite: "frontend", missing: "没有 results" }],
			expectOk: false,
			expectTolerated: 0,
			expectBlocking: 1,
		},
		{
			name: "清单过期（这次没失败）→ 只提示，不红",
			results: [{ suite: "frontend", failures: [] }],
			expectOk: true,
			expectTolerated: 0,
			expectBlocking: 0,
			expectStale: 1,
		},
		{
			name: "results 目录整体不存在 + 有选中套件 → 拦（不许假绿）",
			results: [{ suite: "frontend", missing: "results 目录不存在" }],
			expectOk: false,
			expectTolerated: 0,
			expectBlocking: 1,
		},
		{
			name: "全绿 → 绿",
			results: [{ suite: "frontend", failures: [] }, { suite: "contract", failures: [] }],
			expectOk: true,
			expectTolerated: 0,
			expectBlocking: 0,
		},
	];
	let failed = 0;
	for (const c of cases) {
		const v = verdict({ results: c.results, known });
		const problems = [];
		if (v.ok !== c.expectOk) problems.push(`ok=${v.ok}（期望 ${c.expectOk}）`);
		if (v.tolerated.length !== c.expectTolerated) problems.push(`tolerated=${v.tolerated.length}（期望 ${c.expectTolerated}）`);
		if (v.blocking.length !== c.expectBlocking) problems.push(`blocking=${v.blocking.length}（期望 ${c.expectBlocking}）`);
		if (c.expectStale !== undefined && v.stale.length !== c.expectStale) problems.push(`stale=${v.stale.length}（期望 ${c.expectStale}）`);
		if (problems.length === 0) {
			console.log(`  ✅ ${c.name}`);
		} else {
			failed++;
			console.log(`  ❌ ${c.name}：${problems.join("；")}`);
		}
	}
	// 端到端再加一条：用**真的 CLI**（不是纯函数）喂一个清单之外的假失败，必须 exit 1。
	// 这才是「清单外的新失败会让 PR 变红」的直接证据；纯函数自测只证明判据，不证明接线。
	// 注意：目录固定写在 .e2e-bin/ 下（本目录禁止裸临时目录调用，见 check-tmp-leaks 的静态检查）。
	const dir = path.join(ROOT, ".e2e-bin", "verdict-selftest");
	mkdirSync(path.join(dir, "results"), { recursive: true });
	const runCli = (failures) => {
		writeFileSync(
			path.join(dir, "results", "frontend.json"),
			JSON.stringify({ suite: "frontend", passed: 1, failed: failures.length, failures }),
		);
		return spawnSync(
			process.execPath,
			[
				path.join(ROOT, "scripts/e2e/ci-e2e-verdict.mjs"),
				"--results", path.join(dir, "results"),
				"--known", path.join(ROOT, "scripts/e2e/known-env-failures.json"),
				"--selected", "frontend",
			],
			{ encoding: "utf8" },
		);
	};
	const outOfList = runCli(["totally-new-failure — 这是个假的清单外失败"]);
	const inList = runCli(["source-actions — boom"]);
	const cliCases = [
		{ name: "★ CLI：清单之外的假失败 → exit 1（PR 会变红）", got: outOfList.status, want: 1 },
		{ name: "CLI：清单内的失败 → exit 0（job 绿）", got: inList.status, want: 0 },
	];
	for (const c of cliCases) {
		if (c.got === c.want) console.log(`  ✅ ${c.name}`);
		else {
			failed++;
			console.log(`  ❌ ${c.name}：exit=${c.got}（期望 ${c.want}）\n${c.got === c.want ? "" : (c === cliCases[0] ? outOfList : inList).stderr}`);
		}
	}
	rmSync(dir, { recursive: true, force: true });

	if (failed > 0) {
		console.error(`\n❌ e2e 判定自测失败 ${failed} 条`);
		return 1;
	}
	console.log("\n✅ e2e 判定自测通过（清单内容忍 / 清单外拦 / 清单过期只提示；CLI 端到端同样成立）");
	return 0;
}

function main() {
	const args = parseArgs(process.argv.slice(2));
	if (args.selfTest) return selfTest();

	if (!args.results || !args.known) {
		console.error("用法：ci-e2e-verdict.mjs --results <dir> --known <json> [--selected a,b,c]");
		return 2;
	}
	const known = JSON.parse(readFileSync(path.resolve(ROOT, args.known), "utf8"));
	const results = readResults(path.resolve(ROOT, args.results), args.selected);
	const v = verdict({ results, known });

	for (const t of v.tolerated) {
		const what = t.case === null ? "整套" : t.case;
		console.log(`::warning title=已知环境性失败（已登记）::${t.suite} / ${what} —— ${t.entry.reason}（追踪 ${t.entry.tracking}）`);
	}
	for (const s of v.stale) {
		const what = s.case === null ? "整套" : s.case;
		console.log(`::warning title=已知失败清单可以缩小::${s.suite} / ${what} 这次没失败 —— 删掉 known-env-failures.json 里那一行（棘轮只许变小）`);
	}
	if (v.blocking.length > 0) {
		console.error(`\n❌ e2e 出现 ${v.blocking.length} 条**清单之外**的失败（必须修，或显式登记到 scripts/e2e/known-env-failures.json 并写理由）：`);
		for (const b of v.blocking) {
			const what = b.case === null ? "整套（没有 results）" : b.case;
			console.error(`  - ${b.suite} / ${what}${b.detail && b.case ? `：${b.detail}` : ""}`);
		}
		return 1;
	}
	console.log(`\n✅ e2e 判定：清单内 ${v.tolerated.length} 条（已容忍），清单外 0 条`);
	return 0;
}

const invokedDirectly =
	process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href;
if (invokedDirectly) {
	try {
		process.exit(main());
	} catch (err) {
		console.error("ci-e2e-verdict 失败:", err && err.message ? err.message : err);
		process.exit(2);
	}
}
