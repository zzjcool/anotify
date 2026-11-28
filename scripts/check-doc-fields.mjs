#!/usr/bin/env node
import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { runsAsMain } from "./lib/direct-run.mjs";

export const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
export const SPEC = "api/ingress.yaml";
export const DOC = "web-src/pages/docs.html";
export const SCHEMA = "EventIngestRequest";
export const KEY_SOURCE = "internal/auth/apikey.go";
export const DOC_PREFIX_DIRS = ["web-src/pages", "web-src/locales"];
export const FLOORS = { specFields: 3, docFields: 3, prefixTokens: 1 };

export const RULE = {
	MISSING: "doc-field-missing",
	EXTRA: "doc-field-extra",
	REQUIRED: "doc-field-required",
	PREFIX: "doc-key-prefix",
};

export const USAGE = `用法：
  node scripts/check-doc-fields.mjs             # 体检：exit 0 绿 / 1 有 findings / 2 解析或环境问题
  node scripts/check-doc-fields.mjs --self-test # 守卫自身的红/绿双侧自证
  node scripts/check-doc-fields.mjs --help

判据（#593：接入文档教的字段服务端不认——按文档写 idemKey 不去重、traceId 被静默忽略）：
  a. 事实源：${SPEC} 里 ${SCHEMA}.properties / required；
  b. 扫面：${DOC} 的 #events 字段表（param-name + req/opt 标记）；
  c. 两边字段集合必须相等，必填标记必须一致；
  d. Key 前缀：文档里出现的 ant_<label>_ 必须是 ${KEY_SOURCE} 的 scopeLabel 真能生成的，
     否则用户照着文档造的 Key 前缀与服务端对不上；
  e. 任一解析面归零一律 exit 2 —— 绿与「没检查」不能同形。`;

export function lineAt(src, index) {
	return src.slice(0, index).split("\n").length;
}

export function parseSpecFields(yaml) {
	const lines = yaml.split("\n");
	const start = lines.findIndex((l) => new RegExp(`^    ${SCHEMA}:\\s*$`).test(l));
	if (start < 0) return { err: `找不到 schema ${SCHEMA}` };
	let end = lines.length;
	for (let i = start + 1; i < lines.length; i += 1) {
		if (/^    [A-Za-z_]\w*:\s*$/.test(lines[i])) {
			end = i;
			break;
		}
	}
	const schema = lines.slice(start, end);
	const propIdx = schema.findIndex((l) => /^ {6}properties:\s*$/.test(l));
	if (propIdx < 0) return { err: `${SCHEMA} 没有 properties 块` };
	const props = [];
	for (let i = propIdx + 1; i < schema.length; i += 1) {
		const line = schema[i];
		if (line.trim() && !/^ {7,}\S/.test(line)) break;
		const name = /^ {8}([A-Za-z_]\w*):\s*$/.exec(line);
		if (name) props.push({ name: name[1], line: start + i + 1 });
	}
	const reqLine = schema.findIndex((l) => /^ {6}required:/.test(l));
	if (reqLine < 0) return { err: `${SCHEMA} 没有 required 列表` };
	const reqText = /^ {6}required:\s*\[(.*)\]\s*$/.exec(schema[reqLine]);
	if (!reqText) return { err: `${SCHEMA}.required 不是行内列表：${schema[reqLine].trim()}` };
	const required = reqText[1]
		.split(",")
		.map((s) => s.trim())
		.filter(Boolean);
	if (props.length === 0) return { err: `${SCHEMA}.properties 解析出 0 个字段` };
	return { props, required, fields: props.map((p) => p.name) };
}

export function parseDocFields(html) {
	const start = html.search(/<section[^>]*id="events"/);
	if (start < 0) return { err: `${DOC} 找不到 #events 小节` };
	const end = html.indexOf("</section>", start);
	if (end < 0) return { err: `${DOC} 的 #events 小节没有收尾` };
	const block = html.slice(start, end);
	const fields = [];
	let cursor = 0;
	for (;;) {
		const at = block.indexOf("<tr>", cursor);
		if (at < 0) break;
		const next = block.indexOf("<tr>", at + 4);
		const row = block.slice(at, next < 0 ? block.length : next);
		cursor = next < 0 ? block.length : next;
		const name = /class="param-name"\s*>([A-Za-z_][\w-]*)</.exec(row);
		if (!name) continue;
		const req = /class="req"/.test(row);
		const opt = /class="opt"/.test(row);
		if (req === opt) return { err: `字段 ${name[1]} 的必填/可选标记不唯一（req=${req} opt=${opt}）` };
		fields.push({ name: name[1], required: req, line: lineAt(html, start + at) });
	}
	if (fields.length === 0) return { err: `${DOC} 的 #events 字段表解析出 0 个字段` };
	return { fields };
}

export function diffFields(spec, doc) {
	const findings = [];
	const docNames = new Set(doc.map((f) => f.name));
	for (const name of spec.fields) {
		if (!docNames.has(name)) {
			findings.push({ rule: RULE.MISSING, line: doc.length ? doc[0].line : 0, detail: `服务端接受 ${name}，文档字段表没有` });
		}
	}
	const specNames = new Set(spec.fields);
	for (const f of doc) {
		if (!specNames.has(f.name)) findings.push({ rule: RULE.EXTRA, line: f.line, detail: `文档写了 ${f.name}，${SPEC} 的 ${SCHEMA} 没有这个字段` });
	}
	for (const f of doc) {
		if (!specNames.has(f.name)) continue;
		const want = spec.required.includes(f.name);
		if (want !== f.required) {
			findings.push({
				rule: RULE.REQUIRED,
				line: f.line,
				detail: `${f.name} 在文档里被标成${f.required ? "必填" : "可选"}，${SPEC} 里是${want ? "必填" : "可选"}`,
			});
		}
	}
	return findings;
}

export function parseScopeLabels(goSrc) {
	const m = /func scopeLabel\([\s\S]*?\n\}/.exec(goSrc);
	if (!m) return { err: `${KEY_SOURCE} 找不到 scopeLabel（或它没有正常收尾）` };
	const labels = [...m[0].matchAll(/return\s+"([a-z]+)"/g)].map((x) => x[1]);
	if (labels.length === 0) return { err: `${KEY_SOURCE} 的 scopeLabel 里没有 return "<label>"` };
	return { labels };
}

export function scanPrefixes(src, file) {
	const out = [];
	const re = /\bant_[a-z]+_/g;
	let m;
	while ((m = re.exec(src)) !== null) out.push({ token: m[0], file, line: lineAt(src, m.index) });
	return out;
}

export function diffPrefixes(tokens, labels) {
	const allowed = new Set(labels.map((l) => `ant_${l}_`));
	const findings = [];
	for (const t of tokens) {
		if (allowed.has(t.token)) continue;
		findings.push({
			rule: RULE.PREFIX,
			file: t.file,
			line: t.line,
			detail: `文档里的 ${t.token} 不是 scopeLabel 生成的（${KEY_SOURCE} 只会生成 ${[...allowed].join(" / ")}）`,
		});
	}
	return findings;
}

export function floorProblems(spec, doc, tokens) {
	const out = [];
	if (!spec || spec.fields.length < FLOORS.specFields) out.push(`spec 字段 ${spec?.fields.length ?? 0} < ${FLOORS.specFields}`);
	if (!doc || doc.length < FLOORS.docFields) out.push(`文档字段 ${doc?.length ?? 0} < ${FLOORS.docFields}`);
	if (!tokens || tokens.length < FLOORS.prefixTokens) out.push(`文档 Key 前缀 ${tokens?.length ?? 0} < ${FLOORS.prefixTokens}`);
	return out;
}

export async function loadReal(root = ROOT) {
	const read = (p) => fs.readFile(path.join(root, p), "utf8");
	const [yaml, html, goSrc] = await Promise.all([read(SPEC), read(DOC), read(KEY_SOURCE)]);
	const spec = parseSpecFields(yaml);
	const doc = parseDocFields(html);
	const scope = parseScopeLabels(goSrc);
	const tokens = [];
	for (const dir of DOC_PREFIX_DIRS) {
		const names = await fs.readdir(path.join(root, dir)).catch(() => []);
		const files = names.filter((n) => n.endsWith(".html") || n.endsWith(".yaml"));
		for (const name of files) {
			const rel = path.posix.join(dir, name);
			tokens.push(...scanPrefixes(await read(rel), rel));
		}
	}
	return { spec, doc, scope, tokens };
}

export function judge(real) {
	const problems = floorProblems(real.spec.err ? null : real.spec, real.doc.err ? null : real.doc.fields, real.tokens);
	if (real.spec.err) problems.push(real.spec.err);
	if (real.doc.err) problems.push(real.doc.err);
	if (real.scope.err) problems.push(real.scope.err);
	if (problems.length) return { problems, findings: [] };
	const findings = diffFields(real.spec, real.doc.fields).concat(diffPrefixes(real.tokens, real.scope.labels));
	return { problems, findings };
}

export const SELF_DOC = `<section id="events" class="doc-anchor">
  <tr><td><span class="param-name">source</span><span class="req">必填</span></td></tr>
  <tr><td><span class="param-name">type</span><span class="opt">可选</span></td></tr>
  <tr><td><span class="param-name">id</span><span class="opt">可选</span></td></tr>
  <tr><td><span class="param-name">payload</span><span class="opt">可选</span></td></tr>
</section>
<span class="param-name">outside</span>`;

export const SELF_SPEC = `  schemas:
    EventIngestRequest:
      type: object
      required: [source]
      properties:
        source:
          type: string
        type:
          type: string
        id:
          type: string
        payload:
          type: object
          additionalProperties: true
      additionalProperties: true
    HookBody:
      type: object
      properties:
        type:
          type: string
`;

export const SELF_GO = `func scopeLabel(scopes []string) string {
	if has(ScopeNotifySend) {
		return "send"
	}
	return "key"
}`;

export const SELF_TEST = [
	{ name: "green：字段集合一致", doc: SELF_DOC, want: [] },
	{ name: "red（变异）：文档写 idemKey", doc: SELF_DOC.replace('"param-name">id<', '"param-name">idemKey<'), want: [RULE.EXTRA, RULE.MISSING] },
	{ name: "red：文档漏掉 id", doc: SELF_DOC.replace('<tr><td><span class="param-name">id</span><span class="opt">可选</span></td></tr>', ""), want: [RULE.MISSING] },
	{
		name: "red：必填标记相反",
		doc: SELF_DOC.replace('<span class="param-name">source</span><span class="req">', '<span class="param-name">source</span><span class="opt">'),
		want: [RULE.REQUIRED],
	},
	{ name: "red：旧前缀 ant_live_", doc: SELF_DOC, tokens: ["ant_live_", "ant_live_"], want: [RULE.PREFIX, RULE.PREFIX] },
	{ name: "green：scopeLabel 真会生成的 send", doc: SELF_DOC, tokens: ["ant_send_"], want: [] },
];

export function selfTestCases() {
	return SELF_TEST.map((c) => {
		const spec = parseSpecFields(SELF_SPEC);
		const doc = parseDocFields(c.doc);
		const scope = parseScopeLabels(SELF_GO);
		const tokens = (c.tokens ?? ["ant_send_"]).map((t) => ({ token: t, file: DOC, line: 1 }));
		const findings = spec.err || doc.err || scope.err ? [] : diffFields(spec, doc.fields).concat(diffPrefixes(tokens, scope.labels));
		return {
			name: c.name,
			got: findings.map((f) => f.rule),
			want: c.want,
			errs: [spec.err, doc.err, scope.err].filter(Boolean),
			floors: floorProblems(spec.err ? null : spec, doc.err ? null : doc.fields, tokens),
		};
	});
}

export async function main(root = ROOT) {
	const real = await loadReal(root);
	const { problems, findings } = judge(real);
	if (problems.length) {
		console.error("❌ 文档字段检查：解析面坏了（绿与「没检查」不能同形）");
		for (const p of problems) console.error(`   ${p}`);
		return 2;
	}
	if (findings.length) {
		console.error(`❌ 文档字段检查：${findings.length} 处与 ${SPEC} / ${KEY_SOURCE} 不一致（照文档接入会静默失效）`);
		for (const f of findings) console.error(`   ${f.file ?? DOC}:${f.line}  ${f.rule}  ${f.detail}`);
		console.error("");
		console.error(`   改法：字段表改成服务端真的认的字段（${SPEC} 的 ${SCHEMA}），Key 前缀改成 scopeLabel 的产出；`);
		console.error("   不要改这个守卫的判据（它是文档的唯一执行者）。");
		return 1;
	}
	console.log(
		`✅ 文档字段检查：${DOC} #events ↔ ${SPEC} ${SCHEMA}（${real.spec.fields.join("/")}）· required ${real.spec.required.join("/")} · Key 前缀 ${[...new Set(real.tokens.map((t) => t.token))].join("/")}`,
	);
	return 0;
}

export async function selfTest() {
	let failed = 0;
	const check = (ok, label, detail = "") => {
		console.log(`${ok ? "✅" : "❌"} ${label}${ok ? "" : ` —— ${detail}`}`);
		if (!ok) failed += 1;
	};
	for (const c of selfTestCases()) {
		const sorted = (a) => [...a].sort();
		const ok = JSON.stringify(sorted(c.got)) === JSON.stringify(sorted(c.want)) && c.errs.length === 0 && c.floors.length === 0;
		check(ok, c.name, `want ${JSON.stringify(c.want)} got ${JSON.stringify(c.got)} errs=${JSON.stringify(c.errs)} floors=${JSON.stringify(c.floors)}`);
	}

	const specNoSchema = parseSpecFields("    HookBody:\n      properties:\n        a:\n");
	check(Boolean(specNoSchema.err), "fail-loud：找不到 schema", JSON.stringify(specNoSchema));
	const specNoProps = parseSpecFields("    EventIngestRequest:\n      required: [source]\n");
	check(Boolean(specNoProps.err), "fail-loud：没有 properties", JSON.stringify(specNoProps));
	const specNoReq = parseSpecFields("    EventIngestRequest:\n      properties:\n        source:\n");
	check(Boolean(specNoReq.err), "fail-loud：没有 required", JSON.stringify(specNoReq));
	const specEmptyDoc = parseDocFields("<section id=\"events\"><p>无表</p></section>");
	check(Boolean(specEmptyDoc.err), "fail-loud：字段表解析出 0 个字段", JSON.stringify(specEmptyDoc));
	const goNoLabel = parseScopeLabels("func scopeLabel(s []string) string { return \"\" }");
	check(Boolean(goNoLabel.err), "fail-loud：scopeLabel 没有 return \"<label>\"", JSON.stringify(goNoLabel));

	const real = await loadReal();
	const verdict = judge(real);
	check(verdict.problems.length === 0, "真实仓库文件可解析（下限达标）", JSON.stringify(verdict.problems));
	check(real.spec.fields.length >= FLOORS.specFields, "真实 spec 字段数下限", String(real.spec.fields.length));
	check(real.tokens.length >= FLOORS.prefixTokens, "真实文档里至少出现一个 Key 前缀", String(real.tokens.length));
	return failed === 0;
}

const isDirectRun = runsAsMain(import.meta.url);
if (isDirectRun) {
	const argv = process.argv.slice(2);
	const known = new Set(["--self-test", "--help", "-h"]);
	const unknown = argv.filter((a) => !known.has(a));
	if (unknown.length) {
		console.error(`✗ 不认识的参数：${unknown.join(" ")}`);
		console.error(USAGE);
		process.exit(2);
	}
	if (argv.includes("--help") || argv.includes("-h")) {
		console.log(USAGE);
		process.exit(0);
	}
	const finished = argv.includes("--self-test")
		? selfTest().then((passed) => process.exit(passed ? 0 : 1))
		: main().then((code) => process.exit(code));
	finished.catch((e) => {
		console.error(e);
		process.exit(2);
	});
}
