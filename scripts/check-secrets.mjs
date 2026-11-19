#!/usr/bin/env node
/* Anotify · 秘密扫描前置守卫（push 之前就能跑的那道闸）
 *
 * 为什么需要它（真实事故背景）：
 *   GitHub 上的 `GitGuardian Security Checks` 是**服务端 App**：它只在 push 之后扫 PR
 *   diff。本地 `git commit` 时毫无感觉 —— 而本仓的秘密面恰好都在「提交一次就完蛋」的位置：
 *   · `anode install` 把节点身份令牌写进 0600 的 `anode.yaml`（`cmd/anode/internal/config/config.go:59`
 *     的 `token` 字段；同目录的 `install.json` 是 PATH 记录），而 `.gitignore` 头三行专门列了
 *     「敏感数据（含私钥，绝勿提交）」：`vapid.json` / `subscriptions.json`；
 *   · VAPID 私钥对（`make keys` 生成，`scripts/genkeys.go` 直接打印）；
 *   · `docs/` 里成百条示例命令，最容易「照抄一条把真值粘回来」。
 *   秘密一旦 push 出去，即使事后 rewrite 历史，PR diff / CI 日志 / GitHub 的副本都已经
 *   存下来了，GitGuardian 也会把该 commit 与令牌本身永久记档 —— **唯一正确处置是轮换
 *   令牌**。所以判据必须在「离开本机之前」跑，而不是在 push 之后等一个红色 PR 检查。
 *
 * 判定方法（全部纯静态，只用 node: 内置模块，零 npm 依赖）：
 *   扫描范围：默认 `git ls-files`（全部被跟踪文件，读工作区内容）；`--staged` 只扫
 *   `git diff --cached --name-only`（读**索引内容** `git show :path`，即真正要提交的字节）。
 *   内容不做熵检测（见「已知边界」），只认**高信号形状**，每条都能点名 `文件:行`：
 *   ① `PEM_PRIVATE_KEY`        `-----BEGIN (RSA |EC |OPENSSH |PGP |ENCRYPTED )?PRIVATE KEY-----`
 *                              （`BEGIN PRIVATE KEY` / `BEGIN EC PRIVATE KEY` 都在内；VAPID 私钥
 *                              若以 PEM 形态落在文件里，命中的就是 BEGIN 这一行）
 *   ② 供应商形状 7 条           `ghp_` / `github_pat_` / `AKIA` / `xox[baprs]-` / `sk-` /
 *                              `sk_live_` / `AIza`
 *   ③ 本仓专属形状：`VAPID_PRIVATE(_KEY)` 或 `PRIVATE_KEY` / `"privateKey"` 后跟**实际值**
 *      （≥40 字符 base64url，即 32 字节私钥的真实长度；`YOUR_VAPID_PRIVATE_KEY_HERE` 这种
 *      25 字符的占位符根本达不到长度阈值）；`token:` / `token=` / `ANOTIFY_*TOKEN[= ]` /
 *      `Bearer ` 后跟 ≥32 字符 base64url/hex 字面量（**占位符语法**另行放行，见下）。
 *   ④ 文件名判据（**被跟踪即报**，与内容无关）：`.env` / `.env.*` / `*.pem` / `*.key` /
 *      `*.p12` / `*.pfx` / `id_rsa|id_dsa|id_ecdsa|id_ed25519*` / `*_secret*`，以及
 *      `install.json` 里出现 `"token":` 字段（`anode install` 的落盘产物）。
 *
 * 反误报（这条最重要）：
 *   本仓到处都是**夹具令牌**与**哈希**：`fixtures/`、`testdata/`、`*_test.go` 里是
 *   `fixtureToken` / `*-sentinel` 之类的哨兵字符串，`docs/` 与 `.pi-orchestrator/` 里是
 *   成片的 sha256（80 处 64 位 hex）与镜像 digest。所以：
 *   · **不做熵检测**。熵规则在这种仓库里必然误报，一旦误报就会被加宽放行，最后变噪声。
 *   · 占位符**语法**在判据层直接放行（不算命中，也就不需要登记）：
 *     `<token>`、`${VAR}`、`$(cmd)`、`{{x}}` 这类含 `<>${}()[]` 的值；
 *     `xxxx…` / `****` / `----` 这类重复掩码；以及含 `PLACEHOLDER` / `REDACTED` /
 *     `CHANGEME` / `REPLACE_ME` / `YOUR_` / `DUMMY` / `EXAMPLE` 词的值。
 *     —— 真实密钥是随机串，不会拼出这些词；而长度阈值已经把短占位符挡在门外。
 *   · 剩下的「结构上像密钥、其实是我方夹具」一律进 `ALLOWLIST`，每条必须写
 *     **路径 glob + 匹配到的原文片段 + 理由**，且**过期即红**（登记的片段若不再命中任何
 *     命中点 → 报 `EXPIRED_ALLOWLIST`，要求删掉），照 `internal/server/openapi_parity_test.go`
 *     的例外表纪律。路径 glob 参与匹配：同一个片段出现在 glob 之外 ⇒ 照样红（有自测）。
 *     过期判定只在**全量模式**做：`--staged` 只扫暂存的那几个文件，登记表必然一条都不
 *     命中，若照旧判过期，每次 pre-commit 都会叫红 5 条 —— 检查变噪声后人就开始
 *     `--no-verify`。
 *   · `.env.example` / `.env.*.example` 这类**模板文件**走 `PATH_EXEMPTIONS`
 *     （同样过期即红），且只豁免**文件名判据** —— 里面的内容判据照常跑（模板里粘了真值
 *     一样红）。
 *
 * 反假绿：
 *   · 永远打印「扫描文件数 / 检测器数 / 命中数 / 放行数」，并断言扫描文件数 > `FILES_MIN`、
 *     检测器数 > `DETECTOR_MIN`；扫到 0 个可读文件直接红 —— 「绿」不能和「没检查」同形。
 *   · 被跳过的文件（二进制、>2MB、`node_modules/` 等）**按原因打印计数**，不静默。
 *   · `--self-test`：19 个内存 fixture 覆盖红/绿双侧（PEM / `ghp_` / `AKIA` / 夹具令牌 /
 *     sha256 / 占位符 / 被跟踪的 `.env` / `.env.example` / `--staged` 空暂存 / ALLOWLIST
 *     过期 / 0 文件 / 7 条供应商形状全覆盖 / 真值不被占位符过滤器吞掉 / 路径豁免过期 /
 *     glob 之外同片段仍红 / 检测器下限 / `--staged` 不判过期 / 4 条文件名判据都有牙 /
 *     `--staged` 只暂存二进制也算绿），
 *     纯内存，不动仓库文件。
 *
 * 用法：
 *   node scripts/check-secrets.mjs              # 全仓体检（push 前手动跑）
 *   node scripts/check-secrets.mjs --staged     # 只扫暂存内容（供 pre-commit 用）
 *   node scripts/check-secrets.mjs --self-test  # 跑本脚本自己的红/绿自测
 *   退出码：0 通过 / 1 有命中或例外表过期 / 2 用法或环境问题（不在 git 仓库、git 失败、
 *   参数不认识、默认模式下 0 个被跟踪文件）
 *
 * 已知边界（有意不做的部分）：
 *   · **不查 git 历史**：只看当前工作区（默认）或索引（`--staged`）。已经提交进去的秘密
 *     本脚本无能为力 —— 那要走的是轮换令牌 + 历史清理。
 *   · **不做熵检测**（上述理由）：因此「自定义形状的令牌」（没有 `token:`/`Bearer ` 前缀、
 *     也不属于任何供应商前缀）扫不出来。宁可窄、不可吵：漏报一条自定义令牌，好过每天
 *     几十条夹具误报会把检查变成噪声。
 *   · 逐行匹配：跨行的私钥块只认 `-----BEGIN … PRIVATE KEY-----` 那一行（这就够了），
 *     不解析 PEM 正文；也不校验 base64 是否合法。
 *   · 只看内容与文件名，**不看**文件权限位（`0600` 与否）、不看 `.gitignore` 是否覆盖。
 *   · 默认模式读的是**工作区**内容（不是 HEAD）；`--staged` 读索引内容。两者都不含
 *     未跟踪文件（`git ls-files` 的语义）。
 *   · 本脚本还没有接进 `make` / CI（本轮硬约束只允许新建本文件）；手动跑或在自己的
 *     pre-commit hook 里跑。
 */
import { execFileSync } from "node:child_process";
import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

/** 仓库根（按脚本位置推，不依赖 cwd；与 scripts/check-css.mjs 同款）。 */
const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

/** 单文件大小上限：超过就不读内容（文件名判据仍然生效）。 */
const MAX_BYTES = 2 * 1024 * 1024;

/** 目录前缀跳过（相对仓库根）。`.git/` 与 `node_modules/` 不该出现在 ls-files 里，
 *  `internal/server/dist/` 是 sitegen 生成的 embed 产物（当前未被跟踪，留着防将来入库）。 */
export const SKIP_DIRS = ["node_modules/", ".git/", "internal/server/dist/"];

/** 二进制扩展名：内容不读（文件名判据照常跑，例如 `*.p12` 依然会被点红）。 */
export const BINARY_EXT = new Set([
	".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".bmp", ".tif", ".tiff", ".avif",
	".woff", ".woff2", ".ttf", ".otf", ".eot",
	".pdf", ".zip", ".gz", ".tgz", ".bz2", ".xz", ".7z", ".tar", ".jar", ".wasm",
	".so", ".dylib", ".dll", ".a", ".o", ".exe", ".bin", ".class",
	".db", ".db-wal", ".db-shm", ".sqlite", ".sqlite3",
	".mp3", ".mp4", ".mov", ".webm", ".wav",
]);

/* ---------- 检测器 ----------
 * 每条：{ id, label, needle, needleCi?, re, group?, placeholderGuard?, appliesTo? }
 *   needle  廉价预筛（行内必须出现的字面量），避免对全部行跑全部正则；
 *   re      带 `g` 的行内正则；group 是「秘密原文」的捕获组（默认整段匹配）；
 *   placeholderGuard=true 时，捕获到的值先过 isPlaceholder()，是占位符就不算命中；
 *   appliesTo 是路径过滤器（只有那类文件才跑这条）。
 * 新增检测器必须同步 DETECTOR_MIN（棘轮：删检测器要连下限一起改，并在 PR 说明）。 */
export const CONTENT_DETECTORS = [
	{
		id: "PEM_PRIVATE_KEY",
		label: "PEM 私钥块（BEGIN … PRIVATE KEY）",
		needle: "-----BEGIN",
		re: /-----BEGIN (?:RSA |EC |OPENSSH |PGP |ENCRYPTED )?PRIVATE KEY-----/g,
	},
	{
		id: "GITHUB_PAT",
		label: "GitHub 经典 PAT（ghp_…）",
		needle: "ghp_",
		re: /ghp_[A-Za-z0-9]{36}/g,
	},
	{
		id: "GITHUB_PAT_FINE",
		label: "GitHub 细粒度 PAT（github_pat_…）",
		needle: "github_pat_",
		re: /github_pat_[A-Za-z0-9_]{60,}/g,
	},
	{
		id: "AWS_ACCESS_KEY_ID",
		label: "AWS Access Key ID（AKIA…）",
		needle: "AKIA",
		re: /AKIA[0-9A-Z]{16}/g,
	},
	{
		id: "SLACK_TOKEN",
		label: "Slack token（xox[baprs]-…）",
		needle: "xox",
		re: /xox[baprs]-[A-Za-z0-9-]{10,}/g,
	},
	{
		id: "OPENAI_KEY",
		label: "OpenAI 风格 key（sk-…）",
		needle: "sk-",
		re: /sk-[A-Za-z0-9]{32,}/g,
	},
	{
		id: "STRIPE_LIVE_KEY",
		label: "Stripe live key（sk_live_…）",
		needle: "sk_live_",
		re: /sk_live_[A-Za-z0-9]{20,}/g,
	},
	{
		id: "GOOGLE_API_KEY",
		label: "Google API key（AIza…）",
		needle: "AIza",
		re: /AIza[0-9A-Za-z_-]{35}/g,
	},
	{
		// 覆盖三种落盘形态：`ANOTIFY_VAPID_PRIVATE_KEY=<值>`（env 文件）、
		// `VAPID_PRIVATE_KEY: <值>`（yaml/compose）、`"privateKey": "<值>"`（vapid.json，
		// scripts/e2e/run_all.sh:19 读的就是这个文件；本仓 .gitignore 头一行专门禁它）。
		// 阈值 40 字符 = 32 字节私钥的 base64url 真实长度：短占位符天然不命中。
		id: "VAPID_PRIVATE_VALUE",
		label: "VAPID/私钥变量带了实际值",
		needle: "private",
		needleCi: true,
		re: /(?:VAPID[_-]PRIVATE(?:[_-]KEY)?|PRIVATE[_-]?KEY)["']?\s*[:=]\s*["']?([A-Za-z0-9_+/=-]{40,})/gi,
		group: 1,
		placeholderGuard: true,
	},
	{
		id: "TOKEN_LITERAL",
		label: "token: / token= 后跟 ≥32 字符字面量",
		needle: "token",
		needleCi: true,
		re: /\btoken["']?\s*[:=]\s*["']?([A-Za-z0-9_+/=-]{32,})/g,
		group: 1,
		placeholderGuard: true,
	},
	{
		id: "ANOTIFY_TOKEN_LITERAL",
		label: "ANOTIFY_*TOKEN= 后跟 ≥32 字符字面量",
		needle: "ANOTIFY_",
		re: /ANOTIFY_[A-Z_]*TOKEN\s*[=: ]\s*["']?([A-Za-z0-9_+/=-]{32,})/g,
		group: 1,
		placeholderGuard: true,
	},
	{
		id: "BEARER_LITERAL",
		label: "Bearer 后跟 ≥32 字符字面量",
		needle: "Bearer",
		re: /\bBearer\s+([A-Za-z0-9_+/=.-]{32,})/g,
		group: 1,
		placeholderGuard: true,
	},
	{
		// `anode install` 的同目录落盘产物：一个本该只活在本机的文件被跟踪、还带 token 字段
		// （真令牌在 0600 的 anode.yaml 里；install.json 一旦带 token 就是事故现场）。
		id: "INSTALL_JSON_TOKEN",
		label: "install.json 含 \"token\" 字段",
		needle: "\"token\"",
		re: /"token"\s*:/g,
		appliesTo: (file) => path.posix.basename(file) === "install.json",
	},
];

/* 文件名判据：**被跟踪即报**，与内容无关（二进制也照报，例如误入的 *.p12）。 */
export const FILENAME_DETECTORS = [
	{
		id: "FILENAME_DOTENV",
		label: ".env / .env.* 被跟踪",
		test: (file) => {
			const base = path.posix.basename(file);
			return base === ".env" || base.startsWith(".env.");
		},
	},
	{
		id: "FILENAME_PRIVATE_KEY_FILE",
		label: "私钥/密钥容器文件（*.pem / *.key / *.p12 / *.pfx）",
		test: (file) => /\.(?:pem|key|p12|pfx)$/i.test(file),
	},
	{
		id: "FILENAME_SSH_KEY",
		label: "OpenSSH 私钥文件（id_rsa / id_ed25519 …）",
		test: (file) => /(?:^|\/)id_(?:rsa|dsa|ecdsa|ed25519)[^/]*$/i.test(file),
	},
	{
		id: "FILENAME_SECRET_NAME",
		label: "文件名带 _secret",
		test: (file) => /_secret/i.test(path.posix.basename(file)),
	},
];

/* ---------- 例外表（**过期即红**，照 openapi_parity_test.go 的纪律） ----------
 * PATH_EXEMPTIONS：只豁免**文件名判据**（模板文件本来就该叫 .env.example）。
 *   内容判据照常跑 —— 模板里粘了真值一样红。每条匹配数必须 ≥1，否则 EXPIRED。
 * ALLOWLIST：结构上像密钥、其实是我方夹具的原文片段。必须同时写「路径 glob」与
 *   「匹配到的原文片段」；片段只在 glob 内的文件上生效（glob 之外同片段仍红）。
 *   每条必须命中 ≥1 次，否则 EXPIRED_ALLOWLIST。宽放行（`.pi-orchestrator/**` 整目录
 *   或 `*` 片段）是禁止的：宁可为一条新夹具补一条带理由的登记。 */
// self-test 用的假令牌在运行时拼出来：本文件自己也在扫描范围内，源码里留字面量会被
// 自己的 TOKEN_LITERAL / SLACK_TOKEN 判据命中（首次提交后立刻自证了这一点）。
const FAKE_TOKEN = "fixtureTokenAb12cd34" + "0".repeat(24);
const FAKE_SLACK = "xox" + "b-" + "1".repeat(24);

export const PATH_EXEMPTIONS = [
	{
		id: "env-example",
		glob: ".env.example",
		reason: "部署模板：值是 YOUR_VAPID_PRIVATE_KEY_HERE 之类占位符；真实 .env 已在 .gitignore",
	},
	{
		id: "env-suffix-example",
		glob: ".env.*.example",
		reason: ".env.dev.example / .env.compose.example 同上（模板，值为占位符）",
	},
];

export const ALLOWLIST = [
	{
		id: "docs-node-token-placeholder",
		glob: "docs/**",
		codes: ["TOKEN_LITERAL"],
		fragment: "REPLACE_WITH_EXISTING_NODE_IDENTITY_TOKEN",
		reason: "docs/anode-install.md:312 的示例配置：全大写 REPLACE_WITH_… 是给人看的占位符，不是令牌",
	},
	{
		id: "e2e-fixture-source-token-sentinel",
		glob: "scripts/e2e/fixtures/**",
		codes: ["TOKEN_LITERAL"],
		fragment: "different-browser-token-sentinel",
		reason: "scripts/e2e/fixtures/f3/f3-playbook.mjs:173 的内存 fixture 哨兵：断言「浏览器传来的令牌不被采用」，值本身无意义",
	},
	{
		id: "frontend-test-token-sentinel",
		glob: "scripts/v4-frontend.test.mjs",
		codes: ["TOKEN_LITERAL"],
		fragment: "explicit-different-secret-sentinel",
		reason: "v4-frontend.test.mjs:3886 的内存 fixture 哨兵：同样只用于断言「显式传入的不同值被保留」",
	},
];

/* ---------- 反假绿下限（实测值，不许改成 0） ----------
 * 实测出处：分支 guards/lint-batch 上的工作区，实测命令
 *   `node scripts/check-secrets.mjs`（默认模式）+ `git ls-files | wc -l`
 * 实测：`git ls-files` 992 个被跟踪文件；跳过二进制扩展名 15（11 个 woff2 + 4 个 png），
 *   0 个 >2MB（最大 371KB：scripts/v4-frontend.test.mjs），0 个含 NUL，0 个读不到
 *   → 实际读取内容并扫描 977 个文件。
 * 检测器 17 条（13 条内容判据 + 4 条文件名判据）。下限取「明显低于实测、远高于 0」的整数：
 * 文件数 977 → 900（约 92%）；检测器 17 → 15。文件数掉到下限以下 = git 读坏了/跳过
 * 规则写宽了；检测器数掉到下限以下 = 有人删判据没同步改这里的棘轮。两者都必须红。
 * （真遇到仓库大幅缩水要同步下调时，在 PR 里写清原因 —— 别默默改小。）
 *
 * 2026-11-19 下调（仓库真的缩了，不是跳过规则变宽）：`.pi-orchestrator/` 从版本里摘掉
 *   （协调者工作区不入库，见 PR #443 + docs/reports/orchestrator-untrack.md），被跟踪文件
 *   992 → 814，实测扫描 977 → 799（差额全是被摘掉的那批协调者文档，它们从来不是代码输入）。
 *   按同一个 92% 比例重算 799 → 735，跳过规则和检测器一条没动。 */
export const FILES_MIN = 735;
export const DETECTOR_MIN = 15;

/* ---------- 纯函数：占位符 / glob / 行匹配 ---------- */

/** 占位符**语法**与占位符**词**：这类值不是秘密（见头注释「反误报」）。 */
export function isPlaceholder(value) {
	const v = String(value ?? "").trim();
	if (v.length === 0) return true;
	// 语法：<token> / ${VAR} / $(cmd) / {{x}} / [redacted]
	if (/[<>${}()[\]]/.test(v)) return true;
	// 掩码：xxxxxx / ****** / ---- / ……（同一字符重复 ≥4 次）
	if (/^(.)\1{3,}$/u.test(v)) return true;
	// 词：真实密钥是随机串，不会拼出这些词
	if (/(?:placeholder|redacted|change[_-]?me|replace[_-]?me|your[_-]|dummy|example)/i.test(v)) {
		return true;
	}
	return false;
}

/** 极简 glob → RegExp：`**` 跨目录，`*` 不跨 `/`，`?` 单字符。 */
export function globToRegExp(glob) {
	let out = "";
	for (let i = 0; i < glob.length; i++) {
		const ch = glob[i];
		if (ch === "*") {
			if (glob[i + 1] === "*") {
				i++;
				if (glob[i + 1] === "/") {
					i++;
					out += "(?:[^/]+/)*";
				} else {
					out += ".*";
				}
			} else {
				out += "[^/]*";
			}
		} else if (ch === "?") {
			out += "[^/]";
		} else {
			out += ch.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
		}
	}
	return new RegExp(`^${out}$`);
}

const globCache = new Map();
/** 路径是否匹配 glob（相对仓库根、正斜杠）。 */
export function matchGlob(glob, file) {
	if (!globCache.has(glob)) globCache.set(glob, globToRegExp(glob));
	return globCache.get(glob).test(file);
}

/** 把行里的命中点收集出来（纯函数，供 analyzeScan 与自测共用）。 */
export function matchDetector(det, lines) {
	const hits = [];
	for (let i = 0; i < lines.length; i++) {
		const line = lines[i];
		if (det.needle) {
			const hay = det.needleCi ? line.toLowerCase() : line;
			if (!hay.includes(det.needleCi ? det.needle.toLowerCase() : det.needle)) continue;
		}
		det.re.lastIndex = 0;
		let m;
		while ((m = det.re.exec(line)) !== null) {
			const value = m[det.group ?? 0] ?? m[0];
			if (det.placeholderGuard && isPlaceholder(value)) continue;
			hits.push({ line: i + 1, text: value, lineText: line });
			if (m[0].length === 0) det.re.lastIndex++; // 防空匹配死循环
		}
	}
	return hits;
}

/** 报告里的一行原文（压平 + 截断）。 */
function oneLine(text, max = 120) {
	const flat = String(text).replace(/\s+/g, " ").trim();
	return flat.length > max ? `${flat.slice(0, max)}…` : flat;
}

/** 统一格式：`文件:行 [规则] 原文`；文件名判据没有行号，就标注一下（不然看起来像丢了行号）。 */
export function formatHit(h) {
	if (h.line > 0) return `${h.file}:${h.line} [${h.code}] ${oneLine(h.text)}`;
	return `${h.file}（文件名判据）[${h.code}]`;
}

/* ---------- 纯函数：扫描判定 ---------- */

/**
 * 纯函数：对「路径清单 + 可读文本」跑全部判据。
 * @param {{
 *   paths?: string[],                       // 全部被跟踪/暂存路径（文件名判据用）
 *   sources?: {file: string, text: string}[], // 已读到的内容（内容判据用）
 *   allowlist?: object[], pathExemptions?: object[],
 *   fileMin?: number, detectorMin?: number,
 *   expiryCheck?: boolean,                   // `--staged` 模式传 false（见下）
 * }} input
 * @returns {{stats: object, hits: object[], allowed: object[], expired: object[], violations: object[]}}
 */
export function analyzeScan(input = {}) {
	const paths = input.paths ?? [];
	const sources = input.sources ?? [];
	const allowlist = input.allowlist ?? ALLOWLIST;
	const pathExemptions = input.pathExemptions ?? PATH_EXEMPTIONS;
	const fileMin = input.fileMin === undefined ? FILES_MIN : input.fileMin; // null = 不适用（--staged）
	const detectorMin = input.detectorMin ?? DETECTOR_MIN;
	// 过期判定只能在「全量扫描」里判：`--staged` 只扫暂存的那几个文件，登记表里的条目
	// 当然一条都不命中 —— 若照旧判过期，每一次 pre-commit 都会对 5 条登记叫红，
	// 检查立刻变成噪声，人就开始 `--no-verify`。所以 staged 模式关掉过期判定（并在输出里说明）。
	const expiryCheck = input.expiryCheck ?? true;

	const rawHits = [];
	const exempted = [];
	const usage = new Map(allowlist.map((e) => [e.id, 0]));
	const exemptUsage = new Map(pathExemptions.map((e) => [e.id, 0]));

	// ① 文件名判据：对全部路径跑（含后面会因二进制/大小被跳过内容的文件）
	for (const file of paths) {
		for (const det of FILENAME_DETECTORS) {
			if (!det.test(file)) continue;
			const ex = pathExemptions.find((e) => matchGlob(e.glob, file));
			if (ex) {
				exemptUsage.set(ex.id, (exemptUsage.get(ex.id) ?? 0) + 1);
				exempted.push({ file, code: det.id, exemption: ex });
				continue;
			}
			rawHits.push({ file, line: 0, code: det.id, text: file, lineText: file });
		}
	}

	// ② 内容判据
	for (const src of sources) {
		const lines = src.text.split("\n");
		for (const det of CONTENT_DETECTORS) {
			if (det.appliesTo && !det.appliesTo(src.file)) continue;
			for (const h of matchDetector(det, lines)) {
				rawHits.push({ file: src.file, line: h.line, code: det.id, text: h.text, lineText: h.lineText });
			}
		}
	}

	// ③ 允许名单：路径 glob + 原文片段 + 规则码都对上才算放行（首条命中者计数）
	const hits = [];
	const allowed = [];
	for (const h of rawHits) {
		const entry = allowlist.find(
			(e) =>
				(!e.codes || e.codes.includes(h.code)) &&
				matchGlob(e.glob, h.file) &&
				String(h.text).includes(e.fragment),
		);
		if (entry) {
			usage.set(entry.id, (usage.get(entry.id) ?? 0) + 1);
			allowed.push({ hit: h, entry });
		} else {
			hits.push(h);
		}
	}

	// ④ 例外表过期（不再命中任何点 ⇒ 必须删掉，防止例外变永久后门）
	const expired = [];
	if (expiryCheck) {
		for (const e of allowlist) {
			if ((usage.get(e.id) ?? 0) === 0) expired.push({ kind: "allowlist", entry: e });
		}
		for (const e of pathExemptions) {
			if ((exemptUsage.get(e.id) ?? 0) === 0) expired.push({ kind: "pathExemption", entry: e });
		}
	}

	const detectors = CONTENT_DETECTORS.length + FILENAME_DETECTORS.length;
	const violations = [];
	for (const h of hits) {
		violations.push({
			...h,
			kind: "hit",
			message:
				h.code.startsWith("FILENAME_")
					? "这个文件被跟踪了，而它按名字就是秘密容器 —— 从索引里移除（git rm --cached），确认真值没进过历史"
					: "这一行有密钥形状的原文 —— 改成占位符/环境变量；**已经 push 过的必须轮换令牌**，改代码不算处置",
		});
	}
	for (const x of expired) {
		violations.push({
			kind: "expired",
			code: x.kind === "allowlist" ? "EXPIRED_ALLOWLIST" : "EXPIRED_PATH_EXEMPTION",
			file: x.kind === "allowlist" ? "（ALLOWLIST）" : "（PATH_EXEMPTIONS）",
			line: 0,
			text: `${x.entry.id} (${x.entry.glob})`,
			message:
				"这条例外已经不再命中任何点（片段改了/文件删了）—— 例外表过期即红，请删掉这条登记，别让它变成永久后门",
		});
	}
	// 反假绿下限：0 个路径 = 真的什么都没查；路径有但内容全被跳过时，只有全量模式才判下限
	// （`--staged` 传 fileMin:null：只暂存了一张 png 也是合法提交，不该报红）。
	if (paths.length === 0) {
		violations.push({
			kind: "floor",
			code: "NO_FILES_SCANNED",
			file: "（总计）",
			line: 0,
			text: "待扫描文件 0 个",
			message: "一个文件都没扫到 —— 判据在空转，「绿」不能和「没检查」同形",
		});
	} else if (fileMin !== null && !(sources.length > fileMin)) {
		violations.push({
			kind: "floor",
			code: "FILES_BELOW_MIN",
			file: "（总计）",
			line: 0,
			text: `扫描文件 ${sources.length} ≤ 下限 ${fileMin}`,
			message: "扫描文件数没超过实测下限 —— git 读坏了或跳过规则写宽了；确认是仓库缩了再同步下调 FILES_MIN",
		});
	}
	if (!(detectors > detectorMin)) {
		violations.push({
			kind: "floor",
			code: "DETECTORS_BELOW_MIN",
			file: "（总计）",
			line: 0,
			text: `检测器 ${detectors} ≤ 下限 ${detectorMin}`,
			message: "检测器数量掉到下限以下 —— 有人删了判据没同步棘轮（DETECTOR_MIN）",
		});
	}

	return {
		stats: {
			paths: paths.length,
			files: sources.length,
			detectors,
			hits: hits.length,
			allowed: allowed.length,
			exempted: exempted.length,
			allowlist: allowlist.length,
			pathExemptions: pathExemptions.length,
		},
		hits,
		allowed,
		expired,
		exempted,
		usage,
		exemptUsage,
		violations,
	};
}

/* ---------- 收集（读取文件，含跳过计数） ---------- */

/** 前 8KB 有 NUL 就当二进制。 */
function looksBinary(buf) {
	const n = Math.min(buf.length, 8192);
	for (let i = 0; i < n; i++) if (buf[i] === 0) return true;
	return false;
}

/**
 * 读内容并分类跳过。返回 { sources, skipped }，skipped 按原因计数（**不静默**）。
 * @param {{root: string, paths: string[], readBuffer: (file: string) => Promise<Buffer>}} input
 */
export async function collectSources({ root, paths, readBuffer }) {
	const skipped = { dirs: 0, binaryExt: 0, tooBig: 0, binaryContent: 0, unreadable: 0 };
	const sources = [];
	for (const file of paths) {
		if (SKIP_DIRS.some((d) => file.startsWith(d))) {
			skipped.dirs++;
			continue;
		}
		if (BINARY_EXT.has(path.extname(file).toLowerCase())) {
			skipped.binaryExt++;
			continue;
		}
		let buf;
		try {
			buf = await readBuffer(file);
		} catch {
			skipped.unreadable++;
			continue;
		}
		if (buf.length > MAX_BYTES) {
			skipped.tooBig++;
			continue;
		}
		if (looksBinary(buf)) {
			skipped.binaryContent++;
			continue;
		}
		sources.push({ file, text: buf.toString("utf8") });
	}
	return { sources, skipped };
}

/** 读工作区内容（默认模式）。 */
export function worktreeReader(root) {
	return (file) => fs.readFile(path.join(root, file));
}

/** 读**索引**内容（`--staged` 模式：真正要提交的字节，不是工作区）。 */
export function indexReader(root) {
	return async (file) => execFileSync("git", ["show", `:${file}`], { cwd: root, maxBuffer: MAX_BYTES * 4 });
}

/* ---------- git 交互 ---------- */

function runGit(args, root = ROOT) {
	try {
		return execFileSync("git", args, { cwd: root, maxBuffer: 1 << 28, encoding: "utf8" });
	} catch (e) {
		const detail = String(e?.stderr ?? e?.message ?? e).trim();
		throw new Error(`git ${args.join(" ")} 失败：${detail}`);
	}
}

/** 全部被跟踪文件（相对仓库根）。 */
export function listTracked(root = ROOT) {
	return runGit(["ls-files", "-z"], root).split("\0").filter(Boolean);
}

/** 暂存改动里新增/修改/改名的文件（排除删除）。 */
export function listStaged(root = ROOT) {
	return runGit(["diff", "--cached", "--name-only", "-z", "--diff-filter=ACMR"], root)
		.split("\0")
		.filter(Boolean);
}

/**
 * 纯函数：`--staged` 在「没有暂存内容」时必须绿（不是失败，也绝不静默）。
 * @returns {{done: true, exitCode: 0, message: string} | {done: false, paths: string[]}}
 */
export function stagedDecision(paths) {
	if (!Array.isArray(paths)) return { done: false, paths: [] };
	if (paths.length === 0) return { done: true, exitCode: 0, message: "无暂存改动" };
	return { done: false, paths };
}

/* ---------- 输出 ---------- */

function describeSkipped(skipped) {
	const parts = [];
	if (skipped.dirs) parts.push(`跳过目录 ${skipped.dirs}`);
	if (skipped.binaryExt) parts.push(`二进制扩展名 ${skipped.binaryExt}`);
	if (skipped.tooBig) parts.push(`>2MB ${skipped.tooBig}`);
	if (skipped.binaryContent) parts.push(`含 NUL ${skipped.binaryContent}`);
	if (skipped.unreadable) parts.push(`读不到 ${skipped.unreadable}`);
	return parts.length > 0 ? parts.join(" / ") : "无";
}

function printAllowed(res) {
	if (res.allowed.length === 0) return;
	console.log("  放行明细（ALLOWLIST 登记命中的原文片段）：");
	for (const { hit, entry } of res.allowed) {
		console.log(`    ${formatHit(hit)}  ← ${entry.id}`);
	}
	const stale = [...res.usage.entries()].filter(([, n]) => n === 0).map(([id]) => id);
	if (stale.length > 0) console.log(`    （未命中的登记：${stale.join(", ")} —— 已按过期处理）`);
}

/* ---------- 自测：证明「错的判红、对的判绿」（纯内存，不改仓库文件） ---------- */
function selfTest() {
	const results = [];
	const push = (name, why, problem) => {
		results.push({ name, why, problem });
	};

	/** 跑一个 fixture 用例并断言。want.codes = violations 的规则码集合；want.allowed = 放行条数。 */
	const run = (name, why, c) => {
		let res;
		try {
			res = analyzeScan({
				paths: c.paths ?? (c.sources ?? []).map((s) => s.file),
				sources: c.sources ?? [],
				// fixture 用例默认不背仓库的两张例外表：显式传才生效，免得「真实登记的过期」
				// 混进合成用例的违规码里，把断言变成噪声。
				allowlist: c.allowlist ?? [],
				pathExemptions: c.pathExemptions ?? [],
				fileMin: Object.hasOwn(c, "fileMin") ? c.fileMin : 0, // 显式 null = 不适用（--staged）
				detectorMin: c.detectorMin,
				expiryCheck: c.expiryCheck ?? true,
			});
		} catch (e) {
			push(name, why, `analyzeScan 抛异常：${e?.stack ?? e}`);
			return;
		}
		const codes = [...new Set(res.violations.map((v) => v.code))].sort();
		const want = [...(c.want?.codes ?? [])].sort();
		const wantAllowed = c.want?.allowed;
		let problem = null;
		if (JSON.stringify(codes) !== JSON.stringify(want)) {
			problem = `违规码期望 [${want.join(", ") || "无"}]，实际 [${codes.join(", ") || "无"}]`;
		} else if (wantAllowed !== undefined && res.allowed.length !== wantAllowed) {
			problem = `放行条数期望 ${wantAllowed}，实际 ${res.allowed.length}`;
		} else if (c.want?.hits !== undefined && res.hits.length !== c.want.hits) {
			problem = `命中数期望 ${c.want.hits}，实际 ${res.hits.length}`;
		} else if (c.want?.line !== undefined && res.hits[0]?.line !== c.want.line) {
			problem = `首个命中的行号期望 ${c.want.line}，实际 ${res.hits[0]?.line}`;
		}
		push(name, why, problem);
	};

	// ① PEM 私钥块 ⇒ 必须红（含 BEGIN PRIVATE KEY / BEGIN EC PRIVATE KEY 两种）
	run("PEM 私钥块", "必须红：私钥落在仓库里就是事故", {
		sources: [
			{
				file: "cmd/anode/keys.go",
				text: ["package main", "\t-----BEGIN " + "EC PRIVATE KEY-----", "\t" + "M".repeat(64), "\t-----END " + "EC PRIVATE KEY-----"].join("\n"),
			},
		],
		want: { codes: ["PEM_PRIVATE_KEY"], hits: 1, line: 2 },
	});

	// ② GitHub PAT ⇒ 必须红
	run("ghp_ 经典 PAT", "必须红：供应商形状是高信号", {
		sources: [{ file: "docs/example.md", text: 'AUTH="-H Authorization: $(ghp_' + "a".repeat(36) + ')"' }],
		want: { codes: ["GITHUB_PAT"], hits: 1 },
	});

	// ③ AWS Access Key ID ⇒ 必须红
	run("AKIA 形状", "必须红：AWS key id 是高信号", {
		sources: [{ file: "scripts/deploy.sh", text: "export AWS_ACCESS_KEY_ID=" + "AKIA" + "Q".repeat(16) }],
		want: { codes: ["AWS_ACCESS_KEY_ID"], hits: 1 },
	});

	// ④ 夹具令牌 ⇒ 必须绿，且必须走 ALLOWLIST 点名的路径 glob + 片段
	run("夹具令牌 fixtureToken", "必须绿：夹具令牌靠 ALLOWLIST（路径 glob + 原文片段）放行，不是靠判据放水", {
		sources: [
			{
				file: "cmd/anode/fixtures/bootstrap.yaml",
				text: `token = "${FAKE_TOKEN}"`,
			},
		],
		allowlist: [
			{
				id: "test-fixture-token",
				glob: "cmd/anode/fixtures/**",
				codes: ["TOKEN_LITERAL"],
				fragment: "fixtureTokenAb12cd34",
				reason: "自测夹具",
			},
		],
		pathExemptions: [],
		want: { codes: [], allowed: 1 },
	});

	// ⑤ sha256 摘要与镜像 digest ⇒ 必须绿（不做熵检测的直接理由）
	run("sha256 摘要 / 镜像 digest", "必须绿：本仓 80 处 64 位 hex，熵规则必然误报", {
		sources: [
			{
				file: "scripts/perf/3a/raw/provenance.txt",
				text: [
					"sha256=" + "3f3e041b".repeat(8),
					"image: ghcr.io/zzjcool/anotify@sha256:" + "0123456789abcdef".repeat(4),
					"digest fdba2013" + "9".repeat(56),
				].join("\n"),
			},
		],
		want: { codes: [] },
	});

	// ⑥ 占位符 ⇒ 必须绿（<token> / ${VAR} / xxx 掩码 / YOUR_…）
	run("占位符与掩码", "必须绿：占位符不是秘密，判据层直接放行", {
		sources: [
			{
				file: "docs/anode-install.md",
				text: [
					'token: "<token>"',
					"token: ${ANOTIFY_NODE_TOKEN}",
					"ANOTIFY_NODE_TOKEN=" + "x".repeat(40),
					"ANOTIFY_VAPID_PRIVATE_KEY=YOUR_VAPID_PRIVATE_KEY_HERE",
					"Bearer <node token>",
					"token: $(cat /run/secrets/token)",
				].join("\n"),
			},
		],
		want: { codes: [] },
	});

	// ⑦ 被跟踪的 .env ⇒ 必须红（文件名判据，与内容无关）
	run("被跟踪的 .env", "必须红：.env 被跟踪就是事故，内容再干净也一样", {
		sources: [{ file: ".env", text: "ANOTIFY_DB=./anotify.db" }],
		want: { codes: ["FILENAME_DOTENV"], hits: 1 },
	});

	// ⑧ .env.example ⇒ 必须绿（路径豁免），但豁免只针对文件名判据
	run(".env.example 模板", "必须绿：模板文件走 PATH_EXEMPTIONS（只豁免文件名判据）", {
		sources: [{ file: ".env.example", text: "ANOTIFY_VAPID_PRIVATE_KEY=YOUR_VAPID_PRIVATE_KEY_HERE" }],
		// 用**真实**登记里的那一条（证明 .env.example 的 glob 真的能呑住模板文件）
		pathExemptions: PATH_EXEMPTIONS.filter((e) => e.id === "env-example"),
		want: { codes: [], allowed: 0 },
	});

	// ⑨ --staged 在无暂存内容时 ⇒ 必须绿（打印「无暂存改动」，不是静默通过）
	{
		const empty = stagedDecision([]);
		const some = stagedDecision(["web/app.js"]);
		const problem =
			empty.done === true && empty.exitCode === 0 && empty.message === "无暂存改动" && some.done === false
				? null
				: `期望空暂存 ⇒ {done:true, exitCode:0, message:"无暂存改动"}、有暂存 ⇒ done:false，实际 ${JSON.stringify({ empty, some })}`;
		push("--staged 无暂存改动", "必须绿：pre-commit 在没有暂存内容时不该报错，也不该假装扫过", problem);
	}

	// ⑩ ALLOWLIST 过期条目 ⇒ 必须红
	run("ALLOWLIST 过期条目", "必须红：不再命中任何点的登记必须删掉（过期即红）", {
		sources: [{ file: "web/app.js", text: "const x = 1;" }],
		allowlist: [
			{
				id: "stale-entry",
				glob: "web/**",
				fragment: "this-fragment-is-gone",
				reason: "自测：故意过期的登记",
			},
		],
		pathExemptions: [],
		want: { codes: ["EXPIRED_ALLOWLIST"] },
	});

	// ⑪ 扫描 0 文件 ⇒ 必须红
	run("扫描 0 文件", "必须红：否则「没检查」与「通过」同形", {
		paths: [],
		sources: [],
		pathExemptions: [],
		allowlist: [],
		want: { codes: ["NO_FILES_SCANNED"] },
	});

	// ⑫ 7 条供应商形状全覆盖 ⇒ 必须红 7 条（证明没有死正则）
	run("供应商形状全覆盖", "必须红：7 条供应商前缀一条都不能是死正则", {
		sources: [
			{
				file: "scripts/x.sh",
				text: [
					"ghp_" + "b".repeat(36),
					"github_pat_" + "c".repeat(60),
					"AKIA" + "D".repeat(16),
					FAKE_SLACK,
					"sk-" + "e".repeat(32),
					"sk_live_" + "f".repeat(20),
					"AIza" + "g".repeat(35),
				].join("\n"),
			},
		],
		want: {
			codes: [
				"AWS_ACCESS_KEY_ID",
				"GITHUB_PAT",
				"GITHUB_PAT_FINE",
				"GOOGLE_API_KEY",
				"OPENAI_KEY",
				"SLACK_TOKEN",
				"STRIPE_LIVE_KEY",
			],
			hits: 7,
		},
	});

	// ⑬ 真值形态 ⇒ 必须红（证明占位符过滤器没把判据吞掉）
	run("真值形态（token 字面量 / VAPID 私钥 / JSON privateKey）", "必须红：占位符过滤器不能把真值也放过去", {
		sources: [
			{
				file: "cmd/anode/config.go",
				text: [
					`\ttoken = "` + "Zm9vYmFyQmF6UXV1eENvcmdlR3JhdWx0R2FycGx5SGF1bHQ" + `"`,
					`ANOTIFY_VAPID_PRIVATE_KEY=` + "kQ7rT2xLp9ZaB4nR8sV1yD3fH6jM0cW5uE2iO7gK4tA",
					`  "privateKey": "` + "q_9-zA1bC2dE3fG4hJ5kL6mN7pQ8rS9tU0vW1xY2z",
				].join("\n"),
			},
		],
		want: { codes: ["TOKEN_LITERAL", "VAPID_PRIVATE_VALUE"], hits: 3 },
	});

	// ⑭ 路径豁免过期 ⇒ 必须红
	run("PATH_EXEMPTIONS 过期", "必须红：豁免登记也要过期即红", {
		sources: [{ file: "web/app.js", text: "const x = 1;" }],
		allowlist: [],
		pathExemptions: [{ id: "stale-exempt", glob: ".env.example", reason: "自测：没有文件的豁免" }],
		want: { codes: ["EXPIRED_PATH_EXEMPTION"] },
	});

	// ⑮ glob 之外的同片段 ⇒ 必须红（证明 allowlist 的路径 glob 真的参与匹配）
	run("ALLOWLIST glob 之外同片段", "必须红：片段一样但路径不在登记 glob 内，照红（防「一条登记管全仓」）", {
		sources: [
			{ file: "cmd/anode/fixtures/x.yaml", text: `token = "${FAKE_TOKEN}"` },
			{ file: ".env", text: `token = "${FAKE_TOKEN}"` },
		],
		allowlist: [
			{
				id: "only-fixtures",
				glob: "cmd/anode/fixtures/**",
				codes: ["TOKEN_LITERAL"],
				fragment: "fixtureTokenAb12cd34",
				reason: "自测：只覆盖 fixtures 目录",
			},
		],
		pathExemptions: [],
		want: { codes: ["FILENAME_DOTENV", "TOKEN_LITERAL"], hits: 2, allowed: 1 },
	});

	// ⑯ 检测器下限棘轮本身要能红（不然「删了判据」也能绿）
	run("检测器数低于下限", "必须红：删除判据没同步棘轮", {
		sources: [{ file: "a.txt", text: "hello" }],
		allowlist: [],
		pathExemptions: [],
		detectorMin: CONTENT_DETECTORS.length + FILENAME_DETECTORS.length,
		want: { codes: ["DETECTORS_BELOW_MIN"] },
	});

	// ⑰ --staged 关掉过期判定 ⇒ 必须绿（否则每次 pre-commit 都对 5 条登记叫红，检查变噪声）
	run("--staged 不判例外表过期", "必须绿：只暂存 1 个文件时，全仓的登记必然一条都不命中", {
		sources: [{ file: "web/app.js", text: "const x = 1;" }],
		allowlist: [
			{ id: "whole-repo-entry", glob: "docs/**", fragment: "REPLACE_WITH_EXISTING", reason: "自测" },
		],
		pathExemptions: [{ id: "whole-repo-exempt", glob: ".env.example", reason: "自测" }],
		expiryCheck: false,
		want: { codes: [] },
	});

	// ⑱ 4 条文件名判据都不能是死规则（install.json 靠内容、其余靠名字）
	run("文件名判据全覆盖", "必须红：*.pem / id_ed25519 / *_secret / install.json+token 四条都得有牙", {
		paths: ["keys/server.pem", ".ssh/id_ed25519", "config/app_secret.yaml", "node-data/install.json"],
		sources: [{ file: "node-data/install.json", text: '{"server":"http://x","token":"{{API_KEY_p2tgm2pu}}"}' }],
		want: {
			codes: ["FILENAME_PRIVATE_KEY_FILE", "FILENAME_SECRET_NAME", "FILENAME_SSH_KEY", "INSTALL_JSON_TOKEN"],
			hits: 4,
		},
	});

	// ⑲ --staged 只暂存了二进制（内容全被跳过）⇒ 必须绿（合法提交，不能报假红）
	run("--staged 只暂存二进制", "必须绿：paths 非空、内容全跳过，不是「什么都没查」", {
		paths: ["web/icons/x.png"],
		sources: [],
		fileMin: null,
		want: { codes: [] },
	});

	let failed = 0;
	for (const r of results) {
		if (r.problem) {
			console.error(`  ❌ ${r.name}\n     ${r.why}\n     ${r.problem}`);
			failed++;
		} else {
			console.log(`  ✅ ${r.name} —— ${r.why}`);
		}
	}
	console.log(
		failed === 0
			? `✅ self-test ${results.length}/${results.length} 通过（红/绿双侧自证）`
			: `✗ self-test ${results.length - failed}/${results.length} 通过`,
	);
	return failed === 0;
}

/* ---------- 主流程 ---------- */
async function main(argv) {
	const args = argv.slice();
	let staged = false;
	const unknown = [];
	for (const a of args) {
		if (a === "--staged") staged = true;
		else unknown.push(a);
	}
	if (unknown.length > 0) {
		console.error(`✗ 不认识的参数：${unknown.join(" ")}`);
		console.error("  用法：node scripts/check-secrets.mjs [--staged | --self-test]");
		return 2;
	}

	let paths;
	try {
		paths = staged ? listStaged(ROOT) : listTracked(ROOT);
	} catch (e) {
		console.error(`✗ 读取 git 文件清单失败（在 ${ROOT} 下）：${e.message}`);
		console.error("  这不是通过：请确认当前目录是 git 仓库、git 可用。");
		return 2;
	}

	if (staged) {
		const decision = stagedDecision(paths);
		if (decision.done) {
			console.log(`✅ ${decision.message}：跳过秘密扫描（--staged 模式）`);
			return 0;
		}
		paths = decision.paths;
	} else if (paths.length === 0) {
		console.error(`✗ git ls-files 返回 0 个文件（在 ${ROOT} 下）—— 空仓库/不是 git 仓库是环境问题，不是通过`);
		return 2;
	}

	// 文件名判据对全部路径跑；内容按需读取
	let collected;
	try {
		collected = await collectSources({ root: ROOT, paths, readBuffer: staged ? indexReader(ROOT) : worktreeReader(ROOT) });
	} catch (e) {
		console.error(`✗ 读取文件内容失败：${e?.message ?? e}`);
		return 2;
	}

	const res = analyzeScan({
		paths,
		sources: collected.sources,
		fileMin: staged ? null : FILES_MIN,
		// 过期判定只在全量模式做：staged 只扫几个文件，登记表必然一条都不命中（见 analyzeScan 注释）
		expiryCheck: !staged,
	});
	const label = staged ? "秘密扫描（--staged）通过：" : "秘密扫描通过：";

	if (res.violations.length === 0) {
		console.log(`✅ ${label}扫描 ${res.stats.files} 文件 / 检测器 ${res.stats.detectors} / 命中 0 / 放行 ${res.stats.allowed}`);
		console.log(`  · 被跟踪 ${res.stats.paths} 文件；跳过：${describeSkipped(collected.skipped)}`);
		console.log(
			staged
				? `  · 例外表：ALLOWLIST ${res.stats.allowlist} 条 / PATH_EXEMPTIONS ${res.stats.pathExemptions} 条（staged 模式不判过期，见下）`
				: `  · 例外表：ALLOWLIST ${res.stats.allowlist} 条全部命中 / PATH_EXEMPTIONS ${res.stats.pathExemptions} 条（豁免 ${res.stats.exempted} 个模板文件）`,
		);
		printAllowed(res);
		console.log(
			`  · 下限：文件 ${staged ? "不适用（--staged）" : `> ${FILES_MIN}（实测 977，出处见 FILES_MIN 注释）`}、检测器 > ${DETECTOR_MIN}`,
		);
		if (staged) console.log("  · 注：--staged 只扫暂存内容，例外表过期判定在默认（全量）模式里做");
		return 0;
	}

	const hits = res.violations.filter((v) => v.kind === "hit");
	const expired = res.violations.filter((v) => v.kind === "expired");
	const floors = res.violations.filter((v) => v.kind === "floor");

	console.error(
		`✗ ${staged ? "秘密扫描（--staged）" : "秘密扫描"}失败：命中 ${hits.length} 处 / 例外表过期 ${expired.length} 条 / 反假绿 ${floors.length} 条`,
	);
	console.error(
		`  扫描 ${res.stats.files} 文件（被跟踪 ${res.stats.paths}）/ 检测器 ${res.stats.detectors} / 放行 ${res.stats.allowed}`,
	);
	console.error(`  跳过：${describeSkipped(collected.skipped)}`);

	if (hits.length > 0) {
		console.error("\n  疑似秘密（文件:行）：");
		hits.sort((a, b) => (a.file === b.file ? a.line - b.line : a.file < b.file ? -1 : 1));
		for (const h of hits) {
			console.error(`    ${formatHit(h)}`);
			console.error(`      → ${h.message}`);
			if (h.lineText) console.error(`      原文：${oneLine(h.lineText, 160)}`);
		}
	}
	if (expired.length > 0) {
		console.error("\n  例外的例外（过期登记，必须删）：");
		for (const v of expired) {
			console.error(`    ${v.code} ${v.text}`);
			console.error(`      → ${v.message}`);
		}
	}
	if (floors.length > 0) {
		console.error("\n  反假绿：");
		for (const v of floors) console.error(`    ${v.code} ${v.text} —— ${v.message}`);
	}

	printAllowed({ allowed: res.allowed, usage: res.usage });

	console.error("\n  处置建议：");
	console.error("    · 真命中：把值换成占位符/环境变量，从索引里移除秘密文件；**已经 push 过的必须轮换令牌**");
	console.error("    · 夹具/哨兵误报：往 ALLOWLIST 加一条「路径 glob + 原文片段 + 理由」，别放宽判据本身");
	console.error("    · 模板文件（.env.example 之类）：走 PATH_EXEMPTIONS，同样要写理由；过期不删会红");
	return 1;
}

const isDirectRun =
	process.argv[1] && path.resolve(process.argv[1]) === path.resolve(fileURLToPath(import.meta.url));
if (isDirectRun) {
	if (process.argv.includes("--self-test")) {
		process.exit(selfTest() ? 0 : 1);
	} else {
		main(process.argv.slice(2))
			.then((code) => process.exit(code))
			.catch((e) => {
				console.error(`✗ check-secrets 运行失败：${e?.stack ?? e}`);
				process.exit(2);
			});
	}
}
