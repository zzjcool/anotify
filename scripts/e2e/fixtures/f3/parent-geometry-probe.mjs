import assert from 'node:assert/strict';
// Passive geometry instrumentation; opt-in native mutations are negative test fixtures only.
// No frame is discarded; document identity is retained across the real reload.
export async function attachGeometry(d) {
  d.report.parentNavigations=[];
  d.page.on('framenavigated',frame=>{
    if(frame===d.page.mainFrame()) d.report.parentNavigations.push({url:frame.url(),at:Date.now()});
  });
  const mutation=process.env.PARENT_GEOMETRY_MUTATION||'';
  if(mutation) assert.equal(d.mock,true,'Native geometry mutations require explicit fixture authority');
  await d.context.addInitScript(({mutation})=>{
    const id=crypto.randomUUID(), key='parent-geometry-documents';
    const data=window.__parentGeometry={documentId:id,timeOrigin:performance.timeOrigin,url:location.href,frames:[],events:[],requests:[],paints:[],stop:false};
    const seen=new WeakSet();
    const rect=e=>{if(!e)return null;const r=e.getBoundingClientRect(),s=getComputedStyle(e);return {tag:e.tagName,id:e.id,className:e.className,x:r.x,y:r.y,width:r.width,height:r.height,right:r.right,display:s.display,paddingLeft:s.paddingLeft,paddingRight:s.paddingRight};};
    let mutated=false;
    const mutate=()=>{
      if(!mutation)return;
      const main=document.getElementById('page-main'),shell=document.getElementById('app-shell'),sidebar=document.getElementById('sidebar');
      if(mutation==='pre-offset') {
        if(main&&!shell&&!mutated) { main.style.transform='translateX(13px)';mutated=true; }
        if(main&&shell&&main.style.transform) main.style.transform='';
        return;
      }
      if(mutated||!shell||!sidebar)return;
      if(mutation==='outer-left') { document.querySelector('#app-shell main').style.transform='translateX(11px)';mutated=true; }
      if(mutation==='page-x') { document.querySelector('.page').style.transform='translateX(17px)';mutated=true; }
      const slot=document.getElementById('scope-slot');
      if(slot && mutation==='slot-zero-recovered') {
        const old=slot.getAttribute('style');slot.style.setProperty('height','0px','important');slot.style.setProperty('min-height','0px','important');mutated=true;
        requestAnimationFrame(()=>{if(old===null)slot.removeAttribute('style');else slot.setAttribute('style',old);});
      }
      if(slot && mutation==='slot-missing-recovered') {
        const parent=slot.parentNode,next=slot.nextSibling;slot.remove();mutated=true;
        requestAnimationFrame(()=>parent.insertBefore(slot,next));
      }
    };
    const capture=label=>{
      mutate();
      const main=document.getElementById('page-main'),slot=document.getElementById('scope-slot'),sw=document.getElementById('scope-switcher');
      const box={label,t:performance.now(),documentId:id,url:location.href,readyState:document.readyState,scope:localStorage.getItem('anotify.scope'),shell:rect(document.getElementById('app-shell')),sidebar:rect(document.getElementById('sidebar')),outer:rect(document.querySelector('#app-shell main')),content:rect(main),contentParent:rect(main?.parentElement),page:rect(document.querySelector('.page')),mains:[...document.querySelectorAll('main')].map(rect),slot:rect(slot),nav:rect(document.querySelector('.app-nav')),hint:rect(document.querySelector('.lang-hint')),switcher:rect(sw),switcherText:sw?.textContent?.trim()||null,bodyPresent:!!document.body};
      for(const c of window.__f3Calls||[]) if(!seen.has(c)) {seen.add(c);data.requests.push({t:box.t,documentId:id,url:c.url,method:c.method});}
      box.requests=(window.__f3Calls||[]).filter(c=>c.url==='/v1/auth/me'||c.url.includes('namespace=prod')||c.url.includes('/namespaces/prod/')).map(c=>({url:c.url,method:c.method,httpStatus:c.httpStatus,kind:c.result?.kind,me:c.url==='/v1/auth/me'?{id:c.result?.id,role:c.result?.role}:undefined}));
      return box;
    };
    window.__parentGeometryCapture=capture;
    data.events.push(capture('init-script'));
    new MutationObserver(()=>{if(!data.stop)data.events.push(capture('mutation'));}).observe(document,{subtree:true,childList:true,attributes:true,attributeFilter:['class','style','data-collapsed']});
    const frame=()=>{if(data.stop)return;data.frames.push(capture('raf'));requestAnimationFrame(frame);};
    requestAnimationFrame(frame);
    for(const name of ['DOMContentLoaded','load']) addEventListener(name,()=>data.events.push(capture(name)));
    try {new PerformanceObserver(list=>{data.paints.push(...list.getEntries().map(e=>({name:e.name,startTime:e.startTime,documentId:id})));}).observe({type:'paint',buffered:true});}catch { /* paint timing is optional browser evidence, rects are not */ }
    addEventListener('pagehide',()=>{
      data.events.push(capture('pagehide'));
      const old=JSON.parse(sessionStorage.getItem(key)||'[]');
      old.push(data);sessionStorage.setItem(key,JSON.stringify(old));
    });
  },{mutation});
}
export async function geometrySnapshot(d,label) {
  return d.page.evaluate(label=>window.__parentGeometryCapture(label),label);
}
export async function recordGeometry(d,label,extra={}) {
  const documents=await d.page.evaluate(()=>{
    const data=window.__parentGeometry;
    data.events.push(window.__parentGeometryCapture('diagnostic-read'));
    data.stop=true;
    return [...JSON.parse(sessionStorage.getItem('parent-geometry-documents')||'[]'),data];
  });
  const record={label,...extra,documents};
  d.report.geometry.push(record);
  // Every underlying frame and request remains in report.json; stdout is a finite index.
  console.log('PARENT GEOMETRY '+JSON.stringify({label,measured:extra.measured,before:extra.before,after:extra.after,documents:documents.map(doc=>({documentId:doc.documentId,url:doc.url,timeOrigin:doc.timeOrigin,frames:doc.frames.length,events:doc.events.length,paints:doc.paints,firstFrame:doc.frames[0],lastFrame:doc.frames.at(-1)}))}));
  return record;
}

const equalBox=(actual,expected,claim)=>assert.equal(Math.round(actual*100)/100,Math.round(expected*100)/100,'parent geometry: '+claim);
export function assertPrelayoutGeometry(record) {
  const doc=record.documents.at(-1), frames=doc.frames;
  const pre=frames.filter(x=>!x.shell&&!x.sidebar&&x.content);
  const mounted=frames.filter(x=>x.shell||x.sidebar);
  assert.ok(pre.length>=10,'parent geometry: all existing premount frames retained');
  assert.ok(mounted.length>0,'parent geometry: actual mounted shell required');
  assert.ok(mounted[0].sidebar&&mounted[0].outer,'parent geometry: first mounted outer/sidebar boxes required');
  assert.ok(pre[0].page,'parent geometry: first existing premount page is required');
  const boundary=mounted[0].sidebar.right, visual=pre[0].page.x;
  for(const x of pre) {
    equalBox(x.content.x,boundary,'premount sidebar reservation');
    assert.ok(x.page,'parent geometry: existing premount page is never skipped');
    equalBox(x.page.x,visual,'premount visual page x');
    const left=parseFloat(x.content.paddingLeft),right=parseFloat(x.content.paddingRight);
    equalBox(x.page.x,x.content.x+left+(x.content.width-left-right-x.page.width)/2,'computed premount container gutter');
  }
  for(const x of mounted) {
    assert.ok(x.outer&&x.content&&x.sidebar&&x.page,'parent geometry: mounted boxes stay present');
    equalBox(x.outer.x,x.sidebar.right,'mounted outer boundary');
    equalBox(x.outer.x,boundary,'same premount/mounted outer boundary');
    equalBox(x.content.x,x.outer.x+parseFloat(x.outer.paddingLeft),'computed mounted content gutter');
    equalBox(x.content.right,x.outer.right-parseFloat(x.outer.paddingRight),'computed mounted right gutter');
    equalBox(x.page.x,visual,'same-document visual page x');
    equalBox(x.page.x,x.content.x+(x.content.width-x.page.width)/2,'computed mounted centered page');
  }
  return {documentId:doc.documentId,preFrames:pre.length,mountedFrames:mounted.length,outerX:boundary,visualX:visual};
}
export function assertNamespaceGeometry(record) {
  const before=record.beforeGeometry, after=record.afterRequests;
  assert.notEqual(before.documentId,after.documentId,'parent geometry: native scope switch creates a new document');
  const doc=record.documents.find(x=>x.documentId===after.documentId);
  assert.ok(doc,'parent geometry: new document evidence required');
  const samples=[...doc.events,...doc.frames].sort((a,b)=>a.t-b.t);
  const first=samples.findIndex(x=>x.shell||x.sidebar);
  assert.ok(first>=0,'parent geometry: bounded acquisition of first synchronous shell mount');
  // Earlier absent-DOM samples remain untouched in doc.events/doc.frames.
  for(const [i,x] of samples.slice(first).entries()) {
    assert.ok(x.shell&&x.sidebar,'parent geometry: mounted sidebar/shell must never disappear');
    assert.ok(x.slot&&x.slot.height>=40,'parent geometry: '+(i===0?'first-mounted':'subsequent-mounted')+' slot reservation >=40');
    assert.ok(x.nav,'parent geometry: mounted nav must exist');
    equalBox(x.nav.y,before.nav.y,'native switch nav position');
    equalBox(x.slot.y,before.slot.y,'native switch slot position');
  }
  return {documentId:doc.documentId,firstMounted:samples[first],mountedSamples:samples.length-first,absentSamples:first};
}
