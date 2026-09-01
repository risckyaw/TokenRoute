// Package provider defines the upstream LLM provider interface.
package provider

import (
	"context"
	"net/http"
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
	ModelsURL() string // for tests
}
