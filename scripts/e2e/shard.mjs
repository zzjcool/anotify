#!/usr/bin/env node
/* shard.mjs — 把本次选中的 e2e 套件切成 N 片（CI 一个 job 跑一片）
 * 只改「谁在哪个 runner 上跑」：并集必须等于计划里的 selected（--merge 复核 + --selected 硬校验）。
 */
import { copyFileSync, cpSync, existsSync, mkdirSync, readFileSync, readdirSync, statSync, writeFileSync } from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";


export const DEFAULT_SHARDS = 3;
/** 有效并行度：实测 29 套串行 780s（= 750s 成本）、同一批并行 420s（run 44838842345）⇒ 1.86。 */
export const EFFECTIVE_SLOTS = 1.9;

/** 每套件实测墙钟（秒），来源 run 44865127142（4 vCPU、29 套串行）。过期只让分片不均衡，不漏套件。 */
export const COST_SECONDS = {
	admin_flow: 10.0,
	api_contract: 3.7,
	auth_flow: 2.5,
	capacity: 147.7,
	cli_auth: 24.0,
	contract: 6.9,
	deeplink: 2.5,
	deploy: 3.7,
	edge_cases: 4.2,
	events_paging: 9.1,
	frontend: 158.2,
	i18n: 47.6,
	i18n_coverage: 63.0,
	lang_hint: 15.8,
	live_agent: 3.9,
	node_v4: 4.5,
	orphans: 3.6,
	p2_guards: 5.3,
	passkey_enroll: 13.2,
	passkeys: 3.2,
	"password_login": 5.0,
	persistence: 7.0,
	recovery_next: 4.6,
	routing: 4.6,
	security: 15.1,
	ui_shift: 150.3,
	ux_regression_19_23: 9.8,
	vars_ui: 2.6,
	ws_protocol: 18.4,
};
const DEFAULT_COST = 20;
/** 每套件固定启动成本（spawn node + 起 server + seed，实测 CI 约 4s）：让「把一堆廉价套件堆进同一片」在模型里也变贵。 */
export const SUITE_START_SECONDS = 4;

export function costOf(suite, cost = COST_SECONDS) {
	return (cost[suite] ?? DEFAULT_COST) + SUITE_START_SECONDS;
}

export function predictedWallSeconds(suites, { cost = COST_SECONDS, slots = EFFECTIVE_SLOTS } = {}) {
	let maxOf = 0;
	let sumOf = 0;
	for (const s of suites) {
		const c = costOf(s, cost);
		maxOf = Math.max(maxOf, c);
		sumOf += c;
	}
	return Math.round(Math.max(maxOf, sumOf / slots));
}

/** 纯函数：selected → 分片（最长优先落桶 + 以「最慢一片」为目标的迁移/交换改进，无随机）。 */
export function planShards({ selected, shards = DEFAULT_SHARDS, cost = COST_SECONDS }) {
	const suites = [...new Set(selected)].sort();
	if (suites.length === 0) return { selected: [], shards: [], totalCostSeconds: 0 };
	const k = Math.max(1, Math.min(shards, suites.length));
	const walls = (bks) => bks.map((b) => predictedWallSeconds(b, { cost }));
	// 比「最慢的一片」，并列再比第二慢…… 不用总和：否则把套件挤进更少的片会显得更优。
	const score = (bks) => [...walls(bks)].sort((a, b) => b - a);
	const better = (a, b) => {
		for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return a[i] < b[i];
		return false;
	};

	const buckets = Array.from({ length: k }, () => []);
	const weight = (s) => costOf(s, cost);
	for (const s of [...suites].sort((a, b) => weight(b) - weight(a) || a.localeCompare(b))) {
		let best = 0;
		for (let i = 1; i < k; i++) {
			if (predictedWallSeconds(buckets[i], { cost }) < predictedWallSeconds(buckets[best], { cost }))
				best = i;
		}
		buckets[best].push(s);
	}

	for (let round = 0; round < 200; round++) {
		const cur = score(buckets);
		const w = walls(buckets);
		const worst = w.indexOf(Math.max(...w));
		let applied = false;
		for (let j = 0; j < k && !applied; j++) {
			if (j === worst) continue;
			// 不许把一片搬空：k 个 runner 就是要用上 k 个（片数由 --shards 决定）。
			if (buckets[worst].length <= 1) break;
			for (const s of buckets[worst]) {
				const trial = buckets.map((b) => [...b]);
				trial[worst] = trial[worst].filter((x) => x !== s);
				trial[j] = [...trial[j], s];
				if (better(score(trial), cur)) {
					buckets.splice(0, k, ...trial);
					applied = true;
					break;
				}
			}
			if (applied) break;
			for (const a of buckets[worst]) {
				for (const b of buckets[j]) {
					const trial = buckets.map((x) => [...x]);
					trial[worst] = trial[worst].filter((x) => x !== a).concat(b);
					trial[j] = trial[j].filter((x) => x !== b).concat(a);
					if (better(score(trial), cur)) {
						buckets.splice(0, k, ...trial);
						applied = true;
						break;
					}
				}
				if (applied) break;
			}
		}
		if (!applied) break;
	}

	const out = buckets
		.map((b) => [...b].sort())
		.filter((b) => b.length > 0)
		.map((b, i) => ({
			id: i + 1,
			suites: b,
			costSeconds: Math.round(b.reduce((a, s) => a + costOf(s, cost), 0)),
			predictedWallSeconds: predictedWallSeconds(b, { cost }),
		}));
	return { selected: suites, shards: out, totalCostSeconds: Math.round(suites.reduce((a, s) => a + costOf(s, cost), 0)) };
}

/** 汇总：把各分片 artifact 里的 results 拼到一个目录（裁决交给判定器，缺套件它来判红）。 */
export function mergeShards({ from, out, expect }) {
	const expected = [...new Set(expect)].sort();
	const dirs = existsSync(from)
		? readdirSync(from)
				.map((n) => path.join(from, n))
				.filter((p) => statSync(p).isDirectory())
				.sort()
		: [];
	mkdirSync(out, { recursive: true });
	const shards = [];
	const copied = new Set();
	for (const dir of dirs) {
		let info = null;
		try {
			info = JSON.parse(readFileSync(path.join(dir, "_shard.json"), "utf8"));
		} catch {
			info = null;
		}
		const got = [];
		for (const f of readdirSync(dir)) {
			if (!f.endsWith(".json") || f === "_shard.json") continue;
			const base = f.slice(0, -5);
			if (base === "_authority-manifest") {
				copyFileSync(path.join(dir, f), path.join(out, `_authority-manifest-shard-${info?.shard ?? "?"}.json`));
				continue;
			}
			copyFileSync(path.join(dir, f), path.join(out, f));
			copied.add(base);
			got.push(base);
		}
		const retries = path.join(dir, "_suite-retries");
		if (existsSync(retries)) cpSync(retries, path.join(out, "_suite-retries"), { recursive: true, force: true });
		shards.push({
			artifact: path.basename(dir),
			shard: info?.shard ?? null,
			planned: Array.isArray(info?.suites) ? [...info.suites].sort() : [],
			got: got.sort(),
		});
	}
	const plannedUnion = [...new Set(shards.flatMap((s) => s.planned))].sort();
	return {
		expected,
		shards,
		union: [...copied].sort(),
		missing: expected.filter((s) => !copied.has(s)),
		extra: [...copied].filter((s) => !expected.includes(s)).sort(),
		plannedUnion,
		planMismatch: plannedUnion.join(" ") !== expected.join(" "),
	};
}

export function writeShardInfo({ out, id, suites }) {
	mkdirSync(out, { recursive: true });
	const file = path.join(out, "_shard.json");
	writeFileSync(file, JSON.stringify({ shard: Number(id), suites: [...suites].sort() }, null, 2) + "\n");
	return file;
}

function printPlan(plan) {
	console.log(`分片：${plan.shards.length} 片 / 共 ${plan.selected.length} 套（实测成本合计 ${plan.totalCostSeconds}s）`);
	for (const s of plan.shards) {
		console.log(
			`  片 ${s.id}: ${s.suites.length} 套 · 成本 ${s.costSeconds}s · 预测墙钟 ${s.predictedWallSeconds}s`,
		);
		console.log(`    ${s.suites.join(" ")}`);
	}
}

function printMerge(r) {
	console.log(`分片汇总：期望 ${r.expected.length} 套 / 拿到 ${r.union.length} 套`);
	for (const s of r.shards) {
		console.log(
			`  ${s.artifact}: shard=${s.shard ?? "?"} 计划 ${s.planned.length} 套 / 结果 ${s.got.length} 个` +
				(s.planned.length ? ` · ${s.planned.join(" ")}` : "（没有 _shard.json）"),
		);
	}
	if (r.planMismatch) console.log(`::error title=分片计划与总清单不一致::计划并集 ${r.plannedUnion.join(" ")}`);
	if (r.missing.length) console.log(`::error title=有套件没有结果::${r.missing.join(" ")}`);
	if (r.extra.length) console.log(`::warning title=多出未请求的结果::${r.extra.join(" ")}`);
	return r;
}

function parseInt0(v, dflt) {
	const n = Number(v);
	return Number.isFinite(n) && n > 0 ? Math.floor(n) : dflt;
}

function parseArgs(argv) {
	const o = { fromPlan: null, shards: null, selected: null, githubOutput: false, merge: false, from: null, out: null, expect: null, writeShardInfo: false, id: null, suites: null };
	for (let i = 0; i < argv.length; i++) {
		const a = argv[i];
		const next = () => argv[++i];
		if (a === "--from-plan") o.fromPlan = next();
		else if (a === "--shards") o.shards = parseInt0(next(), DEFAULT_SHARDS);
		else if (a === "--selected") o.selected = next();
		else if (a === "--github-output") o.githubOutput = true;
		else if (a === "--merge") o.merge = true;
		else if (a === "--from") o.from = next();
		else if (a === "--out") o.out = next();
		else if (a === "--expect") o.expect = next();
		else if (a === "--write-shard-info") o.writeShardInfo = true;
		else if (a === "--id") o.id = next();
		else if (a === "--suites") o.suites = next();
		else if (a === "-h" || a === "--help") o.help = true;
		else throw new Error(`未知参数 ${a}`);
	}
	return o;
}

function main() {
	const a = parseArgs(process.argv.slice(2));
	if (a.help) {
		console.log("用法: shard.mjs --from-plan plan.json [--shards 3] [--github-output]");
		console.log("      shard.mjs --merge --from <下载目录> --out <results> --expect \"a b c\"");
		console.log("      shard.mjs --write-shard-info --id 2 --suites \"a b\" --out <results>");
		return 0;
	}
	if (a.writeShardInfo) {
		if (a.id === null || !a.out) throw new Error("--write-shard-info 需要 --id 与 --out");
		const suites = (a.suites || "").split(/[\s,]+/).filter(Boolean);
		console.log(`片 ${a.id}: ${writeShardInfo({ out: a.out, id: a.id, suites })}（${suites.length} 套）`);
		return 0;
	}
	if (a.merge) {
		if (!a.from || !a.out) throw new Error("--merge 需要 --from 与 --out");
		const expect = (a.expect || "").split(/[\s,]+/).filter(Boolean);
		if (expect.length === 0) throw new Error("--merge 需要 --expect（否则「少了整片」会被读成「本来就没什么」）");
		const r = printMerge(mergeShards({ from: a.from, out: a.out, expect }));
		writeFileSync(path.join(a.out, "_shard-inventory.json"), JSON.stringify(r, null, 2) + "\n");
		return 0;
	}
	const selected = a.selected
		? a.selected.split(/[\s,]+/).filter(Boolean)
		: a.fromPlan
			? (JSON.parse(readFileSync(a.fromPlan, "utf8")).e2e?.selected ?? null)
			: null;
	if (!Array.isArray(selected)) throw new Error("需要 --selected 或 --from-plan（且计划里有 e2e.selected）");
	const plan = planShards({ selected, shards: a.shards ?? DEFAULT_SHARDS });
	printPlan(plan);
	if (a.githubOutput) {
		const file = process.env.GITHUB_OUTPUT;
		if (!file) throw new Error("--github-output 需要 $GITHUB_OUTPUT");
		// 0 套时给一个合法的占位矩阵：空 matrix 会让整条流水线报「矩阵没有值」，那是假红。
		const shards = plan.shards.length
			? plan.shards
			: [{ id: 0, suites: [], costSeconds: 0, predictedWallSeconds: 0 }];
		const lines = [
			`count=${plan.selected.length}`,
			`selected=${plan.selected.join(" ")}`,
			`shards=${JSON.stringify(shards)}`,
		];
		writeFileSync(file, lines.join("\n") + "\n", { flag: "a" });
	}
	return 0;
}

const invokedDirectly = process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href;
if (invokedDirectly) {
	try {
		process.exit(main());
	} catch (err) {
		console.error("shard.mjs 失败:", err && err.message ? err.message : err);
		process.exit(2);
	}
}
