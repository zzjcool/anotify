// F3 shared browser driver. Adapted from F2b2/F2c1/F2c2 self-host/early recorder.
// Explicit fixture authority only. Real mode always H.startServer, never fallback.
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { createServer } from 'node:http';
import { readFile, mkdir, writeFile } from 'node:fs/promises';
import { existsSync, readdirSync } from 'node:fs';
import { homedir } from 'node:os';
import path from 'node:path';
import { chromium } from 'playwright-core';
import * as H from '../../lib/harness.mjs';
import { installTraceEvidence, traceEvidenceTools } from './f3-trace-evidence.mjs';
export const ROOT = H.ROOT_DIR;
export const TEAM = path.join(ROOT, 'scripts/e2e/fixtures/f3');
export const OUT = path.join(TEAM, 'screenshots/f3');
export const LANGS = ['zh-CN', 'en', 'ja', 'es'];
export const PAGES = ['admin','capabilities','cli-auth','docs','events','index','keys','login','message','node','nodes','passkey-enroll','playbook','playbooks','receivers','security','source','sources','trace'];
export const GUARDED = ['index','receivers','keys','security','message','events','sources','capabilities','nodes'];
export const isMock = () => process.env.E2E_MOCK_ONLY === '1';
export function chromePath() {
  if (process.env.E2E_CHROME_PATH) return process.env.E2E_CHROME_PATH;
  const cache = path.join(homedir(), '.cache/ms-playwright');
  if (existsSync(cache)) for (const dir of readdirSync(cache).filter(x => x.startsWith('chromium-')).sort().reverse()) {
    for (const rel of ['chrome-linux/chrome','chrome-linux64/chrome']) {
      const file = path.join(cache, dir, rel); if (existsSync(file)) return file;
    }
  }
  for (const file of ['/usr/bin/google-chrome','/usr/bin/chromium']) if (existsSync(file)) return file;
  throw new Error('Installed Chrome required; no VM/browser substitute');
}
export async function rawReq(base, url, opts = {}) {
  assert.ok(url.startsWith('/v1/'), 'raw v4 URL required');
  const r = await fetch(base + url, { ...opts, signal: AbortSignal.timeout(5000) });
  return { status: r.status, body: await r.json() };
}
async function preview() {
  assert.equal(isMock(), true, 'Static preview is admitted only by E2E_MOCK_ONLY=1');
  const root = path.join(ROOT, 'web');
  const types = { '.html':'text/html', '.js':'text/javascript', '.css':'text/css', '.svg':'image/svg+xml', '.woff2':'font/woff2', '.png':'image/png', '.webmanifest':'application/manifest+json' };
  const server = createServer(async (req, res) => {
    try {
      const u = new URL(req.url, 'http://fixture');
      if (u.pathname.startsWith('/v1/')) {
        res.writeHead(501, { 'Content-Type':'application/json' });
        return res.end(JSON.stringify({ apiVersion:'anotify/v1',kind:'Status',status:'Failure',code:501,reason:'unimplemented',message:'Explicit preview: network API not implemented' }));
      }
      let pathname = u.pathname === '/' ? '/index.html' : u.pathname;
      if (u.searchParams.has('lang') && LANGS.includes(u.searchParams.get('lang')) && u.searchParams.get('lang') !== 'zh-CN' && !/^\/(en|ja|es)\//.test(pathname)) pathname = '/' + u.searchParams.get('lang') + pathname;
      const file = path.resolve(root, '.' + decodeURIComponent(pathname));
      assert.ok(file.startsWith(root + path.sep), 'Preview path outside web');
      let body = await readFile(file);
      if (file.endsWith('.html')) body = Buffer.from(body.toString().replace('<head>', '<head><script src="/mock.js"></script>'));
      res.writeHead(200, { 'Content-Type':types[path.extname(file)] || 'application/octet-stream' }); res.end(body);
    } catch { res.writeHead(404); res.end('Fixture file unavailable'); }
  });
  server.requestTimeout = 10000; server.headersTimeout = 10000;
  await new Promise((resolve,reject) => { server.once('error',reject); server.listen(0,'127.0.0.1',resolve); });
  return { base:'http://127.0.0.1:' + server.address().port, stop:async () => { server.closeAllConnections(); await new Promise(resolve => server.close(resolve)); } };
}
// FINAL-R1's bounded collector shares only browser/preview ownership. Every
// driver (including expected-rejection probes) still owns an isolated context.
let previewResources;
export async function withPreviewResources(fn, signal) {
  assert.equal(isMock(),true,'Shared preview resources require explicit mock authority');
  assert.equal(previewResources,undefined,'One SERIAL resource owner');
  const server=await preview(); let browser, closing;
  const interrupt=()=>{ if(browser) closing ||= browser.close(); };
  signal?.addEventListener('abort',interrupt,{once:true});
  try {
    browser=await chromium.launch({executablePath:chromePath(),headless:true,args:['--no-sandbox']});
    previewResources={server,browser};
    if(signal?.aborted) throw new Error('Scoped browser deadline before launch');
    return await fn();
  } finally {
    previewResources=undefined;signal?.removeEventListener('abort',interrupt);
    try { if(browser) await (closing ||= browser.close()); } finally { await server.stop(); }
  }
}
// Immutable mode/attempt evidence, independently testable without a browser.
export function evidenceDirectory(report, directory = OUT) {
  for(const value of [report.runId,report.id,report.attemptId]) assert.match(value,/^[a-zA-Z0-9_-]+$/,'Safe evidence identity required');
  assert.ok(['real','explicit-mock'].includes(report.mode),'Explicit evidence authority required');
  return path.join(directory,'runs',report.runId,report.mode,report.id,report.attemptId);
}
export async function persistReport(report, directory = OUT) {
  const dir=evidenceDirectory(report,directory);await mkdir(dir,{recursive:true});
  const file=path.join(dir,'report.json');
  report.reportPath=file;
  await writeFile(file,JSON.stringify(report,null,2)+'\n',{flag:'wx'});
  return file;
}
export async function openDriver(id, options = {}) {
  await mkdir(OUT, { recursive:true });
  const mock = isMock();
  console.log(`[F3 ${id}] authority=${mock ? 'explicit browser mock fixture, NOT T1/T3 integration' : 'REAL H.startServer, no mock fallback'}`);
  let server, browser, context, page, stopped, archive;
  const shared=mock && previewResources;
  const report = { id, runId:process.env.F3_RUN_ID||'current', attemptId:process.env.F3_ATTEMPT_ID||randomUUID(), cwd:ROOT, sourceHash:process.env.F3_SOURCE_HASH||null, mode:mock?'explicit-mock':'real', checks:[], errors:[], requests:[], screenshots:[], geometry:[] };
  const reportDir=evidenceDirectory(report);await mkdir(reportDir,{recursive:true});
  const stop = () => stopped ||= (async () => {
    // Preserve a failed controller's catch/finally scene before closing its doc.
    // Collection must not replace the existing functional failure on cleanup.
    if (archive && page && !page.isClosed()) {
      try { await archive(); }
      catch (error) { (report.traceArchiveErrors ||= []).push(traceEvidenceTools().error(error)); }
    }
    if (shared) { if(context) await context.close(); }
    else { if (browser) await browser.close(); if (server) await server.stop(); }
    await persistReport(report);
  })();
  try {
    if (mock) server = shared ? shared.server : await preview();
    else {
      assert.ok(existsSync(H.BIN), `T1 seam: ANOTIFY_BIN missing: ${H.BIN}; build genuine v4 server, no preview fallback`);
      assert.ok(existsSync(H.DEVSEED_BIN), `T1 seam: DEVSEED_BIN missing: ${H.DEVSEED_BIN}; generic retained auth seed required`);
      server = await H.startServer({ suiteName:'frontend' });
      const seed = H.seed(server.dbPath, 'f3');
      assert.ok(seed.session, 'T1 seam: devseed must return SESSION');
      server.session = seed.session;
      const ns = await rawReq(server.base,'/v1/namespaces',{headers:{Cookie:'anotify_session='+seed.session}});
      assert.equal(ns.status,200,'T1 seam: genuine namespaced API bootstrap must respond 200');
      assert.equal(ns.body.kind,'NamespaceList','T1 seam: legacy namespace adapter is forbidden');
      assert.ok(ns.body.items.some(n=>n.metadata.name==='default'),'T1 seam: default Namespace fixture required');
      assert.ok(ns.body.items.some(n=>n.metadata.name==='prod'),'T1 seam: prod Namespace fixture required');
    }
    browser = shared ? shared.browser : await chromium.launch({ executablePath:chromePath(),headless:true,args:['--no-sandbox'] });
    context = await browser.newContext({viewport:{width:1440,height:900},locale:options.lang||'en',reducedMotion:'reduce',acceptDownloads:true});
    await H.mockExternalCdn(context);
    if (!mock) await context.addCookies([{name:'anotify_session',value:server.session,url:server.base}]);
    await context.addInitScript({ content: '(' + installTraceEvidence.toString() + ')(' + traceEvidenceTools.toString() + ');' });
    await context.addInitScript(({mock,id,scope,theme}) => {
      if (!sessionStorage.getItem('f3-scope-initialized')) {
        if (scope) localStorage.setItem('anotify.scope',JSON.stringify({namespace:scope}));
        else localStorage.removeItem('anotify.scope');
        sessionStorage.setItem('f3-scope-initialized','1');
      }
      localStorage.setItem('anotify-theme',theme);
      window.__f3Mock = mock; window.__f3Calls = []; window.__f3Reject = ''; window.__f3Guest = false; window.__f3Member = false;
      // Fault authority belongs to this isolated case/context, not its document.
      // Same-case navigation preserves it; an explicit clear really removes it.
      let fault = sessionStorage.getItem('f3-transport-fault') || '';
      Object.defineProperty(window,'__f3Fault',{configurable:true,get:()=>fault,set(value){
        fault=String(value||'');
        if(fault) sessionStorage.setItem('f3-transport-fault',fault);
        else sessionStorage.removeItem('f3-transport-fault');
      }});
      for (const alias of ['__b2Calls','__c1Calls','__c2Calls','__sourceCalls','__c1Transport']) Object.defineProperty(window,alias,{configurable:true,get:()=>window.__f3Calls,set:v=>{window.__f3Calls=v;}});
      let exported;
      Object.defineProperty(window,'Anotify',{configurable:true,get:()=>exported,set(value){
        if (mock) {
          if (!window.MockApi) throw new Error('Explicit preview must provide MockApi before controller');
          const snapshot = sessionStorage.getItem('f3-facts');
          if (snapshot) Object.assign(MockApi.state,JSON.parse(snapshot));
          else if (id.startsWith('node-')) {
            const node = MockApi.state.nodes[0];
            node.spec.vars={SOURCE_ROOT:'/node-default',NODE_ONLY:'node-visible'};
            node.facts.localVars={SOURCE_ROOT:'/local-src'};
            node.spec.limits={total:8,perCapability:{'*':3,fixer:2}};
            node.facts.localLimits={total:4,perCapability:{fixer:1}};
            node.facts.instances=[{slot:0,busy:true,taskId:'tsk_mock_4',attempt:3,leaseId:'physical-lease'},{slot:1},{slot:2,busy:false},{slot:3,busy:true,taskId:'reserved-task',assignmentId:'reservation-no-lease'}];
            node.facts.envPassthrough=['COMPLETE_PRIVATE_WHITELIST_NAME'];
            node.facts.resolvedEnv={PRIVATE_VALUE:'C2_EXACT_HOST_VALUE_NOT_PUBLIC_728f'};
            node.facts.artifactDir='/private/export/absolute/c2-not-public';
            MockApi.state.nsVars.default={SOURCE_ROOT:'/ns-default',NODE_ONLY:'ns-loses',NS_ONLY:'default-visible'};
            MockApi.state.nsVars.prod={NS_ONLY:'prod-visible'};
            if (id==='node-namespace-system') MockApi.state.systemConfig.retention={resultTtlSec:3600,eventTtlSec:7000,taskTtlSec:9000};
          }
          MockApi.latency(0);
          value.pushUIEnabled=true;
        }
        const raw = window.fetch;
        window.fetch = async (url,opts={}) => {
          const target=typeof url==='string'?url:String(url.url),method=opts.method||'GET';
          let body; if(typeof opts.body==='string') { try {body=JSON.parse(opts.body);} catch { /* raw upload */ } }
          const call={url:target,method,body,keys:Object.keys(body||{}),configKeys:Object.keys(body?.spec?.config||body?.source?.spec?.config||{})};
          if(target.startsWith('/v1/')) __f3Calls.push(call);
          try {
            if(__f3Fault && target.includes(__f3Fault)) throw new Error('F3 deliberate disconnected transport');
            let response;
            const failure=(code,reason,message)=>new Response(JSON.stringify({apiVersion:'anotify/v1',kind:'Status',status:'Failure',code,reason,message,details:{causes:[]}}),{status:code,headers:{'Content-Type':'application/json'}});
            if(mock && target==='/v1/auth/me' && (sessionStorage.getItem('f3-guest')==='1'||__f3Guest)) {
              MockApi.state.auth.loggedOut=true; sessionStorage.setItem('anotify.mock.loggedOut','1'); document.cookie='anotify_session=; Path=/; Max-Age=0';
              response=await raw(url,opts); // AUTH1: genuine cookie/state-aligned text/plain {error}401.
            }
            else if(mock && target==='/v1/auth/me' && (sessionStorage.getItem('f3-member')==='1'||__f3Member)) response=new Response(JSON.stringify({id:'member-fixture',username:'member',displayName:'Member',role:'member'}),{headers:{'Content-Type':'application/json'}});
            else if(mock && __f3Reject && method!=='GET' && target.includes(__f3Reject)) response=target.startsWith('/v1/auth/')
              ? new Response(JSON.stringify({error:'Actual retained rejection sentinel'}),{status:400,headers:{'Content-Type':'application/json'}})
              : failure(400,'retained_rejection','Actual retained rejection sentinel'); // Resource/Key fixtures unchanged.
            else response=await raw(url,opts);
            call.httpStatus=response.status;
            if((response.headers.get('Content-Type')||'').includes('json') || (target==='/v1/auth/me' && response.status===401)) {
              window.__f3TraceResponse?.(call,'parse-start');
              call.result=await response.clone().json();call.response=call.result;
              window.__f3TraceResponse?.(call,'parsed');
              if(response.status>=400) {call.status=response.status;call.reason=call.result?.reason;call.details=call.result?.details;}
            }
            if(mock) {
              const snapshot=JSON.parse(JSON.stringify(MockApi.state));
              // Never persist the returned once-only identity token across navigation.
              for(const n of snapshot.nodes||[]) delete n.identityToken;
              sessionStorage.setItem('f3-facts',JSON.stringify(snapshot));
            }
            window.__f3TraceResponse?.(call,'return');
            return response;
          } catch(error) {call.error=String(error);window.__f3TraceResponse?.(call,'throw');throw error;}
        };
        exported=value;
        window.__f3TraceAttach?.(value);
      }});
    },{mock,id,scope:options.scope??'default',theme:options.theme||'light'});
    page=await context.newPage(); page.setDefaultTimeout(4500); page.setDefaultNavigationTimeout(7000);
    page.on('pageerror',e=>report.errors.push(e.message));
    page.on('console',msg=>{if(msg.type()==='error'||msg.text().includes('mock 未实现')) report.errors.push(msg.text());});
    page.on('requestfailed',r=>{if(!r.url().startsWith('http')||r.failure()?.errorText==='net::ERR_ABORTED')return;report.errors.push('network '+r.url()+' '+r.failure()?.errorText);});
    const calls=()=>page.evaluate(()=>window.__f3Calls);
    archive=async()=>{
      if(!page.url().startsWith(server.base)) return;
      // One existing read, bound to this document; save before calls are cleared.
      const {rows,trace}=await page.evaluate(()=>({rows:window.__f3Calls,trace:window.__f3TraceSnapshot?.()}));
      if(trace) {
        const docs=report.traceDocuments ||= [];
        const previous=docs.find(d=>d.documentId===trace.documentId);
        if(previous) { previous.snapshots.push(trace.snapshot); Object.assign(previous,trace); }
        else docs.push({...trace,snapshots:[trace.snapshot]});
      }
      // Reports never contain raw credentials, scripts, host values or receipt values.
      report.requests.push(...rows.map(c=>({url:c.url,method:c.method,httpStatus:c.httpStatus,reason:c.reason,kind:c.result?.kind,itemCount:c.result?.items?.length,resourceVersion:c.body?.metadata?.resourceVersion,error:c.error})));
      await page.evaluate(()=>{window.__f3Calls=[];});
    };
    const goto=async(file,query='namespace=default',lang=options.lang||'en')=>{
      await archive();
      await page.goto(server.base+(lang==='zh-CN'?'/':'/'+lang+'/')+file+'.html'+(query?'?'+query:''),{waitUntil:'domcontentloaded'});
      await page.waitForFunction(()=>!!window.Anotify && !document.querySelector('[aria-busy="true"]'));
    };
    const req=(url,opts)=>page.evaluate(async([url,opts])=>Anotify.api(url,opts),[url,opts]);
    const response=(url,body,method='GET')=>page.evaluate(async([url,body,method])=>{const r=await fetch(url,{method,...(body==null?{}:{body:JSON.stringify(body),headers:{'Content-Type':'application/json'}})});return {status:r.status,body:await r.json()};},[url,body,method]);
    const snapshot=async()=>{
      if(mock)return page.evaluate(()=>JSON.stringify(MockApi.state));
      const paths=['/v1/namespaces/default/capabilities','/v1/namespaces/default/sources','/v1/namespaces/default/playbooks','/v1/namespaces/default/tasks','/v1/console/events?namespace=default','/v1/nodes','/v1/system/config','/v1/namespaces/default/config'];
      const result=[];
      for(const p of paths)result.push(await req(p));
      const pbs=result[2];
      assert.equal(pbs.kind,'PlaybookList');
      for(const p of pbs.items)result.push(await req('/v1/namespaces/default/playbooks/'+encodeURIComponent(p.metadata.name)+'/installs'));
      return JSON.stringify(result);
    };
    const check=(name)=>{report.checks.push(name);console.log('PASS '+id+' :: '+name);};
    const shot=async(name)=>{
      for(const sel of ['#reg-token','#node-token','#new-key-value']) if(await page.locator(sel).count()) assert.equal((await page.locator(sel).textContent()).trim(),'','Never screenshot once-only secret');
      const file=path.join(reportDir,name+'.png');await page.screenshot({path:file});report.screenshots.push(file);
    };
    const clean=async(label)=>{
      const g=await page.evaluate(()=>{
        const overflow=[],clips=[];
        for(const e of document.querySelectorAll('#page-main *')){
          const r=e.getBoundingClientRect(),s=getComputedStyle(e);
          if(s.display==='none'||!r.width||(r.left>=-2&&r.right<=innerWidth+2))continue;
          let bounded=false;
          for(let p=e.parentElement;p&&p!==document.body;p=p.parentElement){
            const ps=getComputedStyle(p),pr=p.getBoundingClientRect();
            if(!['auto','scroll','hidden','clip'].includes(ps.overflowX)||!p.clientWidth||pr.left<-2||pr.right>innerWidth+2)continue;
            if(['auto','scroll'].includes(ps.overflowX)&&p.scrollWidth>p.clientWidth){const before=p.scrollLeft;p.scrollLeft=before?0:Math.min(32,p.scrollWidth-p.clientWidth);const moved=p.scrollLeft!==before;p.scrollLeft=before;if(!moved)continue;}
            bounded=true;clips.push({tag:e.tagName,id:e.id,clip:p.tagName});break;
          }
          if(!bounded)overflow.push({tag:e.tagName,id:e.id,left:r.left,right:r.right});
        }
        return {width:innerWidth,scrollWidth:document.documentElement.scrollWidth,height:innerHeight,scrollHeight:document.documentElement.scrollHeight,overflow,clips};
      });
      report.geometry.push({label,...g});
      assert.ok(g.scrollWidth<=g.width+2,label+': document overflow');assert.deepEqual(g.overflow,[],label+': bounded component overflow');
      await page.evaluate(()=>scrollTo(0,0));await page.mouse.wheel(0,650);
      if(g.scrollHeight>g.height+5)await page.waitForFunction(()=>scrollY>0);
      await page.evaluate(()=>scrollTo(0,0));check(label+': actual geometry and unlocked scroll');
    };
    const finish=async()=>{await archive();assert.deepEqual(report.errors,[],'JS/console/network/unimplemented501 must fail');};
    return {id,mock,base:server.base,page,context,browser,report,stop,finish,goto,req,response,snapshot,calls,check,shot,clean,archive};
  } catch(error){report.errors.push(error.stack||String(error));await stop();throw error;}
}
