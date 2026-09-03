// Dependency-free behavioral tests for dashboard.js settings renderer.
// Loads dashboard.js under a minimal DOM stub and executes the real functions.
// Run: node internal/server/web/dashboard_settings.test.js
"use strict";
const fs = require("fs");
const path = require("path");
const vm = require("vm");

/* ---------- minimal DOM stub ---------- */
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
      const m = sel.match(/^\.(.+)$/);
      while (n) { if (m && n.classList && n.classList.contains(m[1])) return n; n = n.parentElement || null; }
      return null;
    },
    querySelector(sel) {
      if (sel[0] === "#") {
        const want = sel.slice(1);
        const walk = n => {
          for (const c of n.children || []) { if (c.id === want) return c; const r = walk(c); if (r) return r; }
          return null;
        };
        return walk(this);
      }
      return makeEl("div");
    },
    querySelectorAll() { return []; },
    click() { if (typeof this.onclick === "function") this.onclick({ target: this }); this.dispatch("click", { target: this }); },
    focus() {},
    disabled: false,
    value: "",
    hidden: false,
  };
  /* className property mirrors classList, as in a real DOM element */
  Object.defineProperty(el, "className", {
    set(v) { el.classList._s = new Set(String(v).split(/\s+/).filter(Boolean)); },
    get() { return [...el.classList._s].join(" "); },
  });
  return el;
}
function camel(s) { return s.replace(/-([a-z])/g, (_, c) => c.toUpperCase()); }

const ids = {};
function $(id) {
  if (!ids[id]) { ids[id] = makeEl("div"); ids[id].id = id; }
  return ids[id];
}

// elements registered for querySelectorAll by selector
const registry = { "#subnav button": [], ".flist-card": [] };

const documentStub = {
  getElementById: id => {
    /* live elements (appended to settings-body etc.) win, as in a real document */
    const walk = n => {
      for (const c of n.children || []) { if (c.id === id) return c; const r = walk(c); if (r) return r; }
      return null;
    };
    return walk($("settings-body")) || $(id);
  },
  createElement: tag => {
    const el = makeEl(tag);
    el.id = "";
    if (tag === "div") {
      // emulate textContent -> innerHTML escaping used by esc()
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
  querySelectorAll: sel => registry[sel] || [],
  addEventListener() {},
  querySelector: () => null,
  body: makeEl("body"),
};

const sandbox = {
  document: documentStub,
  window: { matchMedia: () => ({ matches: false }), addEventListener: () => {} },
  sessionStorage: { getItem: () => null, setItem: () => {} },
  location: { search: "", hash: "" },
  history: { replaceState: () => {} },
  fetch: async () => { throw new Error("no network in tests"); },
  performance: { now: () => 0 },
  requestAnimationFrame: () => 0,
  cancelAnimationFrame: () => {},
  setInterval: () => 0,
  setTimeout: () => 0,
  navigator: {},
  URLSearchParams,
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
  if (!v) {
    const e = new Error(msg || "expected truthy");
    Error.captureStackTrace(e, ok);
    throw e;
  }
}
function includes(hay, needle, msg) {
  if (!String(hay).includes(needle)) throw new Error((msg || "includes") + ": " + JSON.stringify(needle) + " not in output");
}
function notIncludes(hay, needle, msg) {
  if (String(hay).includes(needle)) throw new Error((msg || "notIncludes") + ": found " + JSON.stringify(needle));
}

// top-level const/let don't attach to the vm global; evaluate inside the context
const ctx = expr => vm.runInContext(expr, sandbox);
const S = new Proxy({}, { get: (_, k) => vm.runInContext(String(k), sandbox) });

/* seed a config with a list of routes + strategy-conditional fields */
function seed() {
  S.configState.base = {
    routes: [
      { virtual_model: "a", strategy: "round_robin", hash_keys: ["x"] },
      { virtual_model: "b", strategy: "consistent_hash", hash_keys: ["y"] },
    ],
    model_prices: { "gpt-4": { in: 1 } },
  };
  S.configState.draft = JSON.parse(JSON.stringify(S.configState.base));
  S.configState.dirtyPaths = new Set();
  S.configState.schema = {
    kind: "group", name: "", path: "", children: [
      { kind: "list", name: "routes", path: "routes", label: "Routes", section: "routes",
        item: { kind: "group", name: "", label: "Route", identity: ["virtual_model"], children: [
          { kind: "string", name: "virtual_model", path: "routes[].virtual_model", label: "Model" },
          { kind: "string", name: "strategy", path: "routes[].strategy", label: "Strategy",
            enum: ["round_robin", "consistent_hash", "fusion_judge"] },
          { kind: "list", name: "hash_keys", path: "routes[].hash_keys", label: "Hash keys",
            visible_when: { path: "routes[].strategy", values: ["consistent_hash"] },
            item: { kind: "string", label: "Key" } },
          { kind: "string", name: "judge", path: "routes[].judge", label: "Judge",
            visible_when: { path: "routes[].strategy", values: ["fusion_judge"] } },
          { kind: "number", name: "multiplier", path: "routes[].multiplier", label: "Multiplier", advanced: true },
        ] } },
      { kind: "map", name: "model_prices", path: "model_prices", label: "Prices", section: "pricing",
        children: [{ kind: "group", name: "", label: "Price", children: [
          { kind: "number", name: "in", path: "model_prices.*.in", label: "In" },
        ] }] },
    ],
  };
}

/* ---------- 1. VisibleWhen wildcard resolves per-item ---------- */
t("visible_when [] resolves against current item index, not [0]", () => {
  seed();
  // route 1 has strategy consistent_hash -> hash_keys visible there
  const route1 = S.configState.schema.children[0].item.children;
  const hashKeys = route1.find(c => c.name === "hash_keys");
  const out1 = S.renderField(hashKeys, ["y"], "routes[1].hash_keys");
  ok(out1.length > 0, "hash_keys should render under routes[1] (consistent_hash)");
  // route 0 is round_robin -> hash_keys hidden there even though routes[1] is consistent_hash
  const out0 = S.renderField(hashKeys, ["x"], "routes[0].hash_keys");
  eq(out0, "", "hash_keys must hide under routes[0] (round_robin)");
});

t("visible_when non-wildcard path still works", () => {
  seed();
  const judge = S.configState.schema.children[0].item.children.find(c => c.name === "judge");
  eq(S.renderField(judge, "j", "routes[1].judge"), "", "judge hidden for consistent_hash");
  S.configState.draft.routes[1].strategy = "fusion_judge";
  ok(S.renderField(judge, "j", "routes[1].judge").length > 0, "judge visible for fusion_judge");
});

t("secret editor states env-reference-only replacement and flags literals", () => {
  seed();
  const schema = { kind: "string", name: "api_key", path: "providers[].api_key", label: "API key", secret: true };
  const html = S.renderField(schema, "__TOKENROUTE_KEEP_SECRET__", "providers[0].api_key");
  includes(html, "Environment reference", "secret guidance");
  includes(html, 'pattern="\\$\\{[A-Za-z_][A-Za-z0-9_]*\\}"', "env-reference pattern");
  includes(html, "__TOKENROUTE_KEEP_SECRET__", "sentinel remains editable as keep marker");
  ok(!S.validSecretReplacement("literal-secret"), "literal replacement rejected client-side");
  ok(S.validSecretReplacement("${P1_KEY}"), "environment reference accepted");
  ok(S.validSecretReplacement(""), "removal accepted");
  ok(S.validSecretReplacement("__TOKENROUTE_KEEP_SECRET__"), "keep sentinel accepted");
});

t("toolbar actions enabled only for concrete item selection", () => {
  seed();
  S.settingsSelect("routes");
  S.updateSettingsToolbar();
  ok($("settings-add").disabled, "add disabled for bare collection path in new model");
  ok($("settings-delete").disabled, "delete disabled without item");
  S.settingsSelect("routes[1]");
  S.updateSettingsToolbar();
  ok(!$("settings-add").disabled, "add enabled with item selected");
  ok(!$("settings-duplicate").disabled, "duplicate enabled");
  ok(!$("settings-delete").disabled, "delete enabled");
  ok(!$("settings-move-up").disabled, "move-up enabled for middle/last item");
  ok($("settings-move-down").disabled, "move-down disabled at end of list");
  S.settingsSelect("routes[0]");
  S.updateSettingsToolbar();
  ok($("settings-move-up").disabled, "move-up disabled at start of list");
  ok(!$("settings-move-down").disabled, "move-down enabled for first item");
  ok(!$("settings-add").disabled && $("settings-add").getAttribute("aria-disabled") !== "true",
     "add not aria-disabled");
});

t("toolbar duplicate acts on selected item path", () => {
  seed();
  S.settingsSelect("routes[0]");
  $("settings-duplicate").click();
  eq(S.configState.draft.routes.length, 3, "duplicate adds a route");
  eq(S.configState.draft.routes[1].virtual_model, "a", "duplicate inserted after index 0");
});

t("toolbar delete acts on selected item path", () => {
  seed();
  S.settingsSelect("routes[1]");
  $("settings-delete").click();
  eq(S.configState.draft.routes.length, 1, "delete removes a route");
  eq(S.configState.draft.routes[0].virtual_model, "a", "deleted the right one");
});

t("toolbar move up/down act on selected item", () => {
  seed();
  S.settingsSelect("routes[1]");
  $("settings-move-up").click();
  eq(S.configState.draft.routes[0].virtual_model, "b", "moved up");
});

/* ---------- 3. no inline handlers / XSS safety in rendered output ---------- */
t("rendered settings HTML contains no inline event handlers", () => {
  seed();
  const h = S.renderList(S.configState.schema.children[0], S.configState.draft.routes, "routes");
  notIncludes(h, "onclick", "list html");
  notIncludes(h, "onchange", "list html");
  notIncludes(h, "onkey", "list html");
});

t("map key with quote/script payload is safe in output and rename path", () => {
  seed();
  const evil = 'a"><img src=x onerror=alert(1)>';
  S.configState.draft.model_prices = { [evil]: { in: 2 } };
  const mapSchema = S.configState.schema.children[1];
  const h = S.renderMap(mapSchema, S.configState.draft.model_prices, "model_prices");
  notIncludes(h, "<img", "map html must not carry injected tag");
  notIncludes(h, '"a"><img', "raw breakout sequence must not survive");
  includes(h, "&lt;img", "payload tag must be entity-escaped");
  notIncludes(h, "onclick", "no inline handlers");
  notIncludes(h, "onchange", "no inline handlers");
});

t("map key with single quote does not break attribute markup", () => {
  seed();
  const evil = "it's\"a";
  S.configState.draft.model_prices = { [evil]: { in: 3 } };
  const h = S.renderMap(S.configState.schema.children[1], S.configState.draft.model_prices, "model_prices");
  // key appears only escaped; markup still parses (no dangling quote breakout)
  notIncludes(h, "it's\"a", "raw key must not appear unescaped");
  includes(h, "data-key", "rename/delete wired via dataset");
});

t("map keys containing dots remain one path segment", () => {
  seed();
  S.configState.draft.model_prices = { "gpt-4.1": { in: 3 } };
  S.setDraftValue("model_prices.gpt-4.1.in", 7);
  eq(S.configState.draft.model_prices["gpt-4.1"].in, 7, "dotted key value updated");
  eq(S.configState.draft.model_prices["gpt-4"], undefined, "no accidental nested map created");
  eq(S.getDraftValue("model_prices.gpt-4.1.in"), 7, "dotted key value resolves");
});

t("search keeps a parent when a descendant schema matches", () => {
  seed();
  ctx('settingsFilter = "hash keys"');
  const routes = S.configState.schema.children[0];
  const h = S.renderField(routes, S.configState.draft.routes, "routes");
  includes(h, "Hash keys", "matching descendant remains rendered");
  ctx('settingsFilter = ""');
});

t("advanced section reaches nested advanced fields only", () => {
  seed();
  S.configState.draft.routes[0].multiplier = 2;
  S.renderSettingsSection("advanced");
  includes($("settings-body").innerHTML, "Multiplier", "nested advanced field rendered");
  notIncludes($("settings-body").innerHTML, ">Strategy<", "common nested field excluded from advanced view");
});

t("struct-valued map renders its value fields", () => {
  seed();
  ctx('settingsSub = "pricing"');
  const mapSchema = { kind: "map", name: "prices", path: "prices", label: "Prices", section: "pricing",
    children: [{ kind: "number", name: "prompt_per_1m", path: "prices.*.prompt_per_1m", label: "Prompt per 1m" }] };
  const h = S.renderMap(mapSchema, { "gpt-4": { prompt_per_1m: 2 }, prompt_per_1m: { prompt_per_1m: 3 } }, "prices");
  includes(h, 'type="number"', "map value uses reflected number editor");
  includes(h, "Prompt per 1m", "map value child label rendered");
  S.configState.schema.children.push(mapSchema);
  S.configState.draft.prices = {};
  S.settingsMapAdd("prices");
  eq(typeof S.configState.draft.prices.new_key, "object", "struct map adds an object value");
  S.setDraftValue("prices.new_key.prompt_per_1m", 9);
  eq(S.configState.draft.prices.new_key.prompt_per_1m, 9, "new struct map value remains editable");
});

t("adding scalar-list item uses scalar default", () => {
  seed();
  S.settingsListAdd("routes[0].hash_keys");
  eq(S.configState.draft.routes[0].hash_keys[1], "", "scalar list adds an empty string, not an object");
});

/* ---------- 4. dirty marking ---------- */
t("setDraftValue marks exact path dirty and renders .frow.dirty", () => {
  seed();
  ctx('settingsSub = "routes"');
  S.setDraftValue("routes[1].virtual_model", "b2");
  ok(S.configState.dirtyPaths.has("routes[1].virtual_model"), "dirty path recorded");
  eq(S.configState.draft.routes[1].virtual_model, "b2", "value set");
  const fld = S.configState.schema.children[0].item.children[0];
  const h = S.renderField(fld, "b2", "routes[1].virtual_model");
  includes(h, 'class="frow dirty"', "dirty class on the frow itself");
});

/* ---------- 5. list item selection renders accessibly ---------- */
t("list cards are keyboard-selectable with aria-selected", () => {
  seed();
  S.settingsSelect("routes[1]");
  const h = S.renderList(S.configState.schema.children[0], S.configState.draft.routes, "routes");
  includes(h, 'role="button"', "card has button role");
  includes(h, 'tabindex="0"', "card focusable");
  includes(h, 'aria-selected="true"', "selected card aria-selected");
});

/* ---------- 6. Task 10: validate/diff/save/conflict state machine ---------- */

/* install a scripted fetch: [{path, method, status, json}] consumed in order */
let fetchLog;
function mockFetch(script) {
  fetchLog = [];
  const queue = script.slice();
  sandbox.fetch = async (path, opts) => {
    fetchLog.push({ path, opts });
    const next = queue.shift();
    if (!next) throw new Error("unexpected fetch " + path);
    const body = next.json !== undefined ? JSON.stringify(next.json) : "";
    return {
      ok: next.status >= 200 && next.status < 300,
      status: next.status,
      json: async () => (next.json !== undefined ? next.json : null),
      text: async () => body,
    };
  };
}

/* dialog stub: native <dialog> API surface */
function dialogStub() {
  const el = $("config-diff-dialog");
  el.open = false;
  el.showModal = () => { el.open = true; };
  el.close = () => { el.open = false; el.dispatch("close", {}); };
  return el;
}

function seedWorkflow() {
  seed();
  S.configState.revision = "sha256:base";
  S.configState.rawDraft = "routes: []\n";
  S.configState.validated = null;
  S.configState.saveState = "dirty";
  dialogStub();
  $("settings-raw").value = "";
}

t("validateConfig posts document with expected_revision and stores exact candidate", async () => {
  seedWorkflow();
  S.configState.draft.routes.push({ virtual_model: "c", strategy: "round_robin" });
  S.configState.dirtyPaths.add("routes");
  const cand = {
    valid: true,
    base_revision: "sha256:base",
    candidate_revision: "sha256:cand1",
    document: { routes: [{ virtual_model: "a" }, { virtual_model: "b" }, { virtual_model: "c" }] },
    raw_yaml: "routes:\n  - virtual_model: c\n",
    diff: [{ path: "routes[2]", kind: "add", after: "c" }],
    changed_paths: ["routes[2]"],
    restart_required_paths: [],
    warnings: [],
  };
  mockFetch([{ status: 200, json: cand }]);
  await S.validateConfig();
  eq(fetchLog[0].path, "/admin/config/validate");
  eq(fetchLog[0].opts.method, "POST");
  const sent = JSON.parse(fetchLog[0].opts.body);
  eq(sent.expected_revision, "sha256:base", "expected_revision sent");
  eq(sent.mode, "structured");
  ok(sent.document, "document sent in structured mode");
  eq(S.configState.validated && S.configState.validated.candidate_revision, "sha256:cand1",
     "exact validated candidate bound");
  eq(S.configState.saveState, "review");
  ok($("config-diff-dialog").open, "diff dialog opened");
  includes($("config-diff-body").innerHTML, "routes[2]", "diff rendered");
});

t("any draft mutation after validation clears the bound candidate", async () => {
  seedWorkflow();
  S.configState.validated = { candidate_revision: "sha256:cand1", document: {}, raw_yaml: "" };
  S.configState.validatedRequest = { expected_revision: "sha256:base", mode: "structured", document: {} };
  S.configState.saveState = "review";
  S.setDraftValue("routes[0].virtual_model", "changed");
  eq(S.configState.validated, null, "candidate cleared on mutation");
  eq(S.configState.saveState, "dirty");
});

t("confirmConfigSave PUTs only the exact validated candidate", async () => {
  seedWorkflow();
  const validated = {
    base_revision: "sha256:base",
    candidate_revision: "sha256:cand1",
    document: { routes: [{ virtual_model: "z" }] },
    raw_yaml: "routes:\n  - virtual_model: z\n",
  };
  S.configState.validated = validated;
  S.configState.validatedRequest = { expected_revision: "sha256:base", mode: "structured", document: validated.document };
  S.configState.saveState = "review";
  mockFetch([{ status: 200, json: { saved: true, applied: true, revision: "sha256:cand1",
    restart_required: false, restart_required_paths: [] } }]);
  await S.confirmConfigSave();
  eq(fetchLog[0].path, "/admin/config");
  eq(fetchLog[0].opts.method, "PUT");
  const sent = JSON.parse(fetchLog[0].opts.body);
  eq(sent.expected_revision, "sha256:base");
  eq(sent.candidate_revision, "sha256:cand1");
  eq(sent.mode, "structured");
  eq(JSON.stringify(sent.document), JSON.stringify(validated.document),
     "PUT body carries the validated document, not the draft");
  eq(S.configState.saveState, "saved");
  eq(S.configState.validated, null, "candidate consumed after save");
});

t("raw validation then confirm PUT preserves exact raw request including comments", async () => {
  seedWorkflow();
  S.configState.mode = "raw";
  const raw = "# comment-only raw edit\nroutes: []\n";
  $("settings-raw").value = raw;
  S.configState.dirtyPaths.add("raw");
  const cand = {
    valid: true,
    base_revision: "sha256:base",
    candidate_revision: "sha256:raw-candidate",
    document: { routes: [] },
    raw_yaml: raw,
    diff: [],
    changed_paths: [],
    restart_required_paths: [],
    warnings: [],
  };
  mockFetch([
    { status: 200, json: cand },
    { status: 200, json: { saved: true, applied: false, revision: "sha256:raw-candidate",
      restart_required: false, restart_required_paths: [] } },
  ]);

  await S.validateConfig();
  const validateBody = JSON.parse(fetchLog[0].opts.body);
  eq(validateBody.mode, "raw");
  eq(validateBody.raw_yaml, raw, "validation carries exact raw buffer");
  await S.confirmConfigSave();

  const saveBody = JSON.parse(fetchLog[1].opts.body);
  eq(saveBody.expected_revision, "sha256:base");
  eq(saveBody.candidate_revision, "sha256:raw-candidate");
  eq(saveBody.mode, "raw", "save preserves validated request mode");
  eq(saveBody.raw_yaml, raw, "save preserves exact validated raw payload");
  ok(!Object.prototype.hasOwnProperty.call(saveBody, "document"), "raw save omits structured document");
});

t("409 keeps the draft and exposes reload-and-compare, never auto-retries", async () => {
  seedWorkflow();
  S.configState.draft.routes.push({ virtual_model: "keepme" });
  S.configState.dirtyPaths.add("routes");
  S.configState.validated = { candidate_revision: "sha256:cand1", document: {}, raw_yaml: "" };
  S.configState.validatedRequest = { expected_revision: "sha256:base", mode: "structured", document: {} };
  S.configState.saveState = "review";
  mockFetch([{ status: 409, json: { error: { message: "config conflict", type: "config_conflict" } } }]);
  await S.confirmConfigSave();
  eq(fetchLog.length, 1, "exactly one PUT attempt, no retry");
  eq(S.configState.saveState, "conflict");
  eq(S.configState.draft.routes.length, 3, "draft survives conflict");
  ok(S.configState.dirtyPaths.has("routes"), "dirty state preserved");
  includes($("settings-status").innerHTML.toLowerCase(), "reload", "reload-and-compare offered");
  eq(S.configState.validated, null, "stale candidate dropped");
});

t("422 surfaces field errors and focuses the first invalid editor", async () => {
  seedWorkflow();
  S.configState.dirtyPaths.add("routes");
  /* mount a rendered field row as live DOM, as renderSettingsSection produces */
  const body = $("settings-body");
  body.children = [];
  const frow = documentStub.createElement("div");
  frow.classList.add("frow");
  const editor = documentStub.createElement("input");
  editor.id = "f-routes-0--virtual-model";
  let focused = null;
  editor.focus = () => { focused = editor.id; };
  frow.appendChild(editor);
  body.appendChild(frow);
  mockFetch([{ status: 422, json: { valid: false, errors: [
    { path: "routes[0].virtual_model", code: "required", message: "Model is <required> & \"quoted\"." },
  ] } }]);
  const bodyHTMLBefore = body.innerHTML;
  await S.validateConfig();
  eq(S.configState.saveState, "invalid");
  eq(body.innerHTML, bodyHTMLBefore, "structured field errors must not rewrite settings-body HTML");
  ok(frow.classList.contains("invalid"), "owning .frow flagged invalid");
  const alert = frow.children.find(c => c.classList.contains("field-error"));
  ok(alert, "field-error node appended to the owning .frow");
  eq(alert.getAttribute("role"), "alert", "role=alert on the error node");
  eq(alert.textContent, "Model is <required> & \"quoted\".",
     "error text set via textContent, never HTML");
  eq(focused, "f-routes-0--virtual-model", "first invalid editor focused");
});

t("422 field error for an unrendered path leaves the body untouched", async () => {
  seedWorkflow();
  S.configState.dirtyPaths.add("routes");
  const body = $("settings-body");
  body.innerHTML = '<div class="frow"><input id="f-other"></div>';
  const before = body.innerHTML;
  mockFetch([{ status: 422, json: { valid: false, errors: [
    { path: "routes[9].virtual_model", code: "required", message: "missing" },
  ] } }]);
  await S.validateConfig();
  eq(S.configState.saveState, "invalid");
  eq(body.innerHTML, before, "unknown path: no string surgery on the body");
});

t("422 in raw mode reports line/column message in the raw editor", async () => {
  seedWorkflow();
  S.configState.mode = "raw";
  $("settings-raw").value = "routes: [broken";
  S.configState.dirtyPaths.add("raw");
  mockFetch([{ status: 422, json: { valid: false, errors: [
    { path: "", code: "yaml_syntax", message: "line 1, column 10: did not find expected node content" },
  ] } }]);
  await S.validateConfig();
  eq(S.configState.saveState, "invalid");
  includes($("settings-raw-error").textContent, "line 1, column 10", "raw error text");
});

t("restart-required success shows persisted and active values", async () => {
  seedWorkflow();
  S.configState.base.listen = ":8400";
  S.configState.draft.listen = ":9400";
  S.configState.validated = {
    candidate_revision: "sha256:cand9",
    document: { listen: ":9400" },
    raw_yaml: "listen: :9400\n",
  };
  S.configState.validatedRequest = { expected_revision: "sha256:base", mode: "structured", document: { listen: ":9400" } };
  S.configState.saveState = "review";
  mockFetch([{ status: 200, json: { saved: true, applied: true, revision: "sha256:cand9",
    restart_required: true, restart_required_paths: ["listen"] } }]);
  await S.confirmConfigSave();
  eq(S.configState.saveState, "restart-required");
  const banner = $("settings-status").innerHTML;
  includes(banner, "listen", "restart field named");
  includes(banner, ":9400", "persisted value shown");
  includes(banner, ":8400", "active value shown");
});

t("rollback failure result renders restored state", async () => {
  seedWorkflow();
  S.configState.validated = { candidate_revision: "sha256:cand1", document: {}, raw_yaml: "" };
  S.configState.validatedRequest = { expected_revision: "sha256:base", mode: "structured", document: {} };
  S.configState.saveState = "review";
  mockFetch([{ status: 500, json: { saved: true, applied: false, restored: true,
    revision: "sha256:cand1", restart_required: false, restart_required_paths: [] } }]);
  await S.confirmConfigSave();
  eq(S.configState.saveState, "restored");
  includes($("settings-status").innerHTML.toLowerCase(), "restored");
});

t("switchEditorMode to raw serializes draft; back requires validation", async () => {
  seedWorkflow();
  mockFetch([{ status: 200, json: { valid: true, base_revision: "sha256:base",
    candidate_revision: "sha256:c2", document: { routes: [] }, raw_yaml: "routes: []\n",
    diff: [], changed_paths: [], restart_required_paths: [], warnings: [] } }]);
  await S.switchEditorMode("raw");
  eq(S.configState.mode, "raw");
  ok($("settings-raw").value.includes("routes"), "raw editor filled from draft");
  // editing raw marks dirty; a failed validation blocks switching back
  $("settings-raw").value = "routes: [changed]";
  S.markRawDirty();
  mockFetch([{ status: 422, json: { valid: false, errors: [
    { path: "", code: "yaml_syntax", message: "line 1: broken" } ] } }]);
  await S.switchEditorMode("structured");
  eq(S.configState.mode, "raw", "dirty raw must validate before switching back");
  // a successful validation lets the switch through
  mockFetch([{ status: 200, json: { valid: true, base_revision: "sha256:base",
    candidate_revision: "sha256:c3", document: { routes: ["changed"] }, raw_yaml: "routes: [changed]\n",
    diff: [], changed_paths: ["routes"], restart_required_paths: [], warnings: [] } }]);
  await S.switchEditorMode("structured");
  eq(S.configState.mode, "structured", "validated raw switches back to structured");
});

t("discardConfigDraft restores base and clears candidate", async () => {
  seedWorkflow();
  S.configState.draft.routes.push({ virtual_model: "junk" });
  S.configState.dirtyPaths.add("routes");
  S.configState.validated = { candidate_revision: "x" };
  S.configState.saveState = "review";
  S.discardConfigDraft();
  eq(S.configState.draft.routes.length, 2, "draft reset to base");
  eq(S.configState.dirtyPaths.size, 0);
  eq(S.configState.validated, null);
  eq(S.configState.saveState, "clean");
});

t("api() exposes parsed status and body on errors without secrets", async () => {
  mockFetch([{ status: 409, json: { error: { message: "conflict", type: "config_conflict" } } }]);
  let err = null;
  try { await S.api("/admin/config", { method: "PUT" }); } catch (e) { err = e; }
  ok(err, "error thrown");
  eq(err.status, 409, "status exposed");
  eq(err.body && err.body.error.type, "config_conflict", "parsed body exposed");
});

t("Escape closes dialog before save, not during save", async () => {
  seedWorkflow();
  S.configState.validated = { candidate_revision: "c", document: {}, raw_yaml: "" };
  S.configState.saveState = "review";
  const dlg = dialogStub();
  dlg.open = true;
  S.handleDiffEscape();
  eq(dlg.open, false, "escape closes before save starts");
  dlg.open = true;
  S.configState.saveState = "saving";
  S.handleDiffEscape();
  eq(dlg.open, true, "escape ignored while saving");
});

t("diff dialog renders exact server change kinds (add/remove/update/reorder)", () => {
  seedWorkflow();
  S.openDiffDialog({ candidate_revision: "c", warnings: [], diff: [
    { path: "routes[2]", kind: "add", after: "new-route" },
    { path: "routes[0]", kind: "remove", before: "old-route" },
    { path: "routes[1].strategy", kind: "update", before: "round_robin", after: "fusion_judge" },
    { path: "routes", kind: "reorder", before: "a,b", after: "b,a" },
  ] });
  const h = $("config-diff-body").innerHTML;
  includes(h, "diff-row add", "add row class");
  includes(h, "diff-row remove", "remove row class");
  includes(h, "diff-row update", "update row class");
  includes(h, "diff-row reorder", "reorder row class");
  includes(h, ">add<", "add label verbatim");
  includes(h, ">remove<", "remove label verbatim");
  includes(h, ">update<", "update label verbatim");
  includes(h, ">reorder<", "reorder label verbatim");
  notIncludes(h, "added", "no client-invented 'added' vocabulary");
  notIncludes(h, "removed", "no client-invented 'removed' vocabulary");
  includes(h, "new-route", "add shows after value");
  includes(h, "old-route", "remove shows before value");
  includes(h, "round_robin → fusion_judge", "update shows before → after");
  includes(h, "a,b → b,a", "reorder shows before → after");
});

t("validateConfig: one active request, stale response never overwrites newer edits", async () => {
  seedWorkflow();
  S.configState.dirtyPaths.add("routes");
  let resolveReq;
  sandbox.fetch = (path, opts) => {
    fetchLog.push({ path, opts });
    return new Promise(res => { resolveReq = () => res({
      ok: true, status: 200,
      json: async () => ({ valid: true, base_revision: "sha256:base",
        candidate_revision: "sha256:stale",
        document: { routes: [{ virtual_model: "stale" }] }, raw_yaml: "stale: true\n",
        diff: [], changed_paths: [], restart_required_paths: [], warnings: [] }),
      text: async () => "{}",
    }); });
  };
  fetchLog = [];
  const p1 = S.validateConfig();
  ok(p1 && typeof p1.then === "function", "validateConfig returns a promise");
  eq(fetchLog.length, 1, "request in flight");
  eq(S.configState.saveState, "validating");
  ok($("settings-validate").disabled, "Validate disabled while validating");
  const p2 = S.validateConfig();
  eq(fetchLog.length, 1, "concurrent validateConfig does not send a second request");
  /* user edits while the request is in flight */
  S.setDraftValue("routes[0].virtual_model", "mine");
  resolveReq();
  eq(await p2, await p1, "concurrent call joins the in-flight validation");
  eq(S.configState.draft.routes[0].virtual_model, "mine", "in-flight edit survives the response");
  eq(S.configState.validated, null, "stale candidate not installed");
  ok(S.configState.dirtyPaths.has("routes[0].virtual_model"), "dirty state kept");
  eq(S.configState.saveState, "dirty", "back to dirty, not review");
  ok(!$("config-diff-dialog").open, "diff dialog not opened for a stale response");
});

t("validateConfig: unmutated draft accepts the candidate (revision guard passes)", async () => {
  seedWorkflow();
  S.configState.dirtyPaths.add("routes");
  mockFetch([{ status: 200, json: { valid: true, base_revision: "sha256:base",
    candidate_revision: "sha256:fresh",
    document: { routes: [{ virtual_model: "a" }] }, raw_yaml: "routes: []\n",
    diff: [], changed_paths: [], restart_required_paths: [], warnings: [] } }]);
  await S.validateConfig();
  eq(S.configState.validated && S.configState.validated.candidate_revision, "sha256:fresh",
     "fresh candidate installed");
  eq(S.configState.saveState, "review");
});

/* ---------- 7. Task 12: periodic refresh must not clobber Settings drafts ---------- */

/* controllable interval stub: capture the callback, never auto-run */
let intervalCb = null;
sandbox.setInterval = (cb) => { intervalCb = cb; return 1; };

/* enter settings section without triggering a load: current is a vm-local let */
function enterSettings() { ctx("current = 'settings'"); }
function leaveSettings() { ctx("current = 'overview'"); }

t("periodic refresh on settings with dirty draft does not call loadSettings", async () => {
  seedWorkflow();
  enterSettings();
  S.configState.saveState = "dirty";
  S.configState.dirtyPaths.add("routes[0].virtual_model");
  S.configState.draft.routes[0].virtual_model = "draft-survival-probe";
  /* if loadSettings runs it will fetch /admin/config; track via fetch */
  let fetched = false;
  sandbox.fetch = async (path) => { if (path === "/admin/config") fetched = true; return { ok: true, status: 200, json: async () => ({ document: {}, schema: {}, revision: "r", raw_yaml: "" }) }; };
  await S.refresh();
  leaveSettings();
  ok(!fetched, "loadSettings must not fetch while settings is dirty");
  eq(S.configState.draft.routes[0].virtual_model, "draft-survival-probe", "draft preserved");
  eq(S.configState.saveState, "dirty", "dirty state preserved");
});

t("periodic refresh on settings in review/saving/conflict does not call loadSettings", async () => {
  for (const st of ["review", "saving", "conflict", "validating"]) {
    seedWorkflow();
    enterSettings();
    S.configState.saveState = st;
    S.configState.dirtyPaths.add("routes");
    S.configState.draft.routes[0].virtual_model = "keep-" + st;
    let fetched = false;
    sandbox.fetch = async (path) => { if (path === "/admin/config") fetched = true; return { ok: true, status: 200, json: async () => ({ document: {}, schema: {}, revision: "r", raw_yaml: "" }) }; };
    await S.refresh();
    leaveSettings();
    ok(!fetched, "loadSettings must not fetch in state " + st);
    eq(S.configState.draft.routes[0].virtual_model, "keep-" + st, "draft preserved in " + st);
    eq(S.configState.saveState, st, "state preserved in " + st);
  }
});

t("periodic refresh on settings with clean state does call loadSettings", async () => {
  seedWorkflow();
  enterSettings();
  S.configState.saveState = "clean";
  S.configState.dirtyPaths = new Set();
  let fetched = false;
  sandbox.fetch = async (path) => { if (path === "/admin/config") fetched = true; return { ok: true, status: 200, json: async () => ({ document: {}, schema: {}, revision: "r", raw_yaml: "" }) }; };
  await S.refresh();
  leaveSettings();
  ok(fetched, "loadSettings runs when settings is clean");
});

t("explicit Reload-and-compare still calls loadSettings even when dirty", async () => {
  seedWorkflow();
  enterSettings();
  S.configState.saveState = "conflict";
  S.configState.dirtyPaths.add("routes");
  let fetched = false;
  sandbox.fetch = async (path) => { if (path === "/admin/config") fetched = true; return { ok: true, status: 200, json: async () => ({ document: {}, schema: {}, revision: "r", raw_yaml: "" }) }; };
  S.handleConfigConflict({ error: { message: "conflict" } });
  const btn = $("settings-reload-compare");
  ok(btn, "reload-compare button rendered");
  btn.click();
  await new Promise(r => setTimeout(r, 0));
  leaveSettings();
  ok(fetched, "explicit reload calls loadSettings");
});

t("select('settings') loads settings on initial navigation (never loaded)", async () => {
  seedWorkflow();
  leaveSettings();
  ctx("loaded = {}");
  S.configState.saveState = "dirty";
  S.configState.dirtyPaths.add("routes");
  let fetched = false;
  sandbox.fetch = async (path) => { if (path === "/admin/config") fetched = true; return { ok: true, status: 200, json: async () => ({ document: {}, schema: {}, revision: "r", raw_yaml: "" }) }; };
  S.select("settings");
  /* select() does not await refresh; flush microtasks */
  await new Promise(r => setTimeout(r, 0));
  ok(fetched, "initial navigation to settings loads");
  eq(S.configState.saveState, "clean", "initial load resets to clean");
});

t("re-click active settings nav with dirty draft preserves draft (no fetch)", async () => {
  seedWorkflow();
  enterSettings();
  ctx("loaded = { settings: true }");
  S.configState.saveState = "dirty";
  S.configState.dirtyPaths.add("routes[0].virtual_model");
  S.configState.draft.routes[0].virtual_model = "reclick-probe";
  let fetched = false;
  sandbox.fetch = async (path) => { if (path === "/admin/config") fetched = true; return { ok: true, status: 200, json: async () => ({ document: {}, schema: {}, revision: "r", raw_yaml: "" }) }; };
  S.select("settings");
  await new Promise(r => setTimeout(r, 0));
  leaveSettings();
  ok(!fetched, "re-click on active settings must not fetch while dirty");
  eq(S.configState.draft.routes[0].virtual_model, "reclick-probe", "draft preserved on re-click");
  eq(S.configState.saveState, "dirty", "dirty state preserved on re-click");
});

t("leave settings and return with dirty draft preserves draft (no fetch)", async () => {
  seedWorkflow();
  enterSettings();
  ctx("loaded = { settings: true, overview: true }");
  S.configState.saveState = "dirty";
  S.configState.dirtyPaths.add("routes");
  S.configState.draft.routes[0].virtual_model = "return-probe";
  let configFetches = 0;
  sandbox.fetch = async (path) => {
    if (path === "/admin/config") { configFetches++; return { ok: true, status: 200, json: async () => ({ document: {}, schema: {}, revision: "r", raw_yaml: "" }) }; }
    /* overview loaders */
    return { ok: true, status: 200, json: async () => (Array.isArray(0) ? [] : {}) };
  };
  S.select("overview");
  await new Promise(r => setTimeout(r, 0));
  S.select("settings");
  await new Promise(r => setTimeout(r, 0));
  eq(configFetches, 0, "returning to settings with dirty draft must not reload config");
  eq(S.configState.draft.routes[0].virtual_model, "return-probe", "draft preserved after leave/return");
  eq(S.configState.saveState, "dirty", "dirty state preserved after leave/return");
});

t("Save key click with dirty settings draft retries connectivity without clobbering", async () => {
  seedWorkflow();
  enterSettings();
  ctx("loaded = { settings: true }");
  S.configState.saveState = "dirty";
  S.configState.dirtyPaths.add("routes");
  S.configState.draft.routes[0].virtual_model = "savekey-probe";
  let savedKey = null, configFetches = 0;
  sandbox.sessionStorage = { getItem: () => null, setItem: (k, v) => { savedKey = v; } };
  sandbox.fetch = async (path) => { if (path === "/admin/config") configFetches++; return { ok: true, status: 200, json: async () => ({}) }; };
  $("key").value = "new-admin-key";
  $("savekey").click();
  await new Promise(r => setTimeout(r, 0));
  leaveSettings();
  eq(savedKey, "new-admin-key", "session credential updated");
  eq(configFetches, 0, "Save key must not reload settings while dirty");
  eq(S.configState.draft.routes[0].virtual_model, "savekey-probe", "settings draft survives Save key");
  eq(S.configState.saveState, "dirty", "dirty state survives Save key");
});

t("Enter in key field with dirty settings draft behaves like Save key click", async () => {
  seedWorkflow();
  enterSettings();
  ctx("loaded = { settings: true }");
  S.configState.saveState = "dirty";
  S.configState.dirtyPaths.add("routes");
  S.configState.draft.routes[0].virtual_model = "enter-probe";
  let configFetches = 0;
  sandbox.fetch = async (path) => { if (path === "/admin/config") configFetches++; return { ok: true, status: 200, json: async () => ({}) }; };
  $("key").dispatch("keydown", { key: "Enter" });
  await new Promise(r => setTimeout(r, 0));
  leaveSettings();
  eq(configFetches, 0, "Enter path must not reload settings while dirty");
  eq(S.configState.draft.routes[0].virtual_model, "enter-probe", "draft survives Enter");
  eq(S.configState.saveState, "dirty", "dirty state survives Enter");
});

t("Save key on clean settings still reloads (auth recovery)", async () => {
  seedWorkflow();
  enterSettings();
  ctx("loaded = { settings: true }");
  S.configState.saveState = "clean";
  S.configState.dirtyPaths = new Set();
  let fetched = false;
  sandbox.fetch = async (path) => { if (path === "/admin/config") fetched = true; return { ok: true, status: 200, json: async () => ({ document: {}, schema: {}, revision: "r", raw_yaml: "" }) }; };
  $("savekey").click();
  await new Promise(r => setTimeout(r, 0));
  leaveSettings();
  ok(fetched, "Save key on clean settings retries the config load");
});

/* ---------- summary ---------- */
run();
