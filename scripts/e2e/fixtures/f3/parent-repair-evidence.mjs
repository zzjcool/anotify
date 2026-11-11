// Finite parent-repair probes. Explicit preview evidence, never T1/Core integration.
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { openDriver, withPreviewResources, LANGS } from './f3-driver.mjs';
import { observability } from './f3-observability.mjs';
import { playbook } from './f3-playbook.mjs';
import { scanRawChrome, chromeResidue } from './f3-raw-content.mjs';
import { runUiCase } from './f3-ui-cases.mjs';
assert.equal(process.env.E2E_MOCK_ONLY, '1', 'Explicit mock authority required');
process.env.F3_RUN_ID ||= 'parent-repair-' + randomUUID();
const mode=process.argv[2];
const abort=new AbortController();
const deadline=setTimeout(()=>abort.abort(),50000);
const probe=async(id,fn,options={})=>{
  const d=await openDriver('parent-'+id,options);
  try { await fn(d); await d.finish(); }
  finally { await d.stop(); }
};
try {
  await withPreviewResources(async()=>{
    if(mode==='observe') return probe('observe',d=>observability(d,'errors'));
    if(mode==='playbook') return probe('playbook',d=>playbook(d,'delete'));
    if(mode==='data') {
      const langs=process.env.PARENT_REPAIR_LANG ? [process.env.PARENT_REPAIR_LANG] : LANGS;
      const failed=[];
      for(const lang of langs) await probe('data-'+lang,async d=>{
        await d.goto('index');
        const fixture=await d.page.evaluate(()=>{
          const s=MockApi.state;
          // Private data only; the real mock renderer and HTTP controller stay intact.
          const raw='原文回归 <img src=x onerror="window.__parentXSS=1"> & "payload"';
          s.capabilities[0].displayName=raw;
          s.keys[0].name=raw;
          s.sources[0].displayName=raw;
          s.events[0].payload={unit:raw};
          sessionStorage.setItem('f3-facts',JSON.stringify(s));
          return {raw,source:s.sources[0].name,snapshot:sessionStorage.getItem('f3-facts')};
        });
        for(const file of ['capabilities','keys','sources','source','events']) {
          try {
            await d.goto(file,'namespace=default'+(file==='source'?'&name='+encodeURIComponent(fixture.source):''),lang);
            const ready={capabilities:'[data-cap]',keys:'#keys-body tr',sources:'#src-list [data-name]',source:'#src-body section',events:'#ev-list article'}[file];
            await d.page.locator(ready).first().waitFor();
            if(file==='events') await d.page.locator('#ev-list article details').evaluateAll(rows=>rows.forEach(e=>{e.open=true;}));
            const scan=await d.page.evaluate(scanRawChrome);
            assert.deepEqual(scan.invalid,[],'Only text-only data SPANs are raw');
            const expectedRaw=file==='events'?JSON.stringify(fixture.raw).slice(1,-1):fixture.raw;
            assert.ok(scan.raw.some(x=>x.text.includes(expectedRaw)),file+' retains exact HTML-like raw data in a leaf');
            assert.deepEqual(chromeResidue(scan,lang),[],file+' translated surrounding chrome');
            assert.equal(await d.page.evaluate(()=>!!window.__parentXSS||!!document.querySelector('#page-main img[src="x"]')),false,'HTML-like raw data is never interpreted as markup');
            // CJK injected into actual chrome must still fail, including Japanese known-ZH guard.
            await d.page.evaluate(()=>{const b=document.createElement('button');b.id='parent-negative-chrome';b.textContent='总览 未翻译';document.getElementById('page-main').append(b);});
            const negative=await d.page.evaluate(scanRawChrome);
            if(lang!=='zh-CN') assert.ok(chromeResidue(negative,lang).length,'CJK chrome is not hidden by data markers');
            await d.page.locator('#parent-negative-chrome').evaluate(e=>e.remove());
            await d.page.evaluate(file=>{
              const key={capabilities:'capabilities',keys:'keys',sources:'sources',source:'sources',events:'events'}[file];
              MockApi.state[key]=[];
              sessionStorage.setItem('f3-facts',JSON.stringify(MockApi.state));
            },file);
            // Reload through the real controller into its empty/no-name branch.
            await d.goto(file,'namespace=default',lang);
            const dataRoot={capabilities:'[data-cap]',keys:'#keys-body',sources:'#src-list',source:'#src-name, #src-body',events:'#ev-list'}[file];
            await d.page.waitForFunction(()=>!document.querySelector('[aria-busy="true"]'));
            assert.equal(await d.page.locator(dataRoot).locator('[data-raw-content]').count(),0,'Empty/loading UI never inherits a raw data marker');
            const emptyScan=await d.page.evaluate(scanRawChrome);
            assert.deepEqual(emptyScan.invalid,[],'Empty chrome still uses the strict scanner');
            assert.deepEqual(chromeResidue(emptyScan,lang),[],'Empty/no-name labels remain localized');
            await d.page.evaluate(snapshot=>sessionStorage.setItem('f3-facts',snapshot),fixture.snapshot);
            d.check(file+' '+lang+': exact raw HTML-like data, strict leaf, translated chrome, CJK-negative guard and unmarked empty state');
          } catch(e) { failed.push({file,lang,error:e.message}); console.error('DATA FAIL '+JSON.stringify(failed.at(-1))); }
        }
      },{lang});
      assert.deepEqual(failed,[],'Every owned data page/locale attempted');
      return;
    }
    if(mode==='geometry') {
      const id=process.env.PARENT_GEOMETRY_CASE;
      assert.ok(['shift-prelayout-expanded','shift-prelayout-collapsed','interact-namespace-switch'].includes(id));
      const result=await runUiCase('ui_shift',id);
      assert.equal(result.ok,true,'Original geometry assertion remains unchanged');
      return result;
    }
    throw new Error('Unknown finite probe '+mode);
  },abort.signal);
  assert.equal(abort.signal.aborted,false,'Finite deadline must not masquerade as success');
  console.log('PARENT REPAIR PASS '+mode+' run='+process.env.F3_RUN_ID);
} catch(error) { console.error(error.stack||error);process.exitCode=1; }
finally { clearTimeout(deadline); }
