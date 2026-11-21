#!/usr/bin/env node
import * as H from "../lib/harness.mjs";
import * as V from "../lib/v4.mjs";

const SUITE = "playbook_release";
const NS = V.DEFAULT_NS;
const VALUES = { project: "demo" };

const PB = {
	list: () => V.P.playbooks(NS),
	install: (slug) => `${V.P.playbook(NS, slug)}/install`,
	render: (slug) => `${V.P.playbook(NS, slug)}/render`,
	installs: (slug) => `${V.P.playbook(NS, slug)}/installs`,
	installById: (slug, id) => `${V.P.playbook(NS, slug)}/installs/${encodeURIComponent(id)}`,
};

const VALUES_BLOCK = `    type: object
    required: [project]
    properties:
      project: { type: string, pattern: '^[a-z0-9][a-z0-9-]{0,20}$' }`;

function entryDoc(slug, caps, srcs) {
	const list = (key, paths) =>
		paths.length === 0
			? `  ${key}: []`
			: `  ${key}:\n${paths.map((p) => `    - ${p}`).join("\n")}`;
	return `apiVersion: anotify/v1
kind: Playbook
metadata:
  name: ${slug}
  displayName: 安装 release 身份 E2E
spec:
  values:
${VALUES_BLOCK}
${list("capabilities", caps)}
${list("sources", srcs)}
`;
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

async function main() {
	H.startTimer();
	console.log("=== SUITE: playbook_release（#505 安装 release 身份）===");
	const server = await H.startServer({ suiteName: SUITE, rpId: "localhost" });
	try {
		const seeded = H.seed(server.dbPath, "e2e");
		const ctx = { base: server.base, session: seeded.session, dbPath: server.dbPath };
		await partLegacyDefaults(ctx);
		await partMultiRelease(ctx);
		await partCrossReleaseConflict(ctx);
		await partTemplatePassthrough(ctx);
		await partInvalidRelease(ctx);
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
