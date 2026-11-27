import assert from 'node:assert/strict';
import { createHmac } from 'node:crypto';
import { acceptDialog } from './f3-dialogs.mjs';

const BASE = '/v1/namespaces/default/sources';
const PATH = BASE + '/';
const GH = 'preserve-gh';
const MANUAL = 'preserve-manual';
const CRON = 'preserve-cron';
const SECRET = '{{SECRET_wpzjcyom}}';

const clone = (v) => JSON.parse(JSON.stringify(v));
const post = (body) => ({ method: 'POST', body: JSON.stringify(body), headers: { 'Content-Type': 'application/json' } });
const marker = (name) => name + '-renamed';

const GH_CONFIG = {
  secret: SECRET,
  payload: { env: 'prod' },
  sample: { action: 'opened', pull_request: { title: 'f3 sample' } },
  envelope: {
    type: { header: 'x-github-event', joinFrom: 'action' },
    deliveryId: { header: 'x-github-delivery' },
    payload: { whole: true },
  },
  signature: { scheme: 'github-sha256' },
};
const GH_TARGET = { capability: 'notify', match: { types: ['pull_request.opened'] }, params: { message: '$(payload.action)' } };
const MANUAL_CONFIG = { payload: { env: 'manual' }, sample: { repo: 'acme/api' } };
const MANUAL_TARGET = { capability: 'notify', match: { all: [{ field: 'repo', op: '==', value: 'acme/api' }] }, params: { repo: '$(payload.repo)' } };
const CRON_CONFIG = { expr: '0 0 1 1 *', tz: 'UTC', payload: { env: 'cron' }, sample: { tick: 1 } }; /* 1 月 1 日：用例期间不会自己触发 */

export async function sourcePreserveEdit(d) { /* #568：只改显示名保存时，表单没有控件的键（webhook 的 envelope / signature / payload、任意 kind 的 sample、targets[].match / outcomes）必须原样留在存储里 —— 丢了 envelope/signature GitHub 原生投递立刻 401（401 不产生事件，系统里查不到现场），丢了 match 每个事件都会派发。断言只看真实服务端的存储面与真实投递。 */
  const { page, goto, api, response, check, clean } = d;
  assert.ok(!d.mock, 'source-preserve-edit 只在真实权威下有意义（要读真服务端存下来的 config / targets）');
  const visible = (sel, timeout = 15000) => page.locator(sel).waitFor({ state: 'visible', timeout });
  const doc = (name) => api(PATH + name);
  const drop = async (name) => {
    const cur = await doc(name);
    if (cur.status === 404) return;
    assert.equal(cur.status, 200, '读 ' + name + ' 只允许 200/404，实际 ' + cur.status);
    const gone = await response(PATH + name + '?resourceVersion=' + encodeURIComponent(cur.body.metadata.resourceVersion), null, 'DELETE');
    assert.equal(gone.status, 204, '上一次尝试留下的 ' + name + ' 必须先删掉，实际 ' + gone.status);
  };
  const deliver = (hookUrl, event, action, deliveryId) => { /* 走页面自己的 fetch（同源、无会话）：GitHub 的三个头 + 对**实际发出的字符串**算的签名 */
    const raw = JSON.stringify({ action, pull_request: { title: 'f3 ' + action } });
    const signature = 'sha256=' + createHmac('sha256', SECRET).update(raw).digest('hex');
    return page.evaluate(async ([url, body, sig, ev, id]) => {
      const r = await fetch(url, {
        method: 'POST',
        body,
        headers: { 'Content-Type': 'application/json', 'X-GitHub-Event': ev, 'X-GitHub-Delivery': id, 'X-Hub-Signature-256': sig },
      });
      const text = await r.text();
      return { status: r.status, body: text ? JSON.parse(text) : null };
    }, [hookUrl, raw, signature, event, deliveryId]);
  };
  let seq = 0;
  const send = (hookUrl, action) => deliver(hookUrl, 'pull_request', action, 'f3-preserve-' + Date.now() + '-' + ++seq);
  const renameOnly = async (name) => { /* 打开编辑器 → 只改显示名 → 保存；返回真实发出去的 PATCH 体 */
    await goto('sources', 'namespace=default&edit=' + name, 'zh-CN');
    await visible('#src-modal:not(.hidden)');
    await page.waitForFunction((n) => document.querySelector('#f-name')?.value === n, name, { timeout: 20000 });
    await page.locator('#f-display').fill(marker(name));
    const reqP = page.waitForRequest((r) => r.method() === 'PATCH' && r.url().endsWith(PATH + name), { timeout: 20000 });
    const respP = page.waitForResponse((r) => r.request().method() === 'PATCH' && r.url().endsWith(PATH + name), { timeout: 20000 });
    await page.locator('#modal-save').click();
    const [req, resp] = await Promise.all([reqP, respP]);
    assert.equal(resp.status(), 200, '只改显示名的保存必须 200，实际 ' + resp.status());
    await page.locator('#src-modal').waitFor({ state: 'hidden', timeout: 20000 });
    return JSON.parse(req.postData() || '{}');
  };

  await goto('sources');
  for (const name of [GH, MANUAL, CRON]) await drop(name);

  const created = await api(BASE, post({ apiVersion: 'anotify/v1', kind: 'Source', metadata: { name: GH }, spec: { kind: 'webhook', enabled: true, config: GH_CONFIG, targets: [GH_TARGET] } }));
  assert.equal(created.status, 201, '建 GitHub 原生 webhook 源必须 201，实际 ' + created.status + ' ' + JSON.stringify(created.body).slice(0, 300));
  const before = (await doc(GH)).body;
  const beforeCfg = before.spec.config;
  const hookUrl = beforeCfg.hookUrl;
  assert.ok(typeof hookUrl === 'string' && hookUrl.startsWith('/v1/hooks/'), '夹具前提：读投影必须给授权读 hookUrl，实际 ' + JSON.stringify(hookUrl));
  assert.equal(beforeCfg.secretSet, true);
  assert.deepEqual(beforeCfg.envelope, GH_CONFIG.envelope, '夹具前提：表单没有 envelope 控件，存储里必须有完整那份');
  assert.deepEqual(beforeCfg.signature, GH_CONFIG.signature, '夹具前提：表单没有 signature 控件，存储里必须有');
  assert.deepEqual(beforeCfg.payload, GH_CONFIG.payload, '夹具前提：webhook 的静态 payload 表单不渲染');
  assert.deepEqual(before.spec.targets[0].match, GH_TARGET.match, '夹具前提：表单行没有 match 控件');
  check('PRESERVE-01 a GitHub-native webhook source (envelope + signature + payload + sample + targets[].match) is stored and reads back with hookUrl / secretSet');

  const opened = await send(hookUrl, 'opened');
  assert.equal(opened.status, 200, '保存前：GitHub 原生投递（X-Hub-Signature-256）必须 200，实际 ' + opened.status + ' ' + JSON.stringify(opened.body));
  assert.equal((opened.body.tasks || []).length, 1, '保存前：opened 必须建 1 个任务，实际 ' + JSON.stringify(opened.body));
  const closedBefore = await send(hookUrl, 'closed');
  assert.equal(closedBefore.status, 200);
  assert.equal((closedBefore.body.tasks || []).length, 0, '保存前：closed 不在 match.types 里，必须不派发，实际 ' + JSON.stringify(closedBefore.body.tasks));
  assert.deepEqual((closedBefore.body.pruned || []).map((p) => p.reason), ['no_match'], '保存前：closed 必须记一条 no_match 剪枝，实际 ' + JSON.stringify(closedBefore.body.pruned));
  check('PRESERVE-02 before any UI edit the GitHub-style delivery is 200 with one task, and action=closed is pruned as no_match');

  await goto('sources', 'namespace=default&edit=' + GH, 'zh-CN');
  await visible('#src-modal:not(.hidden)');
  await page.waitForFunction((n) => document.querySelector('#f-name')?.value === n, GH, { timeout: 20000 });
  await visible('#f-kept:not(.hidden)');
  const keptConfig = (await page.locator('#f-kept').innerText()).trim();
  for (const key of ['envelope', 'signature', 'payload']) assert.ok(keptConfig.includes(key), 'config 提示必须点名被保留的键 ' + key + '，实际 ' + JSON.stringify(keptConfig));
  assert.ok(keptConfig.includes('YAML'), 'config 提示必须说明这些键在 YAML 里编辑，实际 ' + JSON.stringify(keptConfig));
  const keptTarget = (await page.locator('#f-targets .source-target .field-hint').first().innerText()).trim();
  assert.ok(keptTarget.includes('match'), '目标行必须说明这一行还有 match 条件，实际 ' + JSON.stringify(keptTarget));
  check('PRESERVE-03 the editor names the keys it does not own (config: envelope / signature / payload, target row: match) instead of silently dropping them');

  await page.locator('#f-display').fill(marker(GH));
  const reqP = page.waitForRequest((r) => r.method() === 'PATCH' && r.url().endsWith(PATH + GH), { timeout: 20000 });
  const respP = page.waitForResponse((r) => r.request().method() === 'PATCH' && r.url().endsWith(PATH + GH), { timeout: 20000 });
  await page.locator('#modal-save').click();
  const [req, resp] = await Promise.all([reqP, respP]);
  assert.equal(resp.status(), 200, '只改显示名的保存必须 200，实际 ' + resp.status());
  await page.locator('#src-modal').waitFor({ state: 'hidden', timeout: 20000 });
  const sent = JSON.parse(req.postData() || '{}');
  assert.deepEqual(sent.spec.config.envelope, beforeCfg.envelope, 'PATCH 必须带上 envelope（丢了 GitHub 三元组取不到），实际 ' + JSON.stringify(sent.spec.config));
  assert.deepEqual(sent.spec.config.signature, beforeCfg.signature, 'PATCH 必须带上 signature（丢了签名口径回落到 X-Anotify-Signature）');
  assert.deepEqual(sent.spec.config.payload, beforeCfg.payload, 'PATCH 必须带上 webhook 的静态 payload');
  assert.deepEqual(sent.spec.config.sample, beforeCfg.sample, 'PATCH 必须带上 sample');
  assert.deepEqual(sent.spec.targets[0].match, before.spec.targets[0].match, 'PATCH 必须带上 targets[].match（丢了过滤失效，每个事件都派发）');
  assert.equal(sent.spec.targets[0].capability, 'notify');
  assert.deepEqual(sent.spec.targets[0].params, GH_TARGET.params);
  for (const key of ['hookUrl', 'secretSet']) assert.equal(key in sent.spec.config, false, '读投影 ' + key + ' 不许写回去（服务端会当未知字段 400），实际 ' + JSON.stringify(sent.spec.config));
  check('PRESERVE-04 the PATCH body of a rename-only save carries envelope / signature / payload / sample / match and drops the read-only projections hookUrl / secretSet');

  const after = (await doc(GH)).body;
  assert.deepEqual(after.spec, before.spec, '编辑源只改显示名：保存前后 GET 的 spec 必须逐字段相等，实际 ' + JSON.stringify(after.spec));
  assert.equal(after.metadata.displayName, marker(GH), '显示名必须真的保存了，实际 ' + JSON.stringify(after.metadata.displayName));
  assert.equal(JSON.stringify(after).includes(SECRET), false, '读面不得回显 secret 明文');
  check('PRESERVE-05 after the rename-only save the stored spec deep-equals the pre-edit one and only metadata.displayName changed');

  const openedAfter = await send(after.spec.config.hookUrl, 'opened');
  assert.equal(openedAfter.status, 200, '保存后：同样的 GitHub 原生投递必须仍是 200（401 = 签名口径已回落），实际 ' + openedAfter.status + ' ' + JSON.stringify(openedAfter.body));
  assert.equal((openedAfter.body.tasks || []).length, 1, '保存后：opened 仍必须建 1 个任务，实际 ' + JSON.stringify(openedAfter.body));
  const closedAfter = await send(after.spec.config.hookUrl, 'closed');
  assert.equal(closedAfter.status, 200);
  assert.equal((closedAfter.body.tasks || []).length, 0, '保存后：closed 仍不许派发（match 丢了就会多派），实际 ' + JSON.stringify(closedAfter.body.tasks));
  assert.deepEqual((closedAfter.body.pruned || []).map((p) => p.reason), ['no_match'], '保存后：closed 仍必须记 no_match 剪枝，实际 ' + JSON.stringify(closedAfter.body.pruned));
  check('PRESERVE-06 after the save a GitHub-native delivery is still 200 with one task and action=closed is still pruned as no_match');

  const manualCreated = await api(BASE, post({ apiVersion: 'anotify/v1', kind: 'Source', metadata: { name: MANUAL }, spec: { kind: 'manual', enabled: true, config: MANUAL_CONFIG, targets: [MANUAL_TARGET] } }));
  assert.equal(manualCreated.status, 201, '建 manual 源必须 201，实际 ' + manualCreated.status + ' ' + JSON.stringify(manualCreated.body).slice(0, 300));
  const manualBefore = (await doc(MANUAL)).body;
  assert.deepEqual(manualBefore.spec.config.sample, MANUAL_CONFIG.sample, '夹具前提：manual 的 sample 表单没有控件');
  assert.deepEqual(manualBefore.spec.targets[0].match, MANUAL_TARGET.match, '夹具前提：manual 目标的 match.all 表单没有控件');
  const manualSent = await renameOnly(MANUAL);
  assert.deepEqual(manualSent.spec.config.sample, MANUAL_CONFIG.sample, 'PATCH 必须带上 manual 的 sample，实际 ' + JSON.stringify(manualSent.spec.config));
  assert.deepEqual(manualSent.spec.targets[0].match, MANUAL_TARGET.match, 'PATCH 必须带上 match.all（丢了 repo 过滤就失效）');
  assert.deepEqual((await doc(MANUAL)).body.spec, manualBefore.spec, 'manual 只改显示名：spec 必须逐字段相等');
  check('PRESERVE-07 a manual source keeps config.sample and targets[].match.all through a rename-only save');

  const cronCreated = await api(BASE, post({ apiVersion: 'anotify/v1', kind: 'Source', metadata: { name: CRON }, spec: { kind: 'cron', enabled: false, config: CRON_CONFIG, targets: [] } }));
  assert.equal(cronCreated.status, 201, '建 cron 源必须 201，实际 ' + cronCreated.status + ' ' + JSON.stringify(cronCreated.body).slice(0, 300));
  const cronBefore = (await doc(CRON)).body;
  assert.deepEqual(cronBefore.spec.config.sample, CRON_CONFIG.sample, '夹具前提：cron 的 sample 表单没有控件');
  const cronSent = await renameOnly(CRON);
  assert.deepEqual(cronSent.spec.config.sample, CRON_CONFIG.sample, 'PATCH 必须带上 cron 的 sample，实际 ' + JSON.stringify(cronSent.spec.config));
  assert.equal(cronSent.spec.config.expr, CRON_CONFIG.expr, '排期只许是存储的那份');
  assert.equal(cronSent.spec.config.tz, CRON_CONFIG.tz);
  assert.deepEqual((await doc(CRON)).body.spec, cronBefore.spec, 'cron 只改显示名：spec 必须逐字段相等');
  check('PRESERVE-08 a cron source keeps config.sample (and its schedule) through a rename-only save');

  const cronPayload = { env: 'cron-edited' };
  await goto('sources', 'namespace=default&edit=' + CRON, 'zh-CN');
  await visible('#src-modal:not(.hidden)');
  await page.waitForFunction((n) => document.querySelector('#f-name')?.value === n, CRON, { timeout: 20000 });
  const cronKept = (await page.locator('#f-kept').innerText()).trim(); /* 表单 own 的键不许出现在「保留」提示里：r1 的回归正是提示说 payload 归表单、写入路径却把它摘掉 */
  assert.ok(cronKept.includes('sample'), 'cron 的保留提示必须点名 sample，实际 ' + JSON.stringify(cronKept));
  assert.equal(cronKept.includes('payload'), false, 'cron 的 payload 有表单控件（表单 own），不许在「保留」提示里把它说成由 YAML 管理，实际 ' + JSON.stringify(cronKept));
  await page.locator('#f-payload').fill(JSON.stringify(cronPayload, null, 2)); /* 表单 own 的键必须真的写得进去：只改显示名的用例对这一步是瞎的 */
  const cronReqP = page.waitForRequest((r) => r.method() === 'PATCH' && r.url().endsWith(PATH + CRON), { timeout: 20000 });
  const cronRespP = page.waitForResponse((r) => r.request().method() === 'PATCH' && r.url().endsWith(PATH + CRON), { timeout: 20000 });
  await page.locator('#modal-save').click();
  const [cronReq, cronResp] = await Promise.all([cronReqP, cronRespP]);
  assert.equal(cronResp.status(), 200, '在表单里改 cron payload 再保存必须 200，实际 ' + cronResp.status());
  await page.locator('#src-modal').waitFor({ state: 'hidden', timeout: 20000 });
  const cronEditSent = JSON.parse(cronReq.postData() || '{}');
  assert.deepEqual(cronEditSent.spec.config.payload, cronPayload, '表单里改的 cron payload 必须出现在 PATCH 体里（r1 把这个键从写入路径摘掉，保存静默不生效），实际 ' + JSON.stringify(cronEditSent.spec.config));
  assert.deepEqual(cronEditSent.spec.config.sample, CRON_CONFIG.sample, '同一次保存仍不许抹掉 cron 的 sample，实际 ' + JSON.stringify(cronEditSent.spec.config));
  const cronAfter = (await doc(CRON)).body.spec.config;
  assert.deepEqual(cronAfter.payload, cronPayload, '存储里的 cron payload 必须真的变成表单里那份，实际 ' + JSON.stringify(cronAfter.payload));
  assert.deepEqual(cronAfter.sample, CRON_CONFIG.sample, '存储里的 cron sample 必须还在，实际 ' + JSON.stringify(cronAfter.sample));
  assert.equal(cronAfter.expr, CRON_CONFIG.expr, '这次保存不许改掉排期，实际 ' + cronAfter.expr);
  check('PRESERVE-09 a cron save really writes the form-owned config.payload, keeps the non-owned sample, and the kept hint never names a form-owned key');

  await goto('sources', 'namespace=default', 'zh-CN');
  await page.locator('#btn-new').click();
  await visible('#src-modal:not(.hidden)');
  assert.equal(await page.locator('#f-kept').isHidden(), true, '新建源没有存储值，保留提示必须是隐藏的');
  await page.locator('#f-kind').selectOption('webhook');
  await page.locator('#f-token').fill('preserve-new-token');
  await page.locator('#f-secret').fill('preserve-new-hmac');
  await page.locator('#f-sample').fill(JSON.stringify({ a: 1 }));
  await page.locator('#f-kind').selectOption('manual');
  assert.equal(await page.locator('#f-token').inputValue(), '', '切到 manual 之后 webhook 的 token 必须当场清掉');
  assert.equal(await page.locator('#f-secret').inputValue(), '', '切到 manual 之后 webhook 的 secret 必须当场清掉');
  assert.equal(await page.locator('#f-sample').inputValue(), '', '切到 manual 之后 webhook 的样例必须当场清掉');
  check('PRESERVE-10 a new source still clears the other kind fields when the kind changes (#540 口径不回归)');
  await page.locator('#f-name').fill('preserve-kind-switch');
  await acceptDialog(page, () => page.locator('#modal-cancel').click());
  await page.locator('#src-modal').waitFor({ state: 'hidden', timeout: 20000 });
  await clean('source editor zh-CN');

  for (const name of [GH, MANUAL, CRON]) await drop(name);
  assert.equal((await doc(GH)).status, 404, '收尾：夹具源必须删干净');
}
