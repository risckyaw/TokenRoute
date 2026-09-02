# Configuration API and Settings UI Design

Date: 2026-09-03
Status: Proposed

## 1. Objective

Turn TokenRoute's existing admin dashboard from an operations-only console into a complete configuration control plane without replacing `config.yaml` as the source of truth.

The finished dashboard must expose every supported configuration field, preserve YAML comments and ordering, never disclose resolved secrets, detect concurrent edits, validate before writing, save safely, and apply hot-reloadable changes without interrupting active requests.

## 2. Decisions

- Keep `config.yaml` as the sole persistent configuration source.
- Use the Nested Settings layout.
- Expose all configuration fields through structured forms with progressive disclosure.
- Include a Raw YAML escape hatch.
- Use full-document configuration reads and writes guarded by revision hashes.
- Preserve comments and ordering through `yaml.Node` merges.
- Use `Validate -> review diff -> confirm -> atomic save/reload`.
- Show environment-variable references, never resolved secret values.
- Save bootstrap fields but mark them restart-required and leave their active runtime values unchanged.
- Keep the frontend dependency-free and embedded in the Go binary.

## 3. Alternatives Considered

### 3.1 Persistence

1. YAML source of truth with atomic editing: selected. It preserves current deployment and SIGHUP workflows.
2. Database source of truth: rejected. It creates migration and precedence complexity without a current need.
3. Download-only YAML generator: rejected. It does not provide an operational control plane.

### 3.2 API Shape

1. Full-document GET/PUT with revision hash: selected. It fits atomic validation and diff review.
2. Resource CRUD per provider, route, and policy: rejected. It greatly expands API surface and transactional complexity.
3. JSON Patch: rejected. It is compact but makes validation, conflict handling, and UI state harder.

### 3.3 UI Layout

1. Nested Settings: selected. It scales to the full schema while keeping each page understandable.
2. Entity Workbench: rejected as the primary layout. It is efficient for bulk provider operations but weak for global and deeply nested settings.
3. Schema Explorer: rejected as the primary layout. It closely mirrors YAML but exposes implementation structure too directly for routine operations.

## 4. Architecture

### 4.1 Components

#### `internal/config/store.go`

Owns configuration-file operations:

- Read original bytes and parse a `yaml.Node` tree.
- Compute a SHA-256 revision from exact file bytes.
- Produce sanitized structured JSON and sanitized YAML.
- Merge structured documents into the original YAML AST.
- Restore secret sentinels from the current AST.
- Validate candidate configuration.
- Generate a redacted semantic diff.
- Write backups and perform same-filesystem replacement.
- Serialize writes with a process-local mutex.

This component does not apply runtime state.

#### `internal/config/schema.go`

Builds the form schema from Go configuration types using reflection, plus a small explicit metadata override table.

Derived metadata:

- YAML path
- Scalar, object, list, or map kind
- Numeric/string/boolean input type
- Optionality and presence
- Nested item schema

Explicit overrides provide only behavior reflection cannot infer:

- Navigation section
- Human label and help text
- Enum choices
- Secret classification
- Restart-required classification
- Stable sequence identity
- Common versus advanced visibility
- Conditional visibility by provider type or route strategy

A coverage test fails when a new exported YAML field is added to `Config` or a nested configuration type without being reachable through the generated schema.

#### `internal/server/config_admin.go`

Exposes the configuration endpoints and translates validation failures into field-path errors. It receives a configuration store plus an apply callback; it does not own process lifecycle.

#### Runtime apply coordinator

The existing SIGHUP reload code in `cmd/tokenroute/main.go` becomes a reusable coordinator. Both SIGHUP and the admin configuration API call the same build-and-swap logic.

Admin saves apply only hot-reloadable fields. Before building runtime state, restart-required values are replaced with the currently active values. Their new values remain on disk for the next process restart.

Restart-required paths:

- `listen`
- `admin_listen`
- `usage_db`
- `admin_key`

#### Dashboard assets

Split the current large Go string into embedded static assets:

- `internal/server/web/dashboard.html`
- `internal/server/web/dashboard.css`
- `internal/server/web/dashboard.js`
- `internal/server/dashboard.go` for embedding and handlers

The result remains one dependency-free binary. Static assets contain no private data and may be served without admin authentication; every data endpoint remains authenticated.

## 5. Configuration API

### 5.1 Authentication

- `GET /admin/` may continue accepting `?key=` for the initial dashboard bootstrap.
- All `/admin/config` requests require `X-Admin-Key`.
- Query-string authentication is rejected for every configuration endpoint and every mutating endpoint.
- Secrets, raw credentials, and literal secret values never appear in responses, diffs, logs, or validation errors.

### 5.2 `GET /admin/config`

Response:

```json
{
  "revision": "sha256:...",
  "document": {},
  "raw_yaml": "...",
  "schema": {},
  "restart_required_paths": [
    "listen",
    "admin_listen",
    "usage_db",
    "admin_key"
  ]
}
```

`document` represents only fields present in the YAML, preserving the distinction between omitted values and explicit zero values. The browser preserves object insertion order, but the server remains responsible for final YAML ordering.

`raw_yaml` is rendered from the original AST after secret sanitization.

`schema` is generated by the server so the UI and Go configuration model cannot silently drift.

### 5.3 `POST /admin/config/validate`

Request:

```json
{
  "expected_revision": "sha256:...",
  "mode": "structured",
  "document": {}
}
```

Raw mode uses `"mode":"yaml"` and `"raw_yaml":"..."`.

Response on success:

```json
{
  "valid": true,
  "base_revision": "sha256:...",
  "candidate_revision": "sha256:...",
  "document": {},
  "raw_yaml": "...",
  "diff": [],
  "changed_paths": [],
  "restart_required_paths": [],
  "warnings": []
}
```

The server parses, restores allowed secret sentinels, merges structured changes, performs schema and semantic validation, then returns synchronized structured and raw representations. Validation never writes disk or changes runtime state.

### 5.4 `PUT /admin/config`

Request includes the complete candidate representation plus:

```json
{
  "expected_revision": "sha256:...",
  "candidate_revision": "sha256:..."
}
```

The server:

1. Acquires the configuration write lock.
2. Re-reads the target and verifies `expected_revision`.
3. Rebuilds and revalidates the candidate.
4. Verifies `candidate_revision`.
5. Creates a redacted audit description containing revisions and changed paths only.
6. Creates a backup before replacing the target.
7. Writes and syncs a temporary file in the target directory.
8. Replaces the target without exposing partial contents.
9. Applies hot-reloadable fields through the shared coordinator.
10. Restores the previous file and runtime state if runtime construction fails.
11. Keeps the newest five backups.

Success response:

```json
{
  "saved": true,
  "applied": true,
  "revision": "sha256:...",
  "restart_required": false,
  "restart_required_paths": []
}
```

When bootstrap fields changed, `saved` remains true, `applied` describes the hot subset, and `restart_required` is true.

### 5.5 Errors

- `400`: malformed request or unsupported mode.
- `401`: missing or invalid `X-Admin-Key`.
- `409`: `expected_revision` no longer matches disk.
- `422`: YAML, schema, or semantic validation failed.
- `500`: backup, write, replace, or rollback failure.

Validation errors use stable paths:

```json
{
  "valid": false,
  "errors": [
    {
      "path": "routes[auto].candidates[1].provider",
      "code": "unknown_provider",
      "message": "Provider 'missing' does not exist."
    }
  ]
}
```

A conflict never discards the browser draft. The UI offers reload-and-compare, not blind overwrite.

## 6. YAML Preservation

Existing scalar, mapping, and sequence nodes are updated in place whenever possible. Existing comments, style, anchors, aliases, and key ordering remain attached to their nodes.

Stable sequence identities:

- `providers`: `name`
- `routes`: `model`
- `candidates`: `provider` plus `model`
- `free_tier`: `provider` plus `model`
- `search`: `backend` plus occurrence index
- `failure_rules`: selector (`match` or `status`) plus occurrence index

Existing items are matched by identity. New fields use schema order after the nearest existing sibling. Removed fields are removed intentionally. Reordered items retain their attached comments.

No-op validation and save must reproduce byte-identical YAML. A changed document may normalize only touched scalar formatting; unrelated nodes remain unchanged.

## 7. Secret Handling

Secret fields include:

- `admin_key`
- `providers[].api_key`
- `providers[].api_keys[]`
- `search[].api_key`
- `search[].api_keys[]`
- Future fields explicitly marked secret in schema metadata

Rules:

- Environment references such as `${INFERHUB_API_KEY}` are returned unchanged.
- Environment expansion occurs only when building runtime configuration.
- Literal secret values are returned as an opaque keep-existing sentinel.
- A sentinel is accepted only when its stable structural identity matches the current revision.
- Moving or duplicating a sentinel produces a field error.
- Replacing a literal requires an environment reference; the UI does not accept a new literal secret.
- Diffs show only `secret unchanged`, `secret reference changed`, or `secret removed`.

## 8. Settings Information Architecture

Top-level navigation remains:

- Overview
- Keys
- Providers
- Logs
- Settings

Settings sub-navigation:

### General

- Listener addresses
- Usage database
- Admin authentication reference
- Request body limit
- Model catalog sync
- Pricing sync
- Response cache

### Providers

- Provider identity and adapter type
- Base URL and secret references
- Priority and timeouts
- API-key pool
- Model mapping
- Circuit breaker
- Quota window
- Health check
- Request and header overrides
- Balance probe

The existing Providers operations page remains focused on live state, testing, circuit reset, enable, and disable. An `Edit configuration` action deep-links to the provider's settings form.

### Routes

- Virtual model and strategy
- Multiplier
- Candidates
- Fallback routes
- Tags and groups
- Consistent-hash source
- Sticky round-robin
- Prompt-cache affinity
- Fusion judge

Strategy selection reveals only relevant fields while preserving inactive draft values until save.

### Pricing

- Per-model flat pricing
- Expression pricing
- Context limits
- Free-tier budgets
- Group ratios
- Aliases

### Resilience

- Global and provider health checks
- Retry policy
- Failure rules
- Circuit behavior

### Search

- Ordered backend list
- Secret references and key pools

### Advanced

- Less-common global fields
- Request/header override maps
- Schema-derived fields not assigned another section

### Raw YAML

- Monospace editor using sanitized YAML
- Validate before switching back to structured mode
- Field-path and line/column errors
- No client-side YAML parser

## 9. Interaction Design

- Common fields open by default; advanced groups are collapsed.
- Every input has a visible label and concise help text.
- Search filters by label, YAML path, and help text.
- Collection editors support add, duplicate, reorder, and delete.
- Destructive removals require explicit confirmation inside the pending diff, not an extra modal per row.
- Sticky header shows dirty count, Discard, and Validate.
- Validate opens a redacted diff dialog grouped by section.
- Confirm is enabled only for the exact validated candidate revision.
- Save shows progress, success, restart-required, or rollback state.
- Inline validation occurs on blur for scalar constraints; server validation remains authoritative.
- Keyboard navigation, visible focus, semantic labels, and minimum 44px touch targets are required.
- Motion is limited to 150-200ms state transitions and disabled under `prefers-reduced-motion`.
- Responsive verification targets: 375px, 768px, 1024px, and 1440px.

Visual system remains consistent with the current dark operations console:

- Fira Sans plus Fira Code
- Slate background and panels
- Blue interactive accent
- Green, amber, and red reserved for status
- Dense layout without hiding labels
- No decorative emoji or icon font
- Inline SVG icons with accessible names where needed

## 10. API Key UI Completion

The Keys create form must expose all fields already accepted by the API:

- `name`
- `rpm`
- `tpm`
- `model_rpm`
- `limit_by_header`
- `daily_quota`
- `quota_tokens`
- `budget_usd`
- `allowed_models`
- `groups`
- `expires_at`

The key table must expose relevant limits without forcing every value into visible columns. Use a compact primary row plus expandable details.

## 11. Runtime Semantics

Hot-applied configuration includes providers, routes, prices, aliases, policies, search backends, cache behavior, model catalog behavior, and other fields already supported by the runtime state builder.

Restart-required fields are written but excluded from the live candidate passed to the runtime builder. The dashboard displays both persisted and active values until restart.

Active requests continue against the previous immutable state. New requests use the newly swapped state after successful construction. Failed construction leaves the previous state active.

Provider clients and background workers replaced during reload must be closed or stopped after the state swap. Existing request-scoped references remain valid until completion.

## 12. Validation

Validation occurs in layers:

1. YAML syntax and duplicate-key detection.
2. Unknown-field rejection.
3. Existing `Config.Validate()` checks.
4. Referential checks between providers, routes, aliases, candidates, prices, and fallback routes.
5. Secret sentinel integrity.
6. Runtime-state construction without starting listeners or long-running workers.
7. Restart-required field classification.

Validation returns every independently detectable error in one response where practical.

## 13. Testing

### Configuration store

- No-op round trip is byte-identical.
- Structured changes preserve unrelated comments and ordering.
- Sequence add, remove, edit, and reorder preserve matched item comments.
- Literal secrets are redacted and restored safely.
- Environment references round-trip unchanged.
- Secret sentinel movement and stale revisions fail.
- Unknown fields and duplicate YAML keys fail.
- Revision conflicts return 409.
- Invalid candidates never touch the target file.
- Backup rotation retains exactly five successful predecessors.
- Failed runtime apply restores both file and active state.

### Schema

- Every exported YAML field reachable from `Config` appears in generated schema.
- Enum metadata matches accepted router strategies and provider types.
- Secret and restart-required paths are exhaustive.
- Conditional fields map to their controlling type or strategy.

### Admin API

- Configuration reads require header authentication.
- Query authentication cannot mutate or read configuration data.
- GET responses contain no resolved or literal secrets.
- Validate is side-effect free.
- PUT requires matching base and candidate revisions.
- Error responses contain stable field paths.
- Concurrent PUT requests serialize correctly.

### Dashboard

- Every schema field has a renderable editor.
- All eleven key fields are submitted correctly.
- Structured and YAML modes synchronize through server validation.
- Dirty state, discard, conflict, diff confirmation, save, rollback, and restart-required states render correctly.
- Provider runtime actions remain functional.
- Keyboard traversal and focus management work.
- Layout has no horizontal page overflow at target widths.

### Final verification

```bash
gofmt -w <changed-go-files>
go vet ./...
go test ./...
go build ./...
```

Then exercise the dashboard against a temporary configuration and database at 375px, 768px, 1024px, and 1440px. Verify validation, diff, save, live apply, conflict handling, rollback, secret redaction, and restart-required messaging with real HTTP requests and browser interaction.

## 14. Delivery Boundaries

- No database-backed configuration source.
- No new frontend framework, package manager, or third-party dependency.
- No automatic process restart.
- No secret-value editor or secret retrieval endpoint.
- No configuration history UI; five filesystem backups are recovery artifacts only.
- No unrelated routing behavior changes.

## 15. Acceptance Criteria

1. Every supported configuration field is reachable from structured Settings or Raw YAML.
2. A no-op save preserves the original file byte-for-byte.
3. Edited YAML retains unrelated comments and ordering.
4. Resolved and literal secret values never leave the server.
5. Concurrent external edits cannot be overwritten silently.
6. Invalid configuration cannot replace the current file or runtime state.
7. Successful hot changes affect new requests without process restart.
8. Bootstrap changes are persisted, visibly marked, and deferred until restart.
9. The complete key API is available in the dashboard.
10. Full Go checks and browser verification pass before delivery.
