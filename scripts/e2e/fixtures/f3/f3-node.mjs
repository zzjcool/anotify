// F3 real-mode fixture node (#335 第三轮): a **protocol-conformant** node that goes
// through the product's own entry points.
//
// 为什么在 f3 里再写一个节点辅助（而不是直接用 lib/nodes.mjs 的 MockNode）：
//
//   - MockNode.connect 用 seedNodeRows 直写 nodes 行来拿到明文 token，而 f3 这一轮
//     的纪律是「只走真实入口」：节点必须走 POST /v1/nodes → POST /v1/nodes/{name}/tokens
//     → WS hello → welcome → config.applied。本文件因此**不碰数据库**。
//   - 观测类用例需要节点回答 `logs_request`、按 attempt 上传产物（PUT …/artifacts）、
//     在取消时回报 `cancel.ack{processGroupReaped:true}`。这些是协议里已有的事实来源，
//     lib/nodes.mjs 的 MockNode 有意没做（它的边界是「只发执行语义帧」）。这里补齐，
//     但**仍然只是「一个守协议的节点会说什么」**：不跑脚本、不产生执行事实。
//
// 帧形状仍然只有一份镜像：hello / accept / grant / emit / result 全部复用
// lib/nodes.mjs 的 nodeHello 与 … 的字段口径；本文件只负责把自己收发的帧按
// internal/protocol 的名字发出（logs_request/logs_response、cancel.ack）。
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { wsConnect } from '../../lib/wsclient.mjs';
import { nodeHello } from '../../lib/nodes.mjs';
import { machineConfigHash } from '../../lib/v4.mjs';

export const sha256hex = (bytes) => createHash('sha256').update(bytes).digest('hex');

// mergeLimits 是 store.mergeLimits 的节点侧镜像（local 逐叶覆盖 node）。
// 机器配置内容哈希必须用**合并后**的 vars/limits 算，否则中枢算出来的期望值
// 与节点报的 appliedHash 不一致 → grant 以 stale_config 被拒（任务退回
// waiting_for_config 且永远拿不到租约）。
export function mergeLimits(base, override) {
  const out = { ...(base || {}) };
  for (const [key, value] of Object.entries(override || {})) {
    if (value && typeof value === 'object' && !Array.isArray(value) && out[key] && typeof out[key] === 'object') {
      out[key] = mergeLimits(out[key], value);
    } else out[key] = value;
  }
  return out;
}

export function appliedHash(nodeVars, nodeLimits, localVars, localLimits) {
  return machineConfigHash({ ...(nodeVars || {}), ...(localVars || {}) }, mergeLimits(nodeLimits, localLimits));
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// FixtureNode 持有**一条真实 WS 会话**的全部事实（帧、会话、令牌）。
export class FixtureNode {
  constructor({ name, id, token, conn, slots, frames }) {
    this.name = name;
    this.id = id;
    this.token = token;
    this.conn = conn;
    this.slots = slots;
    this.frames = frames;
    this.closed = false;
    this.configRevision = '';
    // 日志档案：attempt → 该次执行打印过的记录（节点侧真实持有的东西）。
    this.logArchive = new Map();
    // 物理槽账本：同一个 slot 不能被两次 accept 同时占（那是「另一个进程还在跑」）。
    this.busySlots = new Set();
    this.logsRequests = [];
    this._dispatchCursor = 0;
    conn.onFrame((frame) => {
      frames.push(frame);
      this._react(frame);
    });
    conn.onClose(() => {
      this.closed = true;
    });
  }

  // _react 是「守协议节点」的自动应答：部署回执、取消回执、日志档案。
  // 这些帧不是被测行为，而是被测页面的**上游事实**：没有它们，页面上的
  // 「部署已应用 / 取消已回收 / 日志尾巴」这类读模型就永远等不到数据。
  _react(frame) {
    if (frame.type === 'capability.deploy') {
      this.conn.send({
        type: 'capability.deployed',
        requestId: frame.requestId,
        namespace: frame.namespace,
        name: frame.name,
        version: frame.version,
        digest: frame.digest,
        status: 'applied',
      });
      return;
    }
    if (frame.type === 'logs_request') {
      this.logsRequests.push(frame);
      const attempt = frame.attempt || this.latestArchivedAttempt();
      const archive = this.logArchive.get(attempt);
      if (!archive) {
        // 错误回包**不能**带 lines（中枢把「错误 + 成功记录」当 correlate 失败）。
        this.conn.send({ type: 'logs_response', taskId: frame.taskId, requestId: frame.requestId, stdout: '', stderr: '', truncated: false, bytes: 0, error: 'no_local_archive' });
        return;
      }
      const lines = archive.lines.slice(0, archive.truncated ? archive.lines.length : frame.tailLines || archive.lines.length);
      this.conn.send({
        type: 'logs_response',
        taskId: frame.taskId,
        requestId: frame.requestId,
        attempt,
        stdout: lines.map((line) => line.text).join('\n'),
        stderr: '',
        truncated: !!archive.truncated,
        bytes: Buffer.byteLength(lines.map((line) => line.text).join('\n')),
        lines,
      });
      return;
    }
    if (frame.type === 'task.cancel') {
      if (Number.isInteger(frame.slot)) this.busySlots.delete(frame.slot);
      // 节点真的收掉了进程组才回 reaped=true：中枢据此把任务落成终态并释放槽。
      this.conn.send({
        type: 'cancel.ack',
        taskId: frame.taskId,
        attempt: frame.attempt,
        leaseId: frame.leaseId,
        slot: frame.slot,
        processGroupReaped: true,
      });
    }
  }

  latestArchivedAttempt() {
    return Math.max(0, ...this.logArchive.keys());
  }

  // archiveLogs 让本节点对某次 attempt 持有日志档案（节点侧的真实事实来源）。
  archiveLogs(attempt, lines, { truncated = false } = {}) {
    assert.ok(attempt >= 1, 'attempt 必须 ≥1');
    this.logArchive.set(attempt, {
      truncated,
      lines: lines.map((text, index) => ({ sequence: index + 1, stream: 'stdout', text, at: new Date().toISOString() })),
    });
  }

  wait(pred, timeoutMs = 8000) {
    const deadline = Date.now() + timeoutMs;
    return (async () => {
      for (;;) {
        const hit = this.frames.find(pred);
        if (hit) return hit;
        if (Date.now() > deadline) return null;
        await sleep(20);
      }
    })();
  }

  // waitDispatch 用游标消费派发帧：同一 assignment 会被 sweep 重复下发，直接 find
  // 会永远拿到第一条。
  async waitDispatch(taskId, timeoutMs = 8000) {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      const rest = this.frames.slice(this._dispatchCursor);
      const index = rest.findIndex((f) => f.type === 'task.dispatch' && (taskId === undefined || f.taskId === taskId));
      if (index >= 0) {
        this._dispatchCursor += index + 1;
        return rest[index];
      }
      if (Date.now() > deadline) return null;
      await sleep(20);
    }
  }

  accept(dispatch, slot = 0) {
    assert.equal(dispatch?.type, 'task.dispatch', 'accept 需要 task.dispatch 帧');
    this.conn.send({ type: 'task.accept', taskId: dispatch.taskId, assignmentId: dispatch.assignmentId, proposedAttempt: dispatch.proposedAttempt, node: this.name, slot });
  }

  async waitGrant(taskId, timeoutMs = 8000) {
    return this.wait((f) => f.type === 'lease.grant' && f.taskId === taskId, timeoutMs);
  }

  freeSlot() {
    for (let slot = 0; slot < this.slots; slot++) if (!this.busySlots.has(slot)) return slot;
    throw new Error('夹具节点没有空闲槽位（slots=' + this.slots + '）');
  }

  // release 在 try/finally 里也要安全：协议上槽由 grant/cancel.ack 定位。
  release(grant) {
    if (grant && Number.isInteger(grant.slot)) this.busySlots.delete(grant.slot);
  }

  // lease 是「接下一个任务的完整握手」：dispatch → accept → lease.grant。
  async lease(taskId, slot) {
    const dispatch = await this.waitDispatch(taskId);
    assert.ok(dispatch, `任务 ${taskId} 没有收到 task.dispatch（节点不是候选或已离线）`);
    this.accept(dispatch, slot === undefined ? this.freeSlot() : slot);
    const grant = await this.waitGrant(taskId);
    assert.ok(grant, `任务 ${taskId} 的 task.accept 没有换来 lease.grant（配置未 apply 或预算拒绝）`);
    // 真的跑过这一次执行的节点一定有这次 attempt 的日志档案（哪怕一条都没有）：
    // 没有档案只可能是「不是这台机器跑的 / 被 GC」——那是 410 的另一条路径，
    // 不该被夹具的懒省事静默冒充。
    if (!this.logArchive.has(grant.attempt)) this.archiveLogs(grant.attempt, ['f3 fixture log: attempt ' + grant.attempt + ' finished']);
    if (Number.isInteger(grant.slot)) this.busySlots.add(grant.slot);
    return grant;
  }

  async emit(grant, { to, data = {}, id } = {}) {
    const requestId = 'rq_' + Math.random().toString(36).slice(2);
    const from = this.frames.length;
    const frame = { type: 'task.emit', requestId, taskId: grant.taskId, attempt: grant.attempt, leaseId: grant.leaseId, attemptToken: grant.attemptToken, to, data };
    if (id !== undefined) frame.id = id;
    this.conn.send(frame);
    const deadline = Date.now() + 8000;
    while (Date.now() < deadline) {
      const hit = this.frames.slice(from).find((f) => f.type === 'task.emit.ack' && f.requestId === requestId);
      if (hit) return hit;
      await sleep(20);
    }
    return null;
  }

  // uploadArtifact 走协议的产物通道（PUT /v1/node/tasks/{taskId}/artifacts/{sha256}）。
  // 中枢只认这条 HTTP：没有上传的 manifest 引用会被 validateResultManifest 拒。
  async uploadArtifact(grant, { path, bytes }) {
    const sha = sha256hex(bytes);
    const response = await fetch(`${this.base}/v1/node/tasks/${grant.taskId}/artifacts/${sha}`, {
      method: 'PUT',
      headers: {
        Authorization: 'Bearer ' + this.token,
        'X-Anotify-Attempt': String(grant.attempt),
        'X-Anotify-Lease-Id': grant.leaseId,
        'X-Anotify-Task-Token': grant.attemptToken,
        'X-Anotify-Artifact-Path': encodeURIComponent(path),
        'Content-Type': 'application/octet-stream',
      },
      body: bytes,
    });
    const text = await response.text();
    return { status: response.status, sha256: sha, body: text ? JSON.parse(text) : null };
  }

  async result(grant, { phase = 'succeeded', summary = '', error, artifacts = [], exitCode = 0 } = {}) {
    // 结果被接受 = 这次执行的进程组已退出：槽位由真实 ack 释放（这里只是夹具账本）。
    this.release(grant);
    const now = new Date().toISOString();
    const from = this.frames.length;
    const frame = {
      type: 'task.result',
      taskId: grant.taskId,
      attempt: grant.attempt,
      node: this.name,
      leaseId: grant.leaseId,
      phase,
      result: { summary, artifacts },
      startedAt: now,
      finishedAt: now,
    };
    if (exitCode !== undefined) frame.exitCode = exitCode;
    if (error !== undefined) frame.result.error = error;
    this.conn.send(frame);
    const deadline = Date.now() + 8000;
    while (Date.now() < deadline) {
      const hit = this.frames.slice(from).find((f) => f.type === 'result.ack' && f.taskId === grant.taskId);
      if (hit) return hit;
      await sleep(20);
    }
    return null;
  }

  close() {
    if (this.closed) return;
    try { this.conn.close(); } catch { /* already closed */ }
    this.closed = true;
  }
}

// bringUpFixtureNode 用**产品自己的入口**把一个节点变成「在线 + 配置已 apply」：
//
//   POST /v1/nodes（登记 + 授权 ns + 机器 vars/limits）
//   → POST /v1/nodes/{name}/tokens（明文只在签发响应里出现一次）
//   → WS hello（localVars / localLimits 是**本机**事实）
//   → welcome（中枢下发 nodeVars/nodeLimits/configRevision）
//   → config.applied（节点回报 applied_revision + 合并后内容哈希）
//
// 三次握手没有捷径：缺 config.applied 时 grant 会以 stale_config 被拒（任务退回
// waiting_for_config），症状看起来像「中枢不派发」。
export async function bringUpFixtureNode({ base, session, name, namespaces = ['default'], vars = {}, limits = {}, localVars = {}, localLimits = {}, slots = 1, agentVersion }) {
  assert.ok(base && session && name, 'bringUpFixtureNode: base/session/name 必填');
  const headers = { Cookie: 'anotify_session=' + session, 'Content-Type': 'application/json' };
  const create = await fetch(`${base}/v1/nodes`, {
    method: 'POST',
    headers,
    body: JSON.stringify({ apiVersion: 'anotify/v1', kind: 'Node', metadata: { name }, spec: { namespaces, vars, limits } }),
  });
  const created = await create.json();
  assert.ok([201, 409].includes(create.status), `登记节点 ${name} 必须 201/409，实际 ${create.status} ${JSON.stringify(created)}`);
  const tokenResponse = await fetch(`${base}/v1/nodes/${encodeURIComponent(name)}/tokens`, { method: 'POST', headers, body: '{}' });
  const tokenDoc = await tokenResponse.json();
  assert.equal(tokenResponse.status, 200, `签发 ${name} 的令牌必须 200，实际 ${tokenResponse.status}`);
  assert.ok(tokenDoc.token && tokenDoc.token.length >= 20, '节点令牌明文只在签发响应里出现一次');

  const conn = await wsConnect(base, tokenDoc.token);
  const frames = [];
  const node = new FixtureNode({ name, id: created.metadata?.uid || '', token: tokenDoc.token, conn, slots, frames });
  node.base = base;
  conn.send(nodeHello({ name, token: tokenDoc.token, namespaces, capabilities: [], slots, localVars, localLimits, agentVersion }));
  const welcome = await node.wait((f) => f.type === 'welcome', 8000);
  assert.ok(welcome, `节点 ${name} 没有收到 welcome`);
  node.configRevision = welcome.configRevision;
  conn.send({
    type: 'config.applied',
    revision: welcome.configRevision,
    hash: appliedHash(welcome.nodeVars || vars, welcome.nodeLimits || limits, localVars, localLimits),
    instances: Array.from({ length: slots }, (_, slot) => ({ slot, busy: false })),
  });
  node.welcome = welcome;
  return node;
}

// capRef 把 POST 响应的能力信封折成部署需要的三件真实事实（uid/version/digest）。
export function capRef(body) {
  const doc = body || {};
  return {
    name: doc.metadata?.name,
    version: doc.metadata?.version,
    digest: doc.status?.digest,
  };
}

// deployApplied 让节点真的持有能力正文：部署动作 → capability.deploy → deployed(applied)
// → 读模型收敛。**(pending 不是候选)**：回执不到位的部署不会派发。
export async function deployApplied(base, session, node, capability, namespace = 'default') {
  const headers = { Cookie: 'anotify_session=' + session, 'Content-Type': 'application/json' };
  const response = await fetch(`${base}/v1/namespaces/${namespace}/capabilities/${capability.name}/deploy`, {
    method: 'POST',
    headers,
    body: JSON.stringify({ nodes: [node.name] }),
  });
  const doc = await response.json();
  assert.equal(response.status, 200, `部署 ${capability.name} → ${node.name} 必须 200，实际 ${response.status} ${JSON.stringify(doc)}`);
  const deadline = Date.now() + 8000;
  while (Date.now() < deadline) {
    const rows = await fetch(`${base}/v1/namespaces/${namespace}/deployments`, { headers: { Cookie: 'anotify_session=' + session } }).then((r) => r.json());
    const applied = (rows.deployments || []).find((d) => d.capability === capability.name && d.state === 'applied' && d.digest === capability.digest);
    if (applied) return applied;
    await sleep(40);
  }
  throw new Error(`部署行 ${capability.name} → ${node.name} 没有进入 applied`);
}
