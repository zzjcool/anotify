#!/usr/bin/env node
/* parallel_runner.mjs — E2E 套件并行执行器
 *
 * 职责：
 *   1. 按套件类型分组：HTTP 类（无 Chrome）全并行，Chrome 类信号量控制并发
 *   2. 每套件 spawn node suites/<name>.mjs，传 E2E_RESULTS_DIR 环境变量
 *   3. per-suite 超时 120s（Node 原生 setTimeout+kill，替代 macOS 缺失的 timeout）
 *   4. 读各套件 JSON 结果聚合汇总报告
 *
 * 用法：
 *   node scripts/e2e/parallel_runner.mjs              # 全量并行
 *   node scripts/e2e/parallel_runner.mjs --serial     # 全量串行
 *   node scripts/e2e/parallel_runner.mjs auth_flow     # 只跑一个套件
 *   node scripts/e2e/parallel_runner.mjs --serial auth_flow  # 串行跑一个
 *
 * 环境变量：
 *   E2E_CONCURRENCY  Chrome 类套件并发上限（默认 4）
 *   E2E_RESULTS_DIR  JSON 结果输出目录（默认 .e2e-bin/results）
 *   ANOTIFY_BIN / DEVSEED_BIN / ANODE_BIN  二进制路径（由 run_all.sh 设置）
 *   ANOTIFY_VAPID_*  VAPID 密钥（由 run_all.sh 设置）
 */
import { spawn } from "node:child_process";
import { mkdirSync, rmSync, readFileSync, readdirSync, existsSync } from "node:fs";
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
	// real_backend（#201）已具名退役：它驱动的是已删除的 cmd/agent v3 CLI；
	// 真节点链由 node_v4（#336）覆盖，真前端后端体检归 #335 real 模式。
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

const SUITE_TIMEOUT = 120_000; // 120s per suite（默认）
// frontend 是唯一一个要跑 249 条断言的套件，光 Chrome 交互就要 ~106s，
// 默认 120s 只剩十几秒余量 —— 并发跑时必然踩线。给它单独放宽。
const SUITE_TIMEOUT_OVERRIDE = {
	// 断言数还会涨（现在 310 条），240s 在并发下只剩十几秒余量，
	// 加几条动画采样就会被 SIGKILL 掉。留足一倍余量。
	frontend: 360_000,
	// node_v4（#336）：绿的时候 ~2s，但它串了 5 个真实进程间阶段（握手 / 部署 / 执行 /
	// 取消 / 离线），每个阶段都有**自己的**有界轮询。真回归时要让那些有界等待把
	// 「卡在哪一步」原样打出来，而不是先被 SIGKILL 成「超时」。套件自己也断言总时长
	// < 300s，所以这里给同一量级的预算。
	node_v4: 300_000,
	// capacity（v4 迁移）：保留期清理挂在 30s 的租约巡检 tick 上（lease.DefaultInterval），
	// 而它要验三件事：默认 30 天真的删、重启后第二轮「删 0 条」（幂等）、系统配置 0 = 永久
	// （含一次「改成 1s 就必须删」的正向对照）。每件都要等一整轮 tick，串起来 ≈ 150s；
	// 默认 120s 会把**有界等待**变成 SIGKILL，断言文本全丢（与 real_backend 同一条理由）。
	capacity: 300_000,
};
const RESULTS_DIR =
	process.env.E2E_RESULTS_DIR || path.join(ROOT, ".e2e-bin", "results");
const CONCURRENCY = parseInt(process.env.E2E_CONCURRENCY || "4", 10);

// ---------- 参数解析 ----------
const args = process.argv.slice(2);
let serial = false;
const suiteArgs = [];
for (const a of args) {
	if (a === "--serial") {
		serial = true;
	} else if (a === "--parallel") {
		serial = false;
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
	serial = true; // 单/多套件参数默认串行
} else {
	suitesToRun = [...ALL_SUITES];
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

// ---------- 跑单个套件 ----------
function runSuite(name) {
	return new Promise((resolve) => {
		const suiteFile = path.join(ROOT, "scripts", "e2e", "suites", `${name}.mjs`);
		const env = {
			...process.env,
			E2E_RESULTS_DIR: RESULTS_DIR,
		};
		const proc = spawn("node", [suiteFile], {
			env,
			cwd: ROOT,
			stdio: ["ignore", "pipe", "pipe"],
		});

		let stdout = "";
		let stderr = "";
		proc.stdout.on("data", (d) => { stdout += d; });
		proc.stderr.on("data", (d) => { stderr += d; });

		const budget = SUITE_TIMEOUT_OVERRIDE[name] || SUITE_TIMEOUT;
		const timer = setTimeout(() => {
			proc.kill("SIGKILL");
			resolve({
				suite: name,
				status: "timeout",
				budget,
				stdout,
				stderr,
			});
		}, budget);

		proc.on("exit", (code) => {
			clearTimeout(timer);
			resolve({
				suite: name,
				status: code === 0 ? "pass" : "fail",
				exitCode: code,
				stdout,
				stderr,
			});
		});

		proc.on("error", (err) => {
			clearTimeout(timer);
			resolve({
				suite: name,
				status: "fail",
				error: err.message,
				stdout,
				stderr,
			});
		});
	});
}

// ---------- 串行执行 ----------
async function runSerial(suites) {
	const results = [];
	for (const name of suites) {
		console.log(`▶ 套件: ${name}`);
		const r = await runSuite(name);
		// 透传 stdout（带缩进）
		const lines = r.stdout.trim().split("\n");
		for (const line of lines) {
			console.log(`  ${line}`);
		}
		if (r.status === "pass") {
			console.log(`  ✅ 套件 ${name} 通过\n`);
		} else if (r.status === "timeout") {
			const b = r.budget || SUITE_TIMEOUT;
			console.log(`  ⏱️ 套件 ${name} 超时（${b / 1000}s）\n`);
		} else {
			console.log(`  ❌ 套件 ${name} 失败\n`);
		}
		results.push(r);
	}
	return results;
}

// ---------- 并行执行 ----------
async function runParallel(suites) {
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
				const r = await runSuite(name);
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
					const r = await runSuite(name);
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

// ---------- 结果聚合 ----------
function readJsonResult(suiteName) {
	const file = path.join(RESULTS_DIR, `${suiteName}.json`);
	if (!existsSync(file)) return null;
	try {
		return JSON.parse(readFileSync(file, "utf8"));
	} catch {
		return null;
	}
}

function aggregateResults(rawResults) {
	let totalPass = 0;
	let totalFail = 0;
	const suiteSummary = [];
	const failedSuites = [];
	const warningSuites = [];

	for (const r of rawResults) {
		const json = readJsonResult(r.suite);
		const passCount = json?.passed ?? 0;
		// 以 JSON 的 failed 数为断言成败的权威依据，而非进程 exit code。
		// 原因：套件断言全过（json.failed===0）后，清理阶段（browser.close()/
		// server.stop()）可能因并行资源争用抛未捕获异常 → 进程 exit≠0。
		// 这种情况断言实际全绿，不应判为套件失败（会致并行模式 flaky）。
		const failCount = json?.failed ?? 0;
		const durationMs = json?.durationMs ?? 0;

		// 套件状态判定：JSON 结果优先；无 JSON（crash/timeout）才退回 exit code。
		let status;
		if (r.status === "timeout") {
			status = "timeout";
		} else if (json) {
			// 有 JSON 结果：按断言失败数判定
			status = failCount === 0 ? "pass" : "fail";
			// exit≠0 但断言全过 → 清理阶段 error，标记 warning 但不算失败
			if (r.status !== "pass" && failCount === 0) {
				status = "pass-warning";
				warningSuites.push(r.suite);
			}
		} else {
			// 无 JSON（进程 crash 未写结果）：按 exit code
			status = r.status === "pass" ? "pass" : "fail";
		}

		totalPass += passCount;
		totalFail += failCount;

		suiteSummary.push({
			suite: r.suite,
			status,
			passed: passCount,
			failed: failCount,
			durationMs,
		});

		if (status === "fail" || status === "timeout") {
			failedSuites.push(r.suite);
		}
	}

	return { totalPass, totalFail, suiteSummary, failedSuites, warningSuites };
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
	console.log("");

	const startMs = Date.now();
	const rawResults = serial
		? await runSerial(suitesToRun)
		: await runParallel(suitesToRun);
	const totalMs = Date.now() - startMs;

	// 聚合
	const agg = aggregateResults(rawResults);

	// 输出汇总
	console.log("==========================================");
	console.log(" 汇总");
	console.log("==========================================");
	console.log(
		`  总耗时: ${(totalMs / 1000).toFixed(1)}s`,
	);
	console.log(`  断言通过: ${agg.totalPass}`);
	console.log(`  断言失败: ${agg.totalFail}`);
	console.log("");

	// 逐套件明细
	for (const s of agg.suiteSummary) {
		const statusIcon =
			s.status === "pass" ? "✅" :
			s.status === "pass-warning" ? "✅" :
			s.status === "timeout" ? "⏱️" : "❌";
		const durStr = s.durationMs > 0 ? `${(s.durationMs / 1000).toFixed(1)}s` : "—";
		const warnTag = s.status === "pass-warning" ? " ⚠️清理异常" : "";
		console.log(
			`  ${statusIcon} ${s.suite.padEnd(18)} ${s.passed}通过/${s.failed}失败  ${durStr}${warnTag}`,
		);
		// 红套件把**第一条失败原因**直接打出来（#210）。
		// 为什么：并行模式不透传套件 stdout，红了只剩一个计数 —— 曾经「0通过/0失败」
		// 让人误判成超时/环境问题，实际是造数时把 undefined 绑进了 sqlite。
		// 一行足够定位，剩下的看 results/<suite>.json。
		if (s.status === "fail" || s.status === "timeout") {
			const json = readJsonResult(s.suite);
			const first = json?.failures?.[0];
			if (first) console.log(`      ↳ ${String(first).slice(0, 300)}`);
		}
	}

	if (agg.warningSuites.length > 0) {
		console.log("");
		console.log(
			`  ⚠️ 清理阶段异常（断言全过，非失败）: ${agg.warningSuites.join(", ")}`,
		);
	}

	if (agg.failedSuites.length > 0) {
		console.log("");
		console.log(`  失败套件: ${agg.failedSuites.join(", ")}`);
	}

	console.log("==========================================");
	if (agg.failedSuites.length > 0) {
		console.log("❌ E2E 未全绿");
		process.exit(1);
	}
	console.log("✅ E2E 全绿");
	process.exit(0);
}

main().catch((err) => {
	console.error("执行器异常:", err);
	process.exit(2);
});
