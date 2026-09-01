// Package provider defines the upstream LLM provider interface.
package provider

import (
	"context"
	"io"
	"net/http"
	"strings"
)

type Request struct {
	Model  string      // upstream model name to use
	Body   []byte      // raw client JSON body (gateway rewrites "model" field)
	Header http.Header // filtered client headers to forward
}

type Provider interface {
	Name() string
	Priority() int
	Models(ctx context.Context) ([]string, error)
	ChatComplete(ctx context.Context, req *Request) (*http.Response, error)
	// Embed posts to the provider's embeddings endpoint. Providers without
	// embeddings support return a 501 response (not an error) so the server
	// relays it like a deterministic 4xx (no failover).
	Embed(ctx context.Context, req *Request) (*http.Response, error)
	ModelsURL() string // for tests
}

// UnsupportedEmbed builds the shared 501 response for providers without an
// embeddings endpoint.
func UnsupportedEmbed() *http.Response {
	body := `{"error":{"message":"embeddings not supported by provider","type":"unsupported"}}`
	return &http.Response{
		StatusCode: http.StatusNotImplemented,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
