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
	restore: (slug, rel) => `${PB.release(slug, rel)}/restore`,
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

// installPinned 是带 version 的 install（version 为空 = 不传，即老包行为）。
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

async function partInstalledSnapshot(ctx) {
	console.log("--- 13. 安装时快照导出（#551）---");
	const slug = "pb-snapshot";
	const cap = "snap-review";
	const src = "snap-push";
	await createPackage(ctx, slug, {
		"playbook.yaml": entryDoc(slug, ["capabilities/review.yaml"], ["sources/on-push.yaml"]),
		"capabilities/review.yaml": capabilityDoc(cap, ["#!/bin/sh", "echo snap-v3"]),
		"sources/on-push.yaml": sourceDoc(src, [cap]),
	});
	const defineCap = (script) => H.req(ctx.base, V.P.capabilities(NS), { // 预置到 v2 → 安装建出 v3（#551 的「装 review@3」形状）
		session: ctx.session,
		method: "POST",
		body: {
			apiVersion: V.API_VERSION,
			kind: "Capability",
			metadata: { name: cap },
			spec: { run: { entrypoint: "run.sh", files: { "run.sh": "#!/bin/sh\necho " + script + "\n" } } },
		},
	});
	V.expectStatus("预置同名能力 v1 → 201", await defineCap("snap-v1"), 201);
	V.expectStatus("预置同名能力 v2 → 201", await defineCap("snap-v2"), 201);

	const inst = await install(ctx, slug, "snap");
	V.expectStatus("安装 release snap → 200", inst, 200);
	const rcpt = inst.json || {};
	const installId = rcpt.install?.id;
	const pinned = Array.isArray(rcpt.created?.pinned) ? rcpt.created.pinned : null;
	H.check("回执 created.pinned 是数组（旧字段之外的新字段）", pinned !== null, JSON.stringify(rcpt.created));
	const pinOf = (kind) => (pinned || []).find((p) => p?.kind === kind) || {};
	const capPin = pinOf("capability");
	const srcPin = pinOf("source");
	H.eq("pinned 能力在前、源在后各一条", (pinned || []).map((p) => p.kind), ["capability", "source"]);
	H.eq("pinned 的能力项 name", capPin.name, cap);
	H.eq("pinned 的能力项 version 是安装时那一版（v3）", capPin.version, 3);
	H.check(
		"pinned 的能力项 digest 以 sha256: 开头",
		typeof capPin.digest === "string" && capPin.digest.startsWith("sha256:"),
		String(capPin.digest),
	);
	H.eq("pinned 的源项 name", srcPin.name, src);
	H.eq("pinned 的源项 kindOf 是源的 kind", srcPin.kindOf, "webhook");
	H.check(
		"源的 pinned 项没有 version/digest（源没有版本概念）",
		srcPin.version === undefined && srcPin.digest === undefined,
		JSON.stringify(srcPin),
	);
	H.eq("旧字段 created.capabilities 原样保留（name@version）", rcpt.created?.capabilities, [`${cap}@3`]);
	H.eq("旧字段 created.sources 原样保留（仍是裸名）", rcpt.created?.sources, [src]);
	const versionRow = dbRows(ctx, "SELECT digest FROM capability_versions WHERE name = ? AND version = 3", [cap])[0] || {};
	H.eq("pinned 的 digest 与能力版本行逐字相同", capPin.digest, versionRow.digest);

	const up = await H.req(ctx.base, V.P.capabilities(NS), {
		session: ctx.session,
		method: "POST",
		body: {
			apiVersion: V.API_VERSION,
			kind: "Capability",
			metadata: { name: cap },
			spec: { run: { entrypoint: "run.sh", files: { "run.sh": "#!/bin/sh\necho snap-v4\n" } } },
		},
	});
	V.expectStatus("同名能力换正文提交 → 201（分配新版本）", up, 201);
	H.eq("升级后的能力版本 = 4", up.json?.metadata?.version, 4);

	const path = PB.releaseYAML(slug, "snap");
	const beforeReads = dbFingerprint(ctx.dbPath);
	const docOf = (text, kind) => text.split("---\n").slice(1).find((d) => d.includes(`kind: ${kind}`)) || "";
	const installed = await H.req(ctx.base, path + "?as=installed", { session: ctx.session });
	V.expectStatus("as=installed 导出 → 200", installed, 200);
	H.check("安装时视图头注释写实「安装时快照」", installed.text.includes("安装时快照"), installed.text.slice(0, 400));
	H.check("安装时视图头注释带这次安装的回执 id", installed.text.includes(`install: ${installId}`), installed.text.slice(0, 400));
	H.check("安装时视图头注释 objects: 2（一个能力 + 一个源）", installed.text.includes("objects: 2"), installed.text.slice(0, 400));
	const snapCap = docOf(installed.text, "Capability");
	H.check("安装时视图的能力文档还是 v3", snapCap.includes("version: 3"), snapCap.slice(0, 300));
	H.check("安装时视图的能力正文是 v3 的脚本", snapCap.includes("snap-v3"), snapCap);
	H.check("安装时视图里没有升级后的 snap-v4", !installed.text.includes("snap-v4"), installed.text.slice(0, 400));
	H.check("安装时视图的源文档按名字取到当前正文", docOf(installed.text, "Source").includes(src), installed.text.slice(0, 400));

	const current = await H.req(ctx.base, path, { session: ctx.session });
	V.expectStatus("默认视图导出 → 200", current, 200);
	const currCap = docOf(current.text, "Capability");
	H.check("默认视图的能力文档是当前 v4", currCap.includes("version: 4"), currCap.slice(0, 300));
	H.check("默认视图的能力正文是升级后的 snap-v4", currCap.includes("snap-v4"), currCap);
	H.check("默认视图头注释仍说「当前生效版本」", current.text.includes("当前生效版本"), current.text.slice(0, 400));
	H.check("两种视图正文不同（安装时快照 ≠ 当前生效）", installed.text !== current.text, "两份正文逐字节相同");
	const asCurrent = await H.req(ctx.base, path + "?as=current", { session: ctx.session });
	V.expectStatus("as=current 导出 → 200", asCurrent, 200);
	H.eq("as=current 与不带参数逐字节相同", asCurrent.text, current.text);
	V.expectReason(
		"as=nope → 400 value_invalid",
		await H.req(ctx.base, path + "?as=nope", { session: ctx.session }),
		400,
		"value_invalid",
	);
	H.eq("这几次导出都没落库（库指纹逐字节相同）", dbFingerprint(ctx.dbPath), beforeReads);

	V.expectStatus("卸载 release snap → 200", await uninstall(ctx, slug, "snap"), 200);
	const afterUninstall = dbFingerprint(ctx.dbPath);
	const gone = await H.req(ctx.base, path + "?as=installed", { session: ctx.session });
	V.expectStatus("卸载后 as=installed 仍能导出 → 200", gone, 200);
	H.check("卸载后的快照头注释仍带那次安装的 id", gone.text.includes(`install: ${installId}`), gone.text.slice(0, 400));
	H.check(
		"卸载后的快照仍是安装时那版正文（软删行仍可读）",
		gone.text.includes("snap-v3") && !gone.text.includes("snap-v4"),
		gone.text.slice(0, 400),
	);
	H.eq("卸载后的快照只剩能力一个文档（源被物理删）", gone.text.split("---\n").length - 1, 1);
	H.check("卸载后的快照不含源文档", !gone.text.includes("kind: Source"), gone.text.slice(0, 400));
	H.check("删掉的源计入「快照缺失: 1」", gone.text.includes("# 快照缺失: 1"), gone.text.slice(0, 400));
	V.expectReason(
		"卸载后默认视图 → 404 release_not_found（口径不变）",
		await H.req(ctx.base, path, { session: ctx.session }),
		404,
		"release_not_found",
	);
	V.expectReason(
		"卸载后 as=current 也 → 404（与不带参数等价）",
		await H.req(ctx.base, path + "?as=current", { session: ctx.session }),
		404,
		"release_not_found",
	);
	H.eq("卸载之后快照的这几次读都不改库（库指纹逐字节相同）", dbFingerprint(ctx.dbPath), afterUninstall);

	const legacyId = "ins_e2e_551_legacy"; // 老回执（#551 之前装的）：手写一条没有 pinned 的回执，读面必须 409 而不是编造一份快照。
	const legacyDigest = "sha256:" + "0".repeat(64);
	const nsRow = dbRows(ctx, "SELECT id FROM namespaces WHERE slug = ?", [NS])[0] || {};
	const pbRow = dbRows(ctx, "SELECT id FROM playbooks WHERE slug = ?", [slug])[0] || {};
	H.check("查得到命名空间 id（直插老回执用）", typeof nsRow.id === "string" && nsRow.id.length > 0, JSON.stringify(nsRow));
	H.check("查得到包 id（直插老回执用）", typeof pbRow.id === "string" && pbRow.id.length > 0, JSON.stringify(pbRow));
	const legacyReceipt = {
		apiVersion: "anotify/v1",
		kind: "InstallReceipt",
		playbook: { name: slug, version: 1 },
		install: { id: legacyId, at: "2026-01-01T00:00:00Z", namespace: NS, release: "legacy", dryRun: false },
		namespace: NS,
		values: VALUES,
		valuesDigest: legacyDigest,
		created: { capabilities: [`${cap}@1`], sources: [] },
		skipped: { capabilities: [], sources: [] },
	};
	V.withDB(ctx.dbPath, (db) =>
		db
			.prepare(
				`INSERT INTO playbook_installs
				   (id, namespace_id, namespace_slug, playbook_id, playbook_slug, "values", values_digest, receipt, source, release, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', 'legacy', ?)`,
			)
			.run(
				legacyId,
				nsRow.id,
				NS,
				pbRow.id,
				slug,
				JSON.stringify(VALUES),
				legacyDigest,
				JSON.stringify(legacyReceipt),
				Date.now(),
			),
	);
	const legacyPath = PB.releaseYAML(slug, "legacy");
	const noSnap = await H.req(ctx.base, legacyPath + "?as=installed", { session: ctx.session });
	V.expectReason("老回执没有 pinned → 409 no_snapshot", noSnap, 409, "no_snapshot");
	H.eq("409 的 details.causes[0].field 指到 release", noSnap.json?.details?.causes?.[0]?.field, "release");
	H.check(
		"409 的 message 说明「安装于未记录快照的版本」",
		typeof noSnap.json?.message === "string" && noSnap.json.message.includes("未记录快照"),
		String(noSnap.json?.message).slice(0, 200),
	);
	const legacyCurrent = await H.req(ctx.base, legacyPath, { session: ctx.session });
	V.expectStatus("老回执的当前视图不回归 → 200", legacyCurrent, 200);
	H.check("老回执的当前视图照旧说「当前生效版本」", legacyCurrent.text.includes("当前生效版本"), legacyCurrent.text.slice(0, 400));
	H.check("老回执的当前视图头注释带它自己的 install id", legacyCurrent.text.includes(`install: ${legacyId}`), legacyCurrent.text.slice(0, 400));
	H.check("老回执的当前视图取的是当前生效的 v4", legacyCurrent.text.includes("snap-v4"), legacyCurrent.text.slice(0, 400));
}

async function readReleaseDetail(ctx, slug, rel, what) {
	const r = await H.req(ctx.base, PB.release(slug, rel), { session: ctx.session });
	V.expectStatus(`${what} → 200`, r, 200);
	return r.json || {};
}

async function partReleaseDetail(ctx) {
	console.log("--- 12. 实例详情端点与按参数组导出（#548）---");
	const capTpl = "{{release.name}}-{{values.project}}-review";
	const srcTpl = "{{release.name}}-{{values.project}}-on-push";
	const capOf = (rel, project) => `${rel}-${project}-review`;
	const srcOf = (rel, project) => `${rel}-${project}-on-push`;
	const pack = (slug, cap, src) => ({
		"playbook.yaml": entryDoc(slug, ["capabilities/review.yaml"], ["sources/on-push.yaml"]),
		"capabilities/review.yaml": capabilityDoc(cap, []),
		"sources/on-push.yaml": sourceDoc(src, [cap]),
	});

	// 两份 release 装同一套模板：详情只该看见自己那一份对象（归属由回执 created 决定，不按前缀猜）。
	const slug = "pb-detail";
	await createPackage(ctx, slug, pack(slug, capTpl, srcTpl));
	const receipts = {};
	for (const rel of ["blue", "green"]) {
		const r = await install(ctx, slug, rel, { project: rel });
		V.expectStatus(`release ${rel} 安装 → 200`, r, 200);
		receipts[rel] = r.json || {};
	}

	const beforeReads = dbFingerprint(ctx.dbPath);
	for (const rel of ["blue", "green"]) {
		const other = rel === "blue" ? "green" : "blue";
		const detail = await readReleaseDetail(ctx, slug, rel, `release ${rel} 的详情`);
		H.eq(`release ${rel} 详情回显自己的 release`, detail.release, rel);
		H.eq(`release ${rel} 详情 playbook.name 是包 slug`, detail.playbook?.name, slug);
		H.eq(`release ${rel} 只有一套 values（params.groups）`, detail.params?.groups, 1);
		H.check(
			`release ${rel} 详情的 valuesDigest 非空`,
			typeof detail.valuesDigest === "string" && detail.valuesDigest.length > 0,
			String(detail.valuesDigest),
		);
		H.eq(`release ${rel} 详情 params.digest 与顶层 valuesDigest 同值`, detail.params?.digest, detail.valuesDigest);
		H.check(
			`release ${rel} 详情的 installedAt 能被 Date.parse 解出`,
			typeof detail.installedAt === "string" && !Number.isNaN(Date.parse(detail.installedAt)),
			String(detail.installedAt),
		);
		H.eq(`release ${rel} 详情 lastInstallId 就是那次安装的回执`, detail.lastInstallId, receipts[rel].install?.id);

		const caps = detail.objects?.capabilities || [];
		const srcs = detail.objects?.sources || [];
		const capNames = caps.map((c) => c.name);
		const srcNames = srcs.map((s) => s.name);
		H.eq(`release ${rel} 装出的能力就是自己那一个`, capNames, [capOf(rel, rel)]);
		H.eq(`release ${rel} 装出的源就是自己那一个`, srcNames, [srcOf(rel, rel)]);
		H.check(
			`release ${rel} 的能力名全部以 ${rel}- 开头`,
			capNames.length > 0 && capNames.every((n) => n.startsWith(`${rel}-`)),
			capNames.join(","),
		);
		H.check(`release ${rel} 的详情不含 ${other} 的能力`, !capNames.some((n) => n.startsWith(`${other}-`)), capNames.join(","));
		H.check(`release ${rel} 的详情不含 ${other} 的源`, !srcNames.some((n) => n.startsWith(`${other}-`)), srcNames.join(","));
		H.eq(`release ${rel} 的源 kind 是 webhook`, srcs[0]?.kind, "webhook");
		for (const cap of caps) {
			H.check(`能力 ${cap.name} 带 sha256 摘要`, typeof cap.digest === "string" && cap.digest.startsWith("sha256:"), String(cap.digest));
			H.check(`能力 ${cap.name} 的 version ≥ 1`, Number.isInteger(cap.version) && cap.version >= 1, String(cap.version));
		}

		for (const cap of caps) {
			const one = await H.req(ctx.base, V.P.capability(NS, cap.name), { session: ctx.session });
			V.expectStatus(`单对象端点读能力 ${cap.name} → 200`, one, 200);
			H.eq(`详情里 ${cap.name} 的版本与单对象端点一致`, one.json?.metadata?.version, cap.version);
		}
		for (const src of srcs) {
			const one = await H.req(ctx.base, V.P.source(NS, src.name), { session: ctx.session });
			V.expectStatus(`单对象端点读源 ${src.name} → 200`, one, 200);
			H.eq(`详情里 ${src.name} 的 kind 与单对象端点一致`, one.json?.spec?.kind, src.kind);
		}

		const rcpt = await H.req(ctx.base, PB.installById(slug, detail.lastInstallId), { session: ctx.session });
		V.expectStatus(`release ${rel} 那次安装的回执 → 200`, rcpt, 200);
		H.eq(`release ${rel} 回执的 valuesDigest 与详情同值`, rcpt.json?.valuesDigest, detail.valuesDigest);
	}
	H.eq("两份 release 并存 → 实例清单仍是 2 组", (await readInstalls(ctx, slug, "详情包")).total, 2);
	H.eq("详情 / 单对象 / 回执 / 清单的 GET 都没落库（库指纹逐字节相同）", dbFingerprint(ctx.dbPath), beforeReads);

	// 同一 release 两套 values 折叠成两组：每组的 objects 来自该组回执，组间不串。
	const grouped = "pb-detail-groups";
	const gsRel = "gs";
	await createPackage(ctx, grouped, pack(grouped, capTpl, srcTpl));
	for (const project of ["one", "two"]) {
		V.expectStatus(`release ${gsRel} 用 project=${project} 安装 → 200`, await install(ctx, grouped, gsRel, { project }), 200);
	}
	const beforeGroups = dbFingerprint(ctx.dbPath);
	const gdetail = await readReleaseDetail(ctx, grouped, gsRel, `release ${gsRel} 的两组详情`);
	H.eq("同一 release 两套 values → params.groups", gdetail.params?.groups, 2);
	H.eq("groups 明细也是 2 组", (gdetail.groups || []).length, 2);
	const digests = (gdetail.groups || []).map((g) => g.valuesDigest);
	H.check("两组的 valuesDigest 不同", digests.length === 2 && digests[0] !== digests[1], digests.join(","));
	H.eq("顶层描述的是最近安装的那一组（project=two）", gdetail.groups?.[0]?.values?.project, "two");
	H.eq("顶层 valuesDigest 就是第一组的摘要", gdetail.valuesDigest, digests[0]);
	const groupOf = (project) => (gdetail.groups || []).find((g) => g.values?.project === project) || {};
	for (const project of ["one", "two"]) {
		const other = project === "one" ? "two" : "one";
		const g = groupOf(project);
		const capNames = (g.objects?.capabilities || []).map((c) => c.name);
		const srcNames = (g.objects?.sources || []).map((s) => s.name);
		H.eq(`组 project=${project} 的能力带自己那套 values 的前缀`, capNames, [capOf(gsRel, project)]);
		H.eq(`组 project=${project} 的源带自己那套 values 的前缀`, srcNames, [srcOf(gsRel, project)]);
		H.check(
			`组 project=${project} 不串另一组的对象`,
			!capNames.concat(srcNames).some((n) => n.includes(`-${other}-`)),
			capNames.concat(srcNames).join(","),
		);
		H.eq(`组 project=${project} 的 attempts = 1`, g.attempts, 1);
		H.check(`组 project=${project} 的 lastInstallId 非空`, typeof g.lastInstallId === "string" && g.lastInstallId.length > 0, String(g.lastInstallId));
	}

	for (const project of ["one", "two"]) {
		const other = project === "one" ? "two" : "one";
		const path = PB.releaseYAML(grouped, gsRel) + "?valuesDigest=" + encodeURIComponent(groupOf(project).valuesDigest);
		const r = await H.req(ctx.base, path, { session: ctx.session });
		V.expectStatus(`按参数组导出（project=${project}）→ 200`, r, 200);
		H.check(
			`按组导出只含 project=${project} 的对象`,
			r.text.includes(capOf(gsRel, project)) && !r.text.includes(`${gsRel}-${other}-`),
			r.text.slice(0, 300),
		);
	}
	const allGroups = await H.req(ctx.base, PB.releaseYAML(grouped, gsRel), { session: ctx.session });
	V.expectStatus("不带 valuesDigest 的导出 → 200", allGroups, 200);
	H.check(
		"不带 valuesDigest 时两组对象都在（并集，不是只剩第一组）",
		allGroups.text.includes(capOf(gsRel, "one")) && allGroups.text.includes(capOf(gsRel, "two")),
		allGroups.text.slice(0, 300),
	);
	H.eq("详情与按组导出的 GET 都没落库（库指纹逐字节相同）", dbFingerprint(ctx.dbPath), beforeGroups);

	// 404 面与「卸载后详情消失」；DELETE 会写库，所以卸载之后重新取指纹，只断言读不改库。
	const beforeNope = dbFingerprint(ctx.dbPath);
	V.expectReason("release 不存在 → 404 release_not_found", await H.req(ctx.base, PB.release(slug, "nope"), { session: ctx.session }), 404, "release_not_found");
	H.eq("404 的详情读不改库（库指纹逐字节相同）", dbFingerprint(ctx.dbPath), beforeNope);
	V.expectStatus("卸载 release blue → 200", await uninstall(ctx, slug, "blue"), 200);
	const afterUninstall = dbFingerprint(ctx.dbPath);
	V.expectReason("已卸载的 release 详情 → 404 release_not_found", await H.req(ctx.base, PB.release(slug, "blue"), { session: ctx.session }), 404, "release_not_found");
	V.expectStatus("blue 卸载后 green 的详情仍能读 → 200", await H.req(ctx.base, PB.release(slug, "green"), { session: ctx.session }), 200);
	H.eq("卸载之后的这几次读都不改库（库指纹逐字节相同）", dbFingerprint(ctx.dbPath), afterUninstall);
}

async function partInstallHistory(ctx) {
	console.log("--- 14. 安装历史（含已卸载）：include=uninstalled / objectsGone / pinned（#553）---");
	const slug = "pb-history";
	// 两个 release 装同一套模板但要建出不同名的对象（`{{release.name}}` 前缀），否则第二份安装会认领冲突。
	const capTpl = "{{release.name}}-{{values.project}}-review";
	const srcTpl = "{{release.name}}-{{values.project}}-on-push";
	await createPackage(ctx, slug, {
		"playbook.yaml": entryDoc(slug, ["capabilities/review.yaml"], ["sources/on-push.yaml"]),
		"capabilities/review.yaml": capabilityDoc(capTpl, []),
		"sources/on-push.yaml": sourceDoc(srcTpl, [capTpl]),
	});
	const receipts = {};
	for (const rel of ["live", "gone"]) {
		const r = await install(ctx, slug, rel);
		V.expectStatus(`release ${rel} 安装 → 200`, r, 200);
		receipts[rel] = r.json || {};
	}

	const RAW = (query = "") => H.req(ctx.base, PB.installs(slug) + query, { session: ctx.session });
	const DETAIL = (rel, query = "") => H.req(ctx.base, PB.release(slug, rel) + query, { session: ctx.session });
	const groupsOf = (res) =>
		(Array.isArray(res.json?.installs) ? res.json.installs : [])
			.slice()
			.sort((a, b) => String(a.release).localeCompare(String(b.release)));
	const hasUninstalledAt = (group) => Object.prototype.hasOwnProperty.call(group, "uninstalledAt");

	const bothLive = await readInstalls(ctx, slug, "两个 release 都生效时");
	H.eq("对照组：两个 release 各一组", bothLive.groups.map((g) => g.release).sort(), ["gone", "live"]);
	H.eq("对照组：两个 release 都生效时 total === 2", bothLive.total, 2);
	H.check(
		"对照组：生效中的组**没有** uninstalledAt 键（缺省口径逐字不变）",
		bothLive.groups.length === 2 && bothLive.groups.every((g) => !hasUninstalledAt(g)),
		JSON.stringify(bothLive.groups.map((g) => Object.keys(g))),
	);

	V.expectStatus("卸载 release gone → 200", await uninstall(ctx, slug, "gone"), 200);

	const afterUninstall = await readInstalls(ctx, slug, "卸载 gone 后的缺省");
	H.eq("缺省清单不再含已卸载的 gone", afterUninstall.groups.map((g) => g.release), ["live"]);
	H.eq("缺省清单 total 只数生效中（1，不是 2）", afterUninstall.total, 1);
	H.check(
		"缺省清单里剩下的 live 组仍没有 uninstalledAt 键",
		!afterUninstall.groups.some(hasUninstalledAt),
		JSON.stringify(afterUninstall.groups.map((g) => Object.keys(g))),
	);

	const beforeReads = dbFingerprint(ctx.dbPath);

	const all = await RAW("?include=uninstalled");
	V.expectStatus("include=uninstalled 的实例清单 → 200", all, 200);
	const allGroups = groupsOf(all);
	const goneGroup = allGroups.find((g) => g.release === "gone") || {};
	const liveGroup = allGroups.find((g) => g.release === "live") || {};
	H.eq("include=uninstalled 时已卸载的 gone 与生效中的 live 都在", allGroups.map((g) => g.release), ["gone", "live"]);
	H.eq("include=uninstalled 时 total === 2", all.json?.total, 2);
	H.check(
		"已卸载的组带 uninstalledAt，且是能 Date.parse 的 RFC3339",
		typeof goneGroup.uninstalledAt === "string" && !Number.isNaN(Date.parse(goneGroup.uninstalledAt)),
		String(goneGroup.uninstalledAt),
	);
	H.eq("已卸载组的 lastInstallId 就是那一次安装的回执", goneGroup.lastInstallId, receipts.gone.install?.id);
	H.check(
		"只有已卸载的组才带 uninstalledAt：生效中的 live 组没有这个键",
		!hasUninstalledAt(liveGroup),
		JSON.stringify(Object.keys(liveGroup)),
	);

	V.expectReason(
		"缺省口径下已卸载的详情 → 404 release_not_found",
		await DETAIL("gone"),
		404,
		"release_not_found",
	);

	const goneDetail = await DETAIL("gone", "?include=uninstalled");
	V.expectStatus("已卸载的详情带 include=uninstalled → 200", goneDetail, 200);
	const doc = goneDetail.json || {};
	H.check("已卸载详情带 objectsGone: true", doc.objectsGone === true, JSON.stringify(doc.objectsGone));
	H.check(
		"已卸载详情的 uninstalledAt 是非空字符串",
		typeof doc.uninstalledAt === "string" && doc.uninstalledAt.length > 0,
		String(doc.uninstalledAt),
	);
	const pins = Array.isArray(doc.pinned) ? doc.pinned : null;
	H.check("已卸载详情的 pinned 是数组（安装时快照仍可读，不是 409）", pins !== null, JSON.stringify(doc.pinned));
	const capPin = (pins || []).find((p) => p?.kind === "capability") || {};
	H.eq("pinned 的能力项 name 是这一份 release 装出的能力", capPin.name, "gone-demo-review");
	H.check("pinned 的能力项 version 是正整数", Number.isInteger(capPin.version) && capPin.version >= 1, String(capPin.version));
	H.check(
		"pinned 的能力项 digest 以 sha256: 开头",
		typeof capPin.digest === "string" && capPin.digest.startsWith("sha256:"),
		String(capPin.digest),
	);
	H.eq("已卸载详情的 objects.capabilities 强制为空", (doc.objects?.capabilities || []).length, 0);
	H.eq("已卸载详情的 objects.sources 强制为空", (doc.objects?.sources || []).length, 0);
	H.check(
		"已卸载详情的 groups[].objects 也强制为空（对象已删，不得泄漏同名重装的对象）",
		(Array.isArray(doc.groups) ? doc.groups : []).every(
			(g) => (g.objects?.capabilities || []).length === 0 && (g.objects?.sources || []).length === 0,
		),
		JSON.stringify((doc.groups || []).map((g) => g.objects)),
	);

	const liveDefault = await DETAIL("live");
	V.expectStatus("生效中 release 的缺省详情 → 200", liveDefault, 200);
	const liveAll = await DETAIL("live", "?include=uninstalled");
	V.expectStatus("生效中 release 带 include=uninstalled → 200", liveAll, 200);
	H.eq(
		"include=uninstalled 打在生效中 release 上与缺省响应逐字相同（UI 可以恒带）",
		JSON.stringify(liveAll.json),
		JSON.stringify(liveDefault.json),
	);

	V.expectReason("详情 include=nope → 400 value_invalid", await DETAIL("gone", "?include=nope"), 400, "value_invalid");
	V.expectReason("清单 include=nope → 400 value_invalid", await RAW("?include=nope"), 400, "value_invalid");

	const yamlPath = PB.releaseYAML(slug, "gone");
	const installedYaml = await H.req(ctx.base, yamlPath + "?as=installed", { session: ctx.session });
	V.expectStatus("已卸载的 as=installed 导出 → 200", installedYaml, 200);
	H.check(
		"已卸载的导出正文带 install: <那一次安装的回执 id>",
		installedYaml.text.includes(`install: ${receipts.gone.install?.id}`),
		installedYaml.text.slice(0, 300),
	);
	V.expectReason(
		"已卸载的缺省导出 → 404 release_not_found",
		await H.req(ctx.base, yamlPath, { session: ctx.session }),
		404,
		"release_not_found",
	);
	V.expectReason(
		"已卸载的 as=current 导出 → 404 release_not_found",
		await H.req(ctx.base, yamlPath + "?as=current", { session: ctx.session }),
		404,
		"release_not_found",
	);

	H.check(
		"已卸载的组不参与生效中计数：缺省 total 只数生效中（1），含已卸载才 2",
		afterUninstall.total === 1 && all.json?.total === 2,
		`缺省 ${afterUninstall.total} / 含已卸载 ${all.json?.total}`,
	);
	H.eq("这一段的历史读都没改库（库指纹逐字节相同）", dbFingerprint(ctx.dbPath), beforeReads);

	H.check(
		"#553 安装历史：默认不含已卸载 / include=uninstalled 带 uninstalledAt / 详情 objectsGone+pinned / 生效中逐字不变",
		afterUninstall.total === 1 &&
			allGroups.length === 2 &&
			typeof goneGroup.uninstalledAt === "string" &&
			doc.objectsGone === true &&
			Array.isArray(doc.pinned) &&
			doc.pinned.length > 0 &&
			JSON.stringify(liveAll.json) === JSON.stringify(liveDefault.json),
		[
			`缺省 total=${afterUninstall.total}`,
			`含已卸载=${allGroups.length}`,
			`uninstalledAt=${String(goneGroup.uninstalledAt)}`,
			`objectsGone=${String(doc.objectsGone)}`,
			`pinned=${Array.isArray(doc.pinned) ? doc.pinned.length : "null"}`,
			`生效中逐字相同=${JSON.stringify(liveAll.json) === JSON.stringify(liveDefault.json)}`,
		].join(" / "),
	);
}

// ---------- #561 按快照重装：pinned 版本 / 四种 409 / 审计 ----------

function restoreRelease(ctx, slug, rel, valuesDigest) {
	return H.req(ctx.base, PB.restore(slug, rel), {
		session: ctx.session,
		method: "POST",
		body: { valuesDigest },
	});
}

function auditRows(ctx, slug, rel) {
	return dbRows(
		ctx,
		`SELECT id, action, install_id, actor_id, detail FROM playbook_install_audit
		  WHERE playbook_slug = ? AND release = ?`,
		[slug, rel],
	);
}

function auditCount(ctx, slug, rel) {
	return auditRows(ctx, slug, rel).length;
}

async function expectZeroWrite(ctx, slug, rel, why, call) {
	const before = dbFingerprint(ctx.dbPath);
	const auditBefore = auditCount(ctx, slug, rel);
	const res = await call();
	const after = dbFingerprint(ctx.dbPath);
	H.check(`${why}：6 张表指纹逐字节相同（0 写）`, after === before, `before ${before.length} 字节 vs after ${after.length} 字节`);
	H.eq(`${why}：审计表没有新增行`, auditCount(ctx, slug, rel), auditBefore);
	return res;
}

function capabilityRowOf(ctx, name, version) {
	return (
		dbRows(
			ctx,
			`SELECT id, digest, spec FROM capability_versions
			  WHERE namespace_id = (SELECT id FROM namespaces WHERE slug = ?) AND name = ? AND version = ?
			  ORDER BY created_at DESC, rowid DESC LIMIT 1`,
			[NS, name, version],
		)[0] || {}
	);
}

function sourceShapeOf(doc) {
	const spec = doc?.spec || {};
	const config = { ...(spec.config || {}) };
	for (const key of ["token", "secret", "hookUrl"]) delete config[key];
	return {
		kind: spec.kind,
		enabled: spec.enabled,
		config,
		targets: spec.targets,
		displayName: doc?.metadata?.displayName ?? null,
	};
}

// 版本号由 API 分配、脚本指定不了第几版，“同名同版本已存在”的两个面只能直插行来造。
function insertManualCapabilityRow(ctx, { name, version, script }) {
	const row = capabilityRowOf(ctx, name, version);
	const spec = JSON.parse(row.spec || "{}");
	if (script !== undefined) {
		spec.run = Object.assign({}, spec.run, {
			files: Object.assign({}, spec.run?.files, { "run.sh": `#!/bin/sh\necho ${script}\n` }),
		});
	}
	const digest = script === undefined ? row.digest : "sha256:" + "9".repeat(64);
	V.withDB(ctx.dbPath, (db) =>
		db
			.prepare(
				`INSERT INTO capability_versions
				   (id, namespace_id, name, display_name, version, spec, digest, fold_key,
				    installed_from, resource_version, created_at)
				 SELECT ?, namespace_id, name, display_name, version, ?, ?, fold_key, '', 1, created_at + 10
				   FROM capability_versions WHERE id = ?`,
			)
			.run(`cap_e2e${Math.random().toString(36).slice(2, 10)}`, JSON.stringify(spec), digest, row.id),
	);
	return { digest, pinned: row.digest };
}

// seedLegacyReceiptRow 直插一条没有 pinned 行的老回执（迁移 8 之前的安装没留快照），且已卸载。
function seedLegacyReceiptRow(ctx, slug, release, digest) {
	const nsRow = dbRows(ctx, "SELECT id FROM namespaces WHERE slug = ?", [NS])[0] || {};
	const pbRow = dbRows(ctx, "SELECT id FROM playbooks WHERE slug = ?", [slug])[0] || {};
	H.check("查得到命名空间 id（直插老回执用）", typeof nsRow.id === "string" && nsRow.id.length > 0, JSON.stringify(nsRow));
	H.check("查得到包 id（直插老回执用）", typeof pbRow.id === "string" && pbRow.id.length > 0, JSON.stringify(pbRow));
	const id = `ins_e2e_561_${release}`;
	const receipt = {
		apiVersion: V.API_VERSION,
		kind: "InstallReceipt",
		playbook: { name: slug, version: 1 },
		install: { id, at: "2026-01-01T00:00:00Z", namespace: NS, release, dryRun: false },
		namespace: NS,
		values: VALUES,
		valuesDigest: digest,
		created: { capabilities: ["legacy561-review@1"], sources: [] },
		skipped: { capabilities: [], sources: [] },
	};
	const now = Math.floor(Date.now() / 1000);
	V.withDB(ctx.dbPath, (db) =>
		db
			.prepare(
				`INSERT INTO playbook_installs
				   (id, namespace_id, namespace_slug, playbook_id, playbook_slug, "values", values_digest,
				    receipt, source, release, created_at, uninstalled_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?)`,
			)
			.run(id, nsRow.id, NS, pbRow.id, slug, JSON.stringify(VALUES), digest, JSON.stringify(receipt), release, now, now),
	);
	return id;
}

function capsOnlyBundle(slug, cap, script) {
	return {
		"playbook.yaml": entryDoc(slug, ["capabilities/review.yaml"], []),
		"capabilities/review.yaml": capabilityDoc(cap, ["#!/bin/sh", `echo ${script}`]),
	};
}

function twoCapBundle(slug, absent, conflict, script) {
	return {
		"playbook.yaml": entryDoc(slug, ["capabilities/absent.yaml", "capabilities/conflict.yaml"], []),
		"capabilities/absent.yaml": capabilityDoc(absent, ["#!/bin/sh", "echo", "absent-v1"]),
		"capabilities/conflict.yaml": capabilityDoc(conflict, ["#!/bin/sh", `echo ${script}`]),
	};
}

async function partRestore(ctx) {
	console.log("--- 15. 按快照重装：pinned 版本 / 四种 409 / 审计（#561）---");

	const slug = "pb-restore-pin";
	const cap = "restore-pin-review";
	await createPackage(ctx, slug, capsOnlyBundle(slug, cap, "pin-v1"));
	const first = await install(ctx, slug, "pinned");
	V.expectStatus("① 装 release pinned → 200", first, 200);
	const rcpt = first.json || {};
	const installId = rcpt.install?.id;
	const digest = rcpt.valuesDigest;
	const pinDigest = rcpt.created?.pinned?.[0]?.digest || "";
	H.eq("① 安装建出的能力是 v1", rcpt.created?.capabilities, [`${cap}@1`]);
	H.eq("① 回执 created.pinned 钉的就是 v1", rcpt.created?.pinned?.[0]?.version, 1);
	const liveDetail = await readReleaseDetail(ctx, slug, "pinned", "① 生效中的详情");
	H.check(
		"① 没复原过时详情没有 restores 键（生效中响应逐字不变）",
		!Object.prototype.hasOwnProperty.call(liveDetail, "restores"),
		JSON.stringify(Object.keys(liveDetail)),
	);

	const bump = await H.req(ctx.base, V.P.capabilities(NS), {
		session: ctx.session,
		method: "POST",
		body: {
			apiVersion: V.API_VERSION,
			kind: "Capability",
			metadata: { name: cap },
			spec: { run: { entrypoint: "run.sh", files: { "run.sh": "#!/bin/sh\necho pin-v2\n" } } },
		},
	});
	V.expectStatus("① 同名能力换正文提交 → 201", bump, 201);
	H.eq("① 换正文之后最新版是 v2（与 pinned 的 v1 不同）", bump.json?.metadata?.version, 2);

	V.expectStatus("① 卸载 release pinned → 200", await uninstall(ctx, slug, "pinned"), 200);
	V.expectReason(
		"① 卸载后 pinned 那一版按版本读 → 404（卸载只是软删）",
		await H.req(ctx.base, V.P.capabilityVersion(NS, cap, 1), { session: ctx.session }),
		404,
		"NotFound",
	);

	const back = await restoreRelease(ctx, slug, "pinned", digest);
	V.expectStatus("① 按快照复原 → 200", back, 200);
	const re = back.json || {};
	H.eq("① 重建的是 pinned 版本 v1（不是最新的 v2）", re.created?.capabilities, [`${cap}@1`]);
	H.eq("① 复原没有复用任何能力", re.skipped?.capabilities, []);
	H.eq("① restoredFrom 就是原 install id", re.restoredFrom, installId);
	H.check("① 复原产生新回执（install.id 换新）", typeof re.install?.id === "string" && re.install.id !== installId, `${installId} vs ${re.install?.id}`);
	H.eq("① 复原回执的 valuesDigest 沿用原回执", re.valuesDigest, digest);
	H.eq("① 复原回执的 release 沿用原 release", re.install?.release, "pinned");
	H.eq("① 复原回执 created.pinned 的 digest 与原快照逐字相同", re.created?.pinned?.[0]?.digest, pinDigest);

	const v1 = await H.req(ctx.base, `${V.P.capabilityVersion(NS, cap, 1)}?includeBody=true`, { session: ctx.session });
	V.expectStatus("① 复原后按版本读 v1 → 200", v1, 200);
	H.eq("① 复原后 v1 的版本号还是 1", v1.json?.metadata?.version, 1);
	const v1Body = JSON.stringify(v1.json?.spec?.run?.files || {});
	H.check("① 复原后 v1 的正文就是 pin-v1", v1Body.includes("echo pin-v1"), v1Body);
	const v2 = await H.req(ctx.base, `${V.P.capabilityVersion(NS, cap, 2)}?includeBody=true`, { session: ctx.session });
	V.expectStatus("① 对照：按版本读 v2 → 200", v2, 200);
	H.check(
		"① 对照：v2 的正文是 pin-v2（两版真的不同，复原没拿它当来源）",
		JSON.stringify(v2.json?.spec?.run?.files || {}).includes("echo pin-v2"),
		JSON.stringify(v2.json?.spec?.run?.files || {}),
	);
	const latest = await H.req(ctx.base, V.P.capability(NS, cap), { session: ctx.session });
	V.expectStatus("① 复原后裸 name 读 → 200", latest, 200);
	H.eq("① 复原不抢最新版：裸 name 仍是 v2", latest.json?.status?.version, 2);

	const detail = await readReleaseDetail(ctx, slug, "pinned", "⑦ 复原后的详情");
	const restores = Array.isArray(detail.restores) ? detail.restores : null;
	H.eq("⑦ 复原后详情 restores[] 有 1 条", restores?.length, 1);
	const line = restores?.[0] || {};
	H.eq("⑦ 审计行的 action", line.action, "install.restored");
	H.eq("⑦ 审计行 restoredFrom = 原 installId", line.restoredFrom, installId);
	H.eq("⑦ 审计行 installId = 这次复原的新回执", line.installId, re.install?.id);
	H.check(
		"⑦ 审计行 recreated.capabilities ≥ 1",
		Number.isInteger(line.recreated?.capabilities) && line.recreated.capabilities >= 1,
		JSON.stringify(line.recreated),
	);
	H.check("⑦ 审计行 actor 非空", typeof line.actor === "string" && line.actor.length > 0, String(line.actor));
	H.check("⑦ 审计行 at 是能 Date.parse 的 RFC3339", typeof line.at === "string" && !Number.isNaN(Date.parse(line.at)), String(line.at));
	const audit = auditRows(ctx, slug, "pinned");
	H.eq("⑦ playbook_install_audit 落 1 行", audit.length, 1);
	H.eq("⑦ 落库那行的 action", audit[0]?.action, "install.restored");
	H.eq("⑦ 落库那行的 install_id", audit[0]?.install_id, re.install?.id);
	H.eq("⑦ 落库那行的 actor_id 与读面同值", audit[0]?.actor_id, line.actor);

	const still = await expectZeroWrite(ctx, slug, "pinned", "② 生效中复原", () => restoreRelease(ctx, slug, "pinned", digest));
	V.expectReason("② 生效中复原 → 409", still, 409, "already_installed");

	const legacyDigest = "sha256:" + "0".repeat(64);
	seedLegacyReceiptRow(ctx, slug, "nosnap", legacyDigest);
	const noSnap = await expectZeroWrite(ctx, slug, "nosnap", "③ 老回执复原", () => restoreRelease(ctx, slug, "nosnap", legacyDigest));
	V.expectReason("③ 老回执（无 pinned）复原 → 409", noSnap, 409, "no_snapshot");
	H.eq("③ details.causes[0].field 指到 release", noSnap.json?.details?.causes?.[0]?.field, "release");

	const goneSlug = "pb-restore-gone";
	const goneFirst = "restore-gone-a";
	const goneCap = "restore-gone-b";
	await createPackage(ctx, goneSlug, {
		"playbook.yaml": entryDoc(goneSlug, ["capabilities/a.yaml", "capabilities/b.yaml"], []),
		"capabilities/a.yaml": capabilityDoc(goneFirst, ["#!/bin/sh", "echo gone-a1"]),
		"capabilities/b.yaml": capabilityDoc(goneCap, ["#!/bin/sh", "echo gone-b1"]),
	});
	const goneInst = await install(ctx, goneSlug, "gone");
	V.expectStatus("④ 装 release gone → 200", goneInst, 200);
	H.eq("④ 装出两个能力（b 在 pinned 的第二个槽位）", goneInst.json?.created?.capabilities, [`${goneFirst}@1`, `${goneCap}@1`]);
	const goneDigest = goneInst.json?.valuesDigest;
	V.expectStatus("④ 卸载 release gone → 200", await uninstall(ctx, goneSlug, "gone"), 200);
	const killed = V.withDB(ctx.dbPath, (db) =>
		db
			.prepare(
				`DELETE FROM capability_versions
				  WHERE namespace_id = (SELECT id FROM namespaces WHERE slug = ?) AND name = ? AND version = 1`,
			)
			.run(NS, goneCap).changes,
	);
	H.check("④ 硬删掉 pinned 能力版本那一行（模拟 GC，删的虽是 b）", Number(killed) >= 1, String(killed));
	const gone = await expectZeroWrite(ctx, goneSlug, "gone", "④ pinned 版本被硬删", () => restoreRelease(ctx, goneSlug, "gone", goneDigest));
	V.expectReason("④ pinned 版本被硬删 → 409", gone, 409, "pinned_gone");
	const goneCause = gone.json?.details?.causes?.[0] || {};
	H.eq("④ causes[0].field 指到坏的那个 pinned 能力（第二个槽位，不是 0）", goneCause.field, "pinned.capabilities[1]");
	H.check(
		"④ causes[0].message 点名 kind=capability / name / version",
		["kind=capability", `name=${goneCap}`, "version=1"].every((s) => String(goneCause.message).includes(s)),
		String(goneCause.message),
	);
	const goneNames = await listNames(ctx, V.P.capabilities(NS), "④ 复原失败后的能力");
	H.check("④ 复原失败没有把能力装回来（两个都不该在）", !goneNames.includes(goneCap) && !goneNames.includes(goneFirst), goneNames.join(","));

	const reuseSlug = "pb-restore-reuse";
	const reuseCap = "restore-reuse-review";
	await createPackage(ctx, reuseSlug, capsOnlyBundle(reuseSlug, reuseCap, "reuse-v1"));
	const reuseInst = await install(ctx, reuseSlug, "dup");
	V.expectStatus("⑤ 装 release dup → 200", reuseInst, 200);
	V.expectStatus("⑤ 卸载 release dup → 200", await uninstall(ctx, reuseSlug, "dup"), 200);
	const same = insertManualCapabilityRow(ctx, { name: reuseCap, version: 1 });
	H.eq("⑤ 手工那行与快照同 digest", same.digest, same.pinned);
	const reuseBack = await restoreRelease(ctx, reuseSlug, "dup", reuseInst.json?.valuesDigest);
	V.expectStatus("⑤ 同名同版本同正文 → 复原 200（复用）", reuseBack, 200);
	H.eq("⑤ skipped.capabilities 里有那一个对象", reuseBack.json?.skipped?.capabilities, [`${reuseCap}@1`]);
	H.eq("⑤ created.capabilities 为空（一个都没重建）", reuseBack.json?.created?.capabilities, []);
	H.eq("⑤ skipped.pinned 的 digest 取自快照", reuseBack.json?.skipped?.pinned?.[0]?.digest, same.pinned);

	const cSlug = "pb-restore-conflict";
	const cCap = "restore-conflict-review";
	const cAbsent = "restore-conflict-absent";
	await createPackage(ctx, cSlug, twoCapBundle(cSlug, cAbsent, cCap, "conflict-v1"));
	const cInst = await install(ctx, cSlug, "dup");
	V.expectStatus("① 对照：装 release dup → 200", cInst, 200);
	H.eq(
		"① 对照：两能力包各建出一个（含缺席那个，它就是「本该新建」的那一半）",
		(cInst.json?.created?.capabilities || []).slice().sort(),
		[`${cAbsent}@1`, `${cCap}@1`],
	);
	V.expectStatus("① 对照：卸载 release dup → 200", await uninstall(ctx, cSlug, "dup"), 200);
	const diff = insertManualCapabilityRow(ctx, { name: cCap, version: 1, script: "conflict-v2" });
	H.check("① 对照：手工那行的 digest 与快照不同", diff.digest !== diff.pinned, `${diff.digest} vs ${diff.pinned}`);
	const conflict = await expectZeroWrite(ctx, cSlug, "dup", "① 同名同版本异正文复原", () => restoreRelease(ctx, cSlug, "dup", cInst.json?.valuesDigest));
	V.expectReason("① 对照：同名同版本但正文不同 → 409", conflict, 409, "restore_conflict");
	const cCause = conflict.json?.details?.causes?.[0] || {};
	H.eq("① 对照：causes[0].field 指到坏的那个槽位（冲突的是第二个能力）", cCause.field, "pinned.capabilities[1]");
	H.check(
		"① 对照：message 同时给库里与快照两个 digest",
		String(cCause.message).includes(diff.digest) && String(cCause.message).includes(diff.pinned),
		String(cCause.message),
	);
	const cNames = await listNames(ctx, V.P.capabilities(NS), "① 对照：冲突复原后的能力");
	H.check("① 对照：409 之后缺席那个能力没有被写一半（不在列表里）", !cNames.includes(cAbsent), cNames.join(","));

	const srcSlug = "pb-restore-src";
	const srcCap = "restore-src-review";
	const srcName = "restore-src-push";
	await createPackage(ctx, srcSlug, {
		"playbook.yaml": entryDoc(srcSlug, ["capabilities/review.yaml"], ["sources/on-push.yaml"], 1),
		"capabilities/review.yaml": capabilityDoc(srcCap, []),
		"sources/on-push.yaml": sourceDoc(srcName, [srcCap]),
	});
	const srcInst = await install(ctx, srcSlug, "nosrc");
	V.expectStatus("⑥ 装 release nosrc → 200", srcInst, 200);
	H.eq("⑥ 装出 1 个源", srcInst.json?.created?.sources, [srcName]);
	const srcBefore = await H.req(ctx.base, V.P.source(NS, srcName), { session: ctx.session });
	V.expectStatus("⑥ 卸载前读一次那个源 → 200", srcBefore, 200);
	const srcBeforeShape = sourceShapeOf(srcBefore.json);
	V.expectStatus("⑥ 卸载 release nosrc → 200", await uninstall(ctx, srcSlug, "nosrc"), 200);
	V.expectReason(
		"⑥ 卸载之后读那个源 → 404（确实是物理删，不是软删）",
		await H.req(ctx.base, V.P.source(NS, srcName), { session: ctx.session }),
		404,
		"NotFound",
	);
	const srcBack = await restoreRelease(ctx, srcSlug, "nosrc", srcInst.json?.valuesDigest);
	V.expectStatus("⑥ 缺源复原 → 200（按钉住的包版本 + 回执 values 重算重建）", srcBack, 200);
	const srcRe = srcBack.json || {};
	H.eq("⑥ created.sources 里有那个源", srcRe.created?.sources, [srcName]);
	H.eq("⑥ skipped.sources 为空（库里没有同名源可复用）", srcRe.skipped?.sources, []);
	const srcPin = (srcRe.created?.pinned || []).find((p) => p?.kind === "source" && p?.name === srcName) || {};
	H.eq("⑥ created.pinned 里的源那一项带 kindOf=webhook", srcPin.kindOf, "webhook");
	H.check("⑥ 源那一项不带 version（源没有版本）", !("version" in srcPin), JSON.stringify(srcPin));
	const srcAfter = await H.req(ctx.base, V.P.source(NS, srcName), { session: ctx.session });
	V.expectStatus("⑥ 复原后再读那个源 → 200", srcAfter, 200);
	H.eq(
		"⑥ 复原出的源与卸载前逐字段一致（token 除外：服务端重新生成）",
		sourceShapeOf(srcAfter.json),
		srcBeforeShape,
	);
	const srcRow =
		dbRows(
			ctx,
			`SELECT installed_from FROM sources
			  WHERE namespace_id = (SELECT id FROM namespaces WHERE slug = ?) AND name = ?`,
			[NS, srcName],
		)[0] || {};
	H.eq("⑥ 重建的源归属这次复原（installed_from = 新回执 install.id）", srcRow.installed_from, srcRe.install?.id);

	const deadSlug = "pb-restore-src-dead";
	const deadCap = "restore-src-dead-review";
	const deadSrc = "restore-src-dead-push";
	await createPackage(ctx, deadSlug, {
		"playbook.yaml": entryDoc(deadSlug, ["capabilities/review.yaml"], ["sources/on-push.yaml"], 1),
		"capabilities/review.yaml": capabilityDoc(deadCap, []),
		"sources/on-push.yaml": sourceDoc(deadSrc, [deadCap]),
	});
	const deadInst = await install(ctx, deadSlug, "gone");
	V.expectStatus("⑥ 对照：装 release gone → 200", deadInst, 200);
	V.expectStatus("⑥ 对照：卸载 release gone → 200", await uninstall(ctx, deadSlug, "gone"), 200);
	const wiped = V.withDB(ctx.dbPath, (db) => {
		const del = db
			.prepare(
				`DELETE FROM playbook_versions
				  WHERE playbook_id = (SELECT id FROM playbooks
				                        WHERE namespace_id = (SELECT id FROM namespaces WHERE slug = ?) AND slug = ?)
				    AND version = 1`,
			)
			.run(NS, deadSlug).changes;
		const bump = db
			.prepare(
				`UPDATE playbooks SET version = 2
				  WHERE namespace_id = (SELECT id FROM namespaces WHERE slug = ?) AND slug = ?`,
			)
			.run(NS, deadSlug).changes;
		return { del: Number(del), bump: Number(bump) };
	});
	H.check("⑥ 删掉钉住的包版本那一行（模拟 GC）", wiped.del >= 1, JSON.stringify(wiped));
	H.check("⑥ 把当前版本推走（否则还能拿当前 files 重算）", wiped.bump >= 1, JSON.stringify(wiped));
	const deadBack = await expectZeroWrite(ctx, deadSlug, "gone", "⑥ 钉住的正文没了复原", () =>
		restoreRelease(ctx, deadSlug, "gone", deadInst.json?.valuesDigest),
	);
	V.expectReason("⑥ 钉住的正文没了复原 → 409", deadBack, 409, "pin_missing_source");
	const srcCause = deadBack.json?.details?.causes?.[0] || {};
	H.eq("⑥ causes[0].field 指到 pinned.sources[0]", srcCause.field, "pinned.sources[0]");
	H.check(
		"⑥ causes[0].message 点名 kind=source / name 与钉住的版本",
		["kind=source", `name=${deadSrc}`, "v1"].every((s) => String(srcCause.message).includes(s)),
		String(srcCause.message),
	);

	V.expectReason("缺 valuesDigest → 400", await restoreRelease(ctx, slug, "pinned", ""), 400, "value_invalid");
	V.expectReason(
		"未知字段 → 400 unknown_field",
		await H.req(ctx.base, PB.restore(slug, "pinned"), { session: ctx.session, method: "POST", body: { valuesDigest: digest, bogus: 1 } }),
		400,
		"unknown_field",
	);
	const ghost = await expectZeroWrite(ctx, slug, "pinned", "未知 release 复原", () => restoreRelease(ctx, slug, "ghost", digest));
	V.expectReason("未知 release → 404", ghost, 404, "release_not_found");
	const ghostDigest = await expectZeroWrite(ctx, slug, "pinned", "错 valuesDigest 复原", () =>
		restoreRelease(ctx, slug, "pinned", "sha256:" + "ab".repeat(32)),
	);
	V.expectReason("该 release 没有这组 valuesDigest → 404", ghostDigest, 404, "release_not_found");

	H.check(
		"#561 按快照重装：pinned 版本复原 / 同内容复用 / 四类 409 与 0 写 / 审计 1 行",
		JSON.stringify(re.created?.capabilities) === JSON.stringify([`${cap}@1`]) &&
			JSON.stringify(reuseBack.json?.skipped?.capabilities) === JSON.stringify([`${reuseCap}@1`]) &&
			restores?.length === 1 &&
			audit.length === 1,
		[`created=${JSON.stringify(re.created?.capabilities)}`, `skipped=${JSON.stringify(reuseBack.json?.skipped?.capabilities)}`, `restores=${restores?.length}`, `audit=${audit.length}`].join(" / "),
	);
}

async function main() {
	H.startTimer();
	console.log("=== SUITE: playbook_release（#505 安装 release 身份 / #510 按 release 卸载 / #511 包版本历史 / #534 依赖锁 / #548 实例详情 / #553 安装历史 / #561 按快照重装）===");
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
		await partInstalledSnapshot(ctx);
		await partReleaseDetail(ctx);
		await partInstallHistory(ctx);
		await partVersions(ctx);
		await partRequires(ctx);
		await partRestore(ctx);
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
