// Package server exposes the gateway HTTP API ([OI]-compatible).
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Jarvisagentic/tokenroute/internal/auth"
	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/ratelimit"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

const maxBody = 10 << 20 // 10MB

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
	// SeparateAdmin, when true, removes /admin routes from this handler —
	// they are served by NewAdminOnly on a dedicated listener.
	SeparateAdmin bool
}

// New builds the handler. Kept for compatibility: auth disabled.
func New(rt *router.Router, ul *usage.Logger, prices map[string]usage.Price) http.Handler {
	return NewWithOptions(Options{Router: rt, Usage: ul, Prices: prices})
}

func NewWithOptions(o Options) http.Handler {
	s := &srv{router: o.Router, usage: o.Usage, prices: o.Prices,
		keys: o.Keys, limiter: o.Limiter, adminKey: o.AdminKey}
	mux := chi.NewRouter()
	mux.Get("/healthz", s.healthz)
	mux.Group(func(r chi.Router) {
		r.Use(s.requireKey)
		r.Post("/v1/chat/completions", s.chatCompletions)
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
		keys: o.Keys, limiter: o.Limiter, adminKey: o.AdminKey}
	mux := chi.NewRouter()
	mux.Get("/healthz", s.healthz)
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
		r.Get("/providers", s.adminProviders)
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

func (s *srv) chatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newRequestID()
	w.Header().Set("X-Request-Id", reqID)

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
		return
	}
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	model, _ := parsed["model"].(string)
	if model == "" {
		writeErr(w, http.StatusBadRequest, "missing or empty \"model\" field", "invalid_request_error")
		return
	}
	stream, _ := parsed["stream"].(bool)

	// Phase 4: per-key authorization, rate limits, quota.
	k, _ := r.Context().Value(ctxAPIKey).(*auth.Key)
	if k != nil {
		if !modelAllowed(k, model) {
			writeErr(w, http.StatusForbidden, "model not allowed for this API key", "model_not_allowed")
			return
		}
		if k.QuotaTokens > 0 && k.SpentTokens >= k.QuotaTokens {
			writeErr(w, http.StatusForbidden, "token quota exceeded", "quota_exceeded")
			return
		}
		if s.limiter != nil {
			if !s.limiter.AllowRPM(k.ID, k.RPM) {
				writeErr(w, http.StatusTooManyRequests, "rate limit exceeded", "rate_limit_exceeded")
				return
			}
			if s.limiter.TPMRemaining(k.ID, k.TPM) <= 0 {
				writeErr(w, http.StatusTooManyRequests, "rate limit exceeded", "rate_limit_exceeded")
				return
			}
		}
	}

	var candidates []router.Candidate
	if rt := s.router.Resolve(model); rt != nil {
		candidates = s.router.OrderCandidates(rt)
	} else {
		// No route: pass through to all providers with the same model name.
		for _, p := range s.router.Providers() {
			candidates = append(candidates, router.Candidate{Provider: p, Model: model})
		}
	}
	if len(candidates) == 0 {
		writeErr(w, http.StatusBadRequest, "no provider available for model: "+model, "invalid_request_error")
		return
	}

	// Phase 3: failover across ordered candidates; each tried at most once.
	var resp *http.Response
	var cand router.Candidate
	var lastErr error
	var lastFailResp *http.Response // buffered retryable upstream error
	var attempts int
	for _, c := range candidates {
		cand = c
		attemptStart := time.Now()
		req := &provider.Request{
			Model:  c.Model,
			Body:   body,
			Header: filterHeaders(r.Header),
		}
		att, err := c.Provider.ChatComplete(r.Context(), req)
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
		// Deterministic answer: 2xx success, other 4xx reachable — no failover.
		s.router.RecordResult(c.Provider.Name(), time.Since(attemptStart), true)
		resp = att
		break
	}
	if resp == nil {
		entry := usage.Entry{
			RequestID: reqID, TS: start, VirtualModel: model,
			KeyID: keyID(k), KeyName: keyName(k),
			Provider: cand.Provider.Name(), Model: cand.Model,
			Stream: stream, Status: http.StatusBadGateway,
			LatencyMs: time.Since(start).Milliseconds(),
		}
		if lastFailResp != nil {
			// All candidates failed with retryable upstream statuses:
			// relay the last one as-is (Phase 1 transparency).
			entry.Status = lastFailResp.StatusCode
			s.relayFull(w, lastFailResp, &entry)
		} else {
			writeErr(w, http.StatusBadGateway, "upstream error: "+lastErr.Error(), "upstream_error")
		}
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
	if stream {
		s.relayStream(w, resp, &entry)
	} else {
		s.relayFull(w, resp, &entry)
	}
	entry.LatencyMs = time.Since(start).Milliseconds()
	if p, ok := s.prices[cand.Model]; ok {
		entry.CostUSD = usage.Cost(entry.PromptTokens, entry.CompletionTokens, &p)
	}
	if k != nil && entry.TotalTokens > 0 {
		if s.limiter != nil {
			s.limiter.DeductTPM(k.ID, k.TPM, entry.TotalTokens)
		}
		if err := s.keys.SpendTokens(k.ID, entry.TotalTokens); err != nil {
			slog.Error("spend tokens", "err", err, "key_id", k.ID)
		}
	}
	s.logEntry(r.Context(), entry)
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
	slog.Info("request", attrs...)
}

// relayFull buffers the whole upstream body (non-stream), extracts usage,
// then writes status+headers+body to the client. Upstream Content-Length is
// dropped so Go recomputes it.
func (s *srv) relayFull(w http.ResponseWriter, resp *http.Response, entry *usage.Entry) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// Headers not sent yet; report as upstream error.
		w.Header().Del("Content-Type")
		writeErr(w, http.StatusBadGateway, "upstream error: "+err.Error(), "upstream_error")
		entry.Status = http.StatusBadGateway
		return
	}
	var parsed struct {
		Usage *usage.Usage `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Usage != nil {
		entry.PromptTokens = parsed.Usage.PromptTokens
		entry.CompletionTokens = parsed.Usage.CompletionTokens
		entry.TotalTokens = parsed.Usage.TotalTokens
	}
	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
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
	}
}

// filterHeaders drops hop-by-hop and auth headers from the client request.
func filterHeaders(h http.Header) http.Header {
	out := http.Header{}
	for k, vs := range h {
		switch http.CanonicalHeaderKey(k) {
		case "Authorization", "Content-Length", "Content-Type", "Connection",
			"Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te",
			"Trailer", "Transfer-Encoding", "Upgrade", "Host":
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
