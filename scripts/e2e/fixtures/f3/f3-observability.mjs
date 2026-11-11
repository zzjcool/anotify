// Attributed F2b2 browser interactions promoted to registered F3 cases.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
export async function observability(d, mode) {
 const {page,goto,calls,req,check,shot,clean,archive:archiveCalls}=d;
 const count=sel=>page.locator(sel).count();
 if(mode==='errors') { await page.goto(d.base+'/en/index.html?namespace=default',{waitUntil:'domcontentloaded'}); await page.waitForFunction(()=>!!window.Anotify); await page.evaluate(()=>window.__f3Fault='/console/events'); }
  if (mode === 'events') {
    await goto('events', 'namespace=default');
    await page.locator('#ev-limit').selectOption('2');
    await page.locator('#ev-filter').click();
    await page.waitForFunction(() => document.querySelectorAll('#ev-list article').length === 2);
    const first = (await calls()).findLast(call => call.url.includes('/console/events')).response;
    assert.equal(first.kind, 'EventList'); assert.ok(first.metadata.continue);
    await page.locator('#ev-more').click();
    await page.waitForFunction(() => document.querySelectorAll('#ev-list article').length === 4);
    await page.locator('#ev-refresh').click();
    await page.waitForFunction(() => !document.querySelector('#ev-refresh').disabled);
    assert.equal(await count('#ev-list article'), 4);
    const uids = await page.locator('#ev-list article').evaluateAll(nodes => nodes.map(n => n.dataset.uid));
    assert.equal(new Set(uids).size, 4);
    check('Actual EventList pagination and first-page UID refresh dedup');
    await page.locator('#ev-source').fill('deploy-hook');
    await page.locator('#ev-trace').fill('trace_mock_t1');
    await page.locator('#ev-filter').click();
    await page.waitForFunction(() => !document.querySelector('#ev-refresh').disabled);
    const filtered = (await calls()).findLast(call => call.url.includes('/console/events'));
    assert.ok(filtered.url.includes('source=deploy-hook') && filtered.url.includes('traceId=trace_mock_t1'));
    assert.ok(!filtered.url.includes('continue='));
    assert.ok(filtered.response.items.every(event => event.spec.traceUID === 'trace_mock_t1'));
    await page.locator('#ev-namespace').selectOption('prod');
    await page.waitForFunction(() => document.querySelector('#ev-list').textContent.includes('No events'));
    assert.ok((await calls()).findLast(c => c.url.includes('/console/events')).url.includes('namespace=prod'));
    await page.locator('#ev-source').fill(''); await page.locator('#ev-trace').fill('');
    await page.locator('#ev-filter').click();
    await page.waitForFunction(() => document.querySelectorAll('#ev-list article').length === 2);
    const foreign = await page.evaluate(async token => {
      const response = await fetch('/v1/console/events?namespace=prod&continue=' + encodeURIComponent(token));
      return { status: response.status, body: await response.json() };
    }, first.metadata.continue);
    assert.equal(foreign.status, 400); assert.equal(foreign.body.reason, 'invalid_continue');
    await page.locator('#ev-list details').first().locator('summary').click();
    assert.match(await page.locator('#ev-list').innerText(), /Normal|Warning/);
    assert.match(await page.locator('#ev-list').innerText(), /deliveryId/);
    check('Actual source+trace filter reset and namespace cursor isolation/empty-to-nonempty');
    await shot('pagination-filters-scope'); await clean('events interaction');
  }
  if (mode === 'index') {
    await goto('index', 'namespace=default');
    assert.equal(await page.locator('#kpi-total').innerText(), '4');
    assert.equal(await page.locator('#kpi-today').innerText(), '4');
    assert.equal(await page.locator('#kpi-running').innerText(), '3');
    assert.equal(await page.locator('#kpi-failed').innerText(), '1');
    assert.equal(await page.locator('#kpi-devices').textContent(), '0');
    assert.equal(await page.locator('#kpi-rate').textContent(), '–');
    assert.equal(await count('[data-push-tile]'), 2);
    assert.equal(await count('#index-events [data-uid]'), 4);
    assert.equal(await count('#hm .hm-cell'), 371);
    assert.equal(await count('#flow-host.flograph--silk'), 1);
    assert.ok(await count('#flow-host svg'));
    assert.equal(await count('#flow-host .sl-node'), 7);
    const paths = (await calls()).map(c => c.url);
    assert.ok(paths.some(url => url === '/v1/stats?namespace=default'));
    assert.ok(paths.some(url => url === '/v1/namespaces/default/traces/trace_mock_t1'));
    assert.ok(paths.every(url => !url.startsWith('/v1/tasks')));
    assert.equal(await count('[data-nonpush-tile] .stat-spark svg'), 0);
    assert.equal(await count('[data-spark-scope="events"] svg'), 2);
    await shot('statistics-overview');
    await page.locator('#flow-host').scrollIntoViewIfNeeded();
    await shot('statistics-graph');
    await archiveCalls();
    await page.locator('#flow-open').click();
    await page.waitForURL('**/trace.html?namespace=default&traceUID=trace_mock_t1');
    await page.locator('#trace-tasks [data-task]').first().waitFor();
    check('Genuine namespace statistics/heatmap/events/graph, retained zero-device KPI, actual trace click');
    await clean('index-to-trace'); await shot('statistics-trace-link');
  }
  if (mode === 'trace') {
    await goto('trace', 'namespace=default&traceUID=trace_mock_t1');
    assert.equal(await count('#trace-tasks [data-task]'), 7);
    assert.equal(await count('#trace-graph.flograph--silk'), 1);
    assert.ok(await count('#trace-graph svg'));
    assert.equal(await count('#trace-graph .sl-node'), 7);
    assert.match(await page.locator('#trace-pruned').innerText(), /param_schema/);
    assert.match(await page.locator('#trace-events').innerText(), /capability.emit/);
    assert.equal((await calls()).filter(c => c.url.includes('/traces/')).length, 1);
    assert.match(await page.locator('#trace-tasks [data-task="tsk_mock_4"]').innerText(), /3\/3/);
    await shot('nonempty-graph-pruned');
    await archiveCalls();
    await page.locator('#trace-tasks [data-task="tsk_mock_5"] a').click();
    await page.waitForURL('**/message.html?namespace=default&id=tsk_mock_5');
    await page.locator('#task-phase').waitFor();
    assert.match(await page.locator('#task-facts').innerText(), /retry_backoff/);
    check('Actual single Trace readmodel, nonempty v4 graph/pruned/attempt facts and Task deep link');
    await clean('trace-to-task');
  }
  if (mode === 'message') {
    await goto('message', 'namespace=default&id=tsk_mock_5');
    assert.match(await page.locator('#task-facts').innerText(), /retry_backoff/);
    assert.match(await page.locator('#task-facts').innerText(), /namespaceVarsRevision/);
    assert.match(await page.locator('#task-facts').innerText(), /leaseExpiresAt/);
    await page.locator('#logs-previous').selectOption('true');
    await page.waitForFunction(() => document.querySelector('#logs-output').textContent.includes('infra_transient'));
    assert.match(await page.locator('#logs-status').innerText(), /Truncated/);
    await page.locator('#task-logs').scrollIntoViewIfNeeded();
    await shot('actual-prior-attempt-logs');
    page.once('dialog', dialog => dialog.accept());
    await page.locator('#task-cancel').click();
    await page.waitForFunction(() => document.querySelector('#task-phase').dataset.phase === 'cancelled');
    assert.ok(await page.locator('#task-abandon').isDisabled());
    check('Actual waiting facts/previous archive logs/truncation/confirmed cancel terminal state');
    await shot('waiting-previous-cancelled');
    await goto('message', 'namespace=default&id=tsk_mock_6');
    page.once('dialog', dialog => dialog.accept());
    await page.locator('#task-abandon').click();
    await page.waitForFunction(() => document.querySelector('#task-phase').dataset.phase === 'abandoned');
    check('Actual running-task confirmed abandon terminal state, real unavailable log state');
    await goto('message', 'namespace=default&id=tsk_mock_2');
    assert.equal(await count('#emissions-list [data-event-id]'), 2);
    await page.locator('#emissions-more').click();
    await page.waitForFunction(() => document.querySelectorAll('#emissions-list [data-event-id]').length === 3);
    assert.match(await page.locator('#emissions-list').innerText(), /param_schema/);
    assert.match(await page.locator('#task-env').innerText(), /SOURCE_ROOT/);
    const envPairs = await page.locator('#task-env dl > div').evaluateAll(rows => rows.map(row => ({
      name: row.querySelector('dt').textContent, source: row.querySelector('dd').textContent,
    })));
    const diagnostics = (await calls()).findLast(call => call.url === '/v1/namespaces/default/tasks/tsk_mock_2').response.status.envDiagnostics;
    assert.ok(diagnostics.every(fact => Object.keys(fact).sort().join(',') === 'name,source'));
    assert.deepEqual(envPairs, diagnostics);
    assert.deepEqual(envPairs, [
      { name: 'SOURCE_ROOT', source: 'local' },
      { name: 'GIT_TERMINAL_PROMPT', source: 'node' },
    ]);
    assert.ok(!/ghp_secret|hmac_mock_secret|never-print-value/.test(await page.locator('#task-env').innerText()));
    const artifact = page.locator('#task-result a');
    const href = await artifact.getAttribute('href');
    assert.match(href, /^\/v1\/namespaces\/default\/tasks\/tsk_mock_2\/artifacts\/[a-f0-9]{64}$/);
    const downloading = page.waitForEvent('download');
    await artifact.click();
    const download = await downloading;
    assert.equal(download.suggestedFilename(), 'report.md');
    const data = await readFile(await download.path());
    assert.equal(createHash('sha256').update(data).digest('hex'), href.split('/').at(-1));
    check('Actual emissions continue pagination and managed artifact download hash, env names only');
    await shot('emissions-env-managed-artifact');
    await goto('message', 'namespace=default&id=tsk_mock_4');
    await page.locator('#task-result a').click();
    await page.waitForFunction(() => document.querySelector('#task-result').textContent.includes('410 artifact_expired'));
    check('Actual expired artifact410 shown while retained manifest remains');
    await clean('message diagnostics'); await shot('artifact-expired410');
  }
  if (mode === 'errors') {
    await goto('index', 'namespace=default');
    const disconnected = (await calls()).filter(c => c.url.includes('/console/events') && c.error);
    assert.ok(disconnected.length > 0, 'Actual Event transport requests fail after same-case navigation');
    assert.ok(disconnected.every(c => c.error.includes('deliberate disconnected transport') && c.httpStatus === undefined), 'Transport failure is not a normal HTTP200 or an empty EventList');
    assert.equal(await page.evaluate(() => window.__f3Fault), '/console/events', 'Fault survives navigation until explicit clear');
    assert.equal(await count('#flow-host button'), 1, 'Graph failure has a real retry action');
    assert.ok(!(await page.locator('#flow-host').innerText()).includes('No events'));
    assert.ok(await count('#index-events button'));
    assert.equal(await page.locator('#kpi-total').innerText(), '4');
    check('Disconnected Event read: graph/list show real errors while genuine statistics remain');
    await page.locator('#flow-host').scrollIntoViewIfNeeded();
    await shot('index-event-error-not-empty');
    await page.evaluate(() => window.__f3Fault = '');
    const beforeRetry = (await calls()).length;
    await page.locator('#flow-host button').click();
    await page.waitForFunction(before => window.__f3Calls.slice(before).some(c => c.url.includes('/console/events') && c.httpStatus === 200 && c.result?.kind === 'EventList'), beforeRetry);
    await page.locator('#flow-host svg').first().waitFor();
    assert.ok(!(await page.locator('#flow-host').innerText()).includes('disconnected'), 'Native retry replaces the Graph error with a real graph');
    await page.waitForFunction(() => { const list=document.querySelector('#index-events'); return !!list?.textContent.trim() && !list.textContent.includes('disconnected'); });
    assert.equal(await count('#index-events button'), 0, 'The same native refresh also restores the Event list, without another retry');
    const recovered = (await calls()).slice(beforeRetry).filter(c => c.url.includes('/console/events'));
    assert.ok(recovered.length > 0 && recovered.every(c => !c.error && c.httpStatus === 200 && c.result?.kind === 'EventList'), 'Cleared fault yields only actual successful Event requests, not stale failure evidence');
    assert.equal(await page.evaluate(() => sessionStorage.getItem('f3-transport-fault')), null, 'Explicit clear does not leave a fault in the next document');
    check('Actual disconnected requests persist across navigation; the unique native Graph retry recovers HTTP200/EventList/SVG');
    await goto('message', 'namespace=default&id=tsk_mock_5');
    await req('/v1/namespaces/default/tasks/tsk_mock_5/cancel', { method: 'POST', body: '{}' });
    page.once('dialog', dialog => dialog.accept());
    await page.locator('#task-abandon').click();
    await page.waitForFunction(() => document.querySelector('#task-action-error button'));
    const rejected = (await calls()).findLast(call => call.url.endsWith('/abandon'));
    assert.equal(rejected.status, 409);
    assert.equal(await page.locator('#task-phase').getAttribute('data-phase'), 'pending');
    await page.locator('#msg-refresh').click();
    await page.waitForFunction(() => document.querySelector('#task-phase').dataset.phase === 'cancelled');
    assert.ok(await page.locator('#task-abandon').isDisabled());
    check('Concurrent actual cancel produces visible abandon409, no optimistic terminal, explicit refresh confirms');
    await shot('task-action409-refreshed-terminal');
  }

}
