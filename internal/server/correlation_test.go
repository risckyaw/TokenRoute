package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCorrelationIDGeneratedAndEchoed(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if correlationIDFrom(r) == "" {
			t.Error("correlation ID missing from context")
		}
		if r.Header.Get(CorrelationHeader) == "" {
			t.Error("correlation ID missing from request header")
		}
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	correlationID(next).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Header().Get(CorrelationHeader) == "" {
		t.Error("correlation ID not echoed downstream")
	}
}

func TestCorrelationIDPassthrough(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := correlationIDFrom(r); got != "abc-123" {
			t.Errorf("context id = %q, want abc-123", got)
		}
	})
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(CorrelationHeader, "abc-123")
	rec := httptest.NewRecorder()
	correlationID(next).ServeHTTP(rec, req)
	if rec.Header().Get(CorrelationHeader) != "abc-123" {
		t.Error("client-supplied correlation ID not preserved")
	}
}
