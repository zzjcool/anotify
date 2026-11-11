import { captureEpoch, epochFields, assertFrozen, validateAttempt, planRun, enrollAttempt, readEnrollments, validateManifest } from './f3-epoch.mjs';
// F3b2b standalone runner: same H.startTimer/summary/result-JSON contract as the
// existing frontend/i18n entries, with immutable per-case epoch evidence.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { mkdir, readFile, writeFile, readdir } from 'node:fs/promises';
import { writeFileSync } from 'node:fs';
import path from 'node:path';
import * as H from '../../lib/harness.mjs';
import { ROOT, TEAM, isMock } from './f3-driver.mjs';
import { casesFor, registeredViews, runUiCase } from './f3-ui-cases.mjs';
import { evidenceDirectory } from './f3-driver.mjs';

export const UI_OUT = path.join(TEAM, 'screenshots/f3ui');

// Authored sources AND the generated site served to Chrome are both frozen.
export async function uiFingerprint(suite) { return (await captureEpoch(ROOT)).fullSourceArtifactSHA; }

// Complete is NOT pass: exact mode/epoch/cwd/attempt plus real child exit and
// every registered view/page attempted in fixed order.
export function validateUiEvidence(selected, result, exit, expected) {
  for (const record of [selected, result, exit])
    for (const key of ['suite', 'id', 'runId', 'attemptId', 'cwd', 'sourceHash', 'mode', 'authority'])
      assert.equal(record[key], expected[key], 'Exact evidence ' + key);
  assert.equal(exit.exitCode, 0, 'Actual child exit must be zero');
  assert.equal(exit.signal, null, 'No signal-terminated case');
  assert.equal(exit.timedOut, false, 'No deadline-truncated case');
  assert.equal(result.ok, true, 'Case ok flag: ' + (result.error || ''));
  assert.ok(result.checks > 0, 'No no-op green');
  assert.ok(result.report, 'Browser report required');
  assert.deepEqual(result.report.errors, [], 'Browser console/pageerror/network failures not waived');
  const views = result.report.views || result.report.pages;
  assert.ok(Array.isArray(views), 'Every case records its real page views');
  if (expected.pages) {
    assert.deepEqual(result.report.pages.map(x => x.page), expected.pages, 'Every registered page in fixed order');
    const seen = new Set();
    for (const view of result.report.pages) {
      assert.equal(view.lang, expected.lang); assert.equal(view.theme, expected.theme); assert.equal(view.viewport, expected.viewport);
      assert.equal(view.attempted, true); assert.equal(view.ok, true);
      const key = [view.page, view.lang, view.theme, view.viewport].join('|');
      assert.equal(seen.has(key), false, 'Duplicate matrix cell ' + key); seen.add(key);
    }
  }
  if (expected.views) assert.equal(views.length, expected.views, 'Every registered view of the case attempted');
  for (const view of views) { assert.equal(view.attempted, true, 'View attempted'); assert.equal(view.ok, true, 'View green: ' + view.page); }
}

export async function uiMain(suite, self) {
  H.startTimer();
  const mode = isMock() ? 'explicit-mock' : 'real';
  const runId = process.env.F3_RUN_ID || randomUUID();
  const cwd = process.cwd();
  const cases = casesFor(suite);
  const registered = registeredViews(suite);
  const children = new Set();
  let interrupted = false;
  const signal = () => {
    interrupted = true;
    for (const child of children) {
      try { process.kill(-child.pid, 'SIGTERM'); } catch { /* settled */ }
      setTimeout(() => { try { process.kill(-child.pid, 'SIGKILL'); } catch { /* settled */ } }, 1200).unref();
    }
  };
  for (const sig of ['SIGINT', 'SIGTERM']) process.on(sig, signal);
  const runDir = path.join(UI_OUT, 'runs', runId, mode, suite);
  let frozen, plan;
  const meta = (id, sourceHash, attemptId) => ({ suite, id, runId, attemptId, cwd, ...epochFields(frozen), mode, authority: mode });
  const directory = m => evidenceDirectory({ ...m, id: suite + '-' + m.id }, UI_OUT);

  async function enroll(identity) { await enrollAttempt(runDir, identity, plan); }
  // In-process single case (spawned fragment): writes the exclusive case record.
  async function fragment(id, sourceHash) {
    assert.ok(cases.some(x => x.id === id), 'Unknown registered case: ' + id);
    const identity = meta(id, sourceHash, process.env.F3_ATTEMPT_ID || randomUUID());
    const dir = directory(identity);
    await mkdir(dir, { recursive: true });
    await enroll(identity);
    process.env.F3_ATTEMPT_ID = identity.attemptId;
    process.env.F3_SOURCE_HASH = sourceHash;
    process.env.E2E_RESULTS_DIR = dir;
    process.once('exit', code => writeFileSync(path.join(dir, 'exit.json'), JSON.stringify({ ...identity, exitCode: code, signal: null, timedOut: false }, null, 2) + '\n'));
    console.log('COMMAND ' + JSON.stringify({ cwd, argv: [process.execPath, self, '--fragment', id], ...identity }));
    const begin = Date.now();
    let outcome;
    try { outcome = await runUiCase(suite, id); }
    catch (error) { outcome = { ok: false, checks: 0, error: String(error.stack || error).slice(0, 1200), report: { errors: [] } }; }
    assertFrozen(frozen, await captureEpoch(ROOT));
    const evidence = { ...outcome, ...identity, durationMs: Date.now() - begin };
    await writeFile(path.join(dir, 'case.json'), JSON.stringify(evidence, null, 2) + '\n', { flag: 'wx' });
    await mkdir(path.join(runDir, id), { recursive: true });
    return evidence;
  }
  // One outer 55s budget per registered case + guaranteed process-group cleanup.
  async function execute(id, sourceHash) {
    const identity = meta(id, sourceHash, plan.attempts.find(x=>x.id===id).attemptId);
    const dir = directory(identity);
    await mkdir(dir, { recursive: true });
    await enroll(identity);
    return new Promise(resolve => {
      const child = spawn(process.execPath, [self, '--fragment', id], { cwd: ROOT, detached: true, stdio: ['ignore', 'pipe', 'pipe'], env: { ...process.env, F3_RUN_ID: runId, F3_ATTEMPT_ID: identity.attemptId, F3_SOURCE_HASH: sourceHash, E2E_RESULTS_DIR: dir } });
      children.add(child);
      let output = 'COMMAND ' + JSON.stringify({ cwd: ROOT, argv: [process.execPath, self, '--fragment', id], ...identity }) + '\n';
      let timedOut = false;
      const kill = sig => { try { process.kill(-child.pid, sig); } catch { /* group settled */ } };
      const soft = setTimeout(() => { timedOut = true; kill('SIGTERM'); }, 52000);
      const hard = setTimeout(() => { if (timedOut || interrupted) kill('SIGKILL'); }, 54000);
      child.stdout.on('data', chunk => { output += chunk; });
      child.stderr.on('data', chunk => { output += chunk; });
      child.once('error', error => { output += String(error); });
      child.once('close', async (code, sig) => {
        clearTimeout(soft); clearTimeout(hard); children.delete(child);try{process.kill(-child.pid,'SIGKILL');}catch{/* group already settled */}
        const exit = { ...identity, exitCode: code, signal: sig, timedOut };
        output += '\nCHILD_EXIT=' + code + ' SIGNAL=' + (sig || 'none') + '\n';
        try {
          await writeFile(path.join(dir, 'full.log'), output);
          await writeFile(path.join(dir, 'exit.json'), JSON.stringify(exit, null, 2) + '\n');
          resolve(exit);
        } catch (error) { resolve({ ...exit, writeError: String(error) }); }
      });
    });
  }

  async function aggregate(sourceHash, exits) {
    assertFrozen(frozen, await captureEpoch(ROOT));
    validateManifest(plan, cases.map(x=>x.id), await readEnrollments(runDir), exits);
    const collected = [];
    const missing = [];
    let passed = 0, failed = 0;
    let viewAttempted = 0, viewPassed = 0, matrixAttempted = 0;
    for (const item of cases) {
      let selected, outcome, exit, dir;
      try {
        const attempts = (await readdir(path.join(runDir, item.id))).filter(x => x.startsWith('attempt-') && x.endsWith('.json'));
        assert.equal(attempts.length, 1, 'Exactly one attempt per registered case in the final epoch; a retry cannot erase failure');
        selected = JSON.parse(await readFile(path.join(runDir, item.id, attempts[0]), 'utf8'));
        if (exits) {
          const matches = exits.filter(x => x.id === item.id);
          assert.equal(matches.length, 1, 'Exactly one settled child per case');
          assert.equal(matches[0].attemptId, selected.attemptId);
        }
        assert.ok(selected, 'Missing settled child');
        dir = directory(selected);
        outcome = JSON.parse(await readFile(path.join(dir, 'case.json'), 'utf8'));
        exit = JSON.parse(await readFile(path.join(dir, 'exit.json'), 'utf8'));
        if (selected.writeError) throw new Error(selected.writeError);
        const views = outcome.report?.views || outcome.report?.pages || [];
        for (const view of views) { if (view.attempted) viewAttempted++; if (view.ok) viewPassed++; if (item.kind === 'grid' && view.attempted) matrixAttempted++; }
        validateAttempt(selected, outcome, exit, meta(item.id, sourceHash, selected.attemptId));
        validateUiEvidence(selected, outcome, exit, { ...meta(item.id, sourceHash, selected.attemptId), pages: item.pages, views: item.views, lang: item.lang, theme: item.theme, viewport: item.viewport });
        passed++;
        H.ok(item.id + ' (' + outcome.checks + ' actual assertions)');
      } catch (error) {
        failed++;
        if (!outcome || !exit) missing.push(item.id);
        H.bad(item.id, outcome?.error || String(error));
      }
      collected.push({ id: item.id, kind: item.kind, views: item.views, dir, attemptId: selected?.attemptId, exitCode: exit?.exitCode, signal: exit?.signal, timedOut: exit?.timedOut, ok: outcome?.ok ?? false, checks: outcome?.checks ?? 0, error: outcome?.error ?? null });
    }
    const aggregateAttempt = randomUUID();
    const record = {
      suite, runId, mode, authority: mode, cwd, ...epochFields(frozen), attemptId: aggregateAttempt,
      registered: cases.map(x => x.id), registeredViews: registered.views, registeredMatrix: registered.matrix,
      actualMatrix: matrixAttempted, viewAttempted, viewPassed,
      complete: missing.length === 0, missing, passed, failed, unrun: missing.length,
      ok: missing.length === 0 && failed === 0 && viewAttempted === registered.views && (registered.matrix === 0 || matrixAttempted === registered.matrix),
      collected,
    };
    const file = path.join(runDir, 'aggregate-' + aggregateAttempt + '.json');
    await writeFile(file, JSON.stringify(record, null, 2) + '\n', { flag: 'wx' });
    await writeFile(path.join(runDir, 'latest-aggregate.json'), JSON.stringify({ file, ...record }, null, 2) + '\n');
    console.log('AGGREGATE ' + JSON.stringify({ file, mode, sourceHash, registered: cases.length, registeredViews: registered.views, registeredMatrix: registered.matrix, actualMatrix: matrixAttempted, viewAttempted, viewPassed, passed, failed, unrun: missing.length, complete: record.complete, ok: record.ok }));
  }

  try {
    assert.equal(cwd, ROOT, 'Exact repository cwd required');
    assert.match(runId, /^[a-zA-Z0-9_-]+$/);
    frozen = await captureEpoch(ROOT);const sourceHash = frozen.fullSourceArtifactSHA;
    await mkdir(runDir, { recursive: true });
    if (process.env.F3_SOURCE_HASH) assert.equal(process.env.F3_SOURCE_HASH, sourceHash, 'Spawned source must stay frozen across the epoch');
    const epoch = { suite, runId, mode, authority: mode, cwd, ...epochFields(frozen), registered: cases.map(x => x.id), registeredViews: registered.views, registeredMatrix: registered.matrix };
    const selectedIndex=process.argv.indexOf('--case');
    plan=await planRun(runDir,{...epoch,...frozen}, selectedIndex===-1?cases.map(x=>x.id):[process.argv[selectedIndex+1]], {fragment:process.argv.includes('--fragment'),aggregate:process.argv.includes('--aggregate')});
    try { await writeFile(path.join(runDir, 'epoch.json'), JSON.stringify(epoch, null, 2) + '\n', { flag: 'wx' }); }
    catch (error) {
      if (error.code !== 'EEXIST') throw error;
      assert.deepEqual(JSON.parse(await readFile(path.join(runDir, 'epoch.json'), 'utf8')), epoch, 'No source/mode/cwd/registration mixing inside an epoch');
    }
    console.log('SUITE ' + suite + ' authority=' + mode + ' registered=' + cases.length + ' views=' + registered.views + (registered.matrix ? ' matrixViews=' + registered.matrix : '') + ' sourceHash=' + sourceHash);
    const fragmentIndex = process.argv.indexOf('--fragment');
    const index = process.argv.indexOf('--case');
    if (fragmentIndex !== -1) {
      assert.ok(process.env.F3_ATTEMPT_ID, 'Internal fragment requires a parent attempt');
      const outcome = await fragment(process.argv[fragmentIndex + 1], sourceHash);
      if (outcome.ok) H.ok(outcome.id + ' (' + outcome.checks + ' assertions)');
      else H.bad(outcome.id, outcome.error || 'case failed');
    } else if (index !== -1) {
      const id = process.argv[index + 1];
      assert.ok(cases.some(x => x.id === id), 'Unknown registered ID');
      const exit = await execute(id, sourceHash);
      const dir = directory(exit);
      console.log(await readFile(path.join(dir, 'full.log'), 'utf8').catch(() => ''));
      const outcome = JSON.parse(await readFile(path.join(dir, 'case.json'), 'utf8'));
      const item = cases.find(x => x.id === id);
      validateAttempt(exit, outcome, JSON.parse(await readFile(path.join(dir,'exit.json'),'utf8')), meta(id,sourceHash,exit.attemptId));
      validateUiEvidence(exit, outcome, JSON.parse(await readFile(path.join(dir, 'exit.json'), 'utf8')), { ...meta(id, sourceHash, exit.attemptId), pages: item.pages, views: item.views, lang: item.lang, theme: item.theme, viewport: item.viewport });
      H.ok(id + ' (' + outcome.checks + ' assertions, actual child exit ' + exit.exitCode + ')');
    } else if (process.argv.includes('--aggregate')) await aggregate(sourceHash);
    else {
      const concurrency = Number(process.env.F3_CONCURRENCY || 2);
      assert.ok(Number.isInteger(concurrency) && concurrency >= 1 && concurrency <= 6, 'Concurrency 1..6');
      let next = 0;
      const exits = [];
      await Promise.all(Array.from({ length: concurrency }, async () => {
        while (!interrupted && next < cases.length) exits.push(await execute(cases[next++].id, sourceHash));
      }));
      await aggregate(sourceHash, exits);
    }
    assertFrozen(frozen, await captureEpoch(ROOT));
  } catch (error) { H.bad(suite + ' fatal', error.stack || String(error)); }
  finally {
    for (const child of children) try { process.kill(-child.pid, 'SIGKILL'); } catch { /* settled */ }
    for (const sig of ['SIGINT', 'SIGTERM']) process.off(sig, signal);
    if (interrupted) H.bad(suite + ' interrupted', 'Unattempted/aborted groups fail the aggregate, no skip');
    process.exitCode = H.summary(suite) ? 0 : 1;
  }
}
