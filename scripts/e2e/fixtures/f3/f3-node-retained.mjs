// Attributed F2c2 actual node/config/retained actions. Explicit mock alone permits fixture identity/failure knobs.
import assert from 'node:assert/strict';
import { acceptDialog, acceptOpenDialog, dismissDialog } from './f3-dialogs.mjs';
/* B-1 的编辑面（方向变更 2026-11-19：YAML 是节点页的**唯一**写入口）。
   命名沿用同一组 id：编辑面 `#node-yaml-input`（textarea）+ 保存 `#node-yaml-save`；
   上一版把它收在 `<details id="node-yaml-edit">` 里，所以先把折叠块打开。
   等 `apiVersion:` 出现即视为「YAML 已经由 GET …/yaml 预热」（不等固定时间）。 */
async function openYamlEditor(page) {
  const details = page.locator('#node-yaml-edit');
  if (await details.count()) await details.evaluate(el => { el.open = true; });
  const editor = page.locator('#node-yaml-input');
  assert.equal(await editor.count(), 1, 'YAML 编辑面必须是唯一的 textarea#node-yaml-input（YAML 是唯一写入口）');
  await page.waitForFunction(() => (document.getElementById('node-yaml-input')?.value || '').includes('apiVersion:'), null, { timeout: 15000 });
  return editor;
}
export async function nodeRetained(d,mode){
 const {page,check,shot,clean,base}=d;
 const goto=(file,lang='en',query='?namespace=default')=>d.goto(file,query.replace(/^\?/,''),lang);
 const req=d.response,calls=d.calls;
 const dict=()=>page.evaluate(()=>Object.assign({},window.AnotifyI18n||{}));
 /* 状态文案一律从**运行时字典**取值再断言：页面文案已本地化/改写过一轮，
    钉英文散文（'Saved.' / 'draft retained exactly'）会让「用户看到的话变了」变成假红，
    而这条断言真正要证的是「状态确实推进到了哪一步」。 */
 const waitStateKey=async(id,key)=>{const t=(await dict())[key];assert.equal(typeof t,'string','字典必须有 '+key);await page.waitForFunction(([id,t])=>(document.getElementById(id)?.textContent||'').includes(t),[id,t]);};
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
    // B-6：轮换令牌已从行内按钮移到行尾「⋯」菜单（高破坏动作不再与「进入详情」同权重）。
    // 所以必须先开菜单再点菜单项：`[data-node=…] button` 会同时命中主链与发生器（strict mode）。
    // 结构本身也钉住：菜单默认收起，轮换令牌是菜单里的条目（不是行首的裸按钮）。
    const nodeRow = page.locator('[data-node="browser-new-node"]');
    assert.equal(await nodeRow.locator('.menu').isVisible(), false, '行尾「⋯」菜单默认收起（危险动作不占行首）');
    assert.equal(await nodeRow.locator('.menu [data-act="rotate"]').count(), 1, '轮换令牌在「⋯」菜单里（唯一入口）');
    const openRowMenu = async () => {
      const menu = nodeRow.locator('.menu');
      if (!(await menu.isVisible())) await nodeRow.locator('.act-more').click();
      await menu.waitFor({ state: 'visible' });
    };
    await openRowMenu();
    const rotate = nodeRow.locator('[data-act="rotate"]');
    await rotate.waitFor({ state: 'visible' });
    const before = (await calls()).filter(c => c.url.endsWith('/tokens')).length;
    await dismissDialog(page, () => rotate.click());
    assert.equal((await calls()).filter(c => c.url.endsWith('/tokens')).length, before);
    await openRowMenu();
    await acceptDialog(page, () => rotate.click());
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
    /* B-1（方向变更 2026-11-19）：节点页的**唯一写入口是 YAML**。
       上一版把 YAML 卡降成只读、把写入口放在「配置表单（PUT …/config）」；现在反过来 ——
       同一份 spec 只能有一处能改，而且那一处就是 YAML。本段钉五件事，全部是「结构上有没有」
       而不是「文案对不对」：
         ① `#node-yaml-save` 存在；② 编辑面是可编辑的 textarea（不是只读 code）；
         ③ 保存真的发 `PUT …/yaml`；④ 全生命周期**没有** `PUT …/config`；
         ⑤ 「编辑原始 JSON」（vars / limits 裸 JSON 折叠块）与它的几个控件都不存在。 */
    await goto('node', 'en', '?id=anode-1&namespace=default');
    const yamlInput = await openYamlEditor(page);
    // 只读事实面仍在（它们不是写入口）：授权命名空间、本机 vars、预算两层来源、版本对。
    assert.equal(await page.locator('#node-namespace').count(), 0, '节点页不再有命名空间选择器');
    assert.equal(await page.locator('#node-effective-vars').count(), 0, '节点页不再承载某 ns 的有效值');
    // ⑤ JSON 写面整块退役（不是 disabled、不是藏起来 —— 结构上就不该存在）。
    assert.equal(await page.locator('#node-config-advanced').count(), 0, '「编辑原始 JSON」折叠块必须整块不存在（一个字段只有一个改法）');
    for (const id of ['node-namespaces', 'node-vars', 'node-limits', 'node-config-save']) {
      assert.equal(await page.locator('#' + id).count(), 0, 'JSON 写面已退役，不得再有 #' + id);
    }
    // ①② 唯一写入口真的存在且可编辑
    assert.equal(await page.locator('#node-yaml-save').count(), 1, 'YAML 保存按钮必须存在（唯一写入口 #node-yaml-save）');
    assert.equal(await yamlInput.evaluate(n => n.tagName.toLowerCase()), 'textarea', 'YAML 编辑面必须是真 textarea（不是只读 code）');
    assert.equal(await yamlInput.isEnabled(), true, 'YAML 编辑面必须可用（不是 disabled）');
    const yamlDoc = await yamlInput.inputValue();
    assert.ok(yamlDoc.includes('apiVersion: anotify/v1') && yamlDoc.includes('kind: Node'), 'YAML 编辑面里必须是 Node 文档：\n' + yamlDoc);
    assert.ok(yamlDoc.includes('namespaces:'), 'YAML 里必须有授权命名空间（单一来源）');
    assert.ok(!yamlDoc.includes('status:'), 'YAML 不含派生只读面 status');
    assert.ok((await calls()).some(c => c.method === 'GET' && /\/yaml$/.test(c.url)), '编辑面由 GET …/yaml 预热（不是本地拼的）');
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

    /* ③ 真保存：只改 YAML 里的一个节点默认值 → 点保存 → `PUT …/yaml` 落地，服务端读回生效。
       断言用的是**请求本身**（方法 + 路径 + 状态），不是「按钮被点过」。 */
    const beforeSave = await yamlInput.inputValue();
    const draft = beforeSave.replace(/^(\s+)NODE_ONLY:.*$/m, '$1NODE_ONLY: yaml-only-write');
    assert.notEqual(draft, beforeSave, 'YAML 文档里必须有 NODE_ONLY，否则「编辑真的落地」无从判断：\n' + beforeSave);
    await yamlInput.fill(draft);
    await page.locator('#node-yaml-save').click();
    await page.waitForFunction(() => __f3Calls.some(c => c.method === 'PUT' && /\/yaml$/.test(c.url) && c.httpStatus === 200), null, { timeout: 15000 });
    const yamlPut = (await calls()).filter(c => c.method === 'PUT' && /\/yaml$/.test(c.url)).at(-1);
    assert.equal(yamlPut.url, '/v1/nodes/anode-1/yaml', 'YAML 保存必须打 PUT …/{name}/yaml，实际 ' + yamlPut.method + ' ' + yamlPut.url);
    assert.equal(yamlPut.httpStatus, 200, 'YAML 保存必须 200（实际 ' + yamlPut.httpStatus + ' ' + yamlPut.reason + '）');
    const saved = (await req(configPath)).body;
    assert.equal(saved.vars.NODE_ONLY, 'yaml-only-write', 'YAML 保存必须真的改到节点默认值');
    assert.equal(saved.appliedRevision, config.appliedRevision, '保存只动配置，不动已应用版本');
    assert.notEqual(saved.configRevision, config.configRevision, '配置版本必须递增');
    // 语义断言：保存后 appliedRevision 未变 → UI 必须透出「待应用」状态。
    // 旧版靠英文协议字面量 ConfigPending，本轮该文案已本地化，改用语言中立的 data 钩子。
    // （保存成功后页面自己会 loadFacts + loadConfig，所以这里只等 DOM 收敛，不再点刷新。）
    await page.waitForFunction(() => (document.querySelector('#node-applied [data-apply-state]')?.getAttribute('data-apply-state') || '') === 'pending', null, { timeout: 15000 });
    assert.equal(await page.locator('#node-applied [data-apply-state]').getAttribute('data-apply-state'), 'pending');
    assert.equal(await page.locator('#node-applied [data-key="configRevision"] .nd-v').innerText(), String(saved.configRevision), '版本对里的 configRevision 必须等于服务端新版本');

    /* #454：YAML 编辑面必须保住 `tags`（P1 数据丢失）。`GET …/config` 是 W5 wrapper
       （namespaces/vars/limits + revision 三兄弟）——**不返回 tags**；页面只能从权威读面
       `GET /v1/nodes/{name}` 的 spec.tags 回读现值。把「读面没告诉我」写成 `tags: []`
       会让「打开编辑 → 改点别的 → 保存 YAML」静默清空标签。 */
    let expectedTags;
    if (d.mock) {
      expectedTags = (await req(nodePath)).body.spec.tags; // 预览种子：['build','prod']
    } else {
      const beforeTag = (await req(nodePath)).body;
      // limits 一并给回一份合法值：本用例前面那次 YAML 保存已把 limits 写成合法形态，
      // 但 PATCH 是 merge-patch（spec 内省略的键 = 不动），这里显式给回一份便于断言。
      const tagged = await req(nodePath, { apiVersion: 'anotify/v1', kind: 'Node', metadata: { resourceVersion: beforeTag.metadata.resourceVersion }, spec: { tags: ['gpu', 'prod'], limits: { total: 4, perCapability: { '*': 2 } } } }, 'PATCH');
      assert.equal(tagged.status, 200, 'T1 seam: PATCH spec.tags 必须 200');
      assert.deepEqual(tagged.body.spec.tags, ['gpu', 'prod']);
      // 缺陷根因就是「读面缺这个字段」：钉成事实，而不是靠猜。
      assert.equal('tags' in (await req(configPath)).body, false, '/config（W5 wrapper）不返回 tags');
      // 重新打开页面：tags 的现值只能从 Node 资源读到（页面重新走一次 boot）。
      await goto('node', 'en', '?id=anode-1');
      await openYamlEditor(page);
      expectedTags = ['gpu', 'prod'];
    }
    assert.ok(expectedTags.length >= 1, 'T1 seam: 节点必须带 tags');
    const tagsInDraft = '  tags:\n' + expectedTags.map(tag => '    - ' + tag).join('\n') + '\n';
    const draftYAML = await yamlInput.inputValue();
    assert.ok(draftYAML.includes(tagsInDraft), 'YAML 草稿必须带 tags（把 `|| []` 写回去这条必红）：\n' + draftYAML);
    if (!d.mock) {
      // 保存：写进去的文本里 tags 在 → 服务端读回的 tags 仍在（不回退成空）。
      await page.locator('#node-yaml-save').click();
      await page.waitForFunction(() => __f3Calls.some(c => c.method === 'PUT' && /\/yaml$/.test(c.url) && c.httpStatus === 200), null, { timeout: 15000 });
      assert.deepEqual((await req(nodePath)).body.spec.tags, expectedTags, '保存 YAML 后 tags 必须仍在');
      assert.ok((await yamlInput.inputValue()).includes('    - ' + expectedTags[0]), '保存后的 YAML 编辑面仍含 tags');
    }

    /* ④ + 并发 CAS：测试侧先打一发 `PUT …/config` 把版本抬走（**宿主侧的 d.req 走裸 fetch，
       不进 __f3Calls**，所以下面的「页面没发 PUT /config」断言不会被它污染）；
       页面拿旧版本保存 YAML → 必须 409、草稿逐字保留、服务端 0 写。 */
    const staleBase = (await req(configPath)).body;
    const concurrent = await req(configPath, { metadata: staleBase.metadata, vars: { SOURCE_ROOT: '/node-default', NODE_ONLY: 'concurrent-write' }, limits: staleBase.limits, namespaces: staleBase.namespaces }, 'PUT');
    assert.equal(concurrent.status, 200, 'T1 seam: 并发写必须成功（这样页面手上的版本才是旧的）');
    const conflictDraft = (await yamlInput.inputValue()).replace(/^(\s+)NODE_ONLY:.*$/m, '$1NODE_ONLY: yaml-conflict-draft');
    await yamlInput.fill(conflictDraft);
    await page.locator('#node-yaml-save').click();
    await page.waitForFunction(() => __f3Calls.some(c => c.method === 'PUT' && /\/yaml$/.test(c.url) && c.httpStatus === 409), null, { timeout: 15000 });
    const stalePut = (await calls()).filter(c => c.method === 'PUT' && /\/yaml$/.test(c.url)).at(-1);
    assert.equal(stalePut.httpStatus, 409, '旧版本保存 YAML 必须 409（实际 ' + stalePut.httpStatus + ' ' + stalePut.reason + '）');
    assert.equal(stalePut.reason, 'resource_version_conflict', '409 的 reason 必须是 resource_version_conflict');
    assert.equal(await yamlInput.inputValue(), conflictDraft, '409 后草稿必须逐字保留（不静默覆盖用户的输入）');
    assert.equal((await req(configPath)).body.vars.NODE_ONLY, 'concurrent-write', '409 必须 0 写（服务端仍是并发写进去的值）');
    // 恢复：重开页面拿最新 YAML，再保存一次必须成功（这条证明「409 之后还能改」）。
    await goto('node', 'en', '?id=anode-1');
    await openYamlEditor(page);
    const recoveredDraft = (await yamlInput.inputValue()).replace(/^(\s+)NODE_ONLY:.*$/m, '$1NODE_ONLY: yaml-recovered');
    await yamlInput.fill(recoveredDraft);
    await page.locator('#node-yaml-save').click();
    await page.waitForFunction(() => __f3Calls.some(c => c.method === 'PUT' && /\/yaml$/.test(c.url) && c.httpStatus === 200), null, { timeout: 15000 });
    assert.equal((await req(configPath)).body.vars.NODE_ONLY, 'yaml-recovered', '409 之后重新载入再保存必须成功');
    /* 「按当前配置重写」：把草稿丢掉、回到已加载的配置（不是清空，也不是把手改留下）。
       这条是 B-1 的配套 —— 只有一个写入口时，「草稿怎么退回」必须是可预期的。 */
    await yamlInput.fill('apiVersion: anotify/v1\nkind: Node\nspec:\n  vars:\n    NODE_ONLY: scratch-draft\n');
    await page.locator('#node-yaml-reset').click();
    const resetYaml = await yamlInput.inputValue();
    assert.ok(resetYaml.includes('apiVersion: anotify/v1') && resetYaml.includes('kind: Node'), '重置后必须还是合法 Node 文档：\n' + resetYaml);
    assert.ok(!resetYaml.includes('scratch-draft'), '重置必须丢掉手改草稿（实际：\n' + resetYaml + '）');
    assert.ok(resetYaml.includes('NODE_ONLY: yaml-recovered'), '重置回的是已加载配置（含刚保存的值）：\n' + resetYaml);
    /* ④ 整个页面生命周期里，页面自己一条 `PUT …/config` 都不许发（写入口只有 YAML）。 */
    const pageConfigWrites = (await calls()).filter(c => c.method === 'PUT' && /\/config$/.test(c.url));
    assert.deepEqual(pageConfigWrites.map(c => c.method + ' ' + c.url), [], '节点页不得再对 /config 发 PUT（唯一写入口是 YAML）');
    /* #436：local > node > namespace 的有效值属于**命名空间维度**，已搬到
       namespace.html（选节点，而不是在节点页选 ns）。这里继续断言同一份事实：
       切到 prod 之后 NS_ONLY 显示 prod 的值，来源层仍然逐键由服务端给出。 */
    await goto('namespace', 'en', '?namespace=default&name=default');
    await page.locator('#namespace-vars:not([disabled])').waitFor();
    await page.selectOption('#namespace-node', 'anode-1');
    await page.locator('#namespace-effective-vars [data-key="SOURCE_ROOT"]').waitFor();
    assert.equal(await page.locator('#namespace-effective-vars [data-key="SOURCE_ROOT"] td').nth(1).innerText(), '/local-src');
    assert.equal(await page.locator('#namespace-effective-vars [data-key="SOURCE_ROOT"] td').nth(2).innerText(), 'local');
    assert.equal(await page.locator('#namespace-effective-vars [data-key="NODE_ONLY"] td').nth(2).innerText(), 'node');
    assert.equal(await page.locator('#namespace-effective-vars [data-key="NS_ONLY"] td').nth(2).innerText(), 'namespace');
    await page.selectOption('#config-namespace', 'prod');
    if (await page.locator('#anotify-dialog:not(.hidden)').count()) await acceptOpenDialog(page);
    await page.waitForFunction(() => (document.querySelector('#namespace-context-revision [data-key="namespace"] .nd-v')?.textContent || '').trim() === 'prod');
    await page.locator('#namespace-effective-vars [data-key="NS_ONLY"]').waitFor();
    assert.equal(await page.locator('#namespace-effective-vars [data-key="NS_ONLY"] td').nth(1).innerText(), 'prod-visible');
    await noLeaks(); await shot('ns-context-limits-revisions'); await clean('Node config and namespace context');
    check('YAML is the only write entry: real PUT …/yaml save changes node vars, zero page-side PUT …/config, no raw-JSON editor, concurrent write yields stale409 with byte-exact draft and 0 writes then a successful recovery save, YAML keeps spec.tags when /config omits it (#454), independent config/applied revisions, local>node>ns sources on the namespace page, per-leaf two-layer limits and machine applied excludes ns');
  }
  if (mode === 'namespace-system') {
    // IA（#436）：节点列表页**不再**承载系统保留期与命名空间 vars；两者各自归位。
    await goto('nodes', 'en', '?namespace=default'); await page.locator('[data-node="anode-1"]').waitFor();
    assert.equal(await page.locator('#system-config').count(), 0, '节点页不再有系统保留配置');
    assert.equal(await page.locator('#namespace-vars').count(), 0, '节点页不再有命名空间 vars 配置');
    await goto('admin', 'en', '?namespace=default'); await page.locator('#system-save:not([disabled])').waitFor();
    // ④ 只读 System YAML：与 retention 同一份事实（kind/retention），且没有编辑框。
    await page.waitForFunction(() => (document.getElementById('system-yaml-text')?.textContent || '').includes('kind: SystemConfig'));
    assert.ok((await page.locator('#system-yaml-text').innerText()).includes('retention:'), 'System YAML 有 spec.retention');
    assert.equal(await page.locator('#system-yaml-input').count(), 0, 'System YAML 只读（没有编辑框）');
    const get = (await calls()).find(c => c.url === '/v1/system/config' && c.method === 'GET');
    // retention 的**当前值**来自真实 GET（api/events.yaml · getSystemConfig）：mock 种子是
    // 3600/7000/9000，真实服务默认是 604800/2592000/2592000。断言「页面显示的就是服务端
    // 刚给的那三个数」，而不是把某个权威的默认值写死进用例。
    const initial = get.result.retention;
    assert.deepEqual(Object.keys(initial).sort(), ['eventTtlSec','resultTtlSec','taskTtlSec']);
    assert.equal(await page.locator('#result-ttl').inputValue(), String(initial.resultTtlSec));
    assert.equal(await page.locator('#event-ttl').inputValue(), String(initial.eventTtlSec));
    assert.equal(await page.locator('#task-ttl').inputValue(), String(initial.taskTtlSec));
    const sysPath = get.url; // #647：保留期保存会真删数据 → 先确认再写；取消必须零写入、草稿保留
    const systemPuts = async () => (await calls()).filter(c => c.method === 'PUT' && c.url === sysPath).length;
    await page.locator('#result-ttl').fill('0');
    const putsBeforeCancel = await systemPuts();
    const cancelText = await dismissDialog(page, () => page.locator('#system-save').click());
    assert.ok(cancelText.includes('forever') && cancelText.includes('unchanged'), '确认弹层要逐字段给出影响摘要（改的字段 + 未变的字段）：' + cancelText);
    assert.equal(await systemPuts(), putsBeforeCancel, '取消确认后不得发出任何写入');
    assert.equal(await page.locator('#result-ttl').inputValue(), '0', '取消后草稿原样保留');
    await shot('system0-save-confirm');
    await acceptDialog(page, () => page.locator('#system-save').click());
    await waitStateKey('system-state', 'nodes.v4.saved');
    // 保存会同时刷新只读 YAML（同一份事实的两个视图，不允许其中一个是旧的）。
    await page.waitForFunction(() => (document.getElementById('system-yaml-text')?.textContent || '').includes('resultTtlSec: 0'));
    const zero = (await req('/v1/system/config')).body; assert.equal(zero.retention.resultTtlSec, 0); assert.equal(zero.retention.eventTtlSec, initial.eventTtlSec);
    const systemPut = (await calls()).filter(c => c.method === 'PUT' && c.url === '/v1/system/config').at(-1); assert.deepEqual(Object.keys(systemPut.body).sort(), ['metadata','retention']);
    const remoteEventTtl = initial.eventTtlSec + 1000;
    const remote = await req('/v1/system/config', { metadata: zero.metadata, retention: { ...zero.retention, eventTtlSec: remoteEventTtl } }, 'PUT'); assert.equal(remote.status, 200);
    await page.locator('#event-ttl').fill('42'); await acceptDialog(page, () => page.locator('#system-save').click()); await waitStateKey('system-state', 'nodes.v4.stale'); assert.equal(await page.locator('#event-ttl').inputValue(), '42');
    await shot('system0-permanent-stale409-draft'); await acceptDialog(page, () => page.locator('#system-reload').click()); await waitStateKey('system-state', 'nodes.v4.loaded'); assert.equal(await page.locator('#event-ttl').inputValue(), String(remoteEventTtl));
    const dialogVisible = async () => (await page.locator('#anotify-dialog:not(.hidden)').count()) > 0; // 弹层是单例，只能按 .hidden 判可见
    const wrotePut = before => page.waitForFunction(([p, n]) => __f3Calls.filter(c => c.method === 'PUT' && c.url === p).length > n, [sysPath, before]);
    const putsAfterReload = await systemPuts();
    await page.locator('#task-ttl').fill(String(initial.taskTtlSec + 86400)); // 延长不删数据：不该弹确认；这两次写必须在 reload 之后，否则会把下面要用的 rv 弄过期
    await page.locator('#system-save').click();
    assert.equal(await dialogVisible(), false, '延长保留期不需要确认');
    await wrotePut(putsAfterReload);
    const putsAfterExtend = await systemPuts();
    await page.locator('#system-save').click();
    assert.equal(await dialogVisible(), false, '未改动时保存不需要确认');
    await wrotePut(putsAfterExtend);
    // 顺序很关键：页面**先**加载旧 rv，再做远端写入，页面的草稿才会真的过期
    // （反过来的话页面拿到的是远端写入后的新 rv，保存会成功，"stale 409" 就假绿了）。
    await goto('namespace', 'en', '?namespace=default&name=default'); await page.locator('#namespace-save:not([disabled])').waitFor();
    const namespacePath = '/v1/namespaces/default/config'; const ns = (await req(namespacePath)).body;
    const nsRemote = await req(namespacePath, { metadata: ns.metadata, vars: { NS_ONLY: 'remote-ns' } }, 'PUT'); assert.equal(nsRemote.status, 200);
    await page.locator('#namespace-vars').fill('{"NS_ONLY":"draft-ns"}'); await page.locator('#namespace-save').click(); await waitStateKey('namespace-state', 'nodes.v4.stale'); assert.equal(await page.locator('#namespace-vars').inputValue(), '{"NS_ONLY":"draft-ns"}');
    await acceptDialog(page, () => page.locator('#namespace-reload').click()); await waitStateKey('namespace-state', 'nodes.v4.loaded');
    await page.locator('#namespace-vars').fill('{"NS_ONLY":"ui-ns-saved"}'); await page.locator('#namespace-save').click(); await waitStateKey('namespace-state', 'nodes.v4.saved');
    // #476：命名空间页的保存同样序列化进 YAML → PUT …/yaml（/config 只剩 GET 读投影）。
    const nsYAMLPath = '/v1/namespaces/default/yaml';
    const nsPut = (await calls()).filter(c => c.url === nsYAMLPath && c.method === 'PUT').at(-1);
    assert.deepEqual(Object.keys(nsPut.body), ['yaml']);
    assert.ok(String(nsPut.body.yaml).includes('NS_ONLY: ui-ns-saved'), nsPut.body.yaml);
    assert.equal(nsPut.httpStatus, 200);
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
    check('System retention lives on admin (not the node page) with actual GET + CAS edit0/stale409 draft; namespace vars live on namespace.html with vars-only CAS/double revision; real batch partial node success, other node unchanged and missing CAS400. No new batch product UI invented.');
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
    await acceptDialog(page, () => page.locator('#node-upgrade').click()); await waitState('node-action-state', 'agent_binary_unavailable');
    await page.evaluate(() => { MockApi.noAgentBinary(false); MockApi.failUpgrade('checksum_mismatch'); });
    await acceptDialog(page, () => page.locator('#node-upgrade').click()); await waitState('node-action-state', 'checksum_mismatch'); await shot('actual-checksum-upgrade-failure');
    }
    const before = JSON.stringify((await req(nodePath)).body.status.instances);
    await dismissDialog(page, () => page.locator('#node-drain').click());
    assert.ok(!(await calls()).some(c => c.url.endsWith('/drain')));
    await acceptDialog(page, () => page.locator('#node-drain').click()); await waitState('node-action-state', 'Hub accepted action');
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
      // 真实的 DELETE /v1/auth/password 带错当前密码必须 400；#587 之后页面展示的是
      // 服务端可读文案（认证域回 {error}），不再只剩「HTTP 400」。
      await page.waitForFunction(() => {
        const modal = document.getElementById('password-modal');
        return !!modal && modal.textContent.includes('当前密码不正确');
      });
      const modalText = await page.locator('#password-modal').innerText();
      assert.ok(!/HTTP 400/.test(modalText), "错误弹层不该只剩状态码（#587 已改成展示服务端文案）");
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
    await goto('admin');
    if (d.mock) {
      assert.equal(await page.locator('#kpi-rate').innerText(), '0%'); assert.equal(await page.locator('#kpi-rate-sub').innerText(), '0 / 0');
    } else {
      // 真实投递账：速率与分子分母都按 /v1/admin/stats 的同一份事实算。
      const stats = (await calls()).findLast(c => c.url === '/v1/admin/stats')?.response;
      assert.ok(stats, 'admin 页必须真的请求 /v1/admin/stats');
      const expectedRate = (stats.finishedTasks > 0 ? Math.round((stats.succeededTasks / stats.finishedTasks) * 100) : 0) + '%';
      assert.equal(await page.locator('#kpi-rate').innerText(), expectedRate);
      assert.equal(await page.locator('#kpi-rate-sub').innerText(), stats.succeededTasks + ' / ' + stats.finishedTasks);
    }
    await shot('admin-actual-zero-device-delivery');
    if(d.mock) await page.evaluate(() => sessionStorage.setItem('f3-member','1')); else { assert.ok(process.env.E2E_V4_MEMBER_SESSION,'T1 seam: retained member session required, never fake real role'); await d.context.addCookies([{name:'anotify_session',value:process.env.E2E_V4_MEMBER_SESSION,url:d.base}]); }
    await goto('admin');
    assert.equal(await page.locator('#kpi-messages').count(), 0, 'member has no misleading admin KPI shell');
    assert.ok((await page.locator('#page-main').innerText()).includes(await page.evaluate(() => Anotify.t('admin.forbidden.title'))));
    await shot('admin-member-real-permission-anchor');
    if(d.mock) await page.evaluate(() => sessionStorage.removeItem('f3-member')); else await d.context.clearCookies();
    await goto('cli-auth'); assert.equal(await page.locator('#code-input').count(), 1); assert.equal(await page.locator('#entry-continue').count(), 1); assert.equal(await page.locator('#state-confirm').count(), 1);
    await goto('passkey-enroll'); assert.equal(await page.locator('#code-input').count(), 1); assert.equal(await page.locator('#overview-continue').count(), 1); assert.equal(await page.locator('#state-overview').count(), 1);
    // Login uses raw fetch. A declared guest fixture precedes controller boot;
    // rejection uses an actual Status body and never fakes authentication success.
    if(d.mock) await page.evaluate(() => sessionStorage.setItem('f3-guest','1')); else await d.context.clearCookies();
    await goto('login');
    // #381：登录页在「本浏览器首次来访 + 无会话」时默认落在「注册」tab（首跑闭环）。
    // 这里要测的是密码登录拒绝，所以先显式切回登录 tab —— 顺便把切换本身也验一次。
    await page.locator('#tab-login').click();
    await page.locator('#login-panel:not(.hidden)').waitFor();
    await page.locator('#password-login-toggle').click(); await page.locator('#password-username').fill('retained-user'); await page.locator('#password-password').fill('wrong-password');
    await page.evaluate(() => { window.__f3Guest = true; window.__f3Reject = '/v1/auth/password/login'; });
    await page.locator('#password-login-btn').click();
    if(d.mock) await waitState('status-box', 'Actual retained rejection sentinel');
    else { await page.waitForFunction(()=>__f3Calls.some(c=>c.url==='/v1/auth/password/login'&&c.httpStatus>=400)); const rejected=(await calls()).findLast(c=>c.url==='/v1/auth/password/login'); assert.deepEqual(Object.keys(rejected.result),['error']); await waitState('status-box',rejected.result.error); }
    await shot('login-actual400-status-message');
    check('Retained password/passkey empty anchor + real400; API-key error preserves draft and CLI/docs anchors; actual delivery-rate admin facts; CLI/passkey-enroll DOM retained; login AUTH1 identity.error is not swallowed');
  }
  if (mode === 'namespace-detail') {
    /* IA ③④：列表 → 详情这条路必须真的通（导航入口在共享侧栏 web/partials.js 的 NAV，
       不是每页各写一份）；详情页要有只读 Namespace YAML 与逐节点有效值；节点页不得回潮；
       非 admin 打开管理后台的系统区块必须看到明确的 403，而不是空白页。 */
    await goto('nodes', 'en', '?namespace=default'); await page.locator('[data-node="anode-1"]').waitFor();
    const navLink = page.locator('#sidebar a[href="namespaces.html"]');
    assert.equal(await navLink.count(), 1, '共享侧栏必须有且只有一个「命名空间」入口');
    await navLink.click(); await page.waitForURL(/namespaces\.html/);
    await page.locator('#ns-list [data-ns]').first().waitFor();
    assert.ok((await page.locator('#ns-list [data-ns]').count()) >= 1, '列表至少一行');
    assert.match(await page.locator('#ns-list [data-ns]').first().getAttribute('href'), /^namespace\.html\?name=/);

    // 列表点一行 → 详情页（URL 带 name，页头显示那一个 ns）。
    await page.locator('#ns-list [data-ns="default"]').click();
    await page.waitForURL(/namespace\.html\?name=default/);
    await page.locator('#namespace-vars:not([disabled])').waitFor();
    assert.ok((await page.locator('#ns-name').innerText()).includes('default'), '详情页标题是该命名空间');
    assert.ok((await page.locator('a[href="namespaces.html"]').count()) >= 1, '详情页有返回列表');

    // 逐节点有效值：先确保这个 ns 有一条默认值，再断言「值 + 覆盖来源」都来自服务端。
    const nsCfg = (await req('/v1/namespaces/default/config')).body;
    assert.equal((await req('/v1/namespaces/default/config', { metadata: nsCfg.metadata, vars: { ...nsCfg.vars, F3_NS_DETAIL: 'from-namespace' } }, 'PUT')).status, 200);
    await page.locator('#ns-refresh').click();
    await page.locator('#namespace-nodes [data-node]').first().waitFor();
    const nodeRows = page.locator('#namespace-nodes [data-node]');
    assert.ok((await nodeRows.count()) >= 1, '逐节点表至少一行');
    assert.equal(await page.locator('#namespace-nodes [data-node="anode-1"] td').count(), 2, '逐节点表列 = 节点 / 状态');
    assert.ok((await page.locator('#namespace-nodes [data-node="anode-1"] td:nth-child(2)').innerText()).trim().length > 0, '状态列非空');
    await page.selectOption('#namespace-node', 'anode-1');
    await page.locator('#namespace-effective-vars [data-key="F3_NS_DETAIL"]').waitFor();
    assert.equal(await page.locator('#namespace-effective-vars [data-key="F3_NS_DETAIL"] td').nth(1).innerText(), 'from-namespace');
    assert.equal(await page.locator('#namespace-effective-vars [data-key="F3_NS_DETAIL"] td').nth(2).innerText(), 'namespace', '覆盖来源由服务端给出');
    // 行点击与 select 是同一个选中态（维度反转：在 ns 页选节点）。
    await page.locator('#namespace-nodes [data-node="anode-2"]').click();
    await page.waitForFunction(() => document.getElementById('namespace-node').value === 'anode-2');
    await page.locator('#namespace-nodes [data-node="anode-1"]').click();
    await page.waitForFunction(() => document.getElementById('namespace-node').value === 'anode-1');

    // 只读 Namespace YAML：kind: Namespace + spec.vars，**没有**编辑框。
    await page.waitForFunction(() => (document.getElementById('namespace-yaml-text')?.textContent || '').includes('kind: Namespace'));
    const nsYaml = await page.locator('#namespace-yaml-text').innerText();
    assert.ok(nsYaml.includes('apiVersion: anotify/v1'), 'YAML 有 apiVersion');
    assert.ok(nsYaml.includes('spec:') && nsYaml.includes('vars:'), 'YAML 有 spec.vars');
    assert.ok(nsYaml.includes('F3_NS_DETAIL: from-namespace'), 'YAML 读的是真实 vars');
    assert.ok(!nsYaml.includes('status:'), 'YAML 不含派生只读面 status');
    // A-3 ④：服务端 YAML 的**前导英文注释常量已删**（它曾在产品界面里讲实现）。
    // 正向钉住「文档从 apiVersion 开始」，反向钉住那段英文提示不再回来。
    assert.equal(nsYaml.trim().split('\n')[0].trim(), 'apiVersion: anotify/v1', 'YAML 首行必须是 apiVersion（不再有英文前导注释）');
    assert.ok(!nsYaml.includes('PUT /v1/namespaces/{name}/yaml'), 'YAML 不得再自带写入口说明（实现叙述已删）');
    assert.equal(await page.locator('#namespace-yaml-input').count(), 0, '只读：没有 YAML 编辑框（写入口是 vars 表与原始 JSON，都走 PUT …/yaml）');
    // 机械证据：写入口收下一份合法文档；坏文档（缺 spec.vars / CAS）仍然 400。
    const putYaml = await req('/v1/namespaces/default/yaml', { yaml: 'kind: Namespace' }, 'PUT');
    assert.equal(putYaml.status, 400, '缺 metadata.resourceVersion 的文档必须 400，实际 ' + putYaml.status);

    if (d.mock) {
      /* 空态只在预览里造：真实权威的节点来自 fixtures，删掉会污染同批其它用例。
         断言「没有节点时是明确空态，不是一张空白表」。 */
      const savedNodes = await page.evaluate(() => { const nodes = MockApi.state.nodes; MockApi.state.nodes = []; return JSON.parse(JSON.stringify(nodes)); });
      await page.locator('#ns-refresh').click();
      await page.waitForFunction(() => !document.querySelector('#namespace-nodes [data-node]') && document.getElementById('namespace-nodes').textContent.includes('No nodes yet'));
      assert.equal(await page.locator('#namespace-node').isDisabled(), true, '没有节点时选择器禁用');
      await page.evaluate(nodes => { MockApi.state.nodes = nodes; }, savedNodes);
      await page.locator('#ns-refresh').click();
      await page.locator('#namespace-nodes [data-node]').first().waitFor();
    }

    // 防回潮：节点页 / 节点列表页都没有这些（ns 维度的事实只属于 ns 页）。
    await goto('node', 'en', '?id=anode-1&namespace=default'); await page.locator('#node-yaml-text').waitFor();
    for (const sel of ['#node-effective-vars', '#node-namespace', '#namespace-yaml-text', '#namespace-nodes', '#namespace-effective-vars']) {
      assert.equal(await page.locator(sel).count(), 0, sel + ' 不应出现在节点页');
    }
    await goto('nodes', 'en', '?namespace=default');
    assert.equal(await page.locator('#namespace-nodes').count(), 0, '节点列表页不承载 ns 逐节点表');

    // ④ 权限态：非 admin 打开 admin 的系统区块 → 明确的 403（不是空白页）。
    if (d.mock) await page.evaluate(() => sessionStorage.setItem('f3-member', '1'));
    else {
      assert.ok(process.env.E2E_V4_MEMBER_SESSION, 'T1 seam: real member session required, never fake real role');
      await d.context.addCookies([{ name: 'anotify_session', value: process.env.E2E_V4_MEMBER_SESSION, url: d.base }]);
    }
    await goto('admin');
    await page.locator('#admin-forbidden').waitFor();
    assert.ok((await page.locator('#admin-forbidden').innerText()).includes('403'), '403 提示必须可见');
    assert.equal(await page.locator('#system-save').count(), 0, 'member 看不到可写的系统区块');
    if (!d.mock) {
      // 服务端同一判据：member 的 system 端点回 403 forbidden（不是 200、不是 401）。
      const res = await fetch(d.base + '/v1/system/yaml', { headers: { cookie: 'anotify_session=' + process.env.E2E_V4_MEMBER_SESSION } });
      assert.equal(res.status, 403);
      assert.equal((await res.json()).reason, 'Forbidden');
    }
    await shot('namespace-detail-list-to-yaml-and-member-403');
    await clean('namespace detail: list → detail → yaml / per-node table / member 403');
    check('Sidebar nav (partials.js NAV) → namespaces list → row → namespace.html; Namespace YAML document (kind: Namespace, no second editor - writes go through the vars table → PUT …/yaml); per-node vars table rows + effective values with server-provided source; node pages carry none of it; member sees real 403 (page + /v1/system/yaml).');
  }
  if (mode === 'errors') {
    await goto('nodes'); await page.evaluate(() => window.__f3Fault = '/v1/nodes'); await page.locator('#nodes-refresh').click();
    await page.waitForFunction(() => document.getElementById('nodes-list').textContent.includes('F3 deliberate disconnected transport')); await shot('nodes-disconnected');
    await page.evaluate(() => window.__f3Fault = ''); /* #378：错误态多了「复制详情」，按 data-err-action 点重试。 */ await page.locator('#nodes-list [data-err-action="retry"]').click(); await page.locator('[data-node="anode-1"]').waitFor();
    await goto('node','en','?id=anode-1&namespace=default'); await page.evaluate(() => window.__f3Fault = '/v1/nodes'); await page.locator('#node-refresh').click();
    /* D-3：节点详情的失败态是**本地化**的空态（标题 + 说明 + 重试），不再把英文协议原文
       （`F3 deliberate disconnected transport` 这种）当用户文案贴出来。所以这里断言
       三件事：标题来自字典、正文不出现原始传输错误、正文区不展示半成品。 */
    const i18n = await dict();
    await page.waitForFunction((title) => (document.querySelector('#node-load-state .nd-empty-title')?.textContent || '') === title, i18n['common.request_failed']);
    assert.ok((await page.locator('#node-load-state').innerText()).includes(i18n['nodes.v4.load_failed']), '错误态必须有可读说明（' + i18n['nodes.v4.load_failed'] + '）');
    assert.equal((await page.locator('#node-load-state').innerText()).includes('F3 deliberate disconnected transport'), false, '不得把英文协议原文当用户文案');
    assert.equal(await page.locator('#node-body').isVisible(), false, '失败时不展示半成品正文');
    await shot('node-disconnected');
    await page.evaluate(() => window.__f3Fault = ''); /* 节点详情页的失败态是页面自己的空态骨架（不是 loadErrorNode），所以这里仍是唯一那个重试按钮。 */ await page.locator('#node-load-state button').click(); await page.locator('#node-body:not([hidden])').waitFor();
    check('Both Node pages genuine disconnected error + working explicit retry; no automatic mock switch or false empty success');
  }

}
