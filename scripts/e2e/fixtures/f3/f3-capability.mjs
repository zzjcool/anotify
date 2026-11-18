// Attributed F2a fill/open and ten actual action chains with exact recorder.
import assert from 'node:assert/strict';
import { bringUpFixtureNode, deployApplied, capRef } from './f3-node.mjs';

// 真实权威下「人工部署」用例落到的节点名。它同时是页面候选 checkbox 的 value，
// 所以断言直接拿这个名字对账（不是随便哪个在线节点）。
const DEPLOY_TARGET = "f3-deploy-target";

async function waitHidden(page, sel) {
	await page.waitForFunction(
		(s) => {
			const n = document.querySelector(s);
			return !!n && n.classList.contains("hidden");
		},
		sel,
		{ timeout: 15000 },
	);
}
/* 详情是**独立页**（capability.html?namespace=<ns>&name=<cap>），不再在列表行里内联展开：
 * 点行标题链接（a.cap-title[data-act="open"]）→ 整页导航 → 等 #cap-detail 拿到这个能力的
 * 身份（data-cap）→ 返回**页面级** locator。之后所有 [data-act] / [data-sec] 都相对详情页根，
 * 不再相对列表行（那是内联展开时代的定位方式）。
 *   · 已经在目标详情页 → 直接复用（不重复导航）；
 *   · 停在**别的**详情页 → 先走页面自己的「返回列表」（#cap-back，带 ?restore=1 还原列表状态）
 *     再点目标行。用例之间不共享「上一次展开过」这种隐式状态。 */
async function openCap(page, name) {
	const here = page.locator('#cap-detail[data-cap="' + name + '"]');
	if (await here.count()) return here;
	if (!/capabilities\.html$/.test(new URL(page.url()).pathname)) {
		await page.locator("#cap-back").click();
	}
	const row = page.locator('.list-row.cap-row[data-cap="' + name + '"]');
	await row.waitFor({ state: "visible", timeout: 15000 });
	await row.locator('a.cap-title[data-act="open"]').click();
	await page.waitForFunction(
		(n) => {
			const root = document.getElementById("cap-detail");
			return !!root && root.getAttribute("data-cap") === n;
		},
		name,
		{ timeout: 15000 },
	);
	return page.locator('#cap-detail[data-cap="' + name + '"]');
}
/* observed 的期望值**只能来自读模型本身**：真实权威由中枢按 24h 窗口派生，mock 权威由
 * MockApi 从同名字段派生。先用页面同一条读路径（`?includeBody=true`）取事实，再把 DOM 与它
 * 逐字对账 —— 这样断言的是「页面显示了服务端此刻说的事实」，不是「页面里有某个字串」。 */
async function readObserved(d, ns, name) {
	const res = await d.req("/v1/namespaces/" + ns + "/capabilities/" + encodeURIComponent(name) + "?includeBody=true");
	assert.equal(res && res.kind, "Capability", ns + "/" + name + " 的读面必须是 Capability 信封");
	return (res.status && res.status.observed) || {};
}
/* 有任务之后 status.observed.taskId 必须非空。轮询带超时（不靠固定 sleep 猜）：派发是同步
 * 事务，但读面是下一次 GET 才看到，给中枢一点余量而不是断言「一定有」。 */
async function waitObserved(d, ns, name, why) {
	const deadline = Date.now() + 15000;
	for (;;) {
		const ob = await readObserved(d, ns, name);
		if (ob.taskId) return ob;
		if (Date.now() > deadline) {
			assert.fail(why + "：轮询 15s 后 " + ns + "/" + name + " 的 status.observed.taskId 仍为空（读模型 " + JSON.stringify(ob) + "）");
		}
		// 轮询间隔走 Node 定时器（带 deadline，不是「等固定时间再断言」）：页面侧
		// `waitForTimeout` 是测试纪律棘轮要清零的形态，这里只做有界轮询的节流。
		await new Promise((r) => setTimeout(r, 200));
	}
}
async function runtimeI18n(d) {
	const dict = await d.page.evaluate(() => Object.assign({}, window.AnotifyI18n || {}));
	for (const key of ["caps.observed.unavailable", "caps.observed.tasks", "caps.observed.tasks_failed", "caps.observed.task", "caps.observed.window"]) {
		assert.equal(typeof dict[key], "string", "运行时字典必须有 " + key + "（缺了页面就渲染兜底文案，四语言守卫也会红）");
		assert.ok(dict[key].trim() && dict[key] !== key, key + " 必须有真实译文");
	}
	return dict;
}
/* 有数据态：taskId + 24h 计数（含窗口说明）都是读模型里的**具体值**，不是“出现过某个标签”。 */
async function assertObservedDataDom(d, root, ob, label) {
	const sec = root.locator('[data-sec="observed"]');
	await sec.waitFor({ state: "visible", timeout: 15000 });
	const text = await sec.textContent();
	const dict = await runtimeI18n(d);
	assert.ok(ob.taskId, label + "：读模型必须已给出 taskId（读模型没给就不该指望页面有）");
	assert.ok(text.includes(String(ob.taskId)), label + "：observed 必须显示最近任务 id " + ob.taskId + "，实际：" + text);
	assert.ok(Number.isInteger(ob.tasks && ob.tasks.total), label + "：读模型必须给 tasks.total（整数）");
	assert.ok(Number.isInteger(ob.tasks && ob.tasks.failed), label + "：读模型必须给 tasks.failed（整数）");
	// 计数必须和它自己的标签连在一起出现：「任务总数：3」而不是碰巧页面里有个 3。
	assert.ok(text.includes(dict["caps.observed.tasks"] + "：" + ob.tasks.total), label + "：必须显示 24h 任务总数 " + ob.tasks.total + "，实际：" + text);
	assert.ok(text.includes(dict["caps.observed.tasks_failed"] + "：" + ob.tasks.failed), label + "：必须显示失败数 " + ob.tasks.failed + "，实际：" + text);
	assert.ok(text.includes(dict["caps.observed.window"]), label + "：计数必须标明是近 24h 窗口，否则 24h 的数字被当成全部历史");
	assert.ok(!text.includes(dict["caps.observed.unavailable"]), label + "：有数据时不得再显示空态文案");
	return text;
}
/* 无数据态（服务端 `{}`）：如实空态，且**不能**出现 0 计数（那是把「没有数据」编造成事实）。 */
async function assertObservedEmptyDom(d, root, label) {
	const sec = root.locator('[data-sec="observed"]');
	await sec.waitFor({ state: "visible", timeout: 15000 });
	const text = await sec.textContent();
	const dict = await runtimeI18n(d);
	assert.ok(text.includes(dict["caps.observed.unavailable"]), label + "：服务端 {} 时页面必须给空态文案，实际：" + text);
	assert.ok(!text.includes(dict["caps.observed.tasks"] + "："), label + "：{} 时不得渲染任务计数（那是把 0 当成事实），实际：" + text);
	assert.ok(!text.includes(dict["caps.observed.task"] + "："), label + "：{} 时不得渲染最近任务行，实际：" + text);
	return text;
}
/* #385 起「新建能力」是 3 步向导（基础 / 脚本 / 高级）：字段按步分组，跨步填必须先
 * 走「下一步」。这个 helper 保持原来「全字段都填一遍」的覆盖度（不是砍用例），
 * 只是按向导的步序填；第 3 步的运维参数在两个 <details> 里，展开方式就用用户点的那个
 * summary（不是 page.evaluate 改 open，那样测不到可点性）。
 * 第 1/2 步的「下一步」= #cap-submit（同一个 id），第 3 步才是「创建」，由调用方点。 */
async function step(page, n) {
	await page.waitForFunction(
		(expected) => {
			const chip = document.querySelector("#cap-modal .cap-step-current");
			return !!chip && chip.getAttribute("data-step-chip") === String(expected);
		},
		n,
		{ timeout: 10000 },
	);
}
async function fillCreateForm(page) {
	// ---- 第 1 步：基础 ----
	await page.fill("#cap-name", "f2a-demo");
	await page.fill("#cap-display-name", "F2a 演示");
	await page.fill("#cap-version", "1");
	await page.click("#cap-submit"); // 下一步
	await step(page, 2);

	// ---- 第 2 步：脚本 ----
	await page.fill(
		"#cap-params",
		JSON.stringify({ type: "object", required: ["repo"], properties: { repo: { type: "string" } } }, null, 2),
	);
	await page.locator("#cap-files .cap-row-edit input").first().fill("run.sh");
	await page
		.locator("#cap-files .cap-row-edit textarea")
		.first()
		.fill('#!/usr/bin/env bash\nset -euo pipefail\necho "repo=$ANOTIFY_PARAM_REPO"\necho "root=$SOURCE_ROOT"\n');
	await page.selectOption("#cap-entrypoint", "run.sh");
	await page.click("#cap-submit"); // 下一步
	await step(page, 3);

	// ---- 第 3 步：高级（可选）——两个折叠块先展开 ----
	const adv = page.locator("#cap-modal details.cap-adv");
	assert.equal(await adv.count(), 2, "高级步必须有两个可折叠块（运行环境与并发 / 依赖与生命周期）");
	await adv.nth(0).locator("summary").click();
	await adv.nth(1).locator("summary").click();

	await page.fill("#cap-next", "notify");
	await page.fill("#cap-workdir", "$(vars.SOURCE_ROOT)");
	await page.fill("#cap-requires-commands", "git");
	await page.fill("#cap-requires-values", "vars.SOURCE_ROOT");
	await page.fill("#cap-concurrency", "2");

	// env 第一行：字面量
	await page.click("#cap-env-add");
	const envRows = page.locator("#cap-env .cap-env-row");
	await envRows.nth(0).locator("input.input").first().fill("LOCAL_ONLY");
	await envRows.nth(0).locator("input.input").nth(1).fill("1");
	// env 第二行：valueFrom 字段引用（切模式会重渲染 #cap-env，需重新定位）
	await page.click("#cap-env-add");
	const rows2 = page.locator("#cap-env .cap-env-row");
	await rows2.nth(1).locator("input.input").first().fill("SOURCE_ROOT");
	await rows2.nth(1).locator("select").selectOption("valueFrom");
	const rows3 = page.locator("#cap-env .cap-env-row");
	await rows3.nth(1).locator("input.input").nth(1).fill("vars.SOURCE_ROOT");

	// lifecycle
	await page.check("#cap-idempotent");
	await page.fill("#cap-max-attempts", "2");
	await page.fill("#cap-result-ttl", "604800");
	await page.fill("#cap-timeout", "300");
}


export async function capability(d){
 const {page,base,shot:takeShot}=d;const created={ok:false};let n=0;
 const shot=(page,name)=>takeShot(name);
 const takeRec=async()=>{const c=await d.calls();await page.evaluate(()=>{window.__f3Calls=[];});return c;};
 const check=async(name,fn)=>{await fn();d.check('F3-CAP-'+String(++n).padStart(2,'0')+' '+name);};
		await page.goto(base + "/capabilities.html", { waitUntil: "domcontentloaded", timeout: 20000 });
		await page.waitForSelector('[data-cap="fixer"]', { timeout: 15000 });
		if(d.mock) await page.evaluate(() => MockApi.latency(0));


		/* ---------- 1) 创建：全字段 ---------- */
		await check("创建：真实表单填满 params/files/env/workDir/next/requires/lifecycle/concurrency 后 POST", async () => {
			await page.click("#caps-new");
			await page.waitForSelector("#cap-modal:not(.hidden)");
			await fillCreateForm(page);
			await shot(page, "01-create-form-filled");
			await takeRec(page);
			await page.click("#cap-submit");
			await waitHidden(page, "#cap-modal");
			await page.waitForSelector('[data-cap="f2a-demo"]', { timeout: 15000 });
			const recs = await takeRec(page);
			const post = recs.find((r) => r.method === "POST" && /\/capabilities$/.test(r.url));
			assert.ok(post, "必须发出 POST /v1/namespaces/default/capabilities");
			assert.equal(post.body.kind, "Capability");
			assert.equal(post.body.metadata.version, 1);
			assert.equal(post.body.spec.run.entrypoint, "run.sh");
			assert.match(post.body.spec.run.files["run.sh"], /^#!\/usr\/bin\/env bash/);
			assert.deepEqual(post.body.spec.run.env.LOCAL_ONLY, "1");
			assert.equal(post.body.spec.run.env.SOURCE_ROOT.valueFrom.fieldRef.fieldPath, "vars.SOURCE_ROOT");
			assert.equal(post.body.spec.run.workDir, "$(vars.SOURCE_ROOT)");
			assert.deepEqual(post.body.spec.next, ["notify"]);
			assert.deepEqual(post.body.spec.requires, { commands: ["git"], values: ["vars.SOURCE_ROOT"] });
			assert.deepEqual(post.body.spec.lifecycle, { idempotent: true, maxAttempts: 2, resultTtlSec: 604800, timeoutSec: 300 });
			assert.deepEqual(post.body.spec.concurrency, { maxPerNode: 2 });
			const got = await page.evaluate(async () => {
				const r = await fetch("/v1/namespaces/default/capabilities/f2a-demo");
				return r.json();
			});
			assert.equal(got.metadata.name, "f2a-demo");
			assert.equal(got.metadata.version, 1);
			assert.equal(got.spec.run.workDir, "$(vars.SOURCE_ROOT)");
			created.ok = true;
			await shot(page, "02-created-list");
			return "POST body 全字段一致，服务端读回 v1";
		});

		/* ---------- 2) 详情：脚本正文懒加载 + 原始 YAML ----------
		 * 读面契约（api/resource.yaml）：GET 列表/详情**默认剥掉** spec.run.files，
		 * 正文只在 `?includeBody=true` 时才回。所以详情页必须**自己**再取一次正文，
		 * 否则真实权威下永远显示「这个能力没有脚本文件」。用例 1 刚创建的 run.sh 正文
		 * 就是这里的权威依据（不是拿列表里的旧事实凑数）。
		 * 详情是独立页：点行标题是**整页导航**，新 document 的 __f3Calls 从空开始，
		 * 所以这条 includeBody 请求只可能由详情页自己发出。先 reload 列表，是用例
		 * 真的从「列表只有摘要」这个真实起点走一遍。 */
		await check("详情：脚本正文按 name@version 懒加载（?includeBody=true）且 run.sh 正文可见", async () => {
			assert.ok(created.ok, "依赖创建成功");
			await page.reload({ waitUntil: "domcontentloaded" });
			await page.waitForSelector('[data-cap="f2a-demo"]', { timeout: 15000 });
			if (d.mock) await page.evaluate(() => MockApi.latency(0));
			await takeRec(page);
			await openCap(page, "f2a-demo");
			await page.waitForFunction(
				() => {
					const sec = document.querySelector('[data-cap="f2a-demo"] [data-sec="run"]');
					return !!sec && !!sec.querySelector(".cap-script-pre");
				},
				{ timeout: 15000 },
			);
			const recs = await takeRec(page);
			// 预演 mock 与真实权威走同一个页面路径：两边都必须自己发这条 includeBody 请求。
			assert.ok(
				recs.some((r) => /\/capabilities\/f2a-demo\?includeBody=true$/.test(r.url)),
				"详情面板必须自己请求 ?includeBody=true（列表行只有摘要）：" + JSON.stringify(recs.map((r) => r.url)),
			);
			const shown = await page.textContent('[data-cap="f2a-demo"] [data-sec="run"]');
			assert.ok(
				!/这个能力没有脚本文件|This capability has no script files/.test(shown),
				"详情不能停在「没有脚本文件」空态：" + shown,
			);
			assert.match(shown, /run\.sh/, "必须显示脚本文件名 run.sh");
			assert.match(shown, /ANOTIFY_PARAM_REPO/, "必须显示 run.sh 的正文（创建时输入的那份）");
			await shot(page, "02b-detail-script-body");
			return "详情自行取正文：run.sh 正文可见";
		});

		/* ---------- 3) 原始 YAML：默认收起，展开才取（懒加载） ---------- */
		await check("原始 YAML：折叠块默认收起，展开才请求 /yaml，内容含 apiVersion 与 run.files", async () => {
			assert.ok(created.ok, "依赖创建成功");
			const raw = page.locator('[data-cap="f2a-demo"] details.cap-raw');
			await raw.waitFor({ state: "attached", timeout: 15000 });
			assert.equal(await raw.evaluate((n) => n.open === true), false, "原始 YAML 默认收起");
			await takeRec(page);
			await raw.locator("summary").click();
			await page.waitForFunction(
				() => {
					const box = document.querySelector('[data-cap="f2a-demo"] details.cap-raw');
					return !!box && /apiVersion/.test(box.textContent || "");
				},
				{ timeout: 15000 },
			);
			const recs = await takeRec(page);
			assert.ok(
				recs.some((r) => /\/capabilities\/f2a-demo\/yaml$/.test(r.url)),
				"展开才请求 /yaml：" + JSON.stringify(recs.map((r) => r.url)),
			);
			const labelSeen = await raw.evaluate((node, phrase) => {
				let text = "";
				const walk = (n) => {
					if (n.nodeType === 3) { text += n.textContent || ""; return; }
					if (n.nodeType !== 1) return;
					for (const c of n.childNodes) walk(c);
				};
				walk(node);
				return text.split(phrase).length - 1;
			}, await page.evaluate(() => window.AnotifyI18n?.["caps.detail.yaml"] || "原始 YAML"));
			assert.equal(
				labelSeen,
				1,
				"折叠块内部「原始 YAML」只能出现一次（summary 那次算）：展开后多出一行=codeBlock 又渲染了标题",
			);
			const yamlText = await raw.textContent();
			assert.match(yamlText, /apiVersion/, "YAML 块必须含 apiVersion");
			assert.match(yamlText, /kind: Capability/, "YAML 块必须含 kind: Capability");
			assert.match(yamlText, /files:/, "YAML 块必须含 run.files");
			assert.match(yamlText, /run\.sh/, "run.files 的文件路径必须出现");
			// 正文的序列化风格由后端决定（块标量或引号串都合法）：只断言内容逐字在场。
			assert.match(yamlText, /ANOTIFY_PARAM_REPO/, "脚本正文逐字保留");
			assert.ok(
				(await raw.locator("button.copy-btn").count()) >= 1,
				"原始 YAML 块必须带复制按钮（.copy-btn）",
			);
			await shot(page, "02c-detail-raw-yaml");
			return "展开才取 /yaml；apiVersion/kind/run.files 均可见";
		});

		/* ---------- 4) 编辑：改内容 → 新版本（POST，内容寻址） ----------
		 * 真实契约（capability.md §3 / resource_capability.go）：能力的**内容面不可变**，
		 * PATCH 只接 displayName 与 next；改 run/params/requires/lifecycle/concurrency
		 * 一律 400 immutable_field。所以"改正文"的唯一合法路径是 **POST 一个新版本**
		 * （省略 metadata.version = 内容寻址）。CAS 的 resourceVersion 属于 PATCH 那条路
		 * （只改 displayName/next 时用），在用例 3 里断言。 */
		await check("编辑：改 run.files 省略 version → POST 内容寻址出 v2（不 PATCH 不可变字段）", async () => {
			assert.ok(created.ok, "依赖创建成功");
			const row = await openCap(page, "f2a-demo");
			await row.locator('[data-act="edit"]').click();
			await page.waitForSelector("#cap-modal:not(.hidden)");
			assert.equal(await page.inputValue("#cap-version"), "", "编辑默认留空 = 内容寻址");
			assert.equal(await page.inputValue("#cap-display-name"), "F2a 演示");
			await page
				.locator("#cap-files .cap-row-edit textarea")
				.first()
				.fill('#!/usr/bin/env bash\nset -euo pipefail\necho "v2"\n');
			await takeRec(page);
			await page.click("#cap-submit");
			await waitHidden(page, "#cap-modal");
			const recs = await takeRec(page);
			const newVersion = recs.find((r) => r.method === "POST" && /\/capabilities$/.test(r.url));
			assert.ok(newVersion, "改正文必须 POST …/capabilities 建新版本");
			assert.equal(Object.hasOwn(newVersion.body.metadata, "version"), false, "省略 metadata.version = 内容寻址");
			assert.equal(recs.some((r) => r.method === "PATCH" && /f2a-demo$/.test(r.url)), false, "不得 PATCH 不可变内容");
			// 默认投影**不含正文**（S1：不泄露 run.files），要看正文必须 ?includeBody=true。
			const got = await page.evaluate(async () => (await fetch("/v1/namespaces/default/capabilities/f2a-demo?includeBody=true")).json());
			assert.equal(got.metadata.version, 2, "改正文必须出 v2");
			assert.ok(Object.values(got.spec.run.files).some((body) => String(body).includes("v2")), "v2 的正文必须是这次的输入");
			return "POST 内容寻址 → v" + got.metadata.version;
		});

		/* ---------- 5) CAS 409：并发写后旧 rv，输入保留 ---------- */
		await check("CAS 409：并发写后旧 rv 保存 → 横幅说明 + 用户输入保留", async () => {
			assert.ok(created.ok, "依赖创建成功");
			const row = await openCap(page, "f2a-demo");
			await row.locator('[data-act="edit"]').click();
			await page.waitForSelector("#cap-modal:not(.hidden)");
			await page.fill("#cap-display-name", "用户本地改动");
			// 并发写只改 displayName（PATCH 的合法面）：内容面不可变，带 spec 重发会被
			// 400 immutable_field 拒掉，那就不再是"并发写"而是"写失败"。
			const stale = await page.evaluate(async () => {
				const g = await fetch("/v1/namespaces/default/capabilities/f2a-demo");
				const res = await g.json();
				const rv = res.metadata.resourceVersion;
				const concurrent = await fetch("/v1/namespaces/default/capabilities/f2a-demo", {
					method: "PATCH",
					headers: { "Content-Type": "application/json" },
					body: JSON.stringify({ apiVersion: "anotify/v1", kind: "Capability", metadata: { name: "f2a-demo", resourceVersion: rv, displayName: "别人写的" } }),
				});
				if (!concurrent.ok) throw new Error("concurrent PATCH failed: " + concurrent.status + " " + (await concurrent.text()));
				return rv;
			});
			await page.click("#cap-submit");
			await page.waitForSelector("#cap-banner:not(.hidden)", { timeout: 15000 }).catch(async (error) => {
				console.log('DEBUG cas', JSON.stringify((await takeRec(page)).map((r) => ({ method: r.method, url: r.url, status: r.status, reason: r.reason }))));
				throw error;
			});
			const banner = await page.textContent("#cap-banner");
			assert.match(banner, /409/);
			assert.match(banner, /输入都还在/);
			assert.match(banner, new RegExp(stale + "|resourceVersion"));
			assert.equal(await page.inputValue("#cap-display-name"), "用户本地改动", "失败后用户输入必须保留");
			assert.equal(await page.locator("#cap-modal:not(.hidden)").count(), 1, "失败后编辑器必须还开着");
			await shot(page, "03-conflict-409-preserved");
			await page.click("#cap-banner-actions button");
			await page.waitForFunction(() => {
				const el = document.getElementById("cap-display-name");
				return el && el.value === "别人写的";
			}, { timeout: 15000 });
			// 用户主动关闭编辑器，回到列表（否则后续点击会被弹层拦住）
			await page.click("#cap-cancel");
			await waitHidden(page, "#cap-modal");
			return "横幅含 409/当前 rv；输入保留；点重新载入才丢弃并拿到最新";
		});

		/* ---------- 6) 显式版本不可变 + 版本切换 ---------- */
		await check("版本：显式同版本改正文 → 400 version_content_immutable，改 version=3 保存成功", async () => {
			const row = await openCap(page, "analyze-pr");
			await row.locator('[data-act="edit"]').click();
			await page.waitForSelector("#cap-modal:not(.hidden)");
			await page.fill("#cap-version", "2");
			await page
				.locator("#cap-files .cap-row-edit textarea")
				.first()
				.fill('#!/usr/bin/env bash\nset -euo pipefail\necho "changed-body"\n');
			await page.click("#cap-submit");
			await page.waitForSelector("#cap-banner:not(.hidden)", { timeout: 15000 });
			const banner = await page.textContent("#cap-banner");
			assert.match(banner, /version_content_immutable/);
			assert.match(banner, /不可变/);
			assert.match(banner, /换一个版本/);
			assert.equal(await page.inputValue("#cap-version"), "2", "400 后输入保留");
			assert.equal(await page.locator("#cap-modal:not(.hidden)").count(), 1);
			await shot(page, "04-version-immutable-400");
			// 版本切换：显式改成 3 再存
			await page.fill("#cap-version", "3");
			await takeRec(page);
			await page.click("#cap-submit");
			await waitHidden(page, "#cap-modal");
			const recs = await takeRec(page);
			// 显式版本号的内容变更同样是 POST（内容面不可变，永远不 PATCH）。
			const patch = recs.find((r) => r.method === "POST" && /\/capabilities$/.test(r.url) && r.body.metadata.version === 3);
			assert.ok(patch, "显式版本 3 的内容变更必须 POST 新版本（body metadata.version=3）");
			const got = await page.evaluate(async () => (await fetch("/v1/namespaces/default/capabilities/analyze-pr?includeBody=true")).json());
			assert.equal(got.metadata.version, 3);
			assert.ok(Object.values(got.spec.run.files).some((body) => String(body).includes("changed-body")), "v3 的正文必须是本次输入");
			return "400 指引完整；改 v3 保存成功（正文=本次输入）";
		});

		/* ---------- 7) 版本历史 + 单版本正文 includeBody=true ---------- */
		await check("版本历史：CapabilityList.items（每项 Capability 信封、无 item.current），单版本 ?includeBody=true", async () => {
			const row = await openCap(page, "analyze-pr");
			await row.locator('[data-act="versions"]').click();
			await page.waitForSelector('[data-cap="analyze-pr"] .cap-ver-row', { timeout: 15000 });
			const vers = await page.locator('[data-cap="analyze-pr"] .cap-ver-no').allTextContents();
			assert.ok(vers.length >= 3, "版本历史至少 3 行，实际 " + vers.join(","));
			assert.equal(vers[0], "v3", "新→旧：当前（max version）在最前");

			// wire 形状：不是独立 kind；每项同一个 Capability 信封；没有自造 current。
			const wire = await page.evaluate(async () => {
				const r = await fetch("/v1/namespaces/default/capabilities/analyze-pr/versions");
				return r.json();
			});
			assert.equal(wire.kind, "CapabilityList", "版本列表 kind 必须是 CapabilityList");
			assert.ok(wire.items.every((i) => i.kind === "Capability"), "每个版本项必须同 Capability 信封");
			assert.ok(wire.items.every((i) => !("current" in i)), "wire 不得带 item.current");
			assert.equal(Math.max(...wire.items.map((i) => i.metadata.version)), 3, "当前版本 = max(metadata.version)");

			await takeRec(page);
			await page.locator('[data-cap="analyze-pr"] .cap-ver-row button').nth(1).click();
			// 等**正文真的有内容**，而不是只等容器出现：容器先挂上、includeBody=true 的响应后到，
			// 只等容器会让下面的 textContent 读到空串（CI 上就是这样红的：那两条断言里
			// 一条是「版本正文不能为空」，本地因为响应快没暴露）。断言本身不放松，只把等待改成有条件。
			await page.waitForFunction(
				() => {
					const el = document.querySelector('[data-cap="analyze-pr"] .cap-ver-body');
					return !!el && (el.textContent || "").trim().length > 0;
				},
				{ timeout: 15000 },
			);
			const recs = await takeRec(page);
			assert.ok(
				recs.some((r) => /\/versions\/\d+\?includeBody=true$/.test(r.url)),
				"单版本正文必须请求 ?includeBody=true",
			);
			const body = await page.textContent('[data-cap="analyze-pr"] .cap-ver-body');
			assert.ok(body.trim().length > 0, "版本正文不能为空");
			await shot(page, "05-version-history-body");
			return "版本 " + vers.join(" / ") + "；正文接口 includeBody=true";
		});

		/* ---------- 8) 人工部署：选历史版本 + 选节点 ---------- */
		await check("人工部署：选 v1 + 节点 → POST /deploy（body version/nodes），部署后该节点 stale", async () => {
			// 候选节点不是随意列的在线节点，而是**被渲染行**的只读 status.nodes（capability.md §5：
			// deployments 事实的投影）。真实权威下 analyze-pr 从没部署到任何节点 → status.nodes=[]
			// → 模态框里没有候选 checkbox（mock 种子直接给了候选节点，所以只有 real 需要这份造数）。
			//
			// 造数只走产品自己的入口：真实节点（POST /v1/nodes → 签发令牌 → WS hello → welcome
			// → config.applied）+ 真实部署 API 把 **v2** 部署上去并等 applied。当前行是 v3，所以
			// v3 的 status.nodes 会把这台节点标成 stale（applied 但 digest ≠ 当前行）—— 候选就
			// 出现了，而且它出现的原因正是下面要验的那条语义（节点拿着别的版本）。
			if (!d.mock) {
				const session = d.fixtures.adminSession;
				const node = await bringUpFixtureNode({ base, session, name: DEPLOY_TARGET, namespaces: ["default"] });
				// 关闭交给驱动 stop()（现有 helper）：下面的 UI 部署要这台节点在线回 capability.deployed，
				// 提前 close 会让部署行停在 pending（那是另一种事实，不是本用例要验的）。
				(d.fixtures.__holdNodes ??= []).push(node);
				const v2 = await d.api("/v1/namespaces/default/capabilities/analyze-pr/versions/2");
				assert.equal(v2.status, 200, "T1 seam: analyze-pr v2 必须读得到，实际 " + v2.status + " " + JSON.stringify(v2.body));
				assert.equal(v2.body.metadata.version, 2, "T1 seam: analyze-pr 必须真的有 v2（用例 4 把当前行改成了 v3）");
				const applied = await deployApplied(base, session, node, capRef(v2.body));
				assert.equal(applied.state, "applied", "T1 seam: v2 部署行必须 applied，实际 " + JSON.stringify(applied));
				// 页面模型是上一次加载的快照（节点是这条用例里现起的）→ 用页面自己的刷新动作
				// 重读这个能力（详情页的 #cap-refresh 走的就是 load()），让读模型里出现这台
				// 节点（用户点「刷新」也是同一回事）。
				const refreshed = page.waitForResponse(
					(r) => {
						try {
							return (
								new URL(r.url()).pathname === "/v1/namespaces/default/capabilities/analyze-pr" && r.status() === 200
							);
						} catch (e) {
							return false;
						}
					},
					{ timeout: 15000 },
				);
				await page.click("#cap-refresh");
				await refreshed;
			}
			const row = await openCap(page, "analyze-pr");
			if (!d.mock) {
				// 刷新后的读模型：v3 行的 nodes 小节里必须列出这台真实节点（否则候选 checkbox 无从出现）
				await page.waitForFunction(
					(n) => {
						const sec = document.querySelector('[data-cap="analyze-pr"] [data-sec="nodes"]');
						return !!sec && sec.textContent.includes(n);
					},
					DEPLOY_TARGET,
					{ timeout: 15000 },
				);
			}
			await row.locator('[data-act="deploy"]').click();
			await page.waitForSelector("#deploy-modal:not(.hidden)");
			await page.waitForFunction(() => {
				const sel = document.getElementById("deploy-version");
				return sel && sel.options.length >= 3;
			}, { timeout: 15000 });
			const hint = await page.textContent("#deploy-modal .modal-body");
			assert.match(hint, /不会自动部署/);
			const opts = await page.$$eval("#deploy-version option", (os) =>
				os.map((o) => ({ v: o.value, text: o.textContent || "" })),
			);
			assert.deepEqual(
				opts.filter((o) => o.text.includes("当前")).map((o) => o.v),
				["3"],
				"“当前”标在 max(metadata.version) 上，不是自造 item.current",
			);
			await page.selectOption("#deploy-version", "1");
			const firstNode = page.locator("#deploy-nodes input[type=checkbox]").first();
			const nodeName = await firstNode.getAttribute("value");
			if (!d.mock) {
				assert.equal(nodeName, DEPLOY_TARGET, "候选节点必须是真实部署事实里的那台节点，实际 " + nodeName);
			}
			await firstNode.check();
			await shot(page, "06-deploy-modal");
			await takeRec(page);
			await page.click("#deploy-submit");
			await waitHidden(page, "#deploy-modal");
			const recs = await takeRec(page);
			const dep = recs.find((r) => r.method === "POST" && /\/deploy$/.test(r.url));
			assert.ok(dep, "必须 POST …/capabilities/analyze-pr/deploy");
			assert.equal(dep.body.version, 1);
			assert.deepEqual(dep.body.nodes, [nodeName]);
			// 状态刷新后：该节点持有旧 digest → stale
			await page.waitForFunction((n) => {
				const row = document.querySelector('[data-cap="analyze-pr"]');
				return row && row.querySelector('[data-sec="nodes"]') && row.textContent.includes(n);
			}, nodeName, { timeout: 15000 });
			const detail = await page.textContent('[data-cap="analyze-pr"] [data-sec="nodes"]');
			assert.match(detail, new RegExp(nodeName));
			assert.match(detail, /旧 digest|old digest|digest (antiguo|旧)/, "部署 v1 后节点必须显示 stale（旧 digest）");
			await shot(page, "07-deploy-result-stale");
			return "POST body " + JSON.stringify(dep.body) + "；节点 " + nodeName + " → stale";
		});

		/* ---------- 9) resolved / observed / conditions 诊断明细 ---------- */
		await check("诊断：conditions 四条 + resolved 明细（missing/reason/required）+ observed emits/pruned", async () => {
			const fx = await openCap(page, "fixer");
			await fx.locator('[data-sec="conditions"]').waitFor({ state: "visible" });
			const condTypes = await fx.locator('[data-sec="conditions"] .cap-cond-type').allTextContents();
			assert.deepEqual(
				[...new Set(condTypes.map((s) => s.trim()))].sort(),
				["Deployed", "DownstreamResolved", "Implemented", "NodesAvailable"].sort(),
				"conditions 必须是冻结四类（不依赖顺序）",
			);
			const resolved = await fx.textContent('[data-sec="resolved"]');
			assert.match(resolved, /analyze-pr/);
			assert.match(resolved, /lint-fix/);
			assert.match(resolved, /v1/, "钉死 lint-fix@1 必须显示版本");
			const observed = await fx.textContent('[data-sec="observed"]');
			/* observed 现在两种权威都说**真实事实**（服务端 `status.observed`：最近任务 +
			 * emits/pruned + 近 24h 计数，见 docs/design/v4/capability.md §2）。修复器在本用例的
			 * 默认 ns 里已经有真实任务（真实权威：base fixtures 经 `POST /sources/deploy-hook/fire`
			 * 建出 fixer 任务；mock 权威：种子 tsk_mock_1），所以这一条直接对账「页面 = 读模型」，
			 * 不再把契约上已不存在的「恒 {}」当预期。 */
			const fxOb = await readObserved(d, "default", "fixer");
			await assertObservedDataDom(d, fx, fxOb, "default/fixer");
			if (d.mock) assert.match(observed, /字段|emit|派出/, "mock 种子里 fixer 的最近任务有 emit 明细，页面必须列出来");
			await shot(page, "08-detail-diagnostics-fixer");

			const dp = await openCap(page, "deploy-preview");
			const dpConds = await dp.textContent('[data-sec="conditions"]');
			// 原因优先级是全局的（internal/store/node.go·nodeIneligibilityReason：离线 >
			// 授权 > 配置未 apply > 未部署 > 缺命令 > 缺值）。真实权威下仓里登记着**离线**的
			// anode-1/anode-2，任何命名空间的能力都会先报 NodeOffline；而下面 prod-deploy 又
			// 要求 NodeOffline —— 两条在真实派生下不可能同时成立（有离线节点就压过 ValueMissing，
			// 没有离线节点就出不了 NodeOffline）。所以值这一路的原因只在 mock 种子里可验。
			assert.match(
				dpConds,
				d.mock ? /ValueMissing/ : /NodeOffline/,
				d.mock ? "deploy-preview 的 NodesAvailable 必须给 ValueMissing 原因" : "真实权威下 deploy-preview 的原因是 NodeOffline（离线优先）",
			);
			const dpResolved = await dp.textContent('[data-sec="resolved"]');
			assert.match(dpResolved, /终端能力/, "next=[] 必须显示终端能力空态");

			return "fixer 四条件+resolved+emits；deploy-preview ValueMissing";
		});

		/* ---------- 10) observed：真实任务 → taskId/24h 计数；没跑过 → 诚实空态 ----------
		 * 造数据走**产品入口**：`POST /sources/{name}/fire`（真实权威同一个 handler，mock 权威同一
		 * 条 MockApi 路由）为源的下游 fixer 建一个真任务；再轮询读面的 status.observed.taskId
		 * 非空（带超时）后打开详情页断言 DOM。反面对照用本用例刚建、从没跑过的 f2a-demo。 */
		await check("observed：真实投递后详情页显示最近任务与 24h 计数（与读模型逐字一致），没跑过的能力保持诚实空态", async () => {
			const before = await readObserved(d, "default", "fixer");
			const fired = await d.req("/v1/namespaces/default/sources/deploy-hook/fire", {
				method: "POST",
				body: JSON.stringify({ payload: { repo: "f3/observed", environment: "staging", commit: "9f2a1b3" } }),
			});
			assert.equal(fired.duplicate, false, "首次投递不得被当成幂等重复");
			assert.ok((fired.tasks || []).some((t) => t.capability === "fixer"), "投递必须为 fixer 建出真实任务：" + JSON.stringify(fired));
			const ob = await waitObserved(d, "default", "fixer", "fire 之后 fixer 必须有最近任务");
			assert.ok(ob.tasks.total >= 1, "24h 窗口内至少含这次投递的任务，实际 " + JSON.stringify(ob.tasks));
			// 最近任务只能前进：投递前如果有更旧的，现在必须变（不能拿旧值硬充）
			if (before.taskId) assert.notEqual(ob.taskId, before.taskId, "投递后 status.observed.taskId 必须指向更近的那个任务");
			if (d.mock) assert.equal(ob.taskId, (fired.tasks || []).find((t) => t.capability === "fixer").taskId, "mock 权威下最近任务就是这次投递建出的那个");

			const fx = await openCap(page, "fixer");
			await assertObservedDataDom(d, fx, ob, "投递后 default/fixer");
			await shot(page, "10-observed-real-task");

			// 反面对照：f2a-demo 在本用例里只被创建/改过，从没跑过 → 服务端 `{}`（不是 null、不是报错）
			const demo = await openCap(page, "f2a-demo");
			const demoOb = await readObserved(d, "default", "f2a-demo");
			assert.deepEqual(demoOb, {}, "从没跑过的能力，服务端必须回空对象 " + JSON.stringify(demoOb));
			await assertObservedEmptyDom(d, demo, "从没跑过的 default/f2a-demo");
			return "fire → taskId=" + ob.taskId + " total=" + ob.tasks.total + " failed=" + ob.tasks.failed + "；从没跑过的 f2a-demo 保持空态（服务端 {}）";
		});

		/* ---------- 11) DELETE CAS：旧 rv 409 重载 + 二次确认 ---------- */
		await check("删除 CAS：旧 rv → 409 重新载入并二次确认；成功 DELETE 带 ?resourceVersion=", async () => {
			const row = await openCap(page, "lint-fix");
			await row.locator('[data-act="delete"]').click();
			await page.waitForSelector("#del-modal:not(.hidden)");
			// 并发写先验一次“内容面不可变”：把整份 spec 塞进 PATCH 必须被 400 immutable_field
			// 拒掉（这就是 f3-cases 里 f2a-demo 那条预期 400 的被测行为），且 rv 不动。
			await page.evaluate(async () => {
				const res = await (await fetch("/v1/namespaces/default/capabilities/f2a-demo")).json();
				const rejected = await fetch("/v1/namespaces/default/capabilities/f2a-demo", {
					method: "PATCH",
					headers: { "Content-Type": "application/json" },
					body: JSON.stringify({ apiVersion: "anotify/v1", kind: "Capability", metadata: { name: "f2a-demo", resourceVersion: res.metadata.resourceVersion }, spec: res.spec }),
				});
				if (rejected.status !== 400) throw new Error("immutable spec PATCH must be 400, got " + rejected.status + " " + (await rejected.text()));
				if ((await rejected.json()).reason !== "immutable_field") throw new Error("immutable spec PATCH reason must be immutable_field");
				const after = await (await fetch("/v1/namespaces/default/capabilities/f2a-demo")).json();
				if (after.metadata.resourceVersion !== res.metadata.resourceVersion) throw new Error("rejected PATCH must not move resourceVersion");
			});
			const freshRv = await page.evaluate(async () => {
				const g = await fetch("/v1/namespaces/default/capabilities/lint-fix");
				const res = await g.json();
				const rv = res.metadata.resourceVersion;
				// 并发写只走 PATCH 的合法面（displayName）：带 spec 会被 400 immutable_field
				// 拒掉，那就不再是“并发写”而是“写失败”。
				const concurrent = await fetch("/v1/namespaces/default/capabilities/lint-fix", {
					method: "PATCH",
					headers: { "Content-Type": "application/json" },
					body: JSON.stringify({ apiVersion: "anotify/v1", kind: "Capability", metadata: { name: "lint-fix", resourceVersion: rv, displayName: "并发改动" } }),
				});
				if (!concurrent.ok) throw new Error("concurrent PATCH failed: " + concurrent.status + " " + (await concurrent.text()));
				const again = await (await fetch("/v1/namespaces/default/capabilities/lint-fix")).json();
				return again.metadata.resourceVersion;
			});
			await takeRec(page);
			await page.click("#del-submit");
			await page.waitForSelector("#del-msg:not(.hidden)", { timeout: 15000 });
			const msg = await page.textContent("#del-msg");
			assert.match(msg, /409/);
			assert.match(msg, /重新载入最新 resourceVersion/);
			await shot(page, "10-delete-conflict");
			await takeRec(page);
			await page.click("#del-submit");
			await waitHidden(page, "#del-modal");
			// DELETE 记录必须在**离开详情页之前**取：回列表是整页导航，新 document 的
			// __f3Calls 会重置。删除成功后详情页自己切成「能力已删除」空态（data-cap 清空）。
			const recs = await takeRec(page);
			const del = recs.find((r) => r.method === "DELETE" && /lint-fix/.test(r.url));
			assert.ok(del, "必须发出 DELETE …/capabilities/lint-fix");
			assert.match(del.url, /resourceVersion=/);
			const param = new URL(del.url, base).searchParams.get("resourceVersion");
			assert.equal(param, freshRv, "二次确认必须用重新载入后的最新 rv");
			assert.equal(
				await page.locator('#cap-detail[data-cap="lint-fix"]').count(),
				0,
				"删除成功后详情页不得还挂着这个能力的身份（data-cap 必须清空）",
			);
			// 回列表（页面自己的「返回列表」带 ?restore=1），这一行必须真的从列表里消失。
			// 先等列表渲染出来（fixer 行在），否则「目标行不存在」在骨架屏上恒真，是空断言。
			await page.locator("#cap-back").click();
			await page.waitForSelector('[data-cap="fixer"]', { timeout: 15000 });
			await page.waitForFunction(() => !document.querySelector('[data-cap="lint-fix"]'), { timeout: 15000 });
			return "409 后二次确认；DELETE rv=" + param + "；回列表该行已消失";
		});

		/* ---------- 12) prod 命名空间诊断：missing 下游 + NodeOffline + pruned ---------- */
		await check("诊断（prod）：scope 切换后 report.send DownstreamResolved=NotFound，prod-deploy NodeOffline，observed pruned", async () => {
			await page.locator(".scope-trigger").click();
			await page.locator('[data-ns-id="prod"]').click();
			await page.waitForSelector('[data-cap="report.send"]', { timeout: 20000 });
			if(d.mock) await page.evaluate(() => MockApi.latency(0));
			const rs = await openCap(page, "report.send");
			const rsResolved = await rs.textContent('[data-sec="resolved"]');
			assert.match(rsResolved, /ghost-cap/);
			assert.match(rsResolved, /NotFound|解析不到/);
			const rsObserved = await rs.textContent('[data-sec="observed"]');
			/* 同 check 10：observed 说真实事实。prod 的 report.send 在 base fixtures 里已经被真实投递
			 * 过一条（`f3-report-hook`），所以这里同样对账「页面 = 读模型」；mock 种子里它还带一条
			 * param_missing 剪枝留痕（下面单独断言）。 */
			const rsOb = await readObserved(d, "prod", "report.send");
			await assertObservedDataDom(d, rs, rsOb, "prod/report.send");
			if (d.mock) assert.match(rsObserved, /param_missing/, "mock 种子里 report.send 的最近任务有 param_missing 剪枝留痕，页面必须列出来");
			await shot(page, "09-detail-diagnostics-prod-missing");
			const pd = await openCap(page, "prod-deploy");
			const pdConds = await pd.textContent('[data-sec="conditions"]');
			assert.match(pdConds, /NodeOffline/, "prod-deploy 的 NodesAvailable 必须给 NodeOffline");
			return "report.send NotFound/pruned；prod-deploy NodeOffline";
		});

		/* ---------- 13) 四语言页面：无裸 key ---------- */
		await check("四语言页面：?lang=en/ja/es/zh-CN 标题与按钮已翻译，页面无裸 caps.* key", async () => {
			const expect = { "zh-CN": "能力", en: "Capabilities", ja: "能力", es: "Capacidades" };
			await page.evaluate(() => {
				try {
					localStorage.setItem("anotify.scope", JSON.stringify({ namespace: "default" }));
				} catch (e) {}
			});
			for (const lang of ["zh-CN", "en", "ja", "es"]) {
				// 真实权威的语种是**服务端按路径前缀**渲染（`/en/capabilities.html`：layout 的
				// `<html lang>` 与 hreflang 都是它）；`?lang=` 只有预览 mock 认。两种权威各走
				// 自己那条真实入口，断言（标题已翻译、无裸 key）一个字都不动。
				const url = d.mock
					? base + "/capabilities.html?lang=" + lang
					: base + (lang === "zh-CN" ? "/" : "/" + lang + "/") + "capabilities.html";
				await page.goto(url, { waitUntil: "domcontentloaded", timeout: 20000 });
				await page.waitForSelector('.page-title', { timeout: 15000 });
				await page.waitForSelector('[data-cap="fixer"]', { timeout: 15000 });
				const title = (await page.textContent(".page-title")).trim();
				assert.ok(title.length > 0, lang + " 标题为空");
				assert.ok(!/^caps\./.test(title), lang + " 标题是裸 key：" + title);
				assert.ok(title === expect[lang], lang + " 标题应翻译为 " + expect[lang] + "，实际 " + title);
				const body = await page.evaluate(() => document.body.innerText);
				assert.ok(!/caps\.[a-z_]/.test(body), lang + " 页面存在裸 caps.* key");
				await shot(page, "11-lang-" + lang);
			}
			return "zh-CN/en/ja/es 标题均已翻译且无裸 key";
		});

}
