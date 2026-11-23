// 源详情页「编辑源」= **就地编辑**（不离开详情页），且编辑器与列表页共用同一份组件。
//
// 用户报过两次：
//   ① 点「编辑源」落到 `source.html`（当时还存在的一张只读页，现已删除 —— 见 source-actions
//      里的 404 断言）→「点了编辑却跳走、看不到表单」；
//   ② 修成 `sources.html?edit=<name>` 后仍不满意 —— 「为什么会跳转到源页面然后在上面弹窗编辑」。
// 也就是说：目标是编辑器还不够，**在详情页点编辑就应该在详情页编辑**。
//
// 本用例钉四条用户可见事实：
//   1. 点 `#src-detail-edit` 后**没有文档级导航**（URL 仍是 source-detail.html，只多 `edit=1`），
//      弹层 `#src-modal` 在本页打开且 `#f-name` 就是这个源；
//   2. 保存（PATCH）后：弹层关闭、`edit=1` 被移除、**详情页立刻显示新值**（保存能看到结果），
//      且写请求带 `metadata.resourceVersion`（CAS 语义没被这次重构弄丢）；
//   3. 深链 `?edit=1` 直接打开编辑器（刷新/分享同一个 URL 都能工作）；
//   4. 编辑流程的请求全部落在**资源自己的 ns**（default），一条 `/v1/namespaces/prod/` 都不许有。
import assert from 'node:assert/strict';

/** 把失败时需要的现场（URL / 导航链 / 请求 / localStorage）一次性附在错误里。 */
const facts = async (d, extra) => {
	const nav = d.nav || [];
	let scope = null;
	try {
		scope = await d.page.evaluate(() => localStorage.getItem('anotify.scope'));
	} catch {
		/* 页面可能还没到 origin */
	}
	let calls = [];
	try {
		calls = (await d.page.evaluate(() => window.__f3Calls || [])).map((c) => c.method + ' ' + c.url + ' -> ' + c.httpStatus);
	} catch {
		/* 同上 */
	}
	const dom = await d.page
		.evaluate(() => ({
			modalVisible: !!document.querySelector('#src-modal:not(.hidden)'),
			modalExists: !!document.querySelector('#src-modal'),
			detail: !!document.querySelector('#src-detail'),
			name: (document.querySelector('#src-detail-name')?.textContent || '').trim(),
			title: document.querySelector('#f-name')?.value ?? null,
		}))
		.catch(() => ({}));
	return JSON.stringify({ url: d.page.url(), nav, scope, calls, dom, ...extra }, null, 2);
};

export async function sourceEditJump(d) {
	const { page, goto, check, api } = d;
	// 期望值从真实 API 现算（不写死夹具名字），所以只能在 real authority 上跑：
	// ns 归属与 PATCH/CAS 是真实后端的语义，mock 预览里没有这些。
	assert.ok(!d.mock, 'source-edit-jump 只属于 real authority（期望值从 /v1/namespaces/*/sources 现算）');
	d.nav = [];
	page.on('framenavigated', (frame) => {
		if (frame === page.mainFrame()) d.nav.push(frame.url());
	});

	// 期望值全部从真实 API 现算：不写死夹具名字。
	const srcList = await api('/v1/namespaces/default/sources');
	assert.equal(srcList.status, 200, 'GET /v1/namespaces/default/sources 必须 200，实际 ' + srcList.status);
	const source = (srcList.body.items || [])[0];
	assert.ok(source, '夹具必须让 default 至少有一个源（否则本用例是空转）');
	const srcName = source.metadata.name;
	assert.equal(source.metadata.namespace, 'default', '源必须属于 default（夹具前提），实际 ' + source.metadata.namespace);

	// ① 持久化 scope 故意与资源 ns 不一致：编辑流程必须跟资源走，不跟 localStorage 走。
	await goto('source-detail', 'namespace=default&name=' + srcName, 'zh-CN');
	await page.evaluate(() => localStorage.setItem('anotify.scope', JSON.stringify({ namespace: 'prod' })));
	await goto('source-detail', 'namespace=default&name=' + srcName, 'zh-CN');
	await page.waitForFunction((n) => document.querySelector('#src-detail')?.getAttribute('data-source') === n, srcName, { timeout: 20000 });

	const before = page.url();
	d.nav.length = 0;
	/* 「就地」= 同一个文档：先在 window 上留个记号，点完还在就说明没有整页导航。
	   （history.replaceState 也会触发 framenavigated，所以不能用 d.nav 为空当判据。） */
	await page.evaluate(() => {
		window.__f3SameDocument = 'yes';
	});
	await page.locator('#src-detail-edit').click();
	// 就地编辑的判据：URL 还是这一页（只多 edit=1），而且**没有发生文档级导航**。
	await page.waitForFunction(() => new URLSearchParams(location.search).get('edit') === '1', null, { timeout: 20000 });
	await page.locator('#src-modal:not(.hidden)').waitFor({ state: 'visible', timeout: 20000 });
	await page.waitForFunction(({ name, sel }) => document.querySelector(sel)?.value === name, { name: srcName, sel: '#f-name' }, { timeout: 20000 });

	assert.equal(
		await page.evaluate(() => window.__f3SameDocument ?? null),
		'yes',
		'「编辑源」必须就地打开（同一个文档，不许整页导航 —— 跳走就是这次要修掉的体验），实际 ' + (await facts(d)),
	);
	assert.equal(new URL(page.url()).pathname, new URL(before).pathname, '点编辑后仍应停在 source-detail.html，实际 ' + (await facts(d)));
	assert.equal(await page.locator('#f-name').inputValue(), srcName, '编辑弹层必须回填这个源的名字，实际 ' + (await facts(d)));
	assert.equal(await page.locator('#src-detail').count(), 1, '编辑时详情页仍在（就地编辑），实际 ' + (await facts(d)));
	/* 弹层文案是运行期通过 A.t() 渲染的（DOM 由共享组件注入），zh-CN 下必须是译文而不是 key。 */
	const titleText = (await page.locator('#modal-title').innerText()).trim();
	const wantTitle = await page.evaluate(() => window.Anotify.t('sources.v4.edit'));
	assert.equal(titleText, wantTitle, '弹层的标题必须走 i18n（运行期 A.t），实际 ' + titleText);
	assert.ok(!titleText.includes('sources.v4.'), '弹层标题不许漏出 i18n key，实际 ' + titleText);
	check('SOURCE-EDIT-INLINE 编辑源 → 就地打开 #src-modal（无导航，URL 仅加 edit=1，表单回填 ' + srcName + '）');

	// 同一次编辑里：请求必须全部落在资源自己的 ns 上。
	const inlineCalls = await page.evaluate(() => window.__f3Calls || []);
	const wrongNs = inlineCalls.filter((c) => /\/v1\/namespaces\/prod\//.test(c.url));
	assert.deepEqual(wrongNs.map((c) => c.url), [], '编辑流程不许请求 prod 的资源（scope 串进了编辑器），实际 ' + (await facts(d)));
	check('SOURCE-EDIT-NS 编辑流程零 /v1/namespaces/prod/ 请求（scope=prod 也不串）');

	// ② 保存 → 弹层关闭 + edit=1 移除 + 详情页立刻显示新值。
	const marker = srcName + '-inline-e2e';
	await page.locator('#f-display').fill(marker);
	/* 先挂上等待再点：超时直接抛（比 .catch(()=>null) 更好 —— 失败现场是「没发出 PATCH」本身）。 */
	const saveRespP = page.waitForResponse(
		(r) => r.request().method() === 'PATCH' && /\/v1\/namespaces\/default\/sources\//.test(r.url()),
		{ timeout: 20000 },
	);
	const saveReqP = page.waitForRequest((r) => r.method() === 'PATCH' && /\/v1\/namespaces\/default\/sources\//.test(r.url()), { timeout: 20000 });
	await page.locator('#modal-save').click();
	const [req, resp] = await Promise.all([saveReqP, saveRespP]);
	const sent = JSON.parse(req.postData() || '{}');
	assert.equal(sent.apiVersion, 'anotify/v1', '写信封apiVersion必须保持 anotify/v1，实际 ' + JSON.stringify(sent));
	assert.ok(
		sent.metadata && sent.metadata.resourceVersion,
		'PATCH 必须带 metadata.resourceVersion（CAS；抽共享模块时最容易丢的就是这条），实际 ' + JSON.stringify(sent.metadata),
	);
	assert.equal(sent.metadata.name, srcName, 'PATCH 的 metadata.name 必须是要编辑的那个源，实际 ' + sent.metadata.name);
	assert.equal(resp.status(), 200, 'PATCH 必须 200，实际 ' + resp.status());

	await page.waitForFunction(() => document.querySelector('#src-modal').classList.contains('hidden'), null, { timeout: 20000 });
	await page.waitForFunction(() => !new URLSearchParams(location.search).get('edit'), null, { timeout: 20000 });
	try {
		await page.waitForFunction(
			(m) => (document.querySelector('#src-detail-name')?.textContent || '').includes(m),
			marker,
			{ timeout: 20000 },
		);
	} catch (e) {
		throw new Error('保存后详情标题没有出现新值 ' + marker + '，现场 ' + (await facts(d, { state: await page.locator('#src-detail-state').innerText().catch(() => '?') })));
	}
	const nameText = (await page.locator('#src-detail-name').innerText()).trim();
	assert.ok(nameText.includes(marker), '保存后详情页必须显示新值（保存要能看到结果），实际标题 ' + nameText + '，现场 ' + (await facts(d)));
	assert.equal(new URL(page.url()).pathname.endsWith('/source-detail.html'), true, '保存后仍应停在详情页，实际 ' + (await facts(d)));
	check('SOURCE-EDIT-SAVE 保存后弹层关闭、edit=1 移除、详情页显示新值（' + marker + '）');

	// ③ 深链 `?edit=1`：直接打开编辑器（不点任何按钮）。
	await goto('source-detail', 'namespace=default&name=' + srcName + '&edit=1', 'zh-CN');
	await page.locator('#src-modal:not(.hidden)').waitFor({ state: 'visible', timeout: 20000 });
	await page.waitForFunction(({ name, sel }) => document.querySelector(sel)?.value === name, { name: srcName, sel: '#f-name' }, { timeout: 20000 });
	assert.equal(new URL(page.url()).searchParams.get('edit'), '1', '深链 edit=1 必须保持，实际 ' + (await facts(d)));
	check('SOURCE-EDIT-DEEPLINK ?edit=1 直接打开编辑器并回填（深链可分享/可刷新）');
	await page.locator('#modal-cancel').click();
	await page.waitForFunction(() => !new URLSearchParams(location.search).get('edit'), null, { timeout: 20000 });

	// ④ 能力详情页（同一天合入的另一张详情页）：返回入口必须落在资源自己的 ns。
	const capList = await api('/v1/namespaces/default/capabilities');
	assert.equal(capList.status, 200, 'GET /v1/namespaces/default/capabilities 必须 200，实际 ' + capList.status);
	const cap = (capList.body.items || [])[0];
	assert.ok(cap, '夹具必须让 default 至少有一个能力（否则本用例是空转）');
	await page.evaluate(() => localStorage.setItem('anotify.scope', JSON.stringify({ namespace: 'prod' })));
	await goto('capability', 'namespace=default&name=' + cap.metadata.name, 'zh-CN');
	await page.waitForFunction(
		(n) => document.querySelector('#cap-actions [data-act="edit"]') !== null && location.search.includes('name=' + n),
		cap.metadata.name,
		{ timeout: 20000 },
	);
	const backHref = await page.locator('#cap-back').getAttribute('href');
	assert.equal(
		new URL(backHref, 'http://x/').searchParams.get('namespace'),
		'default',
		'能力详情页的返回链接必须用资源自己的 ns（default），不是 localStorage 的 prod，实际 ' + backHref,
	);
	check('CAP-DETAIL-BACK 能力详情返回链接 namespace=default（资源 ns，不是 scope 的 prod）：' + backHref);
}
