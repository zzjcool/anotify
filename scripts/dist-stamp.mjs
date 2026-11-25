#!/usr/bin/env node
/* Anotify · dist 新鲜度检查：把「dist 是新的」变成可判定的事实
 *
 * 拦的事故：陈旧 dist 假绿三次（22d14d47 / 1256350f / 一次波末假绿）。根因是
 * scripts/e2e/run_all.sh 的前置段只在 internal/server/dist 缺失或为空时才跑
 * make fe —— 改了 web-src/ 却忘了重建时，e2e 验的是上一轮的前端产物，
 * 「绿」与「验了旧东西」同形（本仓缝隙 ①）。本文件把那件事从人肉记忆变成文件事实。
 *
 * 做法：把 dist 的**输入面**指纹算出来，写进产物目录里的 .source-stamp；
 * e2e 前置比对一次，不一致就硬失败，并点名到具体文件。
 *
 * 判据（全部纯函数，--self-test 在内存 fixture 上自证红/绿两侧）
 *   a. --write  ：输入面逐文件 sha256 → internal/server/dist/.source-stamp
 *                 （JSON：digests 路径→sha256 / stamp 总指纹 / counts 统计 / surfaces 说明）
 *   b. --check  ：重算并与 stamp 比对；一致 ⇒ 0，不一致 ⇒ 1，并逐条列出
 *                 变动 / 新增 / 删除（路径 + 旧前 8 位 → 新前 8 位）以及修法 make fe。
 *   c. 反假绿：stamp 缺失 ⇒ 判红并提示 make fe；输入面解析为空 ⇒ 判红（路径配置坏了，
 *      否则「什么都没查」会被记成绿）；某一路输入面为空同样判红；stamp 字段与
 *      digests 不自洽（被手改）⇒ 判红。
 *   d. 输入面绝不含产物自身（internal/server/dist）：stamp 自我指涉会让比对永远失败。
 *      web/ 下由 sitegen 生成的产物（顶层 html、语言子目录、i18n js）不入手写清单 ——
 *      生成物清单只有 .gitignore 一份，用 git ls-files -co --exclude-standard 取源文件。
 *   e. 二进制文件照常参与 sha256（web/fonts 与 web/assets 是运行时依赖，跳过等于留洞）；
 *      超过上限的大文件跳过，两者都把计数打印出来（不静默跳过）。
 *   f. dist 里出现「源已不存在、本轮也不会产出」的文件 ⇒ DIST_ORPHAN 并点名（#649：
 *      删掉的页面正是这么被 go:embed 编进二进制、继续对外 200 的）；缺本轮该产出的
 *      文件 ⇒ DIST_INCOMPLETE。期望集由 scripts/hash.mjs 的 expectedDistPaths 给出。
 *   g. 阈值与规则码都是常量，--self-test 逐条覆盖；改判据必须先改自测。
 *
 * 用法
 *   node scripts/dist-stamp.mjs --write      # make fe 末尾调用（重建后落 stamp）
 *   node scripts/dist-stamp.mjs --check      # make check-dist-fresh / scripts/e2e/run_all.sh 前置
 *   node scripts/dist-stamp.mjs --self-test  # 内存 fixture 红/绿自证
 *   退出码：0 一致 / 1 陈旧、脏、缺失或输入面为空 / 2 用法或环境问题
 *   来历：docs/lint-guards.md §1 的 L8 / §2 backlog #3（dist 新鲜度检查）
 *
 * 为什么陈旧要硬失败、不自动重建：自动 make fe 会让「你忘了重建」这条缝隙重新变成静默
 * （重建后永远绿），也把 e2e 的启动时间翻倍；硬失败把它换成一次带文件名的显式报错。
 */
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { existsSync } from "node:fs";
import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { runsAsMain } from "./lib/direct-run.mjs";
import { expectedDistPaths, MANIFEST_NAME, OWNED_BY_OTHER_STAGES } from "./hash.mjs";

export const STAMP_NAME = ".source-stamp";
export const STAMP_VERSION = 1;
/** 超过这个大小的文件不读内容（只记名字 + 打印），避免把仓库里的巨物吃进内存。 */
export const MAX_DIGEST_BYTES = 4 * 1024 * 1024;
/** 判定「二进制」只看开头这么多字节里有没有 NUL。 */
const BINARY_SNIFF = 8192;

// 规则码：便于 --self-test 断言红的是哪一类，也便于用 @guard scripts/dist-stamp.mjs#<码> 引用。
export const DIST_STALE = "DIST_STALE";
export const DIST_STAMP_MISSING = "DIST_STAMP_MISSING";
export const DIST_STAMP_INCONSISTENT = "DIST_STAMP_INCONSISTENT";
export const DIST_SURFACE_EMPTY = "DIST_SURFACE_EMPTY";
export const DIST_LEAK_PRODUCT = "DIST_LEAK_PRODUCT";
export const DIST_ORPHAN = "DIST_ORPHAN";
export const DIST_INCOMPLETE = "DIST_INCOMPLETE";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const DIST_REL = "internal/server/dist";
const DIST_DIR = path.join(ROOT, ...DIST_REL.split("/"));

/**
 * 输入面：dist 里每一个字节都是由这些文件派生出来的。
 *
 * 输入面 = `make fe` 两步读的**源**文件（sitegen：web-src/ + api/；hash.mjs：web/）。
 * 由它们生成的东西（web/*.html、web/{en,es,ja}、web/i18n.*.js）不是输入面。
 */
export const SURFACES = Object.freeze([
	{
		id: "web-src",
		note: "sitegen 的 layouts / pages / locales + Tailwind 输入（tailwind.input.css）",
	},
	{
		id: "api",
		note: "docs 页在构建期嵌入的 spec（api/*.yaml）",
	},
	{
		id: "web",
		note: "运行时依赖的前端源文件（sitegen 不生成的那些：ui.css / tokens.css / *.js / assets / fonts / vendor）",
	},
]);

function sha256(buf) {
	return createHash("sha256").update(buf).digest("hex");
}

/** 开头这么多字节里有 NUL 就当二进制（字体、png 会命中；utf-8 文本不会）。 */
export function looksBinary(buf) {
	const head = buf.subarray(0, Math.min(buf.length, BINARY_SNIFF));
	for (const byte of head) if (byte === 0) return true;
	return false;
}

/**
 * digestSurface：纯函数。files 是 `[{ rel, buf, size? }]`。
 *   · 有 buf ⇒ 参与 sha256（二进制也参与：字体/图标变了 dist 就会变）；
 *   · 没有 buf（超大文件只传 size）或 size 超上限 ⇒ 跳过，但记进 counts.skipped
 *     （调用方负责把计数打印出来 —— 静默跳过会让「没查」冒充「查过了」）。
 */
export function digestSurface(files) {
	const digests = {};
	const counts = { text: 0, binary: 0, skipped: [] };
	for (const f of files) {
		const size = f.size ?? (f.buf ? f.buf.length : 0);
		if (f.buf === undefined || size > MAX_DIGEST_BYTES) {
			counts.skipped.push({ rel: f.rel, size });
			continue;
		}
		digests[f.rel] = sha256(f.buf);
		if (looksBinary(f.buf)) counts.binary++;
		else counts.text++;
	}
	return { digests, counts };
}

/** 总指纹：路径排序后按「路径 NUL sha 换行」拼接再 sha256 ⇒ 与遍历顺序无关。 */
export function stampOf(digests) {
	const h = createHash("sha256");
	for (const rel of Object.keys(digests).sort()) h.update(`${rel}\u0000${digests[rel]}\n`);
	return h.digest("hex");
}

/** 逐文件比对，返回按路径排序的变动表（changed / added / removed）。 */
export function diffDigests(prev, next) {
	const out = [];
	for (const rel of [...new Set([...Object.keys(prev), ...Object.keys(next)])].sort()) {
		const a = prev[rel];
		const b = next[rel];
		if (a && b) {
			if (a !== b) out.push({ rel, kind: "changed", old: a, new: b });
		} else if (!a) {
			out.push({ rel, kind: "added", old: null, new: b });
		} else {
			out.push({ rel, kind: "removed", old: a, new: null });
		}
	}
	return out;
}

/** dist 里应当有什么：actual 是实际路径，expected 来自 scripts/hash.mjs 的 expectedDistPaths。 */
export function compareDistShape({ actual, expected }) {
	const want = new Set(expected.files);
	const have = new Set(actual);
	return {
		orphans: actual.filter((rel) => !want.has(rel)).sort(),
		// .source-stamp 不在 missing 面：它是本脚本即将写的文件，缺它就是 DIST_STAMP_MISSING 那条判据的事。
		missing: expected.files.filter((rel) => !have.has(rel) && !OWNED_BY_OTHER_STAGES.has(rel)).sort(),
		unresolved: [...expected.unresolved].sort(),
	};
}

/**
 * judge：纯函数。prev = 读回来的 stamp（null 缺失 / { invalid } 不可用），next = 本次重算的
 * { digests, counts }，emptySurfaces = 解析为空的输入面，dist = { actual, expected|null, manifestError }。
 * 顺序有语义：先证「真的查了东西」，再证 dist 没有跨轮残留，再证 stamp 自洽，最后比内容。
 */
export function judge({ prev, next, emptySurfaces = [], dist = null }) {
	const total = Object.keys(next.digests).length;
	const stamp = stampOf(next.digests);
	const base = { counts: next.counts, changes: [], stamp, total };
	if (total === 0 || emptySurfaces.length) {
		return { ...base, ok: false, kind: DIST_SURFACE_EMPTY, emptySurfaces };
	}
	if (dist && dist.actual.length) {
		if (!dist.expected) {
			return { ...base, ok: false, kind: DIST_INCOMPLETE, detail: `manifest.json ${dist.manifestError}` };
		}
		const shape = compareDistShape(dist);
		const shapeBase = { ...base, shape };
		if (shape.orphans.length) return { ...shapeBase, ok: false, kind: DIST_ORPHAN };
		if (shape.unresolved.length) return { ...shapeBase, ok: false, kind: DIST_INCOMPLETE, unresolved: shape.unresolved };
		if (shape.missing.length) return { ...shapeBase, ok: false, kind: DIST_INCOMPLETE, missing: shape.missing };
	}
	if (prev === null || prev === undefined) return { ...base, ok: false, kind: DIST_STAMP_MISSING };
	if (prev.invalid) return { ...base, ok: false, kind: DIST_STAMP_INCONSISTENT, detail: prev.invalid };
	const changes = diffDigests(prev.digests, next.digests);
	const selfConsistent = prev.stamp === stampOf(prev.digests);
	if (changes.length === 0 && selfConsistent && prev.stamp === stamp) {
		return { ...base, ok: true, kind: "OK", changes };
	}
	return {
		...base,
		ok: false,
		kind: DIST_STALE,
		changes,
		prevStamp: prev.stamp,
		detail: changes.length === 0 ? "stamp 字段与实际输入不自洽（文件被手改，或判据算法变了）" : "",
	};
}

/** 输入面里一旦混进产物自身就判红：那样 stamp 会随自己变，比对永远不可能一致。 */
export function findProductLeak(rels) {
	return rels.filter((rel) => rel === DIST_REL || rel.startsWith(`${DIST_REL}/`));
}

export function summaryLine(counts, total) {
	return `输入面 ${total} 个文件：文本 ${counts.text} / 二进制 ${counts.binary}（已计入 sha256）/ 超大跳过 ${counts.skipped.length}`;
}

const MAX_CHANGES_SHOWN = 40;

/** 把判据结果渲染成给人看的多行文本（CLI 与 --self-test 共用同一份文案）。 */
export function formatVerdict(res) {
	const lines = [summaryLine(res.counts, res.total)];
	for (const s of res.counts.skipped) {
		lines.push(
			`  · 跳过大文件（超过 ${MAX_DIGEST_BYTES / 1024 / 1024} MiB，未参与比对）：${s.rel}（${s.size} 字节）`,
		);
	}
	if (res.ok) {
		lines.push(`✅ dist 与源一致：stamp ${res.stamp.slice(0, 12)}…`);
		return lines;
	}
	if (res.kind === DIST_SURFACE_EMPTY) {
		lines.push("❌ 输入面为空：一个输入文件都没解析出来 —— 路径配置坏了");
		lines.push("   （不判红的话，「什么都没查」会被记成绿）");
		if (res.emptySurfaces && res.emptySurfaces.length) {
			lines.push(`   空的分路：${res.emptySurfaces.join(" / ")}`);
		}
		lines.push("   检查 SURFACES 常量，以及是否在仓库根目录（脚本按自身位置定位根目录）里跑");
		return lines;
	}
	if (res.kind === DIST_ORPHAN) {
		lines.push(`❌ ${DIST_REL} 里有「源已不存在、本轮也不会产出」的残留（它们会被 go:embed 编进二进制）：`);
		for (const rel of res.shape.orphans.slice(0, MAX_CHANGES_SHOWN)) lines.push(`     - ${rel}`);
		if (res.shape.orphans.length > MAX_CHANGES_SHOWN) {
			lines.push(`     … 还有 ${res.shape.orphans.length - MAX_CHANGES_SHOWN} 条，略`);
		}
		lines.push("   为什么不该在：dist 是 web/ 的指纹产物，上面这些路径在 web/ 与 manifest.json 里都找不到来源");
		lines.push("   修法：make fe      # 重新生成；hash.mjs 只删本轮没产出的文件，并逐个打印");
		return lines;
	}
	if (res.kind === DIST_INCOMPLETE) {
		lines.push("❌ dist 不完整：缺少本轮该产出的文件（或 manifest.json 读不出来）");
		for (const rel of res.missing || []) lines.push(`     - 缺 ${rel}`);
		for (const rel of res.unresolved || []) lines.push(`     - ${rel} 在 manifest.json 里没有指纹条目`);
		if (res.detail) lines.push(`   manifest.json ${res.detail}`);
		lines.push("   修法：make fe      # 重建 web/ → internal/server/dist/ 并刷新 stamp");
		return lines;
	}
	if (res.kind === DIST_STAMP_MISSING) {
		lines.push(`❌ 没有 stamp：${DIST_REL}/${STAMP_NAME} 不存在，无法证明这份前端产物是最新的`);
		lines.push("   修法：make fe      # 重新生成 web/ → internal/server/dist/ 并写入 stamp");
		return lines;
	}
	if (res.kind === DIST_STAMP_INCONSISTENT) {
		lines.push(`❌ stamp 不可用：${DIST_REL}/${STAMP_NAME} ${res.detail}`);
		lines.push("   修法：make fe      # 重新写入");
		return lines;
	}
	lines.push(`❌ dist 陈旧：${DIST_REL} 与源不一致（改了源却没重建，e2e 会验旧产物）`);
	if (res.changes.length === 0) {
		lines.push(`   ${res.detail}`);
	} else {
		lines.push("   变动 / 新增 / 删除的输入（旧前 8 位 → 新前 8 位）：");
		for (const c of res.changes.slice(0, MAX_CHANGES_SHOWN)) {
			const old8 = c.old ? c.old.slice(0, 8) : "（新增）";
			const new8 = c.new ? c.new.slice(0, 8) : "（已删除）";
			const mark = c.kind === "changed" ? "~" : c.kind === "added" ? "+" : "-";
			lines.push(`     ${mark} ${c.rel}  ${old8} → ${new8}`);
		}
		if (res.changes.length > MAX_CHANGES_SHOWN) {
			lines.push(`     … 还有 ${res.changes.length - MAX_CHANGES_SHOWN} 条，略`);
		}
	}
	lines.push("   修法：make fe      # 重建 web/ → internal/server/dist/ 并刷新 stamp");
	return lines;
}

// ---------------------------------------------------------------------------
// 输入面：从磁盘取出真实的文件清单（判据本身不碰磁盘，方便 --self-test 纯内存）
// ---------------------------------------------------------------------------

async function walkDir(relDir) {
	const out = [];
	async function rec(rel) {
		let entries;
		try {
			entries = await fs.readdir(path.join(ROOT, rel), { withFileTypes: true });
		} catch (e) {
			if (e.code === "ENOENT") return;
			throw e;
		}
		entries.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
		for (const e of entries) {
			const child = `${rel}/${e.name}`;
			if (e.isDirectory()) await rec(child);
			else out.push(child);
		}
	}
	await rec(relDir);
	return out;
}

/**
 * web/ 下的**源**文件：tracked + untracked，但按 .gitignore 排除生成物。
 * 为什么不手写「生成物黑名单」：那是同一份知识的第二份拷贝，一旦 Makefile 换了
 * -langs 或 sitegen 多产一种文件，黑名单会安静地过期。.gitignore 是唯一事实源。
 */
function gitWebSources() {
	let stdout;
	try {
		stdout = execFileSync("git", ["ls-files", "-co", "--exclude-standard", "-z", "--", "web"], {
			cwd: ROOT,
			encoding: "utf8",
			maxBuffer: 1 << 26,
		});
	} catch (e) {
		throw new Error(
			`取 web/ 源文件失败（git ls-files）：${e.message}；本检查要求 git 工作区（生成物清单以 .gitignore 为准）`,
		);
	}
	return stdout
		.split("\u0000")
		.filter(Boolean)
		.filter((rel) => existsSync(path.join(ROOT, ...rel.split("/"))))
		.sort();
}

export async function loadSurface() {
	const found = [];
	const perSurface = {};

	const webSrc = await walkDir("web-src");
	perSurface["web-src"] = webSrc;
	found.push(...webSrc);

	const apiEntries = await fs.readdir(path.join(ROOT, "api"), { withFileTypes: true }).catch(() => []);
	const api = apiEntries
		.filter((e) => e.isFile() && e.name.endsWith(".yaml"))
		.map((e) => `api/${e.name}`)
		.sort();
	perSurface.api = api;
	found.push(...api);

	const web = gitWebSources();
	perSurface.web = web;
	found.push(...web);

	const rels = [...new Set(found)].sort();
	const leaked = findProductLeak(rels);
	if (leaked.length) {
		throw new Error(
			`${DIST_LEAK_PRODUCT}：输入面里出现了产物自身 ${leaked.slice(0, 3).join(" / ")}（stamp 自我指涉会让比对永远失败）`,
		);
	}
	const emptySurfaces = Object.entries(perSurface)
		.filter(([, list]) => list.length === 0)
		.map(([id]) => id);
	return { rels, perSurface, emptySurfaces };
}

async function readInputs(rels) {
	const files = [];
	for (const rel of rels) {
		const abs = path.join(ROOT, ...rel.split("/"));
		let st;
		try {
			st = await fs.stat(abs);
		} catch (e) {
			if (e.code === "ENOENT") continue; // 被删掉但还在索引里的路径不算输入
			throw e;
		}
		if (!st.isFile()) continue;
		if (st.size > MAX_DIGEST_BYTES) {
			files.push({ rel, size: st.size });
			continue;
		}
		files.push({ rel, size: st.size, buf: await fs.readFile(abs) });
	}
	return files;
}

async function writeStamp({ digests, counts }) {
	const payload = {
		version: STAMP_VERSION,
		tool: "scripts/dist-stamp.mjs",
		// 产物目录被 gitignore，这份 stamp 也是构建副产物；它由 make fe 末尾写入
		// （hash.mjs 只 prune，不碰其它阶段的产物）。
		surfaces: SURFACES.map((s) => ({ id: s.id, note: s.note })),
		counts: {
			total: Object.keys(digests).length,
			text: counts.text,
			binary: counts.binary,
			skipped: counts.skipped.map((s) => s.rel),
		},
		digests,
		stamp: stampOf(digests),
	};
	await fs.mkdir(DIST_DIR, { recursive: true });
	await fs.writeFile(path.join(DIST_DIR, STAMP_NAME), `${JSON.stringify(payload, null, 2)}\n`);
	return payload;
}

const SHA_RE = /^[0-9a-f]{64}$/;

async function readStampFile() {
	let raw;
	try {
		raw = await fs.readFile(path.join(DIST_DIR, STAMP_NAME), "utf8");
	} catch (e) {
		if (e.code === "ENOENT") return null;
		throw e;
	}
	let parsed;
	try {
		parsed = JSON.parse(raw);
	} catch {
		return { invalid: "不是合法 JSON" };
	}
	if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return { invalid: "内容不是对象" };
	const { digests, stamp } = parsed;
	if (!digests || typeof digests !== "object" || Array.isArray(digests)) return { invalid: "缺少 digests 字段" };
	if (typeof stamp !== "string" || !SHA_RE.test(stamp)) return { invalid: "缺少形状正确的 stamp 字段" };
	for (const [rel, digest] of Object.entries(digests)) {
		if (typeof digest !== "string" || !SHA_RE.test(digest)) return { invalid: `digests 里的 ${rel} 形状不对` };
	}
	return { digests, stamp };
}

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

/** dist 的实际文件清单（相对 dist）与「应当有什么」的期望集（源目录 + manifest.json 推出来）。 */
async function loadDistState() {
	const actual = (await walkDir(DIST_REL)).map((rel) => rel.slice(DIST_REL.length + 1)).sort();
	const srcRels = (await walkDir("web")).map((rel) => rel.slice("web/".length));
	let manifest = null;
	let manifestError = "";
	try {
		const parsed = JSON.parse(await fs.readFile(path.join(DIST_DIR, MANIFEST_NAME), "utf8"));
		if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) manifest = parsed;
		else manifestError = "内容不是对象";
	} catch (e) {
		manifestError = e.code === "ENOENT" ? "不存在（dist 不是 hash.mjs 产出的）" : `读不出：${e.message}`;
	}
	return {
		actual,
		manifestError,
		expected: manifest ? expectedDistPaths({ srcRels, manifest }) : null,
	};
}

async function main(mode) {
	const { rels, emptySurfaces } = await loadSurface();
	const inputs = await readInputs(rels);
	const { digests, counts } = digestSurface(inputs);
	const dist = await loadDistState();

	if (mode === "write") {
		const res = judge({ prev: null, next: { digests, counts }, emptySurfaces, dist });
		if (res.kind !== "OK" && res.kind !== DIST_STAMP_MISSING) {
			for (const line of formatVerdict(res)) console.error(line);
			console.error("❌ 拒绝写入 stamp：dist 不是这一轮源的干净产物（先把上一行说的问题修掉）");
			return 1;
		}
		const payload = await writeStamp({ digests, counts });
		console.log(`✅ 已写入 ${DIST_REL}/${STAMP_NAME}：${summaryLine(counts, res.total)}`);
		console.log(`   stamp ${payload.stamp.slice(0, 12)}…`);
		for (const s of counts.skipped) console.log(`   · 跳过大文件：${s.rel}（${s.size} 字节）`);
		return 0;
	}

	const prev = await readStampFile();
	const res = judge({ prev, next: { digests, counts }, emptySurfaces, dist });
	const lines = formatVerdict(res);
	const emit = res.ok ? console.log : console.error;
	for (const line of lines) emit(line);
	return res.ok ? 0 : 1;
}

// ---------------------------------------------------------------------------
// 自测：纯内存 fixture，红/绿两侧都自证（判据失效时不能还是绿的）
// ---------------------------------------------------------------------------

function fixtureFiles() {
	return {
		"web-src/pages/index.html": Buffer.from("<h1>hi</h1>\n"),
		"web-src/locales/en.yaml": Buffer.from("common.close: Close\n"),
		"api/resource.yaml": Buffer.from("openapi: 3.1.0\n"),
		"web/ui.css": Buffer.from(".a { color: red }\n"),
	};
}

function nextOf(map) {
	return digestSurface(Object.entries(map).map(([rel, buf]) => ({ rel, buf })));
}

function prevOf(next) {
	return { digests: next.digests, stamp: stampOf(next.digests) };
}

export function selfTest() {
	let failed = 0;
	let ran = 0;
	const check = (name, fn) => {
		ran++;
		let ok = false;
		let detail = "";
		try {
			ok = fn() === true;
		} catch (e) {
			detail = `（抛异常：${e.message}）`;
		}
		if (!ok) failed++;
		console.log(`${ok ? "✓" : "✗"} ${name}${detail}`);
	};

	check("green: stamp 与输入面一致 ⇒ 判绿", () => {
		const next = nextOf(fixtureFiles());
		const res = judge({ prev: prevOf(next), next });
		return res.ok === true && res.total === 4 && formatVerdict(res).join("\n").includes("✅");
	});

	check("red: 改一个字节 ⇒ 判红并点名该文件（旧 → 新）", () => {
		const before = nextOf(fixtureFiles());
		const mutated = fixtureFiles();
		mutated["web-src/pages/index.html"] = Buffer.from("<h1>hi</h1> \n");
		const after = nextOf(mutated);
		const res = judge({ prev: prevOf(before), next: after });
		if (res.ok || res.kind !== DIST_STALE || res.changes.length !== 1) return false;
		const c = res.changes[0];
		if (c.kind !== "changed" || c.rel !== "web-src/pages/index.html") return false;
		const text = formatVerdict(res).join("\n");
		return (
			text.includes("web-src/pages/index.html") &&
			text.includes(`${c.old.slice(0, 8)} → ${c.new.slice(0, 8)}`) &&
			text.includes("make fe")
		);
	});

	check("red: 新增输入文件 ⇒ 判红（标「新增」并给新指纹）", () => {
		const before = nextOf(fixtureFiles());
		const mutated = fixtureFiles();
		mutated["web-src/pages/brand-new.html"] = Buffer.from("<p>new</p>\n");
		const after = nextOf(mutated);
		const res = judge({ prev: prevOf(before), next: after });
		if (res.ok || res.kind !== DIST_STALE || res.changes.length !== 1) return false;
		const c = res.changes[0];
		const text = formatVerdict(res).join("\n");
		return (
			c.kind === "added" &&
			c.rel === "web-src/pages/brand-new.html" &&
			text.includes("（新增）") &&
			text.includes(c.new.slice(0, 8)) &&
			text.includes("make fe")
		);
	});

	check("red: 删除输入文件 ⇒ 判红（标「已删除」并给旧指纹）", () => {
		const before = nextOf(fixtureFiles());
		const mutated = fixtureFiles();
		delete mutated["web/ui.css"];
		const after = nextOf(mutated);
		const res = judge({ prev: prevOf(before), next: after });
		if (res.ok || res.kind !== DIST_STALE || res.changes.length !== 1) return false;
		const c = res.changes[0];
		const text = formatVerdict(res).join("\n");
		return (
			c.kind === "removed" &&
			c.rel === "web/ui.css" &&
			text.includes("（已删除）") &&
			text.includes(c.old.slice(0, 8)) &&
			text.includes("make fe")
		);
	});

	check("red: stamp 缺失 ⇒ 判红并提示 make fe", () => {
		const next = nextOf(fixtureFiles());
		const res = judge({ prev: null, next });
		const text = formatVerdict(res).join("\n");
		return res.kind === DIST_STAMP_MISSING && text.includes(STAMP_NAME) && text.includes("make fe");
	});

	check("red: 输入面 0 个文件 ⇒ 判红（即使 stamp 存在也红）", () => {
		const empty = digestSurface([]);
		const prev = { digests: {}, stamp: stampOf({}) };
		const res = judge({ prev, next: empty });
		const text = formatVerdict(res).join("\n");
		return (
			res.kind === DIST_SURFACE_EMPTY &&
			res.total === 0 &&
			text.includes("路径配置坏了") &&
			text.includes("什么都没查")
		);
	});

	check("red: 某一路输入面为空 ⇒ 判红并点名该路", () => {
		const next = nextOf(fixtureFiles());
		const res = judge({ prev: prevOf(next), next, emptySurfaces: ["api"] });
		const text = formatVerdict(res).join("\n");
		return res.kind === DIST_SURFACE_EMPTY && text.includes("空的分路：api");
	});

	check("green: 二进制计入 sha256 + 超大跳过，两个计数都打印（不静默）", () => {
		const files = [
			{ rel: "web/fonts/inter-latin.woff2", buf: Buffer.from([0x77, 0x4f, 0x46, 0x32, 0x00, 0x01, 0x02]) },
			{ rel: "web-src/pages/index.html", buf: Buffer.from("ok\n") },
			{ rel: "web/assets/huge.bin", size: MAX_DIGEST_BYTES + 1 },
		];
		const { digests, counts } = digestSurface(files);
		const lines = formatVerdict(judge({ prev: null, next: { digests, counts } }));
		const text = lines.join("\n");
		return (
			counts.binary === 1 &&
			counts.text === 1 &&
			counts.skipped.length === 1 &&
			digests["web/fonts/inter-latin.woff2"] !== undefined &&
			digests["web/assets/huge.bin"] === undefined &&
			text.includes("二进制 1") &&
			text.includes("超大跳过 1") &&
			text.includes("web/assets/huge.bin")
		);
	});

	check("red: stamp 字段被手改成不自洽 ⇒ 判红并说明原因", () => {
		const next = nextOf(fixtureFiles());
		const tampered = { digests: next.digests, stamp: "0".repeat(64) };
		const res = judge({ prev: tampered, next });
		const text = formatVerdict(res).join("\n");
		return res.kind === DIST_STALE && res.changes.length === 0 && text.includes("不自洽") && text.includes("make fe");
	});

	check("red: stamp 内容不可解析（缺字段 / 坏 JSON）⇒ 判红", () => {
		const next = nextOf(fixtureFiles());
		const res = judge({ prev: { invalid: "不是合法 JSON" }, next });
		return res.kind === DIST_STAMP_INCONSISTENT && formatVerdict(res).join("\n").includes("make fe");
	});

	check("red: dist 里有源已不存在的残留 ⇒ 判红并点名该文件", () => {
		const next = nextOf(fixtureFiles());
		const dist = {
			actual: ["index.html", "manifest.json", STAMP_NAME, "sw.js"],
			expected: { files: ["index.html", "manifest.json", STAMP_NAME].sort(), unresolved: [] },
		};
		const res = judge({ prev: prevOf(next), next, dist });
		const text = formatVerdict(res).join("\n");
		return res.ok === false && res.kind === DIST_ORPHAN && text.includes("sw.js") && text.includes("make fe");
	});

	check("green: 指纹资源 / .source-stamp / manifest.webmanifest 都是产出 ⇒ 不误伤", () => {
		const next = nextOf(fixtureFiles());
		const files = [
			"assets/app.1a2b3c4d.css",
			"index.html",
			"manifest.json",
			"manifest.webmanifest",
			STAMP_NAME,
		].sort();
		const res = judge({ prev: prevOf(next), next, dist: { actual: files, expected: { files, unresolved: [] } } });
		return res.ok === true && formatVerdict(res).join("\n").includes("✅");
	});

	check("red: dist 缺本轮该产出的文件 ⇒ 判红并点名（截断的 dist）", () => {
		const next = nextOf(fixtureFiles());
		const files = ["manifest.json", STAMP_NAME, "index.html"];
		const res = judge({
			prev: prevOf(next),
			next,
			dist: { actual: ["manifest.json", STAMP_NAME], expected: { files, unresolved: [] } },
		});
		const text = formatVerdict(res).join("\n");
		return res.kind === DIST_INCOMPLETE && text.includes("缺 index.html");
	});

	check("red: manifest.json 读不出 ⇒ 判红（期望集无法确立）", () => {
		const next = nextOf(fixtureFiles());
		const res = judge({
			prev: prevOf(next),
			next,
			dist: { actual: ["index.html"], expected: null, manifestError: "不存在（dist 不是 hash.mjs 产出的）" },
		});
		const text = formatVerdict(res).join("\n");
		return res.kind === DIST_INCOMPLETE && text.includes("manifest.json");
	});

	check("green: expectedDistPaths 把源映射成指纹名 + dev-only 不进 dist", () => {
		const { files, unresolved } = expectedDistPaths({
			srcRels: ["index.html", "ui.css", "mock.js", "manifest.webmanifest", "fonts/x.woff2"],
			manifest: { "ui.css": "ui.aaaaaaaa.css", "fonts/x.woff2": "fonts/x.bbbbbbbb.woff2" },
		});
		return (
			files.includes("ui.aaaaaaaa.css") &&
			files.includes("index.html") &&
			files.includes("manifest.webmanifest") &&
			files.includes(STAMP_NAME) &&
			!files.includes("mock.js") &&
			unresolved.length === 0
		);
	});

	check("red: 该指纹的源在 manifest 里没条目 ⇒ unresolved（dist 不完整）", () => {
		const { unresolved } = expectedDistPaths({ srcRels: ["ui.css"], manifest: {} });
		const next = nextOf(fixtureFiles());
		const res = judge({
			prev: prevOf(next),
			next,
			dist: { actual: ["manifest.json", STAMP_NAME], expected: { files: ["manifest.json", STAMP_NAME], unresolved } },
		});
		return unresolved.includes("ui.css") && res.kind === DIST_INCOMPLETE && res.unresolved.includes("ui.css");
	});

	check("red: 产物自身混进输入面 ⇒ 点名（stamp 自我指涉）", () => {
		const leaked = findProductLeak([
			"web-src/pages/index.html",
			`${DIST_REL}/${STAMP_NAME}`,
			`${DIST_REL}/index.html`,
		]);
		return leaked.length === 2 && leaked[0].startsWith(DIST_REL);
	});

	check("green: 总指纹与路径顺序无关（同一输入面同一 stamp）", () => {
		const a = { "web-src/b.html": "1".repeat(64), "web-src/a.html": "2".repeat(64) };
		const b = { "web-src/a.html": "2".repeat(64), "web-src/b.html": "1".repeat(64) };
		return stampOf(a) === stampOf(b) && stampOf(a) !== stampOf({ "web-src/a.html": "2".repeat(64) });
	});

	const ok = failed === 0;
	console.log(ok ? `✅ 自测全绿（${ran} 组）` : `❌ 自测失败 ${failed} 组（共 ${ran} 组）`);
	return ok;
}

const isDirectRun = runsAsMain(import.meta.url);
if (isDirectRun) {
	const USAGE = [
		"用法：node scripts/dist-stamp.mjs --write | --check | --self-test",
		"  来历：docs/lint-guards.md §1 的 L8 行 / §2 backlog #3（dist 新鲜度）",
	].join("\n");
	const args = process.argv.slice(2);
	const modes = args.filter((a) => a === "--write" || a === "--check" || a === "--self-test");
	const unknown = args.filter((a) => !modes.includes(a));
	if (unknown.length || modes.length !== 1) {
		if (unknown.length) console.error(`✗ dist-stamp：不认识的参数 ${unknown.join(" ")}`);
		else console.error("✗ dist-stamp：必须且只能给一个模式");
		console.error(USAGE);
		process.exit(2);
	}
	if (modes[0] === "--self-test") {
		process.exit(selfTest() ? 0 : 1);
	}
	main(modes[0] === "--write" ? "write" : "check")
		.then((code) => process.exit(code))
		.catch((e) => {
			console.error(`❌ ${e.message}`);
			process.exit(2);
		});
}
