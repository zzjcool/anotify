// Attributed F2b1 CRUD/fire/dryrun/deep-link chains. Both modes exact namespaced requests.
import assert from 'node:assert/strict';
import * as H from "../../lib/harness.mjs";
import { acceptDialog } from './f3-dialogs.mjs';
/* 「原始 YAML」标签只允许出现一次：折叠块 summary 已经印了它，里面若再套一层带标题的
   codeBlock（历史 bug），展开后就出现两行同样的标题。断言范围=**折叠块内部**（summary 那次算），
   不是整页 —— 整页还有面包屑与别的块。 */
async function labelCountIn(page, selector, keys) {
	return page.locator(selector).evaluate((node, phrase) => {
		let text = "";
		const walk = (n) => {
			if (n.nodeType === 3) { text += n.textContent || ""; return; }
			if (n.nodeType !== 1) return;
			for (const c of n.childNodes) walk(c);
		};
		walk(node);
		return text.split(phrase).length - 1;
	}, await page.evaluate((ks) => ks.map((k) => window.AnotifyI18n?.[k]).find(Boolean) || "原始 YAML", keys));
}
const location_pathname_is_detail = (page) => new URL(page.url()).pathname.endsWith('/source-detail.html');

export async function source(d){
 const {page,base,req,check,shot}=d;const state=d.snapshot;
 // #485：每次等待都打 WAIT 标记。卡住时硬杀留的 timeout fragment 里，
 // 最后一条没有 `WAIT OK` 的行就是「正在等什么」——不再只有「最后一条 PASS」。
 const wait=async(label,fn)=>{console.log('WAIT '+d.id+' :: '+label);const value=await fn();console.log('WAIT OK '+d.id+' :: '+label);return value;};
 const visible=sel=>page.locator(sel).waitFor({state:'visible'});const hidden=sel=>page.locator(sel).waitFor({state:'hidden'});
 const row=name=>page.locator('#src-list > div[data-name="'+name+'"]');
 // #376：行内动作收敛成「1 主 + 1 次 + ⋯」——停用/启用/删除在 ⋯ 菜单里，
 // 必须先把菜单打开；菜单没开时这些条目不在无障碍树里（不可见 = 不可点）。
 // 幂等：菜单可能已经开着（上一段断言后没关），再点触发按钮会把它关掉。
 const openMenu=async name=>{const menu=row(name).locator('.menu');if(!(await menu.isVisible()))await row(name).locator('.act-more').click();await menu.waitFor({state:'visible'});};
 const pickMenu=async(name,label)=>{await openMenu(name);await row(name).locator('.menu-item',{hasText:label}).click();};
  await page.goto(base + '/en/sources.html?namespace=default', { waitUntil: 'domcontentloaded' });
  await visible('#src-list > div[data-name="deploy-hook"]');
  if(d.mock) await page.evaluate(() => MockApi.latency(0));
  check('SourceList.items first-load scope ready, no legacy list fallback');
  await page.locator('#btn-new').click();
  await visible('#src-modal:not(.hidden)');
  await page.locator('#f-name').fill('ui-webhook');
  await page.locator('#f-kind').selectOption('webhook');
  await page.locator('#f-token').fill('ui-hook-token');
  await page.locator('#f-secret').fill('ui-private-hmac');
  await page.locator('#f-sample').fill('{"repo":"example/repo","environment":"staging"}');
  await page.locator('#f-payload-schema').fill('{"type":"object","required":["repo"],"properties":{"repo":{"type":"string"}}}');
  await page.locator('#add-target').click();
  await page.locator('#f-targets input').fill('fixer');
  const params = { repo: '$(payload.repo)', environment: '$(payload.environment)', label: 'constant', report: 'reports/$(payload.repo)' };
  await page.locator('#f-targets textarea').fill(JSON.stringify(params));
  await page.locator('#dry-payload').fill('{"repo":"actual/repo","environment":"prod"}');
  const before = await state();
  await page.locator('#btn-dry').click();
  await page.waitForFunction(() => document.querySelector('#form-result').textContent.includes('ready'));
  assert.equal(await state(), before);
  assert.match(await page.locator('#form-result').innerText(), /actual\/repo/);
  check('Unsaved Source write envelope + payload, ready resolved target params, zero writes');
  await shot('webhook-draft-params-preview');
  await page.locator('#modal-save').click();
  await hidden('#src-modal');
  await visible('#src-list > div[data-name="ui-webhook"]');
  const saved = await req('/v1/namespaces/default/sources/ui-webhook');
  assert.equal(saved.spec.config.secretSet, true);
  assert.equal(saved.spec.config.hookUrl, '/v1/hooks/ui-hook-token');
  assert.ok(!('token' in saved.spec.config) && !('secret' in saved.spec.config));
  assert.deepEqual(saved.spec.targets[0].params, params);
  check('Create webhook with targets/settings -> GET masks token/secret and roundtrips params');
  await row('ui-webhook').getByRole('button', { name: 'Edit source', exact: true }).click();
  await visible('#src-modal:not(.hidden)');
  assert.equal(await page.locator('#f-token').inputValue(), '');
  assert.equal(await page.locator('#f-secret').inputValue(), '');
  assert.equal(await page.locator('#f-name').isDisabled(), true);
  assert.equal(await page.locator('#f-kind').isDisabled(), true);
  await page.locator('#f-display').fill('CAS draft retained');
  const fresh = await req('/v1/namespaces/default/sources/ui-webhook', { method: 'PATCH', body: JSON.stringify({ apiVersion: 'anotify/v1', kind: 'Source', metadata: { resourceVersion: saved.metadata.resourceVersion }, spec: { enabled: true } }) });
  await page.locator('#modal-save').click();
  await page.waitForFunction(() => document.querySelector('#form-result').textContent.includes('resource_version_conflict'));
  assert.equal(await page.locator('#f-display').inputValue(), 'CAS draft retained');
  assert.equal(await page.locator('#src-modal').isVisible(), true);
  check('Edit 409 preserves draft and empty write-only credentials; immutable name/kind');
  await shot('edit-cas-conflict-draft');
  await acceptDialog(page, () => page.locator('#reload-draft').click());
  await page.waitForFunction(() => document.querySelector('#f-display').value === '');
  await page.locator('#f-display').fill('after explicit reload');
  await page.locator('#modal-save').click();
  await hidden('#src-modal');
  assert.equal((await req('/v1/namespaces/default/sources/ui-webhook')).spec.config.secretSet, true);
  check('Explicit reload -> next edit succeeds, omitted credentials preserve secret/hookUrl');
  await wait('row menu: click Disable',()=>pickMenu('ui-webhook', 'Disable'));
  await wait('row badge turns Disabled after disable roundtrip',()=>visible('#src-list > div[data-name="ui-webhook"] .badge:text-is("Disabled")')); // #502：读行内 badge（始终可见）不读菜单条目——菜单会被 toggle 的 PATCH→load()→render() 关掉（TOCTOU）
  const afterDisable=await wait('GET source after disable (enabled=false)',()=>req('/v1/namespaces/default/sources/ui-webhook'));
  assert.equal(afterDisable.spec.enabled, false);
  await wait('row menu: click Enable',()=>pickMenu('ui-webhook', 'Enable'));
  await wait('row badge turns Enabled after enable roundtrip',()=>visible('#src-list > div[data-name="ui-webhook"] .badge:text-is("Enabled")'));
  check('CAS toggle disable/enable actual state changes (via the row action menu)');
  // Make list DELETE stale through a concurrent real PATCH, then require two
  // separate user confirmations: failed delete and explicit retry after reload.
  const latest = await req('/v1/namespaces/default/sources/ui-webhook');
  await req('/v1/namespaces/default/sources/ui-webhook', { method: 'PATCH', body: JSON.stringify({ apiVersion: 'anotify/v1', kind: 'Source', metadata: { resourceVersion: latest.metadata.resourceVersion }, spec: { enabled: true } }) });
  let confirmations = 0;
  await acceptDialog(page, () => pickMenu('ui-webhook', 'Delete source'));
  confirmations++;
  await page.waitForFunction(() => document.querySelector('#action-error').textContent.includes('resource_version_conflict'));
  assert.equal((await req('/v1/namespaces/default/sources/ui-webhook')).metadata.name, 'ui-webhook');
  /* 「重新载入最新」不按英文标签找（文案已本地化过一轮）：走语言中立的动作钩子。
     sources.html 的冲突态只挂一个按钮（reload），所以这里断言恰好一个再点。 */
  const reloadLatest = page.locator('#action-error button');
  assert.equal(await reloadLatest.count(), 1, '冲突态必须给一个「重新载入最新」动作（实际：' + (await page.locator('#action-error').innerText()) + '）');
  await reloadLatest.click();
  await page.waitForFunction(() => document.querySelector('#action-error').textContent.includes('another confirmation'));
  await acceptDialog(page, () => pickMenu('ui-webhook', 'Delete source'));
  confirmations++;
  await hidden('#src-list > div[data-name="ui-webhook"]');
  assert.equal(confirmations, 2);
  check('DELETE query CAS stale409 retains resource; explicit reload and second confirmation deletes');
  await page.locator('#btn-new').click();
  await page.locator('#f-name').fill('ui-manual');
  await page.locator('#f-kind').selectOption('manual');
  await page.locator('#f-payload').fill('{"repo":"configured/repo","environment":"staging"}');
  await page.locator('#add-target').click();
  await page.locator('#f-targets input').fill('fixer');
  await page.locator('#f-targets textarea').fill('{"repo":"$(payload.repo)","environment":"$(payload.environment)"}');
  await page.locator('#modal-save').click();
  await hidden('#src-modal');
  await visible('#src-list > div[data-name="ui-manual"]');
  await row('ui-manual').getByRole('button', { name: 'Fire now', exact: true }).click();
  const fireBefore = await state();
  await page.locator('#fire-dry').click();
  await page.waitForFunction(() => document.querySelector('#fire-result').textContent.includes('configured/repo'));
  assert.equal(await state(), fireBefore);
  await page.locator('#fire-payload').fill('{"repo":"overridden/repo"}');
  await page.locator('#fire-submit').click();
  await page.waitForFunction(() => document.querySelector('#fire-result').textContent.includes('taskId'));
  const calls = await page.evaluate(() => window.__sourceCalls);
  const fire = calls.findLast(c => c.url.endsWith('/ui-manual/fire')).response;
  assert.deepEqual(Object.keys(fire).sort(), ['duplicate', 'eventId', 'pruned', 'tasks', 'traceId']);
  assert.equal(fire.tasks.length, 1);
  const task = await req('/v1/namespaces/default/tasks/' + fire.tasks[0].taskId);
  assert.equal(task.spec.params.repo, 'overridden/repo');
  assert.equal(task.spec.params.environment, 'staging');
  assert.equal(task.status.phase, 'pending');
  check('Saved preview payload omission no writes; manual fire shallow override exact W6 receipt, pending not success');
  await shot('manual-fire-receipt');
  await page.locator('#fire-close').click();
  await page.locator('#btn-new').click();
  await page.locator('#f-name').fill('ui-pruned');
  await page.locator('#f-kind').selectOption('manual');
  await page.locator('#add-target').click();
  await page.locator('#f-targets input').fill('fixer');
  await page.locator('#f-targets textarea').fill('{"repo":"$(payload.missing)"}');
  await page.locator('#add-target').click();
  await page.locator('#f-targets input').nth(1).fill('not-existing');
  const pruneBefore = await state();
  await page.locator('#btn-dry').click();
  await page.waitForFunction(() => document.querySelector('#form-result').textContent.includes('param_missing') && document.querySelector('#form-result').textContent.includes('missing'));
  assert.equal(await state(), pruneBefore);
  const previewCall = (await page.evaluate(() => __sourceCalls)).findLast(c => c.url.endsWith('/sources/dry-run'));
  assert.deepEqual(previewCall.response.targets.map(t => t.state), ['pruned', 'missing']);
  assert.ok(!('eventId' in previewCall.response));
  check('Missing/pruned target draft preview gives reasons and no event/task persistence');
  await shot('missing-pruned-dryrun');
  await acceptDialog(page, () => page.locator('#modal-close').click());
  await hidden('#src-modal');
  // Actual detail link uses metadata.name + namespace. Mock memory resets on
  // navigation, so use the real seeded Source, not an empty detail fixture.
  const requests = (await page.evaluate(() => __sourceCalls)).map(({ response, ...rest }) => rest);
  const createdCall = requests.find(c => c.url.endsWith('/sources') && c.method === 'POST');
  assert.deepEqual(createdCall.keys.sort(), ['apiVersion', 'kind', 'metadata', 'spec']);
  const editCalls = requests.filter(c => c.url.endsWith('/ui-webhook') && c.method === 'PATCH');
  assert.ok(editCalls.every(c => !c.configKeys.includes('secret') && !c.configKeys.includes('token')));
  const deepLink = await row('deploy-hook').locator('a').first().getAttribute('href');
  assert.match(deepLink, /name=deploy-hook/); assert.match(deepLink, /namespace=default/);
  // 名称链接 = **只读详情页**；行内动作（编辑/立即运行/⋯）仍在，编辑能力一点不少。
  assert.match(deepLink, /source-detail\.html/, '名称链接指向 source-detail.html');
  await row('deploy-hook').locator('a').first().click();

  /* ---------- 一、源只读详情页（source-detail.html?namespace=&name=）---------- */
  await page.waitForFunction(() => location.pathname.endsWith('/source-detail.html'));
  await visible('#src-detail[data-source="deploy-hook"]');
  assert.equal(await page.locator('#src-detail').getAttribute('data-source'), 'deploy-hook');
  assert.match(await page.locator('#src-detail-name').innerText(), /deploy-hook/);
  // 四块内容（锚点是冻结契约）：基本信息 / 定义与字段 / 目标 / 近期回执。
  for (const sec of ['basic', 'config', 'targets', 'recent']) {
    assert.equal(await page.locator('#src-detail [data-sec="' + sec + '"]').count(), 1, '详情必须有 [data-sec="' + sec + '"]');
  }
  // 只读：这一页不渲染编辑表单（写动作全在编辑页）。
  assert.equal(await page.locator('#src-body').count(), 0, '只读页不渲染编辑表单 #src-body');
  assert.equal(await page.locator('#src-detail-edit').isVisible(), true, '编辑入口可见');
  // 脱敏：凭据只写不读，明文不得出现在详情页的任何文本里。
  assert.equal((await page.locator('body').innerText()).includes(d.mock ? 'hmac_mock_secret_deploy_hook' : '__unreachable_secret__'), false, '凭据明文不得出现在页面上');
  // 近期回执走既有过滤参数（不是全量拉回来再本地筛）。
  const recentCalls = (await page.evaluate(() => __sourceCalls)).filter((c) => /\/events\?source=deploy-hook&limit=20$/.test(c.url));
  assert.ok(recentCalls.length >= 1, '近期回执必须 GET …/events?source=<name>&limit=20');
  assert.equal(recentCalls[0].httpStatus, 200, '近期回执端点 200');
  const recentText = (await page.locator('#src-detail [data-sec="recent"]').innerText()).trim();
  assert.ok(recentText.length > 0, '近期回执要么列出事件、要么说清楚还没有（诚实空态）');
  check('Source detail read-only page: name link, four sections, source-filtered receipts, no edit form, masked credentials');
  await shot('source-detail-readonly');

  /* 原始 YAML：默认收起，展开才取（懒加载）——详情页的 details.src-raw。
     凭据只写不读：文档里只有 hookUrl/secretSet，绝无 token/secret 明文（红线）。 */
  const rawYamlDetail = page.locator('#src-detail details.src-raw');
  assert.equal(await rawYamlDetail.count(), 1, '源详情必须有且只有一个「原始 YAML」折叠块（details.src-raw）');
  assert.equal(await rawYamlDetail.evaluate((n) => n.open === true), false, '原始 YAML 默认收起');
  const yamlCallsBeforeDetail = (await page.evaluate(() => __sourceCalls)).filter((c) => /\/sources\/deploy-hook\/yaml$/.test(c.url));
  assert.equal(yamlCallsBeforeDetail.length, 0, '收起时不得请求 /yaml（懒加载）');
  await rawYamlDetail.locator('summary').click();
  await page.waitForFunction(
    () => {
      const box = document.querySelector('#src-detail details.src-raw');
      return !!box && /kind: Source/.test(box.textContent || '');
    },
    { timeout: 15000 },
  );
  const yamlCallsAfterDetail = (await page.evaluate(() => __sourceCalls)).filter((c) => /\/sources\/deploy-hook\/yaml$/.test(c.url));
  assert.ok(yamlCallsAfterDetail.length >= 1, '展开才请求 /yaml');
  assert.equal(
    await labelCountIn(page, "#src-detail details.src-raw", ["sources.v4.yaml", "source_detail.yaml"]),
    1,
    "折叠块内部「原始 YAML」只能出现一次（summary 那次算）：展开后多出一行=codeBlock 又渲染了标题",
  );
  const yamlTextDetail = await rawYamlDetail.textContent();
  assert.match(yamlTextDetail, /kind: Source/, 'YAML 块必须含 kind: Source');
  assert.match(yamlTextDetail, /hookUrl: /, '读投影只有 hookUrl');
  assert.match(yamlTextDetail, /secretSet: true/, 'secret 只报「设了没有」');
  assert.ok(!/^\s*secret:/m.test(yamlTextDetail) && !/^\s*token:/m.test(yamlTextDetail), 'YAML 不得出现凭据字段');
  if (d.mock) assert.equal(yamlTextDetail.includes('hmac_mock_secret_deploy_hook'), false, 'YAML 不得出现 secret 明文');
  check('Source detail raw YAML: collapsed by default, lazy /yaml, kind: Source with masked credentials');
  await shot('source-detail-yaml');

  /* 二、点「编辑源」→ **就地**打开共享编辑器（web/source-editor.js，与列表页同一个组件）：
     页面不跳走，弹层在本页打开并回填。旧断言把 source.html（已删除的重复页）当「编辑页」，
     等于用测试把错误的目标钉住；「跳到列表页再弹窗」也不符合用户预期。 */
  await page.locator('#src-detail-edit').click();
  await visible('#src-modal:not(.hidden)');
  await page.waitForFunction(() => document.getElementById('f-name')?.value === 'deploy-hook');
  assert.equal(location_pathname_is_detail(page), true, '就地编辑：仍停在 source-detail.html');
  assert.equal(new URL(page.url()).searchParams.get('edit'), '1', 'URL 用 edit=1 表示「编辑器开着」');
  assert.equal(await page.locator('#f-name').inputValue(), 'deploy-hook', '编辑器必须回填这个源');
  check('Source detail edit entry opens the shared editor in place (URL keeps edit=1, form prefilled)');
  await shot('source-detail-edit-inline');
  await page.locator('#modal-cancel').click();
  await page.waitForFunction(() => document.getElementById('src-modal').classList.contains('hidden'));

  /* 三、旧 source.html **已删除**（黑盒审计 B-4）。
     它曾与这一页重复：内容 / 副标题 / 目标能力 / 原始 YAML 全部一样，而且没有任何
     页面链到它的带参形式。产品侧删掉了它，`/source.html` 由 `internal/server/static.go`
     的未知文件路径兜成 **404** —— 刻意不是重定向（301 到详情页会让旧书签「看起来还在」，
     用户察觉不到这一页已经并进详情页）。
     断言 HTTP 状态本身：只断「页面里没有 #src-name」那种否定句在这里没有意义
     （一个 200 的白页同样满足它）。 */
  const goneParam = await H.req(base, '/source.html?namespace=default&name=deploy-hook');
  assert.equal(goneParam.status, 404, '旧 source.html 已删除（带参也必须 404），实际 ' + goneParam.status);
  const goneBare = await H.req(base, '/source.html');
  assert.equal(goneBare.status, 404, '旧 source.html 无参同样 404（不与带参行为分叉成重定向），实际 ' + goneBare.status);
  check('Gone source.html: the deleted page answers 404 with and without params (no redirect, no second detail page)');

}
