// F3 shared browser driver. Adapted from F2b2/F2c1/F2c2 self-host/early recorder.
// Explicit fixture authority only. Real mode always H.startServer, never fallback.
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { createServer } from 'node:http';
import { execFileSync } from 'node:child_process';
import { readFile, mkdir, writeFile } from 'node:fs/promises';
import { existsSync, readdirSync, readFileSync, rmSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { homedir } from 'node:os';
import path from 'node:path';
import { chromium } from 'playwright-core';
import * as H from '../../lib/harness.mjs';
import { installTraceEvidence, traceEvidenceTools } from './f3-trace-evidence.mjs';
import { bringUpFixtureNode, deployApplied, capRef } from './f3-node.mjs';
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
// ---------------------------------------------------------------------------
// Real-mode fixture bootstrap (#335 B2). Genuine HTTP only, idempotent, and it
// never writes product tables or papers over a product gap: every fixture below
// goes through the same public handler the browser controller talks to.
// devseed supplies *auth/namespace authority* only (a session cookie per role),
// which is what it exists for — no capability/source/event/task rows from it.
// ---------------------------------------------------------------------------
export const PROD = 'prod';
export const DEFAULT_NS = 'default';
const sessionCookie = (session) => ({ Cookie: 'anotify_session=' + session });
const jsonHeaders = (session) => ({ ...sessionCookie(session), 'Content-Type': 'application/json' });

// ensureSession 用 devseed 建一个真实账号 + 会话（幂等：用户名已存在则复用并提权/降权）。
function ensureSession(server, username, extraArgs = []) {
  assert.ok(existsSync(H.DEVSEED_BIN), `T1 seam: DEVSEED_BIN missing: ${H.DEVSEED_BIN}`);
  const out = execFileSync(H.DEVSEED_BIN, ['-db', server.dbPath, '-username', username, ...extraArgs], { encoding: 'utf8' });
  const get = (k) => (out.match(new RegExp(`^${k}=(.*)$`, 'm')) || [])[1];
  assert.ok(get('SESSION'), `T1 seam: devseed must return SESSION for ${username}`);
  return { session: get('SESSION'), uid: get('UID'), role: get('ROLE'), sendKey: get('SEND_KEY'), recvKey: get('RECV_KEY') };
}

// ensureNamespace 保证 default/prod 两个命名空间存在（契约 resource.yaml ·
// NamespaceWrite：创建只需 metadata.name，spec 恒 {}）。已存在就复用，绝不复写。
export async function ensureNamespace(server, session, name) {
  const read = async () => {
    const list = await rawReq(server.base, '/v1/namespaces', { headers: sessionCookie(session) });
    assert.equal(list.status, 200, 'T1 seam: genuine namespaced API bootstrap must respond 200');
    assert.equal(list.body.kind, 'NamespaceList', 'T1 seam: legacy namespace adapter is forbidden');
    return list.body.items || [];
  };
  const items = await read();
  const found = items.find((n) => n.metadata.name === name);
  if (found) return { namespace: found, created: false };
  const created = await rawReq(server.base, '/v1/namespaces', {
    method: 'POST',
    headers: jsonHeaders(session),
    body: JSON.stringify({ apiVersion: 'anotify/v1', kind: 'Namespace', metadata: { name }, spec: {} }),
  });
  // 409 AlreadyExists 只可能来自并发建同名 ns：重读一次即可，不是失败。
  assert.ok([201, 409].includes(created.status), `T1 seam: create ${name} Namespace must be 201, got ${created.status} ${JSON.stringify(created.body)}`);
  const after = await read();
  const ns = after.find((n) => n.metadata.name === name);
  assert.ok(ns, `T1 seam: ${name} Namespace fixture required after create`);
  return { namespace: ns, created: created.status === 201 };
}

// ensureNamespaceVars 给命名空间写一层真实的 ns 级变量（`PUT …/config`，CAS 必须带
// 当前 rv）。节点上下文用例要把「切到 prod 读到的有效值」验成真实数据，而不是只验
// mock 种子里那句 'prod-visible'。
async function ensureNamespaceVars(server, session, ns, vars) {
  const path = `/v1/namespaces/${ns}/config`;
  const current = await rawReq(server.base, path, { headers: sessionCookie(session) });
  assert.equal(current.status, 200, `T1 seam: GET ${ns} config must be 200`);
  if (JSON.stringify(current.body.vars || {}) === JSON.stringify(vars)) return current.body;
  const saved = await rawReq(server.base, path, {
    method: 'PUT',
    headers: jsonHeaders(session),
    body: JSON.stringify({ metadata: current.body.metadata, vars }),
  });
  assert.equal(saved.status, 200, `T1 seam: PUT ${ns} vars must be 200, got ${saved.status} ${JSON.stringify(saved.body)}`);
  return saved.body;
}

// realBootstrap 是 real 模式唯一的前置：会话（admin + member）与 default/prod
// 命名空间。返回给 driver 的 fixtures，后续造数（capability/source/event/task…）
// 也走真实 API 叠在它上面。
async function realBootstrap(server) {
  const member = ensureSession(server, 'f3');
  // 管理员会话：admin.html 的数据面是 /v1/admin/*（adminSession 中间件），
  // member 会话保持独立给「角色降级」用例用——从不伪造 role 字段。
  const admin = ensureSession(server, 'f3-admin', ['-admin']);
  assert.equal(admin.role, 'admin', 'T1 seam: devseed -admin must produce an admin session');
  for (const name of [DEFAULT_NS, PROD]) await ensureNamespace(server, admin.session, name);
  // prod 的 ns 级变量（真实 PUT）：节点上下文/A-B-A 用例要把「切到 prod 读到的有效值」
  // 验成真实数据（mock 种子里的同名值就是这个角色的固定事实）。
  await ensureNamespaceVars(server, admin.session, PROD, { NS_ONLY: 'prod-visible' });
  const fixtures = { adminSession: admin.session, memberSession: member.session, sendKey: admin.sendKey || member.sendKey };
  // retained 角色用例要求「真实 member 会话」：它必须来自真实账号，不是改前端状态。
  if (!process.env.E2E_V4_MEMBER_SESSION) process.env.E2E_V4_MEMBER_SESSION = fixtures.memberSession;
  return fixtures;
}

// ensureCapability 幂等地建一个能力（真实 POST；同内容重发只会折叠到已有版本）。
//
// extra 是「造真实执行事实」需要的 spec 段：params（参数 schema）、requires.values
// （run.env 引用 vars.* 时**必须**声明，否则服务端 400 undeclared_need）、lifecycle
// （重试策略）与 run.env（C11 诊断来源层）。全部走产品校验，没有绕过。
async function ensureCapability(server, session, ns, name, next = [], extra = {}) {
  const list = await rawReq(server.base, `/v1/namespaces/${ns}/capabilities`, { headers: sessionCookie(session) });
  assert.equal(list.status, 200, `T1 seam: GET ${ns} capabilities must be 200`);
  const found = (list.body.items || []).find((c) => c.metadata.name === name);
  if (found) return found;
  const spec = {
    next: next,
    run: {
      entrypoint: 'run.sh',
      files: { 'run.sh': '#!/bin/sh\necho f3 fixture ' + name + '\n' },
      ...(extra.env ? { env: extra.env } : {}),
    },
    ...(extra.params ? { params: extra.params } : {}),
    ...(extra.requires ? { requires: extra.requires } : {}),
    ...(extra.lifecycle ? { lifecycle: extra.lifecycle } : {}),
    ...(extra.concurrency ? { concurrency: extra.concurrency } : {}),
  };
  const created = await rawReq(server.base, `/v1/namespaces/${ns}/capabilities`, {
    method: 'POST',
    headers: jsonHeaders(session),
    body: JSON.stringify({ apiVersion: 'anotify/v1', kind: 'Capability', metadata: { name }, spec }),
  });
  assert.equal(created.status, 201, `T1 seam: create ${ns}/${name} capability must be 201, got ${created.status} ${JSON.stringify(created.body)}`);
  return created.body;
}

// ensureSource 幂等地建一个 webhook 源（真实 POST）。形状和 mock 的 `deploy-hook`
// 种子事实一致：kind=webhook + token/secret/sample —— 详情页要验的 hookUrl /
// secretSet 掩码只有 webhook 源才有（用 manual 源等于把那些断言测空）。
// 故意不带 payloadSchema：机器入口 POST …/events 会按 payloadSchema 校验载荷，
// 夹具投递的是固定小载荷，加 schema 只会让 fixture 自己先被 400 拒掉。
async function ensureSource(server, session, ns, name, target) {
  const list = await rawReq(server.base, `/v1/namespaces/${ns}/sources`, { headers: sessionCookie(session) });
  assert.equal(list.status, 200, `T1 seam: GET ${ns} sources must be 200`);
  const found = (list.body.items || []).find((s) => s.metadata.name === name);
  if (found) return found;
  const created = await rawReq(server.base, `/v1/namespaces/${ns}/sources`, {
    method: 'POST',
    headers: jsonHeaders(session),
    body: JSON.stringify({
      apiVersion: 'anotify/v1', kind: 'Source', metadata: { name },
      spec: {
        kind: 'webhook',
        config: { token: 'f3-hook-token', secret: 'f3-hook-hmac-secret', sample: { f3: 'fixture' } },
        targets: [{ capability: target, params: {} }],
      },
    }),
  });
  assert.equal(created.status, 201, `T1 seam: create ${ns}/${name} source must be 201, got ${created.status} ${JSON.stringify(created.body)}`);
  return created.body;
}

// ensureNode 幂等地登记节点（真实 POST /v1/nodes；不返回任何令牌明文）。
async function ensureNode(server, session, name, spec) {
  const list = await rawReq(server.base, '/v1/nodes', { headers: sessionCookie(session) });
  assert.equal(list.status, 200, 'T1 seam: GET nodes must be 200');
  const found = (list.body.items || []).find((n) => n.metadata.name === name);
  if (found) return found;
  const created = await rawReq(server.base, '/v1/nodes', {
    method: 'POST',
    headers: jsonHeaders(session),
    // vars 是页面的有效值锚点（#node-effective-vars [data-key]）：没有变量时该区块
    // 正常渲染为空，用例的锚点就永远等不到——所以 fixture 里给一条真实生效的变量。
    body: JSON.stringify({ apiVersion: 'anotify/v1', kind: 'Node', metadata: { name }, spec: spec || { namespaces: [DEFAULT_NS], vars: { F3_FIXTURE: '1' } } }),
  });
  assert.equal(created.status, 201, `T1 seam: create node ${name} must be 201, got ${created.status} ${JSON.stringify(created.body)}`);
  return created.body;
}

// fireFixture 用真实入口（POST /v1/namespaces/{ns}/events + 会话）投递一条事件，
// 让中枢自己长出事件/追踪/任务——不插事件、任务行。
async function fireFixture(server, key, ns, source) {
  // 事件入口是**机器入口**（Bearer API Key，没有会话 Cookie）：这是契约 §6.2 的
  // 唯一投递路径，所以 fixture 也走它，不带 Cookie。
  const receipt = await rawReq(server.base, `/v1/namespaces/${ns}/events`, {
    method: 'POST',
    headers: { Authorization: 'Bearer ' + key, 'Content-Type': 'application/json' },
    body: JSON.stringify({ source, payload: { f3: 'fixture' } }),
  });
  assert.equal(receipt.status, 200, `T1 seam: ingest ${ns}/${source} must be 200, got ${receipt.status} ${JSON.stringify(receipt.body)}`);
  const tasks = receipt.body.tasks || [];
  assert.ok(tasks.length > 0, `T1 seam: ingest ${ns}/${source} must produce a Task`);
  return { eventId: receipt.body.eventId, traceId: receipt.body.traceId, taskId: tasks[0].taskId };
}

// MOCK_FIXTURES 是 mock 权威下的固定事实（web/mock.js 的种子数据）。real 权威下
// 同名角色由真实创建的对象承担：capability/source/node 的**名字**是我们自己选的，
// 只有中枢生成的 id（任务/追踪/节点 id）不可预知，所以经 d.fx(key) 注入。
export const MOCK_FIXTURES = Object.freeze({
  msgTask: 'tsk_mock_2',
  // mock 种子里「还没 grant、可以被取消」的那个任务（observe-errors 的 CAS 对象）。
  pendingTask: 'tsk_mock_5',
  // 在跑/成功的任务只有 real 分支用（mock 分支直接写死 tsk_mock_5/6/2）。
  runningTask: 'tsk_mock_6',
  node: 'anode-1',
  defaultTrace: 'trace_mock_t1',
  prodTrace: 'trace_mock_t2',
  prodTask: 'tsk_mock_8',
  capability: 'fixer',
  source: 'deploy-hook',
});

async function realFixtures(server, fixtures, caseId = '') {
  const s = fixtures.adminSession;
  await ensureNode(server, s, MOCK_FIXTURES.node);
  // 第二个节点：namespace/system 的 per-node 批量更新要验「部分成功 + 另一节点保持不变」，
  // 真正权威下那个"另一节点"必须是真实存在的节点（否则 404 而不是 409/400）。
  await ensureNode(server, s, 'anode-2');
  // 与 mock 种子同构的最小能力图：fixer → analyze-pr（边由 spec.next 派生），
  // 外加 notify（源 target 必须是本命名空间里真实存在的能力，否则建源直接 400
  // invalid_source_config）。
  await ensureCapability(server, s, DEFAULT_NS, 'notify');
  await ensureCapability(server, s, DEFAULT_NS, 'analyze-pr');
  await ensureCapability(server, s, DEFAULT_NS, MOCK_FIXTURES.capability, ['analyze-pr']);
  await ensureSource(server, s, DEFAULT_NS, MOCK_FIXTURES.source, MOCK_FIXTURES.capability);
  const key = fixtures.sendKey;
  assert.ok(key, 'T1 seam: devseed SEND_KEY required for the machine event ingress');
  const def = await fireFixture(server, key, DEFAULT_NS, MOCK_FIXTURES.source);
  await ensureCapability(server, s, PROD, 'analyze-pr');
  await ensureCapability(server, s, PROD, MOCK_FIXTURES.capability, ['analyze-pr']);
  await ensureSource(server, s, PROD, MOCK_FIXTURES.source, MOCK_FIXTURES.capability);
  const prod = await fireFixture(server, key, PROD, MOCK_FIXTURES.source);
  // node.html 的 ?id= 是节点**名字**（nodes.html:113 就是这么生成链接的），
  // 服务端 GET /v1/nodes/{name} 也按名字（resource.yaml:868 NodeNamePath），
  // 所以这里不取 nd_ id——否则真服务一律 404（mock 里 id==name 把差异藏住了）。
  const nodes = await rawReq(server.base, '/v1/nodes', { headers: sessionCookie(s) });
  const node = (nodes.body.items || []).find((n) => n.metadata.name === MOCK_FIXTURES.node);
  assert.ok(node, 'T1 seam: node fixture required');
  assert.equal(node.metadata.name, MOCK_FIXTURES.node);
  const built = { ...fixtures, msgTask: def.taskId, defaultTrace: def.traceId, prodTrace: prod.traceId, prodTask: prod.taskId, pendingTask: def.taskId, node: node.metadata.name };
  // 用例专属的真实事实（每条用例一个独立服务器，所以“同一份事实”必须就地造）。
  if (SCENARIOS.has(caseId)) Object.assign(built, await SCENARIOS.get(caseId)(server, built, key));
  return built;
}

// realParams 把 fixture 的"同名事实"集中成一张表：mock 权威给固定种子，real 权威就地造。
const SCENARIOS = new Map();

// fireMany 连续投递 n 条真实事件（每条都会经中枢长出事件 + 一个根任务）。
async function fireMany(server, key, ns, source, times) {
  const out = [];
  for (let i = 0; i < times; i++) out.push(await fireFixture(server, key, ns, source));
  return out;
}

// ============================ 真实执行事实（观测类用例） ============================
//
// 观测类用例（events/index/trace/message）原本拿 mock 的固定种子当事实。真实权威下
// 这些事实**只能由真实入口长出来**：事件入口 → 任务行 → 节点手上的 WS 会话（accept /
// grant / emit / logs_request / 结果上行）。本段把这些动作按「一个守协议的节点会说什么」
// 发出去，assert 全部基于产品自己写下的行（不插库、不改相位）。

// chainScenario 造一条**多跳链** + 一个真实失败任务 + 一个真实在跑任务：
//
//   chain-hook → chain-root ─emit→ chain-mid ─emit→ chain-leaf
//                          └─emit→ chain-broken（params schema 非法 → param_schema 剪枝）
//
// 三件事实都是产品自己写的：子任务来自 emit 接受事务，剪枝观察来自 route 剪枝行，
// trace 节点数来自 tasks 行。
// capability-actions：用例 4 要验「显式版本 2 的正文不可变 → 400 version_content_immutable」，
// 前提是 analyze-pr **真的有 v2**（mock 种子给了两版；真实权威下得多建一版）。
// 用例 7/8 还依赖 mock 种子能力图的另一头：**lint-fix@1**（fixer.next 里钉死版本的那个下游，
// 用例 8 删的也是它）。两件都是真实入口造得出来的事实，所以就在本用例的服务器上就地造；
// 不动全局夹具（其它用例不依赖 lint-fix，把它们拖进来只会扩大变更面）。
SCENARIOS.set('capability-actions', async (server, fixtures) => {
  const s = fixtures.adminSession;
  const created = await rawReq(server.base, `/v1/namespaces/${DEFAULT_NS}/capabilities`, {
    method: 'POST',
    headers: jsonHeaders(s),
    body: JSON.stringify({
      apiVersion: 'anotify/v1', kind: 'Capability', metadata: { name: 'analyze-pr', version: 2 },
      spec: {
        next: ['notify'],
        run: { entrypoint: 'run.sh', files: { 'run.sh': '#!/bin/sh\necho analyze-pr v2\n' } },
      },
    }),
  });
  assert.equal(created.status, 201, 'T1 seam: analyze-pr v2 必须建得出来，实际 ' + created.status + ' ' + JSON.stringify(created.body));
  // lint-fix v1：用例 7 断言 fixer 的 resolved 里能看到**钉死版本**的那个下游（名字 + v1），
  // 用例 8 的 DELETE CAS 对象也是它。真实权威下它只能是真的存在过。
  const lintFix = await ensureCapability(server, s, DEFAULT_NS, 'lint-fix');
  // fixer.next 补成 mock 种子的那两支（PATCH 的合法面只有 metadata.displayName 与 spec.next，
  // 内容面不可变），否则 resolved 里只有一个裸名下游，钉死版本的那一路永远测不到。
  const fixer = await ensureCapability(server, s, DEFAULT_NS, MOCK_FIXTURES.capability);
  const fixerNext = Array.isArray(fixer.spec && fixer.spec.next) ? fixer.spec.next : [];
  if (!fixerNext.includes('lint-fix@1')) {
    const patched = await rawReq(server.base, `/v1/namespaces/${DEFAULT_NS}/capabilities/${MOCK_FIXTURES.capability}`, {
      method: 'PATCH',
      headers: jsonHeaders(s),
      body: JSON.stringify({
        apiVersion: 'anotify/v1', kind: 'Capability',
        metadata: { name: MOCK_FIXTURES.capability, resourceVersion: fixer.metadata.resourceVersion },
        spec: { next: ['analyze-pr', 'lint-fix@1'] },
      }),
    });
    assert.equal(patched.status, 200, 'T1 seam: fixer.next 必须能 PATCH（displayName/next 是唯一可改面），实际 ' + patched.status + ' ' + JSON.stringify(patched.body));
    assert.deepEqual(patched.body.spec.next, ['analyze-pr', 'lint-fix@1'], 'T1 seam: fixer.next 必须真的改成了带钉死版本的两支');
  }
  // 用例 9 切到 prod 后要看的也是两件 mock 种子同名的真实事实：
  //   · report.send：`next` 里有一个解析不到的 ghost-cap（DownstreamResolved = NotFound）；
  //   · prod-deploy：NodesAvailable = NodeOffline（仓里登记的节点不在线，离线优先于其它原因）。
  await ensureCapability(server, s, PROD, 'report.send', ['ghost-cap']);
  await ensureCapability(server, s, PROD, 'prod-deploy');
  // report.send 的「最近一次运行」：经真实源投一条 prod 事件，让中枢自己长出 report.send 的
  // Task 行（prod 没有能跑它的在线节点，任务就停在 pending —— 那也是真实运行过的一行）。
  await ensureSource(server, s, PROD, 'f3-report-hook', 'report.send');
  const reportFire = await fireFixture(server, fixtures.sendKey, PROD, 'f3-report-hook');
  // 用例 7 的 deploy-preview：mock 种子里是「终端能力 + requires.values 解析不到」。
  // `next: []` 是它真正的测点（终端能力空态）；requires 按种子同样给它一份解析不到的值。
  const deployPreview = await ensureCapability(server, s, DEFAULT_NS, 'deploy-preview', [], {
    requires: { values: ['vars.F3_UNDECLARED_VALUE'] },
  });
  return {
    analyzePrV2: created.body.metadata.version,
    lintFix: lintFix.metadata.version,
    reportTask: reportFire.taskId,
    deployPreview: deployPreview.metadata.version,
  };
});

SCENARIOS.set('observe-events', async (server, fixtures) => {
  // 分页要真的有两页：默认 ns 再投 3 条（连同基础夹具的 1 条 = 4），prod 再投 1 条
  // （连同基础夹具 = 2）。事件/任务全部由真实入口产生，不插行。
  const extra = await fireMany(server, fixtures.sendKey, DEFAULT_NS, MOCK_FIXTURES.source, 3);
  await fireMany(server, fixtures.sendKey, PROD, MOCK_FIXTURES.source, 1);
  return { eventTasks: extra.map((x) => x.taskId) };
});
SCENARIOS.set('observe-index', async (server, fx) => chainScenario(server, fx));
SCENARIOS.set('observe-trace', async (server, fx) => chainScenario(server, fx));

async function chainScenario(server, fixtures) {
  const s = fixtures.adminSession;
  const envRef = (name) => ({ valueFrom: { fieldRef: { fieldPath: 'vars.' + name } } });
  const leaf = await ensureCapability(server, s, DEFAULT_NS, 'chain-leaf');
  const mid = await ensureCapability(server, s, DEFAULT_NS, 'chain-mid', ['chain-leaf']);
  // params schema 非法（type 不是 object）→ 向它的 emit 被 route 以 param_schema 剪枝。
  const broken = await ensureCapability(server, s, DEFAULT_NS, 'chain-broken', [], { params: { type: 'array' } });
  const root = await ensureCapability(server, s, DEFAULT_NS, 'chain-root', ['chain-mid', 'chain-broken'], {
    env: { SOURCE_ROOT: envRef('SOURCE_ROOT'), GIT_TERMINAL_PROMPT: envRef('GIT_TERMINAL_PROMPT') },
    requires: { values: ['vars.SOURCE_ROOT', 'vars.GIT_TERMINAL_PROMPT'] },
    // 幂等 + 3 次上限：这样“失联回收”路径才有重排语义（本用例只用它的读模型）。
    lifecycle: { idempotent: true, maxAttempts: 3, timeoutSec: 60 },
    concurrency: { maxPerNode: 2 },
  });
  await ensureSource(server, s, DEFAULT_NS, 'chain-hook', 'chain-root');
  const node = await bringUpFixtureNode({
    base: server.base,
    session: s,
    name: 'f3-chain-node',
    namespaces: [DEFAULT_NS],
    vars: { SOURCE_ROOT: '/node-default', GIT_TERMINAL_PROMPT: '0' },
    localVars: { SOURCE_ROOT: '/local-src' },
    localLimits: { total: 4, perCapability: { 'chain-root': 2 } },
    slots: 3,
  });
  {
    for (const cap of [root, mid, leaf, broken]) await deployApplied(server.base, s, node, capRef(cap));
    // 1) 多跳链：root → mid → leaf，外加一条被剪枝的 emit。
    const chain = await fireFixture(server, fixtures.sendKey, DEFAULT_NS, 'chain-hook');
    const rootGrant = await node.lease(chain.taskId);
    const midEmit = await node.emit(rootGrant, { to: 'chain-mid', data: { hop: 1 } });
    assert.equal(midEmit?.httpStatus, 200, 'emit 到包内下游必须 200');
    const pruned = await node.emit(rootGrant, { to: 'chain-broken', data: { hop: 'pruned' } });
    assert.equal(pruned?.response?.pruned?.reason, 'param_schema', '非法 params schema 的下游 emit 必须被剪枝');
    assert.equal(await node.result(rootGrant, { summary: '链路起点' }).then(Boolean), true, 'root 结果必须被接受');
    const midTask = midEmit.response.taskId;
    const midGrant = await node.lease(midTask);
    const leafEmit = await node.emit(midGrant, { to: 'chain-leaf', data: { hop: 2 } });
    await node.result(midGrant, { summary: '中间跳' });
    const leafGrant = await node.lease(leafEmit.response.taskId);
    await node.result(leafGrant, { summary: '叶子' });
    // 2) 真实失败任务（终态 failed，终态就是终态：失败结果不重试）。
    const failedFire = await fireFixture(server, fixtures.sendKey, DEFAULT_NS, 'chain-hook');
    const failedGrant = await node.lease(failedFire.taskId);
    await node.result(failedGrant, { phase: 'failed', summary: '脚本失败', error: { reason: 'script_failed', message: 'exit 1' }, exitCode: 1 });
    // 3) 真实在跑任务：拿到 lease 就不回报结果（占住一个真实槽位）。
    const runningFire = await fireFixture(server, fixtures.sendKey, DEFAULT_NS, 'chain-hook');
    await node.lease(runningFire.taskId);
    const events = await rawReq(server.base, '/v1/console/events?namespace=default&limit=8', { headers: sessionCookie(s) });
    assert.equal(events.status, 200, 'T1 seam: console events must be 200');
    const latestWithTrace = (events.body.items || []).find((e) => e.spec.traceUID);
    assert.ok(latestWithTrace, 'T1 seam: console events must carry traceUID');
    const latestTrace = await rawReq(server.base, '/v1/namespaces/default/traces/' + latestWithTrace.spec.traceUID, { headers: sessionCookie(s) });
    assert.equal(latestTrace.status, 200, 'T1 seam: latest trace readmodel must be 200');
    const chainTrace = await rawReq(server.base, '/v1/namespaces/default/traces/' + chain.traceId, { headers: sessionCookie(s) });
    assert.equal(chainTrace.status, 200, 'T1 seam: chain trace readmodel must be 200');
    return {
      // 节点必须保持在线：trace 详情页点进 message 会向「跑过这次 attempt 的节点」取日志，
      // 会话一关就是 503 node_offline（那是真的离线，不是页面 bug）。关闭交给 openDriver.stop()。
      __holdNodes: [node],
      chainTrace: chain.traceId,
      chainRootTask: chain.taskId,
      chainTasks: chainTrace.body.tasks.length,
      chainPruned: chainTrace.body.pruned.length,
      failedTask: failedFire.taskId,
      runningTask: runningFire.taskId,
      latestEventTrace: latestWithTrace.spec.traceUID,
      latestTraceTasks: latestTrace.body.tasks.length,
      // 让用例按“页面会看到的同一份事实”算期望值。
      indexEvents: (events.body.items || []).length,
    };
  }
}

// messageScenario 造“任务详情页”的全部真实事实：
//
//   1. 成功任务：2 条下游 emit + 1 条被剪枝的 emit（共 3 条 emission，分页 2+1）
//      + 一条**真实上传**的产物（下载字节的 sha256 就是路径里的 sha256）
//   2. 在跑任务：持有真实 lease（页面显示真 leaseExpiresAt/attempt），日志档案由
//      节点回答 logs_request（truncated=true）
//   3. 还未派发的 pending 任务（cancel 的 CAS 对象）
//   4. 产物被保留期清掉的成功任务（元数据在、副本不在 → 410 artifact_expired）
SCENARIOS.set('observe-message', async (server, fixtures) => messageScenario(server, fixtures));

async function messageScenario(server, fixtures) {
  const s = fixtures.adminSession;
  const envRef = (name) => ({ valueFrom: { fieldRef: { fieldPath: 'vars.' + name } } });
  const child = await ensureCapability(server, s, DEFAULT_NS, 'msg-child');
  const broken = await ensureCapability(server, s, DEFAULT_NS, 'msg-broken', [], { params: { type: 'array' } });
  const root = await ensureCapability(server, s, DEFAULT_NS, 'msg-root', ['msg-child', 'msg-broken'], {
    env: { SOURCE_ROOT: envRef('SOURCE_ROOT'), GIT_TERMINAL_PROMPT: envRef('GIT_TERMINAL_PROMPT') },
    requires: { values: ['vars.SOURCE_ROOT', 'vars.GIT_TERMINAL_PROMPT'] },
    lifecycle: { idempotent: true, maxAttempts: 3, timeoutSec: 60 },
    // 同一节点上允许 3 个并发执行：夹具要同时持有「在跑」与「产物过期」两个 lease。
    concurrency: { maxPerNode: 3 },
  });
  await ensureSource(server, s, DEFAULT_NS, 'msg-hook', 'msg-root');
  const node = await bringUpFixtureNode({
    base: server.base,
    session: s,
    name: 'f3-msg-node',
    namespaces: [DEFAULT_NS],
    vars: { SOURCE_ROOT: '/node-default', GIT_TERMINAL_PROMPT: '0' },
    localVars: { SOURCE_ROOT: '/local-src' },
    localLimits: { total: 4, perCapability: { 'msg-root': 3 } },
    slots: 4,
  });
  {
    for (const cap of [root, child, broken]) await deployApplied(server.base, s, node, capRef(cap));
    // ---- 1) 成功任务：2 条下游 + 1 条剪枝 + 真实产物 ----
    const done = await fireFixture(server, fixtures.sendKey, DEFAULT_NS, 'msg-hook');
    const doneGrant = await node.lease(done.taskId);
    const first = await node.emit(doneGrant, { to: 'msg-child', data: { n: 1 }, id: 'emit-1' });
    const second = await node.emit(doneGrant, { to: 'msg-child', data: { n: 2 }, id: 'emit-2' });
    const pruned = await node.emit(doneGrant, { to: 'msg-broken', data: { n: 3 }, id: 'emit-3' });
    assert.ok(first?.response?.taskId && second?.response?.taskId, '两条下游 emit 都必须建出子任务');
    assert.equal(pruned?.response?.pruned?.reason, 'param_schema', '第三条 emit 必须被 param_schema 剪枝');
    const artifactBytes = Buffer.from('# f3 report\n\nreal bytes served by the hub spool\n', 'utf8');
    const uploaded = await node.uploadArtifact(doneGrant, { path: 'artifacts/report.md', bytes: artifactBytes });
    assert.ok([200, 201].includes(uploaded.status), '产物上传必须被接受，实际 ' + uploaded.status);
    assert.equal(await node.result(doneGrant, { summary: '实际完成的报告', artifacts: [{ path: 'artifacts/report.md', sha256: uploaded.sha256 }], exitCode: 0 }).then(Boolean), true, '成功结果必须被 ack');
    // ---- 2) 产物被保留期清掉：上传 → 结果成功 → 删掉 spool 副本（保留期 GC 的等价删法） ----
    const expired = await fireFixture(server, fixtures.sendKey, DEFAULT_NS, 'msg-hook');
    const expiredGrant = await node.lease(expired.taskId);
    const expiredBytes = Buffer.from('expired artifact body\n', 'utf8');
    const expiredUpload = await node.uploadArtifact(expiredGrant, { path: 'artifacts/old.txt', bytes: expiredBytes });
    assert.ok([200, 201].includes(expiredUpload.status), '过期产物也必须先真实上传');
    assert.equal(await node.result(expiredGrant, { summary: '产物稍后被保留期清掉', artifacts: [{ path: 'artifacts/old.txt', sha256: expiredUpload.sha256 }] }).then(Boolean), true, '结果必须被 ack');
    // ---- 3) 在跑任务：lease + 日志档案（truncated） ----
    const running = await fireFixture(server, fixtures.sendKey, DEFAULT_NS, 'msg-hook');
    const runningGrant = await node.lease(running.taskId);
    const logLines = ['f3 fixture log: attempt 1 started', 'f3 fixture log: infra_transient while fetching toolchain', 'f3 fixture log: retrying'];
    node.archiveLogs(runningGrant.attempt, logLines, { truncated: true });
    // ---- 4) pending 任务（不接派发：它是 cancel 的 CAS 对象） ----
    const pending = await fireFixture(server, fixtures.sendKey, DEFAULT_NS, 'msg-hook');
    // 产物副本落在中枢自己的 spool 里（resultspool 的 object id 是内部 id，不是 sha256），
    // 所以按**字节哈希**找到这个对象再删：保留期 GC 删的就是这份副本，元数据行保留
    // → 下载必须变成 410 artifact_expired（"引用确实存在过、副本没了"）。
    const spoolObjects = path.join(path.dirname(server.dbPath), 'artifacts', 'objects');
    assert.ok(existsSync(spoolObjects), '中枢必须真的建出 artifact spool（' + spoolObjects + '）');
    const expiredObject = readdirSync(spoolObjects)
      .map((file) => path.join(spoolObjects, file))
      .find((file) => createHash('sha256').update(readFileSync(file)).digest('hex') === expiredUpload.sha256);
    assert.ok(expiredObject, '中枢必须真的把产物字节写进 spool（找不到 sha256=' + expiredUpload.sha256 + '）');
    rmSync(expiredObject);
    return {
      __holdNodes: [node],
      messageTask: done.taskId,
      messageArtifactSha: uploaded.sha256,
      messageArtifactBytes: artifactBytes.length,
      runningTask: running.taskId,
      runningAttempt: runningGrant.attempt,
      runningLogLines: logLines,
      pendingTask: pending.taskId,
      expiredTask: expired.taskId,
      expiredArtifactSha: expiredUpload.sha256,
      childTasks: [first.response.taskId, second.response.taskId],
    };
  }
}

// nodeConfigScenario：node.html 的有效值/预算来源层要验真：
//   · SOURCE_ROOT 的**值**来自本机（hello.localVars），来源层是 local；
//   · NODE_ONLY 来自节点 vars（node）；NS_ONLY 来自命名空间 vars（namespace）；
//   · 两层预算逐叶合并：local.total 压住 node.total，perCapability.fixer 来自 local，
//     perCapability.* 来自 node。
// 这些事实只能由「节点真的连上来并报自己的 localVars/localLimits」产生。
SCENARIOS.set('node-config', async (server, fixtures) => {
  const s = fixtures.adminSession;
  await ensureNode(server, s, MOCK_FIXTURES.node);
  // 节点默认值/预算走**真实写入口**（PUT …/nodes/{name}/config，CAS 带当前 rv）：
  // 基础夹具已经建过这个节点，直接改 spec 不是产品入口。写完之后再握手 —— welcome
  // 会带上新的 configRevision 与 nodeVars/nodeLimits，节点据此算 appliedHash。
  const current = await rawReq(server.base, `/v1/nodes/${MOCK_FIXTURES.node}/config`, { headers: sessionCookie(s) });
  assert.equal(current.status, 200, 'T1 seam: GET node config must be 200');
  const saved = await rawReq(server.base, `/v1/nodes/${MOCK_FIXTURES.node}/config`, {
    method: 'PUT',
    headers: jsonHeaders(s),
    body: JSON.stringify({
      metadata: current.body.metadata,
      vars: { SOURCE_ROOT: '/node-default', NODE_ONLY: 'node-visible' },
      limits: { total: 8, perCapability: { '*': 3, fixer: 2 } },
      namespaces: [DEFAULT_NS],
    }),
  });
  assert.equal(saved.status, 200, 'T1 seam: PUT node config must be 200, got ' + saved.status + ' ' + JSON.stringify(saved.body));
  // 命名空间级只定义「只属于 ns 的」变量：真实语义里 ns 默认值压过节点值
  // （任务输入 > 机器配置），所以把 NODE_ONLY 也写进 ns 会让它的来源层变成
  // namespace —— 那就测不到「node 层」这条来源了。
  await ensureNamespaceVars(server, s, DEFAULT_NS, { SOURCE_ROOT: '/ns-default', NS_ONLY: 'default-visible' });
  const node = await bringUpFixtureNode({
    base: server.base,
    session: s,
    name: MOCK_FIXTURES.node,
    namespaces: [DEFAULT_NS],
    vars: { SOURCE_ROOT: '/node-default', NODE_ONLY: 'node-visible' },
    limits: { total: 8, perCapability: { '*': 3, fixer: 2 } },
    localVars: { SOURCE_ROOT: '/local-src' },
    localLimits: { total: 4, perCapability: { fixer: 1 } },
    slots: 1,
  });
  return {
    // 用例在同一个进程里跑完之前必须保持连接（status.localVars 来自会话上报的 hello），
    // 所以这里把关闭动作挂到 process 退出：openDriver 的 stop() 之后事实已经断言完。
    __holdNode: node,
    nodeLocalVars: { SOURCE_ROOT: '/local-src' },
    nodeLocalLimits: { total: 4, fixer: 1 },
  };
});

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
  /* 预期内的 4xx（#335）：真实服务会**故意**拒绝一些请求（slug 冲突、CAS 409、
   * 没有本地归档的日志 410 …），Chrome 会把这些响应记成 console error。用例必须
   * 在注册时**显式声明** {{status, path, note}}：只对「这个状态 + 这个请求路径前缀」
   * 放行，别的 4xx/5xx 仍然 fatal —— 没有全局忽略开关，也不能只按状态码批量忽略。
   * path 匹配 URL 的 pathname 前缀（忽略查询串：CAS 的 resourceVersion 就在查询串里）。 */
  const expected4xx = (options.expectHttpStatus || []).map((entry) => {
    assert.ok(Number.isInteger(entry.status) && entry.status >= 400 && entry.status < 500, 'expectHttpStatus entry must declare a 4xx status: ' + JSON.stringify(entry));
    assert.ok(typeof entry.path === 'string' && entry.path.startsWith('/'), 'expectHttpStatus entry must declare an absolute request path: ' + JSON.stringify(entry));
    assert.ok(entry.pathSuffix === undefined || (typeof entry.pathSuffix === 'string' && entry.pathSuffix.startsWith('/')), 'expectHttpStatus.pathSuffix must be an absolute path tail: ' + JSON.stringify(entry));
    assert.ok(typeof entry.note === 'string' && entry.note.trim().length > 0, 'expectHttpStatus entry must document why (note): ' + JSON.stringify(entry));
    return { status: entry.status, path: entry.path, pathSuffix: entry.pathSuffix, exact: entry.exact === true, note: entry.note, hits: 0 };
  });
  report.expectedHttpStatus = [];
  const pathnameOf = (u) => { try { return new URL(u).pathname; } catch { return ''; } };
  // path 默认是前缀（实例 id 无法预知）；exact=true 要求 pathname 完全相同，
  // pathSuffix 再把前缀锁到尾部 —— 声明越窄越好，别让一条白名单放行整片路径。
  const matchesExpected = (entry, pathname) => {
    if (entry.exact) return pathname === entry.path;
    if (!pathname.startsWith(entry.path)) return false;
    return entry.pathSuffix === undefined || pathname.endsWith(entry.pathSuffix);
  };
  const reportDir=evidenceDirectory(report);await mkdir(reportDir,{recursive:true});
  const stop = () => stopped ||= (async () => {
    // Preserve a failed controller's catch/finally scene before closing its doc.
    // Collection must not replace the existing functional failure on cleanup.
    if (archive && page && !page.isClosed()) {
      try { await archive(); }
      catch (error) { (report.traceArchiveErrors ||= []).push(traceEvidenceTools().error(error)); }
    }
    if (shared) { if(context) await context.close(); }
    else { if (browser) await browser.close(); if (server) { for (const held of server.fixtures?.__holdNodes || []) held.close(); server.fixtures?.__holdNode?.close(); await server.stop(); } }
    await persistReport(report);
  })();
  try {
    if (mock) server = shared ? shared.server : await preview();
    else {
      assert.ok(existsSync(H.BIN), `T1 seam: ANOTIFY_BIN missing: ${H.BIN}; build genuine v4 server, no preview fallback`);
      assert.ok(existsSync(H.DEVSEED_BIN), `T1 seam: DEVSEED_BIN missing: ${H.DEVSEED_BIN}; generic retained auth seed required`);
      server = await H.startServer({ suiteName:'frontend' });
      const fixtures = await realFixtures(server, await realBootstrap(server), id);
      server.session = fixtures.adminSession;
      const ns = await rawReq(server.base,'/v1/namespaces',{headers:sessionCookie(server.session)});
      assert.equal(ns.status,200,'T1 seam: genuine namespaced API bootstrap must respond 200');
      assert.equal(ns.body.kind,'NamespaceList','T1 seam: legacy namespace adapter is forbidden');
      assert.ok(ns.body.items.some(n=>n.metadata.name==='default'),'T1 seam: default Namespace fixture required');
      assert.ok(ns.body.items.some(n=>n.metadata.name==='prod'),'T1 seam: prod Namespace fixture required');
      server.fixtures = fixtures;
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
            // v4 NodeInstanceRef 只有 {id,task,busy}：槽位身份是 id，没有 slot/taskId/attempt/leaseId。
            node.facts.instances=[{id:'nins_mock_1',task:'tsk_mock_4',busy:true},{id:'nins_mock_2',task:null},{id:'nins_mock_3',task:null,busy:false},{id:'nins_mock_4',task:'reserved-task',busy:true}];
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
    page.on('console',msg=>{
      if(msg.type()!=='error'){ if(msg.text().includes('mock 未实现')) report.errors.push(msg.text()); return; }
      // 网络层的失败响应会以 "Failed to load resource: … status of NNN (…)" 出现在
      // console：只有用例声明过的（status, path）组合放行，其余照样记成 fatal。
      const failure=/Failed to load resource: the server responded with a status of (\d+)/.exec(msg.text());
      const url=msg.location()?.url||'', hit=failure && expected4xx.find(e=>e.status===Number(failure[1])&&matchesExpected(e,pathnameOf(url)));
      if(hit){ hit.hits++; report.expectedHttpStatus.push({status:hit.status,path:hit.path,url}); return; }
      report.errors.push(msg.text());
    });
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
    // 204/空体是合法成功（v4 的 DELETE 一律 204，契约明文"无响应体"）：无条件 r.json()
    // 会把这个成功响应抛成 SyntaxError，看起来像产品坏了。
    const response=(url,body,method='GET')=>page.evaluate(async([url,body,method])=>{
      const r=await fetch(url,{method,...(body==null?{}:{body:JSON.stringify(body),headers:{'Content-Type':'application/json'}})});
      const text=await r.text();
      let parsed=null;
      if(text.trim()!==''){ try{ parsed=JSON.parse(text); }catch{ parsed={unparsed:text}; } }
      return {status:r.status,body:parsed};
    },[url,body,method]);
    // api 是**宿主侧**的真实 HTTP 读（不经页面、不进 __f3Calls）：用例在打开页面前
    // 需要先取到真实事实当期望值时用。mock 权威下没有后端，调用即错。
    const api=(url,opts={})=>{
      assert.ok(!mock,'api() 只属于真实权威（mock 预览没有后端）');
      return rawReq(server.base,url,{...opts,headers:{...sessionCookie(server.session),...(opts.headers||{})}});
    };
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
    const finish=async()=>{await archive();
      // 声明了却一次都没发生 = 声明已过期/写错；留着就是一份永远放行的白名单。
      // （mock 权威下网络被 mock 路由接管，不会产生 Chrome 的网络 console：只在 real 校验。）
      if(!mock) assert.deepEqual(expected4xx.filter(e=>e.hits===0).map(e=>e.status+' '+(e.exact?'=':'~')+e.path+(e.pathSuffix||'')),[],'每个 expectHttpStatus 声明都必须在真实网络上发生过');
      assert.deepEqual(report.errors,[],'JS/console/network/unimplemented501 must fail');};
    return {id,mock,base:server.base,page,context,browser,report,stop,finish,goto,req,response,api,snapshot,calls,check,shot,clean,archive,fixtures:server.fixtures,
      // fx 把「用例需要的 fixture 角色」映射到当前权威下的真实身份：mock 固定事实 or
      // 真实创建对象的 id。用例不得再硬编码 tsk_/trace_ 常量走 real 路径。
      fx:(key)=>{const f=server.fixtures||{};return mock?MOCK_FIXTURES[key]:(f[key]??MOCK_FIXTURES[key]);}};
  } catch(error){report.errors.push(error.stack||String(error));await stop();throw error;}
}
