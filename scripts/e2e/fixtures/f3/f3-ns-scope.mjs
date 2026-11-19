// #455 顶栏命名空间 ≡ 页面命名空间（namespace.html）。
//
// 现象（黑盒审计 B-2）：`namespace.html?name=default` 上把顶栏切到 prod，顶栏变 prod、
// 页面表单仍然写 default —— 同一页两套「当前命名空间」，是唯一能把配置写进错对象的一条。
//
// 本用例用**真实写**证明「顶栏显示 == 实际编辑目标」，而不是只比两个字符串：
//   ① 进场基线：深链 `?namespace=default&name=default` ⇒ 顶栏 / 页面身份 / 编辑目标同值；
//   ② 顶栏切 ns → 必须**整页跳转**到 `namespace.html?name=<新ns>`（URL 是唯一真源），
//      且 URL 里不能留两个互相矛盾的 ns；
//   ③ 真写：跳过去之后 fill + save，PUT 必须落在 `/v1/namespaces/prod/config`，
//      default 的 config 一个字节都不许变（写错对象就是这个用例要拦的回归）；
//   ④ 反向：页内下拉改 ns（故意不导航）时，顶栏面包屑 / 触发器必须就地跟上。
// 变异口径：删掉 ② 的跳转（`switchScope` 里的 scopeParam 分支）本用例必红。
import assert from 'node:assert/strict';
import { acceptOpenDialog } from './f3-dialogs.mjs';

/* 顶栏的两个显示面：面包屑 `#<ns>` 与切换器触发器 `.scope-ns`（"Display · name"）。
 * 用 innerText 取真实渲染文本 —— 只看 DOM 属性会把「文案没跟上」也判成通过。 */
const crumb = (page) => page.locator('#scope-crumb').innerText();
const triggerText = (page) => page.locator('#scope-switcher .scope-ns').innerText();
const editTarget = (page) => page.locator('#config-namespace').inputValue();
const query = (page, key) =>
  page.evaluate((k) => new URLSearchParams(location.search).get(k), key);
const stateText = (page, id) => page.locator('#' + id).innerText();
/* 换文档后的收敛点：Anotify 已装配 + 没有 aria-busy + 配置编辑器可用。 */
const waitWired = async (page) => {
  await page.waitForFunction(
    () => !!window.Anotify && !document.querySelector('[aria-busy="true"]'),
  );
  await page.locator('#namespace-vars:not([disabled])').waitFor();
};
const acceptReloadIfOpen = async (page) => {
  if (await page.locator('#anotify-dialog:not(.hidden)').count())
    await acceptOpenDialog(page);
};

export async function nsScopeTopbar(d) {
  const { page, goto, api, calls, check, shot, clean } = d;

  /* ① 进场基线。两个 query 同值，所以这里钉住「顶栏 = 页面 = default」；
     后面任何一步只改其中一个，都必须被下面 ②/④ 的断言拦下。 */
  await goto('namespace', 'namespace=default&name=default');
  await waitWired(page);
  assert.equal((await crumb(page)).trim(), '#default', '顶栏面包屑必须是 default');
  assert.equal(await editTarget(page), 'default', '页面内下拉（编辑目标）必须是 default');
  assert.equal(await query(page, 'name'), 'default', 'URL 的 name= 是页面身份');

  /* ② 顶栏切到 prod。修复前这里只改 localStorage + ?namespace=：URL 的 name 不动、
     页面继续改 default。修复后必须是**整页跳转**到 `?name=prod`。
     用「新文档里不存在上一份文档设置的 marker」判真跳转，不拿 waitForURL 当唯一证据
     （replaceState 会先改 URL，只看 URL 会把「没跳」也判成通过）。 */
  await page.evaluate(() => {
    window.__f3BeforeSwitchDoc = 'old-document';
  });
  await page.locator('#scope-switcher .scope-trigger').click();
  await page.locator('#scope-switcher [data-ns-id="prod"]').click();
  await page.waitForFunction(() => !window.__f3BeforeSwitchDoc, null, { timeout: 15000 });
  await waitWired(page);

  assert.equal(await query(page, 'name'), 'prod', '顶栏切 ns 必须把页面身份 name= 一起改（#455 修复点）');
  assert.equal(await query(page, 'namespace'), 'prod', 'URL 里不许留两个矛盾的 ns：?namespace= 必须同步为 prod');
  assert.equal((await crumb(page)).trim(), '#prod', '顶栏面包屑必须跟着变 prod');
  const shown = await triggerText(page);
  assert.ok(shown.includes('prod'), '切换器触发器必须显示 prod，实际 ' + shown);
  assert.ok((await page.locator('#ns-name').innerText()).includes('prod'), '页面标题必须是 prod');
  assert.equal(await editTarget(page), 'prod', '页面内下拉（编辑目标）必须是 prod');
  assert.equal(
    await page.evaluate(() => Anotify.readScope().namespace),
    'prod',
    '落盘 scope 必须与顶栏同值（其他页面跟随同一个范围）',
  );

  /* ③ 真写：编辑目标 == 顶栏显示。保存之后 PUT 必须落在 prod，
     default 的 config 必须一个字节都没动 —— 这是「写错对象」的机械反证。 */
  const prodBefore = (await api('/v1/namespaces/prod/config')).body.vars || {};
  const defaultBefore = (await api('/v1/namespaces/default/config')).body.vars || {};
  assert.equal(prodBefore.F3_TOP455, undefined, '前置：prod 还没有这个键');
  const draft = JSON.stringify({ ...prodBefore, F3_TOP455: 'written-through-topbar' }, null, 2);
  await page.locator('#namespace-vars').fill(draft);
  await page.locator('#namespace-save:not([disabled])').click();
  await page.waitForFunction(() => document.getElementById('namespace-state')?.textContent?.includes('Saved'));

  const prodAfter = (await api('/v1/namespaces/prod/config')).body.vars || {};
  const defaultAfter = (await api('/v1/namespaces/default/config')).body.vars || {};
  assert.equal(prodAfter.F3_TOP455, 'written-through-topbar', '顶栏显示 prod 时，保存必须真的写进 prod');
  assert.deepEqual(defaultAfter, defaultBefore, '顶栏显示 prod 时，default 的配置一个字节都不许变');
  const puts = (await calls()).filter((c) => c.method === 'PUT');
  assert.equal(puts.length, 1, '这次保存只该有一次 PUT，实际 ' + JSON.stringify(puts.map((c) => c.url)));
  assert.equal(
    String(puts[0].url).split('?')[0],
    '/v1/namespaces/prod/config',
    'PUT 的目标必须是 prod（写对对象）',
  );
  await shot('ns-topbar-writes-prod');

  /* ④ 反向：页内下拉切回 default。这条**故意不导航**（页内 CAS/ABA 判据依赖同一份
     文档的请求代号），所以顶栏必须就地跟上：两个显示面都不许停在 prod。 */
  await page.selectOption('#config-namespace', 'default');
  await acceptReloadIfOpen(page);
  await page.waitForFunction(() =>
    (document.getElementById('namespace-state')?.textContent || '').includes('Current configuration loaded'),
  );
  assert.equal(await query(page, 'name'), 'default', '页内切换必须同步 URL 的 name=');
  assert.equal(
    (await crumb(page)).trim(),
    '#default',
    '页内切换后顶栏面包屑必须跟上（不许「显示 prod、实际改 default」）',
  );
  const shownAfter = await triggerText(page);
  assert.ok(shownAfter.includes('default'), '页内切换后触发器必须显示 default，实际 ' + shownAfter);
  assert.equal(await editTarget(page), 'default', '编辑目标与顶栏必须是同一个 ns');
  assert.equal(
    await page.evaluate(() => Anotify.readScope().namespace),
    'default',
    '页内切换也必须落盘，否则下一个页面会带着旧范围',
  );
  assert.ok((await stateText(page, 'namespace-state')).includes('Current configuration loaded'), '切回 default 后配置已加载');
  await clean('namespace topbar/detail scope consistency');

  check(
    'NS-TOP455 topbar switch navigates namespace.html?name=<ns> in a new document, crumb/trigger/page identity/persisted scope all agree, real save PUTs /v1/namespaces/prod/config and leaves default byte-identical, in-page picker keeps topbar in sync without navigation',
  );
}
