package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/provider/openai"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// capsSetup routes "auto" to a text-only candidate (priority 1) and a vision
// candidate (priority 2), both answering 200 from the same upstream.
func capsSetup(t *testing.T, base string, catalog map[string][]string) http.Handler {
	t.Helper()
	p1 := openai.New(openai.Config{Name: "text", BaseURL: base, Priority: 1, TimeoutMs: 5000})
	p2 := openai.New(openai.Config{Name: "vis", BaseURL: base, Priority: 2, TimeoutMs: 5000})
	rt := router.New([]provider.Provider{p1, p2}, []*router.Route{{
		Model: "auto",
		Candidates: []router.Candidate{
			{Provider: p1, Model: "text-only"},
			{Provider: p2, Model: "vision"},
		},
	}})
	if catalog != nil {
		rt.SetModalityLookup(func(model string) ([]string, bool) {
			mods, ok := catalog[model]
			return mods, ok
		})
	}
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	return NewWithOptions(Options{Router: rt, Usage: ul})
}

func postBody(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

const imageReq = `{"model":"auto","messages":[{"role":"user","content":[
	{"type":"text","text":"what is this"},
	{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`

// An image request routes to the vision model even though the text-only
// candidate has the better priority, and the decision header records why.
func TestCapsRoutingPrefersVisionModel(t *testing.T) {
	up := upstream(t, 200, `{"id":"ok"}`)
	h := capsSetup(t, up.URL, map[string][]string{"text-only": nil, "vision": {"image"}})

	rec := postBody(t, h, imageReq)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	dec := rec.Header().Get("X-TokenRoute-Decision")
	if !strings.Contains(dec, "provider=vis") || !strings.Contains(dec, "model=vision") {
		t.Fatalf("decision %q, want the vision candidate", dec)
	}
	if !strings.Contains(dec, ";caps=image") {
		t.Fatalf("decision %q, want ;caps=image", dec)
	}
}

// Text-only requests keep priority order and carry no caps marker.
func TestCapsRoutingTextOnlyNoMarker(t *testing.T) {
	up := upstream(t, 200, `{"id":"ok"}`)
	h := capsSetup(t, up.URL, map[string][]string{"text-only": nil, "vision": {"image"}})

	rec := postBody(t, h, `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)
	dec := rec.Header().Get("X-TokenRoute-Decision")
	if !strings.Contains(dec, "provider=text") {
		t.Fatalf("decision %q, want the priority candidate", dec)
	}
	if strings.Contains(dec, "caps=") {
		t.Fatalf("decision %q, want no caps marker on a text request", dec)
	}
}

// Without a catalog the image request still gets the marker but ordering is
// untouched — nothing is known, so nothing is preferred.
func TestCapsRoutingNoCatalogKeepsOrder(t *testing.T) {
	up := upstream(t, 200, `{"id":"ok"}`)
	h := capsSetup(t, up.URL, nil)

	rec := postBody(t, h, imageReq)
	dec := rec.Header().Get("X-TokenRoute-Decision")
	if !strings.Contains(dec, "provider=text") {
		t.Fatalf("decision %q, want unchanged priority order", dec)
	}
	if !strings.Contains(dec, ";caps=image") {
		t.Fatalf("decision %q, want ;caps=image (detection is catalog-independent)", dec)
	}
}
