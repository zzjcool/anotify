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
		id: "RAW_ADMIN_ERR_500",
		re: /adminErr\s*\{\s*500\s*,\s*err\s*\.\s*Error\s*\(\s*\)\s*\}/,
		hint: "adminErr 的 500 要带 cause：&adminErr{code: 500, msg: …, cause: err}，由 writeAdminErr 统一脱敏",
	},
];

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
	return findings;
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
		["RAW_ERR_IN_RESPONSE", "RAW_ERR_IN_RESPONSE", "RAW_ERR_IN_401", "RAW_ADMIN_ERR_500"],
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

	t("排除 _test.go", shouldScan("internal/server/handlers_test.go"), false);
	t("排除非 Go", shouldScan("web/partials.js"), false);
	t("保留普通 Go 文件", shouldScan("internal/server/handlers.go"), true);
	t("排除第三方副本", shouldScan("node_modules/x.go"), false);

	t("有命中 → exit 1", badFindings.length > 0 ? 1 : 0, 1);
	t("无命中 → exit 0", scanText(good).length > 0 ? 1 : 0, 0);

	const live = check(ROOT);
	t("扫描面够大（下限 " + MIN_GO_FILES + "）", live.files.length >= MIN_GO_FILES, true);

t("仓库当前无漏原文的 500/401", live.findings.length, 0);

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
		console.error(`✗ 发现 ${findings.length} 处 500/401 直接回 err.Error()（内部原文不该进响应体，#624）：`);
		for (const f of findings) {
			console.error(`  ${f.file}:${f.line}  [${f.rule}]  ${f.text}`);
			console.error(`     → ${f.hint}`);
		}
		return 1;
	}
	console.log(`✅ 扫过 ${files.length} 个 Go 文件：500/401 都没有直接回 err.Error()（#624）`);
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
