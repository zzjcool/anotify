#!/usr/bin/env node
import assert from 'node:assert/strict';
import { captureEpoch, epochFields, assertFrozen, validateAttempt, planRun, enrollAttempt, readEnrollments, validateManifest } from '../fixtures/f3/f3-epoch.mjs';
// Standalone v4 frontend suite. Retirement map: team F3-TEST-MAP.md.
// E2E_MOCK_ONLY=1 is EXPLICIT browser-preview authority; default is real server.
// No H.req legacy adapter, flat compatibility, implicit mock, skips or fake PASS.
import * as H from '../lib/harness.mjs';
import { spawn } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { writeFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { CASES, runCase } from '../fixtures/f3/f3-cases.mjs';
import { ROOT, OUT, isMock, evidenceDirectory } from '../fixtures/f3/f3-driver.mjs';
const SELF=fileURLToPath(import.meta.url),cwd=process.cwd();
const runId=process.env.F3_RUN_ID||randomUUID(),mode=isMock()?'explicit-mock':'real';
if(!/^[a-zA-Z0-9_-]+$/.test(runId))throw new Error('Invalid F3_RUN_ID');
const runDir=path.join(OUT,'runs',runId,mode);
const children=new Set();let interrupted=false;
for(const signal of ['SIGINT','SIGTERM'])process.on(signal,()=>{
  interrupted=true;
  for(const proc of children)try{process.kill(-proc.pid,'SIGTERM');}catch{ /* already settled */ }
});
let frozen,plan;
const identity=(id,sourceHash,attemptId)=>({suite:'frontend',id,runId,mode,authority:mode,...epochFields(frozen),attemptId,cwd});
async function fragment(id,sourceHash){
  const meta=identity(id,sourceHash,process.env.F3_ATTEMPT_ID||plan.attempts.find(x=>x.id===id).attemptId);
  const dir=evidenceDirectory(meta);await mkdir(dir,{recursive:true});
  await enrollAttempt(runDir,meta,plan);
  process.env.F3_ATTEMPT_ID=meta.attemptId;process.env.F3_SOURCE_HASH=sourceHash;
  // Captures the actual normal process exit AFTER H.summary. Signals without a
  // normal exit are captured by the full orchestrator's child close event.
  process.once('exit',code=>writeFileSync(path.join(dir,'exit.json'),JSON.stringify({...meta,exitCode:code,signal:null,timedOut:false},null,2)+'\n'));
  console.log('FRAGMENT_IDENTITY '+JSON.stringify(meta));
  const begin=Date.now();let result;
  try {result=await runCase(id);}catch(error){result={id,ok:false,checks:0,error:error.stack||String(error)};}
  assertFrozen(frozen,await captureEpoch(ROOT));
  const evidence={...result,...meta,reportPath:result.report?.reportPath||path.join(dir,'report.json'),durationMs:Date.now()-begin};
  await writeFile(path.join(dir,'case.json'),JSON.stringify(evidence,null,2)+'\n',{flag:'wx'});
  return evidence;
}
async function execute(id,sourceHash){
  const meta=identity(id,sourceHash,plan.attempts.find(x=>x.id===id).attemptId),dir=evidenceDirectory(meta);await mkdir(dir,{recursive:true});
  await enrollAttempt(runDir,meta,plan);
  return new Promise(resolve=>{
    const proc=spawn(process.execPath,[SELF,'--case',id],{cwd:ROOT,detached:true,stdio:['ignore','pipe','pipe'],env:{...process.env,F3_RUN_ID:runId,F3_ATTEMPT_ID:meta.attemptId,F3_SOURCE_HASH:sourceHash,E2E_RESULTS_DIR:dir}});
    children.add(proc);let output='COMMAND '+JSON.stringify({cwd:ROOT,argv:[process.execPath,SELF,'--case',id],identity:meta})+'\n',timedOut=false;
    const terminate=()=>{try{process.kill(-proc.pid,'SIGTERM');}catch{ /* settled group */ }};
    const hard=setTimeout(()=>{timedOut=true;terminate();},52000);
    const force=setTimeout(()=>{if(timedOut||interrupted)try{process.kill(-proc.pid,'SIGKILL');}catch{ /* settled */ }},54000);
    proc.stdout.on('data',d=>{output+=d;});proc.stderr.on('data',d=>{output+=d;});
    proc.once('error',e=>{output+=String(e);});
    proc.once('close',async(code,signal)=>{
      clearTimeout(hard);clearTimeout(force);children.delete(proc);try{process.kill(-proc.pid,'SIGKILL');}catch{/* group already settled */}
      const exit={...meta,exitCode:code,timedOut,signal,dir};
      output+='\nCHILD_EXIT='+code+' SIGNAL='+(signal||'none')+'\n';
      try{
        await writeFile(path.join(dir,'full.log'),output);
        await writeFile(path.join(dir,'exit.json'),JSON.stringify(exit,null,2)+'\n');
        resolve(exit);
      }catch(error){resolve({...exit,writeError:String(error)});}
    });
  });
}
// Completion/identity is distinct from PASS: a genuine red child is collected.
function validateCompletion(selected,evidence,exit,expected) {
  for (const record of [selected,evidence,exit]) {
    assert.ok(record,'Missing completed record');
    for (const key of ['suite','id','runId','attemptId','cwd','mode','authority','sourceHash','sourceCodeSHA','fullSourceArtifactSHA','artifactEpoch']) {
      assert.ok(expected[key],'Expected completion '+key+' required');
      assert.equal(record[key],expected[key],'Exact completed attempt '+key);
    }
  }
  assert.ok(Number.isInteger(exit.exitCode) || (exit.exitCode === null && typeof exit.signal === 'string'),'Actual settled exit required');
  assert.ok(exit.signal === null || typeof exit.signal === 'string','Actual signal required');
  assert.equal(typeof exit.timedOut,'boolean','Actual deadline required');
  assert.equal(typeof evidence.ok,'boolean','Actual case result required');
  assert.ok(Number.isInteger(evidence.checks) && evidence.checks >= 0,'Actual assertion count required');
}
async function aggregate(sourceHash,exits){
  assertFrozen(frozen,await captureEpoch(ROOT));
  validateManifest(plan,CASES.map(x=>x.id),await readEnrollments(runDir),exits);
  let complete=true,passed=0,failed=0;const collected=[];
  for(const {id} of CASES){
    let evidence,exit,dir;
    try{
      const selected=plan.attempts.find(x=>x.id===id);
      if(!selected)throw new Error('No actual child completion');
      dir=evidenceDirectory(selected);
      evidence=JSON.parse(await readFile(path.join(dir,'case.json'),'utf8'));
      exit=JSON.parse(await readFile(path.join(dir,'exit.json'),'utf8'));
      for(const record of [selected,evidence,exit]){
        if(record.id!==id||record.cwd!==cwd||record.sourceHash!==sourceHash||record.mode!==mode||record.authority!==mode||record.runId!==runId||record.attemptId!==selected.attemptId)throw new Error('Mismatched exact cwd/sourceHash/mode/attempt');
      }
      if(selected.writeError)throw new Error(selected.writeError);
      validateCompletion(selected,evidence,exit,identity(id,sourceHash,selected.attemptId));
    }catch(error){H.bad(id,'Missing/stale/invalid completed fragment: '+error.message);complete=false;failed++;continue;}
    collected.push({id,dir,attemptId:evidence.attemptId,cwd,sourceHash,mode,exitCode:exit.exitCode,signal:exit.signal,timedOut:exit.timedOut,reportPath:evidence.reportPath});
    try {
      validateAttempt(plan.attempts.find(x=>x.id===id),evidence,exit,identity(id,sourceHash,evidence.attemptId));
      passed++;H.ok(id+' ('+evidence.checks+' behavior assertions)');
    } catch(error) { failed++;H.bad(id,evidence.error||error.message); }
  }
  await writeFile(path.join(runDir,'aggregate.json'),JSON.stringify({suite:'frontend',runId,cwd,sourceHash,mode,authority:mode,...epochFields(frozen),registered:CASES.map(x=>x.id),complete,ok:complete&&failed===0&&passed===CASES.length,passed,failed,collected},null,2)+'\n',{flag:'wx'});
}
H.startTimer();
try{
  if(cwd!==ROOT)throw new Error('Frontend runner requires actual repository cwd: '+ROOT);
  frozen=await captureEpoch(ROOT);const sourceHash=frozen.fullSourceArtifactSHA;await mkdir(runDir,{recursive:true});
  if(process.env.F3_SOURCE_HASH && process.env.F3_SOURCE_HASH!==sourceHash)throw new Error('Spawned FULL source/artifact epoch changed');
  const index=process.argv.indexOf('--case');
  const epoch={suite:'frontend',runId,mode,authority:mode,cwd,...frozen,registered:CASES.map(x=>x.id)};
  plan=await planRun(runDir,epoch,index===-1?CASES.map(x=>x.id):[process.argv[index+1]],{fragment:!!process.env.F3_ATTEMPT_ID,aggregate:process.argv.includes('--aggregate'),attemptId:process.env.F3_ATTEMPT_ID});
  if(index!==-1 && !CASES.some(x=>x.id===process.argv[index+1]))throw new Error('Unknown registered case');
  if(index!==-1){
    const id=process.argv[index+1];const result=await fragment(id,sourceHash);
    if(result.ok)H.ok(id+' ('+result.checks+' behavior assertions)');else H.bad(id,result.error);
  }else if(process.argv.includes('--aggregate')){
    // All registered cases must have actual exit facts, not just spawned commands.
    await aggregate(sourceHash);
  }else{
    const concurrency=Number(process.env.F3_CONCURRENCY||2);
    if(!Number.isInteger(concurrency)||concurrency<1||concurrency>6)throw new Error('F3_CONCURRENCY must be 1..6');
    let next=0;const exits=[];
    await Promise.all(Array.from({length:concurrency},async()=>{while(!interrupted&&next<CASES.length){const id=CASES[next++].id;exits.push(await execute(id,sourceHash));}}));
    await aggregate(sourceHash,exits);
  }
  assertFrozen(frozen,await captureEpoch(ROOT));
}catch(error){H.bad('frontend fatal',error.stack||String(error));}
finally{
  for(const proc of children)try{process.kill(-proc.pid,'SIGKILL');}catch{ /* already settled */ }
  if(interrupted)H.bad('frontend interrupted','Collected signal/remaining missing cases fail, never skip');
  process.exitCode=H.summary('frontend')?0:1;
}
