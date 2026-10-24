/* Anotify E2E 共享的**最小 WebSocket 客户端**（零第三方依赖）。
 *
 * 为什么自己写：套件要发的握手是「带 Authorization 头的 WS 升级」，
 * 而 Node 内置的全局 WebSocket 不支持自定义 header。自己实现 60 行
 * 比引入 ws 包更轻，也避免 e2e 增加运行时依赖。
 *
 * 只实现本仓库节点协议需要的那部分：
 *   - 客户端 → 服务端：文本帧（带掩码，RFC 6455 要求）
 *   - 服务端 → 客户端：文本帧 + 关闭帧
 *   - 不做分片、不做 permessage-deflate（协议用不到）
 */
import http from "node:http";
import crypto from "node:crypto";

// wsConnect(base, token) 建立一条节点协议连接。
//
// 返回 { send, onFrame, onClose, close }。
// 握手被拒时 reject，err.status 是 HTTP 状态码（用于 401/403 断言）。
export async function wsConnect(base, token, pathname = "/v1/node/stream") {
	// new URL 遇到坏输入会抛一个只有 "Invalid URL" 的原生 TypeError，
	// 在 e2e 里完全看不出是哪个套件传错了什么。包一层给出可定位的信息。
	let url;
	try {
		url = new URL(base);
	} catch (e) {
		throw new Error(
			`wsConnect: 非法的 server base ${JSON.stringify(base)}（应为 http://host:port）: ${e.message}`,
		);
	}
	const key = crypto.randomBytes(16).toString("base64");

	const socket = await new Promise((resolve, reject) => {
		const req = http.request({
			hostname: url.hostname,
			port: url.port,
			path: pathname,
			headers: {
				Connection: "Upgrade",
				Upgrade: "websocket",
				"Sec-WebSocket-Key": key,
				"Sec-WebSocket-Version": "13",
				...(token ? { Authorization: "Bearer " + token } : {}),
			},
		});
		req.on("upgrade", (res, sock) => {
			sock.on("error", () => {});
			sock.setNoDelay?.(true);
			resolve(sock);
		});
		req.on("response", (res) => {
			res.resume();
			reject(Object.assign(new Error("ws handshake rejected"), { status: res.statusCode }));
		});
		req.on("error", reject);
		req.end();
	});

	const frameListeners = [];
	const closeListeners = [];
	let buf = Buffer.alloc(0);
	let closed = false;

	socket.on("data", (chunk) => {
		buf = Buffer.concat([buf, chunk]);
		for (;;) {
			if (buf.length < 2) return;
			const opcode = buf[0] & 0x0f;
			let len = buf[1] & 0x7f;
			let off = 2;
			if (len === 126) {
				if (buf.length < 4) return;
				len = buf.readUInt16BE(2);
				off = 4;
			} else if (len === 127) {
				if (buf.length < 10) return;
				len = Number(buf.readBigUInt64BE(2));
				off = 10;
			}
			if (buf.length < off + len) return;
			const payload = buf.subarray(off, off + len);
			buf = buf.subarray(off + len);

			if (opcode === 0x1) {
				let frame;
				try {
					frame = JSON.parse(payload.toString("utf8"));
				} catch {
					continue;
				}
				for (const l of frameListeners) l(frame);
			} else if (opcode === 0x8) {
				// 关闭帧：记录 close code
				const code = payload.length >= 2 ? payload.readUInt16BE(0) : 1005;
				const reason = payload.length > 2 ? payload.subarray(2).toString("utf8") : "";
				if (!closed) {
					closed = true;
					for (const l of closeListeners) l({ code, reason });
				}
			} else if (opcode === 0x9) {
				// ping → 回 pong（带掩码）
				writeFrame(socket, 0xa, payload);
			}
		}
	});

	socket.on("close", () => {
		if (!closed) {
			closed = true;
			for (const l of closeListeners) l({ code: 1006, reason: "socket closed" });
		}
	});

	return {
		send(obj) {
			writeFrame(socket, 0x1, Buffer.from(JSON.stringify(obj), "utf8"));
		},
		sendRaw(text) {
			writeFrame(socket, 0x1, Buffer.from(text, "utf8"));
		},
		onFrame(fn) {
			frameListeners.push(fn);
		},
		onClose(fn) {
			closeListeners.push(fn);
		},
		close() {
			socket.destroy();
		},
		get closed() {
			return closed;
		},
	};
}

// writeFrame 写一个带掩码的客户端帧（RFC 6455 §5.3 要求客户端必须掩码）。
function writeFrame(socket, opcode, payload) {
	const mask = crypto.randomBytes(4);
	const len = payload.length;
	let header;
	if (len < 126) {
		header = Buffer.from([0x80 | opcode, 0x80 | len]);
	} else if (len < 65536) {
		header = Buffer.alloc(4);
		header[0] = 0x80 | opcode;
		header[1] = 0x80 | 126;
		header.writeUInt16BE(len, 2);
	} else {
		header = Buffer.alloc(10);
		header[0] = 0x80 | opcode;
		header[1] = 0x80 | 127;
		header.writeBigUInt64BE(BigInt(len), 2);
	}
	const masked = Buffer.alloc(len);
	for (let i = 0; i < len; i++) masked[i] = payload[i] ^ mask[i % 4];
	socket.write(Buffer.concat([header, mask, masked]));
}

// waitFrame(conn, pred, timeoutMs) 等一帧满足条件的消息（轮询已收帧）。
export async function waitFrame(conn, pred, timeoutMs = 3000) {
	const seen = [];
	const hit = await new Promise((resolve) => {
		const timer = setTimeout(() => resolve(null), timeoutMs);
		conn.onFrame((f) => {
			seen.push(f);
			if (pred(f)) {
				clearTimeout(timer);
				resolve(f);
			}
		});
	});
	return { frame: hit, seen };
}
