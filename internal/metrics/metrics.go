// Package metrics renders a Prometheus text exposition of gateway counters.
// No external deps: counters live in sync maps, the text format is written
// by hand on scrape.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Buckets for tokenroute_latency_seconds.
var Buckets = []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}

type labels string // pre-rendered `k="v",...` (no braces)

func render(pairs ...string) labels {
	var b strings.Builder
	for i := 0; i < len(pairs); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(pairs[i])
		b.WriteString(`="`)
		b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(pairs[i+1]))
		b.WriteByte('"')
	}
	return labels(b.String())
}

type counter struct {
	mu sync.Mutex
	m  map[labels]float64
}

func (c *counter) inc(l labels, n float64) {
	c.mu.Lock()
	if c.m == nil {
		c.m = map[labels]float64{}
	}
	c.m[l] += n
	c.mu.Unlock()
}

func (c *counter) write(w io.Writer, name, help string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	c.mu.Lock()
	keys := make([]string, 0, len(c.m))
	for l := range c.m {
		keys = append(keys, string(l))
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s{%s} %v\n", name, k, c.m[labels(k)])
	}
	c.mu.Unlock()
}

type histogram struct {
	mu      sync.Mutex
	buckets map[labels][]float64 // counts per bucket boundary
	sum     map[labels]float64
	count   map[labels]float64
}

func (h *histogram) observe(l labels, v float64) {
	h.mu.Lock()
	if h.buckets == nil {
		h.buckets = map[labels][]float64{}
		h.sum = map[labels]float64{}
		h.count = map[labels]float64{}
	}
	bs, ok := h.buckets[l]
	if !ok {
		bs = make([]float64, len(Buckets))
		h.buckets[l] = bs
	}
	for i, b := range Buckets {
		if v <= b {
			bs[i]++
		}
	}
	h.sum[l] += v
	h.count[l]++
	h.mu.Unlock()
}

func (h *histogram) write(w io.Writer, name, help string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	h.mu.Lock()
	keys := make([]string, 0, len(h.count))
	for l := range h.count {
		keys = append(keys, string(l))
	}
	sort.Strings(keys)
	for _, k := range keys {
		l := labels(k)
		sep := ","
		if k == "" {
			sep = ""
		}
		for i, b := range Buckets {
			fmt.Fprintf(w, `%s_bucket{%s%sle="%v"} %v`+"\n", name, k, sep, b, h.buckets[l][i])
		}
		fmt.Fprintf(w, `%s_bucket{%s%sle="+Inf"} %v`+"\n", name, k, sep, h.count[l])
		fmt.Fprintf(w, "%s_sum{%s} %v\n%s_count{%s} %v\n", name, k, h.sum[l], name, k, h.count[l])
	}
	h.mu.Unlock()
}

// Registry holds all gateway metrics.
type Registry struct {
	requests  counter // tokenroute_requests_total{key,provider,model,status_class}
	tokens    counter // tokenroute_tokens_total{key,provider,kind}
	cacheHits counter // tokenroute_cache_hits_total
	latency   histogram
	// CircuitOpen reports 1 when the provider's circuit is open (read at scrape).
	CircuitOpen func(provider string) bool
	Providers   func() []string
}

func New() *Registry { return &Registry{} }

// statusClass maps an HTTP status to "2xx"/"4xx"/"5xx" (other -> "5xx" bucket
// is wrong; use exact class by hundreds digit).
func statusClass(code int) string {
	return fmt.Sprintf("%dxx", code/100)
}

// RecordRequest counts one completed request and observes its latency.
func (r *Registry) RecordRequest(key, provider, model string, status int, seconds float64) {
	r.requests.inc(render("key", key, "provider", provider, "model", model, "status_class", statusClass(status)), 1)
	r.latency.observe(render("provider", provider), seconds)
}

// RecordTokens counts prompt/completion tokens for a key+provider.
func (r *Registry) RecordTokens(key, provider, kind string, n int) {
	if n <= 0 {
		return
	}
	r.tokens.inc(render("key", key, "provider", provider, "kind", kind), float64(n))
}

// RecordCacheHit counts one response-cache hit.
func (r *Registry) RecordCacheHit() { r.cacheHits.inc(render(), 1) }

// Write renders the full text exposition to w.
func (r *Registry) Write(w io.Writer) {
	r.requests.write(w, "tokenroute_requests_total", "Completed gateway requests.")
	r.tokens.write(w, "tokenroute_tokens_total", "Tokens processed, by kind (prompt/completion).")
	r.cacheHits.write(w, "tokenroute_cache_hits_total", "Response cache hits.")
	r.latency.write(w, "tokenroute_latency_seconds", "Upstream request latency in seconds.")
	fmt.Fprint(w, "# HELP tokenroute_circuit_open Circuit breaker open flag (1=open).\n# TYPE tokenroute_circuit_open gauge\n")
	if r.Providers != nil && r.CircuitOpen != nil {
		for _, p := range r.Providers() {
			v := 0
			if r.CircuitOpen(p) {
				v = 1
			}
			fmt.Fprintf(w, "tokenroute_circuit_open{provider=%q} %d\n", p, v)
		}
	}
}
