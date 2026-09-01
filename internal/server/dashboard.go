package server

import "net/http"

// dashboardPage is the self-contained admin dashboard (dark telemetry
// console, vanilla JS, 5s auto-refresh). The admin key is stored in
// sessionStorage and sent as X-Admin-Key on fetches.
const dashboardPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>TokenRoute Console</title>
<style>
  :root {
    --bg:#0B0B10; --card:#131318; --border:#1E293B; --fg:#F8FAFC;
    --muted:#94A3B8; --accent:#3B82F6; --red:#EF4444; --green:#34D399; --amber:#FBBF24;
    --mono:ui-monospace,SFMono-Regular,Consolas,"Liberation Mono",monospace;
  }
  * { box-sizing:border-box; margin:0; }
  html { font-size:13px; }
  body {
    background:var(--bg); color:var(--fg);
    font:13px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,sans-serif;
  }
  .mono, td.n, .num { font-family:var(--mono); font-variant-numeric:tabular-nums; }
  a { color:var(--accent); text-decoration:none; }
  button { cursor:pointer; font:inherit; }
  button:active { transform:translateY(1px); }
  button:disabled { opacity:.6; cursor:default; }
  button:disabled:active { transform:none; }
  :focus-visible { outline:2px solid var(--accent); outline-offset:2px; }

  /* Icons */
  .ic { width:16px; height:16px; flex:none; stroke:currentColor; fill:none; stroke-width:1.5; stroke-linecap:round; stroke-linejoin:round; }

  /* Layout */
  #app { display:grid; grid-template-columns:200px 1fr; min-height:100vh; }
  aside {
    border-right:1px solid var(--border); padding:20px 12px;
    position:sticky; top:0; height:100vh; display:flex; flex-direction:column; gap:2px;
  }
  .wordmark { font-weight:700; font-size:15px; padding:0 10px 18px; letter-spacing:.02em; }
  .wordmark span { color:var(--accent); }
  nav a {
    display:flex; align-items:center; gap:9px; padding:7px 10px; border-radius:6px; color:var(--muted);
    transition:background 150ms ease,color 150ms ease;
  }
  nav a:hover { color:var(--fg); background:#17171e; }
  nav a.active { color:var(--fg); background:#1a2333; }
  main { padding:0 24px 48px; min-width:0; }

  /* Top bar */
  header {
    display:flex; align-items:center; gap:12px; padding:14px 0;
    border-bottom:1px solid var(--border); margin-bottom:24px;
    position:sticky; top:0; background:var(--bg); z-index:10;
  }
  header input[type=password] {
    background:var(--card); border:1px solid var(--border); color:var(--fg);
    border-radius:6px; padding:6px 10px; width:300px; font-family:var(--mono);
  }
  #live { margin-left:auto; display:flex; align-items:center; gap:8px; color:var(--muted); font-size:12px; }
  #dot { width:8px; height:8px; border-radius:50%; background:var(--green); animation:pulse 2s infinite; }
  @keyframes pulse { 0%,100% { opacity:1; } 50% { opacity:.35; } }
  #updated { font-family:var(--mono); font-size:11px; }

  #errbar {
    display:none; background:#2a1215; border:1px solid var(--red); color:#fca5a5;
    border-radius:6px; padding:10px 14px; margin-bottom:20px;
  }

  section { display:none; }
  section.active { display:block; }
  h2 { font-size:14px; font-weight:600; margin-bottom:14px; letter-spacing:.02em; display:flex; align-items:center; gap:8px; }
  h2 .ic { color:var(--muted); }
  .hint { color:var(--muted); font-size:12px; margin-top:8px; }

  /* Cards + KPI */
  .card { background:var(--card); border:1px solid var(--border); border-radius:8px; padding:16px; }
  .kpis { display:grid; grid-template-columns:repeat(auto-fit,minmax(180px,1fr)); gap:12px; margin-bottom:20px; }
  .kpi .label { color:var(--muted); font-size:11px; text-transform:uppercase; letter-spacing:.08em; display:flex; align-items:center; gap:6px; }
  .kpi .value { font-family:var(--mono); font-size:24px; font-weight:600; margin-top:6px; }
  .kpi .value.accent { color:var(--accent); }
  .grid2 { display:grid; grid-template-columns:1fr 1fr; gap:12px; }
  @media (max-width:1000px) { .grid2 { grid-template-columns:1fr; } }
  .card h3 { font-size:12px; color:var(--muted); text-transform:uppercase; letter-spacing:.08em; margin-bottom:12px; display:flex; align-items:center; gap:6px; }

  /* Tables */
  .tblwrap { overflow-x:auto; }
  table { width:100%; border-collapse:collapse; }
  th, td { padding:8px 10px; text-align:left; border-bottom:1px solid var(--border); white-space:nowrap; }
  th { color:var(--muted); font-weight:600; font-size:11px; text-transform:uppercase; letter-spacing:.06em; }
  tbody tr { transition:background 150ms ease; }
  tbody tr:hover { background:#17171e; }
  tbody tr:last-child td { border-bottom:none; }
  td.wrap { white-space:normal; }

  /* Pills */
  .pill { display:inline-block; padding:2px 10px; border-radius:999px; font-size:11px; font-weight:600; font-family:var(--mono); }
  .pill.closed, .pill.s2 { background:rgba(52,211,153,.12); color:var(--green); }
  .pill.open, .pill.s5 { background:rgba(239,68,68,.12); color:var(--red); }
  .pill.half-open, .pill.s4 { background:rgba(251,191,36,.12); color:var(--amber); }
  .tag {
    display:inline-block; padding:1px 6px; border-radius:4px; font-size:10px; font-weight:600;
    font-family:var(--mono); background:rgba(59,130,246,.14); color:var(--accent);
    border:1px solid rgba(59,130,246,.35); margin-left:6px; vertical-align:1px;
  }

  /* Buttons */
  .btn {
    background:#1a2333; color:var(--fg); border:1px solid var(--border);
    border-radius:6px; padding:5px 12px; transition:background 150ms ease,transform 50ms ease;
  }
  .btn:hover { background:#22304a; }
  .btn.primary { background:var(--accent); border-color:var(--accent); color:#fff; }
  .btn.primary:hover:not(:disabled) { background:#2f6fd6; }
  .btn.danger { background:transparent; border-color:var(--red); color:var(--red); }
  .btn.danger:hover { background:rgba(239,68,68,.12); }
  .btn.sm { padding:3px 9px; font-size:12px; }

  /* Toggle switch */
  .sw { position:relative; width:34px; height:18px; display:inline-block; vertical-align:middle; }
  .sw input { opacity:0; width:0; height:0; }
  .sw .track {
    position:absolute; inset:0; background:#2a3348; border-radius:999px; transition:background 200ms ease;
  }
  .sw .track::after {
    content:""; position:absolute; top:2px; left:2px; width:14px; height:14px;
    border-radius:50%; background:var(--fg); transition:transform 200ms ease;
  }
  .sw input:checked + .track { background:var(--accent); }
  .sw input:checked + .track::after { transform:translateX(16px); }
  .sw input:focus-visible + .track { outline:2px solid var(--accent); outline-offset:2px; }

  /* Forms */
  .formrow { display:flex; flex-wrap:wrap; gap:10px; align-items:flex-end; margin-bottom:16px; }
  .field label { display:block; color:var(--muted); font-size:11px; text-transform:uppercase; letter-spacing:.06em; margin-bottom:4px; }
  .field input {
    background:var(--card); border:1px solid var(--border); color:var(--fg);
    border-radius:6px; padding:6px 10px; width:140px; font-family:var(--mono);
  }
  .field input.wide { width:220px; font-family:inherit; }
  #create-err { color:var(--red); font-size:12px; margin:-8px 0 12px; display:none; }

  /* New key reveal */
  #newkey {
    display:none; background:#0f1a2e; border:1px solid var(--accent); border-radius:8px;
    padding:14px 16px; margin-bottom:16px;
  }
  #newkey .k { font-family:var(--mono); font-size:14px; color:var(--accent); word-break:break-all; margin:6px 0 10px; }

  /* Bar chart */
  .bar-row { display:grid; grid-template-columns:140px 1fr 90px; align-items:center; gap:10px; margin-bottom:8px; }
  .bar-row .name { color:var(--muted); overflow:hidden; text-overflow:ellipsis; }
  .bar-row .amt { font-family:var(--mono); text-align:right; font-size:12px; }
  .bar-row svg { display:block; width:100%; height:14px; }
  .bar-row rect { fill:var(--accent); rx:3; }

  /* Requests-over-time chart */
  #reqchart { margin-bottom:20px; }
  #reqchart .plot { display:block; width:100%; height:120px; }
  #reqchart .marks { display:flex; justify-content:space-between; color:var(--muted); font-family:var(--mono); font-size:10px; margin-top:4px; }

  /* Skeleton shimmer */
  @keyframes shimmer { from { background-position:200% 0; } to { background-position:-200% 0; } }
  .skel td { padding:8px 10px; border-bottom:1px solid var(--border); }
  .skel .b {
    height:12px; border-radius:4px; width:70%;
    background:linear-gradient(90deg,#17171e 25%,#1f2430 50%,#17171e 75%);
    background-size:200% 100%; animation:shimmer 1.4s infinite linear;
  }
  .empty { color:var(--muted); padding:24px 10px; text-align:center; }

  @media (prefers-reduced-motion:reduce) {
    *, *::before, *::after { animation:none !important; transition:none !important; }
  }
</style>
</head>
<body>
<div id="app">
  <aside>
    <div class="wordmark">Token<span>Route</span></div>
    <nav id="nav">
      <a href="#overview" data-s="overview" class="active"><svg class="ic" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 20a8 8 0 1 1 8-8"/><path d="M12 12l5-5"/><circle cx="12" cy="12" r="1.5"/></svg>Overview</a>
      <a href="#keys" data-s="keys"><svg class="ic" viewBox="0 0 24 24" aria-hidden="true"><circle cx="8" cy="12" r="4"/><path d="M12 12h8"/><path d="M17 12v3"/><path d="M20 12v2"/></svg>Keys</a>
      <a href="#providers" data-s="providers"><svg class="ic" viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="4" width="18" height="6" rx="1.5"/><rect x="3" y="14" width="18" height="6" rx="1.5"/><path d="M7 7h.01"/><path d="M7 17h.01"/></svg>Providers</a>
      <a href="#logs" data-s="logs"><svg class="ic" viewBox="0 0 24 24" aria-hidden="true"><path d="M8 6h13"/><path d="M8 12h13"/><path d="M8 18h13"/><path d="M3.5 6h.01"/><path d="M3.5 12h.01"/><path d="M3.5 18h.01"/></svg>Logs</a>
    </nav>
  </aside>
  <main>
    <header>
      <input id="key" type="password" placeholder="admin key" aria-label="admin key">
      <button class="btn" id="savekey">Save key</button>
      <div id="live"><span id="dot" aria-hidden="true"></span>live <span id="updated"></span></div>
    </header>
    <div id="errbar" role="alert"></div>

    <section id="s-overview" class="active">
      <h2><svg class="ic" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 20a8 8 0 1 1 8-8"/><path d="M12 12l5-5"/><circle cx="12" cy="12" r="1.5"/></svg>Overview</h2>
      <div class="kpis">
        <div class="card kpi"><div class="label"><svg class="ic" viewBox="0 0 24 24" aria-hidden="true"><path d="M3 12h4l3-7 4 14 3-7h4"/></svg>Total requests</div><div class="value" id="k-req" data-v="-">-</div></div>
        <div class="card kpi"><div class="label"><svg class="ic" viewBox="0 0 24 24" aria-hidden="true"><path d="M5 4v16"/><path d="M9 4v16"/><path d="M15 4v16"/><path d="M19 4v16"/><path d="M3 8h18"/><path d="M3 16h18"/></svg>Total tokens</div><div class="value" id="k-tok" data-v="-">-</div></div>
        <div class="card kpi"><div class="label"><svg class="ic" viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="8"/><path d="M12 7v10"/><path d="M15 9.5c-.6-1-1.7-1.5-3-1.5-1.7 0-3 .8-3 2.25s1.3 1.9 3 2.25c1.7.35 3 .8 3 2.25S13.7 17 12 17c-1.3 0-2.4-.5-3-1.5"/></svg>Total cost USD</div><div class="value accent" id="k-cost" data-v="-">-</div></div>
        <div class="card kpi"><div class="label"><svg class="ic" viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="13" r="7"/><path d="M12 10v3l2 2"/><path d="M9 3h6"/></svg>Avg latency ms</div><div class="value" id="k-lat" data-v="-">-</div></div>
      </div>
      <div class="card" id="reqchart">
        <h3><svg class="ic" viewBox="0 0 24 24" aria-hidden="true"><path d="M3 17l5-6 4 3 6-8"/><path d="M3 21h18"/></svg>Requests per minute &middot; last 30 min</h3>
        <div id="reqchart-body"><div class="empty">Loading&hellip;</div></div>
      </div>
      <div class="grid2">
        <div class="card">
          <h3>Cost per key</h3>
          <div id="costchart"></div>
        </div>
        <div class="card">
          <h3>Providers</h3>
          <div id="provmini"></div>
        </div>
      </div>
    </section>

    <section id="s-keys">
      <h2><svg class="ic" viewBox="0 0 24 24" aria-hidden="true"><circle cx="8" cy="12" r="4"/><path d="M12 12h8"/><path d="M17 12v3"/><path d="M20 12v2"/></svg>API Keys</h2>
      <div id="newkey">
        <div style="color:var(--muted);font-size:12px">Key created. Copy it now; it will not be shown again.</div>
        <div class="k" id="newkey-val"></div>
        <button class="btn primary sm" id="copykey">Copy</button>
      </div>
      <div class="formrow card" style="padding:12px 16px">
        <div class="field"><label for="f-name">Name</label><input id="f-name" class="wide" placeholder="ci-bot"></div>
        <div class="field"><label for="f-rpm">RPM</label><input id="f-rpm" type="number" min="0" placeholder="0 = unlimited"></div>
        <div class="field"><label for="f-tpm">TPM</label><input id="f-tpm" type="number" min="0" placeholder="0 = unlimited"></div>
        <div class="field"><label for="f-quota">Quota tokens</label><input id="f-quota" type="number" min="0" placeholder="0 = unlimited"></div>
        <button class="btn primary" id="create">Create</button>
      </div>
      <div id="create-err" role="alert"></div>
      <div class="card" style="padding:4px 8px">
        <div class="tblwrap">
        <table id="keys">
          <thead><tr><th>Name</th><th>Key</th><th>RPM</th><th>TPM</th><th>Quota</th><th>Spent</th><th>Enabled</th><th></th></tr></thead>
          <tbody></tbody>
        </table>
        </div>
      </div>
    </section>

    <section id="s-providers">
      <h2><svg class="ic" viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="4" width="18" height="6" rx="1.5"/><rect x="3" y="14" width="18" height="6" rx="1.5"/><path d="M7 7h.01"/><path d="M7 17h.01"/></svg>Providers</h2>
      <div class="card" style="padding:4px 8px">
        <div class="tblwrap">
        <table id="providers">
          <thead><tr><th>Name</th><th>Priority</th><th>EMA latency ms</th><th>Circuit</th><th></th></tr></thead>
          <tbody></tbody>
        </table>
        </div>
      </div>
    </section>

    <section id="s-logs">
      <h2><svg class="ic" viewBox="0 0 24 24" aria-hidden="true"><path d="M8 6h13"/><path d="M8 12h13"/><path d="M8 18h13"/><path d="M3.5 6h.01"/><path d="M3.5 12h.01"/><path d="M3.5 18h.01"/></svg>Request logs</h2>
      <div class="card" style="padding:4px 8px">
        <div class="tblwrap">
        <table id="logs">
          <thead><tr><th>Time</th><th>Key</th><th>Model</th><th>Provider</th><th>Tokens p/c/t</th><th>Status</th><th>Latency</th><th>Cost</th></tr></thead>
          <tbody></tbody>
        </table>
        </div>
      </div>
    </section>
  </main>
</div>

<script>
const $ = id => document.getElementById(id);
const keyEl = $("key");
keyEl.value = sessionStorage.getItem("adminKey") || "";
const q = new URLSearchParams(location.search).get("key");
if (q) { sessionStorage.setItem("adminKey", q); keyEl.value = q; }

$("savekey").onclick = () => { sessionStorage.setItem("adminKey", keyEl.value); refresh(true); };
keyEl.addEventListener("keydown", e => { if (e.key === "Enter") $("savekey").click(); });

function esc(s) { const d = document.createElement("div"); d.textContent = s == null ? "" : String(s); return d.innerHTML; }
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
  if (r.status === 401) { const e = new Error("401"); e.auth = true; throw e; }
  if (!r.ok) throw new Error(path + " -> " + r.status);
  return r.status === 204 ? null : r.json();
}

function showErr(msg) { const b = $("errbar"); b.textContent = msg; b.style.display = "block"; }
function clearErr() { $("errbar").style.display = "none"; }

/* Nav */
let current = "overview";
const sections = ["overview", "keys", "providers", "logs"];
function select(name, push) {
  if (!sections.includes(name)) name = "overview";
  current = name;
  for (const s of sections) $("s-" + s).classList.toggle("active", s === name);
  document.querySelectorAll("#nav a").forEach(a => a.classList.toggle("active", a.dataset.s === name));
  if (push) history.replaceState(null, "", "#" + name);
  refresh();
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
  try {
    if (current === "overview") await loadOverview();
    else if (current === "keys") await loadKeys(force);
    else if (current === "providers") await loadProviders();
    else if (current === "logs") await loadLogs();
    clearErr();
    loaded[current] = true;
    $("updated").textContent = new Date().toLocaleTimeString("en-GB", {hour12:false});
  } catch (e) {
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
    '<line x1="0" y1="' + base + '" x2="' + W + '" y2="' + base + '" stroke="#1E293B" stroke-width="1"/>' +
    '<line x1="0" y1="' + (H * 0.5).toFixed(1) + '" x2="' + W + '" y2="' + (H * 0.5).toFixed(1) + '" stroke="#1E293B" stroke-width="1" stroke-dasharray="3 5" opacity=".6"/>' +
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
  if (force || !loaded.keys) skeleton(tb, 8, 3);
  const data = await api("/admin/keys");
  const keys = data.keys || [];
  tb.innerHTML = keys.length ? keys.map(k =>
    "<tr><td class='wrap'>" + esc(k.name) + "</td><td class='mono'>" + esc(k.key) + "</td>" +
    "<td class='n'>" + unl(k.rpm) + "</td><td class='n'>" + unl(k.tpm) + "</td>" +
    "<td class='n'>" + unl(k.quota_tokens) + "</td><td class='n'>" + fmt(k.spent_tokens) + "</td>" +
    "<td><label class='sw'><input type='checkbox' " + (k.enabled ? "checked " : "") +
    "onchange='toggle(" + k.id + ",this.checked)' aria-label='toggle key " + esc(k.name) + "'><span class='track'></span></label></td>" +
    "<td><button class='btn danger sm' onclick='delKey(" + k.id + ",\"" + esc(k.name).replace(/"/g, "&quot;") + "\")'>Delete</button></td></tr>"
  ).join("") : emptyRow(8, "No API keys yet. Create your first one above.");
}

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
  const body = { name: $("f-name").value.trim(), rpm: num("f-rpm"), tpm: num("f-tpm"), quota_tokens: num("f-quota") };
  cerr.style.display = "none";
  if (!body.name) { cerr.textContent = "Name is required."; cerr.style.display = "block"; return; }
  btn.disabled = true; btn.textContent = "Creating...";
  try {
    const k = await api("/admin/keys", {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify(body)});
    $("newkey-val").textContent = k.key;
    $("newkey").style.display = "block";
    $("copykey").textContent = "Copy";
    $("f-name").value = ""; $("f-rpm").value = ""; $("f-tpm").value = ""; $("f-quota").value = "";
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
  tb.innerHTML = pl.length ? pl.map(p =>
    "<tr><td class='mono'>" + esc(p.name) + "</td><td class='n'>" + p.priority + "</td>" +
    "<td class='n'>" + Math.round(p.ema_latency_ms || 0) + "</td>" +
    "<td><span class='pill " + esc(p.circuit) + "'>" + esc(p.circuit) + "</span></td>" +
    "<td><button class='btn sm' onclick='resetCircuit(\"" + esc(p.name).replace(/"/g, "&quot;") + "\")'>Reset circuit</button></td></tr>"
  ).join("") : emptyRow(5, "No providers configured.");
}

async function resetCircuit(name) {
  try { await api("/admin/providers/" + encodeURIComponent(name) + "/circuit/reset", {method:"POST"}); clearErr(); }
  catch (e) { showErr(e.auth ? "Unauthorized. Re-enter the admin key above." : e.message); }
  refresh();
}

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

select(location.hash.slice(1));
setInterval(refresh, 5000);
</script>
</body>
</html>`

func (s *srv) adminDashboard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(dashboardPage))
}
