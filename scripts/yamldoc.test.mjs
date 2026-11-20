#!/usr/bin/env node
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import vm from 'node:vm';
import test from 'node:test';
import assert from 'node:assert/strict';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const MODULE_PATH = 'web/yamldoc.js';
const MODULE_SRC = readFileSync(path.join(ROOT, MODULE_PATH), 'utf8');

const plain = (value) => JSON.parse(JSON.stringify(value));
const has = (obj, key) => Object.prototype.hasOwnProperty.call(obj || {}, key);

function load(src = MODULE_SRC) {
	const ctx = vm.createContext({ console, JSON, Object, Array, String, Number, RegExp });
	vm.runInContext('globalThis.window = globalThis;', ctx);
	vm.runInContext(src, ctx, { filename: MODULE_PATH });
	const api = ctx.window && ctx.window.AnotifyYamlDoc;
	assert.ok(api, 'yamldoc.js 必须挂载 window.AnotifyYamlDoc');
	return api;
}

const YD = load();

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

	assert.deepEqual(plain(again.model.spec.namespaces), ['default', 'prod']);
	assert.equal(again.model.metadata.resourceVersion, 'rv1:uid-p86c:7');
	assert.equal(again.model.spec.limits.total, 4, 'total 不能从 4 变成 "4" 或消失');
	assert.deepEqual(plain(again.model.spec.limits.perCapability), { '*': 2, fixer: 1 });
	assert.deepEqual(plain(again.model.spec.tags), ['build', 'prod'], 'tags 的顺序也是事实');
	assert.deepEqual(plain(again.model.spec.vars), { ONLY_KEY: 'changed' },
		'patch 里的 spec.vars 是这一层的**整份**映射（表格保存就是整份）：替换而不是合并');

	const deleted = YD.edit('Node', NODE_DOC, { spec: { vars: { B_KEY: '2', TOOLCHAIN: '/opt/go1.25' } } });
	const deletedBack = YD.parse('Node', deleted.text);
	assert.deepEqual(plain(deletedBack.model.spec.vars), { B_KEY: '2', TOOLCHAIN: '/opt/go1.25' },
		'patch 里没有 A_KEY → 文档里也不能留着它');
	assert.doesNotMatch(deleted.text, /A_KEY/, '被删掉的键不能靠合并语义活下来');

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

	const refused = YD.edit('Node', 'apiVersion: aiotify/v1\nkind: !Node\n', PATCH);
	assert.equal(refused.text, '');
	assert.equal(refused.model, null);
	assert.ok(refused.problems.length > 0);
});

test('tags 三态：有值保序 / 未知就整键省略 / 已知为空写 tags: []（#454）', () => {
	const withTags = YD.edit('Node', NODE_DOC, PATCH);
	assert.match(withTags.text, /^  tags:\n    - build\n    - prod\n/m);

	const unknownDoc = NODE_DOC.replace(TAGS_BLOCK, '');
	assert.notEqual(unknownDoc, NODE_DOC, 'fixture 必须真的删掉了 tags 块');
	const unknown = YD.edit('Node', unknownDoc, PATCH);
	assert.doesNotMatch(unknown.text, /^ *tags:/m, '未知 ≠ 空：不许补 tags 键');
	const unknownBack = YD.parse('Node', unknown.text);
	assert.equal(has(unknownBack.model.spec, 'tags'), false);

	const emptyDoc = NODE_DOC.replace(TAGS_BLOCK, '  tags: []\n');
	assert.notEqual(emptyDoc, NODE_DOC);
	const empty = YD.edit('Node', emptyDoc, PATCH);
	assert.match(empty.text, /^  tags: \[\]$/m);
	assert.deepEqual(plain(YD.parse('Node', empty.text).model.spec.tags), []);
});

test('limits 边界：total: null 写回时整键省略 / 没有 limits 就不许凭空造一个', () => {
	const totalNullDoc = NODE_DOC.replace('    total: 4\n', '    total: null\n');
	assert.notEqual(totalNullDoc, NODE_DOC, 'fixture 必须真的改成了 total: null');
	const parsed = YD.parse('Node', totalNullDoc);
	assert.deepEqual(plain(parsed.problems), []);
	assert.deepEqual(plain(parsed.model.spec.limits.perCapability), { '*': 2, fixer: 1 });
	assert.equal(parsed.model.spec.limits.total ?? null, null);

	const totalNullEdit = YD.edit('Node', totalNullDoc, PATCH);
	assert.deepEqual(plain(totalNullEdit.problems), []);
	assert.doesNotMatch(totalNullEdit.text, /total/, '省略 = 不动：绝不写 total: null / total: 0');
	assert.match(totalNullEdit.text, /^    perCapability:\n      "\*": 2\n      fixer: 1$/m, '其它 limits 字段照旧');
	assert.match(totalNullEdit.text, /^  tags:\n    - build\n    - prod\n/m);

	const noLimitsDoc = NODE_DOC.replace(LIMITS_BLOCK, '');
	assert.notEqual(noLimitsDoc, NODE_DOC, 'fixture 必须真的删掉了 limits 块');
	const noLimits = YD.edit('Node', noLimitsDoc, PATCH);
	assert.deepEqual(plain(noLimits.problems), []);
	assert.doesNotMatch(noLimits.text, /^ *limits:/m, '没有 limits 就不许造一个空 limits');
	const noLimitsBack = YD.parse('Node', noLimits.text);
	assert.equal(has(noLimitsBack.model.spec, 'limits'), false);
	assert.deepEqual(plain(noLimitsBack.model.spec.vars), { ONLY_KEY: 'changed' },
		'没有 limits 的文档里，vars 也是整份替换');
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

test('块标量：多行值的 chomping 逐字往返（行尾换行数不许被改），且认得 yaml.v3 的三种写法', () => {
	const values = {
		noTrail: 'line1\nline2',
		oneTrail: 'line1\nline2\n',
		twoTrail: 'a\n\n',
		moreTrail: 'a\n\n\n',
		blankMiddle: 'x\n\ny\n',
		innerIndent: 'a\n  b\n',
		leadingSpace: '  indented\nfirst',
		tabLine: 'a\n\tb\n',
		crlf: 'a\r\nb\r\n',
		onlyNewline: '\n',
		quoteInside: 'he said "hi"\nand left\n',
	};
	const text = YD.render('Capability', { apiVersion: 'anotify/v1', kind: 'Capability', metadata: { name: 'probe' }, spec: { files: values } });
	const back = YD.parse('Capability', text);
	assert.deepEqual(plain(back.problems), [], '渲染出来的文档必须自己能读回来');
	for (const key of Object.keys(values)) {
		assert.equal(back.model.spec.files[key], values[key], `spec.files.${key} 必须逐字往返（chomping 不许加/丢行尾换行）`);
	}
	assert.match(text, /\n    noTrail: \|-/, '不以换行结尾 → `|-`（strip）');
	assert.match(text, /\n    oneTrail: \|\n/, '恰好一个换行 → `|`（clip）');
	assert.match(text, /\n    twoTrail: \|\+/, '两个以上 → `|+`（keep）');

	const v3 = [
		'apiVersion: anotify/v1',
		'kind: Capability',
		'spec:',
		'    files:',
		'        strip: |-',
		'            a',
		'            b',
		'        clip: |',
		'            a',
		'            b',
		'        keep: |+',
		'            a',
		'',
		'        indented: |4-',
		'              x',
		'            y',
		'',
	].join('\n');
	const parsed = YD.parse('Capability', v3);
	assert.deepEqual(plain(parsed.problems), [], 'yaml.v3 的形状必须认得');
	assert.equal(parsed.model.spec.files.strip, 'a\nb', '|- 是 strip：没有行尾换行');
	assert.equal(parsed.model.spec.files.clip, 'a\nb\n', '| 是 clip：恰好一个行尾换行');
	assert.equal(parsed.model.spec.files.keep, 'a\n\n', '|+ 是 keep：末尾空行全算正文');
	assert.equal(parsed.model.spec.files.indented, '  x\ny', '缩进指示器指定正文缩进（首行的额外空格是内容）');
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
	assert.deepEqual(plain(again.model.vars), { ONLY: 'v' }, 'vars 是整份映射（替换）；白名单不进它');
	assert.equal(has(again.model.vars, 'SSH_AUTH_SOCK'), false, '白名单名字不会变成 vars 的键');
	assert.equal(again.model.limits.total, 1);

	const nodeText = YD.render('Node', {
		apiVersion: 'anotify/v1', kind: 'Node', metadata: { name: 'n' },
		spec: { vars: { A: '1' }, envPassthrough: ['X'] },
	});
	assert.match(nodeText, /^  vars:\n    A: "1"\n  envPassthrough:\n    - X$/m, 'envPassthrough 与 vars 同级');
	assert.deepEqual(plain(YD.parse('Node', nodeText).model.spec.vars), { A: '1' }, 'vars 里只有 A');
});


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

const LEGACY_ONLY_VARS_DRAFT = {
	apiVersion: 'anotify/v1',
	kind: 'Node',
	metadata: { name: 'hw' },
	spec: { vars: { A_KEY: '1', B_KEY: '2', TOOLCHAIN: '/opt/go1.25' } },
};

function legacyRewrite(draft) {
	return (text, patch) => {
		const doc = JSON.parse(JSON.stringify(draft));
		doc.spec = Object.assign({}, doc.spec, {
			vars: Object.assign({}, doc.spec.vars, patch.spec.vars),
		});
		return docToYAML(doc);
	};
}

const unifiedRewrite = (text, patch) => YD.edit('Node', text, patch).text;

const FIDELITY_CASES = [
	{ name: 'full', text: NODE_DOC },
	{ name: 'total-null', text: NODE_DOC.replace('    total: 4\n', '    total: null\n') },
	{ name: 'unknown-tags', text: NODE_DOC.replace(TAGS_BLOCK, '') },
	{ name: 'empty-tags', text: NODE_DOC.replace(TAGS_BLOCK, '  tags: []\n') },
	{ name: 'no-limits', text: NODE_DOC.replace(LIMITS_BLOCK, '') },
	{ name: 'ext', text: NODE_DOC + '  futureKnob:\n    nested: 1\nx-ext: 2\n' },
];

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
	if (afterVars.ONLY_KEY !== 'changed') findings.push(`${caseName}/vars: patch 里的键没写进文档`);
	for (const key of Object.keys(beforeVars)) {
		if (has(afterVars, key)) findings.push(`${caseName}/vars: 已经不在 patch 里的键 ${key} 还留在文档里（合并语义把删除吃掉了）`);
	}
	for (const key of Object.keys(afterVars)) {
		if (key !== 'ONLY_KEY') findings.push(`${caseName}/vars: 多出 patch 里没有的键 ${key}`);
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


const PAGES = ['web-src/pages/node.html', 'web-src/pages/namespace.html'];
const LOCAL_SERIALIZER_RE = /(?:const|let|var|function)\s+(?:yamlScalar|yamlList|yamlMap|docToYAML|namespaceYAMLText|varsToYAMLLines)\b/;

test('页面接线：文档文本只从 AnotifyYamlDoc 来，页面里没有本地序列化器也没有拼装字面量', () => {
	for (const rel of PAGES) {
		const src = readFileSync(path.join(ROOT, rel), 'utf8');
		const at = `${rel} 必须走统一层`;
		assert.match(src, /window\.AnotifyYamlDoc/, at + '（从 window 取，不自己造）');
		assert.match(src, /YD\.edit\(/, at + '（表单改一处 = 读原文 → 改一个键 → 重新序列化）');
		assert.match(src, /YD\.parse\(/, at + '（编辑基线来自服务端原文的解析结果）');
		assert.doesNotMatch(src, LOCAL_SERIALIZER_RE, rel + ' 里不许再声明第二份序列化器');
		assert.ok(!/['"]apiVersion: /.test(src), rel + ' 里不许再出现文档文本字面量（\'apiVersion: \' / "apiVersion: "）');
	}

	assert.match(readFileSync(path.join(ROOT, 'web-src/pages/node.html'), 'utf8'), /YD\.render\(/,
		'节点页的重写按钮必须用同一份 render');

	const varsTable = readFileSync(path.join(ROOT, 'web/vars-table.js'), 'utf8');
	assert.doesNotMatch(varsTable, /\bfunction\s+yamlScalar\b/, 'web/vars-table.js 不再自带 yamlScalar 实现');
	assert.doesNotMatch(varsTable, /\bfunction\s+varsToYAMLLines\b/, 'web/vars-table.js 不再自带 varsToYAMLLines 实现');
});
