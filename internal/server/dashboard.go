package server

import "net/http"

// dashboardPage is the self-contained admin dashboard (dark theme, vanilla
// JS, 5s auto-refresh). The admin key is stored in sessionStorage and sent
// as X-Admin-Key on fetches.
const dashboardPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>TokenRoute — Dashboard</title>
<style>
  * { box-sizing: border-box; margin: 0; }
  body { background:#0d1117; color:#c9d1d9; font:14px/1.5 -apple-system,Segoe UI,Roboto,sans-serif; padding:24px; }
  h1 { font-size:20px; margin-bottom:16px; }
  h2 { font-size:15px; color:#8b949e; margin:24px 0 8px; text-transform:uppercase; letter-spacing:.05em; }
  table { width:100%; border-collapse:collapse; background:#161b22; border:1px solid #30363d; border-radius:6px; overflow:hidden; }
  th, td { padding:8px 12px; text-align:left; border-bottom:1px solid #21262d; }
  th { color:#8b949e; font-weight:600; font-size:12px; }
  tr:last-child td { border-bottom:none; }
  code { color:#79c0ff; }
  .closed { color:#3fb950; } .open { color:#f85149; } .half-open { color:#d29922; }
  .on { color:#3fb950; } .off { color:#f85149; }
  button { background:#21262d; color:#c9d1d9; border:1px solid #30363d; border-radius:6px; padding:4px 10px; cursor:pointer; }
  button:hover { background:#30363d; }
  #keybar { margin-bottom:16px; }
  #keybar input { background:#0d1117; border:1px solid #30363d; color:#c9d1d9; border-radius:6px; padding:6px 10px; width:320px; }
  #err { color:#f85149; margin:8px 0; }
  .totals { color:#8b949e; margin-top:8px; }
</style>
</head>
<body>
<h1>TokenRoute Dashboard</h1>
<div id="keybar">
  <input id="key" type="password" placeholder="admin key (stored in sessionStorage)">
  <button onclick="saveKey()">Save</button>
</div>
<div id="err"></div>

<h2>API Keys</h2>
<table id="keys"><thead><tr><th>Name</th><th>Key</th><th>RPM</th><th>TPM</th><th>Spent</th><th>Quota</th><th>Enabled</th><th></th></tr></thead><tbody></tbody></table>

<h2>Usage</h2>
<table id="usage"><thead><tr><th>Key</th><th>Requests</th><th>Tokens</th><th>Cost USD</th></tr></thead><tbody></tbody></table>
<div class="totals" id="totals"></div>

<h2>Providers</h2>
<table id="providers"><thead><tr><th>Name</th><th>Priority</th><th>Circuit</th><th>EMA ms</th></tr></thead><tbody></tbody></table>

<script>
const $ = id => document.getElementById(id);
const keyEl = $("key");
keyEl.value = sessionStorage.getItem("adminKey") || "";
const q = new URLSearchParams(location.search).get("key");
if (q) { sessionStorage.setItem("adminKey", q); keyEl.value = q; }

function saveKey() { sessionStorage.setItem("adminKey", keyEl.value); refresh(); }

async function api(path, opts = {}) {
  opts.headers = Object.assign({"X-Admin-Key": sessionStorage.getItem("adminKey") || ""}, opts.headers);
  const r = await fetch(path, opts);
  if (!r.ok) throw new Error(path + " -> " + r.status);
  return r.status === 204 ? null : r.json();
}

function esc(s) { const d = document.createElement("div"); d.textContent = s; return d.innerHTML; }

async function refresh() {
  $("err").textContent = "";
  try {
    const keys = await api("/admin/keys");
    $("keys").querySelector("tbody").innerHTML = (keys.keys || []).map(k =>
      "<tr><td>" + esc(k.name) + "</td><td><code>" + esc(k.key) + "</code></td><td>" +
      (k.rpm || "∞") + "</td><td>" + (k.tpm || "∞") + "</td><td>" + k.spent_tokens +
      "</td><td>" + (k.quota_tokens || "∞") + "</td><td class='" + (k.enabled ? "on" : "off") + "'>" +
      (k.enabled ? "yes" : "no") + "</td><td><button onclick='toggle(" + k.id + "," + !k.enabled + ")'>" +
      (k.enabled ? "Disable" : "Enable") + "</button></td></tr>").join("");

    const usage = await api("/admin/usage");
    $("usage").querySelector("tbody").innerHTML = (usage.keys || []).map(u =>
      "<tr><td>" + esc(u.key_name || u.key_id || "-") + "</td><td>" + u.requests +
      "</td><td>" + u.total_tokens + "</td><td>$" + (u.cost_usd || 0).toFixed(4) + "</td></tr>").join("");
    const t = usage.totals || {};
    $("totals").textContent = "Totals: " + (t.requests || 0) + " requests, " +
      (t.total_tokens || 0) + " tokens, $" + (t.cost_usd || 0).toFixed(4);

    const provs = await api("/admin/providers");
    $("providers").querySelector("tbody").innerHTML = (provs.providers || []).map(p =>
      "<tr><td>" + esc(p.name) + "</td><td>" + p.priority + "</td><td class='" + esc(p.circuit) + "'>" +
      esc(p.circuit) + "</td><td>" + Math.round(p.ema_latency_ms || 0) + "</td></tr>").join("");
  } catch (e) {
    $("err").textContent = e.message + " — check admin key";
  }
}

async function toggle(id, enable) {
  try { await api("/admin/keys/" + id + (enable ? "/enable" : "/disable"), {method: "POST"}); } catch (e) { $("err").textContent = e.message; }
  refresh();
}

refresh();
setInterval(refresh, 5000);
</script>
</body>
</html>`

func (s *srv) adminDashboard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(dashboardPage))
}
