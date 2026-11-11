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

		/* ---------- 2) 编辑：改内容 → 新版本（POST，内容寻址） ----------
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

		/* ---------- 3) CAS 409：并发写后旧 rv，输入保留 ---------- */
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
			// 显式版本号的内容变更同样是 POST（内容面不可变，永远不 PATCH）。
			const patch = recs.find((r) => r.method === "POST" && /\/capabilities$/.test(r.url) && r.body.metadata.version === 3);
			assert.ok(patch, "显式版本 3 的内容变更必须 POST 新版本（body metadata.version=3）");
			const got = await page.evaluate(async () => (await fetch("/v1/namespaces/default/capabilities/analyze-pr?includeBody=true")).json());
			assert.equal(got.metadata.version, 3);
			assert.ok(Object.values(got.spec.run.files).some((body) => String(body).includes("changed-body")), "v3 的正文必须是本次输入");
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
				// 页面模型是上一次加载的快照（节点是这条用例里现起的）→ 用页面自己的刷新动作重读
				// 列表，让读模型里出现这台节点（用户点「刷新」也是同一回事）。
				const listed = page.waitForResponse(
					(r) => {
						try {
							return new URL(r.url()).pathname === "/v1/namespaces/default/capabilities" && r.status() === 200;
						} catch (e) {
							return false;
						}
					},
					{ timeout: 15000 },
				);
				await page.click("#caps-refresh");
				await listed;
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
