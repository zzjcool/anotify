#!/usr/bin/env node
/* SUITE: vars_ui — 节点页 / 命名空间页的「可视化 vars」（issue #476）
 *
 * 这张表要回答四个问题，本套件逐条钉死（真服务 + 真 Chrome，不用 mock）：
 *   ① 值 / 来源与服务端 `status.effectiveVars` **逐键一致**（页面不自己猜来源层）；
 *   ② 「被哪些能力用到」= 当前 ns 能力读面的 `spec.requires.values` 聚合；
 *   ③ 被能力要求但没有生效值 → 红标（`data-missing="true"` + ⚠ 必填）；
 *   ④ 阶段 2 的增删改**序列化进同一份 YAML 文档**，走 `PUT …/yaml`：
 *      改一个 var → YAML 只有该键（+resourceVersion）变化，其它行逐字不变；
 *      删一个 var → YAML 只消失那个键；
 *      前端拒非法名字（1FOO / FOO-BAR），后端对非法名字 400；
 *      UI 对 `/config` 的写调用 = 0（守卫的运行时对照：写请求只出现 `/yaml`）。
 *
 * 造数全走真实产品路径：能力（信封）→ 节点（登记 + spec.vars）→ 命名空间默认 vars。
 * 明确不做的：不直插库、不 mock 端点、不读 `.config` 写入口来"造"页面状态。
 */
import { chromium } from "playwright-core";
import * as H from "../lib/harness.mjs";
import * as V from "../lib/v4.mjs";

const NS = V.DEFAULT_NS;
const NODE = "anode-vars";

let server, browser;

async function injectSession(ctx, sessionValue, base) {
	const u = new URL(base);
	await ctx.addCookies([
		{
			name: "anotify_session",
			value: sessionValue,
			domain: u.hostname,
			path: "/",
			httpOnly: true,
			secure: u.protocol === "https:",
		},
	]);
}

/* 「scope = default」必须落在页面脚本读到 localStorage 之前。节点页没有自己的 ns
 * 选择器（IA：节点是节点维度），它的有效值表跟着侧栏作用域走。 */
async function primeScope(ctx) {
	await ctx.addInitScript((ns) => {
		window.localStorage.setItem("anotify.scope", JSON.stringify({ namespace: ns }));
	}, NS);
}

/* 行级快照：名字 / 值 / 来源 / 是否红标 / 来源单元格原文。 */
async function readRows(page, hostId) {
	return page.evaluate((id) => {
		const host = document.getElementById(id);
		if (!host) return null;
		const rows = Array.from(host.querySelectorAll("tbody tr[data-key]"));
		return rows.map((tr) => {
			const cells = Array.from(tr.querySelectorAll("td"));
			return {
				name: tr.dataset.key,
				missing: tr.dataset.missing === "true",
				sourceAttr: tr.dataset.source || "",
				value: cells[1] ? cells[1].innerText.trim() : "",
				source: cells[2] ? cells[2].innerText.trim() : "",
				used: cells[3] ? cells[3].innerText.trim() : "",
			};
		});
	}, hostId);
}

/* 行级 YAML diff：键序稳定，按「行集合」比较就够（值改动 = 一行换一行）。打印原文，报告里的前后 diff 就是从这里摘的（要求「贴前后 diff 原文」）。 */
function lineDiff(before, after) {
	const a = before.split("\n");
	const b = after.split("\n");
	const setA = new Set(a), setB = new Set(b);
	const removed = a.filter((line) => !setB.has(line));
	const added = b.filter((line) => !setA.has(line));
	console.log("--- yaml before ---");
	console.log(removed.length ? removed.map((l) => "- " + l).join("\n") : "(no removed lines)");
	console.log("+++ yaml after ---");
	console.log(added.length ? added.map((l) => "+ " + l).join("\n") : "(no added lines)");
	return { removed, added };
}

/* 取 `spec.vars` 里某个键的整行（服务端 YAML 的引号/缩进由 yaml.v3 决定，逐字比较）。 */
function yamlLine(text, name) {
	for (const line of String(text || "").split("\n")) {
		const trimmed = line.trim();
		if (trimmed.startsWith(name + ":") || trimmed.startsWith('"' + name + '":')) return line;
	}
	return "";
}

function onlyVarLine(lines, name) {
	// spec.vars 在文档里的缩进由 yaml.v3 决定（2/4 空格都可能），按 trim 后的键名匹配。
	return lines.filter((line) => {
		const trimmed = line.trim();
		return trimmed.startsWith(name + ":") || trimmed.startsWith('"' + name + '":');
	});
}

function yamlSection(text, key) {
	const out = [];
	let indent = -1;
	for (const line of String(text || "").split("\n")) {
		if (line.trim() === "") continue;
		const lead = line.length - line.trimStart().length;
		const head = line.trim();
		if (indent < 0) {
			if (head === key + ":" || head.startsWith(key + ": ")) {
				indent = lead;
				out.push(line);
			}
			continue;
		}
		if (lead <= indent) break; // 回到同级或更浅 = 本段结束
		out.push(line);
	}
	return out.join("\n");
}

function yamlWithoutSection(text, key) {
	const out = [];
	let skip = -1;
	for (const line of String(text || "").split("\n")) {
		const lead = line.length - line.trimStart().length;
		const head = line.trim();
		if (skip >= 0) {
			if (head === "" || lead > skip) continue;
			skip = -1;
		}
		if (head === key + ":" || head.startsWith(key + ": ")) {
			skip = lead;
			continue;
		}
		out.push(line);
	}
	return out.join("\n");
}

function yamlWithoutLine(text, prefix) {
	return String(text || "")
		.split("\n")
		.filter((line) => !line.trimStart().startsWith(prefix))
		.join("\n");
}

function namespaceDocShell(text) {
	return yamlWithoutLine(yamlWithoutSection(text, "spec"), "resourceVersion:");
}

/* 超时现场：表格里现在有哪些 key（带 deleted/editing/missing 标记）、保存按钮是否 enabled、状态文案。 */
async function varsFacts(page) {
	return page.evaluate(() => {
		const rows = (id) => {
			const host = document.getElementById(id);
			if (!host) return null;
			return Array.from(host.querySelectorAll("tbody tr")).map((tr) => {
				const flags = ["deleted", "editing", "missing"].filter((k) => tr.dataset[k] === "true");
				return (tr.dataset.key || "") + (flags.length ? ":" + flags.join("+") : "");
			});
		};
		const button = (id) => {
			const el = document.getElementById(id);
			return el ? { disabled: el.disabled === true, label: el.textContent.trim() } : null;
		};
		const state = (id) => {
			const el = document.getElementById(id);
			return el ? el.textContent.trim() : "";
		};
		return {
			nodeRows: rows("node-vars-table"),
			namespaceRows: rows("namespace-effective-vars"),
			nodeSave: button("node-vars-save"),
			namespaceSave: button("namespace-vars-save"),
			nodeState: state("node-vars-state"),
			namespaceState: state("namespace-vars-state"),
		};
	});
}

/* 点保存并等**真的** PUT …/yaml 回执（不是等本地行状态变化 —— 那只证明点了按钮）。等不到时把现场拼进错误：一次 timeout 三个字什么都说明不了（#544）。 */
async function saveAndWait(page, saveSelector, pathSuffix) {
	const before = await page.evaluate((sel) => {
		const el = document.querySelector(sel);
		return el ? { found: true, disabled: el.disabled === true } : { found: false };
	}, saveSelector);
	const writes = []; // 浏览器真正发出的写请求：点禁用按钮、handler 提前 return 都不发
	const onRequest = (req) => {
		if (req.method() !== "GET") writes.push(req.method() + " " + req.url());
	};
	page.on("request", onRequest);
	try {
		const [response] = await Promise.all([
			page.waitForResponse((resp) => resp.request().method() === "PUT" && new URL(resp.url()).pathname.endsWith(pathSuffix), { timeout: 15000 }),
			page.locator(saveSelector).click(),
		]);
		if (response.status() !== 200) {
			throw new Error(`保存失败：HTTP ${response.status()} ${await response.text()}`);
		}
		return response;
	} catch (e) {
		let facts;
		try {
			facts = await varsFacts(page);
		} catch (inner) {
			facts = { unreadable: String(inner) };
		}
		const nsSide = saveSelector.includes("namespace");
		const rows = nsSide ? facts.namespaceRows : facts.nodeRows;
		const button = nsSide ? facts.namespaceSave : facts.nodeSave;
		const state = nsSide ? facts.namespaceState : facts.nodeState;
		console.error(`--- 保存超时现场（${pathSuffix} · ${saveSelector}）---\n${JSON.stringify(facts, null, 1)}`); // 全量落 stderr（runner 印在失败套件输出里）；抛出去的只留结论（受 harness 300 字截断）
		throw new Error(
			`等不到写 ${pathSuffix}：${e.message}\n` +
				`按钮(点击前)=${JSON.stringify(before)} 按钮(现在)=${JSON.stringify(button)} 点击后写请求=${JSON.stringify(writes)} 表格=${JSON.stringify(rows)} 状态=${JSON.stringify(state)}`,
		);
	} finally {
		page.off("request", onRequest);
	}
}

async function main() {
	H.startTimer();
	console.log("=== SUITE: vars_ui（#476 可视化 vars：只读表 + YAML 唯一写入口）===");
	server = await H.startServer({ suiteName: "vars_ui", rpId: "localhost" });
	const s = H.seed(server.dbPath, "varsui");

	/* ---------- 造数：两个能力都要 SOURCE_ROOT，其中 build 还要 JAVA_HOME ---------- */
	const buildCap = await V.mkCapability(server.base, s.session, "build", {
		body: V.capabilityBody({ name: "build", requires: { values: ["vars.SOURCE_ROOT", "vars.JAVA_HOME"] } }),
	});
	const deployCap = await V.mkCapability(server.base, s.session, "deploy", {
		body: V.capabilityBody({ name: "deploy", requires: { values: ["vars.SOURCE_ROOT"] } }),
	});
	H.check("造数：build 要求 SOURCE_ROOT + JAVA_HOME", buildCap.name === "build");
	H.check("造数：deploy 要求 SOURCE_ROOT", deployCap.name === "deploy");

	// 节点默认值：SOURCE_ROOT 由节点层提供；NODE_ONLY 只有节点层有。
	const created = await H.req(server.base, V.P.nodes(), {
		session: s.session,
		body: {
			apiVersion: V.API_VERSION, kind: "Node",
			metadata: { name: NODE },
			spec: {
				namespaces: [NS],
				vars: { SOURCE_ROOT: "/from-node", NODE_ONLY: "node-value" },
				limits: { total: 4, perCapability: { "*": 2 } },
				tags: ["gpu", "prod"],
			},
		},
	});
	V.expectStatus("登记节点（带 spec.vars / limits / tags）", created, 201);

	// 命名空间默认值：NS_ONLY 只有 ns 层有。走真实 API（页面写的是同一份事实）。
	const nsCfg = await H.req(server.base, V.P.namespaceConfig(NS), { session: s.session });
	V.expectStatus("读命名空间 config（CAS 令牌）", nsCfg, 200);
	const nsPut = await H.req(server.base, V.P.namespaceConfig(NS), {
		session: s.session,
		method: "PUT",
		body: {
			metadata: { resourceVersion: nsCfg.json?.metadata?.resourceVersion },
			vars: { NS_ONLY: "ns-value" },
		},
	});
	V.expectStatus("写命名空间默认 vars", nsPut, 200);

	// 服务端真相：页面必须与它逐键一致（这是 ① 的判据，不是页面自证）。
	const nodeRead = await H.req(server.base, V.P.node(NODE) + "?namespace=" + NS, { session: s.session });
	V.expectStatus("读节点有效值（?namespace=）", nodeRead, 200);
	const effective = nodeRead.json?.status?.effectiveVars || {};
	H.eq("服务端 effectiveVars 有 3 个键（SOURCE_ROOT / NODE_ONLY / NS_ONLY）", Object.keys(effective).sort().join(","), "NODE_ONLY,NS_ONLY,SOURCE_ROOT");
	H.eq("SOURCE_ROOT 来自节点层", effective.SOURCE_ROOT?.source, "node");
	H.eq("NS_ONLY 来自命名空间层", effective.NS_ONLY?.source, "namespace");

	browser = await chromium.launch({ channel: "chrome", headless: true, args: ["--no-sandbox", "--disable-dev-shm-usage"] });

	/* ================= 阶段 1：节点页只读表 ================= */
	console.log("--- 阶段 1：节点页 vars 表（值 / 来源 / 谁要 / 缺）---");
	{
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		await primeScope(ctx);
		await injectSession(ctx, s.session, server.base);
		const page = await ctx.newPage();
		const errs = [];
		page.on("pageerror", (e) => errs.push(String(e)));
		const loads = [];
		page.on("request", (req) => { if (req.method() !== "GET") loads.push(req.method() + " " + req.url()); });
		await page.goto(server.base + "/node.html?id=" + NODE, { waitUntil: "load", timeout: 15000 });
		await H.waitForAppReady(page, "workspace");
		await page.locator('#node-vars-table tr[data-key="SOURCE_ROOT"]').waitFor({ timeout: 10000 });

		const rows = await readRows(page, "node-vars-table");
		H.check("节点页表已渲染（4 行：3 个有值 + 1 个被要求但缺）", rows && rows.length === 4, JSON.stringify(rows));
		const byName = Object.fromEntries((rows || []).map((r) => [r.name, r]));

		// ① 值与来源逐键对照服务端 effectiveVars。
		for (const name of ["SOURCE_ROOT", "NODE_ONLY", "NS_ONLY"]) {
			H.eq(name + " 的值 = effectiveVars.value", byName[name]?.value, effective[name]?.value);
			H.eq(name + " 的来源 = effectiveVars.source", byName[name]?.source, effective[name]?.source);
			H.eq(name + " 不是红标（有生效值）", byName[name]?.missing, false);
		}
		// ② 需求侧：SOURCE_ROOT 被两个能力要求（build / deploy）。
		H.check("SOURCE_ROOT 的需求列 = 2 个能力", /2/.test(byName.SOURCE_ROOT?.used || ""), byName.SOURCE_ROOT?.used);
		H.check("SOURCE_ROOT 的需求列点名 build 与 deploy",
			(byName.SOURCE_ROOT?.used || "").includes("build@1") && (byName.SOURCE_ROOT?.used || "").includes("deploy@1"),
			byName.SOURCE_ROOT?.used);
		H.check("NODE_ONLY 没有被任何能力要求", (byName.NODE_ONLY?.used || "").trim() === "—", byName.NODE_ONLY?.used);

		// ③ 缺但被要求 → 红标（这就是变异 ② 要打掉的那一条）。
		H.eq("JAVA_HOME 有红标（data-missing=true）", byName.JAVA_HOME?.missing, true);
		H.check("JAVA_HOME 的值列显示缺失", ["(missing)", "（缺）"].includes(byName.JAVA_HOME?.value), byName.JAVA_HOME?.value);
		H.check("JAVA_HOME 的需求列点名 build@1（只有 build 要它）", (byName.JAVA_HOME?.used || "").includes("build@1"), byName.JAVA_HOME?.used);
		const badge = await page.locator('#node-vars-table tr[data-key="JAVA_HOME"] .badge-error').count();
		H.check("JAVA_HOME 行有 ⚠ 必填徽章（.badge-error）", badge >= 1, "count=" + badge);
		const missingNote = await page.locator("#node-vars-missing").innerText();
		H.check("红标汇总文案说明有 1 个缺口", /1/.test(missingNote), missingNote);

		// 只读加载不该发出任何写请求。
		H.check("阶段 1 加载零写请求", loads.length === 0, JSON.stringify(loads));
		H.check("节点页无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 160));
		await page.close();
		await ctx.close();
	}

	/* ================= 阶段 1：命名空间页只读表 ================= */
	console.log("--- 阶段 1：命名空间页 vars 表 ---");
	{
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		await primeScope(ctx);
		await injectSession(ctx, s.session, server.base);
		const page = await ctx.newPage();
		const errs = [];
		page.on("pageerror", (e) => errs.push(String(e)));
		await page.goto(server.base + "/namespace.html?name=" + NS, { waitUntil: "load", timeout: 15000 });
		await H.waitForAppReady(page, "workspace");
		await page.selectOption("#namespace-node", NODE);
		await page.locator('#namespace-effective-vars tr[data-key="SOURCE_ROOT"]').waitFor({ timeout: 10000 });

		const rows = await readRows(page, "namespace-effective-vars");
		const byName = Object.fromEntries((rows || []).map((r) => [r.name, r]));
		H.check("命名空间页表渲染了同一批行", rows && rows.length === 4, JSON.stringify(rows));
		H.eq("命名空间页 SOURCE_ROOT 值与 effectiveVars 一致", byName.SOURCE_ROOT?.value, effective.SOURCE_ROOT?.value);
		H.eq("命名空间页 SOURCE_ROOT 来源 = node", byName.SOURCE_ROOT?.source, "node");
		H.eq("命名空间页 NS_ONLY 来源 = namespace", byName.NS_ONLY?.source, "namespace");
		H.eq("命名空间页 JAVA_HOME 红标", byName.JAVA_HOME?.missing, true);
		const capsText = byName.SOURCE_ROOT?.used || "";
		H.check("命名空间页需求列同样点名 build@1 / deploy@1", capsText.includes("build@1") && capsText.includes("deploy@1"), capsText);
		H.check("命名空间页无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 160));
		await page.close();
		await ctx.close();
	}

	/* ================= 阶段 2：节点页改 / 删 / 增（真 PUT …/yaml） ================= */
	console.log("--- 阶段 2：节点页表格保存 → PUT …/yaml ---");
	const nodeYamlPath = V.P.node(NODE) + "/yaml";
	const beforeNodeYAML = (await H.req(server.base, nodeYamlPath, { session: s.session })).json?.yaml || "";
	H.check("节点 YAML 读面可用", beforeNodeYAML.includes("kind: Node"), beforeNodeYAML.slice(0, 120));
	const nodeBefore = {
		tags: yamlSection(beforeNodeYAML, "tags"),
		limits: yamlSection(beforeNodeYAML, "limits"),
		namespaces: yamlSection(beforeNodeYAML, "namespaces"),
	};
	H.check("造数：节点文档改前含 tags / limits / namespaces 三段",
		!!nodeBefore.tags && !!nodeBefore.limits && !!nodeBefore.namespaces,
		JSON.stringify(nodeBefore));
	H.check('造数：limits 段是 total: 4 + perCapability 的 "*" 档',
		/total: 4/.test(nodeBefore.limits) && /\*'?: 2/.test(nodeBefore.limits),
		nodeBefore.limits);
	H.check("造数：tags 段是 gpu / prod",
		/- gpu/.test(nodeBefore.tags) && /- prod/.test(nodeBefore.tags),
		nodeBefore.tags);
	{
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		await primeScope(ctx);
		await injectSession(ctx, s.session, server.base);
		const page = await ctx.newPage();
		const errs = [];
		page.on("pageerror", (e) => errs.push(String(e)));
		const writes = [];
		page.on("request", (req) => { if (req.method() !== "GET") writes.push(req.method() + " " + req.url()); });
		await page.goto(server.base + "/node.html?id=" + NODE, { waitUntil: "load", timeout: 15000 });
		await H.waitForAppReady(page, "workspace");
		await page.locator('#node-vars-table tr[data-key="NODE_ONLY"] [data-act="vars-edit"]').waitFor({ timeout: 10000 });

		// —— 改一个 var：NODE_ONLY: node-value → node-value-v2
		await page.locator('#node-vars-table tr[data-key="NODE_ONLY"] [data-act="vars-edit"]').click();
		await page.locator('#node-vars-table tr[data-editing="true"] input[data-field="value"]').fill("node-value-v2");
		await page.locator('#node-vars-table [data-act="vars-apply"]').click();
		await saveAndWait(page, "#node-vars-save", "/yaml");
		await page.waitForFunction(() => {
			const cell = document.querySelector('#node-vars-table tr[data-key="NODE_ONLY"] td.vars-value');
			return cell && cell.textContent.trim() === "node-value-v2";
		}, null, { timeout: 10000 });
		const afterNodeYAML = (await H.req(server.base, nodeYamlPath, { session: s.session })).json?.yaml || "";
		const nodeAfter = {
			tags: yamlSection(afterNodeYAML, "tags"),
			limits: yamlSection(afterNodeYAML, "limits"),
			namespaces: yamlSection(afterNodeYAML, "namespaces"),
		};
		H.check("保真：表格保存后 tags 段逐字不变（gpu / prod 都还在）",
			nodeAfter.tags === nodeBefore.tags,
			JSON.stringify({ before: nodeBefore.tags, after: nodeAfter.tags }));
		H.check("保真：表格保存后 limits 段逐字不变（total: 4 与 perCapability 都在）",
			nodeAfter.limits === nodeBefore.limits,
			JSON.stringify({ before: nodeBefore.limits, after: nodeAfter.limits }));
		H.check("保真：表格保存后授权 namespaces 段逐字不变",
			nodeAfter.namespaces === nodeBefore.namespaces,
			JSON.stringify({ before: nodeBefore.namespaces, after: nodeAfter.namespaces }));
		const changeDiff = lineDiff(beforeNodeYAML, afterNodeYAML);
		H.check("改一个 var：YAML 里只有该键 + resourceVersion 变化",
			changeDiff.removed.length === 2 && changeDiff.added.length === 2,
			JSON.stringify(changeDiff));
		H.eq("被删的旧行是 NODE_ONLY", onlyVarLine(changeDiff.removed, "NODE_ONLY").length, 1);
		H.eq("新增的新行是 NODE_ONLY", onlyVarLine(changeDiff.added, "NODE_ONLY").length, 1);
		H.check("其它行逐字不变：SOURCE_ROOT 在两个版本里逐字相同",
			yamlLine(beforeNodeYAML, "SOURCE_ROOT") !== "" && yamlLine(beforeNodeYAML, "SOURCE_ROOT") === yamlLine(afterNodeYAML, "SOURCE_ROOT"),
			JSON.stringify([yamlLine(beforeNodeYAML, "SOURCE_ROOT"), yamlLine(afterNodeYAML, "SOURCE_ROOT")]));
		H.check("删掉的不是别人的键：NS_ONLY 不在节点文档里（层不同）",
			!beforeNodeYAML.includes("NS_ONLY") && !afterNodeYAML.includes("NS_ONLY"), afterNodeYAML);
		H.check("保存请求只打到 …/yaml（没有 /config 写）",
			writes.length >= 1 && writes.every((w) => /\/v1\/nodes\/[^/]+\/yaml$/.test(w.split(" ")[1])),
			JSON.stringify(writes));

		// —— 删一个 var：SOURCE_ROOT 从节点默认层消失
		await page.locator('#node-vars-table tr[data-key="SOURCE_ROOT"] [data-act="vars-delete"]').click();
		await saveAndWait(page, "#node-vars-save", "/yaml");
		await page.waitForFunction(() => {
			const tr = document.querySelector('#node-vars-table tr[data-key="SOURCE_ROOT"]');
			return tr && tr.dataset.missing === "true";
		}, null, { timeout: 10000 });
		const afterDeleteYAML = (await H.req(server.base, nodeYamlPath, { session: s.session })).json?.yaml || "";
		const deleteDiff = lineDiff(afterNodeYAML, afterDeleteYAML);
		H.check("删一个 var：YAML 只少了那一行（+resourceVersion）",
			onlyVarLine(deleteDiff.removed, "SOURCE_ROOT").length === 1 && onlyVarLine(deleteDiff.added, "SOURCE_ROOT").length === 0,
			JSON.stringify(deleteDiff));
		H.check("删除不影响同文档其它键（NODE_ONLY 仍是改后的值）",
			!afterDeleteYAML.includes("SOURCE_ROOT") && yamlLine(afterDeleteYAML, "NODE_ONLY").includes("node-value-v2"),
			afterDeleteYAML);
		H.check("节点页无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 160));
		await page.close();
		await ctx.close();
	}

	/* ================= 阶段 2：命名空间页改 / 增 + 名字校验 ================= */
	console.log("--- 阶段 2：命名空间页表格保存 → PUT …/yaml ---");
	const nsYamlPath = V.P.namespace(NS) + "/yaml";
	const beforeNSYAML = (await H.req(server.base, nsYamlPath, { session: s.session })).json?.yaml || "";
	const nsBeforeShell = namespaceDocShell(beforeNSYAML);
	H.check("造数：命名空间文档壳含 apiVersion / kind / metadata.name",
		nsBeforeShell.includes("apiVersion: anotify/v1") && nsBeforeShell.includes("kind: Namespace") && nsBeforeShell.includes("name: " + NS),
		nsBeforeShell);
	{
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		await primeScope(ctx);
		await injectSession(ctx, s.session, server.base);
		const page = await ctx.newPage();
		const errs = [];
		page.on("pageerror", (e) => errs.push(String(e)));
		const writes = [];
		page.on("request", (req) => { if (req.method() !== "GET") writes.push(req.method() + " " + req.url()); });
		await page.goto(server.base + "/namespace.html?name=" + NS, { waitUntil: "load", timeout: 15000 });
		await H.waitForAppReady(page, "workspace");
		await page.selectOption("#namespace-node", NODE);
		await page.locator('#namespace-effective-vars tr[data-key="NS_ONLY"] [data-act="vars-edit"]').waitFor({ timeout: 10000 });

		// —— 改命名空间默认值：NS_ONLY: ns-value → ns-value-v2
		await page.locator('#namespace-effective-vars tr[data-key="NS_ONLY"] [data-act="vars-edit"]').click();
		await page.locator('#namespace-effective-vars tr[data-editing="true"] input[data-field="value"]').fill("ns-value-v2");
		await page.locator('#namespace-effective-vars [data-act="vars-apply"]').click();
		await saveAndWait(page, "#namespace-vars-save", "/yaml");
		await page.waitForFunction(() => {
			const cell = document.querySelector('#namespace-effective-vars tr[data-key="NS_ONLY"] td.vars-value');
			return cell && cell.textContent.trim() === "ns-value-v2";
		}, null, { timeout: 10000 });
		const afterNSYAML = (await H.req(server.base, nsYamlPath, { session: s.session })).json?.yaml || "";
		H.check("保真：命名空间文档除 spec.vars 与 resourceVersion 外的行逐字不变",
			namespaceDocShell(afterNSYAML) === nsBeforeShell,
			JSON.stringify({ before: nsBeforeShell, after: namespaceDocShell(afterNSYAML) }));
		const changeDiff = lineDiff(beforeNSYAML, afterNSYAML);
		H.check("命名空间改一个 var：YAML 只有该键 + resourceVersion 变化",
			onlyVarLine(changeDiff.removed, "NS_ONLY").length === 1 && onlyVarLine(changeDiff.added, "NS_ONLY").length === 1,
			JSON.stringify(changeDiff));
		H.check("命名空间文档的 apiVersion / kind / metadata.name 逐字不变",
			afterNSYAML.includes("apiVersion: anotify/v1") && afterNSYAML.includes("kind: Namespace") && afterNSYAML.includes("name: " + NS),
			afterNSYAML);
		H.check("命名空间保存请求只打到 …/yaml（没有 /config 写）",
			writes.length >= 1 && writes.every((w) => /\/v1\/namespaces\/[^/]+\/yaml$/.test(w.split(" ")[1])),
			JSON.stringify(writes));

		// —— 非法名字：前端即时拒（apply 禁用 + 说明文案）
		await page.locator("#namespace-vars-add").click();
		await page.locator('#namespace-effective-vars tr[data-editing="true"] input[data-field="name"]').fill("1FOO");
		await page.locator('#namespace-effective-vars tr[data-editing="true"] input[data-field="value"]').fill("x");
		H.check("1FOO：前端即时禁用「确定」", await page.locator('#namespace-effective-vars [data-act="vars-apply"]').isDisabled(), "apply 仍可点");
		const shapeHint = await page.locator('#namespace-effective-vars tr[data-editing="true"] .vars-hint').innerText();
		H.check("1FOO：给出名字形状说明", shapeHint.includes("A-Za-z_"), shapeHint);
		await page.locator('#namespace-effective-vars tr[data-editing="true"] input[data-field="name"]').fill("FOO-BAR");
		H.check("FOO-BAR：前端即时禁用「确定」", await page.locator('#namespace-effective-vars [data-act="vars-apply"]').isDisabled(), "apply 仍可点");
		await page.locator('#namespace-effective-vars [data-act="vars-cancel"]').click();

		// —— 后端也必须拒：绕过页面直接 PUT 一份含非法键的 YAML → 400 value_invalid
		const freshNs = await H.req(server.base, V.P.namespaceConfig(NS), { session: s.session });
		V.expectStatus("读命名空间 config（新鲜 CAS 令牌）", freshNs, 200);
		const badYAML = [
			"apiVersion: anotify/v1", "kind: Namespace", "metadata:", "  name: " + NS,
			"  resourceVersion: " + freshNs.json.metadata.resourceVersion,
			"spec:", "  vars:", '    "1FOO": x', "",
		].join("\n");
		const badPut = await H.req(server.base, nsYamlPath, { session: s.session, method: "PUT", body: { yaml: badYAML } });
		H.eq("后端对非法名字 → 400", badPut.status, 400);
		H.eq("后端拒绝原因 = value_invalid", badPut.json?.reason, "value_invalid");
		H.check("后端拒绝消息点名非法键", String(badPut.json?.message || "").includes("1FOO"), badPut.text.slice(0, 200));

		// —— 新增一个「被要求但缺失」的名字（JAVA_HOME）：填上后红标消失
		await page.locator('#namespace-effective-vars tr[data-key="JAVA_HOME"] [data-act="vars-fill"]').click();
		await page.locator('#namespace-effective-vars tr[data-editing="true"] input[data-field="value"]').fill("/usr/lib/jvm/e2e");
		await page.locator('#namespace-effective-vars [data-act="vars-apply"]').click();
		await saveAndWait(page, "#namespace-vars-save", "/yaml");
		await page.waitForFunction(() => {
			const tr = document.querySelector('#namespace-effective-vars tr[data-key="JAVA_HOME"]');
			return tr && tr.dataset.missing === "false";
		}, null, { timeout: 10000 });
		const filledYAML = (await H.req(server.base, nsYamlPath, { session: s.session })).json?.yaml || "";
		H.check("新增 JAVA_HOME 写进了命名空间文档", yamlLine(filledYAML, "JAVA_HOME").includes("/usr/lib/jvm/e2e"), filledYAML);
		H.check("新增只加一个键：NS_ONLY 仍是改后的值", yamlLine(filledYAML, "NS_ONLY").includes("ns-value-v2"), filledYAML);
		H.check("命名空间页无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 160));
		await page.close();
		await ctx.close();
	}

	console.log("--- 边界：total: null 的文档原样贴回 ---");
	const BARE = "anode-vars-bare";
	const bareCreated = await H.req(server.base, V.P.nodes(), {
		session: s.session,
		body: { apiVersion: V.API_VERSION, kind: "Node", metadata: { name: BARE }, spec: { namespaces: [NS] } },
	});
	V.expectStatus("登记 bare 节点（spec 不带 limits）", bareCreated, 201);
	const barePath = V.P.node(BARE) + "/yaml";
	const bareBefore = (await H.req(server.base, barePath, { session: s.session })).json?.yaml || "";
	H.check("边界前提：没有 limits.total 的节点，读面写成显式 total: null",
		bareBefore.includes("total: null"), bareBefore);
	const barePut = await H.req(server.base, barePath, { session: s.session, method: "PUT", body: { yaml: bareBefore } });
	H.eq("边界：把 total: null 的原文原样贴回 → 400", barePut.status, 400);
	H.eq("边界：拒绝原因 = value_invalid", barePut.json?.reason, "value_invalid");
	const bareAfter = (await H.req(server.base, barePath, { session: s.session })).json?.yaml || "";
	H.check("边界：被拒的写没有落库（文档逐字未变）", bareAfter === bareBefore,
		JSON.stringify({ before: bareBefore, after: bareAfter }));

	const passed = H.summary("vars_ui");
	await browser.close();
	server.stop();
	process.exit(passed ? 0 : 1);
}

main().catch(async (e) => {
	console.error(e);
	try {
		await browser?.close();
		server?.stop();
	} catch {
		/* 清理失败不掩盖原始错误 */
	}
	process.exit(1);
});
