/* suite-budget.mjs — 套件级「超预算先落现场 + 自动重试 1 次」（#488）
 *
 * 对称于 #485 在 **case** 层做的事，这次是 **suite** 层：
 *
 *   ① 超预算时**先写** results/<suite>.json 的部分结论（timedOut/elapsedMs/
 *      completedCases/lastCase/partial），**再**发信号 —— 进程被 SIGKILL 之后
 *      现场仍然在盘上，5 次失败不会再长得一模一样。
 *   ② 判定层把这种失败叫 `timeout`（有现场、可诊断），与 `incomplete`
 *      （整条套件什么都没留下）分开；`assertion`（断言不符）永不重试。
 *   ③ `timeout` / `incomplete` ⇒ 自动重试 1 次；两次的 lastCase 并排比较，
 *      「两次都停在同一条」= 确定性挂点，「停在不同条」= 抖动/环境压力。
 *
 * 为什么「先写再杀」而不是让套件自己收尾：被杀的是 SIGKILL，收尾逻辑没有机会跑；
 * 而 SIGTERM 只是**尽力**（套件可能在 await 里卡住，或事件循环被占满），所以两条路都留：
 * 先落 fragment（保证有现场），再发 SIGTERM 让套件尽量补一份自己的 summary（更全），
 * settle 后再把 timeout 元数据合并回去（套件写的计数优先）。
 *
 * 预算口径（为什么是数据驱动）：固定墙钟 + 慢环境 = 必然超时（#478 的 2s、#485 的 52s
 * 都是同一类）。所以预算由「CI 实测耗时」×2 定，数字与 run id 写进
 * SUITE_TIMEOUT_OVERRIDE 旁边的注释和 docs/reports/e2e-suite-budget.md。
 */
import { spawn } from "node:child_process";
import { copyFileSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import path from "node:path";
import {
	describeProgress,
	emptyProgress,
	progressFromStdout,
	readSuiteProgress,
	stuckSameCase,
} from "./suite-progress.mjs";

/** 发 SIGTERM 之后留给套件收尾（杀子进程 / 写 summary）的宽限；超了才 SIGKILL。 */
export const SUITE_KILL_GRACE_MS = 20_000;

/** 套件级失败的三档（与 case 级的 #483/#485 同词汇，避免两套说法）。 */
export const KIND = Object.freeze({
	PASS: "pass",
	ASSERTION: "assertion", // 有 results 且 failed>0：断言不符，永不重试
	FAIL: "fail", // 进程非 0 但断言全过（#338 口径）：不重试，这是收尾/清理异常
	TIMEOUT: "timeout", // 超出墙钟预算，已落部分现场
	INCOMPLETE: "incomplete", // 没有任何 results（崩溃 / 被 OOM / 连现场都没写出来）
});

/** 套件级重试策略：只有「没有结论」的两种失败才重试。 */
export function shouldRetry(kind) {
	return kind === KIND.TIMEOUT || kind === KIND.INCOMPLETE;
}

function readJsonSafe(file) {
	try {
		return JSON.parse(readFileSync(file, "utf8"));
	} catch {
		return null;
	}
}

function numEnv(value) {
	if (value === undefined || value === null || value === "") return null;
	const n = Number(value);
	return Number.isFinite(n) && n > 0 ? n : null;
}

/**
 * 预算解析（诊断/注入用 env 覆盖，CI 不设它们）：
 *   E2E_SUITE_BUDGET_MS               → 所有套件统一覆盖（例如注入时用 45s 逼出 timeout）
 *   E2E_SUITE_BUDGET_<SUITE 大写>      → 单个套件覆盖
 * 返回值带上来源，runner 会把「被 env 覆盖」显式打印出来 —— 抬预算必须留痕。
 */
export function resolveSuiteBudget({ suite, defaultMs, overrides = {}, env = process.env }) {
	const perSuite = numEnv(env[`E2E_SUITE_BUDGET_${String(suite).toUpperCase()}`]);
	if (perSuite) return { budgetMs: perSuite, source: `env E2E_SUITE_BUDGET_${String(suite).toUpperCase()}` };
	const global = numEnv(env.E2E_SUITE_BUDGET_MS);
	if (global) return { budgetMs: global, source: "env E2E_SUITE_BUDGET_MS" };
	const override = numEnv(overrides[suite]);
	if (override) return { budgetMs: override, source: "per-suite 实测预算" };
	return { budgetMs: defaultMs, source: "默认预算" };
}

/**
 * 一次 attempt 的判定。`json` 是 results/<suite>.json 读回来的内容（可能为空）。
 * 顺序有意：timedOut 优先（超预算的套件即使最后自己补了 summary，也仍然是 timeout）；
 * 没有 results 才是 incomplete；failed>0 是断言（这些都不该被重试掩盖）。
 */
export function classifyAttempt({ timedOut, exitCode, json }) {
	if (timedOut) return KIND.TIMEOUT;
	if (!json) return KIND.INCOMPLETE;
	if ((json.failed ?? 0) > 0) return KIND.ASSERTION;
	if (exitCode === 0) return KIND.PASS;
	return KIND.FAIL;
}

function timeoutFields({ suite, attempt, budgetMs, elapsedMs, progress }) {
	return {
		suite,
		attempt,
		timedOut: true,
		partial: true,
		elapsedMs,
		budgetMs,
		completedCases: progress.completedCases,
		totalCases: progress.totalCases,
		lastCase: progress.lastCase,
		lastCaseKind: progress.lastCaseKind,
		inFlight: progress.inFlight,
		progressSource: progress.source,
		timeoutAt: new Date().toISOString(),
	};
}

/**
 * 超预算时**先落盘**的那份部分结果。已存在的文件（极少见：套件恰好在这个边界写完了自己的
 * summary）优先保留计数，只补 timeout 元数据；套件没写过就补一个自洽的骨架
 * （passed/failed 记 0 —— 「已完成 N 条」不是「N 条断言通过」，不冒充绿）。
 */
export function writeTimeoutFragment({ resultsDir, suite, attempt, budgetMs, elapsedMs, progress }) {
	mkdirSync(resultsDir, { recursive: true });
	const file = path.join(resultsDir, `${suite}.json`);
	const existing = readJsonSafe(file) || {};
	const next = { ...existing, ...timeoutFields({ suite, attempt, budgetMs, elapsedMs, progress }) };
	if (typeof next.passed !== "number") next.passed = 0;
	if (typeof next.failed !== "number") next.failed = 0;
	if (!Array.isArray(next.failures)) next.failures = [];
	if (typeof next.durationMs !== "number") next.durationMs = elapsedMs;
	if (!Array.isArray(next.assertions)) next.assertions = [];
	writeFileSync(file, JSON.stringify(next, null, 2) + "\n");
	return { file, fragment: next };
}

/**
 * settle 之后的收口：套件若被 SIGTERM 叫醒并补写了自己的 summary，那份计数更全（每个 case 的
 * 通过/失败都有账），保留它；timeout 元数据与进度快照再合并上去。progress 用**新的**快照
 * （宽限期内可能又完成了几条 case）。
 */
export function finalizeTimeoutEvidence({ resultsDir, suite, attempt, budgetMs, elapsedMs, progress }) {
	const file = path.join(resultsDir, `${suite}.json`);
	const existing = readJsonSafe(file) || {};
	const merged = { ...existing, ...timeoutFields({ suite, attempt, budgetMs, elapsedMs, progress }) };
	if (typeof merged.passed !== "number") merged.passed = 0;
	if (typeof merged.failed !== "number") merged.failed = 0;
	if (!Array.isArray(merged.assertions)) merged.assertions = [];
	if (!Array.isArray(merged.failures)) merged.failures = [];
	writeFileSync(file, JSON.stringify(merged, null, 2) + "\n");
	return merged;
}

/** 重试前把上一次的证据搬走（重试不能覆盖任何已落盘的结论）。 */
export function archiveAttempt({ resultsDir, suite, attempt, kind, elapsedMs, budgetMs, progress, stdout, stderr }) {
	const dir = path.join(resultsDir, "_suite-retries");
	mkdirSync(dir, { recursive: true });
	const jsonFile = path.join(resultsDir, `${suite}.json`);
	const archived = path.join(dir, `${suite}-a${attempt}.json`);
	if (existsSync(jsonFile)) copyFileSync(jsonFile, archived);
	const logFile = path.join(dir, `${suite}-a${attempt}.log`);
	writeFileSync(
		logFile,
		`# ${suite} attempt ${attempt} kind=${kind} elapsedMs=${elapsedMs} budgetMs=${budgetMs}` +
			` progress=${describeProgress(progress)}\n` +
			`--- stdout ---\n${stdout || ""}\n--- stderr ---\n${stderr || ""}\n`,
	);
	return { archived, logFile };
}

/** 重试账本：两次 attempt 并排（kind/耗时/lastCase），以及「是否停同一条」。 */
export function writeRetryLedger({ resultsDir, suite, attempts, retried, recovered, skippedRetryReason }) {
	const dir = path.join(resultsDir, "_suite-retries");
	mkdirSync(dir, { recursive: true });
	const file = path.join(dir, `${suite}.json`);
	const payload = {
		suite,
		retried: Boolean(retried),
		recovered: Boolean(recovered),
		stuckSameCase: stuckSameCase(attempts),
		skippedRetryReason: skippedRetryReason || null,
		attempts,
	};
	writeFileSync(file, JSON.stringify(payload, null, 2) + "\n");
	return { file, ledger: payload };
}

function killTree(proc, signal) {
	if (!proc || proc.exitCode !== null || proc.signalCode !== null) return;
	try {
		// detached:true ⇒ 套件自己是一个进程组；杀整组，避免只杀 leader 留下子孙
		process.kill(-proc.pid, signal);
	} catch {
		try {
			proc.kill(signal);
		} catch {
			/* 已经退出 */
		}
	}
}

/**
 * 跑一次 attempt（不重试），超预算路径 = 先落 fragment 再 SIGTERM，宽限期后 SIGKILL。
 *
 * @returns {Promise<{exitCode:number|null, signal:string|null, elapsedMs:number,
 *   stdout:string, stderr:string, timedOut:boolean, progress:object}>}
 */
export function runSuiteAttempt({
	suite,
	file,
	cwd,
	env,
	budgetMs,
	resultsDir,
	attempt,
	root,
	graceMs = SUITE_KILL_GRACE_MS,
	onTimeoutFragment = () => {},
}) {
	return new Promise((resolve) => {
		const startedAt = Date.now();
		const proc = spawn(process.execPath, [file], {
			cwd,
			env,
			stdio: ["ignore", "pipe", "pipe"],
			// 独立进程组：超时时能连同它派生的进程一起收掉
			detached: true,
		});
		let stdout = "";
		let stderr = "";
		let timedOut = false;
		let killTimer = null;
		let progress = emptyProgress();

		proc.stdout.on("data", (d) => (stdout += d));
		proc.stderr.on("data", (d) => (stderr += d));

		const snapshot = () => {
			const fromEvidence =
				readSuiteProgress({ root, suite, runId: env.F3_RUN_ID, mode: env.E2E_SUITE_MODE }) || null;
			return fromEvidence || progressFromStdout(stdout);
		};

		const budgetTimer = setTimeout(() => {
			timedOut = true;
			// ① 先落现场（必须在发信号之前 —— 发完信号进程可能立刻没了）
			progress = snapshot();
			const { file: fragmentFile } = writeTimeoutFragment({
				resultsDir,
				suite,
				attempt,
				budgetMs,
				elapsedMs: Date.now() - startedAt,
				progress,
			});
			onTimeoutFragment(fragmentFile);
			// ② 再请它自己收尾（f3 系会杀子进程并补一份 summary；卡死的套件可能收不了）
			killTree(proc, "SIGTERM");
			// ③ 宽限期后硬杀，绝不让一个卡死的套件把整条 e2e 拖住
			killTimer = setTimeout(() => killTree(proc, "SIGKILL"), graceMs);
		}, budgetMs);

		const settle = (code, signal) => {
			clearTimeout(budgetTimer);
			if (killTimer) clearTimeout(killTimer);
			const elapsedMs = Date.now() - startedAt;
			if (timedOut) {
				// 宽限期内套件可能补写了自己的 summary：合并而不是覆盖（计数以套件为准）
				progress = snapshot();
				finalizeTimeoutEvidence({ resultsDir, suite, attempt, budgetMs, elapsedMs, progress });
				killTree(proc, "SIGKILL"); // 收掉可能还在的子孙
			}
			resolve({ exitCode: code, signal: signal ?? null, elapsedMs, stdout, stderr, timedOut, progress });
		};

		proc.once("exit", settle);
		proc.once("error", (err) => {
			stderr += `\nspawn error: ${err && err.message ? err.message : String(err)}\n`;
			settle(null, null);
		});
	});
}

/**
 * 套件级执行 + 自动重试 1 次（timeout / incomplete 才重试；断言失败永不重试）。
 * runId 每个 attempt 唯一：f3 的运行目录按 runId 分代，重试不能污染上一次的证据。
 */
export async function runSuiteWithRetry({
	suite,
	file,
	cwd,
	env,
	mode,
	budgetMs,
	resultsDir,
	root,
	maxAttempts = 2,
	wallLimitMs = null,
	runStartedAt = Date.now(),
	graceMs = SUITE_KILL_GRACE_MS,
	onNote = () => {},
}) {
	const attempts = [];
	const stamp = Date.now().toString(36);
	let skippedRetryReason = null;
	let result = null;

	for (let attempt = 1; attempt <= maxAttempts; attempt++) {
		// 每个 attempt 都从「没有 results」开始：上一次的 fragment 已搬进 _suite-retries/，
		// 不能留在原路径 —— 否则重试的判定会读到上一次的 timedOut（假 timeout / 假 pass）。
		if (attempt > 1) rmSync(path.join(resultsDir, `${suite}.json`), { force: true });
		const runId = `${suite}-${stamp}-a${attempt}`;
		const attemptEnv = {
			...env,
			F3_RUN_ID: runId,
			// 把预算告诉套件（f3 的 case 预算仍由 F3_CASE_BUDGET_MS 独立控制）：
			// 现场里因此能看出「这条是被多长的墙钟掐掉的」。
			E2E_SUITE_BUDGET_MS: String(budgetMs),
			E2E_SUITE_MODE: mode,
		};
		result = await runSuiteAttempt({
			suite,
			file,
			cwd,
			env: attemptEnv,
			budgetMs,
			resultsDir,
			attempt,
			root,
			graceMs,
		});
		const json = readJsonSafe(path.join(resultsDir, `${suite}.json`));
		const kind = classifyAttempt({ timedOut: result.timedOut, exitCode: result.exitCode, json });
		const record = {
			attempt,
			kind,
			exitCode: result.exitCode,
			signal: result.signal,
			elapsedMs: result.elapsedMs,
			budgetMs,
			progress: result.progress,
			stdout: result.stdout,
			stderr: result.stderr,
		};

		const lastAttempt = attempt === maxAttempts;
		const retryable = shouldRetry(kind);
		if (!retryable || lastAttempt) {
			attempts.push(record);
			break;
		}
		// 重试前先搬走这次的全部证据；账本最后一次统一写（两次并排才有诊断价值）。
		const archived = archiveAttempt({
			resultsDir,
			suite,
			attempt,
			kind,
			elapsedMs: result.elapsedMs,
			budgetMs,
			progress: result.progress,
			stdout: result.stdout,
			stderr: result.stderr,
		});
		record.archived = archived.archived;
		record.logFile = archived.logFile;
		attempts.push(record);

		// 墙钟保护：重试会把整条 job 再拖一个预算。CI 的 e2e job 有 40min 上限，
		// 撞上它的话连 artifact 都传不上去（现场全丢）——那比「直接红但留着现场」更糟。
		if (wallLimitMs && Date.now() - runStartedAt + budgetMs > wallLimitMs) {
			skippedRetryReason =
				`重试会把整条 e2e 拖过 job 墙钟上限（已用 ${Math.round((Date.now() - runStartedAt) / 1000)}s` +
				` + 一次预算 ${Math.round(budgetMs / 1000)}s > 上限 ${Math.round(wallLimitMs / 1000)}s）；` +
				"保留现场直接红，比 job 超时丢掉全部 artifact 更好";
			onNote(`${suite}: ${skippedRetryReason}`);
			break;
		}
		onNote(
			`${suite}: 第 ${attempt} 次 ${kind}（${describeProgress(result.progress)}）→ 自动重试 1 次` +
				`（证据搬到 ${path.relative(root, archived.archived)}）`,
		);
	}

	const finalKind = attempts[attempts.length - 1].kind;
	const retried = attempts.length > 1;
	const recovered = retried && finalKind === KIND.PASS;
	const attemptsView = attempts.map((a) => ({
		attempt: a.attempt,
		kind: a.kind,
		elapsedMs: a.elapsedMs,
		budgetMs: a.budgetMs,
		exitCode: a.exitCode,
		signal: a.signal,
		completedCases: a.progress?.completedCases ?? null,
		totalCases: a.progress?.totalCases ?? null,
		lastCase: a.progress?.lastCase ?? null,
		lastCaseKind: a.progress?.lastCaseKind ?? null,
		progressSource: a.progress?.source ?? "none",
		inFlight: a.progress?.inFlight ?? [],
		archived: a.archived ?? null,
		log: a.logFile ?? null,
	}));
	const stuck = stuckSameCase(attemptsView);
	const last = attempts[attempts.length - 1];
	// 账本只在**真的需要诊断**时落（重试过 / 超时）：正常套件不多出一个产物（#488 验收 3）。
	let ledgerFile = null;
	if (retried || finalKind === KIND.TIMEOUT) {
		ledgerFile = writeRetryLedger({
			resultsDir,
			suite,
			attempts: attemptsView,
			retried,
			recovered,
			skippedRetryReason,
		}).file;
	}
	// #488：timeout 的 results JSON 还要带上重试结论（CI 的判定器据此打“两次是否停同一条”），
	// 否则 artifact 里只有 attempt 2 的现场，看不到重试发生过。
	if (finalKind === KIND.TIMEOUT) {
		const jsonFile = path.join(resultsDir, `${suite}.json`);
		const existing = readJsonSafe(jsonFile);
		if (existing) {
			writeFileSync(
				jsonFile,
				JSON.stringify(
					{ ...existing, retried, attempts: attemptsView, stuckSameCase: stuck, skippedRetryReason },
					null,
					2,
				) + "\n",
			);
		}
	}
	return {
		suite,
		mode,
		status:
			finalKind === KIND.PASS
				? "pass"
				: finalKind === KIND.TIMEOUT
					? "timeout"
					: "fail",
		kind: finalKind,
		exitCode: last.exitCode,
		signal: last.signal,
		// 成本可见性：重试花掉的时间也是这次 e2e 的钱，按 attempt 累加
		elapsedMs: attempts.reduce((n, a) => n + a.elapsedMs, 0),
		budgetMs,
		stdout: last.stdout,
		stderr: last.stderr,
		attempts: attemptsView,
		retried,
		recovered,
		stuckSameCase: stuck,
		skippedRetryReason: skippedRetryReason || null,
		lastCase: last.progress?.lastCase ?? null,
		completedCases: last.progress?.completedCases ?? null,
		totalCases: last.progress?.totalCases ?? null,
		progress: last.progress,
		ledgerFile,
		describe: describeProgress(last.progress),
	};
}
