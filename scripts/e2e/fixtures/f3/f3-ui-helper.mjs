// F3b2b shared UI helpers: real Chrome page views, matrix geometry, genuine screenshots.
// Reuses the frozen F3 driver (openDriver/preview authority); never mutates it.
import assert from "node:assert/strict";
import { writeFile } from "node:fs/promises";
import path from "node:path";
import { openDriver } from "./f3-driver.mjs";

export const LANGS = Object.freeze(["zh-CN", "en", "ja", "es"]);
export const PAGES = Object.freeze(["admin","capabilities","cli-auth","docs","events","index","keys","login","message","node","nodes","passkey-enroll","playbook","playbooks","receivers","security","source","sources","trace"]);
export const THEMES = Object.freeze(["light", "dark"]);
export const VIEWPORTS = Object.freeze({ desktop: Object.freeze({ width: 1440, height: 900 }), mobile: Object.freeze({ width: 390, height: 844 }) });
export const STANDALONE = Object.freeze(["cli-auth", "passkey-enroll"]);
export const PREVIEW_BANNER = "前端预览 · 数据均为 mock（接口未真正调用）";
// Batches keep every matrix case inside the fixed 55s outer budget.
export const MATRIX_BATCHES = Object.freeze([[0,5],[5,10],[10,15],[15,19]].map(([a,b]) => PAGES.slice(a,b)));
export const KEY_PAGES = Object.freeze(["index","capabilities","playbooks","security","receivers","events","trace","playbook"]);
// Per-page structural anchor. Detail pages get their identity from real v4 list rows.
// #381：登录页的默认 tab 跟实例/浏览器状态走（首跑默认落在「注册」），所以 #login-panel
// 可能真的是 hidden（0×0）。锚点改成**两个 tab 都一定看得见**的容器：仍然证「这一页真的
// 渲染了」，而不是在默认态上赌一把。
export const ANCHORS = Object.freeze({
  admin: "#kpi-devices", capabilities: "[data-cap]", "cli-auth": "#code-input", docs: "#endpoints",
  events: "#ev-list article", index: "#kpi-total", keys: "#keys-body tr", login: ".auth-tabs",
  message: "#task-facts", node: "#node-slots", nodes: "[data-node]", "passkey-enroll": "#code-input",
  playbook: "#pb-file-path", playbooks: "#pb-grid", receivers: "#current-device-card",
  security: "#passkey-list", source: "#src-name", sources: "#src-list [data-name]", trace: "#trace-tasks",
});
// Pages whose controller must show real data facts, not just an empty shell.
export const DATA_PAGES = Object.freeze(["admin","capabilities","events","index","keys","nodes","sources","security","receivers"]);
const FAILURE_TEXT = /载入失败|加载失败|Failed to load|取不到数据|响应不是 \*List/;

export const MATRIX_VIEWS = PAGES.length * LANGS.length * THEMES.length * 2;
export function matrixCases() {
  const cases = [];
  for (const lang of LANGS) for (const theme of THEMES) for (const viewport of Object.keys(VIEWPORTS))
    MATRIX_BATCHES.forEach((pages, index) => cases.push({ id: `grid-${lang}-${theme}-${viewport}-${index + 1}`, pages, lang, theme, viewport }));
  assert.equal(cases.length, MATRIX_BATCHES.length * LANGS.length * THEMES.length * 2);
  assert.equal(cases.reduce((n, c) => n + c.pages.length, 0), MATRIX_VIEWS, "304 registered page views");
  return cases;
}
export const localeFor = lang => ({ "zh-CN": "zh-CN", en: "en-US", ja: "ja-JP", es: "es-ES" })[lang];

// Real v4 identities for detail pages; never uid/name aliases or the H.req adapter.
export async function fixtures(d) {
  await d.goto("index");
  const [tasks, sources, nodes, caps, devices] = await Promise.all([
    d.req("/v1/namespaces/default/tasks?limit=100"), d.req("/v1/namespaces/default/sources"),
    d.req("/v1/nodes"), d.req("/v1/namespaces/default/capabilities"), d.req("/v1/devices"),
  ]);
  assert.equal(tasks.kind, "TaskList", "canonical TaskList required");
  assert.equal(sources.kind, "SourceList", "canonical SourceList required");
  assert.equal(nodes.kind, "NodeList", "canonical NodeList required");
  assert.equal(caps.kind, "CapabilityList", "canonical CapabilityList required");
  assert.ok(Array.isArray(devices.devices), "device list wrapper required");
  const task = tasks.items[0], source = sources.items[0], node = nodes.items[0];
  assert.ok(task?.metadata?.name && task?.spec?.traceUID, "canonical Task.name/spec.traceUID required");
  assert.ok(source?.metadata?.name, "canonical Source.name required");
  assert.ok(node?.metadata?.name, "canonical Node.metadata.name required");
  const playbooks = await d.req("/v1/namespaces/default/playbooks");
  assert.equal(playbooks.kind, "PlaybookList", "canonical PlaybookList required");
  let playbook = playbooks.items[0];
  if (!playbook) {
    const name = "f3-ui-detail";
    const files = {
      "playbook.yaml": JSON.stringify({ apiVersion: "anotify/v1", kind: "Playbook", metadata: { name, version: 1 }, spec: { values: { type: "object", properties: {} }, capabilities: ["capabilities/view.yaml"], sources: [] } }),
      "capabilities/view.yaml": JSON.stringify({ metadata: { name: "f3-ui-view" }, spec: { run: { entrypoint: "run.sh", files: { "run.sh": "#!/bin/sh\necho UI fixture\n" } } } }),
    };
    const created = await d.response("/v1/namespaces/default/playbooks", { apiVersion: "anotify/v1", kind: "Playbook", metadata: { name, namespace: "default" }, spec: { files } }, "POST");
    assert.equal(created.status, 201, "real namespace Playbook fixture created");
    playbook = created.body;
  }
  assert.ok(playbook.metadata?.name, "canonical Playbook.metadata.name required");
  const detail = await d.req("/v1/namespaces/default/playbooks/" + encodeURIComponent(playbook.metadata.name) + "?includeBody=true");
  assert.equal(detail.metadata.name, playbook.metadata.name);
  assert.ok(detail.spec.files["playbook.yaml"], "real Playbook GET supplies editor content");
  return { task: task.metadata.name, traceUID: task.spec.traceUID, source: source.metadata.name, node: node.metadata.name, playbook: playbook.metadata.name, devices: devices.devices.length };
}
export function query(page, f = {}, guest) {
  const p = new URLSearchParams();
  if (page !== "login") p.set("namespace", "default");
  if (!guest) {
    if (page === "message" && f.task) p.set("id", f.task);
    if (page === "trace" && f.traceUID) p.set("traceUID", f.traceUID);
    if (page === "source" && f.source) p.set("name", f.source);
    if (page === "node" && f.node) p.set("id", f.node);
    if (page === "playbook" && f.playbook) p.set("id", f.playbook);
  }
  return p.toString();
}
export function viewUrl(d, page, f = {}, lang = "zh-CN", guest = false) {
  const dir = lang === "zh-CN" ? "/" : `/${lang}/`;
  const q = query(page, f, guest);
  return d.base + dir + page + ".html" + (q ? "?" + q : "");
}

// Anonymous fixture: an explicit 401 /auth/me on every view of this session.
// sessionStorage needs a real origin: callers load a page first.
const requireOrigin = d => assert.ok(!d.page.url().startsWith("about:"), "auth fixture needs a loaded origin, not about:blank");
export async function greetGuest(d) {
  requireOrigin(d);
  if (d.mock) await d.page.evaluate(() => {
    if (!sessionStorage.getItem("f3-auth-before-guest")) sessionStorage.setItem("f3-auth-before-guest", JSON.stringify({ auth: MockApi.state.auth, cookie: document.cookie.split(";").map(c => c.trim()).find(c => c.startsWith("anotify_session=")), loggedOut: sessionStorage.getItem("anotify.mock.loggedOut") }));
    sessionStorage.setItem("f3-guest", "1");
  });
}
export async function greetMember(d) { requireOrigin(d); if (d.mock) await d.page.evaluate(() => sessionStorage.setItem("f3-member", "1")); }
const authFlag = async (d, guest, member) => {
  if (d.mock && !guest) await d.page.evaluate(member => {
    const saved = JSON.parse(sessionStorage.getItem("f3-auth-before-guest") || "null");
    if (saved) {
      MockApi.state.auth = saved.auth;
      const facts = JSON.parse(sessionStorage.getItem("f3-facts") || "{}"); facts.auth = saved.auth;
      sessionStorage.setItem("f3-facts", JSON.stringify(facts));
      if (saved.loggedOut === null) sessionStorage.removeItem("anotify.mock.loggedOut"); else sessionStorage.setItem("anotify.mock.loggedOut", saved.loggedOut);
      if (saved.cookie) document.cookie = saved.cookie + "; Path=/; SameSite=Lax";
      sessionStorage.removeItem("f3-auth-before-guest");
    }
    sessionStorage.removeItem("f3-guest"); window.__f3Guest = false;
    if (!member) { sessionStorage.removeItem("f3-member"); window.__f3Member = false; }
  }, member);
  if (guest) await greetGuest(d); if (member) await greetMember(d);
};

// One real navigation of one page view; errors are attributed per view.
export async function gotoPage(d, page, f, lang, { guest = false, member = false } = {}) {
  await authFlag(d, guest, member);
  await d.archive();
  await d.page.goto(viewUrl(d, page, f, lang, guest), { waitUntil: "domcontentloaded" });
  if (!STANDALONE.includes(page))
    await d.page.waitForFunction(() => !!window.Anotify && !document.querySelector('[aria-busy="true"]'), null, { timeout: 7000 }).catch(() => { /* audit reports the consequence */ });
  else await d.page.waitForFunction(() => document.readyState === "complete", null, { timeout: 8000 }).catch(() => {});
  if (!(guest && page !== "login"))
    await d.page.waitForFunction(sel => !!document.querySelector(sel), ANCHORS[page], { timeout: 5000 }).catch(() => { /* audit asserts presence */ });
  await d.page.waitForTimeout(120);
  if (!(guest && page !== "login")) assert.equal(new URL(d.page.url()).pathname, new URL(viewUrl(d, page, f, lang, guest)).pathname, "audit must visit its own controller, never accept a protected-page login redirect");
}

/* Geometry/scroll/error/nonempty audit of one real page view. Every number is
 * measured; contained horizontal scrollers are accepted only when they can
 * actually scroll, wide pages are never blanket-excused. */
export async function auditPage(d, spec, errorsFrom) {
  const { page, lang, theme, viewport, guest } = spec;
  const geo = await d.page.evaluate(anchorSel => {
    const doc = document.documentElement;
    const main = document.getElementById("page-main");
    const bounded = [];
    for (const el of document.querySelectorAll("body *")) {
      const r = el.getBoundingClientRect(), s = getComputedStyle(el);
      if (s.display === "none" || !r.width || (r.left >= -2 && r.right <= innerWidth + 2)) continue;
      let ok = false;
      for (let p = el.parentElement; p && p !== document.body; p = p.parentElement) {
        const ps = getComputedStyle(p), pr = p.getBoundingClientRect();
        if (!["auto", "scroll", "hidden", "clip"].includes(ps.overflowX) || !p.clientWidth || pr.left < -2 || pr.right > innerWidth + 2) continue;
        if (["auto", "scroll"].includes(ps.overflowX) && p.scrollWidth > p.clientWidth) {
          const before = p.scrollLeft; p.scrollLeft = before ? 0 : Math.min(32, p.scrollWidth - p.clientWidth);
          const moved = p.scrollLeft !== before; p.scrollLeft = before; if (!moved) continue;
        }
        ok = true; bounded.push({ tag: el.tagName, id: el.id, clip: p.tagName, contained: true }); break;
      }
      if (!ok) bounded.push({ tag: el.tagName, id: el.id, left: Math.round(r.left), right: Math.round(r.right), unbounded: true });
    }
    /* #384：主内容横滚「白名单 + 可见提示」（只在手机档断言）。
       —— 旧口径是**全局豁免**：只要有 overflow 祖先就算合法，于是 keys 表 2.01×、
       链路图 3.6× 全都放行。现在反过来：主内容里的横滚容器必须①在白名单内
       （只放行用户主动打开的完整视图），②本身带可见提示（渐隐 mask / 拖动提示
       文案 / data-scroll-hint 或 aria-label 的语义副本）。默认态的主内容一律不该滚。 */
    const hscroll = [];
    if (innerWidth < 640) {
      const mainEl = document.getElementById("page-main") || document.body;
      const WHITELIST = [".flograph-scroll", ".hm-scroll"];
      for (const el of mainEl.querySelectorAll("*")) {
        const cs = getComputedStyle(el);
        if (cs.overflowX !== "auto" && cs.overflowX !== "scroll") continue;
        if (el.scrollWidth - el.clientWidth <= 2) continue;
        const before = el.scrollLeft;
        el.scrollLeft = before ? 0 : Math.min(24, el.scrollWidth - el.clientWidth);
        const moved = el.scrollLeft !== before;
        el.scrollLeft = before;
        if (!moved) continue;
        const mask = String(cs.maskImage || "") + " " + String(cs.webkitMaskImage || "");
        const hint = (el.getAttribute("data-scroll-hint") || el.getAttribute("aria-label") || "").trim();
        const pan = Array.from(el.parentElement ? el.parentElement.querySelectorAll(".flograph-pan-hint") : [])
          .some((n) => parseFloat(getComputedStyle(n).opacity) > 0.1);
        hscroll.push({
          sel: el.tagName.toLowerCase() + (el.id ? "#" + el.id : "") + "." + String(el.className || "").trim().split(/\s+/).slice(0, 2).join("."),
          whitelisted: WHITELIST.some((s) => el.matches(s)),
          hint: !!hint,
          mask: /gradient/.test(mask),
          pan,
        });
      }
    }
    const anchorEl = document.querySelector(anchorSel);
    const ar = anchorEl ? anchorEl.getBoundingClientRect() : null;
    return {
      hscroll,
      innerWidth, scrollWidth: doc.scrollWidth, innerHeight, scrollHeight: doc.scrollHeight,
      lang: doc.lang, theme: doc.getAttribute("data-theme"), shellReady: doc.hasAttribute("data-shell-ready"),
      overflow: bounded,
      bodyText: (document.body.innerText || "").replace(/\s+/g, " ").trim().slice(0, 220),
      mainText: main ? (main.innerText || "").replace(/\s+/g, " ").trim().slice(0, 220) : null,
      mainWidth: main ? Math.round(main.getBoundingClientRect().width) : null,
      anchor: ar ? { text: (anchorEl.innerText || "").replace(/\s+/g, " ").trim().slice(0, 80), width: Math.round(ar.width), height: Math.round(ar.height), children: anchorEl.children.length } : null,
      banner: (document.body.innerText || "").includes("前端预览 · 数据均为 mock（接口未真正调用）"),
    };
  }, spec.anchor);
  const label = `${page}·${lang}·${theme}·${viewport}`;
  const unbounded = geo.overflow.filter(o => o.unbounded);
  assert.ok(geo.scrollWidth <= geo.innerWidth + 2, label + ": document horizontal overflow " + geo.scrollWidth + ">" + geo.innerWidth + (unbounded.length ? " unbounded=" + JSON.stringify(unbounded.slice(0, 3)) : ""));
  if (spec.viewport === "mobile") {
    for (const h of geo.hscroll || []) {
      assert.ok(h.whitelisted, label + ": #384 main-content horizontal scroller is not whitelisted (" + h.sel + ") \u2014 scroll-to-read main content is forbidden");
      assert.ok(h.hint || h.mask || h.pan, label + ": #384 whitelisted scroller has no visible scroll hint (" + h.sel + "); need a fade mask, the pan hint, or aria/data hint text");
    }
  }
  assert.ok(geo.bodyText.length >= 40, label + ": nonempty rendered body required (" + geo.bodyText.length + " chars)");
  assert.ok(!FAILURE_TEXT.test(geo.mainText ?? geo.bodyText), label + ": genuine load-failure text not waived: " + (geo.mainText ?? geo.bodyText));
  assert.equal(geo.theme, theme, label + ": requested theme applied");
  assert.equal(geo.lang, lang, label + ": served document lang");
  assert.ok(geo.anchor, label + ": structural anchor " + spec.anchor + " present");
  assert.ok(geo.anchor.width > 0 && geo.anchor.height > 0, label + ": anchor has real geometry " + JSON.stringify(geo.anchor));
  if (DATA_PAGES.includes(page)) assert.ok(geo.anchor.children > 0 || geo.anchor.text.length > 0, label + ": data controller nonempty " + JSON.stringify(geo.anchor));
  if (geo.mainWidth !== null) assert.ok(geo.mainWidth >= Math.min(320, Math.round(geo.innerWidth * 0.45)), label + ": main content laid out (" + geo.mainWidth + "px of " + geo.innerWidth + ")");
  if (!guest && !STANDALONE.includes(page)) assert.equal(geo.shellReady, true, label + ": shell mounted");
  assert.equal(geo.banner, d.mock, label + (d.mock ? ": explicit preview authority banner present" : ": no fixture banner in real authority"));
  assert.deepEqual(d.report.errors.slice(errorsFrom), [], label + ": pageerror/console/network/unimplemented501 must fail");
  await d.page.evaluate(() => scrollTo(0, 0));
  await d.page.mouse.wheel(0, 700);
  const scrolled = geo.scrollHeight > geo.innerHeight + 5
    ? await d.page.waitForFunction(() => scrollY > 0, null, { timeout: 3000 }).then(() => true).catch(() => false)
    : true;
  assert.equal(scrolled, true, label + ": real vertical scroll unlocked (scrollHeight " + geo.scrollHeight + ")");
  await d.page.evaluate(() => scrollTo(0, 0));
  d.check(label + " fits the viewport, nonempty " + page + ", no JS/console/network failure, scroll unlocked");
  return { page, lang, theme, viewport, guest: !!guest, attempted: true, ok: true, geometry: { innerWidth: geo.innerWidth, scrollWidth: geo.scrollWidth, scrollHeight: geo.scrollHeight, mainWidth: geo.mainWidth, overflow: unbounded, anchor: geo.anchor }, scroll: { scrollHeight: geo.scrollHeight, innerHeight: geo.innerHeight } };
}

// PNG evidence is written only after the live DOM was asserted; the file itself
// is verified (signature + IHDR size + non-trivial bytes), never "exists" only.
export async function shoot(d, name, dir) {
  const buffer = await d.page.screenshot({ path: path.join(dir, name + ".png") });
  assert.equal(buffer.subarray(0, 8).toString("hex"), "89504e470d0a1a0a", name + ": real PNG signature");
  const width = buffer.readUInt32BE(16), height = buffer.readUInt32BE(20);
  assert.ok(buffer.length > 8000, name + ": non-trivial PNG bytes " + buffer.length);
  assert.ok(width >= 380 && height >= 800, name + ": PNG pixel size " + width + "x" + height);
  await writeFile(path.join(dir, name + ".json"), JSON.stringify({ name, bytes: buffer.length, width, height, url: d.page.url() }, null, 2) + "\n");
  d.check("screenshot " + name + " = " + width + "x" + height + " " + buffer.length + "B");
  return { name, bytes: buffer.length, width, height };
}
export { openDriver };

// E1b diagnostic ONLY. Observes real geometry without delaying mount, fonts,
// network or early frames. WeakMap IDs prove whether it is the SAME live NAV.
export function passiveUiProbe() {
  const ids=new WeakMap();let serial=0;
  const id=n=>n?(ids.has(n)?ids.get(n):(ids.set(n,++serial),serial)):null;
  const round=n=>Math.round(n*100)/100;
  const box=n=>{if(!n)return null;const r=n.getBoundingClientRect();return {domId:id(n),tag:n.tagName,id:n.id,class:n.className,x:round(r.x),y:round(r.y),w:round(r.width),h:round(r.height)};};
  const s={frames:[],events:[],http:[],stop:false};window.__e1bPassive=s;
  const snap=()=>{
    const nav=document.querySelector('.app-nav'),header=document.querySelector('.app-brand'),shell=document.querySelector('#app-shell');
    return {t:round(performance.now()),ready:document.documentElement.hasAttribute('data-shell-ready'),nav:box(nav),shell:box(shell),header:box(header),parent:box(nav?.parentElement),ancestors:nav?[nav.parentElement,nav.parentElement?.parentElement].map(box):[],siblings:nav?.parentElement?[...nav.parentElement.children].map(box):[],children:nav?[...nav.children].map(box):[],hint:box(document.querySelector('.lang-hint')),scope:box(document.querySelector('#scope-slot')),admin:box(document.querySelector('#admin-nav-section')),fonts:document.fonts.status,css:[...document.querySelectorAll('link[rel="stylesheet"]')].map(n=>({href:n.href,ready:!!n.sheet}))};
  };
  const event=(kind,extra={})=>s.events.push({kind,...snap(),...extra});
  const desc=Object.getOwnPropertyDescriptor(window,'Anotify');
  if(desc?.set)Object.defineProperty(window,'Anotify',{...desc,set(value){
    desc.set.call(this,value);
    const mount=value.mountLayout;
    value.mountLayout=function(...args){event('initial-sync-mount-start');try{return mount.apply(this,args);}finally{event('initial-sync-mount-end');}};
    const fetch=window.fetch;
    window.fetch=async function(url,opts){const u=String(url?.url||url);if(!u.includes('/v1/auth/me'))return fetch.call(this,url,opts);const start=round(performance.now());try{const response=await fetch.call(this,url,opts);s.http.push({url:u,start,end:round(performance.now()),status:response.status});event('authme-response',{status:response.status});return response;}catch(e){s.http.push({url:u,start,end:round(performance.now()),error:String(e)});throw e;}};
  }});
  new MutationObserver(records=>{
    const relevant=records.filter(r=>r.target.closest?.('#sidebar, .lang-hint')||r.target.matches?.('html, #app-shell')||[...r.addedNodes,...r.removedNodes].some(n=>n.matches?.('#app-shell, .lang-hint')));
    if(relevant.length)event('mutation',{mutations:relevant.map(r=>({type:r.type,target:box(r.target),attribute:r.attributeName,added:[...r.addedNodes].filter(n=>n.nodeType===1).map(box),removed:[...r.removedNodes].filter(n=>n.nodeType===1).map(box)}))});
  }).observe(document,{subtree:true,childList:true,attributes:true,attributeFilter:['class','style','hidden','data-shell-ready']});
  document.addEventListener('DOMContentLoaded',()=>event('DOMContentLoaded'));
  addEventListener('load',()=>event('load'));
  document.fonts.ready.then(()=>event('fonts-ready-remeasure'));
  document.fonts.addEventListener('loadingdone',()=>event('fonts-loadingdone-remeasure'));
  document.addEventListener('load',e=>{if(e.target.matches?.('link[rel="stylesheet"]'))event('CSS-ready-remeasure',{href:e.target.href});},true);
  try {new PerformanceObserver(list=>{for(const entry of list.getEntries())event('paint',{name:entry.name,paintTime:entry.startTime});}).observe({type:'paint',buffered:true});}catch{/* unsupported paint observer is recorded by absent event */}
  const tick=()=>{s.frames.push(snap());if(!s.stop)requestAnimationFrame(tick);};requestAnimationFrame(tick);
}
