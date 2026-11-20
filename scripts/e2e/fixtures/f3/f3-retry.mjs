/* #483 —— case 子进程死亡 ≠ 断言失败：两层判定 + 一次自动重试（f3 执行层共用）
 *
 * 现象（#483 / run 44608277208）：runner 上 case 子进程偶发死亡（浏览器整批被杀、
 * 渲染进程消失、进程被 SIGKILL），`completed` 证据（`case.json`）没落盘或输出为空。
 * 旧判定只看「结果文件在不在 + 断言过没过」，于是「用例根本没跑完」与「产品断言不符」
 * 混成同一条红 —— 一个环境死亡就等于产品坏了，而且按 case 登记 known-env 永远追不上。
 *
 * 分两层（唯一判据，别在调用点另发明一套）：
 *
 *   incomplete  这条 case 没有给出结论：结果文件缺失 / 子进程输出为空 /
 *               子进程非 0（或被信号杀掉 / 超预算）且没有 fragment；
 *               以及 fragment 里记的是**执行层死亡**（浏览器/页面/传输/进程没了）——
 *               用例常把 "Target page…closed" 包进 `assert(report.errors 空)` 里，
 *               堆栈里会同时出现 AssertionError，所以死亡签名要先于断言判定。
 *               ⇒ 自动重试 1 次（同一个 attempt 身份、同一份预算）。
 *
 *   assertion    拿到了 fragment，且失败是断言不符（产品/页面行为不对）。
 *               ⇒ 永不重试（重试只会把确定性失败跑两遍，并掩盖它）。
 *
 * 重试**不删除证据**：首次 incomplete 的 case.json / exit.json / full.log 被搬到
 * `<case dir>/retry-1/`，判定记录写 `<case dir>/retry.json`。epoch/attempt 清单不变
 * （还是「每条 case 一个 attempt，且只有它自己」），所以重试不能覆盖任何已落盘的结论。
 *
 * 重试计数必须出现在两处（#483 验收口径，静默重试等于掩盖）：
 *   · 套件 stdout：`retrySummaryLine()`（零重试也要打印一行）；
 *   · 结果 JSON：`retries` 字段（`H.setRetryStats`）→ `parallel_runner.mjs` 汇总逐套件打印。
 */
import { mkdir, readFile, rename, writeFile } from 'node:fs/promises';
import path from 'node:path';

export const PASS = 'pass';
export const INCOMPLETE = 'incomplete';
export const ASSERTION = 'assertion';
// 首次 + 重试 1 次。#483 的验收写死这一档：不重试断言、最多两次 incomplete。
export const MAX_ATTEMPTS = 2;

/* 执行层死亡签名：浏览器 / 页面 / 传输 / 子进程层没了，不是「页面行为不对」。
 * 只放**基础设施**措辞；产品断言文案不进这张表（进了就等于给断言开了重试口子）。 */
const DEATH_SIGNATURES = Object.freeze([
	/Target page, context or browser has been closed/i,
	/browser has been closed|browser has disconnected|Browser closed/i,
	/Target closed|Session closed|Protocol error|WebSocket is already in/i,
	/Execution context was destroyed|Cannot find context with specified id/i,
	/navigation failed because the browser has closed/i,
	/socket hang up|ECONNRESET|ECONNREFUSED|EPIPE|EAI_AGAIN/i,
	/ERR_WORKER_OUT_OF_MEMORY|out of memory|OOM killed/i,
]);

const oneLine = (text, max = 300) =>
	String(text == null ? '' : text).replace(/\s+/g, ' ').trim().slice(0, max);

/** 子进程是否「正常收尾」：exit 0、没被信号终止、没撞预算。 */
function settledCleanly(exit) {
	return Boolean(exit) && exit.exitCode === 0 && !exit.signal && exit.timedOut !== true;
}

/** 在 fragment 的失败信息里找执行层死亡签名（没有就返回 null）。 */
function deathSignature(fragment) {
	const text = [
		fragment?.error,
		...(fragment?.report?.errors || []),
		...(fragment?.failures || []),
	]
		.filter(Boolean)
		.join('\n');
	return DEATH_SIGNATURES.find((re) => re.test(text)) || null;
}

/**
 * 纯函数：把一次 case 收尾后的证据判成 pass / incomplete / assertion。
 * evidence: { fragment, fragmentError, exit, output }
 *   fragment       解析后的 case.json（null = 没落盘/不可解析）
 *   fragmentError  读 case.json 的错误（'ENOENT' = 文件不存在）
 *   exit           父进程观察到的子进程收尾事实（exitCode/signal/timedOut）
 *   output         子进程自己的 stdout+stderr（不含父进程拼的 COMMAND 行）
 */
export function classifyCaseEvidence(evidence = {}) {
	const { fragment, fragmentError, exit, output } = evidence;
	if (!fragment) {
		const why = [];
		if (fragmentError && fragmentError !== 'ENOENT')
			why.push(`结果文件不可读（${oneLine(fragmentError, 120)}）`);
		else why.push('结果文件缺失（case.json 未落盘）');
		if (!String(output || '').trim()) why.push('子进程输出为空');
		why.push(
			`子进程 exit=${exit?.exitCode ?? '—'} signal=${exit?.signal || 'none'}` +
				(exit?.timedOut ? ' timedOut' : ''),
		);
		return { kind: INCOMPLETE, why: why.join('；') };
	}
	// fragment.ok=true 已经跑完了自己的断言；进程收尾不自洽时不能重跑（fragment 是独占证据），
	// 也不能算通过 —— 归到「不重试」那一档，红并保留证据。
	if (fragment.ok === true) {
		return settledCleanly(exit)
			? { kind: PASS, why: '' }
			: {
					kind: ASSERTION,
					why:
						'fragment 已落盘但子进程未正常收尾' +
						`（exit=${exit?.exitCode ?? '—'} signal=${exit?.signal || 'none'}` +
						`${exit?.timedOut ? ' timedOut' : ''}）`,
				};
	}
	const death = deathSignature(fragment);
	if (death) return { kind: INCOMPLETE, why: `执行层死亡（${death.source}）` };
	return { kind: ASSERTION, why: oneLine(fragment.error || 'case 断言失败') };
}

async function readJson(file) {
	try {
		return { value: JSON.parse(await readFile(file, 'utf8')), error: null };
	} catch (error) {
		return {
			value: null,
			error: error?.code === 'ENOENT' ? 'ENOENT' : String(error?.message || error),
		};
	}
}

/** 读一次 case 的证据（缺失/坏 JSON 都变成 null + error，不抛）。 */
export async function readCaseEvidence(dir, { output = '' } = {}) {
	const fragment = await readJson(path.join(dir, 'case.json'));
	const exit = await readJson(path.join(dir, 'exit.json'));
	return {
		fragment: fragment.value,
		fragmentError: fragment.error,
		exit: exit.value,
		exitError: exit.error,
		output,
	};
}

/** 读重试判定记录（没写过 = null；老证据目录天然兼容）。 */
export async function readRetryRecord(dir) {
	const payload = await readJson(path.join(dir, 'retry.json'));
	return payload.value;
}

/** 把一次失败尝试的证据搬进 `retry-<n>/`：重跑要独占 `case.json`，但结论不许被抹掉。 */
async function archiveAttempt(dir, attempt, record) {
	const archiveDir = path.join(dir, `retry-${attempt}`);
	await mkdir(archiveDir, { recursive: true });
	for (const name of ['case.json', 'exit.json', 'full.log']) {
		try {
			await rename(path.join(dir, name), path.join(archiveDir, name));
		} catch (error) {
			if (error?.code !== 'ENOENT') throw error;
		}
	}
	await writeFile(
		path.join(archiveDir, 'outcome.json'),
		JSON.stringify(record, null, 2) + '\n',
	);
}

async function writeRetryRecord(dir, outcome) {
	// 只在「重试过」或「首次就不完整」时落盘：全绿的 case 不额外写文件。
	if (!outcome.retried && outcome.kind === PASS) return;
	await writeFile(path.join(dir, 'retry.json'), JSON.stringify(outcome, null, 2) + '\n');
}

/**
 * 执行一条 case，incomplete 时自动重试（最多 maxAttempts 次，默认 2）。
 *
 * spawnAttempt(attempt) 负责真的起子进程 + 写 full.log/exit.json，返回
 * `{ exit, childOutput }`；重试身份、预算、判定口径都不变。
 */
export async function executeWithRetry({
	id,
	dir,
	spawnAttempt,
	maxAttempts = MAX_ATTEMPTS,
	onAttempt,
}) {
	await mkdir(dir, { recursive: true });
	const attempts = [];
	for (let attempt = 1; attempt <= maxAttempts; attempt++) {
		const { exit, childOutput } = await spawnAttempt(attempt);
		const evidence = await readCaseEvidence(dir, { output: childOutput });
		const verdict = classifyCaseEvidence(evidence);
		const record = {
			attempt,
			kind: verdict.kind,
			why: verdict.why,
			exitCode: exit?.exitCode ?? null,
			signal: exit?.signal ?? null,
			timedOut: Boolean(exit?.timedOut),
			emptyOutput: !String(childOutput || '').trim(),
			at: new Date().toISOString(),
		};
		attempts.push(record);
		if (typeof onAttempt === 'function') onAttempt(record);
		const retried = attempts.length > 1;
		if (verdict.kind !== INCOMPLETE || attempt === maxAttempts) {
			const outcome = {
				id,
				kind: verdict.kind,
				why: verdict.why,
				attempts,
				retried,
				recovered: retried && verdict.kind === PASS,
				exhausted: retried && verdict.kind === INCOMPLETE,
			};
			await writeRetryRecord(dir, outcome);
			return { exit, evidence, verdict, outcome, retried };
		}
		// 首次 incomplete：归档证据后重试一次（同一个 attempt 身份，epoch 清单不变）。
		await archiveAttempt(dir, attempt, record);
	}
	throw new Error('executeWithRetry: unreachable (no attempt executed)');
}

/** 汇总用的重试账本：套件内所有 case 的首次判定与重试结果。 */
export function createRetryLedger() {
	const outcomes = [];
	return {
		record(outcome) {
			outcomes.push(outcome);
			return outcome;
		},
		stats() {
			const retried = outcomes.filter((o) => o.retried);
			return {
				retried: retried.length,
				recovered: retried.filter((o) => o.recovered).length,
				exhausted: retried.filter((o) => o.exhausted).length,
				incompleteFirst: outcomes.filter((o) => o.attempts?.[0]?.kind === INCOMPLETE).length,
				assertionFirst: outcomes.filter((o) => o.attempts?.[0]?.kind === ASSERTION).length,
				cases: retried.map((o) => ({
					id: o.id,
					attempts: o.attempts.map((a) => a.kind),
					recovered: o.recovered,
				})),
			};
		},
	};
}

/** 套件 stdout 的重试行：永远打印（零重试也露出来，证明不是无条件重跑）。 */
export function retrySummaryLine(label, stats) {
	if (!stats || !stats.retried) {
		return `[${label}] 重试: 0 条（没有 case 需要重试：incomplete 才重试 1 次，断言失败不重试）`;
	}
	const parts = [`[${label}] 重试: ${stats.retried} 条首次 incomplete`];
	if (stats.recovered) parts.push(`${stats.recovered} 条重试通过`);
	if (stats.exhausted) parts.push(`${stats.exhausted} 条重试仍 incomplete（判红）`);
	return parts.join('，');
}
