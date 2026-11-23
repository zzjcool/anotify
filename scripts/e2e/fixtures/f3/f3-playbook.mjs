// Attributed F2c1 package/UI actions. Raw resource snapshots in real mode.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
// #378：危险操作不再是原生 confirm()，而是文档里的 #anotify-dialog；驱动真实按钮。
import { acceptDialog } from './f3-dialogs.mjs';
const files = {
  'playbook.yaml': `apiVersion: anotify/v1\nkind: Playbook\nmetadata:\n  name: project-chain\n  displayName: Project chain\n  version: 1\nspec:\n  values:\n    type: object\n    additionalProperties: false\n    required: [project]\n    properties:\n      project: { type: string, pattern: '^[a-z][a-z0-9-]*$' }\n      env: { type: string, enum: [dev, prod], default: prod }\n      credential: { type: string, writeOnly: true }\n  capabilities:\n    - capabilities/review.yaml\n    - capabilities/fixer.yaml\n  sources:\n    - sources/on-push.yaml\n`,
  'capabilities/review.yaml': `apiVersion: anotify/v1
kind: Capability
metadata:\n  name: '{{values.project}}-review'\nspec:\n  next: ['{{values.project}}-fixer']\n  run:\n    entrypoint: run.sh\n    files:\n      run.sh: |\n        #!/bin/sh\n        echo 'project={{values.project}} env={{values.env}}'\n`,
  'capabilities/fixer.yaml': `apiVersion: anotify/v1
kind: Capability
metadata:\n  name: '{{values.project}}-fixer'\nspec:\n  next: []\n  run:\n    entrypoint: run.sh\n    files:\n      run.sh: |\n        #!/bin/sh\n        echo 'fix {{values.project}}'\n`,
  'sources/on-push.yaml': `apiVersion: anotify/v1
kind: Source
metadata:\n  name: '{{values.project}}-push'\nspec:\n  kind: webhook\n  config:\n    secret: 'e2e-hook-secret'\n  targets:\n    - capability: '{{values.project}}-review'\n      params: { repo: '$(payload.repo)', env: '{{values.env}}' }\n`,
  'README.md': 'A self-contained package; deploy manually after install.\n',
};
const envelope = (map, rv) => ({ apiVersion: 'anotify/v1', kind: 'Playbook',
  metadata: { name: 'project-chain', namespace: 'default', ...(rv == null ? {} : { resourceVersion: rv }) }, spec: { files: map } });
const credential = 'hmac-browser-exact-secret-sentinel';
const hookSecret = 'e2e-hook-secret'; // #536：源的 config.secret 用字面量（writeOnly values 会要求每次 render/install 都传值）。

export async function playbook(d,mode){
 const {page,calls,req,check,shot,clean}=d;
 const goto=(file,query='namespace=default',lang='en')=>d.goto(file,query,lang);
 const facts=d.snapshot;
 const basePath='/v1/namespaces/default/playbooks/project-chain';
 const resources=async()=>JSON.stringify([await req('/v1/namespaces/default/capabilities'),await req('/v1/namespaces/default/sources')]);
 const keptResources=async receiptId=>JSON.stringify([await resources(),await req(basePath+'/installs/'+encodeURIComponent(receiptId)),await req('/v1/namespaces/default/tasks?limit=100'),await req('/v1/console/events?namespace=default')]);
 const archive=d.archive;
  const stored = () => req(basePath + '?includeBody=true');
  // #376：导入表单搬进就地弹层（页头 #btn-import 与空态 CTA 是同一个入口），
  // 所以「上传/新建」之前必须先把弹层打开。
  const openImport = async () => {
    await page.locator('#btn-import').click();
    await page.waitForSelector('#pb-import-modal:not(.hidden)');
  };
  const closeImport = async () => {
    await page.locator('#pb-import-close').click();
    await page.locator('#pb-import-modal').waitFor({ state: 'hidden' });
  };
  const upload = async () => {
    await goto('playbooks');
    await openImport();
    await page.locator('#pb-upload-files').setInputFiles({ name: 'project-chain-files.json', mimeType: 'application/json', buffer: Buffer.from(JSON.stringify(files)) });
    await page.waitForFunction(() => document.querySelector('#pb-input-name').value === 'project-chain');
    await page.locator('#pb-create').click();
    await page.waitForFunction(() => document.querySelector('#pb-grid [data-slug="project-chain"]'));
  };
  const seed = async install => {
    await goto('playbooks');
    await req('/v1/namespaces/default/playbooks', { method: 'POST', body: JSON.stringify(envelope(files)) });
    if (install) await req(basePath + '/install', { method: 'POST', body: JSON.stringify({ values: { project: 'a' } }) });
    await goto('playbook', 'namespace=default&id=project-chain'); await openDrawer();
    await page.locator('#pb-value-project').waitFor(); await closeDrawer();
  };
  /* #521：安装的输入与主按钮都在抽屉里（「试装/渲染」还在它的「高级」折叠里）——操作前先打开；id 一个都没改。 */
  const drawerState = async () => page.evaluate(() => {
    const el = document.getElementById('pb-install-drawer');
    return { hidden: el.classList.contains('hidden'), closing: el.classList.contains('is-closing') };
  });
  const openDrawer = async () => {
    const state = await drawerState();
    if (state.closing) await page.locator('#pb-install-drawer').waitFor({ state: 'hidden' });
    if (state.hidden || state.closing) await page.locator('#pb-install-open').click();
    await page.locator('#pb-install-drawer').waitFor({ state: 'visible' });
  };
  const closeDrawer = async () => {
    if ((await drawerState()).hidden) return;
    await page.locator('#pb-drawer-close').click();
    await page.locator('#pb-install-drawer').waitFor({ state: 'hidden' });
  };
  const openDetails = async id => {
    if (await page.locator(id).evaluate(node => node.open === true)) return;
    await page.locator(id + ' > summary').click();
  };
  const fill = async (secret = false) => {
    await openDrawer();
    await page.locator('#pb-value-project').fill('a');
    if (secret) await page.locator('#pb-value-credential').fill(credential);
  };
  const clickAction = async (id, suffix) => {
    await openDrawer();
    if (id !== '#btn-install') await openDetails('details:has(#btn-render)');
    const before = (await calls()).length; await page.locator(id).click();
    await page.waitForFunction(([before, suffix]) => __c1Calls.slice(before).some(c => c.url.endsWith(suffix)), [before, suffix]);
    await page.waitForFunction(id => !document.querySelector(id).disabled, id);
    return (await calls()).findLast(c => c.url.endsWith(suffix));
  };
  if (mode === 'upload-edit') {
    await upload();
    assert.deepEqual((await stored()).spec.files, files);
    const post = (await calls()).find(c => c.method === 'POST' && c.url.endsWith('/playbooks'));
    assert.deepEqual(Object.keys(post.body.spec), ['files']); assert.equal(post.body.metadata.name, 'project-chain');
    await page.locator('#pb-create').click();
    await page.waitForFunction(() => document.querySelector('#pb-import-result').textContent.includes('Slug exists'));
    assert.equal(await page.locator('#pb-input-name').inputValue(), 'project-chain');
    assert.deepEqual(JSON.parse(await page.locator('#pb-input-files').inputValue()), files);
    await shot('post409-guidance-draft'); await archive();
    // 弹层仍开着（409 保留草稿是契约），点列表卡之前先关掉它。
    await closeImport();
    await page.locator('#pb-grid [data-slug="project-chain"] a').click();
    await page.waitForURL('**/playbook.html?namespace=default&id=project-chain');
    /* #521：值表单在抽屉里、模板编辑在默认折叠的进阶区 —— 各展开一次再操作既有控件。 */
    await openDrawer(); await page.locator('#pb-value-project').waitFor(); await closeDrawer();
    await openDetails('#pb-template-section');
    assert.equal(await page.locator('#pb-file-content').inputValue(), files['playbook.yaml']);
    await page.locator('#pb-file-path').selectOption('capabilities/fixer.yaml');
    const edited = files['capabilities/fixer.yaml'] + '# edited via raw-file UI\n';
    await page.locator('#pb-file-content').fill(edited); await page.locator('#pb-save-template').click();
    await page.waitForFunction(() => document.querySelector('#pb-template-state').textContent.includes('Whole template saved'));
    assert.equal((await stored()).spec.files['capabilities/fixer.yaml'], edited);
    assert.equal((await req(basePath+'/installs')).total, 0);
    const current = await stored(); await req(basePath, { method: 'PUT', body: JSON.stringify(envelope(current.spec.files, current.metadata.resourceVersion)) });
    const draft = edited + '# draft kept after 409\n'; await page.locator('#pb-file-content').fill(draft);
    await page.locator('#pb-save-template').click();
    await page.waitForFunction(() => document.querySelector('#pb-template-state').textContent.includes('draft retained exactly'));
    assert.equal(await page.locator('#pb-file-content').inputValue(), draft); await shot('put409-raw-draft-retained');
    const downloading = page.waitForEvent('download'); await page.locator('#pb-export').click();
    const download = await downloading; const exported = JSON.parse(await readFile(await download.path(), 'utf8'));
    assert.deepEqual(Object.keys(exported).sort(), Object.keys(files).sort()); assert.equal(exported['capabilities/fixer.yaml'], draft);
    await acceptDialog(page, () => page.locator('#pb-reload-template').click());
    await page.waitForFunction(() => document.querySelector('#pb-template-state').textContent.includes('Latest raw package'));
    await page.locator('#pb-file-path').selectOption('capabilities/fixer.yaml');
    assert.equal(await page.locator('#pb-file-content').inputValue(), edited);
    check('Real upload POST/duplicate409 guidance, raw GET, multi-file PUT/no install, stale409 preserves draft, full-map export and explicit reload');
    await clean('upload/edit');
  }
  if (mode === 'install') {
    await seed(false);
    /* #521 首屏 = 状态行 + 安装按钮 + 实例列表；版本历史与体检明细默认折叠，安装输入全在抽屉里。 */
    await page.waitForFunction(() => document.querySelectorAll('#pb-summary [data-status]').length === 3);
    const statusFacts = await page.locator('#pb-summary [data-status]').evaluateAll(nodes => nodes.map(n => [n.dataset.status, n.textContent.trim()]));
    assert.deepEqual(statusFacts.map(f => f[0]), ['version', 'instances', 'checkup'], '状态行三件事：版本 / 实例数 / 体检');
    for (const [key, text] of statusFacts) assert.ok(text.length > 0, '状态行 ' + key + ' 非空：' + JSON.stringify(text));
    assert.equal(await page.locator('#pb-install-drawer').isVisible(), false, '抽屉默认关闭');
    assert.equal(await page.locator('#pb-versions-section').evaluate(n => n.open), false, '版本历史默认折叠');
    assert.equal(await page.locator('#pb-versions').isVisible(), false, '版本历史明细不在首屏');
    assert.equal(await page.locator('#pb-checkup-section').evaluate(n => n.open), false, '体检默认折叠');
    assert.equal(await page.locator('#pb-conditions').isVisible(), false, '体检明细不在首屏');
    await page.locator('#pb-installs').waitFor();
    assert.equal(await page.locator('#pb-installs').isVisible(), true, '实例列表在首屏');
    assert.equal(await page.locator('#pb-installs [data-release]').count(), 1, '实例列表按 release 聚合：一份实例一行');
    assert.equal(await page.locator('#pb-installs a[data-release-link]').count(), 1, '每一行都链到实例详情');
    await shot('first-screen-ia');
    check('Default IA: one status line + install trigger + instance list on screen; version history and checkup detail stay collapsed');
    await fill(true);
    /* 360px（比矩阵的 390 更窄）：抽屉与页面都不许横向滚动。 */
    const viewport = page.viewportSize();
    await page.setViewportSize({ width: 360, height: 780 });
    const narrow = await page.evaluate(() => ({
      page: document.documentElement.scrollWidth, inner: innerWidth,
      drawer: document.getElementById('pb-install-drawer').scrollWidth,
      panel: document.querySelector('.pb-drawer-panel').getBoundingClientRect().width,
    }));
    assert.ok(narrow.page <= narrow.inner + 2, '360px 页面不得横向滚动：' + JSON.stringify(narrow));
    assert.ok(narrow.drawer <= narrow.inner + 2, '360px 抽屉不得横向滚动：' + JSON.stringify(narrow));
    assert.ok(narrow.panel <= narrow.inner, '360px 抽屉面板不超出视口：' + JSON.stringify(narrow));
    await shot('drawer-360-no-hscroll');
    await page.setViewportSize(viewport);
    const before = await facts();
    // /render 返回平铺 RenderResult（能力在 response.capabilities、源在 response.sources，
    // 没有 documents 包装、也从不返回 install）："render 是纯的"就断言响应里没有 install 键。
    const rendered = await clickAction('#btn-render', '/render');
    assert.equal(rendered.response.capabilities.length, 2);
    assert.equal(rendered.response.sources.length, 1);
    assert.ok(!('install' in rendered.response), 'render 不得返回 install 回执');
    assert.equal(await facts(), before); await shot('pure-render');
    const dry = await clickAction('#btn-dry-run', '/install'); assert.equal(dry.response.install.dryRun, true); assert.equal(await facts(), before);
    assert.equal(dry.response.values.credential, '***');
    // install.id 是 required:string（PlaybookInstallRef）：dryRun 也带真实 ins_ 身份，
    // 区别只在"不进库"——所以断言「id 是真实身份 + 库里查不到实例」。
    assert.match(String(dry.response.install.id), /^ins_/);
    assert.equal((await req(basePath + '/installs')).total, 0, 'dryRun 不得落任何实例');
    await shot('dry-run-zero-writes');
    const first = await clickAction('#btn-install', '/install'); const second = await clickAction('#btn-install', '/install');
    assert.notEqual(first.response.install.id, second.response.install.id); assert.deepEqual(second.response.skipped, first.response.created);
    const groups = await req(basePath + '/installs'); assert.equal(groups.total, 1); assert.equal(groups.installs[0].attempts, 2);
    assert.equal(groups.installs[0].lastInstallId, second.response.install.id); assert.equal(groups.installs[0].values.credential, '***');
    assert.ok(!JSON.stringify(groups).includes(credential));
    assert.equal((await req(basePath+'/installs/'+first.response.install.id)).install.id, first.response.install.id);
    /* #548：列表只留「一份实例一行」，单次回执的入口搬到状态行的「最近安装回执」上。 */
    await page.locator('#pb-last-install').click();
    await page.waitForFunction(id => __c1Calls.some(c => c.url.endsWith('/installs/' + id)), second.response.install.id);
    assert.match(await page.locator('#pb-receipt').innerText(), /Install|receipt/i);
    assert.ok(!((await page.locator('#pb-receipt').innerText()).includes(credential)));
    await page.locator('#pb-receipt').scrollIntoViewIfNeeded(); await shot('repeat-fold-single-receipt');
    const routes = (await calls()).map(c => c.url); assert.ok(routes.every(p => !p.endsWith('/deploy')));
    assert.equal(await page.locator('#pb-receipt a').last().getAttribute('href'), 'capabilities.html?namespace=default');
    /* B-4（结构性）：playbook 里对**源**的引用只能链到现存的只读详情页。
       旧链接指向已删除的 source.html（现在 404）——“点了引用跳进死路”就是这么坏的。 */
    const sourceRefs = page.locator('#pb-receipt [data-receipt-axis] a[href^="source-detail.html?"]');
    assert.ok(await sourceRefs.count() >= 1, '回执里源的引用必须链到 source-detail.html（实际：' + await page.locator('#pb-receipt').innerText() + '）');
    assert.match(await sourceRefs.first().getAttribute('href'), /^source-detail\.html\?namespace=default&name=/);
    assert.equal(await page.locator('#pb-receipt a[href^="source.html"]').count(), 0, '不得再链回已删除的 source.html');
    check('Render differs from dryRun; all-state0 dry run, real repeat creates new receipts, digest-fold group/single receipt, exact writeOnly mask/provenance and manual-only deploy CTA');
    await clean('install');
  }
  if (mode === 'matrix') {
    await seed(true);
    const raw = await stored();
    const changed = await page.evaluate(map => {
      const parse = Anotify.model.playbook.parseDocument;
      const review = parse(map['capabilities/review.yaml']); review.spec.next = ['notify', 'future-cap'];
      const source = parse(map['sources/on-push.yaml']); source.spec.targets[0].params.env = 'unapplied-env';
      return { ...map, 'capabilities/review.yaml': JSON.stringify(review), 'sources/on-push.yaml': JSON.stringify(source) };
    }, raw.spec.files);
    await req(basePath, { method: 'PUT', body: JSON.stringify(envelope(changed, raw.metadata.resourceVersion)) });
    await goto('playbook', 'namespace=default&id=project-chain'); await fill();
    const oldResources = await resources();
    const installed = await clickAction('#btn-install', '/install');
    assert.deepEqual(installed.response.unapplied.map(u => u.reason), ['routing_unapplied', 'targets_unapplied']);
    // external / pending 的 ref 是包里写的那一串（plan.go·resolveRefs 原样收集），不补 @版本。
    assert.ok(installed.response.external.includes('notify'), JSON.stringify(installed.response.external));
    assert.ok(installed.response.pending.includes('future-cap'));
    // 用宿主侧读（d.api）而不是页面 fetch：这是"库里有没有"的事实核对，不该造无关的网络失败。
    if (!d.mock) {
      assert.equal((await d.api('/v1/namespaces/default/capabilities/notify')).status, 200);
      assert.equal((await d.api('/v1/namespaces/default/capabilities/future-cap')).status, 404);
    }
    assert.equal(await resources(), oldResources);
    assert.match(await page.locator('#pb-unapplied').innerText(), /NOT ready/); assert.match(await page.locator('#pb-unapplied').innerText(), /Declared|Actual/);
    assert.equal((await req(basePath)).status.ready, false);
    await page.locator('#pb-unapplied').scrollIntoViewIfNeeded(); await shot('unapplied-declared-actual-not-green');
    // New additions appear BEFORE a conflicting source in package order: still no partial writes.
    const current = await stored();
    const clash = await page.evaluate(map => {
      const parse = Anotify.model.playbook.parseDocument;
      const fixer = parse(map['capabilities/fixer.yaml']); fixer.metadata.name = 'never-partially-created';
      const source = parse(map['sources/on-push.yaml']); source.spec.config.token = 'different-browser-token-sentinel';
      return { ...map, 'capabilities/fixer.yaml': JSON.stringify(fixer), 'sources/on-push.yaml': JSON.stringify(source) };
    }, current.spec.files);
    await req(basePath, { method: 'PUT', body: JSON.stringify(envelope(clash, current.metadata.resourceVersion)) });
    await goto('playbook', 'namespace=default&id=project-chain'); await fill(); const before = await facts();
    // render 是纯的：响应只有 values/valuesDigest/files/capabilities/sources，且一次写都没发生。
    const pure = (await clickAction('#btn-render', '/render')).response;
    assert.deepEqual(Object.keys(pure).sort(), ['capabilities', 'files', 'sources', 'values', 'valuesDigest']);
    assert.equal(await facts(), before);
    for (const button of ['#btn-dry-run', '#btn-install']) {
      const result = await clickAction(button, '/install'); assert.equal(result.status, 400); assert.equal(result.reason, 'playbook_conflict');
      assert.ok(result.details.valuesDigest && result.details.hint); assert.ok(result.details.conflicts.some(c => c.fields.includes('config.token')));
      assert.ok(!JSON.stringify(result.details).includes('different-browser-token-sentinel')); assert.equal(await facts(), before);
    }
    assert.match(await page.locator('#pb-preview').innerText(), /zero resource or receipt writes/);
    await page.locator('#pb-preview').scrollIntoViewIfNeeded(); await shot('atomic-real-conflict-all-state-zero');
    check('Reuse never edits next or targets.params; prominent declared/actual/unapplied, pending/external, render ignores real DB conflicts; dry+real true conflict ALL-state0');
    await clean('matrix');
  }
  if (mode === 'delete') {
    await seed(true); const raw = await stored();
    // Cross-document evidence restores facts, not mock's private RV counter. Two genuine
    // concurrent template edits make the current token differ from the UI's original one.
    const changed = { ...raw.spec.files, 'README.md': raw.spec.files['README.md'] + 'Concurrent edit 1\n' };
    const firstEdit = await req(basePath, { method: 'PUT', body: JSON.stringify(envelope(changed, raw.metadata.resourceVersion)) });
    assert.equal(await page.evaluate(() => __c1Transport.findLast(c => c.method === 'PUT').httpStatus), 200, 'First remote PUT HTTP success');
    assert.equal(firstEdit.kind, 'Playbook');
    assert.equal((await stored()).spec.files['README.md'], changed['README.md'], 'First remote edit is stored in original raw spec.files');
    assert.notEqual(changed['README.md'], raw.spec.files['README.md']);
    const changedAgain = { ...changed, 'README.md': changed['README.md'] + 'Concurrent edit 2\n' };
    const current = await req(basePath, { method: 'PUT', body: JSON.stringify(envelope(changedAgain, firstEdit.metadata.resourceVersion)) });
    assert.equal(await page.evaluate(() => __c1Transport.findLast(c => c.method === 'PUT').httpStatus), 200, 'Second remote PUT HTTP success');
    assert.equal(current.kind, 'Playbook');
    assert.equal((await stored()).spec.files['README.md'], changedAgain['README.md'], 'Second remote edit is stored in original raw spec.files');
    assert.notEqual(changedAgain['README.md'], changed['README.md']);
    assert.notEqual(current.metadata.resourceVersion, raw.metadata.resourceVersion, 'Concurrent PUT setup must advance CAS before testing DELETE409');
    await openDetails('#pb-danger-section');
    await acceptDialog(page, () => page.locator('#pb-delete').click());
    /* 409 的提示文案已经本地化：从运行时字典取值再断言 —— 四语都对，改词也不会假红。 */
    const staleHint = await page.evaluate(() => window.AnotifyI18n && window.AnotifyI18n['pb.v4.delete_stale']);
    assert.equal(typeof staleHint, 'string', '字典必须有 pb.v4.delete_stale');
    await page.waitForFunction((hint) => (document.querySelector('#pb-delete-state')?.textContent || '').includes(hint), staleHint);
    assert.equal((await req('/v1/namespaces/default/playbooks')).items.length, 1);
    await page.locator('#btn-rescan').click(); await page.waitForFunction(() => !document.querySelector('#btn-rescan').disabled);
    const installed = await req(basePath + '/installs');
    const receiptId = installed.installs[0]?.lastInstallId;
    assert.ok(receiptId, 'Capture the exact independent receipt ID while its parent exists');
    await req('/v1/namespaces/default/sources/a-push/fire', { method: 'POST', body: JSON.stringify({ payload: { repo: 'parent-retention-history' } }) });
    const history = await req('/v1/namespaces/default/tasks?limit=100');
    assert.ok(history.items.some(task => JSON.stringify(task.spec).includes('"a-review"')), 'An actual installed Source creates execution references before manifest deletion');
    const keptBefore = await keptResources(receiptId);
    await acceptDialog(page, () => page.locator('#pb-delete').click());
    await page.waitForFunction(() => document.querySelector('#pb-delete-state').textContent.includes('Manifest deleted'));
    const removed = (await calls()).findLast(c => c.method === 'DELETE'); assert.ok(removed.url.includes('?resourceVersion=')); assert.equal(removed.body, undefined);
    // `kept` 是保留的资源引用集合（能力钉版本 / 源名）：整体相等即可，顺序不是契约事实。
    assert.deepEqual(removed.response.kept.capabilities.slice().sort(), ['a-review@1', 'a-fixer@1'].sort());
    assert.deepEqual(removed.response.kept.sources.slice().sort(), ['a-push'].sort());
    assert.equal(removed.response.values[0].project, 'a');
    assert.equal(await keptResources(receiptId), keptBefore, 'Complete Capability/Source lists, exact single receipt and historical Task/Event references remain byte-for-byte equal');
    const gone = await d.response(basePath);
    assert.equal(gone.status, 404, 'Deleted manifest stays gone; receipt retention never resurrects the parent');
    const keptReceipt = await d.response(basePath + '/installs/' + encodeURIComponent(receiptId));
    assert.equal(keptReceipt.status, 200, 'The exact independent receipt GET remains readable after parent deletion');
    const goneGroup = await d.response(basePath + '/installs');
    assert.equal(goneGroup.status, 404, 'Deleted parent group GET ' + basePath + '/installs must return 404');
    await page.locator('#pb-delete-state').scrollIntoViewIfNeeded(); await shot('cas-delete-kept-no-cascade');
    check('Real confirmed stale DELETE409, explicit recheck and second confirmation; query-only CAS removes manifest but keeps resources/values/receipts');
    await clean('delete');
  }
  if (mode === 'errors') {
    await seed(true); await goto('playbooks');
    await page.evaluate(() => window.__f3Fault = '/playbooks'); await page.locator('#btn-refresh').click();
    await page.waitForFunction(() => document.querySelector('#pb-state').textContent.includes('disconnected'));
    await shot('real-list-disconnected');
    /* #378：loadErrorNode 现在固定带「复制详情」，所以按 data-err-action 点**重试**，不按"第一个按钮"。 */
    await page.evaluate(() => window.__f3Fault = ''); await page.locator('#pb-state [data-err-action="retry"]').click();
    await page.waitForFunction(() => document.querySelector('#pb-grid [data-slug]'));
    await goto('playbook', 'namespace=default&id=project-chain'); await page.evaluate(() => window.__f3Fault = '/playbooks'); await page.locator('#btn-rescan').click();
    await page.waitForFunction(() => document.querySelector('#pb-load-state').textContent.includes('disconnected'));
    await page.evaluate(() => window.__f3Fault = ''); await page.locator('#pb-load-state [data-err-action="retry"]').click();
    await page.waitForFunction(() => !document.querySelector('#pb-load-state').textContent);
    const invalid = await clickAction('#btn-render', '/render'); assert.equal(invalid.status, 400); assert.equal(invalid.reason, 'values_missing');
    await shot('actual-values-error');
    for (const lang of ['zh-CN', 'ja', 'es']) {
      await goto('playbook', 'namespace=default&id=project-chain', lang);
      /* #521：体检明细默认折叠 —— 先展开，才读得到渲染出来的 type 文本。 */
      await openDetails('#pb-checkup-section');
      if (d.mock) assert.equal(await page.locator('#pb-conditions [data-condition]').count(), 4);
      else {
        /* 读模型给几条 condition 由服务端定（可能一条都没有），所以断言页面条件区与 API
         * 逐条一致（数量 + type 文本），而不是写死条数。 */
        const doc = await d.api(basePath);
        assert.ok(Array.isArray(doc.body.status.conditions));
        assert.equal(await page.locator('#pb-conditions [data-condition]').count(), doc.body.status.conditions.length);
        for (const condition of doc.body.status.conditions) {
          assert.match(await page.locator('#pb-conditions').innerText(), new RegExp(condition.type));
        }
      }
      assert.ok(!(await page.locator('#page-main').innerText()).includes('pb.v4.'));
      await openDrawer(); await page.locator('#pb-value-project').waitFor();
      // #521：抽屉在 #page-main 之外，它的文案要单独扫一遍裸 key。
      assert.ok(!(await page.locator('#pb-install-drawer').innerText()).includes('pb.v4.'));
      assert.match(await page.locator('#pb-install-drawer').innerText(), /\S/, '抽屉不是空壳');
    }
    await goto('playbooks');
    const raw = await stored(); await req(basePath + '?resourceVersion=' + raw.metadata.resourceVersion, { method: 'DELETE' });
    await page.locator('#btn-refresh').click(); await page.waitForFunction(() => !document.querySelector('#pb-empty').classList.contains('hidden'));
    assert.match(await page.locator('#pb-empty').innerText(), /No playbooks/);
    check('True disconnected errors/retry both pages, genuine HTTP400 schema error, 4 languages and real empty list after manifest deletion');
  }
  if (mode === 'release') {
    /* #548 的两级 IA：包页只列 release；一份实例装出了什么、回执、YAML、卸载都在 release 详情页。
       两份并存实例各自进详情 —— 对象不串的判据是「只由这个 release 的回执 created 决定」。 */
    await goto('playbooks');
    await req('/v1/namespaces/default/playbooks', { method: 'POST', body: JSON.stringify(envelope(files)) });
    const installAs = release => req(basePath + '/install', { method: 'POST', body: JSON.stringify({ values: { project: release }, release }) });
    const blue = await installAs('blue'); const green = await installAs('green');
    assert.equal(blue.install.release, 'blue'); assert.equal(green.install.release, 'green');
    // 1) 包页：release 列表（不展开对象）
    await goto('playbook', 'namespace=default&id=project-chain');
    await page.locator('#pb-installs [data-release="blue"]').waitFor();
    const rows = await page.locator('#pb-installs [data-release]').evaluateAll(nodes => nodes.map(node => ({
      release: node.dataset.release, text: node.innerText,
      href: node.querySelector('a[data-release-link]')?.getAttribute('href') || '',
      digest: node.querySelector('[data-digest]')?.getAttribute('title') || '',
    })));
    assert.equal(rows.length, 2, '两份实例 → 列表 2 行：' + JSON.stringify(rows));
    assert.deepEqual(rows.map(row => row.release).sort(), ['blue', 'green']);
    for (const row of rows) {
      assert.equal(row.href, 'release.html?namespace=default&id=project-chain&release=' + row.release, '行必须链到实例详情');
      assert.ok(row.text.includes('Active'), '行里有状态：' + row.text);
      assert.match(row.text, /2 capabilities \/ 1 sources/, '行里有对象数：' + row.text);
      assert.match(row.digest, /^sha256:[0-9a-f]{64}$/, '参数摘要的全文放在 hover 上：' + row.digest);
    }
    assert.ok(!(await page.locator('#pb-installs').innerText()).includes('blue-review'), '包页不得展开对象（这一页只说有几份实例）');
    await shot('release-list-two');
    // 2) 进第一份：对象 / 身份 / 回执
    const objects = () => page.locator('#rd-objects [data-object]').evaluateAll(nodes => nodes.map(node => [node.dataset.object, node.dataset.name]));
    await page.locator('#pb-installs [data-release="blue"] a[data-release-link]').click();
    await page.waitForURL('**/release.html?**release=blue');
    await page.locator('#rd-objects [data-object]').first().waitFor();
    assert.deepEqual((await objects()).map(o => o[1]).sort(), ['blue-fixer', 'blue-push', 'blue-review'], 'blue 详情只列 blue 的对象');
    assert.deepEqual([...new Set((await objects()).map(o => o[0]))].sort(), ['capability', 'source']);
    const facts = await page.locator('#rd-summary [data-fact]').evaluateAll(nodes => Object.fromEntries(nodes.map(node => [node.dataset.fact, node.innerText])));
    assert.match(facts.playbook, /project-chain@v1/, '身份行有包名@版本：' + JSON.stringify(facts));
    assert.equal(facts.release.trim(), 'blue');
    assert.equal(await page.locator('#rd-copy-digest').count(), 1, '参数摘要全文可复制');
    assert.match(await page.locator('#rd-receipt [data-receipt-axis="created"]').innerText(), /blue-review/, '回执 created 与这次安装一致');
    assert.equal(await page.locator('#rd-receipt [data-receipt-axis="dependencies"]').count(), 1);
    await shot('release-detail-blue');
    // 3) 返回列表 → 进第二份：对象不串
    await page.goBack();
    await page.locator('#pb-installs [data-release="green"] a[data-release-link]').click();
    await page.waitForURL('**/release.html?**release=green');
    await page.locator('#rd-objects [data-object]').first().waitFor();
    const greenObjects = (await objects()).map(o => o[1]).sort();
    assert.deepEqual(greenObjects, ['green-fixer', 'green-push', 'green-review'], 'green 详情只列 green 的对象');
    assert.ok(!greenObjects.some(name => name.startsWith('blue-')), 'green 的详情里不得混进 blue 的对象');
    // 直接打开同一 URL（新标签页）结果一致：URL 就是视图
    const directUrl = page.url().replace('release=green', 'release=blue');
    const other = await page.context().newPage();
    await other.goto(directUrl);
    await other.locator('#rd-objects [data-object]').first().waitFor();
    const directObjects = await other.locator('#rd-objects [data-object]').evaluateAll(nodes => nodes.map(node => node.dataset.name).sort());
    await other.close();
    assert.deepEqual(directObjects, ['blue-fixer', 'blue-push', 'blue-review'], '直接打开 URL 与点进来的结果一致');
    // 4) 实例 YAML（#538 的端点，按所选参数组取）：能力整体在源之前，凭据只以 secretSet 出现
    await goto('release', 'namespace=default&id=project-chain&release=blue');
    await page.locator('#rd-objects [data-object]').first().waitFor();
    await page.locator('#rd-yaml').click();
    await page.locator('#rd-yaml-drawer').waitFor({ state: 'visible' });
    await page.locator('#rd-yaml-text').waitFor();
    const releaseYaml = await page.locator('#rd-yaml-text').evaluate(node => node.textContent);
    assert.equal(releaseYaml.split('\n').filter(line => line.trim() === '---').length, 3, '2 能力 + 1 源 → 3 段文档：' + releaseYaml);
    for (const ref of ['name: blue-review', 'name: blue-fixer', 'name: blue-push']) assert.ok(releaseYaml.includes(ref), '导出缺 ' + ref);
    assert.ok(releaseYaml.indexOf('name: blue-review') < releaseYaml.indexOf('name: blue-push'), '能力文档整体在源文档之前');
    assert.match(releaseYaml, /secretSet: true/);
    assert.ok(!releaseYaml.includes(hookSecret) && !releaseYaml.includes('config.secret'), '导出不得回显凭据原文');
    assert.ok(!releaseYaml.includes('green-'), '按组导出不得混进另一份实例的对象');
    await shot('release-yaml');
    await page.locator('#rd-yaml-close').click();
    await page.locator('#rd-yaml-drawer').waitFor({ state: 'hidden' });
    // 5) 卸载：这一页给明确状态，回到包页后该 release 消失，直接打开旧 URL 也不是白屏
    await acceptDialog(page, () => page.locator('#rd-uninstall').click());
    await page.locator('#rd-gone-panel').waitFor();
    assert.match(await page.locator('#rd-gone-panel').innerText(), /uninstalled or never existed/i);
    await goto('playbook', 'namespace=default&id=project-chain');
    await page.waitForFunction(() => document.querySelectorAll('#pb-installs [data-release]').length === 1);
    assert.equal(await page.locator('#pb-installs [data-release="blue"]').count(), 0, '卸载后从包页消失');
    await goto('release', 'namespace=default&id=project-chain&release=blue');
    await page.locator('#rd-gone-panel').waitFor();
    assert.ok((await page.locator('#rd-gone-panel').innerText()).trim().length > 0, '旧 URL 给明确状态，不是白屏');
    assert.equal(await page.locator('#rd-content').isVisible(), false, '已卸载不渲染对象区');
    assert.equal(await page.locator('#rd-gone-back').getAttribute('href'), 'playbook.html?namespace=default&id=project-chain');
    await shot('release-uninstalled');
    // 6) 0 对象的实例：详情给一句话，YAML 直接空态且不发请求
    const emptyEntry = 'apiVersion: anotify/v1\nkind: Playbook\nmetadata:\n  name: empty-pkg\nspec:\n  capabilities: []\n  sources: []\n';
    await req('/v1/namespaces/default/playbooks', { method: 'POST', body: JSON.stringify({ ...envelope({ 'playbook.yaml': emptyEntry }), metadata: { name: 'empty-pkg', namespace: 'default' } }) });
    await req('/v1/namespaces/default/playbooks/empty-pkg/install', { method: 'POST', body: JSON.stringify({ values: { project: 'a' } }) });
    await goto('release', 'namespace=default&id=empty-pkg&release=empty-pkg');
    await page.locator('#rd-objects').waitFor();
    assert.equal(await page.locator('#rd-objects [data-object]').count(), 0);
    assert.match(await page.locator('#rd-objects').innerText(), /has not installed any objects/i);
    const yamlCallsBefore = (await calls()).filter(call => call.url.endsWith('/yaml')).length;
    await page.locator('#rd-yaml').click();
    await page.locator('#rd-yaml-drawer').waitFor({ state: 'visible' });
    assert.equal(await page.locator('#rd-yaml-empty').isVisible(), true, '0 对象实例直接给空态');
    assert.equal(await page.locator('#rd-yaml-text').count(), 0, '空态不得留空的 <pre>/<code>');
    assert.equal((await calls()).filter(call => call.url.endsWith('/yaml')).length, yamlCallsBefore, '0 对象空态不发请求');
    await page.locator('#rd-yaml-close').click();
    await page.locator('#rd-yaml-drawer').waitFor({ state: 'hidden' });
    check('#548 two-level IA: release list without objects, per-release detail objects/receipt, no crossing, direct URL, uninstall → explicit state, empty YAML');
    await clean('release');
  }
  if (mode === 'missing') {
    // #380：不存在的 playbook id → 徽章必须从「正在读取…」收敛到失败态，
    // 且失败路径不得留下空圆角容器（#pb-summary 是 card card-pad + min-height:80px）。
    const missing = 'f3-no-such-package';
    await goto('playbook', 'namespace=default&id=' + missing);
    await page.waitForFunction(() => document.querySelector('#pb-badge').className.includes('badge-error'));
    const badge = await page.evaluate(() => {
      const el = document.querySelector('#pb-badge');
      return { text: el.textContent.trim(), className: el.className };
    });
    assert.equal(badge.text, 'Load failed', '#380 失败徽章必须写成「载入失败」，不能停在「正在读取…」');
    assert.equal(badge.className, 'badge badge-error', '#380 失败徽章用 badge-error 语义色');
    assert.ok(!(await page.locator('#page-main').innerText()).includes('Loading…'), '#380 失败后页面上不得残留 loading 文案');
    const summary = await page.evaluate(() => {
      const el = document.querySelector('#pb-summary');
      const box = el.getBoundingClientRect();
      return { hidden: el.hidden, display: getComputedStyle(el).display, children: el.children.length, height: box.height };
    });
    assert.equal(summary.children, 0, '#380 失败路径不得往空容器里塞内容');
    assert.ok(summary.hidden === true && summary.display === 'none' && summary.height === 0,
      '#380 失败路径必须移除残留空容器（hidden/display:none/0 高），实际 ' + JSON.stringify(summary));
    // 错误面板与重试入口必须还在：失败不是死路。
    /* #378：错误态里除「重试」还有「复制详情」（报障要机器事实），所以断言从
       "按钮恰好 1 个"改成"恰好 1 个重试入口" —— 意图（失败不是死路）不变。 */
    assert.equal(await page.locator('#pb-load-state [data-err-action="retry"]').count(), 1, '#380 失败态保留重试入口');
    assert.match(await page.locator('#pb-load-state').innerText(), /Failed to load/);
    assert.ok(!(await page.locator('#page-main').innerText()).includes('pb.v4.'), '没有漏翻的 i18n key');
    await shot('missing-package-failed-badge');
    // 再点一次重试：仍然 404，徽章不得回退成 loading 或 success。
    await page.locator('#pb-load-state [data-err-action="retry"]').click();
    await page.waitForFunction(() => document.querySelector('#pb-load-state').textContent.includes('Failed to load'));
    assert.equal((await page.locator('#pb-badge').innerText()).trim(), 'Load failed');
    assert.equal(await page.locator('#pb-summary').isVisible(), false, '#380 重试失败后空容器仍然不出现');
    check('#380 missing playbook id → failed badge text/DOM, no residual empty card, retry stays failed');
    await clean('missing');
  }

}
