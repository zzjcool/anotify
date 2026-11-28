#!/usr/bin/env node
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { runsAsMain } from "./lib/direct-run.mjs";

export const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

export const MIN_GO_FILES = 50;

const SKIP_DIRS = new Set(["node_modules", "vendor", ".git", "dist", "testdata"]);

export const RULES = [
	{
		id: "RAW_ERR_IN_RESPONSE",
		re: /writeErr\s*\(\s*w\s*,\s*(?:500|http\.StatusInternalServerError)\s*,\s*err\s*\.\s*Error\s*\(\s*\)\s*\)/,
		hint: "500 的错误响应不许回 err.Error()：原文进日志（logInternal），响应只回 internalErrMsg",
	},
	{
		id: "RAW_ERR_IN_401",
		re: /writeErr\s*\(\s*w\s*,\s*(?:401|http\.StatusUnauthorized)\s*,\s*err\s*\.\s*Error\s*\(\s*\)\s*\)/,
		hint: "401 不许回 err.Error()：原文进日志，响应回固定文案（口径见 #618）",
	},
	{
		id: "RAW_ERR_IN_STATUS500",
		re: /writeStatus\s*\(\s*w\s*,\s*(?:500|http\.StatusInternalServerError)\s*,[^;]*err\s*\.\s*Error\s*\(\s*\)/,
		hint: "信封 500（writeStatus）的 message 不许是 err.Error()：原文进日志，响应回 internalErrMsg",
	},
	{
		id: "RAW_ERR_IN_APISTATUS500",
		re: /writeAPIStatus\s*\(\s*w\s*,\s*(?:500|http\.StatusInternalServerError)\s*,[^;]*err\s*\.\s*Error\s*\(\s*\)/,
		hint: "信封 500（writeAPIStatus）的 message 不许是 err.Error()：原文进日志，响应回 InternalErrorMessage",
	},
];

const ADMIN_ERR_LITERAL = /&\s*adminErr\s*\{/g;

function literalEnd(text, start) {
	let depth = 0;
	for (let i = start; i < text.length; i++) {
		const ch = text[i];
		if (ch === '"' || ch === "'" || ch === "`") {
			const quote = ch;
			i++;
			while (i < text.length) {
				if (quote !== "`" && text[i] === "\\") {
					i += 2;
					continue;
				}
				if (text[i] === quote) break;
				i++;
			}
			continue;
		}
		if (ch === "{") depth++;
		else if (ch === "}" && --depth === 0) return i;
	}
	return -1;
}

export function scanAdminErrLiterals(text) {
	const findings = [];
	ADMIN_ERR_LITERAL.lastIndex = 0;
	let m;
	while ((m = ADMIN_ERR_LITERAL.exec(text))) {
		const open = text.indexOf("{", m.index);
		const end = literalEnd(text, open);
		if (end < 0) continue;
		const body = text.slice(open, end + 1);
		if (/\bcause\s*:/.test(body)) continue;
		const is500 =
			/^\{\s*(?:500|http\.StatusInternalServerError)\s*,/.test(body) ||
			/\bcode\s*:\s*(?:500|http\.StatusInternalServerError)\b/.test(body);
		const rawMsg = /\bmsg\s*:\s*err\s*\.\s*Error\s*\(\s*\)/.test(body);
		if (!is500 && !rawMsg) continue;
		findings.push({
			line: text.slice(0, m.index).split("\n").length,
			rule: is500 ? "ADMIN_ERR_500_NO_CAUSE" : "ADMIN_ERR_RAW_MSG_NO_CAUSE",
			hint: is500
				? "adminErr 的 500 必须带 cause（或直接 adminInternalErr(err)）：writeAdminErr 才会把原文压进日志"
				: "adminErr 的 msg 不许是 err.Error()：用户面文案固定，原文只进日志（#624）",
			text: body.split("\n")[0].trim(),
		});
	}
	return findings;
}

export function stripGoComments(src) {
	let out = "";
	let i = 0;
	const n = src.length;
	while (i < n) {
		const ch = src[i];
		const next = src[i + 1];
		if (ch === "/" && next === "/") {
			while (i < n && src[i] !== "\n") {
				out += " ";
				i++;
			}
			continue;
		}
		if (ch === "/" && next === "*") {
			out += "  ";
			i += 2;
			while (i < n && !(src[i] === "*" && src[i + 1] === "/")) {
				out += src[i] === "\n" ? "\n" : " ";
				i++;
			}
			if (i < n) {
				out += "  ";
				i += 2;
			}
			continue;
		}
		if (ch === '"' || ch === "'" || ch === "`") {
			const quote = ch;
			out += ch;
			i++;
			while (i < n) {
				const c = src[i];
				if (quote !== "`" && c === "\\") {
					out += c + (src[i + 1] ?? "");
					i += 2;
					continue;
				}
				out += c;
				i++;
				if (c === quote) break;
			}
			continue;
		}
		out += ch;
		i++;
	}
	return out;
}

export function scanText(text) {
	const findings = [];
	const lines = text.split("\n");
	for (let idx = 0; idx < lines.length; idx++) {
		const line = lines[idx];
		for (const rule of RULES) {
			if (rule.re.test(line)) {
				findings.push({ line: idx + 1, rule: rule.id, hint: rule.hint, text: line.trim() });
			}
		}
	}
	findings.push(...scanAdminErrLiterals(text));
	return findings.sort((a, b) => a.line - b.line);
}

export function shouldScan(relPath) {
	if (!relPath.endsWith(".go")) return false;
	if (relPath.endsWith("_test.go")) return false;
	const parts = relPath.split("/");
	return !parts.some((p) => SKIP_DIRS.has(p));
}

export function collectGoFiles(root) {
	const out = [];
	const walk = (dir, rel) => {
		let entries;
		try {
			entries = readdirSync(dir, { withFileTypes: true });
		} catch {
			return; // 读不到就当没有：下限检查会兜住「什么都没扫到」
		}
		for (const e of entries) {
			const childRel = rel ? `${rel}/${e.name}` : e.name;
			if (e.isDirectory()) {
				if (SKIP_DIRS.has(e.name)) continue;
				walk(path.join(dir, e.name), childRel);
			} else if (shouldScan(childRel)) {
				out.push(childRel);
			}
		}
	};
	walk(root, "");
	return out.sort();
}

export function check(root = ROOT) {
	const files = collectGoFiles(root);
	const findings = [];
	for (const rel of files) {
		const text = stripGoComments(readFileSync(path.join(root, rel), "utf8"));
		for (const f of scanText(text)) findings.push({ file: rel, ...f });
	}
	return { files, findings };
}

export function selfTest() {
	const cases = [];
	const t = (name, actual, expected) => cases.push({ name, actual, expected });

	t(
		"行注释被剥、行号不变",
		stripGoComments('a := 1 // writeErr(w, 500, err.Error())\nb := 2').split("\n").length,
		2,
	);
	t(
		"注释里的写法不算违规",
		scanText(stripGoComments("// 以前这里是 writeErr(w, 500, err.Error())\nfoo()")).length,
		0,
	);
	t(
		"块注释被剥",
		scanText(stripGoComments("/*\nwriteErr(w, 500, err.Error())\n*/\nfoo()")).length,
		0,
	);
	t("字符串里的 http:// 不被当注释", stripGoComments('u := "http://x"').includes("http://x"), true);

	const bad = [
		'\t\twriteErr(w, 500, err.Error())',
		'\t\twriteErr(w, 500, err.Error())',
		'\t\twriteErr(w, 401, err.Error())',
		'\t\treturn &adminErr{500, err.Error()}',
	].join("\n");
	const badFindings = scanText(bad);
	t("三种形态都判红", badFindings.length, 4);
	t(
		"规则码分别命中",
		badFindings.map((f) => f.rule),
		["RAW_ERR_IN_RESPONSE", "RAW_ERR_IN_RESPONSE", "RAW_ERR_IN_401", "ADMIN_ERR_500_NO_CAUSE"],
	);
	t("命中带行号", badFindings.map((f) => f.line), [1, 2, 3, 4]);
	t("空白容错：writeErr(w,500,err.Error())", scanText("writeErr(w,500,err.Error())").length, 1);
	t(
		"http 常量也行",
		scanText("writeErr(w, http.StatusInternalServerError, err.Error())")[0]?.rule,
		"RAW_ERR_IN_RESPONSE",
	);

	const good = [
		"\t\twriteInternal(w, err)",
		'\t\twriteErr(w, 500, internalErrMsg)',
		'\t\twriteErr(w, 500, "二维码生成失败")',
		'\t\twriteErr(w, 401, "用户名或密码错误")',
		"\t\treturn &adminErr{code: 500, msg: err.Error(), cause: err}",
		"\t\twriteAdminErr(w, err)",
	].join("\n");
	t("修好的形状判绿", scanText(good).length, 0);

	t(
		"adminErr 的 msg: err.Error() 缺 cause 判红",
		scanText("return &adminErr{code: 500, msg: err.Error()}").map((f) => f.rule),
		["ADMIN_ERR_500_NO_CAUSE"],
	);
	t(
		"adminErr 多行缺 cause 也判红",
		scanText("return &adminErr{\n\tcode: 500,\n\tmsg: err.Error(),\n}").length,
		1,
	);
	t(
		"adminErr 的 400 带 err.Error() 也判红（原文不进用户面）",
		scanText("return &adminErr{code: 400, msg: err.Error()}").map((f) => f.rule),
		["ADMIN_ERR_RAW_MSG_NO_CAUSE"],
	);
	t(
		"adminErr 带 cause 判绿",
		scanText("return &adminErr{code: 500, msg: err.Error(), cause: err}").length,
		0,
	);

	const envelopes = [
		'\t\twriteStatus(w, http.StatusInternalServerError, reasonInternal, err.Error(), nil)',
		'\t\twriteAPIStatus(w, 500, reasonInternal, err.Error())',
	].join("\n");
	const envelopeFindings = scanText(envelopes);
	t("信封 500 两种写法都判红", envelopeFindings.map((f) => f.rule), [
		"RAW_ERR_IN_STATUS500",
		"RAW_ERR_IN_APISTATUS500",
	]);
	t(
		"信封 400/409/502 不算违规（各自有契约口径）",
		scanText(
			[
				'writeStatus(w, http.StatusBadRequest, reasonBadRequest, err.Error(), nil)',
				'writeStatus(w, http.StatusConflict, "already_installed", err.Error(), nil)',
				'writeStatus(w, http.StatusBadGateway, reasonPollFailed, err.Error(), nil)',
				'writeAPIStatus(w, http.StatusUnauthorized, reasonUnauthorized, err.Error())',
			].join("\n"),
		).length,
		0,
	);
	t(
		"信封 500 回固定文案判绿",
		scanText(
			[
				'writeStatus(w, http.StatusInternalServerError, reasonInternal, internalErrMsg, nil)',
				'writeAPIStatus(w, http.StatusInternalServerError, reasonInternal, InternalErrorMessage)',
				'writeAPIStatus(w, http.StatusInternalServerError, reasonInternal, "event ingress is not configured")',
			].join("\n"),
		).length,
		0,
	);

	t("排除 _test.go", shouldScan("internal/server/handlers_test.go"), false);
	t("排除非 Go", shouldScan("web/partials.js"), false);
	t("保留普通 Go 文件", shouldScan("internal/server/handlers.go"), true);
	t("排除第三方副本", shouldScan("node_modules/x.go"), false);

	t("有命中 → exit 1", badFindings.length > 0 ? 1 : 0, 1);
	t("无命中 → exit 0", scanText(good).length > 0 ? 1 : 0, 0);

	const live = check(ROOT);
	t("扫描面够大（下限 " + MIN_GO_FILES + "）", live.files.length >= MIN_GO_FILES, true);

	const realFile = live.files.find((f) => f === "internal/server/handlers.go") ?? live.files[0];
	const realText = realFile ? readFileSync(path.join(ROOT, realFile), "utf8") : "";
	const mutated = scanText(`writeErr(w, 500, err.Error())\n${stripGoComments(realText)}`);
	t("真实文件插一行坏样例 → 恰好报 1 条", mutated.length, 1);
	t("报的是插入的那一行", mutated[0]?.line, 1);

	let failed = 0;
	for (const c of cases) {
		const same = JSON.stringify(c.actual) === JSON.stringify(c.expected);
		if (same) {
			console.log(`  ✅ ${c.name}`);
		} else {
			failed++;
			console.error(
				`  ❌ ${c.name}\n     actual  = ${JSON.stringify(c.actual)}\n     expected= ${JSON.stringify(c.expected)}`,
			);
		}
	}
	console.log(
		failed === 0
			? `✅ self-test ${cases.length}/${cases.length} 通过`
			: `✗ self-test ${cases.length - failed}/${cases.length} 通过`,
	);
	return failed === 0;
}

function main() {
	const { files, findings } = check(ROOT);
	if (files.length < MIN_GO_FILES) {
		console.error(
			`✗ 扫描面异常：只扫到 ${files.length} 个 Go 文件（下限 ${MIN_GO_FILES}）—— 目录读错或过滤写坏，不许当成通过`,
		);
		return 2;
	}
	if (findings.length > 0) {
		console.error(`✗ 发现 ${findings.length} 处 500/401/adminErr 直接回内部原文（#624）：`);
		for (const f of findings) {
			console.error(`  ${f.file}:${f.line}  [${f.rule}]  ${f.text}`);
			console.error(`     → ${f.hint}`);
		}
		return 1;
	}
	console.log(
		`✅ 扫过 ${files.length} 个 Go 文件：500/401、信封 500 与 adminErr 构造都没有直接回内部原文（#624；400/403/409/502 的契约用户面文案与 502 的刻意回传不在本守卫范围）`,
	);
	return 0;
}

const isDirectRun = runsAsMain(import.meta.url);
if (isDirectRun) {
	if (process.argv.includes("--self-test")) {
		process.exit(selfTest() ? 0 : 1);
	} else {
		process.exit(main());
	}
}
