/*
 * Anotify · vars 表格的纯逻辑（#476）
 *
 * 为什么单独一份文件：节点页与命名空间页要显示**同一套**行（名字 / 值 / 来源 /
 * 被哪些能力用到 / 缺但被要求），两页各写一遍必然漂移；而「缺但被要求」这种判据
 * 一旦写宽松，静态检查与 e2e 都看不出来 —— 所以逻辑集中在这里，由
 * `scripts/vars-table.test.mjs` 直接跑（含变异自证）。
 *
 * 页面只负责 DOM 与请求；本文件不碰 document、不读 localStorage、不发请求。
 *
 * 序列化**不在这里**：把行折回映射（rowsToVars）之后，写进文档文本的工作由
 * `web/yamldoc.js` 统一做（同一份实现给节点页、命名空间页与预览用的 mock 端点）。
 * 本文件曾经自带一份 `yamlScalar` / `varsToYAMLLines`，那是同一套转义规则的第二份
 * 实现 —— 两份必然漂移，所以删掉了。
 *
 * 数据来源（都是既有读面，不新造后端）：
 *   · `GET /v1/nodes/{name}?namespace=<ns>` → status.effectiveVars / localVars /
 *     nodeVars / machineVars / namespaceVars（internal/server/resource_node.go）
 *   · `GET /v1/namespaces/{ns}/capabilities` → spec.requires.values
 *     （capabilitySpecView 只剥 run.files，需求侧完整保留）
 */
(function (root) {
	'use strict';

	/* 变量名的**引用**形状：与 workDir 的 `$(vars.X)` 同一套
	 * （internal/artifact/artifact.go:142 的 workDirRefPattern）。名字不符合它，
	 * 能力里就永远引用不到这个变量。 */
	var VAR_NAME_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;

	/* 服务端的**权威**形状：`internal/store/config.go:68` 的 configVarNamePattern。
	 * 前端先把引用形状挡住（即时反馈），再挡这一层：否则小写名字会在保存时被服务端
	 * 400 value_invalid，用户改了半天才发现 —— 两套规则都在这里，且都只做判断。 */
	var SERVER_NAME_RE = /^[A-Z][A-Z0-9_]{0,63}$/;

	/* checkVarName 返回 null 或一个稳定的原因码（页面把它映射成本地化文案）。
	 *   empty     —— 没填名字
	 *   shape     —— 连引用形状都不符（如 `1FOO` / `FOO-BAR`）
	 *   server    —— 引用得到，但服务端不接收（小写 / 超 64 字符）
	 */
	function checkVarName(name) {
		var value = name == null ? '' : String(name);
		if (value === '') return 'empty';
		if (!VAR_NAME_RE.test(value)) return 'shape';
		if (!SERVER_NAME_RE.test(value)) return 'server';
		return null;
	}

	/* capabilityLabel 是「被哪些能力用到」列里的稳定标签：`name@version`。
	 * 版本缺失时只给名字（不编一个 0 出来）。 */
	function capabilityLabel(doc) {
		var meta = (doc && doc.metadata) || {};
		var name = meta.name == null ? '' : String(meta.name);
		if (!name) return '';
		return meta.version == null ? name : name + '@' + meta.version;
	}

	/* requiredNames 把当前的 namespace 能力扫一遍，聚合成「变量名 → 引用它的能力」。
	 * 只读 `spec.requires.values`：契约里它的元素是 `vars.<NAME>` 字段路径
	 * （internal/artifact/artifact.go · validateRequires），所以这里剥掉 `vars.`
	 * 前缀再聚合；非 `vars.` 前缀的元素不是变量需求，跳过（不编造）。 */
	function requiredNames(capabilities) {
		var out = {};
		var items = Array.isArray(capabilities) ? capabilities : [];
		for (var i = 0; i < items.length; i++) {
			var doc = items[i] || {};
			var spec = doc.spec || {};
			var requires = spec.requires || {};
			var values = Array.isArray(requires.values) ? requires.values : [];
			var label = capabilityLabel(doc) || '#' + i;
			for (var j = 0; j < values.length; j++) {
				var name = String(values[j]);
				if (name.indexOf('vars.') !== 0) continue;
				name = name.slice('vars.'.length);
				if (!name) continue;
				if (!out[name]) out[name] = [];
				if (out[name].indexOf(label) < 0) out[name].push(label);
			}
		}
		return out;
	}

	/* buildRows 把三层值 + 需求侧拼成表格行（按名字排序，稳定）。
	 *
	 * 参数：
	 *   layer         页面在编辑哪一层：'node'（节点默认）或 'namespace'（ns 默认）
	 *   effectiveVars status.effectiveVars（server 给的 {value, source}；可缺）
	 *   machineVars   status.machineVars（local 压 node）
	 *   localVars     status.localVars（本机快照）
	 *   namespaceVars status.namespaceVars（仅 ?namespace= 时出现）
	 *   required      requiredNames() 的结果
	 *
	 * 行字段：
	 *   name / value（string|null）/ source（local|node|namespace|''）/
	 *   usedBy（能力标签数组）/ missing（被要求但没有生效值）/ locked（本层改不了）/
	 *   shadowed（本层写进去也压不住上层）
	 *
	 * 「缺但被要求」= 有能力的 requires.values 要它，而**有效值不存在**。注意不是
	 * 「本层没有」：node 层被 local 覆盖、ns 层被 node 覆盖时，任务照样解析得到值，
	 * 把它标红等于教用户改一个改不动的东西。
	 */
	function buildRows(input) {
		var src = input || {};
		// 参数名与 wire 字段同名（status.effectiveVars / machineVars / localVars /
		// namespaceVars），页面可以直接把 status 丢进来；短名是给测试/调用方的别名。
		var effective = src.effectiveVars || src.effective || {};
		var machine = src.machineVars || src.machine || {};
		var local = src.localVars || src.local || {};
		var namespaceVars = src.namespaceVars || {};
		var required = src.required || {};
		var layer = src.layer === 'namespace' ? 'namespace' : 'node';

		var names = [];
		var seen = {};
		function add(name) {
			name = String(name);
			if (!name || seen[name]) return;
			seen[name] = true;
			names.push(name);
		}
		Object.keys(effective).forEach(add);
		Object.keys(machine).forEach(add);
		Object.keys(namespaceVars).forEach(add);
		Object.keys(required).forEach(add);
		names.sort();

		return names.map(function (name) {
			var usedBy = (required[name] || []).slice();
			var value = null;
			var source = '';
			var eff = effective[name];
			if (eff && typeof eff.value === 'string') {
				value = eff.value;
				source = eff.source == null ? '' : String(eff.source);
			} else if (machine[name] != null) {
				value = String(machine[name]);
				source = local[name] != null ? 'local' : 'node';
			} else if (namespaceVars[name] != null) {
				value = String(namespaceVars[name]);
				source = 'namespace';
			}
			var missing = usedBy.length > 0 && value === null;
			// 节点页写的是节点默认层：本机快照（anode.yaml）不在中枢手里，改不了。
			// 命名空间页写的是 ns 默认层：能写，但 local/node 会压住它（shadowed 提示）。
			var locked = layer === 'node' && source === 'local';
			var shadowed = layer === 'namespace' && (source === 'local' || source === 'node');
			// owned = 这个名字属于**本层要写的那份文档**：只有 owned 的行才会进
			// PUT …/yaml 的 spec.vars。别的层提供的行不能顺手抄进本层默认值
			// （那会把 local/namespace 的值复制成 node 默认，悄悄多出一层覆盖）。
			// 用户显式编辑/新增的行由页面置 owned=true。
			var owned = source === layer;
			return {
				name: name,
				value: value,
				source: source,
				usedBy: usedBy,
				missing: missing,
				locked: locked,
				shadowed: shadowed,
				owned: owned,
			};
		});
	}

	/* rowsToVars 把表格行折回「这一层要写的 vars 映射」。
	 * `base` = 这份文档里**现有的** `spec.vars`（页面从 `GET …/yaml` 的模型里取）。
	 *
	 * 三条规则，缺一条就会静默改事实：
	 *   ① 文档里已有的值先原样搬过来。被上层压住的行（节点页的 `local`、命名空间页的
	 *      `local`/`node`）在表格里显示的是**生效值**：抄生效值会把上层的覆盖复制成本层
	 *      默认，直接丢掉又会把本层这条值删掉 —— 两种都不对，原样保留才是「没动」。
	 *   ② `owned`（属于本层文档）的行用行里的值覆盖，那就是用户改的那一份。
	 *   ③ 被标记删除、或值被清空的 `owned` 行从映射里去掉 —— 「删掉一个变量」靠这条。
	 * 别的层才有、本文档里没有的名字不会被抄进来（否则会凭空多出一层覆盖）。 */
	function rowsToVars(rows, base) {
		var out = {};
		var source = base && typeof base === 'object' && !Array.isArray(base) ? base : {};
		Object.keys(source).forEach(function (name) { out[name] = String(source[name]); });
		var list = Array.isArray(rows) ? rows : [];
		for (var i = 0; i < list.length; i++) {
			var row = list[i] || {};
			var name = String(row.name == null ? '' : row.name);
			if (!name) continue;
			if (row.deleted) { delete out[name]; continue; }
			if (!row.owned) continue;
			if (row.value == null || String(row.value) === '') { delete out[name]; continue; }
			out[name] = String(row.value);
		}
		return out;
	}

	/* suggestNames = 现有名字 ∪ 能力要求但本层缺失的名字（新增时的下拉建议）。
	 * 排序去重；被删除的行不算「现有」。 */
	function suggestNames(rows, required) {
		var out = [];
		var seen = {};
		function add(name) {
			name = String(name == null ? '' : name);
			if (!name || seen[name]) return;
			seen[name] = true;
			out.push(name);
		}
		var list = Array.isArray(rows) ? rows : [];
		for (var i = 0; i < list.length; i++) {
			var row = list[i] || {};
			if (row.deleted) continue;
			add(row.name);
		}
		Object.keys(required || {}).forEach(add);
		out.sort();
		return out;
	}

	/* renameImpact 回答「改名会踩到谁」：旧名字被多少个能力引用（0 = 没人引用）。 */
	function renameImpact(rows, oldName) {
		var list = Array.isArray(rows) ? rows : [];
		for (var i = 0; i < list.length; i++) {
			var row = list[i] || {};
			if (row.name === oldName) return (row.usedBy || []).length;
		}
		return 0;
	}

	/* diffVars 给「保存后告诉我改了什么」用（也用于单测断言只有目标键变化）。 */
	function diffVars(before, after) {
		var a = before || {};
		var b = after || {};
		var out = { added: [], removed: [], changed: [] };
		Object.keys(b).forEach(function (key) {
			if (!(key in a)) out.added.push(key);
			else if (String(a[key]) !== String(b[key])) out.changed.push(key);
		});
		Object.keys(a).forEach(function (key) {
			if (!(key in b)) out.removed.push(key);
		});
		out.added.sort();
		out.removed.sort();
		out.changed.sort();
		return out;
	}

	var api = {
		VAR_NAME_RE: VAR_NAME_RE,
		SERVER_NAME_RE: SERVER_NAME_RE,
		checkVarName: checkVarName,
		capabilityLabel: capabilityLabel,
		requiredNames: requiredNames,
		buildRows: buildRows,
		rowsToVars: rowsToVars,
		suggestNames: suggestNames,
		renameImpact: renameImpact,
		diffVars: diffVars,
	};
	root.AnotifyVars = api;
	// 前端单测（node:vm）读 window.AnotifyVars；CommonJS 导出只是给工具留一条路。
	if (typeof module !== 'undefined' && module.exports) module.exports = api;
})(typeof window !== 'undefined' ? window : globalThis);
