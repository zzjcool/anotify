#!/usr/bin/env node
import * as H from "../lib/harness.mjs";
import * as V from "../lib/v4.mjs";

const SUITE = "playbook_release";
const NS = V.DEFAULT_NS;
const VALUES = { project: "demo" };
const UNINST_VALUES = { project: "uninst" };

const PB = {
	list: () => V.P.playbooks(NS),
	install: (slug) => `${V.P.playbook(NS, slug)}/install`,
	render: (slug) => `${V.P.playbook(NS, slug)}/render`,
	installs: (slug) => `${V.P.playbook(NS, slug)}/installs`,
	versions: (slug) => `${V.P.playbook(NS, slug)}/versions`,
	installById: (slug, id) => `${V.P.playbook(NS, slug)}/installs/${encodeURIComponent(id)}`,
	release: (slug, rel) => `${V.P.playbook(NS, slug)}/releases/${encodeURIComponent(rel)}`,
	releaseYAML: (slug, rel) =>
		`${V.P.playbook(NS, slug)}/releases/${encodeURIComponent(rel)}/yaml`,
};

const VALUES_BLOCK = `    type: object
    required: [project]
    properties:
      project: { type: string, pattern: '^[a-z0-9][a-z0-9-]{0,20}$' }`;

function requiresBlock(reqs) {
	if (reqs === null) return "";
	if (reqs.length === 0) return "  requires: []\n";
	const rows = reqs.map((r) =>
		Object.entries(r)
			.map(([k, v], i) => `${i === 0 ? "    - " : "      "}${k}: ${v}`)
			.join("\n"),
	);
	return `  requires:\n${rows.join("\n")}\n`;
}

function entryDoc(slug, caps, srcs, version = 0, requires = null) {
	const list = (key, paths) =>
		paths.length === 0
			? `  ${key}: []`
			: `  ${key}:\n${paths.map((p) => `    - ${p}`).join("\n")}`;
	const declared = version > 0 ? `  version: ${version}\n` : "";
	return `apiVersion: anotify/v1
kind: Playbook
metadata:
  name: ${slug}
  displayName: 安装 release 身份 E2E
${declared}spec:
  values:
${VALUES_BLOCK}
${list("capabilities", caps)}
${list("sources", srcs)}
${requiresBlock(requires)}`;
}

function capabilityDoc(name, runLines, next = []) {
	const lines = runLines && runLines.length ? runLines : ["#!/bin/sh", "echo ok"];
	return `apiVersion: anotify/v1
kind: Capability
metadata:
  name: '${name}'
spec:
  params:
    type: object
  next: [${next.map((n) => `'${n}'`).join(", ")}]
  run:
    entrypoint: run.sh
    files:
      run.sh: |
${lines.map((l) => `        ${l}`).join("\n")}
`;
}

function sourceDoc(name, targets) {
	return `apiVersion: anotify/v1
kind: Source
metadata:
  name: '${name}'
spec:
  kind: webhook
  enabled: true
  config: {}
  targets:
${targets
	.map(
		(t) => `    - capability: '${t}'
      params:
        repo: '{{values.project}}'`,
	)
	.join("\n")}
`;
}

function sourceDocSecret(name, targets) {
	return sourceDoc(name, targets).replace("config: {}", "config: { secret: 'e2e-hook-secret' }");
}

async function createPackage(ctx, slug, files) {
	const r = await H.req(ctx.base, PB.list(), {
		session: ctx.session,
		method: "POST",
		body: {
			apiVersion: V.API_VERSION,
			kind: "Playbook",
			metadata: { name: slug },
			spec: { files },
		},
	});
	V.expectStatus(`建包 ${slug} → 201`, r, 201);
	return r;
}

function install(ctx, slug, release, values = VALUES) {
	const body = { values };
	if (release !== undefined) body.release = release;
	return H.req(ctx.base, PB.install(slug), { session: ctx.session, method: "POST", body });
}

function render(ctx, slug, release, values = VALUES) {
	const body = { values };
	if (release !== undefined) body.release = release;
	return H.req(ctx.base, PB.render(slug), { session: ctx.session, method: "POST", body });
}

async function readInstalls(ctx, slug, what) {
	const r = await H.req(ctx.base, PB.installs(slug), { session: ctx.session });
	V.expectStatus(`${what}的实例清单 → 200`, r, 200);
	const groups = Array.isArray(r.json?.installs) ? r.json.installs : null;
	H.check(`${what}的实例清单 installs 是数组（空列表也要 []）`, groups !== null, JSON.stringify(r.json).slice(0, 200));
	return { total: r.json?.total, groups: groups || [] };
}

async function listNames(ctx, path, what) {
	const r = await H.req(ctx.base, path, { session: ctx.session });
	V.expectStatus(`${what}列表 → 200`, r, 200);
	const names = V.itemNames(r);
	H.check(`${what}列表 items 是数组（不是 null）`, names !== null, JSON.stringify(r.json).slice(0, 200));
	return names || [];
}

function rowCounts(ctx) {
	return {
		capability_versions: V.countRows(ctx.dbPath, "capability_versions"),
		sources: V.countRows(ctx.dbPath, "sources"),
	};
}

function uninstall(ctx, slug, rel, query = "") {
	const path = PB.release(slug, rel) + (query ? "?" + query : "");
	return H.req(ctx.base, path, { session: ctx.session, method: "DELETE" });
}

function dbRows(ctx, sql, args = []) {
	return V.withDB(ctx.dbPath, (db) => db.prepare(sql).all(...args));
}

function dbFingerprint(dbPath) {
	return V.withDB(dbPath, (db) =>
		JSON.stringify({
			capability_versions: db.prepare("SELECT * FROM capability_versions ORDER BY id").all(),
			sources: db.prepare("SELECT * FROM sources ORDER BY id").all(),
			edges: db.prepare("SELECT * FROM edges ORDER BY namespace_id, from_is_source, from_ref, to_ref").all(),
			playbook_installs: db.prepare("SELECT * FROM playbook_installs ORDER BY id").all(),
			playbook_versions: db.prepare("SELECT * FROM playbook_versions ORDER BY playbook_id, version").all(),
			playbook_install_requires: db
				.prepare("SELECT * FROM playbook_install_requires ORDER BY namespace_id, install_id, capability")
				.all(),
		}),
	);
}

async function partLegacyDefaults(ctx) {
	console.log("--- 1. 老包不回归：不传 release 的两次安装 ---");
	const slug = "pb-legacy";
	await createPackage(ctx, slug, {
		"playbook.yaml": entryDoc(slug, ["capabilities/review.yaml"], ["sources/on-push.yaml"]),
		"capabilities/review.yaml": capabilityDoc("{{values.project}}-review", []),
		"sources/on-push.yaml": sourceDoc("{{values.project}}-on-push", ["{{values.project}}-review"]),
	});

	const first = await install(ctx, slug);
	V.expectStatus("第一次安装（不传 release）→ 200", first, 200);
	const r1 = first.json || {};
	H.eq("缺省 release 就是包名（install.release = slug）", r1.install?.release, slug);
	H.eq("第一次安装建出 1 个能力", r1.created?.capabilities, ["demo-review@1"]);
	H.eq("第一次安装建出 1 个源", r1.created?.sources, ["demo-on-push"]);

	const second = await install(ctx, slug);
	V.expectStatus("第二次安装（同一组 values）→ 200（幂等，不是 409）", second, 200);
	const r2 = second.json || {};
	H.eq("第二次 install.release 仍是包名", r2.install?.release, slug);
	H.eq("第二次 created 全空（能力）", r2.created?.capabilities, []);
	H.eq("第二次 created 全空（源）", r2.created?.sources, []);
	H.eq("第二次 skipped 与第一次 created 逐字相同（能力）", r2.skipped?.capabilities, r1.created?.capabilities);
	H.eq("第二次 skipped 与第一次 created 逐字相同（源）", r2.skipped?.sources, r1.created?.sources);
	H.check(
		"两次安装的回执 id 不同（每次安装都是一次独立审计）",
		typeof r1.install?.id === "string" && r1.install.id !== r2.install?.id,
		`${r1.install?.id} vs ${r2.install?.id}`,
	);

	const list = await readInstalls(ctx, slug, "老包");
	H.eq("同一 release + 同一 values 折叠成 1 组", list.total, 1);
	H.eq("折叠组数 = installs 数组长度", list.groups.length, 1);
	H.eq("折叠组的 release = 包名", list.groups[0]?.release, slug);
	H.eq("折叠组的 attempts = 2（同组装了两次）", list.groups[0]?.attempts, 2);
	H.eq("折叠组的成员就是那 1 个能力", list.groups[0]?.capabilities, ["demo-review@1"]);
	H.eq("折叠组的成员就是那 1 个源", list.groups[0]?.sources, ["demo-on-push"]);
	H.eq("折叠组的 lastInstallId 指向第二次", list.groups[0]?.lastInstallId, r2.install?.id);
}

async function partMultiRelease(ctx) {
	console.log("--- 2. 两个 release 各一套名字，真的并存 ---");
	const slug = "pb-multi";
	await createPackage(ctx, slug, {
		"playbook.yaml": entryDoc(slug, ["capabilities/review.yaml"], ["sources/on-push.yaml"]),
		"capabilities/review.yaml": capabilityDoc("{{release.name}}-{{values.project}}-review", []),
		"sources/on-push.yaml": sourceDoc("{{release.name}}-{{values.project}}-on-push", [
			"{{release.name}}-{{values.project}}-review",
		]),
	});

	const ra = await install(ctx, slug, "a");
	V.expectStatus("release a 安装 → 200", ra, 200);
	const rb = await install(ctx, slug, "b");
	V.expectStatus("release b 安装 → 200（同包同 values，不同 release）", rb, 200);
	H.eq("release a 的回执带自己的身份", ra.json?.install?.release, "a");
	H.eq("release b 的回执带自己的身份", rb.json?.install?.release, "b");
	H.eq("release a 建出自己的能力名", ra.json?.created?.capabilities, ["a-demo-review@1"]);
	H.eq("release b 建出自己的能力名（不是复用 a 的）", rb.json?.created?.capabilities, ["b-demo-review@1"]);
	H.eq("release a 建出自己的源名", ra.json?.created?.sources, ["a-demo-on-push"]);
	H.eq("release b 建出自己的源名", rb.json?.created?.sources, ["b-demo-on-push"]);

	const caps = await listNames(ctx, V.P.capabilities(NS), "能力");
	for (const want of ["a-demo-review", "b-demo-review"]) {
		H.check(`能力列表同时有 ${want}`, caps.includes(want), caps.join(","));
	}
	const srcs = await listNames(ctx, V.P.sources(NS), "源");
	for (const want of ["a-demo-on-push", "b-demo-on-push"]) {
		H.check(`源列表同时有 ${want}`, srcs.includes(want), srcs.join(","));
	}

	const list = await readInstalls(ctx, slug, "多实例包");
	H.eq("两个 release 折叠成 2 组（release 是分组键的一部分）", list.total, 2);
	const byRelease = new Map(list.groups.map((g) => [g.release, g]));
	for (const rel of ["a", "b"]) {
		const g = byRelease.get(rel);
		H.check(`实例清单里有 release ${rel} 的组`, !!g, JSON.stringify(list.groups.map((x) => x.release)));
		if (!g) continue;
		H.eq(`release ${rel} 的组成员带自己的前缀（能力）`, g.capabilities, [`${rel}-demo-review@1`]);
		H.eq(`release ${rel} 的组成员带自己的前缀（源）`, g.sources, [`${rel}-demo-on-push`]);
		H.eq(`release ${rel} 的 attempts = 1`, g.attempts, 1);
	}

	for (const [rel, res] of [["a", ra], ["b", rb]]) {
		const id = res.json?.install?.id;
		const one = await H.req(ctx.base, PB.installById(slug, id), { session: ctx.session });
		V.expectStatus(`release ${rel} 的单回执 → 200`, one, 200);
		H.eq(`release ${rel} 的单回执 install.release 对得上`, one.json?.install?.release, rel);
		H.eq(`release ${rel} 的单回执 install.id 对得上`, one.json?.install?.id, id);
	}
}

async function partCrossReleaseConflict(ctx) {
	console.log("--- 3. 跨 release 认领冲突（409 release_conflict）---");
	const slug = "pb-claim";
	await createPackage(ctx, slug, {
		"playbook.yaml": entryDoc(slug, ["capabilities/review.yaml"], ["sources/on-push.yaml"]),
		"capabilities/review.yaml": capabilityDoc("claim-{{values.project}}-review", []),
		"sources/on-push.yaml": sourceDoc("claim-{{values.project}}-on-push", ["claim-{{values.project}}-review"]),
	});

	const first = await install(ctx, slug, "a");
	V.expectStatus("release a 先装成功 → 200", first, 200);
	const ownerId = first.json?.install?.id;
	H.check("release a 的回执 id 非空（409 的 details 要指认它）", typeof ownerId === "string" && ownerId.length > 0, String(ownerId));

	const before = rowCounts(ctx);
	const beforeList = await readInstalls(ctx, slug, "冲突前");

	const conflict = await install(ctx, slug, "b");
	V.expectReason("release b 复用 release a 的行 → 409 release_conflict", conflict, 409, "release_conflict");
	const d = conflict.json?.details || {};
	H.eq("details.ownerRelease 指认装出该行的 release", d.ownerRelease, "a");
	H.eq("details.ownerInstallId 指向第一次安装的回执", d.ownerInstallId, ownerId);
	H.check("details.name 非空（要点名被认领的那一行）", typeof d.name === "string" && d.name.length > 0, JSON.stringify(d.name));
	H.check("details.kind 非空（能力还是源）", typeof d.kind === "string" && d.kind.length > 0, JSON.stringify(d.kind));
	H.check(
		"details.conflicts 逐条列出认领冲突（不是只有一条汇总）",
		Array.isArray(d.conflicts) && d.conflicts.length > 0,
		JSON.stringify(d.conflicts),
	);
	H.check(
		"details.hint 给出出路（换 release / 把 release 写进名字模板）",
		typeof d.hint === "string" && d.hint.includes("release"),
		JSON.stringify(d.hint),
	);

	const after = rowCounts(ctx);
	H.eq("409 之后能力行数不变（单事务回滚 ⇒ 0 写）", after.capability_versions, before.capability_versions);
	H.eq("409 之后源行数不变（0 写）", after.sources, before.sources);
	const afterList = await readInstalls(ctx, slug, "冲突后");
	H.eq("409 之后实例组数不变（不落回执）", afterList.total, beforeList.total);
	H.eq("剩下的组仍然属于 release a", afterList.groups[0]?.release, "a");
}

async function partTemplatePassthrough(ctx) {
	console.log("--- 4. 模板透传：{{payload.x}} / $(vars.REGION) / ${ENV} 原样保留 ---");
	const slug = "pb-tpl";
	const runLines = [
		"#!/bin/sh",
		"echo 'payload={{payload.x}} region=$(vars.REGION) env=${ENV}'",
		"echo 'release={{release.name}} project={{values.project}}'",
	];
	await createPackage(ctx, slug, {
		"playbook.yaml": entryDoc(slug, ["capabilities/tpl.yaml"], []),
		"capabilities/tpl.yaml": capabilityDoc("tpl-{{release.name}}-review", runLines),
	});

	const res = await render(ctx, slug, "a");
	V.expectStatus("带 release a 调 render → 200", res, 200);
	H.eq("render 回显本次 release 的 values", res.json?.values?.project, "demo");
	const files = res.json?.files || {};
	const body = files["capabilities/tpl.yaml"];
	H.check(
		"渲染结果里有能力文件（files 以包内相对路径为键）",
		typeof body === "string" && body.length > 0,
		JSON.stringify(Object.keys(files)),
	);
	if (typeof body === "string") {
		H.check("`{{payload.x}}` 原样透传（不被包层吃掉）", body.includes("{{payload.x}}"), body.slice(0, 400));
		H.check("`$(vars.REGION)` 原样透传", body.includes("$(vars.REGION)"), body.slice(0, 400));
		H.check("`${ENV}` 原样透传", body.includes("${ENV}"), body.slice(0, 400));
		H.check(
			"`{{release.name}}` 已替换成 a 且无残留",
			body.includes("tpl-a-review") && !body.includes("{{release.name}}"),
			body.slice(0, 400),
		);
		H.check("`{{values.project}}` 已替换（没有 values 残留）", !body.includes("{{values."), body.slice(0, 400));
	}
}

async function partInvalidRelease(ctx) {
	console.log("--- 5. release 形状错 → 400 invalid_playbook ---");
	const slug = "pb-badrel";
	await createPackage(ctx, slug, {
		"playbook.yaml": entryDoc(slug, ["capabilities/review.yaml"], ["sources/on-push.yaml"]),
		"capabilities/review.yaml": capabilityDoc("badrel-{{values.project}}-review", []),
		"sources/on-push.yaml": sourceDoc("badrel-{{values.project}}-on-push", ["badrel-{{values.project}}-review"]),
	});

	const before = rowCounts(ctx);
	const bads = [
		["大写字母", "Prod"],
		["含空格", "a b"],
		["超长（64 > 63）", "x".repeat(64)],
	];
	for (const [why, bad] of bads) {
		const r = await install(ctx, slug, bad);
		V.expectReason(`release ${why}（${JSON.stringify(bad).slice(0, 12)}…）→ 400`, r, 400, "invalid_playbook");
	}
	const after = rowCounts(ctx);
	H.eq("坏 release 之后能力行数不变（0 写）", after.capability_versions, before.capability_versions);
	H.eq("坏 release 之后源行数不变（0 写）", after.sources, before.sources);
	const list = await readInstalls(ctx, slug, "坏 release 包");
	H.eq("坏 release 不留回执（实例组数 0）", list.total, 0);
}

function pbVersionRows(ctx, slug) {
	return dbRows(
		ctx,
		`SELECT v.version, v.digest, v.created_at, v.created_by
		   FROM playbook_versions v
		   JOIN playbooks p ON p.id = v.playbook_id
		  WHERE p.slug = ? ORDER BY v.version`,
		[slug],
	);
}

// version 为空 = 不传，即老包行为。
function installPinned(ctx, slug, release, version, values = VALUES) {
	const body = { values };
	if (release !== undefined) body.release = release;
	if (version !== undefined) body.version = version;
	return H.req(ctx.base, PB.install(slug), { session: ctx.session, method: "POST", body });
}

// capabilityRunBody 读某个能力行落库的脚本正文（「装的是哪一版」的实物判据）。
function capabilityRunBody(ctx, name) {
	const rows = dbRows(ctx, "SELECT spec FROM capability_versions WHERE name = ?", [name]);
	if (rows.length === 0) return null;
	const spec = JSON.parse(rows[0].spec || "{}");
	return spec?.run?.files?.["run.sh"] ?? null;
}

async function readVersions(ctx, slug, what) {
	const r = await H.req(ctx.base, PB.versions(slug), { session: ctx.session });
	V.expectStatus(`${what}的版本历史 → 200`, r, 200);
	const list = Array.isArray(r.json?.versions) ? r.json.versions : null;
	H.check(`${what}的版本历史 versions 是数组（空列表也要 []）`, list !== null, JSON.stringify(r.json).slice(0, 200));
	return { total: r.json?.total, currentVersion: r.json?.currentVersion, versions: list || [] };
}

async function putPackage(ctx, slug, files, why) {
	const got = await H.req(ctx.base, `${V.P.playbook(NS, slug)}?includeBody=true`, { session: ctx.session });
	V.expectStatus(`${why}：读取当前版本令牌 → 200`, got, 200);
	const rv = got.json?.metadata?.resourceVersion;
	H.check(`${why}：resourceVersion 非空`, typeof rv === "string" && rv.length > 0, String(rv));
	const r = await H.req(ctx.base, V.P.playbook(NS, slug), {
		session: ctx.session,
		method: "PUT",
		body: { apiVersion: V.API_VERSION, kind: "Playbook", metadata: { name: slug, resourceVersion: rv }, spec: { files } },
	});
	V.expectStatus(`${why}：替换包体 → 200`, r, 200);
	return r;
}

async function partVersions(ctx) {
	console.log("--- 10. 包版本历史 + 按版本安装（#511）---");
	const slug = "pb-versions";
	// 名字带 release：三份实例各自一行（release va/vb/vc 是本段专有，不与其他段撞名）。
	const names = {
		capability: "{{release.name}}-{{values.project}}-review",
		source: "{{release.name}}-{{values.project}}-on-push",
	};
	const bundle = (version, script) => ({
		"playbook.yaml": entryDoc(slug, ["capabilities/review.yaml"], ["sources/on-push.yaml"], version),
		"capabilities/review.yaml": capabilityDoc(names.capability, ["#!/bin/sh", script]),
		"sources/on-push.yaml": sourceDoc(names.source, [names.capability]),
	});
	const byVersion = (list, version) => (list.versions || []).find((v) => v.version === version) || {};

	await createPackage(ctx, slug, bundle(1, "echo v1"));

	let list = await readVersions(ctx, slug, "建包后");
	H.eq("建包后历史行数 = 1", list.total, 1);
	H.eq("建包后当前版本 = 1", list.currentVersion, 1);
	const v1 = byVersion(list, 1);
	H.check("历史行带 current = true", v1.current === true, JSON.stringify(v1));
	H.check(
		"历史行的 filesDigest 是 sha256（与回执同一个算法）",
		typeof v1.filesDigest === "string" && v1.filesDigest.startsWith("sha256:"),
		JSON.stringify(v1.filesDigest),
	);
	const v1Digest = v1.filesDigest;

	// 老包不回归：不传 version = 装当前版本。
	const first = await installPinned(ctx, slug, "va", undefined);
	V.expectStatus("按当前版本装 release va → 200", first, 200);
	H.eq("回执 playbook.version = 当前版本", first.json?.playbook?.version, 1);
	H.eq("回执 playbook.filesDigest = v1 的指纹", first.json?.playbook?.filesDigest, v1Digest);
	H.eq("release va 装出 v1 的能力", first.json?.created?.capabilities, ["va-demo-review@1"]);
	H.check(
		"release va 的能力行脚本正文是 v1",
		(capabilityRunBody(ctx, "va-demo-review") || "").includes("echo v1"),
		String(capabilityRunBody(ctx, "va-demo-review")),
	);

	await putPackage(ctx, slug, bundle(2, "echo v2"), "改包成 v2");
	list = await readVersions(ctx, slug, "改包后");
	H.eq("内容变了之后历史行数 = 2", list.total, 2);
	H.eq("当前版本 = 2", list.currentVersion, 2);
	const v1After = byVersion(list, 1);
	const v2 = byVersion(list, 2);
	H.eq("v1 那一行的指纹没被改写", v1After.filesDigest, v1Digest);
	const v2Digest = v2.filesDigest;
	H.check("v2 的指纹与 v1 不同", typeof v2Digest === "string" && v2Digest !== v1Digest, `${v1Digest} vs ${v2Digest}`);
	H.check("只有 v2 那一行带 current", v2.current === true && v1After.current === false, JSON.stringify(list.versions));

	// 按 v1 再装另一份 release：v1/v2 两份实例并存，各自拿到自己那一版。
	const pinned = await installPinned(ctx, slug, "vb", 1);
	V.expectStatus("按 v1 装 release vb → 200", pinned, 200);
	H.eq("按 v1 装的回执 playbook.version = 1", pinned.json?.playbook?.version, 1);
	H.eq("按 v1 装的回执 filesDigest = v1 指纹", pinned.json?.playbook?.filesDigest, v1Digest);
	H.eq("release vb 装出自己的能力（不是复用 va 的）", pinned.json?.created?.capabilities, ["vb-demo-review@1"]);
	H.check(
		"release vb 的能力行脚本正文是 v1（按版本装拿到了旧那一版）",
		(capabilityRunBody(ctx, "vb-demo-review") || "").includes("echo v1"),
		String(capabilityRunBody(ctx, "vb-demo-review")),
	);

	const fresh = await installPinned(ctx, slug, "vc", undefined);
	V.expectStatus("不传版本再装 release vc → 200", fresh, 200);
	H.eq("不传版本装的是当前版本 2", fresh.json?.playbook?.version, 2);
	H.eq("不传版本的回执 filesDigest = v2 指纹", fresh.json?.playbook?.filesDigest, v2Digest);
	H.check(
		"release vc 的能力行脚本正文是 v2",
		(capabilityRunBody(ctx, "vc-demo-review") || "").includes("echo v2"),
		String(capabilityRunBody(ctx, "vc-demo-review")),
	);
	H.check(
		"两份回执的包版本与文件指纹都不同（可追溯）",
		pinned.json?.playbook?.version !== fresh.json?.playbook?.version &&
			pinned.json?.playbook?.filesDigest !== fresh.json?.playbook?.filesDigest,
		JSON.stringify([pinned.json?.playbook, fresh.json?.playbook]),
	);

	// 单回执 GET 也能看出装的是哪一版。
	const one = await H.req(ctx.base, PB.installById(slug, pinned.json?.install?.id), { session: ctx.session });
	V.expectStatus("按 v1 装的单回执 → 200", one, 200);
	H.eq("单回执 playbook.version = 1", one.json?.playbook?.version, 1);
	H.eq("单回执 playbook.filesDigest = v1 指纹", one.json?.playbook?.filesDigest, v1Digest);

	// 内容相同的重复 PUT 不追加历史行，也不重写已有那一行（createdAt 是写入时刻，不是「最后保存时刻」）。
	const before = pbVersionRows(ctx, slug).length;
	const stampBefore = byVersion(await readVersions(ctx, slug, "重复保存前"), 2).createdAt;
	await new Promise((resolve) => setTimeout(resolve, 1100));
	await putPackage(ctx, slug, bundle(2, "echo v2"), "同内容重复保存");
	const after = pbVersionRows(ctx, slug).length;
	H.eq("内容相同的重复保存不追加历史行（行数不变）", after, before);
	const again = await readVersions(ctx, slug, "同内容保存后");
	H.eq("同内容保存后仍只有 2 版", again.total, 2);
	H.eq("同内容保存不重写历史行（createdAt 不变）", byVersion(again, 2).createdAt, stampBefore);

	// 版本不存在的错误面：404 + 0 写。
	const capsBefore = V.countRows(ctx.dbPath, "capability_versions");
	const missing = await installPinned(ctx, slug, "vd", 9);
	V.expectReason("按不存在的版本装 → 404 version_not_found", missing, 404, "version_not_found");
	H.eq("版本不存在的安装 0 写（能力行数不变）", V.countRows(ctx.dbPath, "capability_versions"), capsBefore);
	const zero = await installPinned(ctx, slug, "vd", 0);
	V.expectReason("version=0 → 400 invalid_playbook", zero, 400, "invalid_playbook");
	const ghostRender = await H.req(ctx.base, PB.render(slug), {
		session: ctx.session,
		method: "POST",
		body: { values: VALUES, version: 9 },
	});
	V.expectReason("渲染不存在的版本 → 404 version_not_found", ghostRender, 404, "version_not_found");
}

async function partUninstallRemovesReleaseOnly(ctx) {
	console.log("--- 6. 卸载一个 release 只撤它装出的行（#510）---");
	const slug = "pb-uninstall";
	await createPackage(ctx, slug, {
		"playbook.yaml": entryDoc(slug, ["capabilities/review.yaml", "capabilities/fixer.yaml"], ["sources/on-push.yaml"]),
		"capabilities/review.yaml": capabilityDoc("{{release.name}}-{{values.project}}-review", []),
		"capabilities/fixer.yaml": capabilityDoc("{{release.name}}-{{values.project}}-fixer", [], [
			"{{release.name}}-{{values.project}}-review",
		]),
		"sources/on-push.yaml": sourceDoc("{{release.name}}-{{values.project}}-on-push", [
			"{{release.name}}-{{values.project}}-review",
		]),
	});

	const ra = await install(ctx, slug, "a", UNINST_VALUES);
	V.expectStatus("release a 安装（2 能力 + 1 源）→ 200", ra, 200);
	const rb = await install(ctx, slug, "b", UNINST_VALUES);
	V.expectStatus("release b 安装 → 200", rb, 200);
	const aInstallId = ra.json?.install?.id;
	const before = rowCounts(ctx);

	const res = await uninstall(ctx, slug, "a");
	V.expectStatus("卸载 release a → 200", res, 200);
	const doc = res.json || {};
	H.eq("动作文档回显 release", doc.release, "a");
	H.eq("首次卸载 alreadyUninstalled=false", doc.alreadyUninstalled, false);
	H.eq("removed.capabilities 只列 a 装出的两行（按名字排序）", doc.removed?.capabilities, ["a-uninst-fixer@1", "a-uninst-review@1"]);
	H.eq("removed.sources 只列 a 装出的源", doc.removed?.sources, ["a-uninst-on-push"]);
	H.eq("kept 是空数组（没有同名的手工/他人行）", doc.kept, []);
	H.check("installIds 只有 1 条（a 只装了一次）", Array.isArray(doc.installIds) && doc.installIds.length === 1, JSON.stringify(doc.installIds));
	H.eq("installIds[0] 就是 a 的回执 id", doc.installIds?.[0], aInstallId);
	H.check(
		"uninstalledAt 是能被 Date.parse 解出的时间",
		typeof doc.uninstalledAt === "string" && !Number.isNaN(Date.parse(doc.uninstalledAt)),
		String(doc.uninstalledAt),
	);
	H.check("note 非空（回执保留、他人对象不删的说明）", typeof doc.note === "string" && doc.note.length > 0, String(doc.note));

	const caps = await listNames(ctx, V.P.capabilities(NS), "能力");
	H.check("a 的两个能力都不在列表里", !caps.includes("a-uninst-review") && !caps.includes("a-uninst-fixer"), caps.join(","));
	H.check("b 的两个能力仍在列表里", caps.includes("b-uninst-review") && caps.includes("b-uninst-fixer"), caps.join(","));
	const srcs = await listNames(ctx, V.P.sources(NS), "源");
	H.check("a 的源不在列表里", !srcs.includes("a-uninst-on-push"), srcs.join(","));
	H.check("b 的源仍在列表里", srcs.includes("b-uninst-on-push"), srcs.join(","));

	const list = await readInstalls(ctx, slug, "卸载 a 后的包");
	H.eq("已卸载的 release 不再出现在实例清单里", list.total, 1);
	H.eq("清单里只剩 release b", list.groups[0]?.release, "b");

	const one = await H.req(ctx.base, PB.installById(slug, aInstallId), { session: ctx.session });
	V.expectStatus("a 的回执仍能单条读（回执是审计）→ 200", one, 200);
	H.eq("单条回执仍然说 release=a", one.json?.install?.release, "a");

	const after = rowCounts(ctx);
	H.eq("卸载不删能力历史行（capability_versions 行数不变）", after.capability_versions, before.capability_versions);
	H.eq("卸载删掉 a 的源行（sources -1）", after.sources, before.sources - 1);

	const nsId = await H.defaultNamespaceId(ctx.base, ctx.session);
	const countWhere = (sql, args) => dbRows(ctx, sql, args).length;
	const capNames = (pattern, live) =>
		dbRows(
			ctx,
			`SELECT name FROM capability_versions
			  WHERE namespace_id = ? AND name LIKE ? AND deleted_at IS ${live ? "NULL" : "NOT NULL"}
			  ORDER BY name`,
			[nsId, pattern],
		).map((r) => r.name);
	H.eq("库里 a 的 2 行能力都打了 deleted_at（软删不物理删）", capNames("a-uninst-%", false), ["a-uninst-fixer", "a-uninst-review"]);
	H.eq("库里 a 的能力没有一行还可见", capNames("a-uninst-%", true), []);
	H.eq("库里 b 的 2 行能力仍可见（一行没动）", capNames("b-uninst-%", true), ["b-uninst-fixer", "b-uninst-review"]);
	const srcRows = (name) => countWhere("SELECT name FROM sources WHERE namespace_id = ? AND name = ?", [nsId, name]);
	H.eq("库里 a 的源已经删行", srcRows("a-uninst-on-push"), 0);
	H.eq("库里 b 的源还在", srcRows("b-uninst-on-push"), 1);
	const edgeFrom = (name) => countWhere("SELECT from_ref FROM edges WHERE namespace_id = ? AND from_is_source = 0 AND from_ref = ?", [nsId, name]);
	H.eq("a-uninst-fixer 的出边随行撤掉", edgeFrom("a-uninst-fixer"), 0);
	H.check("b-uninst-fixer 的出边还在", edgeFrom("b-uninst-fixer") > 0, String(edgeFrom("b-uninst-fixer")));
}

async function partUninstallKeepsManualRow(ctx) {
	console.log("--- 7. 同名手工行进 kept[] 且不删（#510）---");
	const slug = "pb-uninstall-share";
	await createPackage(ctx, slug, {
		"playbook.yaml": entryDoc(slug, ["capabilities/review.yaml"], ["sources/on-push.yaml"]),
		"capabilities/review.yaml": capabilityDoc("share-{{values.project}}-review", []),
		"sources/on-push.yaml": sourceDoc("{{release.name}}-share-{{values.project}}-on-push", ["share-{{values.project}}-review"]),
	});

	const ra = await install(ctx, slug, "a");
	V.expectStatus("release a 安装 → 200", ra, 200);
	const manual = await V.createCapability(
		ctx.base,
		ctx.session,
		NS,
		V.capabilityBody({
			name: "share-demo-review",
			run: { entrypoint: "run.sh", files: { "run.sh": "#!/bin/sh\necho manual\n" } },
		}),
	);
	V.expectStatus("手工再建同名能力（内容不同）→ 201", manual, 201);
	H.eq("手工行落在 v2（同名新内容分配 max+1）", manual.json?.metadata?.version, 2);
	const read = await H.req(ctx.base, V.P.capability(NS, "share-demo-review"), { session: ctx.session });
	V.expectStatus("GET 同名能力 → 200", read, 200);
	H.eq("读回来的名字就是它", read.json?.metadata?.name, "share-demo-review");
	H.eq("读回来的是 v2", read.json?.metadata?.version, 2);

	const res = await uninstall(ctx, slug, "a");
	V.expectStatus("卸载 release a（同名手工行在场）→ 200", res, 200);
	const doc = res.json || {};
	H.eq("kept 只有一条", Array.isArray(doc.kept) ? doc.kept.length : -1, 1);
	H.eq("kept 说清留下谁、哪版、为什么", doc.kept?.[0], {
		kind: "capability",
		name: "share-demo-review",
		version: 2,
		reason: "manual",
	});
	H.eq("removed.capabilities 仍是本 release 的 v1", doc.removed?.capabilities, ["share-demo-review@1"]);
	H.eq("removed.sources 仍是本 release 的源", doc.removed?.sources, ["a-share-demo-on-push"]);

	const still = await H.req(ctx.base, V.P.capability(NS, "share-demo-review"), { session: ctx.session });
	V.expectStatus("手工行没被删：GET 仍 200", still, 200);
	H.eq("手工行仍是 v2", still.json?.metadata?.version, 2);

	const nsId = await H.defaultNamespaceId(ctx.base, ctx.session);
	const rows = dbRows(
		ctx,
		"SELECT version, deleted_at FROM capability_versions WHERE namespace_id = ? AND name = ? ORDER BY version",
		[nsId, "share-demo-review"],
	);
	H.eq("同名两版都在库里（软删不是物理删）", rows.map((r) => r.version), [1, 2]);
	H.check("v1（本 release 装的）已软删", rows[0]?.deleted_at !== null && rows[0]?.deleted_at !== undefined, JSON.stringify(rows[0]));
	H.check("v2（手工的）仍可见", rows[1]?.deleted_at === null, JSON.stringify(rows[1]));
}

async function partUninstallIdempotent(ctx) {
	console.log("--- 8. 第二次卸载是 0 变化的幂等（#510）---");
	const slug = "pb-uninstall";
	const first = await uninstall(ctx, slug, "b");
	V.expectStatus("卸载 release b → 200", first, 200);
	const d1 = first.json || {};
	H.eq("first.alreadyUninstalled=false", d1.alreadyUninstalled, false);
	H.eq("first.removed.capabilities 是 b 的两行", d1.removed?.capabilities, ["b-uninst-fixer@1", "b-uninst-review@1"]);
	H.eq("first.removed.sources 是 b 的源", d1.removed?.sources, ["b-uninst-on-push"]);

	const before = dbFingerprint(ctx.dbPath);
	const second = await uninstall(ctx, slug, "b");
	V.expectStatus("再卸一次 release b → 200", second, 200);
	const d2 = second.json || {};
	H.eq("第二次 alreadyUninstalled=true", d2.alreadyUninstalled, true);
	H.eq("第二次 removed.capabilities 是 []", d2.removed?.capabilities, []);
	H.eq("第二次 removed.sources 是 []", d2.removed?.sources, []);
	H.eq("第二次 kept 是 []", d2.kept, []);
	H.check("第二次 installIds 非空（幂等也返回审计锚点）", Array.isArray(d2.installIds) && d2.installIds.length === 1, JSON.stringify(d2.installIds));
	H.eq("第二次 installIds 与第一次相同", d2.installIds, d1.installIds);
	H.eq("第二次 uninstalledAt 与第一次相同（标记不刷新）", d2.uninstalledAt, d1.uninstalledAt);

	const after = dbFingerprint(ctx.dbPath);
	H.check("第二次卸载后库指纹逐字节相同（0 写）", after === before, `before ${before.length} 字节 vs after ${after.length} 字节`);
}

async function partUninstallErrors(ctx) {
	console.log("--- 9. 卸载的错误面与 0 写（#510）---");
	const slug = "pb-uninstall-share";
	const before = dbFingerprint(ctx.dbPath);
	const cases = [
		["release 不存在", "nope", "", 404, "release_not_found"],
		["installId 与该 release 对不上", "a", "installId=ins_nope", 409, "install_conflict"],
		["release 形状错", "Prod", "", 400, "invalid_playbook"],
	];
	for (const [why, rel, query, code, reason] of cases) {
		const r = await uninstall(ctx, slug, rel, query);
		V.expectReason(`卸载${why} → ${code}`, r, code, reason);
		H.eq(`${why}之后库指纹不变（0 写）`, dbFingerprint(ctx.dbPath), before);
	}
	const missing = await H.req(ctx.base, PB.release("pb-does-not-exist", "a"), { session: ctx.session, method: "DELETE" });
	V.expectReason("包不存在 → 404", missing, 404, "NotFound");
	H.eq("包不存在之后库指纹不变（0 写）", dbFingerprint(ctx.dbPath), before);
}

function createPackageRaw(ctx, slug, requires) {
	return H.req(ctx.base, PB.list(), {
		session: ctx.session,
		method: "POST",
		body: {
			apiVersion: V.API_VERSION,
			kind: "Playbook",
			metadata: { name: slug },
			spec: {
				files: {
					"playbook.yaml": entryDoc(slug, ["capabilities/review.yaml"], [], 0, requires),
					"capabilities/review.yaml": capabilityDoc("req-bad-review", []),
				},
			},
		},
	});
}

async function partRequires(ctx) {
	console.log("--- 11. requires 依赖锁：缺依赖零写 / 版本不够 / 满足即装 / 删除被锁（#534）---");

	const missingSlug = "pb-req-missing";
	await createPackage(ctx, missingSlug, {
		"playbook.yaml": entryDoc(missingSlug, ["capabilities/review.yaml"], [], 0, [{ capability: "nope" }]),
		"capabilities/review.yaml": capabilityDoc("req-missing-review", []),
	});
	const beforeMissing = dbFingerprint(ctx.dbPath);
	const missing = await install(ctx, missingSlug, "m");
	V.expectReason("requires 指向不存在的能力 → 409 dependency_missing", missing, 409, "dependency_missing");
	const mCause = missing.json?.details?.causes?.[0] || {};
	H.eq("缺依赖的 causes[0].field", mCause.field, "spec.requires[0].capability");
	H.check(
		"缺依赖的 causes[0].message 写 have: none（未解析到，不是版本不够）",
		typeof mCause.message === "string" && mCause.message.includes("have: none"),
		JSON.stringify(mCause),
	);
	H.check("缺依赖的 causes[0].message 不写 have: 数字", !/have: \d/.test(String(mCause.message)), JSON.stringify(mCause.message));
	const afterMissing = dbFingerprint(ctx.dbPath);
	H.check(
		"缺依赖的安装一行不写（6 张表指纹逐字节相同）",
		afterMissing === beforeMissing,
		`before ${beforeMissing.length} 字节 vs after ${afterMissing.length} 字节`,
	);

	const capabilityOf = (script) =>
		V.capabilityBody({
			name: "inventory",
			run: { entrypoint: "run.sh", files: { "run.sh": `#!/bin/sh\necho ${script}\n` } },
		});
	const inv1 = await V.createCapability(ctx.base, ctx.session, NS, capabilityOf("inventory-v1"));
	V.expectStatus("建能力 inventory（v1）→ 201", inv1, 201);
	H.eq("inventory 的第一版落在 v1", inv1.json?.metadata?.version, 1);
	const inv2 = await V.createCapability(ctx.base, ctx.session, NS, capabilityOf("inventory-v2"));
	V.expectStatus("同名改内容再建（新内容分配 max+1）→ 201", inv2, 201);
	const invRead = await H.req(ctx.base, V.P.capability(NS, "inventory"), { session: ctx.session });
	V.expectStatus("GET inventory → 200", invRead, 200);
	H.eq("读回来的 status.version = 2（依赖解析看的是可见最高版）", invRead.json?.status?.version, 2);

	const need3Slug = "pb-req-need3";
	await createPackage(ctx, need3Slug, {
		"playbook.yaml": entryDoc(need3Slug, ["capabilities/review.yaml"], [], 0, [
			{ capability: "inventory", minVersion: 3 },
		]),
		"capabilities/review.yaml": capabilityDoc("req-need3-review", []),
	});
	const beforeNeed3 = dbFingerprint(ctx.dbPath);
	const short = await install(ctx, need3Slug, "n3");
	V.expectReason("minVersion: 3 但只有 v2 → 409 dependency_missing", short, 409, "dependency_missing");
	const sCause = short.json?.details?.causes?.[0] || {};
	const sMsg = String(sCause.message || "");
	H.check("版本不够的 causes[0].message 同时给 need: 3 与 have: 2", sMsg.includes("need: 3") && sMsg.includes("have: 2"), JSON.stringify(sCause));
	H.eq("版本不够的 causes[0].field", sCause.field, "spec.requires[0].capability");
	const afterNeed3 = dbFingerprint(ctx.dbPath);
	H.check(
		"版本不够的安装一行不写（6 张表指纹逐字节相同）",
		afterNeed3 === beforeNeed3,
		`before ${beforeNeed3.length} 字节 vs after ${afterNeed3.length} 字节`,
	);

	const okSlug = "pb-req-ok";
	await createPackage(ctx, okSlug, {
		"playbook.yaml": entryDoc(okSlug, ["capabilities/review.yaml"], [], 0, [
			{ capability: "inventory", minVersion: 2 },
		]),
		"capabilities/review.yaml": capabilityDoc("req-ok-review", []),
	});
	const ok = await install(ctx, okSlug, "q");
	V.expectStatus("minVersion: 2 被满足 → 200", ok, 200);
	H.eq(
		"回执 dependencies 恒等 [{capability:inventory, version:2}]（比 JSON 串，不赌数组身份）",
		JSON.stringify(ok.json?.dependencies),
		JSON.stringify([{ capability: "inventory", version: 2 }]),
	);
	H.eq("依赖锁的包照旧装出自己的能力", ok.json?.created?.capabilities, ["req-ok-review@1"]);

	const invPath = V.P.capability(NS, "inventory");
	const invBefore = await H.req(ctx.base, invPath, { session: ctx.session });
	V.expectStatus("锁判定前 GET inventory → 200", invBefore, 200);
	const invRv = invBefore.json?.metadata?.resourceVersion;
	H.check("拿到的是 rv1 令牌", V.isResourceVersion(invRv), String(invRv));
	const locked = await H.req(ctx.base, `${invPath}?resourceVersion=${encodeURIComponent(invRv)}`, {
		session: ctx.session,
		method: "DELETE",
	});
	V.expectReason("被已生效安装依赖的能力 → 409 dependency_locked", locked, 409, "dependency_locked");
	const lCauses = Array.isArray(locked.json?.details?.causes) ? locked.json.details.causes : [];
	const hit = lCauses.find(
		(c) => typeof c?.message === "string" && c.message.includes(okSlug) && c.message.includes('"q"'),
	);
	H.check(`details.causes 点名「${okSlug} 的 release q 依赖它」`, !!hit, JSON.stringify(lCauses));
	const invStill = await H.req(ctx.base, invPath, { session: ctx.session });
	V.expectStatus("409 之后能力仍可见（整事务回滚 = 0 写）", invStill, 200);
	H.eq("409 之后令牌也没动", invStill.json?.metadata?.resourceVersion, invRv);
	const unLock = await uninstall(ctx, okSlug, "q");
	V.expectStatus("卸载 release q（撤掉依赖事实）→ 200", unLock, 200);
	const afterUnlock = await H.req(ctx.base, `${invPath}?resourceVersion=${encodeURIComponent(invRv)}`, {
		session: ctx.session,
		method: "DELETE",
	});
	V.expectStatus("没有消费者之后用同一个令牌删 → 204", afterUnlock, 204);
	const capNamesAfter = await listNames(ctx, V.P.capabilities(NS), "删依赖后的能力");
	H.check("inventory 已不在能力列表里", !capNamesAfter.includes("inventory"), capNamesAfter.join(","));

	const badShapes = [
		["未知键", [{ capability: "inventory", bogus: 1 }], /^spec\.requires\[0\]\.bogus$/],
		["minVersion: 0", [{ capability: "inventory", minVersion: 0 }], /^spec\.requires\[0\]\.minVersion$/],
		["非法能力名", [{ capability: "Bad_Name" }], /^spec\.requires\[0\]\.capability$/],
		["同能力重复声明", [{ capability: "inventory" }, { capability: "inventory" }], /^spec\.requires\[1\]\.capability$/],
	];
	let n = 0;
	for (const [why, reqs, want] of badShapes) {
		const slug = `pb-req-bad-${n++}`;
		const r = await createPackageRaw(ctx, slug, reqs);
		V.expectReason(`建包（requires ${why}）→ 400 invalid_playbook`, r, 400, "invalid_playbook");
		const field = String(r.json?.details?.causes?.[0]?.field || "");
		H.check(`建包（requires ${why}）的 causes[0].field = ${field}`, want.test(field), JSON.stringify(r.json?.details?.causes));
	}

	const selfSlug = "pb-req-self";
	await createPackage(ctx, selfSlug, {
		"playbook.yaml": entryDoc(selfSlug, ["capabilities/self.yaml"], [], 0, [{ capability: "self-review" }]),
		"capabilities/self.yaml": capabilityDoc("self-review", []),
	});
	const self = await install(ctx, selfSlug, "s");
	V.expectReason("自引用只能在安装期判 → 400 invalid_playbook", self, 400, "invalid_playbook");
	H.eq("自引用的 causes[0].field", self.json?.details?.causes?.[0]?.field, "spec.requires[0].capability");

	const noneSlug = "pb-req-none";
	await createPackage(ctx, noneSlug, {
		"playbook.yaml": entryDoc(noneSlug, ["capabilities/review.yaml"], [], 0, null),
		"capabilities/review.yaml": capabilityDoc("req-none-review", []),
	});
	const none = await install(ctx, noneSlug, "n");
	V.expectStatus("完全不写 requires 字段的包 → 200", none, 200);
	H.check(
		"对照组回执 dependencies 是 []（空列表不是 null/省略）",
		Array.isArray(none.json?.dependencies) && none.json.dependencies.length === 0,
		JSON.stringify(none.json?.dependencies),
	);
	const noneList = await readInstalls(ctx, noneSlug, "对照组包");
	H.eq("对照组装的 release 照旧出现在实例清单里", noneList.total, 1);
	const noneUn = await uninstall(ctx, noneSlug, "n");
	V.expectStatus("对照组卸载 → 200", noneUn, 200);

	const emptySlug = "pb-req-empty";
	await createPackage(ctx, emptySlug, {
		"playbook.yaml": entryDoc(emptySlug, ["capabilities/review.yaml"], [], 0, []),
		"capabilities/review.yaml": capabilityDoc("req-empty-review", []),
	});
	const empty = await install(ctx, emptySlug, "n");
	V.expectStatus("显式 requires: [] 的包 → 200（与省略等价）", empty, 200);
	H.check(
		"显式空 requires 的回执 dependencies 也是 []",
		Array.isArray(empty.json?.dependencies) && empty.json.dependencies.length === 0,
		JSON.stringify(empty.json?.dependencies),
	);
}

async function partReleaseYAML(ctx) {
	console.log("--- 10. 按 release 导出对象 YAML（#536）---");
	const slug = "pb-release-yaml";
	await createPackage(ctx, slug, {
		"playbook.yaml": entryDoc(slug, ["capabilities/z.yaml", "capabilities/a.yaml"], ["sources/on-push.yaml"]),
		"capabilities/z.yaml": capabilityDoc("{{values.project}}-z-review", []),
		"capabilities/a.yaml": capabilityDoc("{{values.project}}-a-fixer", []),
		"sources/on-push.yaml": sourceDocSecret("{{values.project}}-export-push", ["{{values.project}}-z-review"]),
	});
	const foreign = "pb-release-yaml-other";
	await createPackage(ctx, foreign, {
		"playbook.yaml": entryDoc(foreign, ["capabilities/manual.yaml"], ["sources/manual.yaml"]),
		"capabilities/manual.yaml": capabilityDoc("demo-manual-review", []),
		"sources/manual.yaml": sourceDoc("demo-manual-push", ["demo-manual-review"]),
	});
	V.expectStatus("别人的包安装 → 200", await install(ctx, foreign), 200);

	const inst = await install(ctx, slug, "default");
	V.expectStatus("导出用例安装 → 200", inst, 200);
	H.eq("回执只记两个能力", (inst.json?.created?.capabilities || []).slice().sort(), ["demo-a-fixer@1", "demo-z-review@1"]);

	const path = PB.releaseYAML(slug, "default");
	const before = dbFingerprint(ctx.dbPath);
	const first = await H.req(ctx.base, path, { session: ctx.session });
	V.expectStatus("导出实例 YAML → 200", first, 200);
	H.eq("Content-Type 是 application/yaml", first.headers["content-type"], "application/yaml; charset=utf-8");
	const parts = first.text.split("---\n");
	const header = parts[0];
	const docs = parts.slice(1);
	H.eq("头注释三行 + 2 能力 + 1 源", docs.length, 3);
	H.check("头注释写清实例与对象数", header.includes("release: default") && header.includes("objects: 3"), header);
	H.check("头注释写实「当前生效版本」", header.includes("当前生效版本"), header);
	H.eq(
		"能力文档与单对象端点逐字节相同（a-fixer）",
		docs[0],
		(await H.req(ctx.base, V.P.capability(NS, "demo-a-fixer") + "/yaml", { session: ctx.session })).json?.yaml,
	);
	H.eq(
		"能力文档与单对象端点逐字节相同（z-review）",
		docs[1],
		(await H.req(ctx.base, V.P.capability(NS, "demo-z-review") + "/yaml", { session: ctx.session })).json?.yaml,
	);
	H.eq(
		"源文档与单对象端点逐字节相同",
		docs[2],
		(await H.req(ctx.base, V.P.source(NS, "demo-export-push") + "/yaml", { session: ctx.session })).json?.yaml,
	);
	H.check(
		"顺序固定：能力升序（a-fixer → z-review）再源",
		first.text.indexOf("name: demo-a-fixer") < first.text.indexOf("name: demo-z-review") &&
			first.text.indexOf("name: demo-z-review") < first.text.indexOf("name: demo-export-push"),
		first.text.slice(0, 200),
	);
	H.check(
		"不越界：别人的同前缀对象不在导出里",
		!first.text.includes("demo-manual-review") && !first.text.includes("demo-manual-push"),
		first.text.slice(0, 200),
	);
	const src = (await H.req(ctx.base, V.P.source(NS, "demo-export-push"), { session: ctx.session })).json || {};
	const hook = src.status?.hookUrl || src.spec?.config?.hookUrl || "";
	H.check("webhook 源的 hookUrl 只读投影在", hook.startsWith("/v1/hooks/"), hook);
	H.check("secret 原文不出现、只给 secretSet", !first.text.includes("e2e-hook-secret") && first.text.includes("secretSet: true"), first.text.slice(0, 400));
	H.check(
		"token 只出现在 hookUrl 里",
		!first.text.replaceAll(hook, "").includes(hook.replace("/v1/hooks/", "")),
		"token 泄漏",
	);

	const second = await H.req(ctx.base, path, { session: ctx.session });
	H.eq("同一 release 两次导出逐字节相同", second.text, first.text);
	H.eq("导出是只读的（库指纹不变）", dbFingerprint(ctx.dbPath), before);

	V.expectReason("不存在的 release → 404 release_not_found", await H.req(ctx.base, PB.releaseYAML(slug, "nope"), { session: ctx.session }), 404, "release_not_found");
	V.expectStatus("卸载 default → 200", await uninstall(ctx, slug, "default"), 200);
	V.expectReason("已卸载的 release → 404 release_not_found", await H.req(ctx.base, path, { session: ctx.session }), 404, "release_not_found");

	const empty = "pb-release-yaml-empty";
	await createPackage(ctx, empty, { "playbook.yaml": entryDoc(empty, [], []) });
	V.expectStatus("0 对象的包安装 → 200", await install(ctx, empty), 200);
	const zero = await H.req(ctx.base, PB.releaseYAML(empty, empty), { session: ctx.session });
	V.expectStatus("0 对象的实例导出 → 200", zero, 200);
	H.check("0 对象只有头注释且 objects: 0", zero.text.includes("objects: 0") && !zero.text.includes("---"), zero.text);
}

async function partReleaseDetail(ctx) {
	console.log("--- 12. 实例详情读面（#548）：一个 release 一份详情，两份 release 的对象不串 ---");
	// 能力 / 源的名字必须含 `{{release.name}}`：否则第二份 release 会去认领第一份的行 → 409 release_conflict。
	const slug = "pb-release-detail";
	await createPackage(ctx, slug, {
		"playbook.yaml": entryDoc(slug, ["capabilities/review.yaml", "capabilities/fixer.yaml"], ["sources/on-push.yaml"]),
		"capabilities/review.yaml": capabilityDoc("{{release.name}}-review", [], ["{{release.name}}-fixer"]),
		"capabilities/fixer.yaml": capabilityDoc("{{release.name}}-fixer", []),
		"sources/on-push.yaml": sourceDoc("{{release.name}}-on-push", ["{{release.name}}-review"]),
	});
	const a = await install(ctx, slug, "a");
	V.expectStatus("装 release a → 200", a, 200);
	const b = await install(ctx, slug, "b");
	V.expectStatus("装 release b → 200", b, 200);
	H.eq("回执 a 只记自己的两个能力", (a.json?.created?.capabilities || []).slice().sort(), ["a-fixer@1", "a-review@1"]);

	const before = dbFingerprint(ctx.dbPath);
	const readA = await H.req(ctx.base, PB.release(slug, "a"), { session: ctx.session });
	V.expectStatus("GET …/releases/a → 200", readA, 200);
	const docA = readA.json || {};
	H.eq("release 身份 = URL 里的 a", docA.release, "a");
	H.eq("objects.capabilities 只有 a 的两个能力（按 name 升序）", (docA.objects?.capabilities || []).map((c) => c.name), ["a-fixer", "a-review"]);
	H.eq("objects.sources 只有 a 的源", (docA.objects?.sources || []).map((s) => s.name), ["a-on-push"]);
	H.eq("源的 kind 取 sources.kind", (docA.objects?.sources || []).map((s) => s.kind), ["webhook"]);
	H.check(
		"能力带当前生效的 version + digest（sha256:…）",
		(docA.objects?.capabilities || []).length === 2 &&
			docA.objects.capabilities.every((c) => c.version === 1 && typeof c.digest === "string" && c.digest.startsWith("sha256:")),
		JSON.stringify(docA.objects?.capabilities),
	);
	H.eq("params.count = 1（这个 release 只有一套 values）", docA.params?.count, 1);
	H.eq("params.digest 与顶层 valuesDigest 同值", docA.params?.digest, docA.valuesDigest);
	H.eq("lastInstallId 是最近一次安装（a）", docA.lastInstallId, a.json?.install?.id);
	H.eq("installedAt 是最新一次安装的 at", docA.installedAt, a.json?.install?.at);
	H.eq("groups 恰好 1 组", (docA.groups || []).length, 1);
	H.eq("groups[0].valuesDigest 镜像顶层", docA.groups?.[0]?.valuesDigest, docA.valuesDigest);
	H.eq("groups[0].installedAt 镜像顶层", docA.groups?.[0]?.installedAt, docA.installedAt);
	H.eq("groups[0].lastInstallId 镜像顶层", docA.groups?.[0]?.lastInstallId, docA.lastInstallId);
	H.eq("groups[0].attempts = 该组回执条数（1）", docA.groups?.[0]?.attempts, 1);
	H.eq("params.digest 与回执的 valuesDigest 一致", docA.valuesDigest, a.json?.valuesDigest);
	const receiptA = await H.req(ctx.base, PB.installById(slug, String(docA.lastInstallId)), { session: ctx.session });
	V.expectStatus("GET …/installs/{lastInstallId} → 200", receiptA, 200);
	H.eq("receipt 与 GET …/installs/{id} 逐字段一致", docA.receipt, receiptA.json);

	const readB = await H.req(ctx.base, PB.release(slug, "b"), { session: ctx.session });
	V.expectStatus("GET …/releases/b → 200", readB, 200);
	const docB = readB.json || {};
	H.eq("objects.capabilities 只有 b 的两个能力", (docB.objects?.capabilities || []).map((c) => c.name), ["b-fixer", "b-review"]);
	H.eq("objects.sources 只有 b 的源", (docB.objects?.sources || []).map((s) => s.name), ["b-on-push"]);
	H.check("a 的详情里没有 b 的任何一个对象", !JSON.stringify(docA.objects).includes('"b-'), JSON.stringify(docA.objects));
	H.check("b 的详情里没有 a 的任何一个对象", !JSON.stringify(docB.objects).includes('"a-'), JSON.stringify(docB.objects));
	H.check("a / b 的 lastInstallId 不是同一条", docA.lastInstallId !== docB.lastInstallId, `${docA.lastInstallId} vs ${docB.lastInstallId}`);
	H.eq("读路径不写库（两次详情读取后库指纹逐个字节相同）", dbFingerprint(ctx.dbPath), before);

	// 同一 release 装过两套 values：params.count 是**不同 valuesDigest 的个数**，groups 最近安装的组在前。
	const valuesSlug = "pb-release-values";
	await createPackage(ctx, valuesSlug, {
		"playbook.yaml": entryDoc(valuesSlug, ["capabilities/review.yaml"], []),
		"capabilities/review.yaml": capabilityDoc("{{values.project}}-review", []),
	});
	V.expectStatus("装 release g（project=one）→ 200", await install(ctx, valuesSlug, "g", { project: "one" }), 200);
	V.expectStatus("再装 release g（project=two）→ 200", await install(ctx, valuesSlug, "g", { project: "two" }), 200);
	const docG = (await H.req(ctx.base, PB.release(valuesSlug, "g"), { session: ctx.session })).json || {};
	H.eq("同一 release 两套 values → params.count = 2", docG.params?.count, 2);
	H.eq("groups 2 条（一组一个 valuesDigest）", (docG.groups || []).length, 2);
	H.eq("groups[0].valuesDigest 镜像顶层", docG.groups?.[0]?.valuesDigest, docG.valuesDigest);
	H.eq("groups[0].installedAt 镜像顶层", docG.groups?.[0]?.installedAt, docG.installedAt);
	H.eq("groups[0].lastInstallId 镜像顶层", docG.groups?.[0]?.lastInstallId, docG.lastInstallId);
	H.eq(
		"最近安装的组在前（groups[0] 是 project=two 那一组）",
		(docG.groups || []).map((g) => (g.objects?.capabilities || []).map((c) => c.name).join(",")),
		["two-review", "one-review"],
	);
	H.eq(
		"顶层 objects 是两组的并集（升序）",
		(docG.objects?.capabilities || []).map((c) => c.name).slice().sort(),
		["one-review", "two-review"],
	);
	H.eq("每组 attempts = 该组回执条数", (docG.groups || []).map((g) => g.attempts), [1, 1]);
	H.check("两个组的 valuesDigest 不同", docG.groups?.[0]?.valuesDigest !== docG.groups?.[1]?.valuesDigest, JSON.stringify((docG.groups || []).map((g) => g.valuesDigest)));

	// 空实例：空列表一律 []（不是 null），groups 仍有一组。
	const emptySlug = "pb-release-detail-empty";
	await createPackage(ctx, emptySlug, { "playbook.yaml": entryDoc(emptySlug, [], []) });
	V.expectStatus("0 对象的包安装 → 200", await install(ctx, emptySlug), 200);
	const zero = (await H.req(ctx.base, PB.release(emptySlug, emptySlug), { session: ctx.session })).json || {};
	H.eq("0 对象的能力列表是 []（不是 null）", zero.objects?.capabilities, []);
	H.eq("0 对象的源列表是 []", zero.objects?.sources, []);
	H.eq("0 对象的 dependencies 是 []", zero.dependencies, []);
	H.eq("0 对象的 groups 仍有 1 条", (zero.groups || []).length, 1);

	V.expectReason("从未存在的 release → 404 release_not_found", await H.req(ctx.base, PB.release(slug, "ghost"), { session: ctx.session }), 404, "release_not_found");
	V.expectStatus("卸载 a → 200", await uninstall(ctx, slug, "a"), 200);
	V.expectReason("已卸载的 release → 404 release_not_found", await H.req(ctx.base, PB.release(slug, "a"), { session: ctx.session }), 404, "release_not_found");
	V.expectStatus("卸载 a 不波及 b（b 仍 200）", await H.req(ctx.base, PB.release(slug, "b"), { session: ctx.session }), 200);
	V.expectReason("包不存在 → 404 NotFound", await H.req(ctx.base, PB.release("no-such-package", "a"), { session: ctx.session }), 404, "NotFound");
}

async function main() {
	H.startTimer();
	console.log("=== SUITE: playbook_release（#505 安装 release 身份 / #510 按 release 卸载 / #511 包版本历史 / #534 依赖锁）===");
	const server = await H.startServer({ suiteName: SUITE, rpId: "localhost" });
	try {
		const seeded = H.seed(server.dbPath, "e2e");
		const ctx = { base: server.base, session: seeded.session, dbPath: server.dbPath };
		await partLegacyDefaults(ctx);
		await partMultiRelease(ctx);
		await partCrossReleaseConflict(ctx);
		await partTemplatePassthrough(ctx);
		await partInvalidRelease(ctx);
		await partUninstallRemovesReleaseOnly(ctx);
		await partUninstallKeepsManualRow(ctx);
		await partUninstallIdempotent(ctx);
		await partUninstallErrors(ctx);
		await partReleaseYAML(ctx);
		await partVersions(ctx);
		await partRequires(ctx);
		await partReleaseDetail(ctx);
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
