#!/usr/bin/env node
/* parallel_runner.mjs — E2E 套件并行执行器
 *
 * 职责：
 *   1. 按套件类型分组：HTTP 类（无 Chrome）全并行，Chrome 类信号量控制并发
 *   2. 每套件 spawn node suites/<name>.mjs，传 E2E_RESULTS_DIR 环境变量
 *   3. per-suite 预算：默认 120s，重套件按**CI 实测 ×2**单独放宽（SUITE_TIMEOUT_OVERRIDE）；
 *      超预算不再「直接 SIGKILL 什么都不留」（#488）：先落 results/<suite>.json 的部分现场
 *      （timedOut/elapsedMs/completedCases/lastCase/partial），再 SIGTERM，宽限后 SIGKILL
 *   4. 读各套件 JSON 结果聚合汇总报告，并按 #338 判分：exit≠0 一律 fail、
 *      每个套件必须 passed>0 且 assertions[] 自洽、结果集合 == 请求集合、
 *      authority 清单必须覆盖请求套件且 mode 与注入计划一致
 *   5. 按 #338 裁决给四个具名 FE 套件注入 E2E_MOCK_ONLY=1，并把每套件的
 *      mode/authority 打进汇总 + 落盘 `_authority-manifest.json`
 *   6. **两层重试并存**（判据只有一个来源，输出必须能分清是哪一层）：
 *      · 套件级（#488）：`timeout` / `incomplete`（整套没有 results）自动重试 1 次，
 *        两次的 lastCase 并排打印（相同 = 确定性挂点，不同 = 抖动）；断言失败永不重试。
 *        实现在 lib/suite-budget.mjs + lib/suite-progress.mjs，由
 *        scripts/e2e/suite-budget-selftest.mjs 用真子进程钉住。
 *      · case 级（#483/#485）：f3 系套件把「首次 incomplete / 首次 timeout / 重试通过 /
 *        重试仍无结论」写进 results JSON 的 `retries`，这里逐套件与全局各打一行（0 也打），
 *        并把首次 incomplete 的现场取证目录带出来。判据在 fixtures/f3/f3-retry.mjs，
 *        由 scripts/e2e/f3-retry-selftest.mjs 钉住。
 *      两层互不替代：套件超预算 → 落 partial → 判 timeout → 重试 1 次；套件内部单条 case
 *      死亡/空输出 → 判 incomplete → 取证 → 重试 1 次。断言失败在两层都永不重试。
 *
 * 用法：
 *   node scripts/e2e/parallel_runner.mjs              # 全量并行
 *   node scripts/e2e/parallel_runner.mjs --serial     # 全量串行
 *   node scripts/e2e/parallel_runner.mjs auth_flow     # 只跑一个套件
 *   node scripts/e2e/parallel_runner.mjs --serial auth_flow  # 串行跑一个
 *   node scripts/e2e/parallel_runner.mjs --authority-plan   # 只打印每套件的 mode/authority（不跑）
 *
 * 环境变量：
 *   E2E_CONCURRENCY  Chrome 类套件并发上限（默认 4）
 *   E2E_RESULTS_DIR  JSON 结果输出目录（默认 .e2e-bin/results）
 *   ANOTIFY_BIN / DEVSEED_BIN / ANODE_BIN  二进制路径（由 run_all.sh 设置）
 *   ANOTIFY_VAPID_*  VAPID 密钥（由 run_all.sh 设置）
 *   E2E_MOCK_ONLY    **不由调用方设置**（#338）：本执行器只对 EXPLICIT_MOCK_SUITES
 *                    里的四个具名套件自注入它，其余套件（含 frontend）一律剥掉
 *   E2E_SUITE_BUDGET_MS / E2E_SUITE_BUDGET_<SUITE>  临时覆盖墙钟预算（**诊断/注入用**；
 *                    被覆盖的套件会在启动横幅里显式打印，抬预算不许留静默）
 *   E2E_RETRY_WALL_LIMIT_MS  套件级重试的墙钟上限（默认 25min）：再重试一次就会撞上
 *                    CI e2e job 的 40min 上限时宁可直接红（保留现场），不赌整份 artifact 丢掉
 */
import {
	resolveSuiteBudget,
	runSuiteWithRetry,
	SUITE_KILL_GRACE_MS,
} from "./lib/suite-budget.mjs";
import { describeProgress } from "./lib/suite-progress.mjs";
import {
	mkdirSync,
	rmSync,
	readFileSync,
	readdirSync,
	existsSync,
	writeFileSync,
} from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = path.resolve(
	path.dirname(fileURLToPath(import.meta.url)),
	"../..",
);

// ---------- 套件分类（design §1.1） ----------
const HTTP_SUITES = [
	"api_contract",
	"contract",
	"ws_protocol",
	"routing",
	"persistence",
	"security",
	"edge_cases",
	"passkeys",
	// 批次 5（#229 保留期 / #228 背压）：503 入口、清理幂等性
	"capacity",
	// #270：孤儿 pending 任务的诊断（/v1/orphans）与处置（abandon）
	"orphans",
	// console-declares（T6）：部署模型的纯 HTTP 面
	// （templates / 产物 digest / deployments 状态机 / 本地变体）
	"deploy",
	// #291 → v4 重写：真 `anode` 的 **CLI 面 + 安装/启动/退出生命周期**
	// （--version 形状、emit 退出码映射、install 落盘权限/幂等/身份不静默覆盖、
	//  安装产物 → serve 往返、SIGTERM 干净退出后中枢承认离线）。
	// 数据面（部署/执行/日志/产物/取消）由 node_v4 覆盖，两者刻意不重复。
	"live_agent",
	// #336：v4 真实节点核心链 —— 真 `cmd/anode` 二进制（不是 mock）+ 真后端：
	// v4 hello/配置对齐 → 人工部署 + 节点真拉产物 → 事件→task→lease.grant→真执行
	// → task.result → 日志 / 产物上传与私有下载 / 两种取消。
	"node_v4",
];
const CHROME_SUITES = [
	"i18n",
	"i18n_coverage",
	"frontend",
	// #476：节点页/命名空间页的可视化 vars（真 server + 真 Chrome；写路径只走 PUT …/yaml）。
	"vars_ui",
	"lang_hint",
	"cli_auth",
	"passkey_enroll",
	"admin_flow",
	"deeplink",
	"auth_flow",
	"ux_regression_19_23",
	"recovery_next",
	// 批次 4B：events 页游标分页 + 资源分层（playwright）
	"events_paging",
	// real_backend（#201）已具名退役，且文件已删（#338）：它驱动的是已删除的
	// cmd/agent v3 CLI（emit/run 两个子命令都不在了）；真节点链由 node_v4（#336）覆盖，
	// 真前端体检由 frontend（real authority）覆盖。分工与退役理由见本文件的 AUTHORITY 清单与 E2E_TESTING.md
	// 开页抖动回归：语言条 / 命名空间槽位 / 折叠态 / 预排版
	// （实测几何去重，专抓「功能全对但首帧后元素逐帧滑动」这类回归）
	"ui_shift",
	// P2 工程守卫：字体自托管 / 抽屉 a11y / reduce 语义 / View Transitions
	"p2_guards",
	// 密码登录（第二套登录模型）：真后端 + 真 Chrome，覆盖设置/修改/移除密码、
	// 密码登录与 Passkey 并存、最后一个 Passkey 删除护栏
	"password_login",
];
const ALL_SUITES = [...HTTP_SUITES, ...CHROME_SUITES];

// ---------- 验收 authority（#338 冻结裁决，2026-11-11 13:5x） ----------
//
// 五个 FE 套件的 authority 是**裁决结果**，不是「跑不动就降级」的产物。原文：
// `docs/history/v4-dispatch/TRACKER.md`「2026-11-11 13:5x 裁决：五个 FE suite 的验收模式」。
//
//   frontend                                              → real（真 server/anode/devseed）
//   i18n / i18n_coverage / ui_shift / ux_regression_19_23 → explicit-mock（E2E_MOCK_ONLY=1）
//
// 为什么这四个是显式 mock：它们验收的是 locale / DOM 锚点 / 几何 / 交互契约，不含真实 API 语义；
// 它们的 real 模式缺 session/seed bootstrap（症状是 `guest index redirected to password login`
// 这类系统性失败），补那套底座属新底座工作，不在本轮范围。真实 API/节点覆盖由
// frontend(real) + node_v4 + 13 个 HTTP 套件 + 7 个迁移后的 legacy Chrome 套件承担
// （分工表：本文件的 AUTHORITY 清单）。
//
// 不静默 fallback 的三条硬纪律：
//   1. 只有下面这份**具名**清单里的套件被注入 E2E_MOCK_ONLY=1；其余套件的 env 一个都不动。
//   2. frontend 是 real authority：父进程环境里若带了 E2E_MOCK_ONLY，会从它的子进程 env 里
//      **剥掉**并告警 —— real 静默降级成 mock 会让门禁对着 mock 报「前端 real 绿」。
//   3. 清单内套件若从父环境继承了别的值（如 `E2E_MOCK_ONLY=0`），按裁决覆盖成 1 并告警，
//      不靠继承猜 authority。
const EXPLICIT_MOCK_SUITES = new Set([
	"i18n",
	"i18n_coverage",
	"ui_shift",
	"ux_regression_19_23",
]);
// real authority 里**明确点名**的套件（必须有 mock 剥除 + 告警，不允许被清单泛化吃掉）。
const REAL_AUTHORITY_SUITES = new Set(["frontend"]);
const MOCK_ENV = "E2E_MOCK_ONLY";
// 汇总里落盘的 authority 清单（证据入口；与 results/<suite>.json 同级，不覆盖任何套件结果）。
const AUTHORITY_MANIFEST = "_authority-manifest.json";

// 套件 → 验收 authority。未列入 mock 清单的一律 real（它们跑真 server/真二进制）。
function suiteAuthority(name) {
	return EXPLICIT_MOCK_SUITES.has(name) ? "explicit-mock" : "real";
}

// 清单自检：写进常量就是契约。frontend 一旦混进 mock 清单，门禁会对着 mock 判
// 「前端 real 通过」——比红更危险，所以这里直接 exit 2，不给「先跑跑看」。
function assertAuthorityContract() {
	const problems = [];
	// 注册表冻结（#338）：全量 e2e = 29 个套件（#476 新增 vars_ui）；判定在 assertAuthorityContract。
	if (ALL_SUITES.length !== SUITE_REGISTRY_SIZE) {
		problems.push(
			`注册套件 ${ALL_SUITES.length} 个，冻结值是 ${SUITE_REGISTRY_SIZE}` +
				`（增删套件必须同步本清单与 authority 裁决；离线核对 --authority-plan）`,
		);
	}
	if (new Set(ALL_SUITES).size !== ALL_SUITES.length) {
		problems.push("ALL_SUITES 里有重名套件（会导致「结果集合 == 请求集合」静默退化）");
	}
	if (EXPLICIT_MOCK_SUITES.has("frontend")) {
		problems.push("frontend 绝不能进 EXPLICIT_MOCK_SUITES（它是 real authority）");
	}
	for (const s of EXPLICIT_MOCK_SUITES) {
		if (REAL_AUTHORITY_SUITES.has(s)) {
			problems.push(`${s} 同时被判为 real 与 explicit-mock`);
		}
		if (!ALL_SUITES.includes(s)) {
			problems.push(`${s} 不是已注册套件（改名/退役后必须同步本清单）`);
		}
	}
	for (const s of REAL_AUTHORITY_SUITES) {
		if (!ALL_SUITES.includes(s)) {
			problems.push(`${s} 不是已注册套件（改名/退役后必须同步本清单）`);
		}
	}
	if (problems.length > 0) {
		console.error("❌ 验收 authority 契约被破坏：");
		for (const p of problems) console.error(`   - ${p}`);
		console.error(
			"   裁决：docs/history/v4-dispatch/TRACKER.md「2026-11-11 13:5x」",
		);
		process.exit(2);
	}
}

const SUITE_TIMEOUT = 120_000; // 120s per suite（默认）
// 重套件的 per-suite 预算按**全量并行实测**定，且不低于 2× 实测（#338 复现记录）：
//   i18n_coverage 全量 113.1/114.4/115.6s（单跑 39s）→ 120s 预算只剩 4-7s 余量，
//   并发抖动下直接 SIGKILL，汇总里变成「0通过/0失败 超时」——和后端真回归分不开。
// 超时仍然判 fail（不因预算调大而降级判分）：加预算只是让「慢但正确」和「卡死」可区分。
const SUITE_TIMEOUT_OVERRIDE = {
	// frontend：real authority，249+ 条断言，全量实测 ~190s（单跑 ~106s）。
	frontend: 360_000,
	// i18n：全量实测 89-104s（单跑 39s）；切语言 + 几何测量的等待在并发下翻倍。
	i18n: 240_000,
	// i18n_coverage：全量实测 113-116s（单跑 39s）→ 2× ≈ 232s，留到 300s。
	i18n_coverage: 300_000,
	// ui_shift（#335/#488）：304 视图 / 87 case。旧值 300s 在 CI 上**从来没跑完过**
	// （连续 5 次 run 全部 0通过/0失败 300.1s，见 #488）。
	// 实测（CI run 44680605214，PR #490 的第一轮）：整套跑完 304.2s（86通过/1失败；
	// 旧 300s 预算只差 4-5s 就够——但「差一点」正是固定墙钟 + 慢环境的必然）。
	// 取 2× 实测 ≈ 608s → 620s 留余量；预算水位会打「>50%」提示下次该不该再抬。
	ui_shift: 620_000,
	// ux_regression_19_23：全量实测 ~60s，2× = 120s。显式写出来，不靠默认值碰运气。
	ux_regression_19_23: 120_000,
	// node_v4（#336）：绿的时候 ~2s，但串了 5 个真实进程阶段（握手/部署/执行/取消/离线），
	// 每阶段都有有界轮询；真回归要看到卡在哪一步，而不是先被 SIGKILL。套件自身断言 <300s。
	node_v4: 300_000,
	// capacity（#336）：保留期清理挂 30s 租约巡检 tick，三轮 ≈150s；默认 120s 会 SIGKILL。
	capacity: 300_000,
};
// 注册套件数冻结（#338）：一次全量 = 29 个套件（#476 新增 vars_ui）。
// 增删套件必须同时改这里 + AUTHORITY 清单（离线核对 --authority-plan），否则启动即 exit 2。
const SUITE_REGISTRY_SIZE = 29;
const RESULTS_DIR =
	process.env.E2E_RESULTS_DIR || path.join(ROOT, ".e2e-bin", "results");
const CONCURRENCY = parseInt(process.env.E2E_CONCURRENCY || "4", 10);
// #488：套件级重试的墙钟上限。CI 的 e2e job 是 40min（setup 约 4min）—— 再拖一个完整
// 预算就会拿 job 超时去赌整份 artifact，那比「直接红 + 现场仍在盘上」更糟。
const RETRY_WALL_LIMIT_MS = (() => {
	const v = Number(process.env.E2E_RETRY_WALL_LIMIT_MS);
	return Number.isFinite(v) && v > 0 ? v : 1_500_000;
})();

// ---------- 参数解析 ----------
const args = process.argv.slice(2);
let serial = false;
// 显式给了模式就听显式的：`--parallel` 以前会在下面被「带套件名默认串行」
// 无条件覆盖掉 —— 一个静默失效的 flag（`run_all.sh --parallel a b c` 仍串行，
// 于是「只跑 5 套」看起来只省了一半时间）。
let serialExplicit = false;
let planOnly = false;
const suiteArgs = [];
for (const a of args) {
	if (a === "--serial") {
		serial = true;
		serialExplicit = true;
	} else if (a === "--parallel") {
		serial = false;
		serialExplicit = true;
	} else if (a === "--authority-plan") {
		// 只打印每套件的 mode/authority，不跑任何套件：门禁要能**离线**自证
		// 「frontend 是 real、四个 FE 套件是显式 mock」，而不是靠读代码相信。
		planOnly = true;
	} else {
		// 逗号分隔也接受（`run_all.sh --serial a,b,c` 是文档里出现过的写法）。
		// 为什么必须支持：旧实现把 "a,b,c" 当成一个套件名，过滤后**静默变成 0 个套件**，
		// 然后打印「✅ E2E 全绿」——一次「什么都验证了」的假绿，比红更危险。
		for (const name of a.split(",").map((x) => x.trim()).filter(Boolean)) {
			suiteArgs.push(name);
		}
	}
}

// 决定要跑哪些套件
let suitesToRun;
if (suiteArgs.length > 0) {
	// 只跑指定的套件（串行，兼容 e2e-one）。
	// 未知套件名是**错误**：静默丢弃会让「指定了 5 个、其实跑了 0 个」也报绿。
	const unknown = suiteArgs.filter((s) => !ALL_SUITES.includes(s));
	if (unknown.length > 0) {
		console.error(`未知套件: ${unknown.join(", ")}`);
		console.error(`已知套件: ${ALL_SUITES.join(", ")}`);
		process.exit(2);
	}
	suitesToRun = [...suiteArgs];
	// 带套件名参数时默认串行（兼容 e2e-one）；但显式 `--parallel` 不再被覆盖。
	if (!serialExplicit) serial = true;
} else {
	suitesToRun = [...ALL_SUITES];
}

assertAuthorityContract();

if (planOnly) {
	printAuthorityPlan(suitesToRun);
	process.exit(0);
}

// ---------- 信号量（Chrome 类并发控制） ----------
class Semaphore {
	constructor(max) {
		this.max = max;
		this.running = 0;
		this.queue = [];
	}
	async acquire() {
		if (this.running < this.max) {
			this.running++;
			return;
		}
		await new Promise((resolve) => this.queue.push(resolve));
		this.running++;
	}
	release() {
		this.running--;
		const next = this.queue.shift();
		if (next) next();
	}
}

// ---------- 验收 authority：plan / 子进程 env ----------

// 每个套件的子进程 env：只动 mock 开关，其余 env 原样透传（不改变其他套件的环境）。
function suiteEnv(name) {
	const env = { ...process.env, E2E_RESULTS_DIR: RESULTS_DIR };
	if (EXPLICIT_MOCK_SUITES.has(name)) {
		env[MOCK_ENV] = "1"; // 具名注入：四个 FE 套件按裁决跑 explicit-mock
	} else {
		delete env[MOCK_ENV]; // real authority（含 frontend）：不带 mock 开关
	}
	return env;
}

// mode 由**注入的 env** 推导，不从套件的 stdout / pass 数反推 ——
// 反推会在「套件其实没读这个开关」时给出假 authority（本轮基线就是这种情况：
// 66d926a 上四个套件还不读 E2E_MOCK_ONLY，读取它们的 f3 驱动在 338/chrome-v4 上）。
function authorityRows(suites) {
	return suites.map((s) => {
		const authority = suiteAuthority(s);
		return {
			suite: s,
			mode: authority,
			authority,
			mockEnv: EXPLICIT_MOCK_SUITES.has(s) ? `${MOCK_ENV}=1` : `${MOCK_ENV}=<unset>`,
		};
	});
}

function printAuthorityPlan(suites) {
	const rows = authorityRows(suites);
	console.log("验收 authority（#338 裁决；分工表 = 本文件的 AUTHORITY 清单）");
	for (const r of rows) {
		console.log(
			`  suite=${r.suite} mode=${r.mode} authority=${r.authority} ${r.mockEnv}`,
		);
	}
	const mockN = rows.filter((r) => r.mode === "explicit-mock").length;
	console.log(`  合计: real ${rows.length - mockN} / explicit-mock ${mockN}`);
	if (process.env[MOCK_ENV] !== undefined) {
		console.log(
			`  注：父环境带了 ${MOCK_ENV}=${process.env[MOCK_ENV] || "(空)"} → 上表的 real 套件会剥掉它，` +
				`explicit-mock 套件仍覆盖成 1（不继承父环境）`,
		);
	}
}

// ---------- 跑单个套件 ----------
// #488：墙钟预算 → 超时先落部分现场 → timeout 判定 → 自动重试 1 次。
// 具体实现搬进 lib/suite-budget.mjs（这样 suite-budget-selftest.mjs 能用**真子进程**
// 直接驱动同一条路径，而不是只能相信汇总输出）。
function runSuite(name, runStartedAt) {
	const mode = suiteAuthority(name);
	const { budgetMs, source } = resolveSuiteBudget({
		suite: name,
		defaultMs: SUITE_TIMEOUT,
		overrides: SUITE_TIMEOUT_OVERRIDE,
	});
	return runSuiteWithRetry({
		suite: name,
		file: path.join(ROOT, "scripts", "e2e", "suites", `${name}.mjs`),
		cwd: ROOT,
		env: suiteEnv(name),
		mode,
		budgetMs,
		resultsDir: RESULTS_DIR,
		root: ROOT,
		wallLimitMs: RETRY_WALL_LIMIT_MS,
		runStartedAt,
		onNote: (line) => console.log(`  ↻ ${line}`),
	}).then((r) => ({ ...r, budget: budgetMs, budgetSource: source }));
}

// ---------- 串行执行 ----------
async function runSerial(suites, runStartedAt) {
	const results = [];
	for (const name of suites) {
		console.log(`▶ 套件: ${name}`);
		const r = await runSuite(name, runStartedAt);
		// 透传 stdout（带缩进）
		const lines = r.stdout.trim().split("\n");
		for (const line of lines) {
			console.log(`  ${line}`);
		}
		if (r.status === "pass") {
			console.log(`  ✅ 套件 ${name} 通过${retrySuffix(r)}\n`);
		} else if (r.status === "timeout") {
			const b = r.budget || SUITE_TIMEOUT;
			// #488：超时也必须能回答「跑了多少、停在哪」——「超时（300s）」等于没信息
			console.log(
				`  ⏱️ 套件 ${name} timeout（预算 ${b / 1000}s，attempts=${r.attempts.length}）：` +
					`${describeProgress(r.progress)}${retrySuffix(r)}\n`,
			);
		} else {
			console.log(`  ❌ 套件 ${name} 失败（${r.kind}）${retrySuffix(r)}\n`);
		}
		results.push(r);
	}
	return results;
}

/** 套件级重试后缀（#488）：只在真的重试过时出现。带「套件级」前缀，避免与 case 级账本混读。 */
function retrySuffix(r) {
	if (!r || !r.retried) return "";
	if (r.recovered) return `（套件级：首次 ${r.attempts[0].kind} → 重试通过）`;
	if (r.stuckSameCase === true) return `（套件级：${r.kind} ×${r.attempts.length}，两次都停在同一条「${r.lastCase}」）`;
	if (r.stuckSameCase === false)
		return `（套件级：${r.kind} ×${r.attempts.length}，两次停在不同条「${r.attempts.map((a) => a.lastCase ?? "?").join(" / ")}」）`;
	return `（套件级：${r.kind} ×${r.attempts.length}）`;
}

/** 逐 attempt 的现场明细：timeout 与重试过的套件才打（正常绿行不刷噪声）。 */
function attemptDetailLines(s) {
	if (!Array.isArray(s.attempts) || s.attempts.length === 0) return [];
	if (s.attempts.length === 1 && s.status !== "timeout") return [];
	return s.attempts.map((a) => {
		const progress =
			a.completedCases === null || a.completedCases === undefined
				? "case 级进度不可得"
				: `已完成 ${a.completedCases}${a.totalCases === null || a.totalCases === undefined ? "" : `/${a.totalCases}`} 条` +
					(a.lastCase ? `，停在「${a.lastCase}」` : "");
		const why =
			a.kind === "timeout"
				? `预算 ${Math.round((a.budgetMs ?? 0) / 1000)}s`
				: `exit=${a.exitCode ?? "?"} signal=${a.signal ?? "none"}`;
		return `      ↳ a${a.attempt} ${a.kind} ${((a.elapsedMs ?? 0) / 1000).toFixed(1)}s ${why}：${progress}`;
	});
}

// ---------- 并行执行 ----------
async function runParallel(suites, runStartedAt) {
	// 分组
	const httpSuites = suites.filter((s) => HTTP_SUITES.includes(s));
	const chromeSuites = suites.filter((s) => CHROME_SUITES.includes(s));

	const sem = new Semaphore(CONCURRENCY);
	const results = [];

	// HTTP 类全并行 + Chrome 类信号量控制
	const tasks = [];

	for (const name of httpSuites) {
		tasks.push(
			(async () => {
				const r = await runSuite(name, runStartedAt);
				results.push(r);
				return r;
			})(),
		);
	}

	for (const name of chromeSuites) {
		tasks.push(
			(async () => {
				await sem.acquire();
				try {
					const r = await runSuite(name, runStartedAt);
					results.push(r);
					return r;
				} finally {
					sem.release();
				}
			})(),
		);
	}

	await Promise.all(tasks);
	return results;
}

// ---------- 结果聚合与判分 ----------
//
// #338 判分口子（旧实现在三个地方会把「没验证」判成绿）：
//   ① `exit≠0 & failed==0 → pass-warning`：进程非 0 退出可能是清理阶段异常，
//      也可能是套件根本没跑完（信号/崩溃/挂死）。凭一份「断言全过」的 JSON 反推它
//      “只是清理失败”，等于把进程层失败当 warning 放行 —— 现在 exit≠0 一律 fail。
//   ② 从不校验断言数：没有 results/<suite>.json、或 0通过/0失败，都照样计入汇总。
//      现在：必须落盘结果、assertions[] 与 passed+failed 自洽，且 passed>0
//      （真存在 0 断言套件就进 ZERO_ASSERTION_ALLOW 具名白名单并写理由）。
//   ③ 不校验集合：请求了 29 个套件，结果里少一个 / 多一个未注册的都不报。
//      现在：结果集合必须等于请求集合（缺失/多余/未注册全部 fail）。
const ZERO_ASSERTION_ALLOW = new Map(); // `套件名 → 为什么合法地 0 断言`（现在是空：不允许）

function readJsonResult(suiteName, resultsDir = RESULTS_DIR) {
	const file = path.join(resultsDir, `${suiteName}.json`);
	if (!existsSync(file)) return null;
	try {
		return JSON.parse(readFileSync(file, "utf8"));
	} catch {
		return null;
	}
}

function aggregateResults(rawResults, requested, resultsDir = RESULTS_DIR) {
	let totalPass = 0;
	let totalFail = 0;
	const suiteSummary = [];
	const failedSuites = [];
	const problems = [];
	const requestedSet = new Set(requested);
	const seen = new Set();

	for (const r of rawResults) {
		const json = readJsonResult(r.suite, resultsDir);
		const passCount = json?.passed ?? 0;
		const failCount = json?.failed ?? 0;
		// #483：套件自报的 case 级重试账本（只有 f3 系五个套件会写）。缺席 = 该套件没重试过。
		// 与下面 #488 的 `attempts`/`retried`（**套件级**）是两个层，字段名不许混用。
		const retries = json?.retries ?? null;
		// #488：重试过就按「attempts 累加」算成本（重试花掉的时间也是这次 e2e 的钱）；
		// 没重试时保持旧口径（套件自报优先，它更接近「被测工作量」）。
		const durationMs = r.retried
			? (r.elapsedMs ?? 0)
			: (json?.durationMs || 0) > 0
				? json.durationMs
				: (r.elapsedMs ?? 0);

		let status = "pass";
		const why = [];
		if (r.status === "timeout") {
			// #488：timeout 与 incomplete 分开。超时**有现场**（timedOut/completedCases/lastCase），
			// 所以它要能回答「跑了多少、停在哪、重试后是不是同一条」，而不是只剩一个时长。
			status = "timeout";
			why.push(
				`timeout：超过 ${(r.budget || SUITE_TIMEOUT) / 1000}s 预算` +
					`（先落部分结果再 SIGTERM，宽限 ${SUITE_KILL_GRACE_MS / 1000}s 后 SIGKILL）；` +
					describeProgress(r.progress),
			);
			if (r.retried) {
				if (r.stuckSameCase === true) {
					why.push(`重试 1 次后仍 timeout；两次都停在同一条「${r.lastCase}」（= 确定性挂点，不是慢）`);
				} else if (r.stuckSameCase === false) {
					why.push(
						`重试 1 次后仍 timeout；两次停在不同条（${r.attempts
							.map((a) => a.lastCase ?? "?")
							.join(" / ")}）= 抖动/环境压力`,
					);
				}
			}
			if (r.skippedRetryReason) why.push(r.skippedRetryReason);
		} else if (r.status !== "pass") {
			// ① exit≠0 / spawn error：套件进程没正常收尾，不再当 warning
			status = "fail";
			why.push(
				`${r.kind === "incomplete" ? "incomplete：" : ""}` +
					`进程退出码 ${r.exitCode ?? "?"}${r.error ? ` (${r.error})` : ""}` +
					`——exit≠0 一律 fail（旧实现会按 JSON 断言全过降级成 pass-warning）`,
			);
		}
		const timedOut = status === "timeout";
		if (!json) {
			// ② 没落盘结果 = 没证据：0通过/0失败 也是失败
			if (!timedOut) status = "fail";
			why.push("incomplete：没有 results/<suite>.json（未跑到 summary / 崩溃前未落盘）");
		} else {
			if (failCount > 0 && !timedOut) {
				status = "fail";
				why.push(`断言失败 ${failCount} 条`);
			} else if (failCount > 0) {
				// timeout 套件的套件自报 failed 是「没跑到的 case」，与断言不符不是一回事：只记录、不升级
				why.push(`套件自报 ${failCount} 条未完成/失败（超时打断的账，不是断言不符）`);
			}
			if (!Array.isArray(json.assertions)) {
				status = timedOut ? status : "fail";
				why.push("结果缺少 assertions[]（断言计数无证据）");
			} else if (passCount + failCount !== json.assertions.length) {
				status = timedOut ? status : "fail";
				why.push(
					`passed(${passCount})+failed(${failCount}) ≠ assertions.length(${json.assertions.length})`,
				);
			}
			if (passCount === 0 && !ZERO_ASSERTION_ALLOW.has(r.suite) && !timedOut) {
				status = "fail";
				why.push("0 条断言通过（0通过/0失败 判 fail，不是空转绿）");
			}
		}

		// ③ 集合断言：结果必须来自本次请求的套件，且不重复
		if (!requestedSet.has(r.suite)) problems.push(`结果里有本次未请求的套件 ${r.suite}`);
		if (seen.has(r.suite)) problems.push(`套件 ${r.suite} 被重复执行（结果重复）`);
		seen.add(r.suite);

		totalPass += passCount;
		totalFail += failCount;

		suiteSummary.push({
			suite: r.suite,
			mode: r.mode || suiteAuthority(r.suite),
			status,
			kind: r.kind ?? status,
			passed: passCount,
			failed: failCount,
			durationMs,
			// #488：套件级 timeout/重试的现场（打印汇总与写 authority 清单都用得到）
			budget: r.budget,
			budgetSource: r.budgetSource,
			attempts: r.attempts ?? [],
			retried: Boolean(r.retried),
			recovered: Boolean(r.recovered),
			stuckSameCase: r.stuckSameCase ?? null,
			lastCase: r.lastCase ?? null,
			completedCases: r.completedCases ?? null,
			totalCases: r.totalCases ?? null,
			progress: r.progress ?? null,
			skippedRetryReason: r.skippedRetryReason ?? null,
			// #483：case 级重试账本（来自套件自报的 results JSON）。与上面的
			// `attempts`/`retried`（套件级，#488）分开：两层各自计数，不许互相覆盖。
			retries,
			why,
		});

		if (status !== "pass") {
			failedSuites.push(r.suite);
			problems.push(`${r.suite}: ${why.join("；")}`);
		}
	}

	// 请求了但没有任何结果 = 缺失（静默少跑一个套件也是“什么都没验”）
	for (const s of requestedSet) {
		if (seen.has(s)) continue;
		failedSuites.push(s);
		problems.push(`${s}: 请求了但没有任何结果（缺失）`);
	}

	// results/ 里的结果文件集合：未注册 = 注册表漂移；未请求 = 多余结果
	let resultFiles = [];
	try {
		resultFiles = readdirSync(resultsDir);
	} catch {
		resultFiles = [];
	}
	for (const file of resultFiles) {
		if (!file.endsWith(".json") || file.startsWith("_")) continue;
		const name = file.slice(0, -5);
		if (!ALL_SUITES.includes(name)) {
			problems.push(`results/${file} 是未注册套件的结果（ALL_SUITES 与套件目录漂移）`);
		} else if (!requestedSet.has(name)) {
			problems.push(`results/${file} 属于本次未请求的套件（多余结果）`);
		}
	}

	return { totalPass, totalFail, suiteSummary, failedSuites, problems, retries: retryTotals(suiteSummary) };
}

// #483：重试计数必须能在汇总里一眼看到（0 也要打），否则重试会变成静默掩盖。
// #485：timeout（硬预算 + 已落 fragment）与 incomplete 分开计数，并暴露「两次都停在同一步」。
function retryTotals(suiteSummary) {
	const withRetry = suiteSummary.filter((s) => s.retries);
	const sum = (get) => withRetry.reduce((n, s) => n + (get(s.retries) || 0), 0);
	return {
		suites: withRetry.length,
		retried: sum((r) => r.retried),
		recovered: sum((r) => r.recovered),
		exhausted: sum((r) => r.exhausted),
		incompleteFirst: sum((r) => r.incompleteFirst),
		timeoutFirst: sum((r) => r.timeoutFirst),
		assertionFirst: sum((r) => r.assertionFirst),
		timeoutsStuckSameStep: sum((r) => r.timeoutsStuckSameStep),
	};
}

// 逐套件后缀：只在真的重试过时出现（没重试的套件行保持原样，不刷噪声）。
// #483 追加：首次 incomplete 的**现场取证**目录必须跟在这一行里（不许静默）。
function retryNote(retries) {
	if (!retries || !retries.retried) return "";
	const first = [];
	if (retries.incompleteFirst) first.push(`${retries.incompleteFirst} 条首次 incomplete`);
	if (retries.timeoutFirst) first.push(`${retries.timeoutFirst} 条首次 timeout`);
	const bits = [`case 级：${retries.retried} 条首次无结论（${first.join(" + ") || "见明细"}）`];
	if (retries.recovered) bits.push(`${retries.recovered} 条重试通过`);
	if (retries.exhausted) bits.push(`${retries.exhausted} 条重试仍无结论`);
	if (retries.timeoutsStuckSameStep) bits.push(`${retries.timeoutsStuckSameStep} 条两次都停在同一步`);
	const dir = forensicsLabelOf(retries);
	return `（${bits.join("、")}${dir ? `；现场取证: ${dir}` : ""}）`;
}

function forensicsLabelOf(retries) {
	const first = retries?.forensics?.[0];
	if (!first) return "";
	return first.dir.endsWith("/") ? first.dir : `${first.dir}/`;
}

// 全局那行也要带上取证目录（只在真的有取证时）。
function suiteSummaryForensicsLabel(suiteSummary) {
	for (const s of suiteSummary) {
		const label = forensicsLabelOf(s.retries);
		if (label) return label;
	}
	return "";
}

// 逐条取证摘要（与套件 stdout 的 forensicsLine() 同形）：判断是否「越跑越死」（case 序号）。
function forensicsDetailLines(retries) {
	return (retries?.forensics || []).map(
		(f, i) =>
			`      ${i + 1}) ${String(f.caseId).padEnd(22)} #${f.caseIndex}   ` +
			`exit=${f.exitCode ?? "?"} signal=${f.signal || "none"}  ` +
			`chrom=${f.chromProcs} procs/${f.chromMb}MB  dmesg: (${f.dmesg})  oom_kill=${f.oomKill ?? "?"}  ` +
			`取证: ${f.file}`,
	);
}

// #485：timeout 的卡点摘要（与套件 stdout 的 timeoutLines() 同形）：
// 两次 attempt 的 lastStep 并排 —— 相同就是确定性挂点，不同就是抖动/环境压力。
function timeoutDetailLines(retries) {
	return (retries?.timeouts || []).map((t, i) => {
		const attempts = t.attempts
			.map(
				(a, n) =>
					`a${n + 1} ${a.kind}(${a.elapsedMs ?? "?"}ms/${a.budgetMs ?? "?"}ms) ` +
					`停在「${a.lastStep ?? "(无 PASS 步骤)"}」` +
					(a.pendingStep ? `，正在等「${a.pendingStep}」` : ""),
			)
			.join(" · ");
		const judgement = t.stuckSameStep
			? `两次都停在同一步「${t.steps[0]}」`
			: `两次停在不同步（${t.steps.map((s) => s || "(无)").join(" / ")}）`;
		return `      ${i + 1}) ${String(t.id).padEnd(22)} ${attempts} ⇒ ${judgement}`;
	});
}

// authority 清单校验（#338）：清单是「这次到底验了什么」的证据文件。
// 它本身不被校验，就只是一张可以随手写的纸：mode 漂了（real 悄悄变 mock）、
// 覆盖面少一个套件，汇总依旧打印「real 24 / explicit-mock 4」。
function validateAuthorityManifest(payload, requested) {
	const problems = [];
	const rows = Array.isArray(payload?.suites) ? payload.suites : [];
	const expected = authorityRows(requested);
	const bySuite = new Map(rows.map((r) => [r.suite, r]));
	if (rows.length !== expected.length) {
		problems.push(
			`manifest 覆盖 ${rows.length} 个套件，请求的是 ${expected.length} 个` +
				`（全量运行必须覆盖 ${SUITE_REGISTRY_SIZE} 个注册套件）`,
		);
	}
	for (const e of expected) {
		const row = bySuite.get(e.suite);
		if (!row) {
			problems.push(`manifest 缺少套件 ${e.suite}`);
			continue;
		}
		if (row.mode !== e.mode || row.authority !== e.authority) {
			problems.push(`${e.suite}: manifest mode=${row.mode}/authority=${row.authority}，注入计划是 ${e.mode}`);
		}
		if (row.mockEnv !== e.mockEnv) {
			problems.push(`${e.suite}: manifest mockEnv=${row.mockEnv}，注入计划是 ${e.mockEnv}`);
		}
	}
	for (const row of rows) {
		if (!expected.some((e) => e.suite === row.suite)) {
			problems.push(`manifest 里有本次未请求的套件 ${row.suite}`);
		}
	}
	const realRows = rows.filter((r) => r.mode === "real").length;
	const mockRows = rows.filter((r) => r.mode === "explicit-mock").length;
	if (payload?.summary?.real !== realRows || payload?.summary?.explicitMock !== mockRows) {
		problems.push(
			`manifest summary(real=${payload?.summary?.real}, explicit-mock=${payload?.summary?.explicitMock})` +
				` 与明细行(real=${realRows}, explicit-mock=${mockRows}) 不一致`,
		);
	}
	return problems;
}

// 落盘 authority 清单：把「这次运行里每个套件到底跑在什么 authority 下」固化成证据文件，
// 和 results/<suite>.json 一起留在 .e2e-bin/results/ 里（文件名以 _ 开头，不与套件重名）。
function writeAuthorityManifest(agg, requested) {
	const rows = authorityRows(requested);
	const byName = new Map(agg.suiteSummary.map((s) => [s.suite, s]));
	const rowsOut = rows.map((r) => {
		const s = byName.get(r.suite) || {};
		return {
			suite: r.suite,
			mode: r.mode,
			authority: r.authority,
			mockEnv: r.mockEnv,
			status: s.status ?? "unknown",
			passed: s.passed ?? 0,
			failed: s.failed ?? 0,
			durationMs: s.durationMs ?? 0,
		};
	});
	const payload = {
		issue: "#338",
		decision:
			"TRACKER.md「2026-11-11 13:5x 裁决：五个 FE suite 的验收模式」",
		inventoryDoc: "scripts/e2e/parallel_runner.mjs#AUTHORITY",
		mockEnvVar: MOCK_ENV,
		explicitMockSuites: [...EXPLICIT_MOCK_SUITES],
		realAuthoritySuites: [...REAL_AUTHORITY_SUITES],
		summary: {
			real: rowsOut.filter((r) => r.mode === "real").length,
			explicitMock: rowsOut.filter((r) => r.mode === "explicit-mock").length,
		},
		suites: rowsOut,
	};
	const file = path.join(RESULTS_DIR, AUTHORITY_MANIFEST);
	writeFileSync(file, JSON.stringify(payload, null, 2) + "\n");
	return file;
}

// ---------- 主入口 ----------
async function main() {
	// 准备结果目录
	rmSync(RESULTS_DIR, { recursive: true, force: true });
	mkdirSync(RESULTS_DIR, { recursive: true });

	console.log(
		serial
			? `模式: 串行（${suitesToRun.length} 个套件）`
			: `模式: 并行（HTTP 类全并行, Chrome 类并发 ${CONCURRENCY}）`,
	);
	// 验收 authority 预告：先声明本轮的 real / explicit-mock 分工（#338 裁决）。
	const mockList = suitesToRun.filter((s) => EXPLICIT_MOCK_SUITES.has(s));
	const realCount = suitesToRun.length - mockList.length;
	console.log(
		`验收 authority: real ${realCount} 个 / explicit-mock ${mockList.length} 个（#338 裁决）`,
	);
	if (mockList.length > 0) {
		console.log(`  explicit-mock（注入 ${MOCK_ENV}=1）: ${mockList.join(", ")}`);
	}
	if (process.env[MOCK_ENV] !== undefined) {
		console.log(
			`  ⚠️ 父环境带了 ${MOCK_ENV}=${process.env[MOCK_ENV] || "(空)"}：real 套件会剥掉它` +
				`（frontend 绝不允许 mock），explicit-mock 套件仍按裁决注入 1`,
		);
	}
	// #488：预算必须写进日志（“抬了多少”本身就是证据）。只列非默认值，避免刷屏。
	const budgetNotes = suitesToRun
		.map((s) => ({ s, ...resolveSuiteBudget({ suite: s, defaultMs: SUITE_TIMEOUT, overrides: SUITE_TIMEOUT_OVERRIDE }) }))
		.filter((x) => x.source !== "默认预算");
	if (budgetNotes.length > 0) {
		console.log(
			`套件墙钟预算（#488：实测×2）: ${budgetNotes
				.map((x) => `${x.s}=${x.budgetMs / 1000}s${x.source.startsWith("env") ? `（${x.source}）` : ""}`)
				.join(" · ")}`,
		);
	}
	if (process.env.E2E_RETRY_WALL_LIMIT_MS !== undefined) {
		console.log(`  ⚠️ 套件级重试墙钟上限被 env 覆盖：${RETRY_WALL_LIMIT_MS / 1000}s`);
	}
	console.log("");

	const startMs = Date.now();
	const rawResults = serial
		? await runSerial(suitesToRun, startMs)
		: await runParallel(suitesToRun, startMs);
	const totalMs = Date.now() - startMs;

	// 聚合（#338：把请求集合一起传进去，结果集合必须等于请求集合）
	const agg = aggregateResults(rawResults, suitesToRun);

	// 输出汇总
	console.log("==========================================");
	console.log(" 汇总");
	console.log("==========================================");
	console.log(
		`  总耗时: ${(totalMs / 1000).toFixed(1)}s`,
	);
	console.log(`  断言通过: ${agg.totalPass}`);
	console.log(`  断言失败: ${agg.totalFail}`);
	// #483 case 级重试计数（永远打印：正常全绿也要能证明「没有 case 被无条件重跑」）。
	const rt = agg.retries;
	const rtDir = suiteSummaryForensicsLabel(agg.suiteSummary);
	console.log(
		`  case 级重试（incomplete/timeout → 自动重试 1 次；断言失败永不重试）: 首次 incomplete ${rt.incompleteFirst} 条` +
			` · 首次 timeout ${rt.timeoutFirst} 条（两次都停在同一步 ${rt.timeoutsStuckSameStep} 条）` +
			` · 重试通过 ${rt.recovered} · 重试仍无结论 ${rt.exhausted}` +
			`（${rt.suites} 个套件上报了重试账本${rtDir ? `；现场取证: ${rtDir}` : ""}）`,
	);
	// #488 的套件级重试计数（同样永远打印，零重试也打）—— 两层各占一行，才分得清是谁吸收了失败。
	const suiteRetried = agg.suiteSummary.filter((s) => s.retried);
	console.log(
		`  套件级重试（timeout/incomplete → 自动重试 1 次；断言失败永不重试）: ${suiteRetried.length} 个套件` +
			(suiteRetried.length
				? `（${suiteRetried.map((s) => `${s.suite} ${s.attempts.map((a) => a.kind).join("→")}`).join(" · ")}；` +
					`账本 ${path.relative(ROOT, path.join(RESULTS_DIR, "_suite-retries"))}/）`
				: "（没有套件需要重试）"),
	);
	// authority 清单（#338）：先落盘再打印，红了也能在 results/ 里查本轮 authority。
	const manifestFile = writeAuthorityManifest(agg, suitesToRun);
	// 从**磁盘读回**再校验：清单是证据文件，写进去了但内容/覆盖面不对就等于假证据。
	let manifestProblems = [];
	try {
		manifestProblems = validateAuthorityManifest(
			JSON.parse(readFileSync(manifestFile, "utf8")),
			suitesToRun,
		);
	} catch (err) {
		manifestProblems = [`authority 清单无法读回/解析：${String(err && err.message ? err.message : err)}`];
	}
	const mockRan = agg.suiteSummary.filter((s) => s.mode === "explicit-mock").length;
	console.log(
		`  验收 authority: real ${agg.suiteSummary.length - mockRan} / explicit-mock ${mockRan}` +
			`（明细见每行 mode=；清单 ${path.relative(ROOT, manifestFile)}）`,
	);
	console.log("");

	// 逐套件明细
	for (const s of agg.suiteSummary) {
		const statusIcon =
			s.status === "pass" ? "✅" :
			s.status === "timeout" ? "⏱️" : "❌";
		const durStr = s.durationMs > 0 ? `${(s.durationMs / 1000).toFixed(1)}s` : "—";
		// #488：timeout 的「0通过/0失败」换成「已完成 N/M 条」——后者才是这批失败的真实语义。
		const counts =
			s.status === "timeout"
				? s.completedCases === null
					? "已完成 ? 条（case 级进度不可得）"
					: `已完成 ${s.completedCases}${s.totalCases === null ? "" : `/${s.totalCases}`} 条`
				: `${s.passed}通过/${s.failed}失败`;
		console.log(
			`  ${statusIcon} ${s.suite.padEnd(18)} ${counts}${retryNote(s.retries)}  ${durStr}  mode=${s.mode}${retrySuffix(s)}`,
		);
		// 套件级（#488）：重试过的套件把两次 attempt 并排打出来（“两次是否停同一条”是机器可判的证据）。
		for (const line of attemptDetailLines(s)) console.log(line);
		// case 级（#485）：两次 attempt 的 lastStep 并排；未重试的套件不会命中。
		for (const line of timeoutDetailLines(s.retries)) console.log(line);
		// case 级（#483）：首次 incomplete 的现场取证（零重试不打印）。
		for (const line of forensicsDetailLines(s.retries)) console.log(line);
		// 红套件把**第一条失败原因**直接打出来（#210）。
		// 为什么：并行模式不透传套件 stdout，红了只剩一个计数 —— 曾经「0通过/0失败」
		// 让人误判成超时/环境问题，实际是造数时把 undefined 绑进了 sqlite。
		// 一行足够定位，剩下的看 results/<suite>.json。
		if (s.status === "fail") {
			const json = readJsonResult(s.suite);
			const first = json?.failures?.[0];
			if (first) console.log(`      ↳ ${String(first).slice(0, 300)}`);
		}
	}

	// 套件级重试账本已在汇总头打印（含账本路径），这里只补重试后仍红的套件名。
	if (agg.failedSuites.length > 0) {
		console.log("");
		console.log(`  失败套件: ${agg.failedSuites.join(", ")}`);
	}

	// 成本可见性：最慢的几个套件（按执行器墙钟）。没排序就不知道钱花在哪，
	// 也就没有依据去做「按改动选套件」或调并发/预算。纯输出，不参与判定。
	const slowest = [...agg.suiteSummary]
		.filter((s) => s.durationMs > 0)
		.sort((a, b) => b.durationMs - a.durationMs)
		.slice(0, 5);
	if (slowest.length > 0) {
		console.log("");
		console.log(
			`  最慢套件: ${slowest
				.map((s) => `${s.suite} ${(s.durationMs / 1000).toFixed(1)}s`)
				.join(" · ")}`,
		);
	}

	// #488：预算是否还够用，要能从日志直接看出来（“离撞线还有多远”）。
	const longestAttemptMs = (s) =>
		Array.isArray(s.attempts) && s.attempts.length > 0
			? Math.max(...s.attempts.map((a) => a.elapsedMs ?? 0))
			: s.durationMs;
	const pressured = agg.suiteSummary.filter((s) => s.budget && longestAttemptMs(s) > s.budget * 0.5);
	if (pressured.length > 0) {
		console.log("");
		console.log(
			`  预算水位（>50% 预算）: ${pressured
				.map((s) => `${s.suite} ${(longestAttemptMs(s) / 1000).toFixed(0)}s/${(s.budget / 1000).toFixed(0)}s`)
				.join(" · ")}`,
		);
	}

	// 判分问题（#338）：这些都不是 warning，是「本次没有验证到什么」。
	const gateProblems = [...agg.problems, ...manifestProblems];
	if (gateProblems.length > 0) {
		console.log("");
		console.log("  判分失败项（一律判 fail，不再有 pass-warning）：");
		for (const p of gateProblems) console.log(`   - ${p}`);
	}

	console.log("==========================================");
	if (agg.failedSuites.length > 0 || gateProblems.length > 0) {
		console.log("❌ E2E 未全绿");
		process.exit(1);
	}
	// 绿灯也要能自证「验了多少」：全量运行必须 29/29 覆盖。
	console.log(
		`✅ E2E 全绿：${agg.suiteSummary.length}/${suitesToRun.length} 个请求套件均达标` +
			`（断言 ${agg.totalPass} 通过 / ${agg.totalFail} 失败，authority 清单覆盖 ${agg.suiteSummary.length} 个）`,
	);
	process.exit(0);
}

main().catch((err) => {
	console.error("执行器异常:", err);
	process.exit(2);
});
