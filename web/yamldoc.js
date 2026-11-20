/*
 * Anotify · YAML 文档层（#473 的第 1 步 / 第 3 步的前端半边）
 *
 * 为什么要有这一份文件：同一个资源以前有三套「把事实写成 YAML 文本」的实现 ——
 * 节点页自带 yamlScalar / yamlList / yamlMap / docToYAML，命名空间页自带
 * namespaceYAMLText，vars 表格自带 yamlScalar / varsToYAMLLines（预览用的 mock
 * 还有第四套）。四套实现的转义规则、空集合形态、字段顺序各自演化，于是同一类
 * 缺陷在每个资源上重新长一遍（#454 的 `tags` 被静默清空就是「页面自己拼一份、
 * 拼的时候漏了一个字段」）。
 *
 * 现在只有这一份：
 *
 *   scalar(v)              标量转义（唯一实现；页面/mock/表格都不再各写一份）
 *   render(kind, model)    「模型 → 文档文本」（唯一实现）
 *   parse(kind, text)      「文档文本 → 模型」（唯一实现；拒绝集见下）
 *   merge(base, patch)     表单编辑：只改它改的键，其余逐字保留
 *   path(kind, name)       该资源的文档端点（写入口永远是 PUT …/yaml）
 *
 * 四条设计约束（改之前先读）：
 *
 *  1. **解析不是校验**。严格校验（未知字段 / 类型 / 必填 / 重复键 / 锚点）在服务端
 *     （`internal/yamldoc`），错误带行号与字段路径。这里的 parse 只为「表单编辑同一
 *     份文档」提供模型：它**不**拒绝未知字段（原样带回模型，写回去让服务端具名拒绝），
 *     也不认本文档方言之外的东西（块标量 `>`、锚点/别名、自定义 tag、多文档、制表符
 *     缩进、复杂键）—— 遇到就返回 problems，调用方必须**不要**渲染，否则就是把
 *     「读不懂」写成「用户清空了」（#454 的同一类错误）。
 *
 *  2. **读面与写面的唯一差异是「未知就省略」**。服务端读面为了表达「不压住下层」会写
 *     `total: null`，而写面要求 total 是 ≥1 的整数（`store.ValidateLimits`）。所以模型里
 *     total 为 null 时**整键省略**（省略 = 不动），绝不写成 `total: null` 或 `total: 0`。
 *     同理 `tags` 未知（null）时整键省略，只有「已知为空」（[]）才写 `tags: []`。
 *
 *  3. **字段顺序由 kind 表固定**，不由模型的键顺序决定；kind 表之外的键（扩展字段）
 *     按模型里的原顺序追加在后面。所以同一份事实永远序列化成同一份文本，而扩展字段
 *     不会因为「表单不认识它」被丢掉。
 *
 *  4. **值可明文**：vars / envPassthrough 的值不是凭据，本层不做脱敏（脱敏是服务端读
 *     投影的职责：`POST …/tokens` 一次性明文、`GET …/sources/{name}/yaml` 只给
 *     hookUrl/secretSet）。`envPassthrough` 是**名字白名单**（列表），不是 vars 表
 *     （映射）—— 两者在 kind 表里是不同的类型，merge 也不会把一个列表搬进映射。
 */
(function (root) {
	'use strict';

	/* ================= ① 标量：唯一一份转义规则 ================= */

	/* 可以裸写（不加引号）的形状：字母/下划线开头、只含不会改变语义的字符。
	 * 数字开头一律不裸写 —— `010` 是 YAML 的八进制、`123` 是整数，两种都会让
	 * 「看起来是字符串」的值变成另一个类型（服务端按类型解码，会 400）。 */
	var PLAIN = /^[A-Za-z_][A-Za-z0-9_./:@+-]*$/;
	/* 保留字：裸写会被 YAML 解析成 bool / null，不是字符串。 */
	var RESERVED = /^(?:true|false|null|yes|no|on|off|y|n|~)$/i;

	function scalar(value) {
		if (value === null || value === undefined) return 'null';
		if (typeof value === 'boolean') return value ? 'true' : 'false';
		if (typeof value === 'number') return isFinite(value) ? String(value) : JSON.stringify(String(value));
		var text = String(value);
		if (text === '') return '""';
		if (PLAIN.test(text) && !RESERVED.test(text)) return text;
		/* 双引号标量就是 JSON 字符串（JSON 的转义是 YAML 双引号标量的子集），
		 * 所以这里不需要第二套转义规则 —— 也不会写错。 */
		return JSON.stringify(text);
	}

	/* ================= ② 通用块发射器 ================= */

	function isMap(value) {
		return value !== null && typeof value === 'object' && !Array.isArray(value);
	}

	/* 多行字符串 → 块标量 `|`（保住换行与每行缩进；能力脚本正文靠它逐字保留）。
	 * 尾部空行去掉：YAML 的块标量会自己补一个换行，多写的空行会变成正文的一部分。 */
	function blockScalar(text, indent) {
		var body = String(text).replace(/\n+$/, '');
		var out = ['|'];
		var rows = body.split('\n');
		for (var i = 0; i < rows.length; i++) out.push(rows[i] === '' ? '' : ' '.repeat(indent) + rows[i]);
		return out;
	}

	/* 序列项里的嵌套块：第一个键内联在 `- ` 后面（`- key: v`），其余行原样跟。
	 * 这与 yaml.v3 的排版一致，也是 mock 的 `/yaml` 里既有的文本形状。 */
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
			if (typeof item === 'string' && item.indexOf('\n') >= 0) {
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
				if (typeof child === 'string' && child.indexOf('\n') >= 0) {
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

	/* ================= ③ kind 表：每种资源文档的形状 ================= */

	/* 字段声明：
	 *   key   文档里的键名
	 *   type  string | int | strings | string-map | int-map | object
	 *   omit  'missing'（默认：null/undefined 整键省略）| 'never'（缺了也按空集合写）
	 *   empty 空集合的文本形态（渲染器自动产出 `{}` / `[]`，这里只做声明以便对账）
	 *   fields object 的子字段
	 *
	 * `envPassthrough` 是**名字列表**（strings），`vars` 是**名字→值映射**（string-map）：
	 * 两者类型不同，谁也不会被渲染成另一个的形状。
	 */

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
				{ key: 'namespaces', type: 'strings', empty: '[]' },
				{ key: 'vars', type: 'string-map', empty: '{}' },
				{
					key: 'limits',
					type: 'object',
					fields: [
						/* 读面会写 `total: null`；写面要求 ≥1 的整数 → null 就整键省略（不动）。 */
						{ key: 'total', type: 'int', omit: 'missing' },
						{ key: 'perCapability', type: 'int-map', empty: '{}' },
					],
				},
				/* #454：未知（null）就整键省略，只有「已知为空」才写 `tags: []`。 */
				{ key: 'tags', type: 'strings', empty: '[]' },
			],
		},
	];

	var KINDS = {
		/* 节点文档（GET/PUT /v1/nodes/{name}/yaml）。 */
		Node: {
			apiVersion: 'anotify/v1',
			kind: 'Node',
			path: function (name) {
				return '/v1/nodes/' + encodeURIComponent(name) + '/yaml';
			},
			fields: NODE_FIELDS,
		},
		/* 命名空间文档（GET/PUT /v1/namespaces/{name}/yaml）。 */
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
						/* PUT 是整份替换：`spec.vars` 缺席被服务端拒成 value_invalid（「忘了写」
						 * 与「清空」不可区分）。所以这里**不**补 `{}` —— 让服务端具名报错。 */
						{ key: 'vars', type: 'string-map', empty: '{}' },
					],
				},
			],
		},
		/* 能力文档（GET /v1/namespaces/{ns}/capabilities/{name}/yaml，只读）。
		 * 形状由 artifact.Spec 决定，本层只做通用发射：键顺序 = 模型顺序（服务端重建时
		 * 已按契约排好），`run.files` 的多行正文是块标量。 */
		Capability: {apiVersion: 'anotify/v1', kind: 'Capability', generic: true},
		/* 源文档（GET …/sources/{name}/yaml，只读；读投影只有 hookUrl/secretSet）。 */
		Source: {apiVersion: 'anotify/v1', kind: 'Source', generic: true},
		/* 节点本机配置（anode.yaml）。控制台不写它（写它的是 install 脚本），但它的字段
		 * 规则收在同一处：`envPassthrough` 是名字白名单、`vars` 是值映射 —— 两者混用
		 * 就是一条凭据回显通道（值会进 vars 表、被复制走）。 */
		AnodeConfig: {
			kind: 'anode.yaml',
			fields: [
				{ key: 'name', type: 'string' },
				{ key: 'server', type: 'string' },
				{ key: 'token', type: 'string', omit: 'missing' },
				{ key: 'namespaces', type: 'strings', empty: '[]' },
				{ key: 'vars', type: 'string-map', empty: '{}' },
				{
					key: 'limits',
					type: 'object',
					fields: [
						{ key: 'total', type: 'int', omit: 'missing' },
						{ key: 'perCapability', type: 'int-map', empty: '{}' },
					],
				},
				{ key: 'envPassthrough', type: 'strings', empty: '[]' },
				{ key: 'listen', type: 'string', omit: 'missing' },
			],
		},
	};

	/* ================= ④ 模型 → 文本 ================= */

	function kindOf(kindId) {
		var kind = KINDS[kindId];
		if (!kind) throw new Error('yamldoc: 未注册的 kind ' + kindId);
		return kind;
	}

	/* 按 kind 表把模型投影成「待发射的有序映射」：
	 *   · 表里声明的键按声明顺序；
	 *   · 值为 null/undefined 的键跳过（= 不动），除非声明 omit: 'never'；
	 *   · 表里没声明的键按模型里的顺序追加（扩展字段一个都不丢）。
	 * 投影结果是纯数据，交给同一个发射器 —— 所以「排版规则」也只有一份。 */
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
			if (value === null || value === undefined) continue;
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
		if (value === null || value === undefined) return value;
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

	/* render 是「模型 → 文档文本」的唯一入口。未知 kind 直接抛（拼错不该静默写半份文档）。 */
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

	/* ================= ⑤ 文本 → 模型 ================= */

	/* 只认服务端读面会产出的那套方言 + 用户手写时的等价写法：
	 *   块映射 / 块序列 / 流式空集合与标量集合（{}、[]、[a, b]、{k: v}）
	 *   / 单双引号标量 / 块标量 `|` `|-` / 整行与行尾注释。
	 * 拒绝集（返回 problems，调用方不得渲染）：`>` 折行块、锚点别名（& / *）、自定义 tag（!）、
	 * 多文档（---）、复杂键（? ）、制表符缩进、重复键。 */
	function parse(text) {
		var problems = [];
		var raw = String(text === null || text === undefined ? '' : text).replace(/\r\n?/g, '\n').split('\n');
		var pos = 0;

		function problem(index, message) {
			problems.push({line: index + 1, message: message});
			return null;
		}

		function indentOf(line) {
			var i = 0;
			while (i < line.length && line.charAt(i) === ' ') i++;
			if (line.charAt(i) === '\t' && line.slice(0, i + 1).trim() === '') return problem(pos + i, '缩进不能用制表符');
			return i;
		}

		function isBlank(line) {
			var t = line.trim();
			return t === '' || t.charAt(0) === '#';
		}

		/* 下一条有内容的行（跳过空行与整行注释）。 */
		function nextContent(from) {
			var i = from;
			while (i < raw.length && isBlank(raw[i])) i++;
			return i;
		}

		/* 去掉行尾注释（` #…`）：引号内的 # 不算注释。 */
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

		/* 标量：返回 {value, rest}（rest = 该行剩下还没消费的文本）。 */
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

		/* 裸标量按 YAML core schema 归型：布尔 / null / 整数 / 小数，其余是字符串。
		 * 类型必须归对，否则「读回来的 4」再写出去会变成 `"4"`（字符串）→ 服务端按 int
		 * 解码报 400（`limits.total` 就是这么被钉住的）。 */
		function resolvePlain(text) {
			if (text === '' || text === '~' || /^null$/i.test(text)) return null;
			if (/^(?:true)$/i.test(text)) return true;
			if (/^(?:false)$/i.test(text)) return false;
			if (/^[-+]?\d+$/.test(text)) {
				/* 前导零不是十进制整数（`010` 是八进制）—— 不当数字，当字符串。 */
				if (/^[-+]?0\d+$/.test(text)) return text;
				return parseInt(text, 10);
			}
			if (/^[-+]?(?:\d+\.\d*|\.\d+)(?:[eE][-+]?\d+)?$/.test(text)) return parseFloat(text);
			return text;
		}

		/* 流式集合：{} / [] / [a, b] / {k: v, k2: v2}（只支持标量与空集合，够读服务端读面
		 * 与文档样例；嵌套更深的用块风格）。 */
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

		/* 块标量 `|` / `|-`：正文行必须比键更缩进；公共缩进取正文首行的缩进。 */
		function parseBlockScalar(header, baseIndent, index) {
			if (/^>/.test(header)) return problem(index, '折行块标量 `>` 不在本层方言内');
			var strip = /^-/.test(header);
			var lines = [];
			var at = index + 1;
			var bodyIndent = -1;
			for (; at < raw.length; at++) {
				var line = raw[at];
				if (line.trim() === '') { lines.push(''); continue; }
				var ind = indentOf(line);
				if (ind === null) return null;
				if (ind <= baseIndent) break;
				if (bodyIndent < 0) bodyIndent = ind;
				lines.push(line.slice(Math.min(bodyIndent, ind)));
			}
			pos = at;
			while (lines.length && lines[lines.length - 1] === '') lines.pop();
			var body = lines.join('\n');
			return {value: strip ? body : body + '\n', rest: ''};
		}

		/* 一行映射条目 → {key, value}；值可能是嵌套块（在同一缩进层级续读）。 */
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
					if (nextIndent === null) return null;
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

		/* 块：先看首行决定是映射还是序列。 */
		function parseBlock(indent) {
			var first = nextContent(pos);
			if (first >= raw.length) return null;
			pos = first;
			var line = raw[first];
			var ind = indentOf(line);
			if (ind === null) return null;
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
				if (ind === null) return null;
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
						if (childIndent === null) return null;
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
				/* `- 键: 值` = 序列项是一个映射：把这一行改写成多缩进两格的映射行再读。 */
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
				if (ind === null) return null;
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

	/* parseDoc = parse + 按 kind 归型。返回 {model, problems}；problems 非空时 model 不可信，
	 * 调用方必须把它当成「这份文本只能整份手改」，不要重新渲染。 */
	function parseDoc(kindId, text) {
		var kind = kindOf(kindId);
		var res = parse(text);
		if (res.problems.length || res.value === null) return {model: null, problems: res.problems};
		var model = isMap(res.value) ? res.value : null;
		if (!model) return {model: null, problems: [{line: 1, message: '文档根必须是映射'}]};
		if (kind.generic) return {model: model, problems: []};
		var typed = {};
		project(kind.fields, model, typed);
		/* project 已按 kind 表归型并带回扩展字段；这里再保证 kind 表的键一定在模型里
		 * （缺席是合法的：省略 = 不动）。 */
		return {model: typed, problems: []};
	}

	/* ================= ⑥ 合并（表单编辑） ================= */

	/* merge(base, patch)：patch 里出现的键覆盖 base（对象递归、数组/标量整体替换），
	 * patch 里**没有**的键原样留着 —— 这就是「表单只改它改的那一项，其余逐字不变」。
	 * 数组整体替换是刻意的：`tags: []` 表达「清空」，合并语义会把它读成「不动」。 */
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

	/* edit(kind, text, patch) = 「表单改一处」的完整动作：读服务端原文 → 只改 patch 里的键
	 * → 重新序列化。页面不再自己拼文档：这就是「表单与原始编辑共用同一份文档」的那一处。
	 * 返回 {text, model, problems}；problems 非空时 text 为空串，调用方必须**不要**写。 */
	function edit(kindId, text, patch) {
		var parsed = parseDoc(kindId, text);
		if (parsed.problems.length || !parsed.model) {
			return {text: '', model: null, problems: parsed.problems.length ? parsed.problems : [{line: 1, message: '文档根必须是映射'}]};
		}
		var model = merge(parsed.model, patch);
		return {text: render(kindId, model), model: model, problems: []};
	}

	/* ================= ⑦ 导出 ================= */

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
	// 前端单测（node:vm）读 window.AnotifyYamlDoc；CommonJS 导出只是给工具留一条路。
	if (typeof module !== 'undefined' && module.exports) module.exports = api;
})(typeof window !== 'undefined' ? window : globalThis);
