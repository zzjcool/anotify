// F3b1 standalone registrations. Shared driver is reusable by the four serial F3b2 suites.
import assert from 'node:assert/strict';
import { createHash, createHmac } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { openDriver, PAGES, LANGS, GUARDED, ROOT } from './f3-driver.mjs';
import { capability } from './f3-capability.mjs';
import { source } from './f3-source.mjs';
import { observability } from './f3-observability.mjs';
import { playbook } from './f3-playbook.mjs';
import { nodeRetained } from './f3-node-retained.mjs';
import { bootstrap, aba, matrix, historyMask } from './f3-extra.mjs';
const definitions = [];
const add=(id,run,options={})=>definitions.push({id,run,options});
add('scope-bootstrap',bootstrap,{scope:''});
add('capability-actions',capability,{lang:'zh-CN'});
add('source-actions',source);
for(const mode of ['events','index','trace','message','errors'])add('observe-'+mode,d=>observability(d,mode));
for(const mode of ['upload-edit','install','matrix','delete','errors'])add('playbook-'+mode,d=>playbook(d,mode));
add('playbook-matrix-12',matrix);
add('playbook-secret-history',historyMask);
for(const mode of ['create','config','namespace-system','actions-slots','errors'])add('node-'+mode,d=>nodeRetained(d,mode));
add('node-aba',aba);
add('retained-security-permissions',d=>nodeRetained(d,'retained'));
add('opaque-cas-aba',async d=>{
  await d.goto('capabilities');
  const token='rv1:uid/new+object:9007199254740993123 ?&#%';
  const url=await d.page.evaluate(token=>Anotify.deletePath('/v1/namespaces/default/capabilities/opaque',token),token);
  assert.equal(url,'/v1/namespaces/default/capabilities/opaque?resourceVersion='+encodeURIComponent(token));
  assert.equal(new URL(url,d.base).searchParams.get('resourceVersion'),token);
  const body={apiVersion:'anotify/v1',kind:'Capability',metadata:{name:'f3-cas-recreated'},spec:{next:[],run:{entrypoint:'run.sh',files:{'run.sh':'echo unchanged'}}}};
  const p='/v1/namespaces/default/capabilities';
  const first=await d.response(p,body,'POST');assert.equal(first.status,201);
  const old=first.body.metadata;
  assert.equal((await d.response(p+'/f3-cas-recreated?resourceVersion='+encodeURIComponent(old.resourceVersion),null,'DELETE')).status,200);
  const second=await d.response(p,body,'POST');assert.equal(second.status,201);
  // Finish both independent assertions even if UID is already known wrong.
  const stale=await d.response(p+'/f3-cas-recreated?resourceVersion='+encodeURIComponent(old.resourceVersion),null,'DELETE');
  const kept=await d.response(p+'/f3-cas-recreated');
  assert.notEqual(second.body.metadata.uid,old.uid,'new UID required after same-name recreation');
  assert.equal(stale.status,409);assert.equal(stale.body.kind,'Status');assert.deepEqual(kept.body,second.body);
  d.check('F3-CAS browser actual public POST/DELETE/recreate rejects former identity and opaque URL token roundtrips');
});
add('source-auth-dedup',async d=>{
  await d.goto('sources');const secret='F3_PRIVATE_HMAC_SIGNING_KEY',token='f3-signed-hook-token';
  const p='/v1/namespaces/default/sources';
  const created=await d.response(p,{apiVersion:'anotify/v1',kind:'Source',metadata:{name:'f3-signed-hook'},spec:{kind:'webhook',config:{token,secret,sample:{message:'hint'}},targets:[{capability:'notify',params:{text:'$(payload.message)',message:'$(payload.message)'}}]}},'POST');
  assert.equal(created.status,201);assert.equal(created.body.spec.config.secretSet,true);assert.ok(!JSON.stringify(created.body).includes(secret));
  const payload=' {"id":"f3-delivery","payload":{"message":"real"}} ';
  const send=(raw,signature)=>d.page.evaluate(async([url,raw,signature])=>{const r=await fetch(url,{method:'POST',body:raw,headers:signature?{'X-Anotify-Signature':signature}:{}});return {status:r.status,body:await r.json()};},['/v1/hooks/'+token,raw,signature]);
  const before=await d.snapshot();assert.equal((await send(payload)).status,401);
  assert.equal((await send(payload,'sha256='+'0'.repeat(64))).status,401);assert.equal(await d.snapshot(),before);
  const signed=raw=>'sha256='+createHmac('sha256',secret).update(raw).digest('hex');
  const first=await send(payload,signed(payload));assert.equal(first.status,200);assert.equal(first.body.tasks.length,1);
  const modified=payload.replace('real','changed');const duplicate=await send(modified,signed(modified));
  assert.equal(duplicate.body.duplicate,true);assert.equal(duplicate.body.eventId,first.body.eventId);assert.equal(duplicate.body.traceId,first.body.traceId);
  const task=await d.req('/v1/namespaces/default/tasks/'+first.body.tasks[0].taskId);assert.equal(task.spec.params.message,'real');
  const event=(await d.req('/v1/namespaces/default/events')).items.find(e=>e.metadata.name===first.body.eventId);assert.equal(event.spec.deliveryId,'f3-delivery');
  const rotated=await d.req(p+'/f3-signed-hook',{method:'PATCH',body:JSON.stringify({apiVersion:'anotify/v1',kind:'Source',metadata:{resourceVersion:created.body.metadata.resourceVersion},spec:{config:{token:'f3-rotated-hook'}}})});
  assert.equal(rotated.spec.config.secretSet,true);assert.equal((await send(payload,signed(payload))).status,404);
  assert.ok(!(await d.page.locator('body').innerText()).includes(secret));
  d.check('F3-SRC-AUTH actual HMAC raw bytes Unauthorized zero writes; exact duplicate receipt/task identity and credential rotation invalidates old hook');
});
add('retained-ux21-identity',async d=>{
  await d.goto('index','namespace=default&u=zheng');
  const me=await d.req('/v1/auth/me'),name=me.displayName||me.username;assert.ok(name);
  await d.page.waitForFunction(name=>document.getElementById('greeting')?.textContent.includes(name)&&document.getElementById('sidebar-username')?.textContent.includes(name),name);
  const identity=await d.page.evaluate(()=>({greeting:document.getElementById('greeting').textContent,sidebar:document.getElementById('sidebar-username').textContent.trim()}));
  assert.ok(identity.greeting.includes(identity.sidebar));assert.ok(name.includes('zheng')||!identity.greeting.includes('zheng'));
  d.check('F3-UX21 actual auth/me identity matches greeting/sidebar, query cannot replace user');
});
add('auth-route-guards',async d=>{
  if(d.mock)await d.context.addInitScript(()=>sessionStorage.setItem('f3-guest','1'));else await d.context.clearCookies();
  for(const file of GUARDED){
    await d.page.goto(d.base+'/'+file+'.html',{waitUntil:'domcontentloaded'});
    await d.page.waitForURL('**/login.html**',{waitUntil:'domcontentloaded'});
    await d.page.locator('#password-login-toggle').waitFor();
    assert.ok(d.page.url().includes('/login.html'));
    d.check('F3-AUTH-GUARD '+file+' genuine 401 -> public login');
  }
  await d.page.locator('#password-login-toggle').click();
  await d.page.fill('#password-username','nonexistent-f3');await d.page.fill('#password-password','wrong-f3');
  await d.page.click('#password-login-btn');await d.page.waitForFunction(()=>document.getElementById('status-box').textContent.trim().length>0);
  const rejected=(await d.calls()).findLast(c=>c.url==='/v1/auth/password/login');
  assert.equal(rejected.httpStatus,401); assert.deepEqual(Object.keys(rejected.result),['error']);
  assert.ok((await d.page.locator('#status-box').innerText()).includes(rejected.result.error));
  assert.ok(d.page.url().includes('login.html'));assert.equal(await d.page.inputValue('#password-username'),'nonexistent-f3');
  d.check('F3-AUTH public password reject preserves form, no fabricated authenticated success');
});
// 深链里的对象身份来自当前权威：mock 是固定事实，real 是 bootstrap 用真实 API 造出来的同一角色。
// 名字类（source/playbook）两边一致，只有中枢生成的 id（任务/追踪/节点）需要经 d.fx 取。
const queryFor=(file,d)=>file==='message'?'namespace=default&id='+d.fx('msgTask'):file==='trace'?'namespace=default&traceUID='+d.fx('defaultTrace'):file==='node'?'namespace=default&id='+d.fx('node'):file==='source'?'namespace=default&name='+d.fx('source'):file==='playbook'?'namespace=default&id=f3-page-package':'namespace=default';
const pageAnchors={admin:'#kpi-devices',capabilities:'[data-cap="fixer"]','cli-auth':'#code-input',docs:'#endpoints',events:'#ev-list article',index:'#kpi-total',keys:'#keys-body tr',message:'#task-facts',node:'#node-effective-vars [data-key]',nodes:'[data-node="anode-1"]','passkey-enroll':'#code-input',playbook:'#pb-file-path',playbooks:'#pb-grid [data-slug]',receivers:'#current-device-card',security:'#passkey-list',source:'#src-name',sources:'#src-list > div[data-name]',trace:'#trace-tasks [data-task]'};
async function seedPagePackage(d){
  await d.goto('playbooks');
  const files={'playbook.yaml':JSON.stringify({apiVersion:'anotify/v1',kind:'Playbook',metadata:{name:'f3-page-package',version:1},spec:{values:{type:'object'},capabilities:['cap.yaml'],sources:[]}}),'cap.yaml':JSON.stringify({apiVersion:'anotify/v1',kind:'Capability',metadata:{name:'f3-page-cap'},spec:{next:[],run:{entrypoint:'run.sh',files:{'run.sh':'#!/bin/sh\necho page'}}}})};
  await d.req('/v1/namespaces/default/playbooks',{method:'POST',body:JSON.stringify({apiVersion:'anotify/v1',kind:'Playbook',metadata:{name:'f3-page-package',namespace:'default'},spec:{files}})});
}
for(const lang of LANGS)add('pages-19-'+lang,async d=>{
  await seedPagePackage(d);
  for(const file of PAGES){
    try {
    await d.goto(file,queryFor(file,d),lang);
    if(file==='login') {await d.page.waitForURL('**/index.html**',{waitUntil:'domcontentloaded'});await d.page.locator('#kpi-total').waitFor();d.check('F3-PAGE '+lang+' login authenticated redirect -> index');continue;}
    await d.page.locator(pageAnchors[file]).first().waitFor();
    assert.equal(await d.page.evaluate(()=>document.documentElement.lang),lang);
    const title=(await d.page.title()).trim();assert.ok(title.length>0);
    const text=await d.page.locator('body').innerText();assert.ok(!/\b(?:pb\.v4|nodes\.v4|sources\.v4|caps\.[a-z_])/.test(text),'No bare translation keys');
    const cs=await d.calls();
    if(['source','message','node','playbook'].includes(file))assert.ok(cs.some(c=>c.httpStatus===200&&c.result?.metadata?.name),'Actual detail controller successfully GETs named object');
    if(file==='docs'){await d.page.locator('a[href="#auth"]:visible').first().click();assert.equal(new URL(d.page.url()).hash,'#auth');}
    if(file==='cli-auth'||file==='passkey-enroll'){await d.page.locator('#code-input').fill('TEST-AB');assert.equal(await d.page.inputValue('#code-input'),'TEST-AB');}
    await d.clean('F3-PAGE '+lang+' '+file);
    d.check('F3-PAGE '+lang+' '+file+' nonempty real controller/retained anchor, locale and accessible scroll');
    } catch(error) {
      d.report.errors.push('F3-PAGE '+lang+' '+file+': '+String(error));
      await d.archive();
    }
  }
  assert.deepEqual(d.report.errors,[],'All 19 pages must pass; failed pages never suppress later page assertions');
},{lang});
for(const file of ['capabilities','sources','nodes','message'])add('views-'+file,async d=>{
  for(const size of [{name:'desktop',width:1440,height:900},{name:'mobile',width:390,height:844}])for(const theme of ['dark','light']){
    await d.page.setViewportSize(size);await d.goto(file,queryFor(file,d));await d.page.evaluate(t=>Anotify.theme.set(t),theme);
    await d.page.locator(pageAnchors[file]).first().waitFor();await d.clean(file+' '+size.name+' '+theme);await d.shot(size.name+'-'+theme);
  }
});
add('graph-prod-node-click',async d=>{
  const traceUID=d.fx('prodTrace'),taskId=d.fx('prodTask');
  assert.ok(taskId,'Graph click requires a real prod Task identity (bootstrap fires a prod event, never a fabricated node)');
  await d.goto('trace','namespace=prod&traceUID='+traceUID);
  const node=d.page.locator('#trace-graph [data-node-id="'+taskId+'"]');await node.waitFor();assert.equal(await node.count(),1);
  const id=await node.getAttribute('data-node-id');assert.ok(id);
  await node.click();await d.page.waitForURL('**/message.html?namespace=prod&id='+id,{waitUntil:'domcontentloaded'});
  await d.page.waitForFunction(id=>__f3Calls.some(c=>c.url==='/v1/namespaces/prod/tasks/'+id&&c.result?.metadata?.name===id),id);
  assert.equal(await d.page.locator('#msg-sub').innerText(),'prod · '+id);
  d.check('F3-GRAPH real physical node click -> exact original-namespace Task GET and displayed Task.name');
});
export const CASES=Object.freeze(definitions);
export async function runCase(id){
  const item=CASES.find(c=>c.id===id);assert.ok(item,'Unknown registered F3 case: '+id);
  // G1 is parent-closed: canonical native node selection above still proves the
  // exact Task GET; historical renderer digest is no longer a delivery blocker.
  let d,timer;
  try{
    d=await openDriver(id,item.options);
    timer=setTimeout(()=>{d.report.errors.push('Case exceeded 48s; failure, never timeout-skip');d.stop();},48000);
    await item.run(d);assert.ok(d.report.checks.length>0,'Registered case cannot be no-op');await d.finish();
    return {id,ok:true,checks:d.report.checks.length,report:d.report};
  }catch(error){if(d){d.report.errors.push(error.stack||String(error));try{await d.archive();d.report.diagnostics=await d.page.evaluate(()=>({url:location.href,namespace:document.getElementById('config-namespace')?.value,revisions:document.getElementById('namespace-revisions')?.textContent}));}catch(capture){d.report.errors.push('Failure capture: '+String(capture));}}return {id,ok:false,checks:d?.report.checks.length||0,error:error.stack||String(error),report:d?.report};}
  finally{clearTimeout(timer);if(d)await d.stop();}
}
