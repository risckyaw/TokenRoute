package server

import (
	"net/http"
	"strings"
)

// requestSizeLimit rejects oversized requests early via Content-Length,
// before any body is read (Kong request-size-limiting pattern):
// 417 when the client sent Expect: 100-continue, 413 otherwise.
// Chunked/unknown-length bodies are still bounded by MaxBytesReader later.
func requestSizeLimit(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if maxBytes > 0 && r.ContentLength > maxBytes {
				if strings.EqualFold(strings.TrimSpace(r.Header.Get("Expect")), "100-continue") {
					writeErr(w, http.StatusExpectationFailed, "request size limit exceeded", "request_too_large")
					return
				}
				writeErr(w, http.StatusRequestEntityTooLarge, "request size limit exceeded", "request_too_large")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
