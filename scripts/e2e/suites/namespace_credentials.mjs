#!/usr/bin/env node


import * as H from "../lib/harness.mjs";
import * as V from "../lib/v4.mjs";

const API_VERSION = "anotify/v1";
const seg = (v) => encodeURIComponent(String(v));

const secretsPath = (ns) => `/v1/namespaces/${seg(ns)}/secrets`;
const secretPath = (ns, name) => `${secretsPath(ns)}/${seg(name)}`;
const sourcesPath = (ns) => `/v1/namespaces/${seg(ns)}/sources`;
const sourcePath = (ns, name) => `${sourcesPath(ns)}/${seg(name)}`;
const secretAuditPath = (ns, name) => `${secretPath(ns, name)}/audit`;

const rand = () => Math.random().toString(36).slice(2, 10);
const secretValue = (tag) => `tok-e2e-${tag}-${rand()}`;

function pollConfig(extra = {}) {
	return {
		url: "https://example.com/feed",
		schedule: { expr: "0 0 1 1 *" },
		...extra,
	};
}

function sourceBody(name, config) {
	return {
		apiVersion: API_VERSION,
		kind: "Source",
		metadata: { name },
		spec: { kind: "poll", enabled: true, targets: [], config },
	};
}

function expectAnon401(name, res) {
	H.eq(name, res.status, 401);
	const err = res.json?.error;
	H.check(
		`${name} → 正文是 {error:"未登录"}（身份域冻结形状）`,
		err === "未登录",
		`${res.status} ${String(res.text).slice(0, 120)}`,
	);
	return res;
}

async function main() {
	console.log("=== SUITE: namespace_credentials（命名空间凭据 CRUD · #554）===");
	H.startTimer();
	const server = await H.startServer({
		suiteName: "namespace_credentials",
		rpId: "localhost",
	});
	const B = server.base;
	const { session } = H.seed(server.dbPath, "secrets");

	const ns = await H.defaultNamespace(B, session);
	H.check("解析默认命名空间（slug + id）", !!(ns && ns.slug && ns.id), JSON.stringify(ns));
	if (!ns) throw new Error("无法解析默认命名空间（GET /v1/namespaces 列表信封）");
	const NS = ns.slug;

	const bodies = [];
	const call = async (path, { anon = false, ...opts } = {}) => {
		const res = await H.req(B, path, anon ? opts : { session, ...opts });
		bodies.push(res.text || "");
		return res;
	};
	const L = () => secretsPath(NS);
	const S = (name) => secretPath(NS, name);

	const v1 = secretValue("gh1");
	const v2 = secretValue("gh2");
	const legacy = secretValue("legacy");

	const empty = await call(L());
	V.expectStatus("A1 空列表 → 200", empty, 200);
	H.check(
		"A2 列表是**非信封**文档（没有 apiVersion/kind 两键）",
		!!empty.json && !("apiVersion" in empty.json) && !("kind" in empty.json),
		V.brief(empty),
	);
	H.check(
		"A3 空列表的 items 是 []（不是 null）",
		Array.isArray(empty.json?.items) && JSON.stringify(empty.json.items) === "[]",
		JSON.stringify(empty.json),
	);

	const created = await call(S("gh-token"), { method: "PUT", body: { value: v1 } });
	V.expectStatus("B1 PUT 新建凭据 → 200", created, 200);
	H.eq("B2 读投影的 name 就是路径里的名字", created.json?.name, "gh-token");
	H.check(
		"B3 updatedAt 是 RFC3339（UTC）",
		/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/.test(String(created.json?.updatedAt || "")),
		String(created.json?.updatedAt),
	);
	H.check(
		"B4 未被引用时 usedBy 是 []（不是 null）",
		Array.isArray(created.json?.usedBy) && created.json.usedBy.length === 0,
		JSON.stringify(created.json?.usedBy),
	);
	H.check(
		"B5 读投影**恰好**三键（没有 value 键）",
		JSON.stringify(Object.keys(created.json || {}).sort()) ===
			JSON.stringify(["name", "updatedAt", "usedBy"]),
		JSON.stringify(Object.keys(created.json || {})),
	);
	H.check("B6 PUT 响应正文里没有值", !created.text.includes(v1), "响应正文含明文值");

	const one = await call(S("gh-token"));
	V.expectStatus("B7 GET 单条 → 200", one, 200);
	H.check(
		"B8 单条读回同一份投影（无 value 键）",
		!!one.json && !("value" in one.json) && one.json.name === "gh-token",
		V.brief(one),
	);
	H.check("B9 单条读的正文里没有值", !one.text.includes(v1), "响应正文含明文值");

	const listed = await call(L());
	V.expectStatus("B10 GET 列表 → 200", listed, 200);
	const items = V.items(listed);
	H.check(
		"B11 列表 items 里有刚建的凭据",
		Array.isArray(items) && items.some((it) => it?.name === "gh-token"),
		JSON.stringify(items),
	);
	const row = (items || []).find((it) => it?.name === "gh-token") || {};
	H.check(
		"B12 列表项字段与单条读一致（恰好三键）",
		JSON.stringify(Object.keys(row).sort()) ===
			JSON.stringify(["name", "updatedAt", "usedBy"]),
		JSON.stringify(Object.keys(row)),
	);
	H.check("B13 列表正文里没有值", !listed.text.includes(v1), "响应正文含明文值");
	H.eq("B14 单条与列表的 updatedAt 是同一个事实", row.updatedAt, one.json?.updatedAt);

	const srcName = "poll-src";
	const madeSource = await call(sourcesPath(NS), {
		body: sourceBody(
			srcName,
			pollConfig({
				secretRef: { name: "gh-token", header: "Authorization", prefix: "Bearer " },
			}),
		),
	});
	V.expectStatus("C1 建 poll 源（config 只带 secretRef）→ 201", madeSource, 201);

	const oneRef = await call(S("gh-token"));
	V.expectStatus("C2 被引用后 GET 单条 → 200", oneRef, 200);
	H.check(
		"C3 单条 usedBy 含引用它的源名",
		Array.isArray(oneRef.json?.usedBy) && oneRef.json.usedBy.includes(srcName),
		JSON.stringify(oneRef.json?.usedBy),
	);
	const listRef = await call(L());
	const refRow = (V.items(listRef) || []).find((it) => it?.name === "gh-token") || {};
	H.check(
		"C4 列表的 usedBy 与单条同源（两个读面一致）",
		JSON.stringify(refRow.usedBy || []) === JSON.stringify(oneRef.json?.usedBy || []),
		JSON.stringify({ list: refRow.usedBy, single: oneRef.json?.usedBy }),
	);

	const blocked = await call(S("gh-token"), { method: "DELETE" });
	V.expectReason("C5 被引用时 DELETE → 409", blocked, 409, "secret_in_use");
	const causes = blocked.json?.details?.causes || [];
	H.check("C6 409 带 details.causes[]", causes.length > 0, V.brief(blocked));
	H.check(
		"C7 每条 cause 的 field 都是 source",
		causes.every((c) => c?.field === "source"),
		JSON.stringify(causes),
	);
	H.check(
		"C8 causes 的 message 点名引用它的源",
		causes.some((c) => String(c?.message || "").includes(srcName)),
		JSON.stringify(causes),
	);
	const stillThere = await call(S("gh-token"));
	V.expectStatus("C9 拒删之后凭据还在（GET → 200）", stillThere, 200);

	const cur = await call(sourcePath(NS, srcName));
	V.expectStatus("C10 读源取 CAS 令牌 → 200", cur, 200);
	const rv = cur.json?.metadata?.resourceVersion;
	H.check("C11 源带 CAS 令牌（rv1:…）", V.isResourceVersion(rv), String(rv));
	const patched = await call(sourcePath(NS, srcName), {
		method: "PATCH",
		body: {
			apiVersion: API_VERSION,
			kind: "Source",
			metadata: { name: srcName, resourceVersion: rv },
			spec: { kind: "poll", config: pollConfig() },
		},
	});
	V.expectStatus("C12 PATCH 源（config 换成不带 secretRef 的整份配置）→ 200", patched, 200);
	const afterPatch = await call(S("gh-token"));
	H.check(
		"C13 引用解除后 usedBy 回到 []",
		Array.isArray(afterPatch.json?.usedBy) && afterPatch.json.usedBy.length === 0,
		JSON.stringify(afterPatch.json?.usedBy),
	);
	const del = await call(S("gh-token"), { method: "DELETE" });
	H.eq("C14 没人引用后 DELETE → 204", del.status, 204);
	H.eq("C15 204 没有响应体", String(del.text || ""), "");
	V.expectReason("C16 删掉后 GET 单条 → 404", await call(S("gh-token")), 404, "secret_not_found");
	V.expectReason(
		"C17 再 DELETE 同一个 → 404（不存在不是幂等 204）",
		await call(S("gh-token"), { method: "DELETE" }),
		404,
		"secret_not_found",
	);
	const listGone = await call(L());
	H.check(
		"C18 列表里已经没有它",
		!(V.items(listGone) || []).some((it) => it?.name === "gh-token"),
		JSON.stringify(V.items(listGone)),
	);

	const badName = await call(S("-bad"), { method: "PUT", body: { value: v1 } });
	V.expectReason("D1 非法名字（-bad）PUT → 400", badName, 400, "value_invalid");
	const nameCauses = badName.json?.details?.causes || [];
	H.check(
		"D2 400 的 details.causes[] 点到 field=name",
		nameCauses.some((c) => c?.field === "name"),
		JSON.stringify(nameCauses),
	);
	const listAfterBad = await call(L());
	H.check(
		"D3 非法名字没有落库（列表里没有 -bad）",
		!(V.items(listAfterBad) || []).some((it) => it?.name === "-bad"),
		JSON.stringify(V.items(listAfterBad)),
	);

	const unknownField = await call(S("gh-token"), {
		method: "PUT",
		body: { value: v1, extra: 1 },
	});
	V.expectReason("D4 未知字段 → 400 unknown_field", unknownField, 400, "unknown_field");
	const missingValue = await call(S("gh-token"), { method: "PUT", body: {} });
	V.expectReason("D5 缺 value → 400 value_invalid", missingValue, 400, "value_invalid");
	H.check(
		"D6 缺 value 的 causes[] 点到 field=value",
		(missingValue.json?.details?.causes || []).some((c) => c?.field === "value"),
		JSON.stringify(missingValue.json?.details?.causes || []),
	);
	const blankValue = await call(S("gh-token"), { method: "PUT", body: { value: "" } });
	V.expectReason("D7 空 value → 400 value_invalid", blankValue, 400, "value_invalid");
	V.expectReason(
		"D8 被拒的写没有落库（GET 仍是 404）",
		await call(S("gh-token")),
		404,
		"secret_not_found",
	);

	const legacySrc = "legacy-src";
	const legacySource = await call(sourcesPath(NS), {
		body: sourceBody(
			legacySrc,
			pollConfig({
				secretRef: { name: "legacy-tok", header: "Authorization", prefix: "Bearer " },
				secret: legacy,
			}),
		),
	});
	V.expectStatus("E1 老路径（config.secret + secretRef）一次创建 → 201", legacySource, 201);
	H.check(
		"E2 源响应不含 config.secret 明文",
		!legacySource.text.includes(legacy),
		"源响应回显了 config.secret",
	);
	const legacyList = await call(L());
	const legacyRow = (V.items(legacyList) || []).find((it) => it?.name === "legacy-tok");
	H.check(
		"E3 老路径写的凭据出现在新读面（同一份 store 写入）",
		!!legacyRow,
		JSON.stringify(V.items(legacyList)),
	);
	H.check(
		"E4 老路径凭据的 usedBy 指向写它的源",
		(legacyRow?.usedBy || []).includes(legacySrc),
		JSON.stringify(legacyRow?.usedBy),
	);
	const legacyGet = await call(S("legacy-tok"));
	V.expectStatus("E5 老路径凭据可单条读 → 200", legacyGet, 200);
	H.check("E6 单条读不含老路径明文", !legacyGet.text.includes(legacy), "响应正文含明文值");
	V.expectReason(
		"E7 老路径凭据同样受引用保护（DELETE → 409 secret_in_use）",
		await call(S("legacy-tok"), { method: "DELETE" }),
		409,
		"secret_in_use",
	);
	const legacyCur = await call(sourcePath(NS, legacySrc));
	const legacyRv = legacyCur.json?.metadata?.resourceVersion;
	const legacyPatch = await call(sourcePath(NS, legacySrc), {
		method: "PATCH",
		body: {
			apiVersion: API_VERSION,
			kind: "Source",
			metadata: { name: legacySrc, resourceVersion: legacyRv },
			spec: {
				kind: "poll",
				config: pollConfig({ url: "https://example.com/other" }),
			},
		},
	});
	V.expectStatus("E8 换掉老路径源的 secretRef → 200", legacyPatch, 200);
	H.eq(
		"E9 老路径凭据解除引用后可删 → 204",
		(await call(S("legacy-tok"), { method: "DELETE" })).status,
		204,
	);

	expectAnon401("F1 匿名 GET 列表 → 401", await call(L(), { anon: true }));
	expectAnon401(
		"F2 匿名 GET 单条 → 401",
		await call(S("anon-probe"), { anon: true }),
	);
	expectAnon401(
		"F3 匿名 PUT → 401",
		await call(S("anon-probe"), { anon: true, method: "PUT", body: { value: v1 } }),
	);
	expectAnon401(
		"F4 匿名 DELETE → 401",
		await call(S("anon-probe"), { anon: true, method: "DELETE" }),
	);

	const rotCreate = await call(S("rot-token"), { method: "PUT", body: { value: v1 } });
	V.expectStatus("G1 PUT 新建（轮换前）→ 200", rotCreate, 200);
	const rotReplace = await call(S("rot-token"), { method: "PUT", body: { value: v2 } });
	V.expectStatus("G2 同名再 PUT（轮换）→ 200", rotReplace, 200);
	H.check("G3 轮换响应不含新值", !rotReplace.text.includes(v2), "响应正文含明文值");
	H.check("G4 轮换响应不含旧值", !rotReplace.text.includes(v1), "响应正文含明文值");
	H.check(
		"G5 轮换不改变读面形状（仍恰好三键）",
		JSON.stringify(Object.keys(rotReplace.json || {}).sort()) ===
			JSON.stringify(["name", "updatedAt", "usedBy"]),
		JSON.stringify(Object.keys(rotReplace.json || {})),
	);
	const rotGet = await call(S("rot-token"));
	H.check(
		"G6 轮换后单条读仍不含任何一版值",
		!rotGet.text.includes(v1) && !rotGet.text.includes(v2),
		"响应正文含明文值",
	);

	const meRes = await call("/v1/auth/me");
	V.expectStatus("AU1 GET /v1/auth/me → 200（actor 的逐字基准）", meRes, 200);
	const actorId = meRes.json?.id;
	H.check(
		"AU1 会话用户 id 非空",
		typeof actorId === "string" && actorId.length > 0,
		JSON.stringify(meRes.json),
	);

	const auName = "audit-a";
	const auV1 = secretValue("au1");
	const auV2 = secretValue("au2");
	V.expectStatus(
		"AU2 PUT 建 standalone 凭据 A → 200（审计 created）",
		await call(S(auName), { method: "PUT", body: { value: auV1 } }),
		200,
	);
	V.expectStatus(
		"AU3 同名再 PUT（轮换）→ 200（审计 rotated）",
		await call(S(auName), { method: "PUT", body: { value: auV2 } }),
		200,
	);

	const auditRes = await call(secretAuditPath(NS, auName));
	V.expectStatus("AU4 GET …/secrets/audit-a/audit → 200", auditRes, 200);
	const auRows = V.items(auditRes) || [];
	H.eq("AU4 审计 2 条（created + rotated）", auRows.length, 2);
	H.eq(
		"AU4 最新在前：actions = [rotated, created]",
		JSON.stringify(auRows.map((r) => r?.action)),
		JSON.stringify(["rotated", "created"]),
	);
	H.check(
		"AU4 每项恰好 4 键（id/action/actor/at，没有 detail、没有 value）",
		auRows.every(
			(r) => JSON.stringify(Object.keys(r || {}).sort()) === JSON.stringify(["action", "actor", "at", "id"]),
		),
		JSON.stringify(auRows.map((r) => Object.keys(r || {}))),
	);
	H.check(
		"AU4 两行 actor 都是会话用户 id（逐字相等）",
		auRows.every((r) => r?.actor === actorId),
		JSON.stringify(auRows.map((r) => r?.actor)),
	);
	H.check(
		"AU4 at 都是 RFC3339 且能被 Date.parse",
		auRows.every((r) => typeof r?.at === "string" && !Number.isNaN(Date.parse(r.at))),
		JSON.stringify(auRows.map((r) => r?.at)),
	);
	H.check(
		"AU4 items[0].at >= items[1].at（同秒时也成立）",
		Date.parse(auRows[0]?.at) >= Date.parse(auRows[1]?.at),
		`${auRows[0]?.at} vs ${auRows[1]?.at}`,
	);
	H.check(
		"AU4 两行 id 不同（一次操作一行）",
		!!auRows[0]?.id && auRows[0].id !== auRows[1]?.id,
		`${auRows[0]?.id} vs ${auRows[1]?.id}`,
	);
	H.check(
		"AU4 审计正文里没有写入的任何一版值",
		!auditRes.text.includes(auV1) && !auditRes.text.includes(auV2),
		"审计响应含明文值",
	);

	const auLimit1 = V.items(await call(secretAuditPath(NS, auName) + "?limit=1")) || [];
	H.eq("AU5 limit=1 → 恰好 1 条", auLimit1.length, 1);
	H.eq("AU5 limit=1 回的是最新那条（与全量第一条同 id）", auLimit1[0]?.id, auRows[0]?.id);

	const auBad = {};
	for (const qs of ["limit=0", "limit=201", "limit=-1", "limit=abc", "before=abc"]) {
		auBad[qs] = await call(secretAuditPath(NS, auName) + "?" + qs);
		V.expectReason(`AU6 ?${qs} → 400`, auBad[qs], 400, "BadRequest");
	}
	const auMsg = (qs) => String(auBad[qs]?.json?.message || "");
	H.eq("AU6 limit=0 的 message 点名 1..200 区间", auMsg("limit=0"), "limit must be between 1 and 200");
	H.eq("AU6 limit=201 的 message 与 limit=0 逐字相同（三个数字只有一个家）", auMsg("limit=201"), auMsg("limit=0"));
	H.eq("AU6 limit=abc 的 message 是「limit must be an integer」", auMsg("limit=abc"), "limit must be an integer");
	H.eq(
		"AU6 before=abc 的 message 是「before must be an RFC3339 timestamp」",
		auMsg("before=abc"),
		"before must be an RFC3339 timestamp",
	);

	const auEpoch = (await call(secretAuditPath(NS, auName) + "?before=1970-01-01T00:00:00Z")).json;
	H.check(
		"AU7 before=1970-01-01T00:00:00Z → 正文恰好 {\"items\":[]}",
		JSON.stringify(auEpoch) === '{"items":[]}',
		JSON.stringify(auEpoch),
	);
	const auFuture = V.items(await call(secretAuditPath(NS, auName) + "?before=" + encodeURIComponent("2999-01-01T00:00:00Z"))) || [];
	H.eq("AU7 before=<未来> 条数不变（严格早于口径不裁未来行）", auFuture.length, auRows.length);

	const nsAuditB = "aud" + Date.now().toString(36);
	V.expectStatus("AU8 建第二个命名空间 → 201", await V.createNamespace(B, session, nsAuditB, "审计隔离"), 201);
	const crossNsAudit = await call(secretAuditPath(nsAuditB, auName));
	V.expectReason("AU8 nsB 里没有该凭据 → 404", crossNsAudit, 404, "secret_not_found");
	const ghostAudit = await call(secretAuditPath(NS, "audit-ghost"));
	V.expectReason("AU8 本 ns 里不存在的凭据 → 404", ghostAudit, 404, "secret_not_found");
	H.eq(
		"AU8 两种 404 正文逐字节相同（跨 ns 与不存在同形，不泄露存在性）",
		crossNsAudit.text,
		ghostAudit.text,
	);
	V.expectStatus(
		"AU8 在 nsB 用老路径（config.secret，不写审计）造同名凭据 → 201",
		await call(sourcesPath(nsAuditB), {
			body: sourceBody(
				"audit-src-b",
				pollConfig({
					secretRef: { name: auName, header: "Authorization", prefix: "Bearer " },
					secret: auV1 + "-b",
				}),
			),
		}),
		201,
	);
	const bAudit = await call(secretAuditPath(nsAuditB, auName));
	V.expectStatus("AU8 nsB 的同名凭据有审计端点 → 200", bAudit, 200);
	H.check(
		"AU8 nsB 读到的是空的 {\"items\":[]}（同名不串号：变异②的 HTTP 探测器）",
		JSON.stringify(bAudit.json) === '{"items":[]}',
		JSON.stringify(bAudit.json),
	);

	const delName = "audit-del";
	const delV1 = secretValue("del1");
	const delV2 = secretValue("del2");
	V.expectStatus("AU9 建删除用例凭据 → 200", await call(S(delName), { method: "PUT", body: { value: delV1 } }), 200);
	V.expectStatus("AU9 轮换它 → 200", await call(S(delName), { method: "PUT", body: { value: delV2 } }), 200);
	const delBefore = V.items(await call(secretAuditPath(NS, delName))) || [];
	H.eq("AU9 删除前审计 2 条", delBefore.length, 2);
	H.eq("AU9 无人引用时 DELETE → 204", (await call(S(delName), { method: "DELETE" })).status, 204);
	V.expectReason(
		"AU9 删除后实体已不存在 → 审计端点同样 404（handler 先解析实体，契约 D4）",
		await call(secretAuditPath(NS, delName)),
		404,
		"secret_not_found",
	);
	const delRowsDB = V.withDB(server.dbPath, (db) =>
		db
			.prepare(
				`SELECT action FROM namespace_secret_audit WHERE namespace_id = ? AND name = ? ORDER BY rowid`,
			)
			.all(ns.id, delName)
			.map((r) => r.action),
	);
	H.check(
		"AU9 落盘证三动作齐全（created → rotated → deleted，deleted 行确实写了）",
		JSON.stringify(delRowsDB) === JSON.stringify(["created", "rotated", "deleted"]),
		JSON.stringify(delRowsDB),
	);

	expectAnon401("AU10 匿名 GET 凭据审计 → 401", await call(secretAuditPath(NS, auName), { anon: true }));

	const seenAudit = bodies.join("\n");
	const leakedAudit = [auV1, auV2, auV1 + "-b", delV1, delV2].filter((v) => seenAudit.includes(v));
	H.check(
		`AU11 全部响应 body（${bodies.length} 份，含审计响应）里 grep 不到任何新写入的值`,
		leakedAudit.length === 0,
		`泄露：${leakedAudit.join(", ")}`,
	);

	const seen = bodies.join("\n");
	const leaked = [v1, v2, legacy].filter((v) => seen.includes(v));
	H.check(
		`G7 全部响应 body（${bodies.length} 份）里 grep 不到任何一个写入的值`,
		leaked.length === 0,
		`泄露：${leaked.join(", ")}`,
	);
	H.check(
		"G8 扫描池非空转（响应里确实有凭据名）",
		seen.includes("gh-token") && seen.includes("rot-token") && bodies.length >= 20,
		`bodies=${bodies.length}`,
	);

	const passed = H.summary("namespace_credentials");
	server.stop();
	process.exit(passed ? 0 : 1);
}

main().catch(async (e) => {
	console.error(e);
	process.exit(1);
});
