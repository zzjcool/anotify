/* #483 —— case 子进程死亡 ≠ 断言失败：两层判定 + 一次自动重试 + 首次死亡的现场取证
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
 *               ⇒ 先取证（见下），再自动重试 1 次（同一个 attempt 身份、同一份预算）。
 *
 *   assertion    拿到了 fragment，且失败是断言不符（产品/页面行为不对）。
 *               ⇒ 永不重试（重试只会把确定性失败跑两遍，并掩盖它）。
 *
 * 重试是**治标**。治本需要根因线索，所以每个 incomplete 都先采一份现场：
 *   kernel dmesg 尾部（OOM kill 会留痕；CI 日志里看不到，dmesg 里可能有）、
 *   free -m / df -h /tmp、chromium 进程数与总 RSS、这条 case 自己的 stdout+stderr
 *   （为空也要留下“为空”这个事实）、以及它是第几个 case（判断是否越跑越死）。
 * 落盘到 `<results>/_incomplete-forensics/`（`.e2e-bin/results` 已被 CI 的 e2e
 * artifact 整目录上传），并在套件 stdout 与并行汇总里打印路径 + 逐条摘要。
 *
 * 重试**不删除证据**：incomplete 的 case.json / exit.json / full.log 被搬到
 * `<case dir>/retry-1/`，判定记录写 `<case dir>/retry.json`。epoch/attempt 清单不变
 * （还是「每条 case 一个 attempt，且只有它自己」），所以重试不能覆盖任何已落盘的结论。
 *
 * 重试计数必须出现在两处（#483 验收口径，静默重试等于掩盖）：
 *   · 套件 stdout：`retrySummaryLine()`（含零重试的情形）；
 *   · 结果 JSON：`retries` 字段（`H.setRetryStats`）→ `parallel_runner.mjs` 汇总逐套件打印。
 */
import { spawnSync } from 'node:child_process';
import { mkdir, readFile, rename, writeFile } from 'node:fs/promises';
import { mkdirSync, writeFileSync, readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { resultsDir } from '../../lib/harness.mjs';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../../..');

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
	return (await readJson(path.join(dir, 'retry.json'))).value;
}

// ---------- 首次 incomplete 的现场取证（#483 追加要求） ----------
//
// 目的：这一次修不完根因，但必须让**下一次**失败自带根因线索。
// 全部同步采集（父进程在重试前阻塞几十毫秒），单条命令都带 3s 上限。

const FORENSICS_DIR = '_incomplete-forensics';

/** 取证落盘目录（在 results 下，CI 的 e2e artifact 已整目录上传 `.e2e-bin/results`）。 */
export function forensicsDir() {
	return path.join(resultsDir(), FORENSICS_DIR);
}

/** 打印用的相对路径（仓库根为基准）。 */
export function forensicsLabel() {
	return path.relative(ROOT, forensicsDir());
}

function run(cmd, args) {
	const r = spawnSync(cmd, args, { encoding: 'utf8', timeout: 3000 });
	return {
		ok: r.status === 0,
		status: r.status,
		error: r.error ? String(r.error.code || r.error.message || r.error) : '',
		stdout: r.stdout || '',
		stderr: r.stderr || '',
	};
}

function kernelTail() {
	const r = run('dmesg', []);
	if (!r.ok) {
		return {
			ok: false,
			error: r.error || oneLine(r.stderr, 120) || `exit=${r.status}`,
			lines: [],
		};
	}
	const lines = r.stdout.split('\n').filter(Boolean);
	return { ok: true, error: '', lines: lines.slice(-40) };
}

// dmesg 在很多 runner 上对非 root 关闭（`kernel.dmesg_restrict=1`），OOM 就看不见了。
// `/proc/vmstat` 的 `oom_kill` 是**不需要权限**的累计计数器（自机器启动起）——
// 同一台 runner 上失败前后从 0 变成非 0，就是一次真 OOM 的内核侧证据。
function oomCounters() {
	try {
		const lines = readFileSync('/proc/vmstat', 'utf8')
			.split('\n')
			.filter((line) => /^oom_kill/.test(line));
		const value = (name) => {
			const hit = lines.find((line) => line.split(/\s+/)[0] === name);
			return hit ? Number(hit.split(/\s+/)[1]) : null;
		};
		return { ok: true, error: '', lines, oomKill: value('oom_kill'), oomKillRss: value('oom_kill_rss') };
	} catch (error) {
		return {
			ok: false,
			error: String(error?.code || error?.message || error),
			lines: [],
			oomKill: null,
			oomKillRss: null,
		};
	}
}

function chromiumFootprint() {
	const r = run('ps', ['-eo', 'rss=,comm=']);
	if (!r.ok)
		return { ok: false, error: r.error || `exit=${r.status}`, procs: 0, rssMb: 0, sample: [] };
	const rows = [];
	for (const line of r.stdout.split('\n')) {
		const m = line.trim().match(/^(\d+)\s+(.+)$/);
		if (!m) continue;
		if (!/chrom/i.test(m[2])) continue;
		rows.push({ rssKb: Number(m[1]), comm: m[2] });
	}
	const rssKb = rows.reduce((n, x) => n + x.rssKb, 0);
	return {
		ok: true,
		error: '',
		procs: rows.length,
		rssMb: Math.round(rssKb / 1024),
		sample: rows.slice(0, 12).map((x) => `${x.comm} ${Math.round(x.rssKb / 1024)}MB`),
	};
}

function clip(text, bytes = 4096) {
	const s = String(text || '');
	if (s.length <= bytes) return s;
	return s.slice(0, bytes) + `\n…（截断，共 ${s.length} 字符）`;
}

/**
 * 采集一次 incomplete 的现场，落盘 JSON（机器读）+ TXT（人读），返回摘要。
 * 采集失败不影响判定：每项都记 `ok:false` + 原因，文件照样写。
 */
export function collectIncompleteForensics({
	suite = '',
	id = '',
	index = 0,
	total = 0,
	attempt = 1,
	exit = null,
	childOutput = '',
}) {
	const dir = forensicsDir();
	const base = `${suite}-${id}-a${attempt}`;
	const kernel = kernelTail();
	const oom = oomCounters();
	const mem = run('free', ['-m']);
	const disk = run('df', ['-h', '/tmp']);
	const chrom = chromiumFootprint();
	const output = String(childOutput || '');
	const snapshot = {
		issue: '#483',
		suite,
		caseId: id,
		caseIndex: index,
		caseTotal: total,
		attempt,
		collectedAt: new Date().toISOString(),
		node: process.version,
		exit: {
			exitCode: exit?.exitCode ?? null,
			signal: exit?.signal ?? null,
			timedOut: Boolean(exit?.timedOut),
		},
		caseOutput: {
			bytes: output.length,
			empty: output.trim().length === 0,
			head: clip(output.slice(0, 4096)),
			tail: clip(output.slice(-4096)),
		},
		kernel: { ok: kernel.ok, error: kernel.error, tail: kernel.lines },
		oom: { ok: oom.ok, error: oom.error, oomKill: oom.oomKill, oomKillRss: oom.oomKillRss },
		memory: { ok: mem.ok, error: mem.error, text: mem.stdout.trim() },
		disk: { ok: disk.ok, error: disk.error, text: disk.stdout.trim() },
		chromium: chrom,
	};
	const text = [
		`#483 incomplete 现场取证`,
		`suite=${suite} case=${id} #${index}/${total} attempt=${attempt} at=${snapshot.collectedAt}`,
		`子进程: exit=${snapshot.exit.exitCode ?? '—'} signal=${snapshot.exit.signal || 'none'} timedOut=${snapshot.exit.timedOut}`,
		`case 输出: ${snapshot.caseOutput.empty ? `为空（0 字符）` : `${snapshot.caseOutput.bytes} 字符`}`,
		`chromium: ${chrom.ok ? `${chrom.procs} procs, ${chrom.rssMb} MB RSS` : `不可用（${chrom.error}）`}`,
		`kernel dmesg(tail 40): ${kernel.ok ? (kernel.lines.length ? `${kernel.lines.length} 行` : `空`) : `不可用（${kernel.error}）`}`,
		`oom_kill(自启动累计, /proc/vmstat): ${oom.ok ? (oom.oomKill ?? '字段缺失') : `不可用（${oom.error}）`}`,
		'',
		'--- dmesg ---',
		kernel.ok ? kernel.lines.join('\n') || '(空)' : `(不可用: ${kernel.error})`,
		'',
		'--- /proc/vmstat (oom_kill，自机器启动累计) ---',
		oom.ok ? oom.lines.join('\n') || '(字段缺失)' : `(不可用: ${oom.error})`,
		'',
		'--- free -m ---',
		mem.ok ? mem.stdout.trim() || '(空)' : `(不可用: ${mem.error})`,
		'',
		'--- df -h /tmp ---',
		disk.ok ? disk.stdout.trim() || '(空)' : `(不可用: ${disk.error})`,
		'',
		'--- chromium 进程样本（rss, comm） ---',
		chrom.ok ? chrom.sample.join('\n') || '(没有 chromium 进程)' : `(不可用: ${chrom.error})`,
		'',
		'--- case stdout/stderr（head） ---',
		snapshot.caseOutput.empty ? '(为空：子进程没有任何输出)' : snapshot.caseOutput.head,
		'',
		'--- case stdout/stderr（tail） ---',
		snapshot.caseOutput.empty ? '(为空：子进程没有任何输出)' : snapshot.caseOutput.tail,
	].join('\n');
	try {
		mkdirSync(dir, { recursive: true });
		writeFileSync(path.join(dir, `${base}.json`), JSON.stringify(snapshot, null, 2) + '\n');
		writeFileSync(path.join(dir, `${base}.txt`), text + '\n');
	} catch {
		/* 取证失败不改变判定：摘要里仍带上已知事实（stdout/exit/chromium） */
	}
	return {
		dir: forensicsLabel(),
		file: path.join(forensicsLabel(), `${base}.txt`),
		caseId: id,
		caseIndex: index,
		caseTotal: total,
		attempt,
		exitCode: snapshot.exit.exitCode,
		signal: snapshot.exit.signal,
		timedOut: snapshot.exit.timedOut,
		outputEmpty: snapshot.caseOutput.empty,
		outputBytes: snapshot.caseOutput.bytes,
		chromProcs: chrom.procs,
		chromMb: chrom.rssMb,
		oomKill: oom.oomKill,
		dmesg: kernel.ok
			? kernel.lines.length
				? `${kernel.lines.length} 行`
				: '空'
			: `不可用（${kernel.error}）`,
	};
}

/** 单条取证的摘要行（套件 stdout 与并行汇总共用同一形状）。 */
export function forensicsLine(f, n) {
	return (
		`  ${n}) ${String(f.caseId).padEnd(22)} #${f.caseIndex}   ` +
		`exit=${f.exitCode ?? '?'} signal=${f.signal || 'none'}  ` +
		`chrom=${f.chromProcs} procs/${f.chromMb}MB  dmesg: (${f.dmesg})  oom_kill=${f.oomKill ?? '?'}  ` +
		`取证: ${f.file}`
	);
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
 * 执行一条 case，incomplete 时先取证、再自动重试（最多 maxAttempts 次，默认 2）。
 *
 * spawnAttempt(attempt) 负责真的起子进程 + 写 full.log/exit.json，返回
 * `{ exit, childOutput }`；重试身份、预算、判定口径都不变。
 */
export async function executeWithRetry({
	suite = '',
	id,
	index = 0,
	total = 0,
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
			outputEmpty: !String(childOutput || '').trim(),
			at: new Date().toISOString(),
		};
		if (verdict.kind === INCOMPLETE) {
			// 治标之前先留根因线索：没有这层取证，incomplete 永远只是「进程死了」的同义词。
			record.forensics = collectIncompleteForensics({
				suite,
				id,
				index,
				total,
				attempt,
				exit,
				childOutput,
			});
		}
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
				forensics: attempts.map((a) => a.forensics).filter(Boolean),
			};
			await writeRetryRecord(dir, outcome);
			return { exit, evidence, verdict, outcome, retried };
		}
		// 首次 incomplete（已取证）：归档证据后重试一次（同一个 attempt 身份，epoch 清单不变）。
		await archiveAttempt(dir, attempt, record);
	}
	throw new Error('executeWithRetry: unreachable (no attempt executed)');
}

/** 汇总用的重试账本：套件内所有 case 的首次判定、重试结果与现场取证。 */
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
				forensics: outcomes.flatMap((o) => o.forensics || []),
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
	if (stats.forensics?.length) parts.push(`现场取证: ${stats.forensics[0].dir}/`);
	return parts.join('，');
}

/** 逐条取证摘要（套件 stdout 用；并行汇总在 parallel_runner 里按同一形状打印）。 */
export function forensicsLines(stats) {
	return (stats?.forensics || []).map((f, i) => forensicsLine(f, i + 1));
}
