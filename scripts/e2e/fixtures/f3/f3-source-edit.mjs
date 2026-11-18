// 源详情页「编辑源」必须进入**编辑器**，而且链接里的 namespace 必须是**资源自己的**。
//
// 用户报的现象（anotify-dev）：在 `source-detail.html?namespace=default&name=<src>`
// 点「编辑源」，地址变成 `source.html?namespace=default&name=<src>`（参数看着对），
// 但落地的不是编辑表单。
//
// 这个用例只钉两条产品事实，两条都对用户可见：
//   1. 「编辑源」的终点是**编辑态**：`sources.html?namespace=<res-ns>&edit=<name>`，
//      弹层打开且表单里就是这个源（`#src-modal:not(.hidden)` + `#f-name` 回填）。
//      落到别的页面（尤其是另一张只读详情页）就是「跳走 / 不是编辑表单」。
//   2. 链接里的 namespace 取**资源自己的 ns**（`metadata.namespace`），不取
//      localStorage 的 scope：深链永远由 URL 决定，持久化范围与资源 ns 不一致时
//      也不许把用户带到错误的 ns（那会 404 成空态）。这里用「scope=prod + 资源在
//      default」把两个来源做成可区分的，判据不是时长而是 URL 参数。
// 附带同族检查：能力详情页的「返回能力列表」也必须落在资源自己的 ns 上（同一天
// 合入的两个详情页共用同一段 scope 拼链接的写法）。
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
	return JSON.stringify({ url: d.page.url(), nav, scope, calls, ...extra }, null, 2);
};

export async function sourceEditJump(d) {
	const { page, goto, check, api } = d;
	// 期望值从真实 API 现算（不写死夹具名字），所以只能在 real authority 上跑：
	// 首页/详情页的 ns 归属是真实后端的授权与 404 语义，mock 预览里没有这些。
	assert.ok(!d.mock, 'source-edit-jump 只属于 real authority（期望值从 /v1/namespaces/*/sources 现算）');
	// 导航链：一次点击触发了哪些文档级导航（"跳走"要看得到，不靠猜）。
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

	// ① 持久化 scope 故意与资源 ns 不一致：链接必须跟资源走，不跟 localStorage 走。
	await goto('source-detail', 'namespace=default&name=' + srcName, 'zh-CN');
	await page.evaluate(() => localStorage.setItem('anotify.scope', JSON.stringify({ namespace: 'prod' })));
	await goto('source-detail', 'namespace=default&name=' + srcName, 'zh-CN');
	await page.waitForFunction((n) => document.querySelector('#src-detail')?.getAttribute('data-source') === n, srcName, { timeout: 20000 });
	const href = await page.locator('#src-detail-edit').getAttribute('href');
	assert.ok(href, '详情页的「编辑源」必须有 href，实际 ' + href);
	check('#src-detail-edit href = ' + href);

	d.nav.length = 0;
	const before = page.url();
	await page.locator('#src-detail-edit').click();
	// 导航到**任何**别的文档都算发生了（不是拿期望值当等待条件，否则失败现场只剩超时）。
	await page.waitForURL((u) => u.href !== before, { timeout: 20000, waitUntil: 'domcontentloaded' });
	// 落地那一页要真的画完：只读页有自己的容器，编辑器是 sources.html 的弹层。
	const state = await page.evaluate(() => ({
		page: document.body?.dataset?.page || '',
		editor: !!document.querySelector('#src-modal'),
		readonlyDetail: !!document.querySelector('#src-detail'),
		flowDetail: !!document.querySelector('#src-body'),
		title: (document.querySelector('#src-name, #src-detail-name, .page-title')?.textContent || '').trim(),
	}));
	const landed = new URL(page.url());
	assert.equal(
		landed.pathname.replace(/^\/(en|ja|es)\//, '/').replace(/^\//, ''),
		'sources.html',
		'「编辑源」必须落在编辑器所在的 sources.html（源只在那一页可编辑），实际 ' +
			(await facts(d, { href, landedState: state })),
	);
	assert.equal(
		landed.searchParams.get('namespace'),
		'default',
		'编辑链接的 namespace 必须是**资源自己的** ns（default），不是 localStorage 的 prod；实际 ' + (await facts(d, { href })),
	);
	assert.equal(
		landed.searchParams.get('edit'),
		srcName,
		'编辑链接必须用 ?edit=<name> 打开该源的编辑器，实际 ' + (await facts(d, { href })),
	);
	// 落地的必须是**编辑态**，不是另一张只读页：弹层打开 + 表单回填到这个源。
	await page.locator('#src-modal:not(.hidden)').waitFor({ state: 'visible', timeout: 20000 });
	await page.waitForFunction(
		({ name, sel }) => document.querySelector(sel)?.value === name,
		{ name: srcName, sel: '#f-name' },
		{ timeout: 20000 },
	);
	assert.equal(await page.locator('#f-name').inputValue(), srcName, '编辑弹层必须回填这个源的名字');
	assert.equal(
		await page.locator('#src-detail').count(),
		0,
		'目的地不许还是只读详情页（source-detail 的容器不在这个页面上），实际 ' + (await facts(d)),
	);
	// 错 ns 的请求一条都不许有：那是「404 → 空态」的另一条路。
	const pageCalls = await page.evaluate(() => window.__f3Calls || []);
	const wrongNs = pageCalls.filter((c) => /\/v1\/namespaces\/prod\//.test(c.url));
	assert.deepEqual(wrongNs.map((c) => c.url), [], '编辑流程不许请求 prod 的资源（scope 串到了链接里），实际 ' + (await facts(d)));
	assert.ok(
		pageCalls.some((c) => c.url.startsWith('/v1/namespaces/default/sources')),
		'编辑弹层必须在 default 里读源（列表页的 SOURCES 已包含该源时不再单对象读，所以不钉具体端点），实际 ' + (await facts(d)),
	);
	check('SOURCE-EDIT-DETAIL 编辑源 → sources.html?namespace=default&edit=<name>，弹层打开且回填（scope=prod 也不串）');

	// ② 全新浏览器（localStorage 没有 scope）：深链的 namespace 仍然说了算。
	await page.evaluate(() => localStorage.removeItem('anotify.scope'));
	await goto('source-detail', 'namespace=default&name=' + srcName, 'zh-CN');
	await page.waitForFunction((n) => document.querySelector('#src-detail')?.getAttribute('data-source') === n, srcName, { timeout: 20000 });
	const emptyScopeHref = await page.locator('#src-detail-edit').getAttribute('href');
	assert.equal(
		new URL(emptyScopeHref, 'http://x/').searchParams.get('namespace'),
		'default',
		'scope 为空时编辑链接也必须带资源自己的 ns（深链不依赖 localStorage），实际 ' + emptyScopeHref,
	);
	check('SOURCE-EDIT-NOSCOPE scope 为空时编辑链接仍是 namespace=default（' + emptyScopeHref + '）');

	// ③ 能力详情页（同一天合入的另一张详情页）：返回入口同样必须落在资源自己的 ns。
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
