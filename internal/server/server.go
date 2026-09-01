// Package server exposes the gateway HTTP API ([OI]-compatible).
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Jarvisagentic/tokenroute/internal/auth"
	"github.com/Jarvisagentic/tokenroute/internal/metrics"
	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/ratelimit"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

const maxBody = 10 << 20 // 10MB

// errContextExceeded marks a candidate skipped by the context-window guard.
var errContextExceeded = errors.New("prompt exceeds model context window")

// estimateChatTokens approximates prompt tokens as len(messages JSON)/4.
func estimateChatTokens(body []byte) int {
	var parsed struct {
		Messages json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Messages) == 0 {
		return len(body) / 4
	}
	return len(parsed.Messages) / 4
}

// estimateEmbedTokens approximates prompt tokens as len(input)/4 (chars).
func estimateEmbedTokens(body []byte) int {
	var parsed struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Input) == 0 {
		return len(body) / 4
	}
	return len(parsed.Input) / 4
}

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
}

// New builds the handler. Kept for compatibility: auth disabled.
func New(rt *router.Router, ul *usage.Logger, prices map[string]usage.Price) http.Handler {
	return NewWithOptions(Options{Router: rt, Usage: ul, Prices: prices})
}

func NewWithOptions(o Options) http.Handler {
	s := &srv{router: o.Router, usage: o.Usage, prices: o.Prices,
		keys: o.Keys, limiter: o.Limiter, adminKey: o.AdminKey, cache: o.Cache, metrics: o.Metrics,
		streamIdleMs: o.StreamIdleMs}
	mux := chi.NewRouter()
	mux.Get("/healthz", s.healthz)
	if s.metrics != nil {
		mux.Get("/metrics", s.metricsHandler)
	}
	mux.Group(func(r chi.Router) {
		r.Use(s.requireKey)
		r.Use(timeoutOverride)
		r.Post("/v1/chat/completions", s.chatCompletions)
		r.Post("/v1/embeddings", s.embeddings)
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
		streamIdleMs: o.StreamIdleMs}
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
	})
}

type srv struct {
	router   *router.Router
	usage    *usage.Logger
	prices   map[string]usage.Price
	keys     *auth.Store
	limiter  *ratelimit.Registry
	adminKey string
	cache    *RespCache
	metrics  *metrics.Registry
	// streamIdleMs: per-provider stream idle timeout (provider name -> ms).
	streamIdleMs map[string]int
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
func (s *srv) prepareRequest(w http.ResponseWriter, r *http.Request) (body []byte, model string, k *auth.Key, candidates []router.Candidate, strategy string, ok bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
		return nil, "", nil, nil, "", false
	}
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	model, _ = parsed["model"].(string)
	if model == "" {
		writeErr(w, http.StatusBadRequest, "missing or empty \"model\" field", "invalid_request_error")
		return nil, "", nil, nil, "", false
	}

	// Phase 4: per-key authorization, rate limits, quota.
	k, _ = r.Context().Value(ctxAPIKey).(*auth.Key)
	if k != nil {
		if !modelAllowed(k, model) {
			writeErr(w, http.StatusForbidden, "model not allowed for this API key", "model_not_allowed")
			return nil, "", nil, nil, "", false
		}
		if k.QuotaTokens > 0 && k.SpentTokens >= k.QuotaTokens {
			writeErr(w, http.StatusForbidden, "token quota exceeded", "quota_exceeded")
			return nil, "", nil, nil, "", false
		}
		if k.BudgetUSD > 0 && k.SpentUSD >= k.BudgetUSD {
			writeErr(w, http.StatusPaymentRequired, "USD budget exhausted", "budget_exceeded")
			return nil, "", nil, nil, "", false
		}
		if s.limiter != nil {
			if !s.limiter.AllowRPM(k.ID, k.RPM) {
				writeErr(w, http.StatusTooManyRequests, "rate limit exceeded", "rate_limit_exceeded")
				return nil, "", nil, nil, "", false
			}
			if s.limiter.TPMRemaining(k.ID, k.TPM) <= 0 {
				writeErr(w, http.StatusTooManyRequests, "rate limit exceeded", "rate_limit_exceeded")
				return nil, "", nil, nil, "", false
			}
		}
	}

	if rt := s.router.Resolve(model); rt != nil {
		candidates = s.router.OrderCandidates(rt)
		strategy = rt.Strategy
	} else {
		// No route: pass through to all providers with the same model name.
		for _, p := range s.router.Providers() {
			candidates = append(candidates, router.Candidate{Provider: p, Model: model})
		}
	}
	if len(candidates) == 0 {
		writeErr(w, http.StatusBadRequest, "no provider available for model: "+model, "invalid_request_error")
		return nil, "", nil, nil, "", false
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
			return nil, "", nil, nil, "", false
		}
	}
	return body, model, k, candidates, strategy, true
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
	for _, c := range candidates {
		cand = c
		// Model mapping: alias -> upstream model, per provider, applied after
		// route resolution so the decision header and usage log see the final
		// upstream model.
		cand.Model = s.router.MapModel(c.Provider.Name(), c.Model)
		if estTokens > 0 {
			if p, ok := s.prices[c.Model]; ok && p.ContextTokens > 0 && estTokens > p.ContextTokens {
				lastErr = errContextExceeded // local rejection; try next candidate
				continue
			}
		}
		attemptStart := time.Now()
		req := &provider.Request{Model: cand.Model, Body: body, Header: hdr}
		att, err := call(ctx, c.Provider, req)
		attempts++
		if err != nil {
			s.router.RecordResult(c.Provider.Name(), time.Since(attemptStart), false)
			lastErr = err
			if lastFailResp != nil {
				lastFailResp.Body.Close()
				lastFailResp = nil
			}
			continue
		}
		if retryableStatus(att.StatusCode) {
			errBody, _ := io.ReadAll(io.LimitReader(att.Body, 64<<10))
			att.Body.Close()
			s.router.RecordResult(c.Provider.Name(), time.Since(attemptStart), false)
			if att.StatusCode == http.StatusTooManyRequests {
				s.router.LockModel(c.Provider.Name(), c.Model, 30*time.Second)
				// After RecordResult so the custom duration isn't clobbered.
				if d := parseRetryAfter(att.Header.Get("Retry-After")); d > 0 {
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
		resp = att
		break
	}
	return cand, resp, lastFailResp, lastErr, attempts
}

// relayAllFailed writes the terminal response when every candidate failed:
// the last retryable upstream response as-is, else a 502 transport error.
func (s *srv) relayAllFailed(w http.ResponseWriter, entry *usage.Entry, lastFailResp *http.Response, lastErr error) {
	if lastFailResp != nil {
		// All candidates failed with retryable upstream statuses:
		// relay the last one as-is (Phase 1 transparency).
		entry.Status = lastFailResp.StatusCode
		s.relayFull(w, lastFailResp, entry)
	} else {
		writeErr(w, http.StatusBadGateway, "upstream error: "+lastErr.Error(), "upstream_error")
	}
}

func (s *srv) chatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newRequestID()
	w.Header().Set("X-Request-Id", reqID)

	body, model, k, candidates, strategy, ok := s.prepareRequest(w, r)
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
		if p, ok := s.prices[candidates[0].Model]; ok {
			est := float64(maxTok) / 1e6 * (p.PromptPer1M + p.CompletionPer1M)
			if est > budget {
				writeErr(w, http.StatusPaymentRequired, "budget exceeded", "budget_exceeded")
				return
			}
		}
	}

	hdr := filterHeaders(r.Header)

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
	est := estimateChatTokens(body)
	if strategy == router.StrategyFusion && !stream && len(candidates) > 1 {
		cand, resp, lastFailResp, lastErr, attempts = s.fusionRun(r.Context(), hdr, body, candidates[:2])
		fused = true
	} else {
		cand, resp, lastFailResp, lastErr, attempts = s.failoverCtx(r.Context(), hdr, body, candidates, chatCall, est)
	}
	setDecisionHeader(w, cand, strategy, attempts)
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
	if p, ok := s.prices[cand.Model]; ok {
		entry.CostUSD = usage.Cost(entry.PromptTokens, entry.CompletionTokens, &p)
	}
	if hasBudget && entry.CostUSD != nil && *entry.CostUSD > budget {
		entry.BudgetExceeded = true
	}
	if k != nil && entry.TotalTokens > 0 {
		if s.limiter != nil {
			s.limiter.DeductTPM(k.ID, k.TPM, entry.TotalTokens)
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

	body, model, k, candidates, strategy, ok := s.prepareRequest(w, r)
	if !ok {
		return
	}
	cand, resp, lastFailResp, lastErr, attempts := s.failoverCtx(r.Context(), filterHeaders(r.Header), body, candidates, embedCall, estimateEmbedTokens(body))
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
	if p, ok := s.prices[cand.Model]; ok {
		entry.CostUSD = usage.EmbedCost(entry.PromptTokens, &p)
	}
	if k != nil && entry.TotalTokens > 0 {
		if s.limiter != nil {
			s.limiter.DeductTPM(k.ID, k.TPM, entry.TotalTokens)
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

// retryableStatus marks upstream statuses that trigger failover.
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
		if err := s.usage.Log(ctx, e); err != nil {
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
		if p, ok := s.prices[entry.Model]; ok {
			if cost := usage.Cost(entry.PromptTokens, entry.CompletionTokens, &p); cost != nil {
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
		if entry.TotalTokens == 0 {
			entry.TotalTokens = entry.PromptTokens + entry.CompletionTokens
		}
	}
}

// filterHeaders drops hop-by-hop and auth headers from the client request.
func filterHeaders(h http.Header) http.Header {
	out := http.Header{}
	for k, vs := range h {
		switch http.CanonicalHeaderKey(k) {
		case "Authorization", "Content-Length", "Content-Type", "Connection",
			"Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te",
			"Trailer", "Transfer-Encoding", "Upgrade", "Host",
			"X-Timeout-Ms", "X-Max-Cost-Usd": // gateway-local controls
			continue
		}
		out[k] = append([]string(nil), vs...)
	}
	return out
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
