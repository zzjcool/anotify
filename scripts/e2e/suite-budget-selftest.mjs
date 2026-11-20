#!/usr/bin/env node
/* suite-budget-selftest.mjs — 套件级 timeout / 重试的自测（#488；真子进程，秒级）
 *
 * 为什么要有它：套件级超时的现场「在进程被 SIGKILL 之后还在不在」只能靠**真起子进程**
 * 证明；纯函数单测证明不了「先落盘、再发信号」这个顺序。所以这里：
 *
 *   ① 纯函数面：预算解析 / 三档判定 / 重试策略 / 进度快照 / fragment 形状
 *   ② 合成证据树：ui 系（suite 布局）与 frontend（flat 布局）都能算出 completedCases/lastCase
 *   ③ 真子进程面：假套件真的挂住 → 执行器真的先写 results/<suite>.json 再 SIGTERM、
 *      宽限期后 SIGKILL；假套件的 SIGTERM handler 会读回那份文件，证明**信号发出时现场已在盘上**
 *   ④ 重试面：hang-once = timeout → 重试通过；hang-twice = timeout ×2 且两次停同一条；
 *      assertion 失败永不重试；incomplete（整套没 results）也会重试 1 次
 *
 * 由 `node scripts/e2e/ci-e2e-verdict.mjs --self-test` 调用（随 make check-affected-map 进 make ci）。
 * 目录固定写在 .e2e-bin/ 下（e2e 里禁止裸 mkdtemp，见 scripts/check-tmp-leaks.mjs）。
 */
import assert from "node:assert/strict";
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import {
	KIND,
	classifyAttempt,
	resolveSuiteBudget,
	runSuiteWithRetry,
	shouldRetry,
	writeTimeoutFragment,
} from "./lib/suite-budget.mjs";
import {
	describeProgress,
	progressFromStdout,
	readSuiteProgress,
	stuckSameCase,
} from "./lib/suite-progress.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const WORK = path.join(ROOT, ".e2e-bin", "suite-budget-selftest");
const RESULTS = path.join(WORK, "results");
const STATE = path.join(WORK, "state");
const FAKE = path.join(WORK, "fake-suite.mjs");

const FAKE_SUITE = `#!/usr/bin/env node
// 假套件：只为 selftest 存在。三种行为都走真实执行路径（真 spawn / 真信号 / 真 results 文件）。
import { appendFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";

const mode = process.env.FAKE_MODE;
const state = process.env.FAKE_STATE;
const root = process.env.FAKE_ROOT;
const suite = process.env.FAKE_SUITE;
const runId = process.env.F3_RUN_ID;
const resultsDir = process.env.E2E_RESULTS_DIR;
const countFile = path.join(state, \`\${suite}.count\`);
const n = (existsSync(countFile) ? Number(readFileSync(countFile, "utf8")) : 0) + 1;
writeFileSync(countFile, String(n));

// 合成 f3 的 ui 系证据树：case-a 已完成，case-b/c 已开始但没结论（b 更早 = 最久 in-flight）
const runRoot = path.join(root, "scripts/e2e/fixtures/f3/screenshots/f3ui/runs", runId, "explicit-mock");
const runDir = path.join(runRoot, suite);
mkdirSync(runDir, { recursive: true });
const attempts = [
	{ id: "case-a", attemptId: "att-a" },
	{ id: "case-b", attemptId: "att-b" },
	{ id: "case-c", attemptId: "att-c" },
];
writeFileSync(path.join(runDir, "attempt-manifest.json"), JSON.stringify({ attempts }));
const dirOf = (id, att) => path.join(runRoot, \`\${suite}-\${id}\`, att);
mkdirSync(dirOf("case-a", "att-a"), { recursive: true });
writeFileSync(path.join(dirOf("case-a", "att-a"), "case.json"), JSON.stringify({ id: "case-a", ok: true }));
mkdirSync(dirOf("case-b", "att-b"), { recursive: true });
await new Promise((r) => setTimeout(r, 40));
mkdirSync(dirOf("case-c", "att-c"), { recursive: true });

const writePass = () => writeFileSync(path.join(resultsDir, \`\${suite}.json\`), JSON.stringify({
	suite, passed: 3, failed: 0, failures: [], durationMs: 5,
	assertions: [{ name: "a", pass: true }, { name: "b", pass: true }, { name: "c", pass: true }],
}));
const writeAssertFail = () => writeFileSync(path.join(resultsDir, \`\${suite}.json\`), JSON.stringify({
	suite, passed: 1, failed: 1, failures: ["case-x — boom"], durationMs: 5,
	assertions: [{ name: "a", pass: true }, { name: "b", pass: false }],
}));
const hang = () => {
	// 不走默认 SIGTERM 退出：模拟「卡住、收不了尾」的套件，必须由宽限期后的 SIGKILL 收掉。
	// 同时读回 results/<suite>.json：这份内容必须在**信号之前**就落盘（#488 的核心顺序）。
	process.on("SIGTERM", () => {
		const f = path.join(resultsDir, \`\${suite}.json\`);
		const json = existsSync(f) ? JSON.parse(readFileSync(f, "utf8")) : null;
		appendFileSync(path.join(state, \`sigterm-\${n}.json\`), JSON.stringify({
			attemptSeq: n, sawTimedOut: json?.timedOut === true,
			completedCases: json?.completedCases ?? null, lastCase: json?.lastCase ?? null,
		}));
	});
	setInterval(() => {}, 1000);
};

if (mode === "pass") { writePass(); process.exit(0); }
else if (mode === "assert-fail") { writeAssertFail(); process.exit(1); }
else if (mode === "incomplete") process.exit(1);
else if (mode === "hang-once") { if (n === 1) hang(); else { writePass(); process.exit(0); } }
else if (mode === "hang-twice") hang();
else if (mode === "hang-then-incomplete") { if (n === 1) hang(); else process.exit(1); }
else throw new Error("unknown FAKE_MODE " + mode);
`;

let failed = 0;
function test(name, fn) {
	try {
		fn();
		console.log(`  ✅ ${name}`);
	} catch (err) {
		failed++;
		console.log(`  ❌ ${name}：${err && err.message ? err.message : err}`);
	}
}

async function testAsync(name, fn) {
	try {
		await fn();
		console.log(`  ✅ ${name}`);
	} catch (err) {
		failed++;
		console.log(`  ❌ ${name}：${err && err.message ? err.message : err}`);
	}
}

rmSync(WORK, { recursive: true, force: true });
mkdirSync(RESULTS, { recursive: true });
mkdirSync(STATE, { recursive: true });
writeFileSync(FAKE, FAKE_SUITE);

// ---------- ① 纯函数：预算解析 / 判定 / 重试策略 ----------
test("预算解析：默认 / per-suite 实测 / env 覆盖（per-suite env 优先）", () => {
	const base = { suite: "ui_shift", defaultMs: 120_000, overrides: { ui_shift: 900_000 } };
	assert.equal(resolveSuiteBudget({ ...base, env: {} }).budgetMs, 900_000);
	assert.equal(resolveSuiteBudget({ suite: "contract", defaultMs: 120_000, overrides: {}, env: {} }).budgetMs, 120_000);
	assert.equal(resolveSuiteBudget({ ...base, env: { E2E_SUITE_BUDGET_MS: "45000" } }).budgetMs, 45_000);
	assert.equal(
		resolveSuiteBudget({ ...base, env: { E2E_SUITE_BUDGET_MS: "45000", E2E_SUITE_BUDGET_UI_SHIFT: "60000" } }).budgetMs,
		60_000,
	);
	assert.match(resolveSuiteBudget({ ...base, env: { E2E_SUITE_BUDGET_MS: "45000" } }).source, /env/);
});

test("三档判定：timeout 优先于套件自报计数；没 results = incomplete；断言失败 = assertion", () => {
	assert.equal(classifyAttempt({ timedOut: true, exitCode: 1, json: { failed: 70 } }), KIND.TIMEOUT);
	assert.equal(classifyAttempt({ timedOut: false, exitCode: 1, json: null }), KIND.INCOMPLETE);
	assert.equal(classifyAttempt({ timedOut: false, exitCode: 1, json: { failed: 2 } }), KIND.ASSERTION);
	assert.equal(classifyAttempt({ timedOut: false, exitCode: 0, json: { failed: 0 } }), KIND.PASS);
	// #338 口径不变：exit≠0 但断言全过 = fail，不重试（收尾异常不许被重试掩盖）
	assert.equal(classifyAttempt({ timedOut: false, exitCode: 1, json: { failed: 0 } }), KIND.FAIL);
	assert.equal(shouldRetry(KIND.TIMEOUT), true);
	assert.equal(shouldRetry(KIND.INCOMPLETE), true);
	assert.equal(shouldRetry(KIND.ASSERTION), false);
	assert.equal(shouldRetry(KIND.FAIL), false);
});

// ---------- ② 进度快照：ui 系（suite 布局）与 frontend（flat 布局） ----------
test("进度快照（ui 系）：completedCases / lastCase = 最久 in-flight / 两次比较可判", () => {
	const root = path.join(WORK, "fixture-ui");
	const runId = "ui-run-1";
	const runRoot = path.join(root, "scripts/e2e/fixtures/f3/screenshots/f3ui/runs", runId, "explicit-mock");
	const runDir = path.join(runRoot, "ui_shift");
	mkdirSync(runDir, { recursive: true });
	writeFileSync(
		path.join(runDir, "attempt-manifest.json"),
		JSON.stringify({ attempts: [{ id: "case-a", attemptId: "a1" }, { id: "case-b", attemptId: "b1" }] }),
	);
	mkdirSync(path.join(runRoot, "ui_shift-case-a", "a1"), { recursive: true });
	writeFileSync(path.join(runRoot, "ui_shift-case-a", "a1", "case.json"), "{}");
	mkdirSync(path.join(runRoot, "ui_shift-case-b", "b1"), { recursive: true });
	const p = readSuiteProgress({ root, suite: "ui_shift", runId, mode: "explicit-mock" });
	assert.equal(p.source, "f3-evidence");
	assert.equal(p.totalCases, 2);
	assert.equal(p.completedCases, 1);
	assert.equal(p.lastCase, "case-b");
	assert.equal(p.lastCaseKind, "in-flight");
	assert.match(describeProgress(p), /已完成 1\/2 条，停在「case-b」/);
	const same = stuckSameCase([{ progress: p }, { progress: p }]);
	assert.equal(same, true);
	assert.equal(stuckSameCase([{ progress: p }, { progress: null }]), null);
	// 目录还没建 = 拿不到进度（返回 null，调用方退化到 stdout），不猜
	assert.equal(readSuiteProgress({ root, suite: "frontend", runId: "nope", mode: "real" }), null);
});

test("进度快照（frontend flat）：<id>/<attemptId>/case.json 也认", () => {
	const root = path.join(WORK, "fixture-flat");
	const runId = "fe-run-1";
	const runRoot = path.join(root, "scripts/e2e/fixtures/f3/screenshots/f3/runs", runId, "real");
	mkdirSync(runRoot, { recursive: true });
	writeFileSync(
		path.join(runRoot, "attempt-manifest.json"),
		JSON.stringify({ attempts: [{ id: "source-actions", attemptId: "x1" }] }),
	);
	mkdirSync(path.join(runRoot, "source-actions", "x1"), { recursive: true });
	writeFileSync(path.join(runRoot, "source-actions", "x1", "case.json"), "{}");
	const p = readSuiteProgress({ root, suite: "frontend", runId, mode: "real" });
	assert.equal(p.completedCases, 1);
	assert.equal(p.lastCase, "source-actions");
	assert.equal(p.lastCaseKind, "completed");
});

test("stdout 兜底：只数断言行，且明确标 source（不冒充 case 数）", () => {
	const p = progressFromStdout("  ✅ first check\n  ✅ second check\n  ❌ third — boom\n");
	assert.equal(p.source, "stdout-assertions");
	assert.equal(p.completedCases, 3);
	assert.equal(p.lastCase, "third");
	assert.equal(p.totalCases, null);
	assert.match(describeProgress(p), /已完成 3 条断言/);
	assert.equal(progressFromStdout("").source, "none");
});

// ---------- ③ fragment 形状（验收要求的三个字段） ----------
test("timeout fragment：timedOut / elapsedMs / completedCases / lastCase / partial 全在", () => {
	const resultsDir = path.join(WORK, "frag-results");
	mkdirSync(resultsDir, { recursive: true });
	const progress = {
		source: "f3-evidence",
		totalCases: 87,
		completedCases: 12,
		lastCase: "interact-dialogs",
		lastCaseKind: "in-flight",
		inFlight: [{ case: "interact-dialogs", startedAtMs: 1, elapsedMs: 5 }],
		completed: [],
	};
	const { fragment } = writeTimeoutFragment({
		resultsDir,
		suite: "ui_shift",
		attempt: 1,
		budgetMs: 900_000,
		elapsedMs: 900_123,
		progress,
	});
	assert.equal(fragment.timedOut, true);
	assert.equal(fragment.partial, true);
	assert.equal(fragment.completedCases, 12);
	assert.equal(fragment.lastCase, "interact-dialogs");
	assert.equal(fragment.totalCases, 87);
	assert.equal(fragment.elapsedMs, 900_123);
	assert.equal(fragment.budgetMs, 900_000);
	// 已存在的套件 summary 不被覆盖计数（套件自己知道更精确的账）
	const resultsDir2 = path.join(WORK, "frag-results2");
	mkdirSync(resultsDir2, { recursive: true });
	writeFileSync(
		path.join(resultsDir2, "frontend.json"),
		JSON.stringify({ suite: "frontend", passed: 5, failed: 2, failures: ["x — y"], assertions: [1, 2, 3, 4, 5, 6, 7] }),
	);
	const { fragment: f2 } = writeTimeoutFragment({
		resultsDir: resultsDir2,
		suite: "frontend",
		attempt: 1,
		budgetMs: 1000,
		elapsedMs: 1000,
		progress: { source: "none", totalCases: null, completedCases: null, lastCase: null, lastCaseKind: null, inFlight: [], completed: [] },
	});
	assert.equal(f2.passed, 5);
	assert.equal(f2.failed, 2);
	assert.equal(f2.timedOut, true);
	assert.equal(f2.completedCases, null);
});

// ---------- ④ 真子进程：先落现场、再发信号；重试策略 ----------
const baseEnv = () => ({
	...process.env,
	E2E_RESULTS_DIR: RESULTS,
	FAKE_STATE: STATE,
	FAKE_ROOT: ROOT,
	FAKE_SUITE: "ui_shift",
});

async function drive(mode) {
	rmSync(RESULTS, { recursive: true, force: true });
	mkdirSync(RESULTS, { recursive: true });
	rmSync(path.join(STATE, "ui_shift.count"), { force: true });
	for (const f of ["sigterm-1.json", "sigterm-2.json"]) rmSync(path.join(STATE, f), { force: true });
	return runSuiteWithRetry({
		suite: "ui_shift",
		file: FAKE,
		cwd: ROOT,
		env: { ...baseEnv(), FAKE_MODE: mode },
		mode: "explicit-mock",
		budgetMs: 900,
		graceMs: 500,
		resultsDir: RESULTS,
		root: ROOT,
		wallLimitMs: null,
	});
}

await testAsync("真子进程：超预算先落 results/<suite>.json（信号发出时现场已在盘上），再 SIGTERM→SIGKILL", async () => {
	const r = await drive("hang-twice");
	assert.equal(r.status, "timeout");
	assert.equal(r.kind, KIND.TIMEOUT);
	assert.equal(r.retried, true);
	assert.equal(r.stuckSameCase, true);
	assert.equal(r.lastCase, "case-b");
	assert.equal(r.completedCases, 1);
	assert.equal(r.totalCases, 3);
	// 两次 attempt 的 SIGTERM handler 都在**收到信号时**读到了带 timedOut 的 results 文件
	for (const f of ["sigterm-1.json", "sigterm-2.json"]) {
		const note = JSON.parse(readFileSync(path.join(STATE, f), "utf8"));
		assert.equal(note.sawTimedOut, true, `${f} 证明落盘先于信号`);
		assert.equal(note.lastCase, "case-b");
	}
	// 最终 results 也带重试结论（CI 判定器据此打「两次是否停同一条」）
	const json = JSON.parse(readFileSync(path.join(RESULTS, "ui_shift.json"), "utf8"));
	assert.equal(json.timedOut, true);
	assert.equal(json.completedCases, 1);
	assert.equal(json.lastCase, "case-b");
	assert.equal(json.stuckSameCase, true);
	assert.equal(json.attempts.length, 2);
	assert.equal(json.attempts.map((a) => a.kind).join(","), "timeout,timeout");
	// 第一次的证据被搬走，不是被覆盖
	assert.ok(existsSync(path.join(RESULTS, "_suite-retries", "ui_shift-a1.json")));
	assert.ok(existsSync(path.join(RESULTS, "_suite-retries", "ui_shift-a1.log")));
	const ledger = JSON.parse(readFileSync(path.join(RESULTS, "_suite-retries", "ui_shift.json"), "utf8"));
	assert.equal(ledger.retried, true);
	assert.equal(ledger.recovered, false);
	assert.equal(ledger.stuckSameCase, true);
});

await testAsync("真子进程：timeout → 重试通过（recovered），不掩盖第一次的现场", async () => {
	const r = await drive("hang-once");
	assert.equal(r.status, "pass");
	assert.equal(r.retried, true);
	assert.equal(r.recovered, true);
	assert.deepEqual(r.attempts.map((a) => a.kind), ["timeout", "pass"]);
	assert.ok(existsSync(path.join(RESULTS, "_suite-retries", "ui_shift-a1.json")));
	const archived = JSON.parse(readFileSync(path.join(RESULTS, "_suite-retries", "ui_shift-a1.json"), "utf8"));
	assert.equal(archived.timedOut, true);
	assert.equal(archived.lastCase, "case-b");
});

await testAsync("真子进程：重试不继承上一次的 timedOut（attempt 2 无结论 ⇒ incomplete，不是假 timeout）", async () => {
	const r = await drive("hang-then-incomplete");
	assert.deepEqual(r.attempts.map((a) => a.kind), ["timeout", "incomplete"]);
	assert.equal(r.kind, KIND.INCOMPLETE);
	assert.equal(r.status, "fail");
	// 上一次的 fragment 必须已搬走且原路径清空：否则 verdict 层会看到一条不存在的 timeout
	assert.equal(existsSync(path.join(RESULTS, "ui_shift.json")), false);
	const archived = JSON.parse(readFileSync(path.join(RESULTS, "_suite-retries", "ui_shift-a1.json"), "utf8"));
	assert.equal(archived.timedOut, true);
});

await testAsync("真子进程：断言失败永不重试（只跑 1 次，没有 _suite-retries）", async () => {
	const r = await drive("assert-fail");
	assert.equal(r.kind, KIND.ASSERTION);
	assert.equal(r.retried, false);
	assert.deepEqual(r.attempts.map((a) => a.kind), ["assertion"]);
	assert.equal(existsSync(path.join(RESULTS, "_suite-retries", "ui_shift.json")), false);
});

await testAsync("真子进程：incomplete（整套没 results）重试 1 次，两次都无结论则红", async () => {
	const r = await drive("incomplete");
	assert.equal(r.kind, KIND.INCOMPLETE);
	assert.equal(r.retried, true);
	assert.equal(r.status, "fail");
	assert.deepEqual(r.attempts.map((a) => a.kind), ["incomplete", "incomplete"]);
	const ledger = JSON.parse(readFileSync(path.join(RESULTS, "_suite-retries", "ui_shift.json"), "utf8"));
	assert.equal(ledger.stuckSameCase, null, "没有 lastCase 时不许伪造「停同一条」");
});

await testAsync("真子进程：全绿套件不重试、不落任何 _suite-retries 账本", async () => {
	const r = await drive("pass");
	assert.equal(r.status, "pass");
	assert.equal(r.retried, false);
	assert.equal(r.kind, KIND.PASS);
	assert.equal(existsSync(path.join(RESULTS, "_suite-retries", "ui_shift.json")), false);
});

rmSync(WORK, { recursive: true, force: true });

if (failed > 0) {
	console.error(`\n❌ #488 套件级自测失败 ${failed} 条`);
	process.exit(1);
}
console.log("\n✅ #488 套件级自测通过（超预算先落现场 / timeout 与 incomplete 分开 / 重试 1 次 / 两次停同一条可判 / 断言失败不重试）");
