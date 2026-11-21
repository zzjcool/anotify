#!/usr/bin/env node
/* Anotify · 能力样例检查
 *
 * 拦的事故
 *   examples/capabilities/*.yaml 是给用户抄的「run.workDir 到底怎么写」的实物。
 *   样例没有执行者复算，所以它只能随时间变错：把 workDir 删掉、把 requires.values 清空、
 *   把被 workDir 引用的 params.project 删掉，YAML 仍然「看起来对」，用户抄走的却是写入期
 *   会被 400 拒绝的文档 —— 或者更隐蔽：被改成「默认目录」版本后，样例再也证明不了它标题
 *   里写的那件事，而没有人会注意到。
 *
 * 判据
 *   a. 静态下限（防假绿）：样例数 ≥ FILE_FLOOR；每个样例必须含 `workDir:`；每个样例的
 *      key 数（按 `键:` 行数近似，下界）≥ KEY_FLOOR。达不到就是 findings。
 *   b. 执行：所有样例真的过 `internal/artifact` 的 Parse —— 复用仓库已有的样例门
 *      （internal/playbook/examples_test.go 的同款思路），入口是
 *      `go test ./internal/artifact -run TestExampleCapabilities`，本脚本不复制解析逻辑。
 *   c. 变异自证（--self-test）：在临时副本上做三处破坏（删 workDir / 删 requires.values /
 *      删被引用的 params.project），每一处都必须让 b 失败并出现对应短码；未破坏的副本必须
 *      通过，证明判据不是恒红。
 *
 * 用法
 *   node scripts/check-examples.mjs              # 体检（CI 用）
 *   node scripts/check-examples.mjs --self-test  # 判据自证（临时副本上做变异）
 *   退出码：0 = 通过 / 1 = 有 findings / 2 = 用法或环境问题（含「样例没扫到」「变异没生效」）
 */
import { spawnSync } from "node:child_process";
import { promises as fs } from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { runsAsMain } from "./lib/direct-run.mjs";

/** 仓库根（按脚本位置推，不依赖 cwd）。 */
export const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
/** 样例目录。 */
export const EXAMPLES_DIR = path.join(ROOT, "examples", "capabilities");
/** 样例数下限：两份可对照的写法（vars 引用 / params 引用）。 */
export const FILE_FLOOR = 2;
/** 每个样例的 key 数下限：实测 30 / 29，删掉半份样例就掉下来。 */
export const KEY_FLOOR = 18;
/** 复用现成校验入口：仓库里已经有「示例真过 Parse」的 Go 测试，这里只负责喂目录与比对。 */
export const GO_TEST = ["test", "./internal/artifact", "-run", "^TestExampleCapabilities$", "-count=1"];

/** 规则短码（findings 用；也是 --self-test 的断言对象）。 */
export const CODES = {
	MISSING_WORKDIR: "EXAMPLE_MISSING_WORKDIR",
	TOO_THIN: "EXAMPLE_TOO_THIN",
	INVALID: "EXAMPLE_INVALID",
	FLOOR: "EXAMPLE_FLOOR",
	SELFTEST_FAILED: "EXAMPLE_SELFTEST_FAILED",
};

/**
 * 纯函数：数一份 YAML 里的 `键:` 行（下界近似）。注释行与列表项不算。
 * 只用来挡住「样例被删成一行」，不追求完整 YAML 语义（语义由 Parse 判）。
 */
export function countKeys(text) {
	let n = 0;
	for (const line of String(text).split("\n")) {
		const stripped = line.replace(/^[ \t]+/, "");
		if (stripped.startsWith("#")) continue;
		if (/^[A-Za-z_][\w.-]*\s*:/.test(stripped)) n += 1;
	}
	return n;
}

/** 纯函数：单个样例的静态判据。返回 { keys, findings }。 */
export function scanExample(file, text) {
	const findings = [];
	const keys = countKeys(text);
	if (!/^[ \t]*workDir:[ \t]*\S/m.test(text)) {
		findings.push({
			rule: CODES.MISSING_WORKDIR,
			file,
			message: "样例里没有 workDir：这个目录专门演示「在已有目录里跑」，删掉它就退化成默认目录样例",
			fix: "把 run.workDir 写回来（$(vars.X) 或 /abs/$(params.X)），别用默认目录替代",
		});
	}
	if (keys < KEY_FLOOR) {
		findings.push({
			rule: CODES.TOO_THIN,
			file,
			message: `样例只剩 ${keys} 个键（下限 ${KEY_FLOOR}）：它已经薄到抄不出完整能力，形同虚设`,
			fix: "把样例补回可抄的完整形状（metadata / params / requires / run / lifecycle / concurrency）",
		});
	}
	return { keys, findings };
}

/** 跑一次复用入口：把 dir 交给 Go 测试里的 Parse。 */
export function runExampleTest(dir) {
	const r = spawnSync("go", GO_TEST, {
		cwd: ROOT,
		encoding: "utf8",
		env: { ...process.env, ANOTIFY_EXAMPLES_DIR: dir },
		timeout: 300_000,
	});
	const output = `${r.stdout ?? ""}${r.stderr ?? ""}`;
	return { ok: r.status === 0, status: r.status, output, timedOut: r.error?.code === "ETIMEDOUT" };
}

/* ────────────────────────── 变异（--self-test） ────────────────────────── */

/** 删除第一处匹配 `键:` 的整行；没匹配到就返回 null（调用方判「变异没生效」）。 */
function dropKeyLine(text, key) {
	const re = new RegExp(`^[ \\t]*${key}:[^\\n]*\\n`, "m");
	const out = text.replace(re, "");
	return out === text ? null : out;
}

/** 删除第一处匹配 `键:`（独占一行）的整块：到下一个同级或更浅缩进的行为止。 */
function dropKeyBlock(text, key) {
	const lines = String(text).split("\n");
	const i = lines.findIndex((l) => new RegExp(`^[ \\t]*${key}:[ \\t]*$`).test(l));
	if (i < 0) return null;
	const indent = lines[i].match(/^[ \t]*/)[0].length;
	let j = i + 1;
	while (j < lines.length) {
		if (lines[j].trim() === "") {
			j += 1;
			continue;
		}
		const cur = lines[j].match(/^[ \t]*/)[0].length;
		if (cur <= indent) break;
		j += 1;
	}
	return lines.slice(0, i).concat(lines.slice(j)).join("\n");
}

/** 三处破坏：每一处都必须让复用入口失败，且错误里出现 expect。 */
export const MUTATIONS = [
	{
		id: "workdir-removed",
		file: "run-in-repo.yaml",
		expect: "workDir",
		apply: (text) => dropKeyLine(text, "workDir"),
	},
	{
		id: "requires-values-removed",
		file: "run-in-repo.yaml",
		expect: "undeclared_need",
		apply: (text) => dropKeyLine(text, "values"),
	},
	{
		id: "params-project-removed",
		file: "run-per-param-dir.yaml",
		expect: "undefined_param_ref",
		apply: (text) => dropKeyBlock(text, "project"),
	},
];

/* ────────────────────────── 主流程 ────────────────────────── */

async function loadExamples() {
	let names;
	try {
		names = (await fs.readdir(EXAMPLES_DIR)).filter((f) => f.endsWith(".yaml")).sort();
	} catch (err) {
		console.error(`✗ 读不到样例目录 ${EXAMPLES_DIR}: ${err.message}`);
		process.exit(2);
	}
	const files = [];
	for (const name of names) {
		files.push({ name, text: await fs.readFile(path.join(EXAMPLES_DIR, name), "utf8") });
	}
	return files;
}

async function main() {
	const problems = [];
	const files = await loadExamples();
	console.log(`扫面：${path.relative(ROOT, EXAMPLES_DIR)} 下 ${files.length} 个样例`);
	if (files.length < FILE_FLOOR) {
		console.error(`✗ 样例数 ${files.length} < 下限 ${FILE_FLOOR}：样例被删掉时本检查不能空跑`);
		process.exit(2);
	}

	for (const f of files) {
		const { keys, findings } = scanExample(f.name, f.text);
		console.log(`  ${f.name}: key 数 ${keys}`);
		problems.push(...findings);
	}

	const run = runExampleTest(EXAMPLES_DIR);
	if (!run.ok) {
		problems.push({
			rule: CODES.INVALID,
			file: "examples/capabilities/*.yaml",
			message: "样例过不了 artifact.Parse（用户抄下来就会被写入期拒绝）",
			fix: "看下面 go test 输出里的短码修样例；不要改校验逻辑或改断言迁就样例",
			detail: run.output.trim(),
		});
	}

	if (problems.length > 0) {
		console.log("");
		for (const p of problems) {
			console.log(`✗ ${p.rule} ${p.file}\n    ${p.message}\n    修法：${p.fix}`);
			if (p.detail) console.log(p.detail.split("\n").map((l) => `    ${l}`).join("\n"));
		}
		process.exit(1);
	}
	console.log("✅ 样例全部过 artifact.Parse，且都带 workDir");
}

async function selfTest() {
	let failed = 0;
	const check = (ok, label, detail = "") => {
		console.log(`${ok ? "✅" : "❌"} ${label}${ok ? "" : ` —— ${detail}`}`);
		if (!ok) failed += 1;
	};

	// 1) 静态判据的红/绿两侧（纯函数，不碰磁盘）
	const bare = scanExample("x.yaml", "apiVersion: x\n");
	check(
		bare.findings.some((f) => f.rule === CODES.MISSING_WORKDIR),
		"静态：没有 workDir 必须报出来",
		JSON.stringify(bare),
	);
	const thin = scanExample("x.yaml", "apiVersion: x\nrun:\n  workDir: /srv/a\n");
	check(
		thin.findings.some((f) => f.rule === CODES.TOO_THIN),
		"静态：样例过薄必须报出来",
		JSON.stringify(thin),
	);
	const fat = ["run:", "  workDir: /srv/a"]
		.concat(Array.from({ length: KEY_FLOOR }, (_, i) => `  k${i}: v`))
		.join("\n");
	const fatScan = scanExample("x.yaml", fat);
	check(fatScan.findings.length === 0, "静态：完整样例不误报", JSON.stringify(fatScan));

	// 2) 真实样例 + 变异副本：每一处破坏都必须让复用入口失败
	const tmpRoot = await fs.mkdtemp(path.join(os.tmpdir(), "anotify-examples-selftest-"));
	try {
		const copy = async (name) => {
			const dir = path.join(tmpRoot, name);
			await fs.cp(EXAMPLES_DIR, dir, { recursive: true });
			return dir;
		};

		const green = await copy("green");
		const greenRun = runExampleTest(green);
		check(greenRun.ok, "副本（未破坏）必须通过", greenRun.output.trim().slice(0, 800));

		for (const m of MUTATIONS) {
			const dir = await copy(m.id);
			const target = path.join(dir, m.file);
			const before = await fs.readFile(target, "utf8");
			const after = m.apply(before);
			if (after === null || after === before) {
				console.error(
					`✗ 变异 ${m.id} 没生效：${m.file} 里找不到可破坏的结构。` +
						"样例改了形状就让这条变异失效了 —— 请同步更新本脚本的变异实现（不要删掉变异）",
				);
				process.exit(2);
			}
			await fs.writeFile(target, after, "utf8");
			const run = runExampleTest(dir);
			const hit = !run.ok && run.output.includes(m.expect);
			check(
				hit,
				`变异 ${m.id} 必须失败并出现 ${m.expect}`,
				run.ok ? "复用入口仍然通过（样例与判据脱节）" : run.output.trim().slice(0, 800),
			);
		}
	} finally {
		await fs.rm(tmpRoot, { recursive: true, force: true });
	}

	return failed === 0;
}

const isDirectRun = runsAsMain(import.meta.url);
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
