package router

import (
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
)

func TestParseTagSelector(t *testing.T) {
	if ParseTagSelector("") != nil || ParseTagSelector("  , ,") != nil {
		t.Fatal("empty header must yield nil selector")
	}
	sel := ParseTagSelector("vision, !beta ,&us")
	if len(sel.Plain) != 1 || sel.Plain[0] != "vision" {
		t.Fatalf("plain = %v", sel.Plain)
	}
	if len(sel.Exclude) != 1 || sel.Exclude[0] != "beta" {
		t.Fatalf("exclude = %v", sel.Exclude)
	}
	if len(sel.Require) != 1 || sel.Require[0] != "us" {
		t.Fatalf("require = %v", sel.Require)
	}
}

func TestTagSelectorMatch(t *testing.T) {
	cases := []struct {
		name string
		hdr  string
		tags []string
		want bool
	}{
		{"subset ok", "vision", []string{"vision", "us"}, true},
		{"subset missing", "vision,cheap", []string{"vision"}, false},
		{"excluded present", "!beta", []string{"beta", "vision"}, false},
		{"excluded absent", "!beta", []string{"vision"}, true},
		{"required present", "&vision", []string{"vision"}, true},
		{"required missing", "&vision", []string{"us"}, false},
		{"required vs empty candidate", "&vision", nil, false},
		{"plain vs empty candidate", "vision", nil, false},
		{"empty candidate, exclusion only", "!beta", nil, true},
		{"combined", "&us,vision,!beta", []string{"us", "vision"}, true},
		{"combined excluded", "&us,vision,!beta", []string{"us", "vision", "beta"}, false},
	}
	for _, tc := range cases {
		if got := ParseTagSelector(tc.hdr).MatchTags(tc.tags); got != tc.want {
			t.Errorf("%s: MatchTags(%v) = %v, want %v", tc.name, tc.tags, got, tc.want)
		}
	}
	var nilSel *TagSelector
	if !nilSel.MatchTags(nil) {
		t.Error("nil selector must match everything")
	}
}

func TestOrderCandidatesTagFilter(t *testing.T) {
	hi := &fakeProvider{name: "hi", priority: 1}
	lo := &fakeProvider{name: "lo", priority: 10}
	rt := &Route{Model: "m", Candidates: []Candidate{
		{Provider: hi, Model: "a", Tags: []string{"vision"}},
		{Provider: lo, Model: "b", Tags: []string{"cheap"}},
	}}
	r := New([]provider.Provider{hi, lo}, []*Route{rt})

	// No selector: both pass, priority order.
	if got := r.OrderCandidates(rt); len(got) != 2 {
		t.Fatalf("no selector: %d candidates, want 2", len(got))
	}
	// Subset: only the tagged candidate survives.
	got := r.OrderCandidates(rt.WithTags(ParseTagSelector("vision")))
	if len(got) != 1 || got[0].Provider.Name() != "hi" {
		t.Fatalf("vision: %v", got)
	}
	// Exclusion drops the tagged one, lower-priority wins.
	got = r.OrderCandidates(rt.WithTags(ParseTagSelector("!vision")))
	if len(got) != 1 || got[0].Provider.Name() != "lo" {
		t.Fatalf("!vision: %v", got)
	}
	// Requirement satisfied by candidate tags.
	got = r.OrderCandidates(rt.WithTags(ParseTagSelector("&cheap")))
	if len(got) != 1 || got[0].Provider.Name() != "lo" {
		t.Fatalf("&cheap: %v", got)
	}
	// Requirement no candidate satisfies: empty.
	if got := r.OrderCandidates(rt.WithTags(ParseTagSelector("&us"))); len(got) != 0 {
		t.Fatalf("&us: %v, want empty", got)
	}
	// WithTags on nil selector returns the same route.
	if rt.WithTags(nil) != rt {
		t.Fatal("WithTags(nil) must return the original route")
	}
}
