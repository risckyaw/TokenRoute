const $ = id => document.getElementById(id);
const keyEl = $("key");
keyEl.value = sessionStorage.getItem("adminKey") || "";
const q = new URLSearchParams(location.search).get("key");
if (q) { sessionStorage.setItem("adminKey", q); keyEl.value = q; }

$("savekey").onclick = () => { sessionStorage.setItem("adminKey", keyEl.value); refresh(); };
keyEl.addEventListener("keydown", e => { if (e.key === "Enter") $("savekey").click(); });

function esc(s) { const d = document.createElement("div"); d.textContent = s == null ? "" : String(s); return d.innerHTML; }
/* escAttr: esc() + apostrophe/backtick encoding for values embedded in quoted attributes
   (esc() alone leaves ' raw, which closes single-quoted attributes -> XSS). */
function escAttr(s) { return esc(s).replace(/'/g, "&#39;").replace(/`/g, "&#96;"); }
function fmt(n) { return (n == null ? 0 : n).toLocaleString("en-US"); }
function fmtCost(c) { return "$" + (c || 0).toFixed(4); }
function unl(v) { return v ? fmt(v) : "inf"; }

const reduceMotion = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;

/* KPI count-up: animate numeric text from old to new over 400ms ease-out. */
const kpiAnims = {};
function setKpi(id, value, format) {
  const el = $(id);
  const oldV = el.dataset.v;
  const newTxt = format(value);
  if (oldV === "-" || oldV === undefined || reduceMotion || typeof value !== "number" || !isFinite(value)) {
    el.dataset.v = String(value); el.textContent = newTxt; return;
  }
  const from = parseFloat(oldV);
  if (!isFinite(from) || from === value) { el.dataset.v = String(value); el.textContent = newTxt; return; }
  el.dataset.v = String(value);
  if (kpiAnims[id]) cancelAnimationFrame(kpiAnims[id]);
  const t0 = performance.now(), dur = 400;
  const step = now => {
    const p = Math.min(1, (now - t0) / dur);
    const e = 1 - Math.pow(1 - p, 3); /* ease-out cubic */
    el.textContent = format(from + (value - from) * e);
    if (p < 1) kpiAnims[id] = requestAnimationFrame(step);
    else { el.textContent = newTxt; delete kpiAnims[id]; }
  };
  kpiAnims[id] = requestAnimationFrame(step);
}

async function api(path, opts) {
  opts = opts || {};
  opts.headers = Object.assign({"X-Admin-Key": sessionStorage.getItem("adminKey") || ""}, opts.headers);
  const r = await fetch(path, opts);
  if (r.status === 401) { const e = new Error("401"); e.auth = true; e.status = 401; throw e; }
  if (!r.ok) {
    const e = new Error(path + " -> " + r.status);
    e.status = r.status;
    try { e.body = await r.json(); } catch (_) { e.body = null; }
    if (e.body && e.body.error && e.body.error.message) e.message = e.body.error.message;
    throw e;
  }
  return r.status === 204 ? null : r.json();
}

function showErr(msg) { const b = $("errbar"); b.textContent = msg; b.style.display = "block"; }
function clearErr() { $("errbar").style.display = "none"; }

/* Nav */
let current = "overview";
const sections = ["overview", "keys", "providers", "logs", "settings"];
function select(name, push) {
  if (!sections.includes(name)) name = "overview";
  current = name;
  for (const s of sections) $("s-" + s).classList.toggle("active", s === name);
  document.querySelectorAll("#nav a").forEach(a => a.classList.toggle("active", a.dataset.s === name));
  if (push) history.replaceState(null, "", "#" + name);
  refresh(true);
}
document.querySelectorAll("#nav a").forEach(a => a.addEventListener("click", e => { e.preventDefault(); select(a.dataset.s, true); }));
window.addEventListener("hashchange", () => select(location.hash.slice(1)));

function skeleton(tbody, cols, rows) {
  let h = "";
  for (let i = 0; i < rows; i++) h += '<tr class="skel">' + "<td><div class='b'></div></td>".repeat(cols) + "</tr>";
  tbody.innerHTML = h;
}
function emptyRow(cols, msg) { return '<tr><td colspan="' + cols + '" class="empty">' + msg + "</td></tr>"; }

let loaded = {};
async function refresh(force) {
  const live = $("live");
  try {
    if (current === "overview") await loadOverview();
    else if (current === "keys") await loadKeys(force);
    else if (current === "providers") await loadProviders();
    else if (current === "logs") await loadLogs();
    else if (current === "settings" && (configState.saveState === "clean" || (force && !loaded.settings))) await loadSettings();
    clearErr();
    loaded[current] = true;
    live.classList.remove("stale");
    $("livelabel").textContent = "live";
    $("updated").textContent = new Date().toLocaleTimeString("en-GB", {hour12:false});
  } catch (e) {
    live.classList.add("stale");
    $("livelabel").textContent = "stale";
    if (e.auth) showErr("Unauthorized. Enter a valid admin key above and press Save key.");
    else showErr(e.message);
  }
}

/* Requests-over-time: bucket last 30 min of usage logs into 30 one-minute
   buckets, render inline SVG area chart. */
async function loadReqChart() {
  const body = $("reqchart-body");
  const rows = await api("/admin/usage/logs?limit=200");
  const now = Date.now();
  const buckets = new Array(30).fill(0);
  (rows || []).forEach(e => {
    const t = new Date(e.ts).getTime();
    if (isNaN(t)) return;
    const i = 29 - Math.floor((now - t) / 60000);
    if (i >= 0 && i < 30) buckets[i]++;
  });
  const pts = buckets.filter(b => b > 0);
  if (pts.length < 2) { body.innerHTML = '<div class="empty">Not enough traffic yet.</div>'; return; }
  const W = 600, H = 120, pad = 2;
  const max = Math.max.apply(null, buckets.concat([1]));
  const xs = i => pad + (i / 29) * (W - 2 * pad);
  const ys = v => H - pad - (v / max) * (H - 2 * pad - 14);
  let line = "", area = "";
  for (let i = 0; i < 30; i++) {
    const x = xs(i).toFixed(1), y = ys(buckets[i]).toFixed(1);
    line += (i ? " L" : "M") + x + " " + y;
  }
  area = line + " L" + xs(29).toFixed(1) + " " + (H - pad) + " L" + xs(0).toFixed(1) + " " + (H - pad) + " Z";
  const base = (H - pad).toFixed(1);
  body.innerHTML =
    '<svg class="plot" viewBox="0 0 ' + W + " " + H + '" preserveAspectRatio="none" aria-hidden="true">' +
    '<line x1="0" y1="' + base + '" x2="' + W + '" y2="' + base + '" stroke="#334155" stroke-width="1"/>' +
    '<line x1="0" y1="' + (H * 0.5).toFixed(1) + '" x2="' + W + '" y2="' + (H * 0.5).toFixed(1) + '" stroke="#334155" stroke-width="1" stroke-dasharray="3 5" opacity=".6"/>' +
    '<path d="' + area + '" fill="#3B82F6" opacity=".2"/>' +
    '<path d="' + line + '" fill="none" stroke="#3B82F6" stroke-width="1.5" stroke-linejoin="round" stroke-linecap="round"/>' +
    "</svg>" +
    '<div class="marks"><span>-30m</span><span>-20m</span><span>-10m</span><span>now</span></div>';
}

async function loadOverview() {
  const [usage, provs] = await Promise.all([api("/admin/usage"), api("/admin/providers"), loadReqChart()]);
  const t = usage.totals || {};
  setKpi("k-req", t.requests || 0, fmt);
  setKpi("k-tok", t.total_tokens || 0, fmt);
  setKpi("k-cost", t.cost_usd || 0, fmtCost);
  let latSum = 0, latN = 0;
  (provs.providers || []).forEach(p => { if (p.ema_latency_ms > 0) { latSum += p.ema_latency_ms; latN++; } });
  setKpi("k-lat", latN ? Math.round(latSum / latN) : NaN, v => isFinite(v) ? String(Math.round(v)) : "-");

  const keys = usage.keys || [];
  const chart = $("costchart");
  if (!keys.length) {
    chart.innerHTML = '<div class="empty">No usage recorded yet.</div>';
  } else {
    const max = Math.max.apply(null, keys.map(u => u.cost_usd || 0).concat([1e-9]));
    chart.innerHTML = keys.map(u => {
      const w = Math.max(1, ((u.cost_usd || 0) / max) * 100);
      return '<div class="bar-row"><div class="name mono">' + esc(u.key_name || u.key_id || "-") +
        '</div><svg viewBox="0 0 100 14" preserveAspectRatio="none"><rect width="' + w + '" height="14"/></svg>' +
        '<div class="amt">' + fmtCost(u.cost_usd) + "</div></div>";
    }).join("");
  }

  const pm = $("provmini");
  const pl = provs.providers || [];
  pm.innerHTML = pl.length
    ? pl.map(p => '<div class="bar-row" style="grid-template-columns:1fr auto auto"><div class="mono">' + esc(p.name) +
        '</div><span class="pill ' + esc(p.circuit) + '">' + esc(p.circuit) + '</span>' +
        '<div class="amt">' + Math.round(p.ema_latency_ms || 0) + " ms</div></div>").join("")
    : '<div class="empty">No providers configured.</div>';
}

async function loadKeys(force) {
  const tb = $("keys").querySelector("tbody");
  if (force || !loaded.keys) skeleton(tb, 6, 3);
  const data = await api("/admin/keys");
  const keys = data.keys || [];
  if (!keys.length) { tb.innerHTML = emptyRow(6, "No API keys yet. Create your first one above."); return; }
  tb.innerHTML = keys.map(k => {
    const exp = k.expires_at ? new Date(k.expires_at).toLocaleString("en-GB", {hour12:false}) : "never";
    const models = (k.allowed_models && k.allowed_models.length) ? k.allowed_models.join(", ") : "all";
    const groups = (k.groups && k.groups.length) ? k.groups.join(", ") : "-";
    const det =
      '<tr class="key-details" data-details-for="' + escAttr(k.id) + '" hidden><td colspan="6"><dl class="kdl">' +
      "<dt>RPM / TPM</dt><dd>" + unl(k.rpm) + " / " + unl(k.tpm) + "</dd>" +
      "<dt>Model RPM</dt><dd>" + unl(k.model_rpm) + "</dd>" +
      "<dt>Limit by header</dt><dd>" + esc(k.limit_by_header || "-") + "</dd>" +
      "<dt>Daily quota</dt><dd>" + unl(k.daily_quota) + " (used " + fmt(k.daily_used) + ")</dd>" +
      "<dt>Quota tokens</dt><dd>" + unl(k.quota_tokens) + " (spent " + fmt(k.spent_tokens) + ")</dd>" +
      "<dt>Budget</dt><dd>" + (k.budget_usd ? fmtCost(k.budget_usd) : "inf") + " (spent " + fmtCost(k.spent_usd) + ")</dd>" +
      "<dt>Allowed models</dt><dd>" + esc(models) + "</dd>" +
      "<dt>Groups</dt><dd>" + esc(groups) + "</dd>" +
      "<dt>Expires</dt><dd>" + esc(exp) + "</dd>" +
      "<dt>Created</dt><dd>" + esc(k.created_at || "-") + "</dd>" +
      "</dl></td></tr>";
    return '<tr><td><button class="btn sm ghost" data-action="key-expand" data-id="' + escAttr(k.id) +
      '" aria-label="expand key details" aria-expanded="false">&#9656;</button></td>' +
      "<td class='wrap'>" + esc(k.name) + "</td><td class='mono'>" + esc(k.key) + "</td>" +
      "<td class='n'>" + fmt(k.spent_tokens) + "</td>" +
      "<td><label class='sw'><input type='checkbox' " + (k.enabled ? "checked " : "") +
      "data-action='key-toggle' data-id='" + escAttr(k.id) + "' aria-label='toggle key " + escAttr(k.name) + "'><span class='track'></span></label></td>" +
      /* key-details rows carry the expandable limits/scopes */
      "<td style='text-align:right'><button class='btn danger sm' data-action='key-delete' data-id='" + escAttr(k.id) +
      "' data-name='" + escAttr(k.name) + "'>Delete</button></td></tr>" + det;
  }).join("");
}

/* Event delegation: no inline handlers; all key/provider/log actions route here. */
document.addEventListener("click", e => {
  const el = e.target.closest("[data-action]");
  if (!el) return;
  const action = el.dataset.action, id = el.dataset.id, name = el.dataset.name;
  if (action === "key-expand") {
    const row = document.querySelector('tr.key-details[data-details-for="' + id + '"]');
    if (!row) return;
    const open = row.hidden;
    row.hidden = !open;
    el.setAttribute("aria-expanded", open ? "true" : "false");
    el.innerHTML = open ? "&#9662;" : "&#9656;";
  } else if (action === "key-delete") delKey(id, name);
  else if (action === "prov-test") testProvider(name, el);
  else if (action === "prov-reset") resetCircuit(name);
  else if (action === "prov-toggle") toggleProvider(name, el.dataset.disabled === "1");
  else if (action === "prov-edit") editProvider(name);
});
document.addEventListener("change", e => {
  const el = e.target.closest("[data-action]");
  if (el && el.dataset.action === "key-toggle") toggle(el.dataset.id, el.checked);
});

async function toggle(id, enable) {
  try { await api("/admin/keys/" + id + (enable ? "/enable" : "/disable"), {method:"POST"}); clearErr(); }
  catch (e) { showErr(e.auth ? "Unauthorized. Re-enter the admin key above." : e.message); }
  refresh();
}

async function delKey(id, name) {
  if (!confirm("Delete key \"" + name + "\"? This cannot be undone.")) return;
  try { await api("/admin/keys/" + id, {method:"DELETE"}); clearErr(); }
  catch (e) { showErr(e.auth ? "Unauthorized. Re-enter the admin key above." : e.message); }
  refresh();
}

$("create").onclick = async () => {
  const btn = $("create"), cerr = $("create-err");
  const num = id => { const v = $(id).value.trim(); return v === "" ? 0 : parseInt(v, 10) || 0; };
  const csv = id => $(id).value.split(",").map(s => s.trim()).filter(Boolean);
  const body = {
    name: $("f-name").value.trim(), rpm: num("f-rpm"), tpm: num("f-tpm"),
    model_rpm: num("f-model-rpm"), limit_by_header: $("f-limit-header").value.trim(),
    daily_quota: num("f-daily-quota"), quota_tokens: num("f-quota"),
    budget_usd: parseFloat($("f-budget").value) || 0,
    allowed_models: csv("f-models"), groups: csv("f-groups"),
  };
  const exp = $("f-expires").value;
  if (exp) body.expires_at = new Date(exp).toISOString(); // datetime-local -> RFC3339
  else body.expires_at = null;
  cerr.style.display = "none";
  if (!body.name) { cerr.textContent = "Name is required."; cerr.style.display = "block"; return; }
  btn.disabled = true; btn.textContent = "Creating...";
  try {
    const k = await api("/admin/keys", {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify(body)});
    $("newkey-val").textContent = k.key;
    $("newkey").style.display = "block";
    $("copykey").textContent = "Copy";
    for (const id of ["f-name","f-rpm","f-tpm","f-quota","f-model-rpm","f-limit-header","f-daily-quota","f-budget","f-models","f-groups","f-expires"]) $(id).value = "";
    clearErr();
  } catch (e) {
    cerr.textContent = e.auth ? "Unauthorized. Re-enter the admin key above." : e.message;
    cerr.style.display = "block";
  } finally {
    btn.disabled = false; btn.textContent = "Create";
  }
  refresh();
};

$("copykey").onclick = async () => {
  const v = $("newkey-val").textContent;
  try { await navigator.clipboard.writeText(v); }
  catch (_) {
    const ta = document.createElement("textarea");
    ta.value = v; document.body.appendChild(ta); ta.select();
    document.execCommand("copy"); ta.remove();
  }
  $("copykey").textContent = "Copied";
  setTimeout(() => { $("copykey").textContent = "Copy"; }, 1500);
};

async function loadProviders() {
  const tb = $("providers").querySelector("tbody");
  if (!loaded.providers) skeleton(tb, 5, 3);
  const data = await api("/admin/providers");
  const pl = data.providers || [];
  tb.innerHTML = pl.length ? pl.map(p => {
    const pills = "<span class='pill " + esc(p.circuit) + "'>" + esc(p.circuit) + "</span>" +
      (p.disabled ? "<span class='pill off'>disabled</span>" : "") +
      (p.balance_low ? "<span class='pill warn'>balance low</span>" : "");
    const escName = escAttr(p.name);
    return "<tr><td class='mono'>" + esc(p.name) + "</td><td class='n'>" + p.priority + "</td>" +
      "<td class='n'>" + Math.round(p.ema_latency_ms || 0) + "</td>" +
      "<td>" + pills + "</td>" +
      "<td style='text-align:right'><button class='btn sm' data-action='prov-test' data-name='" + escName + "'>Test</button> " +
      "<button class='btn sm' data-action='prov-reset' data-name='" + escName + "'>Reset circuit</button> " +
      "<button class='btn sm' data-action='prov-toggle' data-name='" + escName + "' data-disabled='" + (p.disabled ? "1" : "0") + "'>" +
      (p.disabled ? "Enable" : "Disable") + "</button> " +
      "<button class='btn sm' data-action='prov-edit' data-name='" + escName + "'>Edit configuration</button></td></tr>";
  }).join("") : emptyRow(5, "No providers configured.");
}

async function testProvider(name, btn) {
  btn.disabled = true; btn.textContent = "Testing...";
  try {
    const r = await api("/admin/providers/" + encodeURIComponent(name) + "/test", {method:"POST"});
    btn.textContent = r.ok ? ("OK " + r.latency_ms + "ms") : ("FAIL " + (r.status || ""));
    btn.style.color = r.ok ? "var(--ok,#34D399)" : "var(--red,#EF4444)";
    setTimeout(() => { btn.disabled = false; btn.textContent = "Test"; btn.style.color = ""; }, 5000);
  } catch (e) {
    btn.disabled = false; btn.textContent = "Test";
    showErr(e.auth ? "Unauthorized. Re-enter the admin key above." : e.message);
  }
}

async function resetCircuit(name) {
  try { await api("/admin/providers/" + encodeURIComponent(name) + "/circuit/reset", {method:"POST"}); clearErr(); }
  catch (e) { showErr(e.auth ? "Unauthorized. Re-enter the admin key above." : e.message); }
  refresh();
}

async function toggleProvider(name, disabled) {
  try {
    await api("/admin/providers/" + encodeURIComponent(name) + (disabled ? "/enable" : "/disable"), {method:"POST"});
    /* routes: /admin/providers/" + encodeURIComponent(name) + "/disable and
       /admin/providers/" + encodeURIComponent(name) + "/enable */
    clearErr();
  } catch (e) { showErr(e.auth ? "Unauthorized. Re-enter the admin key above." : e.message); }
  refresh();
}

/* Edit configuration: jump to Settings, then activate the providers tab once
   the settings load triggered by select() settles. */
function editProvider(name) {
  select("settings", true);
  const btn = document.querySelector('#subnav button[data-sub="providers"]');
  if (btn) setTimeout(() => btn.click(), 0);
}

/* CSV export: fetch as Blob so the X-Admin-Key header is preserved
   (window.location navigation would drop it and 401). */
$("export-csv").onclick = async () => {
  const btn = $("export-csv");
  btn.disabled = true; btn.textContent = "Exporting...";
  try {
    const r = await fetch("/admin/usage/export?format=csv", {
      headers: {"X-Admin-Key": sessionStorage.getItem("adminKey") || ""},
    });
    if (r.status === 401) { showErr("Unauthorized. Re-enter the admin key above."); return; }
    if (!r.ok) { showErr("Export failed: " + r.status); return; }
    const blob = await r.blob();
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = "tokenroute-usage.csv";
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 10000);
    clearErr();
  } catch (e) { showErr(e.message); }
  finally { btn.disabled = false; btn.textContent = "Export CSV"; }
};

async function loadLogs() {
  const tb = $("logs").querySelector("tbody");
  if (!loaded.logs) skeleton(tb, 8, 6);
  const rows = await api("/admin/usage/logs?limit=100");
  tb.innerHTML = (rows && rows.length) ? rows.map(e => {
    const d = new Date(e.ts);
    const t = isNaN(d) ? "-" : d.toLocaleTimeString("en-GB", {hour12:false});
    const sc = e.status < 300 ? "s2" : e.status < 500 ? "s4" : "s5";
    return "<tr><td class='mono'>" + t + "</td>" +
      "<td class='mono'>" + esc(e.key_name || "-") + "</td>" +
      "<td class='mono wrap'>" + esc(e.virtual_model) + " &rarr; " + esc(e.model) + "</td>" +
      "<td class='mono'>" + esc(e.provider) + "</td>" +
      "<td class='n'>" + e.prompt_tokens + " / " + e.completion_tokens + " / " + e.total_tokens + "</td>" +
      "<td><span class='pill " + sc + "'>" + e.status + "</span>" + (e.stream ? "<span class='tag'>SSE</span>" : "") + "</td>" +
      "<td class='n'>" + e.latency_ms + " ms</td>" +
      "<td class='n'>" + (e.cost_usd != null ? fmtCost(e.cost_usd) : "-") + "</td></tr>";
  }).join("") : emptyRow(8, "No requests logged yet.");
}

/* ===================== Settings: generic schema renderer ===================== */

const SUPPORTED_KINDS = ["string", "number", "bool", "list", "map", "group"];

const configState = {
  base: null,
  draft: null,
  schema: null,
  revision: "",
  validated: null,       // exact server-validated candidate; save uses only this
  validatedRequest: null, // exact edit request that produced validated
  mode: "structured",    // "structured" | "raw"
  rawDraft: "",          // sanitized YAML from the server; raw-mode editing buffer
  dirtyPaths: new Set(),
  draftRevision: 0,       // bumped on every draft mutation; stale-response guard
  saveState: "clean",    // clean|dirty|validating|invalid|review|saving|saved|conflict|restored|restart-required
};

function setSaveState(state) {
  configState.saveState = state;
  updateSettingsToolbar();
}

function showSettingsStatus(kind, html) {
  const el = $("settings-status");
  if (!html) { el.hidden = true; el.textContent = ""; el.className = "settings-status"; return; }
  el.hidden = false;
  el.className = "settings-status " + kind;
  el.innerHTML = html;
}

let settingsSub = "general";
let settingsFilter = "";
let selectedPath = null;

async function loadSettings() {
  const snap = await api("/admin/config");
  configState.base = snap.document;
  configState.draft = JSON.parse(JSON.stringify(snap.document));
  configState.schema = snap.schema;
  configState.revision = snap.revision;
  configState.rawDraft = snap.raw_yaml || "";
  configState.validated = null;
  configState.validatedRequest = null;
  configState.dirtyPaths = new Set();
  configState.mode = "structured";
  setSaveState("clean");
  showSettingsStatus("", null);
  renderSettingsSection(settingsSub === "raw" ? "general" : settingsSub);
  updateSettingsToolbar();
}

function updateSettingsToolbar() {
  const validating = configState.saveState === "validating";
  const dirty = configState.dirtyPaths.size > 0;
  const set = (id, on) => { const b = $(id); b.disabled = !on; b.setAttribute("aria-disabled", on ? "false" : "true"); };
  set("settings-discard", dirty);
  set("settings-validate", dirty && !validating);
  const vb = $("settings-validate");
  vb.setAttribute("aria-busy", validating ? "true" : "false");
  vb.textContent = validating ? "Validating…" : "Validate";
  const m = selectedPath && selectedPath.match(/^(.*)\[(\d+)\]$/);
  const itemSel = !!m;
  set("settings-add", itemSel);
  set("settings-duplicate", itemSel);
  set("settings-delete", itemSel);
  let up = false, down = false;
  if (itemSel) {
    const arr = getDraftValue(m[1]);
    const i = parseInt(m[2], 10);
    up = Array.isArray(arr) && i > 0;
    down = Array.isArray(arr) && i < arr.length - 1;
  }
  set("settings-move-up", up);
  set("settings-move-down", down);
}

function pathParts(path, root) {
  let rest = String(path || "");
  let value = root;
  const parts = [];
  while (rest) {
    if (rest[0] === ".") { rest = rest.slice(1); continue; }
    if (Array.isArray(value) && rest[0] === "[") {
      const m = rest.match(/^\[(\d+)\]/);
      if (m) {
        const index = Number(m[1]);
        parts.push(index);
        value = value[index];
        rest = rest.slice(m[0].length);
        continue;
      }
    }
    if (value && typeof value === "object" && !Array.isArray(value)) {
      const key = Object.keys(value)
        .filter(k => rest === k || rest.startsWith(k + ".") || rest.startsWith(k + "["))
        .sort((a, b) => b.length - a.length)[0];
      if (key !== undefined) {
        parts.push(key);
        value = value[key];
        rest = rest.slice(key.length);
        continue;
      }
    }
    const index = rest.match(/^\[(\d+)\]/);
    if (index) {
      parts.push(Number(index[1]));
      rest = rest.slice(index[0].length);
      value = undefined;
      continue;
    }
    const dot = rest.indexOf(".");
    const bracket = rest.search(/\[\d+\]/);
    const ends = [dot, bracket].filter(i => i >= 0);
    const end = ends.length ? Math.min(...ends) : rest.length;
    const key = rest.slice(0, end);
    parts.push(key);
    value = value && typeof value === "object" ? value[key] : undefined;
    rest = rest.slice(end);
  }
  return parts;
}

function getDraftValue(path) {
  if (!path) return configState.draft;
  const parts = pathParts(path, configState.draft);
  let v = configState.draft;
  for (const p of parts) {
    if (v == null) return undefined;
    v = v[p];
  }
  return v;
}

function setDraftValue(path, value) {
  const parts = pathParts(path, configState.draft);
  let v = configState.draft;
  for (let i = 0; i < parts.length - 1; i++) {
    if (v[parts[i]] == null) v[parts[i]] = /^\d+$/.test(parts[i+1]) ? [] : {};
    v = v[parts[i]];
  }
  v[parts[parts.length - 1]] = value;
  markDirty(path);
}

function markDirty(path) {
  configState.dirtyPaths.add(path);
  configState.draftRevision++;
  /* Any draft mutation invalidates the exact validated candidate; Confirm is
     enabled only for the candidate the user reviewed. */
  configState.validated = null;
  configState.validatedRequest = null;
  if (configState.saveState !== "conflict") setSaveState("dirty");
  updateSettingsToolbar();
}

function markRawDirty() {
  markDirty("raw");
}

function filterSettings(query) {
  settingsFilter = (query || "").toLowerCase();
  renderSettingsSection(settingsSub);
}

function matchesFilter(schema) {
  if (!settingsFilter) return true;
  const hay = [schema.path, schema.name, schema.label, schema.help].join(" ").toLowerCase();
  if (hay.includes(settingsFilter)) return true;
  if (schema.item && matchesFilter(schema.item)) return true;
  return (schema.children || []).some(matchesFilter);
}

function fieldTags(schema) {
  let h = "";
  if (schema.advanced) h += '<span class="advanced-tag">ADV</span>';
  if (schema.secret) h += '<span class="secret-tag">SECRET</span>';
  if (schema.restart_required) h += '<span class="restart-tag">RESTART</span>';
  return h;
}

/* Resolve a visible_when path containing [] wildcards against the concrete
   item path currently being rendered: each [] takes the index of the matching
   collection prefix in currentPath (routes[1].hash_keys -> routes[1].strategy). */
function resolveVisiblePath(visPath, currentPath) {
  if (!visPath.includes("[]")) return visPath;
  let offset = 0;
  return visPath.replace(/\[\]/g, () => {
    const prefix = visPath.slice(0, visPath.indexOf("[]", offset));
    offset = prefix.length + 2;
    const escRe = prefix.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    const m = currentPath && currentPath.match(new RegExp("^" + escRe + "\\[(\\d+)\\]"));
    return m ? "[" + m[1] + "]" : "[0]";
  });
}

function validSecretReplacement(value) {
  return value === "" || value === "__TOKENROUTE_KEEP_SECRET__" || /^\$\{[A-Za-z_][A-Za-z0-9_]*\}$/.test(value);
}

function schemaHasAdvanced(schema) {
  if (!schema) return false;
  if (schema.advanced) return true;
  if (schema.item && schemaHasAdvanced(schema.item)) return true;
  return (schema.children || []).some(schemaHasAdvanced);
}

function schemaVisibleInCurrentSection(schema, inheritedAdvanced) {
  return settingsSub === "advanced" ? !!inheritedAdvanced || schemaHasAdvanced(schema) : !schema.advanced;
}

function renderField(schema, value, path, inheritedAdvanced) {
  if (!schemaVisibleInCurrentSection(schema, inheritedAdvanced) || !matchesFilter(schema)) return "";
  if (schema.visible_when) {
    const dep = getDraftValue(resolveVisiblePath(schema.visible_when.path, path));
    if (!schema.visible_when.values.includes(String(dep))) return "";
  }
  const id = "f-" + path.replace(/[^a-zA-Z0-9]/g, "-");
  const dirty = configState.dirtyPaths.has(path) ? " dirty" : "";
  const val = value == null ? "" : value;
  let inner = "";
  const nestedAdvanced = !!inheritedAdvanced || (settingsSub === "advanced" && schema.advanced);
  switch (schema.kind) {
    case "bool":
      inner = '<label class="sw"><input type="checkbox" id="' + id + '" ' + (value ? "checked " : "") +
        'data-field-path="' + esc(path) + '" data-field-kind="bool" aria-label="' + esc(schema.label) + '">' +
        '<span class="track"></span></label>';
      break;
    case "number":
      inner = '<input type="number" id="' + id + '" value="' + esc(val) + '" ' +
        'data-field-path="' + esc(path) + '" data-field-kind="number" aria-label="' + esc(schema.label) + '">';
      break;
    case "string":
      if (schema.enum && schema.enum.length) {
        inner = '<select id="' + id + '" data-field-path="' + esc(path) + '" data-field-kind="string" aria-label="' + esc(schema.label) + '">' +
          schema.enum.map(e => '<option value="' + esc(e) + '"' + (value === e ? " selected" : "") + '>' + esc(e) + "</option>").join("") +
          "</select>";
      } else if (schema.secret) {
        inner = '<input type="password" id="' + id + '" value="' + esc(val) + '" ' +
          'pattern="\\$\\{[A-Za-z_][A-Za-z0-9_]*\\}" title="Use an environment reference such as ${API_KEY}, keep the existing marker, or clear the field" ' +
          'data-field-path="' + esc(path) + '" data-field-kind="string" data-secret="true" aria-label="' + esc(schema.label) + '">';
      } else {
        inner = '<input type="text" id="' + id + '" value="' + esc(val) + '" ' +
          'data-field-path="' + esc(path) + '" data-field-kind="string" aria-label="' + esc(schema.label) + '">';
      }
      break;
    case "group":
      return renderObject(schema, value, path, nestedAdvanced);
    case "list":
      return renderList(schema, value, path, nestedAdvanced);
    case "map":
      return renderMap(schema, value, path, nestedAdvanced);
    default:
      inner = '<input type="text" id="' + id + '" value="' + esc(val) + '" ' +
        'data-field-path="' + esc(path) + '" data-field-kind="string" aria-label="' + esc(schema.label) + '">';
  }
  const secretHelp = schema.secret ? "Environment reference only (for example ${API_KEY}); keep the existing marker or clear to remove. Literal secrets are not accepted." : "";
  return '<div class="frow' + dirty + '"><label for="' + id + '">' + esc(schema.label) + fieldTags(schema) + "</label>" +
    inner + (secretHelp || schema.help ? '<div class="help">' + esc(secretHelp || schema.help) + "</div>" : "") + "</div>";
}

function renderObject(schema, value, path, inheritedAdvanced) {
  if (!matchesFilter(schema)) return "";
  const nestedAdvanced = !!inheritedAdvanced || (settingsSub === "advanced" && schema.advanced);
  if (settingsSub === "advanced" && !nestedAdvanced && !schemaHasAdvanced(schema)) return "";
  if (settingsSub !== "advanced" && schema.advanced) return "";
  const children = (schema.children || []).filter(c => schemaVisibleInCurrentSection(c, nestedAdvanced));
  const common = settingsSub === "advanced" ? children : children.filter(c => !c.advanced);
  const adv = settingsSub === "advanced" ? [] : children.filter(c => c.advanced);
  let h = "";
  if (path) {
    h += '<details class="fgroup"' + (settingsSub === "advanced" ? "" : " open") + '><summary>' + esc(schema.label) + fieldTags(schema) + "</summary><div class=\"fgroup-body\">";
  }
  for (const c of common) {
    const v = value ? value[c.name] : undefined;
    h += renderField(c, v, path ? path + "." + c.name : c.name, nestedAdvanced);
  }
  if (adv.length) {
    h += '<details class="fgroup"><summary>Advanced</summary><div class="fgroup-body">';
    for (const c of adv) {
      const v = value ? value[c.name] : undefined;
      h += renderField(c, v, path ? path + "." + c.name : c.name, nestedAdvanced);
    }
    h += "</div></details>";
  }
  if (path) h += "</div></details>";
  return h;
}

function renderList(schema, value, path, inheritedAdvanced) {
  if (!schemaVisibleInCurrentSection(schema, inheritedAdvanced) || !matchesFilter(schema)) return "";
  const nestedAdvanced = !!inheritedAdvanced || (settingsSub === "advanced" && schema.advanced);
  const items = Array.isArray(value) ? value : [];
  const itemSchema = schema.item || { kind: "string", label: "Item" };
  let h = '<div class="flist"><div class="flist-head"><span class="flist-title">' + esc(schema.label) + fieldTags(schema) +
    '</span><button class="btn sm" data-action="list-add" data-path="' + esc(path) + '">Add</button></div><div class="flist-body">';
  items.forEach((item, i) => {
    const p = path + "[" + i + "]";
    const title = itemSchema.identity && itemSchema.identity.length
      ? itemSchema.identity.map(k => item && item[k]).filter(Boolean).join(" / ")
      : "Item " + (i + 1);
    const sel = selectedPath === p;
    h += '<div class="flist-card' + (sel ? " selected" : "") + '" role="button" tabindex="0" ' +
      'aria-selected="' + (sel ? "true" : "false") + '" data-action="select" data-path="' + esc(p) + '">' +
      '<div class="flist-card-head"><span class="flist-card-title">' + esc(title) + '</span>' +
      '<div class="flist-card-actions">' +
      '<button class="btn sm" data-action="list-move" data-path="' + esc(path) + '" data-index="' + i + '" data-dir="-1" aria-label="Move up">↑</button>' +
      '<button class="btn sm" data-action="list-move" data-path="' + esc(path) + '" data-index="' + i + '" data-dir="1" aria-label="Move down">↓</button>' +
      '<button class="btn sm" data-action="list-duplicate" data-path="' + esc(path) + '" data-index="' + i + '" aria-label="Duplicate">⧉</button>' +
      '<button class="btn sm danger" data-action="list-delete" data-path="' + esc(path) + '" data-index="' + i + '" aria-label="Delete">×</button>' +
      "</div></div>";
    if (itemSchema.kind === "group") {
      h += renderObject(itemSchema, item, p, nestedAdvanced);
    } else {
      h += renderField(itemSchema, item, p, nestedAdvanced);
    }
    h += "</div>";
  });
  h += "</div></div>";
  return h;
}

function renderMap(schema, value, path, inheritedAdvanced) {
  if (!schemaVisibleInCurrentSection(schema, inheritedAdvanced) || !matchesFilter(schema)) return "";
  const nestedAdvanced = !!inheritedAdvanced || (settingsSub === "advanced" && schema.advanced);
  const obj = value && typeof value === "object" ? value : {};
  const keys = Object.keys(obj);
  const valueSchema = schema.item || ((schema.children || []).length
    ? { kind: "group", label: "Value", children: schema.children }
    : { kind: "string", label: "Value" });
  let h = '<div class="fmap"><div class="fmap-head"><span class="fmap-title">' + esc(schema.label) + fieldTags(schema) +
    '</span><button class="btn sm" data-action="map-add" data-path="' + esc(path) + '">Add</button></div><div class="fmap-body">';
  for (const k of keys) {
    const p = path + "." + k;
    h += '<div class="fmap-row"><input class="fmap-key" value="' + esc(k) + '" data-action="map-rename" data-path="' + esc(path) + '" data-key="' + esc(k) + '" aria-label="Rename key">' +
      '<div class="fmap-val">' + renderField(valueSchema, obj[k], p, nestedAdvanced) + "</div>" +
      '<button class="btn sm danger fmap-del" data-action="map-delete" data-path="' + esc(path) + '" data-key="' + esc(k) + '" aria-label="Delete">×</button></div>';
  }
  h += "</div></div>";
  return h;
}

function settingsSelect(path) {
  selectedPath = path;
  updateSettingsToolbar();
  document.querySelectorAll(".flist-card").forEach(c => {
    const sel = c.dataset.path === path;
    c.classList.toggle("selected", sel);
    c.setAttribute("aria-selected", sel ? "true" : "false");
  });
}

function findSchemaByConcretePath(path) {
  const wanted = String(path).replace(/\[\d+\]/g, "[]");
  let found = null;
  const walk = schema => {
    if (!schema || found) return;
    if (schema.path === wanted) { found = schema; return; }
    walk(schema.item);
    (schema.children || []).forEach(walk);
  };
  walk(configState.schema);
  return found;
}

function settingsListAdd(path) {
  const arr = (getDraftValue(path) || []).slice();
  const schema = findSchemaByConcretePath(path);
  const kind = schema && schema.item && schema.item.kind;
  arr.push(kind === "group" || kind === "map" ? {} : kind === "bool" ? false : kind === "number" ? 0 : "");
  setDraftValue(path, arr);
  selectedPath = path + "[" + (arr.length - 1) + "]";
  renderSettingsSection(settingsSub);
}
function settingsListMove(path, i, d) {
  const arr = (getDraftValue(path) || []).slice();
  const j = i + d;
  if (j < 0 || j >= arr.length) return;
  [arr[i], arr[j]] = [arr[j], arr[i]];
  setDraftValue(path, arr);
  if (selectedPath === path + "[" + i + "]") selectedPath = path + "[" + j + "]";
  renderSettingsSection(settingsSub);
}
function settingsListDuplicate(path, i) {
  const arr = (getDraftValue(path) || []).slice();
  arr.splice(i + 1, 0, JSON.parse(JSON.stringify(arr[i])));
  setDraftValue(path, arr);
  selectedPath = path + "[" + (i + 1) + "]";
  renderSettingsSection(settingsSub);
}
function settingsListDelete(path, i) {
  const arr = (getDraftValue(path) || []).slice();
  arr.splice(i, 1);
  setDraftValue(path, arr);
  if (selectedPath && selectedPath.startsWith(path + "[")) selectedPath = null;
  renderSettingsSection(settingsSub);
}
function settingsMapAdd(path) {
  const obj = Object.assign({}, getDraftValue(path) || {});
  const schema = findSchemaByConcretePath(path);
  const defaultValue = schema && ((schema.children || []).length || (schema.item && (schema.item.kind === "group" || schema.item.kind === "map"))) ? {} : "";
  let k = "new_key";
  let n = 1;
  while (Object.prototype.hasOwnProperty.call(obj, k)) k = "new_key_" + n++;
  obj[k] = defaultValue;
  setDraftValue(path, obj);
  renderSettingsSection(settingsSub);
}
function settingsMapRename(path, oldK, newK) {
  if (!newK || oldK === newK) return;
  const obj = getDraftValue(path) || {};
  obj[newK] = obj[oldK];
  delete obj[oldK];
  setDraftValue(path, obj);
  renderSettingsSection(settingsSub);
}
function settingsMapDelete(path, k) {
  const obj = getDraftValue(path) || {};
  delete obj[k];
  setDraftValue(path, obj);
  renderSettingsSection(settingsSub);
}

function renderSettingsSection(section) {
  settingsSub = section;
  document.querySelectorAll("#subnav button").forEach(b => {
    b.setAttribute("aria-selected", b.dataset.sub === section ? "true" : "false");
  });
  const body = $("settings-body");
  if (!configState.schema) {
    body.innerHTML = '<div class="empty">Loading configuration…</div>';
    return;
  }
  if (section === "raw") {
    configState.mode = "raw";
    body.innerHTML = '<textarea class="fraw" id="settings-raw" aria-label="Raw YAML configuration"></textarea>';
    const ta = $("settings-raw");
    ta.value = configState.rawDraft || "";
    ta.addEventListener("input", () => { configState.rawDraft = ta.value; markRawDirty(); });
    return;
  }
  configState.mode = "structured";
  const root = configState.schema;
  let h = "";
  for (const c of root.children || []) {
    const sec = c.section || "general";
    const adv = c.advanced && !c.section;
    if (section === "advanced") {
      if (!schemaHasAdvanced(c)) continue;
    } else if (adv || sec !== section) {
      continue;
    }
    const v = configState.draft ? configState.draft[c.name] : undefined;
    h += renderField(c, v, c.name);
  }
  body.innerHTML = h || '<div class="empty">No fields in this section.</div>';
}

/* Settings event wiring */
$("settings-search").addEventListener("input", e => filterSettings(e.target.value));
$("settings-discard").onclick = () => discardConfigDraft();

/* ===================== Task 10: validate/diff/save/conflict workflow ===================== */

/* Field id derivation mirrors renderField: "f-" + sanitized path. */
function fieldIdForPath(path) { return "f-" + String(path).replace(/[^a-zA-Z0-9]/g, "-"); }

function sectionForPath(path) {
  const root = String(path || "").replace(/\[(\d+)\]/g, "").split(".")[0];
  const child = (configState.schema && configState.schema.children || []).find(c => c.name === root);
  return (child && child.section) || "general";
}

/* Single-flight + draft-revision guard: one active validation at a time;
   a response whose request started before the latest draft mutation is stale
   and must not overwrite the draft. */
let activeValidation = null;

async function validateConfig() {
  if (activeValidation) return activeValidation;
  if (!configState.dirtyPaths.size && configState.saveState !== "invalid") return;
  const rawMode = configState.mode === "raw";
  const req = { expected_revision: configState.revision, mode: rawMode ? "raw" : "structured" };
  if (rawMode) req.raw_yaml = $("settings-raw").value;
  else req.document = configState.draft;
  setSaveState("validating");
  showSettingsStatus("", null);
  clearFieldErrors();
  const rev = configState.draftRevision;
  activeValidation = (async () => {
    try {
      const cand = await api("/admin/config/validate", {method:"POST",
        headers:{"Content-Type":"application/json"}, body:JSON.stringify(req)});
      if (rev !== configState.draftRevision) return; /* stale: draft moved on */
      /* Successful validation replaces BOTH draft views with server-normalized
         values and binds the exact validated candidate. */
      configState.validated = cand;
      configState.validatedRequest = JSON.parse(JSON.stringify(req));
      configState.draft = JSON.parse(JSON.stringify(cand.document));
      configState.rawDraft = cand.raw_yaml;
      configState.dirtyPaths = new Set();
      setSaveState("review");
      openDiffDialog(cand);
    } catch (e) {
      if (e.status === 409) { handleConfigConflict(e.body); return; }
      if (e.status === 422 && e.body && e.body.errors) { showFieldErrors(e.body.errors); return; }
      setSaveState("dirty");
      showSettingsStatus("err", esc(e.message));
    } finally {
      activeValidation = null;
    }
  })();
  return activeValidation;
}

function clearFieldErrors() {
  const re = $("settings-raw-error");
  re.hidden = true; re.textContent = "";
  document.querySelectorAll(".field-error").forEach(el => el.remove());
  document.querySelectorAll(".frow.invalid").forEach(el => el.classList.remove("invalid"));
}

function showFieldErrors(errors) {
  setSaveState("invalid");
  const rawMode = configState.mode === "raw";
  if (rawMode) {
    const re = $("settings-raw-error");
    re.hidden = false;
    re.textContent = errors.map(x => x.message).join(" ");
    return;
  }
  /* DOM-only error rendering: locate the live editor by its stable field id,
     flag the owning .frow, and append a textContent-populated alert node.
     The body's HTML string is never rewritten, so live controls survive. */
  let firstEditor = null;
  for (const err of errors) {
    if (!err.path) continue;
    const editor = document.getElementById(fieldIdForPath(err.path));
    if (!editor) continue;
    const frow = editor.closest(".frow");
    if (!frow) continue;
    frow.classList.add("invalid");
    const node = document.createElement("div");
    node.className = "field-error";
    node.setAttribute("role", "alert");
    node.textContent = err.message;
    frow.appendChild(node);
    if (!firstEditor) firstEditor = editor;
  }
  if (firstEditor && firstEditor.focus) firstEditor.focus();
}

/* Group diff entries by settings section; kind drives both label text and color. */
function openDiffDialog(candidate) {
  const dlg = $("config-diff-dialog");
  const warn = $("config-diff-warnings");
  if (candidate.warnings && candidate.warnings.length) {
    warn.hidden = false;
    warn.textContent = candidate.warnings.join(" ");
  } else {
    warn.hidden = true; warn.textContent = "";
  }
  const bySection = {};
  for (const ch of candidate.diff || []) {
    const sec = sectionForPath(ch.path);
    (bySection[sec] = bySection[sec] || []).push(ch);
  }
  let html = "";
  const secs = Object.keys(bySection);
  if (!secs.length) {
    html = '<div class="empty">No changes.</div>';
  } else {
    for (const sec of secs) {
      html += '<div class="diff-section"><div class="diff-section-title">' + esc(sec) + "</div>";
      for (const ch of bySection[sec]) {
        /* Server Change.Kind vocabulary: add|remove|update|reorder — render
           verbatim as both the label text and the CSS class. */
        const kind = ch.kind || "update";
        html += '<div class="diff-row ' + esc(kind) + '">' +
          '<span class="diff-kind">' + esc(kind) + '</span>' +
          '<span class="diff-path">' + esc(ch.path) + "</span>" +
          '<span class="diff-val">' + esc(diffValueText(ch)) + "</span></div>";
      }
      html += "</div>";
    }
  }
  $("config-diff-body").innerHTML = html;
  const confirmBtn = $("config-diff-confirm");
  confirmBtn.disabled = false;
  if (dlg.showModal) dlg.showModal(); else dlg.open = true;
  /* Native dialog provides the focus trap; explicitly focus the heading. */
  const title = $("config-diff-title");
  if (title && title.focus) title.focus();
}

function diffValueText(ch) {
  /* Secret diffs arrive as classification text only — render verbatim.
     Server kinds: add shows after, remove shows before, update/reorder show before → after. */
  if (ch.kind === "add") return ch.after != null ? String(ch.after) : "";
  if (ch.kind === "remove") return ch.before != null ? String(ch.before) : "";
  if (ch.after != null && ch.before != null) return String(ch.before) + " → " + String(ch.after);
  return ch.after != null ? String(ch.after) : "";
}

async function confirmConfigSave() {
  const cand = configState.validated;
  const edit = configState.validatedRequest;
  if (!cand || !edit) return; /* Confirm is bound to the exact validated request. */
  const dlg = $("config-diff-dialog");
  const confirmBtn = $("config-diff-confirm");
  confirmBtn.disabled = true;
  setSaveState("saving");
  const req = Object.assign({}, edit, {
    expected_revision: configState.revision,
    candidate_revision: cand.candidate_revision,
  });
  try {
    const res = await api("/admin/config", {method:"PUT",
      headers:{"Content-Type":"application/json"}, body:JSON.stringify(req)});
    if (dlg.close) dlg.close(); else dlg.open = false;
    configState.validated = null;
    configState.validatedRequest = null;
    /* Capture active (pre-save) values for the restart-required banner before
       base is replaced by the persisted candidate. */
    const activeBefore = {};
    for (const p of res.restart_required_paths || []) activeBefore[p] = getBaseValue(p);
    configState.base = JSON.parse(JSON.stringify(cand.document));
    configState.draft = JSON.parse(JSON.stringify(cand.document));
    configState.rawDraft = cand.raw_yaml;
    configState.revision = res.revision;
    configState.dirtyPaths = new Set();
    if (res.restart_required) {
      setSaveState("restart-required");
      showSettingsStatus("warn", restartBannerHtml(res.restart_required_paths || [], activeBefore));
    } else {
      setSaveState("saved");
      showSettingsStatus("ok", "Configuration saved and applied.");
    }
    renderSettingsSection(settingsSub);
  } catch (e) {
    if (dlg.close) dlg.close(); else dlg.open = false;
    confirmBtn.disabled = false;
    if (e.status === 409) { handleConfigConflict(e.body); return; }
    if (e.body && e.body.saved === true && e.body.applied === false) {
      /* Apply failed after write; rollback ran. */
      configState.validated = null;
      configState.validatedRequest = null;
      configState.dirtyPaths = new Set();
      setSaveState("restored");
      showSettingsStatus("err", e.body.restored
        ? "Apply failed; previous configuration restored."
        : "Apply failed and rollback also failed. Check the config file on disk.");
      return;
    }
    setSaveState("review");
    showSettingsStatus("err", esc(e.message));
  }
}

function restartBannerHtml(paths, activeBefore) {
  /* Restart-required success: show persisted (new) and active (pre-save) values. */
  const rows = paths.map(p => {
    const persisted = getDraftValue(p);
    const active = activeBefore ? activeBefore[p] : getBaseValue(p);
    return '<span class="mono">' + esc(p) + "</span>: active <span class=\"mono\">" + esc(active == null ? "" : String(active)) +
      "</span> &rarr; saved <span class=\"mono\">" + esc(persisted == null ? "" : String(persisted)) + "</span> (takes effect after restart)";
  });
  return "Saved. Restart required for: " + rows.join("; ");
}

function getBaseValue(path) {
  const parts = pathParts(path, configState.base);
  let v = configState.base;
  for (const p of parts) { if (v == null) return undefined; v = v[p]; }
  return v;
}

function handleConfigConflict(errorBody) {
  /* A conflict never discards the browser draft and never auto-retries. */
  configState.validated = null;
  setSaveState("conflict");
  const msg = errorBody && errorBody.error && errorBody.error.message ? errorBody.error.message : "Configuration changed on disk.";
  showSettingsStatus("warn", esc(msg) +
    ' Your draft is preserved. <button class="btn sm" id="settings-reload-compare" type="button">Reload and compare</button>');
  const btn = $("settings-reload-compare");
  if (btn) btn.onclick = () => { loadSettings(); };
}

function discardConfigDraft() {
  configState.draft = JSON.parse(JSON.stringify(configState.base));
  configState.rawDraft = "";
  configState.dirtyPaths = new Set();
  configState.validated = null;
  setSaveState("clean");
  showSettingsStatus("", null);
  clearFieldErrors();
  renderSettingsSection(settingsSub);
  updateSettingsToolbar();
}

async function switchEditorMode(mode) {
  if (mode === configState.mode) return;
  if (mode === "raw") {
    /* Structured edits serialize into the raw buffer through server validation
       when dirty; clean state reuses the loaded sanitized YAML. */
    if (configState.dirtyPaths.size) {
      await validateConfig();
      if (!configState.validated) return; /* stay structured; errors shown */
      configState.validated = null;
      setSaveState("dirty");
      renderSettingsSection("raw");
      return;
    }
    renderSettingsSection("raw");
    return;
  }
  /* raw -> structured: dirty raw YAML must validate before switching back. */
  if (configState.dirtyPaths.has("raw")) {
    await validateConfig();
    if (!configState.validated) return; /* stay in raw; line/column feedback shown */
    configState.validated = null;
    setSaveState("dirty");
  }
  renderSettingsSection(settingsSub === "raw" ? "general" : settingsSub);
}

/* Escape closes the dialog only before a save request starts. Native <dialog>
   fires "cancel" on Escape; we preventDefault while saving. */
function handleDiffEscape() {
  const dlg = $("config-diff-dialog");
  if (configState.saveState === "saving") return;
  if (dlg.close) dlg.close(); else dlg.open = false;
}

$("settings-validate").onclick = () => { validateConfig(); };
$("config-diff-cancel").onclick = () => {
  const dlg = $("config-diff-dialog");
  if (configState.saveState === "saving") return;
  if (dlg.close) dlg.close(); else dlg.open = false;
};
$("config-diff-confirm").onclick = () => { confirmConfigSave(); };
$("config-diff-dialog").addEventListener("cancel", e => {
  if (configState.saveState === "saving") e.preventDefault();
});
/* Return focus to the Validate button when the dialog closes. */
$("config-diff-dialog").addEventListener("close", () => {
  const v = $("settings-validate");
  if (v && v.focus) v.focus();
});

/* Raw/structured subnav routes through switchEditorMode so mode transitions
   enforce the validate-before-switch rule. */
document.querySelectorAll("#subnav button").forEach(b => b.addEventListener("click", () => {
  const target = b.dataset.sub;
  const wantMode = target === "raw" ? "raw" : "structured";
  if (wantMode !== configState.mode) { switchEditorMode(wantMode).then(() => {
    if (target !== "raw" && configState.mode === "structured") renderSettingsSection(target);
  }); return; }
  renderSettingsSection(target);
}));
function selectedItem() {
  const m = selectedPath && selectedPath.match(/^(.*)\[(\d+)\]$/);
  return m ? { path: m[1], index: parseInt(m[2], 10) } : null;
}
$("settings-add").onclick = () => { const it = selectedItem(); if (it) settingsListAdd(it.path); };
$("settings-duplicate").onclick = () => { const it = selectedItem(); if (it) settingsListDuplicate(it.path, it.index); };
$("settings-delete").onclick = () => { const it = selectedItem(); if (it) settingsListDelete(it.path, it.index); };
$("settings-move-up").onclick = () => { const it = selectedItem(); if (it) settingsListMove(it.path, it.index, -1); };
$("settings-move-down").onclick = () => { const it = selectedItem(); if (it) settingsListMove(it.path, it.index, 1); };

/* Delegated events for the schema-rendered region: no inline handlers, so map
   keys / paths can never break out of an attribute into script. */
$("settings-body").addEventListener("click", e => {
  const el = e.target.closest("[data-action]");
  if (!el) return;
  const d = el.dataset;
  switch (d.action) {
    case "select": settingsSelect(d.path); break;
    case "list-add": settingsListAdd(d.path); break;
    case "list-move": e.stopPropagation(); settingsListMove(d.path, parseInt(d.index, 10), parseInt(d.dir, 10)); break;
    case "list-duplicate": e.stopPropagation(); settingsListDuplicate(d.path, parseInt(d.index, 10)); break;
    case "list-delete": e.stopPropagation(); settingsListDelete(d.path, parseInt(d.index, 10)); break;
    case "map-add": settingsMapAdd(d.path); break;
    case "map-delete": settingsMapDelete(d.path, d.key); break;
  }
});
$("settings-body").addEventListener("keydown", e => {
  if (e.key !== "Enter" && e.key !== " ") return;
  const card = e.target.closest('.flist-card[data-action="select"]');
  if (!card || e.target !== card) return;
  e.preventDefault();
  settingsSelect(card.dataset.path);
});
$("settings-body").addEventListener("change", e => {
  const el = e.target;
  if (el.dataset && el.dataset.action === "map-rename") {
    settingsMapRename(el.dataset.path, el.dataset.key, el.value.trim());
    return;
  }
  if (el.dataset && el.dataset.fieldPath) {
    const kind = el.dataset.fieldKind;
    if (el.dataset.secret === "true" && !validSecretReplacement(el.value)) {
      el.setCustomValidity("Use an environment reference such as ${API_KEY}, keep the existing marker, or clear the field.");
      el.reportValidity();
      return;
    }
    el.setCustomValidity("");
    const v = kind === "bool" ? el.checked : kind === "number" ? (parseFloat(el.value) || 0) : el.value;
    setDraftValue(el.dataset.fieldPath, v);
    renderSettingsSection(settingsSub);
  }
});

/* ===================== End Settings ===================== */

select(location.hash.slice(1));
setInterval(refresh, 5000);
