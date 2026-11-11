// Attributed F2c1 package/UI actions. Raw resource snapshots in real mode.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
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
metadata:\n  name: '{{values.project}}-push'\nspec:\n  kind: webhook\n  config: {}\n  targets:\n    - capability: '{{values.project}}-review'\n      params: { repo: '$(payload.repo)', env: '{{values.env}}' }\n`,
  'README.md': 'A self-contained package; deploy manually after install.\n',
};
const envelope = (map, rv) => ({ apiVersion: 'anotify/v1', kind: 'Playbook',
  metadata: { name: 'project-chain', namespace: 'default', ...(rv == null ? {} : { resourceVersion: rv }) }, spec: { files: map } });
const credential = 'hmac-browser-exact-secret-sentinel';

export async function playbook(d,mode){
 const {page,calls,req,check,shot,clean}=d;
 const goto=(file,query='namespace=default',lang='en')=>d.goto(file,query,lang);
 const facts=d.snapshot;
 const basePath='/v1/namespaces/default/playbooks/project-chain';
 const resources=async()=>JSON.stringify([await req('/v1/namespaces/default/capabilities'),await req('/v1/namespaces/default/sources')]);
 const keptResources=async receiptId=>JSON.stringify([await resources(),await req(basePath+'/installs/'+encodeURIComponent(receiptId)),await req('/v1/namespaces/default/tasks?limit=100'),await req('/v1/console/events?namespace=default')]);
 const archive=d.archive;
  const stored = () => req(basePath + '?includeBody=true');
  const upload = async () => {
    await goto('playbooks');
    await page.locator('#pb-upload-files').setInputFiles({ name: 'project-chain-files.json', mimeType: 'application/json', buffer: Buffer.from(JSON.stringify(files)) });
    await page.waitForFunction(() => document.querySelector('#pb-input-name').value === 'project-chain');
    await page.locator('#pb-create').click();
    await page.waitForFunction(() => document.querySelector('#pb-grid [data-slug="project-chain"]'));
  };
  const seed = async install => {
    await goto('playbooks');
    await req('/v1/namespaces/default/playbooks', { method: 'POST', body: JSON.stringify(envelope(files)) });
    if (install) await req(basePath + '/install', { method: 'POST', body: JSON.stringify({ values: { project: 'a' } }) });
    await goto('playbook', 'namespace=default&id=project-chain'); await page.locator('#pb-value-project').waitFor();
  };
  const fill = async (secret = false) => {
    await page.locator('#pb-value-project').fill('a');
    if (secret) await page.locator('#pb-value-credential').fill(credential);
  };
  const clickAction = async (id, suffix) => {
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
    await page.locator('#pb-grid [data-slug="project-chain"] a').click();
    await page.waitForURL('**/playbook.html?namespace=default&id=project-chain');
    await page.locator('#pb-value-project').waitFor();
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
    page.once('dialog', dialog => dialog.accept()); await page.locator('#pb-reload-template').click();
    await page.waitForFunction(() => document.querySelector('#pb-template-state').textContent.includes('Latest raw package'));
    await page.locator('#pb-file-path').selectOption('capabilities/fixer.yaml');
    assert.equal(await page.locator('#pb-file-content').inputValue(), edited);
    check('Real upload POST/duplicate409 guidance, raw GET, multi-file PUT/no install, stale409 preserves draft, full-map export and explicit reload');
    await clean('upload/edit');
  }
  if (mode === 'install') {
    await seed(false); await fill(true); const before = await facts();
    const rendered = await clickAction('#btn-render', '/render'); assert.ok(rendered.response.documents.capabilities.length === 2); assert.ok(!rendered.response.install);
    assert.equal(await facts(), before); await shot('pure-render');
    const dry = await clickAction('#btn-dry-run', '/install'); assert.equal(dry.response.install.dryRun, true); assert.equal(await facts(), before);
    assert.equal(dry.response.values.credential, '***'); assert.equal(dry.response.install.id, null); await shot('dry-run-zero-writes');
    const first = await clickAction('#btn-install', '/install'); const second = await clickAction('#btn-install', '/install');
    assert.notEqual(first.response.install.id, second.response.install.id); assert.deepEqual(second.response.skipped, first.response.created);
    const groups = await req(basePath + '/installs'); assert.equal(groups.total, 1); assert.equal(groups.installs[0].attempts, 2);
    assert.equal(groups.installs[0].lastInstallId, second.response.install.id); assert.equal(groups.installs[0].values.credential, '***');
    assert.ok(!JSON.stringify(groups).includes(credential));
    assert.equal((await req(basePath+'/installs/'+first.response.install.id)).install.id, first.response.install.id);
    await page.locator('#pb-installs button').click();
    await page.waitForFunction(id => __c1Calls.some(c => c.url.endsWith('/installs/' + id)), second.response.install.id);
    assert.match(await page.locator('#pb-receipt').innerText(), /Install|receipt/i);
    assert.ok(!((await page.locator('#pb-receipt').innerText()).includes(credential)));
    await page.locator('#pb-receipt').scrollIntoViewIfNeeded(); await shot('repeat-fold-single-receipt');
    const routes = (await calls()).map(c => c.url); assert.ok(routes.every(p => !p.endsWith('/deploy')));
    assert.equal(await page.locator('#pb-receipt a').last().getAttribute('href'), 'capabilities.html?namespace=default');
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
    assert.ok(installed.response.external.some(ref => ref.startsWith('notify@'))); assert.ok(installed.response.pending.includes('future-cap'));
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
    assert.ok((await clickAction('#btn-render', '/render')).response.documents); assert.equal(await facts(), before);
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
    // Cross-document evidence restores facts, not mock's private RV counter.
    // Two genuine concurrent template edits ensure the current token differs
    // from the UI's original token; no fake CAS response or dropped assertion.
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
    page.once('dialog', dialog => dialog.accept()); await page.locator('#pb-delete').click();
    await page.waitForFunction(() => document.querySelector('#pb-delete-state').textContent.includes('Stale CAS'));
    assert.equal((await req('/v1/namespaces/default/playbooks')).items.length, 1);
    await page.locator('#btn-rescan').click(); await page.waitForFunction(() => !document.querySelector('#btn-rescan').disabled);
    const installed = await req(basePath + '/installs');
    const receiptId = installed.installs[0]?.lastInstallId;
    assert.ok(receiptId, 'Capture the exact independent receipt ID while its parent exists');
    await req('/v1/namespaces/default/sources/a-push/fire', { method: 'POST', body: JSON.stringify({ payload: { repo: 'parent-retention-history' } }) });
    const history = await req('/v1/namespaces/default/tasks?limit=100');
    assert.ok(history.items.some(task => JSON.stringify(task.spec).includes('"a-review"')), 'An actual installed Source creates execution references before manifest deletion');
    const keptBefore = await keptResources(receiptId);
    page.once('dialog', dialog => dialog.accept()); await page.locator('#pb-delete').click();
    await page.waitForFunction(() => document.querySelector('#pb-delete-state').textContent.includes('Manifest deleted'));
    const removed = (await calls()).findLast(c => c.method === 'DELETE'); assert.ok(removed.url.includes('?resourceVersion=')); assert.equal(removed.body, undefined);
    // `kept` 是删清单后保留的资源引用**集合**（能力钉版本 / 源名）：整体内容相等即可，
    // 顺序不是契约事实（真实服务按名字排序，mock 保持安装顺序）。
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
    await page.evaluate(() => window.__f3Fault = ''); await page.locator('#pb-state button').click();
    await page.waitForFunction(() => document.querySelector('#pb-grid [data-slug]'));
    await goto('playbook', 'namespace=default&id=project-chain'); await page.evaluate(() => window.__f3Fault = '/playbooks'); await page.locator('#btn-rescan').click();
    await page.waitForFunction(() => document.querySelector('#pb-load-state').textContent.includes('disconnected'));
    await page.evaluate(() => window.__f3Fault = ''); await page.locator('#pb-load-state button').click();
    await page.waitForFunction(() => !document.querySelector('#pb-load-state').textContent);
    const invalid = await clickAction('#btn-render', '/render'); assert.equal(invalid.status, 400); assert.equal(invalid.reason, 'values_missing');
    await shot('actual-values-error');
    for (const lang of ['zh-CN', 'ja', 'es']) {
      await goto('playbook', 'namespace=default&id=project-chain', lang); await page.locator('#pb-value-project').waitFor();
      assert.equal(await page.locator('#pb-conditions [data-condition]').count(), 4);
      assert.ok(!(await page.locator('#page-main').innerText()).includes('pb.v4.'));
    }
    await goto('playbooks');
    const raw = await stored(); await req(basePath + '?resourceVersion=' + raw.metadata.resourceVersion, { method: 'DELETE' });
    await page.locator('#btn-refresh').click(); await page.waitForFunction(() => !document.querySelector('#pb-empty').classList.contains('hidden'));
    assert.match(await page.locator('#pb-empty').innerText(), /No playbooks/);
    check('True disconnected errors/retry both pages, genuine HTTP400 schema error, 4 languages and real empty list after manifest deletion');
  }

}
