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
 *      `{"suite": X, "case": "*"}` 才容忍（`"*"` **只**覆盖这一种情况，不覆盖具体 case）
 *   ⑤ 超预算但有现场（`timedOut:true` 的部分结果，见 #488） → 归 `timeout`（可诊断）：
 *      已跑多少条、停在哪条、重试后是不是同一条都在文件里；**不可**用清单容忍——
 *      那是「预算/卡点」的问题，登记成白名单只会把它重新藏起来。
 *
 * 为什么清单要区分粒度：同一个环境性故障可能按 case 死，也可能整批死（Chrome 渲染进程
 * 整批被关 → 套件进程没跑到 summary → 一条 case 名都拿不到）。后者在只看 case 的清单里
 * 永远匹配不上，于是「同一个故障按 case 死被容忍、按套件死被拦」—— 这是表达力缺口：
 * 要补的是登记粒度，不是放宽容忍范围。`"*"` 把「整套崩」这一档补上，粒度仍然是逐条、带 reason + tracking。
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

/**
 * 从 `"<case> — <detail>"` 里取 case 名（harness 的 bad() 拼法）。
 */
export function caseOf(failure) {
	const s = String(failure);
	const at = s.indexOf(" — ");
	return (at < 0 ? s : s.slice(0, at)).trim();
}

/**
 * #488：套件级的 timeout 现场（执行器超预算时落的结果）。它们**不能**被当成断言失败
 * 逐条列出来（那会把一次超时变成几十条噪声），也**不能**靠 `"*"` 条目容忍
 * （`"*"` 只覆盖「整套没有 results」= incomplete）。
 */
export function timeoutOf(data) {
	if (!data || data.timedOut !== true) return null;
	return {
		budgetMs: data.budgetMs ?? null,
		elapsedMs: data.elapsedMs ?? null,
		completedCases: data.completedCases ?? null,
		totalCases: data.totalCases ?? null,
		lastCase: data.lastCase ?? null,
		lastCaseKind: data.lastCaseKind ?? null,
		attempts: Array.isArray(data.attempts) ? data.attempts : [],
		retried: data.retried === true,
		stuckSameCase: data.stuckSameCase ?? null,
		skippedRetryReason: data.skippedRetryReason ?? null,
	};
}

/** 人读的一行：「已完成 N/M 条，停在 <case>；重试 1 次后仍 timeout，两次都停在同一条」。 */
export function describeTimeout(t) {
	const done =
		t.completedCases === null
			? "已完成条数不可得"
			: `已完成 ${t.completedCases}${t.totalCases === null ? "" : `/${t.totalCases}`} 条`;
	const where = t.lastCase ? `，停在「${t.lastCase}」` : "";
	const budget = t.budgetMs ? `超出 ${Math.round(t.budgetMs / 1000)}s 预算` : "超出套件预算";
	const retry = t.retried
		? t.stuckSameCase === true
			? `；重试 1 次后仍 timeout，两次都停在同一条「${t.lastCase}」（= 确定性挂点）`
			: t.stuckSameCase === false
				? `；重试 1 次后仍 timeout，两次停在不同条（${t.attempts.map((a) => a.lastCase ?? "?").join(" / ")}）`
				: "；重试 1 次后仍 timeout"
		: "";
	const skipped = t.skippedRetryReason ? `；未重试：${t.skippedRetryReason}` : "";
	return `timeout：${budget}；${done}${where}${retry}${skipped}`;
}

/**
 * 条目是否声明「整套」：`"case": "*"`（推荐）或 `"case": null`（历史写法，等价）。
 * 只有这两种能覆盖「套件没有 results」；它们**不**覆盖任何一个具体 case 的失败。
 */
export function entryTiers(entry) {
	const c = entry?.case ?? null;
	return Array.isArray(c) ? c : [c];
}

export function isWholeSuite(entry) {
	return entryTiers(entry).some((c) => c === null || c === "*");
}

/**
 * 清单形状检查：写错形状的条目会「永远匹配不上」，与本次缺口同一类**静默失败**
 * （有条目、但永远不生效），所以直接判数据错误（exit 2），不放进裁决里猜。
 */
export function validateEntries(known) {
	const entries = known?.entries;
	if (!Array.isArray(entries)) return ["entries 必须是数组"];
	const problems = [];
	entries.forEach((e, i) => {
		const at = `entries[${i}]`;
		if (!e || typeof e !== "object") {
			problems.push(`${at} 不是对象`);
			return;
		}
		if (typeof e.suite !== "string" || !e.suite.trim()) problems.push(`${at} 缺 suite`);
		const c = e.case;
		const okTier = (v) => v === "*" || v === null || (typeof v === "string" && v.trim().length > 0);
		const shapeOk = Array.isArray(c) ? c.length > 0 && c.every(okTier) : okTier(c);
		if (!shapeOk)
			problems.push(`${at}.case 必须是具体 case 名、"*"（整套）、null（= "*"）或它们的非空数组`);
		if (typeof e.reason !== "string" || !e.reason.trim()) problems.push(`${at} 缺 reason`);
		if (typeof e.tracking !== "string" || !e.tracking.trim()) problems.push(`${at} 缺 tracking`);
	});
	return problems;
}

/**
 * 纯函数：给定「各套件的结果」与「已知清单」，裁决能不能绿。
 * 不读文件、不打印 —— 这样自测可以逐条钉住判据。
 */
export function verdict({ results, known }) {
	const entries = known.entries || [];
	const tolerated = []; // 命中清单的失败
	const blocking = []; // 清单之外（或清单里没登记整套失败）
	const timeouts = []; // #488：超预算但有现场的套件（独立一档，不可用清单容忍）
	const stale = []; // 清单里这次没失败 → 建议删

	// caseName === null 表示「整套没有 results」：只有 `"*"` / `null` 条目能覆盖它。
	// 具体 case 失败时**只**认同名 case 条目 —— `"*"` 不许顺带放行（否则整套登记会变成万能豁免）。
	const match = (suite, caseName) =>
		entries.find(
			(e) =>
				e.suite === suite &&
				(caseName === null
					? isWholeSuite(e)
					: entryTiers(e).some((t) => t !== null && t !== "*" && t === caseName)),
		);

	for (const r of results) {
		if (r.timeout) {
			// #488：timeout 有现场、可诊断 ⇒ 它是 `timeout`，不是 `incomplete`（后者才是
			// 「整套没有 results」）。已知清单只登记环境性失败，而超预算只能由「预算/卡点」
			// 修掉——把它登记成白名单就又回到了 issue 描述的掩盖路径，所以一律拦。
			timeouts.push({ suite: r.suite, timeout: r.timeout });
			blocking.push({ suite: r.suite, case: null, timeout: r.timeout });
			continue;
		}
		if (r.missing) {
			// 连 results/<suite>.json 都没有：崩溃/被 SIGKILL。只有登记了整套（case: "*" / null）才容忍。
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
		const wholeSuiteMissing = Boolean(results.find((r) => r.suite === e.suite)?.missing);
		const present = entryTiers(e).some((t) =>
			t === null || t === "*" ? wholeSuiteMissing : failedCases.has(t),
		);
		if (!present) stale.push(e);
	}

	return { ok: blocking.length === 0, tolerated, blocking, stale, timeouts };
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
		out.push({
			suite,
			timeout: timeoutOf(data),
			failures: data.failures || [],
			failed: data.failed || 0,
		});
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
 * 钉住五件事：**容忍清单内 / 拦清单外 / 清单过期只提示不红 / `*` 只覆盖整套 / `*` 不放行具体 case**。
 * 后两条是本次补齐的缺口：同一个环境故障按 case 死与按整套死，必须由**各自的**条目分别覆盖；
 * 反过来，把 `*` 当成「这个套件随便怎么失败都行」的万能豁免，必须被【2】当场拦住。 */
function selfTest() {
	let failed = 0;
	// 具体 case 条目：只覆盖同名 case
	const knownCase = {
		entries: [{ suite: "frontend", case: "source-actions", reason: "x", tracking: "#431" }],
	};
	// 套件级条目：只覆盖「整套没有 results」
	const knownSuite = {
		entries: [{ suite: "frontend", case: "*", reason: "整套崩", tracking: "#431" }],
	};
	// 条目可同时登记「整套」与具体 case（同一条环境故障的两种观测形态）
	const knownBoth = {
		entries: [{ suite: "ui_shift", case: ["*", "interact-dialogs"], reason: "两种死法", tracking: "#431" }],
	};
	const knownStarOnly = {
		entries: [{ suite: "ui_shift", case: ["*"], reason: "只登记整套", tracking: "#431" }],
	};
	const cases = [
		{
			name: "【1】具体 case 失败 + 有对应 case 条目 → 容忍（exit 0）",
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
			name: "【4】★ 整套没有 results + 只有具体 case 条目 → 拦（本次缺口就在这）",
			results: [{ suite: "frontend", missing: "没有 results" }],
			expectOk: false,
			expectTolerated: 0,
			expectBlocking: 1,
			// 同时是一条 stale：具体 case 一次都没出现（这行本来就该被棘轮收掉）
			expectStale: 1,
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
		// ── 本次补齐：套件级覆盖（`case: "*"`）──────────────────────
		{
			name: "【2】★ 具体 case 失败 + 只有 `*` 条目 → 必须红（`*` 不放行具体 case）",
			known: knownSuite,
			results: [{ suite: "frontend", failures: ["source-actions — boom"] }],
			expectOk: false,
			expectTolerated: 0,
			expectBlocking: 1,
			// 套件没有崩 → `*` 条目本次未命中，同时提示它可删；**不等于**它可以放行 case 失败
			expectStale: 1,
		},
		{
			name: "【3】★ 整套没有 results + 有 `*` 条目 → 容忍（绿，带 warning）",
			known: knownSuite,
			results: [{ suite: "frontend", missing: "没有 results/<suite>.json" }],
			expectOk: true,
			expectTolerated: 1,
			expectBlocking: 0,
			expectToleratedCase: null,
			expectToleratedTracking: "#431",
		},
		{
			name: "【5】`*` 条目过期（套件跑了且全绿）→ 只提示删，不红",
			known: knownSuite,
			results: [{ suite: "frontend", failures: [] }],
			expectOk: true,
			expectTolerated: 0,
			expectBlocking: 0,
			expectStale: 1,
		},
		{
			name: "★ case 数组 [\"*\", 具体名] + 整套没有 results → 容忍",
			results: [{ suite: "ui_shift", missing: "没有 results" }],
			known: knownBoth,
			expectOk: true,
			expectTolerated: 1,
			expectBlocking: 0,
		},
		{
			name: "★ case 数组 [\"*\", 具体名] + 该具体 case 失败 → 容忍",
			results: [{ suite: "ui_shift", failures: ["interact-dialogs — Target page, context or browser has been closed"] }],
			known: knownBoth,
			expectOk: true,
			expectTolerated: 1,
			expectBlocking: 0,
		},
		{
			name: "★ 只登记 [\"*\"] + 具体 case 失败 → 仍然拦（数组化不放宽这条不变量）",
			results: [{ suite: "ui_shift", failures: ["interact-dialogs — Target page, context or browser has been closed"] }],
			known: knownStarOnly,
			expectOk: false,
			expectTolerated: 0,
			expectBlocking: 1,
		},
		// ── #488：timeout（有现场）是独立一档，不可被白名单容忍 ──────────────
		{
			name: "【#488】★ timeout（有现场）+ 只有 `*` 条目 → 仍然拦（`*` 只覆盖 incomplete）",
			known: knownStarOnly,
			results: [
				{
					suite: "ui_shift",
					timeout: { budgetMs: 900000, elapsedMs: 900000, completedCases: 12, totalCases: 87, lastCase: "interact-dialogs", attempts: [], retried: false },
				},
			],
			expectOk: false,
			expectTolerated: 0,
			expectBlocking: 1,
			expectTimeout: 1,
			expectStale: 1,
		},
		{
			name: "【#488】★ timeout + 同名具体 case 条目 → 仍然拦（timeout 不是「断言不符」）",
			known: knownBoth,
			results: [
				{
					suite: "ui_shift",
					timeout: { budgetMs: 900000, elapsedMs: 900000, completedCases: 12, totalCases: 87, lastCase: "interact-dialogs", attempts: [], retried: true, stuckSameCase: true },
				},
			],
			expectOk: false,
			expectTolerated: 0,
			expectBlocking: 1,
			expectTimeout: 1,
		},
		{
			name: "【#488】timeout 与 incomplete 分开计数（incomplete 仍可被 `*` 容忍，timeout 不占名额）",
			known: knownSuite,
			results: [{ suite: "frontend", missing: "没有 results" }],
			expectOk: true,
			expectTolerated: 1,
			expectBlocking: 0,
			expectTimeout: 0,
		},
	];
	for (const c of cases) {
		const v = verdict({ results: c.results, known: c.known ?? knownCase });
		const problems = [];
		if (v.ok !== c.expectOk) problems.push(`ok=${v.ok}（期望 ${c.expectOk}）`);
		if (v.tolerated.length !== c.expectTolerated) problems.push(`tolerated=${v.tolerated.length}（期望 ${c.expectTolerated}）`);
		if (v.blocking.length !== c.expectBlocking) problems.push(`blocking=${v.blocking.length}（期望 ${c.expectBlocking}）`);
		if (c.expectStale !== undefined && v.stale.length !== c.expectStale) problems.push(`stale=${v.stale.length}（期望 ${c.expectStale}）`);
		if (c.expectTimeout !== undefined && v.timeouts.length !== c.expectTimeout)
			problems.push(`timeouts=${v.timeouts.length}（期望 ${c.expectTimeout}）`);
		if (c.expectToleratedCase !== undefined && v.tolerated[0]?.case !== c.expectToleratedCase)
			problems.push(`tolerated[0].case=${v.tolerated[0]?.case}（期望 ${c.expectToleratedCase}）`);
		if (c.expectToleratedTracking !== undefined && v.tolerated[0]?.entry?.tracking !== c.expectToleratedTracking)
			problems.push(`tolerated[0].tracking 丢了（warning 里必须带 tracking）`);
		if (problems.length === 0) {
			console.log(`  ✅ ${c.name}`);
		} else {
			failed++;
			console.log(`  ❌ ${c.name}：${problems.join("；")}`);
		}
	}
	// 端到端再加一轮：用**真的 CLI**（不是纯函数）跑四个场景。
	// 这才是「清单外的新失败会让 PR 变红 / `*` 不会变成万能豁免」的直接证据；
	// 纯函数自测只证明判据，不证明接线。
	// 注意：目录固定写在 .e2e-bin/ 下（本目录禁止裸临时目录调用，见 check-tmp-leaks 的静态检查）。
	// 这里用**自带的清单**而不是仓库里那份：仓库清单会随棘轮变（entries 最终清空），
	// 把自测钉死在它的内容上，收敛的那天就会白白变红。
	const dir = path.join(ROOT, ".e2e-bin", "verdict-selftest");
	const resultsDir = path.join(dir, "results");
	const knownPath = path.join(dir, "known.json");
	mkdirSync(dir, { recursive: true });
	writeFileSync(
		knownPath,
		JSON.stringify(
			{
				entries: [
					{ suite: "frontend", case: "source-actions", reason: "自测：case 级", tracking: "#431" },
					{ suite: "ui_shift", case: "*", reason: "自测：整套崩", tracking: "#431" },
				],
			},
			null,
			2,
		),
	);
	const runCli = ({ suites = {}, selected }) => {
		rmSync(resultsDir, { recursive: true, force: true });
		mkdirSync(resultsDir, { recursive: true });
		for (const [suite, payload] of Object.entries(suites)) {
			// 数组 = 断言失败列表；对象 = 原样落盘（#488 需要用带 timedOut 的部分结果做用例）
			const data = Array.isArray(payload)
				? { suite, passed: 1, failed: payload.length, failures: payload }
				: { suite, ...payload };
			writeFileSync(path.join(resultsDir, `${suite}.json`), JSON.stringify(data));
		}
		return spawnSync(
			process.execPath,
			[
				path.join(ROOT, "scripts/e2e/ci-e2e-verdict.mjs"),
				"--results", resultsDir,
				"--known", knownPath,
				"--selected", selected,
			],
			{ encoding: "utf8" },
		);
	};
	const cliCases = [
		{
			name: "★ CLI：清单之外的假失败 → exit 1（PR 会变红）",
			run: runCli({ suites: { frontend: ["totally-new-failure — 这是个假的清单外失败"] }, selected: "frontend" }),
			want: 1,
		},
		{
			name: "CLI：清单内的具体 case 失败 → exit 0（job 绿）",
			run: runCli({ suites: { frontend: ["source-actions — boom"] }, selected: "frontend" }),
			want: 0,
		},
		{
			name: "★ CLI：整套没有 results + `*` 条目 → exit 0，且 warning 带 reason + tracking",
			run: runCli({ selected: "ui_shift" }),
			want: 0,
			stdoutHas: ["ui_shift / 整套", "自测：整套崩", "#431"],
		},
		{
			name: "★ CLI：有 `*` 条目但具体 case 失败 → exit 1",
			run: runCli({ suites: { ui_shift: ["interact-dialogs — boom"] }, selected: "ui_shift" }),
			want: 1,
		},
		// #488：timeout 现场的口径必须在**真的 CLI** 上成立（不能只靠纯函数）
		{
			name: "【#488】★ CLI：timeout 现场 + `*` 条目 → exit 1，输出带「已完成 N/M」「停在」",
			run: runCli({
				suites: {
					ui_shift: {
						timedOut: true,
						budgetMs: 900000,
						elapsedMs: 900000,
						completedCases: 12,
						totalCases: 87,
						lastCase: "interact-dialogs",
						retried: true,
						stuckSameCase: true,
						failed: 75,
						failures: ["interact-dialogs — 不该被逐条列出来"],
					},
				},
				selected: "ui_shift",
			}),
			want: 1,
			stderrHas: ["timeout", "已完成 12/87 条", "停在「interact-dialogs」", "两次都停在同一条"],
		},
		{
			name: "【#488】CLI：incomplete（整套没有 results）仍走 `*` 容忍 → exit 0",
			run: runCli({ selected: "ui_shift" }),
			want: 0,
			stdoutHas: ["ui_shift / 整套"],
		},
	];
	for (const c of cliCases) {
		const missing = (c.stdoutHas || []).filter((s) => !(c.run.stdout || "").includes(s));
		const missingErr = (c.stderrHas || []).filter((s) => !(c.run.stderr || "").includes(s));
		if (c.run.status === c.want && missing.length === 0 && missingErr.length === 0) {
			console.log(`  ✅ ${c.name}`);
			continue;
		}
		failed++;
		const why =
			c.run.status !== c.want
				? `exit=${c.run.status}（期望 ${c.want}）`
				: `输出缺 ${[...missing, ...missingErr].join("、")}`;
		console.log(`  ❌ ${c.name}：${why}\n${(c.run.stderr || "").trim()}`);
	}
	rmSync(dir, { recursive: true, force: true });
	// #488：套件级 timeout/重试的自测（真起子进程、秒级）。和上面的判定自测放在一起：
	// 「有现场的 timeout 不许被白名单容忍」与「现场真的先落盘再发信号」是同一个结论的两面。
	const suiteSelftest = spawnSync(
		process.execPath,
		[path.join(ROOT, "scripts/e2e/suite-budget-selftest.mjs")],
		{ encoding: "utf8" },
	);
	if (suiteSelftest.status === 0) {
		console.log("  ✅ #488 套件级自测：超预算先落现场 / timeout 与 incomplete 分开 / 重试 1 次 / 两次停同一条可判");
	} else {
		failed++;
		console.log(
			`  ❌ #488 套件级自测失败（exit=${suiteSelftest.status}）\n` +
				`${suiteSelftest.stdout || ""}${suiteSelftest.stderr || ""}`,
		);
	}

	if (failed > 0) {
		console.error(`\n❌ e2e 判定自测失败 ${failed} 条`);
		return 1;
	}
	console.log(
		"\n✅ e2e 判定自测通过（清单内容忍 / 清单外拦 / 清单过期只提示；" +
			"`*` 只覆盖整套、不放行具体 case；timeout 是独立一档、不可被白名单容忍；CLI 端到端同样成立）",
	);
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
	const problems = validateEntries(known);
	if (problems.length > 0) {
		console.error(`❌ ${args.known} 形状不对（写错的条目会永远匹配不上 = 静默不生效）：`);
		for (const p of problems) console.error(`  - ${p}`);
		return 2;
	}
	const results = readResults(path.resolve(ROOT, args.results), args.selected);
	const v = verdict({ results, known });

	for (const t of v.tolerated) {
		const what = t.case === null ? "整套" : t.case;
		console.log(`::warning title=已知环境性失败（已登记）::${t.suite} / ${what} —— ${t.entry.reason}（追踪 ${t.entry.tracking}）`);
	}
	for (const s of v.stale) {
		const what = isWholeSuite(s) ? "整套" : s.case;
		console.log(`::warning title=已知失败清单可以缩小::${s.suite} / ${what} 这次没失败 —— 删掉 known-env-failures.json 里那一行（棘轮只许变小）`);
	}
	// #488：timeout 是独立一档（有现场、可诊断）。它不属于「已知环境性失败」：
	// 超预算只能靠「预算/卡点」修，登记成白名单就又回到了 issue 描述的掩盖路径。
	if (v.timeouts.length > 0) {
		console.error(`\n⏱️ e2e 有 ${v.timeouts.length} 个套件超出墙钟预算（timeout，现场已落盘）：`);
		for (const t of v.timeouts) console.error(`  - ${t.suite} / ${describeTimeout(t.timeout)}`);
		console.error(
			"    ↳ 现场：results/<suite>.json（timedOut/completedCases/lastCase）+ results/_suite-retries/；" +
				"先看「停在」是不是固定同一条：固定 = 真卡点，不同 = 预算太紧或环境抖动。",
		);
	}
	if (v.blocking.length > 0) {
		const nonTimeout = v.blocking.length - v.timeouts.length;
		console.error(
			`\n❌ e2e 出现 ${v.blocking.length} 条**清单之外**的失败` +
				(nonTimeout > 0 ? `（必须修，或显式登记到 scripts/e2e/known-env-failures.json 并写理由）：` : "（timeout 不可用清单容忍）："),
		);
		for (const b of v.blocking) {
			if (b.timeout) {
				console.error(`  - ${b.suite} / ${describeTimeout(b.timeout)}`);
				continue;
			}
			const what = b.case === null ? "整套（incomplete：没有 results）" : b.case;
			console.error(`  - ${b.suite} / ${what}${b.detail && b.case ? `：${b.detail}` : ""}`);
			if (b.case === null) {
				console.error(
					`    ↳ 若这是已知环境问题：加 {"suite": "${b.suite}", "case": "*"}（只覆盖「整套没有 results」，不覆盖具体 case；timeout 不适用）`,
				);
			}
		}
		return 1;
	}
	console.log(
		`\n✅ e2e 判定：清单内 ${v.tolerated.length} 条（已容忍），清单外 0 条` +
			(v.timeouts.length > 0 ? `，timeout ${v.timeouts.length} 个` : ""),
	);
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
