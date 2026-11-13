import { scanRawChrome, chromeResidue } from './f3-raw-content.mjs';
// F3b2a: genuine locale browser cases. No legacy harness request adapter.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import { openDriver, ROOT, TEAM, PAGES, LANGS } from './f3-driver.mjs';
export const LOCALE_OUT = path.join(TEAM, 'screenshots/f3locales');
const NS = '/v1/namespaces/default';
// #381：登录页默认 tab 跟实例/浏览器状态走（首跑默认落在「注册」），#login-panel 可能
// 是 hidden。锚点换成两个 tab 都在里面的容器：仍然证「这一页真的渲染了」，不赌默认态。
const anchors = {admin:'#kpi-devices',capabilities:'[data-cap]','cli-auth':'#code-input',docs:'#endpoints',events:'#ev-list article',index:'#kpi-total',keys:'#keys-body tr',login:'.auth-tabs',message:'#task-facts',node:'#node-slots',nodes:'[data-node]','passkey-enroll':'#code-input',playbook:'#pb-file-path',playbooks:'#pb-grid [data-slug]',receivers:'#current-device-card',security:'#passkey-list',source:'#src-name',sources:'#src-list [data-name]',trace:'#trace-tasks [data-task]'};
const pkgFiles = {
  'playbook.yaml': 'apiVersion: anotify/v1\nkind: Playbook\nmetadata:\n  name: f3-locale-package\n  version: 1\nspec:\n  values: { type: object }\n  capabilities:\n    - cap.yaml\n  sources: []\n',
  'cap.yaml': 'metadata:\n  name: f3-locale-cap\nspec:\n  next: [notify, f3-locale-missing]\n  run:\n    entrypoint: run.sh\n    files:\n      run.sh: |\n        #!/bin/sh\n        echo locale\n',
};
export const pageBatches = Object.freeze(Array.from({length:5},(_,i)=>PAGES.slice(i*4,i*4+4)));
// 裸 i18n key 扫描：前缀必须是一个独立 token，绝不能在 `-`、`/`、`.` 之后——否则
// 渲染文本里的真实路径（如 locales 的 `common/login.sh`）会被当成 `login.sh` 裸 key（假阳）。
const bareKeys = /(?<![-/\w.])(?:pb\.v4|nodes\.v4|sources\.v4|events\.v4|message\.v4|caps|receivers|keys|security|login)\.[a-z_]+/;
const zhOnly = ['总览','通知接收','已停用','确定停用','已删除','已吊销','暂无该状态的通知','最近收录'];
export function residue(text, lang) {
  if(lang==='en'||lang==='es') return text.split('\n').filter(x=>/[\u4e00-\u9fff\u3400-\u4dbf]/.test(x));
  if(lang==='ja') return zhOnly.filter(x=>text.includes(x));
  return [];
}
async function t(d,key,values={}) {
  const text = await d.page.evaluate(key=>window.AnotifyI18n?.[key],key);
  assert.equal(typeof text,'string','Generated runtime dictionary entry required: '+key);
  assert.ok(text.trim() && text!==key,'Nonempty translated expectation: '+key);
  return Object.entries(values).reduce((s,[k,v])=>s.replaceAll('{'+k+'}',String(v)),text);
}
async function hasText(d,sel,key,values={}) {
  const expected=await t(d,key,values);
  await d.page.waitForFunction(([sel,expected])=>document.querySelector(sel)?.innerText.includes(expected),[sel,expected]);
  assert.ok((await d.page.locator(sel).first().innerText()).includes(expected),sel+' exact localized '+key);
  d.check(sel+' renders exact '+key+' = '+expected);
}
async function dialogClick(d,button,key,values={}) {
  /* #378：确认句不再走原生 `window.confirm`（Playwright 的 dialog 事件），而是共享弹层
   * `#anotify-dialog`（partials.js）。旧断言“精确等于本地化文案”必须保留强度 ——
   * 所以继续比**正文**（#anotify-dialog-body）逐字相等，而不是“包含”：
   * 弹层标题/附注是共享文案，正文才是这条流程自己的那句。
   * 依然点「取消」：这个 case 的事实是“文案正确 + 取消 ⇒ 零写入”。 */
  const expected=await t(d,key,values);
  await button.click();
  await d.page.locator('#anotify-dialog [data-dialog-ok]').waitFor({state:'visible',timeout:5000});
  const body=(await d.page.locator('#anotify-dialog-body').innerText()).trim();
  assert.equal(body,expected,'Actual confirmation dialog body exact locale');
  assert.equal(await d.page.locator('#anotify-dialog-title').isVisible(),true,'dialog exposes a visible title');
  await d.page.locator('#anotify-dialog [data-dialog-cancel]').click();
  await d.page.locator('#anotify-dialog').waitFor({state:'hidden',timeout:5000});
  d.check('Actual click opens exact localized dialog '+key+' (cancelled, zero write)');
}
// Select detail query identities from the actual v4 wire, NEVER uid/name aliases.
async function fixture(d) {
  await d.goto('sources');
  const lists=await Promise.all([d.req(NS+'/tasks?limit=100'),d.req(NS+'/sources'),d.req('/v1/nodes'),d.req(NS+'/capabilities')]);
  for(let i=0;i<lists.length;i++) {assert.equal(lists[i].kind,['TaskList','SourceList','NodeList','CapabilityList'][i]);assert.ok(lists[i].items.length,'Nonempty locale resource graph required');}
  const [tasks,sources,nodes,caps]=lists.map(x=>x.items);
  for(const task of tasks) assert.ok(task.metadata.name && task.spec.traceUID,'Canonical Task.name / spec.traceUID required');
  assert.ok(sources[0].metadata.name,'Canonical Source.name required');
  assert.ok(nodes[0].metadata.name,'Canonical Node.id (resource metadata.name) required');
  const created=await d.response(NS+'/playbooks',{apiVersion:'anotify/v1',kind:'Playbook',metadata:{name:'f3-locale-package',namespace:'default'},spec:{files:pkgFiles}},'POST');
  assert.equal(created.status,201);assert.equal(created.body.kind,'Playbook');
  assert.equal(created.body.metadata.name,'f3-locale-package','Canonical Playbook.id required');
  const userValues=new Set();
  const strings=value=>{if(typeof value==='string')userValues.add(value);else if(value&&typeof value==='object')Object.values(value).forEach(strings);};
  // Only explicit user-data fields are exempt, never entire chrome DOM containers.
  for(const item of [...tasks,...sources,...nodes,...caps]) {strings(item.metadata.displayName);strings(item.spec?.params);strings(item.spec?.run);strings(item.spec?.config);strings(item.status?.result?.summary);}
  const retainedKeys=await d.req('/v1/keys');for(const key of retainedKeys.keys||[])strings(key.name);
  const me=await d.req('/v1/auth/me');strings(me.displayName);strings(me.username);
  const namespaces=await d.req('/v1/namespaces');for(const n of namespaces.items)strings(n.metadata.displayName);
  return {tasks,sources,nodes,caps,pb:created.body.metadata.name,userValues:[...userValues].filter(Boolean)};
}
const query=(file,f)=>new URLSearchParams({namespace:'default',...(file==='message'?{id:f.tasks[0].metadata.name}:file==='trace'?{traceUID:f.tasks[0].spec.traceUID}:file==='source'?{name:f.sources[0].metadata.name}:file==='node'?{id:f.nodes[0].metadata.name}:file==='playbook'?{id:f.pb}:{})}).toString();
async function scan(d,lang,f) {
  if(d.mock)assert.ok((await d.page.locator('body').innerText()).includes('前端预览 · 数据均为 mock（接口未真正调用）'),'Explicit preview authority banner must remain visible');
  const finite=await d.page.evaluate(scanRawChrome);
  const scanned=finite.text;
  assert.deepEqual(finite.invalid,[],'Raw exemption is only a text-only SPAN, never nested UI widgets');
  assert.ok(scanned.trim().length>30,'Nonempty rendered chrome, never empty scan');
  assert.ok(!bareKeys.test(scanned),'No bare i18n keys in rendered DOM');
  const found=chromeResidue(finite,lang);
  d.report.scans ||= [];d.report.scans.push({lang,url:d.page.url(),characters:scanned.length,rawLeaves:finite.raw.map(x=>({position:x.position,characters:x.text.length})),residue:found});
  assert.deepEqual(found,[],'Visible '+lang+' UI chrome residue; exact text-node positions (no API substring exemption)');
  if(lang==='ja')assert.match(scanned,/[\u3040-\u30ff]/,'Actual Japanese phrasing, not empty/Chinese-only DOM');
  d.check('Nonempty '+lang+' visible DOM/no dotted keys/Chinese chrome residue, finite raw leaves only');
}

async function guest(d,on=true) {
  if(d.mock)await d.page.evaluate(on=>{sessionStorage.setItem('f3-guest',on?'1':'0');},on);
  else if(on)await d.context.clearCookies();
  else throw new Error('Real authenticated locale requires a new H seeded driver, not fabricated cookie');
}
async function pageMatrix(d,lang,batch,coverage) {
  let f,setupError;
  try {f=await fixture(d);}catch(e){setupError=e;}
  const failures=[];
  d.report.pages=[];
  for(const file of batch) {
    const record={page:file,lang,attempted:true,ok:false,states:[]};d.report.pages.push(record);
    try {
      if(!f)throw setupError;
      if(file==='login')await guest(d);
      await d.goto(file,query(file,f),lang);
      await d.page.locator(anchors[file]).first().waitFor();
      const url=new URL(d.page.url());
      assert.equal(url.pathname,(lang==='zh-CN'?'/':'/'+lang+'/')+file+'.html','Guarded page must scan itself, not a 401 login redirect');
      assert.equal(await d.page.evaluate(()=>document.documentElement.lang),lang);
      assert.ok((await d.page.title()).trim());
      if(file!=='login')assert.equal(await d.page.locator('#login-panel').count(),0,'No degenerate guarded login scan');
      const switcher=d.page.locator('#lang-switcher, #lang-switcher-login');
      assert.equal(await switcher.locator('a[hreflang]').count(),4,'Four genuine language links');
      assert.equal(await switcher.locator('a[aria-current="true"]').getAttribute('hreflang'),lang,'Current locale selected');
      const alternates=await d.page.locator('head link[rel="alternate"][hreflang]').evaluateAll(es=>es.map(e=>({lang:e.hreflang,href:e.getAttribute('href')})));
      assert.deepEqual(alternates.map(x=>x.lang).sort(),['en','es','ja','x-default','zh-CN'].sort());
      for(const link of alternates){assert.ok(link.href.startsWith('/')&&!link.href.startsWith('//'),'Relative hreflang only');const r=await d.context.request.get(d.base+link.href);assert.equal(r.status(),200,'hreflang target available');}
      const dict=await d.page.evaluate(()=>({dict:window.AnotifyI18n,scripts:[...document.scripts].map(x=>x.src)}));
      assert.ok(Object.keys(dict.dict||{}).length>100 && dict.scripts.some(x=>x.includes('i18n.'+lang+'.')),'Actual loaded locale dictionary, source or hashed');
      if(file==='docs'){await d.page.locator('a[href="#auth"]:visible').first().click();assert.equal(new URL(d.page.url()).hash,'#auth');record.states.push('docs-anchor-click');}
      if(file==='cli-auth'||file==='passkey-enroll'){await d.page.fill('#code-input','TEST-AB');assert.equal(await d.page.inputValue('#code-input'),'TEST-AB');record.states.push('code-input-draft');}
      if(file==='message'){const request=(await d.calls()).find(x=>x.url===NS+'/tasks/'+f.tasks[0].metadata.name&&x.result?.kind==='Task');assert.ok(request);assert.equal(await d.page.locator('#msg-sub').innerText(),'default · '+request.result.metadata.name);record.states.push('task-name-phase-attempt');}
      if(file==='trace'){assert.ok(await d.page.locator('#trace-graph svg').count());record.states.push('nonempty-trace-resource-graph');}
      if(file==='source'||file==='node'||file==='playbook')assert.ok((await d.calls()).some(c=>c.httpStatus===200&&c.result?.metadata?.name),'Genuine canonical detail GET, not placeholder text');
      await scan(d,lang,f);
      await d.clean(lang+' '+file+' desktop');
      record.states.push(coverage?'localized-live-resource':'AC1-2-3-7');record.ok=true;
      d.check(lang+' '+file+' actual page/locale/anchor/switcher/hreflang/data/scroll');
    } catch(e){record.error=String(e);failures.push(file+': '+String(e));}
    finally{try{await d.archive();}catch(e){failures.push(file+' archive: '+String(e));}}
  }
  assert.deepEqual(failures,[],'All registered pages attempted even after earlier failure');
}
async function navigation(d,lang) {
  const f=await fixture(d);
  await d.goto('keys','namespace=default&localeProof=preserved',lang);
  const target=lang==='ja'?'zh-CN':'ja';
  const switcher=d.page.locator('#lang-switcher');
  await switcher.locator('button').first().click();
  const link=switcher.locator('a[hreflang="'+target+'"]');
  assert.ok((await link.getAttribute('href')).includes('localeProof=preserved'));
  await link.click();await d.page.waitForURL('**/'+(target==='zh-CN'?'':'ja/')+'keys.html**');
  assert.equal(new URL(d.page.url()).searchParams.get('localeProof'),'preserved');
  assert.equal(await d.page.evaluate(()=>document.documentElement.lang),target);
  await d.page.setViewportSize({width:390,height:844});
  await d.page.locator('#menu-btn').click();
  await d.page.locator('#lang-switcher button').first().click();
  const current=d.page.locator('#lang-switcher a[hreflang="'+lang+'"]');await current.waitFor({state:'visible'});await current.click();
  await d.page.waitForFunction(lang=>document.documentElement.lang===lang,lang);
  await d.clean('actual mobile locale switch 390');await scan(d,lang,f);
  d.check('AC3.3/3.4/3.5/3.6 actual desktop+mobile language click and exact query retention');
}
async function authenticatedLogin(d,lang) {
  await d.goto('login','namespace=default',lang);
  await d.page.waitForURL('**/index.html**');await d.page.locator('#kpi-total').waitFor();
  assert.equal(await d.page.evaluate(()=>document.documentElement.lang),lang,'Authenticated redirect retains locale');
  d.check('Retained D2 authenticated login truly redirects to localized index (L1 is NOT waived)');
}
async function retained(d,lang) {
  const f=await fixture(d);await d.goto('keys',query('keys',f),lang);
  const keys=await d.req('/v1/keys');assert.ok(keys.keys?.length,'Nonempty retained keys required, no skip');
  const enabled=keys.keys.find(k=>k.enabled);assert.ok(enabled,'Actual enabled key required');
  const button=d.page.locator('#keys-body tr').filter({hasText:enabled.name}).getByRole('button',{name:await t(d,'keys.action.revoke'),exact:true});
  await dialogClick(d,button,'keys.confirm.revoke',{name:enabled.name});
  assert.ok(!(await d.calls()).some(c=>c.method==='POST'&&c.url.endsWith('/revoke')),'Dismissed confirmation sends zero revoke');
  await d.page.locator('#btn-create').click();await hasText(d,'#kx-title-create','keys.create.title');
  await d.page.locator('#new-key-name').fill('locale-retained-draft');
  await d.page.locator('input[name="key-scope"]:checked').uncheck();
  await d.page.locator('#kxf-generate').click();await hasText(d,'#create-error','keys.error.no_scope');
  assert.equal(await d.page.inputValue('#new-key-name'),'locale-retained-draft');
  d.check('Retained key real validation exact localized error + draft, zero network write');
  // Permission-denied is a real browser property, not a fake successful subscription.
  await d.context.addInitScript(()=>{Object.defineProperty(Notification,'permission',{configurable:true,get:()=> 'denied'});});
  await d.goto('receivers',query('receivers',f),lang);
  await hasText(d,'#current-device-card','receivers.local.disabled');
  if(d.mock)await d.page.evaluate(()=>{
    // Explicit preview-only retained device fixture. Real mode uses the actual
    // seeded subscription and NEVER constructs a mock device or intercepts API.
    MockApi.state.devices=[{id:'f3-locale-device',userId:'usr_mock_demo',name:'Locale Receiver',platform:'desktop',userAgent:'Chrome',endpoint:'https://push.example.invalid/f3-locale-device',enabled:true,eventScope:'all',tags:[],p256dh:'preview-key',auth:'preview-auth',createdAt:123,lastDelivered:123}];
    sessionStorage.setItem('f3-facts',JSON.stringify(MockApi.state));
  });
  await d.goto('receivers',query('receivers',f),lang);
  const devices=await d.req('/v1/devices');assert.ok(devices.devices?.length,'Nonempty retained device fixture, no skip');
  const deviceId=d.mock?'f3-locale-device':devices.devices.find(device=>device.enabled)?.id;
  const device=devices.devices.find(device=>device.id===deviceId);assert.ok(device,'Exact retained device ID required');
  const row=d.page.locator('.rc-dev-row').filter({has:d.page.getByText(device.name,{exact:true})});
  assert.equal(await row.count(),1,'One exact retained device row');await row.waitFor();
  const toggle=row.locator('button[aria-pressed]');const before=await toggle.getAttribute('aria-pressed');assert.equal(before,'true');
  await toggle.click();
  await d.page.waitForFunction(id=>__f3Calls.some(c=>c.method==='PATCH'&&c.url==='/v1/devices/'+encodeURIComponent(id)&&c.body?.enabled===false&&c.httpStatus===200&&c.result?.ok===true&&c.result?.device?.id===id&&c.result.device.enabled===false),deviceId);
  const persisted=(await d.req('/v1/devices')).devices.find(item=>item.id===deviceId);
  assert.ok(persisted,'DEV1 same-ID persisted GET');assert.equal(persisted.enabled,false);assert.equal(persisted.name,device.name);
  assert.equal(await toggle.getAttribute('aria-pressed'),'false','Same retained device successfully paused');
  assert.equal(await toggle.getAttribute('aria-label'),await t(d,'receivers.device.switch_aria'));
  await hasText(d,'.rc-dev-row','receivers.device.'+(before==='true'?'paused':'receiving'));
  await hasText(d,'#current-device-card','receivers.local.not_receiving');
  await d.page.locator('#current-device-card button').click();
  await hasText(d,'body','receivers.local.perm_denied_toast');
  assert.equal((await d.calls()).filter(c=>c.method==='POST'&&c.url==='/v1/devices').length,0,'After fresh navigation, denied browser permission never subscribes');
  d.check('Retained JA device status actual click/PATCH plus denied exact locale feedback/no subscribe');
  await scan(d,lang,f);
}
async function resourceStates(d,lang) {
  const f=await fixture(d);
  await d.goto('capabilities',query('capabilities',f),lang);
  const cap=d.page.locator('[data-cap]').first();await cap.getByRole('button',{name:await t(d,'caps.detail.btn'),exact:true}).click();
  await hasText(d,'[data-cap]','caps.cond.title');await hasText(d,'[data-cap]','caps.nodes.title');
  assert.ok((await d.calls()).some(c=>c.result?.kind==='CapabilityList'&&c.result.items.length));
  await d.goto('node',query('node',f),lang);
  assert.ok(await d.page.locator('#node-slots [data-slot="0"]').count(),'Physical slot zero is valid');
  await hasText(d,'#node-slots [data-occupancy="occupied"]','nodes.v4.occupied');
  await hasText(d,'#node-slots [data-occupancy="unknown"]','nodes.v4.unknown');
  d.check('Nonempty Capability conditions/deployment state; actual node unknown != idle and busy/slot0 labels');
  await scan(d,lang,f);
  const prior=await d.response(NS+'/capabilities',{apiVersion:'anotify/v1',kind:'Capability',metadata:{name:'f3-locale-cap'},spec:{next:[],run:{entrypoint:'run.sh',files:{'run.sh':'#!/bin/sh\necho locale\n'}}}},'POST');assert.equal(prior.status,201);
  await d.goto('playbook','namespace=default&id='+encodeURIComponent(f.pb),lang);
  await d.page.locator('#btn-install').click();await d.page.waitForFunction(()=>__f3Calls.some(c=>c.url.endsWith('/install')&&c.httpStatus===200));
  await d.page.locator('#btn-rescan').click();
  await hasText(d,'#pb-unapplied','pb.v4.unapplied');
  assert.ok((await d.page.locator('#pb-unapplied').innerText()).includes('f3-locale-missing'),'Unapplied next is genuine resource graph, not static heading');
  assert.ok((await d.calls()).some(c=>c.url.endsWith('/install')&&c.result?.install?.id&&c.result?.skipped?.capabilities?.length),'Installed reused resource receipt nonempty');
  await scan(d,lang,f);d.check('Actual package install receipt and unapplied next graph exact locale');
}
async function taskStates(d,lang) {
  const f=await fixture(d),waiting=f.tasks.find(x=>x.status.waitingReason==='retry_backoff'&&x.status.attempts?.length),expired=f.tasks.find(x=>x.status.result?.artifacts?.length&&x.status.phase==='failed');
  assert.ok(waiting,'Nonempty real waiting Task fixture required');
  await d.goto('message','namespace=default&id='+encodeURIComponent(waiting.metadata.name),lang);
  await hasText(d,'#task-phase','message.v4.phase.'+waiting.status.phase);
  assert.ok((await d.page.locator('#task-facts').innerText()).includes('waitingReason'));
  await d.page.locator('#logs-previous').selectOption('true');await d.page.locator('#task-logs button').click();
  await d.page.waitForFunction(()=>__f3Calls.some(c=>c.url.includes('/logs?')&&c.url.includes('previous=true')&&c.result?.lines?.length));
  assert.ok((await d.calls()).some(c=>c.url.includes('/logs?')&&c.url.includes('previous=true')&&c.httpStatus===200&&c.result?.lines?.length),'Actual previous attempt logs nonempty');
  await hasText(d,'#task-logs','message.v4.truncated');
  await dialogClick(d,d.page.locator('#task-cancel'),'message.v4.confirm_cancel');
  await scan(d,lang,f);
  assert.ok(expired,'Expired artifact task with retained manifest required');
  await d.goto('message','namespace=default&id='+encodeURIComponent(expired.metadata.name),lang);
  await d.page.locator('#task-result a').first().click();
  await hasText(d,'#task-result','message.v4.artifact_expired');
  assert.ok((await d.calls()).some(c=>c.httpStatus===410&&c.reason==='artifact_expired'),'Actual authorized artifact410 wire');
  assert.ok((await d.page.locator('#task-result').innerText()).includes(expired.status.result.artifacts[0].sha256),'Manifest hash retained after expiry');
  await scan(d,lang,f);d.check('Actual waiting phase/previous logs/truncation/dialog and expired artifact410 exact locale');
}
async function conflict(d,lang) {
  const f=await fixture(d);await d.goto('node',query('node',f),lang);
  const p='/v1/nodes/'+encodeURIComponent(f.nodes[0].metadata.name)+'/config';
  const current=await d.req(p);assert.ok(current.metadata.resourceVersion);
  // #375：JSON textarea 默认收在「编辑原始 JSON」折叠里；fill 前先展开。
  await d.page.locator('#node-config-advanced').evaluate(el=>{el.open=true;});
  await d.page.locator('#node-vars').fill('{"LOCALE_DRAFT":"retained"}');
  const fresh=await d.response(p,{metadata:{resourceVersion:current.metadata.resourceVersion},config:current.config},'PUT');assert.equal(fresh.status,200);
  await d.page.locator('#node-config-save').click();await hasText(d,'#node-config-state','nodes.v4.stale');
  assert.equal(await d.page.locator('#node-vars').inputValue(),'{"LOCALE_DRAFT":"retained"}');
  assert.ok((await d.calls()).some(c=>c.url===p&&c.method==='PUT'&&c.httpStatus===409&&c.reason==='resource_version_conflict'));
  await dialogClick(d,d.page.locator('#node-config-reload'),'nodes.v4.reload_confirm');
  assert.equal(await d.page.locator('#node-vars').inputValue(),'{"LOCALE_DRAFT":"retained"}');
  await scan(d,lang,f);d.check('Real concurrent CAS409 exact localized stale error/unmodified draft/explicit reload dialog');
}
async function readme(d) {
  const [zh,en]=await Promise.all(['README.md','README.en.md'].map(x=>readFile(path.join(ROOT,x),'utf8')));
  assert.ok(zh.includes('README.en.md')&&en.includes('README.md'),'Retained README crosslinks');
  assert.equal((en.match(/^## /gm)||[]).length,(zh.match(/^## /gm)||[]).length);
  for(const m of en.matchAll(/\]\(([^)]+\.md)\)/g))await readFile(path.join(ROOT,m[1]));
  await d.goto('login');assert.equal(await d.page.locator('[data-demo], #demo-btn, #try-demo').count(),0,'Old demo entry retired, not restored');
  d.check('AC5 README parity/relative links, AC4 old demo entry remains retired');
}
export function casesFor(suite) {
  assert.ok(['i18n','i18n_coverage'].includes(suite));
  const items=[];
  for(const lang of LANGS){
    pageBatches.forEach((pages,index)=>items.push({id:'pages-'+lang+'-'+(index+1),lang,pages,run:d=>pageMatrix(d,lang,pages,suite==='i18n_coverage')}));
    items.push({id:'authenticated-login-'+lang,lang,run:d=>authenticatedLogin(d,lang)});
    if(suite==='i18n')items.push({id:'navigation-'+lang,lang,run:d=>navigation(d,lang)});
    else for(const [name,run] of [['retained',retained],['resource-states',resourceStates],['task-states',taskStates],['conflict-draft',conflict]])items.push({id:name+'-'+lang,lang,run:d=>run(d,lang)});
  }
  if(suite==='i18n')items.push({id:'readme-demo-retirement',lang:'en',run:readme});
  return Object.freeze(items);
}
export async function runLocaleCase(suite,id) {
  const item=casesFor(suite).find(c=>c.id===id);assert.ok(item,'Unknown registered locale case: '+id);
  let d,timer;
  try {
    // node-* initializes the shared explicit fixture's real unknown/busy/slot0 facts.
    d=await openDriver('node-'+suite+'-'+id,{lang:item.lang});
    d.page.setDefaultTimeout(2200);d.page.setDefaultNavigationTimeout(5000);
    d.report.suite=suite;d.report.localeCase=id;d.report.lang=item.lang;
    timer=setTimeout(()=>{d.report.errors.push('48s case deadline, failure not skip');void d.stop();},48000);
    await item.run(d);assert.ok(d.report.checks.length,'No empty locale case');await d.finish();
    return {id,ok:true,checks:d.report.checks.length,report:d.report};
  }catch(error){
    if(d){
      d.report.errors.push(error.stack||String(error));
      try{await d.archive();d.report.diagnostics=await d.page.evaluate(()=>({url:location.href,states:['pb-unapplied','pb-badge','pb-receipt','node-config-state','current-device-card'].map(id=>({id,text:document.getElementById(id)?.innerText})),deviceRows:[...document.querySelectorAll('.rc-dev-row')].map(e=>({text:e.innerText,enabled:e.querySelector('[aria-pressed]')?.getAttribute('aria-pressed')}))}));}
      catch(capture){d.report.errors.push('Failure capture: '+String(capture));}
    }
    return {id,ok:false,checks:d?.report.checks.length||0,error:error.stack||String(error),report:d?.report};
  }
  finally{clearTimeout(timer);if(d)await d.stop();}
}
