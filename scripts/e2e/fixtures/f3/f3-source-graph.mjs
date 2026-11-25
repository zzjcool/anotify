import assert from 'node:assert/strict'; // 源详情页链路图（#652）：① 图上有源 + 3 个能力 + 3 条边（源→claim，claim→dispatch/gate）；② 图库缺失只留一行说明、其余区块照常

const NS = '/v1/namespaces/default'; // 夹具全部走真实 API（mock 权威下由 MockApi 承接），不写库、不伪造
const SOURCE = 'graph-chain';
const HOST = '#src-detail-graph'; // 页面新增的图宿主（既有 DOM id 一个都没动）
const GRAPHS_JS = /\/graphs(\.[0-9a-f]+)?\.js(?:\?.*)?$/; // 产物里的库带指纹（graphs.<hash>.js），mock 预览是 /graphs.js —— 两种都要拦到
const CHAIN = [
	['graph-dispatch', []],
	['graph-gate', []], // 先叶子后上游：真实服务要求 spec.next 指向的能力已经存在
	['graph-claim', ['graph-dispatch', 'graph-gate']],
];
const WANT_NODES = ['__source__', 'graph-claim', 'graph-dispatch', 'graph-gate']; // 源节点由渲染层的触发器节点承担
const WANT_EDGES = ['__source__→graph-claim', 'graph-claim→graph-dispatch', 'graph-claim→graph-gate']; // 拓扑：1 条入口 + 2 条下游

async function ensureCapability(d, name, next) {
	const body = {
		apiVersion: 'anotify/v1',
		kind: 'Capability',
		metadata: { name },
		spec: { next, run: { entrypoint: 'run.sh', files: { 'run.sh': '#!/bin/sh\necho ' + name + '\n' } } }, // 入口必须带 shebang（真实服务回 400 entrypoint_no_shebang）
	};
	const created = await d.response(NS + '/capabilities', body, 'POST');
	assert.equal(created.status, 201, 'fixture: create ' + name + ' 必须 201（内容寻址的 POST 复用同内容版本行仍回 201），实际 ' + created.status + ' ' + JSON.stringify(created.body));
	return created.body;
}

async function ensureSource(d) {
	const body = {
		apiVersion: 'anotify/v1',
		kind: 'Source',
		metadata: { name: SOURCE },
		spec: { kind: 'manual', targets: [{ capability: 'graph-claim' }] }, // manual 不认 envelope/token，链路图要的只是 targets 这一条入口边
	};
	const created = await d.response(NS + '/sources', body, 'POST');
	assert.ok([201, 409].includes(created.status), 'fixture: create ' + SOURCE + ' 必须 201/409（409 source_name_used = 源已在，夹具幂等），实际 ' + created.status + ' ' + JSON.stringify(created.body));
	return created;
}

const readGraph = (page, want) => // 三样事实：DOM 节点（用户看得见的）、host._graphSpec（拓扑到底连成什么形状）、宿主是否真挂了图
	page.evaluate(async ([sel, target, budget]) => { // 图是异步挂的（await graphLib + 拓扑取数）：有界地等节点数到位，到点把实测值交回去由断言说清差在哪（不吞异常）
		const count = () => document.querySelectorAll(sel + ' [data-node-id]').length;
		const started = Date.now();
		while (count() !== target && Date.now() - started < budget) await new Promise((r) => setTimeout(r, 50));
		const host = document.querySelector(sel);
		if (!host) return { waited: Date.now() - started, dom: [], spec: null, mounted: false, text: '' };
		const spec = host._graphSpec;
		return {
			waited: Date.now() - started,
			dom: [...host.querySelectorAll('[data-node-id]')].map((n) => n.getAttribute('data-node-id')),
			spec: spec ? { nodes: spec.nodes.map((n) => n.id), edges: spec.edges.map((e) => e.from + '→' + e.to) } : null,
			mounted: host.classList.contains('flograph'),
			text: (host.textContent || '').trim().slice(0, 200),
		};
	}, [HOST, want, 12000]);

export async function sourceDetailGraph(d) {
	const { page, goto, check } = d;
	await goto('sources'); // 造数用相对路径 fetch（d.response 在页面里发）：必须先有一份同源文档
	for (const [name, next] of CHAIN) await ensureCapability(d, name, next);
	await ensureSource(d);

	await goto('source-detail', 'namespace=default&name=' + SOURCE, 'zh-CN');
	await page.waitForFunction((name) => document.querySelector('#src-detail')?.getAttribute('data-source') === name, SOURCE, { timeout: 20000 });
	const drawn = await readGraph(page, WANT_NODES.length);
	assert.equal(drawn.dom.length, WANT_NODES.length, '图必须真画出来（' + WANT_NODES.length + ' 节点），实际 ' + JSON.stringify(drawn));
	assert.equal(drawn.mounted, true, '宿主上必须真的挂了图（.flograph），实际 ' + JSON.stringify(drawn.text));
	assert.deepEqual(drawn.dom.slice().sort(), WANT_NODES.slice().sort(), '图上必须是源 + 三个能力，实际 ' + drawn.dom.join(','));
	assert.ok(drawn.spec, 'host._graphSpec 必须存在（图的拓扑事实）');
	assert.deepEqual(drawn.spec.edges.slice().sort(), WANT_EDGES.slice().sort(), '边的形状必须是 源→claim、claim→dispatch/gate，实际 ' + drawn.spec.edges.join(' '));
	check('SOURCE-GRAPH 图上有源 + 3 个能力（' + WANT_NODES.length + ' 节点 / ' + WANT_EDGES.length + ' 条边：源→graph-claim、graph-claim→graph-dispatch/gate）');

	await d.context.route(GRAPHS_JS, (route) => route.fulfill({ status: 200, contentType: 'text/javascript', body: '/* graph library withheld */\n' })); // 降级现场：库换成空脚本（等价「库没加载」），不制造网络失败以免污染 pageerror 断言
	try {
		await goto('source-detail', 'namespace=default&name=' + SOURCE, 'zh-CN');
		const nolib = await page.evaluate(() => window.Anotify.t('sources.flow.nolib'));
		await page.waitForFunction(([sel, want]) => (document.querySelector(sel)?.textContent || '').trim() === want, [HOST, nolib], { timeout: 20000 }); // 沿用 locale 已有的 nolib 文案，不是新造的一句
		assert.equal(await page.locator(HOST + ' [data-node-id]').count(), 0, '库没加载时不许留半张图');
		const rest = await page.evaluate(() => ({
			title: (document.querySelector('#src-detail-name')?.textContent || '').trim(),
			targets: !!document.querySelector('[data-sec="targets"]'),
			recent: !!document.querySelector('#src-detail-recent'),
			size: (document.querySelector('#src-detail-body')?.textContent || '').trim().length,
		}));
		assert.ok(rest.title.includes('graph-chain'), '图降级不许影响页头，实际 ' + rest.title);
		assert.equal(rest.targets, true, '目标能力区块仍在');
		assert.equal(rest.recent, true, '近期回执区块仍在');
		assert.ok(rest.size > 200, '页面其余内容仍然可读，实际字符数 ' + rest.size);
		check('SOURCE-GRAPH-NOLIB 图库缺失 → 图上只剩 locale 已有的 nolib 一行说明（0 节点），页头/目标/近期回执照常');
	} finally {
		await d.context.unroute(GRAPHS_JS);
	}
}
