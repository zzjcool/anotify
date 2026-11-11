// Read-only Trace evidence. No polling, API promise branches or fixture changes.
// The factory is serialized into the driver's existing isolated browser context.
export function traceEvidenceTools() {
  const type = v => v === null ? 'null' : Array.isArray(v) ? 'array' : typeof v;
  const fields = (v, keys) => Object.fromEntries(keys.map(k => [k, type(v?.[k])]));
  const scalar = v => ['string', 'number', 'boolean'].includes(typeof v) ? v : undefined;
  const clean = value => String(value || '').replace(/(?:Bearer\s+\S+|anotify_session=[^\s;]+|["']?(?:password|token|hmac|secret|authorization|env|vars|params)["']?\s*[:=]\s*(?:"[^"\n]*"|'[^'\n]*'|[^\s,;]+))/gi, '[REDACTED]').slice(0, 700);
  const metadata = v => ({ types: fields(v, ['uid', 'name', 'namespace', 'resourceVersion', 'version']), ...Object.fromEntries(['uid', 'name', 'namespace', 'resourceVersion', 'version'].map(k => [k, scalar(v?.[k])])) });
  const task = v => ({ type: type(v), kind: scalar(v?.kind), metadata: metadata(v?.metadata), spec: { types: fields(v?.spec, ['traceUID', 'parentTaskId', 'capability', 'phase', 'attempt', 'maxAttempts', 'result']), traceUID: scalar(v?.spec?.traceUID), parentTaskId: scalar(v?.spec?.parentTaskId), capability: { types: fields(v?.spec?.capability, ['name', 'version']), version: scalar(v?.spec?.capability?.version) } }, status: { types: fields(v?.status, ['phase', 'attempt', 'maxAttempts', 'result', 'error']), phase: scalar(v?.status?.phase), resultTypes: fields(v?.status?.result, ['summary', 'error']), errorTypes: fields(v?.status?.result?.error, ['code', 'message']) }, facts: { types: fields(v?.facts, ['phase', 'result', 'error']), phase: scalar(v?.facts?.phase) } });
  const array = (v, select) => ({ type: type(v), count: Array.isArray(v) ? v.length : null, items: Array.isArray(v) ? v.map(select) : null });
  const shape = v => ({ type: type(v), kind: scalar(v?.kind), apiVersion: scalar(v?.apiVersion), traceUID: scalar(v?.traceUID), metadata: metadata(v?.metadata), types: fields(v, ['tasks', 'events', 'edges', 'pruned']), tasks: array(v?.tasks, task), events: array(v?.events, e => ({ type: type(e), kind: scalar(e?.kind), metadata: metadata(e?.metadata), specTypes: fields(e?.spec, ['traceUID', 'involvedObject', 'eventType', 'type', 'reason']), traceUID: scalar(e?.spec?.traceUID), involvedObject: { types: fields(e?.spec?.involvedObject, ['uid', 'name', 'kind']), uid: scalar(e?.spec?.involvedObject?.uid), kind: scalar(e?.spec?.involvedObject?.kind), name: e?.spec?.involvedObject?.kind === 'Capability' ? undefined : scalar(e?.spec?.involvedObject?.name) } })), edges: array(v?.edges, e => ({ types: fields(e, ['from', 'to', 'parentTaskId', 'taskId']), from: scalar(e?.from), to: scalar(e?.to), parentTaskId: scalar(e?.parentTaskId), taskId: scalar(e?.taskId) })), pruned: { type: type(v?.pruned), count: Array.isArray(v?.pruned) ? v.pruned.length : null } });
  const apiPath = (value, base) => {
    try {
      const url = new URL(value, base), match = /^\/v1\/namespaces\/([^/]+)\/traces\/([^/]+)$/.exec(url.pathname);
      if (!match || url.origin !== new URL(base).origin) return null;
      return { actualURL: url.origin + url.pathname, namespace: decodeURIComponent(match[1]), uid: decodeURIComponent(match[2]) };
    } catch { return null; }
  };
  const error = e => ({ name: scalar(e?.name) || type(e), message: clean(e?.message || (typeof e === 'string' ? e : '')), stack: clean(e?.stack || '').split('\n').slice(0, 7) });
  return { type, fields, task, shape, apiPath, clean, error };
}

export function installTraceEvidence(makeTools) {
  const T = makeTools();
  const tracePage = /^\/(?:en\/|es\/|ja\/)?trace\.html$/.test(location.pathname);
  const data = { documentId: String(performance.timeOrigin) + ':' + location.pathname, responses: [], events: [] };
  const state = () => {
    const url = new URL(location.href), query = new URLSearchParams();
    for (const key of ['namespace', 'traceUID']) if (url.searchParams.has(key)) query.set(key, url.searchParams.get(key));
    let currentNamespace;
    try { currentNamespace = window.Anotify?.readScope?.().namespace; } catch { /* observation only */ }
    return { documentId: data.documentId, atMs: performance.now(), timeOrigin: performance.timeOrigin, actualDocumentURL: url.origin + url.pathname + (query.size ? '?' + query : ''), readyState: document.readyState, documentLang: document.documentElement.lang, locale: navigator.language, currentNamespace, busy: !!document.querySelector('[aria-busy="true"]'), traceBusy: document.getElementById('trace-content')?.getAttribute('aria-busy') || null, contentMounted: !!document.getElementById('trace-content'), contentChildren: document.getElementById('trace-content')?.childElementCount ?? null, headerMounted: !!document.getElementById('trace-sub'), anchorCount: document.querySelectorAll('#trace-tasks').length, taskRows: document.querySelectorAll('#trace-tasks [data-task]').length, errorText: T.clean(document.getElementById('trace-error')?.textContent || ''), errorStateText: T.clean(document.getElementById('trace-error-state')?.textContent || document.getElementById('trace-state')?.textContent || '') };
  };
  const emit = (event, detail = {}) => {
    try { if (data.events.length < 200) data.events.push({ event, atMs: performance.now(), ...detail, state: state() }); }
    catch { /* Never substitute observer failure for the original call. */ }
  };
  const records = new WeakMap();
  window.__f3TraceResponse = (call, stage) => {
    try {
      const identity = T.apiPath(call.url, location.href);
      if (!identity) return;
      let record = records.get(call);
      if (!record) { record = { documentId: data.documentId, ...identity, method: call.method }; records.set(call, record); data.responses.push(record); }
      record.httpStatus = call.httpStatus;
      if (stage === 'parse-start') record.parseStartedAtMs = performance.now();
      if (stage === 'parsed') { record.parsedAtMs = performance.now(); record.response = T.shape(call.result); }
      if (stage === 'return') record.returnAtMs = performance.now();
      if (stage === 'throw') { record.throwAtMs = performance.now(); record.error = T.clean(call.error); }
    } catch { /* No response/Promise/exception changes on observation failure. */ }
  };
  window.__f3TraceSnapshot = () => tracePage || data.responses.length ? { documentId: data.documentId, responses: data.responses, events: data.events, snapshot: state() } : null;
  if (!tracePage) return;
  const wrapped = new WeakSet();
  const wrap = (owner, key, input) => {
    if (typeof owner?.[key] !== 'function' || wrapped.has(owner[key])) return;
    const original = owner[key];
    const observed = function (...args) {
      try { emit(key + ':input', input(args)); } catch { /* observation only */ }
      try { return Reflect.apply(original, this, args); }
      catch (error) {
        try { emit(key + ':throw-before-cleanup', { error: T.error(error) }); } catch { /* observation only */ }
        throw error;
      }
    };
    wrapped.add(observed); owner[key] = observed;
  };
  window.__f3TraceAttach = value => {
    wrap(value?.model, 'task', args => ({ input: T.task(args[0]) }));
    wrap(value?.model, 'event', args => ({ inputTypes: T.fields(args[0], ['kind', 'metadata', 'spec']) }));
    wrap(value, 'loadErrorNode', args => ({ error: T.error(args[0]) }));
  };
  // Driver may initialize before or after this init script; both orders attach
  // to the final export without replacing Anotify's transport accessor.
  if (window.Anotify) window.__f3TraceAttach(window.Anotify);
  const previous = Object.getOwnPropertyDescriptor(window, 'AnotifyGraphs');
  let graphs = previous?.get ? previous.get.call(window) : window.AnotifyGraphs;
  const attachGraphs = value => wrap(value, 'taskChain', args => ({ inputTypes: T.fields(args[1], ['tasks', 'sourceLabel', 'animate']), tasks: Array.isArray(args[1]?.tasks) ? args[1].tasks.map(T.task) : null }));
  Object.defineProperty(window, 'AnotifyGraphs', { configurable: true, get() { return previous?.get ? previous.get.call(this) : graphs; }, set(value) { if (previous?.set) previous.set.call(this, value); else graphs = value; attachGraphs(value); } });
  if (graphs) attachGraphs(graphs);
  window.addEventListener('error', e => emit('global-error', { error: T.error(e.error || e.message) }));
  window.addEventListener('unhandledrejection', e => emit('global-unhandledrejection', { error: T.error(e.reason) }));
  const ids = node => node.nodeType === 1 ? [node.id, ...Array.from(node.querySelectorAll('[id]'), e => e.id)].filter(id => /^trace-/.test(id)) : [];
  new MutationObserver(records => {
    const changes = records.map(r => ({ target: /^trace-/.test(r.target.id) ? r.target.id : r.target.nodeName, attribute: r.attributeName, added: Array.from(r.addedNodes).flatMap(ids), removed: Array.from(r.removedNodes).flatMap(ids) })).filter(r => /^trace-/.test(r.target) || r.added.length || r.removed.length);
    if (changes.length) emit('mutation-delivery', { changes });
  }).observe(document, { subtree: true, childList: true, attributes: true, attributeFilter: ['aria-busy'] });
  document.addEventListener('DOMContentLoaded', () => emit('DOMContentLoaded'));
  window.addEventListener('load', () => emit('load'));
}
