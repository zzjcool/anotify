import assert from 'node:assert/strict';

const SOURCE = 'poll-feed';
const FEED_URL = 'https://example.com/poll/feed';
const QUIET_EXPR = '0 0 1 1 *';

const POLL_YAML = [
  'apiVersion: anotify/v1',
  'kind: Source',
  'metadata:',
  '  name: ' + SOURCE,
  'spec:',
  '  kind: poll',
  '  enabled: true',
  '  config:',
  '    url: ' + FEED_URL,
  '    schedule:',
  '      expr: "' + QUIET_EXPR + '"',
  '      tz: UTC',
  '    timeoutMs: 5000',
  '    maxItems: 20',
  '    envelope:',
  '      items: "."',
  '      type:',
  '        pointer: type',
  '      deliveryId:',
  '        pointer: id',
  '      payload:',
  '        whole: true',
  '  targets: []',
  '',
].join('\n');

const pairs = async (page, sec) =>
  page.$$eval(sec + ' .sd-pair', (cells) =>
    cells.map((cell) => ({
      k: (cell.querySelector('.sd-k') || {}).textContent || '',
      v: (cell.querySelector('.sd-v') || {}).textContent || '',
      title: (cell.querySelector('.sd-v span') || { title: '' }).title || '',
    })),
  );

const valueOf = (list, label) => {
  const hit = list.find((cell) => cell.k.trim() === label);
  return hit ? hit.v.trim() : '<missing ' + label + '>';
};

export async function sourcePoll(d) {
  const { page, goto, api, calls, check, shot, clean } = d;
  const visible = (sel, timeout = 15000) => page.locator(sel).waitFor({ state: 'visible', timeout });
  const sourcesGet = () => calls().then((rows) => rows.filter((c) => c.method === 'GET' && /\/sources(\/|\?|$)/.test(c.url)));

  await goto('sources');
  await visible('#btn-new-yaml');
  await page.locator('#btn-new-yaml').click();
  await visible('#src-yaml-modal:not(.hidden)');
  await page.locator('#src-yaml-sample > summary').click();
  await visible('#src-yaml-sample-body');
  const sample = (await page.locator('#src-yaml-sample-body').innerText()).trim();
  assert.match(sample, /kind: poll/, 'YAML 弹层里的示例必须是一份 poll 源文档，实际 ' + sample);
  assert.match(sample, /targets: \[\]/, '示例必须把「目标可以为空」写出来（直接可提交），实际 ' + sample);
  assert.match(sample, /schedule:/, '示例必须带 schedule 段');
  await page.locator('#src-yaml-sample-fill').click();
  const filled = await page.locator('#src-yaml-input').inputValue();
  assert.match(filled, /kind: poll/, '「填入示例」必须把示例整份抄进粘贴框，实际 ' + filled);
  assert.equal(filled.trim(), sample, '抄进去的就是示例正文本身（不另抄一份，也不裁行）');
  check('POLL-01 the YAML modal ships a copyable poll example and the fill button moves it into the textarea verbatim');

  await page.locator('#src-yaml-input').fill(POLL_YAML);
  await page.locator('#src-yaml-submit').click();
  await page.waitForFunction(
    ([path, code]) => (window.__f3Calls || []).some((c) => c.url === path && c.method === 'POST' && c.httpStatus === code && c.settled === true),
    ['/v1/namespaces/default/sources/yaml', 201],
  );
  const posts = (await calls()).filter((c) => c.method === 'POST' && c.url === '/v1/namespaces/default/sources/yaml');
  assert.equal(posts.length, 1, '从 YAML 建 poll 源必须只发一次 POST …/sources/yaml，实际 ' + posts.length);
  assert.deepEqual(posts[0].keys, ['yaml'], '写请求体只能有 yaml 一个键，实际 ' + JSON.stringify(posts[0].keys));
  await page.locator('#src-yaml-modal').waitFor({ state: 'hidden', timeout: 15000 });
  await visible('#src-list > div[data-name="' + SOURCE + '"]');
  const stored = await api('/v1/namespaces/default/sources/' + SOURCE);
  assert.equal(stored.status, 200, '刚建好的 poll 源必须可读，实际 ' + stored.status);
  assert.equal(stored.body?.spec?.kind, 'poll');
  assert.equal(stored.body?.spec?.config?.url, FEED_URL, 'URL 必须原样落库');
  check('POLL-02 a poll source created from pasted YAML: POST body only {yaml}, 201, list row appears, store reads back');

  const rowBadge = () => page.locator('#src-list > div[data-name="' + SOURCE + '"] .src-badges .badge').first();
  assert.equal((await rowBadge().innerText()).trim(), '拉取', '列表行的种类徽章必须是本地化文案，不是原文 poll');
  await goto('sources', 'namespace=default', 'en');
  await visible('#src-list > div[data-name="' + SOURCE + '"]');
  assert.equal((await rowBadge().innerText()).trim(), 'Poll', '同一个徽章在英文页面必须是英文词条（不是写死的中文）');
  await goto('sources', 'namespace=default', 'zh-CN');
  await visible('#src-list > div[data-name="' + SOURCE + '"]');
  check('POLL-03 the kind badge is a localized label: 拉取 on zh-CN, Poll on en (never the raw kind)');

  await goto('source-detail', 'namespace=default&name=' + SOURCE, 'zh-CN');
  await visible('#src-detail[data-source="' + SOURCE + '"]');
  await visible('#src-detail-body [data-sec="poll"]');
  const sec = '#src-detail-body [data-sec="poll"]';
  assert.equal((await page.locator(sec + ' .sd-sec-title').first().innerText()).trim(), '拉取', '区块标题是「拉取」');
  const headBadge = page.locator('#src-detail-body [data-sec="basic"] .sd-badges .badge').first();
  assert.equal((await headBadge.innerText()).trim(), '拉取', '详情页头部的种类徽章同样本地化');
  const config = await pairs(page, sec);
  assert.equal(valueOf(config, 'URL').includes(FEED_URL), true, '配置摘要必须给出真实 url，实际 ' + valueOf(config, 'URL'));
  assert.equal(valueOf(config, '排期'), QUIET_EXPR + ' · UTC', '排期必须是表达式 + 时区');
  assert.equal(valueOf(config, '单次超时'), '5000 ms', 'timeoutMs 原样显示');
  assert.equal(valueOf(config, '单轮条目上限'), '20', 'maxItems 原样显示');
  check('POLL-04 the detail page grows a 拉取 block for kind=poll with the config summary (url / schedule / timeout / maxItems)');

  assert.equal(valueOf(config, '条目数组'), '.', 'envelope.items 原样显示');
  assert.equal(valueOf(config, '事件类型'), 'type', 'envelope.type.pointer 原样显示');
  assert.equal(valueOf(config, '去重键'), 'id', 'envelope.deliveryId.pointer 原样显示');
  assert.equal(valueOf(config, '载荷'), '整个条目', 'payload 的 whole 维度说成人话');
  check('POLL-05 the envelope shows all four pointers (items / type / deliveryId / payload)');

  const status = await pairs(page, sec);
  assert.equal(valueOf(status, '最近轮询'), '尚未轮询', '从未轮询过必须说「尚未轮询」，不是 —');
  assert.equal(valueOf(status, '连续失败'), '0');
  assert.equal(valueOf(status, '跳过重叠'), '0');
  assert.equal(valueOf(status, '游标').startsWith('还没有游标'), true, '还没有游标时不能显示成 —，实际 ' + valueOf(status, '游标'));
  const nextFire = valueOf(status, '下次触发时间');
  assert.equal(nextFire === '—' || nextFire === '', false, '排期算出来的下次轮询不能是空值，实际 ' + nextFire);
  assert.equal(Number.isNaN(Date.parse(nextFire)), false, '下次轮询必须是可读时间，实际 ' + nextFire);
  const errFold = page.locator(sec + ' #src-detail-poll-error');
  assert.equal(await errFold.count(), 1, '最近错误必须是可折叠块');
  await errFold.locator('summary').click();
  const errText = await errFold.locator('div').evaluate((n) => n.textContent);
  assert.equal(String(errText).includes('没有失败记录'), true, '没有失败事实时如实说明，不编造原因，实际 ' + errText);
  const sectionText = await page.locator(sec).innerText();
  for (const label of ['最近轮询', '下次触发时间', '连续失败', '跳过重叠', '游标', '最近错误']) {
    assert.ok(sectionText.includes(label), '拉取区块缺状态字段：' + label + '（实际 ' + sectionText + '）');
  }
  await shot('source-detail-poll');
  check('POLL-06 all five poll status facts render; a never-polled source says 尚未轮询 instead of —');

  await page.setViewportSize({ width: 360, height: 800 });
  await goto('source-detail', 'namespace=default&name=' + SOURCE, 'zh-CN');
  await visible(sec);
  await clean('poll detail 360');
  const wide = await page.evaluate(() => {
    const narrow = [360, 320];
    const out = [];
    for (const e of document.querySelectorAll('#src-detail-body [data-sec="poll"] *')) {
      const r = e.getBoundingClientRect();
      if (r.width && r.right > narrow[0] + 2) out.push({ tag: e.tagName, cls: e.className, right: Math.round(r.right) });
    }
    return out;
  });
  assert.deepEqual(wide, [], '360px 下「拉取」区块的每个元素都必须落在视口内，实际 ' + JSON.stringify(wide));
  await shot('source-detail-poll-360');
  check('POLL-07 the 拉取 block never scrolls sideways at 360px (long url and error text wrap)');

  await goto('sources');
  await visible('#src-list > div[data-name="' + SOURCE + '"]');
  await page.addInitScript((name) => {
    const inner = window.fetch;
    window.fetch = async (input, init) => {
      const res = await inner(input, init);
      const url = typeof input === 'string' ? input : (input && input.url) || '';
      if (!/\/v1\/namespaces\/[^/]+\/sources(\/|\?|$)/.test(url)) return res;
      const body = await res.clone().json().catch(() => null);
      if (!body) return res;
      const rows = Array.isArray(body.items) ? body.items : [body];
      let touched = false;
      for (const row of rows) {
        if (row && row.spec && row.metadata && row.metadata.name === name) {
          row.spec.kind = 'smtp-relay';
          touched = true;
        }
      }
      if (!touched) return res;
      return new Response(JSON.stringify(body), { status: res.status, headers: { 'Content-Type': 'application/json' } });
    };
  }, SOURCE);
  await goto('sources');
  await visible('#src-list > div[data-name="' + SOURCE + '"]');
  assert.equal((await sourcesGet()).length >= 1, true, '未知 kind 的读必须真的发生过（否则徽章断言是空转）');
  const rawBadge = await rowBadge().innerText();
  assert.equal(rawBadge.trim(), 'smtp-relay', '表里没有的 kind 必须回落显示原值，实际 ' + JSON.stringify(rawBadge));
  const bodyText = await page.locator('#src-list').innerText();
  assert.equal(/undefined|null/.test(bodyText), false, '未知 kind 不许在页面上露出 undefined/null，实际 ' + bodyText.slice(0, 200));
  check('POLL-08 an unknown kind falls back to its raw value on the badge (never blank, never undefined)');
}
