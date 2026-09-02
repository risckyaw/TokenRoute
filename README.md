# TokenRoute

Production AI gateway / LLM router in Go. Single static binary exposing an
[OI]-compatible API (`/v1/chat/completions`, `/v1/models`) that routes
virtual model names to upstream providers — including Anthropic and Gemini,
which are translated to/from [OI] format at the boundary so clients always
speak one API.

## Features

- **Passthrough proxy** — only the top-level `model` field is rewritten;
  raw bodies forwarded, raw upstream responses (incl. errors) relayed, SSE
  streamed unbuffered.
- **Routing strategies** — priority, round_robin, least_latency, weighted,
  cost, lkgp (last-known-good provider), headroom (fewest requests in the
  last 60s first), fusion (race first two candidates, fastest/cheapest 200
  wins), p2c (power-of-2-choices), reset_aware (quota window resetting
  soonest first), fill_first (exhaust one candidate's quota before the
  next), auto (composite score: health × latency × cost × quota headroom),
  lowest_usage (fewest tokens in the current minute first) — per virtual
  model.
- **Percent circuit threshold** — optional `circuit: {mode: percent,
  failure_percent, min_requests}` trips on a failure ratio in the current
  minute instead of consecutive failures (LiteLLM semantics).
- **Per-exception failure budgets** — `circuit: {allowed_fails: {timeout: 10,
  rate_limit: 3, ...}}` tolerates N consecutive failures per kind before
  opening; unlisted kinds keep the global threshold, auth/permission stay
  instant-open unless listed.
- **Tag-based routing** — candidates carry `tags`; clients send
  `X-Route-Tags: vision,cheap,!beta,&us` (plain = subset match, `!`
  excludes, `&` requires) to filter candidates per request.
- **Cross-model fallback** — `fallback_routes: [other-model]` on a route
  tries another virtual model's candidates when every candidate of the
  route fails retryably (max 3 route hops, cycle-safe; client errors never
  trigger it).
- **Prompt-cache affinity** — `prompt_cache_affinity: true` (global or per
  route) pins a cacheable prompt prefix (explicit `cache_control`, or
  system+first-user ≥ 1024 bytes) to the provider+model that served it for
  1h, so provider-side prompt caches hit; decision header gains
  `;affinity=hit`. Extended per-route form `affinity: {enabled, key_header,
  ttl_ms, skip_retry_on_failure}` pins by a request header's value hash
  instead (session/thread ids; decision header `;aff=h` vs `;aff=k` for
  prefix) and `skip_retry_on_failure` relays a pinned failure to the client
  instead of failing over — the pinned channel holds per-session state
  (new-api channel affinity port).
- **Background health checks** — `health_check: {enabled, interval_ms,
  model}` per provider (or global) probes with a minimal completion so
  circuit state and latency EMA stay warm; never touches usage log or quota
  ledger.
- **Failure classification** — 429s carrying balance/credit wording are
  quota_exhausted (15min model lock), not rate limits; 401/403 open the
  circuit immediately; client aborts never count as provider failures.
- **Escalating circuit breakers** — DEGRADED warning band at 60% of
  threshold (threshold ≥ 4); open cooldown doubles after 3 failed probe
  cycles (16x cap).
- **Provider quota ledger** — optional `quota_token_limit` per provider:
  pre-request budget windows (actual usage recorded post-response) read by
  reset_aware/fill_first/auto.
- **Free-tier catalog** — `free_tier:` entries seed monthly token budgets
  into the quota ledger so quota-aware strategies prefer live free tiers.
- **Pricing sync** — LiteLLM community catalog fills price gaps every 24h
  (`pricing_sync: "off"` disables); config `prices:` always win.
- **Global model aliases** — `aliases:` map client-facing names to virtual
  route models, resolved before route lookup (body model rewritten).
- **Response cache** — optional in-memory cache for non-stream chat
  completions (`cache:` config block); hits return `X-TokenRoute-Cache: HIT`
  with zero cost, tracked in the usage log.
- **Embeddings** — `/v1/embeddings` passthrough with the same routing,
  failover, keys, and metering as chat completions.
- **Per-request budgets** — `X-Max-Cost-USD` header rejects requests whose
  worst-case estimate exceeds the budget (402 `budget_exceeded`); actual
  overruns flagged in the usage log.
- **Per-key USD budgets** — `budget_usd` on virtual keys; cost accumulated
  in `spent_usd` after each request, exhausted keys get 402
  `budget_exceeded`.
- **Context-window guard** — optional `context_tokens` per priced model;
  oversized prompts skip that candidate (400 `context_length_exceeded`
  when none fit). Prompt tokens estimated by a weighted char-class
  heuristic calibrated per provider family (openai/claude/gemini weights;
  CJK-heavy prompts estimate ~3–5× higher than len/4, so oversized CJK
  prompts are caught correctly).
- **Prometheus metrics** — `GET /metrics` (no auth, both listeners):
  request/token/cache counters, latency histogram, circuit-open gauge.
- **Per-request timeout** — `X-Timeout-Ms` header overrides the provider
  timeout for one request (capped at 600000).
- **Provider key pools** — multiple API keys per provider, round-robin with
  60s cooldown on upstream 401/429.
- **Failover + circuit breakers** — retryable statuses (429/5xx) and
  transport errors fall through to the next candidate; per-provider
  breakers with half-open probes; 429 `Retry-After` opens the breaker for
  the hinted duration; 429/404 lock out that provider+model for 30s.
- **Configurable retry/disable policy** — optional `retry_policy:` (new-api
  port): `retry_status_ranges` + `never_retry` decide failover,
  `disable_status_ranges` + `disable_keywords` (case-insensitive body
  match; balance/credit/quota/insufficient wording → quota_exhausted 15min
  lock, else auth-class instant circuit open). Unset = built-in behavior
  exactly.
- **Observability headers** — every chat response carries
  `X-TokenRoute-Decision` (provider/model/strategy/attempts); non-stream
  200s also carry token counts and `X-TokenRoute-Cost-USD` when priced.
- **Usage logging + pricing** — SQLite log with tokens per request, SSE
  token tracking, per-model USD pricing.
- **Expression pricing** — `prices: {model: {expr: "..."}}` (new-api
  billingexpr port): one expression per model with variables `p`, `c`, `len`,
  `cr`, `cc`, `cc1h`, `img`, `ai`, `ao` (coefficients = USD/1M, result
  auto-divided by 1e6); `tier(name, cost)` records the tier in the usage log
  (`price_tier` column). Wins over flat rates for chat cost; invalid
  expressions fail config load. Cached-token pricing needs no separate knob —
  price `cr` explicitly: `p*2 + c*8 + cr*0.2`. [OI] semantics subtract
  referenced detail tokens from `p`; anthropic providers (text-only
  `input_tokens`) skip subtraction and get `len = p+cr+cc`.
- **Virtual API keys** — per-key RPM/TPM token buckets, lifetime quotas,
  model allowlists, expiry; full admin API. Rate limits surfaced via
  `RateLimit-Limit`/`RateLimit-Remaining`/`RateLimit-Reset` (RPM) and
  `X-RateLimit-Token-*` (TPM) response headers; 429s carry `Retry-After` +
  `RateLimit-Reset` computed from the bucket's refill rate (seconds until
  one token is available, Kong-style) instead of a flat 60.
  Optional `limit_by_header` on a key derives the rate-limit identity from
  a request header (e.g. `X-User-Id`) so one key serves many end-users
  with isolated buckets (Kong-style `limit_by`). Optional `daily_quota`
  caps requests per UTC day (persisted, atomic rollover); usage surfaced
  via `X-RateLimit-Daily-Limit/Remaining`, exhaustion returns 429
  `daily_quota_exceeded` with `Retry-After` until midnight UTC.
- **Correlation IDs** — every request gets `X-Correlation-ID` (generated
  when absent, propagated upstream, echoed downstream).
- **Request size limiting** — Content-Length pre-check rejects oversized
  bodies before any read (413; 417 for `Expect: 100-continue`); chunked
  bodies still bounded by `MaxBytesReader` at parse time.
- **Provider adapters** — [OI]-compatible (DeepSeek, OpenRouter, Ollama...),
  Anthropic Messages API, Gemini generateContent — all exposed as [OI].
- **Admin dashboard** — single-page dark UI at `/admin/` (keys, usage,
  provider circuits), 5s auto-refresh.
- **Hot reload** — SIGHUP reloads config without dropping requests.
- **Distroless Docker image** — multi-stage, nonroot, CGO-free.
- **Group-based access** — keys and route candidates carry `groups`; a key only
  reaches candidates sharing a group (empty side = wildcard, else 403
  `group_forbidden`).
- **Group ratio** — `group_ratio: {vip: 1.0, free: 1.2}` multiplies request
  cost by the ratios of the key∩candidate group intersection (product when
  several match; empty intersection = 1.0); cost-only, routing unaffected
  (new-api group_ratio port).
- **Model mapping** — per-provider `model_mapping` rewrites route models to
  upstream aliases; decision header and usage log record the final model.
- **Request overrides** — per-provider `param_override` / `param_delete` /
  `header_override` / `header_pass` (globs, case-insensitive, resurrecting
  blocklisted client headers) and per-candidate `param_override` applied
  after the provider's (candidate wins). Set-only; JSON object bodies only,
  non-objects pass through untouched (new-api override port, reduced).
- **Upstream hardening** — `response_header_timeout_ms` bounds the wait for
  upstream headers without cutting streams; `stream_idle_timeout_ms` cuts SSE
  streams that go silent.
- **Channel test** — `POST /admin/providers/{name}/test` sends a minimal
  completion through the provider (status + latency), wired to a Test button
  in the dashboard.

## Quickstart

```bash
cp config.example.yaml config.yaml
export GATEWAY_ADMIN_KEY=change-me
export DEEPSEEK_API_KEY=...
go run ./cmd/tokenroute serve --config config.yaml
```

Create your first virtual key:

```bash
curl -s -X POST localhost:8400/admin/keys \
  -H "X-Admin-Key: $GATEWAY_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"dev","rpm":60,"tpm":100000}'
# -> {"id":1,"key":"gw-...","name":"dev",...}  (full key shown only here)
```

Chat completion (streaming):

```bash
curl -N localhost:8400/v1/chat/completions \
  -H "Authorization: Bearer gw-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

## Routing strategies

| Strategy       | Behavior                                             | When to use |
|----------------|------------------------------------------------------|-------------|
| `priority`     | First candidate by provider priority (default)       | Preferred provider with fallback |
| `round_robin`  | Rotates the starting candidate each request; `sticky: N` rotates only every N requests | Even spread across equivalent providers (sticky keeps prompt caches warm) |
| `least_latency`| Lowest decayed-EWMA latency first; unseen providers seed with the mean of known latencies (slow-start, Kong-style) | Latency-sensitive traffic |
| `peak_ewma`    | Draws 2 random candidates, lower decayed-ewma/weight score first; rest priority order | Latency+weight balancing without global sorts |
| `weighted`     | Weighted-random first pick (weight default 1); rest stay in priority order | Gradual traffic shifts / canary |
| `cost`         | Lowest prompt+completion price first; unpriced last  | Cost optimization |
| `lkgp`         | Last successfully serving provider first; failure reverts to priority | Sticky good-provider preference |
| `headroom`     | Fewest requests in the last 60s first; ties -> priority | Load-aware spreading |
| `least_connections` | Fewest live in-flight upstream requests first, scored (inflight+1)/weight; ties -> priority | Long-running SSE streams; true live load (Kong) |
| `consistent_hash` | Stateless ring: candidates sorted lexically, start at fnv32(value)%len, walk forward; needs `hash_on` | Sticky sessions without cache state (Kong) |
| `lowest_usage` | Fewest observed tokens in the current minute first; unseen first, ties -> priority | Token-budget-aware spreading |

Latency EWMA now decays lazily with a 10s time constant (Kong
`balancer/latency.lua`): stale latency readings fade instead of pinning a
provider forever. Slow-start: a provider with no data is scored with the
MEAN of providers that do (no more thundering-herd onto freshly added or
circuit-recovered providers). This intentionally changes `least_latency`,
`p2c`/`peak_ewma`, and `auto` ordering.

Failover applies to all strategies: 429/500/502/503/504 and transport
errors try the next candidate (each tried at most once per request). Other
statuses (400/401/403/404...) are relayed as-is. Circuit breakers skip
failing providers (open) and allow one probe while half-open.

## Per-request budgets

Send `X-Max-Cost-USD: <float>` on chat requests. The gateway estimates
worst-case cost as `max_tokens` (default 4096) × (prompt + completion
price) of the first serving candidate; when the estimate exceeds the
budget the request is rejected with 402 `budget_exceeded`. Unpriced models
always pass. When the *actual* cost exceeds the budget, the usage log row
is flagged `budget_exceeded` (visible in `/admin/usage/export`).

## Context-window guard

Add `context_tokens` to a price entry to enforce the model's context
window. The gateway estimates prompt tokens as len(messages JSON)/4
(len(input)/4 for embeddings) and skips candidates whose window is too
small; when every candidate is rejected the client gets 400
`context_length_exceeded`. Entries without `context_tokens` (or 0) are
never rejected.

## Metrics

`GET /metrics` exposes Prometheus text format on both the public and
admin listeners (no auth): `tokenroute_requests_total{key,provider,model,
status_class}`, `tokenroute_tokens_total{key,provider,kind}`,
`tokenroute_cache_hits_total`, `tokenroute_latency_seconds{provider}`
histogram, `tokenroute_circuit_open{provider}` gauge,
`tokenroute_inflight{provider}` gauge (live upstream requests).

## Per-request timeout

`X-Timeout-Ms: <int>` wraps the upstream call in a context deadline for
that request only (capped at 600000 ms). Exceeding it fails over like a
transport error (502 when all candidates time out).

## Tag-based routing

Label route candidates with `tags: [vision, us]`, then send
`X-Route-Tags: vision,!beta,&us` on a request (comma-separated): plain tags
must be a subset of the candidate's tags, `!tag` excludes candidates
carrying it, `&tag` requires it. Empty candidate tags match everything
except `&` requirements; no header = all candidates pass. The header is
gateway-local (never forwarded upstream).

## Consistent hashing

`strategy: consistent_hash` with `hash_on: "header:X-Session-Id"` (any
header) or `hash_on: "key"` (the virtual API key) maps each request value to
a candidate deterministically — zero state, survives restarts. The ring is
the route's candidates sorted lexically by (provider, model); the request
starts at `fnv32(value) % len` and walks forward, skipping circuit-open or
locked candidates. Missing hash value falls back to priority order. Affinity
pins (when configured) still pre-empt strategies as before.

## Upstream quota observations

Providers that return rate-limit headers (`x-ratelimit-remaining-tokens`,
`x-ratelimit-reset-tokens`; Anthropic's `anthropic-ratelimit-*` variants too)
feed the quota ledger automatically: after every upstream response the
gateway records the observed remaining-token budget and reset time. For 60s
the quota-aware strategies (`reset_aware`, `fill_first`, `auto`) prefer this
provider-signalled state over local accounting (Kong response-ratelimiting
style). Missing or invalid headers are ignored — zero config, zero cost.

## Capability-aware routing

Requests carrying non-text content are ranked onto models that can actually
accept them (9router `detectRequiredCapabilities`). The gateway scans the chat
body's content blocks — `image_url`/`image`/`input_image` → image,
`input_audio`/`audio_url`/`audio` → audio, `file`/`document`/`input_file` → the
mime type found in `media_type`/`mime_type` or a `data:<mime>;base64,` prefix
(`image/*`, `application/pdf`, `audio/*`, `video/*`; a file block with no
discoverable mime falls back to PDF) — then stable-sorts candidates whose
models cover every requirement ahead of the rest. Requirements are listed in
the decision header as `;caps=image,pdf`.

Modality data comes from the models.dev catalog sync, so `model_catalog: off`
disables the reordering (detection still runs, and the marker is still set).
Candidates are never dropped: a model with no catalog entry can still serve
text-only requests normally and is only ranked last when media is required —
a route whose models are all uncatalogued behaves exactly as before. Tiering
is applied after the strategy and is stable, so it overrides
cost/latency/quota preference (a text-only model is a guaranteed 400) while
preserving the strategy's order within each tier.

## Sticky round-robin

`strategy: round_robin` with `sticky: N` (9router `getRotatedModels`) advances
the rotation only after N consecutive requests instead of every request, so a
provider's prompt cache is not invalidated by a rotation on the very next call.
The cursor walks the route's **original** candidate list, so circuit-open,
locked, or tag-filtered candidates are skipped forward in order without
shifting the cycle; a cursor past every survivor wraps to the first. `sticky: 1`
(the default) is the legacy per-request rotation, and `sticky` on any other
strategy fails config load. State is per-route and in-memory: a SIGHUP reload
rebuilds routes, resetting the cursor.

## Earliest Retry-After on terminal 429

When every candidate is rate-limited the gateway still relays the last
upstream 429 body verbatim, but rewrites `Retry-After` to the **earliest**
known reset across the whole route (9router `combo.js`): a sibling candidate
whose limit clears in 30s beats the relayed candidate's 10-minute cooldown.
The instants come from upstream 429 rate-limit/`Retry-After` headers (kept as
model locks), the quota ledger's window reset for candidates with no budget
left, and circuit-breaker probe times — including candidates that were
filtered out before the failover loop ran. `X-TokenRoute-Retry-After-Source:
upstream|quota|circuit` names the winning source; when the relayed value is
already the soonest it is left untouched and no marker is set. Non-429
terminal failures are never modified.

## Configuration

`config.yaml` — secrets only via `${ENV_VAR}` placeholders:

| Key | Description |
|---|---|
| `listen` | Address, default `:8400` |
| `usage_db` | SQLite path (usage logs + API keys), default `data/usage.db` |
| `admin_key` | Admin API key (`${GATEWAY_ADMIN_KEY}`); empty disables `/admin` |
| `max_body_mb` | Max request body size in MB, default 10 |
| `prices` | Map of upstream model → `{prompt_per_1m, completion_per_1m, embed_per_1m, context_tokens}` USD per 1M tokens; `context_tokens` enables the context-window guard; optional `expr` (expression pricing, see Features) wins over flat rates for chat cost |
| `providers[]` | `name`, `type` (`openai`/`anthropic`/`gemini`), `base_url`, `api_key` (`${VAR}`, may be empty e.g. Ollama), optional `api_keys` pool (`${VAR}` each; round-robin, 60s cooldown on 401/429), `priority` (lower = preferred), `timeout_ms`, optional `circuit: {failure_threshold, cooldown_ms, auto_disable_after}` (defaults 3/30000/3; after N circuit trips the provider is disabled until re-enabled via admin; also `mode: percent` + `failure_percent`/`min_requests`, and `allowed_fails: {kind: n}` per-exception budgets), optional `health_check: {enabled, interval_ms, model}` (background probes; default disabled), optional `param_override` / `param_delete` / `header_override` / `header_pass` (set-only request overrides) |
| `routes[]` | Virtual `model`, optional `strategy`, optional `multiplier` (cost multiplier, default 1.0), optional `fallback_routes` (other virtual models tried when all candidates fail retryably), optional `prompt_cache_affinity` (pin cacheable prefixes to the serving provider, 1h), optional `affinity: {enabled, key_header, ttl_ms, skip_retry_on_failure}` (header-keyed pinning; wins over the shorthand), ordered `candidates` (`provider`, upstream `model`, optional `weight`, optional `groups`, optional `tags` for `X-Route-Tags` filtering, optional `param_override`) |
| `prompt_cache_affinity` | Global default for per-route prefix pinning, default false |
| `health_check` | Global background-probe default `{enabled, interval_ms}`; per-provider block wins |
| `retry_policy` | Optional `{retry_status_ranges, never_retry, disable_status_ranges, disable_keywords}` failover/disable overrides (unset = built-in) |
| `group_ratio` | Optional map group → cost multiplier; applied on key∩candidate group intersection, cost-only |

Provider types:

- `openai` — any [OI]-compatible endpoint (`POST {base_url}/chat/completions`).
- `anthropic` — Messages API at `{base_url}/messages` (default
  `https://api.anthropic.com/v1`); requests translated (system extraction,
  `max_tokens` default 1024), responses + SSE streams emitted as [OI].
- `gemini` — `POST {base_url}/models/{model}:generateContent` (default
  `https://generativelanguage.googleapis.com/v1beta`); streaming uses
  `:streamGenerateContent?alt=sse`.

Requests for unlisted models pass through to providers in priority order
with the model name unchanged. Every response carries `X-Request-Id`.

## Admin API

Header `X-Admin-Key: <admin_key>` on every call:

| Route | Description |
|---|---|
| `POST /admin/keys` | Create key `{name, rpm, tpm, model_rpm, quota_tokens, budget_usd, allowed_models, expires_at}` — full key returned only here; `model_rpm` = per-(key,model) RPM (0 = use global `rpm`) |
| `GET /admin/keys` | List keys (masked) |
| `POST /admin/keys/{id}/disable` / `/enable` | Toggle key |
| `DELETE /admin/keys/{id}` | Delete key |
| `GET /admin/usage` | Per-key aggregates + totals |
| `GET /admin/usage/export?format=csv&from&to` | Stream usage logs as CSV (RFC3339 range, default last 24h) |
| `GET /admin/providers` | Circuit state + EMA latency + `disabled` flag per provider |
| `POST /admin/providers/{name}/circuit/reset` | Reset circuit breaker |
| `POST /admin/providers/{name}/disable` / `/enable` | Disable/enable provider channel (enable resets breaker + auto-disable counter) |

Client-facing: `GET /v1/models`, `GET /v1/usage/recent?limit=N` (≤500),
`GET /healthz` (unauthenticated).

## Dashboard

`GET /admin/` serves the self-contained dashboard. Open
`http://localhost:8400/admin/?key=<admin_key>` in a browser (the key is
stored in sessionStorage; header `X-Admin-Key` also works). Panels: API
keys (enable/disable inline), usage aggregates, provider circuit states.

## Docker

```bash
docker build -t tokenroute .
docker run -p 8400:8400 \
  -e GATEWAY_ADMIN_KEY=change-me -e DEEPSEEK_API_KEY=... \
  -v "$PWD/config.yaml:/config/config.yaml:ro" \
  -v "$PWD/data:/data" \
  tokenroute
```

Multi-stage build, distroless `static-debian12:nonroot` runtime, `EXPOSE 8400`.

## Development

```bash
go build ./...          # build
go vet ./...            # static checks
go test ./...           # tests
GOOS=linux CGO_ENABLED=0 go build ./cmd/tokenroute   # container-equivalent build
```

## Project layout

```
cmd/tokenroute            main, flags, signals, hot reload
internal/config        YAML load + validate + ${VAR} expansion
internal/provider      Provider interface + Request type
internal/provider/openai     [OI]-compatible provider
internal/provider/anthropic  Anthropic Messages adapter
internal/provider/gemini     Gemini generateContent adapter
internal/router        strategies, circuit breakers, EMA latency
internal/server        chi handlers, failover loop, admin API, dashboard
internal/usage         SQLite usage log, SSE token tracking, pricing
internal/auth          virtual API keys (SQLite api_keys table)
internal/ratelimit     per-key RPM/TPM token buckets
```
