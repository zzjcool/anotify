// Attributed F2b1 CRUD/fire/dryrun/deep-link chains. Both modes exact namespaced requests.
import assert from 'node:assert/strict';
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
export async function source(d){
 const {page,base,req,check,shot}=d;const state=d.snapshot;
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
  await pickMenu('ui-webhook', 'Disable');
  await openMenu('ui-webhook');
  await visible('#src-list > div[data-name="ui-webhook"] button:text-is("Enable")');
  assert.equal((await req('/v1/namespaces/default/sources/ui-webhook')).spec.enabled, false);
  await row('ui-webhook').locator('.menu-item', { hasText: 'Enable' }).click();
  await openMenu('ui-webhook');
  await visible('#src-list > div[data-name="ui-webhook"] button:text-is("Disable")');
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
  await page.locator('#action-error').getByRole('button', { name: 'Reload latest resource' }).click();
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
  await page.locator('#modal-close').click();
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

  /* 二、点编辑入口 → 原有编辑页：图 / 主体 / 边投影 / 原始 YAML 的原断言全部保留。 */
  await page.locator('#src-detail-edit').click();
  await page.waitForFunction(() => location.pathname.endsWith('/source.html'));
  await visible('#src-flow svg.flograph-svg');
  assert.match(await page.locator('#src-name').innerText(), /deploy-hook/);
  assert.match(await page.locator('#src-body').innerText(), /secretSet: true/);
  assert.ok(!(await page.locator('body').innerText()).includes(d.mock ? 'hmac_mock_secret_deploy_hook' : '__unreachable_secret__'), '凭据明文不得出现在页面上');
  const topology = await page.evaluate(() => document.getElementById('src-flow')._graphSpec || document.getElementById('src-flow')._mountSpec);
  assert.ok(topology, 'real graph rendered');
  assert.equal(topology.preview, true, 'configuration preview, not execution success');
  assert.ok(topology.nodes.some(n => n.id === '__source__' && n.label === 'deploy-hook'));
  assert.ok(topology.nodes.some(n => n.id === 'fixer'));
  assert.ok(topology.edges.some(e => e.from === '__source__' && e.to === 'fixer'));
  assert.ok(topology.edges.some(e => e.from === 'fixer' && e.to === 'analyze-pr'));
  assert.ok(topology.edges.every(e => !e.port && !('priority' in e)));
  // 边投影段落显示的是 v4 `{from,to,kind}`（CapabilityEdge），以及"这个源的入口边"：
  // kind="source"、from=源名。旧断言检查 v3 字段名 fromIsSource —— 那在真实服务上
  // 永远不存在（页面会渲染空数组），等于什么都没验。
  // #375：「只读 v4 目标能力投影」不再作为主内容重复渲染（与 targets 同一事实），
  // 原始投影进开发者信息 #src-edges 折叠 —— 断言改读折叠区（textContent 含折叠内容）。
  const edgeText = await page.locator('#src-edges').textContent();
  assert.match(edgeText, /"kind": "source"/);
  assert.match(edgeText, /"from": "deploy-hook"/);
  check('Source detail real metadata.name deep link/ns, masked config/status/params/projected edges and rendered graph');
  await shot('source-detail-graph');

  /* 原始 YAML：默认收起，展开才取（懒加载）。凭据只写不读：文档里只有
     hookUrl/secretSet，绝无 token/secret 明文（红线）。 */
  const rawYaml = page.locator('#src-yaml');
  assert.equal(await rawYaml.count(), 1, '源详情必须有「原始 YAML」折叠块（#src-yaml）');
  assert.equal(await rawYaml.evaluate((n) => n.open === true), false, '原始 YAML 默认收起');
  const yamlCallsBefore = (await page.evaluate(() => __sourceCalls)).filter((c) => /\/sources\/deploy-hook\/yaml$/.test(c.url));
  assert.equal(yamlCallsBefore.length, 0, '收起时不得请求 /yaml（懒加载）');
  await rawYaml.locator('summary').click();
  await page.waitForFunction(
    () => {
      const box = document.getElementById('src-yaml');
      return !!box && /kind: Source/.test(box.textContent || '');
    },
    { timeout: 15000 },
  );
  const yamlCallsAfter = (await page.evaluate(() => __sourceCalls)).filter((c) => /\/sources\/deploy-hook\/yaml$/.test(c.url));
  assert.ok(yamlCallsAfter.length >= 1, '展开才请求 /yaml');
  assert.equal(
    await labelCountIn(page, "#src-yaml", ["sources.v4.yaml", "source_detail.yaml", "yaml"]),
    1,
    "编辑页 YAML 折叠块内部「原始 YAML」只能出现一次（summary 那次算）",
  );
  const yamlText = await rawYaml.textContent();
  assert.match(yamlText, /kind: Source/, 'YAML 块必须含 kind: Source');
  assert.match(yamlText, /hookUrl: /, '读投影只有 hookUrl');
  assert.match(yamlText, /secretSet: true/, 'secret 只报「设了没有」');
  assert.ok(!/^\s*secret:/m.test(yamlText) && !/^\s*token:/m.test(yamlText), 'YAML 不得出现凭据字段');
  if (d.mock) assert.equal(yamlText.includes('hmac_mock_secret_deploy_hook'), false, 'YAML 不得出现 secret 明文');
  assert.equal((await page.locator('body').innerText()).includes(d.mock ? 'hmac_mock_secret_deploy_hook' : '__unreachable_secret__'), false, '凭据明文不得出现在页面上');
  check('Source detail raw YAML: collapsed by default, lazy /yaml, kind: Source with masked credentials');
  await shot('source-detail-yaml');
}
