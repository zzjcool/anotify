#!/usr/bin/env node
/* SUITE: namespace_secrets — 命名空间凭据 CRUD（#554）
 *
 * 被测面（`api/resource.yaml`「secrets（#554）」一节冻结的四条端点）：
 *   GET    /v1/namespaces/{ns}/secrets           → 200 **非信封** {items:[{name,updatedAt,usedBy}]}
 *   GET    /v1/namespaces/{ns}/secrets/{name}    → 200 读投影；不存在 404 reason=secret_not_found
 *   PUT    /v1/namespaces/{ns}/secrets/{name}    → 200 读投影（**永不回显 value**）
 *   DELETE /v1/namespaces/{ns}/secrets/{name}    → 204；被源引用 409 reason=secret_in_use
 *
 * 覆盖 issue #554 验收里**能由黑盒 HTTP 证明**的那几条（编号与 issue 对齐）：
 *   1  值只写不读：PUT/GET/列表/轮换的响应里只有 name/updatedAt/usedBy
 *       ⇒ A2/A3（空列表形状）、B1–B14（三键投影）、G1–G6（轮换不回显）
 *   3  被源引用不删：409 secret_in_use + causes 点名源；换掉源 secretRef 后 204，再读 404
 *       ⇒ C1–C19
 *   4  非法名字 → 400 且 details.causes[].field == "name" ⇒ D1–D3
 *   5  老路径 config.secret + secretRef 一次写入仍能创建（同一份 store 写入）
 *       ⇒ E1–E9
 *   6  明文扫描：本套件**所有响应 body** 拼起来 grep 不到任何一个写入的值 ⇒ G7–G9
 *   7  匿名 401（身份域冻结形状 {error:"未登录"}）⇒ F1–F4
 *       （401 的形状是身份域而不是资源 Status：session 中间件的 http.Error 写出，见
 *        docs/contracts/v4/AUTH-HANDOFF.md；断言方式与 api_contract 的 expectAnon401 同源。）
 *
 * 刻意**不**覆盖：『轮换对下一轮 poll 立即生效』——那需要真实到点的上游并把套件拖到分钟级，
 * 已由 `internal/server/resource_secret_test.go::TestSecretResourceRotationTakesEffectOnNextPoll`
 * 用真 Poller 打真 httptest 上游覆盖。本套件所有 poll 源的 schedule 都排到「1 月 1 日 0 点」，
 * 保证测试窗口内**不会**触发任何真实外网请求（也不等排期）。
 *
 * 明文口径：每条写进去的值都是 `tok-e2e-<tag>-<随机>` 且互不相同；随机后缀让「哪条断言
 * 意外把值写进了响应」不会因为长得像别的测试常量而被扫描放过。
 */
import * as H from "../lib/harness.mjs";
import * as V from "../lib/v4.mjs";

const API_VERSION = "anotify/v1";
const seg = (v) => encodeURIComponent(String(v));

// 凭据路径（v4 的 ns 在**路径**里；凭据不是信封资源，lib/v4.mjs 的 P 表里没有它们）。
const secretsPath = (ns) => `/v1/namespaces/${seg(ns)}/secrets`;
const secretPath = (ns, name) => `${secretsPath(ns)}/${seg(name)}`;
const sourcesPath = (ns) => `/v1/namespaces/${seg(ns)}/sources`;
const sourcePath = (ns, name) => `${sourcesPath(ns)}/${seg(name)}`;

// 凭据值：形状固定（可 grep）、后缀随机（互不相同）、长度远短于 check-secrets 的
// 「Bearer/token 后跟 ≥32 字符随机串」判据 —— 它是夹具，不是秘密。
const rand = () => Math.random().toString(36).slice(2, 10);
const secretValue = (tag) => `tok-e2e-${tag}-${rand()}`;

// poll 源的 config：schedule 排到 1 月 1 日 0 点，本套件窗口内绝不触发真实请求。
// url 用 example.com（公网 https，过 SSRF 判据又不依赖本机任何端口）。
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

// expectAnon401 钉住「会话层未登录」的冻结形状：401 + `{error:"未登录"}`。
// 与 api_contract.mjs 的同名 helper 同源（身份域的错误体不是资源 Status，没有 reason）。
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
	console.log("=== SUITE: namespace_secrets（命名空间凭据 CRUD · #554）===");
	H.startTimer();
	const server = await H.startServer({
		suiteName: "namespace_secrets",
		rpId: "localhost",
	});
	const B = server.base;
	const { session } = H.seed(server.dbPath, "secrets");

	const ns = await H.defaultNamespace(B, session);
	H.check("解析默认命名空间（slug + id）", !!(ns && ns.slug && ns.id), JSON.stringify(ns));
	if (!ns) throw new Error("无法解析默认命名空间（GET /v1/namespaces 列表信封）");
	const NS = ns.slug;

	// bodies 是本套件**所有响应 body** 的池子：验收 6 的明文扫描扫的就是它。
	// 收进 call() 而不是在末尾重放一遍，是为了让「某个中间步骤泄露了值」也被抓到
	// （只扫最终读面会漏掉错误体 / 源响应 / 409 这些面）。
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

	// ---------- A. 空列表：非信封 + items 是 []（不是 null）----------
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

	// ---------- B. PUT 创建 → 读投影只有 name/updatedAt/usedBy（验收 1/4）----------
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

	// ---------- C. secretRef 引用 → usedBy；被引用 DELETE 409（验收 3）----------
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

	// 换掉源的 secretRef（PATCH 对 poll 是整份 config 替换，带 CAS 令牌）→ 引用解除
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

	// ---------- D. 写面边界（验收 4 + 语义 5：名字规则与 secretRef 同一份）----------
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

	// ---------- E. 老路径 config.secret + secretRef 一次写入（验收 5）----------
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

	// ---------- F. 匿名一律 401（验收 7）----------
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

	// ---------- G. 轮换不回显 + 明文扫描总结（验收 1 / 验收 6）----------
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

	// 验收 6 的判据：本套件所有响应 body 拼起来 grep 不到任何一个写入的值。
	const seen = bodies.join("\n");
	const leaked = [v1, v2, legacy].filter((v) => seen.includes(v));
	H.check(
		`G7 全部响应 body（${bodies.length} 份）里 grep 不到任何一个写入的值`,
		leaked.length === 0,
		`泄露：${leaked.join(", ")}`,
	);
	// 反假绿：扫描必须在「真的有内容的池子」上跑。响应池里一定含凭据**名字**
	// （读面投影），所以「一条都搜不到」不可能是池子为空导致的假通过。
	H.check(
		"G8 扫描池非空转（响应里确实有凭据名）",
		seen.includes("gh-token") && seen.includes("rot-token") && bodies.length >= 20,
		`bodies=${bodies.length}`,
	);

	const passed = H.summary("namespace_secrets");
	server.stop();
	process.exit(passed ? 0 : 1);
}

main().catch(async (e) => {
	console.error(e);
	process.exit(1);
});
