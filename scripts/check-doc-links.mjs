#!/usr/bin/env node
/* Anotify · 协调者工作区（`.pi-orchestrator/`）引用检查
 *
 * 作用：`.pi-orchestrator/` 是协调者的**本地**工作区（2026-11-19 起不入库，见 .gitignore）。
 * 仓库里的文档/注释仍然可以提到它，但**不能把里面的文件当作仓库产物来引用** ——
 * fresh clone 里那 300 多个文件根本不存在，读者顺着链接找过去只会扑空。
 * 这就是 docs/ 把「唯一事实源」指向地狱的那条路，本脚本把它变成可执行断言。
 *
 * 判据（只认「引用一个具体文件」这一件事）：
 *   · `REF_OK`    引用的路径在 `git ls-files` 里（迁移后的 docs/ 路径）→ 通过
 *   · `REF_HIST`  `<rev>:<path>` 形态（`git show 9022863:...`）→ 只要求该 rev 下真的有这个 blob
 *                 （历史对象不可变，与本工作区是否入库无关）
 *   · `REF_LOCAL` 目录/通配形态（`.pi-orchestrator/**`、`out/`）：不是产物链接，是「本地目录约定」
 *                 （排除清单、截图落盘目录、gitignore 规则）。逐条打印，不静默放过
 *   · `REF_MIGRATED` 文件头那行 `> 迁移自 .pi-orchestrator/<原路径>（…）`：记的是来历，不是链接；
 *                但它也得能被核验 —— 同名的文件必须真的落在 docs/contracts/ 或 docs/history/ 下
 *   - `REF_DEAD`  其余一律判死链 → 非零退出
 *
 * 扫描面是 **`git ls-files`**，不是工作区：本地 `.pi-orchestrator/` 里的文件一个不少，
 * 拿文件系统做判据会得到「全是绿的」这种假绿 —— 假绿的检查等于没有检查。
 *
 * @guard scripts/check-doc-links.mjs#REF_DEAD
 *
 * 用法：
 *   node scripts/check-doc-links.mjs              # 检查（CI 用）
 *   node scripts/check-doc-links.mjs --list       # 连 REF_LOCAL 明细一起打印
 *   node scripts/check-doc-links.mjs --self-test  # 判据自测（红/绿双侧）
 */
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";

const PREFIX = ".pi-orchestrator";
/** 扩展名 = 「这是一个文件」，不是目录约定。 */
const FILE_EXT_RE = /\.[A-Za-z0-9]{1,6}$/;
/** `<rev>:<path>` 形态里的 rev（7~40 位十六进制，或 HEAD）。 */
const REV_RE = /\b([0-9a-f]{7,40}|HEAD):$/;

/**
 * 纯函数：把一行里出现的 `.pi-orchestrator/<suffix>` 全部抽出来。
 * 后缀遇到空白、引号、半角/全角标点就停 —— 否则「迁移自 x.md（2026-11-19…」会把日期当成路径的一部分。
 * @returns {{target:string, suffix:string, rev:string|null}[]}
 */
export function extractRefs(line) {
	const out = [];
	const re = /\.pi-orchestrator\/([^\s`'")\]<>|，。；、（）「」《》：]+)?/g;
	let m;
	while ((m = re.exec(line)) !== null) {
		const suffix = (m[1] ?? "").replace(/[.,;:]+$/, "");
		const before = line.slice(0, m.index);
		const rev = REV_RE.exec(before);
		out.push({ target: `${PREFIX}/${suffix}`, suffix, rev: rev ? rev[1] : null });
	}
	return out;
}

/** 迁移声明行：`> 迁移自 <原路径>（…）`。记的是来历，不是链接。 */
/** 行级豁免：某一行确实要举例一个悬空路径（自测 fixture、语法示例）时加这个标记。 */
const IGNORE_MARK = "doc-links-ignore";
export const PROVENANCE_RE = /^\s*>?\s*迁移自\s/;

/** 迁移后的落点：只认 docs/contracts/ 与 docs/history/ 下的被跟踪文件。 */
export function migratedHome(suffix, tracked) {
	const base = suffix.split("/").pop();
	for (const p of tracked) {
		if (!p.startsWith("docs/contracts/") && !p.startsWith("docs/history/")) continue;
		if (p === suffix || p.endsWith(`/${suffix}`)) return p;
		if (base && p.endsWith(`/${base}`)) return p;
	}
	return null;
}

/** 通配/占位符：`**`、`{CONTRACT,PLAN}`、`<N>`、`$X` —— 不是某一个具体文件。 */
const GLOB_CHARS_RE = /[*?[\]{}<>$]/;

/**
 * 纯函数：给一条引用定性。
 *   · 带 rev 的 `<rev>:<path>`：只要那个 rev 下真有 blob 就算通过（历史对象不可变）
 *   · 目录 / 通配 / 占位符：本地目录约定，不算产物链接
 *   · 其余（像一个具体文件）：必须在 `git ls-files` 里，否则是死链
 * @param {{target:string, suffix:string, rev:string|null}} ref
 * @param {Set<string>} tracked  被跟踪路径集合
 * @param {(rev:string, path:string)=>boolean} blobAt  rev 下是否有这个 blob
 */
export function classify(ref, tracked, blobAt, isProvenance = false) {
	if (ref.rev) return blobAt(ref.rev, ref.target) ? "REF_HIST" : "REF_DEAD";
	if (isProvenance) return migratedHome(ref.suffix, tracked) ? "REF_MIGRATED" : "REF_DEAD";
	if (ref.suffix === "") return "REF_LOCAL"; // 光秃秃的 `.pi-orchestrator/`
	if (GLOB_CHARS_RE.test(ref.suffix)) return "REF_LOCAL"; // 通配/占位符：不是具体文件
	if (!FILE_EXT_RE.test(ref.suffix)) return "REF_LOCAL"; // 目录
	return tracked.has(ref.target) ? "REF_OK" : "REF_DEAD";
}

const list = (args) => execFileSync("git", args, { encoding: "utf8", maxBuffer: 64 << 20 });
const isBinary = (buf) => buf.includes(0);

function selfTest() {
	// 每条用例自带「哪些路径算被跟踪」，这样 REF_OK 的绿侧也能被自证
	const cases = [
		// 文件引用：在跟踪表里 → 绿；不在 → 红
		["见 `.pi-orchestrator/v4/CONTRACT.md`", [`${PREFIX}/v4/CONTRACT.md`], 1, "REF_OK"], // doc-links-ignore
		["跑 `.pi-orchestrator/v4/CONTRACT.md` 看契约", [], 1, "REF_DEAD"], // doc-links-ignore
		// 目录 / 通配 / 占位符：本地目录约定，不算链接
		["排除 `out/`：`.pi-orchestrator/out/shots/`", [], 1, "REF_LOCAL"], // doc-links-ignore
		["glob `{ glob: \".pi-orchestrator/**\" }`", [], 1, "REF_LOCAL"], // doc-links-ignore
		["`.pi-orchestrator/`", [], 1, "REF_LOCAL"], // doc-links-ignore
		["`.gitignore` 行：.pi-orchestrator/**/*.gz", [], 1, "REF_LOCAL"], // doc-links-ignore
		["根 `.pi-orchestrator/v4/{CONTRACT,PLAN}.md`", [], 1, "REF_LOCAL"], // doc-links-ignore
		// 历史 blob：只要求那个 rev 下真有这个对象
		["git show 9022863:.pi-orchestrator/v4/f3.mjs", [], 1, "REF_HIST"], // doc-links-ignore
		["git show 1111111:.pi-orchestrator/v4/f3.mjs", [], 1, "REF_DEAD"], // doc-links-ignore
		// 没有该前缀：零命中
		["`scripts/e2e/affected-map.mjs`", [], 0, null],
		// 迁移声明：落点存在 → 绿；落点不存在 → 红（来历也得能核验）
		["> 迁移自 .pi-orchestrator/v4/CONTRACT.md（2026-11-19）", // doc-links-ignore
			["docs/contracts/v4/CONTRACT.md"], 1, "REF_MIGRATED"],
		["> 迁移自 .pi-orchestrator/v4/GONE.md（2026-11-19）", // doc-links-ignore
			["docs/contracts/v4/CONTRACT.md"], 1, "REF_DEAD"],
		["> 迁移自 .pi-orchestrator/v4/TRACKER.md（2026-11-19）", // doc-links-ignore
			["docs/history/v4-dispatch/TRACKER.md"], 1, "REF_MIGRATED"], // 改了目录名，按 basename 对
	];
	const blobAt = (rev, p) => rev === "9022863" && p === `${PREFIX}/v4/f3.mjs`;
	let ok = true;
	for (const [line, trackedList, want, expect] of cases) {
		const tracked = new Set(trackedList);
		const prov = PROVENANCE_RE.test(line);
		const got = extractRefs(line).map((r) => classify(r, tracked, blobAt, prov));
		const pass = got.length === want && (want === 0 || got[0] === expect);
		const label = `${JSON.stringify(line.slice(0, 46))} → ${got.length} 条 ${got[0] ?? "-"}`;
		if (!pass) {
			ok = false;
			console.log(`  ✗ ${label}（期望 ${want} 条 ${expect}）`);
		} else {
			console.log(`  ✓ ${label}`);
		}
	}
	console.log(ok ? "✅ self-test 全绿" : "❌ self-test 失败");
	return ok ? 0 : 1;
}

function main(argv) {
	if (argv.includes("--self-test")) return selfTest();
	const verbose = argv.includes("--list");
	const tracked = new Set(list(["ls-files"]).split("\n").filter(Boolean));
	const counts = { REF_OK: 0, REF_HIST: 0, REF_MIGRATED: 0, REF_LOCAL: 0, REF_DEAD: 0 };
	const dead = [];
	const local = [];
	const blobCache = new Map();
	const blobAt = (rev, path) => {
		const key = `${rev}:${path}`;
		if (blobCache.has(key)) return blobCache.get(key);
		let hit = false;
		try {
			list(["cat-file", "-e", `${rev}:${path}`]);
			hit = true;
		} catch {
			hit = false;
		}
		blobCache.set(key, hit);
		return hit;
	};

	for (const file of tracked) {
		let buf;
		try {
			buf = readFileSync(file);
		} catch {
			continue; // 已删除但仍在索引里的路径（不该发生，但不值得因此判红）
		}
		if (isBinary(buf) || buf.length > 2 << 20) continue;
		const lines = buf.toString("utf8").split("\n");
		lines.forEach((line, i) => {
			if (!line.includes(PREFIX)) return;
			// 本行的路径是判据样例（自测 fixture / 文档举例），不是真引用
			if (line.includes(IGNORE_MARK)) return;
			const prov = PROVENANCE_RE.test(line);
			for (const ref of extractRefs(line)) {
				const verdict = classify(ref, tracked, blobAt, prov);
				counts[verdict]++;
				if (verdict === "REF_DEAD") dead.push({ file, line: i + 1, target: ref.target });
				if (verdict === "REF_LOCAL") local.push({ file, line: i + 1, target: ref.target });
			}
		});
	}

	const n = (k) => String(counts[k]).padStart(String(Object.values(counts).reduce((a, b) => a + b, 0)).length);
	console.log("协调者工作区引用检查 · `.pi-orchestrator/` 不再是仓库内容");
	console.log(`  扫描：${tracked.size} 个被跟踪文件（判据是 git ls-files，不是工作区）`);
	console.log(
		`  REF_OK ${n("REF_OK")} / REF_HIST ${n("REF_HIST")} / REF_MIGRATED ${n("REF_MIGRATED")} / ` +
			`REF_LOCAL ${n("REF_LOCAL")} / REF_DEAD ${n("REF_DEAD")}`,
	);
	if (verbose) {
		console.log("  ── REF_LOCAL（本地目录约定，不是产物链接）──");
		for (const l of local) console.log(`    ${l.file}:${l.line}  ${l.target}`);
	}
	if (counts.REF_DEAD > 0) {
		console.log("  ── REF_DEAD（引用指向 fresh clone 里不存在的文件）──");
		for (const d of dead) console.log(`    ${d.file}:${d.line}  ${d.target}`);
		console.log("  ❌ 死链：把目标迁到 docs/ 并改引用，或改成不指向具体路径的历史说明");
		return 1;
	}
	console.log("  ✅ REF_DEAD 0：仓库里没有指向协调者工作区的悬空引用");
	return 0;
}

process.exit(main(process.argv.slice(2)));
