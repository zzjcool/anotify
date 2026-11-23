// #548 预案页两级化的浏览器事实（real authority：frontend 套件用真 server + 真 Chrome）。
//
// 三段：
//   ① 两份 release 并存 → 包页上各占一行（每行是 `<a>`，行内零 button）；
//   ② 点第一行进详情断言对象对、返回、点第二行再断言 —— 两边**不串**（A 的页里没有 B 的
//      对象名，B 的页里没有 A 的，YAML 抽屉同样按 release 分）；
//   ③ 卸载其中一份 → 该 release 从包页消失 → **直接打开旧 URL** 给出明确状态
//      （#rel-state 的「已卸载或从未安装」+ #rel-gone 可见 + 无裸 key + 不是 500）。
//
// 造数走页面请求路径（d.req）：能力 / 源的名字必须含 `{{release.name}}`，否则第二份
// release 会去认领第一份的行 → 409 release_conflict（那是 #505 的用例，不是这条）。
import assert from "node:assert/strict";
import { acceptDialog } from "./f3-dialogs.mjs";

const NS = "/v1/namespaces/default";
const PKG = "f3-release-pkg";
const BASE = NS + "/playbooks/" + PKG;
const QUERY = "namespace=default&id=" + PKG;
const BARE_KEY = /\b(?:pb\.v4|rel)\.[a-z_]+/;

const files = {
  "playbook.yaml": `apiVersion: anotify/v1
kind: Playbook
metadata:
  name: ${PKG}
  displayName: Two releases
  version: 1
spec:
  values:
    type: object
    required: [project]
    properties:
      project: { type: string, pattern: '^[a-z][a-z0-9-]*$' }
  capabilities:
    - capabilities/review.yaml
    - capabilities/fixer.yaml
  sources:
    - sources/on-push.yaml
`,
  "capabilities/review.yaml": `apiVersion: anotify/v1
kind: Capability
metadata:
  name: '{{release.name}}-review'
spec:
  next: ['{{release.name}}-fixer']
  run:
    entrypoint: run.sh
    files:
      run.sh: |
        #!/bin/sh
        echo 'release={{release.name}} project={{values.project}}'
`,
  "capabilities/fixer.yaml": `apiVersion: anotify/v1
kind: Capability
metadata:
  name: '{{release.name}}-fixer'
spec:
  next: []
  run:
    entrypoint: run.sh
    files:
      run.sh: |
        #!/bin/sh
        echo 'fix {{release.name}}'
`,
  "sources/on-push.yaml": `apiVersion: anotify/v1
kind: Source
metadata:
  name: '{{release.name}}-push'
spec:
  kind: webhook
  config: {}
  targets:
    - capability: '{{release.name}}-review'
      params: { repo: '$(payload.repo)' }
`,
};

export async function releaseDetail(d) {
  const { page, req, calls, check, shot, clean } = d;
  const goto = (file, query, lang = "en") => d.goto(file, query, lang);
  const detailQuery = rel => QUERY + "&release=" + encodeURIComponent(rel);
  const detailPath = rel => BASE + "/releases/" + encodeURIComponent(rel);
  const t = async (key, vars = {}) => {
    const text = await page.evaluate(k => window.AnotifyI18n?.[k], key);
    assert.equal(typeof text, "string", "运行时字典必须有 " + key);
    return Object.entries(vars).reduce((s, [k, v]) => s.replaceAll("{" + k + "}", String(v)), text);
  };
  const detailText = async () => (await page.locator("#rel-body").innerText()).replace(/\s+/g, " ");

  // ---------- 造数：一个包 + 两份 release（a / b），各自一套名字 ----------
  await goto("playbooks");
  await req(NS + "/playbooks", {
    method: "POST",
    body: JSON.stringify({ apiVersion: "anotify/v1", kind: "Playbook", metadata: { name: PKG, namespace: "default" }, spec: { files } }),
  });
  const values = { project: "demo" };
  const first = await req(BASE + "/install", { method: "POST", body: JSON.stringify({ release: "a", values }) });
  const second = await req(BASE + "/install", { method: "POST", body: JSON.stringify({ release: "b", values }) });
  assert.equal(first.install.release, "a", "第一次安装落在 release a");
  assert.equal(second.install.release, "b", "第二次安装落在 release b");
  assert.deepEqual((first.created.capabilities || []).slice().sort(), ["a-fixer@1", "a-review@1"], "a 只装出自己的能力");
  assert.deepEqual((second.created.capabilities || []).slice().sort(), ["b-fixer@1", "b-review@1"], "b 只装出自己的能力（没有 409，名字各归各的）");
  // 前置事实（页面断言拿它当期望值，不写死）：实例清单里恰好 2 组（a / b 各一套参数）。
  const installs = await req(BASE + "/installs");
  assert.equal(installs.total, 2, "实例清单里恰好 2 组（a / b 各一套参数）");
  assert.deepEqual(installs.installs.map(g => g.release).sort(), ["a", "b"]);

  // ---------- ① 包页：一行一个 release，行内零 button ----------
  await goto("playbook", QUERY);
  await page.locator("#pb-installs a[data-release]").first().waitFor();
  const rows = page.locator("#pb-installs a[data-release]");
  assert.equal(await rows.count(), 2, "两份 release 并存 → 包页恰好 2 行");
  assert.deepEqual(await rows.evaluateAll(ns => ns.map(n => n.dataset.release).sort()), ["a", "b"], "行身份就是 release 名");
  assert.equal(await page.locator("#pb-installs button").count(), 0, "#548：列表行内不得有任何按钮（YAML/回执/卸载都搬去详情页）");
  assert.equal(await page.locator("#pb-installs [data-action]").count(), 0, "列表行内不得带任何动作属性");
  for (const rel of ["a", "b"]) {
    const href = await page.locator(`#pb-installs a[data-release="${rel}"]`).getAttribute("href");
    assert.equal(href, "release.html?namespace=default&id=" + PKG + "&release=" + rel, "行链接是可独立打开的详情深链（" + rel + "）");
  }
  assert.match(await page.locator("#pb-installs").innerText(), /2 instance/, "列表头给出实例数（2）");
  await shot("release-two-rows");

  // ---------- ② 点第一行 → 详情断言对象对（且不含另一份的对象） ----------
  await page.locator('#pb-installs a[data-release="a"]').click();
  // 谓词而不是 glob：glob 里的 `?` 是单字符通配，参数顺序一变就成了碰运气。
  await page.waitForURL(url => url.pathname.endsWith("/release.html") && url.searchParams.get("id") === PKG && url.searchParams.get("release") === "a", { waitUntil: "domcontentloaded" });
  await page.locator('#release-detail[data-release="a"]').waitFor();
  assert.equal(await page.locator("#release-detail").getAttribute("data-release"), "a", "详情页身份来自那一发 GET（不是 URL 直接抄）");
  assert.ok((await page.locator("#rel-name").innerText()).includes(PKG + "@1"), "头部身份 = 包名@版本：" + await page.locator("#rel-name").innerText());
  const aText = await detailText();
  for (const want of ["a-review", "a-fixer", "a-push"]) assert.ok(aText.includes(want), "a 的详情必须列出 " + want + "：" + aText);
  for (const other of ["b-review", "b-fixer", "b-push"]) assert.ok(!aText.includes(other), "#548 验收 3：a 的详情里不得出现 b 的对象 " + other + "：" + aText);
  // 一次 GET 拿全：除这一发详情请求外不再分别拉 capabilities / sources / installs。
  const aCalls = (await calls()).filter(c => c.url.startsWith(NS));
  assert.ok(aCalls.some(c => c.httpStatus === 200 && c.url === detailPath("a")), "详情页真的 GET 了 …/releases/a：" + JSON.stringify(aCalls.map(c => c.url)));
  assert.deepEqual(
    aCalls.filter(c => /\/capabilities$|\/sources$|\/installs$/.test(c.url)).map(c => c.url),
    [],
    "详情页不得再分别拉 capabilities/sources/installs（一次 GET 拿全）",
  );
  assert.ok(await page.locator('#rel-body a[href^="source-detail.html?"]').count() >= 1, "源引用必须链到现存的 source-detail 页");
  assert.match(await page.locator('#rel-body a[href^="source-detail.html?"]').first().getAttribute("href"), /^source-detail\.html\?namespace=default&name=a-push$/);
  assert.equal(await page.locator('#rel-body a[href^="source.html"]').count(), 0, "不得再链回已删除的 source.html");
  assert.ok((await page.locator("#rel-body").innerText()).includes("a-review@1"), "回执区列出 created 的 a-review@1");
  assert.equal(await page.locator("#rel-state").isVisible(), false, "成功态 #rel-state 不占位（内容交给 #rel-body）");
  assert.equal(await page.locator("#rel-gone").count(), 0, "成功态没有已卸载块");
  assert.ok(!BARE_KEY.test(await page.locator("#page-main").innerText()), "详情页没有漏翻的 i18n 裸 key");
  // YAML 抽屉也按 release 分：a 的正文里不该出现 b 的对象。
  await page.locator("#rel-yaml-open").click();
  await page.locator("#pb-yaml-drawer").waitFor({ state: "visible" });
  await page.locator("#pb-yaml-text").waitFor();
  const aYaml = await page.locator("#pb-yaml-text").evaluate(node => node.textContent);
  for (const want of ["name: a-review", "name: a-fixer", "name: a-push"]) assert.ok(aYaml.includes(want), "a 的实例 YAML 缺 " + want);
  assert.ok(!aYaml.includes("b-push") && !aYaml.includes("b-review"), "a 的实例 YAML 里不得混进 b 的对象：" + aYaml);
  await shot("release-a-yaml");
  await page.locator("#pb-yaml-close").click();
  await page.locator("#pb-yaml-drawer").waitFor({ state: "hidden" });

  // ---------- ② 返回包页 → 点第二行 → 对象对，且不串 ----------
  await page.locator("#rel-back").click();
  await page.waitForURL(url => url.pathname.endsWith("/playbook.html") && url.searchParams.get("id") === PKG, { waitUntil: "domcontentloaded" });
  await page.locator("#pb-installs a[data-release]").first().waitFor();
  await page.locator('#pb-installs a[data-release="b"]').click();
  await page.waitForURL(url => url.pathname.endsWith("/release.html") && url.searchParams.get("id") === PKG && url.searchParams.get("release") === "b", { waitUntil: "domcontentloaded" });
  await page.locator('#release-detail[data-release="b"]').waitFor();
  assert.equal(await page.locator("#release-detail").getAttribute("data-release"), "b", "第二行进的是 b 的详情");
  const bText = await detailText();
  for (const want of ["b-review", "b-fixer", "b-push"]) assert.ok(bText.includes(want), "b 的详情必须列出 " + want + "：" + bText);
  for (const other of ["a-review", "a-fixer", "a-push"]) assert.ok(!bText.includes(other), "#548 验收 3：b 的详情里不得出现 a 的对象 " + other + "：" + bText);
  await shot("release-b-detail");
  await clean("release detail");
  check("#548 two releases: package page lists 2 link-only rows, each detail GET returns only its own objects (no cross-leak), YAML is per-release");

  // ---------- ③ 卸载 b → 从包页消失 → 旧 URL 明确状态 ----------
  const uninstalledLine = await t("rel.uninstalled", { release: "b" });
  await acceptDialog(page, () => page.locator("#rel-uninstall").click());
  await page.waitForFunction(
    () => location.pathname.endsWith("/playbook.html") && (document.getElementById("pb-uninstall-result")?.innerText || "").trim().length > 0,
  );
  await page.locator("#pb-installs a[data-release]").first().waitFor();
  assert.equal(await page.locator("#pb-installs a[data-release]").count(), 1, "卸载 b 之后包页只剩 1 行");
  assert.equal(await page.locator("#pb-installs a[data-release]").first().getAttribute("data-release"), "a", "留下的是 a");
  assert.ok((await page.locator("#pb-uninstall-result").innerText()).includes(uninstalledLine), "回包页后给出本地化的「已卸载」结果：" + await page.locator("#pb-uninstall-result").innerText());
  await shot("release-after-uninstall");

  // 直接打开旧 URL（还是原来那条 query）：必须是明确状态，不是白屏、不是 500、不是裸 key。
  await goto("release", detailQuery("b"));
  await page.locator("#rel-gone").waitFor({ state: "visible" });
  const goneText = await page.locator("#rel-state").innerText();
  assert.ok(goneText.includes(await t("rel.gone")), "#rel-state 必须写「已卸载或从未安装」：" + goneText);
  assert.equal(await page.locator("#rel-gone").isVisible(), true, "已卸载态给 #rel-gone 空态块");
  assert.equal(await page.locator("#release-detail").getAttribute("data-release"), null, "已卸载态不得再声称自己是那个实例");
  assert.equal(await page.locator("#rel-uninstall").isDisabled(), true, "已卸载态没有卸载动作");
  assert.equal(await page.locator("#rel-yaml-open").isDisabled(), true, "已卸载态没有 YAML 动作");
  assert.ok(!BARE_KEY.test(await page.locator("#page-main").innerText()), "已卸载态没有漏翻的 i18n 裸 key");
  const statuses = (await calls()).filter(c => c.url.startsWith(NS)).map(c => c.httpStatus);
  assert.ok(statuses.includes(404), "旧 URL 的读请求是 404（不是 500）：" + JSON.stringify(statuses));
  assert.ok(!statuses.includes(500) && !statuses.includes(501), "旧 URL 不得撞 500/501：" + JSON.stringify(statuses));
  await page.locator('#rel-gone a[href^="playbook.html?"]').first().waitFor();
  await shot("release-gone-direct-url");
  await clean("release gone");
  check("#548 uninstalled release leaves the package list and its old URL renders the explicit gone state (localized, 404 not 500, no bare keys)");
}
