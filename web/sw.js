/* Anotify · Web Push Service Worker
 * 需经 HTTPS 提供；接收 push → showNotification，点击 → 聚焦/打开链接。
 */
self.addEventListener("install", () => {
	self.skipWaiting();
});

self.addEventListener("activate", (event) => {
	event.waitUntil(self.clients.claim());
});

self.addEventListener("push", (event) => {
	let data = { title: "Anotify", body: "你收到一条新通知。" };
	try {
		if (event.data) data = event.data.json();
	} catch (e) {
		/* 非 JSON 载荷忽略 */
	}

	const title = data.title || "Anotify";
	const options = {
		body: data.body || "",
		icon: "assets/icon.png",
		badge: "assets/icon.png",
		tag: data.tag || "anotify-" + Date.now(),
		data: { url: data.link || data.url || "index.html" },
	};

	event.waitUntil(self.registration.showNotification(title, options));
});

self.addEventListener("notificationclick", (event) => {
	const target = (event.notification.data && event.notification.data.url) || "index.html";
	event.notification.close();
	event.waitUntil(
		self.clients
			.matchAll({ type: "window", includeUncontrolled: true })
			.then((clients) => {
				for (const client of clients) {
					if ("focus" in client)
						return client.navigate(target).then(() => client.focus());
				}
				if (self.clients.openWindow) return self.clients.openWindow(target);
			}),
	);
});
