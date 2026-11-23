// #432 能力启停 + 手动试跑（真实权威 frontend 套件的 end-to-end 用例）。
//
// 覆盖用户真正做的那条链，全部走**产品路径**（页面按钮 + 真实 HTTP，不插库、不 mock）：
//
//  1. 能力详情页显示「已启用」徽标与「停用」按钮；
//  2. 点「停用」→ confirmDanger 二次确认 → 徽标变「已停用」+ 按钮变「启用」；
//  3. 停用期间用真实入口触发源：**回执 tasks 为空、pruned 有 capability_disabled**，
//     并且 trace 上留了剪枝事实（事件里能查到该 reason）；
//  4. 点「启用」→ 徽标回「已启用」；
//  5. 再触发同一个源 → 建出真任务（pending），能力名正确；
//  6. 「试跑」弹窗：填参数 → 提交 → 回执含 traceId + 真任务，`origin=test`；
//  7. 详情页「最近操作」审计区读到 disable + test 两条（倒序）。
//
// 为什么断言「回执 + trace 事件」而不是 `status.observed.pruned`：#432 的设计草案 §10
// 曾期望剪枝出现在 `status.observed.pruned`，但该投影以**最近一个任务**的 trace 为键
// （`observedPrunes` 按 taskId 过滤）。一次被完全剪掉的触发没有任务，投影因此为空——
import assert from 'node:assert/strict';
import { acceptDialog } from './f3-dialogs.mjs';

const NS = 'default';
// 用例级唯一名字：frontend 套件并发跑 case，不能撞别的用例的夹具。
const CAP = 'f3-life-capability';
const SRC = 'f3-life-source';

// 参数 schema 走**产品校验**：spec.params 是能力参数的 JSON Schema（不是 spec.values）。
const PARAMS_SCHEMA = { type: 'object', properties: { diff: { type: 'string', description: '要审的变更' } }, required: ['diff'] };

const RUN = { entrypoint: 'run.sh', files: { 'run.sh': '#!/bin/sh\necho f3 lifecycle fixture\n' } };

/** 幂等建夹具：列表里没有才 POST（真实 API，不插库）。 */
async function ensureCapability(d, name) {
	// `d.response` 给 {status, body}；`d.req` 只给正文（需要判状态码时一律用 d.response）。
	const list = await d.response(`/v1/namespaces/${NS}/capabilities`);
	assert.equal(list.status, 200, `夹具前置：GET capabilities 必须 200（实际 ${list.status}）`);
	const found = (list.body.items || []).find((c) => c.metadata.name === name);
	if (found) return found;
	const created = await d.response(`/v1/namespaces/${NS}/capabilities`, {
		apiVersion: 'anotify/v1', kind: 'Capability', metadata: { name },
		spec: { params: PARAMS_SCHEMA, run: RUN },
	}, 'POST');
	assert.ok(created.status === 201 || created.status === 200, `夹具前置：建能力 ${name} 失败（${created.status} ${JSON.stringify(created.body)}）`);
	return created.body;
}

async function ensureSource(d, name, capability) {
	const list = await d.response(`/v1/namespaces/${NS}/sources`);
	assert.equal(list.status, 200, `夹具前置：GET sources 必须 200（实际 ${list.status}）`);
	const found = (list.body.items || []).find((s) => s.metadata.name === name);
	if (found) return found;
	const created = await d.response(`/v1/namespaces/${NS}/sources`, {
		apiVersion: 'anotify/v1', kind: 'Source', metadata: { name },
		spec: { kind: 'manual', config: {}, targets: [{ capability, params: { diff: '$(payload.diff)' } }] },
	}, 'POST');
	assert.ok(created.status === 201 || created.status === 200, `夹具前置：建源 ${name} 失败（${created.status} ${JSON.stringify(created.body)}）`);
	return created.body;
}

/** 用真实入口触发（会话 + POST …/sources/{name}/fire，与页面「试跑一次」同一条路径）。 */
async function fireSource(d, name, payload) {
	const res = await d.response(`/v1/namespaces/${NS}/sources/${encodeURIComponent(name)}/fire`, { payload }, 'POST');
	assert.equal(res.status, 200, `触发源 ${name} 必须 200（实际 ${res.status} ${JSON.stringify(res.body)}）`);
	return res.body;
}

export async function capabilityLifecycle(d) {
	const { page, check, shot } = d;

	// 先落到任意一页再调 API：`d.req` / `d.response` 都走页面里的 window.Anotify
	// （driver 的注入发生在导航之后），空白页上直接调用会 ReferenceError。
	await d.goto('capabilities', `namespace=${NS}`, 'zh-CN');
	await ensureCapability(d, CAP);
	await ensureSource(d, SRC, CAP);

	const openDetail = async () => {
		await d.goto('capability', `namespace=${NS}&name=${encodeURIComponent(CAP)}`, 'zh-CN');
		await page.locator('#cap-enabled-badge').waitFor({ state: 'visible', timeout: 15000 });
		return page.locator('#cap-detail');
	};
	const badgeText = async () => (await page.locator('#cap-enabled-badge').innerText()).trim();
	// 徽标文案可能带一个状态点/空白，用「包含」判而不是逐字相等。
	const waitBadge = (want) => page.waitForFunction(
		([sel, want]) => {
			const n = document.querySelector(sel);
			return !!n && (n.textContent || '').includes(want);
		},
		['#cap-enabled-badge', want],
		{ timeout: 15000 },
	);

	// ---- 1. 初始状态：默认启用（无开关行 = enabled） ----
	await openDetail();
	assert.ok((await badgeText()).includes('已启用'), `默认状态徽标必须是「已启用」（实际 ${await badgeText()}）`);
	assert.equal(await page.locator('#cap-actions [data-act="disable"]').count(), 1, '启用态必须有「停用」按钮');
	assert.equal(await page.locator('#cap-actions [data-act="enable"]').count(), 0, '启用态不应出现「启用」按钮');
	check('capability lifecycle: 默认启用徽标 + 停用按钮（无开关行 = enabled）');

	// ---- 2. 停用（walk 真实二次确认） ----
	await acceptDialog(page, () => page.locator('#cap-actions [data-act="disable"]').click());
	await waitBadge('已停用');
	const disableCall = (await d.calls()).findLast((c) => c.method === 'POST' && c.url.endsWith(`/capabilities/${CAP}/disable`));
	assert.ok(disableCall, '停用必须真的发 POST …/disable');
	assert.equal(disableCall.response?.status?.enabled, false, 'POST …/disable 的 200 正文必须是 status.enabled=false 的 Capability');
	assert.equal(disableCall.response?.kind, 'Capability', '停用返回的是整个 Capability 信封');
	check('capability lifecycle: 停用走真实确认 → 徽标「已停用」→ 返回整个 Capability（status.enabled=false）');
	await shot('capability-disabled');

	// ---- 3. 停用期间触发：不建任务，只留剪枝事实 ----
	const askedWhileDisabled = await fireSource(d, SRC, { diff: 'while-disabled' });
	assert.equal((askedWhileDisabled.tasks || []).length, 0, '停用期间触发源**不能**建任务');
	assert.equal((askedWhileDisabled.pruned || [])[0]?.reason, 'capability_disabled', '停用期间的剪枝原因必须是 capability_disabled');
	// 剪枝事实要能在 trace 上查到（事件面留痕，不是只在回执里说一句）。
	const trace = await d.response(`/v1/namespaces/${NS}/events?trace=${encodeURIComponent(askedWhileDisabled.traceId)}`);
	assert.equal(trace.status, 200, '按 trace 查事件必须 200');
	// 事件 wire 形状：severity 类的 `type` 刻意省略，真实事件类型在 spec.eventType；
	// 剪枝行的 reason 另在 status.pruned.reason（封闭集合）。
	const pruneRows = (trace.body.items || []).filter(
		(e) => e.spec?.eventType === 'capability_disabled' || e.status?.pruned?.reason === 'capability_disabled',
	);
	assert.equal(pruneRows.length, 1, 'trace 上必须留一条 capability_disabled 剪枝事件');
	check('capability lifecycle: 停用 → 源触发 0 任务 + pruned[capability_disabled] + trace 留痕');

	// ---- 4. 重新启用 ----
	await d.goto('capability', `namespace=${NS}&name=${encodeURIComponent(CAP)}`, 'zh-CN');
	await page.locator('#cap-enabled-badge').waitFor({ state: 'visible', timeout: 15000 });
	assert.ok((await badgeText()).includes('已停用'), `重新进入页面后停用状态必须持久（实际 ${await badgeText()}）`);
	await page.locator('#cap-actions [data-act="enable"]').click();
	await waitBadge('已启用');
	assert.equal(await page.locator('#cap-actions [data-act="disable"]').count(), 1, '启用后按钮回到「停用」');
	check('capability lifecycle: 启用 → 徽标恢复「已启用」，状态来自服务端而不是前端缓存');

	// ---- 5. 启用后同一源触发 → 建出真任务 ----
	const askedEnabled = await fireSource(d, SRC, { diff: 'after-enable' });
	assert.equal((askedEnabled.tasks || []).length, 1, '启用后触发必须建出 1 个任务');
	assert.equal((askedEnabled.pruned || []).length, 0, '启用后不应有任何剪枝');
	const taskId = askedEnabled.tasks[0].taskId;
	const task = await d.response(`/v1/namespaces/${NS}/tasks/${encodeURIComponent(taskId)}`);
	assert.equal(task.status, 200, `刚建的任务必须可读（${taskId}）`);
	assert.equal(task.body.spec?.capability?.name, CAP, '真任务的能力名必须是被测能力');
	assert.equal(task.body.status?.phase, 'pending', '真任务落在 pending（派发由节点侧接手）');
	check('capability lifecycle: 启用后同一源触发 → 真任务 pending（停用只拦自动触发，不是删功能）');

	// ---- 6. 试跑弹窗（人工触发在 disabled 上也可用，这里在 enabled 态验证主路径） ----
	await d.goto('capability', `namespace=${NS}&name=${encodeURIComponent(CAP)}`, 'zh-CN');
	await page.locator('#cap-actions [data-act="test"]').click();
	await page.locator('#test-modal').waitFor({ state: 'visible', timeout: 15000 });
	// 参数表单按 spec.params 渲染；抽不出来的 schema 降级成 JSON textarea。两种形态都驱动。
	const jsonBox = page.locator('#test-params-json');
	if (await jsonBox.count() && await jsonBox.isVisible()) {
		await jsonBox.fill('{"diff":"manual-run"}');
	} else {
		const input = page.locator('#test-params input').first();
		await input.fill('manual-run');
	}
	await page.locator('#test-submit').click();
	await page.waitForFunction(() => {
		const n = document.getElementById('test-result');
		return !!n && (n.textContent || '').includes('evt_');
	}, null, { timeout: 15000 });
	const testCall = (await d.calls()).findLast((c) => c.method === 'POST' && c.url.endsWith(`/capabilities/${CAP}/test`));
	assert.ok(testCall, '试跑必须真的发 POST …/test');
	assert.equal(testCall.response?.origin, 'test', '试跑回执的 origin 必须是 test');
	assert.equal((testCall.response?.tasks || []).length, 1, '试跑必须建出真任务（不是空跑预览）');
	const testReceipt = testCall.response;
	assert.ok(Array.isArray(testReceipt.warnings), '试跑回执必须带 warnings 数组');
	const resultText = await page.locator('#test-result').innerText();
	assert.ok(resultText.includes(testReceipt.traceId), '回执面板必须显示这次试跑的 traceId');
	check('capability lifecycle: 试跑弹窗 → 200 回执（origin=test、真任务、traceId 可见）');
	await shot('capability-test-receipt');
	await page.locator('#test-close').click();

	// ---- 7. 审计：谁、何时、什么动作（disable + test 至少两条，倒序） ----
	await d.goto('capability', `namespace=${NS}&name=${encodeURIComponent(CAP)}`, 'zh-CN');
	const auditRow = page.locator('[data-sec="audit"] #cap-audit [data-audit-action]');
	await auditRow.first().waitFor({ state: 'visible', timeout: 15000 });
	const actions = await auditRow.evaluateAll((rows) => rows.map((r) => r.getAttribute('data-audit-action')));
	assert.ok(actions.includes('disable'), `审计里必须有 disable（实际 ${JSON.stringify(actions)}）`);
	assert.ok(actions.includes('test'), `审计里必须有 test（实际 ${JSON.stringify(actions)}）`);
	assert.ok(actions.includes('enable'), `审计里必须有 enable（实际 ${JSON.stringify(actions)}）`);
	assert.equal(actions[0], 'test', `审计必须最新在前（实际 ${JSON.stringify(actions)}）`);
	check('capability lifecycle: 审计区读到 enable/disable/test 三条且倒序');
	await shot('capability-audit');
}
