/* suite-progress.mjs — 「套件被杀时跑到哪了」的只读取证（#488）
 *
 * 为什么需要它：套件超预算时执行器只能 SIGKILL，套件进程里那套「写 results/<suite>.json」
 * 的收尾逻辑（H.summary / crash hook）根本没有机会跑。旧行为于是只剩
 * 「0通过/0失败 300s」——5 次 CI 失败长得一模一样，谁也说不清是慢还是卡死。
 *
 * 证据从哪来：f3 系的五个套件（frontend / i18n / i18n_coverage / ui_shift /
 * ux_regression_19_23）本来就把**每个 case 的 attempt 记录**写进运行目录，case 结束时再写
 * case.json。这些是既有产物（#485 用的也是它们），本模块只是**读**，不改任何套件逻辑：
 *
 *   runRoot/<runId>/<mode>[/<suite>]/attempt-manifest.json   ← 注册了哪些 case（含 attemptId）
 *   runRoot/<runId>/<mode>/<suite>-<id>/<attemptId>/          ← ui / i18n 系（locales / ui）
 *   runRoot/<runId>/<mode>/<id>/<attemptId>/                  ← frontend（flat）
 *
 * started   = 证据目录存在（执行器 spawn 前就建好）
 * completed = 证据目录里有 case.json（case 已有结论，成功或失败都算「跑完了」）
 * lastCase  = 被杀时仍 in-flight、且**开始最早**的那条（跑得最久的那个 = 最可能的卡点）；
 *             没有 in-flight 时退化为「最后一条完成的 case」。
 *             为什么不是「最后一条开始」：并发 2 时它会随其它 case 完成而漂移，
 *             两次重试会得到不同的名字，反而失去「两次是否停同一条」的诊断价值。
 *
 * 刻意不 import fixtures/f3/*：那会把 playwright-core 拖进执行器启动路径，而执行器还要在
 * 没有浏览器的机器上跑 `--authority-plan`。路径在这里显式列一遍，并由
 * scripts/e2e/suite-budget-selftest.mjs 的合成树用例钉住形状。
 */
import { existsSync, readFileSync, statSync } from "node:fs";
import path from "node:path";

/** f3 系五个套件的证据根（与 fixtures 的 OUT / LOCALE_OUT / UI_OUT 同源）。 */
const F3_EVIDENCE = new Map([
	// frontend：runDir 直接是 runs/<runId>/<mode>，证据目录挂在同一层（flat）
	["frontend", { base: "scripts/e2e/fixtures/f3/screenshots/f3", layout: "flat" }],
	// i18n 系：runDir = runs/<runId>/<mode>/<suite>，证据目录是兄弟 <suite>-<id>
	["i18n", { base: "scripts/e2e/fixtures/f3/screenshots/f3locales", layout: "suite" }],
	["i18n_coverage", { base: "scripts/e2e/fixtures/f3/screenshots/f3locales", layout: "suite" }],
	// ui 系
	["ui_shift", { base: "scripts/e2e/fixtures/f3/screenshots/f3ui", layout: "suite" }],
	["ux_regression_19_23", { base: "scripts/e2e/fixtures/f3/screenshots/f3ui", layout: "suite" }],
]);

export const F3_PROGRESS_SUITES = [...F3_EVIDENCE.keys()];

/** 是否是能给出 case 级进度的套件。 */
export function hasCaseProgress(suite) {
	return F3_EVIDENCE.has(suite);
}

function statSafe(file) {
	try {
		return statSync(file);
	} catch {
		return null;
	}
}

function readJsonSafe(file) {
	try {
		return JSON.parse(readFileSync(file, "utf8"));
	} catch {
		return null;
	}
}

/**
 * 从套件自己的证据目录推导进度。拿不到（非 f3 系 / 目录还没建 / manifest 没落盘）返回 null，
 * **绝不猜**：调用方会退化到 stdout 断言计数，并在结果里标 source。
 *
 * @returns {null | {
 *   source: "f3-evidence",
 *   totalCases: number,
 *   completedCases: number,
 *   lastCase: string|null,
 *   lastCaseKind: "in-flight"|"completed"|null,
 *   inFlight: {case: string, startedAtMs: number, elapsedMs: number}[],
 *   completed: string[],
 * }}
 */
export function readSuiteProgress({ root, suite, runId, mode, now = Date.now() }) {
	const layout = F3_EVIDENCE.get(suite);
	if (!layout || !runId || !mode) return null;
	const runRoot = path.join(root, layout.base, "runs", runId, mode);
	const runDir = layout.layout === "suite" ? path.join(runRoot, suite) : runRoot;
	const manifest = readJsonSafe(path.join(runDir, "attempt-manifest.json"));
	const attempts = Array.isArray(manifest?.attempts) ? manifest.attempts : null;
	if (!attempts || attempts.length === 0) return null;

	const evidenceDirOf = (a) =>
		layout.layout === "suite"
			? path.join(runRoot, `${suite}-${a.id}`, a.attemptId)
			: path.join(runRoot, a.id, a.attemptId);

	const completed = [];
	const inFlight = [];
	for (const a of attempts) {
		if (typeof a?.id !== "string" || typeof a?.attemptId !== "string") continue;
		const dir = evidenceDirOf(a);
		const dirStat = statSafe(dir);
		if (!dirStat) continue; // 还没轮到/还没 spawn
		const caseStat = statSafe(path.join(dir, "case.json"));
		if (caseStat) {
			completed.push({ id: a.id, at: caseStat.mtimeMs });
		} else {
			inFlight.push({ id: a.id, startedAtMs: dirStat.mtimeMs });
		}
	}
	if (completed.length === 0 && inFlight.length === 0) return null;

	// 跑得最久的 in-flight 才是「停在」的那条；并发下这是唯一稳定的定义。
	inFlight.sort((a, b) => a.startedAtMs - b.startedAtMs);
	completed.sort((a, b) => a.at - b.at);
	const oldest = inFlight[0];
	const lastCompleted = completed[completed.length - 1];
	const lastCase = oldest?.id ?? lastCompleted?.id ?? null;
	return {
		source: "f3-evidence",
		totalCases: attempts.length,
		completedCases: completed.length,
		lastCase,
		lastCaseKind: oldest ? "in-flight" : "completed",
		inFlight: inFlight.map((c) => ({
			case: c.id,
			startedAtMs: c.startedAtMs,
			elapsedMs: Math.max(0, now - c.startedAtMs),
		})),
		completed: completed.map((c) => c.id),
	};
}

/**
 * 非 f3 系的兜底：从已经捕获的 stdout 里数断言行（H.ok/H.bad 的 `  ✅ <name>` 形状）。
 * 它给出的是**断言数**，不是 case 数 —— 所以 totalCases 恒为 null，source 也明确写
 * `stdout-assertions`，汇总里不会把它冒充成「N/M 条 case」。
 */
export function progressFromStdout(stdout) {
	const lines = String(stdout || "").split("\n");
	let completedCases = 0;
	let lastCase = null;
	for (const line of lines) {
		const m = /^\s*[✅❌]\s+(\S.*)$/.exec(line);
		if (!m) continue;
		completedCases++;
		lastCase = m[1].split(" — ")[0].trim();
	}
	if (completedCases === 0) {
		return { source: "none", totalCases: null, completedCases: null, lastCase: null, lastCaseKind: null, inFlight: [], completed: [] };
	}
	return {
		source: "stdout-assertions",
		totalCases: null,
		completedCases,
		lastCase,
		lastCaseKind: "assertion",
		inFlight: [],
		completed: [],
	};
}

/** 首次拿不到证据时的统一形状（字段齐、值为 null，绝不把缺失伪装成 0）。 */
export function emptyProgress() {
	return { source: "none", totalCases: null, completedCases: null, lastCase: null, lastCaseKind: null, inFlight: [], completed: [] };
}

/** 汇总里那半句「已完成 N/M 条，停在 <case>」；数据不足时明说缺什么。 */
export function describeProgress(progress) {
	if (!progress || progress.source === "none" || progress.completedCases === null) {
		return "case 级进度不可得（套件在建立逐 case 证据之前就没了）";
	}
	const total = progress.totalCases === null ? null : progress.totalCases;
	const count =
		total === null
			? `已完成 ${progress.completedCases} 条断言`
			: `已完成 ${progress.completedCases}/${total} 条`;
	if (!progress.lastCase) return count;
	const where =
		progress.lastCaseKind === "in-flight"
			? `停在「${progress.lastCase}」（仍在跑，最久的一条）`
			: progress.lastCaseKind === "assertion"
				? `最后一条断言「${progress.lastCase}」`
				: `最后完成「${progress.lastCase}」`;
	return `${count}，${where}`;
}

/** 两次 attempt 是否停同一条（只有两边都有 lastCase 才可判）。
 *  接受两种形状：执行中记录 `{progress:{lastCase}}` 与账本里的扁平 `{lastCase}`。 */
export function stuckSameCase(attempts) {
	if (!Array.isArray(attempts) || attempts.length < 2) return null;
	const names = attempts.map((a) => a?.lastCase ?? a?.progress?.lastCase ?? null);
	if (names.some((n) => n === null)) return null;
	return names.every((n) => n === names[0]);
}
