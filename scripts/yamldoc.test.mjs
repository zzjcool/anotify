#!/usr/bin/env node
/* Anotify · YAML 文档层（web/yamldoc.js）的行为单测（#473）
 *
 * 拦的事故类型：#454 —— 每个页面各自拼一份文档文本（各一套标量转义、字段顺序、
 * 空集合与省略规则），表单只改了其中一个键，重写出来的文本却把没被改的字段清掉
 * （`tags` 被静默吞掉）或凭空补出别的字段（`limits` 被写成空映射）。这类缺陷静态检查
 * 看不见，只看「端点返回 200」的 e2e 也看不见：只有「读服务端原文 → 表单改一个键 →
 * 重新序列化 → 再读一遍」这一整圈才会失败。
 *
 * 判据（三段）：
 *   ① 行为：在 vm 里真实执行 web/yamldoc.js，喂 `internal/server/resource_node_yaml_test.go`
 *      期望片段那种真实服务端文本 —— 标量表、字段保真、`tags` 与 `limits` 的省略规则、
 *      能力文档的块标量往返、`envPassthrough`（名字白名单）与 `vars`（值映射）的形状差异。
 *   ② 变异自证：同一组保真判据换「改前页面那份拼装实现 + 那份草稿」重跑，findings 必须非空
 *      —— 否则「统一层全绿」也可能只是断言写松了。findings 的原文随测试输出交付。
 *   ③ 接线：`web-src/pages/node.html` 与 `web-src/pages/namespace.html` 不再自带序列化器，
 *      文档文本只从 web/yamldoc.js 来；换回页面自己拼一份，这一段就失败。
 *
 * 跑法：`node --test scripts/yamldoc.test.mjs`（接线：`make test-frontend`）
 */
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import vm from 'node:vm';
import test from 'node:test';
import assert from 'node:assert/strict';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const MODULE_PATH = 'web/yamldoc.js';
const MODULE_SRC = readFileSync(path.join(ROOT, MODULE_PATH), 'utf8');

/** 把 vm 里造出来的对象/数组折回本 realm（跨 realm 的 deepStrictEqual 会因原型不同报错）。 */
const plain = (value) => JSON.parse(JSON.stringify(value));
const has = (obj, key) => Object.prototype.hasOwnProperty.call(obj || {}, key);

/** 在独立 vm context 里执行模块源码，返回 window.AnotifyYamlDoc。 */
function load(src = MODULE_SRC) {
	const ctx = vm.createContext({ console, JSON, Object, Array, String, Number, RegExp });
	vm.runInContext('globalThis.window = globalThis;', ctx);
	vm.runInContext(src, ctx, { filename: MODULE_PATH });
	const api = ctx.window && ctx.window.AnotifyYamlDoc;
	assert.ok(api, 'yamldoc.js 必须挂载 window.AnotifyYamlDoc');
	return api;
}

const YD = load();

/* ------------------------------------------------------------------ *
 * 真实服务端文本：`internal/server/resource_node_yaml_test.go` 里
 * GET /v1/nodes/{name}/yaml 的期望片段（键序即服务端读面的规范形状）。
 * `'*': 2` 的键是单引号 —— 服务端就这么写，parse 必须认。
 * ------------------------------------------------------------------ */
const NODE_DOC = `apiVersion: anotify/v1
kind: Node
metadata:
  name: hw
  resourceVersion: rv1:uid-p86c:7
spec:
  namespaces:
    - default
    - prod
  vars:
    A_KEY: "1"
    B_KEY: "2"
    TOOLCHAIN: /opt/go1.25
  limits:
    total: 4
    perCapability:
      '*': 2
      fixer: 1
  tags:
    - build
    - prod
`;

const TAGS_BLOCK = '  tags:\n    - build\n    - prod\n';
const LIMITS_BLOCK = "  limits:\n    total: 4\n    perCapability:\n      '*': 2\n      fixer: 1\n";

/* 表单编辑意图：只改 vars（加一个键）。所有保真判据都用同一份 patch。 */
const PATCH = { spec: { vars: { ONLY_KEY: 'changed' } } };

test('scalar：唯一一份标量转义（裸写 / 双引号 / 保留字 / 数字开头 / 非字符串）', () => {
	assert.equal(YD.scalar('A_KEY'), 'A_KEY', '字母下划线开头的普通标识符裸写');
	assert.equal(YD.scalar('fixer'), 'fixer');
	assert.equal(YD.scalar('123'), '"123"', '数字开头会被 YAML 读成整数 → 必须加引号');
	assert.equal(YD.scalar('true'), '"true"', '保留字裸写会变成 bool → 必须加引号');
	assert.equal(YD.scalar(''), '""', '空串是两个引号，不是空（空 = null）');
	assert.equal(YD.scalar('a b'), '"a b"', '空格不能裸写');
	assert.equal(YD.scalar('/srv/repos/anotify'), '"/srv/repos/anotify"', '以 / 开头的值用双引号标量（JSON 字符串即合法 YAML）');
	assert.equal(YD.scalar('010'), '"010"', '前导零是 YAML 的八进制 → 必须加引号');
	assert.equal(YD.scalar(null), 'null');
	assert.equal(YD.scalar(4), '4', '数字写成数字，不是带引号的字符串');
	assert.equal(YD.scalar(false), 'false');
});

test('Node：读服务端原文，表单只改一个键，其余字段逐字保真', () => {
	const parsed = YD.parse('Node', NODE_DOC);
	assert.deepEqual(plain(parsed.problems), [], '服务端自己的规范文本必须读得回来');
	const model = parsed.model;
	assert.deepEqual(plain(model.spec.namespaces), ['default', 'prod']);
	assert.deepEqual(plain(model.spec.vars), { A_KEY: '1', B_KEY: '2', TOOLCHAIN: '/opt/go1.25' });
	assert.equal(model.spec.limits.total, 4);
	assert.equal(model.spec.limits.perCapability['*'], 2, '单引号键 `*` 要读成字符串键');
	assert.equal(model.spec.limits.perCapability.fixer, 1);
	assert.deepEqual(plain(model.spec.tags), ['build', 'prod']);
	assert.equal(model.metadata.resourceVersion, 'rv1:uid-p86c:7');

	const edited = YD.edit('Node', NODE_DOC, PATCH);
	assert.deepEqual(plain(edited.problems), [], '编辑不得把原文读坏');
	const again = YD.parse('Node', edited.text);
	assert.deepEqual(plain(again.problems), [], '编辑后的文本必须还能读回来');

	// 只有 spec.vars 变了 —— 其余每一项逐字不变（值，而不是「大概还在」）。
	assert.deepEqual(plain(again.model.spec.namespaces), ['default', 'prod']);
	assert.equal(again.model.metadata.resourceVersion, 'rv1:uid-p86c:7');
	assert.equal(again.model.spec.limits.total, 4, 'total 不能从 4 变成 "4" 或消失');
	assert.deepEqual(plain(again.model.spec.limits.perCapability), { '*': 2, fixer: 1 });
	assert.deepEqual(plain(again.model.spec.tags), ['build', 'prod'], 'tags 的顺序也是事实');
	assert.deepEqual(plain(again.model.spec.vars), {
		A_KEY: '1', B_KEY: '2', TOOLCHAIN: '/opt/go1.25', ONLY_KEY: 'changed',
	}, '原有的 vars 一个不丢，patch 的键写进去');

	// 文本层面：没被改的块仍按服务端那份形状出现（键序 + 缩进 + 引号形态）。
	assert.match(edited.text, /^  limits:\n    total: 4\n    perCapability:\n      "\*": 2\n      fixer: 1\n/m);
	assert.match(edited.text, /^  tags:\n    - build\n    - prod\n/m);
	assert.match(edited.text, /^  namespaces:\n    - default\n    - prod\n/m);
});

test('parse：读不懂的方言返回 problems 而不是半份模型（这种文本不得拿去渲染）', () => {
	const tagged = YD.parse('Node', 'apiVersion: anotify/v1\nkind: !Node\n');
	assert.equal(tagged.model, null, 'problems 非空时没有可信模型');
	assert.match(plain(tagged.problems)[0].message, /自定义 tag/);

	const folded = YD.parse('Node', 'apiVersion: anotify/v1\nspec:\n  vars: >\n    a\n');
	assert.equal(folded.model, null);
	assert.match(plain(folded.problems)[0].message, /折行块标量/);

	// edit 走同一条判据：读不懂就返回空文本，调用方拿不到任何东西可以写回去。
	const refused = YD.edit('Node', 'apiVersion: aiotify/v1\nkind: !Node\n', PATCH);
	assert.equal(refused.text, '');
	assert.equal(refused.model, null);
	assert.ok(refused.problems.length > 0);
});

test('tags 三态：有值保序 / 未知就整键省略 / 已知为空写 tags: []（#454）', () => {
	// ① 有值：编辑 vars 之后仍有两个序列项。
	const withTags = YD.edit('Node', NODE_DOC, PATCH);
	assert.match(withTags.text, /^  tags:\n    - build\n    - prod\n/m);

	// ② 未知（文档里没有 tags 键）：编辑之后也不许凭空出现这个键。
	const unknownDoc = NODE_DOC.replace(TAGS_BLOCK, '');
	assert.notEqual(unknownDoc, NODE_DOC, 'fixture 必须真的删掉了 tags 块');
	const unknown = YD.edit('Node', unknownDoc, PATCH);
	assert.doesNotMatch(unknown.text, /^ *tags:/m, '未知 ≠ 空：不许补 tags 键');
	const unknownBack = YD.parse('Node', unknown.text);
	assert.equal(has(unknownBack.model.spec, 'tags'), false);

	// ③ 已知为空：`tags: []` 是「标签被清空」这个事实，不许被吞成「不知道」。
	const emptyDoc = NODE_DOC.replace(TAGS_BLOCK, '  tags: []\n');
	assert.notEqual(emptyDoc, NODE_DOC);
	const empty = YD.edit('Node', emptyDoc, PATCH);
	assert.match(empty.text, /^  tags: \[\]$/m);
	assert.deepEqual(plain(YD.parse('Node', empty.text).model.spec.tags), []);
});

test('limits 边界：total: null 写回时整键省略 / 没有 limits 就不许凭空造一个', () => {
	// ① 服务端读面为「不压住下层」写显式 total: null；写面要求 total 是 ≥1 的整数。
	const totalNullDoc = NODE_DOC.replace('    total: 4\n', '    total: null\n');
	assert.notEqual(totalNullDoc, NODE_DOC, 'fixture 必须真的改成了 total: null');
	const parsed = YD.parse('Node', totalNullDoc);
	assert.deepEqual(plain(parsed.problems), []);
	assert.deepEqual(plain(parsed.model.spec.limits.perCapability), { '*': 2, fixer: 1 });
	// 交接说明：计划期望 `total === null`；实现的 project 把 null 与 undefined 一样当作
	// 「不参与渲染」跳过（web/yamldoc.js 的「省略 = 不动」），所以模型里是「没有这个键」。
	// 两者对写面完全等价（都写不出 total 行）。这里把「不参与」统一成 null 钉住：
	// 0 / 字符串 / 别的数字一律失败。
	assert.equal(parsed.model.spec.limits.total ?? null, null);

	const totalNullEdit = YD.edit('Node', totalNullDoc, PATCH);
	assert.deepEqual(plain(totalNullEdit.problems), []);
	assert.doesNotMatch(totalNullEdit.text, /total/, '省略 = 不动：绝不写 total: null / total: 0');
	assert.match(totalNullEdit.text, /^    perCapability:\n      "\*": 2\n      fixer: 1$/m, '其它 limits 字段照旧');
	assert.match(totalNullEdit.text, /^  tags:\n    - build\n    - prod\n/m);

	// ② 文档里完全没有 limits：本层不补空映射（原样贴回由服务端 400 的语义不变）。
	const noLimitsDoc = NODE_DOC.replace(LIMITS_BLOCK, '');
	assert.notEqual(noLimitsDoc, NODE_DOC, 'fixture 必须真的删掉了 limits 块');
	const noLimits = YD.edit('Node', noLimitsDoc, PATCH);
	assert.deepEqual(plain(noLimits.problems), []);
	assert.doesNotMatch(noLimits.text, /^ *limits:/m, '没有 limits 就不许造一个空 limits');
	const noLimitsBack = YD.parse('Node', noLimits.text);
	assert.equal(has(noLimitsBack.model.spec, 'limits'), false);
	assert.deepEqual(plain(noLimitsBack.model.spec.vars), {
		A_KEY: '1', B_KEY: '2', TOOLCHAIN: '/opt/go1.25', ONLY_KEY: 'changed',
	});
});

test('未知扩展字段不丢：spec 下的 futureKnob 与顶层的 x-ext 都要原样带回', () => {
	const extDoc = NODE_DOC + '  futureKnob:\n    nested: 1\nx-ext: 2\n';
	const parsed = YD.parse('Node', extDoc);
	assert.deepEqual(plain(parsed.problems), []);
	assert.deepEqual(plain(parsed.model.spec.futureKnob), { nested: 1 });
	assert.equal(parsed.model['x-ext'], 2);

	const edited = YD.edit('Node', extDoc, PATCH);
	assert.deepEqual(plain(edited.problems), []);
	assert.match(edited.text, /^  futureKnob:\n    nested: 1$/m, 'kind 表之外的键按模型顺序追加，不丢');
	assert.match(edited.text, /^x-ext: 2$/m);

	const again = YD.parse('Node', edited.text);
	assert.deepEqual(plain(again.model.spec.futureKnob), { nested: 1 }, '值逐字（不是字符串化后的 1）');
	assert.equal(again.model['x-ext'], 2);
	assert.deepEqual(plain(again.model.spec.tags), ['build', 'prod'], '扩展字段与 tags 互不影响');
});

test('Capability：requires.values / run.workDir / 块标量正文本轮往返且编辑不碰其它字段', () => {
	const script = '#!/bin/bash\nset -e\nmake $1\n';
	const model = {
		apiVersion: 'anotify/v1',
		kind: 'Capability',
		metadata: { name: 'run-in-repo', version: 3 },
		spec: {
			requires: { commands: ['make'], values: ['vars.SOURCE_ROOT'] },
			run: { entrypoint: 'run.sh', workDir: '$(vars.SOURCE_ROOT)', files: { 'run.sh': script } },
		},
	};
	const text = YD.render('Capability', model);
	assert.match(text, /^    workDir: "\$\(vars\.SOURCE_ROOT\)"$/m, 'workDir 的 $(vars.X) 是字符串，不是模板');
	assert.match(text, /^      run\.sh: \|\n        #!\/bin\/bash\n        set -e\n        make \$1\n$/m, '多行正文用块标量保住换行');

	const back = YD.parse('Capability', text);
	assert.deepEqual(plain(back.problems), []);
	assert.deepEqual(plain(back.model), model, 'render → parse 必须回到同一个模型（含多行正文逐字）');
	assert.equal(back.model.spec.run.files['run.sh'], script);

	const edited = YD.edit('Capability', text, { spec: { run: { workDir: '/srv/x' } } });
	assert.deepEqual(plain(edited.problems), []);
	const again = YD.parse('Capability', edited.text);
	assert.equal(again.model.spec.run.workDir, '/srv/x');
	assert.deepEqual(plain(again.model.spec.requires), { commands: ['make'], values: ['vars.SOURCE_ROOT'] });
	assert.equal(again.model.spec.run.entrypoint, 'run.sh');
	assert.equal(again.model.spec.run.files['run.sh'], script, '脚本正文逐字不变');
	assert.equal(again.model.metadata.version, 3);
});

test('AnodeConfig：envPassthrough 是名字白名单（字符串数组），不会混进 vars 映射', () => {
	const model = {
		name: 'node-a',
		server: 'wss://h',
		namespaces: ['default'],
		vars: { SOURCE_ROOT: '/srv/src' },
		limits: { total: 1 },
		envPassthrough: ['SSH_AUTH_SOCK', 'GITHUB_TOKEN'],
	};
	const text = YD.render('AnodeConfig', model);
	const parsed = YD.parse('AnodeConfig', text);
	assert.deepEqual(plain(parsed.problems), []);
	assert.ok(Array.isArray(parsed.model.envPassthrough), 'envPassthrough 是列表，不是映射');
	assert.equal(parsed.model.envPassthrough.length, 2);
	assert.deepEqual(plain(parsed.model.envPassthrough), ['SSH_AUTH_SOCK', 'GITHUB_TOKEN']);
	assert.deepEqual(plain(parsed.model.vars), { SOURCE_ROOT: '/srv/src' }, 'vars 是名字 → 值映射，且只有它自己那一项');

	const edited = YD.edit('AnodeConfig', text, { vars: { ONLY: 'v' } });
	assert.deepEqual(plain(edited.problems), []);
	const again = YD.parse('AnodeConfig', edited.text);
	assert.deepEqual(plain(again.model.envPassthrough), ['SSH_AUTH_SOCK', 'GITHUB_TOKEN'], '两项都不变、顺序不变');
	assert.deepEqual(plain(again.model.vars), { SOURCE_ROOT: '/srv/src', ONLY: 'v' });
	assert.equal(has(again.model.vars, 'SSH_AUTH_SOCK'), false, '白名单名字不会变成 vars 的键');
	assert.equal(again.model.limits.total, 1);

	// Node 表里没有 envPassthrough：它是扩展字段，必须落在 spec 下、vars 映射**之外**。
	const nodeText = YD.render('Node', {
		apiVersion: 'anotify/v1', kind: 'Node', metadata: { name: 'n' },
		spec: { vars: { A: '1' }, envPassthrough: ['X'] },
	});
	assert.match(nodeText, /^  vars:\n    A: "1"\n  envPassthrough:\n    - X$/m, 'envPassthrough 与 vars 同级');
	assert.deepEqual(plain(YD.parse('Node', nodeText).model.spec.vars), { A: '1' }, 'vars 里只有 A');
});

/* ================================================================== *
 * ② 变异自证
 *
 * 下面这一段是**改前**的实现：页面自己拼文档文本（`web-src/pages/node.html`
 * 改前的 yamlScalar / yamlList / yamlMap / docToYAML，行为等价）。它不读服务端原文：
 * 页面拿 `GET …/config` 那份视图攒一份草稿，再自己拼文本 —— #454 的 tags 丢失就是
 * 「草稿里没有这个键」这条路的直接后果。留着它，「统一层全绿」才有对照物。
 * ================================================================== */

const yamlScalar = (v) => {
	if (v === null || v === undefined) return 'null';
	if (typeof v === 'number' || typeof v === 'boolean') return String(v);
	const s = String(v);
	if (/^[A-Za-z0-9][\w./:@+-]*$/.test(s) && !/^(?:true|false|null|yes|no|on|off|y|n|~)$/i.test(s) && !/^[-+]?\d+(?:\.\d+)?(?:[eE][-+]?\d+)?$/.test(s)) return s;
	return JSON.stringify(s);
};
const sortedEntries = (obj) => Object.entries(obj && typeof obj === 'object' && !Array.isArray(obj) ? obj : {})
	.sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
const yamlList = (key, items, indent) => {
	const pad = ' '.repeat(indent);
	if (!items.length) return [pad + key + ': []'];
	return [pad + key + ':'].concat(items.map((item) => pad + '  - ' + yamlScalar(item)));
};
const yamlMap = (key, obj, indent) => {
	const pad = ' '.repeat(indent), entries = sortedEntries(obj);
	if (!entries.length) return [pad + key + ': {}'];
	return [pad + key + ':'].concat(entries.map(([k, v]) => pad + '  ' + yamlScalar(k) + ': ' + yamlScalar(v)));
};
function docToYAML(doc) {
	const spec = doc.spec || {}, limits = spec.limits || {};
	const lines = ['apiVersion: ' + yamlScalar(doc.apiVersion), 'kind: ' + yamlScalar(doc.kind), 'metadata:', '  name: ' + yamlScalar(doc.metadata.name)];
	if (doc.metadata.resourceVersion) lines.push('  resourceVersion: ' + yamlScalar(doc.metadata.resourceVersion));
	lines.push('spec:');
	lines.push(...yamlList('namespaces', Array.isArray(spec.namespaces) ? spec.namespaces : [], 2));
	lines.push(...yamlMap('vars', spec.vars, 2));
	const perEntries = sortedEntries(limits.perCapability);
	lines.push('  limits:');
	if (limits.total != null) lines.push('    total: ' + yamlScalar(limits.total));
	lines.push('    perCapability:' + (perEntries.length ? '' : ' {}'));
	for (const [k, v] of perEntries) lines.push('      ' + yamlScalar(k) + ': ' + yamlScalar(v));
	if (Array.isArray(spec.tags)) lines.push(...yamlList('tags', spec.tags, 2));
	return lines.join('\n') + '\n';
}

/** 改前页面手上那份草稿：只有它认识的几个键（`tags: null` = 读面没告诉它现值）。 */
const LEGACY_DRAFT = {
	apiVersion: 'anotify/v1',
	kind: 'Node',
	metadata: { name: 'hw', resourceVersion: 'rv1:uid-p86c:7' },
	spec: {
		namespaces: ['default', 'prod'],
		vars: { A_KEY: '1', B_KEY: '2', TOOLCHAIN: '/opt/go1.25' },
		limits: { total: 4, perCapability: { '*': 2, fixer: 1 } },
		tags: null,
	},
};

/** 改前命名空间页那种拼法：手上只有 vars，别的键它连字段都没有。 */
const LEGACY_ONLY_VARS_DRAFT = {
	apiVersion: 'anotify/v1',
	kind: 'Node',
	metadata: { name: 'hw' },
	spec: { vars: { A_KEY: '1', B_KEY: '2', TOOLCHAIN: '/opt/go1.25' } },
};

/** 旧实现的「表单改写」：patch 落在草稿上再拼文本；服务端原文根本不看。 */
function legacyRewrite(draft) {
	return (text, patch) => {
		const doc = JSON.parse(JSON.stringify(draft));
		doc.spec = Object.assign({}, doc.spec, {
			vars: Object.assign({}, doc.spec.vars, patch.spec.vars),
		});
		return docToYAML(doc);
	};
}

/** 统一层：读服务端原文 → 只改 patch 里的键 → 重新序列化。 */
const unifiedRewrite = (text, patch) => YD.edit('Node', text, patch).text;

/* 保真判据用的输入：同一份真实文本，逐个去掉/替换一个块，覆盖 tags、limits、
 * 扩展字段这三类「页面容易顺手改掉」的东西。 */
const FIDELITY_CASES = [
	{ name: 'full', text: NODE_DOC },
	{ name: 'total-null', text: NODE_DOC.replace('    total: 4\n', '    total: null\n') },
	{ name: 'unknown-tags', text: NODE_DOC.replace(TAGS_BLOCK, '') },
	{ name: 'empty-tags', text: NODE_DOC.replace(TAGS_BLOCK, '  tags: []\n') },
	{ name: 'no-limits', text: NODE_DOC.replace(LIMITS_BLOCK, '') },
	{ name: 'ext', text: NODE_DOC + '  futureKnob:\n    nested: 1\nx-ext: 2\n' },
];

/** 路径 → 判据维度（findings 用同一套名字，报告能对着看）。 */
function dimensionOf(pathStr) {
	if (pathStr.startsWith('spec.limits.perCapability')) return 'perCapability';
	if (pathStr === 'spec.limits' || pathStr.startsWith('spec.limits.')) return 'limits';
	if (pathStr === 'spec.tags' || pathStr.startsWith('spec.tags.')) return 'tags';
	if (pathStr.startsWith('spec.namespaces')) return 'namespaces';
	if (pathStr.startsWith('metadata')) return 'metadata';
	if (pathStr.startsWith('spec.vars')) return 'vars';
	if (pathStr === 'apiVersion' || pathStr === 'kind') return 'doc';
	return 'extra';
}

function diffInto(before, after, at, out) {
	if (at === 'spec.vars') return; // vars 单独对账（见 fidelity 里那段）
	const beforeIsMap = before !== null && typeof before === 'object' && !Array.isArray(before);
	const afterIsMap = after !== null && typeof after === 'object' && !Array.isArray(after);
	if (beforeIsMap && afterIsMap) {
		for (const key of new Set([...Object.keys(before), ...Object.keys(after)])) {
			diffInto(before[key], after[key], at ? at + '.' + key : key, out);
		}
		return;
	}
	if (JSON.stringify(before) !== JSON.stringify(after)) out.push({ path: at, before, after });
}

const show = (value) => (value === undefined ? '不存在' : JSON.stringify(value));

/**
 * 保真判据：给一份服务端原文 + 一次表单编辑，用某个实现重写，再逐条对账。
 * findings 为空 = 这次编辑只动了 patch 指定的那个键（spec.vars 之外一个路径都没变）。
 */
function fidelity(caseName, source, rewrite) {
	const findings = [];
	const before = YD.parse('Node', source);
	if (before.problems.length) return [`${caseName}/text: 原文读不回来 ${JSON.stringify(plain(before.problems))}`];

	const rewritten = rewrite(source, PATCH);
	const after = YD.parse('Node', rewritten);
	if (after.problems.length) return [`${caseName}/text: 重写后的文本读不回来 ${JSON.stringify(plain(after.problems))}`];

	const changed = [];
	diffInto(plain(before.model), plain(after.model), '', changed);
	for (const item of changed) {
		findings.push(`${caseName}/${dimensionOf(item.path)}: ${item.path} ${show(item.before)} → ${show(item.after)}`);
	}

	const beforeVars = (before.model.spec && before.model.spec.vars) || {};
	const afterVars = (after.model.spec && after.model.spec.vars) || {};
	for (const key of Object.keys(beforeVars)) {
		if (afterVars[key] !== beforeVars[key]) {
			findings.push(`${caseName}/vars: vars.${key} ${show(beforeVars[key])} → ${show(afterVars[key])}`);
		}
	}
	if (afterVars.ONLY_KEY !== 'changed') findings.push(`${caseName}/vars: patch 里的键没写进文档`);
	for (const key of Object.keys(afterVars)) {
		if (!has(beforeVars, key) && key !== 'ONLY_KEY') findings.push(`${caseName}/vars: 多出没写过的键 ${key}`);
	}

	return findings;
}

function fidelityAll(rewrite) {
	return FIDELITY_CASES.flatMap((one) => fidelity(one.name, one.text, rewrite));
}

test('变异自证：旧实现 + 旧草稿让 tags / 扩展字段 / limits 三个维度都非空（findings ≥ 2）', (t) => {
	const legacy = fidelityAll(legacyRewrite(LEGACY_DRAFT));
	t.diagnostic('旧实现 + 旧草稿 findings:\n  ' + legacy.join('\n  '));
	assert.ok(legacy.length >= 2, `旧实现必须让保真判据非空，实际 ${legacy.length} 条`);
	const dims = new Set(legacy.map((one) => one.split(':')[0].split('/')[1]));
	for (const dim of ['tags', 'extra', 'limits']) {
		assert.ok(dims.has(dim), `旧实现在 ${dim} 维度上必须有一条 finding（实际 ${JSON.stringify([...dims])}）`);
	}
	// 同一组输入上，统一层必须是空的 —— 否则上面那条非空什么也说明不了。
	assert.deepEqual(fidelityAll(unifiedRewrite), []);
});

test('变异自证：只有 spec.vars 的草稿（改前命名空间页那种拼法）也必须让 tags 判据非空', (t) => {
	const onlyVars = fidelityAll(legacyRewrite(LEGACY_ONLY_VARS_DRAFT));
	t.diagnostic('只有 vars 的草稿 findings:\n  ' + onlyVars.join('\n  '));
	assert.ok(onlyVars.length >= 1);
	const tagsFindings = onlyVars.filter((one) => one.includes('/tags: '));
	assert.ok(tagsFindings.length >= 1, 'tags 维度必须有一条 finding');
	assert.match(tagsFindings[0], /spec\.tags/, '点名的是 tags 那一条路径，不是别的东西');
});

/* ================================================================== *
 * ③ 页面接线：文本只有一个来源
 * ================================================================== */

const PAGES = ['web-src/pages/node.html', 'web-src/pages/namespace.html'];
const LOCAL_SERIALIZER_RE = /(?:const|let|var|function)\s+(?:yamlScalar|yamlList|yamlMap|docToYAML|namespaceYAMLText|varsToYAMLLines)\b/;

test('页面接线：文档文本只从 AnotifyYamlDoc 来，页面里没有本地序列化器也没有拼装字面量', () => {
	for (const rel of PAGES) {
		const src = readFileSync(path.join(ROOT, rel), 'utf8');
		const at = `${rel} 必须走统一层`;
		assert.match(src, /YD\.edit\(/, at + '（表单改一处用 YD.edit）');
		assert.match(src, /YD\.render\(/, at + '（预览/回填用 YD.render）');
		assert.match(src, /window\.AnotifyYamlDoc/, at + '（从 window 取，不自己造）');
		assert.doesNotMatch(src, LOCAL_SERIALIZER_RE, rel + ' 里不许再声明第二份序列化器');
		assert.ok(!/['"]apiVersion: /.test(src), rel + ' 里不许再出现文档文本字面量（\'apiVersion: \' / "apiVersion: "）');
	}

	const varsTable = readFileSync(path.join(ROOT, 'web/vars-table.js'), 'utf8');
	assert.doesNotMatch(varsTable, /\bfunction\s+yamlScalar\b/, 'web/vars-table.js 不再自带 yamlScalar 实现');
	assert.doesNotMatch(varsTable, /\bfunction\s+varsToYAMLLines\b/, 'web/vars-table.js 不再自带 varsToYAMLLines 实现');
});
