package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// searchHandler serves POST /v1/search: tries each configured backend in
// order until one returns results. Errors of the last backend surface as-is.
func (s *srv) searchHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if len(s.searchBackends) == 0 {
		writeErr(w, http.StatusNotImplemented, "search not configured", "unsupported")
		return
	}
	var req struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.maxBody)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		return
	}
	if req.Query == "" {
		writeErr(w, http.StatusBadRequest, "missing or empty \"query\" field", "invalid_request_error")
		return
	}
	if req.MaxResults <= 0 || req.MaxResults > 20 {
		req.MaxResults = 5
	}
	var lastErr error
	for _, b := range s.searchBackends {
		res, err := b.Search(r.Context(), req.Query, req.MaxResults)
		if err != nil {
			lastErr = err
			continue
		}
		w.Header().Set("X-TokenRoute-Decision", "backend="+b.Name())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"query": req.Query, "results": res, "backend": b.Name(),
		})
		s.logEntry(r.Context(), usage.Entry{
			RequestID: newRequestID(), TS: start, Provider: "search:" + b.Name(),
			Model: req.Query, Status: http.StatusOK,
			LatencyMs: time.Since(start).Milliseconds(),
		})
		return
	}
	writeErr(w, http.StatusBadGateway, "all search backends failed: "+lastErr.Error(), "upstream_error")
}
