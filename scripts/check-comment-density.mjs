#!/usr/bin/env node
/**
 * 注释密度 + 文档面棘轮（docs/lint-guards.md · L12 / #495）
 *
 * 口径：注释行 = 整行都属于注释（行首 `//`、`#`、`--`、`/*`、`<!--` 及其续行，含块内空行）；
 * 代码与注释同行算代码行。`<script>`/`<style>` 里的内容按 JS/CSS 计，不按 HTML 计。
 *
 * 已知边界：一行的尾注释算代码行；字符串与模板串优先于注释（`"http://x"` 里的 `//` 不算注释）。
 * JS 正则字面量按「本行内能否收尾」认：漏认带反引号的正则，其后整份文件都会被当成字符串。
 *
 * 双向棘轮：目录注释行 / 仓库总量 / 单文件密度 / docs 面文件数与行数 —— 超与降都算不一致。
 *
 * @guard scripts/check-comment-density.mjs#DIR_OVER
 * @guard scripts/check-comment-density.mjs#DENSE_UNPINNED
 * @guard scripts/check-comment-density.mjs#DOC_DIR_FORBIDDEN
 * @count node scripts/check-comment-density.mjs --report
 */
import { readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { execFileSync } from "node:child_process";

import { runsAsMain } from "./lib/direct-run.mjs";

export const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

/* BASELINE-BEGIN（`--init` / `--update` 只改这一段） */
export const BASELINE = {
	"schemaVersion": 2,
	"measuredAt": "2026-11-21",
	"commentLines": 43335,
	"codeLines": 268627,
	"files": 672,
	"dirs": {
		".": 89,
		".github/workflows": 108,
		"api": 271,
		"cmd/anode": 282,
		"cmd/anode/internal/app": 61,
		"cmd/anode/internal/applied": 27,
		"cmd/anode/internal/capability": 84,
		"cmd/anode/internal/clock": 37,
		"cmd/anode/internal/config": 77,
		"cmd/anode/internal/emitgw": 43,
		"cmd/anode/internal/execenv": 99,
		"cmd/anode/internal/hostenv": 34,
		"cmd/anode/internal/hub": 134,
		"cmd/anode/internal/install": 112,
		"cmd/anode/internal/listen": 29,
		"cmd/anode/internal/node": 70,
		"cmd/anode/internal/ops": 97,
		"cmd/anode/internal/resultstore": 394,
		"cmd/anode/internal/runner": 432,
		"cmd/anode/internal/upload": 35,
		"cmd/devseed": 249,
		"cmd/fe-preview": 32,
		"cmd/server": 18,
		"cmd/sitegen": 21,
		"examples/capabilities": 53,
		"examples/playbooks/pr-triage-notify": 15,
		"examples/playbooks/pr-triage-notify/capabilities": 21,
		"examples/playbooks/pr-triage-notify/sources": 8,
		"internal/api": 168,
		"internal/artifact": 424,
		"internal/auth": 1259,
		"internal/authn": 16,
		"internal/decisions": 2,
		"internal/lease": 36,
		"internal/logging": 22,
		"internal/playbook": 1321,
		"internal/protocol": 1061,
		"internal/push": 108,
		"internal/resultspool": 184,
		"internal/route": 299,
		"internal/schema": 73,
		"internal/server": 7765,
		"internal/server/testdata": 2,
		"internal/sitegen": 471,
		"internal/sitegen/testdata/specs": 40,
		"internal/source": 151,
		"internal/sourcespec": 294,
		"internal/store": 6821,
		"internal/store/v4test": 1197,
		"internal/trigger": 25,
		"internal/version": 48,
		"internal/ws": 669,
		"internal/yamldoc": 262,
		"scripts": 3614,
		"scripts/demo": 16,
		"scripts/e2e": 634,
		"scripts/e2e/fixtures": 0,
		"scripts/e2e/fixtures/f3": 1479,
		"scripts/e2e/lib": 766,
		"scripts/e2e/suites": 2182,
		"scripts/migrations": 20,
		"scripts/perf/2a-auth": 41,
		"scripts/perf/3a": 351,
		"scripts/perf/3b": 66,
		"scripts/perf/b": 405,
		"scripts/perf/c": 801,
		"scripts/perf/loadgen": 31,
		"scripts/testdata/fakebroker": 24,
		"web": 4674,
		"web-src": 3,
		"web-src/layouts": 98,
		"web-src/locales": 49,
		"web-src/pages": 2400,
		"web/fonts": 31
	},
	"dense": {
		"docker-compose.dev.yml": 67,
		"docker-compose.yml": 18,
		"examples/capabilities/run-in-repo.yaml": 24,
		"examples/capabilities/run-per-param-dir.yaml": 29,
		"internal/auth/apikey_cache.go": 124,
		"internal/auth/postclose_guard_test.go": 82,
		"internal/auth/session_cache.go": 116,
		"internal/playbook/playbook.go": 116,
		"internal/playbook/sensitive.go": 181,
		"internal/protocol/applied.go": 20,
		"internal/protocol/config_frames.go": 99,
		"internal/protocol/deploy_frames.go": 52,
		"internal/protocol/doc.go": 28,
		"internal/protocol/emit.go": 56,
		"internal/protocol/emit_frames.go": 91,
		"internal/protocol/env.go": 28,
		"internal/protocol/errors.go": 17,
		"internal/protocol/exec_frames.go": 127,
		"internal/protocol/hash.go": 30,
		"internal/protocol/limits.go": 13,
		"internal/protocol/ops_frames.go": 105,
		"internal/push/device_filter.go": 33,
		"internal/server/api_deps.go": 38,
		"internal/server/artifact_download.go": 69,
		"internal/server/artifact_support.go": 44,
		"internal/server/backpressure.go": 155,
		"internal/server/config.go": 305,
		"internal/server/hub_fixture_test.go": 94,
		"internal/server/mux.go": 260,
		"internal/server/node_capability.go": 117,
		"internal/server/node_upgrade_handlers.go": 35,
		"internal/server/postclose_guard_test.go": 87,
		"internal/server/ratelimit.go": 104,
		"internal/server/resource_yaml.go": 34,
		"internal/sitegen/template.go": 38,
		"internal/store/authz.go": 39,
		"internal/store/bench_writeamp_test.go": 137,
		"internal/store/identity.go": 44,
		"internal/store/ids.go": 48,
		"internal/store/page.go": 15,
		"internal/store/ref.go": 52,
		"internal/store/stmt.go": 169,
		"internal/store/store.go": 189,
		"internal/store/txexec.go": 27,
		"internal/store/v4test/doc.go": 19,
		"internal/version/version.go": 35,
		"internal/ws/rehello_clock_test.go": 22,
		"scripts/demo/chain.sh": 16,
		"scripts/dev.sh": 147,
		"scripts/e2e/fixtures/f3/f3-dialogs.mjs": 37,
		"scripts/e2e/run_all.sh": 39,
		"scripts/migrations/capability-lifecycle.sql": 20,
		"scripts/perf/3b/lib.sh": 35,
		"scripts/perf/c/s1-soak.sh": 193,
		"web-src/layouts/base.html": 67,
		"web/graphs.js": 423,
		"web/tokens.css": 126
	},
	"docs": {
		"contracts": {
			"files": 16,
			"lines": 2966
		}
	},
	"rootPlans": {
		"files": 0,
		"lines": 0
	}
};
/* BASELINE-END */

export const PER_FILE_RATIO_MAX = 0.5;
export const PER_FILE_FLOOR = 12;
export const NEW_DIR_COMMENT_MAX = 10;
/** 扫描面坏掉（路径/过滤/解析）时不允许静默通过。2026-11-21（#495 B 阶段前置 1）删掉了
 * `commentLines: 30000`：它随删注释变成「本批必须下调」的墙，而目录等值棘轮已管住这个形态。 */
export const MIN_SCANNED = { files: 600, codeLines: 150000 };

export const CODES = {
	DIR_OVER: "DIR_OVER",
	DIR_UNDER: "DIR_UNDER",
	DIR_NEW: "DIR_NEW",
	TOTAL_OVER: "TOTAL_OVER",
	TOTAL_UNDER: "TOTAL_UNDER",
	DENSE_UNPINNED: "DENSE_UNPINNED",
	DENSE_OVER: "DENSE_OVER",
	DENSE_UNDER: "DENSE_UNDER",
	DENSE_STALE: "DENSE_STALE",
	DOC_DIR_UNKNOWN: "DOC_DIR_UNKNOWN",
	DOC_DIR_FORBIDDEN: "DOC_DIR_FORBIDDEN",
	DOC_DIR_OVER: "DOC_DIR_OVER",
	DOC_DIR_UNDER: "DOC_DIR_UNDER",
	DOC_ROOT_OVER: "DOC_ROOT_OVER",
	DOC_ROOT_UNDER: "DOC_ROOT_UNDER",
	SCAN_TOO_SMALL: "SCAN_TOO_SMALL",
};

/** 只扫被跟踪的文件：工作区的 dist 产物、别人的临时目录不该进基线。 */
export const SOURCE_EXTS = [".go", ".mjs", ".js", ".py", ".sh", ".html", ".css", ".sql", ".yml", ".yaml"];
export const SOURCE_EXCLUDE = ["web/vendor/", "internal/server/dist/", "node_modules/", ".pi-subagents/"];

const LANG_BY_EXT = {
	".go": "go",
	".mjs": "js",
	".js": "js",
	".html": "html",
	".css": "css",
	".sql": "sql",
	".py": "hash",
	".sh": "hash",
	".yml": "hash",
	".yaml": "hash",
};

const LANG_TOKENS = {
	go: { line: ["//"], block: [["/*", "*/"]] },
	js: { line: ["//"], block: [["/*", "*/"]] },
	html: { line: [], block: [["<!--", "-->"]] },
	css: { line: [], block: [["/*", "*/"]] },
	sql: { line: ["--"], block: [["/*", "*/"]] },
	hash: { line: ["#"], block: [] },
};

/**
 * verdict:
 *   manual  操作手册（怎么跑/怎么部署/怎么排障）—— 活的，保留
 *   evidence 事故/交付取证快照 —— 定格记录，保留；不许当现行契约引用
 *   shrink  设计与契约类，**待收缩**：文件数与行数进基线，只许下调
 *   forbidden 已删掉的目录，禁止重建
 */
export const DOC_POLICY = [
	{ dir: "", verdict: "manual", why: "docs/README.md 等入口手册" },
	{ dir: "contracts", verdict: "shrink", why: "冻结契约：迁移进代码与测试前不许增长，只能一块块搬走" },
	{ dir: "v4", verdict: "forbidden", why: "实施期清单快照（inventory/manifest）已删（#495 B）：计数只会腐烂" },
	{ dir: "research", verdict: "forbidden", why: "可行性研究 = 计划，无执行者 ⇒ 已删（#495 B）" },
	{ dir: "perf", verdict: "forbidden", why: "一次性测量快照已删（#495 B）：要留的已并进 reports/" },
	{ dir: "reports", verdict: "evidence", why: "排障取证：按 PR 增长，不定上限" },
	{ dir: "history", verdict: "evidence", why: "更早的档案与已迁移台账" },
	{ dir: "design", verdict: "forbidden", why: "#477 的判据：实施前的计划一旦落地就只能变错" },
	{ dir: "specs", verdict: "forbidden", why: "同上（设计/规格不进 docs）" },
	{ dir: "plans", verdict: "forbidden", why: "同上（计划进 issue/PR，不进 docs）" },
];

/** 仓库根的 markdown：只有手册与会话入口算「活文档」，其余（plan/requirements/…）进 rootPlans 棘轮。 */
export const ROOT_DOC_ALLOW = ["README.md", "README.en.md", "DEVELOPMENT.md", "E2E_TESTING.md", "IOS_TESTING.md", "AGENTS.md", "CHANGELOG.md"];

export function langOf(file) {
	return LANG_BY_EXT[path.extname(file)] ?? null;
}

const ZERO_WIDTH = /[\u200b-\u200d\ufeff]/g;

/** 各语言的字符串定界符：字符串里的注释符号不算注释（`"http://x"`、`"$FIXTURE_ROOT"/*)`）。 */
const STRING_DELIMS = {
	go: ["`", '"', "'"],
	js: ["`", '"', "'"],
	css: ['"', "'"],
	sql: ["'", '"'],
	hash: ["'", '"'],
	html: [],
};

/**
 * JS 正则字面量不能跨行：`/` 只有在本行能找到未转义的收尾 `/` 时才算正则，否则是除号。
 * 漏认带反引号的正则（`/^([A-Za-z_]\w*)\s+(\S+)(?:\s+`([^`]*)`)?$/` 这种）就会把里面的
 * 反引号当成开串 —— 其后整份文件都被算成代码行（#496 的注释就是这么被吞的）。
 */
const REGEX_PREV = "([{,;:=!&|?+-*%~^<>";
function regexSpanEnd(src, start) {
	let inClass = false;
	for (let j = start + 1; j < src.length; j += 1) {
		const ch = src[j];
		if (ch === "\n") return -1;
		if (ch === "\\") j += 1;
		else if (ch === "[") inClass = true;
		else if (ch === "]") inClass = false;
		else if (ch === "/" && !inClass) return j + 1;
	}
	return -1;
}
function regexMayStart(src, start) {
	let j = start - 1;
	while (j >= 0 && (src[j] === " " || src[j] === "\t" || src[j] === "\r")) j -= 1;
	return j < 0 || src[j] === "\n" || REGEX_PREV.includes(src[j]);
}

/** HTML 里的 `<script>` / `<style>` 块按 JS / CSS 计，否则内联脚本的注释会被当成代码。 */
function countHtml(text) {
	const parts = text.split(/(<script\b[\s\S]*?<\/script\s*>|<style\b[\s\S]*?<\/style\s*>)/i);
	const acc = { comment: 0, code: 0, blank: 0 };
	for (const part of parts) {
		if (!part) continue;
		const lang = /^<script/i.test(part) ? "js" : /^<style/i.test(part) ? "css" : "html";
		const one = countTokens(part, lang);
		acc.comment += one.comment;
		acc.code += one.code;
		acc.blank += one.blank;
	}
	return acc;
}

/**
 * 逐字符扫：先认字符串，再认注释。行内既有代码又有注释时算**代码行**；
 * 只有注释的行（含块注释里的空行）算注释行 —— 这样行中开块不会变成免费增长口。
 */
function countTokens(text, langName) {
	const tokens = LANG_TOKENS[langName];
	const delims = STRING_DELIMS[langName] ?? [];
	const src = text.replace(ZERO_WIDTH, "");
	let comment = 0;
	let code = 0;
	let blank = 0;
	let lineCode = false;
	let lineComment = false;
	let blockEnd = null;
	let quote = null;
	let raw = false;
	const flush = () => {
		// 代码优先：`echo hi # 行内` 是代码行；块内的空行仍算注释（它是那段注释的一部分）。
		if (lineCode) code++;
		else if (lineComment) comment++;
		else blank++;
		lineCode = false;
		lineComment = false;
	};
	let i = 0;
	while (i < src.length) {
		const c = src[i];
		if (c === "\n") {
			if (blockEnd) lineComment = true;
			flush();
			i++;
			if (!raw) quote = null;
			continue;
		}
		if (blockEnd) {
			lineComment = true;
			if (src.startsWith(blockEnd, i)) {
				i += blockEnd.length;
				blockEnd = null;
			} else i++;
			continue;
		}
		if (quote) {
			lineCode = true;
			// 反斜杠转义只对非原始串有效：Go 的反引号是原始串（`\` 不转义），JS 的模板串认转义（`\`` 不是收尾）。
			if (c === "\\" && !(raw && langName === "go")) {
				i += 2;
				continue;
			}
			if (src.startsWith(quote, i)) {
				i += quote.length;
				quote = null;
				continue;
			}
			i++;
			continue;
		}
		if (tokens.line.some((t) => src.startsWith(t, i))) {
			lineComment = true;
			const nl = src.indexOf("\n", i);
			i = nl === -1 ? src.length : nl;
			continue;
		}
		const blk = tokens.block.find(([start]) => src.startsWith(start, i));
		if (blk) {
			lineComment = true;
			blockEnd = blk[1];
			i += blk[0].length;
			continue;
		}
		if (langName === "js" && c === "/" && regexMayStart(src, i)) {
			const end = regexSpanEnd(src, i);
			if (end !== -1) {
				lineCode = true;
				i = end;
				continue;
			}
		}
		if (delims.includes(c)) {
			lineCode = true;
			quote = c;
			raw = c === "`";
			i++;
			continue;
		}
		if (c !== " " && c !== "\t" && c !== "\r") lineCode = true;
		i++;
	}
	flush();
	return { comment, code, blank };
}

export function countLines(text, langName) {
	return langName === "html" ? countHtml(text) : countTokens(text, langName);
}

export function listTracked(patterns = ["."]) {
	const out = execFileSync("git", ["ls-files", "-z", "--", ...patterns], { cwd: ROOT, encoding: "utf8" });
	return out.split("\0").filter(Boolean);
}

export function collectSources() {
	return listTracked()
		.filter((f) => SOURCE_EXTS.includes(path.extname(f)) && !SOURCE_EXCLUDE.some((p) => f.startsWith(p)))
		.map((file) => ({
			path: file,
			dir: path.dirname(file),
			lang: langOf(file),
			text: readFileSync(path.join(ROOT, file), "utf8"),
		}));
}

export function measure(sources) {
	const dirs = new Map();
	const files = [];
	let comment = 0;
	let code = 0;
	for (const s of sources) {
		const c = countLines(s.text, s.lang);
		files.push({ path: s.path, dir: s.dir, comment: c.comment, code: c.code });
		dirs.set(s.dir, (dirs.get(s.dir) ?? 0) + c.comment);
		comment += c.comment;
		code += c.code;
	}
	return { files, dirs: Object.fromEntries([...dirs].sort()), total: { files: files.length, comment, code } };
}

function mdLines(file) {
	return readFileSync(path.join(ROOT, file), "utf8").split("\n").filter((l) => l.trim()).length;
}

export function docSurface() {
	const out = { total: 0, lines: 0, dirs: {}, rootPlans: { files: 0, lines: 0 } };
	for (const f of listTracked(["docs/"])) {
		if (path.extname(f) !== ".md") continue;
		const rel = path.relative("docs", f).split(path.sep);
		const dir = rel.length === 1 ? "" : rel[0];
		const slot = (out.dirs[dir] ??= { files: 0, lines: 0 });
		const n = mdLines(f);
		slot.files++;
		slot.lines += n;
		out.total++;
		out.lines += n;
	}
	for (const f of listTracked()) {
		if (f.includes("/") || path.extname(f) !== ".md" || ROOT_DOC_ALLOW.includes(f)) continue;
		out.rootPlans.files++;
		out.rootPlans.lines += mdLines(f);
	}
	return out;
}

export function denseAllowance(codeLines, opts = {}) {
	return Math.max(opts.floor ?? PER_FILE_FLOOR, Math.floor(codeLines * (opts.ratioMax ?? PER_FILE_RATIO_MAX)));
}

function cmpCounts(label, actual, cap, push) {
	if (actual.files > cap.files) push(CODES.DOC_DIR_OVER, `${label}: 文件数 ${actual.files} > 基线 ${cap.files}`);
	if (actual.files < cap.files) push(CODES.DOC_DIR_UNDER, `${label}: 文件数 ${actual.files} < 基线 ${cap.files} —— 把基线降到 ${actual.files}`);
	if (actual.lines > cap.lines) push(CODES.DOC_DIR_OVER, `${label}: 行数 ${actual.lines} > 基线 ${cap.lines}`);
	if (actual.lines < cap.lines) push(CODES.DOC_DIR_UNDER, `${label}: 行数 ${actual.lines} < 基线 ${cap.lines} —— 把基线降到 ${actual.lines}`);
}

export function evaluate({ measured, docs }, baseline, opts = {}) {
	const issues = [];
	const push = (kind, message) => issues.push({ kind, message });
	const baseDirs = baseline.dirs ?? {};
	const newDirMax = opts.newDirMax ?? NEW_DIR_COMMENT_MAX;

	for (const [dir, count] of Object.entries(measured.dirs)) {
		const cap = baseDirs[dir];
		if (cap === undefined) {
			if (count > newDirMax) push(CODES.DIR_NEW, `${dir}: 新目录 ${count} 行注释 > 允许 ${newDirMax} —— 删注释；确需保留就登记进基线`);
			continue;
		}
		if (count > cap) push(CODES.DIR_OVER, `${dir}: ${count} > 基线 ${cap}（超 ${count - cap} 行）`);
		if (count < cap) push(CODES.DIR_UNDER, `${dir}: ${count} < 基线 ${cap} —— 已收紧，把基线降到 ${count}`);
	}
	for (const dir of Object.keys(baseDirs)) {
		if (measured.dirs[dir] === undefined) push(CODES.DIR_UNDER, `${dir}: 目录已不存在/已清空，基线里还留着 ${baseDirs[dir]} —— 删掉这条`);
	}

	const totalCap = baseline.commentLines;
	if (typeof totalCap === "number") {
		if (measured.total.comment > totalCap) push(CODES.TOTAL_OVER, `仓库总注释行 ${measured.total.comment} > 基线 ${totalCap}`);
		if (measured.total.comment < totalCap)
			push(CODES.TOTAL_UNDER, `仓库总注释行 ${measured.total.comment} < 基线 ${totalCap} —— 把基线降到 ${measured.total.comment}`);
	}

	const pins = baseline.dense ?? {};
	const seen = new Set();
	for (const f of measured.files) {
		const allowance = denseAllowance(f.code, opts);
		const pinned = pins[f.path];
		if (pinned !== undefined) {
			seen.add(f.path);
			if (f.comment > pinned) push(CODES.DENSE_OVER, `${f.path}: ${f.comment} 行注释 > 登记预算 ${pinned}`);
			if (f.comment < pinned) push(CODES.DENSE_UNDER, `${f.path}: ${f.comment} < 登记预算 ${pinned} —— 把预算降到 ${f.comment}`);
			else if (f.comment <= allowance)
				// 预算与阈值同时收紧后这条登记就没有理由了：豁免不许自己长出来。
				push(CODES.DENSE_STALE, `${f.path}: ${f.comment} 行 ≤ 阈值 ${allowance} —— 删掉密集文件登记表里的这条`);
			continue;
		}
		if (f.comment > allowance)
			push(CODES.DENSE_UNPINNED, `${f.path}: ${f.comment} 行注释 / ${f.code} 行代码，超阈值 ${allowance} 且未登记`);
	}
	for (const p of Object.keys(pins)) {
		if (!seen.has(p)) push(CODES.DENSE_STALE, `${p}: 已不存在或不超阈值 —— 删掉这条登记`);
	}

	const scan = opts.minScanned ?? MIN_SCANNED;
	if (measured.total.files < scan.files)
		push(CODES.SCAN_TOO_SMALL, `只扫到 ${measured.total.files} 个文件（下限 ${scan.files}）—— 扫描面坏了`);
	if (measured.total.code < scan.codeLines)
		push(CODES.SCAN_TOO_SMALL, `只扫到 ${measured.total.code} 行代码（下限 ${scan.codeLines}）`);

	const policy = new Map(DOC_POLICY.map((d) => [d.dir, d]));
	const docsDirs = docs?.dirs ?? {};
	const docsCaps = baseline.docs ?? {};
	for (const dir of Object.keys(docsDirs)) {
		const rule = policy.get(dir);
		if (!rule) push(CODES.DOC_DIR_UNKNOWN, `docs/${dir}/: 不在 docs 面判据里（design/specs/plans 一律禁止重建，见 docs/lint-guards.md L12）`);
		if (rule?.verdict === "forbidden") push(CODES.DOC_DIR_FORBIDDEN, `docs/${dir}/: 已判删的目录又出现了（${rule.why}）`);
	}
	for (const cap of Object.keys(docsCaps)) {
		if (!policy.has(cap)) push(CODES.DOC_DIR_UNKNOWN, `基线里有 docs/${cap || "(根)"}，但判据表里没有`);
	}
	for (const rule of DOC_POLICY) {
		if (rule.verdict !== "shrink") continue;
		const count = docsDirs[rule.dir] ?? { files: 0, lines: 0 };
		if (!docsCaps[rule.dir]) {
			if (count.files > 0) push(CODES.DOC_DIR_UNKNOWN, `docs/${rule.dir}/: 待收缩目录没登记基线（${count.files} 份）`);
			continue;
		}
		cmpCounts(`docs/${rule.dir}/`, count, docsCaps[rule.dir], push);
	}
	for (const rule of DOC_POLICY) {
		if (rule.verdict !== "evidence") continue;
		if (docsDirs[rule.dir] === undefined)
			push(CODES.DOC_DIR_UNKNOWN, `docs/${rule.dir}/: 判据表登记的取证目录不存在了 —— 删掉这条路`);
	}
	if (baseline.rootPlans) {
		const actual = docs?.rootPlans ?? { files: 0, lines: 0 };
		const cap = baseline.rootPlans;
		if (actual.files > cap.files) push(CODES.DOC_ROOT_OVER, `仓库根的 plan/requirements 类 md: ${actual.files} 份 > 基线 ${cap.files}`);
		if (actual.files < cap.files) push(CODES.DOC_ROOT_UNDER, `仓库根的 plan/requirements 类 md: ${actual.files} < 基线 ${cap.files} —— 把基线降到 ${actual.files}`);
		if (actual.lines > cap.lines) push(CODES.DOC_ROOT_OVER, `仓库根的 plan/requirements 类 md: 行数 ${actual.lines} > 基线 ${cap.lines}`);
		if (actual.lines < cap.lines) push(CODES.DOC_ROOT_UNDER, `仓库根的 plan/requirements 类 md: 行数 ${actual.lines} < 基线 ${cap.lines} —— 把基线降到 ${actual.lines}`);
	}

	return { issues };
}

export function nextBaseline(measured, docs, baseline, today, options = {}) {
	const seedDense = options.seedDense === true;
	// 首次建基线时全仓目录都是「新目录」，新目录上限不适用；此后新目录才受它约束。
	const newDirMax = seedDense ? Infinity : NEW_DIR_COMMENT_MAX;
	const refusals = [];
	const changes = [];
	const baseDirs = baseline.dirs ?? {};
	const dirs = {};
	for (const [dir, count] of Object.entries(measured.dirs)) {
		const cap = baseDirs[dir];
		if (cap === undefined) {
			if (count > newDirMax) {
				refusals.push(`${dir}: 新目录 ${count} 行注释 > 允许 ${newDirMax} —— 新目录不许自动入表`);
				continue;
			}
			dirs[dir] = count;
			changes.push(`+ ${dir}: ${count}`);
			continue;
		}
		if (count > cap) {
			refusals.push(`${dir}: 实测 ${count} > 基线 ${cap} —— 删注释，别抬基线`);
			dirs[dir] = cap;
			continue;
		}
		if (count < cap) changes.push(`↓ ${dir}: ${cap} → ${count}`);
		dirs[dir] = count;
	}
	const pins = {};
	for (const f of measured.files) {
		const allowance = denseAllowance(f.code);
		const pinned = (baseline.dense ?? {})[f.path];
		if (pinned === undefined) {
			if (seedDense && f.comment > allowance) {
				pins[f.path] = f.comment;
				changes.push(`+ ${f.path}: ${f.comment}（首次建基线）`);
				continue;
			}
			if (f.comment > allowance) refusals.push(`${f.path}: 密集文件不许自动入表（${f.comment} 行 > 阈值 ${allowance}）—— 删注释或手工登记`);
			continue;
		}
		if (f.comment > pinned) {
			refusals.push(`${f.path}: ${f.comment} > 预算 ${pinned} —— 删注释，别抬预算`);
			pins[f.path] = pinned;
			continue;
		}
		if (f.comment <= allowance) {
			changes.push(`− ${f.path}: 已不超阈值 ${allowance}，登记作废`);
			continue;
		}
		if (f.comment < pinned) changes.push(`↓ ${f.path}: ${pinned} → ${f.comment}`);
		pins[f.path] = f.comment;
	}
	const docsCaps = {};
	for (const rule of DOC_POLICY) {
		if (rule.verdict !== "shrink") continue;
		const actual = docs.dirs[rule.dir] ?? { files: 0, lines: 0 };
		const cap = (baseline.docs ?? {})[rule.dir];
		if (cap && (actual.files > cap.files || actual.lines > cap.lines)) {
			refusals.push(`docs/${rule.dir}/: ${actual.files} 份 / ${actual.lines} 行 高于基线 ${cap.files} 份 / ${cap.lines} 行 —— 删文档，别抬基线`);
			docsCaps[rule.dir] = cap;
			continue;
		}
		if (cap && (actual.files < cap.files || actual.lines < cap.lines))
			changes.push(`↓ docs/${rule.dir}/: ${cap.files} 份 / ${cap.lines} 行 → ${actual.files} 份 / ${actual.lines} 行`);
		docsCaps[rule.dir] = { files: actual.files, lines: actual.lines };
	}
	let rootPlans = baseline.rootPlans;
	if (seedDense || rootPlans) {
		const actual = docs.rootPlans;
		if (rootPlans && (actual.files > rootPlans.files || actual.lines > rootPlans.lines)) {
			refusals.push(`仓库根 plan 类 md: ${actual.files} 份 / ${actual.lines} 行 高于基线 —— 删文档，别抬基线`);
			rootPlans = rootPlans;
		} else if (rootPlans && (actual.files < rootPlans.files || actual.lines < rootPlans.lines)) {
			changes.push(`↓ 仓库根 plan 类 md: ${rootPlans.files} 份 / ${rootPlans.lines} 行 → ${actual.files} 份 / ${actual.lines} 行`);
			rootPlans = { files: actual.files, lines: actual.lines };
		} else if (!rootPlans) {
			rootPlans = { files: actual.files, lines: actual.lines };
			changes.push(`+ 仓库根 plan 类 md: ${actual.files} 份 / ${actual.lines} 行（首次建基线）`);
		}
	}
	const baselineOut = {
		schemaVersion: 2,
		measuredAt: today,
		commentLines: Math.min(baseline.commentLines ?? Infinity, measured.total.comment),
		codeLines: measured.total.code,
		files: measured.total.files,
		dirs,
		dense: pins,
		docs: docsCaps,
		rootPlans,
	};
	if (measured.total.comment > (baseline.commentLines ?? Infinity))
		refusals.push(`仓库总注释行 ${measured.total.comment} > 基线 ${baseline.commentLines} —— 删注释`);
	return { baseline: baselineOut, refusals, changes };
}

const DETAIL = {
	[CODES.DIR_OVER]: "注释在往回长：删掉能自述的那些（复述函数名、解释显然的 if、历史流水账）。",
	[CODES.DIR_UNDER]: "已收紧：同一 PR 里跑 `node scripts/check-comment-density.mjs --update` 把基线下调到实测值（只降不升）。",
	[CODES.DIR_NEW]: "新目录自带注释预算只有 NEW_DIR_COMMENT_MAX 行；要更多就得显式改基线（review 可见）。",
	[CODES.TOTAL_OVER]: "总量在往回长 —— 基线的意义就是不许它长。",
	[CODES.TOTAL_UNDER]: "总量已收紧：把基线降到实测值。",
	[CODES.DENSE_UNPINNED]: "单文件注释密度超阈值且没登记。先删注释；确属「代码无法自述」的那些，再手工登记进基线。",
	[CODES.DENSE_OVER]: "登记过预算的文件又长了。",
	[CODES.DENSE_UNDER]: "登记过预算的文件已收紧：把预算降到实测值。",
	[CODES.DENSE_STALE]: "登记表不许过期 —— 过期的豁免就是下一次增长的口子。",
	[CODES.DOC_DIR_UNKNOWN]: "docs 面判据见 docs/lint-guards.md 的 L12 行：设计与契约定居代码与测试里。",
	[CODES.DOC_DIR_FORBIDDEN]: "这个目录被判过删：计划落地后只剩错误，重建即红。",
	[CODES.DOC_DIR_OVER]: "待收缩目录长回去了（文件数与行数都算）。",
	[CODES.DOC_DIR_UNDER]: "已删掉一批：把基线降到实测值。",
	[CODES.DOC_ROOT_OVER]: "仓库根的 plan/requirements 类 md 在长：计划进 issue/PR，不进仓库。",
	[CODES.DOC_ROOT_UNDER]: "仓库根的 plan 类 md 已删：把基线降到实测值。",
	[CODES.SCAN_TOO_SMALL]: "先修扫描面 —— 它现在什么都看不见，绿是假的。",
};

function pad(s, n) {
	return String(s).padEnd(n);
}

function report(measured, baseline, docs) {
	console.log("🔍 注释密度盘点（只盘点，不判红）");
	console.log(`   文件 ${measured.total.files} / 注释行 ${measured.total.comment} / 代码行 ${measured.total.code}`);
	console.log("");
	console.log(`   ${pad("目录", 34)} ${pad("注释行", 8)} 基线`);
	for (const [dir, count] of Object.entries(measured.dirs).sort((a, b) => b[1] - a[1])) {
		const cap = baseline.dirs?.[dir];
		console.log(`   ${pad(dir, 34)} ${pad(count, 8)} ${cap === undefined ? "（新）" : cap}`);
	}
	console.log("");
	console.log("   注释行最多的 15 个文件（B 阶段按这个顺序删）：");
	for (const f of [...measured.files].sort((a, b) => b.comment - a.comment).slice(0, 15))
		console.log(`   ${pad(f.path, 56)} ${pad(f.comment, 6)} / ${f.code} 行代码  阈值 ${denseAllowance(f.code)}`);
	console.log("");
	console.log(
		"   文件数分布（注释行 ≥ N）：" + [20, 50, 100, 200].map((n) => `${n}:${measured.files.filter((f) => f.comment >= n).length}`).join("  "),
	);
	console.log(`   超密度阈值 ${PER_FILE_RATIO_MAX} 的文件：${measured.files.filter((f) => f.comment > denseAllowance(f.code)).length}（已登记 ${Object.keys(baseline.dense ?? {}).length}）`);
	console.log("");
	console.log("   docs 面：");
	for (const rule of DOC_POLICY) {
		const c = rule.dir === "" ? { files: docs.dirs[""]?.files ?? 0, lines: docs.dirs[""]?.lines ?? 0 } : (docs.dirs[rule.dir] ?? { files: 0, lines: 0 });
		const cap = baseline.docs?.[rule.dir];
		const capText = cap ? `基线 ${cap.files} 份 / ${cap.lines} 行` : "";
		console.log(`   ${pad(`docs/${rule.dir || "(根)"}`, 20)} ${pad(rule.verdict, 10)} ${pad(`${c.files} 份`, 8)} ${pad(`${c.lines} 行`, 10)} ${capText}   ${rule.why}`);
	}
	console.log(`   仓库根 plan 类 md：${docs.rootPlans.files} 份 / ${docs.rootPlans.lines} 行（基线 ${baseline.rootPlans ? `${baseline.rootPlans.files} 份 / ${baseline.rootPlans.lines} 行` : "未登记"}）`);
	console.log(`   docs/ 合计 ${docs.total} 份 / ${docs.lines} 行`);
	return 0;
}

function runCheck() {
	const measured = measure(collectSources());
	const docs = docSurface();
	if (!BASELINE.commentLines) {
		console.error("❌ BASELINE 未初始化（先跑 --init）");
		return 2;
	}
	const baseline = BASELINE;
	const { issues } = evaluate({ measured, docs }, baseline);

	console.log("🔍 注释密度棘轮（docs/lint-guards.md · L12）");
	console.log(`   扫描 ${measured.total.files} 个文件（下限 ${MIN_SCANNED.files}）· 注释行 ${measured.total.comment} / 基线 ${baseline.commentLines}`);
	console.log(`   docs 面 ${docs.total} 份 / ${docs.lines} 行 · 待收缩 ${Object.entries(baseline.docs ?? {}).map(([k, v]) => `${k}=${v.files}份/${v.lines}行`).join(" ")}`);
	console.log(`   目录：${Object.keys(baseline.dirs ?? {}).length} 个有基线，${Object.entries(measured.dirs).filter(([d, c]) => baseline.dirs?.[d] !== undefined && c !== baseline.dirs[d]).length} 个与基线不一致`);
	console.log("");

	if (issues.length === 0) {
		console.log("✅ 与基线完全一致（超与降都算不一致）—— 有收紧请同批下调基线，新增必须在此刻被拦住");
		return 0;
	}
	const byKind = new Map();
	for (const it of issues) {
		if (!byKind.has(it.kind)) byKind.set(it.kind, []);
		byKind.get(it.kind).push(it);
	}
	console.error(`❌ 注释/文档密度棘轮不通过（${issues.length} 项）：`);
	for (const [kind, list] of byKind) {
		console.error("");
		console.error(`  · [${kind}] ${DETAIL[kind] ?? ""}`);
		for (const it of list.slice(0, 20)) console.error(`    - ${it.message}`);
		if (list.length > 20) console.error(`    … 其余 ${list.length - 20} 项同类（共 ${list.length}）`);
	}
	return 1;
}

/** 只改本文件里标记的那一段：基线跟着判据走，不散成第二个文件。 */
function writeBaseline(next) {
	const self = fileURLToPath(import.meta.url);
	const text = readFileSync(self, "utf8");
	const re = /\/\* BASELINE-BEGIN[^\n]*\*\/[\s\S]*?\/\* BASELINE-END \*\//;
	if (!re.test(text)) throw new Error("BASELINE 标记块找不到");
	const body = `/* BASELINE-BEGIN（\`--init\` / \`--update\` 只改这一段） */\nexport const BASELINE = ${JSON.stringify(next, null, "\t")};\n/* BASELINE-END */`;
	writeFileSync(self, text.replace(re, () => body));
}

function runInit() {
	if (BASELINE.commentLines) {
		console.error("❌ BASELINE 已初始化 —— 只许用 --update 收紧，不许重建");
		return 1;
	}
	const measured = measure(collectSources());
	const docs = docSurface();
	const today = new Date().toISOString().slice(0, 10);
	const { baseline: next, refusals, changes } = nextBaseline(measured, docs, { dirs: {}, dense: {}, docs: {} }, today, { seedDense: true });
	for (const c of changes) console.log(`  ${c}`);
	if (refusals.length) {
		console.error(`❌ 建基线被拒（${refusals.length} 项）`);
		for (const r of refusals) console.error(`    - ${r}`);
		return 1;
	}
	writeBaseline(next);
	console.log(`✅ 基线已写入本文件的 BASELINE 块：注释行 ${next.commentLines}，目录 ${Object.keys(next.dirs).length} 个，密集文件 ${Object.keys(next.dense).length} 个`);
	return 0;
}

function runUpdate() {
	const measured = measure(collectSources());
	const docs = docSurface();
	const baseline = BASELINE.commentLines ? BASELINE : { dirs: {}, dense: {}, docs: {} };
	const today = new Date().toISOString().slice(0, 10);
	const { baseline: next, refusals, changes } = nextBaseline(measured, docs, baseline, today);
	for (const c of changes) console.log(`  ${c}`);
	if (refusals.length) {
		console.error(`❌ 拒绝收紧基线（${refusals.length} 项）—— 基线的方向只有一个：往下。`);
		for (const r of refusals) console.error(`    - ${r}`);
		return 1;
	}
	writeBaseline(next);
	console.log(`✅ 基线已收紧（注释行 ${next.commentLines}，目录 ${Object.keys(next.dirs).length} 个，密集文件 ${Object.keys(next.dense).length} 个）`);
	return 0;
}

const FIXTURE_DOCS = {
	total: 20,
	lines: 900,
	dirs: {
		"": { files: 2, lines: 80 },
		contracts: { files: 5, lines: 400 },
		reports: { files: 3, lines: 120 },
		history: { files: 2, lines: 90 },
	},
	rootPlans: { files: 1, lines: 40 },
};
const FIXTURE_BASELINE = {
	commentLines: 30,
	codeLines: 1000,
	files: 500,
	dirs: { "internal/a": 30 },
	dense: {},
	docs: { contracts: { files: 5, lines: 400 } },
	rootPlans: { files: 1, lines: 40 },
};
const FIXTURE_MIN = { files: 1, codeLines: 1 };

function fixtureMeasured({ dirs = {}, files = [], total } = {}) {
	const comment = Object.values(dirs).reduce((a, b) => a + b, 0);
	const code = files.reduce((a, f) => a + f.code, 0) + 200;
	return {
		dirs,
		files,
		total: total ?? { files: Math.max(1, files.length) + 500, comment, code: Math.max(code, 500) },
	};
}

export function selfTest() {
	let failed = 0;
	const check = (name, cond, detail = "") => {
		if (cond) console.log(`  ✅ ${name}`);
		else {
			failed++;
			console.error(`  ❌ ${name}${detail ? ` — ${detail}` : ""}`);
		}
	};
	const run = (measuredOver, baseOver = {}, opts = {}) =>
		evaluate(
			{ measured: fixtureMeasured(measuredOver), docs: measuredOver.docs ?? FIXTURE_DOCS },
			{ ...FIXTURE_BASELINE, ...baseOver },
			{ minScanned: FIXTURE_MIN, ...opts },
		);
	const kinds = (res) => res.issues.map((i) => i.kind);

	{
		const res = run({ dirs: { "internal/a": 31 } }, { dirs: { "internal/a": 30 }, commentLines: 31 });
		check("① 目录注释行超基线 → DIR_OVER", kinds(res).includes(CODES.DIR_OVER), JSON.stringify(kinds(res)));
	}
	{
		const res = run({ dirs: { "internal/a": 30 } }, { dirs: { "internal/a": 30 }, commentLines: 30 });
		check("② 等于基线 → 绿", res.issues.length === 0, JSON.stringify(res.issues));
	}
	{
		const res = run({ dirs: { "internal/a": 24 } }, { dirs: { "internal/a": 30 }, commentLines: 24 });
		const under = res.issues.find((i) => i.kind === CODES.DIR_UNDER);
		check("③ 低于基线 → DIR_UNDER 并提示「把基线降到 24」", !!under && under.message.includes("降到 24"), JSON.stringify(res.issues));
	}
	{
		const base = { dirs: { "internal/a": 30 }, commentLines: 100 };
		const over = run({ dirs: { "internal/a": 30 }, total: { files: 501, comment: 101, code: 500 } }, base);
		const eq = run({ dirs: { "internal/a": 30 }, total: { files: 501, comment: 100, code: 500 } }, base);
		const low = run({ dirs: { "internal/a": 30 }, total: { files: 501, comment: 99, code: 500 } }, base);
		check(
			"④ 总注释行 超/等于/低于 → TOTAL_OVER / 绿 / TOTAL_UNDER",
			kinds(over).includes(CODES.TOTAL_OVER) && eq.issues.length === 0 && kinds(low).includes(CODES.TOTAL_UNDER),
			JSON.stringify([kinds(over), kinds(eq), kinds(low)]),
		);
	}
	{
		const over = run({ dirs: { "internal/new": NEW_DIR_COMMENT_MAX + 1 } }, { dirs: {}, commentLines: NEW_DIR_COMMENT_MAX + 1 });
		const ok = run({ dirs: { "internal/new": NEW_DIR_COMMENT_MAX } }, { dirs: {}, commentLines: NEW_DIR_COMMENT_MAX });
		check("⑤ 新目录超 NEW_DIR_COMMENT_MAX → DIR_NEW", kinds(over).includes(CODES.DIR_NEW), JSON.stringify(kinds(over)));
		check("⑤' 新目录在允许内 → 绿", ok.issues.length === 0, JSON.stringify(ok.issues));
	}
	{
		const x = { path: "internal/a/x.go", dir: "internal/a", comment: 40, code: 40 };
		const b = (pins, comment) => ({ dirs: { "internal/a": comment }, commentLines: comment, dense: pins });
		const unpinned = run({ dirs: { "internal/a": 40 }, files: [x] }, b({}, 40));
		check("⑥ 密集文件未登记 → DENSE_UNPINNED", kinds(unpinned).includes(CODES.DENSE_UNPINNED), JSON.stringify(kinds(unpinned)));
		const equal = run({ dirs: { "internal/a": 40 }, files: [x] }, b({ "internal/a/x.go": 40 }, 40));
		check("⑥' 登记后等于预算 → 绿", equal.issues.length === 0, JSON.stringify(equal.issues));
		const over = run({ dirs: { "internal/a": 41 }, files: [{ ...x, comment: 41 }] }, b({ "internal/a/x.go": 40 }, 41));
		check("⑥'' 超预算 → DENSE_OVER", kinds(over).includes(CODES.DENSE_OVER), JSON.stringify(kinds(over)));
		const under = run({ dirs: { "internal/a": 39 }, files: [{ ...x, comment: 39 }] }, b({ "internal/a/x.go": 40 }, 39));
		const underIssue = under.issues.find((i) => i.kind === CODES.DENSE_UNDER);
		check("⑥''' 低于预算 → DENSE_UNDER 并提示「降到 39」", !!underIssue && underIssue.message.includes("降到 39"), JSON.stringify(under.issues));
		const stale = run({ dirs: { "internal/a": 40 }, files: [x] }, b({ "internal/a/x.go": 40, "internal/a/gone.go": 5 }, 40));
		check("⑥'''' 登记表里的文件已消失 → DENSE_STALE", kinds(stale).filter((k) => k === CODES.DENSE_STALE).length === 1, JSON.stringify(kinds(stale)));
	}
	{
		const at = { path: "internal/a/small.go", dir: "internal/a", comment: PER_FILE_FLOOR, code: 3 };
		const okRes = run({ dirs: { "internal/a": PER_FILE_FLOOR }, files: [at] }, { dirs: { "internal/a": PER_FILE_FLOOR }, commentLines: PER_FILE_FLOOR });
		const bad = { ...at, comment: PER_FILE_FLOOR + 1 };
		const badRes = run({ dirs: { "internal/a": PER_FILE_FLOOR + 1 }, files: [bad] }, { dirs: { "internal/a": PER_FILE_FLOOR + 1 }, commentLines: PER_FILE_FLOOR + 1 });
		check("⑦ 阈值边界：等于 FLOOR 绿 / +1 红", okRes.issues.length === 0 && kinds(badRes).includes(CODES.DENSE_UNPINNED), JSON.stringify([kinds(okRes), kinds(badRes)]));
	}
	{
		const go = 'var u = "http://x"\n/* 头\n\n 续 */\n*/ var y = 1\n// 注释\n';
		const c = countLines(go, "go");
		check("⑧ Go 口径：4 注释 / 2 代码（字符串里的 // 不算）", c.comment === 4 && c.code === 2, JSON.stringify(c));
		const js = "const s = `a\n// b`\n// c\n";
		const cjs = countLines(js, "js");
		check("⑧' JS 口径：字符串优先，模板串里的 // 不算注释", cjs.comment === 1 && cjs.code === 2, JSON.stringify(cjs));
		const html = "<!-- 说明 -->\n<div>\n<!--\n多行\n-->\n</div>\n";
		const chtml = countLines(html, "html");
		check("⑧'' HTML：4 注释 / 2 代码", chtml.comment === 4 && chtml.code === 2, JSON.stringify(chtml));
		const sh = "#!/bin/sh\n# 说明\necho hi # 行内\n";
		const csh = countLines(sh, "hash");
		check("⑧''' shell：2 注释 / 1 代码（行内 # 不算注释行）", csh.comment === 2 && csh.code === 1, JSON.stringify(csh));
	}
	{
		// #496 反向误判的形态：字符串里的 // 不算注释；反引号只在串外才开串。
		const url = ['const s = "https://example.com // not a comment";', "// 真注释", ""].join("\n");
		const cu = countLines(url, "js");
		check("⑧⁗ 双引号串里的 // 不算注释", cu.comment === 1 && cu.code === 1, JSON.stringify(cu));
		const trailing = "const a = 1; // 尾注释\n";
		const ct = countLines(trailing, "js");
		check("⑧⁗' 代码同行的尾注释算代码行", ct.comment === 0 && ct.code === 1, JSON.stringify(ct));
		const regex = ["const m = /^([A-Za-z_]\\w*)\\s+`([^`]*)`$/.exec(line);", "// 真注释", "const a = 1;", ""].join("\n");
		const cr = countLines(regex, "js");
		check("⑧⁗'' 正则字面量里的反引号不开串（漏认就把其后全当字符串）", cr.comment === 1 && cr.code === 2, JSON.stringify(cr));
		const escaped = ["const why = `a \\`${m[1]}\\` b`;", "// 真注释", ""].join("\n");
		const ce = countLines(escaped, "js");
		check("⑧⁗''' 模板串里的 \\` 是转义而不是收尾", ce.comment === 1 && ce.code === 1, JSON.stringify(ce));
		const goRaw = ["var s = `a\\`", "// Go 原始串里 \\ 不转义 ⇒ 上一行已收尾", ""].join("\n");
		const cg = countLines(goRaw, "go");
		check("⑧⁗'''' Go 反引号仍是原始串（反斜杠不转义）", cg.comment === 1, JSON.stringify(cg));
	}
	{
		const midline = "var x = 1 /* 开头\n * 续1\n * 续2\n */\n";
		const c = countLines(midline, "go");
		check("④' 行中开块 → 续行算注释（不留免费增长口）", c.comment === 3 && c.code === 1, JSON.stringify(c));
		const zw = "\u200b// 零宽前缀\n";
		check("④'' 零宽字符前缀也算注释", countLines(zw, "go").comment === 1, JSON.stringify(countLines(zw, "go")));
		const inline = '<div>\n<script>\n// 注释\nconst a = 1;\n</script>\n<style>\n/* 注释 */\n</style>\n</div>\n';
		const ci = countLines(inline, "html");
		check("④''' HTML 内联 script/style 按 JS/CSS 计（2 注释 / 7 代码）", ci.comment === 2 && ci.code === 7, JSON.stringify(ci));
		const css = "/* a */\n.b { color: red }\n";
		const sql = "-- a\nSELECT 1;\n";
		const yml = "# a\nkey: v\n";
		check(
			"④'''' css/sql/yml 注释符号都被认（各 1 注释 / 1 代码）",
			countLines(css, "css").comment === 1 && countLines(sql, "sql").comment === 1 && countLines(yml, "hash").comment === 1 && countLines(sql, "sql").code === 1,
			JSON.stringify([countLines(css, "css"), countLines(sql, "sql"), countLines(yml, "hash")]),
		);
	}
	{
		const base = { dirs: { "internal/a": 30 }, commentLines: 30 };
		const docsOf = (dirs, rootPlans = FIXTURE_DOCS.rootPlans) => ({ dirs: { "internal/a": 30 }, docs: { ...FIXTURE_DOCS, dirs, rootPlans } });
		const all = FIXTURE_DOCS.dirs;
		const over = run(docsOf({ ...all, contracts: { files: 6, lines: 400 } }), base);
		const under = run(docsOf({ ...all, contracts: { files: 4, lines: 400 } }), base);
		const lineOver = run(docsOf({ ...all, contracts: { files: 5, lines: 401 } }), base);
		const forbidden = run(docsOf({ ...all, design: { files: 1, lines: 10 } }), base);
		const v4Rebuilt = run(docsOf({ ...all, v4: { files: 1, lines: 10 } }), base);
		const unknown = run(docsOf({ ...all, notes: { files: 1, lines: 10 } }), base);
		const rootOver = run(docsOf(all, { files: 2, lines: 40 }), base);
		const rootUnder = run(docsOf(all, { files: 0, lines: 0 }), base);
		check("⑨ docs 待收缩目录 文件数超基线 → DOC_DIR_OVER", kinds(over).includes(CODES.DOC_DIR_OVER), JSON.stringify(kinds(over)));
		check("⑨' docs 待收缩目录 文件数低于基线 → DOC_DIR_UNDER", kinds(under).includes(CODES.DOC_DIR_UNDER), JSON.stringify(kinds(under)));
		check("⑨'' 16 份并成 1 份也拦得住（行数超基线 → DOC_DIR_OVER）", kinds(lineOver).includes(CODES.DOC_DIR_OVER), JSON.stringify(kinds(lineOver)));
		check("⑨''' docs/design 重建 → DOC_DIR_FORBIDDEN", kinds(forbidden).includes(CODES.DOC_DIR_FORBIDDEN), JSON.stringify(kinds(forbidden)));
		check("⑨'''-b docs/v4 重建 → DOC_DIR_FORBIDDEN（#495 B 删完后不许回填）", kinds(v4Rebuilt).includes(CODES.DOC_DIR_FORBIDDEN), JSON.stringify(kinds(v4Rebuilt)));
		check("⑨'''' 判据外的 docs/notes → DOC_DIR_UNKNOWN", kinds(unknown).includes(CODES.DOC_DIR_UNKNOWN), JSON.stringify(kinds(unknown)));
		check("⑨''''' 仓库根 plan 类 md 超基线 → DOC_ROOT_OVER", kinds(rootOver).includes(CODES.DOC_ROOT_OVER), JSON.stringify(kinds(rootOver)));
		check("⑨'''''' 仓库根 plan 类 md 低于基线 → DOC_ROOT_UNDER", kinds(rootUnder).includes(CODES.DOC_ROOT_UNDER), JSON.stringify(kinds(rootUnder)));
		check("⑨''''''' 判据齐备时 docs 面零 finding", run(docsOf(all), base).issues.length === 0, JSON.stringify(run(docsOf(all), base).issues));
	}
	{
		const res = evaluate({ measured: { dirs: {}, files: [], total: { files: 3, comment: 3, code: 3 } }, docs: FIXTURE_DOCS }, { ...FIXTURE_BASELINE, dirs: {}, commentLines: 3 });
		check("⑩ 扫到的文件/代码任一不足下限 → SCAN_TOO_SMALL ×2（注释行不再有绝对地板）", kinds(res).filter((k) => k === CODES.SCAN_TOO_SMALL).length === 2, JSON.stringify(kinds(res)));
	}
	{
		const measured = fixtureMeasured({ dirs: { "internal/a": 24 } });
		const docs = { total: 8, lines: 500, dirs: { contracts: { files: 4, lines: 350 } }, rootPlans: { files: 1, lines: 40 } };
		const baseline = {
			commentLines: 100,
			dirs: { "internal/a": 30 },
			dense: {},
			docs: { contracts: { files: 5, lines: 400 } },
			rootPlans: { files: 1, lines: 40 },
		};
		const { baseline: next, refusals, changes } = nextBaseline(measured, docs, baseline, "2026-11-20");
		check(
			"⑪ 收紧被记录（internal/a 30→24，contracts 5份/400行→4份/350行）",
			next.dirs["internal/a"] === 24 && next.docs.contracts.files === 4 && next.docs.contracts.lines === 350 && changes.length >= 2 && next.commentLines === 24,
			JSON.stringify({ dirs: next.dirs, docs: next.docs, total: next.commentLines }),
		);
		const smallNew = nextBaseline(fixtureMeasured({ dirs: { "internal/a": 24, "internal/b": 5 } }), docs, baseline, "2026-11-20");
		check("⑪' 小新目录自动登记（internal/b=5）", smallNew.baseline.dirs["internal/b"] === 5, JSON.stringify(smallNew.baseline.dirs));
		const bigNew = nextBaseline(fixtureMeasured({ dirs: { "internal/a": 24, "internal/b": NEW_DIR_COMMENT_MAX + 1 } }), docs, baseline, "2026-11-20");
		check("⑪'' 大新目录不许自动入表（走 NEW_DIR_COMMENT_MAX 判据）", bigNew.baseline.dirs["internal/b"] === undefined && bigNew.refusals.some((r) => r.includes("新目录不许自动入表")), JSON.stringify(bigNew.refusals));
		const raised = nextBaseline(fixtureMeasured({ dirs: { "internal/a": 31 } }), docs, baseline, "2026-11-20");
		check("⑪''' 抬高被拒（internal/a 31 > 30）", raised.refusals.some((r) => r.startsWith("internal/a")) && raised.baseline.dirs["internal/a"] === 30, JSON.stringify(raised.refusals));
		const denseRefused = nextBaseline(
			fixtureMeasured({ dirs: { "internal/a": 30 }, files: [{ path: "internal/a/x.go", dir: "internal/a", comment: 99, code: 10 }] }),
			docs,
			baseline,
			"2026-11-20",
		);
		check("⑪'''' 密集文件不许自动入表", denseRefused.refusals.some((r) => r.includes("不许自动入表")) && denseRefused.baseline.dense["internal/a/x.go"] === undefined, JSON.stringify(denseRefused.refusals));
		const dropped = nextBaseline(
			fixtureMeasured({ dirs: { "internal/a": 10 }, files: [{ path: "internal/a/x.go", dir: "internal/a", comment: 10, code: 300 }] }),
			docs,
			{ ...baseline, commentLines: 10, dirs: { "internal/a": 30 }, dense: { "internal/a/x.go": 40 } },
			"2026-11-20",
		);
		check("⑪''''' 已不超阈值的登记在 --update 里作废（不留过期豁免）", dropped.baseline.dense["internal/a/x.go"] === undefined, JSON.stringify(dropped));
		const seeded = nextBaseline(
			fixtureMeasured({ dirs: { "internal/a": 40 }, files: [{ path: "internal/a/x.go", dir: "internal/a", comment: 40, code: 40 }] }),
			docs,
			{ dirs: {}, dense: {}, docs: {} },
			"2026-11-20",
			{ seedDense: true },
		);
		check("⑪'''''' --init 才允许把当前密集文件入表", seeded.baseline.dense["internal/a/x.go"] === 40 && seeded.refusals.length === 0, JSON.stringify(seeded.refusals));
	}
	{
		const res = run({ dirs: { "internal/a": 3 }, files: [{ path: "internal/a/y.go", dir: "internal/a", comment: 3, code: 60 }] }, { dirs: { "internal/a": 3 }, commentLines: 3 });
		check("⑫ 干净文件（3 行注释 / 60 行代码）→ 绿", res.issues.length === 0, JSON.stringify(res.issues));
	}
	{
		const res = run({ dirs: { "internal/b": 4 } }, { dirs: { "internal/a": 30, "internal/b": 4 }, commentLines: 4 });
		check("⑬ 基线里的目录已不存在 → DIR_UNDER 提示删条目", res.issues.some((i) => i.kind === CODES.DIR_UNDER && i.message.includes("删掉这条")), JSON.stringify(res.issues));
	}

	if (failed) {
		console.error(`\n❌ 自测 ${failed} 组不通过 —— 判据本身坏了，先修判据`);
		return 1;
	}
	console.log("\n✅ 自测全部通过（超限/等于/低于 三态 + 密集五态 + 口径五测 + docs 面八态 + 基线只降不升）");
	return 0;
}

function usage() {
	console.error(
		[
			"用法：",
			"  node scripts/check-comment-density.mjs              判定（超/低于基线都红）",
			"  node scripts/check-comment-density.mjs --report     盘点目录/热点/文档面，exit 0",
			"  node scripts/check-comment-density.mjs --self-test  判据红/绿双侧自证",
			"  node scripts/check-comment-density.mjs --init       首次建立基线（已初始化时拒绝）",
			"  node scripts/check-comment-density.mjs --update     收紧基线到实测值（只降不升）",
			"退出码：0 通过 / 1 有 findings / 2 BASELINE 未初始化或用法错误",
		].join("\n"),
	);
}

function main() {
	const argv = process.argv.slice(2);
	for (const a of argv) {
		if (a === "--self-test") return selfTest();
		if (a === "--help" || a === "-h") {
			usage();
			return 0;
		}
		if (a === "--report") return report(measure(collectSources()), BASELINE, docSurface());
		if (a === "--init") return runInit();
		if (a === "--update") return runUpdate();
		if (a !== "--check") {
			console.error(`未知参数：${a}`);
			usage();
			return 2;
		}
	}
	return runCheck();
}

const invokedDirectly = runsAsMain(import.meta.url);
if (invokedDirectly) process.exitCode = main();
