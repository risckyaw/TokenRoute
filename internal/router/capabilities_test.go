package router

import (
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

func TestDetectRequiredModalities(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "plain text is no requirement",
			body: `{"messages":[{"role":"user","content":"hello"}]}`,
		},
		{
			name: "text blocks only",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
		},
		{
			name: "image_url block",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`,
			want: []string{"image"},
		},
		{
			name: "anthropic input_image",
			body: `{"messages":[{"role":"user","content":[{"type":"input_image","source":{"type":"base64"}}]}]}`,
			want: []string{"image"},
		},
		{
			name: "input_audio block",
			body: `{"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"format":"wav"}}]}]}`,
			want: []string{"audio"},
		},
		{
			name: "file with data-URI pdf mime",
			body: `{"messages":[{"role":"user","content":[{"type":"file","file":{"file_data":"data:application/pdf;base64,JVBER"}}]}]}`,
			want: []string{"pdf"},
		},
		{
			name: "document with nested source media_type",
			body: `{"messages":[{"role":"user","content":[{"type":"document","source":{"media_type":"image/png","data":"AAA"}}]}]}`,
			want: []string{"image"},
		},
		{
			name: "file with video mime",
			body: `{"messages":[{"role":"user","content":[{"type":"input_file","mime_type":"video/mp4"}]}]}`,
			want: []string{"video"},
		},
		{
			name: "file with audio mime",
			body: `{"messages":[{"role":"user","content":[{"type":"file","media_type":"audio/mpeg"}]}]}`,
			want: []string{"audio"},
		},
		{
			name: "bare file without mime falls back to pdf",
			body: `{"messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"f-1"}}]}]}`,
			want: []string{"pdf"},
		},
		{
			name: "unknown mime on a file still falls back to pdf",
			body: `{"messages":[{"role":"user","content":[{"type":"file","mime_type":"application/zip"}]}]}`,
			want: []string{"pdf"},
		},
		{
			name: "multiple modalities deduped and sorted",
			body: `{"messages":[
				{"role":"user","content":[{"type":"image_url","image_url":{"url":"a"}},{"type":"file","mime_type":"application/pdf"}]},
				{"role":"user","content":[{"type":"image","image":{"url":"b"}}]}
			]}`,
			want: []string{"image", "pdf"},
		},
		{name: "unparseable body", body: `not json`},
		{name: "no messages", body: `{"model":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectRequiredModalities([]byte(tc.body))
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestDataURIMIME(t *testing.T) {
	cases := map[string]string{
		"data:application/pdf;base64,AAA": "application/pdf",
		"data:image/jpeg,AAA":             "image/jpeg",
		"https://example.com/a.png":       "",
		"":                                "",
	}
	for in, want := range cases {
		if got := dataURIMIME(in); got != want {
			t.Errorf("dataURIMIME(%q) = %q, want %q", in, got, want)
		}
	}
}

// capsRouter wires a route of 3 candidates with a canned modality catalog.
func capsRouter(t *testing.T, catalog map[string][]string) (*Router, *Route) {
	t.Helper()
	a, b, c, provs := threeProviders()
	rt := &Route{Model: "m", Candidates: []Candidate{
		{Provider: a, Model: "text-only"},
		{Provider: b, Model: "vision"},
		{Provider: c, Model: "uncatalogued"},
	}}
	r := New(provs, []*Route{rt})
	r.SetModalityLookup(func(model string) ([]string, bool) {
		mods, ok := catalog[model]
		return mods, ok
	})
	return r, rt
}

var visionCatalog = map[string][]string{
	"text-only": nil,
	"vision":    {"image", "pdf"},
}

// Candidates covering the requirement lead; the rest keep their relative order.
func TestCapabilityTieringPrefersCovering(t *testing.T) {
	r, rt := capsRouter(t, visionCatalog)
	got := names(r.OrderCandidatesCaps(rt, nil, "", []string{"image"}))
	if got[0] != "b" {
		t.Fatalf("got %v, want the vision model (b) first", got)
	}
	if len(got) != 3 {
		t.Fatalf("got %v, want all 3 candidates kept", got)
	}
	// Tier 1 keeps priority order: a (text-only) before c (uncatalogued).
	if got[1] != "a" || got[2] != "c" {
		t.Fatalf("got %v, want stable tail [a c]", got)
	}
}

// No requirement = no reordering at all.
func TestCapabilityTieringNoRequirementNoop(t *testing.T) {
	r, rt := capsRouter(t, visionCatalog)
	got := names(r.OrderCandidatesCaps(rt, nil, "", nil))
	if got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("got %v, want untouched priority order [a b c]", got)
	}
}

// An uncatalogued model is not assumed capable: it sinks when media is
// required, but is never dropped.
func TestCapabilityTieringUnknownModelSinks(t *testing.T) {
	r, rt := capsRouter(t, map[string][]string{"text-only": nil, "vision": {"image"}})
	got := names(r.OrderCandidatesCaps(rt, nil, "", []string{"image"}))
	for _, n := range got {
		if n == "c" {
			return // present, just not first
		}
	}
	t.Fatalf("uncatalogued candidate dropped: %v", got)
}

// With no catalog wired at all, ordering is byte-for-byte the legacy order.
func TestCapabilityTieringNoCatalogNoop(t *testing.T) {
	a, b, c, provs := threeProviders()
	rt := &Route{Model: "m", Candidates: []Candidate{
		{Provider: c, Model: "cm"}, {Provider: a, Model: "am"}, {Provider: b, Model: "bm"},
	}}
	r := New(provs, []*Route{rt})
	got := names(r.OrderCandidatesCaps(rt, nil, "", []string{"image", "pdf"}))
	if got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("got %v, want priority order when no catalog is wired", got)
	}
}

// A requirement the "vision" model does not cover leaves it in tier 1 too.
func TestCapabilityTieringPartialCoverage(t *testing.T) {
	r, rt := capsRouter(t, map[string][]string{
		"text-only":    nil,
		"vision":       {"image"}, // no audio
		"uncatalogued": {"audio", "image"},
	})
	got := names(r.OrderCandidatesCaps(rt, nil, "", []string{"image", "audio"}))
	if got[0] != "c" {
		t.Fatalf("got %v, want the audio+image model (c) first", got)
	}
}

// Capability tiering wins over the strategy's own ordering (a guaranteed 400
// outranks any latency/cost preference) but preserves it within each tier.
func TestCapabilityTieringOverridesStrategy(t *testing.T) {
	r, rt := capsRouter(t, visionCatalog)
	rt.Strategy = StrategyCost
	r.SetPrices(map[string]usage.Price{
		"text-only":    {PromptPer1M: 0.1, CompletionPer1M: 0.1}, // cheapest
		"vision":       {PromptPer1M: 10, CompletionPer1M: 10},   // priciest
		"uncatalogued": {PromptPer1M: 1, CompletionPer1M: 1},
	})
	// Without a media requirement, cost ordering stands.
	if got := names(r.OrderCandidatesCaps(rt, nil, "", nil)); got[0] != "a" {
		t.Fatalf("got %v, want cheapest (a) first", got)
	}
	// With one, the only capable model leads despite being the most expensive,
	// and the tail keeps the cost order (a cheaper than c).
	got := names(r.OrderCandidatesCaps(rt, nil, "", []string{"image"}))
	if got[0] != "b" || got[1] != "a" || got[2] != "c" {
		t.Fatalf("got %v, want [b a c] (capable first, cost order within tier)", got)
	}
}
