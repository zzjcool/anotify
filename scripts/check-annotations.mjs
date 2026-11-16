#!/usr/bin/env node
/**
 * 注解即外键守卫（docs/lint-guards.md §2 的 L5 行 + §4「注解即外键」整节）
 *
 * ── 为什么需要它：注释没有执行者 ──────────────────────────────────────────
 * 本仓反复被「注释/文档当契约」反噬，四次都是同一形态 —— 注释里的承诺没人复算，
 * 于是随时间只能变错，而下一个人（尤其是 agent）对注释的信任度高于对代码的信任：
 *
 *   ① `api/auth.yaml:1400` 散文写「id 破平局」，实现早已改成 `rowid`。契约文本
 *      把修好的 bug **反向诱导回来**（commit 189a57f3）。
 *   ② `internal/.../resource_capability.go:716-719` 把契约缺口写成注释
 *      （「本域不查节点上报，所以不声称 stale」），`capabilityNodeStale` 常量全文
 *      只出现 1 次（声明本身），stale 永不可达 —— 缺口被注释合法化了（e16c2536 / #362）。
 *   ③ `#372` 文件头写「9 个宽度值 / 33 条内联」，实为 13/38，残留 `767` 注释 ——
 *      过期计数注释把人往旧值上拽（30856f69）。
 *   ④ `DEVELOPMENT.md:720` 把零调用点、带 11 条基线红的 `scripts/v4-frontend.test.mjs`
 *      称作「回归测试」—— 任何依赖它的保护都是软的。
 *
 * 共同点不是「不小心」，而是**注解是自由文本**：它可以声称任何事，且不指向任何
 * 可判定的对象。所以本守卫的核心动作是 —— 把注解从散文降级成**外键**：
 *
 *   @guard    scripts/check-css.mjs#NESTED_RULE   ← 脚本必须存在，且真的会抛这个规则码
 *   @decision V4-D1                               ← 决策编号必须能在决策文档里找到
 *   @count    node scripts/check-contrast.mjs     ← 计数型断言必须附可重跑的命令
 *   frozen-doc: v3 设计快照，不再随代码演进        ← 仅 markdown 前 40 行；引用全部解析时必红
 *
 * 外键的引用完整性由本 lint 检查。被引用的脚本改规则名/被删时，锚点立刻红 ——
 * 而不是安静地变成谎。
 *
 * ── 四条规则 ────────────────────────────────────────────────────────────
 * A `REF_UNRESOLVED`（判红）· 引用完整性
 *    只检查**代码块（fenced / 缩进）与行内反引号**里的引用，普通散文一律不看
 *    （否则「make sure」/「make it」全误报 —— 见 --self-test ③⑪）。检查面：
 *      `scripts/<path>`            → 文件/目录必须存在（允许 `#规则码` / `:行号` 后缀）
 *      `make <target>`             → Makefile 里必须有 `^<target>:`（`.PHONY` 声明不影响）
 *      `npm run <script>`          → package.json 的 scripts 里必须有
 *      `node <path>` / `node --test <path>` → path 必须存在
 *      `go run|test|build|vet ./<pkg>`      → 去掉 `-run`/`-count` 等 flag 后目录必须存在
 *      `cmd/<x>`                   → 该路径必须存在（绝大多数是包目录，个别注释指向其中的文件）
 *    代码文件里的「代码块」= 注释里的行内反引号（字符串与普通代码不扫：那里不是注解）。
 *    **历史提及口子**（父会话裁决 2026-11-16）：同一行或紧邻上一行明说
 *    「删除 / 退役 / 下线 / 移除 / 废弃」（「已」可省，也覆盖「随旧节点 CLI 退役」的写法）
 *    或 remove(d)?/retire(d)? 时，**引用不存在**的目标算历史提及 —— 不判红、单独计数
 *    `historical: N` 并列入报告（例：Makefile 里「v3 的 demo-chain … 随旧节点 CLI 退役」
 *    就是历史提及，不该逼人删掉整段历史）。只对「引用不存在」生效：引用存在时照常算
 *    解析成功，不走这条分支。
 *
 * B `ANCHOR_UNRESOLVED`（判红）· 锚点完整性
 *    `@guard <path>#<ruleId>`：path 必须存在，且 ruleId 必须真能在该文件里找到
 *    （允许它出现在字符串字面量/导出常量/注释里的规则码表 —— 本仓的 `check-css.mjs`
 *    的 `BRACE_UNBALANCED` / `NESTED_RULE`、`check-i18n-keys.mjs` 的 `ref-missing-key` /
 *    `locale-drift`、`check-test-discipline.mjs` 的 `over` / `under` /
 *    `skip-unregistered` 都是这种形态）。找不到 ⇒ 红：引用方被改坏。
 *    `@decision <ID>`：ID 必须出现在决策文档里。探测顺序（见 `DECISION_DOC_CANDIDATES`）：
 *      ① `docs/design/v4/decisions.md`
 *      ② `.pi-orchestrator/v4/CONTRACT.md`
 *      ③ `docs/design/v4/*.md`（按文件名序）
 *    实测（2026-11-16）三级都存在，判决取**并集**、每条锚点报出命中的那一份；
 *    本次实测实际命中的是 ①（`V4-D1` 在其中）。`--report` 会打印探测顺序与实际存在的文件。
 *    `@count <command>`：只登记、不执行（打印清单），避免守卫自己变成命令执行面。
 *
 * C `CLAIM_UNBACKED`（棘轮，只许降）· 「声称有检查在兜底」必须有锚点
 *    **只抓「声称有检查/门禁在兜底」这一类**（父会话裁决 2026-11-16 收窄）：
 *    `门禁|lint|守卫|会红|会拦住|由…(保证|兜住)|已由|测试会|自动拦|enforced|guard|
 *    fails the build`。`必须/不得/不许/严禁/只能走/绝不能` **不在**表内 —— 它们是设计
 *    意图（属 `@decision` 面，不进 C）；Go 注释里「必须/不得」是常态，把它们算进来
 *    会得到 2032/2034 的命中率，等于没有基线。
 *    同行/上一行没有 `@guard` 的这类注释行计 1 条 `unbacked`。总数必须
 *    ≤ `CLAIM_UNBACKED_BASELINE`；**低于基线也红**（要求同步下调）。
 *
 * D `COUNT_UNMARKED`（棘轮，只许降）· 计数型断言必须有 `@count`
 *    **只扫契约面文件**（父会话裁决 2026-11-16 收窄，见 `inCountScope`）：`Makefile`、
 *    `scripts/**\/*.mjs|*.py`、`web-src/**`、`docs/**`、`DEVELOPMENT.md`、`AGENTS.md`、
 *    `E2E_TESTING.md`。Go 源码注释里的「64 个 0」「16MiB / 32 项」这类**局部常量**
 *    不算（它们是被测对象的数据，不是对外承诺的计数）。markdown 无注释语法，这些
 *    契约文档按「逐行散文」计入。
 *    出现 `\d+\s*(处|个|条|对|行|套件|断言|项)` 且同行/上一行没有 `@count` 的行，
 *    计 1 条 `unmarked`。棘轮同上。
 *
 * ── 反假绿（本仓教训：恒绿的门禁等于没有门禁） ────────────────────────
 *   1. 永远打印「扫描文件数 / 注释行数 / 引用数 / 锚点数 / unbacked / unmarked」，
 *      每一项都有下限（常量 + 实测出处写在下面注释里）。0 文件、0 引用直接红。
 *   2. `--report`：只盘点、不判红、exit 0（首次盘点用；`make ci` 用默认模式）。
 *   3. `--self-test`：纯内存 fixture，红/绿双侧自证 27 组（①～⑫ 是 docs/lint-guards.md
 *      §2 L5 行冻结的 12 条，㉔～㉗ 等为收窄判据后的加固用例）。
 *
 * ── 用法 ────────────────────────────────────────────────────────────────
 *   node scripts/check-annotations.mjs               # 体检（CI 用）；有 findings ⇒ exit 1
 *   node scripts/check-annotations.mjs --report      # 盘点：只列 findings，不判红，exit 0
 *   node scripts/check-annotations.mjs --self-test   # 守卫自身的红/绿自证
 *   node scripts/check-annotations.mjs --help
 *   退出码：0 = 通过 / 1 = 有 findings / 2 = 用法或环境问题（含「一个文件都没扫到」）
 *
 * ── 扫描范围 ────────────────────────────────────────────────────────────
 *   代码：web/**\/*.js、web-src/**\/*.html、internal/**\/*.go、scripts/**\/*.mjs、
 *         scripts/**\/*.py、cmd/**\/*.go、Makefile（见 CODE_SOURCES / EXTRA_CODE_FILES）
 *   文档：`git ls-files '*.md'`（只认被跟踪的文件 —— 未跟踪的草稿不是契约）
 *   排除：node_modules/ .git/ .pi/ .pi-subagents/ .pi-orchestrator/ 以及仓库根下的
 *         design/ stale/ missing/ _tmp_\* 系列 g2-resume-evidence/，还有 internal/server/dist/。
 *         注意 `design/` 只排除**根目录**那个（docs/design/ 必须扫：它装着决策文档与
 *         capability-spec-v3.md 这类历史契约）。
 *
 * ── 已知边界（有意不做的部分） ──────────────────────────────────────────
 *   · 不判断断言**是否成立**，只判断**注解指向的对象是否存在**（存在性 ≠ 正确性）。
 *     锚点绿只说明被引用方真的有这个规则码/决策号，不说明那条约束真的在代码里被强制了
 *     —— 这是外键语义的上限，也是刻意不做的部分。
 *   · 不做全文自然语言解析：散文里的「make sure」/「scripts/x」不算引用（否则误报
 *     会淹没真问题）。反过来说，散文里写了过期路径本守卫看不见 —— 那是 review 的事。
 *   · 引用的解析面只有 A 里列的六种形态；`docs/xxx.md`、`api/*.yaml` 这类互指不在
 *     范围内（文档互指没有稳定锚，且数量大、语义弱）。
 *   · 缩进代码块按 CommonMark 的「不能打断段落」规则识别（前一行必须为空行或同为
 *     缩进代码行）。列表项里的 4 空格续行因此仍算散文 —— 这正是「make sure」不误报
 *     的原因，代价是「紧跟在段落后的缩进块」不会被当代码块。
 *   · 代码文件里的锚点只认**注释**里的（字符串/普通代码里的 `@guard` 不算），
 *     原因是自测 fixture 与正则来源本身就把锚点语法写成了字符串字面量，若一并认，
 *     守卫会把自己的测试数据当成真锚点判红。
 *   · JS 的正则字面量用「前一个非空字符 ∈ ( , = : [ ! & | ? { } ; + - * % ~ ^ < >」
 *     启发式识别并整段跳过（`/(`+)/` 这类含反引号的正则不会被当成模板字符串）。
 *     启发式会误判极小部分除法表达式，影响仅限于多/漏看几行注释，不会漏检引用。
 *   · Python 只认 `#` 行注释（docstring 不认）；HTML 只认 `<!-- -->`。
 *   · 冻结标记只抑制**规则 A**（引用）；锚点与棘轮照查。冻结标记文件若一条引用都没有，
 *     只打 info（标记可能因为别的理由而存在），只有「有引用且全部解析成功」才判红。
 *   · 占位符（含 `<…>` `*` `{…}` `$…` 的路径/目标）不参与解析，计入 `placeholder`
 *     统计并在盘点里列出 —— 这是显式的缺口，不是静默豁免。
 *
 * ── 基线出处（只许降，改动必须同批下调） ───────────────────────────────
 *   实测日期 2026-11-16，实测命令 `node scripts/check-annotations.mjs --report`
 *   （main e9cff0d8 + 同批**在工作区里**的 Makefile/ci.yml/locale 改动），四个值见下方
 *   常量注释；每个下限/基线都在注释里写了它来自哪一次实测的哪个数字。
 *   口径提醒：同一个脚本在**干净 HEAD** 上跑出的数字更小（实测 568 文件 / 28283 注释行 /
 *   595 引用 / unbacked 295 / unmarked 388，因为父会话的接线注释还没提交）。若那批
 *   改动最终没同批合入，棘轮会显示 UNDER 并要求下调 —— 照提示下调到实测值即可，不要放宽。
 *
 * 接线：docs/lint-guards.md 的 L5 行计划挂在 `make ci`。本任务冻结契约只允许新建本
 *   文件（不改 Makefile/ci.yml/package.json），所以接线由协调者落地；单独跑就是上面的命令。
 *
 * ── 本守卫自身的规则码也是外键（自证：锚点机制在真仓库上是活的） ──────
 * 注：上面语法块里的跨文件示例锚点指向同批的 L1 `scripts/check-css.mjs`（docs/lint-guards.md
 *   §4.2 的原文示例）。L1 尚未合入的树上，它会成为本文件里唯一一条 ANCHOR_UNRESOLVED ——
 *   属**批量合并顺序**问题（L1～L5 同批），不是判据问题。

 * @guard scripts/check-annotations.mjs#REF_UNRESOLVED
 * @guard scripts/check-annotations.mjs#ANCHOR_UNRESOLVED
 * @guard scripts/check-annotations.mjs#CLAIM_UNBACKED_OVER
 * @guard scripts/check-annotations.mjs#COUNT_UNMARKED_OVER
 */
import { readFileSync, existsSync, readdirSync, statSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { execFileSync } from "node:child_process";

/** 仓库根（按脚本位置推，不依赖 cwd；与 scripts/check-css.mjs 同款）。 */
export const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

/* ---------- 扫描范围 ---------- */

/** 代码文件来源：目录 + 允许的扩展名。 */
export const CODE_SOURCES = [
	{ dir: "web", exts: [".js"] },
	{ dir: "web-src", exts: [".html"] },
	{ dir: "internal", exts: [".go"] },
	{ dir: "scripts", exts: [".mjs", ".py"] },
	{ dir: "cmd", exts: [".go"] },
];

/** 额外单文件（相对仓库根）。 */
export const EXTRA_CODE_FILES = ["Makefile"];

/** 任何层级出现即整棵剪掉的目录名。 */
export const EXCLUDE_SEGMENTS = new Set([".git", "node_modules", ".pi", ".pi-subagents", ".pi-orchestrator"]);

/** 相对仓库根的前缀排除（只匹配从根开始的路径）。 */
export const EXCLUDE_PREFIXES = [
	"design/",
	"stale/",
	"missing/",
	"g2-resume-evidence/",
	"internal/server/dist/",
];

/** 根目录下的临时目录（_tmp_pbvalidate 之类）。 */
export const EXCLUDE_ROOT_PREFIX = "_tmp_";

/** 决策文档探测顺序（docs/lint-guards.md §4.2 的冻结口径）。 */
export const DECISION_DOC_CANDIDATES = [
	"docs/design/v4/decisions.md",
	".pi-orchestrator/v4/CONTRACT.md",
];
/** 第三级：`docs/design/v4/*.md`（按文件名序，排除已在候选里的）。 */
export const DECISION_DOC_GLOB_DIR = "docs/design/v4";

/* ---------- 反假绿下限（实测值，出处写在每条后面） ---------- */
/*
 * 实测日期 2026-11-16 · 实测命令 `node scripts/check-annotations.mjs --report`
 * （main e9cff0d8 + 本文件；git 可用）。**下限一律 ≤ 实测值**，语义是「至少」——
 * 它只拦「解析出 0 条 / 解析器坏了 / 文件没读到」，不拦「少了一点」：
 *   扫描文件 575（代码 528 / 文档 47） → 520（文档 40）
 *   注释行   28699                    → 24000
 *   引用     606（解析 583 / 未解析 15 / 历史提及 8 / 占位符 19） → 550
 *   锚点     8（guard 5 / decision 1 / count 1 / 占位符 1） → 4（= 头部四条自锚点）
 * 注意：文档只认 `git ls-files '*.md'` 且排除 .pi-orchestrator/ 等目录 —— 289 份被跟踪
 * md 里 234 份在 .pi-orchestrator/ 下，所以文档实测只有 47 份，这是扫描范围不是 bug。
 */
export const SCAN_FILES_MIN = 520;
export const SCAN_DOCS_MIN = 40;
export const COMMENT_LINES_MIN = 24000;
export const REFS_MIN = 550;
/** 锚点下限 = 本文件头部的 4 条自锚点。为 0 就等于「锚点机制在真仓库上从未跑过」。 */
export const ANCHOR_MIN = 4;

/* ---------- 棘轮基线（实测值；低于基线也红，必须同批下调） ---------- */
/*
 * 实测日期 2026-11-16 · 实测命令 `node scripts/check-annotations.mjs --report`
 * （main e9cff0d8 + 本文件，关键词表与扫描面按父会话裁决收窄**之后**重测）：
 *   unbacked（声称有门禁/守卫兜底、但没有 guard 锚点的行）实测 324 / 命中行共 324
 *   unmarked（契约面里的计数断言、但没有 count 锚点的行）实测 413 / 命中行共 414
 * 口径：
 *   · C 只数代码注释（web/web-src/internal/scripts/cmd/Makefile），关键词见 CLAIM_RE
 *     （只认「声称有检查在兜底」，不认「必须/不得」这类设计意图）。
 *   · D 只数契约面文件（见 inCountScope）：Makefile / scripts 的 mjs+py / web-src /
 *     docs / DEVELOPMENT.md / AGENTS.md / E2E_TESTING.md。
 * 这两个数字就是「注释当契约」的存量账本。本批只落地守卫与账本；清理排期由主 agent
 * 决定。任何新增都必须走锚点/命令，所以基线只许降 —— 低于基线也红，是为了逼同批下调，
 * 避免过期基线变成下一次回归的挡箭牌。
 */
export const CLAIM_UNBACKED_BASELINE = 324;
export const COUNT_UNMARKED_BASELINE = 413;

/* ---------- 判据 ---------- */

/**
 * C：只抓「声称**有检查/门禁在兜底**」这一类断言（父会话裁决，2026-11-16 收窄）。
 * `必须/不得/不许/严禁/只能走/绝不能` 不在表内 —— 它们是设计意图（属于 `@decision` 面，
 * 不进 C）。理由：Go 注释里「必须/不得」是常态，2032/2034 命中率等于没有基线。
 */
export const CLAIM_RE =
	/(门禁|lint(?!-guards)|守卫|会红|会拦住|由.{0,12}(保证|兜住)|已由|测试会|自动拦|enforced|\bguard\b|fails the build)/i;

/** D：计数型断言（`13 个` / `38 条` / `48 对` / `9 行` / `4 套件` / `179 个断言` / `12 项`）。 */
export const COUNT_RE = /\d[\d,]*\s*(?:处|个|条|对|行|套件|断言|项)/;

/**
 * D 的扫描面**只限契约文件**（父会话裁决，2026-11-16 收窄）：`Makefile`、
 * `scripts/**\/*.mjs|*.py`、`web-src/**`、`docs/**`、`DEVELOPMENT.md`、`AGENTS.md`、
 * `E2E_TESTING.md`。Go 源码注释里的「64 个 0」「16MiB / 32 项」这类**局部常量**不算 ——
 * 它们是被测对象的数据，不是对外承诺的计数。
 * markdown 没有注释语法，因此这些文件按「逐行散文」计入（契约文档里的「两条」就是承诺）。
 */
export const COUNT_SCOPE_EXACT = new Set(["Makefile", "DEVELOPMENT.md", "AGENTS.md", "E2E_TESTING.md"]);
const COUNT_SCOPE_PREFIX = ["scripts/", "web-src/", "docs/"];

/** 纯函数：该文件是否在规则 D 的契约面里。 */
export function inCountScope(file) {
	if (COUNT_SCOPE_EXACT.has(file)) return true;
	if (!COUNT_SCOPE_PREFIX.some((p) => file.startsWith(p))) return false;
	if (file.startsWith("scripts/")) return /\.(?:mjs|py)$/.test(file);
	return true;
}

/**
 * A 的「历史提及」口子（父会话裁决，2026-11-16）：同一行或紧邻上一行出现
 * 已删除/已退役/已下线/已移除/已废弃/remove(d)?/retire(d)? 时，**引用不存在**的目标
 * 算历史提及（不判红、单独计数）。只对「引用不存在」生效 —— 引用存在时正常算解析成功。
 */
export const HISTORICAL_RE = /((?:已)?删除|(?:已)?退役|(?:已)?下线|(?:已)?移除|(?:已)?废弃|removed?|retired?)/i;

/** 锚点语法（值是紧跟的「第一个非空白片段」）。 */
export const ANCHOR_RES = {
	guard: /@guard\s+([^\s`'"]+)/,
	decision: /@decision\s+([A-Za-z0-9][A-Za-z0-9-]*)/,
	count: /@count\s+(.+)$/,
};

/** 冻结标记（仅 markdown 前 40 行）。 */
export const FROZEN_RE = /frozen-doc\s*[:：]\s*(\S.*)$/;
export const FROZEN_LINE_LIMIT = 40;

/** 引用前缀：出现在这些字符之后才算「一条命令的开头」（避免匹配单词内部）。 */
const REF_PREFIX = "[\\s`(\"'=\\[<,，、:：;&|]";

/** 规则 A 的六种引用形态。 */
export const REF_RES = [
	{ kind: "script", re: new RegExp(`(?:^|${REF_PREFIX})((?:\\.\\/)?scripts\\/[A-Za-z0-9._\\-/@#*{}]+)`, "g") },
	{ kind: "make", re: new RegExp(`(?:^|${REF_PREFIX})make\\s+([^\\s\`"'()|;&]+)`, "g") },
	{ kind: "npm", re: new RegExp(`(?:^|${REF_PREFIX})npm\\s+run\\s+([^\\s\`"'()|;&]+)`, "g") },
	{ kind: "node", re: new RegExp(`(?:^|${REF_PREFIX})node\\s+((?:--?[A-Za-z][\\w-]*(?:=\\S+)?\\s+)*)([^\\s\`"'()|;&]+)`, "g") },
	{ kind: "go", re: new RegExp(`(?:^|${REF_PREFIX})go\\s+(run|test|build|vet)\\s+(.+)$`, "g") },
	{ kind: "cmd", re: new RegExp(`(?:^|${REF_PREFIX})(cmd\\/[A-Za-z0-9._\\-/*{}]+)`, "g") },
];

/** `go test|run|build` 里**不带值**的 flag（其余 flag 一律按「吃下一个 token 作值」处理）。 */
const GO_BOOL_FLAGS = new Set([
	"v", "short", "race", "json", "cover", "benchmem", "failfast", "trimpath", "n", "a", "q", "x",
	"msan", "asan", "linkshared", "buildvcs", "demo", "work", "x",
]);

/** `/^--?name(=value)?$/` 里名为 name 的 flag 是否吃值（未知 flag 按吃值算）。 */
function flagTakesValue(tok) {
	if (tok.includes("=")) return false; // 自带值
	const name = tok.replace(/^-+/, "");
	if (GO_BOOL_FLAGS.has(name)) return false;
	return true;
}

/* ---------- 小工具 ---------- */

/** 多行/长文本压成一行，供报告贴原文。 */
export function oneLine(text, max = 140) {
	const flat = String(text).replace(/\s+/g, " ").trim();
	return flat.length > max ? `${flat.slice(0, max)}…` : flat;
}

/** 统一 normalization：去掉 `./` 前缀与尾斜杠；拒绝绝对路径与 `..`（不越出仓库）。 */
export function normRel(rel) {
	if (typeof rel !== "string") return null;
	let p = rel.replace(/\\/g, "/").trim();
	while (p.startsWith("./")) p = p.slice(2);
	p = p.replace(/\/+$/, "");
	if (p.startsWith("/") || p.split("/").includes("..")) return null;
	return p;
}

/** 引用/锚点目标末尾的标点（含中文标点、括号、句号）与 `:行号` 后缀剥掉。 */
export function cleanTarget(raw) {
	let t = String(raw).trim();
	t = t.replace(/\\+$/, "");
	// `scripts/<名>.mjs#RULE_ID` → 锚点后缀；`scripts/<名>.py:384` / `:384-390` → 行号后缀
	t = t.replace(/#.*$/, "");
	t = t.replace(/:\d+(?:[-–]\d+)?$/, "");
	t = t.replace(/[.,;:!?)\]}>）】、。；：，]+$/, "");
	t = t.replace(/^`+|`+$/g, "");
	// 路径/目标不含 CJK 字符：遇到中文/全角就截止（避免把 `scripts/<名>.mjs，重写` 整段当路径）
	t = t.replace(/[\u3000-\u303f\u3400-\u9fff\uff00-\uffef].*$/u, "");
	return t;
}

/** 占位符（模板/通配/变量）不参与解析 —— 显式计入 placeholder，不静默豁免。 */
export function isPlaceholder(target) {
	return /[*<>{}…]|\$\{|\$\(|\$[A-Za-z_]/.test(String(target));
}

/* ---------- 虚拟文件系统（自测用：跑的是与真仓库同一份解析/判定代码） ---------- */

/**
 * 用一张 `相对路径 → 文本` 的表构造 env。目录由路径前缀推导，`"a/b/"` 形式的空串
 * 条目可显式声明空目录。
 * @param {Record<string, string>} files
 * @returns {{exists:Function, isDir:Function, read:Function, listDir:Function}}
 */
export function virtualEnv(files) {
	const map = new Map();
	const dirs = new Set([""]);
	for (const [k, v] of Object.entries(files)) {
		const p = normRel(k);
		if (p === null) continue;
		if (p === "" || k.endsWith("/")) {
			if (p) dirs.add(p);
			continue;
		}
		map.set(p, v);
		const parts = p.split("/");
		for (let i = 1; i < parts.length; i++) dirs.add(parts.slice(0, i).join("/"));
	}
	const exists = (rel) => {
		const p = normRel(rel);
		if (p === null) return false;
		return p === "" || map.has(p) || dirs.has(p);
	};
	return {
		exists,
		isDir(rel) {
			const p = normRel(rel);
			if (p === null) return false;
			return p === "" || dirs.has(p);
		},
		read(rel) {
			const p = normRel(rel);
			if (p === null || p === "") return null;
			return map.has(p) ? map.get(p) : null;
		},
		listDir(rel) {
			const p = normRel(rel);
			if (p === null) return [];
			const prefix = p === "" ? "" : `${p}/`;
			const out = [];
			for (const key of [...map.keys(), ...dirs]) {
				if (key === "" || !key.startsWith(prefix)) continue;
				const rest = key.slice(prefix.length);
				if (rest === "" || rest.includes("/")) continue;
				out.push(rest);
			}
			return out.sort();
		},
	};
}

/** 真仓库的 env（同步读；小文件多，缓存一次）。 */
export function realEnv(root = ROOT) {
	const readCache = new Map();
	const statCache = new Map();
	const abs = (rel) => {
		const p = normRel(rel);
		return p === null ? null : path.join(root, p);
	};
	const st = (rel) => {
		const p = normRel(rel);
		if (p === null) return null;
		if (statCache.has(p)) return statCache.get(p);
		let s = null;
		try {
			s = statSync(path.join(root, p));
		} catch {
			s = null;
		}
		statCache.set(p, s);
		return s;
	};
	return {
		exists(rel) {
			const p = normRel(rel);
			if (p === null) return false;
			return p === "" || st(p) !== null;
		},
		isDir(rel) {
			const s = st(rel);
			return !!s && s.isDirectory();
		},
		read(rel) {
			const p = normRel(rel);
			if (p === null || p === "") return null;
			if (readCache.has(p)) return readCache.get(p);
			let text = null;
			try {
				text = readFileSync(path.join(root, p), "utf8");
			} catch {
				text = null;
			}
			readCache.set(p, text);
			return text;
		},
		listDir(rel) {
			const p = normRel(rel);
			if (p === null) return [];
			try {
				return readdirSync(path.join(root, p === "" ? "." : p)).sort();
			} catch {
				return [];
			}
		},
	};
}

/* ---------- 注释解析 ---------- */

/** 每种语言的注释语法 + 字符串定界符（字符串内容不当注释、不当引用）。 */
const COMMENT_SYNTAX = {
	go: { line: ["//"], block: [["/*", "*/"]], quotes: ['"', "'", "`"], regex: false },
	js: { line: ["//"], block: [["/*", "*/"]], quotes: ['"', "'", "`"], regex: true },
	py: { line: ["#"], block: [], quotes: ['"', "'"], regex: false },
	html: { line: [], block: [["<!--", "-->"]], quotes: [], regex: false },
	make: { line: ["#"], block: [], quotes: [], regex: false },
};

/** 文件后缀/名字 → 语言。 */
export function langOf(file) {
	const base = path.basename(file);
	if (base === "Makefile") return "make";
	const ext = path.extname(base);
	if (ext === ".go") return "go";
	if (ext === ".mjs" || ext === ".js" || ext === ".cjs") return "js";
	if (ext === ".py") return "py";
	if (ext === ".html" || ext === ".htm") return "html";
	return null;
}

/**
 * 纯函数：抽出注释行（每个物理行一条，行号精确；多行块注释逐行展开）。
 * @returns {{line:number, text:string}[]}
 */
export function scanComments(lang, text) {
	const syn = COMMENT_SYNTAX[lang];
	if (!syn) return [];
	const entries = [];
	const n = text.length;
	let i = 0;
	let line = 1;
	while (i < n) {
		const c = text[i];
		if (c === "\n") {
			line++;
			i++;
			continue;
		}
		// 行注释
		let lineMark = null;
		for (const m of syn.line) {
			if (text.startsWith(m, i)) {
				lineMark = m;
				break;
			}
		}
		if (lineMark) {
			let j = text.indexOf("\n", i);
			if (j < 0) j = n;
			const body = text.slice(i + lineMark.length, j).trim();
			if (body !== "") entries.push({ line, text: body });
			i = j;
			continue;
		}
		// 块注释
		let blk = null;
		for (const pair of syn.block) {
			if (text.startsWith(pair[0], i)) {
				blk = pair;
				break;
			}
		}
		if (blk) {
			const [open, close] = blk;
			let j = text.indexOf(close, i + open.length);
			if (j < 0) j = n;
			const body = text.slice(i + open.length, j);
			const parts = body.split("\n");
			for (let k = 0; k < parts.length; k++) {
				// 续行常见写法 ` * xyz`：去掉装饰性星号
				const seg = k === 0 ? parts[k] : parts[k].replace(/^\s*\*+\s?/, "");
				const t = seg.trim();
				if (t !== "") entries.push({ line: line + k, text: t });
			}
			line += parts.length - 1;
			i = j + close.length;
			continue;
		}
		// 字符串（跳过，行号照数）
		const q = syn.quotes.find((qq) => text.startsWith(qq, i));
		if (q) {
			let j = i + q.length;
			while (j < n) {
				const ch = text[j];
				if (ch === "\\" && q !== "`") {
					if (text[j + 1] === "\n") line++;
					j += 2;
					continue;
				}
				if (ch === q) {
					j++;
					break;
				}
				if (ch === "\n") line++;
				j++;
			}
			i = j;
			continue;
		}
		// JS 正则字面量（跳过）：否则 `/(`+)/` 里的反引号会被当成模板字符串开头，
		// 导致后续整片代码被误认为字符串/注释（假绿与假红同时产生）。
		if (syn.regex && c === "/" && looksLikeRegexStart(text, i)) {
			let j = i + 1;
			let inClass = false;
			while (j < n) {
				const ch = text[j];
				if (ch === "\\") {
					j += 2;
					continue;
				}
				if (ch === "\n") break; // 未闭合：不跨行（宁可漏跳也不吞整文件）
				if (ch === "[") inClass = true;
				else if (ch === "]") inClass = false;
				else if (ch === "/" && !inClass) {
					j++;
					break;
				}
				j++;
			}
			i = j;
			continue;
		}
		i++;
	}
	return entries;
}

/** 启发式：`/` 是否在「值开始」位置（数组/赋值/括号/逗号之后），而不是除号。 */
const REGEX_PREV = new Set("(,=:[!&|?{};+-*%~^<>\n".split(""));
function looksLikeRegexStart(text, i) {
	let k = i - 1;
	while (k >= 0 && (text[k] === " " || text[k] === "\t")) k--;
	if (k < 0) return true;
	return REGEX_PREV.has(text[k]);
}

/** 纯函数：把注释行里的行内反引号 span 抽成「代码片段」（行号沿用注释行）。 */
export function inlineSpansOf(comments) {
	const out = [];
	for (const c of comments) {
		const spanRe = /(`+)([\s\S]*?)\1/g;
		let m;
		while ((m = spanRe.exec(c.text)) !== null) {
			if (m[2] === "") continue;
			out.push({ line: c.line, text: m[2] });
		}
	}
	return out;
}

/* ---------- markdown 解析 ---------- */

/**
 * 纯函数：把 markdown 拆成「代码块行 / 行内代码 span / 冻结标记」。
 * 缩进代码块按 CommonMark「不能打断段落」规则识别（前一行必须为空行或同为缩进代码），
 * 这样列表项里的 4 空格续行仍然是散文 —— 见头注释「已知边界」。
 * @returns {{codeSegments:{line:number,text:string}[], inlineSegments:{line:number,text:string}[], frozen:{line:number, reason:string}|null}}
 */
export function parseMarkdown(text) {
	const lines = String(text).split("\n");
	const codeSegments = [];
	const inlineSegments = [];
	let frozen = null;
	let inFence = false;
	let fenceMark = "";
	let prevBlank = true;
	let prevIndented = false;

	for (let idx = 0; idx < lines.length; idx++) {
		const raw = lines[idx];
		const lineNo = idx + 1;
		if (frozen === null && lineNo <= FROZEN_LINE_LIMIT) {
			const m = FROZEN_RE.exec(raw);
			if (m) frozen = { line: lineNo, reason: m[1].trim() };
		}
		if (inFence) {
			const closeRe = new RegExp(`^\\s{0,3}${fenceMark.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}+\\s*$`);
			if (closeRe.test(raw)) {
				inFence = false;
				continue;
			}
			codeSegments.push({ line: lineNo, text: raw });
			continue;
		}
		const fence = /^\s{0,3}(`{3,}|~{3,})/.exec(raw);
		if (fence) {
			inFence = true;
			fenceMark = fence[1];
			prevBlank = false;
			prevIndented = false;
			continue;
		}
		const indented = /^(?: {4}|\t)/.test(raw);
		if (indented && (prevBlank || prevIndented)) {
			codeSegments.push({ line: lineNo, text: raw });
			prevIndented = true;
			prevBlank = false;
			continue;
		}
		if (raw.trim() === "") {
			prevBlank = true;
			prevIndented = false;
			continue;
		}
		prevBlank = false;
		prevIndented = false;
		// 行内代码 span（`` `x` `` / ``` ``x`` ```）
		const spanRe = /(`+)([\s\S]*?)\1/g;
		let m;
		while ((m = spanRe.exec(raw)) !== null) {
			if (m[2] === "") continue;
			inlineSegments.push({ line: lineNo, text: m[2] });
		}
	}
	return { codeSegments, inlineSegments, frozen };
}

/* ---------- 规则 A：引用抽取 + 解析 ---------- */

/** 把一行 `go run/test …` 的剩余部分切成包参数（跳过 flag 及其值）。 */
export function goPackageArgs(rest) {
	const toks = String(rest).split(/\s+/).filter(Boolean);
	const args = [];
	let i = 0;
	while (i < toks.length) {
		const t = toks[i];
		if (t.startsWith("-")) {
			i += flagTakesValue(t) ? 2 : 1;
			continue;
		}
		args.push(t);
		i++;
	}
	return args;
}

/**
 * 纯函数：从一段「代码文本」（代码块的一行，或一个行内代码 span 的内容）里抽引用。
 * 同一段里同一目标只报一次（`node scripts/<名>.mjs` 同时命中 node 与 script 两条规则也不重复）。
 * @returns {{kind:string, raw:string, target:string}[]}
 */
export function findRefs(text) {
	const out = [];
	const seen = new Set();
	const push = (kind, raw, target) => {
		const key = `${kind}::${target}`;
		if (seen.has(key)) return;
		seen.add(key);
		out.push({ kind, raw: String(raw).trim(), target });
	};
	for (const rule of REF_RES) {
		rule.re.lastIndex = 0;
		let m;
		while ((m = rule.re.exec(text)) !== null) {
			if (rule.kind === "go") {
				// 先切断 shell 重定向/管道，否则 `go run ./cmd/<x> > /tmp/log` 会把日志路径当包目录
				const rest = String(m[2]).split(/[|;&><)`]/)[0];
				for (const arg of goPackageArgs(rest)) {
					const target = cleanTarget(arg).replace(/\/?\.{3}$/, "");
					if (!target) continue;
					if (/^[a-z][\w+.-]*:\/\//i.test(arg)) continue; // URL 不是包
					const looksLikePkg =
						target === "." || target.startsWith("./") || (target.includes("/") && !target.startsWith("/"));
					if (!looksLikePkg) continue;
					push("go", `${m[1]} ${arg}`, target === "./" ? "." : target);
				}
				continue;
			}
			if (rule.kind === "node") {
				const target = cleanTarget(m[2]);
				if (!target || target.startsWith("-")) continue;
				const looksLikePath = target.includes("/") || /\.(?:mjs|cjs|js|ts)$/.test(target);
				if (!looksLikePath) continue;
				push("node", m[2], target);
				continue;
			}
			const target = cleanTarget(m[1]);
			if (!target) continue;
			push(rule.kind, m[1], target);
		}
	}
	return out;
}

/** 各类引用的失败解释 + 修法（报告要「为什么失败 + 怎么修」）。 */
const REF_META = {
	script: {
		why: "这个 `scripts/…` 路径在仓库里不存在",
		fix:
			"改文档指向现存脚本；若脚本该存在，就补上它。" +
			"若这是历史说明（同一行写明「已删除/已退役」即可，本守卫会按历史提及放行）；" +
			"整篇都是历史快照时，才用文件头 `frozen-doc: 理由`。",
	},
	make: {
		why: "Makefile 里没有这个目标（`^<target>:` 不存在；`.PHONY` 声明不影响判定）",
		fix: "改文档成现存目标（`make help` 可列全部），或补上该目标。",
	},
	npm: {
		why: "package.json 的 `scripts` 里没有这个脚本名",
		fix: "改文档成现存 script，或补 script 条目。",
	},
	node: {
		why: "`node <路径>` 指向的文件不存在",
		fix: "改文档指向现存入口文件，或补文件。",
	},
	go: {
		why: "`go run|test|build|vet` 的包目录不存在（已去掉 flag 与其值，且截断到 shell 重定向之前）",
		fix: "改文档成现存包目录，或补目录。",
	},
	cmd: {
		why: "`cmd/<x>` 路径不存在",
		fix: "改文档成现存 cmd 子路径，或补文件/目录。",
	},
};

/**
 * 纯函数：解析一条引用。返回 null 表示解析成功，否则返回 {message, fix}。
 * @param {{kind:string,target:string}} ref
 * @param {object} ctx {makeTargets:Set, npmScripts:Set, env}
 */
export function resolveRef(ref, ctx) {
	const meta = REF_META[ref.kind];
	if (ref.kind === "make") {
		if (ctx.makeTargets.has(ref.target)) return null;
		return { message: `${meta.why}：「${ref.target}」`, fix: meta.fix };
	}
	if (ref.kind === "npm") {
		if (ctx.npmScripts && ctx.npmScripts.has(ref.target)) return null;
		return { message: `${meta.why}：「${ref.target}」`, fix: meta.fix };
	}
	if (ref.kind === "cmd") {
		if (ctx.env.exists(ref.target)) return null;
		return { message: `${meta.why}：「${ref.target}」`, fix: meta.fix };
	}
	if (ref.kind === "go") {
		if (ctx.env.isDir(ref.target)) return null;
		return { message: `${meta.why}：「${ref.target}」`, fix: meta.fix };
	}
	if (ctx.env.exists(ref.target)) return null;
	return { message: `${meta.why}：「${ref.target}」`, fix: meta.fix };
}

/* ---------- Makefile / package.json ---------- */

/** 纯函数：从 Makefile 文本抽目标名（`^name:` / `^name::`，排除 `:=` 赋值与 recipe 行）。 */
export function parseMakeTargets(text) {
	const set = new Set();
	for (const line of String(text).split("\n")) {
		if (line.startsWith("\t")) continue; // recipe
		if (/^\s*#/.test(line)) continue;
		const m = /^([A-Za-z0-9_][A-Za-z0-9_.-]*)\s*:(?!=)/.exec(line);
		if (m) set.add(m[1]);
	}
	return set;
}

/** 纯函数：从 package.json 文本抽 scripts 名；解析失败返回 null（环境问题）。 */
export function parsePackageScripts(text) {
	if (text === null || text === undefined) return new Set();
	try {
		const json = JSON.parse(text);
		return new Set(Object.keys(json?.scripts ?? {}));
	} catch {
		return null;
	}
}

/* ---------- 规则 B：锚点抽取 + 解析 ---------- */

/** 路径/目标/规则码的合法字符（ASCII）：含中文或其他非 ASCII 的「@guard 后面跟散文」不是锚点。 */
const PATHISH_RE = /^[A-Za-z0-9._\-/@#:]+$/;

/**
 * 纯函数：从一行文本里抽锚点。
 * 注：`@guard` 后面跟的不是路径形态（含中文/空格/标点）时视作**散文提及**，不计入锚点 ——
 * 否则「@guard 规则码不存在 ⇒ 锚点红」这类行会被当成锚点去解析，把守卫自己的文档判红。
 * @returns {{kind:"guard"|"decision"|"count", raw:string, path?:string, ruleId?:string, id?:string, command?:string, placeholder?:boolean}[]}
 */
export function findAnchorsInLine(lineText) {
	const out = [];
	const g = ANCHOR_RES.guard.exec(lineText);
	if (g) {
		const raw = g[1];
		const hash = raw.indexOf("#");
		const p = cleanTarget(hash < 0 ? raw : raw.slice(0, hash));
		const ruleId = hash < 0 ? "" : raw.slice(hash + 1).replace(/[.,;:!?)\]}>）】、。；：，]+$/, "");
		const placeholder = isPlaceholder(raw) || isPlaceholder(p) || isPlaceholder(ruleId);
		const prose =
			!placeholder && (!p || !PATHISH_RE.test(p) || (hash >= 0 && !PATHISH_RE.test(ruleId)));
		if (!prose) {
			out.push({
				kind: "guard",
				raw: raw.trim(),
				path: p,
				ruleId,
				placeholder,
			});
		}
	}
	const d = ANCHOR_RES.decision.exec(lineText);
	if (d) {
		const id = d[1].replace(/[.,;:!?)\]}>）】、。；：，]+$/, "");
		out.push({ kind: "decision", raw: id, id, placeholder: isPlaceholder(id) });
	}
	const c = ANCHOR_RES.count.exec(lineText);
	if (c) {
		const command = c[1].trim().replace(/[`。；，、]+$/u, "");
		// 命令形态 = 「有空格分隔的多个 token」或含路径分隔符；「@count 则不计」这种散文不算锚点
		if (/\s/.test(command) || command.includes("/")) {
			out.push({ kind: "count", raw: command, command, placeholder: isPlaceholder(command) });
		}
	}
	return out;
}

/** 决策号在文本里是否作为独立 token 出现（`D3` 不得命中 `D32`；`V4-D1` 与 `D1` 等价）。 */
export function decisionIdFound(id, text) {
	const bare = String(id).replace(/^V4-/i, "");
	const re = new RegExp(`(^|[^A-Za-z0-9])${bare.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}([^0-9]|$)`);
	return re.test(String(text));
}

/** 纯函数：按冻结顺序探测决策文档。返回 {sources:[{file,text}], probed:[file], used:string[]}。 */
export function probeDecisionDocs(env) {
	const probed = [...DECISION_DOC_CANDIDATES];
	for (const name of env.listDir(DECISION_DOC_GLOB_DIR)) {
		if (!name.endsWith(".md")) continue;
		const rel = `${DECISION_DOC_GLOB_DIR}/${name}`;
		if (!probed.includes(rel)) probed.push(rel);
	}
	const sources = [];
	for (const file of probed) {
		const text = env.read(file);
		if (typeof text === "string") sources.push({ file, text });
	}
	return { sources, probed, used: sources.map((s) => s.file) };
}

/**
 * 纯函数：解析一条锚点。成功返回 null，失败返回 {message, fix}。
 * @param {object} anchor
 * @param {object} ctx {env, decisionDocs}
 */
export function resolveAnchor(anchor, ctx) {
	if (anchor.kind === "count") return null; // 只登记，不执行
	if (anchor.kind === "guard") {
		if (!anchor.ruleId) {
			return {
				message: `锚点缺「#<规则码>」：\`@guard ${anchor.raw}\` —— 只指向脚本不说明它抛什么码，等于没锚`,
				fix: "补成 `@guard scripts/xxx.mjs#RULE_ID`。",
			};
		}
		if (!ctx.env.exists(anchor.path)) {
			return {
				message: `锚点指向的脚本/文件不存在：「${anchor.path}」`,
				fix: "改锚点指向现存守卫文件；被引用的守卫若已删除，这条注解就要重写（它守护的约束得另找执行者）。",
			};
		}
		const text = ctx.env.read(anchor.path) ?? "";
		if (!text.includes(anchor.ruleId)) {
			return {
				message: `「${anchor.path}」里找不到规则码 \`${anchor.ruleId}\`（被引用方改名/删了规则码？）`,
				fix: "改锚点成该文件里真实存在的规则码（导出常量/字符串字面量/规则码表注释里都算），或把规则码补回被引用方。",
			};
		}
		return null;
	}
	if (anchor.kind === "decision") {
		const hit = ctx.decisionDocs.sources.find((s) => decisionIdFound(anchor.id, s.text));
		if (hit) return null;
		const where = ctx.decisionDocs.used.length
			? ctx.decisionDocs.used.join(" / ")
			: "（决策文档一个都没找到）";
		return {
			message: `决策号 \`${anchor.id}\` 不存在于决策文档：${where}`,
			fix: "改锚点成已批准的决策号，或先在决策文档里登记该决策（散文承诺 ≠ 决策）。",
		};
	}
	return null;
}

/* ---------- 主分析（纯函数：同一份代码既跑真仓库也跑自测 fixture） ---------- */

/**
 * @param {{files:{file:string,kind:"code"|"doc",text:string}[], env:object}} input
 * @returns {{counts:object, findings:object[], info:object}}
 */
export function analyzeProject({ files, env }) {
	const findings = [];
	const counts = {
		files: 0,
		codeFiles: 0,
		docFiles: 0,
		commentLines: 0,
		refs: 0,
		refsOk: 0,
		refsUnresolved: 0,
		refsPlaceholder: 0,
		historical: 0,
		anchors: 0,
		anchorsGuard: 0,
		anchorsDecision: 0,
		anchorsCount: 0,
		anchorsPlaceholder: 0,
		claimUnbacked: 0,
		claimLines: 0,
		countUnmarked: 0,
		countClaimLines: 0,
		frozenDocs: 0,
		frozenSuppressed: 0,
		byKind: {},
	};

	const makeText = env.read("Makefile");
	const makeTargets = parseMakeTargets(makeText ?? "");
	const pkgText = env.read("package.json");
	const npmScripts = parsePackageScripts(pkgText);
	const decisionDocs = probeDecisionDocs(env);
	const ctx = { env, makeTargets, npmScripts, decisionDocs };

	if (makeText === null) {
		findings.push({
			rule: "ENV_MAKEFILE_MISSING",
			file: "Makefile",
			line: 0,
			raw: "（读不到 Makefile）",
			message: "`make <target>` 引用无法判定 —— 环境问题，不是通过",
			fix: "在仓库根跑：node scripts/check-annotations.mjs",
		});
	}
	if (npmScripts === null) {
		findings.push({
			rule: "ENV_PACKAGE_JSON_UNREADABLE",
			file: "package.json",
			line: 0,
			raw: "（JSON 解析失败）",
			message: "`npm run <script>` 引用无法判定 —— 环境问题，不是通过",
			fix: "修 package.json 的 JSON 语法。",
		});
	}

	if (files.length === 0) {
		findings.push({
			rule: "SCAN_NO_FILES",
			file: "（扫描）",
			line: 0,
			raw: "0 个文件",
			message: "一个文件都没扫到 —— 「绿」不能和「没检查」同形",
			fix: "确认在仓库根运行、git 可用；扫描范围见头注释「扫描范围」。",
		});
	}

	const normalized = [...files].sort((a, b) => (a.file < b.file ? -1 : a.file > b.file ? 1 : 0));

	for (const f of normalized) {
		counts.files++;
		const guardLines = new Set();
		const countAnchorLines = new Set();
		const fileStat = { refs: 0, refsOk: 0, refsUnresolved: 0, unresolvedAll: 0 };

		// ① 拆出「注解载体」与「代码片段」
		//    代码文件：载体 = 注释（字符串/普通代码不是注解）；代码片段 = 注释里的行内反引号
		//    markdown ：载体 = 全部行；代码片段 = 围栏/缩进代码块 + 行内反引号
		let carriers = [];
		let codeSegments = [];
		let inlineSegments = [];
		if (f.kind === "doc") {
			counts.docFiles++;
			const md = parseMarkdown(f.text);
			carriers = f.text.split("\n").map((t, i) => ({ line: i + 1, text: t }));
			codeSegments = md.codeSegments;
			inlineSegments = md.inlineSegments;
			if (md.frozen) {
				counts.frozenDocs++;
				f.frozen = md.frozen;
			}
		} else {
			counts.codeFiles++;
			const comments = scanComments(langOf(f.file), f.text);
			counts.commentLines += comments.length;
			carriers = comments;
			inlineSegments = inlineSpansOf(comments);
		}

		// ② 引用（规则 A）
		const rawLines = f.text.split("\n");
		const historicalAt = (line) =>
			HISTORICAL_RE.test(rawLines[line - 1] ?? "") || HISTORICAL_RE.test(rawLines[line - 2] ?? "");
		const seenOnLine = new Set();
		for (const seg of [...codeSegments, ...inlineSegments]) {
			for (const ref of findRefs(seg.text)) {
				if (isPlaceholder(ref.target)) {
					counts.refsPlaceholder++;
					counts.placeholderList = (counts.placeholderList ?? []).concat([
						{ file: f.file, line: seg.line, kind: ref.kind, target: ref.target },
					]);
					continue;
				}
				const key = `${seg.line}|${ref.kind}|${ref.target}`;
				if (seenOnLine.has(key)) continue;
				seenOnLine.add(key);
				counts.refs++;
				fileStat.refs++;
				counts.byKind[ref.kind] = (counts.byKind[ref.kind] ?? 0) + 1;
				const bad = resolveRef(ref, ctx);
				if (!bad) {
					counts.refsOk++;
					fileStat.refsOk++;
					continue;
				}
				fileStat.unresolvedAll++;
				// 历史提及口子：只对「引用不存在」生效（解析成功的在上面的 continue 就走了）
				if (historicalAt(seg.line)) {
					counts.historical++;
					counts.historicalList = (counts.historicalList ?? []).concat([
						{ file: f.file, line: seg.line, kind: ref.kind, raw: ref.raw, target: ref.target },
					]);
					continue;
				}
				counts.refsUnresolved++;
				fileStat.refsUnresolved++;
				const finding = {
					rule: "REF_UNRESOLVED",
					file: f.file,
					line: seg.line,
					raw: ref.raw,
					kind: ref.kind,
					message: bad.message,
					fix: bad.fix,
				};
				if (f.frozen) {
					counts.frozenSuppressed++;
					f.suppressed = (f.suppressed ?? []).concat([finding]);
				} else {
					findings.push(finding);
				}
			}
		}

		// ③ 锚点（规则 B）—— 顺带记下锚点行号，供 C/D 的「同行或上一行」判据
		for (const c of carriers) {
			for (const a of findAnchorsInLine(c.text)) {
				counts.anchors++;
				if (a.placeholder) {
					counts.anchorsPlaceholder++;
					continue;
				}
				counts[`anchors${a.kind[0].toUpperCase()}${a.kind.slice(1)}`]++;
				if (a.kind === "guard") guardLines.add(c.line);
				if (a.kind === "count") countAnchorLines.add(c.line);
				const bad = resolveAnchor(a, ctx);
				if (bad) {
					findings.push({
						rule: "ANCHOR_UNRESOLVED",
						file: f.file,
						line: c.line,
						raw: `@${a.kind} ${a.raw}`,
						anchorKind: a.kind,
						message: bad.message,
						fix: bad.fix,
					});
				} else if (a.kind === "count") {
					counts.countCommands = (counts.countCommands ?? []).concat([
						{ file: f.file, line: c.line, command: a.command },
					]);
				}
			}
		}

		// ④ 棘轮计数：C 只看代码注释，D 只看契约面文件（明细见 CLAIM_RE / inCountScope 的注释）
		// 锚点本身不是断言：先把「@ 加上 guard/decision/count 的那一整段 token」从行文本摘掉再判 C/D，
		// 否则「@guard xxx#RULE」这行会因为带 guard 字样而被当成一条门禁承诺。
		const countScope = inCountScope(f.file);
		if (f.kind === "code" || countScope) {
			for (const c of carriers) {
				const bare = c.text.replace(/@(?:guard|decision|count)\b\S*/g, "");
				if (f.kind === "code" && CLAIM_RE.test(bare)) {
					counts.claimLines++;
					if (!(guardLines.has(c.line) || guardLines.has(c.line - 1))) {
						counts.claimUnbacked++;
						counts.unbackedSamples = (counts.unbackedSamples ?? []).concat([
							{ file: f.file, line: c.line, text: oneLine(c.text, 90) },
						]);
					}
				}
				if (countScope && COUNT_RE.test(bare)) {
					counts.countClaimLines++;
					if (!(countAnchorLines.has(c.line) || countAnchorLines.has(c.line - 1))) {
						counts.countUnmarked++;
						counts.unmarkedSamples = (counts.unmarkedSamples ?? []).concat([
							{ file: f.file, line: c.line, text: oneLine(c.text, 90) },
						]);
					}
				}
			}
		}

		// ⑤ 冻结标记自身的存续检查（引用全部解析成功 ⇒ 标记已多余）
		if (f.frozen) {
			if (fileStat.refs === 0) {
				counts.frozenNoRefs = (counts.frozenNoRefs ?? []).concat([f.file]);
			} else if (fileStat.unresolvedAll === 0) {
				findings.push({
					rule: "FROZEN_STALE",
					file: f.file,
					line: f.frozen.line,
					raw: `frozen-doc: ${f.frozen.reason}`,
					message: `该文件的引用已全部解析（${fileStat.refs} 条），冻结标记不再需要`,
					fix: "删掉 `frozen-doc:` 标记 —— 标记自己也由 lint 校验仍需要；冻结的文档会挡掉后续引用检查。",
				});
			}
		}
	}

	return { counts, findings, info: { makeTargets, npmScripts, decisionDocs } };
}

/* ---------- 反假绿下限 + 棘轮判定（默认模式） ---------- */

/** 纯函数：把计数与下限/基线比对，产出 findings。 */
export function judge(counts, { claimBaseline, countBaseline, mins } = {}) {
	const out = [];
	const floor = (rule, actual, min, label, extra) => {
		if (actual >= min) return; // 下限是「至少」语义：等于下限算过
		out.push({
			rule,
			file: "（总计）",
			line: 0,
			raw: `${label} ${actual} < 下限 ${min}`,
			message: `${label}掉到实测下限以下 —— 文件没读到 / 解析器坏了 / 判据在空转；「绿」不能和「没检查」同形`,
			fix: "先确认扫描范围与 git 可用；若仓库真的变小了，同步下调头注释里的下限并写明出处。",
			...extra,
		});
	};
	floor("SCAN_FILES_BELOW_MIN", counts.files, mins.files, "扫描文件数");
	floor("SCAN_DOCS_BELOW_MIN", counts.docFiles, mins.docs, "文档文件数");
	floor("COMMENT_LINES_BELOW_MIN", counts.commentLines, mins.commentLines, "注释行数");
	floor("REFS_BELOW_MIN", counts.refs, mins.refs, "引用数");
	floor("ANCHORS_BELOW_MIN", counts.anchors, mins.anchors, "锚点数");

	const ratchet = (name, actual, baseline, ruleOver, ruleUnder, what) => {
		if (actual > baseline) {
			out.push({
				rule: ruleOver,
				file: "（棘轮）",
				line: 0,
				raw: `${what} ${actual} > 基线 ${baseline}`,
				message: `新增了未锚定的注解：${what} 超过基线 ${actual - baseline} 条`,
				fix: "把这条约束变成可执行断言（lint/test），注释里只留 `@guard <脚本>#<规则码>`；确属不可判定意图则挂 `@decision <编号>`。别上调基线。",
			});
		} else if (actual < baseline) {
			out.push({
				rule: ruleUnder,
				file: "（棘轮）",
				line: 0,
				raw: `${what} ${actual} < 基线 ${baseline}`,
				message: `已改善：${what} 比基线少 ${baseline - actual} 条`,
				fix: `把 ${name} 下调到 ${actual} 再提交 —— 基线过期会成为下一次回归的挡箭牌。`,
			});
		}
	};
	ratchet("CLAIM_UNBACKED_BASELINE", counts.claimUnbacked, claimBaseline, "CLAIM_UNBACKED_OVER", "CLAIM_UNBACKED_UNDER", "未锚定的约束性注释");
	ratchet("COUNT_UNMARKED_BASELINE", counts.countUnmarked, countBaseline, "COUNT_UNMARKED_OVER", "COUNT_UNMARKED_UNDER", "未标记的计数型注释");
	return out;
}

/* ---------- 真实仓库扫描 ---------- */

/** 路径是否在扫描范围内（排除表）。 */
export function isExcluded(rel) {
	const p = normRel(rel);
	if (p === null) return true;
	const segs = p.split("/");
	for (const s of segs) if (EXCLUDE_SEGMENTS.has(s)) return true;
	for (const pre of EXCLUDE_PREFIXES) if (p === pre.replace(/\/$/, "") || p.startsWith(pre)) return true;
	if (segs.length >= 1 && segs[0].startsWith(EXCLUDE_ROOT_PREFIX)) return true;
	return false;
}

/** 递归列出代码文件（按 CODE_SOURCES），外加 EXTRA_CODE_FILES。 */
export function listCodeFiles(env) {
	const out = [];
	const walk = (dir) => {
		for (const name of env.listDir(dir)) {
			const rel = dir === "" ? name : `${dir}/${name}`;
			if (isExcluded(rel)) continue;
			if (env.isDir(rel)) {
				walk(rel);
				continue;
			}
			out.push(rel);
		}
	};
	for (const src of CODE_SOURCES) {
		if (!env.isDir(src.dir)) continue;
		const before = out.length;
		walk(src.dir);
		for (let i = out.length - 1; i >= before; i--) {
			const ext = path.extname(out[i]);
			if (!src.exts.includes(ext)) out.splice(i, 1);
		}
	}
	for (const f of EXTRA_CODE_FILES) if (env.exists(f) && !isExcluded(f)) out.push(f);
	return out.sort();
}

/** 用 `git ls-files` 列出被跟踪的 markdown（未跟踪的草稿不是契约）。 */
export function listTrackedDocs(root = ROOT) {
	const stdout = execFileSync("git", ["ls-files", "-z", "--", "*.md"], {
		cwd: root,
		encoding: "utf8",
		maxBuffer: 64 * 1024 * 1024,
	});
	return stdout
		.split("\0")
		.filter(Boolean)
		.filter((rel) => !isExcluded(rel))
		.sort();
}

/** 组装真实的扫描输入。 */
export function collectRealFiles(env, root = ROOT) {
	const files = [];
	for (const file of listCodeFiles(env)) {
		const text = env.read(file);
		if (typeof text !== "string") continue;
		files.push({ file, kind: "code", text });
	}
	for (const file of listTrackedDocs(root)) {
		const text = env.read(file);
		if (typeof text !== "string") continue;
		files.push({ file, kind: "doc", text });
	}
	return files;
}

/* ---------- 已知真事实核验（只用于 --report 自证，不影响退出码） ---------- */
/*
 * 这三条是真事实（docs/lint-guards.md §2 L5 行 + §4）：守卫跑 --report 若看不见它们，
 * 说明规则 A 有 bug（没扫到代码块/行内反引号，或把散文当引用）。修掉之后对应行会显示
 * ⚠️ 未命中 —— 那时把这一行从表里删掉即可（它是自证探针，不是契约）。
 */
export const KNOWN_FACT_PROBES = [
	{ file: "docs/v4/e2e-inventory.md", kind: "script", target: "scripts/e2e/suites/real_backend.mjs" },
	{ file: "docs/design/capability-spec-v3.md", kind: "make", target: "demo-chain" },
	{ file: "docs/dev-docker.md", kind: "make", target: "mountpoint" },
];

/* ---------- 报告输出 ---------- */

const RULE_LABEL = {
	REF_UNRESOLVED: "引用解析失败（规则 A）",
	ANCHOR_UNRESOLVED: "锚点解析失败（规则 B）",
	FROZEN_STALE: "冻结标记已过期",
	CLAIM_UNBACKED_OVER: "棘轮超标（规则 C）",
	CLAIM_UNBACKED_UNDER: "棘轮已改善但基线未下调（规则 C）",
	COUNT_UNMARKED_OVER: "棘轮超标（规则 D）",
	COUNT_UNMARKED_UNDER: "棘轮已改善但基线未下调（规则 D）",
	SCAN_NO_FILES: "扫描空转",
	SCAN_FILES_BELOW_MIN: "扫描量低于下限",
	SCAN_DOCS_BELOW_MIN: "文档量低于下限",
	COMMENT_LINES_BELOW_MIN: "注释量低于下限",
	REFS_BELOW_MIN: "引用量低于下限",
	ANCHORS_BELOW_MIN: "锚点量低于下限",
	ENV_MAKEFILE_MISSING: "环境问题：读不到 Makefile",
	ENV_PACKAGE_JSON_UNREADABLE: "环境问题：package.json 解析失败",
};

function fmtWhere(f) {
	return f.line > 0 ? `${f.file}:${f.line}` : f.file;
}

function printCounts(counts) {
	console.log(
		`  扫描文件 ${counts.files}（代码 ${counts.codeFiles} / 文档 ${counts.docFiles}）  ` +
			`下限 ${SCAN_FILES_MIN}（文档 ≥ ${SCAN_DOCS_MIN}）`,
	);
	console.log(`  注释行 ${counts.commentLines}   下限 ${COMMENT_LINES_MIN}`);
	console.log(
		`  引用 ${counts.refs}（已解析 ${counts.refsOk} / 未解析 ${counts.refsUnresolved} / 占位符跳过 ${counts.refsPlaceholder}）` +
			`   下限 ${REFS_MIN}`,
	);
	const kinds = Object.entries(counts.byKind)
		.sort((a, b) => b[1] - a[1])
		.map(([k, v]) => `${k} ${v}`)
		.join(" / ");
	console.log(`  引用分类：${kinds || "（无）"}`);
	console.log(
		`  锚点 ${counts.anchors}（@guard ${counts.anchorsGuard} / @decision ${counts.anchorsDecision} / ` +
			`@count ${counts.anchorsCount} / 占位符 ${counts.anchorsPlaceholder}）   下限 ${ANCHOR_MIN}`,
	);
	console.log(
		`  unbacked ${counts.claimUnbacked} / 基线 ${CLAIM_UNBACKED_BASELINE}（约束性注释行共 ${counts.claimLines}）`,
	);
	console.log(
		`  unmarked ${counts.countUnmarked} / 基线 ${COUNT_UNMARKED_BASELINE}（计数型注释行共 ${counts.countClaimLines}）`,
	);
	console.log(`  历史提及 ${counts.historical}（不判红，见 --report 清单）`);
	console.log(`  冻结标记文档 ${counts.frozenDocs}（其中抑制引用 ${counts.frozenSuppressed} 条）`);
}

function printFindings(findings, { verbose }) {
	if (findings.length === 0) return;
	const byRule = new Map();
	for (const f of findings) {
		if (!byRule.has(f.rule)) byRule.set(f.rule, []);
		byRule.get(f.rule).push(f);
	}
	console.log("\n  ── 分类统计 ──");
	for (const [rule, list] of [...byRule.entries()].sort((a, b) => b[1].length - a[1].length)) {
		console.log(`    ${rule}  ${list.length}  ${RULE_LABEL[rule] ?? ""}`);
	}

	console.log("\n  ── findings 清单 ──");
	for (const [rule, list] of [...byRule.entries()].sort((a, b) => (a[0] < b[0] ? -1 : 1))) {
		console.log(`\n  [${rule}] ${RULE_LABEL[rule] ?? ""}（${list.length} 条）`);
		const sorted = [...list].sort((a, b) => (a.file === b.file ? a.line - b.line : a.file < b.file ? -1 : 1));
		for (const f of sorted) {
			console.log(`    ${fmtWhere(f)}  ${f.rule}  ${oneLine(f.raw)}`);
			if (verbose) {
				console.log(`      → ${f.message}`);
				console.log(`      ✎ ${f.fix}`);
			} else {
				console.log(`      → ${f.message}`);
			}
		}
	}
}

function printReportExtras(counts, info, findings) {
	console.log("\n  ── 锚点登记（@count：只登记，不执行） ──");
	const cmds = counts.countCommands ?? [];
	if (cmds.length === 0) {
		console.log("    （无 @count 锚点 —— 计数型注释目前一条都没有可重跑的命令）");
	} else {
		for (const c of cmds) console.log(`    ${c.file}:${c.line}  ${c.command}`);
	}

	console.log("\n  ── 决策文档探测 ──");
	console.log(`    探测顺序：${info.decisionDocs.probed.join(" → ")}`);
	console.log(`    实际存在：${info.decisionDocs.used.join(" / ") || "（无）"}`);

	console.log("\n  ── 已知真事实核验（自证规则 A 没空转；不影响退出码） ──");
	for (const probe of KNOWN_FACT_PROBES) {
		const same = (x) => x.file === probe.file && x.kind === probe.kind && String(x.raw).includes(probe.target);
		const asFinding = findings.some((f) => f.rule === "REF_UNRESOLVED" && same(f));
		const asHistorical = (counts.historicalList ?? []).some(same);
		const mark = asFinding ? "✅ 命中（判红）" : asHistorical ? "✅ 命中（历史提及）" : "⚠️  未命中";
		console.log(`    ${mark}  ${probe.file} · ${probe.kind} ${probe.target}`);
	}
	if ((counts.placeholderList ?? []).length > 0) {
		console.log(`\n  ── 占位符跳过（模板/通配不解析，显式登记的缺口）${counts.placeholderList.length} 条 ──`);
		for (const p of counts.placeholderList) console.log(`    ${p.file}:${p.line}  ${p.kind} ${oneLine(p.target, 70)}`);
	}
	const historical = counts.historicalList ?? [];
	if (historical.length > 0) {
		console.log(`\n  ── 历史提及（引用不存在但同行/上一行明说是退役物，不判红）${historical.length} 条 ──`);
		for (const h of historical) console.log(`    ${h.file}:${h.line}  ${h.kind} ${oneLine(h.raw, 80)}`);
	}
	if ((counts.frozenNoRefs ?? []).length > 0) {
		console.log(`\n  ── 冻结标记但无引用（只提示，不判红） ──`);
		for (const f of counts.frozenNoRefs) console.log(`    ${f}`);
	}
	if (counts.unbackedSamples?.length) {
		console.log(`\n  ── unbacked 抽样（前 10 条；全量见基线常量） ──`);
		for (const s of counts.unbackedSamples.slice(0, 10)) console.log(`    ${s.file}:${s.line}  ${s.text}`);
	}
	if (counts.unmarkedSamples?.length) {
		console.log(`\n  ── unmarked 抽样（前 10 条） ──`);
		for (const s of counts.unmarkedSamples.slice(0, 10)) console.log(`    ${s.file}:${s.line}  ${s.text}`);
	}
}

/* ---------- 自测：红/绿双侧自证（纯内存，不改仓库文件） ---------- */

function selfTest() {
	let failed = 0;
	const check = (name, ok, detail) => {
		if (ok) {
			console.log(`  ✅ ${name}`);
		} else {
			failed++;
			console.log(`  ❌ ${name}${detail ? `\n       ${detail}` : ""}`);
		}
	};

	/** 默认 fixture：Makefile 有 fe、package.json 有 test、决策文档有 V4-D1、守卫脚本文本在。 */
	const fx = (extra = {}) =>
		virtualEnv({
			Makefile: "fe:\n\tnode scripts/check-css.mjs\nci:\n\t$(MAKE) fe\n",
			"package.json": '{"scripts":{"test":"make e2e","tailwind":"node scripts/build-tailwind.mjs"}}',
			"scripts/check-css.mjs": ['export const RULES = ["BRACE_UNBALANCED", "NESTED_RULE"];'].join("\n"),
			"docs/design/v4/decisions.md": "# v4 已批准决策\n\n## V4-D1 轻量资源 API\n\n## V4-D2 节点上报\n",
			...extra,
		});
	const run = (env, docs = {}) => {
		const files = [];
		for (const [file, text] of Object.entries(docs)) {
			const kind = file.endsWith(".md") ? "doc" : "code";
			files.push({ file, kind, text });
		}
		return analyzeProject({ files, env });
	};
	const rules = (r) => r.findings.map((f) => f.rule);
	const at = (r, rule, file, line) =>
		r.findings.some((f) => f.rule === rule && f.file === file && (line === undefined || f.line === line));

	// ① 脚本不存在 ⇒ REF 红
	{
		const doc = "先跑 `scripts/nope.mjs` 再说。\n";
		const r = run(fx({ "docs/a.md": doc }), { "docs/a.md": doc });
		check(
			"① 行内反引号里的 `scripts/<不存在>` → REF_UNRESOLVED",
			at(r, "REF_UNRESOLVED", "docs/a.md", 1) && r.counts.refs === 1,
			JSON.stringify(r.findings),
		);
	}

	// ② make 目标不存在 ⇒ REF 红（同文件里存在的目标不报）
	{
		const doc = "跑 `make fe` 和 `make nope`。\n";
		const r = run(fx({ "docs/a.md": doc }), { "docs/a.md": doc });
		const refs = r.findings.filter((f) => f.rule === "REF_UNRESOLVED");
		check(
			"② 已存在的 make 目标绿、不存在的目标 → REF_UNRESOLVED",
			refs.length === 1 && refs[0].raw === "nope" && r.counts.refsOk === 1,
			JSON.stringify(r.findings),
		);
	}

	// ③ 散文里的「make sure」不误报
	{
		const doc = "Please make sure the build passes, then use make it happen.\n";
		const r = run(fx({ "docs/a.md": doc }), { "docs/a.md": doc });
		check(
			"③ 散文里的「make sure」/「make it」不判引用（不误报）",
			r.findings.length === 0 && r.counts.refs === 0,
			JSON.stringify(r.findings),
		);
	}

	// ④ @guard 规则码不存在 ⇒ 锚点红
	{
		const code = "// @guard scripts/check-css.mjs#NOPE\nconst x = 1;\n";
		const r = run(fx({ "internal/x.go": code }), { "internal/x.go": code });
		check(
			"④ @guard 指向不存在的规则码 → ANCHOR_UNRESOLVED",
			at(r, "ANCHOR_UNRESOLVED", "internal/x.go", 1),
			JSON.stringify(r.findings),
		);
	}

	// ⑤ @guard 规则码存在 ⇒ 绿
	{
		const code = "// @guard scripts/check-css.mjs#NESTED_RULE\nconst x = 1;\n";
		const r = run(fx({ "internal/x.go": code }), { "internal/x.go": code });
		check(
			"⑤ @guard 规则码真存在 → 绿",
			r.findings.length === 0 && r.counts.anchorsGuard === 1,
			JSON.stringify(r.findings),
		);
	}

	// ⑥ @decision 找不到 ⇒ 红（存在的则绿）
	{
		const bad = "// @decision D7\n";
		const good = "// @decision V4-D1\n";
		const rb = run(fx({ "internal/x.go": bad }), { "internal/x.go": bad });
		const rg = run(fx({ "internal/y.go": good }), { "internal/y.go": good });
		check(
			"⑥ @decision 不存在的决策号 → ANCHOR_UNRESOLVED；存在 → 绿",
			at(rb, "ANCHOR_UNRESOLVED", "internal/x.go", 1) && rg.findings.length === 0,
			`bad=${JSON.stringify(rb.findings)} good=${JSON.stringify(rg.findings)}`,
		);
	}

	// ⑦ 未锚定的约束性注释 ⇒ C 计数（关键词 = 「声称有检查/门禁在兜底」）
	{
		const code = "// 这里裸读 window.AnotifyGraphs 不会红 —— 门禁会拦住它，lint 会拦\nconst x = 1;\n";
		const r = run(fx({ "internal/x.go": code }), { "internal/x.go": code });
		check("⑦ 未锚定的约束性注释 → CLAIM_UNBACKED 计 1", r.counts.claimUnbacked === 1, JSON.stringify(r.counts));
	}

	// ⑧ 已锚定的不计数
	{
		const code = [
			"// @guard scripts/check-css.mjs#NESTED_RULE",
			"// 这条约束由 scripts/check-css.mjs 保证，门禁会红",
			"const x = 1;",
		].join("\n");
		const r = run(fx({ "internal/x.go": code }), { "internal/x.go": code });
		check(
			"⑧ 上一行有 @guard 的约束性注释不计数",
			r.counts.claimUnbacked === 0 && r.counts.claimLines === 1,
			JSON.stringify(r.counts),
		);
	}

	// ⑨ 计数型未标记 ⇒ D 计数；带 count 锚点则不计（D 的扫描面 = 契约文件）
	{
		const unmarked = "// 这里一共 13 个宽度值、38 条内联样式\n";
		const marked = "// @count node scripts/check-contrast.mjs\n// 这里一共 13 个宽度值\n";
		const ru = run(fx({ "scripts/a.mjs": unmarked }), { "scripts/a.mjs": unmarked });
		const rm = run(fx({ "scripts/b.mjs": marked }), { "scripts/b.mjs": marked });
		check(
			"⑨ 计数型注释无 @count → COUNT_UNMARKED 计 1；有 @count → 不计",
			ru.counts.countUnmarked === 1 && rm.counts.countUnmarked === 0,
			`unmarked=${JSON.stringify(ru.counts)} marked=${JSON.stringify(rm.counts)}`,
		);
	}

	// ⑩ 冻结标记：抑制引用不判红；引用全部解析成功 ⇒ FROZEN_STALE 红
	{
		const frozenBad = "frozen-doc: v3 设计快照，不再随代码演进\n\n跑 `scripts/gone.mjs`。\n";
		const frozenStale = "frozen-doc: 只是为了挡引用\n\n跑 `scripts/check-css.mjs`。\n";
		const rb = run(fx({ "docs/frozen-bad.md": frozenBad }), { "docs/frozen-bad.md": frozenBad });
		const rs = run(fx({ "docs/frozen-stale.md": frozenStale }), { "docs/frozen-stale.md": frozenStale });
		check(
			"⑩ 冻结标记文件的坏引用不判红；引用全部解析成功 ⇒ FROZEN_STALE 红",
			!at(rb, "REF_UNRESOLVED", "docs/frozen-bad.md") && at(rs, "FROZEN_STALE", "docs/frozen-stale.md"),
			`bad=${JSON.stringify(rb.findings)} stale=${JSON.stringify(rs.findings)}`,
		);
	}

	// ⑪ 代码块外的行内短语不当引用
	{
		const doc = "我们的 scripts/whatever.mjs 流程很稳，go run ./cmd/nonexistent 也行。\n";
		const r = run(fx({ "docs/a.md": doc }), { "docs/a.md": doc });
		check(
			"⑪ 代码块外（无行内反引号）的路径/命令不当引用",
			r.findings.length === 0 && r.counts.refs === 0,
			JSON.stringify(r.findings),
		);
	}

	// ⑫ 扫描 0 文件 ⇒ 红
	{
		const r = run(fx(), {});
		check("⑫ 扫描 0 文件 → SCAN_NO_FILES 红", rules(r).includes("SCAN_NO_FILES"), JSON.stringify(rules(r)));
	}

	/* ---- 以下为加固用例（超出自证清单的 12 条，防判据退化） ---- */

	// ⑬ 代码块（围栏）里的引用也检查 —— 规则 A 的「代码块」侧
	{
		const doc = "```sh\nnode scripts/gone.mjs\n```\n";
		const r = run(fx({ "docs/a.md": doc }), { "docs/a.md": doc });
		check("⑬ 围栏代码块里的 node <不存在> → REF_UNRESOLVED", at(r, "REF_UNRESOLVED", "docs/a.md", 2), JSON.stringify(r.findings));
	}

	// ⑭ 缩进代码块 + go 包的 flag 剥离
	{
		const doc = "示例：\n\n    go test -run TestFoo -count=1 ./internal/nope\n\n结束。\n";
		const r = run(fx({ "docs/a.md": doc, "internal": "" }), { "docs/a.md": doc });
		check(
			"⑭ 缩进代码块里的 go test 剥离 flag 后判目录 → REF_UNRESOLVED",
			at(r, "REF_UNRESOLVED", "docs/a.md", 3) && r.counts.byKind.go === 1,
			JSON.stringify(r.findings),
		);
	}

	// ⑮ 注释里的反引号才算（字符串里的不算）
	{
		const code = 'const msg = "跑 `scripts/gone.mjs`";\nconst y = 1;\n';
		const r = run(fx({ "web/a.js": code }), { "web/a.js": code });
		check("⑮ 字符串字面量里的反引号不当注解引用", r.counts.refs === 0, JSON.stringify(r.counts));
	}

	// ⑯ 占位符不判红，但要计入 refsPlaceholder（显式缺口）
	{
		const doc = "跑 `make <target>` 或 `scripts/*.mjs`。\n";
		const r = run(fx({ "docs/a.md": doc }), { "docs/a.md": doc });
		check(
			"⑯ 模板占位符（<…> / *）不判红、计入 refsPlaceholder",
			r.findings.length === 0 && r.counts.refsPlaceholder === 2,
			`${JSON.stringify(r.findings)} ${JSON.stringify(r.counts)}`,
		);
	}

	// ⑰ cmd/ 必须是目录（文件同名不算）
	{
		const doc = "入口在 `cmd/server`。\n";
		const env = fx({ "docs/a.md": doc, "cmd/server/main.go": "package main\n" });
		const r = run(env, { "docs/a.md": doc });
		check("⑰ cmd/<x> 目录存在 → 绿", r.findings.length === 0, JSON.stringify(r.findings));
	}

	// ⑱ package.json 缺 script ⇒ 红
	{
		const doc = "跑 `npm run build`。\n";
		const r = run(fx({ "docs/a.md": doc }), { "docs/a.md": doc });
		check("⑱ npm run <不存在> → REF_UNRESOLVED", at(r, "REF_UNRESOLVED", "docs/a.md", 1), JSON.stringify(r.findings));
	}

	// ⑲ @guard 缺 #规则码 ⇒ 红（半锚点等于没锚）
	{
		const code = "// @guard scripts/check-css.mjs\n";
		const r = run(fx({ "internal/x.go": code }), { "internal/x.go": code });
		check("⑲ @guard 缺 #规则码 → ANCHOR_UNRESOLVED", at(r, "ANCHOR_UNRESOLVED", "internal/x.go", 1), JSON.stringify(r.findings));
	}

	// ⑳ 棘轮双向：超基线红、低于基线也红、等于基线绿
	{
		const base = { claimBaseline: 2, countBaseline: 1, mins: { files: 0, docs: 0, commentLines: 0, refs: 0, anchors: 0 } };
		const counts = (n) => ({ files: 5, docFiles: 5, commentLines: 9, refs: 9, anchors: 9, claimUnbacked: n, countUnmarked: 1 });
		const over = judge(counts(3), base).map((f) => f.rule);
		const under = judge(counts(1), base).map((f) => f.rule);
		const equal = judge(counts(2), base).map((f) => f.rule);
		check(
			"⑳ 棘轮双向：超基线红 / 低于基线也红 / 等于基线绿",
			over.includes("CLAIM_UNBACKED_OVER") && under.includes("CLAIM_UNBACKED_UNDER") && equal.length === 0,
			`over=${over} under=${under} equal=${equal}`,
		);
	}

	// ㉑ 下限：0 引用 / 少量文件 ⇒ 红（反假绿）
	{
		const mins = { files: 10, docs: 5, commentLines: 100, refs: 50, anchors: 1 };
		const got = judge({ files: 1, docFiles: 1, commentLines: 2, refs: 0, anchors: 0, claimUnbacked: 0, countUnmarked: 0 }, { claimBaseline: 0, countBaseline: 0, mins }).map((f) => f.rule);
		check(
			"㉑ 扫描量/引用量低于下限 → 红（含 0 引用）",
			["SCAN_FILES_BELOW_MIN", "SCAN_DOCS_BELOW_MIN", "COMMENT_LINES_BELOW_MIN", "REFS_BELOW_MIN", "ANCHORS_BELOW_MIN"].every((r) => got.includes(r)),
			JSON.stringify(got),
		);
	}

	// ㉒ 决策文档探测顺序：decisions.md 缺时退到 CONTRACT.md
	{
		const env = virtualEnv({
			Makefile: "fe:\n\tnode scripts/check-css.mjs\n",
			"package.json": '{"scripts":{}}',
			".pi-orchestrator/v4/CONTRACT.md": "## V4-D6 决策在契约里\n",
			"internal/x.go": "// @decision V4-D6\n",
		});
		const r = run(env, { "internal/x.go": "// @decision V4-D6\n" });
		const ok = r.findings.length === 0 && r.info.decisionDocs.used.includes(".pi-orchestrator/v4/CONTRACT.md");
		check("㉒ 决策文档探测：decisions.md 不存在时退到 CONTRACT.md", ok, JSON.stringify(r.findings));
	}

	// ㉓ 冻结标记只在 markdown 前 40 行有效
	{
		const body = Array.from({ length: 45 }, (_, i) => `第 ${i + 1} 行`).join("\n");
		const doc = `${body}\nfrozen-doc: 太晚了\n\n跑 \`scripts/gone.mjs\`。\n`;
		const r = run(fx({ "docs/late.md": doc }), { "docs/late.md": doc });
		check(
			"㉓ frozen-doc 只在前 40 行生效（第 46 行无效 → 引用照判红）",
			at(r, "REF_UNRESOLVED", "docs/late.md", 48),
			JSON.stringify(r.findings),
		);
	}

	// ㉔ markdown 行内代码 span 不被当成散文；块引用里的也算
	{
		const doc = "> ②`scripts/gone.mjs` 在这里被反复引用。\n";
		const r = run(fx({ "docs/a.md": doc }), { "docs/a.md": doc });
		check("㉔ 引用块（>）里的行内反引号仍算引用", at(r, "REF_UNRESOLVED", "docs/a.md", 1), JSON.stringify(r.findings));
	}

	// ㉕ C 收窄：「必须/不得/严禁/只能走」属设计意图，不算门禁承诺
	{
		const code = "// 必须走 Anotify.graphLib()，不得裸读 window.AnotifyGraphs，严禁直接 setAttribute\n";
		const r = run(fx({ "internal/x.go": code }), { "internal/x.go": code });
		check("㉕ 「必须/不得/严禁」不再计入 C（收窄后不算门禁承诺）", r.counts.claimUnbacked === 0 && r.counts.claimLines === 0, JSON.stringify(r.counts));
	}

	// ㉖ D 收窄：Go 注释里的局部常量不算，Makefile / docs 的计数算
	{
		const goCode = "// writeSum 覆盖校验和文件（篡改用例写 64 个 0）\n";
		const mk = "# 覆盖校验和文件（篡改用例写 64 个 0）\n";
		const doc = "节点执行了 4 个任务，链路已全部跑完\n";
		const rGo = run(fx({ "internal/x.go": goCode }), { "internal/x.go": goCode });
		const rMk = run(fx({ Makefile: "fe:\n\tnode scripts/check-css.mjs\n# 覆盖校验和文件（篡改用例写 64 个 0）\n" }), { Makefile: "fe:\n\tnode scripts/check-css.mjs\n# 覆盖校验和文件（篡改用例写 64 个 0）\n" });
		const rDoc = run(fx({ "docs/prose.md": doc }), { "docs/prose.md": doc });
		check(
			"㉖ D 只扫契约面：Go 注释不算，Makefile 注释 / docs 散文算",
			rGo.counts.countUnmarked === 0 && rMk.counts.countUnmarked === 1 && rDoc.counts.countUnmarked === 1,
			`go=${rGo.counts.countUnmarked} mk=${rMk.counts.countUnmarked} doc=${rDoc.counts.countUnmarked}`,
		);
	}

	// ㉗ 历史提及口子：只对「引用不存在」生效，且要同行/上一行明说退役
	{
		const hist = "> `scripts/gone.mjs` 已删除（#338），别再跑它。\n";
		const stillLive = "> `scripts/check-css.mjs` 已删除（#338）。\n";
		const noMarker = "> `scripts/gone2.mjs` 别再跑了。\n";
		const rh = run(fx({ "docs/a.md": hist }), { "docs/a.md": hist });
		const rl = run(fx({ "docs/b.md": stillLive }), { "docs/b.md": stillLive });
		const rn = run(fx({ "docs/c.md": noMarker }), { "docs/c.md": noMarker });
		const retire = "# v3 的 `demo-chain`（起 server + `cmd/agent` 节点）随 `cmd/agent` 退役：\n";
		const rr = run(fx({ Makefile: retire }), { Makefile: retire });
		check(
			"㉗ 历史提及：坏引用 + 「已删除」/「随…退役」→ 只计数不判红；好引用不走此分支；无标记照红",
			rh.counts.historical === 1 &&
				!at(rh, "REF_UNRESOLVED", "docs/a.md") &&
				rl.counts.historical === 0 &&
				rl.counts.refsOk === 1 &&
				at(rn, "REF_UNRESOLVED", "docs/c.md") &&
				rr.findings.length === 0 &&
				rr.counts.historical === 1, // 同一行同一个目标只算一条（去重后）
			`hist=${JSON.stringify(rh.findings)} live=${JSON.stringify(rl.counts)} none=${JSON.stringify(rn.findings)} retire=${JSON.stringify(rr.findings)}`,
		);
	}

	console.log(failed === 0 ? "\n✅ 自测全部通过（27 组红/绿判据）" : `\n❌ 自测失败 ${failed} 组`);
	return failed === 0;
}

/* ---------- 主流程 ---------- */

function usage() {
	console.log("用法：node scripts/check-annotations.mjs [--report|--self-test|--help]");
	console.log("  默认        体检：引用/锚点/棘轮/下限，任何一条不成立 exit 1");
	console.log("  --report    盘点：只列 findings，不判红，exit 0");
	console.log("  --self-test 守卫自身的红/绿双侧自证（纯内存 fixture）");
}

async function main() {
	const argv = process.argv.slice(2);
	const known = new Set(["--report", "--self-test", "--help", "-h"]);
	const unknown = argv.filter((a) => a.startsWith("-") && !known.has(a));
	if (unknown.length > 0) {
		console.error(`✗ 不认识的参数：${unknown.join(" ")}`);
		usage();
		return 2;
	}
	if (argv.includes("--help") || argv.includes("-h")) {
		usage();
		return 0;
	}

	const reportMode = argv.includes("--report");
	const env = realEnv(ROOT);

	let files;
	try {
		files = collectRealFiles(env, ROOT);
	} catch (e) {
		console.error(`✗ 收集文件失败（git 不可用？）：${e?.message ?? e}`);
		return 2;
	}

	const { counts, findings, info } = analyzeProject({ files, env });

	console.log("注解即外键守卫 · docs/lint-guards.md §2 L5 + §4");
	console.log(`  仓库 ${ROOT}`);
	console.log(`  模式 ${reportMode ? "--report（盘点：不判红）" : "默认（体检）"}`);
	console.log(`  Makefile 目标 ${info.makeTargets.size} 个 · package.json scripts ${info.npmScripts ? info.npmScripts.size : "?"} 个`);
	printCounts(counts);

	// 环境类问题（读不到 Makefile / package.json 坏了 / 0 文件）→ exit 2，不是「红」
	const envBroken = findings.some((f) =>
		["ENV_MAKEFILE_MISSING", "ENV_PACKAGE_JSON_UNREADABLE", "SCAN_NO_FILES"].includes(f.rule),
	);

	if (reportMode) {
		printFindings(findings, { verbose: true });
		console.log(`\n  合计 findings ${findings.length} 条（盘点模式：不判红，exit 0）`);
		if (findings.length === 0) console.log("  （没有 findings —— 注意：盘点模式下这本身不构成通过证据，请核对上面的检查量）");
		printReportExtras(counts, info, findings);
		if (envBroken) console.error("\n  ⚠️  有环境级问题（见上方 findings）—— 真实体检会因此 exit 2。");
		return 0;
	}

	const judged = judge(counts, {
		claimBaseline: CLAIM_UNBACKED_BASELINE,
		countBaseline: COUNT_UNMARKED_BASELINE,
		mins: { files: SCAN_FILES_MIN, docs: SCAN_DOCS_MIN, commentLines: COMMENT_LINES_MIN, refs: REFS_MIN, anchors: ANCHOR_MIN },
	});
	const all = [...findings, ...judged];

	if (all.length === 0) {
		console.log("\n✅ 注解即外键守卫通过：全部引用可解析、锚点可解析、棘轮未超基线。");
		printReportExtras(counts, info, all);
		return 0;
	}

	printFindings(all, { verbose: true });
	console.log(`\n  合计 findings ${all.length} 条`);
	printReportExtras(counts, info, all);
	console.log("\n  处置建议：");
	console.log("    · REF_UNRESOLVED：引用指向了不存在的脚本/目标 → 改文档指向现存的，或补上对象；");
	console.log("      历史说明（已删脚本）就给文档头部加 `frozen-doc: 理由`，但那标记在引用全解析后必红。");
	console.log("    · ANCHOR_UNRESOLVED：注解的外键断了（被引用方改规则码/删文件/决策号不存在）→ 改锚点或补被引用方。");
	console.log("    · 棘轮 OVER：新增的「有门禁兜底」声称 / 计数断言 → 变成可执行断言 + 挂 `@guard` / `@count`；别上调基线。");
	console.log("    · 棘轮 UNDER：已改善 → 同批把基线下调到实测值。");
	console.log("    · 历史提及（不判红）：同行/上一行写明「已删除/已退役/移除」的历史说明照旧可留，报告里单独列出便于复核口径。");
	if (envBroken) {
		console.error("\n  ✗ 存在环境级问题（读不到 Makefile / package.json 坏 / 0 文件）—— 按 exit 2 处理。");
		return 2;
	}
	return 1;
}

const isDirectRun =
	process.argv[1] && path.resolve(process.argv[1]) === path.resolve(fileURLToPath(import.meta.url));
if (isDirectRun) {
	if (process.argv.includes("--self-test")) {
		process.exit(selfTest() ? 0 : 1);
	} else {
		main()
			.then((code) => process.exit(code))
			.catch((e) => {
				console.error(`✗ check-annotations 运行失败：${e?.stack ?? e}`);
				process.exit(2);
			});
	}
}
