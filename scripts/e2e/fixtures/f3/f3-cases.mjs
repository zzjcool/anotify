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
/* 预期内的 4xx 声明（#335 #4）：真实服务**故意**拒绝的请求，Chrome 会记成 console
 * error，driver 一律当 fatal。用例必须自己声明 {status, path[, exact/pathSuffix], note}，
 * 只放行「这个状态 + 这个请求」；没声明的 4xx/5xx 仍然红。
 * 五类已知的预期拒绝：
 *   · no_local_archive —— 任务没有本地日志归档时 GET …/logs 是 410；
 *   · slug_taken —— playbook 的 slug 已被占用（POST 409）；
 *   · resource_version_conflict —— CAS 旧 rv（409，PATCH/PUT/DELETE 都会出现）；
 *   · values_missing —— render 缺 values（400）；
 *   · version_content_immutable —— install 的版本内容不可变（400）；
 *   · immutable_field —— PATCH 改不可变字段（400）。 */
const LOGS_410=[{status:410,path:'/v1/namespaces/',pathSuffix:'/logs',note:'no_local_archive：任务没有本地日志归档时 GET …/logs 是 410，页面按“无归档”显示'}];
add('scope-bootstrap',bootstrap,{scope:''});
// capability-actions 故意触发三类真实拒绝（每条都断言页面把原因显示出来）：
//   · CAS 409（用例 3 并发写后旧 rv 保存 / 用例 8 并发写后删旧 rv）—— PATCH、DELETE 各一次；
//   · version_content_immutable 400（用例 4 显式同版本改正文）—— POST …/capabilities；
//   · immutable_field 400（用例 8 把整份 spec 塞进 PATCH）—— 精确到 f2a-demo 那一条。
// 声明越窄越好：只放行这个状态 + 这个请求，别的 4xx/5xx 仍然红。
add('capability-actions',capability,{lang:'zh-CN',expectHttpStatus:[
  {status:400,path:'/v1/namespaces/default/capabilities/f2a-demo',exact:true,note:'immutable_field：用例故意 PATCH 不可变字段验证拒绝'},
  {status:400,path:'/v1/namespaces/default/capabilities',exact:true,note:'version_content_immutable：显式同版本改正文必须 400 并给出“换一个版本”的指引'},
  {status:409,path:'/v1/namespaces/default/capabilities/f2a-demo',exact:true,note:'resource_version_conflict：用例 3 并发写后旧 rv 保存必须 409'},
  {status:409,path:'/v1/namespaces/default/capabilities/lint-fix',exact:true,note:'resource_version_conflict：用例 8 并发写后按旧 rv 删除必须 409'},
]});
add('source-actions',source,{expectHttpStatus:[{status:409,path:'/v1/namespaces/default/sources/ui-webhook',exact:true,note:'resource_version_conflict：并发 PATCH 后再写/删，CAS 409 是用例要的拒绝'}]});
// observe-events 故意用「默认 ns 的游标读 prod」验证 cursor 绑定：真实服务回 400
// （continue token 不属于这次查询），Chrome 记一条 console error，必须声明范围。
add('observe-events',d=>observability(d,'events'),{expectHttpStatus:[
  {status:400,path:'/v1/console/events',note:'cursor 绑定 ns+筛选：拿默认 ns 的游标读 prod 必须 400'},
]});
for(const mode of ['index','trace'])add('observe-'+mode,d=>observability(d,mode));
// observe-errors：pending 任务没有日志档案（410 no_local_archive）；并发取消之后再放弃
// 必须 409（task_not_running）——两条都是用例要的拒绝路径。
add('observe-errors',d=>observability(d,'errors'),{expectHttpStatus:[LOGS_410[0],
  {status:409,path:'/v1/namespaces/default/tasks/',pathSuffix:'/abandon',note:'task_not_running：并发取消之后放弃必须 409，页面显示原因且不做乐观终态'},
]});
// observe-message 故意走两条「预期内的 410」：未 grant 的任务没有日志档案
// （no_local_archive），以及产物副本被保留期清掉后的下载（artifact_expired）。
add('observe-message',d=>observability(d,'message'),{expectHttpStatus:[
  {status:410,path:'/v1/namespaces/',pathSuffix:'/logs',note:'no_local_archive：未 grant/无档案的任务取日志必须 410，页面按“无档案”显示'},
  {status:410,path:'/v1/namespaces/default/tasks/',note:'artifact_expired：产物副本被保留期清掉后下载必须 410（元数据仍在）'},
]});
// upload-edit / delete / errors 需要声明预期 4xx（见下面单独的 add），不能在这里重复注册。
add('playbook-install',d=>playbook(d,'install'));
// playbook-matrix 故意触发**真实** playbook_conflict（库里已有行不许改 → 400）：
// dryRun 与真装各一次，两次都必须原子 0 写。
add('playbook-matrix',d=>playbook(d,'matrix'),{expectHttpStatus:[
  {status:400,path:'/v1/namespaces/default/playbooks/project-chain/install',exact:true,note:'playbook_conflict：复用行的 next/targets 与库里不一致 → 400，原子 0 写'},
]});
// upload-edit：slug_taken（POST 409）+ 并发 PUT 的 CAS 409；matrix-12：install 版本内容不可变（400）；
// errors：render 缺 values（400）。都是用例故意触发的拒绝路径，声明范围到具体请求为止。
const PB_409_SLUG={status:409,path:'/v1/namespaces/default/playbooks',exact:true,note:'slug_taken：故意用已存在的 slug 创建验证前端冲突提示'};
const PB_409_CAS={status:409,path:'/v1/namespaces/default/playbooks/project-chain',exact:true,note:'resource_version_conflict：并发写之后的旧 rv 保存/删除必须 409'};
add('playbook-upload-edit',d=>playbook(d,'upload-edit'),{expectHttpStatus:[PB_409_SLUG,PB_409_CAS]});
add('playbook-delete',d=>playbook(d,'delete'),{expectHttpStatus:[PB_409_CAS,
  // 删掉清单之后父资源/installs 组必须真的 404（用例断言"不复活"，这是被测行为）。
  {status:404,path:'/v1/namespaces/default/playbooks/project-chain',exact:true,note:'删除清单后父资源 GET 必须 404'},
  {status:404,path:'/v1/namespaces/default/playbooks/project-chain/installs',exact:true,note:'删除父清单后 installs 组 GET 必须 404'},
]});
add('playbook-errors',d=>playbook(d,'errors'),{expectHttpStatus:[{status:400,path:'/v1/namespaces/default/playbooks/project-chain/render',exact:true,note:'values_missing：render 缺 values 必须 400，页面显示原因'}]});
add('playbook-matrix-12',matrix,{expectHttpStatus:[
  {status:400,path:'/v1/namespaces/default/playbooks/f3-matrix-6/install',exact:true,note:'version_content_immutable：同版本号内容变化必须在 install 被拒'},
  {status:400,path:'/v1/namespaces/default/playbooks/f3-matrix-10/install',exact:true,note:'playbook_conflict：复用行与库里不一致 → 400，原子 0 写'},
  // 装之前先探「这个源的引用还在不在」：404 就是"还没有"，用例据此决定断言分支。
  {status:404,path:'/v1/namespaces/default/sources/',note:'探引用的源是否存在（404 = 尚未安装；每个矩阵格一个源）'},
]});
add('playbook-secret-history',historyMask);
// node-create 的重复登记必须 409 AlreadyExists（页面要保留输入、不再发明 token）。
add('node-create',d=>nodeRetained(d,'create'),{expectHttpStatus:[
  {status:409,path:'/v1/nodes',exact:true,note:'AlreadyExists：重复登记同名节点必须 409，页面保留输入且不签发第二个 token'},
]});
// namespace-system：保留域两组配置都是 CAS —— 旧 rv 保存必须 409，缺 rv 必须 400
// （用例断言的就是这两条拒绝路径，前端要原样显示原因并保住草稿）。
add('node-namespace-system',d=>nodeRetained(d,'namespace-system'),{expectHttpStatus:[
  {status:409,path:'/v1/system/config',exact:true,note:'resource_version_conflict：旧 rv 保存 retention 必须 409'},
  {status:409,path:'/v1/namespaces/default/config',exact:true,note:'resource_version_conflict：旧 rv 保存 namespace vars 必须 409'},
  {status:400,path:'/v1/namespaces/default/config',exact:true,note:'resource_version_required：缺 CAS 令牌必须 400'},
  {status:400,path:'/v1/system/config',exact:true,note:'resource_version_required：缺 CAS 令牌必须 400'},
]});
// node-config：配置保存会拿到一个**真的** resource_version 冲突（用例先并发 PUT 改 rv，
// 再从页面上用旧 rv 保存），页面必须保留草稿并显示 409。
add('node-config',d=>nodeRetained(d,'config'),{expectHttpStatus:[
  {status:409,path:'/v1/nodes/anode-1/config',note:'resource_version_conflict：并发 PUT 之后用旧 rv 保存必须 409'},
]});
for(const mode of ['actions-slots','errors'])add('node-'+mode,d=>nodeRetained(d,mode));
add('node-aba',aba);
// retained-security-permissions：用错误的当前密码移除密码必须 400（账户已有密码，
// 用例先走真实 POST /v1/auth/password 建密码再验这个失败面）。
add('retained-security-permissions',d=>nodeRetained(d,'retained'),{expectHttpStatus:[
  {status:400,path:'/v1/auth/password',exact:true,note:'wrong_current_password：错误的当前密码移除密码必须 400，页面显示失败'},
  {status:401,path:'/v1/auth/me',exact:true,note:'guest：未登录时读 me 必须 401（登录页/CLI 页都靠它判游客）'},
  {status:401,path:'/v1/auth/password/login',exact:true,note:'AUTH1：未知用户/错密码登录必须 401，页面显示服务端 message 且不吞'},
]});
add('opaque-cas-aba',async d=>{
  await d.goto('capabilities');
  const token='rv1:uid/new+object:9007199254740993123 ?&#%';
  const url=await d.page.evaluate(token=>Anotify.deletePath('/v1/namespaces/default/capabilities/opaque',token),token);
  assert.equal(url,'/v1/namespaces/default/capabilities/opaque?resourceVersion='+encodeURIComponent(token));
  assert.equal(new URL(url,d.base).searchParams.get('resourceVersion'),token);
  // entrypoint 必须带 shebang：真实服务对没有 `#!` 的入口脚本回 400 entrypoint_no_shebang
  // （capability.md §3），这条用例测的是 CAS 身份（uid/rv），不是入口校验。
  const body={apiVersion:'anotify/v1',kind:'Capability',metadata:{name:'f3-cas-recreated'},spec:{next:[],run:{entrypoint:'run.sh',files:{'run.sh':'#!/bin/sh\necho unchanged\n'}}}};
  const p='/v1/namespaces/default/capabilities';
  const first=await d.response(p,body,'POST');assert.equal(first.status,201);
  const old=first.body.metadata;
  // DELETE 的成功码：契约是 **204 无响应体**（api/resource.yaml · deleteCapability）。
  // mock 之前回 200+Status 是预览期的旧实现，现在与真实服务一致：
  // 既验状态码也验“没有响应体”，而“确实删掉了”由后面的 recreate-uid / stale-409 证明。
  const removed=await d.response(p+'/f3-cas-recreated?resourceVersion='+encodeURIComponent(old.resourceVersion),null,'DELETE');
  assert.equal(removed.status,204,'DELETE 成功码必须是 204，实际 '+removed.status);
  assert.equal(removed.body,null,'204 不得带回响应体');
  const second=await d.response(p,body,'POST');assert.equal(second.status,201);
  // Finish both independent assertions even if UID is already known wrong.
  const stale=await d.response(p+'/f3-cas-recreated?resourceVersion='+encodeURIComponent(old.resourceVersion),null,'DELETE');
  const kept=await d.response(p+'/f3-cas-recreated');
  assert.notEqual(second.body.metadata.uid,old.uid,'new UID required after same-name recreation');
  assert.equal(stale.status,409);assert.equal(stale.body.kind,'Status');assert.deepEqual(kept.body,second.body);
  d.check('F3-CAS browser actual public POST/DELETE/recreate rejects former identity and opaque URL token roundtrips');
},{expectHttpStatus:[
  // 用**上一个对象身份**的 rv 删同名新对象：409 就是被测行为（防止重创后复用旧身份）。
  {status:409,path:'/v1/namespaces/default/capabilities/f3-cas-recreated',exact:true,note:'resource_version_conflict：旧 uid/rv 的 DELETE 必须 409'},
]});
add('source-auth-dedup',async d=>{
  await d.goto('sources');const secret='F3_PRIVATE_HMAC_SIGNING_KEY',token='f3-signed-hook-token';
  const p='/v1/namespaces/default/sources';
  const created=await d.response(p,{apiVersion:'anotify/v1',kind:'Source',metadata:{name:'f3-signed-hook'},spec:{kind:'webhook',config:{token,secret,sample:{message:'hint'}},targets:[{capability:'notify',params:{text:'$(payload.message)',message:'$(payload.message)'}}]}},'POST');
  assert.equal(created.status,201);assert.equal(created.body.spec.config.secretSet,true);assert.ok(!JSON.stringify(created.body).includes(secret));
  const payload=' {"id":"f3-delivery","payload":{"message":"real"}} ';
  const send=(raw,signature)=>d.page.evaluate(async([url,raw,signature])=>{const r=await fetch(url,{method:'POST',body:raw,headers:signature?{'X-Anotify-Signature':signature}:{}});const text=await r.text();return {status:r.status,body:text?JSON.parse(text):null};},['/v1/hooks/'+token,raw,signature]);
  const signed=raw=>'sha256='+createHmac('sha256',secret).update(raw).digest('hex');
  if(d.mock){
    const before=await d.snapshot();assert.equal((await send(payload)).status,401);
    assert.equal((await send(payload,'sha256='+'0'.repeat(64))).status,401);assert.equal(await d.snapshot(),before);
    const first=await send(payload,signed(payload));assert.equal(first.status,200);assert.equal(first.body.tasks.length,1);
    const modified=payload.replace('real','changed');const duplicate=await send(modified,signed(modified));
    assert.equal(duplicate.body.duplicate,true);assert.equal(duplicate.body.eventId,first.body.eventId);assert.equal(duplicate.body.traceId,first.body.traceId);
    const task=await d.req('/v1/namespaces/default/tasks/'+first.body.tasks[0].taskId);assert.equal(task.spec.params.message,'real');
    const event=(await d.req('/v1/namespaces/default/events')).items.find(e=>e.metadata.name===first.body.eventId);assert.equal(event.spec.deliveryId,'f3-delivery');
    const rotated=await d.req(p+'/f3-signed-hook',{method:'PATCH',body:JSON.stringify({apiVersion:'anotify/v1',kind:'Source',metadata:{resourceVersion:created.body.metadata.resourceVersion},spec:{config:{token:'f3-rotated-hook'}}})});
    assert.equal(rotated.spec.config.secretSet,true);assert.equal((await send(payload,signed(payload))).status,404);
    assert.ok(!(await d.page.locator('body').innerText()).includes(secret));
    d.check('F3-SRC-AUTH actual HMAC raw bytes Unauthorized zero writes; exact duplicate receipt/task identity and credential rotation invalidates old hook');
    return;
  }
  /* 真实 ingress 的鉴权面（#355 起强制验签）：配了 `config.secret` 的源，投递必须带
     `X-Anotify-Signature: sha256=<64 位小写 hex>`，值是 HMAC-SHA256(secret, 原始 body 字节)
     —— 对**实际发出的那个字符串**算（payload 首尾各有一个空格，JSON.stringify 之后再算必假失败）。
     入口顺序：限流 429 → 读 raw 413 → token 定源 404 → 验签 401 → 解 JSON 400 → 接收 200；
     未签名 / 格式错 / 不匹配一律 401（同码同文案、错误体不含 secret），且**不产生任何写入**。
     真实现场能断言的真事：
       ① 未签名 → 401、错签名 → 401，且两次的 Status 逐字段相同（同码 / 同 reason / 同文案，
          服务端不区分缺失与不匹配）；**非法 JSON** 无论未签名还是签名错也都是 401（验签在解 JSON
          之前，先解析会得 400）——未签名/错签名/签名错的非法 JSON 三发都 0 写（连同未签名的
          非法 JSON 共四发，之后快照逐字节相等）；
       ② 正确签名 → 200 + 一个任务；
       ③ 幂等按 delivery id：同一 id 换内容重投 → duplicate:true + 同一 eventId/traceId/taskId，
          生效的仍是**第一次**的字节（任务 params.message 还是 real，changed 没覆盖）；
       ④ 事件投影记下 deliveryId；轮换 token 之后旧 hook 地址 404（LookupHook 在验签之前）——
          旧 token **未签名**也必须 404（先验签会得 401），所以这一发才判得出「定源早于验签」；
          且这次 404 同样 0 写（轮换后的快照逐字节相等）；
       ⑤ 重新加载 sources 页之后仍然看不到 secret 明文（先等列表真的渲染出来再查）。 */
  // 本用例的 0 写断言依赖「整命名空间快照逐字节相等」：**不得**给这条用例挂真实节点夹具
  // （节点心跳会刷新 last_seen_at，快照窗口内就变了 → 0 写断言会 flaky）。
  const before=await d.snapshot();
  const unsigned=await send(payload);
  assert.equal(unsigned.status,401,'配了 secret 的源：未签名必须 401（验签在解 JSON 之前）');
  assert.ok(!JSON.stringify(unsigned.body).includes(secret),'401 错误体不得回显 secret');
  const wrongSig=await send(payload,'sha256='+'0'.repeat(64));
  assert.equal(wrongSig.status,401,'签名不匹配必须 401（与缺失同码同文案）');
  assert.ok(!JSON.stringify(wrongSig.body).includes(secret),'401 错误体不得回显 secret');
  // 「同码同文案」只比状态码是判不出来的：把两次 401 的**响应体**互相比较
  // （不硬编码任何文案字面量），才真正校验「服务端不区分缺失与不匹配」。
  assert.ok(unsigned.body && wrongSig.body && unsigned.body.kind==='Status','两次拒绝都必须返回 Status 对象（空体不许靠 undefined 相等混过去）');
  assert.equal(wrongSig.body.code,unsigned.body.code,'缺失与不匹配必须同状态码');
  assert.equal(wrongSig.body.reason,unsigned.body.reason,'缺失与不匹配必须同 reason（不区分）');
  assert.equal(wrongSig.body.message,unsigned.body.message,'缺失与不匹配必须同文案（不区分）');
  // 验签在解 JSON **之前**的判别性探针：body 本身非法（不是 JSON）→ 先验签得 401，
  // 先解析得 400。payload 是合法 JSON，光看它判不出这个顺序。两半都要：不带签名、签名错。
  const notJson=await send('not-json');assert.equal(notJson.status,401,'未签名的非法 JSON 必须 401：验签在解 JSON 之前（先解析会得 400）');
  assert.equal((await send('not-json','sha256='+'0'.repeat(64))).status,401,'签名错误 + 非法 JSON 也必须 401：先验签（先解析会得 400）');
  assert.equal(await d.snapshot(),before,'未签名/错签名/签名错的非法 JSON 必须 0 写（快照逐字节相等）');
  const first=await send(payload,signed(payload));assert.equal(first.status,200);assert.equal(first.body.tasks.length,1);
  const modified=payload.replace('real','changed');const duplicate=await send(modified,signed(modified));
  assert.equal(duplicate.status,200);assert.equal(duplicate.body.duplicate,true);assert.equal(duplicate.body.eventId,first.body.eventId);assert.equal(duplicate.body.traceId,first.body.traceId);
  // 幂等回执的身份不只是 event/trace：任务也是第一次那一份（不得因为重投多派/换派）。
  assert.equal(duplicate.body.tasks.length,first.body.tasks.length,'重投不得多派任务');
  assert.equal(duplicate.body.tasks[0].taskId,first.body.tasks[0].taskId,'幂等回执必须回放第一次的 taskId');
  const task=await d.req('/v1/namespaces/default/tasks/'+first.body.tasks[0].taskId);assert.equal(task.spec.params.message,'real');
  const event=(await d.req('/v1/namespaces/default/events')).items.find(e=>e.metadata.name===first.body.eventId);assert.equal(event.spec.deliveryId,'f3-delivery');
  const rotated=await d.req(p+'/f3-signed-hook',{method:'PATCH',body:JSON.stringify({apiVersion:'anotify/v1',kind:'Source',metadata:{resourceVersion:created.body.metadata.resourceVersion},spec:{config:{token:'f3-rotated-hook'}}})});
  assert.equal(rotated.spec.config.secretSet,true);
  // 轮换 token 本身就是一次写（source 的 rv 与 hookUrl 都变了）：所以「旧 token 404 不写」的基线
  // 必须在轮换**之后**取。拿顶部投递前的 `before` 比，等于把轮换成功本身判成「404 写了东西」（实测红），
  // 也把中间真正接收的那一发（event/task）算进去——那样只是把断言写死成永远过不了。
  const afterRotation=await d.snapshot();
  // 判别性：旧 token 即使**不带签名**也必须 404（若服务端先验签，未签名会得 401）。这是旧 real 分支
  // 「不存在的 token → 404」那条判别探针的等价物，对象换成「曾经有效、轮换后失效」的 token（更强）。
  assert.equal((await send(payload)).status,404,'轮换后旧 token 未签名也必须 404：token 定源在验签之前（先验签会得 401）');
  const stale=await send(payload,signed(payload));
  assert.equal(stale.status,404,'轮换后旧 token 必须 404（LookupHook 在验签之前）');
  assert.equal(await d.snapshot(),afterRotation,'轮换后旧 token 的两次 404 也必须 0 写（快照逐字节相等）');
  // 页面文本检查必须落在**源创建之后重新加载**的页面上：源创建前 goto 过的 innerText 里
  // 根本不可能出现这个 secret，那种检查是空转。
  await d.goto('sources');
  // goto 只等 domcontentloaded，不等列表渲染（sources 页的 load() 要 initScope + 两次 fetch 之后才 render()），
  // 直接读 innerText 会读到骨架屏 —— 那样的泄漏检查是空转。先等这一行真的渲染出来，再查泄漏。
  await d.page.locator('#src-list .list-row[data-name="f3-signed-hook"]').first().waitFor({state:'visible',timeout:15000});
  assert.ok(await d.page.locator('#src-list .list-row').count()>0,'sources 页必须真的渲染出行（否则下面的泄漏检查是空转）');
  const rendered=await d.page.locator('body').innerText();
  assert.ok(rendered.includes('f3-signed-hook'),'渲染后的页面必须含源名，证明列表不是骨架屏');
  assert.ok(!rendered.includes(secret),'sources 页渲染出该源之后仍不得出现 secret 明文');
  d.check('F3-SRC-AUTH real ingress: HMAC-SHA256 over raw bytes verified before JSON decode (unsigned/wrong sig 401 and zero writes); duplicate delivery keeps first bytes/event/trace identity; rotation invalidates old hook');
},{expectHttpStatus:[
  // 配了 secret 的源：未签名 / 错签名 / 非法 JSON（无论带哪种签名）一律 401（#355 强制验签），都不许留下任何写入。
  {status:401,path:'/v1/hooks/',note:'带 secret 的源：未签名/错签名/非法 JSON 401 且 0 写'},
  // token 定源在验签**之前**：轮换 token 之后旧 hook token 必须 404（这条必须真实命中）。
  {status:404,path:'/v1/hooks/',note:'rotation：轮换 token 之后旧 hook token 必须 404，0 写'},
]});
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
  // 真实 401 会在**每个**并发请求上各自触发一次跳转：后一次同名跳转会打断前一次
  // 导航，Playwright 于是拿到 net::ERR_ABORTED（目标没变）。重试一次即可；断言仍是
  // 「最终落到 login.html」。
  const waitLogin=async page=>{
    try{ await page.waitForURL('**/login.html**',{waitUntil:'domcontentloaded'}); }
    catch(error){ if(!/ERR_ABORTED/.test(String(error))) throw error; await page.waitForURL('**/login.html**',{waitUntil:'domcontentloaded'}); }
  };
  for(const file of GUARDED){
    await d.page.goto(d.base+'/'+file+'.html',{waitUntil:'domcontentloaded'});
    await waitLogin(d.page);
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
},{expectHttpStatus:[
  // 顺序有意义：匹配取**第一条**命中的声明，所以窄的写在前面。
  // 再提交一次错误的用户名/密码：密码登录本来就该 401 并把服务端错误显示出来。
  {status:401,path:'/v1/auth/password/login',exact:true,note:'错误凭据的密码登录必须 401，页面显示服务端 error 文案'},
  // 这个用例**故意**用未登录的浏览器访问受保护页面：401 就是被测行为（跳登录页）。
  {status:401,path:'/v1/',note:'未登录浏览器：受保护页面上的 /v1 请求一律 401 后跳登录页'},
]});
// 深链里的对象身份来自当前权威：mock 是固定事实，real 是 bootstrap 用真实 API 造出来的同一角色。
// 名字类（source/playbook）两边一致，只有中枢生成的 id（任务/追踪/节点）需要经 d.fx 取。
const queryFor=(file,d)=>file==='message'?'namespace=default&id='+d.fx('msgTask'):file==='trace'?'namespace=default&traceUID='+d.fx('defaultTrace'):file==='node'?'namespace=default&id='+d.fx('node'):file==='source'?'namespace=default&name='+d.fx('source'):file==='playbook'?'namespace=default&id=f3-page-package':'namespace=default';
// receivers：e2e 真实权威**没有 VAPID 密钥**（run_all.sh 只在有 vapid.json 时注入），
// 推送是关闭的 —— 按设计此时页面上不存在 #current-device-card（只在 push 开放分支
// 创建，ux_regression_19_23 断言 card===null），所以 real 锚点必须是"永远存在"的
// 真实控制器输出 #push-count（mock 权威下同样存在，两个权威都不放水）。
const pageAnchors={admin:'#kpi-devices',capabilities:'[data-cap="fixer"]','cli-auth':'#code-input',docs:'#endpoints',events:'#ev-list article',index:'#kpi-total',keys:'#keys-body tr',message:'#task-facts',node:'#node-effective-vars [data-key]',nodes:'[data-node="anode-1"]','passkey-enroll':'#code-input',playbook:'#pb-file-path',playbooks:'#pb-grid [data-slug]',receivers:'#push-count',security:'#passkey-list',source:'#src-name',sources:'#src-list > div[data-name]',trace:'#trace-tasks [data-task]'};
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
    if(file==='login') {
      // 登录后落到首页：真实静态服务刻意把 `…/index.html` 301 到 `…/`（目录 URL，
      // internal/server/static.go），mock 预览没有这条重定向。所以断言落点是
      // **首页本身**（`/<lang>/` 或它的 index.html），而不是某个具体文件名；
      // 仍然要求首页锚点渲染出来（证明是真登录成功，不是停在登录页）。
      const indexDir=lang==='zh-CN'?'/':'/'+lang+'/';
      await d.page.waitForFunction(dir=>location.pathname===dir||location.pathname===dir+'index.html',indexDir);
      await d.page.locator('#kpi-total').waitFor();
      d.check('F3-PAGE '+lang+' login authenticated redirect -> index');
      continue;
    }
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
},{lang,expectHttpStatus:LOGS_410});
for(const file of ['capabilities','sources','nodes','message'])add('views-'+file,async d=>{
  // message 视图会读任务日志：真服务对没有本地归档的任务回 410（no_local_archive）。
  for(const size of [{name:'desktop',width:1440,height:900},{name:'mobile',width:390,height:844}])for(const theme of ['dark','light']){
    await d.page.setViewportSize(size);await d.goto(file,queryFor(file,d));await d.page.evaluate(t=>Anotify.theme.set(t),theme);
    await d.page.locator(pageAnchors[file]).first().waitFor();await d.clean(file+' '+size.name+' '+theme);await d.shot(size.name+'-'+theme);
  }
},{expectHttpStatus:file==='message'?LOGS_410:[]});
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
},{expectHttpStatus:LOGS_410});
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
