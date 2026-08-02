/* iOS Safari notification mechanisms prototype logic. */
(() => {
	var logEl = document.getElementById("log");
	var logCount = 0;

	function log(msg) {
		logCount += 1;
		logEl.textContent =
			logEl.textContent + (logCount > 1 ? "\n" : "") + "> " + msg;
		logEl.scrollTop = logEl.scrollHeight;
		console.log("[proto]", msg);
	}

	// Set a single value field, optionally rendered as a colored badge.
	function setValue(id, label, kind) {
		var el = document.getElementById(id);
		if (!el) return;
		el.textContent = "";
		if (kind) {
			var span = document.createElement("span");
			span.className = "badge " + (kind || "muted");
			span.textContent = label;
			el.appendChild(span);
		} else {
			el.textContent = label;
		}
	}

	// Set the standalone-mode field: a badge plus a secondary detail line.
	function setMode(detailText) {
		var el = document.getElementById("mode");
		if (!el) return;
		el.textContent = "";
		var badge = document.createElement("span");
		badge.className = "badge " + (isStandalone() ? "ok" : "warn");
		badge.textContent = isStandalone()
			? "STANDALONE（已添加到主屏幕）"
			: "普通 Safari 标签页";
		el.appendChild(badge);
		var br = document.createElement("br");
		el.appendChild(br);
		var small = document.createElement("span");
		small.className = "muted";
		small.textContent = detailText;
		el.appendChild(small);
	}

	function isStandalone() {
		return (
			window.matchMedia &&
			(window.matchMedia("(display-mode: standalone)").matches ||
				window.navigator.standalone === true)
		);
	}

	function secureContext() {
		return typeof window.isSecureContext === "boolean"
			? window.isSecureContext
			: !!window.location.protocol.match(/^https/i);
	}

	/* ---------- environment detection ---------- */
	function detectEnv() {
		var standalone = isStandalone();
		var isIOS =
			/iP(hone|ad|od)/.test(navigator.platform || "") ||
			(/Macintosh/.test(navigator.platform || "") &&
				navigator.maxTouchPoints > 1);

		setMode(
			navigator.standalone === true
				? "navigator.standalone=true"
				: "display-mode=" +
						(window.matchMedia("(display-mode: standalone)").matches
							? "standalone"
							: "browser"),
		);

		setValue("ua", navigator.userAgent);
		setValue(
			"secure",
			secureContext() ? "是" : "否",
			secureContext() ? "ok" : "err",
		);

		var hasNotif = "Notification" in window;
		setValue("notifApi", hasNotif ? "支持" : "不支持", hasNotif ? "ok" : "err");
		setValue(
			"swApi",
			"serviceWorker" in navigator ? "支持" : "不支持",
			"serviceWorker" in navigator ? "ok" : "err",
		);
		setValue(
			"pushApi",
			"PushManager" in window ? "支持" : "不支持",
			"PushManager" in window ? "ok" : "err",
		);
		setValue(
			"vibApi",
			"vibrate" in navigator ? "支持" : "不支持（iOS 无振动）",
			"vibrate" in navigator ? "ok" : "err",
		);

		if (!secureContext())
			log("⚠️ 非安全上下文（非 HTTPS），Service Worker / Push 不可用");
		if (!hasNotif) log("⚠️ 此浏览器不支持 Notification API");
		if (isIOS && !standalone)
			log("📌 iOS Safari：请通过「共享 → 添加到主屏幕」安装后再试");

		var perm = "Notification" in window ? Notification.permission : "n/a";
		setValue(
			"perm",
			perm,
			perm === "granted" ? "ok" : perm === "denied" ? "err" : "warn",
		);
		if (perm === "denied")
			log("⛔ 通知权限已被拒绝，请到 设置 → Safari → 通知 中允许");
	}

	/* ---------- permission ---------- */
	function initPermission() {
		var btn = document.getElementById("btnPerm");
		var allow =
			"Notification" in window && Notification.permission === "default";
		btn.disabled = !allow;
		if (allow) {
			btn.textContent = "请求通知权限 (requestPermission)";
			btn.onclick = () => {
				log("请求通知权限…");
				Notification.requestPermission()
					.then((p) => {
						log("requestPermission → " + p);
						setValue(
							"perm",
							p,
							p === "granted" ? "ok" : p === "denied" ? "err" : "warn",
						);
						initPermission();
						initButtons();
					})
					.catch((e) => {
						log("requestPermission 异常: " + e);
					});
			};
		} else {
			btn.disabled = true;
		}
	}

	/* ---------- service worker ---------- */
	function initSW() {
		var btn = document.getElementById("btnSW");
		var enabled = "serviceWorker" in navigator && secureContext();
		btn.disabled = !enabled;
		btn.onclick = () => {
			log("注册 Service Worker…");
			navigator.serviceWorker
				.register("sw.js")
				.then((reg) => {
					log("注册成功 scope=" + reg.scope);
					setValue("swState", "已注册 " + reg.scope, "ok");
					if (reg.active) log("SW active: " + reg.active.state);
					navigator.serviceWorker.ready.then(() => {
						log("SW ready");
					});
					initButtons();
				})
				.catch((e) => {
					log("SW 注册失败: " + e);
				});
		};
		if (enabled && !("Notification" in window)) {
			// SW doesn't need notification permission
			navigator.serviceWorker.ready.then(() => {
				setValue("swState", "ready", "ok");
			});
		}
	}

	/* ---------- local notification ---------- */
	function initLocal() {
		var btn = document.getElementById("btnLocal");
		var allowed =
			"Notification" in window && Notification.permission === "granted";
		btn.disabled = !allowed;
		btn.onclick = () => {
			log("显示本地通知…");
			try {
				var n = new Notification("iOS 通知原型", {
					body: "这是一条本地测试通知 " + new Date().toLocaleTimeString(),
					icon: "icon.png",
					tag: "proto-" + Date.now(),
				});
				n.onclick = () => {
					log("通知被点击");
					n.close();
				};
				log("本地通知已创建");
			} catch (e) {
				log("本地通知异常: " + e);
			}
		};
	}

	/* ---------- web push subscription ---------- */
	function initPush() {
		var btn = document.getElementById("btnSub");
		var pre = document.getElementById("subJson");
		var enabled =
			"PushManager" in window &&
			"serviceWorker" in navigator &&
			secureContext();

		function refreshSub() {
			if (!enabled) return;
			navigator.serviceWorker.ready
				.then((reg) => reg.pushManager.getSubscription())
				.then((sub) => {
					if (sub) {
						pre.textContent = JSON.stringify(
							{
								endpoint: sub.endpoint,
								expirationTime: sub.expirationTime,
								keys: {
									p256dh: btoa(
										String.fromCharCode.apply(
											null,
											new Uint8Array(sub.getKey("p256dh")),
										),
									),
									auth: btoa(
										String.fromCharCode.apply(
											null,
											new Uint8Array(sub.getKey("auth")),
										),
									),
								},
							},
							null,
							2,
						);
						setValue("subJson", "", "value");
						log("已存在 Push 订阅");
					} else {
						pre.textContent = "—";
					}
				})
				.catch((e) => {
					log("读取订阅失败: " + e);
				});
		}

		btn.disabled = !enabled;
		btn.onclick = () => {
			if (Notification.permission !== "granted") {
				log("⚠️ 请先授予通知权限再订阅");
				return;
			}
			log("请求 Push 订阅…");
			getVapidKey()
				.then((vapidKey) => {
					if (!vapidKey) {
						log("⚠️ 未获取到服务端 VAPID 公钥，改用内置占位 key");
						vapidKey = prompt(
							"Paste VAPID public key (base64url), or leave empty:",
							"",
						);
					}
					var opts = {
						userVisibleOnly: true,
						applicationServerKey: urlBase64ToUint8Array(vapidKey),
					};
					return navigator.serviceWorker.ready.then((reg) =>
						reg.pushManager.subscribe(opts),
					);
				})
				.then((sub) => {
					log("订阅成功 ✔");
					refreshSub();
					return saveSubscriptionToServer(sub);
				})
				.catch((e) => {
					log("订阅失败: " + e);
					if (e && e.name === "NotAllowedError")
						log("原因：权限未授予或非 standalone 环境");
				});
		};

		refreshSub();
	}

	/* ---------- background / visibility observation ---------- */
	function initBackground() {
		var vis = document.getElementById("vis");
		var tick = document.getElementById("tick");
		var count = 0;
		document.addEventListener("visibilitychange", () => {
			var v = document.visibilityState;
			vis.textContent = v;
			log("页面可见性 → " + v);
			if (v === "visible") tick.textContent = String(count);
		});
		setInterval(() => {
			count += 1;
			if (document.visibilityState === "visible")
				tick.textContent = String(count);
		}, 1000);
		window.addEventListener("focus", () => {
			vis.textContent = document.visibilityState;
		});
	}

	/* ---------- helpers ---------- */
	function urlBase64ToUint8Array(base64) {
		var padding = "=".repeat((4 - (base64.length % 4)) % 4);
		var b64 = (base64 + padding).replace(/-/g, "+").replace(/_/g, "/");
		var raw = atob(b64);
		var arr = new Uint8Array(raw.length);
		for (var i = 0; i < raw.length; i++) arr[i] = raw.charCodeAt(i);
		return arr;
	}

	// 从服务端拉取真实的 VAPID 公钥
	let cachedVapidKey = null;
	function getVapidKey() {
		if (cachedVapidKey) return Promise.resolve(cachedVapidKey);
		return fetch("vapid-public-key")
			.then((r) => r.json())
			.then((d) => {
				cachedVapidKey = d.publicKey || "";
				return cachedVapidKey;
			})
			.catch(() => "");
	}

	// 把订阅信息上报给服务端，供服务端推送使用
	function saveSubscriptionToServer(sub) {
		if (!sub) return Promise.resolve(false);
		return fetch("subscribe", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({
				endpoint: sub.endpoint,
				expirationTime: sub.expirationTime,
				keys: {
					p256dh: btoa(
						String.fromCharCode.apply(
							null,
							new Uint8Array(sub.getKey("p256dh")),
						),
					),
					auth: btoa(
						String.fromCharCode.apply(null, new Uint8Array(sub.getKey("auth"))),
					),
				},
				userAgent: navigator.userAgent,
			}),
		})
			.then((r) => r.json())
			.then((d) => {
				log(d && d.ok ? "✅ 订阅已上报服务端" : "⚠️ 订阅上报失败");
				return !!(d && d.ok);
			})
			.catch((e) => {
				log("⚠️ 订阅上报异常: " + e);
				return false;
			});
	}

	function initButtons() {
		initLocal();
		initPush();
	}

	/* ---------- boot ---------- */
	detectEnv();
	initPermission();
	initSW();
	initLocal();
	initPush();
	initBackground();
	log("原型加载完成。将本页添加到主屏幕后重试通知相关功能。");
})();
