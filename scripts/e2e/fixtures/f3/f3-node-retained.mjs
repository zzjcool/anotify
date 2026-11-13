// Attributed F2c2 actual node/config/retained actions. Explicit mock alone permits fixture identity/failure knobs.
import assert from 'node:assert/strict';
export async function nodeRetained(d,mode){
 const {page,check,shot,clean,base}=d;
 const goto=(file,lang='en',query='?namespace=default')=>d.goto(file,query.replace(/^\?/,''),lang);
 const req=d.response,calls=d.calls;
 const waitState=(id,text)=>page.waitForFunction(([id,text])=>document.getElementById(id).textContent.includes(text),[id,text]);
 const nodePath='/v1/nodes/anode-1',configPath=nodePath+'/config';
 const noLeaks=async()=>{ const ui=await page.locator('#page-main').innerText();const gets=await page.evaluate(()=>JSON.stringify(__f3Calls.filter(c=>c.method==='GET'&&c.url.startsWith('/v1/nodes')).map(c=>c.result)));for(const v of ['C2_EXACT_HOST_VALUE_NOT_PUBLIC_728f','COMPLETE_PRIVATE_WHITELIST_NAME','/private/export/absolute/c2-not-public']) {assert.ok(!ui.includes(v));assert.ok(!gets.includes(v));}};
 if(mode==='retained') await d.context.addInitScript(()=>Object.defineProperty(Notification,'permission',{configurable:true,get:()=> 'denied'}));
  if (mode === 'create') {
    await goto('nodes', 'en', '?namespace=default');
    assert.equal((await req('/v1/nodes')).body.kind, 'NodeList');
    await page.locator('#node-register-btn').click(); await page.locator('#reg-name').fill('browser-new-node'); await page.locator('#reg-submit').click();
    await page.waitForFunction(() => document.getElementById('reg-token').textContent.length > 0);
    const token = await page.locator('#reg-token').innerText();
    const post = (await calls()).find(c => c.method === 'POST' && c.url === '/v1/nodes');
    assert.deepEqual(post.body, { apiVersion: 'anotify/v1', kind: 'Node', metadata: { name: 'browser-new-node' }, spec: {} });
    // v4 的 NodeStatus 必须带 online / draining 两个**派生** boolean（api/resource.yaml·NodeStatus）：
    // 控制台「节点」页只认 `typeof status.online === 'boolean'` 来区分在线/离线，缺了它页面
    // 只能显示 Unknown（曾经就是「节点明明在线，控制台说看不到」）。
    // 刚注册、没有任何会话的节点：online=false、draining=false。
    assert.equal(post.result.status.online, false, 'v4 NodeStatus 必须携带 online（否则页面只能显示 Unknown）');
    assert.equal(post.result.status.draining, false, 'v4 NodeStatus 必须携带 draining');
    assert.deepEqual(post.result.spec.namespaces, ['*']);
    const issued = (await calls()).find(c => c.url.endsWith('/browser-new-node/tokens'));
    assert.deepEqual(issued.body, {}); assert.equal(issued.result.kind, 'NodeToken'); assert.deepEqual(Object.keys(issued.result).sort(), ['apiVersion','kind','node','token']);
    // yaml 的 server 行由页面从**当前 origin** 推导（http→ws / https→wss）：真实权威跑在
    // http://localhost:<port>（mock 预览是 127.0.0.1），所以断言真实主机名，别写死 IP。
    assert.ok((await page.locator('#reg-yaml').innerText()).includes('server: "ws://' + new URL(base).host));
    assert.ok(!(await page.locator('#reg-yaml').innerText()).includes('instances:'));
    // 一条命令安装（D3）：命令本身就是“拿去就能跑”的那个事实，所以断言它真的指到这个
    // 中枢、这个节点名、这把真令牌（令牌只进 textContent，不进 URL/localStorage）。
    const bootstrap = await page.locator('#reg-bootstrap').innerText();
    const origin = new URL(base).origin;
    // 令牌必须与中枢真正签发的那把做**值等价**（不能只拿同页 #reg-token 自证），
    // 下载 host 也必须钉死到当前 origin（否则一条“能跑但装到别处”的命令也能过）。
    assert.equal(token, issued.result.token);
    assert.ok(bootstrap.includes('curl -fsSL ' + origin + '/install-anode.sh | sh -s -- --name browser-new-node --server ' + origin + ' --token ' + issued.result.token), bootstrap);
    assert.ok(!bootstrap.includes('--token-file'), bootstrap);
    // 顺序必须是 `curl -fsSL … | sh -s --`：反了（先 sh 后 curl）根本装不上。
    assert.ok(bootstrap.indexOf('curl -fsSL') >= 0 && bootstrap.indexOf('curl -fsSL') < bootstrap.indexOf('| sh -s --'), bootstrap);
    assert.equal(await page.locator('#reg-bootstrap-copy').count(), 1);
    assert.equal(await page.locator('#reg-install').innerText(), 'anode install'); assert.equal(await page.locator('#reg-serve').innerText(), 'anode serve');
    const read = await req('/v1/nodes/browser-new-node'); assert.ok(!JSON.stringify(read.body).includes(token));
    assert.ok(!JSON.stringify((await req('/v1/nodes/browser-new-node/config')).body).includes(token));
    await page.locator('#reg-close').click(); assert.equal(await page.locator('#reg-token').innerText(), '');
    // 关闭面板 = 把一次性明文从 DOM 里抹掉（与 #reg-token 同档）：token/yaml/bootstrap 都清空。
    assert.equal(await page.locator('#reg-bootstrap').textContent(), '');
    const rotate = page.locator('[data-node="browser-new-node"] button');
    const before = (await calls()).filter(c => c.url.endsWith('/tokens')).length;
    page.once('dialog', dialog => dialog.dismiss()); await rotate.click();
    assert.equal((await calls()).filter(c => c.url.endsWith('/tokens')).length, before);
    page.once('dialog', dialog => dialog.accept()); await rotate.click();
    await page.waitForFunction(() => document.getElementById('reg-token').textContent.length > 0);
    const rotated = await page.locator('#reg-token').innerText(); assert.notEqual(rotated, token);
    assert.equal((await calls()).findLast(c => c.url.endsWith('/browser-new-node/tokens')).result.token, rotated);
    await page.locator('#reg-close').click(); await page.locator('#node-register-btn').click(); await page.locator('#reg-name').fill('browser-new-node'); await page.locator('#reg-submit').click();
    await waitState('reg-error', 'AlreadyExists'); assert.equal(await page.locator('#reg-name').inputValue(), 'browser-new-node');
    assert.equal(await page.locator('#reg-token').innerText(), ''); assert.ok((await calls()).every(c => !(c.method === 'GET' && c.url.endsWith('/tokens'))));
    await shot('create409-draft-no-token'); await noLeaks(); await clean('create and rotation');
    check('Actual offline envelope POST → {} token action; once-only long identity, masked reads, confirmed rotation invalidates old identity, duplicate409 preserves input; one-liner bootstrap (curl -fsSL … | sh -s -- --name/--server/--token) + copy button + close clears plaintext; exact anode.yaml/install/serve/emit; no auto-registration or GET token');
  }
  if (mode === 'config') {
    await goto('node', 'en', '?id=anode-1&namespace=default'); await page.locator('#node-config-save:not([disabled])').waitFor();
    // #375：三块 JSON textarea 默认收在「编辑原始 JSON」折叠里；要 .fill() 先展开。
    await page.locator('#node-config-advanced').evaluate(el => { el.open = true; });
    assert.equal(await page.locator('#node-effective-vars [data-key="SOURCE_ROOT"] td').nth(1).innerText(), '/local-src');
    assert.equal(await page.locator('#node-effective-vars [data-key="SOURCE_ROOT"] td').nth(2).innerText(), 'local');
    assert.equal(await page.locator('#node-effective-vars [data-key="NODE_ONLY"] td').nth(2).innerText(), 'node');
    assert.equal(await page.locator('#node-effective-vars [data-key="NS_ONLY"] td').nth(2).innerText(), 'namespace');
    if (d.mock) {
      assert.equal(await page.locator('#node-machine-limits [data-key="total"] td').nth(1).innerText(), '4');
      assert.equal(await page.locator('#node-machine-limits [data-key="perCapability.fixer"] td').nth(2).innerText(), 'local');
      assert.equal(await page.locator('#node-machine-limits [data-key="perCapability.*"] td').nth(2).innerText(), 'node');
    } else {
      /* 真实权威下**预算只有节点层**这一份可读事实：NodeStatus（resource.yaml·NodeStatus）
       * 没有 machineLimits / localLimits 两个字段，hello 的 localLimits 只进
       * node_configs.local_limits（grant 用），读模型不投影。所以这里断言页面显示的
       * 就是 config.limits 的真实值，且来源层标 node —— 并显式断言读模型**没有**
       * local 层，把「页面拿不到本机预算」这件事钉成事实而不是静默忽略。 */
      const cfg = (await req(configPath)).body;
      const node = (await req('/v1/nodes/anode-1')).body;
      assert.ok(!('localLimits' in node.status) && !('machineLimits' in node.status));
      assert.equal(await page.locator('#node-machine-limits [data-key="total"] td').nth(1).innerText(), String(cfg.limits.total));
      assert.equal(await page.locator('#node-machine-limits [data-key="perCapability.*"] td').nth(1).innerText(), String(cfg.limits.perCapability['*']));
      assert.equal(await page.locator('#node-machine-limits [data-key="perCapability.fixer"] td').nth(1).innerText(), String(cfg.limits.perCapability.fixer));
      assert.equal(await page.locator('#node-machine-limits [data-key="total"] td').nth(2).innerText(), 'node');
    }
    assert.equal(await page.locator('#node-machine-vars [data-key="NS_ONLY"]').count(), 0);
    const config = (await req(configPath)).body;
    const concurrent = await req(configPath, { metadata: config.metadata, vars: { SOURCE_ROOT: '/new-node-default', NODE_ONLY: 'remote-default' } }, 'PUT'); assert.equal(concurrent.status, 200);
    const draft = '{\n  "NODE_ONLY": "browser-draft-kept",\n  "SOURCE_ROOT": "/new-node-default"\n}';
    await page.locator('#node-vars').fill(draft); await page.locator('#node-config-save').click(); await waitState('node-config-state', 'draft retained exactly');
    assert.equal(await page.locator('#node-vars').inputValue(), draft); await shot('config409-draft');
    page.once('dialog', dialog => dialog.dismiss()); await page.locator('#node-config-reload').click(); assert.equal(await page.locator('#node-vars').inputValue(), draft);
    page.once('dialog', dialog => dialog.accept()); await page.locator('#node-config-reload').click(); await waitState('node-config-state', 'Current configuration loaded');
    assert.deepEqual(JSON.parse(await page.locator('#node-vars').inputValue()), concurrent.body.vars);
    await page.locator('#node-vars').fill('{"NODE_ONLY":"ui-saved", "SOURCE_ROOT":"/new-node-default"}'); await page.locator('#node-config-save').click(); await waitState('node-config-state', 'Saved.');
    const saved = (await req(configPath)).body; assert.equal(saved.vars.NODE_ONLY, 'ui-saved'); assert.equal(saved.appliedRevision, config.appliedRevision); assert.notEqual(saved.configRevision, config.configRevision);
    const uiPut = (await calls()).filter(c => c.method === 'PUT' && c.url === configPath).at(-1); assert.deepEqual(Object.keys(uiPut.body).sort(), ['limits','metadata','namespaces','vars']);
    // 语义断言：保存后 appliedRevision 未变 → UI 必须透出「待应用」状态。
    // 旧版靠英文协议字面量 ConfigPending，本轮该文案已本地化，改用语言中立的 data 钩子。
    assert.equal(await page.locator('#node-applied [data-apply-state]').getAttribute('data-apply-state'), 'pending');
    await page.locator('#node-namespace').selectOption('prod');
    // 旧版渲染成 "namespace=prod" 的键值串；本轮改成 KV 对（本地化标签 + 值），
    // 改用 data-key 锚点断言「值本身」——比断言某个译法更稳。
    await page.waitForFunction(() => (document.querySelector('#node-context-revision [data-key="namespace"] .nd-v')?.textContent || '').trim() === 'prod');
    assert.equal(await page.locator('#node-effective-vars [data-key="NS_ONLY"] td').nth(1).innerText(), 'prod-visible');
    await noLeaks(); await shot('ns-context-limits-revisions'); await clean('Node config and namespace context');
    check('GET W5 config, actual concurrent PUT → stale409 retains byte-exact draft, cancel/explicit reload/save strict writable body, independent config/applied/ns revisions, local>node>ns sources, per-leaf two-layer limits and machine applied excludes ns');
  }
  if (mode === 'namespace-system') {
    await goto('nodes', 'en', '?namespace=default'); await page.locator('#system-save:not([disabled])').waitFor();
    const get = (await calls()).find(c => c.url === '/v1/system/config' && c.method === 'GET');
    // retention 的**当前值**来自真实 GET（api/events.yaml · getSystemConfig）：mock 种子是
    // 3600/7000/9000，真实服务默认是 604800/2592000/2592000。断言「页面显示的就是服务端
    // 刚给的那三个数」，而不是把某个权威的默认值写死进用例。
    const initial = get.result.retention;
    assert.deepEqual(Object.keys(initial).sort(), ['eventTtlSec','resultTtlSec','taskTtlSec']);
    assert.equal(await page.locator('#result-ttl').inputValue(), String(initial.resultTtlSec));
    assert.equal(await page.locator('#event-ttl').inputValue(), String(initial.eventTtlSec));
    assert.equal(await page.locator('#task-ttl').inputValue(), String(initial.taskTtlSec));
    await page.locator('#result-ttl').fill('0'); await page.locator('#system-save').click(); await waitState('system-state', 'Saved.');
    const zero = (await req('/v1/system/config')).body; assert.equal(zero.retention.resultTtlSec, 0); assert.equal(zero.retention.eventTtlSec, initial.eventTtlSec);
    const systemPut = (await calls()).filter(c => c.method === 'PUT' && c.url === '/v1/system/config').at(-1); assert.deepEqual(Object.keys(systemPut.body).sort(), ['metadata','retention']);
    const remoteEventTtl = initial.eventTtlSec + 1000;
    const remote = await req('/v1/system/config', { metadata: zero.metadata, retention: { ...zero.retention, eventTtlSec: remoteEventTtl } }, 'PUT'); assert.equal(remote.status, 200);
    await page.locator('#event-ttl').fill('42'); await page.locator('#system-save').click(); await waitState('system-state', 'draft retained exactly'); assert.equal(await page.locator('#event-ttl').inputValue(), '42');
    await shot('system0-permanent-stale409-draft'); page.once('dialog', dialog => dialog.accept()); await page.locator('#system-reload').click(); await waitState('system-state', 'Current configuration loaded'); assert.equal(await page.locator('#event-ttl').inputValue(), String(remoteEventTtl));
    const namespacePath = '/v1/namespaces/default/config'; const ns = (await req(namespacePath)).body;
    const nsRemote = await req(namespacePath, { metadata: ns.metadata, vars: { NS_ONLY: 'remote-ns' } }, 'PUT'); assert.equal(nsRemote.status, 200);
    await page.locator('#namespace-vars').fill('{"NS_ONLY":"draft-ns"}'); await page.locator('#namespace-save').click(); await waitState('namespace-state', 'draft retained exactly'); assert.equal(await page.locator('#namespace-vars').inputValue(), '{"NS_ONLY":"draft-ns"}');
    page.once('dialog', dialog => dialog.accept()); await page.locator('#namespace-reload').click(); await waitState('namespace-state', 'Current configuration loaded');
    await page.locator('#namespace-vars').fill('{"NS_ONLY":"ui-ns-saved"}'); await page.locator('#namespace-save').click(); await waitState('namespace-state', 'Saved.');
    const nsPut = (await calls()).filter(c => c.url === namespacePath && c.method === 'PUT').at(-1); assert.deepEqual(Object.keys(nsPut.body).sort(), ['metadata','vars']); assert.notEqual(nsPut.result.namespaceVarsRevision, ns.namespaceVarsRevision);
    const first = (await req(configPath)).body, secondBefore = (await req('/v1/nodes/anode-2/config')).body;
    // "过期 rv" 必须**格式合法**：真实服务对字面量 'stale' 回 400 resource_version_invalid
    // （那不是冲突，是畸形）。所以先成功写一次 anode-2 让 rv 前进，再用刚被淘汰的旧 rv
    // 打批量 —— 这才是 CAS 409。
    const advanced = await req('/v1/nodes/anode-2/config', { metadata: secondBefore.metadata, vars: secondBefore.vars }, 'PUT');
    assert.equal(advanced.status, 200);
    const staleRv = secondBefore.metadata.resourceVersion;
    const secondAfter = (await req('/v1/nodes/anode-2/config')).body;
    assert.notEqual(secondAfter.metadata.resourceVersion, staleRv);
    const batch = await req('/v1/nodes/config/batch', { updates: [{ name: 'anode-1', metadata: first.metadata, vars: { BATCH: 'ok' } }, { name: 'anode-2', metadata: { resourceVersion: staleRv }, vars: { BATCH: 'bad' } }] }, 'POST');
    assert.deepEqual(batch.body.items.map(x => x.ok), [true,false]); assert.equal(batch.body.items[1].error.code, 409); assert.deepEqual((await req('/v1/nodes/anode-2/config')).body, secondAfter);
    for (const [url,field] of [[namespacePath,'vars'],['/v1/system/config','retention']]) assert.equal((await req(url, { [field]: {} }, 'PUT')).status, 400);
    await noLeaks(); await clean('namespace and system configuration');
    check('Actual nondefault GET current retention (not guessed defaults), CAS edit0 permanent, stale409 draft+explicit reload, namespace vars-only CAS/double revision; real batch partial node success, other node unchanged and missing CAS400. No new batch product UI invented.');
  }
  if (mode === 'actions-slots') {
    await goto('node', 'en', '?id=anode-1&namespace=default');
    await page.locator('#node-body:not([hidden])').waitFor();
    if(d.mock){
    // mock 权威的种子事实：4 个 v4 实例（{id,task,busy}），槽位序号就是下标。
    // 0 busy=true → occupied；1 缺 busy → unknown；2 busy=false → idle；3 busy=true → occupied。
    assert.equal(await page.locator('[data-slot="0"]').getAttribute('data-occupancy'), 'occupied');
    assert.equal(await page.locator('[data-slot="1"]').getAttribute('data-occupancy'), 'unknown'); assert.equal(await page.locator('[data-slot="2"]').getAttribute('data-occupancy'), 'idle');
    assert.equal(await page.locator('[data-slot="3"]').getAttribute('data-occupancy'), 'occupied');
    assert.equal(await page.locator('[data-slot="3"]').getAttribute('data-task'), 'reserved-task');
    const task = (await req('/v1/namespaces/default/tasks/tsk_mock_4')).body; assert.equal(task.status.phase, 'failed');
    } else {
    // 真实权威下这个 fixture 节点没有 anode 连上来，`status.instances` 合法为空：
    // 页面必须显示“无槽位”，一条 data-slot 都不许凭空造出来（v4 的槽位身份来自
    // instances 本身，没有 instances 就没有槽位，更不能从 spec.limits 猜一个）。
    const status = (await req(nodePath)).body.status;
    assert.deepEqual(status.instances, [], '真实节点未连上时 instances 必须是空数组');
    assert.equal(await page.locator('#node-slots [data-occupancy]').count(), 0, '空 instances 不得渲染槽位');
    assert.match(await page.locator('#node-slots').innerText(), /Unknown/i);
    }
    if(d.mock){
    await page.evaluate(() => { MockApi.noAgentBinary(); });
    page.once('dialog', dialog => dialog.accept()); await page.locator('#node-upgrade').click(); await waitState('node-action-state', 'agent_binary_unavailable');
    await page.evaluate(() => { MockApi.noAgentBinary(false); MockApi.failUpgrade('checksum_mismatch'); });
    page.once('dialog', dialog => dialog.accept()); await page.locator('#node-upgrade').click(); await waitState('node-action-state', 'checksum_mismatch'); await shot('actual-checksum-upgrade-failure');
    }
    const before = JSON.stringify((await req(nodePath)).body.status.instances);
    page.once('dialog', dialog => dialog.dismiss()); await page.locator('#node-drain').click();
    assert.ok(!(await calls()).some(c => c.url.endsWith('/drain')));
    page.once('dialog', dialog => dialog.accept()); await page.locator('#node-drain').click(); await waitState('node-action-state', 'Hub accepted action');
    assert.equal(JSON.stringify((await req(nodePath)).body.status.instances), before); await noLeaks(); await shot('real-upgrade-failure-and-physical-occupancy');
    if(d.mock){
    await page.evaluate(() => { delete MockApi.state.nodes[0].facts.instances; }); await page.locator('#node-refresh').click();
    await page.waitForFunction(() => document.getElementById('node-slots').textContent.includes('Unknown') && !document.querySelector('#node-slots [data-occupancy]'));
    }
    await shot('missing-instances-unknown'); check('A3 slot0 presence; missing busy/instances are unknown, explicit false idle, terminal Task and pending reservation still occupied; real unavailable/checksum failure, confirmed drain does not reap');
  }
  if (mode === 'retained') {
    if (!d.mock) {
      // devseed 的账号**本来没有密码**：移除密码的按钮在「未设置密码」时是禁用的
      // （security.html 按 GET /v1/auth/password 的 hasPassword 决定）。先走真实的
      // POST /v1/auth/password 设一个密码，再断言按钮可用 —— 不是把"按钮不存在"当通过。
      const before = await d.api('/v1/auth/password');
      assert.equal(before.status, 200);
      if (!before.body.hasPassword) {
        const set = await d.api('/v1/auth/password', { method: 'POST', body: JSON.stringify({ newPassword: '{{SECRET_tnvqj0zz}}' }) });
        assert.equal(set.status, 200);
      }
      assert.equal((await d.api('/v1/auth/password')).body.hasPassword, true);
    }
    await goto('security'); await page.locator('#password-edit-btn:not([disabled])').waitFor();
    assert.ok((await page.locator('#passkey-list').innerText()).includes('No Passkeys'));
    await page.locator('#password-remove-btn').click(); await page.locator('#pwd-current').fill('wrong-current'); await page.locator('#password-modal-submit').click();
    if (d.mock) {
      await page.waitForFunction(() => document.getElementById('password-modal').textContent.includes('Current password is incorrect'));
    } else {
      // 真实的 DELETE /v1/auth/password 带错当前密码必须 400，页面必须把失败显示出来
      // （auth 的失败体是 {error} 而不是 Status.message，页面按 HTTP 码兜底 —— 见报告）。
      await page.waitForFunction(() => {
        const modal = document.getElementById('password-modal');
        return !!modal && /HTTP 400|400/.test(modal.textContent);
      });
      const rejected = (await calls()).findLast(c => c.url === '/v1/auth/password' && c.method === 'DELETE');
      assert.equal(rejected.status, 400);
      assert.equal((await d.api('/v1/auth/password')).body.hasPassword, true, '错误的当前密码不得改动密码');
    }
    await shot('security-current-password-actual400');
    await goto('keys'); await page.locator('#keys-body tr').first().waitFor();
    assert.equal(await page.locator('a[href="cli-auth.html"]').count(), 1); assert.equal(await page.locator('a[href="docs.html#api"]').count(), 1);
    if(d.mock) await page.evaluate(() => window.__f3Reject = '/v1/keys'); else await page.evaluate(() => window.__f3Fault='/v1/keys');
    await page.locator('#btn-create').click(); await page.locator('#new-key-name').fill('retained-form-draft');
    await page.locator('#create-submit').click();
    await page.waitForFunction(message => document.getElementById('create-error').textContent.includes(message), d.mock?'Actual retained rejection sentinel':'F3 deliberate disconnected transport');
    assert.equal(await page.locator('#new-key-name').inputValue(),'retained-form-draft');
    await shot('keys-actual-error-draft-anchors');
    await goto('receivers');
    if (d.mock) {
      await page.locator('#current-device-card button').waitFor();
      assert.equal(await page.evaluate(() => Notification.permission), 'denied');
      assert.equal(await page.locator('#push-count').innerText(), '0'); await page.locator('#current-device-card button').click();
      await page.waitForFunction(() => document.body.textContent.includes(Anotify.t('receivers.local.perm_denied_toast')));
      assert.ok(!(await calls()).some(c => c.method === 'POST' && c.url === '/v1/devices')); await shot('receivers-denied-visible-feedback-zero');
    } else {
      /* 手机推送 UI 在产品里是**隐藏中**（web-src/partials.js·PUSH_UI_ENABLED=false）：
       * 真实现场没有 #current-device-card、也没有 #push-count，receivers 页只在
       * #push-unavailable 里给一句「暂未开放」。这一屏要断言的真实事实是：
       *   ① 未开放分支真的渲染了提示（不是空白）；
       *   ② 浏览器确实没有通知权限（harness 语境）；
       *   ③ 页面**没有**发起任何设备订阅（POST /v1/devices）—— 这才是「不会偷偷注册」。 */
      assert.equal(await page.evaluate(() => window.Anotify.pushUIEnabled), false);
      assert.equal(await page.locator('#current-device-card').count(), 0);
      await page.locator('#push-unavailable').waitFor();
      assert.ok((await page.locator('#push-unavailable').innerText()).trim().length > 0, '未开放分支必须给明确提示');
      assert.equal(await page.evaluate(() => Notification.permission), 'denied');
      await new Promise(resolve => setTimeout(resolve, 300));
      assert.ok(!(await calls()).some(c => c.method === 'POST' && c.url === '/v1/devices'));
      await shot('receivers-push-hidden-no-subscribe');
    }
    await goto('admin');
    if (d.mock) {
      await page.waitForFunction(() => document.getElementById('kpi-devices')?.textContent === '0');
      assert.equal(await page.locator('#kpi-rate').innerText(), '0%'); assert.equal(await page.locator('#kpi-rate-sub').innerText(), '0 / 0');
    } else {
      // 真实零设备 + 真实投递账：速率与分子分母都按 /v1/admin/stats 的同一份事实算。
      const stats = (await calls()).findLast(c => c.url === '/v1/admin/stats')?.response;
      assert.ok(stats, 'admin 页必须真的请求 /v1/admin/stats');
      await page.waitForFunction(() => document.getElementById('kpi-devices')?.textContent === '0');
      const expectedRate = (stats.deliveryTotal > 0 ? Math.round((stats.deliveredCount / stats.deliveryTotal) * 100) : 0) + '%';
      assert.equal(await page.locator('#kpi-rate').innerText(), expectedRate);
      assert.equal(await page.locator('#kpi-rate-sub').innerText(), stats.deliveredCount + ' / ' + stats.deliveryTotal);
      assert.equal(stats.deviceCount, 0);
    }
    await shot('admin-actual-zero-device-delivery');
    if(d.mock) await page.evaluate(() => sessionStorage.setItem('f3-member','1')); else { assert.ok(process.env.E2E_V4_MEMBER_SESSION,'T1 seam: retained member session required, never fake real role'); await d.context.addCookies([{name:'anotify_session',value:process.env.E2E_V4_MEMBER_SESSION,url:d.base}]); }
    await goto('admin');
    assert.equal(await page.locator('#kpi-devices').count(), 0, 'member has no misleading admin KPI shell');
    assert.ok((await page.locator('#page-main').innerText()).includes(await page.evaluate(() => Anotify.t('admin.forbidden.title'))));
    await shot('admin-member-real-permission-anchor');
    if(d.mock) await page.evaluate(() => sessionStorage.removeItem('f3-member')); else await d.context.clearCookies();
    await goto('cli-auth'); assert.equal(await page.locator('#code-input').count(), 1); assert.equal(await page.locator('#entry-continue').count(), 1); assert.equal(await page.locator('#state-confirm').count(), 1);
    await goto('passkey-enroll'); assert.equal(await page.locator('#code-input').count(), 1); assert.equal(await page.locator('#overview-continue').count(), 1); assert.equal(await page.locator('#state-overview').count(), 1);
    // Login uses raw fetch. A declared guest fixture precedes controller boot;
    // rejection uses an actual Status body and never fakes authentication success.
    if(d.mock) await page.evaluate(() => sessionStorage.setItem('f3-guest','1')); else await d.context.clearCookies();
    await goto('login');
    await page.locator('#password-login-toggle').click(); await page.locator('#password-username').fill('retained-user'); await page.locator('#password-password').fill('wrong-password');
    await page.evaluate(() => { window.__f3Guest = true; window.__f3Reject = '/v1/auth/password/login'; });
    await page.locator('#password-login-btn').click();
    if(d.mock) await waitState('status-box', 'Actual retained rejection sentinel');
    else { await page.waitForFunction(()=>__f3Calls.some(c=>c.url==='/v1/auth/password/login'&&c.httpStatus>=400)); const rejected=(await calls()).findLast(c=>c.url==='/v1/auth/password/login'); assert.deepEqual(Object.keys(rejected.result),['error']); await waitState('status-box',rejected.result.error); }
    await shot('login-actual400-status-message');
    check('Retained password/passkey empty anchor + real400; API-key error preserves draft and CLI/docs anchors; receivers denied gives visible feedback/no subscribe; actual zero-device0/0 admin; CLI/passkey-enroll DOM retained; login AUTH1 identity.error is not swallowed');
  }
  if (mode === 'errors') {
    await goto('nodes'); await page.evaluate(() => window.__f3Fault = '/v1/nodes'); await page.locator('#nodes-refresh').click();
    await page.waitForFunction(() => document.getElementById('nodes-list').textContent.includes('F3 deliberate disconnected transport')); await shot('nodes-disconnected');
    await page.evaluate(() => window.__f3Fault = ''); await page.locator('#nodes-list button').click(); await page.locator('[data-node="anode-1"]').waitFor();
    await goto('node','en','?id=anode-1&namespace=default'); await page.evaluate(() => window.__f3Fault = '/v1/nodes'); await page.locator('#node-refresh').click();
    await page.waitForFunction(() => document.getElementById('node-load-state').textContent.includes('F3 deliberate disconnected transport')); await shot('node-disconnected');
    await page.evaluate(() => window.__f3Fault = ''); await page.locator('#node-load-state button').click(); await page.locator('#node-body:not([hidden])').waitFor();
    check('Both Node pages genuine disconnected error + working explicit retry; no automatic mock switch or false empty success');
  }

}
