// 首页总览默认「全部命名空间」聚合（契约 .reports/overview-allns-contract.md · D1/D2/D5/D7/D9）。
//
// 为什么单独一个文件：`f3-cases.mjs` 是注册表（本次只加一行 `add`），实现在这里，
// 形状照 `f3-observability.mjs` 的 `observability(d, mode)`：拿 driver 的
// `goto` / `api` / `check`，复用 `f3-driver.mjs` 的 realFixtures（default 与 prod
// **各有一套**数据，所以「全部」与「单个 ns」天然可区分）。
//
// 不新增套件：`parallel_runner.mjs` 的套件注册表大小与端口表是冻结常量，本次用不着
// 多一个套件，用例挂在既有 `frontend` 套件里（它由执行器点名为 **real authority**，
// `E2E_MOCK_ONLY` 会被剥掉）。
//
// 期望值**一个数字都不写死**：全部模式的聚合期望从真实 API 现算
// （`/v1/stats?namespace=*` + `/v1/namespaces` 的 `status.stats.tasksByPhase`），
// 单 ns 期望从 `/v1/stats?namespace=prod` 现算。写死数字等于把「夹具数据量没变」
// 当成被测行为。
//
// 断言锚点只用契约 D5/D7 冻结的 DOM（`[data-ns-id="*"]` / `[data-ns-badge]` /
// `#latest-flow[data-scope-ns]` / `#scope-crumb` 与既有 KPI id），不改产品代码迁就测试。
import assert from 'node:assert/strict';

// 冻结锚点（D5/D7）：全部模式下必须还在的既有 id。
const FROZEN_IDS = [
  '#kpi-total',
  '#kpi-today',
  '#kpi-rate',
  '#kpi-devices',
  '#kpi-running',
  '#kpi-failed',
  '#index-events',
  '#hm .hm-cell',
  '#flow-host',
  '#scope-crumb',
];
// 热力图是固定 53 周 × 7 天窗口，与 ns 无关（D2「同一 371 天窗口」）。
const HM_CELLS = 371;
// 本用例固定 en（注册时不传 options.lang → driver 默认 'en'），所以「全部命名空间」
// 这一项的文案就是 locale 里 `scope.all_namespaces` 的英文值。
const ALL_LABEL = 'All namespaces';
// 夹具只建这两个命名空间（f3-driver 的 realBootstrap / ensureNamespace）。
const ALL_NS = ['default', 'prod'];

// 页面里 `toLocaleString()` 用的是浏览器 locale（本用例 en）。这里显式 en-US：
// 小数字两者一致，将来数据变大也不会因为千分位形状不同而假红。
const num = (n) => Number(n).toLocaleString('en-US');

// D2 的求和口径：在途 = Σ(pending + running)，失败 = Σ failed。
// `tasksByPhase` 是 NamespaceAggregate 的 required 字段（api/resource.yaml），
// 缺了就是产品契约破裂 —— 直接断言，不静默当成 0。
function sumPhases(items, phases, source) {
  let sum = 0;
  for (const item of items) {
    const byPhase = item?.status?.stats?.tasksByPhase;
    assert.ok(
      byPhase && typeof byPhase === 'object',
      source + '：每个命名空间都必须带 status.stats.tasksByPhase（resource.yaml 的 required），实际 ' +
        JSON.stringify(item),
    );
    for (const phase of phases) {
      if (Number.isFinite(byPhase[phase])) sum += byPhase[phase];
    }
  }
  return sum;
}

// 首页 KPI 的既有格式化规则（web-src/pages/index.html · renderKpis）：
//   · total / running / failed 走 toLocaleString()（running 命中 500 上限才补 +，全部模式不发生）
//   · today 直接写数字（不本地化）
//   · rate = 先 Σdelivered / ΣdeliveryTotal 再相除，无设备或无投递样本时是 –（绝不是 0%）
//   · devices 是用户维度（不随 ns 求和），0 写成 "0"
function kpiFrom(stats, counts) {
  const devices = stats.deviceCount ?? 0;
  const deliveryTotal = stats.deliveryTotal ?? 0;
  const rate =
    devices > 0 && deliveryTotal > 0
      ? Math.round(((stats.delivered ?? 0) / deliveryTotal) * 1000) / 10
      : null;
  return {
    total: num(stats.total ?? 0),
    today: String(stats.today ?? 0),
    rate: rate == null ? '–' : rate + '%',
    devices: String(devices || '0'),
    running: counts ? num(counts.working) : null,
    failed: counts ? num(counts.failed) : null,
  };
}

const readKpis = (page) =>
  page.evaluate(() => {
    const text = (id) => (document.getElementById(id)?.textContent || '').trim();
    return {
      total: text('kpi-total'),
      today: text('kpi-today'),
      rate: text('kpi-rate'),
      devices: text('kpi-devices'),
      running: text('kpi-running'),
      failed: text('kpi-failed'),
    };
  });

// 一次取全 D5/D7 的 DOM 事实（选择器选中态 / 面包屑 / 事件行 ns 徽标 / 链路卡 ns）。
const readScope = (page) =>
  page.evaluate(() => {
    const all = document.querySelector('[data-ns-id="*"]');
    const attr = (node, name) => (node ? node.getAttribute(name) : null);
    const trimmed = (node) => (node?.textContent || '').trim();
    return {
      allChecked: attr(all, 'aria-checked'),
      allRole: attr(all, 'role'),
      allClass: all ? all.className : '',
      allName: trimmed(all?.querySelector('.scope-item-name')),
      concreteChecked: Array.from(document.querySelectorAll('[data-ns-id]'))
        .filter(
          (n) =>
            n.getAttribute('data-ns-id') !== '*' &&
            n.getAttribute('aria-checked') === 'true',
        )
        .map((n) => n.getAttribute('data-ns-id')),
      prodChecked: attr(document.querySelector('[data-ns-id="prod"]'), 'aria-checked'),
      crumb: trimmed(document.getElementById('scope-crumb')),
      // 事件行：data-uid 是既有锚点，ns 徽标是全部模式新增的（每行回显事件自己的 ns）。
      rows: Array.from(document.querySelectorAll('#index-events [data-uid]')).map(
        (row) => ({
          uid: row.getAttribute('data-uid'),
          badge: attr(row.querySelector('[data-ns-badge]'), 'data-ns-badge'),
        }),
      ),
      badgeCount: document.querySelectorAll('#index-events [data-ns-badge]').length,
      flowNs: attr(document.getElementById('latest-flow'), 'data-scope-ns'),
      hmCells: document.querySelectorAll('#hm .hm-cell').length,
    };
  });

// 等页面真的把这一轮读模型画完（不用 sleep）：KPI 已离开占位 –、事件列表撤了
// aria-busy、链路卡已经有内容。三者都是**真实条件**，不是时长。
const settle = async (page) => {
  await page.waitForFunction(
    () => {
      const list = document.getElementById('index-events');
      const total = document.getElementById('kpi-total');
      return (
        !!list &&
        !list.hasAttribute('aria-busy') &&
        !!total &&
        total.textContent.trim() !== '–'
      );
    },
    null,
    { timeout: 20000 },
  );
  await page.waitForFunction(
    () => {
      const host = document.getElementById('flow-host');
      return !!host && !host.hasAttribute('aria-busy') && host.children.length > 0;
    },
    null,
    { timeout: 20000 },
  );
};

export async function overviewAll(d) {
  const { page, goto, check } = d;
  assert.ok(
    !d.mock,
    'observe-index-all 只属于 real authority（frontend 套件由执行器剥掉 E2E_MOCK_ONLY）：' +
      '全部模式的聚合期望值必须从真实 API 现算，mock 预览没有后端',
  );

  /* ================= 从真实 API 现算期望值 ================= */
  const allStats = await d.api('/v1/stats?namespace=*');
  assert.equal(
    allStats.status,
    200,
    'GET /v1/stats?namespace=* 必须 200（跨 ns 聚合读），实际 ' +
      allStats.status +
      ' ' +
      JSON.stringify(allStats.body),
  );
  const defStats = await d.api('/v1/stats?namespace=default');
  assert.equal(defStats.status, 200, 'default 的单 ns stats 必须 200，实际 ' + defStats.status);
  const prodStats = await d.api('/v1/stats?namespace=prod');
  assert.equal(prodStats.status, 200, 'prod 的单 ns stats 必须 200，实际 ' + prodStats.status);
  // 聚合 = 逐 ns 相加（D2/D4）。这条同时挡住「`*` 其实只读了一个 ns」这种假聚合：
  // 那种实现会让下面的聚合断言与页面一起错到同一个值上。
  assert.equal(
    allStats.body.total,
    (defStats.body.total ?? 0) + (prodStats.body.total ?? 0),
    '`?namespace=*` 的 total 必须等于各 ns 之和，实际 ' +
      allStats.body.total +
      '（default ' +
      defStats.body.total +
      ' + prod ' +
      prodStats.body.total +
      '）',
  );
  // 两个 ns 的读数必须可区分，否则「带上 namespace 就只显示该 ns」这条用例不具判别力。
  assert.notEqual(
    prodStats.body.total,
    allStats.body.total,
    '夹具必须让 prod 与聚合值可区分（实际 prod=' +
      prodStats.body.total +
      '，聚合=' +
      allStats.body.total +
      '）：数据量变了就补事件，不要放宽断言',
  );

  const nsList = await d.api('/v1/namespaces');
  assert.equal(nsList.status, 200, 'GET /v1/namespaces 必须 200，实际 ' + nsList.status);
  assert.equal(nsList.body.kind, 'NamespaceList', '命名空间列表必须是 NamespaceList 信封');
  const nsItems = nsList.body.items || [];
  const EXPECTED_ALL = kpiFrom(allStats.body, {
    working: sumPhases(nsItems, ['pending', 'running'], '全部模式的在途计数'),
    failed: sumPhases(nsItems, ['failed'], '全部模式的失败计数'),
  });
  const EXPECTED_PROD = kpiFrom(prodStats.body, null);

  const allEvents = await d.api('/v1/console/events?namespace=*&limit=8');
  assert.equal(allEvents.status, 200, '跨 ns 事件流必须 200，实际 ' + allEvents.status);
  assert.equal(allEvents.body.kind, 'EventList', '事件流必须是 EventList 信封');
  const allEventItems = allEvents.body.items || [];
  assert.ok(allEventItems.length > 0, '夹具必须让跨 ns 事件流非空（否则事件/链路卡断言是空转）');
  // 每一行必须回显事件**自己**的 ns（D2 §3.2），这是徽标断言的期望来源。
  const nsByUid = new Map(allEventItems.map((e) => [e.metadata.uid, e.metadata.namespace]));
  for (const [uid, ns] of nsByUid) {
    assert.ok(
      ALL_NS.includes(ns),
      '事件 ' + uid + ' 的 metadata.namespace 必须落在这两个夹具 ns 里（' + ALL_NS.join('|') + '），实际 ' + ns,
    );
  }
  // 链路卡不是聚合：它画的是「最新一条带 traceUID 的事件」**自己**那条 trace（D2）。
  const latestWithTrace = allEventItems.find((e) => e.spec?.traceUID);
  assert.ok(latestWithTrace, '跨 ns 事件流必须至少有一条带 traceUID（链路卡的数据源）');
  const EXPECTED_FLOW_NS = latestWithTrace.metadata.namespace;

  const prodEvents = await d.api('/v1/console/events?namespace=prod&limit=8');
  assert.equal(prodEvents.status, 200, 'prod 的事件流必须 200，实际 ' + prodEvents.status);
  const prodUids = (prodEvents.body.items || []).map((e) => e.metadata.uid).sort();

  /* ================= 1. 裸 index.html（无 ?namespace=）= 全部聚合 ================= */
  await goto('index', '');
  await settle(page);
  const allKpis = await readKpis(page);
  for (const expected of [
    ['total', EXPECTED_ALL.total],
    ['today', EXPECTED_ALL.today],
    ['rate', EXPECTED_ALL.rate],
    ['devices', EXPECTED_ALL.devices],
    ['running', EXPECTED_ALL.running],
    ['failed', EXPECTED_ALL.failed],
  ]) {
    assert.equal(
      allKpis[expected[0]],
      expected[1],
      '全部模式 #kpi-' +
        expected[0] +
        ' 必须等于从真实 API 现算的聚合值，实际 ' +
        allKpis[expected[0]] +
        '，期望 ' +
        expected[1],
    );
  }
  for (const selector of FROZEN_IDS) {
    const hits = await page.locator(selector).count();
    assert.ok(
      hits > 0,
      '全部模式首页必须保住既有锚点 ' + selector + '（D5：既有 id 不变），实际命中 ' + hits,
    );
  }
  check('ALL-OBSERVE bare index.html KPIs equal /v1/stats?namespace=* + Σ/v1/namespaces (total/today/rate/devices/running/failed), frozen ids intact');

  /* ================= 2. 全部模式的选择器 / 面包屑 / 徽标 / 链路卡 ================= */
  const allScope = await readScope(page);
  assert.equal(
    allScope.allChecked,
    'true',
    '全部模式必须选中「全部命名空间」项（[data-ns-id="*"][aria-checked="true"]），实际 ' +
      allScope.allChecked,
  );
  assert.equal(
    allScope.allRole,
    'menuitemradio',
    '「全部命名空间」项必须是 role=menuitemradio（D7 复用既有 scope-item 语义），实际 ' +
      allScope.allRole,
  );
  assert.ok(
    allScope.allClass.split(/\s+/).includes('scope-item'),
    '「全部命名空间」项必须是 button.scope-item，实际 class=' + allScope.allClass,
  );
  assert.deepEqual(
    allScope.concreteChecked,
    [],
    '全部模式下不许有任何具体命名空间处于选中态，实际 ' +
      JSON.stringify(allScope.concreteChecked),
  );
  assert.equal(
    allScope.allName,
    ALL_LABEL,
    '选择器里「全部」那一项的名字必须是 i18n scope.all_namespaces，实际 ' + allScope.allName,
  );
  assert.equal(
    allScope.crumb,
    ALL_LABEL,
    '全部模式下 #scope-crumb 文本必须是 i18n scope.all_namespaces，实际 ' + allScope.crumb,
  );
  assert.equal(
    allScope.rows.length,
    allEventItems.length,
    '事件行数必须等于跨 ns 事件流（limit=8）的条数，实际页面 ' +
      allScope.rows.length +
      '，接口 ' +
      allEventItems.length,
  );
  for (const row of allScope.rows) {
    assert.ok(
      row.badge !== null,
      '全部模式每条事件行都必须带 [data-ns-badge]，实际事件 ' + row.uid + ' 没有',
    );
    assert.ok(
      ALL_NS.includes(row.badge),
      '事件行的 ns 徽标必须属于 ' + ALL_NS.join('|') + '，实际 ' + row.uid + ' → ' + row.badge,
    );
    assert.equal(
      row.badge,
      nsByUid.get(row.uid),
      '事件行的 ns 徽标必须是这条事件**自己**的 ns，实际 ' + row.uid + ' → ' + row.badge,
    );
  }
  assert.equal(
    allScope.badgeCount,
    allEventItems.length,
    '每条事件行都要有且只有一个 ns 徽标，实际徽标 ' +
      allScope.badgeCount +
      ' / 事件行 ' +
      allScope.rows.length,
  );
  assert.ok(
    ALL_NS.includes(allScope.flowNs),
    '全部模式 #latest-flow 必须带 data-scope-ns 且属于 ' + ALL_NS.join('|') + '，实际 ' + allScope.flowNs,
  );
  assert.equal(
    allScope.flowNs,
    EXPECTED_FLOW_NS,
    '链路卡 data-scope-ns 必须是「最新带 traceUID 事件」自己的 ns，实际 ' +
      allScope.flowNs +
      '，期望 ' +
      EXPECTED_FLOW_NS,
  );
  assert.equal(
    allScope.hmCells,
    HM_CELLS,
    '热力图仍是 53 周 × 7 天的 ' + HM_CELLS + ' 格，实际 ' + allScope.hmCells,
  );
  check('ALL-SCOPE all-namespaces item checked, crumb=i18n scope.all_namespaces, per-row data-ns-badge = event own ns, latest-flow data-scope-ns, 371 heatmap cells');

  /* ================= 3. ?namespace=prod 只显示该 ns ================= */
  await goto('index', 'namespace=prod');
  await settle(page);
  const prodKpis = await readKpis(page);
  assert.equal(
    prodKpis.total,
    EXPECTED_PROD.total,
    '单 ns 模式 #kpi-total 必须等于 /v1/stats?namespace=prod 的 total，实际 ' +
      prodKpis.total +
      '，期望 ' +
      EXPECTED_PROD.total,
  );
  const prodScope = await readScope(page);
  assert.equal(
    prodScope.prodChecked,
    'true',
    '单 ns 模式必须选中 prod（[data-ns-id="prod"][aria-checked="true"]），实际 ' +
      prodScope.prodChecked,
  );
  assert.equal(
    prodScope.allChecked,
    'false',
    '单 ns 模式下「全部命名空间」项必须是 aria-checked="false"，实际 ' + prodScope.allChecked,
  );
  assert.equal(
    prodScope.crumb,
    '#prod',
    '单 ns 模式的面包屑仍是 #<name>，实际 ' + prodScope.crumb,
  );
  assert.equal(
    prodScope.badgeCount,
    0,
    '单 ns 模式不得渲染 ns 徽标（整页就一个 ns，徽标只是噪音），实际 ' + prodScope.badgeCount,
  );
  assert.equal(
    prodScope.flowNs,
    null,
    '单 ns 模式的链路卡不带 data-scope-ns（那是「全部」的标注），实际 ' + prodScope.flowNs,
  );
  const prodPageUids = prodScope.rows.map((row) => row.uid).sort();
  assert.deepEqual(
    prodPageUids,
    prodUids,
    '单 ns 模式的事件行必须正好是 prod 的事件流（uid 全集），实际页面 ' +
      JSON.stringify(prodPageUids) +
      '，接口 ' +
      JSON.stringify(prodUids),
  );
  check('ALL-SCOPE-PROD ?namespace=prod shows only prod (kpi=prod stats.total, prod checked/* unchecked, no data-ns-badge, uids all prod)');

  /* ================= 4. 切换同步：裸 index.html → 点 prod ================= */
  await goto('index', '');
  await settle(page);
  await page.locator('#scope-switcher:not(.hidden)').waitFor();
  await page.locator('.scope-trigger').click();
  await page.locator('#scope-switcher [data-ns-id="prod"]').click();
  await page.waitForURL((url) => url.searchParams.get('namespace') === 'prod', {
    timeout: 20000,
    waitUntil: 'domcontentloaded',
  });
  await settle(page);
  assert.ok(
    page.url().includes('namespace=prod'),
    '在首页选具体命名空间必须导航到带 namespace=prod 的 URL（URL 是唯一真源），实际 ' + page.url(),
  );
  const afterProdSwitch = await readKpis(page);
  assert.equal(
    afterProdSwitch.total,
    EXPECTED_PROD.total,
    '切到 prod 后 #kpi-total 必须变成 prod 的单值（' +
      EXPECTED_PROD.total +
      '），实际 ' +
      afterProdSwitch.total,
  );
  const switchedScope = await readScope(page);
  assert.equal(
    switchedScope.prodChecked,
    'true',
    '切到 prod 之后 prod 项必须是选中态，实际 ' + switchedScope.prodChecked,
  );
  check('ALL-SWITCH-TO-PROD clicking [data-ns-id="prod"] on bare index navigates to namespace=prod and KPI becomes prod single value');

  /* ================= 5. 切换同步：?namespace=prod → 点「全部」 ================= */
  await page.locator('.scope-trigger').click();
  await page.locator('#scope-switcher [data-ns-id="*"]').click();
  await page.waitForURL((url) => !url.searchParams.has('namespace'), {
    timeout: 20000,
    waitUntil: 'domcontentloaded',
  });
  await settle(page);
  assert.ok(
    !page.url().includes('namespace='),
    '在首页选「全部命名空间」必须导航到不带 namespace 的 URL，实际 ' + page.url(),
  );
  const afterAllSwitch = await readKpis(page);
  assert.equal(
    afterAllSwitch.total,
    EXPECTED_ALL.total,
    '切回「全部」后 #kpi-total 必须回到聚合值（' +
      EXPECTED_ALL.total +
      '），实际 ' +
      afterAllSwitch.total,
  );
  assert.equal(
    (await readScope(page)).allChecked,
    'true',
    '切回「全部」之后 [data-ns-id="*"] 必须是选中态',
  );
  check('ALL-SWITCH-TO-ALL clicking [data-ns-id="*"] drops namespace= from URL and KPI returns to the aggregate value');

  /* ================= 6. 刷新保持（深链可分享 / 刷新不跑偏） ================= */
  await page.reload({ waitUntil: 'domcontentloaded' });
  await settle(page);
  const reloadedAllKpis = await readKpis(page);
  const reloadedAllScope = await readScope(page);
  assert.equal(
    reloadedAllKpis.total,
    EXPECTED_ALL.total,
    '刷新裸首页后 #kpi-total 必须仍是聚合值 ' + EXPECTED_ALL.total + '，实际 ' + reloadedAllKpis.total,
  );
  assert.equal(
    reloadedAllScope.allChecked,
    'true',
    '刷新裸首页后「全部命名空间」仍是选中态，实际 ' + reloadedAllScope.allChecked,
  );
  assert.deepEqual(
    reloadedAllScope.concreteChecked,
    [],
    '刷新裸首页后不许有具体 ns 被选中，实际 ' + JSON.stringify(reloadedAllScope.concreteChecked),
  );
  assert.equal(
    reloadedAllScope.badgeCount,
    allEventItems.length,
    '刷新裸首页后事件行仍逐行带 ns 徽标，实际徽标 ' + reloadedAllScope.badgeCount,
  );
  await goto('index', 'namespace=prod');
  await page.reload({ waitUntil: 'domcontentloaded' });
  await settle(page);
  const reloadedProdKpis = await readKpis(page);
  const reloadedProdScope = await readScope(page);
  assert.ok(
    page.url().includes('namespace=prod'),
    '刷新之后 URL 里的 namespace=prod 必须仍在（深链可分享），实际 ' + page.url(),
  );
  assert.equal(
    reloadedProdKpis.total,
    EXPECTED_PROD.total,
    '刷新 prod 深链后 #kpi-total 必须仍是 prod 的单值 ' + EXPECTED_PROD.total + '，实际 ' + reloadedProdKpis.total,
  );
  assert.equal(
    reloadedProdScope.prodChecked,
    'true',
    '刷新 prod 深链后 prod 项仍是选中态，实际 ' + reloadedProdScope.prodChecked,
  );
  assert.equal(
    reloadedProdScope.allChecked,
    'false',
    '刷新 prod 深链后「全部」项仍是未选中，实际 ' + reloadedProdScope.allChecked,
  );
  assert.equal(
    reloadedProdScope.badgeCount,
    0,
    '刷新 prod 深链后事件行仍不带 ns 徽标，实际 ' + reloadedProdScope.badgeCount,
  );
  check('ALL-RELOAD reload keeps both modes (bare index = aggregate, ?namespace=prod = prod single value) with scope state and badges unchanged');
}
