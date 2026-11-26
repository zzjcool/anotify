#!/usr/bin/env node
/* affected.test.mjs — 选择器的正确性自测 + 映射表防腐烂守卫（零依赖，node:test）
 *
 * spec-doc: 本文件是「改动→测试」选择器**自己的**规格自检：下面每一段说明的都是
 * 这个测试文件在验什么（它自己实现它），不是对外部代码的无锚点约束声称。
 *
 * 规则码（`@guard` 锚点指向这里；affected.mjs / affected-map.mjs 的注释用它兜底）：
 *   AFFECTED_SELECTION_LOST —— 已知耦合的改动没选中对应套件
 *   AFFECTED_MAP_INCOMPLETE —— 已注册套件缺映射行 / 缺理由 / 表里有幽灵套件
 *   AFFECTED_MAP_ROT        —— 套件源码访问的页面没登记进映射表 / 页面没人覆盖
 *   AFFECTED_MAP_DEAD_GLOB  —— 表里的 glob 一个被跟踪文件都匹配不到
 *   AFFECTED_GO_CLOSURE     —— Go 反向闭包 / 二进制风险面判错
 *   AFFECTED_ROW_DELETION   —— 删掉表里一行而断言仍然绿（说明断言不依赖表）
 *
 * 它回答的不是「代码有没有 bug」，而是三件会被时间侵蚀的事：
 *
 *   ① **选择正确性**：已知耦合的改动必须选中对应套件；已知无关的改动不许选中。
 *      （每加一条映射就加一条断言，否则映射表会变成"没人验证的注释"。）
 *   ② **表不能悄悄变窄**：删掉表里某一行，对应断言必须红。这条元测试钉住①的有效性
 *      —— 少了它，把断言和表一起删掉也能"绿"。
 *   ③ **表不能腐烂**：
 *      · 每个已注册套件必须有行、有理由（新套件不会静默永不选中）；
 *      · 套件源码（含它 import 的 fixtures）里出现的**已知页面**字面量，必须登记进
 *        该套件的 pages —— 给套件加一次页面访问却忘了登记 ⇒ 红；
 *      · 表里每条 glob 必须匹配至少一个 git 跟踪文件 —— 文件被改名/退役后死 glob ⇒ 红；
 *      · 矩阵类套件的 ALL 声明必须真的覆盖全部页面（PAGES 常量 == web-src/pages）。
 *
 * 跑法：node --test scripts/e2e/affected.test.mjs   （make check-affected-map / make ci）
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync, existsSync, statSync } from "node:fs";
import { execFileSync } from "node:child_process";
import path from "node:path";
import { fileURLToPath } from "node:url";

import {
	FULL_SURFACES,
	NO_E2E_IMPACT,
	SHARED_CHROME_SURFACES,
	SUITE_SURFACES,
} from "./affected-map.mjs";
import {
	ANODE_ONLY_SUITES,
	HTTP_SUITES,
	chromeSuites,
	inRunOrder,
	registryOrder,
	findGo,
	globMatch,
	goPlan,
	pageOf,
	registeredSuites,
	selectE2E,
	sitePages,
	suitesForBinaries,
} from "./affected.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const TABLE = {
	SUITE_SURFACES,
	FULL_SURFACES,
	SHARED_CHROME_SURFACES,
	NO_E2E_IMPACT,
};
const SUITES = registeredSuites(ROOT);
const PAGES = sitePages(ROOT);

/** 只跑 e2e 那半段（不碰 git / go）。 */
const pickE2E = (files, table = TABLE) =>
	selectE2E({ files, suites: SUITES, table, pages: PAGES });

let cachedPkgs = null;
const realPkgs = () => (cachedPkgs ??= goListForTest());

/** 与 CLI 同一条链路：e2e 表选择 + Go 反向闭包给出的二进制风险面。 */
function pick(files, table = TABLE) {
	const e2e = pickE2E(files, table);
	const go = goPlan({ files, pkgs: realPkgs() });
	if (e2e.full) return e2e;
	const byBin = suitesForBinaries(go.binaries, SUITES);
	if (byBin.size === 0) return e2e;
	const selected = new Set(e2e.selected);
	const reasons = { ...e2e.reasons };
	for (const [s, why] of byBin) {
		selected.add(s);
		reasons[s] = [...(reasons[s] || []), ...why];
	}
	return {
		...e2e,
		selected: [...selected].sort(),
		reasons,
		skipped: e2e.skipped.filter((x) => !selected.has(x.suite)),
	};
}

/** 真 import 图（一次性）：测试里只读，不改任何东西。 */
function goListForTest() {
	const out = execFileSync(
		findGo(),
		["list", "-f", "{{.Dir}}|{{.ImportPath}}|{{join .Deps \" \"}}|{{join .TestImports \" \"}}|{{join .XTestImports \" \"}}", "./..."],
		{ cwd: ROOT, encoding: "utf8", maxBuffer: 64 << 20 },
	);
	return out
		.split("\n")
		.filter((l) => l.trim())
		.map((line) => {
			const [dir, importPath, deps = "", t = "", xt = ""] = line.split("|");
			return {
				dir,
				importPath,
				prodDeps: deps.split(" ").filter(Boolean),
				testEdges: [...t.split(" "), ...xt.split(" ")].filter(Boolean),
			};
		});
}

// ---------------------------------------------------------------- ① 已知耦合 / 已知无关
//
// 每一条都是「改这里，不跑那个套件就等于没验」的真实耦合，理由写在 why 里。
const COUPLED = [
	{
		why: "改源列表页：f3 的 source-actions case 与 events_paging 都直接驱动它",
		files: ["web-src/pages/sources.html"],
		must: ["frontend", "events_paging", "ui_shift"],
		mustNot: ["api_contract", "passkeys", "orphans", "live_agent", "cli_auth"],
	},
	{
		why: "改生成物（sitegen 写坏的场景）：同样要选中访问列表页的套件",
		files: ["web/sources.html"],
		must: ["frontend", "events_paging"],
		mustNot: ["ws_protocol", "capacity"],
	},
	{
		why: "改 docs 页：只有真正渲染 docs 的套件才需要重跑",
		files: ["web-src/pages/docs.html"],
		must: ["p2_guards", "frontend"],
		mustNot: ["admin_flow", "auth_flow", "passkeys", "routing"],
	},
	{
		why: "#524 改预案页：该页在 playbook_release 里登记在 paths（不是 pages），页面分支取并集才会命中 —— 否则 release/版本/卸载套件默认漏跑",
		files: ["web-src/pages/playbook.html"],
		must: ["playbook_release", "frontend"],
		mustNot: ["api_contract", "passkeys", "routing", "node_v4", "ws_protocol"],
	},
	{
		why: "改协议包：帧形状变了，节点面套件与契约套件必须重跑",
		files: ["internal/protocol/doc.go"],
		must: ["ws_protocol", "node_v4", "contract", "api_contract"],
		mustNot: [],
	},
	{
		why: "改 anode 的 runner：只有真跑节点的套件会读到它",
		files: ["cmd/anode/internal/runner/runner.go"],
		must: ["live_agent", "node_v4", "deploy"],
		mustNot: ["passkeys", "admin_flow", "auth_flow", "events_paging", "p2_guards"],
	},
	{
		why: "改某一个套件自己的源码：只需要重跑它自己",
		files: ["scripts/e2e/suites/passkeys.mjs"],
		must: ["passkeys"],
		mustNot: ["frontend", "ui_shift", "contract", "node_v4"],
	},
	{
		why: "#616 改 Key 页（新建弹层的重复提交）：keys_create 必须重跑，其它页的套件不必",
		files: ["web-src/pages/keys.html"],
		must: ["keys_create", "recovery_next"],
		mustNot: ["frontend", "api_contract", "routing", "node_v4", "ws_protocol"],
	},
	{
		why: "改共享 harness：所有套件都靠它 startServer/seed，必须全量",
		files: ["scripts/e2e/lib/harness.mjs"],
		must: SUITES,
		mustNot: [],
	},
	{
		// api/** 在 FULL_SURFACES 里：OpenAPI 是「HTTP 形状的正本」，无法局部判定，
		// 所以这里断言的是「全量」而不是某个子集 —— 保守面必须显式写在测试里。
		why: "改 OpenAPI 真源 ⇒ 全量（fail-closed，而不是挑几个套件）",
		files: ["api/resource.yaml"],
		must: SUITES,
		mustNot: [],
		full: true,
	},
];

for (const c of COUPLED) {
	test(`选中：${c.why}`, () => {
		const r = pick(c.files);
		for (const s of c.must) {
			assert.ok(
				r.selected.includes(s),
				`${c.files.join(",")} 应选中 ${s}，实际选中 ${r.selected.join(",")}`,
			);
		}
		for (const s of c.mustNot) {
			assert.ok(
				!r.selected.includes(s),
				`${c.files.join(",")} 不该选中 ${s}（跳过原因应能解释）`,
			);
		}
	});
}

const UNRELATED = [
	{ why: "纯文档改动不跑任何 e2e", files: ["docs/reports/comment-and-doc-ratchet.md", "AGENTS.md"], expect: 0 },
	{ why: "协调者报告目录不是运行输入", files: [".reports/ci-cost-reduction.md"], expect: 0 },
	{ why: "CI 编排改动不改变被断言的产品行为", files: [".github/workflows/ci.yml"], expect: 0 },
];

for (const c of UNRELATED) {
	test(`不选中：${c.why}`, () => {
		const r = pick(c.files);
		assert.equal(r.full, null, `不该触发全量：${r.full}`);
		assert.equal(r.selected.length, c.expect, `实际选中 ${r.selected.join(",")}`);
	});
}

test("fail-closed：映射表没登记的路径 ⇒ 全量（宁可多跑，不许漏跑）", () => {
	const r = pick(["some/unknown/thing.xyz"]);
	assert.ok(r.full, "未知路径必须触发全量");
	assert.equal(r.selected.length, SUITES.length);
	assert.ok(r.unknown.some((u) => u.file === "some/unknown/thing.xyz"));
});

test("fail-closed：没有套件登记的页面名 ⇒ 全量", () => {
	// 造一个真实存在的页面名之外的路径：走 pageOf 但不属于任何套件的页面集合
	const r = pick(["web-src/pages/does-not-exist.html"]);
	assert.ok(r.full, "没有套件覆盖的页面必须触发全量");
});

test("#524：页面走 pages ∪ paths 的并集 —— playbook_release 不登记 pages 也必须命中（证的是并集，不是补数据）", () => {
	const files = ["web-src/pages/playbook.html"];
	const r = pickE2E(files);
	assert.ok(r.selected.includes("playbook_release"), `实际选中 ${r.selected.join(",")}`);
	const noPages = {
		...TABLE,
		SUITE_SURFACES: {
			...SUITE_SURFACES,
			playbook_release: { ...SUITE_SURFACES.playbook_release, pages: undefined },
		},
	};
	const r2 = pickE2E(files, noPages);
	assert.ok(
		r2.selected.includes("playbook_release"),
		"只靠 paths 也必须命中 —— 否则根因（页面分支不看 paths）没修，只是把数据补了一遍",
	);
	assert.match(String(r2.reasons.playbook_release), /页面路径/, "命中的理由必须说明来自 paths 分支");
});

test("#524：同一个页面的源与产物（web-src/pages/x.html / web/x.html）选中同一批套件", () => {
	const src = pickE2E(["web-src/pages/playbook.html"]);
	const gen = pickE2E(["web/playbook.html"]);
	assert.ok(src.selected.includes("playbook_release"));
	assert.deepEqual(gen.selected, src.selected, "两种写法算出两套答案 = 改了产物那一半漏跑");
});

test("#524 反向 1：并集不许把「改任意页面」扩成暴涨或全量（capabilities.html）", () => {
	const r = pickE2E(["web-src/pages/capabilities.html"]);
	assert.equal(r.full, null, `不该触发全量：${r.full}`);
	const allPaged = SUITES.filter((s) => SUITE_SURFACES[s]?.pages === "ALL").length;
	const declared = SUITES.filter(
		(s) => Array.isArray(SUITE_SURFACES[s]?.pages) && SUITE_SURFACES[s].pages.includes("capabilities"),
	).length;
	assert.ok(declared > 0, "前置：capabilities 至少被一个套件的 pages 登记");
	assert.ok(
		r.selected.length <= allPaged + declared,
		`选中 ${r.selected.length} 个，超上限 ${allPaged}（ALL）+ ${declared}（登记）：${r.selected.join(",")}`,
	);
	assert.ok(!r.selected.includes("playbook_release"), "别的页面不许被 playbook 的 paths 登记带走");
});

test("#524 反向 2：无人登记的页面仍然 fail-closed（paths 匹配不等于「有人覆盖」）", () => {
	const orphan = "no-suite-page";
	const r = selectE2E({
		files: [`web-src/pages/${orphan}.html`],
		suites: SUITES,
		table: TABLE,
		pages: [...PAGES, orphan],
	});
	assert.equal(r.unknown.length, 0, "页面在站点清单里，不该记成「不认识的名字」");
	const matrices = SUITES.filter((s) => SUITE_SURFACES[s]?.pages === "ALL");
	assert.ok(matrices.length > 0, "前置：至少一个矩阵类套件");
	assert.deepEqual(
		r.selected,
		[...matrices].sort(),
		`无登记页面只该由矩阵类覆盖，实际 ${r.selected.join(",")}（缺口 = 静默通过，满 = 扩成全量）`,
	);
	assert.ok(!r.selected.includes("playbook_release"));
});

test("跳过清单必须带原因（不是空壳）", () => {
	const r = pick(["web-src/pages/docs.html"]);
	assert.ok(r.skipped.length > 0, "这个改动应该有被跳过的套件");
	for (const s of r.skipped) {
		assert.ok(s.why && s.why.includes("未触及"), `${s.suite} 的跳过原因缺内容：${s.why}`);
	}
});

// ---------------------------------------------------------------- ② 表不能悄悄变窄
test("元测试：删掉表里一条 → 对应断言必须红", () => {
	const files = ["scripts/e2e/suites/passkeys.mjs"];
	const before = pick(files);
	assert.ok(before.selected.includes("passkeys"), "前置：正常情况下应该选中 passkeys");

	const narrowed = {
		...TABLE,
		SUITE_SURFACES: Object.fromEntries(
			Object.entries(SUITE_SURFACES).filter(([k]) => k !== "passkeys"),
		),
	};
	const after = pick(files, narrowed);
	assert.ok(
		!after.selected.includes("passkeys"),
		"删掉 passkeys 行以后仍然选中了它 —— 说明「选中」不来自映射表，断言就是假的",
	);
});

test("元测试：删掉页面行 → 页面改动不再选中它（断言依赖表，而不依赖套件名）", () => {
	const files = ["web-src/pages/docs.html"];
	const before = pick(files);
	assert.ok(before.selected.includes("p2_guards"));

	const narrowed = {
		...TABLE,
		SUITE_SURFACES: {
			...SUITE_SURFACES,
			p2_guards: { ...SUITE_SURFACES.p2_guards, pages: ["events", "login"] },
		},
	};
	const after = pick(files, narrowed);
	assert.ok(!after.selected.includes("p2_guards"), "docs 从 p2_guards 的页面表里删掉后仍被选中");
});

// ---------------------------------------------------------------- ③ 防腐烂
test("每个已注册套件必须有映射行与非空理由", () => {
	for (const s of SUITES) {
		const row = SUITE_SURFACES[s];
		assert.ok(row, `套件 ${s} 没有映射行 ⇒ 它永远不会被 affected 选中`);
		assert.ok(row.why && row.why.length > 8, `套件 ${s} 的 why 太短/为空`);
		assert.ok(
			(row.pages !== undefined) || (row.paths && row.paths.length),
			`套件 ${s} 既没有 pages 也没有 paths`,
		);
	}
	for (const s of Object.keys(SUITE_SURFACES)) {
		assert.ok(SUITES.includes(s), `映射表里的 ${s} 不是已注册套件（漂移）`);
	}
});

/** 套件模块图：从 suites/<name>.mjs 出发，跟着相对 import 走。
 *  刻意**不含** scripts/e2e/lib/ —— 那是共享导航 helper，里面出现 login.html
 *  之类是常量而非"这个套件访问这页"，算进来会把每个页面改动都变成全量。 */
function suiteGraph(entry) {
	const seen = new Set();
	const stack = [entry];
	while (stack.length) {
		const f = stack.pop();
		if (seen.has(f) || !existsSync(f) || !statSync(f).isFile()) continue;
		if (f.includes(`${path.sep}e2e${path.sep}lib${path.sep}`)) continue;
		seen.add(f);
		const src = readFileSync(f, "utf8");
		for (const m of src.matchAll(/(?:from|import\()\s*["']([^"']+)["']/g)) {
			if (!m[1].startsWith(".")) continue;
			stack.push(path.resolve(path.dirname(f), m[1]));
		}
	}
	return seen;
}

function derivedPages(suite) {
	const found = new Set();
	for (const f of suiteGraph(path.join(ROOT, "scripts/e2e/suites", `${suite}.mjs`))) {
		const src = readFileSync(f, "utf8");
		for (const m of src.matchAll(/([a-z0-9-]+)\.html/g)) {
			if (PAGES.includes(m[1])) found.add(m[1]);
		}
	}
	return found;
}

test("套件源码里出现的页面必须登记进映射表（漏登记 = 漏跑）", () => {
	const problems = [];
	for (const s of SUITES) {
		const row = SUITE_SURFACES[s];
		if (!row || row.pages === "ALL") continue;
		const declared = new Set(row.pages || []);
		for (const p of derivedPages(s)) {
			if (!declared.has(p)) {
				problems.push(`${s}: 源码访问 ${p}.html 但 pages 里没登记`);
			}
		}
	}
	assert.deepEqual(problems, [], problems.join("\n"));
});

test("页面全覆盖：web-src/pages 下每个页面至少被一个套件盯着", () => {
	const covered = new Set();
	for (const row of Object.values(SUITE_SURFACES)) {
		if (row.pages === "ALL") for (const p of PAGES) covered.add(p);
		else for (const p of row.pages || []) covered.add(p);
	}
	const missing = PAGES.filter((p) => !covered.has(p));
	assert.deepEqual(missing, [], `没人跑这些页面：${missing.join(",")}`);
});

test("矩阵套件声明的 ALL 必须真的覆盖全部页面（PAGES 常量 == web-src/pages）", () => {
	// 「pages: ALL」是保守声明的基石：它说「这一页改了，这个套件也会验到」。
	// 而 i18n / ui_shift 的页面集是**硬编码数组**（f3-driver.PAGES / f3-ui-helper.PAGES），
	// 不是 readdir —— 新增一页却忘了加进数组，ALL 就变成假话（新页面没人验）。
	const files = ["scripts/e2e/fixtures/f3/f3-driver.mjs", "scripts/e2e/fixtures/f3/f3-ui-helper.mjs"];
	for (const rel of files) {
		const src = readFileSync(path.join(ROOT, rel), "utf8");
		const m = src.match(/export const PAGES = (?:Object\.freeze\()?\[([\s\S]*?)\]/);
		assert.ok(m, `${rel} 里找不到 PAGES 数组`);
		const declared = [...m[1].matchAll(/["']([a-z0-9-]+)["']/g)].map((x) => x[1]).sort();
		assert.deepEqual(declared, [...PAGES].sort(), `${rel} 的 PAGES 与 web-src/pages 不一致`);
	}
});

test("页面名必须真实存在（pages 里写的页面不能是幻觉）", () => {
	const bogus = [];
	for (const [s, row] of Object.entries(SUITE_SURFACES)) {
		if (row.pages === "ALL") continue;
		for (const p of row.pages || []) {
			if (!PAGES.includes(p)) bogus.push(`${s}: ${p}`);
		}
	}
	assert.deepEqual(bogus, [], bogus.join("\n"));
});

test("表里每条 glob 都必须匹配至少一个被跟踪文件（死 glob = 表腐烂）", () => {
	const tracked = execFileSync("git", ["ls-files", "-z"], { cwd: ROOT, encoding: "utf8" })
		.split("\0")
		.filter(Boolean);
	const all = [
		...FULL_SURFACES.map((r) => ({ ...r, where: "FULL_SURFACES" })),
		...SHARED_CHROME_SURFACES.map((r) => ({ ...r, where: "SHARED_CHROME_SURFACES" })),
		...NO_E2E_IMPACT.map((r) => ({ ...r, where: "NO_E2E_IMPACT" })),
		...Object.entries(SUITE_SURFACES).flatMap(([s, row]) =>
			(row.paths || []).map((g) => ({ glob: g, why: row.why, where: `SUITE_SURFACES.${s}` })),
		),
	];
	// generated/unversioned 的行不参与「有没有被跟踪文件」判据（理由写在行上）：
	//   generated = sitegen 产物（web/*.html、web/i18n.*.js）不入库；
	//   unversioned = 从不入库的工作目录（.reports / .pi*）。
	const checkable = all.filter((r) => !r.generated && !r.unversioned);
	const exempt = all.length - checkable.length;
	assert.ok(exempt >= 0);
	const dead = checkable.filter((r) => !tracked.some((f) => globMatch(r.glob, f)));
	assert.deepEqual(
		dead.map((r) => `${r.where}: ${r.glob}`),
		[],
		"这些 glob 一个文件都匹配不到（文件被改名/删除了？）",
	);
});

test("优先级：白名单与共享面重叠时按**更保守**的一头算（白名单不许吃掉共享面）", () => {
	// 分类是短路的，所以两条规则重叠时的先后顺序就是判据的一部分。
	// 实例：scripts/check-tmp-leaks.mjs 既像"守卫脚本"（白名单）又是每个 e2e 的
	// 外层包装（共享面）—— 判成"无影响"就漏掉了一次真实的运行期变化。
	// 这里同时钉两件事：① 重叠的文件必须判「全量」；② 白名单不许遮蔽某个套件的耦合面。
	const tracked = execFileSync("git", ["ls-files", "-z"], { cwd: ROOT, encoding: "utf8" })
		.split("\0")
		.filter(Boolean);
	const suiteGlobs = Object.entries(SUITE_SURFACES).flatMap(([s, row]) =>
		(row.paths || []).map((g) => ({ glob: g, where: s })),
	);
	const shadowed = [];
	const fullWins = [];
	for (const f of tracked) {
		// markdown 例外：`**/*.md` 是整类文档，与"包目录里的 AGENTS.md"重叠是**设计**
		// （改 AGENTS.md 不改变运行期行为），不算遮蔽。
		if (f.endsWith(".md")) continue;
		const ni = NO_E2E_IMPACT.find((r) => globMatch(r.glob, f));
		if (!ni) continue;
		if (FULL_SURFACES.some((r) => globMatch(r.glob, f))) fullWins.push(f);
		for (const sg of suiteGlobs) {
			if (globMatch(sg.glob, f)) shadowed.push(`${f}: 白名单 ${ni.glob} vs 套件 ${sg.where} (${sg.glob})`);
		}
	}
	assert.deepEqual(shadowed, [], shadowed.join("\n"));

	// ① 重叠即全量：用真实分类器验证一遍（含 docs 以外的真实文件）
	for (const f of fullWins) {
		const r = pickE2E([f]);
		assert.ok(r.full, `${f} 同时命中白名单与共享面，必须判全量（实际判了"无影响"）`);
	}
	if (fullWins.length === 0) {
		// 没有重叠样本时也要证明规则本身是活的：造一个必然重叠的输入。
		const synthetic = {
			...TABLE,
			NO_E2E_IMPACT: [...NO_E2E_IMPACT, { glob: "scripts/e2e/lib/**", why: "合成的冲突行" }],
		};
		const r = pickE2E(["scripts/e2e/lib/harness.mjs"], synthetic);
		assert.ok(r.full, "白名单遮蔽了共享 harness，仍然判了「无影响」");
	}
});

test("SHARED_CHROME_SURFACES 不能抢走页面级精确匹配（页面先被 pageOf 处理）", () => {
	// 页面文件的匹配必须走 pageOf 分支：否则 web/**/*.html 会把所有 Chrome 套件都拉进来
	for (const p of PAGES) {
		assert.equal(pageOf(`web-src/pages/${p}.html`), p);
		assert.equal(pageOf(`web/${p}.html`), p);
	}
	assert.equal(pageOf("web/ui.css"), null);
});

test("HTTP/Chrome 分区与 parallel_runner.mjs 的注册表一致（否则风险面会算错类）", () => {
	const src = readFileSync(path.join(ROOT, "scripts/e2e/parallel_runner.mjs"), "utf8");
	const block = src.match(/const HTTP_SUITES = \[([\s\S]*?)\];/);
	assert.ok(block, "parallel_runner.mjs 里找不到 HTTP_SUITES");
	const declared = [...block[1].matchAll(/"([^"]+)"/g)].map((m) => m[1]).sort();
	assert.deepEqual(declared, [...HTTP_SUITES].sort());
	assert.deepEqual(
		chromeSuites(SUITES).length + HTTP_SUITES.length,
		SUITES.length,
		"HTTP + Chrome 必须正好等于全部注册套件",
	);
	for (const s of ANODE_ONLY_SUITES) assert.ok(SUITES.includes(s), `ANODE_ONLY_SUITES 里的 ${s} 未注册`);
});

// ---------------------------------------------------------------- ③b 执行顺序
test("执行顺序按注册表（信号量按提交顺序发，字母序会让四个矩阵套件同时开跑）", () => {
	const order = registryOrder(ROOT);
	assert.equal(order.length, SUITES.length, "注册表顺序必须覆盖全部套件");
	assert.deepEqual([...order].sort(), [...SUITES].sort(), "只是顺序不同，集合必须相同");

	// 反例回归：ui_shift 与其它三个矩阵套件同时开跑时，实测会撞到自己的 300s 预算
	// （全量跑里从没超过 256s）。按注册表顺序时 ui_shift 排在它们后面。
	const subset = ["events_paging", "frontend", "i18n", "i18n_coverage", "ui_shift"];
	const ordered = inRunOrder(subset, order);
	assert.equal(ordered[ordered.length - 1], "ui_shift", "ui_shift 必须最后才拿到信号量");
	assert.deepEqual([...ordered].sort(), [...subset].sort(), "排序不许增删套件");
});

test("执行顺序：注册表解析失败时退回字母序，而不是抛错或丢套件", () => {
	const order = registryOrder("/nonexistent-root-for-test");
	assert.ok(Array.isArray(order) && order.length > 0, "必须回退到 registeredSuites，而不是空/抛错");
	assert.deepEqual(inRunOrder(["b", "a"], []), ["a", "b"]);
});

// ---------------------------------------------------------------- ④ Go 包反向闭包
function fakePkgs() {
	const M = "github.com/zzjcool/anotify";
	return [
		{ dir: "/r/internal/protocol", importPath: `${M}/internal/protocol`, prodDeps: [], testEdges: [] },
		{ dir: "/r/internal/store", importPath: `${M}/internal/store`, prodDeps: [`${M}/internal/protocol`], testEdges: [] },
		{ dir: "/r/internal/store/v4test", importPath: `${M}/internal/store/v4test`, prodDeps: [`${M}/internal/store`], testEdges: [] },
		{ dir: "/r/internal/server", importPath: `${M}/internal/server`, prodDeps: [`${M}/internal/store`], testEdges: [`${M}/internal/store/v4test`] },
		{ dir: "/r/cmd/server", importPath: `${M}/cmd/server`, prodDeps: [`${M}/internal/server`], testEdges: [] },
		{ dir: "/r/cmd/anode/internal/runner", importPath: `${M}/cmd/anode/internal/runner`, prodDeps: [], testEdges: [] },
		{ dir: "/r/cmd/anode", importPath: `${M}/cmd/anode`, prodDeps: [`${M}/cmd/anode/internal/runner`], testEdges: [] },
	].map((p) => ({ ...p }));
}

test("Go 反向闭包：改叶子包 → 下游测试包全部入选", () => {
	const r = goPlan({ files: ["internal/protocol/doc.go"], pkgs: fakePkgs(), root: "/r" });
	const short = r.packages.map((p) => p.replace("github.com/zzjcool/anotify/", ""));
	assert.deepEqual(short, ["cmd/server", "internal/protocol", "internal/server", "internal/store", "internal/store/v4test"]);
	assert.ok(r.binaries.has("server"), "cmd/server 在闭包里 ⇒ 中枢二进制变了");
});

test("Go 反向闭包：测试专用 helper 不进二进制（不许因此拉全量 e2e）", () => {
	// v4test 只被 internal/server 的**测试**引用：跑它的测试，但不该判「二进制变了」
	const r = goPlan({ files: ["internal/store/v4test/helper.go"], pkgs: fakePkgs(), root: "/r" });
	assert.ok(r.packages.some((p) => p.endsWith("/internal/server")), "用它的测试要重跑");
	assert.ok(!r.binaries.has("server"), "测试专用依赖不能让中枢被判成变了");
});

test("Go 二进制 → e2e 风险面：anode-only 改动不跑非节点套件", () => {
	const r = goPlan({ files: ["cmd/anode/internal/runner/runner.go"], pkgs: fakePkgs(), root: "/r" });
	assert.deepEqual([...r.binaries.keys()], ["anode"]);
	const picks = suitesForBinaries(r.binaries, SUITES);
	assert.deepEqual([...picks.keys()].sort(), [...ANODE_ONLY_SUITES].sort());
});

test("Go 二进制 → e2e 风险面：中枢变了 ⇒ 全量", () => {
	const r = goPlan({ files: ["internal/store/store.go"], pkgs: fakePkgs(), root: "/r" });
	const picks = suitesForBinaries(r.binaries, SUITES);
	assert.equal(picks.size, SUITES.length);
});

test("go.mod 变了 ⇒ Go 全量（依赖图整体变化）", () => {
	const r = goPlan({ files: ["go.mod"], pkgs: fakePkgs(), root: "/r" });
	assert.ok(r.allPackages);
	assert.equal(r.packages.length, fakePkgs().length);
});
