#!/usr/bin/env node
/**
 * 契约字段守卫：前端 model 层读的 `spec.X` / `status.X` 必须在**服务端真的会发**。
 *
 * 为什么需要它（真实事故，两次）：
 *   ① `model.event` 透传 `spec.type`（v4 的 EventSpec 没有 severity）→ 首页事件行
 *      渲染出一个空 chip，被 44px 最小触控尺寸撑成空圆圈；
 *   ② 「查看原始 JSON」里印 `"dispatched": []` —— EventStatus 只有 `pruned`，
 *      服务端从不发这个字段，那是一条假事实。
 * 两次都不是逻辑写错，而是「前端以为有这个字段」。mock 又往往比契约宽松，
 * 于是 e2e/截图全绿，只有真机才露。
 *
 * 判定方法（两个来源取并集，任一存在即放行）：
 *   · `internal/**` 里**所有** struct 的 `json:"name"` tag —— 服务端能吐出的字段名；
 *   · `api/*.yaml` 里的属性名（`properties:` 下的键 + 行首的 `name:`）—— 契约面。
 *
 * 只扫 model 层的读取点（`spec.X` / `st.X` / `status.X` 在 web/partials.js 里），
 * 不扫页面里对**已建模对象**的访问 —— 那一层是 `model.event().reason` 这种形态，
 * 名字已经过这次校验。
 */
import fs from "node:fs";
import path from "node:path";
import YAML from "yaml";

const ROOT = process.cwd();
const read = (p) => fs.readFileSync(path.join(ROOT, p), "utf8");

function walk(dir, out = []) {
	for (const e of fs.readdirSync(path.join(ROOT, dir), { withFileTypes: true })) {
		const rel = path.join(dir, e.name);
		if (e.isDirectory()) walk(rel, out);
		else out.push(rel);
	}
	return out;
}

/* ---------- 1. 服务端能吐出的字段名 ---------- */
const allowed = new Set();
for (const f of walk("internal")) {
	if (!f.endsWith(".go") || f.endsWith("_test.go")) continue;
	const src = read(f);
	for (const m of src.matchAll(/json:"([A-Za-z0-9_]+)/g)) allowed.add(m[1]);
}

/* ---------- 2. 契约里的属性名 ---------- */
for (const f of walk("api")) {
	if (!f.endsWith(".yaml")) continue;
	const src = read(f);
	for (const m of src.matchAll(/^\s{4,}([a-zA-Z][A-Za-z0-9_]*):/gm)) allowed.add(m[1]);
}

/* ---------- 2b. 按资源精确校验用的 schema 属性表 ---------- */
/* 名字并集太宽松（`type` / `status` 这种名字在很多 schema 里都有），
 * 所以对能对上号的 model 函数，改用**它自己的** Spec/Status schema 来判。
 * 这样 `model.event` 读 `spec.type` 就一定会红（EventSpec 没有这个字段）。 */
const SCHEMA_PROPS = new Map();   // schemaName -> Set(prop)
for (const f of walk("api")) {
	if (!f.endsWith(".yaml")) continue;
	let doc;
	try {
		doc = YAML.parse(read(f));
	} catch {
		continue;
	}
	for (const [name, sch] of Object.entries(doc?.components?.schemas || {})) {
		const props = new Set(Object.keys(sch?.properties || {}));
		if (props.size) SCHEMA_PROPS.set(name, props);
	}
}

/* model 函数 → 它消费的 schema（spec / status） */
const MODEL_SCHEMA = {
	modelEvent: { spec: "EventSpec", status: "EventStatus" },
	modelSource: { spec: "SourceSpec", status: "SourceStatus" },
	modelCapability: { spec: "CapabilitySpec", status: "CapabilityStatus" },
	modelTask: { spec: "TaskSpec", status: "TaskStatus" },
	modelNode: { spec: "NodeSpec", status: "NodeStatus" },
	modelPlaybook: { spec: "PlaybookSpec", status: "PlaybookStatus" },
	modelNamespace: { spec: "NamespaceSpec", status: "NamespaceStatus" },
};

/* ---------- 3. 前端 model 层的读取点 ---------- */
const WORTH_SCANNING = ["web/partials.js", "web/traceview.js", "web/graphs.js", "web/source-editor.js"];
/* 白名单：这些名字属于「模块自己的选项对象」或「DOM 变量」，不是资源字段。 */
const WHITELIST = new Set([
	// graphs.js 的 options（布局参数），不是资源 spec
	"animate", "beats", "idle", "nodeSep", "rankSep", "rankdir", "root", "scope",
	"preview", "showFullscreen", "onNodeClick", "t", "nodes", "edges", "next",
]);

/* 已知遗留：服务端不发、契约里也没有，但当前**不产生用户可见的错误显示**。
 * 写在这里是为了让守卫只对「新出现」的字段报错；每条都注明为什么不修。 */
const KNOWN_DEAD = new Map([
	["warnings@web/partials.js", "API 错误信封（Status）的可选 warnings：Status 契约里没有这个字段、服务端恒不发；前端是「有就用」，无展示位。资源写入回执的顶层 warnings[]（string[]，见 CONTRACT.md §3.1）是另一条面，不在本条目范围内——本条目带文件后缀，只豁免这一个读取点"],
	["blocker", "playbook status 的 v3 遗留：if 分支在真实数据下不执行；mock 仍在造（G1 系列测试依赖），另立事项"],
	["machineLimits", "node：服务端不发，页面已回退到 localLimits（显示正确）"],
	["overrideStale", "node：服务端不发，node.html 仅 `=== true` 时渲染徽章（永不出现）"],
	["dispatched", "event：EventStatus 只有 pruned；页面已不再展示，模型层保留透传"],
	["deliveredAt", "event：契约里没有；模型层保留，没有展示位"],
	["instanceId", "task：TaskStatus 没有；message.html 那一行已删"],
	["localLimits", "node：NodeStatus 没有；页面回退到 spec.limits（显示正确）"],
	["attempts", "task：TaskStatus 没有；message.html 已有诚实的空态（message.v4.attempts_empty）"],
]);

let problems = [];
for (const f of WORTH_SCANNING) {
	if (!fs.existsSync(path.join(ROOT, f))) continue;
	const lines = read(f).split("\n");
	let curModel = null;
	lines.forEach((line, i) => {
		/* model 函数都是顶层一个 tab 缩进；遇到别的顶层 function 就退出当前作用域 */
		const fn = line.match(/^\tfunction\s+(\w+)/);
		if (fn) curModel = fn[1];
		/* 跳过注释行：注释里写旧字段名是解释，不是用法 */
		if (/^\s*(\*|\/\/|\/\*)/.test(line)) return;
		for (const m of line.matchAll(/\b(spec|st|status)\.([A-Za-z_][A-Za-z0-9_]*)/g)) {
			const name = m[2];
			const kind = m[1] === "spec" ? "spec" : "status";
			if (WHITELIST.has(name) || KNOWN_DEAD.has(name) || KNOWN_DEAD.has(`${name}@${f}`)) continue;
			const mapped = curModel ? MODEL_SCHEMA[curModel] : null;
			const schemaName = mapped ? mapped[kind] : null;
			const schemaProps = schemaName ? SCHEMA_PROPS.get(schemaName) : null;
			if (schemaProps) {
				/* 精确判定：这个 model 消费的 schema 里有没有这个字段 */
				if (!schemaProps.has(name)) {
					problems.push({ file: f, line: i + 1, name: `${m[1]}.${name}`, schema: schemaName, text: line.trim().slice(0, 90) });
				}
				continue;
			}
			/* 对不上 schema 的（自定义视图模型）退回名字并集 */
			if (allowed.has(name)) continue;
			problems.push({ file: f, line: i + 1, name: `${m[1]}.${name}`, schema: null, text: line.trim().slice(0, 90) });
		}
	});
}

/* 同一行两个 `st.X` 会命中两次，按 (file,line,name) 去重 */
const uniq = [];
const seen = new Set();
for (const p of problems) {
	const k = `${p.file}:${p.line}:${p.name}`;
	if (seen.has(k)) continue;
	seen.add(k);
	uniq.push(p);
}
problems = uniq;

if (problems.length) {
	console.error(`✗ 前端读了服务端不发、契约里也没有的字段（${problems.length} 处）：\n`);
	for (const p of problems) {
		console.error(`  ${p.file}:${p.line}  ${p.name}${p.schema ? `  (不在 ${p.schema})` : ""}\n     ${p.text}`);
	}
	console.error(
		"\n修法二选一：①页面/模型不该读这个字段 → 删掉；②它确实该存在 → 先加进 api/*.yaml 与 Go DTO，再来读。" +
			"\n（历史上这两个坑都出过：spec.type 的空 chip、status.dispatched 的假 JSON。）",
	);
	process.exit(1);
}
console.log(
	`✅ 契约字段守卫通过（${SCHEMA_PROPS.size} 个 schema、${allowed.size} 个已知字段；` +
		`已知遗留 ${KNOWN_DEAD.size} 个，见脚本内 KNOWN_DEAD）`,
);
