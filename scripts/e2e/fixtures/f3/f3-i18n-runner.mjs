import { captureEpoch, epochFields, assertFrozen, validateAttempt, planRun, enrollAttempt, readEnrollments, validateManifest } from './f3-epoch.mjs';
// Existing standalone H.summary/result JSON contract, complete bounded locale groups.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { mkdir, readFile, writeFile, readdir } from 'node:fs/promises';
import { writeFileSync } from 'node:fs';
import path from 'node:path';
import * as H from '../../lib/harness.mjs';
import { ROOT, TEAM, isMock, evidenceDirectory } from './f3-driver.mjs';
import { LOCALE_OUT, casesFor, runLocaleCase } from './f3-i18n-cases.mjs';
export async function localeFingerprint(suite) { return (await captureEpoch(ROOT)).fullSourceArtifactSHA; }
export function validateLocaleEvidence(selected,result,exit,expected) {
  for(const record of [selected,result,exit])for(const key of ['suite','id','runId','attemptId','cwd','sourceHash','mode','authority'])assert.equal(record[key],expected[key],'Exact evidence '+key);
  assert.equal(exit.exitCode,0,'Actual child exit must be zero');assert.equal(exit.signal,null);assert.equal(exit.timedOut,false);
  assert.equal(result.ok,true);assert.ok(result.checks>0,'No no-op green');assert.ok(result.report,'Browser report required');assert.deepEqual(result.report.errors,[],'Browser console/pageerror/net failures not waived');
  if(expected.pages){assert.deepEqual(result.report.pages.map(x=>x.page),expected.pages,'Every registered page in fixed order');for(const p of result.report.pages){assert.equal(p.lang,expected.lang);assert.equal(p.attempted,true);assert.equal(p.ok,true);}}
}
export async function localeMain(suite,self) {
  H.startTimer();const mode=isMock()?'explicit-mock':'real',runId=process.env.F3_RUN_ID||randomUUID(),cwd=process.cwd(),cases=casesFor(suite),children=new Set();
  let interrupted=false;const signal=()=>{interrupted=true;for(const child of children){try{process.kill(-child.pid,'SIGTERM');}catch{/* settled */}setTimeout(()=>{try{process.kill(-child.pid,'SIGKILL');}catch{/* settled */}},1200).unref();}};
  for(const sig of ['SIGINT','SIGTERM'])process.on(sig,signal);
  const runDir=path.join(LOCALE_OUT,'runs',runId,mode,suite);
  let frozen, plan;
  const meta=(id,sourceHash,attemptId)=>({suite,id,runId,attemptId,cwd,...epochFields(frozen),mode,authority:mode});
  const directory=m=>evidenceDirectory({...m,id:suite+'-'+m.id},LOCALE_OUT);
  async function enroll(identity) { await enrollAttempt(runDir, identity, plan); }
  async function fragment(id,sourceHash) {
    assert.ok(cases.some(x=>x.id===id),'Unknown registered case: '+id);
    const identity=meta(id,sourceHash,process.env.F3_ATTEMPT_ID||randomUUID()),dir=directory(identity);await mkdir(dir,{recursive:true});
    await enroll(identity);
    process.env.F3_ATTEMPT_ID=identity.attemptId;process.env.F3_SOURCE_HASH=sourceHash;
    process.once('exit',code=>writeFileSync(path.join(dir,'exit.json'),JSON.stringify({...identity,exitCode:code,signal:null,timedOut:false},null,2)+'\n'));
    console.log('COMMAND '+JSON.stringify({cwd,argv:[process.execPath,self,'--fragment',id],...identity}));
    const begin=Date.now();const result=await runLocaleCase(suite,id);
    assertFrozen(frozen, await captureEpoch(ROOT));
    const evidence={...result,...identity,durationMs:Date.now()-begin};
    await writeFile(path.join(dir,'case.json'),JSON.stringify(evidence,null,2)+'\n',{flag:'wx'});
    await mkdir(path.join(runDir,id),{recursive:true});
    return evidence;
  }
  async function execute(id,sourceHash) {
    const identity=meta(id,sourceHash,plan.attempts.find(x=>x.id===id).attemptId),dir=directory(identity);await mkdir(dir,{recursive:true});await enroll(identity);
    return new Promise(resolve=>{
      const child=spawn(process.execPath,[self,'--fragment',id],{cwd:ROOT,detached:true,stdio:['ignore','pipe','pipe'],env:{...process.env,F3_RUN_ID:runId,F3_ATTEMPT_ID:identity.attemptId,F3_SOURCE_HASH:sourceHash,E2E_RESULTS_DIR:dir}});
      children.add(child);let output='COMMAND '+JSON.stringify({cwd:ROOT,argv:[process.execPath,self,'--fragment',id],...identity})+'\n',timedOut=false;
      const kill=sig=>{try{process.kill(-child.pid,sig);}catch{/* group settled */}};
      const soft=setTimeout(()=>{timedOut=true;kill('SIGTERM');},52000),hard=setTimeout(()=>{if(timedOut||interrupted)kill('SIGKILL');},54000);
      child.stdout.on('data',chunk=>{output+=chunk;});child.stderr.on('data',chunk=>{output+=chunk;});child.once('error',error=>{output+=String(error);});
      child.once('close',async(code,sig)=>{
        clearTimeout(soft);clearTimeout(hard);children.delete(child);try{process.kill(-child.pid,'SIGKILL');}catch{/* group already settled */}const exit={...identity,exitCode:code,signal:sig,timedOut};
        output+='\nCHILD_EXIT='+code+' SIGNAL='+(sig||'none')+'\n';
        try{await writeFile(path.join(dir,'full.log'),output);await writeFile(path.join(dir,'exit.json'),JSON.stringify(exit,null,2)+'\n');resolve(exit);}
        catch(error){resolve({...exit,writeError:String(error)});}
      });
    });
  }
  async function aggregate(sourceHash,exits) {
    assertFrozen(frozen, await captureEpoch(ROOT));
    validateManifest(plan, cases.map(x=>x.id), await readEnrollments(runDir), exits);
    const collected=[],missing=[];let passed=0,failed=0,pagePassed=0,pageFailed=0,pageAttempted=0;
    for(const item of cases){
      let selected,result,exit,dir;
      try {
        const attempts=(await readdir(path.join(runDir,item.id))).filter(x=>x.startsWith('attempt-')&&x.endsWith('.json'));
        assert.equal(attempts.length,1,'Exactly one attempt per registered ID in final epoch; duplicate or retry cannot erase failure');
        selected=JSON.parse(await readFile(path.join(runDir,item.id,attempts[0]),'utf8'));
        if(exits){const matches=exits.filter(x=>x.id===item.id);assert.equal(matches.length,1,'Exactly one settled child');assert.equal(matches[0].attemptId,selected.attemptId);}
        assert.ok(selected,'Missing settled child');dir=directory(selected);
        result=JSON.parse(await readFile(path.join(dir,'case.json'),'utf8'));exit=JSON.parse(await readFile(path.join(dir,'exit.json'),'utf8'));
        if(selected.writeError)throw new Error(selected.writeError);
        // Count observed page attempts separately, even for a red case.
        for(const p of result.report?.pages||[]){if(p.attempted)pageAttempted++;if(p.ok)pagePassed++;else pageFailed++;}
        validateAttempt(selected, result, exit, meta(item.id, sourceHash, selected.attemptId));
        validateLocaleEvidence(selected,result,exit,{...meta(item.id,sourceHash,selected.attemptId),pages:item.pages,lang:item.lang});
        passed++;H.ok(item.id+' ('+result.checks+' actual assertions)');
      }catch(error){failed++;if(!result||!exit)missing.push(item.id);H.bad(item.id,result?.error||String(error));}
      collected.push({id:item.id,lang:item.lang,pages:item.pages,dir,attemptId:selected?.attemptId,exitCode:exit?.exitCode,signal:exit?.signal,timedOut:exit?.timedOut,ok:result?.ok??false,checks:result?.checks??0,error:result?.error});
    }
    const aggregateAttempt=randomUUID();const record={suite,runId,mode,authority:mode,cwd,...epochFields(frozen),attemptId:aggregateAttempt,registered:cases.map(x=>x.id),complete:missing.length===0,ok:missing.length===0&&failed===0,missing,passed,failed,unrun:missing.length,registeredPages:76,pageAttempted,pagePassed,pageFailed,collected};
    const file=path.join(runDir,'aggregate-'+aggregateAttempt+'.json');await writeFile(file,JSON.stringify(record,null,2)+'\n',{flag:'wx'});await writeFile(path.join(runDir,'latest-aggregate.json'),JSON.stringify({file,...record},null,2)+'\n');
    console.log('AGGREGATE '+JSON.stringify({file,passed,failed,unrun:missing.length,pageAttempted,pagePassed,pageFailed,mode,sourceHash}));
  }
  try {
    assert.equal(cwd,ROOT,'Exact repository cwd required');assert.match(runId,/^[a-zA-Z0-9_-]+$/);
    frozen=await captureEpoch(ROOT);const sourceHash=frozen.fullSourceArtifactSHA;await mkdir(runDir,{recursive:true});
    if(process.env.F3_SOURCE_HASH)assert.equal(process.env.F3_SOURCE_HASH,sourceHash,'Spawned source must remain frozen across complete epoch');
    const epoch={suite,runId,mode,authority:mode,cwd,...epochFields(frozen),registered:cases.map(x=>x.id)};
    const selectedIndex=process.argv.indexOf('--case');
    plan=await planRun(runDir,{...epoch,...frozen}, selectedIndex===-1?cases.map(x=>x.id):[process.argv[selectedIndex+1]], {fragment:process.argv.includes('--fragment'),aggregate:process.argv.includes('--aggregate')});
    try{await writeFile(path.join(runDir,'epoch.json'),JSON.stringify(epoch,null,2)+'\n',{flag:'wx'});}
    catch(error){if(error.code!=='EEXIST')throw error;assert.deepEqual(JSON.parse(await readFile(path.join(runDir,'epoch.json'),'utf8')),epoch,'No source/mode/cwd/registration mixing in epoch');}
    console.log('SUITE '+suite+' authority='+mode+' registered='+cases.length+' page/lang=19x4 sourceHash='+sourceHash);
    const fragmentIndex=process.argv.indexOf('--fragment'),index=process.argv.indexOf('--case');
    if(fragmentIndex!==-1){assert.ok(process.env.F3_ATTEMPT_ID,'Internal fragment requires parent attempt');const result=await fragment(process.argv[fragmentIndex+1],sourceHash);if(result.ok)H.ok(result.id+' ('+result.checks+' assertions)');else H.bad(result.id,result.error);}
    else if(index!==-1){
      const id=process.argv[index+1];assert.ok(cases.some(x=>x.id===id),'Unknown registered ID');
      const exit=await execute(id,sourceHash),dir=directory(exit);
      console.log(await readFile(path.join(dir,'full.log'),'utf8'));
      const result=JSON.parse(await readFile(path.join(dir,'case.json'),'utf8'));
      const item=cases.find(x=>x.id===id);
      validateAttempt(exit, result, JSON.parse(await readFile(path.join(dir,'exit.json'),'utf8')), meta(id,sourceHash,exit.attemptId));validateLocaleEvidence(exit,result,JSON.parse(await readFile(path.join(dir,'exit.json'),'utf8')),{...meta(id,sourceHash,exit.attemptId),pages:item.pages,lang:item.lang});
      H.ok(id+' ('+result.checks+' assertions, actual child exit '+exit.exitCode+')');
    }else if(process.argv.includes('--aggregate'))await aggregate(sourceHash);
    else{
      const concurrency=Number(process.env.F3_CONCURRENCY||2);assert.ok(Number.isInteger(concurrency)&&concurrency>=1&&concurrency<=6,'Concurrency 1..6');
      let next=0;const exits=[];await Promise.all(Array.from({length:concurrency},async()=>{while(!interrupted&&next<cases.length)exits.push(await execute(cases[next++].id,sourceHash));}));
      await aggregate(sourceHash,exits);
    }
    assertFrozen(frozen, await captureEpoch(ROOT));
  }catch(error){H.bad(suite+' fatal',error.stack||String(error));}
  finally {
    for(const child of children)try{process.kill(-child.pid,'SIGKILL');}catch{/* settled */}
    for(const sig of ['SIGINT','SIGTERM'])process.off(sig,signal);
    if(interrupted)H.bad(suite+' interrupted','Unattempted/aborted groups fail aggregate, no skip');
    process.exitCode=H.summary(suite)?0:1;
  }
}
