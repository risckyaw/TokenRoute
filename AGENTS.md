# TokenRoute — Agent Guide

## Overview

Production AI gateway / LLM router in Go. Single binary, [OI]-compatible API,
routes virtual model names to upstream providers by priority.

- Go version: 1.23
- Module: github.com/Jarvisagentic/tokenroute
- Dependencies (only these, never add more without discussion):
  - github.com/go-chi/chi/v5 (HTTP mux)
  - gopkg.in/yaml.v3 (config)
  - modernc.org/sqlite (usage log DB, driver name "sqlite")

## Commands

```bash
go build ./...          # build all packages
go vet ./...            # static checks
go test ./...           # run tests
go run ./cmd/tokenroute serve --config config.yaml   # run locally
```

Config: copy `config.example.yaml` to `config.yaml`, set env vars for keys.

## Conventions

- Package layout:
  - `cmd/tokenroute` — main, flags, signals, hot reload
  - `internal/config` — YAML load + validate + `${VAR}` env expansion
  - `internal/provider` — Provider interface + Request type
  - `internal/provider/openai` — [OI]-compatible provider (covers DeepSeek,
    OpenRouter, Ollama, etc.)
  - `internal/provider/anthropic` — Anthropic Messages API adapter ([OI] <->
    Anthropic translation at the boundary, incl. SSE stream translation)
  - `internal/provider/gemini` — Gemini generateContent adapter ([OI] <->
    Gemini translation, incl. SSE stream translation)
  - `internal/router` — virtual-model → candidate mapping, routing strategies,
    per-provider circuit breakers (`circuit.go`), EMA latency tracking
  - `internal/server` — chi HTTP handlers, request IDs, failover loop, usage
    logging, virtual-key auth middleware, admin API (`admin.go`), embedded
    admin dashboard (`dashboard.go`, GET /admin/)
  - `internal/usage` — SQLite usage log + SSE token tracking + pricing;
    `OpenDB` opens the shared DB handle used by both usage and auth
  - `internal/auth` — virtual API keys (SQLite `api_keys` table)
  - `internal/ratelimit` — per-key token buckets (RPM/TPM)
- Passthrough design: the gateway rewrites only the top-level `model` field
  (plus `stream_options.include_usage=true` on streaming requests, merged not
  clobbered), forwards the raw body otherwise, and relays the raw upstream
  response (including error statuses) unbuffered so SSE streaming works.
- Routing: per-route `strategy` (priority|round_robin|least_latency|weighted|
  cost) orders candidates; candidates whose circuit breaker is open are
  skipped (half-open allows one probe).
- Failover: transport errors and statuses 429/500/502/503/504 try the next
  candidate (each tried at most once per request); other statuses (200, 400,
  401, 403, 404...) are relayed as-is with no failover. If all candidates
  fail with retryable upstream statuses, the last upstream response is
  relayed as-is; if all fail with transport errors, a 502 upstream_error is
  returned.
- Roadmap (all phases complete):
  - Phase 1: priority routing, passthrough, hot reload
  - Phase 2: usage/token logging, pricing, request IDs
  - Phase 3: routing strategies, failover, circuit breaker
  - Phase 4: virtual API keys, per-key rate limiting, quotas, admin API
  - Phase 5: Anthropic + Gemini adapters, admin dashboard, Docker, README

## Boundaries

- NEVER commit `.env`, API keys, tokens, or any secrets.
- Secrets only via environment variables (`${VAR}` placeholders in YAML).
- No new third-party dependencies.
- No git commands from agents.

## Configuration

`config.yaml` (see `config.example.yaml`):

- `listen` — address, default `:8400`
- `usage_db` — SQLite path for usage logs AND virtual API keys, default `data/usage.db`
- `admin_key` — admin API key (`${GATEWAY_ADMIN_KEY}`); empty disables `/admin` (503)
- `prices` — map of upstream model name → `{prompt_per_1m, completion_per_1m}`
  USD per 1M tokens; cost logged only for priced models
- `providers[]` — `name`, `type` (`openai`|`anthropic`|`gemini`), `base_url`, `api_key`
  (`${VAR}` supported, may be empty e.g. Ollama), `priority` (lower = more
  preferred), `timeout_ms`, optional `circuit: {failure_threshold,
  cooldown_ms}` (defaults 3 / 30000)
- `routes[]` — virtual `model` name, optional `strategy` (default
  `priority`), ordered `candidates` (`provider` + upstream `model` +
  optional `weight` used by the weighted strategy)

Every response carries an `X-Request-Id` header; per-request JSON logs
include tokens/cost. `GET /v1/usage/recent?limit=N` (default 50, max 500)
returns recent usage entries.

Requests for unlisted models pass through to the highest-priority provider
with the model name unchanged.

## Virtual API keys + admin API (Phase 4)

Clients authenticate with `Authorization: Bearer gw-...`; keys live in the
`api_keys` table of the usage DB. Per-key limits: `rpm`, `tpm` (token
buckets, 0 = unlimited), `quota_tokens` (lifetime, 0 = unlimited),
`allowed_models`, `expires_at`, `enabled`. Errors: 401 `invalid_api_key`,
403 `model_not_allowed` / `quota_exceeded`, 429 `rate_limit_exceeded`.
`/healthz` stays unauthenticated.

Admin routes (header `X-Admin-Key` = `admin_key`, or `?key=` query param
for the dashboard): `GET /admin/` (embedded dashboard UI), `POST/GET
/admin/keys`, `POST /admin/keys/{id}/disable|enable`, `DELETE
/admin/keys/{id}`, `GET /admin/usage`, `GET /admin/providers`,
`POST /admin/providers/{name}/circuit/reset`. Full key string returned only
by create; list masks it.

## Adapters (Phase 5)

- `anthropic`: POST {base_url}/messages (default https://api.anthropic.com/v1),
  headers `x-api-key` + `anthropic-version: 2023-06-01`. System messages join
  into top-level `system`; `max_tokens` defaults to 1024. Non-stream and SSE
  responses translated to [OI] shape (usage chunk with prompt_tokens/
  completion_tokens/total_tokens so SSEUsageTracker works unchanged).
  `Models()` returns a static list.
- `gemini`: POST {base_url}/models/{model}:generateContent?key=... (stream:
  :streamGenerateContent?alt=sse&key=...; default base
  https://generativelanguage.googleapis.com/v1beta). system ->
  systemInstruction, assistant -> model. `Models()` filters names containing
  "gemini" with the `models/` prefix stripped.

## Docker

```bash
docker build -t tokenroute .
docker run -p 8400:8400 -v "$PWD/config.yaml:/config/config.yaml:ro" tokenroute
```

Multi-stage (golang:1.23-alpine -> distroless static-debian12:nonroot),
`EXPOSE 8400`, entrypoint `serve --config /config/config.yaml`.

## Error Handling

- Client errors (missing/invalid `model`, no route): 400 with
  `{"error":{"message":...,"type":"invalid_request_error"}}`
- Upstream transport errors: next candidate tried; if all fail, 502 with
  `{"error":{"message":"upstream error: ...","type":"upstream_error"}}`
- Upstream retryable HTTP errors (429/500/502/503/504): next candidate
  tried; if all fail, the last upstream response is relayed as-is.
- Other upstream HTTP errors (400/401/403/404...): relayed as-is, unmodified,
  no failover.
- Config load/validate failures: fatal at startup; logged and ignored on
  SIGHUP reload (previous config stays active).

## Troubleshooting

- 502 upstream error — check provider `base_url`, network, API key env var
  is set (empty expansion means the env var is missing).
- Empty `/v1/models` — providers unreachable; endpoint is best-effort and
  ignores per-provider failures.
- Reload not applying — send SIGHUP (`kill -HUP <pid>`); watch stderr JSON
  logs for `config reloaded` or `reload config` errors.
- Streaming not flushing — gateway flushes per write; check client/proxy in
  between isn't buffering.
