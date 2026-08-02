/* iOS Safari Web Push prototype service worker.
 * Must be served over HTTPS from the app's scope.
 */
self.addEventListener("install", () => {
	self.skipWaiting();
});

self.addEventListener("activate", (event) => {
	event.waitUntil(self.clients.claim());
});

// A foreground PushMessage event is required for the new
// "PushMessageEvent" / delivery of a visible notification even when
// the page is open. iOS Safari 18.5+.
self.addEventListener("push", (event) => {
	let data = { title: "iOS Notify Prototype", body: "Received a push." };
	try {
		if (event.data) data = event.data.json();
	} catch (e) {
		/* ignore non-JSON payloads */
	}

	const title = data.title || "iOS Notify Prototype";
	const options = {
		body: data.body || "You received a notification.",
		icon: "/icon.png",
		badge: "/icon.png",
		tag: data.tag || "notify-proto",
		data: { url: data.url || "/" },
	};

	event.waitUntil(self.registration.showNotification(title, options));
});

self.addEventListener("notificationclick", (event) => {
	const target =
		(event.notification.data && event.notification.data.url) || "/";
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
