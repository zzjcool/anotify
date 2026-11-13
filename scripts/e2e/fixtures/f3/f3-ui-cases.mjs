// F3b2b: registered UI-shift matrix + retained UX19–23 browser cases.
// Explicit preview authority only under E2E_MOCK_ONLY=1; the default is the real server.
import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import path from "node:path";
import * as H from "../../lib/harness.mjs";
import {
  ANCHORS, DATA_PAGES, KEY_PAGES, LANGS, MATRIX_VIEWS, PAGES, STANDALONE, THEMES, VIEWPORTS,
  auditPage, fixtures, gotoPage, matrixCases, openDriver, shoot, viewUrl, greetGuest, greetMember, passiveUiProbe,
} from "./f3-ui-helper.mjs";

export const GRID = matrixCases();
const shots = KEY_PAGES.map(page => ({ id: "shots-" + page, page }));
const SUITES = Object.freeze({
  ui_shift: Object.freeze([
    ...GRID.map(c => ({ id: c.id, kind: "grid", pages: c.pages, lang: c.lang, theme: c.theme, viewport: c.viewport, views: c.pages.length })),
    { id: "shift-lang-hint", kind: "shift", lang: "zh-CN", views: 1 },
    { id: "shift-scope-slot", kind: "shift", lang: "zh-CN", views: 1 },
    { id: "shift-collapsed", kind: "shift", lang: "zh-CN", views: 1 },
    { id: "shift-prelayout-expanded", kind: "shift", lang: "zh-CN", views: 1 },
    { id: "shift-prelayout-collapsed", kind: "shift", lang: "zh-CN", views: 1 },
    { id: "interact-namespace-switch", kind: "interact", lang: "zh-CN", views: 1 },
    { id: "interact-dialogs", kind: "interact", lang: "zh-CN", views: 1 },
    { id: "interact-error-states", kind: "interact", lang: "zh-CN", views: 2 },
    { id: "interact-error-403", kind: "interact", lang: "zh-CN", views: 1 },
    { id: "retained-auth-shell", kind: "retained", lang: "en", views: 1 },
    { id: "retained-graph", kind: "retained", lang: "en", views: 1 },
    { id: "guards-guest", kind: "guard", lang: "zh-CN", views: 6 },
    /* #384：390 视口下四类主内容（keys/admin 表 / 链路图 / 热力图 / docs 代码块）
       必须「不用横滚就能读完」——默认态零横向滚动容器。 */
    {
      id: "mobile-390-fits",
      kind: "mobile",
      pages: ["keys", "admin", "index", "docs", "trace", "source"],
      lang: "zh-CN",
      theme: "dark",
      viewport: "mobile",
      views: 6,
    },
    ...shots.map(s => ({ id: s.id, kind: "shots", lang: "zh-CN", page: s.page, views: 4 })),
  ]),
  ux_regression_19_23: Object.freeze([
    { id: "ux19-member-admin", kind: "ux", views: 1 },
    { id: "ux20-notification-denied", kind: "ux", views: 1 },
    { id: "ux21-authme-identity", kind: "ux", views: 1 },
    { id: "ux22-passkey-empty-enrollment", kind: "ux", views: 1 },
    { id: "ux23-zero-devices-honest", kind: "ux", views: 1 },
    { id: "retained-device-patch", kind: "ux", views: 1 },
    { id: "retained-guards", kind: "guard", views: 6 },
    { id: "retained-login-form", kind: "guard", views: 1 },
  ]),
});

export function casesFor(suite) {
  const cases = SUITES[suite];
  assert.ok(cases, "registered suite " + suite);
  assert.equal(new Set(cases.map(c => c.id)).size, cases.length, "unique case ids in " + suite);
  return cases.map(c => ({ ...c }));
}
export function registeredViews(suite) {
  const cases = casesFor(suite);
  if (suite !== "ui_shift") return { views: cases.reduce((n, c) => n + c.views, 0), matrix: 0 };
  const matrix = cases.filter(c => c.kind === "grid").reduce((n, c) => n + c.pages.length, 0);
  assert.equal(matrix, MATRIX_VIEWS, "registered matrix views");
  return { views: cases.reduce((n, c) => n + c.views, 0), matrix };
}

const counter = () => ({ n: 0, inc() { this.n++; } });
async function withDriver(id, options, fn) {
  const d = await openDriver(id, options);
  const count = counter();
  try { return await fn(d, count); }
  finally { await d.stop(); }
}
const ok = (checks, views, extra = {}) => ({ ok: true, checks, report: { errors: [], views, ...extra } });
// Reads the live call log; archiving (which clears it) only happens after a read.
const writeWrites = async d => (await d.calls()).filter(x => x.method !== "GET");

// ---------- matrix: 19 pages x 4 langs x light/dark x desktop1440/mobile390 ----------
async function gridCase(c) {
  return withDriver(c.id, { lang: c.lang, theme: c.theme }, async (d, count) => {
    await d.page.setViewportSize(VIEWPORTS[c.viewport]);
    const f = await fixtures(d);
    const pages = [];
    for (const page of c.pages) {
      const guest = page === "login";
      const errorsFrom = d.report.errors.length;
      try {
        await gotoPage(d, page, f, c.lang, { guest });
        pages.push(await auditPage(d, { page, lang: c.lang, theme: c.theme, viewport: c.viewport, guest, anchor: ANCHORS[page] }, errorsFrom));
        count.inc();
      } catch (error) {
        pages.push({ page, lang: c.lang, theme: c.theme, viewport: c.viewport, guest, attempted: true, ok: false, error: String(error.message || error).slice(0, 500), viewErrors: d.report.errors.slice(errorsFrom).slice(0, 4) });
      }
    }
    await d.finish();
    const failed = pages.filter(p => !p.ok);
    return { ok: failed.length === 0, checks: count.n, error: failed.length ? failed.map(p => p.page + ": " + p.error).join(" | ").slice(0, 900) : null, report: { errors: [], pages } };
  });
}

// ---------- #384 · 390：四类主内容不再需要横滚 ----------
// 断言的是 issue 的四条验收（keys/admin 行式卡片、链路图 3 节点 + 「还有 N →」、
// 热力图一行摘要、docs 代码块软换行），全部量真实 DOM 几何；另加两条全局约束：
//   ① documentElement.scrollWidth == innerWidth（页面级不横滚）；
//   ② #page-main 里默认态下**没有任何**横向滚动容器 —— 不是「有提示就算过」，
//      而是主内容根本不该需要滚（保留的横滚只允许是用户主动打开的完整视图，
//      那条路径由 auditPage 的「白名单 + 可见提示」断言守）。
async function mobileFitsCase(c) {
  return withDriver(c.id, { lang: c.lang, theme: c.theme }, async (d, count) => {
    await d.page.setViewportSize(VIEWPORTS.mobile);
    const f = await fixtures(d);
    const pages = [];
    for (const page of c.pages) {
      const errorsFrom = d.report.errors.length;
      const view = { page, lang: c.lang, theme: c.theme, viewport: c.viewport, attempted: true, ok: false };
      try {
        await gotoPage(d, page, f, c.lang);
        const facts = await d.page.evaluate(() => {
          const main = document.getElementById("page-main") || document.body;
          const scrollers = [];
          for (const el of main.querySelectorAll("*")) {
            const cs = getComputedStyle(el);
            if (cs.overflowX !== "auto" && cs.overflowX !== "scroll") continue;
            if (el.scrollWidth - el.clientWidth <= 2) continue;
            const cls = String(el.className || "").trim().split(/\s+/).slice(0, 2).join(".");
            scrollers.push(el.tagName.toLowerCase() + (el.id ? "#" + el.id : "") + (cls ? "." + cls : ""));
          }
          const visible = (el) => {
            if (!el) return null;
            const r = el.getBoundingClientRect();
            const cs = getComputedStyle(el);
            return cs.display !== "none" && cs.visibility !== "hidden" && r.width > 0 && r.height > 0 ? r : null;
          };
          const compact = main.querySelector(".flograph-compact");
          const more = compact ? compact.querySelector(".flograph-compact-more") : null;
          const graphScroll = main.querySelector(".flograph-scroll");
          const hmScroll = main.querySelector(".hm-scroll");
          const hmSummary = main.querySelector("#hm-summary");
          const codeOver = Array.from(main.querySelectorAll(".code-body"))
            .filter((el) => el.getBoundingClientRect().width > 0)
            .map((el) => el.scrollWidth - el.clientWidth)
            .filter((n) => n > 1);
          // 行尾操作按钮：跳过「你自己」这种没有按钮的行（admin 的 demo 行）
          const lastBtn = main.querySelector("tbody tr td:last-child button");
          const lastRect = lastBtn ? lastBtn.getBoundingClientRect() : null;
          return {
            docW: document.documentElement.scrollWidth,
            innerW: innerWidth,
            scrollers,
            steps: compact ? compact.querySelectorAll(".flograph-compact-step").length : 0,
            moreText: more ? more.textContent.trim() : "",
            graphCompactVisible: !!visible(compact),
            graphScrollVisible: !!visible(graphScroll),
            hmSummaryVisible: !!visible(hmSummary),
            hmSummaryText: hmSummary ? hmSummary.textContent.trim() : "",
            hmScrollVisible: !!visible(hmScroll),
            codeOver,
            rowActionRight: lastRect ? Math.round(lastRect.right) : null,
            rowActionW: lastRect ? Math.round(lastRect.width) : null,
          };
        });
        const label = page + "\u00b7390";
        assert.ok(facts.docW <= facts.innerW + 1, label + ": document horizontal overflow " + facts.docW + ">" + facts.innerW);
        assert.deepEqual(facts.scrollers, [], label + ": \u9ed8\u8ba4\u6001\u4e3b\u5185\u5bb9\u4e0d\u8be5\u6709\u4efb\u4f55\u6a2a\u5411\u6eda\u52a8\u5bb9\u5668: " + JSON.stringify(facts.scrollers));
        assert.deepEqual(facts.codeOver, [], label + ": \u4ee3\u7801\u5757\u4e0d\u5e94\u628a\u5185\u5bb9\u85cf\u5230\u53f3\u8fb9\u754c\u5916: " + JSON.stringify(facts.codeOver));
        if (page === "keys" || page === "admin") {
          assert.ok(facts.rowActionRight !== null && facts.rowActionW > 0, label + ": \u884c\u5185\u64cd\u4f5c\u6309\u94ae\u5fc5\u987b\u5b58\u5728\u4e14\u53ef\u89c1");
          assert.ok(facts.rowActionRight <= facts.innerW, label + ": \u64cd\u4f5c\u5217\u4e0d\u8be5\u9700\u8981\u6a2a\u6eda\u624d\u80fd\u770b\u5230\uff08right=" + facts.rowActionRight + " > " + facts.innerW + "\uff09");
        }
        if (page === "index" || page === "trace" || page === "source") {
          assert.equal(facts.graphCompactVisible, true, label + ": \u94fe\u8def\u56fe 390 \u4e0b\u5e94\u662f\u7d27\u51d1\u89c6\u56fe\uff083 \u8282\u70b9 + \u8fd8\u6709 N\uff09");
          assert.equal(facts.steps, 3, label + ": \u7d27\u51d1\u89c6\u56fe\u53ea\u753b 3 \u4e2a\u8282\u70b9\uff0cgot " + facts.steps);
          assert.equal(facts.graphScrollVisible, false, label + ": \u9ed8\u8ba4\u6001\u4e0d\u663e\u793a\u5b8c\u6574\u56fe\uff081102px \u753b\u5e03\uff09");
          if (facts.moreText) assert.match(facts.moreText, /\d/, label + ": \u300c\u8fd8\u6709 N \u2192\u300d\u5fc5\u987b\u5e26\u6570\u91cf\uff0cgot " + JSON.stringify(facts.moreText));
        }
        if (page === "index") {
          assert.equal(facts.hmSummaryVisible, true, label + ": \u70ed\u529b\u56fe\u6298\u53e0\u6210\u4e00\u884c\u6458\u8981");
          assert.ok(facts.hmSummaryText.length > 0 && facts.hmSummaryText !== "\u2014", label + ": \u6458\u8981\u8981\u6709\u5185\u5bb9\uff0cgot " + JSON.stringify(facts.hmSummaryText));
          assert.equal(facts.hmScrollVisible, false, label + ": \u9ed8\u8ba4\u6001\u4e0d\u663e\u793a\u5b8c\u6574 53 \u5468");
        }
        assert.deepEqual(d.report.errors.slice(errorsFrom), [], label + ": pageerror/console/network \u5931\u8d25");
        view.ok = true;
        count.inc();
        pages.push(view);
      } catch (error) {
        view.error = String(error.message || error).slice(0, 500);
        pages.push(view);
      }
    }
    await d.finish();
    const failed = pages.filter((p) => !p.ok);
    return {
      ok: failed.length === 0,
      checks: count.n,
      error: failed.length ? failed.map((p) => p.page + ": " + p.error).join(" | ").slice(0, 900) : null,
      report: { errors: [], views: pages, pages },
    };
  });
}

// ---------- retained shift anchors, upgraded to the v4 page facts ----------
async function shiftLangHint(c) {
  return withDriver(c.id, { lang: "en-US", theme: "light" }, async (d, count) => {
    if (process.env.E1B_PASSIVE_UI === "1") await d.context.addInitScript(passiveUiProbe);
    await d.context.addInitScript(shiftProbe);
    await d.page.goto(viewUrl(d, "index", {}, "zh-CN"), { waitUntil: "load" });
    await d.page.waitForFunction(() => !!document.getElementById("sidebar"), null, { timeout: 9000 }).catch(() => {});
    await d.page.waitForTimeout(1500);
    const probe = await readProbe(d.page);
    if (process.env.E1B_PASSIVE_UI === "1") {
      d.report.passive = await d.page.evaluate(() => { window.__e1bPassive.stop=true;return window.__e1bPassive; });
      d.report.originalShift = probe;
      console.log("E1B_PASSIVE " + JSON.stringify({frames:d.report.passive.frames.length,events:d.report.passive.events.length,http:d.report.passive.http,richReport:"report.json passive + originalShift (no frame removed)"}));
    }
    const banner = await d.page.evaluate(() => {
      const b = document.querySelector(".lang-hint");
      if (!b) return null;
      const r = b.getBoundingClientRect();
      return { h: Math.round(r.height * 100) / 100, open: b.classList.contains("lang-hint--open") };
    });
    assert.ok(banner && banner.h > 0 && banner.open === true, "en-US visit of the Chinese page shows the in-flow language hint: " + JSON.stringify(banner));
    const nav = nonNull(probe.nav), shell = nonNull(probe.shell);
    assert.ok(nav.length > 0 && uniq(nav).length === 1, ".app-nav y is single-valued from the first frame: " + seq(nav));
    assert.ok(shell.length > 0 && uniq(shell).length === 1, "#app-shell y is single-valued from the first frame: " + seq(shell));
    assert.ok(Math.abs(uniq(shell)[0] - banner.h) <= 1, "shell offset equals the in-flow banner height (" + uniq(shell)[0] + " vs " + banner.h + ")");
    assert.ok(probe.frames >= 60, "sampling window covers first frame +1.5s: " + probe.frames + " frames");
    await d.finish(); count.inc();
    return ok(count.n, [{ page: "index", lang: "zh-CN", theme: "light", viewport: "desktop", attempted: true, ok: true, frames: probe.frames }]);
  });
}
async function shiftScopeSlot(c) {
  return withDriver(c.id, { lang: "zh-CN", theme: "light" }, async (d, count) => {
    await d.context.addInitScript(shiftProbe);
    await d.page.goto(viewUrl(d, "index", {}, "zh-CN"), { waitUntil: "load" });
    await d.page.waitForFunction(() => { const sw = document.getElementById("scope-switcher"); return !!sw && !sw.classList.contains("hidden"); }, null, { timeout: 9000 }).catch(() => {});
    await d.page.waitForTimeout(900);
    const probe = await readProbe(d.page);
    const slot = await d.page.evaluate(() => {
      const el = document.getElementById("scope-slot"), sw = document.getElementById("scope-switcher");
      return { h: el ? Math.round(el.getBoundingClientRect().height * 100) / 100 : null, visible: sw ? !sw.classList.contains("hidden") : null, trigger: !!document.querySelector("#scope-switcher .scope-trigger, #scope-switcher button"), skel: !!document.querySelector(".scope-skel") };
    });
    assert.ok(slot.visible === true && slot.trigger === true && slot.skel === false, "namespace switcher replaced its skeleton with real data: " + JSON.stringify(slot));
    assert.ok(slot.h !== null && slot.h > 0, "#scope-slot reserves layout height: " + slot.h);
    assert.equal(uniq(nonNull(probe.slotH)).length, 1, "#scope-slot height constant from the first frame: " + seq(nonNull(probe.slotH)));
    assert.equal(uniq(nonNull(probe.slotH))[0], slot.h, "sampled slot height equals the measured slot height");
    const muts = probe.scopeMutations.filter(m => m.navY !== null);
    assert.ok(muts.length >= 2, "class mutation observed before/after namespace data: " + muts.length);
    assert.equal(uniq(muts.map(m => m.navY)).length, 1, "nav.y unchanged across namespace data arrival: " + JSON.stringify(muts.slice(0, 4)));
    assert.equal(uniq(nonNull(probe.nav)).length, 1, "nav.y single-valued across all frames: " + seq(nonNull(probe.nav)));
    await d.finish(); count.inc();
    return ok(count.n, [{ page: "index", lang: "zh-CN", theme: "light", viewport: "desktop", attempted: true, ok: true, slotWidth: slot.h }]);
  });
}
async function shiftCollapsed(c) {
  return withDriver(c.id, { lang: "zh-CN", theme: "light" }, async (d, count) => {
    await d.context.addInitScript(() => { try { localStorage.setItem("anotify-sidebar", "collapsed"); } catch { /* storage off */ } });
    await d.context.addInitScript(shiftProbe);
    await d.page.goto(viewUrl(d, "index", {}, "zh-CN"), { waitUntil: "load" });
    await d.page.waitForFunction(() => !!document.getElementById("sidebar"), null, { timeout: 9000 }).catch(() => {});
    await d.page.waitForTimeout(1500);
    const probe = await readProbe(d.page);
    const geo = await d.page.evaluate(() => ({
      sidebarW: Math.round(document.getElementById("sidebar").getBoundingClientRect().width * 100) / 100,
      collapsed: document.getElementById("app-shell").getAttribute("data-collapsed"),
      preW: getComputedStyle(document.documentElement).getPropertyValue("--pre-sidebar-w").trim(),
      stored: localStorage.getItem("anotify-sidebar"),
    }));
    assert.equal(geo.stored, "collapsed", "collapsed preference retained in storage");
    assert.equal(geo.collapsed, "true", "#app-shell reports a collapsed shell");
    assert.equal(geo.preW, "76px", "pre-layout sidebar width pinned before scripts");
    assert.equal(geo.sidebarW, 76, "collapsed sidebar is 76px wide");
    assert.equal(uniq(nonNull(probe.nav)).length, 1, "collapsed nav.y single-valued: " + seq(nonNull(probe.nav)));
    assert.equal(uniq(nonNull(probe.slotH)).length, 1, "collapsed slot height constant");
    await d.finish(); count.inc();
    return ok(count.n, [{ page: "index", lang: "zh-CN", theme: "light", viewport: "desktop", attempted: true, ok: true, sidebarWidth: geo.sidebarW }]);
  });
}
async function shiftPrelayout(c, collapsed) {
  return withDriver(c.id, { lang: "zh-CN", theme: "light" }, async (d, count) => {
    const { attachGeometry, recordGeometry, assertPrelayoutGeometry } = await import("./parent-geometry-probe.mjs");
    await attachGeometry(d);
    if (collapsed) await d.context.addInitScript(() => { try { localStorage.setItem("anotify-sidebar", "collapsed"); } catch { /* storage off */ } });
    await d.context.addInitScript(shiftProbe);
    const f = await fixtures(d);
    let delayed = 0;
    // The shell script is fingerprinted in built sites; match both names so the
    // delay hook cannot silently stop applying (that would fake a green).
    await d.page.route(/\/partials(\.[0-9a-f]+)?\.js$/, async route => { delayed++; await new Promise(r => setTimeout(r, 1200)); await route.continue(); });
    await d.page.goto(viewUrl(d, "index", f, "zh-CN"), { waitUntil: "load" });
    await d.page.waitForFunction(() => !!document.getElementById("sidebar"), null, { timeout: 12000 }).catch(() => {});
    await d.page.waitForTimeout(900);
    const probe = await readProbe(d.page);
    const geo = await d.page.evaluate(() => ({
      sidebarW: Math.round(document.getElementById("sidebar").getBoundingClientRect().width),
      mainX: Math.round(document.getElementById("page-main").getBoundingClientRect().x),
      pageX: document.querySelector(".page") ? Math.round(document.querySelector(".page").getBoundingClientRect().x) : null,
    }));
    const pre = probe.pageMainX.map((x, i) => ({ x, shell: probe.shell[i], page: probe.pageX[i] })).filter(o => o.shell === null && o.x !== null);
    const geometry = await recordGeometry(d, "parent-prelayout", { collapsed, measured: geo, originalPreFrames: pre, originalProbe: probe });
    assert.equal(delayed, 1, "shell script delayed exactly once through a real route hook (fingerprinted name tolerated)");
    assert.ok(pre.length >= 10, "≥10 pre-mounted frames sampled: " + pre.length);
    const verifiedBoundary = assertPrelayoutGeometry(geometry);
    d.report.geometryBoundary = verifiedBoundary;
    assert.equal(uniq(pre.map(o => o.x)).length, 1, "pre-mount #page-main x is single-valued: " + JSON.stringify(uniq(pre.map(o => o.x))));
    if (geo.pageX !== null) assert.equal(uniq(nonNull(probe.pageX)).length, 1, ".page x constant across shell mount: " + seq(nonNull(probe.pageX)));
    await d.finish(); count.inc();
    return ok(count.n, [{ page: "index", lang: "zh-CN", theme: "light", viewport: "desktop", attempted: true, ok: true, preFrames: pre.length, mainX: geo.mainX }]);
  });
}

// ---------- real in-page interactions ----------
async function interactNamespaceSwitch(c) {
  return withDriver(c.id, { lang: "zh-CN", theme: "light" }, async (d, count) => {
    const { attachGeometry, geometrySnapshot, recordGeometry, assertNamespaceGeometry } = await import("./parent-geometry-probe.mjs");
    await attachGeometry(d);
    const f = await fixtures(d);
    await gotoPage(d, "index", f, "zh-CN");
    const beforeGeometry = await geometrySnapshot(d, "before-native-scope-click");
    const before = await d.page.evaluate(() => ({ scope: localStorage.getItem("anotify.scope") }));
    const trigger = d.page.locator("#scope-switcher .scope-trigger");
    assert.equal(await trigger.count(), 1, "one actual namespace trigger");
    await trigger.click();
    await d.page.waitForTimeout(300);
    const target = d.page.locator('#scope-switcher button[role="menuitemradio"][data-ns-id="prod"]');
    assert.equal(await target.count(), 1, "one canonical prod namespace button, not a guessed/first option");
    await d.archive();
    await target.click();
    await d.page.waitForFunction(() => location.search.includes("namespace=prod") || (localStorage.getItem("anotify.scope") || "").includes("prod"), null, { timeout: 9000 });
    const after = await d.page.evaluate(() => ({ scope: localStorage.getItem("anotify.scope"), url: location.href, slot: document.getElementById("scope-slot")?.getBoundingClientRect().height }));
    const atOriginalAfter = await geometrySnapshot(d, "original-after-measurement");
    assert.equal(JSON.parse(after.scope).namespace, "prod", "native click updates namespace storage");
    assert.equal(new URL(after.url).searchParams.get("namespace"), "prod", "native click updates URL namespace");
    await d.page.waitForFunction(() => window.__f3Calls.some(c => c.method === "GET" && (/\/v1\/namespaces\/prod\//.test(c.url) || /[?&]namespace=prod(?:&|$)/.test(c.url))), null, { timeout: 7000 });
    const afterRequests = await geometrySnapshot(d, "after-original-prod-request-wait");
    const geometry = await recordGeometry(d, "parent-namespace-switch", { before, after, beforeGeometry, atOriginalAfter, afterRequests });
    assert.ok((await d.calls()).some(c => c.method === "GET" && (/\/v1\/namespaces\/prod\//.test(c.url) || /[?&]namespace=prod(?:&|$)/.test(c.url))), "next real controller request uses prod context");
    assert.notEqual(after.scope, before.scope, "scope storage changed by the real click, not by a fixture");
    const verifiedMount = assertNamespaceGeometry(geometry);
    d.report.geometryBoundary = verifiedMount;
    await d.finish(); count.inc();
    return ok(count.n, [{ page: "index", lang: "zh-CN", theme: "light", viewport: "desktop", attempted: true, ok: true, scopeAfter: after.scope }]);
  });
}
async function interactDialogs(c) {
  return withDriver(c.id, { lang: "zh-CN", theme: "light" }, async (d, count) => {
    const f = await fixtures(d);
    // Discover the destructive control in a real data row (locale-independent label match).
    let buttons, target = -1, host = null;
    // Keys are retained auth identities; revoke is NOT a namespaced resource delete.
    for (const page of ["capabilities", "sources", "playbooks"]) {
      await gotoPage(d, page, f, "zh-CN");
      /* #376：源列表的「删除源」收进了行内 ⋯ 菜单 —— 菜单未开时不可见即不可点。
         先逐行打开菜单，再只用 `:visible` 找破坏性动作（发现语义不变：仍然只认
         用户真正点得到的控件，隐藏项不再被误当成可点目标）。 */
      if (page === "sources") {
        /* 只开第一行：菜单是浮层，开着的菜单会挡住下面那行的 ⋯，逐行开点不到。 */
        const more = d.page.locator("#src-list [data-name] .act-more").first();
        if (await more.count()) await more.click();
        await d.page.locator("#src-list [data-name] .menu:not(.hidden)").first().waitFor({ state: "visible" });
      }
      const candidates = d.page.locator("#page-main [data-cap] button:visible, #page-main [data-cap] a:visible, #page-main tbody tr button:visible, #page-main .rc-dev-row button:visible, #page-main [data-name] button:visible, #page-main [data-slug] button:visible");
      const total = await candidates.count();
      for (let i = 0; i < total; i++) {
        const text = ((await candidates.nth(i).innerText().catch(() => "")) || "").trim() + " " + ((await candidates.nth(i).getAttribute("aria-label").catch(() => "")) || "");
        if (/删除|Delete|削除|Eliminar|移除|Remove|Revoke|吊销/.test(text)) { buttons = candidates; target = i; host = page; break; }
      }
      if (target >= 0) break;
    }
    assert.ok(target >= 0, "a genuine resource DELETE action exists (capabilities/sources/playbooks); retained Keys coverage stays separate");
    /* #376：源列表的破坏性动作在行内 ⋯ 菜单里，条目点一次就收起 —— 每次点它之前
       先把菜单重新打开（这个助手只对 sources 生效）。 */
    const ensureRowMenu = async () => {
      if (host !== "sources") return;
      const open = d.page.locator("#src-list [data-name] .menu:not(.hidden)").first();
      if (await open.count()) return;
      await d.page.locator("#src-list [data-name] .act-more").first().click();
      await open.waitFor({ state: "visible" });
    };
    const rowSelector = host === "capabilities" ? "#page-main [data-cap]" : host === "keys" ? "#page-main tbody tr" : host === "playbooks" ? "#page-main [data-slug]" : "#page-main [data-name]";
    const rowsBefore = await d.page.locator(rowSelector).count();

    /* #378：原生 confirm 换成了共享弹层 #anotify-dialog（partials.js）。
       旧断言的事实全部保留，并且多钉两件事：
         · 弹层是**真实可点控件**（不是 window.confirm），危险动作用 danger 按钮；
         · 弹层 Esc 可关（C5），关掉同样是零写入。 */
    const dialogOk = d.page.locator("#anotify-dialog [data-dialog-ok]");
    const dialogCancel = d.page.locator("#anotify-dialog [data-dialog-cancel]");
    const dialogBody = d.page.locator("#anotify-dialog-body");
    const waitClosed = () => d.page.locator("#anotify-dialog").waitFor({ state: "hidden", timeout: 5000 });
    const openDialog = async () => {
      await ensureRowMenu();
      await buttons.nth(target).click();
      await dialogOk.waitFor({ state: "visible", timeout: 5000 });
      return { body: (await dialogBody.innerText()).trim(), title: (await d.page.locator("#anotify-dialog-title").innerText()).trim(), cls: (await dialogOk.getAttribute("class")) || "" };
    };

    // ① 取消：零写入、行保留（旧断言）
    const first = await openDialog();
    assert.ok(first.body.length > 0, "danger dialog opened with a real message: " + JSON.stringify(first.body));
    assert.ok(first.title.length > 0, "danger dialog has a visible title: " + JSON.stringify(first.title));
    assert.ok(first.cls.includes("btn-danger"), "destructive actions use the danger button, never a neutral one: " + first.cls);
    await dialogCancel.click();
    await waitClosed();
    assert.deepEqual(await writeWrites(d), [], "dismissed dialog performs zero write");
    const rowsAfterDismiss = await d.page.locator(rowSelector).count();
    assert.equal(rowsAfterDismiss, rowsBefore, "dismissed dialog keeps the row: " + rowsBefore + "→" + rowsAfterDismiss);

    // ② Esc：弹层可关（#378 C5），同样零写入（旧的「重开文案相同」也在这里钉住）
    const second = await openDialog();
    assert.equal(second.body, first.body, "second click repeats the exact same confirm message");
    await d.page.keyboard.press("Escape");
    await waitClosed();
    assert.deepEqual(await writeWrites(d), [], "Esc-dismissed dialog performs zero write");
    assert.equal(await d.page.locator(rowSelector).count(), rowsBefore, "Esc-dismissed dialog keeps the row");

    // ③ 确认：恰好一次真实 DELETE（旧断言）
    const accepted = await openDialog();
    assert.equal(accepted.body, first.body, "confirm message is stable across opens");
    await dialogOk.click();
    await waitClosed();
    /* #378：撤销条只有 5 秒窗口 —— 它比"行消失"更容易错过，所以先钉它（它同时是
       "删除真的成功"的证据：撤销条只在 DELETE + 列表重载都成功后才会弹）。 */
    const undo = d.page.locator("#anotify-undo-host [data-undo-bar]").last();
    await undo.waitFor({ state: "visible", timeout: 4500 });
    const undoText = (await undo.innerText()).replace(/\s+/g, " ").trim();
    const writes = await writeWrites(d);
    assert.equal(writes.length, 1, "accepted dialog issues exactly one real write: " + JSON.stringify(writes.map(x => x.method + " " + x.url)).slice(0, 200));
    assert.equal(writes[0].method, "DELETE", "resource deletion is not Key revocation");
    assert.match(writes[0].url, /\/v1\/namespaces\/default\/(capabilities|sources|playbooks)\//, "write targets the real resource identity: " + writes[0].url);
    assert.ok(new URL(writes[0].url, d.base).searchParams.get("resourceVersion"), "DELETE carries the actual CAS query");
    assert.ok([200, 202, 204].includes(writes[0].httpStatus), "write answered by the wire: " + writes[0].httpStatus);
    const rowsAfter = await d.page.locator(rowSelector).count();
    assert.equal(rowsAfter, rowsBefore - 1, "the deleted row left the live list " + rowsBefore + "→" + rowsAfter);

    // ④ 删除后可撤销（#378）：撤销 = 用删除前的快照重建（不是延迟提交）
    assert.match(undoText, /已删除|Deleted|削除|Eliminad/i, "undo bar names the deleted object: " + undoText);
    assert.match(undoText, /5/, "undo bar states the 5s window: " + undoText);
    await undo.locator("[data-undo-action]").click();
    await d.page.waitForFunction(() => (window.__f3Calls || []).some(c => c.method === "POST" && /\/v1\/namespaces\/default\/(capabilities|sources|playbooks)$/.test(c.url) && c.httpStatus >= 200 && c.httpStatus < 300), null, { timeout: 6000 });
    /* 重建是 POST + 列表重载，等行数回到删除前（不睡固定时长）。 */
    await d.page.waitForFunction(([sel, n]) => document.querySelectorAll(sel).length === n, [rowSelector, rowsBefore], { timeout: 6000 });
    assert.equal(await d.page.locator(rowSelector).count(), rowsBefore, "undo restores the row " + rowsAfter + "→" + rowsBefore);
    assert.deepEqual(await d.page.evaluate(() => Array.from(document.querySelectorAll("[data-error-bar]")).map(n => n.innerText.replace(/\s+/g, " ").trim())), [], "undo did not have to fall back to an error bar");
    await d.finish(); count.inc();
    return ok(count.n, [{ page: host, lang: "zh-CN", theme: "light", viewport: "desktop", attempted: true, ok: true, dialog: first.body, write: writes[0].method + " " + writes[0].url, undo: undoText }]);
  });
}

/* #378：403 的**页级**断言。
 *
 * 为什么这么写：真实服务端上唯一产生 403 的地方是 /v1/admin/*（非管理员会话，
 * internal/auth/admin.go），而**没有任何页面**会在载入时请求 admin 接口
 * （admin.html 自己先看 role，成员看到的是它自己的无权限页）。所以：
 *   ① 先用真实会话向真实服务端要一个真的 403（不是伪造响应）；
 *   ② 再在真实页面里用**页面的 api()**（真实错误对象 → 真实分级 → 真实渲染）
 *      把这次失败画成 loadErrorNode，塞进 #page-main；
 * ③④⑤ 断言的是浏览器里最终看到的那一屏。
 * 这个 case 钉的正是 #378 的原始 bug：403 和 500 曾经共用同一句「请求被拒绝 HTTP 403」。 */
async function interactError403(c) {
  return withDriver(c.id, { lang: "zh-CN", theme: "dark" }, async (d, count) => {
    await d.goto("index");
    if (d.mock) await greetMember(d);
    await d.archive();
    await gotoPage(d, "nodes", await fixtures(d), "zh-CN");
    /* ① 先拿一个 403。
     * 真实权威：服务端 admin 中间件真的拒绝（internal/auth/admin.go:43）。
     * mock 权威：MockApi 没有权限模型（ME 恒为 admin，全库没有 403），mock 预览下
     * 一个 403 都不存在；这里的 403 由本用例在**传输层**给出，响应体逐字复制真实
     * 服务端那条 403（{"error":"需要管理员权限"}）—— 断言的是"403 这一屏"，不是
     * "mock 支持权限"。 */
    /* ① 403 从哪来，按权威分三种（都不是假绿灯，如实记在 view.authority 里）：
     *   · real + member 会话 → 服务端真的拒绝（最有力，端到端）
     *   · real + admin 会话  → admin 本来就不该被拒；构造与真实路径**同形**的错误
     *   · mock 预览          → MockApi 没有权限模型（ME 恒为 admin），同上
     * 后两种情况注入的只是"错误对象"，渲染/分级/文案/DOM 全是生产链路。 */
    const errorsFrom = d.report.errors.length;
    const me = d.mock ? null : await d.req("/v1/auth/me");
    const realMember = !!me && me.role !== "admin";
    if (realMember) {
      const probe = await d.api("/v1/admin/users");
      assert.equal(probe.status, 403, "a member session really is refused the admin API: " + JSON.stringify(probe.body));
    }
    const authority = realMember ? "real-server" : "constructed-same-shape";
    // ② 页面级渲染：走真实 api() 的错误对象 + 真实 loadErrorNode
    const state = await d.page.evaluate(async realMember => {
      const main = document.getElementById("page-main");
      let box = null, status = 0, message = "";
      try {
        /* 非 member 会话（mock 预览 / admin）没有可触发的真 403，见上面的 authority 说明：
           这里构造的错误对象与真实路径产出**同形**（status + 服务端原文），
           之后走的是同一条 apiError → loadErrorNode 生产链路。 */
        if (!realMember) throw Object.assign(new Error("需要管理员权限"), { status: 403 });
        await window.Anotify.api("/v1/admin/users");
      }
      catch (e) { status = e.status; message = String(e.message || ""); box = window.Anotify.loadErrorNode(e); }
      if (!box) return { status, message, rendered: false };
      main.replaceChildren(box);
      return {
        status, message, rendered: true,
        text: (main.innerText || "").replace(/\s+/g, " ").trim(),
        title: (box.querySelector("p") || {}).textContent || "",
        actions: Array.from(box.querySelectorAll("[data-err-action]")).map(n => n.getAttribute("data-err-action")),
        raw: box.querySelectorAll("[data-raw-content]").length,
        scrollWidth: document.documentElement.scrollWidth, inner: innerWidth,
      };
    }, realMember);
    assert.equal(state.status, 403, "the rendered error really carries status 403");
    assert.equal(state.rendered, true, "a 403 renders a real error node");
    assert.match(state.title, /你没有权限执行这个操作/, "403 heading names the situation in plain language: " + state.title);
    assert.match(state.text, /命名空间所有者/, "403 tells the user who to ask: " + state.text);
    assert.match(state.text, /管理员/, "403 also names the instance admin: " + state.text);
    assert.doesNotMatch(state.text, /HTTP ?403|\b403\b/, "no bare HTTP code is shown for a 403: " + state.text);
    assert.doesNotMatch(state.text, /服务端出错|服务端错误|Server error/, "403 never claims a server error: " + state.text);
    assert.ok(state.actions.includes("copy"), "403 keeps copy-details (machine facts stay available): " + JSON.stringify(state.actions));
    assert.ok(state.actions.includes("back"), "403 is not a dead end: it still offers a way out: " + JSON.stringify(state.actions));
    assert.equal(state.actions.includes("retry"), false, "403 offers no pointless retry: " + JSON.stringify(state.actions));
    assert.ok(state.raw >= 1, "the server's own message is still shown as data: " + state.raw);
    assert.ok(state.scrollWidth <= state.inner + 2, "403 error state stays bounded: " + state.scrollWidth + "<=" + state.inner);
    assert.ok(d.report.errors.slice(errorsFrom).every(e => /403|Forbidden/.test(e)), "the only tolerated console entries are the 403s we asked for: " + JSON.stringify(d.report.errors.slice(errorsFrom).slice(0, 3)));
    await d.shot("error-403-page");
    await d.finish(); count.inc();
    return ok(count.n, [{ page: "nodes", lang: "zh-CN", theme: "dark", viewport: "desktop", attempted: true, ok: true, forbidden: state.status, authority, actions: state.actions }]);
  });
}
async function interactErrorStates(c) {
  return withDriver(c.id, { lang: "zh-CN", theme: "dark" }, async (d, count) => {
    const f = await fixtures(d);
    const errorsFrom = d.report.errors.length;
    await d.page.goto(d.base + "/trace.html?namespace=default&traceUID=not-a-real-trace", { waitUntil: "domcontentloaded" });
    await d.page.waitForFunction(() => !!window.Anotify && !document.querySelector('[aria-busy="true"]'), null, { timeout: 9000 }).catch(() => {});
    await d.page.waitForFunction(() => { const m = document.getElementById("page-main"); return !!m && /失败|Failed|错误|Error|not found|不存在/.test(m.innerText || ""); }, null, { timeout: 9000 }).catch(() => {});
    const state = await d.page.evaluate(() => {
      const main = document.getElementById("page-main");
      return {
        text: (main?.innerText || "").replace(/\s+/g, " ").trim().slice(0, 200),
        retry: Array.from(main?.querySelectorAll("button") || []).map(b => (b.textContent || "").trim()).slice(0, 6),
        scrollWidth: document.documentElement.scrollWidth, inner: innerWidth,
        theme: document.documentElement.getAttribute("data-theme"),
        anchor: !!main?.querySelector("#trace-error") && !!main?.querySelector("#trace-error")?.textContent.trim(),
      };
    });
    assert.match(state.text, /失败|Failed|错误|Error|not found|不存在/, "unknown trace shows a genuine error state: " + state.text);
    assert.ok(state.retry.length > 0, "error state keeps a real retry affordance: " + JSON.stringify(state.retry));
    assert.ok(state.anchor, "real visible trace-error region has failure content, not a success-only trace-tasks anchor");
    assert.ok((await d.calls()).some(c => /\/traces\/not-a-real-trace$/.test(c.url) && c.httpStatus === 404), "unknown trace has an actual 404 response");
    assert.ok(state.scrollWidth <= state.inner + 2, "error state stays bounded: " + state.scrollWidth + "<=" + state.inner);
    assert.equal(state.theme, "dark", "error state honours the requested theme");
    const viewErrors = d.report.errors.slice(errorsFrom);
    assert.ok(viewErrors.every(e => /404/.test(e)), "the only tolerated console entries are the real 404s of the missing trace: " + JSON.stringify(viewErrors.slice(0, 3)));
    count.inc();
    await d.finish(); // positive missing-resource UI must remain independently clean
    const negative = await openDriver(c.id + "-expected-unimplemented", { lang: "zh-CN", theme: "light" });
    let guard;
    try {
      await negative.goto("index");
      const probe = await negative.response("/v1/namespaces/default/not-implemented-f3b2b");
      assert.equal(probe.status, d.mock ? 501 : 404, "unknown route is a real non-success response, never functional PASS");
      if (d.mock) {
        assert.ok(negative.report.errors.some(e => e.includes("mock 未实现")), "negative provider preserves the real 501 diagnostic");
        await assert.rejects(negative.finish(), /unimplemented501 must fail/, "the actual finish guard MUST reject expected unimplemented probe");
      } else await negative.finish();
      guard = { identity: { id: negative.id, runId: negative.report.runId, attemptId: negative.report.attemptId }, expectedGuardRejection: d.mock, functionalRoutePass: false, httpStatus: probe.status, errors: [...negative.report.errors], reportPath: negative.report.reportPath };
      count.inc();
    } finally { await negative.stop(); }
    guard.reportPath = negative.report.reportPath;
    return ok(count.n, [
      { page: "trace", lang: "zh-CN", theme: "dark", viewport: "desktop", attempted: true, ok: true, errorState: true },
      { page: "index", lang: "zh-CN", theme: "light", viewport: "desktop", attempted: true, ok: true, unknownProbe: true, guard },
    ]);
  });
}

async function retainedAuthShell(c) {
  return withDriver(c.id, { lang: "en", theme: "light" }, async (d, count) => {
    const f = await fixtures(d);
    const me = await d.req("/v1/auth/me");
    await gotoPage(d, "index", f, "en");
    const shell = await d.page.evaluate(() => ({
      sidebar: !!document.getElementById("sidebar"),
      greeting: (document.getElementById("greeting")?.textContent || "").trim(),
      user: (document.getElementById("sidebar-username")?.textContent || "").trim(),
      scope: !!document.querySelector("#scope-switcher:not(.hidden)"),
      navLinks: document.querySelectorAll("#sidebar a").length,
    }));
    assert.ok(shell.sidebar && shell.navLinks >= 5, "retained shell renders real navigation: " + JSON.stringify(shell));
    assert.ok(shell.greeting.includes(me.displayName) && shell.user.includes(me.displayName), "identity comes from /auth/me: " + JSON.stringify({ greeting: shell.greeting, user: shell.user, expect: me.displayName }));
    assert.equal(shell.scope, true, "namespace switcher retained on the authenticated shell");
    count.inc();
    for (const page of ["capabilities", "nodes", "playbooks", "security", "receivers", "events"]) {
      const errorsFrom = d.report.errors.length;
      await gotoPage(d, page, f, "en");
      const state = await d.page.evaluate(sel => ({ anchor: !!document.querySelector(sel), text: (document.getElementById("page-main")?.innerText || "").replace(/\s+/g, " ").trim().slice(0, 80) }), ANCHORS[page]);
      assert.ok(state.anchor, "retained authenticated page " + page + " keeps its structural anchor");
      assert.ok(state.text.length >= 20, "retained authenticated page " + page + " renders controller content: " + state.text);
      assert.deepEqual(d.report.errors.slice(errorsFrom), [], page + ": no console/network failure");
      count.inc();
    }
    await d.finish();
    return ok(count.n, [{ page: "index+6", lang: "en", theme: "light", viewport: "desktop", attempted: true, ok: true }]);
  });
}
async function retainedGraph(c) {
  return withDriver(c.id, { lang: "en", theme: "dark" }, async (d, count) => {
    const f = await fixtures(d);
    await gotoPage(d, "trace", f, "en");
    await d.page.waitForTimeout(600);
    const graph = await d.page.evaluate(() => {
      const main = document.getElementById("page-main");
      return {
        svg: main ? main.querySelectorAll("svg").length : 0,
        nodes: main ? main.querySelectorAll("[data-node-key], g.node, .graph-node, [data-task]").length : 0,
        text: (main?.innerText || "").replace(/\s+/g, " ").trim().slice(0, 120),
        taskLinks: main ? main.querySelectorAll('a[href*="message.html"]').length : 0,
        canvas: main ? main.querySelectorAll("canvas, [data-graph]").length : 0,
      };
    });
    assert.ok(graph.svg > 0 || graph.nodes > 0 || graph.canvas > 0, "the trace page renders a real graph surface: " + JSON.stringify(graph));
    assert.ok(graph.text.length >= 20, "the trace page renders real task facts: " + graph.text);
    assert.ok(graph.taskLinks > 0, "the trace links to canonical Task.name identities: " + graph.taskLinks);
    await d.finish(); count.inc();
    return ok(count.n, [{ page: "trace", lang: "en", theme: "dark", viewport: "desktop", attempted: true, ok: true, svg: graph.svg }]);
  });
}

const GUARDED = ["index", "keys", "security", "receivers", "capabilities", "nodes"];
// 匿名视图的**真实就绪条件**（#338 flake：全量 3/3 假失败于 guest index/keys/receivers）。
// 旧写法 `waitForFunction(login 路径匹配 || login-panel 存在)` + `.catch(()=>{})` 有两处错：
//   ① 条件里有 `||`，URL 一变到 login.html 就立刻返回，而页面控制器还没把密码表单挂上来
//      —— 紧接着 evaluate 读到 {panel:false,fields:0}，断言假失败（报文恒 panel:false,fields:0）；
//   ② 未登录页会连发好几个 401，每个都执行一次 `location.href = login.html?next=...`，
//      前一次导航被中止时页面执行上下文会被销毁，waitForFunction 整条抛错；
//      `.catch(()=>{})` 把它吞掉后照样读到空 DOM —— 失败被吞，假绿/假红都不受控。
// 所以改成 Node 侧轮询「路径 + 密码表单字段数」两个事实都成立；超时抛**真实断言错误**
// （带最后一次观测到的状态），不 catch 后继续。轮询本身也天然容忍上下文销毁。
const GUEST_FORM_TIMEOUT = 15000; // 单 case 外层预算是 52s，这里留足余量
async function waitGuestPasswordForm(page, label) {
  const deadline = Date.now() + GUEST_FORM_TIMEOUT;
  let last = { path: "(unobserved)", panel: false, fields: 0, error: "" };
  while (Date.now() < deadline) {
    try {
      last = await page.evaluate(() => ({ path: location.pathname + location.search, panel: !!document.getElementById("login-panel"), fields: document.querySelectorAll("#login-panel input").length }));
      if (/login\.html/.test(last.path) && last.panel && last.fields >= 2) return last;
    } catch (error) { last = { ...last, error: String(error).split("\n")[0] }; }
    await new Promise(r => setTimeout(r, 100));
  }
  assert.fail("guest " + label + " never reached the real password form within " + GUEST_FORM_TIMEOUT + "ms: " + JSON.stringify(last));
}
async function guardsGuest(c, pages = GUARDED) {
  return withDriver(c.id, { lang: "zh-CN", theme: "light" }, async (d, count) => {
    await d.goto("index");
    const views = [];
    for (const page of pages) {
      const errorsFrom = d.report.errors.length;
      await greetGuest(d);
      await d.page.goto(viewUrl(d, page, {}, "zh-CN", true), { waitUntil: "domcontentloaded" });
      await waitGuestPasswordForm(d.page, page);
      const state = await d.page.evaluate(() => ({ path: location.pathname + location.search, panel: !!document.getElementById("login-panel"), fields: document.querySelectorAll("#login-panel input").length, sidebar: !!document.getElementById("sidebar") }));
      assert.match(state.path, /login\.html/, "guest " + page + " redirected to the password login, not a blank shell: " + state.path);
      assert.ok(state.panel && state.fields >= 2, "guest " + page + " lands on the real password form: " + JSON.stringify(state));
      assert.equal(state.sidebar, false, "guest " + page + " never renders the authenticated shell");
      assert.deepEqual(d.report.errors.slice(errorsFrom).filter(e => !/401/.test(e)), [], "guest " + page + " only reports the deliberate 401");
      views.push({ page, lang: "zh-CN", theme: "light", viewport: "desktop", guest: true, attempted: true, ok: true });
      count.inc();
    }
    await d.finish();
    return ok(count.n, views);
  });
}

// ---------- 4-view PNG evidence for every changed key page ----------
async function shotsCase(c) {
  return withDriver(c.id, { lang: "zh-CN", theme: "light" }, async (d, count) => {
    const f = await fixtures(d);
    const dir = path.join(process.env.E2E_RESULTS_DIR || path.join(process.env.TMPDIR || "/tmp", "f3ui-shots"), "shots");
    await mkdir(dir, { recursive: true });
    await d.context.addInitScript(() => { try { const t = sessionStorage.getItem("f3-theme"); if (t) localStorage.setItem("anotify-theme", t); } catch { /* storage off */ } });
    const views = [];
    for (const theme of THEMES) for (const viewport of Object.keys(VIEWPORTS)) {
      const errorsFrom = d.report.errors.length;
      await d.page.setViewportSize(VIEWPORTS[viewport]);
      const setTheme = t => { try { sessionStorage.setItem("f3-theme", t); } catch { /* storage off */ } };
      await d.page.evaluate(setTheme, theme);
      await d.page.evaluate(t => localStorage.setItem("anotify-theme", t), theme);
      const page = c.page;
      await gotoPage(d, page, f, "zh-CN", { guest: page === "login" });
      const applied = await d.page.evaluate(() => document.documentElement.getAttribute("data-theme"));
      assert.equal(applied, theme, "screenshot view applied the requested theme " + theme);
      const box = await d.page.evaluate(sel => { const el = document.querySelector(sel); return el ? Math.round(el.getBoundingClientRect().height) : -1; }, ANCHORS[page]);
      assert.ok(box > 0, "screenshot taken only with live content (" + page + " anchor " + ANCHORS[page] + " height " + box + ")");
      assert.deepEqual(d.report.errors.slice(errorsFrom), [], "screenshot view has no JS/console/network failure");
      const shot = await shoot(d, `${page}-${theme}-${viewport}`, dir);
      views.push({ page, lang: "zh-CN", theme, viewport, attempted: true, ok: true, png: shot });
      count.inc();
    }
    await d.finish();
    return ok(count.n, views);
  });
}

// ---------- UX19–23 retained behaviours ----------
async function ux19(c) {
  return withDriver(c.id, { lang: "zh-CN", theme: "light" }, async (d, count) => {
    await d.goto("index");
    const me = await d.req("/v1/auth/me");
    if (d.mock) { await greetMember(d); assert.equal((await d.req("/v1/auth/me")).role, "member", "explicit preview member fixture is a real member identity"); }
    else assert.equal(me.role, "member", "the real seeded operator is a member, never an admin hardcode");
    await d.archive();
    await d.page.goto(d.base + "/admin.html?namespace=default", { waitUntil: "domcontentloaded" });
    await d.page.waitForFunction(() => { const m = document.getElementById("page-main"); return !!m && /无权|Forbidden|not an admin|无权限/.test(m.innerText || ""); }, null, { timeout: 10000 }).catch(() => {});
    const state = await d.page.evaluate(() => {
      const main = document.getElementById("page-main");
      return { text: (main?.innerText || "").replace(/\s+/g, " ").trim().slice(0, 200), back: !!main?.querySelector('a[href="index.html"]'), kpi: !!document.getElementById("kpi-devices"), role: window.Anotify?.me?.role || null };
    });
    assert.match(state.text, /无权|Forbidden|not an admin|无权限/, "#19 member sees the explicit forbidden page: " + state.text);
    assert.equal(state.back, true, "#19 forbidden page links back to the overview");
    assert.equal(state.kpi, false, "#19 no broken KPI shell behind the denial");
    assert.ok(state.role === null || state.role === "member", "#19 page identity stays a member: " + state.role);
    const actualCalls = await d.calls(); // snapshot BEFORE archive clears live calls
    assert.deepEqual(actualCalls.filter(x => x.method !== "GET"), [], "#19 member attempt performs zero privileged write");
    assert.deepEqual(actualCalls.filter(x => /\/v1\/admin/.test(x.url)), [], "#19 no privileged admin API is called for a member");
    await d.archive();
    await d.finish(); count.inc();
    return ok(count.n, [{ page: "admin", lang: "zh-CN", theme: "light", viewport: "desktop", attempted: true, ok: true, member: true }]);
  });
}
async function ux20(c) {
  return withDriver(c.id, { lang: "zh-CN", theme: "light" }, async (d, count) => {
    await d.context.addInitScript(() => {
      Object.defineProperty(window, "Notification", { configurable: true, writable: true, value: class Notification { static get permission() { return "denied"; } static requestPermission() { return Promise.resolve("denied"); } } });
      window.__swRegister = 0;
      if (navigator.serviceWorker) { const base = navigator.serviceWorker.register ? navigator.serviceWorker.register.bind(navigator.serviceWorker) : null; if (base) navigator.serviceWorker.register = (...args) => { window.__swRegister++; return base(...args); }; }
    });
    await d.archive();
    await d.page.goto(d.base + "/receivers.html?namespace=default", { waitUntil: "domcontentloaded" });
    await d.page.waitForFunction(() => !!window.Anotify && !document.querySelector('[aria-busy="true"]'), null, { timeout: 9000 }).catch(() => {});
    await d.page.waitForTimeout(800);
    const card = await d.page.evaluate(() => {
      const c = document.getElementById("current-device-card");
      const main = document.getElementById("page-main");
      return {
        pushUI: !!(window.Anotify && window.Anotify.pushUIEnabled),
        card: !!c,
        text: ((c || main)?.innerText || "").replace(/\s+/g, " ").trim().slice(0, 240),
        buttons: Array.from((c || document.body).querySelectorAll("button")).map(b => (b.textContent || "").trim()).slice(0, 6),
        register: window.__swRegister || 0,
        permission: Notification.permission,
      };
    });
    if (!card.pushUI) {
      assert.match(card.text, /暂未开放|not available|未公開|no disponibles/i, "#20 push UI honestly reported unavailable: " + card.text);
      assert.equal(card.card, false, "#20 no half-built device card while push is unavailable");
    } else {
      assert.ok(/查看如何开启|how to enable|cómo/i.test(card.buttons.join(" ") + " " + card.text), "#20 denied permission surfaces enable-guidance: " + JSON.stringify({ buttons: card.buttons, text: card.text }));
      const button = d.page.locator("#current-device-card button").first();
      if (await button.count()) {
        await button.click();
        await d.page.waitForSelector("#anotify-toast-host", { timeout: 5000 }).catch(() => {});
        const toast = await d.page.evaluate(() => (document.getElementById("anotify-toast-host")?.textContent || "").trim());
        assert.ok(toast.length > 0, "#20 the real click produces visible feedback: " + toast.slice(0, 80));
      }
    }
    assert.equal(card.permission, "denied", "#20 the page really observed the denied permission");
    assert.equal(card.register, 0, "#20 denied permission never registers a push subscription");
    assert.deepEqual((await writeWrites(d)).filter(x => /\/v1\/devices/.test(x.url)), [], "#20 denied permission performs zero device/notification write");
    await d.finish(); count.inc();
    return ok(count.n, [{ page: "receivers", lang: "zh-CN", theme: "light", viewport: "desktop", attempted: true, ok: true, pushUI: card.pushUI }]);
  });
}
async function ux21(c) {
  return withDriver(c.id, { lang: "zh-CN", theme: "light" }, async (d, count) => {
    await d.archive();
    await d.page.goto(d.base + "/index.html?namespace=default&u=zheng", { waitUntil: "domcontentloaded" });
    // 就绪条件必须是**真实事实**（#338 flake，全量第 1 轮复现 ["usr_mock_demo|demo","|"]）：
    //   ① `#greeting` / `#sidebar-username` 的模板占位文本本身就非空（sidebar 默认 "…"），
    //      旧的就绪条件等于什么都没等；
    //   ② window.__f3Calls 里**在飞的请求**也占一行（只有 url/method，没有 result/httpStatus），
    //      index 页有两处 /auth/me（layout 的 loadMe + 页面控制器的 loadMe），并发下读到
    //      「A 已落定、B 还在飞」的中间态，身份集合里就多出一个空条目。
    // 所以先等**所有 /auth/me 都落定**（在飞的请求不是 response，不参与身份比对），
    // 再等同一个身份真的渲染进 DOM（不是模板占位），最后照旧断言。
    const pendingOf = calls => calls.filter(x => x.httpStatus === undefined && x.error === undefined);
    const deadline = Date.now() + 9000;
    let meCalls = [];
    for (;;) {
      meCalls = (await d.calls()).filter(x => x.method === "GET" && /\/v1\/auth\/me$/.test(x.url));
      if (meCalls.length >= 1 && pendingOf(meCalls).length === 0) break;
      if (Date.now() > deadline) break;
      await new Promise(r => setTimeout(r, 50));
    }
    assert.deepEqual(pendingOf(meCalls).map(x => x.url), [], "#21 every recorded /auth/me call settled (in-flight is not a response), no blank identity: " + JSON.stringify(meCalls.map(x => ({ hs: x.httpStatus, err: x.error, keys: Object.keys(x).join(",") }))).slice(0, 300));
    const me = meCalls[0]?.result || {};
    assert.ok(me.displayName, "#21 the identity comes from the recorded /auth/me response: " + JSON.stringify(me).slice(0, 160));
    let names = { greeting: "", sidebar: "" };
    for (;;) {
      names = await d.page.evaluate(() => ({ greeting: (document.getElementById("greeting")?.textContent || "").trim(), sidebar: (document.getElementById("sidebar-username")?.textContent || "").trim() }));
      if (names.greeting.includes(me.displayName) && names.sidebar.includes(me.displayName)) break;
      if (Date.now() > deadline) break;
      await new Promise(r => setTimeout(r, 50));
    }
    assert.ok(names.greeting.includes(me.displayName), "#21 greeting matches the /auth/me identity: " + JSON.stringify({ ...names, expect: me.displayName }));
    assert.ok(names.sidebar.includes(me.displayName), "#21 sidebar identity matches the same /auth/me response");
    assert.ok(!/zheng/.test(names.greeting) || me.displayName.includes("zheng"), "#21 the query-string fake user cannot replace the identity: " + names.greeting);
    assert.ok(meCalls.length >= 1, "#21 the identity is backed by GET /auth/me: " + meCalls.length + " call(s)");
    const identities = new Set(meCalls.map(x => (x.result?.id || "") + "|" + (x.result?.username || "")));
    assert.equal(identities.size, 1, "#21 every /auth/me response is the same identity, no divergent/fake user: " + JSON.stringify([...identities]) + " " + JSON.stringify(meCalls.map(x => ({ hs: x.httpStatus, err: x.error, id: x.result?.id, user: x.result?.username, code: x.result?.code }))).slice(0, 300));
    await d.finish(); count.inc();
    return ok(count.n, [{ page: "index", lang: "zh-CN", theme: "light", viewport: "desktop", attempted: true, ok: true, authMe: meCalls.length }]);
  });
}
async function ux22(c) {
  return withDriver(c.id, { lang: "zh-CN", theme: "light" }, async (d, count) => {
    await d.page.goto(d.base + "/security.html?namespace=default", { waitUntil: "domcontentloaded" });
    await d.page.waitForFunction(() => { const l = document.getElementById("passkey-list"); return !!l && (l.innerText || "").trim().length > 0; }, null, { timeout: 9000 }).catch(() => {});
    const copy = await d.page.evaluate(() => ({
      empty: (document.getElementById("passkey-list")?.innerText || "").replace(/\s+/g, " ").trim().slice(0, 240),
      buttons: Array.from(document.querySelectorAll("button")).map(b => (b.textContent || "").trim()).slice(0, 10),
      count: document.querySelectorAll("#passkey-list [data-passkey], #passkey-list li").length,
    }));
    assert.ok(copy.empty.length > 0, "#22 the empty passkey list renders a meaningful enrollment anchor: " + copy.empty);
    assert.match(copy.empty, /本机添加|Add on this device|この端末|este dispositivo/i, "#22 the empty state points at this device: " + copy.empty);
    assert.ok(!/在新设备添加|点右上角「\+ 添加 Passkey」/.test(copy.empty), "#22 obsolete add-on-new-device copy is gone: " + copy.empty);
    /* #376：添加 Passkey 收敛成「页头 1 主 + 1 次」，空态不再自带第三个按钮。
       唯一性断言从「#passkey-list 里恰好 1 个」升级成「页头 + 空态这两个入口里恰好 1 个」
       —— 隐藏的恢复码 nudge（文案也含「本机添加」）不在范围内。 */
    const add = d.page.locator('.page-head button, #passkey-list button').filter({ hasText: /本机添加|Add on this device|この端末|este dispositivo/i });
    assert.equal(await add.count(), 1, "#22 the canonical add-on-this-device control is unique on the whole page: " + JSON.stringify(copy.buttons));
    const before = d.page.url();
    const beforeFeedback = await d.page.evaluate(() => ({ toast: (document.getElementById("toast")?.textContent || "").trim(), list: (document.getElementById("passkey-list")?.innerText || "").slice(0, 160) }));
    const beforeCalls = (await d.calls()).length;
    const dialogs = [];
    const answer = async dialog => { dialogs.push({ type: dialog.type(), message: dialog.message() }); await dialog.dismiss(); };
    d.page.on("dialog", answer);
    try { await add.click(); await d.page.waitForTimeout(900); }
    finally { d.page.off("dialog", answer); }
    assert.equal(d.page.url(), before, "#22 the add click stays on the live security page");
    const feedback = await d.page.evaluate(() => ({ toast: (document.getElementById("toast")?.textContent || "").trim(), list: (document.getElementById("passkey-list")?.innerText || "").slice(0, 160) }));
    const actionCalls = (await d.calls()).slice(beforeCalls).filter(c => c.method === "POST" && /\/v1\/auth\/passkeys\//.test(c.url));
    assert.ok(actionCalls.length > 0 || dialogs.some(x => ["prompt", "alert"].includes(x.type) && x.message.trim().length > 0) || (feedback.toast.length > 0 && feedback.toast !== beforeFeedback.toast) || feedback.list !== beforeFeedback.list, "#22 the add click is answered, never silent: " + JSON.stringify({ ...feedback, dialogs }));
    await d.finish(); count.inc();
    return ok(count.n, [{ page: "security", lang: "zh-CN", theme: "light", viewport: "desktop", attempted: true, ok: true, passkeys: copy.count, response: { dialogs, actionCount: actionCalls.length, changedFeedback: feedback.toast !== beforeFeedback.toast || feedback.list !== beforeFeedback.list } }]);
  });
}
async function ux23(c) {
  return withDriver(c.id, { lang: "zh-CN", theme: "light" }, async (d, count) => {
    await d.goto("index");
    const devices = await d.req("/v1/devices");
    assert.equal(devices.devices.length, 0, "#23 fixture has zero devices — the honest denominator");
    const f = await fixtures(d);
    await gotoPage(d, "index", f, "zh-CN");
    await d.page.waitForTimeout(600);
    const state = await d.page.evaluate(() => ({
      pushUI: !!(window.Anotify && window.Anotify.pushUIEnabled),
      tiles: Array.from(document.querySelectorAll("[data-push-tile]")).map(el => ({ hidden: el.classList.contains("hidden"), text: (el.textContent || "").replace(/\s+/g, " ").trim().slice(0, 40) })),
      rate: (document.getElementById("kpi-rate")?.textContent || "").trim(),
      sub: (document.getElementById("kpi-rate-sub")?.textContent || "").trim(),
      deviceKpi: (document.getElementById("kpi-devices")?.textContent || "").trim(),
      testNotify: !!document.getElementById("test-notify") || !!document.getElementById("tn-form") || !!document.getElementById("tn-toggle"),
      percent: /(^|\D)100\s*%/.test(document.body.innerText || ""),
    }));
    if (!state.pushUI) assert.ok(state.tiles.length === 0 || state.tiles.every(t => t.hidden), "#23 push tiles honestly hidden while push UI is off: " + JSON.stringify(state.tiles));
    else {
      assert.ok(!/^100/.test(state.rate), "#23 zero devices never shows a fake 100% delivery rate: " + state.rate);
      assert.ok(/–|-|—/.test(state.rate) || /暂无|不可用|no device|N\/A|unavailable/i.test(state.sub), "#23 the rate shows an honest empty state: " + JSON.stringify({ rate: state.rate, sub: state.sub }));
    }
    assert.equal(state.testNotify, false, "#23 the obsolete test-notification panel stays deleted");
    assert.equal(state.percent, false, "#23 no fabricated 100% claim anywhere on the page");
    await d.finish(); count.inc();
    return ok(count.n, [{ page: "index", lang: "zh-CN", theme: "light", viewport: "desktop", attempted: true, ok: true, devices: 0 }]);
  });
}
async function retainedDevicePatch(c) {
  return withDriver(c.id, { lang: "zh-CN", theme: "light" }, async (d, count) => {
    const id = "f3b2b-toggle-device";
    await d.goto("index");
    if (d.mock) {
      await d.goto("receivers");
      const me = await d.req("/v1/auth/me");
      assert.ok(me.id, "actual current me.id owns the flat device fixture");
      await d.page.evaluate(([deviceId, userId]) => {
        const device = { id: deviceId, userId, name: "Toggle Receiver", platform: "web", enabled: true, eventScope: "all", tags: [], endpoint: "https://push.example.invalid/f3b2b" };
        window.MockApi.state.devices = [device];
        const facts = JSON.parse(sessionStorage.getItem("f3-facts") || "{}");
        sessionStorage.setItem("f3-facts", JSON.stringify({ ...facts, devices: [device] }));
      }, [id, me.id]);
    } else {
      const created = await d.response("/v1/devices", H.makeDevice({ id, name: "Toggle Receiver", enabled: true }), "POST");
      assert.ok([200, 201].includes(created.status), "real device registration answered: " + created.status + " " + JSON.stringify(created.body).slice(0, 160));
    }
    await d.archive();
    await d.page.goto(d.base + "/receivers.html?namespace=default", { waitUntil: "domcontentloaded" });
    await d.page.waitForFunction(() => !!window.Anotify && !document.querySelector('[aria-busy="true"]'), null, { timeout: 9000 }).catch(() => {});
    await d.page.waitForFunction(() => (document.body.innerText || "").includes("Toggle Receiver"), null, { timeout: 9000 }).catch(() => {});
    const row = await d.page.evaluate(() => {
      const rows = Array.from(document.querySelectorAll(".rc-dev-row"));
      const scope = rows.find(el => (el.textContent || "").includes("Toggle Receiver")) || null;
      return { found: !!scope, rows: rows.length, toggles: scope ? scope.querySelectorAll("button.switch, button[aria-pressed]").length : 0, pressed: scope ? Array.from(scope.querySelectorAll("button.switch, button[aria-pressed]")).map(b => b.getAttribute("aria-pressed")) : [] };
    });
    assert.equal(row.found, true, "the device row renders in the real list: " + JSON.stringify(row));
    assert.ok(row.toggles > 0, "the device row exposes a real enable switch: " + JSON.stringify(row));
    await d.archive();
    const toggle = d.page.locator(".rc-dev-row").filter({ hasText: "Toggle Receiver" }).locator("button.switch, button[aria-pressed]");
    assert.equal(await toggle.count(), 1, "one enable switch for the exact owned fixture row");
    await toggle.click();
    await d.page.waitForTimeout(1400);
    const writes = await writeWrites(d);
    assert.equal(writes.length, 1, "the device toggle issues exactly one write: " + JSON.stringify(writes.map(x => x.method + " " + x.url)).slice(0, 200));
    assert.equal(writes[0].method, "PATCH", "the device toggle uses PATCH: " + writes[0].method);
    assert.ok(writes[0].url.endsWith("/v1/devices/" + id), "PATCH targets the exact device id in the path, not a body id: " + writes[0].url);
    assert.deepEqual(Object.keys(writes[0].body || {}), ["enabled"], "the PATCH body carries only the enabled fact: " + JSON.stringify(writes[0].body));
    assert.equal(writes[0].body.enabled, false, "the toggle sends enabled=false");
    assert.equal(writes[0].httpStatus, 200, "the success response is consumed from the wire: " + writes[0].httpStatus);
    assert.equal(writes[0].response.device.id, id, "wrapped device response has exact retained id");
    assert.equal(writes[0].response.device.enabled, false, "wrapped response persists false");
    const paused = await d.page.evaluate(() => (document.getElementById("page-main")?.innerText || "").replace(/\s+/g, " "));
    assert.match(paused, /暂停|已停用|Paused|pausado|Desactiv/i, "the paused state is reflected in the live UI: " + paused.slice(0, 180));
    const state = await d.req("/v1/devices");
    assert.equal(state.devices.find(x => x.id === id)?.enabled, false, "a follow-up GET /v1/devices confirms the paused device");
    await d.finish(); count.inc();
    return ok(count.n, [{ page: "receivers", lang: "zh-CN", theme: "light", viewport: "desktop", attempted: true, ok: true, patch: writes[0].url }]);
  });
}
async function retainedLoginForm(c) {
  return withDriver(c.id, { lang: "zh-CN", theme: "light" }, async (d, count) => {
    await d.goto("index");
    await greetGuest(d);
    await d.page.goto(d.base + "/login.html", { waitUntil: "domcontentloaded" });
    await d.page.waitForFunction(() => !!document.getElementById("login-panel"), null, { timeout: 9000 }).catch(() => {});
    const form = await d.page.evaluate(() => {
      const panel = document.getElementById("login-panel");
      const inputs = Array.from(panel?.querySelectorAll("input") || []);
      return { panel: !!panel, password: inputs.filter(i => i.type === "password").length, other: inputs.filter(i => i.type !== "password").length, submit: !!panel?.querySelector('button[type=submit], button'), text: (panel?.innerText || "").replace(/\s+/g, " ").trim().slice(0, 120) };
    });
    assert.ok(form.panel && form.password >= 1 && form.other >= 1 && form.submit, "the retained password login form is intact: " + JSON.stringify(form));
    await d.finish(); count.inc();
    return ok(count.n, [{ page: "login", lang: "zh-CN", theme: "light", viewport: "desktop", guest: true, attempted: true, ok: true }]);
  });
}

// ---------- shift sampling probe (self-contained: serialised into the page) ----------
function shiftProbe() {
  const round = v => Math.round(v * 100) / 100;
  const box = sel => { const el = document.querySelector(sel); if (!el) return null; const r = el.getBoundingClientRect(); return { x: round(r.x), y: round(r.y), h: round(r.height) }; };
  const s = { frames: 0, stop: false, t: [], nav: [], shell: [], slotH: [], pageMainX: [], pageX: [], switcherVisible: [], scopeMutations: [] };
  window.__shift = s;
  const tick = () => {
    s.frames++; s.t.push(Math.round(performance.now()));
    const nav = box(".app-nav"), shell = box("#app-shell"), slot = box("#scope-slot"), main = box("#page-main"), page = box(".page");
    s.nav.push(nav ? nav.y : null); s.shell.push(shell ? shell.y : null); s.slotH.push(slot ? slot.h : null);
    s.pageMainX.push(main ? main.x : null); s.pageX.push(page ? page.x : null);
    const sw = document.getElementById("scope-switcher");
    s.switcherVisible.push(sw ? !sw.classList.contains("hidden") : null);
    if (!s.stop) requestAnimationFrame(tick);
  };
  requestAnimationFrame(tick);
  let tries = 0;
  const watch = () => {
    const sw = document.getElementById("scope-switcher");
    if (!sw) { if (++tries < 1500) requestAnimationFrame(watch); return; }
    const snap = stage => { const nav = box(".app-nav"); return { stage, hidden: sw.classList.contains("hidden"), navY: nav ? nav.y : null }; };
    s.scopeMutations.push(snap("before"));
    new MutationObserver(() => s.scopeMutations.push(snap("mutation"))).observe(sw, { attributes: true, attributeFilter: ["class"] });
  };
  requestAnimationFrame(watch);
}
const seq = (arr, max = 400) => arr.map(String).join("→").slice(0, max);
const uniq = arr => { const out = []; for (const v of arr) if (!out.includes(v)) out.push(v); return out; };
const nonNull = arr => arr.filter(v => v !== null && v !== undefined);
async function readProbe(page) { return page.evaluate(() => { window.__shift.stop = true; return window.__shift; }); }

export async function runUiCase(suite, id) {
  const c = casesFor(suite).find(x => x.id === id);
  assert.ok(c, "unknown registered case " + id);
  if (c.kind === "grid") return gridCase(c);
  if (c.kind === "mobile") return mobileFitsCase(c);
  if (c.kind === "shots") return shotsCase(c);
  if (c.kind === "guard") return id === "retained-login-form" ? retainedLoginForm(c) : guardsGuest(c);
  const single = {
    "shift-lang-hint": () => shiftLangHint(c), "shift-scope-slot": () => shiftScopeSlot(c), "shift-collapsed": () => shiftCollapsed(c),
    "shift-prelayout-expanded": () => shiftPrelayout(c, false), "shift-prelayout-collapsed": () => shiftPrelayout(c, true),
    "interact-namespace-switch": () => interactNamespaceSwitch(c), "interact-dialogs": () => interactDialogs(c), "interact-error-states": () => interactErrorStates(c),
    "interact-error-403": () => interactError403(c),
    "retained-auth-shell": () => retainedAuthShell(c), "retained-graph": () => retainedGraph(c),
    "ux19-member-admin": () => ux19(c), "ux20-notification-denied": () => ux20(c), "ux21-authme-identity": () => ux21(c),
    "ux22-passkey-empty-enrollment": () => ux22(c), "ux23-zero-devices-honest": () => ux23(c),
    "retained-device-patch": () => retainedDevicePatch(c),
  }[id];
  assert.ok(single, "case " + id + " has a registered body");
  return single();
}
export { PAGES, LANGS, THEMES, VIEWPORTS, STANDALONE, DATA_PAGES };
