package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestSizeLimitRejectsEarly(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("next handler should not run for oversized Content-Length")
	})
	h := requestSizeLimit(100)(next)

	req := httptest.NewRequest("POST", "/", strings.NewReader("x"))
	req.ContentLength = 101
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

func TestRequestSizeLimitExpectContinue(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := requestSizeLimit(100)(next)

	req := httptest.NewRequest("POST", "/", strings.NewReader("x"))
	req.ContentLength = 101
	req.Header.Set("Expect", "100-continue")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusExpectationFailed {
		t.Errorf("status = %d, want 417", rec.Code)
	}
}

func TestRequestSizeLimitPasses(t *testing.T) {
	ran := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ran = true })
	h := requestSizeLimit(100)(next)

	req := httptest.NewRequest("POST", "/", strings.NewReader("x"))
	req.ContentLength = 50
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !ran {
		t.Error("next handler did not run for in-limit request")
	}
}
