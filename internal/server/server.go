// Package server exposes the gateway HTTP API ([OI]-compatible).
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Jarvisagentic/tokenroute/internal/auth"
	"github.com/Jarvisagentic/tokenroute/internal/metrics"
	"github.com/Jarvisagentic/tokenroute/internal/pricing"
	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/ratelimit"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/search"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// defaultMaxBodyMB caps request bodies when Options.MaxBodyMB is unset.
const defaultMaxBodyMB = 10

// errContextExceeded marks a candidate skipped by the context-window guard.
var errContextExceeded = errors.New("prompt exceeds model context window")

type ctxKey int

const ctxAPIKey ctxKey = iota

// Options carries Phase 4 dependencies; Keys nil = no auth (legacy behavior).
type Options struct {
	Router   *router.Router
	Usage    *usage.Logger
	Prices   map[string]usage.Price
	Keys     *auth.Store // nil disables virtual-key auth
	Limiter  *ratelimit.Registry
	AdminKey string // empty = /admin disabled (503)
	// Cache enables the in-memory response cache when non-nil.
	Cache *RespCache
	// Metrics collects Prometheus counters; nil disables /metrics.
	Metrics *metrics.Registry
	// SeparateAdmin, when true, removes /admin routes from this handler —
	// they are served by NewAdminOnly on a dedicated listener.
	SeparateAdmin bool
	// StreamIdleMs aborts a streaming relay after N ms without upstream
	// bytes, per provider name (missing/0 = disabled).
	StreamIdleMs map[string]int
	// ProviderTypes maps provider name -> configured type (openai|anthropic|
	// gemini); expression pricing uses it for anthropic usage semantics.
	ProviderTypes map[string]string
	// RetryPolicy overrides failover/disable classification; nil = built-in.
	RetryPolicy *router.RetryPolicy
	// GroupRatio: group name -> cost multiplier (new-api group_ratio port);
	// nil = cost unaffected by groups.
	GroupRatio map[string]float64
	// MaxBodyMB caps request bodies (0 = default 10MB).
	MaxBodyMB int
	// SearchBackends enables POST /v1/search when non-empty.
	SearchBackends []search.Backend
}

// New builds the handler. Kept for compatibility: auth disabled.
func New(rt *router.Router, ul *usage.Logger, prices map[string]usage.Price) http.Handler {
	return NewWithOptions(Options{Router: rt, Usage: ul, Prices: prices})
}

func NewWithOptions(o Options) http.Handler {
	s := &srv{router: o.Router, usage: o.Usage, prices: o.Prices,
		keys: o.Keys, limiter: o.Limiter, adminKey: o.AdminKey, cache: o.Cache, metrics: o.Metrics,
		streamIdleMs: o.StreamIdleMs, providerTypes: o.ProviderTypes, retryPolicy: o.RetryPolicy,
		groupRatio: o.GroupRatio,
		maxBody: maxBodyBytes(o.MaxBodyMB), searchBackends: o.SearchBackends}
	mux := chi.NewRouter()
	mux.Use(correlationID)
	mux.Get("/healthz", s.healthz)
	if s.metrics != nil {
		mux.Get("/metrics", s.metricsHandler)
	}
	mux.Group(func(r chi.Router) {
		r.Use(s.requireKey)
		r.Use(requestSizeLimit(s.maxBody))
		r.Use(timeoutOverride)
		r.Post("/v1/chat/completions", s.chatCompletions)
		r.Post("/v1/embeddings", s.embeddings)
		r.Post("/v1/search", s.searchHandler)
		r.Get("/v1/models", s.models)
		r.Get("/v1/usage/recent", s.usageRecent)
	})
	if !o.SeparateAdmin {
		s.registerAdmin(mux)
	}
	return mux
}

// NewAdminOnly serves only /admin routes (dedicated admin listener).
func NewAdminOnly(o Options) http.Handler {
	s := &srv{router: o.Router, usage: o.Usage, prices: o.Prices,
		keys: o.Keys, limiter: o.Limiter, adminKey: o.AdminKey, metrics: o.Metrics,
		streamIdleMs: o.StreamIdleMs, maxBody: maxBodyBytes(o.MaxBodyMB)}
	mux := chi.NewRouter()
	mux.Get("/healthz", s.healthz)
	if s.metrics != nil {
		mux.Get("/metrics", s.metricsHandler)
	}
	s.registerAdmin(mux)
	return mux
}

func (s *srv) registerAdmin(mux chi.Router) {
	mux.Route("/admin", func(r chi.Router) {
		r.Use(s.requireAdmin)
		r.Get("/", s.adminDashboard)
		r.Post("/keys", s.adminCreateKey)
		r.Get("/keys", s.adminListKeys)
		r.Post("/keys/{id}/disable", s.adminSetKey(false))
		r.Post("/keys/{id}/enable", s.adminSetKey(true))
		r.Delete("/keys/{id}", s.adminDeleteKey)
		r.Get("/usage", s.adminUsage)
		r.Get("/usage/logs", s.adminUsageLogs)
		r.Get("/usage/export", s.adminUsageExport)
		r.Get("/providers", s.adminProviders)
		r.Post("/providers/{name}/test", s.adminProviderTest)
		r.Post("/providers/{name}/circuit/reset", s.adminCircuitReset)
		r.Post("/providers/{name}/disable", s.adminProviderDisable)
		r.Post("/providers/{name}/enable", s.adminProviderEnable)
	})
}

type srv struct {
	router   *router.Router
	usage    *usage.Logger
	priceMu  sync.RWMutex // guards prices (pricing sync swaps the map)
	prices   map[string]usage.Price
	keys     *auth.Store
	limiter  *ratelimit.Registry
	adminKey string
	cache    *RespCache
	metrics  *metrics.Registry
	// streamIdleMs: per-provider stream idle timeout (provider name -> ms).
	streamIdleMs map[string]int
	// providerTypes: provider name -> configured type (expr pricing semantics).
	providerTypes map[string]string
	// retryPolicy: configured failover/disable overrides (nil = built-in).
	retryPolicy *router.RetryPolicy
	// groupRatio: group name -> cost multiplier (nil = unset).
	groupRatio map[string]float64
	maxBody      int64
	// searchBackends: ordered web-search upstreams for /v1/search.
	searchBackends []search.Backend
}

// price returns the price for a model (RLock; pricing sync may swap the map).
func (s *srv) price(model string) (usage.Price, bool) {
	s.priceMu.RLock()
	defer s.priceMu.RUnlock()
	p, ok := s.prices[model]
	return p, ok
}

// estimateFamily maps a provider type to an estimator weight family.
func estimateFamily(providerType string) string {
	switch providerType {
	case "anthropic":
		return "claude"
	case "gemini":
		return "gemini"
	default:
		return "openai"
	}
}

// exprCost evaluates a model's pricing expression against the entry's usage
// (new-api billingexpr): returns (cost, tier, ok). ok=false when the model
// has no expr or eval fails (eval errors are logged, cost falls back).
// anthropicSemantics selects usage normalization per provider type.
func exprCost(e *usage.Entry, exprStr string, anthropicSemantics bool) (float64, string, bool) {
	prog, used, err := pricing.Compile(exprStr)
	if err != nil {
		return 0, "", false
	}
	env := &pricing.Env{
		P:  float64(e.PromptTokens),
		C:  float64(e.CompletionTokens),
		CR: float64(e.CacheReadTokens),
		CC: float64(e.CacheCreateTokens),
		Img: float64(e.ImageTokens),
		Ai: float64(e.AudioInTokens),
		Ao: float64(e.AudioOutTokens),
	}
	pricing.Normalize(env, used, anthropicSemantics)
	cost, tier, err := pricing.Eval(prog, env)
	if err != nil {
		slog.Warn("pricing expr eval", "err", err, "model", e.Model)
		return 0, "", false
	}
	return cost, tier, true
}

// chatCost computes the USD cost for a chat entry: expression pricing when
// configured (wins entirely), flat rates otherwise. Returns nil unpriced.
func (s *srv) chatCost(entry *usage.Entry, p *usage.Price) *float64 {
	if p.Expr != "" {
		if cost, tier, ok := exprCost(entry, p.Expr, s.providerTypes[entry.Provider] == "anthropic"); ok {
			entry.PriceTier = tier
			return &cost
		}
		// fall through to flat on eval failure (expr was valid at load)
	}
	return usage.Cost(entry.PromptTokens, entry.CompletionTokens, p)
}

// SetPrices swaps the price map (pricing sync; config reload).
func (s *srv) SetPrices(prices map[string]usage.Price) {
	s.priceMu.Lock()
	s.prices = prices
	s.priceMu.Unlock()
}

// maxBodyBytes converts MB to bytes; 0/negative -> 10MB default.
func maxBodyBytes(mb int) int64 {
	if mb <= 0 {
		mb = defaultMaxBodyMB
	}
	return int64(mb) << 20
}

// metricsHandler serves GET /metrics (Prometheus text exposition, no auth).
func (s *srv) metricsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.Write(w)
}

// recordMetrics counts the completed request; nil-safe.
func (s *srv) recordMetrics(e usage.Entry) {
	if s.metrics == nil {
		return
	}
	s.metrics.RecordRequest(e.KeyName, e.Provider, e.Model, e.Status, float64(e.LatencyMs)/1000)
	s.metrics.RecordTokens(e.KeyName, e.Provider, "prompt", e.PromptTokens)
	s.metrics.RecordTokens(e.KeyName, e.Provider, "completion", e.CompletionTokens)
	if e.Cached {
		s.metrics.RecordCacheHit()
	}
}

// requireKey validates "Authorization: Bearer gw-..." when auth is enabled.
func (s *srv) requireKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.keys == nil {
			next.ServeHTTP(w, r)
			return
		}
		h := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(h, prefix) {
			writeErr(w, http.StatusUnauthorized, "invalid or missing API key", "invalid_api_key")
			return
		}
		k, err := s.keys.GetByKey(strings.TrimSpace(strings.TrimPrefix(h, prefix)))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "key lookup: "+err.Error(), "internal_error")
			return
		}
		if k == nil || !k.Enabled || (k.ExpiresAt != nil && time.Now().After(*k.ExpiresAt)) {
			writeErr(w, http.StatusUnauthorized, "invalid or missing API key", "invalid_api_key")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxAPIKey, k)))
	})
}

// maxTimeoutMs caps the X-Timeout-Ms per-request override.
const maxTimeoutMs = 600000

// timeoutOverride applies X-Timeout-Ms (int ms, capped at 600000) as a
// per-request context timeout; invalid/absent values are ignored.
func timeoutOverride(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.Header.Get("X-Timeout-Ms")
		if v == "" {
			next.ServeHTTP(w, r)
			return
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n <= 0 {
			next.ServeHTTP(w, r)
			return
		}
		if n > maxTimeoutMs {
			n = maxTimeoutMs
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(n)*time.Millisecond)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// setRetryAfter writes Retry-After + RateLimit-Reset from a bucket-derived
// hint (Kong: own 429s carry Retry-After). secs <= 0 (unknown) -> 60s.
func setRetryAfter(w http.ResponseWriter, secs int) {
	if secs <= 0 {
		secs = 60
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.Header().Set("RateLimit-Reset", strconv.Itoa(secs))
}

// secondsUntilMidnightUTC is the Retry-After hint for daily quota resets.
func secondsUntilMidnightUTC() int {
	now := time.Now().UTC()
	next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	return int(next.Sub(now).Seconds()) + 1
}

// limitIdentity derives the rate-limit identity for a request: k.ID by
// default; when k.LimitByHeader is set and the header is present, an FNV-1a
// hash of "keyID:headerValue" (stable, collision-tolerant for this scale).
func limitIdentity(k *auth.Key, r *http.Request) int64 {
	if k.LimitByHeader == "" {
		return k.ID
	}
	v := r.Header.Get(k.LimitByHeader)
	if v == "" {
		return k.ID
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(strconv.FormatInt(k.ID, 10) + ":" + v))
	return int64(h.Sum64() & 0x7fffffffffffffff)
}

func modelAllowed(k *auth.Key, model string) bool {
	if len(k.AllowedModels) == 0 {
		return true
	}
	for _, m := range k.AllowedModels {
		if m == model {
			return true
		}
	}
	return false
}

func (s *srv) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func writeErr(w http.ResponseWriter, status int, msg, typ string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": msg, "type": typ},
	})
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// callFn is one upstream call shape (chat or embeddings).
type callFn func(context.Context, provider.Provider, *provider.Request) (*http.Response, error)

func chatCall(ctx context.Context, p provider.Provider, req *provider.Request) (*http.Response, error) {
	return p.ChatComplete(ctx, req)
}

func embedCall(ctx context.Context, p provider.Provider, req *provider.Request) (*http.Response, error) {
	return p.Embed(ctx, req)
}

// prepareRequest validates the body/model, applies per-key auth/ratelimit/
// quota, and resolves ordered candidates. ok=false = error already written.
// mult is the route cost multiplier (1.0 for passthrough requests).
func (s *srv) prepareRequest(w http.ResponseWriter, r *http.Request) (body []byte, model string, k *auth.Key, candidates []router.Candidate, strategy string, mult float64, ok bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
		return nil, "", nil, nil, "", 1, false
	}
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	model, _ = parsed["model"].(string)
	if model == "" {
		writeErr(w, http.StatusBadRequest, "missing or empty \"model\" field", "invalid_request_error")
		return nil, "", nil, nil, "", 1, false
	}
	// Global alias resolution (client name -> virtual route model) before
	// auth checks and route lookup, so allowlists see the canonical target.
	if aliased := s.router.ResolveAlias(model); aliased != model {
		model = aliased
		// Rewrite the body so the provider call carries the resolved model;
		// passthrough fidelity only applies to the model field anyway.
		parsed["model"] = aliased
		if raw, err := json.Marshal(parsed); err == nil {
			body = raw
		}
	}

	// Phase 4: per-key authorization, rate limits, quota.
	k, _ = r.Context().Value(ctxAPIKey).(*auth.Key)
	if k != nil {
		if !modelAllowed(k, model) {
			writeErr(w, http.StatusForbidden, "model not allowed for this API key", "model_not_allowed")
			return nil, "", nil, nil, "", 1, false
		}
		if k.QuotaTokens > 0 && k.SpentTokens >= k.QuotaTokens {
			writeErr(w, http.StatusForbidden, "token quota exceeded", "quota_exceeded")
			return nil, "", nil, nil, "", 1, false
		}
		if k.BudgetUSD > 0 && k.SpentUSD >= k.BudgetUSD {
			writeErr(w, http.StatusPaymentRequired, "USD budget exhausted", "budget_exceeded")
			return nil, "", nil, nil, "", 1, false
		}
		if k.DailyQuota > 0 {
			remaining := k.DailyQuota - k.DailyUsed
			if remaining < 0 {
				remaining = 0
			}
			w.Header().Set("X-RateLimit-Daily-Limit", strconv.FormatInt(k.DailyQuota, 10))
			w.Header().Set("X-RateLimit-Daily-Remaining", strconv.FormatInt(remaining, 10))
			if k.DailyUsed >= k.DailyQuota {
				w.Header().Set("Retry-After", strconv.Itoa(secondsUntilMidnightUTC()))
				writeErr(w, http.StatusTooManyRequests, "daily request quota exceeded", "daily_quota_exceeded")
				return nil, "", nil, nil, "", 1, false
			}
		}
		if s.limiter != nil {
			// Rate-limit identity: normally the key ID; when the key has
			// limit_by_header set, derive a per-header-value identity so one
			// key serves many end-users with isolated buckets (Kong limit_by).
			limID := limitIdentity(k, r)
			rpmLimit := k.RPM
			if k.ModelRPM > 0 {
				rpmLimit = k.ModelRPM
			}
			if rpmLimit > 0 {
				w.Header().Set("RateLimit-Limit", strconv.Itoa(rpmLimit))
				w.Header().Set("RateLimit-Remaining", strconv.Itoa(s.limiter.RPMRemaining(limID, rpmLimit)))
				w.Header().Set("RateLimit-Reset", strconv.Itoa(60))
			}
			if k.TPM > 0 {
				w.Header().Set("X-RateLimit-Token-Limit", strconv.Itoa(k.TPM))
				w.Header().Set("X-RateLimit-Token-Remaining", strconv.Itoa(s.limiter.TPMRemaining(limID, k.TPM)))
			}
			if k.ModelRPM > 0 {
				if !s.limiter.AllowModelRPM(limID, model, k.ModelRPM) {
					setRetryAfter(w, s.limiter.ModelRPMRetryAfter(limID, model, k.ModelRPM))
					writeErr(w, http.StatusTooManyRequests, "rate limit exceeded", "rate_limit_exceeded")
					return nil, "", nil, nil, "", 1, false
				}
			} else if !s.limiter.AllowRPM(limID, k.RPM) {
				setRetryAfter(w, s.limiter.RPMRetryAfter(limID, k.RPM))
				writeErr(w, http.StatusTooManyRequests, "rate limit exceeded", "rate_limit_exceeded")
				return nil, "", nil, nil, "", 1, false
			}
			if s.limiter.TPMRemaining(limID, k.TPM) <= 0 {
				setRetryAfter(w, s.limiter.TPMRetryAfter(limID, k.TPM))
				writeErr(w, http.StatusTooManyRequests, "rate limit exceeded", "rate_limit_exceeded")
				return nil, "", nil, nil, "", 1, false
			}
		}
		if k.DailyQuota > 0 {
			if err := s.keys.IncrDaily(k.ID); err != nil {
				slog.Error("incr daily quota", "err", err, "key_id", k.ID)
			}
		}
	}

	mult = 1
	if rt := s.router.Resolve(model); rt != nil {
		// Tag-based routing (LiteLLM tag_based_routing): X-Route-Tags header,
		// comma-separated; plain = subset match, !tag excludes, &tag requires.
		candidates = s.router.OrderCandidatesHash(rt, router.ParseTagSelector(r.Header.Get("X-Route-Tags")), hashRingValue(rt, r, k))
		strategy = rt.Strategy
		mult = rt.Multiplier
	} else {
		// No route: pass through to all providers with the same model name.
		for _, p := range s.router.Providers() {
			candidates = append(candidates, router.Candidate{Provider: p, Model: model})
		}
	}
	if len(candidates) == 0 {
		writeErr(w, http.StatusBadRequest, "no provider available for model: "+model, "invalid_request_error")
		return nil, "", nil, nil, "", 1, false
	}
	// Group access: drop candidates whose groups don't intersect the key's
	// groups (empty on either side = wildcard).
	if k != nil && len(k.Groups) > 0 {
		filtered := candidates[:0]
		for _, c := range candidates {
			if groupsIntersect(k.Groups, c.Groups) {
				filtered = append(filtered, c)
			}
		}
		candidates = filtered
		if len(candidates) == 0 {
			writeErr(w, http.StatusForbidden, "no available channel for your group", "group_forbidden")
			return nil, "", nil, nil, "", 1, false
		}
	}
	return body, model, k, candidates, strategy, mult, true
}

// groupRatioMultiplier multiplies the configured ratios of the groups
// present in both lists (no intersection = 1.0).
func groupRatioMultiplier(ratios map[string]float64, keyGroups, candGroups []string) float64 {
	m := 1.0
	for _, g := range keyGroups {
		for _, cg := range candGroups {
			if g == cg {
				if r, ok := ratios[g]; ok {
					m *= r
				}
			}
		}
	}
	return m
}

func keyGroups(k *auth.Key) []string {
	if k == nil {
		return nil
	}
	return k.Groups
}

// groupsIntersect reports whether any group appears in both lists
// (an empty list is a wildcard matching everything).
func groupsIntersect(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// failover tries candidates in order with the given call; each tried at most
// once. Returns the chosen candidate, its deterministic response (nil when
// all failed), the last retryable upstream response (buffered), and the last
// transport error.
func (s *srv) failover(ctx context.Context, hdr http.Header, body []byte, candidates []router.Candidate, call callFn) (cand router.Candidate, resp, lastFailResp *http.Response, lastErr error, attempts int) {
	return s.failoverCtx(ctx, hdr, body, candidates, call, 0)
}

// failoverCtx is failover plus a prompt-token estimate for the context-window
// guard (0 = skip the guard). Candidates whose priced context window is
// smaller than the estimate are skipped without an upstream call.
func (s *srv) failoverCtx(ctx context.Context, hdr http.Header, body []byte, candidates []router.Candidate, call callFn, estTokens int) (cand router.Candidate, resp, lastFailResp *http.Response, lastErr error, attempts int) {
	return s.failoverPass(ctx, hdr, nil, body, candidates, call, estTokens)
}

// failoverPass is failoverCtx plus the blocked client headers (for
// per-provider header_pass globs); nil blocked = plain filtering.
func (s *srv) failoverPass(ctx context.Context, hdr, blocked http.Header, body []byte, candidates []router.Candidate, call callFn, estTokens int) (cand router.Candidate, resp, lastFailResp *http.Response, lastErr error, attempts int) {
	for _, c := range candidates {
		cand = c
		// Model mapping: alias -> upstream model, per provider, applied after
		// route resolution so the decision header and usage log see the final
		// upstream model.
		cand.Model = s.router.MapModel(c.Provider.Name(), c.Model)
		if estTokens > 0 {
			if p, ok := s.price(c.Model); ok && p.ContextTokens > 0 && estTokens > p.ContextTokens {
				lastErr = errContextExceeded // local rejection; try next candidate
				continue
			}
		}
		// Per-candidate request overrides (new-api port): provider-level
		// param/header ops, then candidate-level param sets (candidate wins).
		ovr := s.router.ProviderOverride(c.Provider.Name())
		reqBody, reqHdr := body, hdr
		if ovr.ParamSet != nil || ovr.ParamDel != nil || cand.ParamOverride != nil {
			reqBody = ParamOps(body, ovr.ParamSet, ovr.ParamDel, cand.ParamOverride)
		}
		if ovr.HeaderSet != nil || (blocked != nil && len(ovr.HeaderPass) > 0) {
			reqHdr = hdr.Clone()
			// header_pass: resurrect blocked client headers matching the globs.
			for k, vs := range blocked {
				if HeaderPassMatch(ovr.HeaderPass, k) {
					reqHdr[k] = append([]string(nil), vs...)
				}
			}
			for k, v := range ovr.HeaderSet {
				reqHdr.Set(k, v)
			}
		}
		attemptStart := time.Now()
		req := &provider.Request{Model: cand.Model, Body: reqBody, Header: reqHdr}
		s.router.IncInflight(c.Provider.Name())
		att, err := call(ctx, c.Provider, req)
		attempts++
		if err != nil {
			s.router.DecInflight(c.Provider.Name())
			// Classify transport errors: client aborts must not count against
			// the provider's circuit breaker (OmniRoute
			// isLocalStreamLifecycleError).
			f := router.ClassifyFailure(0, "", err)
			s.router.RecordResultKind(c.Provider.Name(), time.Since(attemptStart), false, f.Kind, f.Kind != router.FailureUnknown)
			lastErr = err
			if lastFailResp != nil {
				lastFailResp.Body.Close()
				lastFailResp = nil
			}
			continue
		}
		// Feed upstream-signalled quota state into the ledger (success and
		// error responses alike; stream headers are available pre-body).
		s.observeUpstreamQuota(c.Provider.Name(), cand.Model, att.Header)
		if s.retryableStatus(att.StatusCode) {
			errBody, _ := io.ReadAll(io.LimitReader(att.Body, 64<<10))
			att.Body.Close()
			s.router.DecInflight(c.Provider.Name())
			f := router.ClassifyFailure(att.StatusCode, string(errBody), nil)
			// Configured policy: disable keywords (new-api
			// AutomaticDisableKeywords) reclassify to auth/quota so the
			// circuit opens fast; disable_status_ranges force auth class.
			if s.retryPolicy != nil {
				if kind, ok := s.retryPolicy.ClassifyKeyword(string(errBody)); ok {
					f = router.Failure{Kind: kind}
				} else if s.retryPolicy.DisableStatus(att.StatusCode) {
					f = router.Failure{Kind: router.FailureAuth}
				}
			}
			s.router.RecordResultKind(c.Provider.Name(), time.Since(attemptStart), false, f.Kind, true)
			if att.StatusCode == http.StatusTooManyRequests {
				// Quota-aware lock: honor the upstream-signalled reset
				// (rate-limit headers, then Retry-After) so an exhausted
				// model/key pair stays skipped exactly until its quota
				// window resets instead of a flat blind cooldown.
				lock := 30 * time.Second
				if f.Kind == router.FailureQuotaExhausted {
					// Balance/credit exhaustion is not a rate limit: its
					// window is unknown, so lock long instead of hot-looping.
					lock = router.QuotaExhaustedCooldown
				}
				if d := rateLimitReset(att.Header); d > 0 {
					lock = d
				} else if d := parseRetryAfter(att.Header.Get("Retry-After")); d > 0 {
					lock = d
				}
				s.router.LockModel(c.Provider.Name(), c.Model, lock)
				// After RecordResultKind so the custom duration isn't clobbered.
				if d := parseRetryAfter(att.Header.Get("Retry-After")); d > 0 && f.Kind != router.FailureQuotaExhausted {
					s.router.OpenCircuitFor(c.Provider.Name(), d)
				}
			}
			if lastFailResp != nil {
				lastFailResp.Body.Close()
			}
			lastFailResp = &http.Response{
				StatusCode: att.StatusCode,
				Header:     att.Header,
				Body:       io.NopCloser(bytes.NewReader(errBody)),
			}
			lastErr = nil
			continue
		}
		if att.StatusCode == http.StatusNotFound {
			// Model missing upstream: lock it out briefly, then relay as-is.
			s.router.LockModel(c.Provider.Name(), c.Model, 30*time.Second)
		}
		// Deterministic answer: 2xx success, other 4xx reachable — no failover.
		s.router.RecordResult(c.Provider.Name(), time.Since(attemptStart), true)
		// Inflight stays counted until the relayed body is fully read/closed
		// (covers both buffered and SSE streaming relays).
		att.Body = &inflightBody{ReadCloser: att.Body, done: func() {
			s.router.DecInflight(c.Provider.Name())
		}}
		resp = att
		break
	}
	return cand, resp, lastFailResp, lastErr, attempts
}

// affinityInfo reports how a request was pinned (for the decision header).
// keySrc: "h" (key_header value) or "k" (prompt prefix); "" = not pinned.
type affinityInfo struct {
	hit    bool
	keySrc byte // 'h' | 'k'
}

// runRoute executes the failover (or fusion) loop for one route and, when
// every candidate fails retryably, follows the route's fallback_routes
// (LiteLLM fallbacks): other virtual models tried in order, max 3 route
// hops, cycles skipped via the visited set. Client errors (4xx relayed
// as-is) never trigger fallback — failoverCtx returns those as resp != nil.
func (s *srv) runRoute(ctx context.Context, hdr, blocked http.Header, body []byte, model string, candidates []router.Candidate, strategy string, stream bool, estTokens int, tagHeader string, clientHdr http.Header, keyStr string) (cand router.Candidate, resp, lastFailResp *http.Response, lastErr error, attempts int, fused bool, aff affinityInfo) {
	sel := router.ParseTagSelector(tagHeader)
	visited := map[string]bool{model: true}
	cur := s.router.Resolve(model) // nil for passthrough (no fallback config)
	// Affinity key: the route's key_header value hash when configured
	// (new-api channel affinity; session/thread ids), else the cacheable
	// prompt-prefix hash (LiteLLM prompt_caching_cache).
	affinityOn := cur != nil && (cur.PromptCacheAffinity || s.router.AffinityDefault || cur.AffinityKeyHeader != "")
	pinHash, pinTTL, skipRetry := uint64(0), time.Duration(0), false
	if cur != nil && cur.AffinityKeyHeader != "" {
		if v := clientHdr.Get(cur.AffinityKeyHeader); v != "" {
			pinHash = router.HeaderKeyHash(v)
			aff.keySrc = 'h'
		}
		pinTTL = cur.AffinityTTL
		skipRetry = cur.AffinitySkipRetry
	} else if affinityOn {
		pinHash = router.CachePrefixHash(body)
		pinTTL = cur.AffinityTTL
		skipRetry = cur.AffinitySkipRetry
		aff.keySrc = 'k'
	}
	if affinityOn && pinHash != 0 {
		aff.hit = s.router.PinByAffinity(candidates, pinHash)
	}
	for hops := 0; ; hops++ {
		var at int
		var lfr *http.Response
		var le error
		if strategy == router.StrategyFusion && !stream && len(candidates) > 1 {
			cand, resp, lfr, le, at = s.fusionRun(ctx, hdr, body, candidates[:2])
			fused = true
		} else {
			tryCands := candidates
			// skip_retry_on_failure (new-api): on an affinity HIT only the
			// pinned candidate may serve — the pinned channel holds
			// per-session state, retrying elsewhere loses it (and double-
			// bills). The pin hit already reordered it first.
			if aff.hit && skipRetry {
				tryCands = candidates[:1]
			}
			cand, resp, lfr, le, at = s.failoverPass(ctx, hdr, blocked, body, tryCands, chatCall, estTokens)
		}
		attempts += at
		if lfr != nil {
			if lastFailResp != nil {
				lastFailResp.Body.Close()
			}
			lastFailResp = lfr
		}
		if le != nil {
			lastErr = le
		}
		if resp != nil {
			// Pin the serving provider+model for this key (any 2xx/4xx
			// deterministic response proves the deployment serves it).
			if affinityOn && pinHash != 0 && resp.StatusCode < 500 {
				s.router.RecordAffinityTTL(pinHash, cand.Provider.Name(), cand.Model, pinTTL)
			}
			return cand, resp, lastFailResp, lastErr, attempts, fused, aff
		}
		if cur == nil || hops >= 3 || (aff.hit && skipRetry) {
			return cand, resp, lastFailResp, lastErr, attempts, fused, aff
		}
		// All candidates failed retryably: try the next fallback route.
		var next *router.Route
		for _, name := range cur.FallbackRoutes {
			if visited[name] {
				continue
			}
			if rt := s.router.Resolve(name); rt != nil {
				visited[name] = true
				next = rt
				break
			}
		}
		if next == nil {
			return cand, resp, lastFailResp, lastErr, attempts, fused, aff
		}
		candidates = s.router.OrderCandidatesHash(next, sel, hashRingValueHdr(next, clientHdr, keyStr))
		if len(candidates) == 0 {
			cur = next // skip routes with no usable candidates
			continue
		}
		cur = next
		strategy = next.Strategy
		affinityOn = next.PromptCacheAffinity || s.router.AffinityDefault || next.AffinityKeyHeader != ""
		pinHash, pinTTL, skipRetry = 0, 0, false
		aff = affinityInfo{}
		if next.AffinityKeyHeader != "" {
			if v := clientHdr.Get(next.AffinityKeyHeader); v != "" {
				pinHash = router.HeaderKeyHash(v)
				aff.keySrc = 'h'
			}
		} else if affinityOn {
			pinHash = router.CachePrefixHash(body)
			aff.keySrc = 'k'
		}
		pinTTL, skipRetry = next.AffinityTTL, next.AffinitySkipRetry
		if affinityOn && pinHash != 0 {
			aff.hit = s.router.PinByAffinity(candidates, pinHash)
		}
	}
}

// hashRingValue resolves the consistent_hash ring value for a route:
// hash_on "header:Name" -> that request header's value; "key" -> the
// virtual API key string. Anything else/absent -> "" (priority fallback).
func hashRingValue(rt *router.Route, r *http.Request, k *auth.Key) string {
	return hashRingValueHdr(rt, r.Header, keyString(k))
}

// hashRingValueHdr is hashRingValue on a bare header + key string.
func hashRingValueHdr(rt *router.Route, h http.Header, keyStr string) string {
	if rt == nil || rt.Strategy != router.StrategyConsistentHash {
		return ""
	}
	if name, ok := strings.CutPrefix(rt.HashOn, "header:"); ok && name != "" {
		return h.Get(name)
	}
	if rt.HashOn == "key" {
		return keyStr
	}
	return ""
}

// keyString extracts the raw key for hashing ("" when auth disabled).
func keyString(k *auth.Key) string {
	if k == nil {
		return ""
	}
	return k.Key
}

// relayAllFailed writes the terminal response when every candidate failed:
// the last retryable upstream response as-is, else a 502 transport error.
// When the failure is an upstream 429, the Retry-After header is raised to
// the earliest known reset across model locks and the upstream hint.
func (s *srv) relayAllFailed(w http.ResponseWriter, entry *usage.Entry, lastFailResp *http.Response, lastErr error) {
	if lastFailResp != nil {
		// All candidates failed with retryable upstream statuses:
		// relay the last one as-is (Phase 1 transparency).
		entry.Status = lastFailResp.StatusCode
		if lastFailResp.StatusCode == http.StatusTooManyRequests {
			if until := s.router.ModelLockUntil(entry.Provider, entry.Model); !until.IsZero() {
				secs := int(time.Until(until).Seconds()) + 1
				cur, _ := strconv.Atoi(lastFailResp.Header.Get("Retry-After"))
				if secs > cur {
					lastFailResp.Header.Set("Retry-After", strconv.Itoa(secs))
				}
			}
		}
		s.relayFull(w, lastFailResp, entry)
	} else {
		writeErr(w, http.StatusBadGateway, "upstream error: "+lastErr.Error(), "upstream_error")
	}
}

func (s *srv) chatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newRequestID()
	w.Header().Set("X-Request-Id", reqID)

	body, model, k, candidates, strategy, mult, ok := s.prepareRequest(w, r)
	if !ok {
		return
	}
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	stream, _ := parsed["stream"].(bool)

	// Per-request budget: reject when the pessimistic estimate (max_tokens
	// for both prompt and completion) at the first candidate's price
	// exceeds X-Max-Cost-USD. Unknown price -> allow.
	budget, hasBudget := parseMaxCost(r.Header.Get("X-Max-Cost-USD"))
	if hasBudget {
		maxTok := 4096
		if mt, ok := parsed["max_tokens"].(float64); ok && mt > 0 {
			maxTok = int(mt)
		}
		if p, ok := s.price(candidates[0].Model); ok {
			est := float64(maxTok) / 1e6 * (p.PromptPer1M + p.CompletionPer1M)
			if est > budget {
				writeErr(w, http.StatusPaymentRequired, "budget exceeded", "budget_exceeded")
				return
			}
		}
	}

	hdr, blocked := filterHeadersBlocked(r.Header)

	// Response cache: non-stream only; key uses the first candidate's
	// upstream model so a routing change misses instead of cross-serving.
	ck := ""
	if !stream && s.cache != nil {
		ck = cacheKey(model, candidates[0].Model, body)
		if hit := s.cache.get(ck); hit != nil {
			entry := usage.Entry{
				RequestID: reqID, TS: start, VirtualModel: model,
				KeyID: keyID(k), KeyName: keyName(k),
				Provider: "cache", Model: candidates[0].Model,
				Status: hit.status, Cached: true,
				PromptTokens: hit.promptTokens, CompletionTokens: hit.completionTokens,
				TotalTokens: hit.totalTokens,
			}
			zero := 0.0
			entry.CostUSD = &zero
			w.Header().Set("X-TokenRoute-Cache", "HIT")
			w.Header().Set("X-TokenRoute-Decision", "provider=cache;model="+candidates[0].Model+
				";strategy="+strategy+";attempts=0")
			if hit.contentType != "" {
				w.Header().Set("Content-Type", hit.contentType)
			}
			w.WriteHeader(hit.status)
			_, _ = w.Write(hit.body)
			entry.LatencyMs = time.Since(start).Milliseconds()
			s.logEntry(r.Context(), entry)
			return
		}
	}

	// Phase 3: failover across ordered candidates; each tried at most once.
	var resp *http.Response
	var cand router.Candidate
	var lastErr error
	var lastFailResp *http.Response
	var attempts int
	fused := false
	// Weighted token estimate (new-api estimator port): char-class weights
	// per provider family of the first candidate (openai/anthropic/gemini
	// -> openai/claude/gemini weights).
	est := estimateChatTokens(body, estimateFamily(s.providerTypes[candidates[0].Provider.Name()]))
	var aff affinityInfo
	cand, resp, lastFailResp, lastErr, attempts, fused, aff = s.runRoute(r.Context(), hdr, blocked, body, model, candidates, strategy, stream, est, r.Header.Get("X-Route-Tags"), r.Header, keyString(k))
	setDecisionHeader(w, cand, strategy, attempts)
	if aff.hit {
		// ;affinity=hit kept for compatibility; ;aff=h|k marks the key source
		// (h = key_header value, k = prompt-prefix hash).
		marker := ";affinity=hit"
		if aff.keySrc != 0 {
			marker += ";aff=" + string(aff.keySrc)
		}
		w.Header().Set("X-TokenRoute-Decision", w.Header().Get("X-TokenRoute-Decision")+marker)
	}
	if fused {
		w.Header().Set("X-TokenRoute-Decision", w.Header().Get("X-TokenRoute-Decision")+";fusion=1")
	}
	if resp == nil && lastFailResp == nil && errors.Is(lastErr, errContextExceeded) {
		// Every candidate rejected locally by the context-window guard.
		writeErr(w, http.StatusBadRequest, errContextExceeded.Error(), "context_length_exceeded")
		return
	}
	if resp == nil {
		entry := usage.Entry{
			RequestID: reqID, TS: start, VirtualModel: model,
			KeyID: keyID(k), KeyName: keyName(k),
			Provider: cand.Provider.Name(), Model: cand.Model,
			Stream: stream, Status: http.StatusBadGateway,
			LatencyMs: time.Since(start).Milliseconds(),
		}
		s.relayAllFailed(w, &entry, lastFailResp, lastErr)
		entry.LatencyMs = time.Since(start).Milliseconds()
		s.logEntry(r.Context(), entry)
		return
	}
	defer resp.Body.Close()

	entry := usage.Entry{
		RequestID: reqID, TS: start, VirtualModel: model,
		KeyID: keyID(k), KeyName: keyName(k),
		Provider: cand.Provider.Name(), Model: cand.Model,
		Stream: stream, Status: resp.StatusCode,
	}
	var respBody []byte // captured for cache store (non-stream only)
	if stream {
		s.relayStream(w, resp, &entry)
	} else {
		respBody = s.relayFull(w, resp, &entry)
	}
	entry.LatencyMs = time.Since(start).Milliseconds()
	entry.Multiplier = mult
	if p, ok := s.price(cand.Model); ok {
		entry.CostUSD = s.chatCost(&entry, &p)
	}
	if entry.CostUSD != nil && mult != 1 {
		*entry.CostUSD *= mult
	}
	if entry.CostUSD != nil && s.groupRatio != nil {
		// Group ratio (new-api group_ratio): multiply by the ratios of the
		// key∩candidate group intersection (empty intersection = 1.0).
		*entry.CostUSD *= groupRatioMultiplier(s.groupRatio, keyGroups(k), cand.Groups)
	}
	if hasBudget && entry.CostUSD != nil && *entry.CostUSD > budget {
		entry.BudgetExceeded = true
	}
	if k != nil && entry.TotalTokens > 0 {
		if s.limiter != nil {
			s.limiter.DeductTPM(limitIdentity(k, r), k.TPM, entry.TotalTokens)
		}
		if err := s.keys.SpendTokens(k.ID, entry.TotalTokens); err != nil {
			slog.Error("spend tokens", "err", err, "key_id", k.ID)
		}
	}
	if k != nil && entry.CostUSD != nil && *entry.CostUSD > 0 {
		if err := s.keys.SpendUSD(k.ID, *entry.CostUSD); err != nil {
			slog.Error("spend usd", "err", err, "key_id", k.ID)
		}
	}
	// Quota ledger: record actual usage so pre-request strategies
	// (reset_aware/fill_first/auto) see the remaining budget.
	if entry.TotalTokens > 0 && entry.Provider != "" && entry.Provider != "cache" {
		s.router.Quota().Record(entry.Provider, entry.Model, int64(entry.TotalTokens))
		s.router.RecordTokens(entry.Provider, entry.TotalTokens)
	}
	if ck != "" && respBody != nil && entry.Status == http.StatusOK && entry.TotalTokens > 0 {
		s.cache.store(ck, &cacheEntry{
			body: respBody, status: entry.Status,
			contentType:      w.Header().Get("Content-Type"),
			storedAt:         time.Now(),
			promptTokens:     entry.PromptTokens,
			completionTokens: entry.CompletionTokens,
			totalTokens:      entry.TotalTokens,
		})
	}
	s.logEntry(r.Context(), entry)
}

// embeddings relays POST /v1/embeddings to [OI]-compatible providers.
// Same auth/ratelimit/quota and failover semantics as chat completions;
// non-stream only. Cost uses embed_per_1m (fallback prompt_per_1m).
func (s *srv) embeddings(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newRequestID()
	w.Header().Set("X-Request-Id", reqID)

	body, model, k, candidates, strategy, mult, ok := s.prepareRequest(w, r)
	if !ok {
		return
	}
	ehdr, eblocked := filterHeadersBlocked(r.Header)
	cand, resp, lastFailResp, lastErr, attempts := s.failoverPass(r.Context(), ehdr, eblocked, body, candidates, embedCall, estimateEmbedTokens(body))
	setDecisionHeader(w, cand, strategy, attempts)
	if resp == nil && lastFailResp == nil && errors.Is(lastErr, errContextExceeded) {
		writeErr(w, http.StatusBadRequest, errContextExceeded.Error(), "context_length_exceeded")
		return
	}
	if resp == nil {
		entry := usage.Entry{
			RequestID: reqID, TS: start, VirtualModel: model,
			KeyID: keyID(k), KeyName: keyName(k),
			Provider: cand.Provider.Name(), Model: cand.Model,
			Status: http.StatusBadGateway,
		}
		s.relayAllFailed(w, &entry, lastFailResp, lastErr)
		entry.LatencyMs = time.Since(start).Milliseconds()
		s.logEntry(r.Context(), entry)
		return
	}
	defer resp.Body.Close()

	entry := usage.Entry{
		RequestID: reqID, TS: start, VirtualModel: model,
		KeyID: keyID(k), KeyName: keyName(k),
		Provider: cand.Provider.Name(), Model: cand.Model,
		Status: resp.StatusCode,
	}
	s.relayFull(w, resp, &entry)
	// Embeddings bill prompt tokens only.
	entry.CompletionTokens = 0
	entry.TotalTokens = entry.PromptTokens
	entry.LatencyMs = time.Since(start).Milliseconds()
	entry.Multiplier = mult
	if p, ok := s.price(cand.Model); ok {
		entry.CostUSD = usage.EmbedCost(entry.PromptTokens, &p)
	}
	if entry.CostUSD != nil && mult != 1 {
		*entry.CostUSD *= mult
	}
	if entry.CostUSD != nil && s.groupRatio != nil {
		*entry.CostUSD *= groupRatioMultiplier(s.groupRatio, keyGroups(k), cand.Groups)
	}
	if k != nil && entry.TotalTokens > 0 {
		if s.limiter != nil {
			s.limiter.DeductTPM(limitIdentity(k, r), k.TPM, entry.TotalTokens)
		}
		if err := s.keys.SpendTokens(k.ID, entry.TotalTokens); err != nil {
			slog.Error("spend tokens", "err", err, "key_id", k.ID)
		}
	}
	if k != nil && entry.CostUSD != nil && *entry.CostUSD > 0 {
		if err := s.keys.SpendUSD(k.ID, *entry.CostUSD); err != nil {
			slog.Error("spend usd", "err", err, "key_id", k.ID)
		}
	}
	if entry.TotalTokens > 0 && entry.Provider != "" {
		s.router.RecordTokens(entry.Provider, entry.TotalTokens)
	}
	s.logEntry(r.Context(), entry)
}

// parseMaxCost parses the X-Max-Cost-USD header; ok=false when absent or
// not a positive float (invalid values are ignored, not rejected).
func parseMaxCost(v string) (float64, bool) {
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || f <= 0 {
		return 0, false
	}
	return f, true
}

func keyID(k *auth.Key) int64 {
	if k == nil {
		return 0
	}
	return k.ID
}

func keyName(k *auth.Key) string {
	if k == nil {
		return ""
	}
	return k.Name
}

// setDecisionHeader records which provider/model served the request.
// Must be called before WriteHeader.
func setDecisionHeader(w http.ResponseWriter, cand router.Candidate, strategy string, attempts int) {
	w.Header().Set("X-TokenRoute-Decision", "provider="+cand.Provider.Name()+
		";model="+cand.Model+";strategy="+strategy+";attempts="+strconv.Itoa(attempts))
}

// maxRateLimitReset caps upstream-signalled rate-limit resets (24h).
const maxRateLimitReset = 24 * time.Hour

// rateLimitReset parses the reset time from standard rate-limit response
// headers, returning the duration until reset. Checked in order:
// x-ratelimit-reset-requests (unix seconds or duration), x-ratelimit-reset,
// x-ratelimit-reset-tokens. Garbage/absent -> 0. Capped at maxRateLimitReset.
func rateLimitReset(h http.Header) time.Duration {
	for _, name := range []string{
		"X-Ratelimit-Reset-Requests", "X-Ratelimit-Reset", "X-Ratelimit-Reset-Tokens",
	} {
		v := strings.TrimSpace(h.Get(name))
		if v == "" {
			continue
		}
		var d time.Duration
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			if n > 1_000_000_000 { // unix seconds
				d = time.Until(time.Unix(n, 0))
			} else { // duration in seconds
				d = time.Duration(n) * time.Second
			}
		} else if t, err := http.ParseTime(v); err == nil {
			d = time.Until(t)
		}
		if d > maxRateLimitReset {
			return maxRateLimitReset
		}
		if d > 0 {
			return d
		}
	}
	return 0
}

// observeUpstreamQuota parses the upstream's rate-limit headers (Kong
// response-ratelimiting port) and feeds the quota ledger. Covers
// x-ratelimit-remaining-tokens / x-ratelimit-reset-tokens ([OI], DeepSeek)
// and the anthropic-ratelimit-* prefixed variants. Missing/invalid = no-op.
func (s *srv) observeUpstreamQuota(providerName, model string, h http.Header) {
	rem, okRem := headerInt(h, "X-Ratelimit-Remaining-Tokens")
	if !okRem {
		rem, okRem = headerInt(h, "Anthropic-Ratelimit-Remaining-Tokens")
	}
	if !okRem {
		return // token-remaining is the signal we track; requests-only = no observation
	}
	var reset time.Duration
	for _, name := range []string{"X-Ratelimit-Reset-Tokens", "Anthropic-Ratelimit-Reset-Tokens"} {
		if d := parseOIDuration(h.Get(name)); d > 0 {
			reset = d
			break
		}
	}
	s.router.Quota().ObserveUpstream(providerName, model, rem, reset)
}

// headerInt parses a single integer header value.
func headerInt(h http.Header, name string) (int64, bool) {
	v := strings.TrimSpace(h.Get(name))
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// parseOIDuration parses [OI] duration strings ("1s", "1m30s", "6h") or
// plain integer seconds; garbage/non-positive -> 0.
func parseOIDuration(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	return 0
}

// parseRetryAfter parses a Retry-After value (seconds int or http-date);
// garbage -> 0.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		if n <= 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// retryableStatus marks upstream statuses that trigger failover: the
// configured policy when set, else the built-in 429/500/502/503/504.
func (s *srv) retryableStatus(code int) bool {
	if s.retryPolicy != nil {
		return s.retryPolicy.Retryable(code)
	}
	return retryableStatus(code)
}

// retryableStatus is the built-in failover set (no policy configured).
func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func (s *srv) logEntry(ctx context.Context, e usage.Entry) {
	s.recordMetrics(e)
	if s.usage != nil {
		// Usage side effects must survive the client connection: a client
		// that disconnects at the terminal stream event cancels its request
		// context, which must not cancel the usage write (9router lost
		// billing data to this). Give the log its own bounded context.
		logCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.usage.Log(logCtx, e); err != nil {
			slog.Error("usage log", "err", err, "request_id", e.RequestID)
		}
	}
	attrs := []any{
		"request_id", e.RequestID,
		"model", e.VirtualModel,
		"provider", e.Provider,
		"upstream_model", e.Model,
		"status", e.Status,
		"latency_ms", e.LatencyMs,
		"prompt_tokens", e.PromptTokens,
		"completion_tokens", e.CompletionTokens,
		"total_tokens", e.TotalTokens,
		"stream", e.Stream,
	}
	if e.CostUSD != nil {
		attrs = append(attrs, "cost_usd", *e.CostUSD)
	}
	if e.BudgetExceeded {
		attrs = append(attrs, "budget_exceeded", true)
	}
	slog.Info("request", attrs...)
}

// inflightBody decrements the provider's inflight counter on Close
// (once). The gateway always closes relayed bodies exactly once.
type inflightBody struct {
	io.ReadCloser
	done func()
}

func (b *inflightBody) Close() error {
	err := b.ReadCloser.Close()
	if b.done != nil {
		b.done()
		b.done = nil
	}
	return err
}

// relayFull buffers the whole upstream body (non-stream), extracts usage,
// then writes status+headers+body to the client. Upstream Content-Length is
// dropped so Go recomputes it. Returns the relayed body (nil on read error)
// for the response cache.
func (s *srv) relayFull(w http.ResponseWriter, resp *http.Response, entry *usage.Entry) []byte {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// Headers not sent yet; report as upstream error.
		w.Header().Del("Content-Type")
		writeErr(w, http.StatusBadGateway, "upstream error: "+err.Error(), "upstream_error")
		entry.Status = http.StatusBadGateway
		return nil
	}
	var parsed struct {
		Usage *usage.Usage `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Usage != nil {
		entry.PromptTokens = parsed.Usage.PromptTokens
		entry.CompletionTokens = parsed.Usage.CompletionTokens
		entry.TotalTokens = parsed.Usage.TotalTokens
		entry.CacheReadTokens = parsed.Usage.CacheRead()
		entry.CacheCreateTokens = parsed.Usage.CacheCreate()
		entry.ImageTokens = parsed.Usage.ImageTokens()
		entry.AudioInTokens = parsed.Usage.AudioIn()
		entry.AudioOutTokens = parsed.Usage.AudioOut()
		if entry.TotalTokens == 0 {
			// Upstream omitted/zero total: derive from parts.
			entry.TotalTokens = entry.PromptTokens + entry.CompletionTokens
		}
	}
	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if resp.StatusCode == http.StatusOK && entry.TotalTokens > 0 {
		// Non-stream only: usage known before WriteHeader. Streams skip these.
		w.Header().Set("X-TokenRoute-Prompt-Tokens", strconv.Itoa(entry.PromptTokens))
		w.Header().Set("X-TokenRoute-Completion-Tokens", strconv.Itoa(entry.CompletionTokens))
		w.Header().Set("X-TokenRoute-Total-Tokens", strconv.Itoa(entry.TotalTokens))
		if p, ok := s.price(entry.Model); ok {
			if cost := s.chatCost(entry, &p); cost != nil {
				w.Header().Set("X-TokenRoute-Cost-USD", strconv.FormatFloat(*cost, 'f', -1, 64))
			}
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
	return body
}

// relayStream copies the upstream SSE stream to the client byte-for-byte,
// flushing per write, while feeding completed lines to a usage tracker.
func (s *srv) relayStream(w http.ResponseWriter, resp *http.Response, entry *usage.Entry) {
	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	var tr usage.SSEUsageTracker
	var pending []byte // partial line remainder
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, werr := w.Write(chunk); werr != nil {
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
			pending = append(pending, chunk...)
			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				tr.Feed(pending[:i])
				pending = pending[i+1:]
			}
		}
		if rerr != nil {
			break
		}
	}
	if len(pending) > 0 {
		tr.Feed(pending)
	}
	if u := tr.Usage(); u != nil {
		entry.PromptTokens = u.PromptTokens
		entry.CompletionTokens = u.CompletionTokens
		entry.TotalTokens = u.TotalTokens
		entry.CacheReadTokens = u.CacheRead()
		entry.CacheCreateTokens = u.CacheCreate()
		entry.ImageTokens = u.ImageTokens()
		entry.AudioInTokens = u.AudioIn()
		entry.AudioOutTokens = u.AudioOut()
		if entry.TotalTokens == 0 {
			entry.TotalTokens = entry.PromptTokens + entry.CompletionTokens
		}
	}
}

// filterHeaders drops hop-by-hop and auth headers from the client request.
func filterHeaders(h http.Header) http.Header {
	out, _ := filterHeadersBlocked(h)
	return out
}

// filterHeadersBlocked is filterHeaders plus the dropped entries (for
// per-provider header_pass globs, which may resurrect blocked headers).
func filterHeadersBlocked(h http.Header) (passed, blocked http.Header) {
	passed, blocked = http.Header{}, http.Header{}
	for k, vs := range h {
		switch http.CanonicalHeaderKey(k) {
		case "Authorization", "Content-Length", "Content-Type", "Connection",
			"Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te",
			"Trailer", "Transfer-Encoding", "Upgrade", "Host",
			"X-Timeout-Ms", "X-Max-Cost-Usd", "X-Route-Tags": // gateway-local controls
			blocked[k] = append([]string(nil), vs...)
			continue
		}
		passed[k] = append([]string(nil), vs...)
	}
	return passed, blocked
}

type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

func (s *srv) models(w http.ResponseWriter, r *http.Request) {
	seen := map[string]bool{}
	data := []modelEntry{}
	for _, p := range s.router.Providers() {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		ids, err := p.Models(ctx)
		cancel()
		if err != nil {
			continue // best effort
		}
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				data = append(data, modelEntry{ID: id, Object: "model", OwnedBy: p.Name()})
			}
		}
	}
	for _, m := range s.router.RouteModels() {
		if !seen[m] {
			seen[m] = true
			data = append(data, modelEntry{ID: m, Object: "model", OwnedBy: "gateway"})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func (s *srv) usageRecent(w http.ResponseWriter, r *http.Request) {
	if s.usage == nil {
		writeErr(w, http.StatusServiceUnavailable, "usage logging disabled", "invalid_request_error")
		return
	}
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			limit = min(n, 500)
		}
	}
	entries, err := s.usage.QueryRecent(limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "usage query: "+err.Error(), "internal_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}
