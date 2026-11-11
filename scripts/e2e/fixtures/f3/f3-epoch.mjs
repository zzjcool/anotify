// F4: one execution closure shared by all five entries. Evidence is not source.
import assert from 'node:assert/strict';
import { createHash, randomUUID } from 'node:crypto';
import { readdir, readFile, mkdir, writeFile } from 'node:fs/promises';
import { execFileSync } from 'node:child_process';
import path from 'node:path';
export const SUITES = Object.freeze(['frontend','i18n','i18n_coverage','ui_shift','ux_regression_19_23']);
const TEAM = 'scripts/e2e/fixtures/f3';
const ignored = /^(?:\.git|node_modules|\.env(?:\..*)?|settings(?:\..*)?)$|(?:^|[-_])(?:logs?|results?|screenshots)(?:$|[-_.])/i;
const asset = /\.(?:html|js|mjs|css|woff2?|ttf|otf|eot|png|jpe?g|gif|webp|avif|svg|ico|webmanifest|json|map)$/i;
async function walk(root, rel, accept = () => true) {
  let entries;
  try { entries = await readdir(path.join(root,rel), {withFileTypes:true}); }
  catch(e) { if(e.code==='ENOENT') return []; throw e; }
  const files = [];
  for(const e of entries.sort((a,b)=>a.name.localeCompare(b.name))) {
    if(ignored.test(e.name)) continue;
    const p = rel + '/' + e.name;
    if(e.isDirectory()) files.push(...await walk(root,p,accept));
    else if(e.isFile() && accept(p)) files.push(p);
    else if(e.isSymbolicLink()) throw new Error('Fingerprint refuses symlink: '+p);
  }
  return files;
}
async function descriptors(root, files) {
  return Promise.all([...new Set(files)].sort().map(async p => {
    const bytes=await readFile(path.join(root,p));
    return {path:p,bytes:bytes.length,sha256:createHash('sha256').update(bytes).digest('hex')};
  }));
}
// Length-delimited path/byte descriptors prevent path concatenation ambiguity.
export function descriptorSHA(files) { return createHash('sha256').update(JSON.stringify(files)).digest('hex'); }
export async function captureEpoch(root) {
  const source = SUITES.map(s=>'scripts/e2e/suites/'+s+'.mjs');
  source.push(...await walk(root,'scripts/e2e/lib',p=>p.endsWith('.mjs')));
  source.push('scripts/v4-frontend.test.mjs','Makefile','go.mod','go.sum','scripts/hash.mjs','scripts/check-classes.mjs','scripts/check-tailwind-css.mjs');
  source.push(...await walk(root,TEAM,p=>/\/f3-.*\.mjs$/.test(p)||/\/(?:E1B-browser|FINAL-READY)\.mjs$/.test(p)));
  // Exact tracked runtime/test dependencies outside the f3-* entry naming rule.
  source.push(TEAM+'/parent-geometry-probe.mjs',TEAM+'/parent-repair-evidence.mjs');
  for(const dir of ['web-src','cmd/sitegen','internal/sitegen']) source.push(...await walk(root,dir));
  // Authored static assets, excluding sitegen outputs (which belong to full SHA).
  source.push(...await walk(root,'web',p=>asset.test(p)&&!p.endsWith('.html')&&!/\/i18n\.[^/]+\.js$/.test(p)));
  const missingSpecs=[], presentSpecs=[];
  for(const spec of ['resource','events','agent','ingress','auth']) {
    const p='api/'+spec+'.yaml';
    try { await readFile(path.join(root,p)); source.push(p); presentSpecs.push(p); }
    catch(e) { if(e.code!=='ENOENT') throw e; missingSpecs.push(p); }
  }
  const sourceFiles=await descriptors(root,source);
  const files=await descriptors(root,[...source,...await walk(root,'web',p=>asset.test(p)),...await walk(root,'internal/server/dist',p=>asset.test(p))]);
  const sourceCodeSHA=descriptorSHA(sourceFiles),fullSourceArtifactSHA=descriptorSHA(files);
  return {sourceCodeSHA,fullSourceArtifactSHA,sourceHash:fullSourceArtifactSHA,artifactEpoch:fullSourceArtifactSHA,sourceFiles,files,presentSpecs,missingSpecs,authorityNote:'Missing real T1 specs are not fixture parity'};
}
export function gitIdentity(root) {
  const git=(...args)=>execFileSync('git',args,{cwd:root,encoding:'utf8'}).trim();
  return {branch:git('branch','--show-current'),HEAD:git('rev-parse','HEAD')};
}
export function epochFields(epoch) {
  return {sourceHash:epoch.fullSourceArtifactSHA,sourceCodeSHA:epoch.sourceCodeSHA,fullSourceArtifactSHA:epoch.fullSourceArtifactSHA,artifactEpoch:epoch.artifactEpoch};
}
export function assertFrozen(before,after) {
  assert.equal(after.sourceCodeSHA,before.sourceCodeSHA,'Source code changed within run');
  assert.equal(after.fullSourceArtifactSHA,before.fullSourceArtifactSHA,'FULL generated/served artifact changed within run');
}
const keys=['suite','id','runId','attemptId','cwd','mode','authority','sourceHash','sourceCodeSHA','fullSourceArtifactSHA','artifactEpoch'];
export function validateAttempt(selected,result,exit,expected) {
  for(const record of [selected,result,exit]) {
    assert.ok(record,'Missing immutable attempt/result/exit');
    for(const key of keys) { assert.ok(expected[key], 'Expected '+key+' required'); assert.equal(record[key],expected[key],'Exact attempt '+key); }
  }
  assert.equal(exit.exitCode,0,'Actual nonzero exit');assert.equal(exit.signal,null,'Actual signal');assert.equal(exit.timedOut,false,'Actual deadline');
  assert.equal(result.ok,true,'Failed browser case');assert.ok(Number.isInteger(result.checks)&&result.checks>0,'Zero assertions');
  assert.deepEqual(result.report?.errors,[],'Missing browser report or actual errors');
}
export function validateManifest(manifest, registered, enrollments, exits) {
  assert.deepEqual(manifest.registered,registered,'Exact registered IDs');
  const ids=manifest.attempts.map(x=>x.id);
  assert.equal(new Set(ids).size,ids.length,'Duplicate planned ID');
  assert.deepEqual([...ids].sort(),[...registered].sort(),'Missing/extra planned attempt');
  assert.equal(new Set(manifest.attempts.map(x=>x.attemptId)).size,ids.length,'Duplicate attemptId');
  assert.equal(enrollments.length,ids.length,'Missing/extra attempt enrollment: retries never choose latest green');
  for(const planned of manifest.attempts) {
    const matches=enrollments.filter(x=>x.id===planned.id);
    assert.equal(matches.length,1,'Exactly once: '+planned.id);assert.deepEqual(matches[0],planned,'Immutable planned attempt');
    if(exits) { const settled=exits.filter(x=>x.id===planned.id);assert.equal(settled.length,1,'Exactly one settled exit'); for(const key of keys)assert.equal(settled[0][key],planned[key]); }
  }
}
// Only the root reserves attempt IDs; internal fragments can only read the plan.
export async function planRun(dir, epoch, ids, {fragment=false,aggregate=false,attemptId}={}) {
  await mkdir(dir,{recursive:true});const file=path.join(dir,'attempt-manifest.json');
  let plan;
  if(fragment||aggregate) plan=JSON.parse(await readFile(file,'utf8'));
  else {
    const git=gitIdentity(epoch.cwd);
    const status=execFileSync('git',['status','--porcelain','--untracked-files=all'],{cwd:epoch.cwd,encoding:'utf8'}).split('\n').filter(Boolean);
    const sourcePaths=new Set(epoch.sourceFiles.map(x=>x.path));
    const dirtySources=status.map(x=>x.slice(3)).filter(p=>sourcePaths.has(p));
    plan={...epoch,git,SourceVerificationHEAD:dirtySources.length?null:git.HEAD,DeliveryHEAD:git.HEAD,dirtySources,attempts:ids.map(id=>({...epochFields(epoch),suite:epoch.suite,id,runId:epoch.runId,cwd:epoch.cwd,mode:epoch.mode,authority:epoch.authority,attemptId:attemptId||randomUUID()}))};
    await writeFile(file,JSON.stringify(plan,null,2)+'\n',{flag:'wx'});
  }
  for(const key of ['suite','runId','cwd','mode','authority','sourceHash','sourceCodeSHA','fullSourceArtifactSHA','artifactEpoch']) assert.equal(plan[key],epoch[key],'Immutable manifest '+key);
  assert.deepEqual(plan.registered,epoch.registered);
  return plan;
}
export async function enrollAttempt(dir, identity, plan) {
  assert.deepEqual(plan.attempts.find(x=>x.id===identity.id),identity,'Only the reserved attempt can enroll');
  const target=path.join(dir,identity.id);await mkdir(target,{recursive:true});
  const file=path.join(target,'attempt-'+identity.attemptId+'.json');
  try {await writeFile(file,JSON.stringify(identity,null,2)+'\n',{flag:'wx'});}
  catch(e) {if(e.code!=='EEXIST')throw e;assert.deepEqual(JSON.parse(await readFile(file,'utf8')),identity);}
}
export async function readEnrollments(dir) {
  const all=[];
  for(const entry of await readdir(dir,{withFileTypes:true})) if(entry.isDirectory()) {
    for(const file of await readdir(path.join(dir,entry.name))) if(/^attempt-.*\.json$/.test(file))all.push(JSON.parse(await readFile(path.join(dir,entry.name,file),'utf8')));
  }
  return all;
}
