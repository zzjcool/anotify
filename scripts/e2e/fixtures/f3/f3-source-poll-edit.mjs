import assert from 'node:assert/strict';

const SOURCE = 'poll-edit';
const NEW_SOURCE = 'poll-edit-new';
const QUIET_EXPR = '0 0 1 1 *';
const EDIT_EXPR = '0 0 1 7 *';
const SECRET = 'f3-poll-edit-credential';
const BASE = '/v1/namespaces/default/sources';
const PATH = BASE + '/';
const CONFIG = {
  url: 'https://api.github.com/advisories?per_page=20',
  headers: { 'X-F3-Trace': 'poll-edit', Accept: 'application/json' },
  secretRef: { name: 'f3-poll-cred', header: 'Authorization', prefix: 'Bearer ' },
  secret: SECRET,
  schedule: { expr: QUIET_EXPR, tz: 'UTC' },
  timeoutMs: 12000,
  maxItems: 100,
  allowInsecure: true,
  envelope: { items: '.', type: { pointer: 'type' }, deliveryId: { pointer: 'id' }, payload: { whole: true } },
};

const clone = (v) => JSON.parse(JSON.stringify(v));

export async function sourcePollEdit(d) {
  const { page, goto, api, response, check, shot, clean } = d;
  assert.ok(!d.mock, 'source-poll-edit 只在真实权威下有意义（要读真服务端的 config）');
  const visible = (sel, timeout = 15000) => page.locator(sel).waitFor({ state: 'visible', timeout });
  const doc = () => api(PATH + SOURCE);
  const json = (body) => ({ method: 'POST', body: JSON.stringify(body), headers: { 'Content-Type': 'application/json' } });

  await goto('sources');
  const stale = await doc();
  if (stale.status === 200) {
    const removed = await response(PATH + SOURCE + '?resourceVersion=' + encodeURIComponent(stale.body.metadata.resourceVersion), null, 'DELETE');
    assert.equal(removed.status, 204, '上一次尝试留下的同名源必须先删掉，实际 ' + removed.status);
  }
  assert.equal(stale.status === 200 || stale.status === 404, true, '读同名源只允许 200 或 404，实际 ' + stale.status);
  const created = await api(BASE, json({ apiVersion: 'anotify/v1', kind: 'Source', metadata: { name: SOURCE }, spec: { kind: 'poll', config: CONFIG, targets: [] } }));
  assert.equal(created.status, 201, '建 poll 源必须 201，实际 ' + created.status + ' ' + JSON.stringify(created.body).slice(0, 300));
  const before = await doc();
  assert.equal(before.status, 200);
  const beforeCfg = before.body.spec.config;
  assert.equal(beforeCfg.allowInsecure, true, '夹具必须是「表单不提供的键也有值」的那种源，否则下面「不丢配置」的断言什么都没证');
  assert.deepEqual(beforeCfg.secretRef, CONFIG.secretRef);
  assert.equal(JSON.stringify(before.body).includes(SECRET), false, '读面不得回显凭据明文');
  check('POLL-EDIT-01 a real poll source with headers + secretRef + allowInsecure + envelope is created and reads back without the secret');

  await goto('sources', 'namespace=default&edit=' + SOURCE);
  await visible('#src-modal:not(.hidden)');
  await page.waitForFunction((name) => document.querySelector('#f-name')?.value === name, SOURCE, { timeout: 20000 });
  assert.equal(await page.locator('#f-kind').inputValue(), 'poll', '编辑 poll 源时下拉必须选中 poll 而不是回落到 manual');
  assert.equal(await page.locator('#f-kind').isDisabled(), true, '已存在源的 kind 下拉必须是禁用的（不是「能点但保存失败」）');
  await visible('#f-poll-wrap:not(.hidden)');
  assert.equal(await page.locator('#f-hook-wrap').isHidden(), true, 'poll 不许显示 webhook 的凭据区块');
  await visible('#f-cron-wrap:not(.hidden)');
  assert.equal(await page.locator('#f-expr-label').innerText(), '排期', 'poll 的排期复用同一个表达式输入，标签要换成「排期」');
  assert.equal(await page.locator('#f-poll-url').inputValue(), CONFIG.url);
  assert.equal(await page.locator('#f-poll-timeout').inputValue(), String(CONFIG.timeoutMs));
  assert.equal(await page.locator('#f-poll-maxitems').inputValue(), String(CONFIG.maxItems));
  assert.equal(await page.locator('#f-expr').inputValue(), QUIET_EXPR, '排期表达式必须是存储值，不是 cron 的默认值');
  assert.equal(await page.locator('#f-tz').inputValue(), CONFIG.schedule.tz);
  check('POLL-EDIT-02 opening the editor for a poll source shows the transport form (url / limits / schedule) prefilled and keeps kind read-only');

  const flat = await page.$$eval('#f-poll-headers .source-target input', (ns) => ns.map((n) => n.value));
  const heads = [];
  for (let i = 0; i < flat.length; i += 2) heads.push(flat[i] + '=' + flat[i + 1]);
  assert.deepEqual(heads.sort(), ['Accept=application/json', 'X-F3-Trace=poll-edit'], 'headers 必须逐项回填成可编辑行，实际 ' + JSON.stringify(flat));
  assert.equal(await page.locator('#f-poll-cred-name').inputValue(), CONFIG.secretRef.name);
  assert.equal(await page.locator('#f-poll-cred-header').inputValue(), CONFIG.secretRef.header);
  assert.equal(await page.locator('#f-poll-cred-prefix').inputValue(), CONFIG.secretRef.prefix);
  assert.equal(await page.locator('#f-poll-secret').inputValue(), '', '凭据只写：编辑态必须是空框，不能回填任何明文');
  assert.equal(await page.locator('#f-env-items').inputValue(), '.');
  assert.equal(await page.locator('#f-env-type').inputValue(), 'type');
  assert.equal(await page.locator('#f-env-delivery').inputValue(), 'id');
  assert.equal(await page.locator('#f-env-payload-whole').isChecked(), true, 'payload: {whole: true} 必须还原成「整个条目」开关');
  assert.equal(await page.locator('#f-env-payload').isDisabled(), true, '勾了「整个条目」之后指针框必须禁用');
  assert.equal((await page.locator('#src-modal').innerText()).includes(SECRET), false, '弹层里不得出现凭据明文');
  await clean('poll editor 1440');
  await page.setViewportSize({ width: 360, height: 800 });
  const wide = await page.evaluate(() => {
    const out = [];
    for (const e of document.querySelectorAll('#src-modal #f-poll-wrap *')) {
      const r = e.getBoundingClientRect();
      if (r.width && r.right > 362) out.push({ tag: e.tagName, id: e.id, right: Math.round(r.right) });
    }
    return out;
  });
  assert.deepEqual(wide, [], '360px 下 poll 表单的每个元素都必须落在视口内，实际 ' + JSON.stringify(wide));
  await shot('poll-editor-360');
  await page.setViewportSize({ width: 1440, height: 900 });
  check('POLL-EDIT-03 headers / secretRef / envelope all round-trip into editable controls (secret stays an empty write-only box) and the form fits 360px');

  await page.locator('#f-poll-maxitems').fill('42');
  await page.locator('#f-expr').fill(EDIT_EXPR);
  const patchP = page.waitForRequest((r) => r.method() === 'PATCH' && r.url().includes(PATH + SOURCE), { timeout: 20000 });
  const patchRespP = page.waitForResponse((r) => r.request().method() === 'PATCH' && r.url().includes(PATH + SOURCE), { timeout: 20000 });
  await page.locator('#modal-save').click();
  const [patchReq, patchResp] = await Promise.all([patchP, patchRespP]);
  assert.equal(patchResp.status(), 200, '保存必须走既有 PATCH 路径并成功，实际 ' + patchResp.status());
  await page.locator('#src-modal').waitFor({ state: 'hidden', timeout: 20000 });
  const sent = JSON.parse(patchReq.postData() || '{}');
  assert.equal(sent.spec.kind, 'poll', 'PATCH 只回传同一个 kind（改 kind 会被 source_kind_immutable 拒）');
  assert.equal(sent.spec.config.maxItems, 42);
  assert.equal(sent.spec.config.schedule.expr, EDIT_EXPR);
  assert.equal(sent.spec.config.schedule.tz, 'UTC', '时区必须原样带上，不能回落成 Local');
  assert.equal('secret' in sent.spec.config, false, '凭据框留空 = 不改动，提交体里不许出现 secret');
  assert.deepEqual(sent.spec.config.secretRef, CONFIG.secretRef, '凭据引用必须原样带上');
  assert.deepEqual(sent.spec.config.headers, CONFIG.headers, 'headers 必须原样带上（否则保存一次就丢请求头）');
  assert.equal(sent.spec.config.allowInsecure, true, '表单不提供的键也必须留在提交体里');
  assert.deepEqual(sent.spec.config.envelope, CONFIG.envelope);
  check('POLL-EDIT-04 saving sends a complete PATCH: only maxItems / schedule.expr differ, headers + secretRef + allowInsecure + envelope ride along and secret is omitted');

  const after = await doc();
  assert.equal(after.status, 200);
  const afterCfg = after.body.spec.config;
  const want = clone(beforeCfg);
  want.maxItems = 42;
  want.schedule.expr = EDIT_EXPR;
  assert.deepEqual(afterCfg, want, '编辑 poll 源不许丢配置：保存前后 GET 的 config 只能差被改的那两个字段，实际 ' + JSON.stringify(afterCfg));
  assert.equal('secret' in afterCfg, false, '读回不许有 secret 明文');
  assert.equal(JSON.stringify(after.body).includes(SECRET), false, '读回不许夹带凭据明文');
  const monthOf = (stamp) => new Date(stamp).getUTCMonth();
  assert.equal(monthOf(before.body.status.nextFireAt), 0, '夹具的排期是 1 月 1 日，下次轮询必须落在 1 月，实际 ' + before.body.status.nextFireAt);
  assert.equal(monthOf(after.body.status.nextFireAt), 6, '改成 7 月 1 日之后，服务端必须按新表达式重算下次轮询（源仍在轮询），实际 ' + after.body.status.nextFireAt);
  check('POLL-EDIT-05 after the edit the config deep-equals the stored one except the two edited fields, and the scheduler re-arms from the new expression');

  await goto('source-detail', 'namespace=default&name=' + SOURCE, 'zh-CN');
  await visible('#src-detail-body [data-sec="poll"]');
  const kv = async (label) =>
    page.$$eval('#src-detail-body [data-sec="poll"] .sd-pair', (cells, want) => {
      const hit = cells.find((c) => ((c.querySelector('.sd-k') || {}).textContent || '').trim() === want);
      return hit ? ((hit.querySelector('.sd-v') || {}).textContent || '').trim() : '<missing ' + want + '>';
    }, label);
  assert.equal(await kv('单轮条目上限'), '42', '详情页必须显示保存后的新值');
  assert.equal(await kv('排期'), EDIT_EXPR + ' · UTC', '详情页的排期必须是新表达式，实际 ' + await kv('排期'));
  assert.equal(await kv('单次超时'), '12000 ms', '没改的超时必须保持原值');
  await goto('sources', 'namespace=default&edit=' + SOURCE);
  await page.waitForFunction((name) => document.querySelector('#f-name')?.value === name, SOURCE, { timeout: 20000 });
  assert.equal(await page.locator('#f-poll-maxitems').inputValue(), '42', '重新打开必须看到新值');
  assert.equal(await page.locator('#f-expr').inputValue(), EDIT_EXPR, '重新打开必须看到新排期');
  await page.locator('#modal-cancel').click();
  await page.locator('#src-modal').waitFor({ state: 'hidden', timeout: 20000 });
  check('POLL-EDIT-06 the detail page and a reopened editor both show the new maxItems / schedule.expr');

  const kindChange = await api(PATH + SOURCE, {
    method: 'PATCH',
    body: JSON.stringify({ apiVersion: 'anotify/v1', kind: 'Source', metadata: { resourceVersion: after.body.metadata.resourceVersion }, spec: { kind: 'manual' } }),
    headers: { 'Content-Type': 'application/json' },
  });
  assert.equal(kindChange.status, 400, '换 kind 必须 400（UI 那边是靠禁用下拉避免，不是靠保存失败）');
  assert.equal(kindChange.body.reason, 'source_kind_immutable');
  check('POLL-EDIT-07 an existing source still rejects a kind change with source_kind_immutable (the UI prevents it by disabling the select)');

  await goto('sources');
  await page.locator('#btn-new').click();
  await visible('#src-modal:not(.hidden)');
  await page.locator('#f-kind').selectOption('webhook');
  await page.locator('#f-token').fill('f3-poll-edit-token');
  await page.locator('#f-secret').fill('f3-poll-edit-hmac');
  await page.locator('#f-sample').fill(JSON.stringify({ f3: 'sample' }));
  await page.locator('#f-kind').selectOption('poll');
  assert.equal(await page.locator('#f-token').inputValue(), '', '切到 poll 之后 webhook 的 token 必须当场清掉');
  assert.equal(await page.locator('#f-secret').inputValue(), '', '切到 poll 之后 webhook 的 secret 必须当场清掉');
  assert.equal(await page.locator('#f-sample').inputValue(), '', '切到 poll 之后 webhook 的样例必须当场清掉');
  await page.locator('#f-name').fill(NEW_SOURCE);
  await page.locator('#f-poll-url').fill('https://example.com/f3-feed.json');
  await page.locator('#f-expr').fill(QUIET_EXPR);
  await page.locator('#f-tz').fill('UTC');
  await page.locator('#add-poll-header').click();
  const keys = page.locator('#f-poll-headers .source-target input');
  await keys.nth(0).fill('X-Dup');
  await keys.nth(2).fill('X-Dup');
  await page.locator('#modal-save').click();
  await visible('#f-poll-headers-error:not(.hidden)');
  assert.equal((await page.locator('#f-poll-headers-error').innerText()).startsWith('请求头重名'), true, '请求头重名必须在客户端拦住并说清是哪一个');
  assert.equal(await page.locator('#src-modal').isVisible(), true, '客户端校验失败不许关弹层');
  await page.locator('#modal-cancel').click();
  await page.locator('#src-modal').waitFor({ state: 'hidden', timeout: 20000 });
  await page.locator('#btn-new').click();
  await visible('#src-modal:not(.hidden)');
  assert.equal(await page.locator('#f-poll-headers-error').isHidden(), true, '重新打开时必须清掉上一次的字段错误');
  assert.equal(await page.locator('#f-poll-url').inputValue(), '', '重新打开必须是空表单');
  check('POLL-EDIT-09 a duplicate header name is blocked in the client (modal stays open) and a reopened form carries no stale field error');
  await page.locator('#f-kind').selectOption('poll');
  await page.locator('#f-name').fill(NEW_SOURCE);
  await page.locator('#f-poll-url').fill('https://example.com/f3-feed.json');
  await page.locator('#f-expr').fill(QUIET_EXPR);
  await page.locator('#f-tz').fill('UTC');
  const postP = page.waitForRequest((r) => r.method() === 'POST' && r.url().endsWith(BASE), { timeout: 20000 });
  const postRespP = page.waitForResponse((r) => r.request().method() === 'POST' && r.url().endsWith(BASE), { timeout: 20000 });
  await page.locator('#modal-save').click();
  const [postReq, postResp] = await Promise.all([postP, postRespP]);
  assert.equal(postResp.status(), 201, '新建 poll 源必须 201，实际 ' + postResp.status());
  await page.locator('#src-modal').waitFor({ state: 'hidden', timeout: 20000 });
  const postBody = JSON.parse(postReq.postData() || '{}');
  assert.equal(postBody.spec.kind, 'poll');
  for (const key of ['token', 'secret', 'sample', 'hookUrl', 'secretSet', 'expr']) {
    assert.equal(key in postBody.spec.config, false, '新建时切 kind 之后提交体里不许有 ' + key + '，实际 ' + JSON.stringify(postBody.spec.config));
  }
  assert.equal(postBody.spec.config.url, 'https://example.com/f3-feed.json');
  assert.equal(postBody.spec.config.schedule.expr, QUIET_EXPR);
  const fresh = await api(PATH + NEW_SOURCE);
  assert.equal(fresh.status, 200, '新建的 poll 源必须可读');
  assert.equal(fresh.body.spec.kind, 'poll');
  check('POLL-EDIT-08 switching kind on a new source clears the other kind fields in the form and in the POST body (no token / secret / sample leak into poll)');

  const gone = await response(PATH + NEW_SOURCE + '?resourceVersion=' + encodeURIComponent(fresh.body.metadata.resourceVersion), null, 'DELETE');
  assert.equal(gone.status, 204, '清理新建的源必须 204，实际 ' + gone.status);
}

const UNREADABLE = 'poll-edit-unreadable';

export async function sourcePollEditUnreadable(d) {
  const { page, goto, api, response, calls, check } = d;
  assert.ok(!d.mock, 'source-poll-edit-unreadable 只在真实权威下有意义（要真服务端回读原始 config）');
  const visible = (sel, timeout = 15000) => page.locator(sel).waitFor({ state: 'visible', timeout });
  const path = PATH + UNREADABLE;
  const post = (body) => ({ method: 'POST', body: JSON.stringify(body), headers: { 'Content-Type': 'application/json' } });
  const cfgOf = async (r) => r.body.spec.config;

  await goto('sources');
  const stale = await api(path);
  if (stale.status === 200) {
    const removed = await response(path + '?resourceVersion=' + encodeURIComponent(stale.body.metadata.resourceVersion), null, 'DELETE');
    assert.equal(removed.status, 204, '上一次尝试留下的同名源必须先删掉，实际 ' + removed.status);
  }
  const created = await api(BASE, post({ apiVersion: 'anotify/v1', kind: 'Source', metadata: { name: UNREADABLE }, spec: { kind: 'poll', config: CONFIG, targets: [] } }));
  assert.equal(created.status, 201, '建 poll 源必须 201，实际 ' + created.status + ' ' + JSON.stringify(created.body).slice(0, 300));
  const before = await cfgOf(await api(path));
  assert.deepEqual(before.headers, CONFIG.headers, '夹具前提：表单从不渲染的 headers 必须在存储里');
  assert.equal(before.allowInsecure, true, '夹具前提：表单从不渲染的 allowInsecure 必须在存储里（否则下面「不丢配置」什么都没证）');
  assert.deepEqual(before.envelope, CONFIG.envelope, '夹具前提：表单只渲染一部分 envelope 键，存储里必须有完整那份');
  check('POLL-EDIT-10 a poll source whose stored config carries keys the form never renders (headers / allowInsecure / envelope) exists');

  await goto('sources');
  await page.evaluate((p) => { window.__f3Fault = p; }, path);
  await goto('sources', 'namespace=default&edit=' + UNREADABLE);
  await visible('#src-modal:not(.hidden)');
  await visible('#form-result .source-error');
  const opened = await calls();
  const read = opened.filter((c) => c.method === 'GET' && c.url === path);
  assert.equal(read.length, 1, '打开编辑器必须恰好发一次原始 config 读，实际 ' + JSON.stringify(opened.map((c) => c.method + ' ' + c.url + ' (' + (c.httpStatus || c.error) + ')')));
  assert.equal(read[0].httpStatus, undefined, '这次读必须是失败的（注入的传输层故障真的打到了那一次读），实际 ' + read[0].httpStatus);
  const msg = (await page.locator('#form-result').innerText()).trim();
  assert.ok(msg.includes('读取源配置失败'), '读失败必须有看得见的提示（原来这条提示被紧随其后的 replaceChildren 立刻擦掉），实际 ' + JSON.stringify(msg));
  assert.ok(msg.includes('F3 deliberate disconnected transport'), '提示必须带上真实原因，实际 ' + JSON.stringify(msg));
  assert.equal(await page.locator('#f-poll-url').inputValue(), '', '读失败时表单只剩默认值 —— 这正是「保存一次就丢配置」的入口');
  assert.equal(await page.locator('#modal-save').isDisabled(), true, '源配置没读到时保存必须禁用，实际可用');
  assert.equal(await page.locator('#btn-dry').isDisabled(), true, '试运行用的是同一份半成品 config，同样必须禁用');
  assert.equal(await page.locator('#poll-reload').count(), 1, '必须给出重试入口');
  const geom = await page.evaluate(() => { // 「看得见」不能只证明元素有盒子：#form-result 在可滚动 .modal-body 末尾，poll 表单比视口高
    const msg = document.querySelector('#form-result .source-error');
    const body = document.getElementById('src-modal-body');
    const b = msg.getBoundingClientRect(), v = body.getBoundingClientRect();
    return {
      top: Math.round(b.top), bottom: Math.round(b.bottom),
      viewTop: Math.round(v.top), viewBottom: Math.round(v.bottom),
      scrollTop: Math.round(body.scrollTop), scrollHeight: body.scrollHeight, clientHeight: body.clientHeight,
    };
  });
  assert.equal(geom.scrollHeight > geom.clientHeight, true, '夹具前提：poll 表单必须高于弹层视口，否则下面「在视口内」测不到东西，实际 ' + JSON.stringify(geom));
  assert.equal(geom.scrollTop > 0, true, '修复必须把失败原因滚进视口（原来它停在折叠下方），实际 ' + JSON.stringify(geom));
  assert.equal(geom.top >= geom.viewTop && geom.bottom <= geom.viewBottom, true, '失败原因必须落在弹层可视区内，实际 ' + JSON.stringify(geom));
  await page.waitForFunction(() => (document.querySelector('#anotify-toast-host [data-error-bar]')?.textContent || '').includes('读取源配置失败'), null, { timeout: 15000 });
  const bar = (await page.locator('#anotify-toast-host [data-error-bar]').last().innerText()).trim();
  assert.ok(bar.includes('F3 deliberate disconnected transport'), '错误条也必须带上真实原因（它不随滚动位置消失），实际 ' + JSON.stringify(bar));
  assert.equal(await page.locator('#f-poll-url').isDisabled(), true, '读失败期间 poll 表单必须锁死：否则用户先填一份、再点重试，fillPollForm 会把刚填的静默覆盖');
  assert.equal(await page.locator('#f-expr').isDisabled(), true, '排期输入同样要锁（它也是保存时会写进 config 的键）');
  check('POLL-EDIT-11 a failed config read keeps the modal open with a *viewport-visible* error bar + in-form reason, an empty and locked form, disabled save/dry-run and a retry entry');

  await page.evaluate(() => { const b = document.getElementById('modal-save'); b.disabled = false; b.click(); });
  await page.waitForFunction(() => [...document.querySelectorAll('#anotify-toast-host [data-error-bar]')].some((n) => n.textContent.includes('已阻止')), null, { timeout: 15000 }); // 读失败时已有常驻错误条，必须扫全部而不是只看第一条
  assert.equal((await page.locator('#anotify-toast-host [data-error-bar]').last().innerText()).includes('已阻止'), true, '被拦下的保存必须给出看得见的原因');
  const writes = (await calls()).filter((c) => c.method !== 'GET' && c.url === path);
  assert.deepEqual(writes.map((c) => c.method), [], '即使按钮被强行启用，pollConfig() 也必须拦下这次写（防御层不能只有 disabled），实际 ' + JSON.stringify(writes.map((c) => c.method + ' ' + c.httpStatus)));
  assert.equal((await page.locator('#form-result').innerText()).includes('读取源配置失败'), true, '被拦下之后「为什么不能存」必须还看得见（不能被技术错误盖掉），实际 ' + JSON.stringify((await page.locator('#form-result').innerText()).trim()));
  assert.equal(await page.locator('#poll-reload').count(), 1, '被拦下之后重试入口必须还在');
  assert.equal(await page.locator('#src-modal').isVisible(), true, '被拦下的保存不许关弹层');
  const barsBefore = await page.locator('#anotify-toast-host [data-error-bar]').count(); // 只锁保存、不锁试运行的 catch 会把重试入口盖成通用错误
  await page.evaluate(() => { const b = document.getElementById('btn-dry'); b.disabled = false; b.click(); });
  await page.waitForFunction((n) => document.querySelectorAll('#anotify-toast-host [data-error-bar]').length > n, barsBefore, { timeout: 15000 });
  const dry = (await calls()).filter((c) => c.url === BASE + '/dry-run');
  assert.deepEqual(dry.map((c) => c.method), [], '试运行同样只拿得到半成品 config，强行启用也必须被拦下，实际 ' + JSON.stringify(dry.map((c) => c.method + ' ' + c.httpStatus)));
  assert.equal((await page.locator('#anotify-toast-host [data-error-bar]').last().innerText()).includes('已阻止'), true, '被拦下的试运行也必须说明原因，实际 ' + JSON.stringify((await page.locator('#anotify-toast-host [data-error-bar]').last().innerText()).trim()));
  assert.equal((await page.locator('#form-result').innerText()).includes('读取源配置失败'), true, '被拦下的试运行不能把重试入口盖成通用错误');
  assert.equal(await page.locator('#poll-reload').count(), 1, '被拦下的试运行之后重试入口必须还在');
  assert.equal(await page.locator('#src-modal').isVisible(), true, '被拦下的试运行不许关弹层');
  check('POLL-EDIT-12 saving *and* dry-run are refused even when force-enabled, the modal stays open and the retry entry survives both refusals');

  await page.evaluate(() => { window.__f3Fault = ''; });
  await page.locator('#poll-reload').click();
  await page.waitForFunction((want) => document.querySelector('#f-poll-url')?.value === want, CONFIG.url, { timeout: 20000 });
  assert.equal(await page.locator('#modal-save').isDisabled(), false, '重试成功后保存必须恢复可用');
  assert.equal(await page.locator('#f-poll-url').isDisabled(), false, '重试成功后表单必须解锁（否则读到配置也改不了）');
  assert.equal(await page.locator('#f-expr').isDisabled(), false, '重试成功后排期输入必须解锁');
  assert.equal(await page.locator('#form-result .source-error').count(), 0, '重试成功必须清掉错误提示');
  assert.equal(await page.locator('#f-expr').inputValue(), QUIET_EXPR, '重试成功必须回填存储的排期（不是 cron 默认值）');
  assert.equal(await page.locator('#f-tz').inputValue(), CONFIG.schedule.tz);
  assert.equal(await page.locator('#f-poll-cred-name').inputValue(), CONFIG.secretRef.name);
  assert.equal(await page.locator('#f-poll-cred-header').inputValue(), CONFIG.secretRef.header);
  const flat = await page.$$eval('#f-poll-headers .source-target input', (ns) => ns.map((n) => n.value));
  const heads = [];
  for (let i = 0; i < flat.length; i += 2) heads.push(flat[i] + '=' + flat[i + 1]);
  assert.deepEqual(heads.sort(), ['Accept=application/json', 'X-F3-Trace=poll-edit'], '重试成功必须把 headers 回填成可编辑行，实际 ' + JSON.stringify(flat));
  check('POLL-EDIT-13 retry clears the error, re-enables save and refills the stored url / schedule / headers / secretRef');

  await page.locator('#f-poll-maxitems').fill('43');
  const patchP = page.waitForRequest((r) => r.method() === 'PATCH' && r.url().includes(path), { timeout: 20000 });
  const patchRespP = page.waitForResponse((r) => r.request().method() === 'PATCH' && r.url().includes(path), { timeout: 20000 });
  await page.locator('#modal-save').click();
  const [patchReq, patchResp] = await Promise.all([patchP, patchRespP]);
  assert.equal(patchResp.status(), 200, '重试之后保存必须成功，实际 ' + patchResp.status());
  await page.locator('#src-modal').waitFor({ state: 'hidden', timeout: 20000 });
  const sent = JSON.parse(patchReq.postData() || '{}');
  assert.deepEqual(sent.spec.config.headers, CONFIG.headers, '保存必须带上 headers（只有拿到服务器基底才做得到），实际 ' + JSON.stringify(sent.spec.config.headers));
  assert.equal(sent.spec.config.allowInsecure, true, '表单不提供的 allowInsecure 必须原样留着');
  assert.deepEqual(sent.spec.config.envelope, CONFIG.envelope, 'envelope 必须原样留着');
  assert.deepEqual(sent.spec.config.schedule, CONFIG.schedule, '排期只能是存储的那份（改都没改）');
  assert.equal(sent.spec.config.maxItems, 43);
  assert.equal('secret' in sent.spec.config, false, '凭据框留空 = 不改动');
  const after = await cfgOf(await api(path));
  const want = clone(before);
  want.maxItems = 43;
  assert.deepEqual(after, want, '整份 config 只能差 maxItems 一项，实际 ' + JSON.stringify(after));
  check('POLL-EDIT-14 after the retry the saved config deep-equals the stored one except maxItems — headers / allowInsecure / envelope survive the trip through a failed read');

  const gone = await response(path + '?resourceVersion=' + encodeURIComponent((await api(path)).body.metadata.resourceVersion), null, 'DELETE');
  assert.equal(gone.status, 204, '清理必须 204，实际 ' + gone.status);
}
