// Attributed F2a fill/open and ten actual action chains with exact recorder.
import assert from 'node:assert/strict';
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
async function openCap(page, name) {
	const row = page.locator('[data-cap="' + name + '"]');
	await row.waitFor({ state: "visible" });
	if (!(await row.locator(".cap-detail").count())) {
		await row.locator(".cap-row-actions button").first().click();
		await row.locator(".cap-detail").waitFor({ state: "visible" });
	}
	return row;
}
async function fillCreateForm(page) {
	await page.fill("#cap-name", "f2a-demo");
	await page.fill("#cap-display-name", "F2a 演示");
	await page.fill("#cap-version", "1");
	await page.fill(
		"#cap-params",
		JSON.stringify({ type: "object", required: ["repo"], properties: { repo: { type: "string" } } }, null, 2),
	);
	await page.fill("#cap-next", "notify");
	await page.locator("#cap-files .cap-row-edit input").first().fill("run.sh");
	await page
		.locator("#cap-files .cap-row-edit textarea")
		.first()
		.fill('#!/usr/bin/env bash\nset -euo pipefail\necho "repo=$ANOTIFY_PARAM_REPO"\necho "root=$SOURCE_ROOT"\n');
	await page.selectOption("#cap-entrypoint", "run.sh");
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

		/* ---------- 2) 编辑：省略 version → 内容寻址新版本；CAS 带 rv ---------- */
		await check("编辑：改 run.files 省略 version → 内容寻址 v2，PATCH 带 metadata.resourceVersion", async () => {
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
			const patch = recs.find((r) => r.method === "PATCH" && /f2a-demo$/.test(r.url));
			assert.ok(patch, "必须发出 PATCH …/capabilities/f2a-demo");
			assert.ok(patch.body.metadata.resourceVersion, "PATCH 必须带 metadata.resourceVersion");
			const got = await page.evaluate(async () => (await fetch("/v1/namespaces/default/capabilities/f2a-demo")).json());
			assert.equal(got.metadata.version, 2, "改正文必须出 v2");
			return "PATCH rv=" + patch.body.metadata.resourceVersion + "，新版本 v" + got.metadata.version;
		});

		/* ---------- 3) CAS 409：并发写后旧 rv，输入保留 ---------- */
		await check("CAS 409：并发写后旧 rv 保存 → 横幅说明 + 用户输入保留", async () => {
			assert.ok(created.ok, "依赖创建成功");
			const row = await openCap(page, "f2a-demo");
			await row.locator('[data-act="edit"]').click();
			await page.waitForSelector("#cap-modal:not(.hidden)");
			await page.fill("#cap-display-name", "用户本地改动");
			const stale = await page.evaluate(async () => {
				const g = await fetch("/v1/namespaces/default/capabilities/f2a-demo");
				const res = await g.json();
				const rv = res.metadata.resourceVersion;
				await fetch("/v1/namespaces/default/capabilities/f2a-demo", {
					method: "PATCH",
					headers: { "Content-Type": "application/json" },
					body: JSON.stringify({ apiVersion: "anotify/v1", kind: "Capability", metadata: { name: "f2a-demo", resourceVersion: rv, displayName: "别人写的" }, spec: res.spec }),
				});
				return rv;
			});
			await page.click("#cap-submit");
			await page.waitForSelector("#cap-banner:not(.hidden)", { timeout: 15000 });
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

		/* ---------- 4) 显式版本不可变 + 版本切换 ---------- */
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
			const patch = recs.find((r) => r.method === "PATCH" && /analyze-pr$/.test(r.url));
			assert.ok(patch && patch.body.metadata.version === 3, "PATCH body metadata.version 必须是 3");
			const got = await page.evaluate(async () => (await fetch("/v1/namespaces/default/capabilities/analyze-pr")).json());
			assert.equal(got.metadata.version, 3);
			assert.equal(got.spec.run.files["main.sh"].includes("changed-body"), true);
			return "400 指引完整；改 v3 保存成功（正文=本次输入）";
		});

		/* ---------- 5) 版本历史 + 单版本正文 includeBody=true ---------- */
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
			await page.waitForSelector('[data-cap="analyze-pr"] .cap-ver-body', { timeout: 15000 });
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

		/* ---------- 6) 人工部署：选历史版本 + 选节点 ---------- */
		await check("人工部署：选 v1 + 节点 → POST /deploy（body version/nodes），部署后该节点 stale", async () => {
			const row = await openCap(page, "analyze-pr");
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

		/* ---------- 7) resolved / observed / conditions 诊断明细 ---------- */
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
			assert.match(observed, /tsk_mock_1|最近任务/);
			assert.match(observed, /字段|emit|派出/, "observed 必须显示 emits 明细，不只计数");
			await shot(page, "08-detail-diagnostics-fixer");

			const dp = await openCap(page, "deploy-preview");
			const dpConds = await dp.textContent('[data-sec="conditions"]');
			assert.match(dpConds, /ValueMissing/, "deploy-preview 的 NodesAvailable 必须给 ValueMissing 原因");
			const dpResolved = await dp.textContent('[data-sec="resolved"]');
			assert.match(dpResolved, /终端能力/, "next=[] 必须显示终端能力空态");

			return "fixer 四条件+resolved+emits；deploy-preview ValueMissing";
		});

		/* ---------- 8) DELETE CAS：旧 rv 409 重载 + 二次确认 ---------- */
		await check("删除 CAS：旧 rv → 409 重新载入并二次确认；成功 DELETE 带 ?resourceVersion=", async () => {
			const row = await openCap(page, "lint-fix");
			await row.locator('[data-act="delete"]').click();
			await page.waitForSelector("#del-modal:not(.hidden)");
			const freshRv = await page.evaluate(async () => {
				const g = await fetch("/v1/namespaces/default/capabilities/lint-fix");
				const res = await g.json();
				const rv = res.metadata.resourceVersion;
				await fetch("/v1/namespaces/default/capabilities/lint-fix", {
					method: "PATCH",
					headers: { "Content-Type": "application/json" },
					body: JSON.stringify({ apiVersion: "anotify/v1", kind: "Capability", metadata: { name: "lint-fix", resourceVersion: rv, displayName: "并发改动" }, spec: res.spec }),
				});
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
			await page.waitForFunction(() => !document.querySelector('[data-cap="lint-fix"]'), { timeout: 15000 });
			const recs = await takeRec(page);
			const del = recs.find((r) => r.method === "DELETE" && /lint-fix/.test(r.url));
			assert.ok(del, "必须发出 DELETE …/capabilities/lint-fix");
			assert.match(del.url, /resourceVersion=/);
			const param = new URL(del.url, base).searchParams.get("resourceVersion");
			assert.equal(param, freshRv, "二次确认必须用重新载入后的最新 rv");
			return "409 后二次确认；DELETE rv=" + param;
		});

		/* ---------- 9) prod 命名空间诊断：missing 下游 + NodeOffline + pruned ---------- */
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
			assert.match(rsObserved, /param_missing|剪枝/);
			await shot(page, "09-detail-diagnostics-prod-missing");
			const pd = await openCap(page, "prod-deploy");
			const pdConds = await pd.textContent('[data-sec="conditions"]');
			assert.match(pdConds, /NodeOffline/, "prod-deploy 的 NodesAvailable 必须给 NodeOffline");
			return "report.send NotFound/pruned；prod-deploy NodeOffline";
		});

		/* ---------- 10) 四语言页面：无裸 key ---------- */
		await check("四语言页面：?lang=en/ja/es/zh-CN 标题与按钮已翻译，页面无裸 caps.* key", async () => {
			const expect = { "zh-CN": "能力", en: "Capabilities", ja: "能力", es: "Capacidades" };
			await page.evaluate(() => {
				try {
					localStorage.setItem("anotify.scope", JSON.stringify({ namespace: "default" }));
				} catch (e) {}
			});
			for (const lang of ["zh-CN", "en", "ja", "es"]) {
				await page.goto(base + "/capabilities.html?lang=" + lang, { waitUntil: "domcontentloaded", timeout: 20000 });
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
