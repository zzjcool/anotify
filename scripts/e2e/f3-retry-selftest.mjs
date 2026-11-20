#!/usr/bin/env node
/* #483 自测：case 子进程死亡的分层判定 + 一次自动重试（真子进程，不是模拟）
 *
 * 为什么必须有这条自测：这套机制**只在偶发环境下才触发**（CI runner 上进程死亡），
 * 本地正常跑一辈子都走不到那条分支。没有自测，重试逻辑要么悄悄失效（回到「环境死亡
 * 算产品坏了」），要么过度重试（把断言失败重跑两遍并掩盖它）——两种都不会有人发现。
 *
 * 四个场景（与 issue #483 的验收一一对应），每个都真起子进程：
 *   ① die-once     首次 process.exit(1) 且不写 fragment      → 必须重试一次并通过
 *   ② assertion    断言失败（有 fragment）                    → 必须**不重试**（只跑一次）
 *   ③ die-always   每次都死                                   → 重试后仍 incomplete（incomplete ×2）
 *   ④ pass         正常通过                                   → 重试计数为 0
 * 另外两条回归：
 *   ⑤ fragment-death 浏览器死亡签名（Target page…closed）+ fragment → 仍算 incomplete（ui_shift 那条签名的形状）
 *   ⑥ fragment-ok-exit-bad fragment.ok=true 但子进程 exit≠0 → 不重试（fragment 是独占证据）
 *
 * 跑法：node scripts/e2e/f3-retry-selftest.mjs
 * 由 scripts/e2e/ci-e2e-verdict.mjs --self-test 调用（那条在 make check-affected-map /
 * make ci 里跑）；也可以单独跑。
 */
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdirSync, rmSync, writeFileSync, readFileSync, existsSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import {
	executeWithRetry,
	createRetryLedger,
	retrySummaryLine,
	forensicsLines,
	forensicsDir,
	classifyCaseEvidence,
	readRetryRecord,
	PASS,
	INCOMPLETE,
	ASSERTION,
} from './fixtures/f3/f3-retry.mjs';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
// 固定工作目录（不引临时目录 helper：scripts/e2e 里只有 lib/harness.mjs 能建临时目录）。
const WORK = path.join(ROOT, '.e2e-bin/f3-retry-selftest');
// 取证文件落到自测自己的 results 下：别污染真实运行的 `.e2e-bin/results`。
process.env.E2E_RESULTS_DIR = path.join(WORK, 'results');
const CHILD = path.join(WORK, 'fake-case-child.mjs');

/* 假 case 子进程：把 #483 的几种现场写成**最朴素的形态** ——
 * 落盘 = 写 case.json（fragment）；死亡 = 什么都不写直接退出。 */
const CHILD_SOURCE = `import { writeFileSync, mkdirSync } from 'node:fs';
import path from 'node:path';
const mode = process.env.SELFTEST_MODE;
const dir = process.env.SELFTEST_DIR;
const attempt = Number(process.env.SELFTEST_ATTEMPT || '1');
mkdirSync(dir, { recursive: true });
const fragment = (ok, extra = {}) =>
  writeFileSync(path.join(dir, 'case.json'), JSON.stringify({ ok, checks: ok ? 1 : 0, report: { errors: [] }, ...extra }, null, 2) + '\\n');
if (mode === 'pass') { fragment(true); process.exit(0); }
if (mode === 'die-once') {
  if (attempt === 1) { process.exit(1); }          // 首次：不写 fragment，直接死
  fragment(true); process.exit(0);
}
if (mode === 'die-always') { process.exit(1); }    // 每次都死，永不落盘
if (mode === 'assertion') {
  fragment(false, { error: 'AssertionError [ERR_ASSERTION]: injected assertion mismatch (selftest)' });
  process.exit(1);
}
if (mode === 'fragment-death') {
  if (attempt === 1) {
    fragment(false, { error: 'page.evaluate: Target page, context or browser has been closed (selftest)' });
    process.exit(1);
  }
  fragment(true); process.exit(0);
}
if (mode === 'fragment-ok-exit-bad') { fragment(true); process.exit(3); }
process.exit(64);
`;

let failures = 0;
function check(name, fn) {
	try {
		fn();
		console.log(`  ✅ ${name}`);
	} catch (error) {
		failures++;
		console.log(`  ❌ ${name}：${error && error.message ? error.message : error}`);
	}
}

/** 真起子进程；父进程只做「写 full.log/exit.json」这一件事（与三个 f3 执行器同形）。 */
function spawnFakeCase({ mode, dir, attempt }) {
	return new Promise((resolve) => {
		const started = Date.now();
		const child = spawn(process.execPath, [CHILD], {
			cwd: ROOT,
			stdio: ['ignore', 'pipe', 'pipe'],
			env: { ...process.env, SELFTEST_MODE: mode, SELFTEST_DIR: dir, SELFTEST_ATTEMPT: String(attempt) },
		});
		let childOutput = '';
		child.stdout.on('data', (d) => { childOutput += d; });
		child.stderr.on('data', (d) => { childOutput += d; });
		child.once('error', (e) => { childOutput += String(e); });
		child.once('close', (code, signal) => {
			const exit = { exitCode: code, signal: signal || null, timedOut: false, durationMs: Date.now() - started };
			// 父进程写日志与 exit.json：子进程死亡时文件仍在 —— 这正是「有 exit 事实、没 fragment」。
			writeFileSync(path.join(dir, 'full.log'), `COMMAND ${mode}\n${childOutput}\nCHILD_EXIT=${code}\n`);
			writeFileSync(path.join(dir, 'exit.json'), JSON.stringify(exit, null, 2) + '\n');
			resolve({ exit, childOutput });
		});
	});
}

async function runCase(name, mode, { maxAttempts } = {}) {
	const dir = path.join(WORK, 'cases', name);
	const ledger = createRetryLedger();
	const spawned = [];
	const run = await executeWithRetry({
		suite: 'selftest',
		id: name,
		index: 1,
		total: 1,
		dir,
		maxAttempts,
		spawnAttempt: (attempt) => {
			spawned.push(attempt);
			return spawnFakeCase({ mode, dir, attempt });
		},
	});
	ledger.record(run.outcome);
	return { dir, run, ledger, spawned };
}

async function main() {
	rmSync(WORK, { recursive: true, force: true });
	mkdirSync(WORK, { recursive: true });
	writeFileSync(CHILD, CHILD_SOURCE);

	console.log('▶ ① 注入死亡：首次 SIGKILL/exit(1) 且不写 fragment → 重试一次通过');
	const dieOnce = await runCase('die-once', 'die-once');
	check('只重试了 1 次（共 2 次子进程）', () => assert.deepEqual(dieOnce.spawned, [1, 2]));
	check('首次判 incomplete（结果文件缺失 + 输出为空）', () =>
		assert.equal(dieOnce.run.outcome.attempts[0].kind, INCOMPLETE));
	check('首次 incomplete 的 why 点名了缺失/空输出', () => {
		const why = dieOnce.run.outcome.attempts[0].why;
		assert.match(why, /结果文件缺失/);
		assert.match(why, /子进程输出为空/);
	});
	check('最终按 fragment 判 pass（不是「因为重试过就算绿」）', () =>
		assert.equal(dieOnce.run.outcome.kind, PASS));
	check('outcome.retried / recovered 均为 true', () => {
		assert.equal(dieOnce.run.outcome.retried, true);
		assert.equal(dieOnce.run.outcome.recovered, true);
		assert.equal(dieOnce.run.outcome.exhausted, false);
	});
	check('首次死亡的证据被归档到 retry-1/（不删证据）', () => {
		assert.ok(existsSync(path.join(dieOnce.dir, 'retry-1/outcome.json')), '缺 retry-1/outcome.json');
		assert.ok(existsSync(path.join(dieOnce.dir, 'retry-1/full.log')), '缺 retry-1/full.log');
	});
	check('retry.json 记录了两次判定', () => {
		const record = JSON.parse(readFileSync(path.join(dieOnce.dir, 'retry.json'), 'utf8'));
		assert.deepEqual(record.attempts.map((a) => a.kind), [INCOMPLETE, PASS]);
	});
	check('汇总里出现重试计数（不是静默）', () => {
		const line = retrySummaryLine('selftest', dieOnce.ledger.stats());
		assert.match(line, /重试: 1 条首次 incomplete/);
		assert.match(line, /1 条重试通过/);
	});
	// #483 追加要求：首次 incomplete 必须先取证（让下一次失败自带根因线索）。
	check('首次 incomplete 已现场取证（dmesg / free / df / chromium / case 输出）', () => {
		const f = dieOnce.run.outcome.forensics;
		assert.equal(f.length, 1, '取证条数');
		const txt = readFileSync(path.join(ROOT, f[0].file), 'utf8');
		for (const section of ['--- dmesg ---', '--- free -m ---', '--- df -h /tmp ---', '--- chromium 进程样本', '--- /proc/vmstat (oom_kill', '--- case stdout/stderr（head） ---'])
			assert.ok(txt.includes(section), `取证文件缺 ${section}`);
		// die-once 的首次尝试没有任何输出：这个「为空」事实也要留下。
		assert.match(txt, /case 输出: 为空/);
		assert.equal(f[0].outputEmpty, true);
		assert.ok(existsSync(forensicsDir()), '取证目录不存在');
		assert.ok(Number.isInteger(f[0].chromProcs), 'chromium 进程数缺失');
	});
	check('汇总行带取证目录与逐条摘要（case 序号 / chrom / dmesg）', () => {
		const stats = dieOnce.ledger.stats();
		assert.match(retrySummaryLine('selftest', stats), /现场取证: /);
		const lines = forensicsLines(stats);
		assert.equal(lines.length, 1);
		assert.match(lines[0], /1\) die-once\s+#1/);
		assert.match(lines[0], /chrom=\d+ procs\/\d+MB/);
		assert.match(lines[0], /dmesg: \(/);
		assert.match(lines[0], /oom_kill=(\d+|\?)/);
	});

	console.log('▶ ② 注入断言失败：永不重试（该 case 只跑一次）');
	const assertion = await runCase('assertion', 'assertion');
	check('只起了一次子进程（重试会掩盖确定性失败）', () =>
		assert.deepEqual(assertion.spawned, [1]));
	check('判成 assertion（有 fragment + 断言不符）', () =>
		assert.equal(assertion.run.outcome.kind, ASSERTION));
	check('reason 保留 AssertionError 原文', () =>
		assert.match(assertion.run.outcome.why, /AssertionError/));
	check('retried=false', () => assert.equal(assertion.run.outcome.retried, false));
	check('汇总里重试数为 0', () =>
		assert.match(retrySummaryLine('selftest', assertion.ledger.stats()), /重试: 0 条/));

	console.log('▶ ③ 连续死两次：仍然红，reason = incomplete ×2');
	const dieAlways = await runCase('die-always', 'die-always');
	check('两次都跑了，且判定都是 incomplete', () => {
		assert.deepEqual(dieAlways.spawned, [1, 2]);
		assert.deepEqual(dieAlways.run.outcome.attempts.map((a) => a.kind), [INCOMPLETE, INCOMPLETE]);
	});
	check('exhausted=true，kind 仍是 incomplete（不放宽）', () => {
		assert.equal(dieAlways.run.outcome.exhausted, true);
		assert.equal(dieAlways.run.outcome.kind, INCOMPLETE);
	});
	check('汇总里的 reason 形状是 `incomplete ×2`', () => {
		const reason = 'incomplete ×' + dieAlways.run.outcome.attempts.length + '：' + dieAlways.run.outcome.why;
		assert.match(reason, /^incomplete ×2：/);
		assert.match(retrySummaryLine('selftest', dieAlways.ledger.stats()), /重试仍 incomplete/);
	});
	check('两次 incomplete 各有自己的取证文件', () => {
		const f = dieAlways.run.outcome.forensics;
		assert.equal(f.length, 2, '两次死亡都应留证');
		assert.deepEqual(f.map((x) => x.attempt), [1, 2]);
		assert.notEqual(f[0].file, f[1].file);
	});

	console.log('▶ ④ 正常通过：重试计数 = 0（证明不是无条件重跑）');
	const pass = await runCase('pass', 'pass');
	check('只跑一次', () => assert.deepEqual(pass.spawned, [1]));
	check('retried=false / kind=pass', () => {
		assert.equal(pass.run.outcome.retried, false);
		assert.equal(pass.run.outcome.kind, PASS);
	});
	check('没有 retry.json（全绿不留额外证据文件）', () =>
		assert.equal(existsSync(path.join(pass.dir, 'retry.json')), false));
	check('汇总打印的是 0 条', () => {
		const line = retrySummaryLine('selftest', pass.ledger.stats());
		assert.match(line, /重试: 0 条/);
		assert.match(line, /断言失败不重试/);
	});

	console.log('▶ ⑤ 回归：浏览器死亡签名（有 fragment）仍算 incomplete → 重试');
	const fragmentDeath = await runCase('fragment-death', 'fragment-death');
	check('首次判 incomplete（死亡签名优先于 AssertionError 判定）', () =>
		assert.deepEqual(fragmentDeath.run.outcome.attempts.map((a) => a.kind), [INCOMPLETE, PASS]));
	check('重试通过', () => assert.equal(fragmentDeath.run.outcome.recovered, true));

	console.log('▶ ⑥ 回归：fragment.ok=true 但子进程 exit≠0 → 不重试（证据独占，不覆盖）');
	const badExit = await runCase('fragment-ok-exit-bad', 'fragment-ok-exit-bad');
	check('只跑一次', () => assert.deepEqual(badExit.spawned, [1]));
	check('不判 pass（进程没收尾），也不重试', () => {
		assert.equal(badExit.run.outcome.kind, ASSERTION);
		assert.equal(badExit.run.outcome.retried, false);
	});

	console.log('▶ ⑦ 纯函数边界：空输出 + 无 fragment 必须点名「输出为空」');
	check('classifyCaseEvidence 覆盖三类 incomplete 签名', () => {
		const missing = classifyCaseEvidence({ fragment: null, fragmentError: 'ENOENT', exit: { exitCode: 1, signal: null, timedOut: false }, output: '' });
		assert.equal(missing.kind, INCOMPLETE);
		assert.match(missing.why, /结果文件缺失/);
		assert.match(missing.why, /输出为空/);
		const killed = classifyCaseEvidence({ fragment: null, fragmentError: 'ENOENT', exit: { exitCode: null, signal: 'SIGKILL', timedOut: false }, output: 'noise' });
		assert.equal(killed.kind, INCOMPLETE);
		assert.match(killed.why, /signal=SIGKILL/);
		const assertionNoFragment = classifyCaseEvidence({ fragment: null, fragmentError: 'ENOENT', exit: { exitCode: 1, signal: null, timedOut: false }, output: 'AssertionError: x' });
		// 没有 fragment 就是没给结论：输出里出现 AssertionError 也**不能**升级成断言失败。
		assert.equal(assertionNoFragment.kind, INCOMPLETE);
	});

	if (failures > 0) {
		console.error(`\n❌ #483 重试自测失败 ${failures} 条`);
		return 1;
	}
	console.log(`\n✅ #483 重试自测通过（incomplete 重试 1 次 / 断言不重试 / 连续两次 incomplete 仍红 / 正常 0 重试）`);
	return 0;
}

main().then(
	(code) => process.exit(code),
	(error) => {
		console.error('#483 重试自测异常：', error && error.stack ? error.stack : error);
		process.exit(2);
	},
);
