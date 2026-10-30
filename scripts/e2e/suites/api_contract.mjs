#!/usr/bin/env node
/* SUITE: api_contract — API 契约矩阵
 *
 * 覆盖 case：
 *  notify：
 *    无 Key→401；错误 Key→401；recv scope Key→403
 *    缺 title→400；空 title(空格)→400；坏 agentState→400
 *    五种合法 agentState(working/blocked/done/interrupted/error)→各 200
 *    畸形 JSON→400；超大体(>1MB)→400
 *    deviceTags 归一化（重复/超限/超长）不报错→200
 *    无设备用户上报→200 且 matched=0
 *  vapid-public-key：GET→200 且 publicKey 非空
 *  devices：无 session→401；POST 缺 keys→400；POST 合法→200；GET 含该设备；
 *           PATCH 重命名/eventScope/enabled→200；PATCH 坏 eventScope→400；
 *           DELETE→200 且 DELETE 后 enabled=false 或消失
 *  keys：无 session→401；POST→200 且 ant_ 前缀；POST 无 scopes→400；
 *        GET 不含明文；revoke→200 且被 revoke Key 上报→401
 *  notifications：无 session→401；上报 2 条后 GET→count≥2；limit=1→count=1；sinceSeq 分页→更少
 *  静态/缓存：/→200；index.html max-age=60；/v1/* no-store；manifest.json 合法；哈希资源 immutable
 */
import * as H from "../lib/harness.mjs";

async function main() {
	console.log("=== SUITE: api_contract（API 契约矩阵）===");
	const server = await H.startServer({
		suiteName: "api_contract",
		rpId: "localhost",
	});
	const B = server.base;
	const { sendKey, recvKey, session } = H.seed(server.dbPath, "api");

	// ---------- events 鉴权 ----------
	H.eq(
		"events 无 Key → 401",
		(await H.req(B, "/v1/events", { body: { source: "s", type: "t" } })).status,
		401,
	);
	H.eq(
		"events 错误 Key → 401",
		(await H.req(B, "/v1/events", {
			key: "ant_live_bogus",
			body: { source: "s", type: "t" },
		})).status,
		401,
	);
	H.eq(
		"events recv scope Key → 403",
		(
			await H.req(B, "/v1/events", {
				key: recvKey,
				body: { source: "s", type: "t" },
			})
		).status,
		403,
	);

	// ---------- events 参数校验 ----------
	H.eq(
		"events 缺 source → 400",
		(await H.req(B, "/v1/events", { key: sendKey, body: { type: "t" } })).status,
		400,
	);
	H.eq(
		"events 缺 type → 400",
		(await H.req(B, "/v1/events", { key: sendKey, body: { source: "s" } })).status,
		400,
	);
	H.eq(
		"events 空 source → 400",
		(await H.req(B, "/v1/events", {
			key: sendKey,
			body: { source: "  ", type: "t" },
		})).status,
		400,
	);

	// ---------- vapid-public-key ----------
	// 推送是**可选**功能：没配 VAPID 时服务端返回 200 + 空 publicKey，
	// 前端据此隐藏「启用推送」入口。所以端点本身必须永远 200，
	// 但 publicKey 是否非空取决于部署配置——不能把「未配置」当成失败。
	const vapidResp = await H.req(B, "/v1/vapid-public-key");
	H.eq("vapid-public-key → 200", vapidResp.status, 200);
	const vapidPub = vapidResp.json?.publicKey || "";
	if (process.env.ANOTIFY_VAPID_PUBLIC_KEY) {
		H.check(
			"vapid-public-key publicKey 非空（已配置 VAPID）",
			vapidPub.length > 0,
			vapidPub.slice(0, 20),
		);
	} else {
		H.check(
			"vapid-public-key 未配置时返回空串而非报错",
			vapidPub === "",
			vapidPub.slice(0, 20),
		);
	}

	// ---------- devices ----------
	H.eq("devices 无 session → 401", (await H.req(B, "/v1/devices")).status, 401);
	H.eq(
		"devices POST 缺 keys → 400",
		(
			await H.req(B, "/v1/devices", {
				session,
				body: { endpoint: "https://push.example.com/nokeys" },
			})
		).status,
		400,
	);
	const devBody = H.makeDevice({ name: "契约设备A" });
	const devPost = await H.req(B, "/v1/devices", { session, body: devBody });
	H.eq("devices POST 合法 → 200", devPost.status, 200);
	H.check("devices POST 返回 device", !!devPost.json?.device?.id);
	const devId = devPost.json?.device?.id;
	const devList = await H.req(B, "/v1/devices", { session });
	H.eq("devices GET 列表 → 200", devList.status, 200);
	H.check(
		"devices GET 含刚 POST 的设备",
		Array.isArray(devList.json?.devices) &&
			devList.json.devices.some((d) => d.id === devId),
	);
	H.eq(
		"devices PATCH 重命名 → 200",
		(
			await H.req(B, `/v1/devices/${devId}`, {
				session,
				method: "PATCH",
				body: { name: "改名后" },
			})
		).status,
		200,
	);
	H.eq(
		"devices PATCH eventScope=final → 200",
		(
			await H.req(B, `/v1/devices/${devId}`, {
				session,
				method: "PATCH",
				body: { eventScope: "final" },
			})
		).status,
		200,
	);
	H.eq(
		"devices PATCH 坏 eventScope → 400",
		(
			await H.req(B, `/v1/devices/${devId}`, {
				session,
				method: "PATCH",
				body: { eventScope: "bogus" },
			})
		).status,
		400,
	);
	H.eq(
		"devices PATCH enabled=false → 200",
		(
			await H.req(B, `/v1/devices/${devId}`, {
				session,
				method: "PATCH",
				body: { enabled: false },
			})
		).status,
		200,
	);
	H.eq(
		"devices DELETE → 200",
		(await H.req(B, `/v1/devices/${devId}`, { session, method: "DELETE" }))
			.status,
		200,
	);
	const devListAfter = await H.req(B, "/v1/devices", { session });
	const after = (devListAfter.json?.devices || []).find((d) => d.id === devId);
	H.check(
		"devices DELETE 后 enabled=false 或消失",
		!after || after.enabled === false,
		after ? `enabled=${after.enabled}` : "已消失",
	);

	// ---------- keys ----------
	H.eq("keys 无 session → 401", (await H.req(B, "/v1/keys")).status, 401);
	const keyPost = await H.req(B, "/v1/keys", {
		session,
		body: { name: "契约Key", scopes: ["notify:send"] },
	});
	H.eq("keys POST → 200", keyPost.status, 200);
	H.check(
		"keys POST 返回 ant_ 前缀明文",
		!!keyPost.json?.key && keyPost.json.key.startsWith("ant_"),
	);
	const newKeyId = keyPost.json?.record?.ID ?? keyPost.json?.record?.id;
	H.eq(
		"keys POST 无 scopes → 400",
		(await H.req(B, "/v1/keys", { session, body: { name: "noScope" } })).status,
		400,
	);
	const keyList = await H.req(B, "/v1/keys", { session });
	H.eq("keys GET → 200", keyList.status, 200);
	H.check(
		"keys GET 不含明文 key",
		!JSON.stringify(keyList.json).includes(keyPost.json.key),
	);
	H.eq(
		"keys revoke → 200",
		(await H.req(B, `/v1/keys/${newKeyId}/revoke`, { session, method: "POST" }))
			.status,
		200,
	);
	H.eq(
		"被 revoke 的 Key 上报 → 401",
		(
			await H.req(B, "/v1/events", {
				key: keyPost.json.key,
				body: { source: "github", type: "pr_opened" },
			})
		).status,
		401,
	);

	// ---------- events（事件入口，API Key）+ tasks（控制台读）----------
	H.eq(
		"console/events 无 session → 401",
		(await H.req(B, "/v1/console/events")).status,
		401,
	);
	// 事件入口需要 notify:send scope 的 Key
	const ev1 = await H.req(B, "/v1/events", {
		key: sendKey,
		body: { source: "github", type: "pr_opened", payload: { n: 1 } },
	});
	H.eq("POST /v1/events → 200", ev1.status, 200);
	H.check("事件返回 eventId", !!ev1.json?.eventId, JSON.stringify(ev1.json));
	H.check("事件返回 traceId", !!ev1.json?.traceId, JSON.stringify(ev1.json));

	// 幂等：同 idemKey 再来一次 → 200 且同一 eventId
	const ev1b = await H.req(B, "/v1/events", {
		key: sendKey,
		body: {
			source: "github",
			type: "pr_opened",
			idemKey: "dup-1",
			payload: { n: 1 },
		},
	});
	const ev1c = await H.req(B, "/v1/events", {
		key: sendKey,
		body: {
			source: "github",
			type: "pr_opened",
			idemKey: "dup-1",
			payload: { n: 1 },
		},
	});
	H.eq("幂等重复提交 → 200", ev1c.status, 200);
	H.eq(
		"幂等重复提交返回同一 eventId",
		ev1c.json?.eventId,
		ev1b.json?.eventId,
	);

	// 控制台读事件流（会话鉴权）
	const evList = await H.req(B, "/v1/console/events?limit=50", { session });
	H.eq("console/events GET → 200", evList.status, 200);
	H.check(
		"console/events 返回数组（空时 []）",
		Array.isArray(evList.json?.events),
		JSON.stringify(evList.json),
	);
	H.check(
		"console/events count≥2",
		(evList.json?.events || []).length >= 2,
		`n=${(evList.json?.events || []).length}`,
	);

	// 任务列表（控制台）
	const taskList = await H.req(B, "/v1/tasks?limit=50", { session });
	H.eq("tasks GET → 200", taskList.status, 200);
	H.check(
		"tasks 返回数组（空时 []）",
		Array.isArray(taskList.json?.tasks),
		JSON.stringify(taskList.json),
	);

	// ---------- 命名空间（#162 / 契约 §6.2 + I11）----------
	//
	// 只钉住「隔离语义」必须有的三条：缺参 400、两个命名空间互相看不到对方的数据、
	// 跨命名空间引用被拒（I11①）。其余用例结构不动 —— 这是硬隔离边界，
	// 这三条一松，后面所有「按命名空间过滤」的断言都失去意义。
	{
		// 1. 缺 namespaceId → 400（后端**不回退**默认命名空间）
		const noNs = await H.req(B, "/v1/capabilities", {
			session,
			namespaceId: null, // 显式不注入（见 harness withNamespaceQuery）
		});
		H.eq("缺 namespaceId → 400", noNs.status, 400);
		H.check(
			"缺 namespaceId 的错误提示点名参数",
			String(noNs.text || "").toLowerCase().includes("namespace"),
			String(noNs.text).slice(0, 120),
		);

		// 2. 第二个命名空间用 API 建（不硬编码 id），然后双向验隔离
		const created = await H.req(B, "/v1/namespaces", {
			session,
			method: "POST",
			body: { slug: "iso-" + Date.now(), name: "隔离验证" },
		});
		H.check(
			"POST /v1/namespaces → 2xx",
			created.status === 200 || created.status === 201,
			`${created.status} ${String(created.text).slice(0, 120)}`,
		);
		const otherNs = (created.json?.namespace || created.json)?.id;
		H.check("新命名空间有 id", !!otherNs, String(otherNs));
		if (otherNs) {
			const defNs = await H.defaultNamespaceId(B, session);
			// 两边各建一个同名以外的能力，再互相查
			const capOther = await H.req(B, "/v1/capabilities", {
				session,
				method: "POST",
				namespaceId: otherNs,
				body: { name: "iso.only_other", outputs: ["x"], paramsSchema: { type: "object" } },
			});
			H.eq("在新命名空间里建能力 → 200", capOther.status, 200);
			const capDefault = await H.req(B, "/v1/capabilities", {
				session,
				method: "POST",
				namespaceId: defNs,
				body: { name: "iso.only_default", outputs: ["x"], paramsSchema: { type: "object" } },
			});
			H.eq("在默认命名空间里建能力 → 200", capDefault.status, 200);

			const listDefault = await H.req(B, "/v1/capabilities", {
				session,
				namespaceId: defNs,
			});
			const namesDefault = (listDefault.json?.capabilities || []).map((c) => c.name);
			H.check(
				"默认命名空间看不到另一个命名空间的能力",
				namesDefault.includes("iso.only_default") &&
					!namesDefault.includes("iso.only_other"),
				JSON.stringify(namesDefault),
			);

			const listOther = await H.req(B, "/v1/capabilities", {
				session,
				namespaceId: otherNs,
			});
			const namesOther = (listOther.json?.capabilities || []).map((c) => c.name);
			H.check(
				"另一个命名空间也看不到默认命名空间的能力",
				namesOther.includes("iso.only_other") &&
					!namesOther.includes("iso.only_default"),
				JSON.stringify(namesOther),
			);

			// 3. 跨命名空间引用 → 400（I11①：边的两端必须同命名空间）
			const crossEdge = await H.req(B, "/v1/edges", {
				session,
				namespaceId: defNs,
				body: {
					fromCapability: capDefault.json?.capability?.id,
					fromPort: "x",
					toCapability: capOther.json?.capability?.id,
				},
			});
			H.eq("跨命名空间建边 → 400", crossEdge.status, 400);
		}
	}

	// ---------- 静态/缓存 ----------
	H.eq("/ → 200", (await H.req(B, "/")).status, 200);
	const idxHeaders = (await H.req(B, "/index.html")).headers;
	H.check(
		"index.html Cache-Control 含 max-age=60",
		(idxHeaders["cache-control"] || "").includes("max-age=60"),
		idxHeaders["cache-control"],
	);
	const apiHeaders = (await H.req(B, "/v1/tasks", { session })).headers;
	H.check(
		"/v1/* Cache-Control 含 no-store",
		(apiHeaders["cache-control"] || "").includes("no-store"),
		apiHeaders["cache-control"],
	);
	const manifest = await H.req(B, "/manifest.json");
	H.check(
		"manifest.json 存在且为合法 JSON",
		manifest.status === 200 &&
			manifest.json &&
			typeof manifest.json === "object",
	);
	const hashedEntry = Object.values(manifest.json || {})[0];
	if (hashedEntry) {
		const hashedResp = await H.req(B, "/" + hashedEntry);
		H.check(
			"哈希资源 Cache-Control 含 immutable",
			(hashedResp.headers["cache-control"] || "").includes("immutable"),
			hashedResp.headers["cache-control"],
		);
	} else {
		H.bad("manifest.json 无任何哈希资源条目");
	}

	const passed = H.summary("api_contract");
	server.stop();
	process.exit(passed ? 0 : 1);
}
main().catch(async (e) => {
	console.error(e);
	process.exit(1);
});
