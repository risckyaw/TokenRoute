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
  wins), per virtual model.
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
  when none fit).
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
- **Observability headers** — every chat response carries
  `X-TokenRoute-Decision` (provider/model/strategy/attempts); non-stream
  200s also carry token counts and `X-TokenRoute-Cost-USD` when priced.
- **Usage logging + pricing** — SQLite log with tokens per request, SSE
  token tracking, per-model USD pricing.
- **Virtual API keys** — per-key RPM/TPM token buckets, lifetime quotas,
  model allowlists, expiry; full admin API.
- **Provider adapters** — [OI]-compatible (DeepSeek, OpenRouter, Ollama...),
  Anthropic Messages API, Gemini generateContent — all exposed as [OI].
- **Admin dashboard** — single-page dark UI at `/admin/` (keys, usage,
  provider circuits), 5s auto-refresh.
- **Hot reload** — SIGHUP reloads config without dropping requests.
- **Distroless Docker image** — multi-stage, nonroot, CGO-free.

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
| `round_robin`  | Rotates the starting candidate each request          | Even spread across equivalent providers |
| `least_latency`| Lowest EMA latency first; unseen providers first     | Latency-sensitive traffic |
| `weighted`     | Weighted-random first pick (weight default 1); rest stay in priority order | Gradual traffic shifts / canary |
| `cost`         | Lowest prompt+completion price first; unpriced last  | Cost optimization |
| `lkgp`         | Last successfully serving provider first; failure reverts to priority | Sticky good-provider preference |
| `headroom`     | Fewest requests in the last 60s first; ties -> priority | Load-aware spreading |

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
histogram, `tokenroute_circuit_open{provider}` gauge.

## Per-request timeout

`X-Timeout-Ms: <int>` wraps the upstream call in a context deadline for
that request only (capped at 600000 ms). Exceeding it fails over like a
transport error (502 when all candidates time out).

## Configuration

`config.yaml` — secrets only via `${ENV_VAR}` placeholders:

| Key | Description |
|---|---|
| `listen` | Address, default `:8400` |
| `usage_db` | SQLite path (usage logs + API keys), default `data/usage.db` |
| `admin_key` | Admin API key (`${GATEWAY_ADMIN_KEY}`); empty disables `/admin` |
| `prices` | Map of upstream model → `{prompt_per_1m, completion_per_1m, embed_per_1m, context_tokens}` USD per 1M tokens; `context_tokens` enables the context-window guard |
| `providers[]` | `name`, `type` (`openai`/`anthropic`/`gemini`), `base_url`, `api_key` (`${VAR}`, may be empty e.g. Ollama), optional `api_keys` pool (`${VAR}` each; round-robin, 60s cooldown on 401/429), `priority` (lower = preferred), `timeout_ms`, optional `circuit: {failure_threshold, cooldown_ms}` (defaults 3/30000) |
| `routes[]` | Virtual `model`, optional `strategy`, ordered `candidates` (`provider`, upstream `model`, optional `weight`) |

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
| `POST /admin/keys` | Create key `{name, rpm, tpm, quota_tokens, budget_usd, allowed_models, expires_at}` — full key returned only here |
| `GET /admin/keys` | List keys (masked) |
| `POST /admin/keys/{id}/disable` / `/enable` | Toggle key |
| `DELETE /admin/keys/{id}` | Delete key |
| `GET /admin/usage` | Per-key aggregates + totals |
| `GET /admin/usage/export?format=csv&from&to` | Stream usage logs as CSV (RFC3339 range, default last 24h) |
| `GET /admin/providers` | Circuit state + EMA latency per provider |
| `POST /admin/providers/{name}/circuit/reset` | Reset circuit breaker |

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
