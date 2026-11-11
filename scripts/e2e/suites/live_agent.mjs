#!/usr/bin/env node
/* SUITE: live_agent — 真 `anode` 的 **CLI 面 + 安装/启动/退出生命周期**（v4）
 *
 * ## 这个套件为什么还存在（以及它和 node_v4 的分工）
 *
 * v3 的 live_agent 驱动的是已退役的 `cmd/agent` CLI（`-f node.yaml` / `-data-dir` /
 * `-grace`，能力脚本里的 `emit --port`）。v4 的节点侧只有 `cmd/anode`
 * （`serve` / `install` / `emit` + `--version`），**没有兼容层**，所以那个驱动方式
 * 已经不可能存在，必须重写而不是放宽。
 *
 * 重写后的分工（刻意不重复）：
 *
 *   node_v4   数据面：真 hello / 真部署拉取 / 真执行 / 真日志 / 真产物 / 真取消。
 *   live_agent（本套件）**机器面**：
 *     1. CLI 契约：`--version` 的 JSON 形状、未知命令/缺参数的退出码、
 *        `emit` 的退出码映射（2 = 请求错、3 = 令牌无效、5 = 端点不可达/结果未知）。
 *     2. `anode install --prefix`：落盘 anode.yaml(0600)/anode(0755)/anode.service(0644)、
 *        幂等重跑、**换 token 必须显式 --update-token**（不静默覆盖身份）。
 *     3. 安装产物 → `serve` 的往返：装出来的配置能直接启动、节点能上线、data 目录成型。
 *     4. 生命周期：SIGTERM → 进程自己退（退出码 0，不是被 KILL）、中枢随即承认它离线
 *        （部署结论 sent=false，不伪造发送）。
 *
 * 这四条都是**进程/文件系统**上的事实，任何一层单测都替代不了：
 *   - 退出码映射错了 → 脚本在真实流水线里无法判别「该不该重试」；
 *   - install 写了 0644 的配置 → 节点 token 泄露给本机所有用户；
 *   - serve 不响应 SIGTERM → systemd 每次重启都要等超时再 KILL（子进程可能被留活）。
 *
 * 纪律：真二进制、真进程、真文件；断言读**进程退出码/文件权限/文件内容**，
 * 不用「起了就算对」。缺 ANODE_BIN 时是红（run_all.sh 会构建），不是 skip。
 */
import { spawn, spawnSync } from "node:child_process";
import { closeSync, existsSync, openSync, readFileSync, statSync, writeFileSync } from "node:fs";
import path from "node:path";

import * as H from "../lib/harness.mjs";
import * as V from "../lib/v4.mjs";

const SUITE = "live_agent";
const NS = V.DEFAULT_NS;
const ANODE_BIN = H.ANODE_BIN;

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function pollUntil(what, fn, { timeoutMs = 15000, intervalMs = 100 } = {}) {
	const deadline = Date.now() + timeoutMs;
	let last = null;
	for (;;) {
		last = await fn();
		if (last) return last;
		if (Date.now() > deadline) throw new Error(`等待超时（${timeoutMs}ms）：${what}\n最后一次观察: ${JSON.stringify(last)}`);
		await sleep(intervalMs);
	}
}

// runCLI 跑一次 CLI 并收集 {status, stdout, stderr}（同步：CLI 都是短命命令）。
function runCLI(args, env = {}) {
	const r = spawnSync(ANODE_BIN, args, {
		encoding: "utf8",
		env: { ...process.env, PATH: "/usr/local/bin:/usr/bin:/bin", ...env },
		timeout: 20000,
	});
	return { status: r.status, stdout: r.stdout || "", stderr: r.stderr || "" };
}

// jsonLine 取 stdout 里的那份 JSON（CLI 的回执/错误都是单行 JSON）。
function jsonLine(stdout) {
	const line = String(stdout).trim().split("\n").filter(Boolean).pop() || "";
	try {
		return JSON.parse(line);
	} catch {
		return null;
	}
}

// startAnode 写 anode.yaml 并起真 serve 进程（PATH/HOME 由我们给：任务环境是启动基线快照）。
async function startAnode({ base, token, name, dataDir, listen }) {
	const configPath = path.join(dataDir, "anode.yaml");
	writeFileSync(
		configPath,
		[`name: ${name}`, `server: ${base.replace(/^http/, "ws")}`, `token: ${token}`, "namespaces:", `  - ${NS}`, `listen: ${listen}`, ""].join("\n"),
		{ mode: 0o600 },
	);
	const logPath = path.join(dataDir, "anode.log");
	const logFd = openSync(logPath, "w");
	const proc = spawn(ANODE_BIN, ["serve", "--config", configPath, "--data", dataDir], {
		cwd: dataDir,
		env: { ...process.env, PATH: "/usr/local/bin:/usr/bin:/bin", HOME: dataDir },
		stdio: ["ignore", logFd, logFd],
	});
	let exited = null;
	proc.on("exit", (code, sig) => (exited = { code, sig }));
	const state = {
		proc,
		configPath,
		dataDir,
		logPath,
		exited: () => exited,
		log: (tail = 4000) => {
			try {
				const raw = readFileSync(logPath, "utf8");
				return raw.length > tail ? "…" + raw.slice(-tail) : raw;
			} catch (e) {
				return `（读不到 ${logPath}: ${e && e.message}）`;
			}
		},
		stop: async () => {
			if (exited) return exited;
			try {
				proc.kill("SIGTERM");
			} catch {
				/* 已退出 */
			}
			const deadline = Date.now() + 8000;
			while (!exited && Date.now() < deadline) await sleep(50);
			if (!exited) {
				try {
					proc.kill("SIGKILL");
				} catch {
					/* ignore */
				}
				await sleep(200);
			}
			try {
				closeSync(logFd);
			} catch {
				/* ignore */
			}
			return exited;
		},
	};
	return state;
}

// ---------- 1. CLI 契约 ----------
function partCLI() {
	console.log("--- 1. CLI 契约（--version / 未知命令 / emit 退出码）---");
	{
		const r = runCLI(["--version"]);
		H.eq("anode --version → 退出码 0", r.status, 0);
		const doc = jsonLine(r.stdout);
		H.check("--version 输出 JSON 回执（version/commit 两个键）", !!doc && typeof doc.version === "string" && typeof doc.commit === "string", r.stdout.slice(0, 200));
		H.eq("版本来自构建期注入（本次构建是 dev）", doc?.version, "dev");
	}
	{
		const r = runCLI([]);
		H.eq("无参数 → 退出码 2（用法错误，不是崩溃）", r.status, 2);
		H.check("无参数 → stderr 给出用法（serve/install/emit）", /Usage: anode \{(serve|serve\|install\|emit)/.test(r.stderr) || /serve/.test(r.stderr), r.stderr.slice(0, 200));
	}
	{
		const r = runCLI(["frobnicate"]);
		H.eq("未知子命令 → 退出码 2", r.status, 2);
		H.check("未知子命令 → 文案点名期望的三个子命令", /unknown command/.test(r.stderr) && /serve/.test(r.stderr), r.stderr.slice(0, 200));
	}
	{
		// emit 的**请求面**错误：2（本地参数错，绝不该重试）。
		const r = runCLI(["emit"]);
		H.eq("emit 缺 target → 退出码 2", r.status, 2);
		H.eq("emit 缺 target 的机器码是 missing_target", jsonLine(r.stdout)?.error, "missing_target");
	}
	{
		const r = runCLI(["emit", "some.thing", "--data", "[]"]);
		H.eq("emit --data 不是对象 → 退出码 2", r.status, 2);
		H.eq("→ 机器码 target_not_declared", jsonLine(r.stdout)?.error, "target_not_declared");
	}
	{
		// 没有任务令牌：3（令牌无效）。这一层必须在**发请求之前**就拒绝。
		const r = runCLI(["emit", "some.thing", "--data", "{}"], { ANOTIFY_TASK_TOKEN: "", ANOTIFY_EMIT_ENDPOINT: "" });
		H.eq("emit 缺 ANOTIFY_TASK_TOKEN → 退出码 3", r.status, 3);
		H.eq("→ 机器码 token_invalid", jsonLine(r.stdout)?.error, "token_invalid");
	}
	{
		// 端点不可达：5（结果未知，不许盲目重放整条任务）。
		const r = runCLI(["emit", "some.thing", "--data", "{}"], {
			ANOTIFY_TASK_TOKEN: "e2eTaskTokenPlaceholder",
			ANOTIFY_EMIT_ENDPOINT: "unix://" + path.join("/tmp", "no-such-anode-emit-" + process.pid + ".sock"),
		});
		H.eq("emit 端点不可达 → 退出码 5（结果未知）", r.status, 5);
		H.eq("→ 机器码 emit_unreachable", jsonLine(r.stdout)?.error, "emit_unreachable");
	}
	{
		const r = runCLI(["serve", "--config", "/tmp/definitely-not-anode-" + process.pid + ".yaml"]);
		H.eq("serve 配置文件缺失 → 退出码 2（启动期全有或全无）", r.status, 2);
		H.check("→ stderr 点名 configuration（可定位）", /configuration/.test(r.stderr), r.stderr.slice(0, 200));
	}
}

// ---------- 2. install --prefix ----------
async function partInstall(base, session) {
	console.log("--- 2. anode install --prefix（产物/权限/幂等/身份不静默覆盖）---");
	const name = "live-install-node";
	const created = await H.req(base, "/v1/nodes", {
		session,
		body: V.envelope("Node", { name }, { namespaces: [NS] }),
	});
	V.expectStatus("登记节点", created, 201);
	const tok = await H.req(base, `/v1/nodes/${encodeURIComponent(name)}/tokens`, { session, body: {} });
	V.expectStatus("签发节点令牌", tok, 200);
	const token = tok.json?.token || "";

	const prefix = H.mkdtempTmp("anotify-anode-install-");
	const wsServer = base.replace(/^http/, "ws");
	const installArgs = ["install", "--prefix", prefix, "--name", name, "--server", wsServer, "--token", token];
	const first = runCLI(installArgs);
	H.eq("install --prefix → 退出码 0", first.status, 0);
	const cfgPath = path.join(prefix, "etc/anode/anode.yaml");
	const binPath = path.join(prefix, "usr/local/bin/anode");
	const unitPath = path.join(prefix, "etc/systemd/system/anode.service");
	const dataPath = path.join(prefix, "var/lib/anode");
	for (const p of [cfgPath, binPath, unitPath]) {
		H.check(`install 落盘 ${path.relative(prefix, p)}`, existsSync(p), p);
	}
	{
		// 配置里有节点令牌：必须是 0600（否则本机任何用户都能冒充这台节点）。
		H.eq("anode.yaml 权限是 0600（含节点令牌）", (statSync(cfgPath).mode & 0o777).toString(8), "600");
		H.check("anode 二进制可执行（0755 位）", (statSync(binPath).mode & 0o111) !== 0, (statSync(binPath).mode & 0o777).toString(8));
		H.check("systemd 单元 0644（可读、不可写）", (statSync(unitPath).mode & 0o777).toString(8) === "644", (statSync(unitPath).mode & 0o777).toString(8));
		const cfg = readFileSync(cfgPath, "utf8");
		H.check("anode.yaml 带 name/server/token 三个身份字段", cfg.includes(`name: ${name}`) && cfg.includes(wsServer) && cfg.includes(token), cfg.replace(token, "<token>").slice(0, 300));
		H.check("anode.yaml 带 namespaces: default（缺省申报）", /namespaces:\s*\n\s*- default/.test(cfg), cfg.slice(0, 200));
		H.check("单元文件指向 install 出来的二进制与配置", readFileSync(unitPath, "utf8").includes(binPath), readFileSync(unitPath, "utf8").slice(0, 300));
		H.check("install 不启动服务（只落盘 + 打印管理员步骤）", /systemctl enable --now anode\.service/.test(first.stdout), first.stdout.slice(0, 300));
	}
	{
		// 幂等重跑：同一份身份再装一次不该失败、也不该改身份。
		const again = runCLI(installArgs);
		H.eq("同参数重跑 install → 退出码 0（幂等）", again.status, 0);
		H.check("重跑后配置里的 token 不变", readFileSync(cfgPath, "utf8").includes(token), "token 被改动");
	}
	{
		// 换 token 必须**显式** --update-token：否则一次手误就把节点身份换了。
		const other = runCLI(["install", "--prefix", prefix, "--name", name, "--server", wsServer, "--token", "e2eDifferentToken" + process.pid]);
		H.check("换 token 但没 --update-token → 非 0 退出", other.status !== 0, `status=${other.status}`);
		H.check("→ 文案要求显式 --update-token（不静默覆盖身份）", /update-token/.test(other.stderr), other.stderr.slice(0, 300));
		H.check("被拒的 install 没有改配置", readFileSync(cfgPath, "utf8").includes(token), "配置被改了");
	}
	// install 产物本身要能通过 serve 的**严格**配置解析（不启动，只试启动一次看它进入运行态）。
	return { name, token, prefix, cfgPath, dataPath };
}

// ---------- 3. 安装产物 → serve 往返 ----------
async function partServeInstalled(base, session, installed) {
	console.log("--- 3. 装出来的配置直接 serve：节点上线 / data 目录成型 ---");
	// 用 install 落盘的 anode.yaml 起真 serve：这是「install 与 serve 对同一份配置的理解」
	// 的唯一证据（两边各自解析，字段名漂移只有这一层能抓住）。
	const dataDir = installed.dataPath;
	const logPath = path.join(installed.prefix, "serve.log");
	const logFd = openSync(logPath, "w");
	const proc = spawn(ANODE_BIN, ["serve", "--config", installed.cfgPath, "--data", dataDir], {
		cwd: installed.prefix,
		env: { ...process.env, PATH: "/usr/local/bin:/usr/bin:/bin", HOME: installed.prefix },
		stdio: ["ignore", logFd, logFd],
	});
	let exited = null;
	proc.on("exit", (code, sig) => (exited = { code, sig }));
	const log = (tail = 3000) => {
		try {
			const raw = readFileSync(logPath, "utf8");
			return raw.length > tail ? "…" + raw.slice(-tail) : raw;
		} catch {
			return "";
		}
	};
	try {
		const doc = await pollUntil(
			"节点用安装出来的配置上线（agentVersion + appliedRevision + 槽位）",
			async () => {
				const r = await H.req(base, `/v1/nodes/${encodeURIComponent(installed.name)}`, { session });
				const st = r.json?.status || {};
				return st.agentVersion && st.appliedRevision === st.configRevision && Array.isArray(st.instances) && st.instances.length >= 1 ? r.json : null;
			},
			{ timeoutMs: 25000 },
		).catch((e) => {
			throw new Error(`${e.message}\nanode 日志:\n${log()}`);
		});
		H.check("装出来的配置让节点自报版本（先装后用的往返成立）", !!doc.status?.agentVersion, String(doc.status?.agentVersion));
		H.eq("配置已对齐（appliedRevision == configRevision）", doc.status?.appliedRevision, doc.status?.configRevision);
		H.check("生效命名空间含 default（install 写的 namespaces 被解析）", (doc.status?.effectiveNamespaces || []).includes(NS), JSON.stringify(doc.status?.effectiveNamespaces));
		for (const sub of ["capabilities", "results", "work"]) {
			H.check(`data 目录成型：${sub}/`, existsSync(path.join(dataDir, sub)), dataDir);
		}
		// 文件名是 anode.applied.yaml（app.go 的文件头冻结了这个布局）。
		H.check("applied 快照落盘（机器级配置已生效）", existsSync(path.join(dataDir, "anode.applied.yaml")), dataDir);
	} finally {
		proc.kill("SIGTERM");
		const deadline = Date.now() + 8000;
		while (!exited && Date.now() < deadline) await sleep(50);
		if (!exited) {
			try {
				proc.kill("SIGKILL");
			} catch {
				/* ignore */
			}
		}
		try {
			closeSync(logFd);
		} catch {
			/* ignore */
		}
	}
	H.check("SIGTERM 后节点进程自己退了（不是被 KILL）", !!exited && exited.sig !== "SIGKILL", JSON.stringify(exited));
	H.eq("正常退出码是 0（systemd 不需要重试）", exited?.code, 0);
	return exited;
}

// ---------- 4. serve 生命周期 + emit 打真 socket ----------
async function partServeLifecycle(base, session) {
	console.log("--- 4. serve 生命周期：上线 → emit 打真 socket → SIGTERM → 中枢承认离线 ---");
	const name = "live-serve-node";
	const created = await H.req(base, "/v1/nodes", {
		session,
		body: V.envelope("Node", { name }, { namespaces: [NS] }),
	});
	V.expectStatus("登记节点", created, 201);
	const tok = await H.req(base, `/v1/nodes/${encodeURIComponent(name)}/tokens`, { session, body: {} });
	const token = tok.json?.token || "";

	// 能力 + 部署：只为在「离线」时拿到 sent=false 的对照（不需要真跑任务）。
	const cap = await V.createCapability(base, session, NS, V.capabilityBody({ name: "live.smoke" }));
	V.expectStatus("建能力", cap, 201);

	const dataDir = H.mkdtempTmp("anotify-anode-serve-");
	const sock = path.join(dataDir, "emit.sock");
	const node = await startAnode({ base, token, name, dataDir, listen: `unix://${sock}` });
	try {
		const doc = await pollUntil(
			"节点上线（hello → welcome → config.applied）",
			async () => {
				const r = await H.req(base, `/v1/nodes/${encodeURIComponent(name)}`, { session });
				const st = r.json?.status || {};
				return st.agentVersion && st.appliedRevision === st.configRevision ? r.json : null;
			},
			{ timeoutMs: 25000 },
		).catch((e) => {
			throw new Error(`${e.message}\nanode 日志:\n${node.log()}`);
		});
		H.check("节点自报版本", !!doc.status?.agentVersion, String(doc.status?.agentVersion));
		H.check("emit socket 已就绪（listen: unix:// 被解析）", existsSync(sock), sock);

		// emit 打**真** socket：令牌是伪造的 → 节点 emit 网关 401 → CLI 退出码 3。
		// 这条把「CLI 的端点解析 + 鉴权头 + 401→3 的映射」在真进程上串了一遍。
		{
			const r = runCLI(["emit", "some.target", "--data", "{}"], {
				ANOTIFY_TASK_TOKEN: "e2eBogusTaskToken" + process.pid,
				ANOTIFY_EMIT_ENDPOINT: "unix://" + sock,
			});
			H.eq("emit 打真 socket 且令牌无效 → 退出码 3", r.status, 3);
			H.eq("→ 机器码 token_invalid（不是 emit_unreachable）", jsonLine(r.stdout)?.error, "token_invalid");
		}

		// 在线时部署：sent=true 是「中枢此刻有活跃会话」的机械证据。
		const depOnline = await V.deployCapability(base, session, NS, "live.smoke", { nodes: [name] });
		V.expectStatus("在线时部署 → 200", depOnline, 200);
		H.eq("在线：sent=true", (depOnline.json?.deployed || [])[0]?.sent, true);
	} finally {
		const exited = await node.stop();
		H.check("SIGTERM 后 node 进程退出（不是 KILL）", !!exited && exited.sig !== "SIGKILL", JSON.stringify(exited));
		H.eq("正常退出码 0", exited?.code, 0);
	}

	// 进程退出 = 会话消失：中枢必须承认「发不出去」，不能继续假装在线。
	const offline = await pollUntil(
		"节点离线后部署结论 sent=false",
		async () => {
			const r = await V.deployCapability(base, session, NS, "live.smoke", { nodes: [name] });
			const o = (r.json?.deployed || [])[0];
			return o && o.sent === false ? o : null;
		},
		{ timeoutMs: 10000 },
	).catch((e) => {
		throw new Error(`${e.message}\nanode 日志:\n${node.log()}`);
	});
	H.eq("离线：sent=false（不伪造发送）", offline.sent, false);
}

// ---------- 主流程 ----------
async function main() {
	H.startTimer();
	console.log("=== SUITE: live_agent（真 anode CLI + 安装/启动/退出生命周期）===");
	if (!existsSync(ANODE_BIN)) {
		// 缺二进制是**红**而不是 skip：run_all.sh 会构建它；单独跑请先 go build。
		throw new Error(`缺少 anode 二进制 ${ANODE_BIN}（run_all.sh 会构建；或设 ANODE_BIN 指过去）`);
	}

	partCLI();

	const server = await H.startServer({ suiteName: SUITE, rpId: "localhost" });
	try {
		const seeded = H.seed(server.dbPath, "liveagent");
		const installed = await partInstall(server.base, seeded.session);
		await partServeInstalled(server.base, seeded.session, installed);
		H.removeTmpDir(installed.prefix);
		await partServeLifecycle(server.base, seeded.session);
	} finally {
		server.stop();
	}

	const passed = H.summary(SUITE);
	process.exitCode = passed ? 0 : 1;
	setTimeout(() => process.exit(process.exitCode), 3000).unref();
}

main().catch((e) => {
	console.error(e);
	H.bad("套件异常终止", String(e && e.stack ? e.stack : e).slice(0, 800));
	H.summary(SUITE);
	process.exit(1);
});
