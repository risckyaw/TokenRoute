package provider

import (
	"net/http"
	"time"
)

// NewHTTPClient builds the provider HTTP client: a clone of
// http.DefaultTransport with ResponseHeaderTimeout (0 = disabled) bounding
// only the wait for response headers — streaming bodies after headers are
// unaffected. timeoutMs is the total request cap (default 120s).
func NewHTTPClient(timeoutMs, responseHeaderTimeoutMs int) *http.Client {
	timeout := time.Duration(timeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if responseHeaderTimeoutMs > 0 {
		tr.ResponseHeaderTimeout = time.Duration(responseHeaderTimeoutMs) * time.Millisecond
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}
