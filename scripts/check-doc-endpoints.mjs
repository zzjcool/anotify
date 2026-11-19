#!/usr/bin/env node
/* Anotify · 文档端点检查（#457）
 *
 * 拦的事故
 *   接入文档教**已被删除的端点**：`web-src/pages/docs.html` 的「接入 Agent」步骤与
 *   `events.empty` 文案都写 `POST /v1/notify` / `POST /v1/events`，而 v4 的入口是
 *   `POST /v1/namespaces/{namespace}/events`（`api/ingress.yaml` 里 0 命中，真打必 404；
 *   旧扁平路由的下线由 `internal/server/api_integration_test.go` 钉住）。
 *   文档里的 URL 没有执行者复算，所以只能随时间变错 —— 本脚本把它变成构建期事实：
 *   **文档里出现的每个 `/v1/<path>` 必须在 `api/*.yaml` 的 `paths:` 里真的存在**。
 *
 * 判据（纯函数，可 --self-test 自证红/绿两侧）
 *   a. 事实源：`api/*.yaml` 的 `paths:` 块里缩进两格的 `/…:` 键（五份 v4 spec 合并去重）。
 *   b. 扫面：`web-src/`（sitegen 的输入面 = 用户最终看到的文档）与两份 README。
 *      `docs/**`、`design/`、`DEVELOPMENT.md` 等历史/内部文档**不在**扫面内：它们存在的
 *      意义就是记录旧端点（v3 快照、设计稿、复盘报告），把它们也管起来只会逼人到处贴豁免。
 *   c. 归一化：去 `?query` / `#fragment` / 行尾标点；`/v1/<path>` 按 `/` 切段逐段比对，
 *      spec 里的 `{param}`（或 `:param`）段可被文档里的任何非空段命中，因此
 *      `…/namespaces/team-a/events`、`/v1/hooks/src_xxx` 这类**带具体值**的写法合法；
 *      文档里的 `{ns}` / `...` / `:id` 也都能命中对应的 spec 段。
 *   d. 历史提及（沿用 `scripts/check-annotations.mjs` 的「历史提及」口子，父会话裁决）：
 *      同一行写了「已废弃 / 已删除 / 已下线 / 已退役」或 `deprecated` / `retired` /
 *      `removed` ⇒ 引用不存在算**历史提及**，不判红、单独计数；或整段用
 *      `<!-- docs-endpoints:historical -->` … `<!-- /docs-endpoints:historical -->` 包住。
 *      **只对「引用不存在」生效**：端点存在时照常算解析成功。
 *   e. 反假绿：`html` 里的 `<script>` 与 HTML 注释先遮罩（那是客户端代码/给人看的注释，
 *      不是教学文案）；扫面解析出空集（spec 路径 / 文件 / 引用任一为 0）一律**退出码 2** ——
 *      「绿」与「没检查」不能同形。`paths:` 块解析必须 fail-loud，不认识的形状不许静默跳过。
 *
 * 用法
 *   node scripts/check-doc-endpoints.mjs               # 体检（CI 用）；有 findings ⇒ exit 1
 *   node scripts/check-doc-endpoints.mjs --self-test   # 判据自身的红/绿自证（含变异用例）
 *   node scripts/check-doc-endpoints.mjs --report      # 只列 findings，不判红，exit 0
 *   退出码：0 = 通过 / 1 = 有 findings / 2 = 用法或环境问题（含「一条都没扫到」）
 */
import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

/** 文档扫面：由浅到深显式声明，缺一个就少一块覆盖，所以列成常量而不是 glob 猜。 */
export const TARGET_DIRS = ["web-src/pages", "web-src/layouts", "web-src/locales"];
export const TARGET_FILES = ["README.md", "README.en.md"];
const EXTS = new Set([".html", ".yaml"]);

/** 下限（反假绿）：真实仓库远超这些数；达不到就说明解析面坏了，宁可退出码 2。 */
export const FLOORS = { specPaths: 50, files: 10, tokens: 60 };

export const HIST_START = "docs-endpoints:historical";
export const HIST_END = "/docs-endpoints:historical";

/** 「明确标注已废弃」的措辞（大小写不敏感）。 */
export const DEPRECATED_WORDS = [
	"已废弃",
	"已删除",
	"已下线",
	"已退役",
	"废弃",
	"下线",
	"退役",
	"deprecated",
	"retired",
	"removed",
	"deleted",
];

/* ────────────────────────── spec 侧：paths ────────────────────────── */

/**
 * 从一份 OpenAPI 文本里取 `paths:` 块的路径键。
 * fail-loud：`paths:` 之后一旦出现顶格的新键就结束；块内出现既不是路径也不是
 * 注释/空行的形状，返回 error 而不是跳过（静默跳过 = 假绿的经典入口）。
 */
export function parseSpecPaths(text, file = "spec.yaml") {
	const paths = [];
	const errs = [];
	const lines = text.split("\n");
	let inPaths = false;
	let sawPaths = false;
	for (let i = 0; i < lines.length; i++) {
		const line = lines[i];
		const trimmed = line.trim();
		if (!inPaths) {
			if (/^paths:\s*$/.test(line)) {
				inPaths = true;
				sawPaths = true;
			}
			continue;
		}
		if (trimmed === "" || trimmed.startsWith("#")) continue;
		if (/^\S/.test(line)) break; // 顶格新键 ⇒ paths 块结束
		if (line.startsWith("  /")) {
			const m = /^  (\/\S*?):\s*$/.exec(line);
			if (!m) {
				errs.push(`${file}:${i + 1} 无法解析的 paths 键：${line.trim()}`);
				continue;
			}
			paths.push(m[1]);
			continue;
		}
		if (/^ {1,3}\S/.test(line)) {
			errs.push(`${file}:${i + 1} paths 块内出现无法识别的行（禁止静默跳过）：${line.trim()}`);
		}
	}
	if (!sawPaths) errs.push(`${file}: 没有 paths: 块`);
	if (sawPaths && paths.length === 0) errs.push(`${file}: paths: 块里一条路径都没解析出来`);
	return { paths, errs };
}

/* ────────────────────────── 文档侧：归一化与比对 ────────────────────────── */

const PATH_TAIL_CHARS = "/{}:?=&%._~+-*…";

function isPathChar(ch) {
	const c = ch.charCodeAt(0);
	if (c >= 48 && c <= 57) return true; // 0-9
	if (c >= 65 && c <= 90) return true; // A-Z
	if (c >= 97 && c <= 122) return true; // a-z
	return PATH_TAIL_CHARS.includes(ch);
}

/** 去 query / fragment / 行尾标点 / 尾斜杠。 */
export function normalizeDocPath(raw) {
	let s = raw;
	const q = s.indexOf("?");
	if (q >= 0) s = s.slice(0, q);
	const h = s.indexOf("#");
	if (h >= 0) s = s.slice(0, h);
	while (s.length > 0) {
		const last = s[s.length - 1];
		if (last === "/" || last === "." || last === "," || last === ";" || last === ":" || last === "!") {
			s = s.slice(0, -1);
			continue;
		}
		if (last === ")" || last === "]" || last === "}") {
			// 括号只在成对出现时保留（`{param}` 是路径段），否则是行文括号
			const open = last === ")" ? "(" : last === "]" ? "[" : "{";
			if (s.indexOf(open) < 0) {
				s = s.slice(0, -1);
				continue;
			}
		}
		break;
	}
	return s;
}

function segments(p) {
	return p.split("/").filter((x) => x.length > 0);
}

function isTemplateSegment(seg) {
	if (seg.length >= 3 && seg.startsWith("{") && seg.endsWith("}")) return true;
	if (seg.length >= 2 && seg.startsWith(":")) return true;
	return false;
}

function isWildcardSegment(seg) {
	return seg === "..." || seg === "*" || seg === "…";
}

/** spec 段是 `{param}`/`:param` ⇒ 文档里任何非空段都合法。 */
function matchesSegment(docSeg, specSeg) {
	if (isWildcardSegment(docSeg) || isTemplateSegment(docSeg)) return true;
	if (isTemplateSegment(specSeg)) return docSeg.length > 0;
	return docSeg === specSeg;
}

/**
 * 文档里的 `/v1/...` 是否真的存在于 spec 路径集合里。
 * 另允许「基路径」写法（以 / 结尾且是某条 spec 路径的前缀，如页面脚本里的 `/v1/system/`）——
 * 它指向的是一棵子树而不是一个端点。
 */
export function docPathExists(docPath, specPaths) {
	const raw = docPath.trim();
	const basePath = raw.endsWith("/"); // 归一化会吃掉尾斜杠，先记下来
	const doc = normalizeDocPath(raw);
	if (doc === "/v1" || doc === "/v1/") return true; // 版本前缀本身不是端点
	const docSegs = segments(doc);
	for (const spec of specPaths) {
		const specSegs = segments(spec);
		if (specSegs.length !== docSegs.length) continue;
		let ok = true;
		for (let i = 0; i < specSegs.length; i++) {
			if (!matchesSegment(docSegs[i], specSegs[i])) {
				ok = false;
				break;
			}
		}
		if (ok) return true;
	}
	if (basePath) {
		const prefix = doc.replace(/\/+$/, "") + "/";
		for (const spec of specPaths) if (spec.startsWith(prefix)) return true;
	}
	return false;
}

/* ────────────────────────── 文档侧：遮罩与扫描 ────────────────────────── */

/** HTML 注释与 <script> 不是教学文案（前者是给人看的注，后者是客户端代码）。 */
export function maskNonCopy(text, isHtml) {
	let out = text.replace(/<!--[\s\S]*?-->/g, (m) => m.replace(/[^\n]/g, " "));
	if (isHtml) {
		out = out.replace(/<script\b[\s\S]*?<\/script>/gi, (m) => m.replace(/[^\n]/g, " "));
	}
	return out;
}

/** 抽一行里的所有 `/v1/...`（保序，含列号）。 */
export function tokensIn(line) {
	const out = [];
	let i = 0;
	for (;;) {
		const j = line.indexOf("/v1/", i);
		if (j < 0) break;
		let k = j;
		while (k < line.length && isPathChar(line[k])) k += 1;
		out.push({ token: line.slice(j, k), col: j });
		i = k > j ? k : j + 4;
	}
	return out;
}

function lowerAscii(s) {
	return s.toLowerCase();
}

export function lineMarksDeprecated(line) {
	const low = lowerAscii(line);
	for (const w of DEPRECATED_WORDS) if (low.includes(lowerAscii(w))) return true;
	return false;
}

/**
 * 扫一份文档。纯函数（无 fs / 无全局状态），self-test 直接喂 fixture。
 * 返回 { findings, tokens, historical, markerErrors }。
 */
export function scanDoc(name, text, specPaths) {
	const isHtml = name.endsWith(".html");
	const rawLines = text.split("\n");
	const maskedLines = maskNonCopy(text, isHtml).split("\n");

	// 历史区：`<!-- docs-endpoints:historical -->` … `<!-- /docs-endpoints:historical -->`
	const historicalLines = new Set();
	const markerErrors = [];
	let inHist = false;
	for (let i = 0; i < rawLines.length; i++) {
		const line = rawLines[i];
		const starts = line.includes(HIST_START);
		const ends = line.includes(HIST_END);
		if (!inHist && starts) {
			inHist = true;
			if (ends && line.indexOf(HIST_END) > line.indexOf(HIST_START)) inHist = false;
			continue;
		}
		if (inHist && ends) {
			inHist = false;
			continue;
		}
		if (inHist) historicalLines.add(i);
	}
	if (inHist) markerErrors.push(`${name}: 历史区标记没闭合（缺 ${HIST_END}）`);

	const findings = [];
	let tokens = 0;
	let historical = 0;
	for (let i = 0; i < maskedLines.length; i++) {
		for (const t of tokensIn(maskedLines[i])) {
			tokens += 1;
			if (docPathExists(t.token, specPaths)) continue;
			if (lineMarksDeprecated(rawLines[i]) || lineMarksDeprecated(rawLines[i - 1] ?? "")) {
				historical += 1;
				continue;
			}
			if (historicalLines.has(i)) {
				historical += 1;
				continue;
			}
			findings.push({ file: name, line: i + 1, token: t.token });
		}
	}
	return { findings, tokens, historical, markerErrors };
}

/* ────────────────────────── 文件收集 ────────────────────────── */

async function walk(dir, root) {
	const out = [];
	let entries = [];
	try {
		entries = await fs.readdir(dir, { withFileTypes: true });
	} catch {
		return out;
	}
	for (const e of entries.sort((a, b) => a.name.localeCompare(b.name))) {
		const full = path.join(dir, e.name);
		if (e.isDirectory()) out.push(...(await walk(full, root)));
		else if (EXTS.has(path.extname(e.name))) out.push(path.relative(root, full));
	}
	return out;
}

export async function collectTargets(root) {
	const files = [];
	for (const rel of TARGET_DIRS) files.push(...(await walk(path.join(root, rel), root)));
	for (const rel of TARGET_FILES) {
		try {
			await fs.access(path.join(root, rel));
			files.push(rel);
		} catch {
			// 缺文件会撞到下方的文件数下限（不静默放过）
		}
	}
	return files;
}

export async function loadSpecPaths(root) {
	const dir = path.join(root, "api");
	let entries = [];
	try {
		entries = await fs.readdir(dir);
	} catch {
		return { paths: [], errs: [`无法读取 ${dir}（事实源缺失 ⇒ 检查面为空，不是通过）`], files: [] };
	}
	const names = entries.filter((n) => n.endsWith(".yaml")).sort();
	const paths = new Set();
	const errs = [];
	if (names.length === 0) errs.push(`${dir} 下没有 *.yaml`);
	for (const n of names) {
		const text = await fs.readFile(path.join(dir, n), "utf8");
		const r = parseSpecPaths(text, n);
		errs.push(...r.errs);
		for (const p of r.paths) paths.add(p);
	}
	return { paths: [...paths].sort(), errs, files: names };
}

/** 下限自证（纯函数，自测直接喂数）。 */
export function floorProblems(counts) {
	const bad = [];
	if (counts.specPaths < FLOORS.specPaths)
		bad.push(`spec 路径只解析出 ${counts.specPaths} 条（下限 ${FLOORS.specPaths}）：paths 解析面可能坏了`);
	if (counts.files < FLOORS.files)
		bad.push(`只扫到 ${counts.files} 个文档文件（下限 ${FLOORS.files}）：扫面可能被裁掉了`);
	if (counts.tokens < FLOORS.tokens)
		bad.push(`只看到 ${counts.tokens} 处 /v1/ 引用（下限 ${FLOORS.tokens}）：token 解析面可能坏了`);
	return bad;
}

/* ────────────────────────── main ────────────────────────── */

async function main() {
	const root = path.resolve(process.argv.find((a) => a.startsWith("--root="))?.slice(7) ?? ".");
	const reportOnly = process.argv.includes("--report");

	const { paths: specPaths, errs: specErrs, files: specFiles } = await loadSpecPaths(root);
	if (specErrs.length) {
		console.error("❌ 文档端点检查：api/*.yaml 的 paths 块解析失败（fail-loud）");
		for (const e of specErrs) console.error(`   ${e}`);
		process.exit(2);
	}

	const targets = await collectTargets(root);
	if (targets.length === 0) {
		console.error("❌ 文档端点检查：一个文档文件都没扫到（扫面坏了，不是通过）");
		process.exit(2);
	}

	const findings = [];
	const markerErrors = [];
	let tokens = 0;
	let historical = 0;
	for (const rel of targets) {
		const text = await fs.readFile(path.join(root, rel), "utf8");
		const r = scanDoc(rel, text, specPaths);
		findings.push(...r.findings);
		markerErrors.push(...r.markerErrors);
		tokens += r.tokens;
		historical += r.historical;
	}

	const floors = floorProblems({ specPaths: specPaths.length, files: targets.length, tokens });
	if (floors.length) {
		console.error("❌ 文档端点检查：检查面不足（这不是通过）");
		for (const f of floors) console.error(`   ${f}`);
		process.exit(2);
	}

	const head = `${specPaths.length} 条 spec 路径（${specFiles.join(" / ")}）· ${targets.length} 个文档文件 · ${tokens} 处 /v1/ 引用`;

	if (markerErrors.length) {
		console.error(`❌ 文档端点检查：历史区标记坏了 ${markerErrors.length} 处`);
		for (const e of markerErrors) console.error(`   ${e}`);
		if (!reportOnly) process.exit(1);
	}

	if (findings.length) {
		console.error(`❌ 文档端点检查：${findings.length} 处端点不存在于 api/*.yaml（必 404）`);
		for (const f of findings) {
			console.error(`   ${f.file}:${f.line}  ${f.token}`);
		}
		console.error("");
		console.error("   改法只有两种（#457）：① 换成本仓真实存在的端点；");
		console.error(`   ② 若确实是历史对照，就在同一行标注「已废弃 → <新端点>」，或把整段包进`);
		console.error(`      <!-- ${HIST_START} --> … <!-- ${HIST_END} -->（含理由）。`);
		console.error(`   本轮检查面：${head}；历史提及（已废弃/整段豁免）：${historical} 处`);
		if (!reportOnly) process.exit(1);
		return;
	}

	console.log(`✅ 文档端点检查：${head} 全部存在于 api/*.yaml（历史提及 ${historical} 处）`);
}

/* ────────────────────────── self-test ────────────────────────── */

const SPEC_FIXTURE = [
	"/v1/namespaces/{namespace}/events",
	"/v1/namespaces/{namespace}/tasks",
	"/v1/devices",
	"/v1/devices/{id}",
	"/v1/hooks/{token}",
	"/v1/system/config",
	"/v1/console/events",
	"/v1/vapid-public-key",
];

export function selfTestCases() {
	return [
		{
			name: "green: 存在的端点",
			src: "POST /v1/devices 注册订阅",
			want: [],
		},
		{
			name: "green: 带 {param} 的 v4 入口",
			src: "POST /v1/namespaces/{namespace}/events",
			want: [],
		},
		{
			name: "green: 具体值落在 {param} 位上（hook token）",
			src: "回调地址形如 /v1/hooks/src_xxx",
			want: [],
		},
		{
			name: "green: query string 不算路径差异",
			src: "GET /v1/console/events?limit=50&cursor=abc",
			want: [],
		},
		{
			name: "green: 行尾标点/尾斜杠被归一化",
			src: "见 /v1/devices.",
			want: [],
		},
		{
			name: "red（变异）：已删除的 /v1/notify",
			src: "携带 Key POST /v1/notify。",
			want: ["doc-endpoint-missing"],
		},
		{
			name: "red（变异）：已下线的扁平 /v1/events",
			src: "用 POST /v1/events 提交第一个。",
			want: ["doc-endpoint-missing"],
		},
		{
			name: "red: 不存在的子路径（前缀对不上不算通过）",
			src: "GET /v1/devices/abc/extra",
			want: ["doc-endpoint-missing"],
		},
		{
			name: "green: 同一行标注「已废弃」⇒ 历史提及",
			src: "（已废弃）POST /v1/notify → 改用 POST /v1/namespaces/{namespace}/events",
			want: [],
		},
		{
			name: "green: 整段 historical 标记包住",
			src: `<!-- ${HIST_START} · v3 快照 -->\n| POST /v1/notify | 旧入口 |\n<!-- ${HIST_END} -->`,
			want: [],
		},
		{
			name: "red: 历史区结束后立刻恢复判红",
			src: `<!-- ${HIST_START} -->\nPOST /v1/notify\n<!-- ${HIST_END} -->\nPOST /v1/notify`,
			want: ["doc-endpoint-missing"],
		},
		{
			name: "red: 历史区标记没闭合（fail-loud）",
			src: `<!-- ${HIST_START} -->\nPOST /v1/notify`,
			want: [],
			wantMarkers: 1,
		},
		{
			name: "green: HTML 注释里的端点不是教学文案",
			src: "<!-- 旧入口 POST /v1/notify 已删除 -->\n<p>hi</p>",
			want: [],
			html: true,
		},
		{
			name: "green: <script> 里拼的路径是客户端代码",
			src: '<div>x</div>\n<script>\nfetch("/v1/notify", {});\n</script>',
			want: [],
			html: true,
		},
		{
			name: "red: script 之外同样的字符串要判红",
			src: '<div>x</div>\n<p>POST /v1/notify</p>\n<script>\nfetch("/v1/devices");\n</script>',
			want: ["doc-endpoint-missing"],
			html: true,
		},
		{
			name: "green: /v1/ 版本前缀本身不是端点",
			src: "所有动态接口带 /v1/ 前缀",
			want: [],
		},
	];
}

async function selfTest() {
	let failed = 0;
	const check = (ok, label, detail = "") => {
		console.log(`${ok ? "✅" : "❌"} ${label}${ok ? "" : ` —— ${detail}`}`);
		if (!ok) failed += 1;
	};

	for (const c of selfTestCases()) {
		const r = scanDoc(c.html ? "fixture.html" : "fixture.yaml", c.src, SPEC_FIXTURE);
		const got = r.findings.map(() => "doc-endpoint-missing");
		const ok = JSON.stringify(got) === JSON.stringify(c.want) && (r.markerErrors.length === (c.wantMarkers ?? 0));
		check(ok, c.name, `want ${JSON.stringify(c.want)} markers=${c.wantMarkers ?? 0}, got ${JSON.stringify(got)} markers=${r.markerErrors.length}`);
	}

	// 行号必须指向真实行（不能是一句「某处」）
	const lr = scanDoc("fixture.yaml", "ok\n\nPOST /v1/notify", SPEC_FIXTURE);
	check(lr.findings.length === 1 && lr.findings[0].line === 3, "findings 带真实行号", JSON.stringify(lr.findings));

	// 基路径（页面脚本里的 /v1/system/）指向一棵子树，不算端点缺失
	const base = scanDoc("fixture.yaml", "前缀 /v1/system/ 下", SPEC_FIXTURE);
	check(base.findings.length === 0, "green: 基路径 /v1/system/ 允许", JSON.stringify(base.findings));

	// 下限自证：三项下限任意一项归零都必须判「检查面不足」
	const f0 = floorProblems({ specPaths: 0, files: 0, tokens: 0 });
	check(f0.length === 3, "下限：0/0/0 三项全红", JSON.stringify(f0));
	const fOk = floorProblems({ specPaths: 100, files: 30, tokens: 200 });
	check(fOk.length === 0, "下限：正常规模不误报", JSON.stringify(fOk));

	// spec 侧 fail-loud：paths 块里出现无法识别的行必须报错，而不是静默少算
	const bad = parseSpecPaths("paths:\n  /v1/a:\n    get: {}\n  not-a-path\n");
	check(bad.errs.length >= 1 && bad.paths.length === 1, "spec: paths 块内坏行 fail-loud", JSON.stringify(bad));
	const noPaths = parseSpecPaths("openapi: 3.1.0\ninfo: {}\n");
	check(noPaths.errs.length >= 1, "spec: 没有 paths 块 ⇒ 报错", JSON.stringify(noPaths));
	const good = parseSpecPaths("paths:\n  /v1/a:\n    get: {}\n  # 注释\n\n  /v1/b:\n    post: {}\ncomponents: {}\n");
	check(good.paths.length === 2 && good.errs.length === 0, "spec: 正常解析两条路径", JSON.stringify(good));

	// 本仓库真实事实源能被解析（防「fixture 绿、真文件解析为空」）
	const real = await loadSpecPaths(path.resolve(path.dirname(fileURLToPath(import.meta.url)), ".."));
	check(real.paths.length >= FLOORS.specPaths && real.errs.length === 0, "真实 api/*.yaml 解析通过", `${real.paths.length} 条 / errs=${JSON.stringify(real.errs)}`);

	return failed === 0;
}

const isDirectRun =
	process.argv[1] && path.resolve(process.argv[1]) === path.resolve(fileURLToPath(import.meta.url));
if (isDirectRun) {
	if (process.argv.includes("--self-test")) {
		selfTest()
			.then((passed) => process.exit(passed ? 0 : 1))
			.catch((e) => {
				console.error(e);
				process.exit(2);
			});
	} else {
		main().catch((e) => {
			console.error(e);
			process.exit(2);
		});
	}
}
