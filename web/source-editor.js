/* 源编辑器（共享组件）
 *
 * 为什么是共享模块：源的写表单在「列表页行内编辑 / 新建」和「源详情页就地编辑」两处都要用。
 * 复制一份 DOM 或逻辑，下一次改字段（见 ./sources.html 的历史）就会两边不同步，所以只有这一份：
 *   · 弹层 DOM 由本模块注入到 <body>（一次性，幂等），id/class 与历史实现完全一致；
 *   · 取数、写信封、CAS/409、草稿试运行、变量建议、字段级错误都在这里；
 *   · 调用方只提供上下文：要编辑的资源（或 null = 新建）、命名空间、保存成功后的回调。
 *
 * 用法：await Anotify.sourceEditor.open({ resource, namespace, onSaved })；close() 关闭。
 *
 * 深链（同一份 DOM，e2e 锚点一致）：
 *   · sources.html?edit=<name>     → 列表页打开；
 *   · source-detail.html?…&edit=1  → 详情页加载完后打开。
 */
(function () {
  "use strict";
  const A = window.Anotify;
  /* 前缀包装器：check-i18n-keys 能据此还原出 sources.v4.<key> / uxaudit.res.sources.<key>。 */
  const t = (key) => A.t("sources.v4." + key);
  const xt = (key) => A.t("uxaudit.res.sources." + key);
  const $ = (id) => document.getElementById(id);
  const el = (tag, cls, text) => {
    const n = document.createElement(tag);
    n.className = cls || "";
    if (text != null) n.textContent = String(text);
    return n;
  };
  const action = (key, fn) => {
    const b = el("button", "btn btn-ghost btn-sm", t(key));
    b.type = "button";
    b.onclick = () => fn(b);
    return b;
  };

  const MARKUP = `<div id="src-modal" class="modal-backdrop hidden" role="dialog" aria-modal="true" aria-labelledby="modal-title">
<div class="modal-panel">
<div class="modal-head">
<h3 id="modal-title">
</h3>
<button id="modal-close" class="btn btn-ghost btn-icon" type="button" aria-label="${A.t("common.action.close")}">
✕
</button>
</div>
<!-- SRC-6：body 可滚（.modal-body 自带 overflow-y:auto）、footer 粘底（.modal-foot 是
     面板的固定末行）。内容按「基本 / 载荷 / 目标 / 其它」分段，不再是一列 14 个字段。 -->
<div id="src-modal-body" class="modal-body src-modal-body">
<div class="source-form">

<div class="src-group">
<div class="src-group-head"><span class="src-group-title">${A.t("uxaudit.res.sources.group_basic")}</span><span class="src-group-hint">${A.t("uxaudit.res.sources.group_basic_hint")}</span></div>
<div class="src-cols">
<div class="field">
<label class="field-label" for="f-name">
${A.t("sources.v4.name")}
</label>
<input id="f-name" class="input"  />
<p id="f-name-error" class="field-error hidden" role="alert"></p>
</div>
<div class="field">
<label class="field-label" for="f-display">
${A.t("sources.v4.display")}
</label>
<input id="f-display" class="input"  />
</div>
</div>
<div class="field">
<label class="field-label" for="f-kind">
${A.t("sources.v4.kind")}
</label>
<select id="f-kind" class="select">
<option value="cron">${A.t("sources.kind.cron")}</option>
<option value="webhook">${A.t("sources.kind.webhook")}</option>
<option value="manual">${A.t("sources.kind.manual")}</option>
<option value="poll">${A.t("sources.kind.poll")}</option>
</select>
<p class="field-hint">
${A.t("sources.v4.immutable")}
</p>
</div>
<div id="f-cron-wrap">
<div class="src-cols">
<div class="field">
<label class="field-label" for="f-expr">
<span id="f-expr-label">${A.t("sources.v4.expr")}</span>
</label>
<input id="f-expr" class="input"  />
</div>
<div class="field">
<label class="field-label" for="f-tz">
${A.t("sources.v4.tz")}
</label>
<input id="f-tz" class="input"  />
</div>
</div>
<!-- #379：cron 预设。i18n 里本来就有 sources.preset.* 四个键但页面 0 引用，
     用户得手写五段表达式。点一下写进 #f-expr（写进输入框后仍可继续改）。 -->
<div id="cron-presets" class="source-hints" role="group" aria-label="${A.t("sources.preset.label")}">
<button type="button" class="btn btn-ghost btn-sm" data-cron="*/15 * * * *">${A.t("sources.preset.every15")}</button>
<button type="button" class="btn btn-ghost btn-sm" data-cron="0 * * * *">${A.t("sources.preset.hourly")}</button>
<button type="button" class="btn btn-ghost btn-sm" data-cron="0 3 * * *">${A.t("sources.preset.daily3")}</button>
<button type="button" class="btn btn-ghost btn-sm" data-cron="0 9 * * 1-5">${A.t("sources.preset.weekdays9")}</button>
</div>
</div>
<div id="f-poll-wrap" class="hidden">
<div class="src-group">
<div class="src-group-head"><span class="src-group-title">${t("poll_transport")}</span><span class="src-group-hint">${t("poll_transport_hint")}</span></div>
<div class="field">
<label class="field-label" for="f-poll-url">
${A.t("source_detail.poll_url")}
</label>
<input id="f-poll-url" class="input mono" />
<p id="f-poll-url-error" class="field-error hidden" role="alert"></p>
<p class="field-hint">
${t("poll_url_hint")}
</p>
</div>
<div class="field">
<div class="field-label">
${t("poll_headers")}
</div>
<div id="f-poll-headers" class="source-form">
</div>
<button id="add-poll-header" type="button" class="btn btn-ghost">
${t("poll_header_add")}
</button>
<p id="f-poll-headers-error" class="field-error hidden" role="alert"></p>
<p class="field-hint">
${t("poll_headers_hint")}
</p>
</div>
<div class="src-cols">
<div class="field">
<label class="field-label" for="f-poll-timeout">
${A.t("source_detail.poll_timeout")}
</label>
<input id="f-poll-timeout" class="input" type="number" min="1000" max="120000" step="1000" />
</div>
<div class="field">
<label class="field-label" for="f-poll-maxitems">
${A.t("source_detail.poll_max_items")}
</label>
<input id="f-poll-maxitems" class="input" type="number" min="1" max="500" />
</div>
</div>
<p class="field-hint">
${t("poll_limits_hint")}
</p>
<div class="src-group-head"><span class="src-group-title">${t("poll_cred")}</span><span class="src-group-hint">${t("poll_cred_hint")}</span></div>
<div class="src-cols">
<div class="field">
<label class="field-label" for="f-poll-cred-name">
${t("poll_cred_name")}
</label>
<input id="f-poll-cred-name" class="input mono" />
</div>
<div class="field">
<label class="field-label" for="f-poll-cred-header">
${t("poll_cred_header")}
</label>
<input id="f-poll-cred-header" class="input mono" />
</div>
</div>
<div class="field">
<label class="field-label" for="f-poll-cred-prefix">
${t("poll_cred_prefix")}
</label>
<input id="f-poll-cred-prefix" class="input mono" />
</div>
<div class="field">
<label class="field-label" for="f-poll-secret">
${t("poll_secret")}
</label>
<input id="f-poll-secret" class="input" type="password" autocomplete="new-password" />
<p class="field-hint">
${t("poll_secret_set")}
</p>
</div>
</div>
<div class="src-group">
<div class="src-group-head"><span class="src-group-title">${A.t("source_detail.poll_envelope")}</span><span class="src-group-hint">${A.t("source_detail.poll_env_hint")}</span></div>
<div class="src-cols">
<div class="field">
<label class="field-label" for="f-env-items">
${A.t("source_detail.poll_env_items")}
</label>
<input id="f-env-items" class="input mono" />
</div>
<div class="field">
<label class="field-label" for="f-env-type">
${A.t("source_detail.poll_env_type")}
</label>
<input id="f-env-type" class="input mono" />
</div>
</div>
<div class="src-cols">
<div class="field">
<label class="field-label" for="f-env-delivery">
${A.t("source_detail.poll_env_delivery")}
</label>
<input id="f-env-delivery" class="input mono" />
</div>
<div class="field">
<label class="field-label" for="f-env-payload">
${A.t("source_detail.poll_env_payload")}
</label>
<input id="f-env-payload" class="input mono" />
</div>
</div>
<label class="field-label">
<input id="f-env-payload-whole" type="checkbox" /> ${t("poll_env_whole")}
</label>
</div>
</div>
<div id="f-hook-wrap" class="hidden">
<div class="src-group">
<div class="src-group-head"><span class="src-group-title">${A.t("uxaudit.res.sources.group_credentials")}</span><span class="src-group-hint">${A.t("sources.v4.blank_credentials")}</span></div>
<div class="src-cols">
<div class="field">
<label class="field-label" for="f-token">
${A.t("sources.v4.token")}
</label>
<input id="f-token" class="input" type="password" autocomplete="new-password" />
</div>
<div class="field">
<label class="field-label" for="f-secret">
${A.t("sources.v4.secret")}
</label>
<input id="f-secret" class="input" type="password" autocomplete="new-password" />
</div>
</div>
<div class="field">
<label class="field-label" for="f-sample">
${A.t("sources.v4.sample")}
</label>
<textarea id="f-sample" class="textarea mono" rows="3">
</textarea>
</div>
<div id="hook-read" class="source-wrap">
</div>
<p class="field-hint">
${A.t("sources.v4.hmac")}
</p>
</div>
</div>
</div>

<div class="src-group">
<div class="src-group-head"><span class="src-group-title">${A.t("uxaudit.res.sources.group_payload")}</span><span class="src-group-hint">${A.t("uxaudit.res.sources.group_payload_hint")}</span></div>
<div id="f-payload-wrap">
<div class="field">
<label class="field-label" for="f-payload">
${A.t("sources.v4.payload")}
</label>
<textarea id="f-payload" class="textarea mono" rows="3">
</textarea>
</div>
</div>
<div class="field">
<label class="field-label" for="f-payload-schema">
${A.t("sources.v4.schema")}
</label>
<textarea id="f-payload-schema" class="textarea mono" rows="3">
</textarea>
<p class="field-hint">
${A.t("sources.v4.schema_hint")}
</p>
</div>
</div>

<div class="src-group">
<div class="src-group-head"><span class="src-group-title">${A.t("uxaudit.res.sources.group_targets")}</span><span class="src-group-hint">${A.t("uxaudit.res.sources.group_targets_hint")}</span></div>
<div class="field">
<div id="f-targets" class="source-form">
</div>
<button id="add-target" type="button" class="btn btn-ghost">
${A.t("sources.v4.add_target")}
</button>
<datalist id="source-capabilities">
</datalist>
<p class="field-hint">
${A.t("sources.v4.params_hint")}
</p>
</div>
</div>

<div class="src-group">
<div class="src-group-head"><span class="src-group-title">${A.t("uxaudit.res.sources.group_other")}</span><span class="src-group-hint">${A.t("uxaudit.res.sources.group_other_hint")}</span></div>
<label class="field-label">
<input id="f-enabled" type="checkbox" /> ${A.t("sources.v4.enabled")}
</label>
<div class="field">
<label class="field-label" for="dry-payload">
${A.t("sources.v4.override")}
</label>
<textarea id="dry-payload" class="textarea mono" rows="3">
</textarea>
<p class="field-hint">
${A.t("sources.v4.omitted")}
</p>
</div>
<div>
<div class="field-label">
${A.t("sources.v4.suggestions")}
</div>
<p id="avail-src" class="field-hint">
</p>
<div id="avail-vars-body" class="source-hints">
</div>
</div>
<div id="form-result" aria-live="polite" class="source-wrap">
</div>
<button id="reload-draft" class="btn btn-ghost hidden" type="button">
${A.t("sources.v4.reload_draft")}
</button>
</div>

</div>
</div>
<div class="modal-foot">
<button id="modal-cancel" class="btn btn-ghost" type="button">
${A.t("uxaudit.res.common.cancel")}
</button>
<button id="btn-dry" class="btn btn-ghost">
${A.t("sources.v4.draft_dry")}
</button>
<button id="modal-save" class="btn">
${A.t("sources.v4.save")}
</button>
</div>
</div>
</div>`;

  /* ---------- 上下文（调用方给的，不是全局隐式状态） ---------- */
  const CTX = { onSaved: null, onClose: null, namespace: "" };
  let DOM = null;
  let EDITING = null;
  let EVENTS = [];
  let CAPS = [];
  let editorSeq = 0;
  /* 读投影里没有 headers/secretRef/allowInsecure ⇒ 打开时另读原始 config 当基底；读不到就置 POLLERR。 */
  let POLLCFG = null;
  let POLLERR = null;
  let KIND = null;
  let GUARD = null;
  const POLL_HEADER_MAX = 10;
  const POLL_TIMEOUT_DEFAULT = 10000;
  const POLL_MAXITEMS_DEFAULT = 100;

  /* 当前命名空间：调用方给的资源 ns 优先（详情页深链的权威来源），否则才用持久化 scope。 */
  function nsNow() {
    const own = typeof CTX.namespace === "string" ? CTX.namespace.trim() : "";
    if (own) return own;
    const scoped = A.readScope().namespace;
    return typeof scoped === "string" ? scoped.trim() : "";
  }
  function nsBase(plural) {
    return "/v1/namespaces/" + encodeURIComponent(nsNow()) + "/" + plural;
  }
  /* 编辑器自己的请求：路径用 nsNow()，不靠全局 nsPath（详情页可能 scope≠资源 ns）。 */
  const request = (name, body, suffix = "", method = "POST") => {
    const base = nsBase("sources");
    const url = (name ? base + "/" + encodeURIComponent(name) : base) + suffix;
    const opts = { method };
    if (body !== null && body !== undefined) opts.body = JSON.stringify(body);
    return A.api(url, opts);
  };
  async function loadCaps() {
    try {
      const doc = await A.api(nsBase("capabilities") + "?limit=500");
      CAPS = A.itemsOf(doc);
    } catch (e) {
      CAPS = [];
    }
    return CAPS;
  }
  async function loadEvents() {
    try {
      const doc = await A.api(nsBase("events") + "?limit=500");
      return A.itemsOf(doc);
    } catch (e) {
      return [];
    }
  }

const json = (box, optional=false) => {
  const raw=box.value.trim();
  if(!raw)return optional ? undefined : {
  };
  const value=A.parseJSON(raw,box.id);
  if(!value||typeof value!=="object"||Array.isArray(value))throw new Error(t("object_required"));
  return value;
};

const CAUSE_FIELDS={
  name:"f-name",
  displayName:"f-display",
  kind:"f-kind",
  "config.url":"f-poll-url",
  "config.schedule":"f-expr",
  "config.schedule.expr":"f-expr",
  "config.schedule.tz":"f-tz",
  "config.timeoutMs":"f-poll-timeout",
  "config.maxItems":"f-poll-maxitems",
  "config.headers":"f-poll-headers",
  "config.secretRef":"f-poll-cred-name",
  "config.secret":"f-poll-secret",
  "config.envelope":"f-env-items",
  "config.payload":"f-payload",
  "config.payloadSchema":"f-payload-schema",
  "config.expr":"f-expr",
  "config.tz":"f-tz",
  targets:"f-targets",
};
function error(box, err, action){
  const res=A.applyCauses($("src-modal-body"),err,CAUSE_FIELDS);
  if(res.causes.length){
    box.replaceChildren(A.causeReport(err,res,{action}));
    return;
  }
  box.replaceChildren(el("p","source-error",[err.reason,err.message].filter(Boolean).join(": ")));
}

function preview(box, res){
  box.replaceChildren(el("h4","card-title",t("preview")));
  if(!res.targets.length)box.append(el("p","card-sub",t("no_target")));
  for(const row of res.targets){
    const p=el("div","source-target");
    p.append(el("strong","source-wrap",row.capability+(row.version ? "@"+row.version : "")+" · "+row.state),el("pre","source-json mono",JSON.stringify(row.params,null,2)));
    if(row.reason)p.append(el("p","source-error",row.reason+": "+row.detail));
    box.append(p);
  }for(const row of res.pruned)box.append(el("p","source-error",[row.capability,row.reason,row.detail].filter(Boolean).join(" · ")));
}
function targetRow(target={
  capability:"",params:{
  }
}){
  const row=el("div","source-target");
  const label=el("label","field-label",t("capability"));
  const cap=el("input","input");
  cap.setAttribute("aria-label",t("capability"));
  cap.setAttribute("list","source-capabilities");
  cap.value=target.capability;
  label.append(cap);
  const paramLabel=el("label","field-label",t("params"));
  const params=el("textarea","textarea mono source-params");
  params.setAttribute("aria-label",t("params"));
  params.value=JSON.stringify(target.params||{
  },null,2);
  paramLabel.append(params);
  row.append(label,paramLabel,action("remove_target",()=>row.remove()));
  return row;
}

  /* ---------- 打开 / 关闭 ---------- */
async function openEditor(src){
  const seq=++editorSeq;
  EDITING=src;
  EVENTS=[];
  const m=src ? A.model.source(src) : null;
  const cfg=m?.config||{
  };
  $("modal-title").textContent=t(src?"edit":"new");
  $("f-name").value=m?.name||"";
  $("f-name").disabled=!!src;
  $("f-display").value=m?.displayName||"";
  $("f-kind").value=m?.kind||"manual";
  $("f-kind").disabled=!!src;
  $("f-enabled").checked=m ? m.enabled : true;
  $("f-expr").value=cfg.expr||"0 3 * * *";
  $("f-tz").value=cfg.tz||"Local";
  for(const [id,value] of [["f-payload",cfg.payload],["f-sample",cfg.sample],["f-payload-schema",cfg.payloadSchema]])$(id).value=value ? JSON.stringify(value,null,2) : "";
  $("f-token").value="";
  $("f-secret").value="";
  $("dry-payload").value="";
  KIND=null;
  POLLCFG=null;
  POLLERR=null;
  resetPollForm();
  /* #637：清理必须先于读取 —— 原来清在读取之后，读取失败写进去的提示正好被它擦掉。 */
  $("form-result").replaceChildren();
  if(m?.kind==="poll"){
    try{
      const doc=await request(m.name,null,"","GET");
      if(seq!==editorSeq)return;
      POLLCFG=(doc.spec||{
      }).config||{
      };
      fillPollForm(POLLCFG);
    }catch(err){
      if(seq!==editorSeq)return;
      pollLoadFailed(err);
    }
  }
  $("f-targets").replaceChildren(...(m?.targets||[]).map(targetRow));
  $("source-capabilities").replaceChildren(...CAPS.map(cap=>{
    const opt=el("option");opt.value=cap.metadata.name;return opt;
  }));
  $("reload-draft").classList.add("hidden");
  $("hook-read").replaceChildren();
  if(cfg.hookUrl)$("hook-read").append(el("code","source-wrap",cfg.hookUrl),el("p","source-status","secretSet: "+String(cfg.secretSet)));
  syncKind();
  syncEditorLocks();
  A.openModal($("src-modal"));
  if(POLLERR)scrollToResult(); // 打开之前滚是空操作（弹层还 hidden），所以补在 openModal 之后
  syncScrollHint();
  suggest();
  if(GUARD)GUARD.markClean();
  try{
    const events={ items: await loadEvents() };
    if(seq!==editorSeq)return;
    EVENTS=events.items.filter(e=>e.spec.involvedObject?.name===m?.name && e.spec.reason==="Ingested").sort((a,b)=>b.metadata.creationTimestamp.localeCompare(a.metadata.creationTimestamp));
    suggest();
  }catch(err){
    error($("avail-src"),err);
  }
}

function syncKind(){
  const kind=$("f-kind").value;
  const prev=KIND;
  KIND=kind;
  /* poll 的排期与 cron 是同一套输入（同一个 #f-expr/#f-tz/#cron-presets），只是标签换成「排期」。 */
  $("f-cron-wrap").classList.toggle("hidden",kind!=="cron"&&kind!=="poll");
  $("f-expr-label").textContent=kind==="poll" ? A.t("source_detail.poll_schedule") : t("expr");
  $("f-poll-wrap").classList.toggle("hidden",kind!=="poll");
  $("f-hook-wrap").classList.toggle("hidden",kind!=="webhook");
  $("f-payload-wrap").classList.toggle("hidden",kind==="webhook");
  if(prev&&prev!==kind)clearKindFields(prev);
  suggest();
}
/* 新建时切 kind：另一类的字段必须当场清掉，不能只在提交时忽略（否则草稿里还留着 webhook 的 token）。 */
function clearKindFields(prev){
  if(prev==="webhook"){
    $("f-token").value="";
    $("f-secret").value="";
    $("f-sample").value="";
    $("hook-read").replaceChildren();
  }
  if(prev==="poll")resetPollForm();
}
function failField(field,message){
  const err=new Error(message);
  err.field=field;
  return err;
}
function headerRow(key="",value=""){
  const row=el("div","source-target");
  const kLabel=el("label","field-label",t("poll_header_key"));
  const k=el("input","input mono");
  k.value=key;
  kLabel.append(k);
  const vLabel=el("label","field-label",t("poll_header_value"));
  const v=el("input","input mono");
  v.value=value;
  vLabel.append(v);
  row.append(kLabel,vLabel,action("remove_target",()=>{
    row.remove();
    syncHeaderCap();
  }));
  return row;
}
function syncHeaderCap(){
  $("add-poll-header").disabled=$("f-poll-headers").children.length>=POLL_HEADER_MAX;
}
function resetPollForm(){
  A.clearCauses($("src-modal-body"));
  for(const id of ["f-poll-url","f-poll-cred-name","f-poll-cred-header","f-poll-cred-prefix","f-poll-secret","f-env-items","f-env-type","f-env-delivery"])$(id).value="";
  $("f-poll-timeout").value=POLL_TIMEOUT_DEFAULT;
  $("f-poll-maxitems").value=POLL_MAXITEMS_DEFAULT;
  $("f-poll-headers").replaceChildren(headerRow());
  $("f-env-payload-whole").checked=false;
  $("f-env-payload").value="";
  $("f-env-payload").disabled=false;
  flagField("f-poll-url","");
  flagField("f-poll-headers","");
  syncHeaderCap();
}
function fillPollForm(cfg){
  const sched=cfg.schedule||{
  };
  const ref=cfg.secretRef||{
  };
  const env=cfg.envelope||{
  };
  $("f-expr").value=sched.expr||"*/15 * * * *";
  $("f-tz").value=sched.tz||"Local";
  $("f-poll-url").value=cfg.url||"";
  $("f-poll-timeout").value=cfg.timeoutMs==null ? POLL_TIMEOUT_DEFAULT : cfg.timeoutMs;
  $("f-poll-maxitems").value=cfg.maxItems==null ? POLL_MAXITEMS_DEFAULT : cfg.maxItems;
  $("f-poll-cred-name").value=ref.name||"";
  $("f-poll-cred-header").value=ref.header||"";
  $("f-poll-cred-prefix").value=ref.prefix||"";
  const rows=Object.entries(cfg.headers||{
  }).map(([k,v])=>headerRow(k,v));
  $("f-poll-headers").replaceChildren(...(rows.length ? rows : [headerRow()]));
  $("f-env-items").value=env.items||"";
  $("f-env-type").value=(env.type||{
  }).pointer||"";
  $("f-env-delivery").value=(env.deliveryId||{
  }).pointer||"";
  const whole=(env.payload||{
  }).whole===true;
  $("f-env-payload-whole").checked=whole;
  $("f-env-payload").value=whole ? "" : (env.payload||{
  }).pointer||"";
  $("f-env-payload").disabled=whole;
  syncHeaderCap();
}
/* #637：poll 原始 config 没读到 ⇒ 表单只有默认值，存下去就是静默丢配置：弹层照开 + 表单/保存/试运行锁死 + 重试。 */
function pollLoadFailed(err,notify){
  POLLERR=err||new Error(t("poll_load_blocked"));
  const detail=[POLLERR.reason,POLLERR.message].filter(Boolean).join(": ")||String(POLLERR.status||"");
  const msg=t("poll_load_failed").replace("{reason}",detail||"?");
  const box=$("form-result");
  box.replaceChildren(el("p","source-error",msg));
  const retry=el("button","btn btn-ghost btn-sm",t("poll_load_retry"));
  retry.type="button";
  retry.id="poll-reload";
  retry.setAttribute("data-err-action","retry");
  retry.onclick=reloadPollConfig;
  box.append(retry);
  syncEditorLocks();
  // 只写 #form-result 仍然看不见：它在可滚动的 .modal-body 末尾（poll 表单很高）⇒ 按 W2-4 的口径补错误条 + 滚进视口
  if(notify!==false)A.toast(msg,"error");
  scrollToResult();
}
function scrollToResult(){
  const box=$("form-result");
  if(typeof box.scrollIntoView==="function")box.scrollIntoView({
    block:"nearest"
  });
}
const LOCKED=new Set(); // 读不到基底就锁表单：否则先填再重试，fillPollForm 会静默覆盖刚填的（只恢复这里锁过的，别碰自带禁用口径的控件）
function syncEditorLocks(){
  const blocked=!!POLLERR;
  $("modal-save").disabled=blocked;
  $("btn-dry").disabled=blocked;
  for(const node of LOCKED)node.disabled=false;
  LOCKED.clear();
  if(!blocked)return;
  for(const node of $("src-modal-body").querySelectorAll("input,textarea,select,button")){
    if(node.disabled||node.dataset.errAction)continue;
    node.disabled=true;
    LOCKED.add(node);
  }
}
/* 重试：再读一次原始 config。序号 + EDITING 双检 —— 等待期间弹层可能被关掉或换了源。 */
async function reloadPollConfig(){
  const target=EDITING,seq=editorSeq,btn=$("poll-reload");
  if(!target)return;
  if(btn)btn.disabled=true;
  try{
    const doc=await request(target.metadata.name,null,"","GET");
    if(seq!==editorSeq||EDITING!==target)return;
    POLLCFG=(doc.spec||{
    }).config||{
    };
    POLLERR=null;
    syncEditorLocks(); // 先解锁再回填：fillPollForm 自己的禁用口径（whole 载荷）不能被解锁盖掉
    fillPollForm(POLLCFG);
    $("form-result").replaceChildren();
  }catch(err){
    if(seq!==editorSeq||EDITING!==target)return;
    pollLoadFailed(err);
  }
}
function schemaPaths(schema,root="payload"){
  return Object.entries(schema?.properties||{
  }).flatMap(([k,v])=>{
    const p=root+"."+k;return [p,...schemaPaths(v,p),...(v.items ? schemaPaths(v.items,p+".0") : [])];
  });
}
function dataPaths(value,root="payload"){
  if(!value||typeof value!=="object")return [];
  return Object.keys(value).flatMap(k=>{
    const p=root+"."+k;return [p,...dataPaths(value[k],p)];
  });
}
function suggest(){
  try{
    const dry=json($("dry-payload"),true);
    const sample=json($("f-sample"),true);
    const schema=json($("f-payload-schema"),true);
    const payload=json($("f-payload"),true);
    let value,source,paths;
    if(dry){
      value=dry;
      source="suggest_dry";
    }else if(EDITING && EVENTS.length){
      value=EVENTS[0].spec.payload;
      source="suggest_event";
    }else if($("f-kind").value==="webhook" && sample){
      value=sample;
      source="suggest_sample";
    }else if(schema){
      paths=schemaPaths(schema);
      source="suggest_schema";
    }else{
      value=payload||{
      };
      source="suggest_payload";
    }$("avail-src").textContent=t(source);
    const values=["payload",...(paths||dataPaths(value))];
    $("avail-vars-body").replaceChildren(...values.map(p=>{
      const token="$("+p+")";const b=el("button","btn btn-ghost btn-sm mono",token);b.type="button";b.onclick=()=>A.copyText(b,()=>token);return b;
    }));
  }catch(err){
    error($("avail-src"),err);
  }
}
function writeSource(){
  const kind=$("f-kind").value;
  const config=kind==="poll" ? pollConfig() : {
    payloadSchema:json($("f-payload-schema"),true)||null
  };
  if(kind!=="poll"){
    if(kind==="cron"){
      config.expr=$("f-expr").value.trim();
      config.tz=$("f-tz").value.trim()||"Local";
    }if(kind==="webhook"){
      config.sample=json($("f-sample"));
      for(const key of ["token","secret"]){
        const value=$("f-"+key).value.trim();
        if(value)config[key]=value;
      }
    }else config.payload=json($("f-payload"));
  }
  const targets=[...$("f-targets").children].map(row=>({
    capability:row.querySelector("input").value.trim(),params:json(row.querySelector("textarea"))
  }));
  const name=$("f-name").value.trim();
  /* 必填失败必须给出看得见的反馈（字段红框 + 文案 + toast）：只写进 #form-result 在可视区外。 */
  if(!name){
    const err=new Error(t("name_required"));
    err.field="name";
    throw err;
  }
  if(targets.some(x=>!x.capability)){
    const err=new Error(t("capability_required"));
    err.field="targets";
    throw err;
  }
  return {
    apiVersion:"anotify/v1",kind:"Source",metadata:{
      name,namespace:nsNow(),displayName:$("f-display").value.trim()
    },spec:{
      kind,enabled:$("f-enabled").checked,config,targets
    }
  };
}
function setOrDrop(cfg,key,value){
  const empty=value==null||(typeof value==="object"&&!Array.isArray(value)&&!Object.keys(value).length);
  if(empty)delete cfg[key];
  else cfg[key]=value;
}
function intOr(box,dflt){
  const raw=box.value.trim();
  const n=Number(raw);
  return raw!==""&&Number.isFinite(n) ? Math.trunc(n) : dflt;
}
function collectHeaders(){
  const out={
  };
  for(const row of $("f-poll-headers").children){
    const inputs=row.querySelectorAll("input");
    const key=inputs[0].value.trim();
    if(!key)continue;
    if(Object.prototype.hasOwnProperty.call(out,key))throw failField("f-poll-headers",t("poll_header_dup")+": "+key);
    out[key]=inputs[1].value;
  }
  return out;
}
function collectSecretRef(){
  const name=$("f-poll-cred-name").value.trim();
  const header=$("f-poll-cred-header").value.trim();
  const prefix=$("f-poll-cred-prefix").value;
  if(!name&&!header)return null;
  if(!name||!header)throw failField("f-poll-cred-name",t("poll_cred_pair"));
  const ref={
    name,header
  };
  if(prefix!=="")ref.prefix=prefix;
  return ref;
}
function collectEnvelope(){
  const env={
  };
  const items=$("f-env-items").value.trim();
  const type=$("f-env-type").value.trim();
  const delivery=$("f-env-delivery").value.trim();
  const payload=$("f-env-payload").value.trim();
  if(items)env.items=items;
  if(type)env.type={
    pointer:type
  };
  if(delivery)env.deliveryId={
    pointer:delivery
  };
  if($("f-env-payload-whole").checked)env.payload={
    whole:true
  };
  else if(payload)env.payload={
    pointer:payload
  };
  return Object.keys(env).length ? env : null;
}
function pollConfig(){
  /* 编辑已有 poll 源时 config 只以服务器那份为基底、只覆盖表单 own 的键：拿不到基底就只剩读投影，保存会静默抹掉 headers / sample / allowInsecure / secretRef / envelope（#540 / #637）。 */
  if(EDITING && !POLLCFG)throw new Error(t("poll_load_blocked"));
  const cfg=POLLCFG ? JSON.parse(JSON.stringify(POLLCFG)) : {
  };
  const url=$("f-poll-url").value.trim();
  if(!url)throw failField("f-poll-url",t("poll_url_required"));
  cfg.url=url;
  cfg.schedule={
    expr:$("f-expr").value.trim(),tz:$("f-tz").value.trim()||"Local"
  };
  cfg.timeoutMs=intOr($("f-poll-timeout"),POLL_TIMEOUT_DEFAULT);
  cfg.maxItems=intOr($("f-poll-maxitems"),POLL_MAXITEMS_DEFAULT);
  setOrDrop(cfg,"headers",collectHeaders());
  setOrDrop(cfg,"secretRef",collectSecretRef());
  setOrDrop(cfg,"envelope",collectEnvelope());
  setOrDrop(cfg,"payload",json($("f-payload"),true));
  setOrDrop(cfg,"payloadSchema",json($("f-payload-schema"),true));
  const secret=$("f-poll-secret").value;
  if(secret!=="")cfg.secret=secret;
  else delete cfg.secret;
  return cfg;
}
function flagField(id,message){
  const input=$(id),box=$(id+"-error");
  if(!input)return;
  input.classList.toggle("is-invalid",!!message);
  /* aria-invalid 与动态 cause hint 是 applyCauses 的通道，flagField 不清就会与红框脱节。 */
  if(message)input.setAttribute("aria-invalid","true");
  else{
    input.removeAttribute("aria-invalid");
    const stale=document.querySelector('[data-cause-for="'+id+'"]');
    if(stale)stale.remove();
  }
  if(box){
    box.textContent=message||"";
    box.classList.toggle("hidden",!message);
  }
  if(message){
    input.focus();
    if(typeof input.scrollIntoView==="function")input.scrollIntoView({
      block:"center"
    });
  }
}

    function bindActions() {
  /* ---------- 提交 / 试运行 / 409 重载 ---------- */
  $("modal-save").onclick=async()=>{
    const btn=$("modal-save");
    btn.disabled=true;
    try{
      let body=writeSource();
      if(EDITING)body=A.withResourceVersion(body,EDITING.metadata.resourceVersion);
      await request(EDITING?.metadata.name,body,"",EDITING?"PATCH":"POST");
      /* 统一走 closeEditor：清凭据 + EDITING，并通知宿主（详情页据此移除 URL 上的 edit=1）。 */
      closeEditor();
      await (CTX.onSaved ? CTX.onSaved() : Promise.resolve());
    }catch(err){
      if(POLLERR)pollLoadFailed(POLLERR,false); // 被 pollConfig() 拦下时重试入口必须留在原处（错误条由下面那句 toast 统一给）
      else error($("form-result"),err,"save");
      /* 校验类错误：字段红框 + 立即可见的 toast（W2-4）；带 causes 时 toast 只报条数。 */
      const causes=err.details&&Array.isArray(err.details.causes)?err.details.causes:[];
      if(err.field)flagField(err.field,err.message);
      else if(causes.length)A.toast(A.causeTitle("save",causes.length),"error");
      else A.toast([err.reason,err.message].filter(Boolean).join(": ")||t("save_failed"),"error");
      if(err.field==="targets")A.toast(err.message,"error");
      if(A.isConflict(err)||err.reason==="resource_version_required"){
        $("form-result").append(el("p","field-hint",t("draft_preserved")));
        $("reload-draft").classList.remove("hidden");
      }
    }finally{
      btn.disabled=false;
      syncEditorLocks();
    }
  };

  $("btn-dry").onclick=async()=>{
    const btn=$("btn-dry");
    btn.disabled=true;
    try{
      /* 未保存源的 dry-run 请求体是 InlinePreviewBody：`source` 是 InlineSourceDraft，metadata
       * 只认 name/displayName（additionalProperties:false），多塞 namespace 会被严格解码拒掉。 */
      const envelope=writeSource();
      const source={
        apiVersion:envelope.apiVersion,
        kind:envelope.kind,
        metadata:{name:envelope.metadata.name,displayName:envelope.metadata.displayName},
        spec:envelope.spec
      };
      const payload=json($("dry-payload"),true);
      preview($("form-result"),await request("dry-run",Object.assign({
        source
      },payload===undefined?{
      }:{
        payload
      })));
    }catch(err){
      if(POLLERR)pollLoadFailed(POLLERR,false); // 试运行同样过 pollConfig() 闸门：被拦下也要留住重试入口
      else error($("form-result"),err,"preview");
      const causes=err.details&&Array.isArray(err.details.causes)?err.details.causes:[];
      if(causes.length)A.toast(A.causeTitle("preview",causes.length),"error");
      else A.toast([err.reason,err.message].filter(Boolean).join(": ")||t("save_failed"),"error");
    }finally{
      btn.disabled=false;
      syncEditorLocks();
    }
  };

  $("reload-draft").onclick=async()=>{
    if(!(await A.confirmDanger({
      body:t("reload_confirm")
    })))return;
    try{
      const src=await request(EDITING.metadata.name, null, "", "GET");
      await openEditor(src);
    }catch(err){
      error($("form-result"),err);
    }
  };
  }

  function closeEditor() {
    const done = CTX.onClose;
    CTX.onClose = null;
    /* 关掉弹层只隐藏 DOM，标记不清就会活到下一次打开，在新表单上报上一轮的错。 */
    A.clearCauses($("src-modal-body"));
    A.closeModal($("src-modal"));
    $("f-token").value="";
    $("f-secret").value="";
    $("f-poll-secret").value="";
    POLLCFG=null;
    POLLERR=null;
    syncEditorLocks();
    flagField("f-name","");
    EDITING=null;
    if (GUARD) GUARD.markClean();
    if (done) {
      try {
        done();
      } catch (e) {
        /* 关闭回调只是让宿主同步自己的 URL/状态，失败不该吃掉关闭动作 */
      }
    }
  }

  /* ---------- 弹层滚动提示 ---------- */
function syncScrollHint(){
  const body=$("src-modal-body");
  if(!body)return;
  const more=body.scrollHeight-body.clientHeight-body.scrollTop>8;
  body.classList.toggle("is-more",more);
}

  function wire() {
    /* Esc：openModal 的默认逃生门只切 class，不跑收尾逻辑（凭据/EDITING 都要清），
       所以拦下 modal:dismiss 交给脏检查（闸门未就绪时退回直接关，别让 Esc 变死键）。 */
    DOM.addEventListener("modal:dismiss", (event) => {
      event.preventDefault();
      if (GUARD) GUARD.requestClose();
      else closeEditor();
    });
    bindActions();
    $("modal-close").onclick = () => (GUARD ? GUARD.requestClose() : closeEditor());
    $("modal-cancel").onclick = () => (GUARD ? GUARD.requestClose() : closeEditor());
    $("f-name").addEventListener("input", () => flagField("f-name", ""));
    $("f-kind").onchange = syncKind;
    $("add-target").onclick = () => $("f-targets").append(targetRow());
    $("add-poll-header").onclick = () => {
      if ($("f-poll-headers").children.length >= POLL_HEADER_MAX) return;
      $("f-poll-headers").append(headerRow());
      flagField("f-poll-headers", "");
      syncHeaderCap();
    };
    $("f-env-payload-whole").onchange = () => {
      const whole = $("f-env-payload-whole").checked;
      if (whole) $("f-env-payload").value = "";
      $("f-env-payload").disabled = whole;
    };
    for (const btn of document.querySelectorAll("#cron-presets [data-cron]")) {
      btn.onclick = () => {
        $("f-expr").value = btn.getAttribute("data-cron");
        $("f-expr").focus();
      };
    }
    for (const id of ["dry-payload", "f-sample", "f-payload-schema", "f-payload"]) $(id).oninput = suggest;
    syncScrollHint();
    const body = $("src-modal-body");
    body.addEventListener("scroll", syncScrollHint, { passive: true });
    window.addEventListener("resize", syncScrollHint);
  }

  function ensureDom() {
    if (DOM) return DOM;
    const host = document.createElement("div");
    host.innerHTML = MARKUP;
    DOM = host.firstElementChild;
    document.body.append(DOM);
    wire();
    GUARD = A.guardDirty(DOM, { onDiscard: closeEditor });
    return DOM;
  }

  /* ---------- 对外 API ---------- */
  async function open(opts) {
    const o = opts || {};
    CTX.onSaved = typeof o.onSaved === "function" ? o.onSaved : null;
    CTX.onClose = typeof o.onClose === "function" ? o.onClose : null;
    CTX.namespace = typeof o.namespace === "string" ? o.namespace : "";
    ensureDom();
    if (Array.isArray(o.caps)) CAPS = o.caps;
    else await loadCaps();
    await openEditor(o.resource || null);
  }
  function close() {
    if (DOM) closeEditor();
  }

  A.sourceEditor = { open, close };
})();
