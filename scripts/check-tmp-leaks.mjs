#!/usr/bin/env node
/* Anotify · 临时目录泄漏守卫（#326）
 *
 * 为什么需要它：本机 `/tmp` 是 **tmpfs（7.5G）**，测试漏下的临时目录吃的是
 * **内存**而不是磁盘。2026-11 一次清理就从 /tmp 回收了 4.5G：
 *   281 个 anotify-agent-bin（cmd/agent 每次跑编一份真二进制）
 *   1424 个 anotify-e2e-*（端口重试循环里创建、失败尝试从不删）
 *   214 个 anotify-script-*（cli_auth 的 HOME）
 * 修一次不够 —— 下次谁新增套件又会漏，所以要有能自动跑、不会误报的守卫。
 *
 * 三种模式：
 *
 *   1) 包住一条命令（默认）：
 *        node scripts/check-tmp-leaks.mjs -- go test ./cmd/agent -count=3
 *      跑前记录快照，跑完断言「本次运行自己的新增 = 0」，有新增就逐个列出来
 *      并 exit 1（即使被包命令自己是成功的）。
 *
 *      为什么不是直接比对 /tmp 的集合差：本机经常**同时**有别的 e2e / 测试在跑
 *      （并行子 agent、另一个 worktree 的 make ci）。它们用的也是
 *      /tmp/anotify-*，集合差会把别人漏的算到你头上 —— 实测发生过：本分支
 *      最后一次全量 e2e 时，另一个 worktree 正跑旧代码的 make e2e，4 个目录
 *      全记到了本次运行的账上。
 *
 *      所以这里做**归因隔离**：守卫给被包命令一个私有的 TMPDIR
 *      （/tmp/.tXXXXXX，名字故意很短），再断言这个私有目录里没有 anotify-* 残留。
 *      Go 的 os.MkdirTemp / t.TempDir 与 Node 的 os.tmpdir() 都跟随 TMPDIR，
 *      所以「我们的泄漏」全部落在私有目录里、与别的进程天然分开；
 *      /tmp 顶层的新增只作为提示打印（不计入失败），并随私有目录一起清理。
 *
 *   2) --lint：静态守卫。`scripts/e2e/` 里只允许 lib/harness.mjs 定义
 *      mkdtempTmp（它内部才用 mkdtempSync）；其余文件出现裸调用即失败。
 *      动态守卫只能抓到「这次真的漏了」，静态守卫拦的是「新套件绕过登记」。
 *
 *   3) --self-test：守卫自己的红/绿/透传/不误报自测（毫秒级，不碰仓库文件）：
 *        红：子进程在私有 TMPDIR 里创建一个 anotify-* 目录不删 → 必须 exit 1 并点名；
 *        绿：子进程创建后删掉 → 必须 exit 0；
 *        不误报：子进程在 /tmp 顶层硬编码创建一个 anotify-* → 判为「非本次运行」，
 *                守卫不红（只提示），并清掉它；
 *        透传：无泄漏时被包命令的退出码原样带出。
 *      守卫坏掉时 CI 会先红，而不是等 /tmp 又被吃满。
 *
 * 用法：
 *   node scripts/check-tmp-leaks.mjs -- <command...>
 *   node scripts/check-tmp-leaks.mjs --lint
 *   node scripts/check-tmp-leaks.mjs --self-test
 *   node scripts/check-tmp-leaks.mjs --prefix=anotify- -- <command...>   # 换前缀
 *
 * 接线：`make ci` 用它包住 `make test`（含 go test ./cmd/agent），
 *       `scripts/e2e/run_all.sh` 用它包住并行执行器。见 E2E_TESTING.md。
 */
import { spawn } from "node:child_process";
import { mkdtempSync, readFileSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = path.resolve(
	path.dirname(fileURLToPath(import.meta.url)),
	"..",
);
const SELF = fileURLToPath(import.meta.url);
const DEFAULT_PREFIX = "anotify-";
// 守卫自己的隔离 TMPDIR（在 /tmp 下，仍然压 tmpfs；跑完立刻删）。
//
// 前缀故意很短且隐藏（/tmp/.tXXXXXX）：unix domain socket 的 sun_path 上限是
// 108 字节，而测试里 socket 路径常常拼在 t.TempDir() 下面。隔离目录若用长名字
// （例如 anotify-guard-XXXXXX），会把「本来刚好放得下」的用例挤爆 —— 实测
// TestEmitJSONSuccessAndTextMode 的 fake.sock 因此 bind: invalid argument。
const GUARD_TMP_PREFIX = ".t";

// snapshotTmp 记录 /tmp 顶层匹配前缀的名字（只记名字，不读内容）。
function snapshotTmp(prefix = DEFAULT_PREFIX) {
	const names = new Set();
	for (const name of readdirSync(tmpdir())) {
		if (name.startsWith(prefix)) names.add(name);
	}
	return names;
}

// runWrapped 跑一条命令，返回：
//   leaked  —— 私有 TMPDIR 里本次新增的 anotify-*（**我们的泄漏**，要判红）
//   outside —— /tmp 顶层本次新增、但不属于私有目录的 anotify-*（别人的，仅提示）
//
// stdio=pipe 只给自测用（正常模式必须 inherit，否则套件输出会被吞掉）。
function runWrapped(argv, { prefix = DEFAULT_PREFIX, stdio = "inherit" } = {}) {
	const guardRoot = mkdtempSync(path.join(tmpdir(), GUARD_TMP_PREFIX));
	const beforeOutside = snapshotTmp(prefix);
	const startedAt = Date.now();

	const collect = () => {
		let leaked = [];
		try {
			leaked = readdirSync(guardRoot)
				.filter((n) => n.startsWith(prefix))
				.sort()
				.map((n) => ({ name: n, path: path.join(guardRoot, n) }));
		} catch {
			/* 私有 TMPDIR 被外部删掉：视为无残留（别让守卫自己崩） */
		}
		const outside = [...snapshotTmp(prefix)]
			.filter((n) => !beforeOutside.has(n) && n !== path.basename(guardRoot))
			.sort();
		return { leaked, outside };
	};

	return new Promise((resolve) => {
		let settled = false;
		let out = "";
		const finish = (code, extra = {}) => {
			if (settled) return;
			settled = true;
			const { leaked, outside } = collect();
			// 私有目录永远清掉：守卫不制造新的 /tmp 垃圾（泄漏证据已记录在返回里）。
			rmSync(guardRoot, { recursive: true, force: true });
			resolve({
				leaked,
				outside,
				code,
				output: out,
				ms: Date.now() - startedAt,
				...extra,
			});
		};

		let child;
		try {
			child = spawn(argv[0], argv.slice(1), {
				stdio,
				// 归因隔离：所有走 os.tmpdir()/tmpdir() 的临时目录都落在这里。
				env: { ...process.env, TMPDIR: guardRoot },
				cwd: process.cwd(),
			});
		} catch (e) {
			finish(127, { error: String(e && e.message ? e.message : e) });
			return;
		}

		if (stdio === "pipe") {
			child.stdout.on("data", (d) => { out += d; });
			child.stderr.on("data", (d) => { out += d; });
		}
		child.on("error", (err) => finish(127, { error: err.message }));
		child.on("exit", (code, signal) => finish(signal ? 1 : (code ?? 1), { signal: signal || undefined }));
	});
}

// report 打印结果并返回是否该红。
function report(r, prefix) {
	if (r.outside.length > 0) {
		// 不计入失败：本次运行的所有临时目录都走私有 TMPDIR，顶层新增按定义
		// 不归因于我们（实测：并行 worktree 跑旧代码时，它漏的目录全落在这里）。
		console.log(
			`  ⚠️ /tmp 顶层另有 ${r.outside.length} 个 ${prefix}* 是运行期间由其它进程创建的，` +
				"已忽略（不计入本次判定）：",
		);
		for (const name of r.outside) console.log(`     - ${path.join(tmpdir(), name)}`);
	}
	if (r.leaked.length === 0) return false;
	console.error("");
	console.error(
		`❌ 临时目录泄漏：本次运行在隔离 TMPDIR 里留下 ${r.leaked.length} 个 ${prefix}*（/tmp 是 tmpfs，占的是内存）`,
	);
	for (const item of r.leaked) console.error(`   - ${item.path}`);
	console.error("   守卫已把隔离目录连同残留一并清掉；请修代码，别靠清理。");
	console.error(
		"   修法：Go 侧用 cmd/agent 的 mkTestTempDir + TestMain 统一删；" +
			"e2e 侧用 H.mkdtempTmp() 创建、finally 里 H.removeTmpDir()。",
	);
	console.error("   约定与兜底见 E2E_TESTING.md「临时目录与 /tmp 卫生」。");
	return true;
}

// ---------- --lint：静态守卫 ----------
function walkFiles(dir) {
	const out = [];
	for (const ent of readdirSync(dir, { withFileTypes: true })) {
		const p = path.join(dir, ent.name);
		if (ent.isDirectory()) out.push(...walkFiles(p));
		else out.push(p);
	}
	return out;
}

function lint() {
	// 唯一的合法调用点是 harness 的 mkdtempTmp 实现本身。
	const allowed = path.join(ROOT, "scripts", "e2e", "lib", "harness.mjs");
	const e2eDir = path.join(ROOT, "scripts", "e2e");
	const violations = [];
	for (const file of walkFiles(e2eDir)) {
		if (!file.endsWith(".mjs") || file === allowed) continue;
		const lines = readFileSync(file, "utf8").split("\n");
		lines.forEach((line, i) => {
			if (line.includes("mkdtempSync")) {
				violations.push({ file: path.relative(ROOT, file), line: i + 1, text: line.trim() });
			}
		});
	}
	if (violations.length > 0) {
		console.error("❌ e2e 里出现了裸 mkdtempSync（必须走 H.mkdtempTmp 登记）：");
		for (const v of violations) {
			console.error(`   - ${v.file}:${v.line}  ${v.text}`);
		}
		console.error("   /tmp 是 tmpfs，泄漏吃内存；创建即登记才能在进程退出时兜底清理。");
		return 1;
	}
	console.log("  ✅ 静态检查：e2e 中没有裸 mkdtempSync（只允许 lib/harness.mjs 的 helper）");
	return 0;
}

// ---------- --self-test：守卫自身的红/绿/不误报/透传自测 ----------
async function selfTest() {
	const prefix = "anotify-guard-selftest-";
	const leakName = prefix + "leak";
	// 子进程在**私有 TMPDIR**里创建目录 = 模拟测试忘记清理。
	const leakInside = `require("node:fs").mkdirSync(require("node:path").join(process.env.TMPDIR, ${JSON.stringify(leakName)}))`;
	// 子进程在 /tmp 顶层硬编码创建 = 模拟“并行进程”的泄漏（不该算到我们头上）。
	const outsideName = prefix + "outside";
	const outsideDir = path.join(tmpdir(), outsideName);
	const leakOutside = `require("node:fs").mkdirSync(${JSON.stringify(outsideDir)})`;
	let failed = 0;
	const check = (name, cond, detail = "") => {
		if (cond) {
			console.log(`  ✅ ${name}`);
		} else {
			failed++;
			console.error(`  ❌ ${name}${detail ? ` — ${detail}` : ""}`);
		}
	};

	rmSync(outsideDir, { recursive: true, force: true });
	try {
		// ① 红：私有 TMPDIR 里漏一个目录 → 必须被识别。
		const red = await runWrapped([process.execPath, "-e", leakInside], {
			prefix,
			stdio: "pipe",
		});
		check(
			"红：私有 TMPDIR 里的泄漏被点名",
			red.leaked.some((x) => x.name === leakName),
			JSON.stringify(red.leaked),
		);
		check("红：被包命令自身是成功的（异常只来自守卫）", red.code === 0, `code=${red.code}`);

		// ② CLI 级红：真走一遍守卫进程，退出码必须 1 且点名泄漏目录。
		const cliRed = await runWrapped(
			[process.execPath, SELF, `--prefix=${prefix}`, "--", process.execPath, "-e", leakInside],
			{ prefix, stdio: "pipe" },
		);
		check("红：CLI 退出码 1", cliRed.code === 1, `code=${cliRed.code}`);
		check(
			"红：CLI 输出点名泄漏目录",
			cliRed.output.includes(leakName),
			cliRed.output.slice(-300),
		);

		// ③ 绿：创建后立刻删掉 → 本次新增为 0。
		const cleanScript =
			`const fs = require("node:fs"), path = require("node:path");` +
			`const d = fs.mkdtempSync(path.join(process.env.TMPDIR, ${JSON.stringify(prefix)}));` +
			`fs.rmSync(d, { recursive: true, force: true });`;
		const green = await runWrapped([process.execPath, "-e", cleanScript], {
			prefix,
			stdio: "pipe",
		});
		check("绿：自己清理干净的目录不算泄漏", green.leaked.length === 0, JSON.stringify(green.leaked));
		check("绿：退出码 0", green.code === 0, `code=${green.code}`);

		// ④ 不误报：/tmp 顶层的新增（不是我们私有 TMPDIR 里的）只提示、不判红。
		const outsideRun = await runWrapped([process.execPath, "-e", leakOutside], {
			prefix,
			stdio: "pipe",
		});
		check("不误报：/tmp 顶层新增不算本次泄漏", outsideRun.leaked.length === 0, JSON.stringify(outsideRun.leaked));
		check(
			"不误报：/tmp 顶层新增被单独列为提示",
			outsideRun.outside.includes(outsideName),
			JSON.stringify(outsideRun.outside),
		);
		check("不误报：退出码仍为 0", outsideRun.code === 0, `code=${outsideRun.code}`);

		// ⑤ 透传：没有泄漏时，被包命令的退出码原样带出（CI 不能吞掉测试失败）。
		const pass3 = await runWrapped([process.execPath, "-e", "process.exit(3)"], {
			prefix,
			stdio: "pipe",
		});
		check("透传：无泄漏时退出码原样（3）", pass3.code === 3, `code=${pass3.code}`);
		check("透传：无泄漏", pass3.leaked.length === 0, JSON.stringify(pass3.leaked));
	} finally {
		rmSync(outsideDir, { recursive: true, force: true });
	}

	if (failed > 0) {
		console.error(`❌ 守卫自测失败（${failed} 项）—— 先修守卫，别让它假绿`);
		return 1;
	}
	console.log("  ✅ 守卫自测通过（红/绿/不误报/透传四条路都钉住了）");
	return 0;
}

// ---------- CLI ----------
function usage() {
	console.error(
		[
			"用法：",
			"  node scripts/check-tmp-leaks.mjs -- <command...>   跑命令并断言本次没有新增 anotify-* 残留",
			"  node scripts/check-tmp-leaks.mjs --lint             静态检查 e2e 里没有裸 mkdtempSync",
			"  node scripts/check-tmp-leaks.mjs --self-test        守卫自身的红/绿/不误报/透传自测",
			"  --prefix=<p>                                       自定义临时目录前缀（默认 anotify-）",
		].join("\n"),
	);
}

async function main() {
	const argv = process.argv.slice(2);
	let prefix = DEFAULT_PREFIX;
	let mode = "run";
	let command = [];
	for (let i = 0; i < argv.length; i++) {
		const a = argv[i];
		if (a === "--lint") mode = "lint";
		else if (a === "--self-test") mode = "self-test";
		else if (a === "--help" || a === "-h") mode = "help";
		else if (a.startsWith("--prefix=")) prefix = a.slice("--prefix=".length);
		else if (a === "--") {
			command = argv.slice(i + 1);
			break;
		} else {
			command = argv.slice(i);
			break;
		}
	}

	if (mode === "help") {
		usage();
		return 0;
	}
	if (mode === "lint") return lint();
	if (mode === "self-test") return selfTest();

	if (command.length === 0) {
		usage();
		return 2;
	}

	const r = await runWrapped(command, { prefix });
	const leaked = report(r, prefix);
	if (leaked) return 1; // 泄漏优先：别让被包命令的失败/成功掩盖它
	if (r.error) {
		console.error(`❌ 命令启动失败: ${r.error}`);
		return 127;
	}
	if (r.signal) {
		console.error(`❌ 命令被信号中断: ${r.signal}`);
		return 1;
	}
	console.log(
		`  ✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0，耗时 ${(r.ms / 1000).toFixed(1)}s）`,
	);
	return r.code;
}

main()
	.then((code) => {
		process.exitCode = code;
	})
	.catch((e) => {
		console.error(e);
		process.exitCode = 1;
	});
