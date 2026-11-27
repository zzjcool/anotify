// #584 回归：首页「接入你的 Agent」（#quickstart，只在 0 事件时显示）的两个复制按钮，必须把按钮自己的文本真的交给 navigator.clipboard.writeText 并回报「已复制 ✓」。
import assert from 'node:assert/strict';

const LANGS = ['zh-CN', 'en', 'ja', 'es'];
const BTN = '#quickstart .copy-btn';

export async function quickstartCopy(d) {
  const { page, goto, response, check, shot } = d;
  // 0 事件的事实是真造的：新建一个没有任何事件的命名空间，它的 /v1/stats.total 必然是 0（不拦响应、不改统计；POST /v1/namespaces 真 server 与预览 mock 是同一个 handler）。
  await goto('index', 'namespace=default');
  const ns = 'f3-quickstart-empty';
  const created = await response('/v1/namespaces', { apiVersion: 'anotify/v1', kind: 'Namespace', metadata: { name: ns }, spec: {} }, 'POST');
  assert.equal(created.status, 201, '空命名空间必须由真入口建出来，实际 ' + created.status + ' ' + JSON.stringify(created.body));
  // 无头浏览器没有系统剪贴板：把 writeText 收到的字节记下来（就是用户粘贴出来的内容）。
  await page.addInitScript(() => {
    window.__f3Copied = [];
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText: (text) => { window.__f3Copied.push(String(text)); return Promise.resolve(); } },
    });
  });
  const copy = async (index) => {
    const btn = page.locator(BTN).nth(index);
    const before = (await btn.textContent()).trim();
    await btn.click();
    // 有界等待后给一条说人话的失败：写死 4.5s 默认超时的话，去掉修复的现场只剩一句 Timeout。
    const wrote = await page.waitForFunction((n) => window.__f3Copied.length === n, index + 1, { timeout: 8000 }).then(() => true, () => false);
    assert.ok(wrote, '点完「' + before + '」后 navigator.clipboard.writeText 一次都没被调用（#584：文本被当 CSS 选择器就是这个症状）');
    return { before, text: await page.evaluate(() => window.__f3Copied.at(-1)) };
  };
  const label = (index) => page.evaluate((i) => (document.querySelectorAll('#quickstart .copy-btn')[i]?.textContent || '').trim(), index);
  for (const lang of LANGS) {
    await goto('index', 'namespace=' + ns, lang);
    await page.waitForFunction(() => { const q = document.getElementById('quickstart'); return !!q && q.hidden === false; }, null, { timeout: 15000 });
    assert.equal((await page.locator('#kpi-total').innerText()).trim(), '0', lang + '：空命名空间的 KPI 必须是 0（否则 #quickstart 的显示条件没被验到）');
    const want = await page.evaluate(() => ({
      texts: [Anotify.t('index.quickstart.cmd_install'), Anotify.t('index.quickstart.cmd_a')],
      attrs: Array.from(document.querySelectorAll('#quickstart .copy-btn'), (b) => b.dataset.copyText),
      copied: Anotify.t('common.copy.copied'),
    }));
    assert.equal(want.attrs.length, 2, lang + '：#quickstart 必须正好两个复制按钮（实际 ' + want.attrs.length + '）');
    assert.deepEqual(want.attrs, want.texts, lang + '：data-copy-text 必须与 locale 的 index.quickstart.cmd_install / cmd_a 逐字节一致');
    for (const index of [0, 1]) {
      const got = await copy(index);
      assert.equal(got.text, want.texts[index], lang + '：第 ' + (index + 1) + ' 个按钮写进剪贴板的必须是它自己那段文本');
      const after = await label(index);
      assert.equal(after, want.copied, lang + '：复制成功后按钮必须回报「已复制 ✓」（实际「' + after + '」）');
      if (lang === LANGS[0] && index === 0) {
        await page.waitForFunction(([i, text]) => (document.querySelectorAll('#quickstart .copy-btn')[i]?.textContent || '').trim() === text, [index, got.before], { timeout: 6000 });
        check('F3-QS-COPY the copied feedback reverts to the button own label');
      }
    }
  }
  await shot('quickstart-copy');
  check('F3-QS-COPY 0-event #quickstart copies index.quickstart.cmd_install / cmd_a verbatim in all 4 languages with copied feedback and no pageerror');
}
