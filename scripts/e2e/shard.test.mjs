#!/usr/bin/env node
/* shard.test.mjs — 分片自测：分片错了的代价是漏跑，所以钉死并集/互斥、纯函数、缺片可见。 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import path from "node:path";

import { DEFAULT_SHARDS, planShards, predictedWallSeconds, mergeShards, writeShardInfo } from "./shard.mjs";
import { registeredSuites } from "./affected.mjs";

const ROOT = path.resolve(path.dirname(new URL(import.meta.url).pathname), "../..");
const SELFTEST_DIR = path.join(ROOT, ".e2e-bin", "shard-selftest"); // e2e 里禁止裸临时目录调用
rmSync(SELFTEST_DIR, { recursive: true, force: true });
let seq = 0;
const tmp = () => {
	const dir = path.join(SELFTEST_DIR, String(++seq));
	mkdirSync(dir, { recursive: true });
	return dir;
};

test("全量 29 套：并集等于注册表、片间互斥、每片非空", () => {
	const suites = registeredSuites();
	assert.equal(suites.length, 29);
	const plan = planShards({ selected: suites, shards: 3 });
	assert.equal(plan.shards.length, 3);
	const all = plan.shards.flatMap((s) => s.suites);
	assert.deepEqual([...all].sort(), [...suites].sort());
	assert.equal(new Set(all).size, all.length, "有套件被切进两片");
	for (const s of plan.shards) {
		assert.ok(s.suites.length > 0, `片 ${s.id} 是空的：分片刻意不允许空片（空 matrix 会假红）`);
		assert.deepEqual(s.suites, [...s.suites].sort(), "片内顺序必须稳定（便于比对日志）");
	}
	assert.deepEqual(plan.shards.map((s) => s.id), [1, 2, 3]);
});

test("分片是纯函数：重跑同结果，输入顺序不影响", () => {
	const suites = registeredSuites();
	const a = planShards({ selected: suites, shards: 3 });
	const b = planShards({ selected: [...suites].reverse(), shards: 3 });
	const c = planShards({ selected: [...suites].sort(() => 0.5 - Math.random()), shards: 3 });
	assert.deepEqual(a.shards, b.shards);
	assert.deepEqual(a.shards, c.shards);
});

test("分片均衡：没有哪一片吃掉超过平均成本 1.5 倍", () => {
	const plan = planShards({ selected: registeredSuites(), shards: DEFAULT_SHARDS });
	const avg = plan.totalCostSeconds / plan.shards.length;
	for (const s of plan.shards) {
		assert.ok(s.costSeconds <= avg * 1.5, `片 ${s.id} 成本 ${s.costSeconds}s，平均 ${Math.round(avg)}s`);
	}
});

test("片数不超过套件数：3 套 / 5 片 ⇒ 3 片", () => {
	const plan = planShards({ selected: ["frontend", "i18n", "passkeys"], shards: 5 });
	assert.equal(plan.shards.length, 3);
	assert.deepEqual(plan.shards.flatMap((s) => s.suites).sort(), ["frontend", "i18n", "passkeys"]);
	assert.equal(planShards({ selected: [], shards: 3 }).shards.length, 0);
});

test("预测墙钟：最慢的一套是下界，成本越大越高", () => {
	assert.equal(predictedWallSeconds(["frontend"]), 158);
	assert.equal(predictedWallSeconds([]), 0);
	assert.ok(predictedWallSeconds(["frontend", "i18n"]) >= 158);
	assert.ok(predictedWallSeconds(["passkeys", "i18n"]) < predictedWallSeconds(["frontend", "i18n"]));
});

test("汇总：缺一片能被看见（missing = 那一片的套件）", () => {
	const dir = tmp();
	const from = path.join(dir, "artifacts");
	const out = path.join(dir, "results");
	mkdirSync(path.join(from, "e2e-results-shard-1"), { recursive: true });
	writeShardInfo({ out: path.join(from, "e2e-results-shard-1"), id: 1, suites: ["frontend"] });
	writeFileSync(path.join(from, "e2e-results-shard-1", "frontend.json"), JSON.stringify({ passed: 1, failed: 0 }));
	writeFileSync(path.join(from, "e2e-results-shard-1", "_authority-manifest.json"), JSON.stringify({ suites: [] }));
	// 片 2 的 artifact 完全没上传（job 崩溃）
	const r = mergeShards({ from, out, expect: ["frontend", "i18n"] });
	assert.deepEqual(r.union, ["frontend"]);
	assert.deepEqual(r.missing, ["i18n"]);
	assert.deepEqual(r.extra, []);
	assert.equal(r.planMismatch, true, "计划并集只有 frontend，跟 expect 不一致必须报出来");
	assert.ok(readFileSync(path.join(out, "frontend.json"), "utf8").includes("passed"));
	assert.ok(existsSync(path.join(out, "_authority-manifest-shard-1.json")), "authority 清单要按片改名保留证据");
	assert.ok(!existsSync(path.join(out, "_shard.json")), "_shard.json 是分片凭据，不进判定目录");
});

test("汇总：结果目录不存在时不炸、所有期望套件都进 missing", () => {
	const dir = tmp();
	const r = mergeShards({ from: path.join(dir, "nope"), out: path.join(dir, "results"), expect: ["a", "b"] });
	assert.deepEqual(r.shards, []);
	assert.deepEqual(r.missing, ["a", "b"]);
	assert.deepEqual(r.union, []);
});

test("汇总：多出未请求的结果要报出来（extra）", () => {
	const dir = tmp();
	const from = path.join(dir, "artifacts");
	mkdirSync(path.join(from, "x"), { recursive: true });
	writeFileSync(path.join(from, "x", "ghost.json"), JSON.stringify({ passed: 1 }));
	const r = mergeShards({ from, out: path.join(dir, "results"), expect: ["frontend"] });
	assert.deepEqual(r.extra, ["ghost"]);
	assert.deepEqual(r.missing, ["frontend"]);
});

test("CLI：--from-plan --github-output 写出 count/selected/shards 三行", () => {
	const dir = tmp();
	const planFile = path.join(dir, "plan.json");
	writeFileSync(
		planFile,
		JSON.stringify({ e2e: { selected: ["frontend", "i18n", "passkeys", "contract"], full: "测试" } }),
	);
	const outFile = path.join(dir, "gha.out");
	writeFileSync(outFile, "");
	const r = spawnSync(process.execPath, [path.join(ROOT, "scripts/e2e/shard.mjs"), "--from-plan", planFile, "--shards", "2", "--github-output"], {
		env: { ...process.env, GITHUB_OUTPUT: outFile },
		encoding: "utf8",
	});
	assert.equal(r.status, 0, r.stderr);
	const lines = Object.fromEntries(
		readFileSync(outFile, "utf8")
			.trim()
			.split("\n")
			.map((l) => [l.slice(0, l.indexOf("=")), l.slice(l.indexOf("=") + 1)]),
	);
	assert.equal(lines.count, "4");
	assert.equal(lines.selected, "contract frontend i18n passkeys");
	const shards = JSON.parse(lines.shards);
	assert.equal(shards.length, 2);
	assert.deepEqual(shards.flatMap((s) => s.suites).sort(), ["contract", "frontend", "i18n", "passkeys"]);
});

test("CLI：0 套时给一个合法占位矩阵（空 matrix 会让流水线假红）", () => {
	const dir = tmp();
	const planFile = path.join(dir, "plan.json");
	writeFileSync(planFile, JSON.stringify({ e2e: { selected: [] } }));
	const outFile = path.join(dir, "gha.out");
	writeFileSync(outFile, "");
	const r = spawnSync(process.execPath, [path.join(ROOT, "scripts/e2e/shard.mjs"), "--from-plan", planFile, "--github-output"], {
		env: { ...process.env, GITHUB_OUTPUT: outFile },
		encoding: "utf8",
	});
	assert.equal(r.status, 0, r.stderr);
	const lines = Object.fromEntries(
		readFileSync(outFile, "utf8")
			.trim()
			.split("\n")
			.map((l) => [l.slice(0, l.indexOf("=")), l.slice(l.indexOf("=") + 1)]),
	);
	assert.equal(lines.count, "0");
	const shards = JSON.parse(lines.shards);
	assert.equal(shards.length, 1);
	assert.deepEqual(shards[0].suites, []);
});

test("CLI：--merge 少了 --expect 直接报用法错误（不许把「整片没跑」读成「本来就没事」）", () => {
	const dir = tmp();
	const r = spawnSync(process.execPath, [path.join(ROOT, "scripts/e2e/shard.mjs"), "--merge", "--from", dir, "--out", path.join(dir, "o")], {
		encoding: "utf8",
	});
	assert.equal(r.status, 2);
	assert.match(r.stderr, /--expect/);
});
