// Dependency-free behavioral tests for dashboard.js Task 11 operations:
// complete key creation payload (11 fields), compact/expandable key rows,
// provider actions (test/reset/enable-disable/edit), CSV Blob export,
// and DOM-safe rendering with no inline handlers.
// Run: node internal/server/web/dashboard_ops.test.js
"use strict";
const fs = require("fs");
const path = require("path");
const vm = require("vm");

/* ---------- minimal DOM stub (same shape as dashboard_settings.test.js) ---------- */
function makeEl(tag) {
  const el = {
    tagName: (tag || "div").toUpperCase(),
    children: [],
    attrs: {},
    dataset: {},
    style: {},
    classList: {
      _s: new Set(),
      add(c) { this._s.add(c); },
      remove(c) { this._s.delete(c); },
      toggle(c, force) {
        const want = force === undefined ? !this._s.has(c) : !!force;
        want ? this._s.add(c) : this._s.delete(c);
        return want;
      },
      contains(c) { return this._s.has(c); },
    },
    listeners: {},
    set innerHTML(v) { this._html = String(v); this.children = []; },
    get innerHTML() { return this._html || ""; },
    set textContent(v) { this._text = String(v); },
    get textContent() {
      if (this._text !== undefined) return this._text;
      return this.children.map(c => c.textContent).join("");
    },
    setAttribute(k, v) { this.attrs[k] = String(v); if (k.startsWith("data-")) this.dataset[camel(k.slice(5))] = String(v); },
    getAttribute(k) { return this.attrs[k] ?? null; },
    addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); },
    dispatch(t, ev) {
      ev = ev || {};
      ev.target = ev.target || this;
      ev.stopPropagation = ev.stopPropagation || (() => { ev._stopped = true; });
      (this.listeners[t] || []).slice().forEach(fn => fn(ev));
    },
    appendChild(c) { this.children.push(c); c.parentElement = this; return c; },
    remove() { const p = this.parentElement; if (p) p.children = p.children.filter(c => c !== this); },
    closest(sel) {
      let n = this;
      const attr = sel.match(/^\[(.+)\]$/);
      while (n) {
        if (attr && n.dataset && n.dataset[camel(attr[1].slice(5))] !== undefined) return n;
        n = n.parentElement || null;
      }
      return null;
    },
    querySelector() { return makeEl("div"); },
    querySelectorAll() { return []; },
    click() { if (typeof this.onclick === "function") this.onclick({ target: this }); this.dispatch("click", { target: this }); },
    focus() {},
    disabled: false,
    value: "",
    checked: false,
    hidden: false,
  };
  Object.defineProperty(el, "className", {
    set(v) { el.classList._s = new Set(String(v).split(/\s+/).filter(Boolean)); },
    get() { return [...el.classList._s].join(" "); },
  });
  return el;
}
function camel(s) { return s.replace(/-([a-z])/g, (_, c) => c.toUpperCase()); }

const ids = {};
function $(id) {
  if (!ids[id]) {
    ids[id] = makeEl("div");
    ids[id].id = id;
    const tbody = makeEl("tbody");
    ids[id].querySelector = () => tbody;
  }
  return ids[id];
}

/* network stub: records every fetch; responses queue via respond() */
const calls = [];
let responder = async () => ({ status: 200, ok: true, json: async () => ({}), blob: async () => ({}) });
function respond(fn) { responder = fn; }

const documentStub = {
  getElementById: id => $(id),
  createElement: tag => {
    const el = makeEl(tag);
    el.id = "";
    if (tag === "div") {
      let txt = "";
      Object.defineProperty(el, "textContent", {
        set(v) { txt = String(v); },
        get() { return txt; },
      });
      Object.defineProperty(el, "innerHTML", {
        set(v) { el._html = String(v); },
        get() { return txt.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;"); },
      });
    }
    return el;
  },
  querySelector: () => null,
  querySelectorAll: () => [],
  addEventListener(t, fn) { (this._ls = this._ls || {})[t] = (this._ls[t] || []).concat(fn); },
  body: makeEl("body"),
};

const sandbox = {
  document: documentStub,
  window: { matchMedia: () => ({ matches: false }), addEventListener: () => {} },
  sessionStorage: { _v: "test-admin-secret", getItem(k) { return k === "adminKey" ? this._v : null; }, setItem(k, v) { this._v = v; } },
  location: { search: "", hash: "" },
  history: { replaceState: () => {} },
  fetch: async (url, opts) => { calls.push({ url, opts }); return responder(url, opts); },
  performance: { now: () => 0 },
  requestAnimationFrame: () => 0,
  cancelAnimationFrame: () => {},
  setInterval: () => 0,
  setTimeout: fn => 0,
  navigator: {},
  URL: { createObjectURL: () => "blob:fake", revokeObjectURL: () => {} },
  URLSearchParams,
  confirm: () => true,
  console,
};
sandbox.window.document = documentStub;
vm.createContext(sandbox);

const js = fs.readFileSync(path.join(__dirname, "dashboard.js"), "utf8");
vm.runInContext(js, sandbox, { filename: "dashboard.js" });

/* ---------- tiny assert harness ---------- */
let pass = 0, fail = 0;
const tests = [];
function t(name, fn) { tests.push([name, fn]); }
async function run() {
  for (const [name, fn] of tests) {
    try { await fn(); pass++; console.log("ok - " + name); }
    catch (e) { fail++; console.log("FAIL - " + name + "\n    " + (e && e.stack || e)); }
  }
  console.log("\n" + pass + " passed, " + fail + " failed");
  process.exit(fail ? 1 : 0);
}
function eq(got, want, msg) {
  if (got !== want) {
    const e = new Error((msg || "eq") + ": got " + JSON.stringify(got) + " want " + JSON.stringify(want));
    Error.captureStackTrace(e, eq);
    throw e;
  }
}
function ok(v, msg) {
  if (!v) { const e = new Error(msg || "expected truthy"); Error.captureStackTrace(e, ok); throw e; }
}
function includes(hay, needle, msg) {
  if (!String(hay).includes(needle)) throw new Error((msg || "includes") + ": " + JSON.stringify(needle) + " not in output");
}
function notIncludes(hay, needle, msg) {
  if (String(hay).includes(needle)) throw new Error((msg || "notIncludes") + ": found " + JSON.stringify(needle));
}

const ctx = expr => vm.runInContext(expr, sandbox);

/* ---------- tests ---------- */

t("create key sends all eleven fields, arrays parsed, expires RFC3339", async () => {
  calls.length = 0;
  $("f-name").value = "ci-bot";
  $("f-rpm").value = "60"; $("f-tpm").value = "1000";
  $("f-model-rpm").value = "7"; $("f-limit-header").value = " X-Team ";
  $("f-daily-quota").value = "5000"; $("f-quota").value = "9000";
  $("f-budget").value = "12.50";
  $("f-models").value = " auto , gpt-4o ,"; $("f-groups").value = "ci, batch";
  $("f-expires").value = "2027-01-02T03:04";
  respond(async () => ({ status: 201, ok: true, json: async () => ({ key: "gw-ci-abc" }) }));
  await ctx('$("create").onclick')();
  const post = calls.find(c => c.url === "/admin/keys");
  ok(post, "POST /admin/keys issued");
  eq(post.opts.method, "POST");
  eq(post.opts.headers["X-Admin-Key"], "test-admin-secret", "admin header sent");
  const body = JSON.parse(post.opts.body);
  eq(body.name, "ci-bot");
  eq(body.rpm, 60); eq(body.tpm, 1000); eq(body.model_rpm, 7);
  eq(body.limit_by_header, "X-Team", "header trimmed");
  eq(body.daily_quota, 5000); eq(body.quota_tokens, 9000);
  eq(body.budget_usd, 12.5);
  eq(JSON.stringify(body.allowed_models), JSON.stringify(["auto", "gpt-4o"]), "csv trimmed+filtered");
  eq(JSON.stringify(body.groups), JSON.stringify(["ci", "batch"]));
  ok(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$/.test(body.expires_at), "expires_at RFC3339: " + body.expires_at);
  eq($("newkey-val").textContent, "gw-ci-abc", "raw key shown once after create");
});

t("key rows are compact, escaped, expandable, no raw key, no inline handlers", async () => {
  calls.length = 0;
  const evil = '<img src=x onerror=alert(1)>';
  respond(async () => ({ status: 200, ok: true, json: async () => ({ keys: [{
    id: 3, key: "gw-ci-...", name: evil, rpm: 60, tpm: 0, model_rpm: 5,
    limit_by_header: "X-T", daily_quota: 100, daily_used: 2,
    quota_tokens: 1000, spent_tokens: 42, budget_usd: 5, spent_usd: 0.5,
    allowed_models: ["auto"], groups: ["ci"], expires_at: "2027-01-02T03:04:00Z",
    enabled: true, created_at: "2026-09-01T00:00:00Z",
  }] }) }));
  await ctx("loadKeys(true)");
  const html = $("keys").querySelector("tbody").innerHTML;
  includes(html, "&lt;img", "name HTML-escaped");
  /* the escaped name keeps its text but cannot execute: "<img" became "&lt;img",
     so the onerror attribute text is inert inside an entity-escaped label */
  const nameCell = html.split("</td>")[1];
  notIncludes(nameCell, "<img", "name cell has no live tag");
  notIncludes(html, " onclick=", "no inline onclick attribute");
  notIncludes(html, " onchange=", "no inline onchange attribute");
  includes(html, "key-details", "expandable details row present");
  includes(html, 'data-action="key-expand"', "delegated expand control");
  includes(html, "data-action='key-delete'", "delegated delete control");
  includes(html, "data-action='key-toggle'", "delegated toggle control");
  includes(html, "Model RPM", "details list all limits");
  includes(html, "Allowed models", "details list scopes");
  notIncludes(html, "gw-ci-full-secret", "no raw key in list");
});

t("attribute-context XSS: apostrophe/quote payload cannot break single-quoted attrs", async () => {
  calls.length = 0;
  /* payload designed to close a single-quoted attribute and inject a handler */
  const evil = "' autofocus onfocus='alert(1)";
  respond(async () => ({ status: 200, ok: true, json: async () => ({ keys: [{
    id: 9, key: "gw-ev-...", name: evil, rpm: 0, tpm: 0, enabled: true,
  }], providers: [{
    name: evil, priority: 1, circuit: "closed", disabled: false, ema_latency_ms: 1,
  }] }) }));
  /* loadKeys + loadProviders share the api() stub; serve both from one responder */
  await ctx("loadKeys(true)");
  const khtml = $("keys").querySelector("tbody").innerHTML;
  /* no raw apostrophe-delimited breakout: every user value inside a single-quoted
     attribute must have its apostrophes entity-encoded */
  notIncludes(khtml, "aria-label='toggle key '", "aria-label breakable by raw apostrophe");
  /* raw payload may legitimately appear in text cells (apostrophes are safe in
     text content); it must NOT appear inside an attribute value. Check the two
     attribute carriers: aria-label and data-name/data-id */
  const attrs = khtml.match(/(?:aria-label|data-name|data-id|data-details-for)='[^']*'/g) || [];
  for (const a of attrs) notIncludes(a, "' ", "attribute closed early by raw apostrophe: " + a);
  /* accessible label still carries the (encoded) name for round-trip */
  includes(khtml, "toggle key ", "aria-label still names the key");
  /* dataset round-trip: the name we read back must equal the original string */
  const m = khtml.match(/data-name='([^']*)'/);
  ok(m, "delete button data-name present");
  const decoded = m[1].replace(/&#39;/g, "'").replace(/&quot;/g, '"')
    .replace(/&gt;/g, ">").replace(/&lt;/g, "<").replace(/&amp;/g, "&");
  eq(decoded, evil, "dataset round-trips exact original name");
  /* key id is server-derived too; must be attribute-encoded (no raw quote breakout) */
  const badId = "' onmouseover='alert(1)";
  respond(async () => ({ status: 200, ok: true, json: async () => ({ keys: [{
    id: badId, key: "gw-x-...", name: "n", rpm: 0, tpm: 0, enabled: false,
  }] }) }));
  await ctx("loadKeys(true)");
  const khtml2 = $("keys").querySelector("tbody").innerHTML;
  notIncludes(khtml2, "data-id='" + badId, "raw id breaks data-id attribute");
  /* providers: same payload in single-quoted data-name */
  respond(async () => ({ status: 200, ok: true, json: async () => ({ providers: [{
    name: evil, priority: 1, circuit: "closed", disabled: false, ema_latency_ms: 1,
  }] }) }));
  await ctx("loadProviders()");
  const phtml = $("providers").querySelector("tbody").innerHTML;
  const pattrs = phtml.match(/data-name='[^']*'/g) || [];
  ok(pattrs.length >= 4, "provider action buttons carry data-name");
  for (const a of pattrs) notIncludes(a, "' ", "provider data-name closed early");
  const pm = phtml.match(/data-name='([^']*)'/);
  ok(pm, "provider data-name present");
  const pdecoded = pm[1].replace(/&#39;/g, "'").replace(/&quot;/g, '"')
    .replace(/&gt;/g, ">").replace(/&lt;/g, "<").replace(/&amp;/g, "&");
  eq(pdecoded, evil, "provider dataset round-trips exact original name");
});

t("key expand toggles details row via delegation", async () => {
  const btn = makeEl("button");
  btn.dataset.action = "key-expand";
  btn.dataset.id = "3";
  const row = { hidden: true };
  documentStub.querySelector = () => row;
  const fns = documentStub._ls.click;
  ok(fns && fns.length, "document click delegation registered");
  fns.forEach(fn => fn({ target: btn }));
  eq(row.hidden, false, "details row unhidden");
  documentStub.querySelector = () => null;
});

t("provider rows show disabled/balance_low pills and four delegated actions", async () => {
  calls.length = 0;
  respond(async () => ({ status: 200, ok: true, json: async () => ({ providers: [{
    name: "up1", priority: 1, circuit: "closed", disabled: true,
    balance_low: true, ema_latency_ms: 12,
  }] }) }));
  await ctx("loadProviders()");
  const html = $("providers").querySelector("tbody").innerHTML;
  includes(html, "closed", "circuit pill");
  includes(html, "disabled", "disabled pill independent of circuit");
  includes(html, "balance low", "balance_low pill");
  for (const a of ["prov-test", "prov-reset", "prov-toggle", "prov-edit"]) {
    includes(html, "data-action='" + a + "'", a + " present");
  }
  includes(html, ">Enable<", "disabled provider offers Enable");
  notIncludes(html, "onclick=", "no inline handlers");
});

t("provider disable/enable hit header-auth POST routes", async () => {
  calls.length = 0;
  respond(async () => ({ status: 200, ok: true, json: async () => ({}) }));
  await ctx('toggleProvider("up1", false)'); // currently enabled -> disable
  let c = calls.find(x => x.url.includes("/disable"));
  ok(c, "disable route called");
  eq(c.url, "/admin/providers/up1/disable");
  eq(c.opts.method, "POST");
  eq(c.opts.headers["X-Admin-Key"], "test-admin-secret", "header auth (not ?key=)");
  notIncludes(c.url, "key=", "no query key");
  calls.length = 0;
  await ctx('toggleProvider("up1", true)'); // currently disabled -> enable
  c = calls.find(x => x.url.includes("/enable"));
  ok(c, "enable route called");
  eq(c.url, "/admin/providers/up1/enable");
});

t("CSV export fetches as Blob with admin header, no navigation", async () => {
  calls.length = 0;
  respond(async () => ({ status: 200, ok: true, blob: async () => ({ type: "text/csv" }) }));
  await ctx('$("export-csv").onclick')();
  const c = calls.find(x => x.url.includes("/admin/usage/export"));
  ok(c, "export fetch issued");
  eq(c.url, "/admin/usage/export?format=csv");
  eq(c.opts.headers["X-Admin-Key"], "test-admin-secret", "header preserved via fetch");
});

run();
