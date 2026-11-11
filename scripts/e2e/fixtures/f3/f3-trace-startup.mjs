// TRACE-STARTUP · native 启动依赖现场（TRACE-STARTUP 专用，独立 case/run ID）
//
// 只读诊断：复用冻结的 f3-driver（explicit-mock 预览 + 真实 Chrome），不改 fixture、
// 不改生产文件、不复用旧 run。三个 case 各自 openDriver（独立 context/attempt）：
//
//   1. trace-startup-delayed-graphs
//      故意把 /graphs.js 的响应延迟 900ms（defer 脚本因此晚于 Trace 响应执行），
//      Trace API 仍由 mock 快速应答。原码会在图库还没赋值时读
//      window.AnotifyGraphs.taskChain → 真实现场 final-28bbef29 的同一 TypeError；
//      修复后 7 Task rows + 真实图 DOM + canonical node 点击成功。
//   2. trace-startup-missing-graphs
//      /graphs.js 真实失败（route.abort）→ 必须显示真实可见 error（trace.nolib）+
//      原重试按钮、0 假 rows；随后真实加载回该脚本（addScriptTag 真 <script src>），
//      点击原重试按钮必须真的再取一次 Trace API 并画出真图。
//   3. trace-startup-loaded-page
//      正常已加载页面（无注入）成功：7 rows、真实图、无 error、无全局 pageerror。
//
// 有界：每步 Playwright 4.5s 超时、外层 watchdog 55s 真实 exit，无轮询/无忙等。
// 退出码：0 全部通过；1 断言失败；2 watchdog。
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { readFile, mkdir } from 'node:fs/promises';
import path from 'node:path';
import { openDriver, ROOT, TEAM } from './f3-driver.mjs';

const sha256 = (buf) => createHash('sha256').update(buf).digest('hex');
const RUN = process.env.F3_RUN_ID || 'trace-startup-' + crypto.randomUUID();
const WATCHDOG_MS = 55000;
const DELAYED_GRAPHS_MS = 900;

const watchdog = setTimeout(() => {
  console.error('TRACE-STARTUP WATCHDOG ' + WATCHDOG_MS + 'ms exceeded');
  process.exit(2);
}, WATCHDOG_MS);

async function sourceEpoch() {
  let head = 'unknown';
  try {
    head = execFileSync('git', ['rev-parse', 'HEAD'], { cwd: ROOT, encoding: 'utf8' }).trim();
  } catch { /* evidence only */ }
  const files = {};
  for (const rel of ['web-src/pages/trace.html', 'web/trace.html', 'web/graphs.js', 'web/flograph.js']) {
    try { files[rel] = sha256(await readFile(path.join(ROOT, rel))); }
    catch { files[rel] = 'missing'; }
  }
  return { head, files, f3SourceHash: process.env.F3_SOURCE_HASH || null };
}

async function scene(d) {
  return d.page.evaluate(() => {
    const snapshot = window.__f3TraceSnapshot?.() || null;
    const events = snapshot?.events || [];
    const first = (name) => events.find((e) => e.event === name) || null;
    const response = (snapshot?.responses || [])[0] || null;
    return {
      documentId: snapshot?.documentId || null,
      actualDocumentURL: snapshot?.snapshot?.actualDocumentURL || null,
      readyState: document.readyState,
      rows: document.querySelectorAll('#trace-tasks [data-task]').length,
      graphNodes: document.querySelectorAll('#trace-graph [data-node-id]').length,
      // flograph 把容器本身加上 .flograph（trace 页容器就是 #trace-graph 的 body）。
      graphMounted: !!document.getElementById('trace-graph')?.classList.contains('flograph'),
      busy: document.getElementById('trace-content')?.getAttribute('aria-busy') || null,
      error: (document.getElementById('trace-error')?.textContent || '').trim().slice(0, 400),
      retry: !!document.querySelector('#trace-error button'),
      response: response && {
        httpStatus: response.httpStatus,
        kind: response.response?.kind || null,
        taskCount: response.response?.tasks?.count ?? null,
        returnAtMs: response.returnAtMs ?? null,
      },
      dclAtMs: first('DOMContentLoaded')?.atMs ?? null,
      loadAtMs: first('load')?.atMs ?? null,
      taskChainAtMs: first('taskChain:input')?.atMs ?? null,
      taskChainInput: first('taskChain:input')?.inputTypes ?? null,
      caught: first('loadErrorNode:input')
        ? { name: first('loadErrorNode:input').error?.name, message: first('loadErrorNode:input').error?.message, atMs: first('loadErrorNode:input').atMs }
        : null,
    };
  });
}

async function waitTraceSettled(d, timeout = 4500) {
  await d.page.waitForFunction(
    () => {
      const rows = document.querySelectorAll('#trace-tasks [data-task]').length;
      const error = (document.getElementById('trace-error')?.textContent || '').trim();
      return rows > 0 || error.length > 0;
    },
    null,
    { timeout },
  );
}

const traceURL = (d) => d.base + '/trace.html?namespace=default&traceUID=trace_mock_t1';
const results = [];

async function runCase(id, fn) {
  const d = await openDriver(id, { lang: 'zh-CN', theme: 'light' });
  try {
    const evidence = await fn(d);
    results.push({ id, ok: true, ...evidence });
    console.log('TRACE-STARTUP PASS ' + id + ' ' + JSON.stringify(evidence));
    return evidence;
  } catch (error) {
    results.push({ id, ok: false, failure: String(error && error.message || error), errors: d.report.errors });
    console.log('TRACE-STARTUP FAIL ' + id + ' ' + JSON.stringify({ failure: String(error && error.message || error), pageErrors: d.report.errors }));
    throw error;
  } finally {
    await d.stop();
    console.log('TRACE-STARTUP REPORT ' + id + ' ' + JSON.stringify({ reportPath: d.report.reportPath, pageErrors: d.report.errors, checks: d.report.checks }));
  }
}

// case 1：defer 图库晚于 Trace 响应（真实现场同构）
async function delayedGraphs(d) {
  let delayed = 0;
  await d.context.route(/\/graphs\.js(?:\?.*)?$/, async (route) => {
    delayed += 1;
    await new Promise((resolve) => setTimeout(resolve, DELAYED_GRAPHS_MS));
    await route.continue();
  });
  await d.page.goto(traceURL(d), { waitUntil: 'domcontentloaded', timeout: 7000 });
  await waitTraceSettled(d);
  const observed = await scene(d);
  console.log('TRACE-STARTUP SCENE delayed-graphs ' + JSON.stringify(observed));
  assert.equal(delayed, 1, 'graphs.js was really delayed exactly once');
  assert.equal(observed.rows, 7, 'seven Task rows; observed error=' + observed.error);
  assert.ok(observed.graphMounted, 'real graph DOM mounted');
  assert.ok(observed.graphNodes >= 7, 'real graph nodes rendered: ' + observed.graphNodes);
  assert.equal(observed.error, '', 'no visible error');
  assert.equal(observed.busy, null, 'busy cleared after render');
  assert.equal(observed.response?.httpStatus, 200);
  assert.equal(observed.response?.kind, 'Trace');
  assert.equal(observed.response?.taskCount, 7);
  assert.ok(
    observed.response.returnAtMs != null && observed.dclAtMs != null && observed.response.returnAtMs < observed.dclAtMs,
    'the race window is real: Trace returned before DOMContentLoaded',
  );
  assert.ok(
    observed.taskChainAtMs != null && observed.taskChainAtMs >= observed.dclAtMs,
    'first render happened after the deferred graph library (DOMContentLoaded); taskChainAtMs=' + observed.taskChainAtMs + ' dclAtMs=' + observed.dclAtMs,
  );
  assert.equal(observed.taskChainInput?.tasks, 'array');
  // canonical node 真实点击 → 任务深链
  await d.page.click('#trace-graph [data-node-id="tsk_mock_1"]');
  await d.page.waitForURL(/\/message\.html\?namespace=default&id=tsk_mock_1$/, { timeout: 4500 });
  const href = new URL(d.page.url());
  assert.equal(href.pathname, '/message.html');
  assert.equal(href.searchParams.get('id'), 'tsk_mock_1');
  await d.finish();
  return { delayedMs: DELAYED_GRAPHS_MS, observed, clicked: href.pathname + href.search };
}

// case 2：图库真失败 → 可见错误（无假 rows）→ 脚本恢复后原 retry 成功
async function missingGraphs(d) {
  let aborted = 0;
  await d.context.route(/\/graphs\.js(?:\?.*)?$/, async (route) => {
    aborted += 1;
    await route.abort('failed');
  });
  await d.page.goto(traceURL(d), { waitUntil: 'domcontentloaded', timeout: 7000 });
  await waitTraceSettled(d);
  const broken = await scene(d);
  console.log('TRACE-STARTUP SCENE missing-graphs ' + JSON.stringify(broken));
  assert.equal(aborted, 1, 'graphs.js really failed once');
  assert.ok(broken.error.length > 0, 'a real visible error is shown');
  assert.match(broken.error, /渲染脚本未能加载/, 'the explicit dependency error is shown, not a raw taskChain TypeError');
  assert.equal(broken.rows, 0, 'no fabricated Task rows');
  assert.equal(broken.graphMounted, false, 'no fabricated graph');
  assert.equal(broken.retry, true, 'the original retry button is present');
  assert.equal(broken.busy, null, 'failure does not leave the page busy');
  // 真实恢复：浏览器里真的再加载一次 <script src="/graphs.js">
  await d.context.unroute(/\/graphs\.js(?:\?.*)?$/);
  await d.page.addScriptTag({ url: '/graphs.js' });
  await d.page.waitForFunction(() => typeof window.AnotifyGraphs?.taskChain === 'function', null, { timeout: 4500 });
  const before = await d.page.evaluate(() => (window.__f3Calls || []).filter((c) => /\/traces\//.test(c.url)).length);
  await d.page.click('#trace-error button');
  await d.page.waitForFunction(() => document.querySelectorAll('#trace-tasks [data-task]').length === 7, null, { timeout: 4500 });
  const healed = await d.page.evaluate((previous) => ({
    rows: document.querySelectorAll('#trace-tasks [data-task]').length,
    graphNodes: document.querySelectorAll('#trace-graph [data-node-id]').length,
    error: (document.getElementById('trace-error')?.textContent || '').trim(),
    traceCalls: (window.__f3Calls || []).filter((c) => /\/traces\//.test(c.url)).length,
    previous,
  }), before);
  console.log('TRACE-STARTUP SCENE healed-graphs ' + JSON.stringify(healed));
  assert.equal(healed.traceCalls, before + 1, 'the real retry click issues a fresh Trace GET');
  assert.ok(healed.graphNodes >= 7, 'retry draws the real graph: ' + healed.graphNodes);
  assert.equal(healed.error, '', 'error cleared after a successful retry');
  // 注入的 graphs.js 失败是本 case 唯一的 page error（requestfailed + 浏览器 console 各一条，
  // 都是同一份真实网络失败）；不冒充干净页面，如实记录。
  const injected = d.report.errors.filter((e) => /graphs\.js|Failed to load resource/.test(e));
  assert.ok(injected.length >= 1, 'the injected graphs.js failure is observed');
  assert.equal(d.report.errors.length, injected.length, 'no other page error: ' + JSON.stringify(d.report.errors));
  return { aborted, broken, healed };
}

// case 3：正常已加载页面路径
async function loadedPage(d) {
  await d.page.goto(traceURL(d), { waitUntil: 'domcontentloaded', timeout: 7000 });
  await d.page.waitForFunction(() => document.querySelectorAll('#trace-tasks [data-task]').length === 7, null, { timeout: 4500 });
  const observed = await scene(d);
  console.log('TRACE-STARTUP SCENE loaded-page ' + JSON.stringify(observed));
  assert.equal(observed.rows, 7);
  assert.ok(observed.graphMounted && observed.graphNodes >= 7, 'real graph in the normal path');
  assert.equal(observed.error, '');
  assert.equal(observed.response?.taskCount, 7);
  await d.finish();
  return { observed };
}

async function main() {
  await mkdir(TEAM, { recursive: true });
  const epoch = await sourceEpoch();
  console.log('TRACE-STARTUP EPOCH ' + JSON.stringify(epoch));
  await runCase('trace-startup-delayed-graphs', delayedGraphs);
  await runCase('trace-startup-missing-graphs', missingGraphs);
  await runCase('trace-startup-loaded-page', loadedPage);
  console.log('TRACE-STARTUP OK ' + JSON.stringify({ runId: RUN, epoch, cases: results.map((r) => ({ id: r.id, ok: r.ok })) }));
}

try {
  await main();
} catch (error) {
  console.error('TRACE-STARTUP ABORT exit=1');
  console.error(error && error.stack || String(error));
  process.exitCode = 1;
} finally {
  clearTimeout(watchdog);
}
