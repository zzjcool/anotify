/*
 * 资源文档层：页面 / preview mock / vars 表格共用的唯一一份 YAML 序列化与解析。
 *
 * 与 yaml.v3 对拍（跨语言的接口事实，不是本文件的实现细节）：
 *  · chomping 按末尾换行数选：不以换行结尾 `|-`、恰好一个 `|`、两个以上 `|+`；
 *    写死 `|` 会把「没有行尾换行」的值补上一个行尾换行。
 *  · 双引号标量用 JSON 转义（YAML 双引号标量是 JSON 的超集）；块标量只在首行不带
 *    空白、正文没有 `\r` 时使用 —— 否则退回双引号标量，值逐字不变。
 *  · 以数字开头的值一律加引号：`010` 是八进制、`123` 是整数，服务端按类型解码会 400。
 *  · `total` 为 null 时整键省略（写面要求 ≥1，省略 = 不动）；`tags` 的 null（未知）
 *    与 `[]`（清空）是两件事。
 *  · 数组/映射是值节点，整份替换（`spec.vars` 删键就是清空）；读不懂的方言返回
 *    problems，调用方不得渲染 —— 不把「没读懂」写成「用户清空了」。
 */
(function (root) {
	'use strict';


	var PLAIN = /^[A-Za-z_][A-Za-z0-9_./:@+-]*$/;
	var RESERVED = /^(?:true|false|null|yes|no|on|off|y|n|~)$/i;

	function scalar(value) {
		if (value === null || value === undefined) return 'null';
		if (typeof value === 'boolean') return value ? 'true' : 'false';
		if (typeof value === 'number') return isFinite(value) ? String(value) : JSON.stringify(String(value));
		var text = String(value);
		if (text === '') return '""';
		if (PLAIN.test(text) && !RESERVED.test(text)) return text;
		return JSON.stringify(text);
	}


	function isMap(value) {
		return value !== null && typeof value === 'object' && !Array.isArray(value);
	}

	function useBlockScalar(text) {
		if (typeof text !== 'string' || text.indexOf('\n') < 0) return false;
		if (text.indexOf('\r') >= 0) return false;
		var core = text.replace(/\n+$/, '');
		if (core === '') return false;
		return !/^[ \t]/.test(core.split('\n')[0]);
	}

	function blockScalar(text, indent) {
		var core = String(text).replace(/\n+$/, '');
		var trail = String(text).length - core.length;
		var out = ['|' + (trail === 0 ? '-' : trail === 1 ? '' : '+')];
		var rows = core.split('\n');
		for (var i = 0; i < rows.length; i++) out.push(rows[i] === '' ? '' : ' '.repeat(indent) + rows[i]);
		for (var b = 0; b < trail - 1; b++) out.push('');
		return out;
	}

	function emitArray(value, indent, out) {
		var pad = ' '.repeat(indent);
		if (!value.length) {
			out.push(pad + '[]');
			return;
		}
		for (var i = 0; i < value.length; i++) {
			var item = value[i];
			if (isMap(item) || Array.isArray(item)) {
				var inner = [];
				emit(item, indent + 2, inner);
				out.push(pad + '- ' + inner[0].slice(indent + 2));
				for (var j = 1; j < inner.length; j++) out.push(inner[j]);
				continue;
			}
			if (useBlockScalar(item)) {
				var lines = blockScalar(item, indent + 2);
				out.push(pad + '- ' + lines[0]);
				for (var k = 1; k < lines.length; k++) out.push(lines[k]);
				continue;
			}
			out.push(pad + '- ' + scalar(item));
		}
	}

	function emit(value, indent, out) {
		var pad = ' '.repeat(indent);
		if (Array.isArray(value)) {
			emitArray(value, indent, out);
			return;
		}
		if (isMap(value)) {
			var keys = Object.keys(value);
			if (!keys.length) {
				out.push(pad + '{}');
				return;
			}
			for (var i = 0; i < keys.length; i++) {
				var key = scalar(keys[i]);
				var child = value[keys[i]];
				if (isMap(child)) {
					if (!Object.keys(child).length) {
						out.push(pad + key + ': {}');
						continue;
					}
					out.push(pad + key + ':');
					var mapLines = [];
					emit(child, indent + 2, mapLines);
					for (var m = 0; m < mapLines.length; m++) out.push(mapLines[m]);
					continue;
				}
				if (Array.isArray(child)) {
					if (!child.length) {
						out.push(pad + key + ': []');
						continue;
					}
					out.push(pad + key + ':');
					var seqLines = [];
					emitArray(child, indent + 2, seqLines);
					for (var s = 0; s < seqLines.length; s++) out.push(seqLines[s]);
					continue;
				}
				if (useBlockScalar(child)) {
					var block = blockScalar(child, indent + 2);
					out.push(pad + key + ': ' + block[0]);
					for (var b = 1; b < block.length; b++) out.push(block[b]);
					continue;
				}
				out.push(pad + key + ': ' + scalar(child));
			}
			return;
		}
		out.push(pad + scalar(value));
	}



	var NODE_FIELDS = [
		{ key: 'apiVersion', type: 'string' },
		{ key: 'kind', type: 'string' },
		{
			key: 'metadata',
			type: 'object',
			fields: [
				{ key: 'name', type: 'string' },
				{ key: 'resourceVersion', type: 'string', omit: 'missing' },
			],
		},
		{
			key: 'spec',
			type: 'object',
			fields: [
				{ key: 'namespaces', type: 'strings' },
				{ key: 'vars', type: 'string-map' },
				{
					key: 'limits',
					type: 'object',
					fields: [
						{ key: 'total', type: 'int', omit: 'missing' },
						{ key: 'perCapability', type: 'int-map' },
					],
				},
				{ key: 'tags', type: 'strings' },
			],
		},
	];

	var KINDS = {
		Node: {
			apiVersion: 'anotify/v1',
			kind: 'Node',
			path: function (name) {
				return '/v1/nodes/' + encodeURIComponent(name) + '/yaml';
			},
			fields: NODE_FIELDS,
		},
		Namespace: {
			apiVersion: 'anotify/v1',
			kind: 'Namespace',
			path: function (name) {
				return '/v1/namespaces/' + encodeURIComponent(name) + '/yaml';
			},
			fields: [
				{ key: 'apiVersion', type: 'string' },
				{ key: 'kind', type: 'string' },
				{
					key: 'metadata',
					type: 'object',
					fields: [
						{ key: 'name', type: 'string' },
						{ key: 'resourceVersion', type: 'string', omit: 'missing' },
					],
				},
				{
					key: 'spec',
					type: 'object',
					fields: [
						{ key: 'vars', type: 'string-map' },
					],
				},
			],
		},
		Capability: {apiVersion: 'anotify/v1', kind: 'Capability', generic: true},
		Source: {apiVersion: 'anotify/v1', kind: 'Source', generic: true},
		AnodeConfig: {
			kind: 'anode.yaml',
			fields: [
				{ key: 'name', type: 'string' },
				{ key: 'server', type: 'string' },
				{ key: 'token', type: 'string', omit: 'missing' },
				{ key: 'namespaces', type: 'strings' },
				{ key: 'vars', type: 'string-map' },
				{
					key: 'limits',
					type: 'object',
					fields: [
						{ key: 'total', type: 'int', omit: 'missing' },
						{ key: 'perCapability', type: 'int-map' },
					],
				},
				{ key: 'envPassthrough', type: 'strings' },
				{ key: 'listen', type: 'string', omit: 'missing' },
			],
		},
	};


	function kindOf(kindId) {
		var kind = KINDS[kindId];
		if (!kind) throw new Error('yamldoc: 未注册的 kind ' + kindId);
		return kind;
	}

	function project(fields, source, out) {
		var src = isMap(source) ? source : {};
		for (var i = 0; i < fields.length; i++) {
			var f = fields[i];
			var value = src[f.key];
			if (f.type === 'object') {
				var child = {};
				if (isMap(value)) project(f.fields, value, child);
				if (Object.keys(child).length) out[f.key] = child;
				continue;
			}
			if (value === null || value === undefined) {
				if (f.omit === 'never') out[f.key] = coerce(f.type, undefined);
				continue;
			}
			out[f.key] = coerce(f.type, value);
		}
		var declared = {};
		for (var d = 0; d < fields.length; d++) declared[fields[d].key] = true;
		var keys = Object.keys(src);
		for (var k = 0; k < keys.length; k++) {
			if (declared[keys[k]]) continue;
			out[keys[k]] = src[keys[k]];
		}
	}

	function coerce(type, value) {
		if (value === null) return value;
		if (value === undefined) {
			if (type === 'strings') return [];
			if (type === 'string-map' || type === 'int-map') return {};
			return value;
		}
		switch (type) {
		case 'int': {
			var n = typeof value === 'number' ? value : Number(String(value).trim());
			return isFinite(n) ? n : value;
		}
		case 'strings': {
			var list = Array.isArray(value) ? value : [value];
			var strs = [];
			for (var i = 0; i < list.length; i++) strs.push(String(list[i]));
			return strs;
		}
		case 'string-map': {
			var map = isMap(value) ? value : {};
			var out = {};
			var names = Object.keys(map);
			for (var m = 0; m < names.length; m++) out[names[m]] = map[names[m]] === null || map[names[m]] === undefined ? '' : String(map[names[m]]);
			return out;
		}
		case 'int-map': {
			var obj = isMap(value) ? value : {};
			var ints = {};
			var ks = Object.keys(obj);
			for (var j = 0; j < ks.length; j++) ints[ks[j]] = coerce('int', obj[ks[j]]);
			return ints;
		}
		default:
			return typeof value === 'string' ? value : String(value);
		}
	}

	function render(kindId, model) {
		var kind = kindOf(kindId);
		var src = isMap(model) ? model : {};
		var doc = {};
		if (kind.generic) {
			var keys = Object.keys(src);
			for (var i = 0; i < keys.length; i++) doc[keys[i]] = src[keys[i]];
		} else {
			project(kind.fields, src, doc);
		}
		var lines = [];
		emit(doc, 0, lines);
		return lines.length ? lines.join('\n') + '\n' : '';
	}


	function parse(text) {
		var problems = [];
		var source = String(text === null || text === undefined ? '' : text).replace(/\r\n?/g, '\n');
		var endsWithNewline = source.charAt(source.length - 1) === '\n';
		var raw = source.split('\n');
		var pos = 0;

		function problem(index, message) {
			problems.push({line: index + 1, message: message});
			return null;
		}

		function spacesOf(line) {
			var i = 0;
			while (i < line.length && line.charAt(i) === ' ') i++;
			return i;
		}

		function indentOf(line) {
			var i = 0;
			while (i < line.length && line.charAt(i) === ' ') i++;
			return line.charAt(i) === '\t' ? -1 : i;
		}

		function isBlank(line) {
			var t = line.trim();
			return t === '' || t.charAt(0) === '#';
		}

		function nextContent(from) {
			var i = from;
			while (i < raw.length && isBlank(raw[i])) i++;
			return i;
		}

		function stripComment(line) {
			var quote = '';
			for (var i = 0; i < line.length; i++) {
				var c = line.charAt(i);
				if (quote) {
					if (c === '\\' && quote === '"') i++;
					else if (c === quote) quote = '';
					continue;
				}
				if (c === '"' || c === "'") { quote = c; continue; }
				if (c === '#' && (i === 0 || /\s/.test(line.charAt(i - 1)))) return line.slice(0, i);
			}
			return line;
		}

		function parseDouble(textIn) {
			try {
				return JSON.parse(textIn);
			} catch (e) {
				return problem(pos, '双引号标量没有闭合');
			}
		}

		function parseScalar(textIn, index) {
			var text = textIn.trim();
			if (!text) return {value: null, rest: ''};
			var first = text.charAt(0);
			if (first === '"') {
				var end = -1;
				for (var i = 1; i < text.length; i++) {
					if (text.charAt(i) === '\\') { i++; continue; }
					if (text.charAt(i) === '"') { end = i; break; }
				}
				if (end < 0) return problem(index, '双引号标量没有闭合');
				var quoted = parseDouble(text.slice(0, end + 1));
				return {value: quoted, rest: text.slice(end + 1).trim()};
			}
			if (first === "'") {
				var out = '';
				var j = 1;
				for (; j < text.length; j++) {
					if (text.charAt(j) === "'") {
						if (text.charAt(j + 1) === "'") { out += "'"; j++; continue; }
						break;
					}
					out += text.charAt(j);
				}
				if (j >= text.length) return problem(index, '单引号标量没有闭合');
				return {value: out, rest: text.slice(j + 1).trim()};
			}
			return {value: resolvePlain(text.trim()), rest: ''};
		}

		function resolvePlain(text) {
			if (text === '' || text === '~' || /^null$/i.test(text)) return null;
			if (/^(?:true)$/i.test(text)) return true;
			if (/^(?:false)$/i.test(text)) return false;
			if (/^[-+]?\d+$/.test(text)) {
				if (/^[-+]?0\d+$/.test(text)) return text;
				return parseInt(text, 10);
			}
			if (/^[-+]?(?:\d+\.\d*|\.\d+)(?:[eE][-+]?\d+)?$/.test(text)) return parseFloat(text);
			return text;
		}

		function parseFlow(textIn, index) {
			var text = textIn.trim();
			var closing = text.charAt(0) === '{' ? '}' : ']';
			if (text.charAt(text.length - 1) !== closing) return problem(index, '流式集合没有闭合');
			var body = text.slice(1, -1).trim();
			if (closing === '}') {
				var obj = {};
				if (!body) return {value: obj, rest: ''};
				var parts = body.split(',');
				for (var i = 0; i < parts.length; i++) {
					var pair = parts[i];
					var colon = pair.indexOf(':');
					if (colon < 0) return problem(index, '流式映射缺少冒号');
					var k = parseScalar(pair.slice(0, colon), index);
					var v = parseScalar(pair.slice(colon + 1), index);
					if (k === null || v === null) return null;
					obj[String(k.value)] = v.value;
				}
				return {value: obj, rest: ''};
			}
			var arr = [];
			if (!body) return {value: arr, rest: ''};
			var items = body.split(',');
			for (var j = 0; j < items.length; j++) {
				var one = parseScalar(items[j], index);
				if (one === null) return null;
				arr.push(one.value);
			}
			return {value: arr, rest: ''};
		}

		function parseBlockScalar(header, baseIndent, index) {
			if (/^>/.test(header)) return problem(index, '折行块标量 `>` 不在本层方言内');
			var chomp = header.indexOf('-') >= 0 ? '-' : header.indexOf('+') >= 0 ? '+' : '';
			var digit = /[1-9]/.exec(header);
			var fixedIndent = digit ? baseIndent + Number(digit[0]) : -1;
			var lines = [];
			var at = index + 1;
			var bodyIndent = -1;
			for (; at < raw.length; at++) {
				var line = raw[at];
				if (line.trim() === '') { lines.push(''); continue; }
				var ind = spacesOf(line);
				if (ind <= baseIndent) break;
				if (fixedIndent < 0 && bodyIndent < 0) bodyIndent = ind;
				lines.push(line.slice(Math.min(fixedIndent < 0 ? bodyIndent : fixedIndent, ind)));
			}
			var terminatedByDedent = at < raw.length;
			pos = at;
			var finalNewline = terminatedByDedent || endsWithNewline;
			var body = lines.join('\n') + (finalNewline && lines.length ? '\n' : '');
			if (chomp === '+') return {value: body, rest: ''};
			if (chomp === '-') return {value: body.replace(/\n+$/, ''), rest: ''};
			var clip = body.replace(/\n+$/, '');
			return {value: clip === '' ? '' : clip + '\n', rest: ''};
		}

		function parseEntry(line, indent, index) {
			var body = stripComment(line).slice(indent);
			var key = '';
			var rest = '';
			if (body.charAt(0) === '"' || body.charAt(0) === "'") {
				var head = parseScalar(body, index);
				if (head === null) return null;
				key = String(head.value);
				rest = head.rest;
				if (rest.charAt(0) !== ':') return problem(index, '引号键后面缺少冒号');
				rest = rest.slice(1);
			} else {
				var colon = -1;
				for (var i = 0; i < body.length; i++) {
					if (body.charAt(i) === ':' && (i === body.length - 1 || body.charAt(i + 1) === ' ')) { colon = i; break; }
				}
				if (colon < 0) return problem(index, '缺少「键: 值」里的冒号');
				key = body.slice(0, colon).trim();
				rest = body.slice(colon + 1);
			}
			var tail = rest.trim();
			pos = index + 1;
			if (tail === '') {
				var next = nextContent(pos);
				if (next < raw.length) {
					var nextIndent = indentOf(raw[next]);
					if (nextIndent < 0) return problem(next, '缩进不能用制表符');
					if (nextIndent > indent) {
						pos = next;
						var child = parseBlock(nextIndent);
						if (child === null) return null;
						return {key: key, value: child};
					}
				}
				return {key: key, value: null};
			}
			if (tail.charAt(0) === '&' || tail.charAt(0) === '*') return problem(index, '锚点 / 别名不在本层方言内');
			if (tail.charAt(0) === '!') return problem(index, '自定义 tag 不在本层方言内');
			var leaf = tail.charAt(0) === '|' || tail.charAt(0) === '>'
				? parseBlockScalar(tail, indent, index)
				: tail.charAt(0) === '{' || tail.charAt(0) === '['
					? parseFlow(tail, index)
					: parseScalar(tail, index);
			if (leaf === null) return null;
			return {key: key, value: leaf.value};
		}

		function parseBlock(indent) {
			var first = nextContent(pos);
			if (first >= raw.length) return null;
			pos = first;
			var line = raw[first];
			var ind = indentOf(line);
			if (ind < 0) return problem(first, '缩进不能用制表符');
			if (ind !== indent) return problem(first, '缩进不一致');
			var trimmed = stripComment(line).slice(ind);
			if (trimmed === '---' || trimmed === '...') return problem(first, '多文档 / 文档结束标记不在本层方言内');
			if (/^-(?: |$)/.test(trimmed)) return parseSeq(indent);
			if (trimmed.charAt(0) === '?') return problem(first, '复杂键不在本层方言内');
			return parseMap(indent);
		}

		function parseSeq(indent) {
			var out = [];
			for (;;) {
				var at = nextContent(pos);
				if (at >= raw.length) break;
				var line = raw[at];
				var ind = indentOf(line);
				if (ind < 0) return problem(at, '缩进不能用制表符');
				if (ind < indent) break;
				if (ind > indent) return problem(at, '序列项缩进不一致');
				var body = stripComment(line).slice(ind);
				if (!/^-(?: |$)/.test(body)) break;
				pos = at + 1;
				var rest = body.slice(1).trim();
				if (rest === '') {
					var next = nextContent(pos);
					if (next < raw.length) {
						var childIndent = indentOf(raw[next]);
						if (childIndent < 0) return problem(next, '缩进不能用制表符');
						if (childIndent > indent) {
							pos = next;
							var child = parseBlock(childIndent);
							if (child === null) return null;
							out.push(child);
							continue;
						}
					}
					out.push(null);
					continue;
				}
				if (rest.charAt(0) === '|' || rest.charAt(0) === '>') {
					var block = parseBlockScalar(rest, ind, at);
					if (block === null) return null;
					out.push(block.value);
					continue;
				}
				if (rest.charAt(0) === '{' || rest.charAt(0) === '[') {
					var flow = parseFlow(rest, at);
					if (flow === null) return null;
					out.push(flow.value);
					continue;
				}
				if (/^[^"'\s][^:]*:(?: |$)/.test(rest) || /^["'][^"']*["']\s*:/.test(rest)) {
					raw[at] = ' '.repeat(ind + 2) + rest;
					pos = at;
					var map = parseMap(ind + 2);
					if (map === null) return null;
					out.push(map);
					continue;
				}
				var one = parseScalar(rest, at);
				if (one === null) return null;
				out.push(one.value);
			}
			return out;
		}

		function parseMap(indent) {
			var out = {};
			for (;;) {
				var at = nextContent(pos);
				if (at >= raw.length) break;
				var line = raw[at];
				var ind = indentOf(line);
				if (ind < 0) return problem(at, '缩进不能用制表符');
				if (ind < indent) break;
				if (ind > indent) return problem(at, '映射缩进不一致');
				var body = stripComment(line).slice(ind);
				if (/^-(?: |$)/.test(body)) break;
				pos = at;
				var entry = parseEntry(line, ind, at);
				if (entry === null) return null;
				if (Object.prototype.hasOwnProperty.call(out, entry.key)) return problem(at, '重复键 ' + entry.key);
				out[entry.key] = entry.value;
			}
			return out;
		}

		var start = nextContent(0);
		var value = null;
		if (start < raw.length) {
			pos = start;
			value = parseBlock(indentOf(raw[start]));
			if (value === null && !problems.length) problems.push({line: start + 1, message: '无法解析'});
		}
		return {value: value, problems: problems};
	}

	function parseDoc(kindId, text) {
		var kind = kindOf(kindId);
		var res = parse(text);
		if (res.problems.length) return {model: null, problems: res.problems};
		if (res.value === null) return {model: null, problems: [{line: 1, message: '文档根必须是映射'}]};
		var model = isMap(res.value) ? res.value : null;
		if (!model) return {model: null, problems: [{line: 1, message: '文档根必须是映射'}]};
		if (kind.generic) return {model: model, problems: []};
		var typed = {};
		project(kind.fields, model, typed);
		return {model: typed, problems: []};
	}


	function merge(base, patch) {
		if (!isMap(base)) return isMap(patch) ? merge({}, patch) : patch;
		if (!isMap(patch)) return patch;
		var out = {};
		var keys = Object.keys(base);
		for (var i = 0; i < keys.length; i++) out[keys[i]] = base[keys[i]];
		var patches = Object.keys(patch);
		for (var j = 0; j < patches.length; j++) {
			var key = patches[j];
			var next = patch[key];
			if (next === undefined) continue;
			out[key] = isMap(next) && isMap(out[key]) ? merge(out[key], next) : next;
		}
		return out;
	}

	function patchObject(fields, base, patch) {
		var out = isMap(base) ? merge({}, base) : {};
		var byKey = {};
		for (var i = 0; i < fields.length; i++) byKey[fields[i].key] = fields[i];
		var keys = Object.keys(isMap(patch) ? patch : {});
		for (var j = 0; j < keys.length; j++) {
			var key = keys[j];
			var next = patch[key];
			if (next === undefined) continue;
			var f = byKey[key];
			if (f && f.type === 'object') out[key] = patchObject(f.fields, out[key], next);
			else out[key] = next;
		}
		return out;
	}

	function edit(kindId, text, patch) {
		var kind = kindOf(kindId);
		var parsed = parseDoc(kindId, text);
		if (parsed.problems.length || !parsed.model) {
			return {text: '', model: null, problems: parsed.problems.length ? parsed.problems : [{line: 1, message: '文档根必须是映射'}]};
		}
		var model = kind.generic ? merge(parsed.model, patch) : patchObject(kind.fields, parsed.model, patch);
		return {text: render(kindId, model), model: model, problems: []};
	}


	var api = {
		scalar: scalar,
		render: render,
		parse: parseDoc,
		edit: edit,
		merge: merge,
		KINDS: KINDS,
		path: function (kindId, name) {
			var kind = kindOf(kindId);
			if (typeof kind.path !== 'function') throw new Error('yamldoc: ' + kindId + ' 没有文档端点');
			return kind.path(name);
		},
	};
	root.AnotifyYamlDoc = api;
	if (typeof module !== 'undefined' && module.exports) module.exports = api;
})(typeof window !== 'undefined' ? window : globalThis);
