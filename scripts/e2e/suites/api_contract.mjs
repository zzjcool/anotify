#!/usr/bin/env node
/* SUITE: api_contract — API 契约矩阵
 *
 * 覆盖 case：
 *  notify：
 *    无 Key→401；错误 Key→401；recv scope Key→403
 *    缺 title→400；空 title(空格)→400；坏 status→400
 *    五种合法 status(success/error/interrupted/info/warning)→各 200
 *    畸形 JSON→400；超大体(>1MB)→400
 *    deviceTags 归一化（重复/超限/超长）不报错→200
 *    无设备用户上报→200 且 matched=0
 *  vapid-public-key：GET→200 且 publicKey 非空
 *  devices：无 session→401；POST 缺 keys→400；POST 合法→200；GET 含该设备；
 *           PATCH 重命名/statusFilter/enabled→200；PATCH 坏 statusFilter→400；
 *           DELETE→200 且 DELETE 后 enabled=false 或消失
 *  keys：无 session→401；POST→200 且 ant_ 前缀；POST 无 scopes→400；
 *        GET 不含明文；revoke→200 且被 revoke Key 上报→401
 *  notifications：无 session→401；上报 2 条后 GET→count≥2；limit=1→count=1；sinceSeq 分页→更少
 *  静态/缓存：/→200；index.html max-age=60；/v1/* no-store；manifest.json 合法；哈希资源 immutable
 */
import * as H from "../lib/harness.mjs";

async function main() {
	console.log("=== SUITE: api_contract（API 契约矩阵）===");
	const server = await H.startServer({ suiteName: "api_contract", rpId: "localhost" });
	const B = server.base;
	const { sendKey, recvKey, session } = H.seed(server.dbPath, "api");

	// ---------- notify 鉴权 ----------
	H.eq(
		"notify 无 Key → 401",
		(await H.req(B, "/v1/notify", { body: { title: "t", status: "success" } }))
			.status,
		401,
	);
	H.eq(
		"notify 错误 Key → 401",
		(
			await H.req(B, "/v1/notify", {
				key: "ant_send_wrong_wrong",
				body: { title: "t", status: "success" },
			})
		).status,
		401,
	);
	H.eq(
		"notify recv scope Key → 403",
		(
			await H.req(B, "/v1/notify", {
				key: recvKey,
				body: { title: "t", status: "success" },
			})
		).status,
		403,
	);

	// ---------- notify 参数校验 ----------
	H.eq(
		"notify 缺 title → 400",
		(
			await H.req(B, "/v1/notify", {
				key: sendKey,
				body: { status: "success" },
			})
		).status,
		400,
	);
	H.eq(
		"notify 空 title(空格) → 400",
		(
			await H.req(B, "/v1/notify", {
				key: sendKey,
				body: { title: "   ", status: "success" },
			})
		).status,
		400,
	);
	H.eq(
		"notify 坏 status → 400",
		(
			await H.req(B, "/v1/notify", {
				key: sendKey,
				body: { title: "t", status: "bogus" },
			})
		).status,
		400,
	);
	for (const st of ["success", "error", "interrupted", "info", "warning"]) {
		H.eq(
			`notify status=${st} → 200`,
			(
				await H.req(B, "/v1/notify", {
					key: sendKey,
					body: { title: "t", status: st },
				})
			).status,
			200,
		);
	}
	H.eq(
		"notify 畸形 JSON → 400",
		(
			await H.req(B, "/v1/notify", {
				key: sendKey,
				body: "{not json",
				headers: { "Content-Type": "application/json" },
			})
		).status,
		400,
	);
	const bigBody = JSON.stringify({
		title: "t",
		status: "success",
		body: "x".repeat(2 * 1024 * 1024),
	});
	const bigResp = await H.req(B, "/v1/notify", {
		key: sendKey,
		body: bigBody,
		headers: { "Content-Type": "application/json" },
	});
	H.check(
		"notify 超大体(>1MB) → 400/413",
		bigResp.status === 400 || bigResp.status === 413,
		`got ${bigResp.status}`,
	);
	H.eq(
		"notify deviceTags 重复/超限/超长不报错 → 200",
		(
			await H.req(B, "/v1/notify", {
				key: sendKey,
				body: {
					title: "t",
					status: "success",
					deviceTags: [
						"a",
						"a",
						"b",
						...Array.from({ length: 20 }, (_, i) => "tag" + i),
						"x".repeat(50),
					],
				},
			})
		).status,
		200,
	);
	const noDevResp = await H.req(B, "/v1/notify", {
		key: sendKey,
		body: { title: "t", status: "success" },
	});
	H.eq("无设备用户上报 → 200", noDevResp.status, 200);
	H.eq("无设备用户上报 matched=0", noDevResp.json?.matched, 0);

	// ---------- vapid-public-key ----------
	const vapidResp = await H.req(B, "/v1/vapid-public-key");
	H.eq("vapid-public-key → 200", vapidResp.status, 200);
	H.check(
		"vapid-public-key publicKey 非空",
		!!vapidResp.json?.publicKey && vapidResp.json.publicKey.length > 0,
	);

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
		"devices PATCH statusFilter=error → 200",
		(
			await H.req(B, `/v1/devices/${devId}`, {
				session,
				method: "PATCH",
				body: { statusFilter: "error" },
			})
		).status,
		200,
	);
	H.eq(
		"devices PATCH 坏 statusFilter → 400",
		(
			await H.req(B, `/v1/devices/${devId}`, {
				session,
				method: "PATCH",
				body: { statusFilter: "bogus" },
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
			await H.req(B, "/v1/notify", {
				key: keyPost.json.key,
				body: { title: "t", status: "success" },
			})
		).status,
		401,
	);

	// ---------- notifications ----------
	H.eq(
		"notifications 无 session → 401",
		(await H.req(B, "/v1/notifications")).status,
		401,
	);
	await H.req(B, "/v1/notify", {
		key: sendKey,
		body: { title: "n1", status: "success" },
	});
	await H.req(B, "/v1/notify", {
		key: sendKey,
		body: { title: "n2", status: "error" },
	});
	const notifAll = await H.req(B, "/v1/notifications?limit=50", { session });
	H.eq("notifications GET → 200", notifAll.status, 200);
	H.check(
		"notifications count≥2",
		(notifAll.json?.count ?? 0) >= 2,
		`count=${notifAll.json?.count}`,
	);
	const notifLimit1 = await H.req(B, "/v1/notifications?limit=1", { session });
	H.eq("notifications limit=1 → count=1", notifLimit1.json?.count, 1);
	const maxSeq = Math.max(
		...(notifAll.json?.notifications || []).map((m) => m.Seq ?? m.seq ?? 0),
	);
	const notifSince = await H.req(
		B,
		`/v1/notifications?sinceSeq=${maxSeq - 1}`,
		{ session },
	);
	H.check(
		"notifications sinceSeq 分页返回更少",
		(notifSince.json?.count ?? 0) < (notifAll.json?.count ?? 0),
		`since=${notifSince.json?.count} all=${notifAll.json?.count}`,
	);

	// ---------- 静态/缓存 ----------
	H.eq("/ → 200", (await H.req(B, "/")).status, 200);
	const idxHeaders = (await H.req(B, "/index.html")).headers;
	H.check(
		"index.html Cache-Control 含 max-age=60",
		(idxHeaders["cache-control"] || "").includes("max-age=60"),
		idxHeaders["cache-control"],
	);
	const apiHeaders = (await H.req(B, "/v1/notifications", { session })).headers;
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
