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
import { executeWithRetry, createRetryLedger, readRetryRecord, retrySummaryLine, forensicsLines, timeoutLines, writeTimeoutFragment, TIMEOUT, INCOMPLETE } from '../fixtures/f3/f3-retry.mjs';
const SELF=fileURLToPath(import.meta.url),cwd=process.cwd();
// #483：incomplete（子进程死亡/结果缺失）自动重试 1 次，断言失败永不重试；
// 计数进结果 JSON（`retries`）供 parallel_runner 汇总打印，不许静默。
const retryLedger=createRetryLedger();
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
// spawnAttempt 只负责「起一次子进程 + 写 full.log/exit.json」，不判定；
// #483 的分层判定与重试在 f3-retry.mjs（三个 f3 执行器共用同一套口径）。
async function spawnAttempt(meta,dir,id,sourceHash){
  return new Promise(resolve=>{
    const proc=spawn(process.execPath,[SELF,'--case',id],{cwd:ROOT,detached:true,stdio:['ignore','pipe','pipe'],env:{...process.env,F3_RUN_ID:runId,F3_ATTEMPT_ID:meta.attemptId,F3_SOURCE_HASH:sourceHash,E2E_RESULTS_DIR:dir}});
    children.add(proc);let output='COMMAND '+JSON.stringify({cwd:ROOT,argv:[process.execPath,SELF,'--case',id],identity:meta})+'\n',childOutput='',timedOut=false;
    // 单 case 外层预算：默认 52s（本地实测 3–5s 的 10× 余量）。**刻意不读 CI 的
    // F3_CASE_BUDGET_MS**（那里给 ui/i18n 设的 150s）：frontend 一个套件五十个 case、
    // per-suite 预算 360s，单 case 150s 只会把「一个 case 卡住」变成「整套被 SIGKILL、
    // 一条 results 都没有」—— 那正是本轮要消灭的证据丢失。要压预算的本地调试用这个独立开关。
    const budgetMs=Number(process.env.F3_FRONTEND_CASE_BUDGET_MS||52000),startedAt=Date.now();
    const terminate=()=>{try{process.kill(-proc.pid,'SIGTERM');}catch{ /* settled group */ }};
    // #485：发信号之前先把卡点写成 fragment。SIGTERM 会走 harness 的信号钩子
    // （process.exit(1)），子进程没有机会落盘 case.json —— 所以现场必须由父进程留。
    const hard=setTimeout(async()=>{
      timedOut=true;
      try{
        await writeTimeoutFragment({dir,identity:meta,output:childOutput,budgetMs,elapsedMs:Date.now()-startedAt});
      }catch(error){output+='\nTIMEOUT_FRAGMENT_ERROR='+String(error&&error.stack||error)+'\n';}
      terminate();
    },budgetMs);
    const force=setTimeout(()=>{if(timedOut||interrupted)try{process.kill(-proc.pid,'SIGKILL');}catch{ /* settled */ }},budgetMs+2000);
    proc.stdout.on('data',d=>{output+=d;childOutput+=d;});proc.stderr.on('data',d=>{output+=d;childOutput+=d;});
    proc.once('error',e=>{output+=String(e);childOutput+=String(e);});
    proc.once('close',async(code,signal)=>{
      clearTimeout(hard);clearTimeout(force);children.delete(proc);try{process.kill(-proc.pid,'SIGKILL');}catch{/* group already settled */}
      const exit={...meta,exitCode:code,timedOut,signal,dir};
      output+='\nCHILD_EXIT='+code+' SIGNAL='+(signal||'none')+'\n';
      try{
        await writeFile(path.join(dir,'full.log'),output);
        await writeFile(path.join(dir,'exit.json'),JSON.stringify(exit,null,2)+'\n');
        resolve({exit,childOutput});
      }catch(error){resolve({exit:{...exit,writeError:String(error)},childOutput});}
    });
  });
}
async function execute(id,sourceHash,index){
  const meta=identity(id,sourceHash,plan.attempts.find(x=>x.id===id).attemptId),dir=evidenceDirectory(meta);await mkdir(dir,{recursive:true});
  await enrollAttempt(runDir,meta,plan);
  const run=await executeWithRetry({suite:'frontend',id,index,total:CASES.length,dir,spawnAttempt:()=>spawnAttempt(meta,dir,id,sourceHash)});
  retryLedger.record(run.outcome);
  return run.exit;
}
// #485：两次 attempt 的卡点结论要写进失败项（同一步 = 确定性挂点）。
function stepJudgement(retry){
  if(!retry?.attempts?.length)return '';
  const steps=retry.attempts.map(a=>a.lastStep??'(无 PASS 步骤)');
  const pending=retry.attempts.map(a=>a.pendingStep).filter(Boolean);
  const waiting=pending.length?'（正在等「'+pending[0]+'」）':'';
  if(retry.stuckSameStep)return '；两次都停在同一步「'+steps[0]+'」'+waiting;
  if(retry.attempts.length>1)return '；两次停在不同步（'+steps.join(' / ')+'）'+waiting;
  return '；停在「'+steps[0]+'」'+waiting;
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
    let evidence,exit,dir,retry;
    try{
      const selected=plan.attempts.find(x=>x.id===id);
      if(!selected)throw new Error('No actual child completion');
      dir=evidenceDirectory(selected);
      retry=await readRetryRecord(dir);
      evidence=JSON.parse(await readFile(path.join(dir,'case.json'),'utf8'));
      exit=JSON.parse(await readFile(path.join(dir,'exit.json'),'utf8'));
      for(const record of [selected,evidence,exit]){
        if(record.id!==id||record.cwd!==cwd||record.sourceHash!==sourceHash||record.mode!==mode||record.authority!==mode||record.runId!==runId||record.attemptId!==selected.attemptId)throw new Error('Mismatched exact cwd/sourceHash/mode/attempt');
      }
      if(selected.writeError)throw new Error(selected.writeError);
      validateCompletion(selected,evidence,exit,identity(id,sourceHash,selected.attemptId));
    }catch(error){
      complete=false;failed++;
      // 重试后仍无结论：红，且原因必须能一句话读懂（#483 验收：`incomplete ×2`；
      // #485 验收：`timeout ×2` + 两次都停在同一步）。
      if(retry?.kind===INCOMPLETE)H.bad(id,'incomplete ×'+retry.attempts.length+'：'+retry.why);
      else if(retry?.kind===TIMEOUT)H.bad(id,'timeout ×'+retry.attempts.length+'：'+retry.why+stepJudgement(retry));
      else H.bad(id,'Missing/stale/invalid completed fragment: '+error.message);
      continue;
    }
    collected.push({id,dir,attemptId:evidence.attemptId,cwd,sourceHash,mode,exitCode:exit.exitCode,signal:exit.signal,timedOut:exit.timedOut,reportPath:evidence.reportPath,retried:Boolean(retry?.retried),retryKind:retry?.kind||null});
    try {
      validateAttempt(plan.attempts.find(x=>x.id===id),evidence,exit,identity(id,sourceHash,evidence.attemptId));
      passed++;H.ok(id+' ('+evidence.checks+' behavior assertions)'+(retry?.recovered?' [首次 incomplete → 重试通过]':''));
    } catch(error) {
      failed++;
      const timeout=retry?.kind===TIMEOUT;
      H.bad(id,(timeout?'timeout ×'+retry.attempts.length+'：'+retry.why+stepJudgement(retry):(evidence.error||error.message)));
    }
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
    await Promise.all(Array.from({length:concurrency},async()=>{while(!interrupted&&next<CASES.length){const index=next+1,id=CASES[next++].id;exits.push(await execute(id,sourceHash,index));}}));
    await aggregate(sourceHash,exits);
  }
  assertFrozen(frozen,await captureEpoch(ROOT));
}catch(error){H.bad('frontend fatal',error.stack||String(error));}
finally{
  for(const proc of children)try{process.kill(-proc.pid,'SIGKILL');}catch{ /* already settled */ }
  if(interrupted)H.bad('frontend interrupted','Collected signal/remaining missing cases fail, never skip');
  const retryStats=retryLedger.stats();
  console.log(retrySummaryLine('frontend',retryStats));
  for(const line of timeoutLines(retryStats))console.log(line);
  for(const line of forensicsLines(retryStats))console.log(line);
  H.setRetryStats(retryStats);
  process.exitCode=H.summary('frontend')?0:1;
}
