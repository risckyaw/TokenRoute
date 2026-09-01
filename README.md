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
  cost, lkgp (last-known-good provider, per virtual model).
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

Failover applies to all strategies: 429/500/502/503/504 and transport
errors try the next candidate (each tried at most once per request). Other
statuses (400/401/403/404...) are relayed as-is. Circuit breakers skip
failing providers (open) and allow one probe while half-open.

## Configuration

`config.yaml` — secrets only via `${ENV_VAR}` placeholders:

| Key | Description |
|---|---|
| `listen` | Address, default `:8400` |
| `usage_db` | SQLite path (usage logs + API keys), default `data/usage.db` |
| `admin_key` | Admin API key (`${GATEWAY_ADMIN_KEY}`); empty disables `/admin` |
| `prices` | Map of upstream model → `{prompt_per_1m, completion_per_1m}` USD per 1M tokens |
| `providers[]` | `name`, `type` (`openai`/`anthropic`/`gemini`), `base_url`, `api_key` (`${VAR}`, may be empty e.g. Ollama), `priority` (lower = preferred), `timeout_ms`, optional `circuit: {failure_threshold, cooldown_ms}` (defaults 3/30000) |
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
| `POST /admin/keys` | Create key `{name, rpm, tpm, quota_tokens, allowed_models, expires_at}` — full key returned only here |
| `GET /admin/keys` | List keys (masked) |
| `POST /admin/keys/{id}/disable` / `/enable` | Toggle key |
| `DELETE /admin/keys/{id}` | Delete key |
| `GET /admin/usage` | Per-key aggregates + totals |
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
