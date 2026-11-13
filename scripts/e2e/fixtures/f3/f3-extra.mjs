// New F3 registered behavior: bootstrap, node ABA, full install matrix and historical recursive masking.
import assert from 'node:assert/strict';
import { acceptOpenDialog } from './f3-dialogs.mjs';
export async function bootstrap(d) {
  const {page,goto,calls,req,check}=d;
  await goto('sources','namespace=prod');
  await page.locator('#src-list > div[data-name]').first().waitFor();
  // #377 起列表请求带分页参数（?limit=20）；匹配时只剥查询串，命名空间仍逐字比对。
  const pathOf=u=>String(u).split('?')[0];
  const list=(await calls()).find(c=>pathOf(c.url)==='/v1/namespaces/prod/sources');
  assert.equal(list.result.kind,'SourceList');assert.ok(Array.isArray(list.result.items));
  assert.ok(!(await calls()).some(c=>pathOf(c.url)==='/v1/namespaces/default/sources'));
  assert.equal(await page.evaluate(()=>Anotify.readScope().namespace),'prod');
  assert.ok((await page.evaluate(()=>localStorage.getItem('anotify.scope'))).includes('prod'));
  check('F3-SCOPE-01 empty-storage prod deep link awaits scope and exact SourceList.items');
  const before=await req('/v1/namespaces/prod/sources');
  await page.locator('.scope-trigger').click();await page.locator('[data-ns-id="default"]').click();
  await page.waitForURL('**namespace=default');
  await page.locator('#src-list > div[data-name="deploy-hook"]').waitFor();
  assert.equal(await page.evaluate(()=>Anotify.readScope().namespace),'default');
  assert.equal((await req('/v1/namespaces/prod/sources')).metadata?.resourceVersion,before.metadata?.resourceVersion);
  check('F3-SCOPE-02 real switch synchronizes URL, reloads exact namespace without mutating prod');
  await page.evaluate(()=>localStorage.removeItem('anotify.scope'));
  await goto('capabilities','');await page.locator('[data-cap="fixer"]').waitFor();
  const rows=await calls();
  assert.ok(rows.some(c=>c.url==='/v1/namespaces'));
  assert.ok(rows.some(c=>c.url==='/v1/namespaces/default/capabilities'&&c.result.kind==='CapabilityList'));
  check('F3-SCOPE-03 first visit NamespaceList -> default CapabilityList, no flat compatibility');
}
export async function aba(d) {
  const {page,goto,check,calls,req}=d;
  await goto('nodes','namespace=default');
  await page.locator('#namespace-save:not([disabled])').waitFor();
  const original=await req('/v1/namespaces/prod/config');
  await page.evaluate(()=>{
    const raw=window.fetch;
    window.fetch=async(url,opts={})=>{
      const response=await raw(url,opts);
      if(url==='/v1/namespaces/default/config'&&opts.method==='PUT')return new Promise(resolve=>{window.__f3Release=()=>resolve(response);});
      return response;
    };
  });
  /* 切换 namespace 时，草稿 ≠ 已加载内容会弹「重载会替换当前草稿」（#378 的真实弹层）——
     旧代码用全局 dialog 事件自动确认，这里改成显式点那个真实的确认按钮。
     注意：同样的草稿切回去不会弹（草稿 === 已加载内容），所以只在弹层真的开着时才点，
     否则 acceptOpenDialog 会空等到 5s 超时。 */
  const acceptReloadIfOpen = async () => {
    if (await page.locator('#anotify-dialog:not(.hidden)').count()) await acceptOpenDialog(page);
  };
  await page.fill('#namespace-vars','{"OLD_SAVE":"old-submission"}');await page.click('#namespace-save');
  await page.waitForFunction(()=>!!window.__f3Release);
  await page.selectOption('#config-namespace','prod');await acceptReloadIfOpen();await page.waitForFunction(()=>document.getElementById('namespace-vars').value.includes('prod-visible'));
  await page.selectOption('#config-namespace','default');await acceptReloadIfOpen();await page.waitForFunction(()=>document.getElementById('namespace-vars').value.includes('OLD_SAVE'));
  await page.fill('#namespace-vars','{"NEW_DRAFT":"must-survive-old-response"}');
  const snapshot=()=>page.evaluate(()=>({draft:document.getElementById('namespace-vars').value,rv:document.getElementById('namespace-revisions').textContent,state:document.getElementById('namespace-state').textContent,disabled:document.getElementById('namespace-save').disabled,count:__f3Calls.length}));
  const before=await snapshot();await page.evaluate(()=>window.__f3Release());
  // Event-loop barrier after the submitted fetch continuation; no fake callback/controller.
  await page.evaluate(()=>new Promise(resolve=>setTimeout(resolve,50)));
  assert.deepEqual(await snapshot(),before);
  assert.deepEqual(await req('/v1/namespaces/prod/config'),original);
  assert.deepEqual((await req('/v1/namespaces/default/config')).vars,{OLD_SAVE:'old-submission'});
  assert.equal((await calls()).filter(c=>c.method==='PUT').length,1);
  check('F3-NODE-ABA actual delayed PUT after A/B/A keeps byte-exact new draft, RV/status/finally; no crossscope write or auto retry');
}
const matrixFile=(name,{version,body='echo v1',next=[],source=false,kind='manual',target='fixer',param='one'}={})=>{
  const root={apiVersion:'anotify/v1',kind:'Playbook',metadata:{name,version:1},spec:{values:{type:'object'},capabilities:source?[]:['cap.yaml'],sources:source?['source.yaml']:[]}};
  // 渲染后的能力/源文档必须是**契约合法**的资源文档（apiVersion+kind+metadata+spec）：
  // 真服务端在 install 时会重新校验渲染结果，缺 apiVersion/kind 一律 400 invalid_playbook。
  return {'playbook.yaml':JSON.stringify(root),...(source?{'source.yaml':JSON.stringify({apiVersion:'anotify/v1',kind:'Source',metadata:{name:name+'-source'},spec:{kind,config:kind==='manual'?{payload:{fixed:true}}:{},targets:[{capability:target,params:{value:param}}]}})}:{'cap.yaml':JSON.stringify({apiVersion:'anotify/v1',kind:'Capability',metadata:{name:name+'-cap',...(version==null?{}:{version})},spec:{next,run:{entrypoint:'run.sh',files:{'run.sh':'#!/bin/sh\n'+body}}}})})};
};
export async function matrix(d) {
  const {page,goto,req,response,calls,check}=d;
  await goto('playbooks');
  for(let cell=1;cell<=12;cell++) {
    const name='f3-matrix-'+cell,source=cell>=8;
    const opts={source,...([4,5,6].includes(cell)?{version:4}:{})};
    let files=matrixFile(name,opts);
    const base='/v1/namespaces/default/playbooks/'+name;
    const pb=await req('/v1/namespaces/default/playbooks',{method:'POST',body:JSON.stringify({apiVersion:'anotify/v1',kind:'Playbook',metadata:{name,namespace:'default'},spec:{files}})});
    if([2,3,5,6,7,9,10,12].includes(cell)) {
      await req(base+'/install',{method:'POST',body:'{"values":{}}'});
      if(cell===3||cell===6)opts.body='echo different-body';
      if(cell===7)opts.next=['notify'];
      if(cell===10)opts.kind='webhook';
      if(cell===12)opts.param='changed-routing-not-applied';
      files=matrixFile(name,opts);
      const cur=await req(base+'?includeBody=true');
      await req(base,{method:'PUT',body:JSON.stringify({apiVersion:'anotify/v1',kind:'Playbook',metadata:{name,namespace:'default',resourceVersion:cur.metadata.resourceVersion},spec:{files}})});
    }
    if(cell===11){files=matrixFile(name,{source:true,target:'f3-not-present'});const cur=await req(base+'?includeBody=true');await req(base,{method:'PUT',body:JSON.stringify({apiVersion:'anotify/v1',kind:'Playbook',metadata:{name,namespace:'default',resourceVersion:cur.metadata.resourceVersion},spec:{files}})});}
    assert.equal(pb.kind,'Playbook');
    await goto('playbook','namespace=default&id='+name);await page.locator('#btn-install:not([disabled])').waitFor();
    const before=await d.snapshot();
    await page.click('#btn-dry-run');await page.waitForFunction(()=>!document.getElementById('btn-dry-run').disabled);
    assert.equal(await d.snapshot(),before,'matrix '+cell+' dryRun zero resources/receipts');
    const dry=(await calls()).findLast(c=>c.url.endsWith('/install'));
    assert.ok(dry,'Actual dryRun request');
    let old=null;
    if(source){const prior=await response('/v1/namespaces/default/sources/'+name+'-source');assert.ok([200,404].includes(prior.status));if(prior.status===200)old=prior.body;}
    await page.click('#btn-install');await page.waitForFunction(()=>!document.getElementById('btn-install').disabled);
    const call=(await calls()).findLast(c=>c.url.endsWith('/install')),r=call.result;
    if([6,10].includes(cell)) {
      assert.equal(call.httpStatus,400);assert.ok(r.details.conflicts.length);
      assert.equal(await d.snapshot(),before,'atomic conflict zero resources/receipts');
      // 真实错误体：reason 是**顶层**封闭码（Status.reason），details 里只有冲突明细
      // （field/declared/actual）。mock 把原因码塞进 details 里，真实现场要按顶层读。
      if(cell===6)assert.ok(String(r.reason).includes('version_content_immutable'));
      else assert.equal(r.reason,'playbook_conflict');
    } else {
      assert.equal(call.httpStatus,200,'matrix '+cell+' actual Status '+JSON.stringify(r));assert.ok(r.install.id);
      const bucket=[2,5,9].includes(cell)?'skipped':'created';
      if(![7,12].includes(cell))assert.ok(r[bucket][source?'sources':'capabilities'].length);
      if(cell===3){assert.ok(r.upgraded.length);assert.ok(r.created.capabilities.includes(name+'-cap@2'));}
      if(cell===7||cell===12){const u=r.unapplied.find(u=>u.reason===(cell===7?'routing_unapplied':'targets_unapplied'));assert.ok(u);assert.notDeepEqual(u.declared,u.actual);assert.match(await page.locator('#pb-unapplied').innerText(),/NOT ready/);}
      if(cell===11){
        // 「引用不存在」的事实由回执的 pending[] 说出来；源读模型的 targetsResolved
        // 在真实实现里是「**声明了**至少一个出口」（resource_source.go·srcEnvelopeOf），
        // 不是「引用能解析到库里已有的能力」。所以这里断言三件事：pending 有它、
        // 源仍然保留这条未解析的 target、targetsResolved 因此为 true/health=ok。
        assert.ok(r.pending.includes('f3-not-present'));
        const detail=await req('/v1/namespaces/default/sources/'+name+'-source');
        assert.equal(detail.status.targetsResolved,true);
        assert.equal(detail.status.health,'ok');
        assert.equal(detail.spec.targets.length,1);
        assert.equal(detail.spec.targets[0].capability,'f3-not-present');
        assert.ok(detail.spec.targets[0].params && typeof detail.spec.targets[0].params==='object');
      }
      if(old&&[9,12].includes(cell))assert.deepEqual((await req('/v1/namespaces/default/sources/'+name+'-source')).spec.targets,old.spec.targets);
      if(cell===7)assert.deepEqual((await req('/v1/namespaces/default/capabilities/'+name+'-cap')).spec.next,[]);
    }
    check('F3-PB-MATRIX-'+String(cell).padStart(2,'0')+' actual dryRun/install click, exact action/conflict/unapplied and persistence');
  }
}
export async function historyMask(d) {
  const {page,goto,req,calls,check}=d;
  const name='f3-secret-history';
  const files=Object.fromEntries(Object.entries({
    'playbook.yaml':{apiVersion:'anotify/v1',kind:'Playbook',metadata:{name,version:1},spec:{values:{type:'object',properties:{blob:{type:'object',writeOnly:true},list:{type:'array',writeOnly:true},pin:{type:'integer',writeOnly:true},flag:{type:'boolean',writeOnly:true}}},capabilities:['cap.yaml'],sources:['source.yaml']}},
    'cap.yaml':{apiVersion:'anotify/v1',kind:'Capability',metadata:{name:name+'-cap'},spec:{next:[],run:{entrypoint:'run.sh',files:{'run.sh':'#!/bin/sh\necho unchanged'}}}},
    'source.yaml':{apiVersion:'anotify/v1',kind:'Source',metadata:{name:name+'-source'},spec:{kind:'webhook',config:{},targets:[{capability:name+'-cap',params:{blob:'{{values.blob}}',list:'{{values.list}}',pin:'{{values.pin}}',flag:'{{values.flag}}',mixed:'secret={{values.pin}}',fixedNumber:77,fixedBoolean:true}}]}},
  }).map(([p,v])=>[p,JSON.stringify(v)]));
  await goto('playbooks');await req('/v1/namespaces/prod/playbooks',{method:'POST',body:JSON.stringify({apiVersion:'anotify/v1',kind:'Playbook',metadata:{name,namespace:'prod'},spec:{files}})});
  await goto('playbook','namespace=prod&id='+name);
  await page.locator('details:has(#pb-values-json) > summary').click();
  const receipts=[];
  for(const [i,values] of [{pin:77,flag:true,blob:{inner:'F3_PRIVATE_OLD_OBJECT'},list:['F3_PRIVATE_OLD_LIST']},{pin:88,flag:false,blob:{inner:'F3_PRIVATE_NEW_OBJECT'},list:['F3_PRIVATE_NEW_LIST']}].entries()) {
    await page.fill('#pb-values-json',JSON.stringify(values));await page.click('#pb-use-json');
    await page.click('#btn-render');await page.waitForFunction(()=>!document.getElementById('btn-render').disabled);
    const render=(await calls()).findLast(c=>c.url.endsWith('/render')).result;
    // /render 的响应是**平铺**的 RenderResult（{values,valuesDigest,files,capabilities,sources}），
    // 没有 mock 私有的一层 `documents` 包装。
    assert.equal(render.sources[0].spec.targets[0].params.pin,values.pin);
    await page.click('#btn-install');await page.waitForFunction(()=>!document.getElementById('btn-install').disabled);
    const r=(await calls()).findLast(c=>c.url.endsWith('/install')).result;
    assert.equal(r.valuesDigest,render.valuesDigest);receipts.push(r);
    if(i){const u=r.unapplied.find(u=>u.field==='targets');assert.ok(u);for(const side of ['declared','actual']){for(const k of ['blob','list','pin','flag','mixed'])assert.equal(u[side][0].params[k],'***');assert.equal(u[side][0].params.fixedNumber,77);assert.equal(u[side][0].params.fixedBoolean,true);}}
  }
  const history=[];for(const r of receipts)history.push(await req('/v1/namespaces/prod/playbooks/'+name+'/installs/'+r.install.id));
  const groups=await req('/v1/namespaces/prod/playbooks/'+name+'/installs');
  const publicText=await page.locator('#pb-receipt').innerText()+await page.locator('#pb-unapplied').innerText();
  for(const secret of ['F3_PRIVATE_OLD_OBJECT','F3_PRIVATE_NEW_OBJECT','F3_PRIVATE_OLD_LIST','F3_PRIVATE_NEW_LIST'])assert.ok(!JSON.stringify([receipts,history,groups,publicText]).includes(secret));
  assert.equal((await req('/v1/namespaces/prod/sources/'+name+'-source')).spec.targets[0].params.pin,77);
  const links=await page.locator('#pb-items a').evaluateAll(as=>as.map(a=>a.href));assert.ok(links.some(u=>u.includes('namespace=prod')&&u.includes('name='+name+'-source')));
  check('F3-PB-HISTORY two actual render/install clicks, old actual/new declared typed/mixed recursive mask; digest/raw/real routing and manualfix links preserved');
}
