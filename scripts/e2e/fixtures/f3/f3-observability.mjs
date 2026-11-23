// Attributed F2b2 browser interactions promoted to registered F3 cases.
//
// 两种权威，两条事实来源（#335 第三轮）：
//   · mock（E2E_MOCK_ONLY=1，显式浏览器预览）：事实是 web/mock.js 的种子，断言写死
//     种子里的固定值（4 条事件 / 7 个节点 / trace_mock_t1 …）。
//   · real（默认）：事实只能由产品自己长出来。夹具用真实事件入口 + 一个守协议的
//     节点（fixtures/f3/f3-node.mjs）造出**同语义**的事实，断言按真实派生值对齐：
//     KPI = /v1/stats 与 TaskList 的真实计数；trace 节点数 = readmodel.tasks；
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
// #378：原生 confirm() 已下线，危险操作的确认层由 #anotify-dialog 承载 ——
// 驱动必须点真实按钮（Playwright 的 dialog 事件不会再触发）。
import { acceptDialog } from './f3-dialogs.mjs';
export async function observability(d, mode) {
 const {page,goto,calls,req,check,shot,clean,archive:archiveCalls}=d;
 const count=sel=>page.locator(sel).count();
 /* 事件面板的「查询已落下」锚点 —— 用真实条件而不是时长。
  *
  * 病根（#493）：events 页的查询是**事件驱动**的（change / submit / refresh 各发一轮），
  * 每一轮先把 `#ev-list` 换成骨架行（reset() → skeletonRows()），再等 fetch 回来才画行。
  * 骨架行没有文本 ⇒ 这个窗口里 `innerText`/`textContent` 都是空串。断言如果在窗口里读，
  * 拿到的是空气，红成 `did not match /Ingested/. Input: ''`（= CI 上 observe-events 的偶发红）。
  *
  * 所以读面板内容前必须等「这一轮真的画完」：没有 aria-busy、也没有骨架行。
  * 注意不能靠 `#ev-list article` 计数当锚点：上一轮的旧行在被顶掉之前也满足计数，
  * 而骨架窗口里计数是 0 —— 计数只说明「某一刻」，不说明「当前这一轮」。 */
 const settledList = () => page.waitForFunction(() => {
   const list = document.getElementById('ev-list');
   return !!list && !list.hasAttribute('aria-busy') && !list.querySelector('.ev-skel-row');
 }, null, { timeout: 15000 });
 if(mode==='errors') { await page.goto(d.base+'/en/index.html?namespace=default',{waitUntil:'domcontentloaded'}); await page.waitForFunction(()=>!!window.Anotify); await page.evaluate(()=>window.__f3Fault='/console/events'); }
  if (mode === 'events') {
    await goto('events', 'namespace=default');
    /* B-3：筛选栏**全即时** —— 每个控件 change 即查询，不再有「应用筛选」按钮。
     * selectOption 自带 change；文本框先用 fill（它只发 input），再补**同一个 change**
     * （浏览器在失焦时补的就是这个事件）—— 断的就是「change 即查询」这条接线本身。 */
    assert.equal(await page.locator('#ev-filter').count(), 0, 'events 页不得再有「应用筛选」按钮');
    const fireChange = (sel) => page.locator(sel).dispatchEvent('change');
    await page.locator('#ev-limit').selectOption('2');
    await page.waitForFunction(() => document.querySelectorAll('#ev-list article').length === 2);
    const first = (await calls()).findLast(call => call.url.includes('/console/events')).response;
    assert.equal(first.kind, 'EventList'); assert.ok(first.metadata.continue);
    // D-2 的反面：还有下一页时它必须真的看得见，否则下面的 click 就是点空气。
    assert.equal(await page.locator('#ev-more').isVisible(), true, '还有下一页时「加载更多」必须可见');
    await page.locator('#ev-more').click();
    await page.waitForFunction(() => document.querySelectorAll('#ev-list article').length === 4);
    // D-2：这一页已经到底（所有事件都在）⇒「加载更多」整块 hidden，而不是只置灰占位。
    await page.waitForFunction(() => document.getElementById('ev-more').hidden === true);
    assert.equal(await page.locator('#ev-more').isVisible(), false, '到底后「加载更多」必须整块 hidden');
    await page.locator('#ev-refresh').click();
    await page.waitForFunction(() => !document.querySelector('#ev-refresh').disabled);
    assert.equal(await count('#ev-list article'), 4);
    const uids = await page.locator('#ev-list article').evaluateAll(nodes => nodes.map(n => n.dataset.uid));
    assert.equal(new Set(uids).size, 4);
    check('Actual EventList pagination and first-page UID refresh dedup');
    await page.locator('#ev-source').fill(d.fx('source'));
    await fireChange('#ev-source');
    await page.locator('#ev-trace').fill(d.fx('defaultTrace'));
    await fireChange('#ev-trace');
    /* 两个 change 各发一次查询；等到**同时带两个参数**的那一发落地再断言（先到的那发
       只有 source，拿它当结果就是拿一个中间态当事实）。 */
    // 等的是**已经拿到响应**的那一发（__f3Calls 在 fetch 之前就 push，只等 url 会拿到半条记录）。
    await page.waitForFunction(([source, trace]) => window.__f3Calls.some(call =>
      call.url.includes('/console/events') && !!call.response &&
      call.url.includes('source=' + encodeURIComponent(source)) &&
      call.url.includes('traceId=' + encodeURIComponent(trace)) &&
      !call.url.includes('continue=')), [d.fx('source'), d.fx('defaultTrace')]);
    const filtered = (await calls()).filter(call => call.url.includes('/console/events') && call.response).at(-1);
    assert.ok(filtered.url.includes('source=' + encodeURIComponent(d.fx('source'))) && filtered.url.includes('traceId=' + encodeURIComponent(d.fx('defaultTrace'))));
    assert.ok(!filtered.url.includes('continue='));
    assert.ok(filtered.response.items.every(event => event.spec.traceUID === d.fx('defaultTrace')));
    await page.locator('#ev-namespace').selectOption('prod');
    await page.waitForFunction(() => document.querySelector('#ev-list').textContent.includes('No events'));
    assert.ok((await calls()).findLast(c => c.url.includes('/console/events')).url.includes('namespace=prod'));
    await page.locator('#ev-source').fill(''); await fireChange('#ev-source');
    await page.locator('#ev-trace').fill(''); await fireChange('#ev-trace');
    await page.waitForFunction(() => document.querySelectorAll('#ev-list article').length === 2);
    const foreign = await page.evaluate(async token => {
      const response = await fetch('/v1/console/events?namespace=prod&continue=' + encodeURIComponent(token));
      return { status: response.status, body: await response.json() };
    }, first.metadata.continue);
    // cursor 绑 ns+筛选条件（api_cas.go·decodeCursor）：拿默认 ns 的游标读 prod 必须
    // 400（真实 reason 是 BadRequest，"它就是不属于这次查询"），不是静默回第一页。
    assert.equal(foreign.status, 400);
    assert.match(String(foreign.body.message), /continue token/);
    await page.locator('#ev-list details').first().locator('summary').click();
    /* 点击本身可能刚触发一轮查询（点走焦点 ⇒ 文本框补 change ⇒ 页面 change 即查询）：
     * 等这一轮画完再读面板，否则读到骨架行（空串）。 */
    await settledList();
    /* 严重度字段 `spec.type` 在 v4 **不存在**（events.yaml 刻意不带 Normal/Warning）。
     * 之前 mock 硬塞了它，把「真实数据里第三个 chip 是空药丸」这个 bug 藏住了；
     * mock 已对齐真实 wire，这里两种模式都断言**真实字段** reason。 */
    const opened = (await calls()).find(c => c.url.includes('/console/events'))?.response?.items?.[0];
    assert.ok(opened?.spec?.reason, '事件必须带 spec.reason');
    assert.match(await page.locator('#ev-list').innerText(), new RegExp(opened.spec.reason));
    /* 空 chip 守卫：v4 没有 severity，页面不许再渲染空的 .ev-chip/.ix-chip——
     * 空 chip 被 44px 最小触控尺寸撑成空圆圈（用户实拍的那张图）。 */
    const emptyChips = await page.locator('#ev-list .ev-chip').evaluateAll((els) =>
      els.filter((e) => !(e.textContent || '').trim()).length);
    assert.equal(emptyChips, 0, '事件行不得有空的 chip（v4 没有 severity 字段）');
    // #374：deliveryId 是投递层内部标识，收进行内「明细」折叠 —— 断言改读 textContent（含折叠内容）
    assert.match(await page.locator('#ev-list').textContent(), /deliveryId/);
    check('Actual source+trace filter reset and namespace cursor isolation/empty-to-nonempty');
    await shot('pagination-filters-scope'); await clean('events interaction');
  }
  if (mode === 'index') {
    if (d.mock) {
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
      return;
    }
    /* 真实权威：期望值从**同一份真实事实**算。
     *   · kpi-total / kpi-today 是 /v1/stats 的**事件**口径（total/today）；
     *   · kpi-running / kpi-failed 是 TaskList 的真实相位计数（pending+running / failed）；
     *   · 事件条数 = 页面同一条件（limit=8）的 console events。 */
    const stats = await d.api('/v1/stats?namespace=default');
    assert.equal(stats.status, 200);
    assert.ok(Number.isInteger(stats.body.total) && Number.isInteger(stats.body.today), '/v1/stats 必须给 total/today 数字');
    const taskList = await d.api('/v1/namespaces/default/tasks?limit=500');
    assert.equal(taskList.status, 200); assert.equal(taskList.body.kind, 'TaskList');
    const phases = taskList.body.items.map(t => t.status.phase);
    const working = phases.filter(p => p === 'pending' || p === 'running').length;
    const failed = phases.filter(p => p === 'failed').length;
    assert.ok(phases.length >= 4, '夹具必须长出多条真实任务，实际 ' + phases.length);
    assert.ok(working > 0 && failed > 0, '真实任务里必须同时有在途与失败（实际 ' + JSON.stringify(phases) + '）');
    const eventPage = await d.api('/v1/console/events?namespace=default&limit=8');
    assert.equal(eventPage.status, 200); assert.equal(eventPage.body.kind, 'EventList');
    const latestWithTrace = eventPage.body.items.find(e => e.spec.traceUID);
    assert.ok(latestWithTrace, 'console events 必须带 traceUID');
    const flowTrace = await d.api('/v1/namespaces/default/traces/' + latestWithTrace.spec.traceUID);
    assert.equal(flowTrace.status, 200);
    await goto('index', 'namespace=default');
    assert.equal(await page.locator('#kpi-total').innerText(), String(stats.body.total));
    assert.equal(await page.locator('#kpi-today').innerText(), String(stats.body.today));
    assert.equal(await page.locator('#kpi-running').innerText(), String(working));
    assert.equal(await page.locator('#kpi-failed').innerText(), String(failed));
    assert.equal(await page.locator('#kpi-devices').textContent(), '0');
    assert.equal(await page.locator('#kpi-rate').textContent(), '–');
    /* C-1（结构性）：已经有事件 ⇒ 「从 0 到第一条事件」与「接入你的 Agent」两块都收起。
       两块留在 DOM 里只切 hidden（排版/锚点不变），标记照旧置上——收起不等于没渲染过。 */
    assert.ok(stats.body.total > 0, '夹具必须让这个命名空间真的有事件（实际 total=' + stats.body.total + '）');
    assert.equal(await page.locator('#first-event').evaluate(n => n.hidden), true, '有事件时三步引导必须 hidden');
    assert.equal(await page.locator('#quickstart').evaluate(n => n.hidden), true, '有事件时「接入你的 Agent」必须 hidden');
    assert.equal(await page.locator('#first-event').getAttribute('data-ready'), '1', '收起不等于未渲染：data-ready 仍置 1');
    assert.equal(await page.locator('#first-event').getAttribute('data-complete'), '1', '收起不等于未渲染：data-complete 仍置 1');
    /* D-1（结构性）：失败 KPI 整个包在一个真链接里（数字本身可点）。目标页的 `status`
       参数还没有服务端过滤（api/events.yaml 里没有 status）——这是已登记的遗留，不伪造。 */
    const failedLink = page.locator('#kpi-failed-link');
    assert.equal(await failedLink.count(), 1, '失败 KPI 必须有入口链接 #kpi-failed-link');
    assert.equal(await failedLink.evaluate(n => n.tagName), 'A', '入口必须是 <a>（不是 div + onclick）');
    assert.equal(await failedLink.getAttribute('href'), 'events.html?status=failed', '入口指向事件流的失败过滤');
    assert.equal(await failedLink.locator('#kpi-failed').count(), 1, '链接必须包住 KPI 数字本身（#kpi-failed）');
    /* C-2 的另一半：没有对比数据时不得写「暂无对比」这种散文 —— 要么收起并留空文本，
       要么是真实数字（药丸里带数字）。 */
    const trend = page.locator('#kpi-total-trend');
    const trendHidden = await trend.evaluate(n => n.hidden);
    const trendText = (await trend.textContent()).trim();
    if (trendHidden) assert.equal(trendText, '', '趋势药丸收起时不得留文字（实际：' + trendText + '）');
    else assert.match(trendText, /[0-9]/, '趋势药丸要么收起，要么是真实数字（不得写「暂无对比」）：' + trendText);
    assert.equal(await count('[data-push-tile]'), 2);
    assert.equal(await count('#index-events [data-uid]'), eventPage.body.items.length);
    assert.equal(await count('#hm .hm-cell'), 371);
    assert.equal(await count('#flow-host.flograph--silk'), 1);
    assert.ok(await count('#flow-host svg'));
    // 首页链路卡与 trace / 源页同一套：底图（源的配置拓扑）+ 执行染色。
    // 所以图里有触发器节点 + 拓扑里的能力，而不是"只画这条 trace 的任务"。
    assert.equal(await count('#flow-host [data-node-id="__trigger__"]'), 1,
      '首页必须画出触发器节点（与其他页对齐）');
    const flowIds = await page.$$eval('#flow-host [data-node-id]', (ns) => ns.map((n) => n.getAttribute('data-node-id')));
    assert.ok(flowIds.length >= flowTrace.body.tasks.length + 1,
      '底图节点应包含触发器 + 执行过的能力，实际 ' + JSON.stringify(flowIds));
    const paths = (await calls()).map(c => c.url);
    assert.ok(paths.some(url => url === '/v1/stats?namespace=default'));
    assert.ok(paths.some(url => url === '/v1/namespaces/default/traces/' + latestWithTrace.spec.traceUID));
    assert.ok(paths.every(url => !url.startsWith('/v1/tasks')));
    assert.equal(await count('[data-nonpush-tile] .stat-spark svg'), 0);
    assert.equal(await count('[data-spark-scope="events"] svg'), 2);
    await shot('statistics-overview');
    await page.locator('#flow-host').scrollIntoViewIfNeeded();
    await shot('statistics-graph');
    await archiveCalls();
    await page.locator('#flow-open').click();
    await page.waitForURL('**/trace.html?namespace=default&traceUID=' + latestWithTrace.spec.traceUID);
    await page.locator('#trace-tasks [data-task]').first().waitFor();
    assert.equal(await count('#trace-tasks [data-task]'), flowTrace.body.tasks.length);
    check('Genuine namespace statistics/heatmap/events/graph, retained zero-device KPI, actual trace click');
    await clean('index-to-trace'); await shot('statistics-trace-link');
  }
  if (mode === 'trace') {
    if (d.mock) {
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
      return;
    }
    /* 真实多跳链 trace（夹具：chain-root ─emit→ chain-mid ─emit→ chain-leaf，外加一条
     * 向非法 params schema 能力的 emit 被 route 剪枝）。断言按 readmodel 的真实事实对齐：
     * 节点数 = tasks 数（没有底图时 graphs.js 走 useTaskIds，不为触发器画节点）。 */
    const traceId = d.fx('chainTrace');
    const model = await d.api('/v1/namespaces/default/traces/' + traceId);
    assert.equal(model.status, 200);
    assert.ok(model.body.tasks.length >= 3, '夹具必须造出多跳链，实际 ' + model.body.tasks.length + ' 个任务');
    assert.ok(model.body.pruned.length >= 1, '夹具必须造出真实剪枝，实际 ' + model.body.pruned.length);
    await goto('trace', 'namespace=default&traceUID=' + traceId);
    assert.equal(await count('#trace-tasks [data-task]'), model.body.tasks.length);
    assert.equal(await count('#trace-graph.flograph--silk'), 1);
    assert.ok(await count('#trace-graph svg'));
    assert.equal(await count('#trace-graph .sl-node'), model.body.tasks.length);
    // 剪枝面板必须逐条显示 readmodel 里的真实原因码（夹具造的是 param_schema）。
    for (const fact of model.body.pruned) {
      assert.equal(fact.spec.eventType, 'param_schema');
      assert.match(await page.locator('#trace-pruned').innerText(), new RegExp(fact.spec.eventType));
    }
    // 事件面板必须显示 readmodel 里每条事件的真实 eventType（v4 没有 mock 的
    // `capability.emit`：剪枝观察行的 type 就是原因码）。
    const eventsText = await page.locator('#trace-events').innerText();
    for (const event of model.body.events) assert.ok(eventsText.includes(event.spec.eventType), '事件面板必须显示 ' + event.spec.eventType);
    assert.equal((await calls()).filter(c => c.url.includes('/traces/')).length, 1);
    const firstTask = model.body.tasks[0];
    assert.match(await page.locator('#trace-tasks [data-task="' + firstTask.metadata.name + '"]').innerText(),
      new RegExp(firstTask.status.attempt + '/' + firstTask.status.maxAttempts));
    await shot('nonempty-graph-pruned');
    await archiveCalls();
    await page.locator('#trace-tasks [data-task="' + firstTask.metadata.name + '"] a').click();
    await page.waitForURL('**/message.html?namespace=default&id=' + firstTask.metadata.name);
    await page.locator('#task-phase').waitFor();
    assert.equal(await page.locator('#task-phase').getAttribute('data-phase'), firstTask.status.phase);
    const factsText = await page.locator('#task-facts').innerText();
    assert.match(factsText, new RegExp(firstTask.status.attempt + '/' + firstTask.status.maxAttempts));
    // #374：configRevision / namespaceVarsRevision 收进「开发者信息」折叠 ——
    // 断言改读折叠区（textContent 含折叠内容），事实本身仍然被验证。
    const devText = await page.locator('#task-facts #task-dev').textContent();
    assert.match(devText, /namespaceVarsRevision/);
    assert.match(devText, new RegExp('configRevision[\\s\\S]*' + firstTask.status.configRevision));
    check('Actual multi-hop Trace readmodel, real task-node graph/pruned codes/attempt facts and Task deep link');
    await clean('trace-to-task');
  }
  if (mode === 'message') {
    if (d.mock) {
      await goto('message', 'namespace=default&id=tsk_mock_5');
      assert.match(await page.locator('#task-facts').innerText(), /retry_backoff/);
      assert.match(await page.locator('#task-facts #task-dev').textContent(), /namespaceVarsRevision/);
      assert.match(await page.locator('#task-facts').innerText(), /leaseExpiresAt/);
      await page.locator('#logs-previous').selectOption('true');
      await page.waitForFunction(() => document.querySelector('#logs-output').textContent.includes('infra_transient'));
      assert.match(await page.locator('#logs-status').innerText(), /Truncated/);
      await page.locator('#task-logs').scrollIntoViewIfNeeded();
      await shot('actual-prior-attempt-logs');
      await acceptDialog(page, () => page.locator('#task-cancel').click());
      await page.waitForFunction(() => document.querySelector('#task-phase').dataset.phase === 'cancelled');
      assert.ok(await page.locator('#task-abandon').isDisabled());
      check('Actual waiting facts/previous archive logs/truncation/confirmed cancel terminal state');
      await shot('waiting-previous-cancelled');
      await goto('message', 'namespace=default&id=tsk_mock_6');
      await acceptDialog(page, () => page.locator('#task-abandon').click());
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
      return;
    }
    /* 真实权威：message 页的每一块事实都由最近一次真实执行产生 ——
     *   · 在跑任务：真实 lease（leaseExpiresAt/attempt 都是产品写的行）；
     *     日志档案由执行它的节点回答 logs_request（truncated=true）；
     *   · pending 任务：真取消 → cancelled 终态（不乐观上屏）；
     *   · 成功任务：3 条真实 emission（2 条建子任务 + 1 条 param_schema 剪枝，分页 2+1）
     *     + 一条**真实上传**的产物（下载字节的 sha256 就是路径里的 sha256）；
     *   · 产物副本被保留期清掉的任务：下载必须 410 artifact_expired。 */
    const runningTask = d.fx('runningTask');
    const running = await d.api('/v1/namespaces/default/tasks/' + runningTask);
    assert.equal(running.status, 200);
    assert.equal(running.body.status.phase, 'running');
    await goto('message', 'namespace=default&id=' + runningTask);
    const factsText = await page.locator('#task-facts').innerText();
    assert.match(factsText, /waitingReason/);
    assert.match(factsText, /leaseExpiresAt/);
    assert.match(factsText, new RegExp(running.body.status.attempt + '/' + running.body.status.maxAttempts));
    // 任务行的真实配置版本（不是 mock 的固定 7/12）。
    // #374：revision 字段收进「开发者信息」折叠 —— 断言改读折叠区
    const devText = await page.locator('#task-facts #task-dev').textContent();
    assert.match(devText, new RegExp('configRevision[\\s\\S]*' + running.body.status.configRevision));
    assert.match(devText, new RegExp('namespaceVarsRevision[\\s\\S]*' + running.body.status.namespaceVarsRevision));
    // 当前 attempt 的日志：中枢转发 logs_request，节点回 logs_response（truncated=true）。
    const logLines = d.fx('runningLogLines');
    await page.waitForFunction(text => document.querySelector('#logs-output').textContent.includes(text), logLines[1]);
    assert.match(await page.locator('#logs-status').innerText(), /Truncated/);
    assert.match(await page.locator('#logs-status').innerText(), new RegExp('\\b' + running.body.status.attempt + '\\b'));
    // previous=true 且没有上一次执行：契约唯一允许的空结果（attempt 0 / lines []）。
    await page.locator('#logs-previous').selectOption('true');
    // 空结果的可见文案来自 locale（message.v4.logs_empty）；等它出现，证明页面真的
    // 拿到了「200 + attempt 0 + lines []」而不是把 410/超时画成"什么都没打印"。
    await page.waitForFunction(() => document.querySelector('#logs-status').textContent.includes('0') && document.querySelector('#logs-output').textContent.includes('No lines for this attempt'));
    const previous = await d.api('/v1/namespaces/default/tasks/' + runningTask + '/logs?previous=true&tailLines=100');
    assert.equal(previous.status, 200);
    assert.equal(previous.body.attempt, 0);
    assert.deepEqual(previous.body.lines, []);
    await page.locator('#task-logs').scrollIntoViewIfNeeded();
    await shot('actual-attempt-logs');
    // pending 任务：真取消 → cancelled 终态。
    const pendingTask = d.fx('pendingTask');
    await goto('message', 'namespace=default&id=' + pendingTask);
    await acceptDialog(page, () => page.locator('#task-cancel').click());
    await page.waitForFunction(() => document.querySelector('#task-phase').dataset.phase === 'cancelled');
    assert.ok(await page.locator('#task-abandon').isDisabled());
    assert.equal((await d.api('/v1/namespaces/default/tasks/' + pendingTask)).body.status.phase, 'cancelled');
    check('Actual waiting facts/real node log archive truncation/confirmed cancel terminal state');
    await shot('waiting-logs-cancelled');
    // 在跑任务：真放弃 → abandoned 终态（节点回 cancel.ack{reaped:true}）。
    await goto('message', 'namespace=default&id=' + runningTask);
    await acceptDialog(page, () => page.locator('#task-abandon').click());
    await page.waitForFunction(() => document.querySelector('#task-phase').dataset.phase === 'abandoned');
    assert.equal((await d.api('/v1/namespaces/default/tasks/' + runningTask)).body.status.phase, 'abandoned');
    check('Actual running-task confirmed abandon terminal state, real unavailable log state');
    // 成功任务：emission 分页 + 真实产物下载。
    const messageTask = d.fx('messageTask');
    await goto('message', 'namespace=default&id=' + messageTask);
    assert.equal(await count('#emissions-list [data-event-id]'), 2);
    await page.locator('#emissions-more').click();
    await page.waitForFunction(() => document.querySelectorAll('#emissions-list [data-event-id]').length === 3);
    assert.match(await page.locator('#emissions-list').innerText(), /param_schema/);
    const envPairs = await page.locator('#task-env dl > div').evaluateAll(rows => rows.map(row => ({
      name: row.querySelector('dt').textContent, source: row.querySelector('dd').textContent,
    })));
    const detail = (await calls()).findLast(call => call.url === '/v1/namespaces/default/tasks/' + messageTask).response;
    // envDiagnostics 是 TaskStatus 的契约字段（resource.yaml·TaskEnvDiagnostic）：页面必须
    // 逐条等于服务端给的那份（只 name/source，没有值）。
    const diagnostics = detail.status.envDiagnostics || [];
    assert.deepEqual(envPairs, diagnostics.map(fact => ({ name: fact.name, source: fact.source })));
    assert.ok(envPairs.every(pair => Object.keys(pair).sort().join(',') === 'name,source'));
    // 真实产物：下载字节的 sha256 必须等于 URL 里的 sha256（中枢 spool 的真实副本）。
    const artifact = page.locator('#task-result a');
    const href = await artifact.getAttribute('href');
    assert.match(href, new RegExp('^/v1/namespaces/default/tasks/' + messageTask + '/artifacts/[a-f0-9]{64}$'));
    const downloading = page.waitForEvent('download');
    await artifact.click();
    const download = await downloading;
    assert.equal(download.suggestedFilename(), 'report.md');
    const data = await readFile(await download.path());
    assert.equal(createHash('sha256').update(data).digest('hex'), href.split('/').at(-1));
    check('Actual emissions continue pagination and managed artifact download hash, env names only');
    await shot('emissions-env-managed-artifact');
    // 产物副本被保留期清掉：元数据在、副本不在 → 410 artifact_expired。
    await goto('message', 'namespace=default&id=' + d.fx('expiredTask'));
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
    /* #378：loadErrorNode 现在固定带「复制详情」（报障要机器事实），所以"按钮恰好 1 个"
       不再是事实；断言改成"恰好 1 个**重试动作**" —— 意图（错误态里有真重试）不变且更准。 */
    assert.equal(await count('#flow-host [data-err-action="retry"]'), 1, 'Graph failure has a real retry action');
    assert.ok(!(await page.locator('#flow-host').innerText()).includes('No events'));
    assert.ok(await count('#index-events button'));
    // KPI 的 total 是事件口径：真实权威下它等于 /v1/stats 的 total（mock 下是种子里的 4）。
    const total = d.mock ? '4' : String((await d.api('/v1/stats?namespace=default')).body.total);
    assert.equal(await page.locator('#kpi-total').innerText(), total);
    check('Disconnected Event read: graph/list show real errors while genuine statistics remain');
    await page.locator('#flow-host').scrollIntoViewIfNeeded();
    await shot('index-event-error-not-empty');
    await page.evaluate(() => window.__f3Fault = '');
    const beforeRetry = (await calls()).length;
    await page.locator('#flow-host [data-err-action="retry"]').click();
    await page.waitForFunction(before => window.__f3Calls.slice(before).some(c => c.url.includes('/console/events') && c.httpStatus === 200 && c.result?.kind === 'EventList'), beforeRetry);
    await page.locator('#flow-host svg').first().waitFor();
    assert.ok(!(await page.locator('#flow-host').innerText()).includes('disconnected'), 'Native retry replaces the Graph error with a real graph');
    await page.waitForFunction(() => { const list=document.querySelector('#index-events'); return !!list?.textContent.trim() && !list.textContent.includes('disconnected'); });
    assert.equal(await count('#index-events button'), 0, 'The same native refresh also restores the Event list, without another retry');
    const recovered = (await calls()).slice(beforeRetry).filter(c => c.url.includes('/console/events'));
    assert.ok(recovered.length > 0 && recovered.every(c => !c.error && c.httpStatus === 200 && c.result?.kind === 'EventList'), 'Cleared fault yields only actual successful Event requests, not stale failure evidence');
    assert.equal(await page.evaluate(() => sessionStorage.getItem('f3-transport-fault')), null, 'Explicit clear does not leave a fault in the next document');
    check('Actual disconnected requests persist across navigation; the unique native Graph retry recovers HTTP200/EventList/SVG');
    const pendingTask = d.fx('pendingTask');
    await goto('message', 'namespace=default&id=' + pendingTask);
    await req('/v1/namespaces/default/tasks/' + pendingTask + '/cancel', { method: 'POST', body: '{}' });
    await acceptDialog(page, () => page.locator('#task-abandon').click());
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
