#!/usr/bin/env node
/**
 * Anotify · 错误响应脱敏（#624：500/401 不回 err.Error()）
 *
 * 拦的事故：500 / 401 的响应体直接回 `err.Error()`。库层原文里带表名、列名、
 * 约束名与部署路径（`sql: database is closed`、`UNIQUE constraint failed:
 * users.username`、`/var/lib/…/anotify.db`），任何一个能触发 5xx 的调用方都能
 * 免费拿到库结构与文件布局；而排障真正需要的原文进日志就够了。
 *
 * 判据（只认下面三种单行形状，注释里的同形文本不算 —— 先剥注释再匹配）：
 *   RAW_ERR_IN_RESPONSE   `writeErr(w, 500, err.Error())`
 *   RAW_ERR_IN_401        `writeErr(w, 401, err.Error())`
 *   RAW_ADMIN_ERR_500     `adminErr{500, err.Error()}`
 * 右值换成固定文案、或整体改走 `writeInternal(w, err)` 之后即不再命中。
 *
 * 诚实边界：换变量名（`cause.Error()`）、跨行写法、`fmt.Sprintf` 拼出来的
 * 响应体都扫不到。本脚本是**棘轮**（只堵这三种已发生的形态），不是「所有
 * 响应体都安全」的证明；新增 500 出口请走 `writeInternal`（它内部只回固定文案）。
 *
 * 用法：
 *   node scripts/check-no-raw-errors.mjs             # 体检（make check-no-raw-errors）
 *   node scripts/check-no-raw-errors.mjs --self-test # 自测：坏样例必须判红、好样例必须判绿
 *
 * @guard scripts/check-no-raw-errors.mjs#RAW_ERR_IN_RESPONSE
 * @guard scripts/check-no-raw-errors.mjs#RAW_ERR_IN_401
 * @guard scripts/check-no-raw-errors.mjs#RAW_ADMIN_ERR_500
 */
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { runsAsMain } from "./lib/direct-run.mjs";

export const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

/** 扫描面下限：目录读错 / 过滤写坏时一个文件都扫不到 —— 那不许长得像「没问题」。 */
export const MIN_GO_FILES = 50;

/** 不参与扫描的目录名（第三方副本、工具产物、版本库元数据）。 */
const SKIP_DIRS = new Set(["node_modules", "vendor", ".git", "dist", "testdata"]);

/** 三种漏原文的形态。空白容错：`writeErr(w,500,err.Error())` 也命中。 */
export const RULES = [
	{
		id: "RAW_ERR_IN_RESPONSE",
		// `writeErr(w, 500, err.Error())` —— 500 回库层原文。
		re: /writeErr\s*\(\s*w\s*,\s*(?:500|http\.StatusInternalServerError)\s*,\s*err\s*\.\s*Error\s*\(\s*\)\s*\)/,
		hint: "500 的错误响应不许回 err.Error()：原文进日志（logInternal），响应只回 internalErrMsg",
	},
	{
		id: "RAW_ERR_IN_401",
		// `writeErr(w, 401, err.Error())` —— 认证失败回服务端原文（可被用来枚举用户）。
		re: /writeErr\s*\(\s*w\s*,\s*(?:401|http\.StatusUnauthorized)\s*,\s*err\s*\.\s*Error\s*\(\s*\)\s*\)/,
		hint: "401 不许回 err.Error()：原文进日志，响应回固定文案（口径见 #618）",
	},
	{
		id: "RAW_ADMIN_ERR_500",
		// `adminErr{500, err.Error()}` —— 域内错误结构体把原文带到 dispatch 点。
		re: /adminErr\s*\{\s*500\s*,\s*err\s*\.\s*Error\s*\(\s*\)\s*\}/,
		hint: "adminErr 的 500 要带 cause：&adminErr{code: 500, msg: …, cause: err}，由 writeAdminErr 统一脱敏",
	},
];

/**
 * 剥掉 Go 源码里的注释（保留行号：注释字符换成空格，换行不动）。
 *
 * 为什么必须剥：注释里写「以前这里是 writeErr(w, 500, err.Error())」是**说明**，
 * 不是调用点；把它算成违规会逼人删掉有价值的历史注释。
 * 字符串 / 原始串里的 `//` 不是注释（`"http://x"`），所以按状态机走。
 */
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

/** 扫描一段（已剥注释的）源码文本，返回 findings（行号从 1 起）。 */
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

/** 这个文件是否属于扫描面（非测试 Go 文件）。 */
export function shouldScan(relPath) {
	if (!relPath.endsWith(".go")) return false;
	if (relPath.endsWith("_test.go")) return false;
	const parts = relPath.split("/");
	return !parts.some((p) => SKIP_DIRS.has(p));
}

/** 递归收集扫描面里的 Go 文件（相对 ROOT 的路径，排序后稳定）。 */
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

/** 体检：返回 {files, findings}。 */
export function check(root = ROOT) {
	const files = collectGoFiles(root);
	const findings = [];
	for (const rel of files) {
		const text = stripGoComments(readFileSync(path.join(root, rel), "utf8"));
		for (const f of scanText(text)) findings.push({ file: rel, ...f });
	}
	return { files, findings };
}

/** 自测：证明「坏样例判红、好样例判绿」两侧都成立（恒绿的自测等于没有自测）。 */
export function selfTest() {
	const cases = [];
	const t = (name, actual, expected) => cases.push({ name, actual, expected });

	// ① 剥注释：注释里的同形文本不算违规，字符串里的 `//` 不许被误剥。
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

	// ② 三种形态各自判红（正例）。
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

	// ③ 修好之后的形状必须判绿（否则会逼人改回坏写法）。
	const good = [
		"\t\twriteInternal(w, err)",
		'\t\twriteErr(w, 500, internalErrMsg)',
		'\t\twriteErr(w, 500, "二维码生成失败")',
		'\t\twriteErr(w, 401, "用户名或密码错误")',
		"\t\treturn &adminErr{code: 500, msg: err.Error(), cause: err}",
		"\t\twriteAdminErr(w, err)",
	].join("\n");
	t("修好的形状判绿", scanText(good).length, 0);

	// ④ 扫描面：测试文件与非 Go 文件不在扫描范围内（否则夹具自己被判红）。
	t("排除 _test.go", shouldScan("internal/server/handlers_test.go"), false);
	t("排除非 Go", shouldScan("web/partials.js"), false);
	t("保留普通 Go 文件", shouldScan("internal/server/handlers.go"), true);
	t("排除第三方副本", shouldScan("node_modules/x.go"), false);

	// ⑤ 退出码语义：有命中就必须非 0（自测自己也要证明「判红」这件事）。
	t("有命中 → exit 1", badFindings.length > 0 ? 1 : 0, 1);
	t("无命中 → exit 0", scanText(good).length > 0 ? 1 : 0, 0);

	// ⑥ 真实仓库此刻必须干净（与主流程同口径，让自测顺带复算一次扫描面）。
	const live = check(ROOT);
	t("扫描面够大（下限 " + MIN_GO_FILES + "）", live.files.length >= MIN_GO_FILES, true);

t("仓库当前无漏原文的 500/401", live.findings.length, 0);

	// ⑦ 反恒绿：往**真实文件**的文本前面插一行坏样例，同一个扫描函数必须报出它
	// （若扫描器读不到内容 / 正则坏了，这一条先红）。
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

/** 主流程：体检整个仓库。返回进程退出码。 */
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
