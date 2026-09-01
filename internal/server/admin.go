package server

import (
	"context"
	"crypto/subtle"
	"encoding/csv"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Jarvisagentic/tokenroute/internal/auth"
	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// requireAdmin gates /admin on the configured admin key.
func (s *srv) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.adminKey == "" {
			writeErr(w, http.StatusServiceUnavailable, "admin disabled", "admin_disabled")
			return
		}
		// Browser dashboard may pass the key as ?key=; header takes precedence.
		key := r.Header.Get("X-Admin-Key")
		if key == "" {
			key = r.URL.Query().Get("key")
		}
		if subtle.ConstantTimeCompare([]byte(key), []byte(s.adminKey)) != 1 {
			writeErr(w, http.StatusUnauthorized, "invalid admin key", "invalid_admin_key")
			return
		}
		if s.keys == nil {
			writeErr(w, http.StatusServiceUnavailable, "auth store unavailable", "admin_disabled")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// masked returns the key list view with secrets masked.
func masked(k auth.Key) map[string]any {
	m := "gw-****"
	if len(k.Key) >= 7 {
		m = k.Key[:7] + "..."
	}
	return map[string]any{
		"id": k.ID, "key": m, "name": k.Name, "rpm": k.RPM, "tpm": k.TPM,
		"model_rpm":    k.ModelRPM,
		"quota_tokens": k.QuotaTokens, "spent_tokens": k.SpentTokens,
		"budget_usd": k.BudgetUSD, "spent_usd": k.SpentUSD,
		"allowed_models": k.AllowedModels, "groups": k.Groups, "expires_at": k.ExpiresAt,
		"enabled": k.Enabled, "created_at": k.CreatedAt,
	}
}

type createKeyReq struct {
	Name          string   `json:"name"`
	RPM           int      `json:"rpm"`
	TPM           int      `json:"tpm"`
	ModelRPM      int      `json:"model_rpm"`
	LimitByHeader string   `json:"limit_by_header"`
	DailyQuota    int64    `json:"daily_quota"`
	QuotaTokens   int64    `json:"quota_tokens"`
	BudgetUSD     float64  `json:"budget_usd"`
	AllowedModels []string `json:"allowed_models"`
	Groups        []string `json:"groups"`
	ExpiresAt     *string  `json:"expires_at"` // RFC3339
}

func (s *srv) adminCreateKey(w http.ResponseWriter, r *http.Request) {
	var req createKeyReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required", "invalid_request_error")
		return
	}
	k := auth.Key{
		Name: req.Name, RPM: req.RPM, TPM: req.TPM, ModelRPM: req.ModelRPM,
		LimitByHeader: req.LimitByHeader, DailyQuota: req.DailyQuota,
		QuotaTokens: req.QuotaTokens, BudgetUSD: req.BudgetUSD,
		AllowedModels: req.AllowedModels, Groups: req.Groups, Enabled: true,
	}
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "expires_at must be RFC3339", "invalid_request_error")
			return
		}
		k.ExpiresAt = &t
	}
	created, err := s.keys.Create(k)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create key: "+err.Error(), "internal_error")
		return
	}
	// Full key string shown only here.
	writeJSON(w, http.StatusCreated, created)
}

func (s *srv) adminListKeys(w http.ResponseWriter, _ *http.Request) {
	keys, err := s.keys.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list keys: "+err.Error(), "internal_error")
		return
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, masked(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (s *srv) adminSetKey(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid key id", "invalid_request_error")
			return
		}
		if err := s.keys.SetEnabled(id, enabled); err != nil {
			writeErr(w, http.StatusInternalServerError, "update key: "+err.Error(), "internal_error")
			return
		}
		if s.limiter != nil {
			s.limiter.Invalidate(id)
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": enabled})
	}
}

func (s *srv) adminDeleteKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid key id", "invalid_request_error")
		return
	}
	if err := s.keys.Delete(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "delete key: "+err.Error(), "internal_error")
		return
	}
	if s.limiter != nil {
		s.limiter.Invalidate(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *srv) adminUsage(w http.ResponseWriter, _ *http.Request) {
	if s.usage == nil {
		writeErr(w, http.StatusServiceUnavailable, "usage logging disabled", "invalid_request_error")
		return
	}
	aggs, err := s.usage.AggregateByKey()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "usage aggregate: "+err.Error(), "internal_error")
		return
	}
	totals := map[string]any{"requests": 0, "total_tokens": 0, "cost_usd": 0.0}
	reqs, toks := 0, 0
	cost := 0.0
	for _, a := range aggs {
		reqs += a.Requests
		toks += a.TotalToken
		cost += a.CostUSD
	}
	totals["requests"] = reqs
	totals["total_tokens"] = toks
	totals["cost_usd"] = cost
	writeJSON(w, http.StatusOK, map[string]any{"keys": aggs, "totals": totals})
}

func (s *srv) adminUsageLogs(w http.ResponseWriter, r *http.Request) {
	if s.usage == nil {
		writeErr(w, http.StatusServiceUnavailable, "usage logging disabled", "invalid_request_error")
		return
	}
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			limit = min(n, 200)
		}
	}
	entries, err := s.usage.QueryRecent(limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "usage logs query: "+err.Error(), "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// adminUsageExport streams usage logs as CSV. Query: from/to RFC3339
// (default: last 24h). format=csv is the only supported format.
func (s *srv) adminUsageExport(w http.ResponseWriter, r *http.Request) {
	if s.usage == nil {
		writeErr(w, http.StatusServiceUnavailable, "usage logging disabled", "invalid_request_error")
		return
	}
	if f := r.URL.Query().Get("format"); f != "" && f != "csv" {
		writeErr(w, http.StatusBadRequest, "unsupported format: "+f, "invalid_request_error")
		return
	}
	q := r.URL.Query()
	to := time.Now()
	from := to.Add(-24 * time.Hour)
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "from must be RFC3339", "invalid_request_error")
			return
		}
		from = t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "to must be RFC3339", "invalid_request_error")
			return
		}
		to = t
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="tokenroute-usage.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"id", "ts", "key_name", "virtual_model", "provider", "model",
		"prompt_tokens", "completion_tokens", "total_tokens", "stream", "status",
		"latency_ms", "cost_usd", "multiplier", "budget_exceeded", "cached"})
	err := s.usage.ExportRows(from, to, func(e usage.Entry) error {
		cost := ""
		if e.CostUSD != nil {
			cost = strconv.FormatFloat(*e.CostUSD, 'f', -1, 64)
		}
		mult := e.Multiplier
		if mult == 0 {
			mult = 1
		}
		return cw.Write([]string{
			strconv.FormatInt(e.ID, 10),
			e.TS.UTC().Format(time.RFC3339Nano),
			e.KeyName, e.VirtualModel, e.Provider, e.Model,
			strconv.Itoa(e.PromptTokens), strconv.Itoa(e.CompletionTokens), strconv.Itoa(e.TotalTokens),
			strconv.FormatBool(e.Stream), strconv.Itoa(e.Status),
			strconv.FormatInt(e.LatencyMs, 10), cost, strconv.FormatFloat(mult, 'f', -1, 64),
			strconv.FormatBool(e.BudgetExceeded),
			strconv.FormatBool(e.Cached),
		})
	})
	cw.Flush()
	if err != nil {
		slog.Error("usage export", "err", err)
	}
}

func (s *srv) adminProviders(w http.ResponseWriter, _ *http.Request) {
	out := []map[string]any{}
	for _, p := range s.router.Providers() {
		out = append(out, map[string]any{
			"name":           p.Name(),
			"priority":       p.Priority(),
			"circuit":        s.router.CircuitState(p.Name()),
			"disabled":       s.router.ProviderDisabled(p.Name()),
			"ema_latency_ms": s.router.LatencyMs(p.Name()),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}

func (s *srv) adminCircuitReset(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	s.router.ResetCircuit(name)
	writeJSON(w, http.StatusOK, map[string]any{"provider": name, "circuit": s.router.CircuitState(name)})
}

func (s *srv) adminProviderDisable(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	s.router.DisableProvider(name)
	writeJSON(w, http.StatusOK, map[string]any{"provider": name, "disabled": s.router.ProviderDisabled(name)})
}

// adminProviderEnable re-enables a disabled provider: breaker closed, auto-
// disable trip counter reset.
func (s *srv) adminProviderEnable(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	s.router.EnableProvider(name)
	writeJSON(w, http.StatusOK, map[string]any{"provider": name, "disabled": s.router.ProviderDisabled(name), "circuit": s.router.CircuitState(name)})
}

// adminProviderTest sends a minimal chat completion through the provider to
// verify reachability + auth, returning status and latency.
func (s *srv) adminProviderTest(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var target provider.Provider
	for _, p := range s.router.Providers() {
		if p.Name() == name {
			target = p
			break
		}
	}
	if target == nil {
		writeErr(w, http.StatusNotFound, "unknown provider: "+name, "invalid_request_error")
		return
	}

	model := ""
	ctx0, cancel0 := context.WithTimeout(r.Context(), 5*time.Second)
	if ms, err := target.Models(ctx0); err == nil && len(ms) > 0 {
		model = ms[0]
	}
	cancel0()
	if model == "" {
		model = s.router.FirstModelFor(name)
	}
	if model == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "status": 0, "latency_ms": 0, "error": "no model available to test"})
		return
	}

	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
	})
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	start := time.Now()
	resp, err := target.ChatComplete(ctx, &provider.Request{Model: model, Body: body})
	lat := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "status": 0, "latency_ms": lat, "error": err.Error()})
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         resp.StatusCode >= 200 && resp.StatusCode < 300,
		"status":     resp.StatusCode,
		"latency_ms": lat,
		"error":      "",
	})
}
