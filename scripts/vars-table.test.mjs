#!/usr/bin/env node
/* Anotify · vars 表格纯逻辑单测（#476）
 *
 * 拦的事故类型：表格的「缺但被要求」判据写宽松（把「本层没有」当成「缺」）→ 用户被
 * 教去改一个改不动的东西；或者行聚合漏掉某一层（namespace / local）→ 页面显示的值与
 * `status.effectiveVars` 对不上。这两种静态检查（check-classes / check-i18n-keys）都
 * 看不见，只有把真实形状的数据喂进渲染逻辑才会失败。
 *
 * 判据（三层，缺一层就等于没查）：
 *   ① 行为：在 vm 里真实执行 web/vars-table.js，喂 `GET /v1/nodes/{name}?namespace=`
 *      的真实形状（effectiveVars {value,source} + machineVars/localVars/namespaceVars）
 *      与能力读面（spec.requires.values），断言行的 name/value/source/missing/usedBy。
 *   ② 变异自证：把 `missing` 判据改成恒 false 重新加载，① 的「缺但被要求」断言必须变红
 *      —— 否则绿可能只是断言写松了。
 *
 * 跑法：`node --test scripts/vars-table.test.mjs`（接线：`make test-frontend`）
 */
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import vm from 'node:vm';
import test from 'node:test';
import assert from 'node:assert/strict';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const MODULE_PATH = 'web/vars-table.js';
const MODULE_SRC = readFileSync(path.join(ROOT, MODULE_PATH), 'utf8');

/** 把 vm 里造出来的对象/数组折回本 realm（跨 realm 的 deepStrictEqual 会因原型不同报错）。 */
const plain = (value) => JSON.parse(JSON.stringify(value));

/** 在独立 vm context 里执行模块源码，返回 window.AnotifyVars。 */
function load(src = MODULE_SRC) {
	const ctx = vm.createContext({ console, JSON, Object, Array, String, Number, RegExp });
	vm.runInContext("globalThis.window = globalThis;", ctx);
	vm.runInContext(src, ctx, { filename: MODULE_PATH });
	const api = ctx.window && ctx.window.AnotifyVars;
	assert.ok(api, 'vars-table.js 必须挂载 window.AnotifyVars');
	return api;
}

/* ------------------------------------------------------------------ *
 * 真实形状：GET /v1/nodes/anode-1?namespace=default 的 status 段
 * （internal/server/resource_node.go · nodeStatus + store.EffectiveVar）
 * 与 GET /v1/namespaces/default/capabilities 的 items[]。
 * ------------------------------------------------------------------ */
const NODE_STATUS = {
	// local > node > namespace：服务端逐键解析后的真相
	effectiveVars: {
		SOURCE_ROOT: { value: '/local-src', source: 'local' },
		NODE_ONLY: { value: 'node-default', source: 'node' },
		NS_ONLY: { value: 'namespace-default', source: 'namespace' },
	},
	localVars: { SOURCE_ROOT: '/local-src' },
	nodeVars: { NODE_ONLY: 'node-default' },
	machineVars: { SOURCE_ROOT: '/local-src', NODE_ONLY: 'node-default' },
	namespaceVars: { NS_ONLY: 'namespace-default' },
};

const CAPABILITIES = [
	{
		metadata: { name: 'build', version: 3 },
		spec: { requires: { values: ['vars.SOURCE_ROOT', 'vars.JAVA_HOME'] } },
	},
	{
		metadata: { name: 'test', version: 1 },
		spec: { requires: { values: ['vars.JAVA_HOME', 'vars.SOURCE_ROOT'] } },
	},
	// 没有 requires 的能力：不产生任何行
	{ metadata: { name: 'notify', version: 2 }, spec: { run: { entrypoint: 'run.sh' } } },
];

test('checkVarName：引用形状 + 服务端权威形状两层拦截', () => {
	const V = load();
	assert.equal(V.checkVarName('SOURCE_ROOT'), null);
	assert.equal(V.checkVarName('_PRIVATE'), 'server', '引用形状允许下划线开头，但服务端要求 A-Z 开头');
	assert.equal(V.checkVarName('A'), null);
	assert.equal(V.checkVarName('1FOO'), 'shape', '数字开头 → 引用形状就不符');
	assert.equal(V.checkVarName('FOO-BAR'), 'shape', '连字符不是名字字符');
	assert.equal(V.checkVarName('FOO.BAR'), 'shape');
	assert.equal(V.checkVarName(''), 'empty');
	assert.equal(V.checkVarName(null), 'empty');
	assert.equal(V.checkVarName('lower_case'), 'server', '引用得到，但服务端只收大写');
	assert.equal(V.checkVarName('A'.repeat(65)), 'server', '超 64 字符服务端会 400');
});

test('requiredNames：聚合成「名字 → 引用它的能力（name@version）」', () => {
	const V = load();
	const required = V.requiredNames(CAPABILITIES);
	assert.deepEqual(plain(Object.keys(required).sort()), ['JAVA_HOME', 'SOURCE_ROOT']);
	assert.deepEqual(plain(required.JAVA_HOME), ['build@3', 'test@1']);
	assert.deepEqual(plain(required.SOURCE_ROOT), ['build@3', 'test@1']);
	assert.equal(V.requiredNames(null) && Object.keys(V.requiredNames(null)).length, 0);
	// 非 vars. 前缀的元素不是变量需求（契约只认 vars.<NAME>），不聚合。
	assert.deepEqual(plain(V.requiredNames([{ metadata: { name: 'x', version: 1 }, spec: { requires: { values: ['params.x', 'vars.OK'] } } }])), { OK: ['x@1'] });
});

test('buildRows：五列数据（名字/值/来源/被谁要/缺）与 effectiveVars 一致', () => {
	const V = load();
	const required = V.requiredNames(CAPABILITIES);
	const rows = V.buildRows({ ...NODE_STATUS, required, layer: 'node' });
	const byName = Object.fromEntries(rows.map((r) => [r.name, r]));
	assert.deepEqual(plain(Object.keys(byName)), ['JAVA_HOME', 'NODE_ONLY', 'NS_ONLY', 'SOURCE_ROOT']);

	assert.deepEqual(plain(byName.SOURCE_ROOT), {
		name: 'SOURCE_ROOT', value: '/local-src', source: 'local',
		usedBy: ['build@3', 'test@1'], missing: false, locked: true, shadowed: false,
		owned: false,
	});
	assert.equal(byName.NODE_ONLY.value, 'node-default');
	assert.equal(byName.NODE_ONLY.source, 'node');
	assert.equal(byName.NS_ONLY.source, 'namespace');
	assert.equal(byName.NS_ONLY.value, 'namespace-default');
	assert.equal(byName.SOURCE_ROOT.locked, true, '节点页改不了本机快照（anode.yaml 不在中枢）');
	assert.equal(byName.NODE_ONLY.locked, false);
	assert.equal(byName.NODE_ONLY.owned, true, '节点默认层：node 来源的行属于这份 Node 文档');
	assert.equal(byName.NS_ONLY.owned, false, '命名空间默认值的行不属于 Node 文档（不能抄成 node 默认）');

	// 被要求但**没有任何层**提供值 → 红标；被要求且有值（哪怕来自 namespace 层）不算缺。
	assert.equal(byName.JAVA_HOME.missing, true);
	assert.equal(byName.JAVA_HOME.value, null);
	assert.equal(byName.JAVA_HOME.source, '');
	assert.deepEqual(plain(byName.JAVA_HOME.usedBy), ['build@3', 'test@1']);
	assert.equal(byName.NODE_ONLY.missing, false);
	assert.equal(byName.NS_ONLY.missing, false);
});

test('buildRows：命名空间层把 local/node 行标成 shadowed（可写但压不住上层）', () => {
	const V = load();
	const rows = V.buildRows({ ...NODE_STATUS, required: V.requiredNames(CAPABILITIES), layer: 'namespace' });
	const byName = Object.fromEntries(rows.map((r) => [r.name, r]));
	assert.equal(byName.SOURCE_ROOT.shadowed, true);
	assert.equal(byName.NODE_ONLY.shadowed, true);
	assert.equal(byName.NS_ONLY.shadowed, false);
	assert.equal(byName.JAVA_HOME.shadowed, false);
	assert.equal(byName.NS_ONLY.owned, true, '命名空间层：namespace 来源的行属于这份 Namespace 文档');
	assert.equal(byName.NODE_ONLY.owned, false, '别的层提供的值不抄进命名空间默认值');
});

test('buildRows：没有 ns 上下文时退回机器级两层，需求列为空（不编造红标）', () => {
	const V = load();
	const rows = V.buildRows({
		machine: { SOURCE_ROOT: '/local-src', NODE_ONLY: 'node-default' },
		local: { SOURCE_ROOT: '/local-src' },
		layer: 'node',
	});
	assert.deepEqual(plain(rows.map((r) => [r.name, r.value, r.source, r.missing, r.owned])), [
		['NODE_ONLY', 'node-default', 'node', false, true],
		['SOURCE_ROOT', '/local-src', 'local', false, false],
	]);
});

test('rowsToVars：只收有值的行；missing 与 deleted 都不写', () => {
	const V = load();
	const required = V.requiredNames(CAPABILITIES);
	const rows = V.buildRows({ ...NODE_STATUS, required, layer: 'node' });
	const vars = V.rowsToVars(rows);
	// 只写本层（Node.spec.vars）：namespace / local 的值不抄进来。
	assert.deepEqual(plain(vars), { NODE_ONLY: 'node-default' });
	// 删除一个已存在的键 → 只少那一个键。
	assert.deepEqual(plain(V.rowsToVars(rows.map((r) => (r.name === 'NODE_ONLY' ? { ...r, deleted: true } : r)))), {});
	// 填入一个缺失键（页面把该行标成 owned）→ 只多那一个键。
	const filled = V.rowsToVars(rows.map((r) => (r.name === 'JAVA_HOME' ? { ...r, value: '/usr/lib/jvm', owned: true } : r)));
	assert.equal(filled.JAVA_HOME, '/usr/lib/jvm');
	assert.deepEqual(plain(Object.keys(filled).sort()), ['JAVA_HOME', 'NODE_ONLY']);
	// 别的层的行即使有值也不会被写进本层默认值（行编辑后才会置 owned）。
	assert.deepEqual(plain(V.rowsToVars(rows.map((r) => ({ ...r, owned: false })))), {});
});

test('rowsToVars：文档里已有、但被上层压住的键不会被表格保存顺手删掉', () => {
	const V = load();
	const rows = V.buildRows({
		effective: { A: { value: 'localA', source: 'local' }, B: { value: 'nodeB', source: 'node' } },
		machine: { A: 'nodeA', B: 'nodeB' },
		namespaceVars: {},
		required: {},
		layer: 'node',
	});
	const a = rows.find((r) => r.name === 'A'), b = rows.find((r) => r.name === 'B');
	assert.equal(a.owned, false, '被 local 压住 → 不属于本层文档（表格里锁定）');
	assert.equal(a.locked, true);
	assert.equal(b.owned, true, '本层的行才是可写的那一份');

	const kept = plain(V.rowsToVars(rows, { A: 'nodeA', B: 'old' }));
	assert.equal(kept.A, 'nodeA', '保留下来的必须是**文档里**的值，不是表格显示的生效值 localA');
	assert.equal(kept.B, 'nodeB', 'owned 行用行里的值覆盖');

	const deleted = plain(V.rowsToVars(rows.map((r) => (r.name === 'A' ? { ...r, deleted: true } : r)), { A: 'nodeA', B: 'old' }));
	assert.deepEqual(deleted, { B: 'nodeB' }, '用户显式删掉一个被压住的键 → 才从文档里去掉');

	const other = V.buildRows({
		effective: { N: { value: 'nsv', source: 'namespace' } },
		machine: {},
		namespaceVars: { N: 'nsv' },
		required: {},
		layer: 'node',
	});
	assert.deepEqual(plain(V.rowsToVars(other, {})), {}, '别的层才有、文档里没有的名字不许抄进来');
	assert.deepEqual(plain(V.rowsToVars(rows, { Z: 'stale' })), { Z: 'stale', B: 'nodeB' },
		'文档里有、表格里根本没有对应行的键也不许丢（表格不是文档的全集）');
});

test('suggestNames：现有名字 ∪ 能力要求但缺失的名字；删除的行不算', () => {
	const V = load();
	const required = V.requiredNames(CAPABILITIES);
	const rows = V.buildRows({ ...NODE_STATUS, required, layer: 'node' });
	const suggestions = V.suggestNames(rows, required);
	assert.ok(suggestions.includes('JAVA_HOME'), '缺失但被要求 → 必须出现在下拉建议里');
	for (const name of ['SOURCE_ROOT', 'NODE_ONLY', 'NS_ONLY']) assert.ok(suggestions.includes(name));
	const dropped = V.suggestNames(rows.map((r) => (r.name === 'NS_ONLY' ? { ...r, deleted: true } : r)), required);
	assert.ok(!dropped.includes('NS_ONLY'));
});

test('renameImpact：改名提示引用它的能力数', () => {
	const V = load();
	const rows = V.buildRows({ ...NODE_STATUS, required: V.requiredNames(CAPABILITIES), layer: 'node' });
	assert.equal(V.renameImpact(rows, 'SOURCE_ROOT'), 2);
	assert.equal(V.renameImpact(rows, 'NS_ONLY'), 0);
	assert.equal(V.renameImpact(rows, 'NOPE'), 0);
});

test('diffVars：改一个键 / 删一个键的差集口径', () => {
	const V = load();
	assert.deepEqual(plain(V.diffVars({ A: '1', B: '2' }, { A: '1', B: '3' })), {
		added: [], removed: [], changed: ['B'],
	});
	assert.deepEqual(plain(V.diffVars({ A: '1', B: '2' }, { A: '1' })), {
		added: [], removed: ['B'], changed: [],
	});
});

/* ------------------------------------------------------------------ *
 * ② 变异自证：把「缺但被要求」的判据改坏，上面的断言必须变红。
 * 不这样做的话，`missing: false` 恒成立也会绿 —— 那正是要拦的假绿。
 * ------------------------------------------------------------------ */
test('变异自证：missing 判据改成恒 false → 「缺但被要求」断言变红', () => {
	const mutant = MODULE_SRC.replace(
		'var missing = usedBy.length > 0 && value === null;',
		'var missing = false;',
	);
	assert.notEqual(mutant, MODULE_SRC, '变异必须真的改到源码（原文变了才说明锚点还在）');
	const V = load(mutant);
	const rows = V.buildRows({ ...NODE_STATUS, required: V.requiredNames(CAPABILITIES), layer: 'node' });
	const java = rows.find((r) => r.name === 'JAVA_HOME');
	assert.equal(java.missing, false, '变异后红标消失');
	assert.throws(() => assert.equal(java.missing, true), /Expected values to be strictly equal/);
});
